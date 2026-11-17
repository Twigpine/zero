package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/mcp"
)

// A CREDENTIAL LONGER THAN THE OVERLAP MUST NOT LEAVE A VISIBLE PREFIX.
//
// The bound cuts the raw error before redaction, and redaction matches whole
// values, so a secret the cut sliced in half matches nothing and its surviving
// prefix is ordinary text. The fixed overlap makes that require a secret longer
// than the window, and nothing caps a configured header, URL, environment or
// stored token value, so it is a configuration away rather than impossible.
//
// The server also controls what comes before it. Enough control sequences to
// spend the raw budget produce no visible text of their own, so the credential's
// prefix becomes the first thing a reader sees.
func TestAnOversizedCredentialLeavesNoVisiblePrefix(t *testing.T) {
	// Longer than maxMCPSecretMatchWindow, which is the case the overlap alone
	// cannot cover, and OPAQUE. A recognisable shape like "sk-live-..." is caught
	// by the generic patterns whatever the bound does, so it would test nothing
	// here: this has to exercise the exact-value path.
	secret := strings.Repeat("Qw7ZmPr4", 700)
	if len(secret) <= maxMCPSecretMatchWindow {
		t.Fatalf("the fixture secret is %d bytes, which does not exceed the %d-byte window", len(secret), maxMCPSecretMatchWindow)
	}

	raw := config.MCPServerConfig{
		URL:     "https://host.invalid/mcp",
		Headers: map[string]string{"X-Workspace": secret},
	}

	// Padding that consumes the raw budget while rendering nothing, so the
	// credential starts just before the cut.
	padding := strings.Repeat("\x1b[2K", maxMCPReasonRawLen/4)
	got := redactMCPFailureReason(errors.New(padding+secret), raw, nil)

	for size := len(secret); size >= shortestMCPSecret; size /= 2 {
		if strings.Contains(got, secret[:size]) {
			t.Fatalf("a %d-character prefix of the credential survived into the panel:\n%.200q", size, got)
		}
	}
}

// Through the assembled state as well, since that is what the panel and the
// transcript actually carry.
func TestAnOversizedCredentialLeavesNoPrefixInTheAssembledState(t *testing.T) {
	secret := strings.Repeat("Qw7ZmPr4", 700)
	padding := strings.Repeat("\x1b[2K", maxMCPReasonRawLen/4)

	state := BuildMCPViewState(MCPStateOptions{
		Config: config.MCPConfig{
			Servers: map[string]config.MCPServerConfig{
				"docs": {URL: "https://host.invalid/mcp", Headers: map[string]string{"X-Workspace": secret}},
			},
		},
		Skipped: []mcp.SkippedServer{{Name: "docs", Err: errors.New(padding + secret)}},
	})

	for _, server := range state.Servers {
		if server.Name != "docs" {
			continue
		}
		rendered := server.Error + "\n" + server.Target
		for size := len(secret); size >= shortestMCPSecret; size /= 2 {
			if strings.Contains(rendered, secret[:size]) {
				t.Fatalf("a %d-character prefix reached the panel:\n%.200q", size, rendered)
			}
		}
		return
	}
	t.Fatal("the failed server is missing from the assembled state")
}

// And an ordinary failure that merely ENDS with something secret-shaped is not
// eaten. The tail trim only runs when the bound actually truncated.
func TestAnUntruncatedFailureKeepsItsTail(t *testing.T) {
	raw := config.MCPServerConfig{URL: "https://host.invalid/mcp"}
	message := "dial tcp 10.0.0.5:443: connect: connection refused"
	got := redactMCPFailureReason(errors.New(message), raw, nil)
	if !strings.Contains(got, message) {
		t.Errorf("an ordinary failure lost its tail:\n%s", got)
	}
}
