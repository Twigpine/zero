package mcp

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/tools"
)

// disclosingRaceClient is a launched server that reports a disclosure.
type disclosingRaceClient struct {
	fakeToolClient
	notices []string
}

func (c *disclosingRaceClient) StartupNotices() []string { return c.notices }

// THE CONCURRENT PHASE TOUCHES NO SHARED STATE, AND THAT IS LOAD-BEARING.
//
// RegisterTools runs one goroutine per server and commits everything in a
// deterministic serial phase afterwards, which is what lets the result be
// identical regardless of completion order. Collecting the startup disclosures
// inside the goroutine broke both halves of that: the append raced the slice
// header, so entries could be lost or overwritten, and whichever survived were
// ordered by completion time rather than by server.
//
// Many servers rather than one, because a single disclosing server cannot
// exercise a shared write at all. Run this package with -race.
func TestStartupDisclosuresAreCollectedWithoutRacing(t *testing.T) {
	const servers = 32
	configured := map[string]config.MCPServerConfig{}
	for index := range servers {
		configured[fmt.Sprintf("srv%02d", index)] = config.MCPServerConfig{Type: "stdio", Command: "server"}
	}

	registry := tools.NewRegistry()
	runtime, err := RegisterTools(context.Background(), registry, config.MCPConfig{Servers: configured}, RegisterOptions{
		ClientFactory: func(_ context.Context, server Server) (ToolClient, error) {
			return &disclosingRaceClient{
				fakeToolClient: fakeToolClient{listed: []RemoteTool{{Name: "tool_" + server.Name, Description: "d"}}},
				notices:        []string{"denyRead is configured for " + server.Name},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("RegisterTools() error = %v", err)
	}
	defer runtime.Close()

	got := runtime.StartupDisclosures()
	if len(got) != servers {
		t.Fatalf("collected %d disclosures, want %d: a shared append loses entries", len(got), servers)
	}

	// Deterministic server order, not completion order. Sorting the result and
	// then comparing would hide exactly the defect this pins.
	names := make([]string, 0, len(got))
	for _, disclosure := range got {
		names = append(names, disclosure.Name)
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	for index := range names {
		if names[index] != sorted[index] {
			t.Fatalf("disclosure %d is %q, want %q: order follows completion rather than server order", index, names[index], sorted[index])
		}
	}
}

// failingDisclosingClient launches (so it has a disclosure) and then fails
// tools/list, which is the shape that used to drop the fact.
type failingDisclosingClient struct {
	fakeToolClient
	notices []string
}

func (c *failingDisclosingClient) StartupNotices() []string { return c.notices }
func (c *failingDisclosingClient) ListTools(context.Context) ([]RemoteTool, error) {
	return nil, fmt.Errorf("initialize failed after the process started")
}

// THE LAUNCH FACT OUTLIVES THE CONNECTION.
//
// connectStdio records the disclosure once cmd.Start returns, which is the right
// moment. But a stdio server can start, do filesystem work, and then fail
// initialize or tools/list, and that path closes the client and returns nil. The
// disclosure was reachable only through that client, so it died with it, and the
// operator was told the server was unavailable without being told the process
// had already run without the write jail.
func TestADisclosureSurvivesAFailureAfterTheProcessLaunched(t *testing.T) {
	const notice = "denyRead is configured, so the write jail is not confining writes"
	registry := tools.NewRegistry()
	runtime, err := RegisterTools(context.Background(), registry, config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {Type: "stdio", Command: "docs-mcp"},
	}}, RegisterOptions{
		ClientFactory: func(context.Context, Server) (ToolClient, error) {
			return &failingDisclosingClient{notices: []string{notice}}, nil
		},
	})
	if err != nil {
		t.Fatalf("RegisterTools() error = %v", err)
	}
	defer runtime.Close()

	skipped := runtime.Skipped()
	if len(skipped) != 1 {
		t.Fatalf("Skipped() = %#v, want the failure recorded", skipped)
	}
	disclosures := runtime.StartupDisclosures()
	if len(disclosures) != 1 || len(disclosures[0].Notices) != 1 || disclosures[0].Notices[0] != notice {
		t.Fatalf("StartupDisclosures() = %#v, want the launch disclosure kept despite the failure", disclosures)
	}
	if disclosures[0].Name != "docs" {
		t.Errorf("Name = %q, want the server it describes", disclosures[0].Name)
	}
}

// And a server that never launched still discloses nothing.
func TestAFactoryFailureDisclosesNothing(t *testing.T) {
	registry := tools.NewRegistry()
	runtime, err := RegisterTools(context.Background(), registry, config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {Type: "stdio", Command: "docs-mcp"},
	}}, RegisterOptions{
		ClientFactory: func(context.Context, Server) (ToolClient, error) {
			return nil, fmt.Errorf("could not start the process")
		},
	})
	if err != nil {
		t.Fatalf("RegisterTools() error = %v", err)
	}
	defer runtime.Close()
	if disclosures := runtime.StartupDisclosures(); len(disclosures) != 0 {
		t.Errorf("StartupDisclosures() = %#v, want none for a process that never started", disclosures)
	}
}

// A launched process that fails its HANDSHAKE keeps its disclosure too.
//
// connectStdio records the notices once cmd.Start returns, which is the right
// moment, but the initialize failure path closes and discards the client. The
// client was the only carrier, so the fact died with the connection unless the
// failure carries it out itself.
func TestADisclosureSurvivesAnInitializeFailure(t *testing.T) {
	const notice = "denyRead is configured, so the write jail is not confining writes"
	registry := tools.NewRegistry()
	runtime, err := RegisterTools(context.Background(), registry, config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {Type: "stdio", Command: "docs-mcp"},
	}}, RegisterOptions{
		ClientFactory: func(context.Context, Server) (ToolClient, error) {
			// What connectStdio does once Start has succeeded and the handshake
			// then fails: the client is gone, the fact rides the error.
			return nil, &startupDisclosureError{
				err:     fmt.Errorf("initialize MCP server docs: handshake timed out"),
				notices: []string{notice},
			}
		},
	})
	if err != nil {
		t.Fatalf("RegisterTools() error = %v", err)
	}
	defer runtime.Close()

	if skipped := runtime.Skipped(); len(skipped) != 1 {
		t.Fatalf("Skipped() = %#v, want the failure recorded", skipped)
	}
	disclosures := runtime.StartupDisclosures()
	if len(disclosures) != 1 || len(disclosures[0].Notices) != 1 || disclosures[0].Notices[0] != notice {
		t.Fatalf("StartupDisclosures() = %#v, want the launch disclosure kept through the handshake failure", disclosures)
	}
}

// And a plain failure with no launch behind it still discloses nothing, so the
// error path is not just attaching notices to everything.
func TestAPlainConnectFailureDisclosesNothing(t *testing.T) {
	registry := tools.NewRegistry()
	runtime, err := RegisterTools(context.Background(), registry, config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {Type: "stdio", Command: "docs-mcp"},
	}}, RegisterOptions{
		ClientFactory: func(context.Context, Server) (ToolClient, error) {
			return nil, fmt.Errorf("could not start the process")
		},
	})
	if err != nil {
		t.Fatalf("RegisterTools() error = %v", err)
	}
	defer runtime.Close()
	if disclosures := runtime.StartupDisclosures(); len(disclosures) != 0 {
		t.Errorf("StartupDisclosures() = %#v, want none", disclosures)
	}
}
