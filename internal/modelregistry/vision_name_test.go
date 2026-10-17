package modelregistry

import "testing"

func TestVisionCapableByName(t *testing.T) {
	vision := []string{
		"gemini-3-flash-preview", "gemini-2.5-flash", "ollama-cloud/gemini-3-flash-preview",
		"grok-4.3", "grok-4-fast", "grok-2-vision",
		"gpt-4o", "gpt-4.1-mini", "gpt-4-turbo", "gpt-5", "o3-mini", "o1",
		"claude-sonnet-4.5", "claude-3-haiku",
		"MiniMax-M3",
		"llava:13b", "qwen2.5-vl-7b", "llama3.2-vision", "pixtral-12b", "moondream",
		"google/gemma-3-27b-it", "gemma3:27b", "gemma3:4b", "gemma-3-12b-it",
		"llama-4-scout", "llama4:17b", "mistral-small-3.1", "mistral-small3.1:24b",
		"phi-4-multimodal", "phi4-multimodal-instruct",
	}
	textOnly := []string{
		"gpt-oss:120b", "kimi-for-coding", "deepseek-coder", "qwen2.5-coder",
		"codestral", "llama3.1-8b", "grok-text-only", "mistral-large",
		"gemma3:1b", "gemma-3-1b-it", "gemma3:270m", "gemma-3-270m-it",
		"gemma2:27b", "mistral-small3:24b", "phi4:14b",
		// Negated "vision" names must NOT match the bare-"vision" fallback.
		"my-custom-vision-less-model", "no-vision-model", "grok-vision-less",
	}
	for _, m := range vision {
		if !VisionCapableByName(m) {
			t.Errorf("expected %q to be recognized as vision-capable", m)
		}
	}
	for _, m := range textOnly {
		if VisionCapableByName(m) {
			t.Errorf("expected %q to NOT be vision-capable", m)
		}
	}
}

func TestSupportsVisionFallbackForUnknownModels(t *testing.T) {
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatal(err)
	}
	// Catalog model: authoritative vision flag.
	if !SupportsVision(reg, "gpt-4o") {
		t.Error("gpt-4o should be vision (catalog)")
	}
	// Unknown-to-catalog but real vision models now pass via the name fallback.
	for _, m := range []string{"gemini-3-flash-preview", "grok-4.3", "MiniMax-M3"} {
		if !SupportsVision(reg, m) {
			t.Errorf("%q should be vision via name fallback", m)
		}
	}
	// Unknown text-only models stay refused.
	for _, m := range []string{"gpt-oss:120b", "kimi-for-coding"} {
		if SupportsVision(reg, m) {
			t.Errorf("%q should NOT be vision", m)
		}
	}
}
