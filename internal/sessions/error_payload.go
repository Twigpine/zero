package sessions

import (
	"errors"

	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// ErrorEventPayload builds the payload for an EventError session event. It
// always includes the flattened message (unchanged, for backward
// compatibility with existing consumers of events.jsonl), and additionally
// persists the HTTP status code and upstream cause when err is (or wraps) a
// *zeroruntime.StreamError — the structured error a failed provider stream
// returns. Without this, a provider error left no way to diagnose *why* a
// call failed from the CLI or its logs — only the generic top-level message
// was ever recorded (#674). Cause has already been redacted for secrets by
// the provider before reaching here (see zeroruntime.StreamEvent.Cause) — it
// is never re-scrubbed or stored raw at this layer.
func ErrorEventPayload(err error) map[string]any {
	payload := map[string]any{"message": err.Error()}
	var streamErr *zeroruntime.StreamError
	if errors.As(err, &streamErr) {
		if streamErr.StatusCode != 0 {
			payload["statusCode"] = streamErr.StatusCode
		}
		if streamErr.Cause != "" {
			payload["cause"] = streamErr.Cause
		}
	}
	return payload
}
