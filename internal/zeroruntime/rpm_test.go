package zeroruntime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type rpmTransport struct{ calls atomic.Int64 }

func (p *rpmTransport) StreamCompletion(context.Context, CompletionRequest) (<-chan StreamEvent, error) {
	p.calls.Add(1)
	ch := make(chan StreamEvent)
	close(ch)
	return ch, nil
}
func rpmSessionFor(t *testing.T, l *ModelRPMLimiter, m string, p Provider) TurnSession {
	t.Helper()
	s, e := l.Wrap(m, NewProviderTurnSessionProvider(p, ProviderCapabilities{Model: m})).OpenTurnSession(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func TestRPMRejectsNPlusOneBeforeTransportAcrossSessions(t *testing.T) {
	l := NewModelRPMLimiter(map[string]int{"m": 3})
	clock := time.Unix(0, 0)
	l.now = func() time.Time { return clock }
	p := &rpmTransport{}
	for i := 0; i < 3; i++ {
		if _, e := rpmSessionFor(t, l, "m", p).Stream(context.Background(), CompletionRequest{}); e != nil {
			t.Fatal(e)
		}
	}
	s := rpmSessionFor(t, l, "m", p)
	ch, e := s.Stream(context.Background(), CompletionRequest{})
	var hit *RPMLimitError
	if !errors.As(e, &hit) || ch != nil || hit.RetryAfter != time.Minute {
		t.Fatalf("N+1: stream=%v error=%v", ch, e)
	}
	if p.calls.Load() != 3 {
		t.Fatalf("transport calls=%d, want 3", p.calls.Load())
	}
	clock = clock.Add(time.Minute)
	if _, e = s.Stream(context.Background(), CompletionRequest{}); e != nil {
		t.Fatal(e)
	}
}
func TestRPMSlidingWindow(t *testing.T) {
	l := NewModelRPMLimiter(map[string]int{"m": 2})
	clock := time.Unix(59, 0)
	l.now = func() time.Time { return clock }
	s := rpmSessionFor(t, l, "m", &rpmTransport{})
	for i := 0; i < 2; i++ {
		if _, e := s.Stream(context.Background(), CompletionRequest{}); e != nil {
			t.Fatal(e)
		}
	}
	clock = time.Unix(60, 0)
	_, e := s.Stream(context.Background(), CompletionRequest{})
	var hit *RPMLimitError
	if !errors.As(e, &hit) || hit.RetryAfter != 59*time.Second {
		t.Fatalf("minute boundary reset window: %v", e)
	}
}
func TestRPMConcurrentAdmissions(t *testing.T) {
	l := NewModelRPMLimiter(map[string]int{"m": 7})
	l.now = func() time.Time { return time.Unix(0, 0) }
	p := &rpmTransport{}
	var grants, unexpected atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 100; i++ {
		s := rpmSessionFor(t, l, "m", p)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, e := s.Stream(context.Background(), CompletionRequest{})
			var hit *RPMLimitError
			if e == nil {
				grants.Add(1)
			} else if !errors.As(e, &hit) {
				unexpected.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if grants.Load() != 7 || p.calls.Load() != 7 || unexpected.Load() != 0 {
		t.Fatalf("grants=%d transport=%d unexpected=%d", grants.Load(), p.calls.Load(), unexpected.Load())
	}
}
func TestRPMCancellationFailedAttemptsAndIndependentModels(t *testing.T) {
	l := NewModelRPMLimiter(map[string]int{"a": 1, "b": 1})
	p := &recordingProvider{err: errors.New("fixture transport failure")}
	s := rpmSessionFor(t, l, "a", p)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := s.Stream(ctx, CompletionRequest{}); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if _, e := s.Stream(context.Background(), CompletionRequest{}); !errors.Is(e, p.err) {
		t.Fatal(e)
	}
	_, e := s.Stream(context.Background(), CompletionRequest{})
	var hit *RPMLimitError
	if !errors.As(e, &hit) || len(p.requests) != 1 {
		t.Fatalf("failed attempt refunded: %v", e)
	}
	_, _ = rpmSessionFor(t, l, "b", p).Stream(context.Background(), CompletionRequest{})
	for i := 0; i < 3; i++ {
		_, _ = rpmSessionFor(t, l, "unlimited", p).Stream(context.Background(), CompletionRequest{})
	}
	if len(p.requests) != 5 {
		t.Fatal("other model incorrectly blocked")
	}
}
func TestRPMDisabled(t *testing.T) {
	l := NewModelRPMLimiter(map[string]int{"m": 0})
	p := &providerTurnSessionProvider{provider: &rpmTransport{}}
	if l != nil || l.Wrap("m", p) != p {
		t.Fatal("disabled limit altered provider")
	}
}
