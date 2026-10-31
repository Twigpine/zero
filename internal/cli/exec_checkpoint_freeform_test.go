package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

func freeformApplyPatchArguments(root string, absolute bool) string {
	target := "main.txt"
	if absolute {
		target = filepath.Join(root, "main.txt")
	}
	return "*** Begin Patch\n*** Update File: " + target + "\n@@\n-before\n+after\n*** End Patch\n"
}

// captureCheckpoint must resolve a freeform apply_patch call through the same
// preparation execution uses, so the before-state is captured and rewind can
// restore it. The JSON control pins the unchanged path.
func TestCaptureCheckpointFreeformApplyPatch(t *testing.T) {
	for _, tc := range []struct {
		name     string
		freeform bool
		absolute bool
		jsonArgs bool
	}{
		{name: "freeform", freeform: true},
		{name: "freeform absolute header", freeform: true, absolute: true},
		{name: "json arguments", jsonArgs: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			t.Setenv("ZERO_CHECKPOINTS", "on")
			root := t.TempDir()
			// The absolute-header case needs the physical root spelling the tool
			// resolves (macOS /var vs /private/var, Windows short names).
			if resolved, err := filepath.EvalSymlinks(root); err == nil {
				root = resolved
			}
			if err := os.WriteFile(filepath.Join(root, "main.txt"), []byte("before\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			store := sessions.NewStore(sessions.StoreOptions{})
			session, err := store.Create(sessions.CreateInput{Title: "freeform checkpoint"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.AppendEvent(session.SessionID, sessions.AppendEventInput{
				Type:    sessions.EventMessage,
				Payload: map[string]string{"role": "user", "content": "apply the patch"},
			}); err != nil {
				t.Fatal(err)
			}
			recorder := &execSessionRecorder{prepared: sessions.PreparedExec{Store: store, Session: session}}
			registry := tools.NewRegistry()
			registry.Register(tools.NewScopedApplyPatchTool(root, nil))

			call := agent.ToolCall{ID: "call", Name: "apply_patch", Freeform: tc.freeform}
			if tc.jsonArgs {
				encoded, err := json.Marshal(map[string]string{"patch": freeformApplyPatchArguments(root, false)})
				if err != nil {
					t.Fatal(err)
				}
				call.Arguments = string(encoded)
			} else {
				call.Arguments = freeformApplyPatchArguments(root, tc.absolute)
			}

			event, ok := recorder.captureCheckpoint(root, registry, call)
			if !ok || event.Type != sessions.EventSessionCheckpoint {
				t.Fatalf("captureCheckpoint() = %#v, %t; want a checkpoint event", event, ok)
			}
			var payload sessions.CheckpointPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatalf("decode checkpoint payload: %v", err)
			}
			if len(payload.Files) != 1 || payload.Files[0].Path != "main.txt" || payload.Files[0].Blob == "" {
				t.Fatalf("checkpoint files = %#v, want main.txt with a blob", payload.Files)
			}

			// Simulate the tool's mutation, then rewind past it.
			if err := os.WriteFile(filepath.Join(root, "main.txt"), []byte("after\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			report, err := store.ApplyRewind(session.SessionID, root, 1)
			if err != nil {
				t.Fatalf("ApplyRewind: %v", err)
			}
			if report.FilesRestored != 1 {
				t.Fatalf("restore report = %#v, want one restored file", report)
			}
			content, err := os.ReadFile(filepath.Join(root, "main.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if string(content) != "before\n" {
				t.Fatalf("restored content = %q, want before newline", content)
			}
		})
	}
}

// freeformPatchExecProvider emits one native freeform apply_patch call, then a
// final text turn.
type freeformPatchExecProvider struct {
	patch string
	calls int
}

func (provider *freeformPatchExecProvider) StreamCompletion(ctx context.Context, request zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	ch := make(chan zeroruntime.StreamEvent, 5)
	calls := provider.calls
	provider.calls++
	if calls == 0 {
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call_patch", ToolName: "apply_patch", ToolCallFreeform: true}
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call_patch", ArgumentsFragment: provider.patch}
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call_patch"}
	} else {
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventText, Content: "patched"}
	}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}
	close(ch)
	return ch, nil
}

// End-to-end: exec captures the freeform checkpoint and `sessions rewind`
// restores the pre-patch content.
func TestRunExecFreeformApplyPatchCheckpointsForRewind(t *testing.T) {
	isolateCLIUserState(t)
	t.Setenv("ZERO_CHECKPOINTS", "on")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.txt"), []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	provider := &freeformPatchExecProvider{patch: freeformApplyPatchArguments(root, false)}
	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{
		"exec", "--init-session-id", "freeform_rewind", "--skip-permissions-unsafe",
		"--output-format", "stream-json", "apply the patch",
	}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return root, nil },
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return execResolvedConfig(), nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return provider, nil
		},
	})
	if exitCode != exitSuccess {
		t.Fatalf("exit = %d, want %d; stderr=%q stdout=%q", exitCode, exitSuccess, stderr.String(), stdout.String())
	}
	content, err := os.ReadFile(filepath.Join(root, "main.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "after\n" {
		t.Fatalf("patched content = %q, want after newline", content)
	}

	store := sessions.NewStore(sessions.StoreOptions{})
	events, err := store.ReadEvents("freeform_rewind")
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	checkpoints := 0
	for _, event := range events {
		if event.Type == sessions.EventSessionCheckpoint {
			checkpoints++
		}
	}
	if checkpoints != 1 {
		t.Fatalf("checkpoint events = %d, want 1; events=%v", checkpoints, sessionEventTypes(events))
	}

	report, err := store.ApplyRewind("freeform_rewind", root, 1)
	if err != nil {
		t.Fatalf("ApplyRewind: %v", err)
	}
	if report.FilesRestored != 1 {
		t.Fatalf("restore report = %#v, want one restored file", report)
	}
	content, err = os.ReadFile(filepath.Join(root, "main.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "before\n" {
		t.Fatalf("rewound content = %q, want before newline", content)
	}
}

func sessionEventTypes(events []sessions.Event) []string {
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, string(event.Type))
	}
	return types
}
