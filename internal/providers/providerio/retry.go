package providerio

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/trace"
)

// Transient-failure retry, shared by every provider.
//
// SendWithRetry centralizes one retry policy so all providers behave consistently
// (previously only the OpenAI provider retried; Anthropic and Gemini surfaced the
// first failure).
//
// What is retried: 429 (rate limit) and 503 (service unavailable) — the statuses
// where the server explicitly did NOT accept the request — plus PROVABLY
// pre-send transport failures (DNS resolution, a refused/unreachable dial, a TLS
// handshake timeout) where no request bytes ever left this host. In every one of
// those cases we KNOW the server did not receive and process the request, so a
// replay cannot duplicate work. Other 5xx (500/502/504) and every ambiguous or
// post-send transport error (connection reset, broken pipe, EOF, a generic i/o
// timeout, context deadline) are NOT retried: a completion POST is
// non-idempotent and may already have reached and been processed by the server,
// so replaying it could duplicate (billable) work. Only the INITIAL request is
// ever in scope; once the response body starts streaming it is never re-issued.
// Pre-send classification is by syscall errno (portable across POSIX and
// Windows), scoped to an Op=="dial" *net.OpError so the same errno raised on a
// post-send read/write can't be misread as pre-send. NXDOMAIN is treated as
// permanent (not retried), and the pre-send path fails fast on its own
// sub-second schedule rather than the seconds-long 429 backoff.

const defaultMaxRetryAttempts = 6

// maxBackoff caps a single backoff wait so a hostile or buggy Retry-After can't
// stall the agent for minutes.
const maxBackoff = 30 * time.Second

// retryBackoffBase is the first wait when the server supplied no Retry-After.
// Rate-limit windows are measured in seconds, not milliseconds: retrying a 429
// after 400ms almost always burns the attempt while still limited, so the
// schedule is 2s, 4s, 8s, 16s, then maxBackoff. A var so tests can shrink it.
var retryBackoffBase = 2 * time.Second

// ShrinkBackoffForTest sets backoff bases to 1ms for test duration.
func ShrinkBackoffForTest() func() {
	savedRetry, savedPreSend := retryBackoffBase, preSendBackoffBase
	retryBackoffBase = time.Millisecond
	preSendBackoffBase = time.Millisecond
	return func() {
		retryBackoffBase = savedRetry
		preSendBackoffBase = savedPreSend
	}
}

// preSendMaxAttempts bounds retries of a provably pre-send transport failure.
// Unlike a 429 (whose window is measured in seconds), a refused/unreachable dial
// either recovers within a few hundred ms or is a deterministic misconfiguration
// (wrong host/port, dead local daemon) that will never recover — so a short,
// low-count schedule fails fast instead of stalling the agent ~60s on the
// rate-limit schedule. This bounds the pre-send path independently of the caller's
// maxAttempts, which is tuned for 429/503.
const preSendMaxAttempts = 3

// preSendBackoffBase is the first wait before replaying a pre-send transport
// failure. Modeled on the mid-stream reconnect base (internal/agent/reconnect.go),
// it is sub-second so a transient dial blip recovers quickly and a permanent one
// fails fast. A var so tests can shrink it.
var preSendBackoffBase = 500 * time.Millisecond

// SendWithRetry issues an HTTP request, retrying ONLY the safe-to-replay server
// responses (429 and 503, see ShouldRetryStatus) up to maxAttempts — backing off
// between tries and honoring a server Retry-After header and context
// cancellation. The one transport-error exception is a PROVABLY pre-send failure
// (see isPreSendTransportError): a refused/unreachable dial or DNS failure where
// no request bytes left this host, retried on its own short schedule
// (preSendMaxAttempts / preSendBackoff). Every other 5xx and every ambiguous or
// post-send transport error is returned immediately, never replayed, because a
// non-idempotent completion POST may already have been received. The request is
// rebuilt from body each attempt; setHeader (if non-nil) sets headers on every
// attempt.
//
// It returns the final *http.Response (which the caller inspects for a non-2xx
// status, exactly as before) or an error for a network failure / context
// cancellation. Retries exhausted on a retryable status return that response,
// not an error, so the caller's existing HTTP-error path still runs.
func SendWithRetry(
	ctx context.Context,
	client *http.Client,
	method string,
	url string,
	body []byte,
	setHeader func(*http.Request),
	maxAttempts int,
) (*http.Response, error) {
	if maxAttempts <= 0 {
		maxAttempts = defaultMaxRetryAttempts
	}
	if client == nil {
		client = http.DefaultClient
	}
	// Redirects are still FOLLOWED, so a redirecting gateway keeps working, but
	// entering one makes that attempt non-retryable. The hazard is that client.Do
	// can transmit the original POST to the first host, follow a 307/308, and then
	// fail the dial to the redirect target: that error is an Op=="dial" error, so
	// isPreSendTransportError would read it as "nothing was sent" and replay a
	// completion the first host already received. Observing the redirect rather
	// than blocking it keeps both properties, since a request that was redirected
	// is never replayed while redirecting endpoints stay usable. The caller's own
	// CheckRedirect still runs and still decides whether to proceed.
	redirected := false
	callerCheckRedirect := client.CheckRedirect
	redirectAwareClient := *client
	redirectAwareClient.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		redirected = true
		if callerCheckRedirect != nil {
			return callerCheckRedirect(request, via)
		}
		// Mirror net/http's default policy, which a nil CheckRedirect would have
		// applied: stop after 10 hops.
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	// The two retry budgets are fully independent, each with its own counter and
	// its own backoff ordinal, because they answer different questions: a
	// dial/DNS/TLS failure is a fast local blip (preSendMaxAttempts, sub-second
	// schedule) while a 429/503 is a server-side window measured in seconds
	// (maxAttempts, retryBackoffBase). A single shared loop counter coupled them
	// in both directions: status retries starved the pre-send budget, and
	// pre-send retries shortened the status window and started its backoff at a
	// later rung. Neither kind of failure now consumes or advances the other.
	preSendAttempts := 0
	statusAttempts := 0
	for {
		// Per attempt: only a redirect entered by THIS attempt can make its own
		// transport error unsafe to replay.
		redirected = false
		request, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		if setHeader != nil {
			setHeader(request)
		}

		connectSpan := trace.FromContext(ctx).Span(trace.SpanProviderConnect)
		response, err := redirectAwareClient.Do(request)
		connectSpan.End()
		if err != nil {
			// Context cancellation always surfaces as cancellation, never a retry.
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			// A transport failure on a POST does NOT usually mean the server didn't
			// receive it — the request may have arrived and be generating a
			// (billable, non-idempotent) completion while only the response or
			// connection failed. Replaying that could duplicate work, so it is
			// surfaced immediately. The one safe exception is a PROVABLY pre-send
			// failure (see isPreSendTransportError): the request never left this
			// host, so a replay cannot duplicate anything — exactly like a 429/503
			// where we KNOW the request was not accepted. The pre-send path uses
			// its own short bound and sub-second schedule (preSendMaxAttempts /
			// preSendBackoff), not the seconds-long 429 schedule, so a permanent
			// dial failure fails fast instead of stalling the agent for ~60s.
			// A redirect means the original POST already left this host, so the
			// dial that failed belongs to a later hop and replaying would duplicate
			// a completion the first host may already have processed.
			if isPreSendTransportError(err) && !redirected {
				preSendAttempts++
				if preSendAttempts < preSendMaxAttempts {
					if r := trace.FromContext(ctx); r != nil {
						r.Counter(trace.CounterRetryCount, 1)
					}
					if preSendBackoff(ctx, preSendAttempts) {
						continue
					}
					return nil, ctx.Err()
				}
			}
			return nil, err
		}

		// Deliberately NOT guarded on `redirected`, unlike the transport-error
		// branch above. The two differ in what is known about the request. A
		// transport failure after a redirect leaves it unknown whether the
		// redirect target received and began processing the POST, so replaying
		// could duplicate billable work. Here every hop answered: a 307/308 is the
		// origin declining and delegating, and a retryable status is the target
		// declining as well, so nothing accepted the request and a replay
		// duplicates nothing.
		//
		// The exposure is also identical to a retryable status with no redirect in
		// play, which this package has always retried. Guarding on redirects here
		// would not close a gap, it would make a rate-limited completion behind a
		// redirecting gateway fail outright instead of backing off.
		// The transport hop succeeded, so the connect phase is healthy again and
		// the pre-send budget starts over. It bounds CONSECUTIVE connect failures,
		// not the lifetime of the call: without this reset, two early dial retries
		// followed by a 429 and then one more dial blip would surface a transport
		// error while the status budget still had room, which is the coupling
		// these two counters were split apart to remove. Total work stays bounded,
		// since every reset costs a status attempt and those are capped.
		preSendAttempts = 0

		if ShouldRetryStatus(response.StatusCode) {
			statusAttempts++
			if statusAttempts < maxAttempts {
				if r := trace.FromContext(ctx); r != nil {
					r.Counter(trace.CounterRetryCount, 1)
				}
				wait := RetryAfter(response)
				_ = response.Body.Close()
				if Backoff(ctx, statusAttempts, wait) {
					continue
				}
				// Backoff aborted: the only reason it returns false is ctx cancellation.
				return nil, ctx.Err()
			}
		}

		// Success, a non-retryable status, or retries exhausted on a retryable
		// status. If the context was cancelled meanwhile, surface that instead of
		// a misclassified upstream status.
		if ctx.Err() != nil {
			_ = response.Body.Close()
			return nil, ctx.Err()
		}
		return response, nil
	}
}

// isPreSendTransportError reports whether a transport error PROVES no request
// bytes reached the server, so replaying the request cannot duplicate a
// billable completion. Only narrow, unambiguous pre-connect failures qualify: a
// recoverable DNS lookup failure, a refused/unreachable dial, and a TLS
// handshake timeout (the handshake completes before any HTTP request bytes are
// written).
//
// The refused/unreachable errnos (ECONNREFUSED/ENETUNREACH/EHOSTUNREACH) are
// matched by syscall errno via errors.Is — portable across platforms, since Go
// maps the WSA* codes on Windows too — but ONLY when carried by an Op=="dial"
// *net.OpError. errors.Is matches an errno anywhere in the chain regardless of
// which socket operation raised it, and the kernel raises these same errnos on
// an established connection when a route drops mid-generation; scoping to the
// dial phase is what makes "no request bytes were sent" structural rather than
// argued, so a post-send read/write can never replay a non-idempotent POST.
// NXDOMAIN is treated as permanent (deterministic) and is NOT retried.
//
// Ambiguous or post-send failures are deliberately excluded and checked FIRST,
// so an error that is both is never treated as pre-send: a reset or broken pipe
// can follow a request that was already sent, and a bare "i/o timeout" covers
// read timeouts after send as well as dial timeouts, so none of these prove the
// request was not received and a non-idempotent POST must not be replayed on
// them.
func isPreSendTransportError(err error) bool {
	if err == nil {
		return false
	}
	// DNS resolution precedes any connection, so a lookup failure proves nothing
	// was sent — but only retry a lookup that could plausibly succeed on a replay.
	// NXDOMAIN (IsNotFound) is authoritative and deterministic (a mistyped or
	// non-existent host), so retrying it just stalls the agent through the whole
	// backoff schedule before failing anyway; a timeout or temporary SERVFAIL is
	// worth another try. errors.As unwraps url.Error/OpError to reach the DNSError.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return !dnsErr.IsNotFound
	}
	// Post-send / ambiguous failures, excluded first. EOF and reset are matched
	// by identity so wording (or a hostname containing "eof") can't fool them.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || isConnResetErrno(err) {
		return false
	}
	// A CONNECT-phase timeout is provably pre-send for the same structural reason
	// a refused dial is: the connection was never established, so no request
	// bytes left this host. It has to be admitted BEFORE the wording exclusions
	// below, because net surfaces it with "i/o timeout" in the message, which the
	// blanket exclusion would otherwise reject. The gate is what keeps that safe:
	// a post-send read timeout is also Timeout(), but carries Op "read", so it
	// never reaches here.
	if opErr := connectOpError(err); opErr != nil && opErr.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "i/o timeout") {
		return false
	}
	// A refused/unreachable connection is provably pre-send ONLY when it was
	// raised during the DIAL (connection-establishment) phase. The same errnos
	// are also raised by the kernel on an ESTABLISHED connection — when a route
	// drops or an ICMP unreachable arrives mid-generation — which is post-send.
	// errors.Is matches an errno anywhere in the chain regardless of the
	// operation, so we first require an Op=="dial" *net.OpError: that makes "no
	// request bytes left this host" structural, so a post-send read/write carrying
	// one of these can never replay a non-idempotent POST. dialPreSendErrnos is
	// platform-specific: on Windows a refused dial carries the raw WSA* errno
	// (WSAECONNREFUSED etc.), which does NOT satisfy errors.Is against the POSIX
	// syscall.ECONNREFUSED, so the Windows list adds those codes. The string
	// markers only catch a POSIX dial error already flattened past its errno.
	if connectOpError(err) != nil {
		for _, errno := range dialPreSendErrnos {
			if errors.Is(err, errno) {
				return true
			}
		}
		switch {
		case strings.Contains(msg, "connection refused"),
			strings.Contains(msg, "network is unreachable"),
			strings.Contains(msg, "no route to host"):
			return true
		}
	}
	// A TLS handshake times out after the TCP dial connects but before any HTTP
	// request bytes are written, so it is structurally pre-send. net/http surfaces
	// it as its own error string, not a *net.OpError, so it is matched by wording
	// independent of the dial gate.
	if strings.Contains(msg, "tls handshake timeout") {
		return true
	}
	return false
}

// connectOpError returns the outermost *net.OpError in err's chain when it comes
// from the connection-establishment phase, else nil. Go tags a direct dial with
// Op "dial" and a failed CONNECT through an HTTP proxy with Op "proxyconnect";
// a failure on an already-established connection carries Op "read"/"write".
// This is how isPreSendTransportError scopes the pre-send match to the connect
// phase so a post-send failure can't be misclassified as pre-send.
//
// "proxyconnect" belongs here for the same reason "dial" does: when HTTPS_PROXY
// is set and the CONNECT fails, the tunnel to the model host was never built, so
// no request bytes reached it. Omitting it meant every pre-send failure behind a
// corporate proxy fell through to no-retry, which is the environment where
// transient connect failures are most common.
func connectOpError(err error) *net.OpError {
	var opErr *net.OpError
	if errors.As(err, &opErr) && (opErr.Op == "dial" || opErr.Op == "proxyconnect") {
		return opErr
	}
	return nil
}

// ShouldRetryStatus reports whether an HTTP status is safe to retry for a
// non-idempotent completion POST: 429 (Too Many Requests), 503 (Service
// Unavailable), and 529 (Anthropic's "overloaded"). All mean the server
// explicitly did NOT accept the request — it was rate-limited or the service
// was unavailable — so replaying it cannot duplicate work. Other 5xx
// (500/502/504) are deliberately NOT retried: they do not guarantee the
// request had no effect (e.g. a 504 gateway timeout may follow an upstream
// that already produced a billable completion), so replaying them risks
// duplicate work.
func ShouldRetryStatus(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable || code == 529
}

// Backoff waits before retry attempt N (1-based), returning false if the context
// is cancelled during the wait. A server-supplied (positive) Retry-After wins;
// otherwise the wait doubles from retryBackoffBase per attempt. Either way the
// wait is capped at maxBackoff.
func Backoff(ctx context.Context, attempt int, retryAfter time.Duration) bool {
	timer := time.NewTimer(backoffWait(attempt, retryAfter))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// preSendBackoff waits before pre-send retry attempt N (1-based) on a short
// sub-second schedule (500ms, 1s, ...), returning false if the context is
// cancelled during the wait. Separate from Backoff's seconds-long rate-limit
// schedule because a pre-send dial failure recovers in milliseconds or never.
func preSendBackoff(ctx context.Context, attempt int) bool {
	timer := time.NewTimer(preSendBackoffWait(attempt))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// preSendBackoffWait computes the pre-send wait before retry attempt N (1-based):
// exponential from preSendBackoffBase (500ms, 1s, 2s, ...), capped at maxBackoff.
// The exponent is clamped so a large attempt count cannot overflow.
func preSendBackoffWait(attempt int) time.Duration {
	exponent := attempt - 1
	if exponent > 5 {
		exponent = 5
	}
	if exponent < 0 {
		exponent = 0
	}
	wait := preSendBackoffBase * time.Duration(1<<exponent)
	if wait > maxBackoff {
		wait = maxBackoff
	}
	return wait
}

// backoffWait computes the wait before retry attempt N (1-based): Retry-After
// when supplied, else exponential from retryBackoffBase, both capped at
// maxBackoff. The exponent is clamped so a large attempt count cannot overflow.
func backoffWait(attempt int, retryAfter time.Duration) time.Duration {
	wait := retryAfter
	if wait <= 0 {
		exponent := attempt - 1
		if exponent > 5 {
			exponent = 5
		}
		if exponent < 0 {
			exponent = 0
		}
		wait = retryBackoffBase * time.Duration(1<<exponent)
	}
	if wait > maxBackoff {
		wait = maxBackoff
	}
	return wait
}

// RetryAfter parses a response's Retry-After header (delay-seconds or an HTTP
// date) into a positive duration, or 0 when absent/unparseable. The result is
// capped at maxBackoff by Backoff.
func RetryAfter(response *http.Response) time.Duration {
	if response == nil {
		return 0
	}
	value := strings.TrimSpace(response.Header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		if delay := time.Until(when); delay > 0 {
			return delay
		}
	}
	return 0
}
