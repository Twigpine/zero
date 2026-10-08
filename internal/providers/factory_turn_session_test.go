package providers

import (
	"context"
	"errors"
	"testing"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// TestNewTurnSessionProviderForEveryKind asserts every current provider kind
// wraps into the default TurnSessionProvider: construction succeeds, a session
// opens, and Compact reports unsupported (the default adapter contract).
func TestNewTurnSessionProviderForEveryKind(t *testing.T) {
	t.Parallel()
	kinds := []config.ProviderKind{
		config.ProviderKindOpenAI,
		config.ProviderKindOpenAICompatible,
		config.ProviderKindAnthropic,
		config.ProviderKindAnthropicCompat,
		config.ProviderKindGoogle,
	}
	for _, kind := range kinds {
		t.Run(string(kind), func(t *testing.T) {
			tsp, err := NewTurnSessionProvider(config.ProviderProfile{
				Name:         "pr7-" + string(kind),
				ProviderKind: kind,
				BaseURL:      "https://provider.example/v1",
				APIKey:       "sk-turn-session-test",
				// A model absent from the registry: resolution falls back to the
				// raw id, so this test stays hermetic across catalog changes.
				Model: "pr7-unregistered-model",
			}, Options{UserAgent: "zero-turn-session-test"})
			if err != nil {
				t.Fatalf("NewTurnSessionProvider(%s): %v", kind, err)
			}
			session, err := tsp.OpenTurnSession(context.Background())
			if err != nil {
				t.Fatalf("OpenTurnSession(%s): %v", kind, err)
			}
			defer func() {
				if closeErr := session.Close(); closeErr != nil {
					t.Fatalf("Close(%s): %v", kind, closeErr)
				}
			}()
			if err := session.Prewarm(context.Background()); err != nil {
				t.Fatalf("Prewarm(%s): %v", kind, err)
			}
			if _, compactErr := session.Compact(context.Background(), zeroruntime.CompletionRequest{}); !errors.Is(compactErr, zeroruntime.ErrCompactionUnsupported) {
				t.Fatalf("Compact(%s) = %v, want ErrCompactionUnsupported", kind, compactErr)
			}
			caps := tsp.Capabilities()
			if caps.Model != "pr7-unregistered-model" {
				t.Fatalf("Capabilities(%s).Model = %q, want the resolved api model", kind, caps.Model)
			}
			if caps.NativeCompaction {
				t.Fatalf("Capabilities(%s).NativeCompaction = true, want false for the default adapter", kind)
			}
		})
	}
}

// TestNewTurnSessionProviderProjectsRegistryCapabilities asserts the factory
// projects the model-registry entry (context limits, capability flags,
// reasoning efforts) into the flat ProviderCapabilities.
func TestNewTurnSessionProviderProjectsRegistryCapabilities(t *testing.T) {
	t.Parallel()
	registry, err := modelregistry.NewRegistry([]modelregistry.ModelEntry{{
		ID:          "pr7-caps-model",
		DisplayName: "PR7 Capability Probe",
		APIModel:    "pr7-caps-api-model",
		Provider:    modelregistry.ProviderOpenAI,
		ContextLimits: modelregistry.ContextLimits{
			ContextWindow:   200_000,
			MaxOutputTokens: 64_000,
		},
		ReasoningEfforts: []modelregistry.ReasoningEffort{
			modelregistry.ReasoningEffortLow,
			modelregistry.ReasoningEffortMedium,
			modelregistry.ReasoningEffortHigh,
		},
		Capabilities: modelregistry.ModelCapabilities{
			modelregistry.ModelCapabilityChat,
			modelregistry.ModelCapabilityVision,
			modelregistry.ModelCapabilityReasoning,
			modelregistry.ModelCapabilityPromptCache,
		},
		Cost: modelregistry.ModelCost{
			Currency:           "USD",
			Unit:               "per_1m_tokens",
			InputPerMillion:    1,
			OutputPerMillion:   2,
			Source:             "https://provider.example/pricing (test fixture)",
			SourceLastVerified: "2026-07-18",
		},
		Status:  modelregistry.ModelStatusActive,
		Aliases: []string{"pr7-caps"},
	}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	tsp, err := NewTurnSessionProvider(config.ProviderProfile{
		Name:         "pr7-caps",
		ProviderKind: config.ProviderKindOpenAI,
		BaseURL:      "https://provider.example/v1",
		APIKey:       "sk-turn-session-test",
		Model:        "pr7-caps-model",
	}, Options{
		UserAgent:     "zero-turn-session-test",
		ModelRegistry: &registry,
	})
	if err != nil {
		t.Fatalf("NewTurnSessionProvider: %v", err)
	}

	caps := tsp.Capabilities()
	if caps.Model != "pr7-caps-api-model" {
		t.Fatalf("Model = %q, want the registry APIModel", caps.Model)
	}
	if caps.ContextWindow != 200_000 || caps.MaxOutputTokens != 64_000 {
		t.Fatalf("limits = (%d, %d), want (200000, 64000)", caps.ContextWindow, caps.MaxOutputTokens)
	}
	if !caps.SupportsVision || !caps.SupportsReasoning || !caps.SupportsPromptCache {
		t.Fatalf("capability flags = %+v, want vision/reasoning/prompt-cache all true", caps)
	}
	if len(caps.ReasoningEfforts) != 3 || caps.ReasoningEfforts[0] != "low" || caps.ReasoningEfforts[2] != "high" {
		t.Fatalf("ReasoningEfforts = %v, want [low medium high]", caps.ReasoningEfforts)
	}
	if caps.NativeCompaction {
		t.Fatal("NativeCompaction = true, want false for the default adapter")
	}
}

// TestNewTurnSessionProviderUsesEffectiveReasoningEfforts asserts the
// projection reads efforts through Registry.ReasoningEfforts, so a catalog
// entry that enumerates no efforts of its own still reports the name-inferred
// effective tiers the /effort picker and run-time resolver advertise.
func TestNewTurnSessionProviderUsesEffectiveReasoningEfforts(t *testing.T) {
	t.Parallel()
	registry, err := modelregistry.NewRegistry([]modelregistry.ModelEntry{{
		// A gpt-5-family id with NO ReasoningEfforts listed: the effective
		// efforts come from name inference, differing from the raw entry.
		ID:          "gpt-5-pr7-probe",
		DisplayName: "PR7 Effective Efforts Probe",
		APIModel:    "gpt-5-pr7-probe-api",
		Provider:    modelregistry.ProviderOpenAI,
		ContextLimits: modelregistry.ContextLimits{
			ContextWindow:   400_000,
			MaxOutputTokens: 128_000,
		},
		Capabilities: modelregistry.ModelCapabilities{
			modelregistry.ModelCapabilityChat,
			modelregistry.ModelCapabilityReasoning,
		},
		Cost: modelregistry.ModelCost{
			Currency:           "USD",
			Unit:               "per_1m_tokens",
			InputPerMillion:    1,
			OutputPerMillion:   2,
			Source:             "https://provider.example/pricing (test fixture)",
			SourceLastVerified: "2026-07-18",
		},
		Status:  modelregistry.ModelStatusActive,
		Aliases: []string{"pr7-effective-efforts"},
	}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	tsp, err := NewTurnSessionProvider(config.ProviderProfile{
		Name:         "pr7-effective",
		ProviderKind: config.ProviderKindOpenAI,
		BaseURL:      "https://provider.example/v1",
		APIKey:       "sk-turn-session-test",
		Model:        "gpt-5-pr7-probe",
	}, Options{
		UserAgent:     "zero-turn-session-test",
		ModelRegistry: &registry,
	})
	if err != nil {
		t.Fatalf("NewTurnSessionProvider: %v", err)
	}

	got := tsp.Capabilities().ReasoningEfforts
	want := []string{"minimal", "low", "medium", "high"}
	if len(got) != len(want) {
		t.Fatalf("ReasoningEfforts = %v, want %v (name-inferred fallback)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ReasoningEfforts[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}
