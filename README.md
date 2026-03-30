<div align="center">

# memoryd

**Shared knowledge for engineering teams — built automatically from the work you already do.**

[![CI](https://github.com/memory-daemon/memoryd/actions/workflows/ci.yml/badge.svg)](https://github.com/memory-daemon/memoryd/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/memory-daemon/memoryd)](https://goreportcard.com/report/github.com/memory-daemon/memoryd)
[![Go Version](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Docs](https://img.shields.io/badge/docs-memoryd-orange)](https://memory-daemon.github.io/memoryd/)

</div>

---

`memoryd` is a shared knowledge layer for engineering teams. It connects to your team's AI coding tools (Claude Code, Cursor, Windsurf, etc.) and transparently captures institutional knowledge from every session — architecture decisions, debugging insights, deployment procedures, codebase conventions. All of it flows into a shared MongoDB Atlas database that every team member's tools can draw from.

No one writes documentation. No one maintains a wiki. **Knowledge builds itself from the work your team is already doing.**

```
Your Team's AI Tools → memoryd → LLM Provider
                          ↕
                    MongoDB Atlas (shared)
```

**[Read the full documentation →](https://memory-daemon.github.io/memoryd/)**

## Quick Start

```bash
# Install
curl -fsSL https://raw.githubusercontent.com/memory-daemon/memoryd/main/install.sh | bash

# Configure — point at your team's shared Atlas cluster
# Edit ~/.memoryd/config.yaml:
#   mongodb_atlas_uri: "mongodb+srv://team:pass@cluster0.mongodb.net/..."
#   atlas_mode: true

# Run
memoryd start
export ANTHROPIC_BASE_URL=http://127.0.0.1:7432
```

Work normally with Claude Code — knowledge builds in the background and is available to the whole team.

For Cursor, Windsurf, or any MCP-compatible tool:

```json
{
  "mcpServers": {
    "memoryd": { "command": "memoryd", "args": ["mcp"] }
  }
}
```

See the **[Getting Started](https://memory-daemon.github.io/memoryd/getting-started)** guide for full team setup.

## Why memoryd?

| Challenge | How memoryd helps |
|---|---|
| **Knowledge walks out the door** | Institutional knowledge is captured automatically and persisted in a shared store |
| **Stale wikis and docs** | Knowledge stays current because it's built from current work |
| **Slow onboarding** | New hires' AI tools inherit months of team context from day one |
| **Repeated debugging** | One person's fix becomes everyone's knowledge |
| **Tool fragmentation** | Works with Claude Code, Cursor, Windsurf, Cline — same knowledge store |

## How It Works

### [Knowledge Capture](https://memory-daemon.github.io/memoryd/how-it-works/write-path)

Every AI response is captured asynchronously (zero latency impact), passed through a multi-stage quality filter (length gate, adaptive content scoring, LLM synthesis gate), scrubbed of secrets (API keys, tokens, passwords — 13 detection patterns), deduplicated, and stored in the shared database. The system learns what noise looks like from rejected exchanges, improving filtering accuracy over time.

### [Context Retrieval](https://memory-daemon.github.io/memoryd/how-it-works/read-path)

Every prompt is enriched with relevant knowledge from the shared store. The AI tool sees prior context — from its own sessions and teammates' — without anyone asking for it.

### [Quality Maintenance](https://memory-daemon.github.io/memoryd/how-it-works/quality-loop)

A background process scores knowledge by usefulness and recency, prunes noise, and merges near-duplicates across the whole team's contributions. The store self-maintains.

### [Hybrid Search](https://memory-daemon.github.io/memoryd/how-it-works/hybrid-search)

On Atlas, retrieval combines meaning-based and keyword-based search with diversity optimization. Finds both conceptually related and exact-match results.

### [Tool Integration](https://memory-daemon.github.io/memoryd/agents/mcp-server)

Connects via transparent proxy (Claude Code) or MCP server (Cursor, Windsurf, any MCP tool). Different team members can use different tools — they all share the same knowledge.

### [Team Knowledge Hub](https://memory-daemon.github.io/memoryd/team-knowledge-hub)

Point your team at a shared Atlas cluster. Each person works normally. Knowledge accumulates organically from daily work. Coming soon: team-scoped and BU-scoped knowledge layers that overlap naturally.

## Architecture

```
cmd/memoryd/              CLI (cobra commands)
internal/
  config/                 YAML config with defaults
  chunker/                Text chunking at natural boundaries
  embedding/              Local embeddings (voyage-4-nano, 1024-dim)
  pipeline/
    read.go               Search → format context → inject
    write.go              Chunk → filter → scrub secrets → dedup → store
    inject.go             Context formatting with token budget
  proxy/
    proxy.go              HTTP server, REST API, dashboard
    anthropic.go          Anthropic proxy with streaming support
  store/
    store.go              Store interfaces
    mongo.go              MongoDB implementation
    atlas.go              Atlas hybrid search (vector + text + RRF + MMR)
  redact/                 Secret scrubbing (13 patterns)
  quality/                Usage tracking, content scoring, adaptive noise learning
  rejection/              Rejection store — ring buffer for adaptive noise prototype learning
  steward/                Background maintenance (score → prune → merge)
  ingest/                 Source ingestion and change detection
  crawler/                Web crawler with change detection
  mcp/                    MCP stdio server (8 tools)
  export/                 Markdown export
website/                  Documentation site
```

## CLI

```
memoryd start                    Start the daemon
memoryd mcp                      Start as MCP server
memoryd status                   Check daemon health
memoryd search "query"           Search the knowledge base
memoryd ingest <name> <url>      Ingest a docs site or wiki
memoryd sources                  List ingested sources
memoryd export                   Export knowledge to markdown
memoryd forget <id>              Delete a specific item
memoryd wipe                     Clear the entire store
memoryd version                  Print version
```

## Configuration

Only `mongodb_atlas_uri` is required. Everything else has sensible defaults.

```yaml
mongodb_atlas_uri: "mongodb+srv://..."   # Required — team's shared cluster
atlas_mode: true                          # Enable hybrid search (recommended)
port: 7432                                # Local proxy port
retrieval_top_k: 5                        # Knowledge items per query
retrieval_max_tokens: 2048                # Context budget

steward:
  interval_minutes: 60
  prune_threshold: 0.1
  merge_threshold: 0.88
  decay_half_days: 90

pipeline:
  ingest_min_len: 80                  # Skip short responses before LLM call
  content_score_pre_gate: 0.35        # Adaptive noise score threshold
```

See the full **[Configuration Reference](https://memory-daemon.github.io/memoryd/configuration)**.

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

- [x] Transparent Anthropic proxy with streaming
- [x] Local embeddings (voyage-4-nano via llama.cpp)
- [x] MCP server (8 tools, any agent)
- [x] Source ingestion (crawl docs/wikis)
- [x] Quality maintenance (scoring, pruning, merging)
- [x] Atlas hybrid search (vector + text + RRF + MMR)
- [x] Secret scrubbing (13 detection patterns)
- [x] Adaptive noise filtering (pre-Haiku gates, rejection-based learning)
- [x] Documentation site
- [x] macOS menu bar app
- [ ] Team-scoped knowledge (overlapping layers per team/BU)
- [ ] OpenAI-compatible endpoint support
- [ ] Multi-provider support (beyond Anthropic)
- [ ] Hosted team knowledge hub

## License

[MIT](LICENSE)
