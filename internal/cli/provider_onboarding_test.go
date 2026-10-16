package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/config"
)

func TestProviderCommandsRejectConcreteCatalogAlias(t *testing.T) {
	for _, source := range []string{"project", "environment"} {
		for _, command := range []string{"use", "remove", "rename"} {
			for _, address := range []string{"openai", "OPENAI", "work"} {
				for _, jsonOutput := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%s/json=%t", source, command, address, jsonOutput), func(t *testing.T) {
						setCLIUserConfigRoot(t)
						clearProviderEnv(t)
						t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
						path := filepath.Join(t.TempDir(), "config.json")
						writeProviderOnboardingConfig(t, path, config.FileConfig{
							ActiveProvider: "other",
							Providers: []config.ProviderProfile{
								{Name: "work", CatalogID: "openai", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://user.example/v1", Model: "user-model", APIKeyStored: true},
								{Name: "other", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://other.example/v1", Model: "other-model"},
							},
						})
						workspace := t.TempDir()
						if source == "project" {
							writeProviderOnboardingConfig(t, filepath.Join(workspace, ".zero", "config.json"), config.FileConfig{Providers: []config.ProviderProfile{
								{Name: "openai", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://project.example/v1", Model: "project-model"},
							}})
						} else {
							t.Setenv("OPENAI_API_KEY", "env-key")
							t.Setenv("OPENAI_BASE_URL", "https://env.example/v1")
						}
						resolveOptions, err := config.DefaultResolveOptions(workspace)
						if err != nil {
							t.Fatal(err)
						}
						resolveOptions.UserConfigPath = path
						names, err := config.ResolveProviderSourceNames(resolveOptions)
						if _, lookup := config.LookupProviderName(names, "openai"); err != nil || lookup != config.ProviderNameExact {
							t.Fatalf("fixture must contain a concrete openai row: names=%v err=%v", names, err)
						}
						before, err := os.ReadFile(path)
						if err != nil {
							t.Fatal(err)
						}
						store, err := config.ProviderKeyStoreAt(filepath.Dir(path))
						if err != nil {
							t.Fatal(err)
						}
						if err := store.Set("work", "user-key"); err != nil {
							t.Fatal(err)
						}
						deps := providerSetupDeps(path)
						deps.getwd = func() (string, error) { return workspace, nil }
						args := []string{"providers", command, address}
						if command == "rename" {
							args = append(args, "renamed")
						}
						if jsonOutput {
							args = append(args, "--json")
						}
						var stdout, stderr bytes.Buffer
						code := runWithDeps(args, &stdout, &stderr, deps)
						if address == "work" {
							if code != exitSuccess || stdout.Len() == 0 {
								t.Fatalf("exact saved row rejected: exit=%d stderr=%q", code, stderr.String())
							}
							cfg := readFileConfig(t, path)
							switch command {
							case "use":
								if cfg.ActiveProvider != "work" {
									t.Fatalf("activeProvider = %q, want work", cfg.ActiveProvider)
								}
							case "remove":
								if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "other" {
									t.Fatal("exact saved row was not removed")
								}
							case "rename":
								if cfg.Providers[0].Name != "renamed" {
									t.Fatal("exact saved row was not renamed")
								}
							}
							return
						}
						if code != exitCrash || stdout.Len() != 0 || !strings.Contains(stderr.String(), "isn't saved in config.json") {
							t.Errorf("concrete row should report ownership error, exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
						}
						after, err := os.ReadFile(path)
						if err != nil || !bytes.Equal(before, after) {
							t.Errorf("concrete row request changed user config: %v", err)
						}
						key, ok, err := store.Get("work")
						if err != nil || !ok || key != "user-key" {
							t.Errorf("concrete row request changed user credential: present=%t error=%v", ok, err)
						}
					})
				}
			}
		}
	}
}

func TestProviderMutationsRejectProjectCaseSibling(t *testing.T) {
	for _, command := range []string{"remove", "rename"} {
		t.Run(command, func(t *testing.T) {
			setCLIUserConfigRoot(t)
			t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
			path, err := config.DefaultUserConfigPath()
			if err != nil {
				t.Fatal(err)
			}
			user := config.ProviderProfile{Name: "work", ProviderKind: config.ProviderKindOpenAI, Model: "gpt-4o", APIKeyStored: true}
			writeProviderOnboardingConfig(t, path, config.FileConfig{ActiveProvider: "work", Providers: []config.ProviderProfile{user}})
			workspace := t.TempDir()
			project := config.ProviderProfile{Name: "WORK", ProviderKind: config.ProviderKindOpenAI, Model: "gpt-4.1"}
			writeProviderOnboardingConfig(t, filepath.Join(workspace, ".zero", "config.json"), config.FileConfig{Providers: []config.ProviderProfile{project}})
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			store, err := config.ProviderKeyStore()
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Set("work", "user-key"); err != nil {
				t.Fatal(err)
			}
			deps := providerSetupDeps(path)
			deps.getwd = func() (string, error) { return workspace, nil }
			args := []string{"providers", command, "WORK"}
			if command == "rename" {
				args = append(args, "renamed")
			}
			var stdout, stderr bytes.Buffer
			if code := runWithDeps(args, &stdout, &stderr, deps); code == exitSuccess || !strings.Contains(stderr.String(), "not the saved provider") {
				t.Fatalf("project mutation exit=%d, stderr=%q", code, stderr.String())
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("project mutation changed user config: %v", err)
			}
			key, ok, err := store.Get("work")
			if err != nil || !ok || key != "user-key" {
				t.Fatalf("project mutation changed user credential: present=%t, error=%v", ok, err)
			}
		})
	}
}

func TestRunProvidersRepairConfigPreservesLegacyMergedProfile(t *testing.T) {
	for _, reversed := range []bool{false, true} {
		t.Run(fmt.Sprint(reversed), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			rows := []config.ProviderProfile{
				{ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://legacy.example/v1", APIKey: "legacy-key"},
				{Name: "work", Model: "legacy-model"},
			}
			if reversed {
				rows[0], rows[1] = rows[1], rows[0]
			}
			writeProviderOnboardingConfig(t, path, config.FileConfig{ActiveProvider: "work", Providers: rows, MaxTurns: 17})
			var stdout, stderr bytes.Buffer
			if code := runWithDeps([]string{"providers", "repair-config"}, &stdout, &stderr, providerSetupDeps(path)); code != exitSuccess {
				t.Fatalf("repair exit=%d stderr=%q", code, stderr.String())
			}
			resolved, err := config.Resolve(config.ResolveOptions{UserConfigPath: path, Env: map[string]string{}})
			if err != nil {
				t.Fatalf("fresh Resolve after legacy upgrade repair: %v", err)
			}
			p := resolved.Provider
			if len(resolved.Providers) != 1 || p.Name != "work" || p.Model != "legacy-model" || p.BaseURL != "https://legacy.example/v1" || p.APIKey != "legacy-key" || resolved.MaxTurns != 17 {
				t.Fatal("repair changed the legacy merged provider or unrelated config")
			}
		})
	}
}

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

func TestRunProvidersRepairConfigRecoversLegacyUnnamedProvider(t *testing.T) {
	for _, jsonOutput := range []bool{false, true} {
		name := "text"
		if jsonOutput {
			name = "json"
		}
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			configPath := filepath.Join(t.TempDir(), "zero", "config.json")
			if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
				t.Fatal(err)
			}
			seed := []byte(`{"activeProvider":"legacy","providers":[{"name":"","provider_kind":"openai","model":"gpt-4o"}],"maxTurns":17}`)
			if err := os.WriteFile(configPath, seed, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := config.Resolve(config.ResolveOptions{UserConfigPath: configPath, Env: map[string]string{}}); err == nil {
				t.Fatal("legacy unnamed config unexpectedly resolved before repair")
			}
			args := []string{"providers", "repair-config"}
			if jsonOutput {
				args = append(args, "--json")
			}
			code := runWithDeps(args, &stdout, &stderr, providerSetupDeps(configPath))
			if code != exitSuccess {
				t.Fatalf("repair exit = %d, stderr=%q", code, stderr.String())
			}
			resolved, err := config.Resolve(config.ResolveOptions{UserConfigPath: configPath, Env: map[string]string{}})
			if err != nil {
				t.Fatalf("repaired config does not resolve: %v", err)
			}
			if resolved.ActiveProvider != "legacy" || resolved.Provider.Name != "legacy" || resolved.Provider.Model != "gpt-4o" || resolved.MaxTurns != 17 {
				t.Fatalf("resolved repaired config = %+v", resolved)
			}
			if jsonOutput {
				var payload map[string]any
				if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil || payload["repairedProvider"] != "legacy" {
					t.Fatalf("repair JSON = %q, err=%v", stdout.String(), err)
				}
			} else if !strings.Contains(stdout.String(), "Named legacy provider legacy") {
				t.Fatalf("repair output = %q", stdout.String())
			}
		})
	}
}

func TestRunProvidersRepairConfigMigratesLegacyActiveReference(t *testing.T) {
	var stdout, stderr bytes.Buffer
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	seed := []byte(`{"activeProvider":"legacy","providers":[{"name":"","provider_kind":"openai","model":"gpt-4o"},{"name":"other","provider_kind":"openai","model":"gpt-4.1"}]}`)
	if err := os.WriteFile(configPath, seed, 0o600); err != nil {
		t.Fatal(err)
	}

	code := runWithDeps(
		[]string{"providers", "repair-config", "--name", "work"},
		&stdout,
		&stderr,
		providerSetupDeps(configPath),
	)
	if code != exitSuccess {
		t.Fatalf("repair exit = %d, stderr=%q", code, stderr.String())
	}
	resolved, err := config.Resolve(config.ResolveOptions{UserConfigPath: configPath, Env: map[string]string{}})
	if err != nil {
		t.Fatalf("fresh Resolve after repair: %v", err)
	}
	if resolved.ActiveProvider != "work" || resolved.Provider.Name != "work" {
		t.Fatalf("resolved active provider = %q profile = %q, want work", resolved.ActiveProvider, resolved.Provider.Name)
	}
}

func TestProviderRepairCommandsCanResolveIndependentLegacyNameProblems(t *testing.T) {
	setCLIUserConfigRoot(t)
	var stdout, stderr bytes.Buffer
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")
	writeProviderOnboardingConfig(t, configPath, config.FileConfig{Providers: []config.ProviderProfile{
		{Name: ""}, {Name: "work"}, {Name: "WORK"},
	}})
	deps := providerSetupDeps(configPath)
	if code := runWithDeps([]string{"providers", "repair-config", "--name", "legacy"}, &stdout, &stderr, deps); code != exitSuccess {
		t.Fatalf("repair-config exit=%d stderr=%q", code, stderr.String())
	}
	if err := config.ValidatePersistedProviderNames(readFileConfig(t, configPath)); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("first repair should leave only the independent duplicate issue, got %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runWithDeps([]string{"providers", "remove", "WORK"}, &stdout, &stderr, deps); code != exitSuccess {
		t.Fatalf("remove exit=%d stderr=%q", code, stderr.String())
	}
	if err := config.ValidatePersistedProviderNames(readFileConfig(t, configPath)); err != nil {
		t.Fatalf("final config remains invalid: %v", err)
	}
}

func TestProviderMutationsResolvePersistedIdentity(t *testing.T) {
	tests := []struct {
		name    string
		command string
		address string
	}{
		{name: "use case variant", command: "use", address: "WORK"},
		{name: "use catalog id", command: "use", address: "acme"},
		{name: "remove case variant", command: "remove", address: "WORK"},
		{name: "remove catalog id", command: "remove", address: "acme"},
		{name: "rename case variant", command: "rename", address: "WORK"},
		{name: "rename catalog id", command: "rename", address: "acme"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.json")
			writeProviderOnboardingConfig(t, configPath, config.FileConfig{
				ActiveProvider: "other",
				Providers: []config.ProviderProfile{
					{Name: "work", CatalogID: "acme", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://work.example/v1", Model: "m1"},
					{Name: "other", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://other.example/v1", Model: "m2"},
				},
			})
			args := []string{"providers", test.command, test.address}
			if test.command == "rename" {
				args = append(args, "renamed")
			}
			var stdout, stderr bytes.Buffer
			if code := runWithDeps(args, &stdout, &stderr, providerSetupDeps(configPath)); code != exitSuccess {
				t.Fatalf("exit = %d stderr = %q", code, stderr.String())
			}
			cfg := readFileConfig(t, configPath)
			switch test.command {
			case "use":
				if cfg.ActiveProvider != "work" {
					t.Fatalf("activeProvider = %q, want canonical work", cfg.ActiveProvider)
				}
			case "remove":
				if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "other" {
					t.Fatalf("providers = %+v, want work removed", cfg.Providers)
				}
			case "rename":
				if len(cfg.Providers) != 2 || cfg.Providers[0].Name != "renamed" {
					t.Fatalf("providers = %+v, want work renamed", cfg.Providers)
				}
			}
		})
	}
}

// A legacy config can hold rows that differ only by case. Addressing such a pair
// by a third spelling identifies no single row, so a mutation must fail instead
// of picking the one that happens to come first and deleting its credential.
func TestProviderRemoveRejectsAmbiguousFoldedName(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	configPath := filepath.Join(t.TempDir(), "config.json")
	writeProviderOnboardingConfig(t, configPath, config.FileConfig{
		ActiveProvider: "work",
		Providers: []config.ProviderProfile{
			{Name: "work", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://work.example/v1", Model: "m1", APIKeyStored: true},
			{Name: "WORK", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://upper.example/v1", Model: "m2", APIKeyStored: true},
		},
	})
	configBefore, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := config.ProviderKeyStoreAt(filepath.Dir(configPath))
	if err != nil {
		t.Fatal(err)
	}
	if store.Backend() != "encrypted-file" {
		t.Fatalf("credential backend = %q, want isolated encrypted-file store", store.Backend())
	}
	if err := store.Set("work", "sk-lower"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := runWithDeps([]string{"providers", "remove", "wOrK"}, &stdout, &stderr, providerSetupDeps(configPath)); code == exitSuccess {
		t.Fatalf("providers remove wOrK succeeded, want an ambiguity failure; stdout = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "ambiguous provider name") {
		t.Fatalf("stderr = %q, want an ambiguous-provider-name error", stderr.String())
	}

	configAfter, err := os.ReadFile(configPath)
	if err != nil || !bytes.Equal(configAfter, configBefore) {
		t.Fatalf("config changed after rejected removal: err=%v\nbefore=%s\nafter=%s", err, configBefore, configAfter)
	}
	key, ok, err := store.Get("work")
	if err != nil || !ok || key != "sk-lower" {
		t.Fatalf("credential present=%v err=%v matched=%v, want preserved", ok, err, key == "sk-lower")
	}

	// The exact spelling still works, so a legacy config remains repairable.
	stdout.Reset()
	stderr.Reset()
	if code := runWithDeps([]string{"providers", "remove", "WORK"}, &stdout, &stderr, providerSetupDeps(configPath)); code != exitSuccess {
		t.Fatalf("exact-name removal exit = %d stderr = %q", code, stderr.String())
	}
	cfg := readFileConfig(t, configPath)
	if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "work" {
		t.Fatalf("providers = %+v, want only the exact row removed", cfg.Providers)
	}
	key, ok, err = store.Get("work")
	if err != nil || !ok || key != "sk-lower" {
		t.Fatalf("surviving credential present=%v err=%v matched=%v, want preserved", ok, err, key == "sk-lower")
	}
}
func TestProviderMutationsRejectAmbiguousCatalogID(t *testing.T) {
	for _, command := range []string{"use", "remove", "rename"} {
		t.Run(command, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.json")
			expected := config.FileConfig{
				ActiveProvider: "other",
				Providers: []config.ProviderProfile{
					{Name: "work", CatalogID: "acme", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://work.example/v1", Model: "m1"},
					{Name: "personal", CatalogID: "acme", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://personal.example/v1", Model: "m2"},
					{Name: "other", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://other.example/v1", Model: "m3"},
				},
			}
			writeProviderOnboardingConfig(t, configPath, expected)
			args := []string{"providers", command, "acme"}
			if command == "rename" {
				args = append(args, "renamed")
			}
			var stdout, stderr bytes.Buffer
			if code := runWithDeps(args, &stdout, &stderr, providerSetupDeps(configPath)); code == exitSuccess {
				t.Fatalf("ambiguous catalog id mutated a profile; stdout = %q", stdout.String())
			}
			cfg := readFileConfig(t, configPath)
			if !reflect.DeepEqual(cfg, expected) {
				t.Fatalf("config mutated on an ambiguous address:\ngot  %+v\nwant %+v", cfg, expected)
			}
		})
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
	t.Setenv(config.ActiveProviderEnv, "work")
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
	t.Setenv(config.ActiveProviderEnv, "work")
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
		ActiveProvider      string `json:"activeProvider"`
		EffectiveProvider   string `json:"effectiveProvider"`
		OverriddenByEnv     string `json:"overriddenByEnv"`
		EnvProvider         string `json:"envProvider"`
		EnvProviderResolves bool   `json:"envProviderResolves"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("JSON did not decode: %v\n%s", err, stdout.String())
	}
	if payload.ActiveProvider != "fast" || payload.EffectiveProvider != "work" || payload.OverriddenByEnv != config.ActiveProviderEnv || payload.EnvProvider != "work" || !payload.EnvProviderResolves {
		t.Fatalf("JSON must flag the env override, got %#v", payload)
	}
}

// effectiveProvider is the row the override resolved to, not the raw
// ZERO_PROVIDER spelling, which envProvider already reports.
func TestRunProvidersUseJSONReportsResolvedEffectiveProvider(t *testing.T) {
	t.Setenv(config.ActiveProviderEnv, "WORK")
	var stdout, stderr bytes.Buffer
	deps := providerSetupDeps(providersUseOverrideConfig(t))
	cwd := t.TempDir()
	deps.getwd = func() (string, error) { return cwd, nil }
	deps.getenv = func(key string) string {
		if key == config.ActiveProviderEnv {
			return "WORK"
		}
		return ""
	}

	if code := runWithDeps([]string{"providers", "use", "fast", "--json"}, &stdout, &stderr, deps); code != exitSuccess {
		t.Fatalf("exit = %d, want %d: %s", code, exitSuccess, stderr.String())
	}
	var payload struct {
		EffectiveProvider   string `json:"effectiveProvider"`
		EnvProvider         string `json:"envProvider"`
		EnvProviderResolves bool   `json:"envProviderResolves"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("JSON did not decode: %v\n%s", err, stdout.String())
	}
	if !payload.EnvProviderResolves || payload.EnvProvider != "WORK" || payload.EffectiveProvider != "work" {
		t.Fatalf("want envProvider WORK resolving to effectiveProvider work, got %#v", payload)
	}
}

func TestRunProvidersUseExplainsUnresolvableOverride(t *testing.T) {
	t.Setenv(config.ActiveProviderEnv, "removed")
	configPath := providersUseOverrideConfig(t)
	deps := providerSetupDeps(configPath)
	deps.getenv = os.Getenv

	var stdout, stderr bytes.Buffer
	if code := runWithDeps([]string{"providers", "use", "fast", "--json"}, &stdout, &stderr, deps); code != exitSuccess {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["envProvider"] != "removed" || payload["envProviderResolves"] != false {
		t.Fatalf("payload = %#v, want an unresolved override", payload)
	}
	if _, ok := payload["effectiveProvider"]; ok {
		t.Fatalf("unresolvable override reported effective: %#v", payload)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runWithDeps([]string{"providers", "use", "fast"}, &stdout, &stderr, deps); code != exitSuccess {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "no provider named removed can be resolved") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunProvidersUseDefersOverrideWithProviderCommand(t *testing.T) {
	t.Setenv(config.ActiveProviderEnv, "work")
	configPath := providersUseOverrideConfig(t)
	deps := providerSetupDeps(configPath)
	deps.getenv = func(key string) string {
		if key == providerCommandEnv {
			return "must-not-run"
		}
		return os.Getenv(key)
	}

	var stdout, stderr bytes.Buffer
	if code := runWithDeps([]string{"providers", "use", "fast", "--json"}, &stdout, &stderr, deps); code != exitSuccess {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["envProviderResolution"] != "deferred" || payload["envProviderResolves"] != nil {
		t.Fatalf("payload = %#v, want deferred resolution", payload)
	}
	if _, ok := payload["effectiveProvider"]; ok {
		t.Fatalf("deferred override reported effective: %#v", payload)
	}
}

func TestRunProvidersUseReportsUnrelatedConfigError(t *testing.T) {
	t.Setenv(config.ActiveProviderEnv, "work")
	configPath := providersUseOverrideConfig(t)
	workspace := t.TempDir()
	projectPath := filepath.Join(workspace, ".zero", "config.json")
	if err := os.MkdirAll(filepath.Dir(projectPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectPath, []byte(`{"tools":{"deferThreshold":-1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := providerSetupDeps(configPath)
	deps.getwd = func() (string, error) { return workspace, nil }
	deps.getenv = os.Getenv

	var stdout, stderr bytes.Buffer
	if code := runWithDeps([]string{"providers", "use", "fast"}, &stdout, &stderr, deps); code != exitSuccess {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "configuration is invalid") || !strings.Contains(stderr.String(), "deferThreshold") {
		t.Fatalf("stderr = %q, want the actual config error", stderr.String())
	}
	if strings.Contains(stderr.String(), "no provider named work") {
		t.Fatalf("unrelated config error blamed on the override: %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runWithDeps([]string{"providers", "use", "fast", "--json"}, &stdout, &stderr, deps); code != exitSuccess {
		t.Fatalf("json exit = %d, stderr = %q", code, stderr.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["envProviderResolution"] != "config-error" || payload["envProviderResolves"] != nil {
		t.Fatalf("payload = %#v, want config-error with null resolution", payload)
	}
	if message, _ := payload["envProviderResolutionError"].(string); !strings.Contains(message, "deferThreshold") {
		t.Fatalf("payload = %#v, want the unrelated configuration failure", payload)
	}
}

func TestRunProvidersUseCaseVariantOverrideDoesNotHideProjectProvider(t *testing.T) {
	t.Setenv(config.ActiveProviderEnv, "WORK")
	configPath := providersUseOverrideConfig(t)
	workspace := t.TempDir()
	projectPath := filepath.Join(workspace, ".zero", "config.json")
	writeProviderOnboardingConfig(t, projectPath, config.FileConfig{Providers: []config.ProviderProfile{{
		Name: "WORK", ProviderKind: config.ProviderKindOpenAICompatible,
		BaseURL: "https://project.example/v1", Model: "project-model",
	}}})
	deps := providerSetupDeps(configPath)
	deps.getwd = func() (string, error) { return workspace, nil }
	deps.getenv = os.Getenv

	var stdout, stderr bytes.Buffer
	if code := runWithDeps([]string{"providers", "use", "work"}, &stdout, &stderr, deps); code != exitSuccess {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "WORK stays the active provider") {
		t.Fatalf("project provider override was hidden: %q", stderr.String())
	}
}

func TestRunProvidersUseCaseVariantOverrideSelectsSameUserProvider(t *testing.T) {
	t.Setenv(config.ActiveProviderEnv, "FAST")
	configPath := providersUseOverrideConfig(t)
	deps := providerSetupDeps(configPath)
	deps.getenv = os.Getenv

	var stdout, stderr bytes.Buffer
	if code := runWithDeps([]string{"providers", "use", "fast"}, &stdout, &stderr, deps); code != exitSuccess {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("case variant resolving to the selected user row warned: %q", stderr.String())
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
	isolateCLIUserState(t)
	clearProviderEnv(t)
	t.Setenv("OPENAI_API_KEY", "sk-env")
	t.Setenv(config.ActiveProviderEnv, "openai")
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
	isolateCLIUserState(t)
	clearProviderEnv(t)
	t.Setenv("OPENAI_API_KEY", "sk-env")
	t.Setenv(config.ActiveProviderEnv, "openai")
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
	isolateCLIUserState(t)
	clearProviderEnv(t)
	t.Setenv("OPENAI_API_KEY", "sk-env")
	t.Setenv(config.ActiveProviderEnv, "openai")
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
	isolateCLIUserState(t)
	clearProviderEnv(t)
	t.Setenv("OPENAI_API_KEY", "sk-env")
	t.Setenv(config.ActiveProviderEnv, "openai")
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

// Provider credentials live beside the config being edited. An injected config
// path must not redirect deletion into the default per-user credential store.
func TestRunProvidersRemoveDeletesKeyFromConfigStore(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	setCLIUserConfigRoot(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	seed := `{"activeProvider":"gw","providers":[{"name":"gw","provider_kind":"openai-compatible","baseURL":"https://gw.example.com/v1","apiKeyStored":true,"model":"m1"},{"name":"other","provider_kind":"openai-compatible","baseURL":"https://o.example.com/v1","model":"m2"}]}`
	if err := os.WriteFile(configPath, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	defaultStore, err := config.ProviderKeyStore()
	if err != nil {
		t.Fatalf("open default store: %v", err)
	}
	if err := defaultStore.Set("gw", "ambient-secret"); err != nil {
		t.Fatalf("seed default key: %v", err)
	}
	store, err := config.ProviderKeyStoreAt(dir)
	if err != nil {
		t.Fatalf("open config store: %v", err)
	}
	if err := store.Set("gw", "config-secret"); err != nil {
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
		ActiveProvider string `json:"activeProvider"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, stdout.String())
	}
	if payload.Removed != "gw" || !payload.KeyRemoved {
		t.Fatalf("unexpected payload: %+v", payload)
	}
	// Credential cleanup is no longer reported as a field beside a success exit
	// code: a cleanup failure fails the command. Asserting the key is ABSENT
	// pins that contract, where asserting an empty "keyError" string would pass
	// merely because the field is gone.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &fields); err != nil {
		t.Fatalf("decode JSON fields: %v", err)
	}
	if _, ok := fields["keyError"]; ok {
		t.Fatalf("removal payload still reports keyError: %s", stdout.String())
	}
	if payload.ActiveProvider != "other" {
		t.Fatalf("active must hand off, got %q", payload.ActiveProvider)
	}
	if _, ok, _ := store.Get("gw"); ok {
		t.Fatalf("stored key must be deleted from the config-adjacent store")
	}
	if key, ok, getErr := defaultStore.Get("gw"); getErr != nil || !ok || key != "ambient-secret" {
		t.Fatalf("default-store key = %q, %v, %v; want it untouched", key, ok, getErr)
	}
}

func TestProviderMutationsRedactStoredKeyFailures(t *testing.T) {
	const secret = "sk-proj-1234567890abcdefghijklmnop"
	for _, command := range []string{"remove", "rename"} {
		for _, jsonOutput := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%t", command, jsonOutput), func(t *testing.T) {
				t.Setenv("ZERO_CRED_STORAGE", "file")
				dir := filepath.Join(t.TempDir(), secret)
				configPath := filepath.Join(dir, "config.json")
				writeProviderOnboardingConfig(t, configPath, config.FileConfig{
					ActiveProvider: secret,
					Providers:      []config.ProviderProfile{{Name: secret, APIKeyStored: true}},
				})
				before, err := os.ReadFile(configPath)
				if err != nil {
					t.Fatal(err)
				}
				store, err := config.ProviderKeyStoreAt(dir)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.Set(secret, "stored-key"); err != nil {
					t.Fatal(err)
				}
				// A directory at the lock-file path prevents credential writes
				// without depending on platform-specific permission behavior.
				lockPath := filepath.Join(dir, "credentials.json.lock")
				if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				if err := os.Mkdir(lockPath, 0o700); err != nil {
					t.Fatal(err)
				}

				args := []string{"providers", command, secret}
				if command == "rename" {
					args = append(args, "renamed")
				}
				if jsonOutput {
					args = append(args, "--json")
				}
				var stdout, stderr bytes.Buffer
				code := runWithDeps(args, &stdout, &stderr, providerSetupDeps(configPath))
				if code != exitCrash {
					t.Fatalf("exit = %d, want transactional failure; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				}
				// Handle-relative lock errors can omit paths entirely. Require the
				// credential failure, not a redaction marker for absent secret text.
				if !strings.Contains(stderr.String(), "credstore:") {
					t.Fatalf("stderr = %q, want a credential failure", stderr.String())
				}
				if strings.Contains(stderr.String(), secret) || stdout.Len() != 0 {
					t.Fatalf("failed mutation exposed a secret or reported success: stdout=%q stderr=%q", stdout.String(), stderr.String())
				}
				after, err := os.ReadFile(configPath)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("failed mutation changed config: %v", err)
				}

				if err := os.Remove(lockPath); err != nil {
					t.Fatal(err)
				}
				if key, ok, getErr := store.Get(secret); getErr != nil || !ok || key != "stored-key" {
					t.Fatalf("failed mutation changed key: present=%v err=%v", ok, getErr)
				}
				if _, ok, getErr := store.Get("renamed"); getErr != nil || ok {
					t.Fatalf("failed mutation left a renamed key: present=%v err=%v", ok, getErr)
				}
			})
		}
	}
}

func TestRunProvidersRemoveKeepsSharedCredentialForCaseVariantSurvivor(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	seed := []byte(`{"activeProvider":"work","providers":[{"name":"work","apiKeyStored":true},{"name":"WORK","apiKeyStored":true}]}`)
	if err := os.WriteFile(configPath, seed, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := config.ProviderKeyStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("work", "sk-shared"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	deps := appDeps{userConfigPath: func() (string, error) { return configPath, nil }}
	if code := runWithDeps([]string{"providers", "remove", "WORK", "--json"}, &stdout, &stderr, deps); code != exitSuccess {
		t.Fatalf("remove failed: code=%d stderr=%s", code, stderr.String())
	}
	cfg := readFileConfig(t, configPath)
	if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "work" || !cfg.Providers[0].APIKeyStored {
		t.Fatalf("survivor = %+v, want credentialed work row", cfg.Providers)
	}
	if key, ok, getErr := store.Get("work"); getErr != nil || !ok || key != "sk-shared" {
		t.Fatalf("shared key present=%v matched=%v err=%v; want present, matching, nil", ok, key == "sk-shared", getErr)
	}
	var payload struct {
		KeyRemoved bool `json:"keyRemoved"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.KeyRemoved {
		t.Fatal("remove reported deleting a credential still owned by the survivor")
	}
}

func TestRunProvidersRemoveReportsRetainedSharedCredential(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	seed := []byte(`{"activeProvider":"work","providers":[{"name":"work","apiKeyStored":true},{"name":"WORK","apiKeyStored":true}]}`)
	if err := os.WriteFile(configPath, seed, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := config.ProviderKeyStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("work", "sk-shared"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	deps := appDeps{userConfigPath: func() (string, error) { return configPath, nil }}
	if code := runWithDeps([]string{"providers", "remove", "WORK"}, &stdout, &stderr, deps); code != exitSuccess {
		t.Fatalf("remove failed: code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Kept its stored API key because another saved provider still uses that credential.") {
		t.Fatalf("stdout = %q, want retained-key explanation", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want no warning", stderr.String())
	}
	if key, ok, getErr := store.Get("work"); getErr != nil || !ok || key != "sk-shared" {
		t.Fatalf("shared key present=%v matched=%v err=%v; want present, matching, nil", ok, key == "sk-shared", getErr)
	}
}

func TestRunProvidersUseMatchesCredentialIdentityButNotUnicodeCaseFold(t *testing.T) {
	isolateCLIUserState(t)
	clearProviderEnv(t)
	t.Run("case variant selects persisted spelling", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "config.json")
		writeProviderOnboardingConfig(t, configPath, config.FileConfig{
			ActiveProvider: "fast",
			Providers: []config.ProviderProfile{
				{Name: "OpenAI", ProviderKind: config.ProviderKindOpenAI, Model: "gpt-4.1"},
				{Name: "fast", ProviderKind: config.ProviderKindOpenAI, Model: "gpt-4.1"},
			},
		})
		var stdout, stderr bytes.Buffer
		if code := runWithDeps([]string{"providers", "use", "openai"}, &stdout, &stderr, providerEnvSetupDeps(t, configPath, map[string]string{})); code != exitSuccess {
			t.Fatalf("use failed: code=%d stderr=%s", code, stderr.String())
		}
		if active := readFileConfig(t, configPath).ActiveProvider; active != "OpenAI" {
			t.Fatalf("active provider = %q, want persisted spelling OpenAI", active)
		}
	})

	t.Run("environment provider accepts case variant", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "config.json")
		writeProviderOnboardingConfig(t, configPath, config.FileConfig{})
		var stdout, stderr bytes.Buffer
		deps := providerEnvSetupDeps(t, configPath, map[string]string{"OPENAI_API_KEY": "sk-env", config.ActiveProviderEnv: "openai"})
		if code := runWithDeps([]string{"providers", "use", "OpenAI"}, &stdout, &stderr, deps); code != exitSuccess {
			t.Fatalf("environment use failed: code=%d stderr=%s", code, stderr.String())
		}
	})

	t.Run("long s is not plain s", func(t *testing.T) {
		t.Setenv(config.ActiveProviderEnv, "")
		t.Setenv("OPENAI_API_KEY", "")
		configPath := filepath.Join(t.TempDir(), "config.json")
		writeProviderOnboardingConfig(t, configPath, config.FileConfig{
			ActiveProvider: "s",
			Providers:      []config.ProviderProfile{{Name: "s", ProviderKind: config.ProviderKindOpenAI, Model: "gpt-4.1"}},
		})
		before, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if code := runWithDeps([]string{"providers", "use", "ſ"}, &stdout, &stderr, providerEnvSetupDeps(t, configPath, map[string]string{})); code != exitCrash {
			t.Fatalf("use exit = %d, want crash for distinct identity; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
		}
		after, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(before) {
			t.Fatal("distinct Unicode identity request rewrote config")
		}
	})
}

// Bare repair preserves the exact merge older releases performed, including
// later-row precedence, instead of proposing a duplicate or splitting the row.
func TestRunProvidersRepairConfigMergesAnOwnedActiveName(t *testing.T) {
	var stdout, stderr bytes.Buffer
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	seed := `{"activeProvider":"Groq","providers":[{"name":"","provider_kind":"openai","model":"gpt-4o"},{"name":"Groq","provider_kind":"openai","model":"llama"}]}`
	if err := os.WriteFile(configPath, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := runWithDeps([]string{"providers", "repair-config"}, &stdout, &stderr, providerSetupDeps(configPath)); code != exitSuccess {
		t.Fatalf("bare repair exit=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Named legacy provider Groq") {
		t.Fatalf("repair reported a name the row did not get: %q", stdout.String())
	}
	cfg := readFileConfig(t, configPath)
	if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "Groq" || cfg.Providers[0].Model != "llama" {
		t.Fatalf("repaired rows = %+v", cfg.Providers)
	}
	// activeProvider already selected the named row; the repair must not move it.
	if cfg.ActiveProvider != "Groq" {
		t.Fatalf("ActiveProvider = %q, want the untouched Groq pointer", cfg.ActiveProvider)
	}
	resolved, err := config.Resolve(config.ResolveOptions{UserConfigPath: configPath, Env: map[string]string{}})
	if err != nil {
		t.Fatalf("fresh Resolve after repair: %v", err)
	}
	if resolved.Provider.Name != "Groq" {
		t.Fatalf("resolved provider = %q, want Groq", resolved.Provider.Name)
	}
}

// When the fallback name is ALSO owned there is no free default left, so the
// repair must stop before mutating and say which name it wanted, who owns it,
// and the command that works. Reporting a duplicate that exists only in the
// rejected candidate state left the user with no next step.
func TestRunProvidersRepairConfigExplainsCollidingDefaultName(t *testing.T) {
	var stdout, stderr bytes.Buffer
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	seed := `{"activeProvider":"openai","providers":[{"provider_kind":"openai-compatible","baseURL":"https://legacy.example/v1","model":"legacy-model","apiKey":"legacy-key"},{"name":"OPENAI","provider_kind":"openai-compatible","baseURL":"https://other.example/v1","model":"other-model","apiKey":"other-key"}]}`
	if err := os.WriteFile(configPath, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	deps := providerSetupDeps(configPath)

	if code := runWithDeps([]string{"providers", "repair-config"}, &stdout, &stderr, deps); code == exitSuccess {
		t.Fatalf("bare repair succeeded with no free default name; stdout=%q", stdout.String())
	}
	message := stderr.String()
	if !strings.Contains(message, `"openai"`) {
		t.Fatalf("failure does not name the proposed default: %q", message)
	}
	if !strings.Contains(message, "zero providers repair-config --name") {
		t.Fatalf("failure does not show the escape command: %q", message)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("refused repair rewrote config:\nbefore=%s\nafter=%s", before, after)
	}

	// The guidance must actually work, and the result must resolve.
	stdout.Reset()
	stderr.Reset()
	if code := runWithDeps([]string{"providers", "repair-config", "--name", "legacy"}, &stdout, &stderr, deps); code != exitSuccess {
		t.Fatalf("guided repair exit=%d stderr=%q", code, stderr.String())
	}
	resolved, err := config.Resolve(config.ResolveOptions{UserConfigPath: configPath, Env: map[string]string{}})
	if err != nil {
		t.Fatalf("fresh Resolve after guided repair: %v", err)
	}
	if p := resolved.Provider; p.Name != "legacy" || p.BaseURL != "https://legacy.example/v1" || p.Model != "legacy-model" || p.APIKey != "legacy-key" {
		t.Fatal("guided repair changed the active endpoint, model or credential")
	}
	if readFileConfig(t, configPath).Providers[0].Name != "legacy" {
		t.Fatalf("guided repair did not name the legacy row: %+v", readFileConfig(t, configPath).Providers)
	}
}

func TestProviderMutationsWithoutRunnableActiveProvider(t *testing.T) {
	for _, unusable := range []bool{false, true} {
		for _, command := range []string{"remove", "rename"} {
			for _, name := range []string{"work", "WORK"} {
				t.Run(fmt.Sprintf("%t/%s/%s", unusable, command, name), func(t *testing.T) {
					setCLIUserConfigRoot(t)
					clearProviderEnv(t)
					t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
					path, err := config.DefaultUserConfigPath()
					if err != nil {
						t.Fatal(err)
					}
					rows := []config.ProviderProfile{{Name: "work", ProviderKind: config.ProviderKindOpenAI, Model: "gpt-4o"}, {Name: "other", ProviderKind: config.ProviderKindOpenAI, Model: "gpt-4.1"}}
					if unusable {
						rows = rows[:1]
						rows[0].ProviderKind = "obsolete"
					}
					writeProviderOnboardingConfig(t, path, config.FileConfig{Providers: rows})
					deps := providerSetupDeps(path)
					workspace := t.TempDir()
					deps.getwd = func() (string, error) { return workspace, nil }
					args := []string{"providers", command, name}
					if command == "rename" {
						args = append(args, "renamed")
					}
					var out, stderr bytes.Buffer
					if code := runWithDeps(args, &out, &stderr, deps); code != exitSuccess {
						t.Fatalf("exit=%d: %s", code, stderr.String())
					}
					cfg := readFileConfig(t, path)
					for _, p := range cfg.Providers {
						if p.Name == "work" {
							t.Fatal("old row remains")
						}
					}
					if command == "rename" && cfg.Providers[0].Name != "renamed" {
						t.Fatal("renamed row missing")
					}
				})
			}
		}
	}
}

func TestProviderAliasMutationRejectsUntrustedSourceErrors(t *testing.T) {
	for _, command := range []string{"remove", "rename"} {
		for _, body := range []string{`{"providers":`, `{"providers":[{"name":"work","provider_kind":"openai-compatible","baseURL":"https://untrusted.example/v1"}]}`} {
			t.Run(command+"/"+fmt.Sprint(len(body)), func(t *testing.T) {
				setCLIUserConfigRoot(t)
				clearProviderEnv(t)
				clearProviderEnv(t)
				path, err := config.DefaultUserConfigPath()
				if err != nil {
					t.Fatal(err)
				}
				writeProviderOnboardingConfig(t, path, config.FileConfig{Providers: []config.ProviderProfile{{Name: "work", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://user.example/v1", APIKey: "user-key", Model: "user-model"}}})
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				workspace := t.TempDir()
				if err := os.MkdirAll(filepath.Join(workspace, ".zero"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(workspace, ".zero", "config.json"), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
				deps := providerSetupDeps(path)
				deps.getwd = func() (string, error) { return workspace, nil }
				args := []string{"providers", command, "WORK"}
				if command == "rename" {
					args = append(args, "renamed")
				}
				var out, stderr bytes.Buffer
				if code := runWithDeps(args, &out, &stderr, deps); code == exitSuccess {
					t.Fatal("untrusted project error was ignored")
				}
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("rejection mutated the user config")
				}
			})
		}
	}
}

func TestProviderMutationsRedactPreflightErrors(t *testing.T) {
	const secret = "sk-proj-1234567890abcdefghijklmnop"
	for _, command := range []string{"remove", "rename"} {
		for _, stage := range []string{"usage", "config path", "config read", "ambiguous identity"} {
			t.Run(command+"/"+stage, func(t *testing.T) {
				configPath := filepath.Join(t.TempDir(), secret, "config.json")
				deps := providerSetupDeps(configPath)
				args := []string{"providers", command, secret}
				if command == "rename" {
					args = append(args, "renamed")
				}
				var reason string
				switch stage {
				case "usage":
					args = append(args, "--api-key="+secret)
					reason = "unknown flag"
				case "config path":
					deps.userConfigPath = func() (string, error) {
						return "", fmt.Errorf("locate config %s: %w", configPath, os.ErrPermission)
					}
					reason = "permission denied"
				case "config read":
					if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(configPath, []byte(`{"providers":`), 0o600); err != nil {
						t.Fatal(err)
					}
					reason = "invalid config JSON"
				case "ambiguous identity":
					writeProviderOnboardingConfig(t, configPath, config.FileConfig{
						Providers: []config.ProviderProfile{
							{Name: secret},
							{Name: secret},
						},
					})
					reason = "ambiguous provider name"
				}
				var stdout, stderr bytes.Buffer
				if code := runWithDeps(args, &stdout, &stderr, deps); code == exitSuccess {
					t.Fatalf("mutation succeeded despite %s failure", stage)
				}
				if !strings.Contains(stderr.String(), reason) || !strings.Contains(stderr.String(), "[REDACTED]") {
					t.Fatalf("stderr = %q, want a redacted error retaining %q", stderr.String(), reason)
				}
				if strings.Contains(stderr.String(), secret) || stdout.Len() != 0 {
					t.Fatalf("failed mutation exposed a secret or reported success: stdout=%q stderr=%q", stdout.String(), stderr.String())
				}
			})
		}
	}
}
