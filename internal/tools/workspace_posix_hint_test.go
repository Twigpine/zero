package tools

import (
	"context"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPosixRootedPathHint(t *testing.T) {
	cases := []struct {
		name     string
		goos     string
		path     string
		err      error
		wantHint bool
	}{
		{
			name:     "windows posix-rooted miss is explained",
			goos:     "windows",
			path:     "/home/user/repo/main.go",
			err:      fs.ErrNotExist,
			wantHint: true,
		},
		{
			name:     "windows unc-style double slash keeps its raw error",
			goos:     "windows",
			path:     "//server/share/missing.go",
			err:      fs.ErrNotExist,
			wantHint: false,
		},
		{
			name:     "linux posix-rooted miss keeps its raw error",
			goos:     "linux",
			path:     "/home/user/repo/main.go",
			err:      fs.ErrNotExist,
			wantHint: false,
		},
		{
			name:     "windows relative miss keeps its raw error",
			goos:     "windows",
			path:     "src/main.go",
			err:      fs.ErrNotExist,
			wantHint: false,
		},
		{
			name:     "windows posix-rooted permission error keeps its raw error",
			goos:     "windows",
			path:     "/home/user/repo/main.go",
			err:      fs.ErrPermission,
			wantHint: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hint := posixRootedPathHint(tc.goos, tc.path, `C:\ws`, tc.err)
			if !tc.wantHint {
				if hint != "" {
					t.Fatalf("expected no hint, got %q", hint)
				}
				return
			}
			if hint == "" {
				t.Fatal("expected a hint, got none")
			}
			if strings.Contains(hint, tc.path) {
				t.Errorf("hint must not echo the requested path (no-leak contract for out-of-workspace paths): %q", hint)
			}
			if !strings.Contains(hint, `C:\ws`) {
				t.Errorf("hint does not name the workspace root: %q", hint)
			}
			if !strings.Contains(hint, "Windows") {
				t.Errorf("hint does not name the host OS: %q", hint)
			}
		})
	}
}

func TestReadFileToolPosixRootedPathHintOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("hint fires only in Windows path resolution")
	}
	root := t.TempDir()

	result := NewScopedReadFileTool(root, nil).Run(context.Background(), map[string]any{
		"path": "/home/user/project/main.go",
	})

	if result.Status != StatusError {
		t.Fatalf("expected error status, got %s with output %q", result.Status, result.Output)
	}
	for _, want := range []string{
		"/home/user/project/main.go",
		"does not exist on this Windows host",
		root,
		"workspace-relative",
	} {
		if !strings.Contains(result.Output, want) {
			t.Errorf("expected output to mention %q, got %q", want, result.Output)
		}
	}
}

// A workspace that really does keep sources under home/... must keep reading
// them through a POSIX-rooted request: the hint only replaces the message of a
// miss, it must not turn an existing target into an error.
func TestReadFileToolPosixRootedTwinInsideWorkspaceReadsNormally(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("POSIX-rooted requests join onto the root only on Windows")
	}
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "home", "user", "x.go"), "package x\n")

	result := NewScopedReadFileTool(root, nil).Run(context.Background(), map[string]any{
		"path": "/home/user/x.go",
	})

	if result.Status != StatusOK {
		t.Fatalf("expected ok status for an existing workspace twin, got %s with output %q", result.Status, result.Output)
	}
	if !strings.Contains(result.Output, "home/user/x.go") {
		t.Errorf("expected the workspace-relative path in output, got %q", result.Output)
	}
}
