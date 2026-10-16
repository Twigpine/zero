package providerio

import (
	"testing"
	"time"
)

func TestResolveResponseHeaderTimeout(t *testing.T) {
	const env = "ZERO_RESPONSE_HEADER_TIMEOUT"

	t.Run("default when env is unset or empty", func(t *testing.T) {
		t.Setenv(env, "")
		if got := ResolveResponseHeaderTimeout(); got != DefaultResponseHeaderTimeout {
			t.Fatalf("got %v, want default %v", got, DefaultResponseHeaderTimeout)
		}
	})

	t.Run("default keeps the value that was previously hardcoded", func(t *testing.T) {
		if DefaultResponseHeaderTimeout != 120*time.Second {
			t.Fatalf("default is %v; this override must not change the 120s default", DefaultResponseHeaderTimeout)
		}
	})

	t.Run("env Go duration", func(t *testing.T) {
		t.Setenv(env, "240s")
		if got := ResolveResponseHeaderTimeout(); got != 240*time.Second {
			t.Fatalf("got %v, want 240s", got)
		}
		t.Setenv(env, "5m")
		if got := ResolveResponseHeaderTimeout(); got != 5*time.Minute {
			t.Fatalf("got %v, want 5m", got)
		}
	})

	t.Run("env bare seconds", func(t *testing.T) {
		t.Setenv(env, "300")
		if got := ResolveResponseHeaderTimeout(); got != 300*time.Second {
			t.Fatalf("got %v, want 300s", got)
		}
	})

	t.Run("env value is trimmed", func(t *testing.T) {
		t.Setenv(env, "  90s ")
		if got := ResolveResponseHeaderTimeout(); got != 90*time.Second {
			t.Fatalf("got %v, want 90s", got)
		}
	})

	t.Run("env removes the limit", func(t *testing.T) {
		for _, value := range []string{"0", "off", "none", "disabled", "OFF", "Disabled"} {
			t.Setenv(env, value)
			if got := ResolveResponseHeaderTimeout(); got != 0 {
				t.Fatalf("%q: got %v, want 0 (no limit)", value, got)
			}
		}
	})

	t.Run("bare seconds that overflow time.Duration keep the default", func(t *testing.T) {
		t.Setenv(env, "36028797018963968")
		if got := ResolveResponseHeaderTimeout(); got != DefaultResponseHeaderTimeout {
			t.Fatalf("got %v, want default %v (overflow must not become a zero timeout)", got, DefaultResponseHeaderTimeout)
		}
	})

	t.Run("invalid env falls back to default, not unlimited", func(t *testing.T) {
		for _, value := range []string{"banana", "-5s", "-1", "1.5x"} {
			t.Setenv(env, value)
			if got := ResolveResponseHeaderTimeout(); got != DefaultResponseHeaderTimeout {
				t.Fatalf("%q: got %v, want default %v (a typo must not remove the limit)", value, got, DefaultResponseHeaderTimeout)
			}
		}
	})
}
