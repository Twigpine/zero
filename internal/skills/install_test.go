package skills

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/installtxn"
)

// initGitSkillRepo creates a real local git repo holding a skill and returns a
// file:// URL for it, so the DEFAULT git runner (system git) is exercised end to
// end rather than only the injected runner. The test is skipped when git is
// unavailable.
func initGitSkillRepo(t *testing.T, content string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	writeSourceSkill(t, repo, content)
	run("add", "-A")
	run("commit", "-qm", "init")
	return "file://" + repo
}

func TestInstallFromRealLocalGitRepo(t *testing.T) {
	destDir := t.TempDir()
	url := initGitSkillRepo(t, "---\nname: from-git\ndescription: cloned\n---\nbody from git\n")

	result, err := Install(context.Background(), InstallOptions{Source: url, Dir: destDir})
	if err != nil {
		t.Fatalf("Install from git: %v", err)
	}
	if result.Name != "from-git" {
		t.Fatalf("Name = %q, want from-git", result.Name)
	}
	got, ok := Get(destDir, "from-git")
	if !ok || !strings.Contains(got.Content, "body from git") {
		t.Fatalf("installed git skill not discoverable: ok=%v skill=%+v", ok, got)
	}
	// The .git clone metadata must not leak into the skills dir.
	if _, err := os.Stat(filepath.Join(destDir, "from-git", ".git")); err == nil {
		t.Fatalf(".git metadata must not be installed into the skills dir")
	}
}

// writeSourceSkill lays out a candidate skill directory containing a SKILL.md so
// it can be used as a local install source.
func writeSourceSkill(t *testing.T, dir string, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, skillFileName), []byte(content), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	return dir
}

// writeWorkspaceMarker plants the ownership marker installtxn writes inside a
// workspace before it moves any tree. Recovery acts on a workspace only when
// this marker proves the workspace is one of its own, so a planted interrupted
// state is invisible to it without one.
func writeWorkspaceMarker(t *testing.T, workspace string, name string) {
	t.Helper()
	marker := []byte("zero-install-txn v1\ntarget " + name + "\n")
	if err := os.WriteFile(filepath.Join(workspace, ".zero-install-txn"), marker, 0o600); err != nil {
		t.Fatalf("write workspace marker: %v", err)
	}
}

func TestInstallCopiesLocalSkillAndRecordsHash(t *testing.T) {
	destDir := t.TempDir()
	source := writeSourceSkill(t, filepath.Join(t.TempDir(), "src"),
		"---\nname: confirmation-policy\ndescription: Ask first.\n---\nAsk before risky actions.\n")

	result, err := Install(context.Background(), InstallOptions{Source: source, Dir: destDir})
	if err != nil {
		t.Fatalf("Install returned error: %v", err)
	}
	if result.Name != "confirmation-policy" {
		t.Fatalf("Name = %q, want confirmation-policy", result.Name)
	}
	if result.Hash == "" {
		t.Fatalf("expected a recorded content hash")
	}
	if result.Updated {
		t.Fatalf("first install should not be flagged as an update")
	}

	// The skill is discoverable through the normal loader.
	got, ok := Get(destDir, "confirmation-policy")
	if !ok {
		t.Fatalf("installed skill not discoverable via Get")
	}
	if !strings.Contains(got.Content, "Ask before risky actions.") {
		t.Fatalf("installed content unexpected: %q", got.Content)
	}

	// The lockfile records name -> source + hash.
	entries, err := ReadLock(destDir)
	if err != nil {
		t.Fatalf("ReadLock: %v", err)
	}
	entry, ok := entries["confirmation-policy"]
	if !ok {
		t.Fatalf("lockfile missing entry for confirmation-policy: %#v", entries)
	}
	if entry.Hash != result.Hash {
		t.Fatalf("lockfile hash %q != install hash %q", entry.Hash, result.Hash)
	}
	// The recorded source is the canonical (absolute, symlink-resolved) local path.
	if entry.Source != canonicalSource(source) {
		t.Fatalf("lockfile source = %q, want %q", entry.Source, canonicalSource(source))
	}
}

func TestInstallRejectsInvalidSkill(t *testing.T) {
	destDir := t.TempDir()
	// A source directory with no SKILL.md is not a valid skill.
	src := filepath.Join(t.TempDir(), "empty")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := Install(context.Background(), InstallOptions{Source: src, Dir: destDir})
	if err == nil {
		t.Fatalf("expected an error for a source without SKILL.md")
	}
	// Nothing should have been written into the destination dir.
	if entries, _ := os.ReadDir(destDir); len(entries) != 0 {
		t.Fatalf("invalid source must not write into dest, found: %#v", entries)
	}
}

func TestInstallRejectsSkillWithBlankFrontmatterName(t *testing.T) {
	destDir := t.TempDir()
	// Frontmatter name is blank, so the name falls back to the source dir's base —
	// which is itself not a usable skill name (validSkillName rejects "..") — so
	// there is nothing valid to install under. (A whitespace-only dir name would be
	// the other no-usable-name case, but that is not creatable on Windows.)
	src := writeSourceSkill(t, filepath.Join(t.TempDir(), "no..usable..name"),
		"---\nname:    \n---\nbody\n")

	if _, err := Install(context.Background(), InstallOptions{Source: src, Dir: destDir}); err == nil {
		t.Fatalf("expected rejection of a skill with no usable name")
	}
}

func TestInstallReinstallShowsHashChange(t *testing.T) {
	destDir := t.TempDir()
	srcRoot := t.TempDir()
	src := writeSourceSkill(t, filepath.Join(srcRoot, "src"),
		"---\nname: demo\ndescription: v1\n---\nfirst body\n")

	first, err := Install(context.Background(), InstallOptions{Source: src, Dir: destDir})
	if err != nil {
		t.Fatalf("first Install: %v", err)
	}

	// Change the source content and reinstall.
	writeSourceSkill(t, src, "---\nname: demo\ndescription: v2\n---\nsecond body\n")
	second, err := Install(context.Background(), InstallOptions{Source: src, Dir: destDir})
	if err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if !second.Updated {
		t.Fatalf("reinstall with changed content should be flagged as an update")
	}
	if second.PreviousHash != first.Hash {
		t.Fatalf("PreviousHash = %q, want %q", second.PreviousHash, first.Hash)
	}
	if second.Hash == first.Hash {
		t.Fatalf("hash should change when content changes")
	}

	// The lockfile reflects the new hash.
	entries, err := ReadLock(destDir)
	if err != nil {
		t.Fatalf("ReadLock: %v", err)
	}
	if entries["demo"].Hash != second.Hash {
		t.Fatalf("lockfile not updated: %q != %q", entries["demo"].Hash, second.Hash)
	}

	// The installed content is the new version.
	got, _ := Get(destDir, "demo")
	if !strings.Contains(got.Content, "second body") {
		t.Fatalf("reinstall did not overwrite content: %q", got.Content)
	}
}

func TestConcurrentInstallsPreserveEveryLockEntry(t *testing.T) {
	destDir := t.TempDir()
	const count = 12
	sources := make([]string, count)
	for index := range count {
		content := fmt.Sprintf("---\nname: concurrent-%02d\ndescription: test\n---\nbody\n", index)
		sources[index] = writeSourceSkill(t, filepath.Join(t.TempDir(), "src"), content)
	}

	errs := make(chan error, count)
	for _, source := range sources {
		go func() {
			_, err := Install(context.Background(), InstallOptions{Source: source, Dir: destDir})
			errs <- err
		}()
	}
	for range count {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent Install: %v", err)
		}
	}

	entries, err := ReadLock(destDir)
	if err != nil {
		t.Fatalf("ReadLock: %v", err)
	}
	if len(entries) != count {
		t.Fatalf("lockfile has %d entries, want %d: %#v", len(entries), count, entries)
	}
}

// TestInstallSameLocalSourceDifferentSpellingIsNotAClash verifies that a local
// source installed via one spelling (e.g. a relative path) and re-installed via
// an equivalent spelling (the absolute path) is treated as the same source, not
// a clash, because the recorded source is canonicalized.
func TestInstallSameLocalSourceDifferentSpellingIsNotAClash(t *testing.T) {
	destDir := t.TempDir()
	srcRoot := t.TempDir()
	abs := writeSourceSkill(t, filepath.Join(srcRoot, "src"),
		"---\nname: demo\ndescription: v1\n---\nbody\n")

	if _, err := Install(context.Background(), InstallOptions{Source: abs, Dir: destDir}); err != nil {
		t.Fatalf("first install: %v", err)
	}

	// Reinstall using a different textual spelling of the same directory (a
	// redundant "/./" segment). canonicalSource normalizes both spellings to the
	// same absolute path, so this must not be treated as a clash. (A cwd-relative
	// spelling can't be expressed across drives on Windows, where the temp dir and
	// the repo live on different volumes, so use a same-directory alternate.)
	messy := filepath.Dir(abs) + string(filepath.Separator) + "." + string(filepath.Separator) + filepath.Base(abs)
	if _, err := Install(context.Background(), InstallOptions{Source: messy, Dir: destDir}); err != nil {
		t.Fatalf("reinstall with an equivalent spelling should not clash: %v", err)
	}

	want := canonicalSource(abs)
	entries, err := ReadLock(destDir)
	if err != nil {
		t.Fatalf("ReadLock: %v", err)
	}
	if entries["demo"].Source != want {
		t.Fatalf("lockfile should record the canonical source %q, got %q", want, entries["demo"].Source)
	}
}

func TestInstallNameClashWarnsAndDoesNotOverwriteWithoutForce(t *testing.T) {
	destDir := t.TempDir()
	// Pre-existing skill installed from source A.
	srcA := writeSourceSkill(t, filepath.Join(t.TempDir(), "a"),
		"---\nname: shared\ndescription: original\n---\noriginal body\n")
	if _, err := Install(context.Background(), InstallOptions{Source: srcA, Dir: destDir}); err != nil {
		t.Fatalf("seed install: %v", err)
	}

	// A different source declaring the same name must not silently overwrite.
	srcB := writeSourceSkill(t, filepath.Join(t.TempDir(), "b"),
		"---\nname: shared\ndescription: replacement\n---\nreplacement body\n")
	_, err := Install(context.Background(), InstallOptions{Source: srcB, Dir: destDir})
	if !errors.Is(err, ErrNameClash) {
		t.Fatalf("expected ErrNameClash without Force, got %v", err)
	}

	// The original content survives.
	got, _ := Get(destDir, "shared")
	if !strings.Contains(got.Content, "original body") {
		t.Fatalf("clash should not overwrite: %q", got.Content)
	}

	// With Force the install proceeds and overwrites.
	result, err := Install(context.Background(), InstallOptions{Source: srcB, Dir: destDir, Force: true})
	if err != nil {
		t.Fatalf("forced reinstall: %v", err)
	}
	if !result.Updated {
		t.Fatalf("forced overwrite should be flagged as an update")
	}
	got, _ = Get(destDir, "shared")
	if !strings.Contains(got.Content, "replacement body") {
		t.Fatalf("forced overwrite did not replace content: %q", got.Content)
	}
}

// TestInstallReinstallSameSourceUnchangedIsNotAClash verifies that re-running an
// install from the SAME recorded source is treated as an idempotent update, not a
// name clash (the clash guard only protects against a DIFFERENT source).
func TestInstallReinstallSameSourceUnchangedIsNotAClash(t *testing.T) {
	destDir := t.TempDir()
	src := writeSourceSkill(t, filepath.Join(t.TempDir(), "src"),
		"---\nname: demo\ndescription: v1\n---\nbody\n")
	if _, err := Install(context.Background(), InstallOptions{Source: src, Dir: destDir}); err != nil {
		t.Fatalf("first install: %v", err)
	}
	result, err := Install(context.Background(), InstallOptions{Source: src, Dir: destDir})
	if err != nil {
		t.Fatalf("idempotent reinstall from same source should succeed, got %v", err)
	}
	if result.Updated {
		t.Fatalf("reinstall of identical content from same source is not an update")
	}
}

func TestInstallGitSourceUsesRunnerAndDoesNotExecuteContent(t *testing.T) {
	destDir := t.TempDir()
	cloneRoot := t.TempDir()

	executed := false
	runner := func(ctx context.Context, destination string, source string) error {
		// Emulate a clone by laying down a skill plus a hostile executable that
		// must NEVER be run during install.
		writeSourceSkill(t, destination, "---\nname: remote\ndescription: fetched\n---\nremote body\n")
		script := filepath.Join(destination, "install.sh")
		if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+filepath.Join(cloneRoot, "PWNED")+"\n"), 0o755); err != nil {
			return err
		}
		executed = true
		return nil
	}

	result, err := Install(context.Background(), InstallOptions{
		Source:    "https://example.com/remote-skill.git",
		Dir:       destDir,
		GitRunner: runner,
	})
	if err != nil {
		t.Fatalf("Install via git runner: %v", err)
	}
	if !executed {
		t.Fatalf("git runner was not invoked for a URL source")
	}
	if result.Name != "remote" {
		t.Fatalf("Name = %q, want remote", result.Name)
	}
	// The hostile install script must not have run.
	if _, err := os.Stat(filepath.Join(cloneRoot, "PWNED")); err == nil {
		t.Fatalf("install must never execute fetched content")
	}
	// The fetched executable must not be copied into the skills dir either (skills
	// are markdown — only SKILL.md is installed, not arbitrary fetched files).
	if _, err := os.Stat(filepath.Join(destDir, "remote", "install.sh")); err == nil {
		t.Fatalf("install must not copy fetched executable into the skills dir")
	}
}

func TestRemoveDeletesSkillAndLockEntry(t *testing.T) {
	destDir := t.TempDir()
	src := writeSourceSkill(t, filepath.Join(t.TempDir(), "src"),
		"---\nname: demo\ndescription: d\n---\nbody\n")
	if _, err := Install(context.Background(), InstallOptions{Source: src, Dir: destDir}); err != nil {
		t.Fatalf("install: %v", err)
	}

	if err := Remove(destDir, "demo"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := Get(destDir, "demo"); ok {
		t.Fatalf("skill still present after Remove")
	}
	entries, err := ReadLock(destDir)
	if err != nil {
		t.Fatalf("ReadLock: %v", err)
	}
	if _, ok := entries["demo"]; ok {
		t.Fatalf("lockfile entry survived Remove: %#v", entries)
	}
}

func TestRemoveUnknownSkillErrors(t *testing.T) {
	if err := Remove(t.TempDir(), "nope"); err == nil {
		t.Fatalf("expected an error removing an unknown skill")
	}
}

func TestInfoReturnsFrontmatterSourceAndHash(t *testing.T) {
	destDir := t.TempDir()
	src := writeSourceSkill(t, filepath.Join(t.TempDir(), "src"),
		"---\nname: demo\ndescription: described\n---\nbody text\n")
	installed, err := Install(context.Background(), InstallOptions{Source: src, Dir: destDir})
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	info, ok := Info(destDir, "demo")
	if !ok {
		t.Fatalf("Info(demo) not found")
	}
	if info.Skill.Description != "described" {
		t.Fatalf("Info description = %q", info.Skill.Description)
	}
	if info.Source != installed.Source {
		t.Fatalf("Info source = %q, want %q", info.Source, installed.Source)
	}
	if info.Hash != installed.Hash {
		t.Fatalf("Info hash = %q, want %q", info.Hash, installed.Hash)
	}
	if info.HashDrift {
		t.Fatal("expected no hash drift immediately after install")
	}

	if err := os.WriteFile(info.Skill.Path, []byte("---\nname: demo\ndescription: described\n---\nedited body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, ok = Info(destDir, "demo")
	if !ok {
		t.Fatal("Info(demo) not found after edit")
	}
	if !info.HashDrift {
		t.Fatal("expected hash drift after SKILL.md edit")
	}
}

func TestSkillHashDriftUnreadableLockedPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing", "SKILL.md")
	if !skillHashDrift(Skill{Path: missing}, "sha256:deadbeef") {
		t.Fatal("expected drift when locked SKILL.md cannot be read")
	}
	if skillHashDrift(Skill{Path: missing}, "") {
		t.Fatal("missing lock hash must not count as drift")
	}
}

// skills.Install carries the same recovery call as plugins.Install, so it needs
// the same proof. An install killed mid-commit leaves the skill's only copy in
// a workspace backup; the next install over the same directory has to put it
// back rather than leave it stranded where nothing reads it.
func TestInstallRecoversASkillLeftByAnInterruptedCommit(t *testing.T) {
	dir := t.TempDir()
	src := writeSourceSkill(t, filepath.Join(t.TempDir(), "src"),
		"---\nname: alpha\ndescription: first.\n---\nalpha body\n")
	if _, err := Install(context.Background(), InstallOptions{Source: src, Dir: dir}); err != nil {
		t.Fatalf("seeding the install: %v", err)
	}

	// The state a kill between CommitDir's two renames leaves behind.
	staged, _, err := installtxn.StageDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Dir(staged)
	writeWorkspaceMarker(t, workspace, "alpha")
	if err := os.Rename(filepath.Join(dir, "alpha"), filepath.Join(workspace, "previous")); err != nil {
		t.Fatal(err)
	}

	src2 := writeSourceSkill(t, filepath.Join(t.TempDir(), "src2"),
		"---\nname: beta\ndescription: second.\n---\nbeta body\n")
	if _, err := Install(context.Background(), InstallOptions{Source: src2, Dir: dir}); err != nil {
		t.Fatalf("later install: %v", err)
	}

	got, ok := Get(dir, "alpha")
	if !ok || !strings.Contains(got.Content, "alpha body") {
		t.Fatalf("the interrupted skill install was not put back: ok=%v skill=%+v", ok, got)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Errorf("the recovered workspace should be cleared, got %v", err)
	}
}

// skills.Remove carries the same recovery call as plugins.Remove, so it needs
// the same proof. A commit killed after its publish rename leaves the tree the
// install replaced in a backup beside the live skill; removing the skill must
// not leave that backup for the next install's recovery to publish, since Get
// reads the directory rather than the lockfile and would find the removed skill
// loadable again.
func TestRemoveLeavesNoSupersededBackupARecoveryCanResurrect(t *testing.T) {
	dir := t.TempDir()
	src := writeSourceSkill(t, filepath.Join(t.TempDir(), "src"),
		"---\nname: alpha\ndescription: first.\n---\nalpha body\n")
	if _, err := Install(context.Background(), InstallOptions{Source: src, Dir: dir}); err != nil {
		t.Fatalf("seeding the install: %v", err)
	}

	// The state a kill after CommitDir's second rename leaves behind.
	staged, _, err := installtxn.StageDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Dir(staged)
	writeWorkspaceMarker(t, workspace, "alpha")
	previous := filepath.Join(workspace, "previous")
	if err := os.MkdirAll(previous, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(previous, skillFileName),
		[]byte("---\nname: alpha\ndescription: superseded.\n---\nold alpha body\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Remove(dir, "alpha"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	src2 := writeSourceSkill(t, filepath.Join(t.TempDir(), "src2"),
		"---\nname: beta\ndescription: second.\n---\nbeta body\n")
	if _, err := Install(context.Background(), InstallOptions{Source: src2, Dir: dir}); err != nil {
		t.Fatalf("later install: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "alpha")); !os.IsNotExist(err) {
		t.Errorf("a removed skill was put back on disk: %v", err)
	}
	if _, ok := Get(dir, "alpha"); ok {
		t.Errorf("a removed skill is loadable again")
	}
	lock, err := ReadLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := lock["alpha"]; ok {
		t.Errorf("the lockfile still names a removed skill")
	}
}

// The recovery states an interrupted update of an installed skill can be killed
// in. Each is planted by hand rather than by killing a real commit, because a
// real kill cannot be aimed at a single step between two renames.
const (
	oldAlphaSkill = "---\nname: alpha\ndescription: old.\n---\nold alpha body\n"
	newAlphaSkill = "---\nname: alpha\ndescription: new.\n---\nnew alpha body\n"
	gammaSkill    = "---\nname: gamma\ndescription: unrelated.\n---\ngamma body\n"
	betaSkill     = "---\nname: beta\ndescription: later.\n---\nbeta body\n"
	deltaSkill    = "---\nname: delta\ndescription: later still.\n---\ndelta body\n"
)

// interruptedUpdate is a skills dir holding an installed alpha and an unrelated
// gamma, plus the workspace an update of alpha to newAlphaSkill left behind when
// it was killed partway through its commit.
type interruptedUpdate struct {
	dir       string
	workspace string
	oldSource string
	newSource string
}

// plantInterruptedAlphaUpdate builds the on-disk state a kill at the named step
// of CommitDir leaves. The steps are the commit's own order: write the marker,
// move the target aside, move the staged tree in, publish the lockfile, clean up.
func plantInterruptedAlphaUpdate(t *testing.T, step string) interruptedUpdate {
	t.Helper()
	dir := t.TempDir()
	oldSource := writeSourceSkill(t, filepath.Join(t.TempDir(), "old-alpha"), oldAlphaSkill)
	if _, err := Install(context.Background(), InstallOptions{Source: oldSource, Dir: dir}); err != nil {
		t.Fatalf("seed alpha: %v", err)
	}
	gammaSource := writeSourceSkill(t, filepath.Join(t.TempDir(), "gamma"), gammaSkill)
	if _, err := Install(context.Background(), InstallOptions{Source: gammaSource, Dir: dir}); err != nil {
		t.Fatalf("seed gamma: %v", err)
	}
	newSource := writeSourceSkill(t, filepath.Join(t.TempDir(), "new-alpha"), newAlphaSkill)

	staged, _, err := installtxn.StageDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Dir(staged)
	writeWorkspaceMarker(t, workspace, "alpha")
	writeSourceSkill(t, staged, newAlphaSkill)

	target := filepath.Join(dir, "alpha")
	previous := filepath.Join(workspace, "previous")
	rename := func(from string, to string) {
		if err := os.Rename(from, to); err != nil {
			t.Fatalf("plant %s: %v", step, err)
		}
	}
	switch step {
	case "S1":
		// The marker is down and no tree has moved yet.
	case "S2":
		rename(target, previous)
	case "S3":
		rename(target, previous)
		rename(staged, target)
	case "S4":
		rename(target, previous)
		rename(staged, target)
		publishLockEntry(t, dir, "alpha", LockEntry{Source: canonicalSource(newSource), Hash: hashContent([]byte(newAlphaSkill))})
	case "S5":
		// An interrupted rollback: the failed tree was set aside and the backup is
		// still the only complete copy.
		rename(target, previous)
		writeSourceSkill(t, filepath.Join(workspace, "failed"), newAlphaSkill)
		if err := os.RemoveAll(staged); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown step %q", step)
	}
	// Compare against the source the installer records, not the path the test
	// handed it. Install stores canonicalSource(source), so on macOS the recorded
	// value is /private/var where t.TempDir returns /var, and on Windows it is the
	// long user name where t.TempDir returns the 8.3 short one.
	return interruptedUpdate{dir: dir, workspace: workspace, oldSource: canonicalSource(oldSource), newSource: canonicalSource(newSource)}
}

func publishLockEntry(t *testing.T, dir string, name string, entry LockEntry) {
	t.Helper()
	lock, err := ReadLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	lock[name] = entry
	if err := writeLock(dir, lock); err != nil {
		t.Fatal(err)
	}
}

// recoveredAlpha is what the skills dir must hold once recovery has resolved a
// planted state: the SKILL.md at the target, the lock entry beside it, and
// whether the workspace was resolved away or left for somebody else.
type recoveredAlpha struct {
	content           string
	description       string
	fromNewSource     bool
	workspaceRetained bool
}

func assertRecoveredAlpha(t *testing.T, update interruptedUpdate, want recoveredAlpha) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(update.dir, "alpha", skillFileName))
	if err != nil {
		t.Fatalf("read recovered alpha: %v", err)
	}
	if string(data) != want.content {
		t.Errorf("recovered tree is the wrong one: got %q want %q", data, want.content)
	}
	lock, err := ReadLock(update.dir)
	if err != nil {
		t.Fatal(err)
	}
	entry, locked := lock["alpha"]
	if !locked {
		t.Fatal("the lockfile no longer records the recovered skill")
	}
	wantSource := update.oldSource
	if want.fromNewSource {
		wantSource = update.newSource
	}
	if entry.Source != wantSource {
		t.Errorf("lock source: got %q want %q", entry.Source, wantSource)
	}
	if wantHash := hashContent([]byte(want.content)); entry.Hash != wantHash {
		t.Errorf("lock hash: got %q want %q (the recorded hash must describe the tree on disk)", entry.Hash, wantHash)
	}
	// The tree has to come back through the loader every other caller reads, not
	// merely be present as bytes on disk.
	skill, ok := Get(update.dir, "alpha")
	if !ok {
		t.Fatal("the recovered skill does not load")
	}
	if skill.Description != want.description {
		t.Errorf("loaded skill is the wrong tree: got description %q want %q", skill.Description, want.description)
	}
	_, statErr := os.Stat(update.workspace)
	if want.workspaceRetained && statErr != nil {
		t.Errorf("a workspace recovery cannot attribute must be left alone: %v", statErr)
	}
	if !want.workspaceRetained && !os.IsNotExist(statErr) {
		t.Errorf("a resolved workspace must be cleared, got %v", statErr)
	}
}

// installDriver and removeDriver are the two entry points that take the install
// lock, so every recovery state has to come out the same through both.
func installDriver(t *testing.T, dir string) error {
	return installSkill(t, dir, betaSkill)
}

// installSkill installs a skill unrelated to the planted state, so the drive is
// an ordinary install that happens to run recovery first.
func installSkill(t *testing.T, dir string, content string) error {
	t.Helper()
	source := writeSourceSkill(t, filepath.Join(t.TempDir(), "source"), content)
	_, err := Install(context.Background(), InstallOptions{Source: source, Dir: dir})
	return err
}

func removeDriver(t *testing.T, dir string) error {
	t.Helper()
	return Remove(dir, "gamma")
}

var recoveryDrivers = []struct {
	name string
	run  func(t *testing.T, dir string) error
}{
	{"install", installDriver},
	{"remove", removeDriver},
}

// treeSnapshot records every path under dir and the content of every regular
// file, so a caller that must not have touched anything can be held to it.
func treeSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	if _, err := os.Lstat(dir); os.IsNotExist(err) {
		// An absent tree is a state like any other, and a caller that must not have
		// touched anything must not have created it either.
		return snapshot
	}
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		if entry.IsDir() {
			snapshot[rel] = "<dir>"
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		snapshot[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", dir, err)
	}
	return snapshot
}

func assertSnapshotUnchanged(t *testing.T, before map[string]string, after map[string]string) {
	t.Helper()
	for path, content := range before {
		got, present := after[path]
		if !present {
			t.Errorf("%s was removed by a caller that must have aborted", path)
			continue
		}
		if got != content {
			t.Errorf("%s was rewritten by a caller that must have aborted: got %q want %q", path, got, content)
		}
	}
	for path := range after {
		if _, present := before[path]; !present {
			t.Errorf("%s was created by a caller that must have aborted", path)
		}
	}
}

// readLockBytes returns the lockfile exactly as it sits on disk, so a caller
// that must not have republished it can be held to the bytes.
func readLockBytes(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, LockFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	return string(data)
}

// Every step an interrupted update can be killed at, resolved through both entry
// points that take the install lock. The recorded hash is what decides: the tree
// the lockfile describes is the truthful one, and the tree it does not describe
// is the one recovery undoes.
func TestRecoveryResolvesEveryInterruptedUpdateStep(t *testing.T) {
	steps := []struct {
		step string
		want recoveredAlpha
	}{
		// The marker is down but no tree moved, so there is nothing to put back and
		// the workspace belongs to whoever made it.
		{"S1", recoveredAlpha{content: oldAlphaSkill, description: "old.", workspaceRetained: true}},
		// The target is gone and the backup is the only copy there is.
		{"S2", recoveredAlpha{content: oldAlphaSkill, description: "old."}},
		// Both trees are on disk and only the lockfile can say which one it records:
		// it still names the old source and hash, so the swap landed and the publish
		// never did.
		{"S3", recoveredAlpha{content: oldAlphaSkill, description: "old."}},
		// The lockfile records the new tree, so the publish committed and the backup
		// beside it is superseded.
		{"S4", recoveredAlpha{content: newAlphaSkill, description: "new.", fromNewSource: true}},
		// An interrupted rollback looks like an interrupted swap and is repaired the
		// same way.
		{"S5", recoveredAlpha{content: oldAlphaSkill, description: "old."}},
	}
	for _, step := range steps {
		for _, driver := range recoveryDrivers {
			t.Run(step.step+"/"+driver.name, func(t *testing.T) {
				update := plantInterruptedAlphaUpdate(t, step.step)

				if err := driver.run(t, update.dir); err != nil {
					t.Fatalf("%s over a recoverable state: %v", driver.name, err)
				}

				assertRecoveredAlpha(t, update, step.want)
				// A second pass over an already resolved state must change nothing.
				if err := installSkill(t, update.dir, deltaSkill); err != nil {
					t.Fatalf("second recovery pass: %v", err)
				}
				assertRecoveredAlpha(t, update, step.want)
			})
			t.Run(step.step+"/"+driver.name+"/removal-is-final", func(t *testing.T) {
				update := plantInterruptedAlphaUpdate(t, step.step)
				if err := driver.run(t, update.dir); err != nil {
					t.Fatalf("%s over a recoverable state: %v", driver.name, err)
				}

				if err := Remove(update.dir, "alpha"); err != nil {
					t.Fatalf("remove alpha: %v", err)
				}
				// A later recovery reading a leftover backup would publish the removed
				// skill again, and Get reads the directory rather than the lockfile, so
				// the user would find it back.
				if err := installSkill(t, update.dir, deltaSkill); err != nil {
					t.Fatalf("install after removal: %v", err)
				}

				if _, err := os.Stat(filepath.Join(update.dir, "alpha")); !os.IsNotExist(err) {
					t.Errorf("a removed skill came back on disk: %v", err)
				}
				if _, ok := Get(update.dir, "alpha"); ok {
					t.Error("a removed skill is loadable again")
				}
				lock, err := ReadLock(update.dir)
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := lock["alpha"]; ok {
					t.Error("the lockfile still names a removed skill")
				}
			})
		}
	}
}

// A recovery that could not be made is not the same as nothing to recover.
// Installing over or reporting the removal of a tree that is still owed a
// restore destroys the only copy of it, so the caller has to stop before it
// reads the lockfile or touches the target.
func TestCallersAbortWhenRecoveryCannotResolveAWorkspace(t *testing.T) {
	injections := []struct {
		name string
		step string
		// break renders the planted state unresolvable and returns a repair the test
		// cleanup needs, so t.TempDir can still remove the tree.
		breakState func(t *testing.T, update interruptedUpdate)
		skipOnRoot bool
		// wholeDirUntouched holds the injection to leaving every path in the skills
		// dir exactly as it found it. A retirement that fails partway is the one
		// exception: os.RemoveAll deletes what it can before the denied unlink, so
		// the superseded backup it was clearing can be partly gone. That tree is
		// already superseded by the committed target, which is why the failure is
		// reported rather than acted on further.
		wholeDirUntouched bool
	}{
		{
			// Neither tree is the one the lockfile records, so recovery cannot tell
			// which one the user is owed and either guess destroys the other.
			name:              "neither tree matches the recorded hash",
			step:              "S3",
			wholeDirUntouched: true,
			breakState: func(t *testing.T, update interruptedUpdate) {
				t.Helper()
				writeSourceSkill(t, filepath.Join(update.workspace, "previous"),
					"---\nname: alpha\ndescription: drifted.\n---\ndrifted alpha body\n")
			},
		},
		{
			// The restore is the whole point of the transaction, so a rename it
			// cannot complete is the loudest failure recovery has.
			name:              "the backup cannot be restored",
			step:              "S2",
			wholeDirUntouched: true,
			breakState: func(t *testing.T, update interruptedUpdate) {
				t.Helper()
				t.Cleanup(func() { _ = os.Chmod(update.workspace, 0o755) })
				if err := os.Chmod(update.workspace, 0o555); err != nil {
					t.Fatal(err)
				}
			},
			skipOnRoot: true,
		},
		{
			// A retirement that failed leaves a backup the next pass reads again, so
			// it is reported rather than swallowed.
			name: "the superseded workspace cannot be retired",
			step: "S4",
			breakState: func(t *testing.T, update interruptedUpdate) {
				t.Helper()
				t.Cleanup(func() { _ = os.Chmod(update.workspace, 0o755) })
				if err := os.Chmod(update.workspace, 0o555); err != nil {
					t.Fatal(err)
				}
			},
			skipOnRoot: true,
		},
	}
	for _, injection := range injections {
		for _, driver := range recoveryDrivers {
			t.Run(injection.name+"/"+driver.name, func(t *testing.T) {
				if injection.skipOnRoot {
					if runtime.GOOS == "windows" {
						t.Skip("directory permissions do not block renames on windows")
					}
					if os.Geteuid() == 0 {
						t.Skip("root ignores the directory permissions this test relies on")
					}
				}
				update := plantInterruptedAlphaUpdate(t, injection.step)
				injection.breakState(t, update)
				target := filepath.Join(update.dir, "alpha")
				lockBefore := readLockBytes(t, update.dir)
				targetBefore := treeSnapshot(t, target)
				dirBefore := treeSnapshot(t, update.dir)

				err := driver.run(t, update.dir)

				if err == nil {
					t.Fatalf("%s continued over a transaction recovery could not resolve", driver.name)
				}
				assertSnapshotUnchanged(t, targetBefore, treeSnapshot(t, target))
				if got := readLockBytes(t, update.dir); got != lockBefore {
					t.Errorf("the lockfile was republished by a caller that must have aborted: got %q want %q", got, lockBefore)
				}
				if injection.wholeDirUntouched {
					assertSnapshotUnchanged(t, dirBefore, treeSnapshot(t, update.dir))
				}
			})
		}
	}
}

// The workspace prefix is a public dot prefixed name and the loader enumerates
// dot prefixed directories, so a user authored skill can carry that name and
// hold the same entries a workspace does. Recovery acting on one would destroy
// installed content, so the marker is the only evidence that counts.
func TestRecoveryLeavesAUserSkillNamedLikeAWorkspaceAlone(t *testing.T) {
	for _, driver := range recoveryDrivers {
		t.Run(driver.name, func(t *testing.T) {
			dir := t.TempDir()
			gammaSource := writeSourceSkill(t, filepath.Join(t.TempDir(), "gamma"), gammaSkill)
			if _, err := Install(context.Background(), InstallOptions{Source: gammaSource, Dir: dir}); err != nil {
				t.Fatalf("seed gamma: %v", err)
			}
			notes := writeSourceSkill(t, filepath.Join(dir, ".zero-install-txn-notes"),
				"---\nname: notes\ndescription: user authored.\n---\nnotes body\n")
			if err := os.MkdirAll(filepath.Join(notes, "previous"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(notes, "target"), []byte("gamma"), 0o600); err != nil {
				t.Fatal(err)
			}
			before := treeSnapshot(t, notes)

			if err := driver.run(t, dir); err != nil {
				t.Fatalf("%s over a prefix colliding user skill: %v", driver.name, err)
			}

			assertSnapshotUnchanged(t, before, treeSnapshot(t, notes))
			skill, ok := Get(dir, "notes")
			if !ok {
				t.Fatal("a user authored skill was made unloadable by recovery")
			}
			if skill.Description != "user authored." {
				t.Errorf("the wrong tree loads as notes: %q", skill.Description)
			}
		})
	}
}

// A directory at the target that the lockfile does not name is not proof that a
// publish was interrupted. Anything can have created it, and a hand-written
// skill is an ordinary thing to find in the skills directory. Recovery used to
// read a missing entry as proof the publish never ran and replace that tree with
// the retained backup, which deleted the user's own work.
func TestAnUnrelatedSkillAtTheTargetIsNeverReplaced(t *testing.T) {
	dir := t.TempDir()
	workspace, err := os.MkdirTemp(dir, ".zero-install-txn-")
	if err != nil {
		t.Fatal(err)
	}
	marker := []byte("zero-install-txn v1\ntarget notes\n")
	if err := os.WriteFile(filepath.Join(workspace, ".zero-install-txn"), marker, 0o600); err != nil {
		t.Fatal(err)
	}
	writeSourceSkill(t, filepath.Join(workspace, "previous"),
		"---\nname: notes\ndescription: stale backup.\n---\nstale\n")
	// The user's own skill, which no lockfile entry names.
	mine := filepath.Join(dir, "notes")
	writeSourceSkill(t, mine, "---\nname: notes\ndescription: my own.\n---\nmy careful notes\n")
	if err := os.WriteFile(filepath.Join(mine, "research.md"), []byte("my research"), 0o644); err != nil {
		t.Fatal(err)
	}

	err = installSkill(t, dir, deltaSkill)

	if err == nil {
		t.Fatal("recovery must not act on a tree the lockfile does not describe")
	}
	if _, statErr := os.Stat(filepath.Join(mine, "research.md")); statErr != nil {
		t.Fatalf("the user's own skill was destroyed: %v", statErr)
	}
	data, readErr := os.ReadFile(filepath.Join(mine, skillFileName))
	if readErr != nil || !strings.Contains(string(data), "my own") {
		t.Fatalf("the user's own SKILL.md was replaced: %q %v", data, readErr)
	}
}
