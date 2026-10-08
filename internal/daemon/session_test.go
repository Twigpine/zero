package daemon

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/testutil"
)

func drain(t *testing.T, buffered []string, live <-chan string) []string {
	t.Helper()
	got := append([]string(nil), buffered...)
	timeout := time.After(2 * time.Second)
	for {
		select {
		case line, ok := <-live:
			if !ok {
				return got
			}
			got = append(got, line)
		case <-timeout:
			t.Fatal("timed out draining session stream")
		}
	}
}

func TestSessionSlowSubscriberGetsGapNotice(t *testing.T) {
	// A subscriber that stops reading must not silently lose lines: once its channel
	// fills, Line counts the drops and flushes a single gap notice the moment the
	// channel drains, so the consumer learns its stream is incomplete (D8).
	sess := newSession("s", "", 0)
	_, live, cancel := sess.Subscribe()
	defer cancel()

	capacity := cap(live)
	// Fill the channel, then push extra lines that must be dropped (not block).
	for i := 0; i < capacity+4; i++ {
		sess.Line("x")
	}
	// Drain everything currently buffered.
	drained := 0
	for {
		select {
		case <-live:
			drained++
			continue
		default:
		}
		break
	}
	if drained != capacity {
		t.Fatalf("drained %d lines, want %d (the channel capacity)", drained, capacity)
	}

	// The next line triggers the gap notice (4 lines were dropped) ahead of itself.
	sess.Line("resume")
	first := <-live
	if first != gapNotice(4) {
		t.Fatalf("first post-lag line = %q, want gap notice %q", first, gapNotice(4))
	}
	second := <-live
	if second != "resume" {
		t.Fatalf("line after gap notice = %q, want %q", second, "resume")
	}
}

func TestSessionStartRoutesAndStreams(t *testing.T) {
	launcher, _ := seqLauncher(&fakeWorker{pid: 1, out: []string{"e1", "e2", "e3"}, exitCode: 0})
	pool, _ := NewPool(PoolOptions{Size: 2, Launcher: launcher})
	mgr, err := NewSessionManager(SessionManagerOptions{Pool: pool})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	sess, err := mgr.Start(context.Background(), WorkerSpec{Session: "s1"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	buffered, live, cancel := sess.Subscribe()
	defer cancel()
	got := drain(t, buffered, live)
	if len(got) != 3 || got[0] != "e1" || got[2] != "e3" {
		t.Fatalf("streamed lines = %v, want [e1 e2 e3]", got)
	}
	<-sess.Done()
	if sess.State() != SessionDone {
		t.Fatalf("state = %s, want done", sess.State())
	}
}

func TestSessionDuplicateIDRejected(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	launcher, _ := seqLauncher(&fakeWorker{pid: 1, waitCh: block})
	pool, _ := NewPool(PoolOptions{Size: 2, Launcher: launcher})
	mgr, _ := NewSessionManager(SessionManagerOptions{Pool: pool})
	if _, err := mgr.Start(context.Background(), WorkerSpec{Session: "dup"}); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if _, err := mgr.Start(context.Background(), WorkerSpec{Session: "dup"}); !errors.Is(err, ErrSessionExists) {
		t.Fatalf("second Start err = %v, want ErrSessionExists", err)
	}
}

func TestSessionLeaseQueuesWhenPoolFull(t *testing.T) {
	block := make(chan struct{})
	a := &fakeWorker{pid: 1, waitCh: block}
	b := &fakeWorker{pid: 2, out: []string{"b1"}, exitCode: 0}
	launcher, calls := seqLauncher(a, b)
	pool, _ := NewPool(PoolOptions{Size: 1, Launcher: launcher})
	mgr, _ := NewSessionManager(SessionManagerOptions{Pool: pool})

	sa, _ := mgr.Start(context.Background(), WorkerSpec{Session: "a"})
	testutil.WaitFor(t, "session running", func() bool { return sa.State() == SessionRunning })

	sb, _ := mgr.Start(context.Background(), WorkerSpec{Session: "b"})
	// b must stay queued (its worker not yet launched) while a holds the slot.
	time.Sleep(50 * time.Millisecond)
	if sb.State() != SessionQueued {
		t.Fatalf("session b state = %s, want queued while pool full", sb.State())
	}
	if n := atomic.LoadInt32(calls); n != 1 {
		t.Fatalf("launcher calls = %d, want 1 (b not launched yet)", n)
	}

	close(block) // a finishes, slot frees, b proceeds
	<-sb.Done()
	if sb.State() != SessionDone {
		t.Fatalf("session b state = %s, want done", sb.State())
	}
	if n := atomic.LoadInt32(calls); n != 2 {
		t.Fatalf("launcher calls = %d, want 2 after slot freed", n)
	}
}

func TestSessionAttachAfterFinishSeesBuffer(t *testing.T) {
	launcher, _ := seqLauncher(&fakeWorker{pid: 1, out: []string{"x", "y"}, exitCode: 0})
	pool, _ := NewPool(PoolOptions{Size: 1, Launcher: launcher})
	mgr, _ := NewSessionManager(SessionManagerOptions{Pool: pool})
	sess, _ := mgr.Start(context.Background(), WorkerSpec{Session: "s"})
	<-sess.Done()

	buffered, live, cancel, err := mgr.Attach("s")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer cancel()
	got := drain(t, buffered, live)
	if len(got) != 2 || got[0] != "x" || got[1] != "y" {
		t.Fatalf("attach buffered = %v, want [x y]", got)
	}
	if _, _, _, err := mgr.Attach("missing"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Attach(missing) err = %v, want ErrSessionNotFound", err)
	}
}

func TestSessionManagerEvictsFinishedOverCap(t *testing.T) {
	launcher, _ := seqLauncher(
		&fakeWorker{pid: 1, out: []string{"a"}, exitCode: 0},
		&fakeWorker{pid: 2, out: []string{"b"}, exitCode: 0},
		&fakeWorker{pid: 3, out: []string{"c"}, exitCode: 0},
	)
	pool, _ := NewPool(PoolOptions{Size: 2, Launcher: launcher})
	mgr, _ := NewSessionManager(SessionManagerOptions{Pool: pool, MaxSessions: 2})

	for _, id := range []string{"s1", "s2", "s3"} {
		s, err := mgr.Start(context.Background(), WorkerSpec{Session: id})
		if err != nil {
			t.Fatalf("Start(%s): %v", id, err)
		}
		<-s.Done() // finish before starting the next so prune can evict it
	}

	// Past the cap of 2, the oldest FINISHED session (s1) is evicted; the two
	// newest are retained.
	if _, ok := mgr.Get("s1"); ok {
		t.Fatal("oldest finished session must be evicted past the cap")
	}
	if _, ok := mgr.Get("s2"); !ok {
		t.Fatal("s2 must be retained")
	}
	if _, ok := mgr.Get("s3"); !ok {
		t.Fatal("s3 (newest) must be retained")
	}
}

func TestSessionManagerKeepsRunningOverCap(t *testing.T) {
	// All sessions are running (workers block), so none are evicted even past cap.
	block := make(chan struct{})
	defer close(block)
	launcher, _ := seqLauncher(
		&fakeWorker{pid: 1, waitCh: block},
		&fakeWorker{pid: 2, waitCh: block},
	)
	pool, _ := NewPool(PoolOptions{Size: 2, Launcher: launcher})
	mgr, _ := NewSessionManager(SessionManagerOptions{Pool: pool, MaxSessions: 1})

	s1, _ := mgr.Start(context.Background(), WorkerSpec{Session: "r1"})
	s2, _ := mgr.Start(context.Background(), WorkerSpec{Session: "r2"})
	testutil.WaitFor(t, "both sessions running", func() bool { return s1.State() == SessionRunning && s2.State() == SessionRunning })
	if _, ok := mgr.Get("r1"); !ok {
		t.Fatal("a running session must never be evicted, even past the cap")
	}
	if _, ok := mgr.Get("r2"); !ok {
		t.Fatal("a running session must never be evicted, even past the cap")
	}
}

func TestSessionStatuses(t *testing.T) {
	launcher, _ := seqLauncher(&fakeWorker{pid: 1, out: []string{"l"}, exitCode: 0})
	pool, _ := NewPool(PoolOptions{Size: 1, Launcher: launcher})
	mgr, _ := NewSessionManager(SessionManagerOptions{Pool: pool})
	sess, _ := mgr.Start(context.Background(), WorkerSpec{Session: "only"})
	<-sess.Done()
	st := mgr.Statuses()
	if len(st) != 1 || st[0].ID != "only" || st[0].State != string(SessionDone) || st[0].Lines != 1 {
		t.Fatalf("statuses = %+v, want one done session with 1 line", st)
	}
}
