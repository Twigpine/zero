package providers

import (
	"context"
	"errors"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/zeroruntime"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

type rpmRoundTripper struct{ calls atomic.Int64 }

func (r *rpmRoundTripper) RoundTrip(q *http.Request) (*http.Response, error) {
	r.calls.Add(1)
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n")), Request: q}, nil
}
func TestConfiguredRPMBlocksNPlusOneBeforeHTTP(t *testing.T) {
	for _, opt := range []string{"0", "1"} {
		t.Run("optimized="+opt, func(t *testing.T) {
			t.Setenv(openaiTurnSessionEnv, opt)
			r := &rpmRoundTripper{}
			o := Options{HTTPClient: &http.Client{Transport: r}, ModelRPM: zeroruntime.NewModelRPMLimiter(map[string]int{"gpt-4.1": 2})}
			profile := config.ProviderProfile{Name: "test", ProviderKind: config.ProviderKindOpenAI, Model: "openai:gpt-4.1", APIKey: "fixture-only"}
			p, e := New(profile, o)
			if e != nil {
				t.Fatal(e)
			}
			for i := 0; i < 3; i++ {
				if i == 2 {
					profile.Model = "gpt-4.1"
				}
				ss, ok := ConfiguredTurnSessions(profile, p, o)
				if !ok {
					t.Fatal("missing wrapper")
				}
				s, e := ss.OpenTurnSession(context.Background())
				if e != nil {
					t.Fatal(e)
				}
				ch, e := s.Stream(context.Background(), zeroruntime.CompletionRequest{Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "fixture"}}})
				if i == 2 {
					var hit *zeroruntime.RPMLimitError
					if !errors.As(e, &hit) || ch != nil {
						t.Fatalf("N+1 was not denied: %v", e)
					}
				} else {
					if e != nil {
						t.Fatal(e)
					}
					for range ch {
					}
				}
				if e := s.Close(); e != nil {
					t.Fatal(e)
				}
			}
			if r.calls.Load() != 2 {
				t.Fatalf("transport=%d want 2", r.calls.Load())
			}
		})
	}
}
func TestEscalationSharesRPMWhenStartingUnoptimized(t *testing.T) {
	t.Setenv(openaiTurnSessionEnv, "0")
	o := Options{ModelRPM: zeroruntime.NewModelRPMLimiter(map[string]int{"gpt-4.1": 1})}
	profile := config.ProviderProfile{Name: "test", ProviderKind: config.ProviderKindOpenAI, Model: "gpt-4.1"}
	p := escalationStubProvider{}
	ss, _ := ConfiguredTurnSessions(profile, p, o)
	s, e := ss.OpenTurnSession(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Stream(context.Background(), zeroruntime.CompletionRequest{}); e != nil {
		t.Fatal(e)
	}
	_, switcher := EscalationSwitchers(profile, p, func(config.ProviderProfile) (zeroruntime.Provider, error) { return p, nil }, nil, o)
	if switcher == nil {
		t.Fatal("missing shared session switcher")
	}
	ss, e = switcher(context.Background(), "openai:gpt-4.1")
	if e != nil {
		t.Fatal(e)
	}
	s, e = ss.OpenTurnSession(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.Stream(context.Background(), zeroruntime.CompletionRequest{})
	var hit *zeroruntime.RPMLimitError
	if !errors.As(e, &hit) {
		t.Fatalf("switch reset cap: %v", e)
	}
}
