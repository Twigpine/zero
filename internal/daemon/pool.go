package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Exit codes a worker may use to signal restart policy. Mirrors
// reference-daemon-code-agent-js/worker-manager.js EXIT_TEMPFAIL/EXIT_PERMANENT.
const (
	// ExitTempfail asks for a retry after a fixed cool-off (transient failure).
	ExitTempfail = 75
	// ExitPermanent asks the daemon to STOP retrying this work (fatal).
	ExitPermanent = 76
)

// Exit codes `zero exec` (internal/cli/exec.go) uses to report a run that
// finished, or was stopped on purpose, rather than crashed. They are duplicated
// here because the daemon cannot import the CLI package; keep them in sync.
const (
	exitUsage       = 2   // bad flags or arguments: a retry fails the same way
	exitProvider    = 3   // provider failure the run already surfaced
	exitIncomplete  = 4   // the agent ran (edits, commands) and stopped unfinished
	exitInterrupted = 130 // SIGINT: someone stopped it on purpose
)

// isFinalExit reports whether a worker exit code means the run is over and must
// not be retried. Retrying would repeat the agent's side effects and re-bill the
// provider, and append a second run to the same session stream. Only crash-type
// exits (1, signals) and launch/read errors are worth another attempt.
func isFinalExit(code int) bool {
	switch code {
	case exitUsage, exitProvider, exitIncomplete, exitInterrupted:
		return true
	}
	return false
}

// defaultPoolSize, defaultMaxAttempts and defaultKillTimeout are used when a
// PoolOptions field is left zero.
const (
	defaultPoolSize    = 4
	defaultMaxAttempts = 5
	defaultKillTimeout = 5 * time.Second
)

// ErrPoolDraining is returned by Run once the pool is shutting down.
var ErrPoolDraining = errors.New("daemon: pool is draining")

// ErrPermanent is returned when a worker exits ExitPermanent or the attempt cap
// is exhausted; the request must not be retried.
var ErrPermanent = errors.New("daemon: worker failed permanently")

// WorkerSpec describes the worker to launch for one session attempt.
type WorkerSpec struct {
	Session string
	Cwd     string
	// Args are the per-session exec flags (e.g. --prompt ...), parsed by the CLI
	// and appended after `exec` by the production launcher.
	Args []string
}

// WorkerHandle is a launched worker process the pool supervises. The production
// launcher wraps an exec.Cmd running `zero exec -i/-o stream-json`; tests inject
// a fake. Stdout yields the worker's stream-json event lines.
type WorkerHandle interface {
	Stdout() Lines
	// Wait blocks until the worker exits and returns its process exit code.
	Wait() (int, error)
	// Kill force-terminates the worker (used on drain timeout).
	Kill() error
	Pid() int
}

// Lines is a stream of stream-json output lines from a worker. Next returns the
// next line and io-EOF-style ok=false when the stream ends.
type Lines interface {
	Next() (line string, ok bool, err error)
}

// Launcher spawns a worker for a single attempt. It must apply the sandbox/env
// policy (the production launcher scrubs the re-entrancy markers so the worker
// re-establishes its own sandbox — see newExecLauncher).
type Launcher func(ctx context.Context, spec WorkerSpec) (WorkerHandle, error)

// PoolOptions configures a Pool. Zero fields take documented defaults.
type PoolOptions struct {
	// Size is the max number of concurrent workers (lease slots). Sessions beyond
	// this queue until a slot frees.
	Size int
	// Launcher spawns a worker. Required.
	Launcher Launcher
	// MaxAttempts caps the total attempts per request (first + retries). When
	// exhausted, Run returns ErrPermanent (mirrors capping consecutive restarts).
	MaxAttempts int
	// KillTimeout bounds graceful termination before SIGKILL-equivalent on drain.
	KillTimeout time.Duration
	// Backoff returns the delay before retry attempt n (1-based crash count).
	// Defaults to BackoffWithJitter; tests inject a deterministic function.
	Backoff func(attempt int) time.Duration
	// TempfailDelay is the cool-off after an ExitTempfail exit. Defaults to 30s.
	TempfailDelay time.Duration
	// Log, when set, receives one line per lifecycle event.
	Log func(string)
}

// Pool is a bounded, self-healing worker pool. A crashed worker never takes down
// the pool: Run retries on a fresh worker with backoff up to MaxAttempts, and an
// ExitPermanent worker stops retries. Drain stops new work and kills stragglers.
type Pool struct {
	opts  PoolOptions
	slots chan struct{}

	mu       sync.Mutex
	draining bool
	active   map[int]WorkerHandle // worker id -> handle, for drain/kill + status
	nextID   int

	drainOnce sync.Once
	drained   chan struct{}
}

// workerStat tracks one in-flight request's restart count (local to Run).
type workerStat struct {
	id       int
	restarts int
}

// NewPool builds a pool from opts, filling defaults.
func NewPool(opts PoolOptions) (*Pool, error) {
	if opts.Launcher == nil {
		return nil, errors.New("daemon: pool requires a Launcher")
	}
	if opts.Size <= 0 {
		opts.Size = defaultPoolSize
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = defaultMaxAttempts
	}
	if opts.KillTimeout <= 0 {
		opts.KillTimeout = defaultKillTimeout
	}
	if opts.TempfailDelay <= 0 {
		opts.TempfailDelay = 30 * time.Second
	}
	if opts.Backoff == nil {
		opts.Backoff = BackoffWithJitter
	}
	return &Pool{
		opts:    opts,
		slots:   make(chan struct{}, opts.Size),
		active:  map[int]WorkerHandle{},
		drained: make(chan struct{}),
	}, nil
}

// Size returns the configured max concurrency.
func (p *Pool) Size() int { return p.opts.Size }

func (p *Pool) logf(format string, args ...any) {
	if p.opts.Log != nil {
		p.opts.Log(fmt.Sprintf(format, args...))
	}
}

// acquire takes a lease slot, queuing until one frees or ctx is cancelled / the
// pool drains.
func (p *Pool) acquire(ctx context.Context) error {
	p.mu.Lock()
	draining := p.draining
	p.mu.Unlock()
	if draining {
		return ErrPoolDraining
	}
	select {
	case p.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.drained:
		return ErrPoolDraining
	}
}

func (p *Pool) release() { <-p.slots }

// QueueDepth reports how many lease slots are currently in use.
func (p *Pool) QueueDepth() int { return len(p.slots) }

// Sink receives a worker's stream-json output lines for one request.
type Sink interface {
	Line(line string)
}

// Run leases a worker slot and dispatches spec to a worker, streaming its
// stream-json lines to sink. It is at-least-once with bounded retries: a worker
// that crashes (non-zero, non-permanent) is retried on a fresh worker after a
// backoff; ExitPermanent, a final `zero exec` exit (usage, provider, incomplete,
// interrupted) or exhausting MaxAttempts returns ErrPermanent. Run
// queues when all slots are busy. The returned int is the final worker exit code.
func (p *Pool) Run(ctx context.Context, spec WorkerSpec, sink Sink) (int, error) {
	if err := p.acquire(ctx); err != nil {
		return 0, err
	}
	defer p.release()

	// Lease hook: a sink that tracks queue->running state is notified the instant
	// a worker slot is leased (mirrors lease.js acquiring a worker for a session).
	if s, ok := sink.(interface{ Started() }); ok {
		s.Started()
	}

	stat := p.newStat()
	var lastErr error
	for attempt := 1; attempt <= p.opts.MaxAttempts; attempt++ {
		if p.isDraining() {
			return 0, ErrPoolDraining
		}
		code, err := p.runOnce(ctx, stat.id, spec, sink)
		switch {
		case err != nil:
			lastErr = err
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			p.logf("worker %d launch/run error: %v", stat.id, err)
		case code == 0:
			return 0, nil // clean success
		case code == ExitPermanent:
			p.logf("worker %d exited permanently (code=%d) — not retrying", stat.id, code)
			return code, ErrPermanent
		case isFinalExit(code):
			p.logf("worker %d finished with exit code %d — not retrying", stat.id, code)
			return code, fmt.Errorf("%w: worker %d exited code=%d", ErrPermanent, stat.id, code)
		case code == ExitTempfail:
			lastErr = fmt.Errorf("worker %d tempfail (code=%d)", stat.id, code)
			p.logf("worker %d tempfail — retry after %s", stat.id, p.opts.TempfailDelay)
			if !p.sleep(ctx, p.opts.TempfailDelay) {
				return 0, ctx.Err()
			}
			continue // tempfail retries do not count against the crash backoff
		default:
			lastErr = fmt.Errorf("worker %d exited code=%d", stat.id, code)
		}

		if attempt == p.opts.MaxAttempts {
			break
		}
		stat.restarts++
		delay := p.opts.Backoff(stat.restarts)
		p.logf("worker %d restart %d after backoff %s", stat.id, stat.restarts, delay)
		if !p.sleep(ctx, delay) {
			return 0, ctx.Err()
		}
	}
	if lastErr == nil {
		lastErr = ErrPermanent
	}
	return ExitPermanent, fmt.Errorf("%w: %v", ErrPermanent, lastErr)
}

// runOnce launches a single worker, pumps its output to sink, and returns its
// exit code. The worker handle is tracked so Drain can kill it.
func (p *Pool) runOnce(ctx context.Context, id int, spec WorkerSpec, sink Sink) (int, error) {
	handle, err := p.opts.Launcher(ctx, spec)
	if err != nil {
		return 0, err
	}
	p.track(id, handle)
	defer p.untrack(id)

	// Pump stdout lines until the stream ends.
	lines := handle.Stdout()
	var readErr error
	for {
		line, ok, lerr := lines.Next()
		if lerr != nil {
			readErr = lerr
			break
		}
		if !ok {
			break
		}
		if sink != nil {
			sink.Line(line)
		}
	}
	if readErr != nil {
		// A stdout read error means the worker's output was NOT fully consumed, so the
		// request must surface as a failure (Run then retries / records it) rather than
		// a bogus clean success that silently drops output (D1). Kill first so the
		// worker can't block writing to the now-unread pipe and hang Wait (D2).
		_ = handle.Kill()
		_, _ = handle.Wait()
		return -1, fmt.Errorf("daemon: worker %d stdout read failed: %w", id, readErr)
	}
	return handle.Wait()
}

func (p *Pool) newStat() *workerStat {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextID++
	return &workerStat{id: p.nextID}
}

// track/untrack key the active set by the pool's monotonic worker id, not the OS
// pid: the OS can reuse a pid the instant a worker exits, so a pid key could collide
// a finished worker with a freshly-launched one and drop the wrong handle (D10).
func (p *Pool) track(id int, h WorkerHandle) {
	p.mu.Lock()
	p.active[id] = h
	p.mu.Unlock()
}

func (p *Pool) untrack(id int) {
	p.mu.Lock()
	delete(p.active, id)
	p.mu.Unlock()
}

func (p *Pool) isDraining() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.draining
}

// sleep waits d, returning false if ctx is cancelled first. A non-positive d
// returns immediately.
func (p *Pool) sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// Drain stops accepting new work, gives in-flight workers a grace window
// (KillTimeout) to finish on their own, then force-kills any straggler. It
// returns as soon as the pool is idle (graceful) or the window elapses. Safe to
// call once; subsequent calls are no-ops.
func (p *Pool) Drain() {
	p.drainOnce.Do(func() {
		p.mu.Lock()
		p.draining = true
		p.mu.Unlock()
		close(p.drained)

		// Grace window: poll until idle or the deadline.
		deadline := time.Now().Add(p.opts.KillTimeout)
		for time.Now().Before(deadline) {
			p.mu.Lock()
			n := len(p.active)
			p.mu.Unlock()
			if n == 0 {
				return // all workers drained gracefully
			}
			time.Sleep(5 * time.Millisecond)
		}

		// Timed out: force-kill remaining workers.
		p.mu.Lock()
		handles := make([]WorkerHandle, 0, len(p.active))
		for _, h := range p.active {
			handles = append(handles, h)
		}
		p.mu.Unlock()
		for _, h := range handles {
			p.logf("drain: killing straggler worker pid=%d", h.Pid())
			_ = h.Kill()
		}
	})
}

// WorkerStats returns a snapshot of the currently-active (busy) workers, bounded
// by the pool size. The on-demand pool keeps no idle workers, so an empty result
// means the pool is idle.
func (p *Pool) WorkerStats() []WorkerStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]WorkerStatus, 0, len(p.active))
	for _, h := range p.active {
		out = append(out, WorkerStatus{PID: h.Pid(), State: "busy"})
	}
	return out
}
