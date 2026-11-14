package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/hooks"
)

func TestNewHookDispatcherFailsClosedOnMalformedConfig(t *testing.T) {
	home := t.TempDir()
	configHome := filepath.Join(home, ".config")
	dataHome := filepath.Join(home, ".local", "share")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_DATA_HOME", dataHome)

	userHooks := filepath.Join(configHome, "zero", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(userHooks), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userHooks, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	dispatcher, _ := newHookDispatcher(t.TempDir(), "")
	if dispatcher == nil {
		t.Fatal("newHookDispatcher returned nil for an invalid user config")
	}
	outcome := dispatcher.Dispatch(context.Background(), hooks.DispatchInput{
		Event:    hooks.EventBeforeTool,
		ToolName: "bash",
	})
	if !outcome.Blocked {
		t.Fatalf("outcome = %#v, want blocked", outcome)
	}
	if outcome.BlockedBy != "hook-config" {
		t.Fatalf("BlockedBy = %q, want hook-config", outcome.BlockedBy)
	}
	if !strings.Contains(outcome.Reason, "hook config could not be loaded") {
		t.Fatalf("Reason = %q", outcome.Reason)
	}
}

func TestRunHooksListReportsInvalidConfigAsIncomplete(t *testing.T) {
	home := t.TempDir()
	configHome := filepath.Join(home, ".config")
	dataHome := filepath.Join(home, ".local", "share")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_DATA_HOME", dataHome)

	userHooks := filepath.Join(configHome, "zero", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(userHooks), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userHooks, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runHooks([]string{"list"}, &stdout, &stderr, appDeps{
		getwd:     func() (string, error) { return t.TempDir(), nil },
		loadHooks: hooks.LoadConfig,
	})
	if code != exitSuccess {
		t.Fatalf("runHooks exit = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Hook policy is incomplete") {
		t.Fatalf("stdout missing incomplete marker: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "unexpected end of JSON input") {
		t.Fatalf("stdout missing diagnostic: %q", stdout.String())
	}
}
