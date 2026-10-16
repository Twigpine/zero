package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/Gitlawb/zero/internal/remotetoken"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

func TestTUICheckpointBeforeDeniedCredentialMutation(t *testing.T) {
	for _, name := range []string{"write_file", "edit_file"} {
		t.Run(name, func(t *testing.T) {
			store := testSessionStore(t)
			root := t.TempDir()
			const secret = "tui-checkpoint-bearer"
			token := filepath.Join(root, "token")
			if err := os.WriteFile(token, []byte(secret), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv(remotetoken.EnvToken, "")
			t.Setenv(remotetoken.EnvTokenFile, token)
			t.Setenv(remotetoken.EnvTokenFileResolved, "")
			t.Setenv(remotetoken.EnvTokenFileIdentity, "")
			args := `{"path":"token","content":"replacement","overwrite":true,"old_string":"tui-checkpoint-bearer","new_string":"replacement"}`
			provider := &scriptedProvider{scripts: [][]zeroruntime.StreamEvent{{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "mutate", ToolName: name},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "mutate", ArgumentsFragment: args},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "mutate"},
				{Type: zeroruntime.StreamEventDone},
			}, textScript("finished")}}
			registry := tools.NewRegistry()
			registry.Register(tools.NewScopedWriteFileTool(root, nil))
			registry.Register(tools.NewScopedEditFileTool(root, nil))
			messages := make(chan tea.Msg, 128)
			m := newPermissionTestModel(root, provider, registry, store, nil, messages)
			m.input.SetValue("update the file")
			updated, cmd := m.Update(testKey(tea.KeyEnter))
			next := updated.(model)
			if cmd == nil {
				t.Fatal("agent run did not start")
			}
			_, _ = next.Update(execCmd(cmd))
			events := readOnlySessionEvents(t, store)
			if countSessionEvents(events, sessions.EventToolCall) != 1 {
				t.Fatal("mutation callback was not reached")
			}
			if countSessionEvents(events, sessions.EventToolResult) != 1 {
				t.Fatal("mutation result was not recorded")
			}
			if countSessionEvents(events, sessions.EventSessionCheckpoint) != 0 {
				t.Fatal("credential was checkpointed before denied mutation")
			}
			content, err := os.ReadFile(token)
			if err != nil || string(content) != secret {
				t.Fatalf("credential mutation was not denied: %v", err)
			}
			for _, event := range events {
				if event.Type == sessions.EventToolResult && !strings.Contains(string(event.Payload), "holds the remote bridge token") {
					t.Fatalf("mutation failed for a reason other than credential protection: %s", event.Payload)
				}
			}
		})
	}
}
