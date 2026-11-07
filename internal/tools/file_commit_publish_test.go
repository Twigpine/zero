package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func installFileWriteRename(t *testing.T, rename func(*os.Root, string, string) error) {
	t.Helper()
	prior := fileWriteRename
	fileWriteRename = rename
	t.Cleanup(func() { fileWriteRename = prior })
}

func installFileWriteBeforePublish(t *testing.T, hook func(string)) {
	t.Helper()
	prior := fileWriteBeforePublish
	fileWriteBeforePublish = hook
	t.Cleanup(func() { fileWriteBeforePublish = prior })
}

func installFileWriteNoReplaceRename(t *testing.T, rename func(*os.Root, string, string) error) {
	t.Helper()
	prior := fileWriteNoReplaceRename
	fileWriteNoReplaceRename = rename
	t.Cleanup(func() { fileWriteNoReplaceRename = prior })
}

// A FINAL-COMPONENT SYMLINK STAYS A LINK. Main opened the target through the
// link and wrote through it, so replacing the link with a regular file leaves
// the link's target stuck on its old bytes. Publishing over the resolved path
// preserves both halves: the link is still a link, and the file it names has
// the new content.
//
// The public tools reject a symlink during path validation today, so this is
// asserted at the commit layer, which is where the publish semantics live and
// where a scoped write that resolves through an extra root reaches them.
func TestOverwriteKeepsAnInWorkspaceSymlink(t *testing.T) {
	for _, tc := range []struct {
		name  string
		chain bool
	}{
		{name: "direct link"},
		{name: "link chain", chain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "target.txt")
			if err := os.WriteFile(target, []byte("observed\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(root, "link.txt")
			if err := os.Symlink("target.txt", link); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			publishPath, links := "link.txt", []string{link}
			if tc.chain {
				chained := filepath.Join(root, "chained.txt")
				if err := os.Symlink("link.txt", chained); err != nil {
					t.Fatalf("create second symlink: %v", err)
				}
				publishPath, links = "chained.txt", append(links, chained)
			}

			workspace, err := os.OpenRoot(root)
			if err != nil {
				t.Fatal(err)
			}
			defer workspace.Close()
			expectedInfo, err := workspace.Stat(publishPath)
			if err != nil {
				t.Fatal(err)
			}
			preimage := "observed\n"
			if err := commitRootedFileContents(workspace, filepath.Join(root, publishPath), publishPath, expectedInfo, &preimage, "zero\n"); err != nil {
				t.Fatalf("symlink overwrite: %v", err)
			}
			for _, path := range links {
				info, err := os.Lstat(path)
				if err != nil {
					t.Fatalf("inspect %s: %v", path, err)
				}
				if info.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("%s is no longer a symlink: mode=%v", path, info.Mode())
				}
			}
			if got, err := os.ReadFile(target); err != nil || string(got) != "zero\n" {
				t.Fatalf("link target = %q, err=%v; the link must keep pointing at the rewritten file", got, err)
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasSuffix(entry.Name(), ".tmp") {
					t.Fatalf("staging file %s survived a successful publish", entry.Name())
				}
			}
		})
	}
}

// A READER CAN HOLD THE FILE OPEN. Windows refuses a rename over a name while
// another handle has it open without delete sharing, so the atomic publish is
// not available there; the commit must fall back to writing through the
// validated handle rather than failing an ordinary overwrite. The same test
// also runs on the rename-capable platforms, where it proves the common path.
func TestOverwriteSucceedsWithAnOpenReader(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "existing.txt")
	if err := os.WriteFile(target, []byte("observed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	result := NewScopedWriteFileTool(root, nil).Run(context.Background(), map[string]any{
		"path": "existing.txt", "content": "zero\n", "overwrite": true,
	})
	if result.Status != StatusOK {
		t.Fatalf("overwrite with an open reader = %s: %s", result.Status, result.Output)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "zero\n" {
		t.Fatalf("target = %q, err=%v", got, err)
	}
}

// WHEN THE PLATFORM REFUSES THE REPLACEMENT, the previous in-place publish is
// still available. The fallback reopens and re-validates the object first, so
// this is not a licence to write into whatever currently carries the name.
func TestOverwriteFallsBackToInPlaceWhenReplacementIsRefused(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "existing.txt")
	if err := os.WriteFile(target, []byte("observed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	installFileWriteRename(t, func(*os.Root, string, string) error {
		return errors.New("replacement refused")
	})

	result := NewScopedWriteFileTool(root, nil).Run(context.Background(), map[string]any{
		"path": "existing.txt", "content": "zero\n", "overwrite": true,
	})
	if result.Status != StatusOK {
		t.Fatalf("fallback overwrite = %s: %s", result.Status, result.Output)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "zero\n" {
		t.Fatalf("fallback target = %q, err=%v", got, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "existing.txt" {
		t.Fatalf("fallback left %d entries; the staged replacement must be removed", len(entries))
	}
}

// THE FALLBACK IS NOT A REPLACEMENT LICENCE. A competing writer that installs
// a new file during the refused publish must be detected by identity before
// the fallback truncates anything, even when the bytes are identical.
func TestOverwriteFallbackRefusesAReplacedTarget(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "existing.txt")
	if err := os.WriteFile(target, []byte("observed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	installFileWriteRename(t, func(*os.Root, string, string) error {
		preserved := target + ".preserved"
		if err := os.Rename(target, preserved); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte("observed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return errors.New("replacement refused")
	})

	result := NewScopedWriteFileTool(root, nil).Run(context.Background(), map[string]any{
		"path": "existing.txt", "content": "zero\n", "overwrite": true,
	})
	if result.Status != StatusError || !strings.Contains(result.Output, errFileChangedDuringWrite.Error()) {
		t.Fatalf("replaced fallback target = %s: %s", result.Status, result.Output)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "observed\n" {
		t.Fatalf("replacement = %q, err=%v; the fallback must not truncate a file it did not validate", got, err)
	}
}

// A FILESYSTEM MAY NOT OFFER A NO-REPLACE RENAME. The hard-link fallback is
// still atomic against readers — a reader sees the absent name or the complete
// inode — and refuses a name another writer created.
func TestCreateFallsBackToAHardLinkWhenNoReplaceRenameIsUnsupported(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "created.txt")
	installFileWriteNoReplaceRename(t, func(*os.Root, string, string) error {
		return errors.ErrUnsupported
	})

	result := NewScopedWriteFileTool(root, nil).Run(context.Background(), map[string]any{
		"path": "created.txt", "content": "zero\n",
	})
	if result.Status != StatusOK {
		t.Fatalf("hard-link create = %s: %s", result.Status, result.Output)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "zero\n" {
		t.Fatalf("created file = %q, err=%v", got, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "created.txt" {
		t.Fatalf("root holds %d entries; the staging name must not survive the publish", len(entries))
	}
}

func TestCreateHardLinkFallbackStillRefusesAnExistingName(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "existing.txt")
	if err := os.WriteFile(target, []byte("other writer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	installFileWriteNoReplaceRename(t, func(*os.Root, string, string) error {
		return errors.ErrUnsupported
	})

	workspace, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	if err := commitRootedFileContents(workspace, target, "existing.txt", nil, nil, "zero\n"); !errors.Is(err, os.ErrExist) {
		t.Fatalf("hard-link create over an existing name = %v, want os.ErrExist", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "other writer\n" {
		t.Fatalf("existing file = %q, err=%v; the fallback must not replace it", got, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "existing.txt" {
		t.Fatalf("root holds %d entries; a refused create must remove its staging file", len(entries))
	}
}

// A REPLACEMENT INSTALLED AFTER THE BYTES WERE STAGED must not be silently
// overwritten by the publish. The identity check immediately before the rename
// is what keeps the conflict guarantee for the window that remains.
func TestOverwriteRefusesAReplacementInstalledBeforePublish(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "existing.txt")
	if err := os.WriteFile(target, []byte("observed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	installFileWriteBeforePublish(t, func(path string) {
		preserved := path + ".preserved"
		if err := os.Rename(path, preserved); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("competing\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})

	result := NewScopedWriteFileTool(root, nil).Run(context.Background(), map[string]any{
		"path": "existing.txt", "content": "zero\n", "overwrite": true,
	})
	if result.Status != StatusError || !strings.Contains(result.Output, errFileChangedDuringWrite.Error()) {
		t.Fatalf("pre-publish replacement = %s: %s", result.Status, result.Output)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "competing\n" {
		t.Fatalf("competing file = %q, err=%v; the publish must refuse a replacement it did not observe", got, err)
	}
}
