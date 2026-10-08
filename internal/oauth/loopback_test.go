package oauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestLoopbackCapturesCode(t *testing.T) {
	l, err := NewLoopbackListener("the-state")
	if err != nil {
		t.Fatalf("NewLoopbackListener: %v", err)
	}
	defer l.Close()
	if !strings.HasPrefix(l.RedirectURI(), "http://127.0.0.1:") {
		t.Fatalf("redirect URI not loopback: %q", l.RedirectURI())
	}
	go func() {
		_, _ = http.Get(l.RedirectURI() + "?code=auth-code&state=the-state")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	code, err := l.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if code != "auth-code" {
		t.Fatalf("code = %q, want auth-code", code)
	}
}

func TestNewLoopbackListenerRejectsEmptyState(t *testing.T) {
	if _, err := NewLoopbackListener("   "); err == nil {
		t.Fatal("an empty CSRF state must be rejected (fail closed)")
	}
}

func TestRedirectURIWithHostUsesGivenHostAndPath(t *testing.T) {
	l, err := NewLoopbackListener("the-state")
	if err != nil {
		t.Fatalf("NewLoopbackListener: %v", err)
	}
	defer l.Close()
	got := l.RedirectURIWithHost("localhost", "/auth/callback")
	if !strings.HasPrefix(got, "http://localhost:") {
		t.Fatalf("redirect URI host = %q, want localhost", got)
	}
	if !strings.HasSuffix(got, "/auth/callback") {
		t.Fatalf("redirect URI path = %q, want /auth/callback suffix", got)
	}
}

func TestLoopbackCapturesCodeOnAuthCallbackPath(t *testing.T) {
	// The ChatGPT flow uses /auth/callback, not /callback; the listener must
	// capture the code on that path too.
	l, err := NewLoopbackListener("the-state")
	if err != nil {
		t.Fatalf("NewLoopbackListener: %v", err)
	}
	defer l.Close()
	uri := l.RedirectURIWithHost("127.0.0.1", "/auth/callback")
	go func() {
		_, _ = http.Get(uri + "?code=auth-code&state=the-state")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	code, err := l.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if code != "auth-code" {
		t.Fatalf("code = %q, want auth-code", code)
	}
}

func TestLoopbackRejectsStateMismatch(t *testing.T) {
	l, err := NewLoopbackListener("expected-state")
	if err != nil {
		t.Fatalf("NewLoopbackListener: %v", err)
	}
	defer l.Close()
	go func() {
		_, _ = http.Get(l.RedirectURI() + "?code=x&state=WRONG")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = l.Wait(ctx)
	if !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("Wait err = %v, want ErrStateMismatch", err)
	}
}

func TestLoopbackTimesOut(t *testing.T) {
	l, err := NewLoopbackListener("s")
	if err != nil {
		t.Fatalf("NewLoopbackListener: %v", err)
	}
	defer l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := l.Wait(ctx); err == nil {
		t.Fatal("Wait should time out cleanly")
	}
}

func TestLoopbackProviderError(t *testing.T) {
	l, err := NewLoopbackListener("s")
	if err != nil {
		t.Fatalf("NewLoopbackListener: %v", err)
	}
	defer l.Close()
	go func() {
		_, _ = http.Get(l.RedirectURI() + "?error=access_denied&error_description=nope&state=s")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = l.Wait(ctx)
	if err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("Wait err = %v, want provider error", err)
	}
}

func TestLoopbackCloseForceClosesSlowHeaderConnection(t *testing.T) {
	accepted := make(chan struct{}, 1)
	l, err := newLoopbackListener("audit-state", 0, func(_ net.Conn, state http.ConnState) {
		// StateNew fires as soon as Serve accepts the connection, before it
		// blocks reading the (never-completed) header. That's the earliest
		// point at which net/http's Shutdown will treat this connection as
		// non-idle, which is what this test needs to exercise.
		if state == http.StateNew {
			select {
			case accepted <- struct{}{}:
			default:
			}
		}
	})
	if err != nil {
		t.Fatalf("NewLoopbackListener: %v", err)
	}
	t.Cleanup(l.Close)
	addr := l.listener.Addr().String()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	// Wait for Serve to accept the connection before sending a request line
	// and a header key with no terminating CRLF, so Serve is guaranteed to
	// be blocked reading a partial header (rather than a fixed sleep guessing
	// that Accept has run).
	select {
	case <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("server never accepted the connection")
	}
	fmt.Fprintf(conn, "GET /callback?code=audit-code&state=audit-state HTTP/1.1\r\nHost: %s\r\nX-Stall:", addr)

	started := time.Now()
	l.Close()
	if elapsed := time.Since(started); elapsed < 900*time.Millisecond {
		t.Fatalf("Close returned after %s; expected it to wait out its one-second shutdown deadline first", elapsed)
	}

	conn.SetReadDeadline(time.Now().Add(time.Second))
	var one [1]byte
	_, readErr := conn.Read(one[:])
	if readErr == nil {
		t.Fatal("read after Close succeeded; wanted the stalled connection to be closed")
	}
	if netErr, ok := readErr.(net.Error); ok && netErr.Timeout() {
		t.Fatalf("read after Close timed out (%v); wanted the stalled connection closed rather than left open", readErr)
	}
}

func TestLoopbackCloseIsIdempotent(t *testing.T) {
	l, err := NewLoopbackListener("s")
	if err != nil {
		t.Fatalf("NewLoopbackListener: %v", err)
	}
	l.Close()
	l.Close() // must not panic or hang
}

func TestLoopbackCloseReturnsPromptlyAfterSuccessfulCallback(t *testing.T) {
	l, err := NewLoopbackListener("s")
	if err != nil {
		t.Fatalf("NewLoopbackListener: %v", err)
	}
	t.Cleanup(l.Close)
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		resp, err := http.Get(l.RedirectURI() + "?code=x&state=s")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := l.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	// Join the client goroutine so the response is fully read (and its
	// connection returned to idle) before timing Close, instead of racing it.
	select {
	case <-requestDone:
	case <-time.After(3 * time.Second):
		t.Fatal("http.Get goroutine did not complete")
	}

	started := time.Now()
	l.Close()
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("Close took %s after a completed, idle callback; want a prompt return", elapsed)
	}
}
