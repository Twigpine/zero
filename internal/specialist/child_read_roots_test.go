package specialist

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
)

const readGrant = "/grants/read-only"

// THE RUN'S READ GRANTS REACH AN ORDINARY TASK, AND ONLY AS READ.
//
// The live read-root supplier was wired into the orchestrate tool alone, so a
// fresh ordinary Task got no read-only roots: it could not inspect a path the
// parent had just been authorised to read, while a plan task could. The grant
// must arrive as --add-read-dir and never as --add-dir, which would be a write.
func TestAnOrdinaryTaskIsLaunchedWithTheRunsReadGrantsReadOnly(t *testing.T) {
	var argv []string
	executor := lifetimeExecutor(t, &argv)
	executor.ExtraReadRoots = func(bool) []string { return []string{readGrant} }
	result := NewTaskTool(executor).RunWithOptions(context.Background(),
		map[string]any{"name": "explorer", "prompt": "audit it"}, tools.RunOptions{Cwd: t.TempDir()})
	if result.Status != tools.StatusOK {
		t.Fatalf("Task failed: %s", result.Output)
	}
	assertReadOnlyGrant(t, argv, readGrant)
}

// AND A RESUMED ONE. BuildResumeArgs emitted only the write roots, so a task
// that read the granted path on its first launch failed on the same path when
// resumed.
func TestAResumedTaskIsLaunchedWithTheRunsReadGrantsReadOnly(t *testing.T) {
	store, session := resumableSession(t)
	var argv []string
	executor := lifetimeExecutor(t, &argv)
	executor.SessionStore = store
	executor.ExtraReadRoots = func(bool) []string { return []string{readGrant} }
	result := NewTaskTool(executor).RunWithOptions(context.Background(),
		map[string]any{"resume": session.SessionID, "prompt": "carry on"}, tools.RunOptions{Cwd: t.TempDir()})
	if result.Status != tools.StatusOK {
		t.Fatalf("resume failed: %s", result.Output)
	}
	assertReadOnlyGrant(t, argv, readGrant)
}

// The read supplier follows the same lifetime rule as the write one: a
// turn-scoped read grant reaches only a child that ends within the turn.
func TestATurnScopedReadGrantSkipsABackgroundTask(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     map[string]any
		wantTurn bool
	}{
		{name: "foreground", args: map[string]any{"name": "explorer", "prompt": "p"}, wantTurn: true},
		{name: "background", args: map[string]any{"name": "explorer", "prompt": "p", "run_in_background": true}, wantTurn: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var argv []string
			executor := lifetimeExecutor(t, &argv)
			executor.ExtraWriteRoots = nil
			executor.ExtraReadRoots = lifetimeSupplier
			result := NewTaskTool(executor).RunWithOptions(context.Background(), tc.args, tools.RunOptions{Cwd: t.TempDir()})
			if result.Status != tools.StatusOK {
				t.Fatalf("Task failed: %s", result.Output)
			}
			assertReadOnlyGrant(t, argv, sessionRoot)
			if got := argsContainPair(argv, "--add-read-dir", turnRoot); got != tc.wantTurn {
				t.Fatalf("turn-scoped read root in argv = %v, want %v:\n%s", got, tc.wantTurn, strings.Join(argv, " "))
			}
		})
	}
}

// A path that is both a write root and a read root is passed once, as the
// write root it is. An ancestor withheld from an isolated child's WRITE roots
// still reaches it read-only: reading the parent tree does not defeat write
// isolation, and the child stands on no less than it may.
func TestReadGrantsBesideWriteRootsAndIsolation(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dev := filepath.Join(base, "dev")
	parentWorkspace := filepath.Join(dev, "zero")
	worktree := filepath.Join(base, "worktrees", "plan-a")
	granted := filepath.Join(base, "granted")
	for _, dir := range []string{parentWorkspace, worktree, granted} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	executor := Executor{
		NewSessionID:    func() (string, error) { return "specialist_00000000000000000000000a", nil },
		WorkspaceRoot:   parentWorkspace,
		ExtraWriteRoots: func(bool) []string { return []string{dev, granted} },
		ExtraReadRoots:  func(bool) []string { return []string{dev, granted} },
	}
	ordinary, err := executor.BuildArgs(BuildArgsInput{
		Manifest: Manifest{Metadata: Metadata{Name: "explorer"}}, Prompt: "p", Cwd: parentWorkspace,
	})
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}
	for _, root := range []string{dev, granted} {
		if !argsContainPair(ordinary.Args, "--add-dir", root) || argsContainPair(ordinary.Args, "--add-read-dir", root) {
			t.Errorf("%q should be passed once, as a write root:\n%s", root, strings.Join(ordinary.Args, " "))
		}
	}
	isolated, err := executor.BuildArgs(BuildArgsInput{
		Manifest: Manifest{Metadata: Metadata{Name: "explorer"}}, Prompt: "p", Cwd: worktree,
	})
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}
	assertReadOnlyGrant(t, isolated.Args, dev)
}

func assertReadOnlyGrant(t *testing.T, argv []string, root string) {
	t.Helper()
	if !argsContainPair(argv, "--add-read-dir", root) {
		t.Fatalf("the read grant %q never reached the child as --add-read-dir:\n%s", root, strings.Join(argv, " "))
	}
	if argsContainPair(argv, "--add-dir", root) {
		t.Fatalf("the read grant %q was emitted as --add-dir — a read grant escalated to write:\n%s", root, strings.Join(argv, " "))
	}
}
