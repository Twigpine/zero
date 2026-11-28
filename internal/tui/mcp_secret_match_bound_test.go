package tui

import (
	"runtime"
	"strings"
	"testing"
)

// referenceLongestPrefixSuffix is the definition, written the slow obvious way:
// the longest prefix of pattern that is also a suffix of text.
func referenceLongestPrefixSuffix(pattern, text string) int {
	limit := len(pattern)
	if len(text) < limit {
		limit = len(text)
	}
	for size := limit; size > 0; size-- {
		if strings.HasSuffix(text, pattern[:size]) {
			return size
		}
	}
	return 0
}

// THE BOUND MUST NOT CHANGE ANY ANSWER.
//
// The work is now capped by the text rather than the candidate, on the argument
// that a prefix longer than the text cannot be a suffix of it. This checks that
// argument against the definition rather than trusting it.
func TestSecretMatchBoundPreservesEveryAnswer(t *testing.T) {
	patterns := []string{
		"", "a", "ab", "abc", "secret-token-value",
		strings.Repeat("ab", 40),
		"tok_" + strings.Repeat("z", 200),
		strings.Repeat("x", 5000),
	}
	texts := []string{
		"", "a", "ab", "xyz",
		"error: connecting with secret-token-value",
		"error: connecting with secret-token-va",
		"...trailing ab",
		"abababababab",
		strings.Repeat("ab", 30),
		strings.Repeat("x", 100),
		"prefix " + strings.Repeat("x", 4999),
	}
	for _, pattern := range patterns {
		for _, text := range texts {
			got := longestPrefixSuffix(pattern, text)
			want := referenceLongestPrefixSuffix(pattern, text)
			if got != want {
				t.Errorf("longestPrefixSuffix(len %d, len %d) = %d, want %d", len(pattern), len(text), got, want)
			}
		}
	}
}

func allocatedMiB(fn func()) float64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return float64(after.TotalAlloc-before.TotalAlloc) / (1 << 20)
}

// AND THE COST MUST NOT BE SET BY THE CONFIGURED VALUE.
//
// dropTrailingSecretPrefix runs on every /mcp state rebuild, once per configured
// or stored credential. longestPrefixSuffix built pattern+sentinel+text and an
// []int over the whole thing, so the candidate's length drove allocation:
// headers, env values, URL components, OAuth fields and token-store values have
// no size limit, and three 2 MiB candidates measured 54.5 MiB and 33ms against a
// render budget that is nominally fixed.
func TestSecretMatchWorkDoesNotScaleWithTheConfiguredSecret(t *testing.T) {
	rendered := strings.Repeat("x", 16<<10)
	// Built OUTSIDE the measured closure, or strings.Repeat itself dominates the
	// measurement and hides what the function does.
	smallSecret := []string{strings.Repeat("a", 64<<10)}
	largeSecret := []string{strings.Repeat("a", 4<<20)}
	small := allocatedMiB(func() {
		_ = dropTrailingSecretPrefix(rendered, smallSecret)
	})
	large := allocatedMiB(func() {
		_ = dropTrailingSecretPrefix(rendered, largeSecret)
	})
	// A 64x longer candidate must not cost meaningfully more. Generous, because
	// this is a scaling assertion and not a fixed-size one.
	if large > small+1 {
		t.Errorf("a 4 MiB candidate allocated %.1f MiB against %.1f MiB for a 64 KiB one; the configured value is still sizing the work", large, small)
	}
}

// The suppression itself still works, or the bound above is satisfied by never
// matching anything.
func TestTrailingSecretPrefixIsStillDropped(t *testing.T) {
	const secret = "sk-live-abcdefghijklmnopqrstuvwxyz"
	rendered := "dial failed for " + secret[:20]
	got := dropTrailingSecretPrefix(rendered, []string{secret})
	if strings.Contains(got, secret[:20]) {
		t.Errorf("the partial credential survived: %q", got)
	}
	if !strings.HasPrefix(got, "dial failed for") {
		t.Errorf("the message body was lost: %q", got)
	}
}
