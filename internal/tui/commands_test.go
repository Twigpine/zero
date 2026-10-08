package tui

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/config"
	internalmcp "github.com/Gitlawb/zero/internal/mcp"
	"github.com/Gitlawb/zero/internal/tools"
)

func applyCommandResult(t *testing.T, m model, cmd tea.Cmd) model {
	t.Helper()
	msg := execCmd(cmd)
	if msg == nil {
		t.Fatal("expected command to return a result message")
	}
	updated, _ := m.Update(msg)
	return updated.(model)
}

func TestFormatCommandHelpLinesGroupsCommandsByStableOrder(t *testing.T) {
	lines := formatCommandHelpLines()
	help := strings.Join(lines, "\n")

	groupOrder := []string{"model:", "session:", "runtime:", "tools:", "meta:"}
	lastIndex := -1
	for _, group := range groupOrder {
		index := strings.Index(help, group)
		if index < 0 {
			t.Fatalf("expected grouped help to contain %q, got:\n%s", group, help)
		}
		if index <= lastIndex {
			t.Fatalf("expected %q after previous groups, got:\n%s", group, help)
		}
		lastIndex = index
	}

	for _, want := range []string{
		"model:",
		"  /provider [add|status] - Manage providers: activate, add, edit, delete.",
		"  /model [list|id] - Show or switch the active model.",
		"  /effort [list|level|auto] - Show or set reasoning effort for supported models.",
		"  /fast - Toggle fast mode for supported ChatGPT subscription models.",
		"session:",
		"  /plan [status|on|off] - Show plan status, or enter/exit read-only planning mode.",
		"runtime:",
		"  /permissions - Show the active permission mode and sandbox grants.",
		"  /debug (/debug-mode) - Show debug mode status.",
		"tools:",
		"  /mcp (/mcp-status) - Show MCP server status.",
		"  /search <query> (/find) - Search local session events. Requires a query argument.",
		"meta:",
		"  /exit (/quit) - Exit Zero.",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("expected grouped help to contain %q, got:\n%s", want, help)
		}
	}
}

func TestMCPCommandMetadataAndAutocomplete(t *testing.T) {
	command, ok := resolveCommand("/mcp")
	if !ok {
		t.Fatal("expected /mcp to resolve")
	}
	if command.kind == commandUnknown || command.kind == commandPrompt || command.kind == commandEmpty {
		t.Fatalf("expected /mcp to resolve to a concrete command kind, got %v", command.kind)
	}
	if command.group != commandGroupTools {
		t.Fatalf("expected /mcp in tools group, got %q", command.group)
	}
	if commandSelectionRequiresInput("/mcp") {
		t.Fatal("/mcp should run without required input")
	}

	alias, ok := resolveCommand("/mcp-status")
	if !ok || alias.kind != command.kind {
		t.Fatalf("expected /mcp-status to resolve to MCP command, got ok=%v command=%#v", ok, alias)
	}

	names := listCommandNames()
	for _, want := range []string{"/mcp", "/mcp-status"} {
		if !commandTestStringSliceContains(names, want) {
			t.Fatalf("expected command names to contain %s, got %#v", want, names)
		}
	}

	for _, token := range []string{"/mc", "/mcp-status"} {
		if !commandSuggestionNamesContain(matchCommandSuggestions(token), "/mcp") {
			t.Fatalf("expected autocomplete for %q to surface canonical /mcp", token)
		}
	}
}

func TestMCPCommandRendersConfiguredStateWithoutAgentRun(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(commandTestMCPTool{
		name:        "mcp_docs_lookup",
		serverName:  "docs",
		description: "Look up docs",
		safety: tools.Safety{
			SideEffect: tools.SideEffectNetwork,
			Permission: tools.PermissionPrompt,
		},
	})

	permissionStore, err := internalmcp.NewPermissionStore(internalmcp.StoreOptions{
		FilePath: filepath.Join(t.TempDir(), "mcp-permissions.json"),
		Now:      func() time.Time { return time.Date(2026, 6, 13, 9, 30, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewPermissionStore returned error: %v", err)
	}
	if _, err := permissionStore.GrantServer(internalmcp.GrantServerInput{
		ServerName:     "docs",
		ServerIdentity: "docs-identity",
		MaxAutonomy:    internalmcp.AutonomyLow,
	}); err != nil {
		t.Fatalf("GrantServer returned error: %v", err)
	}
	if _, err := permissionStore.GrantTool(internalmcp.GrantToolInput{
		ServerName:     "github",
		ServerIdentity: "github-identity",
		ToolName:       "create_issue",
		MaxAutonomy:    internalmcp.AutonomyMedium,
	}); err != nil {
		t.Fatalf("GrantTool returned error: %v", err)
	}

	tokenStore, err := internalmcp.NewTokenStore(internalmcp.TokenStoreOptions{
		FilePath: filepath.Join(t.TempDir(), "mcp-oauth.json"),
		Now:      func() time.Time { return time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewTokenStore returned error: %v", err)
	}
	if err := tokenStore.Save("github", internalmcp.StoredToken{
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		TokenType:    "Bearer",
		Scopes:       []string{"issues:read", "issues:write"},
		ExpiresAt:    time.Date(2026, 6, 13, 11, 45, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("Save token returned error: %v", err)
	}

	m := newModel(context.Background(), Options{
		Registry:       registry,
		PermissionMode: agent.PermissionModeAsk,
	})
	m.mcpConfig = config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {
			Type:    "stdio",
			Command: "zero-docs-mcp",
			Args:    []string{"--workspace", "."},
		},
		"github": {
			Type: "http",
			URL:  "https://mcp.github.example",
			Auth: "oauth",
			OAuth: &config.MCPOAuthConfig{
				Scopes: []string{"issues:read", "issues:write"},
			},
		},
	}}
	m.mcpPermissionStore = permissionStore
	m.mcpTokenStore = tokenStore
	m.width = 220
	m.height = 42
	m.input.SetValue("/mcp")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd != nil {
		t.Fatal("expected /mcp to be handled without starting an agent run")
	}
	if next.pending || next.activeRunID != 0 || next.runID != 0 {
		t.Fatalf("expected /mcp not to mutate agent run state, pending=%v activeRunID=%d runID=%d", next.pending, next.activeRunID, next.runID)
	}
	if next.mcpManager == nil {
		t.Fatal("expected /mcp to open the selectable MCP manager")
	}
	if len(next.transcript) != len(m.transcript) {
		t.Fatalf("/mcp should open a manager overlay without appending transcript rows; before=%d after=%d", len(m.transcript), len(next.transcript))
	}
	text := plainRender(t, next.View())
	for _, want := range []string{
		"Manage MCP servers",
		"2 servers",
		"User MCPs",
		"docs",
		"enabled",
		"github",
		"oauth",
		"Add MCP server",
		"Add local stdio MCP",
		"List configured",
		"d disable",
		"r remove",
		"Enter action",
		"Esc close",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected clean MCP manager overlay to contain %q, got:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{
		"lookup [network/prompt]",
		"persistent grants:",
		"server grants:",
		"OAuth",
		"add: zero mcp add",
		"disconnect: zero mcp disable",
	} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("MCP manager overlay should not include old status report text %q:\n%s", unwanted, text)
		}
	}
}

func TestMCPManagerNavigationOpensAddWizard(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.input.SetValue("/mcp")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd != nil {
		t.Fatal("expected /mcp to open synchronously")
	}
	if next.mcpManager == nil {
		t.Fatal("expected MCP manager to open")
	}

	updated, cmd = next.Update(testKeyText("custom remote"))
	next = updated.(model)
	if cmd != nil {
		t.Fatal("expected MCP manager search to update synchronously")
	}

	updated, cmd = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if cmd != nil {
		t.Fatal("expected MCP manager selection to prefill synchronously")
	}
	if next.mcpManager != nil {
		t.Fatal("expected add command selection to close the MCP manager")
	}
	if next.mcpAddWizard == nil {
		t.Fatal("expected add command selection to open the MCP wizard")
	}
	if got := next.input.Value(); got != "" {
		t.Fatalf("composer = %q, want empty while wizard is active", got)
	}
}

func TestMCPManagerSearchFiltersMarketplace(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.width = 120
	m.height = 36
	m.input.SetValue("/mcp")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	updated, cmd := next.Update(testKeyText("playwright"))
	next = updated.(model)
	if cmd != nil {
		t.Fatal("expected search typing to be synchronous")
	}

	view := plainRender(t, next.View())
	for _, want := range []string{
		"search > playwright",
		"Marketplace",
		"Playwright",
		"@playwright/mcp",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("MCP marketplace search missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "Filesystem") {
		t.Fatalf("MCP marketplace search should filter non-matching entries:\n%s", view)
	}
}

func TestMCPManagerDeleteEditsSearchQuery(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.input.SetValue("/mcp")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	updated, _ = next.Update(testKeyText("play"))
	next = updated.(model)
	updated, cmd := next.Update(testKey(tea.KeyDelete))
	next = updated.(model)

	if cmd != nil {
		t.Fatal("expected Delete in MCP search to update synchronously")
	}
	if next.mcpManager == nil || next.mcpManager.query != "pla" {
		t.Fatalf("query after Delete = %#v, want pla", next.mcpManager)
	}
}

func TestMCPManagerMarketplaceSelectionPrefillsInstallCommand(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.input.SetValue("/mcp")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	updated, _ = next.Update(testKeyText("context7"))
	next = updated.(model)
	updated, cmd := next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if cmd != nil {
		t.Fatal("expected marketplace selection to prefill synchronously")
	}
	if next.mcpManager != nil {
		t.Fatal("expected marketplace install selection to close manager")
	}
	if got, want := next.input.Value(), "/mcp add context7 --url https://mcp.context7.com/mcp"; got != want {
		t.Fatalf("composer = %q, want %q", got, want)
	}
}

func TestMCPManagerAddRemoteOpensWizard(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.width = 120
	m.height = 36
	m.input.SetValue("/mcp")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	updated, cmd := next.Update(testKeyAltText("a"))
	next = updated.(model)

	if cmd != nil {
		t.Fatal("expected MCP add wizard to open synchronously")
	}
	if next.mcpManager != nil {
		t.Fatal("expected MCP manager to close when add wizard opens")
	}
	if next.mcpAddWizard == nil {
		t.Fatal("expected MCP add wizard to be active")
	}
	view := plainRender(t, next.View())
	for _, want := range []string{
		"Add MCP Server",
		"Server Name",
		"HTTP remote",
		"Enter continue",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("MCP add wizard missing %q:\n%s", want, view)
		}
	}
}

func TestMCPAddWizardInvalidURLShowsUnsavedResult(t *testing.T) {
	m := newModel(context.Background(), Options{
		MCPCommand: func(_ context.Context, args []string) MCPCommandResult {
			t.Fatalf("MCPCommand should not run for invalid URL, got %#v", args)
			return MCPCommandResult{}
		},
	})
	m.width = 120
	m.height = 36
	m.mcpAddWizard = newMCPAddWizard("http")

	for _, key := range []tea.Msg{
		testKeyText("adds"),
		testKey(tea.KeyEnter),
		testKey(tea.KeyEnter),
		testKeyText("sxas"),
		testKey(tea.KeyEnter),
	} {
		updated, cmd := m.Update(key)
		if cmd != nil {
			t.Fatal("expected MCP add wizard input to update synchronously")
		}
		m = updated.(model)
	}

	if m.mcpAddWizard == nil {
		t.Fatal("expected wizard result to stay visible")
	}
	view := plainRender(t, m.View())
	for _, want := range []string{
		"MCP setup issue",
		"adds",
		"HTTP remote",
		"URL could not be parsed",
		"No config was saved yet.",
		"Edit URL",
		"Save disabled",
		"Discard",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("invalid URL result missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "Disable server") {
		t.Fatalf("unsaved invalid result should not offer Disable server:\n%s", view)
	}
}

func TestMCPAddWizardInvalidURLCanSaveDisabledDraft(t *testing.T) {
	var called []string
	m := newModel(context.Background(), Options{
		MCPCommand: func(_ context.Context, args []string) MCPCommandResult {
			called = append([]string{}, args...)
			return MCPCommandResult{
				ExitCode: 0,
				Output:   "Added MCP server draft.",
				Config: config.MCPConfig{Servers: map[string]config.MCPServerConfig{
					"draft": {Type: "http", URL: "sxas", Disabled: true},
				}},
			}
		},
	})
	m.width = 120
	m.height = 36
	m.mcpAddWizard = newMCPAddWizard("http")

	var cmd tea.Cmd
	for index, key := range []tea.Msg{
		testKeyText("draft"),
		testKey(tea.KeyEnter),
		testKey(tea.KeyEnter),
		testKeyText("sxas"),
		testKey(tea.KeyEnter),
		testKeyText("s"),
	} {
		updated, nextCmd := m.Update(key)
		if nextCmd != nil && index != 5 {
			t.Fatal("expected MCP add wizard input to update synchronously")
		}
		m = updated.(model)
		cmd = nextCmd
	}
	if cmd == nil {
		t.Fatal("expected save disabled action to run asynchronously")
	}
	m = applyCommandResult(t, m, cmd)

	want := []string{"add", "draft", "--type", "http", "--disabled", "--url", "sxas"}
	if !reflect.DeepEqual(called, want) {
		t.Fatalf("MCPCommand args = %#v, want %#v", called, want)
	}
	if !m.mcpConfig.Servers["draft"].Disabled {
		t.Fatalf("draft server was not saved disabled: %#v", m.mcpConfig.Servers["draft"])
	}
	view := plainRender(t, m.View())
	for _, want := range []string{"MCP server saved", "disabled", "Edit config"} {
		if !strings.Contains(view, want) {
			t.Fatalf("disabled draft result missing %q:\n%s", want, view)
		}
	}
}

func TestMCPAddWizardSavesRemoteWithPastedHeader(t *testing.T) {
	var called []string
	m := newModel(context.Background(), Options{
		MCPCommand: func(_ context.Context, args []string) MCPCommandResult {
			called = append([]string{}, args...)
			return MCPCommandResult{
				ExitCode: 0,
				Output:   "Added MCP server docs.",
				Config: config.MCPConfig{Servers: map[string]config.MCPServerConfig{
					"docs": {Type: "http", URL: "https://docs.example/mcp", Headers: map[string]string{"Authorization": "Bearer secret"}},
				}},
			}
		},
	})
	m.width = 120
	m.height = 36
	m.mcpAddWizard = newMCPAddWizard("http")

	var cmd tea.Cmd
	for index, key := range []tea.Msg{
		testKeyText("docs"),
		testKey(tea.KeyEnter),
		testKey(tea.KeyEnter),
		testKeyText("https://docs.example/mcp"),
		testKey(tea.KeyEnter),
		testKeyText("Authorization: Bearer secret"),
		testKey(tea.KeyEnter),
		testKey(tea.KeyEnter),
	} {
		updated, nextCmd := m.Update(key)
		if nextCmd != nil && index != 7 {
			t.Fatal("expected MCP add wizard input to update synchronously")
		}
		m = updated.(model)
		cmd = nextCmd
	}
	if cmd == nil {
		t.Fatal("expected MCP add wizard save to run asynchronously")
	}
	m = applyCommandResult(t, m, cmd)

	want := []string{"add", "docs", "--type", "http", "--url", "https://docs.example/mcp", "--header", "Authorization=Bearer secret"}
	if !reflect.DeepEqual(called, want) {
		t.Fatalf("MCPCommand args = %#v, want %#v", called, want)
	}
	if m.mcpAddWizard == nil {
		t.Fatal("expected saved result card to stay visible")
	}
	view := plainRender(t, m.View())
	for _, want := range []string{
		"MCP server ready",
		"docs",
		"connected",
		"Use server",
		"Manage tools",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("saved result missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "Bearer secret") {
		t.Fatalf("saved result leaked header secret:\n%s", view)
	}
}

func TestChatMCPSetupStitchURLOpensPrefilledWizard(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.width = 120
	m.height = 36
	m.input.SetValue("configure this MCP https://stitch.withgoogle.com/docs/mcp/setup")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd != nil {
		t.Fatal("expected MCP setup intent to open synchronously")
	}
	if next.pending {
		t.Fatal("MCP setup intent should not start an agent run before confirmation")
	}
	if next.mcpAddWizard == nil {
		t.Fatal("expected MCP setup intent to open add wizard")
	}
	if next.mcpAddWizard.step != mcpAddWizardStepConfirm {
		t.Fatalf("wizard step = %v, want confirm", next.mcpAddWizard.step)
	}
	view := plainRender(t, next.View())
	for _, want := range []string{
		"Add MCP Server",
		"Review setup",
		"stitch",
		"STDIO",
		"npx -y @_davideast/stitch-mcp@latest proxy",
		"Google Cloud auth",
		"stitch.withgoogle.com/docs/mcp/setup",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("Stitch setup wizard missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(transcriptText(next.transcript), "No provider configured") {
		t.Fatalf("MCP setup intent should not fall through to provider error:\n%s", transcriptText(next.transcript))
	}
}

func TestChatMCPSetupFalsePositiveSendsPrompt(t *testing.T) {
	provider := &fakeProvider{}
	m := newModel(context.Background(), Options{
		Provider: provider,
		Registry: tools.NewRegistry(),
	})
	m.width = 120
	m.height = 36
	prompt := "how do I add a fetch call to my mcp client?"
	m.input.SetValue(prompt)

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if next.mcpAddWizard != nil {
		t.Fatal("ordinary MCP chat prompt should not open the add wizard")
	}
	if cmd == nil {
		t.Fatal("ordinary MCP chat prompt should be sent to the agent")
	}
	if !transcriptContains(next.transcript, prompt) {
		t.Fatalf("expected original prompt in transcript, got %#v", next.transcript)
	}
	_ = applyCommandResult(t, next, cmd)
	foundPrompt := false
	for _, request := range provider.requests {
		for _, message := range request.Messages {
			if strings.Contains(message.Content, prompt) {
				foundPrompt = true
				break
			}
		}
	}
	if !foundPrompt {
		t.Fatalf("expected prompt to reach provider, got %#v", provider.requests)
	}
}

func TestMCPAddWizardOpenCancelsStaleManagerResult(t *testing.T) {
	m := newModel(context.Background(), Options{
		MCPCommand: func(_ context.Context, _ []string) MCPCommandResult {
			return MCPCommandResult{
				ExitCode: 0,
				Output:   "checked",
				Config:   config.MCPConfig{Servers: map[string]config.MCPServerConfig{}},
			}
		},
	})
	m.mcpManager = &mcpManagerState{selected: 1, query: "docs"}
	var cmd tea.Cmd
	m, cmd = m.startMCPCommand(mcpCommandRequest{
		origin:          mcpCommandOriginManager,
		args:            []string{"check", "docs"},
		managerSelected: 1,
		managerQuery:    "docs",
	})
	if cmd == nil {
		t.Fatal("expected manager command")
	}

	m = m.openMCPAddWizard("http")
	if m.mcpAddWizard == nil {
		t.Fatal("expected add wizard to open")
	}
	if m.mcpManager != nil {
		t.Fatal("expected manager to close when add wizard opens")
	}
	resultMsg := execCmd(cmd)
	msg, ok := resultMsg.(mcpCommandResultMsg)
	if !ok {
		t.Fatalf("expected mcpCommandResultMsg, got %T", resultMsg)
	}
	m = m.applyMCPCommandResultMessage(msg)
	if m.mcpManager != nil {
		t.Fatal("stale manager result should not reopen manager over wizard")
	}
	if m.mcpAddWizard == nil {
		t.Fatal("stale manager result should leave wizard open")
	}
}

func TestMCPAddWizardConfirmRedactsSensitiveSourceAndEndpoint(t *testing.T) {
	wizard := newMCPAddWizard("http")
	wizard.step = mcpAddWizardStepConfirm
	wizard.serverName = "remote"
	wizard.serverType = "http"
	wizard.endpoint = "https://user:password@remote.example/mcp?access_token=endpoint-secret&safe=value#api_key=fragment-secret"
	wizard.sourceLabel = "Remote Docs"
	wizard.sourceURL = "https://docs.example/setup?api_key=docs-secret"
	wizard.headerKey = "Authorization"

	got := plainRender(t, wizard.render(120))
	for _, leaked := range []string{"user:password", "endpoint-secret", "fragment-secret", "docs-secret"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("wizard confirm leaked %q in:\n%s", leaked, got)
		}
	}
	for _, want := range []string{"remote.example", "safe=value", "access_token=[REDACTED]", "api_key=[REDACTED]", "Authorization=[REDACTED]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("wizard confirm missing redacted context %q in:\n%s", want, got)
		}
	}
}

func TestChatMCPSetupStitchConfirmSavesServer(t *testing.T) {
	var called []string
	m := newModel(context.Background(), Options{
		MCPCommand: func(_ context.Context, args []string) MCPCommandResult {
			called = append([]string{}, args...)
			return MCPCommandResult{
				ExitCode: 0,
				Output:   "Added MCP server stitch.",
				Config: config.MCPConfig{Servers: map[string]config.MCPServerConfig{
					"stitch": {Type: "stdio", Command: "npx", Args: []string{"-y", "@_davideast/stitch-mcp@latest", "proxy"}},
				}},
			}
		},
	})
	m.width = 120
	m.height = 36
	m.input.SetValue("setup stitch mcp from https://stitch.withgoogle.com/docs/mcp/setup")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd != nil {
		t.Fatal("expected MCP setup intent to open synchronously")
	}

	updated, cmd = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if cmd == nil {
		t.Fatal("expected MCP setup save to run asynchronously")
	}
	next = applyCommandResult(t, next, cmd)

	want := []string{"add", "stitch", "--type", "stdio", "--", "npx", "-y", "@_davideast/stitch-mcp@latest", "proxy"}
	if !reflect.DeepEqual(called, want) {
		t.Fatalf("MCPCommand args = %#v, want %#v", called, want)
	}
	if _, ok := next.mcpConfig.Servers["stitch"]; !ok {
		t.Fatalf("stitch server was not applied to TUI state: %#v", next.mcpConfig.Servers)
	}
	view := plainRender(t, next.View())
	for _, want := range []string{"MCP server ready", "stitch", "connected", "Use server"} {
		if !strings.Contains(view, want) {
			t.Fatalf("saved Stitch result missing %q:\n%s", want, view)
		}
	}
}

func TestMCPManagerRunsSelectedServerAction(t *testing.T) {
	var called []string
	m := newModel(context.Background(), Options{
		PermissionMode: agent.PermissionModeAsk,
		MCPConfig: config.MCPConfig{Servers: map[string]config.MCPServerConfig{
			"docs": {Type: "stdio", Command: "zero-docs-mcp"},
		}},
		MCPCommand: func(_ context.Context, args []string) MCPCommandResult {
			called = append([]string{}, args...)
			return MCPCommandResult{
				ExitCode: 0,
				Output:   "MCP server docs is now disabled.",
				Config: config.MCPConfig{Servers: map[string]config.MCPServerConfig{
					"docs": {Type: "stdio", Command: "zero-docs-mcp", Disabled: true},
				}},
			}
		},
	})
	m.input.SetValue("/mcp")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if next.mcpManager == nil {
		t.Fatal("expected MCP manager to open")
	}

	updated, cmd := next.Update(testKeyAltText("d"))
	next = updated.(model)
	if cmd == nil {
		t.Fatal("expected MCP action to run asynchronously")
	}
	if called != nil {
		t.Fatalf("MCPCommand ran during Update; called=%#v", called)
	}
	next = applyCommandResult(t, next, cmd)
	if !reflect.DeepEqual(called, []string{"disable", "docs"}) {
		t.Fatalf("MCPCommand args = %#v, want disable docs", called)
	}
	if !next.mcpConfig.Servers["docs"].Disabled {
		t.Fatalf("docs server was not disabled in TUI state: %#v", next.mcpConfig.Servers["docs"])
	}
	text := transcriptText(next.transcript)
	for _, want := range []string{
		"MCP action complete",
		"MCP server docs is now disabled.",
		"docs",
		"disabled",
		"stdio",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("MCP manager action output missing %q:\n%s", want, text)
		}
	}
}

func TestMCPCommandRunsManagerActionAndRefreshesState(t *testing.T) {
	m := newModel(context.Background(), Options{
		PermissionMode: agent.PermissionModeAsk,
		MCPConfig: config.MCPConfig{Servers: map[string]config.MCPServerConfig{
			"docs": {Type: "stdio", Command: "zero-docs-mcp"},
		}},
		MCPCommand: func(_ context.Context, args []string) MCPCommandResult {
			if !reflect.DeepEqual(args, []string{"disable", "docs"}) {
				t.Fatalf("MCPCommand args = %#v, want disable docs", args)
			}
			return MCPCommandResult{
				ExitCode: 0,
				Output:   "MCP server docs is now disabled.",
				Config: config.MCPConfig{Servers: map[string]config.MCPServerConfig{
					"docs": {Type: "stdio", Command: "zero-docs-mcp", Disabled: true},
				}},
			}
		},
	})
	m.input.SetValue("/mcp disable docs")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd == nil {
		t.Fatal("expected /mcp disable to run asynchronously")
	}
	next = applyCommandResult(t, next, cmd)
	if !next.mcpConfig.Servers["docs"].Disabled {
		t.Fatalf("docs server was not disabled in TUI state: %#v", next.mcpConfig.Servers["docs"])
	}
	text := transcriptText(next.transcript)
	for _, want := range []string{
		"MCP action complete",
		"MCP server docs is now disabled.",
		"docs · disabled · stdio",
		"zero mcp enable docs",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("/mcp action text missing %q:\n%s", want, text)
		}
	}
}

func TestMCPCommandPreservesQuotedArguments(t *testing.T) {
	var called []string
	m := newModel(context.Background(), Options{
		MCPCommand: func(_ context.Context, args []string) MCPCommandResult {
			called = append([]string{}, args...)
			return MCPCommandResult{
				ExitCode: 0,
				Output:   "Added MCP server docs.",
				Config: config.MCPConfig{Servers: map[string]config.MCPServerConfig{
					"docs": {Type: "stdio", Command: `C:\Program Files\docs mcp.exe`, Args: []string{"--label", "Zero Docs"}},
				}},
			}
		},
	})
	m.input.SetValue(`/mcp add docs -- "C:\Program Files\docs mcp.exe" --label "Zero Docs"`)

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd == nil {
		t.Fatal("expected /mcp add to run asynchronously")
	}
	next = applyCommandResult(t, next, cmd)
	want := []string{"add", "docs", "--", `C:\Program Files\docs mcp.exe`, "--label", "Zero Docs"}
	if !reflect.DeepEqual(called, want) {
		t.Fatalf("MCPCommand args = %#v, want %#v", called, want)
	}
	if got := next.mcpConfig.Servers["docs"].Command; got != `C:\Program Files\docs mcp.exe` {
		t.Fatalf("persisted command = %q", got)
	}
}

func TestMCPCommandDoesNotApplyFailedConfig(t *testing.T) {
	m := newModel(context.Background(), Options{
		MCPConfig: config.MCPConfig{Servers: map[string]config.MCPServerConfig{
			"docs": {Type: "stdio", Command: "zero-docs-mcp"},
		}},
		MCPCommand: func(_ context.Context, args []string) MCPCommandResult {
			return MCPCommandResult{
				ExitCode: 1,
				Error:    "permission denied",
				Config:   config.MCPConfig{},
			}
		},
	})
	m.input.SetValue("/mcp disable docs")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd == nil {
		t.Fatal("expected failed /mcp command to run asynchronously")
	}
	next = applyCommandResult(t, next, cmd)
	if _, ok := next.mcpConfig.Servers["docs"]; !ok {
		t.Fatalf("failed MCP command cleared existing config: %#v", next.mcpConfig.Servers)
	}
	text := transcriptText(next.transcript)
	if !strings.Contains(text, "MCP action failed") || !strings.Contains(text, "permission denied") {
		t.Fatalf("missing failure output:\n%s", text)
	}
}

type commandTestMCPTool struct {
	name        string
	serverName  string
	description string
	safety      tools.Safety
}

func (tool commandTestMCPTool) Name() string {
	return tool.name
}

func (tool commandTestMCPTool) Description() string {
	return tool.description
}

func (tool commandTestMCPTool) Parameters() tools.Schema {
	return tools.Schema{Type: "object"}
}

func (tool commandTestMCPTool) Safety() tools.Safety {
	return tool.safety
}

func (tool commandTestMCPTool) Run(context.Context, map[string]any) tools.Result {
	return tools.Result{Status: tools.StatusOK}
}

func (tool commandTestMCPTool) MCPServerName() string {
	return tool.serverName
}

func commandTestStringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func commandSuggestionNamesContain(suggestions []commandSuggestion, want string) bool {
	for _, suggestion := range suggestions {
		if suggestion.Name == want {
			return true
		}
	}
	return false
}

func TestParseImageCommand(t *testing.T) {
	cases := []struct {
		input string
		kind  commandKind
		text  string
	}{
		{input: "/image photo.png", kind: commandImage, text: "photo.png"},
		{input: "/image ./a b.png", kind: commandImage, text: "./a b.png"},
		{input: "/image clear", kind: commandImage, text: "clear"},
		{input: "/image", kind: commandImage, text: ""},
	}
	for _, tc := range cases {
		got := parseCommand(tc.input)
		if got.kind != tc.kind || got.text != tc.text {
			t.Fatalf("%q: got kind=%v text=%q, want kind=%v text=%q", tc.input, got.kind, got.text, tc.kind, tc.text)
		}
	}
}

func TestInputStyleCommandRemoved(t *testing.T) {
	// AUDIT-H3: /input-style was an inert stub (registered + in /help + autocompleted,
	// but only printed "no backend setting yet"). It must no longer be a known command.
	if got := parseCommand("/input-style"); got.kind != commandUnknown {
		t.Fatalf("/input-style should be unknown after removal, got kind=%v", got.kind)
	}
	for _, c := range commandDefinitions {
		if c.name == "/input-style" {
			t.Fatal("/input-style must be gone from the command registry")
		}
	}
	if strings.Contains(formatGroupedCommandHelp(), "/input-style") {
		t.Fatal("/help must not advertise /input-style")
	}
}

func TestParseBackgroundTerminalCommands(t *testing.T) {
	cases := []struct {
		input string
		kind  commandKind
		text  string
	}{
		{input: "/ps", kind: commandPS, text: ""},
		{input: "/stop", kind: commandStop, text: ""},
		{input: "/stop 1000", kind: commandStop, text: "1000"},
		{input: "/attach", kind: commandAttach, text: ""},
		{input: "/attach 7", kind: commandAttach, text: "7"},
	}
	for _, tc := range cases {
		got := parseCommand(tc.input)
		if got.kind != tc.kind || got.text != tc.text {
			t.Fatalf("%q: got kind=%v text=%q, want kind=%v text=%q", tc.input, got.kind, got.text, tc.kind, tc.text)
		}
	}
}

// /undo is an alias for /rewind (issue #698): previously it fell through to
// commandUnknown.
func TestUndoAliasesRewind(t *testing.T) {
	rewind, ok := resolveCommand("/rewind")
	if !ok {
		t.Fatal("expected /rewind to resolve")
	}
	alias, ok := resolveCommand("/undo")
	if !ok || alias.kind != rewind.kind {
		t.Fatalf("expected /undo to alias /rewind, got ok=%v command=%#v", ok, alias)
	}
	// End-to-end: a typed /undo dispatches to the rewind handler with args intact,
	// for both the "latest" and numeric-<sequence> forms of the /rewind contract.
	if parsed := parseCommand("/undo latest"); parsed.kind != commandRewind || parsed.text != "latest" {
		t.Fatalf("parseCommand(%q) = %#v, want commandRewind with text=latest", "/undo latest", parsed)
	}
	if parsed := parseCommand("/undo 123"); parsed.kind != commandRewind || parsed.text != "123" {
		t.Fatalf("parseCommand(%q) = %#v, want commandRewind with text=123", "/undo 123", parsed)
	}
	// Guard: without the alias it would be commandUnknown.
	if parsed := parseCommand("/undo"); parsed.kind == commandUnknown {
		t.Fatal("/undo resolved to commandUnknown — the alias is not wired")
	}
}

func TestCommandSelectionRequiresInputFromUsage(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{name: "/spec", want: true},
		{name: "/search", want: true},
		{name: "/find", want: true},
		{name: "/image", want: true},
		{name: "/rewind", want: false},
		{name: "/model", want: false},
		{name: "/help", want: false},
	}
	for _, tc := range cases {
		if got := commandSelectionRequiresInput(tc.name); got != tc.want {
			t.Fatalf("commandSelectionRequiresInput(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCommandRequiredInputHintFromUsage(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{name: "/spec", want: "[task]"},
		{name: "/search", want: "[query]"},
		{name: "/find", want: "[query]"},
		{name: "/image", want: "[path]"},
		{name: "/model", want: ""},
	}
	for _, tc := range cases {
		if got := commandRequiredInputHint(tc.name); got != tc.want {
			t.Fatalf("commandRequiredInputHint(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestImageCommandIsDiscoverable(t *testing.T) {
	found := false
	for _, name := range listCommandNames() {
		if name == "/image" {
			found = true
		}
	}
	if !found {
		t.Fatal("/image should be listed so it appears in help and autocomplete")
	}
}
