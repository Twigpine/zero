package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/providerhealth"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// seedStoredProviderKey writes key into a credential store rooted beside the
// returned config path, and returns deps fields wired at that path — the same
// layout applyStoredProviderKey resolves through in production.
func seedStoredProviderKey(t *testing.T, providerName, key string) (userConfigPath string) {
	t.Helper()
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file") // never touch the real OS keychain in tests
	dir := t.TempDir()
	store, err := config.ProviderKeyStoreAt(dir)
	if err != nil {
		t.Fatalf("ProviderKeyStoreAt: %v", err)
	}
	if err := store.Set(providerName, key); err != nil {
		t.Fatalf("seed stored key: %v", err)
	}
	return filepath.Join(dir, "config.json")
}

// applyStoredProviderKeyAt must fill APIKey from the credential store for an
// apiKeyStored profile — the single central helper the newProvider wrapper uses.
func TestApplyStoredProviderKeyAtFillsFromCredstore(t *testing.T) {
	configPath := seedStoredProviderKey(t, "echo", "sk-stored-test")
	ucp := func() (string, error) { return configPath, nil }

	stored := applyStoredProviderKeyAt(config.ProviderProfile{Name: "echo", APIKeyStored: true}, ucp)
	if stored.APIKey != "sk-stored-test" {
		t.Fatalf("APIKey = %q, want the stored key", stored.APIKey)
	}

	// A profile that did NOT opt into stored-key auth must be left unchanged —
	// a stale store entry must not silently reactivate credentials.
	plain := applyStoredProviderKeyAt(config.ProviderProfile{Name: "echo"}, ucp)
	if plain.APIKey != "" {
		t.Fatalf("non-stored profile APIKey = %q, want empty", plain.APIKey)
	}
}

// fillAppDeps wraps newProvider so EVERY surface that builds a runtime provider
// (buildProvider, the ACP builder — which is just deps.newProvider — and exec's
// escalation switcher) gets the stored key applied. Passing the raw resolved
// profile previously sent unauthenticated requests for apiKeyStored profiles,
// the default onboarding outcome. Testing the wrap covers all those surfaces.
func TestFillAppDepsWrapsNewProviderWithStoredKey(t *testing.T) {
	configPath := seedStoredProviderKey(t, "echo", "sk-stored-wrap")

	var captured config.ProviderProfile
	deps := fillAppDeps(appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
		newProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			captured = profile
			return nil, nil // only the captured profile matters here
		},
	})

	if _, err := deps.newProvider(config.ProviderProfile{Name: "echo", APIKeyStored: true}); err != nil {
		t.Fatalf("wrapped newProvider: %v", err)
	}
	if captured.APIKey != "sk-stored-wrap" {
		t.Fatalf("wrapped newProvider built with APIKey = %q, want the stored key (unauthenticated regression)", captured.APIKey)
	}
}

// fillAppDeps also wraps probeProviderHealth, the code path zero providers
// check --connectivity, zero doctor --connectivity, and the TUI doctor panel
// all go through. Passing the raw resolved profile previously sent an
// unauthenticated (keyless) health probe for apiKeyStored profiles, same
// regression class as TestFillAppDepsWrapsNewProviderWithStoredKey but for
// health checks instead of the runtime provider build.
func TestFillAppDepsWrapsProbeProviderHealthWithStoredKey(t *testing.T) {
	configPath := seedStoredProviderKey(t, "echo", "sk-stored-health")

	var captured config.ProviderProfile
	deps := fillAppDeps(appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
		probeProviderHealth: func(_ context.Context, options providerhealth.Options) providerhealth.Result {
			captured = options.Profile
			return providerhealth.Result{Status: providerhealth.StatusPass}
		},
	})

	deps.probeProviderHealth(context.Background(), providerhealth.Options{
		Profile: config.ProviderProfile{Name: "echo", APIKeyStored: true},
	})
	if captured.APIKey != "sk-stored-health" {
		t.Fatalf("wrapped probeProviderHealth ran with APIKey = %q, want the stored key (keyless health-check regression)", captured.APIKey)
	}
}

// buildProvider (the TUI/exec STARTUP construction site) must export
// ZERO_PROVIDER so children spawned at any point in the run are pinned to the
// parent's provider from launch — not only after an in-session switch. Without
// this, a provider switch persisted by another zero process mid-session moves
// new children onto a different provider (and credentials) than the parent.
func TestBuildProviderExportsActiveProviderEnv(t *testing.T) {
	t.Setenv(config.ActiveProviderEnv, "stale-from-elsewhere")
	// The exporter is injected (defaultAppDeps wires config.SetActiveProviderEnv;
	// fillAppDeps deliberately leaves it nil so ordinary tests never mutate the
	// process env). Inject the real one here to assert the actual env effect.
	deps := appDeps{
		exportActiveProvider: config.SetActiveProviderEnv,
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return nil, nil
		},
	}

	resolved := config.ResolvedConfig{Provider: config.ProviderProfile{Name: "pinned", ProviderKind: config.ProviderKindAnthropic, Model: "m"}}
	if _, err := buildProvider(resolved, deps); err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	if got := os.Getenv(config.ActiveProviderEnv); got != "pinned" {
		t.Fatalf("%s = %q after startup build, want %q (children would spawn on the stale provider)", config.ActiveProviderEnv, got, "pinned")
	}

	// A FAILED build must not move the pin: the process never committed to the
	// new provider, so children must keep resolving whatever was in effect.
	t.Setenv(config.ActiveProviderEnv, "still-current")
	failing := appDeps{
		exportActiveProvider: config.SetActiveProviderEnv,
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return nil, errors.New("boom")
		},
	}
	if _, err := buildProvider(resolved, failing); err == nil {
		t.Fatal("expected build error")
	}
	if got := os.Getenv(config.ActiveProviderEnv); got != "still-current" {
		t.Fatalf("%s = %q after failed build, want it untouched", config.ActiveProviderEnv, got)
	}

	// No provider profile at all → no provider, no pin change.
	if _, err := buildProvider(config.ResolvedConfig{}, deps); err != nil {
		t.Fatalf("buildProvider(empty): %v", err)
	}
	if got := os.Getenv(config.ActiveProviderEnv); got != "still-current" {
		t.Fatalf("%s = %q after profile-less build, want it untouched", config.ActiveProviderEnv, got)
	}
}

// A mid-run --allow-escalation model switch previously rebuilt the provider
// from the PURE resolved profile (the stored key lives only in buildProvider's
// local copy), so the escalated provider was keyless and the next completion
// call 401'd mid-run. Every profile handed to newProvider on this path must
// carry the stored key.
func TestRunExecEscalationSwitchKeepsStoredKey(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	cwd := t.TempDir()

	if _, ok := mustUpgradeTarget(t, "claude-haiku-4.5"); !ok {
		t.Skip("registry has no upgrade target for claude-haiku-4.5")
	}

	configPath := seedStoredProviderKey(t, "echo", "sk-stored-escalation")

	var builtProfiles []config.ProviderProfile
	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{
		"exec",
		"--allow-escalation",
		"--model", "claude-haiku-4.5",
		"--init-session-id", "escalation_stored_key_run",
		"escalate please",
	}, &stdout, &stderr, appDeps{
		getwd:          func() (string, error) { return cwd, nil },
		userConfigPath: func() (string, error) { return configPath, nil },
		resolveConfig: func(_ string, overrides config.Overrides) (config.ResolvedConfig, error) {
			model := "claude-haiku-4.5"
			if overrides.Provider.Model != "" {
				model = overrides.Provider.Model
			}
			cfg := execResolvedConfig()
			cfg.Provider.ProviderKind = config.ProviderKindAnthropic
			cfg.Provider.Model = model
			cfg.Provider.APIKeyStored = true // key lives ONLY in the credstore
			cfg.MaxTurns = 3
			return cfg, nil
		},
		newProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			builtProfiles = append(builtProfiles, profile)
			return &usageEmittingEscalatingProvider{escalate: len(builtProfiles) == 1}, nil
		},
	})
	if exitCode != exitSuccess {
		t.Fatalf("exit = %d, want success; stderr=%s", exitCode, stderr.String())
	}
	if len(builtProfiles) < 2 {
		t.Fatalf("want at least 2 provider builds (initial + escalated), got %d", len(builtProfiles))
	}
	for i, profile := range builtProfiles {
		if profile.APIKey != "sk-stored-escalation" {
			t.Fatalf("provider build %d APIKey = %q, want the stored key (escalated build went out keyless)", i, profile.APIKey)
		}
	}
}
