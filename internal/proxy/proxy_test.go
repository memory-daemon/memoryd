package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/memory-daemon/memoryd/internal/config"
	"github.com/memory-daemon/memoryd/internal/pipeline"
	"github.com/memory-daemon/memoryd/internal/store"
	"github.com/memory-daemon/memoryd/internal/synthesizer"
)

func TestExtractResponseText_ValidResponse(t *testing.T) {
	resp := map[string]any{
		"content": []any{
			map[string]any{"type": "text", "text": "Hello "},
			map[string]any{"type": "text", "text": "world!"},
		},
	}
	body, _ := json.Marshal(resp)
	got := extractResponseText(body)
	want := "Hello \nworld!"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExtractResponseText_EmptyContent(t *testing.T) {
	resp := map[string]any{"content": []any{}}
	body, _ := json.Marshal(resp)
	got := extractResponseText(body)
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestExtractResponseText_InvalidJSON(t *testing.T) {
	got := extractResponseText([]byte("not json"))
	if got != "" {
		t.Errorf("got %q, want empty for invalid JSON", got)
	}
}

func TestExtractResponseText_NoContent(t *testing.T) {
	resp := map[string]any{"id": "msg123"}
	body, _ := json.Marshal(resp)
	got := extractResponseText(body)
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestExtractTextDelta_Valid(t *testing.T) {
	var buf strings.Builder
	data := `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello "}}`
	extractTextDelta(data, &buf)
	if buf.String() != "hello " {
		t.Errorf("buf = %q, want hello ", buf.String())
	}
}

func TestExtractTextDelta_NotTextDelta(t *testing.T) {
	var buf strings.Builder
	data := `{"type":"content_block_start","delta":{"type":"start"}}`
	extractTextDelta(data, &buf)
	if buf.String() != "" {
		t.Errorf("buf should be empty for non-text_delta, got %q", buf.String())
	}
}

func TestExtractTextDelta_InvalidJSON(t *testing.T) {
	var buf strings.Builder
	extractTextDelta("not json", &buf)
	if buf.String() != "" {
		t.Errorf("buf should be empty for invalid JSON, got %q", buf.String())
	}
}

func TestExtractTextDelta_NoDelta(t *testing.T) {
	var buf strings.Builder
	data := `{"type":"message_start"}`
	extractTextDelta(data, &buf)
	if buf.String() != "" {
		t.Errorf("buf should be empty when no delta field, got %q", buf.String())
	}
}

func TestExtractTextDelta_Accumulates(t *testing.T) {
	var buf strings.Builder
	extractTextDelta(`{"delta":{"type":"text_delta","text":"one "}}`, &buf)
	extractTextDelta(`{"delta":{"type":"text_delta","text":"two "}}`, &buf)
	extractTextDelta(`{"delta":{"type":"text_delta","text":"three"}}`, &buf)
	if buf.String() != "one two three" {
		t.Errorf("buf = %q, want one two three", buf.String())
	}
}

func TestCopyHeaders(t *testing.T) {
	src := http.Header{}
	src.Set("Content-Type", "application/json")
	src.Set("X-Custom", "value")

	dst := http.Header{}
	copyHeaders(dst, src)

	if dst.Get("Content-Type") != "application/json" {
		t.Error("Content-Type not copied")
	}
	if dst.Get("X-Custom") != "value" {
		t.Error("X-Custom not copied")
	}
}

func TestOpenAIHandler_Returns501(t *testing.T) {
	handler := newOpenAIHandler()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", w.Code)
	}
}

func TestHealthEndpoint(t *testing.T) {
	cfg := &config.Config{Port: 0}
	read := pipeline.NewReadPipeline(nil, nil, cfg)
	write := pipeline.NewWritePipeline(nil, nil)
	srv := NewServer(cfg, "test", read, write) //nolint — no synth needed for health test

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("health status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "ok") {
		t.Error("expected ok in health response")
	}
}

func TestAnthropicHandler_SyncRoundTrip(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" {
			t.Error("x-api-key not forwarded")
		}
		if r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Error("anthropic-version not forwarded")
		}

		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(body, &req)

		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"id":   "msg_test",
			"type": "message",
			"content": []any{
				map[string]any{"type": "text", "text": "Test response"},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer upstream.Close()

	handler := &anthropicHandler{
		upstreamURL: upstream.URL,
		write:       pipeline.NewWritePipeline(nil, nil),
		client:      upstream.Client(),
	}

	reqBody := `{"messages":[{"role":"user","content":"hello"}],"model":"claude-3-5-sonnet-20241022"}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("x-api-key", "test-key")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["id"] != "msg_test" {
		t.Errorf("response id = %v, want msg_test", resp["id"])
	}
}

func TestAnthropicHandler_InvalidJSON(t *testing.T) {
	handler := &anthropicHandler{
		upstreamURL: "http://unused",
		client:      &http.Client{},
	}

	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader("not json"))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestAnthropicHandler_StreamRoundTrip(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		events := []string{
			`data: {"type":"message_start","message":{"id":"msg_test"}}`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello "}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world!"}}`,
			`data: {"type":"content_block_stop","index":0}`,
			`data: {"type":"message_stop"}`,
		}
		for _, e := range events {
			fmt.Fprintf(w, "%s\n", e)
		}
	}))
	defer upstream.Close()

	handler := &anthropicHandler{
		upstreamURL: upstream.URL,
		write:       pipeline.NewWritePipeline(nil, nil),
		client:      upstream.Client(),
	}

	reqBody := `{"messages":[{"role":"user","content":"hi"}],"stream":true,"model":"claude-3-5-sonnet-20241022"}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("content-type", "application/json")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "Hello ") {
		t.Error("expected Hello in streamed response")
	}
	if !strings.Contains(body, "world!") {
		t.Error("expected world! in streamed response")
	}
}

// --- Q&A pairing helpers ---

func TestExtractLastUserMessage_String(t *testing.T) {
	raw := map[string]json.RawMessage{
		"messages": json.RawMessage(`[
			{"role":"user","content":"first question"},
			{"role":"assistant","content":"first answer"},
			{"role":"user","content":"second question"}
		]`),
	}
	got := extractLastUserMessage(raw)
	if got != "second question" {
		t.Errorf("got %q, want second question", got)
	}
}

func TestExtractLastUserMessage_BlockContent(t *testing.T) {
	raw := map[string]json.RawMessage{
		"messages": json.RawMessage(`[
			{"role":"user","content":[{"type":"text","text":"block question"}]}
		]`),
	}
	got := extractLastUserMessage(raw)
	if got != "block question" {
		t.Errorf("got %q, want block question", got)
	}
}

func TestExtractLastUserMessage_NoMessages(t *testing.T) {
	raw := map[string]json.RawMessage{}
	got := extractLastUserMessage(raw)
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestExtractLastUserMessage_OnlyAssistant(t *testing.T) {
	raw := map[string]json.RawMessage{
		"messages": json.RawMessage(`[{"role":"assistant","content":"hello"}]`),
	}
	got := extractLastUserMessage(raw)
	if got != "" {
		t.Errorf("expected empty when no user message, got %q", got)
	}
}

func TestExtractContentText_PlainString(t *testing.T) {
	raw := json.RawMessage(`"hello world"`)
	got := extractContentText(raw)
	if got != "hello world" {
		t.Errorf("got %q, want hello world", got)
	}
}

func TestExtractContentText_Blocks(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"part one"},{"type":"image","text":""},{"type":"text","text":"part two"}]`)
	got := extractContentText(raw)
	if got != "part one\npart two" {
		t.Errorf("got %q, want part one\\npart two", got)
	}
}

func TestExtractConversationTurns_MultiTurn(t *testing.T) {
	raw := map[string]json.RawMessage{
		"messages": json.RawMessage(`[
			{"role":"user","content":"question one"},
			{"role":"assistant","content":"answer one"},
			{"role":"user","content":"question two"}
		]`),
	}
	turns := extractConversationTurns(raw)
	if len(turns) != 3 {
		t.Fatalf("expected 3 turns, got %d", len(turns))
	}
	if turns[0].Role != "user" || turns[0].Content != "question one" {
		t.Errorf("unexpected turn 0: %+v", turns[0])
	}
	if turns[1].Role != "assistant" || turns[1].Content != "answer one" {
		t.Errorf("unexpected turn 1: %+v", turns[1])
	}
}

func TestExtractConversationTurns_Empty(t *testing.T) {
	raw := map[string]json.RawMessage{}
	turns := extractConversationTurns(raw)
	if len(turns) != 0 {
		t.Errorf("expected 0 turns, got %d", len(turns))
	}
}

func TestCountPairs(t *testing.T) {
	turns := []synthesizer.ConversationTurn{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "q2"},
		{Role: "assistant", Content: "a2"},
		{Role: "user", Content: "q3"},
	}
	// 3 user, 2 assistant → min = 2 pairs
	if got := countPairs(turns); got != 2 {
		t.Errorf("countPairs = %d, want 2", got)
	}
}

// --- Steward stats dashboard tests ---

type mockStewardStats struct {
	stats SweepStats
}

func (m *mockStewardStats) LastSweep() SweepStats { return m.stats }

func TestDashboardAPI_IncludesStewardStats(t *testing.T) {
	// Minimal mock store for the dashboard handler.
	handler := &dashboardHandler{
		store:   &minimalStore{},
		steward: &mockStewardStats{stats: SweepStats{Scored: 42, Pruned: 3, Merged: 1, Elapsed: 150 * time.Millisecond}},
	}

	req := httptest.NewRequest("GET", "/api/dashboard", nil)
	w := httptest.NewRecorder()
	handler.handleDashboard(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200", w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	stw, ok := resp["steward"].(map[string]any)
	if !ok {
		t.Fatal("expected steward key in response")
	}
	if stw["active"] != true {
		t.Error("expected steward active=true")
	}
	if stw["scored"].(float64) != 42 {
		t.Errorf("scored = %v, want 42", stw["scored"])
	}
	if stw["merged"].(float64) != 1 {
		t.Errorf("merged = %v, want 1", stw["merged"])
	}
}

func TestDashboardAPI_NilSteward(t *testing.T) {
	handler := &dashboardHandler{
		store:   &minimalStore{},
		steward: nil,
	}

	req := httptest.NewRequest("GET", "/api/dashboard", nil)
	w := httptest.NewRecorder()
	handler.handleDashboard(w, req)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)

	stw := resp["steward"].(map[string]any)
	if stw["active"] != false {
		t.Error("expected steward active=false when nil")
	}
}

// minimalStore implements store.Store for dashboard tests (all methods return empty/nil).
type minimalStore struct{}

func (m *minimalStore) VectorSearch(_ context.Context, _ []float32, _ int) ([]store.Memory, error) {
	return nil, nil
}
func (m *minimalStore) Insert(_ context.Context, _ store.Memory) error { return nil }
func (m *minimalStore) Delete(_ context.Context, _ string) error       { return nil }
func (m *minimalStore) List(_ context.Context, _ string, _ int) ([]store.Memory, error) {
	return nil, nil
}
func (m *minimalStore) DeleteAll(_ context.Context) error                        { return nil }
func (m *minimalStore) CountBySource(_ context.Context, _ string) (int64, error) { return 0, nil }
func (m *minimalStore) UpdateContent(_ context.Context, _ string, _ string, _ []float32) error {
	return nil
}
func (m *minimalStore) ListBySource(_ context.Context, _ string, _ int) ([]store.Memory, error) {
	return nil, nil
}
func (m *minimalStore) Close() error { return nil }

func TestAnthropicHandler_UpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "overloaded", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	handler := &anthropicHandler{
		upstreamURL: upstream.URL,
		write:       pipeline.NewWritePipeline(nil, nil),
		client:      upstream.Client(),
	}

	reqBody := `{"messages":[{"role":"user","content":"test"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("content-type", "application/json")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// --- /api/settings GET tests ---

func TestHandleSettings_GET_IncludesServerSection(t *testing.T) {
	cfg := &config.Config{
		Mode:                 "proxy",
		Port:                 7432,
		MongoDBAtlasURI:      "mongodb://localhost:27017/?directConnection=true",
		MongoDBDatabase:      "memoryd",
		ModelPath:            "/tmp/model.gguf",
		EmbeddingDim:         1024,
		RetrievalTopK:        5,
		RetrievalMaxTokens:   2048,
		UpstreamAnthropicURL: "https://api.anthropic.com",
		AtlasMode:            false,
		LLMSynthesis:         true,
		SynthesisProvider:    config.SynthesisAuto,
	}
	a := &apiHandler{cfg: cfg}

	req := httptest.NewRequest("GET", "/api/settings", nil)
	w := httptest.NewRecorder()
	a.handleSettings(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	server, ok := resp["server"].(map[string]any)
	if !ok {
		t.Fatalf("missing server section, got: %v", resp)
	}
	wantKeys := []string{"port", "mongodb_database", "model_path", "embedding_dim",
		"retrieval_top_k", "retrieval_max_tokens", "upstream_anthropic_url",
		"atlas_mode", "llm_synthesis", "synthesis_provider"}
	for _, k := range wantKeys {
		if _, ok := server[k]; !ok {
			t.Errorf("server section missing key %q", k)
		}
	}
	if got := server["synthesis_provider"]; got != "auto" {
		t.Errorf("synthesis_provider = %v, want auto", got)
	}

	mongo := resp["mongodb"].(map[string]any)
	if configured, _ := mongo["configured"].(bool); !configured {
		t.Errorf("mongodb.configured = false, want true (URI is set on cfg)")
	}
	if mongo["uri_masked"] == "" {
		t.Error("mongodb.uri_masked is empty for a configured URI")
	}
}

func TestHandleSettings_GET_MongoFallbackWhenKeychainEmpty(t *testing.T) {
	// Even with no keychain entry, a plain-text URI on cfg should report configured.
	cfg := &config.Config{
		Mode:            "proxy",
		MongoDBAtlasURI: "mongodb://example.local:27017",
	}
	a := &apiHandler{cfg: cfg}
	req := httptest.NewRequest("GET", "/api/settings", nil)
	w := httptest.NewRecorder()
	a.handleSettings(w, req)
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	mongo := resp["mongodb"].(map[string]any)
	if c, _ := mongo["configured"].(bool); !c {
		t.Error("expected mongodb.configured=true when only cfg.MongoDBAtlasURI is set")
	}
}

// --- /api/settings POST tests ---

func TestHandleSettings_POST_RejectsInvalidSynthesisProvider(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	cfg := &config.Config{Mode: "proxy"}
	a := &apiHandler{cfg: cfg}

	body := `{"server":{"synthesis_provider":"openai"}}`
	req := httptest.NewRequest("POST", "/api/settings", strings.NewReader(body))
	w := httptest.NewRecorder()
	a.handleSettings(w, req)

	if w.Code != 400 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "synthesis_provider") {
		t.Errorf("error should mention synthesis_provider; got %s", w.Body.String())
	}
}

func TestHandleSettings_POST_RejectsInvalidPort(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	cfg := &config.Config{Mode: "proxy"}
	a := &apiHandler{cfg: cfg}

	body := `{"server":{"port":99999}}`
	req := httptest.NewRequest("POST", "/api/settings", strings.NewReader(body))
	w := httptest.NewRecorder()
	a.handleSettings(w, req)

	if w.Code != 400 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "port") {
		t.Errorf("error should mention port; got %s", w.Body.String())
	}
}

func TestHandleSettings_POST_PersistsServerFields(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	cfg := &config.Config{Mode: "proxy", Port: 7432}
	a := &apiHandler{cfg: cfg}

	body := `{"server":{"port":8080,"retrieval_top_k":11,"synthesis_provider":"azure","llm_synthesis":true}}`
	req := httptest.NewRequest("POST", "/api/settings", strings.NewReader(body))
	w := httptest.NewRecorder()
	a.handleSettings(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	// In-memory cfg updated.
	if cfg.Port != 8080 {
		t.Errorf("in-memory port = %d, want 8080", cfg.Port)
	}
	if cfg.RetrievalTopK != 11 {
		t.Errorf("in-memory retrieval_top_k = %d, want 11", cfg.RetrievalTopK)
	}
	if cfg.SynthesisProvider != config.SynthesisAzure {
		t.Errorf("in-memory synthesis_provider = %q, want azure", cfg.SynthesisProvider)
	}
	if !cfg.LLMSynthesis {
		t.Errorf("in-memory llm_synthesis = false, want true")
	}

	// Persisted to disk.
	loaded, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Port != 8080 {
		t.Errorf("disk port = %d, want 8080", loaded.Port)
	}
	if loaded.SynthesisProvider != config.SynthesisAzure {
		t.Errorf("disk synthesis_provider = %q, want azure", loaded.SynthesisProvider)
	}

	// Response indicates need_restart.
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if nr, _ := resp["need_restart"].(bool); !nr {
		t.Errorf("need_restart should be true after server changes")
	}
}
