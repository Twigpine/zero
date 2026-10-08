package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/provideronboarding"
)

func TestRunProvidersDetectSurfacesLocalRuntimeAndProviderActions(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := commandCenterDeps(t)
	deps.detectLocalRuntimes = func(context.Context, provideronboarding.LocalDetectOptions) []provideronboarding.DetectedLocalRuntime {
		return []provideronboarding.DetectedLocalRuntime{{
			LocalRuntime: provideronboarding.LocalRuntime{CatalogID: "ollama", Name: "Ollama Local", BaseURL: "http://localhost:11434/v1"},
			Reachable:    true,
			Models:       []string{"llama3.1"},
		}}
	}
	deps.resolveConfig = func(string, config.Overrides) (config.ResolvedConfig, error) {
		// Configured-but-inactive provider with no credential: should yield the full
		// "Use provider" / "Check provider" / "Set API key" action set.
		profile := config.ProviderProfile{
			Name:         "openai",
			ProviderKind: config.ProviderKindOpenAI,
			Model:        "gpt-4o",
			APIKeyEnv:    "OPENAI_API_KEY",
		}
		return config.ResolvedConfig{ActiveProvider: "other", Providers: []config.ProviderProfile{profile}}, nil
	}

	exitCode := runWithDeps([]string{"providers", "detect"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("exit=%d stderr=%s", exitCode, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"Detected local runtimes:",
		"Ollama Local — http://localhost:11434/v1",
		"models: llama3.1",
		"zero providers add ollama", // SetupAction's no-key adopt command
		"Configured providers:",
		"openai",
		"Use provider",
		"Check provider",
		"Set API key",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("detect output missing %q:\n%s", want, out)
		}
	}
}

func TestRunProvidersDetectJSONNoRuntimesActiveProvider(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := commandCenterDeps(t)
	deps.detectLocalRuntimes = func(context.Context, provideronboarding.LocalDetectOptions) []provideronboarding.DetectedLocalRuntime {
		return nil // nothing running locally
	}
	deps.resolveConfig = func(string, config.Overrides) (config.ResolvedConfig, error) {
		// Active provider with an inline key: only "Check provider" remains.
		profile := config.ProviderProfile{
			Name:         "local",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://api.example.com/v1",
			APIKey:       "sk-secret",
			Model:        "custom",
		}
		return config.ResolvedConfig{ActiveProvider: "local", Provider: profile, Providers: []config.ProviderProfile{profile}}, nil
	}

	exitCode := runWithDeps([]string{"providers", "detect", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("exit=%d stderr=%s", exitCode, stderr.String())
	}
	if strings.Contains(stdout.String(), "sk-secret") {
		t.Fatalf("detect JSON leaked the API key: %s", stdout.String())
	}
	var payload struct {
		DetectedRuntimes []struct {
			CatalogID string `json:"catalogID"`
		} `json:"detectedRuntimes"`
		Providers []struct {
			Name    string `json:"name"`
			Active  bool   `json:"active"`
			Actions []struct {
				Label string `json:"label"`
			} `json:"actions"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("detect JSON did not decode: %v\n%s", err, stdout.String())
	}
	if len(payload.DetectedRuntimes) != 0 {
		t.Fatalf("expected no detected runtimes, got %#v", payload.DetectedRuntimes)
	}
	if len(payload.Providers) != 1 || payload.Providers[0].Name != "local" || !payload.Providers[0].Active {
		t.Fatalf("expected one active provider 'local', got %#v", payload.Providers)
	}
	if len(payload.Providers[0].Actions) != 1 || payload.Providers[0].Actions[0].Label != "Check provider" {
		t.Fatalf("expected only a Check action for an active keyed provider, got %#v", payload.Providers[0].Actions)
	}
}

func TestProviderDetectShowsGuidanceWithoutCommand(t *testing.T) {
	for _, models := range [][]string{nil, {"local-model"}, {"x&calc"}} {
		runtime := provideronboarding.DetectedLocalRuntime{LocalRuntime: provideronboarding.LocalRuntime{CatalogID: "atomic-chat-local", Name: "Atomic Chat Local", DefaultModel: "local-model"}, Models: models}
		out := formatProviderDetectReport(buildProviderDetectReport(config.ResolvedConfig{}, []provideronboarding.DetectedLocalRuntime{runtime}))
		if strings.Contains(out, "providers add") || !strings.Contains(out, runtime.SetupAction().Detail) {
			t.Fatalf("missing actionable guidance: %s", out)
		}
	}
}
