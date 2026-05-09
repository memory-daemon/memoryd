package pipeline

import (
	"strings"
	"testing"

	"github.com/memory-daemon/memoryd/internal/store"
)

func TestFormatContext_Empty(t *testing.T) {
	result := FormatContext(nil, 2048)
	if result != "" {
		t.Errorf("expected empty string for nil memories, got %q", result)
	}

	result = FormatContext([]store.Memory{}, 2048)
	if result != "" {
		t.Errorf("expected empty string for empty memories, got %q", result)
	}
}

func TestFormatContext_SingleMemory(t *testing.T) {
	memories := []store.Memory{
		{Content: "Go is a compiled language.", Source: "claude-code", Score: 0.95},
	}

	result := FormatContext(memories, 2048)

	if !strings.Contains(result, "<memory>") {
		t.Error("expected opening memory tag")
	}
	if !strings.Contains(result, "</memory>") {
		t.Error("expected closing memory tag")
	}
	if !strings.Contains(result, "Go is a compiled language.") {
		t.Error("expected memory content")
	}
}

func TestFormatContext_MultipleMemories(t *testing.T) {
	memories := []store.Memory{
		{Content: "Memory one.", Source: "test", Score: 0.9},
		{Content: "Memory two.", Source: "test", Score: 0.8},
		{Content: "Memory three.", Source: "test", Score: 0.7},
	}

	result := FormatContext(memories, 2048)

	if !strings.Contains(result, "1.") || !strings.Contains(result, "2.") || !strings.Contains(result, "3.") {
		t.Error("expected numbered entries for all memories")
	}
}

func TestFormatContext_TokenBudget(t *testing.T) {
	memories := []store.Memory{
		{Content: strings.Repeat("A", 100), Source: "test", Score: 0.9},
		{Content: strings.Repeat("B", 100), Source: "test", Score: 0.8},
	}

	result := FormatContext(memories, 10)

	if len(result) == 0 {
		return
	}
	if !strings.Contains(result, "</memory>") {
		t.Error("expected closing tag even when truncated")
	}
}

func TestInjectSystemPrompt_BothPresent(t *testing.T) {
	existing := "You are a helpful assistant."
	ctx := "<retrieved_context>some context</retrieved_context>"

	result := InjectSystemPrompt(existing, ctx)

	if !strings.HasPrefix(result, ctx) {
		t.Error("expected context to be prepended")
	}
	if !strings.HasSuffix(result, existing) {
		t.Error("expected existing prompt to be appended")
	}
	if !strings.Contains(result, "\n\n") {
		t.Error("expected separator between context and existing prompt")
	}
}

func TestInjectSystemPrompt_EmptyContext(t *testing.T) {
	existing := "You are helpful."
	result := InjectSystemPrompt(existing, "")
	if result != existing {
		t.Errorf("expected %q unchanged, got %q", existing, result)
	}
}

func TestInjectSystemPrompt_EmptyExisting(t *testing.T) {
	ctx := "some context"
	result := InjectSystemPrompt("", ctx)
	if result != ctx {
		t.Errorf("expected %q, got %q", ctx, result)
	}
}

func TestInjectSystemPrompt_BothEmpty(t *testing.T) {
	result := InjectSystemPrompt("", "")
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}
