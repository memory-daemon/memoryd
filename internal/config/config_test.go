package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home directory")
	}

	tests := []struct {
		input string
		want  string
	}{
		{"~/.memoryd/models/foo.gguf", filepath.Join(home, ".memoryd/models/foo.gguf")},
		{"/absolute/path", "/absolute/path"},
		{"relative/path", "relative/path"},
		{"", ""},
	}

	for _, tt := range tests {
		got := expandHome(tt.input)
		if got != tt.want {
			t.Errorf("expandHome(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestDefault_HasSaneValues(t *testing.T) {
	if Default.Port != 7432 {
		t.Errorf("default port = %d, want 7432", Default.Port)
	}
	if Default.EmbeddingDim != 1024 {
		t.Errorf("default embedding dim = %d, want 1024", Default.EmbeddingDim)
	}
	if Default.RetrievalTopK != 5 {
		t.Errorf("default retrieval top-k = %d, want 5", Default.RetrievalTopK)
	}
	if Default.MongoDBDatabase != "memoryd" {
		t.Errorf("default db = %q, want memoryd", Default.MongoDBDatabase)
	}
	if Default.UpstreamAnthropicURL != "https://api.anthropic.com" {
		t.Errorf("default upstream URL = %q", Default.UpstreamAnthropicURL)
	}
}

func TestLoad_MissingFile_ReturnsDefaults(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Port != Default.Port {
		t.Errorf("port = %d, want default %d", cfg.Port, Default.Port)
	}
	if cfg.MongoDBDatabase != Default.MongoDBDatabase {
		t.Errorf("database = %q, want default %q", cfg.MongoDBDatabase, Default.MongoDBDatabase)
	}
}

func TestLoad_CustomConfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	dir := filepath.Join(tmp, ".memoryd")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}

	content := `port: 9999
mongodb_atlas_uri: "mongodb+srv://test:pass@cluster0.example.com"
mongodb_database: testdb
embedding_dim: 256
retrieval_top_k: 10
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Port != 9999 {
		t.Errorf("port = %d, want 9999", cfg.Port)
	}
	if cfg.MongoDBAtlasURI != "mongodb+srv://test:pass@cluster0.example.com" {
		t.Errorf("atlas_uri = %q", cfg.MongoDBAtlasURI)
	}
	if cfg.MongoDBDatabase != "testdb" {
		t.Errorf("database = %q, want testdb", cfg.MongoDBDatabase)
	}
	if cfg.EmbeddingDim != 256 {
		t.Errorf("embedding_dim = %d, want 256", cfg.EmbeddingDim)
	}
	if cfg.RetrievalTopK != 10 {
		t.Errorf("retrieval_top_k = %d, want 10", cfg.RetrievalTopK)
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	dir := filepath.Join(tmp, ".memoryd")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("{{invalid yaml"), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := Load()
	if err == nil {
		t.Error("expected error for invalid YAML, got nil")
	}
}

func TestWriteDefault_CreatesFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	if err := WriteDefault(); err != nil {
		t.Fatalf("WriteDefault() error: %v", err)
	}

	path := filepath.Join(tmp, ".memoryd", "config.yaml")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("config file not created: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("config file permissions = %o, want 0600", info.Mode().Perm())
	}
}

func TestWriteDefault_NoOverwrite(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	dir := filepath.Join(tmp, ".memoryd")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}

	existing := "port: 1234\n"
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(existing), 0600); err != nil {
		t.Fatal(err)
	}

	if err := WriteDefault(); err != nil {
		t.Fatalf("WriteDefault() error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != existing {
		t.Error("WriteDefault overwrote existing config file")
	}
}

func TestEnsureDir_CreatesDirectory(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	if err := EnsureDir(); err != nil {
		t.Fatalf("EnsureDir() error: %v", err)
	}

	dir := filepath.Join(tmp, ".memoryd")
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("directory not created: %v", err)
	}
	if !info.IsDir() {
		t.Error("expected a directory")
	}
}

func TestLoad_ExpandsHomePath(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	dir := filepath.Join(tmp, ".memoryd")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}

	content := `model_path: "~/.memoryd/models/test.gguf"
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	expected := filepath.Join(tmp, ".memoryd/models/test.gguf")
	if cfg.ModelPath != expected {
		t.Errorf("model_path = %q, want %q", cfg.ModelPath, expected)
	}
}

// --- Mode tests ---

func TestDefaultModeIsProxy(t *testing.T) {
	if Default.Mode != ModeProxy {
		t.Errorf("default mode = %q, want %q", Default.Mode, ModeProxy)
	}
}

func TestValidMode(t *testing.T) {
	valid := []string{ModeProxy, ModeMCP, ModeMCPReadOnly}
	for _, m := range valid {
		if !ValidMode(m) {
			t.Errorf("ValidMode(%q) = false, want true", m)
		}
	}

	invalid := []string{"", "invalid", "read-only", "PROXY", "MCP"}
	for _, m := range invalid {
		if ValidMode(m) {
			t.Errorf("ValidMode(%q) = true, want false", m)
		}
	}
}

func TestProxyWriteEnabled(t *testing.T) {
	tests := []struct {
		mode string
		want bool
	}{
		{"", true}, // empty defaults to proxy-like behaviour
		{ModeProxy, true},
		{ModeMCP, false},
		{ModeMCPReadOnly, false},
	}
	for _, tt := range tests {
		cfg := &Config{Mode: tt.mode}
		if got := cfg.ProxyWriteEnabled(); got != tt.want {
			t.Errorf("ProxyWriteEnabled() with mode %q = %v, want %v", tt.mode, got, tt.want)
		}
	}
}

func TestMCPReadOnly(t *testing.T) {
	tests := []struct {
		mode string
		want bool
	}{
		{"", false},
		{ModeProxy, false},
		{ModeMCP, false},
		{ModeMCPReadOnly, true},
	}
	for _, tt := range tests {
		cfg := &Config{Mode: tt.mode}
		if got := cfg.MCPReadOnly(); got != tt.want {
			t.Errorf("MCPReadOnly() with mode %q = %v, want %v", tt.mode, got, tt.want)
		}
	}
}

func TestSetMode(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	// SetMode should work even without an existing config file.
	if err := SetMode(ModeMCP); err != nil {
		t.Fatalf("SetMode(%q) error: %v", ModeMCP, err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Mode != ModeMCP {
		t.Errorf("mode = %q, want %q", cfg.Mode, ModeMCP)
	}

	// Switch to read-only, preserving other fields.
	if err := SetMode(ModeMCPReadOnly); err != nil {
		t.Fatalf("SetMode(%q) error: %v", ModeMCPReadOnly, err)
	}
	cfg, _ = Load()
	if cfg.Mode != ModeMCPReadOnly {
		t.Errorf("mode = %q, want %q", cfg.Mode, ModeMCPReadOnly)
	}
}

func TestSetMode_InvalidMode(t *testing.T) {
	if err := SetMode("bogus"); err == nil {
		t.Error("expected error for invalid mode, got nil")
	}
}

func TestSetMode_PreservesOtherFields(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	dir := filepath.Join(tmp, ".memoryd")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}

	content := `port: 8888
mongodb_atlas_uri: "mongodb://custom:27017"
mode: proxy
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	if err := SetMode(ModeMCPReadOnly); err != nil {
		t.Fatalf("SetMode error: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg.Port != 8888 {
		t.Errorf("port = %d, want 8888 (SetMode should preserve other fields)", cfg.Port)
	}
	if cfg.MongoDBAtlasURI != "mongodb://custom:27017" {
		t.Errorf("uri = %q (SetMode should preserve other fields)", cfg.MongoDBAtlasURI)
	}
	if cfg.Mode != ModeMCPReadOnly {
		t.Errorf("mode = %q, want %q", cfg.Mode, ModeMCPReadOnly)
	}
}

func TestLoad_ModeField(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	dir := filepath.Join(tmp, ".memoryd")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}

	content := `mode: mcp-readonly
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg.Mode != ModeMCPReadOnly {
		t.Errorf("mode = %q, want %q", cfg.Mode, ModeMCPReadOnly)
	}
}

// --- SaveServerConfig tests ---

func ptrInt(v int) *int             { return &v }
func ptrStr(v string) *string       { return &v }
func ptrBool(v bool) *bool          { return &v }

func TestSaveServerConfig_PartialPatchPreservesOtherFields(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	if err := WriteDefault(); err != nil {
		t.Fatalf("WriteDefault: %v", err)
	}

	// Patch only Port. Other fields should remain at defaults.
	if err := SaveServerConfig(ServerSettings{Port: ptrInt(9999)}); err != nil {
		t.Fatalf("SaveServerConfig: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 9999 {
		t.Errorf("port = %d, want 9999", cfg.Port)
	}
	if cfg.RetrievalTopK != Default.RetrievalTopK {
		t.Errorf("retrieval_top_k changed unexpectedly: got %d want %d", cfg.RetrievalTopK, Default.RetrievalTopK)
	}
	if cfg.UpstreamAnthropicURL != Default.UpstreamAnthropicURL {
		t.Errorf("upstream_anthropic_url changed unexpectedly: %q", cfg.UpstreamAnthropicURL)
	}
}

func TestSaveServerConfig_AllFields(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	if err := WriteDefault(); err != nil {
		t.Fatalf("WriteDefault: %v", err)
	}

	patch := ServerSettings{
		Port:                 ptrInt(8080),
		MongoDBDatabase:      ptrStr("custom_db"),
		ModelPath:            ptrStr("/tmp/model.gguf"),
		EmbeddingDim:         ptrInt(768),
		RetrievalTopK:        ptrInt(7),
		RetrievalMaxTokens:   ptrInt(4096),
		UpstreamAnthropicURL: ptrStr("https://upstream.example"),
		AtlasMode:            ptrBool(true),
		LLMSynthesis:         ptrBool(false),
		SynthesisProvider:    ptrStr(SynthesisAzure),
	}
	if err := SaveServerConfig(patch); err != nil {
		t.Fatalf("SaveServerConfig: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 8080 {
		t.Errorf("port = %d", cfg.Port)
	}
	if cfg.MongoDBDatabase != "custom_db" {
		t.Errorf("mongodb_database = %q", cfg.MongoDBDatabase)
	}
	if cfg.EmbeddingDim != 768 {
		t.Errorf("embedding_dim = %d", cfg.EmbeddingDim)
	}
	if cfg.RetrievalTopK != 7 {
		t.Errorf("retrieval_top_k = %d", cfg.RetrievalTopK)
	}
	if cfg.RetrievalMaxTokens != 4096 {
		t.Errorf("retrieval_max_tokens = %d", cfg.RetrievalMaxTokens)
	}
	if cfg.UpstreamAnthropicURL != "https://upstream.example" {
		t.Errorf("upstream_anthropic_url = %q", cfg.UpstreamAnthropicURL)
	}
	if !cfg.AtlasMode {
		t.Errorf("atlas_mode = false, want true")
	}
	if cfg.LLMSynthesis {
		t.Errorf("llm_synthesis = true, want false")
	}
	if cfg.SynthesisProvider != SynthesisAzure {
		t.Errorf("synthesis_provider = %q, want %q", cfg.SynthesisProvider, SynthesisAzure)
	}
}

func TestSaveServerConfig_PreservesOtherSections(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	dir := filepath.Join(tmp, ".memoryd")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}

	// Hand-craft yaml with steward + azure fields populated.
	yaml := `port: 7432
mode: proxy
azure:
    endpoint: "https://az.example"
    deployment: "gpt-4o-mini"
    api_version: "2024-06-01"
steward:
    interval_minutes: 30
    prune_threshold: 0.2
    grace_period_hours: 12
    decay_half_days: 60
    merge_threshold: 0.9
    batch_size: 100
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}

	if err := SaveServerConfig(ServerSettings{Port: ptrInt(8888)}); err != nil {
		t.Fatalf("SaveServerConfig: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 8888 {
		t.Errorf("port = %d", cfg.Port)
	}
	if cfg.Azure.Endpoint != "https://az.example" {
		t.Errorf("azure endpoint lost: %q", cfg.Azure.Endpoint)
	}
	if cfg.Azure.Deployment != "gpt-4o-mini" {
		t.Errorf("azure deployment lost: %q", cfg.Azure.Deployment)
	}
	if cfg.Steward.IntervalMinutes != 30 {
		t.Errorf("steward interval_minutes lost: %d", cfg.Steward.IntervalMinutes)
	}
	if cfg.Steward.MergeThreshold != 0.9 {
		t.Errorf("steward merge_threshold lost: %v", cfg.Steward.MergeThreshold)
	}
}

// --- ValidSynthesisProvider tests ---

func TestValidSynthesisProvider(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"", true},
		{SynthesisAuto, true},
		{SynthesisAnthropic, true},
		{SynthesisAzure, true},
		{"openai", false},
		{"AUTO", false}, // case-sensitive
		{"  ", false},
		{"anthropic ", false},
	}
	for _, tt := range tests {
		if got := ValidSynthesisProvider(tt.input); got != tt.want {
			t.Errorf("ValidSynthesisProvider(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestSynthesisProviderConstants(t *testing.T) {
	// Guard against accidental string-value changes (yaml/JSON wire compat).
	if SynthesisAuto != "auto" {
		t.Errorf("SynthesisAuto = %q, want \"auto\"", SynthesisAuto)
	}
	if SynthesisAnthropic != "anthropic" {
		t.Errorf("SynthesisAnthropic = %q, want \"anthropic\"", SynthesisAnthropic)
	}
	if SynthesisAzure != "azure" {
		t.Errorf("SynthesisAzure = %q, want \"azure\"", SynthesisAzure)
	}
}
