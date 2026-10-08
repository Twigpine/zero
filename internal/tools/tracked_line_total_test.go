package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

func TestTrackedLineTotalMatchesReadFileStats(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{"trailing": "a\nb\nc\n", "unterminated": "a\nb\nc", "single": "x\n", "empty": "", "crlf": "a\r\nb\r\n"} {
		path := filepath.Join(root, name+".txt")
		writeTestFile(t, path, content)
		stats, err := scanReadFileStats(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := trackedLineTotal(content); got != stats.lines {
			t.Fatalf("%s: trackedLineTotal = %d, read_file reports %d", name, got, stats.lines)
		}
	}
}

// A whole-file read, an edit, then a partial re-read must keep the file "seen
// whole": the writer and reader now agree on the line total, so the partial
// read accumulates instead of resetting and a later edit far from the re-read
// window is not refused.
func TestEditThenPartialReadKeepsWholeFileObservation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "notes.txt")
	content := ""
	for i := 1; i <= 40; i++ {
		content += fmt.Sprintf("line %02d\n", i)
	}
	writeTestFile(t, path, content)
	tracker := NewFileTracker()
	registry := NewRegistry()
	registry.Register(NewScopedReadFileTool(root, nil))
	registry.Register(NewScopedEditFileTool(root, nil))
	opts := RunOptions{PermissionGranted: true, FileTracker: tracker}

	if r := registry.RunWithOptions(context.Background(), "read_file", map[string]any{"path": "notes.txt"}, opts); r.Status != StatusOK {
		t.Fatalf("read: %s", r.Output)
	}
	if r := registry.RunWithOptions(context.Background(), "edit_file", map[string]any{"path": "notes.txt", "old_string": "line 01\nline 02\n", "new_string": "line 01\nline two\n"}, opts); r.Status != StatusOK {
		t.Fatalf("first edit: %s", r.Output)
	}
	if r := registry.RunWithOptions(context.Background(), "read_file", map[string]any{"path": "notes.txt", "limit": 5}, opts); r.Status != StatusOK {
		t.Fatalf("partial read: %s", r.Output)
	}
	absolute, _ := filepath.EvalSymlinks(path)
	if !tracker.SeenWhole(absolute) {
		t.Fatal("partial re-read of an unchanged file must not discard whole-file knowledge")
	}
	if r := registry.RunWithOptions(context.Background(), "edit_file", map[string]any{"path": "notes.txt", "old_string": "line 39\nline 40\n", "new_string": "line 39\nline forty\n"}, opts); r.Status != StatusOK {
		t.Fatalf("edit far from the re-read window must succeed: %s", r.Output)
	}
}
