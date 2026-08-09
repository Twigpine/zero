package sessions

import (
	"errors"
	"fmt"
	"testing"

	"github.com/Gitlawb/zero/internal/zeroruntime"
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
