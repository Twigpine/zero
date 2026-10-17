package tui

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/mcp"
)

// AN OPAQUE PATH SEGMENT IS CREDENTIAL MATERIAL.
//
// Query values and userinfo were treated as secret-bearing while the path was
// assumed to be an identifier, and the configuration contract accepts an
// arbitrary HTTP or SSE path. Opaque path-segment credentials are an ordinary
// endpoint convention.
//
// This needs no crafted response body. A failing http.Client.Do returns a
// *url.Error carrying the request URL, which the failed-server path wraps and
// renders, so the token reaches the Error field, the panel and the transcript,
// with the Target row showing it as well.
func TestAnOpaquePathSegmentIsRedactedEverywhere(t *testing.T) {
	const token = "9f3c2b7ae1d84c6fa0b5"
	endpoint := "https://host.invalid/mcp/" + token + "/sse"

	// The shape a real connection failure produces.
	failure := &url.Error{Op: "Post", URL: endpoint, Err: errors.New("dial tcp: connection refused")}

	state := BuildMCPViewState(MCPStateOptions{
		Config: config.MCPConfig{
			Servers: map[string]config.MCPServerConfig{"docs": {URL: endpoint, Type: "sse"}},
		},
		Skipped: []mcp.SkippedServer{{Name: "docs", Err: failure}},
	})

	for _, server := range state.Servers {
		if server.Name != "docs" {
			continue
		}
		if strings.Contains(server.Error, token) {
			t.Errorf("the path credential reached the failure reason:\n%s", server.Error)
		}
		if strings.Contains(server.Target, token) {
			t.Errorf("the path credential reached the target row:\n%s", server.Target)
		}
		// The route shape survives, or the operator cannot tell which endpoint
		// failed.
		if !strings.Contains(server.Target, "host.invalid") {
			t.Errorf("the target lost the host, so the diagnostic is gone too: %s", server.Target)
		}
		return
	}
	t.Fatal("the failed server is missing from the assembled state")
}

// Short route segments are structure, not secrets, and must survive. Redacting
// them would eat the diagnostic while protecting nothing.
func TestOrdinaryRouteSegmentsSurvive(t *testing.T) {
	endpoint := "https://host.invalid/v1/sse"
	state := BuildMCPViewState(MCPStateOptions{
		Config: config.MCPConfig{
			Servers: map[string]config.MCPServerConfig{"docs": {URL: endpoint, Type: "sse"}},
		},
		Skipped: []mcp.SkippedServer{{Name: "docs", Err: errors.New("connection refused")}},
	})
	for _, server := range state.Servers {
		if server.Name != "docs" {
			continue
		}
		for _, want := range []string{"v1", "sse"} {
			if !strings.Contains(server.Target, want) {
				t.Errorf("the route segment %q was redacted out of %s", want, server.Target)
			}
		}
		return
	}
	t.Fatal("the failed server is missing from the assembled state")
}
