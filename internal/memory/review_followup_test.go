package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/gitsafe"
)

func TestRunStoreGitIgnoresInheritedRepositoryRedirects(t *testing.T) {
	root, decoy := t.TempDir(), t.TempDir()
	seedStoreRepo(t, root)
	seedStoreRepo(t, decoy)
	t.Setenv("GIT_DIR", filepath.Join(decoy, ".git"))
	t.Setenv("GIT_COMMON_DIR", filepath.Join(decoy, ".git"))
	t.Setenv("GIT_WORK_TREE", decoy)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(decoy, ".git", "index"))
	got := strings.TrimSpace(runStoreGit(t, root, "rev-parse", "--show-toplevel"))
	actual, err := os.Stat(got)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(actual, want) {
		t.Fatalf("test Git helper reached inherited repository %q", got)
	}
}

func TestNoteSyncFailurePreservesExistingNoteAndRemovesTemporaryFile(t *testing.T) {
	paths := DefaultPaths(t.TempDir())
	path, err := Write(paths, ScopeProject, "note", "d", "original")
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected note sync failure")
	previous := syncNoteFile
	calls := 0
	syncNoteFile = func(file *os.File) error {
		calls++
		if _, err := file.Stat(); err != nil {
			t.Errorf("temporary file closed before sync: %v", err)
		}
		body, err := os.ReadFile(file.Name())
		if err != nil || !strings.Contains(string(body), "replacement") {
			t.Errorf("note was not written before sync: %q %v", body, err)
		}
		return injected
	}
	t.Cleanup(func() { syncNoteFile = previous })
	if _, err := Write(paths, ScopeProject, "note", "d", "replacement"); !errors.Is(err, injected) {
		t.Errorf("sync failure not returned: %v", err)
	}
	if calls != 1 {
		t.Errorf("sync calls=%d, want 1", calls)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(original) {
		t.Errorf("sync failure replaced the old note: %q %v", got, err)
	}
	entries, err := os.ReadDir(paths.ProjectDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "note.md" {
		t.Errorf("failed note left temporary files: %v", entries)
	}
}

func TestLocalPrivacyIndexReadDoesNotExecuteConfiguredGitPrograms(t *testing.T) {
	for _, tracked := range []bool{false, true} {
		t.Run(map[bool]string{false: "untracked", true: "tracked"}[tracked], func(t *testing.T) {
			root := t.TempDir()
			paths := DefaultPaths(root)
			seedStoreRepo(t, root)
			note := writeStoreFile(t, paths, "private.md", "---\nname: private\n---\n\noriginal\n")
			writeStoreFile(t, paths, gitignoreName, localIgnoreContent)
			if tracked {
				runStoreGit(t, root, "add", "-f", ".zero/memory/local/private.md")
			}
			marker := filepath.Join(root, "repository-program-ran")
			program := "echo invoked >> '" + strings.ReplaceAll(filepath.ToSlash(marker), "'", "'\\''") + "'"
			for _, key := range []string{"filter.tripwire.clean", "filter.tripwire.smudge", "filter.tripwire.process", "diff.tripwire.command", "diff.tripwire.textconv", "core.fsmonitor"} {
				runStoreGit(t, root, "config", key, program)
			}
			if err := os.WriteFile(filepath.Join(root, ".gitattributes"), []byte("* filter=tripwire diff=tripwire\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := gitsafe.Command(context.Background(), root, "status", "--porcelain"); !errors.Is(err, gitsafe.ErrRepositoryDefinesCommands) {
				t.Fatalf("general Git safety gate changed: %v", err)
			}
			_, err := Write(paths, ScopeLocal, "private", "d", "replacement")
			if tracked {
				if !errors.Is(err, ErrNotPrivate) {
					t.Errorf("tracked private note accepted: %v", err)
				}
				body, readErr := os.ReadFile(note)
				if readErr != nil || !strings.Contains(string(body), "original") {
					t.Errorf("tracked note changed: %q %v", body, readErr)
				}
			} else if err != nil {
				t.Errorf("index-only privacy query refused a harmless filter definition: %v", err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("repository-defined program ran: %v", err)
			}
		})
	}
}

func TestLocalPrivacyIndexErrorStaysClosed(t *testing.T) {
	root := t.TempDir()
	paths := DefaultPaths(root)
	seedStoreRepo(t, root)
	path := writeStoreFile(t, paths, "private.md", "---\nname: private\n---\n\noriginal\n")
	writeStoreFile(t, paths, gitignoreName, localIgnoreContent)
	if err := os.WriteFile(filepath.Join(root, ".git", "index"), []byte("corrupt index"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(paths, ScopeLocal, "private", "d", "replacement"); !errors.Is(err, ErrNotPrivate) {
		t.Fatalf("failed index query admitted a local write: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(body), "original") {
		t.Fatalf("failed privacy query replaced the note: %q %v", body, err)
	}
}
