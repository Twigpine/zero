package tui

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/testutil"
)

// Guards TestMain: the per-user directories every newModel reads (and the ones
// tests could write to) must resolve inside the isolated root on every OS.
func TestUserDirsAreIsolatedFromTheDeveloperHome(t *testing.T) {
	if isolatedUserDirsRoot == "" {
		t.Fatal("TestMain did not isolate the user directories")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	userConfig, err := config.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{"home": home, "config": userConfig, "cache": cache} {
		if !testutil.PathWithin(isolatedUserDirsRoot, dir) {
			t.Errorf("%s dir %q is outside the isolated root %q", name, dir, isolatedUserDirsRoot)
		}
	}
}

// newModel loads slash commands from <user config>/zero/commands. A fresh model
// must see none, and must see exactly what is planted in the isolated dir.
func TestNewModelLoadsUserCommandsOnlyFromIsolatedConfig(t *testing.T) {
	if got := len(newModel(context.Background(), Options{Cwd: t.TempDir()}).userCommands); got != 0 {
		t.Fatalf("fresh model loaded %d user command(s) from outside the test sandbox", got)
	}

	userConfig, err := config.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(userConfig, "zero", "commands")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "hermetic-probe.md")
	if err := os.WriteFile(path, []byte("probe"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })

	commands := newModel(context.Background(), Options{Cwd: t.TempDir()}).userCommands
	if len(commands) != 1 || commands[0].Name != "hermetic-probe" {
		t.Fatalf("user commands = %#v, want only the planted hermetic-probe", commands)
	}
}
