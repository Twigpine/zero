package memory

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNestedRepositoryCannotHideOuterTrackedLocalNote(t *testing.T) {
	for _, atStore := range []bool{false, true} {
		t.Run(map[bool]string{false: "ancestor", true: "store"}[atStore], func(t *testing.T) {
			root := t.TempDir()
			paths := DefaultPaths(root)
			const original = "---\nname: private\n---\n\noriginal\n"
			p := writeStoreFile(t, paths, "private.md", original)
			writeStoreFile(t, paths, gitignoreName, localIgnoreContent)
			seedStoreRepo(t, root)
			runStoreGit(t, root, "add", "-f", ".zero/memory/local/private.md")
			runStoreGit(t, root, "commit", "-m", "track local note")
			nested := filepath.Dir(paths.LocalDir)
			if atStore {
				nested = paths.LocalDir
			}
			runStoreGit(t, nested, "init")
			if _, err := Write(paths, ScopeLocal, "private", "d", "secret"); !errors.Is(err, ErrNotPrivate) {
				t.Fatalf("outer index bypass: %v", err)
			}
			body, err := os.ReadFile(p)
			if err != nil || string(body) != original {
				t.Fatalf("tracked body changed: %q, %v", body, err)
			}
		})
	}
}

func TestMixedCaseGitIndexRedirectCannotHideLocalNote(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("environment keys fold case on Windows")
	}
	for _, key := range []string{"git_index_file", "Git_Index_File"} {
		t.Run(key, func(t *testing.T) {
			root := t.TempDir()
			paths := DefaultPaths(root)
			writeStoreFile(t, paths, "private.md", "---\nname: private\n---\n\noriginal\n")
			seedStoreRepo(t, root)
			runStoreGit(t, root, "add", ".zero/memory/local/private.md")
			runStoreGit(t, root, "commit", "-m", "track local note")
			t.Setenv(key, filepath.Join(t.TempDir(), "empty.index"))
			if _, err := Write(paths, ScopeLocal, "private", "d", "secret"); !errors.Is(err, ErrNotPrivate) {
				t.Fatalf("%s bypass: %v", key, err)
			}
		})
	}
}

func TestGitDiscoveryEnvironmentUsesPlatformKeySemantics(t *testing.T) {
	input := []string{"GIT_DIR=elsewhere", "git_index_file=empty", "Git_Work_Tree=other", "UNCHANGED=value"}
	got := strings.Join(gitDiscoveryEnv(input), "\n")
	if strings.Contains(got, "GIT_DIR=") {
		t.Fatal("uppercase redirect kept")
	}
	if runtime.GOOS == "windows" && strings.Contains(strings.ToUpper(got), "GIT_") {
		t.Fatalf("Windows redirect kept: %s", got)
	}
	if runtime.GOOS != "windows" && !strings.Contains(got, "git_index_file=empty") {
		t.Fatal("case-sensitive environment key removed")
	}
}
