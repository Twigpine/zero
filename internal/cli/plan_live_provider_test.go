package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
)

// pointConfigHomeAtATempDir redirects config.UserConfigDir at a scratch directory
// and returns it.
//
// The variable that does the redirecting IS NOT THE SAME ON EVERY PLATFORM:
// os.UserConfigDir reads %AppData% on Windows and $XDG_CONFIG_HOME elsewhere, and
// config.UserConfigDir lets XDG win on macOS too. Setting only XDG_CONFIG_HOME
// left Windows resolving against the real %AppData%, where this file was never
// written — so every case below silently exercised the resolve-failed fallback
// and the switch-following test could not fail on Windows for the right reason.
// Mirrors internal/config/paths_test.go's setUserConfigRoot.
func pointConfigHomeAtATempDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	switch runtime.GOOS {
	case "windows":
		t.Setenv("APPDATA", root)
	default:
		t.Setenv("XDG_CONFIG_HOME", root)
	}
	base, err := config.UserConfigDir()
	if err != nil {
		t.Fatalf("resolve user config dir: %v", err)
	}
	// The redirect must have LANDED. Without this, setting the wrong variable
	// for the platform resolves back to the developer's real config directory:
	// the helper writes a provider file into it and the tests pass for the
	// wrong reason — which is exactly how this went unnoticed on Windows.
	if !strings.HasPrefix(base, root) {
		t.Fatalf("config home resolved to %q, outside the temp root %q: this test would read and write the real user config", base, root)
	}
	return base
}

func writeTwoProviderConfig(t *testing.T, active string) {
	t.Helper()
	dir := filepath.Join(pointConfigHomeAtATempDir(t), "zero")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The provider shape a real config uses — provider_kind + apiFormat matter:
	// without them "xai" is read as an OpenAI profile and refused for not using
	// the official base URL.
	body := `{
	  "activeProvider": "` + active + `",
	  "providers": [
	    {"name": "ollama-cloud", "provider_kind": "openai-compatible", "catalogID": "ollama-cloud",
	     "baseURL": "https://ollama.com/v1", "apiFormat": "chat-completions",
	     "model": "glm-5.2", "apiKey": "k1"},
	    {"name": "xai", "provider_kind": "openai-compatible", "catalogID": "xai",
	     "baseURL": "https://api.x.ai/v1", "apiFormat": "chat-completions",
	     "model": "grok-4.5", "apiKey": "k2"}
	  ]
	}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// DISCOVERY MUST FOLLOW AN IN-SESSION PROVIDER SWITCH, because child execution
// already does and the two must never describe different providers.
//
// The orchestrate tool is wired once at startup. The TUI's /model picker can
// switch provider afterwards; switchProviderModel re-points the parent client and
// exports ZERO_PROVIDER so spawned children follow. Before this, nothing
// re-pointed model discovery — a session on xai/grok-4.5 had its plan assigned
// from a nineteen-model Ollama list and the provider rejected every task.
func TestPlanDiscoveryFollowsAnInSessionProviderSwitch(t *testing.T) {
	writeTwoProviderConfig(t, "ollama-cloud")
	captured := config.ProviderProfile{Name: "ollama-cloud", BaseURL: "https://ollama.com/v1", Model: "glm-5.2"}

	// What switchProviderModel does when the user picks a model from another
	// saved provider.
	t.Setenv(config.ActiveProviderEnv, "xai")

	got := livePlanProvider(t.TempDir(), captured)
	if got.Name != "xai" {
		t.Fatalf("discovery stayed on the launch-time provider %q after a switch to xai", got.Name)
	}
	if got.BaseURL != "https://api.x.ai/v1" {
		t.Errorf("the switched profile carries the wrong endpoint: %q", got.BaseURL)
	}
}

// THE CAPTURED PROFILE WINS WHEN NOTHING SWITCHED, and this is what keeps
// headless runs correct. --provider, --base-url and an inline --api-key are
// applied by the caller on top of Resolve, so re-resolving would silently discard
// them and probe the wrong endpoint with the wrong key.
func TestPlanDiscoveryKeepsFlagOverridesWhenTheProviderDidNotChange(t *testing.T) {
	writeTwoProviderConfig(t, "xai")
	// A profile that exists nowhere in the config file: exactly what a
	// --base-url / --api-key override produces.
	captured := config.ProviderProfile{
		Name:    "xai",
		BaseURL: "https://gateway.internal/v1",
		Model:   "grok-4.5",
		APIKey:  "flag-only-key",
	}
	// Empty reads as unset (applyEnv TrimSpaces it) and t.Setenv restores the
	// real value afterwards — os.Unsetenv here would leak into sibling tests.
	t.Setenv(config.ActiveProviderEnv, "")

	got := livePlanProvider(t.TempDir(), captured)
	if got.BaseURL != "https://gateway.internal/v1" || got.APIKey != "flag-only-key" {
		t.Fatalf("re-resolution discarded the caller's overrides: %+v", got)
	}
}

// An unreadable or absent config must not blank the profile discovery probes
// with. Falling back to the captured one leaves behaviour exactly as it was.
func TestPlanDiscoveryFallsBackToTheCapturedProfileWhenResolutionFails(t *testing.T) {
	// A config home with no zero/config.json in it: resolution finds nothing.
	pointConfigHomeAtATempDir(t)
	t.Setenv(config.ActiveProviderEnv, "")
	captured := config.ProviderProfile{Name: "xai", BaseURL: "https://api.x.ai/v1", Model: "grok-4.5"}

	if got := livePlanProvider(t.TempDir(), captured); got.Name != "xai" || got.BaseURL != captured.BaseURL {
		t.Fatalf("a failed resolve must leave the captured profile intact, got %+v", got)
	}
}
