package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLSPNavigateConfinesPathToWorkspace(t *testing.T) {
	// Regression: lsp_navigate must enforce the same workspace confinement its
	// sibling read-only tools do — an absolute or `..`-escaping path must be
	// rejected before any read or LSP open, so it can't exfiltrate files off disk
	// (reachable via indirect prompt injection).
	root := t.TempDir()
	tool := NewScopedLSPNavigateTool(root, nil) // workspace-only scope (nil)
	escapes := []string{
		"/etc/passwd",
		"../../../../etc/passwd",
		"../../../../../../etc/ssh/id_rsa",
	}
	for _, p := range escapes {
		for _, op := range []string{"definition", "workspace_symbol"} {
			args := map[string]any{"op": op, "path": p}
			if op == "definition" {
				args["line"], args["character"] = 1, 1
			} else {
				args["query"] = "x"
			}
			got := tool.Run(context.Background(), args)
			if got.Status != StatusError {
				t.Fatalf("path %q op %s: expected StatusError (confinement), got %s: %s", p, op, got.Status, got.Output)
			}
			// The error must not leak the resolved absolute path.
			if strings.Contains(got.Output, "/etc/") && strings.Contains(got.Output, root) {
				t.Fatalf("path %q: error should not echo an absolute out-of-workspace path: %q", p, got.Output)
			}
		}
	}
}

func TestLSPNavigateRejectsBadArgs(t *testing.T) {
	tool := NewScopedLSPNavigateTool(t.TempDir(), nil)
	cases := []map[string]any{
		{},                   // missing op + path
		{"op": "definition"}, // missing path
		{"op": "workspace_symbol", "path": "x.go"},                 // missing query
		{"op": "definition", "path": "x.go"},                       // missing line
		{"op": "bogus", "path": "x.go", "line": 1, "character": 1}, // unknown op
	}
	for i, args := range cases {
		if got := tool.Run(context.Background(), args); got.Status != StatusError {
			t.Fatalf("case %d: expected StatusError for %#v, got %s: %s", i, args, got.Status, got.Output)
		}
	}
}

func TestLSPNavigateUnsupportedFileDegrades(t *testing.T) {
	root := t.TempDir()
	// A file type with no language server → the tool returns OK with a clear
	// "unavailable, fall back to grep" message rather than an error.
	path := filepath.Join(root, "notes.unknownext")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewScopedLSPNavigateTool(root, nil)
	got := tool.Run(context.Background(), map[string]any{
		"op": "definition", "path": "notes.unknownext", "line": 1, "character": 1,
	})
	if got.Status != StatusOK {
		t.Fatalf("unsupported file should degrade to StatusOK, got %s: %s", got.Status, got.Output)
	}
	if !strings.Contains(got.Output, "unavailable") {
		t.Fatalf("expected an 'unavailable' message, got %q", got.Output)
	}
}

func TestLSPNavigateIsReadOnly(t *testing.T) {
	tool := NewScopedLSPNavigateTool(t.TempDir(), nil)
	if s := tool.Safety(); s.SideEffect != SideEffectRead || s.Permission != PermissionAllow {
		t.Fatalf("lsp_navigate should be read-only/allow, got %+v", s)
	}
	if tool.Name() != "lsp_navigate" {
		t.Fatalf("name = %q", tool.Name())
	}
}

func TestReadWorkspaceFileRejectsPostResolutionSwap(t *testing.T) {
	for _, kind := range []string{"hardlink", "escaping symlink", "ordinary"} {
		t.Run(kind, func(t *testing.T) {
			ws, token, _ := daemonTokenFixture(t)
			path := filepath.Join(ws, "candidate")
			if err := os.WriteFile(path, []byte("ordinary"), 0o600); err != nil {
				t.Fatal(err)
			}
			resolved, _, err := resolveScopedReadPath(ws, nil, path)
			if err != nil {
				t.Fatal(err)
			}
			if kind != "ordinary" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if kind == "hardlink" {
					err = os.Link(token, path)
				} else {
					outside := filepath.Join(t.TempDir(), "outside")
					if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
						t.Fatal(err)
					}
					err = os.Symlink(outside, path)
				}
				if err != nil {
					t.Skipf("alias unavailable: %v", err)
				}
			}
			text, err := readWorkspaceFile(resolved, ws)
			if kind == "ordinary" {
				if err != nil || text != "ordinary" {
					t.Fatalf("ordinary read: %q, %v", text, err)
				}
			} else if err == nil || text != "" {
				t.Fatalf("post-resolution swap read: %q, %v", text, err)
			}
		})
	}
}
