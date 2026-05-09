package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/memory-daemon/memoryd/internal/pipeline"
	"github.com/memory-daemon/memoryd/internal/rejection"
	"github.com/memory-daemon/memoryd/internal/synthesizer"
)

// minSessionTurns is the minimum number of complete Q&A pairs needed before
// a session summary is generated. A "pair" is one user + one assistant turn.
const minSessionTurns = 3

// sessionSynthesisInterval controls how often session summaries are generated
// after the initial one. Fires at minSessionTurns, then every N pairs after,
// preventing an ever-growing stack of overlapping summaries.
const sessionSynthesisInterval = 5

// anthropicHandler transparently proxies /v1/messages, capturing response
// content on the way out for the write pipeline (ingestion). Retrieval is
// handled by the MCP server, not the proxy.
type anthropicHandler struct {
	upstreamURL  string
	write        *pipeline.WritePipeline
	client       *http.Client
	writeEnabled bool // false in mcp or mcp-readonly modes
	synth        *synthesizer.Synthesizer
	rejLog       *rejection.Store
	xbuf         *exchangeBuffer
}

func newAnthropicHandler(upstreamURL string, write *pipeline.WritePipeline, client *http.Client, writeEnabled bool, synth *synthesizer.Synthesizer, rejLog *rejection.Store) *anthropicHandler {
	return &anthropicHandler{
		upstreamURL:  upstreamURL,
		write:        write,
		client:       client,
		writeEnabled: writeEnabled,
		synth:        synth,
		rejLog:       rejLog,
		xbuf:         newExchangeBuffer(0),
	}
}

func (h *anthropicHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Printf("[proxy] incoming %s %s", r.Method, r.URL.Path)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	// Parse into raw-message map to extract user text for ingestion.
	var rawReq map[string]json.RawMessage
	if err := json.Unmarshal(body, &rawReq); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// --- Forward to Anthropic unchanged ---
	upReq, err := http.NewRequestWithContext(r.Context(), "POST",
		h.upstreamURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		http.Error(w, "failed to build upstream request", http.StatusInternalServerError)
		return
	}

	// Forward all original headers, skipping hop-by-hop headers and
	// accept-encoding (we need uncompressed responses for streaming/logging).
	for key, values := range r.Header {
		switch strings.ToLower(key) {
		case "connection", "transfer-encoding", "content-length", "host", "accept-encoding":
			continue
		}
		for _, v := range values {
			upReq.Header.Add(key, v)
		}
	}

	upResp, err := h.client.Do(upReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("upstream error: %v", err), http.StatusBadGateway)
		return
	}
	defer upResp.Body.Close()

	// --- Dispatch based on streaming ---
	var isStreaming bool
	if raw, ok := rawReq["stream"]; ok {
		json.Unmarshal(raw, &isStreaming)
	}
	log.Printf("[proxy] streaming=%v, upstream status=%d", isStreaming, upResp.StatusCode)
	if upResp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(upResp.Body)
		// Decompress if gzip for logging
		logBody := errBody
		if upResp.Header.Get("Content-Encoding") == "gzip" {
			if gr, err := gzip.NewReader(bytes.NewReader(errBody)); err == nil {
				logBody, _ = io.ReadAll(gr)
				gr.Close()
			}
		}
		log.Printf("[proxy] upstream error body: %s", string(logBody))
		copyHeaders(w.Header(), upResp.Header)
		w.WriteHeader(upResp.StatusCode)
		w.Write(errBody)
		return
	}
	if isStreaming {
		h.handleStream(w, upResp, rawReq)
	} else {
		h.handleSync(w, upResp, rawReq)
	}
}

// handleSync proxies a non-streaming response and kicks off the write path.
func (h *anthropicHandler) handleSync(w http.ResponseWriter, resp *http.Response, rawReq map[string]json.RawMessage) {
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[proxy] error reading upstream body: %v", err)
		return
	}
	w.Write(body)

	if resp.StatusCode == http.StatusOK && h.writeEnabled {
		text := extractResponseText(body)
		if text != "" {
			go h.ingest(rawReq, text)
		}
	}
}

// handleStream proxies SSE events in real time while buffering the full
// response text for the async write path.
func (h *anthropicHandler) handleStream(w http.ResponseWriter, resp *http.Response, rawReq map[string]json.RawMessage) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	var responseBuf strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		fmt.Fprintf(w, "%s\n", line)
		flusher.Flush()

		if strings.HasPrefix(line, "data: ") {
			extractTextDelta(line[6:], &responseBuf)
		}
	}

	if text := responseBuf.String(); text != "" {
		log.Printf("[proxy] stream done, writing %d bytes to store", len(text))
		if h.writeEnabled {
			go h.ingest(rawReq, text)
		}
	} else {
		log.Printf("[proxy] stream done, no text extracted")
	}
}

// ingest stores the assistant response, pairing it with the last user message
// and optionally synthesizing a session summary.
func (h *anthropicHandler) ingest(rawReq map[string]json.RawMessage, assistantText string) {
	if h.write == nil {
		log.Printf("[proxy] pipeline not ready, skipping ingest")
		return
	}
	ctx := context.Background()
	userMsg := extractLastUserMessage(rawReq)

	h.gateAndBuffer(ctx, userMsg, assistantText)

	// Session synthesis: distill every sessionSynthesisInterval new pairs.
	// Fires at minSessionTurns, then every sessionSynthesisInterval pairs after,
	// preventing an ever-growing stack of overlapping session summaries.
	if h.synth.Available() {
		turns := extractConversationTurns(rawReq)
		// Append the current assistant response as the final turn.
		if assistantText != "" {
			turns = append(turns, synthesizer.ConversationTurn{Role: "assistant", Content: assistantText})
		}
		pairs := countPairs(turns)
		if pairs >= minSessionTurns && (pairs == minSessionTurns || pairs%sessionSynthesisInterval == 0) {
			go func() {
				// Strip code blocks from assistant turns before the session synthesis
				// call — the Haiku context window is spent on prose facts, not implementations.
				cleanedTurns := make([]synthesizer.ConversationTurn, len(turns))
				for i, t := range turns {
					if t.Role == "assistant" {
						t.Content = stripFencedCode(t.Content)
					}
					cleanedTurns[i] = t
				}
				facts, err := h.synth.SynthesizeConversation(ctx, cleanedTurns)
				if err != nil {
					log.Printf("[proxy] session synthesis error: %v", err)
					return
				}
				for _, fact := range facts {
					h.write.ProcessDirect(fact, "claude-code-session", nil)
				}
				log.Printf("[proxy] session summary: stored %d fact(s) from %d turns", len(facts), len(turns))
			}()
		}
	}
}

// gateAndBuffer runs the pre-synthesis quality gates in cheapest-first order,
// embeds the exchange (once) for both content-score gating and the exchange
// buffer, and either stores the exchange or buffers it as rejected context.
func (h *anthropicHandler) gateAndBuffer(ctx context.Context, userMsg, assistantText string) {
	pCfg := h.write.Config()

	// Discovery bypass — check on raw text before code stripping, since discovery
	// language ("Wait, that's wrong", "Interesting!") lives in prose not code blocks.
	if rejection.DiscoverySignal(assistantText) {
		log.Printf("[proxy] discovery-signal: bypassing pre-filters (assistant shows discovery language)")
		stripped := stripFencedCode(assistantText)
		vec, _ := h.write.EmbedText(ctx, stripped)
		if h.xbuf.SeenSimilar(vec, 0.85) {
			h.xbuf.Add(userMsg, stripped, vec, false)
			log.Printf("[proxy] exchange-dedup: similar discovery exchange already processed")
			return
		}
		h.xbuf.Add(userMsg, stripped, vec, true)
		h.storeExchange(ctx, userMsg, stripped, vec)
		return
	}

	// Strip fenced code blocks before all downstream processing. Code blocks are
	// implementations, not facts — the prose around them carries the extractable
	// signal. Stripping here means the length gate, content scorer, and Haiku call
	// all operate on prose quality rather than code volume.
	assistantText = stripFencedCode(assistantText)

	// Gate 1: String-match pre-filter (cheapest — no embedding needed).
	// Catches pure acks ("I'll do that") that carry no topical signal.
	if userMsg != "" && rejection.QuickFilter(userMsg, assistantText) {
		h.rejLog.Add(rejection.StagePreFilter, userMsg, assistantText)
		log.Printf("[proxy] pre-filter: skipped procedural exchange (user=%d asst=%d chars)", len(userMsg), len(assistantText))
		return
	}

	// Gate 2: Length gate — too short for durable knowledge.
	// Still buffer (without embedding) — short exchanges carry topic signal.
	if pCfg.IngestMinLen > 0 && len(strings.TrimSpace(assistantText)) < pCfg.IngestMinLen {
		h.rejLog.Add(rejection.StagePreFilter, userMsg, assistantText)
		h.xbuf.Add(userMsg, assistantText, nil, false)
		log.Printf("[proxy] length-filter: skipped short response (%d chars < %d)", len(assistantText), pCfg.IngestMinLen)
		return
	}

	// Embed once — used for exchange dedup, content-score gate, and exchange buffer.
	// When no embedder is available, vec is nil and we skip similarity-based
	// features but the pipeline still functions.
	vec, _ := h.write.EmbedText(ctx, assistantText)

	// Gate 3: Exchange-level dedup — skip synthesis if a similar exchange was
	// already processed. The same Q&A always produces the same embedding, but
	// Haiku rephrases on each call, so fact-level dedup (0.92) misses it.
	// Checking at the exchange level catches it before the API call.
	if h.xbuf.SeenSimilar(vec, 0.85) {
		h.xbuf.Add(userMsg, assistantText, vec, false)
		log.Printf("[proxy] exchange-dedup: similar exchange already processed (sim >= 0.85)")
		return
	}

	// Gate 4: Content score pre-gate — score against noise prototypes.
	//    Do NOT add to rejection store — scorer learns only from Haiku SKIP verdicts.
	if pCfg.ContentScorePreGate > 0 && vec != nil {
		if score, ok := h.write.ScoreVec(vec); ok && score < pCfg.ContentScorePreGate {
			h.xbuf.Add(userMsg, assistantText, vec, false)
			log.Printf("[proxy] content-score-filter: skipped noise (score=%.2f < gate=%.2f)", score, pCfg.ContentScorePreGate)
			return
		}
	}

	// Passed all gates — buffer as accepted and store.
	h.xbuf.Add(userMsg, assistantText, vec, true)
	h.storeExchange(ctx, userMsg, assistantText, vec)
}

// synthesizeAndStore runs the Haiku quality gate in a goroutine and stores the result.
// vec is the pre-computed embedding of assistantText (may be nil).
func (h *anthropicHandler) synthesizeAndStore(ctx context.Context, userMsg, assistantText string, vec []float32) {
	// Build topical context from recently-rejected exchanges that are
	// semantically similar to this one (reuses the same cosine-similarity
	// threshold as the write pipeline's topic grouping).
	topicCtx := h.xbuf.TopicalContext(vec, pipeline.TopicBoundaryThreshold)

	go func() {
		facts, err := h.synth.SynthesizeQA(ctx, userMsg, assistantText, topicCtx)
		if err != nil {
			log.Printf("[proxy] SynthesizeQA error: %v — skipping (quality gate)", err)
			return
		}
		if len(facts) == 0 {
			h.rejLog.Add(rejection.StageSynthesizer, userMsg, assistantText)
			log.Printf("[proxy] SynthesizeQA: exchange skipped (no durable value)")
			return
		}
		for _, fact := range facts {
			h.write.ProcessDirect(fact, "claude-code", nil)
		}
		log.Printf("[proxy] SynthesizeQA: stored %d atomic fact(s)", len(facts))
	}()
}

// storeExchange routes to Haiku synthesis when available. Without synthesis
// there is no quality gate — raw assistant prose (narration, summaries,
// markdown) produces noise rather than facts, so storage is skipped entirely.
// The MCP memory_store tool remains available for explicit, curated storage.
// vec is the pre-computed embedding of assistantText (may be nil).
func (h *anthropicHandler) storeExchange(ctx context.Context, userMsg, assistantText string, vec []float32) {
	if h.synth.Available() {
		h.synthesizeAndStore(ctx, userMsg, assistantText, vec)
	} else {
		log.Printf("[proxy] synthesis not configured — skipping storage (set ANTHROPIC_API_KEY to enable fact extraction)")
	}
}

// countPairs counts the number of complete user+assistant turn pairs.
func countPairs(turns []synthesizer.ConversationTurn) int {
	var users, assistants int
	for _, t := range turns {
		if t.Role == "user" {
			users++
		} else if t.Role == "assistant" {
			assistants++
		}
	}
	if users < assistants {
		return users
	}
	return assistants
}

// --- helpers ---

func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// extractResponseText pulls the assistant text from a non-streaming response.
func extractResponseText(body []byte) string {
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		return ""
	}
	content, ok := resp["content"].([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, block := range content {
		if b, ok := block.(map[string]any); ok {
			if t, ok := b["text"].(string); ok {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// extractTextDelta accumulates text from a streaming content_block_delta event.
func extractTextDelta(data string, buf *strings.Builder) {
	var event map[string]any
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return
	}
	delta, ok := event["delta"].(map[string]any)
	if !ok {
		return
	}
	if t, _ := delta["type"].(string); t == "text_delta" {
		if text, ok := delta["text"].(string); ok {
			buf.WriteString(text)
		}
	}
}

// extractLastUserMessage returns the text of the last user message in the request.
func extractLastUserMessage(rawReq map[string]json.RawMessage) string {
	raw, ok := rawReq["messages"]
	if !ok {
		return ""
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		return ""
	}
	// Walk backwards to find the last user message.
	for i := len(messages) - 1; i >= 0; i-- {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(messages[i], &msg); err != nil {
			continue
		}
		if msg.Role == "user" {
			return extractContentText(msg.Content)
		}
	}
	return ""
}

// extractContentText handles both string content and structured content blocks.
func extractContentText(raw json.RawMessage) string {
	// Try plain string first.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Try array of content blocks.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// stripFencedCode removes fenced code blocks (```...```) from text, leaving
// surrounding prose intact. Inline code spans (`code`) are preserved because
// they carry concrete anchors (function names, flags, error messages).
// Unclosed fences are treated as extending to end-of-string.
func stripFencedCode(text string) string {
	for {
		start := strings.Index(text, "```")
		if start < 0 {
			break
		}
		// Advance past the opening fence line.
		lineEnd := strings.IndexByte(text[start:], '\n')
		if lineEnd < 0 {
			// Unclosed fence with no newline — drop to end.
			text = strings.TrimSpace(text[:start])
			break
		}
		searchFrom := start + lineEnd + 1
		close := strings.Index(text[searchFrom:], "```")
		if close < 0 {
			// Unclosed fence — drop from opening to end.
			text = strings.TrimSpace(text[:start])
			break
		}
		closeAbs := searchFrom + close + 3 // +3 for the closing ``` chars
		text = text[:start] + text[closeAbs:]
	}
	return strings.TrimSpace(text)
}

// extractConversationTurns parses all messages from the request into turns.
func extractConversationTurns(rawReq map[string]json.RawMessage) []synthesizer.ConversationTurn {
	raw, ok := rawReq["messages"]
	if !ok {
		return nil
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		return nil
	}
	turns := make([]synthesizer.ConversationTurn, 0, len(messages))
	for _, m := range messages {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(m, &msg); err != nil {
			continue
		}
		text := extractContentText(msg.Content)
		if text != "" && (msg.Role == "user" || msg.Role == "assistant") {
			turns = append(turns, synthesizer.ConversationTurn{
				Role:    msg.Role,
				Content: text,
			})
		}
	}
	return turns
}
