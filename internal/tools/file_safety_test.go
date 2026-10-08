package tools

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// safetyRegistry wires the read/write tools the conflict-detection feature spans,
// so tests exercise the real RunWithOptions dispatch (including optionsAwareTool
// routing) rather than calling tool methods directly.
func safetyRegistry(t *testing.T, dir string) *Registry {
	t.Helper()
	registry := NewRegistry()
	for _, tool := range []Tool{NewScopedReadFileTool(dir, nil), NewScopedEditFileTool(dir, nil), NewScopedWriteFileTool(dir, nil)} {
		registry.Register(tool)
	}
	return registry
}

func grantedOpts(tracker *FileTracker) RunOptions {
	return RunOptions{PermissionGranted: true, FileTracker: tracker}
}

func TestEditFileRefusesAfterFileChangedOnDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := safetyRegistry(t, dir)
	tracker := NewFileTracker()

	// Model reads the file → baseline recorded.
	if res := registry.RunWithOptions(context.Background(), "read_file", map[string]any{"path": "f.txt"}, grantedOpts(tracker)); res.Status != StatusOK {
		t.Fatalf("read_file failed: %s", res.Output)
	}
	// Something outside Zero rewrites the file.
	if err := os.WriteFile(path, []byte("alpha changed externally\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The edit (anchored on the stale view) must be refused, not applied.
	res := registry.RunWithOptions(context.Background(), "edit_file", map[string]any{
		"path": "f.txt", "old_string": "alpha", "new_string": "beta",
	}, grantedOpts(tracker))
	if res.Status != StatusError || !strings.Contains(res.Output, "changed on disk") {
		t.Fatalf("expected an on-disk-change refusal, got status=%s output=%q", res.Status, res.Output)
	}
	if got, _ := os.ReadFile(path); string(got) != "alpha changed externally\n" {
		t.Fatalf("file must be untouched after a refused edit, got %q", got)
	}
}

func TestEditFileSucceedsWhenUnchangedAndRebaselines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := safetyRegistry(t, dir)
	tracker := NewFileTracker()

	registry.RunWithOptions(context.Background(), "read_file", map[string]any{"path": "f.txt"}, grantedOpts(tracker))
	if res := registry.RunWithOptions(context.Background(), "edit_file", map[string]any{
		"path": "f.txt", "old_string": "alpha", "new_string": "beta",
	}, grantedOpts(tracker)); res.Status != StatusOK {
		t.Fatalf("edit on unchanged file should succeed, got %q", res.Output)
	}
	// A second edit must not false-conflict: the write re-baselined the tracker.
	if res := registry.RunWithOptions(context.Background(), "edit_file", map[string]any{
		"path": "f.txt", "old_string": "beta", "new_string": "gamma",
	}, grantedOpts(tracker)); res.Status != StatusOK {
		t.Fatalf("second consecutive edit should succeed (re-baselined), got %q", res.Output)
	}
	if got, _ := os.ReadFile(path); string(got) != "gamma\n" {
		t.Fatalf("file = %q, want gamma", got)
	}
}

func TestApplyPatchPreservesWholeFileObservationForFollowUpEdit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := safetyRegistry(t, dir)
	registry.Register(NewScopedApplyPatchTool(dir, nil))
	tracker := NewFileTracker()
	opts := grantedOpts(tracker)

	if result := registry.RunWithOptions(context.Background(), "read_file", map[string]any{"path": "f.txt"}, opts); result.Status != StatusOK {
		t.Fatalf("read_file failed: %s", result.Output)
	}
	patch := strings.Join([]string{
		"diff --git a/f.txt b/f.txt",
		"--- a/f.txt",
		"+++ b/f.txt",
		"@@ -1,2 +1,2 @@",
		"-alpha",
		"+gamma",
		" beta",
		"",
	}, "\n")
	if result := registry.RunWithOptions(context.Background(), "apply_patch", map[string]any{"patch": patch}, opts); result.Status != StatusOK {
		if gitApplyUnavailable(result.Output) {
			t.Skipf("git binary unavailable: %s", result.Output)
		}
		t.Fatalf("apply_patch failed: %s", result.Output)
	}

	result := registry.RunWithOptions(context.Background(), "edit_file", map[string]any{
		"path": "f.txt", "old_string": "beta", "new_string": "delta",
	}, opts)
	if result.Status != StatusOK {
		t.Fatalf("follow-up edit should not require a wasted re-read: %s", result.Output)
	}
}

func TestApplyPatchRenameOrCopyDoesNotCreditUnreadDestination(t *testing.T) {
	for _, tc := range []struct {
		name  string
		patch string
	}{
		{
			name: "rename",
			patch: strings.Join([]string{
				"diff --git a/source.txt b/destination.txt",
				"similarity index 100%",
				"rename from source.txt",
				"rename to destination.txt",
				"",
			}, "\n"),
		},
		{
			name: "copy",
			patch: strings.Join([]string{
				"diff --git a/source.txt b/destination.txt",
				"similarity index 100%",
				"copy from source.txt",
				"copy to destination.txt",
				"",
			}, "\n"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "source.txt"), []byte("unread source\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			registry := safetyRegistry(t, dir)
			registry.Register(NewScopedApplyPatchTool(dir, nil))
			tracker := NewFileTracker()
			opts := grantedOpts(tracker)

			if result := registry.RunWithOptions(context.Background(), "apply_patch", map[string]any{"patch": tc.patch}, opts); result.Status != StatusOK {
				if gitApplyUnavailable(result.Output) {
					t.Skipf("git binary unavailable: %s", result.Output)
				}
				t.Fatalf("apply_patch failed: %s", result.Output)
			}
			destination, err := filepath.EvalSymlinks(filepath.Join(dir, "destination.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if tracker.SeenWhole(destination) {
				t.Fatal("rename/copy destination was credited as fully seen without a read")
			}
			result := registry.RunWithOptions(context.Background(), "write_file", map[string]any{
				"path": "destination.txt", "content": "blind overwrite\n", "overwrite": true,
			}, opts)
			if result.Status != StatusError || !strings.Contains(result.Output, "has not been read exactly") {
				t.Fatalf("blind destination overwrite was not refused: %#v", result)
			}
		})
	}
}

func TestApplyPatchCompleteCreationCreditsSuppliedFile(t *testing.T) {
	dir := t.TempDir()
	registry := safetyRegistry(t, dir)
	registry.Register(NewScopedApplyPatchTool(dir, nil))
	tracker := NewFileTracker()
	opts := grantedOpts(tracker)
	patch := strings.Join([]string{
		"diff --git a/new.txt b/new.txt",
		"new file mode 100644",
		"--- /dev/null",
		"+++ b/new.txt",
		"@@ -0,0 +1 @@",
		"+supplied content",
		"",
	}, "\n")
	if result := registry.RunWithOptions(context.Background(), "apply_patch", map[string]any{"patch": patch}, opts); result.Status != StatusOK {
		if gitApplyUnavailable(result.Output) {
			t.Skipf("git binary unavailable: %s", result.Output)
		}
		t.Fatalf("apply_patch failed: %s", result.Output)
	}
	created, err := filepath.EvalSymlinks(filepath.Join(dir, "new.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !tracker.SeenWhole(created) {
		t.Fatal("complete /dev/null creation was not credited as supplied content")
	}
	result := registry.RunWithOptions(context.Background(), "edit_file", map[string]any{
		"path": "new.txt", "old_string": "supplied", "new_string": "updated",
	}, opts)
	if result.Status != StatusOK {
		t.Fatalf("follow-up edit of supplied file failed: %s", result.Output)
	}
	updated, err := os.ReadFile(filepath.Join(dir, "new.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.ReplaceAll(string(updated), "\r\n", "\n"), "updated content\n"; got != want {
		t.Fatalf("follow-up edit content = %q, want %q", got, want)
	}
}

func TestStructuredPatchDoesNotCreditUnreadFileForOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := safetyRegistry(t, dir)
	registry.Register(NewScopedApplyPatchTool(dir, nil))
	tracker := NewFileTracker()
	opts := grantedOpts(tracker)
	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: f.txt",
		"@@",
		"-beta",
		"+updated",
		"*** End Patch",
		"",
	}, "\n")

	if result := registry.RunWithOptions(context.Background(), "apply_patch", map[string]any{"patch": patch}, opts); result.Status != StatusOK {
		t.Fatalf("structured patch failed: %s", result.Output)
	}
	trackedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if tracker.SeenWhole(trackedPath) {
		t.Fatal("a partial structured patch credited the unread file as fully seen")
	}
	result := registry.RunWithOptions(context.Background(), "write_file", map[string]any{
		"path": "f.txt", "content": "blind overwrite\n", "overwrite": true,
	}, opts)
	if result.Status != StatusError || !strings.Contains(result.Output, "has not been read exactly") {
		t.Fatalf("blind overwrite after a partial patch was not refused: %#v", result)
	}
	if got, want := mustReadTestFile(t, path), "alpha\nupdated\ngamma\n"; got != want {
		t.Fatalf("file = %q, want %q", got, want)
	}
}

func TestStructuredPatchPreservesWholeFileObservation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := safetyRegistry(t, dir)
	registry.Register(NewScopedApplyPatchTool(dir, nil))
	tracker := NewFileTracker()
	opts := grantedOpts(tracker)
	if result := registry.RunWithOptions(context.Background(), "read_file", map[string]any{"path": "f.txt"}, opts); result.Status != StatusOK {
		t.Fatalf("read_file failed: %s", result.Output)
	}
	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: f.txt",
		"@@",
		"-alpha",
		"+updated",
		"*** End Patch",
		"",
	}, "\n")
	if result := registry.RunWithOptions(context.Background(), "apply_patch", map[string]any{"patch": patch}, opts); result.Status != StatusOK {
		t.Fatalf("structured patch failed: %s", result.Output)
	}
	trackedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if !tracker.SeenWhole(trackedPath) {
		t.Fatal("structured patch discarded an existing whole-file observation")
	}
	result := registry.RunWithOptions(context.Background(), "write_file", map[string]any{
		"path": "f.txt", "content": "known overwrite\n", "overwrite": true,
	}, opts)
	if result.Status != StatusOK {
		t.Fatalf("overwrite after a whole-file read was refused: %s", result.Output)
	}
}

func TestStructuredPatchCreationCreditsSuppliedFile(t *testing.T) {
	dir := t.TempDir()
	registry := safetyRegistry(t, dir)
	registry.Register(NewScopedApplyPatchTool(dir, nil))
	tracker := NewFileTracker()
	opts := grantedOpts(tracker)
	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Add File: created.txt",
		"+supplied",
		"*** End Patch",
		"",
	}, "\n")
	if result := registry.RunWithOptions(context.Background(), "apply_patch", map[string]any{"patch": patch}, opts); result.Status != StatusOK {
		t.Fatalf("structured patch failed: %s", result.Output)
	}
	path, err := filepath.EvalSymlinks(filepath.Join(dir, "created.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !tracker.SeenWhole(path) {
		t.Fatal("a fully supplied structured add was not credited as fully seen")
	}
}

func TestWriteFileOverwriteRefusedWhenChangedOnDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := safetyRegistry(t, dir)
	tracker := NewFileTracker()

	registry.RunWithOptions(context.Background(), "read_file", map[string]any{"path": "f.txt"}, grantedOpts(tracker))
	if err := os.WriteFile(path, []byte("v2 external\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := registry.RunWithOptions(context.Background(), "write_file", map[string]any{
		"path": "f.txt", "content": "v3 stale\n", "overwrite": true,
	}, grantedOpts(tracker))
	if res.Status != StatusError || !strings.Contains(res.Output, "changed on disk") {
		t.Fatalf("expected overwrite refusal, got status=%s output=%q", res.Status, res.Output)
	}
	if got, _ := os.ReadFile(path); string(got) != "v2 external\n" {
		t.Fatalf("file must be untouched after a refused overwrite, got %q", got)
	}
}

func TestWriteFileOverwriteRequiresExactRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := safetyRegistry(t, dir)
	tracker := NewFileTracker()

	// Never read through a tool: an overwrite would discard unseen content.
	res := registry.RunWithOptions(context.Background(), "write_file", map[string]any{
		"path": "f.txt", "content": "v3\n", "overwrite": true,
	}, grantedOpts(tracker))
	if res.Status != StatusError || !strings.Contains(res.Output, "has not been read exactly") {
		t.Fatalf("unseen overwrite should be refused, got %q", res.Output)
	}
	if got, _ := os.ReadFile(path); string(got) != "v1\n" {
		t.Fatalf("file = %q, want original content", got)
	}
}

func TestEditFileAllowsSeenRangeAndRejectsUnseenRange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := safetyRegistry(t, dir)
	tracker := NewFileTracker()
	opts := grantedOpts(tracker)

	if res := registry.RunWithOptions(context.Background(), "read_file", map[string]any{
		"path": "f.txt", "start_line": 2, "end_line": 2,
	}, opts); res.Status != StatusOK {
		t.Fatalf("partial read failed: %s", res.Output)
	}
	if res := registry.RunWithOptions(context.Background(), "edit_file", map[string]any{
		"path": "f.txt", "old_string": "gamma", "new_string": "delta",
	}, opts); res.Status != StatusError || !strings.Contains(res.Output, "has not been read exactly") {
		t.Fatalf("unseen edit should be refused, got %q", res.Output)
	}
	if res := registry.RunWithOptions(context.Background(), "edit_file", map[string]any{
		"path": "f.txt", "old_string": "beta", "new_string": "bravo",
	}, opts); res.Status != StatusOK {
		t.Fatalf("seen-range edit should succeed, got %q", res.Output)
	}
	if content, err := os.ReadFile(path); err != nil || !strings.Contains(string(content), "bravo") {
		t.Fatalf("seen-range edit did not update the file: content=%q err=%v", content, err)
	}
}

func TestCanonicalLimitedReadAuthorizesEdit(t *testing.T) {
	dir := t.TempDir()
	var source strings.Builder
	for line := 1; line <= 1000; line++ {
		source.WriteString("line ")
		source.WriteString(strconv.Itoa(line))
		source.WriteByte('\n')
	}
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte(source.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := safetyRegistry(t, dir)
	tracker := NewFileTracker()
	opts := grantedOpts(tracker)

	read := registry.RunWithOptions(context.Background(), "read_file", map[string]any{
		"path": "f.txt", "offset": 500, "limit": 10,
	}, opts)
	if read.Status != StatusOK || read.Truncated {
		t.Fatalf("bounded canonical read must be exact: status=%s truncated=%v meta=%#v", read.Status, read.Truncated, read.Meta)
	}
	if read.Meta["seen_lines"] != "500-509" {
		t.Fatalf("bounded canonical read did not receive exact-read credit: %#v", read.Meta)
	}

	edit := registry.RunWithOptions(context.Background(), "edit_file", map[string]any{
		"path": "f.txt", "old_string": "line 505", "new_string": "updated 505",
	}, opts)
	if edit.Status != StatusOK {
		t.Fatalf("edit inside canonical read range was refused: %s", edit.Output)
	}
}

func TestEditFileReplaceAllRecordsEveryShiftedReplacementSpan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("before\nneedle\nmiddle\nneedle\nafter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := safetyRegistry(t, dir)
	tracker := NewFileTracker()
	opts := grantedOpts(tracker)

	if res := registry.RunWithOptions(context.Background(), "read_file", map[string]any{
		"path": "f.txt", "start_line": 2, "end_line": 4,
	}, opts); res.Status != StatusOK {
		t.Fatalf("partial read failed: %q", res.Output)
	}
	if res := registry.RunWithOptions(context.Background(), "edit_file", map[string]any{
		"path":        "f.txt",
		"old_string":  "needle",
		"new_string":  "left\nright",
		"replace_all": true,
	}, opts); res.Status != StatusOK {
		t.Fatalf("replace_all failed: %q", res.Output)
	}
	// This follow-up targets both generated "right" lines. It succeeds only if
	// replace_all recorded the shifted second replacement as seen.
	if res := registry.RunWithOptions(context.Background(), "edit_file", map[string]any{
		"path":        "f.txt",
		"old_string":  "right",
		"new_string":  "done",
		"replace_all": true,
	}, opts); res.Status != StatusOK {
		t.Fatalf("follow-up edit of every replacement should succeed: %q", res.Output)
	}
	if content, err := os.ReadFile(path); err != nil || strings.Count(string(content), "done") != 2 {
		t.Fatalf("unexpected follow-up content: content=%q err=%v", content, err)
	}
}

func TestWriteFileCanOverwriteNewlyCreatedEmptyFile(t *testing.T) {
	dir := t.TempDir()
	registry := safetyRegistry(t, dir)
	tracker := NewFileTracker()
	opts := grantedOpts(tracker)

	if res := registry.RunWithOptions(context.Background(), "write_file", map[string]any{
		"path": "empty.txt", "content": "",
	}, opts); res.Status != StatusOK {
		t.Fatalf("empty create failed: %q", res.Output)
	}
	if res := registry.RunWithOptions(context.Background(), "write_file", map[string]any{
		"path": "empty.txt", "content": "now populated\n", "overwrite": true,
	}, opts); res.Status != StatusOK {
		t.Fatalf("empty overwrite should succeed: %q", res.Output)
	}
	if content, err := os.ReadFile(filepath.Join(dir, "empty.txt")); err != nil || string(content) != "now populated\n" {
		t.Fatalf("unexpected overwritten content: content=%q err=%v", content, err)
	}
}

func TestMinifiedReadDoesNotAuthorizeExactEdit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.go")
	if err := os.WriteFile(path, []byte("package p\n\nvar value = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry()
	registry.Register(NewScopedReadMinifiedFileTool(dir, nil))
	registry.Register(NewScopedEditFileTool(dir, nil))
	tracker := NewFileTracker()
	opts := grantedOpts(tracker)

	if res := registry.RunWithOptions(context.Background(), "read_minified_file", map[string]any{"path": "f.go"}, opts); res.Status != StatusOK {
		t.Fatalf("minified read failed: %s", res.Output)
	}
	res := registry.RunWithOptions(context.Background(), "edit_file", map[string]any{
		"path": "f.go", "old_string": "var value = 1", "new_string": "var value = 2",
	}, opts)
	if res.Status != StatusError || !strings.Contains(res.Output, "has not been read exactly") {
		t.Fatalf("minified view must not authorize exact edit, got %q", res.Output)
	}
}

func TestWriteAndEditUnaffectedWithoutTracker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := safetyRegistry(t, dir)

	// nil tracker (RunOptions without FileTracker) preserves the old behavior:
	// read, external change, then edit all proceed with no conflict gate.
	opts := RunOptions{PermissionGranted: true}
	registry.RunWithOptions(context.Background(), "read_file", map[string]any{"path": "f.txt"}, opts)
	if err := os.WriteFile(path, []byte("alpha changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := registry.RunWithOptions(context.Background(), "edit_file", map[string]any{
		"path": "f.txt", "old_string": "alpha changed", "new_string": "beta",
	}, opts); res.Status != StatusOK {
		t.Fatalf("without a tracker, edit should proceed, got %q", res.Output)
	}
}
