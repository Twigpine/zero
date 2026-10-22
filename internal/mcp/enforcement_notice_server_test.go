package mcp

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
)

// A MODEL-FACING PROTOCOL BOUNDARY IS A PRESENTATION CONSUMER.
//
// This branch changed the result contract: Result.Output holds the UNDECORATED
// base text and ModelOutput is the sole model-facing projection that composes it
// with the typed enforcement notices. tools/call serialized Output directly,
// which was a complete value before and is not one now, so an affected Windows
// command reached an MCP client with its ordinary output and no statement that
// its DenyRead token shape left writes unconfined.
//
// Driven through Serve rather than the accessor, because the question is what
// goes on the wire.
func TestMCPToolsCallCarriesTheEnforcementNotice(t *testing.T) {
	const notice = "denyRead is configured, so the write jail is not confining writes"
	const output = "ran the command"

	for _, testCase := range []struct {
		name       string
		result     tools.Result
		wantText   string
		wantIsErr  bool
		wantNotice bool
	}{
		{
			name:       "successful command with a notice",
			result:     tools.Result{Status: tools.StatusOK, Output: output, EnforcementNotices: []string{notice}},
			wantIsErr:  false,
			wantNotice: true,
		},
		{
			name:       "failed command with a notice",
			result:     tools.Result{Status: tools.StatusError, Output: output, EnforcementNotices: []string{notice}},
			wantIsErr:  true,
			wantNotice: true,
		},
		{
			name:       "ordinary command with no notice",
			result:     tools.Result{Status: tools.StatusOK, Output: output},
			wantIsErr:  false,
			wantNotice: false,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			registry := tools.NewRegistry()
			registry.Register(serverFakeTool{
				name:        "run_thing",
				description: "runs a thing",
				parameters:  tools.Schema{Type: "object", AdditionalProperties: false},
				safety:      tools.Safety{SideEffect: tools.SideEffectRead, Permission: tools.PermissionAllow, Reason: "test"},
				run:         func(map[string]any) tools.Result { return testCase.result },
			})

			var input bytes.Buffer
			writeServerTestMessage(t, &input, rpcMessage{ID: 1, Method: "initialize"})
			writeServerTestMessage(t, &input, rpcMessage{Method: "notifications/initialized"})
			writeServerTestMessage(t, &input, rpcMessage{
				ID:     2,
				Method: "tools/call",
				Params: mustRaw(map[string]any{"name": "run_thing", "arguments": map[string]any{}}),
			})

			var out bytes.Buffer
			if err := Serve(context.Background(), &input, &out, registry, ServeOptions{Name: "zero-test", Version: "1.2.3"}); err != nil {
				t.Fatalf("Serve() error = %v", err)
			}
			reader := newMessageReader(&out)
			readServerTestMessage(t, reader) // initialize
			var call CallToolResult
			decodeServerTestResult(t, readServerTestMessage(t, reader), &call)

			if len(call.Content) != 1 || call.Content[0].Type != "text" {
				t.Fatalf("content shape changed: %#v", call.Content)
			}
			text := call.Content[0].Text
			if call.IsError != testCase.wantIsErr {
				t.Errorf("IsError = %v, want %v", call.IsError, testCase.wantIsErr)
			}
			if count := strings.Count(text, output); count != 1 {
				t.Errorf("the command's own output appears %d times, want exactly 1: %q", count, text)
			}
			gotNotice := strings.Count(text, notice)
			if testCase.wantNotice && gotNotice != 1 {
				t.Errorf("the disclosure appears %d times, want exactly 1: %q", gotNotice, text)
			}
			if !testCase.wantNotice {
				if gotNotice != 0 {
					t.Errorf("a disclosure appeared for a command that had none: %q", text)
				}
				if text != output {
					t.Errorf("ordinary output was altered: got %q, want %q", text, output)
				}
			}
		})
	}
}
