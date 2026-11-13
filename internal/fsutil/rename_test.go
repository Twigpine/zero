package fsutil

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"testing"
)

func TestRenameWithRetryRetriesOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("RenameWithRetry only retries on Windows")
	}
	var attempts int
	err := RenameWithRetry("src", "dst", func(src, dst string) error {
		attempts++
		switch attempts {
		case 1:
			return syscall.Errno(32) // ERROR_SHARING_VIOLATION
		case 2:
			return syscall.Errno(33) // ERROR_LOCK_VIOLATION
		default:
			return nil
		}
	})
	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if attempts != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts)
	}
}

func TestRenameWithRetryNonRetryableError(t *testing.T) {
	sentinel := errors.New("disk on fire")
	var attempts int
	err := RenameWithRetry("src", "dst", func(src, dst string) error {
		attempts++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got: %v", err)
	}
	if attempts != 1 {
		t.Errorf("expected only 1 attempt for non-retryable error, got %d", attempts)
	}
}

// Errno 32 and 33 are EPIPE and EDOM on Unix, so only Windows may classify them.
func TestIsSharingOrLockViolation(t *testing.T) {
	onWindows := runtime.GOOS == "windows"
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{syscall.Errno(32), onWindows},
		{fmt.Errorf("wrapped: %w", syscall.Errno(33)), onWindows},
		{syscall.Errno(5), false}, // ERROR_ACCESS_DENIED
		{errors.New("other"), false},
		{nil, false},
	} {
		if got := IsSharingOrLockViolation(tc.err); got != tc.want {
			t.Errorf("IsSharingOrLockViolation(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
