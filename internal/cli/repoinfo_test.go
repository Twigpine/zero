package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/repoinfo"
)

func TestParseRepoInfoArgs(t *testing.T) {
	opts, help, err := parseRepoInfoArgs([]string{"--json", "--cwd", "/tmp/x"})
	if err != nil || help {
		t.Fatalf("parse: help=%v err=%v", help, err)
	}
	if !opts.json || opts.cwd != "/tmp/x" {
		t.Fatalf("got %+v", opts)
	}
	if _, h, _ := parseRepoInfoArgs([]string{"--help"}); !h {
		t.Fatal("expected help")
	}
	if _, _, err := parseRepoInfoArgs([]string{"--bogus"}); err == nil {
		t.Fatal("expected error for unknown flag")
	}
}

func TestFormatRepoInfoText(t *testing.T) {
	age := 42
	contrib := 3
	info := repoInfoTestData(age, contrib)
	out := formatRepoInfo(info)
	for _, want := range []string{"Files", "TypeScript", "main", "GitHub Actions", "42", "3", "Commits:      100", "Branches:     5", "Tags:         10"} {
		if !strings.Contains(out, want) {
			t.Fatalf("text output missing %q:\n%s", want, out)
		}
	}
}

func TestRunRepoInfoJSON(t *testing.T) {
	dir := initTempGitRepo(t) // hermetic: do not depend on running inside a checkout
	var stdout, stderr bytes.Buffer
	code := runRepoInfo([]string{"--json", "--cwd", dir}, &stdout, &stderr, testRepoInfoDeps())
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	var info map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &info); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout.String())
	}
	if info["hasGit"] != true {
		t.Fatalf("expected hasGit=true, got %v", info["hasGit"])
	}
	if info["primaryLanguage"] != "Go" {
		t.Fatalf("expected primaryLanguage Go, got %v", info["primaryLanguage"])
	}
	if strings.Contains(stdout.String(), "ghp_TESTSECRET") {
		t.Fatalf("credential leaked into --json output:\n%s", stdout.String())
	}
	if info["remoteURL"] != "https://github.com/o/r.git" {
		t.Fatalf("remoteURL=%v want sanitized (no credentials)", info["remoteURL"])
	}
}

func TestRunRepoInfoTextNoCredentials(t *testing.T) {
	dir := initTempGitRepo(t)
	var stdout, stderr bytes.Buffer
	code := runRepoInfo([]string{"--cwd", dir}, &stdout, &stderr, testRepoInfoDeps())
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "ghp_TESTSECRET") {
		t.Fatalf("credential leaked into text output:\n%s", out)
	}
	if !strings.Contains(out, "github.com/o/r.git") {
		t.Fatalf("expected sanitized remote in text output:\n%s", out)
	}
}

// initTempGitRepo creates a throwaway git repo with a single Go file committed,
// so the command can be exercised end-to-end without depending on the test being
// run inside a git checkout.
func initTempGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "init")
	// A remote whose URL embeds a credential, to prove the report never leaks it.
	git("remote", "add", "origin", "https://x-access-token:ghp_TESTSECRET@github.com/o/r.git")
	return dir
}

func repoInfoTestData(age, contrib int) repoinfo.Info {
	commits := 100
	branches := 5
	tags := 10
	return repoinfo.Info{
		FileCount: 10, DirectoryCount: 3, MaxDepth: 2, LOCEstimate: 500,
		Languages:       []repoinfo.LangStat{{Name: "TypeScript", LOCEstimate: 400, FileCount: 4}},
		PrimaryLanguage: "TypeScript", LanguageCount: 1,
		WorkspaceType: "none", CICD: []string{"GitHub Actions"},
		HasGit: true, Branch: "main", AgeDays: &age, Contributors90d: &contrib,
		CommitCount: &commits, BranchCount: &branches, TagCount: &tags,
	}
}

func testRepoInfoDeps() appDeps {
	return defaultAppDeps()
}
