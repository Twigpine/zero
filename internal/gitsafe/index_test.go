package gitsafe

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestIndexEntriesAreLiteralCachedAndNeverRunRepositoryPrograms(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	if !gitAtLeast(t, 1, 9) {
		t.Skip("literal pathspecs require Git 1.9")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = environment()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q")
	for _, dir := range []string{"notes[1]", "notes1", "--others"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, dir, "tracked.md"), []byte("tracked"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	if err := os.WriteFile(filepath.Join(root, "untracked.md"), []byte("untracked"), 0600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "program-ran")
	program := "echo invoked >> '" + strings.ReplaceAll(filepath.ToSlash(marker), "'", "'\\''") + "'"
	for _, key := range []string{"filter.tripwire.clean", "filter.tripwire.smudge", "filter.tripwire.process", "diff.tripwire.command", "diff.tripwire.textconv", "core.fsmonitor"} {
		git("config", key, program)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitattributes"), []byte("* filter=tripwire diff=tripwire\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), ".git"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(t.TempDir(), "empty-index"))
	// Conflicting inherited pathspec modes must not turn a literal query into
	// an empty or wildcard answer about a different set of tracked paths.
	for _, key := range []string{"GIT_LITERAL_PATHSPECS", "GIT_GLOB_PATHSPECS", "GIT_NOGLOB_PATHSPECS", "GIT_ICASE_PATHSPECS"} {
		t.Setenv(key, "1")
	}
	for _, dir := range []string{"notes[1]", "--others"} {
		out, err := IndexEntriesCommand(context.Background(), root, dir).CombinedOutput()
		if err != nil || string(out) != dir+"/tracked.md\x00" {
			t.Fatalf("literal index query %q = %q, %v", dir, out, err)
		}
	}
	if _, err := Command(context.Background(), root, "status", "--porcelain"); !errors.Is(err, ErrRepositoryDefinesCommands) {
		t.Fatalf("status/diff policy was weakened: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("repository program executed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "index"), []byte("corrupt index"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := IndexEntriesCommand(context.Background(), root, ".").CombinedOutput(); err == nil {
		t.Fatalf("corrupt index appeared to be a valid empty index: %q", out)
	}
}
