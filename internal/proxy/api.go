package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/kindling-sh/memoryd/internal/embedding"
	"github.com/kindling-sh/memoryd/internal/pipeline"
	"github.com/kindling-sh/memoryd/internal/redact"
	"github.com/kindling-sh/memoryd/internal/store"
)

type apiHandler struct {
	store    store.Store
	multi    *store.MultiStore // non-nil when multi-database is active
	read     *pipeline.ReadPipeline
	write    *pipeline.WritePipeline
	embedder embedding.Embedder
}

func registerAPI(mux *http.ServeMux, st store.Store, read *pipeline.ReadPipeline, write *pipeline.WritePipeline, emb embedding.Embedder) {
	h := &apiHandler{store: st, read: read, write: write, embedder: emb}
	if ms, ok := st.(*store.MultiStore); ok {
		h.multi = ms
	}
	mux.HandleFunc("/api/search", h.handleSearch)
	mux.HandleFunc("/api/store", h.handleStore)
	mux.HandleFunc("/api/memories", h.handleMemories)
	mux.HandleFunc("/api/memories/", h.handleMemoryByID)
	mux.HandleFunc("/api/databases", h.handleDatabases)
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

	retrieved, err := a.read.Retrieve(ctx, req.Query)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, 200, map[string]string{"context": retrieved})
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
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	if a.multi == nil {
		writeJSON(w, 200, []any{})
		return
	}
	writeJSON(w, 200, a.multi.DatabaseList())
}
