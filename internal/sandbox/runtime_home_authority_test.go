package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
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
