<div align="center">

# memoryd

**Persistent memory for coding agents — institutional knowledge that builds itself.**

[![CI](https://github.com/jeff-vincent/memoryd/actions/workflows/ci.yml/badge.svg)](https://github.com/jeff-vincent/memoryd/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/jeff-vincent/memoryd)](https://goreportcard.com/report/github.com/jeff-vincent/memoryd)
[![Go Version](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Docs](https://img.shields.io/badge/docs-memoryd-orange)](https://jeff-vincent.github.io/memoryd/)

</div>

---

`memoryd` is a local daemon that gives coding agents long-term memory. It sits between your agent and the Anthropic API as a transparent proxy — capturing knowledge from every session, curating it automatically, and injecting relevant context into future conversations. No workflow changes, no special prompts. Just set one environment variable and your agent gets smarter over time.

It also exposes an **MCP server** that any agent (Cursor, Windsurf, custom pipelines) can use to search, store, and manage memories — making memoryd a **shared knowledge hub** for your entire team.

```
Developer → Agent → memoryd (localhost:7432) → Anthropic API
                        ↕
                   MongoDB Atlas ← shared across team
```

**[Read the full documentation →](https://kindling-sh.github.io/memoryd/)**

## Quick Start

```bash
# Install
git clone https://github.com/kindling-sh/memoryd.git && cd memoryd && make build

# Configure (only mongodb_atlas_uri is required)
./bin/memoryd start    # creates ~/.memoryd/config.yaml, then edit it

# Run
./bin/memoryd start
export ANTHROPIC_BASE_URL=http://127.0.0.1:7432
```

The embedding model (~70 MB) downloads automatically on first launch. Work normally with Claude Code — memory builds in the background.

For MCP integration with any agent:

```json
{
  "mcpServers": {
    "memoryd": { "command": "memoryd", "args": ["mcp"] }
  }
}
```

See the **[Getting Started](https://kindling-sh.github.io/memoryd/getting-started)** guide for full setup including MongoDB and Atlas configuration.

## How It Works

### [Read Path](https://kindling-sh.github.io/memoryd/how-it-works/read-path)

Every prompt is enriched with relevant context from the memory store. The query is embedded locally ([voyage-4-nano](https://huggingface.co/jsonMartin/voyage-4-nano-gguf), 1024 dims via llama.cpp), searched against MongoDB using vector or hybrid search (RRF + MMR), and injected into the system prompt — invisibly.

### [Write Path](https://kindling-sh.github.io/memoryd/how-it-works/write-path)

Every response is captured asynchronously with zero added latency. Text is chunked at paragraph boundaries, noise-filtered, secret-redacted (13 regex patterns covering AWS keys, GitHub tokens, JWTs, private keys, connection strings, and more), batch-embedded, and deduplicated (cosine ≥ 0.92 = skip).

### [Quality Loop](https://kindling-sh.github.io/memoryd/how-it-works/quality-loop)

A background steward scores every memory by retrieval frequency and recency decay, prunes zero-value memories after a grace period, and merges near-duplicates (cosine ≥ 0.88). The store self-maintains — no manual curation needed.

### [Hybrid Search](https://kindling-sh.github.io/memoryd/how-it-works/hybrid-search)

When connected to Atlas, memoryd upgrades to a hybrid pipeline: vector search + full-text Lucene, fused with Reciprocal Rank Fusion (k=60), diversified with Maximal Marginal Relevance (λ=0.7). Exact keyword matches and semantic similarity work together.

### [MCP Server](https://kindling-sh.github.io/memoryd/agents/mcp-server)

8 tools (`memory_search`, `memory_store`, `source_ingest`, `quality_stats`, etc.) exposed over stdio JSON-RPC. Any MCP-compatible agent can read from and write to the store. The store is the product, not the agent.

### [Proxy Mode](https://kindling-sh.github.io/memoryd/agents/proxy-mode)

Transparent HTTP proxy on port 7432. Handles both sync and SSE streaming. Captures both sides of every conversation. Zero configuration beyond `ANTHROPIC_BASE_URL`.

### [Read-Only Mode](https://kindling-sh.github.io/memoryd/agents/read-only-mode)

Agents can consume institutional knowledge without contributing — connect via MCP, use `memory_search` only. Ideal for evaluation, security-sensitive contexts, or non-Anthropic agents.

### [Team Knowledge Hub](https://kindling-sh.github.io/memoryd/team-knowledge-hub)

Point every team member at a shared Atlas cluster. Each person works normally. Their sessions populate the store. The steward curates across all contributions. Knowledge accumulates organically — no one writes docs, no one maintains a wiki. The knowledge web builds itself.

## Architecture

```
cmd/memoryd/              CLI (cobra commands)
internal/
  config/                 YAML config with defaults
  chunker/                Paragraph-boundary text chunking (~512 token windows)
  embedding/              Local embeddings via llama.cpp (voyage-4-nano, 1024-dim)
  pipeline/
    read.go               Embed → search → format context → inject
    write.go              Chunk → filter → redact → batch embed → dedup → store
    inject.go             XML context formatting with token budget
  proxy/
    proxy.go              HTTP server, REST API, dashboard
    anthropic.go          Anthropic proxy with sync + SSE streaming
  store/
    store.go              Interfaces: Store, QualityStore, SourceStore, HybridSearcher
    mongo.go              MongoDB standalone / Atlas Local
    atlas.go              Atlas hybrid search: vector + Lucene + RRF + MMR
  redact/                 13 secret-scrubbing patterns
  quality/                Retrieval tracking (50-event learning threshold)
  steward/                Background sweep: score → prune → merge
  ingest/                 Web source ingestion + change detection
  crawler/                BFS crawler with SHA256 dedup
  mcp/                    MCP stdio server (8 tools)
  export/                 Markdown export grouped by source
website/                  Docusaurus documentation site
```

## CLI

```
memoryd start                    Start the daemon
memoryd mcp                      Start as MCP stdio server
memoryd status                   Check daemon health
memoryd search "query"           Search memories (regex)
memoryd forget <id>              Delete a memory
memoryd wipe                     Clear the entire store
memoryd env                      Print shell export command
memoryd ingest <name> <url>      Crawl a URL into the store
memoryd sources                  List ingested sources
memoryd export                   Export memories to markdown
memoryd version                  Print version
```

## Configuration

Only `mongodb_atlas_uri` is required. Everything else has sensible defaults.

```yaml
mongodb_atlas_uri: "mongodb+srv://..."   # Required
port: 7432                                # Proxy port
atlas_mode: false                         # true for hybrid search
retrieval_top_k: 5                        # Memories per query
retrieval_max_tokens: 2048                # Context budget

steward:
  interval_minutes: 60
  prune_threshold: 0.1
  merge_threshold: 0.88
  decay_half_days: 7
```

See the full **[Configuration Reference](https://kindling-sh.github.io/memoryd/configuration)** for all options and tuning guidance.

## Development

```bash
# Local MongoDB via Docker
docker run -d --name memoryd-mongo -p 27017:27017 mongodb/mongodb-atlas-local:8.0
docker cp scripts/create_index.js memoryd-mongo:/tmp/create_index.js
docker exec memoryd-mongo mongosh memoryd --quiet --file /tmp/create_index.js

# Build & test
make build
go test -race ./...

# Docs site
cd website && npm start
```

## Roadmap

- [x] Transparent Anthropic proxy with SSE streaming
- [x] Local embeddings (voyage-4-nano via llama.cpp)
- [x] MCP server (8 tools, any agent)
- [x] Source ingestion (crawl docs/wikis)
- [x] Quality steward (scoring, pruning, merging)
- [x] Atlas hybrid search (vector + text + RRF + MMR)
- [x] Batch embedding
- [x] Security redaction (13 secret patterns)
- [x] Documentation site
- [ ] OpenAI-compatible endpoint support
- [ ] macOS menu bar app
- [ ] Multi-provider store population (beyond Claude Code proxy)
- [ ] Hosted team knowledge hub

## License

[MIT](LICENSE)
