package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/providermodeldiscovery"
)

func TestRunProvidersModelsListsDiscoveredModels(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	deps := commandCenterDeps(t)
	var probed config.ProviderProfile
	deps.discoverProviderModels = func(_ context.Context, profile config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
		probed = profile
		return []providermodeldiscovery.Model{
			{ID: "team/model-a", Description: "first"},
			{ID: "team/model-b"},
		}, nil
	}

	exitCode := runWithDeps([]string{"providers", "models"}, &stdout, &stderr, deps)

	if exitCode != exitSuccess {
		t.Fatalf("exit = %d, want %d: %s", exitCode, exitSuccess, stderr.String())
	}
	// No name given → the active provider is probed, with its resolved credential.
	if probed.Name != "work" {
		t.Fatalf("probed provider = %q, want active provider work", probed.Name)
	}
	out := stdout.String()
	for _, want := range []string{"Provider models (work)", "team/model-a — first", "team/model-b", "2 models discovered", "--model team/model-a"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRunProvidersModelsJSON(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	deps := commandCenterDeps(t)
	deps.discoverProviderModels = func(_ context.Context, _ config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
		return []providermodeldiscovery.Model{{ID: "m1", Description: "d1"}, {ID: "m2"}}, nil
	}

	exitCode := runWithDeps([]string{"providers", "models", "--json"}, &stdout, &stderr, deps)

	if exitCode != exitSuccess {
		t.Fatalf("exit = %d, want %d: %s", exitCode, exitSuccess, stderr.String())
	}
	var payload struct {
		Provider string `json:"provider"`
		Count    int    `json:"count"`
		Models   []struct {
			ID          string `json:"id"`
			Description string `json:"description"`
		} `json:"models"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode json: %v\n%s", err, stdout.String())
	}
	if payload.Provider != "work" || payload.Count != 2 || len(payload.Models) != 2 {
		t.Fatalf("payload = %#v", payload)
	}
	if payload.Models[0].ID != "m1" || payload.Models[0].Description != "d1" {
		t.Fatalf("model[0] = %#v", payload.Models[0])
	}
	// A model with no description omits the field rather than emitting an empty string.
	if payload.Models[1].ID != "m2" || payload.Models[1].Description != "" {
		t.Fatalf("model[1] = %#v", payload.Models[1])
	}
}

func TestRunProvidersModelsSelectsNamedProvider(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	deps := commandCenterDeps(t)
	custom := config.ProviderProfile{
		Name:         "self-hosted",
		ProviderKind: config.ProviderKindOpenAICompatible,
		BaseURL:      "https://models.internal/v1",
		APIKey:       "sk-internal",
		Model:        "house-model",
	}
	deps.resolveConfig = func(string, config.Overrides) (config.ResolvedConfig, error) {
		work := config.ProviderProfile{Name: "work", ProviderKind: config.ProviderKindOpenAI, BaseURL: config.OpenAIBaseURL, APIKey: "sk", Model: "gpt-4.1"}
		return config.ResolvedConfig{ActiveProvider: "work", Provider: work, Providers: []config.ProviderProfile{work, custom}}, nil
	}
	var probed config.ProviderProfile
	deps.discoverProviderModels = func(_ context.Context, profile config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
		probed = profile
		return []providermodeldiscovery.Model{{ID: "house-model"}}, nil
	}

	exitCode := runWithDeps([]string{"providers", "models", "self-hosted"}, &stdout, &stderr, deps)

	if exitCode != exitSuccess {
		t.Fatalf("exit = %d, want %d: %s", exitCode, exitSuccess, stderr.String())
	}
	if probed.Name != "self-hosted" || probed.BaseURL != "https://models.internal/v1" {
		t.Fatalf("probed = %#v, want the named custom provider", probed)
	}
}

func TestRunProvidersModelsUnknownProviderFails(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	deps := commandCenterDeps(t)
	called := false
	deps.discoverProviderModels = func(_ context.Context, _ config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
		called = true
		return nil, nil
	}

	exitCode := runWithDeps([]string{"providers", "models", "ghost"}, &stdout, &stderr, deps)

	if exitCode == exitSuccess {
		t.Fatalf("exit = %d, want non-zero for unknown provider", exitCode)
	}
	if called {
		t.Fatal("discovery must not run for an unknown provider")
	}
	if !strings.Contains(stderr.String(), `provider "ghost" not found`) {
		t.Fatalf("stderr = %q, want provider-not-found", stderr.String())
	}
}

func TestRunProvidersModelsFiltersForCatalogProfile(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	deps := commandCenterDeps(t)
	// Override the resolved config to include a catalog-backed profile.
	deps.resolveConfig = func(_ string, _ config.Overrides) (config.ResolvedConfig, error) {
		profile := config.ProviderProfile{
			Name:         "opencode-go",
			ProviderKind: config.ProviderKindAnthropicCompat,
			CatalogID:    "opencode-go-anthropic-compatible",
			BaseURL:      "https://api.example.com/zen/go/v1",
			APIKey:       "sk-test",
			Model:        "minimax-max",
		}
		return config.ResolvedConfig{
			ActiveProvider: "opencode-go",
			Providers:      []config.ProviderProfile{profile},
			Provider:       profile,
		}, nil
	}
	deps.discoverProviderModels = func(_ context.Context, _ config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
		return []providermodeldiscovery.Model{
			{ID: "deepseek-chat", Description: "DeepSeek Chat"},
			{ID: "qwen-max", Description: "Qwen Max"},
			{ID: "minimax-max-01", Description: "MiniMax Max"},
			{ID: "kimi-k2", Description: "Kimi K2"},
			{ID: "glm-4", Description: "GLM-4"},
		}, nil
	}

	exitCode := runWithDeps([]string{"providers", "models"}, &stdout, &stderr, deps)

	if exitCode != exitSuccess {
		t.Fatalf("exit = %d, want %d: %s", exitCode, exitSuccess, stderr.String())
	}
	out := stdout.String()
	// Only Qwen and MiniMax model IDs should survive the filter.
	if strings.Contains(out, "deepseek-chat") {
		t.Error("output should not contain deepseek-chat (blocked by opencode-go-anthropic filter)")
	}
	if strings.Contains(out, "kimi-k2") {
		t.Error("output should not contain kimi-k2 (blocked by opencode-go-anthropic filter)")
	}
	if strings.Contains(out, "glm-4") {
		t.Error("output should not contain glm-4 (blocked by opencode-go-anthropic filter)")
	}
	for _, want := range []string{"qwen-max", "minimax-max-01"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing allowed model %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "2 models discovered") {
		t.Fatalf("output should show 2 models:\n%s", out)
	}
}

func TestRunProvidersModelsSurfacesDiscoveryError(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	deps := commandCenterDeps(t)
	deps.discoverProviderModels = func(_ context.Context, _ config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
		return nil, errors.New("models endpoint returned 404")
	}

	exitCode := runWithDeps([]string{"providers", "models"}, &stdout, &stderr, deps)

	if exitCode != exitProvider {
		t.Fatalf("exit = %d, want exitProvider %d", exitCode, exitProvider)
	}
	if !strings.Contains(stderr.String(), "models endpoint returned 404") {
		t.Fatalf("stderr = %q, want the discovery error", stderr.String())
	}
}
