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
