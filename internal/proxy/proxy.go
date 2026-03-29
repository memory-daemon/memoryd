package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/memory-daemon/memoryd/internal/config"
	"github.com/memory-daemon/memoryd/internal/embedding"
	"github.com/memory-daemon/memoryd/internal/ingest"
	"github.com/memory-daemon/memoryd/internal/pipeline"
	"github.com/memory-daemon/memoryd/internal/quality"
	"github.com/memory-daemon/memoryd/internal/rejection"
	"github.com/memory-daemon/memoryd/internal/store"
	"github.com/memory-daemon/memoryd/internal/synthesizer"
)

// Server is the memoryd HTTP proxy.
type Server struct {
	httpServer  *http.Server
	addr        string
	token       string
	version     string
	mode        string
	startedAt   time.Time
	mongoStatus func() string // optional: returns "connected", "connecting", etc.
}

type serverOpts struct {
	store        store.Store
	sourceStore  store.SourceStore
	ingester     *ingest.Ingester
	quality      *quality.Tracker
	embedder     embedding.Embedder
	stewardStats StewardStatsProvider
	synth        *synthesizer.Synthesizer
	rejLog       *rejection.Store
	mongoStatus  func() string
}

// StewardStatsProvider exposes steward sweep results to the dashboard.
type StewardStatsProvider interface {
	LastSweep() SweepStats
}

// SweepStats is a dashboard-friendly snapshot of the last steward sweep.
type SweepStats struct {
	Scored  int           `json:"scored"`
	Pruned  int           `json:"pruned"`
	Merged  int           `json:"merged"`
	Elapsed time.Duration `json:"elapsed_ms"`
}

// ServerOption configures the server.
type ServerOption func(*serverOpts)

// WithStore enables the web dashboard.
func WithStore(st store.Store) ServerOption {
	return func(o *serverOpts) { o.store = st }
}

// WithSourceStore enables source ingestion API endpoints.
func WithSourceStore(ss store.SourceStore) ServerOption {
	return func(o *serverOpts) { o.sourceStore = ss }
}

// WithIngester sets the source ingester.
func WithIngester(ing *ingest.Ingester) ServerOption {
	return func(o *serverOpts) { o.ingester = ing }
}

// WithQuality sets the quality tracker.
func WithQuality(qt *quality.Tracker) ServerOption {
	return func(o *serverOpts) { o.quality = qt }
}

// WithEmbedder sets the embedder for re-embedding edited content.
func WithEmbedder(emb embedding.Embedder) ServerOption {
	return func(o *serverOpts) { o.embedder = emb }
}

// WithStewardStats exposes steward sweep results on the dashboard.
func WithStewardStats(sp StewardStatsProvider) ServerOption {
	return func(o *serverOpts) { o.stewardStats = sp }
}

// WithSynthesizer attaches an LLM synthesizer for Q&A pairing and session distillation.
func WithSynthesizer(s *synthesizer.Synthesizer) ServerOption {
	return func(o *serverOpts) { o.synth = s }
}

// WithRejectionLog attaches the rejection log for pre-filter and synthesizer SKIP recording.
func WithRejectionLog(rl *rejection.Store) ServerOption {
	return func(o *serverOpts) { o.rejLog = rl }
}

// WithMongoStatus provides a callback that returns the current MongoDB connection status.
func WithMongoStatus(fn func() string) ServerOption {
	return func(o *serverOpts) { o.mongoStatus = fn }
}

// NewServer wires up all endpoints and returns a ready-to-start server.
func NewServer(cfg *config.Config, version string, read *pipeline.ReadPipeline, write *pipeline.WritePipeline, opts ...ServerOption) *Server {
	var so serverOpts
	for _, o := range opts {
		o(&so)
	}

	mux := http.NewServeMux()

	client := &http.Client{} // no timeout -- streaming responses can be long-lived

	mux.Handle("/v1/messages", newAnthropicHandler(cfg.UpstreamAnthropicURL, write, client, cfg.ProxyWriteEnabled(), so.synth, so.rejLog))
	mux.Handle("/v1/chat/completions", newOpenAIHandler())
	startedAt := time.Now()
	mode := cfg.Mode
	synthAvail := so.synth != nil && so.synth.Available()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		resp := map[string]any{
			"status":     "ok",
			"version":    version,
			"mode":       mode,
			"synthesis":  synthAvail,
			"started_at": startedAt.UTC().Format(time.RFC3339),
			"uptime_s":   int(time.Since(startedAt).Seconds()),
		}
		if so.mongoStatus != nil {
			resp["mongodb"] = so.mongoStatus()
		} else {
			resp["mongodb"] = "connected"
		}
		json.NewEncoder(w).Encode(resp)
	})

	if so.store != nil {
		registerDashboard(mux, so.store, so.sourceStore, so.quality, so.stewardStats)
		registerAPI(mux, so.store, read, write, so.embedder, cfg, so.rejLog, so.synth)
	}

	if so.sourceStore != nil && so.ingester != nil {
		registerSourceAPI(mux, so.sourceStore, so.store, so.ingester, so.quality)
	}

	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	var handler http.Handler = mux
	token := cfg.APIToken
	if token != "" {
		handler = authMiddleware(token, mux)
	}
	return &Server{
		httpServer: &http.Server{
			Addr:    addr,
			Handler: handler,
		},
		addr:        addr,
		token:       token,
		version:     version,
		mode:        mode,
		startedAt:   startedAt,
		mongoStatus: so.mongoStatus,
	}
}

func (s *Server) Start() error {
	fmt.Printf("memoryd listening on %s\n", s.addr)
	fmt.Printf("  export ANTHROPIC_BASE_URL=http://%s\n", s.addr)
	if s.token != "" {
		fmt.Printf("  Dashboard: http://%s/?token=%s\n", s.addr, s.token)
		fmt.Printf("  API token: %s\n", s.token)
	} else {
		fmt.Printf("  Dashboard: http://%s/\n", s.addr)
	}
	return s.httpServer.ListenAndServe()
}

func (s *Server) Stop() error {
	return s.httpServer.Close()
}

// Shutdown gracefully drains in-flight requests before closing.
// The provided context sets the deadline for draining; use a short timeout
// (e.g. 5s) so the process doesn't hang waiting for streaming responses.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}
