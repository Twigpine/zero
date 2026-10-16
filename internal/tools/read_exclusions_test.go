package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/sandbox"
)

// denyReadFixture builds a workspace with an ordinary file and a secret subtree,
// returning the resolved workspace root and a sandbox engine whose DenyRead
// covers the secret dir.
func denyReadFixture(t *testing.T) (string, *sandbox.Engine) {
	t.Helper()
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	secret := filepath.Join(ws, "secret")
	if err := os.MkdirAll(secret, 0o755); err != nil {
		t.Fatalf("mkdir secret: %v", err)
	}
	if err := os.WriteFile(filepath.Join(secret, "creds.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatalf("write creds.go: %v", err)
	}

	scope, err := sandbox.NewScope(ws, nil)
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	engine := sandbox.NewEngine(sandbox.EngineOptions{
		WorkspaceRoot: ws,
		Policy: sandbox.Policy{
			Mode:             sandbox.ModeEnforce,
			EnforceWorkspace: true,
			DenyRead:         []string{secret},
		},
		Scope: scope,
	})
	return ws, engine
}

func TestGrepSkipsDenyReadSubtree(t *testing.T) {
	ws, engine := denyReadFixture(t)
	tool, ok := NewScopedGrepTool(ws, nil).(sandboxAwareTool)
	if !ok {
		t.Fatal("grep tool must be sandbox-aware")
	}
	args := map[string]any{"pattern": "package main", "output_mode": "files_with_matches"}

	// Sandboxed: the DenyRead subtree must be excluded from results.
	sandboxed := tool.RunWithSandbox(context.Background(), args, engine)
	if sandboxed.Status != StatusOK {
		t.Fatalf("grep failed: %s", sandboxed.Output)
	}
	if !strings.Contains(sandboxed.Output, "main.go") {
		t.Fatalf("grep must still match the non-denied file, got:\n%s", sandboxed.Output)
	}
	if strings.Contains(sandboxed.Output, "creds.go") {
		t.Fatalf("grep must NOT surface a DenyRead file, got:\n%s", sandboxed.Output)
	}

	// Without a sandbox, the same search includes the secret file (default behavior).
	plain := NewScopedGrepTool(ws, nil).Run(context.Background(), args)
	if !strings.Contains(plain.Output, "creds.go") {
		t.Fatalf("non-sandboxed grep should include the secret file, got:\n%s", plain.Output)
	}
}

func TestGlobSkipsDenyReadSubtree(t *testing.T) {
	ws, engine := denyReadFixture(t)
	tool, ok := NewScopedGlobTool(ws, nil).(sandboxAwareTool)
	if !ok {
		t.Fatal("glob tool must be sandbox-aware")
	}
	args := map[string]any{"pattern": "**/*.go"}

	sandboxed := tool.RunWithSandbox(context.Background(), args, engine)
	if sandboxed.Status != StatusOK {
		t.Fatalf("glob failed: %s", sandboxed.Output)
	}
	if !strings.Contains(sandboxed.Output, "main.go") {
		t.Fatalf("glob must still match the non-denied file, got:\n%s", sandboxed.Output)
	}
	if strings.Contains(sandboxed.Output, "creds.go") {
		t.Fatalf("glob must NOT surface a DenyRead file, got:\n%s", sandboxed.Output)
	}

	plain := NewScopedGlobTool(ws, nil).Run(context.Background(), args)
	if !strings.Contains(plain.Output, "creds.go") {
		t.Fatalf("non-sandboxed glob should include the secret file, got:\n%s", plain.Output)
	}
}

func TestListDirectorySkipsDenyReadSubtree(t *testing.T) {
	ws, engine := denyReadFixture(t)
	registry := NewRegistry()
	registry.Register(NewScopedListDirectoryTool(ws, nil))
	args := map[string]any{"path": ".", "recursive": true, "max_depth": 2}

	sandboxed := registry.RunWithOptions(context.Background(), "list_directory", args, RunOptions{Sandbox: engine})
	if sandboxed.Status != StatusOK {
		t.Fatalf("list_directory failed: %s", sandboxed.Output)
	}
	if !strings.Contains(sandboxed.Output, "main.go") {
		t.Fatalf("list_directory must still show the non-denied file, got:\n%s", sandboxed.Output)
	}
	if strings.Contains(sandboxed.Output, "secret") || strings.Contains(sandboxed.Output, "creds.go") {
		t.Fatalf("list_directory must NOT surface a DenyRead subtree, got:\n%s", sandboxed.Output)
	}

	plain := NewScopedListDirectoryTool(ws, nil).Run(context.Background(), args)
	if !strings.Contains(plain.Output, "secret/") || !strings.Contains(plain.Output, "creds.go") {
		t.Fatalf("non-sandboxed list_directory should include the secret subtree, got:\n%s", plain.Output)
	}
}

func TestListDirectoryDescendsToNestedAllowRead(t *testing.T) {
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	secret := filepath.Join(ws, "secret")
	allowed := filepath.Join(secret, "allowed")
	if err := os.MkdirAll(allowed, 0o755); err != nil {
		t.Fatalf("mkdir allowed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(secret, "hidden.txt"), []byte("hidden\n"), 0o600); err != nil {
		t.Fatalf("write hidden: %v", err)
	}
	if err := os.WriteFile(filepath.Join(allowed, "visible.txt"), []byte("visible\n"), 0o600); err != nil {
		t.Fatalf("write visible: %v", err)
	}
	engine := sandbox.NewEngine(sandbox.EngineOptions{
		WorkspaceRoot: ws,
		Policy: sandbox.Policy{
			Mode:             sandbox.ModeEnforce,
			EnforceWorkspace: true,
			DenyRead:         []string{secret},
			AllowRead:        []string{allowed},
		},
	})
	registry := NewRegistry()
	registry.Register(NewScopedListDirectoryTool(ws, nil))

	result := registry.RunWithOptions(context.Background(), "list_directory", map[string]any{
		"path": ".", "recursive": true, "max_depth": 3,
	}, RunOptions{Sandbox: engine})
	if result.Status != StatusOK {
		t.Fatalf("list_directory failed: %s", result.Output)
	}
	if !strings.Contains(result.Output, "allowed/") || !strings.Contains(result.Output, "visible.txt") {
		t.Fatalf("list_directory must descend to the nested AllowRead subtree, got:\n%s", result.Output)
	}
	if strings.Contains(result.Output, "secret/") || strings.Contains(result.Output, "hidden.txt") {
		t.Fatalf("list_directory must hide denied entries outside the nested AllowRead subtree, got:\n%s", result.Output)
	}
}
