package steward

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/memory-daemon/memoryd/internal/embedding"
	"github.com/memory-daemon/memoryd/internal/quality"
	"github.com/memory-daemon/memoryd/internal/store"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Config tunes the steward's behavior.
type Config struct {
	// How often the steward runs a sweep (default 1h).
	Interval time.Duration

	// Memories below this quality_score get pruned (default 0.1).
	PruneThreshold float64

	// Minimum age before a memory can be pruned (default 24h).
	PruneGracePeriod time.Duration

	// Score decay half-life: how long until an unretrieved memory loses half its score (default 90d).
	DecayHalfLife time.Duration

	// Cosine similarity above which two memories are candidates for merging (default 0.88).
	MergeThreshold float64

	// Max memories to scan per sweep (default 500). Keeps each cycle bounded.
	BatchSize int

	// NowFunc overrides time.Now() for testing. If nil, uses time.Now.
	NowFunc func() time.Time
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{
		Interval:         1 * time.Hour,
		PruneThreshold:   0.1,
		PruneGracePeriod: 24 * time.Hour,
		DecayHalfLife:    90 * 24 * time.Hour,
		MergeThreshold:   0.88,
		BatchSize:        500,
	}
}

// StewardStore extends the base store with operations the steward needs.
// MongoStore implements this via the new methods we add.
type StewardStore interface {
	store.Store

	// ListOldest returns the oldest memories (by created_at), including embeddings.
	ListOldest(ctx context.Context, limit int) ([]store.Memory, error)

	// UpdateQualityScore sets the quality_score for a memory.
	UpdateQualityScore(ctx context.Context, id primitive.ObjectID, score float64) error
}

// Stats captures what a single sweep accomplished.
type Stats struct {
	Scored  int
	Pruned  int
	Merged  int
	Elapsed time.Duration
}

func (s Stats) String() string {
	return fmt.Sprintf("scored=%d pruned=%d merged=%d elapsed=%s",
		s.Scored, s.Pruned, s.Merged, s.Elapsed.Round(time.Millisecond))
}

// scoreQuerier is satisfied by stores that track per-retrieval similarity
// scores. The Steward uses a type assertion so stores without this capability
// degrade to frequency-based scoring without any interface change.
type scoreQuerier interface {
	AverageRetrievalScore(ctx context.Context, id primitive.ObjectID) (float64, bool, error)
}

// Steward is a long-running background service that maintains memory quality.
type Steward struct {
	cfg      Config
	store    StewardStore
	embedder embedding.Embedder

	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu        sync.Mutex
	lastStats Stats
}

// New creates a steward. Call Start() to begin the background loop.
// The embedder is optional — if nil, merges will keep the longer memory
// rather than combining content (no re-embedding available).
func New(cfg Config, s StewardStore, emb embedding.Embedder) *Steward {
	if cfg.Interval == 0 {
		cfg = DefaultConfig()
	}
	return &Steward{
		cfg:      cfg,
		store:    s,
		embedder: emb,
	}
}

// now returns the current time, using NowFunc if configured.
func (s *Steward) now() time.Time {
	if s.cfg.NowFunc != nil {
		return s.cfg.NowFunc()
	}
	return time.Now()
}

// Start begins the steward loop in a background goroutine.
// It runs one sweep immediately, then on the configured interval.
func (s *Steward) Start(ctx context.Context) {
	ctx, s.cancel = context.WithCancel(ctx)
	s.wg.Add(1)
	go s.loop(ctx)
}

// Stop gracefully shuts down the steward and waits for the current sweep to finish.
func (s *Steward) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
}

// LastStats returns the results of the most recent sweep.
func (s *Steward) LastStats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastStats
}

// Sweep runs a single scoring + pruning + merging cycle synchronously.
// Useful for testing and validation where you want direct control.
func (s *Steward) Sweep(ctx context.Context) Stats {
	s.sweep(ctx)
	return s.LastStats()
}

func (s *Steward) loop(ctx context.Context) {
	defer s.wg.Done()

	// Run one sweep immediately at startup.
	s.sweep(ctx)

	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("[steward] shutting down")
			return
		case <-ticker.C:
			s.sweep(ctx)
		}
	}
}

func (s *Steward) sweep(ctx context.Context) {
	start := time.Now()
	log.Println("[steward] starting sweep")

	stats := Stats{}

	// Phase 1: Score all memories using decay function.
	scored, err := s.scoreMemories(ctx)
	if err != nil {
		log.Printf("[steward] scoring error: %v", err)
		return
	}
	stats.Scored = scored

	// Phase 2: Prune low-quality memories past grace period.
	pruned, err := s.pruneMemories(ctx)
	if err != nil {
		log.Printf("[steward] pruning error: %v", err)
	}
	stats.Pruned = pruned

	// Phase 3: Merge near-duplicate clusters.
	merged, err := s.mergeNearDuplicates(ctx)
	if err != nil {
		log.Printf("[steward] merge error: %v", err)
	}
	stats.Merged = merged

	stats.Elapsed = time.Since(start)

	s.mu.Lock()
	s.lastStats = stats
	s.mu.Unlock()

	log.Printf("[steward] sweep complete: %s", stats)
}

// scoreMemories computes quality_score for each memory based on:
//   - hit_count: more retrievals = higher base score
//   - recency of last retrieval: decays with half-life
//   - age: older memories get a small boost for surviving this long
//
// Formula: quality_score = baseScore * decayFactor
//
//	where baseScore = log2(hit_count + 1) / log2(maxHits + 1)
//	      decayFactor = 0.5 ^ (timeSinceLastRetrieval / halfLife)
//	Fresh memories with no retrievals start at 0.5 and decay from creation time.
func (s *Steward) scoreMemories(ctx context.Context) (int, error) {
	memories, err := s.store.ListOldest(ctx, s.cfg.BatchSize)
	if err != nil {
		return 0, fmt.Errorf("listing memories: %w", err)
	}

	if len(memories) == 0 {
		return 0, nil
	}

	// Find max hit_count for normalization.
	maxHits := 1
	for _, m := range memories {
		if m.HitCount > maxHits {
			maxHits = m.HitCount
		}
	}

	now := s.now()
	scored := 0

	sq, hasSQ := s.store.(scoreQuerier)

	for _, m := range memories {
		select {
		case <-ctx.Done():
			return scored, ctx.Err()
		default:
		}

		// Base score: average retrieval similarity when available (preferred),
		// falling back to normalized hit frequency. A memory retrieved at high
		// cosine similarity is more valuable than one retrieved often at low
		// similarity — this is the right signal for non-obvious facts.
		var baseScore float64
		if hasSQ && m.HitCount > 0 {
			if avg, ok, err := sq.AverageRetrievalScore(ctx, m.ID); err == nil && ok {
				baseScore = avg
			} else {
				// Fallback: frequency-normalized score.
				baseScore = math.Log2(float64(m.HitCount)+1) / math.Log2(float64(maxHits)+1)
			}
		} else if m.HitCount > 0 {
			baseScore = math.Log2(float64(m.HitCount)+1) / math.Log2(float64(maxHits)+1)
		} else {
			baseScore = 0.5 // benefit of doubt for new memories
		}

		// Decay factor based on time since last useful retrieval.
		// Content score scales the effective half-life: low-quality chunks
		// decay faster. Existing memories with no content_score (zero value)
		// keep the full half-life so they aren't retroactively penalised.
		lastActive := m.LastRetrieved
		if lastActive.IsZero() {
			lastActive = m.CreatedAt
		}
		elapsed := now.Sub(lastActive)
		halfLife := float64(s.cfg.DecayHalfLife)
		if m.ContentScore > 0 {
			halfLife = quality.ContentScaleHalfLife(halfLife, m.ContentScore)
		}
		decayFactor := math.Pow(0.5, float64(elapsed)/halfLife)

		score := baseScore * decayFactor

		// Clamp to [0, 1].
		if score > 1.0 {
			score = 1.0
		}
		if score < 0.0 {
			score = 0.0
		}

		if err := s.store.UpdateQualityScore(ctx, m.ID, score); err != nil {
			log.Printf("[steward] failed to score memory %s: %v", m.ID.Hex(), err)
			continue
		}
		scored++
	}

	return scored, nil
}

// pruneMemories deletes low-scoring memories that are older than the grace period.
func (s *Steward) pruneMemories(ctx context.Context) (int, error) {
	memories, err := s.store.ListOldest(ctx, s.cfg.BatchSize)
	if err != nil {
		return 0, fmt.Errorf("listing for prune: %w", err)
	}

	now := s.now()
	pruned := 0

	for _, m := range memories {
		select {
		case <-ctx.Done():
			return pruned, ctx.Err()
		default:
		}

		// Skip memories still in grace period.
		if now.Sub(m.CreatedAt) < s.cfg.PruneGracePeriod {
			continue
		}

		// Skip memories above the quality threshold.
		if m.QualityScore >= s.cfg.PruneThreshold {
			continue
		}

		// Skip memories that have ever been retrieved — they proved useful once.
		if m.HitCount > 0 {
			continue
		}

		if err := s.store.Delete(ctx, m.ID.Hex()); err != nil {
			log.Printf("[steward] failed to prune %s: %v", m.ID.Hex(), err)
			continue
		}
		log.Printf("[steward] pruned memory %s (score=%.3f, age=%s)",
			m.ID.Hex(), m.QualityScore, now.Sub(m.CreatedAt).Round(time.Hour))
		pruned++
	}

	return pruned, nil
}

// mergeNearDuplicates finds pairs of memories with high cosine similarity
// and consolidates them. When an embedder is available, the two memories'
// content is combined and re-embedded so that the merged memory is richer
// than either original. Without an embedder, we keep the longer memory.
func (s *Steward) mergeNearDuplicates(ctx context.Context) (int, error) {
	// Only scan a subset to keep each sweep bounded.
	scanLimit := s.cfg.BatchSize
	if scanLimit > 200 {
		scanLimit = 200 // cap merge scan since it's O(n) vector searches
	}

	memories, err := s.store.ListOldest(ctx, scanLimit)
	if err != nil {
		return 0, fmt.Errorf("listing for merge: %w", err)
	}

	merged := 0
	deleted := make(map[primitive.ObjectID]bool) // track already-deleted to avoid double-delete

	for _, m := range memories {
		select {
		case <-ctx.Done():
			return merged, ctx.Err()
		default:
		}

		if deleted[m.ID] {
			continue
		}

		// Skip memories without embeddings (shouldn't happen but be safe).
		if len(m.Embedding) == 0 {
			continue
		}

		// Find nearest neighbors.
		neighbors, err := s.store.VectorSearch(ctx, m.Embedding, 5)
		if err != nil {
			continue
		}

		for _, n := range neighbors {
			if n.ID == m.ID || deleted[n.ID] {
				continue
			}

			// Score comes from VectorSearch as cosine similarity.
			if n.Score < s.cfg.MergeThreshold {
				continue
			}

			// Decide which memory to keep and which to absorb.
			keep, drop := m, n
			if n.HitCount > m.HitCount || (n.HitCount == m.HitCount && n.CreatedAt.Before(m.CreatedAt)) {
				keep, drop = n, m
			}

			// Try to combine content if the drop has unique information.
			if s.embedder != nil && !isSubstring(keep.Content, drop.Content) {
				combined := combineContent(keep.Content, drop.Content)
				vec, err := s.embedder.Embed(ctx, combined)
				if err != nil {
					log.Printf("[steward] re-embed failed during merge, keeping longer: %v", err)
				} else {
					if err := s.store.UpdateContent(ctx, keep.ID.Hex(), combined, vec); err != nil {
						log.Printf("[steward] update content failed for %s: %v", keep.ID.Hex(), err)
					}
				}
			}

			if err := s.store.Delete(ctx, drop.ID.Hex()); err != nil {
				log.Printf("[steward] merge delete failed for %s: %v", drop.ID.Hex(), err)
				continue
			}

			deleted[drop.ID] = true
			merged++
			log.Printf("[steward] merged: kept %s (hits=%d) absorbed %s (hits=%d, sim=%.3f)",
				keep.ID.Hex(), keep.HitCount, drop.ID.Hex(), drop.HitCount, n.Score)
		}
	}

	return merged, nil
}

// isSubstring returns true if the shorter text is wholly contained in the longer.
func isSubstring(a, b string) bool {
	if len(a) >= len(b) {
		return strings.Contains(a, b)
	}
	return strings.Contains(b, a)
}

// combineContent merges two similar memories into one. When the texts overlap
// heavily (one is a prefix/suffix of the other), we extend rather than
// concatenate. Otherwise, we join with a clear separator, keeping the longer
// text first for coherence.
func combineContent(keep, drop string) string {
	// If drop is already contained in keep, nothing to add.
	if strings.Contains(keep, drop) {
		return keep
	}
	// If keep is contained in drop, the drop is the richer version.
	if strings.Contains(drop, keep) {
		return drop
	}

	// Look for a suffix/prefix overlap (at least 20 chars) to splice.
	if merged, ok := spliceOverlap(keep, drop, 20); ok {
		return merged
	}

	// No overlap — concatenate with separator, longer first.
	if len(drop) > len(keep) {
		keep, drop = drop, keep
	}
	return keep + "\n\n" + drop
}

// spliceOverlap finds where the end of a overlaps the start of b (or vice
// versa) and returns the combined text. Returns false if no overlap >= minLen.
func spliceOverlap(a, b string, minLen int) (string, bool) {
	// Try a's suffix overlapping b's prefix.
	if merged, ok := trySuffix(a, b, minLen); ok {
		return merged, true
	}
	// Try b's suffix overlapping a's prefix.
	if merged, ok := trySuffix(b, a, minLen); ok {
		return merged, true
	}
	return "", false
}

func trySuffix(a, b string, minLen int) (string, bool) {
	maxCheck := len(a)
	if maxCheck > len(b) {
		maxCheck = len(b)
	}
	for i := maxCheck; i >= minLen; i-- {
		if a[len(a)-i:] == b[:i] {
			return a + b[i:], true
		}
	}
	return "", false
}
