package providerio

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/trace"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

const maxSSELineBytes = 16 * 1024 * 1024

// ErrStreamIdle reports that a streaming upstream stopped sending data without
// closing the connection. Callers surface it as an idle-timeout error.
var ErrStreamIdle = errors.New("idle timeout (upstream stopped sending data)")

// ErrStreamStalled reports that a streaming upstream kept the connection alive
// (SSE keep-alives reset the idle watchdog, so it never fired) but produced no
// actual output for ContentStallTimeout(idle). Without this an upstream that
// heartbeats-but-stalls — observed on chatgpt/gpt-5.x and ollama reasoning
// models — would hang the agent indefinitely.
var ErrStreamStalled = errors.New("stream stalled (upstream kept the connection alive but produced no output)")

// StreamTimeoutMessage returns the human-readable detail for a stream-timeout
// error (ErrStreamIdle or ErrStreamStalled), given the configured idle timeout.
// Callers prepend their own "provider stream error: " prefix. A stalled stream
// gets a distinct, actionable message — it did NOT stop sending data (keep-alives
// kept arriving); it just produced no output, which usually means the model is
// stuck or very slow.
func StreamTimeoutMessage(err error, idleTimeout time.Duration) string {
	if errors.Is(err, ErrStreamStalled) {
		return fmt.Sprintf("no output for %s (the model kept the connection alive but produced nothing — it may be stuck; try a faster model or lower reasoning effort)", ContentStallTimeout(idleTimeout))
	}
	return fmt.Sprintf("idle timeout after %s (upstream stopped sending data)", idleTimeout)
}

// DefaultStreamIdleTimeout is the single source of truth for how long every
// provider waits on a silent stream before aborting it. The watchdog only fires
// on genuine silence — SSE keep-alive comments reset it (see
// ScanSSEDataWithContext) — so this needs to be generous enough for slow cloud
// and reasoning backends that pause for minutes without heartbeating, while
// still bounding a truly hung connection. 90s was too aggressive and killed
// healthy long generations; 5 minutes is the floor a real stall must cross.
// Override globally with ZERO_STREAM_IDLE_TIMEOUT.
const DefaultStreamIdleTimeout = 5 * time.Minute

// ContentStallTimeout bounds a heartbeat-but-no-output stream: keep-alives reset
// the idle watchdog (a heartbeating upstream is not "dead"), but if NO real data
// line arrives for this long the stream is aborted (ErrStreamStalled). Scaled
// above idleTimeout (so a slow-but-producing request that emits data between
// heartbeats is never killed — the watchdog only ever fires when NOTHING real
// arrives), but only 1.2× rather than the old 2×: a genuine heartbeat-pause
// stall on chatgpt/gpt-5.x rarely recovers, so a ~10-minute dead wait (at the 5m
// idle default) was a terrible UX for a doomed turn. At the default idle this is
// 6 minutes; it still scales with ZERO_STREAM_IDLE_TIMEOUT. A returned value <= 0
// (idle watchdog disabled) leaves the content watchdog off too.
func ContentStallTimeout(idleTimeout time.Duration) time.Duration {
	if idleTimeout <= 0 {
		return 0
	}
	// 1.2× computed as idle + idle/5 (not idle*6/5), plus a clamp: a
	// pathologically large ZERO_STREAM_IDLE_TIMEOUT could make idle*6 overflow
	// int64 and wrap to a NEGATIVE duration, which would arm the content timer
	// to fire immediately and abort every stream. Clamping to the max duration
	// on overflow just means "effectively no content watchdog" — the sane
	// result for an absurd idle timeout.
	extra := idleTimeout / 5
	if idleTimeout > math.MaxInt64-extra {
		return math.MaxInt64
	}
	return idleTimeout + extra
}

// streamIdleTimeoutEnv is the global override for the stream idle timeout. It
// accepts a Go duration ("5m", "300s", "90s") or a bare number of seconds
// ("300"). A value of "0", "off", "none", or "disabled" turns the watchdog off
// entirely (streams may then hang until the HTTP/transport layer gives up).
const streamIdleTimeoutEnv = "ZERO_STREAM_IDLE_TIMEOUT"

// ResolveStreamIdleTimeout selects the effective stream idle timeout. Precedence:
// an explicit positive option (e.g. set by a test) wins; otherwise the
// ZERO_STREAM_IDLE_TIMEOUT env override if set and valid; otherwise
// DefaultStreamIdleTimeout. A returned value <= 0 disables the idle watchdog.
func ResolveStreamIdleTimeout(option time.Duration) time.Duration {
	if option > 0 {
		return option
	}
	if raw := strings.TrimSpace(os.Getenv(streamIdleTimeoutEnv)); raw != "" {
		switch strings.ToLower(raw) {
		case "0", "off", "none", "disabled":
			return 0
		}
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
		if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
		// Unparseable / non-positive: fall through to the default rather than
		// silently disabling the watchdog on a typo.
	}
	return DefaultStreamIdleTimeout
}

// NormalizeBaseURL trims trailing slashes and validates an HTTP API base URL.
func NormalizeBaseURL(baseURL string, defaultBaseURL string, label string) (string, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if _, err := url.ParseRequestURI(baseURL); err != nil {
		return "", fmt.Errorf("invalid %s base URL: %w", label, err)
	}
	return baseURL, nil
}

// sharedHTTPClient is the process-wide client used when a provider supplies none.
// It tunes the default transport to defeat the stale-pooled-connection hang: Go
// keeps idle keep-alive connections in a pool, and a later request can reuse one
// the server/NAT has silently dropped. Because the model call is a POST (non-
// idempotent), Go will NOT auto-retry it on a fresh connection — so it blocks
// forever waiting for a response that never arrives. This is provider-agnostic
// (every provider shares the default transport), which is why it reproduced on
// BOTH chatgpt and ollama, and it surfaces on macOS far more than Linux because
// macOS keeps dead pooled connections around longer.
//
//   - ResponseHeaderTimeout bounds the gap between finishing the request and the
//     first response header, so a reused-dead connection fails fast (then the
//     reconnect path re-dials) instead of hanging. It does NOT bound streaming
//     after the 200 header arrives, so slow first tokens / long reasoning are
//     unaffected (the stream-idle + content watchdogs cover that).
//   - IdleConnTimeout is shortened so a connection that went idle across a pause
//     is closed (and re-dialed fresh) rather than reused after it has gone stale.
//   - DisableKeepAlives on darwin only: ResponseHeaderTimeout/IdleConnTimeout
//     above catch a reused connection that's fully dead (never responds) or
//     idle past the timeout, but not one that's alive-but-severely-degraded —
//     e.g. reused shortly after a prior request on the same host (well within
//     30s, common across quick retries of the same turn), where it still
//     delivers real bytes, just at a crippled rate, resetting Zero's stream
//     idle/content-stall watchdogs (which only fire on true silence) without
//     ever recovering. That degraded-not-dead case is indistinguishable from
//     genuine backend slowness from inside the stream, so the only reliable
//     fix is removing pooling from the equation entirely on the one platform
//     where this class of bug has actually reproduced. A fresh TCP+TLS
//     handshake per request costs low tens of milliseconds — negligible next
//     to the minutes-long stalls this avoids — and this doesn't touch
//     Linux/Windows, where the underlying OS doesn't keep dead/degraded
//     pooled connections around as long.
var sharedHTTPClient = func() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// 120s, not 60s: a slow cloud proxy (e.g. ollama `*:cloud`) can withhold its
	// 200 response header until the upstream model emits a first token, so a 60s cap
	// risked aborting a legitimately-slow-but-alive request. 120s still bounds a
	// truly dead reused connection (which never responds) while tolerating slow
	// header delivery; slow first tokens after the header are covered by the idle +
	// content-stall watchdogs.
	transport.ResponseHeaderTimeout = 120 * time.Second
	transport.IdleConnTimeout = 30 * time.Second
	// Periodically close idle connections to prevent stale HTTP/2
	// connections from causing PROTOCOL_ERROR on the next request.
	// OpenRouter (and other HTTP/2 backends) can silently RST or
	// GOAWAY a connection that has been idle in the pool; the next
	// request over that connection gets a PROTOCOL_ERROR. Closing
	// idle connections every 30s forces a fresh dial so the error
	// window is bounded to ~30s while reuse still benefits healthy
	// connections. No-op on darwin where DisableKeepAlives already
	// prevents pooling.
	if runtime.GOOS != "darwin" {
		startIdleConnCloser(transport, defaultIdleConnCloseInterval)
	}
	transport.DisableKeepAlives = runtime.GOOS == "darwin"
	return &http.Client{Transport: transport}
}()

const defaultIdleConnCloseInterval = 30 * time.Second

// startIdleConnCloser launches a background goroutine that periodically closes
// idle keep-alive connections on transport until the returned stop function is
// invoked.
func startIdleConnCloser(transport *http.Transport, interval time.Duration) func() {
	if interval <= 0 || transport == nil {
		return func() {}
	}
	ticker := time.NewTicker(interval)
	done := make(chan struct{})
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				transport.CloseIdleConnections()
			}
		}
	}()
	return func() {
		select {
		case <-done:
		default:
			close(done)
		}
	}
}

// HTTPClient returns the configured client or the shared, stall-hardened default.
func HTTPClient(client *http.Client) *http.Client {
	if client != nil {
		return client
	}
	return sharedHTTPClient
}

// SendEvent writes a provider event without blocking cancellation cleanup.
func SendEvent(ctx context.Context, events chan<- zeroruntime.StreamEvent, event zeroruntime.StreamEvent) {
	select {
	case <-ctx.Done():
		if event.Type == zeroruntime.StreamEventError {
			select {
			case events <- event:
			default:
			}
		}
	case events <- event:
	}
}

// scanSSEPayloads accumulates SSE "data:" lines into payloads (joined across
// continuation lines, flushed on a blank line or EOF) and forwards each to
// handle. It is the shared core of ScanSSEData and the idle-aware variant.
// onComment (optional) fires for ":"-prefixed comment lines — SSE keep-alive
// heartbeats (e.g. OpenRouter's ": OPENROUTER PROCESSING") that carry no data
// but prove the upstream is alive; returning false stops the scan.
func scanSSEPayloads(scanner *bufio.Scanner, handle func(data string) bool, onComment func() bool) error {
	dataLines := []string{}
	flush := func() bool {
		if len(dataLines) == 0 {
			return true
		}
		data := strings.TrimSpace(strings.Join(dataLines, "\n"))
		dataLines = dataLines[:0]
		if data == "" || data == "[DONE]" {
			return true
		}
		return handle(data)
	}

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			if !flush() {
				return nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			if onComment != nil && !onComment() {
				return nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimLeft(strings.TrimPrefix(line, "data:"), " \t"))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	flush()
	return nil
}

// ScanSSEDataWithContext parses SSE data payloads while enforcing an idle
// timeout and honoring ctx cancellation. The blocking scan runs on a goroutine
// that forwards each completed payload over a buffered channel; this consumer
// selects on ctx.Done, the idle timer, and incoming payloads. When the upstream
// goes silent for idleTimeout, cancel is invoked to abort the in-flight request
// (unblocking the reader) and ErrStreamIdle is returned. On ctx cancellation
// ctx.Err() is returned. A non-positive idleTimeout disables the watchdog.
func ScanSSEDataWithContext(
	ctx context.Context,
	cancel context.CancelFunc,
	reader io.Reader,
	idleTimeout time.Duration,
	handle func(data string) bool,
) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 4096), maxSSELineBytes)

	type payload struct {
		data      string
		keepAlive bool
	}
	payloads := make(chan payload)
	scanDone := make(chan error, 1)

	go func() {
		scanDone <- scanSSEPayloads(scanner, func(data string) bool {
			select {
			case payloads <- payload{data: data}:
				return true
			case <-ctx.Done():
				return false
			}
		}, func() bool {
			// Comment keep-alives carry no payload but must feed the idle
			// watchdog: a heartbeating upstream is NOT idle, and aborting it
			// killed healthy long-running requests. The marker is forwarded to
			// the consumer goroutine because the timer is not safe to reset
			// from this one.
			select {
			case payloads <- payload{keepAlive: true}:
				return true
			case <-ctx.Done():
				return false
			}
		})
		close(payloads)
	}()

	// The idle watchdog is optional. When idleTimeout <= 0 it is disabled, but we
	// STILL run the goroutine + select loop so ctx cancellation is always honored
	// (a nil idleC channel simply never fires in the select).
	var idleC, contentC <-chan time.Time
	resetIdle := func() {}
	resetContent := func() {}
	if idleTimeout > 0 {
		// reset drains the timer's channel if it already fired before re-arming, so
		// a stale tick can't trip the watchdog after activity resumed.
		reset := func(t *time.Timer, d time.Duration) func() {
			return func() {
				if !t.Stop() {
					select {
					case <-t.C:
					default:
					}
				}
				t.Reset(d)
			}
		}
		idle := time.NewTimer(idleTimeout)
		defer idle.Stop()
		idleC = idle.C
		resetIdle = reset(idle, idleTimeout)

		// Content watchdog: only real data lines reset it (keep-alives do not), so a
		// stream that heartbeats without producing output is bounded instead of
		// hanging forever.
		contentTimeout := ContentStallTimeout(idleTimeout)
		content := time.NewTimer(contentTimeout)
		defer content.Stop()
		contentC = content.C
		resetContent = reset(content, contentTimeout)
	}

	for {
		select {
		case <-ctx.Done():
			// Abort the in-flight request so the reader goroutine unblocks and
			// exits on its own; do not wait for it (it may be parked in a read
			// that only the request-context cancel can interrupt).
			cancel()
			return ctx.Err()
		case <-idleC:
			// Upstream went silent without closing. Abort the read and surface
			// a timeout instead of blocking the agent forever.
			cancel()
			return ErrStreamIdle
		case <-contentC:
			// Upstream kept heartbeating (idle watchdog never fired) but produced
			// no output for the content window. Treat as stalled and abort rather
			// than hanging forever.
			cancel()
			return ErrStreamStalled
		case item, ok := <-payloads:
			if !ok {
				// Reader finished: deliver its terminal status (EOF -> nil,
				// scanner error, or ctx cancel observed inside the goroutine).
				if err := <-scanDone; err != nil {
					return err
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				return nil
			}
			resetIdle()
			if item.keepAlive {
				// A heartbeat keeps the connection "alive" (idle watchdog) but is
				// NOT output, so it must not reset the content watchdog.
				continue
			}
			resetContent()
			if !handle(item.data) {
				// The provider asked to stop (e.g. it already emitted an error
				// for this payload). Abort the read and end like ScanSSEData:
				// return nil so callers fall through to their post-scan checks.
				// Do not stamp FirstToken — this payload was not accepted model
				// output, so counting it as first-token time would be misleading.
				cancel()
				return nil
			}
			// First accepted non-keepalive payload = first real model output. Stamp
			// once; later real payloads are no-ops. nil recorder (untraced run) is a
			// no-op.
			trace.FromContext(ctx).StampFirstToken()
		}
	}
}

// ClassifiedError normalizes provider HTTP/stream errors and redacts secrets.
func ClassifiedError(statusCode int, message string, secrets ...string) string {
	switch statusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		// Lead with an actionable instruction rather than the raw upstream auth blurb
		// (which often points the user at the wrong provider's dashboard URL). Keep a
		// redacted, one-line upstream detail for context — never the raw body. (AUDIT-H7)
		curated := "auth error: your API key is missing or invalid — run `zero setup`, `zero auth openrouter` for OpenRouter, or set the provider's API key, then retry."
		if detail := strings.TrimSpace(Redact(message, secrets...)); detail != "" {
			return curated + " (provider said: " + detail + ")"
		}
		return curated
	case http.StatusTooManyRequests, http.StatusServiceUnavailable, 529:
		return Redact("rate limit error: "+message, secrets...)
	default:
		prefix := "provider error: "
		if statusCode >= http.StatusBadRequest && statusCode < http.StatusInternalServerError {
			prefix = "provider request error: "
		}
		return Redact(prefix+message, secrets...)
	}
}

// tokenShape matches a long credential-like token (API key / JWT) so the Bearer
// heuristic in Redact only scrubs an actual token, not ordinary words.
var tokenShape = regexp.MustCompile(`^[A-Za-z0-9._\-]{16,}$`)

// looksLikeToken reports whether w is credential-shaped: long, token-charset, and
// either very long or containing a digit (so "Bearer authentication" / "Bearer
// token" in upstream help text is not mangled, while real keys/JWTs are redacted).
func looksLikeToken(w string) bool {
	w = strings.Trim(w, ".,;:\"'`)(")
	if !tokenShape.MatchString(w) {
		return false
	}
	if len(w) >= 24 {
		return true
	}
	return strings.ContainsAny(w, "0123456789")
}

// Redact removes known API-key and bearer-token forms from provider messages.
func Redact(message string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	words := strings.Fields(message)
	for index := 0; index < len(words)-1; index++ {
		// Only redact the word after "Bearer" when it is actually token-shaped, so the
		// provider's own help text ("use Bearer authentication", "Bearer token") is no
		// longer corrupted into "authorization [REDACTED]". (AUDIT-H7)
		if strings.EqualFold(strings.TrimRight(words[index], ":"), "Bearer") && looksLikeToken(words[index+1]) {
			words[index+1] = "[REDACTED]"
		}
	}
	return strings.Join(words, " ")
}

// upstreamFailureMarkers are transport-level failures that mean a request never
// reached the model: the server the client connected to could not establish a
// connection to its upstream. They distinguish a connectivity problem (outside
// the agent's control) from the model rejecting the request.
var upstreamFailureMarkers = []string{
	"TLS handshake timeout",
	"context deadline exceeded",
	"connection refused",
	"no such host",
	"network is unreachable",
	"i/o timeout",
}

// UpstreamUnreachable detects a provider error that is really a connectivity
// failure to an upstream host rather than a model/request error, and rewrites it
// into a clear, actionable message. The common case is a local Ollama daemon
// serving a "-cloud" model: it answers on localhost but returns HTTP 502 because
// it cannot reach its own cloud backend, surfacing an opaque proxied string like
// `Post "https://ollama.com:443/...": net/http: TLS handshake timeout`. It
// matches only when both a transport failure marker and a concrete host are
// present, so the agent's own request-deadline cancellations are left untouched.
// Non-matching messages are returned unchanged with false.
func UpstreamUnreachable(message string) (string, bool) {
	reason := ""
	for _, marker := range upstreamFailureMarkers {
		if strings.Contains(message, marker) {
			reason = marker
			break
		}
	}
	host := upstreamHost(message)
	if reason == "" || host == "" {
		return message, false
	}

	return "upstream unreachable: the model server could not connect to " + host +
		" (" + reason + "). The request never reached the model — this is a network failure " +
		"between the model server and its upstream, not a model error. Verify the host is " +
		"reachable from the machine running the model server (DNS/proxy/VPN/firewall); a local " +
		"daemon proxying a cloud model — e.g. an Ollama daemon serving a \"-cloud\" model — must " +
		"itself be able to reach the internet.", true
}

// upstreamHost extracts the unreachable host from a Go transport error. It
// handles the two shapes these errors take: a quoted request URL
// (`... "https://host:port/path": ...`) and a raw dial target
// (`dial tcp host:port: ...`). The URL form is preferred when both are present.
// It returns "" when neither yields a host.
func upstreamHost(message string) string {
	if index := strings.Index(message, "\"http"); index >= 0 {
		rest := message[index+1:]
		if end := strings.IndexByte(rest, '"'); end >= 0 {
			if parsed, err := url.Parse(rest[:end]); err == nil && parsed.Host != "" {
				return parsed.Host
			}
		}
	}
	const dialPrefix = "dial tcp "
	if index := strings.Index(message, dialPrefix); index >= 0 {
		rest := message[index+len(dialPrefix):]
		if end := strings.Index(rest, ": "); end >= 0 {
			rest = rest[:end]
		}
		if host := strings.TrimSpace(rest); host != "" && !strings.Contains(host, " ") {
			return host
		}
	}
	return ""
}

// PositiveOrDefault validates optional max token settings.
func PositiveOrDefault(value int, fallback int, label string) (int, error) {
	if value == 0 {
		return fallback, nil
	}
	if value < 0 {
		return 0, fmt.Errorf("%s must be a positive integer", label)
	}
	return value, nil
}
