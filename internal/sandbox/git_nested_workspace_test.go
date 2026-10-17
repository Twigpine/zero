package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A WORKSPACE INSIDE ANOTHER REPOSITORY KEEPS ITS CARVEOUTS.
//
// This branch skipped them for a while, so that Zero would not create a .git in
// a repository it does not own. The cost was worse than the thing avoided:
// nothing then denied writes to <root>/.git, so a sandboxed command could build
// a repository there by hand and put core.fsmonitor in its config, and Zero runs
// git outside the sandbox against that workspace. The nested-repository refusal
// recognises git creating a repository; it cannot recognise mkdir.
//
// TestCarveoutsInsideAnAncestorRepositoryDoNotCaptureDiscovery measures the
// reason the skip existed and shows it does not hold.
func TestCarveoutsAreSynthesizedInsideAnAncestorRepository(t *testing.T) {
	parent := t.TempDir()
	if err := os.MkdirAll(filepath.Join(parent, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(parent, "sub", "project")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}

	// SETUP: the workspace itself has no .git, or this is a different case.
	if _, err := os.Lstat(filepath.Join(workspace, ".git")); err == nil {
		t.Fatal("SETUP INVALID: the workspace already carries .git")
	}
	if !gitMetadataGovernedByAncestor(workspace) {
		t.Fatal("SETUP INVALID: the ancestor is not detected, so this proves nothing")
	}

	specs := gitMetadataWriteCarveoutSpecs(workspace)
	want := map[string]bool{
		filepath.Join(workspace, ".git", "config"): true,
		filepath.Join(workspace, ".git", "hooks"):  true,
	}
	for _, spec := range specs {
		delete(want, spec.Path)
	}
	for path := range want {
		t.Errorf("a workspace inside another repository gets no carveout for %s, so nothing denies writing it", path)
	}
}

// The reason the skip existed, measured rather than argued: materializing only
// config and hooks does not make the directory a repository, so git's discovery
// walk still reaches the ancestor and a config planted there is never read.
// If a future git changes that, this fails and the trade has to be reopened.
func TestCarveoutsInsideAnAncestorRepositoryDoNotCaptureDiscovery(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git on PATH")
	}
	parent := t.TempDir()
	run := func(dir string, args ...string) (string, error) {
		command := exec.Command(git, args...)
		command.Dir = dir
		command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+parent, "USERPROFILE="+parent)
		out, err := command.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if out, err := run(parent, "init", "-q", "."); err != nil {
		t.Skipf("cannot create the ancestor repository: %v: %s", err, out)
	}
	workspace := filepath.Join(parent, "sub", "project")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	ancestor, err := run(workspace, "rev-parse", "--show-toplevel")
	if err != nil {
		t.Skipf("cannot resolve the ancestor: %v: %s", err, ancestor)
	}

	// Materialize exactly what the plan materializes, and make the config as
	// hostile as a sandboxed command would if it could write one.
	for _, spec := range gitMetadataWriteCarveoutSpecs(workspace) {
		if spec.IsFile {
			if err := os.MkdirAll(filepath.Dir(spec.Path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(spec.Path, []byte("[core]\n\tfsmonitor = \"exit 9\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(spec.Path, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	after, err := run(workspace, "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatalf("git stopped working in the workspace after the carveouts were materialized: %v: %s", err, after)
	}
	if after != ancestor {
		t.Fatalf("discovery moved from %s to %s: the materialized carveouts captured the walk, which is the trade this test exists to watch", ancestor, after)
	}
	if out, err := run(workspace, "status", "--porcelain"); err != nil {
		t.Fatalf("git status failed in the workspace, so the planted config was read: %v: %s", err, out)
	}
}

// And a standalone directory keeps the materialized carveouts, or the guard
// above would be satisfied by returning nothing everywhere and the protection
// would be gone for the case it was written for.
func TestCarveoutsStillCoverAStandaloneDirectory(t *testing.T) {
	workspace := t.TempDir()
	if gitMetadataGovernedByAncestor(workspace) {
		t.Skip("this temp directory sits inside a repository, so it is not the standalone case")
	}

	specs := gitMetadataWriteCarveoutSpecs(workspace)
	wantConfig := filepath.Join(workspace, ".git", "config")
	wantHooks := filepath.Join(workspace, ".git", "hooks")
	var sawConfig, sawHooks bool
	for _, spec := range specs {
		switch spec.Path {
		case wantConfig:
			sawConfig = true
			if !spec.IsFile {
				t.Errorf("%s is planned as a directory; creating .git/config as a directory makes git init fail", spec.Path)
			}
		case wantHooks:
			sawHooks = true
		}
	}
	if !sawConfig || !sawHooks {
		t.Fatalf("a standalone directory lost its carveouts: config=%t hooks=%t specs=%+v", sawConfig, sawHooks, specs)
	}
}

// A linked worktree is unchanged: the pointer file is denied and nothing beneath
// it is named, which is what keeps elevated setup from descending through a
// regular file.
func TestCarveoutsStillDenyTheLinkedWorktreePointer(t *testing.T) {
	workspace := t.TempDir()
	pointer := filepath.Join(workspace, ".git")
	if err := os.WriteFile(pointer, []byte("gitdir: ../real/.git/worktrees/w\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	specs := gitMetadataWriteCarveoutSpecs(workspace)
	if len(specs) != 1 || specs[0].Path != pointer || !specs[0].IsFile {
		t.Fatalf("linked worktree carveouts = %+v, want exactly the file-shaped pointer %s", specs, pointer)
	}
}

// The ancestor test itself has to distinguish shapes, because a linked worktree
// or submodule ancestor carries .git as a FILE and still owns this directory.
func TestAncestorDetectionAcceptsBothGitShapes(t *testing.T) {
	for _, shape := range []struct {
		name string
		make func(t *testing.T, parent string)
	}{
		{name: "directory", make: func(t *testing.T, parent string) {
			if err := os.MkdirAll(filepath.Join(parent, ".git"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "pointer file", make: func(t *testing.T, parent string) {
			if err := os.WriteFile(filepath.Join(parent, ".git"), []byte("gitdir: elsewhere\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			parent := t.TempDir()
			shape.make(t, parent)
			workspace := filepath.Join(parent, "nested", "deep")
			if err := os.MkdirAll(workspace, 0o700); err != nil {
				t.Fatal(err)
			}
			if !gitMetadataGovernedByAncestor(workspace) {
				t.Fatalf("an ancestor carrying .git as a %s was not recognised as governing %s", shape.name, workspace)
			}
		})
	}
}

// A NESTED WORKSPACE THAT OWNS ITS OWN REPOSITORY KEEPS ITS OWN CARVEOUTS.
//
// The ancestor branch above exists so Zero does not synthesize a control
// directory inside somebody else's repository. It was asked unconditionally,
// so a workspace that already HAS its own .git lost the hooks and config
// carveouts too — and those are its own metadata, not the ancestor's. Nothing
// compensated: workspaceGovernedByAncestorRepository answers the same question
// by testing for a local .git, so it reported this workspace as not governed
// and the initialization refusal never fired. Commands then received plain
// workspace write access over a live .git/config and .git/hooks, which is
// exactly the configuration that decides what git executes. Reported by
// @jatmn.
//
// Asserted through the profile as well as the spec helper, because the spec
// list is only load-bearing if it reaches ReadOnlySubpaths.
func TestNestedWorkspaceOwningGitKeepsItsOwnCarveouts(t *testing.T) {
	outer := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outer, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(outer, "project")
	if err := os.MkdirAll(filepath.Join(workspace, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}

	wantHooks := filepath.Join(workspace, ".git", "hooks")
	wantConfig := filepath.Join(workspace, ".git", "config")
	specs := gitMetadataWriteCarveoutSpecs(workspace)
	foundHooks, foundConfig := false, false
	for _, spec := range specs {
		switch {
		case spec.Path == wantHooks && !spec.IsFile:
			foundHooks = true
		case spec.Path == wantConfig && spec.IsFile:
			foundConfig = true
		}
	}
	if !foundHooks || !foundConfig {
		t.Fatalf("a workspace owning its own .git lost its carveouts: hooks=%v config=%v specs=%+v",
			foundHooks, foundConfig, specs)
	}

	// The specs only matter if they reach the profile the runner enforces.
	profile := PermissionProfileFromPolicy(workspace, DefaultPolicy(), nil)
	var root *WritableRoot
	for index := range profile.FileSystem.WriteRoots {
		if sameGitTestPath(profile.FileSystem.WriteRoots[index].Root, workspace) {
			root = &profile.FileSystem.WriteRoots[index]
			break
		}
	}
	if root == nil {
		t.Fatalf("no writable root for the workspace: %+v", profile.FileSystem.WriteRoots)
	}
	for _, want := range []string{wantHooks, wantConfig} {
		covered := false
		for _, sub := range root.ReadOnlySubpaths {
			if sameGitTestPath(sub, want) {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("%s is not read-only in the workspace write root: %+v", want, root.ReadOnlySubpaths)
		}
	}
}

// Compared through the profile's own canonicalization. PermissionProfileFromPolicy
// stores normalizeProfilePath(root), which resolves symlinks, so a raw t.TempDir()
// path does not match it on a runner where the temp directory is reached through
// one: macOS /var -> /private/var, and the Windows short-name expansion of the
// runner's profile directory. Comparing cleaned strings passed locally and failed
// on both CI runners for that reason alone.
func sameGitTestPath(a, b string) bool {
	return normalizeProfilePath(a) == normalizeProfilePath(b)
}
