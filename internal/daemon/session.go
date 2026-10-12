package daemon

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Session/lease/roster model. Mirrors reference-daemon-code-agent-js/
// session-manager.js (sessionID -> worker + metrics), lease.js (one active
// request per worker) and roster.js (the live session registry). Because zero's
// `exec` worker is one-shot, each session is served by exactly one worker via
// Pool.Run, so "one active request per worker" holds inherently and the pool's
// bounded slots provide the queue-when-full behavior.

// SessionState is a session's lifecycle phase.
type SessionState string

const (
	// SessionQueued: created, waiting for a free pool slot.
	SessionQueued SessionState = "queued"
	// SessionRunning: a worker is leased and producing output.
	SessionRunning SessionState = "running"
	// SessionDone: the worker finished cleanly.
	SessionDone SessionState = "done"
	// SessionFailed: the worker failed permanently / the run errored.
	SessionFailed SessionState = "failed"
)

// ErrSessionExists is returned when starting a session ID already in use.
var ErrSessionExists = errors.New("daemon: session already exists")

// ErrSessionOverloaded means the manager's unfinished-session limit is reached.
var ErrSessionOverloaded = errors.New("daemon: pending session limit reached")

// ErrSessionNotFound is returned by Get/Attach for an unknown session ID.
var ErrSessionNotFound = errors.New("daemon: session not found")

const defaultSessionBuffer = 1024

// Session is one routed agent run. It buffers recent output (so a late `attach`
// sees history) and fans live lines out to subscribers. It implements the pool's
// Sink (Line) and the lease "started" hook (Started).
type Session struct {
	id  string
	cwd string

	mu          sync.Mutex
	state       SessionState
	buffer      []string
	maxBuffer   int
	lines       int
	finished    bool
	err         error
	exitCode    int
	subscribers map[int]*subscriber
	nextSub     int
	done        chan struct{}
}

// subscriber is one live attach: a bounded channel plus a count of live lines
// dropped because that channel was full. The drop count is flushed to the consumer
// as a gap notice (see Line) the moment the channel drains, so a lagging client
// learns its stream is incomplete instead of silently losing output (D8).
type subscriber struct {
	ch      chan string
	dropped int
}

// gapNotice is a stream-json event line emitted to a subscriber that fell behind,
// reporting how many live lines it missed. It uses a daemon-namespaced type so a
// stream-json consumer that switches on "type" ignores it rather than mistaking it
// for a terminal event; the full output remains available via a fresh attach
// (which replays the buffer).
func gapNotice(dropped int) string {
	return fmt.Sprintf(`{"type":"daemon_gap","dropped":%d}`, dropped)
}

func newSession(id, cwd string, maxBuffer int) *Session {
	if maxBuffer <= 0 {
		maxBuffer = defaultSessionBuffer
	}
	return &Session{
		id:          id,
		cwd:         cwd,
		state:       SessionQueued,
		maxBuffer:   maxBuffer,
		subscribers: map[int]*subscriber{},
		done:        make(chan struct{}),
	}
}

// ID returns the session ID.
func (s *Session) ID() string { return s.id }

// Done is closed when the session finishes (success or failure).
func (s *Session) Done() <-chan struct{} { return s.done }

// Err returns the terminal error (nil on clean completion); valid after Done.
func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// State returns the current lifecycle phase.
func (s *Session) State() SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Started marks the session running once the pool leases a worker (lease hook).
func (s *Session) Started() {
	s.mu.Lock()
	if s.state == SessionQueued {
		s.state = SessionRunning
	}
	s.mu.Unlock()
}

// Line records and broadcasts one stream-json output line.
func (s *Session) Line(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines++
	s.buffer = append(s.buffer, line)
	if len(s.buffer) > s.maxBuffer {
		// Drop the oldest line; the ring keeps only the most recent maxBuffer.
		s.buffer = s.buffer[len(s.buffer)-s.maxBuffer:]
	}
	// Hold s.mu across the (non-blocking) sends. The sends never block — every case
	// has a default — so the lock is held only briefly, and holding it stops
	// cancel()/finish() from closing a subscriber channel mid-send, which would
	// panic with "send on closed channel". It also makes each subscriber's dropped
	// counter mutated under the lock.
	for _, sub := range s.subscribers {
		// Non-blocking: a slow/stalled subscriber must not stall the worker pump.
		if sub.dropped > 0 {
			// The consumer fell behind earlier. Before delivering more output, flush a
			// gap notice so it learns its stream is incomplete (D8). If the channel is
			// still full, keep counting and skip this line too.
			select {
			case sub.ch <- gapNotice(sub.dropped):
				sub.dropped = 0
			default:
				sub.dropped++
				continue
			}
		}
		select {
		case sub.ch <- line:
		default:
			// Channel full: count the drop; the gap notice above will report it once
			// the consumer catches up. Full output stays available via a fresh attach.
			sub.dropped++
		}
	}
}

func (s *Session) finish(exitCode int, err error) {
	s.mu.Lock()
	s.finished = true
	s.exitCode = exitCode
	s.err = err
	if err != nil {
		s.state = SessionFailed
	} else {
		s.state = SessionDone
	}
	subs := s.subscribers
	s.subscribers = map[int]*subscriber{}
	s.mu.Unlock()
	for _, sub := range subs {
		close(sub.ch)
	}
	close(s.done)
}

// Subscribe returns the buffered history plus a channel of subsequent live lines
// and a cancel func. If the session has already finished, the live channel is
// returned already closed. Mirrors attach.js.
func (s *Session) Subscribe() (buffered []string, live <-chan string, cancel func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	buffered = append([]string(nil), s.buffer...)
	if s.finished {
		ch := make(chan string)
		close(ch)
		return buffered, ch, func() {}
	}
	id := s.nextSub
	s.nextSub++
	sub := &subscriber{ch: make(chan string, 256)}
	s.subscribers[id] = sub
	cancel = func() {
		s.mu.Lock()
		if c, ok := s.subscribers[id]; ok {
			delete(s.subscribers, id)
			close(c.ch)
		}
		s.mu.Unlock()
	}
	return buffered, sub.ch, cancel
}

func (s *Session) status() SessionStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SessionStatus{ID: s.id, State: string(s.state), Lines: s.lines}
}

func (s *Session) isFinished() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finished
}

// defaultMaxSessions bounds the retained session registry so a long-running
// daemon does not accumulate finished sessions without limit.
const defaultMaxSessions = 256

// defaultMaxPending bounds queued and running sessions independently of history.
const defaultMaxPending = 256

// SessionManager owns the live session registry and routes each session to a
// pool worker. Mirrors session-manager.js + roster.js. Finished sessions are
// retained (so a late `attach` sees history) up to MaxSessions, past which the
// oldest FINISHED ones are evicted; running/queued sessions are never evicted.
type SessionManager struct {
	pool        *Pool
	maxBuffer   int
	maxSessions int
	maxPending  int

	mu       sync.Mutex
	pending  int // queued + running; protected by mu
	sessions map[string]*Session
	order    []string // creation order, for FIFO eviction of finished sessions
}

// SessionManagerOptions configures a SessionManager.
type SessionManagerOptions struct {
	Pool      *Pool
	MaxBuffer int // per-session output ring size; 0 => default
	// MaxSessions caps the retained session registry; 0 => default. The oldest
	// FINISHED sessions are evicted once the cap is exceeded.
	MaxSessions int
	// MaxPending caps unfinished sessions (queued plus running), independently
	// of retained history. Non-positive values use the default of 256.
	MaxPending int
}

// NewSessionManager builds a manager over pool.
func NewSessionManager(opts SessionManagerOptions) (*SessionManager, error) {
	if opts.Pool == nil {
		return nil, errors.New("daemon: session manager requires a Pool")
	}
	maxSessions := opts.MaxSessions
	if maxSessions <= 0 {
		maxSessions = defaultMaxSessions
	}
	maxPending := opts.MaxPending
	if maxPending <= 0 {
		maxPending = defaultMaxPending
	}
	return &SessionManager{
		pool:        opts.Pool,
		maxBuffer:   opts.MaxBuffer,
		maxSessions: maxSessions,
		maxPending:  maxPending,
		sessions:    map[string]*Session{},
	}, nil
}

// Start creates a session for spec and dispatches it to the pool. It returns the
// Session immediately (non-blocking); the run proceeds in the background and the
// session transitions queued -> running -> done/failed. A duplicate ID is
// rejected with ErrSessionExists; a full admission limit returns
// ErrSessionOverloaded without creating a session or background goroutine.
func (m *SessionManager) Start(ctx context.Context, spec WorkerSpec) (*Session, error) {
	m.mu.Lock()
	if _, exists := m.sessions[spec.Session]; exists {
		m.mu.Unlock()
		return nil, ErrSessionExists
	}
	if err := ctx.Err(); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if m.pending >= m.maxPending {
		m.mu.Unlock()
		return nil, ErrSessionOverloaded
	}
	m.pending++
	sess := newSession(spec.Session, spec.Cwd, m.maxBuffer)
	m.sessions[spec.Session] = sess
	m.order = append(m.order, spec.Session)
	m.pruneLocked()
	m.mu.Unlock()

	go func() {
		code, err := m.pool.Run(ctx, spec, sess)
		m.mu.Lock()
		sess.finish(code, err)
		m.pending--
		m.pruneLocked()
		m.mu.Unlock()
	}()
	return sess, nil
}

// pruneLocked evicts the oldest FINISHED sessions while the registry exceeds
// maxSessions, so a long-running daemon does not retain finished sessions without
// bound. Running/queued sessions are never evicted (a transient burst can exceed
// the cap until those finish). Caller must hold m.mu.
func (m *SessionManager) pruneLocked() {
	if len(m.sessions) <= m.maxSessions {
		return
	}
	kept := m.order[:0]
	for _, id := range m.order {
		s, ok := m.sessions[id]
		if !ok {
			continue // already removed
		}
		if len(m.sessions) > m.maxSessions && s.isFinished() {
			delete(m.sessions, id)
			continue
		}
		kept = append(kept, id)
	}
	m.order = kept
}

// Get returns a session by ID.
func (m *SessionManager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}

// Attach returns the buffered history + live stream for an existing session.
func (m *SessionManager) Attach(id string) (buffered []string, live <-chan string, cancel func(), err error) {
	s, ok := m.Get(id)
	if !ok {
		return nil, nil, nil, ErrSessionNotFound
	}
	b, l, c := s.Subscribe()
	return b, l, c, nil
}

// Statuses returns a snapshot of all sessions, sorted by ID for stable output.
func (m *SessionManager) Statuses() []SessionStatus {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()
	out := make([]SessionStatus, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, s.status())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
