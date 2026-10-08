package swarm

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func newTestMailbox(t *testing.T) *Mailbox {
	t.Helper()
	mb, err := NewMailbox(t.TempDir())
	if err != nil {
		t.Fatalf("NewMailbox: %v", err)
	}
	return mb
}

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"":           "default",
		"   ":        "default",
		"team-1_ok":  "team-1_ok",
		"../../etc":  "------etc",
		"a/b\\c":     "a-b-c",
		"team name!": "team-name-",
		"!!!":        "---",
		"plain":      "plain",
	}
	for in, want := range cases {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMailboxSendAndConsume(t *testing.T) {
	mb := newTestMailbox(t)
	if err := mb.Send("team", "bob", Message{From: "alice", Body: "hello"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := mb.Send("team", "bob", Message{From: "alice", Body: "again"}); err != nil {
		t.Fatalf("Send 2: %v", err)
	}
	msgs, err := mb.ReadAndConsume("team", "bob")
	if err != nil {
		t.Fatalf("ReadAndConsume: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	// First read returns them as unread (pre-consume snapshot), with defaults set.
	if msgs[0].Read || msgs[0].Type != "message" || msgs[0].To != "bob" || msgs[0].Time == "" {
		t.Fatalf("message defaults wrong: %+v", msgs[0])
	}
	// Second read shows them already consumed (read).
	msgs2, err := mb.ReadAndConsume("team", "bob")
	if err != nil {
		t.Fatalf("ReadAndConsume 2: %v", err)
	}
	for _, m := range msgs2 {
		if !m.Read {
			t.Fatalf("message should be read after consume: %+v", m)
		}
	}
}

func TestMailboxReadMissingIsEmpty(t *testing.T) {
	mb := newTestMailbox(t)
	msgs, err := mb.ReadAndConsume("team", "nobody")
	if err != nil {
		t.Fatalf("ReadAndConsume missing: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("missing inbox should be empty, got %d", len(msgs))
	}
}

func TestMailboxOversizeMessageRejected(t *testing.T) {
	mb := newTestMailbox(t)
	mb.MaxMessageBytes = 64
	err := mb.Send("team", "bob", Message{From: "a", Body: strings.Repeat("x", 1000)})
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("oversize Send err = %v, want ErrMessageTooLarge", err)
	}
	// Nothing should have been written.
	msgs, _ := mb.ReadAndConsume("team", "bob")
	if len(msgs) != 0 {
		t.Fatalf("oversize Send must not persist, got %d messages", len(msgs))
	}
}

func TestMailboxFullRejected(t *testing.T) {
	mb := newTestMailbox(t)
	mb.MaxMessages = 2
	for i := 0; i < 2; i++ {
		if err := mb.Send("team", "bob", Message{From: "a", Body: "ok"}); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}
	if err := mb.Send("team", "bob", Message{From: "a", Body: "overflow"}); !errors.Is(err, ErrMailboxFull) {
		t.Fatalf("full Send err = %v, want ErrMailboxFull", err)
	}
}

func TestMailboxMalformedFailsClosed(t *testing.T) {
	mb := newTestMailbox(t)
	// Write a valid message so the inbox path exists, then corrupt the file.
	if err := mb.Send("team", "bob", Message{From: "a", Body: "ok"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	path, err := mb.inboxPath("team", "bob")
	if err != nil {
		t.Fatalf("inboxPath: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("corrupt inbox: %v", err)
	}
	if _, err := mb.ReadAndConsume("team", "bob"); err == nil {
		t.Fatal("malformed inbox must fail closed (error), not be treated as empty")
	}
	// A subsequent Send also fails closed rather than clobbering unknown data.
	if err := mb.Send("team", "bob", Message{From: "a", Body: "x"}); err == nil {
		t.Fatal("Send into a malformed inbox must fail closed")
	}
}

func TestMailboxPathConfinement(t *testing.T) {
	mb := newTestMailbox(t)
	// A traversal-style name is sanitized into a single safe segment; the inbox
	// stays under BaseDir.
	path, err := mb.inboxPath("../../evil", "../escape")
	if err != nil {
		t.Fatalf("inboxPath: %v", err)
	}
	rel, err := filepath.Rel(mb.BaseDir, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("inbox path escaped base dir: base=%q path=%q rel=%q", mb.BaseDir, path, rel)
	}
}

func TestMailboxFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file modes")
	}
	mb := newTestMailbox(t)
	if err := mb.Send("team", "bob", Message{From: "a", Body: "ok"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	path, _ := mb.inboxPath("team", "bob")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("inbox file mode = %o, want 600", perm)
	}
}

func TestMailboxTightensDirPerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file modes")
	}
	base := t.TempDir()
	// Pre-create the team + inboxes dirs with broad perms; ensureInboxDir must
	// tighten them to 0700 despite MkdirAll leaving existing dirs untouched.
	broad := filepath.Join(base, "team", "inboxes")
	if err := os.MkdirAll(broad, 0o755); err != nil {
		t.Fatalf("pre-create: %v", err)
	}
	mb, err := NewMailbox(base)
	if err != nil {
		t.Fatalf("NewMailbox: %v", err)
	}
	if err := mb.Send("team", "bob", Message{From: "a", Body: "ok"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	for _, dir := range []string{broad, filepath.Join(base, "team")} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Fatalf("dir %s mode = %o, want 700", dir, perm)
		}
	}
}

func TestMailboxRejectsSymlinkedInboxDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ")
	}
	base := t.TempDir()
	outside := t.TempDir() // a dir OUTSIDE base
	// Plant a symlink: <base>/team/inboxes -> <outside>. A lexical check passes,
	// but the symlink-aware confinement must reject the write.
	if err := os.MkdirAll(filepath.Join(base, "team"), 0o700); err != nil {
		t.Fatalf("mkdir team: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(base, "team", "inboxes")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	mb, err := NewMailbox(base)
	if err != nil {
		t.Fatalf("NewMailbox: %v", err)
	}
	if err := mb.Send("team", "bob", Message{From: "a", Body: "escape"}); err == nil {
		t.Fatal("Send into a symlinked-out inbox dir must fail closed")
	}
	// And nothing was written outside the base.
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("write escaped base via symlink: %d entries in %s", len(entries), outside)
	}
}

func TestMailboxLockReleaseKeepsStablePath(t *testing.T) {
	mb := newTestMailbox(t)
	path, _ := mb.inboxPath("team", "bob")
	if err := mb.ensureInboxDir(path); err != nil {
		t.Fatalf("ensureInboxDir: %v", err)
	}
	lockPath := path + ".lock"
	releaseA, err := acquireLock(mb.BaseDir, lockPath, time.Second)
	if err != nil {
		t.Fatalf("acquire A: %v", err)
	}
	releaseA()
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("release removed stable advisory-lock path: %v", err)
	}
}

func TestAcquireLockIgnoresOldUnlockedFile(t *testing.T) {
	// A crashed holder can leave an old file behind, but the kernel releases its
	// advisory lock. Acquisition must succeed without renaming the stable path.
	lockPath := filepath.Join(t.TempDir(), "x.lock")
	if err := os.WriteFile(lockPath, []byte("dead-holder"), 0o600); err != nil {
		t.Fatalf("seed old lock metadata: %v", err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(lockPath, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	release, err := acquireLock(filepath.Dir(lockPath), lockPath, time.Second)
	if err != nil {
		t.Fatalf("acquire over old unlocked file: %v", err)
	}
	release()
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("stable lock path missing after release: %v", err)
	}
}

func TestAcquireLockDoesNotBreakHeldLock(t *testing.T) {
	// File age is irrelevant while the kernel reports an active holder.
	lockPath := filepath.Join(t.TempDir(), "x.lock")
	release, err := acquireLock(filepath.Dir(lockPath), lockPath, time.Second)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer release()
	if _, err := acquireLock(filepath.Dir(lockPath), lockPath, 50*time.Millisecond); err == nil {
		t.Fatal("acquireLock admitted a second holder")
	}
}

func TestAcquireLockRefusesRedirectedPath(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	const sentinel = "do not overwrite"
	if err := os.WriteFile(target, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	lockPath := makeRedirectedLockPath(t, root, target)
	if release, err := acquireLock(root, lockPath, time.Second); err == nil {
		release()
		t.Fatal("acquireLock followed a redirected lock path")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != sentinel {
		t.Fatalf("redirected target was modified: %q", data)
	}
}

func TestMailboxConcurrentSends(t *testing.T) {
	mb := newTestMailbox(t)
	// High fan-out exercises heavy contention (a lock regression — e.g. a Windows
	// sharing-violation treated as fatal — surfaces as lost/failed messages here).
	// The lock timeout is generous so a slow CI (Windows file ops are slow under
	// 200-way contention) never times a legitimate send out and flakes.
	mb.LockTimeout = 60 * time.Second
	const n = 200
	var wg sync.WaitGroup
	var failures atomic.Int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := mb.Send("team", "bob", Message{From: "a", Body: "concurrent"}); err != nil {
				failures.Add(1)
				t.Logf("Send error: %v", err)
			}
		}()
	}
	wg.Wait()
	if f := failures.Load(); f != 0 {
		t.Fatalf("%d concurrent Sends failed (lock contention not handled)", f)
	}
	msgs, err := mb.ReadAndConsume("team", "bob")
	if err != nil {
		t.Fatalf("ReadAndConsume: %v", err)
	}
	if len(msgs) != n {
		t.Fatalf("concurrent sends lost messages: got %d, want %d", len(msgs), n)
	}
}

func TestMailboxRenameRetry(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("renameWithRetry only retries on Windows")
	}
	mb := newTestMailbox(t)

	var attempts int
	mb.rename = func(src, dst string) error {
		attempts++
		if attempts < 3 {
			// Return a retryable Windows sharing violation error
			return syscall.Errno(32)
		}
		// Then succeed
		return os.Rename(src, dst)
	}

	err := mb.Send("team", "alice", Message{Body: "retry-test"})
	if err != nil {
		t.Fatalf("expected Send to succeed after retries, got: %v", err)
	}
	if attempts < 3 {
		t.Errorf("expected at least 3 attempts, got %d", attempts)
	}
}

func TestMailboxRenameNonRetryableError(t *testing.T) {
	mb := newTestMailbox(t)

	var attempts int
	expectedErr := errors.New("non-retryable error")
	mb.rename = func(src, dst string) error {
		attempts++
		return expectedErr
	}

	err := mb.Send("team", "alice", Message{Body: "retry-test"})
	if err == nil {
		t.Fatal("expected Send to fail immediately")
	}
	if !errors.Is(err, expectedErr) && !strings.Contains(err.Error(), expectedErr.Error()) {
		t.Errorf("expected error %v, got %v", expectedErr, err)
	}
	if attempts != 1 {
		t.Errorf("expected only 1 attempt for non-retryable error, got %d", attempts)
	}
}
