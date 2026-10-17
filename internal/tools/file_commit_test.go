package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func installFileWriteRace(t *testing.T, mutate func(string)) {
	t.Helper()
	prior := fileWriteBeforeCommit
	fileWriteBeforeCommit = mutate
	t.Cleanup(func() { fileWriteBeforeCommit = prior })
}

func installWriteRootBeforeOpen(t *testing.T, hook func(string)) {
	t.Helper()
	prior := writeRootBeforeOpen
	writeRootBeforeOpen = hook
	t.Cleanup(func() { writeRootBeforeOpen = prior })
}

// A granted root swapped for a symlink between the pre-open identity stat and
// os.OpenRoot must be refused: os.OpenRoot re-resolves the path, so without the
// identity comparison the returned descriptor would be bound to the external
// directory and every later mutation would escape the validated boundary. The
// hook reproduces the exact check-to-use window deterministically.
func TestOpenScopedWriteRootRejectsSubstitutedRoot(t *testing.T) {
	t.Run("accepts stable root", func(t *testing.T) {
		root := t.TempDir()
		resolvedRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			t.Fatal(err)
		}
		handle, relative, err := openScopedWriteRoot(root, nil, filepath.Join(resolvedRoot, "created.txt"))
		if err != nil {
			t.Fatalf("stable root rejected: %v", err)
		}
		defer handle.Close()
		if relative != "created.txt" {
			t.Fatalf("relative = %q, want created.txt", relative)
		}
	})

	t.Run("rejects substituted root", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		resolvedRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			t.Fatal(err)
		}
		originalRoot := resolvedRoot + "-original"
		t.Cleanup(func() { _ = os.RemoveAll(originalRoot) })
		installWriteRootBeforeOpen(t, func(path string) {
			if path != resolvedRoot {
				return
			}
			if err := os.Rename(resolvedRoot, originalRoot); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, resolvedRoot); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
		})

		handle, _, err := openScopedWriteRoot(root, nil, filepath.Join(resolvedRoot, "created.txt"))
		if err == nil {
			_ = handle.Close()
			t.Fatal("openScopedWriteRoot accepted a root substituted between stat and open")
		}
		if !errors.Is(err, errWriteRootSubstituted) {
			t.Fatalf("error = %v, want errWriteRootSubstituted", err)
		}
	})
}

func installWritePreimageBeforeObserve(t *testing.T, hook func(string)) {
	t.Helper()
	prior := writePreimageBeforeObserve
	writePreimageBeforeObserve = hook
	t.Cleanup(func() { writePreimageBeforeObserve = prior })
}

func installFileWriteStat(t *testing.T, stat func(*os.File) (os.FileInfo, error)) {
	t.Helper()
	prior := fileWriteStat
	fileWriteStat = stat
	t.Cleanup(func() { fileWriteStat = prior })
}

func TestWriteFileRefusesCreateAndOverwriteRaces(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "created.txt")
		installFileWriteRace(t, func(path string) {
			if err := os.WriteFile(path, []byte("other writer\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		})
		result := NewScopedWriteFileTool(root, nil).Run(context.Background(), map[string]any{
			"path": "created.txt", "content": "zero\n",
		})
		if result.Status != StatusError {
			t.Fatalf("raced create status = %s, want error", result.Status)
		}
		if got, err := os.ReadFile(target); err != nil || string(got) != "other writer\n" {
			t.Fatalf("raced create content = %q, err=%v", got, err)
		}
	})

	t.Run("overwrite", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "existing.txt")
		if err := os.WriteFile(target, []byte("observed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		installFileWriteRace(t, func(path string) {
			if err := os.WriteFile(path, []byte("other writer\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		})
		result := NewScopedWriteFileTool(root, nil).Run(context.Background(), map[string]any{
			"path": "existing.txt", "content": "zero\n", "overwrite": true,
		})
		if result.Status != StatusError || !strings.Contains(result.Output, errFileChangedDuringWrite.Error()) {
			t.Fatalf("raced overwrite = %s: %s", result.Status, result.Output)
		}
		if got, err := os.ReadFile(target); err != nil || string(got) != "other writer\n" {
			t.Fatalf("raced overwrite content = %q, err=%v", got, err)
		}
	})
}

func TestEditFileRefusesPreimageRace(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "existing.txt")
	if err := os.WriteFile(target, []byte("observed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	installFileWriteRace(t, func(path string) {
		if err := os.WriteFile(path, []byte("other writer\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	result := NewScopedEditFileTool(root, nil).Run(context.Background(), map[string]any{
		"path": "existing.txt", "old_string": "observed", "new_string": "zero",
	})
	if result.Status != StatusError || !strings.Contains(result.Output, errFileChangedDuringWrite.Error()) {
		t.Fatalf("raced edit = %s: %s", result.Status, result.Output)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "other writer\n" {
		t.Fatalf("raced edit content = %q, err=%v", got, err)
	}
}

// A parent directory swapped for a symlink that escapes the workspace between
// validation and the commit must be refused. The create is opened relative to
// the granted root handle, so the kernel never follows the swapped link. This
// is the regression that fails on the pre-migration code: there the unanchored
// os.OpenFile(path, O_CREATE|O_EXCL) followed the symlink and created the file
// outside the workspace.
func TestWriteFileRefusesParentSymlinkSwapBeforeCreate(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	installFileWriteRace(t, func(string) {
		if err := os.Rename(sub, filepath.Join(root, "sub-original")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, sub); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	})

	result := NewScopedWriteFileTool(root, nil).Run(context.Background(), map[string]any{
		"path": "sub/created.txt", "content": "zero\n",
	})
	if result.Status == StatusOK {
		t.Fatalf("create through a swapped parent symlink must fail, got OK: %s", result.Output)
	}
	if _, err := os.Stat(filepath.Join(outside, "created.txt")); err == nil {
		t.Fatal("create escaped the workspace through the swapped parent symlink")
	}
}

func TestOverwriteDoesNotStatOpenedFileAfterFinalPreimageComparison(t *testing.T) {
	for name, run := range map[string]func(string) Result{
		"write overwrite": func(root string) Result {
			return NewScopedWriteFileTool(root, nil).Run(context.Background(), map[string]any{
				"path": "existing.txt", "content": "zero\n", "overwrite": true,
			})
		},
		"edit": func(root string) Result {
			return NewScopedEditFileTool(root, nil).Run(context.Background(), map[string]any{
				"path": "existing.txt", "old_string": "observed", "new_string": "zero",
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "existing.txt")
			if err := os.WriteFile(target, []byte("observed\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			statCalls := 0
			installFileWriteStat(t, func(file *os.File) (os.FileInfo, error) {
				statCalls++
				if statCalls == 2 {
					// This preserves the inode, so identity-only checks cannot detect it.
					if err := os.WriteFile(target, []byte("other writer\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				return file.Stat()
			})

			result := run(root)
			if result.Status != StatusOK {
				t.Fatalf("overwrite status = %s: %s", result.Status, result.Output)
			}
			if statCalls != 1 {
				t.Fatalf("opened file was statted %d times; the final byte comparison must be followed directly by mutation", statCalls)
			}
		})
	}
}

// A pathname re-resolution after the write root is open must not supply the
// preimage. The root handle still names the directory that was opened; a
// same-name directory, or a symlink planted at the old path, is a different
// object. Observing that object through the absolute path makes the later
// identity check disagree with the rooted open and refuse a write the rooted
// preimage would have committed. The commit must land in the rooted object and
// leave the pathname substitute untouched.
func TestWriteFilePreimageIgnoresDissentingPathname(t *testing.T) {
	t.Run("same-name directory", func(t *testing.T) {
		assertRootedPreimageNotPathnameSubstitute(t, false)
	})
	t.Run("symlink permutation", func(t *testing.T) {
		assertRootedPreimageNotPathnameSubstitute(t, true)
	})
}

func assertRootedPreimageNotPathnameSubstitute(t *testing.T, symlink bool) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows kernel locks open directory handles held by *os.Root, preventing directory swap")
	}
	root := t.TempDir()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resolved, "existing.txt"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	moved := resolved + "-moved"
	decoyHold := resolved + "-decoy"
	t.Cleanup(func() {
		_ = os.RemoveAll(moved)
		_ = os.RemoveAll(decoyHold)
	})

	installWritePreimageBeforeObserve(t, func(absolutePath string) {
		if filepath.Base(absolutePath) != "existing.txt" {
			return
		}
		dir := filepath.Dir(absolutePath)
		if err := os.Rename(dir, moved); err != nil {
			t.Fatal(err)
		}
		if symlink {
			if err := os.Mkdir(decoyHold, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(decoyHold, "existing.txt"), []byte("decoy\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(decoyHold, dir); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			return
		}
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "existing.txt"), []byte("decoy\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})

	result := NewScopedWriteFileTool(resolved, nil).Run(context.Background(), map[string]any{
		"path": "existing.txt", "content": "zero\n", "overwrite": true,
	})
	if result.Status != StatusOK {
		t.Fatalf("rooted overwrite status = %s: %s", result.Status, result.Output)
	}
	movedBytes, err := os.ReadFile(filepath.Join(moved, "existing.txt"))
	if err != nil || string(movedBytes) != "zero\n" {
		t.Fatalf("rooted object = %q, err=%v; a dissenting pathname must not redirect the commit", movedBytes, err)
	}
	decoyPath := filepath.Join(resolved, "existing.txt")
	if symlink {
		decoyPath = filepath.Join(decoyHold, "existing.txt")
	}
	decoyBytes, err := os.ReadFile(decoyPath)
	if err != nil || string(decoyBytes) != "decoy\n" {
		t.Fatalf("pathname substitute = %q, err=%v", decoyBytes, err)
	}
}

// Replacing the destination with a different file that carries the same bytes,
// after the preimage and before the commit, must not be treated as the observed
// object. On Windows, a FileInfo from os.Stat fills volume and file index only
// when os.SameFile re-opens the path, so the byte comparison is the only check
// left and it cannot see this substitution. The rooted preimage captures
// identity at observation time, and the write must refuse.
func TestWriteFileRefusesSameNameSubstitutionWithIdenticalBytes(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "existing.txt")
	const observed = "original\n"
	if err := os.WriteFile(target, []byte(observed), 0o644); err != nil {
		t.Fatal(err)
	}
	installFileWriteRace(t, func(path string) {
		preserved := path + ".preserved"
		if err := os.Rename(path, preserved); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(observed), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	result := NewScopedWriteFileTool(root, nil).Run(context.Background(), map[string]any{
		"path": "existing.txt", "content": "REPLACED BY THE TOOL\n", "overwrite": true,
	})
	if result.Status != StatusError || !strings.Contains(result.Output, errFileChangedDuringWrite.Error()) {
		t.Fatalf("same-name substitution = %s: %s", result.Status, result.Output)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != observed {
		t.Fatalf("substituted object = %q, err=%v; the write must not land in an object the preimage did not observe", got, err)
	}
}
