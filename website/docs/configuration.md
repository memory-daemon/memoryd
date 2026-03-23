---
sidebar_position: 6
title: Configuration
---

# Configuration

memoryd reads configuration from `~/.memoryd/config.yaml` on each team member's machine. Only the MongoDB connection string is required — everything else has sensible defaults.

## Minimal setup (what most team members need)

```yaml
mongodb_atlas_uri: "mongodb+srv://team-user:password@cluster0.mongodb.net/?retryWrites=true"
atlas_mode: true
```

That's it for most people. The connection string comes from whoever set up the shared cluster. `atlas_mode: true` turns on the full feature set.

## Full reference

```yaml
# Required — shared MongoDB Atlas connection string
mongodb_atlas_uri: "mongodb+srv://team:pass@cluster0.mongodb.net/?retryWrites=true"

# Local proxy port (default: 7432)
port: 7432

# MongoDB database name (default: "memoryd")
mongodb_database: "memoryd"

# Path to the local embedding model (auto-downloaded on first run)
model_path: "~/.memoryd/models/voyage-4-nano.gguf"

# Embedding dimensions — must match the Atlas vector index (default: 1024)
embedding_dim: 1024

# Number of knowledge items retrieved per query (default: 5)
retrieval_top_k: 5

# Maximum tokens of context returned per search (default: 2048)
retrieval_max_tokens: 2048

# Upstream LLM provider URL (default: https://api.anthropic.com)
upstream_anthropic_url: "https://api.anthropic.com"

# Enable Atlas hybrid search (default: false)
# Set to true when using a shared Atlas cluster
atlas_mode: false

# Quality maintenance settings
steward:
  interval_minutes: 60       # How often the quality sweep runs
  prune_threshold: 0.1       # Score below which low-value items are removed
  grace_period_hours: 24     # Minimum age before removal is considered
  decay_half_days: 7         # How quickly unused knowledge loses relevance
  merge_threshold: 0.88      # Similarity threshold for deduplication
  batch_size: 500            # Items processed per sweep
```

## Atlas mode

When `atlas_mode: true`, memoryd enables:

- **Hybrid search** — combines meaning-based and keyword-based retrieval for more accurate results
- **Quality pre-filtering** — search results are filtered by quality score at the database level
- **Source-scoped search** — filter results by knowledge source

This is the recommended setting for any team deployment using a shared Atlas cluster.

## Tuning for your team

The defaults work well for most teams. If you need to adjust:

| Scenario | What to change |
|---|---|
| **Large team (10+ contributors)** | Lower `interval_minutes` to 30, increase `batch_size` to 1000 |
| **Want to retain knowledge longer** | Increase `decay_half_days` to 14–30 |
| **Seeing too many near-duplicates** | Lower `merge_threshold` to 0.85 |
| **Want less aggressive cleanup** | Lower `prune_threshold` to 0.05, increase `grace_period_hours` to 72 |
| **Heavy use of ingested sources** | Increase `batch_size` to 2000, lower `interval_minutes` to 30 |

## Environment variables

The only environment variable relevant to memoryd is the one that connects your AI tool to the proxy:

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:7432
```

This tells Claude Code (or any Anthropic-compatible tool) to route through memoryd. memoryd itself reads everything from `config.yaml`.
