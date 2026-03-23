package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Mode constants control how the daemon integrates with agents.
const (
	ModeProxy       = "proxy"        // Proxy intercepts API calls — auto read + write
	ModeMCP         = "mcp"          // MCP tools only — explicit read + write
	ModeMCPReadOnly = "mcp-readonly" // MCP tools, search only — no writes
)

// Config holds all daemon configuration.
// Database role constants.
const (
	RoleFull     = "full"      // Read + write
	RoleReadOnly = "read-only" // Search only, no writes
)

// DatabaseConfig describes a single team database connection.
type DatabaseConfig struct {
	Name     string `yaml:"name"`     // Human label (e.g., "platform", "payments")
	Database string `yaml:"database"` // MongoDB database name
	Role     string `yaml:"role"`     // "full" or "read-only"
}

type Config struct {
	Port                 int              `yaml:"port"`
	Mode                 string           `yaml:"mode"`
	MongoDBAtlasURI      string           `yaml:"mongodb_atlas_uri"`
	MongoDBDatabase      string           `yaml:"mongodb_database"`
	Databases            []DatabaseConfig `yaml:"databases,omitempty"`
	ModelPath            string           `yaml:"model_path"`
	EmbeddingDim         int              `yaml:"embedding_dim"`
	RetrievalTopK        int              `yaml:"retrieval_top_k"`
	RetrievalMaxTokens   int              `yaml:"retrieval_max_tokens"`
	UpstreamAnthropicURL string           `yaml:"upstream_anthropic_url"`
	AtlasMode            bool             `yaml:"atlas_mode"`
	Steward              StewardConfig    `yaml:"steward"`
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
	IntervalMinutes  int     `yaml:"interval_minutes"`
	PruneThreshold   float64 `yaml:"prune_threshold"`
	GracePeriodHours int     `yaml:"grace_period_hours"`
	DecayHalfDays    int     `yaml:"decay_half_days"`
	MergeThreshold   float64 `yaml:"merge_threshold"`
	BatchSize        int     `yaml:"batch_size"`
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
		DecayHalfDays:    7,
		MergeThreshold:   0.88,
		BatchSize:        500,
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

func expandHome(path string) string {
	if len(path) > 0 && path[0] == '~' {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[1:])
	}
	return path
}
