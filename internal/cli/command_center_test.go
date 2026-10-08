package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/oauth"
	"github.com/Gitlawb/zero/internal/providerhealth"
	"github.com/Gitlawb/zero/internal/zerocommands"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

func TestRunConfigPrintsRedactedSummary(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"config"}, &stdout, &stderr, commandCenterDeps(t))

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"Config", "active provider: work", "max turns: 7", "work [openai]", "api key: set"} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected config output to contain %q, got %q", want, output)
		}
	}
	if strings.Contains(output, "sk-test") {
		t.Fatalf("config output leaked API key: %q", output)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
}

func TestRunConfigPrintsJSONSummary(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"config", "--json"}, &stdout, &stderr, commandCenterDeps(t))

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{`"activeProvider": "work"`, `"apiKeySet": true`, `"maxTurns": 7`} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected config JSON to contain %q, got %q", want, output)
		}
	}
	if strings.Contains(output, "sk-test") {
		t.Fatalf("config JSON leaked API key: %q", output)
	}
}

func TestRunConfigAndProvidersRedactBaseURLSecrets(t *testing.T) {
	deps := commandCenterSecretBaseURLDeps(t)
	commands := [][]string{
		{"config", "--json"},
		{"providers", "current"},
		{"providers", "current", "--json"},
	}

	for _, command := range commands {
		var stdout bytes.Buffer
		var stderr bytes.Buffer

		exitCode := runWithDeps(command, &stdout, &stderr, deps)

		if exitCode != exitSuccess {
			t.Fatalf("%v: expected exit code %d, got %d: %s", command, exitSuccess, exitCode, stderr.String())
		}
		output := stdout.String()
		errorOutput := stderr.String()
		if errorOutput != "" {
			t.Fatalf("%v: expected empty stderr, got %q", command, errorOutput)
		}
		for _, leaked := range []string{"user:", "super-secret", "query-secret", "sk-test"} {
			if strings.Contains(output, leaked) {
				t.Fatalf("%v: output leaked %q: %q", command, leaked, output)
			}
			if strings.Contains(errorOutput, leaked) {
				t.Fatalf("%v: stderr leaked %q: %q", command, leaked, errorOutput)
			}
		}
		if !strings.Contains(output, "https://proxy.example/v1") {
			t.Fatalf("%v: expected sanitized provider base URL host/path, got %q", command, output)
		}
		if !strings.Contains(output, "api_key=[REDACTED]") {
			t.Fatalf("%v: expected redacted query secret, got %q", command, output)
		}
	}
}

func TestRunConfigRejectsModelOnlyFlags(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"config", "--include-deprecated"}, &stdout, &stderr, commandCenterDeps(t))

	if exitCode != exitUsage {
		t.Fatalf("expected exit code %d, got %d", exitUsage, exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected empty stdout, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), `unknown flag "--include-deprecated"`) {
		t.Fatalf("expected unknown flag error, got %q", stderr.String())
	}
}

func TestRunModelsListsRegistryModels(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"models", "list", "--provider", "anthropic"}, &stdout, &stderr, commandCenterDeps(t))

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "Models") || !strings.Contains(output, "claude-sonnet-4.5") {
		t.Fatalf("expected anthropic models in output, got %q", output)
	}
	if strings.Contains(output, "gpt-4.1") {
		t.Fatalf("expected provider filter to hide OpenAI models, got %q", output)
	}
}

func TestRunModelsRejectsUnknownProvider(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"models", "--provider", "missing"}, &stdout, &stderr, commandCenterDeps(t))

	if exitCode != exitUsage {
		t.Fatalf("expected exit code %d, got %d", exitUsage, exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected empty stdout, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "unknown model provider") {
		t.Fatalf("expected unknown provider error, got %q", stderr.String())
	}
}

func TestRunProvidersShowsCurrentProvider(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"providers", "current"}, &stdout, &stderr, commandCenterDeps(t))

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"Provider", "name: work", "kind: openai", "model: gpt-4.1"} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected provider output to contain %q, got %q", want, output)
		}
	}
	if strings.Contains(output, "sk-test") {
		t.Fatalf("provider output leaked API key: %q", output)
	}
}

func TestRunProvidersCurrentJSONIncludesRuntimeMetadata(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"providers", "current", "--json"}, &stdout, &stderr, commandCenterDeps(t))

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{`"name": "work"`, `"providerKind": "openai"`, `"apiModel": "gpt-4.1"`, `"apiKeySet": true`} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected provider JSON to contain %q, got %q", want, output)
		}
	}
	if strings.Contains(output, "sk-test") {
		t.Fatalf("provider JSON leaked API key: %q", output)
	}
}

func TestRunProvidersCatalogListsDescriptors(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"providers", "catalog"}, &stdout, &stderr, providerCatalogDeps(t))

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{
		"Provider Catalog",
		"id=openai",
		"name=OpenAI",
		"transport=openai",
		"defaultModel=gpt-4.1",
		"defaultBaseURL=https://api.openai.com/v1",
		"authEnvVars=OPENAI_API_KEY",
		"requiresAuth=true",
		"local=false",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected providers catalog output to contain %q, got %q", want, output)
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
}

func TestFormatProviderCatalogValueQuotesControlCharacters(t *testing.T) {
	value := formatProviderCatalogValue("bad\rvalue", "none")

	if value != strconv.Quote("bad\rvalue") {
		t.Fatalf("formatProviderCatalogValue() = %q, want quoted control character value", value)
	}
}

func TestRunProvidersCatalogJSONIncludesDescriptors(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"providers", "catalog", "--json"}, &stdout, &stderr, providerCatalogDeps(t))

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	var payload struct {
		Providers []zerocommands.ProviderCatalogSnapshot `json:"providers"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode providers catalog JSON: %v\n%s", err, stdout.String())
	}
	openai := findProviderCatalogSnapshot(t, payload.Providers, "openai")
	if openai.Name != "OpenAI" ||
		openai.Transport != "openai" ||
		openai.DefaultBaseURL != "https://api.openai.com/v1" ||
		openai.DefaultModel != "gpt-4.1" ||
		!openai.RequiresAuth ||
		openai.Local {
		t.Fatalf("unexpected OpenAI catalog descriptor: %#v", openai)
	}
	if len(openai.AuthEnvVars) != 1 || openai.AuthEnvVars[0] != "OPENAI_API_KEY" {
		t.Fatalf("unexpected OpenAI auth env vars: %#v", openai.AuthEnvVars)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
}

func TestRunProvidersCatalogFiltersByTransport(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"providers", "catalog", "--transport", "openai"}, &stdout, &stderr, providerCatalogDeps(t))

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "id=openai") || !strings.Contains(output, "transport=openai") {
		t.Fatalf("expected OpenAI transport providers in filtered catalog output, got %q", output)
	}
	if strings.Contains(output, "(none)") {
		t.Fatalf("expected non-empty HTTP catalog output, got %q", output)
	}
}

func TestRunProvidersCatalogRejectsUnknownTransport(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"providers", "catalog", "--transport", "space-link"}, &stdout, &stderr, providerCatalogDeps(t))

	if exitCode != exitUsage {
		t.Fatalf("expected exit code %d, got %d", exitUsage, exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected empty stdout, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), `unknown provider transport "space-link"`) {
		t.Fatalf("expected unknown transport error, got %q", stderr.String())
	}
}

func TestRunProvidersCatalogRejectsUnknownFlags(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"providers", "catalog", "--include-deprecated"}, &stdout, &stderr, providerCatalogDeps(t))

	if exitCode != exitUsage {
		t.Fatalf("expected exit code %d, got %d", exitUsage, exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected empty stdout, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), `unknown flag "--include-deprecated"`) {
		t.Fatalf("expected unknown flag error, got %q", stderr.String())
	}
}

func TestRunProvidersAddWritesCatalogProfile(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")

	exitCode := runWithDeps([]string{"providers", "add", "groq", "--name", "fast", "--set-active"}, &stdout, &stderr, providerSetupDeps(configPath))

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	cfg := readFileConfig(t, configPath)
	if cfg.ActiveProvider != "fast" {
		t.Fatalf("ActiveProvider = %q, want fast", cfg.ActiveProvider)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("providers = %#v, want one provider", cfg.Providers)
	}
	profile := cfg.Providers[0]
	if profile.Name != "fast" ||
		profile.CatalogID != "groq" ||
		profile.ProviderKind != config.ProviderKindOpenAICompatible ||
		profile.BaseURL != "https://api.groq.com/openai/v1" ||
		profile.Model != "llama-3.3-70b-versatile" ||
		profile.APIKeyEnv != "GROQ_API_KEY" {
		t.Fatalf("unexpected provider profile: %#v", profile)
	}
	if profile.APIKey != "" {
		t.Fatalf("providers add must not persist raw API keys: %#v", profile)
	}
	output := stdout.String()
	if !strings.Contains(output, "Added provider fast") || !strings.Contains(output, configPath) {
		t.Fatalf("unexpected add output: %q", output)
	}
}

func TestRunProvidersAddAimlapiWritesDefaultHeaders(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")

	exitCode := runWithDeps([]string{"providers", "add", "aimlapi"}, &stdout, &stderr, providerSetupDeps(configPath))

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	cfg := readFileConfig(t, configPath)
	if len(cfg.Providers) != 1 {
		t.Fatalf("providers = %#v, want one provider", cfg.Providers)
	}
	profile := cfg.Providers[0]
	if profile.Name != "aimlapi" ||
		profile.CatalogID != "aimlapi" ||
		profile.BaseURL != "https://api.aimlapi.com/v1" ||
		profile.Model != "anthropic/claude-sonnet-5" ||
		profile.APIKeyEnv != "AIMLAPI_API_KEY" {
		t.Fatalf("unexpected provider profile: %#v", profile)
	}
	if profile.CustomHeaders["X-AIMLAPI-Partner-ID"] != "part_62yQoGYDq4Yqnrj2R1iGrDNJ" ||
		profile.CustomHeaders["X-AIMLAPI-Integration-Repo"] != "Gitlawb/zero" {
		t.Fatalf("missing aimlapi.com default headers: %#v", profile.CustomHeaders)
	}
}

func TestRunProvidersAddAimlapiMixedCaseHeaderOverride(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")

	// A differently-cased override must replace the catalog header in place, not
	// leave both spellings behind to race when request construction canonicalizes.
	exitCode := runWithDeps([]string{
		"providers", "add", "aimlapi",
		"--header", "x-aimlapi-partner-id=part_override",
	}, &stdout, &stderr, providerSetupDeps(configPath))

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	profile := readFileConfig(t, configPath).Providers[0]
	if _, ok := profile.CustomHeaders["x-aimlapi-partner-id"]; ok {
		t.Fatalf("lowercase override left a duplicate key: %#v", profile.CustomHeaders)
	}
	if profile.CustomHeaders["X-AIMLAPI-Partner-ID"] != "part_override" {
		t.Fatalf("override did not win on the canonical key: %#v", profile.CustomHeaders)
	}
}

func TestRunProvidersAddAimlapiOverrideDropsCatalogHeaders(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")

	exitCode := runWithDeps([]string{
		"providers", "add", "aimlapi",
		"--base-url", "https://staging.example/v1",
		"--header", "X-Environment=staging",
	}, &stdout, &stderr, providerSetupDeps(configPath))
	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	profile := readFileConfig(t, configPath).Providers[0]
	if _, ok := profile.CustomHeaders["X-AIMLAPI-Partner-ID"]; ok {
		t.Fatalf("partner attribution leaked to overridden endpoint: %#v", profile.CustomHeaders)
	}
	if _, ok := profile.CustomHeaders["X-AIMLAPI-Integration-Repo"]; ok {
		t.Fatalf("integration attribution leaked to overridden endpoint: %#v", profile.CustomHeaders)
	}
	if profile.CustomHeaders["X-Environment"] != "staging" {
		t.Fatalf("explicit user header was dropped: %#v", profile.CustomHeaders)
	}
}

func TestRunProvidersAddWritesCustomHeaders(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	configPath := filepath.Join(t.TempDir(), "config.json")

	exitCode := runWithDeps([]string{
		"providers", "add", "custom-openai-compatible",
		"--name", "gateway",
		"--base-url", "https://gateway.example/v1",
		"--model", "gateway-model",
		"--api-key-env", "GATEWAY_API_KEY",
		"--auth-header", "X-API-Key",
		"--auth-scheme", "Token",
		"--header", "HTTP-Referer=https://zero.dev",
		"--header", "X-Title=Zero",
	}, &stdout, &stderr, providerSetupDeps(configPath))

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	profile := readFileConfig(t, configPath).Providers[0]
	if profile.AuthHeader != "X-API-Key" || profile.AuthScheme != "Token" {
		t.Fatalf("unexpected auth override: %#v", profile)
	}
	if profile.CustomHeaders["HTTP-Referer"] != "https://zero.dev" || profile.CustomHeaders["X-Title"] != "Zero" {
		t.Fatalf("unexpected custom headers: %#v", profile.CustomHeaders)
	}
}

func TestRunProvidersAddRejectsCatalogOnlyTransports(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"providers", "add", "bedrock"}, &stdout, &stderr, providerSetupDeps(filepath.Join(t.TempDir(), "config.json")))

	if exitCode != exitUsage {
		t.Fatalf("expected exit code %d, got %d", exitUsage, exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected empty stdout, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "native adapter") {
		t.Fatalf("expected native adapter warning, got %q", stderr.String())
	}
}

func TestRunProvidersCheckConstructsProvider(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	var checked config.ProviderProfile
	deps := commandCenterDeps(t)
	deps.newProvider = func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
		checked = profile
		return commandCenterProvider{}, nil
	}

	exitCode := runWithDeps([]string{"providers", "check", "work"}, &stdout, &stderr, deps)

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if checked.Name != "work" || checked.Model != "gpt-4.1" {
		t.Fatalf("checked profile = %#v, want work provider", checked)
	}
	output := stdout.String()
	if !strings.Contains(output, "Provider check") || !strings.Contains(output, "status: ok") {
		t.Fatalf("unexpected check output: %q", output)
	}
}

func TestRunProvidersCheckConnectivityJSON(t *testing.T) {
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
	deps.probeProviderHealth = func(_ context.Context, options providerhealth.Options) providerhealth.Result {
		if !options.Connectivity || options.Profile.Name != "local" {
			t.Fatalf("unexpected health probe options: %#v", options)
		}
		return providerhealth.Result{
			Status: providerhealth.StatusPass,
			Checks: []providerhealth.Check{
				{ID: "provider.connectivity", Status: providerhealth.StatusPass, Message: "reachable"},
			},
		}
	}
	deps.newProvider = func(config.ProviderProfile) (zeroruntime.Provider, error) {
		t.Fatal("newProvider should not run during connectivity health check")
		return nil, nil
	}

	exitCode := runWithDeps([]string{"providers", "check", "local", "--connectivity", "--json"}, &stdout, &stderr, deps)

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
	var payload struct {
		Status string `json:"status"`
		Health struct {
			Status string `json:"status"`
			Checks []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"checks"`
		} `json:"health"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("providers check JSON did not decode: %v\n%s", err, stdout.String())
	}
	if payload.Status != "ok" || payload.Health.Status != "pass" {
		t.Fatalf("unexpected health payload: %#v", payload)
	}
	if !providerHealthCheckStatus(payload.Health.Checks, "provider.connectivity", "pass") {
		t.Fatalf("missing provider.connectivity pass: %#v", payload.Health.Checks)
	}
}

func TestRunProvidersCheckConnectivityJSONSurfacesWarningStatus(t *testing.T) {
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
			Status: providerhealth.StatusWarn,
			Checks: []providerhealth.Check{
				{ID: "provider.connectivity", Status: providerhealth.StatusWarn, Category: providerhealth.CategoryRateLimit, Message: "rate limited"},
			},
		}
	}

	exitCode := runWithDeps([]string{"providers", "check", "local", "--connectivity", "--json"}, &stdout, &stderr, deps)

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	var payload struct {
		Status string `json:"status"`
		Health struct {
			Status string `json:"status"`
		} `json:"health"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("providers check JSON did not decode: %v\n%s", err, stdout.String())
	}
	if payload.Status != "warn" || payload.Health.Status != "warn" {
		t.Fatalf("unexpected warning payload: %#v", payload)
	}
}

func TestRunProvidersCheckConnectivityJSONReturnsHealthFailure(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	deps := commandCenterDeps(t)
	deps.resolveConfig = func(string, config.Overrides) (config.ResolvedConfig, error) {
		profile := config.ProviderProfile{
			Name:         "local",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://user:base-secret@example.invalid/v1?api_key=query-secret",
		}
		return config.ResolvedConfig{ActiveProvider: "local", Provider: profile, Providers: []config.ProviderProfile{profile}, MaxTurns: 7}, nil
	}
	deps.newProvider = func(config.ProviderProfile) (zeroruntime.Provider, error) {
		t.Fatal("newProvider should not run before emitting connectivity health")
		return nil, nil
	}

	exitCode := runWithDeps([]string{"providers", "check", "local", "--connectivity", "--json"}, &stdout, &stderr, deps)

	if exitCode != exitProvider {
		t.Fatalf("expected exit code %d, got %d: %s", exitProvider, exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
	var payload struct {
		Status string `json:"status"`
		Health struct {
			Status  string `json:"status"`
			BaseURL string `json:"baseURL"`
			Checks  []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"checks"`
		} `json:"health"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("providers check JSON did not decode: %v\n%s", err, stdout.String())
	}
	if payload.Status != "fail" || payload.Health.Status != "fail" {
		t.Fatalf("unexpected health failure payload: %#v", payload)
	}
	if !providerHealthCheckStatus(payload.Health.Checks, "provider.config", "fail") {
		t.Fatalf("missing provider.config failure: %#v", payload.Health.Checks)
	}
	output := stdout.String()
	for _, secret := range []string{"base-secret", "query-secret"} {
		if strings.Contains(output, secret) {
			t.Fatalf("secret %q leaked in providers check JSON: %s", secret, output)
		}
	}
	if strings.Contains(output, "user:") {
		t.Fatalf("URL userinfo leaked in providers check JSON: %s", output)
	}
}

func providerHealthCheckStatus(checks []struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}, id string, status string) bool {
	for _, check := range checks {
		if check.ID == id && check.Status == status {
			return true
		}
	}
	return false
}

func TestRunProvidersCheckAcceptsAuthHeaderValueCredential(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	var checked config.ProviderProfile
	deps := commandCenterDeps(t)
	deps.resolveConfig = func(string, config.Overrides) (config.ResolvedConfig, error) {
		profile := config.ProviderProfile{
			Name:            "groq",
			ProviderKind:    config.ProviderKindOpenAICompatible,
			CatalogID:       "groq",
			BaseURL:         "https://api.groq.com/openai/v1",
			APIKeyEnv:       "GROQ_API_KEY",
			AuthHeader:      "X-API-Key",
			AuthHeaderValue: "direct-secret",
			Model:           "llama-3.3-70b-versatile",
		}
		return config.ResolvedConfig{ActiveProvider: "groq", Provider: profile, Providers: []config.ProviderProfile{profile}, MaxTurns: 7}, nil
	}
	deps.newProvider = func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
		checked = profile
		return commandCenterProvider{}, nil
	}

	exitCode := runWithDeps([]string{"providers", "check", "groq"}, &stdout, &stderr, deps)

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if checked.AuthHeaderValue != "direct-secret" || checked.APIKey != "" {
		t.Fatalf("checked profile = %#v, want direct auth header value without API key", checked)
	}
	if !strings.Contains(stdout.String(), "status: ok") {
		t.Fatalf("expected successful check output, got %q", stdout.String())
	}
}

func TestRunProvidersCheckAcceptsOfficialAuthHeaderValueCredential(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	var checked config.ProviderProfile
	deps := commandCenterDeps(t)
	deps.resolveConfig = func(string, config.Overrides) (config.ResolvedConfig, error) {
		profile := config.ProviderProfile{
			Name:            "manual-openai",
			ProviderKind:    config.ProviderKindOpenAI,
			BaseURL:         config.OpenAIBaseURL,
			AuthHeaderValue: "Bearer direct-secret",
			Model:           "gpt-4.1",
		}
		return config.ResolvedConfig{ActiveProvider: "manual-openai", Provider: profile, Providers: []config.ProviderProfile{profile}, MaxTurns: 7}, nil
	}
	deps.newProvider = func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
		checked = profile
		return commandCenterProvider{}, nil
	}

	exitCode := runWithDeps([]string{"providers", "check", "manual-openai"}, &stdout, &stderr, deps)

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if checked.AuthHeaderValue != "Bearer direct-secret" || checked.APIKey != "" {
		t.Fatalf("checked profile = %#v, want direct auth header value without API key", checked)
	}
}

func TestRunProvidersCheckFailsWhenCatalogAuthEnvIsMissing(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	deps := commandCenterDeps(t)
	deps.resolveConfig = func(string, config.Overrides) (config.ResolvedConfig, error) {
		profile := config.ProviderProfile{
			Name:         "groq",
			ProviderKind: config.ProviderKindOpenAICompatible,
			CatalogID:    "groq",
			BaseURL:      "https://api.groq.com/openai/v1",
			APIKeyEnv:    "GROQ_API_KEY",
			Model:        "llama-3.3-70b-versatile",
		}
		return config.ResolvedConfig{ActiveProvider: "groq", Provider: profile, Providers: []config.ProviderProfile{profile}, MaxTurns: 7}, nil
	}

	exitCode := runWithDeps([]string{"providers", "check", "groq"}, &stdout, &stderr, deps)

	if exitCode != exitProvider {
		t.Fatalf("expected exit code %d, got %d", exitProvider, exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected empty stdout, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "GROQ_API_KEY") || strings.Contains(stderr.String(), "sk-") {
		t.Fatalf("expected missing env error without secret leak, got %q", stderr.String())
	}
}

func TestRunProvidersPositionalHelp(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"providers", "help"}, &stdout, &stderr, commandCenterDeps(t))

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"Usage:", "zero providers", "list", "current", "catalog"} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected providers help to contain %q, got %q", want, output)
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
}

func TestRunProvidersRejectsModelOnlyFlags(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"providers", "list", "--provider", "openai"}, &stdout, &stderr, commandCenterDeps(t))

	if exitCode != exitUsage {
		t.Fatalf("expected exit code %d, got %d", exitUsage, exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected empty stdout, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), `unknown flag "--provider"`) {
		t.Fatalf("expected unknown flag error, got %q", stderr.String())
	}
}

func commandCenterDeps(t *testing.T) appDeps {
	t.Helper()

	cwd := t.TempDir()
	return appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			if workspaceRoot != cwd {
				t.Fatalf("workspaceRoot = %q, want %q", workspaceRoot, cwd)
			}
			profile := config.ProviderProfile{
				Name:         "work",
				ProviderKind: config.ProviderKindOpenAI,
				BaseURL:      config.OpenAIBaseURL,
				APIKey:       "sk-test",
				Model:        "gpt-4.1",
			}
			return config.ResolvedConfig{
				ActiveProvider: "work",
				Providers:      []config.ProviderProfile{profile},
				Provider:       profile,
				MaxTurns:       7,
			}, nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return commandCenterProvider{}, nil
		},
	}
}

func commandCenterSecretBaseURLDeps(t *testing.T) appDeps {
	t.Helper()

	deps := commandCenterDeps(t)
	cwd, err := deps.getwd()
	if err != nil {
		t.Fatalf("getwd error: %v", err)
	}
	deps.resolveConfig = func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
		if workspaceRoot != cwd {
			t.Fatalf("workspaceRoot = %q, want %q", workspaceRoot, cwd)
		}
		profile := config.ProviderProfile{
			Name:         "gateway",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://user:super-secret@proxy.example/v1?api_key=query-secret&mode=test",
			APIKey:       "sk-test",
			Model:        "gateway-model",
		}
		return config.ResolvedConfig{
			ActiveProvider: "gateway",
			Providers:      []config.ProviderProfile{profile},
			Provider:       profile,
			MaxTurns:       7,
		}, nil
	}
	return deps
}

func providerCatalogDeps(t *testing.T) appDeps {
	t.Helper()

	return appDeps{
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			t.Fatalf("providers catalog should not resolve runtime config")
			return config.ResolvedConfig{}, nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			t.Fatalf("providers catalog should not construct runtime providers")
			return nil, nil
		},
	}
}

func providerSetupDeps(configPath string) appDeps {
	return appDeps{
		userConfigPath: func() (string, error) {
			return configPath, nil
		},
	}
}

func readFileConfig(t *testing.T, path string) config.FileConfig {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config %s: %v", path, err)
	}
	var cfg config.FileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("decode config %s: %v\n%s", path, err, string(data))
	}
	return cfg
}

func findProviderCatalogSnapshot(t *testing.T, snapshots []zerocommands.ProviderCatalogSnapshot, id string) zerocommands.ProviderCatalogSnapshot {
	t.Helper()

	for _, snapshot := range snapshots {
		if snapshot.ID == id {
			return snapshot
		}
	}
	t.Fatalf("catalog descriptor %q not found in %#v", id, snapshots)
	return zerocommands.ProviderCatalogSnapshot{}
}

type commandCenterProvider struct{}

func (commandCenterProvider) StreamCompletion(context.Context, zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	ch := make(chan zeroruntime.StreamEvent)
	close(ch)
	return ch, nil
}

func TestProviderCredentialStateShowsOAuthLogin(t *testing.T) {
	if got := providerCredentialState(providerSummary{APIKeySet: true}); got != "set" {
		t.Fatalf("keyed provider = %q, want set", got)
	}
	if got := providerCredentialState(providerSummary{OAuthLogin: true}); got != "oauth login" {
		t.Fatalf("token-login provider = %q, want oauth login", got)
	}
	if got := providerCredentialState(providerSummary{}); got != "not set" {
		t.Fatalf("credential-less provider = %q, want not set", got)
	}
}

// TestProvidersListMarksOAuthLoginProviders: a keyless profile whose credential
// is a stored OAuth login (the shape `zero auth chatgpt` now writes) must render
// as "oauth login", not as a broken "api key: not set" entry.
func TestProvidersListMarksOAuthLoginProviders(t *testing.T) {
	tokensPath := filepath.Join(t.TempDir(), "oauth-tokens.json")
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", tokensPath)
	store, err := oauth.NewStore(oauth.StoreOptions{})
	if err != nil {
		t.Fatalf("oauth store: %v", err)
	}
	if err := store.Save(oauth.ProviderKey("chatgpt"), oauth.Token{AccessToken: "bearer-123"}); err != nil {
		t.Fatalf("save token: %v", err)
	}

	resolved := config.ResolvedConfig{
		ActiveProvider: "opengateway",
		Providers: []config.ProviderProfile{
			{Name: "opengateway", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://gateway.example.com/v1", APIKeyStored: true, Model: "some-model"},
			{Name: "chatgpt", CatalogID: "chatgpt", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://chatgpt.com/backend-api/codex", Model: "gpt-5.5"},
		},
	}
	summary := summarizeConfig(resolved)
	byName := map[string]providerSummary{}
	for _, provider := range summary.Providers {
		byName[provider.Name] = provider
	}
	if !byName["opengateway"].APIKeySet {
		t.Fatalf("stored-key provider must report a credential: %+v", byName["opengateway"])
	}
	if byName["opengateway"].OAuthLogin {
		t.Fatalf("key-authed provider must not claim the OAuth login: %+v", byName["opengateway"])
	}
	if !byName["chatgpt"].OAuthLogin || byName["chatgpt"].APIKeySet {
		t.Fatalf("chatgpt must be marked oauth-login and keyless: %+v", byName["chatgpt"])
	}

	rendered := formatProviderSummaries("list", summary.Providers)
	if !strings.Contains(rendered, "oauth login") {
		t.Fatalf("list should render the oauth login state, got:\n%s", rendered)
	}
}

func TestAtomicCatalogSetupRequiresLoadedModel(t *testing.T) {
	out := formatProviderCatalogLine(providerCatalogSummary{ID: "atomic-chat-local", RuntimeSupported: true})
	if strings.Contains(out, "zero providers setup atomic-chat-local --set-active") || !strings.Contains(out, "zero setup") {
		t.Fatalf("catalog advertises a failing setup command: %s", out)
	}
}
