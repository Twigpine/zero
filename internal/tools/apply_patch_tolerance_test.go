package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Models routinely decorate the structured markers ("*** Begin Patch ***") and
// write unified-diff ranges after "@@"; both used to fail on the first line and
// pushed the model into whole-file rewrites.
func TestApplyPatchToleratesDecoratedMarkersAndRangeHeaders(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "hello.txt"), "hello\nold\nbye\n")

	patch := strings.Join([]string{
		"*** Begin Patch ***",
		"*** Update File: hello.txt",
		"@@ -1,3 +1,3 @@",
		" hello",
		"-old",
		"+new",
		"*** End Patch ***",
		"",
	}, "\n")

	if !isStructuredPatch(patch) {
		t.Fatal("decorated begin marker must still be recognised as a structured patch")
	}
	result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": patch})
	if result.Status != StatusOK {
		t.Fatalf("decorated structured patch should apply, got %s: %s", result.Status, result.Output)
	}
	content, err := os.ReadFile(filepath.Join(root, "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.ReplaceAll(string(content), "\r\n", "\n"); got != "hello\nnew\nbye\n" {
		t.Fatalf("patched content = %q", got)
	}
}

func TestStructuredHunkAnchorKeepsHeadingAndPlainContext(t *testing.T) {
	cases := map[string]string{
		"-12,4 +12,6":                  "",
		"-12 +12":                      "",
		"-12,4 +12,6 @@":               "",
		"-12,4 +12,6 @@ func main() {": "func main() {",
		"func main() {":                "func main() {",
		"":                             "",
	}
	for input, want := range cases {
		if got := structuredHunkAnchor(input); got != want {
			t.Fatalf("structuredHunkAnchor(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestStructuredPatchMarkerSpellings(t *testing.T) {
	begin := []string{"*** Begin Patch", "*** Begin Patch ***", "***Begin Patch", "  *** Begin Patch  ", "*** Begin Patch **"}
	for _, line := range begin {
		if structuredPatchMarker(line) != "begin" {
			t.Fatalf("%q must read as the begin marker", line)
		}
	}
	if structuredPatchMarker("*** End Patch ***") != "end" {
		t.Fatal("decorated end marker must read as the end marker")
	}
	for _, line := range []string{"*** Update File: x", "Begin Patch", "*** Begin Patchwork"} {
		if structuredPatchMarker(line) != "" {
			t.Fatalf("%q must not read as a marker", line)
		}
	}
}

func TestStructuredPatchHeaderErrorCarriesFormatHint(t *testing.T) {
	_, err := parseStructuredPatch("--- a/x\n+++ b/x\n")
	if err == nil || !strings.Contains(err.Error(), "*** Update File: path") {
		t.Fatalf("header error should teach the format, got %v", err)
	}
}

func TestApplyPatchAcceptsAbsoluteInWorkspacePaths(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "hello.txt"), "hello\nold\n")
	absolute := filepath.Join(root, "hello.txt")

	structured := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: " + absolute,
		"@@",
		" hello",
		"-old",
		"+structured",
		"*** End Patch",
		"",
	}, "\n")
	result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": structured})
	if result.Status != StatusOK {
		t.Fatalf("absolute in-workspace structured path should apply, got %s: %s", result.Status, result.Output)
	}
	content, _ := os.ReadFile(absolute)
	if got := strings.ReplaceAll(string(content), "\r\n", "\n"); got != "hello\nstructured\n" {
		t.Fatalf("structured content = %q", got)
	}
	if len(result.ChangedFiles) != 1 || result.ChangedFiles[0] != "hello.txt" {
		t.Fatalf("changed files should be workspace-relative, got %v", result.ChangedFiles)
	}

	unified := strings.Join([]string{
		"--- " + absolute,
		"+++ " + absolute,
		"@@ -1,2 +1,2 @@",
		" hello",
		"-structured",
		"+unified",
		"",
	}, "\n")
	result = NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": unified})
	if result.Status != StatusOK {
		t.Fatalf("absolute in-workspace unified path should apply, got %s: %s", result.Status, result.Output)
	}
	content, _ = os.ReadFile(absolute)
	if got := strings.ReplaceAll(string(content), "\r\n", "\n"); got != "hello\nunified\n" {
		t.Fatalf("unified content = %q", got)
	}
}

func TestApplyPatchStillRejectsAbsolutePathsOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "escape.txt")
	writeTestFile(t, outside, "old\n")

	for name, patch := range map[string]string{
		"structured": strings.Join([]string{"*** Begin Patch", "*** Update File: " + outside, "@@", "-old", "+new", "*** End Patch", ""}, "\n"),
		"unified":    strings.Join([]string{"--- " + outside, "+++ " + outside, "@@ -1 +1 @@", "-old", "+new", ""}, "\n"),
	} {
		result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": patch})
		if result.Status == StatusOK {
			t.Fatalf("%s patch outside the workspace must be rejected", name)
		}
		if content, _ := os.ReadFile(outside); string(content) != "old\n" {
			t.Fatalf("%s patch must not touch a file outside the workspace", name)
		}
	}
}

// Unified diffs are applied in-process through the same os.Root engine as
// structured patches: git C-quoted paths with whitespace resolve to one file,
// and the same header outside the workspace is refused before anything is
// written.
func TestUnifiedPatchQuotedWhitespacePaths(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "work tree")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "file.txt")
	writeTestFile(t, target, "hello\nold\n")

	quoted := func(path string) string { return strconv.Quote(path) }
	inside := strings.Join([]string{
		"diff --git " + quoted("a/"+filepath.ToSlash(target)) + " " + quoted("b/"+filepath.ToSlash(target)),
		"--- " + quoted(target),
		"+++ " + quoted(target),
		"@@ -1,2 +1,2 @@",
		" hello",
		"-old",
		"+new",
		"",
	}, "\n")
	result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": inside})
	if result.Status != StatusOK {
		t.Fatalf("quoted whitespace path inside the workspace should apply, got %s: %s", result.Status, result.Output)
	}
	content, _ := os.ReadFile(target)
	if got := strings.ReplaceAll(string(content), "\r\n", "\n"); got != "hello\nnew\n" {
		t.Fatalf("content = %q", got)
	}
	if len(result.ChangedFiles) != 1 || result.ChangedFiles[0] != "work tree/file.txt" {
		t.Fatalf("changed files = %v", result.ChangedFiles)
	}

	outsideDir := filepath.Join(t.TempDir(), "other tree")
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(outsideDir, "file.txt")
	writeTestFile(t, outside, "hello\nold\n")
	escape := strings.Join([]string{"--- " + quoted(outside), "+++ " + quoted(outside), "@@ -1,2 +1,2 @@", " hello", "-old", "+new", ""}, "\n")
	result = NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": escape})
	if result.Status == StatusOK {
		t.Fatal("quoted whitespace path outside the workspace must be rejected")
	}
	if content, _ := os.ReadFile(outside); string(content) != "hello\nold\n" {
		t.Fatal("outside file must be untouched")
	}
}

// A workspace path that is swapped for a symlink escaping the root — the
// classic check-to-use attack — is refused by the os.Root engine for both
// formats, and the outside file is never modified.
func TestApplyPatchRefusesSymlinkEscapingWorkspace(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	writeTestFile(t, outside, "hello\nold\n")
	link := filepath.Join(root, "target.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for name, patch := range map[string]string{
		"unified":    strings.Join([]string{"--- a/target.txt", "+++ b/target.txt", "@@ -1,2 +1,2 @@", " hello", "-old", "+new", ""}, "\n"),
		"structured": strings.Join([]string{"*** Begin Patch", "*** Update File: target.txt", "@@", " hello", "-old", "+new", "*** End Patch", ""}, "\n"),
	} {
		result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": patch})
		if result.Status == StatusOK {
			t.Fatalf("%s patch through an escaping symlink must be refused", name)
		}
		if content, _ := os.ReadFile(outside); string(content) != "hello\nold\n" {
			t.Fatalf("%s patch must not modify the file outside the workspace", name)
		}
	}
}

func TestUnifiedPatchCreatesDeletesAndHonoursNoNewlineMarker(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "gone.txt"), "bye\n")
	writeTestFile(t, filepath.Join(root, "keep.txt"), "a\nb\n")
	patch := strings.Join([]string{
		"diff --git a/new.txt b/new.txt",
		"new file mode 100644",
		"--- /dev/null",
		"+++ b/new.txt",
		"@@ -0,0 +1,2 @@",
		"+first",
		"+second",
		"\\ No newline at end of file",
		"diff --git a/gone.txt b/gone.txt",
		"deleted file mode 100644",
		"--- a/gone.txt",
		"+++ /dev/null",
		"@@ -1 +0,0 @@",
		"-bye",
		"--- a/keep.txt",
		"+++ b/keep.txt",
		"@@ -1,2 +1,2 @@",
		" a",
		"-b",
		"+c",
		"\\ No newline at end of file",
		"",
	}, "\n")
	result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": patch})
	if result.Status != StatusOK {
		t.Fatalf("multi-file unified patch should apply, got %s: %s", result.Status, result.Output)
	}
	if content, _ := os.ReadFile(filepath.Join(root, "new.txt")); string(content) != "first\nsecond" {
		t.Fatalf("created file = %q", string(content))
	}
	if _, err := os.Stat(filepath.Join(root, "gone.txt")); !os.IsNotExist(err) {
		t.Fatal("deleted file must be gone")
	}
	if content, _ := os.ReadFile(filepath.Join(root, "keep.txt")); string(content) != "a\nc" {
		t.Fatalf("updated file = %q", string(content))
	}
	if len(result.ChangedFiles) != 3 {
		t.Fatalf("changed files = %v", result.ChangedFiles)
	}
}

func TestUnifiedPatchUsesRangeHintThenFallsBackToContext(t *testing.T) {
	root := t.TempDir()
	// Two identical blocks: the hunk's range picks the second one, which
	// content search alone would call ambiguous.
	writeTestFile(t, filepath.Join(root, "dup.txt"), "x\ny\nx\ny\n")
	patch := strings.Join([]string{"--- a/dup.txt", "+++ b/dup.txt", "@@ -3,2 +3,2 @@", " x", "-y", "+z", ""}, "\n")
	result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": patch})
	if result.Status != StatusOK {
		t.Fatalf("range-hinted hunk should apply, got %s: %s", result.Status, result.Output)
	}
	if content, _ := os.ReadFile(filepath.Join(root, "dup.txt")); string(content) != "x\ny\nx\nz\n" {
		t.Fatalf("content = %q", string(content))
	}
	// A stale range (file grew above the hunk) still applies by unique context.
	writeTestFile(t, filepath.Join(root, "moved.txt"), "header\nhello\nold\n")
	patch = strings.Join([]string{"--- a/moved.txt", "+++ b/moved.txt", "@@ -1,2 +1,2 @@", " hello", "-old", "+new", ""}, "\n")
	result = NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": patch})
	if result.Status != StatusOK {
		t.Fatalf("offset hunk should apply by context, got %s: %s", result.Status, result.Output)
	}
	if content, _ := os.ReadFile(filepath.Join(root, "moved.txt")); string(content) != "header\nhello\nnew\n" {
		t.Fatalf("content = %q", string(content))
	}
}

func TestUnifiedPatchRenameWithHunkAndRejectsBinary(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "x.txt"), "hello\nold\n")
	rename := strings.Join([]string{
		"diff --git a/x.txt b/y.txt",
		"similarity index 90%",
		"rename from x.txt",
		"rename to y.txt",
		"--- a/x.txt",
		"+++ b/y.txt",
		"@@ -1,2 +1,2 @@",
		" hello",
		"-old",
		"+new",
		"",
	}, "\n")
	result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": rename})
	if result.Status != StatusOK {
		t.Fatalf("rename with hunk should apply, got %s: %s", result.Status, result.Output)
	}
	if _, err := os.Stat(filepath.Join(root, "x.txt")); !os.IsNotExist(err) {
		t.Fatal("renamed source must be gone")
	}
	if content, _ := os.ReadFile(filepath.Join(root, "y.txt")); string(content) != "hello\nnew\n" {
		t.Fatalf("renamed content = %q", string(content))
	}
	for name, patch := range map[string]string{
		"binary": "diff --git a/x b/x\nBinary files a/x and b/x differ\n",
		"empty":  "diff --git a/x b/x\n",
	} {
		if result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": patch}); result.Status == StatusOK {
			t.Fatalf("%s patch must be rejected", name)
		}
	}
}

func TestUnifiedPatchCopyOperation(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "src.txt"), "hello\nold\n")
	copyPatch := func(from, to string, hunk bool) string {
		lines := []string{"diff --git a/" + from + " b/" + to, "similarity index 90%", "copy from " + from, "copy to " + to}
		if hunk {
			lines = append(lines, "--- a/"+from, "+++ b/"+to, "@@ -1,2 +1,2 @@", " hello", "-old", "+new")
		}
		return strings.Join(append(lines, ""), "\n")
	}

	// Success: source kept, destination created with the hunk applied, and the
	// destination inherits the source's tracker state (read whole -> whole).
	tracker := NewFileTracker()
	registry := NewRegistry()
	registry.Register(NewScopedReadFileTool(root, nil))
	registry.Register(NewScopedApplyPatchTool(root, nil))
	opts := RunOptions{PermissionGranted: true, FileTracker: tracker}
	if r := registry.RunWithOptions(context.Background(), "read_file", map[string]any{"path": "src.txt"}, opts); r.Status != StatusOK {
		t.Fatalf("read: %s", r.Output)
	}
	result := registry.RunWithOptions(context.Background(), "apply_patch", map[string]any{"patch": copyPatch("src.txt", "dst.txt", true)}, opts)
	if result.Status != StatusOK {
		t.Fatalf("copy with hunk should apply, got %s: %s", result.Status, result.Output)
	}
	if content, _ := os.ReadFile(filepath.Join(root, "src.txt")); string(content) != "hello\nold\n" {
		t.Fatalf("copy must keep the source, got %q", string(content))
	}
	if content, _ := os.ReadFile(filepath.Join(root, "dst.txt")); string(content) != "hello\nnew\n" {
		t.Fatalf("copy destination = %q", string(content))
	}
	if len(result.ChangedFiles) != 1 || result.ChangedFiles[0] != "dst.txt" {
		t.Fatalf("changed files = %v", result.ChangedFiles)
	}
	destination, err := filepath.EvalSymlinks(filepath.Join(root, "dst.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !tracker.SeenWhole(destination) {
		t.Fatal("destination copied from a fully read source must be tracked as seen whole")
	}
	unreadSource, _ := filepath.EvalSymlinks(filepath.Join(root, "src.txt"))
	if _, tracked := tracker.Version(unreadSource); !tracked {
		t.Fatal("source must stay tracked after a copy")
	}

	// Failure: destination already exists — nothing is written.
	writeTestFile(t, filepath.Join(root, "taken.txt"), "keep\n")
	result = NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": copyPatch("src.txt", "taken.txt", false)})
	if result.Status == StatusOK || !strings.Contains(result.Output, "already exists") {
		t.Fatalf("copy onto an existing destination must be refused, got %s: %s", result.Status, result.Output)
	}
	if content, _ := os.ReadFile(filepath.Join(root, "taken.txt")); string(content) != "keep\n" {
		t.Fatal("existing destination must be untouched")
	}

	// Failure: copy onto itself.
	result = NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": copyPatch("src.txt", "src.txt", false)})
	if result.Status == StatusOK || !strings.Contains(result.Output, "onto itself") {
		t.Fatalf("copy onto itself must be refused, got %s: %s", result.Status, result.Output)
	}
}

func TestUnifiedPatchRejectsOverDeclaredHunkCounts(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "a.txt"), "hello\nold\n")
	writeTestFile(t, filepath.Join(root, "b.txt"), "x\n")
	// "-1,3 +1,3" over-declares a two-line hunk, so without a guard the next
	// file's ---/+++ headers would be swallowed as "-"/"+" content.
	patch := strings.Join([]string{
		"--- a/a.txt", "+++ b/a.txt", "@@ -1,3 +1,3 @@", " hello", "-old", "+new",
		"--- a/b.txt", "+++ b/b.txt", "@@ -1 +1 @@", "-x", "+y", "",
	}, "\n")
	result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": patch})
	if result.Status == StatusOK || !strings.Contains(result.Output, "declared line counts") {
		t.Fatalf("over-declared hunk must be reported as malformed, got %s: %s", result.Status, result.Output)
	}
	for name, want := range map[string]string{"a.txt": "hello\nold\n", "b.txt": "x\n"} {
		if content, _ := os.ReadFile(filepath.Join(root, name)); string(content) != want {
			t.Fatalf("%s must be untouched after a malformed patch, got %q", name, string(content))
		}
	}
	// Truncated at end of input is reported the same way.
	truncated := "--- a/a.txt\n+++ b/a.txt\n@@ -1,2 +1,2 @@\n hello\n-old\n"
	if result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": truncated}); result.Status == StatusOK || !strings.Contains(result.Output, "declared line counts") {
		t.Fatalf("truncated hunk must be reported as malformed, got %s: %s", result.Status, result.Output)
	}
}

func TestUnifiedPatchNoNewlineMarkerOnlyAfterFinalHunk(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "two.txt"), "a\nb\nc\nd\ne\nf\n")
	// A marker after the first of two hunks describes nothing real and must
	// not leak into the second hunk's result: the patch is rejected untouched.
	early := strings.Join([]string{
		"--- a/two.txt", "+++ b/two.txt",
		"@@ -1,2 +1,2 @@", " a", "-b", "+B", "\\ No newline at end of file",
		"@@ -5,2 +5,2 @@", " e", "-f", "+F", "",
	}, "\n")
	result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": early})
	if result.Status == StatusOK || !strings.Contains(result.Output, "must follow the last hunk") {
		t.Fatalf("marker before a later hunk must be rejected, got %s: %s", result.Status, result.Output)
	}
	if content, _ := os.ReadFile(filepath.Join(root, "two.txt")); string(content) != "a\nb\nc\nd\ne\nf\n" {
		t.Fatal("rejected patch must not modify the file")
	}
	// The same marker after the final hunk applies and strips the newline.
	final := strings.Join([]string{
		"--- a/two.txt", "+++ b/two.txt",
		"@@ -1,2 +1,2 @@", " a", "-b", "+B",
		"@@ -5,2 +5,2 @@", " e", "-f", "+F", "\\ No newline at end of file", "",
	}, "\n")
	result = NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": final})
	if result.Status != StatusOK {
		t.Fatalf("two-hunk patch with a final marker should apply, got %s: %s", result.Status, result.Output)
	}
	if content, _ := os.ReadFile(filepath.Join(root, "two.txt")); string(content) != "a\nB\nc\nd\ne\nF" {
		t.Fatalf("content = %q", string(content))
	}
}

// A removed "-- …" line directly followed by an added "++ …" line looks like a
// ---/+++ header pair; the boundary check must not mistake it for one.
func TestUnifiedPatchKeepsAdjacentDashPlusContentLines(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "notes.sql"), "select 1;\n-- old comment\nselect 2;\n")
	patch := strings.Join([]string{
		"--- a/notes.sql", "+++ b/notes.sql",
		"@@ -1,3 +1,3 @@", " select 1;", "--- old comment", "+++ new comment", " select 2;", "",
	}, "\n")
	result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": patch})
	if result.Status != StatusOK {
		t.Fatalf("adjacent -- / ++ content lines must apply, got %s: %s", result.Status, result.Output)
	}
	if content, _ := os.ReadFile(filepath.Join(root, "notes.sql")); string(content) != "select 1;\n++ new comment\nselect 2;\n" {
		t.Fatalf("content = %q", string(content))
	}
}

// A unified deletion states the content it removes; it must be verified
// against the current file before anything is deleted.
func TestUnifiedPatchDeletionVerifiesExpectedContent(t *testing.T) {
	deletion := func(path string, hunks ...string) string {
		lines := []string{"diff --git a/" + path + " b/" + path, "deleted file mode 100644", "--- a/" + path, "+++ /dev/null"}
		return strings.Join(append(append(lines, hunks...), ""), "\n")
	}
	t.Run("matching deletion succeeds", func(t *testing.T) {
		root := t.TempDir()
		writeTestFile(t, filepath.Join(root, "gone.txt"), "current\n")
		result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": deletion("gone.txt", "@@ -1 +0,0 @@", "-current")})
		if result.Status != StatusOK {
			t.Fatalf("matching deletion should apply, got %s: %s", result.Status, result.Output)
		}
		if _, err := os.Stat(filepath.Join(root, "gone.txt")); !os.IsNotExist(err) {
			t.Fatal("file must be deleted")
		}
	})
	t.Run("stale deletion is refused untouched", func(t *testing.T) {
		root := t.TempDir()
		writeTestFile(t, filepath.Join(root, "gone.txt"), "current\n")
		result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": deletion("gone.txt", "@@ -1 +0,0 @@", "-stale")})
		if result.Status == StatusOK || !strings.Contains(result.Output, "does not match its current content") {
			t.Fatalf("stale deletion must be refused, got %s: %s", result.Status, result.Output)
		}
		if content, _ := os.ReadFile(filepath.Join(root, "gone.txt")); string(content) != "current\n" {
			t.Fatalf("file must be byte-for-byte unchanged, got %q", string(content))
		}
	})
	t.Run("partial deletion is refused", func(t *testing.T) {
		root := t.TempDir()
		writeTestFile(t, filepath.Join(root, "gone.txt"), "one\ntwo\n")
		result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": deletion("gone.txt", "@@ -1 +0,0 @@", "-one")})
		if result.Status == StatusOK || !strings.Contains(result.Output, "does not match its current content") {
			t.Fatalf("deletion that does not cover the file must be refused, got %s: %s", result.Status, result.Output)
		}
		if content, _ := os.ReadFile(filepath.Join(root, "gone.txt")); string(content) != "one\ntwo\n" {
			t.Fatal("file must be unchanged")
		}
	})
	t.Run("header-only deletion is refused", func(t *testing.T) {
		root := t.TempDir()
		writeTestFile(t, filepath.Join(root, "gone.txt"), "current\n")
		result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": deletion("gone.txt")})
		if result.Status == StatusOK || !strings.Contains(result.Output, "must include the hunk") {
			t.Fatalf("header-only deletion must be refused, got %s: %s", result.Status, result.Output)
		}
		if _, err := os.Stat(filepath.Join(root, "gone.txt")); err != nil {
			t.Fatal("file must still exist")
		}
	})
	t.Run("multi-hunk deletion verifies everything before removing", func(t *testing.T) {
		root := t.TempDir()
		writeTestFile(t, filepath.Join(root, "gone.txt"), "a\nb\nc\nd\n")
		good := deletion("gone.txt", "@@ -1,2 +0,0 @@", "-a", "-b", "@@ -3,2 +0,0 @@", "-c", "-d")
		bad := deletion("gone.txt", "@@ -1,2 +0,0 @@", "-a", "-b", "@@ -3,2 +0,0 @@", "-c", "-stale")
		result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": bad})
		if result.Status == StatusOK {
			t.Fatalf("deletion with one stale hunk must be refused: %s", result.Output)
		}
		if content, _ := os.ReadFile(filepath.Join(root, "gone.txt")); string(content) != "a\nb\nc\nd\n" {
			t.Fatal("no hunk may be applied when any hunk is stale")
		}
		result = NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": good})
		if result.Status != StatusOK {
			t.Fatalf("multi-hunk deletion should apply, got %s: %s", result.Status, result.Output)
		}
		if _, err := os.Stat(filepath.Join(root, "gone.txt")); !os.IsNotExist(err) {
			t.Fatal("file must be deleted")
		}
	})
	t.Run("structured delete stays unconditional", func(t *testing.T) {
		root := t.TempDir()
		writeTestFile(t, filepath.Join(root, "gone.txt"), "anything\n")
		patch := "*** Begin Patch\n*** Delete File: gone.txt\n*** End Patch\n"
		if result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": patch}); result.Status != StatusOK {
			t.Fatalf("structured delete should apply, got %s: %s", result.Status, result.Output)
		}
	})
}

// The source is re-read through the root immediately before commit, so a
// change made after planning is refused instead of overwritten or removed.
func TestApplyStructuredPatchChangeRefusesSourceChangedAfterPlanning(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.txt")
	writeTestFile(t, path, "planned\n")
	workspace, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	target := structuredPatchTarget{requested: "file.txt", absolute: path, relative: "file.txt"}
	destination := structuredPatchTarget{requested: "copy.txt", absolute: filepath.Join(root, "copy.txt"), relative: "copy.txt"}
	for name, change := range map[string]structuredPatchChange{
		"update": {kind: structuredPatchUpdate, from: target, to: target, before: "planned\n", after: "new\n", mode: 0o644},
		"delete": {kind: structuredPatchDelete, from: target, to: target, before: "planned\n", mode: 0o644},
		"copy":   {kind: structuredPatchCopy, from: target, to: destination, before: "planned\n", after: "planned\n", mode: 0o644},
	} {
		writeTestFile(t, path, "changed after planning\n")
		outcome, err := applyStructuredPatchChange(workspace, change)
		if err == nil || len(outcome.completed) != 0 || len(outcome.incompletePaths) != 0 || !strings.Contains(err.Error(), "changed on disk between planning and commit") {
			t.Fatalf("%s: expected a refusal, got outcome=%#v err=%v", name, outcome, err)
		}
		if content, _ := os.ReadFile(path); string(content) != "changed after planning\n" {
			t.Fatalf("%s: file must be untouched, got %q", name, string(content))
		}
		if _, statErr := os.Stat(filepath.Join(root, "copy.txt")); !os.IsNotExist(statErr) {
			t.Fatalf("%s: refused change must not create copy.txt", name)
		}
	}
}

func TestUnifiedPatchDeletionIsByteExact(t *testing.T) {
	deletion := func(hunks ...string) string {
		return strings.Join(append(append([]string{"--- a/gone.txt", "+++ /dev/null"}, hunks...), ""), "\n")
	}
	for name, tc := range map[string]struct{ file, patch string }{
		"trailing whitespace differs": {"current  \n", deletion("@@ -1 +0,0 @@", "-current")},
		"indentation differs":         {"  current\n", deletion("@@ -1 +0,0 @@", "-current")},
		"blank line not covered":      {"current\n\n", deletion("@@ -1 +0,0 @@", "-current")},
		"space-only line not covered": {"current\n   \n", deletion("@@ -1 +0,0 @@", "-current")},
		"missing final newline":       {"current", deletion("@@ -1 +0,0 @@", "-current")},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeTestFile(t, filepath.Join(root, "gone.txt"), tc.file)
			result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": tc.patch})
			if result.Status == StatusOK {
				t.Fatalf("deletion must be byte-exact; %q was deleted by a non-matching hunk", tc.file)
			}
			if content, _ := os.ReadFile(filepath.Join(root, "gone.txt")); string(content) != tc.file {
				t.Fatalf("file must be unchanged, got %q", string(content))
			}
		})
	}
	t.Run("no-newline marker on the removed side matches exactly", func(t *testing.T) {
		root := t.TempDir()
		writeTestFile(t, filepath.Join(root, "gone.txt"), "current")
		patch := deletion("@@ -1 +0,0 @@", "-current", "\\ No newline at end of file")
		if result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": patch}); result.Status != StatusOK {
			t.Fatalf("deletion with a matching no-newline marker should apply, got %s: %s", result.Status, result.Output)
		}
		if _, err := os.Stat(filepath.Join(root, "gone.txt")); !os.IsNotExist(err) {
			t.Fatal("file must be deleted")
		}
	})
	t.Run("CRLF file matches an LF patch of the same content", func(t *testing.T) {
		root := t.TempDir()
		writeTestFile(t, filepath.Join(root, "gone.txt"), "a\r\nb\r\n")
		if result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": deletion("@@ -1,2 +0,0 @@", "-a", "-b")}); result.Status != StatusOK {
			t.Fatalf("CRLF deletion should apply, got %s: %s", result.Status, result.Output)
		}
	})
}

// git describes an empty file's creation or deletion with header lines only.
func TestUnifiedPatchHeaderOnlyEmptyFileForms(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "empty.txt"), "")
	writeTestFile(t, filepath.Join(root, "full.txt"), "content\n")
	writeTestFile(t, filepath.Join(root, "keep.txt"), "a\n")
	deleteHeader := func(path string) string {
		return "diff --git a/" + path + " b/" + path + "\ndeleted file mode 100644\nindex e69de29..0000000\n"
	}
	// A non-empty file is not deleted by the header-only form.
	result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": deleteHeader("full.txt")})
	if result.Status == StatusOK || !strings.Contains(result.Output, "expects an empty file") {
		t.Fatalf("header-only deletion of a non-empty file must be refused, got %s: %s", result.Status, result.Output)
	}
	if _, err := os.Stat(filepath.Join(root, "full.txt")); err != nil {
		t.Fatal("non-empty file must still exist")
	}
	// In a multi-file patch the empty-file deletion and creation are applied
	// alongside a normal hunk.
	patch := deleteHeader("empty.txt") +
		"diff --git a/blank.txt b/blank.txt\nnew file mode 100644\nindex 0000000..e69de29\n" +
		"diff --git a/keep.txt b/keep.txt\n--- a/keep.txt\n+++ b/keep.txt\n@@ -1 +1 @@\n-a\n+b\n"
	result = NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": patch})
	if result.Status != StatusOK {
		t.Fatalf("multi-file patch with header-only forms should apply, got %s: %s", result.Status, result.Output)
	}
	if _, err := os.Stat(filepath.Join(root, "empty.txt")); !os.IsNotExist(err) {
		t.Fatal("empty file must be deleted")
	}
	if content, err := os.ReadFile(filepath.Join(root, "blank.txt")); err != nil || len(content) != 0 {
		t.Fatalf("empty file must be created, got err=%v content=%q", err, string(content))
	}
	if content, _ := os.ReadFile(filepath.Join(root, "keep.txt")); string(content) != "b\n" {
		t.Fatalf("hunk file = %q", string(content))
	}
	if len(result.ChangedFiles) != 3 {
		t.Fatalf("changed files = %v", result.ChangedFiles)
	}
}

// When a later change fails, the error names exactly which files were already
// committed so the caller knows what changed and what did not.
func TestApplyPatchOperationsReportsCommittedPrefixOnFailure(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "first.txt"), "one\n")
	writeTestFile(t, filepath.Join(root, "second.txt"), "two\n")
	writeTestFile(t, filepath.Join(root, "third.txt"), "three\n")
	// Make second.txt change after planning so its commit is refused.
	structuredPatchBeforeCommit = func(change structuredPatchChange) {
		if change.to.relative == "second.txt" {
			writeTestFile(t, filepath.Join(root, "second.txt"), "changed\n")
		}
	}
	defer func() { structuredPatchBeforeCommit = nil }()
	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: first.txt", "@@", "-one", "+ONE",
		"*** Update File: second.txt", "@@", "-two", "+TWO",
		"*** Update File: third.txt", "@@", "-three", "+THREE",
		"*** End Patch", "",
	}, "\n")
	result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": patch})
	if result.Status == StatusOK {
		t.Fatal("patch must fail when a later change is refused")
	}
	for _, want := range []string{"already committed: first.txt", "remaining files are unchanged"} {
		if !strings.Contains(result.Output, want) {
			t.Fatalf("error must report the committed prefix, got: %s", result.Output)
		}
	}
	if strings.Contains(result.Output, "committed: first.txt, second.txt") || strings.Contains(result.Output, "third.txt") {
		t.Fatalf("error must not list uncommitted files as committed: %s", result.Output)
	}
	if content, _ := os.ReadFile(filepath.Join(root, "first.txt")); string(content) != "ONE\n" {
		t.Fatalf("first.txt must hold the committed change, got %q", string(content))
	}
	if content, _ := os.ReadFile(filepath.Join(root, "second.txt")); string(content) != "changed\n" {
		t.Fatalf("second.txt must be untouched by the patch, got %q", string(content))
	}
	if content, _ := os.ReadFile(filepath.Join(root, "third.txt")); string(content) != "three\n" {
		t.Fatalf("third.txt must be untouched, got %q", string(content))
	}
	if got := result.ChangedFiles; len(got) != 1 || got[0] != "first.txt" {
		t.Fatalf("partial patch ChangedFiles = %#v, want committed prefix only", got)
	}
	resolvedFirst, err := filepath.EvalSymlinks(filepath.Join(root, "first.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := result.FileDiffs; len(got) != 1 || got[0] != (FileDiff{Path: resolvedFirst, OldExists: true, NewExists: true, OldText: "one\n", NewText: "ONE\n"}) {
		t.Fatalf("partial patch FileDiffs = %#v", got)
	}
}

func TestApplyPatchOperationsReportsWorkspaceRelativeCommittedPrefixUnderCwd(t *testing.T) {
	root := t.TempDir()
	applyRoot := filepath.Join(root, "sub", "dir")
	if err := os.MkdirAll(applyRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(applyRoot, "first.txt"), "one\n")
	writeTestFile(t, filepath.Join(applyRoot, "second.txt"), "two\n")
	structuredPatchBeforeCommit = func(change structuredPatchChange) {
		if change.to.relative == "second.txt" {
			writeTestFile(t, filepath.Join(applyRoot, "second.txt"), "changed\n")
		}
	}
	t.Cleanup(func() { structuredPatchBeforeCommit = nil })
	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: first.txt", "@@", "-one", "+ONE",
		"*** Update File: second.txt", "@@", "-two", "+TWO",
		"*** End Patch", "",
	}, "\n")

	result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{
		"cwd": "sub/dir", "patch": patch,
	})
	if result.Status != StatusError || !strings.Contains(result.Output, "already committed: sub/dir/first.txt") {
		t.Fatalf("nested partial failure = status=%s output=%q", result.Status, result.Output)
	}
	if got := result.ChangedFiles; len(got) != 1 || got[0] != "sub/dir/first.txt" {
		t.Fatalf("nested partial ChangedFiles = %#v", got)
	}
}

func TestApplyPatchSeparatesVerifiedAndUnverifiedPartialPublications(t *testing.T) {
	root := t.TempDir()
	priorPublish := structuredPatchPublishNoReplace
	structuredPatchPublishNoReplace = func(workspace *os.Root, source, target string, mode os.FileMode) (bool, error) {
		if target != "uncertain.txt" {
			return priorPublish(workspace, source, target, mode)
		}
		file, err := workspace.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
		if err != nil {
			return false, err
		}
		if _, err := file.WriteString("partial"); err != nil {
			_ = file.Close()
			return true, err
		}
		if err := file.Close(); err != nil {
			return true, err
		}
		return true, errors.New("injected failure after partial publication")
	}
	t.Cleanup(func() { structuredPatchPublishNoReplace = priorPublish })

	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Add File: verified.txt", "+verified",
		"*** Add File: uncertain.txt", "+intended complete content",
		"*** Add File: untouched.txt", "+untouched",
		"*** End Patch", "",
	}, "\n")
	result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": patch})

	if result.Status != StatusError {
		t.Fatalf("partial publication status = %s, want error", result.Status)
	}
	for _, want := range []string{"already committed: verified.txt", "published but unverified: uncertain.txt"} {
		if !strings.Contains(result.Output, want) {
			t.Fatalf("partial publication error must contain %q: %s", want, result.Output)
		}
	}
	committedFragment := strings.SplitN(result.Output, "; published but unverified:", 2)[0]
	if strings.Contains(committedFragment, "uncertain.txt") {
		t.Fatalf("unverified path appears in committed fragment: %s", result.Output)
	}
	if got, want := result.ChangedFiles, []string{"verified.txt"}; !slices.Equal(got, want) {
		t.Fatalf("ChangedFiles = %#v, want verified publications only %#v", got, want)
	}
	resolvedVerified, err := filepath.EvalSymlinks(filepath.Join(root, "verified.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := result.FileDiffs; len(got) != 1 || got[0].Path != resolvedVerified {
		t.Fatalf("FileDiffs = %#v, want verified.txt only", got)
	}
	if got := mustReadTestFile(t, filepath.Join(root, "verified.txt")); got != "verified\n" {
		t.Fatalf("verified publication = %q", got)
	}
	if got := mustReadTestFile(t, filepath.Join(root, "uncertain.txt")); got != "partial" {
		t.Fatalf("unverified publication = %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "untouched.txt")); !os.IsNotExist(err) {
		t.Fatalf("later publication must remain untouched: %v", err)
	}
}

func TestApplyPatchMoveReportsPublishedDestinationWhenSourceRemovalFails(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "source.txt"), "before\n")
	priorRemove := structuredPatchRemove
	structuredPatchRemove = func(workspace *os.Root, name string) error {
		if name == "source.txt" {
			return os.ErrPermission
		}
		return priorRemove(workspace, name)
	}
	t.Cleanup(func() { structuredPatchRemove = priorRemove })

	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: source.txt",
		"*** Move to: destination.txt",
		"@@",
		"-before",
		"+after",
		"*** End Patch",
		"",
	}, "\n")
	result := NewScopedApplyPatchTool(root, nil).Run(context.Background(), map[string]any{"patch": patch})
	if result.Status != StatusError {
		t.Fatalf("move status = %s, want error", result.Status)
	}
	if got := mustReadTestFile(t, filepath.Join(root, "source.txt")); got != "before\n" {
		t.Fatalf("source content = %q", got)
	}
	if got := mustReadTestFile(t, filepath.Join(root, "destination.txt")); got != "after\n" {
		t.Fatalf("destination content = %q", got)
	}
	if got, want := result.ChangedFiles, []string{"destination.txt"}; !slices.Equal(got, want) {
		t.Fatalf("ChangedFiles = %#v, want %#v", got, want)
	}
	resolvedDestination, err := filepath.EvalSymlinks(filepath.Join(root, "destination.txt"))
	if err != nil {
		t.Fatal(err)
	}
	wantDiff := FileDiff{Path: resolvedDestination, OldExists: false, NewExists: true, NewText: "after\n"}
	if got := result.FileDiffs; len(got) != 1 || got[0] != wantDiff {
		t.Fatalf("FileDiffs = %#v, want %#v", got, []FileDiff{wantDiff})
	}
	if !strings.Contains(result.Display.Preview, "destination.txt") || strings.Contains(result.Display.Preview, "source.txt") {
		t.Fatalf("preview must show only destination creation: %q", result.Display.Preview)
	}
}
