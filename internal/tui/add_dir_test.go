package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/sandbox"
)

func TestParseCommandRecognizesAddDir(t *testing.T) {
	got := parseCommand("/add-dir /tmp/extra")
	if got.kind != commandAddDir || got.text != "/tmp/extra" {
		t.Fatalf("parseCommand(/add-dir /tmp/extra) = kind=%v text=%q, want kind=%v text=%q",
			got.kind, got.text, commandAddDir, "/tmp/extra")
	}
}

// newAddDirTestModel builds a minimal model whose agent options carry a real
// sandbox engine with a shared scope rooted at a fresh temp workspace, so
// handleAddDirCommand exercises the same Scope.Add path production uses.
func newAddDirTestModel(t *testing.T) (model, *sandbox.Scope) {
	t.Helper()
	workspace := t.TempDir()
	scope, err := sandbox.NewScope(workspace, nil)
	if err != nil {
		t.Fatalf("NewScope(%q): %v", workspace, err)
	}
	engine := sandbox.NewEngine(sandbox.EngineOptions{WorkspaceRoot: workspace, Scope: scope})
	return model{agentOptions: agent.Options{Sandbox: engine}}, scope
}

func TestHandleAddDirCommand(t *testing.T) {
	m, scope := newAddDirTestModel(t)
	extra := tempDirOutsideDefaultTemp(t)
	initialRootCount := len(scope.Roots())

	// Granting a directory widens the shared scope and confirms inline.
	next := m.handleAddDirCommand(extra)
	if roots := scope.Roots(); len(roots) != initialRootCount+1 {
		t.Fatalf("expected one additional write root after grant, got %#v", roots)
	}
	notice := next.transientNotice.text
	if !strings.Contains(notice, "Write access added") || !strings.Contains(notice, "session only") {
		t.Fatalf("grant notice = %q, want it to mention the grant and its session-only lifetime", notice)
	}

	// Bare form lists the current write roots, workspace first.
	bare := m.handleAddDirCommand("")
	if notice := lastTranscriptText(bare); !strings.Contains(notice, scope.WorkspaceRoot()) {
		t.Fatalf("bare /add-dir notice = %q, want it to list the workspace root %q", notice, scope.WorkspaceRoot())
	}

	// A nonexistent path surfaces the scope error inline.
	bad := m.handleAddDirCommand(filepath.Join(scope.WorkspaceRoot(), "does-not-exist"))
	if notice := lastTranscriptText(bad); !strings.Contains(notice, "add-dir:") {
		t.Fatalf("bad-path notice = %q, want an add-dir: error", notice)
	}
}

func tempDirOutsideDefaultTemp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", ".zero-sandbox-outside-")
	if err != nil {
		t.Fatalf("MkdirTemp outside default temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("Abs(%q): %v", dir, err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return filepath.Clean(abs)
}
