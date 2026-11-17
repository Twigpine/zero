package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/config"
	mcppkg "github.com/Twigpine/zero/internal/mcp"
)

// AN ALREADY-OPEN MANAGER HAS TO BE TOLD, NOT ASKED.
//
// Optional MCP registration runs on its own goroutine so a slow server cannot
// delay the first response, which means its result lands with no user input
// behind it. Bubble Tea renders only in response to a message, so a getter that
// reports late failures is not enough on its own: an overlay opened while a
// server was still connecting keeps rendering the configuration-derived enabled
// state until unrelated input or a resize happens to redraw it.
func TestAnOpenManagerRebuildsWhenBackgroundStartupCompletes(t *testing.T) {
	cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"optional": {URL: "https://optional.invalid/mcp"},
	}}
	var late []mcppkg.SkippedServer
	done := make(chan struct{})

	m := &model{
		width:               120,
		mcpConfig:           cfg,
		mcpStartupConfig:    cfg,
		mcpManager:          &mcpManagerState{},
		mcpLateSkipped:      func() []mcppkg.SkippedServer { return late },
		mcpStartupCompleted: done,
	}

	// Opened while the background registration is still in flight.
	before := m.mcpManagerOverlay(m.width)
	if !strings.Contains(before, "optional") {
		t.Fatalf("SETUP INVALID: the manager does not list the server:\n%s", before)
	}
	if strings.Contains(strings.ToLower(before), "failed") {
		t.Fatalf("SETUP INVALID: it already reads as failed before the result arrived:\n%s", before)
	}

	// The background registration finishes and reports the failure. No key, no
	// resize, no command: only the completion.
	late = []mcppkg.SkippedServer{{Name: "optional", Err: errors.New("optional server refused the connection")}}
	close(done)

	updated, _ := m.Update(mcpStartupCompletedMsg{})
	next, ok := updated.(model)
	if !ok {
		t.Fatalf("Update returned %T, want a model", updated)
	}

	// Asserted on the CACHE, not by rendering. Rendering calls mcpViewState,
	// which invalidates on demand, so an overlay drawn after the message would
	// look right even if the handler did nothing: the test would pass for the
	// wrong reason and stop pinning the handler at all. The cache being fresh
	// BEFORE anything renders is what says the completion was consumed.
	var cached string
	for _, server := range next.mcpViewStateCache.Servers {
		if server.Name == "optional" {
			cached = server.State + "|" + server.Error
		}
	}
	if !strings.Contains(cached, "failed") {
		t.Errorf("the completion did not rebuild the view state: %q", cached)
	}
	if !strings.Contains(cached, "refused the connection") {
		t.Errorf("the rebuilt state carries no reason: %q", cached)
	}

	after := next.mcpManagerOverlay(next.width)
	if !strings.Contains(strings.ToLower(after), "failed") {
		t.Errorf("the open manager still shows the configured state after the background failure:\n%s", after)
	}
	if !strings.Contains(after, "refused the connection") {
		t.Errorf("the reason never reached the open manager:\n%s", after)
	}
}

// And Init actually schedules the wait, or the message above would never be
// produced in a real session.
func TestInitWaitsForBackgroundStartupCompletion(t *testing.T) {
	done := make(chan struct{})
	m := model{mcpStartupCompleted: done}
	close(done)

	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init scheduled nothing")
	}
	if !producesMCPStartupCompletion(cmd) {
		t.Error("Init never scheduled the wait for background MCP startup, so a completed registration reaches no open surface")
	}
}

// producesMCPStartupCompletion runs a batched command tree far enough to see
// whether the completion wait is part of it.
func producesMCPStartupCompletion(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	switch msg := cmd().(type) {
	case mcpStartupCompletedMsg:
		return true
	case tea.BatchMsg:
		for _, sub := range msg {
			if producesMCPStartupCompletion(sub) {
				return true
			}
		}
	}
	return false
}
