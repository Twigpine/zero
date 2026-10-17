package acp

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

const persistedNotice = "least-privilege notice: read access was narrowed"

func mustRawPayload(t *testing.T, payload map[string]any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func toolUpdateText(update ToolCallUpdate) string {
	var text strings.Builder
	for _, part := range update.Content {
		if part.Content != nil {
			text.WriteString(part.Content.Text)
		}
	}
	return text.String()
}

// nextToolResultUpdate returns the next tool_call_update, skipping the
// tool_call that announces it.
func nextToolResultUpdate(t *testing.T, ctx context.Context, updates <-chan ToolCallUpdate, stage string) ToolCallUpdate {
	t.Helper()
	for {
		select {
		case update := <-updates:
			if update.SessionUpdate == UpdateToolCallUpdate {
				return update
			}
		case <-ctx.Done():
			t.Fatalf("%s: the tool result update never arrived", stage)
		}
	}
}

// A DISCLOSURE SHOWN LIVE HAS TO SURVIVE BEING WRITTEN DOWN.
//
// enforcement_notice_test.go pins the live translation: toolResultContent reads
// ModelOutput, so a connected ACP client sees the sandbox's narrowing. That was
// the only half fixed. The turn also persists each result, and the persisted
// payload was spelled by hand in this package with result.Output, which after
// the output/notice split is the undecorated text, and with no notices field at
// all. The disclosure was gone from disk, so session/load replayed a sandboxed
// command as if nothing had constrained it.
//
// This runs a real turn, so the result reaches the store through the same
// OnToolResult callback production uses, then loads the session in a fresh
// agent and reads the replayed update. It fails if only translate.go is fixed,
// which is the state it was written against. Reported by @jatmn.
func TestACPReloadedToolResultCarriesTheEnforcementNoticeOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
	}{
		{name: "a command with output", output: "the command output"},
		// An enforced command that printed nothing still has a disclosure, and
		// an empty body is where a reader that falls back to the decorated text
		// draws it twice.
		{name: "a command that printed nothing", output: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := testDeps(t)
			deps.RunAgent = func(_ context.Context, _ string, _ zeroruntime.Provider, opts agent.Options) (agent.Result, error) {
				call := agent.ToolCall{ID: "call-sandboxed", Name: "bash", Arguments: `{"command":"ls"}`}
				opts.OnToolCall(call)
				opts.OnToolResult(agent.ToolResult{
					ToolCallID:         call.ID,
					Name:               call.Name,
					Status:             tools.StatusOK,
					Output:             tc.output,
					EnforcementNotices: []string{persistedNotice},
				})
				return agent.Result{FinalAnswer: "done"}, nil
			}
			workspace := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			live := newHarness(t, deps)
			var created NewSessionResult
			if err := live.client.Call(ctx, MethodSessionNew, NewSessionParams{Cwd: workspace, McpServers: []McpServer{}}, &created); err != nil {
				t.Fatalf("session/new: %v", err)
			}
			if err := live.client.Call(ctx, MethodSessionPrompt, PromptParams{SessionID: created.SessionID, Prompt: []ContentBlock{TextBlock("list the files")}}, &PromptResult{}); err != nil {
				t.Fatalf("session/prompt: %v", err)
			}
			liveText := toolUpdateText(nextToolResultUpdate(t, ctx, live.tools, "live"))
			if count := strings.Count(liveText, persistedNotice); count != 1 {
				t.Fatalf("premise: the live update carries the notice %d times, want 1:\n%s", count, liveText)
			}
			live.stop()

			loader := newHarness(t, deps)
			defer loader.stop()
			if err := loader.client.Call(ctx, MethodSessionLoad, LoadSessionParams{SessionID: created.SessionID, Cwd: workspace, McpServers: []McpServer{}}, &LoadSessionResult{}); err != nil {
				t.Fatalf("session/load: %v", err)
			}
			replayed := toolUpdateText(nextToolResultUpdate(t, ctx, loader.tools, "replay"))
			if count := strings.Count(replayed, persistedNotice); count != 1 {
				t.Errorf("the replayed result carries the notice %d times, want exactly 1:\n%s", count, replayed)
			}
			if replayed != liveText {
				t.Errorf("the replayed result does not match what the client saw live\nlive:     %q\nreplayed: %q", liveText, replayed)
			}
		})
	}
}

// THE ACP WRITER IS THE SHARED CONTRACT, NOT A COPY OF IT. The session store is
// the one the TUI and headless exec also write to and resume from, so a payload
// spelled here by hand is a third representation waiting to drift. It already
// had: it was the only writer with no notices and no undecorated body.
func TestToolResultEventIsTheSharedSessionPayload(t *testing.T) {
	result := agent.ToolResult{
		ToolCallID:         "call-1",
		Name:               "bash",
		Status:             tools.StatusOK,
		Output:             "the command output",
		EnforcementNotices: []string{persistedNotice},
		ChangedFiles:       []string{"a.go"},
	}
	event := toolResultEvent(result)
	if event.Type != sessions.EventToolResult {
		t.Fatalf("event type = %q", event.Type)
	}
	if want := agent.ToolResultSessionPayload(result); !reflect.DeepEqual(event.Payload, want) {
		t.Fatalf("the ACP payload is not the shared one\n got: %#v\nwant: %#v", event.Payload, want)
	}
	payload, ok := event.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload is %T", event.Payload)
	}
	if notices, _ := payload["enforcementNotices"].([]string); len(notices) != 1 || notices[0] != persistedNotice {
		t.Errorf("typed notices were not persisted: %#v", payload["enforcementNotices"])
	}
	if body, present := payload["displayPreview"]; !present || body != "the command output" {
		t.Errorf("the undecorated body was not persisted: %#v (present=%v)", body, present)
	}
}

// A record can reach the reader in either shape, and both have to come out as
// one disclosure: output with the notices already composed in, which is what
// every current writer stores, and output left undecorated beside the typed
// field, which a reader must not answer by dropping the notice.
func TestReplayedToolResultComposesTheNoticeOnceForEitherStoredShape(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
		want   string
	}{
		{name: "output stored decorated", output: persistedNotice + "\n\nthe command output", want: persistedNotice + "\n\nthe command output"},
		{name: "output stored undecorated", output: "the command output", want: persistedNotice + "\n\nthe command output"},
		{name: "decorated and otherwise empty", output: persistedNotice, want: persistedNotice},
		{name: "undecorated and empty", output: "", want: persistedNotice},
	} {
		t.Run(tc.name, func(t *testing.T) {
			update := replayToolUpdate(sessions.Event{Type: sessions.EventToolResult, Payload: mustRawPayload(t, map[string]any{
				"toolCallId": "call-1", "name": "bash", "status": "ok",
				"output": tc.output, "enforcementNotices": []string{persistedNotice},
			})})
			if update == nil {
				t.Fatal("the stored result was not replayed")
			}
			if got := toolUpdateText(*update); got != tc.want {
				t.Errorf("replayed text = %q, want %q", got, tc.want)
			}
		})
	}

	// And a stored result with no notices is replayed as it was written.
	plain := replayToolUpdate(sessions.Event{Type: sessions.EventToolResult, Payload: mustRawPayload(t, map[string]any{
		"toolCallId": "call-2", "name": "bash", "status": "ok", "output": "plain output",
	})})
	if plain == nil || toolUpdateText(*plain) != "plain output" {
		t.Errorf("an ordinary stored result changed on replay: %+v", plain)
	}
}
