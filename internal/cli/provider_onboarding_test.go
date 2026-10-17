package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
)

func TestRunProvidersUseSetsActiveProvider(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")
	writeProviderOnboardingConfig(t, configPath, config.FileConfig{
		ActiveProvider: "work",
		Providers: []config.ProviderProfile{
			{Name: "work", ProviderKind: config.ProviderKindOpenAI, BaseURL: config.OpenAIBaseURL, Model: "gpt-4.1"},
			{Name: "fast", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://api.groq.com/openai/v1", Model: "llama-3.3-70b-versatile"},
		},
	})

	exitCode := runWithDeps([]string{"providers", "use", "fast"}, &stdout, &stderr, providerSetupDeps(configPath))

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	cfg := readFileConfig(t, configPath)
	if cfg.ActiveProvider != "fast" {
		t.Fatalf("ActiveProvider = %q, want fast", cfg.ActiveProvider)
	}
	output := stdout.String()
	for _, want := range []string{"Active provider set to fast", "zero providers check fast"} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected providers use output to contain %q, got %q", want, output)
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
}

func TestRunProvidersUseJSONIncludesActiveProviderAndConfigPath(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	configPath := filepath.Join(t.TempDir(), "config.json")
	writeProviderOnboardingConfig(t, configPath, config.FileConfig{
		ActiveProvider: "work",
		Providers: []config.ProviderProfile{
			{Name: "work", ProviderKind: config.ProviderKindOpenAI, BaseURL: config.OpenAIBaseURL, Model: "gpt-4.1"},
			{Name: "fast", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://api.groq.com/openai/v1", Model: "llama-3.3-70b-versatile"},
		},
	})

	exitCode := runWithDeps([]string{"providers", "use", "fast", "--json"}, &stdout, &stderr, providerSetupDeps(configPath))

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	var payload struct {
		ActiveProvider string `json:"activeProvider"`
		ConfigPath     string `json:"configPath"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("providers use JSON did not decode: %v\n%s", err, stdout.String())
	}
	if payload.ActiveProvider != "fast" || payload.ConfigPath != configPath {
		t.Fatalf("unexpected providers use JSON payload: %#v", payload)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
}

func providersUseOverrideConfig(t *testing.T) string {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.json")
	writeProviderOnboardingConfig(t, configPath, config.FileConfig{
		ActiveProvider: "work",
		Providers: []config.ProviderProfile{
			{Name: "work", ProviderKind: config.ProviderKindOpenAI, BaseURL: config.OpenAIBaseURL, Model: "gpt-4.1"},
			{Name: "fast", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://api.groq.com/openai/v1", Model: "llama-3.3-70b-versatile"},
		},
	})
	return configPath
}

// The write to config.json still succeeds, but when ZERO_PROVIDER names a
// different provider the saved selection is NOT effective, so the command must
// warn instead of reporting a silent success (issue #721).
func TestRunProvidersUseWarnsWhenEnvOverrides(t *testing.T) {
	var stdout, stderr bytes.Buffer
	configPath := providersUseOverrideConfig(t)
	deps := providerSetupDeps(configPath)
	deps.getenv = func(key string) string {
		if key == config.ActiveProviderEnv {
			return "work" // ZERO_PROVIDER=work overrides the switch to fast
		}
		return ""
	}

	if code := runWithDeps([]string{"providers", "use", "fast"}, &stdout, &stderr, deps); code != exitSuccess {
		t.Fatalf("exit = %d, want %d: %s", code, exitSuccess, stderr.String())
	}
	if cfg := readFileConfig(t, configPath); cfg.ActiveProvider != "fast" {
		t.Fatalf("ActiveProvider = %q, want fast (the write still lands)", cfg.ActiveProvider)
	}
	if !strings.Contains(stdout.String(), "Active provider set to fast") {
		t.Fatalf("stdout missing the success line: %q", stdout.String())
	}
	note := stderr.String()
	for _, want := range []string{config.ActiveProviderEnv, "overrides config.json", "work"} {
		if !strings.Contains(note, want) {
			t.Fatalf("override note missing %q, got %q", want, note)
		}
	}
}

func TestRunProvidersUseJSONFlagsEnvOverride(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := providerSetupDeps(providersUseOverrideConfig(t))
	deps.getenv = func(key string) string {
		if key == config.ActiveProviderEnv {
			return "work"
		}
		return ""
	}

	if code := runWithDeps([]string{"providers", "use", "fast", "--json"}, &stdout, &stderr, deps); code != exitSuccess {
		t.Fatalf("exit = %d, want %d: %s", code, exitSuccess, stderr.String())
	}
	var payload struct {
		ActiveProvider    string `json:"activeProvider"`
		EffectiveProvider string `json:"effectiveProvider"`
		OverriddenByEnv   string `json:"overriddenByEnv"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("JSON did not decode: %v\n%s", err, stdout.String())
	}
	if payload.ActiveProvider != "fast" || payload.EffectiveProvider != "work" || payload.OverriddenByEnv != config.ActiveProviderEnv {
		t.Fatalf("JSON must flag the env override, got %#v", payload)
	}
}

// No override note when ZERO_PROVIDER is unset or already names the selection.
func TestRunProvidersUseNoWarnWithoutEnvOverride(t *testing.T) {
	cases := map[string]func(string) string{
		"env unset": func(string) string { return "" },
		"env matches fast": func(key string) string {
			if key == config.ActiveProviderEnv {
				return "fast"
			}
			return ""
		},
	}
	for name, getenv := range cases {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			deps := providerSetupDeps(providersUseOverrideConfig(t))
			deps.getenv = getenv
			if code := runWithDeps([]string{"providers", "use", "fast"}, &stdout, &stderr, deps); code != exitSuccess {
				t.Fatalf("exit = %d: %s", code, stderr.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("expected no override note, got %q", stderr.String())
			}
		})
	}
}

func TestRunProvidersUseSurfacesMalformedConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte("{"), 0o600); err != nil {
		t.Fatalf("write malformed config: %v", err)
	}

	exitCode := runWithDeps([]string{"providers", "use", "openai"}, &stdout, &stderr, providerSetupDeps(configPath))

	if exitCode != exitCrash {
		t.Fatalf("exit code = %d, want %d", exitCode, exitCrash)
	}
	if !strings.Contains(stderr.String(), "invalid config JSON") {
		t.Fatalf("stderr = %q, want malformed-config error", stderr.String())
	}
}

func TestRunProvidersUseEnvDerivedJSONIncludesConfigPath(t *testing.T) {
	var stdout, stderr bytes.Buffer
	configPath := filepath.Join(t.TempDir(), "config.json")
	writeProviderOnboardingConfig(t, configPath, config.FileConfig{})

	exitCode := runWithDeps([]string{"providers", "use", "openai", "--json"}, &stdout, &stderr, providerEnvSetupDeps(t, configPath, map[string]string{"OPENAI_API_KEY": "sk-env"}))

	if exitCode != exitSuccess {
		t.Fatalf("exit code = %d, want %d: %s", exitCode, exitSuccess, stderr.String())
	}
	var payload struct {
		ConfigPath string `json:"configPath"`
		Persisted  bool   `json:"persisted"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, stdout.String())
	}
	if payload.ConfigPath != configPath || payload.Persisted {
		t.Fatalf("unexpected payload: %+v", payload)
	}
}

func TestRunProvidersRemoveEnvDerivedJSONKeepsSchema(t *testing.T) {
	var stdout, stderr bytes.Buffer
	configPath := filepath.Join(t.TempDir(), "config.json")
	writeProviderOnboardingConfig(t, configPath, config.FileConfig{})

	exitCode := runWithDeps([]string{"providers", "remove", "openai", "--json"}, &stdout, &stderr, providerEnvSetupDeps(t, configPath, map[string]string{"OPENAI_API_KEY": "sk-env"}))

	if exitCode != exitSuccess {
		t.Fatalf("exit code = %d, want %d: %s", exitCode, exitSuccess, stderr.String())
	}
	var payload struct {
		Removed    string `json:"removed"`
		KeyRemoved bool   `json:"keyRemoved"`
		Persisted  bool   `json:"persisted"`
		ConfigPath string `json:"configPath"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, stdout.String())
	}
	if payload.Removed != "" || payload.KeyRemoved || payload.Persisted || payload.ConfigPath != configPath {
		t.Fatalf("unexpected payload: %+v", payload)
	}
}

func TestRunProvidersRenameEnvDerivedExplainsNoSavedProfile(t *testing.T) {
	var stdout, stderr bytes.Buffer
	configPath := filepath.Join(t.TempDir(), "config.json")
	writeProviderOnboardingConfig(t, configPath, config.FileConfig{})

	exitCode := runWithDeps([]string{"providers", "rename", "openai", "renamed"}, &stdout, &stderr, providerEnvSetupDeps(t, configPath, map[string]string{"OPENAI_API_KEY": "sk-env"}))

	if exitCode != exitSuccess {
		t.Fatalf("exit code = %d, want %d: %s", exitCode, exitSuccess, stderr.String())
	}
	if !strings.Contains(stdout.String(), "no saved profile to rename") {
		t.Fatalf("stdout = %q, want unpersisted explanation", stdout.String())
	}
}

func TestRunProvidersRenameEnvDerivedJSONKeepsSchema(t *testing.T) {
	var stdout, stderr bytes.Buffer
	configPath := filepath.Join(t.TempDir(), "config.json")
	writeProviderOnboardingConfig(t, configPath, config.FileConfig{})

	exitCode := runWithDeps([]string{"providers", "rename", "openai", "renamed", "--json"}, &stdout, &stderr, providerEnvSetupDeps(t, configPath, map[string]string{"OPENAI_API_KEY": "sk-env"}))

	if exitCode != exitSuccess {
		t.Fatalf("exit code = %d, want %d: %s", exitCode, exitSuccess, stderr.String())
	}
	var payload struct {
		Renamed    *struct{} `json:"renamed"`
		ConfigPath string    `json:"configPath"`
		Persisted  bool      `json:"persisted"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, stdout.String())
	}
	if payload.Renamed != nil || payload.ConfigPath != configPath || payload.Persisted {
		t.Fatalf("unexpected payload: %+v", payload)
	}
}

// Two env-derived providers and no saved active provider is the case issue
// #1130 reports: the resolver only elects an active provider by itself when
// there is exactly one, so `providers list/use/remove/rename` used to exit 3
// with `active provider "" not found` before doing anything.
func TestRunProvidersSubcommandsWithMultipleEnvDerivedProviders(t *testing.T) {
	env := map[string]string{
		"OPENAI_API_KEY": "sk-env",
		"GEMINI_API_KEY": "gem-env",
	}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "list", args: []string{"providers", "list"}, want: "openai"},
		{name: "list json", args: []string{"providers", "list", "--json"}, want: `"name": "openai"`},
		{name: "use", args: []string{"providers", "use", "openai"}, want: "is not saved in config.json"},
		{name: "remove", args: []string{"providers", "remove", "openai"}, want: "nothing to remove there"},
		{name: "rename", args: []string{"providers", "rename", "openai", "renamed"}, want: "no saved profile to rename"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.json")
			writeProviderOnboardingConfig(t, configPath, config.FileConfig{})
			var stdout, stderr bytes.Buffer

			exitCode := runWithDeps(tt.args, &stdout, &stderr, providerEnvSetupDeps(t, configPath, env))

			if exitCode != exitSuccess {
				t.Fatalf("exit code = %d, want %d: %s", exitCode, exitSuccess, stderr.String())
			}
			if !strings.Contains(stdout.String(), tt.want) {
				t.Fatalf("stdout = %q, want it to contain %q", stdout.String(), tt.want)
			}
		})
	}
}

func TestRunProvidersListShowsEveryEnvDerivedProvider(t *testing.T) {
	env := map[string]string{
		"OPENAI_API_KEY": "sk-env",
		"GEMINI_API_KEY": "gem-env",
	}
	configPath := filepath.Join(t.TempDir(), "config.json")
	writeProviderOnboardingConfig(t, configPath, config.FileConfig{})
	var stdout, stderr bytes.Buffer

	exitCode := runWithDeps([]string{"providers", "list", "--json"}, &stdout, &stderr, providerEnvSetupDeps(t, configPath, env))

	if exitCode != exitSuccess {
		t.Fatalf("exit code = %d, want %d: %s", exitCode, exitSuccess, stderr.String())
	}
	var payload struct {
		Providers []struct {
			Name string `json:"name"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, stdout.String())
	}
	names := make(map[string]bool, len(payload.Providers))
	for _, provider := range payload.Providers {
		names[provider.Name] = true
	}
	for _, want := range []string{"openai", "google"} {
		if !names[want] {
			t.Fatalf("providers = %v, want %q listed", names, want)
		}
	}
}

// The provider-management commands report what is CONFIGURED, not what a run
// would use: with env-derived providers and no saved active provider they must
// not present any provider as selected. Electing firstUsableProvider in
// resolveCommandCenterConfig made `providers current` claim `openai` while
// `zero exec` in the same environment still exited 3 with `no active provider
// configured` — and it read the OAuth store on this path (maintainer review of
// this PR).
func TestRunProvidersSubcommandsElectNoActiveProvider(t *testing.T) {
	env := map[string]string{
		"OPENAI_API_KEY": "sk-env",
		"GEMINI_API_KEY": "gem-env",
	}
	deps := func(t *testing.T) appDeps {
		configPath := filepath.Join(t.TempDir(), "config.json")
		writeProviderOnboardingConfig(t, configPath, config.FileConfig{})
		return providerEnvSetupDeps(t, configPath, env)
	}

	t.Run("current shows nothing selected", func(t *testing.T) {
		var stdout, stderr bytes.Buffer

		exitCode := runWithDeps([]string{"providers", "current"}, &stdout, &stderr, deps(t))

		if exitCode != exitSuccess {
			t.Fatalf("exit code = %d, want %d: %s", exitCode, exitSuccess, stderr.String())
		}
		if !strings.Contains(stdout.String(), "(none)") {
			t.Fatalf("stdout = %q, want an empty selection", stdout.String())
		}
		if strings.Contains(stdout.String(), "openai") {
			t.Fatalf("stdout = %q, want no provider elected as current", stdout.String())
		}
	})

	t.Run("list marks nothing active", func(t *testing.T) {
		var stdout, stderr bytes.Buffer

		exitCode := runWithDeps([]string{"providers", "list", "--json"}, &stdout, &stderr, deps(t))

		if exitCode != exitSuccess {
			t.Fatalf("exit code = %d, want %d: %s", exitCode, exitSuccess, stderr.String())
		}
		var payload struct {
			Providers []struct {
				Name   string `json:"name"`
				Active bool   `json:"active"`
			} `json:"providers"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
			t.Fatalf("decode JSON: %v\n%s", err, stdout.String())
		}
		if len(payload.Providers) < 2 {
			t.Fatalf("providers = %+v, want both env-derived profiles listed", payload.Providers)
		}
		for _, provider := range payload.Providers {
			if provider.Active {
				t.Fatalf("provider %q marked active, want nothing active", provider.Name)
			}
		}
	})

	t.Run("use json leaves activeProvider empty", func(t *testing.T) {
		var stdout, stderr bytes.Buffer

		exitCode := runWithDeps([]string{"providers", "use", "openai", "--json"}, &stdout, &stderr, deps(t))

		if exitCode != exitSuccess {
			t.Fatalf("exit code = %d, want %d: %s", exitCode, exitSuccess, stderr.String())
		}
		var payload struct {
			ActiveProvider string `json:"activeProvider"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
			t.Fatalf("decode JSON: %v\n%s", err, stdout.String())
		}
		if payload.ActiveProvider != "" {
			t.Fatalf("activeProvider = %q, want it empty for an unpersisted selection", payload.ActiveProvider)
		}
	})
}

// A base-URL-only environment (OPENAI_BASE_URL / GEMINI_BASE_URL set, no API
// keys) is the other half of issue #1130: electing firstUsableProvider made
// `providers list` exit 3 because the election demands a credential. The list
// must succeed and still mark nothing active. The resolver itself keeps only
// the keyless openai-compatible profile here (a google profile without a
// credential is dropped during normalization), so `openai` is the expected
// entry — pinning that as-is keeps this test about the COMMAND, not the
// resolver.
func TestRunProvidersListWithBaseURLOnlyProviders(t *testing.T) {
	env := map[string]string{
		"OPENAI_BASE_URL": "https://openai.proxy.example.com/v1",
		"GEMINI_BASE_URL": "https://gemini.proxy.example.com",
	}
	configPath := filepath.Join(t.TempDir(), "config.json")
	writeProviderOnboardingConfig(t, configPath, config.FileConfig{})
	var stdout, stderr bytes.Buffer

	exitCode := runWithDeps([]string{"providers", "list", "--json"}, &stdout, &stderr, providerEnvSetupDeps(t, configPath, env))

	if exitCode != exitSuccess {
		t.Fatalf("exit code = %d, want %d: %s", exitCode, exitSuccess, stderr.String())
	}
	var payload struct {
		Providers []struct {
			Name   string `json:"name"`
			Active bool   `json:"active"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, stdout.String())
	}
	names := make(map[string]bool, len(payload.Providers))
	for _, provider := range payload.Providers {
		if provider.Active {
			t.Fatalf("provider %q marked active, want nothing active", provider.Name)
		}
		names[provider.Name] = true
	}
	if !names["openai"] {
		t.Fatalf("providers = %v, want the base-URL-only openai profile listed", names)
	}
}

// `zero config` is NOT one of the provider-management commands: it reports the
// resolved configuration, so a missing active provider has to stay actionable
// (the headless contract the resolver documents for `zero config` / `zero
// exec`). Only the interactive TUI recovers from that state.
func TestRunConfigSummaryStillFailsWithNoActiveProvider(t *testing.T) {
	env := map[string]string{
		"OPENAI_API_KEY": "sk-env",
		"GEMINI_API_KEY": "gem-env",
	}
	configPath := filepath.Join(t.TempDir(), "config.json")
	writeProviderOnboardingConfig(t, configPath, config.FileConfig{})
	var stdout, stderr bytes.Buffer

	exitCode := runWithDeps([]string{"config"}, &stdout, &stderr, providerEnvSetupDeps(t, configPath, env))

	if exitCode == exitSuccess {
		t.Fatalf("exit code = %d, want the actionable resolve error: %s", exitCode, stdout.String())
	}
	message := stderr.String()
	if message == "" {
		message = stdout.String()
	}
	if !strings.Contains(message, "no active provider configured") {
		t.Fatalf("output = %q / %q, want the actionable resolve error", stdout.String(), stderr.String())
	}
}

func TestRunProvidersUseRejectsUsageErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing name", args: []string{"providers", "use"}, want: "provider name is required"},
		{name: "extra arg", args: []string{"providers", "use", "fast", "extra"}, want: `unexpected argument "extra"`},
		{name: "unknown flag", args: []string{"providers", "use", "fast", "--bogus"}, want: `unknown flag "--bogus"`},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer

			exitCode := runWithDeps(tt.args, &stdout, &stderr, providerSetupDeps(filepath.Join(t.TempDir(), "config.json")))

			if exitCode != exitUsage {
				t.Fatalf("expected exit code %d, got %d", exitUsage, exitCode)
			}
			if stdout.Len() != 0 {
				t.Fatalf("expected empty stdout, got %q", stdout.String())
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Fatalf("expected stderr to contain %q, got %q", tt.want, stderr.String())
			}
		})
	}
}

func TestRunProvidersSetupPrintsCommandPlan(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")

	exitCode := runWithDeps([]string{
		"providers", "setup", "groq",
		"--name", "fast",
		"--model", "llama-3.1-70b",
		"--base-url", "https://gateway.example/v1",
		"--api-key-env", "FAST_API_KEY",
	}, &stdout, &stderr, providerSetupDeps(configPath))

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{
		"Set FAST_API_KEY to your API key",
		"zero providers add groq --name fast --model llama-3.1-70b --base-url https://gateway.example/v1 --api-key-env FAST_API_KEY",
		"zero providers check fast --connectivity",
		"zero providers use fast",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected setup output to contain %q, got %q", want, output)
		}
	}
	if strings.Contains(output, "sk-") {
		t.Fatalf("setup output leaked a secret-looking value: %q", output)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("providers setup should not write config, stat err = %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
}

func TestRunProvidersSetupJSONIncludesCommands(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	configPath := filepath.Join(t.TempDir(), "config.json")

	exitCode := runWithDeps([]string{"providers", "setup", "groq", "--name", "fast", "--set-active", "--json"}, &stdout, &stderr, providerSetupDeps(configPath))

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	var payload struct {
		CatalogID    string `json:"catalogID"`
		Name         string `json:"name"`
		AddCommand   string `json:"addCommand"`
		CheckCommand string `json:"checkCommand"`
		UseCommand   string `json:"useCommand"`
		EnvVar       string `json:"envVar"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("providers setup JSON did not decode: %v\n%s", err, stdout.String())
	}
	if payload.CatalogID != "groq" ||
		payload.Name != "fast" ||
		payload.AddCommand != "zero providers add groq --name fast --api-key-env GROQ_API_KEY --set-active" ||
		payload.CheckCommand != "zero providers check fast --connectivity" ||
		payload.UseCommand != "" ||
		payload.EnvVar != "GROQ_API_KEY" {
		t.Fatalf("unexpected setup JSON payload: %#v", payload)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("providers setup should not write config, stat err = %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
}

func TestRunProvidersSetupRejectsCatalogOnlyTransports(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	configPath := filepath.Join(t.TempDir(), "config.json")

	exitCode := runWithDeps([]string{"providers", "setup", "bedrock"}, &stdout, &stderr, providerSetupDeps(configPath))

	if exitCode != exitUsage {
		t.Fatalf("expected exit code %d, got %d", exitUsage, exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected empty stdout, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "native adapter") {
		t.Fatalf("expected native adapter warning, got %q", stderr.String())
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("providers setup should not write config, stat err = %v", err)
	}
}

func TestRunProvidersSetupHelpListsOnboardingCommands(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"providers", "help"}, &stdout, &stderr, commandCenterDeps(t))

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"zero providers use <name>", "zero providers setup <catalog-id>"} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected providers help to contain %q, got %q", want, output)
		}
	}
}

func writeProviderOnboardingConfig(t *testing.T, path string, cfg config.FileConfig) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create config dir: %v", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("encode config: %v", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// TestRunProvidersRemoveDeletesKeyBesideConfig: the stored key must be deleted
// from the credential store CO-LOCATED with the config being edited (where
// SecureProviderProfile captured it), not the default-path store.
func TestRunProvidersRemoveDeletesKeyBesideConfig(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	seed := `{"activeProvider":"gw","providers":[{"name":"gw","provider_kind":"openai-compatible","baseURL":"https://gw.example.com/v1","apiKeyStored":true,"model":"m1"},{"name":"other","provider_kind":"openai-compatible","baseURL":"https://o.example.com/v1","model":"m2"}]}`
	if err := os.WriteFile(configPath, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	store, err := config.ProviderKeyStoreAt(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.Set("gw", "sk-secret"); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	var stdout, stderr bytes.Buffer
	deps := appDeps{userConfigPath: func() (string, error) { return configPath, nil }}
	if code := runWithDeps([]string{"providers", "remove", "gw", "--json"}, &stdout, &stderr, deps); code != exitSuccess {
		t.Fatalf("remove failed: code=%d stderr=%s", code, stderr.String())
	}

	var payload struct {
		Removed        string `json:"removed"`
		KeyRemoved     bool   `json:"keyRemoved"`
		KeyError       string `json:"keyError"`
		ActiveProvider string `json:"activeProvider"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, stdout.String())
	}
	if payload.Removed != "gw" || !payload.KeyRemoved || payload.KeyError != "" {
		t.Fatalf("unexpected payload: %+v", payload)
	}
	if payload.ActiveProvider != "other" {
		t.Fatalf("active must hand off, got %q", payload.ActiveProvider)
	}
	if _, ok, _ := store.Get("gw"); ok {
		t.Fatalf("stored key must be deleted from the store beside the config")
	}
}
