package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// writeRecordedRoot puts a setup marker naming root under sandboxHome.
func writeRecordedRoot(t *testing.T, sandboxHome, root string) {
	t.Helper()
	path := WindowsSandboxSetupMarkerPath(sandboxHome)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(WindowsSandboxSetupMarker{SchemaVersion: windowsSandboxSetupMarkerSchemaVersion, RuntimeRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
}

// ONE COMMAND, ONE SANDBOX HOME.
//
// Runtime preparation happens before Windows platform planning, and it used to
// resolve the sandbox home from the AMBIENT environment while the planner
// resolves ZERO_WINDOWS_SANDBOX_HOME out of the command's own spec.Env and hands
// that one to the runner for marker validation. So a command that explicitly
// selects home B, while the parent process still points at home A, pinned A's
// recorded root into the profile; the runner then loaded B's marker, saw a
// different root, and rejected the command as out of date even though setup for
// B was valid. The two homes need no different derivation rules to disagree,
// only different valid selections from the same preferred/fallback pair.
func TestThePinnedRootComesFromTheCommandsOwnSandboxHome(t *testing.T) {
	homeA := t.TempDir()
	homeB := t.TempDir()
	preferred := filepath.Join(t.TempDir(), "preferred")
	fallback := filepath.Join(t.TempDir(), "fallback")

	writeRecordedRoot(t, homeA, preferred)
	writeRecordedRoot(t, homeB, fallback)

	if got := pinnedSandboxRuntimeRoot(t.TempDir(), preferred, fallback, homeB); got != fallback {
		t.Errorf("pinned %q for a command that selected home B, want %q: the ambient home decided instead of the command's", got, fallback)
	}
	if got := pinnedSandboxRuntimeRoot(t.TempDir(), preferred, fallback, homeA); got != preferred {
		t.Errorf("pinned %q for home A, want %q", got, preferred)
	}
}

// A recorded root that this workspace could not select is still refused, which
// is the protection that stops one workspace's tree being pinned into another.
func TestARecordedRootFromAnotherWorkspaceIsStillRefused(t *testing.T) {
	home := t.TempDir()
	writeRecordedRoot(t, home, filepath.Join(t.TempDir(), "someone-elses-tree"))
	if got := pinnedSandboxRuntimeRoot(t.TempDir(), filepath.Join(t.TempDir(), "preferred"), filepath.Join(t.TempDir(), "fallback"), home); got != "" {
		t.Errorf("pinned %q, want none: it matches neither candidate this workspace derives", got)
	}
}

// No command context means the ambient environment is the only authority there
// is, so an empty home must still resolve rather than refusing outright.
func TestAnEmptyCommandHomeFallsBackToTheAmbientEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZERO_WINDOWS_SANDBOX_HOME", home)
	preferred := filepath.Join(t.TempDir(), "preferred")
	writeRecordedRoot(t, home, preferred)
	if got := pinnedSandboxRuntimeRoot(t.TempDir(), preferred, filepath.Join(t.TempDir(), "fallback"), ""); got != preferred {
		t.Errorf("pinned %q with no command home, want the ambient one to decide (%q)", got, preferred)
	}
}

// BuildCommandPlan must not run runtime preparation when the command's own env
// makes sandbox-home resolution fail; otherwise selection pins against the
// ambient home while Windows planning later uses spec.Env.
func TestBuildCommandPlanFailsClosedOnInvalidCommandSandboxHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("relative grant path with deleted cwd is exercised on Unix CI")
	}
	workspace := t.TempDir()
	engine := NewEngine(EngineOptions{
		WorkspaceRoot: workspace,
		Policy:        DefaultPolicy(),
		Backend: Backend{
			Name:            BackendLinuxBwrap,
			Available:       true,
			Platform:        "linux",
			Executable:      "/usr/bin/zero-linux-sandbox",
			CommandWrapping: true,
			NativeIsolation: true,
		},
	})

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	removed := t.TempDir()
	if err := os.Chdir(removed); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	if err := os.RemoveAll(removed); err != nil {
		t.Fatalf("remove cwd parent: %v", err)
	}

	_, err = engine.BuildCommandPlan(CommandSpec{
		Name: "/bin/true",
		Dir:  workspace,
		Env:  []string{"ZERO_SANDBOX_GRANTS_PATH=relative/grants.json"},
	})
	if err == nil {
		t.Fatal("want BuildCommandPlan to fail when command env prevents sandbox-home resolution")
	}
}
