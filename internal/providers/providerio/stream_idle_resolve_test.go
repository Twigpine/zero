package providerio

import (
	"testing"
	"time"
)

func TestResolveStreamIdleTimeout(t *testing.T) {
	const env = "ZERO_STREAM_IDLE_TIMEOUT"

	t.Run("explicit option wins over env", func(t *testing.T) {
		t.Setenv(env, "30s")
		if got := ResolveStreamIdleTimeout(50 * time.Millisecond); got != 50*time.Millisecond {
			t.Fatalf("got %v, want 50ms (explicit option must win)", got)
		}
	})

	t.Run("default when option and env are unset", func(t *testing.T) {
		t.Setenv(env, "")
		if got := ResolveStreamIdleTimeout(0); got != DefaultStreamIdleTimeout {
			t.Fatalf("got %v, want default %v", got, DefaultStreamIdleTimeout)
		}
	})

	t.Run("env Go duration", func(t *testing.T) {
		t.Setenv(env, "30s")
		if got := ResolveStreamIdleTimeout(0); got != 30*time.Second {
			t.Fatalf("got %v, want 30s", got)
		}
		t.Setenv(env, "4m")
		if got := ResolveStreamIdleTimeout(0); got != 4*time.Minute {
			t.Fatalf("got %v, want 4m", got)
		}
	})

	t.Run("env bare seconds", func(t *testing.T) {
		t.Setenv(env, "45")
		if got := ResolveStreamIdleTimeout(0); got != 45*time.Second {
			t.Fatalf("got %v, want 45s", got)
		}
	})

	t.Run("env disables the watchdog", func(t *testing.T) {
		for _, value := range []string{"0", "off", "none", "disabled", "OFF"} {
			t.Setenv(env, value)
			if got := ResolveStreamIdleTimeout(0); got != 0 {
				t.Fatalf("%q: got %v, want 0 (disabled)", value, got)
			}
		}
	})

	t.Run("bare seconds that overflow time.Duration keep the default", func(t *testing.T) {
		t.Setenv(env, "36028797018963968")
		if got := ResolveStreamIdleTimeout(0); got != DefaultStreamIdleTimeout {
			t.Fatalf("got %v, want default %v (overflow must not disable the watchdog)", got, DefaultStreamIdleTimeout)
		}
	})

	t.Run("invalid env falls back to default, not disabled", func(t *testing.T) {
		t.Setenv(env, "banana")
		if got := ResolveStreamIdleTimeout(0); got != DefaultStreamIdleTimeout {
			t.Fatalf("got %v, want default %v on a typo (must not silently disable)", got, DefaultStreamIdleTimeout)
		}
	})

	t.Run("default is generous enough for reasoning/cloud backends", func(t *testing.T) {
		if DefaultStreamIdleTimeout < 3*time.Minute {
			t.Fatalf("default %v is too aggressive; silent reasoning/cloud pauses can exceed it", DefaultStreamIdleTimeout)
		}
	})
}
