package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/memory-daemon/memoryd/internal/config"
	"github.com/memory-daemon/memoryd/internal/credential"
	"github.com/memory-daemon/memoryd/internal/embedding"
	"github.com/memory-daemon/memoryd/internal/pipeline"
	"github.com/memory-daemon/memoryd/internal/quality"
	"github.com/memory-daemon/memoryd/internal/redact"
	"github.com/memory-daemon/memoryd/internal/rejection"
	"github.com/memory-daemon/memoryd/internal/store"
	"github.com/memory-daemon/memoryd/internal/synthesizer"
)

type apiHandler struct {
	store    store.Store
	multi    *store.MultiStore // non-nil when multi-database is active
	read     *pipeline.ReadPipeline
	write    *pipeline.WritePipeline
	embedder embedding.Embedder
	cfg      *config.Config
	rejLog   *rejection.Store
	synth    *synthesizer.Synthesizer
}

func registerAPI(mux *http.ServeMux, st store.Store, read *pipeline.ReadPipeline, write *pipeline.WritePipeline, emb embedding.Embedder, cfg *config.Config, rejLog *rejection.Store, synth *synthesizer.Synthesizer) {
	h := &apiHandler{store: st, read: read, write: write, embedder: emb, cfg: cfg, rejLog: rejLog, synth: synth}
	if ms, ok := st.(*store.MultiStore); ok {
		h.multi = ms
	}
	mux.HandleFunc("/api/search", h.handleSearch)
	mux.HandleFunc("/api/store", h.handleStore)
	mux.HandleFunc("/api/ingest", h.handleIngest)
	mux.HandleFunc("/api/memories", h.handleMemories)
	mux.HandleFunc("/api/memories/", h.handleMemoryByID)
	mux.HandleFunc("/api/databases", h.handleDatabases)
	mux.HandleFunc("/api/databases/", h.handleDatabaseByName)
	mux.HandleFunc("/api/pipeline", h.handlePipelineConfig)
	mux.HandleFunc("/api/prompts", h.handlePrompts)
	mux.HandleFunc("/api/rejections", h.handleRejections)
	mux.HandleFunc("/api/settings", h.handleSettings)
	mux.HandleFunc("/api/export", h.handleExport)
	mux.HandleFunc("/api/logs", handleLogs)
}

func (a *apiHandler) handleSearch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}

	var req struct {
		Query    string `json:"query"`
		Database string `json:"database,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Query == "" {
		writeJSON(w, 400, map[string]string{"error": "query is required"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// If a specific database is requested and multi-database is active, search it directly.
	if req.Database != "" && a.multi != nil {
		vec, err := a.embedder.Embed(ctx, req.Query)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "embedding failed: " + err.Error()})
			return
		}
		mems, err := a.multi.SearchTargeted(ctx, req.Database, vec, 5)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		formatted := pipeline.FormatContext(mems, 2048)
		if formatted == "" {
			formatted = "No relevant memories found."
		}
		writeJSON(w, 200, map[string]string{"context": formatted})
		return
	}

	retrieved, memories, err := a.read.RetrieveWithScores(ctx, req.Query)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}

	scores := make([]float64, len(memories))
	for i, m := range memories {
		scores[i] = m.Score
	}
	writeJSON(w, 200, map[string]any{"context": retrieved, "scores": scores})
}

func (a *apiHandler) handleStore(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}

	var req struct {
		Content  string         `json:"content"`
		Source   string         `json:"source,omitempty"`
		Metadata map[string]any `json:"metadata,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Content == "" {
		writeJSON(w, 400, map[string]string{"error": "content is required"})
		return
	}
	if req.Source == "" {
		req.Source = "mcp"
	}

	result := a.write.ProcessFiltered(req.Content, req.Source, req.Metadata)

	writeJSON(w, 200, map[string]string{"status": "ok", "summary": result.Summary()})
}

// handleIngest handles POST /api/ingest.
// Runs a user+assistant exchange through the full quality pipeline:
// pre-filter → SynthesizeQA → write. Returns the stage that handled the
// exchange and, if stored, the distilled entry text.
//
// Request: {"user_prompt": "...", "assistant_response": "...", "source": "..."}
// Response: {"stage": "stored|pre_filter|length_filter|content_score_filter|synthesizer_skip|noise_filtered|no_synthesizer", "stored": 0|1, "entry": "..."}
func (a *apiHandler) handleIngest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}

	var req struct {
		UserPrompt        string `json:"user_prompt"`
		AssistantResponse string `json:"assistant_response"`
		Source            string `json:"source,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.AssistantResponse == "" {
		writeJSON(w, 400, map[string]string{"error": "assistant_response is required"})
		return
	}
	if req.Source == "" {
		req.Source = "eval"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	pCfg := a.write.Config()

	// --- Pre-Haiku gates (ordered cheapest → most expensive) ---

	// 1. String-match pre-filter (ack + procedural prefix).
	if req.UserPrompt != "" && rejection.QuickFilter(req.UserPrompt, req.AssistantResponse) {
		a.rejLog.Add(rejection.StagePreFilter, req.UserPrompt, req.AssistantResponse)
		writeJSON(w, 200, map[string]any{"stage": "pre_filter", "stored": 0})
		return
	}

	// 2. Length gate — responses too short to contain durable knowledge.
	if pCfg.IngestMinLen > 0 && len(strings.TrimSpace(req.AssistantResponse)) < pCfg.IngestMinLen {
		a.rejLog.Add(rejection.StagePreFilter, req.UserPrompt, req.AssistantResponse)
		writeJSON(w, 200, map[string]any{"stage": "length_filter", "stored": 0})
		return
	}

	// 3. Content score pre-gate — embed raw text, score against noise prototypes.
	//    This is the adaptive feedback loop: rejections train the scorer, and
	//    the scorer blocks future similar noise before the expensive Haiku call.
	//    Note: we do NOT add these rejections back to the rejection store — the
	//    store should only learn from Haiku SKIP verdicts and pre-filter catches.
	//    Adding scorer-filtered items back would create a positive feedback loop.
	if pCfg.ContentScorePreGate > 0 {
		if score, ok := a.write.PreScore(ctx, req.AssistantResponse); ok && score < pCfg.ContentScorePreGate {
			writeJSON(w, 200, map[string]any{"stage": "content_score_filter", "stored": 0})
			return
		}
	}

	// --- Haiku LLM quality gate (when available) or raw storage fallback ---
	if a.synth.Available() {
		facts, err := a.synth.SynthesizeQA(ctx, req.UserPrompt, req.AssistantResponse, "")
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "synthesis error: " + err.Error()})
			return
		}
		if len(facts) == 0 {
			a.rejLog.Add(rejection.StageSynthesizer, req.UserPrompt, req.AssistantResponse)
			writeJSON(w, 200, map[string]any{"stage": "synthesizer_skip", "stored": 0})
			return
		}

		var totalStored int
		for _, fact := range facts {
			result := a.write.ProcessDirect(fact, req.Source, nil)
			totalStored += result.Stored
		}
		writeJSON(w, 200, map[string]any{
			"stage":  "stored",
			"stored": totalStored,
			"facts":  len(facts),
		})
	} else {
		// No synthesizer — store raw Q&A through the chunking pipeline.
		var text string
		if req.UserPrompt != "" {
			text = fmt.Sprintf("Q: %s\n\nA: %s", req.UserPrompt, req.AssistantResponse)
		} else {
			text = req.AssistantResponse
		}
		result := a.write.ProcessFiltered(text, req.Source, nil)
		writeJSON(w, 200, map[string]any{
			"stage":   "stored_raw",
			"stored":  result.Stored,
			"summary": result.Summary(),
		})
	}
}

func (a *apiHandler) handleMemories(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	query := r.URL.Query().Get("q")
	memories, err := a.store.List(ctx, query, 0)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, 200, memories)
}

func (a *apiHandler) handleMemoryByID(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	id := r.URL.Path[len("/api/memories/"):]
	if id == "" {
		writeJSON(w, 400, map[string]string{"error": "id is required"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	switch r.Method {
	case http.MethodDelete:
		if err := a.store.Delete(ctx, id); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})

	case http.MethodPut:
		var req struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid JSON"})
			return
		}
		if req.Content == "" {
			writeJSON(w, 400, map[string]string{"error": "content is required"})
			return
		}
		// Redact before storing.
		cleaned := redact.Clean(req.Content)
		// Re-embed the updated content.
		vec, err := a.embedder.Embed(ctx, cleaned)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "embedding failed: " + err.Error()})
			return
		}
		if err := a.store.UpdateContent(ctx, id, cleaned, vec); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})

	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

func (a *apiHandler) handleDatabases(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		if a.multi == nil {
			writeJSON(w, 200, []any{})
			return
		}
		writeJSON(w, 200, a.multi.DatabaseList())

	case http.MethodPost:
		// Add a secondary database.
		if a.multi == nil {
			writeJSON(w, 400, map[string]string{"error": "multi-database not active"})
			return
		}

		var req struct {
			Name     string `json:"name"`
			URI      string `json:"uri"`
			Database string `json:"database"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid JSON"})
			return
		}
		if req.Name == "" || req.URI == "" || req.Database == "" {
			writeJSON(w, 400, map[string]string{"error": "name, uri, and database are required"})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()

		// Connect to the new database.
		var ms *store.MongoStore
		var ss store.Store
		if a.cfg.AtlasMode {
			atlas, err := store.NewAtlasStore(ctx, req.URI, req.Database)
			if err != nil {
				writeJSON(w, 500, map[string]string{"error": fmt.Sprintf("connection failed: %v", err)})
				return
			}
			ms = atlas.MongoStore
			ss = atlas
		} else {
			var err error
			ms, err = store.NewMongoStore(ctx, req.URI, req.Database)
			if err != nil {
				writeJSON(w, 500, map[string]string{"error": fmt.Sprintf("connection failed: %v", err)})
				return
			}
			ss = ms
		}

		entry := store.DatabaseEntry{
			Name:        req.Name,
			Database:    req.Database,
			Role:        config.RoleReadOnly,
			URI:         req.URI,
			Store:       ms,
			SearchStore: ss,
			Mongo:       ms,
		}

		if err := a.multi.AddEntry(entry); err != nil {
			ms.Close()
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}

		// Persist to config.
		a.persistDatabases()

		writeJSON(w, 200, map[string]string{"status": "ok", "message": fmt.Sprintf("Database %q added (read-only)", req.Name)})

	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

func (a *apiHandler) handleDatabaseByName(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	name := strings.TrimPrefix(r.URL.Path, "/api/databases/")
	if name == "" {
		writeJSON(w, 400, map[string]string{"error": "database name is required"})
		return
	}

	if a.multi == nil {
		writeJSON(w, 400, map[string]string{"error": "multi-database not active"})
		return
	}

	switch r.Method {
	case http.MethodPut:
		// Toggle enabled/disabled.
		var req struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid JSON"})
			return
		}

		if err := a.multi.SetEntryEnabled(name, req.Enabled); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}

		a.persistDatabases()

		state := "enabled"
		if !req.Enabled {
			state = "disabled"
		}
		writeJSON(w, 200, map[string]string{"status": "ok", "message": fmt.Sprintf("Database %q %s", name, state)})

	case http.MethodDelete:
		if err := a.multi.RemoveEntry(name); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}

		a.persistDatabases()

		writeJSON(w, 200, map[string]string{"status": "ok", "message": fmt.Sprintf("Database %q removed", name)})

	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

// handlePipelineConfig handles GET/POST /api/pipeline.
// GET returns the live pipeline and steward configuration.
// POST updates them: pipeline changes apply immediately; steward changes are
// saved to disk and take effect on next restart.
func (a *apiHandler) handlePipelineConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		pCfg := a.write.Config()
		// Return effective proto values (defaults if not customised).
		if len(pCfg.QualityProtos) == 0 {
			pCfg.QualityProtos = quality.DefaultQualityProtos
		}
		if len(pCfg.NoiseProtos) == 0 {
			pCfg.NoiseProtos = quality.DefaultNoiseProtos
		}
		writeJSON(w, 200, map[string]any{
			"pipeline": pCfg,
			"steward":  a.cfg.Steward,
		})

	case http.MethodPost:
		var raw struct {
			Pipeline json.RawMessage `json:"pipeline"`
			Steward  json.RawMessage `json:"steward"`
		}
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid JSON"})
			return
		}

		scorerReloaded := false
		var notes []string

		if len(raw.Pipeline) > 0 && string(raw.Pipeline) != "null" {
			newCfg := a.write.Config() // start from existing
			if err := json.Unmarshal(raw.Pipeline, &newCfg); err != nil {
				writeJSON(w, 400, map[string]string{"error": "invalid pipeline JSON: " + err.Error()})
				return
			}

			// Validate.
			if newCfg.DedupThreshold <= 0 || newCfg.DedupThreshold > 1 {
				writeJSON(w, 400, map[string]string{"error": "dedup_threshold must be in (0, 1]"})
				return
			}
			if newCfg.TopicBoundaryThreshold < 0 || newCfg.TopicBoundaryThreshold > 1 {
				writeJSON(w, 400, map[string]string{"error": "topic_boundary_threshold must be in [0, 1]"})
				return
			}
			if newCfg.ContentScoreGate < 0 || newCfg.ContentScoreGate > 1 {
				writeJSON(w, 400, map[string]string{"error": "content_score_gate must be in [0, 1]"})
				return
			}
			if newCfg.NoiseMinLen < 1 {
				writeJSON(w, 400, map[string]string{"error": "noise_min_len must be >= 1"})
				return
			}
			if newCfg.MaxGroupChars < 256 {
				writeJSON(w, 400, map[string]string{"error": "max_group_chars must be >= 256"})
				return
			}

			// Check if prototypes changed — reload scorer if so.
			existing := a.write.Config()
			existingQP := existing.QualityProtos
			if len(existingQP) == 0 {
				existingQP = quality.DefaultQualityProtos
			}
			existingNP := existing.NoiseProtos
			if len(existingNP) == 0 {
				existingNP = quality.DefaultNoiseProtos
			}
			newQP := newCfg.QualityProtos
			if len(newQP) == 0 {
				newQP = quality.DefaultQualityProtos
			}
			newNP := newCfg.NoiseProtos
			if len(newNP) == 0 {
				newNP = quality.DefaultNoiseProtos
			}

			protosChanged := !strSlicesEqual(existingQP, newQP) || !strSlicesEqual(existingNP, newNP)
			if protosChanged && a.embedder != nil {
				ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
				defer cancel()
				scorer, err := quality.NewContentScorerWithProtos(ctx, a.embedder, newQP, newNP)
				if err != nil {
					writeJSON(w, 500, map[string]string{"error": "scorer reload failed: " + err.Error()})
					return
				}
				a.write.UpdateScorer(scorer)
				scorerReloaded = true
			}

			a.write.UpdateConfig(newCfg)
			if err := config.SavePipelineConfig(newCfg); err != nil {
				notes = append(notes, "pipeline saved in memory but disk write failed: "+err.Error())
			}
		}

		if len(raw.Steward) > 0 && string(raw.Steward) != "null" {
			sCfg := a.cfg.Steward // start from existing
			if err := json.Unmarshal(raw.Steward, &sCfg); err != nil {
				writeJSON(w, 400, map[string]string{"error": "invalid steward JSON: " + err.Error()})
				return
			}
			a.cfg.Steward = sCfg
			if err := config.SaveStewardConfig(sCfg); err != nil {
				notes = append(notes, "steward disk write failed: "+err.Error())
			} else {
				notes = append(notes, "Steward settings saved — take effect on next restart")
			}
		}

		resp := map[string]any{"status": "ok", "scorer_reloaded": scorerReloaded}
		if len(notes) > 0 {
			resp["note"] = strings.Join(notes, "; ")
		}
		writeJSON(w, 200, resp)

	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

// handleRejections handles GET /api/rejections.
// Returns aggregate stats and a sample of recent rejected exchanges, useful for
// tuning the pre-filter and identifying new procedural patterns.
//
// Query params:
//   - n: sample size (default 20, max 200)
func (a *apiHandler) handleRejections(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}

	n := 20
	if s := r.URL.Query().Get("n"); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			n = v
			if n > 200 {
				n = 200
			}
		}
	}

	writeJSON(w, 200, map[string]any{
		"stats":  a.rejLog.Stats(),
		"sample": a.rejLog.Sample(n),
	})
}

// handleSettings handles GET/POST /api/settings.
// GET returns current configuration state (credentials are masked).
// POST updates credentials (keychain) and config (disk), then signals
// that a daemon restart is recommended.
func (a *apiHandler) handleSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		anthropicKey := config.GetAnthropicAPIKey()
		azureKey := config.GetAzureAPIKey()
		mongoURI, _ := credential.Get("mongodb_atlas_uri")
		// Fall back to the resolved URI from config (plain-text yaml, env, etc.)
		// so the dashboard reflects "configured" status regardless of where
		// the credential is stored.
		if mongoURI == "" {
			mongoURI = a.cfg.MongoDBAtlasURI
		}

		writeJSON(w, 200, map[string]any{
			"mode": a.cfg.Mode,
			"server": map[string]any{
				"port":                   a.cfg.Port,
				"mongodb_database":       a.cfg.MongoDBDatabase,
				"model_path":             a.cfg.ModelPath,
				"embedding_dim":          a.cfg.EmbeddingDim,
				"retrieval_top_k":        a.cfg.RetrievalTopK,
				"retrieval_max_tokens":   a.cfg.RetrievalMaxTokens,
				"upstream_anthropic_url": a.cfg.UpstreamAnthropicURL,
				"atlas_mode":             a.cfg.AtlasMode,
				"llm_synthesis":          a.cfg.LLMSynthesis,
				"synthesis_provider":     a.cfg.SynthesisProvider,
			},
			"mongodb": map[string]any{
				"configured": mongoURI != "",
				"uri_masked": maskCredential(mongoURI),
			},
			"anthropic": map[string]any{
				"configured": anthropicKey != "",
				"key_masked": maskCredential(anthropicKey),
			},
			"azure": map[string]any{
				"configured":  azureKey != "",
				"key_masked":  maskCredential(azureKey),
				"endpoint":    a.cfg.Azure.Endpoint,
				"deployment":  a.cfg.Azure.Deployment,
				"api_version": a.cfg.Azure.APIVersion,
			},
			"synthesis": a.synth != nil && a.synth.Available(),
		})

	case http.MethodPost:
		var req struct {
			Mode            *string `json:"mode,omitempty"`
			MongoURI        *string `json:"mongo_uri,omitempty"`
			AnthropicKey    *string `json:"anthropic_key,omitempty"`
			AzureKey        *string `json:"azure_key,omitempty"`
			AzureEndpoint   *string `json:"azure_endpoint,omitempty"`
			AzureDeployment *string `json:"azure_deployment,omitempty"`
			AzureAPIVersion *string `json:"azure_api_version,omitempty"`
			Server          *config.ServerSettings `json:"server,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid JSON"})
			return
		}

		var changes []string
		needRestart := false

		// Mode.
		if req.Mode != nil {
			if !config.ValidMode(*req.Mode) {
				writeJSON(w, 400, map[string]string{"error": "invalid mode: must be proxy, mcp, or mcp-readonly"})
				return
			}
			if err := config.SetMode(*req.Mode); err != nil {
				writeJSON(w, 500, map[string]string{"error": "failed to save mode: " + err.Error()})
				return
			}
			a.cfg.Mode = *req.Mode
			changes = append(changes, "mode")
			needRestart = true
		}

		// MongoDB URI.
		if req.MongoURI != nil {
			if *req.MongoURI == "" {
				_ = credential.Delete("mongodb_atlas_uri")
				changes = append(changes, "mongodb_uri removed")
			} else {
				if err := config.StoreCredential("mongodb_atlas_uri", *req.MongoURI); err != nil {
					writeJSON(w, 500, map[string]string{"error": "failed to save MongoDB URI: " + err.Error()})
					return
				}
				changes = append(changes, "mongodb_uri")
			}
			needRestart = true
		}

		// Anthropic API key.
		if req.AnthropicKey != nil {
			if *req.AnthropicKey == "" {
				_ = credential.Delete("anthropic_api_key")
				changes = append(changes, "anthropic_key removed")
			} else {
				if err := credential.Set("anthropic_api_key", *req.AnthropicKey); err != nil {
					writeJSON(w, 500, map[string]string{"error": "failed to save Anthropic key: " + err.Error()})
					return
				}
				changes = append(changes, "anthropic_key")
			}
			needRestart = true
		}

		// Azure OpenAI.
		azureChanged := false
		azureCfg := a.cfg.Azure
		if req.AzureEndpoint != nil {
			azureCfg.Endpoint = *req.AzureEndpoint
			azureChanged = true
		}
		if req.AzureDeployment != nil {
			azureCfg.Deployment = *req.AzureDeployment
			azureChanged = true
		}
		if req.AzureAPIVersion != nil {
			azureCfg.APIVersion = *req.AzureAPIVersion
			azureChanged = true
		}
		if req.AzureKey != nil {
			if *req.AzureKey == "" {
				_ = credential.Delete("azure_openai_api_key")
				changes = append(changes, "azure_key removed")
			} else {
				if err := credential.Set("azure_openai_api_key", *req.AzureKey); err != nil {
					writeJSON(w, 500, map[string]string{"error": "failed to save Azure key: " + err.Error()})
					return
				}
				changes = append(changes, "azure_key")
			}
			azureChanged = true
		}
		if azureChanged {
			if err := config.SaveAzureConfig(azureCfg); err != nil {
				writeJSON(w, 500, map[string]string{"error": "failed to save Azure config: " + err.Error()})
				return
			}
			a.cfg.Azure = azureCfg
			changes = append(changes, "azure")
			needRestart = true
		}

		// Server-level settings (port, mongodb_database, model_path,
		// embedding_dim, retrieval_top_k/max_tokens, upstream_anthropic_url,
		// atlas_mode, llm_synthesis). Always require a restart.
		if req.Server != nil {
			s := *req.Server
			if s.Port != nil && (*s.Port < 1 || *s.Port > 65535) {
				writeJSON(w, 400, map[string]string{"error": "port must be between 1 and 65535"})
				return
			}
			if s.EmbeddingDim != nil && *s.EmbeddingDim < 1 {
				writeJSON(w, 400, map[string]string{"error": "embedding_dim must be >= 1"})
				return
			}
			if s.RetrievalTopK != nil && *s.RetrievalTopK < 1 {
				writeJSON(w, 400, map[string]string{"error": "retrieval_top_k must be >= 1"})
				return
			}
			if s.RetrievalMaxTokens != nil && *s.RetrievalMaxTokens < 1 {
				writeJSON(w, 400, map[string]string{"error": "retrieval_max_tokens must be >= 1"})
				return
			}
			if s.SynthesisProvider != nil && !config.ValidSynthesisProvider(*s.SynthesisProvider) {
				writeJSON(w, 400, map[string]string{"error": "synthesis_provider must be one of: auto, anthropic, azure"})
				return
			}
			if err := config.SaveServerConfig(s); err != nil {
				writeJSON(w, 500, map[string]string{"error": "failed to save server settings: " + err.Error()})
				return
			}
			// Reflect into in-memory cfg for the GET endpoint until restart.
			if s.Port != nil {
				a.cfg.Port = *s.Port
			}
			if s.MongoDBDatabase != nil {
				a.cfg.MongoDBDatabase = *s.MongoDBDatabase
			}
			if s.ModelPath != nil {
				a.cfg.ModelPath = *s.ModelPath
			}
			if s.EmbeddingDim != nil {
				a.cfg.EmbeddingDim = *s.EmbeddingDim
			}
			if s.RetrievalTopK != nil {
				a.cfg.RetrievalTopK = *s.RetrievalTopK
			}
			if s.RetrievalMaxTokens != nil {
				a.cfg.RetrievalMaxTokens = *s.RetrievalMaxTokens
			}
			if s.UpstreamAnthropicURL != nil {
				a.cfg.UpstreamAnthropicURL = *s.UpstreamAnthropicURL
			}
			if s.AtlasMode != nil {
				a.cfg.AtlasMode = *s.AtlasMode
			}
			if s.LLMSynthesis != nil {
				a.cfg.LLMSynthesis = *s.LLMSynthesis
			}
			if s.SynthesisProvider != nil {
				a.cfg.SynthesisProvider = *s.SynthesisProvider
			}
			changes = append(changes, "server")
			needRestart = true
		}

		resp := map[string]any{
			"status":       "ok",
			"changes":      changes,
			"need_restart": needRestart,
		}
		if needRestart {
			resp["message"] = "Settings saved. Restart the daemon for changes to take effect."
		}
		writeJSON(w, 200, resp)

	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

// maskCredential returns a masked version of a credential for display.
func maskCredential(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 12 {
		return "••••••••"
	}
	return s[:4] + "••••" + s[len(s)-4:]
}

// strSlicesEqual returns true if two string slices have identical contents.
func strSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// handleExport handles GET /api/export — generates a markdown document from memories.
func (a *apiHandler) handleExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	source := r.URL.Query().Get("source")
	minQuality := 0.0
	if v := r.URL.Query().Get("min_quality"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			minQuality = f
		}
	}

	// Fetch memories.
	var memories []store.Memory
	var err error
	if source != "" {
		memories, err = a.store.ListBySource(ctx, source, 10000)
	} else {
		memories, err = a.store.List(ctx, "", 10000)
	}
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}

	// Quality filter.
	if minQuality > 0 {
		var filtered []store.Memory
		for _, m := range memories {
			if m.QualityScore >= minQuality {
				filtered = append(filtered, m)
			}
		}
		memories = filtered
	}

	if len(memories) == 0 {
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, 200, map[string]string{"error": "no memories match the filter"})
		return
	}

	// Group by source.
	groups := map[string][]store.Memory{}
	for _, m := range memories {
		src := m.Source
		if src == "" {
			src = "captured"
		}
		groups[src] = append(groups[src], m)
	}

	// Build markdown.
	var sb strings.Builder
	title := "memoryd Knowledge Export"
	if source != "" {
		title = "memoryd Export: " + source
	}
	sb.WriteString("# " + title + "\n\n")
	sb.WriteString(fmt.Sprintf("> %d memories across %d sources | exported %s\n\n",
		len(memories), len(groups), time.Now().Format("2006-01-02 15:04")))

	// Sort sources for stable output.
	var srcKeys []string
	for k := range groups {
		srcKeys = append(srcKeys, k)
	}
	sort.Strings(srcKeys)

	for _, src := range srcKeys {
		mems := groups[src]
		sb.WriteString("## " + src + "\n\n")
		for _, m := range mems {
			sb.WriteString("### " + exportFirstLine(m.Content) + "\n\n")
			sb.WriteString(m.Content + "\n\n")
			if m.QualityScore > 0 || m.HitCount > 0 {
				sb.WriteString(fmt.Sprintf("_quality: %.2f | hits: %d | created: %s_\n\n",
					m.QualityScore, m.HitCount, m.CreatedAt.Format("2006-01-02")))
			}
			sb.WriteString("---\n\n")
		}
	}

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=\"memoryd-export.md\"")
	w.WriteHeader(200)
	w.Write([]byte(sb.String()))
}

func exportFirstLine(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[:idx]
	}
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return s
}

// persistDatabases saves the current secondary database list to config.
func (a *apiHandler) persistDatabases() {
	if a.multi == nil {
		return
	}
	dbs := a.multi.DatabaseList()
	var cfgDBs []config.DatabaseConfig
	for _, db := range dbs {
		enabled := db.Enabled
		cfgDBs = append(cfgDBs, config.DatabaseConfig{
			Name:     db.Name,
			Database: db.Database,
			Role:     db.Role,
			URI:      db.URI,
			Enabled:  &enabled,
		})
	}
	if err := config.SaveDatabases(cfgDBs); err != nil {
		fmt.Printf("[api] warning: failed to persist database config: %v\n", err)
	}
}

// handleLogs returns the last N lines of ~/.memoryd/daemon.log.
// Query params:
//   - lines: number of lines to return (default 500, max 5000)
func handleLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}

	n := 500
	if s := r.URL.Query().Get("lines"); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			n = v
			if n > 5000 {
				n = 5000
			}
		}
	}

	logPath := filepath.Join(config.Dir(), "daemon.log")
	f, err := os.Open(logPath)
	if err != nil {
		writeJSON(w, 200, map[string]any{"lines": []string{}, "error": "log file not available"})
		return
	}
	defer f.Close()

	var all []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	for scanner.Scan() {
		all = append(all, scanner.Text())
	}

	// Return last n lines
	if len(all) > n {
		all = all[len(all)-n:]
	}

	writeJSON(w, 200, map[string]any{"lines": all, "total": len(all)})
}

// handlePrompts handles GET/POST/DELETE for prompt templates.
// GET returns the active prompts (custom overrides or defaults).
// POST saves custom prompt overrides.
// DELETE resets all prompts to built-in defaults.
func (a *apiHandler) handlePrompts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		active := synthesizer.DefaultPromptTemplates()
		if a.synth != nil {
			active = a.synth.PromptTemplates()
		}
		defaults := synthesizer.DefaultPromptTemplates()
		writeJSON(w, 200, map[string]any{
			"qa":           active["qa"],
			"merge":        active["merge"],
			"conversation": active["conversation"],
			"customized": map[string]bool{
				"qa":           active["qa"] != defaults["qa"],
				"merge":        active["merge"] != defaults["merge"],
				"conversation": active["conversation"] != defaults["conversation"],
			},
		})

	case http.MethodPost:
		var req struct {
			QA           *string `json:"qa"`
			Merge        *string `json:"merge"`
			Conversation *string `json:"conversation"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid JSON"})
			return
		}

		// Start from current custom prompts.
		qa, merge, conv := "", "", ""
		if a.synth != nil {
			qa, merge, conv = a.synth.CustomPrompts()
		}

		// Update only the fields that were sent.
		if req.QA != nil {
			qa = strings.TrimSpace(*req.QA)
		}
		if req.Merge != nil {
			merge = strings.TrimSpace(*req.Merge)
		}
		if req.Conversation != nil {
			conv = strings.TrimSpace(*req.Conversation)
		}

		// Apply to synthesizer.
		if a.synth != nil {
			a.synth.SetCustomPrompts(qa, merge, conv)
		}

		// Persist to disk.
		if err := config.SavePromptsConfig(config.PromptsConfig{
			QA: qa, Merge: merge, Conversation: conv,
		}); err != nil {
			writeJSON(w, 200, map[string]any{
				"status": "ok",
				"note":   "applied in memory but disk write failed: " + err.Error(),
			})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})

	case http.MethodDelete:
		if a.synth != nil {
			a.synth.SetCustomPrompts("", "", "")
		}
		if err := config.SavePromptsConfig(config.PromptsConfig{}); err != nil {
			writeJSON(w, 200, map[string]any{
				"status": "ok",
				"note":   "reset in memory but disk write failed: " + err.Error(),
			})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})

	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}
