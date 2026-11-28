//go:build unix

package peermsg

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestEnsurePrivateDirRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateDir(link); err == nil {
		t.Fatal("expected symlink runtime directory to be rejected")
	}
}

func TestEnsurePrivateDirRejectsSymlinkedParent(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateDir(filepath.Join(link, "peers")); err == nil {
		t.Fatal("expected symlinked parent to be rejected")
	}
}

// THE DESCRIPTOR THE WALK ENDS ON HAS TO BE CLOSED TOO.
//
// EnsurePrivateDir walks the path one component at a time, closing each parent
// as it opens the child. A deferred close that captured the first descriptor
// closed a number this function no longer owned and left the last one open, so
// every fallback-runtime preparation leaked a descriptor. The lowest free
// descriptor is the watermark: dup(0) hands it back, and a leak per cycle walks
// it upward. Several depths, because the kernel reuses numbers and a single
// shallow fixture can hide the leak by accident.
func TestEnsurePrivateDirClosesTheDescriptorItOwns(t *testing.T) {
	lowestFree := func() int {
		fd, err := unix.Dup(0)
		if err != nil {
			t.Fatalf("dup: %v", err)
		}
		unix.Close(fd)
		return fd
	}
	// The physical path: on macOS t.TempDir() sits under /var, which is itself
	// a symlink to /private/var, and this walk refuses a symlink component by
	// design. Production resolves its temp root the same way before calling.
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	for _, depth := range []int{1, 3, 6} {
		parts := []string{base}
		for i := 0; i < depth; i++ {
			parts = append(parts, fmt.Sprintf("level%d", i))
		}
		target := filepath.Join(parts...)

		before := lowestFree()
		for cycle := 0; cycle < 40; cycle++ {
			if err := EnsurePrivateDir(target); err != nil {
				t.Fatalf("depth %d cycle %d: %v", depth, cycle, err)
			}
		}
		if after := lowestFree(); after != before {
			t.Fatalf("depth %d: lowest free descriptor moved from %d to %d over 40 successful walks, so each one leaks", depth, before, after)
		}

		// And the failing walk: a symlink component is refused partway down.
		linked := filepath.Join(base, fmt.Sprintf("linked%d", depth))
		if err := os.Symlink(target, linked); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		failing := filepath.Join(linked, "below")
		before = lowestFree()
		for cycle := 0; cycle < 40; cycle++ {
			if err := EnsurePrivateDir(failing); err == nil {
				t.Fatalf("depth %d: a symlinked component was not refused", depth)
			}
		}
		if after := lowestFree(); after != before {
			t.Fatalf("depth %d: lowest free descriptor moved from %d to %d over 40 refused walks", depth, before, after)
		}
	}
}
