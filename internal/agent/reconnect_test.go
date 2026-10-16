package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// flakyProvider fails the first failBefore connect attempts with failErr, then
// succeeds with a single-text stream.
type flakyProvider struct {
	calls      int32
	failBefore int32
	failErr    error
}

func (p *flakyProvider) StreamCompletion(_ context.Context, _ zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	n := atomic.AddInt32(&p.calls, 1)
	if n <= p.failBefore {
		return nil, p.failErr
	}
	ch := make(chan zeroruntime.StreamEvent, 1)
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}
	close(ch)
	return ch, nil
}

func TestStreamWithReconnectRecoversFromTransientDisconnect(t *testing.T) {
	p := &flakyProvider{failBefore: 1, failErr: errors.New("unexpected EOF")}
	stream, err := streamWithReconnect(context.Background(), p, zeroruntime.CompletionRequest{}, nil)
	if err != nil {
		t.Fatalf("expected reconnect to recover, got %v", err)
	}
	if stream == nil {
		t.Fatal("expected a live stream after reconnect")
	}
	if got := atomic.LoadInt32(&p.calls); got != 2 {
		t.Fatalf("expected 2 connect attempts (1 fail + 1 success), got %d", got)
	}
}

func TestStreamWithReconnectGivesUpAfterMax(t *testing.T) {
	// Shrink the backoff so exhausting all retries stays fast (real base would
	// sleep ~7.5s across 4 attempts).
	defer func(orig time.Duration) { streamReconnectBase = orig }(streamReconnectBase)
	streamReconnectBase = time.Millisecond
	// Always fails with a disconnect error → exhausts retries and returns it.
	p := &flakyProvider{failBefore: 99, failErr: errors.New("connection reset by peer")}
	_, err := streamWithReconnect(context.Background(), p, zeroruntime.CompletionRequest{}, nil)
	if err == nil {
		t.Fatal("expected an error after exhausting reconnects")
	}
	// 1 initial + maxStreamReconnects retries.
	if got := atomic.LoadInt32(&p.calls); got != int32(1+maxStreamReconnects) {
		t.Fatalf("expected %d attempts, got %d", 1+maxStreamReconnects, got)
	}
}

func TestStreamWithReconnectDoesNotRetryNonDisconnect(t *testing.T) {
	// A context-limit error is the compactor's job, not the reconnect's — return
	// immediately without retrying.
	p := &flakyProvider{failBefore: 99, failErr: errors.New("context length exceeded")}
	_, err := streamWithReconnect(context.Background(), p, zeroruntime.CompletionRequest{}, nil)
	if err == nil {
		t.Fatal("expected the original error")
	}
	if got := atomic.LoadInt32(&p.calls); got != 1 {
		t.Fatalf("context-limit error must not be retried, got %d attempts", got)
	}
}

func TestStreamWithReconnectStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &flakyProvider{failBefore: 99, failErr: errors.New("i/o timeout")}
	_, err := streamWithReconnect(ctx, p, zeroruntime.CompletionRequest{}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	// Cancelled ctx → no retry beyond the first attempt.
	if got := atomic.LoadInt32(&p.calls); got != 1 {
		t.Fatalf("cancelled context must not retry, got %d attempts", got)
	}
}

func TestShouldReconnectClassification(t *testing.T) {
	ctx := context.Background()
	disconnects := []string{
		"unexpected EOF", "connection reset by peer", "broken pipe",
		"i/o timeout", "server closed the connection", "connection refused",
		// Windows socket aborts also remain reconnectable at connect time.
		"wsarecv: An established connection was aborted by the software in your host machine",
		"read: connection was aborted",
		// Unix/macOS ECONNABORTED text.
		"read: software caused connection abort",
		"An existing connection was forcibly closed by the remote host",
	}
	for _, m := range disconnects {
		if !shouldReconnect(ctx, errors.New(m)) {
			t.Errorf("expected reconnect for %q", m)
		}
	}
	notDisconnects := []string{
		"context length exceeded", "invalid api key", "model not found",
		"400 bad request: unsupported parameter",
		// HTTP 5xx statuses are handled by providerio.SendWithRetry (503) or are
		// non-idempotent (500/502/504); the reconnect path must not double-retry
		// them. "504 Gateway Timeout" must NOT slip through on the generic "timeout"
		// transport signal — a gateway timeout is non-idempotent, since the upstream
		// may have already processed the completion POST before giving up.
		"503 Service Unavailable", "502 Bad Gateway",
		"504 Gateway Timeout", "500 Internal Server Error",
	}
	for _, m := range notDisconnects {
		if shouldReconnect(ctx, errors.New(m)) {
			t.Errorf("did NOT expect reconnect for %q", m)
		}
	}
}

func TestBackoffGrows(t *testing.T) {
	if backoffFor(1) != streamReconnectBase {
		t.Fatalf("attempt 1 backoff = %v, want %v", backoffFor(1), streamReconnectBase)
	}
	if backoffFor(2) != 2*streamReconnectBase {
		t.Fatalf("attempt 2 backoff = %v, want %v", backoffFor(2), 2*streamReconnectBase)
	}
	// The exponential base is capped so late attempts don't wait minutes.
	if got := backoffFor(20); got != streamReconnectMax {
		t.Fatalf("attempt 20 backoff = %v, want cap %v", got, streamReconnectMax)
	}
}

func TestJitteredBackoffStaysInBounds(t *testing.T) {
	// Jitter never drops below the deterministic base and never exceeds base*1.5,
	// so backoff still grows attempt over attempt while decorrelating retries.
	for attempt := 1; attempt <= 5; attempt++ {
		base := backoffFor(attempt)
		for i := 0; i < 200; i++ {
			got := jitteredBackoff(attempt)
			if got < base || got > base+base/2 {
				t.Fatalf("attempt %d jittered backoff %v out of [%v, %v]", attempt, got, base, base+base/2)
			}
		}
	}
}

func TestReconnectNoticeRoutesThroughReasoning(t *testing.T) {
	var got string
	notify := reconnectNoticeFor(Options{OnReasoning: func(s string) { got += s }})
	if notify == nil {
		t.Fatal("expected a notifier when OnReasoning is set")
	}
	notify(1, 2)
	if got == "" || !contains(got, "reconnecting 1/2") {
		t.Fatalf("notice = %q, want a reconnecting message", got)
	}
	if reconnectNoticeFor(Options{}) != nil {
		t.Fatal("expected nil notifier when OnReasoning is unset")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexFold(s, sub) >= 0)
}

func indexFold(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			a, b := s[i+j], sub[j]
			if a >= 'A' && a <= 'Z' {
				a += 'a' - 'A'
			}
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

var _ = time.Second
