package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/mcp"
)

// A FAILING SERVER CAN ECHO ITS OWN CONFIGURED CREDENTIAL BACK AT US.
//
// Remote MCP servers are configured with arbitrary headers, so a credential can
// sit under a name nobody can predict. The generic redaction patterns match
// shapes they recognise, and `X-Workspace-Credential` is not one of them. This
// PR newly renders the startup error in both /mcp surfaces and the session
// transcript, so an echoed value would be printed and persisted.
//
// Redacting by VALUE rather than by shape is what closes that, and it does not
// depend on the `Authorization:` matcher, which control bytes can split before
// the later sanitizer strips them.
func TestFailedServerErrorRedactsConfiguredHeaderValues(t *testing.T) {
	const credential = "wk-live-4f9c2b7ae1d8"
	cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"remote": {
			Type:    "http",
			URL:     "https://mcp.example.com",
			Headers: map[string]string{"X-Workspace-Credential": credential},
		},
	}}
	// What a server that returns the request it choked on looks like.
	skipped := []mcp.SkippedServer{{
		Name: "remote",
		Err:  errors.New("startup failed: upstream rejected X-Workspace-Credential: " + credential),
	}}

	state := BuildMCPViewState(MCPStateOptions{Config: cfg, Skipped: skipped})
	if len(state.Servers) != 1 {
		t.Fatalf("expected one server view, got %d", len(state.Servers))
	}
	if got := state.Servers[0].Error; strings.Contains(got, credential) {
		t.Fatalf("the configured credential is rendered in the /mcp surface and the transcript: %q", got)
	}
	if state.Servers[0].State != "failed" {
		t.Errorf("state = %q, want failed", state.Servers[0].State)
	}
}

// The same for a stdio server's environment, which a launch failure often
// reports back verbatim.
func TestFailedServerErrorRedactsConfiguredEnvValues(t *testing.T) {
	const secret = "sk-env-9c1f2a6b40de"
	cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"local": {Command: "server", Env: map[string]string{"COMPANY_TOKEN": secret}},
	}}
	skipped := []mcp.SkippedServer{{Name: "local", Err: errors.New("exec failed with COMPANY_TOKEN=" + secret)}}

	state := BuildMCPViewState(MCPStateOptions{Config: cfg, Skipped: skipped})
	if got := state.Servers[0].Error; strings.Contains(got, secret) {
		t.Fatalf("the configured env secret is rendered: %q", got)
	}
}

// The message must still SAY something. Redacting by equality can punch holes
// through unrelated text, so short configured values are skipped; without that
// a server configured with "1" would blank every digit in the error.
func TestShortConfiguredValuesDoNotShredTheMessage(t *testing.T) {
	cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"remote": {Type: "http", URL: "https://mcp.example.com", Headers: map[string]string{"X-Retry": "1", "X-Mode": "true"}},
	}}
	skipped := []mcp.SkippedServer{{Name: "remote", Err: errors.New("connection refused after 1 attempt in true isolation")}}

	state := BuildMCPViewState(MCPStateOptions{Config: cfg, Skipped: skipped})
	got := state.Servers[0].Error
	if !strings.Contains(got, "connection refused") {
		t.Fatalf("the error text was destroyed by redacting trivial configured values: %q", got)
	}
}

// A server that starts fine carries no error, so nothing is rendered at all.
func TestHealthyServerHasNoErrorText(t *testing.T) {
	cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"remote": {Type: "http", URL: "https://mcp.example.com"},
	}}
	state := BuildMCPViewState(MCPStateOptions{Config: cfg})
	if got := state.Servers[0].Error; got != "" {
		t.Errorf("a healthy server reported %q", got)
	}
}
