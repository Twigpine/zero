package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/providers/openai"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// TestRunExecOptimizedSessionUnderGate proves the end-to-end wiring: with
// ZERO_OPENAI_TURN_SESSION on and a real official-OpenAI provider, a headless
// run streams through the optimized session — the server sees the prewarm HEAD
// probe in addition to the turn's POST.
func TestRunExecOptimizedSessionUnderGate(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("ZERO_OPENAI_TURN_SESSION", "1")
	cwd := t.TempDir()

	var heads, posts atomic.Int64
	// Closed when the prewarm probe lands, so the turn can be held open until it
	// does. See the POST handler for why that ordering has to be forced.
	headSeen := make(chan struct{})
	var headOnce sync.Once
	var prewarmWaitTimedOut atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead:
			heads.Add(1)
			headOnce.Do(func() { close(headSeen) })
			w.WriteHeader(http.StatusMethodNotAllowed)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chat/completions"):
			// Hold the turn open until the prewarm probe arrives. The probe is
			// issued on the run's context, and runExec cancels that context the
			// moment it returns, because the stop function signalContext hands
			// back is a CancelFunc. A probe that has not reached the wire by the
			// end of the run therefore never reaches it at all. That is why the
			// poll this replaced could not work at any deadline: once the run is
			// over the probe is cancelled, not merely slow.
			//
			// This widens the window, it does not close it. Once the goroutine is
			// scheduled it still has only prewarmTimeout to dial loopback and
			// write the request, and that timeout is armed inside the goroutine
			// itself, so holding the turn here cannot extend it. If this test
			// ever fails again, prewarmTimeout is where to look, not this hold.
			//
			// No self-deadlock: the pinned transport leaves MaxConnsPerHost at 0,
			// so the HEAD dials its own connection instead of queueing behind
			// this held POST. Do not set a per-host connection limit there.
			//
			// Bounded, so a probe that genuinely never fires fails the assertion
			// below with a useful message instead of hanging the test.
			select {
			case <-headSeen:
			case <-time.After(10 * time.Second):
				// Record it. A probe that shows up after this gave up would still
				// leave heads at 1, so the count alone proves the probe was
				// eventually received, not that it was ordered before the turn,
				// which is the whole claim of this test.
				prewarmWaitTimedOut.Store(true)
			}
			posts.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"exec", "say done"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return cwd, nil },
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{
				ActiveProvider: "official-openai",
				Provider: config.ProviderProfile{
					Name:         "official-openai",
					ProviderKind: config.ProviderKindOpenAI,
					BaseURL:      server.URL,
					APIKey:       "sk-exec-test",
					Model:        "pr8-exec-model",
				},
			}, nil
		},
		newProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			// Pin a keep-alive transport so the probe fires on every platform:
			// the shared default transport disables keep-alives on macOS, where
			// the session correctly skips the prewarm.
			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.DisableKeepAlives = false
			return openai.New(openai.Options{
				APIKey:     profile.APIKey,
				BaseURL:    profile.BaseURL,
				Model:      profile.Model,
				HTTPClient: &http.Client{Transport: transport},
			})
		},
	})
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stdout=%s stderr=%s", exitCode, stdout.String(), stderr.String())
	}
	if got := posts.Load(); got < 1 {
		t.Fatalf("server saw %d chat-completions POSTs, want >= 1", got)
	}
	// Checked before the count, because the count cannot tell the two apart: a
	// probe that arrived only after the guard expired still leaves heads at 1.
	if prewarmWaitTimedOut.Load() {
		t.Fatal("the turn was not held until the prewarm probe arrived; the probe did not precede it")
	}
	// No polling: the turn above could not complete until the probe landed, so
	// the count is settled by this point.
	if got := heads.Load(); got != 1 {
		t.Fatalf("server saw %d prewarm HEAD probes, want exactly 1", got)
	}
}

// TestRunExecGateOnFallsBackForFakeProvider proves fallback safety: with the
// gate on but a provider that is not the concrete *openai.Provider, the run
// proceeds on the default path exactly as today.
func TestRunExecGateOnFallsBackForFakeProvider(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("ZERO_OPENAI_TURN_SESSION", "1")
	cwd := t.TempDir()

	var builds int
	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"exec", "--model", "claude-haiku-4.5", "hello"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return cwd, nil },
		resolveConfig: func(_ string, overrides config.Overrides) (config.ResolvedConfig, error) {
			model := "claude-haiku-4.5"
			if overrides.Provider.Model != "" {
				model = overrides.Provider.Model
			}
			cfg := execResolvedConfig()
			cfg.Provider.ProviderKind = config.ProviderKindAnthropic
			cfg.Provider.Model = model
			cfg.MaxTurns = 3
			return cfg, nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			builds++
			return &escalatingExecProvider{}, nil
		},
	})
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stdout=%s stderr=%s", exitCode, stdout.String(), stderr.String())
	}
	if builds != 1 {
		t.Fatalf("newProvider called %d times, want 1 (default path, no optimized session)", builds)
	}
}
