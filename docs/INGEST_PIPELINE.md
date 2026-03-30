# Ingest Pipeline Deep Dive

> The ingest pipeline is what makes memoryd more than "RAG with an MCP wrapper." It transforms raw documentation into structure-preserving, deduplicated, incrementally-refreshable knowledge — the foundation for a shared team knowledge system.

This document provides line-by-line clarity on the ingest pipeline's behavior, decision points, and design rationale. It's intended for engineers building on or debugging the pipeline.

---

## Table of Contents

- [Why This Matters](#why-this-matters)
- [Entry Points](#entry-points)
- [End-to-End Flow: Web Crawl](#end-to-end-flow-web-crawl)
- [End-to-End Flow: File Upload](#end-to-end-flow-file-upload)
- [Stage 1: Crawler](#stage-1-crawler)
- [Stage 2: Change Detection](#stage-2-change-detection)
- [Stage 3: Structured Chunking](#stage-3-structured-chunking)
- [Stage 4: Section Building](#stage-4-section-building)
- [Stage 5: Secret Redaction](#stage-5-secret-redaction)
- [Stage 6: Batch Embedding](#stage-6-batch-embedding)
- [Stage 7: Storage & Provenance](#stage-7-storage--provenance)
- [Design Decisions & Rationale](#design-decisions--rationale)
- [Comparison: Ingest vs. Write Pipeline](#comparison-ingest-vs-write-pipeline)
- [Future Work](#future-work)

---

## Why This Matters

Most RAG systems are glorified `split("\n\n")` → embed → search. They throw away document structure, can't incrementally update when sources change, have no concept of source provenance, and treat a heading the same as a footnote.

memoryd's ingest pipeline is different:

1. **Structure-aware chunking** — Headings, code blocks, lists, and tables are parsed as typed structural segments. A code block is never split mid-function. List items aren't severed from their context. Headings propagate to all their children.

2. **Heading-based section grouping** — After chunking, consecutive segments under the same heading are collapsed into a single memory. Documentation about "Authentication" becomes one coherent memory, not 4 fragments.

3. **Incremental change detection** — SHA-256 hashes track every page/file. Re-ingesting a 500-page wiki only re-processes pages that actually changed. Other pages keep their existing memories.

4. **Source provenance** — Every memory carries full provenance: which source, which page, when crawled. Memories can be traced back to their origin, refreshed when documentation updates, and deleted cleanly when a source is removed.

5. **Secret safety** — All content is redacted before embedding. AWS keys, API tokens, connection strings, and PII never enter the vector store.

This combination means memoryd can serve as a **team knowledge hub** — not just a session-scoped memory store, but a durable, infrastructure-grade knowledge base that improves with every crawl.

---

## Entry Points

### Web Crawl

```
POST /api/sources          (dashboard/REST API)
source_ingest MCP tool     (Claude Code/Cursor)
memoryd ingest CLI         (command line)
```

All three create a `Source` record and call `Ingester.IngestSource()` in a background goroutine with a 30-minute context timeout.

**Request payload:**
```json
{
  "name": "company-docs",
  "url": "https://wiki.example.com/docs",
  "max_depth": 3,
  "max_pages": 100,
  "headers": {
    "Cookie": "session=abc123",
    "Authorization": "Bearer ..."
  }
}
```

### File Upload

```
POST /api/sources/upload   (multipart form or JSON body)
source_upload MCP tool     (Claude Code/Cursor)
```

Creates a `Source` record (with `base_url = "upload://NAME"`) and calls `Ingester.IngestFiles()`.

**JSON payload:**
```json
{
  "name": "runbooks",
  "files": [
    {"filename": "deploy.md", "content": "# Deployment\n\n..."},
    {"filename": "oncall.md", "content": "# On-Call\n\n..."}
  ]
}
```

---

## End-to-End Flow: Web Crawl

```
IngestSource(ctx, source)
│
├─ 1. crawler.Crawl(baseURL, opts)
│     └─ BFS traversal → []Page{URL, Content, ContentHash}
│
├─ 2. for each page:
│     ├─ GetSourcePage(sourceID, url) → existing hash?
│     │   ├─ hash matches → SKIP (count existing memories)
│     │   └─ hash differs → DeleteMemoriesBySource("source:NAME|URL")
│     │
│     ├─ buildSections(page.Content)
│     │   ├─ ChunkStructured(text, 512) → []ChunkedSegment
│     │   ├─ groupByHeading(segments) → []string
│     │   ├─ redact.Clean(section) for each
│     │   └─ drop sections < 30 chars
│     │
│     ├─ EmbedBatch(sections) → [][]float32
│     │
│     ├─ Insert each as Memory{
│     │     content:  section text,
│     │     embedding: vector,
│     │     source:   "source:company-docs|https://wiki.example.com/docs/auth",
│     │     metadata: {source_name: "company-docs", page_url: "https://..."}
│     │   }
│     │
│     └─ UpsertSourcePage(sourceID, url, hash)
│
└─ 3. UpdateSourceStatus("ready", pageCount, memoryCount)
```

---

## End-to-End Flow: File Upload

```
IngestFiles(ctx, source, files)
│
├─ for each file:
│     ├─ Skip if len(content) < 50
│     │
│     ├─ SHA-256(content) → hash
│     ├─ GetSourcePage(sourceID, filename) → existing hash?
│     │   ├─ hash matches → SKIP
│     │   └─ hash differs → DeleteMemoriesBySource("source:NAME|filename")
│     │
│     ├─ buildSections(content) → same as web path
│     ├─ EmbedBatch(sections)
│     ├─ Insert each as Memory{source: "source:runbooks|deploy.md", ...}
│     └─ UpsertSourcePage(sourceID, filename, hash)
│
└─ UpdateSourceStatus("ready", fileCount, memoryCount)
```

The file path is virtually identical to the web path — only the content source differs. No crawling, no link following, no HTML stripping. The file content is expected to be plain text or Markdown.

---

## Stage 1: Crawler

**Package:** `internal/crawler`
**Function:** `Crawl(ctx, baseURL, opts) → ([]Page, error)`

### Algorithm: Breadth-First Search

```go
queue := []queueEntry{{url: baseURL, depth: 0}}
visited := map[string]bool{}

for len(queue) > 0 && len(pages) < MaxPages {
    entry := queue[0]; queue = queue[1:]
    if visited[entry.url] || entry.depth > MaxDepth { continue }
    visited[entry.url] = true

    body := fetchPage(entry.url, headers)
    text := ExtractText(body)
    links := ExtractLinks(body, entry.url)

    pages = append(pages, Page{URL: entry.url, Content: text, ContentHash: sha256(text)})

    for _, link := range links {
        if isInternal(link, baseURL) && !visited[link] {
            queue = append(queue, queueEntry{url: link, depth: entry.depth + 1})
        }
    }

    time.Sleep(opts.Delay)  // politeness: default 100ms
}
```

### Scope Control

`isInternal(link, baseURL)` ensures only links under the same host AND path prefix are followed:

```
Base URL: https://wiki.example.com/docs
✅ https://wiki.example.com/docs/api        → same host + prefix
✅ https://wiki.example.com/docs/api/auth    → deeper under prefix
❌ https://wiki.example.com/blog             → same host, different path
❌ https://other.example.com/docs            → different host
❌ javascript:void(0)                         → not HTTP
❌ mailto:user@example.com                    → not HTTP
```

### Text Extraction

`ExtractText(html)` transforms raw HTML into clean text:

1. **Strip unsafe elements** — Remove `<script>`, `<style>`, `<noscript>` tags and their contents entirely (regex: `<(script|style|noscript)[\s\S]*?</\1>`)
2. **Remove HTML tags** — Strip all remaining tags (`<[^>]+>`)
3. **Decode entities** — `&amp;` → `&`, `&lt;` → `<`, `&gt;` → `>`, `&quot;` → `"`, `&#39;` → `'`, `&nbsp;` → ` `
4. **Normalize whitespace** — Collapse spaces/tabs on each line, limit consecutive blank lines to 2

**Design notes:**
- Regex-based HTML processing is intentional — it's simpler and faster than a DOM parser, and documentation HTML is generally well-formed enough for regex patterns.
- The regex strips tag *contents* for script/style/noscript (preventing JS code from appearing in the extracted text), but only strips the tags themselves for all other elements (preserving the text content within them).
- The 5MB limit on `fetchPage()` prevents memory exhaustion on large files.

### Link Extraction

`ExtractLinks(html, baseURL)` finds all `href` attributes:

1. Regex: `href=["']?([^\s"'>]+)`
2. Skip: `#`-only, `javascript:`, `mailto:` links
3. Resolve: relative URLs resolved against the base URL
4. Strip fragments: `page#section` → `page`
5. Deduplicate

### Content Hashing

```go
page.ContentHash = fmt.Sprintf("%x", sha256.Sum256([]byte(page.Content)))
```

Computed on **extracted text**, not raw HTML. This means layout changes (new CSS class, restructured `<div>`s) don't trigger re-ingestion — only actual content changes do.

---

## Stage 2: Change Detection

**Collections:** `source_pages` (MongoDB)
**Key:** `(source_id, url)` — uniquely identifies one page within one source

### Flow

```
For each crawled page:
  1. Lookup: GetSourcePage(sourceID, pageURL)
  2. Compare: existing.ContentHash == page.ContentHash ?

  Case A: No existing record (new page)
    → Continue to chunking/embedding/storage
    → UpsertSourcePage(hash) creates the record

  Case B: Hash matches (unchanged page)
    → Skip all processing
    → Count existing memories for this page (CountBySource)
    → Add to running total (so status shows correct memory count)

  Case C: Hash differs (changed page)
    → DeleteMemoriesBySource("source:NAME|URL")  ← delete ALL old memories for this page
    → Re-chunk, re-embed, re-store
    → UpsertSourcePage(hash) updates the record
```

### Why Page-Level Granularity?

A changed page deletes ALL its memories and re-creates them, rather than trying to diff at the section level. Rationale:

1. **Simplicity** — Section-level diffing requires tracking which section maps to which memory, handling renamed/reordered sections, and dealing with partially changed sections. The complexity is enormous for minimal benefit.

2. **Speed** — Re-embedding 5-10 sections for one page takes < 1 second with local llama-server. The cost of re-processing is trivial.

3. **Correctness** — A heading change or section reorder could change how `groupByHeading()` groups chunks, producing entirely different memories than a diff-based approach would expect.

---

## Stage 3: Structured Chunking

**Package:** `internal/chunker`
**Function:** `ChunkStructured(text, maxTokens) → []ChunkedSegment`
**Budget:** 512 tokens ≈ 2048 characters

This is the most complex stage. The chunker produces structure-aware text segments that preserve document semantics.

### Phase 1: Parse into Typed Segments

The text is scanned line-by-line. Each line is classified and consumed by the appropriate handler:

```
Line Classification (checked in order):

1. Heading?     ^#{1,6}\s+(.+)          → segHeading
                Creates heading segment, updates currentHeading for all subsequent segments

2. Code fence?  ^```                     → segCodeBlock
                consumeCodeBlock() reads until closing fence (handles missing close gracefully)

3. Table row?   ^\|.*\|                  → segTableBlock
                consumeTable() reads consecutive pipe-delimited lines

4. List item?   ^\s*[-*+]|\s*\d+[.)]    → segListBlock
                consumeList() reads items + continuation lines + inter-item blanks

5. Otherwise                              → segPlain
                consumeProse() reads until structural marker or double blank line
```

**Every non-heading segment inherits the most recent heading.** This is the critical detail that enables heading-based grouping later.

### Consume Functions

**`consumeCodeBlock(lines, i)`**
```
Start: line[i] = ```python
       line[i+1] = def hello():
       line[i+2] =     print("hello")
       line[i+3] = ```

Result: "```python\ndef hello():\n    print(\"hello\")\n```"
```
Reads from opening fence to closing fence (or end of input if unclosed).

**`consumeTable(lines, i)`**
```
Start: line[i] = | Name | Type |
       line[i+1] = |------|------|
       line[i+2] = | foo  | int  |
       line[i+3] = (blank or non-table line)

Result: "| Name | Type |\n|------|------|\n| foo  | int  |"
```
Consumes consecutive lines matching `^\|.*\|`.

**`consumeList(lines, i)`**
```
Start: line[i] = - First item
       line[i+1] =   continuation of first item
       line[i+2] =
       line[i+3] = - Second item
       line[i+4] = Something else entirely

Result: "- First item\n  continuation of first item\n\n- Second item"
```
Handles: indented continuations, blank lines between items (peeks ahead for next item), nested lists.

**`consumeProse(lines, i)`**
Reads until hitting:
- A heading (`^#{1,6}\s+`)
- A code fence (`` ^``` ``)
- A table row (`^\|.*\|`)
- A list item (`^\s*[-*+]` or `^\s*\d+[.)]`)
- Two consecutive blank lines (paragraph boundary)

### Phase 2: Merge Adjacent Compatible Segments

```go
for each segment after the first:
    if compatible(prev, current) && fits(prev + current, budget):
        prev.text += "\n\n" + current.text
    else:
        flush prev, start new group
```

**Compatibility rules:**
- Same heading context (both inheriting "## API Reference")
- Not crossing a heading boundary of equal or higher level
- Code blocks are never merged with non-code segments

**Purpose:** Instead of 20 tiny segments of 50 words each, produce 4 coherent segments of 250 words each. This dramatically improves embedding quality — vectors for "Authentication uses JWT in httpOnly cookies, tokens expire in 24h, refresh at /oauth/token" are far more useful than a vector for just "Authentication uses JWT."

### Phase 3: Split Oversized Segments

Segments exceeding the budget are split using type-specific strategies:

**Prose → `splitProse()`**
Priority: paragraph boundaries (`\n\n`) → sentence boundaries (`. `, `! `, `? `) → word boundaries (space)
Greedy: accumulate until budget exceeded, flush and start new chunk.

**Code → `splitCode()`**
Priority: function/class boundaries (`func `, `def `, `class `, `type `, etc.) → blank lines → word boundaries
This keeps function bodies together rather than splitting mid-function.

**List → `splitList()`**
Priority: item boundaries. Greedy grouping of items under budget.
A list with 30 items splits into ~3 chunks of ~10 items each, never splitting mid-item.

### Output

```go
type ChunkedSegment struct {
    Text    string   // formatted chunk text
    Heading string   // nearest heading (e.g., "API Reference")
    Kind    string   // "prose", "code", "list", "table"
}
```

**Heading prefix injection:** When a split produces multiple chunks from a segment with a heading, each chunk gets prefixed with `## Heading\n\n` so it's self-contained. The original heading line is not duplicated if it's already in the text.

---

## Stage 4: Section Building

**Package:** `internal/ingest`
**Functions:** `buildSections()`, `groupByHeading()`

This stage is unique to the ingest pipeline (the write pipeline uses `detectTopicGroups()` instead). It exploits the heading metadata from `ChunkStructured()` to produce document-section-level memories.

### buildSections()

```go
func buildSections(content string) []string {
    segments := chunker.ChunkStructured(content, chunker.DefaultMaxTokens)
    grouped := groupByHeading(segments)
    var result []string
    for _, s := range grouped {
        s = redact.Clean(strings.TrimSpace(s))
        if len(s) >= minSectionLen {  // 30 chars minimum
            result = append(result, s)
        }
    }
    return result
}
```

### groupByHeading()

Collapses consecutive segments sharing the same heading into a single entry.

**Example input:**
```
segments[0] = {Text: "JWT tokens expire after 24h",   Heading: "Authentication"}
segments[1] = {Text: "Use /oauth/token to refresh",   Heading: "Authentication"}
segments[2] = {Text: "All endpoints require HTTPS",    Heading: "Security"}
segments[3] = {Text: "Rate limit: 100 req/min",        Heading: "Security"}
```

**Output:**
```
sections[0] = "## Authentication\n\nJWT tokens expire after 24h\n\nUse /oauth/token to refresh"
sections[1] = "## Security\n\nAll endpoints require HTTPS\n\nRate limit: 100 req/min"
```

Two memories instead of four. Each is self-contained (heading included in text), coherent (related content stays together), and large enough to produce a high-quality embedding.

**Edge cases:**
- Segments without a heading are stored individually (no heading prefix)
- A heading change flushes the current group and starts a new one
- Empty chunks (all whitespace) are silently skipped

---

## Stage 5: Secret Redaction

**Package:** `internal/redact`
**Function:** `Clean(text) → text`

Applied to every section after grouping, before embedding. Secrets never enter the vector store.

### Pattern-Based Redaction

16 regex patterns applied in sequence:

| Pattern | Example | Replacement |
|---------|---------|------------|
| AWS Access Key | `AKIAIOSFODNN7EXAMPLE` | `[REDACTED:AWS_KEY]` |
| AWS Secret Key | `wJalrXUtnFEMI/K7MDENG/bP...` | `[REDACTED:AWS_SECRET]` |
| Generic API key/token | `api_key="sk-proj-abc123def..."` | `[REDACTED]` |
| Bearer token | `Authorization: Bearer eyJ...` | `Bearer [REDACTED]` |
| GitHub PAT | `ghp_abcdef1234567890abcdef12` | `[REDACTED:GITHUB_TOKEN]` |
| Slack token | `xoxb-123-456-abc123` | `[REDACTED:SLACK_TOKEN]` |
| Stripe key | `sk_live_abcdef123456` | `[REDACTED:STRIPE_KEY]` |
| Private key block | `-----BEGIN RSA PRIVATE KEY-----` | `[REDACTED:PRIVATE_KEY]` |
| Connection string | `mongodb+srv://user:pass@cluster.mongodb.net` | `[REDACTED:CONNECTION_STRING]@cluster.mongodb.net` |
| Password in key=value | `DB_PASSWORD=hunter2` | `DB_PASSWORD=[REDACTED]` |
| JWT | `eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIx...` | `[REDACTED:JWT]` |
| SSH key | `ssh-rsa AAAA...` | `[REDACTED:SSH_KEY]` |
| Email address | `user@example.com` | `[REDACTED:EMAIL]` |

### Keyword-Based Redaction

Scans each line for sensitive keywords:
```
password, passwd, secret, token, api_key, apikey, access_key,
private_key, client_secret, auth_token, bearer, credential, ...
```

If a keyword appears before `=` or `:` with a value ≥ 4 chars, the value is replaced:
```
MONGO_PASSWORD=hunter2  →  MONGO_PASSWORD=[REDACTED]
auth_token: eyJabc...   →  auth_token: [REDACTED]
```

### Order Matters

Pattern-based redaction runs first, keyword-based second. This means specific patterns (AWS keys, JWTs) get their typed labels, and the fallback keyword scan catches anything the patterns missed.

---

## Stage 6: Batch Embedding

**Package:** `internal/embedding`
**Function:** `embedder.EmbedBatch(ctx, texts) → ([][]float32, error)`

All sections for a single page are embedded in one batch call.

### How Batch Embedding Works

```
Sections from page: ["## Auth\n\nJWT tokens...", "## Security\n\nHTTPS required..."]
                                │
                                ▼
                    LlamaEmbedder.EmbedBatch()
                                │
                    ┌───────────▼───────────┐
                    │  Sub-batch of ≤ 4     │  ← maxBatchSize = 4
                    │                       │     (KV cache constraint)
                    │  POST /v1/embeddings  │
                    │  to llama-server:7433 │
                    │                       │
                    │  Body: {              │
                    │    "input": [text₁,   │
                    │             text₂],   │
                    │    "model": "..."     │
                    │  }                    │
                    └───────────┬───────────┘
                                │
                    Response: [{embedding: [0.12, -0.34, ...]},
                               {embedding: [0.56, 0.78, ...]}]
                                │
                                ▼
                    [][]float32 — one 1024-dim vector per section
```

**Sub-batching:** If a page produces > 4 sections, they're split into groups of 4 and each group is embedded in a separate HTTP call. Results are concatenated.

**Why batch?** A page with 8 sections would require 8 HTTP round-trips with `Embed()` but only 2 with `EmbedBatch()`. The network overhead of each call (10-50ms) adds up.

---

## Stage 7: Storage & Provenance

### Memory Construction

Each section becomes a `Memory` document:

```go
Memory{
    Content:   "## Authentication\n\nJWT tokens expire...",
    Embedding: [0.12, -0.34, ...],   // 1024-dim vector
    Source:    "source:company-docs|https://wiki.example.com/docs/auth",
    Metadata: map[string]any{
        "source_name": "company-docs",
        "page_url":    "https://wiki.example.com/docs/auth",
    },
}
```

`Insert()` adds `created_at: time.Now()`, `hit_count: 0`, `quality_score: 0`.

### Source Label Format

```
"source:" + name + "|" + page_url_or_filename
```

The pipe `|` separator enables:
- **Page-level deletion:** `DeleteMemoriesBySource("source:docs|https://wiki.example.com/docs/auth")` — when one page changes
- **Source-level deletion:** `DeleteMemoriesBySource("source:docs")` uses regex prefix matching — when the entire source is removed
- **Page-level counting:** `CountBySource("source:docs|https://wiki.example.com/docs/auth")` — for unchanged-page memory counts

### Source Page Recording

After storing all memories for a page:

```go
UpsertSourcePage(SourcePage{
    SourceID:    sourceID,
    URL:         page.URL,
    ContentHash: sha256Hash,
})
```

This creates or updates the record used by change detection on the next crawl.

### Source Status

After all pages are processed:

```go
UpdateSourceStatus(sourceID, "ready", "", len(pages), memoryCount)
```

Status transitions: `""` → `"crawling"` (set by API before calling IngestSource) → `"ready"` or `"error"`

---

## Design Decisions & Rationale

### Why no dedup in the ingest pipeline?

The write pipeline checks every new memory against the store (cosine ≥ 0.92 = skip). The ingest pipeline does not. Why?

**Page-level delete-and-recreate** makes per-memory dedup unnecessary. When a page changes, ALL its old memories are deleted before new ones are created. There's nothing to dedup against.

For unchanged pages, we skip the entire page (change detection). There's no scenario where a duplicate would be created.

### Why heading-based grouping instead of topic detection?

The write pipeline uses `detectTopicGroups()` (cosine similarity between adjacent chunks) to find topic boundaries. The ingest pipeline uses `groupByHeading()` instead. Why?

1. **Documentation has explicit structure.** Headings already define topic boundaries — we don't need to infer them.
2. **Heading grouping is deterministic.** The same input always produces the same output. Topic detection depends on embedding similarity, which can vary slightly.
3. **Heading grouping produces better provenance.** Each memory maps to a named section, making it easy to trace back to the source.

### Why no LLM synthesis in the ingest pipeline?

The write pipeline optionally uses LLM synthesis to distill agent conversations. The ingest pipeline does not. Why?

1. **Documentation is already well-written.** It doesn't need distillation — it needs faithful representation.
2. **Scale.** A 500-page wiki might produce 2000 sections. LLM calls at $0.01 each = $20 per crawl. Not viable for hourly refreshes.
3. **Fidelity.** Synthesis might lose technical details that keyword search would find. The raw text is the most faithful representation.

### Why no content scoring in the ingest pipeline?

The write pipeline scores chunks against quality/noise prototypes. The ingest pipeline does not. Why?

**Ingested content is assumed high-quality.** A user explicitly chose to ingest this documentation. Unlike agent conversations (which are 80%+ noise), documentation is the signal. Scoring would add latency without meaningful filtering.

### Why redact before embedding (not before storage)?

Secrets extracted from the text affect the embedding vector. If we redacted only the stored text but embedded the original (with secrets), the vector would encode information about the secret's content, potentially enabling side-channel recovery via nearest-neighbor search.

By redacting before embedding, the vector captures the semantic meaning of the surrounding context without any trace of the secret's value.

---

## Comparison: Ingest vs. Write Pipeline

| Stage | Ingest Pipeline | Write Pipeline |
|-------|----------------|---------------|
| **Input** | External docs (crawled sites, uploaded files) | Agent conversations (proxy capture, MCP stores) |
| **Preprocessing** | HTML stripping via `ExtractText()` | Strip `<think>` blocks, 50K char truncation |
| **Chunking** | `ChunkStructured()` → typed segments with heading metadata | `Chunk()` → text chunks (no metadata) |
| **Grouping** | `groupByHeading()` — deterministic, heading-based | `detectTopicGroups()` — cosine similarity between adjacent chunks |
| **Synthesis** | None — raw text is the best representation | Optional LLM synthesis via `Synthesize()`, `SynthesizeQA()` |
| **Content scoring** | None — ingested content is assumed high quality | `ContentScorer` with quality/noise prototype embeddings |
| **Dedup** | None — page-level delete-and-recreate handles it | Per-chunk cosine ≥ 0.92 check against store |
| **Source extension** | Not applicable | Tags new memories similar to source memories (cosine ≥ 0.75) |
| **Change detection** | SHA-256 per page/file | Not applicable (each response is new) |
| **Redaction** | Yes — `redact.Clean()` per section | Yes — `redact.Clean()` per chunk |
| **Noise filtering** | Drop sections < 30 chars | Drop chunks < 20 chars or < 40% alphanumeric |
| **Pre-LLM gates** | None — ingested content is assumed worth embedding | 3-stage: QuickFilter → length gate (< 80 chars) → content score gate (< 0.35) |
| **Adaptive learning** | None | Rejection store feeds noise prototypes back into content scorer |
| **Embedding** | Batch per page (all sections in one call) | Single or batch per response |
| **Execution** | Async goroutine, 30-min timeout | Async goroutine, fire-and-forget |

### The Key Insight

The ingest pipeline trusts its input quality (curated docs) and focuses on **structure preservation** and **efficient updates**.

The write pipeline faces hostile input quality (80%+ noise) and focuses on **quality filtering**, **synthesis**, and **deduplication**.

Both produce the same output type (`Memory` with content, embedding, source, metadata) and use the same chunker core. The divergence is all about fitness for their respective input characteristics.

---

## Future Work

Potential enhancements identified during this review:

1. **Cross-page section linking** — When two pages reference the same concept, link their memories via metadata. Enables "see also" style retrieval.

2. **Section-level change detection** — Instead of page-level delete-and-recreate, hash individual sections to minimize re-embedding on small edits. Trade-off: significant complexity for marginal embedding cost savings.

3. **Source-extension in ingest** — Currently only the write pipeline tags memories as extending source content. The ingest pipeline could do this between sources (e.g., wiki A's memory extends wiki B's coverage of the same topic).

4. **Parallel page processing** — Currently pages are processed sequentially within a source. Parallelizing with a worker pool would reduce total ingest time for large crawls.

5. **Content-type awareness** — Currently only `text/html` pages are processed. Supporting PDF, Markdown files served via HTTP, and other content types would expand the ingestible universe.

6. **Configurable chunker budget** — The 512-token budget is hardcoded. Some knowledge domains (API reference docs with many short entries) might benefit from smaller chunks, while narrative documentation might benefit from larger ones.
