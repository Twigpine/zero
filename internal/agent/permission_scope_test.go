package agent

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/sandbox"
)

func TestPermissionScope(t *testing.T) {
	tests := []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{name: "write file path", tool: "write_file", args: map[string]any{"path": "src/main.go", "content": "x"}, want: "src/main.go"},
		{name: "edit file path", tool: "edit_file", args: map[string]any{"path": "a/b.txt"}, want: "a/b.txt"},
		{name: "bash explicit cwd", tool: "bash", args: map[string]any{"command": "ls", "cwd": "services/api"}, want: "services/api"},
		{name: "bash workspace-root cwd is no scope", tool: "bash", args: map[string]any{"command": "ls", "cwd": "."}, want: ""},
		{name: "no path-like args", tool: "bash", args: map[string]any{"command": "ls"}, want: ""},
		{name: "directory key", tool: "list_directory", args: map[string]any{"directory": "pkg"}, want: "pkg"},
		{name: "non-string path ignored", tool: "write_file", args: map[string]any{"path": 42}, want: ""},
		{name: "path wins over cwd", tool: "x", args: map[string]any{"cwd": "a", "path": "b"}, want: "b"},
		{name: "whitespace path is no scope", tool: "write_file", args: map[string]any{"path": "  "}, want: ""},
		{name: "web fetch host", tool: "web_fetch", args: map[string]any{"url": "https://Example.COM:443/a"}, want: "example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := permissionScope(tt.tool, tt.args); got != tt.want {
				t.Fatalf("permissionScope(%q, %v) = %q, want %q", tt.tool, tt.args, got, tt.want)
			}
		})
	}
}

func TestPermissionScopeTruncatesLongPaths(t *testing.T) {
	long := strings.Repeat("a", 200)
	got := permissionScope("write_file", map[string]any{"path": long})
	if runes := len([]rune(got)); runes > 80 {
		t.Fatalf("scope not truncated: %d runes", runes)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated scope should end with an ellipsis: %q", got)
	}
}

func TestPersistPermissionGrantScopesWebFetchToHost(t *testing.T) {
	store, err := sandbox.NewGrantStore(sandbox.StoreOptions{FilePath: filepath.Join(t.TempDir(), "sandbox-grants.json")})
	if err != nil {
		t.Fatal(err)
	}
	engine := sandbox.NewEngine(sandbox.EngineOptions{
		WorkspaceRoot: t.TempDir(),
		Policy:        sandbox.DefaultPolicy(),
		Store:         store,
	})

	grant, err := persistPermissionGrant("web_fetch", map[string]any{"url": "https://Example.COM:443/docs"}, "trust this host", Options{
		Sandbox:  engine,
		Autonomy: "medium",
	})
	if err != nil {
		t.Fatal(err)
	}
	if grant.ScopeKind != sandbox.ScopeHost || grant.Scope != "example.com" {
		t.Fatalf("grant = %#v, want host-scoped example.com", grant)
	}
	if lookup, err := store.Lookup("web_fetch", "example.com"); err != nil || !lookup.Matched {
		t.Fatalf("expected exact host lookup to match: lookup=%#v err=%v", lookup, err)
	}
	if lookup, err := store.Lookup("web_fetch", "api.example.com"); err != nil || lookup.Matched {
		t.Fatalf("expected subdomain lookup to re-prompt: lookup=%#v err=%v", lookup, err)
	}
}
