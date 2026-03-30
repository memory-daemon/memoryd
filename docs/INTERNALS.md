# memoryd — Internal Architecture Documentation

> Persistent memory for coding agents. A local daemon that gives AI coding agents long-term memory via transparent ingestion and retrieval backed by MongoDB vector search.

This document is the definitive engineering reference for memoryd. It covers every subsystem in detail, with particular depth on the **ingest processing pipeline** — the core differentiator that transforms raw web pages, documents, and agent conversations into high-quality, searchable knowledge.

---

## Table of Contents

1. [System Overview](#1-system-overview)
2. [Data Model](#2-data-model)
3. [Ingest Processing Pipeline (Deep Dive)](#3-ingest-processing-pipeline-deep-dive)
   - [3.1 Source Ingestion: Web Crawl Path](#31-source-ingestion-web-crawl-path)
   - [3.2 Source Ingestion: File Upload Path](#32-source-ingestion-file-upload-path)
   - [3.3 Crawler](#33-crawler)
   - [3.4 Structured Chunker](#34-structured-chunker)
   - [3.5 Section Builder & Heading Grouper](#35-section-builder--heading-grouper)
   - [3.6 Secret Redaction](#36-secret-redaction)
   - [3.7 Embedding](#37-embedding)
   - [3.8 Change Detection](#38-change-detection)
   - [3.9 Source Labeling & Provenance](#39-source-labeling--provenance)
4. [Write Pipeline (Proxy & MCP Capture)](#4-write-pipeline-proxy--mcp-capture)
   - [4.1 Preprocessing](#41-preprocessing)
   - [4.2 Chunking & Noise Filtering](#42-chunking--noise-filtering)
   - [4.3 Content Scoring](#43-content-scoring)
   - [4.4 Topic Group Detection](#44-topic-group-detection)
   - [4.5 LLM Synthesis](#45-llm-synthesis)
   - [4.6 Deduplication & Source Extension Tagging](#46-deduplication--source-extension-tagging)
   - [4.7 Rejection Store & Adaptive Noise Learning](#47-rejection-store--adaptive-noise-learning)
5. [Read Pipeline (Retrieval)](#5-read-pipeline-retrieval)
   - [5.1 Local Mode (Vector Search)](#51-local-mode-vector-search)
   - [5.2 Atlas Mode (Hybrid Search)](#52-atlas-mode-hybrid-search)
   - [5.3 Context Formatting & Injection](#53-context-formatting--injection)
   - [5.4 Quality Tracking](#54-quality-tracking)
6. [Steward (Background Quality Maintenance)](#6-steward-background-quality-maintenance)
   - [6.1 Scoring Formula](#61-scoring-formula)
   - [6.2 Pruning](#62-pruning)
   - [6.3 Merging Near-Duplicates](#63-merging-near-duplicates)
7. [Storage Layer](#7-storage-layer)
   - [7.1 MongoStore](#71-mongostore)
   - [7.2 AtlasStore](#72-atlasstore)
   - [7.3 MultiStore (Fan-Out)](#73-multistore-fan-out)
8. [Proxy Layer](#8-proxy-layer)
   - [8.1 Anthropic Proxy](#81-anthropic-proxy)
   - [8.2 Q&A Synthesis & Session Summaries](#82-qa-synthesis--session-summaries)
   - [8.3 Pre-Filter & Rejection Flow](#83-pre-filter--rejection-flow)
9. [MCP Server](#9-mcp-server)
10. [Configuration & Security](#10-configuration--security)
11. [Key Thresholds & Constants](#11-key-thresholds--constants)
12. [Package Dependency Map](#12-package-dependency-map)

---

## 1. System Overview

memoryd is a Go daemon that runs locally per developer. It captures knowledge from AI coding sessions and external documentation, stores it in MongoDB with vector embeddings, and surfaces it when relevant in future sessions.

```
                     ┌─────────────────────────────────┐
                     │         Coding Agent             │
                     │   (Claude Code, Cursor, etc.)    │
                     └──────┬──────────────┬────────────┘
                            │              │
                    Proxy Mode         MCP Mode
                    (transparent)      (explicit tools)
                            │              │
                     ┌──────▼──────────────▼────────────┐
                     │          memoryd daemon           │
                     │        127.0.0.1:7432             │
                     │                                   │
                     │  ┌─────────┐  ┌──────────────┐   │
                     │  │  Proxy  │  │  MCP Server   │   │
                     │  │ /v1/msg │  │  (stdio)      │   │
                     │  └────┬────┘  └──────┬────────┘   │
                     │       │              │             │
                     │  ┌────▼──────────────▼────────┐   │
                     │  │     Write Pipeline          │   │
                     │  │  chunk → filter → embed →   │   │
                     │  │  dedup → score → store      │   │
                     │  └─────────────────────────────┘   │
                     │                                    │
                     │  ┌─────────────────────────────┐   │
                     │  │     Read Pipeline            │   │
                     │  │  embed query → search →      │   │
                     │  │  format → inject/return      │   │
                     │  └─────────────────────────────┘   │
                     │                                    │
                     │  ┌──────────┐  ┌──────────────┐   │
                     │  │ Ingester │  │   Steward     │   │
                     │  │ crawl →  │  │  score →      │   │
                     │  │ chunk →  │  │  prune →      │   │
                     │  │ store    │  │  merge        │   │
                     │  └──────────┘  └──────────────┘   │
                     │                                    │
                     │  ┌─────────────────────────────┐   │
                     │  │   llama-server (port 7433)   │   │
                     │  │   voyage-4-nano embeddings   │   │
                     │  └─────────────────────────────┘   │
                     └──────────────┬─────────────────────┘
                                    │
                     ┌──────────────▼─────────────────────┐
                     │     MongoDB (Atlas or Local)        │
                     │                                     │
                     │  memories  │ retrieval_events       │
                     │  sources   │ source_pages           │
                     └─────────────────────────────────────┘
```

**Three integration modes:**

| Mode | Read | Write | Use Case |
|------|------|-------|----------|
| `proxy` | MCP tools | Auto-capture via API proxy | Full automation — agent doesn't need to know about memory |
| `mcp` | MCP tools | MCP tools | Explicit control — agent decides what to search and store |
| `mcp-readonly` | MCP tools | Disabled | Safe mode — agents can search but cannot pollute the store |

---

## 2. Data Model

### Collection: `memories`

| Field | Type | Purpose |
|-------|------|---------|
| `_id` | ObjectId | Primary key |
| `content` | string | The stored text chunk (secrets redacted) |
| `embedding` | []float32 | 1024-dim voyage-4-nano vector |
| `source` | string | Origin label: `"claude-code"`, `"mcp"`, `"source:docs\|https://..."` |
| `created_at` | time.Time | When stored |
| `metadata` | map[string]any | Extensible: `extends_source`, `extends_memory`, `page_url`, `source_name`, `database`, etc. |
| `hit_count` | int | Times returned in search results |
| `quality_score` | float64 | Steward-computed score (0–1), used for pre-filtering |
| `content_score` | float64 | Write-time semantic quality score (0–1) |
| `last_retrieved` | time.Time | Most recent search appearance |

### Collection: `sources`

Tracks ingested external knowledge sources (crawled sites, uploaded files).

| Field | Type | Purpose |
|-------|------|---------|
| `name` | string | Human label for the source |
| `base_url` | string | Crawl origin URL or `upload://NAME` |
| `status` | string | `"crawling"`, `"processing"`, `"ready"`, `"error"` |
| `page_count` | int | Pages/files processed |
| `memory_count` | int | Total memories stored |
| `max_depth` / `max_pages` | int | Crawl bounds |
| `headers` | map | Custom HTTP headers (Cookie, Authorization) |
| `error` | string | Last error message if status is "error" |

### Collection: `source_pages`

Page-level change detection for incremental re-crawls.

| Field | Type | Purpose |
|-------|------|---------|
| `source_id` | ObjectId | References parent source |
| `url` | string | Page URL or filename |
| `content_hash` | string | SHA-256 of extracted text |
| `last_fetched` | time.Time | When last crawled |

### Collection: `retrieval_events`

Quality learning signal — logged every time memories appear in search results.

| Field | Type | Purpose |
|-------|------|---------|
| `memory_id` | ObjectId | Which memory was retrieved |
| `score` | float64 | Similarity score at retrieval time |
| `created_at` | time.Time | When the retrieval happened |

---

## 3. Ingest Processing Pipeline (Deep Dive)

The ingest pipeline is responsible for turning **external knowledge** — documentation sites, company wikis, uploaded files — into high-quality, searchable memory entries. This is the primary differentiator from simple RAG systems because of its multi-stage approach to structure preservation, intelligent chunking, and incremental change detection.

### Overview: Ingest Data Flow

```
                    ┌──────────────────────────────────────┐
                    │           Source Definition           │
                    │  name, URL, max_depth, max_pages,    │
                    │  headers (Cookie, Authorization)     │
                    └──────────────────┬───────────────────┘
                                       │
                    ┌──────────────────▼───────────────────┐
                    │           Crawler (BFS)               │
                    │  fetch → extract text → extract links │
                    │  → SHA-256 hash → follow internal     │
                    │    links up to max_depth              │
                    │  Delay: 100ms between requests        │
                    │  Limit: 5MB per page                  │
                    └──────────────────┬───────────────────┘
                                       │
                          List of Page{URL, Content, Hash}
                                       │
              ┌────────────────────────▼──────────────────────┐
              │        Per-Page Processing Loop                │
              │                                                │
              │  1. Change Detection                           │
              │     └─ Compare hash to stored source_pages     │
              │     └─ Unchanged? Skip (count existing mems)   │
              │     └─ Changed? Delete old mems from that page │
              │                                                │
              │  2. Structured Chunking                        │
              │     └─ ChunkStructured() → []ChunkedSegment    │
              │        ├─ Parse into typed segments             │
              │        │  (heading, code, list, table, prose)   │
              │        ├─ Merge adjacent compatible segments    │
              │        │  within 512-token budget               │
              │        └─ Split oversized at type boundaries    │
              │                                                │
              │  3. Section Building                            │
              │     └─ groupByHeading(): consecutive segments   │
              │        sharing a heading → single section       │
              │        prefixed with "## Heading\n\n"          │
              │                                                │
              │  4. Secret Redaction                            │
              │     └─ redact.Clean() on each section           │
              │                                                │
              │  5. Length Gate                                 │
              │     └─ Sections < 30 chars are dropped          │
              │                                                │
              │  6. Batch Embedding                             │
              │     └─ EmbedBatch() → one HTTP call to llama   │
              │        All sections for a page in one batch     │
              │                                                │
              │  7. Storage                                     │
              │     └─ Insert each section as a Memory          │
              │        source = "source:NAME|PAGE_URL"          │
              │        metadata = {source_name, page_url}       │
              │                                                │
              │  8. Record Source Page                          │
              │     └─ UpsertSourcePage(hash) for next crawl    │
              └────────────────────────────────────────────────┘
```

### 3.1 Source Ingestion: Web Crawl Path

**Entry point:** `Ingester.IngestSource(ctx, source)`
**Triggered by:** POST `/api/sources` (dashboard/API), `source_ingest` MCP tool, `memoryd ingest` CLI

The web crawl path processes documentation sites and wikis. It runs asynchronously (fired as a goroutine from the API) with a 30-minute timeout.

**Step-by-step flow:**

1. **Crawl** — `crawler.Crawl()` does a BFS traversal starting from the source's base URL.
2. **Per-page loop** — For each crawled page:
   - **Change detection:** Fetch the stored `SourcePage` record by `(sourceID, pageURL)`. If the content hash matches, skip this page entirely and add its existing memory count to the running total.
   - **Cleanup:** If a page has changed, delete all memories whose source matches `source:NAME|URL` (the old version's memories).
   - **Build sections:** Call `buildSections(page.Content)` which runs the structured chunker + heading grouper + redactor.
   - **Batch embed:** All sections for one page are embedded in a single `EmbedBatch()` call to llama-server.
   - **Store:** Each section becomes a `Memory` with source `"source:NAME|PAGE_URL"` and metadata containing `source_name` and `page_url`.
   - **Record page hash:** `UpsertSourcePage()` stores the content hash for the next crawl's change detection.
3. **Finalize** — Update the source's status to `"ready"` with page/memory counts.

**Why this matters:** The change detection + page-level memory management means re-crawling a 500-page wiki only re-embeds pages that actually changed. This makes hourly/daily refreshes practical.

### 3.2 Source Ingestion: File Upload Path

**Entry point:** `Ingester.IngestFiles(ctx, source, files)`
**Triggered by:** POST `/api/sources/upload` (multipart or JSON), `source_upload` MCP tool

Identical to the crawl path but skips the crawler — files are pre-read with their content provided directly. Same change detection (SHA-256 hash per file), same chunking, same embedding. The source label becomes `"source:NAME|filename"` instead of a URL.

Files shorter than 50 characters are skipped entirely.

### 3.3 Crawler

**Package:** `internal/crawler`

The crawler performs a BFS (breadth-first search) web traversal with:

- **Scope control:** Only follows links to pages under the same host and path prefix as the base URL (`isInternal()` check).
- **Depth/page limits:** Configurable `max_depth` (default 3) and `max_pages` (default 500).
- **Politeness delay:** 100ms between requests (configurable).
- **Page size limit:** 5MB max per response.
- **Content type filtering:** Only processes `text/html` responses.
- **Custom headers:** Supports `Cookie`, `Authorization`, and other headers for authenticated sites.

**Text extraction** (`ExtractText()`):
1. Strip `<script>`, `<style>`, `<noscript>` blocks entirely (regex-based)
2. Remove all HTML tags
3. Decode HTML entities (`&amp;` → `&`)
4. Normalize whitespace (collapse tabs/spaces, limit consecutive blank lines to 2)

**Link extraction** (`ExtractLinks()`):
1. Find all `href="..."` attributes via regex
2. Skip fragment-only (`#`), `javascript:`, and `mailto:` links
3. Resolve relative URLs against the base URL
4. Deduplicate

**Content hashing:** Each page's extracted text is SHA-256 hashed for change detection. The hash is computed on the extracted text (after HTML stripping), not the raw HTML, so cosmetic/layout changes don't trigger re-ingestion.

### 3.4 Structured Chunker

**Package:** `internal/chunker`
**Entry point:** `ChunkStructured(text, maxTokens)` → `[]ChunkedSegment`

The chunker is structure-aware — it doesn't blindly split on character counts. It produces chunks that preserve logical document structure: headings stay with their content, code blocks remain intact, list items aren't split mid-item, and table rows stay together.

**Token budget:** 512 tokens ≈ 2048 characters (using a 4 chars/token approximation).

**Three-phase algorithm:**

#### Phase 1: Parse into Structural Segments

The text is scanned line-by-line. Each line is classified by regex patterns:

| Pattern | Segment Type | Detection |
|---------|-------------|-----------|
| `^#{1,6}\s+(.+)` | `segHeading` | Markdown heading |
| `` ^``` `` | `segCodeBlock` | Fenced code block (consumed until closing fence) |
| `^\|.*\|` | `segTableBlock` | Pipe-delimited table rows (consumed as block) |
| `^\s*[-*+]\|^\s*\d+[.)]` | `segListBlock` | List items (including continuation/indent lines) |
| everything else | `segPlain` | Prose paragraphs (consumed until structural boundary or double blank line) |

**Critical detail:** Each segment carries its **nearest heading context** — when a `# Heading` is encountered, all subsequent segments inherit that heading text until a new heading appears. This metadata flows through all subsequent phases.

**Consume functions:**
- `consumeCodeBlock()` — reads from opening fence to closing fence, preserving language tag
- `consumeTable()` — reads consecutive pipe-delimited lines
- `consumeList()` — reads items + continuation lines + blank lines between items (peek ahead for continuation)
- `consumeProse()` — reads until hitting a structural marker or two consecutive blank lines

#### Phase 2: Merge Adjacent Compatible Segments

Compatible segments are merged if:
- They share the same heading context
- The merged result fits under the token budget
- They're not crossing a heading boundary of equal or higher level
- Code blocks are never merged with non-code (keeps code self-contained)

This is the key optimization: instead of many tiny segements, you get coherent, topic-scoped chunks up to the full 512-token budget.

#### Phase 3: Split Oversized Segments

Segments exceeding the budget are split using **type-appropriate strategies**:

| Segment Type | Split Strategy |
|-------------|---------------|
| Prose | Paragraph boundaries → sentence boundaries → word boundaries (last resort) |
| Code | Function/class/type declaration boundaries (`funcBoundary` regex) → blank lines → word boundaries |
| List | Item boundaries (greedy grouping under budget) |

**Heading prefix injection:** When a split segment had a heading context that isn't already in the text, each resulting chunk gets prefixed with `## Heading\n\n` so the chunk is self-contained even when extracted from its document.

**Output:** `ChunkedSegment{Text, Heading, Kind}` — the text with heading/kind metadata preserved for downstream use.

### 3.5 Section Builder & Heading Grouper

**Location:** `internal/ingest/ingest.go` → `buildSections()`, `groupByHeading()`

This is the ingest pipeline's custom post-processing on top of the chunker, producing larger, more coherent memories than the chunker alone would yield.

**`buildSections(content)` flow:**
1. Call `chunker.ChunkStructured()` to get `[]ChunkedSegment`
2. Pass through `groupByHeading()` which collapses consecutive segments sharing the same heading into a single formatted entry
3. Run `redact.Clean()` on each result
4. Drop entries under 30 characters

**`groupByHeading()` logic:**

Given chunks A(heading=Install), B(heading=Install), C(heading=Usage), D(heading=Usage):

- A and B are grouped into one section: `## Install\n\nA_text\n\nB_text`
- C and D are grouped into one section: `## Usage\n\nC_text\n\nD_text`

This means a documentation page with a clear heading structure produces one memory per logical section rather than one per 512-token window. The memories are self-contained (heading included) and semantically coherent.

### 3.6 Secret Redaction

**Package:** `internal/redact`
**Called from:** `buildSections()` in ingest, `ProcessFiltered()` in write pipeline

Redaction happens **before embedding** — secrets never enter the vector store or embedding model.

**Pattern-based redaction** (16 regex patterns):

| Pattern | Replacement |
|---------|------------|
| AWS Access Key ID (`AKIA...`) | `[REDACTED:AWS_KEY]` |
| AWS Secret | `[REDACTED:AWS_SECRET]` |
| Generic API keys/tokens (20+ char values) | `[REDACTED]` |
| Bearer tokens | `[REDACTED]` |
| GitHub PATs/tokens | `[REDACTED:GITHUB_TOKEN]` |
| Slack tokens | `[REDACTED:SLACK_TOKEN]` |
| Stripe keys | `[REDACTED:STRIPE_KEY]` |
| Private keys (RSA, EC, DSA, OpenSSH) | `[REDACTED:PRIVATE_KEY]` |
| Connection strings with credentials | `[REDACTED:CONNECTION_STRING]@` |
| Passwords in key=value patterns | `[REDACTED]` |
| JWTs | `[REDACTED:JWT]` |
| SSH public keys | `[REDACTED:SSH_KEY]` |
| Email addresses | `[REDACTED:EMAIL]` |

**Keyword-based redaction** (`redactKeyValueLine()`):
- Scans each line for sensitive keywords (`password`, `secret`, `token`, `api_key`, etc.)
- If a keyword is followed by `=` or `:` and a value ≥ 4 chars, replaces the value with `[REDACTED]`
- Only operates on ASCII lines (skips multibyte UTF-8 to avoid byte-offset mismatches)

### 3.7 Embedding

**Package:** `internal/embedding`
**Implementation:** `LlamaEmbedder` → llama-server subprocess on port 7433

All text → vector conversion goes through the `Embedder` interface:

```go
type Embedder interface {
    Embed(ctx, text string) ([]float32, error)       // single text
    EmbedBatch(ctx, texts []string) ([][]float32, error) // batch
    Dim() int    // 1024 for voyage-4-nano
    Close() error
}
```

**Model:** voyage-4-nano Q8_0 GGUF (~70MB). Produces 1024-dimensional vectors with cosine similarity.

**Batch processing:** `EmbedBatch()` sends arrays of texts in a single HTTP POST to llama-server's `/v1/embeddings` endpoint. Batches are capped at 4 texts (`maxBatchSize`) to avoid KV cache overflow in llama-server. Larger batches are automatically split into sub-batches.

**Subprocess management:**
- llama-server runs in its own process group (`Setpgid: true`)
- Context window: 2048 tokens (`-c 2048`)
- Startup wait: up to 15 seconds for health check
- Cleanup: `sync.Once` guarded `Close()` sends SIGTERM to process group, waits 3s, then SIGKILL

### 3.8 Change Detection

The ingest pipeline uses SHA-256 content hashing for incremental processing:

```
Crawl page → extract text → SHA-256(text) → compare to stored hash
  ├─ Hash matches:  skip page, count existing memories
  └─ Hash differs:  delete old page memories → re-chunk → re-embed → store
```

**Collection:** `source_pages` stores `(source_id, url, content_hash, last_fetched)`.

**Granularity:** Page-level (web crawl) or file-level (uploads). A changed page/file causes **all** its memories to be deleted and re-created. This is a deliberate design choice — partial updates within a page would require diffing at the section level, which adds complexity for minimal benefit since re-embedding a page's sections is fast.

**Hash target:** The hash is computed on **extracted text**, not raw HTML. This means cosmetic HTML changes (layout, CSS classes, timestamps) don't trigger re-ingestion.

### 3.9 Source Labeling & Provenance

Every memory has full provenance tracking:

**Source field format:** `"source:NAME|URL"`
- `"source:docs|https://wiki.example.com/docs/api"` — crawled page
- `"source:runbooks|deployment.md"` — uploaded file
- `"claude-code"` — captured from proxy (agent conversation)
- `"claude-code-session"` — session synthesis summary
- `"mcp"` — explicitly stored via MCP tool

**Metadata:**
```json
{
  "source_name": "docs",
  "page_url": "https://wiki.example.com/docs/api",
  "extends_source": "source:docs|...",     // if this memory extends source content
  "extends_memory": "67f...",               // ID of the memory it extends
  "extends_score": 0.82,                   // similarity to the extended memory
  "database": "platform"                    // which database in multi-store
}
```

The pipe `|` in source labels is significant — it separates source name from page URL, enabling both page-level deletion (when a page changes) and source-level deletion (when removing an entire source).

---

## 4. Write Pipeline (Proxy & MCP Capture)

**Package:** `internal/pipeline`
**Entry point:** `WritePipeline.ProcessFiltered(text, source, metadata)` → `WriteResult`

The write pipeline handles **agent-generated** content — responses captured by the proxy, or content stored explicitly via MCP. It applies a different (more aggressive) filtering strategy than the ingest pipeline because agent conversations contain far more noise (acknowledgments, status updates, generic explanations).

### Overview: Write Data Flow

```
Raw assistant response or MCP content
                │
                ▼
        preprocessContent()
        ├─ Strip <think>...</think> blocks
        └─ Truncate at 50,000 chars (at newline boundary)
                │
                ▼
           chunker.Chunk()
        (512-token structure-aware chunks)
                │
                ▼
        Noise Filter (per chunk)
        ├─ Length gate: < noise_min_len (default 20) → drop
        └─ Alnum ratio: < noise_min_alnum_ratio (default 0.40) → drop
                │
                ▼
           redact.Clean()
        (strip secrets before embedding)
                │
                ▼
      ┌─ Single chunk ──────────────────────────────────────┐
      │  Embed() → storeMemory()                            │
      │                                                      │
      ├─ Multiple chunks ───────────────────────────────────┐
      │  EmbedBatch() → detectTopicGroups()                 │
      │       │                                              │
      │       ▼                                              │
      │  Per topic group:                                    │
      │  ├─ Single chunk  → storeMemory() (pre-computed vec)│
      │  ├─ Multi-chunk + synthesis available:               │
      │  │   └─ Synthesize() → re-embed → storeMemory()    │
      │  └─ Multi-chunk + no synthesis:                      │
      │      └─ join("\n\n") → re-embed → storeMemory()    │
      └─────────────────────────────────────────────────────┘
                │
                ▼
         storeMemory()
         ├─ Content score gate (< threshold → drop)
         ├─ Dedup check (cosine ≥ 0.92 → skip)
         ├─ Source extension tagging (cosine ≥ 0.75 → tag)
         └─ Insert to MongoDB
```

### 4.1 Preprocessing

**`preprocessContent(text)`** does two things:

1. **Strip `<think>` blocks** — Claude's extended-thinking output is the model's internal reasoning process, not durable knowledge. Stripped iteratively (handles nested/multiple blocks). Unclosed tags strip everything from `<think>` to end of string.

2. **Truncate to 50,000 chars** — Oversized inputs (full multi-file code dumps, complete session transcripts) produce hundreds of noisy chunks. Truncation happens at the last newline before the limit so the opening context (usually the most informative part) is preserved.

### 4.2 Chunking & Noise Filtering

Text passes through the same structural chunker as the ingest pipeline (`chunker.Chunk()`), then each chunk is filtered:

- **Minimum length:** Chunks under `noise_min_len` characters (default 20) are dropped.
- **Alphanumeric ratio:** Chunks where less than `noise_min_alnum_ratio` (default 0.40) of characters are letters or digits are dropped. This catches chunks that are mostly whitespace, punctuation, or symbols.

Both thresholds are configurable via the dashboard and take effect immediately.

### 4.3 Content Scoring

**Package:** `internal/quality`
**Type:** `ContentScorer`

Every chunk is scored against two sets of **prototype embeddings** to determine semantic proximity to "high-value knowledge" vs. "noise":

**Quality prototypes** (high signal):
- "important technical decision with reasoning and rationale"
- "architecture pattern approach and implementation details"
- "debugging solution root cause analysis and fix"
- "configuration setup deployment and environment instructions"
- "code pattern best practice convention and design"
- "error message workaround resolution and explanation"

**Noise prototypes** (low signal):
- "greeting acknowledgment helpful response sure happy"
- "let me know if you need anything else feel free"
- "i will help you with that certainly of course"

**Scoring formula:** `score = avgQualitySim / (avgQualitySim + avgNoiseSim)`

This ratio normalization always produces a score in (0, 1), independent of absolute similarity magnitudes. A score near 1.0 means the chunk resembles technical knowledge; near 0.0 means it resembles conversational noise.

**Content score gate:** If `content_score_gate > 0` and the chunk scores below it, the chunk is dropped before dedup and storage.

**Nil safety:** A nil scorer returns 0.5 (neutral), so the system works without scoring.

### 4.4 Topic Group Detection

When multiple chunks exist, they're analyzed for topic boundaries before storage.

**Algorithm:**
1. Batch-embed all valid chunks
2. Walk consecutive pairs computing cosine similarity
3. Split at points where similarity drops below `topic_boundary_threshold` (default 0.65)
4. Also split when cumulative group length exceeds `max_group_chars` (default 2048) — ensures the re-embedded vector accurately represents the full text

**Result:** Contiguous runs of related chunks are grouped together. A 10-chunk response about two different topics becomes 2 groups, not 10 individual memories.

### 4.5 LLM Synthesis

**Package:** `internal/synthesizer`
**Model:** `claude-haiku-4-5-20251001`

When enabled (`llm_synthesis: true` + `ANTHROPIC_API_KEY` set), multi-chunk topic groups are distilled by an LLM call instead of naively joined.

**Topic group synthesis** (`Synthesize()`):
- Input: multiple related text chunks
- Output: single coherent memory entry formatted as `[module] topic` with 3-10 actionable bullets
- The prompt instructs the model to extract non-obvious facts, decisions with rationale, gotchas, and architecture shortcuts — not process narration.

**Q&A pair synthesis** (`SynthesizeQA()`):
- Input: user question + assistant answer
- Output: distilled memory entry, or sentinel `"SKIP"` if the exchange has no durable value
- The model acts as a quality gate — procedural exchanges ("I'll look at that", "I've made the changes") are rejected

**Session synthesis** (`SynthesizeConversation()`):
- Input: full conversation arc (multiple turns)
- Output: structured summary with key findings, decisions, dead ends
- Triggered at turn 3, then every 5 turns
- Stored with source `"claude-code-session"`

**Fallback:** When synthesis is unavailable, chunks are joined with `"\n\n"` separators.

### 4.6 Deduplication & Source Extension Tagging

**`storeMemory()` flow for each final chunk/group:**

1. **Content score gate** — If configured, drop chunks below the threshold.

2. **Dedup check** — `VectorSearch(vec, 1)` finds the single closest existing memory.
   - If similarity ≥ `dedup_threshold` (0.92): **skip entirely** — it's a duplicate.
   - If no match or below threshold: proceed to insert.

3. **Source extension tagging** — If the closest match has a `source:` prefix in its Source field and similarity ≥ `source_extension_threshold` (0.75):
   - Tag the new memory's metadata with `extends_source`, `extends_memory`, `extends_score`
   - This means "this memory adds developer-specific context on top of reference documentation"
   - The memory is still stored (it's not a duplicate), just annotated

4. **Insert** with `content_score` stored alongside the content.

**Important:** Dedup check is sequential per chunk within a batch. Earlier inserts in the same batch affect later lookups. This prevents storing near-identical chunks from the same response.

### 4.7 Rejection Store & Adaptive Noise Learning

**Package:** `internal/rejection`

The rejection store is a bounded ring-buffer (default 500 entries) persisted as JSONL at `~/.memoryd/rejection_log.jsonl`. It captures exchanges rejected by either the pre-filter or the LLM synthesizer.

**What gets stored:**
```go
Entry{
    Time:       time.Time,
    Stage:      "pre_filter" | "synthesizer",
    UserLen:    int,
    AsstLen:    int,
    UserPrefix: string,  // first 100 chars
    AsstText:   string,  // first 500 chars (used for embedding)
}
```

**Adaptive learning:** Every 25 additions, the store signals its `RebuildCh` channel. A background goroutine re-embeds all stored assistant texts as noise prototypes and hot-swaps the `ContentScorer`. This means the system **learns what noise looks like** from actual rejected content, gradually improving its scoring accuracy beyond the static default prototypes.

**Startup seeding:** If the rejection log has entries from a prior run, the scorer is immediately seeded with those noise prototypes.

---

## 5. Read Pipeline (Retrieval)

**Package:** `internal/pipeline`
**Entry point:** `ReadPipeline.Retrieve(ctx, userMessage)` → `(context string, error)`

The read pipeline converts a user query into relevant context for the agent. It's used by the MCP `memory_search` tool (all modes) and the proxy system prompt injection (proxy mode, currently disabled — retrieval is MCP-only).

### 5.1 Local Mode (Vector Search)

**Store:** `MongoStore`
**Query:** `$vectorSearch` aggregation pipeline

```
User message → Embed(text) → $vectorSearch(vector, topK=5, candidates=topK*20)
```

Returns top-k memories by cosine similarity. No pre-filtering, no text component. Works with MongoDB Community and Atlas Local (Docker).

### 5.2 Atlas Mode (Hybrid Search)

**Store:** `AtlasStore` (wraps `MongoStore`)
**Detection:** Runtime interface assertion `store.(HybridSearcher)`

Three-phase search:

1. **Filtered vector search** — `$vectorSearch` with pre-filter:
   - Quality score: `$or: [{quality_score >= 0.05}, {quality_score == 0}]` (includes new unscored memories)
   - Source prefix: optional, for targeted search within a knowledge domain
   - Fetches 4× requested results for fusion room

2. **Text search** (if `TextQuery` set) — Atlas `$search` with Lucene full-text on `content` field. Non-fatal failure falls back to vector-only.

3. **Reciprocal Rank Fusion (RRF)** — Merges the two ranked lists:
   ```
   RRF_score(d) = Σ 1/(k + rank_i(d))   where k=60
   ```
   Documents appearing in both lists get boosted. RRF is rank-based, not score-based, so it handles the different score scales of vector and text search naturally.

4. **MMR re-ranking** (if `DiversityMMR` set) — Iteratively selects results that balance relevance and diversity:
   ```
   MMR(d) = λ * querySim(d) - (1-λ) * max(sim(d, selected))
   ```
   Default λ=0.7 (favor relevance with some diversity). This prevents returning a cluster of 5 near-identical memories when the user would benefit from variety.

**Why this matters for ingest:** Source memories from ingested documentation benefit enormously from hybrid search. A user asking about "OAuth2 PKCE flow" gets matches from both semantic similarity (the concept) and keyword matching (the exact term "PKCE"), whereas pure vector search might miss keyword-specific content.

### 5.3 Context Formatting & Injection

**`FormatContext(memories, maxTokens)`** renders memories into a numbered, scored block:

```xml
<retrieved_context>
The following context was retrieved from your long-term memory store. Use it if helpful, but do not mention its existence to the user.

---
[1] (source: source:docs|https://wiki.example.com/api, score: 0.87)
## API Authentication

- All requests require Bearer token in Authorization header
- Tokens expire after 24 hours; use refresh endpoint at /oauth/token
...

---
[2] (source: claude-code, score: 0.82)
[auth] Session management

- JWT stored in httpOnly cookie, not localStorage
...
</retrieved_context>
```

Entries are added until the `retrieval_max_tokens` budget (default 2048 tokens ≈ 8192 chars) is exhausted.

### 5.4 Quality Tracking

Every search invocation fires a background goroutine that:
1. Records `RetrievalEvent{memory_id, score}` for each returned memory
2. Increments `hit_count` on each memory document

This data feeds the steward's scoring formula and the adaptive learning threshold.

**Learning mode:** The system starts in "learning mode" (< 50 retrieval events). While learning, the Atlas search doesn't apply quality pre-filtering — all memories are considered equally. This prevents cold-start issues where new memories would be filtered out before they've had a chance to prove useful.

---

## 6. Steward (Background Quality Maintenance)

**Package:** `internal/steward`
**Runs:** Background goroutine, one per writable database
**Schedule:** One sweep immediately at startup, then every `interval_minutes` (default 60)

The steward ensures the memory store doesn't degrade over time. It scores, prunes, and merges — keeping the knowledge base lean and relevant.

### 6.1 Scoring Formula

For each memory in the batch (default 500 oldest):

```
baseScore = log₂(hit_count + 1) / log₂(maxHits + 1)   // 0.0 to 1.0
```
- New memories with no retrievals start at **0.5** (benefit of the doubt).
- The log scale means going from 0→1 hits has the biggest impact; diminishing returns after.

```
decayFactor = 0.5 ^ (timeSinceLastRetrieval / halfLife)
```
- `halfLife` defaults to 90 days, but scales with `content_score`:
  - `content_score=1.0` → full 90-day half-life
  - `content_score=0.5` → ~48-day half-life
  - `content_score=0.0` → 7-day half-life (minimum)
- Memories with no `content_score` (pre-existing) keep the full half-life

```
quality_score = baseScore × decayFactor   // clamped to [0, 1]
```

### 6.2 Pruning

A memory is pruned when ALL of:
- `quality_score < prune_threshold` (default 0.1)
- Age > `grace_period` (default 24h) — new memories get a chance
- `hit_count == 0` — never been retrieved (memories that proved useful once are kept)

This is intentionally conservative. The grace period protects fresh memories, and the hit_count check means any memory that was ever useful is preserved.

### 6.3 Merging Near-Duplicates

Scans up to 200 memories per sweep:

1. For each memory, find 5 nearest neighbors via `VectorSearch`
2. If any neighbor has cosine similarity ≥ `merge_threshold` (0.88):
   - **Keep** the one with more hits (ties go to older)
   - **Combine content** if the embedder is available and texts aren't substrings:
     - Concatenate with section header: `--- merged ---`
     - Re-embed the combined text
     - Update the kept memory's content and embedding
   - **Delete** the other one
   - Track deleted IDs to avoid double-deletion

**Content combination** (`combineContent()`):
- Checks if either text is a substring of the other (skip combination if so)
- Non-substring memories are joined with a `--- merged ---` separator
- The combined text is re-embedded so the vector accurately represents the full content

---

## 7. Storage Layer

### 7.1 MongoStore

**Package:** `internal/store`
**Implements:** `Store`, `QualityStore`, `SourceStore`, `StewardStore` (4 interfaces)

The base MongoDB implementation. Works with Community Edition, Atlas Local (Docker), and Atlas proper.

**Collections:** `memories`, `retrieval_events`, `sources`, `source_pages`

**Vector search:** Uses Atlas `$vectorSearch` aggregation stage with `numCandidates = topK * 20` for recall quality.

**Key methods:**
- `VectorSearch(embedding, topK)` — plain $vectorSearch, no filters
- `Insert(mem)` — sets `created_at` if missing
- `List(query, limit)` — regex text search on content field
- `ListBySource(prefix, limit)` — regex prefix match on source field
- `CountBySource(source)` — exact match count
- `ListOldest(limit)` — for steward scanning, includes embeddings
- `UpdateQualityScore(id, score)` — steward scoring
- `UpdateContent(id, content, embedding)` — steward merging
- `CheckVectorIndex(ctx)` — verifies vector_index exists at startup

### 7.2 AtlasStore

**Package:** `internal/store`
**Wraps:** `*MongoStore` (embedded)
**Adds:** `HybridSearcher` interface

Atlas-specific features that require Atlas proper (not Community, not Atlas Local):

- **Pre-filtered vector search** with quality score thresholds and source prefix filters
- **Full-text search** via Lucene `$search` on the `text_index`
- **Hybrid search** combining both via Reciprocal Rank Fusion
- **MMR re-ranking** for result diversity

**Detection:** The read pipeline uses runtime interface assertion `store.(HybridSearcher)` to auto-detect capabilities. No build tags or separate binaries.

### 7.3 MultiStore (Fan-Out)

**Package:** `internal/store`
**Implements:** `Store`, `HybridSearcher`

Fans out search across multiple databases and merges results by score. Supports the team knowledge hub pattern where each team has its own database.

**Write routing:** Only the primary (first writable) database accepts writes. All others are enforced read-only, even if configured as "full".

**Search fan-out:**
1. Launch a search goroutine per enabled database
2. Tag each result with its database name in metadata
3. Collect all results, sort by score descending, return top-k

**Database management:** Databases can be added/removed/toggled at runtime via the dashboard API. Changes are persisted to config.

---

## 8. Proxy Layer

### 8.1 Anthropic Proxy

**Package:** `internal/proxy`
**Endpoint:** `/v1/messages`

Transparently forwards all requests to the Anthropic API. Supports both synchronous and SSE streaming responses.

**Request path:** Headers forwarded as-is (except hop-by-hop). No modification to the request body.

**Response path (streaming):**
1. SSE events are forwarded to the client in real-time (`Flusher` interface)
2. Content block deltas with type `text_delta` are accumulated in a buffer
3. After stream completion, the buffered text is passed to the write pipeline asynchronously

**Response path (sync):**
1. Response body forwarded to client
2. Content text extracted from JSON response
3. Passed to write pipeline asynchronously

**Key property:** The proxy adds **zero latency** to the client path. All write pipeline work happens in goroutines after the response is fully delivered.

### 8.2 Q&A Synthesis & Session Summaries

When LLM synthesis is enabled, the proxy does more than raw capture:

**Per-exchange (`ingest()`):**
1. Extract the last user message from the request
2. **Pre-filter:** `QuickFilter()` checks if both user and assistant messages are procedural → reject immediately (feeds rejection store)
3. **Length gate:** Responses shorter than `ingest_min_len` (default 80 chars) are skipped — no LLM call
4. **Content score gate:** Raw assistant text is embedded and scored against noise prototypes via `PreScore()`. Below `content_score_pre_gate` (default 0.35) → skipped. Does NOT feed rejection store (prevents positive feedback loop)
5. **LLM quality gate:** `SynthesizeQA()` asks the model to distill or return `"SKIP"` → reject if no durable value (feeds rejection store)
6. **Store:** Distilled entry goes through `ProcessDirect()` (no chunking, already formatted)
7. **Fallback:** If no synthesizer, store raw Q&A pair

**Session synthesis:**
- Fired at 3 complete Q&A pairs, then every 5 pairs after
- Extracts all conversation turns from the request's messages array
- `SynthesizeConversation()` produces a structured session summary
- Stored with source `"claude-code-session"`
- Deduplicated against existing session summaries automatically

### 8.3 Pre-Filter & Rejection Flow

The `QuickFilter()` function is the cheapest rejection gate — pure string matching, no LLM call:

**Rejects when BOTH conditions are true:**
1. User message is a known acknowledgment ("ok", "thanks", "lgtm", "👍", etc. — ~35 patterns)
2. Assistant response starts with a procedural prefix ("i'll ", "i've updated", "sure! ", "done.", etc. — ~45 patterns)

**Design:** Intentionally conservative. Only rejects exchanges where both sides are clearly procedural. Ambiguous cases (detailed user question + procedural response) are passed to the LLM synthesizer for judgment.

Rejected exchanges are logged to the rejection store for adaptive noise learning.

---

## 9. MCP Server

**Package:** `internal/mcp`
**Protocol:** JSON-RPC over stdio
**Started by:** `memoryd mcp` command

The MCP server is a thin stdio bridge that calls the daemon's HTTP API. It delegates all logic to the daemon, ensuring a single source of truth.

**Tools:**

| Tool | Purpose | Read-Only? |
|------|---------|-----------|
| `memory_search` | Embed + vector search → formatted results | Yes |
| `memory_store` | Store content via write pipeline | No |
| `memory_list` | List memories with optional text filter | Yes |
| `memory_delete` | Delete a memory by ID | No |
| `memory_update` | Update memory content (re-embeds) | No |
| `source_ingest` | Crawl URL → chunk → embed → store | No |
| `source_upload` | Upload files → chunk → embed → store | No |
| `source_list` | List ingested sources | Yes |
| `source_remove` | Delete source + all its memories | No |
| `database_list` | List configured databases | Yes |
| `quality_stats` | Adaptive learning status | Yes |

In `mcp-readonly` mode, all write tools are omitted from the tool list.

**Auto-registration:** On first `memoryd start`, the daemon auto-registers itself as an MCP server in Claude Code, Cursor, Windsurf, and Cline (if installed).

---

## 10. Configuration & Security

### Config File: `~/.memoryd/config.yaml`

Created automatically on first run with sensible defaults. All pipeline thresholds are hot-reloadable via the dashboard.

### Authentication

- **API token** stored at `~/.memoryd/token` (64 hex characters, 0600 permissions)
- Auto-generated by `EnsureToken()` on daemon startup
- Required for `/api/*` and `/` (dashboard) endpoints
- Not required for `/v1/*` (proxy) or `/health`
- Checked via `Authorization: Bearer <token>` header or `?token=` query param

### Credential Management

- MongoDB URIs with passwords use OS keychain storage
- Config file stores sentinel `keychain:memoryd/mongodb_atlas_uri`
- At load time, `resolveCredential()` fetches from keychain
- Never stored in plaintext on disk

### Network Security

- Daemon binds to `127.0.0.1` only — not exposed to network
- llama-server also binds to `127.0.0.1` on port 7433
- Token protects against local process attacks (other software on the machine)

---

## 11. Key Thresholds & Constants

| Constant | Default | Location | Purpose |
|----------|---------|----------|---------|
| `DedupThreshold` | 0.92 | pipeline/write.go | Cosine ≥ this = duplicate, skip |
| `SourceExtensionThreshold` | 0.75 | pipeline/write.go, ingest.go | Cosine ≥ this (but < dedup) = tag as extending source |
| `TopicBoundaryThreshold` | 0.65 | pipeline/write.go | Cosine < this between adjacent chunks = topic boundary |
| `maxGroupChars` | 2048 | pipeline/write.go | Max chars in a topic group (matches embedding context window) |
| `minContentLen` | 20 | pipeline/write.go | Minimum chars to store |
| `minSectionLen` | 30 | ingest/ingest.go | Minimum chars for an ingested section |
| `DefaultMaxTokens` (chunker) | 512 | chunker/chunker.go | Target chunk size (~2048 chars) |
| `maxPreprocessChars` | 50,000 | pipeline/write.go | Max input length before truncation |
| `RetrievalTopK` | 5 | config default | Memories per search |
| `RetrievalMaxTokens` | 2048 | config default | Context budget for injection |
| `QualityLearningThreshold` | 50 | quality/tracker.go | Retrieval events before quality filtering activates |
| `PruneThreshold` | 0.1 | steward config | Score below which memories get pruned |
| `PruneGracePeriod` | 24h | steward config | Minimum age before pruning |
| `DecayHalfLife` | 90d | steward config | Unretrieved memory score half-life |
| `MergeThreshold` | 0.88 | steward config | Similarity for merge candidates |
| `StewardBatchSize` | 500 | steward config | Max memories per sweep |
| `maxBatchSize` | 4 | embedding/llama.go | Max texts per EmbedBatch call |
| `minSessionTurns` | 3 | proxy/anthropic.go | Pairs before first session summary |
| `sessionSynthesisInterval` | 5 | proxy/anthropic.go | Pairs between subsequent summaries |
| `RebuildEvery` (rejection) | 25 | rejection/store.go | Rejections between scorer rebuilds |
| `DefaultMaxSize` (rejection) | 500 | rejection/store.go | Max entries in rejection ring buffer |
| `IngestMinLen` | 80 | config/PipelineConfig | Responses shorter than this skip Haiku entirely |
| `ContentScorePreGate` | 0.35 | config/PipelineConfig | Pre-Haiku noise gate: below this → skip |
| `noiseTopK` | 3 | quality/content.go | Top-K noise prototypes used in scoring |
| `maxRejectionProtos` | 150 | quality/content.go | Max rejection texts used as noise prototypes |

---

## 12. Package Dependency Map

```
cmd/memoryd/main.go ──────────────────────── CLI entrypoint
  ├── config          Load YAML, expand paths, resolve keychain
  ├── embedding       LlamaEmbedder (llama-server subprocess)
  ├── store           MongoStore → AtlasStore → MultiStore
  ├── pipeline        ReadPipeline, WritePipeline
  ├── quality         Tracker, ContentScorer
  ├── steward         Background quality sweep
  ├── ingest          Ingester (crawl + file paths)
  ├── synthesizer     LLM synthesis via Anthropic API
  ├── rejection       Ring-buffer of rejected exchanges
  └── proxy           HTTP server, Anthropic proxy, dashboard, APIs

internal/ingest/ ─────────────────────────── Source ingestion
  ├── crawler         BFS web crawler with change detection
  ├── chunker         Structure-aware text splitting
  ├── redact          Secret scrubbing
  ├── embedding       Batch embedding via Embedder interface
  └── store           Insert memories + manage source pages

internal/pipeline/ ───────────────────────── Agent content processing
  ├── write.go        Chunk → filter → score → group → dedup → store
  ├── read.go         Embed query → search → quality tracking
  └── inject.go       Format context for system prompt

internal/proxy/ ──────────────────────────── HTTP layer
  ├── proxy.go        Server setup, route registration
  ├── anthropic.go    Transparent proxy + async write capture
  ├── api.go          REST API (search, store, ingest, memories, pipeline, databases)
  ├── source_api.go   Source CRUD (add/remove/refresh/upload)
  ├── dashboard.go    Dashboard HTML + data endpoint
  └── auth.go         Token-based auth middleware

internal/quality/ ────────────────────────── Scoring
  ├── content.go      ContentScorer (prototype-based semantic scoring)
  └── tracker.go      Retrieval event tracking + learning mode

internal/store/ ──────────────────────────── Persistence
  ├── store.go        Interfaces (Store, QualityStore, SourceStore, HybridSearcher)
  ├── mongo.go        MongoStore (Community + Atlas Local)
  ├── atlas.go        AtlasStore (hybrid search, RRF, MMR)
  └── multi.go        MultiStore (fan-out across databases)

internal/mcp/ ────────────────────────────── MCP stdio bridge
  └── server.go       JSON-RPC tools → daemon HTTP API
```

---

## Appendix: Ingest Pipeline vs. Write Pipeline — Comparison

| Aspect | Ingest Pipeline | Write Pipeline |
|--------|----------------|---------------|
| **Source** | External docs, wikis, uploaded files | Agent conversations (proxy/MCP) |
| **Chunker** | `ChunkStructured()` with heading metadata | `Chunk()` (same core, no metadata return) |
| **Post-chunk** | `groupByHeading()` → section-level memories | `detectTopicGroups()` → cosine-based grouping |
| **Synthesis** | None (sections are already well-structured) | LLM synthesis via `Synthesize()`, `SynthesizeQA()` |
| **Dedup** | None (page-level delete + re-create) | Per-chunk cosine ≥ 0.92 dedup |
| **Content scoring** | None (ingested content is assumed high quality) | `ContentScorer` gate + `content_score` stored |
| **Change detection** | SHA-256 hash per page/file | Not applicable (each response is new) |
| **Source extension** | Not applicable | Tags chunks similar to source memories |
| **Async** | Yes (30-minute timeout goroutine) | Yes (goroutine, errors logged not returned) |
| **Noise filtering** | Length gate (30 chars) | Length + alnum ratio + content score |
| **Secrets** | Redacted before embedding | Redacted before embedding |
| **Embedding** | Batch per page (all sections) | Single or batch per response |

The ingest pipeline trusts the quality of its input (curated documentation) and focuses on structure preservation and change detection. The write pipeline faces adversarial noise (agent chatter) and focuses on quality filtering, deduplication, and synthesis.
