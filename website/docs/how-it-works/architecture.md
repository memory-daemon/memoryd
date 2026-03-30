---
sidebar_position: 5
title: Architecture & Design Decisions
---

# Architecture & Design Decisions

This page is for the technically curious — the design choices behind memoryd, the specific tools and thresholds selected, and the reasoning behind each.

## System design

memoryd runs as a local daemon on each team member's machine. It serves two roles simultaneously:

1. **HTTP proxy** — a passthrough proxy between the AI tool and the LLM provider. Forwards requests unmodified and captures responses asynchronously for knowledge storage.
2. **MCP server** — exposes knowledge operations (search, store, delete, ingest) over the Model Context Protocol. This is how AI tools access the shared knowledge base.

The proxy handles **capture** (write path only). The MCP server handles **retrieval and explicit operations** (read path + write path). Both share the same store and quality system. The daemon binds to `127.0.0.1` only — it's never exposed to the network.

```
┌─────────────────────────────────────────────┐
│              memoryd daemon                  │
│                                              │
│  ┌──────────┐  ┌───────────┐  ┌──────────┐ │
│  │  Proxy   │  │ MCP Server│  │ Dashboard│ │
│  │ (HTTP)   │  │  (stdio)  │  │  (HTTP)  │ │
│  └────┬─────┘  └─────┬─────┘  └──────────┘ │
│       │               │                      │
│  ┌────▼───────────────▼─────┐               │
│  │    Read Pipeline (MCP)    │               │
│  │  embed → search → return  │               │
│  └──────────────────────────┘               │
│  ┌──────────────────────────┐               │
│  │  Write Pipeline (async)   │               │
│  │  chunk → filter → redact  │               │
│  │  → embed → dedup → store  │               │
│  └──────────────────────────┘               │
│  ┌──────────────────────────┐               │
│  │    Steward (background)   │               │
│  │  score → prune → merge    │               │
│  └──────────────────────────┘               │
│  ┌──────────────────────────┐               │
│  │    Embedder (llama.cpp)   │               │
│  │  voyage-4-nano, port 7433 │               │
│  └──────────────────────────┘               │
└──────────────────┬──────────────────────────┘
                   │
            MongoDB Atlas (shared)
```

### Why a local daemon?

The daemon runs on each machine rather than as a centralized service because:

- **Latency** — embedding happens locally. No round-trip to a remote embedding service on every request.
- **Privacy** — prompts and responses never leave the machine for embedding. Only the resulting knowledge (post-redaction) enters the shared store.
- **Simplicity** — no infrastructure to manage beyond the MongoDB cluster your team already knows how to operate.
- **Resilience** — if the daemon is down, the AI tool falls through to the LLM provider directly. No single point of failure.

## Embedding model: voyage-4-nano

memoryd uses [voyage-4-nano](https://huggingface.co/jsonMartin/voyage-4-nano-gguf) running locally via [llama.cpp](https://github.com/ggerganov/llama.cpp).

**Why this model:**

- **1024-dimensional embeddings** — high enough fidelity for precise similarity matching, small enough for fast local inference
- **~70MB GGUF quantized** — downloads in seconds, minimal disk footprint
- **Runs on CPU** — no GPU required. Works on any developer laptop including base-model MacBook Airs
- **Cosine similarity optimized** — the model was trained for retrieval tasks, which is exactly our use case

**Why local, not an API:**

- **Zero marginal cost** — no per-token embedding charges. Important when every conversation gets embedded in both directions.
- **No rate limits** — batch embedding of 50+ chunks doesn't hit API throttles.
- **Privacy** — raw conversation text never leaves the machine for embedding. Only post-redaction knowledge enters the shared store.

The embedder runs as a subprocess on port 7433, managed by the daemon. Batch embedding sends all chunks in a single HTTP call to amortize inference overhead.

## Vector similarity thresholds

Three similarity thresholds control how knowledge flows through the system. Each was tuned empirically against real coding conversations:

### Deduplication threshold: 0.92

When a new chunk's nearest neighbor in the store has cosine similarity ≥ **0.92**, the chunk is skipped as a duplicate.

**Why 0.92:** This is deliberately high. At 0.90, we saw too many false positives — chunks about the same *topic* but with meaningfully different details were being dropped. At 0.95, near-identical rephrasing slipped through and accumulated. 0.92 hits the sweet spot: "this says the same thing in slightly different words" gets caught, but "this adds new detail about the same topic" gets stored.

### Source extension threshold: 0.75

When a new chunk is similar to an existing *source* memory (ingested docs, wikis) at cosine ≥ **0.75**, it's stored as a "source extension" — linked back to the original reference material via metadata.

**Why 0.75:** This is intentionally loose. We want to capture the connection between reference documentation and real-world learnings about the same topic, even when the wording is quite different. A wiki article about the deploy process and a debugging session about a failed deploy are related at ~0.75-0.85 — different enough to both be valuable, similar enough that the link is meaningful.

### Merge threshold: 0.88

The steward merges near-duplicate items when their cosine similarity is ≥ **0.88**, keeping the one with more retrieval signal.

**Why 0.88:** Lower than the dedup threshold (0.92) because merge operates on items that have already been stored — meaning they passed the dedup check at ingest time. Over time, as context evolves, items that were distinct enough to both be stored can converge in meaning. 0.88 catches these convergences while preserving items that cover genuinely different aspects of a topic.

### Why these numbers matter for teams

With a shared store across 10+ contributors, the difference between a 0.90 and 0.92 dedup threshold is hundreds of near-duplicate items per week. Too aggressive and you lose nuance; too loose and retrieval quality degrades as the store grows. The same logic applies at scale for merge thresholds — the steward's hourly sweep over a team store processes far more candidate pairs than an individual store.

## Chunking strategy

Text is split at **paragraph boundaries** (double newlines), with a target of **~512 tokens** (~2048 characters) per chunk.

**Why paragraph-aware:**

- Logical units stay together. A function explanation, a list of deployment steps, a debugging narrative — these are natural knowledge boundaries.
- Sentence-level splitting loses context. Document-level chunks are too large for precise retrieval.
- 512 tokens balances retrieval precision (smaller = more specific matches) against context completeness (larger = more self-contained).

Oversized paragraphs are split further at sentence boundaries (`. `). Chunks shorter than 20 characters are discarded as noise.

## Quality scoring algorithm

The steward scores each knowledge item using a logarithmic usage signal with exponential time decay:

$$
\text{score} = \frac{\log_2(\text{hitCount} + 1)}{\log_2(\text{maxHits} + 1)} \times 0.5^{\text{daysSinceLastActive} / 7}
$$

**Why logarithmic usage:** A memory retrieved 100 times isn't 100x more valuable than one retrieved once — but it's meaningfully more valuable. Log scaling captures this diminishing return while still rewarding consistent retrieval.

**Why exponential decay with 90-day half-life:** Institutional knowledge stays relevant longer than you'd think — architecture decisions, debugging patterns, and deployment procedures are useful for months or quarters. The 90-day half-life means an unretrieved item loses half its score per quarter, preserving knowledge for intermittent tasks (quarterly reviews, seasonal deployments, periodic migrations) while still letting truly stale items fade.

**Why a 50-event learning period:** The system needs enough data to distinguish signal from noise. With fewer than 50 retrieval events, quality scoring would be based on too small a sample — penalizing items that simply haven't had a chance to be retrieved yet.

## Reciprocal Rank Fusion (RRF)

On Atlas, vector and text search results are fused using RRF with smoothing constant $k = 60$:

$$
\text{score}(d) = \sum_{L \in \{vector, text\}} \frac{1}{\text{rank}_L(d) + k + 1}
$$

**Why RRF over learned-weight fusion:** RRF is rank-based, not score-based. Vector similarity scores (0-1) and Lucene relevance scores (unbounded) live on completely different scales. RRF sidesteps the calibration problem entirely — it only cares about *relative ordering* within each result list.

**Why k=60:** The smoothing constant controls how much weight goes to top-ranked results vs. lower-ranked ones. Lower k (e.g., 10) concentrates weight heavily on #1; higher k (e.g., 100) flattens the distribution. k=60 is the standard in information retrieval literature and works well empirically for our mix of semantic and keyword results.

## Maximal Marginal Relevance (MMR)

After RRF fusion, results are re-ranked for diversity:

$$
\text{MMR}(d) = \lambda \cdot \text{relevance}(d) - (1 - \lambda) \cdot \max_{d_j \in S} \text{sim}(d, d_j)
$$

with $\lambda = 0.7$ (70% relevance, 30% diversity).

**Why MMR matters for teams:** A shared knowledge store accumulates multiple perspectives on the same topics. Without diversity re-ranking, the top 5 results might be five variations of "how to deploy the auth service" from five different team members' sessions. MMR ensures the AI tool gets context from *different angles* — the deploy procedure, a related debugging insight, and an architecture decision — rather than five near-redundant items.

## Secret redaction

13 regex patterns scrub sensitive content *before* embedding. This is a deliberate design choice: **secrets never enter the vector space**. Even if someone extracts the raw embeddings from MongoDB, they can't reverse-engineer redacted content.

The patterns cover:

| Category | Patterns |
|---|---|
| Cloud credentials | AWS access keys (`AKIA...`), AWS secret keys |
| Platform tokens | GitHub (`ghp_`, `gho_`, PATs), Slack (`xox...`), Stripe (`sk_live_...`) |
| Cryptographic material | Private key blocks (`-----BEGIN...`), SSH keys, JWTs (`eyJ...`) |
| Connection strings | Database URIs with embedded passwords |
| Generic | Bearer tokens, key-value pairs with `password`, `secret`, `token`, `api_key`, `credential` |

A second pass scans each line for key-value patterns containing sensitive keywords and redacts their values. The order is deliberate — specific patterns match first (AWS, GitHub), then generic patterns catch the rest.

## Adaptive noise filtering

Before any response reaches the LLM synthesis step, it passes through three progressively more expensive filters:

### Pre-filter (string matching)

Pure pattern matching — checks whether the user message is a known acknowledgment and the assistant response starts with a procedural prefix. Catches ~15-20% of exchanges at zero LLM cost.

### Length gate

Responses under 80 characters (configurable via `ingest_min_len`) are skipped. In benchmarking, 0% of responses under 100 characters contained durable knowledge.

### Content score gate

The raw response text is embedded and scored against **noise prototypes** — embeddings of content the system has previously identified as low-value. The scoring formula:

$$
\text{score} = \frac{\text{avgQualitySim}}{\text{avgQualitySim} + \text{topKNoiseSim}}
$$

Uses the **top-3 most similar** noise prototypes rather than averaging all of them. This prevents dilution when the prototype set grows large — averaging hundreds of diverse noise vectors would converge to a constant, destroying discriminative power.

### Adaptive learning loop

The noise prototypes aren't static. A ring buffer (500 entries) accumulates rejected exchanges from both the pre-filter and LLM synthesis stages. Every 25 rejections, the assistant texts are re-embedded and hot-swapped into the content scorer. The system progressively learns what noise looks like for your specific team and codebase.

**Critical design detail:** The content score gate does *not* feed its rejections back into the rejection store. Only pre-filter and synthesizer rejections contribute. This prevents a positive feedback loop where the scorer would amplify its own signal, gradually rejecting everything.

The rejection log persists across daemon restarts as JSONL at `~/.memoryd/rejection_log.jsonl`.

## Streaming architecture

The proxy handles Server-Sent Events (SSE) end-to-end:

1. Request arrives → forwarded to upstream exactly as-is (no modification)
2. Response streams back to the client in real-time via `http.Flusher`
3. Simultaneously, `text_delta` events are buffered
4. After stream completion, the buffered text enters the write pipeline asynchronously

**Why this matters:** The proxy is a true passthrough — it never modifies requests or responses. The write pipeline runs in a goroutine after the response is fully delivered, adding exactly zero latency to the developer experience. Knowledge retrieval happens separately through the MCP server, where AI tools explicitly call `memory_search` to access the shared knowledge base. This separation keeps the proxy simple and predictable.

## MongoDB Atlas: the power multiplier

This deserves its own section. The difference between Atlas and Community/Docker isn't just hosting — it's a fundamentally different retrieval engine.

### What Atlas provides that Community doesn't

| Feature | What it does | Impact on retrieval |
|---|---|---|
| **Lucene full-text search** (`$search`) | Exact keyword matching on content | Finds error codes, class names, config keys that semantic search underranks |
| **Hybrid search** (vector + text, fused) | Two complementary search strategies combined via RRF | Retrieves items that neither search would surface alone |
| **Pre-filtering in `$vectorSearch`** | Filter by `quality_score`, `source` regex *before* similarity ranking | Excludes noise at the database level — faster and more accurate |
| **MMR diversity re-ranking** | Post-fusion re-ranking to maximize result diversity | Top results cover different angles instead of near-duplicates |
| **Source-scoped regex filters** | Restrict search to specific knowledge sources | Team leads or BUs can scope queries to their domain |

### What Community/Docker provides

- Basic `$vectorSearch` with cosine similarity — finds semantically similar items
- No pre-filtering, no text search, no fusion, no re-ranking
- All knowledge items are candidates regardless of quality score or source

### The practical difference

On an individual store with a few hundred items, the gap is small — basic vector search works fine.

On a team store with thousands of items from dozens of contributors:

- **Without Lucene text search**, exact error codes and config values get buried under semantically-similar-but-wrong items
- **Without quality pre-filtering**, noisy low-value items dilute the top-K results
- **Without MMR diversity**, the AI tool gets five variations of the same knowledge instead of five different useful context items
- **Without hybrid fusion**, you get semantic *or* keyword results — never the best of both

Atlas turns memoryd from "decent knowledge recall" into "consistently surfaces the right context." For team deployments, this is the difference between adoption and abandonment.

### Atlas setup cost

Minimal. One person on the team:

1. Creates a free-tier or shared Atlas cluster
2. Runs the provided index creation script (`scripts/create_atlas_indexes.js`)
3. Shares the connection string

That's it. Every team member's daemon connects to the same cluster. Atlas handles replication, scaling, and index management.

See [Hybrid Search](hybrid-search) for the full technical details of the Atlas search pipeline.

## Technology choices

| Component | Choice | Rationale |
|---|---|---|
| **Language** | Go | Single binary, no runtime dependencies. Cross-compiles to macOS/Linux. Excellent concurrency primitives for the async write pipeline and background steward. |
| **Database** | MongoDB Atlas | Vector search + full-text search in one platform. Atlas-specific features (hybrid search, pre-filtering, re-ranking) are the [power multiplier](#mongodb-atlas-the-power-multiplier) detailed above. Teams already know how to manage Atlas. |
| **Embedding runtime** | llama.cpp | Mature C++ inference engine. Runs any GGUF model. Subprocess management is simpler than embedding a Go inference library. |
| **Embedding model** | voyage-4-nano (Q8_0 GGUF) | Optimized for retrieval. 1024-dim is the sweet spot for fidelity vs. performance. ~70MB quantized. |
| **Protocol** | MCP (Model Context Protocol) | Open standard supported by Claude Code, Cursor, Windsurf, and growing. Stdio transport means no port conflicts. |
| **Proxy transport** | HTTP with SSE | Native to Anthropic's API. No protocol translation needed. Streaming works with `http.Flusher`. |
