// Package acp implements the Agent Client Protocol (ACP) surface for ZERO: a
// JSON-RPC 2.0 peer spoken over stdio (newline-delimited JSON) so editors such
// as Zed, JetBrains, and Neovim can drive ZERO's agent core as a backend.
//
// The protocol shapes (method names, message fields) are derived solely from the
// public, Apache-licensed ACP specification at agentclientprotocol.com and the
// agentclientprotocol/agent-client-protocol repository (schema/v1). All logic
// here is written originally against ZERO's own interfaces.
package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// JSON-RPC 2.0 standard error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// rpcError is a JSON-RPC 2.0 error object.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if e == nil {
		return "<nil rpc error>"
	}
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}

// RPCError builds a protocol error to return from a handler. Use it when a
// handler needs to control the wire-level error code; any other error a handler
// returns is reported as a generic internal error.
func RPCError(code int, message string) error {
	return &rpcError{Code: code, Message: message}
}

// rpcMessage is the union of request, response, and notification frames. The
// shape is disambiguated by which fields are present (per JSON-RPC 2.0):
//   - request:      method + id
//   - notification: method, no id
//   - response:     id + (result | error), no method
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (m rpcMessage) isResponse() bool { return m.Method == "" && len(m.ID) > 0 }
func (m rpcMessage) isRequest() bool  { return m.Method != "" && len(m.ID) > 0 }
func (m rpcMessage) isNotify() bool   { return m.Method != "" && len(m.ID) == 0 }

// HandlerFunc handles an inbound request and returns the result to encode, or an
// error. Returning an *rpcError controls the wire code; any other error becomes
// a generic internal error.
type HandlerFunc func(ctx context.Context, params json.RawMessage) (any, error)

// NotifyFunc handles an inbound notification (no response is sent).
type NotifyFunc func(ctx context.Context, params json.RawMessage)

const (
	maxFrameBytes         = 64 * 1024 * 1024
	maxConcurrentRequests = 128
)

// Conn is a JSON-RPC 2.0 peer over a single ndjson stream pair. It both serves
// inbound requests/notifications (via registered handlers) and issues outbound
// requests/notifications — needed because ACP is bidirectional (the agent calls
// the client for session/request_permission, fs/*, terminal/*).
type Conn struct {
	rawReader    io.Reader // wrapped lazily in Serve, once ctx is known — see interruptibleReader
	readerCloser io.Closer
	w            io.Writer

	writeMu sync.Mutex // serializes all writes to w

	handlers  map[string]HandlerFunc
	notifiers map[string]NotifyFunc

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan rpcMessage
	closed  bool

	sem chan struct{}
	wg  sync.WaitGroup // tracks in-flight inbound handlers
}

// NewConn builds a peer reading ndjson from r and writing ndjson to w. This
// Conn does not own r: cancelling Serve's context cannot forcibly close it, so
// a Read on r that's genuinely idle (never returns on its own) leaves Serve
// blocked until r itself produces something — e.g. a test that closes r's own
// write side to unblock it. Use NewOwnedConn when this Conn should own r's
// lifetime.
func NewConn(r io.Reader, w io.Writer) *Conn {
	return &Conn{
		rawReader: r,
		w:         w,
		handlers:  make(map[string]HandlerFunc),
		notifiers: make(map[string]NotifyFunc),
		pending:   make(map[int64]chan rpcMessage),
		sem:       make(chan struct{}, maxConcurrentRequests),
	}
}

// NewOwnedConn is like NewConn but declares that this Conn exclusively owns r:
// if r implements io.Closer, Serve closes it on context cancellation to
// interrupt an otherwise-idle blocking Read. Only pass a reader whose lifetime
// this Conn should control — e.g. the process's own stdin — never a reader a
// caller retains or shares elsewhere, since cancellation tears it down
// permanently for every consumer, not just this Conn.
func NewOwnedConn(r io.Reader, w io.Writer) *Conn {
	c := NewConn(r, w)
	c.readerCloser, _ = r.(io.Closer)
	return c
}

// Handle registers a request handler for method.
func (c *Conn) Handle(method string, fn HandlerFunc) { c.handlers[method] = fn }

// HandleNotify registers a notification handler for method.
func (c *Conn) HandleNotify(method string, fn NotifyFunc) { c.notifiers[method] = fn }

// Serve runs the read loop until the stream ends, ctx is cancelled, or a fatal
// decode error occurs. Inbound requests and notifications are dispatched on
// their own goroutines so a long-running handler (e.g. session/prompt) never
// blocks the loop from delivering session/cancel or a permission response.
func (c *Conn) Serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	// On exit, cancel in-flight handlers (so a blocked outbound Call unblocks via
	// ctx) and then wait for them to finish writing their responses. Without this,
	// a finite input stream (e.g. piped ndjson that EOFs right after a request)
	// would race the dispatch goroutine and drop the response.
	defer func() {
		cancel()
		c.wg.Wait()
	}()

	interruptible := newInterruptibleReader(ctx, c.rawReader, c.readerCloser)
	reader := bufio.NewReader(interruptible)

	for {
		// One ndjson value per line. Framing per line (rather than streaming the
		// decoder) keeps a single malformed line from making the whole connection
		// unrecoverable — we report -32700 and continue.
		//
		// generation brackets this SPECIFIC ReadBytes call: bufio may invoke
		// interruptible.Read zero or more times underneath it (zero when the
		// answer is already sitting in bufio's own buffer — including a
		// previously-buffered, not-yet-surfaced error: bufio can return a
		// complete line with a nil error while quietly caching an error it
		// received ALONGSIDE that line's bytes, which then surfaces on the VERY
		// NEXT call with no further contact with interruptible.Read at all). The
		// generation only advances inside interruptible.Read's own cancellation
		// branch, so "unchanged across this call" is proof this call's outcome
		// didn't pass through that branch — whether because it was answered from
		// the buffer or because it raced ctx.Done() and won. Only that proof
		// makes it safe to call the outcome genuine.
		before := interruptible.generation()
		line, err := reader.ReadBytes('\n')
		interrupted := interruptible.generation() != before

		if len(bytes.TrimSpace(line)) > 0 {
			c.handleLine(ctx, line)
		}
		if err != nil {
			c.failAllPending(err)
			if errors.Is(err, io.EOF) || interrupted {
				return nil
			}
			return err
		}
	}
}

// interruptibleReader wraps a reader so a context cancellation can unblock a
// currently in-flight Read without ever discarding — or misattributing — the
// real outcome of a call that wasn't actually blocked.
//
// Each Read runs the underlying call on its own goroutine and races it against
// ctx.Done(). If the underlying call finishes first, its result is returned
// exactly as received; ctx being cancelled a moment later has no bearing on a
// call that already had its answer. If ctx.Done() fires first, closer (when
// non-nil) is closed to nudge a genuinely blocked read, the generation counter
// is bumped, and the call STILL waits for and returns the underlying call's own
// result — never a synthesized one — since closing a reader's outcome is
// whatever the reader itself reports for it, not ours to invent.
//
// The counter is what lets Serve tell a call that was actually raced against
// cancellation apart from one merely answered while cancellation happened to be
// pending — the distinction the read loop's own doc comment explains.
type interruptibleReader struct {
	inner  io.Reader
	closer io.Closer
	ctx    context.Context

	closeOnce sync.Once
	gen       atomic.Int64
}

func newInterruptibleReader(ctx context.Context, inner io.Reader, closer io.Closer) *interruptibleReader {
	return &interruptibleReader{inner: inner, closer: closer, ctx: ctx}
}

func (r *interruptibleReader) generation() int64 { return r.gen.Load() }

type interruptibleReadResult struct {
	n   int
	err error
}

// interruptibleReadDecision arbitrates whether an underlying Read completed or
// cancellation claimed it first. Completion is recorded before the result is
// published so cancellation cannot misclassify a finished read merely because
// its goroutine has not sent the result yet.
type interruptibleReadDecision struct {
	once sync.Once
}

func (d *interruptibleReadDecision) complete() { d.once.Do(func() {}) }

func (d *interruptibleReadDecision) interrupt() (claimed bool) {
	d.once.Do(func() { claimed = true })
	return claimed
}

// readInner records completion in the Read call's deferred epilogue. It also
// converts a reader panic into a terminal error so the helper goroutine always
// publishes a result for Read to join.
func (r *interruptibleReader) readInner(p []byte, decision *interruptibleReadDecision) (res interruptibleReadResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			res = interruptibleReadResult{err: fmt.Errorf("acp: reader panicked: %v", recovered)}
		}
		decision.complete()
	}()
	res.n, res.err = r.inner.Read(p)
	return res
}

func (r *interruptibleReader) Read(p []byte) (int, error) {
	resultCh := make(chan interruptibleReadResult, 1)
	var decision interruptibleReadDecision
	go func() {
		resultCh <- r.readInner(p, &decision)
	}()
	select {
	case res := <-resultCh:
		return res.n, res.err
	case <-r.ctx.Done():
		// resultCh may not be readable yet even when inner.Read has returned: its
		// goroutine can be descheduled between recording completion and publishing
		// the result. Only interrupt a call that cancellation actually claims.
		if decision.interrupt() {
			r.gen.Add(1)
			if r.closer != nil {
				r.closeOnce.Do(func() { _ = r.closer.Close() })
			}
		}
		// Wait for the real result rather than returning synthetically: p is
		// still being written by the goroutine above until it does, so returning
		// early would race that write, and the actual error (whatever closing
		// the reader produced, or whatever the reader was already about to
		// report) is what the caller should see.
		res := <-resultCh
		return res.n, res.err
	}
}

// handleLine decodes and dispatches one ndjson frame. A parse failure replies
// with -32700 (id null) and keeps the connection alive.
func (c *Conn) handleLine(ctx context.Context, line []byte) {
	var msg rpcMessage
	if err := json.Unmarshal(bytes.TrimSpace(line), &msg); err != nil {
		c.writeError(json.RawMessage("null"), &rpcError{Code: codeParseError, Message: "parse error"})
		return
	}
	if msg.JSONRPC != "" && msg.JSONRPC != "2.0" {
		if len(msg.ID) > 0 {
			c.writeError(msg.ID, &rpcError{Code: codeInvalidRequest, Message: "unsupported jsonrpc version"})
		}
		return
	}
	switch {
	case msg.isResponse():
		c.deliver(msg)
	case msg.isRequest():
		c.wg.Add(1)
		c.acquireSem(ctx)
		go func(m rpcMessage) {
			defer c.releaseSem()
			defer c.wg.Done()
			c.dispatchRequest(ctx, m)
		}(msg)
	case msg.isNotify():
		if fn := c.notifiers[msg.Method]; fn != nil {
			c.wg.Add(1)
			c.acquireSem(ctx)
			go func(m rpcMessage) {
				defer c.releaseSem()
				defer c.wg.Done()
				fn(ctx, m.Params)
			}(msg)
		}
	default:
		// Malformed frame; reply only if we can identify a request id.
		if len(msg.ID) > 0 {
			c.writeError(msg.ID, &rpcError{Code: codeInvalidRequest, Message: "invalid request"})
		}
	}
}

func (c *Conn) acquireSem(ctx context.Context) {
	if c.sem == nil {
		return
	}
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
	}
}

func (c *Conn) releaseSem() {
	if c.sem == nil {
		return
	}
	select {
	case <-c.sem:
	default:
	}
}

func (c *Conn) dispatchRequest(ctx context.Context, msg rpcMessage) {
	fn := c.handlers[msg.Method]
	if fn == nil {
		c.writeError(msg.ID, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + msg.Method})
		return
	}
	result, err := fn(ctx, msg.Params)
	if err != nil {
		var re *rpcError
		if errors.As(err, &re) {
			c.writeError(msg.ID, re)
		} else {
			c.writeError(msg.ID, &rpcError{Code: codeInternalError, Message: err.Error()})
		}
		return
	}
	c.writeResult(msg.ID, result)
}

// Call issues an outbound request and blocks until the response arrives, ctx is
// cancelled, or the connection closes.
func (c *Conn) Call(ctx context.Context, method string, params any, result any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("acp: connection closed")
	}
	c.nextID++
	id := c.nextID
	ch := make(chan rpcMessage, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	idRaw, _ := json.Marshal(id)
	if err := c.write(rpcMessage{JSONRPC: "2.0", ID: idRaw, Method: method, Params: raw}); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Error
		}
		if result != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, result)
		}
		return nil
	}
}

// Notify issues an outbound notification (no response expected).
func (c *Conn) Notify(method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	return c.write(rpcMessage{JSONRPC: "2.0", Method: method, Params: raw})
}

func (c *Conn) deliver(msg rpcMessage) {
	var id int64
	if err := json.Unmarshal(msg.ID, &id); err != nil {
		return
	}
	c.mu.Lock()
	ch := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if ch == nil {
		return
	}
	// Non-blocking: the pending channel is cap-1 and may have been abandoned (e.g.
	// a Call cancelled by session/cancel). A duplicate or late response frame must
	// never block the read-loop goroutine.
	select {
	case ch <- msg:
	default:
	}
}

func (c *Conn) failAllPending(err error) {
	c.mu.Lock()
	c.closed = true
	pending := c.pending
	c.pending = make(map[int64]chan rpcMessage)
	c.mu.Unlock()
	for _, ch := range pending {
		select {
		case ch <- rpcMessage{Error: &rpcError{Code: codeInternalError, Message: err.Error()}}:
		default:
		}
	}
}

func (c *Conn) writeResult(id json.RawMessage, result any) {
	raw, err := json.Marshal(result)
	if err != nil {
		c.writeError(id, &rpcError{Code: codeInternalError, Message: err.Error()})
		return
	}
	_ = c.write(rpcMessage{JSONRPC: "2.0", ID: id, Result: raw})
}

func (c *Conn) writeError(id json.RawMessage, e *rpcError) {
	_ = c.write(rpcMessage{JSONRPC: "2.0", ID: id, Error: e})
}

func (c *Conn) write(msg rpcMessage) error {
	msg.JSONRPC = "2.0"
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.w.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

// marshalParams encodes params, treating nil as an absent params field.
func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	if raw, ok := params.(json.RawMessage); ok {
		return raw, nil
	}
	return json.Marshal(params)
}
