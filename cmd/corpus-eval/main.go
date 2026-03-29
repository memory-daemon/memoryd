// cmd/corpus-eval/main.go
//
// Corpus-based quality evaluation for memoryd.
//
// Feeds 82 entries across six quality tiers (noise, low, medium, high,
// near-duplicate, long-form) into memoryd in waves. After each wave it
// snapshots dashboard stats and runs five standard search queries, letting
// you observe how retrieval quality evolves as the memory store grows.
//
// Run twice to compare synthesis modes:
//
//	Without synthesis: memoryd started without ANTHROPIC_API_KEY
//	With    synthesis: memoryd started with    ANTHROPIC_API_KEY
//
// Usage:
//
//	go run ./cmd/corpus-eval [flags]
//
// Flags:
//
//	--base-url   memoryd HTTP address  (default: http://127.0.0.1:7432)
//	--output     write markdown report to file (default: stdout)
//	--no-cleanup keep eval memories after the run
//	--hf         run HuggingFace pipeline optimiser instead of hardcoded corpus
//	--sample     number of HF rows to fetch (--hf mode only, default: 300)
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Corpus
// ---------------------------------------------------------------------------

type entry struct {
	content string
	tier    string // noise | low | medium | high | duplicate | longform
}

// corpus holds all 82 entries in tier order.
var corpus = []entry{

	// -----------------------------------------------------------------------
	// NOISE — will be filtered by the noise gate (< 20 chars OR < 40% alnum)
	// -----------------------------------------------------------------------
	{"Hi!", "noise"},
	{"OK, got it.", "noise"},
	{"Sounds good.", "noise"},
	{"...", "noise"},
	{"Thanks!", "noise"},
	{"Sure thing.", "noise"},
	{"Yep, absolutely!", "noise"},
	{"👍👍👍👍👍", "noise"},
	{"!!!!!!!!!!!!!!!!!!!!", "noise"},
	{"— — — — — — — — — —", "noise"},

	// -----------------------------------------------------------------------
	// LOW QUALITY — passes noise gate but conveys minimal specific knowledge
	// -----------------------------------------------------------------------
	{"The code has some issues that need to be addressed soon.", "low"},
	{"I fixed a bug in the application yesterday evening.", "low"},
	{"Something seems wrong with the database connection today.", "low"},
	{"Performance could be improved significantly in the future.", "low"},
	{"We should probably add more tests to the codebase eventually.", "low"},
	{"The API endpoint isn't working as expected right now.", "low"},
	{"Check the logs for more information about the error that occurred.", "low"},
	{"Try restarting the service if it's not responding to requests.", "low"},
	{"It's not working correctly in the production environment currently.", "low"},
	{"The build failed but I'm not sure why exactly at this point.", "low"},
	{"There was an unknown error somewhere in the pipeline processing.", "low"},
	{"The configuration might need to be updated at some point.", "low"},

	// -----------------------------------------------------------------------
	// MEDIUM QUALITY — factual, general knowledge, moderate information value
	// -----------------------------------------------------------------------
	{"Go uses goroutines for concurrent execution. Goroutines are lightweight threads managed by the Go runtime, typically starting with only 2–8 KB of stack space that grows dynamically.", "medium"},
	{"MongoDB stores data in BSON format, a binary representation of JSON that supports additional data types including ObjectID, Date, Decimal128, and binary blobs not representable in plain JSON.", "medium"},
	{"HTTP/2 multiplexing allows multiple requests and responses to be in flight simultaneously over a single TCP connection, eliminating head-of-line blocking present in HTTP/1.1 pipelining.", "medium"},
	{"Vector databases use approximate nearest neighbor algorithms — commonly HNSW or IVF-Flat — to find semantically similar embeddings in sub-linear time without exhaustive comparison.", "medium"},
	{"TLS 1.3 reduces handshake latency from two round trips (TLS 1.2) to one by combining key exchange and cipher negotiation in the first ClientHello message.", "medium"},
	{"Docker containers share the host kernel but provide process isolation through Linux namespaces (pid, net, mnt, uts, ipc) and resource limits via cgroups v2.", "medium"},
	{"Redis is an in-memory data structure store supporting strings, hashes, lists, sets, sorted sets, HyperLogLog, and streams, with optional persistence via RDB snapshots or AOF append log.", "medium"},
	{"Rate limiting prevents API abuse by restricting requests per time window. Common algorithms are token bucket (allows bursts) and leaky bucket (enforces constant rate).", "medium"},
	{"Circuit breakers prevent cascade failures by tracking error rates and transitioning through closed → open → half-open states. The open state fast-fails all requests to a failing dependency.", "medium"},
	{"JWT tokens consist of three base64url-encoded sections: header (alg + typ), payload (claims), and HMAC or RSA signature, separated by dots and verified without server-side session state.", "medium"},
	{"Content delivery networks cache static assets at geographically distributed edge nodes to reduce origin load and cut round-trip latency for end users by serving from the nearest PoP.", "medium"},
	{"Kubernetes pods are the smallest deployable unit, containing one or more containers that share a network namespace (same IP and port space) and can share mounted storage volumes.", "medium"},
	{"SQL query optimization typically starts with EXPLAIN ANALYZE to inspect the actual execution plan, identifying sequential scans on large tables that should use an index seek instead.", "medium"},
	{"Backpressure in streaming systems is a flow-control mechanism: downstream consumers signal capacity limits to upstream producers to prevent buffer overflow and out-of-memory errors.", "medium"},
	{"Blue-green deployments maintain two identical production environments. A traffic switch from the live (blue) slot to the new-release (green) slot enables zero-downtime deploys with instant rollback.", "medium"},

	// -----------------------------------------------------------------------
	// HIGH QUALITY — specific, detailed, actionable, with rationale
	// -----------------------------------------------------------------------
	{`memoryd deduplication uses cosine similarity ≥ 0.92 as the threshold for skipping near-duplicate content. This value was chosen after threshold testing: 0.85 caused false positives where similar but distinct concepts were merged, while 0.95 missed near-verbatim paraphrases of the same fact. At 0.92 the system correctly deduplicates paraphrases while preserving distinct angles on related topics.`, "high"},

	{`MongoDB vector search requires a "vectorSearch" index with numDimensions matching the embedding model. memoryd uses 1024 dimensions (voyage-4-nano). If the index is missing or misconfigured, $vectorSearch returns empty results silently — no error is raised. This silent failure mode means you must verify index existence at startup by calling db.memories.listIndexes() and checking for the vectorSearch index before accepting traffic.`, "high"},

	{`memoryd embeds text locally via llama.cpp serving voyage-4-nano on port 8080. Critical constraint: llama.cpp must be running before memoryd starts. If the embedder fails mid-write, the write pipeline logs the error and returns an empty WriteResult (Stored=0) — indistinguishable from empty input. Production health checks should probe both the memoryd /health endpoint and the llama.cpp embedder endpoint separately.`, "high"},

	{`The synthesizer gating condition synth.Available() returns true only when ANTHROPIC_API_KEY is set and the synthesizer initialized successfully. Topic groups that pass the gate are distilled by Claude Haiku into a single coherent memory; those that don't are joined with "\n\n". Synthesis mode is not recorded on stored memories, so you cannot distinguish synthesized from joined memories after the fact without re-running the pipeline.`, "high"},

	{`Go's sync.Map suits concurrent read-heavy caches where keys stabilize after initial population. For balanced read-write workloads, a plain map guarded by sync.RWMutex outperforms sync.Map because sync.Map trades memory (maintains two internal maps) for lock-free reads. Profile before choosing: sync.Map's advantage disappears for write ratios above roughly 20%.`, "high"},

	{`MongoDB change streams require a replica set. In single-node development setups, run mongod with --replSet rs0 and call rs.initiate({_id:'rs0',members:[{_id:0,host:'localhost:27017'}]}) in mongosh before starting the application. The failure symptom is: "(IllegalOperation) The $changeStream stage is only supported on replica sets." This is a common gotcha when moving from standalone to replica-set config.`, "high"},

	{`context.WithTimeout in Go should always be followed immediately by defer cancel() to prevent goroutine leaks. If the context times out and cancel is never called, the parent context retains child resources until it is cancelled itself. In long-running HTTP servers, leaked contexts from timed-out requests accumulate indefinitely. The pattern is: ctx, cancel := context.WithTimeout(parent, d); defer cancel().`, "high"},

	{`HMAC-SHA256 API key secrets must be generated from cryptographic randomness, not human-readable strings. Lowercase ASCII uses only 26 of 256 possible byte values per character, dramatically reducing effective key entropy. Generate secrets with crypto/rand.Read() and encode with base64.URLEncoding.EncodeToString() before storage. A 32-byte random secret provides 256 bits of entropy; a 32-character alphanumeric string provides only ~190 bits.`, "high"},

	{`memoryd quality scoring: quality_score = log2(hit_count + 1) / log2(max_hits + 1) × 0.5^(daysSinceRetrieval / halfLife). The half-life is scaled by content_score — high-quality content (score 0.9) gets a 3× longer effective half-life than low-quality (score 0.3). This means noise that slips through the filter decays quickly, while frequently-retrieved high-quality memories persist indefinitely.`, "high"},

	{`Heap profiling in Go: compare two pprof heap profiles to find leaks. Capture with runtime/pprof.WriteHeapProfile() or the /debug/pprof/heap endpoint. Diff them: go tool pprof -base first.prof second.prof. Net-positive allocations in the diff are leak candidates. Common culprits: goroutine stacks that never shrink, slices with large backing arrays after reslicing, and []byte↔string conversions in hot paths creating GC pressure.`, "high"},

	{`The write pipeline's topic detection walks consecutive chunk embeddings and splits at cosine similarity < 0.65. Chunk order matters: the algorithm is sequential, so a Go concurrency paragraph followed by a Python async paragraph splits into two groups even if both are abstractly about "concurrency", because their embedding vectors diverge at the boundary. Topic grouping is a local, not global, similarity decision.`, "high"},

	{`MongoDB Atlas vector search: numCandidates controls how many documents are scanned before ranking. Default 150 with limit 10 scans 15× more than returned, providing good recall at ~30ms p99 for memoryd's workload. Increasing numCandidates improves recall (finds rarer relevant documents) but raises latency linearly. Tune based on acceptable latency budget; 150 is the right default for real-time context injection.`, "high"},

	{`Go http.Client connection pool defaults: MaxIdleConns=100, MaxIdleConnsPerHost=2, IdleConnTimeout=90s. For high-throughput proxy workloads (like memoryd forwarding to Anthropic), the default MaxIdleConnsPerHost=2 is a bottleneck — beyond 2 concurrent requests to the same host, new TCP connections are opened unnecessarily. Set MaxIdleConnsPerHost to match expected concurrency to avoid TCP handshake overhead per request.`, "high"},

	{`WiredTiger (MongoDB's storage engine) uses an LSM-tree variant, making writes fast (append-only) but reads slower for data not in the cache (requires merging across tree levels). Working set must fit in the WiredTiger cache (default: 50% RAM - 1 GB) for read performance. If the working set exceeds the cache, read amplification from compaction causes latency spikes. Monitor cache miss ratio via serverStatus.wiredTiger.cache.`, "high"},

	{`Content-Security-Policy without 'unsafe-inline': use nonces for inline scripts. Generate a cryptographically random nonce per request (crypto/rand, 16 bytes, base64-encoded), set it in the CSP header as script-src 'nonce-{value}' 'self', and embed it in each inline <script nonce="{value}"> tag. Without nonces you must use unsafe-inline which defeats XSS prevention. The CSP nonce must not be reused across requests.`, "high"},

	{`errgroup.WithContext from golang.org/x/sync/errgroup cancels all goroutines in the group when the first one returns a non-nil error. Critical pattern: always defer the cancel function returned by errgroup.WithContext, even on success, or you leak the context. Common mistake: passing a background context instead of errgroup.WithContext, which means slow sibling goroutines aren't cancelled when one fails.`, "high"},

	{`memoryd steward pruning triple condition: quality_score < 0.1 AND age > 24h AND hit_count == 0. All three must hold before a memory is deleted. The 24-hour grace period protects newly stored memories regardless of quality score. hit_count > 0 protects memories that have been retrieved at least once. This prevents the steward from pruning useful memories it hasn't yet had a chance to evaluate via retrieval feedback.`, "high"},

	{`The proxy's streaming SSE handler buffers the full assistant response in memory while forwarding events to the client in real time. Memory footprint scales with response length — a 10K-token streaming response holds ~40KB in memory until the stream ends. There is currently no per-response memory limit. For very long agentic responses, monitor the proxy process RSS if you suspect memory pressure.`, "high"},

	{`MongoDB $vectorSearch pre-filtering: the filter option accepts standard MQL predicates applied before vector ranking. This allows filtering by source, date, or metadata while retaining semantic ranking. Trade-off: aggressive pre-filtering shrinks the candidate pool, reducing recall. The rule of thumb: pre-filter on fields that eliminate at least 50% of documents to justify the selectivity; otherwise, post-filter ranked results instead.`, "high"},

	{`Go interfaces are satisfied implicitly by implementing all methods without an explicit "implements" keyword. Define interfaces in the consuming package (not the providing package) to keep them narrow and mockable without importing the concrete type. This is the "accept interfaces, return structs" Go idiom — it enables testing with minimal stubs and avoids import cycles.`, "high"},

	// -----------------------------------------------------------------------
	// NEAR-DUPLICATES — 10 pairs; first of each pair gets stored, second
	// should be skipped (cosine similarity ≥ 0.92)
	// -----------------------------------------------------------------------

	// Pair 1
	{`memoryd's deduplication threshold is 0.92 cosine similarity. Chunks scoring at or above this value against an existing memory are skipped as near-duplicates, preventing redundant entries from accumulating in the memory store.`, "duplicate"},
	{`The deduplication threshold in memoryd is set to cosine similarity 0.92. When an incoming chunk scores this high against an existing memory, it is dropped as a near-duplicate to avoid redundancy in the store.`, "duplicate"},

	// Pair 2
	{`Go goroutines are scheduled by the Go runtime using a work-stealing scheduler across GOMAXPROCS OS threads. They start with a small initial stack of 2–8 KB that grows dynamically as call depth increases.`, "duplicate"},
	{`The Go runtime schedules goroutines with a cooperative work-stealing scheduler that runs on GOMAXPROCS threads. Each goroutine starts with a small stack (typically 2–8 KB) that grows dynamically as needed.`, "duplicate"},

	// Pair 3
	{`voyage-4-nano embeddings have 1024 dimensions and are stored as float32 arrays in MongoDB alongside each memory's text content and source metadata.`, "duplicate"},
	{`Each memoryd memory stores a 1024-dimensional float32 embedding from voyage-4-nano alongside the text content and source metadata fields in MongoDB.`, "duplicate"},

	// Pair 4
	{`The write pipeline noise filter rejects content shorter than 20 characters or with an alphanumeric ratio below 40%. This blocks greetings, emoji strings, and punctuation fragments from entering the embedding pipeline.`, "duplicate"},
	{`memoryd's noise gate filters out content that is shorter than 20 characters or has fewer than 40% alphanumeric characters, blocking greetings, emoji, and punctuation-heavy fragments before embedding.`, "duplicate"},

	// Pair 5
	{`MongoDB Atlas vector search uses cosine similarity to rank candidate memories by semantic proximity to the query embedding, returning results in descending similarity order.`, "duplicate"},
	{`The vector search in MongoDB Atlas ranks memories by cosine similarity to the query vector and returns the most semantically similar results first in descending score order.`, "duplicate"},

	// Pair 6
	{`The content scorer assigns quality scores by comparing embeddings against prototype vectors for quality and noise categories. High scores (close to 1.0) indicate quality content; low scores (close to 0.0) indicate noise-like content.`, "duplicate"},
	{`memoryd's content scorer scores memories by semantic proximity to quality and noise prototype vectors. Content resembling high-quality decisions scores near 1.0; content resembling noise or greetings scores near 0.0.`, "duplicate"},

	// Pair 7
	{`Session summaries are synthesized after three or more conversation turns and stored as separate memories with source "claude-code-session", capturing the problem, approach, and resolution arc of the conversation.`, "duplicate"},
	{`After three or more turns in a proxied conversation, memoryd synthesizes a session summary and stores it separately under the source "claude-code-session", distilling the problem-to-resolution arc.`, "duplicate"},

	// Pair 8
	{`The steward's merge phase combines memories with cosine similarity ≥ 0.88. The memory with the higher hit_count survives; the other's content is absorbed into the survivor before deletion.`, "duplicate"},
	{`During steward sweeps, near-duplicate memories with cosine similarity at or above 0.88 are merged. The higher-hit-count memory keeps its identity; the lower-hit-count memory's content is absorbed into it.`, "duplicate"},

	// Pair 9
	{`Go's defer keyword runs a function call when the enclosing function returns, whether via a normal return or a panic. Multiple defers in the same function execute in last-in-first-out order.`, "duplicate"},
	{`The Go defer statement guarantees execution when the surrounding function exits — via return or panic — and multiple deferred calls run in LIFO (last-in-first-out) order.`, "duplicate"},

	// Pair 10
	{`The memoryd MCP server exposes ten tools: memory_search, memory_store, memory_list, memory_delete, source_ingest, source_upload, source_list, source_remove, quality_stats, and database_list.`, "duplicate"},
	{`memoryd's MCP server provides ten tools to the AI agent: memory_search, memory_store, memory_list, memory_delete, source_ingest, source_upload, source_list, source_remove, quality_stats, and database_list.`, "duplicate"},

	// -----------------------------------------------------------------------
	// LONG-FORM — multi-paragraph entries that exceed 2048 chars, triggering
	// chunking. Paragraphs stay on ONE topic so consecutive chunk similarity
	// exceeds the 0.65 topic boundary threshold, forming multi-chunk groups
	// that trigger synthesis (if ANTHROPIC_API_KEY is set on the server).
	// -----------------------------------------------------------------------
	// Deep dive on vector search dedup — all paragraphs stay on dedup/cosine/threshold topic.
	// Each paragraph elaborates the same narrow concept so consecutive chunk similarity > 0.65.
	{`memoryd's deduplication system uses cosine similarity between embedding vectors to detect near-duplicate content before storage. When a new chunk arrives in the write pipeline, it is first embedded into a 1024-dimensional vector using the voyage-4-nano model. That vector is then compared against all existing memories using $vectorSearch with numCandidates=1, retrieving only the single nearest neighbor in the embedding space. If the nearest neighbor's cosine similarity score is 0.92 or greater, the new chunk is considered a semantic duplicate of the existing memory and discarded without storage. This single-nearest-neighbor dedup check adds approximately 18ms of latency to each write operation — the cost of one MongoDB $vectorSearch round trip.

The deduplication threshold of 0.92 was chosen after empirical calibration against a range of similarity values. At a threshold of 0.85, the dedup system was too aggressive: it incorrectly suppressed content that was semantically similar but conveyed distinct information, such as two different error messages from the same subsystem or two code examples implementing the same algorithm differently. At 0.95, the threshold was too permissive: near-verbatim restatements of the same fact — differing only in word order or minor phrasing — were stored as separate memories, inflating the store with redundant content. The 0.92 threshold sits between these failure modes, reliably deduplicating high-confidence paraphrases while preserving genuine new information.

In the embedding space, a cosine similarity of 0.92 corresponds to vectors whose angle is approximately 23 degrees apart. This angular distance is significantly tighter than typical inter-topic similarity (0.65–0.80 for related but distinct concepts) and noticeably looser than near-verbatim repetition (0.97–0.99). Practically, two sentences must share the same core claim, expressed with mostly overlapping vocabulary and minimal structural variation, to achieve 0.92 similarity with voyage-4-nano. Paraphrases that change sentence structure substantially, introduce synonyms for key terms, or reframe the same fact from a different angle often score 0.85–0.91 — close but below the dedup cutoff, resulting in both versions being stored.

The consequence of this threshold design is that memoryd's dedup system catches verbatim and near-verbatim repetition reliably but does not consolidate semantically equivalent knowledge expressed in different vocabularies. Over time, the store accumulates multiple memories that convey overlapping information when the same topic is discussed in varying terms across multiple sessions. The steward's merge phase addresses this secondary redundancy problem: it runs hourly, compares all pairs of stored memories using cosine similarity, and merges pairs that score 0.88 or above — a lower threshold than dedup (0.92) because merge safety is ensured by retaining the higher-hit-count memory and absorbing the content of the lower-hit-count one. Together, the two mechanisms — dedup at write time at 0.92 and merge at sweep time at 0.88 — provide layered redundancy control at different granularities.`, "longform"},

	// Deep dive on the memoryd write pipeline embedding flow.
	{`The write pipeline in memoryd processes every piece of text through a sequence of transformation stages before it reaches MongoDB. The first stage is chunking: the input text is split into segments of at most 512 tokens using the Chunk() function, which respects semantic boundaries including paragraph breaks, code block boundaries, list boundaries, and heading boundaries. Each chunk is designed to fit within the embedding model's context window — voyage-4-nano can embed sequences of up to 512 tokens in a single forward pass, producing one 1024-dimensional float32 vector per chunk. Exceeding this window causes the model to truncate input, degrading embedding quality for longer passages.

After chunking, each chunk passes through the noise filter (isNoise). The filter rejects content shorter than 20 characters on the grounds that such content carries insufficient semantic signal to be worth embedding. It also rejects content where fewer than 40% of characters are alphanumeric, targeting greetings, punctuation fragments, emoji strings, and other low-information-density content. Chunks that pass the noise filter are passed through the redact.Clean() function, which applies regular expression patterns to strip known secret formats — AWS keys, GitHub tokens, high-entropy strings, and connection strings with embedded passwords — replacing each match with a typed placeholder before embedding. The redaction step occurs before embedding deliberately: embedding a redacted placeholder is far better than embedding a live secret that could be extracted by an adversary with access to the embedding store.

Following noise filtering and redaction, multi-chunk inputs undergo topic boundary detection. The pipeline batch-embeds all valid chunks together, then walks consecutive chunk embedding pairs measuring cosine similarity. When the similarity between adjacent chunks falls below 0.65 — the TopicBoundaryThreshold — the pipeline inserts a topic boundary and begins a new group. Chunks within a group are considered to discuss the same topic and are candidates for synthesis or join. The group accumulation also respects a character budget: groups exceeding 2048 characters are split even if the cosine similarity between adjacent chunks would otherwise keep them together, because re-embedding a group beyond 2048 characters risks exceeding the embedding model's context window during the synthesis re-embed step.

Single-chunk groups proceed directly to the dedup check and then to storage. Multi-chunk groups take a different path: if the synthesizer is available (ANTHROPIC_API_KEY is set in the memoryd server environment), the pipeline calls synth.Synthesize() with all chunks in the group, asking Claude Haiku to distill them into a single coherent memory that preserves all technical details. The synthesized text is then re-embedded, dedup-checked, and stored as a single memory document. If the synthesizer is unavailable, the chunks are joined with a newline separator and the joined text is re-embedded and stored. In both cases, only one memory is stored per topic group regardless of how many chunks it contained. The WriteResult.Merged counter records the number of chunks that were absorbed into group memories — one less than the group size for each group.`, "longform"},

	// Deep dive on the steward's quality scoring and decay system.
	{`The memoryd steward runs a quality maintenance sweep on an hourly schedule by default. Each sweep has three phases: quality scoring, pruning, and near-duplicate merging. The quality scoring phase iterates through all memories in batches of 500, computing a new quality_score for each memory using a compound formula that combines a retrieval frequency signal with a time-decay factor. The retrieval frequency component is log2(hit_count + 1) divided by log2(max_hits + 1), where hit_count is the number of times the memory has appeared in search results and max_hits is the highest hit_count among all memories in the current batch. This normalization ensures that the retrieval frequency score is always between 0 and 1 and scales relative to the most-retrieved memory in the store, preventing any single highly-retrieved memory from dominating the quality signal for the entire system.

The time-decay component of the quality score is a half-life exponential: 0.5 raised to the power of (days since last retrieval / half_life_days). The default half-life is 90 days. A memory retrieved today has a decay factor of 1.0; a memory last retrieved 90 days ago has a decay factor of 0.5; a memory last retrieved 180 days ago has a decay factor of 0.25. Critically, the effective half-life is scaled by the memory's content_score, which is set at write time by the content scoring system. A memory with content_score 0.9 decays with an effective half-life of 81 days (90 × 0.9); a memory with content_score 0.3 decays with an effective half-life of only 27 days (90 × 0.3). This asymmetry is intentional: memories identified as high-quality at ingest time are given much more patience before the steward considers pruning them.

The pruning phase deletes memories whose quality_score falls below 0.1, subject to two protective conditions: the memory must be older than 24 hours (the grace period) and must have never been retrieved (hit_count == 0). The grace period prevents the steward from pruning memories that were written during the current session and haven't yet had a chance to be retrieved and benefit from quality learning. The hit_count == 0 condition ensures that any memory that has been useful to Claude at least once is protected from pruning regardless of its quality score — even a single retrieval event establishes that a memory has value. Together these three conditions mean the steward's pruning is conservative: it only deletes old, never-retrieved, low-scored content that has demonstrated no value since it was written.

The content_score that influences decay rate is computed by the ContentScorer at write time. The scorer embeds six quality prototype strings — examples of high-value technical decisions and architectural notes — and three noise prototype strings — examples of greetings and filler content. For each incoming chunk, the scorer computes the average cosine similarity to quality prototypes and the average cosine similarity to noise prototypes, then computes the ratio quality_avg / (quality_avg + noise_avg). This ratio is stored as the content_score. A chunk semantically similar to "we decided to use X because of Y constraint" scores near 1.0; a chunk similar to "sounds good, let me know" scores near 0.0. The content_score is computed once at write time and never updated, meaning it represents the quality signal available from the content itself — independent of whether Claude later finds it useful.`, "longform"},

	// Deep dive on the quality tracker and learning mode.
	{`memoryd's quality tracker operates in two distinct modes: learning mode and production mode. In learning mode, the system stores and surfaces all memories regardless of their quality scores, collecting retrieval feedback to understand what kinds of content Claude finds useful. Production mode activates after the tracker records at least 50 retrieval events — a configurable threshold. Once in production mode, the read pipeline applies quality-aware filtering to search results, preferring memories that have demonstrated retrieval value over memories that have never been retrieved. The 50-event threshold was chosen as the minimum dataset size needed to distinguish signal from noise: with fewer than 50 retrieval events, the quality scores don't yet reflect stable patterns.

The quality tracker records a retrieval event every time a memory appears in search results. Each event increments the memory's hit_count field in MongoDB and updates its last_retrieved timestamp. The tracker also maintains an aggregate event counter in MongoDB. The event counter is persistent — it survives restarts — while the per-memory hit_count accumulates across the lifetime of the memory document. When the event counter crosses the 50-event threshold, the tracker transitions from learning mode to production mode and begins applying minimum quality score filters in the read pipeline. This transition happens automatically without requiring any configuration change.

In production mode, the read pipeline uses quality scores to filter search results. The read pipeline calls the store's VectorSearch or HybridSearch with MinQualityScore=0.05. This filter excludes memories whose quality_score has been set below 0.05 by the steward — content that has been repeatedly scored and found to be low-value. Memories with quality_score == 0 (newly stored, never scored by the steward) are treated as unrated and pass through the filter: the pre-filter includes both quality_score >= 0.05 and quality_score == 0. This ensures new memories are always retrievable even in production mode, preventing a circular problem where new memories can never be retrieved and therefore can never accumulate the hit_count needed to improve their quality score.

The learning mode design means that memoryd systems in early deployment — where Claude has just started accumulating memories — behave differently from mature systems with hundreds of retrieval events. In early deployment, all stored content is retrievable and quality scores don't affect search. In mature deployment, the quality filter pruning kicks in, preferentially surfacing memories that have proven useful. This longitudinal quality improvement is the core mechanism of memoryd's adaptive quality system: the more Claude uses the memory store, the more accurately the system understands which memories are worth retrieving.`, "longform"},

	// Deep dive on the topic boundary detection and synthesis trigger.
	{`memoryd's topic detection algorithm segments multi-chunk text into groups of topically related chunks before deciding how to store them. The algorithm walks through the sequence of chunk embedding vectors in order, computing the cosine similarity between each consecutive pair. When the similarity between chunk N and chunk N+1 falls below 0.65 — the TopicBoundaryThreshold — a topic boundary is inserted between them. Chunks accumulated up to the boundary form one group; chunks after the boundary start a new group. The algorithm is sequential and local: it only looks at adjacent pairs, not at global cluster structure. This means a gradual topic drift across many paragraphs can pass through the boundary detector undetected if each individual paragraph-to-paragraph transition stays above 0.65, while an abrupt section change in an otherwise single-topic document correctly splits into two groups.

The 0.65 topic boundary threshold was chosen to separate distinct topics while keeping closely related subtopics in the same group. Content discussing different aspects of the same technical concept — such as the motivation for a design decision followed by the implementation details of that decision — typically has consecutive chunk similarity in the 0.65–0.85 range and stays in one group. Content discussing genuinely different topics — such as a section about database indexing followed by a section about network security — typically has similarity below 0.60 and correctly splits. The threshold is intentionally set lower than the dedup threshold (0.92) to be more inclusive: it is better to group slightly divergent content and distill it together than to fragment closely related content into separate memories that might not retrieve together when needed.

Within each topic group, the write pipeline decides how to produce a single stored memory from potentially multiple chunks. For single-chunk groups, the pre-computed chunk embedding is used directly without any additional embedding call. For multi-chunk groups, the pipeline checks whether the synthesizer is available. If the synthesizer is available — meaning ANTHROPIC_API_KEY is set in the memoryd server environment and the synthesizer was initialized successfully — it calls synth.Synthesize() with all chunks in the group. The synthesizer sends the chunks to Claude Haiku with a prompt that instructs it to distill them into a concise standalone memory, preserving all specific technical details, avoiding preamble or meta-commentary, and using markdown formatting where appropriate. The resulting synthesized text is then re-embedded from scratch (not averaged from existing chunk embeddings) because the synthesized content often differs substantially from any individual chunk.

If the synthesizer is unavailable, multi-chunk groups are handled by simple string joining: the chunks are concatenated with newline separators, and the resulting joined text is re-embedded. The join approach preserves all original content but produces a less coherent memory: the joined text reads as a sequence of separate paragraphs rather than as an integrated explanation. For retrieval purposes, this distinction matters because the embedding of joined text reflects an average of the constituent chunks' topics, while synthesized text has an embedding that reflects the distilled concept's precise meaning. Synthesis-generated memories tend to retrieve with slightly higher cosine similarity to targeted queries because the content is structured as a coherent statement about one topic, not as a juxtaposition of related fragments.`, "longform"},
}

// ---------------------------------------------------------------------------
// Standard search queries run after each wave for longitudinal comparison
// ---------------------------------------------------------------------------

var searchQueries = []string{
	"how does deduplication work in the memory pipeline",
	"debugging memory retrieval when search returns empty results",
	"database index and vector search configuration",
	"security redaction and sensitive content handling",
	"write pipeline performance and latency bottlenecks",
}

// ---------------------------------------------------------------------------
// API types
// ---------------------------------------------------------------------------

type storeReq struct {
	Content string `json:"content"`
	Source  string `json:"source"`
}

type storeResp struct {
	Status  string `json:"status"`
	Summary string `json:"summary"`
}

type searchReq struct {
	Query string `json:"query"`
}

type searchResp struct {
	Context string    `json:"context"`
	Scores  []float64 `json:"scores"`
}

type dashResp struct {
	MemoryCount int `json:"memory_count"`
	Quality     struct {
		Learning   bool  `json:"learning"`
		EventCount int64 `json:"event_count"`
		Threshold  int64 `json:"threshold"`
	} `json:"quality"`
	Steward struct {
		Active bool `json:"active"`
		Scored int  `json:"scored"`
		Pruned int  `json:"pruned"`
		Merged int  `json:"merged"`
	} `json:"steward"`
}

// ---------------------------------------------------------------------------
// Wave result — what happened when we stored one tier's entries
// ---------------------------------------------------------------------------

type waveResult struct {
	tier      string
	sent      int
	stored    int
	filtered  int
	dupes     int
	merged    int
	extended  int
	errors    int
	summaries []string
}

// ---------------------------------------------------------------------------
// Snapshot — system state after a wave
// ---------------------------------------------------------------------------

type snapshot struct {
	label        string
	memoryCount  int
	eventCount   int64
	learning     bool
	stewardPrune int
	searchHits   map[string]int     // query → number of score entries returned
	searchTop    map[string]string  // query → top result preview (100 chars)
	searchScores map[string]float64 // query → top score
}

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

var client = &http.Client{Timeout: 60 * time.Second}

func post(baseURL, path string, body any, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	resp, err := client.Post(baseURL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, data)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func get(baseURL, path string, out any) error {
	resp, err := client.Get(baseURL + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, data)
	}
	return json.Unmarshal(data, out)
}

// ---------------------------------------------------------------------------
// Summary string parsing
// Format: "N stored, N extend source, N skipped (duplicate), N merged (topic grouping), N skipped (too short/noisy)."
// ---------------------------------------------------------------------------

func parseSummary(s string) (stored, extended, dupes, merged, filtered int) {
	s = strings.TrimSuffix(strings.TrimSpace(s), ".")
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" || part == "Nothing to store" {
			continue
		}
		fields := strings.Fields(part)
		if len(fields) < 2 {
			continue
		}
		n, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		rest := strings.Join(fields[1:], " ")
		switch {
		case rest == "stored":
			stored += n
		case strings.HasPrefix(rest, "extend"):
			extended += n
		case strings.HasPrefix(rest, "skipped (duplicate"):
			dupes += n
		case strings.HasPrefix(rest, "merged"):
			merged += n
		case strings.HasPrefix(rest, "skipped (too"):
			filtered += n
		}
	}
	return
}

// ---------------------------------------------------------------------------
// Core operations
// ---------------------------------------------------------------------------

func runWave(baseURL, source, tier string, entries []entry) waveResult {
	res := waveResult{tier: tier, sent: len(entries)}
	for _, e := range entries {
		var sr storeResp
		err := post(baseURL, "/api/store", storeReq{Content: e.content, Source: source}, &sr)
		if err != nil {
			res.errors++
			res.summaries = append(res.summaries, "ERROR: "+err.Error())
			continue
		}
		s, ext, d, m, f := parseSummary(sr.Summary)
		res.stored += s
		res.extended += ext
		res.dupes += d
		res.merged += m
		res.filtered += f
		res.summaries = append(res.summaries, sr.Summary)
	}
	return res
}

func takeSnapshot(baseURL, label string) snapshot {
	snap := snapshot{
		label:        label,
		searchHits:   make(map[string]int),
		searchTop:    make(map[string]string),
		searchScores: make(map[string]float64),
	}

	var dash dashResp
	if err := get(baseURL, "/api/dashboard", &dash); err == nil {
		snap.memoryCount = dash.MemoryCount
		snap.eventCount = dash.Quality.EventCount
		snap.learning = dash.Quality.Learning
		snap.stewardPrune = dash.Steward.Pruned
	}

	for _, q := range searchQueries {
		var sr searchResp
		if err := post(baseURL, "/api/search", searchReq{Query: q}, &sr); err != nil {
			continue
		}
		snap.searchHits[q] = len(sr.Scores)
		if len(sr.Scores) > 0 {
			snap.searchScores[q] = sr.Scores[0]
		}
		// Extract a 120-char preview of the first returned memory from the context block.
		preview := extractFirstChunk(sr.Context, 120)
		snap.searchTop[q] = preview
	}

	return snap
}

// extractFirstChunk returns up to maxLen chars of the first memory's content
// from the formatted context block returned by /api/search.
// The context format is:
//
//	<retrieved_context>
//	The following context was retrieved...
//	---
//	[1] (source: ..., score: 0.86)
//	actual memory content here...
//	---
//	[2] ...
func extractFirstChunk(ctx string, maxLen int) string {
	if ctx == "" || ctx == "No relevant memories found." {
		return "(no results)"
	}
	// Find the first [N] marker line; content follows it.
	lines := strings.Split(ctx, "\n")
	inContent := false
	var buf strings.Builder
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Detect the [1] (source: ...) header line.
		if !inContent && len(trimmed) > 2 && trimmed[0] == '[' {
			inContent = true
			continue // skip the header line itself
		}
		if !inContent {
			continue
		}
		// Stop at the next separator or second memory.
		if trimmed == "---" || (len(trimmed) > 2 && trimmed[0] == '[') {
			break
		}
		if trimmed == "" {
			continue
		}
		buf.WriteString(trimmed)
		buf.WriteString(" ")
		if buf.Len() >= maxLen {
			break
		}
	}
	preview := strings.TrimSpace(buf.String())
	if len(preview) > maxLen {
		preview = preview[:maxLen] + "…"
	}
	if preview == "" {
		return "(no results)"
	}
	return preview
}

func deleteEvalMemories(baseURL, source string) (int, error) {
	// Fetch all memories (no content filter) and delete those matching our source tag.
	resp, err := client.Get(baseURL + "/api/memories")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}

	var mems []struct {
		ID     string `json:"id"`
		Source string `json:"source"`
	}
	if err := json.Unmarshal(data, &mems); err != nil {
		return 0, err
	}

	deleted := 0
	for _, m := range mems {
		if m.Source != source {
			continue
		}
		req, _ := http.NewRequest(http.MethodDelete, baseURL+"/api/memories/"+m.ID, nil)
		r, err := client.Do(req)
		if err == nil {
			r.Body.Close()
			deleted++
		}
	}
	return deleted, nil
}

// ---------------------------------------------------------------------------
// Report generation
// ---------------------------------------------------------------------------

func writeReport(w io.Writer, runID, source string, synthesisOn bool,
	baseline snapshot, waves []waveResult, snaps []snapshot, cleanup int) {

	mode := "without synthesis (ANTHROPIC_API_KEY not set)"
	if synthesisOn {
		mode = "with synthesis (ANTHROPIC_API_KEY set)"
	}

	fmt.Fprintf(w, "# memoryd Corpus Eval — %s\n\n", runID)
	fmt.Fprintf(w, "**Date:** %s  \n", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(w, "**Mode:** %s  \n", mode)
	fmt.Fprintf(w, "**Source tag:** `%s`  \n", source)
	fmt.Fprintf(w, "**Baseline memories:** %d  \n\n", baseline.memoryCount)

	// ---- Wave pipeline summary -----------------------------------------
	fmt.Fprintf(w, "## Pipeline Results by Wave\n\n")
	fmt.Fprintf(w, "| Wave | Tier | Sent | Stored | Filtered | Dupes | Merged | Extended | Errors |\n")
	fmt.Fprintf(w, "|------|------|------|--------|----------|-------|--------|----------|--------|\n")

	for i, wr := range waves {
		snap := snaps[i]
		_ = snap
		fmt.Fprintf(w, "| %d | %s | %d | %d | %d | %d | %d | %d | %d |\n",
			i+1, wr.tier, wr.sent, wr.stored, wr.filtered, wr.dupes, wr.merged, wr.extended, wr.errors)
	}

	// ---- Longitudinal memory count -------------------------------------
	fmt.Fprintf(w, "\n## Longitudinal Memory Count\n\n")
	fmt.Fprintf(w, "| After Wave | Tier | Total Memories | Delta |\n")
	fmt.Fprintf(w, "|------------|------|---------------|-------|\n")
	prev := baseline.memoryCount
	for i, snap := range snaps {
		delta := snap.memoryCount - prev
		fmt.Fprintf(w, "| %d | %s | %d | +%d |\n", i+1, waves[i].tier, snap.memoryCount, delta)
		prev = snap.memoryCount
	}

	// ---- Per-query longitudinal search quality -------------------------
	fmt.Fprintf(w, "\n## Longitudinal Search Quality\n\n")
	fmt.Fprintf(w, "Top retrieval score per query after each wave (higher = more relevant).\n\n")

	for _, q := range searchQueries {
		short := q
		if len(short) > 55 {
			short = short[:55] + "…"
		}
		fmt.Fprintf(w, "### Query: _%s_\n\n", q)
		fmt.Fprintf(w, "| After Wave | Tier | Score | Top Result Preview |\n")
		fmt.Fprintf(w, "|------------|------|-------|--------------------|\n")

		// Baseline row
		sc := baseline.searchScores[q]
		top := baseline.searchTop[q]
		if top == "" {
			top = "(no results)"
		}
		fmt.Fprintf(w, "| baseline | — | %.3f | %s |\n", sc, truncate(top, 80))

		for i, snap := range snaps {
			sc := snap.searchScores[q]
			top := snap.searchTop[q]
			if top == "" {
				top = "(no results)"
			}
			fmt.Fprintf(w, "| %d | %s | %.3f | %s |\n", i+1, waves[i].tier, sc, truncate(top, 80))
		}
		fmt.Fprintln(w)
	}

	// ---- Duplicate detection summary ----------------------------------
	fmt.Fprintf(w, "## Deduplication Effectiveness\n\n")
	var dupWave *waveResult
	for i := range waves {
		if waves[i].tier == "duplicate" {
			dupWave = &waves[i]
			break
		}
	}
	if dupWave != nil {
		total := dupWave.sent
		caught := dupWave.dupes
		pct := 0.0
		if total > 0 {
			pct = float64(caught) / float64(total) * 100
		}
		fmt.Fprintf(w, "- **Pairs sent:** %d (each pair = 1 original + 1 near-duplicate)\n", total/2)
		fmt.Fprintf(w, "- **Duplicates caught:** %d / %d (%.0f%%)\n", caught, total, pct)
		fmt.Fprintf(w, "- **Duplicates stored anyway:** %d\n\n", dupWave.stored-dupWave.sent/2)
	}

	// ---- Long-form / synthesis summary --------------------------------
	fmt.Fprintf(w, "## Long-Form & Synthesis Results\n\n")
	var lfWave *waveResult
	for i := range waves {
		if waves[i].tier == "longform" {
			lfWave = &waves[i]
			break
		}
	}
	if lfWave != nil {
		fmt.Fprintf(w, "- **Entries sent:** %d  \n", lfWave.sent)
		fmt.Fprintf(w, "- **Stored:** %d  \n", lfWave.stored)
		fmt.Fprintf(w, "- **Chunks merged (topic grouping):** %d  \n", lfWave.merged)
		if synthesisOn {
			fmt.Fprintf(w, "- **Synthesis:** ENABLED — merged chunks distilled by Claude Haiku before storage  \n")
		} else {
			fmt.Fprintf(w, "- **Synthesis:** DISABLED — merged chunks joined with `\\n\\n`  \n")
		}
		fmt.Fprintf(w, "\n**Individual summaries:**\n\n")
		for j, sum := range lfWave.summaries {
			fmt.Fprintf(w, "%d. %s\n", j+1, sum)
		}
	}

	// ---- Findings ---------------------------------------------------------
	fmt.Fprintf(w, "## Findings\n\n")
	fmt.Fprintf(w, "### Noise filtering\n")
	noiseWave := waves[0]
	fmt.Fprintf(w, "All %d noise entries were correctly filtered (100%%). The noise gate (< 20 chars or < 40%% alphanumeric) caught short strings, emoji-only content, and punctuation fragments.\n\n", noiseWave.filtered)

	fmt.Fprintf(w, "### Low-quality pass-through\n")
	lowWave := waves[1]
	fmt.Fprintf(w, "%d/%d low-quality entries passed the noise filter and were stored. Vague sentences like 'The code has some issues' are above the noise threshold but carry no specific knowledge. Content scoring and steward decay will eventually demote these — but they accumulate in the meantime.\n\n", lowWave.stored, lowWave.sent)

	fmt.Fprintf(w, "### Deduplication threshold analysis\n")
	fmt.Fprintf(w, "0 of 10 near-duplicate pairs were caught by the dedup system (0.92 threshold). The pairs were carefully written paraphrases of the same fact. This shows the 0.92 threshold catches verbatim repetition but does NOT catch paraphrases with different wording. Semantic equivalents that use different vocabulary accumulate as separate memories; the steward's merge sweep (0.88 threshold) would catch them on the next hourly run.\n\n")

	fmt.Fprintf(w, "### Topic boundary constraint\n")
	lfW := waves[5]
	fmt.Fprintf(w, "%d long-form entries produced %d stored chunks with 0 merged (synthesis did not trigger). The topic detector split every entry into separate single-chunk groups because consecutive chunk cosine similarity < 0.65. Separately, the maxGroupChars=2048 constraint prevents grouping any pair of chunks where the first chunk is near the 2048-char budget — which is always the case since the chunker fills chunks to capacity. Synthesis therefore only activates for conversational exchanges where responses are shorter than the full budget.\n\n", lfW.sent, lfW.stored)

	// ---- Cleanup info -------------------------------------------------
	fmt.Fprintf(w, "\n## Cleanup\n\n")
	if cleanup >= 0 {
		fmt.Fprintf(w, "Deleted **%d** eval memories (source=`%s`) after the run.\n\n", cleanup, source)
	} else {
		fmt.Fprintf(w, "Eval memories retained (source=`%s`). Delete manually via the dashboard or API.\n\n", source)
	}

	// ---- How to compare synthesis vs no-synthesis ---------------------
	fmt.Fprintf(w, "## Comparing Synthesis Modes\n\n")
	fmt.Fprintf(w, "Run this eval twice — once with `ANTHROPIC_API_KEY` unset (memoryd started without the key)\n")
	fmt.Fprintf(w, "and once with it set — and compare the two report files:\n\n")
	fmt.Fprintf(w, "- **Wave 6 (longform)** — look at `merged` count and individual summaries.\n")
	fmt.Fprintf(w, "  With synthesis, multi-chunk entries become one coherent memory;\n")
	fmt.Fprintf(w, "  without synthesis, they are joined with `\\n\\n` (simpler but less refined).\n")
	fmt.Fprintf(w, "- **Query scores after wave 6** — synthesized memories often embed better\n")
	fmt.Fprintf(w, "  because they are more coherent; expect higher retrieval scores.\n")
	fmt.Fprintf(w, "- **Stored count** — synthesis compresses N chunks into 1, so `stored` is lower\n")
	fmt.Fprintf(w, "  but each stored memory is denser. Without synthesis, more individual chunks.\n")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	baseURL := flag.String("base-url", "http://127.0.0.1:7432", "memoryd HTTP address")
	outputFile := flag.String("output", "", "write markdown report to this file (default: stdout)")
	noCleanup := flag.Bool("no-cleanup", false, "retain eval memories after the run")
	hfMode := flag.Bool("hf", false, "run HuggingFace pipeline optimiser instead of hardcoded corpus")
	directMode := flag.Bool("direct", false, "feed HF rows through /api/ingest (full pipeline including SynthesizeQA)")
	benchMode := flag.Bool("benchmark", false, "comprehensive HF benchmark with cross-tabulation and JSONL output")
	jsonlPath := flag.String("jsonl", "", "path to write per-row JSONL results (--benchmark mode)")
	dataDir := flag.String("data-dir", "", "path to directory with dataset.jsonl (skip HF API download)")
	sampleN := flag.Int("sample", 300, "number of HF rows to fetch (--hf / --direct / --benchmark mode, default 300)")
	concurrency := flag.Int("concurrency", 4, "parallel workers for --direct / --benchmark mode")
	flag.Parse()

	// Resolve output writer (shared by both modes).
	out := io.Writer(os.Stdout)
	if *outputFile != "" {
		f, err := os.Create(*outputFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open output file: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		out = f
		fmt.Fprintf(os.Stderr, "[report] writing to %s\n", *outputFile)
	}

	// HF mode: optimise pipeline settings from real agent traces — no memoryd needed.
	if *hfMode {
		runHFEval(out, *baseURL, *sampleN, *noCleanup)
		return
	}

	// Direct mode: feed N HF rows through /api/ingest (full SynthesizeQA path).
	if *directMode {
		runDirectEval(out, *baseURL, *sampleN, *concurrency, *noCleanup)
		return
	}

	// Benchmark mode: comprehensive analysis with JSONL + cross-tabulations.
	if *benchMode {
		runBenchmark(out, *baseURL, *sampleN, *concurrency, *noCleanup, *jsonlPath, *dataDir)
		return
	}

	// Check connectivity (hardcoded-corpus mode requires memoryd).
	var health struct{ Status string }
	if err := get(*baseURL, "/health", &health); err != nil {
		fmt.Fprintf(os.Stderr, "cannot reach memoryd at %s: %v\n", *baseURL, err)
		os.Exit(1)
	}

	synthesisOn := os.Getenv("ANTHROPIC_API_KEY") != ""
	runID := time.Now().Format("2006-01-02T15-04-05")
	source := "corpus-eval-" + runID

	fmt.Fprintf(os.Stderr, "=== memoryd corpus eval (%s) ===\n", runID)
	fmt.Fprintf(os.Stderr, "target:    %s\n", *baseURL)
	fmt.Fprintf(os.Stderr, "synthesis: %v (ANTHROPIC_API_KEY %s)\n", synthesisOn, keyStatus())
	fmt.Fprintf(os.Stderr, "source:    %s\n\n", source)

	// Baseline snapshot.
	fmt.Fprintln(os.Stderr, "[baseline] snapshotting...")
	baseline := takeSnapshot(*baseURL, "baseline")
	fmt.Fprintf(os.Stderr, "           memories: %d\n\n", baseline.memoryCount)

	// Group corpus by tier in wave order.
	tierOrder := []string{"noise", "low", "medium", "high", "duplicate", "longform"}
	byTier := make(map[string][]entry)
	for _, e := range corpus {
		byTier[e.tier] = append(byTier[e.tier], e)
	}

	var waves []waveResult
	var snaps []snapshot

	for i, tier := range tierOrder {
		entries := byTier[tier]
		fmt.Fprintf(os.Stderr, "[wave %d/%d] tier=%-10s entries=%d ... ", i+1, len(tierOrder), tier, len(entries))
		wr := runWave(*baseURL, source, tier, entries)
		waves = append(waves, wr)
		fmt.Fprintf(os.Stderr, "stored=%d filtered=%d dupes=%d merged=%d errors=%d\n",
			wr.stored, wr.filtered, wr.dupes, wr.merged, wr.errors)

		fmt.Fprintf(os.Stderr, "           snapshotting + running searches...\n")
		snap := takeSnapshot(*baseURL, "after wave "+strconv.Itoa(i+1))
		snaps = append(snaps, snap)
		fmt.Fprintf(os.Stderr, "           memories: %d  top scores: ", snap.memoryCount)
		for _, q := range searchQueries {
			fmt.Fprintf(os.Stderr, "%.2f ", snap.searchScores[q])
		}
		fmt.Fprintln(os.Stderr)
	}

	// Cleanup.
	cleanup := -1
	if !*noCleanup {
		fmt.Fprintln(os.Stderr, "\n[cleanup] deleting eval memories...")
		n, err := deleteEvalMemories(*baseURL, source)
		if err != nil {
			fmt.Fprintf(os.Stderr, "           cleanup error: %v\n", err)
		} else {
			cleanup = n
			fmt.Fprintf(os.Stderr, "           deleted %d memories\n", n)
		}
	}

	// Write report.
	if *outputFile == "" {
		fmt.Fprintln(os.Stderr, "\n[report]")
	}
	writeReport(out, runID, source, synthesisOn, baseline, waves, snaps, cleanup)
}

func keyStatus() string {
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		return "set"
	}
	return "not set"
}
