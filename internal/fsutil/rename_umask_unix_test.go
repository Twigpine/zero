//go:build !windows

package fsutil

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWriteFileAtomicRespectsProcessUmask(t *testing.T) {
	// Temporarily set umask to 0o077
	oldMask := syscall.Umask(0o077)
	defer syscall.Umask(oldMask)

	dir := t.TempDir()
	target := filepath.Join(dir, "umask_test.txt")

	// Write new file with 0o644 requested perm
	if err := WriteFileAtomic(target, []byte("umask test"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	// Under umask 0o077, 0o644 & ~0o077 = 0o600
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("created file perm = %04o, want %04o (honoring umask 0o077)", got, 0o600)
	}
}

func TestWriteFileAtomicStagesReplacementWithDestinationMode(t *testing.T) {
	oldMask := syscall.Umask(0)
	defer syscall.Umask(oldMask)

	dir := t.TempDir()
	target := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	orig := stagingModeObserver
	t.Cleanup(func() { stagingModeObserver = orig })
	var observed os.FileMode
	stagingModeObserver = func(mode os.FileMode) { observed = mode }

	if err := WriteFileAtomic(target, []byte("new"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	if got := observed.Perm(); got != 0o600 {
		t.Fatalf("staging mode = %04o, want the destination mode %04o", got, 0o600)
	}
}
