package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestLockSingleInstance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.lock")
	alive := func(int) bool { return true }

	l1, err := acquireLock(path, alive)
	if err != nil {
		t.Fatalf("first acquireLock: %v", err)
	}
	// Second acquire while the holder is "alive" must be refused.
	if _, err := acquireLock(path, alive); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second acquireLock err = %v, want ErrAlreadyRunning", err)
	}
	// After release, a new acquire succeeds.
	if err := l1.release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	l2, err := acquireLock(path, alive)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	_ = l2.release()
}

// A PID liveness probe is diagnostic only. Even if it reports the recorded PID
// dead, a contended kernel lock proves that the holder is active and must win.
// The old rename-aside protocol trusted this stale observation and admitted a
// second daemon while the first one was still inside its critical section.
func TestLockDoesNotOverrideKernelHolderWithStalePIDProbe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.lock")
	first, err := acquireLock(path, func(int) bool { return true })
	if err != nil {
		t.Fatalf("first acquireLock: %v", err)
	}
	defer first.release()

	second, err := acquireLock(path, func(int) bool { return false })
	if second != nil {
		_ = second.release()
	}
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("contended acquire with stale PID probe = %v, want ErrAlreadyRunning", err)
	}
}

func TestLockIgnoresStaleMetadataWhenKernelLockIsFree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.lock")
	// A crashed daemon may leave PID metadata behind, but its kernel lock is
	// released automatically, so the next daemon can acquire without moving it.
	if err := os.WriteFile(path, []byte("4242\n"), 0o600); err != nil {
		t.Fatalf("write stale lock: %v", err)
	}
	dead := func(int) bool { return false }
	l, err := acquireLock(path, dead)
	if err != nil {
		t.Fatalf("acquire over stale metadata: %v", err)
	}
	// The lock now records OUR pid, not the stale one.
	data, _ := os.ReadFile(path)
	if strings.TrimSpace(string(data)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("lock pid = %q, want %d", strings.TrimSpace(string(data)), os.Getpid())
	}
	_ = l.release()
}

func TestLockReleaseKeepsStableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.lock")
	l, err := acquireLock(path, func(int) bool { return true })
	if err != nil {
		t.Fatalf("acquireLock: %v", err)
	}
	if err := l.release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("release removed stable lock file: %v", err)
	}
}

func TestReadPidFileMetadataFormats(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.lock")
	for _, test := range []struct {
		name     string
		metadata string
		wantPID  int
		wantErr  bool
	}{
		{name: "legacy PID", metadata: "4242\n", wantPID: 4242},
		{name: "PID and holder sequence", metadata: "4242-7\n", wantPID: 4242},
		{name: "missing sequence", metadata: "4242-", wantErr: true},
		{name: "non-numeric sequence", metadata: "4242-next", wantErr: true},
		{name: "extra component", metadata: "4242-7-8", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(test.metadata), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := readPidFile(path)
			if (err != nil) != test.wantErr {
				t.Fatalf("readPidFile() error = %v, wantErr %v", err, test.wantErr)
			}
			if !test.wantErr && got != test.wantPID {
				t.Fatalf("readPidFile() = %d, want %d", got, test.wantPID)
			}
		})
	}
}

func TestProcessAliveSelfAndDead(t *testing.T) {
	if !osProcessAlive(os.Getpid()) {
		t.Fatal("osProcessAlive(self) = false, want true")
	}
	// PID 0 / negative are never live.
	if osProcessAlive(0) || osProcessAlive(-1) {
		t.Fatal("osProcessAlive must reject non-positive pids")
	}
}
