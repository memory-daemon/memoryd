package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/memory-daemon/memoryd/internal/config"
	"github.com/memory-daemon/memoryd/internal/quality"
	"github.com/memory-daemon/memoryd/internal/store"
)

type mockEmbedder struct {
	dim     int
	calls   int
	failOn  int
	lastTxt string
}

func (m *mockEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	m.calls++
	m.lastTxt = text
	if m.failOn > 0 && m.calls == m.failOn {
		return nil, context.DeadlineExceeded
	}
	vec := make([]float32, m.dim)
	// Place energy in a different dimension per call so distinct texts
	// get near-orthogonal embeddings (won't trigger consolidation).
	idx := (m.calls - 1) % m.dim
	vec[idx] = 1.0
	return vec, nil
}

func (m *mockEmbedder) Dim() int     { return m.dim }
func (m *mockEmbedder) Close() error { return nil }

func (m *mockEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	vecs := make([][]float32, len(texts))
	for i, t := range texts {
		v, err := m.Embed(ctx, t)
		if err != nil {
			return nil, err
		}
		vecs[i] = v
	}
	return vecs, nil
}

type mockStore struct {
	memories  []store.Memory
	inserted  []store.Memory
	searchErr error
}

func (m *mockStore) VectorSearch(_ context.Context, _ []float32, topK int) ([]store.Memory, error) {
	if m.searchErr != nil {
		return nil, m.searchErr
	}
	limit := topK
	if limit > len(m.memories) {
		limit = len(m.memories)
	}
	return m.memories[:limit], nil
}

func (m *mockStore) Insert(_ context.Context, mem store.Memory) error {
	m.inserted = append(m.inserted, mem)
	return nil
}

func (m *mockStore) Delete(_ context.Context, id string) error { return nil }
func (m *mockStore) List(_ context.Context, _ string, _ int) ([]store.Memory, error) {
	return m.memories, nil
}
func (m *mockStore) DeleteAll(_ context.Context) error                        { return nil }
func (m *mockStore) CountBySource(_ context.Context, _ string) (int64, error) { return 0, nil }
func (m *mockStore) UpdateContent(_ context.Context, _ string, _ string, _ []float32) error {
	return nil
}
func (m *mockStore) ListBySource(_ context.Context, _ string, _ int) ([]store.Memory, error) {
	return nil, nil
}
func (m *mockStore) Close() error { return nil }

func TestReadPipeline_Retrieve_WithMatches(t *testing.T) {
	emb := &mockEmbedder{dim: 512}
	st := &mockStore{
		memories: []store.Memory{
			{Content: "Go uses goroutines for concurrency.", Source: "claude-code", Score: 0.92},
			{Content: "MongoDB Atlas supports vector search.", Source: "manual", Score: 0.85},
		},
	}
	cfg := &config.Config{RetrievalTopK: 5, RetrievalMaxTokens: 2048}

	rp := NewReadPipeline(emb, st, cfg)
	result, err := rp.Retrieve(context.Background(), "how does Go handle concurrency?")
	if err != nil {
		t.Fatalf("Retrieve() error: %v", err)
	}

	if result == "" {
		t.Fatal("expected non-empty context")
	}
	if !strings.Contains(result, "goroutines") {
		t.Error("expected context to contain goroutines")
	}
	if !strings.Contains(result, "vector search") {
		t.Error("expected context to contain vector search")
	}
	if !strings.Contains(result, "<retrieved_context>") {
		t.Error("expected XML tags in formatted context")
	}
	if emb.calls != 1 {
		t.Errorf("embedder called %d times, want 1", emb.calls)
	}
}

func TestReadPipeline_Retrieve_NoMatches(t *testing.T) {
	emb := &mockEmbedder{dim: 512}
	st := &mockStore{memories: nil}
	cfg := &config.Config{RetrievalTopK: 5, RetrievalMaxTokens: 2048}

	rp := NewReadPipeline(emb, st, cfg)
	result, err := rp.Retrieve(context.Background(), "obscure query")
	if err != nil {
		t.Fatalf("Retrieve() error: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}

func TestReadPipeline_Retrieve_EmbeddingError(t *testing.T) {
	emb := &mockEmbedder{dim: 512, failOn: 1}
	st := &mockStore{}
	cfg := &config.Config{RetrievalTopK: 5, RetrievalMaxTokens: 2048}

	rp := NewReadPipeline(emb, st, cfg)
	_, err := rp.Retrieve(context.Background(), "test")
	if err == nil {
		t.Error("expected error when embedder fails")
	}
}

func TestReadPipeline_Retrieve_StoreError(t *testing.T) {
	emb := &mockEmbedder{dim: 512}
	st := &mockStore{searchErr: context.DeadlineExceeded}
	cfg := &config.Config{RetrievalTopK: 5, RetrievalMaxTokens: 2048}

	rp := NewReadPipeline(emb, st, cfg)
	_, err := rp.Retrieve(context.Background(), "test")
	if err == nil {
		t.Error("expected error when store fails")
	}
}

func TestWritePipeline_Process_SingleChunk(t *testing.T) {
	emb := &mockEmbedder{dim: 512}
	st := &mockStore{}

	wp := NewWritePipeline(emb, st)
	wp.Process("Short text to store — must be at least forty characters long.", "test-source", map[string]any{"session": "s1"})

	if len(st.inserted) != 1 {
		t.Fatalf("expected 1 insertion, got %d", len(st.inserted))
	}
	if st.inserted[0].Content != "Short text to store — must be at least forty characters long." {
		t.Errorf("content = %q", st.inserted[0].Content)
	}
	if st.inserted[0].Source != "test-source" {
		t.Errorf("source = %q, want test-source", st.inserted[0].Source)
	}
	if len(st.inserted[0].Embedding) != 512 {
		t.Errorf("embedding dim = %d, want 512", len(st.inserted[0].Embedding))
	}
}

func TestWritePipeline_Process_MultipleChunks(t *testing.T) {
	emb := &mockEmbedder{dim: 512}
	st := &mockStore{}

	var sb strings.Builder
	for i := 0; i < 200; i++ {
		sb.WriteString(strings.Repeat("word ", 30))
		sb.WriteString("\n\n")
	}

	wp := NewWritePipeline(emb, st)
	wp.Process(sb.String(), "test", nil)

	if len(st.inserted) < 2 {
		t.Errorf("expected multiple chunks stored, got %d", len(st.inserted))
	}
	for i, m := range st.inserted {
		if len(m.Embedding) != 512 {
			t.Errorf("chunk %d: embedding dim = %d, want 512", i, len(m.Embedding))
		}
	}
}

func TestWritePipeline_Process_EmbeddingFailureContinues(t *testing.T) {
	emb := &mockEmbedder{dim: 512, failOn: 1}
	st := &mockStore{}

	text := "Paragraph one has enough content to pass the noise gate.\n\nParagraph two also has enough content to pass the noise gate."
	wp := NewWritePipeline(emb, st)
	wp.Process(text, "test", nil)

	if emb.calls < 1 {
		t.Error("expected at least 1 embed call")
	}
}

func TestWritePipeline_Process_EmptyText(t *testing.T) {
	emb := &mockEmbedder{dim: 512}
	st := &mockStore{}

	wp := NewWritePipeline(emb, st)
	wp.Process("", "test", nil)

	if len(st.inserted) != 0 {
		t.Errorf("expected 0 insertions for empty text, got %d", len(st.inserted))
	}
	if emb.calls != 0 {
		t.Errorf("expected 0 embed calls for empty text, got %d", emb.calls)
	}
}

// --- ProcessDirect tests ---

func TestWritePipeline_ProcessDirect_StoresEntry(t *testing.T) {
	emb := &mockEmbedder{dim: 512}
	st := &mockStore{}

	wp := NewWritePipeline(emb, st)
	result := wp.ProcessDirect("Q: How do goroutines work?\n\nA: They are lightweight threads managed by the Go runtime.", "claude-code", nil)

	if result.Stored != 1 {
		t.Errorf("expected 1 stored, got %d", result.Stored)
	}
	if len(st.inserted) != 1 {
		t.Fatalf("expected 1 insertion, got %d", len(st.inserted))
	}
	if !strings.Contains(st.inserted[0].Content, "goroutines") {
		t.Error("expected content to contain goroutines")
	}
}

func TestWritePipeline_ProcessDirect_SkipsNoise(t *testing.T) {
	emb := &mockEmbedder{dim: 512}
	st := &mockStore{}

	wp := NewWritePipeline(emb, st)
	result := wp.ProcessDirect("hi", "claude-code", nil)

	if result.Filtered != 1 {
		t.Errorf("expected 1 filtered, got %d", result.Filtered)
	}
	if emb.calls != 0 {
		t.Errorf("expected 0 embed calls for noise, got %d", emb.calls)
	}
}

func TestWritePipeline_ProcessDirect_SkipsDuplicate(t *testing.T) {
	emb := &mockEmbedder{dim: 512}
	st := &mockStore{
		memories: []store.Memory{
			{Content: "Go uses goroutines.", Score: 0.95},
		},
	}

	wp := NewWritePipeline(emb, st)
	result := wp.ProcessDirect("Q: Tell me about goroutines.\n\nA: Go uses goroutines for concurrency.", "test", nil)

	if result.Duplicates != 1 {
		t.Errorf("expected 1 duplicate, got %d", result.Duplicates)
	}
	if result.Stored != 0 {
		t.Errorf("expected 0 stored, got %d", result.Stored)
	}
}

func TestWritePipeline_ProcessDirect_NilPipeline(t *testing.T) {
	// WritePipeline with nil embedder/store should return empty result without panic.
	wp := NewWritePipeline(nil, nil)
	result := wp.ProcessDirect("some meaningful text here worth storing", "test", nil)
	if result.Stored != 0 {
		t.Errorf("expected 0 stored with nil pipeline, got %d", result.Stored)
	}
}

// --- Self-filtering tests ---

func TestWritePipeline_DedupSkipsDuplicate(t *testing.T) {
	emb := &mockEmbedder{dim: 512}
	// Existing memory with high score — triggers dedup.
	st := &mockStore{
		memories: []store.Memory{
			{Content: "Go uses goroutines.", Score: 0.95},
		},
	}

	wp := NewWritePipeline(emb, st)
	result := wp.ProcessFiltered("Go uses goroutines for concurrency and lightweight thread management.", "test", nil)

	if result.Duplicates != 1 {
		t.Errorf("expected 1 duplicate, got %d", result.Duplicates)
	}
	if result.Stored != 0 {
		t.Errorf("expected 0 stored, got %d", result.Stored)
	}
	if len(st.inserted) != 0 {
		t.Errorf("expected 0 insertions, got %d", len(st.inserted))
	}
}

func TestWritePipeline_DedupAllowsNovel(t *testing.T) {
	emb := &mockEmbedder{dim: 512}
	// Existing memory with low score — not a duplicate.
	st := &mockStore{
		memories: []store.Memory{
			{Content: "Something unrelated.", Score: 0.3},
		},
	}

	wp := NewWritePipeline(emb, st)
	result := wp.ProcessFiltered("MongoDB Atlas supports vector search with cosine similarity indexing.", "test", nil)

	if result.Stored != 1 {
		t.Errorf("expected 1 stored, got %d", result.Stored)
	}
	if result.Duplicates != 0 {
		t.Errorf("expected 0 duplicates, got %d", result.Duplicates)
	}
}

func TestWritePipeline_NoiseFilterShortContent(t *testing.T) {
	emb := &mockEmbedder{dim: 512}
	st := &mockStore{}

	wp := NewWritePipeline(emb, st)
	result := wp.ProcessFiltered("hi", "test", nil)

	if result.Filtered != 1 {
		t.Errorf("expected 1 filtered, got %d", result.Filtered)
	}
	if result.Stored != 0 {
		t.Errorf("expected 0 stored, got %d", result.Stored)
	}
	if emb.calls != 0 {
		t.Errorf("expected 0 embed calls for noise, got %d", emb.calls)
	}
}

func TestWritePipeline_NoiseFilterNonAlphanumeric(t *testing.T) {
	emb := &mockEmbedder{dim: 512}
	st := &mockStore{}

	wp := NewWritePipeline(emb, st)
	// Mostly punctuation/symbols — should be filtered.
	result := wp.ProcessFiltered("----====....!!!!@@@@####$$$$", "test", nil)

	if result.Filtered != 1 {
		t.Errorf("expected 1 filtered, got %d", result.Filtered)
	}
}

func TestWritePipeline_ProcessFilteredSummary(t *testing.T) {
	emb := &mockEmbedder{dim: 512}
	st := &mockStore{}

	wp := NewWritePipeline(emb, st)
	result := wp.ProcessFiltered("This is meaningful content worth remembering.", "test", nil)

	summary := result.Summary()
	if summary != "1 stored." {
		t.Errorf("unexpected summary: %q", summary)
	}
}

func TestWriteResult_SummaryMixed(t *testing.T) {
	r := WriteResult{Stored: 2, Duplicates: 1, Filtered: 1}
	summary := r.Summary()
	if summary != "2 stored, 1 skipped (duplicate), 1 skipped (too short/noisy)." {
		t.Errorf("unexpected summary: %q", summary)
	}
}

func TestWriteResult_SummaryEmpty(t *testing.T) {
	r := WriteResult{}
	if r.Summary() != "Nothing to store." {
		t.Errorf("unexpected summary: %q", r.Summary())
	}
}

func TestIsNoise(t *testing.T) {
	tests := []struct {
		text  string
		noise bool
	}{
		{"", true},
		{"hi", true},
		{"   short   ", true},
		{"----!!!!@@@@####$$$$%%%%", true},
		{"This is a meaningful sentence with real content.", false},
		{"Go uses goroutines for lightweight concurrency.", false},
	}
	for _, tc := range tests {
		got := isNoise(tc.text, minContentLen, 0.4)
		if got != tc.noise {
			t.Errorf("isNoise(%q) = %v, want %v", tc.text, got, tc.noise)
		}
	}
}

// --- Topic boundary detection tests ---

func TestDetectTopicGroups_SingleChunk(t *testing.T) {
	chunks := []string{"only one"}
	vecs := [][]float32{{1, 0, 0}}
	groups := detectTopicGroups(chunks, vecs, TopicBoundaryThreshold, maxGroupChars)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if len(groups[0].chunks) != 1 {
		t.Errorf("expected 1 chunk in group, got %d", len(groups[0].chunks))
	}
}

func TestDetectTopicGroups_AllSameTopic(t *testing.T) {
	// High similarity between consecutive chunks → all one group.
	chunks := []string{"Go concurrency part 1", "Go concurrency part 2", "Go concurrency part 3"}
	vecs := [][]float32{
		{0.9, 0.1, 0, 0},
		{0.85, 0.15, 0, 0},
		{0.88, 0.12, 0, 0},
	}
	groups := detectTopicGroups(chunks, vecs, TopicBoundaryThreshold, maxGroupChars)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if len(groups[0].chunks) != 3 {
		t.Errorf("expected 3 chunks in group, got %d", len(groups[0].chunks))
	}
}

func TestDetectTopicGroups_TwoTopics(t *testing.T) {
	// Chunks 0-1 similar, chunk 2 orthogonal → boundary between 1 and 2.
	chunks := []string{"Go concurrency basics", "Go goroutines deep dive", "MongoDB Atlas indexing"}
	vecs := [][]float32{
		{0.9, 0.1, 0, 0},
		{0.85, 0.15, 0, 0}, // similar to 0
		{0, 0, 0.9, 0.1},   // different topic
	}
	groups := detectTopicGroups(chunks, vecs, TopicBoundaryThreshold, maxGroupChars)
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}
	if len(groups[0].chunks) != 2 {
		t.Errorf("expected 2 chunks in first group, got %d", len(groups[0].chunks))
	}
	if len(groups[1].chunks) != 1 {
		t.Errorf("expected 1 chunk in second group, got %d", len(groups[1].chunks))
	}
}

func TestDetectTopicGroups_AllDifferent(t *testing.T) {
	// All orthogonal → each chunk is its own group.
	chunks := []string{"topic A", "topic B", "topic C"}
	vecs := [][]float32{
		{1, 0, 0, 0},
		{0, 1, 0, 0},
		{0, 0, 1, 0},
	}
	groups := detectTopicGroups(chunks, vecs, TopicBoundaryThreshold, maxGroupChars)
	if len(groups) != 3 {
		t.Fatalf("expected 3 groups, got %d", len(groups))
	}
}

func TestDetectTopicGroups_Empty(t *testing.T) {
	groups := detectTopicGroups(nil, nil, TopicBoundaryThreshold, maxGroupChars)
	if len(groups) != 0 {
		t.Errorf("expected 0 groups for nil input, got %d", len(groups))
	}
}

func TestDetectTopicGroups_SizeCapSplitsSameTopic(t *testing.T) {
	// 3 chunks, all same topic (identical vectors), but each chunk is
	// large enough that 2 together exceed maxGroupChars.
	bigChunk := strings.Repeat("a", maxGroupChars/2+100) // just over half the budget
	chunks := []string{bigChunk, bigChunk, bigChunk}
	vec := []float32{0.9, 0.1, 0, 0}
	vecs := [][]float32{vec, vec, vec}

	groups := detectTopicGroups(chunks, vecs, TopicBoundaryThreshold, maxGroupChars)
	// First two won't fit together (combined > maxGroupChars), so each gets its own group.
	if len(groups) < 2 {
		t.Fatalf("expected at least 2 groups due to size cap, got %d", len(groups))
	}
	// Verify no group's combined text exceeds the budget.
	for i, g := range groups {
		total := 0
		for j, c := range g.chunks {
			if j > 0 {
				total += len(joinSeparator)
			}
			total += len(c)
		}
		if total > maxGroupChars {
			t.Errorf("group %d combined length %d exceeds maxGroupChars %d", i, total, maxGroupChars)
		}
	}
}

func TestDetectTopicGroups_SmallChunksFitInOneGroup(t *testing.T) {
	// 3 small same-topic chunks that easily fit within the budget.
	chunks := []string{"Go channels", "Go goroutines", "Go select"}
	vec := []float32{0.9, 0.1, 0, 0}
	vecs := [][]float32{vec, vec, vec}

	groups := detectTopicGroups(chunks, vecs, TopicBoundaryThreshold, maxGroupChars)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group for small same-topic chunks, got %d", len(groups))
	}
	if len(groups[0].chunks) != 3 {
		t.Errorf("expected 3 chunks in group, got %d", len(groups[0].chunks))
	}
}

func TestDetectTopicGroups_MixedTopicAndSizeBoundaries(t *testing.T) {
	// 4 chunks: 0-1 same topic (small), 2-3 different topic (big).
	// Should split at topic boundary between 1 and 2, and again between
	// 2 and 3 due to size.
	small := "short chunk about Go"
	big := strings.Repeat("b", maxGroupChars/2+100)
	chunks := []string{small, small, big, big}
	vecs := [][]float32{
		{0.9, 0.1, 0, 0},
		{0.85, 0.15, 0, 0}, // same topic as 0
		{0, 0, 0.9, 0.1},   // topic boundary
		{0, 0, 0.85, 0.15}, // same topic as 2 but too big to merge
	}

	groups := detectTopicGroups(chunks, vecs, TopicBoundaryThreshold, maxGroupChars)
	if len(groups) != 3 {
		t.Fatalf("expected 3 groups (topic split + size split), got %d", len(groups))
	}
	if len(groups[0].chunks) != 2 {
		t.Errorf("first group should have 2 small chunks, got %d", len(groups[0].chunks))
	}
	if len(groups[1].chunks) != 1 {
		t.Errorf("second group should have 1 big chunk, got %d", len(groups[1].chunks))
	}
	if len(groups[2].chunks) != 1 {
		t.Errorf("third group should have 1 big chunk, got %d", len(groups[2].chunks))
	}
}

func TestWriteResult_SummaryWithMerged(t *testing.T) {
	r := WriteResult{Stored: 2, Merged: 3, Duplicates: 1}
	summary := r.Summary()
	if !strings.Contains(summary, "3 merged (topic grouping)") {
		t.Errorf("expected merged in summary, got %q", summary)
	}
}

func TestCosineSim_Identical(t *testing.T) {
	v := []float32{1, 2, 3, 4}
	sim := CosineSim(v, v)
	if sim < 0.999 {
		t.Errorf("expected ~1.0 for identical vectors, got %f", sim)
	}
}

func TestCosineSim_Orthogonal(t *testing.T) {
	a := []float32{1, 0, 0}
	b := []float32{0, 1, 0}
	sim := CosineSim(a, b)
	if sim > 0.001 {
		t.Errorf("expected ~0 for orthogonal vectors, got %f", sim)
	}
}

func TestCosineSim_Empty(t *testing.T) {
	sim := CosineSim(nil, nil)
	if sim != 0 {
		t.Errorf("expected 0 for empty vectors, got %f", sim)
	}
}

// --- preprocessContent tests ---

func TestPreprocessContent_StripThinkBlock(t *testing.T) {
	in := "<think>This is reasoning.</think>\n\nThe actual answer goes here with enough text to be useful."
	got := preprocessContent(in)
	if strings.Contains(got, "<think>") || strings.Contains(got, "reasoning") {
		t.Errorf("think block not stripped: %q", got)
	}
	if !strings.Contains(got, "actual answer") {
		t.Errorf("post-think content missing: %q", got)
	}
}

func TestPreprocessContent_MultipleThinkBlocks(t *testing.T) {
	in := "<think>first</think>keep this<think>second</think>and this"
	got := preprocessContent(in)
	if strings.Contains(got, "<think>") || strings.Contains(got, "first") || strings.Contains(got, "second") {
		t.Errorf("think blocks not fully stripped: %q", got)
	}
	if !strings.Contains(got, "keep this") || !strings.Contains(got, "and this") {
		t.Errorf("content between blocks missing: %q", got)
	}
}

func TestPreprocessContent_UnclosedThinkBlock(t *testing.T) {
	in := "before<think>unclosed reasoning that goes on and on"
	got := preprocessContent(in)
	if strings.Contains(got, "<think>") || strings.Contains(got, "unclosed") {
		t.Errorf("unclosed think block not stripped: %q", got)
	}
	if got != "before" {
		t.Errorf("expected 'before', got %q", got)
	}
}

func TestPreprocessContent_NoThinkBlock(t *testing.T) {
	in := "Normal response without any think blocks."
	got := preprocessContent(in)
	if got != in {
		t.Errorf("content changed unexpectedly: %q", got)
	}
}

func TestPreprocessContent_TruncatesOverlong(t *testing.T) {
	// Build a string longer than maxPreprocessChars with a newline at position 45k.
	var sb strings.Builder
	for sb.Len() < 45_000 {
		sb.WriteString("word ")
	}
	sb.WriteString("\n")
	for sb.Len() < 60_000 {
		sb.WriteString("extra ")
	}
	got := preprocessContent(sb.String())
	if len(got) > maxPreprocessChars {
		t.Errorf("output len %d exceeds maxPreprocessChars %d", len(got), maxPreprocessChars)
	}
}

func TestPreprocessContent_EmptyAfterStrip(t *testing.T) {
	in := "<think>everything is inside the think block</think>"
	got := preprocessContent(in)
	if got != "" {
		t.Errorf("expected empty string after full strip, got %q", got)
	}
}

func TestPreScore_NilScorer(t *testing.T) {
	emb := &mockEmbedder{dim: 4}
	st := &mockStore{}
	wp := NewWritePipeline(emb, st) // no scorer attached

	_, ok := wp.PreScore(context.Background(), "some text")
	if ok {
		t.Error("PreScore should return false when scorer is nil")
	}
	if emb.calls != 0 {
		t.Error("PreScore should not call embedder when scorer is nil")
	}
}

func TestPreScore_WithScorer(t *testing.T) {
	emb := &mockEmbedder{dim: 4}
	st := &mockStore{}

	// Build a scorer using the mock embedder. The mock produces orthogonal
	// vectors per call, so quality/noise prototypes will be distinct.
	scorer, err := quality.NewContentScorer(context.Background(), emb)
	if err != nil {
		t.Fatalf("NewContentScorer: %v", err)
	}

	wp := NewWritePipeline(emb, st,
		WithContentScorer(scorer),
	)

	score, ok := wp.PreScore(context.Background(), "technical content here")
	if !ok {
		t.Error("PreScore should return true when scorer is non-nil")
	}
	if score < 0 || score > 1 {
		t.Errorf("PreScore returned out-of-range score: %f", score)
	}
}
