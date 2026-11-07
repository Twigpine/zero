package tui

import (
	"context"
	"reflect"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/config"
)

// BOTH CHECK ACTIONS HAND THE RECEIVING COMMAND THE SAME ARGUMENTS.
//
// The manager offers the check twice, on Enter and on Alt+c. What either sends
// is the exact configuration key, and the command that receives it is the one
// place that has to turn that key into a runtime name before it reads the
// registration outcome. internal/cli drives these exact arguments through that
// command with a failed registration (TestRunMCPCheckMatchesFailuresByRuntimeName);
// this pins that there is one argument shape to drive, so a fix proven for one
// action is proven for the other.
func TestManagerCheckActionsDispatchTheSameExactKey(t *testing.T) {
	const paddedKey = " docs "
	for _, tc := range []struct {
		name string
		key  tea.Msg
	}{
		{"enter", testKey(tea.KeyEnter)},
		{"alt+c", testKeyAltText("c")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var called []string
			m := newModel(context.Background(), Options{
				MCPConfig: config.MCPConfig{Servers: map[string]config.MCPServerConfig{
					paddedKey: {Type: "stdio", Command: "docs-mcp"},
				}},
				MCPCommand: func(_ context.Context, args []string) MCPCommandResult {
					called = append([]string{}, args...)
					return MCPCommandResult{ExitCode: 1, Error: "MCP server  docs  is not reachable"}
				},
			})
			m.width = 120
			m.height = 36
			m = m.openMCPManager()
			selected := false
			for index, item := range m.mcpManagerItems() {
				if item.Kind == mcpManagerItemServer && item.ConfigKey == paddedKey {
					m.mcpManager.selected = index
					selected = true
				}
			}
			if !selected {
				t.Fatalf("the manager lists no server under the exact key %q", paddedKey)
			}
			updated, cmd := m.Update(tc.key)
			if cmd == nil {
				t.Fatal("the check action did not start a command")
			}
			applyCommandResult(t, updated.(model), cmd)
			if want := []string{"check", paddedKey}; !reflect.DeepEqual(called, want) {
				t.Fatalf("dispatched %#v, want %#v", called, want)
			}
		})
	}
}
