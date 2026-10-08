package mcp

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/tools"
)

func TestRegisterToolsAddsPromptGatedMCPTools(t *testing.T) {
	registry := tools.NewRegistry()
	client := &fakeToolClient{listed: []RemoteTool{{
		Name:        "lookup",
		Description: "Lookup documentation",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"query": map[string]any{"type": "string"},
			},
		},
	}}}

	runtime, err := RegisterTools(context.Background(), registry, config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {Type: "stdio", Command: "docs-mcp"},
	}}, RegisterOptions{
		ClientFactory: func(context.Context, Server) (ToolClient, error) {
			return client, nil
		},
	})
	if err != nil {
		t.Fatalf("RegisterTools() error = %v", err)
	}
	defer runtime.Close()

	tool, ok := registry.Get("mcp_docs_lookup")
	if !ok {
		t.Fatal("expected mcp_docs_lookup to be registered")
	}
	if tool.Safety().Permission != tools.PermissionPrompt {
		t.Fatalf("Safety.Permission = %q, want prompt", tool.Safety().Permission)
	}
	if tool.Safety().SideEffect != tools.SideEffectNetwork {
		t.Fatalf("Safety.SideEffect = %q, want network", tool.Safety().SideEffect)
	}

	denied := registry.Run(context.Background(), "mcp_docs_lookup", map[string]any{"query": "zero"})
	if denied.Status != tools.StatusError {
		t.Fatalf("Run without approval = %#v, want permission error", denied)
	}
	approved := registry.RunWithOptions(context.Background(), "mcp_docs_lookup", map[string]any{"query": "zero"}, tools.RunOptions{PermissionGranted: true})
	if approved.Status != tools.StatusOK || approved.Output != "lookup: zero" {
		t.Fatalf("approved run = %#v, want lookup output", approved)
	}
	if approved.Meta["mcp.server"] != "docs" || approved.Meta["mcp.tool"] != "lookup" {
		t.Fatalf("approved meta = %#v, want mcp server/tool", approved.Meta)
	}
	if client.closed != 0 {
		t.Fatalf("client.closed before runtime close = %d, want 0", client.closed)
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("Runtime.Close() error = %v", err)
	}
	if client.closed != 1 {
		t.Fatalf("client.closed after runtime close = %d, want 1", client.closed)
	}
}

func TestRegisterToolsPublishesServerToolsInOneGeneration(t *testing.T) {
	registry := tools.NewRegistry()
	before := registry.Snapshot().Generation
	client := &fakeToolClient{listed: []RemoteTool{
		{Name: "lookup", Description: "Lookup documentation"},
		{Name: "search", Description: "Search documentation"},
	}}
	runtime, err := RegisterTools(context.Background(), registry, config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {Type: "stdio", Command: "docs-mcp"},
	}}, RegisterOptions{ClientFactory: func(context.Context, Server) (ToolClient, error) {
		return client, nil
	}})
	if err != nil {
		t.Fatalf("RegisterTools() error = %v", err)
	}
	defer runtime.Close()

	after := registry.Snapshot()
	if after.Generation != before+1 {
		t.Fatalf("registry generation = %d, want %d", after.Generation, before+1)
	}
	for _, name := range []string{"mcp_docs_lookup", "mcp_docs_search"} {
		if _, ok := registry.Get(name); !ok {
			t.Fatalf("expected %s to be published in the batch", name)
		}
	}
}

func TestRegisterToolsPublishesNoneWhenServerToolValidationFails(t *testing.T) {
	registry := tools.NewRegistry()
	before := registry.Snapshot().Generation
	client := &fakeToolClient{listed: []RemoteTool{
		{Name: "lookup", Description: "Lookup documentation"},
		{Name: "", Description: "Invalid nameless tool"},
	}}
	runtime, err := RegisterTools(context.Background(), registry, config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {Type: "stdio", Command: "docs-mcp"},
	}}, RegisterOptions{ClientFactory: func(context.Context, Server) (ToolClient, error) {
		return client, nil
	}})
	if err != nil {
		t.Fatalf("RegisterTools() error = %v", err)
	}
	defer runtime.Close()

	if got := registry.Snapshot().Generation; got != before {
		t.Fatalf("registry generation = %d, want unchanged %d", got, before)
	}
	if _, ok := registry.Get("mcp_docs_lookup"); ok {
		t.Fatal("valid prefix of rejected server batch was published")
	}
	if skipped := runtime.Skipped(); len(skipped) != 1 || skipped[0].Name != "docs" || skipped[0].Err == nil {
		t.Fatalf("Skipped() = %#v, want one validation failure for docs", skipped)
	}
}

func TestRegisterToolsMarksPersistentlyApprovedToolsAllow(t *testing.T) {
	store, err := NewPermissionStore(StoreOptions{
		FilePath: filepath.Join(t.TempDir(), "permissions.json"),
		Now:      func() time.Time { return time.Date(2026, 6, 5, 10, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	servers, err := NormalizeConfig(config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {Type: "stdio", Command: "docs-mcp"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GrantTool(GrantToolInput{
		ServerName:     "docs",
		ServerIdentity: servers[0].Identity,
		ToolName:       "lookup",
		MaxAutonomy:    AutonomyLow,
	}); err != nil {
		t.Fatal(err)
	}

	registry := tools.NewRegistry()
	runtime, err := RegisterTools(context.Background(), registry, config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {Type: "stdio", Command: "docs-mcp"},
	}}, RegisterOptions{
		PermissionStore: store,
		Autonomy:        AutonomyLow,
		ClientFactory: func(context.Context, Server) (ToolClient, error) {
			return &fakeToolClient{listed: []RemoteTool{{Name: "lookup", Description: "Lookup documentation"}}}, nil
		},
	})
	if err != nil {
		t.Fatalf("RegisterTools() error = %v", err)
	}
	defer runtime.Close()

	tool, ok := registry.Get("mcp_docs_lookup")
	if !ok {
		t.Fatal("expected mcp_docs_lookup to be registered")
	}
	if tool.Safety().Permission != tools.PermissionAllow {
		t.Fatalf("Safety.Permission = %q, want allow from persistent MCP grant", tool.Safety().Permission)
	}
}

func TestRegisterToolsSkipsUnreachableServerAndKeepsOthers(t *testing.T) {
	// NormalizeConfig sorts server names, so "alpha" registers before "zebra".
	// "zebra" is unreachable; it must be SKIPPED (recorded in Skipped()), not fatal,
	// and "alpha"'s tools must still register — one bad server can't disable the rest
	// or abort startup. A server still contributes its tools all-or-none.
	registry := tools.NewRegistry()
	alphaClient := &fakeToolClient{listed: []RemoteTool{{Name: "lookup", Description: "Lookup documentation"}}}

	runtime, err := RegisterTools(context.Background(), registry, config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"alpha": {Type: "stdio", Command: "alpha-mcp"},
		"zebra": {Type: "stdio", Command: "zebra-mcp"},
	}}, RegisterOptions{
		ClientFactory: func(_ context.Context, server Server) (ToolClient, error) {
			if server.Name == "zebra" {
				return nil, errors.New("zebra connect failed")
			}
			return alphaClient, nil
		},
	})
	if err != nil {
		t.Fatalf("RegisterTools returned error, want a skip: %v", err)
	}
	defer runtime.Close()
	if _, ok := registry.Get("mcp_alpha_lookup"); !ok {
		t.Fatal("expected the reachable server's tools to register when another is skipped")
	}
	skipped := runtime.Skipped()
	if len(skipped) != 1 || skipped[0].Name != "zebra" {
		t.Fatalf("Skipped() = %#v, want exactly one entry for zebra", skipped)
	}
	if skipped[0].Err == nil {
		t.Fatal("expected the skipped server to record why it was skipped")
	}
}

func TestRegisterToolsFlagsUnconfiguredDefaultOnSkip(t *testing.T) {
	// Exa is seeded by config.DefaultMCPServers with no credentials; when a
	// user never configures it, a connect failure must be recorded as
	// UnconfiguredDefault so callers can avoid
	// warning about a server the user never asked for. "custom" is configured by
	// the user and must NOT be flagged even though it also fails to connect.
	registry := tools.NewRegistry()

	runtime, err := RegisterTools(context.Background(), registry, config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"exa":    config.DefaultMCPServers()["exa"],
		"custom": {Type: "stdio", Command: "custom-mcp"},
	}}, RegisterOptions{
		ClientFactory: func(_ context.Context, server Server) (ToolClient, error) {
			return nil, errors.New(server.Name + " connect failed")
		},
	})
	if err != nil {
		t.Fatalf("RegisterTools returned error, want a skip: %v", err)
	}
	defer runtime.Close()

	byName := make(map[string]SkippedServer)
	for _, skipped := range runtime.Skipped() {
		byName[skipped.Name] = skipped
	}
	exa, ok := byName["exa"]
	if !ok {
		t.Fatalf("Skipped() = %#v, want an entry for exa", runtime.Skipped())
	}
	if !exa.UnconfiguredDefault {
		t.Fatalf("Skipped()[exa] = %#v, want UnconfiguredDefault", exa)
	}
	custom, ok := byName["custom"]
	if !ok {
		t.Fatalf("Skipped() = %#v, want an entry for custom", runtime.Skipped())
	}
	if custom.UnconfiguredDefault {
		t.Fatalf("Skipped()[custom] = %#v, want a user-configured server not flagged", custom)
	}
}

func TestRegisterToolsKeepsPriorStateAndReachableServersWhenOneIsSkipped(t *testing.T) {
	// A tool that predates registration must survive, the reachable server's tools
	// must register, and the unreachable server is skipped (recorded), not fatal.
	registry := tools.NewRegistry()
	registry.Register(&fakePreexistingTool{name: "preexisting"})

	runtime, err := RegisterTools(context.Background(), registry, config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"alpha": {Type: "stdio", Command: "alpha-mcp"},
		"zebra": {Type: "stdio", Command: "zebra-mcp"},
	}}, RegisterOptions{
		ClientFactory: func(_ context.Context, server Server) (ToolClient, error) {
			if server.Name == "zebra" {
				return nil, errors.New("zebra connect failed")
			}
			return &fakeToolClient{listed: []RemoteTool{{Name: "lookup"}}}, nil
		},
	})
	if err != nil {
		t.Fatalf("RegisterTools returned error, want a skip: %v", err)
	}
	defer runtime.Close()
	if _, ok := registry.Get("preexisting"); !ok {
		t.Fatal("expected the pre-existing tool to survive registration")
	}
	if _, ok := registry.Get("mcp_alpha_lookup"); !ok {
		t.Fatal("expected the reachable server's tools to register")
	}
	if skipped := runtime.Skipped(); len(skipped) != 1 || skipped[0].Name != "zebra" {
		t.Fatalf("Skipped() = %#v, want exactly one entry for zebra", skipped)
	}
}

func TestRegisterToolsSkipsServerThatExceedsConnectTimeout(t *testing.T) {
	// A slow server must not block startup: it is abandoned after ConnectTimeout
	// and skipped, while a fast server registers normally. This is the latency fix
	// — one unreachable server can't hold up the first response.
	registry := tools.NewRegistry()
	fastClient := &fakeToolClient{listed: []RemoteTool{{Name: "lookup", Description: "fast"}}}

	runtime, err := RegisterTools(context.Background(), registry, config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"fast": {Type: "stdio", Command: "fast-mcp"},
		"slow": {Type: "stdio", Command: "slow-mcp"},
	}}, RegisterOptions{
		ConnectTimeout: 50 * time.Millisecond,
		ClientFactory: func(ctx context.Context, server Server) (ToolClient, error) {
			if server.Name == "slow" {
				// Block past the timeout, honoring ctx cancellation so the abandoned
				// connect tears down cleanly instead of leaking.
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return fastClient, nil
		},
	})
	if err != nil {
		t.Fatalf("RegisterTools error: %v", err)
	}
	defer runtime.Close()
	if _, ok := registry.Get("mcp_fast_lookup"); !ok {
		t.Fatal("expected the fast server's tools to register despite the slow one timing out")
	}
	skipped := runtime.Skipped()
	if len(skipped) != 1 || skipped[0].Name != "slow" {
		t.Fatalf("Skipped() = %#v, want exactly one entry for slow", skipped)
	}
}

func TestRegistryToolIsDeferredEligible(t *testing.T) {
	client := &fakeToolClient{}
	tool := newRegistryTool(
		Server{Name: "docs", Type: "stdio"},
		RemoteTool{Name: "lookup", Description: "Lookup documentation"},
		client,
		RegisterOptions{},
	)

	// Sanity: the wrapper synthesizes the expected sanitized name so the test
	// exercises the real production path, not a hand-built struct.
	if tool.Name() != "mcp_docs_lookup" {
		t.Fatalf("registryTool.Name() = %q, want mcp_docs_lookup", tool.Name())
	}

	if !tool.Deferred() {
		t.Fatal("registryTool.Deferred() = false, want true (all MCP tools are deferred-eligible)")
	}

	// The exported helper in the tools package must agree via the optional
	// interface, since the agent loop partitions tools through tools.IsDeferred.
	if !tools.IsDeferred(tool) {
		t.Fatal("tools.IsDeferred(registryTool) = false, want true")
	}
}

func TestRegistryToolCanAdvertiseInAutoWithoutPreapproval(t *testing.T) {
	tool := newRegistryTool(
		Server{Name: "docs", Type: "stdio"},
		RemoteTool{Name: "search"},
		&fakeToolClient{},
		RegisterOptions{Autonomy: AutonomyLow, AdvertiseInAuto: true},
	)
	safety := tool.Safety()
	if safety.Permission != tools.PermissionPrompt || !safety.AdvertiseInAuto {
		t.Fatalf("MCP safety = %#v", safety)
	}
}

// TestRegistryToolReportsMCPServerName verifies the registryTool reports its true
// configured server name (not the sanitized tool-name token) so the deferred-tools
// discovery label names a multi-token server correctly via tools.DeferredSource.
func TestRegistryToolReportsMCPServerName(t *testing.T) {
	client := &fakeToolClient{}
	// A server name that sanitizes to a token containing an underscore ("git_hub"):
	// the name-only parser would truncate the label to "git".
	tool := newRegistryTool(
		Server{Name: "git hub", Type: "stdio"},
		RemoteTool{Name: "create_issue", Description: "Create a GitHub issue."},
		client,
		RegisterOptions{},
	)

	if tool.MCPServerName() != "git hub" {
		t.Fatalf("MCPServerName() = %q, want %q", tool.MCPServerName(), "git hub")
	}

	// DeferredSource must prefer the reported server name over the name-derived token.
	if source := tools.DeferredSource(tool); source != "git hub" {
		t.Fatalf("DeferredSource = %q, want %q via MCPServerName()", source, "git hub")
	}
}

// TestRegistryToolNameRoundTripsToServerToken pins the synthesized name format
// for a single-token server so the fallback label parser (mcpServerFromToolName,
// exercised in the tools package) recovers the right server token.
func TestRegistryToolNameRoundTripsToServerToken(t *testing.T) {
	if name := registryToolName("docs", "lookup"); name != "mcp_docs_lookup" {
		t.Fatalf("registryToolName(docs, lookup) = %q, want mcp_docs_lookup", name)
	}
}

type fakePreexistingTool struct {
	name string
}

func (t *fakePreexistingTool) Name() string             { return t.name }
func (t *fakePreexistingTool) Description() string      { return "preexisting tool" }
func (t *fakePreexistingTool) Parameters() tools.Schema { return tools.Schema{} }
func (t *fakePreexistingTool) Safety() tools.Safety     { return tools.Safety{} }
func (t *fakePreexistingTool) Run(context.Context, map[string]any) tools.Result {
	return tools.Result{Status: tools.StatusOK}
}

type fakeToolClient struct {
	listed []RemoteTool
	closed int
}

func (client *fakeToolClient) ListTools(context.Context) ([]RemoteTool, error) {
	return client.listed, nil
}

func (client *fakeToolClient) CallTool(_ context.Context, _ string, args map[string]any) (CallToolResult, error) {
	return CallToolResult{
		Content: []Content{{Type: "text", Text: "lookup: " + args["query"].(string)}},
	}, nil
}

func (client *fakeToolClient) Close() error {
	client.closed++
	return nil
}
