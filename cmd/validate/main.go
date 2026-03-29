// Command validate is a standalone validation harness for memoryd's pipeline,
// steward, and quality systems. It runs entirely in-memory with no external
// dependencies (no MongoDB, no llama-server, no network).
//
// The harness exercises:
//   - Write pipeline: chunking, noise filtering, dedup, topic grouping
//   - Steward: scoring, pruning (with time fast-forwarding), merging near-duplicates
//   - Retrieval: selective hits to test quality score differentiation
//   - End-to-end: corpus ingestion → retrieval simulation → steward sweeps → assertions
//
// Usage:
//
//	go run ./cmd/validate
//	go run ./cmd/validate -v          # verbose logging
//	go run ./cmd/validate -scenario pruning   # run one scenario
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/memory-daemon/memoryd/internal/pipeline"
	"github.com/memory-daemon/memoryd/internal/steward"
	"github.com/memory-daemon/memoryd/internal/store"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

var (
	verbose      = flag.Bool("v", false, "verbose output (show all log lines)")
	scenarioFlag = flag.String("scenario", "", "run only this scenario by name, or 'all'. Run without -scenario to see the full ordered suite.")
)

func main() {
	flag.Parse()
	if !*verbose {
		log.SetOutput(&quietLogger{})
	}

	scenarios := map[string]func(context.Context) error{
		// Original scenarios — core pipeline, steward, retrieval cycle.
		"ingestion": scenarioIngestion,
		"dedup":     scenarioDedup,
		"pruning":   scenarioPruning,
		"merging":   scenarioMerging,
		"retrieval": scenarioRetrieval,

		// Write pipeline deep tests.
		"boundary-filtering":   scenarioBoundaryFiltering,
		"bulk-dedup":           scenarioBulkDedup,
		"concurrent-writes":    scenarioConcurrentWrites,
		"write-result-count":   scenarioWriteResultCounting,
		"long-doc-chunking":    scenarioLongDocumentChunking,
		"source-preservation":  scenarioSourcePreservation,

		// Redaction tests.
		"redact-api-key":        scenarioRedactAPIKey,
		"redact-conn-string":    scenarioRedactConnectionString,
		"redact-preserves-code": scenarioRedactPreservesCode,
		"redact-jwt":            scenarioRedactJWT,
		"redact-multiline":      scenarioRedactMultilineSecret,

		// Read pipeline tests.
		"read-empty-store":       scenarioReadEmptyStore,
		"read-ranking":           scenarioReadRanking,
		"read-token-budget":      scenarioReadTokenBudget,
		"read-top-k":             scenarioReadTopK,
		"read-self-retrieval":    scenarioReadQuerySelfRetrieval,

		// Quality scoring and tracker tests.
		"content-scorer-range":   scenarioContentScorerRange,
		"content-scale-halflife": scenarioContentScaleHalfLife,
		"learning-mode":          scenarioLearningModeThreshold,
		"tracker-hit-counting":   scenarioTrackerHitCounting,

		// Steward edge cases.
		"steward-batch-limit":       scenarioStewardBatchLimit,
		"steward-decay-monotonic":   scenarioStewardDecayMonotonic,
		"steward-merge-winner":      scenarioStewardMergeWinner,
		"steward-grace-protection":  scenarioStewardGracePeriodProtection,

		// Edge cases and stability.
		"empty-whitespace":    scenarioEmptyAndWhitespace,
		"unicode-content":     scenarioUnicodeContent,
		"very-long-document":  scenarioVeryLongDocument,
		"repeated-writes":     scenarioRepeatedIdenticalWrites,
		"pipeline-stability":  scenarioPipelineStability,
	}

	// Order matters: build up from basic to complex.
	order := []string{
		// Foundation: the pipeline must work before anything else.
		"ingestion",
		"boundary-filtering",
		"empty-whitespace",
		"pipeline-stability",

		// Deduplication.
		"dedup",
		"bulk-dedup",
		"concurrent-writes",
		"repeated-writes",
		"write-result-count",

		// Chunking and source handling.
		"long-doc-chunking",
		"very-long-document",
		"source-preservation",

		// Redaction — security must hold before anything is stored.
		"redact-api-key",
		"redact-conn-string",
		"redact-preserves-code",
		"redact-jwt",
		"redact-multiline",

		// Unicode and exotic inputs.
		"unicode-content",

		// Read pipeline — depends on write pipeline working.
		"read-empty-store",
		"read-self-retrieval",
		"read-ranking",
		"read-top-k",
		"read-token-budget",

		// Quality subsystem.
		"content-scorer-range",
		"content-scale-halflife",
		"learning-mode",
		"tracker-hit-counting",

		// Steward — depends on store and scoring.
		"pruning",
		"merging",
		"retrieval",
		"steward-batch-limit",
		"steward-decay-monotonic",
		"steward-merge-winner",
		"steward-grace-protection",
	}

	if *scenarioFlag != "" && *scenarioFlag != "all" {
		if fn, ok := scenarios[*scenarioFlag]; ok {
			order = []string{*scenarioFlag}
			scenarios = map[string]func(context.Context) error{*scenarioFlag: fn}
		} else {
			fmt.Fprintf(os.Stderr, "Unknown scenario: %s\nAvailable (%d total):\n", *scenarioFlag, len(order))
			for _, name := range order {
				fmt.Fprintf(os.Stderr, "  %s\n", name)
			}
			os.Exit(1)
		}
	}

	ctx := context.Background()
	passed, failed := 0, 0

	fmt.Println("═══════════════════════════════════════════════════════════════════")
	fmt.Println("  memoryd validation harness")
	fmt.Println("  in-memory store • hash embedder • time-controlled • no network")
	fmt.Printf("  %d scenarios: pipeline · dedup · redaction · read · quality · steward · edge\n", len(order))
	fmt.Println("═══════════════════════════════════════════════════════════════════")
	fmt.Println()

	for _, name := range order {
		fn := scenarios[name]
		fmt.Printf("▶ %-20s ", name)
		start := time.Now()
		if err := fn(ctx); err != nil {
			fmt.Printf("FAIL (%s)\n", time.Since(start).Round(time.Millisecond))
			fmt.Printf("  ✗ %v\n", err)
			failed++
		} else {
			fmt.Printf("PASS (%s)\n", time.Since(start).Round(time.Millisecond))
			passed++
		}
	}

	fmt.Println()
	fmt.Printf("Results: %d passed, %d failed, %d total\n", passed, failed, passed+failed)
	if failed > 0 {
		os.Exit(1)
	}
}

// scenarioIngestion validates the write pipeline: chunking, noise filtering,
// and basic storage of a known corpus.
func scenarioIngestion(ctx context.Context) error {
	st := newMemStore()
	emb := newHashEmbedder(128)
	wp := pipeline.NewWritePipeline(emb, st)

	corpus := loadCorpus()
	var totalStored, totalFiltered, totalDuplicates int

	for _, entry := range corpus {
		result := wp.ProcessFiltered(entry.Content, "validate-"+entry.Category, nil)
		totalStored += result.Stored
		totalFiltered += result.Filtered
		totalDuplicates += result.Duplicates
	}

	memCount := st.count()

	// Assertions:
	// 1. Some memories should be stored.
	if memCount == 0 {
		return fmt.Errorf("no memories stored from %d corpus entries", len(corpus))
	}
	// 2. Short noise entries (<20 chars) should be filtered.
	if totalFiltered == 0 {
		return fmt.Errorf("expected noise filtering, but nothing was filtered")
	}
	// 3. Duplicate entries should trigger dedup.
	if totalDuplicates == 0 {
		return fmt.Errorf("expected some duplicates to be caught, but got 0")
	}
	// 4. We should have fewer memories than corpus entries.
	if memCount >= len(corpus) {
		return fmt.Errorf("expected fewer memories (%d) than corpus entries (%d) due to filtering/dedup",
			memCount, len(corpus))
	}

	if *verbose {
		fmt.Printf("\n    corpus=%d stored=%d filtered=%d dedup=%d final=%d\n",
			len(corpus), totalStored, totalFiltered, totalDuplicates, memCount)
	}

	return nil
}

// scenarioDedup tests the write pipeline's deduplication by ingesting
// semantically similar content and verifying only one copy is kept.
func scenarioDedup(ctx context.Context) error {
	st := newMemStore()
	emb := newHashEmbedder(128)
	wp := pipeline.NewWritePipeline(emb, st)

	// Store the first phrasing.
	r1 := wp.ProcessFiltered(
		"The deduplication threshold is set to cosine similarity 0.92. Chunks scoring above this against existing memories are skipped.",
		"validate", nil)

	// Store the same content verbatim — should be exact duplicate.
	r2 := wp.ProcessFiltered(
		"The deduplication threshold is set to cosine similarity 0.92. Chunks scoring above this against existing memories are skipped.",
		"validate", nil)

	if r2.Duplicates == 0 {
		return fmt.Errorf("exact duplicate was not caught")
	}
	if st.count() != r1.Stored {
		return fmt.Errorf("expected %d memory after dedup, got %d", r1.Stored, st.count())
	}

	return nil
}

// scenarioPruning validates the steward's pruning behavior with time fast-forwarding.
// It creates memories at different ages and hit counts, then runs sweeps at
// progressively later "now" times to verify:
//   - New memories are protected by the grace period
//   - Old unretrieved memories get pruned after scoring below threshold
//   - Memories with hit counts survive regardless of age
func scenarioPruning(ctx context.Context) error {
	st := newMemStore()
	emb := newHashEmbedder(128)
	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Simulated clock that we advance manually.
	currentTime := baseTime
	nowFunc := func() time.Time { return currentTime }

	cfg := steward.Config{
		Interval:         time.Millisecond, // irrelevant — we call Sweep directly
		PruneThreshold:   0.1,
		PruneGracePeriod: 2 * 24 * time.Hour, // 2 days
		DecayHalfLife:    20 * 24 * time.Hour, // 20 days — fast enough that 60-day-old 0-hit memories drop below 0.1
		MergeThreshold:   0.88,
		BatchSize:        500,
		NowFunc:          nowFunc,
	}

	swd := steward.New(cfg, st, emb)

	// --- Phase 1: Insert memories at different historical times ---

	// Group A: "old and never retrieved" — should be pruned.
	// Each uniquely phrased so hash embedder won't merge them.
	groupA := []string{
		"The capital of France is Paris, located along the Seine river.",
		"A binary search tree has O(log n) lookup time when balanced.",
		"The Pythagorean theorem states that a squared plus b squared equals c squared.",
		"Photosynthesis converts carbon dioxide and water into glucose and oxygen.",
		"The United Nations has 193 member states as of 2024.",
		"A stack data structure follows last-in-first-out ordering for push and pop.",
		"The Pacific Ocean is the largest and deepest ocean on Earth.",
		"HTTP status code 404 means the requested resource was not found on the server.",
		"The human genome contains approximately three billion base pairs of DNA.",
		"A linked list provides O(1) insertion at the head but O(n) random access.",
	}
	for _, content := range groupA {
		vec, _ := emb.Embed(ctx, content)
		_ = st.Insert(ctx, store.Memory{
			ID:        primitive.NewObjectID(),
			Content:   content,
			Embedding: vec,
			Source:    "validate",
			CreatedAt: baseTime.Add(-60 * 24 * time.Hour), // 60 days ago
			HitCount:  0,
		})
	}

	// Group B: "old but frequently retrieved" — should survive
	// Each entry is semantically distinct to avoid merge.
	groupB := []string{
		"MongoDB Atlas uses WiredTiger as its storage engine with compression enabled by default.",
		"The Go garbage collector uses a concurrent tri-color mark-and-sweep algorithm since Go 1.5.",
		"TLS 1.3 reduces handshake round trips from two to one compared to TLS 1.2.",
		"Kubernetes liveness probes should have initialDelaySeconds set to avoid killing slow-starting pods.",
		"PostgreSQL MVCC implementation keeps old row versions in the main table, requiring periodic VACUUM.",
	}
	for i, content := range groupB {
		vec, _ := emb.Embed(ctx, content)
		_ = st.Insert(ctx, store.Memory{
			ID:            primitive.NewObjectID(),
			Content:       content,
			Embedding:     vec,
			Source:        "validate",
			CreatedAt:     baseTime.Add(-60 * 24 * time.Hour), // 60 days ago
			HitCount:      10 + i,
			LastRetrieved: baseTime.Add(-1 * 24 * time.Hour), // retrieved yesterday
		})
	}

	// Group C: "brand new" — protected by grace period.
	groupC := []string{
		"WebAssembly provides near-native execution speed in web browsers using a stack-based virtual machine.",
		"The Rust borrow checker prevents data races at compile time without a garbage collector.",
		"GraphQL allows clients to request exactly the fields they need, reducing over-fetching.",
		"ZFS provides built-in checksumming, copy-on-write snapshots, and automatic data repair.",
		"Raft consensus algorithm elects a leader then replicates log entries to followers.",
	}
	for _, content := range groupC {
		vec, _ := emb.Embed(ctx, content)
		_ = st.Insert(ctx, store.Memory{
			ID:        primitive.NewObjectID(),
			Content:   content,
			Embedding: vec,
			Source:    "validate",
			CreatedAt: baseTime, // created "right now"
			HitCount:  0,
		})
	}

	initialCount := st.count()
	if initialCount != 20 {
		return fmt.Errorf("expected 20 initial memories, got %d", initialCount)
	}

	// --- Phase 2: First sweep at baseTime (day 0) ---
	// Score everything, but grace period protects group C.
	// Group A: old, no hits → score decays, but pruning needs score < threshold.
	stats := swd.Sweep(ctx)
	countAfterSweep1 := st.count()

	if stats.Scored != initialCount {
		return fmt.Errorf("sweep 1: expected %d scored, got %d", initialCount, stats.Scored)
	}

	// Group A should be pruned (old, 0 hits, score decayed well below 0.1).
	// Group B survives (has hit counts).
	// Group C survives (within grace period).
	if countAfterSweep1 >= initialCount {
		return fmt.Errorf("sweep 1: expected some pruning, count went from %d to %d",
			initialCount, countAfterSweep1)
	}

	// Check group B survived.
	survived := 0
	for _, m := range st.snapshot() {
		if m.HitCount >= 10 {
			survived++
		}
	}
	if survived != 5 {
		return fmt.Errorf("sweep 1: expected 5 high-hit memories to survive, got %d", survived)
	}

	// --- Phase 3: Advance time 7 days and sweep again ---
	// Group C is now past grace period. With no hits, they should start decaying.
	currentTime = baseTime.Add(7 * 24 * time.Hour)
	stats2 := swd.Sweep(ctx)
	countAfterSweep2 := st.count()

	// Group B (high hits) should still be there.
	survived = 0
	for _, m := range st.snapshot() {
		if m.HitCount >= 10 {
			survived++
		}
	}
	if survived != 5 {
		return fmt.Errorf("sweep 2 (day 7): high-hit memories should survive, got %d", survived)
	}

	// --- Phase 4: Advance to day 90 — aggressive decay ---
	currentTime = baseTime.Add(90 * 24 * time.Hour)
	stats3 := swd.Sweep(ctx)
	countAfterSweep3 := st.count()

	// After 90 days, only the frequently-retrieved group B should remain.
	// Group C (0 hits) should be pruned by now.
	survived = 0
	for _, m := range st.snapshot() {
		if m.HitCount >= 10 {
			survived++
		}
	}
	if survived != 5 {
		return fmt.Errorf("sweep 3 (day 90): high-hit memories should survive, got %d", survived)
	}

	if *verbose {
		fmt.Printf("\n    initial=%d sweep1=%d(scored=%d pruned=%d) sweep2=%d(pruned=%d) sweep3=%d(pruned=%d)\n",
			initialCount, countAfterSweep1, stats.Scored, stats.Pruned,
			countAfterSweep2, stats2.Pruned, countAfterSweep3, stats3.Pruned)
	}

	return nil
}

// scenarioMerging tests the steward's ability to merge near-duplicate memories.
func scenarioMerging(ctx context.Context) error {
	st := newMemStore()
	emb := newHashEmbedder(128)
	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cfg := steward.Config{
		Interval:         time.Millisecond,
		PruneThreshold:   0.1,
		PruneGracePeriod: 24 * time.Hour,
		DecayHalfLife:    90 * 24 * time.Hour,
		MergeThreshold:   0.80, // lower threshold for hash embedder
		BatchSize:        500,
		NowFunc:          func() time.Time { return baseTime },
	}

	swd := steward.New(cfg, st, emb)

	// Insert exact-duplicate content with different IDs.
	content := "The embedding model produces 1024-dimensional vectors. These are stored as float32 arrays in MongoDB."
	vec, _ := emb.Embed(ctx, content)

	st.Insert(ctx, store.Memory{
		ID:        primitive.NewObjectID(),
		Content:   content,
		Embedding: vec,
		Source:    "validate",
		CreatedAt: baseTime.Add(-48 * time.Hour),
		HitCount:  5,
	})

	st.Insert(ctx, store.Memory{
		ID:        primitive.NewObjectID(),
		Content:   content, // same content
		Embedding: vec,     // same embedding
		Source:    "validate",
		CreatedAt: baseTime.Add(-24 * time.Hour),
		HitCount:  1,
	})

	before := st.count()
	if before != 2 {
		return fmt.Errorf("expected 2 memories before merge, got %d", before)
	}

	stats := swd.Sweep(ctx)
	after := st.count()

	if stats.Merged == 0 {
		return fmt.Errorf("expected at least 1 merge, got 0")
	}
	if after >= before {
		return fmt.Errorf("expected fewer memories after merge: before=%d after=%d", before, after)
	}

	// The surviving memory should be the one with more hits.
	for _, m := range st.snapshot() {
		if m.HitCount < 5 {
			return fmt.Errorf("expected the higher-hit-count memory to survive, got hit_count=%d", m.HitCount)
		}
	}

	if *verbose {
		fmt.Printf("\n    before=%d after=%d merged=%d\n", before, after, stats.Merged)
	}

	return nil
}

// scenarioRetrieval tests the full cycle: ingest → selective retrieval →
// steward scoring → verify that retrieved memories score higher.
func scenarioRetrieval(ctx context.Context) error {
	st := newMemStore()
	emb := newHashEmbedder(128)
	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	currentTime := baseTime
	nowFunc := func() time.Time { return currentTime }

	cfg := steward.Config{
		Interval:         time.Millisecond,
		PruneThreshold:   0.1,
		PruneGracePeriod: 2 * 24 * time.Hour,
		DecayHalfLife:    30 * 24 * time.Hour,
		MergeThreshold:   0.88,
		BatchSize:        500,
		NowFunc:          nowFunc,
	}

	swd := steward.New(cfg, st, emb)

	// Insert two groups of memories, all aged 10 days.
	createdAt := baseTime.Add(-10 * 24 * time.Hour)

	// Group "retrieved": will be simulated as retrieved.
	// Each is semantically distinct to avoid steward merging.
	retrievedContents := []string{
		"Redis uses single-threaded event loop for commands, achieving low latency without locking.",
		"The Linux kernel OOM killer selects processes based on oom_score_adj plus memory consumption.",
		"gRPC uses HTTP/2 multiplexing, allowing concurrent streams over a single TCP connection.",
		"B-tree indexes in InnoDB store row data in leaf pages, making range scans efficient.",
		"Docker overlay2 storage driver uses hardlinks to share common layers between containers.",
	}
	retrievedIDs := make([]primitive.ObjectID, len(retrievedContents))
	for i, content := range retrievedContents {
		id := primitive.NewObjectID()
		retrievedIDs[i] = id
		vec, _ := emb.Embed(ctx, content)
		st.Insert(ctx, store.Memory{
			ID:        id,
			Content:   content,
			Embedding: vec,
			Source:    "validate",
			CreatedAt: createdAt,
			HitCount:  0,
		})
	}

	// Group "ignored": never retrieved.
	ignoredContents := []string{
		"The Fibonacci sequence appears in sunflower seed spirals and pinecone scales in nature.",
		"Ancient Sumerian cuneiform tablets recorded beer recipes over four thousand years ago.",
		"The speed of light in a vacuum is approximately 299,792,458 meters per second.",
		"Octopuses have three hearts: two pump blood to gills, one pumps it to the body.",
		"The Great Wall of China is not a single continuous wall but a collection of segments.",
	}
	for _, content := range ignoredContents {
		vec, _ := emb.Embed(ctx, content)
		st.Insert(ctx, store.Memory{
			ID:        primitive.NewObjectID(),
			Content:   content,
			Embedding: vec,
			Source:    "validate",
			CreatedAt: createdAt,
			HitCount:  0,
		})
	}

	// --- Simulate retrieval hits on the "retrieved" group ---
	for _, id := range retrievedIDs {
		for j := 0; j < 5; j++ { // 5 hits each
			_ = st.IncrementHitCount(ctx, id)
		}
	}

	// --- Sweep to score ---
	swd.Sweep(ctx)

	// Check that retrieved memories have higher quality scores.
	mems := st.snapshot()
	var retrievedScores, ignoredScores []float64
	for _, m := range mems {
		isRetrieved := false
		for _, id := range retrievedIDs {
			if m.ID == id {
				isRetrieved = true
				break
			}
		}
		if isRetrieved {
			retrievedScores = append(retrievedScores, m.QualityScore)
		} else {
			ignoredScores = append(ignoredScores, m.QualityScore)
		}
	}

	avgRetrieved := avg(retrievedScores)
	avgIgnored := avg(ignoredScores)

	if avgRetrieved <= avgIgnored {
		return fmt.Errorf("retrieved memories should score higher: avg_retrieved=%.3f avg_ignored=%.3f",
			avgRetrieved, avgIgnored)
	}

	// --- Advance time and sweep again: ignored should decay more ---
	currentTime = baseTime.Add(45 * 24 * time.Hour)
	swd.Sweep(ctx)
	countAfterDecay := st.count()

	// Retrieved memories should still be alive.
	retrievedAlive := 0
	for _, m := range st.snapshot() {
		for _, id := range retrievedIDs {
			if m.ID == id {
				retrievedAlive++
				break
			}
		}
	}
	if retrievedAlive != 5 {
		return fmt.Errorf("expected 5 retrieved memories to survive after day 45, got %d", retrievedAlive)
	}

	if *verbose {
		fmt.Printf("\n    avg_retrieved=%.3f avg_ignored=%.3f alive_at_day45=%d/%d\n",
			avgRetrieved, avgIgnored, retrievedAlive, countAfterDecay)
	}

	return nil
}

// --- Helpers ---

func avg(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

// quietLogger discards log output unless verbose mode is on.
type quietLogger struct{}

func (q *quietLogger) Write(p []byte) (int, error) { return len(p), nil }
