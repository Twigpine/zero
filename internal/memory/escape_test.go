package memory

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// linkDir uses a junction on Windows: it needs no privilege, unlike a symlink,
// so it is both the reachable attack and the only one testable on an ordinary
// Windows account.
func linkDir(t *testing.T, target, link string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Skipf("cannot create a junction: %v %s", err, out)
		}
		return
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create a symlink: %v", err)
	}
}

// The store used to check only its own directory and file, so a link at the
// ANCESTOR .zero turned every operation into one aimed outside the workspace:
// Write and Forget were arbitrary write and delete, and Read was an arbitrary
// read in a tool the model can call by name.
//
// All three are asserted, because fixing one and leaving the others is exactly
// what the original guard did.
func TestAnAncestorLinkCannotTakeTheStoreOutOfTheWorkspace(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	workspace := filepath.Join(base, "workspace")
	for _, dir := range []string{outside, workspace} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// A note already sitting in the external directory, so a successful read
	// would be visible rather than merely "no error".
	if err := os.MkdirAll(filepath.Join(outside, "memory"), 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "memory", "secret.md")
	if err := os.WriteFile(secret, []byte("do not read me"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkDir(t, outside, filepath.Join(workspace, ".zero"))

	paths := DefaultPaths(workspace)

	if _, err := Write(paths, ScopeProject, "escaped", "d", "b"); err == nil {
		t.Error("Write went through the linked ancestor")
	}
	if _, err := os.Stat(filepath.Join(outside, "memory", "escaped.md")); !os.IsNotExist(err) {
		t.Errorf("a note was written outside the workspace, stat error = %v", err)
	}
	if _, err := Read(paths, ScopeProject, "secret"); err == nil {
		t.Error("Read returned a note from outside the workspace")
	}
	if err := Forget(paths, ScopeProject, "secret"); err == nil {
		t.Error("Forget accepted a target outside the workspace")
	}
	if _, err := os.Stat(secret); err != nil {
		t.Errorf("Forget deleted a file outside the workspace: %v", err)
	}
	notes, listErr := List(paths)
	if len(notes) != 0 {
		t.Errorf("List surfaced %d note(s) from outside the workspace", len(notes))
	}
	// The refusal is REPORTED, not swallowed. A store that cannot be opened
	// because a link redirects it out of the workspace is an operational failure;
	// returning an empty list for it would tell the caller there are no notes,
	// which is how a redirected store looks exactly like an empty one.
	if listErr == nil {
		t.Error("List reported no problem for a store redirected outside the workspace")
	}
}

// The ordinary path still works, or the test above would pass against a store
// that refused everything.
func TestAnOrdinaryWorkspaceStoreStillRoundTrips(t *testing.T) {
	workspace := t.TempDir()
	paths := DefaultPaths(workspace)
	if _, err := Write(paths, ScopeProject, "note", "a summary", "the body"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	note, err := Read(paths, ScopeProject, "note")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if note.Description != "a summary" || strings.TrimSpace(note.Body) != "the body" {
		t.Errorf("round trip lost content: %+v", note)
	}
	notes, listErr := List(paths)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(notes) != 1 {
		t.Errorf("List returned %d notes, want 1", len(notes))
	}
	if err := Forget(paths, ScopeProject, "note"); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if _, err := Read(paths, ScopeProject, "note"); err == nil {
		t.Error("the note survived Forget")
	}
}
