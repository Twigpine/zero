package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/hooks"
)

func hooksManageDeps(cwd string) appDeps {
	return appDeps{getwd: func() (string, error) { return cwd, nil }}
}

func projectHooks(t *testing.T, cwd string) hooks.Config {
	t.Helper()
	paths, err := hooks.ResolvePaths(hooks.ResolvePathOptions{Cwd: cwd})
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	store, err := hooks.NewConfigStore(hooks.StoreOptions{ConfigPath: paths.ProjectConfigPath})
	if err != nil {
		t.Fatalf("NewConfigStore: %v", err)
	}
	config, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return config
}

func TestRunHooksAddPersistsToProjectConfig(t *testing.T) {
	cwd := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runHooksAdd([]string{"zero.preflight", "--event", "beforeTool", "--matcher", "bash", "--command", "sh", "--arg", "-c", "--arg", "echo hi"}, &stdout, &stderr, hooksManageDeps(cwd))
	if code != exitSuccess {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, exitSuccess, stderr.String())
	}
	config := projectHooks(t, cwd)
	if len(config.Hooks) != 1 {
		t.Fatalf("hooks = %d, want 1", len(config.Hooks))
	}
	hook := config.Hooks[0]
	if hook.ID != "zero.preflight" || hook.Event != hooks.EventBeforeTool || hook.Command != "sh" {
		t.Fatalf("unexpected hook: %+v", hook)
	}
	if !hook.Enabled {
		t.Fatalf("new hook should be enabled: %+v", hook)
	}
	if strings.Join(hook.Args, " ") != "-c echo hi" {
		t.Fatalf("args = %v, want [-c, echo hi]", hook.Args)
	}
}

func TestRunHooksAddRejectsMissingRequiredFlags(t *testing.T) {
	cwd := t.TempDir()
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no event", []string{"h1", "--command", "sh"}},
		{"no command", []string{"h1", "--event", "beforeTool"}},
		{"no id", []string{"--event", "beforeTool", "--command", "sh"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := runHooksAdd(tc.args, &stdout, &stderr, hooksManageDeps(cwd)); code == exitSuccess {
				t.Fatalf("expected non-success exit for %q", tc.name)
			}
			if len(projectHooks(t, cwd).Hooks) != 0 {
				t.Fatalf("no hook should be written on a usage error")
			}
		})
	}
}

func TestRunHooksAddRejectsUnknownEvent(t *testing.T) {
	cwd := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := runHooksAdd([]string{"h1", "--event", "bogusEvent", "--command", "sh"}, &stdout, &stderr, hooksManageDeps(cwd)); code == exitSuccess {
		t.Fatalf("expected non-success exit for an unknown event; stderr=%q", stderr.String())
	}
	if len(projectHooks(t, cwd).Hooks) != 0 {
		t.Fatalf("no hook should be written when the event is invalid")
	}
}

func TestRunHooksAddJSONRedactsSecretArgs(t *testing.T) {
	cwd := t.TempDir()
	secret := "sk-proj-" + strings.Repeat("z", 23) + "0"
	var stdout, stderr bytes.Buffer
	code := runHooksAdd([]string{"h1", "--event", "beforeTool", "--command", "sh", "--arg", "-c", "--arg", "echo " + secret, "--json"}, &stdout, &stderr, hooksManageDeps(cwd))
	if code != exitSuccess {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, exitSuccess, stderr.String())
	}
	if strings.Contains(stdout.String(), secret) || strings.Contains(stdout.String(), "sk-proj-") {
		t.Fatalf("JSON output must redact secret args, got %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "[REDACTED]") {
		t.Fatalf("expected a redaction marker in JSON output, got %q", stdout.String())
	}
}

func TestRunHooksToggleAndRemove(t *testing.T) {
	cwd := t.TempDir()
	deps := hooksManageDeps(cwd)
	var out, errBuf bytes.Buffer
	if code := runHooksAdd([]string{"h1", "--event", "afterTool", "--command", "sh"}, &out, &errBuf, deps); code != exitSuccess {
		t.Fatalf("add exit = %d; stderr=%q", code, errBuf.String())
	}

	// disable
	out.Reset()
	errBuf.Reset()
	if code := runHooksToggle([]string{"h1"}, &out, &errBuf, deps, true); code != exitSuccess {
		t.Fatalf("disable exit = %d; stderr=%q", code, errBuf.String())
	}
	if projectHooks(t, cwd).Hooks[0].Enabled {
		t.Fatalf("hook should be disabled after disable")
	}

	// enable
	if code := runHooksToggle([]string{"h1"}, &out, &errBuf, deps, false); code != exitSuccess {
		t.Fatalf("enable exit = %d; stderr=%q", code, errBuf.String())
	}
	if !projectHooks(t, cwd).Hooks[0].Enabled {
		t.Fatalf("hook should be enabled after enable")
	}

	// toggle of an unknown id is a usage error
	if code := runHooksToggle([]string{"missing"}, &out, &errBuf, deps, true); code == exitSuccess {
		t.Fatalf("disabling an unknown hook should fail")
	}

	// remove
	if code := runHooksRemove([]string{"h1"}, &out, &errBuf, deps); code != exitSuccess {
		t.Fatalf("remove exit = %d; stderr=%q", code, errBuf.String())
	}
	if len(projectHooks(t, cwd).Hooks) != 0 {
		t.Fatalf("hook should be gone after remove")
	}

	// removing again is a no-op success
	if code := runHooksRemove([]string{"h1"}, &out, &errBuf, deps); code != exitSuccess {
		t.Fatalf("removing a missing hook should succeed as a no-op, got %d", code)
	}
}
