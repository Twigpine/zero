package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// gofmt ships with the Go toolchain, so it is the one formatter guaranteed to
// exist wherever these tests run.
func requireGofmt(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("gofmt"); err != nil {
		t.Skip("gofmt not on PATH")
	}
}

func TestFormatOnWriteDisabledByDefault(t *testing.T) {
	requireGofmt(t)
	t.Setenv("ZERO_FORMAT_ON_WRITE", "")
	dir := t.TempDir()
	ugly := "package a\n\nfunc  A( ) {   }\n"

	result := NewScopedWriteFileTool(dir, nil).(optionsAwareTool).RunWithOptions(context.Background(), map[string]any{
		"path":    "a.go",
		"content": ugly,
	}, RunOptions{})
	if result.Status != StatusOK {
		t.Fatalf("write failed: %q", result.Output)
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, "a.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != ugly {
		t.Fatalf("formatting must be off by default, got %q", onDisk)
	}
}

func TestFormatOnWriteFormatsAndKeepsTrackerConsistent(t *testing.T) {
	requireGofmt(t)
	t.Setenv("ZERO_FORMAT_ON_WRITE", "1")
	dir := t.TempDir()
	tracker := NewFileTracker()

	write := NewScopedWriteFileTool(dir, nil).(optionsAwareTool).RunWithOptions(context.Background(), map[string]any{
		"path":    "a.go",
		"content": "package a\n\nfunc  A( ) {   }\n",
	}, RunOptions{FileTracker: tracker})
	if write.Status != StatusOK {
		t.Fatalf("write failed: %q", write.Output)
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, "a.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(onDisk), "func A() {") {
		t.Fatalf("expected gofmt-formatted content, got %q", onDisk)
	}

	// The tracker is re-baselined to the post-format bytes, but those bytes were
	// not returned exactly to the model, so an edit must require an exact read.
	edit := NewScopedEditFileTool(dir, nil).(optionsAwareTool).RunWithOptions(context.Background(), map[string]any{
		"path":       "a.go",
		"old_string": "func A() {",
		"new_string": "func B() {",
	}, RunOptions{FileTracker: tracker})
	if edit.Status != StatusError || !strings.Contains(edit.Output, "has not been read exactly") {
		t.Fatalf("formatter-modified content must require an exact read: %q", edit.Output)
	}
	read := NewScopedReadFileTool(dir, nil).(optionsAwareTool).RunWithOptions(context.Background(), map[string]any{
		"path": "a.go",
	}, RunOptions{FileTracker: tracker})
	if read.Status != StatusOK {
		t.Fatalf("exact read failed: %q", read.Output)
	}
	edit = NewScopedEditFileTool(dir, nil).(optionsAwareTool).RunWithOptions(context.Background(), map[string]any{
		"path":       "a.go",
		"old_string": "func A() {",
		"new_string": "func B( ) {",
	}, RunOptions{FileTracker: tracker})
	if edit.Status != StatusOK {
		t.Fatalf("follow-up edit after exact read failed: %q", edit.Output)
	}
	edit = NewScopedEditFileTool(dir, nil).(optionsAwareTool).RunWithOptions(context.Background(), map[string]any{
		"path":       "a.go",
		"old_string": "func B() {",
		"new_string": "func C() {",
	}, RunOptions{FileTracker: tracker})
	if edit.Status != StatusError || !strings.Contains(edit.Output, "has not been read exactly") {
		t.Fatalf("formatter-modified edit must require an exact read: %q", edit.Output)
	}
}

func TestFormatOnWriteSkipsUnknownExtensions(t *testing.T) {
	t.Setenv("ZERO_FORMAT_ON_WRITE", "1")
	formatting := maybeFormatWrittenFile(context.Background(), filepath.Join(t.TempDir(), "notes.xyz"), "raw   text")
	if formatting.Content != "raw   text" || !formatting.ContentKnown {
		t.Fatalf("unknown extension must pass through: %q", formatting.Content)
	}
	if notice := formatting.notice("notes.xyz"); notice != "" {
		t.Fatalf("an extension with no formatter is not a miss worth reporting, got %q", notice)
	}
}

func TestFormatOnWriteFormatterLookupFailure(t *testing.T) {
	t.Setenv("ZERO_FORMAT_ON_WRITE", "1")
	t.Setenv("PATH", t.TempDir())
	targetPath := filepath.Join(t.TempDir(), "a.go")
	uglyContent := "package a\n\nfunc  A( ) {   }\n"
	if err := os.WriteFile(targetPath, []byte(uglyContent), 0o644); err != nil {
		t.Fatal(err)
	}
	formatting := maybeFormatWrittenFile(context.Background(), targetPath, uglyContent)
	if formatting.Content != uglyContent || !formatting.ContentKnown {
		t.Fatalf("missing formatter must return written content, got %q", formatting.Content)
	}
	if notice := formatting.notice("a.go"); notice != "" {
		t.Fatalf("an uninstalled formatter is a standing fact, not a miss worth reporting, got %q", notice)
	}
}

func TestFormatOnWriteReadsMutatedFileAfterFormatterFailure(t *testing.T) {
	requireGofmt(t)
	t.Setenv("ZERO_FORMAT_ON_WRITE", "1")
	targetPath := filepath.Join(t.TempDir(), "a.go")
	if err := os.WriteFile(targetPath, []byte("requested"), 0o644); err != nil {
		t.Fatal(err)
	}
	priorRunner := runFormatOnWriteCommand
	runFormatOnWriteCommand = func(_ context.Context, _ string, _ []string, _ string) error {
		if err := os.WriteFile(targetPath, []byte("formatter-mutated"), 0o644); err != nil {
			t.Fatal(err)
		}
		return exec.ErrNotFound
	}
	t.Cleanup(func() { runFormatOnWriteCommand = priorRunner })

	formatting := maybeFormatWrittenFile(context.Background(), targetPath, "requested")
	if !formatting.ContentKnown || formatting.Content != "requested" {
		t.Fatalf("formatter failure content = %q, known=%t", formatting.Content, formatting.ContentKnown)
	}
}

func TestFormatOnWriteReportsWhenFormatterFailureCannotBeRestored(t *testing.T) {
	requireGofmt(t)
	t.Setenv("ZERO_FORMAT_ON_WRITE", "1")
	targetPath := filepath.Join(t.TempDir(), "a.go")
	if err := os.WriteFile(targetPath, []byte("requested"), 0o644); err != nil {
		t.Fatal(err)
	}
	priorRunner := runFormatOnWriteCommand
	runFormatOnWriteCommand = func(_ context.Context, _ string, _ []string, _ string) error {
		if err := os.Remove(targetPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(targetPath, 0o755); err != nil {
			t.Fatal(err)
		}
		return exec.ErrNotFound
	}
	t.Cleanup(func() { runFormatOnWriteCommand = priorRunner })

	formatting := maybeFormatWrittenFile(context.Background(), targetPath, "requested")
	if !formatting.RestoreFailed {
		t.Fatal("failed recovery was not reported")
	}
	if formatting.ContentKnown {
		t.Fatalf("failed recovery reported known content %q", formatting.Content)
	}
	if notice := formatting.notice("a.go"); !strings.Contains(notice, "WARNING: a.go may not hold what was written") {
		t.Fatalf("failed recovery notice = %q", notice)
	}
}

func TestWriteFileRecreatesFormatterDeletedTargetWithOriginalMode(t *testing.T) {
	requireGofmt(t)
	t.Setenv("ZERO_FORMAT_ON_WRITE", "1")
	for _, tc := range []struct {
		name      string
		existing  bool
		wantMode  os.FileMode
		overwrite bool
	}{
		{name: "create", wantMode: 0o644},
		{name: "overwrite", existing: true, wantMode: 0o751, overwrite: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			targetPath := filepath.Join(root, "a.go")
			if tc.existing {
				if err := os.WriteFile(targetPath, []byte("before\n"), tc.wantMode); err != nil {
					t.Fatal(err)
				}
			}
			priorRunner := runFormatOnWriteCommand
			runFormatOnWriteCommand = func(_ context.Context, _ string, _ []string, _ string) error {
				if err := os.Remove(targetPath); err != nil {
					t.Fatal(err)
				}
				return exec.ErrNotFound
			}
			t.Cleanup(func() { runFormatOnWriteCommand = priorRunner })

			result := NewScopedWriteFileTool(root, nil).Run(context.Background(), map[string]any{
				"path": "a.go", "content": "requested\n", "overwrite": tc.overwrite,
			})
			if result.Status != StatusOK {
				t.Fatalf("write status = %s: %s", result.Status, result.Output)
			}
			content, err := os.ReadFile(targetPath)
			if err != nil || string(content) != "requested\n" {
				t.Fatalf("recreated content = %q, err=%v", content, err)
			}
			if runtime.GOOS != "windows" {
				info, err := os.Stat(targetPath)
				if err != nil {
					t.Fatal(err)
				}
				if got := info.Mode().Perm(); got != tc.wantMode {
					t.Fatalf("recreated mode = %o, want %o", got, tc.wantMode)
				}
			}
		})
	}
}

func TestFormatOnWriteMarksUnreadableFinalStateUnknown(t *testing.T) {
	requireGofmt(t)
	t.Setenv("ZERO_FORMAT_ON_WRITE", "1")
	targetPath := filepath.Join(t.TempDir(), "a.go")
	if err := os.WriteFile(targetPath, []byte("requested"), 0o644); err != nil {
		t.Fatal(err)
	}
	priorRunner := runFormatOnWriteCommand
	priorReader := readFormattedFile
	runFormatOnWriteCommand = func(context.Context, string, []string, string) error { return nil }
	readFormattedFile = func(*os.Root, string) ([]byte, os.FileInfo, error) { return nil, nil, os.ErrPermission }
	t.Cleanup(func() {
		runFormatOnWriteCommand = priorRunner
		readFormattedFile = priorReader
	})

	formatting := maybeFormatWrittenFile(context.Background(), targetPath, "requested")
	if formatting.ContentKnown || formatting.Content != "requested" {
		t.Fatalf("unreadable formatter result = %q, known=%t", formatting.Content, formatting.ContentKnown)
	}
}

func TestWriteFileUsesVerifiedBytesAfterFormatterFailure(t *testing.T) {
	requireGofmt(t)
	t.Setenv("ZERO_FORMAT_ON_WRITE", "1")
	root := t.TempDir()
	targetPath := filepath.Join(root, "a.go")
	priorRunner := runFormatOnWriteCommand
	runFormatOnWriteCommand = func(_ context.Context, _ string, _ []string, _ string) error {
		if err := os.WriteFile(targetPath, []byte("formatter-mutated\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return exec.ErrNotFound
	}
	t.Cleanup(func() { runFormatOnWriteCommand = priorRunner })

	result := NewScopedWriteFileTool(root, nil).Run(context.Background(), map[string]any{
		"path": "a.go", "content": "requested\n",
	})
	if result.Status != StatusOK {
		t.Fatalf("write status = %s: %s", result.Status, result.Output)
	}
	if got := result.FileDiffs; len(got) != 1 || got[0].NewText != "requested\n" {
		t.Fatalf("formatter-failure FileDiff = %#v", got)
	}
}

func TestEditFileUsesVerifiedBytesAfterFormatterFailure(t *testing.T) {
	requireGofmt(t)
	t.Setenv("ZERO_FORMAT_ON_WRITE", "1")
	root := t.TempDir()
	targetPath := filepath.Join(root, "a.go")
	if err := os.WriteFile(targetPath, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	priorRunner := runFormatOnWriteCommand
	runFormatOnWriteCommand = func(_ context.Context, _ string, _ []string, _ string) error {
		if err := os.WriteFile(targetPath, []byte("formatter-mutated\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return exec.ErrNotFound
	}
	t.Cleanup(func() { runFormatOnWriteCommand = priorRunner })

	result := NewScopedEditFileTool(root, nil).Run(context.Background(), map[string]any{
		"path": "a.go", "old_string": "before", "new_string": "requested",
	})
	if result.Status != StatusOK {
		t.Fatalf("edit status = %s: %s", result.Status, result.Output)
	}
	if got := result.FileDiffs; len(got) != 1 || got[0].OldText != "before\n" || got[0].NewText != "requested\n" {
		t.Fatalf("formatter-failure edit FileDiff = %#v", got)
	}
}

func TestWriteFileOmitsRichDiffWhenFormatterFinalReadFails(t *testing.T) {
	requireGofmt(t)
	t.Setenv("ZERO_FORMAT_ON_WRITE", "1")
	root := t.TempDir()
	priorRunner := runFormatOnWriteCommand
	priorReader := readFormattedFile
	runFormatOnWriteCommand = func(context.Context, string, []string, string) error { return nil }
	readFormattedFile = func(*os.Root, string) ([]byte, os.FileInfo, error) { return nil, nil, os.ErrPermission }
	t.Cleanup(func() {
		runFormatOnWriteCommand = priorRunner
		readFormattedFile = priorReader
	})

	result := NewScopedWriteFileTool(root, nil).Run(context.Background(), map[string]any{
		"path": "a.go", "content": "requested\n",
	})
	if result.Status != StatusOK || len(result.ChangedFiles) != 1 || len(result.FileDiffs) != 0 {
		t.Fatalf("unverified formatter result = status=%s changed=%#v diffs=%#v", result.Status, result.ChangedFiles, result.FileDiffs)
	}
	if result.Display.Preview != "" {
		t.Fatalf("unverified formatter result exposed stale preview: %q", result.Display.Preview)
	}
}

func TestEditFileOmitsPreviewWhenFormatterFinalReadFails(t *testing.T) {
	requireGofmt(t)
	t.Setenv("ZERO_FORMAT_ON_WRITE", "1")
	root := t.TempDir()
	targetPath := filepath.Join(root, "a.go")
	if err := os.WriteFile(targetPath, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	priorRunner := runFormatOnWriteCommand
	priorReader := readFormattedFile
	runFormatOnWriteCommand = func(context.Context, string, []string, string) error { return nil }
	readFormattedFile = func(*os.Root, string) ([]byte, os.FileInfo, error) { return nil, nil, os.ErrPermission }
	t.Cleanup(func() {
		runFormatOnWriteCommand = priorRunner
		readFormattedFile = priorReader
	})

	result := NewScopedEditFileTool(root, nil).Run(context.Background(), map[string]any{
		"path": "a.go", "old_string": "before", "new_string": "requested",
	})
	if result.Status != StatusOK || len(result.ChangedFiles) != 1 || len(result.FileDiffs) != 0 {
		t.Fatalf("unverified formatter result = status=%s changed=%#v diffs=%#v", result.Status, result.ChangedFiles, result.FileDiffs)
	}
	if result.Display.Preview != "" {
		t.Fatalf("unverified formatter result exposed stale preview: %q", result.Display.Preview)
	}
}

func TestWriteFileOmitsRichEvidenceWhenFormatterReplacesTargetWithOutOfRootSymlink(t *testing.T) {
	requireGofmt(t)
	t.Setenv("ZERO_FORMAT_ON_WRITE", "1")
	root := t.TempDir()
	targetPath := filepath.Join(root, "a.go")
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	trackedPath := filepath.Join(resolvedRoot, "a.go")
	outsidePath := filepath.Join(t.TempDir(), "outside.go")
	outsideContent := "package external\n\nconst Secret = \"outside\"\n"
	if err := os.WriteFile(outsidePath, []byte(outsideContent), 0o644); err != nil {
		t.Fatal(err)
	}
	priorRunner := runFormatOnWriteCommand
	runFormatOnWriteCommand = func(context.Context, string, []string, string) error {
		if err := os.Remove(targetPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outsidePath, targetPath); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		return nil
	}
	t.Cleanup(func() { runFormatOnWriteCommand = priorRunner })
	tracker := NewFileTracker()

	diagnosticsCalled := false
	result := NewScopedWriteFileTool(root, nil).(optionsAwareTool).RunWithOptions(context.Background(), map[string]any{
		"path": "a.go", "content": "package requested\n",
	}, RunOptions{FileTracker: tracker, Diagnostics: func(context.Context, string) string {
		diagnosticsCalled = true
		return "must not run"
	}})
	if result.Status != StatusOK || len(result.ChangedFiles) != 1 || len(result.FileDiffs) != 0 || result.Display.Preview != "" {
		t.Fatalf("out-of-root formatter result = status=%s changed=%#v diffs=%#v preview=%q output=%q", result.Status, result.ChangedFiles, result.FileDiffs, result.Display.Preview, result.Output)
	}
	if diagnosticsCalled {
		t.Fatal("diagnostics must not inspect an unverified formatter target")
	}
	if _, tracked := tracker.Version(trackedPath); tracked {
		t.Fatal("out-of-root formatter target must not be recorded in the tracker")
	}
}

func TestEditFileOmitsRichEvidenceWhenFormatterReplacesTargetWithOutOfRootSymlink(t *testing.T) {
	requireGofmt(t)
	t.Setenv("ZERO_FORMAT_ON_WRITE", "1")
	root := t.TempDir()
	targetPath := filepath.Join(root, "a.go")
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	trackedPath := filepath.Join(resolvedRoot, "a.go")
	if err := os.WriteFile(targetPath, []byte("package before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outsidePath := filepath.Join(t.TempDir(), "outside.go")
	outsideContent := "package external\n\nconst Secret = \"outside\"\n"
	if err := os.WriteFile(outsidePath, []byte(outsideContent), 0o644); err != nil {
		t.Fatal(err)
	}
	priorRunner := runFormatOnWriteCommand
	runFormatOnWriteCommand = func(context.Context, string, []string, string) error {
		if err := os.Remove(targetPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outsidePath, targetPath); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		return nil
	}
	t.Cleanup(func() { runFormatOnWriteCommand = priorRunner })
	tracker := NewFileTracker()
	read := NewScopedReadFileTool(root, nil).(optionsAwareTool).RunWithOptions(context.Background(), map[string]any{
		"path": "a.go",
	}, RunOptions{FileTracker: tracker})
	if read.Status != StatusOK {
		t.Fatalf("read before edit failed: %s", read.Output)
	}

	diagnosticsCalled := false
	result := NewScopedEditFileTool(root, nil).(optionsAwareTool).RunWithOptions(context.Background(), map[string]any{
		"path": "a.go", "old_string": "before", "new_string": "requested",
	}, RunOptions{FileTracker: tracker, Diagnostics: func(context.Context, string) string {
		diagnosticsCalled = true
		return "must not run"
	}})
	if result.Status != StatusOK || len(result.ChangedFiles) != 1 || len(result.FileDiffs) != 0 || result.Display.Preview != "" {
		t.Fatalf("out-of-root formatter result = status=%s changed=%#v diffs=%#v preview=%q output=%q", result.Status, result.ChangedFiles, result.FileDiffs, result.Display.Preview, result.Output)
	}
	if diagnosticsCalled {
		t.Fatal("diagnostics must not inspect an unverified formatter target")
	}
	if _, tracked := tracker.Version(trackedPath); tracked {
		t.Fatal("out-of-root formatter target must not be recorded in the tracker")
	}
}

func TestWriteFileAcceptsInRootAtomicFormatterReplacement(t *testing.T) {
	requireGofmt(t)
	t.Setenv("ZERO_FORMAT_ON_WRITE", "1")
	root := t.TempDir()
	targetPath := filepath.Join(root, "a.go")
	formatted := "package formatted\n"
	priorRunner := runFormatOnWriteCommand
	runFormatOnWriteCommand = func(context.Context, string, []string, string) error {
		tempPath := filepath.Join(root, "formatter.tmp")
		if err := os.WriteFile(tempPath, []byte(formatted), 0o644); err != nil {
			t.Fatal(err)
		}
		return os.Rename(tempPath, targetPath)
	}
	t.Cleanup(func() { runFormatOnWriteCommand = priorRunner })
	tracker := NewFileTracker()

	result := NewScopedWriteFileTool(root, nil).(optionsAwareTool).RunWithOptions(context.Background(), map[string]any{
		"path": "a.go", "content": "package requested\n",
	}, RunOptions{FileTracker: tracker})
	if result.Status != StatusOK || len(result.FileDiffs) != 1 || result.FileDiffs[0].NewText != formatted {
		t.Fatalf("atomic formatter write = status=%s diffs=%#v output=%q", result.Status, result.FileDiffs, result.Output)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	version, tracked := tracker.Version(filepath.Join(resolvedRoot, "a.go"))
	if !tracked || version.Hash != HashContent([]byte(formatted)) {
		t.Fatalf("atomic formatter tracker = %#v, tracked=%t", version, tracked)
	}
}

func TestEditFileAcceptsInRootAtomicFormatterReplacement(t *testing.T) {
	requireGofmt(t)
	t.Setenv("ZERO_FORMAT_ON_WRITE", "1")
	root := t.TempDir()
	targetPath := filepath.Join(root, "a.go")
	if err := os.WriteFile(targetPath, []byte("package before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	formatted := "package formatted\n"
	priorRunner := runFormatOnWriteCommand
	runFormatOnWriteCommand = func(context.Context, string, []string, string) error {
		tempPath := filepath.Join(root, "formatter.tmp")
		if err := os.WriteFile(tempPath, []byte(formatted), 0o644); err != nil {
			t.Fatal(err)
		}
		return os.Rename(tempPath, targetPath)
	}
	t.Cleanup(func() { runFormatOnWriteCommand = priorRunner })
	tracker := NewFileTracker()
	read := NewScopedReadFileTool(root, nil).(optionsAwareTool).RunWithOptions(context.Background(), map[string]any{
		"path": "a.go",
	}, RunOptions{FileTracker: tracker})
	if read.Status != StatusOK {
		t.Fatalf("read before edit failed: %s", read.Output)
	}

	result := NewScopedEditFileTool(root, nil).(optionsAwareTool).RunWithOptions(context.Background(), map[string]any{
		"path": "a.go", "old_string": "before", "new_string": "requested",
	}, RunOptions{FileTracker: tracker})
	if result.Status != StatusOK || len(result.FileDiffs) != 1 || result.FileDiffs[0].NewText != formatted {
		t.Fatalf("atomic formatter edit = status=%s diffs=%#v output=%q", result.Status, result.FileDiffs, result.Output)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	version, tracked := tracker.Version(filepath.Join(resolvedRoot, "a.go"))
	if !tracked || version.Hash != HashContent([]byte(formatted)) {
		t.Fatalf("atomic formatter tracker = %#v, tracked=%t", version, tracked)
	}
}

// Prettier's JavaScript configs are modules, so loading one is arbitrary code
// execution from the workspace. Every prettier command must therefore disable
// config resolution; this pins that, because a future entry copied without the
// flag would silently reopen the hole.
func TestFormatOnWritePrettierDisablesProjectConfig(t *testing.T) {
	for extension, command := range formatterCommands {
		if command[0] != "prettier" {
			continue
		}
		found := false
		for _, argument := range command[1:] {
			if argument == "--no-config" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("prettier command for %s is %#v; it must pass --no-config so a project .prettierrc.js is never evaluated", extension, command)
		}
	}
}

// A formatter that resolves inside the workspace is repository-controlled. npm
// run puts node_modules/.bin on PATH, so a cloned project can shadow the real
// binary; it must be refused rather than executed with Zero's privileges.
func TestFormatOnWriteRejectsFormatterResolvedInsideWorkspace(t *testing.T) {
	t.Setenv("ZERO_FORMAT_ON_WRITE", "1")
	root := t.TempDir()
	marker := filepath.Join(root, "formatter-ran")
	binaryName := "plantedfmt" + formatterScriptExtension()
	if err := os.WriteFile(filepath.Join(root, binaryName), []byte(plantingFormatterScript(marker)), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	registerFormatterCommand(t, ".plantedfmt", binaryName)

	target := filepath.Join(root, "subject.plantedfmt")
	const written = "written   but   not   formatted\n"
	if err := os.WriteFile(target, []byte(written), 0o644); err != nil {
		t.Fatal(err)
	}

	formatting := maybeFormatWrittenFile(context.Background(), target, written)
	if formatting.Content != written || !formatting.ContentKnown {
		t.Fatalf("workspace-planted formatter result = %q, known=%t", formatting.Content, formatting.ContentKnown)
	}
	if formatting.Skipped != formatSkippedInsideWriteRoot {
		t.Fatalf("workspace-planted formatter skip = %q, want %q", formatting.Skipped, formatSkippedInsideWriteRoot)
	}
	notice := formatting.notice("subject.plantedfmt")
	if !strings.Contains(notice, formatSkippedInsideWriteRoot) {
		t.Fatalf("workspace-planted formatter notice = %q, want the skip reason", notice)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("a formatter binary resolved inside the workspace was executed")
	}
}

// The formatter can evaluate project code, so it must not inherit credentials.
// PATH and ordinary variables survive; the provider keys the sandbox scrubs do
// not.
func TestFormatOnWriteScrubsCredentialsFromFormatterEnvironment(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "envdump")
	installFakeFormatter(t, ".envfmt", "envfmt", envDumpFormatterScript(marker))
	t.Setenv("ZERO_FORMAT_ON_WRITE", "1")
	t.Setenv("OPENAI_API_KEY", "sk-super-secret-value")
	t.Setenv("ZERO_FORMAT_TEST_CONTROL", "present")

	target := filepath.Join(t.TempDir(), "subject.envfmt")
	const written = "x\n"
	if err := os.WriteFile(target, []byte(written), 0o644); err != nil {
		t.Fatal(err)
	}
	formatting := maybeFormatWrittenFile(context.Background(), target, written)
	if formatting.RestoreFailed {
		t.Fatal("formatter failed unexpectedly")
	}

	dumped, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("formatter did not run: %v", err)
	}
	if strings.Contains(string(dumped), "sk-super-secret-value") {
		t.Fatal("formatter inherited a credential from the environment")
	}
	if !strings.Contains(string(dumped), "ZERO_FORMAT_TEST_CONTROL=present") {
		t.Fatal("formatter env dump did not contain the control variable, so the formatter may not have run")
	}
}

// plantingFormatterScript leaves a marker when executed, so a test can prove
// the refusal happened before the binary ran.
func plantingFormatterScript(marker string) string {
	if runtime.GOOS == "windows" {
		return "@echo off\r\necho ran> \"" + marker + "\"\r\nexit /b 0\r\n"
	}
	return "#!/bin/sh\necho ran > \"" + marker + "\"\nexit 0\n"
}

// envDumpFormatterScript writes its environment to marker.
func envDumpFormatterScript(marker string) string {
	if runtime.GOOS == "windows" {
		return "@echo off\r\nset > \"" + marker + "\"\r\nexit /b 0\r\n"
	}
	return "#!/bin/sh\nenv > \"" + marker + "\"\nexit 0\n"
}

// registerFormatterCommand registers an already-on-PATH binary for extension
// for the test's duration.
func registerFormatterCommand(t *testing.T, extension, binaryName string) {
	t.Helper()
	previous, existed := formatterCommands[extension]
	formatterCommands[extension] = []string{binaryName}
	t.Cleanup(func() {
		if existed {
			formatterCommands[extension] = previous
			return
		}
		delete(formatterCommands, extension)
	})
}
