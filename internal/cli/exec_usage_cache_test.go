package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/agent"
)

// THE CHILD MUST PUBLISH WHAT MAKES ITS TURN PRICEABLE.
//
// This writer is the only channel a parent has. Its session record already keeps
// cachedInputTokens / cacheWriteTokens / reasoningTokens so a turn can be costed
// exactly; the stream carried prompt/completion/total alone, so every sub-agent
// turn rolled up to its parent was priced as if nothing had been cached.
//
// A measured plan task had 49,280 of 49,894 prompt tokens served from cache —
// 98.8%. Plan tasks are the ideal cache case: one large stable prompt, re-sent
// every turn.
func TestTheChildPublishesCacheAndReasoningTokensOnTheUsageStream(t *testing.T) {
	var stdout, stderr bytes.Buffer
	writer := execEventWriter{
		stdout:       &stdout,
		stderr:       &stderr,
		format:       execOutputStreamJSON,
		runID:        "run_usage",
		streamedText: &strings.Builder{},
	}
	writer.usage(agent.Usage{
		PromptTokens:      30000,
		CompletionTokens:  500,
		CachedInputTokens: 29000,
		CacheWriteTokens:  1000,
		ReasoningTokens:   120,
	})
	if writer.err != nil {
		t.Fatalf("usage: %v", writer.err)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &payload); err != nil {
		t.Fatalf("decode stream event %q: %v", stdout.String(), err)
	}
	for key, want := range map[string]float64{
		"cachedInputTokens": 29000, "cacheWriteTokens": 1000, "reasoningTokens": 120,
	} {
		got, ok := payload[key]
		if !ok {
			t.Errorf("the stream omits %q, so a parent prices this turn as uncached: %v", key, payload)
			continue
		}
		if number, ok := got.(float64); !ok || number != want {
			t.Errorf("%s: want %v, got %v", key, want, got)
		}
	}
	// The three original fields must be exactly as before.
	for key, want := range map[string]float64{
		"promptTokens": 30000, "completionTokens": 500, "totalTokens": 30500,
	} {
		if got, ok := payload[key].(float64); !ok || got != want {
			t.Errorf("%s changed: want %v, got %v", key, want, payload[key])
		}
	}
}

// NON-ZERO ONLY, exactly like usage.EventUsagePayload. A provider reporting no
// cache must produce the same three-field event it always did, or every existing
// reader of this stream sees a shape it has never seen.
func TestAProviderWithNoCacheStillEmitsTheOriginalUsageShape(t *testing.T) {
	var stdout, stderr bytes.Buffer
	writer := execEventWriter{
		stdout: &stdout, stderr: &stderr, format: execOutputStreamJSON,
		runID: "run_usage", streamedText: &strings.Builder{},
	}
	writer.usage(agent.Usage{PromptTokens: 100, CompletionTokens: 10})

	var payload map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"cachedInputTokens", "cacheWriteTokens", "reasoningTokens"} {
		if _, present := payload[key]; present {
			t.Errorf("%q was emitted as a zero, widening the event for every provider that reports no cache", key)
		}
	}
}
