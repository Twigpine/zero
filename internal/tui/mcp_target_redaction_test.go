package tui

import (
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
)

// THE TARGET ROW IS ON THE SAME PANEL AS THE ERROR ROW.
//
// Redacting the failure reason while printing the same credential verbatim one
// line lower hands it straight back, and this row is persisted to the transcript
// too. Every header-flag form and any long query value reached it untouched.
func TestServerTargetDoesNotPrintCredentials(t *testing.T) {
	const token = "opaque-workspace-token-9f3c2b7ae1d8"

	for _, testCase := range []struct {
		name string
		raw  config.MCPServerConfig
	}{
		{"header separated", config.MCPServerConfig{Command: "mcp-server", Args: []string{"--header", "X-Workspace-Id: " + token}}},
		{"header equals", config.MCPServerConfig{Command: "mcp-server", Args: []string{"--header=X-Workspace-Id: " + token}}},
		{"header packed", config.MCPServerConfig{Command: "mcp-server", Args: []string{"--header X-Workspace-Id: " + token}}},
		{"short separated", config.MCPServerConfig{Command: "mcp-server", Args: []string{"-H", "X-Workspace-Id: " + token}}},
		{"short equals", config.MCPServerConfig{Command: "mcp-server", Args: []string{"-H=X-Workspace-Id: " + token}}},
		{"short packed", config.MCPServerConfig{Command: "mcp-server", Args: []string{"-H X-Workspace-Id: " + token}}},
		{"no space after colon", config.MCPServerConfig{Command: "mcp-server", Args: []string{"--header", "X-Workspace-Id:" + token}}},
		{"url arbitrary query", config.MCPServerConfig{URL: "https://host.invalid/mcp?workspace=" + token}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			target := mcpServerTarget(testCase.raw)
			if strings.Contains(target, token) {
				t.Errorf("the credential is printed verbatim in the Target row, which the panel shows and the transcript keeps:\n%s", target)
			}
			if !strings.Contains(target, mcpDisplayRedacted) {
				t.Errorf("nothing was redacted at all:\n%s", target)
			}
		})
	}
}

// The header name and the endpoint still have to be readable, or the row stops
// describing the server it is there to describe.
func TestServerTargetKeepsTheReadableParts(t *testing.T) {
	const token = "opaque-workspace-token-9f3c2b7ae1d8"

	stdio := mcpServerTarget(config.MCPServerConfig{Command: "mcp-server", Args: []string{"--header", "X-Workspace-Id: " + token}})
	for _, want := range []string{"mcp-server", "--header", "X-Workspace-Id"} {
		if !strings.Contains(stdio, want) {
			t.Errorf("%q missing from the target row, so it no longer describes the invocation:\n%s", want, stdio)
		}
	}

	http := mcpServerTarget(config.MCPServerConfig{URL: "https://docs.host.invalid/mcp/v1?mode=sse&workspace=" + token})
	for _, want := range []string{"docs.host.invalid", "/mcp/v1", "mode=sse"} {
		if !strings.Contains(http, want) {
			t.Errorf("%q missing from the target row, so it no longer identifies the endpoint:\n%s", want, http)
		}
	}
}
