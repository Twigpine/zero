package tui

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/usage"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// USAGE FOLLOWS THE SWITCH. exec reassigns its currentModel when the escalation
// switcher fires; the TUI captured usageModelID once per run and handed the
// switcher no callback, so every usage event after an escalation was billed to
// the model the run started on. Both attribution paths are pinned here: the
// live per-event message, and the per-event record the final response carries
// for the batch fallback, which must not swing the other way and bill the
// events before the switch to the escalated model.
func TestEscalatedRunAttributesUsageToTheModelInForce(t *testing.T) {
	store := testSessionStore(t)
	session, err := store.Create(sessions.CreateInput{SessionID: "escalation_usage"})
	if err != nil {
		t.Fatal(err)
	}

	// A catalog model with an upgrade target; the target is whatever the
	// catalog says, read back from the provider factory rather than assumed.
	const startModel = "claude-haiku-4.5"
	starting := &scriptedProvider{scripts: [][]zeroruntime.StreamEvent{{
		{Type: zeroruntime.StreamEventUsage, Usage: zeroruntime.Usage{InputTokens: 10, OutputTokens: 1}},
		{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "escalate", ToolName: "escalate_model"},
		{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "escalate", ArgumentsFragment: `{"reason":"harder than it looked"}`},
		{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "escalate"},
		{Type: zeroruntime.StreamEventDone},
	}}}
	escalated := &scriptedProvider{scripts: [][]zeroruntime.StreamEvent{{
		{Type: zeroruntime.StreamEventText, Content: "Done on the stronger model."},
		{Type: zeroruntime.StreamEventUsage, Usage: zeroruntime.Usage{InputTokens: 20, OutputTokens: 2}},
		{Type: zeroruntime.StreamEventDone},
	}}}
	registry := tools.NewRegistry()
	registry.Register(tools.NewEscalateModelTool())

	switchedTo := ""
	m := newModel(context.Background(), Options{
		ProviderName:    "anthropic",
		ModelName:       startModel,
		Provider:        starting,
		Registry:        registry,
		SessionStore:    store,
		AllowEscalation: true,
		ProviderProfile: config.ProviderProfile{Model: startModel},
		NewProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			switchedTo = profile.Model
			return escalated, nil
		},
	})
	m.activeSession = session
	m.agentOptions.Model = startModel
	var live []string
	m.runtimeMessageSink = func(msg tea.Msg) {
		if usage, ok := msg.(agentUsageMsg); ok {
			live = append(live, usage.modelID)
		}
	}

	msg := execCmd(m.runAgentWithOptions(1, context.Background(), "do the hard thing", nil, tuiAgentRunOptions{}))
	response, ok := msg.(agentResponseMsg)
	if !ok {
		t.Fatalf("run returned %T, want agentResponseMsg", msg)
	}
	if response.err != nil {
		t.Fatalf("run failed: %v", response.err)
	}
	if switchedTo == "" || switchedTo == startModel {
		t.Fatalf("SETUP INVALID: the escalation never switched providers (switched to %q), so nothing here exercises attribution", switchedTo)
	}
	if len(escalated.requests) == 0 {
		t.Fatal("SETUP INVALID: the escalated provider was never asked for a completion")
	}
	if len(response.usageEvents) != 2 {
		t.Fatalf("usage events = %d, want one before the switch and one after", len(response.usageEvents))
	}

	want := []string{startModel, switchedTo}
	if !reflect.DeepEqual(live, want) {
		t.Fatalf("live usage attribution = %v, want %v", live, want)
	}
	if !reflect.DeepEqual(response.usageModelIDs, want) {
		t.Fatalf("per-event usage attribution on the response = %v, want %v", response.usageModelIDs, want)
	}
	for index := range response.usageEvents {
		if got := response.usageModelIDAt(index); got != want[index] {
			t.Fatalf("usageModelIDAt(%d) = %q, want %q", index, got, want[index])
		}
	}
}

// A response built without the per-event record, which is every constructor
// that predates escalation, still attributes through the run-level model.
func TestUsageModelIDAtFallsBackToTheRunModel(t *testing.T) {
	msg := agentResponseMsg{usageModelID: "gpt-4.1", usageEvents: make([]zeroruntime.Usage, 2)}
	for index := range msg.usageEvents {
		if got := msg.usageModelIDAt(index); got != "gpt-4.1" {
			t.Fatalf("usageModelIDAt(%d) = %q, want the run-level model", index, got)
		}
	}
	msg.usageModelIDs = []string{"gpt-4.1-mini"}
	if got := msg.usageModelIDAt(0); got != "gpt-4.1-mini" {
		t.Fatalf("usageModelIDAt(0) = %q, want the per-event model", got)
	}
	if got := msg.usageModelIDAt(1); got != "gpt-4.1" {
		t.Fatalf("usageModelIDAt(1) = %q, want the run-level fallback past the record", got)
	}
}

// AND THE PRICE THAT COMES BACK OUT OF THE SESSION LOG. The live record and the
// persisted event are two representations of one usage event, and only the
// first carried the model. `zero usage report` rebuilds cost from the persisted
// payload, falling back to the session's own model when the event does not name
// one, so an escalated run was priced end to end at the model it started on.
// This drives the real persistence path and reconstructs the report from it.
func TestEscalatedRunPricesPersistedUsageAtEachModel(t *testing.T) {
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		t.Fatal(err)
	}
	store := testSessionStore(t)
	session, err := store.Create(sessions.CreateInput{SessionID: "escalation_cost"})
	if err != nil {
		t.Fatal(err)
	}

	const startModel = "claude-haiku-4.5"
	before := zeroruntime.Usage{InputTokens: 10, OutputTokens: 1}
	after := zeroruntime.Usage{InputTokens: 20, OutputTokens: 2}
	starting := &scriptedProvider{scripts: [][]zeroruntime.StreamEvent{{
		{Type: zeroruntime.StreamEventUsage, Usage: before},
		{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "escalate", ToolName: "escalate_model"},
		{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "escalate", ArgumentsFragment: `{"reason":"harder than it looked"}`},
		{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "escalate"},
		{Type: zeroruntime.StreamEventDone},
	}}}
	escalated := &scriptedProvider{scripts: [][]zeroruntime.StreamEvent{{
		{Type: zeroruntime.StreamEventText, Content: "Done on the stronger model."},
		{Type: zeroruntime.StreamEventUsage, Usage: after},
		{Type: zeroruntime.StreamEventDone},
	}}}
	toolRegistry := tools.NewRegistry()
	toolRegistry.Register(tools.NewEscalateModelTool())

	switchedTo := ""
	m := newModel(context.Background(), Options{
		ProviderName:    "anthropic",
		ModelName:       startModel,
		Provider:        starting,
		Registry:        toolRegistry,
		SessionStore:    store,
		AllowEscalation: true,
		ProviderProfile: config.ProviderProfile{Model: startModel},
		NewProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			switchedTo = profile.Model
			return escalated, nil
		},
	})
	m.activeSession = session
	m.agentOptions.Model = startModel

	msg := execCmd(m.runAgentWithOptions(1, context.Background(), "do the hard thing", nil, tuiAgentRunOptions{}))
	response, ok := msg.(agentResponseMsg)
	if !ok || response.err != nil {
		t.Fatalf("run returned %T err=%v", msg, response.err)
	}
	if switchedTo == "" || switchedTo == startModel {
		t.Fatalf("SETUP INVALID: no escalation happened (switched to %q)", switchedTo)
	}

	// Persist through the same path a real run uses, then read the events back.
	m, rows := m.appendSessionEvents(response.sessionEvents)
	for _, row := range rows {
		if row.kind == rowError {
			t.Fatalf("session record error: %s", row.text)
		}
	}
	events, err := store.ReadEvents(session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	persisted := []string{}
	for _, event := range events {
		if event.Type != sessions.EventUsage {
			continue
		}
		var payload struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		persisted = append(persisted, payload.Model)
	}
	if want := []string{startModel, switchedTo}; !reflect.DeepEqual(persisted, want) {
		t.Fatalf("persisted usage models = %v, want %v", persisted, want)
	}

	// The report prices from those payloads. Its session metadata names only the
	// starting model, which is the fallback that used to price both events.
	metadata := []sessions.Metadata{{SessionID: session.SessionID, ModelID: startModel}}
	report, err := usage.BuildReport(events, metadata, &registry, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Expected cost comes from the catalog, one completion at each model.
	expected := 0.0
	for _, priced := range []struct {
		modelID string
		usage   zeroruntime.Usage
	}{{startModel, before}, {switchedTo, after}} {
		model, err := registry.Require(priced.modelID)
		if err != nil {
			t.Fatal(err)
		}
		cost, err := modelregistry.CalculateCost(model, priced.usage)
		if err != nil {
			t.Fatal(err)
		}
		expected += cost.TotalCost
	}
	// And the wrong answer, for contrast: both events at the starting model.
	startingOnly := 0.0
	startingEntry, err := registry.Require(startModel)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []zeroruntime.Usage{before, after} {
		cost, err := modelregistry.CalculateCost(startingEntry, event)
		if err != nil {
			t.Fatal(err)
		}
		startingOnly += cost.TotalCost
	}
	if expected == startingOnly {
		t.Fatal("SETUP INVALID: the two models price this usage identically, so the assertion below proves nothing")
	}
	if difference := report.Total.TotalCost - expected; difference > 1e-12 || difference < -1e-12 {
		t.Fatalf("report cost = %v, want %v (pricing both events at %s gives %v)", report.Total.TotalCost, expected, startModel, startingOnly)
	}
}

// A run without escalation persists no model on its usage events: the model
// cannot change mid-run, so the session-wide identity is the whole story and
// the payload stays compact, exactly as exec does it.
func TestUnescalatedRunLeavesTheUsagePayloadAlone(t *testing.T) {
	store := testSessionStore(t)
	session, err := store.Create(sessions.CreateInput{SessionID: "no_escalation_usage"})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{scripts: [][]zeroruntime.StreamEvent{{
		{Type: zeroruntime.StreamEventText, Content: "done"},
		{Type: zeroruntime.StreamEventUsage, Usage: zeroruntime.Usage{InputTokens: 10, OutputTokens: 1}},
		{Type: zeroruntime.StreamEventDone},
	}}}
	m := newModel(context.Background(), Options{
		ProviderName: "anthropic",
		ModelName:    "claude-haiku-4.5",
		Provider:     provider,
		Registry:     tools.NewRegistry(),
		SessionStore: store,
	})
	m.activeSession = session

	msg := execCmd(m.runAgentWithOptions(1, context.Background(), "hello", nil, tuiAgentRunOptions{}))
	response, ok := msg.(agentResponseMsg)
	if !ok || response.err != nil {
		t.Fatalf("run returned %T err=%v", msg, response.err)
	}
	usageEvents := 0
	for _, event := range response.sessionEvents {
		if event.Type != sessions.EventUsage {
			continue
		}
		usageEvents++
		payload, ok := event.Payload.(map[string]any)
		if !ok {
			t.Fatalf("usage payload = %T, want map", event.Payload)
		}
		if _, present := payload["model"]; present {
			t.Errorf("a non-escalation run recorded a model on its usage payload: %v", payload)
		}
	}
	if usageEvents == 0 {
		t.Fatal("SETUP INVALID: the run recorded no usage event")
	}
}
