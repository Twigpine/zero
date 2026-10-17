package tui

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/sandbox"
	"github.com/Twigpine/zero/internal/tools"
)

func TestHelpCommandRendersGroupedSections(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.input.SetValue("/help")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd != nil {
		t.Fatal("expected /help to be handled without starting an agent run")
	}
	text := transcriptText(next.transcript)
	for _, want := range []string{
		"Commands",
		"Model",
		"Session",
		"Runtime",
		"Tools",
		"Meta",
		"  /model [list|id]",
		"  /permissions",
		"hint: submit plain text to ask Zero",
	} {
		assertContains(t, text, want)
	}
	assertNotContains(t, text, "Commands:\n/provider")
}

func TestProviderAndConfigCommandsUseStableStatusOutput(t *testing.T) {
	m := newModel(context.Background(), Options{
		ProviderName: "openai",
		ModelName:    "gpt-4.1",
		ProviderProfile: config.ProviderProfile{
			Name:         "openai",
			ProviderKind: config.ProviderKindOpenAI,
			BaseURL:      config.OpenAIBaseURL,
			APIKey:       "sk-sensitive",
			Model:        "gpt-4.1",
		},
		AgentOptions:  agent.Options{MaxTurns: 42},
		RecapsEnabled: true,
	})

	m.input.SetValue("/provider status")
	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd != nil {
		t.Fatal("expected /provider to be handled without starting an agent run")
	}
	providerText := transcriptText(next.transcript)
	for _, want := range []string{"Provider", "status: ok", "provider: openai", "model: gpt-4.1", "api key: set"} {
		assertContains(t, providerText, want)
	}
	assertNotContains(t, providerText, "sk-sensitive")

	next.input.SetValue("/config")
	updated, cmd = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if cmd != nil {
		t.Fatal("expected /config to be handled without starting an agent run")
	}
	configText := transcriptText(next.transcript)
	for _, want := range []string{"Config", "status: ok", "runtime: go", "max turns: 42", "permission mode:", "recaps: on"} {
		assertContains(t, configText, want)
	}
	assertNotContains(t, configText, "sk-sensitive")
}

func TestProviderCommandRedactsCredentialBearingBaseURL(t *testing.T) {
	m := newModel(context.Background(), Options{
		ProviderName: "openai",
		ModelName:    "gpt-4.1",
		ProviderProfile: config.ProviderProfile{
			Name:         "openai",
			ProviderKind: config.ProviderKindOpenAI,
			BaseURL:      "https://user:super-secret@proxy.local/v1?api_key=query-secret",
			APIKey:       "query-secret",
			Model:        "gpt-4.1",
		},
	})
	m.input.SetValue("/provider status")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd != nil {
		t.Fatal("expected /provider to be handled without starting an agent run")
	}
	text := transcriptText(next.transcript)
	for _, unwanted := range []string{"super-secret", "query-secret", "user:super-secret@"} {
		assertNotContains(t, text, unwanted)
	}
	assertContains(t, text, "base url: https://proxy.local/v1?api_key=[REDACTED]")
}

func TestToolsCommandRendersCommandCard(t *testing.T) {
	m := newModel(context.Background(), Options{
		Registry: tools.NewRegistry(),
	})
	m.input.SetValue("/tools")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd != nil {
		t.Fatal("expected /tools to be handled without starting an agent run")
	}
	emptyText := transcriptText(next.transcript)
	for _, want := range []string{
		"Tools",
		"0 registered | no tools available",
		"Registry",
		"registered  0",
		"actions: /mcp manage servers | /permissions manage access",
	} {
		assertContains(t, emptyText, want)
	}
	assertNotContains(t, emptyText, "status: warning")
	assertNotContains(t, emptyText, "registered tools:")

	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(".", nil))
	m = newModel(context.Background(), Options{
		Registry: registry,
	})
	m.input.SetValue("/tools")

	updated, cmd = m.Update(testKey(tea.KeyEnter))
	next = updated.(model)

	if cmd != nil {
		t.Fatal("expected /tools to be handled without starting an agent run")
	}
	toolsText := transcriptText(next.transcript)
	for _, want := range []string{
		"Tools",
		"1 registered | registered catalog",
		"Registry",
		"registered  1",
		"Available",
		"- read_file",
		"actions: /mcp manage servers | /permissions manage access",
	} {
		assertContains(t, toolsText, want)
	}
	assertNotContains(t, toolsText, "status: ok")
	assertNotContains(t, toolsText, "registered tools:")
}

func TestToolsCommandCardHandlesNilRegistry(t *testing.T) {
	text := model{}.toolsText()

	for _, want := range []string{
		"Tools",
		"0 registered | no tools available",
		"registered  0",
	} {
		assertContains(t, text, want)
	}
}

func TestToolsCommandShowsFullSortedCatalog(t *testing.T) {
	registry := tools.NewRegistry()
	for _, name := range []string{
		"write_file",
		"read_file",
		"grep",
		"glob",
		"edit_file",
		"apply_patch",
		"bash",
		"web_search",
		"web_fetch",
	} {
		registry.Register(commandTestMCPTool{name: name, description: name + " tool"})
	}

	m := newModel(context.Background(), Options{
		Registry: registry,
	})
	m.input.SetValue("/tools")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd != nil {
		t.Fatal("expected /tools to be handled without starting an agent run")
	}
	text := transcriptText(next.transcript)
	for _, want := range []string{
		"9 registered | registered catalog",
		"- apply_patch",
		"- bash",
		"- edit_file",
		"- glob",
		"- grep",
		"- read_file",
		"- web_fetch",
		"- web_search",
		"- write_file",
	} {
		assertContains(t, text, want)
	}
	assertNotContains(t, text, "... 1 more")

	previous := -1
	for _, want := range []string{
		"- apply_patch",
		"- bash",
		"- edit_file",
		"- glob",
		"- grep",
		"- read_file",
		"- web_fetch",
		"- web_search",
		"- write_file",
	} {
		current := strings.Index(text, want)
		if current < 0 {
			t.Fatalf("expected tools output to contain %q, got:\n%s", want, text)
		}
		if current <= previous {
			t.Fatalf("expected tools output to keep sorted order at %q, got:\n%s", want, text)
		}
		previous = current
	}
}

func TestContextAndPermissionsCommandsRenderProductState(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(".", nil))

	store, err := sandbox.NewGrantStore(sandbox.StoreOptions{FilePath: filepath.Join(t.TempDir(), "sandbox-grants.json")})
	if err != nil {
		t.Fatalf("NewGrantStore returned error: %v", err)
	}
	if _, err := store.Grant(sandbox.GrantInput{
		ToolName: "bash",
		Decision: sandbox.GrantAllow,
		Reason:   "sk-proj-sensitive-credential-value approved shell",
	}); err != nil {
		t.Fatalf("Grant returned error: %v", err)
	}

	m := newModel(context.Background(), Options{
		Cwd:            `D:\codings\Opensource\Zero`,
		ProviderName:   "openai",
		ModelName:      "gpt-4.1",
		Registry:       registry,
		SandboxStore:   store,
		PermissionMode: agent.PermissionModeAsk,
	})

	m.input.SetValue("/context")
	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd != nil {
		t.Fatal("expected /context to be handled without starting an agent run")
	}
	contextText := transcriptText(next.transcript)
	for _, want := range []string{
		"Context",
		"go runtime | ask permissions | 1 tool",
		"Runtime",
		"cwd        D:\\codings\\Opensource\\Zero",
		"provider   openai",
		"model      gpt-4.1",
		"Session",
		"active      none",
		"compaction  idle",
		"Tools",
		"registered  1",
		"actions: /permissions manage access | /tools inspect catalog",
	} {
		assertContains(t, contextText, want)
	}
	assertNotContains(t, contextText, "status: ok")
	assertNotContains(t, contextText, "permission mode:")

	next.input.SetValue("/permissions")
	updated, cmd = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if cmd != nil {
		t.Fatal("expected /permissions to be handled without starting an agent run")
	}
	permissionText := transcriptText(next.transcript)
	for _, want := range []string{
		"Permissions",
		"ask permissions",
		"1 persistent grant",
		"State",
		"mode  ask",
		"Grants",
		"bash [allow]",
		"[REDACTED]",
	} {
		assertContains(t, permissionText, want)
	}
	assertNotContains(t, permissionText, "sk-proj-sensitive-credential-value")
	assertNotContains(t, permissionText, "status: ok")
	assertNotContains(t, permissionText, "Permission mode:")
}

// stubGrantStore is a minimal test double for sandbox.GrantStore. It only
// implements List, which is enough to exercise the error path of
// permissionsText().
type stubGrantStore struct {
	listErr error
}

func (store *stubGrantStore) List() ([]sandbox.Grant, error) {
	return nil, store.listErr
}

func TestPermissionsCommandCardHandlesNilStoreAndEmptyGrants(t *testing.T) {
	nilStore := model{permissionMode: agent.PermissionModeAuto}.permissionsText()
	for _, want := range []string{
		"Permissions",
		"auto permissions",
		"grants unavailable",
		"mode  auto",
		"persistent grants: unavailable",
	} {
		assertContains(t, nilStore, want)
	}
	assertNotContains(t, nilStore, "status: warning")

	store, err := sandbox.NewGrantStore(sandbox.StoreOptions{FilePath: filepath.Join(t.TempDir(), "empty-grants.json")})
	if err != nil {
		t.Fatalf("NewGrantStore returned error: %v", err)
	}
	emptyText := model{permissionMode: agent.PermissionModeAsk, sandboxStore: store}.permissionsText()
	for _, want := range []string{
		"Permissions",
		"ask permissions",
		"no persistent grants",
		"mode  ask",
		"none",
	} {
		assertContains(t, emptyText, want)
	}
	assertNotContains(t, emptyText, "status: info")

	errStore := &stubGrantStore{listErr: errors.New("storage failure")}
	errText := model{permissionMode: agent.PermissionModeAsk}.permissionsTextWithStore(errStore)
	for _, want := range []string{
		"Permissions",
		"ask permissions",
		"grants error",
		"mode  ask",
		"error: storage failure",
	} {
		assertContains(t, errText, want)
	}
	assertNotContains(t, errText, "status: blocked")
}

func TestContextCommandCardHandlesNilRegistryAndStableStyle(t *testing.T) {
	text := model{}.contextText()

	for _, want := range []string{
		"Context",
		"0 tools",
		"style      concise",
		"root        unknown",
	} {
		assertContains(t, text, want)
	}
}

func TestCompactCommandAvoidsShellOnlyPlaceholder(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.input.SetValue("/compact")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd != nil {
		t.Fatal("expected /compact to be handled without starting an agent run")
	}
	text := transcriptText(next.transcript)
	for _, want := range []string{"Compact", "status: warning", "requested: yes", "visible transcript rows:"} {
		assertContains(t, text, want)
	}
	if strings.Contains(text, "not wired") || strings.Contains(text, "future compaction backend") {
		t.Fatalf("compact output should describe product state, got %q", text)
	}
}
