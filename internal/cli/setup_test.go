package cli

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/providercatalog"
	"github.com/Gitlawb/zero/internal/providerhealth"
	"github.com/Gitlawb/zero/internal/tui"
)

func TestSetupMissingCredentialEnv(t *testing.T) {
	tests := []struct {
		name    string
		profile config.ProviderProfile
		wantEnv string
		want    bool
	}{
		{
			name: "catalog provider",
			profile: config.ProviderProfile{
				Name:      "groq",
				CatalogID: "groq",
			},
			wantEnv: "GROQ_API_KEY",
			want:    true,
		},
		{
			name: "openai compatible without catalog",
			profile: config.ProviderProfile{
				Name:         "custom",
				ProviderKind: config.ProviderKindOpenAICompatible,
			},
			wantEnv: "OPENAI_API_KEY",
			want:    true,
		},
		{
			name: "local provider",
			profile: config.ProviderProfile{
				Name:      "local",
				CatalogID: "ollama",
			},
			want: false,
		},
		{
			name: "ollama cloud provider",
			profile: config.ProviderProfile{
				Name:      "ollama-cloud",
				CatalogID: "ollama-cloud",
			},
			wantEnv: "OLLAMA_API_KEY",
			want:    true,
		},
		{
			name: "credential resolved",
			profile: config.ProviderProfile{
				Name:         "openai",
				ProviderKind: config.ProviderKindOpenAI,
				APIKey:       "sk-test",
			},
			want: false,
		},
		{
			// Regression for issue #555: a custom OpenAI-compatible profile saved
			// with no credential (e.g. a local llama.cpp server with no auth) must
			// not be reported as missing one, or it gets filtered out of /model on
			// the next run even though it was configured exactly as intended.
			name: "custom openai compatible with no credential configured",
			profile: config.ProviderProfile{
				Name:      "local-llama",
				CatalogID: "custom-openai-compatible",
				BaseURL:   "http://192.168.1.50:8080/v1",
			},
			want: false,
		},
		{
			name: "custom openai compatible with explicit api key env",
			profile: config.ProviderProfile{
				Name:      "local-llama",
				CatalogID: "custom-openai-compatible",
				BaseURL:   "http://192.168.1.50:8080/v1",
				APIKeyEnv: "LLAMA_CPP_API_KEY",
			},
			wantEnv: "LLAMA_CPP_API_KEY",
			want:    true,
		},
		{
			// Self-heal regression: profiles saved by the pre-fix wizard were
			// stamped with the catalog's own guessed default (OPENAI_API_KEY)
			// even when the endpoint needed no auth. That stale value must be
			// treated the same as unset so already-broken saved profiles start
			// working again without a config.json rewrite.
			name: "custom openai compatible with stale legacy default env",
			profile: config.ProviderProfile{
				Name:      "local-llama",
				CatalogID: "custom-openai-compatible",
				BaseURL:   "http://192.168.1.50:8080/v1",
				APIKeyEnv: "OPENAI_API_KEY",
			},
			want: false,
		},
		{
			name: "custom anthropic compatible with no credential configured",
			profile: config.ProviderProfile{
				Name:      "local-proxy",
				CatalogID: "custom-anthropic-compatible",
				BaseURL:   "http://localhost:9000/anthropic",
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotEnv, got := setupMissingCredentialEnv(tt.profile)
			if got != tt.want || gotEnv != tt.wantEnv {
				t.Fatalf("setupMissingCredentialEnv() = (%q, %v), want (%q, %v)", gotEnv, got, tt.wantEnv, tt.want)
			}
		})
	}
}

func TestSetupProviderOptionsUseRuntimeSupportedCatalog(t *testing.T) {
	options := setupProviderOptions()
	got := make([]string, 0, len(options))
	for _, option := range options {
		got = append(got, option.ID)
	}

	want := make([]string, 0)
	for _, descriptor := range providercatalog.All() {
		if providercatalog.RuntimeSupported(descriptor) {
			want = append(want, descriptor.ID)
		}
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("setupProviderOptions IDs = %#v, want runtime-supported catalog IDs %#v", got, want)
	}
	for _, excluded := range []string{"bedrock", "vertex"} {
		for _, id := range got {
			if id == excluded {
				t.Fatalf("setupProviderOptions included unsupported provider %q in %#v", excluded, got)
			}
		}
	}
}

func TestSaveSetupProviderStoresPastedAPIKey(t *testing.T) {
	// Use the encrypted-file backend in the temp config dir so the test never
	// touches the real OS keychain.
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")

	result, err := saveSetupProvider(appDeps{
		userConfigPath: func() (string, error) {
			return configPath, nil
		},
	}, tui.SetupSelection{
		CatalogID: "ollama-cloud",
		Model:     "qwen3-coder:480b",
		APIKey:    "sk-pasted-secret",
	}, setupSaveOptions{})
	if err != nil {
		t.Fatalf("saveSetupProvider() error = %v", err)
	}

	if result.Provider.APIKey != "sk-pasted-secret" {
		t.Fatalf("Provider.APIKey = %q, want pasted key", result.Provider.APIKey)
	}
	if result.Provider.APIKeyEnv != "" {
		t.Fatalf("Provider.APIKeyEnv = %q, want empty when API key is pasted", result.Provider.APIKeyEnv)
	}

	cfg := readFileConfig(t, configPath)
	if cfg.ActiveProvider != "ollama-cloud" {
		t.Fatalf("ActiveProvider = %q, want ollama-cloud", cfg.ActiveProvider)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("Providers = %#v, want one provider", cfg.Providers)
	}
	// The capture flip: config.json must NOT hold the cleartext key — it carries the
	// APIKeyStored marker, and the secret lives in the co-located credential store.
	if cfg.Providers[0].APIKey != "" || cfg.Providers[0].APIKeyEnv != "" {
		t.Fatalf("config must not persist the key: APIKey %q APIKeyEnv %q", cfg.Providers[0].APIKey, cfg.Providers[0].APIKeyEnv)
	}
	if !cfg.Providers[0].APIKeyStored {
		t.Fatal("expected APIKeyStored marker in config")
	}
	store, err := config.ProviderKeyStoreAt(filepath.Dir(configPath))
	if err != nil {
		t.Fatal(err)
	}
	if key, ok, _ := store.Get("ollama-cloud"); !ok || key != "sk-pasted-secret" {
		t.Fatalf("stored key = %q,%v; want sk-pasted-secret in the credential store", key, ok)
	}
}

func TestSaveSetupProviderAimlapiEnvReferenceAndOverrideHeaders(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")
	result, err := saveSetupProvider(appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
	}, tui.SetupSelection{
		CatalogID: "aimlapi",
		BaseURL:   "https://staging.example/v1",
		Model:     "anthropic/claude-sonnet-5",
	}, setupSaveOptions{})
	if err != nil {
		t.Fatalf("saveSetupProvider() error = %v", err)
	}
	if result.Provider.APIKey != "" || result.Provider.APIKeyEnv != "AIMLAPI_API_KEY" {
		t.Fatalf("runtime profile did not retain AIMLAPI_API_KEY reference: %+v", result.Provider)
	}
	if len(result.Provider.CustomHeaders) != 0 {
		t.Fatalf("catalog headers leaked to first-run override: %#v", result.Provider.CustomHeaders)
	}
	persisted := readFileConfig(t, configPath).Providers[0]
	if persisted.APIKey != "" || persisted.APIKeyEnv != "AIMLAPI_API_KEY" || persisted.APIKeyStored {
		t.Fatalf("persisted credential source changed: %+v", persisted)
	}
	if len(persisted.CustomHeaders) != 0 {
		t.Fatalf("persisted override contains catalog headers: %#v", persisted.CustomHeaders)
	}
}

func TestSaveSetupProviderAimlapiUsesResolvedPartnerOverride(t *testing.T) {
	t.Setenv("AIMLAPI_PARTNER_ID", "part_override")
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")
	result, err := saveSetupProvider(appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
	}, tui.SetupSelection{
		CatalogID: "aimlapi",
		BaseURL:   "https://api.aimlapi.com/v1",
		Model:     "anthropic/claude-sonnet-5",
	}, setupSaveOptions{})
	if err != nil {
		t.Fatalf("saveSetupProvider() error = %v", err)
	}
	if got := result.Provider.CustomHeaders["X-AIMLAPI-Partner-ID"]; got != "part_override" {
		t.Fatalf("runtime partner header = %q, want part_override", got)
	}
	if got := readFileConfig(t, configPath).Providers[0].CustomHeaders["X-AIMLAPI-Partner-ID"]; got != "part_override" {
		t.Fatalf("persisted partner header = %q, want part_override", got)
	}
}

func TestSaveSetupProviderStoresCustomEndpointSelection(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")

	result, err := saveSetupProvider(appDeps{
		userConfigPath: func() (string, error) {
			return configPath, nil
		},
	}, tui.SetupSelection{
		CatalogID: "custom-openai-compatible",
		Name:      "minimax",
		BaseURL:   "https://api.minimax.io/v1",
		Model:     "MiniMax-M3",
	}, setupSaveOptions{})
	if err != nil {
		t.Fatalf("saveSetupProvider() error = %v", err)
	}

	if result.Provider.Name != "minimax" {
		t.Fatalf("Provider.Name = %q, want minimax", result.Provider.Name)
	}
	if result.Provider.BaseURL != "https://api.minimax.io/v1" {
		t.Fatalf("Provider.BaseURL = %q, want custom endpoint", result.Provider.BaseURL)
	}
	if result.Provider.Model != "MiniMax-M3" {
		t.Fatalf("Provider.Model = %q, want typed model", result.Provider.Model)
	}

	cfg := readFileConfig(t, configPath)
	if cfg.ActiveProvider != "minimax" {
		t.Fatalf("ActiveProvider = %q, want minimax", cfg.ActiveProvider)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("Providers = %#v, want one provider", cfg.Providers)
	}
	profile := cfg.Providers[0]
	if profile.Name != "minimax" || profile.CatalogID != "custom-openai-compatible" {
		t.Fatalf("stored provider identity = %#v, want minimax custom-openai-compatible", profile)
	}
	if profile.BaseURL != "https://api.minimax.io/v1" || profile.Model != "MiniMax-M3" {
		t.Fatalf("stored provider endpoint/model = %#v", profile)
	}
}

func TestSaveSetupProviderCLIOptionsOverrideCustomEndpointSelection(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")

	result, err := saveSetupProvider(appDeps{
		userConfigPath: func() (string, error) {
			return configPath, nil
		},
	}, tui.SetupSelection{
		CatalogID: "custom-openai-compatible",
		Name:      "selection-name",
		BaseURL:   "https://selection.example/v1",
		Model:     "selection-model",
	}, setupSaveOptions{
		name:    "cli-name",
		baseURL: "https://cli.example/v1",
	})
	if err != nil {
		t.Fatalf("saveSetupProvider() error = %v", err)
	}

	if result.Provider.Name != "cli-name" {
		t.Fatalf("Provider.Name = %q, want CLI name", result.Provider.Name)
	}
	if result.Provider.BaseURL != "https://cli.example/v1" {
		t.Fatalf("Provider.BaseURL = %q, want CLI endpoint", result.Provider.BaseURL)
	}
	if result.Provider.Model != "selection-model" {
		t.Fatalf("Provider.Model = %q, want selection model", result.Provider.Model)
	}

	cfg := readFileConfig(t, configPath)
	if cfg.ActiveProvider != "cli-name" {
		t.Fatalf("ActiveProvider = %q, want cli-name", cfg.ActiveProvider)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("Providers = %#v, want one provider", cfg.Providers)
	}
	profile := cfg.Providers[0]
	if profile.Name != "cli-name" || profile.BaseURL != "https://cli.example/v1" || profile.Model != "selection-model" {
		t.Fatalf("stored provider = %#v, want CLI name/baseURL and selection model", profile)
	}
}

func TestVerifySetupProviderDistinguishesMissingFromRejectedKey(t *testing.T) {
	// AUDIT-M1: verifying a remote provider with no key must say "no API key found",
	// not probe and report "the provider rejected the API key". The probe must not run.
	probed := false
	deps := appDeps{probeProviderHealth: func(context.Context, providerhealth.Options) providerhealth.Result {
		probed = true
		return providerhealth.Result{}
	}}
	_, err := verifySetupProvider(deps, config.ProviderProfile{
		Name:         "openai",
		ProviderKind: config.ProviderKindOpenAI,
		BaseURL:      "https://api.openai.com/v1",
		Model:        "gpt-4.1",
	})
	if err == nil || !strings.Contains(err.Error(), "no API key found") {
		t.Fatalf("missing key should report 'no API key found', got %v", err)
	}
	if probed {
		t.Fatal("must not probe the endpoint when no key is configured")
	}

	// A keyless LOCAL provider still probes (no key needed).
	probed = false
	_, _ = verifySetupProvider(deps, config.ProviderProfile{
		Name:         "ollama",
		ProviderKind: config.ProviderKindOpenAICompatible,
		BaseURL:      "http://localhost:11434/v1",
		Model:        "llama3",
	})
	if !probed {
		t.Fatal("a keyless local provider should still be probed")
	}
}

func TestSaveSetupProviderRejectsAtomicPlaceholderBeforeConfigAccess(t *testing.T) {
	for _, model := range []string{"", "local-model", " local-model "} {
		accessed := false
		deps := appDeps{userConfigPath: func() (string, error) { accessed = true; return filepath.Join(t.TempDir(), "config.json"), nil }}
		_, err := saveSetupProvider(deps, tui.SetupSelection{CatalogID: "atomic-chat-local", Model: model}, setupSaveOptions{})
		if err == nil || !strings.Contains(err.Error(), "--model") || accessed {
			t.Fatalf("invalid model %q reached config access: accessed=%v err=%v", model, accessed, err)
		}
	}
}
