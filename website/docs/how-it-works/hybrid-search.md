---
sidebar_position: 4
title: Hybrid Search
---

# Hybrid Search

When connected to a MongoDB Atlas cluster (the recommended setup for teams), memoryd uses a multi-stage search pipeline that combines two complementary approaches to find the most relevant knowledge.

## MongoDB Atlas vs. Community Edition

This is the single biggest reason to use Atlas for team deployments. The difference in retrieval quality is substantial:

| Capability | Atlas Cluster | Community / Atlas Local (Docker) |
|---|---|---|
| **Vector search** | ✅ Full `$vectorSearch` with pre-filtering | ✅ Basic `$vectorSearch` only |
| **Full-text search** | ✅ Lucene-powered `$search` on content | ❌ Not available |
| **Hybrid search** | ✅ Vector + text fused with RRF | ❌ Vector only |
| **Quality pre-filtering** | ✅ Filter by `quality_score` at search time | ❌ All results returned, noise included |
| **Source-scoped search** | ✅ Filter by source using regex in `$vectorSearch` | ❌ No filter support |
| **Diversity re-ranking (MMR)** | ✅ Results diversified to avoid redundancy | ❌ Nearest-neighbor only |
| **Index types** | Vector index + Lucene text index | Vector index only |

**In practice:** On a team store with 5,000+ knowledge items, Atlas hybrid search returns noticeably better results — the right error code, the relevant architecture context, *and* the related debugging insight, all in one retrieval. Community edition's vector-only search finds semantically similar items but misses exact keyword matches and can't filter out noise.

Community/Docker is fine for individual development. **For team deployments, Atlas is the clear choice.**

## Why hybrid?

Consider a developer asking about `ERR_CONN_REFUSED`. A meaning-based search finds knowledge about connection errors in general — useful, but not specific. A keyword-based search finds the exact item that mentions that precise error code. Hybrid search combines both signals to deliver the best of each.

This matters for teams: the shared knowledge store contains a mix of high-level architectural context and specific technical details. Hybrid search surfaces both.

## How the Atlas hybrid pipeline works

memoryd automatically detects whether your store supports hybrid search at runtime. When `atlas_mode: true`, the full pipeline activates:

```
Query → [Vector Search] + [Text Search] → RRF Fusion → MMR Re-ranking → Top-K
```

### 1. Meaning-based search (semantic)

The query embedding is compared against stored knowledge using Atlas Vector Search (`$vectorSearch`):

- **Quality pre-filtering** — items with `quality_score < 0.05` are excluded at the database level (brand new, unscored items are kept via an `$or` clause)
- **Source scoping** — optional regex filter on the `source` field lets you restrict results to specific knowledge sources
- **Oversampling** — fetches `topK × 20` candidates to give the fusion and diversity stages a rich pool to work with

This is an Atlas-only feature. Community edition supports basic `$vectorSearch` but cannot pre-filter on fields or combine with other pipeline stages.

### 2. Keyword-based search (lexical)

A parallel Lucene full-text search runs via Atlas `$search` against the `content` field. This catches exact keyword matches — acronyms, error codes, class names, specific config values — that embedding similarity alone might not rank highly.

**This entire stage is unavailable on Community edition.** It requires a Lucene text index, which is an Atlas-specific feature. This is the biggest capability gap between Atlas and Community.

### 3. Reciprocal Rank Fusion (RRF)

The two result lists are combined using RRF with smoothing constant $k = 60$:

$$
\text{score}(d) = \sum_{L \in \{vector, text\}} \frac{1}{\text{rank}_L(d) + k + 1}
$$

RRF is rank-based, not score-based, so it works naturally across the different scoring scales of vector similarity (0-1) and Lucene relevance (unbounded). An item ranked highly by both searches scores highest; an item found by only one search still appears.

### 4. Maximal Marginal Relevance (MMR)

The fused results are re-ranked to maximize diversity:

$$
\text{MMR}(d) = \lambda \cdot \text{relevance}(d) - (1 - \lambda) \cdot \max_{d_j \in S} \text{sim}(d, d_j)
$$

With $\lambda = 0.7$ — 70% weight on relevance, 30% on diversity. This prevents the top results from being five variations of the same knowledge. Instead, the AI tool gets context from *different angles* — a deployment procedure, a related debugging insight, and an architecture decision.

## What this means for teams

For a team of 10+ contributors, the shared knowledge store grows quickly. Atlas hybrid search ensures that:

- **Specific technical details** (error codes, config values, API endpoints) are found even when the question is phrased broadly — thanks to Lucene text search
- **Conceptual knowledge** (architecture decisions, design rationale) is found even when the question uses different terminology — thanks to vector search
- **Results are diverse** — the AI tool gets context from multiple angles, not five versions of the same thing — thanks to MMR
- **Low-quality noise is filtered out** — knowledge that was captured but never proved useful doesn't clutter results — thanks to quality pre-filtering

None of these capabilities (except basic vector search) are available on Community edition. For team deployments, Atlas is a genuine power multiplier — not just a hosting convenience, but a fundamentally better retrieval engine.

See [Architecture & Design Decisions](architecture) for the technical rationale behind the specific thresholds and algorithms.
