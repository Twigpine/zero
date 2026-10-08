package lockutil

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestFileLockSerializesAndKeepsStablePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	first, err := TryAcquireFileLock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	firstInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat first lock path: %v", err)
	}
	if _, err := TryAcquireFileLock(path); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("contended acquire = %v, want ErrLockHeld", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("release first: %v", err)
	}
	releasedInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("release removed stable lock path: %v", err)
	}
	if !os.SameFile(firstInfo, releasedInfo) {
		t.Fatal("release replaced the stable lock file")
	}

	second, err := TryAcquireFileLock(path)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	secondInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat second lock path: %v", err)
	}
	if !os.SameFile(firstInfo, secondInfo) {
		t.Fatal("reacquire used a different lock file")
	}
	if err := second.Release(); err != nil {
		t.Fatalf("release second: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatalf("idempotent release: %v", err)
	}
}

func TestFileLockMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	lock, err := TryAcquireFileLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.WriteMetadata([]byte("holder\n")); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "holder\n" {
		t.Fatalf("metadata = %q, err %v", data, err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if err := lock.WriteMetadata(nil); !errors.Is(err, ErrLockReleased) {
		t.Fatalf("write after release = %v, want ErrLockReleased", err)
	}
}

func TestFileLockConcurrentFirstAcquisition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	const contenders = 200
	start := make(chan struct{})
	results := make(chan error, contenders)
	locks := make(chan *FileLock, contenders)
	var ready sync.WaitGroup
	ready.Add(contenders)
	for range contenders {
		go func() {
			ready.Done()
			<-start
			lock, err := TryAcquireFileLock(path)
			if lock != nil {
				locks <- lock
			}
			results <- err
		}()
	}
	ready.Wait()
	close(start)
	acquired := 0
	for range contenders {
		err := <-results
		switch {
		case err == nil:
			acquired++
		case !errors.Is(err, ErrLockHeld):
			t.Errorf("concurrent first acquire: %v", err)
		}
	}
	close(locks)
	for lock := range locks {
		if err := lock.Release(); err != nil {
			t.Error(err)
		}
	}
	if acquired != 1 {
		t.Fatalf("concurrent first acquire: got %d holders, want 1", acquired)
	}
}

func TestFileLockRefusesMultiplyLinkedEntry(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	lockPath := filepath.Join(dir, "lock")
	const sentinel = "do not overwrite"
	if err := os.WriteFile(target, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, lockPath); err != nil {
		t.Fatalf("create redirected hard link: %v", err)
	}
	if lock, err := TryAcquireFileLock(lockPath); err == nil {
		_ = lock.Release()
		t.Fatal("TryAcquireFileLock accepted a multiply-linked entry")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != sentinel {
		t.Fatalf("redirected target was modified: %q", data)
	}
}

func TestFileLockAtRejectsEscapingPath(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(filepath.Dir(base), "outside.lock")
	if lock, err := TryAcquireFileLockAt(base, outside); err == nil {
		_ = lock.Release()
		t.Fatal("TryAcquireFileLockAt accepted a path outside its root")
	}
	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("escaping path was created: %v", err)
	}
}

// TestFileLockReleasedByProcessExit proves crash recovery is provided by the
// kernel lock lifetime. The child exits without calling Release; the next
// process can then lock the same persistent file without moving or deleting it.
func TestFileLockReleasedByProcessExit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	cmd := exec.Command(os.Args[0], "-test.run=^TestFileLockHelperProcess$")
	cmd.Env = append(os.Environ(), "ZERO_LOCKUTIL_HELPER=1", "ZERO_LOCKUTIL_PATH="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	ready, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(ready) != "ready" {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("helper readiness = %q, err %v", ready, err)
	}
	if _, err := TryAcquireFileLock(path); !errors.Is(err, ErrLockHeld) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("acquire while helper holds lock = %v, want ErrLockHeld", err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper exit: %v", err)
	}

	lock, err := TryAcquireFileLock(path)
	if err != nil {
		t.Fatalf("acquire after helper exit: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestFileLockHelperProcess(t *testing.T) {
	if os.Getenv("ZERO_LOCKUTIL_HELPER") != "1" {
		return
	}
	lock, err := TryAcquireFileLock(os.Getenv("ZERO_LOCKUTIL_PATH"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if _, err := fmt.Fprintln(os.Stdout, "ready"); err != nil {
		os.Exit(3)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	runtime.KeepAlive(lock) // deliberately exit without Release
}
