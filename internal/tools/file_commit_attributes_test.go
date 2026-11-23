package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func installFileWriteAttributes(t *testing.T, apply func(*os.File, *uint32) error) {
	t.Helper()
	prior := fileWriteApplyAttributes
	fileWriteApplyAttributes = apply
	t.Cleanup(func() { fileWriteApplyAttributes = prior })
}

// AN OVERWRITE THAT CANNOT CARRY THE FILE'S ATTRIBUTES DOES NOT HAPPEN.
//
// The staged file is a new one and starts with no attributes. If the replaced
// file's cannot be put on it, publishing anyway would change how the file is
// presented behind the user's back, so the write is refused, the observed
// bytes stay, and the side file is removed. This runs on every platform: the
// seam is the same everywhere, and only what sits behind it differs.
func TestOverwriteRefusesWhenAttributesCannotBeCarried(t *testing.T) {
	failure := errors.New("injected attribute failure")
	root := t.TempDir()
	target := filepath.Join(root, "existing.txt")
	if err := os.WriteFile(target, []byte("observed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = workspace.Close() }()
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	preimage := "observed\n"
	calls := 0
	installFileWriteAttributes(t, func(*os.File, *uint32) error {
		calls++
		return failure
	})

	err = commitRootedFileContents(workspace, target, "existing.txt", info, &preimage, "replacement\n")
	if !errors.Is(err, failure) {
		t.Fatalf("commit error = %v, want the injected attribute failure", err)
	}
	if calls != 1 {
		t.Fatalf("SETUP INVALID: the attribute seam ran %d times, want once", calls)
	}
	got, readErr := os.ReadFile(target)
	if readErr != nil || string(got) != "observed\n" {
		t.Fatalf("file = %q, err=%v; the observed bytes must outlive a refused overwrite", got, readErr)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("directory holds %s; a refused overwrite must remove its side file", strings.Join(names, ", "))
	}
}

// A CREATE CARRIES NO ATTRIBUTES.
//
// It has no original to take them from. The overwrite that follows proves the
// seam is reached at all, so the create half cannot pass by never calling it.
func TestACreateCarriesNoAttributes(t *testing.T) {
	root := t.TempDir()
	var applied []*uint32
	installFileWriteAttributes(t, func(_ *os.File, attributes *uint32) error {
		applied = append(applied, attributes)
		return nil
	})
	tool := NewScopedWriteFileTool(root, nil)

	created := tool.Run(context.Background(), map[string]any{"path": "note.txt", "content": "first\n"})
	if created.Status != StatusOK {
		t.Fatalf("create = %s: %s", created.Status, created.Output)
	}
	if len(applied) != 1 {
		t.Fatalf("SETUP INVALID: the attribute seam ran %d times for a create, want once", len(applied))
	}
	if applied[0] != nil {
		t.Fatalf("a create handed its side file the attributes %#x; it has no original to carry any from", *applied[0])
	}
	overwrote := tool.Run(context.Background(), map[string]any{"path": "note.txt", "content": "second\n", "overwrite": true})
	if overwrote.Status != StatusOK {
		t.Fatalf("overwrite = %s: %s", overwrote.Status, overwrote.Output)
	}
	if len(applied) != 2 {
		t.Fatalf("the attribute seam ran %d times across a create and an overwrite, want twice", len(applied))
	}
}
