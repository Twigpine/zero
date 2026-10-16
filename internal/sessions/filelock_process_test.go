package sessions

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestSessionFileLockBlocksAnotherProcessUntilItExits proves the OS session lock
// (flock on unix, LockFileEx on Windows) serializes mutations across processes,
// not just across Store instances in one process, and that it is released when
// the holder exits without unlocking.
func TestSessionFileLockBlocksAnotherProcessUntilItExits(t *testing.T) {
	root := t.TempDir()
	store := NewStore(StoreOptions{RootDir: root})
	if _, err := store.Create(CreateInput{SessionID: "s"}); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestSessionFileLockHelperProcess$")
	cmd.Env = append(os.Environ(), "ZERO_SESSIONS_LOCK_HELPER=1", "ZERO_SESSIONS_LOCK_ROOT="+root)
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
	exited := false
	t.Cleanup(func() {
		if !exited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	ready, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(ready) != "ready" {
		t.Fatalf("helper readiness = %q, err %v", ready, err)
	}

	// The helper process holds the session lock, so this must block.
	done := make(chan error, 1)
	go func() {
		_, appendErr := store.AppendEvent("s", AppendEventInput{Type: EventMessage, Payload: map[string]any{}})
		done <- appendErr
	}()
	select {
	case err := <-done:
		t.Fatalf("AppendEvent completed while another process held the session lock (err=%v)", err)
	case <-time.After(300 * time.Millisecond):
		// Expected: still blocked.
	}

	// The helper exits without unlocking; the OS must release the lock for us.
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper exit: %v", err)
	}
	exited = true
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("AppendEvent after the holder exited: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AppendEvent did not proceed after the lock holder exited")
	}
}

func TestSessionFileLockHelperProcess(t *testing.T) {
	if os.Getenv("ZERO_SESSIONS_LOCK_HELPER") != "1" {
		return
	}
	store := NewStore(StoreOptions{RootDir: os.Getenv("ZERO_SESSIONS_LOCK_ROOT")})
	unlock, err := store.lockSession("s")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if _, err := fmt.Fprintln(os.Stdout, "ready"); err != nil {
		os.Exit(3)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	// The unlock closure is the only reference to the open lock file/handle.
	// Keep it alive until stdin closes, or GC finalization of the file could
	// release the OS lock while this helper is meant to be holding it.
	runtime.KeepAlive(unlock) // deliberately exit without calling unlock
}
