package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func installFileWriteSecurity(t *testing.T, capture func(*os.File) (*fileSecurity, error), apply func(*os.File, *fileSecurity) error) {
	t.Helper()
	priorCapture, priorApply := fileWriteCaptureSecurity, fileWriteApplySecurity
	if capture != nil {
		fileWriteCaptureSecurity = capture
	}
	if apply != nil {
		fileWriteApplySecurity = apply
	}
	t.Cleanup(func() { fileWriteCaptureSecurity, fileWriteApplySecurity = priorCapture, priorApply })
}

// AN OVERWRITE THAT CANNOT CARRY THE FILE'S ACCESS CONTROL DOES NOT HAPPEN.
//
// The overwrite replaces the file with a freshly created one, and a fresh file
// has whatever the directory gives it. If the original's access control cannot
// be read, or cannot be put on the staged file, publishing anyway would quietly
// widen who can read a file its owner had locked down. Both failures therefore
// refuse the write, leave the observed bytes standing, and remove the side
// file. These run on every platform: the seams are the same everywhere, and
// only what the production functions do behind them differs.
func TestOverwriteRefusesWhenAccessControlCannotBeCarried(t *testing.T) {
	failure := errors.New("injected access control failure")
	for _, testCase := range []struct {
		name    string
		capture func(*os.File) (*fileSecurity, error)
		apply   func(*os.File, *fileSecurity) error
	}{
		{name: "reading it fails", capture: func(*os.File) (*fileSecurity, error) { return nil, failure }},
		{name: "applying it fails", apply: func(*os.File, *fileSecurity) error { return failure }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
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
			installFileWriteSecurity(t, testCase.capture, testCase.apply)

			err = commitRootedFileContents(workspace, target, "existing.txt", info, &preimage, "replacement\n")
			if !errors.Is(err, failure) {
				t.Fatalf("commit error = %v, want the injected access control failure", err)
			}
			got, readErr := os.ReadFile(target)
			if readErr != nil || string(got) != "observed\n" {
				t.Fatalf("file = %q, err=%v; the observed bytes must outlive a refused overwrite", got, readErr)
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				names := make([]string, 0, len(entries))
				for _, entry := range entries {
					names = append(names, entry.Name())
				}
				t.Fatalf("directory holds %s; a refused overwrite must remove its side file", strings.Join(names, ", "))
			}
		})
	}
}

// THE CARRY IS FOR THE FILE THAT IS REPLACED, AND ONLY THAT ONE.
//
// A create has no original to take access control from and keeps what the
// directory gives a new file, so it must hand the side file nothing. An
// overwrite hands it exactly what was read from the observed file. The
// assertion on the overwrite also proves the seam was reached, so the create
// half cannot pass by the seam never being called.
func TestOnlyAnOverwriteCarriesAccessControl(t *testing.T) {
	root := t.TempDir()
	read := &fileSecurity{}
	var applied []*fileSecurity
	installFileWriteSecurity(t,
		func(*os.File) (*fileSecurity, error) { return read, nil },
		func(_ *os.File, security *fileSecurity) error {
			applied = append(applied, security)
			return nil
		},
	)
	tool := NewScopedWriteFileTool(root, nil)

	created := tool.Run(context.Background(), map[string]any{"path": "note.txt", "content": "first\n"})
	if created.Status != StatusOK {
		t.Fatalf("create = %s: %s", created.Status, created.Output)
	}
	if len(applied) != 1 || applied[0] != nil {
		t.Fatalf("a create handed its side file %v; it has no original to carry access control from", applied)
	}

	overwrote := tool.Run(context.Background(), map[string]any{"path": "note.txt", "content": "second\n", "overwrite": true})
	if overwrote.Status != StatusOK {
		t.Fatalf("overwrite = %s: %s", overwrote.Status, overwrote.Output)
	}
	if len(applied) != 2 || applied[1] != read {
		t.Fatalf("an overwrite handed its side file %v, want what was read from the file it replaces", applied)
	}
}
