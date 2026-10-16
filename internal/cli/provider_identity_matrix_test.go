package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/config"
)

// providerIdentityFixture seeds a config file plus its co-located credential
// store and hands back the config path. Every row of the matrix below starts
// from one of these so CLI mutations and runtime credential lookup see the same
// store when the injected config path is non-default.
type providerIdentityFixture struct {
	// configJSON is written verbatim: these scenarios need spellings and
	// duplicate rows that FileConfig round-tripping would not preserve.
	configJSON string
	// storedKeys are seeded into the config-adjacent credential store.
	storedKeys map[string]string
}

func seedProviderIdentityFixture(t *testing.T, fixture providerIdentityFixture) string {
	t.Helper()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(fixture.configJSON), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	if len(fixture.storedKeys) > 0 {
		store, err := config.ProviderKeyStoreAt(dir)
		if err != nil {
			t.Fatalf("open credential store: %v", err)
		}
		for provider, key := range fixture.storedKeys {
			if err := store.Set(provider, key); err != nil {
				t.Fatalf("seed key for %q: %v", provider, err)
			}
		}
	}
	return configPath
}

func storedProviderKey(t *testing.T, configPath string, provider string) (string, bool) {
	t.Helper()

	store, err := config.ProviderKeyStoreAt(filepath.Dir(configPath))
	if err != nil {
		t.Fatalf("open credential store: %v", err)
	}
	key, ok, err := store.Get(provider)
	if err != nil {
		t.Fatalf("read key for %q: %v", provider, err)
	}
	return key, ok
}

// TestProviderIdentityMatrix is the invariant test for this slice's contract:
// user input is matched by CREDENTIAL IDENTITY, persisted rows are mutated by
// EXACT SPELLING, a stored key survives only while a remaining row still claims
// it, and nothing is captured before the config it belongs to validates. Each
// row is one boundary where those rules previously disagreed; adding a row is
// how a future boundary gets covered, rather than another per-finding test.
func TestProviderIdentityMatrix(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	// The mutation-name lookup resolves source names from the process
	// environment, not deps; an exported OPENAI_API_KEY would add a concrete
	// "openai" row and correctly refuse the case-variant use below.
	clearProviderEnv(t)

	t.Run("case-variant remove targets the sole persisted row", func(t *testing.T) {
		configPath := seedProviderIdentityFixture(t, providerIdentityFixture{
			configJSON: `{"activeProvider":"WORK","providers":[{"name":"WORK","apiKeyStored":true}]}`,
			storedKeys: map[string]string{"WORK": "sk-work"},
		})
		var stdout, stderr bytes.Buffer

		if code := runWithDeps([]string{"providers", "remove", "work"}, &stdout, &stderr, providerEnvSetupDeps(t, configPath, map[string]string{})); code != exitSuccess {
			t.Fatalf("exit = %d, want %d: %s", code, exitSuccess, stderr.String())
		}
		if cfg := readFileConfig(t, configPath); len(cfg.Providers) != 0 {
			t.Fatalf("providers = %#v, want the row removed", cfg.Providers)
		}
		if _, ok := storedProviderKey(t, configPath, "work"); ok {
			t.Fatal("stored key survived removal of its only owner")
		}
	})

	t.Run("case-variant rename targets the sole persisted row", func(t *testing.T) {
		configPath := seedProviderIdentityFixture(t, providerIdentityFixture{
			configJSON: `{"activeProvider":"WORK","providers":[{"name":"WORK"}]}`,
		})
		var stdout, stderr bytes.Buffer

		if code := runWithDeps([]string{"providers", "rename", "work", "acme"}, &stdout, &stderr, providerEnvSetupDeps(t, configPath, map[string]string{})); code != exitSuccess {
			t.Fatalf("exit = %d, want %d: %s", code, exitSuccess, stderr.String())
		}
		cfg := readFileConfig(t, configPath)
		if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "acme" || cfg.ActiveProvider != "acme" {
			t.Fatalf("config = %#v, want the row and active pointer renamed to acme", cfg)
		}
	})

	t.Run("ambiguous duplicate rows are rejected before any mutation", func(t *testing.T) {
		seed := `{"activeProvider":"work","providers":[{"name":"work","apiKeyStored":true},{"name":"WORK"}]}`
		configPath := seedProviderIdentityFixture(t, providerIdentityFixture{
			configJSON: seed,
			storedKeys: map[string]string{"work": "sk-work"},
		})
		var stdout, stderr bytes.Buffer

		// "Work" matches neither row exactly and both by identity.
		if code := runWithDeps([]string{"providers", "remove", "Work"}, &stdout, &stderr, providerEnvSetupDeps(t, configPath, map[string]string{})); code == exitSuccess {
			t.Fatalf("ambiguous removal reported success: %s", stdout.String())
		}
		if !strings.Contains(stderr.String(), "ambiguous provider") {
			t.Fatalf("stderr = %q, want an ambiguity error", stderr.String())
		}
		after, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != seed {
			t.Fatalf("rejected removal rewrote config:\n%s", after)
		}
		if key, ok := storedProviderKey(t, configPath, "work"); !ok || key != "sk-work" {
			t.Fatalf("stored key does not match (present=%v, len=%d), want sk-work untouched", ok, len(key))
		}
	})

	t.Run("removing a row whose case variant still claims the key keeps it", func(t *testing.T) {
		configPath := seedProviderIdentityFixture(t, providerIdentityFixture{
			configJSON: `{"activeProvider":"work","providers":[{"name":"work","apiKeyStored":true},{"name":"WORK","apiKeyStored":true}]}`,
			storedKeys: map[string]string{"work": "sk-shared"},
		})
		var stdout, stderr bytes.Buffer

		if code := runWithDeps([]string{"providers", "remove", "work"}, &stdout, &stderr, providerEnvSetupDeps(t, configPath, map[string]string{})); code != exitSuccess {
			t.Fatalf("exit = %d, want %d: %s", code, exitSuccess, stderr.String())
		}
		cfg := readFileConfig(t, configPath)
		if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "WORK" || !cfg.Providers[0].APIKeyStored {
			t.Fatalf("config = %#v, want WORK surviving with its marker", cfg)
		}
		key, ok := storedProviderKey(t, configPath, "WORK")
		if !ok || key != "sk-shared" {
			t.Fatalf("stored key does not match (present=%v, len=%d), want the survivor's sk-shared kept", ok, len(key))
		}
		// The survivor must actually be able to load it.
		store, err := config.ProviderKeyStoreAt(filepath.Dir(configPath))
		if err != nil {
			t.Fatal(err)
		}
		if loaded := config.ApplyStoredAPIKey(cfg.Providers[0], store); strings.TrimSpace(loaded.APIKey) != "sk-shared" {
			t.Fatalf("survivor did not load the retained key (len=%d), want sk-shared", len(strings.TrimSpace(loaded.APIKey)))
		}
	})

	t.Run("removing the only row that claims the key deletes it", func(t *testing.T) {
		configPath := seedProviderIdentityFixture(t, providerIdentityFixture{
			configJSON: `{"activeProvider":"work","providers":[{"name":"work","apiKeyStored":true},{"name":"WORK"}]}`,
			storedKeys: map[string]string{"work": "sk-shared"},
		})
		var stdout, stderr bytes.Buffer

		if code := runWithDeps([]string{"providers", "remove", "work"}, &stdout, &stderr, providerEnvSetupDeps(t, configPath, map[string]string{})); code != exitSuccess {
			t.Fatalf("exit = %d, want %d: %s", code, exitSuccess, stderr.String())
		}
		// The surviving WORK row never claimed the credential, so keeping the
		// secret would only orphan it behind a marker ApplyStoredAPIKey skips.
		if _, ok := storedProviderKey(t, configPath, "WORK"); ok {
			t.Fatal("stored key was orphaned behind a markerless survivor")
		}
		if !strings.Contains(stdout.String(), "Deleted its stored API key.") {
			t.Fatalf("stdout = %q, want the key-deletion note", stdout.String())
		}
	})

	t.Run("repair removal re-points a stale activeProvider spelling", func(t *testing.T) {
		configPath := seedProviderIdentityFixture(t, providerIdentityFixture{
			configJSON: `{"activeProvider":"WoRk","providers":[{"name":"work"},{"name":"WORK"}]}`,
		})
		var stdout, stderr bytes.Buffer

		if code := runWithDeps([]string{"providers", "remove", "WORK"}, &stdout, &stderr, providerEnvSetupDeps(t, configPath, map[string]string{})); code != exitSuccess {
			t.Fatalf("exit = %d, want %d: %s", code, exitSuccess, stderr.String())
		}
		cfg := readFileConfig(t, configPath)
		if cfg.ActiveProvider != "work" {
			t.Fatalf("activeProvider = %q, want the surviving row's spelling work", cfg.ActiveProvider)
		}
		// The exact mutators must be able to find it again.
		if _, err := config.SetProviderModel(configPath, cfg.ActiveProvider, "gpt-4"); err != nil {
			t.Fatalf("exact mutator cannot address the repaired active row: %v", err)
		}
	})

	t.Run("case-variant use activates the persisted row", func(t *testing.T) {
		configPath := seedProviderIdentityFixture(t, providerIdentityFixture{
			configJSON: `{"activeProvider":"other","providers":[{"name":"other"},{"name":"OpenAI"}]}`,
		})
		var stdout, stderr bytes.Buffer

		if code := runWithDeps([]string{"providers", "use", "openai"}, &stdout, &stderr, providerEnvSetupDeps(t, configPath, map[string]string{})); code != exitSuccess {
			t.Fatalf("exit = %d, want %d: %s", code, exitSuccess, stderr.String())
		}
		if cfg := readFileConfig(t, configPath); cfg.ActiveProvider != "OpenAI" {
			t.Fatalf("activeProvider = %q, want the row's own spelling OpenAI", cfg.ActiveProvider)
		}
	})

	t.Run("unicode long-s stays a distinct identity end to end", func(t *testing.T) {
		configPath := seedProviderIdentityFixture(t, providerIdentityFixture{
			configJSON: "{\"activeProvider\":\"s\",\"providers\":[{\"name\":\"s\",\"apiKeyStored\":true},{\"name\":\"ſ\",\"apiKeyStored\":true}]}",
			storedKeys: map[string]string{"s": "sk-latin", "ſ": "sk-long"},
		})
		var stdout, stderr bytes.Buffer

		if code := runWithDeps([]string{"providers", "remove", "s"}, &stdout, &stderr, providerEnvSetupDeps(t, configPath, map[string]string{})); code != exitSuccess {
			t.Fatalf("exit = %d, want %d: %s", code, exitSuccess, stderr.String())
		}
		// strings.EqualFold folds these two together; the credential store does
		// not, so removing "s" must not reach the long-s profile or its secret.
		cfg := readFileConfig(t, configPath)
		if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "ſ" {
			t.Fatalf("config = %#v, want only the long-s row remaining", cfg)
		}
		if key, ok := storedProviderKey(t, configPath, "ſ"); !ok || key != "sk-long" {
			t.Fatalf("long-s key does not match (present=%v, len=%d), want sk-long untouched", ok, len(key))
		}
		if _, ok := storedProviderKey(t, configPath, "s"); ok {
			t.Fatal("latin-s key survived removal of its only owner")
		}
	})
}
