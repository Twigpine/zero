package pathjail

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func openRenameRoot(t *testing.T) (*os.Root, string) {
	t.Helper()
	base := t.TempDir()
	root, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root, base
}

// A CREATE'S NAME MUST NOT BE LOST TO A RACE. The no-replace rename reports the
// collision as fs.ErrExist and leaves both files exactly as they were, so the
// caller can refuse the create instead of replacing the other writer.
func TestRenameNoReplaceRefusesAnExistingDestination(t *testing.T) {
	root, base := openRenameRoot(t)
	if err := os.WriteFile(filepath.Join(base, "source"), []byte("source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "target"), []byte("target\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := RenameNoReplace(root, "source", "target"); !errors.Is(err, os.ErrExist) {
		t.Fatalf("RenameNoReplace over an existing name = %v, want os.ErrExist", err)
	}
	for name, want := range map[string]string{"source": "source\n", "target": "target\n"} {
		got, err := os.ReadFile(filepath.Join(base, name))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, err=%v; a refused rename must not move either file", name, got, err)
		}
	}
}

func TestRenameNoReplacePublishesAnAbsentDestination(t *testing.T) {
	root, base := openRenameRoot(t)
	if err := os.WriteFile(filepath.Join(base, "source"), []byte("whole\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := RenameNoReplace(root, "source", "target"); err != nil {
		t.Fatalf("RenameNoReplace = %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(base, "target")); err != nil || string(got) != "whole\n" {
		t.Fatalf("target = %q, err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(base, "source")); !os.IsNotExist(err) {
		t.Fatalf("source survived a successful rename: %v", err)
	}
}

// The syscall is given a directory handle and bare names. Allowing a
// separator here would let an ancestor component resolve outside the directory
// the caller checked, so it is refused before the syscall.
func TestRenameNoReplaceRejectsPathComponents(t *testing.T) {
	root, _ := openRenameRoot(t)
	for _, tc := range []struct{ oldname, newname string }{
		{"", "target"},
		{"source", ""},
		{".", "target"},
		{"source", ".."},
		{"dir/source", "target"},
		{"source", "dir/target"},
		{filepath.Join("dir", "source"), "target"},
	} {
		if err := RenameNoReplace(root, tc.oldname, tc.newname); !errors.Is(err, ErrEscapes) {
			t.Errorf("RenameNoReplace(%q, %q) = %v, want ErrEscapes", tc.oldname, tc.newname, err)
		}
	}
}
