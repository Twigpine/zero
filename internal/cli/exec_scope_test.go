package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/sandbox"
	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// TestRunAddDirDispatchForwardsGrantIntoExecScope pins the dispatch seam
// end-to-end: a leading --add-dir on the ROOT argv (both the "exec" and the
// "-p" dispatch shapes in runWithDeps) must reach exec's sandbox scope so a
// write_file call inside the extra root succeeds. The negative control runs
// the identical provider WITHOUT --add-dir and must be denied (fail-closed).
// If addDirFlagArgs forwarding were dropped from either dispatch case, the
// positive runs here would fail while every parser/registry unit test stayed
// green.
func TestRunAddDirDispatchForwardsGrantIntoExecScope(t *testing.T) {
	for _, tc := range []struct {
		name string
		args func(extra string) []string
	}{
		{
			name: "exec subcommand",
			args: func(extra string) []string {
				return []string{"--add-dir", extra, "exec", "--skip-permissions-unsafe", "write the note"}
			},
		},
		{
			name: "prompt flag",
			args: func(extra string) []string {
				return []string{"--add-dir", extra, "-p", "write the note", "--skip-permissions-unsafe"}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			cwd := t.TempDir()
			extra := tempDirOutsideDefaultTemp(t)
			target := filepath.Join(extra, "granted.txt")

			exitCode, stderr := runExecAddDirWriteProbe(t, cwd, tc.args(extra), target)
			if exitCode != exitSuccess {
				t.Fatalf("exit = %d, want %d; stderr = %q", exitCode, exitSuccess, stderr)
			}
			content, err := os.ReadFile(target)
			if err != nil {
				t.Fatalf("expected write inside --add-dir root to land, read %s: %v", target, err)
			}
			if string(content) != "granted" {
				t.Fatalf("content = %q, want %q", string(content), "granted")
			}

			// Negative control: the SAME run without --add-dir must be denied —
			// the grant is the only thing that widens the scope.
			denied := filepath.Join(extra, "denied.txt")
			deniedArgs := tc.args(extra)[2:]
			deniedExit, deniedStderr := runExecAddDirWriteProbe(t, cwd, deniedArgs, denied)
			if deniedExit != exitSuccess {
				t.Fatalf("denied-run exit = %d, want %d (tool denial is not fatal); stderr = %q", deniedExit, exitSuccess, deniedStderr)
			}
			if _, err := os.Stat(denied); !os.IsNotExist(err) {
				t.Fatalf("write outside the workspace without --add-dir must not land, stat err=%v", err)
			}
		})
	}
}

// runExecAddDirWriteProbe dispatches the given root argv through runWithDeps
// with a provider that calls write_file on target, then answers. It returns
// the exit code and stderr.
func runExecAddDirWriteProbe(t *testing.T, cwd string, args []string, target string) (int, string) {
	t.Helper()

	arguments, err := json.Marshal(map[string]string{"path": target, "content": "granted"})
	if err != nil {
		t.Fatalf("marshal write_file arguments: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps(args, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(_ string, _ config.Overrides) (config.ResolvedConfig, error) {
			return execResolvedConfig(), nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return toolCallingExecProvider{
				toolCallID: "call_write_extra",
				toolName:   "write_file",
				arguments:  string(arguments),
				answer:     "done",
			}, nil
		},
		newSandboxStore: func() (*sandbox.GrantStore, error) {
			return sandbox.NewGrantStore(sandbox.StoreOptions{FilePath: filepath.Join(t.TempDir(), "sandbox-grants.json")})
		},
	})
	return exitCode, stderr.String()
}

// TestExecScopeReRegistrationSwapsCoreToolsByName pins the mechanism runExec
// relies on: a nil-scope core registry is built first (so --list-tools and
// tool-filter validation work before config resolve), then once the run scope
// is known the scoped core tools are re-registered and Registry.Register
// replaces the earlier instances BY NAME. The before/after write_file probes
// prove both the overwrite and the scoped enforcement it ships.
func TestExecScopeReRegistrationSwapsCoreToolsByName(t *testing.T) {
	root := t.TempDir()
	extra := tempDirOutsideDefaultTemp(t)
	inside := filepath.Join(extra, "inside.txt")

	registry := newCoreRegistry(root)

	// Before re-registration the tools carry a nil scope, so an absolute path
	// inside the extra root (but outside the workspace) must be denied.
	denied := registry.RunWithOptions(context.Background(), "write_file", map[string]any{
		"path":    inside,
		"content": "too early",
	}, tools.RunOptions{PermissionGranted: true})
	if denied.Status == tools.StatusOK {
		t.Fatalf("nil-scope registry must deny extra-root write, got ok: %s", denied.Output)
	}
	if _, err := os.Stat(inside); !os.IsNotExist(err) {
		t.Fatalf("expected inside file to remain absent before re-registration, stat err=%v", err)
	}

	// Re-register exactly like exec.go does once the run scope is resolved.
	scope, err := sandbox.NewScope(root, []string{extra})
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	for _, tool := range tools.CoreToolsScoped(root, scope) {
		registry.Register(tool)
	}

	allowed := registry.RunWithOptions(context.Background(), "write_file", map[string]any{
		"path":    inside,
		"content": "granted",
	}, tools.RunOptions{PermissionGranted: true})
	if allowed.Status != tools.StatusOK {
		t.Fatalf("scoped registry must allow extra-root write, got %s: %s", allowed.Status, allowed.Output)
	}
	content, err := os.ReadFile(inside)
	if err != nil {
		t.Fatalf("read %s: %v", inside, err)
	}
	if string(content) != "granted" {
		t.Fatalf("inside content = %q, want %q", string(content), "granted")
	}

	// A path outside every scope root must still be denied after re-registration.
	outside := filepath.Join(tempDirOutsideDefaultTemp(t), "outside.txt")
	deniedOutside := registry.RunWithOptions(context.Background(), "write_file", map[string]any{
		"path":    outside,
		"content": "never",
	}, tools.RunOptions{PermissionGranted: true})
	if deniedOutside.Status == tools.StatusOK {
		t.Fatalf("write outside all roots must fail, got ok: %s", deniedOutside.Output)
	}
	if !strings.Contains(deniedOutside.Output, "must stay inside the workspace") {
		t.Fatalf("expected outside-workspace denial, got %q", deniedOutside.Output)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("expected outside file to remain absent, stat err=%v", err)
	}
}

func tempDirOutsideDefaultTemp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", ".zero-sandbox-outside-")
	if err != nil {
		t.Fatalf("MkdirTemp outside default temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("Abs(%q): %v", dir, err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return filepath.Clean(abs)
}
