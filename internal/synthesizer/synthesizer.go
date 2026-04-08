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
	prompt := `You are a memory curator for an AI coding assistant's long-term knowledge store. These fragments are related content that should be distilled into a single entry.

A future AI assistant will search this memory store at the start of a new session to avoid re-discovering context. Accept the raw informational value in these fragments without question. Your job is to rewrite that value as clearly as possible.

Rewrite as direct, journalistic prose. State facts plainly. Lead with the most important finding. Every sentence should carry specific, concrete information: file paths, function names, config keys, error messages, version numbers, exact identifiers. No filler, no preamble, no hedging.

Write 2-8 sentences covering the key facts from across the fragments. Each sentence must stand alone as a useful search result. Include the "why" behind any decision. Prefer precision over brevity and never sacrifice a specific identifier to save words.

Omit:
- Process narration ("first I read X, then I changed Y")
- Generic observations without specific technical anchors
- Anything obvious from reading the code

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
func (s *Synthesizer) SynthesizeQA(ctx context.Context, question, answer string) (string, error) {
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

	prompt := fmt.Sprintf(`You are a memory curator for an AI coding assistant's long-term knowledge store.

YOUR TASK HAS TWO STAGES. Apply them in order.

STAGE 1: VALUE GATE
Determine whether this text contains independently valuable technical information. "Independently valuable" means a future AI instance encountering this memory in isolation, with no surrounding conversation, would learn something concrete and useful about the codebase, system, or domain.

Valuable information includes: architectural facts, file locations, configuration values, API behaviors, error patterns, decisions with rationale, constraints and gotchas, implementation patterns with specific names, dependency relationships, performance characteristics, or any concrete technical fact that saves future exploration.

Not valuable: procedural narration ("I looked at X", "I made the changes"), mid-task navigation ("Let me check...", "Now fix..."), status updates, generic explanations without specific anchors, restatements of requirements, or anything that describes process rather than findings. If the text is any of these, respond with exactly: SKIP

STAGE 2: REWRITE
If you reach this stage, the text has raw informational value. Accept that value without question. Your job is not to evaluate or second-guess the content but to make it maximally clear and retrievable.

Rewrite the information as direct, journalistic prose. State facts plainly. Lead with the most important finding. Every sentence should carry specific, concrete information: file paths, function names, config keys, error messages, version numbers, exact identifiers. No filler, no preamble, no hedging.

Write 2-6 sentences in a single paragraph, or use a short heading followed by 2-6 sentences if the topic benefits from labeling. Each sentence must stand alone as a useful search result. Include the "why" behind any decision. Prefer precision over brevity and never sacrifice a specific identifier to save words.

---
%s
---

Output the rewritten memory directly, or SKIP. Nothing else.`, exchangeBlock)

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

	prompt := fmt.Sprintf(`You are a memory curator for an AI coding assistant's long-term knowledge store. This is a complete coding session arc.

Create a compact session summary capturing what a future AI instance starting fresh on this project would need to know. Accept the raw informational value in this conversation without question. Your job is to identify what was learned and restate it as clearly as possible.

Write in direct, journalistic prose. Lead with the most important discovery. Every sentence should carry specific, concrete information: file paths, function names, config keys, error messages, exact identifiers.

Cover these if present:
- The core problem and its non-obvious root cause
- Key files and functions discovered and why they matter
- Decisions made and the reasoning behind them
- What was tried and failed, so future sessions avoid repeating dead ends
- Config values, env vars, thresholds, or external dependencies that were pinned
- Architecture facts: how modules connect, where things live, what the data flow is

Omit:
- The sequence of steps taken (re-derivable from git history)
- Restatements of the user's requirements
- Generic explanations of well-documented patterns
- Confirmations that tasks were completed

Write 4-12 sentences of clear prose. Include the "why" behind every decision. Prefer precision over brevity. Under 350 words. Output directly, no preamble.

Conversation:

%s`, convBuf.String())

	return s.complete(ctx, prompt)
}
