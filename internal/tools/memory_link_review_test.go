package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/memory"
)

func TestMemoryReadRefusalNamesTheLinkedPathTruthfully(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "actual-zero")
	if err := os.MkdirAll(filepath.Join(target, "memory"), 0700); err != nil {
		t.Fatal(err)
	}
	linkDirForTool(t, target, filepath.Join(root, ".zero"))
	paths := memory.DefaultPaths(root)
	for _, args := range []map[string]any{{"scope": "project"}, {"scope": "project", "name": "linked"}} {
		got := NewMemoryTool(paths).Run(context.Background(), args)
		if got.Status != StatusError {
			t.Fatalf("linked path accepted: %+v", got)
		}
		for _, lie := range []string{"pointing outside", "refusing to write", "No saved notes yet"} {
			if strings.Contains(got.Output, lie) {
				t.Errorf("false read claim %q: %s", lie, got.Output)
			}
		}
		if !strings.Contains(got.Output, ".zero") || !strings.Contains(got.Output, "link") {
			t.Errorf("failed path lost: %s", got.Output)
		}
	}
}

func TestLinkedNoteCaveatRetainsItsPathWithoutInventingTarget(t *testing.T) {
	root := t.TempDir()
	paths := memory.DefaultPaths(root)
	if _, err := memory.Write(paths, memory.ScopeProject, "readable", "d", "body"); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "body.md")
	if err := os.WriteFile(target, []byte("body"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(paths.ProjectDir, "linked.md")); err != nil {
		t.Skipf("file symlink unavailable: %v", err)
	}
	for _, named := range []bool{false, true} {
		args := map[string]any{"scope": "project"}
		if named {
			args["name"] = "linked"
		}
		result := NewMemoryTool(paths).Run(context.Background(), args)
		if !strings.Contains(result.Output, "linked.md") || strings.Contains(result.Output, "outside the workspace") || strings.Contains(result.Output, "refusing to write") {
			t.Errorf("misleading linked-note read: %s", result.Output)
		}
		if !named && !strings.Contains(result.Output, "readable") {
			t.Error("partial listing lost readable note")
		}
	}
	if err := os.Remove(filepath.Join(paths.ProjectDir, "readable.md")); err != nil {
		t.Fatal(err)
	}
	result := NewMemoryTool(paths).Run(context.Background(), map[string]any{"scope": "project"})
	if result.Status != StatusError || !strings.Contains(result.Output, "listing could not read any notes") || strings.Contains(result.Output, "The memory store is a link") {
		t.Fatalf("one linked note became a whole-store claim: %s", result.Output)
	}
}
