package acp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// `zero exec` persists every provider usage report as a session usage event, so
// `zero usage report --session <id>` can total a run. The ACP agent did not set
// OnUsage, which dropped the token counts of every ACP session. A turn with two
// provider requests (a tool round trip) must leave two usage events, in the shape
// the usage report reads, next to the turn's messages.
func TestACPPromptPersistsProviderUsage(t *testing.T) {
	deps := testDeps(t)
	deps.RunAgent = func(_ context.Context, _ string, _ zeroruntime.Provider, opts agent.Options) (agent.Result, error) {
		if opts.OnUsage == nil {
			t.Errorf("agent.Options.OnUsage is not wired: the ACP agent drops provider token usage")
			return agent.Result{FinalAnswer: "done"}, nil
		}
		opts.OnUsage(agent.Usage{InputTokens: 1200, OutputTokens: 34})
		opts.OnUsage(agent.Usage{InputTokens: 1300, OutputTokens: 21, CachedInputTokens: 900})
		return agent.Result{FinalAnswer: "done"}, nil
	}

	h := newHarness(t, deps)
	defer h.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var created NewSessionResult
	if err := h.client.Call(ctx, MethodSessionNew, NewSessionParams{Cwd: t.TempDir()}, &created); err != nil {
		t.Fatal(err)
	}
	var result PromptResult
	if err := h.client.Call(ctx, MethodSessionPrompt, PromptParams{
		SessionID: created.SessionID, Prompt: []ContentBlock{TextBlock("count my tokens")},
	}, &result); err != nil {
		t.Fatalf("session/prompt: %v", err)
	}

	events, err := deps.Store.ReadEvents(created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	type usagePayload struct {
		PromptTokens      int `json:"promptTokens"`
		CompletionTokens  int `json:"completionTokens"`
		TotalTokens       int `json:"totalTokens"`
		CachedInputTokens int `json:"cachedInputTokens"`
	}
	var got []usagePayload
	messages := 0
	for _, event := range events {
		switch event.Type {
		case sessions.EventUsage:
			var payload usagePayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatalf("usage payload %s: %v", event.Payload, err)
			}
			assertUsagePayloadIsTokenCountsOnly(t, event.Payload)
			got = append(got, payload)
		case sessions.EventMessage:
			messages++
		}
	}
	want := []usagePayload{
		{PromptTokens: 1200, CompletionTokens: 34, TotalTokens: 1234},
		{PromptTokens: 1300, CompletionTokens: 21, TotalTokens: 1321, CachedInputTokens: 900},
	}
	if len(got) != len(want) {
		t.Fatalf("usage events = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("usage event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if messages != 2 {
		t.Fatalf("message events = %d, want the user and assistant messages of the turn", messages)
	}
}

// Usage joins the turn's buffered batch, so a hard run failure that aborts the
// turn leaves nothing durable, exactly as it does for the turn's messages.
func TestACPHardTurnFailureDropsItsUsageWithTheTurn(t *testing.T) {
	deps := testDeps(t)
	deps.RunAgent = func(_ context.Context, _ string, _ zeroruntime.Provider, opts agent.Options) (agent.Result, error) {
		if opts.OnUsage == nil {
			t.Errorf("agent.Options.OnUsage is not wired: the ACP agent drops provider token usage")
			return agent.Result{}, errors.New("injected hard run failure")
		}
		opts.OnUsage(agent.Usage{InputTokens: 500, OutputTokens: 5})
		return agent.Result{}, errors.New("injected hard run failure")
	}

	h := newHarness(t, deps)
	defer h.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var created NewSessionResult
	if err := h.client.Call(ctx, MethodSessionNew, NewSessionParams{Cwd: t.TempDir()}, &created); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Call(ctx, MethodSessionPrompt, PromptParams{
		SessionID: created.SessionID, Prompt: []ContentBlock{TextBlock("failing turn")},
	}, &PromptResult{}); err == nil {
		t.Fatal("hard RunAgent failure was reported as a successful turn")
	}
	events, err := deps.Store.ReadEvents(created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("aborted turn left durable events: %+v", events)
	}
}

// StopCancelled is a successful ACP outcome: the turn commits. Tokens the
// provider already reported were spent, so they stay in the session log even
// though the client cancelled. A hard error (the test above) is the path that
// drops them.
func TestACPCancelledTurnKeepsReportedUsage(t *testing.T) {
	deps := testDeps(t)
	deps.RunAgent = func(_ context.Context, _ string, _ zeroruntime.Provider, opts agent.Options) (agent.Result, error) {
		if opts.OnUsage == nil {
			t.Errorf("agent.Options.OnUsage is not wired: the ACP agent drops provider token usage")
			return agent.Result{}, context.Canceled
		}
		opts.OnUsage(agent.Usage{InputTokens: 80, OutputTokens: 3})
		return agent.Result{FinalAnswer: "partial"}, context.Canceled
	}

	h := newHarness(t, deps)
	defer h.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var created NewSessionResult
	if err := h.client.Call(ctx, MethodSessionNew, NewSessionParams{Cwd: t.TempDir()}, &created); err != nil {
		t.Fatal(err)
	}
	var result PromptResult
	if err := h.client.Call(ctx, MethodSessionPrompt, PromptParams{
		SessionID: created.SessionID, Prompt: []ContentBlock{TextBlock("cancelled turn")},
	}, &result); err != nil {
		t.Fatalf("cancelled turn returned an RPC error: %v", err)
	}
	if result.StopReason != StopCancelled {
		t.Fatalf("stop reason = %q, want %q", result.StopReason, StopCancelled)
	}
	events, err := deps.Store.ReadEvents(created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	usageEvents := 0
	for _, event := range events {
		if event.Type != sessions.EventUsage {
			continue
		}
		usageEvents++
		assertUsagePayloadIsTokenCountsOnly(t, event.Payload)
		var payload struct {
			PromptTokens     int `json:"promptTokens"`
			CompletionTokens int `json:"completionTokens"`
			TotalTokens      int `json:"totalTokens"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.PromptTokens != 80 || payload.CompletionTokens != 3 || payload.TotalTokens != 83 {
			t.Fatalf("cancelled-turn usage = %+v, want 80/3/83", payload)
		}
	}
	if usageEvents != 1 {
		t.Fatalf("usage events = %d, want the one report from the cancelled turn", usageEvents)
	}
}

// Usage events sit in the same log session/load and session/resume read, but
// history reconstruction skips them. The wire replay stays the conversation,
// and the next turn's prompt stays the conversation: token counts never become
// context and never cross the ACP wire.
func TestACPLoadAndResumeIgnoreUsageEvents(t *testing.T) {
	const userLine = "unique-acp-user-line"
	const answer = "unique-acp-answer"
	deps := testDeps(t)
	deps.RunAgent = func(_ context.Context, _ string, _ zeroruntime.Provider, opts agent.Options) (agent.Result, error) {
		if opts.OnUsage == nil {
			t.Errorf("agent.Options.OnUsage is not wired: the ACP agent drops provider token usage")
			return agent.Result{FinalAnswer: answer}, nil
		}
		opts.OnUsage(agent.Usage{InputTokens: 1200, OutputTokens: 34})
		return agent.Result{FinalAnswer: answer}, nil
	}

	workspace := t.TempDir()
	h := newHarness(t, deps)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var created NewSessionResult
	if err := h.client.Call(ctx, MethodSessionNew, NewSessionParams{Cwd: workspace}, &created); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Call(ctx, MethodSessionPrompt, PromptParams{
		SessionID: created.SessionID, Prompt: []ContentBlock{TextBlock(userLine)},
	}, &PromptResult{}); err != nil {
		t.Fatalf("session/prompt: %v", err)
	}
	h.stop()

	events, err := deps.Store.ReadEvents(created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	sawUsage := false
	for _, event := range events {
		if event.Type == sessions.EventUsage {
			sawUsage = true
			assertUsagePayloadIsTokenCountsOnly(t, event.Payload)
		}
	}
	if !sawUsage {
		t.Fatal("session log has no usage event to ignore")
	}

	loader := newHarness(t, deps)
	if err := loader.client.Call(ctx, MethodSessionLoad, LoadSessionParams{
		SessionID: created.SessionID, Cwd: workspace, McpServers: []McpServer{},
	}, &LoadSessionResult{}); err != nil {
		t.Fatalf("session/load: %v", err)
	}
	var replayed []string
	for {
		select {
		case update := <-loader.notifications:
			replayed = append(replayed, update.Content.Text)
		case <-time.After(150 * time.Millisecond):
			goto drained
		}
	}
drained:
	loader.stop()
	if len(replayed) != 2 || replayed[0] != userLine || replayed[1] != answer {
		t.Fatalf("session/load replay = %#v, want the user line and the answer only", replayed)
	}
	for _, text := range replayed {
		if strings.Contains(text, "promptTokens") || strings.Contains(text, "1200") {
			t.Fatalf("session/load replayed usage onto the wire: %q", text)
		}
	}

	var gotPrompt string
	deps.RunAgent = func(_ context.Context, prompt string, _ zeroruntime.Provider, _ agent.Options) (agent.Result, error) {
		gotPrompt = prompt
		return agent.Result{FinalAnswer: "continued"}, nil
	}
	resumer := newHarness(t, deps)
	defer resumer.stop()
	if err := resumer.client.Call(ctx, MethodSessionResume, ResumeSessionParams{
		SessionID: created.SessionID, Cwd: workspace, McpServers: []McpServer{},
	}, &ResumeSessionResult{}); err != nil {
		t.Fatalf("session/resume: %v", err)
	}
	if err := resumer.client.Call(ctx, MethodSessionPrompt, PromptParams{
		SessionID: created.SessionID, Prompt: []ContentBlock{TextBlock("carry on")},
	}, &PromptResult{}); err != nil {
		t.Fatalf("resumed session/prompt: %v", err)
	}
	if !strings.Contains(gotPrompt, userLine) || !strings.Contains(gotPrompt, answer) {
		t.Fatalf("resumed prompt = %q, want the prior conversation", gotPrompt)
	}
	if strings.Contains(gotPrompt, "promptTokens") || strings.Contains(gotPrompt, "1200") {
		t.Fatalf("resumed prompt contains persisted usage: %q", gotPrompt)
	}
}

// usagePayloadAllowedKeys is the non-escalating EventUsagePayload shape. ACP
// has no model escalation, and the writer must not grow a prompt, tool
// argument, or other raw field.
var usagePayloadAllowedKeys = map[string]bool{
	"promptTokens":      true,
	"completionTokens":  true,
	"totalTokens":       true,
	"cachedInputTokens": true,
	"cacheWriteTokens":  true,
	"reasoningTokens":   true,
}

func assertUsagePayloadIsTokenCountsOnly(t *testing.T, payload json.RawMessage) {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatalf("usage payload %s: %v", payload, err)
	}
	if len(raw) == 0 {
		t.Fatalf("usage payload is empty: %s", payload)
	}
	for key := range raw {
		if !usagePayloadAllowedKeys[key] {
			t.Fatalf("usage payload key %q is not a token count: %s", key, payload)
		}
	}
}
