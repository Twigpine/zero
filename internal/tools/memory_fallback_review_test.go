package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/memory"
)

func TestNamedMemoryFallbackKeepsEarlierReadFailureVisible(t *testing.T) {
	paths := memory.DefaultPaths(t.TempDir())
	if _, err := memory.Write(paths, memory.ScopeProject, "shared", "d", "project note"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.LocalDir, 0700); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(paths.LocalDir, "shared.md")
	if err := os.WriteFile(local, []byte(strings.Repeat("x", 65*1024)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.Read(paths, memory.ScopeLocal, "shared"); !errors.Is(err, memory.ErrTooLarge) {
		t.Fatalf("fixture does not reach the failed-read path: %v", err)
	}
	tool := NewMemoryTool(paths)
	result := tool.Run(context.Background(), map[string]any{"name": "shared"})
	if result.Status != StatusOK || !strings.Contains(result.Output, "project note") {
		t.Fatalf("readable fallback lost: %+v", result)
	}
	if !strings.Contains(result.Output, "local") || !strings.Contains(result.Output, "shared.md") || !strings.Contains(result.Output, memory.ErrTooLarge.Error()) {
		t.Errorf("earlier local read failure hidden: %s", result.Output)
	}
	project := tool.Run(context.Background(), map[string]any{"name": "shared", "scope": "project"})
	if project.Status != StatusOK || strings.Contains(project.Output, memory.ErrTooLarge.Error()) {
		t.Errorf("explicit project read inherited a different scope's failure: %+v", project)
	}
	if err := os.Remove(local); err != nil {
		t.Fatal(err)
	}
	missing := tool.Run(context.Background(), map[string]any{"name": "shared"})
	if missing.Status != StatusOK || strings.Contains(missing.Output, "failed") || strings.Contains(missing.Output, memory.ErrTooLarge.Error()) {
		t.Errorf("ordinary missing local note became a caveat: %+v", missing)
	}
}
