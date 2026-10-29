package mcp

import (
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
)

// THE NORMALIZED NAME IS AN IDENTITY, SO IT HAS TO BE UNIQUE.
//
// Trimming means "docs" and " docs " are two config keys and one runtime
// server, and everything downstream keys on the runtime name: tool accounting,
// the skipped-server observations the panel renders, and invalidation. Two rows
// then share one failure and report the same state, with Go map iteration
// deciding which configuration survives.
//
// It is also a confidentiality problem rather than only a wrong status. Each row
// redacts that shared error with its OWN configuration, so if the server that
// actually failed echoed a credential, the other row does not have that value
// among its candidates and prints it.
func TestDuplicateNamesAfterNormalizationAreRejected(t *testing.T) {
	cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs":   {URL: "https://a.invalid/mcp"},
		"  docs": {URL: "https://b.invalid/mcp"},
	}}
	servers, err := NormalizeConfig(cfg)
	if err == nil {
		t.Fatalf("two names resolving to one identity were accepted: %#v", servers)
	}
	// Actionable means naming both spellings: the operator cannot see the
	// collision in a config file where one key merely looks indented.
	for _, want := range []string{`"docs"`, `"  docs"`, "rename"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %s: %v", want, err)
		}
	}
}

// A single padded name is not the problem and must keep working: trimming is
// the intended behaviour, two names trimming to one is not.
func TestASinglePaddedNameStillNormalizes(t *testing.T) {
	servers, err := NormalizeConfig(config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"  docs  ": {URL: "https://a.invalid/mcp"},
	}})
	if err != nil {
		t.Fatalf("a single padded name was rejected: %v", err)
	}
	if len(servers) != 1 || servers[0].Name != "docs" {
		t.Fatalf("servers = %#v, want one server named docs", servers)
	}
}

// A disabled entry claims no identity, so it cannot collide with the live one
// that replaced it.
func TestADisabledDuplicateDoesNotCollide(t *testing.T) {
	servers, err := NormalizeConfig(config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs":   {URL: "https://a.invalid/mcp"},
		"  docs": {URL: "https://b.invalid/mcp", Disabled: true},
	}})
	if err != nil {
		t.Fatalf("a disabled duplicate was treated as a collision: %v", err)
	}
	if len(servers) != 1 {
		t.Fatalf("servers = %#v, want only the enabled one", servers)
	}
}
