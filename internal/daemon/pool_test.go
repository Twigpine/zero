package daemon

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/testutil"
)

// --- test doubles ---------------------------------------------------------

type fakeLines struct {
	lines []string
	i     int
	err   error // when set, returned once after the lines are exhausted (read-error tests)
}

func (f *fakeLines) Next() (string, bool, error) {
	if f.i >= len(f.lines) {
		if f.err != nil {
			e := f.err
			f.err = nil
			return "", false, e
		}
		return "", false, nil
	}
	l := f.lines[f.i]
	f.i++
	return l, true, nil
}

type fakeWorker struct {
	pid      int
	out      []string
	outErr   error // a stdout read error surfaced after the lines (read-error tests)
	exitCode int
	killed   int32
	waitCh   chan struct{} // when non-nil, Wait blocks until closed (drain tests)
}

func (w *fakeWorker) Stdout() Lines { return &fakeLines{lines: w.out, err: w.outErr} }
func (w *fakeWorker) Wait() (int, error) {
	if w.waitCh != nil {
		<-w.waitCh
	}
	return w.exitCode, nil
}
func (w *fakeWorker) Kill() error {
	atomic.StoreInt32(&w.killed, 1)
	if w.waitCh != nil {
		select {
		case <-w.waitCh:
		default:
			close(w.waitCh)
		}
	}
	return nil
}
func (w *fakeWorker) Pid() int { return w.pid }

// seqLauncher hands out the given workers in order, then errors.
func seqLauncher(workers ...*fakeWorker) (Launcher, *int32) {
	var calls int32
	l := func(_ context.Context, _ WorkerSpec) (WorkerHandle, error) {
		n := atomic.AddInt32(&calls, 1)
		idx := int(n) - 1
		if idx >= len(workers) {
			return nil, errors.New("no more fake workers")
		}
		return workers[idx], nil
	}
	return l, &calls
}

type collectSink struct {
	mu    sync.Mutex
	lines []string
}

func (c *collectSink) Line(line string) {
	c.mu.Lock()
	c.lines = append(c.lines, line)
	c.mu.Unlock()
}

// --- tests ----------------------------------------------------------------

func TestPoolRunStreamsAndSucceeds(t *testing.T) {
	launcher, calls := seqLauncher(&fakeWorker{pid: 1, out: []string{"a", "b"}, exitCode: 0})
	pool, err := NewPool(PoolOptions{Size: 2, Launcher: launcher})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	sink := &collectSink{}
	code, err := pool.Run(context.Background(), WorkerSpec{Session: "s"}, sink)
	if err != nil || code != 0 {
		t.Fatalf("Run = (%d,%v), want (0,nil)", code, err)
	}
	if len(sink.lines) != 2 || sink.lines[0] != "a" || sink.lines[1] != "b" {
		t.Fatalf("sink lines = %v, want [a b]", sink.lines)
	}
	if *calls != 1 {
		t.Fatalf("launcher calls = %d, want 1", *calls)
	}
}

func TestPoolRunRetriesWithBackoff(t *testing.T) {
	launcher, calls := seqLauncher(
		&fakeWorker{pid: 1, exitCode: 1},                      // crash
		&fakeWorker{pid: 2, exitCode: 1},                      // crash
		&fakeWorker{pid: 3, exitCode: 0, out: []string{"ok"}}, // success
	)
	var backoffArgs []int
	pool, _ := NewPool(PoolOptions{
		Size: 1, Launcher: launcher, MaxAttempts: 5,
		Backoff: func(attempt int) time.Duration { backoffArgs = append(backoffArgs, attempt); return 0 },
	})
	sink := &collectSink{}
	code, err := pool.Run(context.Background(), WorkerSpec{Session: "s"}, sink)
	if err != nil || code != 0 {
		t.Fatalf("Run = (%d,%v), want (0,nil)", code, err)
	}
	if *calls != 3 {
		t.Fatalf("launcher calls = %d, want 3 (2 crashes + success)", *calls)
	}
	// Backoff invoked between attempts with an increasing 1-based restart count.
	if len(backoffArgs) != 2 || backoffArgs[0] != 1 || backoffArgs[1] != 2 {
		t.Fatalf("backoff attempts = %v, want [1 2]", backoffArgs)
	}
}

func TestPoolRunPermanentStopsImmediately(t *testing.T) {
	launcher, calls := seqLauncher(&fakeWorker{pid: 1, exitCode: ExitPermanent})
	pool, _ := NewPool(PoolOptions{Size: 1, Launcher: launcher, Backoff: func(int) time.Duration { return 0 }})
	code, err := pool.Run(context.Background(), WorkerSpec{Session: "s"}, &collectSink{})
	if !errors.Is(err, ErrPermanent) {
		t.Fatalf("Run err = %v, want ErrPermanent", err)
	}
	if code != ExitPermanent {
		t.Fatalf("Run code = %d, want %d", code, ExitPermanent)
	}
	if *calls != 1 {
		t.Fatalf("permanent exit must not retry; launcher calls = %d, want 1", *calls)
	}
}

func TestPoolRunCapExhausted(t *testing.T) {
	launcher, calls := seqLauncher(
		&fakeWorker{pid: 1, exitCode: 1},
		&fakeWorker{pid: 2, exitCode: 1},
		&fakeWorker{pid: 3, exitCode: 1},
	)
	pool, _ := NewPool(PoolOptions{Size: 1, Launcher: launcher, MaxAttempts: 3, Backoff: func(int) time.Duration { return 0 }})
	_, err := pool.Run(context.Background(), WorkerSpec{Session: "s"}, &collectSink{})
	if !errors.Is(err, ErrPermanent) {
		t.Fatalf("Run err = %v, want ErrPermanent after cap", err)
	}
	if *calls != 3 {
		t.Fatalf("launcher calls = %d, want 3 (== MaxAttempts)", *calls)
	}
}

func TestPoolRunTempfailRetries(t *testing.T) {
	launcher, calls := seqLauncher(
		&fakeWorker{pid: 1, exitCode: ExitTempfail},
		&fakeWorker{pid: 2, exitCode: 0, out: []string{"ok"}},
	)
	pool, _ := NewPool(PoolOptions{Size: 1, Launcher: launcher, TempfailDelay: time.Millisecond})
	code, err := pool.Run(context.Background(), WorkerSpec{Session: "s"}, &collectSink{})
	if err != nil || code != 0 {
		t.Fatalf("Run = (%d,%v), want (0,nil)", code, err)
	}
	if *calls != 2 {
		t.Fatalf("launcher calls = %d, want 2 (tempfail + success)", *calls)
	}
}

func TestPoolQueuesWhenFull(t *testing.T) {
	block := make(chan struct{})
	first := &fakeWorker{pid: 1, waitCh: block}
	second := &fakeWorker{pid: 2, exitCode: 0}
	launcher, _ := seqLauncher(first, second)
	pool, _ := NewPool(PoolOptions{Size: 1, Launcher: launcher})

	started := make(chan struct{})
	go func() {
		close(started)
		_, _ = pool.Run(context.Background(), WorkerSpec{Session: "a"}, &collectSink{})
	}()
	<-started
	// Wait until the first run holds the only slot.
	testutil.WaitFor(t, "queue depth 1", func() bool { return pool.QueueDepth() == 1 })

	secondDone := make(chan struct{})
	go func() {
		_, _ = pool.Run(context.Background(), WorkerSpec{Session: "b"}, &collectSink{})
		close(secondDone)
	}()
	// The second run must NOT complete while the slot is held.
	select {
	case <-secondDone:
		t.Fatal("second run completed while the only slot was busy (no queueing)")
	case <-time.After(50 * time.Millisecond):
	}
	// Free the slot; the queued run now proceeds.
	close(block)
	select {
	case <-secondDone:
	case <-time.After(2 * time.Second):
		t.Fatal("queued run did not proceed after slot freed")
	}
}

func TestPoolDrainKillsStraggler(t *testing.T) {
	block := make(chan struct{})
	straggler := &fakeWorker{pid: 1, waitCh: block}
	launcher, _ := seqLauncher(straggler)
	pool, _ := NewPool(PoolOptions{Size: 1, Launcher: launcher, MaxAttempts: 1, KillTimeout: 10 * time.Millisecond})

	runDone := make(chan struct{})
	go func() {
		_, _ = pool.Run(context.Background(), WorkerSpec{Session: "a"}, &collectSink{})
		close(runDone)
	}()
	testutil.WaitFor(t, "queue depth 1", func() bool { return pool.QueueDepth() == 1 })

	pool.Drain() // KillTimeout elapses, straggler is force-killed
	if atomic.LoadInt32(&straggler.killed) != 1 {
		t.Fatal("Drain must kill a straggler still running after KillTimeout")
	}
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not finish after drain kill")
	}

	// A new Run after drain is refused.
	if _, err := pool.Run(context.Background(), WorkerSpec{Session: "b"}, &collectSink{}); !errors.Is(err, ErrPoolDraining) {
		t.Fatalf("Run after drain err = %v, want ErrPoolDraining", err)
	}
}

func TestPoolRunSurfacesStdoutReadError(t *testing.T) {
	// A worker stdout read error must surface as a failure (not be swallowed and
	// reported as clean success), and the worker is killed so it can't block on an
	// unread pipe (D1/D2).
	w := &fakeWorker{pid: 1, out: []string{"partial"}, outErr: errors.New("read failed")}
	launcher, _ := seqLauncher(w)
	pool, err := NewPool(PoolOptions{Size: 1, Launcher: launcher, MaxAttempts: 1, Backoff: func(int) time.Duration { return 0 }})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if _, runErr := pool.Run(context.Background(), WorkerSpec{Session: "s"}, &collectSink{}); runErr == nil {
		t.Fatal("a worker stdout read error must surface, not report clean success")
	}
	if atomic.LoadInt32(&w.killed) != 1 {
		t.Error("the worker should be killed on a read error to avoid a pipe-block hang")
	}
}
