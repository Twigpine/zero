package tui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/specmode"
	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

func TestSpecCommandCreatesDraftReview(t *testing.T) {
	store := testSessionStore(t)
	provider := &scriptedProvider{scripts: [][]zeroruntime.StreamEvent{
		submitSpecScript("call-1", "Review Flow", "# Goal\n\nAdd review flow."),
	}}
	m := newSpecModeTestModel(t.TempDir(), provider, store)
	m.input.SetValue("/spec add review flow")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd == nil {
		t.Fatal("expected /spec to start a draft run")
	}

	updated, _ = next.Update(execCmd(cmd))
	next = updated.(model)

	if next.pendingSpecReview == nil {
		t.Fatalf("expected pending spec review, got %#v", next)
	}
	if next.activeSession.SessionKind != sessions.SessionKindSpecDraft || next.activeSession.SpecStatus != sessions.SpecStatusDraft {
		t.Fatalf("unexpected active spec session: %#v", next.activeSession)
	}
	if !strings.Contains(next.pendingSpecReview.RelativePath, ".zero/specs/") {
		t.Fatalf("spec path not recorded: %#v", next.pendingSpecReview)
	}
	if !providerRequestIncludesTool(provider.requests[0], specmode.SubmitToolName) {
		t.Fatalf("submit_spec was not advertised: %#v", provider.requests[0].Tools)
	}
	if providerRequestIncludesTool(provider.requests[0], "write_file") {
		t.Fatalf("spec draft must not advertise write_file: %#v", provider.requests[0].Tools)
	}
}

func TestSpecApproveStartsImplementationSession(t *testing.T) {
	store := testSessionStore(t)
	provider := &scriptedProvider{scripts: [][]zeroruntime.StreamEvent{
		submitSpecScript("call-1", "Review Flow", "# Goal\n\nAdd review flow."),
		textScript("implemented from approved spec"),
	}}
	m := newSpecModeTestModel(t.TempDir(), provider, store)
	m.input.SetValue("/spec add review flow")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	updated, _ = next.Update(execCmd(cmd))
	next = updated.(model)
	review := next.pendingSpecReview
	if review == nil {
		t.Fatal("expected pending review before approval")
	}

	updated, cmd = next.Update(testKeyText("a"))
	next = updated.(model)
	if cmd == nil {
		t.Fatal("expected approval to start implementation run")
	}
	if next.pendingSpecReview != nil {
		t.Fatal("expected pending review to clear on approval")
	}
	if next.activeSession.SessionKind != sessions.SessionKindSpecImpl {
		t.Fatalf("expected active implementation session, got %#v", next.activeSession)
	}

	updated, _ = next.Update(execCmd(cmd))
	next = updated.(model)
	if !transcriptContains(next.transcript, "implemented from approved spec") {
		t.Fatalf("implementation answer missing from transcript: %#v", next.transcript)
	}
	draft, err := store.Get(review.DraftSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if draft == nil || draft.SpecStatus != sessions.SpecStatusApproved || draft.SpecImplSessionID != next.activeSession.SessionID {
		t.Fatalf("draft metadata not approved: %#v", draft)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("provider request count = %d, want 2", len(provider.requests))
	}
	last := provider.requests[1].Messages[len(provider.requests[1].Messages)-1]
	if !strings.Contains(last.Content, "Implement the following approved spec") || !strings.Contains(last.Content, "# Goal") {
		t.Fatalf("implementation prompt missing spec body: %#v", last)
	}
}

func TestSpecReviewBlocksShiftTabModeCycle(t *testing.T) {
	m := newModel(context.Background(), Options{PermissionMode: agent.PermissionModeAuto})
	m.pendingSpecReview = &pendingSpecReviewPrompt{SpecID: "spec", SpecFilePath: ".zero/specs/spec.md"}

	updated, _ := m.Update(testKeyShift(tea.KeyTab))
	next := updated.(model)

	if next.permissionMode != agent.PermissionModeAuto {
		t.Fatalf("expected permission mode unchanged during spec review, got %q", next.permissionMode)
	}
	if next.pendingSpecReview == nil {
		t.Fatal("expected spec review to remain pending")
	}
}

func TestSpecReviewCancelLaunchesQueuedPrompt(t *testing.T) {
	provider := &scriptedProvider{scripts: [][]zeroruntime.StreamEvent{{
		{Type: zeroruntime.StreamEventDone},
	}}}
	m := newSpecModeTestModel(t.TempDir(), provider, testSessionStore(t))
	m.pendingSpecReview = &pendingSpecReviewPrompt{SpecID: "spec", SpecFilePath: ".zero/specs/spec.md"}
	m.queuedMessage = "continue after cancel"

	updated, cmd := m.Update(testKey(tea.KeyEsc))
	next := updated.(model)

	if cmd == nil {
		t.Fatal("expected queued prompt to launch after spec review cancel")
	}
	if next.pendingSpecReview != nil {
		t.Fatal("expected spec review to clear")
	}
	if next.hasQueuedMessage() {
		t.Fatalf("expected queued prompt to be consumed, got %q", next.queuedMessage)
	}
	if !next.pending {
		t.Fatal("expected queued prompt launch to mark model pending")
	}
	beforeRequests := len(provider.requests)
	_ = execCmd(cmd)
	if len(provider.requests) <= beforeRequests {
		t.Fatalf("expected queued prompt to issue a provider request, before=%d after=%d", beforeRequests, len(provider.requests))
	}
	if !providerRequestsContain(provider.requests[beforeRequests:], "continue after cancel") {
		t.Fatalf("expected queued prompt in launched request, got %#v", provider.requests[beforeRequests:])
	}
}

func TestSpecReviewRejectLaunchesQueuedPrompt(t *testing.T) {
	store := testSessionStore(t)
	provider := &scriptedProvider{scripts: [][]zeroruntime.StreamEvent{
		submitSpecScript("call-1", "Review Flow", "# Goal\n\nAdd review flow."),
		textScript("queued after reject"),
	}}
	m := newSpecModeTestModel(t.TempDir(), provider, store)
	m.input.SetValue("/spec add review flow")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	updated, _ = next.Update(execCmd(cmd))
	next = updated.(model)
	if next.pendingSpecReview == nil {
		t.Fatal("expected pending review before rejection")
	}
	next.queuedMessage = "continue after reject"

	updated, cmd = next.Update(testKeyText("r"))
	next = updated.(model)

	if cmd == nil {
		t.Fatal("expected queued prompt to launch after spec review reject")
	}
	if next.pendingSpecReview != nil {
		t.Fatal("expected spec review to clear")
	}
	if next.hasQueuedMessage() {
		t.Fatalf("expected queued prompt to be consumed, got %q", next.queuedMessage)
	}
	if !next.pending {
		t.Fatal("expected queued prompt launch to mark model pending")
	}
	beforeRequests := len(provider.requests)
	_ = execCmd(cmd)
	if len(provider.requests) <= beforeRequests {
		t.Fatalf("expected queued reject follow-up to issue a provider request, before=%d after=%d", beforeRequests, len(provider.requests))
	}
	if !providerRequestsContain(provider.requests[beforeRequests:], "continue after reject") {
		t.Fatalf("expected queued prompt in launched request, got %#v", provider.requests[beforeRequests:])
	}
}

func newSpecModeTestModel(root string, provider zeroruntime.Provider, store *sessions.Store) model {
	registry := tools.NewRegistry()
	for _, tool := range tools.CoreToolsScoped(root, nil) {
		registry.Register(tool)
	}
	return newModel(context.Background(), Options{
		Cwd:            root,
		ProviderName:   "openai",
		ModelName:      "gpt-4.1",
		Provider:       provider,
		Registry:       registry,
		SessionStore:   store,
		PermissionMode: agent.PermissionModeAsk,
	})
}

func submitSpecScript(callID string, title string, plan string) []zeroruntime.StreamEvent {
	args, _ := json.Marshal(map[string]string{
		"title": title,
		"plan":  plan,
	})
	return []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: callID, ToolName: specmode.SubmitToolName},
		{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: callID, ArgumentsFragment: string(args)},
		{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: callID},
		{Type: zeroruntime.StreamEventDone},
	}
}

func providerRequestIncludesTool(request zeroruntime.CompletionRequest, name string) bool {
	for _, tool := range request.Tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

func providerRequestsContain(requests []zeroruntime.CompletionRequest, text string) bool {
	for _, request := range requests {
		for _, message := range request.Messages {
			if strings.Contains(message.Content, text) {
				return true
			}
		}
	}
	return false
}

// Regression (PR #254 review): spec-mode launches must seed turnStartedAt so the
// working status line's live elapsed clock renders on spec runs — previously only
// the normal prompt path set it. Both draft and impl now route through beginRun.
func TestSpecLaunchesSeedElapsedClock(t *testing.T) {
	store := testSessionStore(t)
	provider := &scriptedProvider{scripts: [][]zeroruntime.StreamEvent{
		submitSpecScript("call-1", "Review Flow", "# Goal\n\nAdd review flow."),
		textScript("implemented from approved spec"),
	}}
	m := newSpecModeTestModel(t.TempDir(), provider, store)
	m.input.SetValue("/spec add review flow")

	// Draft launch must seed the clock.
	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd == nil || !next.pending {
		t.Fatal("expected /spec to start a pending draft run")
	}
	if next.turnStartedAt.IsZero() {
		t.Fatal("draft launch did not seed turnStartedAt (elapsed clock would not render)")
	}

	updated, _ = next.Update(execCmd(cmd))
	next = updated.(model)
	if next.pendingSpecReview == nil {
		t.Fatal("expected pending spec review")
	}
	next.turnStartedAt = time.Time{} // zero it to prove the impl path re-seeds independently

	// Approval launches the implementation run, which must also seed the clock.
	updated, cmd = next.Update(testKeyText("a"))
	next = updated.(model)
	if cmd == nil || !next.pending {
		t.Fatal("expected approval to start a pending implementation run")
	}
	if next.turnStartedAt.IsZero() {
		t.Fatal("impl launch did not seed turnStartedAt (elapsed clock would not render)")
	}
}
