package modelregistry

import "testing"

func TestReasoningEffortsFallbackForGPT5AndOSeries(t *testing.T) {
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		want []ReasoningEffort
	}{
		{"gpt-5", []ReasoningEffort{ReasoningEffortMinimal, ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh}},
		{"gpt-5.5", []ReasoningEffort{ReasoningEffortMinimal, ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh}},
		{"gpt-5.4-mini", []ReasoningEffort{ReasoningEffortMinimal, ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh}},
		{"o3-mini", []ReasoningEffort{ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh}},
		{"gpt-5.3-codex-spark", []ReasoningEffort{ReasoningEffortMinimal, ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh}},
		{"tencent/hy3", []ReasoningEffort{ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh}},
		{"hy3", []ReasoningEffort{ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh}},
		{"hunyuan-t1", []ReasoningEffort{ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh}},
		{"openai/o3-mini", []ReasoningEffort{ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh}}, // github-style vendor/ id
		{"deepseek-v4-flash", []ReasoningEffort{ReasoningEffortLow, ReasoningEffortHigh, ReasoningEffortMax}},
		{"deepseek-v4-pro", []ReasoningEffort{ReasoningEffortLow, ReasoningEffortHigh, ReasoningEffortMax}},
		{"deepseek-reasoner", []ReasoningEffort{ReasoningEffortLow, ReasoningEffortHigh, ReasoningEffortMax}},
		{"deepseek-chat", []ReasoningEffort{ReasoningEffortLow, ReasoningEffortHigh, ReasoningEffortMax}},
		{"gpt-4.1", nil}, // non-reasoning, registered: stays empty
		{"gpt-4o-mini", nil},
		{"ollama/llama3.1", nil},
		{"nvidia/llama-3.1-nemotron-70b-instruct", nil}, // vendor/ id, non-reasoning: stays empty
	}
	for _, c := range cases {
		got := reg.ReasoningEfforts(c.name)
		if len(got) != len(c.want) {
			t.Fatalf("%s: got %v, want %v", c.name, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%s: got %v, want %v", c.name, got, c.want)
			}
		}
	}
}
