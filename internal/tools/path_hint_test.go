package tools

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLooksLikePosixAbsolutePath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/home/user/repo/notes.txt", true},
		{"/tmp/notes.txt", true},
		{"/", true},
		{"/home/../etc/passwd", true},
		{"//server/share/notes.txt", false},
		{"///x", false},
		{`/\server\share\notes.txt`, false},
		{`/\?\C:\notes.txt`, false},
		{`C:/Users/me/notes.txt`, false},
		{`C:\Users\me\notes.txt`, false},
		{`\home\user\notes.txt`, false},
		{"home/user/notes.txt", false},
		{"", false},
	}
	for _, test := range tests {
		if got := looksLikePosixAbsolutePath(test.path); got != test.want {
			t.Errorf("looksLikePosixAbsolutePath(%q) = %v, want %v", test.path, got, test.want)
		}
	}
}

func TestAnnotateMissingPosixPathError(t *testing.T) {
	const root = `C:\Users\me\repo`
	missing := &os.PathError{Op: "lstat", Path: `C:\Users\me\repo\home`, Err: fs.ErrNotExist}

	tests := []struct {
		name        string
		goos        string
		requested   string
		err         error
		wantSameErr bool
	}{
		{
			name:        "windows posix miss gets the hint",
			goos:        "windows",
			requested:   "/home/user/repo/notes.txt",
			err:         missing,
			wantSameErr: false,
		},
		{
			name:        "windows root-like miss gets the hint",
			goos:        "windows",
			requested:   "/tmp/notes.txt",
			err:         missing,
			wantSameErr: false,
		},
		{
			name:        "linux leaves the error unchanged",
			goos:        "linux",
			requested:   "/home/user/repo/notes.txt",
			err:         missing,
			wantSameErr: true,
		},
		{
			name:        "darwin leaves the error unchanged",
			goos:        "darwin",
			requested:   "/home/user/repo/notes.txt",
			err:         missing,
			wantSameErr: true,
		},
		{
			name:        "windows compact path is not a posix path",
			goos:        "windows",
			requested:   `C:\Users\me\repo\notes.txt`,
			err:         missing,
			wantSameErr: true,
		},
		{
			name:        "windows unc path is not a posix path",
			goos:        "windows",
			requested:   `//server/share/notes.txt`,
			err:         missing,
			wantSameErr: true,
		},
		{
			name:        "windows mixed-separator unc path is not a posix path",
			goos:        "windows",
			requested:   `/\server\share/notes.txt`,
			err:         missing,
			wantSameErr: true,
		},
		{
			name:        "windows device path is not a posix path",
			goos:        "windows",
			requested:   `/\?\C:\notes.txt`,
			err:         missing,
			wantSameErr: true,
		},
		{
			name:        "windows relative path is not a posix path",
			goos:        "windows",
			requested:   "notes.txt",
			err:         missing,
			wantSameErr: true,
		},
		{
			name:        "windows non-missing error is not hinted",
			goos:        "windows",
			requested:   "/home/user/repo/notes.txt",
			err:         &os.PathError{Op: "lstat", Path: "/home", Err: fs.ErrPermission},
			wantSameErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := annotateMissingPosixPathError(test.goos, root, test.requested, test.err)
			if test.wantSameErr {
				if got != test.err {
					t.Fatalf("expected the original error to pass through, got %v", got)
				}
				return
			}
			if got == test.err {
				t.Fatal("expected a wrapped error, got the original")
			}
			message := got.Error()
			for _, want := range []string{
				test.err.Error(),
				"[zero] path hint:",
				"Windows",
				root,
				"workspace-relative",
			} {
				if !strings.Contains(message, want) {
					t.Fatalf("expected %q in %q", want, message)
				}
			}
			if !errors.Is(got, fs.ErrNotExist) {
				t.Fatalf("expected wrapped error to keep fs.ErrNotExist, got %v", got)
			}
		})
	}

	if got := annotateMissingPosixPathError("windows", root, "/home/x", nil); got != nil {
		t.Fatalf("expected nil error to pass through, got %v", got)
	}
}

// posixWiringRequest builds a POSIX-shaped request that resolves only against
// the test-owned tree under root on the host running the test. On Windows a
// leading-slash request is not absolute and is joined under root, which is the
// production behavior under test. On Unix it is already absolute, so root is
// prepended explicitly to keep the test hermetic; a fixed "/home/..." pathname
// would read host state the test does not own.
func posixWiringRequest(root string, relative string) string {
	if runtime.GOOS == "windows" {
		return "/" + relative
	}
	return filepath.ToSlash(root) + "/" + relative
}

func TestResolveScopedReadPathForGOOSAnnotatesWindowsPosixMiss(t *testing.T) {
	root := t.TempDir()
	const existingRelative = "home/zero-hint-real/real.txt"
	writeTestFile(t, filepath.Join(root, filepath.FromSlash(existingRelative)), "real")

	// The resolved relative path proves the resolution stayed inside the
	// test-owned tree rather than matching the hint text only.
	_, relative, err := resolveScopedReadPathForGOOS("windows", root, nil, posixWiringRequest(root, existingRelative))
	if err != nil {
		t.Fatalf("expected the POSIX-shaped workspace path to resolve, got %v", err)
	}
	if relative != existingRelative {
		t.Fatalf("resolved relative path = %q, want %q", relative, existingRelative)
	}

	const missingRelative = "home/zero-hint-missing/missing.txt"
	_, _, err = resolveScopedReadPathForGOOS("windows", root, nil, posixWiringRequest(root, missingRelative))
	if err == nil {
		t.Fatal("expected a resolution error for a missing path")
	}
	message := err.Error()
	for _, want := range []string{"[zero] path hint:", "Windows", root} {
		if !strings.Contains(message, want) {
			t.Fatalf("expected %q in %q", want, message)
		}
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected missing-path error to keep fs.ErrNotExist, got %v", err)
	}

	if _, _, err := resolveScopedReadPathForGOOS("linux", root, nil, posixWiringRequest(root, missingRelative)); err == nil || strings.Contains(err.Error(), "[zero] path hint:") {
		t.Fatalf("linux must not get the windows hint, got %v", err)
	}

	if _, _, err := resolveScopedReadPathForGOOS("windows", root, nil, "missing.txt"); err == nil || strings.Contains(err.Error(), "[zero] path hint:") {
		t.Fatalf("a relative path must not get the posix hint, got %v", err)
	}
}

// TestReadFileWindowsPosixPathHint is the end-to-end check on the platform the
// hint exists for: read_file must surface the guidance when a POSIX path is
// joined under the workspace root and misses.
func TestReadFileWindowsPosixPathHint(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skipf("windows-only path resolution; host is %s", runtime.GOOS)
	}
	root := t.TempDir()

	result := NewReadFileTool(root).Run(context.Background(), map[string]any{
		"path": "/home/zero-hint-missing/missing.txt",
	})

	if result.Status != StatusError {
		t.Fatalf("expected an error result, got %s: %s", result.Status, result.Output)
	}
	if !strings.Contains(result.Output, "[zero] path hint:") || !strings.Contains(result.Output, "Windows") {
		t.Fatalf("expected the windows path hint, got %q", result.Output)
	}
}
