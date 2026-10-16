package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Gitlawb/zero/internal/installtxn"
)

// initGitPluginRepo creates a real local git repo holding a plugin and returns a
// file:// URL, exercising the DEFAULT git runner end to end. Skipped when git is
// unavailable.
func initGitPluginRepo(t *testing.T, manifest map[string]any) string {
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
	writeSourcePlugin(t, repo, manifest)
	run("add", "-A")
	run("commit", "-qm", "init")
	return "file://" + repo
}

func TestInstallFromRealLocalGitRepo(t *testing.T) {
	destDir := t.TempDir()
	url := initGitPluginRepo(t, validManifest())

	result, err := Install(context.Background(), InstallOptions{Source: url, Dir: destDir})
	if err != nil {
		t.Fatalf("Install from git: %v", err)
	}
	if result.ID != "zero.demo" {
		t.Fatalf("ID = %q, want zero.demo", result.ID)
	}
	loaded, err := Load(LoadOptions{Roots: []Root{{Source: SourceUser, Path: destDir}}})
	if err != nil || len(loaded.Plugins) != 1 {
		t.Fatalf("installed git plugin not discoverable: err=%v plugins=%#v", err, loaded.Plugins)
	}
	// copyTree must skip .git so clone metadata never lands in the plugins dir.
	if _, err := os.Stat(filepath.Join(destDir, "zero.demo", ".git")); err == nil {
		t.Fatalf(".git metadata must not be copied into the plugins dir")
	}
}

func writeSourcePlugin(t *testing.T, dir string, manifest map[string]any) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, manifestFileName), data, 0o644); err != nil {
		t.Fatalf("write plugin.json: %v", err)
	}
	return dir
}

func validManifest() map[string]any {
	return map[string]any{
		"schemaVersion": float64(1),
		"id":            "zero.demo",
		"name":          "Zero Demo",
		"version":       "0.1.0",
		"description":   "Demo plugin",
	}
}

func TestInstallCopiesLocalPluginAndRecordsHash(t *testing.T) {
	destDir := t.TempDir()
	src := writeSourcePlugin(t, filepath.Join(t.TempDir(), "src"), validManifest())

	result, err := Install(context.Background(), InstallOptions{Source: src, Dir: destDir})
	if err != nil {
		t.Fatalf("Install returned error: %v", err)
	}
	if result.ID != "zero.demo" {
		t.Fatalf("ID = %q, want zero.demo", result.ID)
	}
	if result.Hash == "" {
		t.Fatalf("expected a recorded content hash")
	}

	// The installed manifest is discoverable through the loader.
	loaded, err := Load(LoadOptions{Roots: []Root{{Source: SourceUser, Path: destDir}}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Plugins) != 1 || loaded.Plugins[0].ID != "zero.demo" {
		t.Fatalf("installed plugin not discoverable: %#v", loaded.Plugins)
	}

	// Lockfile records id -> source + hash.
	entries, err := ReadLock(destDir)
	if err != nil {
		t.Fatalf("ReadLock: %v", err)
	}
	if entries["zero.demo"].Hash != result.Hash || entries["zero.demo"].Source != canonicalSource(src) {
		t.Fatalf("lockfile entry unexpected: %#v", entries["zero.demo"])
	}
}

func TestInstallRejectsInvalidManifest(t *testing.T) {
	destDir := t.TempDir()
	// Missing required fields (id/name/version) -> ParseManifest rejects it.
	src := writeSourcePlugin(t, filepath.Join(t.TempDir(), "bad"), map[string]any{
		"schemaVersion": float64(1),
	})

	_, err := Install(context.Background(), InstallOptions{Source: src, Dir: destDir})
	if err == nil {
		t.Fatalf("expected an error for an invalid manifest")
	}
	if entries, _ := os.ReadDir(destDir); len(entries) != 0 {
		t.Fatalf("invalid manifest must not write into dest, found: %#v", entries)
	}
}

func TestInstallRejectsMissingManifest(t *testing.T) {
	destDir := t.TempDir()
	src := filepath.Join(t.TempDir(), "empty")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), InstallOptions{Source: src, Dir: destDir}); err == nil {
		t.Fatalf("expected an error for a source without plugin.json")
	}
}

func TestInstallNeverExecutesInstallScript(t *testing.T) {
	destDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "PWNED")
	src := writeSourcePlugin(t, filepath.Join(t.TempDir(), "src"), validManifest())
	// Drop a hostile install script alongside the manifest. Install must copy the
	// plugin tree verbatim but NEVER run anything.
	script := filepath.Join(src, "install.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := Install(context.Background(), InstallOptions{Source: src, Dir: destDir}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("install must never execute an install script")
	}
}

func TestInstallReinstallShowsHashChange(t *testing.T) {
	destDir := t.TempDir()
	src := filepath.Join(t.TempDir(), "src")
	writeSourcePlugin(t, src, validManifest())

	first, err := Install(context.Background(), InstallOptions{Source: src, Dir: destDir})
	if err != nil {
		t.Fatalf("first install: %v", err)
	}

	bumped := validManifest()
	bumped["version"] = "0.2.0"
	writeSourcePlugin(t, src, bumped)
	second, err := Install(context.Background(), InstallOptions{Source: src, Dir: destDir})
	if err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if !second.Updated {
		t.Fatalf("reinstall with changed content should be flagged as an update")
	}
	if second.PreviousHash != first.Hash || second.Hash == first.Hash {
		t.Fatalf("expected a hash change: prev=%q first=%q new=%q", second.PreviousHash, first.Hash, second.Hash)
	}
}

func TestConcurrentInstallsPreserveEveryLockEntry(t *testing.T) {
	destDir := t.TempDir()
	const count = 12
	sources := make([]string, count)
	for index := range count {
		manifest := validManifest()
		manifest["id"] = fmt.Sprintf("zero.concurrent.%02d", index)
		sources[index] = writeSourcePlugin(t, filepath.Join(t.TempDir(), "src"), manifest)
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

// TestInstallReinstallDetectsNestedFileChange guards that the recorded hash
// covers the whole installed tree, not just plugin.json. A change to a tool
// script (with the manifest unchanged) must still be reported as an update so
// checksum pinning catches modified executable content.
func TestInstallReinstallDetectsNestedFileChange(t *testing.T) {
	destDir := t.TempDir()
	src := filepath.Join(t.TempDir(), "src")
	writeSourcePlugin(t, src, map[string]any{
		"schemaVersion": float64(1),
		"id":            "zero.tool",
		"name":          "Tool",
		"version":       "0.1.0",
		"tools": []any{map[string]any{
			"name":    "lookup",
			"command": "node",
			"args":    []any{"tools/lookup.mjs"},
		}},
	})
	entryDir := filepath.Join(src, "tools")
	if err := os.MkdirAll(entryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(entryDir, "lookup.mjs")
	if err := os.WriteFile(entry, []byte("console.log('v1')\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	first, err := Install(context.Background(), InstallOptions{Source: src, Dir: destDir})
	if err != nil {
		t.Fatalf("first install: %v", err)
	}

	// Change ONLY the nested tool script; the manifest is untouched.
	if err := os.WriteFile(entry, []byte("console.log('v2')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := Install(context.Background(), InstallOptions{Source: src, Dir: destDir})
	if err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if !second.Updated {
		t.Fatalf("changing a nested file should be flagged as an update")
	}
	if second.PreviousHash != first.Hash || second.Hash == first.Hash {
		t.Fatalf("expected the hash to change: prev=%q first=%q new=%q", second.PreviousHash, first.Hash, second.Hash)
	}
}

func TestInstallNameClashWarnsAndDoesNotOverwriteWithoutForce(t *testing.T) {
	destDir := t.TempDir()
	srcA := writeSourcePlugin(t, filepath.Join(t.TempDir(), "a"), validManifest())
	if _, err := Install(context.Background(), InstallOptions{Source: srcA, Dir: destDir}); err != nil {
		t.Fatalf("seed install: %v", err)
	}

	srcB := writeSourcePlugin(t, filepath.Join(t.TempDir(), "b"), validManifest())
	_, err := Install(context.Background(), InstallOptions{Source: srcB, Dir: destDir})
	if !errors.Is(err, ErrNameClash) {
		t.Fatalf("expected ErrNameClash from a different source, got %v", err)
	}

	if _, err := Install(context.Background(), InstallOptions{Source: srcB, Dir: destDir, Force: true}); err != nil {
		t.Fatalf("forced reinstall: %v", err)
	}
	entries, _ := ReadLock(destDir)
	if entries["zero.demo"].Source != canonicalSource(srcB) {
		t.Fatalf("forced overwrite did not update source: %#v", entries["zero.demo"])
	}
}

// TestInstallSameLocalSourceDifferentSpellingIsNotAClash verifies that a local
// source installed via an absolute path and re-installed via an equivalent
// relative spelling is treated as the same source, not a clash, because the
// recorded source is canonicalized.
func TestInstallSameLocalSourceDifferentSpellingIsNotAClash(t *testing.T) {
	destDir := t.TempDir()
	abs := writeSourcePlugin(t, filepath.Join(t.TempDir(), "src"), validManifest())

	if _, err := Install(context.Background(), InstallOptions{Source: abs, Dir: destDir}); err != nil {
		t.Fatalf("first install: %v", err)
	}

	// A different textual spelling of the same directory (a redundant "/./"
	// segment) canonicalizes to the same absolute path, so it must not clash. (A
	// cwd-relative spelling can't be expressed across drives on Windows, where the
	// temp dir and the repo are on different volumes, so use a same-dir alternate.)
	messy := filepath.Dir(abs) + string(filepath.Separator) + "." + string(filepath.Separator) + filepath.Base(abs)
	if _, err := Install(context.Background(), InstallOptions{Source: messy, Dir: destDir}); err != nil {
		t.Fatalf("reinstall with an equivalent spelling should not clash: %v", err)
	}

	entries, err := ReadLock(destDir)
	if err != nil {
		t.Fatalf("ReadLock: %v", err)
	}
	if entries["zero.demo"].Source != canonicalSource(abs) {
		t.Fatalf("lockfile should record the canonical source %q, got %q", canonicalSource(abs), entries["zero.demo"].Source)
	}
}

func TestInstallGitSourceUsesRunner(t *testing.T) {
	destDir := t.TempDir()
	used := false
	runner := func(ctx context.Context, destination string, source string) error {
		used = true
		writeSourcePlugin(t, destination, validManifest())
		return nil
	}
	result, err := Install(context.Background(), InstallOptions{
		Source:    "https://example.com/plugin.git",
		Dir:       destDir,
		GitRunner: runner,
	})
	if err != nil {
		t.Fatalf("install via runner: %v", err)
	}
	if !used {
		t.Fatalf("git runner not invoked for URL source")
	}
	if result.ID != "zero.demo" {
		t.Fatalf("ID = %q", result.ID)
	}
}

func TestRemoveDeletesPluginAndLockEntry(t *testing.T) {
	destDir := t.TempDir()
	src := writeSourcePlugin(t, filepath.Join(t.TempDir(), "src"), validManifest())
	if _, err := Install(context.Background(), InstallOptions{Source: src, Dir: destDir}); err != nil {
		t.Fatalf("install: %v", err)
	}

	if err := Remove(destDir, "zero.demo"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	loaded, _ := Load(LoadOptions{Roots: []Root{{Source: SourceUser, Path: destDir}}})
	if len(loaded.Plugins) != 0 {
		t.Fatalf("plugin still present after Remove: %#v", loaded.Plugins)
	}
	entries, _ := ReadLock(destDir)
	if _, ok := entries["zero.demo"]; ok {
		t.Fatalf("lockfile entry survived Remove")
	}
}

func TestRemoveUnknownPluginErrors(t *testing.T) {
	if err := Remove(t.TempDir(), "missing.plugin"); err == nil {
		t.Fatalf("expected an error removing an unknown plugin")
	}
}

// TestInstallCopiesEntireTree confirms install copies the whole plugin directory
// (entry scripts, prompt/skill files) so an installed plugin is actually runnable
// through Stage 09 activation — it just never executes any of it during install.
func TestInstallCopiesEntireTree(t *testing.T) {
	destDir := t.TempDir()
	src := writeSourcePlugin(t, filepath.Join(t.TempDir(), "src"), map[string]any{
		"schemaVersion": float64(1),
		"id":            "zero.tool",
		"name":          "Tool",
		"version":       "0.1.0",
		"tools": []any{map[string]any{
			"name":    "lookup",
			"command": "node",
			"args":    []any{"tools/lookup.mjs"},
		}},
	})
	entryDir := filepath.Join(src, "tools")
	if err := os.MkdirAll(entryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entryDir, "lookup.mjs"), []byte("console.log('hi')\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := Install(context.Background(), InstallOptions{Source: src, Dir: destDir})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	copied := filepath.Join(filepath.Dir(result.ManifestPath), "tools", "lookup.mjs")
	if _, err := os.Stat(copied); err != nil {
		t.Fatalf("entry script not copied into install dir: %v", err)
	}
}

// A process killed between installtxn.CommitDir's two renames leaves the
// plugin's only copy in a workspace backup with nothing at the target: the
// plugin disappears and no later run looks in the workspace. The next install
// over the same directory already takes the same cross-process lock, so it is
// where the interrupted one gets put back.
func TestInstallRecoversAPluginLeftByAnInterruptedCommit(t *testing.T) {
	dir := t.TempDir()
	src := writeSourcePlugin(t, filepath.Join(t.TempDir(), "src"), validManifest())
	if _, err := Install(context.Background(), InstallOptions{Source: src, Dir: dir}); err != nil {
		t.Fatalf("seeding the install: %v", err)
	}

	// The on-disk state a kill in that window leaves, built the way CommitDir
	// builds it: a staged workspace naming its target, with the live tree moved
	// into the backup beside it and the target gone.
	staged, _, err := installtxn.StageDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Dir(staged)
	writeWorkspaceMarker(t, workspace, "zero.demo")
	if err := os.Rename(filepath.Join(dir, "zero.demo"), filepath.Join(workspace, "previous")); err != nil {
		t.Fatal(err)
	}

	other := validManifest()
	other["id"] = "zero.other"
	src2 := writeSourcePlugin(t, filepath.Join(t.TempDir(), "src2"), other)
	if _, err := Install(context.Background(), InstallOptions{Source: src2, Dir: dir}); err != nil {
		t.Fatalf("second install: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "zero.demo", manifestFileName)); err != nil {
		t.Fatalf("the interrupted install was not put back: %v", err)
	}
	loaded, err := Load(LoadOptions{Roots: []Root{{Source: SourceUser, Path: dir}}})
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, p := range loaded.Plugins {
		ids = append(ids, p.ID)
	}
	if len(ids) != 2 {
		t.Errorf("Load sees %v, want both plugins", ids)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Errorf("the recovered workspace should be cleared, got %v", err)
	}
}

// A removal has to stick. An install killed mid-commit leaves the tree in a
// workspace backup with nothing at the target, and Remove then takes the
// not-present branch: it drops the lockfile entry, reports success, and leaves
// the backup behind for the next install's recovery to publish again. Since
// Load enumerates directories rather than the lockfile, that republished tree
// is a live plugin the user already deleted.
func TestRemoveLeavesNothingARecoveryCanResurrect(t *testing.T) {
	dir := t.TempDir()
	src := writeSourcePlugin(t, filepath.Join(t.TempDir(), "src"), validManifest())
	if _, err := Install(context.Background(), InstallOptions{Source: src, Dir: dir}); err != nil {
		t.Fatalf("seeding the install: %v", err)
	}
	staged, _, err := installtxn.StageDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Dir(staged)
	writeWorkspaceMarker(t, workspace, "zero.demo")
	if err := os.Rename(filepath.Join(dir, "zero.demo"), filepath.Join(workspace, "previous")); err != nil {
		t.Fatal(err)
	}

	if err := Remove(dir, "zero.demo"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	other := validManifest()
	other["id"] = "zero.other"
	src2 := writeSourcePlugin(t, filepath.Join(t.TempDir(), "src2"), other)
	if _, err := Install(context.Background(), InstallOptions{Source: src2, Dir: dir}); err != nil {
		t.Fatalf("later install: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "zero.demo")); !os.IsNotExist(err) {
		t.Errorf("a removed plugin was put back on disk: %v", err)
	}
	loaded, err := Load(LoadOptions{Roots: []Root{{Source: SourceUser, Path: dir}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range loaded.Plugins {
		if p.ID == "zero.demo" {
			t.Errorf("a removed plugin is loadable again")
		}
	}
}

// A commit killed after its publish rename but before it cleared the workspace
// leaves the tree the install replaced in a backup beside the live plugin.
// Removing that plugin deletes the live tree and its lockfile entry, so a
// backup that outlived it would be published again by the next install's
// recovery, and Load enumerates directories rather than the lockfile: the
// removed plugin would be live again with no entry naming it.
func TestRemoveLeavesNoSupersededBackupARecoveryCanResurrect(t *testing.T) {
	dir := t.TempDir()
	src := writeSourcePlugin(t, filepath.Join(t.TempDir(), "src"), validManifest())
	if _, err := Install(context.Background(), InstallOptions{Source: src, Dir: dir}); err != nil {
		t.Fatalf("seeding the install: %v", err)
	}

	// The state a kill in that window leaves: the plugin live at its target,
	// with the tree it replaced still retained in the workspace beside it.
	staged, _, err := installtxn.StageDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Dir(staged)
	writeWorkspaceMarker(t, workspace, "zero.demo")
	previous := filepath.Join(workspace, "previous")
	if err := os.MkdirAll(previous, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(dir, "zero.demo", manifestFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(previous, manifestFileName), manifest, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Remove(dir, "zero.demo"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	other := validManifest()
	other["id"] = "zero.other"
	src2 := writeSourcePlugin(t, filepath.Join(t.TempDir(), "src2"), other)
	if _, err := Install(context.Background(), InstallOptions{Source: src2, Dir: dir}); err != nil {
		t.Fatalf("later install: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "zero.demo")); !os.IsNotExist(err) {
		t.Errorf("a removed plugin was put back on disk: %v", err)
	}
	loaded, err := Load(LoadOptions{Roots: []Root{{Source: SourceUser, Path: dir}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range loaded.Plugins {
		if p.ID == "zero.demo" {
			t.Errorf("a removed plugin is loadable again")
		}
	}
	lock, err := ReadLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := lock["zero.demo"]; ok {
		t.Errorf("the lockfile still names a removed plugin")
	}
}

// writeWorkspaceMarker plants the ownership marker installtxn.CommitDir writes
// before it moves a live tree aside. Recovery acts only on a workspace carrying
// that marker's magic first line, so a test standing in for a killed process has
// to write the real thing: the workspace prefix and a previous directory beside
// it are shapes ordinary user content can have too, and prove nothing.
func writeWorkspaceMarker(t *testing.T, workspace string, id string) {
	t.Helper()
	marker := []byte("zero-install-txn v1\ntarget " + id + "\n")
	if err := os.WriteFile(filepath.Join(workspace, ".zero-install-txn"), marker, 0o600); err != nil {
		t.Fatal(err)
	}
}

// generationSource writes a plugin source whose content names which generation
// it is, so a test can tell the recovered tree from the one it replaced by
// reading the tree on disk instead of trusting the lockfile that is itself
// under test.
func generationSource(t *testing.T, generation string) string {
	t.Helper()
	versions := map[string]string{"old": "0.1.0", "new": "0.2.0"}
	manifest := validManifest()
	manifest["version"] = versions[generation]
	dir := writeSourcePlugin(t, filepath.Join(t.TempDir(), "src-"+generation), manifest)
	if err := os.WriteFile(filepath.Join(dir, "generation.txt"), []byte(generation), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// interruptedUpdate is a plugins dir with the old generation installed, plus
// everything the update over it would have used. An update is the only shape
// that leaves a backup owed a restore, so every interrupted state below is one.
type interruptedUpdate struct {
	dir       string
	oldSource string
	oldHash   string
	newSource string
	newHash   string
}

func seedInterruptedUpdate(t *testing.T) interruptedUpdate {
	t.Helper()
	dir := t.TempDir()
	seeded, err := Install(context.Background(), InstallOptions{Source: generationSource(t, "old"), Dir: dir})
	if err != nil {
		t.Fatalf("seeding the install: %v", err)
	}
	newSource := generationSource(t, "new")
	newHash, err := hashTree(newSource)
	if err != nil {
		t.Fatal(err)
	}
	return interruptedUpdate{
		dir:       dir,
		oldSource: seeded.Source,
		oldHash:   seeded.Hash,
		newSource: canonicalSource(newSource),
		newHash:   newHash,
	}
}

// plantWorkspace builds what CommitDir has on disk before it touches the live
// tree: a staged copy of the new generation and the ownership marker naming the
// install it is about to replace.
func (u interruptedUpdate) plantWorkspace(t *testing.T) string {
	t.Helper()
	staged, _, err := installtxn.StageDir(u.dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := copyTree(u.newSource, staged); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Dir(staged)
	writeWorkspaceMarker(t, workspace, "zero.demo")
	return workspace
}

func (u interruptedUpdate) target() string {
	return filepath.Join(u.dir, "zero.demo")
}

func (u interruptedUpdate) mustRename(t *testing.T, from string, to string) {
	t.Helper()
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
}

// publishNewEntry writes the lockfile the interrupted update was about to
// publish, which is the only thing separating a commit killed before the
// publish from one killed after it.
func (u interruptedUpdate) publishNewEntry(t *testing.T) {
	t.Helper()
	lock, err := ReadLock(u.dir)
	if err != nil {
		t.Fatal(err)
	}
	lock["zero.demo"] = LockEntry{Source: u.newSource, Hash: u.newHash}
	if err := writeLock(u.dir, lock); err != nil {
		t.Fatal(err)
	}
}

// recoveryWant is the state a resolved workspace has to leave behind.
type recoveryWant struct {
	generation string
	version    string
	source     string
	hash       string
	workspace  bool
}

// interruptedState is one point CommitDir can be killed at, with the state the
// next run must arrive at.
type interruptedState struct {
	name  string
	plant func(t *testing.T, u interruptedUpdate) string
	want  func(u interruptedUpdate) recoveryWant
}

func interruptedStates() []interruptedState {
	oldTree := func(u interruptedUpdate, workspace bool) recoveryWant {
		return recoveryWant{generation: "old", version: "0.1.0", source: u.oldSource, hash: u.oldHash, workspace: workspace}
	}
	return []interruptedState{
		{
			// Killed after the marker, before anything moved. Nothing is owed a
			// restore, and the workspace belongs to whoever made it.
			name: "S1_after_marker",
			plant: func(t *testing.T, u interruptedUpdate) string {
				return u.plantWorkspace(t)
			},
			want: func(u interruptedUpdate) recoveryWant { return oldTree(u, true) },
		},
		{
			// Killed between the two renames: the backup is the only copy of the
			// plugin in existence and nothing is at the target.
			name: "S2_after_retaining_previous",
			plant: func(t *testing.T, u interruptedUpdate) string {
				workspace := u.plantWorkspace(t)
				u.mustRename(t, u.target(), filepath.Join(workspace, "previous"))
				return workspace
			},
			want: func(u interruptedUpdate) recoveryWant { return oldTree(u, false) },
		},
		{
			// Killed after the swap and before the publish. Both trees are on disk
			// and only the lockfile says which one the user has: it still records
			// the backup, so the backup goes back and the new tree is dropped.
			name: "S3_after_swap_before_publish",
			plant: func(t *testing.T, u interruptedUpdate) string {
				workspace := u.plantWorkspace(t)
				u.mustRename(t, u.target(), filepath.Join(workspace, "previous"))
				u.mustRename(t, filepath.Join(workspace, "staged"), u.target())
				return workspace
			},
			want: func(u interruptedUpdate) recoveryWant { return oldTree(u, false) },
		},
		{
			// The same shape on disk as S3, told apart only by the published entry:
			// the update committed, so the live tree stands and the backup beside
			// it is superseded.
			name: "S4_after_publish_before_cleanup",
			plant: func(t *testing.T, u interruptedUpdate) string {
				workspace := u.plantWorkspace(t)
				u.mustRename(t, u.target(), filepath.Join(workspace, "previous"))
				u.mustRename(t, filepath.Join(workspace, "staged"), u.target())
				u.publishNewEntry(t)
				return workspace
			},
			want: func(u interruptedUpdate) recoveryWant {
				return recoveryWant{generation: "new", version: "0.2.0", source: u.newSource, hash: u.newHash, workspace: false}
			},
		},
		{
			// Killed inside a rollback: the failed tree is already set aside and the
			// backup is again the only copy, which is the state that ordering buys.
			name: "S5_interrupted_rollback",
			plant: func(t *testing.T, u interruptedUpdate) string {
				workspace := u.plantWorkspace(t)
				u.mustRename(t, u.target(), filepath.Join(workspace, "previous"))
				u.mustRename(t, filepath.Join(workspace, "staged"), filepath.Join(workspace, "failed"))
				return workspace
			},
			want: func(u interruptedUpdate) recoveryWant { return oldTree(u, false) },
		},
	}
}

// installOther installs an unrelated plugin, which is how a recovery pass is
// driven: the next install over the same dir takes the same lock.
func installOther(t *testing.T, dir string, id string) error {
	t.Helper()
	manifest := validManifest()
	manifest["id"] = id
	source := writeSourcePlugin(t, filepath.Join(t.TempDir(), id), manifest)
	_, err := Install(context.Background(), InstallOptions{Source: source, Dir: dir})
	return err
}

func assertRecovered(t *testing.T, u interruptedUpdate, workspace string, want recoveryWant) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(u.target(), "generation.txt"))
	if err != nil {
		t.Fatalf("reading the live tree: %v", err)
	}
	if string(data) != want.generation {
		t.Errorf("the live tree is the %q generation, want %q", data, want.generation)
	}
	lock, err := ReadLock(u.dir)
	if err != nil {
		t.Fatal(err)
	}
	entry := lock["zero.demo"]
	if entry.Source != want.source {
		t.Errorf("lock source = %q, want %q", entry.Source, want.source)
	}
	if entry.Hash != want.hash {
		t.Errorf("lock hash = %q, want %q", entry.Hash, want.hash)
	}
	// A tree that only stats is not a recovered install: it has to come back
	// through the same loader the rest of the program reads plugins with.
	loaded, err := Load(LoadOptions{Roots: []Root{{Source: SourceUser, Path: u.dir}}})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, plugin := range loaded.Plugins {
		if plugin.ID != "zero.demo" {
			continue
		}
		found = true
		if plugin.Version != want.version {
			t.Errorf("Load sees version %q, want %q", plugin.Version, want.version)
		}
	}
	if !found {
		t.Errorf("Load does not see the recovered plugin")
	}
	_, err = os.Stat(workspace)
	if want.workspace && err != nil {
		t.Errorf("the workspace should be left alone, got %v", err)
	}
	if !want.workspace && !os.IsNotExist(err) {
		t.Errorf("the resolved workspace should be cleared, got %v", err)
	}
}

func assertRemovedForGood(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, "zero.demo")); !os.IsNotExist(err) {
		t.Errorf("a removed plugin is on disk again: %v", err)
	}
	lock, err := ReadLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := lock["zero.demo"]; ok {
		t.Errorf("the lockfile still names a removed plugin")
	}
	loaded, err := Load(LoadOptions{Roots: []Root{{Source: SourceUser, Path: dir}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, plugin := range loaded.Plugins {
		if plugin.ID == "zero.demo" {
			t.Errorf("a removed plugin is loadable again")
		}
	}
}

// Every point an update can be killed at has one right resolution, and the next
// install is one of the two places it happens.
func TestInstallResolvesEveryInterruptedUpdateState(t *testing.T) {
	for _, state := range interruptedStates() {
		t.Run(state.name, func(t *testing.T) {
			u := seedInterruptedUpdate(t)
			workspace := state.plant(t, u)

			if err := installOther(t, u.dir, "zero.other"); err != nil {
				t.Fatalf("install after the interrupted update: %v", err)
			}
			assertRecovered(t, u, workspace, state.want(u))

			// A second pass must find nothing to do. Recovery that resolved the same
			// workspace twice would undo the state it just settled on.
			if err := installOther(t, u.dir, "zero.third"); err != nil {
				t.Fatalf("second recovery pass: %v", err)
			}
			assertRecovered(t, u, workspace, state.want(u))
		})
	}
}

// The other place recovery runs is a removal, and it has to run there first:
// removing without resolving leaves a backup the next install republishes,
// reinstating a plugin the user deleted.
func TestRemoveResolvesEveryInterruptedUpdateStateAndSticks(t *testing.T) {
	for _, state := range interruptedStates() {
		t.Run(state.name, func(t *testing.T) {
			u := seedInterruptedUpdate(t)
			state.plant(t, u)

			if err := Remove(u.dir, "zero.demo"); err != nil {
				t.Fatalf("remove after the interrupted update: %v", err)
			}
			assertRemovedForGood(t, u.dir)

			if err := installOther(t, u.dir, "zero.other"); err != nil {
				t.Fatalf("install after the removal: %v", err)
			}
			assertRemovedForGood(t, u.dir)
		})
	}
}

// assertUntouched pins the two things a caller must not have changed after
// recovery reported a transaction it could not resolve.
func assertUntouched(t *testing.T, u interruptedUpdate, generation string, want LockEntry) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(u.target(), "generation.txt"))
	if err != nil {
		t.Fatalf("reading the live tree: %v", err)
	}
	if string(data) != generation {
		t.Errorf("the live tree is the %q generation, want %q", data, generation)
	}
	lock, err := ReadLock(u.dir)
	if err != nil {
		t.Fatal(err)
	}
	if lock["zero.demo"] != want {
		t.Errorf("lock entry = %+v, want %+v", lock["zero.demo"], want)
	}
	if _, ok := lock["zero.other"]; ok {
		t.Errorf("the aborted install still published its own entry")
	}
	if _, err := os.Stat(filepath.Join(u.dir, "zero.other")); !os.IsNotExist(err) {
		t.Errorf("the aborted install still wrote its tree: %v", err)
	}
}

// Neither tree matches what the lockfile records, so there is no answer to give
// and either guess destroys the tree the user is actually owed. The failure has
// to reach the caller, which must not install over or remove anything.
func TestInterruptedUpdateMatchingNeitherTreeAbortsBothCallers(t *testing.T) {
	u := seedInterruptedUpdate(t)
	workspace := u.plantWorkspace(t)
	u.mustRename(t, u.target(), filepath.Join(workspace, "previous"))
	u.mustRename(t, filepath.Join(workspace, "staged"), u.target())
	unmatched := LockEntry{Source: u.oldSource, Hash: "sha256:0000000000000000000000000000000000000000000000000000000000000000"}
	lock, err := ReadLock(u.dir)
	if err != nil {
		t.Fatal(err)
	}
	lock["zero.demo"] = unmatched
	if err := writeLock(u.dir, lock); err != nil {
		t.Fatal(err)
	}

	if err := installOther(t, u.dir, "zero.other"); err == nil {
		t.Errorf("Install did not report the unresolved transaction")
	}
	assertUntouched(t, u, "new", unmatched)
	if err := Remove(u.dir, "zero.demo"); err == nil {
		t.Errorf("Remove did not report the unresolved transaction")
	}
	assertUntouched(t, u, "new", unmatched)
	if _, err := os.Stat(filepath.Join(workspace, "previous")); err != nil {
		t.Errorf("the backup should still be there for a hand rescue, got %v", err)
	}
}

// A lockfile that cannot be read is not an empty one: the reconciler has no
// answer, and a caller that went on would install over or report the removal of
// a tree that may still be owed a restore.
func TestInterruptedUpdateWithAnUnreadableLockAbortsBothCallers(t *testing.T) {
	u := seedInterruptedUpdate(t)
	workspace := u.plantWorkspace(t)
	u.mustRename(t, u.target(), filepath.Join(workspace, "previous"))
	u.mustRename(t, filepath.Join(workspace, "staged"), u.target())
	if err := os.WriteFile(filepath.Join(u.dir, LockFileName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := installOther(t, u.dir, "zero.other"); err == nil {
		t.Errorf("Install did not report the unresolved transaction")
	}
	if err := Remove(u.dir, "zero.demo"); err == nil {
		t.Errorf("Remove did not report the unresolved transaction")
	}
	data, err := os.ReadFile(filepath.Join(u.target(), "generation.txt"))
	if err != nil || string(data) != "new" {
		t.Errorf("the live tree changed under an unresolved transaction: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "previous")); err != nil {
		t.Errorf("the backup should still be there for a hand rescue, got %v", err)
	}
}

// A restore that cannot be carried out is the case where silence is worst: the
// plugin is missing from the target and its only copy is in a workspace nothing
// else reads.
// skipWithoutPermissionFaults skips a test that injects a failure by making a
// directory unwritable. Windows does not block renames or deletes that way, and
// root ignores the permission entirely, so on both the injected failure never
// fires and the test would assert against a success it never meant to produce.
func skipWithoutPermissionFaults(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not block renames or removals on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permissions this test relies on")
	}
}

func TestAFailedRestoreAbortsBothCallers(t *testing.T) {
	skipWithoutPermissionFaults(t)
	u := seedInterruptedUpdate(t)
	workspace := u.plantWorkspace(t)
	u.mustRename(t, u.target(), filepath.Join(workspace, "previous"))
	// A workspace that cannot be written is the cheap stand-in for a rename that
	// fails: the backup cannot leave it.
	if err := os.Chmod(workspace, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(workspace, 0o755) })

	if err := installOther(t, u.dir, "zero.other"); err == nil {
		t.Errorf("Install did not report the failed restore")
	}
	if err := Remove(u.dir, "zero.demo"); err == nil {
		t.Errorf("Remove did not report the failed restore")
	}
	if _, err := os.Stat(u.target()); !os.IsNotExist(err) {
		t.Errorf("nothing should have been written at the target: %v", err)
	}
	lock, err := ReadLock(u.dir)
	if err != nil {
		t.Fatal(err)
	}
	if lock["zero.demo"] != (LockEntry{Source: u.oldSource, Hash: u.oldHash}) {
		t.Errorf("lock entry = %+v, want the untouched old entry", lock["zero.demo"])
	}
	if _, ok := lock["zero.other"]; ok {
		t.Errorf("the aborted install still published its own entry")
	}
	if _, err := os.Stat(filepath.Join(workspace, "previous")); err != nil {
		t.Errorf("the only copy of the plugin should still be there, got %v", err)
	}
}

// A publish always writes the lockfile entry, so an interrupted update with no
// entry naming it proves the publish never ran and the backup is still the tree
// the lockfile describes, even though there is no hash left to match it against.
func TestInterruptedUpdateWithNoLockEntryAbortsTheCaller(t *testing.T) {
	u := seedInterruptedUpdate(t)
	workspace := u.plantWorkspace(t)
	u.mustRename(t, u.target(), filepath.Join(workspace, "previous"))
	u.mustRename(t, filepath.Join(workspace, "staged"), u.target())
	lock, err := ReadLock(u.dir)
	if err != nil {
		t.Fatal(err)
	}
	delete(lock, "zero.demo")
	if err := writeLock(u.dir, lock); err != nil {
		t.Fatal(err)
	}
	err = installOther(t, u.dir, "zero.other")

	if err == nil {
		t.Fatal("a live tree the lockfile does not describe must stop the caller")
	}
	assertUntouched(t, u, "new", LockEntry{})
}

// A directory at the target that the lockfile does not name is not proof that a
// publish was interrupted. Anything can have created it: `zero tools make`, a
// hand-written plugin, a restored file sync. Recovery used to read a missing
// entry as proof the publish never ran and replace that tree with the retained
// backup, which deleted whatever was there.
func TestAnUnrelatedTreeAtTheTargetIsNeverReplaced(t *testing.T) {
	dir := t.TempDir()
	workspace, err := os.MkdirTemp(dir, ".zero-install-txn-")
	if err != nil {
		t.Fatal(err)
	}
	marker := []byte("zero-install-txn v1\ntarget zero.demo\n")
	if err := os.WriteFile(filepath.Join(workspace, ".zero-install-txn"), marker, 0o600); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(workspace, "previous")
	writeSourcePlugin(t, backup, validManifest())
	// The user's own tree, which no lockfile entry names.
	mine := filepath.Join(dir, "zero.demo")
	writeSourcePlugin(t, mine, validManifest())
	if err := os.WriteFile(filepath.Join(mine, "mycode.txt"), []byte("my work"), 0o644); err != nil {
		t.Fatal(err)
	}

	err = installOther(t, dir, "zero.other")

	if err == nil {
		t.Fatal("recovery must not act on a tree the lockfile does not describe")
	}
	if _, statErr := os.Stat(filepath.Join(mine, "mycode.txt")); statErr != nil {
		t.Fatalf("the user's own tree was destroyed: %v", statErr)
	}
}

// An entry that records no hash describes no tree, so neither side can be
// matched against it and there is no phase to report.
func TestInterruptedUpdateWithAHashlessLockEntryAbortsTheCaller(t *testing.T) {
	u := seedInterruptedUpdate(t)
	workspace := u.plantWorkspace(t)
	u.mustRename(t, u.target(), filepath.Join(workspace, "previous"))
	u.mustRename(t, filepath.Join(workspace, "staged"), u.target())
	hashless := LockEntry{Source: u.oldSource}
	lock, err := ReadLock(u.dir)
	if err != nil {
		t.Fatal(err)
	}
	lock["zero.demo"] = hashless
	if err := writeLock(u.dir, lock); err != nil {
		t.Fatal(err)
	}

	if err := installOther(t, u.dir, "zero.other"); err == nil {
		t.Errorf("Install did not report the unresolved transaction")
	}
	assertUntouched(t, u, "new", hashless)
}

// interruptedCaller is one of the two entry points that take the install lock.
// Both must abort on a transaction recovery could not resolve.
type interruptedCaller struct {
	name string
	run  func(t *testing.T, dir string) error
}

func interruptedCallers() []interruptedCaller {
	return []interruptedCaller{
		{name: "install", run: func(t *testing.T, dir string) error { return installOther(t, dir, "zero.other") }},
		{name: "remove", run: func(t *testing.T, dir string) error { return Remove(dir, "zero.demo") }},
	}
}

// Retiring a superseded backup can fail too, and leaving it behind is not
// harmless: a later removal deletes the live tree, and a backup that outlived it
// would be read as an interrupted swap and published again. Each caller gets its
// own planted state here, because a retirement that fails partway can take the
// ownership marker with it and leave the next pass a workspace it cannot
// attribute.
func TestAFailedRetirementAbortsBothCallers(t *testing.T) {
	skipWithoutPermissionFaults(t)
	for _, caller := range interruptedCallers() {
		t.Run(caller.name, func(t *testing.T) {
			u := seedInterruptedUpdate(t)
			workspace := u.plantWorkspace(t)
			u.mustRename(t, u.target(), filepath.Join(workspace, "previous"))
			u.mustRename(t, filepath.Join(workspace, "staged"), u.target())
			u.publishNewEntry(t)
			// The backup's own contents cannot be unlinked, so removing the
			// workspace around it fails.
			if err := os.Chmod(filepath.Join(workspace, "previous"), 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(filepath.Join(workspace, "previous"), 0o755) })

			if err := caller.run(t, u.dir); err == nil {
				t.Errorf("%s did not report the failed retirement", caller.name)
			}
			assertUntouched(t, u, "new", LockEntry{Source: u.newSource, Hash: u.newHash})
		})
	}
}

// The workspace prefix is a public dot prefixed name and a previous directory
// beside a target file is a shape ordinary content can have, so neither is
// evidence. Without the magic marker this is somebody's content and recovery
// must leave it exactly as it found it, and say nothing.
func TestRecoveryLeavesAPrefixCollidingDirectoryAlone(t *testing.T) {
	dir := t.TempDir()
	collided := filepath.Join(dir, ".zero-install-txn-notes")
	previous := filepath.Join(collided, "previous")
	if err := os.MkdirAll(previous, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(previous, "SKILL.md"), []byte("notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(collided, "target"), []byte("zero.demo"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := installOther(t, dir, "zero.other"); err != nil {
		t.Fatalf("install alongside a prefix colliding directory: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(previous, "SKILL.md"))
	if err != nil || string(data) != "notes\n" {
		t.Errorf("user content inside the colliding directory was disturbed: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(collided, "target")); err != nil {
		t.Errorf("the colliding directory lost a file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "zero.demo")); !os.IsNotExist(err) {
		t.Errorf("recovery published content it does not own: %v", err)
	}
}
