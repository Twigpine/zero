package providercatalog

import "testing"

// EVERY FIRST-PARTY SINGLE-FAMILY PROVIDER DECLARES ITS FAMILY, and every
// gateway declares none.
//
// This is the fact prompt tuning reads. Before it existed, tuning guessed from
// the model id: of this catalog's providers, only a handful had a default model
// matching any prefix arm, so the rest silently received no family guidance —
// including the recommended default, and including OpenAI's own OAuth entry,
// which worked only because "gpt-5.5" begins with "gpt".
func TestFirstPartyProvidersDeclareTheirFamily(t *testing.T) {
	for id, want := range map[string]string{
		"openai":        FamilyOpenAI,
		"chatgpt":       FamilyOpenAI, // OAuth, subscription endpoint
		"chatgpt-proxy": FamilyOpenAI,
		"anthropic":     FamilyAnthropic,
		"google":        FamilyGemini,
	} {
		if got := ModelFamilyFor(id); got != want {
			t.Errorf("%s declares family %q, want %q", id, got, want)
		}
	}
}

// A GATEWAY MUST DECLARE NOTHING. Ollama Cloud speaks openai-compatible and
// serves Qwen; calling that "openai" would hand GPT-shaped guidance to a model
// that never asked for it, which is worse than the silence it replaces.
func TestGatewaysAndMultiFamilyProvidersDeclareNothing(t *testing.T) {
	for _, id := range []string{
		"ollama-cloud", "openrouter", "huggingface", "groq", "together",
		"fireworks", "gitlawb-opengateway", "xai", "deepseek", "zai",
		"bedrock", "vertex", "custom-openai-compatible",
	} {
		if got := ModelFamilyFor(id); got != "" {
			t.Errorf("%s declares family %q; it serves more than one, so the model id must decide", id, got)
		}
	}
}

// An unknown provider is not an error and not a guess.
func TestAnUnknownProviderDeclaresNothing(t *testing.T) {
	if got := ModelFamilyFor("no-such-provider"); got != "" {
		t.Errorf("an unknown provider returned %q", got)
	}
	if got := ModelFamilyFor(""); got != "" {
		t.Errorf("an empty catalog id returned %q", got)
	}
}

// A DECLARED FAMILY MUST BE ONE THE PROMPT LAYER KNOWS. A typo here would be
// invisible: it reads as "unknown" and silently degrades to id-guessing, which
// is exactly the state this field was added to fix.
func TestEveryDeclaredFamilyIsAKnownOne(t *testing.T) {
	known := map[string]bool{FamilyOpenAI: true, FamilyAnthropic: true, FamilyGemini: true}
	declared := 0
	for _, descriptor := range All() {
		if descriptor.ModelFamily == "" {
			continue
		}
		declared++
		if !known[descriptor.ModelFamily] {
			t.Errorf("%s declares unknown family %q", descriptor.ID, descriptor.ModelFamily)
		}
	}
	if declared == 0 {
		t.Fatal("no provider declares a family, so the whole mechanism is inert")
	}
}
