//go:build !windows

package tui

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestFileView_SpecialTargetNonRegularFileRejected(t *testing.T) {
	resetFileViewCacheForTest()
	dir := t.TempDir()
	pipePath := filepath.Join(dir, "test.pipe")

	// Create a FIFO if supported on the platform
	if err := syscall.Mkfifo(pipePath, 0o600); err != nil {
		t.Skipf("mkfifo not supported on this environment: %v", err)
	}

	res := readFileViewBounded(pipePath, 100, 1000, 10000)
	if res.err == nil {
		t.Fatal("expected error reading FIFO non-regular file, got nil")
	}
	if !strings.Contains(res.err.Error(), "non-regular") {
		t.Fatalf("expected non-regular error message, got: %v", res.err)
	}
}

func TestFileView_FIFONeverBlocksOpen(t *testing.T) {
	resetFileViewCacheForTest()
	dir := t.TempDir()
	pipePath := filepath.Join(dir, "hang_test.pipe")

	if err := syscall.Mkfifo(pipePath, 0o600); err != nil {
		t.Skipf("mkfifo not supported on this environment: %v", err)
	}

	done := make(chan fileViewReadResult, 1)
	go func() {
		res := readFileViewBounded(pipePath, 100, 1000, 10000)
		done <- res
	}()

	select {
	case res := <-done:
		if res.err == nil || !strings.Contains(res.err.Error(), "non-regular") {
			t.Fatalf("expected non-regular error, got: %v", res.err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("readFileViewBounded blocked on open(2) for FIFO without writer (TOCTOU / missing O_NONBLOCK)")
	}
}
