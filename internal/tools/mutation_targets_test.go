package tools

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestMutationTargets(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		tool string
		args map[string]any
		want []string
	}{
		{"write_file", map[string]any{"path": "a/b.txt", "content": "x"}, []string{"a/b.txt"}},
		{"edit_file", map[string]any{"path": "c.txt", "old_string": "x", "new_string": "y"}, []string{"c.txt"}},
		{"apply_patch", map[string]any{"patch": "--- a/d.txt\n+++ b/d.txt\n@@ -1 +1 @@\n-x\n+y\n"}, []string{"d.txt"}},
		{"apply_patch", map[string]any{"patch": "*** Begin Patch\n*** Add File: nested/new.txt\n+x\n*** Update File: old.txt\n*** Move to: moved.txt\n@@\n-old\n+new\n*** End Patch\n"}, []string{"nested/new.txt", "old.txt", "moved.txt"}},
		{"bash", map[string]any{"command": "echo hi"}, nil},
		{"read_file", map[string]any{"path": "e.txt"}, nil},
		{"grep", map[string]any{"pattern": "x"}, nil},
	}
	for _, tc := range cases {
		got := MutationTargets(root, tc.tool, tc.args)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.tool, got, tc.want)
		}
	}
}

func TestMutationTargetsResolvesAliasKeys(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name string
		tool string
		args map[string]any
		want []string
	}{
		{"write_file file alias", "write_file", map[string]any{"file": "x.go", "content": "x"}, []string{"x.go"}},
		{"write_file file_path alias", "write_file", map[string]any{"file_path": "y.go", "content": "x"}, []string{"y.go"}},
		{"write_file filename alias", "write_file", map[string]any{"filename": "z.go", "content": "x"}, []string{"z.go"}},
		{"edit_file file alias", "edit_file", map[string]any{"file": "e.go", "old_string": "a", "new_string": "b"}, []string{"e.go"}},
		{"apply_patch diff alias", "apply_patch", map[string]any{"diff": "--- a/d.txt\n+++ b/d.txt\n@@ -1 +1 @@\n-x\n+y\n"}, []string{"d.txt"}},
	}
	for _, tc := range cases {
		got := MutationTargets(root, tc.tool, tc.args)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestMutationTargetsRejectsEscapingPaths(t *testing.T) {
	root := t.TempDir()
	if got := MutationTargets(root, "write_file", map[string]any{"path": "../escape.txt", "content": "x"}); len(got) != 0 {
		t.Errorf("expected no targets for escaping path, got %v", got)
	}
	// apply_patch must also reject a patch whose paths escape the workspace, so it
	// never returns an out-of-workspace checkpoint target.
	escapePatch := "--- a/../escape.txt\n+++ b/../escape.txt\n@@ -1 +1 @@\n-one\n+two\n"
	if got := MutationTargets(root, "apply_patch", map[string]any{"patch": escapePatch}); len(got) != 0 {
		t.Errorf("expected no targets for escaping apply_patch, got %v", got)
	}
	structuredEscapePatch := "*** Begin Patch\n*** Add File: ../escape.txt\n+x\n*** End Patch\n"
	if got := MutationTargets(root, "apply_patch", map[string]any{"patch": structuredEscapePatch}); len(got) != 0 {
		t.Errorf("expected no targets for escaping structured apply_patch, got %v", got)
	}
}

// Finding 3: with cwd != ".", apply_patch's MutationTargets must return
// WORKSPACE-relative paths (cwd-prefixed), not paths relative to the patch cwd —
// otherwise /rewind snapshots the wrong (workspace-root-relative) path.
func TestMutationTargetsApplyPatchPrefixesCwd(t *testing.T) {
	root := t.TempDir()
	// cwd must exist for the patch to be applicable; MutationTargets resolves it
	// the same way apply_patch does (resolveWorkspacePath -> EvalSymlinks).
	if err := os.MkdirAll(filepath.Join(root, "sub", "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	patch := "--- a/d.txt\n+++ b/d.txt\n@@ -1 +1 @@\n-x\n+y\n"

	got := MutationTargets(root, "apply_patch", map[string]any{"patch": patch, "cwd": "sub/dir"})
	want := []string{"sub/dir/d.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	structuredPatch := "*** Begin Patch\n*** Update File: d.txt\n@@\n-x\n+y\n*** End Patch\n"
	got = MutationTargets(root, "apply_patch", map[string]any{"patch": structuredPatch, "cwd": "sub/dir"})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("structured cwd: got %v, want %v", got, want)
	}

	// cwd "." (the default) must keep paths unprefixed.
	got = MutationTargets(root, "apply_patch", map[string]any{"patch": patch, "cwd": "."})
	if !reflect.DeepEqual(got, []string{"d.txt"}) {
		t.Fatalf("cwd=.: got %v, want [d.txt]", got)
	}

	// Absent cwd behaves like ".".
	got = MutationTargets(root, "apply_patch", map[string]any{"patch": patch})
	if !reflect.DeepEqual(got, []string{"d.txt"}) {
		t.Fatalf("no cwd: got %v, want [d.txt]", got)
	}
}

func TestStripPatchPrefixStripsOnlyOne(t *testing.T) {
	root := t.TempDir()
	// A workspace file under a directory literally named "b".
	got := MutationTargets(root, "apply_patch", map[string]any{
		"patch": "--- a/b/foo.txt\n+++ b/b/foo.txt\n@@ -1 +1 @@\n-x\n+y\n",
	})
	if len(got) != 1 || got[0] != "b/foo.txt" {
		t.Fatalf("expected [b/foo.txt], got %v", got)
	}
}
