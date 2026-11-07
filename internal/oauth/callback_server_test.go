package oauth

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

// startStalledCallbackClient starts a CallbackServer and connects a client that
// sends part of a request header and then stops, which is the shape #1028 is
// about. It returns once the server has ACCEPTED the connection, using the
// ConnState hook rather than a sleep. StateNew, not StateActive: Go reports
// StateActive only after a request read returns, and a stalled header read
// returns only when the header timeout fires, so waiting for StateActive would
// make the setup depend on the very bound the test is about.
func startStalledCallbackClient(t *testing.T) (*CallbackServer, net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct{}, 1)
	server := startCallbackServer(listener, http.NotFoundHandler(), func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			select {
			case accepted <- struct{}{}:
			default:
			}
		}
	})
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(conn, "GET /callback HTTP/1.1\r\nHost: %s\r\nX-Stall:", listener.Addr()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("SETUP INVALID: the server never accepted the stalled client")
	}
	return server, conn
}

// connClosedWithin reports whether the server end of conn closes before the
// deadline: a read must fail with something other than a timeout.
func connClosedWithin(conn net.Conn, deadline time.Duration) bool {
	_ = conn.SetReadDeadline(time.Now().Add(deadline))
	var one [1]byte
	_, err := conn.Read(one[:])
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return false
	}
	return err != nil
}

// THE HEADER READ IS BOUNDED EVEN BEFORE CLOSE. The force-close in Close ends a
// stalled client when the flow ends; the header timeout ends it while the flow
// is still waiting for its callback, which can be minutes. Close is not called
// until after the assertion, so this can only pass on ReadHeaderTimeout.
func TestCallbackServerBoundsAStalledHeaderWithoutClose(t *testing.T) {
	previous := callbackReadHeaderTimeout
	callbackReadHeaderTimeout = 200 * time.Millisecond
	defer func() { callbackReadHeaderTimeout = previous }()

	server, conn := startStalledCallbackClient(t)
	defer server.Close()
	defer conn.Close()

	if !connClosedWithin(conn, 3*time.Second) {
		t.Fatal("a client stalled mid-header was still connected with Close never called; the header read is unbounded")
	}
}

// CLOSE RETURNS ONLY AFTER SERVE HAS. Its doc comment promises that nothing the
// server started outlives the call. With no connections open, Shutdown closes
// the listener and returns at once, so this is the case where Close could beat
// the Serve goroutine back; the goroutine closing done is the last thing it
// does, so done has to be closed by the time Close returns.
func TestCallbackServerCloseWaitsForServe(t *testing.T) {
	for attempt := 0; attempt < 50; attempt++ {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		server := StartCallbackServer(listener, http.NotFoundHandler())
		server.Close()
		select {
		case <-server.done:
		default:
			t.Fatalf("attempt %d: Close returned while the Serve goroutine was still running", attempt)
		}
	}
}
