package proxy

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/memory-daemon/memoryd/internal/pipeline"
)

const (
	// defaultXBufSize is the number of recent exchanges to retain.
	defaultXBufSize = 50

	// topicalScanWindow limits how far back to scan for topical context.
	topicalScanWindow = 20

	// maxContextChars caps the total context text passed to the synthesizer.
	maxContextChars = 1500

	// maxEntryChars caps a single entry's contribution to the context.
	maxEntryChars = 300
)

// exchangeEntry is one captured proxy exchange with optional embedding.
type exchangeEntry struct {
	userMsg   string
	asstText  string
	vec       []float32 // nil when embedder unavailable
	passed    bool
	timestamp time.Time
}

// exchangeBuffer is a bounded ring buffer of recent proxy exchanges.
// It enables similarity-based topical context lookup: when an exchange
// passes the quality gate, the buffer is scanned for topically-similar
// rejected exchanges to provide anchoring context to the synthesizer.
//
// All methods are nil-safe and goroutine-safe.
type exchangeBuffer struct {
	mu      sync.Mutex
	entries []exchangeEntry
	maxSize int
}

func newExchangeBuffer(maxSize int) *exchangeBuffer {
	if maxSize <= 0 {
		maxSize = defaultXBufSize
	}
	return &exchangeBuffer{maxSize: maxSize}
}

// Add records an exchange. When the buffer is full the oldest entry is evicted.
func (b *exchangeBuffer) Add(userMsg, asstText string, vec []float32, passed bool) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.entries) >= b.maxSize {
		copy(b.entries, b.entries[1:])
		b.entries = b.entries[:len(b.entries)-1]
	}
	b.entries = append(b.entries, exchangeEntry{
		userMsg:   userMsg,
		asstText:  asstText,
		vec:       vec,
		passed:    passed,
		timestamp: time.Now(),
	})
}

// TopicalContext finds recently-rejected exchanges that are semantically
// similar to vec (cosine similarity >= threshold) and formats them as
// context text for the synthesizer. Returns "" when no relevant context
// is found or when embeddings are unavailable.
func (b *exchangeBuffer) TopicalContext(vec []float32, threshold float64) string {
	if b == nil || vec == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	n := len(b.entries)
	if n == 0 {
		return ""
	}

	start := n - topicalScanWindow
	if start < 0 {
		start = 0
	}

	var parts []string
	totalChars := 0
	for i := start; i < n; i++ {
		e := b.entries[i]
		if e.passed || e.vec == nil {
			continue
		}
		if pipeline.CosineSim(e.vec, vec) < threshold {
			continue
		}

		part := formatContextEntry(e)
		if totalChars+len(part) > maxContextChars {
			break
		}
		parts = append(parts, part)
		totalChars += len(part)
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n")
}

// Len returns the number of entries in the buffer. Useful for testing.
func (b *exchangeBuffer) Len() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.entries)
}

func formatContextEntry(e exchangeEntry) string {
	if e.userMsg != "" {
		return fmt.Sprintf("Q: %s", truncateStr(e.userMsg, maxEntryChars))
	}
	return truncateStr(e.asstText, maxEntryChars)
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
