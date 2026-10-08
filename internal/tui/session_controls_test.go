package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

func TestEffortCommandListsAndSetsSupportedEffort(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "claude-sonnet-4.5"})
	m.input.SetValue("/effort list")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd != nil {
		t.Fatal("expected /effort list to be handled without starting an agent run")
	}
	// The output is now a command card payload, so the effort list and the
	// active effort appear inside the same row's text rather than as separate
	// transcript rows. Strip the card prefix and assert against the rendered
	// payload.
	var cardPayload string
	for _, row := range next.transcript {
		if strings.HasPrefix(row.text, "\x00command-card\x00") {
			cardPayload = strings.TrimPrefix(row.text, "\x00command-card\x00")
			break
		}
	}
	if cardPayload == "" {
		t.Fatalf("expected an effort command card row, got %#v", next.transcript)
	}
	for _, want := range []string{"Effort", "active effort: auto", "available", "low, medium, high"} {
		if !strings.Contains(cardPayload, want) {
			t.Fatalf("expected card to contain %q, got %q", want, cardPayload)
		}
	}

	next.input.SetValue("/effort high")
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)

	if next.pending {
		t.Fatal("expected /effort high to be handled without starting an agent run")
	}
	if next.reasoningEffort != modelregistry.ReasoningEffortHigh {
		t.Fatalf("expected effort high, got %q", next.reasoningEffort)
	}
	if next.transientNotice.text != "Reasoning effort: high" {
		t.Fatalf("effort switch notice = %q", next.transientNotice.text)
	}
}

func TestEffortCommandRejectsUnsupportedActiveModel(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4.1"})
	m.input.SetValue("/effort high")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if next.reasoningEffort != "" {
		t.Fatalf("expected effort to remain auto, got %q", next.reasoningEffort)
	}
	if !transcriptContains(next.transcript, "does not expose reasoning effort controls") {
		t.Fatalf("expected unsupported model message, got %#v", next.transcript)
	}
}

// The Ctrl+T cycle walks the active model's supported ring:
// auto ("") -> first supported -> ... -> last supported -> auto. These cover
// every branch of cycleReasoningEffort: empty/auto start, mid-ring advance,
// last-slot wrap, an effort the model doesn't support, and a model with no
// effort controls at all.

func TestCycleReasoningEffortAutoToFirst(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "claude-sonnet-4.5"})
	if m.reasoningEffort != "" {
		t.Fatalf("expected default effort auto, got %q", m.reasoningEffort)
	}
	next, cmd := m.cycleReasoningEffort()
	if cmd != nil {
		t.Fatal("expected cycle to produce no command")
	}
	if next.reasoningEffort != modelregistry.ReasoningEffortLow {
		t.Fatalf("expected cycle from auto to land on first supported effort (low), got %q", next.reasoningEffort)
	}
}

func TestCycleReasoningEffortAdvancesToNext(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "claude-sonnet-4.5"})
	m.reasoningEffort = modelregistry.ReasoningEffortLow
	next, _ := m.cycleReasoningEffort()
	if next.reasoningEffort != modelregistry.ReasoningEffortMedium {
		t.Fatalf("expected cycle low -> medium, got %q", next.reasoningEffort)
	}
}

func TestCycleReasoningEffortWrapsToAuto(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "claude-sonnet-4.5"})
	m.reasoningEffort = modelregistry.ReasoningEffortHigh
	next, _ := m.cycleReasoningEffort()
	if next.reasoningEffort != "" {
		t.Fatalf("expected cycle from last supported (high) to wrap to auto, got %q", next.reasoningEffort)
	}
}

func TestCycleReasoningEffortUnknownResetsToAuto(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "claude-sonnet-4.5"})
	// Use a sentinel that is guaranteed to be unplaceable in any supported ring,
	// so this test stays correct even if Minimal gets supported later.
	m.reasoningEffort = modelregistry.ReasoningEffort("__unknown_effort__")
	next, _ := m.cycleReasoningEffort()
	if next.reasoningEffort != "" {
		t.Fatalf("expected unknown effort to reset to auto, got %q", next.reasoningEffort)
	}
}

func TestCycleReasoningEffortNoOpOnUnsupportedModel(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4.1"})
	// gpt-4.1 exposes no effort controls; set a value directly (the /effort command
	// would reject this) to prove the cycle is a true no-op and leaves it untouched.
	m.reasoningEffort = modelregistry.ReasoningEffortHigh
	next, cmd := m.cycleReasoningEffort()
	if cmd != nil {
		t.Fatal("expected cycle on unsupported model to produce no command")
	}
	if next.reasoningEffort != modelregistry.ReasoningEffortHigh {
		t.Fatalf("expected cycle to be a no-op on a model without effort controls, got %q", next.reasoningEffort)
	}
}

func TestStyleCommandListsAndSetsSessionPreference(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.input.SetValue("/style")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd != nil {
		t.Fatal("expected /style to be handled without starting an agent run")
	}
	if !transcriptContains(next.transcript, "active style: concise") || !transcriptContains(next.transcript, "explanatory") {
		t.Fatalf("expected style list transcript, got %#v", next.transcript)
	}

	next.input.SetValue("/style explanatory")
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)

	if next.pending {
		t.Fatal("expected /style explanatory to be handled without starting an agent run")
	}
	if next.responseStyle != "explanatory" {
		t.Fatalf("expected explanatory style, got %q", next.responseStyle)
	}
	if next.transientNotice.text != "Style: explanatory" {
		t.Fatalf("style switch notice = %q", next.transientNotice.text)
	}
}

func TestCompactStatusShowsManualFlowState(t *testing.T) {
	called := false
	m := newModel(context.Background(), Options{
		ModelName: "gpt-4.1",
		SessionCompactor: compactSessionFunc(func(context.Context, CompactRequest) (CompactResult, error) {
			called = true
			return CompactResult{}, nil
		}),
	})
	m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowUser, text: strings.Repeat("abcd ", 80)})
	m.input.SetValue("/compact status")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd != nil {
		t.Fatal("expected /compact status to be handled without starting an agent run")
	}
	if called {
		t.Fatal("status should not invoke the manual compactor")
	}
	if next.compactRequests != 0 {
		t.Fatalf("status should not add compact requests, got %d", next.compactRequests)
	}
	for _, want := range []string{
		"Compact",
		"status: info",
		"model: gpt-4.1",
		"context window: 1M tokens",
		"estimated transcript:",
		"compactable: yes",
	} {
		if !transcriptContains(next.transcript, want) {
			t.Fatalf("expected compact status transcript to contain %q, got %#v", want, next.transcript)
		}
	}
}

func TestCompactCommandCallsInjectedCompactorAndReportsResult(t *testing.T) {
	var request CompactRequest
	calls := 0
	m := newModel(context.Background(), Options{
		ModelName: "gpt-4.1",
		SessionCompactor: compactSessionFunc(func(_ context.Context, req CompactRequest) (CompactResult, error) {
			calls++
			request = req
			return CompactResult{
				Compacted:    true,
				BeforeTokens: req.EstimatedTokens,
				AfterTokens:  42,
				Summary:      "summarized earlier turns",
			}, nil
		}),
	})
	m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowUser, text: strings.Repeat("context ", 90)})
	m.input.SetValue("/compact")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd == nil {
		t.Fatal("expected /compact to start an async compaction command")
	}
	if !next.compactInFlight {
		t.Fatal("expected compaction to be marked in flight")
	}
	for _, want := range []string{
		"Compressing session",
		"Keep editing your draft; press Enter after compression finishes to send.",
		"Compressing history",
	} {
		if !transcriptContains(next.transcript, want) {
			t.Fatalf("expected compact start transcript to contain %q, got %#v", want, next.transcript)
		}
	}
	for _, unwanted := range []string{"context window:", "compactable:", "hint:", "State"} {
		if transcriptContains(next.transcript, unwanted) {
			t.Fatalf("compact running transcript should not contain diagnostic %q, got %#v", unwanted, next.transcript)
		}
	}
	msg := execCmd(cmd)
	if msg == nil {
		t.Fatal("expected async compact command to return a completion message")
	}
	updated, cmd = next.Update(msg)
	next = updated.(model)

	if cmd != nil {
		t.Fatal("expected compact completion to be handled without starting another command")
	}
	if calls != 1 {
		t.Fatalf("expected one manual compaction call, got %d", calls)
	}
	if request.ModelName != "gpt-4.1" {
		t.Fatalf("expected request model gpt-4.1, got %q", request.ModelName)
	}
	if request.ContextWindow != next.modelContextWindow("gpt-4.1") {
		t.Fatalf("expected request context window %d, got %d", next.modelContextWindow("gpt-4.1"), request.ContextWindow)
	}
	if request.EstimatedTokens <= 0 || request.VisibleTranscriptRows != len(m.transcript) {
		t.Fatalf("expected request estimate and transcript count, got %#v", request)
	}
	if next.compactRequests != 1 {
		t.Fatalf("expected one compact request, got %d", next.compactRequests)
	}
	for _, want := range []string{
		"Compression complete",
		"Session summary saved",
		"Ready for the next prompt",
	} {
		if !transcriptContains(next.transcript, want) {
			t.Fatalf("expected compact transcript to contain %q, got %#v", want, next.transcript)
		}
	}
	for _, unwanted := range []string{"context window:", "compactable:", "hint:", "summarized earlier turns"} {
		if transcriptContains(next.transcript, unwanted) {
			t.Fatalf("compact completion transcript should not contain diagnostic %q, got %#v", unwanted, next.transcript)
		}
	}
	if got := next.compactionStatus(); !strings.Contains(got, "compacted manually") {
		t.Fatalf("expected compacted status after manual compaction, got %q", got)
	}
}

func TestCompactSpinnerTickRefreshesProgressFrame(t *testing.T) {
	m := newModel(context.Background(), Options{
		ModelName: "gpt-4.1",
		SessionCompactor: compactSessionFunc(func(context.Context, CompactRequest) (CompactResult, error) {
			return CompactResult{Compacted: true, Summary: "done"}, nil
		}),
	})
	m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowUser, text: strings.Repeat("context ", 90)})
	// The compact ring animates on tick, which reduced motion freezes; CI is
	// no-TTY (reducedMotion auto-on), so force it off to test the animation.
	m.reducedMotion = false
	m.input.SetValue("/compact")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd == nil || !next.compactInFlight {
		t.Fatal("expected /compact to start an in-flight animated compaction")
	}
	before := compactStatusText(next.transcript)
	updated, _ = next.Update(next.spinner.Tick())
	next = updated.(model)
	after := compactStatusText(next.transcript)
	if before == "" || after == "" {
		t.Fatalf("expected compact status row before=%q after=%q", before, after)
	}
	if before == after {
		t.Fatalf("expected spinner tick to refresh compact status, still %q", after)
	}
	if !strings.Contains(after, "Compressing history") {
		t.Fatalf("expected animated compact copy, got %q", after)
	}
	if strings.Contains(after, "context window:") || strings.Contains(after, "compactable:") {
		t.Fatalf("animated compact copy should stay concise, got %q", after)
	}
}

func TestCompactRunningRowRendersAsAmberCompressionCard(t *testing.T) {
	m := newModel(context.Background(), Options{
		ModelName: "gpt-4.1",
		SessionCompactor: compactSessionFunc(func(context.Context, CompactRequest) (CompactResult, error) {
			return CompactResult{Compacted: true, Summary: "done"}, nil
		}),
	})
	m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowUser, text: strings.Repeat("context ", 90)})
	m.input.SetValue("/compact")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	rendered := plainRender(t, next.renderRow(transcriptRow{
		kind: rowSystem,
		id:   compactStatusRowID,
		text: compactStatusText(next.transcript),
	}, 92, buildRowContext(next.transcript)))

	for _, want := range []string{
		"╭",
		"╰",
		"Compressing session",
		"Keep editing your draft; press Enter after compression finishes to send.",
		"Compressing history",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("expected compact card render to contain %q, got:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "context window:") || strings.Contains(rendered, "compactable:") {
		t.Fatalf("compact card should not include diagnostic status text:\n%s", rendered)
	}
}

func TestCompactCompleteRowRendersAsSuccessCard(t *testing.T) {
	m := newModel(context.Background(), Options{})
	rendered := plainRender(t, m.renderRow(transcriptRow{
		kind: rowSystem,
		id:   compactStatusRowID,
		text: compactCompleteText(CompactResult{
			Compacted:    true,
			BeforeTokens: 320,
			AfterTokens:  120,
			Summary:      "raw model summary should stay hidden",
		}),
	}, 92, rowContext{}))

	for _, want := range []string{
		"╭",
		"╰",
		"Compression complete",
		"Session summary saved",
		"Ready for the next prompt",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("expected compact complete card to contain %q, got:\n%s", want, rendered)
		}
	}
	for _, unwanted := range []string{"raw model summary", "context window:", "compactable:"} {
		if strings.Contains(rendered, unwanted) {
			t.Fatalf("compact complete card should not contain %q, got:\n%s", unwanted, rendered)
		}
	}
}

func TestCompactCommandRecordsSessionCompactionAndShrinksReplayContext(t *testing.T) {
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	m := newModel(context.Background(), Options{
		ModelName:    "gpt-4.1",
		SessionStore: store,
	})
	var err error
	m, err = m.ensureActiveSession("compact this session")
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{
		"alpha old user intent",
		"beta old assistant answer",
		"gamma old tool result",
		"delta old follow-up",
		"epsilon recent",
		"zeta recent",
		"eta recent",
		"theta recent",
		"iota recent",
		"kappa recent",
		"lambda recent",
		"mu recent",
	} {
		m, err = m.appendSessionEvent(sessions.EventMessage, map[string]any{
			"role":    "user",
			"content": content,
		})
		if err != nil {
			t.Fatal(err)
		}
		m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowUser, text: content})
	}
	eventsBefore := len(m.sessionEvents)
	m.input.SetValue("/compact")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd == nil {
		t.Fatal("expected /compact to start an async session-backed compaction command")
	}
	msg := execCmd(cmd)
	if msg == nil {
		t.Fatal("expected async compact command to return a completion message")
	}
	updated, cmd = next.Update(msg)
	next = updated.(model)
	if cmd != nil {
		t.Fatal("expected compact completion to be handled without starting another command")
	}
	if next.lastCompactResult == nil || !next.lastCompactResult.Compacted {
		t.Fatalf("expected session-backed compaction result, got %#v", next.lastCompactResult)
	}
	if len(next.sessionEvents) >= eventsBefore {
		t.Fatalf("expected in-memory context to shrink after compaction, before=%d after=%d", eventsBefore, len(next.sessionEvents))
	}
	if !transcriptContains(next.transcript, "Compacted earlier session context") {
		t.Fatalf("expected transcript to render compaction summary, got %#v", next.transcript)
	}
	prompt := next.sessionPrompt("next task")
	for _, dropped := range []string{"alpha old user intent", "beta old assistant answer", "gamma old tool result", "delta old follow-up"} {
		if strings.Contains(prompt, dropped) {
			t.Fatalf("compacted-away event %q reached the next prompt:\n%s", dropped, prompt)
		}
	}
	if !strings.Contains(prompt, "Compacted earlier session context") || !strings.Contains(prompt, "mu recent") {
		t.Fatalf("prompt missing compacted summary or preserved recent event:\n%s", prompt)
	}
}

func TestCompactCommandUsesProviderSummaryWhenAvailable(t *testing.T) {
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	provider := &fakeProvider{events: []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventText, Content: "Provider summary keeps the actual old decisions."},
		{Type: zeroruntime.StreamEventDone},
	}}
	m := newModel(context.Background(), Options{
		ModelName:    "gpt-4.1",
		Provider:     provider,
		SessionStore: store,
	})
	var err error
	m, err = m.ensureActiveSession("compact with provider")
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []sessions.AppendEventInput{
		{Type: sessions.EventToolCall, Payload: map[string]any{"id": "ask", "name": "ask_user", "arguments": `{"questions":[{"question":"Which database should remain?"}]}`}},
		{Type: sessions.EventToolResult, Payload: map[string]any{"toolCallId": "ask", "name": "ask_user", "status": "ok", "output": "Postgres only"}},
	} {
		m, err = m.appendSessionEvent(input.Type, input.Payload)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, content := range []string{
		"old decision A",
		"old decision B",
		"old file note",
		"old blocker",
		"recent one",
		"recent two",
		"recent three",
		"recent four",
		"recent five",
		"recent six",
		"recent seven",
		"recent eight",
	} {
		m, err = m.appendSessionEvent(sessions.EventMessage, map[string]any{"role": "user", "content": content})
		if err != nil {
			t.Fatal(err)
		}
		m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowUser, text: content})
	}
	m.input.SetValue("/compact")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd == nil {
		t.Fatal("expected /compact to start an async provider-backed compaction command")
	}
	msg := execCmd(cmd)
	if msg == nil {
		t.Fatal("expected async compact command to return a completion message")
	}
	updated, cmd = next.Update(msg)
	next = updated.(model)
	if cmd != nil {
		t.Fatal("expected compact completion to be handled without starting another command")
	}

	if len(provider.requests) != 1 {
		t.Fatalf("expected one provider summarization request, got %d", len(provider.requests))
	}
	request := provider.requests[0]
	if len(request.Messages) != 2 || request.Messages[0].Content != agent.CompactionSummaryInstructions {
		t.Fatalf("manual compaction did not use the shared agent compaction prompt: %#v", request.Messages)
	}
	for _, want := range []string{"Which database should remain?", "Postgres only"} {
		if !strings.Contains(request.Messages[1].Content, want) {
			t.Fatalf("manual compaction projection lost %q: %q", want, request.Messages[1].Content)
		}
	}
	if next.lastCompactResult == nil || next.lastCompactResult.Summary != "Provider summary keeps the actual old decisions." {
		t.Fatalf("expected provider summary result, got %#v", next.lastCompactResult)
	}
	prompt := next.sessionPrompt("continue")
	if !strings.Contains(prompt, "Provider summary keeps the actual old decisions.") {
		t.Fatalf("next prompt missing provider summary:\n%s", prompt)
	}
	if strings.Contains(prompt, "old decision A") {
		t.Fatalf("next prompt should not include raw compacted event:\n%s", prompt)
	}
}

func TestManualCompactionPersistsStructuralTruncationMetadata(t *testing.T) {
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	provider := &fakeProvider{events: []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventText, Content: "Bounded summary."},
		{Type: zeroruntime.StreamEventDone},
	}}
	m := newModel(context.Background(), Options{ModelName: "gpt-4.1", Provider: provider, SessionStore: store})
	var err error
	m, err = m.ensureActiveSession("compact a large session")
	if err != nil {
		t.Fatal(err)
	}
	for index := range 20 {
		content := strings.Repeat("contextword ", 256) + string(rune('a'+index))
		m, err = m.appendSessionEvent(sessions.EventMessage, map[string]any{"role": "user", "content": content})
		if err != nil {
			t.Fatal(err)
		}
		m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowUser, text: content})
	}

	next, result, err := m.compactActiveSession()
	if err != nil || !result.Compacted {
		t.Fatalf("compactActiveSession() = (%#v, %v)", result, err)
	}
	raw, err := store.ReadEvents(next.activeSession.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	latest := raw[len(raw)-1]
	if latest.Type != sessions.EventCompaction {
		t.Fatalf("latest event = %s, want compaction", latest.Type)
	}
	payload := sessionPayload(latest)
	if !payloadBool(payload, "truncated") {
		t.Fatalf("compaction did not persist structural truncation: %#v", payload)
	}
	request := provider.requests[0]
	wantPromptChars := len(agent.CompactionSummaryInstructions)
	for _, message := range request.Messages[1:] {
		wantPromptChars += len(message.Content)
		for _, call := range message.ToolCalls {
			wantPromptChars += len(call.Name) + len(call.Arguments)
		}
	}
	if promptChars, ok := payload["promptChars"].(float64); !ok || int(promptChars) != wantPromptChars {
		t.Fatalf("promptChars = %#v, want %d", payload["promptChars"], wantPromptChars)
	}
}

func TestCompactCommandRecordsRequestWhenNoCompactorIsAvailable(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.input.SetValue("/compact")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd != nil {
		t.Fatal("expected /compact to be handled without starting an agent run")
	}
	if next.compactRequests != 1 {
		t.Fatalf("expected one compact request, got %d", next.compactRequests)
	}
	for _, want := range []string{"Compact", "requested, awaiting manual compactor", "compactable: no", "manual compactor unavailable"} {
		if !transcriptContains(next.transcript, want) {
			t.Fatalf("expected compact transcript to contain %q, got %#v", want, next.transcript)
		}
	}
	if transcriptContains(next.transcript, "pending integration") || transcriptContains(next.transcript, "future compaction backend") || transcriptContains(next.transcript, "not wired") {
		t.Fatalf("compact transcript should avoid shell-only placeholder text, got %#v", next.transcript)
	}

	next.input.SetValue("/compact status")
	updated, cmd = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)

	if cmd != nil {
		t.Fatal("expected /compact status to be handled without starting an agent run")
	}
	if next.compactRequests != 1 {
		t.Fatalf("status should not add compact requests, got %d", next.compactRequests)
	}
	if got := next.transcript[len(next.transcript)-1].text; !strings.Contains(got, "status: info") {
		t.Fatalf("expected /compact status to render info status, got %q", got)
	}
}

type compactSessionFunc func(context.Context, CompactRequest) (CompactResult, error)

func (f compactSessionFunc) CompactSession(ctx context.Context, request CompactRequest) (CompactResult, error) {
	return f(ctx, request)
}

func compactStatusText(rows []transcriptRow) string {
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].id == compactStatusRowID {
			return rows[i].text
		}
	}
	return ""
}

func TestUsageEventsUpdateFooterAndContext(t *testing.T) {
	provider := &fakeProvider{events: []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventText, Content: "done"},
		{Type: zeroruntime.StreamEventUsage, Usage: zeroruntime.Usage{InputTokens: 100, CachedInputTokens: 25, OutputTokens: 20}},
		{Type: zeroruntime.StreamEventDone},
	}}
	m := newModel(context.Background(), Options{
		ModelName:    "gpt-4.1",
		Provider:     provider,
		Registry:     tools.NewRegistry(),
		SessionStore: testSessionStore(t),
	})
	m.input.SetValue("track usage")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd == nil {
		t.Fatal("expected prompt to start agent run")
	}
	updated, _ = next.Update(execCmd(cmd))
	next = updated.(model)

	usageText := next.usageSummaryText()
	for _, want := range []string{"120 tokens", "$"} {
		if !strings.Contains(usageText, want) {
			t.Fatalf("expected usage summary to contain %q, got %q", want, usageText)
		}
	}

	next.input.SetValue("/context")
	updated, cmd = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)

	if cmd != nil {
		t.Fatal("expected /context to be handled without starting an agent run")
	}
	for _, want := range []string{"usage      1 request, 120 tokens", "style      concise", "effort     auto", "compaction  idle"} {
		if !transcriptContains(next.transcript, want) {
			t.Fatalf("expected context transcript to contain %q, got %#v", want, next.transcript)
		}
	}
}

func TestUsageRuntimeMessageUpdatesFooterBeforeFinalResponse(t *testing.T) {
	provider := &fakeProvider{events: []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventUsage, Usage: zeroruntime.Usage{InputTokens: 10, OutputTokens: 5}},
		{Type: zeroruntime.StreamEventText, Content: "done"},
		{Type: zeroruntime.StreamEventDone},
	}}
	runtimeMessages := []tea.Msg{}
	m := newModel(context.Background(), Options{
		ModelName:    "gpt-4.1",
		Provider:     provider,
		Registry:     tools.NewRegistry(),
		SessionStore: testSessionStore(t),
		RuntimeMessageSink: func(msg tea.Msg) {
			runtimeMessages = append(runtimeMessages, msg)
		},
	})
	m.input.SetValue("track usage")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd == nil {
		t.Fatal("expected prompt to start agent run")
	}
	finalMsg := execCmd(cmd)

	liveUsageCount := 0
	for _, msg := range runtimeMessages {
		if usageMsg, ok := msg.(agentUsageMsg); ok {
			updated, _ = next.Update(usageMsg)
			next = updated.(model)
			liveUsageCount++
		}
	}
	if liveUsageCount == 0 {
		t.Fatalf("expected a live usage message, got %#v", runtimeMessages)
	}

	if !strings.Contains(next.usageSummaryText(), "15 tokens") {
		t.Fatalf("expected live usage before final response, got %q", next.usageSummaryText())
	}

	updated, _ = next.Update(finalMsg)
	next = updated.(model)
	summary := next.usageTracker.Summary()
	if summary.RecordCount != 1 || summary.TotalTokens != 15 {
		t.Fatalf("expected final response not to double count live usage, got records=%d tokens=%d", summary.RecordCount, summary.TotalTokens)
	}
}

func TestUsageEventsForwardExistingAgentCallback(t *testing.T) {
	provider := &fakeProvider{events: []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventUsage, Usage: zeroruntime.Usage{InputTokens: 10, OutputTokens: 5}},
		{Type: zeroruntime.StreamEventText, Content: "done"},
		{Type: zeroruntime.StreamEventDone},
	}}
	seen := []zeroruntime.Usage{}
	m := newModel(context.Background(), Options{
		ModelName:    "gpt-4.1",
		Provider:     provider,
		Registry:     tools.NewRegistry(),
		SessionStore: testSessionStore(t),
		AgentOptions: agent.Options{
			OnUsage: func(event agent.Usage) {
				seen = append(seen, event)
			},
		},
	})
	m.input.SetValue("track usage")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd == nil {
		t.Fatal("expected prompt to start agent run")
	}
	msg := execCmd(cmd)
	if len(seen) != 1 || seen[0].TotalTokens() != 15 {
		t.Fatalf("expected original usage callback to receive event, got %#v", seen)
	}
	updated, _ = next.Update(msg)
	next = updated.(model)

	if !strings.Contains(next.usageSummaryText(), "15 tokens") {
		t.Fatalf("expected usage to still be tracked, got %q", next.usageSummaryText())
	}
}

func TestUsageEventsForCustomModelUseTokenOnlyFallback(t *testing.T) {
	provider := &fakeProvider{events: []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventText, Content: "done"},
		{Type: zeroruntime.StreamEventUsage, Usage: zeroruntime.Usage{InputTokens: 100, OutputTokens: 20}},
		{Type: zeroruntime.StreamEventDone},
	}}
	m := newModel(context.Background(), Options{
		ModelName:    "custom-coder",
		Provider:     provider,
		Registry:     tools.NewRegistry(),
		SessionStore: testSessionStore(t),
	})
	m.input.SetValue("track usage")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd == nil {
		t.Fatal("expected prompt to start agent run")
	}
	updated, _ = next.Update(execCmd(cmd))
	next = updated.(model)

	usageText := next.usageSummaryText()
	for _, want := range []string{"1 request, 120 tokens", "cost unavailable"} {
		if !strings.Contains(usageText, want) {
			t.Fatalf("expected usage summary to contain %q, got %q", want, usageText)
		}
	}
	if transcriptContains(next.transcript, "usage:") {
		t.Fatalf("custom model usage should not append a transcript error, got %#v", next.transcript)
	}
}

func TestUnpricedUsageStatusUsesLatestEventNotCumulative(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "custom-coder"})

	var rows []transcriptRow
	m, rows = m.recordUsageEvent("custom-coder", zeroruntime.Usage{InputTokens: 100, OutputTokens: 20})
	if len(rows) != 0 {
		t.Fatalf("unpriced usage should not append transcript rows, got %#v", rows)
	}
	m, rows = m.recordUsageEvent("custom-coder", zeroruntime.Usage{InputTokens: 10, OutputTokens: 5})
	if len(rows) != 0 {
		t.Fatalf("unpriced usage should not append transcript rows, got %#v", rows)
	}

	if !strings.Contains(m.usageSummaryText(), "135 tokens") {
		t.Fatalf("expected cumulative usage summary to stay intact, got %q", m.usageSummaryText())
	}
	if got := m.usageStatusSegment(); !strings.Contains(got, "15 tok") || strings.Contains(got, "135") {
		t.Fatalf("expected status to show latest usage only, got %q", got)
	}
	if got := m.sidebarTokenText(); !strings.Contains(got, "15 tokens") || strings.Contains(got, "135") {
		t.Fatalf("expected sidebar to show latest usage only, got %q", got)
	}
}

func TestStatusLineShowsTokenFigureWithoutPersistentRail(t *testing.T) {
	m := sidebarTestModel()
	m, _ = m.recordUsageEvent("test-model", zeroruntime.Usage{InputTokens: 100, OutputTokens: 20})
	// Run details retains the full token count on demand.
	if got := m.sidebarTokenText(); !strings.Contains(got, "120 tokens") {
		t.Fatalf("sidebar token text = %q, want it to carry the 120-token figure", got)
	}
	// The status line is the always-visible token readout.
	status := plainRender(t, m.statusLine(m.width))
	if !strings.Contains(status, "120 tok") {
		t.Fatalf("status line = %q, want the token figure", status)
	}
}

func TestInvalidUsageEventsAppendTranscriptError(t *testing.T) {
	provider := &fakeProvider{events: []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventText, Content: "done"},
		{Type: zeroruntime.StreamEventUsage, Usage: zeroruntime.Usage{InputTokens: -1, OutputTokens: 20}},
		{Type: zeroruntime.StreamEventDone},
	}}
	m := newModel(context.Background(), Options{
		ModelName:    "gpt-4.1",
		Provider:     provider,
		Registry:     tools.NewRegistry(),
		SessionStore: testSessionStore(t),
	})
	m.input.SetValue("track usage")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd == nil {
		t.Fatal("expected prompt to start agent run")
	}
	updated, _ = next.Update(execCmd(cmd))
	next = updated.(model)

	if !transcriptContains(next.transcript, "usage: expected inputTokens to be non-negative") {
		t.Fatalf("expected invalid usage transcript error, got %#v", next.transcript)
	}
	if next.unpricedRequests != 0 || strings.Contains(next.usageSummaryText(), "cost unavailable") {
		t.Fatalf("invalid usage should not be counted as unpriced, requests=%d usage=%q", next.unpricedRequests, next.usageSummaryText())
	}
}

func TestStaleAgentUsageResponseIsIgnored(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4.1"})

	updated, _ := m.Update(agentResponseMsg{
		runID:        42,
		usageModelID: "gpt-4.1",
		usageEvents:  []zeroruntime.Usage{{InputTokens: 100, OutputTokens: 20}},
	})
	next := updated.(model)

	if strings.Contains(next.usageSummaryText(), "120 tokens") {
		t.Fatalf("stale usage response should be ignored, got %q", next.usageSummaryText())
	}
}

func TestModelSwitchClearsUnsupportedEffortPreference(t *testing.T) {
	nextProvider := &fakeProvider{}
	m := newModel(context.Background(), Options{
		ProviderName:    "openai",
		ModelName:       "gpt-4.1-mini",
		ReasoningEffort: modelregistry.ReasoningEffortHigh,
		Provider:        &fakeProvider{},
		ProviderProfile: openAITestProfile("gpt-4.1-mini"),
		NewProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			if profile.Model != "gpt-4.1" {
				t.Fatalf("expected provider rebuild for gpt-4.1, got %#v", profile)
			}
			return nextProvider, nil
		},
	})
	m.input.SetValue("/model gpt-4.1")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if next.pending {
		t.Fatal("expected /model to be handled without starting an agent run")
	}
	if next.reasoningEffort != "" {
		t.Fatalf("expected unsupported effort preference to reset, got %q", next.reasoningEffort)
	}
	if !strings.Contains(next.transientNotice.text, "effort auto") {
		t.Fatalf("expected model switch notice to mention effort reset, got %q", next.transientNotice.text)
	}
}

func TestModelSwitchRedirectsDeprecatedModelWithNotice(t *testing.T) {
	nextProvider := &fakeProvider{}
	m := newModel(context.Background(), Options{
		ProviderName:    "openai",
		ModelName:       "gpt-4.1",
		Provider:        &fakeProvider{},
		ProviderProfile: openAITestProfile("gpt-4.1"),
		NewProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			if profile.Model != "gpt-4.1" {
				t.Fatalf("expected deprecated model to redirect to gpt-4.1, got %#v", profile)
			}
			return nextProvider, nil
		},
	})
	m.input.SetValue("/model gpt-4-turbo")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd != nil {
		t.Fatal("expected /model to be handled without starting an agent run")
	}
	if next.modelName != "gpt-4.1" {
		t.Fatalf("expected active model to be gpt-4.1 after redirect, got %q", next.modelName)
	}
	if !transcriptContains(next.transcript, "deprecated") {
		t.Fatalf("expected deprecation notice in transcript, got %#v", next.transcript)
	}
	if !transcriptContains(next.transcript, "gpt-4.1 · openai") {
		t.Fatalf("expected switch to canonical fallback id, got %#v", next.transcript)
	}
}

func TestModelSwitchUnknownModelReportsError(t *testing.T) {
	m := newModel(context.Background(), Options{
		ProviderName:    "openai",
		ModelName:       "gpt-4.1",
		Provider:        &fakeProvider{},
		ProviderProfile: openAITestProfile("gpt-4.1"),
		NewProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			t.Fatal("provider should not be rebuilt for an unknown model")
			return nil, nil
		},
	})
	m.input.SetValue("/model totally-unknown-model")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if next.modelName != "gpt-4.1" {
		t.Fatalf("expected active model to stay gpt-4.1, got %q", next.modelName)
	}
	if !transcriptContains(next.transcript, "unknown Zero model") {
		t.Fatalf("expected unknown model error, got %#v", next.transcript)
	}
}

func openAITestProfile(modelID string) config.ProviderProfile {
	return config.ProviderProfile{
		Name:         "openai",
		ProviderKind: config.ProviderKindOpenAI,
		BaseURL:      config.OpenAIBaseURL,
		APIKey:       "sk-test",
		Model:        modelID,
	}
}

func anthropicTestProfile(modelID string) config.ProviderProfile {
	return config.ProviderProfile{
		Name:         "anthropic",
		ProviderKind: config.ProviderKindAnthropic,
		BaseURL:      config.AnthropicBaseURL,
		APIKey:       "sk-test",
		Model:        modelID,
	}
}
