package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func portFromURL(t *testing.T, rawURL string) int {
	t.Helper()
	parts := strings.Split(rawURL, ":")
	portStr := parts[len(parts)-1]
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("failed to parse port from %q: %v", rawURL, err)
	}
	return port
}

func healthServer(payload map[string]any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(payload)
	}))
}

func TestGetHealth_Success(t *testing.T) {
	srv := healthServer(map[string]any{
		"status":    "ok",
		"mongodb":   "connected",
		"synthesis": true,
	})
	defer srv.Close()

	port := portFromURL(t, srv.URL)
	result := getHealth(port)
	if result == nil {
		t.Fatal("expected non-nil health response")
	}
	if result["status"] != "ok" {
		t.Errorf("status: got %v, want ok", result["status"])
	}
	if result["mongodb"] != "connected" {
		t.Errorf("mongodb: got %v, want connected", result["mongodb"])
	}
	if synth, _ := result["synthesis"].(bool); !synth {
		t.Errorf("synthesis: got %v, want true", result["synthesis"])
	}
}

func TestGetHealth_NotOK(t *testing.T) {
	srv := healthServer(map[string]any{"status": "degraded"})
	defer srv.Close()
	if result := getHealth(portFromURL(t, srv.URL)); result != nil {
		t.Errorf("expected nil for non-ok status, got %v", result)
	}
}

func TestGetHealth_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	if result := getHealth(portFromURL(t, srv.URL)); result != nil {
		t.Errorf("expected nil for 503, got %v", result)
	}
}

func TestGetHealth_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()
	if result := getHealth(portFromURL(t, srv.URL)); result != nil {
		t.Errorf("expected nil for bad JSON, got %v", result)
	}
}

func TestGetHealth_Unreachable(t *testing.T) {
	if result := getHealth(19999); result != nil {
		t.Errorf("expected nil for unreachable, got %v", result)
	}
}

func TestCheckHealth_True(t *testing.T) {
	srv := healthServer(map[string]any{"status": "ok"})
	defer srv.Close()
	if !checkHealth(portFromURL(t, srv.URL)) {
		t.Error("expected true for healthy daemon")
	}
}

func TestCheckHealth_False(t *testing.T) {
	if checkHealth(19999) {
		t.Error("expected false for unreachable port")
	}
}

func TestGetHealth_FieldVariations(t *testing.T) {
	tests := []struct {
		name    string
		payload map[string]any
		wantNil bool
	}{
		{"minimal ok", map[string]any{"status": "ok"}, false},
		{"synthesis false", map[string]any{"status": "ok", "synthesis": false}, false},
		{"mongodb connecting", map[string]any{"status": "ok", "mongodb": "connecting"}, false},
		{"missing status", map[string]any{"mongodb": "connected"}, true},
		{"empty response", map[string]any{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := healthServer(tt.payload)
			defer srv.Close()
			result := getHealth(portFromURL(t, srv.URL))
			if tt.wantNil && result != nil {
				t.Errorf("expected nil, got %v", result)
			}
			if !tt.wantNil && result == nil {
				t.Error("expected non-nil")
			}
		})
	}
}

func TestGetHealth_SynthesisFieldTypes(t *testing.T) {
	tests := []struct {
		name      string
		payload   string
		wantSynth bool
	}{
		{"true", `{"status":"ok","synthesis":true}`, true},
		{"false", `{"status":"ok","synthesis":false}`, false},
		{"absent", `{"status":"ok"}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(tt.payload))
			}))
			defer srv.Close()
			result := getHealth(portFromURL(t, srv.URL))
			if result == nil {
				t.Fatal("expected non-nil")
			}
			synth, _ := result["synthesis"].(bool)
			if synth != tt.wantSynth {
				t.Errorf("synthesis: got %v, want %v", synth, tt.wantSynth)
			}
		})
	}
}

func TestGetHealth_MongoDBStates(t *testing.T) {
	for _, state := range []string{"connected", "connecting", "disconnected"} {
		t.Run(state, func(t *testing.T) {
			srv := healthServer(map[string]any{"status": "ok", "mongodb": state})
			defer srv.Close()
			result := getHealth(portFromURL(t, srv.URL))
			if result == nil {
				t.Fatal("expected non-nil")
			}
			got, _ := result["mongodb"].(string)
			if got != state {
				t.Errorf("mongodb: got %q, want %q", got, state)
			}
		})
	}
}

func TestStartGrace_SetAndCheck(t *testing.T) {
	setStartGrace()
	if !inStartGrace() {
		t.Error("expected true immediately after setStartGrace()")
	}
}

func TestStartGrace_Expired(t *testing.T) {
	uiGraceMu.Lock()
	uiGrace = time.Now().Add(-1 * time.Second)
	uiGraceMu.Unlock()
	if inStartGrace() {
		t.Error("expected false after expiry")
	}
}

func TestStartGrace_ZeroValue(t *testing.T) {
	uiGraceMu.Lock()
	uiGrace = time.Time{}
	uiGraceMu.Unlock()
	if inStartGrace() {
		t.Error("expected false for zero time")
	}
}

func TestStartGrace_ConcurrentAccess(t *testing.T) {
	done := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func() {
			setStartGrace()
			inStartGrace()
			done <- struct{}{}
		}()
	}
	for i := 0; i < 10; i++ {
		<-done
	}
}

func TestStopGrace_SetAndCheck(t *testing.T) {
	setStopGrace()
	if !inUIGrace() {
		t.Error("expected inUIGrace() = true immediately after setStopGrace()")
	}
}

func TestUIGrace_StopOverridesStart(t *testing.T) {
	// setStopGrace has a shorter duration than setStartGrace; either should
	// activate the grace window.
	setStopGrace()
	if !inStartGrace() {
		t.Error("inStartGrace wraps inUIGrace, should return true")
	}
}

func writeLog(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "daemon.log")
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExtractCrashReason_Table(t *testing.T) {
	tests := []struct {
		name      string
		log       string
		wantSub   string
		wantEmpty bool
	}{
		{
			"simple error",
			"2024/01/01 12:00:00 starting\n2024/01/01 12:00:01 fatal: mongodb connection refused",
			"mongodb connection refused",
			false,
		},
		{
			"error in middle",
			"line1\nline2\n2024/01/01 12:00:00 error binding port 7432: address already in use\nline4",
			"binding port 7432",
			false,
		},
		{
			"failed keyword",
			"info\n2024/06/15 09:00:00 failed to load embedding model",
			"failed to load embedding model",
			false,
		},
		{
			"no error lines",
			"2024/01/01 12:00:00 starting up\n2024/01/01 12:00:01 listening on :7432\n",
			"",
			true,
		},
		{
			"empty log",
			"",
			"",
			true,
		},
		{
			"long error truncated",
			"2024/01/01 12:00:00 error " + strings.Repeat("x", 200),
			"...",
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractCrashReason(writeLog(t, tt.log))
			if tt.wantEmpty && got != "" {
				t.Errorf("expected empty, got %q", got)
			}
			if !tt.wantEmpty && !strings.Contains(got, tt.wantSub) {
				t.Errorf("expected %q in result, got %q", tt.wantSub, got)
			}
		})
	}
}

func TestExtractCrashReason_MissingFile(t *testing.T) {
	if got := extractCrashReason("/no/such/file"); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestExtractCrashReason_FindsLastError(t *testing.T) {
	log := "2024/01/01 12:00:00 error first\n" +
		"2024/01/01 12:00:01 info ok\n" +
		"2024/01/01 12:00:02 error second\n"
	got := extractCrashReason(writeLog(t, log))
	if !strings.Contains(got, "second") {
		t.Errorf("expected last error, got %q", got)
	}
}

func TestExtractCrashReason_CaseInsensitive(t *testing.T) {
	got := extractCrashReason(writeLog(t, "2024/01/01 12:00:00 FATAL: boom\n"))
	if got == "" {
		t.Error("expected match on FATAL (uppercase)")
	}
}

func TestExtractCrashReason_OnlyLast20Lines(t *testing.T) {
	var lines []string
	lines = append(lines, "2024/01/01 12:00:00 error old")
	for i := 0; i < 25; i++ {
		lines = append(lines, fmt.Sprintf("2024/01/01 12:00:%02d info %d", i+1, i))
	}
	got := extractCrashReason(writeLog(t, strings.Join(lines, "\n")))
	if got != "" {
		t.Errorf("expected empty (error beyond 20-line window), got %q", got)
	}
}

func TestExtractCrashReason_TimestampStripped(t *testing.T) {
	got := extractCrashReason(writeLog(t, "2024/06/15 09:30:45 error: connection refused"))
	if strings.HasPrefix(got, "2024") {
		t.Errorf("timestamp should be stripped, got %q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("message lost, got %q", got)
	}
}

func TestExtractCrashReason_SingleLine(t *testing.T) {
	got := extractCrashReason(writeLog(t, "error: boom"))
	if got == "" {
		t.Error("expected non-empty for single error line")
	}
}

func TestCleanMCPConfig_RemovesMemoryd(t *testing.T) {
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"memoryd":      map[string]any{"command": "memoryd", "args": []string{"mcp"}},
			"other-server": map[string]any{"command": "other"},
		},
	}
	tmp := writeMCPConfig(t, cfg)

	if !cleanMCPConfig(tmp) {
		t.Fatal("expected true")
	}

	result := readMCPConfig(t, tmp)
	servers := result["mcpServers"].(map[string]any)
	if _, ok := servers["memoryd"]; ok {
		t.Error("memoryd should be removed")
	}
	if _, ok := servers["other-server"]; !ok {
		t.Error("other-server should remain")
	}
}

func TestCleanMCPConfig_NoMemoryd(t *testing.T) {
	cfg := map[string]any{
		"mcpServers": map[string]any{"foo": map[string]any{"command": "foo"}},
	}
	if cleanMCPConfig(writeMCPConfig(t, cfg)) {
		t.Error("expected false")
	}
}

func TestCleanMCPConfig_MissingFile(t *testing.T) {
	if cleanMCPConfig("/no/such/file.json") {
		t.Error("expected false")
	}
}

func TestCleanMCPConfig_InvalidJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(p, []byte("{{bad"), 0600)
	if cleanMCPConfig(p) {
		t.Error("expected false")
	}
}

func TestCleanMCPConfig_NoServersKey(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	data, _ := json.Marshal(map[string]any{"other": 1})
	os.WriteFile(p, data, 0600)
	if cleanMCPConfig(p) {
		t.Error("expected false")
	}
}

func TestCleanMCPConfig_ServersNotObject(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	data, _ := json.Marshal(map[string]any{"mcpServers": "string"})
	os.WriteFile(p, data, 0600)
	if cleanMCPConfig(p) {
		t.Error("expected false")
	}
}

func TestCleanMCPConfig_PreservesOtherKeys(t *testing.T) {
	cfg := map[string]any{
		"mcpServers":     map[string]any{"memoryd": map[string]any{"command": "m"}},
		"globalShortcut": "Ctrl+M",
	}
	tmp := writeMCPConfig(t, cfg)
	cleanMCPConfig(tmp)

	result := readMCPConfig(t, tmp)
	if result["globalShortcut"] != "Ctrl+M" {
		t.Error("non-mcpServers key lost")
	}
}

func TestCleanMCPConfig_FilePermissions(t *testing.T) {
	cfg := map[string]any{
		"mcpServers": map[string]any{"memoryd": map[string]any{"command": "m"}},
	}
	tmp := writeMCPConfig(t, cfg)
	cleanMCPConfig(tmp)

	info, err := os.Stat(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("expected 0600, got %04o", perm)
	}
}

func TestCleanMCPConfig_OnlyExactKey(t *testing.T) {
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"memoryd":        map[string]any{"command": "m"},
			"memoryd-custom": map[string]any{"command": "c"},
			"not-memoryd":    map[string]any{"command": "n"},
		},
	}
	tmp := writeMCPConfig(t, cfg)
	cleanMCPConfig(tmp)

	servers := readMCPConfig(t, tmp)["mcpServers"].(map[string]any)
	if _, ok := servers["memoryd"]; ok {
		t.Error("memoryd should be gone")
	}
	if _, ok := servers["memoryd-custom"]; !ok {
		t.Error("memoryd-custom should remain")
	}
	if _, ok := servers["not-memoryd"]; !ok {
		t.Error("not-memoryd should remain")
	}
}

func TestFindBinary_ReturnsNonEmpty(t *testing.T) {
	if findBinary() == "" {
		t.Error("findBinary should return a non-empty path")
	}
}

func writeMCPConfig(t *testing.T, cfg map[string]any) string {
	t.Helper()
	data, _ := json.MarshalIndent(cfg, "", "  ")
	p := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func readMCPConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	return result
}
