package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func installFileWriteTempWrite(t *testing.T, write func(*os.File, string) error) {
	t.Helper()
	prior := fileWriteTempWrite
	fileWriteTempWrite = write
	t.Cleanup(func() { fileWriteTempWrite = prior })
}

// A WRITE THAT CANNOT COMPLETE MUST NOT TAKE THE FILE IT REPLACED WITH IT.
//
// Running out of disk space part-way through a write used to be fatal to the
// preimage: the overwrite truncated the observed file first, so the failed
// write left an empty husk where a working file stood. The replacement bytes
// are now written whole to a side file, so a failure there costs the side file
// and the observed bytes survive untouched. Each case reopens the target after
// the tool has run and checks the preimage is what still stands, and that no
// side file was left behind.
func TestOverwriteFailureKeepsThePreimageStanding(t *testing.T) {
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
			// The write that stands in for ENOSPC fails after the file was
			// observed, accepted and staged — the exact point the old
			// truncate-then-write had nothing left to lose.
			writeErr := errors.New("injected unrecoverable write failure")
			installFileWriteTempWrite(t, func(*os.File, string) error { return writeErr })

			result := run(root)
			if result.Status != StatusError {
				t.Fatalf("failed-write overwrite = %s, want error: %s", result.Status, result.Output)
			}
			got, err := os.ReadFile(target)
			if err != nil || string(got) != "observed\n" {
				t.Fatalf("preimage = %q, err=%v; the observed file must outlive a write that failed", got, err)
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != "existing.txt" {
				t.Fatalf("root holds %d entries; a failed write must remove its side file", len(entries))
			}
		})
	}
}

// A NEW FILE WHOSE WRITE FAILS MUST NOT APPEAR AT ALL. An exclusive create used
// to leave whatever the write had managed to write behind, half-written and
// complete-looking to anything that did not read past the visible length.
func TestCreateFailureCreatesNothing(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "created.txt")
	writeErr := errors.New("injected unrecoverable write failure")
	installFileWriteTempWrite(t, func(*os.File, string) error { return writeErr })

	result := NewScopedWriteFileTool(root, nil).Run(context.Background(), map[string]any{
		"path": "created.txt", "content": "zero\n",
	})
	if result.Status != StatusError {
		t.Fatalf("failed-write create = %s, want error: %s", result.Status, result.Output)
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("target = %v; a create whose bytes never made it must leave no file", statErr)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("root holds %d entries; a failed create must remove its side file", len(entries))
	}
}

// AT THE LAYER THE COMMIT RUNS, the injected failure is reported as the
// failure it injected and nothing else, so a caller can distinguish a failed
// write from a refused write.
func TestCommitRootedOverwriteFailedWriteIsTheWriteError(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "existing.txt")
	if err := os.WriteFile(target, []byte("observed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	preimage := "observed\n"
	writeErr := errors.New("injected unrecoverable write failure")
	installFileWriteTempWrite(t, func(*os.File, string) error { return writeErr })

	err = commitRootedFileContents(workspace, filepath.Join(root, "existing.txt"), "existing.txt", info, &preimage, "zero\n")
	if !errors.Is(err, writeErr) {
		t.Fatalf("commit error = %v, want the injected write failure", err)
	}
	got, readErr := os.ReadFile(target)
	if readErr != nil || string(got) != "observed\n" {
		t.Fatalf("preimage = %q, err=%v; the observed file must outlive a write that failed", got, readErr)
	}
}
