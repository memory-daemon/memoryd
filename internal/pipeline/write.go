package pipeline

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"unicode"

	"github.com/memory-daemon/memoryd/internal/chunker"
	"github.com/memory-daemon/memoryd/internal/config"
	"github.com/memory-daemon/memoryd/internal/embedding"
	"github.com/memory-daemon/memoryd/internal/quality"
	"github.com/memory-daemon/memoryd/internal/redact"
	"github.com/memory-daemon/memoryd/internal/store"
	"github.com/memory-daemon/memoryd/internal/synthesizer"
)

// DedupThreshold is the cosine-similarity score above which a new chunk
// is considered a duplicate of an existing memory.
const DedupThreshold = 0.92

// SourceExtensionThreshold: if a new chunk scores this high against a source
// memory (but below DedupThreshold), tag it as extending that source.
const SourceExtensionThreshold = 0.75

// TopicBoundaryThreshold is the minimum cosine similarity between consecutive
// chunks for them to be considered part of the same topic. When similarity
// drops below this, a topic boundary is detected.
const TopicBoundaryThreshold = 0.65

// maxGroupChars is the maximum combined character length for a topic group.
// Set to match the embedding model's context window (512 tokens * ~4 chars/token).
// Groups are split at this boundary to ensure the re-embedded vector accurately
// represents the full joined text.
const maxGroupChars = 2048

// joinSeparator is inserted between chunks when merging a topic group.
const joinSeparator = "\n\n"

// minContentLen is the minimum character length for content to be worth storing.
const minContentLen = 20

// WriteResult reports what the write pipeline did with the input.
type WriteResult struct {
	Stored     int // memories actually inserted (after topic merging)
	Duplicates int // memories skipped as near-duplicates
	Filtered   int // chunks skipped as noise
	Extended   int // memories that extend source content
	Merged     int // chunks merged into topic groups before storing
}

func (r WriteResult) Summary() string {
	parts := []string{}
	if r.Stored > 0 {
		parts = append(parts, fmt.Sprintf("%d stored", r.Stored))
	}
	if r.Extended > 0 {
		parts = append(parts, fmt.Sprintf("%d extend source", r.Extended))
	}
	if r.Duplicates > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped (duplicate)", r.Duplicates))
	}
	if r.Merged > 0 {
		parts = append(parts, fmt.Sprintf("%d merged (topic grouping)", r.Merged))
	}
	if r.Filtered > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped (too short/noisy)", r.Filtered))
	}
	if len(parts) == 0 {
		return "Nothing to store."
	}
	return strings.Join(parts, ", ") + "."
}

// WritePipeline handles the post-response path: chunk the text,
// embed each chunk, and write to the store. Designed to run in a goroutine.
type WritePipeline struct {
	embedder embedding.Embedder
	store    store.Store
	synth    *synthesizer.Synthesizer

	mu     sync.RWMutex
	scorer *quality.ContentScorer
	pCfg   config.PipelineConfig
}

// WriteOption configures the WritePipeline.
type WriteOption func(*WritePipeline)

// WithContentScorer attaches a content scorer to the write pipeline.
func WithContentScorer(cs *quality.ContentScorer) WriteOption {
	return func(wp *WritePipeline) { wp.scorer = cs }
}

// WithSynthesizer attaches an LLM synthesizer for topic group distillation.
func WithSynthesizer(s *synthesizer.Synthesizer) WriteOption {
	return func(wp *WritePipeline) { wp.synth = s }
}

// WithPipelineConfig sets the initial pipeline configuration.
func WithPipelineConfig(cfg config.PipelineConfig) WriteOption {
	return func(wp *WritePipeline) { wp.pCfg = cfg }
}

func NewWritePipeline(e embedding.Embedder, s store.Store, opts ...WriteOption) *WritePipeline {
	wp := &WritePipeline{
		embedder: e,
		store:    s,
		pCfg:     config.Default.Pipeline,
	}
	for _, o := range opts {
		o(wp)
	}
	return wp
}

// Config returns a snapshot of the current pipeline configuration.
func (wp *WritePipeline) Config() config.PipelineConfig {
	wp.mu.RLock()
	defer wp.mu.RUnlock()
	return wp.pCfg
}

// UpdateConfig atomically replaces the pipeline configuration.
func (wp *WritePipeline) UpdateConfig(cfg config.PipelineConfig) {
	wp.mu.Lock()
	defer wp.mu.Unlock()
	wp.pCfg = cfg
}

// UpdateScorer atomically replaces the content scorer (e.g. after prototype changes).
func (wp *WritePipeline) UpdateScorer(scorer *quality.ContentScorer) {
	wp.mu.Lock()
	defer wp.mu.Unlock()
	wp.scorer = scorer
}

// ProcessDirect embeds and stores pre-formatted text without chunking.
// Use this for Q&A pairs and session summaries that are already well-structured.
// Safe to call from a goroutine — errors are logged, not returned.
func (wp *WritePipeline) ProcessDirect(text, source string, metadata map[string]any) WriteResult {
	var result WriteResult
	if wp.embedder == nil || wp.store == nil {
		return result
	}

	wp.mu.RLock()
	pCfg := wp.pCfg
	wp.mu.RUnlock()

	text = preprocessContent(text)
	if isNoise(text, pCfg.NoiseMinLen, pCfg.NoiseMinAlnumRatio) {
		result.Filtered++
		return result
	}

	text = redact.Clean(text)
	ctx := context.Background()

	vec, err := wp.embedder.Embed(ctx, text)
	if err != nil {
		log.Printf("[write] ProcessDirect embed error: %v", err)
		return result
	}
	wp.storeMemory(ctx, text, vec, source, metadata, &result)
	return result
}

// Process chunks, embeds, deduplicates, and stores the text.
// Safe to call from a goroutine -- errors are logged, not returned.
//
// Note: in proxy mode each agent response is a separate Process() call,
// so topic grouping rarely activates (responses are typically single-topic).
// Grouping mainly benefits MCP memory_store calls and source ingestion where
// larger, multi-topic texts are common.
func (wp *WritePipeline) Process(text, source string, metadata map[string]any) {
	wp.ProcessFiltered(text, source, metadata)
}

// ProcessFiltered is like Process but returns a WriteResult describing
// what happened (stored, deduplicated, filtered).
func (wp *WritePipeline) ProcessFiltered(text, source string, metadata map[string]any) WriteResult {
	var result WriteResult

	if wp.embedder == nil || wp.store == nil {
		return result
	}

	wp.mu.RLock()
	pCfg := wp.pCfg
	wp.mu.RUnlock()

	text = preprocessContent(text)
	if text == "" {
		return result
	}

	ctx := context.Background()

	chunks := chunker.Chunk(text, chunker.DefaultMaxTokens)

	// First pass: filter noise and redact, collect valid chunks.
	var validChunks []string
	for _, chunk := range chunks {
		if isNoise(chunk, pCfg.NoiseMinLen, pCfg.NoiseMinAlnumRatio) {
			result.Filtered++
			log.Printf("[write] filtered noisy chunk (%d chars)", len(chunk))
			continue
		}
		validChunks = append(validChunks, redact.Clean(chunk))
	}

	if len(validChunks) == 0 {
		return result
	}

	// Single chunk: embed and store directly — no topic detection needed.
	if len(validChunks) == 1 {
		vec, err := wp.embedder.Embed(ctx, validChunks[0])
		if err != nil {
			log.Printf("[write] embedding error: %v", err)
			return result
		}
		wp.storeMemory(ctx, validChunks[0], vec, source, metadata, &result)
		return result
	}

	// Multi-chunk: batch-embed for topic boundary detection.
	vecs, err := wp.embedder.EmbedBatch(ctx, validChunks)
	if err != nil {
		log.Printf("[write] batch embedding error: %v", err)
		return result
	}

	// Detect topic boundaries by walking consecutive cosine similarities.
	groups := detectTopicGroups(validChunks, vecs, pCfg.TopicBoundaryThreshold, pCfg.MaxGroupChars)
	log.Printf("[write] detected %d topic group(s) from %d chunks", len(groups), len(validChunks))

	for _, g := range groups {
		if len(g.chunks) == 1 {
			// Single-chunk group: use pre-computed embedding directly.
			wp.storeMemory(ctx, g.chunks[0], g.vecs[0], source, metadata, &result)
		} else {
			// Multi-chunk group: synthesize or join, then re-embed.
			var text string
			if wp.synth.Available() {
				synthesized, err := wp.synth.Synthesize(ctx, g.chunks)
				if err != nil {
					log.Printf("[write] synthesis error for topic group (%d chunks): %v — falling back to join", len(g.chunks), err)
					text = strings.Join(g.chunks, "\n\n")
				} else {
					text = synthesized
					log.Printf("[write] synthesized %d chunks into 1 topic memory (%d chars)", len(g.chunks), len(text))
				}
			} else {
				text = strings.Join(g.chunks, "\n\n")
			}

			vec, err := wp.embedder.Embed(ctx, text)
			if err != nil {
				log.Printf("[write] re-embed error for topic group (%d chunks): %v", len(g.chunks), err)
				// Fall back to storing chunks individually with pre-computed embeddings.
				for i, chunk := range g.chunks {
					wp.storeMemory(ctx, chunk, g.vecs[i], source, metadata, &result)
				}
				continue
			}
			result.Merged += len(g.chunks) - 1
			wp.storeMemory(ctx, text, vec, source, metadata, &result)
		}
	}
	return result
}

// storeMemory dedup-checks and inserts a single memory.
func (wp *WritePipeline) storeMemory(ctx context.Context, content string, vec []float32, source string, metadata map[string]any, result *WriteResult) {
	wp.mu.RLock()
	scorer := wp.scorer
	pCfg := wp.pCfg
	wp.mu.RUnlock()

	// Content score gate — drop low-quality chunks before dedup and storage.
	contentScore := scorer.Score(vec) // nil-safe: returns 0.5 when scorer unavailable
	if pCfg.ContentScoreGate > 0 && contentScore < pCfg.ContentScoreGate {
		result.Filtered++
		log.Printf("[write] filtered low-quality chunk (score=%.2f < gate=%.2f)", contentScore, pCfg.ContentScoreGate)
		return
	}

	isDup, closest := wp.checkDuplicate(ctx, vec, pCfg.DedupThreshold)
	if isDup {
		result.Duplicates++
		log.Printf("[write] skipped duplicate memory")
		return
	}

	chunkMeta := metadata
	if closest != nil && strings.HasPrefix(closest.Source, "source:") &&
		closest.Score >= pCfg.SourceExtensionThreshold {
		if chunkMeta == nil {
			chunkMeta = map[string]any{}
		} else {
			copied := make(map[string]any, len(chunkMeta)+3)
			for k, v := range chunkMeta {
				copied[k] = v
			}
			chunkMeta = copied
		}
		chunkMeta["extends_source"] = closest.Source
		chunkMeta["extends_memory"] = closest.ID.Hex()
		chunkMeta["extends_score"] = closest.Score
		result.Extended++
		log.Printf("[write] tagged as extending source %s (score %.2f)", closest.Source, closest.Score)
	}

	mem := store.Memory{
		Content:      content,
		Embedding:    vec,
		Source:       source,
		Metadata:     chunkMeta,
		ContentScore: contentScore,
	}

	if err := wp.store.Insert(ctx, mem); err != nil {
		log.Printf("[write] store error: %v", err)
		return
	}
	result.Stored++
}

// checkDuplicate returns whether the vector is a duplicate and the closest match.
func (wp *WritePipeline) checkDuplicate(ctx context.Context, vec []float32, dedupThreshold float64) (bool, *store.Memory) {
	matches, err := wp.store.VectorSearch(ctx, vec, 1)
	if err != nil {
		log.Printf("[write] dedup search error (storing anyway): %v", err)
		return false, nil
	}
	if len(matches) == 0 {
		return false, nil
	}
	if matches[0].Score >= dedupThreshold {
		return true, &matches[0]
	}
	return false, &matches[0]
}

// topicGroup is a contiguous run of chunks about the same topic.
type topicGroup struct {
	chunks []string
	vecs   [][]float32
}

// detectTopicGroups walks consecutive chunk embeddings and splits at topic
// boundaries — points where cosine similarity between adjacent chunks drops
// below the threshold. Groups are also split when accumulated text would
// exceed maxChars, ensuring every merged memory fits within the embedding
// model's context window.
func detectTopicGroups(chunks []string, vecs [][]float32, threshold float64, maxChars int) []topicGroup {
	if len(chunks) == 0 {
		return nil
	}
	if len(chunks) == 1 {
		return []topicGroup{{chunks: chunks, vecs: vecs}}
	}

	var groups []topicGroup
	start := 0
	groupLen := len(chunks[0]) // running char count for current group

	for i := 1; i < len(chunks); i++ {
		nextLen := len(joinSeparator) + len(chunks[i])
		sim := cosineSim(vecs[i-1], vecs[i])

		// Split on topic boundary OR when adding the next chunk would
		// exceed the embedding model's context window.
		if sim < threshold || groupLen+nextLen > maxChars {
			groups = append(groups, topicGroup{
				chunks: chunks[start:i],
				vecs:   vecs[start:i],
			})
			start = i
			groupLen = len(chunks[i])
		} else {
			groupLen += nextLen
		}
	}
	// Emit the final group.
	groups = append(groups, topicGroup{
		chunks: chunks[start:],
		vecs:   vecs[start:],
	})

	return groups
}

// cosineSim computes cosine similarity between two vectors.
func cosineSim(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return dot / denom
}

// maxPreprocessChars is the maximum input length accepted by the write pipeline.
// Entries larger than this (e.g. full multi-file implementation dumps) produce
// hundreds of noisy chunks and strain the embedding batch. Content is truncated
// at the last newline before this limit.
const maxPreprocessChars = 50_000

// preprocessContent normalises raw text before it reaches the chunker:
//  1. Strips <think>...</think> blocks — Claude extended-thinking output contains
//     the model's reasoning process, which is not durable knowledge.
//  2. Enforces maxPreprocessChars — oversized entries (code dumps, full session
//     transcripts) are truncated rather than rejected so we still capture the
//     opening context, which is usually the most informative part.
func preprocessContent(text string) string {
	// Strip all <think>...</think> blocks iteratively.
	for {
		start := strings.Index(text, "<think>")
		if start < 0 {
			break
		}
		end := strings.Index(text[start:], "</think>")
		if end < 0 {
			// Unclosed tag — drop everything from <think> to end of string.
			text = strings.TrimSpace(text[:start])
			break
		}
		text = text[:start] + text[start+end+len("</think>"):]
	}
	text = strings.TrimSpace(text)
	if len(text) > maxPreprocessChars {
		log.Printf("[write] input too long (%d chars), truncating to %d", len(text), maxPreprocessChars)
		cutoff := strings.LastIndexByte(text[:maxPreprocessChars], '\n')
		if cutoff < maxPreprocessChars/2 {
			cutoff = maxPreprocessChars // no suitable newline nearby, hard cut
		}
		text = strings.TrimSpace(text[:cutoff])
	}
	return text
}

// isNoise returns true if the text is too short, mostly whitespace,
// or lacks meaningful alphanumeric content.
func isNoise(text string, minLen int, minAlnum float64) bool {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) < minLen {
		return true
	}

	// Count alphanumeric characters.
	var alnumCount int
	for _, r := range trimmed {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			alnumCount++
		}
	}

	return float64(alnumCount)/float64(len(trimmed)) < minAlnum
}
