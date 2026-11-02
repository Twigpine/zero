package fsutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "sub", "test.txt")
	content := []byte("hello atomic world")

	if err := WriteFileAtomic(target, content, 0o644); err != nil {
		t.Fatalf("WriteFileAtomic failed: %v", err)
	}

	read, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if string(read) != string(content) {
		t.Fatalf("read content mismatch: got %q, want %q", string(read), string(content))
	}

	// Overwrite test
	newContent := []byte("overwritten atomic content")
	if err := WriteFileAtomic(target, newContent, 0o644); err != nil {
		t.Fatalf("WriteFileAtomic overwrite failed: %v", err)
	}
	read2, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile after overwrite failed: %v", err)
	}
	if string(read2) != string(newContent) {
		t.Fatalf("read content mismatch: got %q, want %q", string(read2), string(newContent))
	}
}

func TestWriteFileAtomicPreservesExistingMode(t *testing.T) {
	dir := t.TempDir()
	cases := []os.FileMode{0o600, 0o755}
	for _, want := range cases {
		target := filepath.Join(dir, "mode-"+want.String()+".txt")
		if err := os.WriteFile(target, []byte("old"), want); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := os.Chmod(target, want); err != nil {
			t.Fatalf("Chmod: %v", err)
		}
		initialInfo, err := os.Stat(target)
		if err != nil {
			t.Fatalf("Stat initial: %v", err)
		}
		expectedMode := initialInfo.Mode().Perm()
		if err := WriteFileAtomic(target, []byte("new"), 0o644); err != nil {
			t.Fatalf("WriteFileAtomic: %v", err)
		}
		info, err := os.Stat(target)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if got := info.Mode().Perm(); got != expectedMode {
			t.Fatalf("mode = %04o, want %04o", got, expectedMode)
		}
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if string(got) != "new" {
			t.Fatalf("content = %q, want %q", got, "new")
		}
	}
}

func TestRenameWithRetryNonRetryableError(t *testing.T) {
	var attempts int
	expectedErr := os.ErrInvalid
	err := RenameWithRetry("src", "dst", func(src, dst string) error {
		attempts++
		return expectedErr
	})
	if !errors.Is(err, expectedErr) {
		t.Fatalf("got err %v, want %v", err, expectedErr)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestWriteFileAtomicRefusesDirectoryBeforeStaging(t *testing.T) {
	previous := privateCreationObserver
	privateCreationObserver = func(string) { t.Fatal("directory validation reached staging creation") }
	defer func() { privateCreationObserver = previous }()
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	marker := filepath.Join(target, "keep.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := WriteFileAtomic(target, []byte("should-not-land"), 0o644); !errors.Is(err, ErrNonRegularDestination) {
		t.Fatalf("expected directory validation failure, got %v", err)
	}

	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("original destination was disturbed: %v", err)
	}
	if string(got) != "keep" {
		t.Fatalf("marker = %q, want keep", got)
	}
	leftovers, err := filepath.Glob(filepath.Join(dir, ".zero-tmp-*"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v", leftovers)
	}
}

func TestWriteFileAtomicRefusesNonWritableTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "readonly.txt")
	if err := os.WriteFile(target, []byte("original"), 0o444); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(target, 0o444); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	probe, err := os.OpenFile(target, os.O_WRONLY, 0)
	if err == nil {
		_ = probe.Close()
		t.Skip("this host allows writing a mode-0444 file (for example when running as root)")
	}

	if err := WriteFileAtomic(target, []byte("replaced"), 0o644); err == nil {
		t.Fatal("expected WriteFileAtomic to refuse a non-writable destination")
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "original" {
		t.Fatalf("content = %q, want %q", got, "original")
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got&0o222 != 0 {
		t.Fatalf("destination became writable: perm=%04o", got)
	}
}

func TestWriteFileAtomicLeavesDestinationOnReplaceFailure(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected replacement failure")
	var owned string
	err := writeFileAtomic(target, []byte("complete replacement"), 0o644, func(src, dst string) error {
		owned = src
		if dst != target {
			t.Fatalf("destination = %q", dst)
		}
		got, err := os.ReadFile(src)
		if err != nil || string(got) != "complete replacement" {
			t.Fatalf("replacement did not receive complete staging: %q, %v", got, err)
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("error = %v, want replacement failure", err)
	}
	if owned == "" {
		t.Fatal("replacement was never reached")
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "original" {
		t.Fatalf("original destination changed: %q, %v", got, err)
	}
	if _, err := os.Lstat(owned); !os.IsNotExist(err) {
		t.Fatalf("owned staging was not cleaned: %s: %v", owned, err)
	}
	leftovers, err := filepath.Glob(filepath.Join(dir, ".zero-tmp-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("staging leftovers: %v, %v", leftovers, err)
	}
}

func TestWriteFileAtomicExclusiveCreatesWithoutReplacing(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "created.txt")

	if err := WriteFileAtomicExclusive(target, []byte("first"), 0o644); err != nil {
		t.Fatalf("exclusive create: %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "first" {
		t.Fatalf("created content = %q, err=%v", got, err)
	}

	if err := WriteFileAtomicExclusive(target, []byte("second"), 0o644); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing-destination error = %v, want os.ErrExist", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "first" {
		t.Fatalf("exclusive create overwrote the existing file: %q, err=%v", got, err)
	}
}

// The no-replace decision must be atomic: a file appearing after the staging
// copy is synced and closed is refused by the publication primitive rather than
// overwritten. This is the window the create branch previously lost.
func TestWriteFileAtomicExclusiveRefusesConcurrentAppearance(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "raced.txt")
	previous := exclusiveBeforePublish
	exclusiveBeforePublish = func(filename string) {
		if err := os.WriteFile(filename, []byte("other writer"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { exclusiveBeforePublish = previous }()

	if err := WriteFileAtomicExclusive(target, []byte("zero"), 0o644); !errors.Is(err, os.ErrExist) {
		t.Fatalf("raced create error = %v, want os.ErrExist", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "other writer" {
		t.Fatalf("raced create overwrote the competitor: %q, err=%v", got, err)
	}
	leftovers, err := filepath.Glob(filepath.Join(dir, ".zero-tmp-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("staging leftovers: %v, %v", leftovers, err)
	}
}

func TestWriteFileAtomicExclusiveSurfacesStagingCleanupFailure(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "created.txt")
	injected := errors.New("injected staging remove failure")
	previous := exclusiveRemoveStaging
	var failedPath string
	exclusiveRemoveStaging = func(path string) error {
		failedPath = path
		return injected
	}
	t.Cleanup(func() {
		exclusiveRemoveStaging = previous
		if failedPath != "" {
			_ = os.Remove(failedPath)
		}
	})

	err := WriteFileAtomicExclusive(target, []byte("published"), 0o644)
	var committed *CommittedReplacementCleanupError
	if !errors.As(err, &committed) || !errors.Is(err, injected) {
		t.Fatalf("error = %v, want CommittedReplacementCleanupError wrapping %v", err, injected)
	}
	if got, readErr := os.ReadFile(target); readErr != nil || string(got) != "published" {
		t.Fatalf("created destination = %q, err=%v", got, readErr)
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
