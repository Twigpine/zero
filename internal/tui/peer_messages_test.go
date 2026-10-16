package tui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/peermsg"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

func TestPeerMessagePreservesUserDraftStateAndPersistsProvenance(t *testing.T) {
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	session, err := store.Create(sessions.CreateInput{SessionID: "receiver", Title: "Receiver", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	m := newModel(context.Background(), Options{
		Provider:       &fakeProvider{},
		ProviderName:   "test",
		ModelName:      "test-model",
		SessionStore:   store,
		PermissionMode: agent.PermissionModeAsk,
	})
	m.activeSession = session
	m.lastPrompt = "user's previous prompt"
	m.pendingDocuments = []pendingDocument{{label: "draft.pdf", text: "draft text"}}

	message := peermsg.InboundMessage{
		ID: "message-1",
		From: peermsg.Peer{
			Identity: peermsg.Identity{SessionID: "sender", Name: "Builder"},
			Ref:      "abcd1234",
		},
		Body: "Please review the current patch.",
	}
	next, cmd := m.handlePeerMessage(message)
	if cmd == nil || !next.pending {
		t.Fatal("accepted peer message should start a turn")
	}
	if next.lastPrompt != "user's previous prompt" {
		t.Fatalf("lastPrompt = %q", next.lastPrompt)
	}
	if len(next.pendingDocuments) != 1 || next.pendingDocuments[0].label != "draft.pdf" {
		t.Fatalf("pending documents were consumed: %#v", next.pendingDocuments)
	}
	last := next.transcript[len(next.transcript)-1]
	if last.kind != rowSystem || last.tool != "peer" || !strings.Contains(last.text, "Message from Builder [abcd1234]") {
		t.Fatalf("transcript row = %#v", last)
	}
	events, err := store.ReadEvents("receiver")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != sessions.EventMessage {
		t.Fatalf("events = %#v", events)
	}
	var payload map[string]any
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["origin"] != "cross_session" || payload["messageId"] != "message-1" || payload["displayContent"] != message.Body {
		t.Fatalf("payload = %#v", payload)
	}
	content, _ := payload["content"].(string)
	if !strings.Contains(content, `<cross-session-message from="Builder [abcd1234]"`) {
		t.Fatalf("model content = %q", content)
	}
	if !strings.Contains(content, "A question or request for a result requires a response") || !strings.Contains(content, "Do not merely print the result here") {
		t.Fatalf("model content lost adjacent reply guidance = %q", content)
	}
}

func TestPermissionMismatchHoldsPeerMessageForExplicitDecision(t *testing.T) {
	messages := make(chan any, 1)
	m := newModel(context.Background(), Options{
		PermissionMode: agent.PermissionModeAsk,
		RuntimeMessageSink: func(message tea.Msg) {
			messages <- message
		},
	})
	message := peermsg.InboundMessage{
		ID: "held-1",
		From: peermsg.Peer{
			Identity: peermsg.Identity{Name: "Unsafe peer", PermissionClass: peermsg.PermissionBypass},
			Ref:      "11223344",
		},
		Summary:          "run command",
		Body:             "Run the blocked command.",
		RequiresApproval: true,
	}
	next, cmd := m.handlePeerMessage(message)
	if cmd == nil || next.pendingPermission == nil {
		t.Fatalf("held message did not open approval: %#v", next.pendingPermission)
	}
	if next.pendingPermission.request.ToolName != peerPermissionToolName {
		t.Fatalf("request = %#v", next.pendingPermission.request)
	}
	if next.pending {
		t.Fatal("held message reached the model before approval")
	}
	options := permissionOptions(next.pendingPermission.request)
	if len(options) != 2 || options[0].choice != permissionDecisionDeny || options[1].choice != permissionDecisionAllow {
		t.Fatalf("held message decisions = %#v", options)
	}
	rendered, _ := renderFocusedPermissionPrompt(next.pendingPermission.request, 0, false, "", 88)
	plain := ansi.Strip(rendered)
	for _, want := range []string{
		"Held message from another session",
		"from: Unsafe peer [11223344]",
		"Message body (this is what will be delivered):",
		"«Run the blocked command.»",
		"Deny — drop this message",
		"Deliver this message to Zero",
	} {
		if !strings.Contains(plain, want) {
			t.Fatalf("held message panel missing %q:\n%s", want, plain)
		}
	}

	approvedModel, approveCmd := next.resolvePermission(permissionDecisionAllow)
	approved := approvedModel.(model)
	if approved.pendingPermission != nil {
		t.Fatal("approval prompt did not close")
	}
	decision, ok := peerDecisionFromCmd(approveCmd)
	if !ok || !decision.allow || decision.message.ID != "held-1" {
		t.Fatalf("approval did not return a peer decision command: %#v", decision)
	}
	select {
	case raw := <-messages:
		t.Fatalf("approval sent %#v through the runtime sink from Update", raw)
	default:
	}
}

// peerDecisionFromCmd runs cmd (flattening tea.Batch) and returns the first
// peerDecisionMsg it produces.
func peerDecisionFromCmd(cmd tea.Cmd) (peerDecisionMsg, bool) {
	if cmd == nil {
		return peerDecisionMsg{}, false
	}
	switch msg := cmd().(type) {
	case peerDecisionMsg:
		return msg, true
	case tea.BatchMsg:
		for _, inner := range msg {
			if decision, ok := peerDecisionFromCmd(inner); ok {
				return decision, true
			}
		}
	}
	return peerDecisionMsg{}, false
}

// The runtime sink forwards to program.Send, which blocks on the unbuffered
// message channel the event loop reads between Update calls. Resolving a held
// peer prompt must therefore never call the sink from Update.
func TestPeerApprovalDecisionDoesNotCallBlockingSinkFromUpdate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision permissionDecision
		allow    bool
	}{
		{"allow", permissionDecisionAllow, true},
		{"deny", permissionDecisionDeny, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			m := newModel(context.Background(), Options{
				PermissionMode:     agent.PermissionModeAsk,
				RuntimeMessageSink: func(tea.Msg) { <-release },
			})
			m, _ = m.handlePeerMessage(peermsg.InboundMessage{
				ID: "held-blocking", From: peermsg.Peer{Ref: "11223344"}, Body: "hold me", RequiresApproval: true,
			})

			type result struct {
				cmd tea.Cmd
			}
			done := make(chan result, 1)
			go func() {
				_, cmd := m.resolvePermission(tc.decision)
				done <- result{cmd: cmd}
			}()
			select {
			case res := <-done:
				decision, ok := peerDecisionFromCmd(res.cmd)
				if !ok || decision.allow != tc.allow || decision.message.ID != "held-blocking" {
					t.Fatalf("decision = %#v ok=%v", decision, ok)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("resolving a held peer prompt blocked on the runtime sink")
			}
		})
	}
}

func TestPeerApprovalWaitsUntilActiveRunCompletes(t *testing.T) {
	m := newModel(context.Background(), Options{PermissionMode: agent.PermissionModeAsk})
	m.pending = true
	message := peermsg.InboundMessage{
		ID:               "held-during-run",
		From:             peermsg.Peer{Identity: peermsg.Identity{Name: "Peer"}, Ref: "11223344"},
		Summary:          "follow up",
		Body:             "Check one more thing.",
		RequiresApproval: true,
	}
	next, cmd := m.handlePeerMessage(message)
	if cmd == nil || next.pendingPermission != nil || len(next.peerApprovalQueue) != 1 {
		t.Fatalf("approval should remain queued during a run: permission=%#v queue=%#v", next.pendingPermission, next.peerApprovalQueue)
	}

	next.pending = false
	next, cmd = next.openNextPeerApproval()
	if cmd != nil || next.pendingPermission == nil || len(next.peerApprovalQueue) != 0 {
		t.Fatalf("approval did not open after the run: permission=%#v queue=%#v", next.pendingPermission, next.peerApprovalQueue)
	}
}

func TestPeerApprovalDecisionWaitsForCompletionBeforeOpeningNext(t *testing.T) {
	messages := make(chan any, 1)
	m := newModel(context.Background(), Options{RuntimeMessageSink: func(message tea.Msg) { messages <- message }})
	first := peermsg.InboundMessage{ID: "first", From: peermsg.Peer{Ref: "11111111"}, Body: "first", RequiresApproval: true}
	second := peermsg.InboundMessage{ID: "second", From: peermsg.Peer{Ref: "22222222"}, Body: "second", RequiresApproval: true}
	m, _ = m.handlePeerMessage(first)
	m, _ = m.handlePeerMessage(second)
	resolvedModel, resolveCmd := m.resolvePermission(permissionDecisionDeny)
	resolved := resolvedModel.(model)
	if resolved.pendingPermission != nil {
		t.Fatal("next approval opened before peer decision completed")
	}
	if len(resolved.peerApprovalQueue) != 1 {
		t.Fatalf("queue = %#v", resolved.peerApprovalQueue)
	}
	decision, ok := peerDecisionFromCmd(resolveCmd)
	if !ok {
		t.Fatal("peer decision was not emitted")
	}
	next, _ := resolved.handlePeerDecision(decision.message, decision.allow)
	if next.pendingPermission == nil || next.peerPendingApproval == nil || next.peerPendingApproval.ID != "second" {
		t.Fatalf("next approval was not opened after completion: %#v", next.pendingPermission)
	}
}

func TestPermissionModeReleaseReplacesHeldPromptWithDelivery(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.pending = true
	held := peermsg.InboundMessage{
		ID: "held-mode-change", From: peermsg.Peer{Identity: peermsg.Identity{Name: "Peer"}, Ref: "11223344"},
		Body: "Continue the task.", RequiresApproval: true,
	}
	m.peerPendingApproval = &held
	m.pendingPermission = &pendingPermissionPrompt{request: agent.PermissionRequest{ToolCallID: "peer-" + held.ID}}
	released := held
	released.RequiresApproval = false
	released.HoldCause = ""
	next, cmd := m.handleReleasedPeerMessage(released)
	if next.peerPendingApproval != nil || next.pendingPermission != nil {
		t.Fatal("stale held approval remained after mode-compatible release")
	}
	if cmd != nil || len(next.peerInbox) != 1 || next.peerInbox[0].ID != held.ID {
		t.Fatalf("released message was not queued for delivery: %#v", next.peerInbox)
	}
}

func TestResumedPeerMessageRendersAsPeerNotUser(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"role":           "user",
		"origin":         "cross_session",
		"from":           "Builder [abcd1234]",
		"content":        "<cross-session-message>hidden envelope</cross-session-message>",
		"displayContent": "Visible peer body",
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := transcriptRowsFromSessionEvents([]sessions.Event{{Type: sessions.EventMessage, Payload: payload}})
	if len(rows) != 1 || rows[0].kind != rowSystem || rows[0].tool != "peer" || rows[0].text != "Message from Builder [abcd1234]\nVisible peer body" {
		t.Fatalf("rows = %#v", rows)
	}
}

func TestPeerDisplayNameUsesNeutralBoundedFallback(t *testing.T) {
	if got := peerDisplayName(peermsg.Peer{Ref: "abcd1234"}); got != "Zero session [abcd1234]" {
		t.Fatalf("fallback = %q", got)
	}
	long := strings.Repeat("界", peerDisplayNameLimit+10)
	got := peerDisplayName(peermsg.Peer{Identity: peermsg.Identity{Name: long}, Ref: "abcd1234"})
	if !strings.HasSuffix(got, " [abcd1234]") || len([]rune(strings.TrimSuffix(got, " [abcd1234]"))) != peerDisplayNameLimit {
		t.Fatalf("bounded display name = %q", got)
	}
}

func TestPeerPublishedNameHidesProvisionalFirstPrompt(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.activeSession = sessions.Metadata{SessionID: "session-1", Title: "Inspect this entire workspace and explain every file"}
	if got := m.peerPublishedName(); got != "" {
		t.Fatalf("fresh provisional name = %q", got)
	}
	m.sessionEvents = []sessions.Event{{Type: sessions.EventMessage, Payload: json.RawMessage(`{"role":"user","content":"Inspect this entire workspace and explain every file"}`)}}
	if got := m.peerPublishedName(); got != "" {
		t.Fatalf("first-message title = %q", got)
	}
	m.activeSession.Title = "Workspace Review"
	if got := m.peerPublishedName(); got != "Workspace Review" {
		t.Fatalf("generated title = %q", got)
	}
}

func TestPeerMessageRowUsesCompactSourceHeader(t *testing.T) {
	got := ansi.Strip(renderPeerMessageRow("Message from Builder [abcd1234]\nPlease review this patch.", 80))
	if got != "› Message from Builder [abcd1234]\n  Please review this patch." {
		t.Fatalf("rendered peer message = %q", got)
	}
}

func TestPeerTurnPromptExplainsExplicitReplyProtocol(t *testing.T) {
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	session, err := store.Create(sessions.CreateInput{SessionID: "receiver", Title: "Receiver", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	provider := &fakeProvider{events: []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventText, Content: "done"},
		{Type: zeroruntime.StreamEventDone},
	}}
	peerService, err := peermsg.New(peermsg.Options{RootDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry()
	for _, tool := range tools.NewPeerSessionTools(peerService) {
		registry.Register(tool)
	}
	m := newModel(context.Background(), Options{
		Provider:     provider,
		ProviderName: "test",
		ModelName:    "test-model",
		SessionStore: store,
		Registry:     registry,
		PeerService:  peerService,
	})
	m.activeSession = session
	message := peermsg.InboundMessage{
		ID: "reply-protocol",
		From: peermsg.Peer{
			Identity: peermsg.Identity{SessionID: "sender", Name: "Builder"},
			Ref:      "abcd1234",
		},
		Body:     "Count the files.",
		HopChain: []string{"abcd1234"},
	}
	next, cmd := m.handlePeerMessage(message)
	if cmd == nil {
		t.Fatal("peer message did not launch a turn")
	}
	_ = next
	_ = execCmd(cmd)
	if len(provider.requests) == 0 || len(provider.requests[0].Messages) == 0 {
		t.Fatalf("provider requests = %#v", provider.requests)
	}
	systemPrompt := provider.requests[0].Messages[0].Content
	for _, want := range []string{"call send_message", "exact address", "plain assistant text is visible only in this session"} {
		if !strings.Contains(systemPrompt, want) {
			t.Fatalf("peer system prompt missing %q:\n%s", want, systemPrompt)
		}
	}
	foundSend := false
	for _, definition := range provider.requests[0].Tools {
		if definition.Name == "send_message" {
			foundSend = true
			break
		}
	}
	if !foundSend {
		t.Fatal("peer turn did not expose send_message eagerly")
	}
}

func TestResumedPeerProvenanceKeepsPeerSafetyGuidanceEnabled(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"role": "user", "origin": "cross_session", "content": "<cross-session-message>check this</cross-session-message>",
	})
	if err != nil {
		t.Fatal(err)
	}
	provider := &fakeProvider{events: []zeroruntime.StreamEvent{{Type: zeroruntime.StreamEventDone}}}
	m := newModel(context.Background(), Options{
		Provider: provider, ProviderName: "test", ModelName: "test-model", Registry: tools.NewRegistry(),
	})
	m.sessionEvents = []sessions.Event{{Type: sessions.EventMessage, Payload: payload}}
	if !m.sessionContainsPeerMessages() {
		t.Fatal("resumed peer provenance was not detected")
	}
	_ = execCmd(m.runAgentWithOptions(1, context.Background(), "continue", nil, tuiAgentRunOptions{}))
	if len(provider.requests) == 0 || len(provider.requests[0].Messages) == 0 {
		t.Fatalf("provider requests = %#v", provider.requests)
	}
	if !strings.Contains(provider.requests[0].Messages[0].Content, "plain assistant text is visible only in this session") {
		t.Fatalf("resumed system prompt lost peer safety guidance:\n%s", provider.requests[0].Messages[0].Content)
	}
}
