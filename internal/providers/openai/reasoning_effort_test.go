package openai

import "testing"

func TestOpenAIReasoningEffortPreservesKnownProviderTiers(t *testing.T) {
	t.Parallel()
	for _, effort := range []string{"minimal", "low", "medium", "high", "xhigh", "max", "ultra"} {
		if got := openAIReasoningEffort(effort); got != effort {
			t.Fatalf("openAIReasoningEffort(%q) = %q", effort, got)
		}
	}
	if got := openAIReasoningEffort("unknown"); got != "" {
		t.Fatalf("unknown effort = %q, want omitted", got)
	}
}

func TestOpenAIServiceTierNormalizesKnownValues(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		input string
		want  string
	}{
		{input: "priority", want: "priority"},
		{input: "flex", want: "flex"},
		{input: " PRIORITY ", want: "priority"},
		{input: "FlEx", want: "flex"},
		{input: "", want: ""},
		{input: "standard", want: ""},
	} {
		t.Run(test.input, func(t *testing.T) {
			if got := openAIServiceTier(test.input); got != test.want {
				t.Fatalf("openAIServiceTier(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}
