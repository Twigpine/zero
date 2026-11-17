package tui

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestParseGitPorcelain(t *testing.T) {
	out := " M web/app.js\n?? web/new.css\nA  docs/added.md\nR  old.go -> pkg/new.go\n?? \"web/my file.js\"\n\n"
	files := parseGitPorcelain(out)
	if len(files) != 5 {
		t.Fatalf("expected 5 entries, got %d: %+v", len(files), files)
	}
	byPath := map[string]gitSweepFile{}
	for _, f := range files {
		byPath[f.path] = f
	}
	if f := byPath["web/app.js"]; f.created {
		t.Error("modified file must not read as created")
	}
	if f := byPath["web/new.css"]; !f.created {
		t.Error("untracked file should read as created")
	}
	if f := byPath["docs/added.md"]; !f.created {
		t.Error("index-added file should read as created")
	}
	if _, ok := byPath["pkg/new.go"]; !ok {
		t.Errorf("rename should keep the new path: %+v", files)
	}
	if _, ok := byPath["web/my file.js"]; !ok {
		t.Errorf("quoted path should unquote: %+v", files)
	}
}

// TestParseGitNumstat feeds the exact `git diff --numstat -z` record shapes:
// plain "added\tdeleted\tpath\0", binary "-\t-\tpath\0", and the rename form
// "added\tdeleted\t\0preimage\0postimage\0" — the stats must key the postimage
// (the path porcelain/changedFiles report), which the old newline parser lost
// (plain --numstat brace-mangles renames, so they never matched a roster path).
func TestParseGitNumstat(t *testing.T) {
	out := "12\t3\tweb/app.js\x00" +
		"-\t-\tassets/logo.png\x00" +
		"4\t0\t\x00old.go\x00pkg/new.go\x00" +
		"1\t1\tweb/my file.js\x00" // -z paths are never quoted
	stats := parseGitNumstat(out)
	if got := stats["web/app.js"]; got != [2]int{12, 3} {
		t.Errorf("web/app.js = %v, want [12 3]", got)
	}
	if _, ok := stats["assets/logo.png"]; ok {
		t.Error("binary '-' entries must be skipped")
	}
	if got := stats["pkg/new.go"]; got != [2]int{4, 0} {
		t.Errorf("renamed file must key the postimage path = %v, want [4 0]", got)
	}
	if _, ok := stats["old.go"]; ok {
		t.Error("the rename preimage must not appear as its own entry")
	}
	if got := stats["web/my file.js"]; got != [2]int{1, 1} {
		t.Errorf("path with spaces = %v, want [1 1] (no quoting in -z mode)", got)
	}
	// Truncated rename record (missing postimage) must not panic or mis-key.
	if got := parseGitNumstat("4\t0\t\x00old.go"); len(got) != 0 {
		t.Errorf("truncated rename record should be dropped, got %v", got)
	}
}

// TestGitSweepMergeAndBaseline: the baseline snapshot hides pre-existing dirty
// paths; live sweeps upsert only new ones; a failed sweep latches unavailable.
func TestGitSweepMergeAndBaseline(t *testing.T) {
	m := model{}
	m = m.handleGitSweepMsg(gitSweepMsg{baseline: true, ok: true, files: []gitSweepFile{{path: "dirty-before.go"}}})
	if !m.gitFileBaseline["dirty-before.go"] {
		t.Fatal("baseline should record pre-existing dirty paths")
	}

	m = m.handleGitSweepMsg(gitSweepMsg{ok: true, files: []gitSweepFile{
		{path: "dirty-before.go", adds: 9},
		{path: "kanban/board.tsx", created: true, adds: 120},
	}})
	if len(m.gitTouched) != 1 || m.gitTouched[0].path != "kanban/board.tsx" {
		t.Fatalf("only newly dirty paths should merge: %+v", m.gitTouched)
	}

	// Re-sweep updates stats in place, no duplicate.
	m = m.handleGitSweepMsg(gitSweepMsg{ok: true, files: []gitSweepFile{{path: "kanban/board.tsx", created: true, adds: 150, dels: 2}}})
	if len(m.gitTouched) != 1 || m.gitTouched[0].adds != 150 {
		t.Fatalf("re-sweep should upsert stats: %+v", m.gitTouched)
	}

	failed := model{}
	failed = failed.handleGitSweepMsg(gitSweepMsg{baseline: true, ok: false})
	if !failed.gitSweepUnavailable {
		t.Fatal("a failed sweep should latch unavailable")
	}
	if _, cmd := failed.maybeGitSweep(); cmd != nil {
		t.Fatal("no further sweeps once unavailable")
	}
}

// TestMaybeGitSweepGating: no sweep before the baseline answers, none while one
// is in flight, and the in-flight flag sets when one is issued.
func TestMaybeGitSweepGating(t *testing.T) {
	m := model{cwd: "/tmp"}
	if _, cmd := m.maybeGitSweep(); cmd != nil {
		t.Fatal("no baseline yet: sweep must not run")
	}
	m.gitFileBaseline = map[string]bool{}
	next, cmd := m.maybeGitSweep()
	if cmd == nil || !next.gitSweepInFlight {
		t.Fatal("with a baseline, a sweep should be issued and marked in flight")
	}
	if _, again := next.maybeGitSweep(); again != nil {
		t.Fatal("single-flight: no second sweep while one runs")
	}
}

// TestTouchedFilesMergesGitSweep: git-discovered files append below the
// transcript-derived entries, deduped by path (a tool-result entry wins).
func TestTouchedFilesMergesGitSweep(t *testing.T) {
	m := filesPanelTestModel()
	m.gitTouched = []gitSweepFile{
		{path: "kanban/board.tsx", created: true, adds: 120},
		{path: "web/app.js", adds: 999}, // duplicate of a transcript entry
	}
	files := m.touchedFiles()
	var kanban *touchedFile
	appCount := 0
	for i := range files {
		if files[i].path == "kanban/board.tsx" {
			kanban = &files[i]
		}
		if files[i].path == "web/app.js" {
			appCount++
			if files[i].adds == 999 {
				t.Error("transcript-derived entry should win over the git duplicate")
			}
		}
	}
	if kanban == nil || !kanban.created || kanban.lastRowIndex != -1 {
		t.Fatalf("git-only file should merge with created badge and no row anchor: %+v", files)
	}
	if appCount != 1 {
		t.Fatalf("web/app.js should appear exactly once, got %d", appCount)
	}
}

// TestOpenFileViewGitOnlyFallsBackToFull: a file with no edit cards (git sweep
// discovery) opens straight on the full-file view instead of an empty diff.
func TestOpenFileViewGitOnlyFallsBackToFull(t *testing.T) {
	m := filesPanelTestModel()
	m.gitTouched = []gitSweepFile{{path: "kanban/board.tsx", created: true}}
	m, _ = m.openFileView("kanban/board.tsx")
	if m.fileView.mode != fileViewFull {
		t.Fatal("git-only file should open in full mode")
	}
	m = m.exitFileView()
	m, _ = m.openFileView("web/app.js")
	if m.fileView.mode != fileViewDiff {
		t.Fatal("a file with edit cards still opens in diff mode")
	}
}

// TestGitSweepCmdAgainstRealRepo: end-to-end against a real git repo — the
// baseline sees pre-existing dirt, a post-mutation sweep reports the new file
// as created and the modified file with its numstat.
func TestGitSweepCmdAgainstRealRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q")
	write("tracked.txt", "one\ntwo\n")
	run("add", ".")
	run("commit", "-q", "-m", "seed")
	write("pre-existing.txt", "dirt\n") // dirty BEFORE the TUI "opens"

	baseline := gitSweepCmd(context.TODO(), dir, true)().(gitSweepMsg)
	if !baseline.ok || len(baseline.files) != 1 || baseline.files[0].path != "pre-existing.txt" {
		t.Fatalf("baseline should see only the pre-existing dirt: %+v", baseline)
	}

	// The "agent" now scaffolds via the shell.
	write("scaffolded.txt", "hello\n")
	write("tracked.txt", "one\ntwo\nthree\nfour\n")

	sweep := gitSweepCmd(context.TODO(), dir, false)().(gitSweepMsg)
	if !sweep.ok {
		t.Fatal("sweep failed")
	}
	byPath := map[string]gitSweepFile{}
	for _, f := range sweep.files {
		byPath[f.path] = f
	}
	if f, ok := byPath["scaffolded.txt"]; !ok || !f.created {
		t.Fatalf("scaffolded file should report created: %+v", sweep.files)
	}
	if f := byPath["tracked.txt"]; f.adds != 2 || f.dels != 0 {
		t.Fatalf("tracked.txt numstat = +%d −%d, want +2 −0", f.adds, f.dels)
	}

	// A rename WITH edits: porcelain reports the new path, and the -z numstat
	// rename record must resolve to the same (postimage) path so the diffstat
	// actually attaches — the plain --numstat form brace-mangles the path and
	// the counts were silently lost.
	run("add", ".")
	run("commit", "-q", "-m", "second")
	run("mv", "tracked.txt", "renamed.txt")
	write("renamed.txt", "one\ntwo\nthree\nfour\nfive\n")
	renameSweep := gitSweepCmd(context.TODO(), dir, false)().(gitSweepMsg)
	if !renameSweep.ok {
		t.Fatal("rename sweep failed")
	}
	renamed := map[string]gitSweepFile{}
	for _, f := range renameSweep.files {
		renamed[f.path] = f
	}
	if f, ok := renamed["renamed.txt"]; !ok || f.adds != 1 {
		t.Fatalf("renamed file should carry its numstat under the new path, got %+v", renameSweep.files)
	}

	// Not-a-repo → ok=false (the sweep latches off).
	if msg := gitSweepCmd(context.TODO(), t.TempDir(), false)().(gitSweepMsg); msg.ok {
		t.Fatal("a non-repo should report ok=false")
	}
}
