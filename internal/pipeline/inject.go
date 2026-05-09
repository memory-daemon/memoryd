package pipeline

import (
	"fmt"
	"strings"

	"github.com/memory-daemon/memoryd/internal/store"
)

const (
	contextHeader = "<memory>\n"
	contextFooter = "\n</memory>"
)

// FormatContext renders retrieved memories into a block suitable for system prompt injection.
// Each memory is a numbered fact on its own line — no score or source metadata,
// keeping the injected block dense and immediately usable by the agent.
func FormatContext(memories []store.Memory, maxTokens int) string {
	if len(memories) == 0 {
		return ""
	}

	maxChars := maxTokens * 4

	var b strings.Builder
	b.WriteString(contextHeader)

	for i, m := range memories {
		entry := fmt.Sprintf("%d. %s\n", i+1, m.Content)
		if b.Len()+len(entry)+len(contextFooter) > maxChars {
			break
		}
		b.WriteString(entry)
	}

	b.WriteString(contextFooter)
	return b.String()
}

// InjectSystemPrompt prepends the retrieved context block to an existing system prompt.
func InjectSystemPrompt(existing, context string) string {
	if context == "" {
		return existing
	}
	if existing == "" {
		return context
	}
	return context + "\n\n" + existing
}
