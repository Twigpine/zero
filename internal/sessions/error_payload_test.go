package sessions

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Twigpine/zero/internal/zeroruntime"
)

func TestErrorEventPayloadPlainError(t *testing.T) {
	payload := ErrorEventPayload(errors.New("provider error: provider returned error"))

	if got, want := payload["message"], "provider error: provider returned error"; got != want {
		t.Fatalf("message = %v, want %v", got, want)
	}
	if _, ok := payload["statusCode"]; ok {
		t.Fatalf("statusCode should be absent for a plain error, got %v", payload["statusCode"])
	}
	if _, ok := payload["cause"]; ok {
		t.Fatalf("cause should be absent for a plain error, got %v", payload["cause"])
	}
}

func TestErrorEventPayloadStreamError(t *testing.T) {
	err := &zeroruntime.StreamError{
		Message:    "provider error: provider returned error",
		StatusCode: 500,
		Cause:      "upstream returned an empty completion",
	}

	payload := ErrorEventPayload(err)

	if got, want := payload["message"], "provider error: provider returned error"; got != want {
		t.Fatalf("message = %v, want %v", got, want)
	}
	if got, want := payload["statusCode"], 500; got != want {
		t.Fatalf("statusCode = %v, want %v", got, want)
	}
	if got, want := payload["cause"], "upstream returned an empty completion"; got != want {
		t.Fatalf("cause = %v, want %v", got, want)
	}
}

func TestErrorEventPayloadWrappedStreamError(t *testing.T) {
	// A StreamError wrapped by another layer (e.g. fmt.Errorf("...: %w", err))
	// must still surface its status/cause via errors.As, not just a direct type
	// assertion — this is the shape #674's real repro chain produces.
	inner := &zeroruntime.StreamError{Message: "rate limit error: too many requests", StatusCode: 429, Cause: "retry after 30s"}
	wrapped := fmt.Errorf("turn failed: %w", inner)

	payload := ErrorEventPayload(wrapped)

	if got, want := payload["statusCode"], 429; got != want {
		t.Fatalf("statusCode = %v, want %v", got, want)
	}
	if got, want := payload["cause"], "retry after 30s"; got != want {
		t.Fatalf("cause = %v, want %v", got, want)
	}
	if got, want := payload["message"], "turn failed: rate limit error: too many requests"; got != want {
		t.Fatalf("message = %v, want %v", got, want)
	}
}

func TestErrorEventPayloadZeroStatusCodeOmitted(t *testing.T) {
	// A transport-level failure (e.g. context cancellation) never got an HTTP
	// response, so StatusCode is legitimately 0 — that must stay omitted rather
	// than be persisted as a misleading "statusCode": 0.
	err := &zeroruntime.StreamError{Message: "context deadline exceeded"}

	payload := ErrorEventPayload(err)

	if _, ok := payload["statusCode"]; ok {
		t.Fatalf("statusCode should be omitted when zero, got %v", payload["statusCode"])
	}
	if _, ok := payload["cause"]; ok {
		t.Fatalf("cause should be omitted when empty, got %v", payload["cause"])
	}
}

func TestStoreAppendEventPersistsErrorEventStatusCodeAndCause(t *testing.T) {
	// Closes the seam between ErrorEventPayload (unit tested above) and Run
	// returning a *StreamError (tested in internal/agent): this asserts the
	// two actually meet at the Store, i.e. a *StreamError survives a real
	// AppendEvent/ReadEvents round trip with statusCode/cause intact.
	store := NewStore(StoreOptions{RootDir: t.TempDir(), Now: fixedClock("2026-08-10T12:00:00Z")})
	session, err := store.Create(CreateInput{SessionID: "error_join"})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	streamErr := &zeroruntime.StreamError{
		Message:    "provider error: provider returned error",
		StatusCode: 503,
		Cause:      "upstream returned an empty completion",
	}

	if _, err := store.AppendEvent(session.SessionID, AppendEventInput{
		Type:    EventError,
		Payload: ErrorEventPayload(streamErr),
	}); err != nil {
		t.Fatalf("AppendEvent returned error: %v", err)
	}

	events, err := store.ReadEvents(session.SessionID)
	if err != nil {
		t.Fatalf("ReadEvents returned error: %v", err)
	}
	if len(events) != 1 || events[0].Type != EventError {
		t.Fatalf("expected a single error event, got %#v", events)
	}

	var payload map[string]any
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatalf("failed to unmarshal persisted payload: %v", err)
	}
	if got, want := payload["message"], "provider error: provider returned error"; got != want {
		t.Fatalf("persisted message = %v, want %v", got, want)
	}
	if got, want := payload["statusCode"], float64(503); got != want {
		t.Fatalf("persisted statusCode = %v, want %v", got, want)
	}
	if got, want := payload["cause"], "upstream returned an empty completion"; got != want {
		t.Fatalf("persisted cause = %v, want %v", got, want)
	}
}

func TestErrorEventPayloadBoundsCause(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause string
	}{
		// "é" is two bytes; an odd cap boundary must not split it.
		{"two-byte runes", strings.Repeat("é", maxErrorCauseBytes)},
		{"one byte over the cap", strings.Repeat("a", maxErrorCauseBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := ErrorEventPayload(&zeroruntime.StreamError{Message: "m", StatusCode: 500, Cause: tc.cause})

			got, ok := payload["cause"].(string)
			if !ok {
				t.Fatalf("cause = %#v, want string", payload["cause"])
			}
			if !strings.HasSuffix(got, errorCauseTruncationMarker) {
				t.Fatalf("cause missing truncation marker: ...%q", got[len(got)-20:])
			}
			// The marker counts: the persisted cause as a whole stays within the cap.
			if len(got) > maxErrorCauseBytes {
				t.Fatalf("bounded cause is %d bytes with its marker, want <= %d", len(got), maxErrorCauseBytes)
			}
			if !utf8.ValidString(got) {
				t.Fatal("bounded cause is not valid UTF-8")
			}
		})
	}
}

func TestErrorEventPayloadKeepsCauseAtCap(t *testing.T) {
	cause := strings.Repeat("a", maxErrorCauseBytes)
	payload := ErrorEventPayload(&zeroruntime.StreamError{Message: "m", Cause: cause})

	if got := payload["cause"]; got != cause {
		t.Fatalf("cause at exactly the cap was altered (len %d)", len(got.(string)))
	}
}

// The bound holds for the cause as it reads back from events.jsonl, not only
// for the string handed to AppendEvent. JSON turns each invalid byte into a
// three-byte U+FFFD, so an error body of invalid bytes cut to the limit came
// back at up to three times it.
func TestAnInvalidUTF8CauseStaysWithinTheBoundAfterTheStoreRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause string
	}{
		{"invalid bytes between ASCII", strings.Repeat("\xffa", maxErrorCauseBytes)},
		{"a run of invalid bytes", strings.Repeat("\xfe", maxErrorCauseBytes) + "tail"},
		{"short and invalid", "status \xff\xfe body"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := NewStore(StoreOptions{RootDir: t.TempDir(), Now: fixedClock("2026-11-09T12:00:00Z")})
			session, err := store.Create(CreateInput{SessionID: "invalid_cause"})
			if err != nil {
				t.Fatalf("Create returned error: %v", err)
			}
			streamErr := &zeroruntime.StreamError{Message: "provider error", StatusCode: 502, Cause: tc.cause}
			if _, err := store.AppendEvent(session.SessionID, AppendEventInput{Type: EventError, Payload: ErrorEventPayload(streamErr)}); err != nil {
				t.Fatalf("AppendEvent returned error: %v", err)
			}
			events, err := store.ReadEvents(session.SessionID)
			if err != nil || len(events) != 1 {
				t.Fatalf("ReadEvents = %d events, %v", len(events), err)
			}
			var payload map[string]any
			if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
				t.Fatalf("unmarshal persisted payload: %v", err)
			}
			got, ok := payload["cause"].(string)
			if !ok {
				t.Fatalf("persisted cause = %#v, want a string", payload["cause"])
			}
			if len(got) > maxErrorCauseBytes {
				t.Fatalf("persisted cause reads back at %d bytes, want <= %d", len(got), maxErrorCauseBytes)
			}
			if !utf8.ValidString(got) {
				t.Fatal("persisted cause is not valid UTF-8")
			}
		})
	}
}
