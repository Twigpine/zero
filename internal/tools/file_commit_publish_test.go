package tools

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
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

// An open reader keeps the observed inode. Platforms that cannot replace the
// name while it is open must fail without rewriting that reader's bytes.
func TestOverwriteWithAnOpenReaderPreservesAtomicPublication(t *testing.T) {
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
	want := "zero\n"
	if runtime.GOOS == "windows" && result.Status == StatusError && strings.Contains(result.Output, "atomic replacement refused") {
		want = "observed\n"
	} else if result.Status != StatusOK {
		t.Fatalf("overwrite with an open reader = %s: %s", result.Status, result.Output)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != want {
		t.Fatalf("target = %q, err=%v", got, err)
	}
	if got, err := io.ReadAll(reader); err != nil || string(got) != "observed\n" {
		t.Fatalf("open reader = %q, err=%v; publication must preserve the observed inode", got, err)
	}
}

// A refused atomic replacement must leave both names of the observed inode
// untouched. Exercise both production callers and their session baselines.
func TestOverwriteRefusedReplacementPreservesThePreimageAndHardLink(t *testing.T) {
	for _, name := range []string{"write", "edit"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			root, err := filepath.EvalSymlinks(root)
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, "existing.txt")
			sibling := filepath.Join(root, "sibling.txt")
			preimage := []byte("observed\n")
			if err := os.WriteFile(target, preimage, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(target, sibling); err != nil {
				t.Skipf("hard links unavailable: %v", err)
			}
			info, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			tracker := NewFileTracker()
			tracker.Record(target, preimage, info)
			tracker.RecordSeenRange(target, 1, 1, 1)
			installFileWriteRename(t, func(*os.Root, string, string) error {
				return errors.New("replacement refused")
			})
			tool := NewScopedWriteFileTool(root, nil)
			args := map[string]any{"path": "existing.txt", "content": "zero\n", "overwrite": true}
			if name == "edit" {
				tool = NewScopedEditFileTool(root, nil)
				args = map[string]any{"path": "existing.txt", "old_string": "observed", "new_string": "zero"}
			}
			registry := NewRegistry()
			registry.Register(tool)
			result := registry.RunWithOptions(context.Background(), tool.Name(), args, RunOptions{FileTracker: tracker, PermissionGranted: true})
			if result.Status != StatusError || !strings.Contains(result.Output, "replacement refused") {
				t.Fatalf("refused overwrite = %s: %s", result.Status, result.Output)
			}
			for _, path := range []string{target, sibling} {
				got, err := os.ReadFile(path)
				if err != nil || string(got) != string(preimage) {
					t.Fatalf("%s = %q, err=%v; a refused publish must preserve the shared preimage", path, got, err)
				}
			}
			if err := tracker.CheckConflict(target, preimage); err != nil {
				t.Fatalf("tracker changed after refused publication: %v", err)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 2 {
				t.Fatalf("refused publish left staging files: %v, err=%v", entries, err)
			}
		})
	}
}

// A competing writer that replaces the path during a refused rename keeps its
// own file. No in-place write may follow the failed publication.
func TestOverwriteRefusedRenameLeavesACompetingReplacementUntouched(t *testing.T) {
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
	if result.Status != StatusError || !strings.Contains(result.Output, "replacement refused") {
		t.Fatalf("replaced target = %s: %s", result.Status, result.Output)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "observed\n" {
		t.Fatalf("replacement = %q, err=%v; a refused rename must leave the competing file untouched", got, err)
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

// Publication and cleanup are separate outcomes. Even when cleanup fails, the
// session must know that the new file exists and contains the requested bytes.
func TestCreateCleanupFailureRecordsThePublishedFile(t *testing.T) {
	for _, cleanupErr := range []error{errors.New("injected staging cleanup failure"), os.ErrPermission} {
		t.Run(cleanupErr.Error(), func(t *testing.T) {
			root := t.TempDir()
			root, err := filepath.EvalSymlinks(root)
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, "created.txt")
			var publishedMode os.FileMode
			installFileWriteNoReplaceRename(t, func(root *os.Root, oldName, _ string) error {
				info, err := root.Stat(oldName)
				if err != nil {
					t.Fatal(err)
				}
				publishedMode = info.Mode().Perm()
				return errors.ErrUnsupported
			})
			prior := fileWriteRemove
			fileWriteRemove = func(*os.Root, string) error { return cleanupErr }
			t.Cleanup(func() { fileWriteRemove = prior })

			tracker := NewFileTracker()
			registry := NewRegistry()
			registry.Register(NewScopedWriteFileTool(root, nil))
			result := registry.RunWithOptions(context.Background(), "write_file", map[string]any{
				"path": "created.txt", "content": "zero\n",
			}, RunOptions{FileTracker: tracker, PermissionGranted: true})
			if result.Status != StatusError || !strings.Contains(result.Output, "published") || !strings.Contains(result.Output, cleanupErr.Error()) {
				t.Fatalf("published create with cleanup failure = %s: %s", result.Status, result.Output)
			}
			if got, err := os.ReadFile(target); err != nil || string(got) != "zero\n" {
				t.Fatalf("published content = %q, err=%v", got, err)
			}
			if info, err := os.Stat(target); err != nil || info.Mode().Perm() != publishedMode {
				t.Fatalf("published mode changed after cleanup failure: %v, err=%v, want %v", info, err, publishedMode)
			}
			if version, ok := tracker.Version(target); !ok || version.Hash != HashContent([]byte("zero\n")) {
				t.Fatalf("published file missing from tracker: %+v, tracked=%v", version, ok)
			}
			if !tracker.SeenWhole(target) {
				t.Fatal("published bytes must be recorded as seen")
			}
			if created := tracker.CreatedFiles(); len(created) != 1 || created[0] != target {
				t.Fatalf("created files = %v, want %s", created, target)
			}
			if len(result.ChangedFiles) != 1 || result.ChangedFiles[0] != "created.txt" {
				t.Fatalf("changed files = %v", result.ChangedFiles)
			}
		})
	}
}
