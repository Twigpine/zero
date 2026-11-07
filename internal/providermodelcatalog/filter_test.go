package providermodelcatalog

import (
	"testing"

	"github.com/Twigpine/zero/internal/providercatalog"
)

func TestModelIDAllowedForProvider(t *testing.T) {
	cases := []struct {
		testName   string
		providerID string
		modelID    string
		want       bool
	}{
		// Empty model ID is always rejected.
		{"empty model", "any-provider", "", false},
		{"empty model after trim", "any-provider", "  ", false},

		// opencode-go-anthropic-compatible: only qwen and minimax allowed.
		{"opencode-anthropic qwen", "opencode-go-anthropic-compatible", "qwen3.7-plus", true},
		{"opencode-anthropic minimax", "opencode-go-anthropic-compatible", "MiniMax-M3", true},
		{"opencode-anthropic non-allowed", "opencode-go-anthropic-compatible", "claude-sonnet-4.5", false},
		{"opencode-anthropic deepseek", "opencode-go-anthropic-compatible", "deepseek-chat", false},

		// Case insensitivity and trimming for the blocked provider.
		{"opencode-anthropic case insensitive", "opencode-go-anthropic-compatible", "QWEN3.7-PLUS", true},
		{"opencode-anthropic whitespace", "opencode-go-anthropic-compatible", "  minimax-m3  ", true},

		// Normalized provider ID — e.g. NormalizeID converts spaces/dots to dashes.
		{"opencode-anthropic normalized provider", "opencode go anthropic compatible", "qwen3.7-plus", true},

		// Default branch: all model IDs allowed.
		{"default provider allows anything", "openai", "claude-sonnet-4.5", true},
		{"default provider allows any name", "openai", "x", true},
		{"default provider groq", "groq", "deepseek-chat", true},
	}

	for _, tc := range cases {
		t.Run(tc.testName, func(t *testing.T) {
			got := ModelIDAllowedForProvider(tc.providerID, tc.modelID)
			if got != tc.want {
				t.Errorf("ModelIDAllowedForProvider(%q, %q) = %v, want %v",
					tc.providerID, tc.modelID, got, tc.want)
			}
		})
	}
}

// TestFilterModelsForProvider verifies that filtering delegates correctly.
func TestFilterModelsForProvider(t *testing.T) {
	models := []Model{
		{ID: "qwen3.7-plus"},
		{ID: "MiniMax-M3"},
		{ID: "claude-sonnet-4.5"},
		{ID: "deepseek-chat"},
	}

	filtered := FilterModelsForProvider("opencode-go-anthropic-compatible", models)
	if len(filtered) != 2 {
		t.Fatalf("FilterModelsForProvider returned %d models, want 2: %#v", len(filtered), modelIDs(filtered))
	}
	if filtered[0].ID != "qwen3.7-plus" || filtered[1].ID != "MiniMax-M3" {
		t.Errorf("FilterModelsForProvider returned %#v, want [qwen3.7-plus MiniMax-M3]", modelIDs(filtered))
	}

	// With a default provider nothing is filtered out.
	all := FilterModelsForProvider("openai", models)
	if len(all) != len(models) {
		t.Fatalf("FilterModelsForProvider(openai) returned %d, want %d", len(all), len(models))
	}

	// Empty input.
	empty := FilterModelsForProvider("opencode-go-anthropic-compatible", nil)
	if len(empty) != 0 {
		t.Fatal("expected empty result for nil input")
	}
}

// TestModelIDAllowedForProvider_NormalizedProviderIDs verifies that various
// raw provider ID formats that NormalizeID maps to the same canonical form
// are handled consistently.
func TestModelIDAllowedForProvider_NormalizedProviderIDs(t *testing.T) {
	canonical := providercatalog.NormalizeID("opencode-go-anthropic-compatible")
	variants := []string{
		"opencode-go-anthropic-compatible",
		"opencode_go_anthropic_compatible",
		"opencode go anthropic compatible",
		"OpenCode-Go-Anthropic-Compatible",
		"  opencode-go-anthropic-compatible  ",
		"opencode.go.anthropic.compatible",
	}
	for _, variant := range variants {
		if n := providercatalog.NormalizeID(variant); n != canonical {
			t.Fatalf("NormalizeID(%q) = %q, want %q", variant, n, canonical)
		}
	}

	// All variants should produce the same filter result.
	for _, variant := range variants {
		if !ModelIDAllowedForProvider(variant, "qwen3.7-plus") {
			t.Errorf("ModelIDAllowedForProvider(%q, qwen3.7-plus) = false, want true", variant)
		}
		if ModelIDAllowedForProvider(variant, "claude-sonnet-4.5") {
			t.Errorf("ModelIDAllowedForProvider(%q, claude-sonnet-4.5) = true, want false", variant)
		}
	}
}
