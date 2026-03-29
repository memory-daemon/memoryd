package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/memory-daemon/memoryd/internal/credential"
	"gopkg.in/yaml.v3"
)

// Mode constants control how the daemon integrates with agents.
const (
	ModeProxy       = "proxy"        // Proxy intercepts API calls — auto read + write
	ModeMCP         = "mcp"          // MCP tools only — explicit read + write
	ModeMCPReadOnly = "mcp-readonly" // MCP tools, search only — no writes
)

// PipelineConfig tunes the write pipeline quality filters.
// All values are live-reloadable via the dashboard — changes take effect
// immediately without restarting the daemon. Prototype changes trigger a
// background scorer reload.
type PipelineConfig struct {
	// Noise filter
	NoiseMinLen        int     `yaml:"noise_min_len"         json:"noise_min_len"`
	NoiseMinAlnumRatio float64 `yaml:"noise_min_alnum_ratio" json:"noise_min_alnum_ratio"`

	// Content score gate — chunks scoring below this threshold are dropped
	// before storage. Set to 0 to disable (default).
	ContentScoreGate float64 `yaml:"content_score_gate" json:"content_score_gate"`

	// Deduplication
	DedupThreshold           float64 `yaml:"dedup_threshold"            json:"dedup_threshold"`
	SourceExtensionThreshold float64 `yaml:"source_extension_threshold" json:"source_extension_threshold"`

	// Topic grouping
	TopicBoundaryThreshold float64 `yaml:"topic_boundary_threshold" json:"topic_boundary_threshold"`
	MaxGroupChars          int     `yaml:"max_group_chars"          json:"max_group_chars"`

	// Content scorer prototypes — empty means use built-in defaults.
	QualityProtos []string `yaml:"quality_protos,omitempty" json:"quality_protos,omitempty"`
	NoiseProtos   []string `yaml:"noise_protos,omitempty"   json:"noise_protos,omitempty"`
}

// Config holds all daemon configuration.
// Database role constants.
const (
	RoleFull     = "full"      // Read + write
	RoleReadOnly = "read-only" // Search only, no writes
)

// DatabaseConfig describes a single team database connection.
type DatabaseConfig struct {
	Name     string `yaml:"name"`              // Human label (e.g., "platform", "payments")
	Database string `yaml:"database"`          // MongoDB database name
	Role     string `yaml:"role"`              // "full" or "read-only"
	URI      string `yaml:"uri,omitempty"`     // Connection string (empty = use primary URI)
	Enabled  *bool  `yaml:"enabled,omitempty"` // nil/true = enabled, false = disabled
}

// IsEnabled returns true if the database is active (nil defaults to true).
func (dc DatabaseConfig) IsEnabled() bool {
	return dc.Enabled == nil || *dc.Enabled
}

type Config struct {
	Port                 int              `yaml:"port"`
	Mode                 string           `yaml:"mode"`
	APIToken             string           `yaml:"-"` // loaded from ~/.memoryd/token at startup, not stored in YAML
	MongoDBAtlasURI      string           `yaml:"mongodb_atlas_uri"`
	MongoDBDatabase      string           `yaml:"mongodb_database"`
	Databases            []DatabaseConfig `yaml:"databases,omitempty"`
	ModelPath            string           `yaml:"model_path"`
	EmbeddingDim         int              `yaml:"embedding_dim"`
	RetrievalTopK        int              `yaml:"retrieval_top_k"`
	RetrievalMaxTokens   int              `yaml:"retrieval_max_tokens"`
	UpstreamAnthropicURL string           `yaml:"upstream_anthropic_url"`
	AtlasMode            bool             `yaml:"atlas_mode"`
	LLMSynthesis         bool             `yaml:"llm_synthesis"`
	Steward              StewardConfig    `yaml:"steward"`
	Pipeline             PipelineConfig   `yaml:"pipeline"`
}

// ResolvedDatabases returns the effective list of databases.
// If Databases is empty, falls back to a single entry from MongoDBDatabase.
func (c *Config) ResolvedDatabases() []DatabaseConfig {
	if len(c.Databases) > 0 {
		return c.Databases
	}
	name := c.MongoDBDatabase
	if name == "" {
		name = "memoryd"
	}
	return []DatabaseConfig{{Name: name, Database: name, Role: RoleFull}}
}

// DefaultDatabase returns the first full-role database name (the write target
// for proxy capture and default MCP operations).
func (c *Config) DefaultDatabase() string {
	for _, db := range c.ResolvedDatabases() {
		if db.Role == "" || db.Role == RoleFull {
			return db.Database
		}
	}
	// Fallback: first database regardless of role.
	dbs := c.ResolvedDatabases()
	if len(dbs) > 0 {
		return dbs[0].Database
	}
	return "memoryd"
}

// StewardConfig tunes the background memory consolidation behaviour.
type StewardConfig struct {
	IntervalMinutes  int     `yaml:"interval_minutes"   json:"interval_minutes"`
	PruneThreshold   float64 `yaml:"prune_threshold"    json:"prune_threshold"`
	GracePeriodHours int     `yaml:"grace_period_hours" json:"grace_period_hours"`
	DecayHalfDays    int     `yaml:"decay_half_days"    json:"decay_half_days"`
	MergeThreshold   float64 `yaml:"merge_threshold"    json:"merge_threshold"`
	BatchSize        int     `yaml:"batch_size"         json:"batch_size"`
}

var Default = Config{
	Port:                 7432,
	Mode:                 ModeProxy,
	MongoDBDatabase:      "memoryd",
	ModelPath:            "~/.memoryd/models/voyage-4-nano.gguf",
	EmbeddingDim:         1024,
	RetrievalTopK:        5,
	RetrievalMaxTokens:   2048,
	UpstreamAnthropicURL: "https://api.anthropic.com",
	Steward: StewardConfig{
		IntervalMinutes:  60,
		PruneThreshold:   0.1,
		GracePeriodHours: 24,
		DecayHalfDays:    90,
		MergeThreshold:   0.88,
		BatchSize:        500,
	},
	Pipeline: PipelineConfig{
		NoiseMinLen:              40, // eval-tuned: 40 chars captures short-but-real agent responses without admitting trivial noise
		NoiseMinAlnumRatio:       0.40,
		ContentScoreGate:         0.0,
		DedupThreshold:           0.92,
		SourceExtensionThreshold: 0.75,
		TopicBoundaryThreshold:   0.65,
		MaxGroupChars:            2048,
	},
}

// Dir returns the memoryd config directory path.
func Dir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".memoryd")
}

// Path returns the config file path.
func Path() string {
	return filepath.Join(Dir(), "config.yaml")
}

// Load reads the config file, falling back to defaults for missing fields.
func Load() (*Config, error) {
	cfg := Default

	data, err := os.ReadFile(Path())
	if err != nil {
		if os.IsNotExist(err) {
			return &cfg, nil
		}
		return nil, fmt.Errorf("reading config: %w", err)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	cfg.ModelPath = expandHome(cfg.ModelPath)

	// Resolve credentials from OS keychain.
	cfg.MongoDBAtlasURI = resolveCredential(cfg.MongoDBAtlasURI, "mongodb_atlas_uri")
	for i := range cfg.Databases {
		if cfg.Databases[i].URI != "" {
			cfg.Databases[i].URI = resolveCredential(cfg.Databases[i].URI, "db_uri_"+cfg.Databases[i].Name)
		}
	}

	return &cfg, nil
}

// EnsureDir creates the config directory if it doesn't exist.
func EnsureDir() error {
	return os.MkdirAll(Dir(), 0700)
}

// WriteDefault writes a default config file if one doesn't already exist.
func WriteDefault() error {
	if err := EnsureDir(); err != nil {
		return err
	}

	path := Path()
	if _, err := os.Stat(path); err == nil {
		return nil
	}

	data, err := yaml.Marshal(Default)
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0600)
}

// ToStewardDuration helpers — convert the yaml-friendly int fields to time.Duration.
func (sc StewardConfig) Interval() time.Duration {
	return time.Duration(sc.IntervalMinutes) * time.Minute
}
func (sc StewardConfig) GracePeriod() time.Duration {
	return time.Duration(sc.GracePeriodHours) * time.Hour
}
func (sc StewardConfig) DecayHalfLife() time.Duration {
	return time.Duration(sc.DecayHalfDays) * 24 * time.Hour
}

// ProxyWriteEnabled returns true when the proxy should capture conversations.
func (c *Config) ProxyWriteEnabled() bool {
	return c.Mode == "" || c.Mode == ModeProxy
}

// MCPReadOnly returns true when MCP tools should be limited to reads.
func (c *Config) MCPReadOnly() bool {
	return c.Mode == ModeMCPReadOnly
}

// ValidMode returns true if the mode string is recognized.
func ValidMode(mode string) bool {
	switch mode {
	case ModeProxy, ModeMCP, ModeMCPReadOnly:
		return true
	}
	return false
}

// SetMode updates the mode in the config file on disk.
func SetMode(mode string) error {
	if !ValidMode(mode) {
		return fmt.Errorf("invalid mode %q: must be proxy, mcp, or mcp-readonly", mode)
	}

	cfg := Default
	data, err := os.ReadFile(Path())
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading config: %w", err)
	}
	if err == nil {
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("parsing config: %w", err)
		}
	}

	cfg.Mode = mode
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	if err := EnsureDir(); err != nil {
		return err
	}
	return os.WriteFile(Path(), out, 0600)
}

// SaveDatabases persists the secondary database list to the config file.
func SaveDatabases(databases []DatabaseConfig) error {
	cfg := Default
	data, err := os.ReadFile(Path())
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading config: %w", err)
	}
	if err == nil {
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("parsing config: %w", err)
		}
	}

	cfg.Databases = databases
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	if err := EnsureDir(); err != nil {
		return err
	}
	return os.WriteFile(Path(), out, 0600)
}

// SavePipelineConfig persists the pipeline config to the config file.
func SavePipelineConfig(pipelineCfg PipelineConfig) error {
	cfg := Default
	data, err := os.ReadFile(Path())
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading config: %w", err)
	}
	if err == nil {
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("parsing config: %w", err)
		}
	}

	cfg.Pipeline = pipelineCfg
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	if err := EnsureDir(); err != nil {
		return err
	}
	return os.WriteFile(Path(), out, 0600)
}

// SaveStewardConfig persists the steward config to the config file.
func SaveStewardConfig(stewardCfg StewardConfig) error {
	cfg := Default
	data, err := os.ReadFile(Path())
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading config: %w", err)
	}
	if err == nil {
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("parsing config: %w", err)
		}
	}

	cfg.Steward = stewardCfg
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	if err := EnsureDir(); err != nil {
		return err
	}
	return os.WriteFile(Path(), out, 0600)
}

func expandHome(path string) string {
	if len(path) > 0 && path[0] == '~' {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[1:])
	}
	return path
}

// keychainPrefix is the sentinel value in config that triggers keychain lookup.
const keychainPrefix = "keychain:"

// resolveCredential checks if a config value is a keychain reference.
// If it starts with "keychain:" or equals "keychain", the actual value is
// fetched from the OS keychain. Otherwise the value is returned as-is.
func resolveCredential(value, key string) string {
	if value == "keychain" || strings.HasPrefix(value, keychainPrefix) {
		resolved, err := credential.Get(key)
		if err != nil || resolved == "" {
			return "" // let caller handle missing URI
		}
		return resolved
	}
	return value
}

// StoreCredential saves a credential in the OS keychain and writes a sentinel
// to the config file so the credential is resolved from keychain on next load.
func StoreCredential(key, value string) error {
	if err := credential.Set(key, value); err != nil {
		return err
	}

	// Update config file to use keychain sentinel (for primary URI).
	if key == "mongodb_atlas_uri" {
		cfg := Default
		data, err := os.ReadFile(Path())
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("reading config: %w", err)
		}
		if err == nil {
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				return fmt.Errorf("parsing config: %w", err)
			}
		}
		cfg.MongoDBAtlasURI = "keychain:mongodb_atlas_uri"
		out, err := yaml.Marshal(cfg)
		if err != nil {
			return fmt.Errorf("marshaling config: %w", err)
		}
		if err := EnsureDir(); err != nil {
			return err
		}
		return os.WriteFile(Path(), out, 0600)
	}
	return nil
}

// GetAnthropicAPIKey returns the Anthropic API key, checking the OS
// keychain first, then falling back to the ANTHROPIC_API_KEY env var.
func GetAnthropicAPIKey() string {
	if key, err := credential.Get("anthropic_api_key"); err == nil && key != "" {
		return key
	}
	return os.Getenv("ANTHROPIC_API_KEY")
}

// DeleteCredentials removes all memoryd credentials from the OS keychain.
func DeleteCredentials() {
	_ = credential.Delete("mongodb_atlas_uri")
	_ = credential.Delete("anthropic_api_key")
	// Also clean up any per-database URI credentials.
	cfg := Default
	data, _ := os.ReadFile(Path())
	if data != nil {
		_ = yaml.Unmarshal(data, &cfg)
	}
	for _, db := range cfg.Databases {
		if db.Name != "" {
			_ = credential.Delete("db_uri_" + db.Name)
		}
	}
}
