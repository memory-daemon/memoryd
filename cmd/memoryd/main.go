package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/memory-daemon/memoryd/internal/config"
	"github.com/memory-daemon/memoryd/internal/credential"
	"github.com/memory-daemon/memoryd/internal/embedding"
	"github.com/memory-daemon/memoryd/internal/export"
	"github.com/memory-daemon/memoryd/internal/ingest"
	"github.com/memory-daemon/memoryd/internal/mcp"
	"github.com/memory-daemon/memoryd/internal/pipeline"
	"github.com/memory-daemon/memoryd/internal/proxy"
	"github.com/memory-daemon/memoryd/internal/quality"
	"github.com/memory-daemon/memoryd/internal/rejection"
	"github.com/memory-daemon/memoryd/internal/steward"
	"github.com/memory-daemon/memoryd/internal/store"
	"github.com/memory-daemon/memoryd/internal/synthesizer"
)

var version = "dev"

func main() {
	root := &cobra.Command{
		Use:   "memoryd",
		Short: "Persistent memory layer for coding agents",
	}

	root.AddCommand(
		startCmd(),
		mcpCmd(),
		statusCmd(),
		searchCmd(),
		forgetCmd(),
		wipeCmd(),
		envCmd(),
		versionCmd(),
		ingestCmd(),
		uploadCmd(),
		sourcesCmd(),
		exportCmd(),
		credentialsCmd(),
		tokenCmd(),
	)

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func startCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the memoryd daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := config.WriteDefault(); err != nil {
				return fmt.Errorf("init config: %w", err)
			}

			cfg, err := config.Load()
			if err != nil {
				return err
			}

			token, err := config.EnsureToken()
			if err != nil {
				log.Printf("warning: could not create API token: %v — local API will be unprotected", err)
			}
			cfg.APIToken = token

			registerMCPServers()

			if cfg.MongoDBAtlasURI == "" {
				return fmt.Errorf("mongodb_atlas_uri is not configured.\n\n"+
					"  1. Edit %s and set mongodb_atlas_uri to your connection string, OR\n"+
					"  2. Store it securely: memoryd credentials set-uri \"mongodb+srv://...\"\n\n"+
					"  For local development with Docker:\n"+
					"    docker run -d --name memoryd-mongo -p 27017:27017 mongodb/mongodb-atlas-local:8.0\n"+
					"    Then set: mongodb_atlas_uri: \"mongodb://localhost:27017/?directConnection=true\"",
					config.Path())
			}

			// Stop any existing daemon on this port.
			addr := fmt.Sprintf("127.0.0.1:%d", cfg.Port)
			if existingIsMemoryd(addr) {
				log.Printf("Stopping existing memoryd on port %d...", cfg.Port)
				shutdownExisting(addr)
			} else if !portAvailable(addr) {
				return fmt.Errorf("port %d is already in use by another process", cfg.Port)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// Track MongoDB connection status for the health endpoint.
			var mongoStatus atomic.Value
			mongoStatus.Store("connecting")
			mongoStatusFn := func() string { return mongoStatus.Load().(string) }

			// connectMongo attempts to connect to all configured databases.
			// Returns nil entries and an error if the connection fails.
			connectMongo := func() ([]store.DatabaseEntry, error) {
				databases := cfg.ResolvedDatabases()
				var entries []store.DatabaseEntry
				for _, dbCfg := range databases {
					if !dbCfg.IsEnabled() {
						continue
					}
					uri := dbCfg.URI
					if uri == "" {
						uri = cfg.MongoDBAtlasURI
					}
					var ms *store.MongoStore
					var ss store.Store
					if cfg.AtlasMode {
						atlas, err := store.NewAtlasStore(ctx, uri, dbCfg.Database)
						if err != nil {
							return nil, fmt.Errorf("database %q: %w", dbCfg.Name, err)
						}
						ms = atlas.MongoStore
						ss = atlas
					} else {
						var err error
						ms, err = store.NewMongoStore(ctx, uri, dbCfg.Database)
						if err != nil {
							return nil, fmt.Errorf("database %q: %w", dbCfg.Name, err)
						}
						ss = ms
					}
					role := dbCfg.Role
					if role == "" {
						role = config.RoleFull
					}
					log.Printf("  [%s] database=%s role=%s atlas=%v", dbCfg.Name, dbCfg.Database, role, cfg.AtlasMode)
					entries = append(entries, store.DatabaseEntry{
						Name: dbCfg.Name, Database: dbCfg.Database,
						Role: role, URI: dbCfg.URI,
						Store: ms, SearchStore: ss, Mongo: ms,
					})
				}
				return entries, nil
			}

			// Try initial connection.
			log.Printf("Connecting to %d database(s)...", len(cfg.ResolvedDatabases()))
			entries, connErr := connectMongo()
			if connErr != nil {
				log.Printf("ERROR: MongoDB is not reachable: %v", connErr)
				log.Println("  ╔══════════════════════════════════════════════════════════════╗")
				log.Println("  ║  MongoDB is required. memoryd will keep retrying.           ║")
				log.Println("  ║                                                              ║")
				log.Println("  ║  For local development:                                      ║")
				log.Println("  ║    1. Start Docker Desktop                                   ║")
				log.Println("  ║    2. docker start memoryd-mongo                             ║")
				log.Println("  ║       (or: docker run -d --name memoryd-mongo \\              ║")
				log.Println("  ║         -p 27017:27017 mongodb/mongodb-atlas-local:8.0)      ║")
				log.Println("  ║                                                              ║")
				log.Println("  ║  For Atlas: check your connection string in config.yaml      ║")
				log.Println("  ╚══════════════════════════════════════════════════════════════╝")
				mongoStatus.Store("disconnected")
			} else {
				mongoStatus.Store("connected")
			}

			// wiredMu protects the full-pipeline state so the background retry
			// goroutine can safely swap it in after a delayed connection.
			var wiredMu sync.Mutex
			var (
				multi    *store.MultiStore
				emb      embedding.Embedder
				read     *pipeline.ReadPipeline
				write    *pipeline.WritePipeline
				qt       *quality.Tracker
				rejLog   *rejection.Store
				ing      *ingest.Ingester
				stewards []*steward.Steward
				synth    *synthesizer.Synthesizer
			)

			// wirePipeline sets up the full processing pipeline once MongoDB is connected.
			wirePipeline := func(entries []store.DatabaseEntry) error {
				var err error
				multi, err = store.NewMultiStore(entries)
				if err != nil {
					return fmt.Errorf("building multi-store: %w", err)
				}
				primary := multi.Primary()

				if err := primary.Mongo.CheckVectorIndex(ctx); err != nil {
					log.Printf("WARNING: %v", err)
					log.Println("memoryd will start, but vector search will not work until the index is created.")
				} else {
					log.Println("  vector_index verified")
				}

				log.Println("Loading embedding model...")
				emb, err = embedding.NewLlamaEmbedder(cfg.ModelPath, cfg.EmbeddingDim)
				if err != nil {
					return err
				}

				qt = quality.NewTracker(primary.Mongo, quality.DefaultThreshold)
				read = pipeline.NewReadPipeline(emb, multi, cfg, pipeline.WithQualityTracker(qt))

				scorer, err := quality.NewContentScorerWithProtos(ctx, emb, cfg.Pipeline.QualityProtos, cfg.Pipeline.NoiseProtos)
				if err != nil {
					log.Printf("warning: content scorer unavailable, chunks will not be quality-scored: %v", err)
				}

				if cfg.LLMSynthesis {
					apiKey := config.GetAnthropicAPIKey()
					synth = synthesizer.New(apiKey, cfg.UpstreamAnthropicURL)
					if synth.Available() {
						log.Printf("LLM synthesis enabled (model: claude-haiku-4-5-20251001)")
					} else {
						log.Printf("LLM synthesis: enabled in config but no Anthropic API key found — disabled")
					}
				} else {
					synth = synthesizer.New("", "")
					log.Printf("LLM synthesis disabled (set llm_synthesis: true in config to enable)")
				}

				write = pipeline.NewWritePipeline(emb, primary.Store,
					pipeline.WithContentScorer(scorer),
					pipeline.WithSynthesizer(synth),
					pipeline.WithPipelineConfig(cfg.Pipeline),
				)

				rejStorePath := filepath.Join(config.Dir(), "rejection_log.jsonl")
				rejLog, err = rejection.Open(rejStorePath, 0)
				if err != nil {
					log.Printf("warning: could not open rejection store: %v", err)
				} else {
					log.Printf("rejection store: %s (%d entries)", rejStorePath, rejLog.Len())
				}

				if rejLog != nil {
					go func() {
						if rejLog.Len() > 0 {
							texts := rejLog.Texts()
							if s, err := quality.NewContentScorerFromRejections(ctx, emb, texts, cfg.Pipeline.QualityProtos); err == nil {
								write.UpdateScorer(s)
								log.Printf("[rejection] scorer seeded with %d noise prototypes from stored rejections", len(texts))
							}
						}
						for {
							select {
							case <-ctx.Done():
								return
							case _, ok := <-rejLog.RebuildCh():
								if !ok {
									return
								}
								texts := rejLog.Texts()
								s, err := quality.NewContentScorerFromRejections(ctx, emb, texts, cfg.Pipeline.QualityProtos)
								if err != nil {
									log.Printf("[rejection] scorer rebuild error: %v", err)
									continue
								}
								write.UpdateScorer(s)
								log.Printf("[rejection] scorer rebuilt with %d noise prototypes", len(texts))
							}
						}
					}()
				}

				ing = ingest.NewIngester(emb, primary.Mongo, primary.Mongo)

				stwCfg := steward.Config{
					Interval:         cfg.Steward.Interval(),
					PruneThreshold:   cfg.Steward.PruneThreshold,
					PruneGracePeriod: cfg.Steward.GracePeriod(),
					DecayHalfLife:    cfg.Steward.DecayHalfLife(),
					MergeThreshold:   cfg.Steward.MergeThreshold,
					BatchSize:        cfg.Steward.BatchSize,
				}
				for _, e := range multi.Entries() {
					if !e.IsWritable() {
						continue
					}
					stw := steward.New(stwCfg, e.Mongo, emb)
					stw.Start(ctx)
					stewards = append(stewards, stw)
					log.Printf("  [%s] steward started", e.Name)
				}

				return nil
			}

			// Wire up the pipeline if MongoDB connected on first try.
			if connErr == nil {
				if err := wirePipeline(entries); err != nil {
					return err
				}
			}

			// If MongoDB failed, retry in the background.
			if connErr != nil {
				go func() {
					backoff := 10 * time.Second
					const maxBackoff = 60 * time.Second
					for {
						select {
						case <-ctx.Done():
							return
						case <-time.After(backoff):
						}

						log.Println("Retrying MongoDB connection...")
						entries, err := connectMongo()
						if err != nil {
							log.Printf("  MongoDB still unreachable: %v", err)
							if backoff < maxBackoff {
								backoff = backoff * 2
								if backoff > maxBackoff {
									backoff = maxBackoff
								}
							}
							continue
						}

						log.Println("MongoDB connected!")
						mongoStatus.Store("connected")

						wiredMu.Lock()
						if wErr := wirePipeline(entries); wErr != nil {
							log.Printf("ERROR: failed to wire pipeline after connect: %v", wErr)
							wiredMu.Unlock()
							continue
						}
						wiredMu.Unlock()

						log.Println("Full pipeline active — memoryd is fully operational")
						return
					}
				}()
			}

			// Build server options — may be nil if MongoDB isn't connected yet.
			// The proxy handles nil store gracefully (no dashboard/API, passthrough only).
			var serverOpts []proxy.ServerOption
			serverOpts = append(serverOpts, proxy.WithMongoStatus(mongoStatusFn))

			wiredMu.Lock()
			if multi != nil {
				serverOpts = append(serverOpts,
					proxy.WithStore(multi),
					proxy.WithSourceStore(multi.Primary().Mongo),
					proxy.WithEmbedder(emb),
					proxy.WithSynthesizer(synth),
					proxy.WithRejectionLog(rejLog),
					proxy.WithQuality(qt),
					proxy.WithIngester(ing),
				)
				if len(stewards) > 0 {
					serverOpts = append(serverOpts, proxy.WithStewardStats(&stewardAdapter{stewards: stewards}))
				}
			}
			wiredMu.Unlock()

			srv := proxy.NewServer(cfg, version, read, write, serverOpts...)

			// Graceful shutdown on SIGINT / SIGTERM
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

			go func() {
				<-sigCh
				log.Println("Shutting down...")

				shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer shutCancel()
				if err := srv.Shutdown(shutCtx); err != nil {
					log.Printf("server shutdown error: %v", err)
				}

				for _, stw := range stewards {
					stw.Stop()
				}
				cancel()

				if rejLog != nil {
					rejLog.Close()
				}
				if emb != nil {
					emb.Close()
				}
			}()

			return srv.Start()
		},
	}
}

// portAvailable checks whether the address can be bound.
func portAvailable(addr string) bool {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// existingIsMemoryd checks if a running memoryd instance is on the port.
func existingIsMemoryd(addr string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result map[string]string
	if json.Unmarshal(body, &result) != nil {
		return false
	}
	return result["status"] == "ok"
}

// shutdownExisting sends SIGTERM to the running daemon and waits for the port to free up.
func shutdownExisting(addr string) {
	pid := findListenerPID(addr)
	if pid > 0 {
		if proc, err := os.FindProcess(pid); err == nil {
			proc.Signal(syscall.SIGTERM)
		}
	}

	// Wait for port to become available.
	for i := 0; i < 30; i++ {
		time.Sleep(200 * time.Millisecond)
		if portAvailable(addr) {
			return
		}
	}
	log.Println("Warning: timed out waiting for previous instance to stop")
}

// findListenerPID returns the PID of the process listening on addr, or 0 if not found.
func findListenerPID(addr string) int {
	_, port, _ := net.SplitHostPort(addr)
	out, err := exec.Command("lsof", "-ti", "tcp:"+port, "-sTCP:LISTEN").Output()
	if err != nil {
		return 0
	}
	line := strings.TrimSpace(string(out))
	lines := strings.Split(line, "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil {
		return 0
	}
	return pid
}

func mcpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Run as an MCP (Model Context Protocol) server over stdio",
		Long:  "Starts an MCP server that communicates over stdin/stdout. Requires the memoryd daemon to be running.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			// Verify daemon is running
			resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", cfg.Port))
			if err != nil {
				return fmt.Errorf("memoryd daemon is not running -- start it with: memoryd start")
			}
			resp.Body.Close()

			srv := mcp.NewServer(cfg.Port, cfg.MCPReadOnly(), config.LoadToken())
			return srv.Run()
		},
	}
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Check if the daemon is running",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", cfg.Port))
			if err != nil {
				fmt.Println("memoryd is not running")
				return nil
			}
			resp.Body.Close()
			fmt.Printf("memoryd is running on port %d\n", cfg.Port)
			return nil
		},
	}
}

func searchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "search [query]",
		Short: "Search stored memories (regex on content)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := connectStore()
			if err != nil {
				return err
			}
			defer st.Close()

			memories, err := st.List(context.Background(), args[0], 10)
			if err != nil {
				return err
			}
			if len(memories) == 0 {
				fmt.Println("No memories found.")
				return nil
			}
			for _, m := range memories {
				data, _ := json.MarshalIndent(m, "", "  ")
				fmt.Println(string(data))
				fmt.Println("---")
			}
			return nil
		},
	}
}

func forgetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "forget [id]",
		Short: "Delete a specific memory by ID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := connectStore()
			if err != nil {
				return err
			}
			defer st.Close()

			if err := st.Delete(context.Background(), args[0]); err != nil {
				return err
			}
			fmt.Println("Memory deleted.")
			return nil
		},
	}
}

func wipeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "wipe",
		Short: "Delete all stored memories",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Print("This will delete ALL stored memories. Type 'yes' to confirm: ")
			var confirm string
			fmt.Scanln(&confirm)
			if confirm != "yes" {
				fmt.Println("Aborted.")
				return nil
			}

			st, err := connectStore()
			if err != nil {
				return err
			}
			defer st.Close()

			if err := st.DeleteAll(context.Background()); err != nil {
				return err
			}

			// Also remove all knowledge sources and their page records.
			sources, _ := st.ListSources(context.Background())
			for _, s := range sources {
				if err := st.DeleteSourcePages(context.Background(), s.ID); err != nil {
					log.Printf("delete source pages error: %v", err)
				}
				if err := st.DeleteSource(context.Background(), s.ID.Hex()); err != nil {
					log.Printf("delete source error: %v", err)
				}
			}

			fmt.Println("All memories deleted.")
			return nil
		},
	}
}

func envCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "env",
		Short: "Print shell export commands for integration",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			fmt.Printf("export ANTHROPIC_BASE_URL=http://127.0.0.1:%d\n", cfg.Port)
			return nil
		},
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print memoryd version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("memoryd %s\n", version)
		},
	}
}

func tokenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "token",
		Short: "Print the dashboard API token",
		RunE: func(cmd *cobra.Command, args []string) error {
			token, err := config.EnsureToken()
			if err != nil {
				return fmt.Errorf("could not load or generate token: %w", err)
			}
			fmt.Println(token)
			return nil
		},
	}
}

func credentialsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "credentials",
		Short: "Manage stored credentials (OS keychain)",
	}

	cmd.AddCommand(
		&cobra.Command{
			Use:   "set-uri <mongodb_uri>",
			Short: "Store MongoDB URI in OS keychain",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				if err := config.StoreCredential("mongodb_atlas_uri", args[0]); err != nil {
					return fmt.Errorf("storing credential: %w", err)
				}
				fmt.Println("MongoDB URI stored in OS keychain.")
				fmt.Println("Config updated to use keychain reference.")
				return nil
			},
		},
		&cobra.Command{
			Use:   "set-api-key <anthropic_api_key>",
			Short: "Store Anthropic API key in OS keychain",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				if err := credential.Set("anthropic_api_key", args[0]); err != nil {
					return fmt.Errorf("storing credential: %w", err)
				}
				fmt.Println("Anthropic API key stored in OS keychain.")
				fmt.Println("Restart the daemon to pick it up.")
				return nil
			},
		},
		&cobra.Command{
			Use:   "clear",
			Short: "Remove all memoryd credentials from OS keychain",
			RunE: func(cmd *cobra.Command, args []string) error {
				config.DeleteCredentials()
				fmt.Println("All memoryd credentials removed from OS keychain.")
				return nil
			},
		},
		&cobra.Command{
			Use:   "check",
			Short: "Check if credentials are stored in OS keychain",
			RunE: func(cmd *cobra.Command, args []string) error {
				uri, err := credential.Get("mongodb_atlas_uri")
				if err != nil {
					fmt.Println("Keychain: not available (", err, ")")
					return nil
				}
				if uri == "" {
					fmt.Println("Keychain: no MongoDB URI stored")
				} else {
					masked := uri
					if len(uri) > 20 {
						masked = uri[:20] + "..." + uri[len(uri)-4:]
					}
					fmt.Printf("Keychain: MongoDB URI stored (%s)\n", masked)
				}

				apiKey, _ := credential.Get("anthropic_api_key")
				if apiKey == "" {
					fmt.Println("Keychain: no Anthropic API key stored")
				} else {
					masked := apiKey
					if len(apiKey) > 12 {
						masked = apiKey[:7] + "••••" + apiKey[len(apiKey)-4:]
					}
					fmt.Printf("Keychain: Anthropic API key stored (%s)\n", masked)
				}
				return nil
			},
		},
	)

	return cmd
}

// daemonRequest creates an HTTP request to the local daemon with the API token set.
func daemonRequest(method, url string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	if token := config.LoadToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// connectStore creates a direct Atlas connection for CLI commands.
func connectStore() (*store.MongoStore, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if cfg.MongoDBAtlasURI == "" {
		return nil, fmt.Errorf("mongodb_atlas_uri not configured -- edit %s", config.Path())
	}
	return store.NewMongoStore(context.Background(), cfg.MongoDBAtlasURI, cfg.DefaultDatabase())
}

func ingestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ingest [url]",
		Short: "Ingest a data source (crawl a URL) via the running daemon",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, _ := cmd.Flags().GetString("name")
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			maxDepth, _ := cmd.Flags().GetInt("max-depth")
			maxPages, _ := cmd.Flags().GetInt("max-pages")

			cfg, err := config.Load()
			if err != nil {
				return err
			}

			headerFlags, _ := cmd.Flags().GetStringSlice("header")
			headers := make(map[string]string)
			for _, h := range headerFlags {
				if k, v, ok := strings.Cut(h, ":"); ok {
					headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
				}
			}

			payload := map[string]any{
				"base_url":  args[0],
				"name":      name,
				"max_depth": maxDepth,
				"max_pages": maxPages,
			}
			if len(headers) > 0 {
				payload["headers"] = headers
			}
			body, _ := json.Marshal(payload)

			req, _ := daemonRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/api/sources", cfg.Port), bytes.NewReader(body))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return fmt.Errorf("daemon not running -- start it with: memoryd start")
			}
			defer resp.Body.Close()

			data, _ := io.ReadAll(resp.Body)
			var result map[string]string
			json.Unmarshal(data, &result)

			if resp.StatusCode != 200 {
				return fmt.Errorf("ingest failed: %s", result["error"])
			}

			fmt.Printf("Source '%s' created (id: %s). Crawl started.\n", name, result["id"])
			return nil
		},
	}
	cmd.Flags().String("name", "", "Short label for this source (required)")
	cmd.Flags().Int("max-depth", 3, "Maximum crawl depth")
	cmd.Flags().Int("max-pages", 500, "Maximum pages to crawl")
	cmd.Flags().StringSlice("header", nil, "HTTP header as 'Key: Value' (repeatable, e.g. --header 'Cookie: session=abc' --header 'Authorization: Bearer tok')")
	return cmd
}

func uploadCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "upload [path]",
		Short: "Upload files or a directory to seed long-term memory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, _ := cmd.Flags().GetString("name")
			if name == "" {
				return fmt.Errorf("--name is required")
			}

			cfg, err := config.Load()
			if err != nil {
				return err
			}

			root := args[0]
			var files []map[string]string

			supportedExts := map[string]bool{
				".md": true, ".txt": true, ".html": true, ".htm": true,
				".json": true, ".yaml": true, ".yml": true,
				".go": true, ".py": true, ".js": true, ".ts": true,
				".rst": true, ".xml": true, ".csv": true, ".toml": true,
			}

			info, err := os.Stat(root)
			if err != nil {
				return fmt.Errorf("cannot access %s: %w", root, err)
			}

			var paths []string
			if info.IsDir() {
				err = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
					if err != nil {
						return err
					}
					if !fi.IsDir() && supportedExts[filepath.Ext(p)] {
						paths = append(paths, p)
					}
					return nil
				})
				if err != nil {
					return fmt.Errorf("walking directory: %w", err)
				}
			} else {
				paths = []string{root}
			}

			if len(paths) == 0 {
				return fmt.Errorf("no supported files found in %s", root)
			}

			for _, p := range paths {
				data, err := os.ReadFile(p)
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: skipping %s: %v\n", p, err)
					continue
				}
				rel, _ := filepath.Rel(filepath.Dir(root), p)
				if rel == "" {
					rel = filepath.Base(p)
				}
				files = append(files, map[string]string{"filename": rel, "content": string(data)})
			}

			if len(files) == 0 {
				return fmt.Errorf("no files could be read")
			}

			fmt.Printf("Uploading %d files as source '%s'...\n", len(files), name)

			payload := map[string]any{"name": name, "files": files}
			body, _ := json.Marshal(payload)

			req, _ := daemonRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/api/sources/upload", cfg.Port), bytes.NewReader(body))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return fmt.Errorf("daemon not running -- start it with: memoryd start")
			}
			defer resp.Body.Close()

			respData, _ := io.ReadAll(resp.Body)
			var result map[string]string
			json.Unmarshal(respData, &result)

			if resp.StatusCode != 200 {
				return fmt.Errorf("upload failed: %s", result["error"])
			}

			fmt.Printf("Source '%s' created (id: %s). %s\n", name, result["id"], result["message"])
			return nil
		},
	}
	cmd.Flags().String("name", "", "Short label for this source (required)")
	return cmd
}

func sourcesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sources",
		Short: "List ingested data sources",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			req, _ := daemonRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/api/sources", cfg.Port), nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return fmt.Errorf("daemon not running -- start it with: memoryd start")
			}
			defer resp.Body.Close()

			data, _ := io.ReadAll(resp.Body)
			var sources []map[string]any
			json.Unmarshal(data, &sources)

			if len(sources) == 0 {
				fmt.Println("No sources configured.")
				return nil
			}
			for _, s := range sources {
				out, _ := json.MarshalIndent(s, "", "  ")
				fmt.Println(string(out))
				fmt.Println("---")
			}
			return nil
		},
	}

	removeCmd := &cobra.Command{
		Use:   "remove [id]",
		Short: "Remove an ingested source and all its memories",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			req, _ := daemonRequest("DELETE",
				fmt.Sprintf("http://127.0.0.1:%d/api/sources/%s", cfg.Port, args[0]), nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return fmt.Errorf("daemon not running -- start it with: memoryd start")
			}
			defer resp.Body.Close()

			data, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 {
				return fmt.Errorf("remove failed: %s", string(data))
			}
			fmt.Println("Source removed.")
			return nil
		},
	}

	cmd.AddCommand(removeCmd)
	return cmd
}

func exportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export the knowledge base as a set of markdown documents",
		RunE: func(cmd *cobra.Command, args []string) error {
			outDir, _ := cmd.Flags().GetString("output")
			minQuality, _ := cmd.Flags().GetFloat64("min-quality")

			st, err := connectStore()
			if err != nil {
				return err
			}
			defer st.Close()

			return export.Run(context.Background(), st, export.Options{
				OutputDir:       outDir,
				MinQualityScore: minQuality,
				Format:          "markdown",
			})
		},
	}
	cmd.Flags().StringP("output", "o", "memoryd-export", "Output directory for the doc set")
	cmd.Flags().Float64("min-quality", 0, "Only include memories with quality_score >= this value (0 = all)")
	return cmd
}

// stewardAdapter bridges steward.Steward to the proxy.StewardStatsProvider
// interface by aggregating stats from all active stewards.
type stewardAdapter struct {
	stewards []*steward.Steward
}

func (a *stewardAdapter) LastSweep() proxy.SweepStats {
	var total proxy.SweepStats
	for _, s := range a.stewards {
		st := s.LastStats()
		total.Scored += st.Scored
		total.Pruned += st.Pruned
		total.Merged += st.Merged
		if st.Elapsed > total.Elapsed {
			total.Elapsed = st.Elapsed
		}
	}
	return total
}
