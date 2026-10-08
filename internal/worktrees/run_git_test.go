package worktrees

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// The constructor every git call goes through must carry the hardening, so a
// future call site cannot get an unhardened command by using it.
func TestTheGitConstructorHardens(t *testing.T) {
	command := newHardenedCommand(context.Background(), t.TempDir(), "git", "status")
	if command.WaitDelay == 0 {
		t.Fatal("WaitDelay is unset; a cancelled git can block Wait indefinitely")
	}
	if command.Dir == "" {
		t.Fatal("the working directory was dropped")
	}
	// The process-group half is POSIX-only and is asserted in
	// run_git_unix_test.go, which can name Setpgid at all — this file must
	// compile on Windows, where it does not exist.
	if runtime.GOOS != "windows" && command.Cancel == nil {
		t.Fatal("Cancel is unset; the process group is never signalled")
	}
}

// ...and the git runner itself still works, so the hardening did not break the
// ordinary path.
func TestHardenedGitStillRuns(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("no git: %v", err)
	}
	result, err := defaultRunGit(context.Background(), t.TempDir(), "--version")
	if err != nil {
		t.Fatalf("git --version: %v", err)
	}
	if result.ExitCode != 0 || result.Stdout == "" {
		t.Fatalf("result = %+v", result)
	}
}

// EXACTLY ONE PLACE BUILDS A SUBPROCESS IN THIS PACKAGE.
//
// Every behavioural test above goes through newHardenedCommand, so all of them
// pass against a defaultRunGit that quietly builds its own unhardened command —
// which is the wiring gap this feature has produced five times, and the only one
// no runtime assertion here can reach: defaultRunGit returns a result, not the
// command it used.
//
// So the invariant is enforced at the SOURCE. This is the small, local form of
// the AST enforcement the repo-wide hardening sweep would need, applied to the
// one package a plan reaches.
func TestOnlyTheHardenedConstructorBuildsSubprocesses(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	offenders := []string{}
	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for index, line := range strings.Split(string(raw), "\n") {
			if !strings.Contains(line, "exec.Command") {
				continue
			}
			// The one permitted site is inside newHardenedCommand, which
			// hardens what it builds.
			if name == "worktrees.go" && strings.Contains(line, "exec.CommandContext(ctx, name, args...)") {
				continue
			}
			offenders = append(offenders, fmt.Sprintf("%s:%d: %s", name, index+1, strings.TrimSpace(line)))
		}
	}
	if checked == 0 {
		t.Fatal("no source files were scanned; this test checked nothing")
	}
	if len(offenders) > 0 {
		t.Fatalf("a subprocess is built outside newHardenedCommand, so it is not hardened:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}
