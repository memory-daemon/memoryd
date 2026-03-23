package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestServer(handler http.Handler) (*Server, *httptest.Server) {
	ts := httptest.NewServer(handler)
	return &Server{baseURL: ts.URL, client: ts.Client()}, ts
}

func call(s *Server, method string, id any, params any) *jsonRPCResponse {
	var raw json.RawMessage
	if params != nil {
		raw, _ = json.Marshal(params)
	}
	req := jsonRPCRequest{JSONRPC: "2.0", ID: id, Method: method, Params: raw}
	data, _ := json.Marshal(req)
	return s.handleMessage(data)
}

func TestInitialize(t *testing.T) {
	s := &Server{baseURL: "http://unused", client: &http.Client{}}
	resp := call(s, "initialize", 1, map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "0.1"},
	})
	if resp.Error != nil {
		t.Fatalf("expected no error, got %v", resp.Error)
	}
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatal("result is not a map")
	}
	if result["protocolVersion"] != "2024-11-05" {
		t.Errorf("expected protocolVersion 2024-11-05, got %v", result["protocolVersion"])
	}
	info := result["serverInfo"].(map[string]any)
	if info["name"] != "memoryd" {
		t.Errorf("expected serverInfo.name memoryd, got %v", info["name"])
	}
}

func TestToolsList(t *testing.T) {
	s := &Server{baseURL: "http://unused", client: &http.Client{}}
	resp := call(s, "tools/list", 2, nil)
	if resp.Error != nil {
		t.Fatalf("expected no error, got %v", resp.Error)
	}
	result := resp.Result.(map[string]any)
	tools := result["tools"].([]map[string]any)
	if len(tools) != 10 {
		t.Fatalf("expected 10 tools, got %d", len(tools))
	}
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool["name"].(string)] = true
	}
	for _, want := range []string{"memory_search", "memory_store", "memory_list", "memory_delete", "source_ingest", "source_list", "source_remove", "quality_stats", "database_list"} {
		if !names[want] {
			t.Errorf("missing tool %s", want)
		}
	}
}

func TestPing(t *testing.T) {
	s := &Server{baseURL: "http://unused", client: &http.Client{}}
	resp := call(s, "ping", 3, nil)
	if resp.Error != nil {
		t.Fatalf("expected no error, got %v", resp.Error)
	}
}

func TestNotificationNoResponse(t *testing.T) {
	s := &Server{baseURL: "http://unused", client: &http.Client{}}
	resp := call(s, "notifications/initialized", nil, nil)
	if resp != nil {
		t.Errorf("expected nil response for notification, got %v", resp)
	}
}

func TestUnknownMethod(t *testing.T) {
	s := &Server{baseURL: "http://unused", client: &http.Client{}}
	resp := call(s, "foo/bar", 4, nil)
	if resp.Error == nil {
		t.Fatal("expected error for unknown method")
	}
	errMap := resp.Error.(map[string]any)
	if errMap["code"].(int) != -32601 {
		t.Errorf("expected -32601, got %v", errMap["code"])
	}
}

func TestParseError(t *testing.T) {
	s := &Server{baseURL: "http://unused", client: &http.Client{}}
	resp := s.handleMessage([]byte("not json"))
	if resp.Error == nil {
		t.Fatal("expected parse error")
	}
}

func TestMemorySearch(t *testing.T) {
	s, ts := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/search" || r.Method != "POST" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"context": "found some memories"})
	}))
	defer ts.Close()

	resp := call(s, "tools/call", 5, map[string]any{
		"name":      "memory_search",
		"arguments": map[string]any{"query": "test query"},
	})
	if resp.Error != nil {
		t.Fatalf("expected no error, got %v", resp.Error)
	}
	result := resp.Result.(map[string]any)
	content := result["content"].([]map[string]any)
	if content[0]["text"] != "found some memories" {
		t.Errorf("unexpected text: %v", content[0]["text"])
	}
	if result["isError"].(bool) {
		t.Error("expected isError=false")
	}
}

func TestMemorySearchMissingQuery(t *testing.T) {
	s := &Server{baseURL: "http://unused", client: &http.Client{}}
	resp := call(s, "tools/call", 6, map[string]any{
		"name":      "memory_search",
		"arguments": map[string]any{},
	})
	result := resp.Result.(map[string]any)
	if !result["isError"].(bool) {
		t.Error("expected isError=true for missing query")
	}
}

func TestMemoryStore(t *testing.T) {
	s, ts := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/store" || r.Method != "POST" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["source"] != "mcp" {
			t.Errorf("expected default source mcp, got %s", body["source"])
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer ts.Close()

	resp := call(s, "tools/call", 7, map[string]any{
		"name":      "memory_store",
		"arguments": map[string]any{"content": "important fact"},
	})
	result := resp.Result.(map[string]any)
	content := result["content"].([]map[string]any)
	if content[0]["text"] != "Memory stored successfully." {
		t.Errorf("unexpected text: %v", content[0]["text"])
	}
}

func TestMemoryStoreMissingContent(t *testing.T) {
	s := &Server{baseURL: "http://unused", client: &http.Client{}}
	resp := call(s, "tools/call", 8, map[string]any{
		"name":      "memory_store",
		"arguments": map[string]any{},
	})
	result := resp.Result.(map[string]any)
	if !result["isError"].(bool) {
		t.Error("expected isError=true for missing content")
	}
}

func TestMemoryList(t *testing.T) {
	s, ts := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/memories" || r.Method != "GET" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]map[string]any{
			{"id": "abc123", "content": "test memory"},
		})
	}))
	defer ts.Close()

	resp := call(s, "tools/call", 9, map[string]any{
		"name":      "memory_list",
		"arguments": map[string]any{},
	})
	result := resp.Result.(map[string]any)
	if result["isError"].(bool) {
		t.Error("expected isError=false")
	}
}

func TestMemoryDelete(t *testing.T) {
	s, ts := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/memories/abc123" || r.Method != "DELETE" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer ts.Close()

	resp := call(s, "tools/call", 10, map[string]any{
		"name":      "memory_delete",
		"arguments": map[string]any{"id": "abc123"},
	})
	result := resp.Result.(map[string]any)
	content := result["content"].([]map[string]any)
	if content[0]["text"] != "Memory deleted." {
		t.Errorf("unexpected text: %v", content[0]["text"])
	}
}

func TestMemoryDeleteMissingID(t *testing.T) {
	s := &Server{baseURL: "http://unused", client: &http.Client{}}
	resp := call(s, "tools/call", 11, map[string]any{
		"name":      "memory_delete",
		"arguments": map[string]any{},
	})
	result := resp.Result.(map[string]any)
	if !result["isError"].(bool) {
		t.Error("expected isError=true for missing id")
	}
}

func TestUnknownTool(t *testing.T) {
	s := &Server{baseURL: "http://unused", client: &http.Client{}}
	resp := call(s, "tools/call", 12, map[string]any{
		"name":      "nonexistent_tool",
		"arguments": map[string]any{},
	})
	result := resp.Result.(map[string]any)
	if !result["isError"].(bool) {
		t.Error("expected isError=true for unknown tool")
	}
}

func TestDaemonUnreachable(t *testing.T) {
	s := &Server{baseURL: "http://127.0.0.1:1", client: &http.Client{}}
	resp := call(s, "tools/call", 13, map[string]any{
		"name":      "memory_search",
		"arguments": map[string]any{"query": "test"},
	})
	result := resp.Result.(map[string]any)
	if !result["isError"].(bool) {
		t.Error("expected isError=true when daemon unreachable")
	}
	content := result["content"].([]map[string]any)
	text := content[0]["text"].(string)
	if len(text) == 0 {
		t.Error("expected error message")
	}
}

// --- Read-only mode tests ---

func TestToolsListReadOnly(t *testing.T) {
	s := &Server{baseURL: "http://unused", client: &http.Client{}, readOnly: true}
	resp := call(s, "tools/list", 20, nil)
	if resp.Error != nil {
		t.Fatalf("expected no error, got %v", resp.Error)
	}
	result := resp.Result.(map[string]any)
	tools := result["tools"].([]map[string]any)
	// Read-only should expose only non-write tools: memory_search, memory_list, source_list, quality_stats, database_list
	if len(tools) != 5 {
		names := make([]string, len(tools))
		for i, tool := range tools {
			names[i], _ = tool["name"].(string)
		}
		t.Fatalf("expected 5 tools in read-only mode, got %d: %v", len(tools), names)
	}
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool["name"].(string)] = true
	}
	for _, want := range []string{"memory_search", "memory_list", "source_list", "quality_stats", "database_list"} {
		if !names[want] {
			t.Errorf("missing read tool %s", want)
		}
	}
	for _, blocked := range []string{"memory_store", "memory_delete", "source_ingest", "source_remove"} {
		if names[blocked] {
			t.Errorf("write tool %s should not be listed in read-only mode", blocked)
		}
	}
}

func TestWriteToolBlockedInReadOnly(t *testing.T) {
	s := &Server{baseURL: "http://unused", client: &http.Client{}, readOnly: true}

	writeToolCalls := []struct {
		name string
		args map[string]any
	}{
		{"memory_store", map[string]any{"content": "test"}},
		{"memory_delete", map[string]any{"id": "abc"}},
		{"source_ingest", map[string]any{"url": "https://example.com", "name": "test"}},
		{"source_remove", map[string]any{"id": "abc"}},
	}

	for _, tc := range writeToolCalls {
		resp := call(s, "tools/call", 21, map[string]any{
			"name":      tc.name,
			"arguments": tc.args,
		})
		result := resp.Result.(map[string]any)
		if !result["isError"].(bool) {
			t.Errorf("%s: expected isError=true in read-only mode", tc.name)
		}
		content := result["content"].([]map[string]any)
		text := content[0]["text"].(string)
		if text == "" {
			t.Errorf("%s: expected error message", tc.name)
		}
	}
}

func TestReadToolsAllowedInReadOnly(t *testing.T) {
	s, ts := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/search":
			json.NewEncoder(w).Encode(map[string]string{"context": "found"})
		case "/api/memories":
			json.NewEncoder(w).Encode([]map[string]any{})
		case "/api/sources":
			json.NewEncoder(w).Encode([]map[string]any{})
		case "/api/quality":
			json.NewEncoder(w).Encode(map[string]any{"retrieval_count": 10})
		}
	}))
	defer ts.Close()

	// Enable read-only on the test server
	s.readOnly = true

	readToolCalls := []struct {
		name string
		args map[string]any
	}{
		{"memory_search", map[string]any{"query": "test"}},
		{"memory_list", map[string]any{}},
		{"source_list", map[string]any{}},
		{"quality_stats", map[string]any{}},
	}

	for _, tc := range readToolCalls {
		resp := call(s, "tools/call", 22, map[string]any{
			"name":      tc.name,
			"arguments": tc.args,
		})
		result := resp.Result.(map[string]any)
		if result["isError"].(bool) {
			content := result["content"].([]map[string]any)
			t.Errorf("%s: should succeed in read-only mode, got error: %v", tc.name, content[0]["text"])
		}
	}
}

func TestNewServerReadOnly(t *testing.T) {
	s := NewServer(7432, true)
	if !s.readOnly {
		t.Error("expected readOnly=true")
	}
	if s.baseURL != "http://127.0.0.1:7432" {
		t.Errorf("baseURL = %q", s.baseURL)
	}
}

func TestNewServerReadWrite(t *testing.T) {
	s := NewServer(7432, false)
	if s.readOnly {
		t.Error("expected readOnly=false")
	}
}
