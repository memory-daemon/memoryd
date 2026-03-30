package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/memory-daemon/memoryd/internal/config"
	"github.com/memory-daemon/memoryd/internal/pipeline"
	"github.com/memory-daemon/memoryd/internal/store"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func defaultTestConfig(topK, maxTokens int) *config.Config {
	return &config.Config{
		RetrievalTopK:      topK,
		RetrievalMaxTokens: maxTokens,
	}
}

// scenarioReadEmptyStore verifies that querying an empty memory store returns
// an empty string and does not error. This is the cold-start case.
func scenarioReadEmptyStore(ctx context.Context) error {
	st := newMemStore()
	emb := newHashEmbedder(128)
	cfg := defaultTestConfig(5, 2048)
	rp := pipeline.NewReadPipeline(emb, st, cfg)

	result, err := rp.Retrieve(ctx, "how does the dedup threshold work?")
	if err != nil {
		return fmt.Errorf("retrieve on empty store returned error: %w", err)
	}
	if result != "" {
		return fmt.Errorf("expected empty string from empty store, got: %q", result[:min(100, len(result))])
	}

	if *verbose {
		fmt.Printf("\n    empty store → empty context ✓\n")
	}
	return nil
}

// scenarioReadRanking verifies that retrieval returns results in descending
// similarity order. We insert memories at known similarity levels by making
// them share varying amounts of n-gram overlap with the query.
func scenarioReadRanking(ctx context.Context) error {
	st := newMemStore()
	emb := newHashEmbedder(128)
	cfg := defaultTestConfig(5, 4096)
	rp := pipeline.NewReadPipeline(emb, st, cfg)
	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// "High" memory: shares many n-grams with our query about cosine similarity deduplication.
	highContent := "The deduplication system checks cosine similarity between new chunk vectors and stored memory vectors. Any chunk scoring above 0.92 cosine similarity is considered a duplicate and discarded without storage."
	// "Low" memory: entirely different topic, minimal n-gram overlap.
	lowContent := "The Pacific Ocean covers more than 30 percent of the Earth's total surface area and contains more than half of all oceanic water by volume."

	highVec, _ := emb.Embed(ctx, highContent)
	lowVec, _ := emb.Embed(ctx, lowContent)

	_ = st.Insert(ctx, store.Memory{
		ID: primitive.NewObjectID(), Content: highContent, Embedding: highVec,
		Source: "validate", CreatedAt: baseTime,
	})
	_ = st.Insert(ctx, store.Memory{
		ID: primitive.NewObjectID(), Content: lowContent, Embedding: lowVec,
		Source: "validate", CreatedAt: baseTime,
	})

	query := "cosine similarity deduplication threshold"
	_, memories, err := rp.RetrieveWithScores(ctx, query)
	if err != nil {
		return fmt.Errorf("retrieve: %w", err)
	}
	if len(memories) < 2 {
		return fmt.Errorf("expected 2 memories returned, got %d", len(memories))
	}

	// First result must have a higher score than the second.
	if memories[0].Score <= memories[1].Score {
		return fmt.Errorf("retrieval not ranked: score[0]=%.4f should be > score[1]=%.4f",
			memories[0].Score, memories[1].Score)
	}
	// The high-similarity memory must rank first.
	if !strings.Contains(memories[0].Content, "cosine similarity") {
		return fmt.Errorf("expected on-topic memory to rank first, got: %q", memories[0].Content[:60])
	}

	if *verbose {
		fmt.Printf("\n    query=%q → top=%q (%.4f) second=%q (%.4f)\n",
			query, memories[0].Content[:40], memories[0].Score,
			memories[1].Content[:40], memories[1].Score)
	}
	return nil
}

// scenarioReadTokenBudget verifies that FormatContext respects RetrievalMaxTokens.
// With maxTokens=50 (200 chars), the formatted context must stay within that budget
// even when many large memories are stored.
func scenarioReadTokenBudget(ctx context.Context) error {
	st := newMemStore()
	emb := newHashEmbedder(128)

	// Each memory has ~300 chars of content — well over the 200-char budget alone.
	contents := []string{
		"MongoDB Atlas vector search requires a pre-built HNSW index. The $vectorSearch stage uses numCandidates to oversample before returning the final topK results, improving recall at the cost of latency.",
		"The voyage-4-nano model produces 1024-dimensional float32 embedding vectors. It runs locally via llama.cpp in server mode on port 7433. Q8_0 quantization preserves 99.2 percent of full-precision retrieval accuracy.",
		"The steward background service sweeps hourly and decays quality scores using an exponential formula with a 90-day half-life. Memories below 0.1 quality score after the grace period are permanently deleted.",
		"Hybrid search combines $vectorSearch semantic results with $search keyword results using Reciprocal Rank Fusion with k=60. RRF is rank-based so it handles the different score distributions correctly.",
		"Source ingestion crawls documentation sites using BFS and detects changes with SHA-256 hashes per page. Only pages whose content hash changed are re-chunked and re-embedded on subsequent crawls.",
	}

	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, c := range contents {
		vec, _ := emb.Embed(ctx, c)
		_ = st.Insert(ctx, store.Memory{
			ID:        primitive.NewObjectID(),
			Content:   c,
			Embedding: vec,
			Source:    "validate",
			Score:     float64(len(contents)-i) * 0.1, // descending pre-set scores
			CreatedAt: baseTime,
		})
	}

	// Tight token budget: 50 tokens = 200 chars.
	const maxTokens = 50
	cfg := defaultTestConfig(5, maxTokens)
	rp := pipeline.NewReadPipeline(emb, st, cfg)

	result, err := rp.Retrieve(ctx, "embedding model vector search")
	if err != nil {
		return fmt.Errorf("retrieve: %w", err)
	}

	maxChars := maxTokens * 4
	if len(result) > maxChars+50 { // +50 for header/footer overhead
		return fmt.Errorf("context length %d exceeds token budget (maxTokens=%d → maxChars=%d): %q",
			len(result), maxTokens, maxChars, result[:min(100, len(result))])
	}

	if *verbose {
		fmt.Printf("\n    budget=%d tokens (%d chars) → context=%d chars\n", maxTokens, maxChars, len(result))
	}
	return nil
}

// scenarioReadTopK verifies that retrieval returns at most RetrievalTopK results
// regardless of how many memories are in the store.
func scenarioReadTopK(ctx context.Context) error {
	st := newMemStore()
	emb := newHashEmbedder(128)
	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Store 20 memories — all with unique content to avoid dedup.
	for i := 0; i < 20; i++ {
		content := fmt.Sprintf("Technical note %d: the memoryd pipeline component number %d handles a distinct stage of the ingestion and retrieval workflow with specific configuration options.", i, i)
		vec, _ := emb.Embed(ctx, content)
		_ = st.Insert(ctx, store.Memory{
			ID:        primitive.NewObjectID(),
			Content:   content,
			Embedding: vec,
			Source:    "validate",
			CreatedAt: baseTime,
		})
	}

	const topK = 3
	cfg := defaultTestConfig(topK, 8192)
	rp := pipeline.NewReadPipeline(emb, st, cfg)

	_, memories, err := rp.RetrieveWithScores(ctx, "pipeline component stage ingestion retrieval")
	if err != nil {
		return fmt.Errorf("retrieve: %w", err)
	}
	if len(memories) > topK {
		return fmt.Errorf("expected at most %d results (topK), got %d", topK, len(memories))
	}
	if len(memories) == 0 {
		return fmt.Errorf("expected >0 results, got none")
	}

	if *verbose {
		fmt.Printf("\n    store=20 memories topK=%d → returned=%d\n", topK, len(memories))
	}
	return nil
}

// scenarioReadQuerySelfRetrieval verifies the basic retrieval contract: store a
// memory, then query with text that closely matches it, and confirm the memory
// is returned. This is the fundamental "store and retrieve" smoke test.
func scenarioReadQuerySelfRetrieval(ctx context.Context) error {
	st := newMemStore()
	emb := newHashEmbedder(128)
	cfg := defaultTestConfig(5, 4096)
	rp := pipeline.NewReadPipeline(emb, st, cfg)
	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Store a specific fact.
	fact := "The memoryd write pipeline runs deduplication using cosine similarity threshold 0.92 against all stored memory embeddings before inserting a new chunk."
	vec, _ := emb.Embed(ctx, fact)
	_ = st.Insert(ctx, store.Memory{
		ID:        primitive.NewObjectID(),
		Content:   fact,
		Embedding: vec,
		Source:    "validate",
		CreatedAt: baseTime,
	})

	// Query with closely matching text.
	query := "memoryd write pipeline deduplication cosine similarity threshold"
	result, memories, err := rp.RetrieveWithScores(ctx, query)
	if err != nil {
		return fmt.Errorf("retrieve: %w", err)
	}
	if len(memories) == 0 {
		return fmt.Errorf("expected stored memory to be retrieved, got none")
	}
	if !strings.Contains(result, "deduplication") {
		return fmt.Errorf("retrieved context does not contain stored fact: %q", result[:min(200, len(result))])
	}

	if *verbose {
		fmt.Printf("\n    stored 1 memory → query returned %d result(s), top score=%.4f\n",
			len(memories), memories[0].Score)
	}
	return nil
}
