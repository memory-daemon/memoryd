package synthesizer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mockAnthropicResponse builds a minimal Anthropic messages response body.
func mockAnthropicResponse(text string) []byte {
	resp := map[string]any{
		"id":   "msg_test",
		"type": "message",
		"content": []map[string]any{
			{"type": "text", "text": text},
		},
	}
	b, _ := json.Marshal(resp)
	return b
}

func TestAvailable_NilSynthesizer(t *testing.T) {
	var s *Synthesizer
	if s.Available() {
		t.Error("nil Synthesizer should not be available")
	}
}

func TestAvailable_NoAPIKey(t *testing.T) {
	s := New("", "http://localhost")
	if s.Available() {
		t.Error("Synthesizer with empty apiKey should not be available")
	}
}

func TestAvailable_WithAPIKey(t *testing.T) {
	s := New("sk-test-key", "http://localhost")
	if !s.Available() {
		t.Error("Synthesizer with apiKey should be available")
	}
}

func TestSynthesize_Unavailable_FallsBackToJoin(t *testing.T) {
	s := New("", "http://unused")
	chunks := []string{"chunk one", "chunk two", "chunk three"}
	result, err := s.Synthesize(context.Background(), chunks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "chunk one\n\nchunk two\n\nchunk three" {
		t.Errorf("unexpected fallback result: %q", result)
	}
}

func TestSynthesize_BelowMinChunks_FallsBackToJoin(t *testing.T) {
	s := New("sk-key", "http://unused", WithMinChunks(3))
	// Only 2 chunks — below minChunks=3.
	chunks := []string{"first chunk", "second chunk"}
	result, err := s.Synthesize(context.Background(), chunks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "first chunk\n\nsecond chunk" {
		t.Errorf("expected joined fallback, got: %q", result)
	}
}

func TestSynthesize_CallsAPI(t *testing.T) {
	var capturedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "sk-test" {
			t.Errorf("x-api-key not forwarded")
		}
		json.NewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write(mockAnthropicResponse("Synthesized content about Go concurrency."))
	}))
	defer server.Close()

	s := New("sk-test", server.URL)
	chunks := []string{
		"Go uses goroutines for lightweight concurrency.",
		"Channels are the primary communication mechanism between goroutines.",
	}
	result, err := s.Synthesize(context.Background(), chunks)
	if err != nil {
		t.Fatalf("Synthesize() error: %v", err)
	}
	if result != "Synthesized content about Go concurrency." {
		t.Errorf("unexpected result: %q", result)
	}

	// Verify the prompt was sent.
	msgs, _ := capturedBody["messages"].([]any)
	if len(msgs) != 1 {
		t.Errorf("expected 1 message, got %d", len(msgs))
	}
	msg := msgs[0].(map[string]any)
	content := msg["content"].(string)
	if !strings.Contains(content, "Go uses goroutines") {
		t.Error("prompt should contain the chunk text")
	}
}

func TestSynthesize_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "overloaded", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	s := New("sk-test", server.URL)
	_, err := s.Synthesize(context.Background(), []string{"chunk one", "chunk two"})
	if err == nil {
		t.Error("expected error on API failure")
	}
}

func TestSynthesizeConversation_Unavailable_FallsBackToConcat(t *testing.T) {
	s := New("", "http://unused")
	turns := []ConversationTurn{
		{Role: "user", Content: "How do I fix this error?"},
		{Role: "assistant", Content: "You need to add a nil check."},
	}
	result, err := s.SynthesizeConversation(context.Background(), turns)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "user: How do I fix this error?") {
		t.Errorf("fallback should contain user turn: %q", result)
	}
	if !strings.Contains(result, "assistant: You need to add a nil check.") {
		t.Errorf("fallback should contain assistant turn: %q", result)
	}
}

func TestSynthesizeConversation_SingleTurn_FallsBack(t *testing.T) {
	s := New("sk-key", "http://unused")
	turns := []ConversationTurn{
		{Role: "user", Content: "Just one message."},
	}
	result, err := s.SynthesizeConversation(context.Background(), turns)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "Just one message.") {
		t.Errorf("fallback should include turn content: %q", result)
	}
}

func TestSynthesizeConversation_CallsAPI(t *testing.T) {
	var capturedPrompt string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		msgs := body["messages"].([]any)
		msg := msgs[0].(map[string]any)
		capturedPrompt = msg["content"].(string)

		w.Header().Set("Content-Type", "application/json")
		w.Write(mockAnthropicResponse("## Problem\nFix nil pointer.\n\n## Resolution\nAdded nil check."))
	}))
	defer server.Close()

	s := New("sk-test", server.URL)
	turns := []ConversationTurn{
		{Role: "user", Content: "Getting nil pointer dereference on line 42."},
		{Role: "assistant", Content: "You need to check if the pointer is nil before dereferencing."},
		{Role: "user", Content: "That fixed it, thanks!"},
	}
	result, err := s.SynthesizeConversation(context.Background(), turns)
	if err != nil {
		t.Fatalf("SynthesizeConversation() error: %v", err)
	}
	if !strings.Contains(result, "## Problem") {
		t.Errorf("expected structured result, got: %q", result)
	}

	// Verify all turns appear in the prompt.
	if !strings.Contains(capturedPrompt, "nil pointer dereference") {
		t.Error("prompt should include user turn content")
	}
	if !strings.Contains(capturedPrompt, "check if the pointer is nil") {
		t.Error("prompt should include assistant turn content")
	}
}

func TestNilSynthesizer_Synthesize(t *testing.T) {
	var s *Synthesizer
	result, err := s.Synthesize(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("nil Synthesizer.Synthesize() should not error: %v", err)
	}
	if result != "a\n\nb" {
		t.Errorf("expected joined fallback, got: %q", result)
	}
}

func TestNilSynthesizer_SynthesizeConversation(t *testing.T) {
	var s *Synthesizer
	turns := []ConversationTurn{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "world"},
	}
	result, err := s.SynthesizeConversation(context.Background(), turns)
	if err != nil {
		t.Fatalf("nil Synthesizer.SynthesizeConversation() should not error: %v", err)
	}
	if !strings.Contains(result, "hello") {
		t.Errorf("expected fallback with turn content, got: %q", result)
	}
}

func TestOptions(t *testing.T) {
	s := New("key", "http://base",
		WithModel("claude-opus-4-6"),
		WithMaxTokens(512),
		WithMinChunks(4),
	)
	if s.model != "claude-opus-4-6" {
		t.Errorf("model = %q", s.model)
	}
	if s.maxTokens != 512 {
		t.Errorf("maxTokens = %d", s.maxTokens)
	}
	if s.minChunks != 4 {
		t.Errorf("minChunks = %d", s.minChunks)
	}
}

// ---------------------------------------------------------------------------
// SynthesizeQA tests
// ---------------------------------------------------------------------------

func TestSynthesizeQA_Unavailable_ReturnsEmpty(t *testing.T) {
	s := New("", "http://unused")
	result, err := s.SynthesizeQA(context.Background(), "question?", "answer.")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty result when unavailable, got: %q", result)
	}
}

func TestSynthesizeQA_NilSynthesizer(t *testing.T) {
	var s *Synthesizer
	result, err := s.SynthesizeQA(context.Background(), "q", "a")
	if err != nil {
		t.Fatalf("nil Synthesizer.SynthesizeQA() should not error: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty result for nil synthesizer, got: %q", result)
	}
}

func TestSynthesizeQA_SKIP_ReturnsEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(mockAnthropicResponse("SKIP"))
	}))
	defer server.Close()

	s := New("sk-test", server.URL)
	result, err := s.SynthesizeQA(context.Background(), "Let me check that file", "OK, looking at it now.")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty result for SKIP, got: %q", result)
	}
}

func TestSynthesizeQA_SKIP_WithWhitespace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(mockAnthropicResponse("  SKIP\n"))
	}))
	defer server.Close()

	s := New("sk-test", server.URL)
	result, err := s.SynthesizeQA(context.Background(), "q", "a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "" {
		t.Errorf("SKIP with whitespace should still return empty, got: %q", result)
	}
}

func TestSynthesizeQA_ReturnsContent(t *testing.T) {
	const synthesized = "The proxy server in internal/proxy/proxy.go binds to 127.0.0.1:7432 and enriches requests via the read pipeline before forwarding to the upstream Anthropic API."
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(mockAnthropicResponse(synthesized))
	}))
	defer server.Close()

	s := New("sk-test", server.URL)
	result, err := s.SynthesizeQA(context.Background(), "How does the proxy work?", "The proxy binds to 127.0.0.1:7432...")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != synthesized {
		t.Errorf("expected synthesized content, got: %q", result)
	}
}

func TestSynthesizeQA_PromptContainsUserAndAssistant(t *testing.T) {
	var capturedPrompt string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		msgs := body["messages"].([]any)
		msg := msgs[0].(map[string]any)
		capturedPrompt = msg["content"].(string)
		w.Header().Set("Content-Type", "application/json")
		w.Write(mockAnthropicResponse("SKIP"))
	}))
	defer server.Close()

	s := New("sk-test", server.URL)
	_, _ = s.SynthesizeQA(context.Background(), "Why does config use port 7432?", "Because that port is unlikely to conflict.")

	if !strings.Contains(capturedPrompt, "USER: Why does config use port 7432?") {
		t.Error("prompt should contain USER: prefix with the question")
	}
	if !strings.Contains(capturedPrompt, "ASSISTANT: Because that port is unlikely to conflict.") {
		t.Error("prompt should contain ASSISTANT: prefix with the answer")
	}
}

func TestSynthesizeQA_EmptyQuestion_UsesAssistantOutputFrame(t *testing.T) {
	var capturedPrompt string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		msgs := body["messages"].([]any)
		msg := msgs[0].(map[string]any)
		capturedPrompt = msg["content"].(string)
		w.Header().Set("Content-Type", "application/json")
		w.Write(mockAnthropicResponse("SKIP"))
	}))
	defer server.Close()

	s := New("sk-test", server.URL)
	_, _ = s.SynthesizeQA(context.Background(), "", "The embedder uses voyage-4-nano at 1024 dimensions.")

	if !strings.Contains(capturedPrompt, "ASSISTANT OUTPUT:") {
		t.Error("empty question should use ASSISTANT OUTPUT: frame, not USER:/ASSISTANT:")
	}
	if strings.Contains(capturedPrompt, "USER:") {
		t.Error("empty question should not include USER: line")
	}
}

func TestSynthesizeQA_PromptHasTwoStageStructure(t *testing.T) {
	var capturedPrompt string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		msgs := body["messages"].([]any)
		msg := msgs[0].(map[string]any)
		capturedPrompt = msg["content"].(string)
		w.Header().Set("Content-Type", "application/json")
		w.Write(mockAnthropicResponse("SKIP"))
	}))
	defer server.Close()

	s := New("sk-test", server.URL)
	_, _ = s.SynthesizeQA(context.Background(), "q", "a")

	if !strings.Contains(capturedPrompt, "STAGE 1: VALUE GATE") {
		t.Error("prompt should contain STAGE 1: VALUE GATE")
	}
	if !strings.Contains(capturedPrompt, "STAGE 2: REWRITE") {
		t.Error("prompt should contain STAGE 2: REWRITE")
	}
	if !strings.Contains(capturedPrompt, "journalistic prose") {
		t.Error("prompt should instruct journalistic prose output")
	}
	if !strings.Contains(capturedPrompt, "Accept that value without question") {
		t.Error("prompt should instruct unconditional acceptance of raw value")
	}
}

func TestSynthesizeQA_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"type":"overloaded_error"}}`, http.StatusServiceUnavailable)
	}))
	defer server.Close()

	s := New("sk-test", server.URL)
	_, err := s.SynthesizeQA(context.Background(), "q", "a")
	if err == nil {
		t.Error("expected error on API failure")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error should contain status code, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Synthesize prompt structure tests
// ---------------------------------------------------------------------------

func TestSynthesize_PromptContainsJournalisticInstruction(t *testing.T) {
	var capturedPrompt string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		msgs := body["messages"].([]any)
		msg := msgs[0].(map[string]any)
		capturedPrompt = msg["content"].(string)
		w.Header().Set("Content-Type", "application/json")
		w.Write(mockAnthropicResponse("Combined fact about the system."))
	}))
	defer server.Close()

	s := New("sk-test", server.URL)
	_, _ = s.Synthesize(context.Background(), []string{"chunk one", "chunk two"})

	if !strings.Contains(capturedPrompt, "journalistic prose") {
		t.Error("Synthesize prompt should instruct journalistic prose")
	}
	if !strings.Contains(capturedPrompt, "Accept the raw informational value") {
		t.Error("Synthesize prompt should instruct accepting raw value")
	}
}

func TestSynthesizeConversation_PromptContainsJournalisticInstruction(t *testing.T) {
	var capturedPrompt string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		msgs := body["messages"].([]any)
		msg := msgs[0].(map[string]any)
		capturedPrompt = msg["content"].(string)
		w.Header().Set("Content-Type", "application/json")
		w.Write(mockAnthropicResponse("Session summary about the fix."))
	}))
	defer server.Close()

	s := New("sk-test", server.URL)
	turns := []ConversationTurn{
		{Role: "user", Content: "Fix the bug."},
		{Role: "assistant", Content: "The nil check was missing."},
	}
	_, _ = s.SynthesizeConversation(context.Background(), turns)

	if !strings.Contains(capturedPrompt, "journalistic prose") {
		t.Error("SynthesizeConversation prompt should instruct journalistic prose")
	}
	if !strings.Contains(capturedPrompt, "Accept the raw informational value") {
		t.Error("SynthesizeConversation prompt should instruct accepting raw value")
	}
}
