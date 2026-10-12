package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func installFileWriteRace(t *testing.T, mutate func(string)) {
	t.Helper()
	prior := fileWriteBeforeCommit
	fileWriteBeforeCommit = mutate
	t.Cleanup(func() { fileWriteBeforeCommit = prior })
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
