//go:build !windows

package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWriteFileAtomicRefusesNamedPipe(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "endpoint.fifo")
	if err := syscall.Mkfifo(target, 0o644); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	err := WriteFileAtomic(target, []byte("should-not-replace-fifo"), 0o644)
	if err == nil {
		t.Fatal("expected WriteFileAtomic to refuse a FIFO destination")
	}
	if !errors.Is(err, ErrNonRegularDestination) {
		t.Fatalf("error = %v, want ErrNonRegularDestination", err)
	}

	info, err := os.Lstat(target)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("FIFO was replaced; mode = %s", info.Mode())
	}
	leftovers, err := filepath.Glob(filepath.Join(dir, ".zero-tmp-*"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v", leftovers)
	}
}
