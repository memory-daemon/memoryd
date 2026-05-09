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

func TestSynthesize_Unavailable_FallsBackToChunks(t *testing.T) {
	s := New("", "http://unused")
	chunks := []string{"chunk one", "chunk two", "chunk three"}
	result, err := s.Synthesize(context.Background(), chunks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 3 || result[0] != "chunk one" || result[1] != "chunk two" || result[2] != "chunk three" {
		t.Errorf("expected chunks unchanged, got: %v", result)
	}
}

func TestSynthesize_BelowMinChunks_FallsBackToChunks(t *testing.T) {
	s := New("sk-key", "http://unused", WithMinChunks(3))
	// Only 2 chunks — below minChunks=3.
	chunks := []string{"first chunk", "second chunk"}
	result, err := s.Synthesize(context.Background(), chunks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 2 || result[0] != "first chunk" || result[1] != "second chunk" {
		t.Errorf("expected chunks unchanged, got: %v", result)
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
		w.Write(mockAnthropicResponse("FACT: goroutines: Go uses goroutines for lightweight concurrency; channels are the primary communication mechanism."))
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
	if len(result) != 1 {
		t.Fatalf("expected 1 fact, got %d: %v", len(result), result)
	}
	if !strings.Contains(result[0], "goroutines") {
		t.Errorf("fact should contain 'goroutines', got: %q", result[0])
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

func TestSynthesizeConversation_Unavailable_ReturnsNil(t *testing.T) {
	s := New("", "http://unused")
	turns := []ConversationTurn{
		{Role: "user", Content: "How do I fix this error?"},
		{Role: "assistant", Content: "You need to add a nil check."},
	}
	result, err := s.SynthesizeConversation(context.Background(), turns)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("unavailable synthesizer should return nil, got: %v", result)
	}
}

func TestSynthesizeConversation_SingleTurn_ReturnsNil(t *testing.T) {
	s := New("sk-key", "http://unused")
	turns := []ConversationTurn{
		{Role: "user", Content: "Just one message."},
	}
	result, err := s.SynthesizeConversation(context.Background(), turns)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("single-turn should return nil, got: %v", result)
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
		w.Write(mockAnthropicResponse("FACT: nil pointer on line 42 in store/query.go — pointer returned by DB scan is nil when no rows match; check rows.Next() before dereferencing."))
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
	if len(result) != 1 {
		t.Fatalf("expected 1 fact, got %d: %v", len(result), result)
	}
	if !strings.Contains(result[0], "nil pointer") {
		t.Errorf("fact should mention nil pointer, got: %q", result[0])
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
	if len(result) != 2 || result[0] != "a" || result[1] != "b" {
		t.Errorf("expected chunks unchanged, got: %v", result)
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
	if result != nil {
		t.Errorf("nil synthesizer should return nil, got: %v", result)
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

func TestSynthesizeQA_Unavailable_ReturnsNil(t *testing.T) {
	s := New("", "http://unused")
	result, err := s.SynthesizeQA(context.Background(), "question?", "answer.", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected nil result when unavailable, got: %v", result)
	}
}

func TestSynthesizeQA_NilSynthesizer(t *testing.T) {
	var s *Synthesizer
	result, err := s.SynthesizeQA(context.Background(), "q", "a", "")
	if err != nil {
		t.Fatalf("nil Synthesizer.SynthesizeQA() should not error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected nil result for nil synthesizer, got: %v", result)
	}
}

func TestSynthesizeQA_SKIP_ReturnsNil(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(mockAnthropicResponse("SKIP"))
	}))
	defer server.Close()

	s := New("sk-test", server.URL)
	result, err := s.SynthesizeQA(context.Background(), "Let me check that file", "OK, looking at it now.", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected nil result for SKIP, got: %v", result)
	}
}

func TestSynthesizeQA_SKIP_WithWhitespace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(mockAnthropicResponse("  SKIP\n"))
	}))
	defer server.Close()

	s := New("sk-test", server.URL)
	result, err := s.SynthesizeQA(context.Background(), "q", "a", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("SKIP with whitespace should still return nil, got: %v", result)
	}
}

func TestSynthesizeQA_SKIP_WithExplanation(t *testing.T) {
	// Haiku sometimes returns "SKIP\n\nThis is procedural narration..." instead of bare "SKIP".
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(mockAnthropicResponse("SKIP\n\nThis is procedural narration describing the current state of an existing connector implementation."))
	}))
	defer server.Close()

	s := New("sk-test", server.URL)
	result, err := s.SynthesizeQA(context.Background(), "q", "I looked at the code and it seems fine.", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("SKIP with explanation should return nil, got: %v", result)
	}
}

func TestSynthesizeQA_SKIP_WithSpaceExplanation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(mockAnthropicResponse("SKIP The text is procedural narration."))
	}))
	defer server.Close()

	s := New("sk-test", server.URL)
	result, err := s.SynthesizeQA(context.Background(), "q", "a", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("SKIP with space-separated explanation should return nil, got: %v", result)
	}
}

func TestSynthesizeQA_NoPrefixLine_ReturnsNil(t *testing.T) {
	// When the model outputs garbage without FACT: or SKIP prefixes, return nil.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(mockAnthropicResponse("This text contains procedural narration about reading code."))
	}))
	defer server.Close()

	s := New("sk-test", server.URL)
	result, err := s.SynthesizeQA(context.Background(), "q", "Let me look at the files...", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("unprefixed output should return nil, got: %v", result)
	}
}

func TestSynthesizeQA_ReturnsFacts(t *testing.T) {
	const fact1 = "The proxy server in internal/proxy/proxy.go binds to 127.0.0.1:7432 and enriches requests via the read pipeline before forwarding to the upstream Anthropic API."
	const fact2 = "Config key proxy.port defaults to 7432 because it avoids conflicts with common services."
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(mockAnthropicResponse("FACT: " + fact1 + "\nFACT: " + fact2))
	}))
	defer server.Close()

	s := New("sk-test", server.URL)
	result, err := s.SynthesizeQA(context.Background(), "How does the proxy work?", "The proxy binds to 127.0.0.1:7432...", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 facts, got %d: %v", len(result), result)
	}
	if result[0] != fact1 {
		t.Errorf("fact[0] = %q, want %q", result[0], fact1)
	}
	if result[1] != fact2 {
		t.Errorf("fact[1] = %q, want %q", result[1], fact2)
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
	_, _ = s.SynthesizeQA(context.Background(), "Why does config use port 7432?", "Because that port is unlikely to conflict.", "")

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
	_, _ = s.SynthesizeQA(context.Background(), "", "The embedder uses voyage-4-nano at 1024 dimensions.", "")

	if !strings.Contains(capturedPrompt, "ASSISTANT OUTPUT:") {
		t.Error("empty question should use ASSISTANT OUTPUT: frame, not USER:/ASSISTANT:")
	}
	if strings.Contains(capturedPrompt, "USER:") {
		t.Error("empty question should not include USER: line")
	}
}

func TestSynthesizeQA_PromptHasUnifiedStructure(t *testing.T) {
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
	_, _ = s.SynthesizeQA(context.Background(), "q", "a", "")

	if !strings.Contains(capturedPrompt, "GATE") {
		t.Error("prompt should contain GATE section")
	}
	if !strings.Contains(capturedPrompt, "FORMAT") {
		t.Error("prompt should contain FORMAT section")
	}
	if !strings.Contains(capturedPrompt, "FACT: ") {
		t.Error("prompt should instruct FACT: output format")
	}
	if !strings.Contains(capturedPrompt, "root cause") {
		t.Error("prompt should reference root causes")
	}
}

func TestSynthesizeQA_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"type":"overloaded_error"}}`, http.StatusServiceUnavailable)
	}))
	defer server.Close()

	s := New("sk-test", server.URL)
	_, err := s.SynthesizeQA(context.Background(), "q", "a", "")
	if err == nil {
		t.Error("expected error on API failure")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error should contain status code, got: %v", err)
	}
}

func TestSynthesizeQA_TopicHintIncluded(t *testing.T) {
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
	_, _ = s.SynthesizeQA(context.Background(), "q", "a", "fix the billing service | update k8s manifests")

	if !strings.Contains(capturedPrompt, "SURROUNDING TOPIC CONTEXT") {
		t.Error("prompt should include SURROUNDING TOPIC CONTEXT when topicHint is provided")
	}
	if !strings.Contains(capturedPrompt, "fix the billing service | update k8s manifests") {
		t.Error("prompt should include the actual topic hint text")
	}
}

func TestSynthesizeQA_NoTopicHint(t *testing.T) {
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
	_, _ = s.SynthesizeQA(context.Background(), "q", "a", "")

	if strings.Contains(capturedPrompt, "SURROUNDING TOPIC CONTEXT (for anchoring") {
		t.Error("prompt should NOT include topic context block when topicHint is empty")
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

	if !strings.Contains(capturedPrompt, "Extract every distinct technical fact") {
		t.Error("Synthesize prompt should instruct extraction of atomic facts")
	}
	if !strings.Contains(capturedPrompt, "No narration") {
		t.Error("Synthesize prompt should prohibit narration")
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

	if !strings.Contains(capturedPrompt, "root causes") {
		t.Error("SynthesizeConversation prompt should focus on root causes and gotchas")
	}
	if !strings.Contains(capturedPrompt, "Ignore the task narrative") {
		t.Error("SynthesizeConversation prompt should ignore task narrative")
	}
}

// ---------------------------------------------------------------------------
// Azure OpenAI backend tests
// ---------------------------------------------------------------------------

func mockAzureResponse(text string) []byte {
	resp := map[string]any{
		"choices": []map[string]any{
			{"message": map[string]string{"role": "assistant", "content": text}},
		},
	}
	b, _ := json.Marshal(resp)
	return b
}

func TestAzure_Available(t *testing.T) {
	s := NewAzure(AzureConfig{
		Endpoint:   "https://myresource.openai.azure.com",
		Deployment: "gpt-4o-mini",
		APIVersion: "2024-06-01",
		APIKey:     "abc123",
	})
	if !s.Available() {
		t.Error("Azure synthesizer with all fields should be available")
	}
}

func TestAzure_NotAvailable_NoKey(t *testing.T) {
	s := NewAzure(AzureConfig{
		Endpoint:   "https://myresource.openai.azure.com",
		Deployment: "gpt-4o-mini",
		APIVersion: "2024-06-01",
	})
	if s.Available() {
		t.Error("Azure synthesizer without API key should not be available")
	}
}

func TestAzure_NotAvailable_NoEndpoint(t *testing.T) {
	s := NewAzure(AzureConfig{
		Deployment: "gpt-4o-mini",
		APIVersion: "2024-06-01",
		APIKey:     "abc123",
	})
	if s.Available() {
		t.Error("Azure synthesizer without endpoint should not be available")
	}
}

func TestAzure_SynthesizeQA(t *testing.T) {
	const fact1 = "The proxy server binds to 127.0.0.1:7432."
	var capturedPath string
	var capturedAPIKey string
	var capturedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedAPIKey = r.Header.Get("api-key")
		json.NewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write(mockAzureResponse("FACT: " + fact1))
	}))
	defer server.Close()

	s := NewAzure(AzureConfig{
		Endpoint:   server.URL,
		Deployment: "gpt-4o-mini",
		APIVersion: "2024-06-01",
		APIKey:     "test-key-123",
	})
	result, err := s.SynthesizeQA(context.Background(), "How does the proxy work?", "It binds to 127.0.0.1:7432.", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 || result[0] != fact1 {
		t.Errorf("unexpected result: %v", result)
	}
	if capturedPath != "/openai/v1/chat/completions" {
		t.Errorf("unexpected request path: %s", capturedPath)
	}
	if capturedAPIKey != "test-key-123" {
		t.Errorf("unexpected api-key header: %s", capturedAPIKey)
	}
	if model, _ := capturedBody["model"].(string); model != "gpt-4o-mini" {
		t.Errorf("expected model in request body, got: %q", model)
	}
}

func TestAzure_SKIP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(mockAzureResponse("SKIP"))
	}))
	defer server.Close()

	s := NewAzure(AzureConfig{
		Endpoint:   server.URL,
		Deployment: "gpt-4o-mini",
		APIVersion: "2024-06-01",
		APIKey:     "test-key",
	})
	result, err := s.SynthesizeQA(context.Background(), "q", "a", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected nil result for SKIP, got: %v", result)
	}
}

func TestAzure_Synthesize(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(mockAzureResponse("FACT: Combined knowledge about Go concurrency patterns."))
	}))
	defer server.Close()

	s := NewAzure(AzureConfig{
		Endpoint:   server.URL,
		Deployment: "gpt-4o-mini",
		APIVersion: "2024-06-01",
		APIKey:     "test-key",
	})
	result, err := s.Synthesize(context.Background(), []string{"chunk one", "chunk two"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 || !strings.Contains(result[0], "Go concurrency") {
		t.Errorf("unexpected result: %v", result)
	}
}
