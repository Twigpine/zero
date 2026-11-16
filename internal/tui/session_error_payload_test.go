package tui

import (
	"context"
	"encoding/json"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// A provider error reaches the TUI session log with its HTTP status and the
// upstream cause, not only the flattened message (#674).
func TestPromptSubmitRecordsTheProviderErrorStatusAndCause(t *testing.T) {
	store := testSessionStore(t)
	provider := &fakeProvider{events: []zeroruntime.StreamEvent{{
		Type:       zeroruntime.StreamEventError,
		Error:      "provider request error: model not found",
		StatusCode: 404,
		Cause:      "upstream said: no such model",
	}}}
	m := newModel(context.Background(), Options{
		Cwd:          isolatedCwd(t),
		ProviderName: "openai",
		ModelName:    "gpt-4.1",
		Provider:     provider,
		Registry:     tools.NewRegistry(),
		SessionStore: store,
	})
	m.input.SetValue("inspect repo")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd == nil {
		t.Fatal("expected prompt submit to start an agent run")
	}
	updated, _ = next.Update(execCmd(cmd))
	_ = updated.(model)

	list, err := store.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("sessions = %d, %v; want one", len(list), err)
	}
	events, err := store.ReadEvents(list[0].SessionID)
	if err != nil {
		t.Fatalf("ReadEvents returned error: %v", err)
	}
	if got := countSessionEvents(events, sessions.EventError); got != 1 {
		t.Fatalf("recorded %d error events, want 1: %#v", got, eventTypes(events))
	}
	var payload map[string]any
	if err := json.Unmarshal(nthSessionEvent(t, events, sessions.EventError, 1).Payload, &payload); err != nil {
		t.Fatalf("unmarshal error payload: %v", err)
	}
	if got, want := payload["statusCode"], float64(404); got != want {
		t.Errorf("statusCode = %#v, want %v: the run recorded only the flattened message", got, want)
	}
	if got, want := payload["cause"], "upstream said: no such model"; got != want {
		t.Errorf("cause = %#v, want %q: the run recorded only the flattened message", got, want)
	}
}
