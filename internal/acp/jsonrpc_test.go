package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testReadCloser struct {
	read  func([]byte) (int, error)
	close func() error
}

func (r testReadCloser) Read(p []byte) (int, error) { return r.read(p) }
func (r testReadCloser) Close() error               { return r.close() }

type testReader func([]byte) (int, error)

func (r testReader) Read(p []byte) (int, error) { return r(p) }

type testWriter func([]byte) (int, error)

func (w testWriter) Write(p []byte) (int, error) { return w(p) }

// connPair wires two Conns together over in-memory pipes and serves both.
func connPair(t *testing.T) (a, b *Conn, stop func()) {
	t.Helper()
	ar, bw := io.Pipe() // b -> a
	br, aw := io.Pipe() // a -> b
	a = NewConn(ar, aw)
	b = NewConn(br, bw)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = a.Serve(ctx) }()
	go func() { _ = b.Serve(ctx) }()
	return a, b, func() {
		cancel()
		_ = aw.Close()
		_ = bw.Close()
	}
}

func TestConnServeCancellationInterruptsIdleRead(t *testing.T) {
	pipeReader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	// Buffered + non-blocking send, not close: interruptibleReader retries each
	// underlying Read on its own goroutine, so if bufio ever calls this more than
	// once, a close() here would panic on the second call.
	readStarted := make(chan struct{}, 1)
	closeCalls := make(chan struct{}, 2)
	reader := testReadCloser{
		read: func(p []byte) (int, error) {
			select {
			case readStarted <- struct{}{}:
			default:
			}
			return pipeReader.Read(p)
		},
		close: func() error {
			closeCalls <- struct{}{}
			return pipeReader.Close()
		},
	}
	conn := NewOwnedConn(reader, io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- conn.Serve(ctx) }()

	<-readStarted
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v after cancellation, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not return after cancelling an idle connection")
	}
	if got := len(closeCalls); got != 1 {
		t.Fatalf("reader Close calls = %d, want 1", got)
	}
}

func TestConnServePreservesTerminalReadErrorDuringCancellation(t *testing.T) {
	for _, closable := range []bool{false, true} {
		name := "non-closable"
		if closable {
			name = "closable"
		}
		t.Run(name, func(t *testing.T) {
			wantErr := errors.New("read failed")
			read := testReader(func(p []byte) (int, error) {
				return copy(p, "not json"), wantErr
			})
			closeCalls := make(chan struct{}, 1)
			var reader io.Reader = read
			if closable {
				reader = testReadCloser{
					read: read,
					close: func() error {
						closeCalls <- struct{}{}
						return nil
					},
				}
			}
			// Buffered + non-blocking send, not close: a later change adding a
			// second write (e.g. an additional error frame) must not panic on a
			// repeated close of an already-closed channel.
			writeStarted := make(chan struct{}, 1)
			releaseWrite := make(chan struct{})
			writer := testWriter(func(p []byte) (int, error) {
				select {
				case writeStarted <- struct{}{}:
				default:
				}
				<-releaseWrite
				return len(p), nil
			})
			newConn := NewConn
			if closable {
				// Ownership matters here: this case exists to prove that even a
				// Conn entitled to close its reader on cancellation still
				// preserves a terminal error that was already decided.
				newConn = NewOwnedConn
			}
			conn := newConn(reader, writer)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- conn.Serve(ctx) }()

			<-writeStarted
			cancel()
			close(releaseWrite)
			select {
			case err := <-done:
				if !errors.Is(err, wantErr) {
					t.Fatalf("Serve returned %v, want terminal read error %v", err, wantErr)
				}
			case <-time.After(time.Second):
				t.Fatal("Serve did not return after terminal read error")
			}
			if got := len(closeCalls); got != 0 {
				t.Fatalf("reader Close calls = %d, want 0 after terminal read", got)
			}
		})
	}
}

// TestConnServePreservesReadErrorBufferedAlongsideAPriorLine is the deterministic
// regression test for jatmn's #782 finding: a real, non-EOF ReadBytes failure can
// be DECIDED before cancellation ever happens, yet only SURFACE afterward, and
// must still be reported rather than swallowed as a clean shutdown.
//
// bufio.Reader can obtain data AND a terminal error from ONE underlying Read call
// (io.Reader explicitly permits returning (n>0, err) together). If a delimiter is
// found within that data, ReadBytes returns the line with a nil error for THIS
// call, but bufio caches the error internally and returns it — WITHOUT calling the
// underlying reader again — on the very next ReadBytes call. This test forces that
// exact sequence: the underlying reader hands back one complete, validly-framed
// line together with wantErr in a SINGLE call (asserted below to be its ONLY
// call), the loop dispatches that line through a synchronous path that blocks the
// read loop, cancellation is asserted to have happened before the loop attempts
// its next read, and only then is the block released. By the time the read loop
// asks for more input, cancellation is already in effect and the underlying
// reader is never touched again — so if Serve attributed the resulting error to
// cancellation merely because ctx was already done, it would incorrectly return
// nil. It must instead recognize that this read was answered entirely from
// bufio's own cache, never raced against ctx.Done(), and report wantErr.
func TestConnServePreservesReadErrorBufferedAlongsideAPriorLine(t *testing.T) {
	wantErr := errors.New("read failed")
	var readCalls atomic.Int32
	read := testReader(func(p []byte) (int, error) {
		readCalls.Add(1)
		// An unsupported jsonrpc version with an id takes the SYNCHRONOUS
		// writeError path in handleLine (not a dispatched goroutine), giving this
		// test a reliable blocking point between processing this line and the
		// loop's next ReadBytes call.
		return copy(p, `{"jsonrpc":"1.0","id":1}`+"\n"), wantErr
	})
	writeStarted := make(chan struct{}, 1)
	releaseWrite := make(chan struct{})
	var releaseWriteOnce sync.Once
	release := func() { releaseWriteOnce.Do(func() { close(releaseWrite) }) }
	t.Cleanup(release)
	writer := testWriter(func(p []byte) (int, error) {
		select {
		case writeStarted <- struct{}{}:
		default:
		}
		<-releaseWrite
		return len(p), nil
	})
	conn := NewConn(read, writer)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- conn.Serve(ctx) }()

	// The synchronous write confirms the first (and, per the panic guard above,
	// only) read has already returned and is being processed — cancelling now
	// lands squarely in the gap between that read and the loop's next one, never
	// racing an in-flight interruptibleReader.Read call.
	select {
	case <-writeStarted:
	case <-time.After(time.Second):
		t.Fatal("Serve did not reach the synchronous write")
	}
	cancel()
	release()

	select {
	case err := <-done:
		if !errors.Is(err, wantErr) {
			t.Fatalf("Serve returned %v, want the buffered terminal read error %v", err, wantErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not return after the buffered terminal read error")
	}
	if got := readCalls.Load(); got != 1 {
		t.Fatalf("underlying Read calls = %d, want exactly 1 (bufio must have served the second ReadBytes from its cache)", got)
	}
}

// TestInterruptibleReaderPrefersDecidedResultOverCancellation is the regression
// test for jatmn's second #782 finding: interruptibleReader.Read's select
// between resultCh and ctx.Done() chooses pseudo-randomly when both are ready,
// so a real, already-decided outcome (e.g. a terminal transport error) could be
// misattributed to cancellation purely because ctx also happened to be done by
// the time the select ran. Each iteration lets the underlying Read return (so
// resultCh already holds the real outcome) before cancelling, maximizing the
// odds of landing exactly in that ambiguous window; the generation counter must
// never advance when the real, already-decided result is what gets returned.
func TestInterruptibleReaderPrefersDecidedResultOverCancellation(t *testing.T) {
	wantErr := errors.New("read failed")
	for i := 0; i < 100; i++ {
		resultReady := make(chan struct{})
		read := testReader(func(p []byte) (int, error) {
			defer close(resultReady)
			return copy(p, "x"), wantErr
		})
		ctx, cancel := context.WithCancel(context.Background())
		r := newInterruptibleReader(ctx, read, nil)

		readDone := make(chan interruptibleReadResult, 1)
		go func() {
			n, err := r.Read(make([]byte, 8))
			readDone <- interruptibleReadResult{n, err}
		}()

		<-resultReady
		time.Sleep(time.Millisecond) // bias resultCh's send ahead of cancel
		cancel()

		res := <-readDone
		if !errors.Is(res.err, wantErr) {
			t.Fatalf("iteration %d: Read returned %v, want %v", i, res.err, wantErr)
		}
		if got := r.generation(); got != 0 {
			t.Fatalf("iteration %d: generation = %d, want 0 (a decided result must never be attributed to cancellation)", i, got)
		}
		cancel()
	}
}

// TestInterruptibleReadCompletionWinsBeforeResultPublication exercises the
// actual inner-read wrapper at the handoff between the underlying Read and the
// helper goroutine's result publication. Cancellation must not reclassify a
// result returned by this wrapper as interrupted.
func TestInterruptibleReadCompletionWinsBeforeResultPublication(t *testing.T) {
	wantErr := errors.New("read failed")
	read := testReader(func(p []byte) (int, error) {
		return copy(p, "x"), wantErr
	})
	r := newInterruptibleReader(context.Background(), read, nil)
	var decision interruptibleReadDecision
	readComplete := make(chan struct{})
	resultCh := make(chan interruptibleReadResult)
	go func() {
		res := r.readInner(make([]byte, 8), &decision)
		close(readComplete)
		resultCh <- res
	}()

	<-readComplete
	if decision.interrupt() {
		t.Fatal("cancellation claimed a read that completed before result publication")
	}
	res := <-resultCh
	if res.n != 1 || !errors.Is(res.err, wantErr) {
		t.Fatalf("inner read result = (%d, %v), want (1, %v)", res.n, res.err, wantErr)
	}
}

func TestConnServeReportsReaderPanicAsTerminalError(t *testing.T) {
	read := testReader(func([]byte) (int, error) {
		panic("boom")
	})
	conn := NewConn(read, io.Discard)
	done := make(chan error, 1)
	go func() { done <- conn.Serve(context.Background()) }()

	select {
	case err := <-done:
		const want = "acp: reader panicked: boom"
		if err == nil || err.Error() != want {
			t.Fatalf("Serve returned %v, want %q", err, want)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not return after reader panic")
	}
}

func TestConnRequestResponse(t *testing.T) {
	a, b, stop := connPair(t)
	defer stop()

	b.Handle("add", func(_ context.Context, params json.RawMessage) (any, error) {
		var in struct{ X, Y int }
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, RPCError(codeInvalidParams, "bad params")
		}
		return map[string]int{"sum": in.X + in.Y}, nil
	})

	var out struct{ Sum int }
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := a.Call(ctx, "add", map[string]int{"X": 2, "Y": 3}, &out); err != nil {
		t.Fatalf("call: %v", err)
	}
	if out.Sum != 5 {
		t.Fatalf("sum = %d, want 5", out.Sum)
	}
}

func TestConnNotification(t *testing.T) {
	a, b, stop := connPair(t)
	defer stop()

	got := make(chan string, 1)
	b.HandleNotify("ping", func(_ context.Context, params json.RawMessage) {
		var in struct{ Msg string }
		_ = json.Unmarshal(params, &in)
		got <- in.Msg
	})

	if err := a.Notify("ping", map[string]string{"Msg": "hello"}); err != nil {
		t.Fatalf("notify: %v", err)
	}
	select {
	case msg := <-got:
		if msg != "hello" {
			t.Fatalf("got %q, want hello", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("notification not delivered")
	}
}

func TestConnNotificationsForOneMethodStayInWireOrder(t *testing.T) {
	a, b, stop := connPair(t)
	defer stop()

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	delivered := make(chan int, 2)
	b.HandleNotify("update", func(_ context.Context, params json.RawMessage) {
		var in struct{ Sequence int }
		_ = json.Unmarshal(params, &in)
		if in.Sequence == 1 {
			close(firstStarted)
			<-releaseFirst
		}
		delivered <- in.Sequence
	})

	if err := a.Notify("update", map[string]int{"Sequence": 1}); err != nil {
		t.Fatalf("first notify: %v", err)
	}
	select {
	case <-firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first notification did not start")
	}
	if err := a.Notify("update", map[string]int{"Sequence": 2}); err != nil {
		t.Fatalf("second notify: %v", err)
	}
	select {
	case sequence := <-delivered:
		t.Fatalf("notification %d overtook the blocked first notification", sequence)
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseFirst)
	for want := 1; want <= 2; want++ {
		select {
		case got := <-delivered:
			if got != want {
				t.Fatalf("notification order = ...,%d; want %d", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("notification %d was not delivered", want)
		}
	}
}

func TestConnDifferentNotificationMethodsStayConcurrent(t *testing.T) {
	a, b, stop := connPair(t)
	defer stop()

	updateStarted := make(chan struct{})
	releaseUpdate := make(chan struct{})
	cancelDelivered := make(chan struct{})
	b.HandleNotify("update", func(context.Context, json.RawMessage) {
		close(updateStarted)
		<-releaseUpdate
	})
	b.HandleNotify("cancel", func(context.Context, json.RawMessage) {
		close(cancelDelivered)
	})

	if err := a.Notify("update", nil); err != nil {
		t.Fatalf("update notify: %v", err)
	}
	select {
	case <-updateStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("update notification did not start")
	}
	if err := a.Notify("cancel", nil); err != nil {
		t.Fatalf("cancel notify: %v", err)
	}
	select {
	case <-cancelDelivered:
	case <-time.After(2 * time.Second):
		t.Fatal("different notification method was blocked behind update")
	}
	close(releaseUpdate)
}

func TestConnMethodNotFound(t *testing.T) {
	a, _, stop := connPair(t)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := a.Call(ctx, "does_not_exist", nil, nil)
	var re *rpcError
	if !asRPCError(err, &re) {
		t.Fatalf("expected rpcError, got %v", err)
	}
	if re.Code != codeMethodNotFound {
		t.Fatalf("code = %d, want %d", re.Code, codeMethodNotFound)
	}
}

func TestConnHandlerError(t *testing.T) {
	a, b, stop := connPair(t)
	defer stop()
	b.Handle("boom", func(_ context.Context, _ json.RawMessage) (any, error) {
		return nil, RPCError(codeInvalidParams, "nope")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := a.Call(ctx, "boom", nil, nil)
	var re *rpcError
	if !asRPCError(err, &re) || re.Code != codeInvalidParams {
		t.Fatalf("expected invalid-params rpcError, got %v", err)
	}
}

// TestConnBidirectionalDuringHandler proves that while one peer is inside a
// request handler it can issue an outbound request back to the caller and the
// caller answers it — exactly the session/prompt -> session/request_permission
// pattern. If the read loop blocked on the handler, this would deadlock.
func TestConnBidirectionalDuringHandler(t *testing.T) {
	a, b, stop := connPair(t)
	defer stop()

	// a answers an "approve?" callback.
	a.Handle("approve", func(_ context.Context, _ json.RawMessage) (any, error) {
		return map[string]bool{"ok": true}, nil
	})

	// b's "run" handler calls back to a mid-flight.
	b.Handle("run", func(ctx context.Context, _ json.RawMessage) (any, error) {
		var approval struct{ OK bool }
		if err := b.Call(ctx, "approve", nil, &approval); err != nil {
			return nil, err
		}
		return map[string]bool{"ran": approval.OK}, nil
	})

	var out struct{ Ran bool }
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := a.Call(ctx, "run", nil, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !out.Ran {
		t.Fatal("expected ran=true via mid-handler callback")
	}
}

// TestConnSurvivesMalformedLine proves a single bad ndjson line yields a -32700
// and does NOT tear down the connection — a following valid request still works.
func TestConnSurvivesMalformedLine(t *testing.T) {
	clientR, serverW := io.Pipe() // server -> client
	serverR, clientW := io.Pipe() // client -> server
	server := NewConn(serverR, serverW)
	server.Handle("ping", func(_ context.Context, _ json.RawMessage) (any, error) { return "pong", nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		_ = serverW.Close()
		_ = clientW.Close()
	}()
	go func() { _ = server.Serve(ctx) }()

	go func() {
		_, _ = clientW.Write([]byte("this is not json\n"))
		_, _ = clientW.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"))
	}()

	dec := json.NewDecoder(clientR)
	var sawParseError, sawPong bool
	for i := 0; i < 2; i++ {
		var msg struct {
			Result any `json:"result"`
			Error  *struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		done := make(chan error, 1)
		go func() { done <- dec.Decode(&msg) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("decode response %d: %v", i, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for a response")
		}
		if msg.Error != nil && msg.Error.Code == codeParseError {
			sawParseError = true
		}
		if r, ok := msg.Result.(string); ok && r == "pong" {
			sawPong = true
		}
	}
	if !sawParseError {
		t.Error("expected a -32700 parse error for the malformed line")
	}
	if !sawPong {
		t.Error("expected the valid request to still be answered (connection survived the bad line)")
	}
}

func asRPCError(err error, target **rpcError) bool {
	re, ok := err.(*rpcError)
	if ok {
		*target = re
	}
	return ok
}
