package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/providerhealth"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

func TestRunProvidersCheckMissingKeyPrintsNextAction(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	deps := commandCenterDeps(t)
	deps.resolveConfig = func(string, config.Overrides) (config.ResolvedConfig, error) {
		profile := providerCheckGuidanceProfile()
		return config.ResolvedConfig{ActiveProvider: "groq", Provider: profile, Providers: []config.ProviderProfile{profile}, MaxTurns: 7}, nil
	}
	deps.newProvider = func(config.ProviderProfile) (zeroruntime.Provider, error) {
		t.Fatal("newProvider should not run with a missing API key")
		return nil, nil
	}

	exitCode := runWithDeps([]string{"providers", "check", "groq"}, &stdout, &stderr, deps)

	if exitCode != exitProvider {
		t.Fatalf("exit code = %d, want %d", exitCode, exitProvider)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	output := stderr.String()
	if !strings.Contains(output, "provider groq requires API key; set GROQ_API_KEY") {
		t.Fatalf("missing key error did not name env var: %q", output)
	}
	if !strings.Contains(output, "next: set GROQ_API_KEY and rerun zero providers check groq") {
		t.Fatalf("missing next action: %q", output)
	}
}

func TestRunProvidersCheckMissingKeyDoesNotLeakSecrets(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	deps := commandCenterDeps(t)
	deps.resolveConfig = func(string, config.Overrides) (config.ResolvedConfig, error) {
		profile := providerCheckGuidanceProfile()
		profile.BaseURL = "https://user:base-secret@api.groq.com/openai/v1?api_key=query-secret"
		profile.AuthHeader = "Authorization"
		profile.CustomHeaders = map[string]string{"X-Token": "custom-secret"}
		return config.ResolvedConfig{ActiveProvider: "groq", Provider: profile, Providers: []config.ProviderProfile{profile}, MaxTurns: 7}, nil
	}

	exitCode := runWithDeps([]string{"providers", "check", "groq"}, &stdout, &stderr, deps)

	if exitCode != exitProvider {
		t.Fatalf("exit code = %d, want %d", exitCode, exitProvider)
	}
	output := stdout.String() + stderr.String()
	for _, leaked := range []string{"base-secret", "query-secret", "custom-secret", "user:"} {
		if strings.Contains(output, leaked) {
			t.Fatalf("providers check leaked %q: %q", leaked, output)
		}
	}
	if strings.Contains(output, "Authorization:") || strings.Contains(output, "Bearer ") {
		t.Fatalf("providers check leaked auth header material: %q", output)
	}
}

func TestRunProvidersCheckConnectivitySkippedPrintsNextAction(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	deps := commandCenterDeps(t)

	exitCode := runWithDeps([]string{"providers", "check", "work"}, &stdout, &stderr, deps)

	if exitCode != exitSuccess {
		t.Fatalf("exit code = %d, want %d: %s", exitCode, exitSuccess, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "next: run zero providers check work --connectivity") {
		t.Fatalf("missing connectivity next action: %q", output)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunProvidersCheckConnectivityFailurePrintsGuidance(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	deps := commandCenterDeps(t)
	deps.resolveConfig = func(string, config.Overrides) (config.ResolvedConfig, error) {
		profile := config.ProviderProfile{
			Name:         "local",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://api.example.com/v1",
			APIKey:       "sk-test-secret",
			Model:        "custom-model",
		}
		return config.ResolvedConfig{ActiveProvider: "local", Provider: profile, Providers: []config.ProviderProfile{profile}, MaxTurns: 7}, nil
	}
	deps.probeProviderHealth = func(context.Context, providerhealth.Options) providerhealth.Result {
		return providerhealth.Result{
			Status: providerhealth.StatusFail,
			Checks: []providerhealth.Check{
				{ID: "provider.connectivity", Status: providerhealth.StatusFail, Category: providerhealth.CategoryAuth, Message: "Provider endpoint returned 401: invalid API key"},
			},
		}
	}

	exitCode := runWithDeps([]string{"providers", "check", "local", "--connectivity"}, &stdout, &stderr, deps)

	if exitCode != exitProvider {
		t.Fatalf("exit code = %d, want %d", exitCode, exitProvider)
	}
	output := stdout.String()
	if !strings.Contains(output, "provider.connectivity: Provider endpoint returned 401: invalid API key") {
		t.Fatalf("missing primary health check message: %q", output)
	}
	if !strings.Contains(output, "next: verify the API key, base URL, and model, then rerun zero providers check local --connectivity") {
		t.Fatalf("missing connectivity failure next action: %q", output)
	}
	if strings.Contains(output, "sk-test-secret") {
		t.Fatalf("providers check leaked API key: %q", output)
	}
}

func TestRunProvidersCheckConnectivityOKPrintsExecNextAction(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	deps := commandCenterDeps(t)
	deps.probeProviderHealth = func(context.Context, providerhealth.Options) providerhealth.Result {
		return providerhealth.Result{
			Status: providerhealth.StatusPass,
			Checks: []providerhealth.Check{
				{ID: "provider.connectivity", Status: providerhealth.StatusPass, Message: "reachable"},
			},
		}
	}

	exitCode := runWithDeps([]string{"providers", "check", "work", "--connectivity"}, &stdout, &stderr, deps)

	if exitCode != exitSuccess {
		t.Fatalf("exit code = %d, want %d: %s", exitCode, exitSuccess, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, `next: run zero exec "hello" --model gpt-4.1`) {
		t.Fatalf("missing ready next action: %q", output)
	}
}

func providerCheckGuidanceProfile() config.ProviderProfile {
	return config.ProviderProfile{
		Name:         "groq",
		ProviderKind: config.ProviderKindOpenAICompatible,
		CatalogID:    "groq",
		BaseURL:      "https://api.groq.com/openai/v1",
		APIKeyEnv:    "GROQ_API_KEY",
		Model:        "llama-3.3-70b-versatile",
	}
}
