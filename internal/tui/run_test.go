package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUseAltScreenForInteractiveChat(t *testing.T) {
	if !useAltScreen(Options{}) {
		t.Fatal("normal chat should use the alternate screen")
	}
	if !useAltScreen(Options{Setup: SetupOptions{Visible: true}}) {
		t.Fatal("setup takeover should also use the alternate screen")
	}
}

func TestTerminalPetFrameCache(t *testing.T) {
	configRoot := filepath.Join(t.TempDir(), "config-root")
	cacheRoot := filepath.Join(t.TempDir(), "cache-root")
	absConfig := filepath.Join(t.TempDir(), "custom", "config.json")
	configDir := func() (string, error) { return configRoot, nil }
	cacheDir := func() (string, error) { return cacheRoot, nil }

	tests := []struct {
		name    string
		options Options
		want    string
	}{
		{name: "absolute config", options: Options{UserConfigPath: absConfig}, want: filepath.Join(filepath.Dir(absConfig), "pets", "frame-cache")},
		{name: "relative config falls back", options: Options{UserConfigPath: "config.json"}, want: filepath.Join(configRoot, "zero", "pets", "frame-cache")},
		{name: "whitespace config falls back", options: Options{UserConfigPath: "   "}, want: filepath.Join(configRoot, "zero", "pets", "frame-cache")},
		{name: "empty config falls back", options: Options{}, want: filepath.Join(configRoot, "zero", "pets", "frame-cache")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := terminalPetFrameCacheWith(test.options, configDir, cacheDir)
			if canonicalTestPath(t, got) != canonicalTestPath(t, test.want) {
				t.Fatalf("terminalPetFrameCacheWith() = %q, want %q", got, test.want)
			}
		})
	}

	unavailable := func() (string, error) { return "", errors.New("unavailable") }
	blank := func() (string, error) { return "", nil }
	if got, want := terminalPetFrameCacheWith(Options{}, unavailable, cacheDir), filepath.Join(cacheRoot, "zero", "pets", "frame-cache"); canonicalTestPath(t, got) != canonicalTestPath(t, want) {
		t.Fatalf("cache fallback = %q, want %q", got, want)
	}
	if got, want := terminalPetFrameCacheWith(Options{}, blank, cacheDir), filepath.Join(cacheRoot, "zero", "pets", "frame-cache"); canonicalTestPath(t, got) != canonicalTestPath(t, want) {
		t.Fatalf("blank config root fallback = %q, want %q", got, want)
	}
	if got := terminalPetFrameCacheWith(Options{}, unavailable, unavailable); got != "" {
		t.Fatalf("unavailable roots returned %q, want empty", got)
	}
	if got := terminalPetFrameCacheWith(Options{}, unavailable, blank); got != "" {
		t.Fatalf("blank cache root returned %q, want empty", got)
	}
}

func canonicalTestPath(t *testing.T, value string) string {
	t.Helper()
	abs, err := filepath.Abs(value)
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return filepath.Clean(abs)
}

// TestRunRejectsNonTTYStdin pins that the interactive shell fails fast with a
// non-zero code when stdin is not a terminal, instead of blocking forever in the
// Bubble Tea event loop (e.g. `echo "" | zero`). The guard runs before any model
// construction, so empty Options are fine.
func TestRunRejectsNonTTYStdin(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	w.Close() // a pipe read-end is not a character device

	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old; r.Close() }()

	done := make(chan int, 1)
	go func() { done <- Run(context.Background(), Options{}) }()

	select {
	case code := <-done:
		if code != 2 {
			t.Fatalf("Run with non-TTY stdin returned %d; want exit code 2 from the stdin TTY guard", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run blocked on non-TTY stdin instead of failing fast")
	}
}
