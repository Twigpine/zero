package agent

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/errhint"
	"github.com/Gitlawb/zero/internal/trace"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// Mid-stream reconnect: a long autonomous task (a big refactor, a swarm member,
// a headless/cron run) should survive a single transient upstream hiccup
// instead of dying and re-burning every token on a restart. When the initial
// StreamCompletion connect fails with a disconnect-shaped error — before any
// content has been forwarded — re-issue the same request with backoff a few
// times. We retry ONLY the connect (not a partially-consumed stream), so no
// already-forwarded OnText is ever duplicated.
const (
	// maxStreamReconnects is how many times the connect is re-issued after a
	// transient disconnect. 4 (not 2): with jittered exponential backoff this rides
	// out a multi-second network blip (~0.5s + 1s + 2s + 4s worst case) instead of
	// dying on a 2s hiccup and re-burning every token on a restart.
	maxStreamReconnects = 4
	// streamReconnectMax caps a single backoff so the tail attempts don't wait
	// minutes on a long outage.
	streamReconnectMax = 8 * time.Second
)

// streamReconnectBase is the first-retry delay (doubled each subsequent attempt).
// A var, not a const, so tests can shrink it to keep the exhaustion path fast.
var streamReconnectBase = 500 * time.Millisecond

// reconnectNotifier is called before each retry with the 1-based attempt number
// and the max, so the caller can surface a "Reconnecting N/max" notice. Nil is
// fine.
type reconnectNotifier func(attempt, max int)

// reconnectNoticeFor builds a notifier that surfaces reconnect attempts through
// OnReasoning — a non-content channel that is never folded into the answer
// text, so the user sees "Reconnecting…" without corrupting streamed output.
// Returns nil when there is no reasoning sink (the reconnect still happens
// silently).
func reconnectNoticeFor(options Options) reconnectNotifier {
	if options.OnReasoning == nil {
		return nil
	}
	return func(attempt, max int) {
		options.OnReasoning(fmt.Sprintf("\n[connection lost — reconnecting %d/%d…]\n", attempt, max))
	}
}

// stallRetryNoticeFor builds a notifier for the loop's content-stall retry (a
// stream that connected and produced only transient output, then went silent).
// Distinct wording from reconnectNoticeFor's "connection lost" because the
// connection was fine — the model stalled. Surfaced through OnReasoning, the
// non-content channel, so it never folds into the answer text. Nil when there
// is no reasoning sink (the retry still happens silently).
func stallRetryNoticeFor(options Options) reconnectNotifier {
	if options.OnReasoning == nil {
		return nil
	}
	return func(attempt, max int) {
		options.OnReasoning(fmt.Sprintf("\n[no output — model stalled; retrying %d/%d…]\n", attempt, max))
	}
}

// streamWithReconnect issues request via provider.StreamCompletion and, on a
// transient disconnect error, retries the connect up to maxStreamReconnects
// times with exponential backoff. It returns the live stream on success, or the
// last error. A context-cancellation, a non-disconnect error, or a context
// already past its deadline is returned immediately (no retry) — those have
// their own handling (compaction for context-limit, image-rejection, etc.).
func streamWithReconnect(ctx context.Context, provider Provider, request zeroruntime.CompletionRequest, notify reconnectNotifier) (<-chan zeroruntime.StreamEvent, error) {
	recorder := trace.FromContext(ctx)
	stream, err := provider.StreamCompletion(ctx, request)
	if err == nil {
		if recorder != nil {
			recorder.Counter(trace.CounterModelRequests, 1)
		}
		return stream, nil
	}
	for attempt := 1; attempt <= maxStreamReconnects; attempt++ {
		if !shouldReconnect(ctx, err) {
			return nil, err
		}
		if recorder != nil {
			recorder.Counter(trace.CounterReconnectCount, 1)
		}
		if notify != nil {
			notify(attempt, maxStreamReconnects)
		}
		if waitErr := sleepWithContext(ctx, jitteredBackoff(attempt)); waitErr != nil {
			return nil, err // ctx cancelled while waiting; surface the original error
		}
		stream, err = provider.StreamCompletion(ctx, request)
		if err == nil {
			if recorder != nil {
				recorder.Counter(trace.CounterModelRequests, 1)
			}
			return stream, nil
		}
	}
	return nil, err
}

// shouldReconnect reports whether err is a transient disconnect worth retrying.
// It excludes context cancellation/expiry (caller is shutting down) and
// context-limit errors (the compactor recovers those), so the reconnect path
// never fights the existing handlers.
func shouldReconnect(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if isContextLimitError(msg) || isImageRejectionError(err) {
		return false
	}
	// HTTP 5xx statuses are non-reconnectable and must be excluded BEFORE the
	// transport substring match below — otherwise "504 Gateway Timeout" would slip
	// through on the generic "timeout" needle. 503 already exhausted
	// providerio.SendWithRetry (retrying is a redundant double-retry); 500/502/504
	// are non-idempotent by providerio's rule — the completion POST may already
	// have reached the model before the upstream/gateway gave up, so replaying the
	// connect risks duplicate billable work. Digit-boundary matched so an
	// incidental "504" inside a latency/id number is not mistaken for a status.
	if errhint.HasStatusCode(msg, "500", "502", "503", "504") {
		return false
	}
	// Transport-level disconnects only. A genuine transport failure (EOF, reset,
	// refused, timeout) means no response was received, which is safe to reconnect.
	for _, needle := range []string{
		"eof",
		"connection reset",
		"connection refused",
		"broken pipe",
		"connection closed",
		"timeout",
		"timed out",
		"temporarily unavailable",
		"i/o timeout",
		"server closed",
		"unexpected end",
		// Windows mid-stream aborts (WSAECONNABORTED / wsarecv) — host or AV
		// forcibly closes an established socket during CollectStream.
		// "software caused connection abort" is the Unix/macOS ECONNABORTED
		// text; it also appears in midStreamAbortNeedles, which must stay a
		// subset of this list.
		"wsarecv",
		"connection was aborted",
		"software caused connection abort",
		"forcibly closed",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// midStreamAbortNeedles are the transport-abort classes retried AFTER connect
// succeeded and the stream body has started. This is a subset of shouldReconnect:
// connect-phase signals (timeout, connection refused, "temporarily unavailable")
// stay on the connect retry path, because matching them here would re-prefill a
// healthy-but-slow server (ollama cloud header timeouts) and tell the user the
// connection was lost. Issue #973 names WSAECONNABORTED / connection reset /
// unexpected stream EOF.
var midStreamAbortNeedles = []string{
	"unexpected end",
	"connection reset",
	"broken pipe",
	"connection closed",
	"server closed",
	"wsarecv",
	"connection was aborted",
	"software caused connection abort",
	"forcibly closed",
}

var classifiedNonTransportPrefixes = []string{
	"provider request error:",
	"provider error:",
	"auth error:",
	"rate limit error:",
}

// isMidStreamTransportAbort reports whether a collected stream error string is a
// retryable mid-stream transport abort. It does NOT delegate to shouldReconnect:
// that list is a connect-phase argument ("no response was received"). The stream
// body has already started here, so only abort/reset/EOF/close classes retry.
//
// Used for failures DURING the stream body AFTER a successful connect and BEFORE
// tool dispatch — the incomplete turn committed no answer text and executed no
// tools, so a bounded re-issue is safe (same safety rules as the stall path).
func isMidStreamTransportAbort(message string) bool {
	lowered := strings.ToLower(strings.TrimSpace(message))
	if lowered == "" || isContextLimitError(lowered) {
		return false
	}
	for _, prefix := range classifiedNonTransportPrefixes {
		if strings.Contains(lowered, prefix) {
			return false
		}
	}
	if errhint.HasStatusCode(lowered, "500", "502", "503", "504") {
		return false
	}
	if containsWordBoundary(lowered, "eof") {
		return true
	}
	for _, needle := range midStreamAbortNeedles {
		if strings.Contains(lowered, needle) {
			return true
		}
	}
	return false
}

func containsWordBoundary(text, word string) bool {
	start := 0
	for {
		idx := strings.Index(text[start:], word)
		if idx < 0 {
			return false
		}
		pos := start + idx
		endPos := pos + len(word)
		leftBoundary := pos == 0 || !isAlphaNumByte(text[pos-1])
		rightBoundary := endPos == len(text) || !isAlphaNumByte(text[endPos])
		if leftBoundary && rightBoundary {
			return true
		}
		start = pos + 1
	}
}

func isAlphaNumByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
}

// backoffFor is the deterministic exponential base delay for a 1-based attempt,
// capped at streamReconnectMax. Jitter is layered on separately (jitteredBackoff).
func backoffFor(attempt int) time.Duration {
	d := streamReconnectBase
	for i := 1; i < attempt; i++ {
		if d >= streamReconnectMax {
			return streamReconnectMax
		}
		d *= 2
	}
	if d > streamReconnectMax {
		d = streamReconnectMax
	}
	return d
}

// jitteredBackoff adds up to 50% random jitter on top of backoffFor so concurrent
// runs (swarm members, a cron fleet) that all trip on the same outage don't
// reconnect in lockstep and hammer a recovering endpoint. Never shorter than the
// deterministic base, so backoff still grows attempt over attempt.
func jitteredBackoff(attempt int) time.Duration {
	base := backoffFor(attempt)
	return base + time.Duration(rand.Int63n(int64(base/2)+1))
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
