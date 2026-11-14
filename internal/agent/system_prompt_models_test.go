package agent

import (
	"strings"
	"testing"
)

func TestModelFamilyClassification(t *testing.T) {
	cases := map[string]string{
		"gpt-5":                  familyOpenAI,
		"gpt-4o":                 familyOpenAI,
		"o3-mini":                familyOpenAI,
		"gemini-2.5-pro":         familyGemini,
		"claude-opus-4-6":        familyAnthropic,
		"anthropic/claude-haiku": familyAnthropic,
		"some-unknown-model":     "",
		"":                       "",
	}
	for model, want := range cases {
		if got := modelFamily(model); got != want {
			t.Errorf("modelFamily(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestBuildSystemPromptAppendsModelAddendum(t *testing.T) {
	// Assert on the addendum constants themselves (the core prompt shares phrases
	// like "one tool call per file", so substring checks can't distinguish them).
	if got := buildSystemPrompt(Options{Model: "gpt-5"}); !strings.Contains(got, openAIPromptAddendum) {
		t.Fatalf("expected the OpenAI addendum in the gpt-5 prompt")
	}
	if got := strings.Join(strings.Fields(strings.ToLower(buildSystemPrompt(Options{Model: "gpt-5"}))), " "); !strings.Contains(got, "never run apply_patch through exec_command") {
		t.Fatalf("OpenAI prompt must route edits through the native patch tool:\n%s", got)
	}
	// Claude is aligned with the core prompt and gets no family addendum now that
	// comment discipline is universal; it must not pick up another family's block.
	claude := buildSystemPrompt(Options{Model: "claude-opus-4-6"})
	if strings.Contains(claude, "<model_guidance>") {
		t.Fatalf("expected no model_guidance block for Claude (aligned with core prompt)")
	}
	if strings.Contains(claude, openAIPromptAddendum) {
		t.Fatalf("the claude prompt must not contain the OpenAI addendum")
	}
	// Unknown / unset model gets no family block.
	if got := modelPromptAddendum("", ""); got != "" {
		t.Fatalf("expected no addendum without a model, got %q", got)
	}
	if strings.Contains(buildSystemPrompt(Options{}), "<model_guidance>") {
		t.Fatalf("expected no <model_guidance> block without a model")
	}
}

func TestBuildSystemPromptIncludesActiveSessionRuntime(t *testing.T) {
	prompt := buildSystemPrompt(Options{
		ProviderName: "ollama-cloud",
		Model:        "glm-5.1",
	})

	for _, want := range []string{
		"<session>",
		"Active provider: ollama-cloud",
		"Active model: glm-5.1",
		"Persisted config commands may show saved defaults",
		"</session>",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("expected prompt to contain %q, got:\n%s", want, prompt)
		}
	}
}
