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

// Default prompt templates used by each synthesis function. These are surfaced
// in the dashboard so operators can see exactly what the LLM receives.

// DefaultMergePrompt is the system prompt used by Synthesize() to merge
// related text chunks into atomic facts. Uses the same FACT: format as
// SynthesizeQA so parseFacts() handles both paths identically.
const DefaultMergePrompt = `Extract every distinct technical fact from these related fragments. Remove duplication.

Rules:
- One fact per line, prefixed with "FACT: "
- Lead with the concrete anchor: file path, function name, config key, error message.
- State the fact and the reason. No narration, no preamble, no interpretation.
- No forward-looking statements ("you should", "next time", "remember to").
- 1-2 sentences per fact. Concrete identifiers only.
- If there are no distinct technical facts, respond with exactly: SKIP

Fragments:

`

// DefaultQAPrompt is the system prompt used by SynthesizeQA() — the mandatory
// quality gate for all proxy-captured content. It performs value gating, atomic
// decomposition, and noise filtering in a single LLM call.
const DefaultQAPrompt = `Extract reusable technical facts from this coding exchange. Each fact must stand alone — one fact per line.

GATE: Does this exchange contain a non-obvious technical fact? A fact is: a root cause, a gotcha, an error-fix pair, a config value that matters, an architectural constraint, a dependency quirk, a decision and its rationale, a performance characteristic.

NOT a fact: what someone did or plans to do, task progress, status updates, restatements of requirements, well-documented patterns, procedural narration. If there are no facts, respond with exactly: SKIP

FORMAT:
- Each fact on its own line, prefixed with "FACT: "
- Lead with the specific anchor: file path, function name, config key, error message.
- State the fact, then the reason. No interpretation, no advice, no hedging.
- 1-2 sentences per fact. Concrete identifiers only.

Bad: "The team discovered that the build was failing due to a caching issue and decided to fix it by clearing the cache."
Good: "FACT: go build caches cgo artifacts by architecture — cross-compiling from arm64 to amd64 requires go clean -cache first or the linker emits symbol errors."

Bad: "I'll need to update the configuration to handle the new authentication requirements."
Good: "FACT: OAuth2 PKCE flow in auth/handler.go requires code_verifier stored in session before the redirect — stateless mode breaks the exchange."

---
[exchange]
---

Output FACT: lines or SKIP. Nothing else.`

// DefaultConversationPrompt is the system prompt used by SynthesizeConversation()
// to distill a multi-turn coding session into atomic facts. Uses the same FACT:
// format as DefaultQAPrompt so parseFacts() handles all synthesis paths identically.
const DefaultConversationPrompt = `Extract the reusable technical facts from this coding session. Ignore the task narrative — what was done is in git. Extract what is not in git: root causes, gotchas, constraints, rationale, non-obvious wiring.

GATE: If there are no non-obvious facts, respond with exactly: SKIP

FORMAT:
- Each fact on its own line, prefixed with "FACT: "
- Lead with the concrete anchor: file path, function name, config key, error message.
- State the fact and the reason. No narration, no "we discovered", no "this means".
- No forward-looking language ("next time", "you should", "will need to").
- No task summaries ("implemented X", "fixed Y", "added Z").
- 1-2 sentences per fact. Concrete identifiers only.

Output FACT: lines or SKIP. Nothing else.

Conversation:

[turns]`

// PromptTemplates returns the active prompt templates keyed by name.
// Custom overrides take precedence over the built-in defaults.
func (s *Synthesizer) PromptTemplates() map[string]string {
	if s == nil {
		return DefaultPromptTemplates()
	}
	m := DefaultPromptTemplates()
	if s.customQA != "" {
		m["qa"] = s.customQA
	}
	if s.customMerge != "" {
		m["merge"] = s.customMerge
	}
	if s.customConversation != "" {
		m["conversation"] = s.customConversation
	}
	return m
}

// DefaultPromptTemplates returns the built-in default prompt templates.
func DefaultPromptTemplates() map[string]string {
	return map[string]string{
		"qa":           DefaultQAPrompt,
		"merge":        DefaultMergePrompt,
		"conversation": DefaultConversationPrompt,
	}
}

// PromptTemplates is a package-level accessor returning the built-in default
// prompt templates. Retained for backwards compatibility with callers that
// don't have access to a Synthesizer instance.
func PromptTemplates() map[string]string {
	return DefaultPromptTemplates()
}

// SetCustomPrompts updates the custom prompt overrides. Empty strings
// revert to the built-in defaults.
func (s *Synthesizer) SetCustomPrompts(qa, merge, conversation string) {
	if s == nil {
		return
	}
	s.customQA = qa
	s.customMerge = merge
	s.customConversation = conversation
}

// CustomPrompts returns the current custom overrides (empty = using default).
func (s *Synthesizer) CustomPrompts() (qa, merge, conversation string) {
	if s == nil {
		return "", "", ""
	}
	return s.customQA, s.customMerge, s.customConversation
}

// qaPrompt returns the active QA prompt (custom or default).
func (s *Synthesizer) qaPrompt() string {
	if s.customQA != "" {
		return s.customQA
	}
	return DefaultQAPrompt
}

// mergePrompt returns the active merge prompt (custom or default).
func (s *Synthesizer) mergePrompt() string {
	if s.customMerge != "" {
		return s.customMerge
	}
	return DefaultMergePrompt
}

// conversationPrompt returns the active conversation prompt (custom or default).
func (s *Synthesizer) conversationPrompt() string {
	if s.customConversation != "" {
		return s.customConversation
	}
	return DefaultConversationPrompt
}

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

	// Custom prompt overrides — empty means use the Default* constants.
	customQA           string
	customMerge        string
	customConversation string
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
		"model":                 model,
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

// Synthesize extracts atomic facts from a set of related text chunks.
// Returns each fact as a separate string via parseFacts().
// If len(chunks) < minChunks or the synthesizer is unavailable, returns
// the chunks unchanged so the caller can store them individually.
func (s *Synthesizer) Synthesize(ctx context.Context, chunks []string) ([]string, error) {
	if !s.Available() || len(chunks) < s.minChunks {
		return chunks, nil
	}

	combined := strings.Join(chunks, "\n\n---\n\n")
	prompt := s.mergePrompt() + combined

	raw, err := s.complete(ctx, prompt)
	if err != nil {
		return nil, err
	}
	facts := parseFacts(raw)
	if len(facts) == 0 {
		// Model returned SKIP or nothing useful — fall back to storing chunks as-is.
		return chunks, nil
	}
	return facts, nil
}

// SynthesizeQA is the mandatory quality gate for ALL proxy-captured content.
// It performs value gating, atomic decomposition, and noise filtering in a
// single LLM call. Returns a list of independent atomic facts ready to store,
// or (nil, nil) when the exchange has no durable value.
//
// When question is empty (proxy couldn't extract a user message), the assistant
// text is evaluated on its own merits — the same quality bar applies.
//
// topicHint is an optional short string describing the surrounding conversation
// topic (e.g. from recently-rejected exchanges). It anchors the rewritten
// memory in its topical context so it's useful when retrieved in isolation.
func (s *Synthesizer) SynthesizeQA(ctx context.Context, question, answer, topicHint string) ([]string, error) {
	if !s.Available() {
		return nil, nil
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

	prompt := strings.Replace(s.qaPrompt(), "[exchange]", exchangeBlock+topicBlock, 1)

	result, err := s.complete(ctx, prompt)
	if err != nil {
		return nil, err
	}
	return parseFacts(result), nil
}

// parseFacts extracts atomic facts from the LLM response. Returns nil when
// the model signals SKIP or produces no parseable facts.
func parseFacts(raw string) []string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == skipSentinel || strings.HasPrefix(trimmed, skipSentinel+"\n") || strings.HasPrefix(trimmed, skipSentinel+" ") {
		return nil
	}

	var facts []string
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "FACT: "); ok {
			if f := strings.TrimSpace(after); f != "" {
				facts = append(facts, f)
			}
		}
	}
	return facts
}

// SynthesizeConversation distills a multi-turn coding session into atomic facts.
// Returns each fact as a separate string. Returns (nil, nil) when the synthesizer
// is unavailable or the conversation has fewer than 2 turns — callers should skip
// storage in that case since the per-exchange path already handles individual turns.
func (s *Synthesizer) SynthesizeConversation(ctx context.Context, turns []ConversationTurn) ([]string, error) {
	if !s.Available() || len(turns) < 2 {
		return nil, nil
	}

	var convBuf strings.Builder
	for _, t := range turns {
		fmt.Fprintf(&convBuf, "**%s:** %s\n\n", t.Role, t.Content)
	}

	prompt := strings.Replace(s.conversationPrompt(), "[turns]", convBuf.String(), 1)

	raw, err := s.complete(ctx, prompt)
	if err != nil {
		return nil, err
	}
	facts := parseFacts(raw)
	// nil facts (SKIP or empty) is a valid signal — no storage needed.
	return facts, nil
}
