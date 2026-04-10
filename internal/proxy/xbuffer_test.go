package proxy

import (
	"testing"
)

func TestExchangeBuffer_NilSafe(t *testing.T) {
	var b *exchangeBuffer
	b.Add("q", "a", nil, false) // should not panic
	if b.Len() != 0 {
		t.Error("nil buffer Len should be 0")
	}
	if got := b.TopicalContext([]float32{1, 0}, 0.5); got != "" {
		t.Errorf("nil buffer TopicalContext should be empty, got: %q", got)
	}
}

func TestExchangeBuffer_Add_And_Len(t *testing.T) {
	b := newExchangeBuffer(3)
	b.Add("q1", "a1", nil, false)
	b.Add("q2", "a2", nil, true)
	if b.Len() != 2 {
		t.Errorf("expected 2, got %d", b.Len())
	}

	// Fill past capacity — oldest should be evicted.
	b.Add("q3", "a3", nil, false)
	b.Add("q4", "a4", nil, false)
	if b.Len() != 3 {
		t.Errorf("expected 3 (capped), got %d", b.Len())
	}

	// Verify oldest was evicted: first entry should be q2.
	b.mu.Lock()
	if b.entries[0].userMsg != "q2" {
		t.Errorf("expected q2 as oldest after eviction, got %q", b.entries[0].userMsg)
	}
	b.mu.Unlock()
}

func TestExchangeBuffer_TopicalContext_NoEmbeddings(t *testing.T) {
	b := newExchangeBuffer(10)
	b.Add("q1", "a1", nil, false) // no vec
	b.Add("q2", "a2", nil, false) // no vec

	got := b.TopicalContext([]float32{1, 0, 0}, 0.5)
	if got != "" {
		t.Errorf("expected empty context when entries have no embeddings, got: %q", got)
	}
}

func TestExchangeBuffer_TopicalContext_SkipsPassedEntries(t *testing.T) {
	b := newExchangeBuffer(10)
	vec := []float32{1, 0, 0}
	b.Add("q1", "a1", vec, true) // passed — should be skipped

	got := b.TopicalContext(vec, 0.5)
	if got != "" {
		t.Errorf("should skip passed entries, got: %q", got)
	}
}

func TestExchangeBuffer_TopicalContext_FindsSimilar(t *testing.T) {
	b := newExchangeBuffer(10)

	similar := []float32{1, 0, 0}
	different := []float32{0, 1, 0}
	query := []float32{0.9, 0.1, 0}

	b.Add("related question", "a1", similar, false)     // sim ~0.99 with query
	b.Add("unrelated question", "a2", different, false) // sim ~0.11 with query

	got := b.TopicalContext(query, 0.5)
	if got == "" {
		t.Fatal("expected topical context, got empty")
	}
	if got != "Q: related question" {
		t.Errorf("expected only the similar entry, got: %q", got)
	}
}

func TestExchangeBuffer_TopicalContext_NilVecQuery(t *testing.T) {
	b := newExchangeBuffer(10)
	b.Add("q", "a", []float32{1, 0, 0}, false)

	got := b.TopicalContext(nil, 0.5)
	if got != "" {
		t.Errorf("nil query vec should return empty, got: %q", got)
	}
}

func TestExchangeBuffer_TopicalContext_EmptyUserMsg(t *testing.T) {
	b := newExchangeBuffer(10)
	vec := []float32{1, 0, 0}
	b.Add("", "assistant text here", vec, false)

	got := b.TopicalContext(vec, 0.5)
	if got != "assistant text here" {
		t.Errorf("expected assistant text for empty userMsg, got: %q", got)
	}
}

func TestExchangeBuffer_TopicalContext_RespectsMaxChars(t *testing.T) {
	b := newExchangeBuffer(100)
	vec := []float32{1, 0, 0}

	// Add many entries with the same vector to fill context.
	longQ := string(make([]byte, 400))
	for i := 0; i < 20; i++ {
		b.Add(longQ, "a", vec, false)
	}

	got := b.TopicalContext(vec, 0.5)
	if len(got) > maxContextChars+maxEntryChars+10 { // allow for trailing truncation
		t.Errorf("context should be capped near %d chars, got %d", maxContextChars, len(got))
	}
}

func TestTruncateStr(t *testing.T) {
	if got := truncateStr("hello", 10); got != "hello" {
		t.Errorf("short string should not be truncated, got: %q", got)
	}
	if got := truncateStr("hello world", 5); got != "hello..." {
		t.Errorf("expected truncation with ..., got: %q", got)
	}
}
