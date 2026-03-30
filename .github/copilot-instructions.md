# memoryd — Copilot Instructions

> Persistent memory for coding agents. A local daemon that gives Claude Code (and other agents) long-term memory via transparent RAG.

## What This Is

memoryd is a Go daemon that sits between a coding agent and the Anthropic API. It intercepts every request, enriches it with relevant context from a MongoDB vector store, and stores useful information from responses — all transparently. The agent never knows it's there.

```
Developer → Claude Code → memoryd (127.0.0.1:7432) → Anthropic API
                              ↕
                        MongoDB (Atlas or Local)
```

Module: `github.com/memory-daemon/memoryd`
Go version: 1.26+
Config: `~/.memoryd/config.yaml`

---

## Architecture

### Package Map

```
cmd/memoryd/main.go         CLI entrypoint (cobra). Wires everything together.
internal/
  config/                   YAML config loading with defaults
  embedding/                Embedder interface + llama.cpp subprocess (voyage-4-nano, 1024 dims)
  pipeline/
    read.go                 Embed query → vector/hybrid search → format context
    write.go                Chunk → batch-embed → dedup → store (async, goroutine-safe)
    inject.go               Format retrieved memories as XML for system prompt
  proxy/
    proxy.go                HTTP server: /v1/messages, /health, /shutdown, dashboard, REST APIs
    anthropic.go            Full Anthropic proxy: sync + SSE streaming
    openai.go               Stub (501 Not Implemented)
  store/
    store.go                Interfaces: Store, QualityStore, SourceStore, HybridSearcher
    mongo.go                MongoDB implementation — works with Community and Atlas Local
    atlas.go                Atlas-proper implementation — hybrid search, RRF, MMR re-ranking
  chunker/                  Paragraph-boundary text splitting (~512 token chunks)
  redact/                   Security scrubbing (AWS keys, API tokens, passwords, PII)
  quality/                  Adaptive learning — tracks retrieval hits, content scoring, noise prototypes
  rejection/                Ring-buffer of rejected exchanges, adaptive noise learning
  steward/                  Background quality maintenance (scoring, pruning, merging)
  ingest/                   Source ingestion: crawl → chunk → batch-embed → store
  crawler/                  BFS web crawler with SHA256 change detection
  mcp/                      MCP stdio server — memory_search, memory_store, source_ingest, etc.
  export/                   Markdown export grouped by source
eval/                       Qualitative A/B evaluation framework
cmd/eval/                   CLI for running evals
cmd/stress/                 Stress test harness
cmd/memoryd-tray/           macOS menu bar app (systray)
scripts/
  create_index.js           Vector index for Atlas Local / Community
  create_atlas_indexes.js   Vector + text indexes for Atlas proper
  build-app.sh              macOS .app bundle builder
```

### Two-Tier Storage

memoryd has two storage tiers controlled by `atlas_mode` in config:

**Local mode** (`atlas_mode: false`, default):
- Works with MongoDB Community Edition or Atlas Local (Docker)
- Plain `$vectorSearch` — no pre-filtering, no text component
- Uses `MongoStore` for everything

**Atlas mode** (`atlas_mode: true`):
- Requires MongoDB Atlas proper (not Community, not Atlas Local)
- Hybrid search: pre-filtered `$vectorSearch` + Lucene `$search` + Reciprocal Rank Fusion
- MMR (Maximal Marginal Relevance) re-ranking for diversity
- Quality score pre-filtering at search time
- Uses `AtlasStore` (wraps `MongoStore`) for reads, `MongoStore` for writes

The read pipeline auto-detects via interface assertion:
```go
if hs, ok := rp.store.(store.HybridSearcher); ok {
    // Atlas path: hybrid search with RRF + MMR
} else {
    // Local path: plain vector search
}
```

### Data Flow

**Read path** (every prompt):
1. User message embedded locally via llama.cpp
2. Vector search (local) or hybrid search (Atlas) for top-k memories
3. Retrieved memories formatted as XML, injected into system prompt
4. Enriched prompt forwarded to Anthropic API

**Write path** (every response):
1. Response buffered from SSE stream
2. **Pre-Haiku gates** (before any LLM call):
   a. `QuickFilter` — pure string heuristic rejects procedural exchanges
   b. Length gate — responses < `ingest_min_len` (default 80 chars) skipped
   c. Content score gate — raw text embedded & scored against noise prototypes; below `content_score_pre_gate` (default 0.35) → skipped
3. LLM synthesis gate (`SynthesizeQA`) — Haiku distills or returns "SKIP"
4. Text chunked at paragraph boundaries (~512 tokens)
5. Noise filtered (< 20 chars, < 40% alphanumeric)
6. Secrets redacted before embedding
7. All chunks batch-embedded in single HTTP call to llama.cpp
8. Each chunk dedup-checked against store (cosine ≥ 0.92 = skip)
9. Similar-to-source chunks tagged as extensions (cosine ≥ 0.75)
10. Stored. All of this runs async in a goroutine — zero latency to Claude.

**Rejection store** (adaptive noise learning):
- Exchanges rejected by QuickFilter or synthesizer are logged to a ring buffer (500 entries)
- Every 25 rejections, assistant texts are re-embedded as noise prototypes
- Hot-swapped into the ContentScorer — the system learns what noise looks like
- Persisted as JSONL at `~/.memoryd/rejection_log.jsonl`

**Steward** (hourly background sweep):
1. Score memories: `log2(hit_count + 1) / log2(maxHits + 1) × 0.5^(timeSinceRetrieval / 7d)`
2. Prune: score < 0.1 AND age > 24h → delete
3. Merge: similarity > 0.88 → keep higher-hit-count version

---

## Key Interfaces

```go
// internal/store/store.go

type Store interface {
    VectorSearch(ctx, embedding []float32, topK int) ([]Memory, error)
    Insert(ctx, mem Memory) error
    Delete(ctx, id string) error
    List(ctx, query string, limit int) ([]Memory, error)
    DeleteAll(ctx) error
    CountBySource(ctx, source string) (int64, error)
    UpdateContent(ctx, id string, content string, embedding []float32) error
    ListBySource(ctx, sourcePrefix string, limit int) ([]Memory, error)
    Close() error
}

type HybridSearcher interface {
    HybridSearch(ctx, embedding []float32, topK int, opts SearchOptions) ([]Memory, error)
}

type QualityStore interface {
    RecordRetrievalBatch(ctx, events []RetrievalEvent) error
    GetRetrievalCount(ctx) (int64, error)
    IncrementHitCount(ctx, id primitive.ObjectID) error
    RecentRetrievals(ctx, limit int) ([]RetrievalLog, error)
    TopMemories(ctx, limit int) ([]Memory, error)
}

type SourceStore interface {
    InsertSource(ctx, src Source) (string, error)
    ListSources(ctx) ([]Source, error)
    DeleteSource(ctx, id string) error
    UpdateSourceStatus(ctx, id, status, err string, pageCount, memoryCount int) error
    GetSourcePage(ctx, sourceID, url string) (*SourcePage, error)
    UpsertSourcePage(ctx, page SourcePage) error
    DeleteSourcePages(ctx, sourceID) error
    DeleteMemoriesBySource(ctx, source string) error
}

// internal/embedding/embedding.go

type Embedder interface {
    Embed(ctx, text string) ([]float32, error)
    EmbedBatch(ctx, texts []string) ([][]float32, error)
    Dim() int
    Close() error
}
```

`MongoStore` implements `Store`, `QualityStore`, `SourceStore`, and `StewardStore`.
`AtlasStore` embeds `*MongoStore` and adds `HybridSearcher`.

---

## Critical Thresholds

| Constant | Value | Location | Purpose |
|----------|-------|----------|---------|
| `DedupThreshold` | 0.92 | pipeline/write.go | Cosine similarity above which a chunk is skipped as duplicate |
| `SourceExtensionThreshold` | 0.75 | pipeline/write.go, ingest.go | Tag chunk as "extends source" instead of standalone |
| `minContentLen` | 20 | pipeline/write.go | Minimum chars to store |
| `NoiseRatio` | 0.40 | pipeline/write.go | Below 40% alphanumeric = noise |
| `DefaultMaxTokens` | 512 | chunker/ | Target chunk size in tokens (~2048 chars) |
| `RetrievalTopK` | 5 | config default | Memories per search |
| `RetrievalMaxTokens` | 2048 | config default | Context budget for injection |
| `QualityLearningThreshold` | 50 | quality/ | Retrievals before quality filtering activates |
| `IngestMinLen` | 80 | config/PipelineConfig | Responses shorter than this skip Haiku entirely |
| `ContentScorePreGate` | 0.35 | config/PipelineConfig | Pre-Haiku noise gate: below this → skip |
| `noiseTopK` | 3 | quality/content.go | Top-K noise prototypes used in scoring (prevents dilution) |
| `maxRejectionProtos` | 150 | quality/content.go | Max rejection texts used as noise prototypes |
| `RebuildEvery` | 25 | rejection/store.go | Rejections between scorer rebuilds |
| `DefaultMaxSize` | 500 | rejection/store.go | Ring buffer capacity |
| `PruneThreshold` | 0.1 | steward config | Score below which memories get pruned |
| `PruneGracePeriod` | 24h | steward config | Minimum age before pruning eligible |
| `DecayHalfLife` | 90d | steward config | Unretrieved memory score half-life |
| `MergeThreshold` | 0.88 | steward config | Similarity threshold for merge candidates |
| `BatchSize` | 500 | steward config | Max memories scanned per steward sweep |

---

## Config Reference

File: `~/.memoryd/config.yaml` (created on first `memoryd start`)

```yaml
port: 7432
mongodb_atlas_uri: ""                # required — connection string
mongodb_database: memoryd
model_path: ~/.memoryd/models/voyage-4-nano.gguf
embedding_dim: 1024                  # voyage-4-nano = 1024 dimensions
retrieval_top_k: 5
retrieval_max_tokens: 2048
upstream_anthropic_url: https://api.anthropic.com
atlas_mode: false                    # true = Atlas proper with hybrid search

steward:
  interval_minutes: 60
  prune_threshold: 0.1
  grace_period_hours: 24
  decay_half_days: 90
  merge_threshold: 0.88
  batch_size: 500

pipeline:
  ingest_min_len: 80              # responses < this skip Haiku entirely
  content_score_pre_gate: 0.35    # pre-Haiku noise gate threshold
```

---

## CLI Commands

```
memoryd start      Start daemon (foreground). Creates config on first run.
memoryd mcp        Start as MCP stdio server (for Claude Code MCP integration)
memoryd status     Ping health endpoint
memoryd search     Regex search on memory content
memoryd forget     Delete one memory by hex ID
memoryd wipe       Delete all memories (confirmation required)
memoryd env        Print ANTHROPIC_BASE_URL export
memoryd version    Print version
memoryd ingest     Crawl a URL and store as source
memoryd sources    List ingested sources
memoryd export     Export memories to markdown
```

---

## Development Setup

### Prerequisites
- Go 1.26+
- llama.cpp (`brew install llama.cpp`)
- Docker (for Atlas Local in dev)
- voyage-4-nano GGUF model (~70MB, auto-downloaded on first run)

### Local MongoDB (development)
```bash
docker run -d --name memoryd-mongo -p 27017:27017 mongodb/mongodb-atlas-local:8.0
docker cp scripts/create_index.js memoryd-mongo:/tmp/create_index.js
docker exec memoryd-mongo mongosh memoryd --quiet --file /tmp/create_index.js
```

Config for local: `mongodb_atlas_uri: "mongodb://localhost:27017/?directConnection=true"`

### Atlas Proper Setup
Run `scripts/create_atlas_indexes.js` against your Atlas cluster. This creates:
- `vector_index`: vectorSearch with filter fields (quality_score, source)
- `text_index`: Lucene search on content field
Set `atlas_mode: true` in config.

### Build & Test
```bash
make build              # → bin/memoryd
go test ./...           # all unit tests (no external deps needed)
go vet ./...            # static analysis
```

### Running
```bash
memoryd start                                    # start daemon
export ANTHROPIC_BASE_URL=http://127.0.0.1:7432  # point Claude Code at it
```

---

## Conventions & Patterns

### Code Style
- Standard Go: `gofmt`, `go vet`, no external linters configured
- Interfaces defined in the package that uses them (Store in store/, Embedder in embedding/)
- Functional options pattern for configuration (e.g., `proxy.WithStore()`, `pipeline.WithQualityTracker()`)
- Errors logged at the boundary, not propagated through async paths

### Testing
- Unit tests use in-memory mocks (`mockEmbedder`, `mockStore` in pipeline_test.go)
- No integration tests that require MongoDB running (those go in cmd/stress)
- Mock embedders must implement all interface methods including `EmbedBatch`
- Test files live next to the code they test

### Async Pattern
- Write pipeline runs in goroutines — errors are logged, never returned to caller
- Steward runs in a background goroutine with context cancellation
- Quality tracking (`RecordHits`) fires as a goroutine from the read pipeline

### Interface Detection
- Runtime type assertion detects capabilities: `store.(store.HybridSearcher)`
- This pattern lets the same read pipeline work with both local and Atlas stores
- No build tags, no separate binaries — single binary, runtime config

### Security
- `redact.Clean()` strips secrets BEFORE embedding — secrets never enter the vector store
- Covers: AWS keys, API tokens, MongoDB URIs, private keys, passwords, JWTs
- Config file written with 0600 permissions
- Daemon binds to 127.0.0.1 only — not exposed to network

### Embedding
- All embeddings go through the `Embedder` interface
- `EmbedBatch` sends array of texts in single HTTP call to llama-server
- llama-server runs as a subprocess managed by the daemon, port 7433
- Model: voyage-4-nano Q8_0 GGUF, 1024 dimensions, cosine similarity

---

## Gotchas

1. **Embedding dim is 1024, not 512.** The README and some old docs say 512 — that's wrong. voyage-4-nano produces 1024-dim vectors. The vector index must match.

2. **Atlas Local doesn't support `$search` or `$vectorSearch` filters.** Those are Atlas-proper features. Don't try to use Lucene text search or filter clauses in `$vectorSearch` against Atlas Local or Community.

3. **The steward needs `StewardStore` methods on `MongoStore`.** If you add a new store backend, it needs `ListOldest`, `UpdateQualityScore`, `DeleteMemory`, `FindSimilar` for the steward to work.

4. **New memories have quality_score 0.** The quality pre-filter in AtlasStore uses `$or: [{quality_score >= threshold}, {quality_score == 0}]` to avoid filtering out unscored memories.

5. **Write pipeline dedup is sequential per chunk.** Even though embedding is batched, each chunk's dedup check runs against the store one at a time (because earlier inserts affect later lookups).

6. **`MongoStore` implements 4 interfaces.** Store, QualityStore, SourceStore, and StewardStore are all on the same type. The daemon passes `st` (MongoStore) to components that need direct access and `searchStore` (Store/HybridSearcher) to the read pipeline.

7. **SSE streaming.** The proxy handles Server-Sent Events end-to-end. It buffers the full response for the write path while streaming to the client. Don't break the streaming path for write-path changes.

8. **Config path expansion.** `~` in `model_path` is expanded by the config loader. Don't use `os.ExpandEnv` — use the config package's path handling.

9. **Source naming.** Ingested sources are stored with `source:NAME|URL` format. The pipe separator is significant for page-level change detection.

10. **Graceful shutdown.** The daemon catches SIGINT/SIGTERM, stops the steward, stops the HTTP server, then cancels context. The order matters.

11. **Content score pre-gate does NOT feed rejection store.** Exchanges filtered by the content score gate are NOT added to the rejection store — only QuickFilter and synthesizer rejections feed back. This prevents a positive feedback loop where the scorer would amplify its own noise signal.

12. **Top-K noise scoring, not averaging.** The ContentScorer uses the top-3 most similar noise prototypes, not the average of all. When the rejection store grows to 150+ entries, averaging would converge to a constant, destroying discriminative power.

---

## Memory Data Model

MongoDB collection: `memories`

```
_id:            ObjectId
content:        string          # the text chunk
embedding:      []float32       # 1024-dim vector
source:         string          # "claude-code", "source:docs|https://...", "mcp", etc.
created_at:     time.Time
metadata:       map[string]any  # extends_source, extends_memory, page_url, etc.
hit_count:      int             # times retrieved in search results
quality_score:  float64         # steward-computed score (0–1)
last_retrieved: time.Time       # last time this appeared in search results
score:          float64         # vector search similarity score (transient, not stored)
```

Additional collections: `retrieval_events`, `sources`, `source_pages`

---

## Adding New Features

### New store capability
1. Add method to relevant interface in `store/store.go`
2. Implement on `MongoStore` in `store/mongo.go`
3. If Atlas-specific, add to `AtlasStore` in `store/atlas.go`
4. If it needs runtime detection, use interface assertion pattern in caller

### New CLI command
1. Add `fooCmd()` function in `cmd/memoryd/main.go`
2. Register with `root.AddCommand(fooCmd())`
3. Follow cobra pattern — `Use`, `Short`, `RunE`

### New MCP tool
1. Add handler in `internal/mcp/server.go`
2. Register in the tool list with name, description, input schema
3. Follow existing patterns (memory_search, memory_store, etc.)

### New pipeline stage
- Read path changes go in `pipeline/read.go`
- Write path changes go in `pipeline/write.go`
- Context formatting in `pipeline/inject.go`
- Keep write path async — never block the response to Claude
- Pre-Haiku gate changes go in `proxy/anthropic.go` and `proxy/api.go`
- Rejection store logic is in `rejection/store.go`
- Content scoring prototypes and noise learning are in `quality/content.go`
