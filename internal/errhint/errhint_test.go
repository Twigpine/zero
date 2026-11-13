package errhint

import (
	"errors"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want Category
	}{
		{"nil-ish empty", "", Unknown},
		{"providerio auth prefix", "auth error: your API key is missing or invalid — run `zero auth`", Auth},
		{"raw 401", "provider request error: 401 Unauthorized", Auth},
		{"invalid api key", "provider request error: invalid_api_key: incorrect key provided", Auth},
		{"rate limit prefix", "rate limit error: 429 too many requests", RateLimit},
		{"local RPM", `rate limit error: local RPM limit for "gpt-4.1" (2 requests/60s); retry in 12s`, LocalRPM},
		{"overloaded", "provider error: model is overloaded, please retry", RateLimit},
		{"resource exhausted gemini", "provider stream error: rpc error: code = ResourceExhausted desc = quota exceeded", RateLimit},
		{"context length openai", "provider request error: this model's maximum context length is 128000 tokens", ContextOverflow},
		{"context_length_exceeded", "provider request error: context_length_exceeded", ContextOverflow},
		{"model not found", "provider request error: 404 the model `gpt-9` does not exist", ModelNotFound},
		{"unknown model", "provider request error: unknown model: sonnet-99", ModelNotFound},
		{"dns failure", "provider stream error: Post \"https://api...\": dial tcp: lookup api.foo.com: no such host", Connectivity},
		{"connection refused", "provider stream error: dial tcp 127.0.0.1:443: connection refused", Connectivity},
		{"deadline", "provider stream error: context deadline exceeded (Client.Timeout exceeded)", Connectivity},
		{"provider-marked but no sub-signature", "provider error: something totally unexpected happened", Unknown},

		// An incidental number that merely contains a status-code digit run must NOT
		// be mis-bucketed (digit-boundary check), even behind a provider marker.
		{"incidental latency number", "provider error: request completed in 4290ms then failed", Unknown},
		{"incidental request id", "provider error: request id 14015 failed unexpectedly", Unknown},
		{"standalone 401 still auth", "provider request error: got 401 from upstream", Auth},
		{"standalone 429 still rate limit", "provider error: 429 returned", RateLimit},

		// Local (non-provider) failures must NOT be classified — no provider marker,
		// so no bogus recovery hint attaches to them.
		{"local fs permission denied", "open /etc/shadow: permission denied", Unknown},
		{"local file missing", "stat foo.go: no such file or directory", Unknown},
		{"local model config typo", "unknown model: sonnet-99", Unknown},
		{"tool timeout local", "tool bash timed out after 600s", Unknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(errors.New(tc.msg)); got != tc.want {
				t.Fatalf("Classify(%q) = %v, want %v", tc.msg, got, tc.want)
			}
		})
	}
}

func TestClassifyNil(t *testing.T) {
	if got := Classify(nil); got != Unknown {
		t.Fatalf("Classify(nil) = %v, want Unknown", got)
	}
}

// Context-overflow must win over the connectivity "timeout" catch-all: a message
// mentioning both "context length" and a timeout is an overflow, not a network
// problem.
func TestContextOverflowBeatsConnectivity(t *testing.T) {
	err := errors.New("provider request error: maximum context length exceeded; request timeout")
	if got := Classify(err); got != ContextOverflow {
		t.Fatalf("Classify = %v, want ContextOverflow", got)
	}
}

func TestHintsPresentForKnownCategories(t *testing.T) {
	known := []error{
		errors.New("auth error: bad key"),
		errors.New("rate limit error: 429"),
		errors.New("provider stream error: dial tcp: no such host"),
		errors.New("provider request error: model does not exist"),
		errors.New("provider request error: maximum context length is 128000 tokens"),
	}
	for _, err := range known {
		if h := TUIHint(err); strings.TrimSpace(h) == "" {
			t.Fatalf("TUIHint(%q) empty, want a hint", err)
		}
		if h := CLIHint(err); strings.TrimSpace(h) == "" {
			t.Fatalf("CLIHint(%q) empty, want a hint", err)
		}
	}
	// Unknown yields no hint on either surface.
	unknown := errors.New("provider error: mystery")
	if h := TUIHint(unknown); h != "" {
		t.Fatalf("TUIHint(unknown) = %q, want empty", h)
	}
	if h := CLIHint(unknown); h != "" {
		t.Fatalf("CLIHint(unknown) = %q, want empty", h)
	}
}
