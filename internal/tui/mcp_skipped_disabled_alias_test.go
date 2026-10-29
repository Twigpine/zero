package tui

import (
	"errors"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/mcp"
)

func docsSkipped() []mcp.SkippedServer {
	return []mcp.SkippedServer{{Name: "docs", Err: errors.New("connect timed out")}}
}

// A DISABLED ALIAS MUST NOT DECIDE WHETHER THE ENABLED SERVER'S FAILURE SURVIVES.
//
// ValidateUniqueNames deliberately accepts an enabled "docs" alongside a disabled
// " docs": a disabled entry claims no runtime identity. canonicalMCPServers
// copied both into a map keyed by the trimmed name, so they collided, and Go
// randomises map iteration. The before and after snapshots each picked a winner
// independently, so an unrelated /mcp operation that left the enabled entry
// untouched could compare it against the disabled alias, find them different, and
// discard the failure. A server that is still unavailable was then reported as
// fine, on roughly one run in five.
//
// Looped because a single run passed most of the time even with the defect.
func TestDisabledAliasDoesNotAgeOutTheEnabledServersFailure(t *testing.T) {
	withAlias := func() config.MCPConfig {
		return config.MCPConfig{Servers: map[string]config.MCPServerConfig{
			"docs":  {Type: "http", URL: "https://real.example.com"},
			" docs": {Type: "http", URL: "https://disabled.example.com", Disabled: true},
		}}
	}
	for run := 0; run < 400; run++ {
		kept := retainedMCPSkipped(docsSkipped(), withAlias(), withAlias())
		if len(kept) != 1 {
			t.Fatalf("run %d: the enabled server's failure was discarded on an unchanged config, so /mcp reports a still-unavailable server as fine", run)
		}
	}
}

// And the aging still works, or the fix above is satisfied by never dropping.
func TestRetainedMCPSkippedStillAgesOutAReplacedServer(t *testing.T) {
	before := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {Type: "http", URL: "https://real.example.com"},
	}}
	after := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {Type: "http", URL: "https://replacement.example.com"},
	}}
	if kept := retainedMCPSkipped(docsSkipped(), before, after); len(kept) != 0 {
		t.Errorf("a replaced endpoint inherited the old one's failure: %#v", kept)
	}
}

// Disabling the server that failed ages the observation out too: registration
// would not have run it, so there is no longer a running subject to describe.
func TestRetainedMCPSkippedDropsAFailureForADisabledServer(t *testing.T) {
	before := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {Type: "http", URL: "https://real.example.com"},
	}}
	after := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {Type: "http", URL: "https://real.example.com", Disabled: true},
	}}
	if kept := retainedMCPSkipped(docsSkipped(), before, after); len(kept) != 0 {
		t.Errorf("a disabled server kept a startup failure it can no longer have: %#v", kept)
	}
}

// Two ENABLED entries claiming one canonical name is ambiguous, not arbitrary.
// ValidateUniqueNames rejects that config so it should not arrive here, but if it
// does there is no way to say which entry the observation was about.
func TestRetainedMCPSkippedDropsAnAmbiguousCanonicalName(t *testing.T) {
	ambiguous := func() config.MCPConfig {
		return config.MCPConfig{Servers: map[string]config.MCPServerConfig{
			"docs":  {Type: "http", URL: "https://one.example.com"},
			" docs": {Type: "http", URL: "https://two.example.com"},
		}}
	}
	for run := 0; run < 100; run++ {
		if kept := retainedMCPSkipped(docsSkipped(), ambiguous(), ambiguous()); len(kept) != 0 {
			t.Fatalf("run %d: an observation was attributed to one of two enabled entries sharing a canonical name: %#v", run, kept)
		}
	}
}
