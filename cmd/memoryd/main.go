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
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/kindling-sh/memoryd/internal/config"
	"github.com/kindling-sh/memoryd/internal/embedding"
	"github.com/kindling-sh/memoryd/internal/export"
	"github.com/kindling-sh/memoryd/internal/ingest"
	"github.com/kindling-sh/memoryd/internal/mcp"
	"github.com/kindling-sh/memoryd/internal/pipeline"
	"github.com/kindling-sh/memoryd/internal/proxy"
	"github.com/kindling-sh/memoryd/internal/quality"
	"github.com/kindling-sh/memoryd/internal/steward"
	"github.com/kindling-sh/memoryd/internal/store"
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

			if cfg.MongoDBAtlasURI == "" {
				return fmt.Errorf("mongodb_atlas_uri is required -- edit %s", config.Path())
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

			// 1. Connect to MongoDB — build entries for each configured database
			databases := cfg.ResolvedDatabases()
			log.Printf("Connecting to %d database(s)...", len(databases))

			var entries []store.DatabaseEntry
			for _, dbCfg := range databases {
				var ms *store.MongoStore
				var ss store.Store
				if cfg.AtlasMode {
					atlas, err := store.NewAtlasStore(ctx, cfg.MongoDBAtlasURI, dbCfg.Database)
					if err != nil {
						return fmt.Errorf("connecting to database %q: %w", dbCfg.Name, err)
					}
					ms = atlas.MongoStore
					ss = atlas
				} else {
					var err error
					ms, err = store.NewMongoStore(ctx, cfg.MongoDBAtlasURI, dbCfg.Database)
					if err != nil {
						return fmt.Errorf("connecting to database %q: %w", dbCfg.Name, err)
					}
					ss = ms
				}
				role := dbCfg.Role
				if role == "" {
					role = config.RoleFull
				}
				log.Printf("  [%s] database=%s role=%s atlas=%v", dbCfg.Name, dbCfg.Database, role, cfg.AtlasMode)
				entries = append(entries, store.DatabaseEntry{
					Name:        dbCfg.Name,
					Database:    dbCfg.Database,
					Role:        role,
					Store:       ms,
					SearchStore: ss,
					Mongo:       ms,
				})
			}

			multi, err := store.NewMultiStore(entries)
			if err != nil {
				return fmt.Errorf("building multi-store: %w", err)
			}
			defer multi.Close()

			primary := multi.Primary()

			// 2. Start the embedding model
			log.Println("Loading embedding model...")
			emb, err := embedding.NewLlamaEmbedder(cfg.ModelPath, cfg.EmbeddingDim)
			if err != nil {
				return err
			}
			defer emb.Close()

			// 3. Build pipelines
			// Read pipeline uses MultiStore (fan-out search across all databases).
			// Write pipeline uses primary store (default write target).
			qt := quality.NewTracker(primary.Mongo, quality.DefaultThreshold)
			read := pipeline.NewReadPipeline(emb, multi, cfg, pipeline.WithQualityTracker(qt))
			write := pipeline.NewWritePipeline(emb, primary.Store)

			// 4. Build ingester (operates on primary database)
			ing := ingest.NewIngester(emb, primary.Mongo, primary.Mongo)

			// 5. Start the proxy
			srv := proxy.NewServer(cfg, read, write,
				proxy.WithStore(multi),
				proxy.WithSourceStore(primary.Mongo),
				proxy.WithIngester(ing),
				proxy.WithQuality(qt),
				proxy.WithEmbedder(emb),
			)

			// 6. Start a steward for each writable database
			stwCfg := steward.Config{
				Interval:         cfg.Steward.Interval(),
				PruneThreshold:   cfg.Steward.PruneThreshold,
				PruneGracePeriod: cfg.Steward.GracePeriod(),
				DecayHalfLife:    cfg.Steward.DecayHalfLife(),
				MergeThreshold:   cfg.Steward.MergeThreshold,
				BatchSize:        cfg.Steward.BatchSize,
			}
			var stewards []*steward.Steward
			for _, e := range multi.Entries() {
				if !e.IsWritable() {
					continue
				}
				stw := steward.New(stwCfg, e.Mongo)
				stw.Start(ctx)
				stewards = append(stewards, stw)
				log.Printf("  [%s] steward started", e.Name)
			}

			// Graceful shutdown on SIGINT / SIGTERM
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

			go func() {
				<-sigCh
				log.Println("Shutting down...")
				for _, stw := range stewards {
					stw.Stop()
				}
				if err := srv.Stop(); err != nil {
					log.Printf("server stop error: %v", err)
				}
				cancel()
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

			srv := mcp.NewServer(cfg.Port, cfg.MCPReadOnly())
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

			resp, err := http.Post(
				fmt.Sprintf("http://127.0.0.1:%d/api/sources", cfg.Port),
				"application/json",
				bytes.NewReader(body),
			)
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

			resp, err := http.Post(
				fmt.Sprintf("http://127.0.0.1:%d/api/sources/upload", cfg.Port),
				"application/json",
				bytes.NewReader(body),
			)
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

			resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/sources", cfg.Port))
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

			req, _ := http.NewRequest("DELETE",
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
