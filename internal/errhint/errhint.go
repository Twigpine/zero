// Package errhint classifies provider/model failures into a few user-actionable
// categories and turns them into a one-line "next step" hint.
//
// Provider errors already arrive with a classified string prefix from
// providerio.ClassifiedError ("auth error:", "rate limit error:", …); lower-level
// failures (DNS, TLS, timeouts, context-length) arrive as raw driver or library
// messages. Classify matches both so the interactive error row (TUI) and the
// `zero exec` provider-error path (CLI) can append one concrete next step instead
// of dumping an identical red blob for every failure mode.
package errhint

import "strings"

// Category buckets a provider/model failure into a small set of classes that each
// map to a distinct recovery action.
type Category int

const (
	// Unknown means the error didn't match any known signature; callers should
	// emit no hint rather than guess.
	Unknown Category = iota
	Auth
	RateLimit
	Connectivity
	ModelNotFound
	ContextOverflow
	LocalRPM
)

// providerMarkers are the prefixes the provider layer attaches to every
// provider-originated failure: providerio.ClassifiedError emits "auth error:",
// "rate limit error:", "provider error:", and "provider request error:", and the
// streaming paths wrap transport/decoding failures as "provider stream error:".
// A UI surface's error can also be a *local* failure (a tool's "permission
// denied", a "file does not exist", a config error), so Classify only proceeds
// past this gate for messages that are recognizably from the provider — otherwise
// a broad substring like "does not exist" would attach a bogus /model hint to an
// unrelated local error.
var providerMarkers = []string{
	"auth error:",
	"rate limit error:",
	"provider error:",
	"provider request error:",
	"provider stream error:",
}

// Classify buckets err by scanning its message for known signatures. It is a
// deliberately conservative string heuristic — the provider layer has already
// discarded the numeric HTTP status by the time the error reaches a UI surface,
// so the message is all we have. It first gates on a provider-origin marker (see
// providerMarkers) so local failures never draw a provider hint, then sub-classifies.
// Order matters: more specific signatures are tested before broader ones (e.g.
// "context length" as overflow before the generic "timeout" as connectivity).
func Classify(err error) Category {
	if err == nil {
		return Unknown
	}
	m := strings.ToLower(err.Error())
	if !containsAny(m, providerMarkers...) {
		return Unknown
	}
	switch {
	case strings.Contains(m, "rate limit error: local rpm limit for"):
		return LocalRPM
	case containsAny(m, "auth error:", "unauthorized", "api key", "api_key", "invalid_api_key",
		"authentication", "permission denied", "forbidden") || containsStatusCode(m, "401", "403"):
		return Auth
	case containsAny(m, "rate limit", "rate_limit", "too many requests", "quota",
		"resource_exhausted", "overloaded") || containsStatusCode(m, "429", "529"):
		return RateLimit
	case containsAny(m, "context length", "context window", "maximum context", "context_length_exceeded",
		"too many tokens", "prompt is too long", "reduce the length", "maximum context length"):
		return ContextOverflow
	case containsAny(m, "model not found", "model_not_found", "does not exist", "unknown model",
		"no such model", "unsupported model", "invalid model", "model is not"):
		return ModelNotFound
	case containsAny(m, "dial tcp", "no such host", "connection refused", "network is unreachable",
		"i/o timeout", "context deadline exceeded", "tls handshake", "connection reset",
		"unexpected eof", "lookup ", "timeout"):
		return Connectivity
	default:
		return Unknown
	}
}

// TUIHint returns a one-line hint referencing interactive slash commands, or ""
// when the category is Unknown. Meant to sit under the raw error in the live
// error row.
func TUIHint(err error) string {
	switch Classify(err) {
	case Auth:
		return "API key rejected — run /provider to re-check your credentials"
	case RateLimit:
		return "Rate limited — wait a moment, or switch model with /model"
	case LocalRPM:
		return "Local modelRPM cap reached — wait for the window to expire, or switch model with /model"
	case Connectivity:
		return "Can't reach the provider — run /doctor --connectivity"
	case ModelNotFound:
		return "Model unavailable — pick another with /model"
	case ContextOverflow:
		return "Context window full — run /compact to free space"
	default:
		return ""
	}
}

// CLIHint returns a one-line hint referencing `zero …` subcommands, or "" when the
// category is Unknown. Meant for the non-interactive `zero exec` error path, where
// slash commands don't apply.
func CLIHint(err error) string {
	switch Classify(err) {
	case Auth:
		return "API key rejected — run `zero setup`, `zero auth openrouter` for OpenRouter, or set the provider's API key"
	case RateLimit:
		return "Rate limited — wait a moment, or switch model with --model"
	case LocalRPM:
		return "Local modelRPM cap reached — wait for the window to expire, or switch model with --model"
	case Connectivity:
		return "Can't reach the provider — run `zero doctor`"
	case ModelNotFound:
		return "Model unavailable — run `zero doctor` or pick another with --model"
	case ContextOverflow:
		return "Context window full — shorten the prompt or start a fresh session"
	default:
		return ""
	}
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// containsStatusCode reports whether haystack contains any of the given HTTP
// status codes as a standalone number — not embedded in a longer digit run like
// "completed in 4290ms" or "request id 14015" — so an incidental number can't be
// mis-bucketed as an auth/rate-limit failure.
func containsStatusCode(haystack string, codes ...string) bool {
	return HasStatusCode(haystack, codes...)
}

// HasStatusCode reports whether haystack contains any of the given HTTP status
// codes as a standalone number — not embedded in a longer digit run like
// "completed in 4290ms" or "request id 14015". Exported so other packages (e.g.
// the reconnect classifier) can gate on a status code without re-implementing
// the digit-boundary check.
func HasStatusCode(haystack string, codes ...string) bool {
	for _, code := range codes {
		for from := 0; ; {
			rel := strings.Index(haystack[from:], code)
			if rel < 0 {
				break
			}
			pos := from + rel
			beforeOK := pos == 0 || !isASCIIDigit(haystack[pos-1])
			end := pos + len(code)
			afterOK := end >= len(haystack) || !isASCIIDigit(haystack[end])
			if beforeOK && afterOK {
				return true
			}
			from = pos + 1
		}
	}
	return false
}

func isASCIIDigit(b byte) bool {
	return b >= '0' && b <= '9'
}
