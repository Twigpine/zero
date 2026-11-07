package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func strPtr(value string) *string { return &value }

// installCommitRace replaces the destination after the caller observed it but
// before commitFileContents opens the object it will publish. It is the
// deterministic half of the check-to-use window.
func installCommitRace(t *testing.T, mutate func(string)) {
	t.Helper()
	prior := fileWriteBeforeCommit
	fileWriteBeforeCommit = mutate
	t.Cleanup(func() { fileWriteBeforeCommit = prior })
}

func TestCommitFileContentsRefusesCreateRace(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "created.txt")
	installCommitRace(t, func(path string) {
		if err := os.WriteFile(path, []byte("other writer\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})

	if _, err := commitFileContents(target, nil, nil, "zero\n"); err == nil {
		t.Fatal("a path that appeared after the observation must not be overwritten")
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "other writer\n" {
		t.Fatalf("raced create content = %q, err=%v", got, err)
	}
}

func TestCommitFileContentsRefusesOverwriteInodeSwap(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(target, []byte("observed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	observed, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	installCommitRace(t, func(path string) {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("other writer\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})

	if _, err := commitFileContents(target, observed, strPtr("observed\n"), "zero\n"); err == nil {
		t.Fatal("a destination replaced since the observation must not be overwritten")
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "other writer\n" {
		t.Fatalf("raced overwrite content = %q, err=%v", got, err)
	}
}

func TestCommitFileContentsRefusesOverwriteContentChange(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(target, []byte("observed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	observed, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	installCommitRace(t, func(path string) {
		// Same inode, different bytes: only the preimage comparison can catch it.
		if err := os.WriteFile(path, []byte("rewritten in place\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})

	if _, err := commitFileContents(target, observed, strPtr("observed\n"), "zero\n"); err == nil {
		t.Fatal("a destination whose bytes changed since the observation must not be overwritten")
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "rewritten in place\n" {
		t.Fatalf("raced preimage content = %q, err=%v", got, err)
	}
}

// The publication must be a same-directory temp-and-replace, not an in-place
// truncation: that is what makes invariant #921 (no partial destination after a
// crash or cancellation) hold. The inode changing across a successful overwrite
// is the observable consequence.
func TestCommitFileContentsPublishesByReplacingTheInode(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(target, []byte("observed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	observed, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}

	warning, err := commitFileContents(target, observed, strPtr("observed\n"), "replacement\n")
	if err != nil {
		t.Fatalf("commitFileContents: %v", err)
	}
	if warning != "" {
		t.Fatalf("unexpected cleanup warning: %q", warning)
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(observed, after) {
		t.Fatal("destination inode survived the write: this is an in-place truncation, not an atomic replacement")
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "replacement\n" {
		t.Fatalf("published content = %q, err=%v", got, err)
	}
	// No staging leftovers next to the destination.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".zero-tmp-") {
			t.Fatalf("staging file %q was left behind", entry.Name())
		}
	}
}

func TestWriteFileRefusesRaceBeforeAtomicPublish(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "existing.txt")
	if err := os.WriteFile(target, []byte("observed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	installCommitRace(t, func(path string) {
		if err := os.WriteFile(path, []byte("other writer\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	result := NewScopedWriteFileTool(root, nil).Run(context.Background(), map[string]any{
		"path": "existing.txt", "content": "zero\n", "overwrite": true,
	})
	if result.Status != StatusError || !strings.Contains(result.Output, errFileChangedDuringWrite.Error()) {
		t.Fatalf("raced overwrite = %s: %s", result.Status, result.Output)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "other writer\n" {
		t.Fatalf("raced overwrite content = %q, err=%v", got, err)
	}
}

func TestEditFileRefusesRaceBeforeAtomicPublish(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "existing.txt")
	if err := os.WriteFile(target, []byte("observed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	installCommitRace(t, func(path string) {
		if err := os.WriteFile(path, []byte("other writer\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	result := NewScopedEditFileTool(root, nil).Run(context.Background(), map[string]any{
		"path": "existing.txt", "old_string": "observed", "new_string": "zero",
	})
	if result.Status != StatusError || !strings.Contains(result.Output, errFileChangedDuringWrite.Error()) {
		t.Fatalf("raced edit = %s: %s", result.Status, result.Output)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "other writer\n" {
		t.Fatalf("raced edit content = %q, err=%v", got, err)
	}
}
