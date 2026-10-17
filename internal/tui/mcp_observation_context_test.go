package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	mcppkg "github.com/Twigpine/zero/internal/mcp"
)

func mcpViewFor(t *testing.T, state MCPViewState, name string) MCPServerView {
	t.Helper()
	for _, server := range state.Servers {
		if server.Name == name {
			return server
		}
	}
	t.Fatalf("server %q is missing from the panel: %#v", name, state.Servers)
	return MCPServerView{}
}

// AN OBSERVATION'S OWN CONTEXT OUTRANKS THE ONE SAMPLED AT THE SURFACE.
//
// The surface samples the token store when it is built, which is after startup
// registered these failures and after everything that runs in between could
// have rotated the store. Comparing against that sample asks "has anything
// changed since I started rendering", which is always no, instead of "is the
// material that made this error safe still here".
//
// The visible consequence: the raw error quotes a bearer that was refreshed
// away during startup, the surface-level sample matches the current store
// exactly, the row is rendered, and the redaction candidates no longer contain
// the bearer that is sitting in the text.
func TestTheObservationsOwnCredentialContextDecidesStaleness(t *testing.T) {
	const rotatedAway = "bearer-rotated-away-9f3c2b7ae1"
	cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {URL: "https://host.invalid/mcp"},
	}}
	// Only the stored-token candidate set was ever hiding this value; no generic
	// pattern can recognise it.
	failure := errors.New("upstream rejected the session; echoed credential was " + rotatedAway)

	state := BuildMCPViewState(MCPStateOptions{
		Config: cfg,
		Skipped: []mcppkg.SkippedServer{{
			Name: "docs",
			Err:  failure,
			// Recorded at registration, while the bearer still existed.
			Credentials: mcppkg.CredentialFingerprint([]string{rotatedAway}),
		}},
		// Sampled when this surface was built: the store is already empty, so this
		// agrees with the current state and claims nothing changed.
		SkippedCredentials: mcppkg.CredentialFingerprint(nil),
	})

	docs := mcpViewFor(t, state, "docs")
	if strings.Contains(docs.Error, rotatedAway) {
		t.Errorf("the credential reached the panel because staleness was judged against the surface's own sample: %q", docs.Error)
	}
	if docs.State != "failed" {
		t.Errorf("State = %q, want the failure still reported", docs.State)
	}
}

// And an observation whose context is unchanged still renders normally, or the
// per-observation check would just be suppressing everything.
func TestAnUnchangedObservationContextStillRenders(t *testing.T) {
	cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {URL: "https://host.invalid/mcp"},
	}}
	state := BuildMCPViewState(MCPStateOptions{
		Config: cfg,
		Skipped: []mcppkg.SkippedServer{{
			Name:        "docs",
			Err:         errors.New("dial tcp 10.0.0.5:443: connect: connection refused"),
			Credentials: mcppkg.CredentialFingerprint(nil),
		}},
		SkippedCredentials: mcppkg.CredentialFingerprint(nil),
	})
	if got := mcpViewFor(t, state, "docs").Error; !strings.Contains(got, "connection refused") {
		t.Errorf("an observation with an unchanged context lost its reason: %q", got)
	}
}

// An observation that recorded nothing falls back to the surface's sample,
// which is what every caller that has not been taught to record one still gets.
func TestAnObservationWithoutContextFallsBackToTheSurfaceSample(t *testing.T) {
	const bearer = "stored-bearer-9f3c2b7ae1d8c4"
	cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {URL: "https://host.invalid/mcp"},
	}}
	state := BuildMCPViewState(MCPStateOptions{
		Config:             cfg,
		Skipped:            []mcppkg.SkippedServer{{Name: "docs", Err: errors.New("echoed " + bearer)}},
		SkippedCredentials: mcpCredentialFingerprint([]string{bearer}),
	})
	if got := mcpViewFor(t, state, "docs").Error; strings.Contains(got, bearer) {
		t.Errorf("the fallback stopped guarding an observation that recorded no context: %q", got)
	}
}

// A BACKGROUND REGISTRATION'S FAILURES ARE STILL THIS PANEL'S SUBJECT.
//
// Optional servers are moved off the critical path so a slow one cannot delay
// the first response. That is a scheduling decision, not a visibility one: they
// are ordinary configured rows. Their failures land after this surface exists,
// so a snapshot taken at construction cannot carry them, and the rows render
// from configuration alone -- reporting a server that never connected as
// enabled, which is the single thing the panel exists to prevent.
func TestLateFailuresReachThePanelAndInvalidateTheCache(t *testing.T) {
	cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs":     {URL: "https://host.invalid/mcp"},
		"optional": {URL: "https://optional.invalid/mcp"},
	}}
	var late []mcppkg.SkippedServer
	m := &model{
		mcpConfig:        cfg,
		mcpStartupConfig: cfg,
		mcpSkipped:       []mcppkg.SkippedServer{{Name: "docs", Err: errors.New("docs refused")}},
		mcpLateSkipped:   func() []mcppkg.SkippedServer { return late },
	}

	// Rendered before the background registration finished: the row is honest
	// about what is known, and the cache is now warm.
	if got := mcpViewFor(t, m.mcpViewState(), "optional").State; got != "enabled" {
		t.Fatalf("State = %q before the background result, want enabled", got)
	}

	late = []mcppkg.SkippedServer{{Name: "optional", Err: errors.New("optional server refused the connection")}}

	optional := mcpViewFor(t, m.mcpViewState(), "optional")
	if optional.State != "failed" {
		t.Errorf("State = %q, want failed: the background registration reported it as skipped", optional.State)
	}
	if !strings.Contains(optional.Error, "refused the connection") {
		t.Errorf("the reason did not reach the panel: %q", optional.Error)
	}
	// The failure known at startup is untouched.
	if got := mcpViewFor(t, m.mcpViewState(), "docs").State; got != "failed" {
		t.Errorf("the startup failure was lost by the merge: State = %q", got)
	}
}

// A late observation is aged against the configuration it was made under,
// exactly as the startup snapshot is: replacing the server it is about leaves it
// describing something that no longer exists.
func TestALateFailureIsDroppedWhenItsSubjectIsReplaced(t *testing.T) {
	startup := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"optional": {URL: "https://optional.invalid/mcp"},
	}}
	replaced := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"optional": {URL: "https://different.invalid/mcp"},
	}}
	m := &model{
		mcpConfig:        replaced,
		mcpStartupConfig: startup,
		mcpLateSkipped: func() []mcppkg.SkippedServer {
			return []mcppkg.SkippedServer{{Name: "optional", Err: errors.New("the old endpoint refused")}}
		},
	}
	optional := mcpViewFor(t, m.mcpViewState(), "optional")
	if optional.State == "failed" {
		t.Errorf("the replacement inherited the dead endpoint's failure: %q", optional.Error)
	}
}
