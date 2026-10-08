package tools

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func mustReadTestFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func TestCoreToolsExposeWriteAndPlanTools(t *testing.T) {
	toolset := CoreToolsScoped(t.TempDir(), nil)
	byName := make(map[string]Tool, len(toolset))
	for _, tool := range toolset {
		byName[tool.Name()] = tool
	}

	for _, name := range []string{"write_file", "edit_file", "apply_patch"} {
		tool, ok := byName[name]
		if !ok {
			t.Fatalf("expected core tools to include %s", name)
		}
		if tool.Safety().SideEffect != SideEffectWrite {
			t.Fatalf("%s side effect = %s, want write", name, tool.Safety().SideEffect)
		}
		if tool.Safety().Permission != PermissionPrompt {
			t.Fatalf("%s permission = %s, want prompt", name, tool.Safety().Permission)
		}
	}

	planTool, ok := byName["update_plan"]
	if !ok {
		t.Fatalf("expected core tools to include update_plan")
	}
	if planTool.Safety().Permission != PermissionAllow {
		t.Fatalf("update_plan permission = %s, want allow", planTool.Safety().Permission)
	}
}

func TestRegistryBlocksPromptToolsWithoutGrant(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "blocked.txt")
	registry := NewRegistry()
	registry.Register(NewScopedWriteFileTool(root, nil))

	result := registry.Run(context.Background(), "write_file", map[string]any{
		"path":    "blocked.txt",
		"content": "nope",
	})

	if result.Status != StatusError {
		t.Fatalf("expected error status, got %s", result.Status)
	}
	if !strings.Contains(result.Output, "Permission required for write_file") {
		t.Fatalf("expected permission error, got %q", result.Output)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("expected file to remain absent, stat err=%v", err)
	}
}

func TestRegistryRunsPromptToolsWithGrant(t *testing.T) {
	root := t.TempDir()
	registry := NewRegistry()
	registry.Register(NewScopedWriteFileTool(root, nil))

	result := registry.RunWithOptions(context.Background(), "write_file", map[string]any{
		"path":    "allowed.txt",
		"content": "hello",
	}, RunOptions{PermissionGranted: true})

	if result.Status != StatusOK {
		t.Fatalf("expected ok status, got %s: %s", result.Status, result.Output)
	}
	content, err := os.ReadFile(filepath.Join(root, "allowed.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "hello" {
		t.Fatalf("expected written content, got %q", string(content))
	}
}

func TestWriteFileToolCreatesAndProtectsExistingFiles(t *testing.T) {
	root := t.TempDir()
	tool := NewScopedWriteFileTool(root, nil)

	created := tool.Run(context.Background(), map[string]any{
		"path":    "nested/file.txt",
		"content": "first",
	})
	if created.Status != StatusOK {
		t.Fatalf("expected create ok, got %s: %s", created.Status, created.Output)
	}
	if !strings.Contains(created.Output, "Created nested/file.txt") {
		t.Fatalf("unexpected create output: %q", created.Output)
	}

	refused := tool.Run(context.Background(), map[string]any{
		"path":    "nested/file.txt",
		"content": "second",
	})
	if refused.Status != StatusError {
		t.Fatalf("expected overwrite refusal, got %s", refused.Status)
	}
	content, err := os.ReadFile(filepath.Join(root, "nested", "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "first" {
		t.Fatalf("expected original content, got %q", string(content))
	}

	overwrote := tool.Run(context.Background(), map[string]any{
		"path":      "nested/file.txt",
		"content":   "second",
		"overwrite": true,
	})
	if overwrote.Status != StatusOK {
		t.Fatalf("expected overwrite ok, got %s: %s", overwrote.Status, overwrote.Output)
	}
	content, err = os.ReadFile(filepath.Join(root, "nested", "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "second" {
		t.Fatalf("expected overwritten content, got %q", string(content))
	}
}

func TestWriteFileToolRecordsCreatedFileButNotOverwrite(t *testing.T) {
	root := t.TempDir()
	registry := NewRegistry()
	registry.Register(NewScopedWriteFileTool(root, nil))
	tracker := NewFileTracker()

	created := registry.RunWithOptions(context.Background(), "write_file", map[string]any{
		"path":    "scratch.txt",
		"content": "first",
	}, RunOptions{PermissionGranted: true, FileTracker: tracker})
	if created.Status != StatusOK {
		t.Fatalf("expected create ok, got %s: %s", created.Status, created.Output)
	}
	absPath := filepath.Join(root, "scratch.txt")
	absPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", absPath, err)
	}
	if got := tracker.CreatedFiles(); len(got) != 1 || got[0] != absPath {
		t.Fatalf("CreatedFiles() = %v, want [%s]", got, absPath)
	}

	overwrote := registry.RunWithOptions(context.Background(), "write_file", map[string]any{
		"path":      "scratch.txt",
		"content":   "second",
		"overwrite": true,
	}, RunOptions{PermissionGranted: true, FileTracker: tracker})
	if overwrote.Status != StatusOK {
		t.Fatalf("expected overwrite ok, got %s: %s", overwrote.Status, overwrote.Output)
	}
	// Overwriting an existing file is not a "new" creation, so CreatedFiles
	// must not gain a second (duplicate) entry for it.
	if got := tracker.CreatedFiles(); len(got) != 1 || got[0] != absPath {
		t.Fatalf("CreatedFiles() after overwrite = %v, want unchanged [%s]", got, absPath)
	}
}

func TestApplyPatchRecordsCreatedFileButNotExistingEdits(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "existing.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry()
	registry.Register(NewScopedApplyPatchTool(root, nil))
	tracker := NewFileTracker()
	patch := strings.Join([]string{
		"diff --git a/scratch.txt b/scratch.txt",
		"new file mode 100644",
		"index 0000000..e965047",
		"--- /dev/null",
		"+++ b/scratch.txt",
		"@@ -0,0 +1 @@",
		"+hello",
		"diff --git a/existing.txt b/existing.txt",
		"--- a/existing.txt",
		"+++ b/existing.txt",
		"@@ -1 +1 @@",
		"-old",
		"+new",
		"",
	}, "\n")

	result := registry.RunWithOptions(context.Background(), "apply_patch", map[string]any{
		"patch": patch,
	}, RunOptions{PermissionGranted: true, FileTracker: tracker})
	if result.Status != StatusOK {
		if gitApplyUnavailable(result.Output) {
			t.Skipf("git binary unavailable: %s", result.Output)
		}
		t.Fatalf("expected apply_patch ok, got %s: %s", result.Status, result.Output)
	}
	absPath := filepath.Join(root, "scratch.txt")
	absPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", absPath, err)
	}
	if got := tracker.CreatedFiles(); len(got) != 1 || got[0] != absPath {
		t.Fatalf("CreatedFiles() = %v, want [%s]", got, absPath)
	}
}

func TestWriteFileSummaryReportsLineCount(t *testing.T) {
	root := t.TempDir()
	tool := NewScopedWriteFileTool(root, nil)
	// Three lines, no trailing newline -> "3 lines" (not a byte count).
	result := tool.Run(context.Background(), map[string]any{
		"path":    "multi.txt",
		"content": "one\ntwo\nthree",
	})
	if result.Status != StatusOK {
		t.Fatalf("expected ok, got %s: %s", result.Status, result.Output)
	}
	if !strings.Contains(result.Output, "(3 lines)") {
		t.Fatalf("summary should report a line count, got %q", result.Output)
	}
	if strings.Contains(result.Output, "bytes") {
		t.Errorf("summary should no longer report bytes: %q", result.Output)
	}
}

func TestWriteFileToolAllowsEmptyContent(t *testing.T) {
	root := t.TempDir()

	result := NewScopedWriteFileTool(root, nil).Run(context.Background(), map[string]any{
		"path":    "empty.txt",
		"content": "",
	})

	if result.Status != StatusOK {
		t.Fatalf("expected ok status, got %s: %s", result.Status, result.Output)
	}
	content, err := os.ReadFile(filepath.Join(root, "empty.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "" {
		t.Fatalf("expected empty file, got %q", string(content))
	}
}

func TestWriteFileToolReportsTypeErrorsForEmptyAllowedStrings(t *testing.T) {
	result := NewScopedWriteFileTool(t.TempDir(), nil).Run(context.Background(), map[string]any{
		"path":    "bad.txt",
		"content": 42,
	})

	if result.Status != StatusError {
		t.Fatalf("expected error status, got %s", result.Status)
	}
	if !strings.Contains(result.Output, "content must be a string") {
		t.Fatalf("expected string type error, got %q", result.Output)
	}
}

func TestWriteFileToolRejectsOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")

	result := NewScopedWriteFileTool(root, nil).Run(context.Background(), map[string]any{
		"path":    outside,
		"content": "secret",
	})

	if result.Status != StatusError {
		t.Fatalf("expected error status, got %s", result.Status)
	}
	if !strings.Contains(result.Output, "must stay inside the workspace") {
		t.Fatalf("expected workspace error, got %q", result.Output)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("expected outside file to remain absent, stat err=%v", err)
	}
}

func TestWriteFileToolRejectsSymlinkParent(t *testing.T) {
	root := t.TempDir()
	realDirectory := filepath.Join(root, "real")
	if err := os.MkdirAll(realDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDirectory, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	result := NewScopedWriteFileTool(root, nil).Run(context.Background(), map[string]any{
		"path":    "link/escape.txt",
		"content": "secret",
	})

	if result.Status != StatusError {
		t.Fatalf("expected error status, got %s", result.Status)
	}
	if !strings.Contains(result.Output, "must not traverse symlink") {
		t.Fatalf("expected symlink error, got %q", result.Output)
	}
	if _, err := os.Stat(filepath.Join(realDirectory, "escape.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected symlink target file to remain absent, stat err=%v", err)
	}
}

func TestEditFileToolReplacesExactStrings(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "code.go")
	writeTestFile(t, path, "const a = 1\nconst b = 2\n")
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}

	result := NewScopedEditFileTool(root, nil).Run(context.Background(), map[string]any{
		"path":       "code.go",
		"old_string": "const a = 1",
		"new_string": "const a = 42",
	})

	if result.Status != StatusOK {
		t.Fatalf("expected edit ok, got %s: %s", result.Status, result.Output)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "const a = 42\nconst b = 2\n" {
		t.Fatalf("unexpected edited content: %q", string(content))
	}
}

func TestEditFileToolReplacesCRLF(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "code.go")
	writeTestFile(t, path, "const a = 1\r\nconst b = 2\r\n")

	result := NewScopedEditFileTool(root, nil).Run(context.Background(), map[string]any{
		"path":       "code.go",
		"old_string": "const a = 1\nconst b = 2",
		"new_string": "const a = 42\nconst b = 24",
	})

	if result.Status != StatusOK {
		t.Fatalf("expected edit ok, got %s: %s", result.Status, result.Output)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "const a = 42\r\nconst b = 24\r\n" {
		t.Fatalf("unexpected edited content: %q", string(content))
	}
}

func TestEditFileToolEmitsUnifiedDiff(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "code.go")
	writeTestFile(t, path, "const a = 1\nconst b = 2\n")
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	res := NewScopedEditFileTool(root, nil).Run(context.Background(), map[string]any{
		"path": "code.go", "old_string": "const a = 1", "new_string": "const a = 42",
	})
	if res.Status != StatusOK {
		t.Fatalf("edit failed: %s", res.Output)
	}
	// The model-facing Output stays the one-line summary; the red/green diff lives
	// on the card-only Display.Preview, so it costs the model zero tokens.
	if !strings.HasPrefix(res.Output, "Successfully edited") {
		t.Fatalf("summary must be the Output: %q", res.Output)
	}
	if strings.Contains(res.Output, "@@") {
		t.Fatalf("Output must NOT carry the diff (card-only preview): %q", res.Output)
	}
	for _, want := range []string{"@@", "-const a = 1", "+const a = 42"} {
		if !strings.Contains(res.Display.Preview, want) {
			t.Fatalf("edit preview missing diff marker %q: %q", want, res.Display.Preview)
		}
	}
	if got := res.FileDiffs; len(got) != 1 || got[0].Path != path || !got[0].OldExists || !got[0].NewExists || got[0].OldText != "const a = 1\nconst b = 2\n" || got[0].NewText != "const a = 42\nconst b = 2\n" {
		t.Fatalf("file diffs = %#v", got)
	}
}

func TestWriteFileToolEmitsAdditionsDiff(t *testing.T) {
	root := t.TempDir()
	res := NewScopedWriteFileTool(root, nil).Run(context.Background(), map[string]any{
		"path": "new.txt", "content": "line one\nline two\n",
	})
	if res.Status != StatusOK {
		t.Fatalf("write failed: %s", res.Output)
	}
	if strings.Contains(res.Output, "@@") {
		t.Fatalf("Output must stay summary-only (the diff is card-only): %q", res.Output)
	}
	for _, want := range []string{"@@", "+line one", "+line two"} {
		if !strings.Contains(res.Display.Preview, want) {
			t.Fatalf("new-file preview missing additions diff %q: %q", want, res.Display.Preview)
		}
	}
	if strings.Contains(res.Display.Preview, "\n-line") {
		t.Fatalf("a fresh-create diff must have no removed lines: %q", res.Display.Preview)
	}
	path, err := filepath.EvalSymlinks(filepath.Join(root, "new.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := res.FileDiffs; len(got) != 1 || got[0].Path != path || got[0].OldExists || !got[0].NewExists || got[0].OldText != "" || got[0].NewText != "line one\nline two\n" {
		t.Fatalf("file diffs = %#v", got)
	}
}

func TestWriteFileToolOverwriteEmitsRedGreenDiff(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "f.txt"), "old line\nkeep\n")
	res := NewScopedWriteFileTool(root, nil).Run(context.Background(), map[string]any{
		"path": "f.txt", "content": "new line\nkeep\n", "overwrite": true,
	})
	if res.Status != StatusOK {
		t.Fatalf("overwrite failed: %s", res.Output)
	}
	if strings.Contains(res.Output, "@@") {
		t.Fatalf("Output must stay summary-only (the diff is card-only): %q", res.Output)
	}
	for _, want := range []string{"-old line", "+new line"} {
		if !strings.Contains(res.Display.Preview, want) {
			t.Fatalf("overwrite preview missing %q: %q", want, res.Display.Preview)
		}
	}
}

func TestWriteFileToolOmitsDiffWhenOverwritePreimageCannotBeRead(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "private.txt")
	writeTestFile(t, path, "before\n")
	tool := NewScopedWriteFileTool(root, nil).(writeFileTool)
	tool.readFile = func(string) ([]byte, error) { return nil, os.ErrPermission }
	registry := NewRegistry()
	registry.Register(tool)
	result := registry.RunWithOptions(context.Background(), tool.Name(), map[string]any{
		"path": "private.txt", "content": "after\n", "overwrite": true,
	}, RunOptions{PermissionGranted: true})
	if result.Status != StatusOK {
		t.Fatalf("write = %s", result.Output)
	}
	if len(result.FileDiffs) != 0 {
		t.Fatalf("unreadable preimage must not produce a create-like diff: %#v", result.FileDiffs)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "after\n" {
		t.Fatalf("written content = %q, err = %v", got, err)
	}
}

func TestEditFileToolAllowsDeletingRegions(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "notes.txt")
	writeTestFile(t, path, "keep\nremove\nkeep\n")

	result := NewScopedEditFileTool(root, nil).Run(context.Background(), map[string]any{
		"path":       "notes.txt",
		"old_string": "remove\n",
		"new_string": "",
	})

	if result.Status != StatusOK {
		t.Fatalf("expected edit ok, got %s: %s", result.Status, result.Output)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "keep\nkeep\n" {
		t.Fatalf("unexpected edited content: %q", string(content))
	}
}

func TestEditFileToolRejectsMissingAndAmbiguousMatches(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "dup.txt")
	writeTestFile(t, path, "x\nx\n")
	tool := NewScopedEditFileTool(root, nil)

	missing := tool.Run(context.Background(), map[string]any{
		"path":       "dup.txt",
		"old_string": "missing",
		"new_string": "y",
	})
	if missing.Status != StatusError || !strings.Contains(missing.Output, "Could not find") {
		t.Fatalf("expected missing error, got %s: %s", missing.Status, missing.Output)
	}

	ambiguous := tool.Run(context.Background(), map[string]any{
		"path":       "dup.txt",
		"old_string": "x",
		"new_string": "y",
	})
	if ambiguous.Status != StatusError || !strings.Contains(ambiguous.Output, "matches 2 locations") {
		t.Fatalf("expected ambiguity error, got %s: %s", ambiguous.Status, ambiguous.Output)
	}

	all := tool.Run(context.Background(), map[string]any{
		"path":        "dup.txt",
		"old_string":  "x",
		"new_string":  "y",
		"replace_all": true,
	})
	if all.Status != StatusOK {
		t.Fatalf("expected replace_all ok, got %s: %s", all.Status, all.Output)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "y\ny\n" {
		t.Fatalf("expected all replacements, got %q", string(content))
	}
}

func TestApplyPatchToolAppliesUnifiedDiff(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "hello.txt"), "hello\nold\n")
	patch := strings.Join([]string{
		"diff --git a/hello.txt b/hello.txt",
		"--- a/hello.txt",
		"+++ b/hello.txt",
		"@@ -1,2 +1,2 @@",
		" hello",
		"-old",
		"+new",
		"",
	}, "\n")

	result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{
		"patch": patch,
	})

	if result.Status != StatusOK {
		t.Fatalf("expected patch ok, got %s: %s", result.Status, result.Output)
	}
	content, err := os.ReadFile(filepath.Join(root, "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.ReplaceAll(string(content), "\r\n", "\n") != "hello\nnew\n" {
		t.Fatalf("unexpected patched content: %q", string(content))
	}
}

func TestApplyPatchToolAppliesStructuredPatch(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "hello.txt"), "hello\nold\n")

	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: hello.txt",
		"@@",
		" hello",
		"-old",
		"+new",
		"*** End Patch",
		"",
	}, "\n")

	result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": patch})
	if result.Status != StatusOK {
		t.Fatalf("structured patch should apply, got %s: %s", result.Status, result.Output)
	}
	content, err := os.ReadFile(filepath.Join(root, "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.ReplaceAll(string(content), "\r\n", "\n"); got != "hello\nnew\n" {
		t.Fatalf("structured patch content = %q", got)
	}
}

func TestApplyPatchToolAppliesStructuredAddAndMove(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "old.txt"), "old\n")
	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Add File: nested/new.txt",
		"+created",
		"*** Update File: old.txt",
		"*** Move to: moved.txt",
		"@@",
		"-old",
		"+moved",
		"*** End Patch",
		"",
	}, "\n")

	result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": patch})
	if result.Status != StatusOK {
		t.Fatalf("structured patch should apply, got %s: %s", result.Status, result.Output)
	}
	if got, err := os.ReadFile(filepath.Join(root, "nested", "new.txt")); err != nil || string(got) != "created\n" {
		t.Fatalf("added file = %q, err = %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "moved.txt")); err != nil || string(got) != "moved\n" {
		t.Fatalf("moved file = %q, err = %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, "old.txt")); !os.IsNotExist(err) {
		t.Fatalf("old source should be removed, stat err = %v", err)
	}
	if got := result.ChangedFiles; strings.Join(got, ",") != "nested/new.txt,old.txt,moved.txt" {
		t.Fatalf("ChangedFiles = %v", got)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := result.FileDiffs, []FileDiff{
		{Path: filepath.Join(resolvedRoot, "nested", "new.txt"), OldExists: false, NewExists: true, OldText: "", NewText: "created\n"},
		{Path: filepath.Join(resolvedRoot, "old.txt"), OldExists: true, NewExists: false, OldText: "old\n", NewText: ""},
		{Path: filepath.Join(resolvedRoot, "moved.txt"), OldExists: false, NewExists: true, OldText: "", NewText: "moved\n"},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("FileDiffs = %#v, want %#v", got, want)
	}
}

func TestApplyPatchToolStructuredPatchMatchesWhitespaceTolerantly(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "hello.txt"), "  hello   \n")
	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: hello.txt",
		"@@",
		"-hello",
		"+goodbye",
		"*** End Patch",
		"",
	}, "\n")

	result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": patch})
	if result.Status != StatusOK {
		t.Fatalf("structured patch should tolerate surrounding whitespace, got %s: %s", result.Status, result.Output)
	}
	content, err := os.ReadFile(filepath.Join(root, "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(content); got != "goodbye\n" {
		t.Fatalf("structured patch content = %q", got)
	}
}

func TestApplyPatchToolStructuredPatchPreservesWhitespaceTolerantContext(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "example.go")
	original := "func f() {\n\tif cond {\n\t\tdoWork()\n\t\tlogIt()\n\t}\n}\n"
	writeTestFile(t, path, original)
	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: example.go",
		"@@",
		"     if cond {",
		"         doWork()",
		"-\t\tlogIt()",
		"+\t\tlogIt(ctx)",
		"     }",
		"*** End Patch",
		"",
	}, "\n")

	result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": patch})
	if result.Status != StatusOK {
		t.Fatalf("structured patch failed: %s", result.Output)
	}
	want := "func f() {\n\tif cond {\n\t\tdoWork()\n\t\tlogIt(ctx)\n\t}\n}\n"
	if got := mustReadTestFile(t, path); got != want {
		t.Fatalf("structured patch rewrote unchanged context:\n got %q\nwant %q", got, want)
	}
}

func TestApplyPatchToolStructuredPatchPreservesMixedContextLineEndings(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "example.txt")
	original := "alpha\r\n\tcontext\r\n\told\nomega\r\n"
	writeTestFile(t, path, original)
	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: example.txt",
		"@@",
		" alpha",
		"     context",
		"-\told",
		"+\tnew",
		" omega",
		"*** End Patch",
		"",
	}, "\n")

	result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": patch})
	if result.Status != StatusOK {
		t.Fatalf("structured patch failed: %s", result.Output)
	}
	want := "alpha\r\n\tcontext\r\n\tnew\nomega\r\n"
	if got := mustReadTestFile(t, path); got != want {
		t.Fatalf("structured patch rewrote context line endings:\n got %q\nwant %q", got, want)
	}
}

func TestApplyPatchToolStructuredPatchInsertsAtContext(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "example.txt"), "anchor\nmiddle\nremove\n")
	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: example.txt",
		"@@ anchor",
		"+inserted",
		"@@ middle",
		"-remove",
		"*** End Patch",
		"",
	}, "\n")

	result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": patch})
	if result.Status != StatusOK {
		t.Fatalf("structured insertion failed: %s", result.Output)
	}
	content, err := os.ReadFile(filepath.Join(root, "example.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(content), "anchor\ninserted\nmiddle\n"; got != want {
		t.Fatalf("structured insertion content = %q, want %q", got, want)
	}
}

func TestApplyPatchToolStructuredPatchRejectsAmbiguousMatch(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "example.go")
	original := "func a() {\n\treturn nil\n}\n\nfunc b() {\n\treturn nil\n}\n"
	writeTestFile(t, path, original)
	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: example.go",
		"@@",
		"-\treturn nil",
		"+\treturn errNotFound",
		"*** End Patch",
		"",
	}, "\n")

	result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": patch})
	if result.Status != StatusError || !strings.Contains(strings.ToLower(result.Output), "ambiguous") {
		t.Fatalf("ambiguous structured patch should be refused, got %s: %s", result.Status, result.Output)
	}
	if got := mustReadTestFile(t, path); got != original {
		t.Fatalf("ambiguous structured patch changed the file: %q", got)
	}
}

func TestApplyPatchToolStructuredPatchPreservesCRLF(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "example.txt")
	writeTestFile(t, path, "alpha\r\nbravo\r\ncharlie\r\n")
	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: example.txt",
		"@@",
		"-bravo",
		"+BRAVO",
		"*** End Patch",
		"",
	}, "\n")

	result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": patch})
	if result.Status != StatusOK {
		t.Fatalf("structured patch failed: %s", result.Output)
	}
	if got, want := mustReadTestFile(t, path), "alpha\r\nBRAVO\r\ncharlie\r\n"; got != want {
		t.Fatalf("structured patch content = %q, want %q", got, want)
	}
}

func TestApplyPatchToolStructuredPatchPreservesMissingFinalNewline(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "example.txt")
	writeTestFile(t, path, "alpha\nbravo")
	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: example.txt",
		"@@",
		"-alpha",
		"+ALPHA",
		"*** End Patch",
		"",
	}, "\n")

	result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": patch})
	if result.Status != StatusOK {
		t.Fatalf("structured patch failed: %s", result.Output)
	}
	if got, want := mustReadTestFile(t, path), "ALPHA\nbravo"; got != want {
		t.Fatalf("structured patch content = %q, want %q", got, want)
	}
}

func TestApplyPatchToolStructuredPatchKeepsInsertedEmptyFileWithoutFinalNewline(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "empty.txt")
	writeTestFile(t, path, "")
	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: empty.txt",
		"@@",
		"+content",
		"*** End Patch",
		"",
	}, "\n")

	result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": patch})
	if result.Status != StatusOK {
		t.Fatalf("structured patch failed: %s", result.Output)
	}
	if got, want := mustReadTestFile(t, path), "content"; got != want {
		t.Fatalf("structured patch content = %q, want %q", got, want)
	}
}

func TestApplyPatchToolStructuredPatchRejectsOutOfOrderEndHunk(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "example.txt")
	original := "alpha\nbravo\ncharlie\ndelta\necho\n"
	writeTestFile(t, path, original)
	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: example.txt",
		"@@",
		"-delta",
		"+DELTA",
		"@@",
		"-delta",
		"-echo",
		"+tail",
		"*** End of File",
		"*** End Patch",
		"",
	}, "\n")

	result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": patch})
	if result.Status != StatusError {
		t.Fatalf("out-of-order structured patch should be refused, got %s: %s", result.Status, result.Output)
	}
	if got := mustReadTestFile(t, path); got != original {
		t.Fatalf("out-of-order structured patch changed the file: %q", got)
	}
}

func TestStructuredPatchAddDoesNotOverwriteRacedDestination(t *testing.T) {
	root := t.TempDir()
	targetPath := filepath.Join(root, "created.txt")
	writeTestFile(t, targetPath, "other writer\n")
	workspace, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	change := structuredPatchChange{
		kind:  structuredPatchAdd,
		to:    structuredPatchTarget{absolute: targetPath, relative: "created.txt"},
		after: "patch content\n", mode: 0o644,
	}

	_, err = applyStructuredPatchChanges(workspace, ".", []structuredPatchChange{change}, nil)
	if err == nil || !errors.Is(err, os.ErrExist) {
		t.Fatalf("raced add destination = %v, want os.ErrExist", err)
	}
	if strings.Contains(err.Error(), "exclusive-copy fallback") {
		t.Fatalf("existing destination should be reported directly: %v", err)
	}
	if got := mustReadTestFile(t, targetPath); got != "other writer\n" {
		t.Fatalf("raced add overwrote another writer's file: %q", got)
	}
}

func TestStructuredPatchFailedDeleteDoesNotRecreateMissingFile(t *testing.T) {
	root := t.TempDir()
	workspace, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	missingPath := filepath.Join(root, "missing.txt")
	change := structuredPatchChange{
		kind:   structuredPatchDelete,
		from:   structuredPatchTarget{absolute: missingPath, relative: "missing.txt"},
		before: "removed by another writer\n", mode: 0o644,
	}

	_, err = applyStructuredPatchChanges(workspace, ".", []structuredPatchChange{change}, nil)
	if err == nil {
		t.Fatal("delete of an already removed file should fail")
	}
	if _, statErr := os.Stat(missingPath); !os.IsNotExist(statErr) {
		t.Fatalf("failed delete recreated the missing file: %v", statErr)
	}
}

// A move whose source vanishes after planning is refused by the pre-commit
// recheck before the destination is published, so nothing is left half-done.
func TestStructuredPatchMoveWithMissingSourceIsRefusedBeforePublishing(t *testing.T) {
	root := t.TempDir()
	workspace, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	sourcePath := filepath.Join(root, "source.txt")
	destinationPath := filepath.Join(root, "destination.txt")
	writeTestFile(t, sourcePath, "removed source\n")
	change := structuredPatchChange{
		kind:   structuredPatchUpdate,
		from:   structuredPatchTarget{absolute: sourcePath, relative: "source.txt"},
		to:     structuredPatchTarget{absolute: destinationPath, relative: "destination.txt"},
		before: "removed source\n", after: "moved content\n", mode: 0o644,
	}
	// The source exists at planning time and disappears just before commit.
	removed := false
	structuredPatchBeforeCommit = func(structuredPatchChange) {
		if err := os.Remove(sourcePath); err != nil {
			t.Fatal(err)
		}
		removed = true
	}
	defer func() { structuredPatchBeforeCommit = nil }()

	_, err = applyStructuredPatchChanges(workspace, ".", []structuredPatchChange{change}, nil)
	if !removed {
		t.Fatal("pre-commit hook did not run")
	}
	if err == nil || !strings.Contains(err.Error(), "before commit") || strings.Contains(err.Error(), "partially applied") {
		t.Fatalf("move with a missing source = %v, want a pre-commit refusal with nothing applied", err)
	}
	if _, statErr := os.Stat(sourcePath); !os.IsNotExist(statErr) {
		t.Fatalf("failed move recreated a missing source: %v", statErr)
	}
	if _, statErr := os.Stat(destinationPath); !os.IsNotExist(statErr) {
		t.Fatalf("refused move must not publish the destination: %v", statErr)
	}
}

func TestStructuredPatchUpdatePreservesCompetingWriteBeforeRename(t *testing.T) {
	for _, tc := range []struct {
		name            string
		competing       string
		replaceIdentity bool
	}{
		{name: "content changed", competing: "competing writer\n"},
		{name: "identity changed with same content", competing: "planned\n", replaceIdentity: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			targetPath := filepath.Join(root, "file.txt")
			writeTestFile(t, targetPath, "planned\n")
			patch := "*** Begin Patch\n*** Update File: file.txt\n@@\n-planned\n+patched\n*** End Patch\n"
			hookRan := false
			var competingInfo os.FileInfo
			structuredPatchBeforeRename = func(change structuredPatchChange) {
				if tc.replaceIdentity {
					tempPath := filepath.Join(root, "competing.tmp")
					writeTestFile(t, tempPath, tc.competing)
					if err := os.Rename(tempPath, targetPath); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(targetPath, []byte(tc.competing), 0o644); err != nil {
					t.Fatal(err)
				}
				var err error
				competingInfo, err = os.Stat(targetPath)
				if err != nil {
					t.Fatal(err)
				}
				hookRan = true
			}
			t.Cleanup(func() { structuredPatchBeforeRename = nil })

			result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": patch})
			if !hookRan {
				t.Fatal("pre-rename hook did not run")
			}
			if result.Status != StatusError || len(result.ChangedFiles) != 0 || len(result.FileDiffs) != 0 {
				t.Fatalf("raced update = status=%s changed=%#v diffs=%#v output=%q", result.Status, result.ChangedFiles, result.FileDiffs, result.Output)
			}
			if got := mustReadTestFile(t, targetPath); got != tc.competing {
				t.Fatalf("raced update overwrote competing content: %q", got)
			}
			finalInfo, err := os.Stat(targetPath)
			if err != nil || !os.SameFile(competingInfo, finalInfo) {
				t.Fatalf("raced update replaced the competing file identity: %v", err)
			}
		})
	}
}

func TestStructuredPatchCreateOnlyFallsBackWhenHardLinksUnsupported(t *testing.T) {
	root := t.TempDir()
	workspace, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	sourcePath := filepath.Join(root, "source.tmp")
	writeTestFile(t, sourcePath, "complete content\n")
	if err := workspace.Chmod("source.tmp", 0o444); err != nil {
		t.Fatal(err)
	}
	committed, err := publishStructuredPatchNoReplaceWith(
		workspace,
		"source.tmp",
		"target.txt",
		0o444,
		func(string, string) error { return errors.New("hard links unsupported") },
	)
	if err != nil || !committed {
		t.Fatalf("fallback publish = committed %v, error %v", committed, err)
	}
	if got := mustReadTestFile(t, filepath.Join(root, "target.txt")); got != "complete content\n" {
		t.Fatalf("fallback target content = %q", got)
	}
	if _, statErr := os.Stat(sourcePath); !os.IsNotExist(statErr) {
		t.Fatalf("read-only source was not cleaned up: %v", statErr)
	}
}

func TestStructuredPatchCopyFailureReportsSurvivingPartialTarget(t *testing.T) {
	root := t.TempDir()
	workspace, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	sourcePath := filepath.Join(root, "source.tmp")
	writeTestFile(t, sourcePath, "complete content\n")
	copyErr := errors.New("injected copy failure")
	cleanupErr := errors.New("injected target cleanup failure")
	committed, err := copyStructuredPatchNoReplaceWith(
		workspace,
		"source.tmp",
		"target.txt",
		0o644,
		func(destination io.Writer, _ io.Reader) (int64, error) {
			written, writeErr := io.WriteString(destination, "partial")
			if writeErr != nil {
				return int64(written), writeErr
			}
			return int64(written), copyErr
		},
		func(root *os.Root, name string) error {
			if name == "target.txt" {
				return cleanupErr
			}
			return removeStructuredPatchTemp(root, name)
		},
	)
	if !committed || !errors.Is(err, copyErr) || !errors.Is(err, cleanupErr) {
		t.Fatalf("copy failure = committed %v, error %v", committed, err)
	}
	targetPath := filepath.Join(root, "target.txt")
	if got := mustReadTestFile(t, targetPath); got != "partial" {
		t.Fatalf("partial target content = %q", got)
	}
}

func TestIncompleteStructuredPatchNoReplaceWriteReportsOnlyVerifiedTarget(t *testing.T) {
	for name, content := range map[string]string{
		"complete publication": "complete content\n",
		"partial publication":  "partial",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			workspace, err := os.OpenRoot(root)
			if err != nil {
				t.Fatal(err)
			}
			defer workspace.Close()
			targetPath := filepath.Join(root, "target.txt")
			writeTestFile(t, targetPath, content)
			change := structuredPatchChange{
				kind: structuredPatchAdd,
				to: structuredPatchTarget{
					requested: "target.txt",
					relative:  "target.txt",
					absolute:  targetPath,
				},
				after: "complete content\n",
				mode:  0o644,
			}

			outcome := incompleteStructuredPatchWrite(workspace, change, true)
			if content == change.after {
				if len(outcome.completed) != 1 || outcome.completed[0].kind != structuredPatchAdd || len(outcome.incompletePaths) != 0 {
					t.Fatalf("verified publication outcome = %#v", outcome)
				}
				return
			}
			if len(outcome.completed) != 0 || !slices.Equal(outcome.incompletePaths, []string{"target.txt"}) {
				t.Fatalf("partial publication outcome = %#v", outcome)
			}
		})
	}
}

func TestStructuredPatchPartialFailureLeavesCompletedChangeAndClearsTrackedState(t *testing.T) {
	root := t.TempDir()
	trackedPath := filepath.Join(root, "tracked.txt")
	writeTestFile(t, trackedPath, "original\n")
	workspace, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()

	trackedPath, err = filepath.EvalSymlinks(trackedPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(trackedPath)
	if err != nil {
		t.Fatal(err)
	}
	tracker := NewFileTracker()
	tracker.Record(trackedPath, []byte("original\n"), info)
	tracker.RecordSeenRange(trackedPath, 1, 1, 1)
	trackedTarget := structuredPatchTarget{absolute: trackedPath, relative: "tracked.txt"}
	blockedPath := filepath.Join(root, "blocked")
	if err := os.Mkdir(blockedPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(blockedPath, "sentinel"), "keep\n")
	changes := []structuredPatchChange{
		{
			kind: structuredPatchUpdate, from: trackedTarget, to: trackedTarget,
			before: "original\n", after: "updated\n", mode: 0o644,
		},
		{
			kind:  structuredPatchAdd,
			to:    structuredPatchTarget{absolute: blockedPath, relative: "blocked"},
			after: "cannot replace a non-empty directory\n", mode: 0o644,
		},
	}

	_, err = applyStructuredPatchChanges(workspace, ".", changes, tracker)
	if err == nil || !strings.Contains(err.Error(), "partially applied") {
		t.Fatalf("second change = %v, want partial-application error", err)
	}
	if got := mustReadTestFile(t, trackedPath); got != "updated\n" {
		t.Fatalf("completed change was unexpectedly altered: %q", got)
	}
	if _, tracked := tracker.Version(trackedPath); tracked || tracker.SeenWhole(trackedPath) {
		t.Fatal("partial failure retained stale file-tracker state")
	}
}

func TestApplyPatchToolRejectsMalformedOrEscapingStructuredPatchBeforeWriting(t *testing.T) {
	root := t.TempDir()
	tool := NewScopedApplyPatchTool(root, nil)
	for name, patch := range map[string]string{
		"empty update": strings.Join([]string{
			"*** Begin Patch",
			"*** Update File: target.txt",
			"*** End Patch",
		}, "\n"),
		"escape": strings.Join([]string{
			"*** Begin Patch",
			"*** Add File: ../outside.txt",
			"+nope",
			"*** End Patch",
		}, "\n"),
	} {
		t.Run(name, func(t *testing.T) {
			result := tool.Run(context.Background(), map[string]any{"patch": patch})
			if result.Status != StatusError {
				t.Fatalf("expected error, got %s: %s", result.Status, result.Output)
			}
			if strings.Contains(result.Output, "No valid patches") {
				t.Fatalf("structured patch should fail before git apply, got %q", result.Output)
			}
			if name == "empty update" {
				if _, err := os.Stat(filepath.Join(root, "target.txt")); !os.IsNotExist(err) {
					t.Fatalf("empty update must not write target file, stat err = %v", err)
				}
			}
		})
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "outside.txt")); !os.IsNotExist(err) {
		t.Fatalf("escaping patch must not write outside workspace, stat err = %v", err)
	}
}

// A hunk-body line that removes content beginning with "-- " appears in the diff
// as "--- ..."; it must NOT be mistaken for a file header (which previously made
// apply_patch reject a valid patch as targeting an outside path).
func TestApplyPatchToolHandlesHunkBodyLookingLikeHeader(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "notes.md"), "keep\n-- /etc/old\n")
	patch := strings.Join([]string{
		"diff --git a/notes.md b/notes.md",
		"--- a/notes.md",
		"+++ b/notes.md",
		"@@ -1,2 +1,1 @@",
		" keep",
		"--- /etc/old",
		"",
	}, "\n")

	result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": patch})

	if result.Status != StatusOK {
		t.Fatalf("expected patch ok (hunk body must not be parsed as a header), got %s: %s", result.Status, result.Output)
	}
	content, err := os.ReadFile(filepath.Join(root, "notes.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.ReplaceAll(string(content), "\r\n", "\n") != "keep\n" {
		t.Fatalf("unexpected patched content: %q", string(content))
	}
}

func TestApplyPatchToolRejectsHunkCountInflationHidingEscapePath(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "safe.txt"), "old\n")
	// A crafted section heading after the closing "@@" injects a "+9,999999"
	// token. If parseHunkCounts scanned the whole line it would treat 999999 as
	// the new-line count, stay stuck in hunk mode, and swallow the second file
	// header below — hiding the ../escape.txt write from path validation.
	patch := strings.Join([]string{
		"diff --git a/safe.txt b/safe.txt",
		"--- a/safe.txt",
		"+++ b/safe.txt",
		"@@ -1,1 +1,1 @@ +9,999999",
		"-old",
		"+new",
		"--- a/../escape.txt",
		"+++ b/../escape.txt",
		"@@ -1,1 +1,1 @@",
		"-secret",
		"+pwned",
		"",
	}, "\n")

	result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": patch})

	if result.Status != StatusError {
		t.Fatalf("crafted hunk header must not hide the out-of-workspace path, got %s: %s", result.Status, result.Output)
	}
	if !strings.Contains(result.Output, "must stay inside the workspace") {
		t.Fatalf("expected workspace-confinement rejection, got %q", result.Output)
	}
}

func TestApplyPatchToolRejectsSymlinkPath(t *testing.T) {
	root := t.TempDir()
	realDirectory := filepath.Join(root, "real")
	if err := os.MkdirAll(realDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDirectory, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	patch := strings.Join([]string{
		"diff --git a/link/new.txt b/link/new.txt",
		"new file mode 100644",
		"index 0000000..e965047",
		"--- /dev/null",
		"+++ b/link/new.txt",
		"@@ -0,0 +1 @@",
		"+hello",
		"",
	}, "\n")

	result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{
		"patch": patch,
	})

	if result.Status != StatusError {
		t.Fatalf("expected error status, got %s", result.Status)
	}
	if !strings.Contains(result.Output, "must not traverse symlink") {
		t.Fatalf("expected symlink error, got %q", result.Output)
	}
	if _, err := os.Stat(filepath.Join(realDirectory, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected symlink target file to remain absent, stat err=%v", err)
	}
}

func TestApplyPatchToolRejectsOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{
		"cwd": outside,
		"patch": strings.Join([]string{
			"diff --git a/nope.txt b/nope.txt",
			"--- a/nope.txt",
			"+++ b/nope.txt",
			"@@ -0,0 +1 @@",
			"+nope",
			"",
		}, "\n"),
	})

	if result.Status != StatusError {
		t.Fatalf("expected error status, got %s", result.Status)
	}
	if !strings.Contains(result.Output, "must stay inside the workspace") {
		t.Fatalf("expected workspace error, got %q", result.Output)
	}
}

// Finding 3: apply_patch with cwd != "." must report WORKSPACE-relative
// ChangedFiles (cwd-prefixed), not cwd-relative paths. Otherwise the session's
// rewind/diff layer keys off the wrong path.
func TestApplyPatchReportsWorkspaceRelativeChangedFilesUnderCwd(t *testing.T) {
	root := t.TempDir()
	subdir := filepath.Join(root, "sub", "dir")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subdir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := "--- a/a.txt\n+++ b/a.txt\n@@ -1 +1 @@\n-one\n+two\n"

	res := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": patch, "cwd": "sub/dir"})
	if res.Status != StatusOK {
		if gitApplyUnavailable(res.Output) {
			t.Skipf("git binary unavailable: %s", res.Output)
		}
		t.Fatalf("apply_patch with cwd failed (possible regression): %s", res.Output)
	}
	if len(res.ChangedFiles) != 1 || res.ChangedFiles[0] != "sub/dir/a.txt" {
		t.Fatalf("ChangedFiles = %v, want [sub/dir/a.txt]", res.ChangedFiles)
	}
}

func TestWriteFileReportsChangedFileAndDisplay(t *testing.T) {
	root := t.TempDir()
	res := NewScopedWriteFileTool(root, nil).Run(context.Background(), map[string]any{"path": "notes.txt", "content": "hello"})
	if res.Status != StatusOK {
		t.Fatalf("status=%s output=%s", res.Status, res.Output)
	}
	if len(res.ChangedFiles) != 1 || res.ChangedFiles[0] != "notes.txt" {
		t.Fatalf("ChangedFiles = %v, want [notes.txt]", res.ChangedFiles)
	}
	if res.Display.Kind != "file" {
		t.Errorf("Display.Kind = %q, want file", res.Display.Kind)
	}
	if res.Display.Summary == "" {
		t.Error("expected a non-empty Display.Summary")
	}
}

func TestEditFileReportsChangedFileAndDisplay(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("alpha beta"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := NewScopedEditFileTool(root, nil).Run(context.Background(), map[string]any{"path": "f.txt", "old_string": "alpha", "new_string": "gamma"})
	if res.Status != StatusOK {
		t.Fatalf("status=%s output=%s", res.Status, res.Output)
	}
	if len(res.ChangedFiles) != 1 || res.ChangedFiles[0] != "f.txt" {
		t.Fatalf("ChangedFiles = %v, want [f.txt]", res.ChangedFiles)
	}
	if res.Display.Kind != "diff" {
		t.Errorf("Display.Kind = %q, want diff", res.Display.Kind)
	}
}

func TestApplyPatchReportsChangedFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := "--- a/a.txt\n+++ b/a.txt\n@@ -1 +1 @@\n-one\n+two\n"
	res := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": patch})
	if res.Status != StatusOK {
		if gitApplyUnavailable(res.Output) {
			t.Skipf("git binary unavailable: %s", res.Output)
		}
		t.Fatalf("apply_patch failed (possible regression): %s", res.Output)
	}
	found := false
	for _, f := range res.ChangedFiles {
		if f == "a.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a.txt in ChangedFiles, got %v", res.ChangedFiles)
	}
	if res.Display.Kind != "diff" {
		t.Errorf("Display.Kind = %q, want diff", res.Display.Kind)
	}
}

func TestWriteFileAcceptsContentAlias(t *testing.T) {
	root := t.TempDir()
	// minimax-style: content under an alias key instead of "content".
	res := NewScopedWriteFileTool(root, nil).Run(context.Background(), map[string]any{
		"path":     "shop.html",
		"contents": "<html>hi</html>",
	})
	if res.Status != StatusOK {
		t.Fatalf("alias content should write, got %s: %s", res.Status, res.Output)
	}
	got, _ := os.ReadFile(filepath.Join(root, "shop.html"))
	if string(got) != "<html>hi</html>" {
		t.Fatalf("file content = %q", got)
	}
}

// gitApplyUnavailable reports whether an apply_patch failure is due to the git
// binary being absent (an environment condition worth skipping) rather than a
// real regression (which must fail the test). apply_patch shells out to
// `git apply`; a missing binary surfaces as exec's "executable file not found".
func gitApplyUnavailable(output string) bool {
	return strings.Contains(output, "executable file not found")
}
