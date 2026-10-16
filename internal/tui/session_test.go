package tui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/sandbox"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

type scriptedProvider struct {
	scripts    [][]zeroruntime.StreamEvent
	requests   []zeroruntime.CompletionRequest
	beforeCall func(int)
	calls      int
}

func (provider *scriptedProvider) StreamCompletion(
	ctx context.Context,
	request zeroruntime.CompletionRequest,
) (<-chan zeroruntime.StreamEvent, error) {
	provider.requests = append(provider.requests, request)
	if len(provider.scripts) == 0 {
		ch := make(chan zeroruntime.StreamEvent)
		close(ch)
		return ch, nil
	}
	index := provider.calls
	provider.calls++
	if index >= len(provider.scripts) {
		index = len(provider.scripts) - 1
	}
	if provider.beforeCall != nil {
		provider.beforeCall(index)
	}
	ch := make(chan zeroruntime.StreamEvent, len(provider.scripts[index]))
	for _, event := range provider.scripts[index] {
		ch <- event
	}
	close(ch)
	return ch, nil
}

func TestPromptSubmitPersistsTUISessionEvents(t *testing.T) {
	store := testSessionStore(t)
	provider := &fakeProvider{events: []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventText, Content: "saved"},
		{Type: zeroruntime.StreamEventUsage, Usage: zeroruntime.Usage{InputTokens: 10, OutputTokens: 4}},
		{Type: zeroruntime.StreamEventDone},
	}}
	m := newModel(context.Background(), Options{
		Cwd:          "repo",
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
	next = updated.(model)

	list, err := store.List()
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected one persisted TUI session, got %d", len(list))
	}
	session := list[0]
	if session.Title != "inspect repo" || session.Cwd != "repo" || session.ModelID != "gpt-4.1" || session.Provider != "openai" {
		t.Fatalf("unexpected session metadata: %#v", session)
	}

	events, err := store.ReadEvents(session.SessionID)
	if err != nil {
		t.Fatalf("ReadEvents returned error: %v", err)
	}
	if got := eventTypes(events); !equalEventTypes(got, []sessions.EventType{
		sessions.EventMessage,
		sessions.EventUsage,
		sessions.EventMessage,
	}) {
		t.Fatalf("unexpected event sequence: %#v", got)
	}
	assertPayloadField(t, events[0], "role", "user")
	assertPayloadField(t, events[0], "content", "inspect repo")
	assertPayloadField(t, events[1], "promptTokens", float64(10))
	assertPayloadField(t, events[1], "completionTokens", float64(4))
	assertPayloadField(t, events[1], "totalTokens", float64(14))
	assertPayloadField(t, events[2], "role", "assistant")
	assertPayloadField(t, events[2], "content", "saved")
	if !transcriptContains(next.transcript, "saved") {
		t.Fatalf("expected persisted run to still render assistant text, got %#v", next.transcript)
	}
}

func TestAppendSessionEventsBatchesAndUpdatesActiveSession(t *testing.T) {
	store := testSessionStore(t)
	session, err := store.Create(sessions.CreateInput{SessionID: "tui_batch", Title: "batch"})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	m := model{
		sessionStore:  store,
		activeSession: session,
	}

	next, rows := m.appendSessionEvents([]pendingSessionEvent{
		{Type: sessions.EventMessage, Payload: map[string]any{"role": "assistant", "content": "done"}},
		{Type: sessions.EventUsage, Payload: map[string]any{"totalTokens": 3}},
	})
	if len(rows) != 0 {
		t.Fatalf("appendSessionEvents returned error rows: %#v", rows)
	}
	if next.activeSession.EventCount != 2 || next.activeSession.LastEventType != sessions.EventUsage {
		t.Fatalf("active session metadata not updated from batch: %#v", next.activeSession)
	}
	if len(next.sessionEvents) != 2 || next.sessionEvents[0].Sequence != 1 || next.sessionEvents[1].Sequence != 2 {
		t.Fatalf("in-memory session events not updated from batch: %#v", next.sessionEvents)
	}
	events, err := store.ReadEvents(session.SessionID)
	if err != nil {
		t.Fatalf("ReadEvents returned error: %v", err)
	}
	if got := eventTypes(events); !equalEventTypes(got, []sessions.EventType{sessions.EventMessage, sessions.EventUsage}) {
		t.Fatalf("unexpected persisted event types: %#v", got)
	}
	loaded, err := store.Get(session.SessionID)
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if loaded == nil || loaded.EventCount != next.activeSession.EventCount || loaded.LastEventType != next.activeSession.LastEventType {
		t.Fatalf("store metadata diverged from active session: store=%#v active=%#v", loaded, next.activeSession)
	}
}

func TestPromptWithoutProviderDoesNotCreateSession(t *testing.T) {
	store := testSessionStore(t)
	m := newModel(context.Background(), Options{SessionStore: store})
	m.input.SetValue("hello")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd != nil {
		t.Fatal("expected missing provider prompt to stay synchronous")
	}
	if !transcriptContains(next.transcript, "No provider configured.") {
		t.Fatalf("expected missing provider message, got %#v", next.transcript)
	}
	list, err := store.List()
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected missing provider prompt not to create sessions, got %#v", list)
	}
}

func TestPromptSubmitPersistsToolSessionEvents(t *testing.T) {
	store := testSessionStore(t)
	root := t.TempDir()
	writeTestFile(t, root, "notes.txt", "file contents")
	provider := &scriptedProvider{scripts: [][]zeroruntime.StreamEvent{
		{
			{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call_1", ToolName: "read_file"},
			{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call_1", ArgumentsFragment: `{"path":"notes.txt"}`},
			{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call_1"},
			{Type: zeroruntime.StreamEventDone},
		},
		{
			{Type: zeroruntime.StreamEventText, Content: "read complete"},
			{Type: zeroruntime.StreamEventDone},
		},
	}}
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(root, nil))
	m := newModel(context.Background(), Options{
		Cwd:          root,
		ProviderName: "openai",
		ModelName:    "gpt-4.1",
		Provider:     provider,
		Registry:     registry,
		SessionStore: store,
	})
	m.input.SetValue("read notes")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd == nil {
		t.Fatal("expected prompt submit to start an agent run")
	}
	_, _ = next.Update(execCmd(cmd))

	list, err := store.List()
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected one session, got %d", len(list))
	}
	events, err := store.ReadEvents(list[0].SessionID)
	if err != nil {
		t.Fatalf("ReadEvents returned error: %v", err)
	}
	if got := eventTypes(events); !equalEventTypes(got, []sessions.EventType{
		sessions.EventMessage,
		sessions.EventToolCall,
		sessions.EventToolResult,
		sessions.EventMessage,
	}) {
		t.Fatalf("unexpected event sequence: %#v", got)
	}
	assertPayloadField(t, events[1], "id", "call_1")
	assertPayloadField(t, events[1], "name", "read_file")
	assertPayloadField(t, events[1], "arguments", `{"path":"notes.txt"}`)
	assertPayloadField(t, events[2], "toolCallId", "call_1")
	assertPayloadField(t, events[2], "name", "read_file")
	assertPayloadField(t, events[2], "status", "ok")
	assertPayloadFieldContains(t, events[2], "output", "file contents")
	assertPayloadField(t, events[3], "content", "read complete")
}

func TestPromptSubmitPersistsPermissionSessionEvents(t *testing.T) {
	store := testSessionStore(t)
	root := t.TempDir()
	provider := &scriptedProvider{scripts: [][]zeroruntime.StreamEvent{
		{
			{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call_write", ToolName: "write_file"},
			{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call_write", ArgumentsFragment: `{"path":"notes.txt","content":"hello"}`},
			{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call_write"},
			{Type: zeroruntime.StreamEventDone},
		},
		{
			{Type: zeroruntime.StreamEventText, Content: "write blocked"},
			{Type: zeroruntime.StreamEventDone},
		},
	}}
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedWriteFileTool(root, nil))
	runtimeMessages := []tea.Msg{}
	runtimeMessageCh := make(chan tea.Msg, 8)
	m := newModel(context.Background(), Options{
		Cwd:            root,
		ProviderName:   "openai",
		ModelName:      "gpt-4.1",
		Provider:       provider,
		Registry:       registry,
		SessionStore:   store,
		PermissionMode: agent.PermissionModeAsk,
		RuntimeMessageSink: func(msg tea.Msg) {
			runtimeMessages = append(runtimeMessages, msg)
			runtimeMessageCh <- msg
		},
		AgentOptions: agent.Options{
			Autonomy: "medium",
			Sandbox: sandbox.NewEngine(sandbox.EngineOptions{
				WorkspaceRoot: root,
				Policy:        promptWritePolicy(),
			}),
		},
	})
	m.input.SetValue("write notes")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd == nil {
		t.Fatal("expected prompt submit to start an agent run")
	}

	finalCh := make(chan tea.Msg, 1)
	go func() {
		finalCh <- execCmd(cmd)
	}()

	for received := 0; received < 4; {
		runtimeMsg := receiveRuntimeMessage(t, runtimeMessageCh)
		updated, _ = next.Update(runtimeMsg)
		next = updated.(model)
		// Live tool-call streaming-preview messages (the file being written) are
		// not one of the 4 semantic steps this test drives — process them so the
		// model state advances, but don't count them toward the budget.
		switch runtimeMsg.(type) {
		case toolCallStreamStartMsg, toolCallStreamDeltaMsg, interactiveExecStartMsg:
			continue
		}
		received++
		if _, ok := runtimeMsg.(permissionRequestMsg); ok {
			updated, _ = next.Update(testKeyText("d"))
			next = updated.(model)
		}
	}
	if !next.pending {
		t.Fatal("expected live runtime rows to leave run pending until final response")
	}
	for _, want := range []string{"tool call: write_file", "permission: write_file prompt", "permission: write_file deny", "tool result: write_file error"} {
		if !transcriptContains(next.transcript, want) {
			t.Fatalf("expected live transcript to contain %q, got %#v", want, next.transcript)
		}
	}
	finalMsg := receiveFinalMessage(t, finalCh)
	updated, _ = next.Update(finalMsg)
	next = updated.(model)

	if _, err := os.Stat(filepath.Join(root, "notes.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected prompt-gated write to stay blocked, stat error: %v", err)
	}
	list, err := store.List()
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected one session, got %d", len(list))
	}
	events, err := store.ReadEvents(list[0].SessionID)
	if err != nil {
		t.Fatalf("ReadEvents returned error: %v", err)
	}
	if got := eventTypes(events); !equalEventTypes(got, []sessions.EventType{
		sessions.EventMessage,
		sessions.EventToolCall,
		sessions.EventSessionCheckpoint,
		sessions.EventPermissionRequest,
		sessions.EventPermissionDecision,
		sessions.EventToolResult,
		sessions.EventMessage,
	}) {
		t.Fatalf("unexpected event sequence: %#v", got)
	}
	assertPayloadField(t, events[3], "toolCallId", "call_write")
	assertPayloadField(t, events[3], "name", "write_file")
	assertPayloadField(t, events[3], "action", "prompt")
	assertPayloadField(t, events[3], "permission", "prompt")
	assertPayloadField(t, events[3], "permissionMode", "ask")
	assertPayloadField(t, events[3], "sideEffect", "write")
	assertPayloadField(t, events[4], "action", "deny")
	assertPayloadField(t, events[4], "decisionReason", "denied in TUI")
	assertPayloadField(t, events[6], "content", "write blocked")
	if !transcriptContains(next.transcript, "permission: write_file prompt") {
		t.Fatalf("expected permission row in transcript, got %#v", next.transcript)
	}
	if countTranscriptRows(next.transcript, rowPermission) != 2 {
		t.Fatalf("expected request and decision permission rows, got %#v", next.transcript)
	}
}

func TestPermissionWaitDoesNotCountTowardTurnElapsed(t *testing.T) {
	store := testSessionStore(t)
	root := t.TempDir()
	base := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	var clockNanos atomic.Int64
	clockNanos.Store(base.UnixNano())
	provider := &scriptedProvider{scripts: [][]zeroruntime.StreamEvent{
		writeFileToolScript("call_write", "notes.txt", "hello"),
		textScript("write blocked"),
	}}
	provider.beforeCall = func(index int) {
		switch index {
		case 0:
			clockNanos.Store(base.Add(2 * time.Second).UnixNano())
		case 1:
			clockNanos.Store(base.Add(95 * time.Second).UnixNano())
		}
	}
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedWriteFileTool(root, nil))
	runtimeMessageCh := make(chan tea.Msg, 8)
	m := newPermissionTestModel(root, provider, registry, store, nil, runtimeMessageCh)

	m.now = func() time.Time {
		return time.Unix(0, clockNanos.Load()).UTC()
	}
	m.input.SetValue("write notes")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd == nil {
		t.Fatal("expected prompt submit to start an agent run")
	}

	finalCh := make(chan tea.Msg, 1)
	go func() {
		finalCh <- execCmd(cmd)
	}()

	for received := 0; received < 4; {
		runtimeMsg := receiveRuntimeMessage(t, runtimeMessageCh)
		updated, _ = next.Update(runtimeMsg)
		next = updated.(model)
		switch runtimeMsg.(type) {
		case toolCallStreamStartMsg, toolCallStreamDeltaMsg, interactiveExecStartMsg:
			continue
		case permissionRequestMsg:
			clockNanos.Store(base.Add(92 * time.Second).UnixNano())
			if status := plainRender(t, next.workingStatusLine()); !strings.Contains(status, "·  2s  ·") || strings.Contains(status, "still generating") {
				t.Fatalf("live elapsed advanced during permission wait: %q", status)
			}
			updated, _ = next.Update(testKeyText("d"))
			next = updated.(model)
			if hint := next.quietGenerationHint(); hint != "" {
				t.Fatalf("permission wait leaked into quiet-generation age: %q", hint)
			}
		}
		received++
	}

	finalMsg := receiveFinalMessage(t, finalCh)
	response, ok := finalMsg.(agentResponseMsg)
	if !ok {
		t.Fatalf("expected agent response, got %T", finalMsg)
	}
	if response.turnElapsed != 5*time.Second {
		t.Fatalf("turn elapsed = %s, want 5s of active work", response.turnElapsed)
	}
	if response.ttft != 5*time.Second {
		t.Fatalf("ttft = %s, want 5s with permission wait excluded", response.ttft)
	}
	if response.ttft > response.turnElapsed {
		t.Fatalf("ttft = %s exceeds turn elapsed = %s", response.ttft, response.turnElapsed)
	}
	updated, _ = next.Update(finalMsg)
	next = updated.(model)

	answer, ok := findTranscriptRow(next.transcript, rowAssistant)
	if !ok {
		t.Fatalf("expected final assistant row, got %#v", next.transcript)
	}
	if answer.turnElapsed != 5*time.Second {
		t.Fatalf("assistant turn elapsed = %s, want 5s", answer.turnElapsed)
	}
}

func TestPermissionPromptAllowWritesFileAndRecordsDecision(t *testing.T) {
	store := testSessionStore(t)
	root := t.TempDir()
	provider := &scriptedProvider{scripts: [][]zeroruntime.StreamEvent{
		writeFileToolScript("call_write", "notes.txt", "hello"),
		textScript("write allowed"),
	}}
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedWriteFileTool(root, nil))
	runtimeMessageCh := make(chan tea.Msg, 8)
	m := newPermissionTestModel(root, provider, registry, store, nil, runtimeMessageCh)

	next := submitAndDrivePermissionRun(t, m, "write notes", "a", runtimeMessageCh, 4)

	content, err := os.ReadFile(filepath.Join(root, "notes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "hello" {
		t.Fatalf("expected allow decision to write file, got %q", content)
	}
	events := readOnlySessionEvents(t, store)
	if got := eventTypes(events); !equalEventTypes(got, []sessions.EventType{
		sessions.EventMessage,
		sessions.EventToolCall,
		sessions.EventSessionCheckpoint,
		sessions.EventPermissionRequest,
		sessions.EventPermissionDecision,
		sessions.EventToolResult,
		sessions.EventMessage,
	}) {
		t.Fatalf("unexpected event sequence: %#v", got)
	}
	assertPayloadField(t, events[4], "action", "allow")
	assertPayloadField(t, events[4], "decisionReason", "approved in TUI")
	assertPayloadField(t, events[5], "status", "ok")
	assertPayloadField(t, events[6], "content", "write allowed")
	if !transcriptContains(next.transcript, "permission: write_file allow") {
		t.Fatalf("expected allow decision in transcript, got %#v", next.transcript)
	}
}

func TestPermissionPromptAlwaysPersistsGrantAndSkipsLaterPrompt(t *testing.T) {
	store := testSessionStore(t)
	root := t.TempDir()
	grantStore, err := sandbox.NewGrantStore(sandbox.StoreOptions{FilePath: filepath.Join(t.TempDir(), "sandbox-grants.json")})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{scripts: [][]zeroruntime.StreamEvent{
		writeFileToolScript("call_first", "notes.txt", "first"),
		textScript("first write"),
		writeFileToolScript("call_second", "notes.txt", "second"),
		textScript("second write"),
	}}
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedWriteFileTool(root, nil))
	runtimeMessageCh := make(chan tea.Msg, 8)
	m := newPermissionTestModel(root, provider, registry, store, grantStore, runtimeMessageCh)

	next := submitAndDrivePermissionRun(t, m, "write first", "y", runtimeMessageCh, 4)

	// The always-decision persists a grant scoped to exactly the file written.
	lookup, err := grantStore.Lookup("write_file", filepath.Join(root, "notes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !lookup.Matched || lookup.Grant.Decision != sandbox.GrantAllow {
		t.Fatalf("expected always decision to persist allow grant, got %#v", lookup)
	}
	if lookup.Grant.ScopeKind != sandbox.ScopeFile {
		t.Fatalf("expected file-scoped grant, got %#v", lookup.Grant)
	}
	content, err := os.ReadFile(filepath.Join(root, "notes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "first" {
		t.Fatalf("expected first approved write, got %q", content)
	}

	runtimeMessageCh = make(chan tea.Msg, 8)
	next.runtimeMessageSink = func(msg tea.Msg) {
		runtimeMessageCh <- msg
	}
	next = submitAndDrivePermissionRun(t, next, "write second", "", runtimeMessageCh, 3)

	content, err = os.ReadFile(filepath.Join(root, "notes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "second" {
		t.Fatalf("expected grant-backed second write, got %q", content)
	}
	events := readOnlySessionEvents(t, store)
	if countSessionEvents(events, sessions.EventPermissionRequest) != 1 {
		t.Fatalf("expected only first run to request permission, got %#v", eventTypes(events))
	}
	if countSessionEvents(events, sessions.EventPermissionDecision) != 2 {
		t.Fatalf("expected both runs to record decisions, got %#v", eventTypes(events))
	}
	secondDecision := nthSessionEvent(t, events, sessions.EventPermissionDecision, 2)
	assertPayloadField(t, secondDecision, "action", "allow")
	assertPayloadField(t, secondDecision, "grantMatched", true)
	if !transcriptContains(next.transcript, "second write") {
		t.Fatalf("expected second run answer in transcript, got %#v", next.transcript)
	}
}

func receiveRuntimeMessage(t *testing.T, messages <-chan tea.Msg) tea.Msg {
	t.Helper()
	select {
	case msg := <-messages:
		return msg
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for runtime message")
		return nil
	}
}

func receiveFinalMessage(t *testing.T, messages <-chan tea.Msg) tea.Msg {
	t.Helper()
	select {
	case msg := <-messages:
		return msg
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for final agent message")
		return nil
	}
}

func submitAndDrivePermissionRun(t *testing.T, m model, prompt string, key string, runtimeMessages <-chan tea.Msg, expectedRuntimeMessages int) model {
	t.Helper()
	m.input.SetValue(prompt)
	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd == nil {
		t.Fatal("expected prompt submit to start an agent run")
	}

	finalCh := make(chan tea.Msg, 1)
	go func() {
		finalCh <- execCmd(cmd)
	}()

	for received := 0; received < expectedRuntimeMessages; received++ {
		runtimeMsg := receiveRuntimeMessage(t, runtimeMessages)
		updated, _ = next.Update(runtimeMsg)
		next = updated.(model)
		if _, ok := runtimeMsg.(permissionRequestMsg); ok && key != "" {
			updated, _ = next.Update(testKeyText(key))
			next = updated.(model)
		}
	}

	finalMsg := receiveFinalMessage(t, finalCh)
	updated, _ = next.Update(finalMsg)
	return updated.(model)
}

func newPermissionTestModel(root string, provider zeroruntime.Provider, registry *tools.Registry, store *sessions.Store, grantStore *sandbox.GrantStore, runtimeMessages chan<- tea.Msg) model {
	engineOptions := sandbox.EngineOptions{
		WorkspaceRoot: root,
		Policy:        promptWritePolicy(),
	}
	if grantStore != nil {
		engineOptions.Store = grantStore
	}
	return newModel(context.Background(), Options{
		Cwd:            root,
		ProviderName:   "openai",
		ModelName:      "gpt-4.1",
		Provider:       provider,
		Registry:       registry,
		SessionStore:   store,
		SandboxStore:   grantStore,
		PermissionMode: agent.PermissionModeAsk,
		RuntimeMessageSink: func(msg tea.Msg) {
			runtimeMessages <- msg
		},
		AgentOptions: agent.Options{
			Autonomy: "medium",
			Sandbox:  sandbox.NewEngine(engineOptions),
		},
	})
}

func promptWritePolicy() sandbox.Policy {
	policy := sandbox.DefaultPolicy()
	policy.EnforceWorkspace = false
	return policy
}

func writeFileToolScript(callID string, path string, content string) []zeroruntime.StreamEvent {
	return []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: callID, ToolName: "write_file"},
		{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: callID, ArgumentsFragment: `{"path":` + jsonString(path) + `,"content":` + jsonString(content) + `,"overwrite":true}`},
		{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: callID},
		{Type: zeroruntime.StreamEventDone},
	}
}

func textScript(text string) []zeroruntime.StreamEvent {
	return []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventText, Content: text},
		{Type: zeroruntime.StreamEventDone},
	}
}

func jsonString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func readOnlySessionEvents(t *testing.T, store *sessions.Store) []sessions.Event {
	t.Helper()
	list, err := store.List()
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected one session, got %d", len(list))
	}
	events, err := store.ReadEvents(list[0].SessionID)
	if err != nil {
		t.Fatalf("ReadEvents returned error: %v", err)
	}
	return events
}

func countSessionEvents(events []sessions.Event, eventType sessions.EventType) int {
	count := 0
	for _, event := range events {
		if event.Type == eventType {
			count++
		}
	}
	return count
}

func nthSessionEvent(t *testing.T, events []sessions.Event, eventType sessions.EventType, ordinal int) sessions.Event {
	t.Helper()
	seen := 0
	for _, event := range events {
		if event.Type != eventType {
			continue
		}
		seen++
		if seen == ordinal {
			return event
		}
	}
	t.Fatalf("missing %s event #%d in %#v", eventType, ordinal, eventTypes(events))
	return sessions.Event{}
}

func TestResumeCommandHydratesSessionTranscript(t *testing.T) {
	store := testSessionStore(t)
	session, err := store.Create(sessions.CreateInput{Title: "Hydrate me", Cwd: "repo", ModelID: "gpt-4.1", Provider: "openai"})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	appendTestEvent(t, store, session.SessionID, sessions.EventMessage, map[string]any{"role": "user", "content": "previous request"})
	appendTestEvent(t, store, session.SessionID, sessions.EventToolCall, map[string]any{"id": "call_1", "name": "grep", "arguments": `{"pattern":"Zero"}`})
	appendTestEvent(t, store, session.SessionID, sessions.EventPermissionDecision, map[string]any{
		"toolCallId":     "call_1",
		"name":           "grep",
		"action":         "allow",
		"decisionAction": "allow",
		"permission":     "allow",
		"permissionMode": "ask",
		"sideEffect":     "read",
		"scope":          "src",
		"risk":           map[string]any{"level": "low"},
	})
	appendTestEvent(t, store, session.SessionID, sessions.EventToolResult, map[string]any{"toolCallId": "call_1", "name": "grep", "status": "error", "output": "matches"})
	appendTestEvent(t, store, session.SessionID, sessions.EventMessage, map[string]any{"role": "assistant", "content": "previous answer"})
	appendTestEvent(t, store, session.SessionID, sessions.EventError, map[string]any{"message": "old error"})

	m := newModel(context.Background(), Options{SessionStore: store})
	m.input.SetValue("/resume " + session.SessionID)

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd != nil {
		t.Fatal("expected /resume to hydrate synchronously")
	}
	for _, want := range []string{"Resumed Zero session", session.SessionID, "previous request", "tool call: grep", "permission: grep allow", "tool result: grep error matches", "previous answer", "old error"} {
		if !transcriptContains(next.transcript, want) {
			t.Fatalf("expected resumed transcript to contain %q, got %#v", want, next.transcript)
		}
	}
	toolCall, ok := findTranscriptRow(next.transcript, rowToolCall)
	if !ok || toolCall.tool != "grep" || toolCall.detail != "Zero" {
		t.Fatalf("expected hydrated tool call metadata, got ok=%v row=%#v", ok, toolCall)
	}
	permissionRow, ok := findTranscriptRow(next.transcript, rowPermission)
	if !ok || permissionRow.tool != "grep" || permissionRow.permission == nil || permissionRow.permission.Action != agent.PermissionActionAllow {
		t.Fatalf("expected hydrated permission metadata, got ok=%v row=%#v", ok, permissionRow)
	}
	if permissionRow.permission.Scope != "src" {
		t.Fatalf("expected hydrated permission scope, got %#v", permissionRow.permission)
	}
	toolResult, ok := findTranscriptRow(next.transcript, rowToolResult)
	if !ok || toolResult.tool != "grep" || toolResult.status != tools.StatusError || toolResult.detail != "matches" {
		t.Fatalf("expected hydrated tool result metadata, got ok=%v row=%#v", ok, toolResult)
	}
	if transcriptContains(next.transcript, "zero exec --resume") {
		t.Fatalf("resume should not show headless-only guidance after hydration, got %#v", next.transcript)
	}
}

// Regression: after /rewind the in-memory session state must be reloaded so the
// rewound-away events don't linger in the transcript or get re-sent to the agent
// as ContextEvents on the next prompt.
func TestRewindRefreshesInMemorySessionState(t *testing.T) {
	store := testSessionStore(t)
	session, err := store.Create(sessions.CreateInput{Title: "Rewind me", Cwd: t.TempDir(), ModelID: "gpt-4.1", Provider: "openai"})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	appendTestEvent(t, store, session.SessionID, sessions.EventMessage, map[string]any{"role": "user", "content": "first request"})
	// A checkpoint to rewind back through, then events that must be dropped.
	appendTestEvent(t, store, session.SessionID, sessions.EventSessionCheckpoint, map[string]any{"tool": "write_file", "files": []any{}})
	appendTestEvent(t, store, session.SessionID, sessions.EventMessage, map[string]any{"role": "assistant", "content": "DROPPED-AFTER-CHECKPOINT"})

	m := newModel(context.Background(), Options{SessionStore: store})
	m.input.SetValue("/resume " + session.SessionID)
	updated, _ := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if !transcriptContains(m.transcript, "DROPPED-AFTER-CHECKPOINT") {
		t.Fatalf("setup: resumed transcript should include the post-checkpoint message")
	}
	eventsBefore := len(m.sessionEvents)

	m, out, _ := m.handleRewindCommand("latest")
	if !strings.Contains(out, "Rewound") {
		t.Fatalf("expected a rewind summary, got %q", out)
	}

	if len(m.sessionEvents) >= eventsBefore {
		t.Fatalf("expected sessionEvents to shrink after rewind, before=%d after=%d", eventsBefore, len(m.sessionEvents))
	}
	for _, ev := range m.sessionEvents {
		if ev.Type == sessions.EventSessionCheckpoint {
			t.Fatalf("rewound-away checkpoint still in m.sessionEvents: %#v", m.sessionEvents)
		}
	}
	if transcriptContains(m.transcript, "DROPPED-AFTER-CHECKPOINT") {
		t.Fatalf("rewound-away message still visible in transcript: %#v", m.transcript)
	}
	// The crux: the next prompt must NOT re-send the rewound-away content.
	if prompt := m.sessionPrompt("next request"); strings.Contains(prompt, "DROPPED-AFTER-CHECKPOINT") {
		t.Fatalf("rewound-away content leaked into the next prompt: %q", prompt)
	}
}

func TestResumeCommandIsBlockedWhileRunPending(t *testing.T) {
	store := testSessionStore(t)
	active, err := store.Create(sessions.CreateInput{Title: "Active"})
	if err != nil {
		t.Fatalf("Create active returned error: %v", err)
	}
	other, err := store.Create(sessions.CreateInput{Title: "Other"})
	if err != nil {
		t.Fatalf("Create other returned error: %v", err)
	}
	m := newModel(context.Background(), Options{SessionStore: store})
	m.activeSession = active
	m.pending = true
	m.input.SetValue("/resume " + other.SessionID)

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd != nil {
		t.Fatal("expected pending /resume to be handled without a command")
	}
	if next.activeSession.SessionID != active.SessionID {
		t.Fatalf("expected pending /resume not to replace active session, got %#v", next.activeSession)
	}
	if !transcriptContains(next.transcript, "Cannot resume sessions while a run is active") {
		t.Fatalf("expected pending resume error, got %#v", next.transcript)
	}
}

func TestResumePickerExcludesSubRunSessions(t *testing.T) {
	store := testSessionStore(t)
	if _, err := store.Create(sessions.CreateInput{Title: "Real Conversation"}); err != nil {
		t.Fatalf("Create conversation: %v", err)
	}
	// A specialist/sub-agent run and a spec draft both create sessions; neither is
	// a standalone conversation and must not flood the /resume picker.
	if _, err := store.Create(sessions.CreateInput{Title: "Specialist Run", SessionKind: sessions.SessionKindChild}); err != nil {
		t.Fatalf("Create child: %v", err)
	}
	if _, err := store.Create(sessions.CreateInput{Title: "Spec Draft", SessionKind: sessions.SessionKindSpecDraft}); err != nil {
		t.Fatalf("Create spec draft: %v", err)
	}

	out := newModel(context.Background(), Options{SessionStore: store}).resumeText()

	if !strings.Contains(out, "Real Conversation") {
		t.Fatalf("resume picker should list the real conversation:\n%s", out)
	}
	if strings.Contains(out, "Specialist Run") || strings.Contains(out, "Spec Draft") {
		t.Fatalf("resume picker must exclude child/spec sub-runs:\n%s", out)
	}
}

func TestResumeLatestHydratesNewestSession(t *testing.T) {
	store := testSessionStore(t)
	older, err := store.Create(sessions.CreateInput{Title: "Older"})
	if err != nil {
		t.Fatalf("Create older returned error: %v", err)
	}
	appendTestEvent(t, store, older.SessionID, sessions.EventMessage, map[string]any{"role": "assistant", "content": "old answer"})
	newer, err := store.Create(sessions.CreateInput{Title: "Newer"})
	if err != nil {
		t.Fatalf("Create newer returned error: %v", err)
	}
	appendTestEvent(t, store, newer.SessionID, sessions.EventMessage, map[string]any{"role": "assistant", "content": "new answer"})

	m := newModel(context.Background(), Options{SessionStore: store})
	m.input.SetValue("/resume latest")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if !transcriptContains(next.transcript, "Newer") || !transcriptContains(next.transcript, "new answer") {
		t.Fatalf("expected latest session to hydrate, got %#v", next.transcript)
	}
	if transcriptContains(next.transcript, "old answer") {
		t.Fatalf("expected /resume latest to skip older session transcript, got %#v", next.transcript)
	}
}

func TestEscCancelRecordsSessionError(t *testing.T) {
	store := testSessionStore(t)
	provider := &fakeProvider{events: []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventText, Content: "ignored"},
		{Type: zeroruntime.StreamEventDone},
	}}
	m := newModel(context.Background(), Options{
		Provider:     provider,
		Registry:     tools.NewRegistry(),
		SessionStore: store,
	})
	m.input.SetValue("cancel me")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd == nil {
		t.Fatal("expected prompt submit to start an agent run")
	}

	updated, _ = next.Update(testKey(tea.KeyEsc))
	next = updated.(model)
	updated, _ = next.Update(testKey(tea.KeyEsc))
	next = updated.(model)

	list, err := store.List()
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected one session, got %d", len(list))
	}
	events, err := store.ReadEvents(list[0].SessionID)
	if err != nil {
		t.Fatalf("ReadEvents returned error: %v", err)
	}
	if got := eventTypes(events); !equalEventTypes(got, []sessions.EventType{
		sessions.EventMessage,
		sessions.EventError,
	}) {
		t.Fatalf("unexpected event sequence after cancel: %#v", got)
	}
	assertPayloadField(t, events[1], "message", "Run cancelled.")
	if next.pending {
		t.Fatal("expected Esc to clear pending state")
	}
}

func TestCancelledRunFlushesCheckpointSessionEvents(t *testing.T) {
	store := testSessionStore(t)
	root := t.TempDir()
	writeTestFile(t, root, "notes.txt", "before")
	provider := &scriptedProvider{scripts: [][]zeroruntime.StreamEvent{
		writeFileToolScript("call_write", "notes.txt", "after"),
		textScript("never reached"),
	}}
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedWriteFileTool(root, nil))
	runtimeMessageCh := make(chan tea.Msg, 8)
	m := newPermissionTestModel(root, provider, registry, store, nil, runtimeMessageCh)
	m.input.SetValue("rewrite notes")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd == nil {
		t.Fatal("expected prompt submit to start an agent run")
	}

	finalCh := make(chan tea.Msg, 1)
	go func() {
		finalCh <- execCmd(cmd)
	}()

	// Drain live runtime messages until the permission prompt is up. The tool call
	// (and its checkpoint snapshot) is captured into the goroutine's in-flight
	// sessionEvents before the run blocks on the permission decision.
	cancelled := false
	for !cancelled {
		runtimeMsg := receiveRuntimeMessage(t, runtimeMessageCh)
		updated, _ = next.Update(runtimeMsg)
		next = updated.(model)
		if _, ok := runtimeMsg.(permissionRequestMsg); ok {
			// Cancel mid-run via Esc while the permission prompt is pending: this
			// unblocks the goroutine through ctx cancellation. Esc now requires a
			// second press within the confirmation window to actually cancel.
			updated, _ = next.Update(testKey(tea.KeyEsc))
			next = updated.(model)
			updated, _ = next.Update(testKey(tea.KeyEsc))
			next = updated.(model)
			cancelled = true
		}
	}
	if next.pending {
		t.Fatal("expected Esc to clear pending state")
	}
	if len(next.flushRunIDs) == 0 {
		t.Fatal("expected cancelled run to be flagged for session-event flush")
	}

	// The cancelled goroutine still returns its accumulated session events. Deliver
	// that final message; the checkpoint must be persisted even though activeRunID
	// is already zeroed.
	finalMsg := receiveFinalMessage(t, finalCh)
	updated, _ = next.Update(finalMsg)
	next = updated.(model)
	if len(next.flushRunIDs) != 0 {
		t.Fatalf("expected flush set to clear after draining cancelled run, got %v", next.flushRunIDs)
	}

	events := readOnlySessionEvents(t, store)
	if countSessionEvents(events, sessions.EventSessionCheckpoint) != 1 {
		t.Fatalf("expected the in-flight checkpoint to be persisted on cancel, got %#v", eventTypes(events))
	}
	if countSessionEvents(events, sessions.EventToolCall) != 1 {
		t.Fatalf("expected the in-flight tool call to be persisted on cancel, got %#v", eventTypes(events))
	}
	// The cancel path records exactly one "Run cancelled." error; the goroutine's
	// trailing cancellation error must be dropped, not double-recorded.
	if got := countSessionEvents(events, sessions.EventError); got != 1 {
		t.Fatalf("expected exactly one cancellation error event, got %d in %#v", got, eventTypes(events))
	}
	cancelErr := nthSessionEvent(t, events, sessions.EventError, 1)
	assertPayloadField(t, cancelErr, "message", "Run cancelled.")
}

func TestCtrlCCancelsAndFlushesCheckpointSessionEvents(t *testing.T) {
	store := testSessionStore(t)
	root := t.TempDir()
	writeTestFile(t, root, "notes.txt", "before")
	provider := &scriptedProvider{scripts: [][]zeroruntime.StreamEvent{
		writeFileToolScript("call_write", "notes.txt", "after"),
		textScript("never reached"),
	}}
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedWriteFileTool(root, nil))
	runtimeMessageCh := make(chan tea.Msg, 8)
	m := newPermissionTestModel(root, provider, registry, store, nil, runtimeMessageCh)
	m.input.SetValue("rewrite notes")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd == nil {
		t.Fatal("expected prompt submit to start an agent run")
	}

	finalCh := make(chan tea.Msg, 1)
	go func() {
		finalCh <- execCmd(cmd)
	}()

	// Drain live runtime messages until the permission prompt is up; the tool call
	// and its checkpoint snapshot are captured into the goroutine's in-flight
	// sessionEvents before the run blocks on the permission decision.
	cancelled := false
	for !cancelled {
		runtimeMsg := receiveRuntimeMessage(t, runtimeMessageCh)
		updated, _ = next.Update(runtimeMsg)
		next = updated.(model)
		if _, ok := runtimeMsg.(permissionRequestMsg); ok {
			// Ctrl+C while the permission prompt is pending: this must cancel the
			// in-flight run (unblocking the goroutine via ctx), arm the second-press
			// exit confirmation, and still avoid quitting before the in-flight run's
			// final message has been drained, or the captured checkpoint is orphaned.
			updated, _ = next.Update(testKeyCtrl('c'))
			next = updated.(model)
			if next.exiting {
				t.Fatal("first Ctrl+C should not mark model exiting")
			}
			if !next.exitConfirmActive {
				t.Fatal("first Ctrl+C should arm exit confirmation")
			}
			cancelled = true
		}
	}
	if next.pending {
		t.Fatal("expected Ctrl+C to clear pending state")
	}
	if len(next.flushRunIDs) == 0 {
		t.Fatal("expected cancelled run to be flagged for session-event flush")
	}

	updated, quitCmd := next.Update(testKeyCtrl('c'))
	next = updated.(model)
	if !next.exiting {
		t.Fatal("second Ctrl+C should mark model exiting")
	}
	if quitCmd != nil {
		t.Fatal("second Ctrl+C should wait for the cancelled run's flush before quitting")
	}

	// The cancelled goroutine still returns its accumulated session events. Deliver
	// that final message; the checkpoint must be persisted even though activeRunID
	// is already zeroed, and the second Ctrl+C's deferred quit must fire now.
	finalMsg := receiveFinalMessage(t, finalCh)
	updated, quitCmd = next.Update(finalMsg)
	next = updated.(model)
	if len(next.flushRunIDs) != 0 {
		t.Fatalf("expected flush set to clear after draining cancelled run, got %v", next.flushRunIDs)
	}
	if quitCmd == nil {
		t.Fatal("expected deferred quit to fire once the in-flight run is flushed on second Ctrl+C")
	}

	events := readOnlySessionEvents(t, store)
	if countSessionEvents(events, sessions.EventSessionCheckpoint) != 1 {
		t.Fatalf("expected the in-flight checkpoint to be persisted on Ctrl+C, got %#v", eventTypes(events))
	}
	if countSessionEvents(events, sessions.EventToolCall) != 1 {
		t.Fatalf("expected the in-flight tool call to be persisted on Ctrl+C, got %#v", eventTypes(events))
	}
	// The cancel path records exactly one "Run cancelled." error; the goroutine's
	// trailing cancellation error must be dropped, not double-recorded.
	if got := countSessionEvents(events, sessions.EventError); got != 1 {
		t.Fatalf("expected exactly one cancellation error event, got %d in %#v", got, eventTypes(events))
	}
}

func TestResumedPromptIncludesSessionContext(t *testing.T) {
	store := testSessionStore(t)
	session, err := store.Create(sessions.CreateInput{Title: "Existing", Cwd: "repo", ModelID: "gpt-4.1", Provider: "openai"})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	appendTestEvent(t, store, session.SessionID, sessions.EventMessage, map[string]any{"role": "user", "content": "previous request"})
	appendTestEvent(t, store, session.SessionID, sessions.EventMessage, map[string]any{"role": "assistant", "content": "previous answer"})
	provider := &scriptedProvider{scripts: [][]zeroruntime.StreamEvent{{
		{Type: zeroruntime.StreamEventText, Content: "continued"},
		{Type: zeroruntime.StreamEventDone},
	}}}
	m := newModel(context.Background(), Options{
		Provider:     provider,
		Registry:     tools.NewRegistry(),
		SessionStore: store,
	})
	m.input.SetValue("/resume " + session.SessionID)
	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	next.input.SetValue("continue")

	updated, cmd := next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if cmd == nil {
		t.Fatal("expected resumed prompt to start an agent run")
	}
	_, _ = next.Update(execCmd(cmd))

	if len(provider.requests) != 1 {
		t.Fatalf("expected one provider request, got %d", len(provider.requests))
	}
	messages := provider.requests[0].Messages
	if len(messages) == 0 {
		t.Fatal("expected provider request to include messages")
	}
	prompt := messages[len(messages)-1].Content
	for _, want := range []string{"Continuing Zero session", session.SessionID, "previous request", "previous answer", "Current user request:", "continue"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("expected resumed prompt to contain %q, got %q", want, prompt)
		}
	}
}

func TestResumeActiveGoalDoesNotStartAutomaticRun(t *testing.T) {
	store := testSessionStore(t)
	session, err := store.Create(sessions.CreateInput{SessionID: "resume_active_goal"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateGoal(session.SessionID, "Wait for explicit resume", 0); err != nil {
		t.Fatal(err)
	}
	m := newModel(context.Background(), Options{
		Provider:     &scriptedProvider{},
		Registry:     tools.NewRegistry(),
		SessionStore: store,
	})
	m.input.SetValue("/resume " + session.SessionID)

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd != nil || next.pending {
		t.Fatal("resuming a session unexpectedly started a billed goal run")
	}
	if next.activeSession.Goal == nil || next.activeSession.Goal.Status != sessions.GoalStatusActive {
		t.Fatalf("active goal was not restored: %#v", next.activeSession.Goal)
	}
	if !transcriptContains(next.transcript, "run /goal resume to continue") {
		t.Fatalf("resume did not explain how to continue the goal: %#v", next.transcript)
	}
}

func TestResumeCommandReportsMissingSession(t *testing.T) {
	m := newModel(context.Background(), Options{SessionStore: testSessionStore(t)})
	m.input.SetValue("/resume missing_session")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if !transcriptContains(next.transcript, "zero session not found: missing_session") {
		t.Fatalf("expected missing session error, got %#v", next.transcript)
	}
}

func appendTestEvent(t *testing.T, store *sessions.Store, sessionID string, eventType sessions.EventType, payload any) {
	t.Helper()

	if _, err := store.AppendEvent(sessionID, sessions.AppendEventInput{Type: eventType, Payload: payload}); err != nil {
		t.Fatalf("AppendEvent(%s) returned error: %v", eventType, err)
	}
}

func writeTestFile(t *testing.T, root string, name string, content string) {
	t.Helper()

	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write %s: %v", path, err)
	}
}

func eventTypes(events []sessions.Event) []sessions.EventType {
	types := make([]sessions.EventType, 0, len(events))
	for _, event := range events {
		types = append(types, event.Type)
	}
	return types
}

func equalEventTypes(left []sessions.EventType, right []sessions.EventType) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func assertPayloadField(t *testing.T, event sessions.Event, key string, want any) {
	t.Helper()

	payload := map[string]any{}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("failed to decode payload for %s: %v", event.Type, err)
	}
	if payload[key] != want {
		t.Fatalf("expected payload %s=%#v, got %#v in %#v", key, want, payload[key], payload)
	}
}

func assertPayloadFieldContains(t *testing.T, event sessions.Event, key string, want string) {
	t.Helper()

	payload := map[string]any{}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("failed to decode payload for %s: %v", event.Type, err)
	}
	text, ok := payload[key].(string)
	if !ok || !strings.Contains(text, want) {
		t.Fatalf("expected payload %s to contain %q, got %#v in %#v", key, want, payload[key], payload)
	}
}

func TestRelativeAgeFormatsLastActivity(t *testing.T) {
	now := time.Date(2026, 9, 12, 15, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		ts   time.Time
		want string
	}{
		{"just now", now.Add(-10 * time.Second), "now"},
		{"slight clock skew", now.Add(30 * time.Second), "now"},
		{"minutes", now.Add(-42 * time.Minute), "42m ago"},
		{"hours", now.Add(-3 * time.Hour), "3h ago"},
		{"days", now.Add(-12 * 24 * time.Hour), "12d ago"},
		{"exactly one month", now.Add(-30 * 24 * time.Hour), "30d ago"},
		{"this year", time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC), "Jan  5"},
		{"last year", time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC), "2025-06-01"},
	}
	for _, tc := range cases {
		if got := relativeAge(tc.ts.Format(time.RFC3339), now); got != tc.want {
			t.Errorf("%s: sessionAge = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := relativeAge("not-a-time", now); got != "" {
		t.Errorf("unparseable timestamp = %q, want empty", got)
	}
	if got := relativeAge("", now); got != "" {
		t.Errorf("empty timestamp = %q, want empty", got)
	}
}

func TestSessionPickerDetailComposesProjectAndModel(t *testing.T) {
	m := model{cwd: "/repo"}
	detail := m.sessionPickerDetail(sessions.Metadata{
		Title:   "work",
		Cwd:     "/repo",
		ModelID: "gpt-5",
	})
	for _, want := range []string{"/repo", "gpt-5"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail %q missing %q", detail, want)
		}
	}
	// Missing fields are omitted rather than rendered as empty segments.
	if sparse := m.sessionPickerDetail(sessions.Metadata{Title: "bare"}); sparse != "" {
		t.Fatalf("empty metadata should produce no detail line, got %q", sparse)
	}
}

func TestSessionPickerDetailStatusChips(t *testing.T) {
	m := model{}
	if got := m.sessionPickerDetail(sessions.Metadata{SessionKind: sessions.SessionKindFork}); !strings.Contains(got, "fork") {
		t.Fatalf("fork session detail %q should carry the fork chip", got)
	}
	if got := m.sessionPickerDetail(sessions.Metadata{Tag: "btw"}); !strings.Contains(got, "btw") {
		t.Fatalf("tagged session detail %q should carry the tag", got)
	}
	if got := m.sessionPickerDetail(sessions.Metadata{Title: "plain"}); got != "" {
		t.Fatalf("plain session detail %q should have no status chip", got)
	}
}

func TestResumePickerDetailRendersOnlyAtMediumWidth(t *testing.T) {
	store := testSessionStore(t)
	sess, err := store.Create(sessions.CreateInput{Title: "Detail row", ModelID: "gpt-5-pickertest", Provider: "openai"})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if _, err := store.AppendEvent(sess.SessionID, sessions.AppendEventInput{
		Type:    sessions.EventMessage,
		Payload: map[string]any{"role": "assistant", "content": "hi"},
	}); err != nil {
		t.Fatalf("AppendEvent returned error: %v", err)
	}
	m := newModel(context.Background(), Options{SessionStore: store})
	m.input.SetValue("/resume")
	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if next.picker == nil || next.picker.kind != pickerSession {
		t.Fatalf("expected /resume to open the session picker, got %#v", next.picker)
	}

	next.width = 120
	if wide := viewString(next.View()); !strings.Contains(wide, "gpt-5-pickertest") {
		t.Fatalf("wide picker should render the detail line with the model:\n%s", wide)
	}
	next.width = 60
	if narrow := viewString(next.View()); strings.Contains(narrow, "gpt-5-pickertest") {
		t.Fatalf("narrow picker should keep the single-line fallback:\n%s", narrow)
	}
}

func TestResumePickerFilterMatchesDetailFields(t *testing.T) {
	store := testSessionStore(t)
	mk := func(title, modelID string) {
		sess, err := store.Create(sessions.CreateInput{Title: title, ModelID: modelID, Provider: "openai"})
		if err != nil {
			t.Fatalf("Create %q returned error: %v", title, err)
		}
		if _, err := store.AppendEvent(sess.SessionID, sessions.AppendEventInput{
			Type:    sessions.EventMessage,
			Payload: map[string]any{"role": "assistant", "content": "hi"},
		}); err != nil {
			t.Fatalf("AppendEvent %q returned error: %v", title, err)
		}
	}
	mk("alpha", "gpt-5")
	mk("beta", "claude-sonnet-4.5")

	m := newModel(context.Background(), Options{SessionStore: store})
	m.input.SetValue("/resume")
	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if next.picker == nil {
		t.Fatal("expected /resume to open the session picker")
	}
	// The model id lives only on the detail line; it must still filter rows.
	next.picker.query = "claude-sonnet"
	next.picker.applyQuery()
	if len(next.picker.items) != 1 || !strings.Contains(next.picker.items[0].Label, "beta") {
		t.Fatalf("filter by model should keep only the matching session, got %#v", next.picker.items)
	}
}

func TestResumePickerSanitizesMultilineMetadata(t *testing.T) {
	// Persisted session metadata is user-controlled: a title or model id can
	// legally contain newlines, which must not break the picker's fixed
	// one-or-two-line row geometry (a stray line shifts every row below it and
	// misaligns mouse hit-testing).
	store := testSessionStore(t)
	sess, err := store.Create(sessions.CreateInput{
		Title:   "alpha\nbravo",
		ModelID: "gpt\x1b[31m5",
		Cwd:     "/repo",
		Tag:     "side\x1b]0;x\achat",
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if _, err := store.AppendEvent(sess.SessionID, sessions.AppendEventInput{
		Type:    sessions.EventMessage,
		Payload: map[string]any{"role": "assistant", "content": "hi"},
	}); err != nil {
		t.Fatalf("AppendEvent returned error: %v", err)
	}
	m := newModel(context.Background(), Options{SessionStore: store, Cwd: "/repo"})
	m.input.SetValue("/resume")
	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if next.picker == nil || len(next.picker.items) != 1 {
		t.Fatalf("expected one picker item, got %#v", next.picker)
	}
	item := next.picker.items[0]
	for _, r := range item.Label + item.Detail {
		if unicode.IsControl(r) {
			t.Fatalf("picker row leaked control rune %q: label=%q detail=%q", r, item.Label, item.Detail)
		}
	}
}

func TestGenericPickerMouseSelectsItemFromDetailLine(t *testing.T) {
	m := mouseTestModel()
	m.width = 120
	m.picker = &commandPicker{
		kind: pickerSession,
		items: []pickerItem{
			{Label: "one", Value: "s1", Detail: "meta one"},
			{Label: "two", Value: "s2", Detail: "meta two"},
		},
	}
	m.picker.allItems = append([]pickerItem{}, m.picker.items...)

	width := chatWidth(m.width)
	overlay := m.pickerOverlay(width)
	rect := m.overlayMouseRect(len(viewLines(overlay)), width)
	// Rows begin at y=3 inside the overlay; item 1's detail line is y=6.
	// Click near the horizontal middle so the point lands inside the
	// centered overlay block.
	target, ok := m.selectGenericPickerAtMouse(testMouseClick(tea.MouseLeft, width/2, rect.y+6))
	if !ok {
		t.Fatal("click on a detail line should select its item")
	}
	if target.Value != "s2" {
		t.Fatalf("detail-line click selected %q, want s2", target.Value)
	}
}
