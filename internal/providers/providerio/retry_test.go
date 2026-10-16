//go:build !plan9

package providerio

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// wrapDialErrno builds the error shape a dial failure has (a *net.OpError
// wrapping an *os.SyscallError wrapping the errno) so errors.Is reaches the
// errno exactly as in production. These fixtures use the POSIX syscall.Exxx
// constants, which do NOT reproduce a real Windows dial: that carries distinct
// windows.WSA* errnos (Go's syscall.ECONNREFUSED is a value the net package
// never produces on Windows). They still classify on Windows only because
// dialPreSendErrnos there also lists the POSIX constants for exactly this
// fixture reason; the real Windows dial branch is covered separately by
// TestIsPreSendTransportErrorRealRefusedDial, which makes an actual refused dial.
func wrapDialErrno(op string, errno syscall.Errno) error {
	return &net.OpError{Op: op, Net: "tcp", Err: os.NewSyscallError("connectex", errno)}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSendWithRetryDoesNotReplayTransportErrors(t *testing.T) {
	var calls int32
	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return nil, errors.New("connection reset by peer")
	})}

	resp, err := SendWithRetry(context.Background(), client, http.MethodPost, "http://example.invalid", []byte("{}"), nil, 3)
	if resp != nil {
		if cerr := resp.Body.Close(); cerr != nil {
			t.Errorf("close response body: %v", cerr)
		}
	}
	if err == nil {
		t.Fatal("expected a transport error to surface")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("transport error replayed %d times — a non-idempotent POST must not be retried, want 1", got)
	}
}

// A PROVABLY pre-send transport failure (no request bytes left this host) is
// safe to replay and must be retried, bounded by preSendMaxAttempts (its own
// short schedule), unlike an ambiguous post-send failure. Real dial failures
// arrive as an Op=="dial" *net.OpError, so the errno cases are the production
// shape on every platform (Windows included); the string case exercises the
// wording fallback for a dial error already flattened past its errno.
func TestSendWithRetryReplaysProvablyPreSendErrors(t *testing.T) {
	shrinkBackoff(t)
	cases := map[string]error{
		"errno refused (dial)":      wrapDialErrno("dial", syscall.ECONNREFUSED),
		"errno network unreachable": wrapDialErrno("dial", syscall.ENETUNREACH),
		"errno host unreachable":    wrapDialErrno("dial", syscall.EHOSTUNREACH),
		"dial string fallback":      &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")},
		"tls handshake timeout":     errors.New("net/http: TLS handshake timeout"),
		"dns timeout":               &net.DNSError{Err: "server misbehaving", Name: "nope.invalid", IsTimeout: true},
	}
	for name, transportErr := range cases {
		t.Run(name, func(t *testing.T) {
			var calls int32
			client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
				atomic.AddInt32(&calls, 1)
				return nil, transportErr
			})}
			// maxAttempts=6 (the default) proves the pre-send path is bounded by
			// preSendMaxAttempts, not the caller's 429-tuned maxAttempts.
			resp, err := SendWithRetry(context.Background(), client, http.MethodPost, "http://example.invalid", []byte("{}"), nil, 6)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if err == nil {
				t.Fatal("expected the transport error to surface after retries are exhausted")
			}
			if got := atomic.LoadInt32(&calls); got != preSendMaxAttempts {
				t.Fatalf("pre-send error tried %d times, want %d (preSendMaxAttempts)", got, preSendMaxAttempts)
			}
		})
	}
}

// An ambiguous transport failure that could have followed a sent request must
// NOT be replayed: a non-idempotent completion POST could duplicate billable
// work. This is the safety line the fix must not cross.
func TestSendWithRetryDoesNotReplayAmbiguousTransportErrors(t *testing.T) {
	for name, transportErr := range map[string]error{
		"generic i/o timeout": errors.New("dial tcp 1.2.3.4:443: i/o timeout"),
		"broken pipe":         errors.New("write tcp: broken pipe"),
		"unexpected eof":      io.ErrUnexpectedEOF,
		// The pre-send errnos are also raised on an ESTABLISHED connection: a route
		// dropping mid-generation surfaces on the pending read/write, which is
		// post-send. Scoping to Op=="dial" must keep these from replaying the POST.
		"host unreachable on read is post-send": wrapDialErrno("read", syscall.EHOSTUNREACH),
		"net unreachable on write is post-send": wrapDialErrno("write", syscall.ENETUNREACH),
		// NXDOMAIN is authoritative and deterministic, so retrying it would only
		// stall the agent before failing anyway.
		"dns nxdomain": &net.DNSError{Err: "no such host", Name: "nope.invalid", IsNotFound: true},
	} {
		t.Run(name, func(t *testing.T) {
			var calls int32
			client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
				atomic.AddInt32(&calls, 1)
				return nil, transportErr
			})}
			resp, err := SendWithRetry(context.Background(), client, http.MethodPost, "http://example.invalid", []byte("{}"), nil, 3)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if err == nil {
				t.Fatal("expected the transport error to surface")
			}
			if got := atomic.LoadInt32(&calls); got != 1 {
				t.Fatalf("ambiguous transport error replayed %d times, want 1 (no retry)", got)
			}
		})
	}
}

// A REAL refused dial (nothing listening on the target) must classify as
// pre-send on every platform. This is the regression the errno-constant fixtures
// miss: on Windows the kernel raises WSAECONNREFUSED, which errors.Is does NOT
// match against syscall.ECONNREFUSED, so before dialPreSendErrnos carried the WSA
// codes a refused dial returned false here and Windows never retried it. Using a
// real Dial exercises the platform's true error shape.
func TestIsPreSendTransportErrorRealRefusedDial(t *testing.T) {
	// Reserve an ephemeral port, then close it, so a dial there is refused.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if cerr := ln.Close(); cerr != nil {
		t.Fatalf("close listener: %v", cerr)
	}
	conn, dialErr := (&net.Dialer{Timeout: 2 * time.Second}).Dial("tcp", addr)
	if dialErr == nil {
		_ = conn.Close()
		t.Skip("expected a refused dial but the port accepted a connection")
	}
	if !isPreSendTransportError(dialErr) {
		t.Fatalf("real refused dial not classified pre-send: %v", dialErr)
	}
}

func TestIsPreSendTransportError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		// DNS: a lookup that could recover is pre-send; NXDOMAIN is permanent.
		{"dns timeout", &net.DNSError{Err: "server misbehaving", Name: "x.invalid", IsTimeout: true}, true},
		{"dns temporary", &net.DNSError{Err: "server misbehaving", Name: "x.invalid", IsTemporary: true}, true},
		{"dns nxdomain is permanent", &net.DNSError{Err: "no such host", Name: "x.invalid", IsNotFound: true}, false},
		{"tls handshake timeout", errors.New("net/http: TLS handshake timeout"), true},
		// Errno-wrapped DIAL failures: how a real refused/unreachable dial arrives
		// on EVERY platform, including Windows where the wording differs entirely.
		{"errno refused (dial)", wrapDialErrno("dial", syscall.ECONNREFUSED), true},
		{"errno network unreachable (dial)", wrapDialErrno("dial", syscall.ENETUNREACH), true},
		{"errno host unreachable (dial)", wrapDialErrno("dial", syscall.EHOSTUNREACH), true},
		{"dial operror string fallback", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}, true},
		// The SAME errnos raised on a post-send read/write are NOT pre-send — the
		// kernel reports them on an established connection when a route drops.
		{"host unreachable on read is post-send", wrapDialErrno("read", syscall.EHOSTUNREACH), false},
		{"net unreachable on write is post-send", wrapDialErrno("write", syscall.ENETUNREACH), false},
		{"errno reset is post-send", wrapDialErrno("read", syscall.ECONNRESET), false},
		// A refused/unreachable error already flattened past its *net.OpError can't
		// be proven pre-send, so it is NOT retried (conservative direction).
		{"flattened refused, no opError", errors.New("dial tcp 127.0.0.1:1: connect: connection refused"), false},
		{"connection reset", errors.New("read tcp: connection reset by peer"), false},
		{"broken pipe", errors.New("write tcp: broken pipe"), false},
		{"unexpected eof", io.ErrUnexpectedEOF, false},
		{"eof", io.EOF, false},
		{"generic io timeout", errors.New("dial tcp: i/o timeout"), false},
		{"context deadline", context.DeadlineExceeded, false},
		// Exclusion is checked before inclusion, even for a dial OpError: an "i/o
		// timeout" in the message wins over the refused wording.
		{"exclusion wins over inclusion", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused / i/o timeout")}, false},
		{"unrelated", errors.New("some other error"), false},
	}
	for _, c := range cases {
		if got := isPreSendTransportError(c.err); got != c.want {
			t.Errorf("%s: isPreSendTransportError(%v) = %v, want %v", c.name, c.err, got, c.want)
		}
	}
}

func TestShouldRetryStatus(t *testing.T) {
	cases := map[int]bool{
		http.StatusOK:                  false,
		http.StatusBadRequest:          false,
		http.StatusNotFound:            false,
		http.StatusUnauthorized:        false,
		http.StatusTooManyRequests:     true,  // 429: rate-limited, not accepted
		http.StatusServiceUnavailable:  true,  // 503: unavailable, not accepted
		http.StatusInternalServerError: false, // 500: ambiguous — may have had an effect
		http.StatusBadGateway:          false, // 502: ambiguous
		http.StatusGatewayTimeout:      false, // 504: upstream may have processed it
	}
	for code, want := range cases {
		if got := ShouldRetryStatus(code); got != want {
			t.Errorf("ShouldRetryStatus(%d) = %v, want %v", code, got, want)
		}
	}
}

func TestRetryAfterParsesHeader(t *testing.T) {
	mk := func(value string) *http.Response {
		resp := &http.Response{Header: http.Header{}}
		if value != "" {
			resp.Header.Set("Retry-After", value)
		}
		return resp
	}
	if got := RetryAfter(mk("3")); got != 3*time.Second {
		t.Errorf("RetryAfter(\"3\") = %v, want 3s", got)
	}
	if got := RetryAfter(mk("")); got != 0 {
		t.Errorf("RetryAfter(absent) = %v, want 0", got)
	}
	if got := RetryAfter(mk("0")); got != 0 {
		t.Errorf("RetryAfter(\"0\") = %v, want 0", got)
	}
	if got := RetryAfter(mk("not-a-number")); got != 0 {
		t.Errorf("RetryAfter(garbage) = %v, want 0", got)
	}
	if got := RetryAfter(nil); got != 0 {
		t.Errorf("RetryAfter(nil) = %v, want 0", got)
	}
}

func TestBackoffReturnsFalseOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if Backoff(ctx, 5, 0) {
		t.Fatal("Backoff should return false when the context is already cancelled")
	}
}

func TestBackoffWaitsThenReturnsTrue(t *testing.T) {
	// retryAfter overrides the attempt-based wait, keeping the test fast.
	if !Backoff(context.Background(), 1, time.Millisecond) {
		t.Fatal("Backoff should return true after waiting out a short delay")
	}
}

// shrinkBackoff makes both retry schedules (429 and pre-send) negligible for the
// duration of a test.
func shrinkBackoff(t *testing.T) {
	t.Helper()
	savedRetry, savedPreSend := retryBackoffBase, preSendBackoffBase
	retryBackoffBase = time.Millisecond
	preSendBackoffBase = time.Millisecond
	t.Cleanup(func() {
		retryBackoffBase = savedRetry
		preSendBackoffBase = savedPreSend
	})
}

func TestShrinkBackoffForTest(t *testing.T) {
	origRetry, origPreSend := retryBackoffBase, preSendBackoffBase
	restore := ShrinkBackoffForTest()
	if retryBackoffBase != time.Millisecond || preSendBackoffBase != time.Millisecond {
		t.Fatalf("ShrinkBackoffForTest did not set bases to 1ms: got retry=%v, preSend=%v",
			retryBackoffBase, preSendBackoffBase)
	}
	restore()
	if retryBackoffBase != origRetry || preSendBackoffBase != origPreSend {
		t.Fatalf("restore() failed: got retry=%v, preSend=%v; want %v, %v",
			retryBackoffBase, preSendBackoffBase, origRetry, origPreSend)
	}
}

func TestBackoffWaitSchedule(t *testing.T) {
	// Without Retry-After the wait doubles per attempt from 2s and caps at 30s;
	// a supplied Retry-After wins but is capped too.
	cases := []struct {
		attempt    int
		retryAfter time.Duration
		want       time.Duration
	}{
		{1, 0, 2 * time.Second},
		{2, 0, 4 * time.Second},
		{3, 0, 8 * time.Second},
		{4, 0, 16 * time.Second},
		{5, 0, 30 * time.Second},  // 32s capped
		{50, 0, 30 * time.Second}, // clamped exponent, no overflow
		{1, 7 * time.Second, 7 * time.Second},
		{1, 5 * time.Minute, 30 * time.Second}, // hostile Retry-After capped
	}
	for _, c := range cases {
		if got := backoffWait(c.attempt, c.retryAfter); got != c.want {
			t.Errorf("backoffWait(%d, %v) = %v, want %v", c.attempt, c.retryAfter, got, c.want)
		}
	}
}

// The pre-send schedule is sub-second and doubles, far shorter than the 429
// schedule: a permanent dial failure fails in ~1.5s across preSendMaxAttempts
// (500ms + 1s) instead of stalling the agent ~60s on 2/4/8/16/30s. This is the
// second half of the fix for a mistyped host / dead local daemon hanging a turn.
func TestPreSendBackoffWaitSchedule(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 500 * time.Millisecond},
		{2, 1 * time.Second},
		{3, 2 * time.Second},
		// The exponent clamps at 5 (500ms<<5 = 16s) so a large attempt count can't
		// overflow; in practice preSendMaxAttempts caps retries at attempt 2, so
		// only the 500ms/1s rungs are ever reached.
		{50, 16 * time.Second},
	}
	for _, c := range cases {
		if got := preSendBackoffWait(c.attempt); got != c.want {
			t.Errorf("preSendBackoffWait(%d) = %v, want %v", c.attempt, got, c.want)
		}
	}
}

func TestSendWithRetryRetriesThenSucceeds(t *testing.T) {
	shrinkBackoff(t)
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable) // 503: retryable (not accepted)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	resp, err := SendWithRetry(context.Background(), server.Client(), http.MethodPost, server.URL, []byte("{}"), nil, 3)
	if err != nil {
		t.Fatalf("SendWithRetry returned error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after retry", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("server hit %d times, want 2 (one failure + one success)", got)
	}
}

func TestSendWithRetryReturnsNonRetryableImmediately(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusBadRequest) // 400 is not retryable
	}))
	defer server.Close()

	resp, err := SendWithRetry(context.Background(), server.Client(), http.MethodPost, server.URL, []byte("{}"), nil, 3)
	if err != nil {
		t.Fatalf("SendWithRetry returned error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("server hit %d times, want 1 (no retry on 400)", got)
	}
}

func TestSendWithRetryReturnsLastResponseAfterMaxAttempts(t *testing.T) {
	shrinkBackoff(t)
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusServiceUnavailable) // always retryable
	}))
	defer server.Close()

	resp, err := SendWithRetry(context.Background(), server.Client(), http.MethodPost, server.URL, []byte("{}"), nil, 2)
	if err != nil {
		t.Fatalf("SendWithRetry returned error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (exhausted retries surface the response)", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("server hit %d times, want 2 (maxAttempts)", got)
	}
}

// Redirects are still followed, so a gateway that answers the completion endpoint
// with a 307/308 keeps working: the caller gets the final response, not the 3xx.
func TestSendWithRetryFollowsRedirects(t *testing.T) {
	var calls int32
	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return &http.Response{
				StatusCode: http.StatusTemporaryRedirect,
				Header:     http.Header{"Location": {"https://redirect-target.invalid/v1"}},
				Body:       http.NoBody,
				Request:    r,
			}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: r}, nil
	})}

	resp, err := SendWithRetry(context.Background(), client, http.MethodPost, "https://origin.invalid/v1", []byte("{}"), nil, 3)
	if err != nil {
		t.Fatalf("a redirected completion must succeed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the redirect should have been followed)", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("transport called %d times, want 2 (origin then redirect target)", got)
	}
}

// Once an attempt has entered a redirect, the original POST has already left this
// host, so a dial failure on the redirect hop must NOT be replayed even though it
// arrives as an Op=="dial" error that isPreSendTransportError would otherwise
// treat as provably pre-send. Replaying it would re-bill a completion the first
// host may already have processed.
func TestSendWithRetryDoesNotReplayAfterRedirect(t *testing.T) {
	shrinkBackoff(t)
	var calls int32
	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return &http.Response{
				StatusCode: http.StatusTemporaryRedirect,
				Header:     http.Header{"Location": {"https://redirect-target.invalid/v1"}},
				Body:       http.NoBody,
				Request:    r,
			}, nil
		}
		// The dial to the redirect target fails: pre-send in shape, but post-send
		// in fact, because the origin already received the POST.
		return nil, wrapDialErrno("dial", syscall.ECONNREFUSED)
	})}

	resp, err := SendWithRetry(context.Background(), client, http.MethodPost, "https://origin.invalid/v1", []byte("{}"), nil, 6)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected the redirect-hop dial failure to surface as an error")
	}
	// Exactly two: the origin request and the single redirect hop. A third would
	// mean the redirected POST was replayed.
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("redirected POST was replayed: transport called %d times, want 2", got)
	}
}

// A caller that supplies its own CheckRedirect keeps it: the retry path observes
// redirects, it does not take the decision away from the caller.
func TestSendWithRetryPreservesCallerRedirectPolicy(t *testing.T) {
	var policyCalls int32
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			atomic.AddInt32(&policyCalls, 1)
			return http.ErrUseLastResponse
		},
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusTemporaryRedirect,
				Header:     http.Header{"Location": {"https://redirect-target.invalid/v1"}},
				Body:       http.NoBody,
				Request:    r,
			}, nil
		}),
	}

	resp, err := SendWithRetry(context.Background(), client, http.MethodPost, "https://origin.invalid/v1", []byte("{}"), nil, 3)
	if err != nil {
		t.Fatalf("caller policy asked to stop at the 3xx, so it must surface: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want the 307 surfaced per the caller's policy", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&policyCalls); got != 1 {
		t.Fatalf("caller CheckRedirect called %d times, want 1 (it must not be discarded)", got)
	}
}

// The pre-send retry budget is independent of the 429/503 status retries: two
// rate-limit responses must not consume the pre-send allowance, so the first
// refused dial after them still gets its own preSendMaxAttempts tries.
func TestSendWithRetryPreSendBudgetSurvivesStatusRetries(t *testing.T) {
	shrinkBackoff(t)
	var calls int32
	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			return &http.Response{StatusCode: http.StatusTooManyRequests, Body: http.NoBody, Request: r}, nil
		}
		return nil, wrapDialErrno("dial", syscall.ECONNREFUSED)
	})}

	resp, err := SendWithRetry(context.Background(), client, http.MethodPost, "https://x.invalid/v1", []byte("{}"), nil, 6)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected the exhausted pre-send failure to surface as an error")
	}
	// Two status responses (calls 1-2), then the pre-send failure gets its own
	// budget: preSendMaxAttempts=3 means it retries twice more before returning.
	if got := atomic.LoadInt32(&calls); got != 2+preSendMaxAttempts {
		t.Fatalf("pre-send budget eaten by status retries: transport called %d times, want %d (2 status + %d pre-send)", got, 2+preSendMaxAttempts, preSendMaxAttempts)
	}
}

// The inverse sequence: pre-send retries must not consume the 429/503 budget or
// advance its backoff rung either. Two safely replayed refused dials followed by
// rate limiting must still leave the status path its full maxAttempts window,
// otherwise a couple of dial blips silently shorten every later rate-limit wait.
func TestSendWithRetryStatusBudgetSurvivesPreSendRetries(t *testing.T) {
	shrinkBackoff(t)
	const maxAttempts = 3
	var calls int32
	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			return nil, wrapDialErrno("dial", syscall.ECONNREFUSED)
		}
		return &http.Response{StatusCode: http.StatusTooManyRequests, Body: http.NoBody, Request: r}, nil
	})}

	resp, err := SendWithRetry(context.Background(), client, http.MethodPost, "https://x.invalid/v1", []byte("{}"), nil, maxAttempts)
	if err != nil {
		t.Fatalf("exhausted status retries surface the response, not an error: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	// Calls 1-2 are the retried dial failures; the status path then gets its own
	// full window (maxAttempts requests) before the 429 is surfaced.
	if got := atomic.LoadInt32(&calls); got != 2+maxAttempts {
		t.Fatalf("status budget eaten by pre-send retries: transport called %d times, want %d (2 pre-send + %d status)", got, 2+maxAttempts, maxAttempts)
	}
}

// A redirect followed by a RETRYABLE STATUS is still retried, and deliberately
// so. This is the case a reviewer flagged as a possible replay of billable work,
// and the asymmetry with the transport-error guard above is intentional rather
// than an oversight, so it is pinned here.
//
// The two situations differ in what is known about the request:
//
//   - Transport error after a redirect: the original POST reached the origin and
//     the dial to the redirect target then failed, so whether the target received
//     and began processing it is UNKNOWN. Replaying could duplicate work, which
//     is why that path refuses to retry.
//   - Retryable status after a redirect: every hop answered. A 307/308 is the
//     origin declining to process and delegating, and a 429/503/529 is the target
//     declining as well. Nothing accepted the request, so replaying duplicates
//     nothing.
//
// The risk here is also exactly the risk of a 429 without any redirect, which
// this package has always retried; the redirect adds no new ambiguity. Refusing
// to retry would instead make a rate-limited completion behind a redirecting
// gateway fail outright rather than back off.
func TestSendWithRetryRetriesRedirectedRetryableStatus(t *testing.T) {
	var calls int32
	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		switch atomic.AddInt32(&calls, 1) {
		case 1, 3:
			// The origin declines and delegates, both times.
			return &http.Response{
				StatusCode: http.StatusTemporaryRedirect,
				Header:     http.Header{"Location": {"https://redirect-target.invalid/v1"}},
				Body:       http.NoBody,
				Request:    r,
			}, nil
		case 2:
			// The target declines too: rate limited, so nothing was accepted.
			return &http.Response{StatusCode: http.StatusTooManyRequests, Body: http.NoBody, Request: r}, nil
		default:
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: r}, nil
		}
	})}

	resp, err := SendWithRetry(context.Background(), client, http.MethodPost, "https://origin.invalid/v1", []byte("{}"), nil, 3)
	if err != nil {
		t.Fatalf("a rate-limited completion behind a redirect must still retry: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after the retry succeeded", resp.StatusCode)
	}
	// origin, target(429), origin, target(200).
	if got := atomic.LoadInt32(&calls); got != 4 {
		t.Fatalf("transport called %d times, want 4 (redirect, 429, redirect, 200)", got)
	}
}

// A CONNECT-phase timeout is provably pre-send: the connection was never
// established, so no request bytes left this host. It reaches the classifier
// carrying "i/o timeout", which the blanket wording exclusion rejects, so
// admitting it depends on the connect gate running first.
//
// The post-send counterpart is the point of the table: a read timeout is also
// Timeout(), and must stay excluded, or a reply that timed out mid-generation
// would replay a completion the server already billed.
func TestPreSendClassifiesConnectTimeouts(t *testing.T) {
	timeoutErr := &timeoutError{}
	for name, testCase := range map[string]struct {
		err  error
		want bool
	}{
		"dial timeout": {
			err:  &net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr},
			want: true,
		},
		"proxy connect timeout": {
			err:  &net.OpError{Op: "proxyconnect", Net: "tcp", Err: timeoutErr},
			want: true,
		},
		"read timeout stays post-send": {
			err:  &net.OpError{Op: "read", Net: "tcp", Err: timeoutErr},
			want: false,
		},
		"write timeout stays post-send": {
			err:  &net.OpError{Op: "write", Net: "tcp", Err: timeoutErr},
			want: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := isPreSendTransportError(testCase.err); got != testCase.want {
				t.Fatalf("isPreSendTransportError(%v) = %v, want %v", testCase.err, got, testCase.want)
			}
		})
	}
}

// A failed CONNECT through an HTTP proxy is pre-send for the same structural
// reason a refused dial is, but net tags it Op "proxyconnect", so a gate
// matching only "dial" left every pre-send failure behind a corporate proxy
// unretried.
func TestPreSendClassifiesProxyConnectRefusal(t *testing.T) {
	err := &net.OpError{Op: "proxyconnect", Net: "tcp", Err: syscall.ECONNREFUSED}
	if !isPreSendTransportError(err) {
		t.Fatal("a refused proxy CONNECT was not classified pre-send; no request bytes reached the model host")
	}
	// The op still has to be a connect phase. The same errno on an established
	// connection is post-send and must not replay a POST.
	post := &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNREFUSED}
	if isPreSendTransportError(post) {
		t.Fatal("a read-phase error was classified pre-send")
	}
}

// timeoutError is a net.Error whose Timeout() is true, matching the shape
// net/http surfaces for a connect deadline.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// The pre-send budget bounds CONSECUTIVE connect failures, not the lifetime of
// the call. Without a reset on a successful hop, an early run of dial blips
// permanently spends the budget, so a later blip surfaces as a transport error
// while the status budget still has room. That is the coupling the two counters
// were split apart to remove, in the one ordering the earlier tests did not
// cover: pre-send, then status, then pre-send again.
func TestPreSendBudgetResetsAfterSuccessfulHop(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	var calls int32
	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		switch atomic.AddInt32(&calls, 1) {
		case 1, 2:
			// Two connect failures, spending the pre-send budget under the old
			// lifetime accounting.
			return nil, refused
		case 3:
			// A hop that reaches the server. It declines, so the request was not
			// accepted and retrying stays safe.
			return &http.Response{StatusCode: http.StatusTooManyRequests, Body: http.NoBody, Request: r}, nil
		case 4:
			// One more connect blip. With a lifetime cap this surfaces as an
			// error; the call should still recover.
			return nil, refused
		default:
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: r}, nil
		}
	})}

	resp, err := SendWithRetry(context.Background(), client, http.MethodPost, "https://origin.invalid/v1", []byte("{}"), nil, 3)
	if err != nil {
		t.Fatalf("interleaved connect and status failures returned %v; the pre-send budget did not reset after the hop that reached the server", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 5 {
		t.Fatalf("transport called %d times, want 5 (dial, dial, 429, dial, 200)", got)
	}
}
