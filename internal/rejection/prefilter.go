package rejection

import "strings"

// proceduralAsstPrefixes are assistant response openings that almost always
// signal procedural narration — "I've done X", "I'll do Y" — with no durable
// technical knowledge. Seeded from corpus eval analysis; refine by running
// `memoryd analyze-rejections` against an accumulated rejection log.
var proceduralAsstPrefixes = []string{
	"i'll ",
	"i will ",
	"i've ",
	"i have ",
	"i've made",
	"i've updated",
	"i've added",
	"i've removed",
	"i've changed",
	"i've fixed",
	"i've created",
	"i've modified",
	"i've implemented",
	"i've written",
	"i've completed",
	"i've finished",
	"i've done",
	"i've set",
	"i've moved",
	"i've deleted",
	"i looked at",
	"i made ",
	"i updated ",
	"i changed ",
	"i added ",
	"i removed ",
	"i fixed ",
	"i created ",
	"i modified ",
	"i implemented ",
	"i wrote ",
	"i ran ",
	"let me look",
	"let me check",
	"let me read",
	"let me see",
	"sure! ",
	"sure, i",
	"sure, let",
	"of course",
	"done! ",
	"done.",
	"absolutely",
}

// shortUserAcks are user messages that are pure workflow acknowledgments with
// no technical content. When paired with a procedural assistant prefix, the
// exchange is guaranteed noise.
var shortUserAcks = map[string]bool{
	"ok":           true,
	"okay":         true,
	"ok.":          true,
	"okay.":        true,
	"ok!":          true,
	"thanks":       true,
	"thank you":    true,
	"thanks!":      true,
	"thank you!":   true,
	"sounds good":  true,
	"sounds good!": true,
	"great":        true,
	"great!":       true,
	"great.":       true,
	"perfect":      true,
	"perfect!":     true,
	"perfect.":     true,
	"go ahead":     true,
	"go ahead.":    true,
	"yes":          true,
	"yes.":         true,
	"yes!":         true,
	"yep":          true,
	"yep.":         true,
	"sure":         true,
	"sure.":        true,
	"please":       true,
	"please do":    true,
	"continue":     true,
	"proceed":      true,
	"got it":       true,
	"got it.":      true,
	"good":         true,
	"good.":        true,
	"👍":            true,
	"lgtm":         true,
}

// QuickFilter returns true when the exchange is almost certainly procedural
// narration with no durable technical value, without invoking the LLM.
//
// Conservative by design — only fires when BOTH:
//  1. The user message is a known short acknowledgment.
//  2. The assistant response starts with a known procedural prefix.
//
// The LLM quality gate (SynthesizeQA) handles everything else.
// Use the rejection log to identify new patterns to add here.
func QuickFilter(userMsg, asstMsg string) bool {
	userNorm := strings.ToLower(strings.TrimSpace(userMsg))
	if !shortUserAcks[userNorm] {
		return false
	}
	asstLower := strings.ToLower(strings.TrimSpace(asstMsg))
	return hasProceduralPrefix(asstLower)
}

func hasProceduralPrefix(s string) bool {
	for _, p := range proceduralAsstPrefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// discoverySignals are phrases that indicate the agent just made a non-obvious
// finding — the "aha moment" pattern. When detected anywhere in the assistant
// response, the exchange should bypass all pre-filters and go straight to
// synthesis, because these are exactly the insights worth capturing.
//
// Patterns are matched case-insensitively against the first 500 chars of the
// assistant response to keep the scan cheap.
var discoverySignals = []string{
	"interesting!",
	"interesting —",
	"interesting -",
	"interestingly,",
	"that's interesting",
	"this is interesting",
	"aha!",
	"aha,",
	"aha —",
	"oh!",
	"oh interesting",
	"oh wait",
	"oh, that",
	"ah,",
	"ah!",
	"ah —",
	"found it!",
	"found the",
	"here's the problem",
	"here's the issue",
	"here's what's happening",
	"the root cause",
	"root cause is",
	"root cause:",
	"the real issue",
	"the actual problem",
	"the actual issue",
	"turns out",
	"it turns out",
	"this explains why",
	"that explains why",
	"that's why",
	"this is why",
	"the key insight",
	"key finding",
	"surprisingly,",
	"this is surprising",
	"unexpectedly,",
	"counterintuitively",
	"the trick is",
	"the gotcha is",
	"the catch is",
	"important discovery",
	"critical discovery",
	"i didn't expect",
	"we didn't expect",
	"wasn't obvious",
	"non-obvious",
	"not obvious",
	"the subtle",
	"a subtle",
	"wait —",
	"wait,",
	"actually,",
	"actually!",
	"actually —",
}

// discoverySignalScanLen is how far into the assistant text to scan for
// discovery signals. Scanning the full response would be wasteful; the
// exclamation of surprise is almost always in the first paragraph.
const discoverySignalScanLen = 500

// DiscoverySignal returns true when the assistant response contains language
// indicating a non-obvious discovery or insight — an "aha moment". These
// exchanges should bypass pre-filters and go straight to LLM synthesis.
func DiscoverySignal(asstMsg string) bool {
	text := strings.ToLower(strings.TrimSpace(asstMsg))
	if len(text) > discoverySignalScanLen {
		text = text[:discoverySignalScanLen]
	}
	for _, sig := range discoverySignals {
		if strings.Contains(text, sig) {
			return true
		}
	}
	return false
}
