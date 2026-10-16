package zeroruntime

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ModelRPMLimiter shares a sliding 60-second window across turn sessions in
// one runtime process. Separate processes do not share state.
type ModelRPMLimiter struct {
	mu      sync.Mutex
	limits  map[string]int
	windows map[string][]time.Time
	now     func() time.Time
}

// NewModelRPMLimiter copies validated canonical-model limits. Zero/unset is off.
func NewModelRPMLimiter(limits map[string]int) *ModelRPMLimiter {
	active := make(map[string]int)
	for m, n := range limits {
		if n > 0 {
			active[m] = n
		}
	}
	if len(active) == 0 {
		return nil
	}
	return &ModelRPMLimiter{limits: active, windows: make(map[string][]time.Time), now: time.Now}
}

// RPMLimitError denotes rejection before the wrapped Stream method is called.
type RPMLimitError struct {
	Model      string
	Limit      int
	RetryAfter time.Duration
}

func (e *RPMLimitError) Error() string {
	retry := e.RetryAfter.Round(time.Second)
	if retry < e.RetryAfter {
		retry += time.Second
	}
	return fmt.Sprintf("rate limit error: local RPM limit for %q (%d requests/60s); retry in %s", e.Model, e.Limit, retry)
}

// Wrap gates Stream admissions, not setup/prewarm, compaction or transport retries.
func (l *ModelRPMLimiter) Wrap(model string, p TurnSessionProvider) TurnSessionProvider {
	if l == nil || l.limits[model] == 0 || p == nil {
		return p
	}
	return rpmProvider{TurnSessionProvider: p, limiter: l, model: model}
}
func (l *ModelRPMLimiter) admit(ctx context.Context, model string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	now := l.now()
	w := l.windows[model]
	first := 0
	for first < len(w) && !w[first].After(now.Add(-time.Minute)) {
		first++
	}
	w = w[:copy(w, w[first:])]
	l.windows[model] = w
	if len(w) >= l.limits[model] {
		return &RPMLimitError{Model: model, Limit: l.limits[model], RetryAfter: w[0].Add(time.Minute).Sub(now)}
	}
	l.windows[model] = append(w, now)
	return nil
}

type rpmProvider struct {
	TurnSessionProvider
	limiter *ModelRPMLimiter
	model   string
}

func (p rpmProvider) OpenTurnSession(ctx context.Context) (TurnSession, error) {
	s, err := p.TurnSessionProvider.OpenTurnSession(ctx)
	if err != nil {
		return nil, err
	}
	return rpmSession{TurnSession: s, limiter: p.limiter, model: p.model}, nil
}

type rpmSession struct {
	TurnSession
	limiter *ModelRPMLimiter
	model   string
}

func (s rpmSession) Stream(ctx context.Context, r CompletionRequest) (<-chan StreamEvent, error) {
	if err := s.limiter.admit(ctx, s.model); err != nil {
		return nil, err
	}
	return s.TurnSession.Stream(ctx, r)
}
