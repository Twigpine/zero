package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/sandbox"
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
