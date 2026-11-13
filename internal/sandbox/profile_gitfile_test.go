package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A LINKED WORKTREE HAS .git AS A FILE.
//
// The Windows principal plan materializes every carveout, and it does so by
// descending through .git as a directory. Against a `gitdir:` pointer file that
// walk cannot open a child, so opted-in elevated setup aborts and the sandbox
// cannot be used in a worktree at all. Zero's own development worktrees have
// exactly this layout, so this is the common case for anyone working on the
// sandbox itself.
func TestGitCarveoutsHandleAWorktreeGitfile(t *testing.T) {
	root := t.TempDir()
	gitFile := filepath.Join(root, ".git")
	// The real shape git writes for a linked worktree.
	if err := os.WriteFile(gitFile, []byte("gitdir: "+filepath.Join(root, "..", "main", ".git", "worktrees", "wt")+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	specs := gitMetadataWriteCarveoutSpecs(root)
	if len(specs) != 1 {
		t.Fatalf("carveout specs = %#v, want only the .git pointer file", specs)
	}
	if filepath.Clean(specs[0].Path) != filepath.Clean(gitFile) {
		t.Fatalf("carveout path = %s, want the .git pointer file %s", specs[0].Path, gitFile)
	}
	if !specs[0].IsFile {
		t.Fatal("the .git pointer carveout is not marked as a file, so materialization would create a directory where git needs a file")
	}
	// Nothing may name a path BENEATH the pointer file, which is what forces the
	// directory descent that aborts setup.
	for _, path := range gitMetadataWriteCarveouts(root) {
		rest := strings.TrimPrefix(filepath.Clean(path), filepath.Clean(gitFile))
		if rest != "" {
			t.Errorf("carveout %s descends through the .git pointer file, which cannot have children", path)
		}
	}
}

// The ordinary repository layout must keep the config and hooks carveouts. They
// are the whole reason the set exists: without them a principal reinstates
// credential.helper or core.hooksPath through inherited workspace write access.
func TestGitCarveoutsKeepConfigAndHooksForARealGitDir(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	paths := gitMetadataWriteCarveouts(root)
	for _, want := range []string{filepath.Join(root, ".git", "hooks"), filepath.Join(root, ".git", "config")} {
		found := false
		for _, got := range paths {
			if filepath.Clean(got) == filepath.Clean(want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("carveout %s missing for an ordinary .git directory: %#v", want, paths)
		}
	}
}

// A workspace where git has never run keeps the directory-shaped carveouts. That
// is what lets the Windows plan create them before git first runs, so the deny
// ACEs are already in place rather than being applied to paths git will create
// later with inherited write access.
func TestGitCarveoutsKeepConfigAndHooksWhenGitIsAbsent(t *testing.T) {
	root := t.TempDir()
	paths := gitMetadataWriteCarveouts(root)
	if len(paths) != 2 {
		t.Fatalf("carveouts = %#v, want the config and hooks pair when .git is absent", paths)
	}
}
