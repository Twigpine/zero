package providerio

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A stalled-but-open upstream must not block forever: the helper aborts after
// the idle timeout, cancels the request context, and returns ErrStreamIdle.
func TestScanSSEDataWithContextAbortsOnIdle(t *testing.T) {
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()

	// Send one event, then never send anything else and never close.
	go func() {
		_, _ = io.WriteString(pw, "data: first\n\n")
	}()

	cancelled := false
	cancel := func() { cancelled = true }

	var got []string
	done := make(chan error, 1)
	go func() {
		done <- ScanSSEDataWithContext(context.Background(), cancel, pr, 60*time.Millisecond, func(data string) bool {
			got = append(got, data)
			return true
		})
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrStreamIdle) {
			t.Fatalf("err = %v, want ErrStreamIdle", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ScanSSEDataWithContext hung on a stalled stream")
	}

	if len(got) != 1 || got[0] != "first" {
		t.Fatalf("got payloads %#v, want [first]", got)
	}
	if !cancelled {
		t.Fatal("idle abort did not cancel the request context")
	}
}

// ctx cancellation must unblock a hung read and surface ctx.Err().
func TestScanSSEDataWithContextHonorsContextCancel(t *testing.T) {
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- ScanSSEDataWithContext(ctx, cancel, pr, time.Hour, func(string) bool { return true })
	}()

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ScanSSEDataWithContext did not honor context cancellation")
	}
}

// ctx cancellation must unblock a hung read EVEN WHEN the idle watchdog is
// disabled (idleTimeout <= 0). Regression: the helper used to skip the
// goroutine + select loop when idle was off, so a context cancel could not
// interrupt a parked read and the call hung forever.
func TestScanSSEDataWithContextHonorsCancelWhenIdleDisabled(t *testing.T) {
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		// idleTimeout == 0 disables the watchdog; only ctx cancel can return.
		done <- ScanSSEDataWithContext(ctx, cancel, pr, 0, func(string) bool { return true })
	}()

	// Cancel shortly after the call has parked in a blocking read with no data.
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ScanSSEDataWithContext hung with idle watchdog disabled; ctx cancel ignored")
	}
}

// Normal completion (EOF) must return nil after delivering all data payloads,
// matching ScanSSEData's multi-line accumulation semantics.
func TestScanSSEDataWithContextDeliversThenEOF(t *testing.T) {
	body := "data: line-a\ndata: line-b\n\ndata: [DONE]\n\n"
	var got []string
	err := ScanSSEDataWithContext(context.Background(), func() {}, strings.NewReader(body), time.Hour, func(data string) bool {
		got = append(got, data)
		return true
	})
	if err != nil {
		t.Fatalf("err = %v, want nil on EOF", err)
	}
	if len(got) != 1 || got[0] != "line-a\nline-b" {
		t.Fatalf("got %#v, want one accumulated payload", got)
	}
}

// UpstreamUnreachable rewrites a transport/gateway connectivity failure into a
// clear message naming the host, and leaves genuine model errors (and bare
// markers with no host) untouched.
func TestUpstreamUnreachable(t *testing.T) {
	cases := []struct {
		name       string
		message    string
		wantMatch  bool
		wantHost   string
		wantReason string
	}{
		{
			name:       "ollama daemon 502 cloud proxy",
			message:    `Post "https://ollama.com:443/v1/chat/completions?ts=1781690613": net/http: TLS handshake timeout`,
			wantMatch:  true,
			wantHost:   "ollama.com:443",
			wantReason: "TLS handshake timeout",
		},
		{
			name:       "direct connection tls timeout",
			message:    `Post "https://ollama.com/v1/chat/completions": net/http: TLS handshake timeout`,
			wantMatch:  true,
			wantHost:   "ollama.com",
			wantReason: "TLS handshake timeout",
		},
		{
			name:       "url form preferred over dial target",
			message:    `Get "https://api.example.com/v1/models": dial tcp 203.0.113.7:443: i/o timeout`,
			wantMatch:  true,
			wantHost:   "api.example.com",
			wantReason: "i/o timeout",
		},
		{
			name:       "dns lookup failure keeps url host",
			message:    `Post "https://ollama.com/api/chat": dial tcp: lookup ollama.com on 8.8.8.8:53: no such host`,
			wantMatch:  true,
			wantHost:   "ollama.com",
			wantReason: "no such host",
		},
		{
			name:       "local daemon not running",
			message:    `dial tcp 127.0.0.1:11434: connect: connection refused`,
			wantMatch:  true,
			wantHost:   "127.0.0.1:11434",
			wantReason: "connection refused",
		},
		{
			name:      "genuine model error untouched",
			message:   `{"error":{"message":"model not found"}}`,
			wantMatch: false,
		},
		{
			name:      "marker without host untouched",
			message:   `context deadline exceeded`,
			wantMatch: false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, ok := UpstreamUnreachable(testCase.message)
			if ok != testCase.wantMatch {
				t.Fatalf("match = %v, want %v (got %q)", ok, testCase.wantMatch, got)
			}
			if !testCase.wantMatch {
				if got != testCase.message {
					t.Fatalf("non-match must return input unchanged, got %q", got)
				}
				return
			}
			if !strings.HasPrefix(got, "upstream unreachable: ") {
				t.Errorf("missing prefix in %q", got)
			}
			if !strings.Contains(got, testCase.wantHost) {
				t.Errorf("missing host %q in %q", testCase.wantHost, got)
			}
			if !strings.Contains(got, testCase.wantReason) {
				t.Errorf("missing reason %q in %q", testCase.wantReason, got)
			}
		})
	}
}

// ContentStallTimeout is 1.2× the idle timeout, disabled (0) when idle is
// disabled, and must never overflow to a negative duration for an absurdly
// large idle timeout (which would arm the content timer to fire immediately
// and abort every stream).
func TestContentStallTimeout(t *testing.T) {
	cases := []struct {
		idle time.Duration
		want time.Duration
	}{
		{5 * time.Minute, 6 * time.Minute},
		{30 * time.Second, 36 * time.Second},
		{100 * time.Millisecond, 120 * time.Millisecond},
		{0, 0},
		{-1, 0},
	}
	for _, c := range cases {
		if got := ContentStallTimeout(c.idle); got != c.want {
			t.Fatalf("ContentStallTimeout(%v) = %v, want %v", c.idle, got, c.want)
		}
	}
	// Overflow guard: idle*6 would wrap negative; the clamp keeps it positive.
	if got := ContentStallTimeout(math.MaxInt64); got <= 0 {
		t.Fatalf("ContentStallTimeout(MaxInt64) = %v, must stay positive (no overflow wrap)", got)
	}
}

// A heartbeating-but-output-less upstream must not hang forever: SSE keep-alives
// feed the idle watchdog (so it never fires), but the content watchdog
// (ContentStallTimeout(idle)) aborts with ErrStreamStalled when no real data
// line arrives. This is the gpt-5.x / ollama "still generating forever" hang.
func TestScanSSEDataWithContextAbortsOnContentStall(t *testing.T) {
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		// Heartbeat every 30ms — comfortably under the 100ms idle timeout, so idle
		// never fires under CI/GC jitter — but never send a data line.
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := io.WriteString(pw, ": keep-alive\n\n"); err != nil {
				return
			}
			time.Sleep(30 * time.Millisecond)
		}
	}()

	cancelled := false
	cancel := func() { cancelled = true }

	done := make(chan error, 1)
	go func() {
		// idle 100ms → content stall at 120ms (ContentStallTimeout = idle*1.2).
		// Keep-alives reset idle but not content.
		done <- ScanSSEDataWithContext(context.Background(), cancel, pr, 100*time.Millisecond, func(string) bool { return true })
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrStreamStalled) {
			t.Fatalf("err = %v, want ErrStreamStalled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ScanSSEDataWithContext hung on a heartbeat-but-no-output stream")
	}
	if !cancelled {
		t.Fatal("content stall did not cancel the request context")
	}
}

// Real data lines reset the content watchdog, so a slow-but-producing stream that
// runs longer than the content window is never aborted.
func TestScanSSEDataWithContextContentResetsOnData(t *testing.T) {
	pr, pw := io.Pipe()

	go func() {
		// One data line every 30ms for ~300ms (well past the 120ms content window,
		// with comfortable slack under the 100ms idle timeout), so the content
		// watchdog keeps resetting, then close cleanly.
		for i := 0; i < 10; i++ {
			if _, err := io.WriteString(pw, "data: chunk\n\n"); err != nil {
				return
			}
			time.Sleep(30 * time.Millisecond)
		}
		_ = pw.Close()
	}()

	n := 0
	done := make(chan error, 1)
	go func() {
		done <- ScanSSEDataWithContext(context.Background(), func() {}, pr, 100*time.Millisecond, func(string) bool { n++; return true })
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("err = %v, want nil (data kept the stream alive)", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ScanSSEDataWithContext hung on a producing stream")
	}
	if n != 10 {
		t.Fatalf("handled %d data lines, want 10", n)
	}
}

// StreamTimeoutMessage must give a stalled stream a distinct, accurate detail —
// it reports the content window (ContentStallTimeout) and must NOT claim the
// upstream "stopped sending data" (keep-alives were still arriving).
func TestStreamTimeoutMessage(t *testing.T) {
	idle := 5 * time.Minute
	if msg := StreamTimeoutMessage(ErrStreamIdle, idle); !strings.Contains(msg, "idle timeout after 5m") {
		t.Fatalf("idle message = %q, want it to mention the 5m idle timeout", msg)
	}
	stalled := StreamTimeoutMessage(ErrStreamStalled, idle)
	if !strings.Contains(stalled, "no output for 6m") {
		t.Fatalf("stalled message = %q, want it to report the 6m content window (idle 5m × 1.2)", stalled)
	}
	if strings.Contains(stalled, "stopped sending data") {
		t.Fatalf("stalled message must not claim the upstream stopped sending data: %q", stalled)
	}
}

// HTTPClient(nil) must return the shared, stall-hardened transport (bounded
// response-header wait + shorter idle-conn reuse) that defeats the macOS stale-
// pooled-connection hang; an explicit client is returned untouched.
func TestHTTPClientReturnsStallHardenedSharedClient(t *testing.T) {
	got := HTTPClient(nil)
	if got == nil {
		t.Fatal("HTTPClient(nil) returned nil")
	}
	tr, ok := got.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", got.Transport)
	}
	if tr.ResponseHeaderTimeout != 120*time.Second {
		t.Fatalf("ResponseHeaderTimeout = %v, want 120s", tr.ResponseHeaderTimeout)
	}
	if tr.IdleConnTimeout != 30*time.Second {
		t.Fatalf("IdleConnTimeout = %v, want 30s", tr.IdleConnTimeout)
	}
	// DisableKeepAlives is the stronger mitigation scoped to darwin only: the
	// two timeouts above catch a reused connection that's fully dead or idle
	// too long, but not one that's alive-and-degraded (still delivers real
	// bytes, just at a crippled rate) — which resets Zero's stream watchdogs
	// without ever recovering. Removing pooling from the equation entirely is
	// only worth its reconnect cost on the platform this class of bug has
	// actually reproduced on.
	wantDisableKeepAlives := runtime.GOOS == "darwin"
	if tr.DisableKeepAlives != wantDisableKeepAlives {
		t.Fatalf("DisableKeepAlives = %v, want %v (GOOS=%s)", tr.DisableKeepAlives, wantDisableKeepAlives, runtime.GOOS)
	}
	if HTTPClient(nil) != got {
		t.Fatal("HTTPClient(nil) must return a shared instance so the conn pool is reused")
	}
	custom := &http.Client{}
	if HTTPClient(custom) != custom {
		t.Fatal("an explicit client must be returned unchanged")
	}
}

// startIdleConnCloser periodically closes idle pooled connections so stale HTTP/2
// connections are not reused across long idle periods.
func TestStartIdleConnCloserClosesIdleConnections(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DisableKeepAlives = false
	defer tr.CloseIdleConnections()

	client := &http.Client{Transport: tr}

	// First request creates a connection in the pool.
	resp1, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp1.Body)
	_ = resp1.Body.Close()

	// Immediate second request reuses the pooled connection.
	var reusedFirst bool
	trace1 := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			reusedFirst = info.Reused
		},
	}
	req2, err := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace1), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("new request failed: %v", err)
	}
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("second request failed: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp2.Body)
	_ = resp2.Body.Close()

	if !reusedFirst {
		t.Fatal("expected immediate second request to reuse keep-alive connection")
	}

	// Start closer with a short interval and wait for it to fire.
	stop := startIdleConnCloser(tr, 20*time.Millisecond)
	defer stop()

	// Poll until the idle closer evicts the connection and a fresh dial occurs.
	deadline := time.Now().Add(2 * time.Second)
	var closed bool
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		var reused bool
		trace := &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) {
				reused = info.Reused
			},
		}
		req, err := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), http.MethodGet, server.URL, nil)
		if err != nil {
			t.Fatalf("new request failed: %v", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if !reused {
			closed = true
			break
		}
	}
	if !closed {
		t.Fatal("expected connection to be closed by idle closer and not reused")
	}
}

func TestStartIdleConnCloserEdgeCases(t *testing.T) {
	// Zero or negative interval returns a no-op stop func without starting goroutine.
	stopZero := startIdleConnCloser(nil, 0)
	stopZero()

	tr := http.DefaultTransport.(*http.Transport).Clone()
	stopNil := startIdleConnCloser(tr, -1*time.Second)
	stopNil()

	// Calling stop multiple times is idempotent.
	stop := startIdleConnCloser(tr, 100*time.Millisecond)
	stop()
	stop()
}
