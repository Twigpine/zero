package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/mcp"
)

// THROUGH THE REAL ENTRY POINT, not the helper.
//
// The rest of this file's neighbours call redactMCPFailureReason directly, which
// proves the helper and nothing else. BuildMCPViewState is what the panel and
// the transcript actually go through, and it is where a second surface could
// reintroduce either problem: the row renderer inspects the raw query field
// separately, and the reason is cut again downstream. A helper that is clean
// while the assembled view is not would still be a leak.
func buildOneFailedServer(t *testing.T, endpoint string, failure error) MCPServerView {
	t.Helper()
	state := BuildMCPViewState(MCPStateOptions{
		Config: config.MCPConfig{
			Servers: map[string]config.MCPServerConfig{
				"docs": {URL: endpoint},
			},
		},
		Skipped: []mcp.SkippedServer{{Name: "docs", Err: failure}},
	})
	for _, server := range state.Servers {
		if server.Name == "docs" {
			return server
		}
	}
	t.Fatalf("the failed server is missing from the assembled state: %+v", state.Servers)
	return MCPServerView{}
}

// An arbitrary query key carrying a percent-encoded credential. "workspace" is
// not a conventionally sensitive name, so the generic patterns do not catch it,
// and url.ParseQuery hands back the decoded spelling while the server echoes the
// escaped one it was given.
func TestAssembledStateRedactsBothCredentialSpellings(t *testing.T) {
	const token = "opaque-workspace-token-9f3c2b7ae1d8"
	escaped := strings.ReplaceAll(token, "-", "%2D")

	for _, testCase := range []struct {
		name     string
		endpoint string
	}{
		{"arbitrary query key", "https://host.invalid/mcp?workspace=" + escaped},
		{"userinfo username", "https://" + escaped + "@host.invalid/mcp"},
		{"userinfo password", "https://svc:" + escaped + "@host.invalid/mcp"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := buildOneFailedServer(t, testCase.endpoint, errors.New("502 Bad Gateway from "+testCase.endpoint))
			// Both the row's reason AND its target are rendered and persisted.
			rendered := server.Error + "\n" + server.Target
			if strings.Contains(rendered, token) {
				t.Errorf("the decoded credential reached the panel:\n%s", rendered)
			}
			if strings.Contains(rendered, escaped) {
				t.Errorf("the escaped credential reached the panel; %%2D decodes to a hyphen, so it is fully recoverable:\n%s", rendered)
			}
		})
	}
}

// And the fixed budget holds at the entry point, for a hostile error AND for a
// hostile secret. The second half is the one that matters: the margin past the
// cut used to be sized to the longest configured value, so the other side could
// widen the limit simply by configuring a large credential.
func TestAssembledStateBoundsHostileFailuresAndSecrets(t *testing.T) {
	budget := maxMCPReasonRawLen + maxMCPSecretMatchWindow

	for _, testCase := range []struct {
		name     string
		endpoint string
		failure  string
	}{
		{"multi-megabyte failure", "https://host.invalid/mcp", "tool name conflict: " + strings.Repeat("A", 8*1024*1024)},
		{"multi-megabyte configured secret", "https://host.invalid/mcp?workspace=" + strings.Repeat("s", 2*1024*1024), "conflict: " + strings.Repeat("B", 64*1024)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			started := time.Now()
			server := buildOneFailedServer(t, testCase.endpoint, errors.New(testCase.failure))
			elapsed := time.Since(started)

			if len(server.Error) > budget {
				t.Errorf("the retained reason is %d bytes against a fixed budget of %d", len(server.Error), budget)
			}
			// The work has to be bounded too, not only the result. Before the fix an
			// eight megabyte reason took seconds here; the ceiling is loose on purpose
			// so it fails on the old unbounded behaviour and not on a slow machine.
			if elapsed > 2*time.Second {
				t.Errorf("building the state took %s; the pipeline is still doing work proportional to the input", elapsed)
			}
		})
	}
}

// A credential straddling the cut must not leave a usable prefix behind, or the
// bound would have manufactured the leak it has nothing to do with.
func TestAssembledStateRedactsASecretCrossingTheBoundary(t *testing.T) {
	const token = "opaque-workspace-token-9f3c2b7ae1d8"
	endpoint := "https://host.invalid/mcp?workspace=" + token

	filler := strings.Repeat("A", maxMCPReasonRawLen-len(token)/2)
	server := buildOneFailedServer(t, endpoint, errors.New(filler+token+strings.Repeat("B", 4096)))

	for size := len(token); size > 8; size-- {
		if strings.Contains(server.Error, token[:size]) {
			t.Errorf("a %d-character prefix of the credential crossed the boundary intact: %q", size, token[:size])
			break
		}
	}
}
