package store

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// DatabaseEntry holds a named store with its role.
type DatabaseEntry struct {
	Name     string // Human label (e.g., "platform", "payments")
	Database string // MongoDB database name
	Role     string // "full" or "read-only"

	Store       Store       // base store (MongoStore)
	SearchStore Store       // may be AtlasStore (HybridSearcher) or MongoStore
	Mongo       *MongoStore // direct access for steward, quality, sources
}

// IsWritable returns true if this database accepts writes.
func (de *DatabaseEntry) IsWritable() bool {
	return de.Role == "" || de.Role == "full"
}

// MultiStore fans out reads across multiple databases and routes writes
// to the appropriate one. It implements Store and HybridSearcher.
type MultiStore struct {
	entries  []DatabaseEntry
	byName   map[string]*DatabaseEntry
	primary  *DatabaseEntry // first writable entry — default write target
}

// NewMultiStore creates a fan-out store across multiple databases.
// At least one entry must have role "full".
func NewMultiStore(entries []DatabaseEntry) (*MultiStore, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("at least one database entry is required")
	}

	ms := &MultiStore{
		entries: entries,
		byName:  make(map[string]*DatabaseEntry, len(entries)),
	}

	for i := range entries {
		ms.byName[entries[i].Name] = &entries[i]
		if entries[i].IsWritable() && ms.primary == nil {
			ms.primary = &entries[i]
		}
	}

	if ms.primary == nil {
		return nil, fmt.Errorf("at least one database must have role 'full'")
	}

	// When multiple databases are configured, only the primary accepts writes.
	// External knowledge stores from other teams are always read-only.
	if len(ms.entries) > 1 {
		for i := range ms.entries {
			if &ms.entries[i] != ms.primary && ms.entries[i].IsWritable() {
				log.Printf("[multistore] %s: enforcing read-only (only primary %q is writable)", ms.entries[i].Name, ms.primary.Name)
				ms.entries[i].Role = "read-only"
			}
		}
	}

	return ms, nil
}

// Entries returns all database entries.
func (ms *MultiStore) Entries() []DatabaseEntry {
	return ms.entries
}

// Primary returns the default write-target database entry.
func (ms *MultiStore) Primary() *DatabaseEntry {
	return ms.primary
}

// Entry returns a database entry by name.
func (ms *MultiStore) Entry(name string) (*DatabaseEntry, bool) {
	e, ok := ms.byName[name]
	return e, ok
}

// VectorSearch fans out across all databases, merges by score, returns top-k.
func (ms *MultiStore) VectorSearch(ctx context.Context, embedding []float32, topK int) ([]Memory, error) {
	return ms.fanOutSearch(ctx, func(e *DatabaseEntry) ([]Memory, error) {
		return e.SearchStore.VectorSearch(ctx, embedding, topK)
	}, topK)
}

// HybridSearch fans out across all databases that support it, merges results.
func (ms *MultiStore) HybridSearch(ctx context.Context, embedding []float32, topK int, opts SearchOptions) ([]Memory, error) {
	return ms.fanOutSearch(ctx, func(e *DatabaseEntry) ([]Memory, error) {
		if hs, ok := e.SearchStore.(HybridSearcher); ok {
			return hs.HybridSearch(ctx, embedding, topK, opts)
		}
		return e.SearchStore.VectorSearch(ctx, embedding, topK)
	}, topK)
}

// fanOutSearch runs a search function against all databases in parallel,
// merges results by score, and returns the top-k.
func (ms *MultiStore) fanOutSearch(ctx context.Context, searchFn func(*DatabaseEntry) ([]Memory, error), topK int) ([]Memory, error) {
	type result struct {
		memories []Memory
		name     string
		err      error
	}

	results := make(chan result, len(ms.entries))
	var wg sync.WaitGroup

	for i := range ms.entries {
		wg.Add(1)
		go func(e *DatabaseEntry) {
			defer wg.Done()
			mems, err := searchFn(e)
			if err != nil {
				log.Printf("[multistore] search error on %s: %v", e.Name, err)
			}
			// Tag each memory with its database for provenance.
			for j := range mems {
				if mems[j].Metadata == nil {
					mems[j].Metadata = map[string]any{}
				}
				mems[j].Metadata["database"] = e.Name
			}
			results <- result{memories: mems, name: e.Name, err: err}
		}(&ms.entries[i])
	}

	wg.Wait()
	close(results)

	var all []Memory
	var lastErr error
	for r := range results {
		if r.err != nil {
			lastErr = r.err
			continue
		}
		all = append(all, r.memories...)
	}

	if len(all) == 0 && lastErr != nil {
		return nil, lastErr
	}

	// Sort by score descending and take top-k.
	sort.Slice(all, func(i, j int) bool {
		return all[i].Score > all[j].Score
	})

	if len(all) > topK {
		all = all[:topK]
	}

	return all, nil
}

// Insert writes to the primary (default) database.
func (ms *MultiStore) Insert(ctx context.Context, mem Memory) error {
	return ms.primary.Store.Insert(ctx, mem)
}

// Delete removes a memory from all databases (checks each one).
func (ms *MultiStore) Delete(ctx context.Context, id string) error {
	for _, e := range ms.entries {
		if !e.IsWritable() {
			continue
		}
		if err := e.Store.Delete(ctx, id); err != nil {
			// ObjectID not found in this database is fine — try the next.
			continue
		}
	}
	return nil
}

// List returns memories from all databases, merged.
func (ms *MultiStore) List(ctx context.Context, query string, limit int) ([]Memory, error) {
	var all []Memory
	for _, e := range ms.entries {
		mems, err := e.Store.List(ctx, query, limit)
		if err != nil {
			log.Printf("[multistore] list error on %s: %v", e.Name, err)
			continue
		}
		for j := range mems {
			if mems[j].Metadata == nil {
				mems[j].Metadata = map[string]any{}
			}
			mems[j].Metadata["database"] = e.Name
		}
		all = append(all, mems...)
	}
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// DeleteAll removes all memories from all writable databases.
func (ms *MultiStore) DeleteAll(ctx context.Context) error {
	for _, e := range ms.entries {
		if !e.IsWritable() {
			continue
		}
		if err := e.Store.DeleteAll(ctx); err != nil {
			return fmt.Errorf("delete all on %s: %w", e.Name, err)
		}
	}
	return nil
}

// CountBySource counts across all databases.
func (ms *MultiStore) CountBySource(ctx context.Context, source string) (int64, error) {
	var total int64
	for _, e := range ms.entries {
		n, err := e.Store.CountBySource(ctx, source)
		if err != nil {
			continue
		}
		total += n
	}
	return total, nil
}

// UpdateContent updates on all writable databases (the memory lives in one).
func (ms *MultiStore) UpdateContent(ctx context.Context, id string, content string, embedding []float32) error {
	for _, e := range ms.entries {
		if !e.IsWritable() {
			continue
		}
		if err := e.Store.UpdateContent(ctx, id, content, embedding); err != nil {
			continue
		}
		return nil
	}
	return fmt.Errorf("memory %s not found in any writable database", id)
}

// ListBySource returns from all databases.
func (ms *MultiStore) ListBySource(ctx context.Context, sourcePrefix string, limit int) ([]Memory, error) {
	var all []Memory
	for _, e := range ms.entries {
		mems, err := e.Store.ListBySource(ctx, sourcePrefix, limit)
		if err != nil {
			continue
		}
		all = append(all, mems...)
	}
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// Close closes all underlying stores.
func (ms *MultiStore) Close() error {
	for _, e := range ms.entries {
		e.Store.Close()
	}
	return nil
}

// --- QualityStore delegation --- //

// RecordRetrievalBatch records across all databases' quality stores.
// Events are grouped by memory source database.
func (ms *MultiStore) RecordRetrievalBatch(ctx context.Context, events []RetrievalEvent) error {
	// For simplicity, record on primary. Quality events are global.
	return ms.primary.Mongo.RecordRetrievalBatch(ctx, events)
}

func (ms *MultiStore) GetRetrievalCount(ctx context.Context) (int64, error) {
	var total int64
	for _, e := range ms.entries {
		n, err := e.Mongo.GetRetrievalCount(ctx)
		if err != nil {
			continue
		}
		total += n
	}
	return total, nil
}

func (ms *MultiStore) IncrementHitCount(ctx context.Context, id primitive.ObjectID) error {
	// Try each store — the memory lives in one.
	for _, e := range ms.entries {
		if err := e.Mongo.IncrementHitCount(ctx, id); err != nil {
			continue
		}
		return nil
	}
	return nil
}

func (ms *MultiStore) RecentRetrievals(ctx context.Context, limit int) ([]RetrievalLog, error) {
	return ms.primary.Mongo.RecentRetrievals(ctx, limit)
}

func (ms *MultiStore) TopMemories(ctx context.Context, limit int) ([]Memory, error) {
	var all []Memory
	for _, e := range ms.entries {
		mems, err := e.Mongo.TopMemories(ctx, limit)
		if err != nil {
			continue
		}
		all = append(all, mems...)
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].HitCount > all[j].HitCount
	})
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// --- SourceStore delegation (primary only) --- //

func (ms *MultiStore) InsertSource(ctx context.Context, src Source) (string, error) {
	return ms.primary.Mongo.InsertSource(ctx, src)
}

func (ms *MultiStore) ListSources(ctx context.Context) ([]Source, error) {
	var all []Source
	for _, e := range ms.entries {
		sources, err := e.Mongo.ListSources(ctx)
		if err != nil {
			continue
		}
		all = append(all, sources...)
	}
	return all, nil
}

func (ms *MultiStore) DeleteSource(ctx context.Context, id string) error {
	for _, e := range ms.entries {
		if !e.IsWritable() {
			continue
		}
		if err := e.Mongo.DeleteSource(ctx, id); err != nil {
			continue
		}
		return nil
	}
	return fmt.Errorf("source %s not found in any writable database", id)
}

func (ms *MultiStore) UpdateSourceStatus(ctx context.Context, id string, status string, errStr string, pageCount int, memoryCount int) error {
	for _, e := range ms.entries {
		if err := e.Mongo.UpdateSourceStatus(ctx, id, status, errStr, pageCount, memoryCount); err != nil {
			continue
		}
		return nil
	}
	return nil
}

func (ms *MultiStore) GetSourcePage(ctx context.Context, sourceID primitive.ObjectID, url string) (*SourcePage, error) {
	for _, e := range ms.entries {
		page, err := e.Mongo.GetSourcePage(ctx, sourceID, url)
		if err != nil {
			continue
		}
		if page != nil {
			return page, nil
		}
	}
	return nil, nil
}

func (ms *MultiStore) UpsertSourcePage(ctx context.Context, page SourcePage) error {
	return ms.primary.Mongo.UpsertSourcePage(ctx, page)
}

func (ms *MultiStore) DeleteSourcePages(ctx context.Context, sourceID primitive.ObjectID) error {
	for _, e := range ms.entries {
		if !e.IsWritable() {
			continue
		}
		if err := e.Mongo.DeleteSourcePages(ctx, sourceID); err != nil {
			continue
		}
	}
	return nil
}

func (ms *MultiStore) DeleteMemoriesBySource(ctx context.Context, source string) error {
	for _, e := range ms.entries {
		if !e.IsWritable() {
			continue
		}
		if err := e.Mongo.DeleteMemoriesBySource(ctx, source); err != nil {
			log.Printf("[multistore] delete source memories error on %s: %v", e.Name, err)
		}
	}
	return nil
}

// --- Targeted operations --- //

// DatabaseInfo describes a database for the list endpoint.
type DatabaseInfo struct {
	Name     string `json:"name"`
	Database string `json:"database"`
	Role     string `json:"role"`
}

// DatabaseList returns info about all configured databases.
func (ms *MultiStore) DatabaseList() []DatabaseInfo {
	out := make([]DatabaseInfo, len(ms.entries))
	for i, e := range ms.entries {
		out[i] = DatabaseInfo{Name: e.Name, Database: e.Database, Role: e.Role}
	}
	return out
}

// SearchTargeted searches a single named database. Returns an error if the database is not found.
func (ms *MultiStore) SearchTargeted(ctx context.Context, dbName string, embedding []float32, topK int) ([]Memory, error) {
	e, ok := ms.byName[dbName]
	if !ok {
		return nil, fmt.Errorf("database %q not found", dbName)
	}
	var mems []Memory
	var err error
	if hs, ok := e.SearchStore.(HybridSearcher); ok {
		mems, err = hs.HybridSearch(ctx, embedding, topK, SearchOptions{})
	} else {
		mems, err = e.SearchStore.VectorSearch(ctx, embedding, topK)
	}
	if err != nil {
		return nil, err
	}
	for j := range mems {
		if mems[j].Metadata == nil {
			mems[j].Metadata = map[string]any{}
		}
		mems[j].Metadata["database"] = e.Name
	}
	return mems, nil
}

// InsertTargeted writes to a specific named database. Returns an error if not found or read-only.
func (ms *MultiStore) InsertTargeted(ctx context.Context, dbName string, mem Memory) error {
	e, ok := ms.byName[dbName]
	if !ok {
		return fmt.Errorf("database %q not found", dbName)
	}
	if !e.IsWritable() {
		return fmt.Errorf("database %q is read-only", dbName)
	}
	return e.Store.Insert(ctx, mem)
}

// ListTargeted lists memories from a specific database.
func (ms *MultiStore) ListTargeted(ctx context.Context, dbName string, query string, limit int) ([]Memory, error) {
	e, ok := ms.byName[dbName]
	if !ok {
		return nil, fmt.Errorf("database %q not found", dbName)
	}
	mems, err := e.Store.List(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	for j := range mems {
		if mems[j].Metadata == nil {
			mems[j].Metadata = map[string]any{}
		}
		mems[j].Metadata["database"] = e.Name
	}
	return mems, nil
}

// DeleteTargeted deletes from a specific database.
func (ms *MultiStore) DeleteTargeted(ctx context.Context, dbName string, id string) error {
	e, ok := ms.byName[dbName]
	if !ok {
		return fmt.Errorf("database %q not found", dbName)
	}
	if !e.IsWritable() {
		return fmt.Errorf("database %q is read-only", dbName)
	}
	return e.Store.Delete(ctx, id)
}
