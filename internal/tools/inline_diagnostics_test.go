package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A wired Diagnostics callback must surface its block in the tool output for
// both mutating tools, and a clean file (empty block) must leave the output
// byte-identical to the pre-diagnostics behavior.
func TestMutatingToolsAppendInlineDiagnostics(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diagnostics := func(_ context.Context, absPath string) string {
		return absPath + ":1:1: error: undefined: x"
	}

	editResult := NewScopedEditFileTool(dir, nil).(optionsAwareTool).RunWithOptions(context.Background(), map[string]any{
		"path":       "a.go",
		"old_string": "package a",
		"new_string": "package b",
	}, RunOptions{Diagnostics: diagnostics})
	if editResult.Status != StatusOK {
		t.Fatalf("edit failed: %q", editResult.Output)
	}
	if !strings.Contains(editResult.Output, "Diagnostics in a.go after this change") || !strings.Contains(editResult.Output, "undefined: x") {
		t.Fatalf("edit output missing diagnostics block: %q", editResult.Output)
	}

	writeResult := NewScopedWriteFileTool(dir, nil).(optionsAwareTool).RunWithOptions(context.Background(), map[string]any{
		"path":    "b.go",
		"content": "package b\n",
	}, RunOptions{Diagnostics: diagnostics})
	if writeResult.Status != StatusOK {
		t.Fatalf("write failed: %q", writeResult.Output)
	}
	if !strings.Contains(writeResult.Output, "Diagnostics in b.go after this change") {
		t.Fatalf("write output missing diagnostics block: %q", writeResult.Output)
	}

	clean := NewScopedWriteFileTool(dir, nil).(optionsAwareTool).RunWithOptions(context.Background(), map[string]any{
		"path":    "c.go",
		"content": "package c\n",
	}, RunOptions{Diagnostics: func(context.Context, string) string { return "" }})
	if strings.Contains(clean.Output, "Diagnostics") {
		t.Fatalf("clean file must not gain a diagnostics block: %q", clean.Output)
	}
}

func TestProtectedCredentialDoesNotDisableInlineDiagnosticsForOtherFiles(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "bridge-token")
	if err := os.WriteFile(token, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZERO_DAEMON_REMOTE_TOKEN", "")
	t.Setenv("ZERO_DAEMON_REMOTE_TOKEN_FILE", token)
	t.Setenv("ZERO_INTERNAL_DAEMON_REMOTE_TOKEN_FILE_RESOLVED", "")
	diagnostics := func(context.Context, string) string { return "ordinary-file diagnostic" }

	edit := NewScopedEditFileTool(dir, nil).(optionsAwareTool).RunWithOptions(context.Background(), map[string]any{
		"path": "a.go", "old_string": "package a", "new_string": "package b",
	}, RunOptions{Diagnostics: diagnostics})
	if edit.Status != StatusOK || !strings.Contains(edit.Output, "ordinary-file diagnostic") {
		t.Fatalf("edit diagnostics were suppressed by unrelated token: %+v", edit)
	}
	write := NewScopedWriteFileTool(dir, nil).(optionsAwareTool).RunWithOptions(context.Background(), map[string]any{
		"path": "b.go", "content": "package b\n",
	}, RunOptions{Diagnostics: diagnostics})
	if write.Status != StatusOK || !strings.Contains(write.Output, "ordinary-file diagnostic") {
		t.Fatalf("write diagnostics were suppressed by unrelated token: %+v", write)
	}
}
