package tui

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/agent"
)

// FIX 3 (safety): "!cmd" runs only in unsafe permission mode; auto/ask gate it.
func TestBashEscapeGatedByPermissionMode(t *testing.T) {
	lastSystemText := func(m model) string {
		for i := len(m.transcript) - 1; i >= 0; i-- {
			if m.transcript[i].kind == rowSystem {
				return m.transcript[i].text
			}
		}
		return ""
	}

	for _, mode := range []agent.PermissionMode{agent.PermissionModeAuto, agent.PermissionModeAsk} {
		m := newModel(context.Background(), Options{PermissionMode: mode})
		m.input.SetValue("!rm -rf /")
		updated, cmd := m.handleSubmit()
		if cmd != nil {
			t.Fatalf("%s mode: !cmd must be gated (nil cmd), got a command to run", mode)
		}
		if msg := lastSystemText(updated.(model)); !strings.Contains(msg, "disabled") || !strings.Contains(msg, "unsafe") {
			t.Fatalf("%s mode: expected a gate message naming unsafe, got %q", mode, msg)
		}
	}

	m := newModel(context.Background(), Options{PermissionMode: agent.PermissionModeUnsafe})
	m.input.SetValue("!echo hi")
	_, cmd := m.handleSubmit()
	if cmd == nil {
		t.Fatal("unsafe mode: !cmd should execute (non-nil cmd)")
	}
}

// The "!" escape must use the same detected host shell as the agent tools.
func TestEscapeShellWrapsCommandForPlatform(t *testing.T) {
	name, args := escapeShell("echo hi")
	if name == "" {
		t.Fatal("escapeShell returned an empty executable")
	}
	if len(args) == 0 || !strings.Contains(args[len(args)-1], "echo hi") {
		t.Fatalf("escapeShell args = %v, want the command in the final arg", args)
	}
	if runtime.GOOS == "windows" {
		lowerName := strings.ToLower(filepath.Base(name))
		if lowerName != "pwsh.exe" && lowerName != "powershell.exe" && lowerName != "cmd.exe" {
			t.Fatalf("windows shell = %q %v, want PowerShell or cmd.exe fallback", name, args)
		}
	} else if name != "/bin/sh" || args[0] != "-c" {
		t.Fatalf("unix shell = %q %v, want /bin/sh -c", name, args)
	}
}

// FIX 1: a bare "/" surfaces the full command palette (was suppressed).
func TestBareSlashListsCommandPalette(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m = typeRunes(t, m, "/")
	if !m.suggestionsActive() {
		t.Fatal("a bare / should surface the command palette")
	}
	if len(m.suggestions) == 0 {
		t.Fatal("expected command suggestions for a bare /")
	}
	if m.suggestionsAreFiles {
		t.Fatal("a bare / should be command suggestions, not files")
	}
}

// FIX 4: "@" helpers — trailing-token detection and replacement.
func TestTrailingAtToken(t *testing.T) {
	cases := []struct {
		in    string
		token string
		ok    bool
	}{
		{"@", "", true},
		{"@foo", "foo", true},
		{"read @foo/bar", "foo/bar", true},
		{"hello", "", false},
		{"read @foo done", "", false}, // trailing word is "done", not an @token
		{"", "", false},
	}
	for _, c := range cases {
		token, ok := trailingAtToken(c.in)
		if ok != c.ok || token != c.token {
			t.Errorf("trailingAtToken(%q) = (%q,%v), want (%q,%v)", c.in, token, ok, c.token, c.ok)
		}
	}
}

func TestReplaceTrailingAtToken(t *testing.T) {
	if got := replaceTrailingAtToken("read @fo", "@internal/loop.go"); got != "read @internal/loop.go" {
		t.Fatalf("replaceTrailingAtToken mid-prompt = %q", got)
	}
	if got := replaceTrailingAtToken("@m", "@main.go"); got != "@main.go" {
		t.Fatalf("replaceTrailingAtToken whole-input = %q", got)
	}
}

// FIX 4: "@" surfaces workspace files, filters, and skips VCS dirs.
func TestFileSuggestionsListsAndFiltersWorkspaceFiles(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "main.go"))
	mustWrite(t, filepath.Join(dir, "internal", "loop.go"))
	mustWrite(t, filepath.Join(dir, ".git", "config")) // must be skipped

	all := suggestionNames2(fileSuggestions(dir, ""))
	if !contains(all, "@main.go") || !contains(all, "@internal/loop.go") {
		t.Fatalf("expected workspace files, got %v", all)
	}
	for _, s := range all {
		if strings.Contains(s, ".git/") {
			t.Fatalf("file suggestions must skip .git, got %v", all)
		}
	}
	filtered := suggestionNames2(fileSuggestions(dir, "loop"))
	if !contains(filtered, "@internal/loop.go") || contains(filtered, "@main.go") {
		t.Fatalf("filter 'loop' = %v, want only loop.go", filtered)
	}
}

// FIX 3: "!cmd" parses as a shell escape, not a chat prompt.
func TestParseCommandBangIsShellEscape(t *testing.T) {
	got := parseCommand("!ls -la")
	if got.kind != commandBash || got.text != "ls -la" {
		t.Fatalf("parseCommand(!ls -la) = {kind:%v text:%q}, want commandBash/\"ls -la\"", got.kind, got.text)
	}
	if parseCommand("/help").kind == commandBash {
		t.Fatal("/help must not parse as bash")
	}
	if parseCommand("just chatting").kind != commandPrompt {
		t.Fatal("plain text must still parse as a prompt")
	}
}

func mustWrite(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func suggestionNames2(s []commandSuggestion) []string {
	out := make([]string, 0, len(s))
	for _, x := range s {
		out = append(out, x.Name)
	}
	return out
}

func TestGitBranchInSubdirectory(t *testing.T) {
	root := t.TempDir()
	gitdir := filepath.Join(root, ".git")
	if err := os.MkdirAll(gitdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitdir, "HEAD"), []byte("ref: refs/heads/feature-tui-branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(root, "nested", "sub", "dir")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := gitBranch(subdir); got != "feature-tui-branch" {
		t.Fatalf("gitBranch in subdirectory = %q, want feature-tui-branch", got)
	}
}
