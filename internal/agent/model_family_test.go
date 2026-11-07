package agent

import (
	"strings"
	"testing"
)

// THE PROVIDER OUTRANKS THE MODEL ID, because the provider knows and the id only
// hints.
//
// A ChatGPT OAuth session was classified correctly for one reason: "gpt-5.5"
// happens to begin with "gpt". Nothing obliges the next id to — OpenAI has
// already shipped o1, o3 and o4 — and the prefix list has to be amended each
// time to keep a first-party provider working. Asking the provider does not.
func TestTheProviderFamilyWinsOverTheModelId(t *testing.T) {
	// An id no prefix arm matches, from a provider that declares OpenAI.
	if got := modelPromptAddendum("openai", "some-unreleased-id"); got != openAIPromptAddendum {
		t.Error("a provider that declares openai did not get the openai addendum for an unrecognised id")
	}
	if got := modelPromptAddendum("gemini", "some-unreleased-id"); got != geminiPromptAddendum {
		t.Error("a provider that declares gemini did not get the gemini addendum")
	}
	// Anthropic is aligned with the core prompt and takes no addendum, declared
	// or guessed. Asserted so "no addendum" stays a decision, not an accident.
	if got := modelPromptAddendum("anthropic", "some-unreleased-id"); got != "" {
		t.Errorf("anthropic gained an addendum: %q", got)
	}
}

// A GATEWAY DECLARES NOTHING, and must fall back to the id — it is the only
// thing that distinguishes openai/gpt-4.1 from z-ai/glm-4.6 on one endpoint.
func TestAGatewayFallsBackToTheModelId(t *testing.T) {
	for _, testCase := range []struct {
		model string
		want  string
	}{
		{"openai/gpt-4.1", openAIPromptAddendum},
		{"google/gemini-2.5-pro", geminiPromptAddendum},
		{"anthropic/claude-sonnet-4.5", ""},
		// Unclassified, and honestly so: no family addendum exists for it.
		{"z-ai/glm-4.6", ""},
		{"qwen3-coder:480b", ""},
	} {
		if got := modelPromptAddendum("", testCase.model); got != testCase.want {
			t.Errorf("%s: addendum = %q, want %q", testCase.model, truncateForTest(got), truncateForTest(testCase.want))
		}
	}
}

// The wiring is not decoration: a family that reaches Options must reach the
// prompt. Asserted through the real assembly rather than the helper, because
// asserting the helper is how a value gets threaded to a layer that drops it.
func TestTheDeclaredFamilyReachesTheAssembledPrompt(t *testing.T) {
	withProvider := BuildSystemPromptPreview(Options{ModelFamily: "openai", Model: "some-unreleased-id"})
	if !strings.Contains(withProvider, "Persist until the task is fully handled this turn") {
		t.Error("the openai guidance did not reach the assembled prompt when the provider declared it")
	}
	withoutProvider := BuildSystemPromptPreview(Options{Model: "some-unreleased-id"})
	if strings.Contains(withoutProvider, "Persist until the task is fully handled this turn") {
		t.Error("an unclassified model gained openai guidance from nowhere")
	}
}

func truncateForTest(text string) string {
	if len(text) > 40 {
		return text[:40] + "..."
	}
	return text
}
