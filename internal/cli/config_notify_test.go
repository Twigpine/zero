package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/config"
)

// `zero config notify` with no flag and a fresh config: nothing is configured,
// the resolver leaves the fields blank (defaults deliberately live in the TUI,
// not the resolver — maintainer review, PR #1001), and the command reports
// "(default)" for both.
func TestRunConfigNotifyPrintsUnconfiguredAsDefault(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	// A valid openai profile so the resolver does not error with
	// ErrNoActiveProvider. The notify defaults are applied independently of
	// the provider resolution path.
	seed := `{
		"activeProvider": "openai",
		"providers": [{
			"name": "openai",
			"providerKind": "openai",
			"baseUrl": "https://api.openai.com/v1",
			"model": "gpt-4.1",
			"apiKeyEnv": "OPENAI_API_KEY"
		}]
	}`
	if err := os.WriteFile(configPath, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	deps := commandCenterDeps(t)
	deps.userConfigPath = func() (string, error) { return configPath, nil }
	deps.resolveConfig = func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
		return config.Resolve(config.ResolveOptions{UserConfigPath: configPath, Env: map[string]string{"OPENAI_API_KEY": "sk-test"}})
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"config", "notify"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("exit = %d, want %d: %s", exitCode, exitSuccess, stderr.String())
	}
	if !strings.Contains(stdout.String(), "mode:      (default)") {
		t.Errorf("stdout should show unconfigured mode as (default), got: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "focusMode: (default)") {
		t.Errorf("stdout should show unconfigured focus as (default), got: %s", stdout.String())
	}
}

// `zero config notify --json` emits a machine-readable payload. A configured
// pair round-trips; unconfigured fields are empty strings (never defaults
// filled in), so scripts can distinguish "user chose" from "default".
func TestRunConfigNotifyPrintsJSON(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	seed := `{
		"activeProvider": "openai",
		"providers": [{
			"name": "openai",
			"providerKind": "openai",
			"baseUrl": "https://api.openai.com/v1",
			"model": "gpt-4.1",
			"apiKeyEnv": "OPENAI_API_KEY"
		}],
		"notify": {"mode": "bell", "focusMode": "always"}
	}`
	if err := os.WriteFile(configPath, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	deps := commandCenterDeps(t)
	deps.userConfigPath = func() (string, error) { return configPath, nil }
	deps.resolveConfig = func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
		return config.Resolve(config.ResolveOptions{UserConfigPath: configPath, Env: map[string]string{"OPENAI_API_KEY": "sk-test"}})
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"config", "notify", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("exit = %d, want %d: %s", exitCode, exitSuccess, stderr.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, stdout.String())
	}
	if payload["mode"] != "bell" {
		t.Errorf("mode = %v, want bell", payload["mode"])
	}
	if payload["focusMode"] != "always" {
		t.Errorf("focusMode = %v, want always", payload["focusMode"])
	}
}

// The unconfigured JSON shape: fields are empty strings, never defaults
// filled in, so scripts can tell "user chose" from "default".
func TestRunConfigNotifyPrintsJSONUnconfigured(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	seed := `{
		"activeProvider": "openai",
		"providers": [{
			"name": "openai",
			"providerKind": "openai",
			"baseUrl": "https://api.openai.com/v1",
			"model": "gpt-4.1",
			"apiKeyEnv": "OPENAI_API_KEY"
		}]
	}`
	if err := os.WriteFile(configPath, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	deps := commandCenterDeps(t)
	deps.userConfigPath = func() (string, error) { return configPath, nil }
	deps.resolveConfig = func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
		return config.Resolve(config.ResolveOptions{UserConfigPath: configPath, Env: map[string]string{"OPENAI_API_KEY": "sk-test"}})
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"config", "notify", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("exit = %d, want %d: %s", exitCode, exitSuccess, stderr.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, stdout.String())
	}
	if payload["mode"] != "" {
		t.Errorf("mode = %v, want empty (unconfigured)", payload["mode"])
	}
	if payload["focusMode"] != "" {
		t.Errorf("focusMode = %v, want empty (unconfigured)", payload["focusMode"])
	}
}

// `zero config notify --mode off` writes the new value to disk and prints
// confirmation. The user can read it back by running the command again.
func TestRunConfigNotifyWritesModeChange(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	seed := `{
		"activeProvider": "openai",
		"providers": [{
			"name": "openai",
			"providerKind": "openai",
			"baseUrl": "https://api.openai.com/v1",
			"model": "gpt-4.1",
			"apiKeyEnv": "OPENAI_API_KEY"
		}],
		"notify": {"mode": "both", "focusMode": "always"}
	}`
	if err := os.WriteFile(configPath, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	deps := commandCenterDeps(t)
	deps.userConfigPath = func() (string, error) { return configPath, nil }
	deps.resolveConfig = func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
		return config.Resolve(config.ResolveOptions{UserConfigPath: configPath, Env: map[string]string{"OPENAI_API_KEY": "sk-test"}})
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"config", "notify", "--mode", "off"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("exit = %d, want %d: %s", exitCode, exitSuccess, stderr.String())
	}
	cfg := readFileConfig(t, configPath)
	if cfg.Notify.Mode != "off" {
		t.Errorf("Notify.Mode = %q, want off", cfg.Notify.Mode)
	}
	// A mode-only update must preserve the configured focusMode, not wipe it.
	if cfg.Notify.FocusMode != "always" {
		t.Errorf("Notify.FocusMode = %q, want preserved %q", cfg.Notify.FocusMode, "always")
	}
	if !strings.Contains(stdout.String(), "mode:      off") {
		t.Errorf("stdout should confirm the change, got: %s", stdout.String())
	}
}

// A focus-only update preserves the configured mode.
func TestRunConfigNotifyFocusOnlyPreservesMode(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	seed := `{
		"activeProvider": "openai",
		"providers": [{
			"name": "openai",
			"providerKind": "openai",
			"baseUrl": "https://api.openai.com/v1",
			"model": "gpt-4.1",
			"apiKeyEnv": "OPENAI_API_KEY"
		}],
		"notify": {"mode": "bell", "focusMode": "unfocused"}
	}`
	if err := os.WriteFile(configPath, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	deps := commandCenterDeps(t)
	deps.userConfigPath = func() (string, error) { return configPath, nil }
	deps.resolveConfig = func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
		return config.Resolve(config.ResolveOptions{UserConfigPath: configPath, Env: map[string]string{"OPENAI_API_KEY": "sk-test"}})
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"config", "notify", "--focus", "always"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("exit = %d, want %d: %s", exitCode, exitSuccess, stderr.String())
	}
	cfg := readFileConfig(t, configPath)
	if cfg.Notify.Mode != "bell" {
		t.Errorf("Notify.Mode = %q, want preserved %q", cfg.Notify.Mode, "bell")
	}
	if cfg.Notify.FocusMode != "always" {
		t.Errorf("Notify.FocusMode = %q, want always", cfg.Notify.FocusMode)
	}
}

// Maintainer regression (PR #1001): a partial update must seed from the USER'S
// OWN file, never from the resolved view. A project .zero/config.json setting
// mode=off resolves into the session, but `--focus always` inside that repo
// must NOT copy the project's off into the user's global config.
func TestRunConfigNotifyDoesNotCopyProjectNotifyIntoUserConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "user.json")
	projectPath := filepath.Join(t.TempDir(), "project.json")
	if err := os.WriteFile(configPath, []byte(`{"activeProvider": "openai"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectPath, []byte(`{"notify": {"mode": "off"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := commandCenterDeps(t)
	deps.userConfigPath = func() (string, error) { return configPath, nil }
	// Resolve EXACTLY the way production does: user config + project config
	// merged, so resolved.Notify.Mode is the project's "off".
	deps.resolveConfig = func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
		return config.Resolve(config.ResolveOptions{
			UserConfigPath:    configPath,
			ProjectConfigPath: projectPath,
			Env:               map[string]string{"OPENAI_API_KEY": "sk-test"},
		})
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"config", "notify", "--focus", "always"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("exit = %d, want %d: %s", exitCode, exitSuccess, stderr.String())
	}
	cfg := readFileConfig(t, configPath)
	// The user's file must keep an unspecified mode unspecified — the
	// project's "off" stays in the project file where it belongs.
	if cfg.Notify.Mode != "" {
		t.Errorf("user Notify.Mode = %q, want blank (project's off must not leak into the user file)", cfg.Notify.Mode)
	}
	if cfg.Notify.FocusMode != "always" {
		t.Errorf("Notify.FocusMode = %q, want always", cfg.Notify.FocusMode)
	}
	// The project file is untouched.
	project := readFileConfig(t, projectPath)
	if project.Notify.Mode != "off" {
		t.Errorf("project Notify.Mode = %q, want untouched off", project.Notify.Mode)
	}
}

// Maintainer regression (PR #1001): with a clean config, `--mode off` must not
// also pin focusMode as an explicit choice — blank means "use the built-in
// default", and a partial update keeps it that way.
func TestRunConfigNotifyDoesNotPinDefaultsAsExplicitChoices(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"activeProvider": "openai"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := commandCenterDeps(t)
	deps.userConfigPath = func() (string, error) { return configPath, nil }
	deps.resolveConfig = func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
		return config.Resolve(config.ResolveOptions{UserConfigPath: configPath, Env: map[string]string{"OPENAI_API_KEY": "sk-test"}})
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"config", "notify", "--mode", "off"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("exit = %d, want %d: %s", exitCode, exitSuccess, stderr.String())
	}
	cfg := readFileConfig(t, configPath)
	if cfg.Notify.Mode != "off" {
		t.Errorf("Notify.Mode = %q, want off", cfg.Notify.Mode)
	}
	if cfg.Notify.FocusMode != "" {
		t.Errorf("Notify.FocusMode = %q, want blank (unspecified stays unspecified; the default must not be pinned)", cfg.Notify.FocusMode)
	}
}

// `--mode` and `--focus` together update both fields in one call.
func TestRunConfigNotifyWritesModeAndFocus(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	seed := `{
		"activeProvider": "openai",
		"providers": [{
			"name": "openai",
			"providerKind": "openai",
			"baseUrl": "https://api.openai.com/v1",
			"model": "gpt-4.1",
			"apiKeyEnv": "OPENAI_API_KEY"
		}]
	}`
	if err := os.WriteFile(configPath, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	deps := commandCenterDeps(t)
	deps.userConfigPath = func() (string, error) { return configPath, nil }
	deps.resolveConfig = func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
		return config.Resolve(config.ResolveOptions{UserConfigPath: configPath, Env: map[string]string{"OPENAI_API_KEY": "sk-test"}})
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"config", "notify", "--mode", "both", "--focus", "unfocused"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("exit = %d, want %d: %s", exitCode, exitSuccess, stderr.String())
	}
	cfg := readFileConfig(t, configPath)
	if cfg.Notify.Mode != "both" || cfg.Notify.FocusMode != "unfocused" {
		t.Errorf("Notify = %+v, want mode=both focusMode=unfocused", cfg.Notify)
	}
}

// `--mode loud` is a usage error. The config must not be mutated.
func TestRunConfigNotifyRejectsInvalidMode(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	seed := `{
		"activeProvider": "openai",
		"providers": [{
			"name": "openai",
			"providerKind": "openai",
			"baseUrl": "https://api.openai.com/v1",
			"model": "gpt-4.1",
			"apiKeyEnv": "OPENAI_API_KEY"
		}],
		"notify": {"mode": "off"}
	}`
	if err := os.WriteFile(configPath, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	deps := commandCenterDeps(t)
	deps.userConfigPath = func() (string, error) { return configPath, nil }
	deps.resolveConfig = func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
		return config.Resolve(config.ResolveOptions{UserConfigPath: configPath, Env: map[string]string{"OPENAI_API_KEY": "sk-test"}})
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"config", "notify", "--mode", "loud"}, &stdout, &stderr, deps)
	if exitCode == exitSuccess {
		t.Fatalf("expected failure for invalid mode, got success; stdout=%s", stdout.String())
	}
	cfg := readFileConfig(t, configPath)
	if cfg.Notify.Mode != "off" {
		t.Errorf("Notify.Mode = %q, want off (unchanged after failed write)", cfg.Notify.Mode)
	}
}

// `--reset` blanks both fields so the TUI's effective default applies again
// (an unconfigured headless run stays silent). Useful for "go back to the
// recommended setup" after a custom value.
func TestRunConfigNotifyResetClearsStoredValues(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	seed := `{
		"activeProvider": "openai",
		"providers": [{
			"name": "openai",
			"providerKind": "openai",
			"baseUrl": "https://api.openai.com/v1",
			"model": "gpt-4.1",
			"apiKeyEnv": "OPENAI_API_KEY"
		}],
		"notify": {"mode": "off", "focusMode": "always"}
	}`
	if err := os.WriteFile(configPath, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	deps := commandCenterDeps(t)
	deps.userConfigPath = func() (string, error) { return configPath, nil }
	deps.resolveConfig = func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
		return config.Resolve(config.ResolveOptions{UserConfigPath: configPath, Env: map[string]string{"OPENAI_API_KEY": "sk-test"}})
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"config", "notify", "--reset"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("exit = %d, want %d: %s", exitCode, exitSuccess, stderr.String())
	}
	cfg := readFileConfig(t, configPath)
	if cfg.Notify.Mode != "" || cfg.Notify.FocusMode != "" {
		t.Errorf("Notify after reset = %+v, want empty (defaults apply)", cfg.Notify)
	}
}

// CodeRabbit regression (PR #1001): the command manages a user preference and
// must not require provider resolution. A brand-new user with NO provider
// configured (the resolver would return ErrNoActiveProvider) can still read,
// set, and reset their notification preference.
func TestRunConfigNotifyWorksWithoutAnyProviderConfigured(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// resolveConfig fails the way the real resolver does for a fresh user —
	// the command must never call it, so a panic-free stub is enough to prove
	// the point; use the failing resolver to catch any regression to the
	// resolve-first shape.
	deps := commandCenterDeps(t)
	deps.userConfigPath = func() (string, error) { return configPath, nil }
	deps.resolveConfig = func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
		return config.Resolve(config.ResolveOptions{UserConfigPath: configPath, Env: map[string]string{}})
	}

	// Read works and shows defaults.
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"config", "notify"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("read: exit = %d, want %d: %s", exitCode, exitSuccess, stderr.String())
	}
	if !strings.Contains(stdout.String(), "mode:      (default)") {
		t.Errorf("read should show (default), got: %s", stdout.String())
	}

	// Write works.
	stdout.Reset()
	exitCode = runWithDeps([]string{"config", "notify", "--mode", "bell", "--focus", "always"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("write: exit = %d, want %d: %s", exitCode, exitSuccess, stderr.String())
	}
	cfg := readFileConfig(t, configPath)
	if cfg.Notify.Mode != "bell" || cfg.Notify.FocusMode != "always" {
		t.Fatalf("write: Notify = %+v, want bell/always", cfg.Notify)
	}

	// JSON read reflects the stored pair.
	stdout.Reset()
	exitCode = runWithDeps([]string{"config", "notify", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("json: exit = %d, want %d: %s", exitCode, exitSuccess, stderr.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("json output invalid: %v\n%s", err, stdout.String())
	}
	if payload["mode"] != "bell" || payload["focusMode"] != "always" {
		t.Errorf("json = mode %v focus %v, want bell/always", payload["mode"], payload["focusMode"])
	}

	// Reset works.
	stdout.Reset()
	exitCode = runWithDeps([]string{"config", "notify", "--reset"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("reset: exit = %d, want %d: %s", exitCode, exitSuccess, stderr.String())
	}
	cfg = readFileConfig(t, configPath)
	if cfg.Notify.Mode != "" || cfg.Notify.FocusMode != "" {
		t.Errorf("reset: Notify = %+v, want empty", cfg.Notify)
	}
}

// `zero config` (no subcommand) still works after the dispatch change.
func TestRunConfigSummaryStillWorks(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"config"}, &stdout, &stderr, commandCenterDeps(t))
	if exitCode != exitSuccess {
		t.Fatalf("exit = %d, want %d: %s", exitCode, exitSuccess, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Config") {
		t.Errorf("stdout should show the config summary, got: %s", stdout.String())
	}
}
