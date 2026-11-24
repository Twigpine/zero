package zeroruntime

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestSanitizeResolvedModelPassesThroughCleanIDs(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"deepseek/deepseek-r1:free",
		"accounts/fireworks/models/llama-v3p1-405b-instruct",
		"arn:aws:bedrock:us-east-1:123456789012:inference-profile/us.anthropic.claude-3-5-sonnet-20241022-v2:0",
		"gpt-5-2025-08-07",
	} {
		if got := SanitizeResolvedModel(in); got != in {
			t.Errorf("SanitizeResolvedModel(%q) = %q, want identity", in, got)
		}
	}
}

func TestSanitizeResolvedModelStripsControlCharacters(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{in: "\x1b[31mevil\x1b[0m", want: "[31mevil[0m"},
		{in: "\x1b]8;;https://x\x07link\x1b]8;;\x07", want: "]8;;https://xlink]8;;"},
		{in: "gpt\x00-4", want: "gpt-4"},
		{in: "a\x7fb", want: "ab"},
		{in: "a\u0085b\u009bc", want: "abc"},
		{in: "a\r\nb\tc", want: "abc"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			if got := SanitizeResolvedModel(tc.in); got != tc.want {
				t.Fatalf("SanitizeResolvedModel(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeResolvedModelStripsFormatCharacters(t *testing.T) {
	t.Parallel()
	inputs := []string{
		"\u202egpt-4",
		"gpt\u200b-4",
		"\ufeffgpt-4",
		"gpt\u00ad-4",
		"gpt\u2066-4\u2069",
		"gpt-4\u2028",
		"gpt\u00a0-4",
	}
	for _, in := range inputs {
		in := in
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			if got := SanitizeResolvedModel(in); got != "gpt-4" {
				t.Fatalf("SanitizeResolvedModel(%q) = %q, want gpt-4", in, got)
			}
		})
	}
}

func TestSanitizeResolvedModelRepairsInvalidUTF8(t *testing.T) {
	t.Parallel()
	got := SanitizeResolvedModel("a\xffb\xfe")
	if got != "ab" {
		t.Fatalf("SanitizeResolvedModel = %q, want ab", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("result is not valid UTF-8: %q", got)
	}
}

func TestSanitizeResolvedModelCapsLength(t *testing.T) {
	t.Parallel()
	t.Run("cjk-3-byte", func(t *testing.T) {
		t.Parallel()
		in := strings.Repeat("\u6a21", 1<<20)
		got := SanitizeResolvedModel(in)
		if len(got) > MaxResolvedModelBytes {
			t.Fatalf("len = %d, want at most %d", len(got), MaxResolvedModelBytes)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("invalid UTF-8")
		}
		if len(got) != 255 {
			t.Fatalf("len = %d, want 255 (largest rune-aligned prefix under cap)", len(got))
		}
		if !strings.HasPrefix(in, got) {
			t.Fatalf("result is not a prefix of the cleaned input")
		}
	})
	t.Run("emoji-4-byte", func(t *testing.T) {
		t.Parallel()
		in := strings.Repeat("\U0001f600", 1<<20)
		got := SanitizeResolvedModel(in)
		if len(got) > MaxResolvedModelBytes {
			t.Fatalf("len = %d, want at most %d", len(got), MaxResolvedModelBytes)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("invalid UTF-8")
		}
		if len(got) != 256 {
			t.Fatalf("len = %d, want 256", len(got))
		}
		if !strings.HasPrefix(in, got) {
			t.Fatalf("result is not a prefix of the cleaned input")
		}
	})
	t.Run("ascii-300", func(t *testing.T) {
		t.Parallel()
		in := strings.Repeat("a", 300)
		got := SanitizeResolvedModel(in)
		if len(got) != 256 {
			t.Fatalf("len = %d, want 256", len(got))
		}
		if got != in[:256] {
			t.Fatalf("result = %q, want first 256 bytes", got)
		}
	})
}

func TestSanitizeResolvedModelTrimsAndCapsWithoutTrailingSpace(t *testing.T) {
	t.Parallel()
	if got := SanitizeResolvedModel("\x00 gpt-x \x00"); got != "gpt-x" {
		t.Fatalf("trimmed control/spaces = %q, want gpt-x", got)
	}
	prefix := strings.Repeat("a", 255)
	got := SanitizeResolvedModel(prefix + " z")
	if len(got) != 255 {
		t.Fatalf("len = %d, want 255", len(got))
	}
	if strings.HasSuffix(got, " ") {
		t.Fatalf("result has trailing space: %q", got)
	}
	if got != prefix {
		t.Fatalf("result = %q, want %q", got, prefix)
	}
}

func fuzzSanitizeSeeds() []string {
	var seeds []string
	for _, tc := range []struct{ in string }{
		{"\x1b[31mevil\x1b[0m"},
		{"\x1b]8;;https://x\x07link\x1b]8;;\x07"},
		{"gpt\x00-4"},
		{"a\x7fb"},
		{"a\u0085b\u009bc"},
		{"a\r\nb\tc"},
	} {
		seeds = append(seeds, tc.in)
	}
	seeds = append(seeds,
		"\u202egpt-4", "gpt\u200b-4", "\ufeffgpt-4", "gpt\u00ad-4",
		"gpt\u2066-4\u2069", "gpt-4\u2028", "gpt\u00a0-4",
	)
	seeds = append(seeds, "a\xffb\xfe")
	seeds = append(seeds, strings.Repeat("\u6a21", 1000))
	seeds = append(seeds, strings.Repeat("\U0001f600", 1000))
	seeds = append(seeds, strings.Repeat("a", 300))
	seeds = append(seeds, "\x00 gpt-x \x00")
	seeds = append(seeds, strings.Repeat("a", 255)+" z")
	return seeds
}

func FuzzSanitizeResolvedModel(f *testing.F) {
	for _, seed := range fuzzSanitizeSeeds() {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		got := SanitizeResolvedModel(raw)
		if !utf8.ValidString(got) {
			t.Fatalf("invalid UTF-8: %q", got)
		}
		if len(got) > MaxResolvedModelBytes {
			t.Fatalf("len %d exceeds cap", len(got))
		}
		for _, r := range got {
			if !unicode.IsPrint(r) {
				t.Fatalf("non-printable rune U+%04X in %q", r, got)
			}
		}
		if got != "" && (strings.HasPrefix(got, " ") || strings.HasSuffix(got, " ")) {
			t.Fatalf("leading/trailing space in %q", got)
		}
		again := SanitizeResolvedModel(got)
		if again != got {
			t.Fatalf("not idempotent: first %q, second %q", got, again)
		}
	})
}

func TestResolvedModelAttribution(t *testing.T) {
	t.Parallel()
	requested300 := strings.Repeat("m", 300)
	cases := []struct {
		name      string
		requested string
		reported  string
		want      string
	}{
		{"equal", "gpt-test", "gpt-test", ""},
		{"differs", "router/auto", "deepseek/deepseek-r1:free", "deepseek/deepseek-r1:free"},
		{"split-nul", "gpt-4", "gpt-\x004", ""},
		{"split-bidi", "gpt-4", "gpt-\u202e4", ""},
		{"empty-reported", "gpt-4", "", ""},
		{"only-removable", "gpt-4", "\x1b\x00\u200b", ""},
		{"trimmed-equal", "gpt-4", " gpt-4 ", ""},
		{"case-sensitive", "GPT-4", "gpt-4", "gpt-4"},
		{"long-equal", requested300, requested300, ""},
		{"escape-other", "gpt-4", "\x1b[31mother", "[31mother"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ResolvedModelAttribution(tc.requested, tc.reported); got != tc.want {
				t.Fatalf("ResolvedModelAttribution(%q, %q) = %q, want %q", tc.requested, tc.reported, got, tc.want)
			}
		})
	}
}

// The sanitizer must stop at the cap instead of materializing the whole
// printable value: a megabyte-long model id would otherwise be copied in full
// only to keep 256 bytes.
func TestSanitizeResolvedModelDoesNotAllocateTheDiscardedSuffix(t *testing.T) {
	huge := strings.Repeat("m", 1<<20)
	allocs := testing.AllocsPerRun(5, func() { _ = SanitizeResolvedModel(huge) })
	if allocs > 4 {
		t.Fatalf("allocs per run = %v, want a small constant", allocs)
	}
	if got := SanitizeResolvedModel(huge); len(got) != MaxResolvedModelBytes {
		t.Fatalf("len = %d, want %d", len(got), MaxResolvedModelBytes)
	}
}

// A later snapshot that reports the requested model itself ends the router
// attribution: its ResolvedModel is empty because the provider reported a model
// that matched, not because it reported none. A snapshot with no model update
// keeps the earlier attribution.
func TestMergeUsageSnapshotEchoOfRequestedModelClearsAttribution(t *testing.T) {
	t.Parallel()
	left := Usage{PromptTokens: 10, ResolvedModel: "deepseek/x", ModelReported: true}
	t.Run("echo-clears", func(t *testing.T) {
		t.Parallel()
		merged := mergeUsageSnapshot(left, Usage{CompletionTokens: 5, ModelReported: true})
		if merged.ResolvedModel != "" {
			t.Fatalf("ResolvedModel = %q, want cleared by the echo of the requested model", merged.ResolvedModel)
		}
		if merged.PromptTokens != 10 && merged.EffectiveInputTokens() != 10 || merged.EffectiveOutputTokens() != 5 {
			t.Fatalf("token counts lost: %+v", merged)
		}
	})
	t.Run("no-model-update-keeps", func(t *testing.T) {
		t.Parallel()
		merged := mergeUsageSnapshot(left, Usage{CompletionTokens: 5})
		if merged.ResolvedModel != "deepseek/x" {
			t.Fatalf("ResolvedModel = %q, want deepseek/x kept", merged.ResolvedModel)
		}
	})
}
