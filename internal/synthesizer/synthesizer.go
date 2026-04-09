// Package synthesizer uses an LLM to distill topic groups and conversation
// arcs into coherent, standalone memory entries.
package synthesizer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	defaultModel     = "claude-haiku-4-5-20251001"
	defaultMaxTokens = 1024
	defaultMinChunks = 2
)

// skipSentinel is the exact string the model returns when an exchange has no
// durable technical value and should be dropped entirely.
const skipSentinel = "SKIP"

// ConversationTurn is a single message in a conversation arc.
type ConversationTurn struct {
	Role    string // "user" or "assistant"
	Content string
}

// backend abstracts the LLM completion call so the Synthesizer works
// with both Anthropic and Azure OpenAI.
type backend interface {
	complete(ctx context.Context, model string, maxTokens int, prompt string) (string, error)
	available() bool
}

// Synthesizer calls an LLM to synthesize fragmented text into
// coherent, standalone memory entries. All methods are nil-safe — a nil
// Synthesizer is a no-op and Available() returns false.
type Synthesizer struct {
	be        backend
	model     string
	maxTokens int
	minChunks int
}

// Option configures a Synthesizer.
type Option func(*Synthesizer)

// WithModel overrides the default model.
func WithModel(model string) Option {
	return func(s *Synthesizer) { s.model = model }
}

// WithMaxTokens overrides the default max output tokens.
func WithMaxTokens(n int) Option {
	return func(s *Synthesizer) { s.maxTokens = n }
}

// WithMinChunks sets the minimum number of chunks needed before synthesis
// is attempted. Groups smaller than this are returned as-is via Join.
func WithMinChunks(n int) Option {
	return func(s *Synthesizer) { s.minChunks = n }
}

// New creates a Synthesizer using the Anthropic API. Pass an empty apiKey
// to disable synthesis (Available() will return false).
func New(apiKey, baseURL string, opts ...Option) *Synthesizer {
	s := &Synthesizer{
		be: &anthropicBackend{
			apiKey:  apiKey,
			baseURL: strings.TrimRight(baseURL, "/"),
			client:  &http.Client{},
		},
		model:     defaultModel,
		maxTokens: defaultMaxTokens,
		minChunks: defaultMinChunks,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// AzureConfig holds the connection parameters for Azure OpenAI.
type AzureConfig struct {
	Endpoint   string // e.g. https://myresource.openai.azure.com
	Deployment string // deployment name in Azure portal
	APIVersion string // e.g. 2024-06-01
	APIKey     string // primary or secondary key
}

// NewAzure creates a Synthesizer using Azure OpenAI. Pass an empty APIKey
// to disable synthesis.
func NewAzure(cfg AzureConfig, opts ...Option) *Synthesizer {
	s := &Synthesizer{
		be: &azureBackend{
			endpoint:   strings.TrimRight(cfg.Endpoint, "/"),
			deployment: cfg.Deployment,
			apiVersion: cfg.APIVersion,
			apiKey:     cfg.APIKey,
			client:     &http.Client{},
		},
		model:     cfg.Deployment, // Azure uses deployment name as the model label
		maxTokens: defaultMaxTokens,
		minChunks: defaultMinChunks,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Available returns true when synthesis is possible (non-nil, backend ready).
func (s *Synthesizer) Available() bool {
	return s != nil && s.be != nil && s.be.available()
}

// complete dispatches to the configured backend.
func (s *Synthesizer) complete(ctx context.Context, prompt string) (string, error) {
	return s.be.complete(ctx, s.model, s.maxTokens, prompt)
}

// ---------------------------------------------------------------------------
// Anthropic backend
// ---------------------------------------------------------------------------

type anthropicBackend struct {
	apiKey  string
	baseURL string
	client  *http.Client
}

func (ab *anthropicBackend) available() bool {
	return ab.apiKey != ""
}

func (ab *anthropicBackend) complete(ctx context.Context, model string, maxTokens int, prompt string) (string, error) {
	reqBody, err := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": maxTokens,
		"messages": []map[string]any{
			{"role": "user", "content": prompt},
		},
	})
	if err != nil {
		return "", fmt.Errorf("synthesizer: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		ab.baseURL+"/v1/messages", bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("synthesizer: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", ab.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := ab.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("synthesizer: API call: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("synthesizer: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("synthesizer: API error %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("synthesizer: parse response: %w", err)
	}

	var parts []string
	for _, block := range result.Content {
		if block.Type == "text" && block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("synthesizer: empty response")
	}
	return strings.TrimSpace(strings.Join(parts, "\n")), nil
}

// ---------------------------------------------------------------------------
// Azure OpenAI backend
// ---------------------------------------------------------------------------

type azureBackend struct {
	endpoint   string
	deployment string
	apiVersion string
	apiKey     string
	client     *http.Client
}

func (az *azureBackend) available() bool {
	return az.apiKey != "" && az.endpoint != "" && az.deployment != ""
}

func (az *azureBackend) complete(ctx context.Context, model string, maxTokens int, prompt string) (string, error) {
	reqBody, err := json.Marshal(map[string]any{
		"model":                model,
		"max_completion_tokens": maxTokens,
		"messages": []map[string]any{
			{"role": "user", "content": prompt},
		},
	})
	if err != nil {
		return "", fmt.Errorf("synthesizer: marshal request: %w", err)
	}

	url := az.endpoint + "/openai/v1/chat/completions"

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("synthesizer: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("api-key", az.apiKey)

	resp, err := az.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("synthesizer: API call: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("synthesizer: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("synthesizer: API error %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("synthesizer: parse response: %w", err)
	}

	if len(result.Choices) == 0 || result.Choices[0].Message.Content == "" {
		return "", fmt.Errorf("synthesizer: empty response")
	}
	return strings.TrimSpace(result.Choices[0].Message.Content), nil
}

// Synthesize merges a set of related text chunks into a single coherent entry.
// If len(chunks) < minChunks or the synthesizer is unavailable, it falls back
// to joining with "\n\n".
func (s *Synthesizer) Synthesize(ctx context.Context, chunks []string) (string, error) {
	if !s.Available() || len(chunks) < s.minChunks {
		return strings.Join(chunks, "\n\n"), nil
	}

	combined := strings.Join(chunks, "\n\n---\n\n")
	prompt := `You are a memory curator for an AI coding assistant's long-term knowledge store. Every entry you write is a SIGNPOST FOR FUTURE AGENTS. These fragments are related content that should be distilled into a single entry.

A future AI assistant will search this memory store at the start of a new session to orient itself. Extract the core insights — the aha moments, gotchas, non-obvious facts, and decision rationale — and write them as a single signpost entry.

Lead with the most important discovery. Every sentence should carry specific, concrete information: file paths, function names, config keys, error messages, version numbers, exact identifiers. No filler, no preamble.

Write 2-8 sentences. Focus on what was LEARNED, not what was DONE. Include the "why" behind decisions. Omit task summaries, step sequences, and generic explanations. Each sentence should be independently useful as a search result.

Output the entry directly. No preamble.

Fragments:

` + combined

	return s.complete(ctx, prompt)
}

// SynthesizeQA distills a user question + assistant answer (or assistant-only
// text) into a memory entry, or returns ("", nil) if it has no durable value.
//
// This is the mandatory quality gate for ALL proxy-captured content. The model
// returns the sentinel "SKIP" for procedural exchanges ("I'll look at that",
// "I've made the changes") that carry no reusable knowledge.
//
// When question is empty (proxy couldn't extract a user message), the assistant
// text is evaluated on its own merits — the same quality bar applies.
//
// topicHint is an optional short string describing the surrounding conversation
// topic (e.g. from recently-rejected exchanges). It anchors the rewritten
// memory in its topical context so it's useful when retrieved in isolation.
func (s *Synthesizer) SynthesizeQA(ctx context.Context, question, answer, topicHint string) (string, error) {
	if !s.Available() {
		return "", nil
	}

	// When no user message is available, evaluate the assistant output alone
	// rather than framing it as a conversation with an empty USER: line.
	var exchangeBlock string
	if strings.TrimSpace(question) != "" {
		exchangeBlock = fmt.Sprintf("USER: %s\n\nASSISTANT: %s", question, answer)
	} else {
		exchangeBlock = fmt.Sprintf("ASSISTANT OUTPUT:\n%s", answer)
	}

	var topicBlock string
	if topicHint != "" {
		topicBlock = fmt.Sprintf("\n\nSURROUNDING TOPIC CONTEXT (for anchoring only — do not store this directly):\n%s", topicHint)
	}

	prompt := fmt.Sprintf(`You are a memory curator for an AI coding assistant's long-term knowledge store. Every entry you write is a SIGNPOST FOR FUTURE AGENTS — an agent starting fresh on this codebase will search this store to orient itself.

YOUR TASK HAS TWO STAGES. Apply them in order.

STAGE 1: VALUE GATE
Determine whether this exchange contains an insight a future agent would benefit from. "Insight" means a non-obvious discovery, gotcha, root cause, architectural fact, or decision rationale that would save a future agent from re-discovering it the hard way.

Valuable: root causes of bugs, non-obvious gotchas, "why" behind a decision, architectural constraints, dependency quirks, config values that matter, error patterns and their fixes, performance characteristics, integration patterns.

Not valuable: procedural narration ("I looked at X", "I made the changes"), task completion summaries (these duplicate git history), generic explanations of well-documented patterns, status updates, restatements of requirements. If the text is any of these, respond with exactly: SKIP

STAGE 2: REWRITE AS A SIGNPOST
If you reach this stage, extract the core insight — the aha moment, the gotcha, the thing that would save a future agent time — and write it as a signpost.

Frame it as guidance a future agent would find in a search result. Lead with the specific technical anchor (file, function, config, error). State what was discovered and why it matters. Include enough topical context that the entry makes sense in isolation (what project area, what service, what problem domain).

Write 2-5 sentences. Every sentence must carry a specific technical identifier (file path, function name, config key, error message, version). No filler, no preamble. Include the "why" behind decisions. Do NOT summarize the task that was being done — focus on what was LEARNED.

If a SURROUNDING TOPIC CONTEXT block is provided below, use it to anchor your entry with the right project/service/domain context, but do not store the topic context itself.

---
%s%s
---

Output the signpost directly, or SKIP. Nothing else.`, exchangeBlock, topicBlock)

	result, err := s.complete(ctx, prompt)
	if err != nil {
		return "", err
	}
	trimmed := strings.TrimSpace(result)
	if trimmed == skipSentinel || strings.HasPrefix(trimmed, skipSentinel+"\n") || strings.HasPrefix(trimmed, skipSentinel+" ") {
		return "", nil
	}
	// Also catch "STAGE 1: VALUE GATE" preamble where the model echoes the prompt
	// structure instead of outputting the rewritten memory directly.
	if strings.HasPrefix(trimmed, "STAGE 1:") || strings.HasPrefix(trimmed, "STAGE 2:") {
		return "", nil
	}
	return result, nil
}

// SynthesizeConversation distills a multi-turn conversation into a structured
// memory entry capturing the problem, approach, and resolution.
func (s *Synthesizer) SynthesizeConversation(ctx context.Context, turns []ConversationTurn) (string, error) {
	if !s.Available() || len(turns) < 2 {
		// Fall back: concatenate turns with role labels.
		var parts []string
		for _, t := range turns {
			parts = append(parts, fmt.Sprintf("%s: %s", t.Role, t.Content))
		}
		return strings.Join(parts, "\n\n"), nil
	}

	var convBuf strings.Builder
	for _, t := range turns {
		fmt.Fprintf(&convBuf, "**%s:** %s\n\n", t.Role, t.Content)
	}

	prompt := fmt.Sprintf(`You are a memory curator for an AI coding assistant's long-term knowledge store. Every entry you write is a SIGNPOST FOR FUTURE AGENTS. This is a complete coding session arc.

Extract the insights from this session that would help a future agent working on the same codebase. Focus on what was DISCOVERED, not what was DONE. A future agent can read git history to see what changed — what it cannot see is the reasoning, the gotchas, the dead ends, and the non-obvious root causes.

Write as a series of signposts, each anchored to a specific technical fact. Lead with the biggest aha moment. Every sentence should carry specific, concrete information: file paths, function names, config keys, error messages, exact identifiers.

Extract these if present:
- Non-obvious root causes and why they were hard to find
- Gotchas, traps, and dead ends a future agent should avoid
- Decisions made and the reasoning behind them (the "why" matters most)
- Architectural facts discovered: how modules connect, data flow, dependency quirks
- Config values, thresholds, or external dependencies that matter and why

Omit:
- Task summaries ("implemented X", "fixed Y", "added Z") — these duplicate git history
- The sequence of steps taken
- Restatements of the user's requirements
- Generic explanations of well-documented patterns

Write 3-10 sentences. Each sentence should be independently useful as a search result. Include the "why" behind every decision. Prefer precision over brevity. Under 300 words. Output directly, no preamble.

Conversation:

%s`, convBuf.String())

	return s.complete(ctx, prompt)
}
