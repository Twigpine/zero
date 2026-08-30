package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
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

func TestConnSaturatedRequestsReturnsServerBusy(t *testing.T) {
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	defer inWriter.Close()
	defer outReader.Close()

	conn := NewConn(inReader, outWriter)
	// Use a small 2-slot semaphore for test
	conn.sem = make(chan struct{}, 2)

	handlerBlock := make(chan struct{})
	defer close(handlerBlock)

	conn.Handle("slow", func(ctx context.Context, params json.RawMessage) (any, error) {
		<-handlerBlock
		return "ok", nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = conn.Serve(ctx) }()

	// Send 2 requests that fill semaphore slots
	_, _ = inWriter.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"slow","params":{}}` + "\n"))
	_, _ = inWriter.Write([]byte(`{"jsonrpc":"2.0","id":2,"method":"slow","params":{}}` + "\n"))

	deadline := time.Now().Add(2 * time.Second)
	for len(conn.sem) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for semaphore saturation, len = %d", len(conn.sem))
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Send 3rd request while pool is saturated
	_, _ = inWriter.Write([]byte(`{"jsonrpc":"2.0","id":3,"method":"slow","params":{}}` + "\n"))

	scanner := bufio.NewScanner(outReader)
	if !scanner.Scan() {
		t.Fatalf("expected response line, got none: %v", scanner.Err())
	}
	var resp rpcMessage
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != codeServerBusy {
		t.Fatalf("resp.Error = %#v, want code %d (Server Busy)", resp.Error, codeServerBusy)
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

// TestCancelNotificationDeliveredDuringHandlerSaturation proves that when all 128
// request handler slots are occupied by blocking work, an inbound notification
// (such as session/cancel) is still delivered and executed immediately rather than
// being dropped, blocked, or starved behind saturated request workers.
func TestCancelNotificationDeliveredDuringHandlerSaturation(t *testing.T) {
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()

	server := NewConn(serverR, serverW)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		_ = clientR.Close()
		_ = serverW.Close()
		_ = clientW.Close()
	}()

	releaseHandlers := make(chan struct{})
	handlerStarted := make(chan struct{}, maxConcurrentRequests)

	server.Handle("block", func(ctx context.Context, _ json.RawMessage) (any, error) {
		handlerStarted <- struct{}{}
		select {
		case <-releaseHandlers:
			return "ok", nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})

	cancelDelivered := make(chan struct{}, 1)
	server.HandleNotify("session/cancel", func(_ context.Context, _ json.RawMessage) {
		cancelDelivered <- struct{}{}
		close(releaseHandlers)
	})

	go func() { _ = server.Serve(ctx) }()

	// 1. Fill all 128 request slots
	for i := 1; i <= maxConcurrentRequests; i++ {
		req := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"block"}`+"\n", i)
		_, _ = clientW.Write([]byte(req))
	}

	for i := 0; i < maxConcurrentRequests; i++ {
		select {
		case <-handlerStarted:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for handler %d to start", i)
		}
	}

	// 2. Send notification while all request slots are occupied
	notify := `{"jsonrpc":"2.0","method":"session/cancel","params":{}}` + "\n"
	_, _ = clientW.Write([]byte(notify))

	// 3. Verify notification is delivered without delay
	select {
	case <-cancelDelivered:
		// Success: notification bypassed request throttling and freed the handlers
	case <-time.After(2 * time.Second):
		t.Fatal("cancel notification was dropped or blocked by saturated request handlers")
	}
}

// TestReadNDJSONFrameRejectsOversizedTerminatedFrame proves that frames exceeding
// the configured frame limit are rejected with an error rather than buffered unboundedly.
func TestReadNDJSONFrameRejectsOversizedTerminatedFrame(t *testing.T) {
	serverR, clientW := io.Pipe()

	server := NewConn(serverR, io.Discard)
	server.frameLimit = 64 // 64 bytes frame limit for test injection
	var invoked atomic.Bool
	server.Handle("ping", func(_ context.Context, _ json.RawMessage) (any, error) {
		invoked.Store(true)
		return "pong", nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		_ = clientW.Close()
	}()

	errCh := make(chan error, 1)
	go func() { errCh <- server.Serve(ctx) }()

	// Valid request padded with JSON whitespace past the limit: dispatch would
	// run ping unless the frame-limit error is checked before handleLine.
	oversized := `{"jsonrpc":"2.0","id":1,"method":"ping"}` + strings.Repeat(" ", 64) + "\n"
	_, _ = clientW.Write([]byte(oversized))

	select {
	case err := <-errCh:
		if err == nil || !errors.Is(err, errFrameTooLarge) {
			t.Fatalf("expected frame limit error, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Serve to reject oversized frame")
	}
	if invoked.Load() {
		t.Fatal("handler invoked for an oversized frame")
	}
}

type stallWriter struct {
	started chan struct{}
	gate    chan struct{}
	once    sync.Once
}

func (w *stallWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.gate
	return len(p), nil
}

func TestSaturatedBusyWriteDoesNotBlockCancelNotification(t *testing.T) {
	inReader, inWriter := io.Pipe()
	defer inWriter.Close()

	started := make(chan struct{})
	gate := make(chan struct{})
	defer close(gate)
	conn := NewConn(inReader, &stallWriter{started: started, gate: gate})
	conn.sem = make(chan struct{}, 2)

	handlerStarted := make(chan struct{}, 2)
	conn.Handle("slow", func(ctx context.Context, _ json.RawMessage) (any, error) {
		handlerStarted <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	cancelSeen := make(chan struct{})
	conn.HandleNotify("session/cancel", func(_ context.Context, _ json.RawMessage) {
		close(cancelSeen)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = conn.Serve(ctx) }()

	_, _ = inWriter.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"slow"}` + "\n"))
	_, _ = inWriter.Write([]byte(`{"jsonrpc":"2.0","id":2,"method":"slow"}` + "\n"))
	for i := 0; i < 2; i++ {
		select {
		case <-handlerStarted:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for slow handler %d", i)
		}
	}

	_, _ = inWriter.Write([]byte(`{"jsonrpc":"2.0","id":3,"method":"slow"}` + "\n"))
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for busy write to start")
	}

	_, _ = inWriter.Write([]byte(`{"jsonrpc":"2.0","method":"session/cancel","params":{}}` + "\n"))
	select {
	case <-cancelSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel notification stalled behind a blocked busy write")
	}
}

func TestStalledBusyRepliesStayBoundedAndServeExits(t *testing.T) {
	inReader, inWriter := io.Pipe()
	defer inWriter.Close()

	started := make(chan struct{})
	gate := make(chan struct{})
	defer close(gate)
	conn := NewConn(inReader, &stallWriter{started: started, gate: gate})
	conn.sem = make(chan struct{}, 2)

	handlerStarted := make(chan struct{}, 2)
	conn.Handle("slow", func(ctx context.Context, _ json.RawMessage) (any, error) {
		handlerStarted <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- conn.Serve(ctx) }()

	_, _ = inWriter.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"slow"}` + "\n"))
	_, _ = inWriter.Write([]byte(`{"jsonrpc":"2.0","id":2,"method":"slow"}` + "\n"))
	for i := 0; i < 2; i++ {
		select {
		case <-handlerStarted:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for slow handler %d", i)
		}
	}

	_, _ = inWriter.Write([]byte(`{"jsonrpc":"2.0","id":3,"method":"slow"}` + "\n"))
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for busy write to stall")
	}

	before := runtime.NumGoroutine()
	const extra = 50
	go func() {
		for i := 0; i < extra; i++ {
			req := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"slow"}`+"\n", i+10)
			_, _ = inWriter.Write([]byte(req))
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not exit after the busy-reply queue overflowed")
	}
	settle := time.Now().Add(2 * time.Second)
	delta := runtime.NumGoroutine() - before
	for delta > 8 && time.Now().Before(settle) {
		time.Sleep(10 * time.Millisecond)
		delta = runtime.NumGoroutine() - before
	}
	if delta > 8 {
		t.Fatalf("goroutine growth = %d after %d rejected requests, want bounded", delta, extra)
	}
}

func TestServeEOFStillWritesInFlightResponse(t *testing.T) {
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	defer outWriter.Close()

	conn := NewConn(inReader, outWriter)
	started := make(chan struct{})
	conn.Handle("echo", func(ctx context.Context, _ json.RawMessage) (any, error) {
		close(started)
		<-ctx.Done()
		return map[string]string{"ok": "yes"}, nil
	})

	errCh := make(chan error, 1)
	go func() { errCh <- conn.Serve(context.Background()) }()
	if _, err := inWriter.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"echo"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not start")
	}
	if err := inWriter.Close(); err != nil {
		t.Fatal(err)
	}

	got := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(outReader)
		if scanner.Scan() {
			got <- scanner.Text()
		}
	}()
	select {
	case line := <-got:
		if !strings.Contains(line, `"ok":"yes"`) && !strings.Contains(line, `"ok": "yes"`) {
			t.Fatalf("missing in-flight response: %s", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve dropped the in-flight response at EOF")
	}
}

func TestCallCancelsWhileWriterHoldsWriteMu(t *testing.T) {
	inReader, inWriter := io.Pipe()
	defer inWriter.Close()

	started := make(chan struct{})
	gate := make(chan struct{})
	defer close(gate)
	conn := NewConn(inReader, &stallWriter{started: started, gate: gate})

	serveCtx, serveCancel := context.WithCancel(context.Background())
	defer serveCancel()
	go func() { _ = conn.Serve(serveCtx) }()

	go func() { _ = conn.Call(context.Background(), "ping", nil, nil) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first write did not stall")
	}

	callCtx, callCancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- conn.Call(callCtx, "ping", nil, nil) }()
	time.Sleep(50 * time.Millisecond)
	callCancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Call returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not return after context cancel while writeMu was held")
	}
}

func TestOverloadUnblocksHandlerWaitingOnStalledWriter(t *testing.T) {
	inReader, inWriter := io.Pipe()
	defer inWriter.Close()

	started := make(chan struct{})
	gate := make(chan struct{})
	defer close(gate)
	conn := NewConn(inReader, &stallWriter{started: started, gate: gate})
	conn.sem = make(chan struct{}, 1)

	handlerStarted := make(chan struct{})
	release := make(chan struct{})
	conn.Handle("slow", func(ctx context.Context, _ json.RawMessage) (any, error) {
		close(handlerStarted)
		<-release
		return "ok", nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- conn.Serve(ctx) }()

	_, _ = inWriter.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"slow"}` + "\n"))
	select {
	case <-handlerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("admitted handler did not start")
	}

	_, _ = inWriter.Write([]byte(`{"jsonrpc":"2.0","id":2,"method":"slow"}` + "\n"))
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for busy write to stall")
	}

	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for conn.writeWaiters.Load() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("admitted handler did not block behind the stalled writer")
		}
		time.Sleep(5 * time.Millisecond)
	}

	_, _ = inWriter.Write([]byte(`{"jsonrpc":"2.0","id":3,"method":"slow"}` + "\n"))
	_, _ = inWriter.Write([]byte(`{"jsonrpc":"2.0","id":4,"method":"slow"}` + "\n"))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not exit on overload while a handler waited behind the stalled writer")
	}
}

func TestNotificationFloodStaysBoundedWhileSaturatedRequestStillCancels(t *testing.T) {
	inReader, inWriter := io.Pipe()
	defer inWriter.Close()

	conn := NewConn(inReader, io.Discard)
	conn.sem = make(chan struct{}, 1)

	started := make(chan struct{})
	unblocked := make(chan struct{})
	conn.Handle("slow", func(ctx context.Context, _ json.RawMessage) (any, error) {
		close(started)
		select {
		case <-unblocked:
			return "ok", nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})

	entered := make(chan struct{}, 1)
	hold := make(chan struct{})
	var inFlight atomic.Int64
	conn.HandleNotify("session/cancel", func(_ context.Context, _ json.RawMessage) {
		inFlight.Add(1)
		select {
		case entered <- struct{}{}:
		default:
		}
		<-hold
		select {
		case <-unblocked:
		default:
			close(unblocked)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = conn.Serve(ctx) }()

	_, _ = inWriter.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"slow"}` + "\n"))
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("saturated request did not start")
	}

	_, _ = inWriter.Write([]byte(`{"jsonrpc":"2.0","method":"session/cancel","params":{}}` + "\n"))
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel notification was not delivered while the request slot was full")
	}

	before := runtime.NumGoroutine()
	const flood = 200
	for i := 0; i < flood; i++ {
		_, _ = inWriter.Write([]byte(`{"jsonrpc":"2.0","method":"session/cancel","params":{}}` + "\n"))
	}

	settle := time.Now().Add(2 * time.Second)
	var delta int
	for {
		delta = runtime.NumGoroutine() - before
		got := inFlight.Load()
		if got <= 2 && delta <= 8 {
			break
		}
		if time.Now().After(settle) {
			t.Fatalf("notification flood in-flight=%d goroutine growth=%d, want bounded", got, delta)
		}
		time.Sleep(10 * time.Millisecond)
	}

	close(hold)
	select {
	case <-unblocked:
	case <-time.After(2 * time.Second):
		t.Fatal("saturated request was not cancelled after coalesced session/cancel")
	}
}

type gatedRecorder struct {
	started chan struct{}
	gate    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	got     string
}

func (w *gatedRecorder) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.gate
	w.mu.Lock()
	defer w.mu.Unlock()
	w.got += string(p)
	return len(p), nil
}

func (w *gatedRecorder) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.got
}

type closeUnblocksWrite struct {
	once   sync.Once
	closed chan struct{}
}

func (w *closeUnblocksWrite) Write([]byte) (int, error) {
	<-w.closed
	return 0, io.ErrClosedPipe
}

func (w *closeUnblocksWrite) Close() error {
	w.once.Do(func() { close(w.closed) })
	return nil
}

func TestOverloadBlockedWriteServeExits(t *testing.T) {
	inReader, inWriter := io.Pipe()
	defer inWriter.Close()
	out := &closeUnblocksWrite{closed: make(chan struct{})}
	conn := NewOwnedConn(inReader, out)
	conn.sem = make(chan struct{}, 1)

	started := make(chan struct{})
	conn.Handle("slow", func(ctx context.Context, _ json.RawMessage) (any, error) {
		close(started)
		<-ctx.Done()
		return map[string]any{"ok": true}, nil
	})

	done := make(chan error, 1)
	go func() { done <- conn.Serve(context.Background()) }()

	if _, err := inWriter.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"slow"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not start")
	}
	if _, err := inWriter.Write([]byte(`{"jsonrpc":"2.0","id":2,"method":"slow"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int{3, 4} {
		frame := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"slow"}`+"\n", id)
		if _, err := inWriter.Write([]byte(frame)); err != nil {
			t.Fatal(err)
		}
	}

	select {
	case err := <-done:
		if !errors.Is(err, errBusyOverload) {
			t.Fatalf("Serve = %v, want %v", err, errBusyOverload)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve must return after closing the writer; do not hang on Write")
	}
}

func TestQueuedBusyReplySurvivesOverloadBurst(t *testing.T) {
	inReader, inWriter := io.Pipe()
	defer inWriter.Close()

	started := make(chan struct{})
	gate := make(chan struct{})
	rec := &gatedRecorder{started: started, gate: gate}
	conn := NewConn(inReader, rec)
	conn.sem = make(chan struct{}, 1)

	handlerStarted := make(chan struct{})
	conn.Handle("slow", func(ctx context.Context, _ json.RawMessage) (any, error) {
		close(handlerStarted)
		<-ctx.Done()
		return nil, ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- conn.Serve(ctx) }()

	go func() { _ = conn.Notify("hold", nil) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for writer to stall")
	}

	_, _ = inWriter.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"slow"}` + "\n"))
	select {
	case <-handlerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not start")
	}

	_, _ = inWriter.Write([]byte(`{"jsonrpc":"2.0","id":2,"method":"slow"}` + "\n"))
	deadline := time.Now().Add(2 * time.Second)
	for conn.writeWaiters.Load() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("busy writer did not wait for writeMu")
		}
		time.Sleep(5 * time.Millisecond)
	}

	_, _ = inWriter.Write([]byte(`{"jsonrpc":"2.0","id":3,"method":"slow"}` + "\n"))
	_, _ = inWriter.Write([]byte(`{"jsonrpc":"2.0","id":4,"method":"slow"}` + "\n"))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not exit on overload")
	}

	close(gate)
}

func TestInterleavedSessionCancelsDoNotCoalesceAcrossSessions(t *testing.T) {
	inReader, inWriter := io.Pipe()
	defer inWriter.Close()

	conn := NewConn(inReader, io.Discard)

	firstRun := make(chan struct{})
	gate := make(chan struct{})
	var (
		mu         sync.Mutex
		cancelledA int
		cancelledB int
	)

	conn.HandleNotify("session/cancel", func(_ context.Context, params json.RawMessage) {
		var p struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(params, &p)

		mu.Lock()
		if p.SessionID == "sess-A" {
			cancelledA++
		} else if p.SessionID == "sess-B" {
			cancelledB++
		}
		first := (cancelledA + cancelledB) == 1
		mu.Unlock()

		if first {
			close(firstRun)
			<-gate
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = conn.Serve(ctx) }()

	_, _ = inWriter.Write([]byte(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"sess-A"}}` + "\n"))
	select {
	case <-firstRun:
	case <-time.After(2 * time.Second):
		t.Fatal("first session cancel did not start")
	}

	_, _ = inWriter.Write([]byte(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"sess-B"}}` + "\n"))
	_, _ = inWriter.Write([]byte(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"sess-A"}}` + "\n"))

	// Wait until worker for sess-B runs or notifyQ receives the second sess-A before releasing gate.
	deadlineWait := time.Now().Add(2 * time.Second)
	for {
		conn.notifyMu.Lock()
		queued := string(conn.notifyQ[notifyKey{method: "session/cancel", target: "sess-A"}])
		conn.notifyMu.Unlock()
		if strings.Contains(queued, "sess-A") || time.Now().After(deadlineWait) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	close(gate)

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		gotA := cancelledA
		gotB := cancelledB
		mu.Unlock()
		if gotA >= 1 && gotB >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session cancels not delivered to both sessions: sess-A=%d, sess-B=%d", gotA, gotB)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestOverloadTerminatesHandlerInsideStalledWrite(t *testing.T) {
	inReader, inWriter := io.Pipe()
	defer inWriter.Close()

	started := make(chan struct{})
	gate := make(chan struct{})
	defer close(gate)
	conn := NewConn(inReader, &stallWriter{started: started, gate: gate})
	conn.sem = make(chan struct{}, 1)

	conn.Handle("echo", func(ctx context.Context, _ json.RawMessage) (any, error) {
		return "ok", nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- conn.Serve(ctx) }()

	_, _ = inWriter.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"echo"}` + "\n"))
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not enter stalled write")
	}

	_, _ = inWriter.Write([]byte(`{"jsonrpc":"2.0","id":2,"method":"echo"}` + "\n"))
	_, _ = inWriter.Write([]byte(`{"jsonrpc":"2.0","id":3,"method":"echo"}` + "\n"))
	_, _ = inWriter.Write([]byte(`{"jsonrpc":"2.0","id":4,"method":"echo"}` + "\n"))

	select {
	case err := <-done:
		if !errors.Is(err, errBusyOverload) {
			t.Fatalf("Serve returned %v, want %v", err, errBusyOverload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return on overload while handler was stalled in Write")
	}
}

func TestNotifyActiveIsBounded(t *testing.T) {
	inReader, inWriter := io.Pipe()
	defer inWriter.Close()
	conn := NewConn(inReader, io.Discard)
	gate := make(chan struct{})
	conn.HandleNotify("custom/ping", func(context.Context, json.RawMessage) {
		<-gate
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = conn.Serve(ctx) }()

	for i := 0; i < maxNotifyActive+40; i++ {
		frame := fmt.Sprintf(`{"jsonrpc":"2.0","method":"custom/ping","params":{"sessionId":"s-%d"}}`+"\n", i)
		if _, err := inWriter.Write([]byte(frame)); err != nil {
			t.Fatalf("write cancel %d: %v", i, err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn.notifyMu.Lock()
		n := len(conn.notifyOn)
		conn.notifyMu.Unlock()
		if n >= maxNotifyActive || time.Now().After(deadline) {
			if n > maxNotifyActive {
				close(gate)
				t.Fatalf("notifyOn = %d, want <= %d", n, maxNotifyActive)
			}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(gate)
}

func TestSessionUpdatePreservesOrder(t *testing.T) {
	inReader, inWriter := io.Pipe()
	defer inWriter.Close()
	conn := NewConn(inReader, io.Discard)
	gate := make(chan struct{})
	var got []string
	var mu sync.Mutex
	conn.HandleNotify(MethodSessionUpdate, func(_ context.Context, params json.RawMessage) {
		<-gate
		mu.Lock()
		got = append(got, string(params))
		mu.Unlock()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = conn.Serve(ctx) }()

	for i, payload := range []string{`{"sessionId":"s","n":1}`, `{"sessionId":"s","n":2}`, `{"sessionId":"s","n":3}`} {
		frame := `{"jsonrpc":"2.0","method":"session/update","params":` + payload + `}` + "\n"
		if _, err := inWriter.Write([]byte(frame)); err != nil {
			t.Fatalf("write update %d: %v", i, err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	close(gate)
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{`{"sessionId":"s","n":1}`, `{"sessionId":"s","n":2}`, `{"sessionId":"s","n":3}`}
	if len(got) != len(want) {
		t.Fatalf("got %d updates, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("update %d = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestSessionCancelExceedsNotifyCap(t *testing.T) {
	inReader, inWriter := io.Pipe()
	defer inWriter.Close()
	conn := NewConn(inReader, io.Discard)
	var started atomic.Int32
	gate := make(chan struct{})
	conn.HandleNotify(MethodSessionCancel, func(context.Context, json.RawMessage) {
		started.Add(1)
		<-gate
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = conn.Serve(ctx) }()

	want := maxNotifyActive + 8
	for i := 0; i < want; i++ {
		frame := fmt.Sprintf(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s-%d"}}`+"\n", i)
		if _, err := inWriter.Write([]byte(frame)); err != nil {
			t.Fatalf("write cancel %d: %v", i, err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for started.Load() < int32(want) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if started.Load() != int32(want) {
		close(gate)
		t.Fatalf("cancels started = %d, want %d", started.Load(), want)
	}
	close(gate)
}

func TestInflightByteBudgetRejects(t *testing.T) {
	inReader, inWriter := io.Pipe()
	defer inWriter.Close()
	var mu sync.Mutex
	var out strings.Builder
	conn := NewConn(inReader, testWriter(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return out.Write(p)
	}))
	frame1 := `{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{}}` + "\n"
	frame2 := `{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{}}` + "\n"
	conn.inflightLimit = int64(len(frame1))
	conn.sem = make(chan struct{}, 8)
	gate := make(chan struct{})
	started := make(chan struct{})
	conn.Handle("session/prompt", func(context.Context, json.RawMessage) (any, error) {
		select {
		case <-started:
		default:
			close(started)
		}
		<-gate
		return map[string]any{}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = conn.Serve(ctx) }()

	if _, err := inWriter.Write([]byte(frame1)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first request did not start")
	}
	if _, err := inWriter.Write([]byte(frame2)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	got := ""
	for time.Now().Before(deadline) {
		mu.Lock()
		got = out.String()
		mu.Unlock()
		if strings.Contains(got, "-32000") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(gate)
	_ = inWriter.Close()
	if !strings.Contains(got, "-32000") {
		t.Fatalf("expected busy from byte budget, got %q", got)
	}
}
