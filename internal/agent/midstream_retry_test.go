package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/trace"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// midStreamAbortProvider connects successfully but emits a transport-abort
// StreamEventError on the first abortBefore calls, then succeeds with "done".
// hangOnCall, if > 0, makes that 1-based call block until ctx is done so a
// cancel during the retried CollectStream can be reproduced.
// connectFailFrom, if > 0, makes that 1-based call and later return a
// reconnectable connect error so streamWithReconnect enters its backoff.
type midStreamAbortProvider struct {
	calls           int32
	abortBefore     int32
	abortError      string
	partialText     string
	emptyTextEvent  bool
	partialToolCall string
	hangOnCall      int32
	connectFailFrom int32
	connectErr      string
	started         chan struct{}
}

func (p *midStreamAbortProvider) StreamCompletion(ctx context.Context, _ zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	n := atomic.AddInt32(&p.calls, 1)
	if p.connectFailFrom > 0 && n >= p.connectFailFrom {
		if p.started != nil {
			select {
			case <-p.started:
			default:
				close(p.started)
			}
		}
		errMsg := p.connectErr
		if errMsg == "" {
			errMsg = "connection reset by peer"
		}
		return nil, errors.New(errMsg)
	}
	if p.hangOnCall > 0 && n == p.hangOnCall {
		if p.started != nil {
			close(p.started)
		}
		ch := make(chan zeroruntime.StreamEvent)
		go func() {
			<-ctx.Done()
			close(ch)
		}()
		return ch, nil
	}
	ch := make(chan zeroruntime.StreamEvent, 5)
	if n <= p.abortBefore {
		if p.emptyTextEvent {
			ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventText, Content: ""}
		}
		if p.partialText != "" {
			ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventText, Content: p.partialText}
		}
		if p.partialToolCall != "" {
			ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "tc_1", ToolName: p.partialToolCall}
			ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "tc_1", ArgumentsFragment: `{"path":"x.html","content":"<!doctype`}
		}
		errMsg := p.abortError
		if errMsg == "" {
			errMsg = "provider stream error: read: connection reset by peer"
		}
		ch <- zeroruntime.StreamEvent{
			Type:  zeroruntime.StreamEventError,
			Error: errMsg,
		}
		close(ch)
		return ch, nil
	}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventText, Content: "done"}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}
	close(ch)
	return ch, nil
}

func TestRunRetriesMidStreamConnectionReset(t *testing.T) {
	p := &midStreamAbortProvider{abortBefore: 1}
	result, err := Run(context.Background(), "go", p, Options{Registry: tools.NewRegistry()})
	if err != nil {
		t.Fatalf("a mid-stream connection reset should retry to success, got %v", err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("final answer = %q, want %q", result.FinalAnswer, "done")
	}
	if got := atomic.LoadInt32(&p.calls); got != 2 {
		t.Fatalf("want 2 calls (1 abort + 1 retry), got %d", got)
	}
}

func TestRunRetriesMidStreamWindowsAbort(t *testing.T) {
	p := &midStreamAbortProvider{
		abortBefore: 1,
		abortError:  "provider stream error: read: wsarecv: An established connection was aborted by the software in your host machine",
	}
	result, err := Run(context.Background(), "go", p, Options{Registry: tools.NewRegistry()})
	if err != nil {
		t.Fatalf("a Windows mid-stream abort should retry to success, got %v", err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("final answer = %q, want %q", result.FinalAnswer, "done")
	}
	if got := atomic.LoadInt32(&p.calls); got != 2 {
		t.Fatalf("want 2 calls (1 abort + 1 retry), got %d", got)
	}
}

// wsarecv alone (no "connection was aborted" substring) must still retry, so
// deleting the wsarecv needle cannot go green.
func TestRunRetriesMidStreamWsarecvNeedle(t *testing.T) {
	p := &midStreamAbortProvider{
		abortBefore: 1,
		abortError:  "provider stream error: read: wsarecv: 10053",
	}
	result, err := Run(context.Background(), "go", p, Options{Registry: tools.NewRegistry()})
	if err != nil {
		t.Fatalf("wsarecv without 'connection was aborted' should retry to success, got %v", err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("final answer = %q, want %q", result.FinalAnswer, "done")
	}
	if got := atomic.LoadInt32(&p.calls); got != 2 {
		t.Fatalf("want 2 calls (1 abort + 1 retry), got %d", got)
	}
}

func TestRunDoesNotRetryMidStreamAbortAfterPartialOutput(t *testing.T) {
	sawText := 0
	p := &midStreamAbortProvider{abortBefore: 1, partialText: "partial"}
	_, err := Run(context.Background(), "go", p, Options{
		Registry: tools.NewRegistry(),
		OnText:   func(string) { sawText++ },
	})
	if err == nil {
		t.Fatal("a mid-stream abort after partial output must NOT be retried; want an error")
	}
	if got := atomic.LoadInt32(&p.calls); got != 1 {
		t.Fatalf("partial-then-abort must not retry, got %d calls", got)
	}
	if sawText == 0 {
		t.Fatal("OnText must have fired so forwardedVisibleText is the named guard")
	}
}

// A zero-length OnText chunk is not committed answer prose, so an eligible
// transport abort still retries.
func TestRunRetriesMidStreamAbortAfterEmptyTextEvent(t *testing.T) {
	p := &midStreamAbortProvider{abortBefore: 1, emptyTextEvent: true}
	result, err := Run(context.Background(), "go", p, Options{
		Registry: tools.NewRegistry(),
		OnText:   func(string) {},
	})
	if err != nil {
		t.Fatalf("empty OnText must not block mid-stream abort retry, got %v", err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("final answer = %q, want %q", result.FinalAnswer, "done")
	}
	if got := atomic.LoadInt32(&p.calls); got != 2 {
		t.Fatalf("want 2 calls (1 abort + 1 retry), got %d", got)
	}
}

func TestRunRetriesMidStreamAbortAfterIncompleteToolCall(t *testing.T) {
	starts := 0
	dispatched := 0
	p := &midStreamAbortProvider{abortBefore: 1, partialToolCall: "write_file"}
	result, err := Run(context.Background(), "go", p, Options{
		Registry:        tools.NewRegistry(),
		OnToolCallStart: func(string, string) { starts++ },
		OnToolCallDelta: func(string, string) {},
		OnToolCall:      func(ToolCall) { dispatched++ },
	})
	if err != nil {
		t.Fatalf("a mid-stream abort mid-incomplete-tool-call should retry to success, got %v", err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("final answer = %q, want %q", result.FinalAnswer, "done")
	}
	if got := atomic.LoadInt32(&p.calls); got != 2 {
		t.Fatalf("want 2 calls (1 abort + 1 retry), got %d", got)
	}
	if starts != 1 {
		t.Fatalf("the incomplete tool call should have been forwarded once before the abort, got %d starts", starts)
	}
	if dispatched != 0 {
		t.Fatalf("OnToolCall must never fire for an incomplete aborted tool call (no dispatch), got %d", dispatched)
	}
}

func TestRunGivesUpAfterMaxMidStreamAbortRetries(t *testing.T) {
	if maxStreamStallRetries != 1 {
		t.Fatalf("maxStreamStallRetries = %d, want 1 (#973 must not raise this bound)", maxStreamStallRetries)
	}
	defer func(orig time.Duration) { streamReconnectBase = orig }(streamReconnectBase)
	streamReconnectBase = time.Millisecond
	p := &midStreamAbortProvider{abortBefore: 99}
	_, err := Run(context.Background(), "go", p, Options{Registry: tools.NewRegistry()})
	if err == nil {
		t.Fatal("a persistent mid-stream abort must surface an error after exhausting retries")
	}
	if got := atomic.LoadInt32(&p.calls); got != 2 {
		t.Fatalf("want 2 calls (1 abort + 1 retry); pin the bound at 1, got %d", got)
	}
}

func TestRunMidStreamAbortUsesReconnectNotice(t *testing.T) {
	defer func(orig time.Duration) { streamReconnectBase = orig }(streamReconnectBase)
	streamReconnectBase = time.Millisecond
	var notices string
	p := &midStreamAbortProvider{abortBefore: 1}
	result, err := Run(context.Background(), "go", p, Options{
		Registry:    tools.NewRegistry(),
		OnReasoning: func(s string) { notices += s },
	})
	if err != nil {
		t.Fatalf("retry should succeed, got %v", err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("final answer = %q, want %q", result.FinalAnswer, "done")
	}
	lower := strings.ToLower(notices)
	if !strings.Contains(lower, "connection lost") || !strings.Contains(lower, "reconnecting") {
		t.Fatalf("transport abort must use reconnect wording, notices = %q", notices)
	}
	if strings.Contains(lower, "model stalled") {
		t.Fatalf("transport abort must not use stall wording, notices = %q", notices)
	}
}

func TestRunCancelDuringMidStreamRetryPreservesContextCanceled(t *testing.T) {
	defer func(orig time.Duration) { streamReconnectBase = orig }(streamReconnectBase)
	streamReconnectBase = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	p := &midStreamAbortProvider{abortBefore: 1, hangOnCall: 2, started: started}
	errCh := make(chan error, 1)
	go func() {
		_, err := Run(ctx, "go", p, Options{Registry: tools.NewRegistry()})
		errCh <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for retried stream")
	}
	cancel()
	err := <-errCh
	if err == nil {
		t.Fatal("want context.Canceled after cancel during retried stream")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel during retried stream must keep the sentinel, got %q (errors.Is Canceled = false)", err)
	}
}

func TestRunCancelDuringReplacementConnectBackoffPreservesContextCanceled(t *testing.T) {
	defer func(orig time.Duration) { streamReconnectBase = orig }(streamReconnectBase)
	streamReconnectBase = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	p := &midStreamAbortProvider{abortBefore: 1, connectFailFrom: 2, started: started}
	errCh := make(chan error, 1)
	go func() {
		_, err := Run(ctx, "go", p, Options{Registry: tools.NewRegistry()})
		errCh <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for replacement connect failure")
	}
	cancel()
	err := <-errCh
	if err == nil {
		t.Fatal("want context.Canceled after cancel during replacement connect backoff")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel during replacement connect backoff must keep the sentinel, got %q (errors.Is Canceled = false)", err)
	}
}

func TestRunMidStreamAbortRecordsReconnectCountOnImmediateReplacement(t *testing.T) {
	defer func(orig time.Duration) { streamReconnectBase = orig }(streamReconnectBase)
	streamReconnectBase = time.Millisecond
	rec := trace.NewRecorder("midstream-abort", "run-1", "test")
	p := &midStreamAbortProvider{abortBefore: 1}
	result, err := Run(context.Background(), "go", p, Options{
		Registry: tools.NewRegistry(),
		Trace:    rec,
	})
	if err != nil {
		t.Fatalf("retry should succeed, got %v", err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("final answer = %q, want %q", result.FinalAnswer, "done")
	}
	if got := atomic.LoadInt32(&p.calls); got != 2 {
		t.Fatalf("want 2 calls (1 abort + 1 immediate replacement), got %d", got)
	}
	gotTrace := rec.Finish()
	if got := gotTrace.Counter(trace.CounterReconnectCount); got != 1 {
		t.Fatalf("reconnect_count = %d, want 1 for a successful replacement connection", got)
	}
	if got := gotTrace.Counter(trace.CounterModelRequests); got != 2 {
		t.Fatalf("model_requests = %d, want 2 (initial + replacement)", got)
	}
}

func TestRunDoesNotRetryResponseHeaderTimeout(t *testing.T) {
	p := &midStreamAbortProvider{
		abortBefore: 99,
		abortError:  "net/http: timeout awaiting response headers",
	}
	_, err := Run(context.Background(), "go", p, Options{Registry: tools.NewRegistry()})
	if err == nil {
		t.Fatal("a response-header timeout must not be retried as a mid-stream abort")
	}
	if got := atomic.LoadInt32(&p.calls); got != 1 {
		t.Fatalf("header timeout must not retry, got %d calls", got)
	}
}

func TestIsMidStreamTransportAbort(t *testing.T) {
	aborts := []string{
		"provider stream error: read: connection reset by peer",
		"provider stream error: unexpected EOF",
		"provider stream error: read: wsarecv: 10053",
		"provider stream error: read: wsarecv: An established connection was aborted by the software in your host machine",
		"An existing connection was forcibly closed by the remote host",
		"provider stream error: read: connection closed",
		"write: broken pipe",
		"provider stream error: server closed the connection",
		"provider stream error: software caused connection abort",
	}
	for _, m := range aborts {
		if !isMidStreamTransportAbort(m) {
			t.Fatalf("want mid-stream transport abort: %q", m)
		}
	}
	notAborts := []string{
		"",
		"context length exceeded",
		"rate limit error: slow down",
		"model not found",
		"net/http: timeout awaiting response headers",
		"dial tcp 10.0.0.1:443: connect: connection refused",
		"i/o timeout",
		"500 Internal Server Error",
		"502 Bad Gateway",
		"503 Service Unavailable",
		"504 Gateway Timeout",
		"provider request error: request does not satisfy oneOf schema",
		"provider request error: connection closed is not a supported finish reason",
		"provider error: connection closed is not a supported finish reason",
		"provider error: upstream connection reset by peer",
		"provider error: unexpected EOF while reading upstream response",
		"provider error: 502 Bad Gateway",
		"auth error: connection reset by peer",
		"rate limit error: server closed",
		"schema error: property oneOf is invalid",
	}
	for _, m := range notAborts {
		if isMidStreamTransportAbort(m) {
			t.Fatalf("must NOT classify as mid-stream transport abort: %q", m)
		}
	}
}

func TestRunDoesNotRetryClassifiedProviderErrorWithSocketPhrase(t *testing.T) {
	p := &midStreamAbortProvider{
		abortBefore: 1,
		abortError:  "provider request error: connection closed is not a supported finish reason",
	}
	var notices string
	opts := Options{
		Registry:    tools.NewRegistry(),
		OnReasoning: func(s string) { notices += s },
	}
	_, err := Run(context.Background(), "go", p, opts)
	if err == nil {
		t.Fatal("expected application error, got nil")
	}
	if got := atomic.LoadInt32(&p.calls); got != 1 {
		t.Fatalf("classified provider error with 'connection closed' must not retry (got %d calls, want 1)", got)
	}
	if notices != "" {
		t.Fatalf("expected no reconnect notices, got %q", notices)
	}
	if !strings.Contains(err.Error(), "connection closed") {
		t.Fatalf("expected original error preserved, got %v", err)
	}
}

func TestRunDoesNotRetryProviderErrorWithSocketPhrase(t *testing.T) {
	p := &midStreamAbortProvider{
		abortBefore: 1,
		abortError:  "provider error: unexpected EOF while reading upstream response",
	}
	var notices string
	opts := Options{
		Registry:    tools.NewRegistry(),
		OnReasoning: func(s string) { notices += s },
	}
	_, err := Run(context.Background(), "go", p, opts)
	if err == nil {
		t.Fatal("expected application error, got nil")
	}
	if got := atomic.LoadInt32(&p.calls); got != 1 {
		t.Fatalf("provider error with 'unexpected EOF' must not retry (got %d calls, want 1)", got)
	}
	if notices != "" {
		t.Fatalf("expected no reconnect notices, got %q", notices)
	}
	if got := err.Error(); got != p.abortError {
		t.Fatalf("error = %q, want original provider error %q", got, p.abortError)
	}
}

func TestRunDoesNotRetryApplicationErrorContainingEOFSubstring(t *testing.T) {
	p := &midStreamAbortProvider{
		abortBefore: 1,
		abortError:  "provider request error: request does not satisfy oneOf schema",
	}
	var notices string
	opts := Options{
		Registry:    tools.NewRegistry(),
		OnReasoning: func(s string) { notices += s },
	}
	_, err := Run(context.Background(), "go", p, opts)
	if err == nil {
		t.Fatal("expected application error, got nil")
	}
	if got := atomic.LoadInt32(&p.calls); got != 1 {
		t.Fatalf("application error with 'oneOf' must not retry (got %d calls, want 1)", got)
	}
	if notices != "" {
		t.Fatalf("expected no reconnect notices, got %q", notices)
	}
	if !strings.Contains(err.Error(), "oneOf") {
		t.Fatalf("expected original error preserved, got %v", err)
	}
}

func TestRunRetriesMidStreamUnexpectedEOF(t *testing.T) {
	defer func(orig time.Duration) { streamReconnectBase = orig }(streamReconnectBase)
	streamReconnectBase = time.Millisecond
	p := &midStreamAbortProvider{
		abortBefore: 1,
		abortError:  "provider stream error: unexpected EOF",
	}
	var notices string
	opts := Options{
		Registry:    tools.NewRegistry(),
		OnReasoning: func(s string) { notices += s },
	}
	result, err := Run(context.Background(), "go", p, opts)
	if err != nil {
		t.Fatalf("unexpected EOF should retry to success, got %v", err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("final answer = %q, want %q", result.FinalAnswer, "done")
	}
	if got := atomic.LoadInt32(&p.calls); got != 2 {
		t.Fatalf("want 2 calls (1 abort + 1 retry), got %d", got)
	}
	lower := strings.ToLower(notices)
	if !strings.Contains(lower, "connection lost") || !strings.Contains(lower, "reconnecting") {
		t.Fatalf("expected reconnect notice, got %q", notices)
	}
}
