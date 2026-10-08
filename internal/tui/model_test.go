package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/notify"
	"github.com/Gitlawb/zero/internal/providermodeldiscovery"
	"github.com/Gitlawb/zero/internal/sandbox"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// execCmd runs a possibly-batched command synchronously and returns the first
// substantive message. Run starts batch the agent command with the spinner
// tick, so tests unwrap the batch and skip spinner housekeeping.
func execCmd(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return msg
	}
	for _, sub := range batch {
		if inner := execCmd(sub); inner != nil {
			if _, isTick := inner.(spinner.TickMsg); !isTick {
				return inner
			}
		}
	}
	return nil
}

type fakeProvider struct {
	events   []zeroruntime.StreamEvent
	requests []zeroruntime.CompletionRequest
}

func (provider *fakeProvider) StreamCompletion(
	ctx context.Context,
	request zeroruntime.CompletionRequest,
) (<-chan zeroruntime.StreamEvent, error) {
	provider.requests = append(provider.requests, request)
	ch := make(chan zeroruntime.StreamEvent, len(provider.events))
	for _, event := range provider.events {
		ch <- event
	}
	close(ch)
	return ch, nil
}

func TestPromptSubmitInjectsLiveSessionModelContext(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	provider := &fakeProvider{events: []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventText, Content: "I am using the active session model."},
		{Type: zeroruntime.StreamEventDone},
	}}
	m := newModel(context.Background(), Options{
		Cwd:          t.TempDir(),
		ProviderName: "ollama-cloud",
		ModelName:    "glm-5.1",
		Provider:     provider,
		Registry:     tools.NewRegistry(),
	})
	m.input.SetValue("which model are you")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd == nil {
		t.Fatal("expected prompt submit to start an agent run")
	}

	updated, _ = next.Update(execCmd(cmd))
	_ = updated.(model)

	if len(provider.requests) != 1 {
		t.Fatalf("expected one provider request, got %d", len(provider.requests))
	}
	if len(provider.requests[0].Messages) == 0 {
		t.Fatal("expected provider request to include a system message")
	}
	systemPrompt := provider.requests[0].Messages[0].Content
	for _, want := range []string{
		"Active provider: ollama-cloud",
		"Active model: glm-5.1",
		"Persisted config",
	} {
		if !strings.Contains(systemPrompt, want) {
			t.Fatalf("expected system prompt to contain %q, got:\n%s", want, systemPrompt)
		}
	}
}

func TestPromptWaitsForToolReadinessBeforeSnapshot(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	registry := tools.NewRegistry()
	provider := &fakeProvider{events: []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventText, Content: "done"},
		{Type: zeroruntime.StreamEventDone},
	}}
	awaited := false
	m := newModel(context.Background(), Options{
		Cwd:          t.TempDir(),
		ProviderName: "test",
		ModelName:    "test-model",
		Provider:     provider,
		Registry:     registry,
		AwaitToolReadiness: func(context.Context) {
			awaited = true
			registry.Register(tools.NewScopedReadFileTool(t.TempDir(), nil))
		},
	})
	m.input.SetValue("inspect the workspace")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd == nil {
		t.Fatal("expected prompt submit to start an agent run")
	}
	next.Update(execCmd(cmd))

	if !awaited {
		t.Fatal("agent run did not await tool readiness")
	}
	if len(provider.requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(provider.requests))
	}
	for _, definition := range provider.requests[0].Tools {
		if definition.Name == "read_file" {
			return
		}
	}
	t.Fatal("provider request omitted tool published by readiness callback")
}

func TestPromptBuildsTurnSessionForCurrentProvider(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	provider := &fakeProvider{events: []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventText, Content: "done"},
		{Type: zeroruntime.StreamEventDone},
	}}
	profile := config.ProviderProfile{Name: "chatgpt", Model: "gpt-test"}
	called := false
	m := newModel(context.Background(), Options{
		Cwd:             t.TempDir(),
		ProviderName:    profile.Name,
		ModelName:       profile.Model,
		ProviderProfile: profile,
		Provider:        provider,
		Registry:        tools.NewRegistry(),
		NewTurnSessionProvider: func(gotProfile config.ProviderProfile, gotProvider zeroruntime.Provider) zeroruntime.TurnSessionProvider {
			called = true
			if gotProfile.Name != profile.Name || gotProfile.Model != profile.Model || gotProvider != provider {
				t.Fatalf("turn session factory inputs = %#v/%#v", gotProfile, gotProvider)
			}
			return zeroruntime.NewProviderTurnSessionProvider(gotProvider, zeroruntime.ProviderCapabilities{})
		},
	})
	m.input.SetValue("inspect the workspace")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	next.Update(execCmd(cmd))
	if !called {
		t.Fatal("agent run did not build a turn session for the current provider")
	}
}

func TestPromptSubmitStoresReasoningSeparatelyFromAnswer(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	provider := &fakeProvider{events: []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventReasoning, Content: "private "},
		{Type: zeroruntime.StreamEventReasoning, Content: "thought"},
		{Type: zeroruntime.StreamEventText, Content: "public answer"},
		{Type: zeroruntime.StreamEventDone},
	}}
	m := newModel(context.Background(), Options{
		Cwd:          t.TempDir(),
		ProviderName: "tokenrouter",
		ModelName:    "MiniMax-M3",
		Provider:     provider,
		Registry:     tools.NewRegistry(),
	})
	base := time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC)
	times := []time.Time{
		base, // burst tracker on Enter keypress (timing-based paste guard)
		base, // run start: consumed by turnStartedAt (the working-line elapsed clock)
		base,
		base.Add(1 * time.Second),
		base.Add(1800 * time.Millisecond),
		base.Add(2500 * time.Millisecond),
		base.Add(6 * time.Second),
	}
	m.now = func() time.Time {
		if len(times) == 0 {
			return base.Add(6 * time.Second)
		}
		next := times[0]
		times = times[1:]
		return next
	}
	m.input.SetValue("hello")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd == nil {
		t.Fatal("expected prompt submit to start an agent run")
	}

	updated, _ = next.Update(execCmd(cmd))
	next = updated.(model)

	reasoning, ok := findTranscriptRow(next.transcript, rowReasoning)
	if !ok || reasoning.text != "private thought" {
		t.Fatalf("reasoning row = %#v, ok=%v", reasoning, ok)
	}
	if reasoning.turnElapsed != 1500*time.Millisecond {
		t.Fatalf("reasoning elapsed = %s, want 1.5s", reasoning.turnElapsed)
	}
	assistant, ok := findTranscriptRow(next.transcript, rowAssistant)
	if !ok || assistant.text != "public answer" {
		t.Fatalf("assistant row = %#v, ok=%v", assistant, ok)
	}
	if assistant.turnElapsed != 6*time.Second {
		t.Fatalf("assistant elapsed = %s, want 6s", assistant.turnElapsed)
	}
	if strings.Contains(assistant.text, "private thought") {
		t.Fatalf("assistant answer leaked reasoning: %#v", assistant)
	}
}

func TestParseCommand(t *testing.T) {
	cases := []struct {
		input string
		kind  commandKind
		text  string
	}{
		{input: "", kind: commandEmpty},
		{input: "   ", kind: commandEmpty},
		{input: "/help", kind: commandHelp},
		{input: "/clear", kind: commandClear},
		{input: "/exit", kind: commandExit},
		{input: "/quit", kind: commandExit},
		{input: "/tools", kind: commandTools},
		{input: "/permissions", kind: commandPermissions},
		{input: "/sandbox-setup", kind: commandSandboxSetup},
		{input: "/context", kind: commandContext},
		{input: "/model", kind: commandModel},
		{input: "/model list", kind: commandModel, text: "list"},
		{input: "/search needle", kind: commandSearch, text: "needle"},
		{input: "/find needle", kind: commandSearch, text: "needle"},
		{input: "/resume", kind: commandResume},
		{input: "/sessions", kind: commandResume},
		{input: "/spec add review flow", kind: commandSpec, text: "add review flow"},
		{input: "/plan", kind: commandPlan},
		{input: "/plan on", kind: commandPlan, text: "on"},
		{input: "/plan off", kind: commandPlan, text: "off"},
		{input: "/compact", kind: commandCompact},
		{input: "/effort high", kind: commandEffort, text: "high"},
		{input: "/fast", kind: commandFast},
		{input: "/style concise", kind: commandStyle, text: "concise"},
		{input: "/debug-mode", kind: commandDebug},
		{input: "hello zero", kind: commandPrompt, text: "hello zero"},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			command := parseCommand(tc.input)
			if command.kind != tc.kind || command.text != tc.text {
				t.Fatalf("expected kind=%v text=%q, got kind=%v text=%q", tc.kind, tc.text, command.kind, command.text)
			}
		})
	}
}

func TestCommandRegistryResolvesAliasesAndFormatsHelp(t *testing.T) {
	names := listCommandNames()
	for _, name := range []string{"/help", "/model", "/provider", "/context", "/debug-mode", "/quit"} {
		if !stringSliceContains(names, name) {
			t.Fatalf("expected command names to contain %s, got %#v", name, names)
		}
	}

	resolved, ok := resolveCommand("/quit")
	if !ok || resolved.kind != commandExit {
		t.Fatalf("expected /quit to resolve to exit, got ok=%v command=%#v", ok, resolved)
	}

	help := strings.Join(formatCommandHelpLines(), "\n")
	for _, want := range []string{"/model", "/context", "/debug", "/permissions", "/spec", "model"} {
		assertContains(t, help, want)
	}
}

func TestTranscriptReducer(t *testing.T) {
	transcript := initialTranscript()
	transcript = reduceTranscript(transcript, transcriptAction{kind: actionAppendUser, text: "hello"})
	transcript = reduceTranscript(transcript, transcriptAction{kind: actionAppendAssistant, text: "hi"})
	transcript = reduceTranscript(transcript, transcriptAction{kind: actionAppendSystem, text: "note"})
	transcript = reduceTranscript(transcript, transcriptAction{kind: actionAppendError, text: "boom"})

	if len(transcript) != 5 {
		t.Fatalf("expected welcome plus four rows, got %#v", transcript)
	}
	if transcript[1].kind != rowUser || transcript[1].text != "hello" {
		t.Fatalf("expected user row, got %#v", transcript[1])
	}
	if transcript[3].kind != rowSystem || transcript[3].text != "note" {
		t.Fatalf("expected system row, got %#v", transcript[3])
	}

	cleared := reduceTranscript(transcript, transcriptAction{kind: actionClear})
	if len(cleared) != 1 || cleared[0].kind != rowWelcome {
		t.Fatalf("expected clear to reset to welcome row, got %#v", cleared)
	}
}

func TestInitialRenderShowsLimeChatSurface(t *testing.T) {
	model := newModel(context.Background(), Options{
		Cwd:          `/workspace/zero`,
		ProviderName: "openai",
		ModelName:    "gpt-4.1",
	})
	model.width = 120
	model.height = 34

	view := viewString(model.View())
	assertContains(t, view, `/workspace/zero`)
	assertContains(t, view, "openai/gpt-4.1")
	assertContains(t, view, emptyStateTagline)
	assertNotContains(t, view, "running zero against ")
	assertNotContains(t, view, " 0 ")
	assertContains(t, view, composerPlaceholder)
	assertNotContains(t, view, "interactive")
	if strings.Contains(view, "Welcome to Zero") {
		t.Fatalf("empty chat surface should not show welcome transcript clutter, got %q", view)
	}
}

func TestEmptyStateCollapsesAfterFirstPrompt(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.width = 100
	m.height = 30
	m.input.SetValue("inspect the repo")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	next.width = m.width
	next.height = m.height

	if next.pending {
		t.Fatal("expected missing provider prompt not to start an agent run")
	}
	if !transcriptContains(next.transcript, "inspect the repo") || !transcriptContains(next.transcript, "No provider configured.") {
		t.Fatalf("expected prompt and notice rows in transcript, got %#v", next.transcript)
	}
	if next.flushed != len(next.transcript) {
		t.Fatalf("expected settled rows to flush to scrollback, flushed=%d rows=%d", next.flushed, len(next.transcript))
	}
	view := viewString(next.View())
	if strings.Contains(view, emptyStateTagline) {
		t.Fatalf("empty state should collapse after first prompt, got %q", view)
	}
	// Working view shows provider status and the composer divider model fallback.
	assertNotContains(t, view, "interactive")
	assertContains(t, view, "no model")
}

func TestEmptyStateStaysVisibleOnEmptySubmit(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.width = 96
	m.height = 30
	m.input.SetValue("   ")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	next.width = m.width
	next.height = m.height

	view := viewString(next.View())
	assertContains(t, view, emptyStateTagline)
	assertNotContains(t, view, "❯ inspect")
}

func TestHelpCommandAppendsHelpRow(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.input.SetValue("/help")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if !transcriptContains(next.transcript, "/tools") {
		t.Fatalf("expected help transcript to mention /tools, got %#v", next.transcript)
	}
	if !transcriptContains(next.transcript, "/model") || !transcriptContains(next.transcript, "/context") {
		t.Fatalf("expected help transcript to mention model and context commands, got %#v", next.transcript)
	}
}

func TestClearCommandResetsTranscript(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendUser, text: "hello"})
	m.input.SetValue("/clear")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if len(next.transcript) != 2 || next.transcript[0].kind != rowWelcome {
		t.Fatalf("expected clear to reset transcript to welcome + note, got %#v", next.transcript)
	}
	if !transcriptContains(next.transcript, "/new") {
		t.Fatalf("expected clear to point users to /new, got %#v", next.transcript)
	}
}

func TestToolsCommandListsRegisteredTools(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(".", nil))
	m := newModel(context.Background(), Options{Registry: registry})
	m.input.SetValue("/tools")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if !transcriptContains(next.transcript, "read_file") {
		t.Fatalf("expected tools transcript to list read_file, got %#v", next.transcript)
	}
}

func TestPermissionsCommandListsPersistentSandboxGrants(t *testing.T) {
	store, err := sandbox.NewGrantStore(sandbox.StoreOptions{FilePath: filepath.Join(t.TempDir(), "sandbox-grants.json")})
	if err != nil {
		t.Fatalf("NewGrantStore returned error: %v", err)
	}
	if _, err := store.Grant(sandbox.GrantInput{
		ToolName: "bash",
		Decision: sandbox.GrantAllow,
		Reason:   "sk-proj-sensitive-credential-value trusted shell",
	}); err != nil {
		t.Fatalf("Grant bash returned error: %v", err)
	}
	if _, err := store.Grant(sandbox.GrantInput{
		ToolName: "write_file",
		Decision: sandbox.GrantDeny,
	}); err != nil {
		t.Fatalf("Grant write_file returned error: %v", err)
	}
	m := newModel(context.Background(), Options{
		PermissionMode: agent.PermissionModeAsk,
		SandboxStore:   store,
	})
	m.input.SetValue("/permissions")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd != nil {
		t.Fatal("expected /permissions to be handled without starting an agent run")
	}
	text := transcriptText(next.transcript)
	for _, want := range []string{
		"Permissions",
		"ask permissions",
		"mode  ask",
		"Grants",
		"bash [allow]",
		"write_file [deny]",
		"[REDACTED]",
	} {
		assertContains(t, text, want)
	}
	assertNotContains(t, text, "sk-proj-sensitive-credential-value")
	assertNotContains(t, text, "status: ok")
	assertNotContains(t, text, "Permission mode:")
}

func TestPlanCommandShowsCurrentPlan(t *testing.T) {
	registry := tools.NewRegistry()
	planTool := tools.NewUpdatePlanTool()
	result := planTool.Run(context.Background(), map[string]any{
		"plan": []any{
			map[string]any{
				"id":      "one",
				"content": "Wire model catalog",
				"status":  "completed",
			},
			map[string]any{
				"id":      "two",
				"content": "Add max turns",
				"status":  "in_progress",
				"notes":   "Go exec parity",
			},
		},
	})
	if result.Status != tools.StatusOK {
		t.Fatalf("update_plan setup failed: %#v", result)
	}
	registry.Register(planTool)
	m := newModel(context.Background(), Options{Registry: registry})
	m.input.SetValue("/plan")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd != nil {
		t.Fatal("expected /plan to be handled without starting an agent run")
	}
	for _, want := range []string{"Current Plan", "Wire model catalog", "Add max turns", "in_progress", "Go exec parity"} {
		if !transcriptContains(next.transcript, want) {
			t.Fatalf("expected plan transcript to contain %q, got %#v", want, next.transcript)
		}
	}
}

func TestPlanCommandHandlesMissingPlanTool(t *testing.T) {
	m := newModel(context.Background(), Options{Registry: tools.NewRegistry()})
	m.input.SetValue("/plan")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if !transcriptContains(next.transcript, "No plan is active") {
		t.Fatalf("expected missing plan message, got %#v", next.transcript)
	}
}

func TestContextCommandShowsSessionState(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(".", nil))
	m := newModel(context.Background(), Options{
		Cwd:            `D:\codings\Opensource\Zero`,
		ProviderName:   "openai",
		ModelName:      "gpt-4.1",
		Registry:       registry,
		PermissionMode: agent.PermissionModeAsk,
	})
	m.input.SetValue("/context")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd != nil {
		t.Fatal("expected /context to be handled without starting an agent run")
	}
	for _, want := range []string{
		`D:\codings\Opensource\Zero`,
		"go runtime | ask permissions | 1 tool",
		"provider   openai",
		"model      gpt-4.1",
		"max turns  ",
		"root        ",
		"registered  1",
	} {
		if !transcriptContains(next.transcript, want) {
			t.Fatalf("expected context transcript to contain %q, got %#v", want, next.transcript)
		}
	}
}

func TestModelCommandShowsActiveModelWithoutRunningAgent(t *testing.T) {
	m := newModel(context.Background(), Options{
		ProviderName: "openai",
		ModelName:    "gpt-4.1",
		Provider:     &fakeProvider{},
	})
	m.input.SetValue("/model list")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd != nil {
		t.Fatal("expected /model to be handled without starting an agent run")
	}
	for _, want := range []string{"Active model: gpt-4.1", "provider: openai", "Available models", "* gpt-4.1"} {
		if !transcriptContains(next.transcript, want) {
			t.Fatalf("expected model transcript to contain %q, got %#v", want, next.transcript)
		}
	}
	if !transcriptHasMarkedModelEntry(next.transcript) {
		t.Fatalf("expected model transcript to contain a marked model entry, got %#v", next.transcript)
	}
	if transcriptContains(next.transcript, "Model switching") {
		t.Fatalf("expected /model list to show catalog, got switching placeholder: %#v", next.transcript)
	}
}

func TestModelCommandSwitchesSessionModel(t *testing.T) {
	var rebuilt config.ProviderProfile
	nextProvider := &fakeProvider{}
	m := newModel(context.Background(), Options{
		ProviderName: "openai",
		ModelName:    "gpt-4.1",
		ProviderProfile: config.ProviderProfile{
			Name:         "openai",
			ProviderKind: config.ProviderKindOpenAI,
			BaseURL:      config.OpenAIBaseURL,
			APIKey:       "sk-test",
			Model:        "gpt-4.1",
		},
		Provider: &fakeProvider{},
		NewProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			rebuilt = profile
			return nextProvider, nil
		},
	})
	m.input.SetValue("/model gpt-4.1-mini")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if next.pending {
		t.Fatal("expected /model to be handled without starting an agent run")
	}
	if next.modelName != "gpt-4.1-mini" || next.provider != nextProvider {
		t.Fatalf("expected model/provider to update, got model=%q provider=%#v", next.modelName, next.provider)
	}
	if rebuilt.Model != "gpt-4.1-mini" {
		t.Fatalf("expected provider rebuild with selected model, got %#v", rebuilt)
	}
	for _, want := range []string{"Model:", "gpt-4.1-mini · openai"} {
		if !strings.Contains(next.transientNotice.text, want) {
			t.Fatalf("expected model notice to contain %q, got %q", want, next.transientNotice.text)
		}
	}
}

func TestModelCommandAcceptsChatGPTCatalogModelID(t *testing.T) {
	var rebuilt config.ProviderProfile
	nextProvider := &fakeProvider{}
	m := newModel(context.Background(), Options{
		ProviderName: "chatgpt",
		ModelName:    "gpt-5.6-terra",
		ProviderProfile: config.ProviderProfile{
			Name:         "chatgpt",
			CatalogID:    "chatgpt",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://chatgpt.com/backend-api/codex",
			Model:        "gpt-5.6-terra",
		},
		Provider: &fakeProvider{},
		NewProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			rebuilt = profile
			return nextProvider, nil
		},
	})
	m.modelPickerLiveByProvider = map[string][]providermodeldiscovery.Model{
		"chatgpt": {{ID: "gpt-5.6-sol"}},
	}
	m.input.SetValue("/model openai/gpt-5.6-sol")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if next.pending {
		t.Fatal("expected /model to be handled without starting an agent run")
	}
	if next.modelName != "gpt-5.6-sol" || next.provider != nextProvider {
		t.Fatalf("expected ChatGPT model switch to use bare catalog ID, got model=%q provider=%#v", next.modelName, next.provider)
	}
	if rebuilt.Model != "gpt-5.6-sol" {
		t.Fatalf("expected provider rebuild with bare ChatGPT model ID, got %#v", rebuilt)
	}
}

func TestProviderModelSwitchCandidatesPreserveGatewayModelIDs(t *testing.T) {
	for _, test := range []struct {
		name     string
		provider string
		input    string
		want     []string
	}{
		{
			name:     "ChatGPT accepts the models.dev OpenAI namespace",
			provider: "chatgpt",
			input:    "openai/gpt-5.6-sol",
			want:     []string{"openai/gpt-5.6-sol", "gpt-5.6-sol"},
		},
		{
			name:     "gateway keeps qualified model ID",
			provider: "openrouter",
			input:    "openai/gpt-5.6-sol",
			want:     []string{"openai/gpt-5.6-sol"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := providerModelSwitchCandidates(test.provider, test.input); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("providerModelSwitchCandidates(%q, %q) = %#v, want %#v", test.provider, test.input, got, test.want)
			}
		})
	}
}

func TestModelCommandPersistsSelectedModelToUserConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "zero.json")
	if _, err := config.UpsertProvider(configPath, config.ProviderProfile{
		Name:         "openai",
		ProviderKind: config.ProviderKindOpenAI,
		BaseURL:      config.OpenAIBaseURL,
		APIKey:       "sk-test",
		Model:        "gpt-4.1",
	}, true); err != nil {
		t.Fatalf("write user config: %v", err)
	}

	m := newModel(context.Background(), Options{
		UserConfigPath: configPath,
		ProviderName:   "openai",
		ModelName:      "gpt-4.1",
		ProviderProfile: config.ProviderProfile{
			Name:         "openai",
			ProviderKind: config.ProviderKindOpenAI,
			BaseURL:      config.OpenAIBaseURL,
			APIKey:       "sk-test",
			Model:        "gpt-4.1",
		},
		Provider: &fakeProvider{},
		NewProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return &fakeProvider{}, nil
		},
	})
	m.input.SetValue("/model gpt-4.1-mini")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if next.pending {
		t.Fatal("expected /model to be handled without starting an agent run")
	}
	if next.modelName != "gpt-4.1-mini" {
		t.Fatalf("modelName = %q, want gpt-4.1-mini", next.modelName)
	}
	persisted, err := config.Resolve(config.ResolveOptions{UserConfigPath: configPath})
	if err != nil {
		t.Fatalf("resolve persisted config: %v", err)
	}
	if got := persisted.Provider.Model; got != "gpt-4.1-mini" {
		t.Fatalf("persisted provider model = %q, want gpt-4.1-mini", got)
	}
	if !strings.Contains(next.transientNotice.text, "gpt-4.1-mini") {
		t.Fatalf("expected model notice to name selected model, got %q", next.transientNotice.text)
	}
}

type stubModelSwitchCompactionGuard struct {
	decision modelSwitchCompactionDecision
	requests []modelSwitchCompactionRequest
}

func (guard *stubModelSwitchCompactionGuard) BeforeModelSwitch(request modelSwitchCompactionRequest) modelSwitchCompactionDecision {
	guard.requests = append(guard.requests, request)
	return guard.decision
}

func TestDefaultModelSwitchCompactionPolicyRequestsCompactionForLargeTargetWindowUsage(t *testing.T) {
	decision := defaultModelSwitchCompactionPolicy{}.BeforeModelSwitch(modelSwitchCompactionRequest{
		CurrentModel:        "gpt-4.1",
		TargetModel:         "small-context-model",
		TargetContextWindow: 1000,
		EstimatedTokens:     850,
		SessionEventCount:   20,
		CompactRequests:     0,
	})

	if !decision.RequestCompaction {
		t.Fatalf("expected default policy to request compaction, got %#v", decision)
	}
	if !strings.Contains(decision.Reason, "target context") {
		t.Fatalf("expected reason to mention target context, got %q", decision.Reason)
	}
}

func TestModelCommandRequestsCompactionBeforeDirtyContextSwitch(t *testing.T) {
	guard := &stubModelSwitchCompactionGuard{
		decision: modelSwitchCompactionDecision{
			RequestCompaction: true,
			Reason:            "dirty context uses most of the target window",
		},
	}
	previousGuard := modelSwitchCompactionGuard
	modelSwitchCompactionGuard = guard
	defer func() { modelSwitchCompactionGuard = previousGuard }()

	originalProvider := &fakeProvider{}
	rebuilds := 0
	m := newModel(context.Background(), Options{
		ProviderName: "openai",
		ModelName:    "gpt-4.1",
		ProviderProfile: config.ProviderProfile{
			Name:         "openai",
			ProviderKind: config.ProviderKindOpenAI,
			BaseURL:      config.OpenAIBaseURL,
			APIKey:       "sk-test",
			Model:        "gpt-4.1",
		},
		Provider: originalProvider,
		NewProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			rebuilds++
			return &fakeProvider{}, nil
		},
	})
	m.sessionEvents = []sessions.Event{{Sequence: 1, Type: sessions.EventMessage}}
	m.input.SetValue("/model gpt-4.1-mini")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd != nil {
		t.Fatal("expected /model to be handled without starting an agent run")
	}
	if rebuilds != 0 {
		t.Fatalf("provider should not be rebuilt before requested compaction, got %d rebuilds", rebuilds)
	}
	if next.modelName != "gpt-4.1" || next.provider != originalProvider {
		t.Fatalf("expected active model/provider to remain unchanged, got model=%q provider=%#v", next.modelName, next.provider)
	}
	if next.compactRequests != 1 {
		t.Fatalf("expected model switch to request compaction, got %d requests", next.compactRequests)
	}
	if len(guard.requests) != 1 {
		t.Fatalf("expected one compaction guard request, got %d", len(guard.requests))
	}
	request := guard.requests[0]
	if request.CurrentModel != "gpt-4.1" || request.TargetModel != "gpt-4.1-mini" {
		t.Fatalf("unexpected guard model transition: %#v", request)
	}
	if request.SessionEventCount != 1 {
		t.Fatalf("expected dirty session event count in guard request, got %#v", request)
	}
	for _, want := range []string{
		"Context compaction requested before switching models.",
		"dirty context uses most of the target window",
	} {
		if !transcriptContains(next.transcript, want) {
			t.Fatalf("expected model transcript to contain %q, got %#v", want, next.transcript)
		}
	}
}

func TestModelCommandRequiresProviderRebuildForSwitch(t *testing.T) {
	m := newModel(context.Background(), Options{
		ModelName: "gpt-4.1",
		ProviderProfile: config.ProviderProfile{
			Name:         "openai",
			ProviderKind: config.ProviderKindOpenAI,
			BaseURL:      config.OpenAIBaseURL,
			Model:        "gpt-4.1",
		},
	})
	m.input.SetValue("/model gpt-4.1-mini")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd != nil {
		t.Fatal("expected /model to be handled without starting an agent run")
	}
	if next.modelName != "gpt-4.1" {
		t.Fatalf("expected active model to remain unchanged, got %q", next.modelName)
	}
	if !transcriptContains(next.transcript, "Provider rebuild is not available") {
		t.Fatalf("expected provider rebuild availability error, got %#v", next.transcript)
	}
}

func TestModelCommandRejectsSwitchWhilePending(t *testing.T) {
	m := newModel(context.Background(), Options{
		ModelName: "gpt-4.1",
		ProviderProfile: config.ProviderProfile{
			Name:         "openai",
			ProviderKind: config.ProviderKindOpenAI,
			BaseURL:      config.OpenAIBaseURL,
			Model:        "gpt-4.1",
		},
	})
	m.pending = true
	m.input.SetValue("/model gpt-4.1-mini")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd != nil {
		t.Fatal("expected /model to be handled without starting an agent run")
	}
	if next.modelName != "gpt-4.1" {
		t.Fatalf("expected active model to remain unchanged, got %q", next.modelName)
	}
	if !transcriptContains(next.transcript, "Cannot switch models while a run is active") {
		t.Fatalf("expected pending switch error, got %#v", next.transcript)
	}
}

func TestModelCommandReportsProviderRebuildErrors(t *testing.T) {
	m := newModel(context.Background(), Options{
		ModelName: "gpt-4.1",
		ProviderProfile: config.ProviderProfile{
			Name:         "openai",
			ProviderKind: config.ProviderKindOpenAI,
			BaseURL:      config.OpenAIBaseURL,
			Model:        "gpt-4.1",
		},
		NewProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return nil, errors.New("rebuild failed")
		},
	})
	m.input.SetValue("/model gpt-4.1-mini")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if next.modelName != "gpt-4.1" {
		t.Fatalf("expected active model to remain unchanged, got %q", next.modelName)
	}
	if !transcriptContains(next.transcript, "rebuild failed") {
		t.Fatalf("expected rebuild error, got %#v", next.transcript)
	}
}

func TestDoctorCommandUsesCurrentProviderProfile(t *testing.T) {
	m := newModel(context.Background(), Options{
		ProviderProfile: config.ProviderProfile{
			Name:         "openai",
			ProviderKind: config.ProviderKindOpenAI,
			BaseURL:      config.OpenAIBaseURL,
			Model:        "gpt-4.1",
			APIKey:       "sk-test", // credentialed so provider.config passes (isolates this render test from the no-key check)
		},
	})
	m.input.SetValue("/doctor")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd != nil {
		t.Fatal("expected /doctor to be handled without starting an agent run")
	}
	for _, want := range []string{"Diagnostics", "Provider", "provider.connectivity", "Actions"} {
		if !transcriptContains(next.transcript, want) {
			t.Fatalf("expected doctor transcript to contain %q, got %#v", want, next.transcript)
		}
	}
	for _, unwanted := range []string{"provider.config", "provider.model", "Generated", "Checks"} {
		if transcriptContains(next.transcript, unwanted) {
			t.Fatalf("expected doctor transcript to hide %q, got %#v", unwanted, next.transcript)
		}
	}
}

func TestSearchCommandUsesSessionStore(t *testing.T) {
	store := testSessionStore(t)
	session, err := store.Create(sessions.CreateInput{Title: "Searchable", Cwd: "repo", ModelID: "gpt-4.1", Provider: "openai"})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if _, err := store.AppendEvent(session.SessionID, sessions.AppendEventInput{
		Type: sessions.EventMessage,
		Payload: map[string]any{
			"role":    "assistant",
			"content": "needle appears here",
		},
	}); err != nil {
		t.Fatalf("AppendEvent returned error: %v", err)
	}
	m := newModel(context.Background(), Options{SessionStore: store})
	m.input.SetValue("/search needle")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd != nil {
		t.Fatal("expected /search to be handled without starting an agent run")
	}
	if !transcriptContains(next.transcript, "Found 1 local session event") || !transcriptContains(next.transcript, "needle appears here") {
		t.Fatalf("expected search hit in transcript, got %#v", next.transcript)
	}
}

func TestSearchCommandRequiresQuery(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.input.SetValue("/search")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if !transcriptContains(next.transcript, "usage: /search <query>") {
		t.Fatalf("expected search usage, got %#v", next.transcript)
	}
}

func TestResumeCommandListsRecentSessions(t *testing.T) {
	store := testSessionStore(t)
	first, err := store.Create(sessions.CreateInput{Title: "Older", ModelID: "gpt-4.1", Provider: "openai"})
	if err != nil {
		t.Fatalf("Create older returned error: %v", err)
	}
	if _, err := store.AppendEvent(first.SessionID, sessions.AppendEventInput{Type: sessions.EventMessage, Payload: map[string]any{"content": "old"}}); err != nil {
		t.Fatalf("Append older returned error: %v", err)
	}
	second, err := store.Create(sessions.CreateInput{Title: "Newer", ModelID: "claude-sonnet-4.5", Provider: "anthropic"})
	if err != nil {
		t.Fatalf("Create newer returned error: %v", err)
	}
	if _, err := store.AppendEvent(second.SessionID, sessions.AppendEventInput{Type: sessions.EventMessage, Payload: map[string]any{"content": "new"}}); err != nil {
		t.Fatalf("Append newer returned error: %v", err)
	}
	m := newModel(context.Background(), Options{SessionStore: store})
	m.input.SetValue("/resume")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd != nil {
		t.Fatal("expected /resume to be handled without starting an agent run")
	}
	// Bare /resume now opens the interactive session picker (like /model & /provider).
	if next.picker == nil || next.picker.kind != pickerSession {
		t.Fatalf("expected /resume to open the session picker, got picker=%#v", next.picker)
	}
	// Every row carries the session title after the timestamp and resolves by its
	// hidden session id. Raw ids are implementation detail and must not consume
	// the visible title width.
	findByID := func(id string) (pickerItem, bool) {
		for _, item := range next.picker.items {
			if item.Value == id {
				return item, true
			}
		}
		return pickerItem{}, false
	}
	for _, want := range []struct{ title, id string }{{"Newer", second.SessionID}, {"Older", first.SessionID}} {
		item, ok := findByID(want.id)
		if !ok {
			t.Fatalf("picker missing session id %q; items=%#v", want.id, next.picker.items)
		}
		if !strings.Contains(item.Label, want.title) {
			t.Fatalf("picker Label %q should contain the title %q", item.Label, want.title)
		}
		if item.Meta != "" {
			t.Fatalf("picker %q should not expose raw session id metadata, got %q", want.title, item.Meta)
		}
	}
	// The picker overlay renders clean title rows plus a position indicator.
	view := viewString(next.View())
	for _, want := range []string{"Resume a session", "Newer", "Older", "1 / 2"} {
		if !strings.Contains(view, want) {
			t.Fatalf("session picker view missing %q:\n%s", want, view)
		}
	}
	for _, id := range []string{first.SessionID, second.SessionID} {
		if strings.Contains(view, id) {
			t.Fatalf("session picker view should not expose raw id %q:\n%s", id, view)
		}
	}

	// IDs remain searchable even though they are hidden from normal rows.
	searchByID := *next.picker
	searchByID.query = first.SessionID
	searchByID.applyQuery()
	if len(searchByID.items) != 1 || searchByID.items[0].Value != first.SessionID {
		t.Fatalf("hidden session id should remain searchable, got %#v", searchByID.items)
	}

	// An empty result set reports an unambiguous zero position.
	noResults := next
	noResults.picker.query = "__missing_session__"
	noResults.picker.applyQuery()
	if view := viewString(noResults.View()); !strings.Contains(view, "0 / 0") {
		t.Fatalf("empty session search should show 0 / 0:\n%s", view)
	}
}

func TestSessionPickerLabelAlignsTitles(t *testing.T) {
	recent := sessionPickerLabel("2h ago", "Recent title")
	older := sessionPickerLabel("2006-01-02", "Older title")

	recentColumn := strings.Index(recent, "Recent title")
	olderColumn := strings.Index(older, "Older title")
	if recentColumn < 0 || olderColumn < 0 {
		t.Fatalf("sessionPickerLabel omitted a title: recent=%q older=%q", recent, older)
	}
	if recentColumn != olderColumn {
		t.Fatalf("title columns differ: recent=%d (%q), older=%d (%q)", recentColumn, recent, olderColumn, older)
	}
}

func TestResumePickerSelectionHydratesSession(t *testing.T) {
	store := testSessionStore(t)
	target, err := store.Create(sessions.CreateInput{Title: "Pick me", ModelID: "gpt-4.1", Provider: "openai"})
	if err != nil {
		t.Fatalf("Create target: %v", err)
	}
	if _, err := store.AppendEvent(target.SessionID, sessions.AppendEventInput{Type: sessions.EventMessage, Payload: map[string]any{"content": "hello"}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := store.Create(sessions.CreateInput{Title: "Other", ModelID: "x", Provider: "y"}); err != nil {
		t.Fatalf("Create other: %v", err)
	}

	m := newModel(context.Background(), Options{SessionStore: store})
	m.input.SetValue("/resume")
	updated, _ := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if m.picker == nil || m.picker.kind != pickerSession {
		t.Fatalf("expected the session picker to open, got %#v", m.picker)
	}
	for i, item := range m.picker.items {
		if item.Value == target.SessionID {
			m.picker.selected = i
		}
	}

	updated, cmd := m.Update(testKey(tea.KeyEnter)) // choosePicker
	next := updated.(model)
	if cmd != nil {
		t.Fatal("selecting a session to resume should not start an agent run")
	}
	if next.picker != nil {
		t.Fatal("picker should close after a selection")
	}
	if next.activeSession.SessionID != target.SessionID {
		t.Fatalf("active session = %q, want %q", next.activeSession.SessionID, target.SessionID)
	}
	if !transcriptContains(next.transcript, "Resumed Zero session") || !transcriptContains(next.transcript, target.SessionID) {
		t.Fatalf("expected the resume summary in the transcript, got %#v", next.transcript)
	}
}

func TestResumePickerHidesEmptyFailedSessions(t *testing.T) {
	store := testSessionStore(t)
	real, err := store.Create(sessions.CreateInput{Title: "Real one", ModelID: "gpt-4.1", Provider: "openai"})
	if err != nil {
		t.Fatalf("Create real: %v", err)
	}
	if _, err := store.AppendEvent(real.SessionID, sessions.AppendEventInput{Type: sessions.EventMessage, Payload: map[string]any{"role": "user", "content": "do a thing"}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := store.AppendEvent(real.SessionID, sessions.AppendEventInput{Type: sessions.EventMessage, Payload: map[string]any{"role": "assistant", "content": "here is the thing"}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// An empty/failed run: prompt + the no-output guardrail stop, nothing else.
	empty, err := store.Create(sessions.CreateInput{Title: "Empty one", ModelID: "gpt-4.1", Provider: "openai"})
	if err != nil {
		t.Fatalf("Create empty: %v", err)
	}
	if _, err := store.AppendEvent(empty.SessionID, sessions.AppendEventInput{Type: sessions.EventMessage, Payload: map[string]any{"role": "user", "content": "do a thing"}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := store.AppendEvent(empty.SessionID, sessions.AppendEventInput{Type: sessions.EventMessage, Payload: map[string]any{"role": "assistant", "content": "Agent stopped after 3 turns with no output (no visible text and no tool calls) to avoid consuming tokens without making progress."}}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	picker := newModel(context.Background(), Options{SessionStore: store}).newSessionPicker()
	if picker == nil {
		t.Fatal("expected a picker containing the real session")
	}
	for _, item := range picker.items {
		if item.Value == empty.SessionID {
			t.Fatalf("empty/no-output session must be hidden from the picker: %#v", picker.items)
		}
	}
	shown := false
	for _, item := range picker.items {
		if item.Value == real.SessionID {
			shown = true
		}
	}
	if !shown {
		t.Fatalf("the real session must be shown: %#v", picker.items)
	}
}

func TestResumeHonorsPriorCompaction(t *testing.T) {
	store := testSessionStore(t)
	session, err := store.Create(sessions.CreateInput{Title: "Compacted", ModelID: "gpt-4.1", Provider: "openai"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, content := range []string{"alpha", "beta", "gamma", "delta"} {
		if _, err := store.AppendEvent(session.SessionID, sessions.AppendEventInput{Type: sessions.EventMessage, Payload: map[string]string{"content": content}}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	plan, err := store.PlanCompaction(session.SessionID, sessions.CompactionOptions{PreserveLast: 2, MaxPromptChars: 500})
	if err != nil {
		t.Fatalf("PlanCompaction: %v", err)
	}
	if _, err := store.RecordCompaction(session.SessionID, sessions.RecordCompactionInput{Plan: plan, Summary: "early summary"}); err != nil {
		t.Fatalf("RecordCompaction: %v", err)
	}
	if _, err := store.AppendEvent(session.SessionID, sessions.AppendEventInput{Type: sessions.EventMessage, Payload: map[string]string{"content": "epsilon"}}); err != nil {
		t.Fatalf("Append epsilon: %v", err)
	}

	raw, err := store.ReadEvents(session.SessionID)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	rehydrated, err := store.ReadRehydratedEvents(session.SessionID)
	if err != nil {
		t.Fatalf("ReadRehydratedEvents: %v", err)
	}
	if len(rehydrated) >= len(raw) {
		t.Fatalf("test setup invalid: rehydrated (%d) should be < raw (%d)", len(rehydrated), len(raw))
	}

	m := newModel(context.Background(), Options{SessionStore: store})
	next, _ := m.handleResumeCommand(session.SessionID)
	// Resume must load the rehydrated (compaction-aware) context, not the raw log —
	// matching the CLI's --resume and the in-TUI /compact reload. Compare contents,
	// not just length: a regression that returned a same-length but reordered or
	// substituted slice (e.g. a dropped original in place of the summary) would
	// slip past a length check.
	if !reflect.DeepEqual(next.sessionEvents, rehydrated) {
		t.Fatalf("resumed sessionEvents do not match the rehydrated context (resume must honor prior compaction)\nresumed:    %+v\nrehydrated: %+v\nraw:        %+v", next.sessionEvents, rehydrated, raw)
	}
}

func TestResumeCommandWithUnknownIDReportsMissingSession(t *testing.T) {
	m := newModel(context.Background(), Options{SessionStore: testSessionStore(t)})
	m.input.SetValue("/resume zero_123")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if !transcriptContains(next.transcript, "zero session not found: zero_123") {
		t.Fatalf("expected missing session message, got %#v", next.transcript)
	}
}

func TestPromptSubmitAppendsUserAndAssistantRows(t *testing.T) {
	provider := &fakeProvider{events: []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventText, Content: "hello"},
		{Type: zeroruntime.StreamEventText, Content: " back"},
		{Type: zeroruntime.StreamEventDone},
	}}
	m := newModel(context.Background(), Options{
		Provider:     provider,
		Registry:     tools.NewRegistry(),
		SessionStore: testSessionStore(t),
	})
	m.input.SetValue("say hi")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if !transcriptContains(next.transcript, "say hi") {
		t.Fatalf("expected user row after submit, got %#v", next.transcript)
	}
	if cmd == nil {
		t.Fatal("expected submit to return agent command")
	}

	msg := execCmd(cmd)
	updated, _ = next.Update(msg)
	next = updated.(model)
	if !transcriptContains(next.transcript, "hello back") {
		t.Fatalf("expected assistant row after agent response, got %#v", next.transcript)
	}
}

func TestPromptSubmitDoesNotStartAnotherRunWhilePending(t *testing.T) {
	m := newModel(context.Background(), Options{
		Provider: &fakeProvider{},
		Registry: tools.NewRegistry(),
	})
	m.pending = true
	m.input.SetValue("second prompt")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd != nil {
		t.Fatal("expected no command while another run is pending")
	}
	if transcriptContains(next.transcript, "second prompt") {
		t.Fatalf("pending prompt should not be appended, got %#v", next.transcript)
	}
	if !next.pending {
		t.Fatal("expected existing pending run to remain pending")
	}
}

func TestEscRequiresSecondPressToCancelPendingRun(t *testing.T) {
	m := newModel(context.Background(), Options{})
	cancelled := false
	m.pending = true
	m.activeRunID = 1
	m.runCancel = func() { cancelled = true }

	updated, cmd := m.Update(testKey(tea.KeyEsc))
	next := updated.(model)

	if cancelled {
		t.Fatal("first Esc should not cancel the pending run")
	}
	if !next.pending {
		t.Fatal("first Esc should leave the run pending")
	}
	if !next.cancelConfirmActive {
		t.Fatal("first Esc should arm cancel confirmation")
	}
	if cmd == nil {
		t.Fatal("first Esc should schedule confirmation expiry")
	}
	status := plainRender(t, next.statusLine(80))
	if !strings.Contains(status, escCancelConfirmText) {
		t.Fatalf("status line = %q, want cancel confirmation", status)
	}

	updated, _ = next.Update(testKey(tea.KeyEsc))
	next = updated.(model)

	if !cancelled {
		t.Fatal("second Esc should cancel pending run")
	}
	if next.pending {
		t.Fatal("expected second Esc to clear pending state")
	}
	if next.activeRunID != 0 || next.runCancel != nil {
		t.Fatalf("expected active run state to clear, got id=%d cancel=%v", next.activeRunID, next.runCancel)
	}
	if next.cancelConfirmActive {
		t.Fatal("cancelling should clear cancel confirmation")
	}
}

// TestEscArmingCancelConfirmationPreservesComposerDraft: the first Esc only
// arms the confirmation — nothing has actually been cancelled yet, so it
// must not destroy a draft the user is still typing. Only the confirming
// second Esc (the one that actually cancels the run) clears the composer.
func TestEscArmingCancelConfirmationPreservesComposerDraft(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.pending = true
	m.activeRunID = 1
	m.runCancel = func() {}
	m.input.SetValue("draft prompt")

	updated, _ := m.Update(testKey(tea.KeyEsc))
	next := updated.(model)

	if !next.cancelConfirmActive {
		t.Fatal("first Esc should arm cancel confirmation")
	}
	if next.composerValue() != "draft prompt" {
		t.Fatalf("first Esc should preserve the draft, got %q", next.composerValue())
	}

	updated, _ = next.Update(testKey(tea.KeyEsc))
	next = updated.(model)

	if next.pending {
		t.Fatal("second Esc should cancel the pending run")
	}
	if next.composerValue() != "" {
		t.Fatalf("the confirming second Esc should clear the draft, got %q", next.composerValue())
	}
}

// TestPasteDisarmsCancelConfirmation: a paste isn't a keypress, so it never
// went through the generic "any non-Esc key disarms it" hook. Pasting is a
// deliberate action just like typing or clicking — it must disarm a stale
// confirmation too, or a later, unrelated Esc could silently cancel the run.
func TestPasteDisarmsCancelConfirmation(t *testing.T) {
	m := newModel(context.Background(), Options{})
	cancelled := false
	m.pending = true
	m.activeRunID = 1
	m.runCancel = func() { cancelled = true }

	updated, _ := m.Update(testKey(tea.KeyEsc))
	next := updated.(model)
	if !next.cancelConfirmActive {
		t.Fatal("first Esc should arm cancel confirmation")
	}

	updated, _ = next.Update(testPaste("pasted text"))
	next = updated.(model)
	if next.cancelConfirmActive {
		t.Fatal("a paste should disarm the stale cancel confirmation")
	}

	updated, _ = next.Update(testKey(tea.KeyEsc))
	next = updated.(model)
	if cancelled {
		t.Fatal("Esc after a paste should re-arm, not immediately cancel")
	}
	if !next.cancelConfirmActive {
		t.Fatal("Esc after a paste should arm a fresh confirmation")
	}
}

func TestEscCancelConfirmationExpires(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.pending = true
	m.activeRunID = 1
	m.runCancel = func() {}

	updated, _ := m.Update(testKey(tea.KeyEsc))
	next := updated.(model)
	seq := next.cancelConfirmSeq

	updated, _ = next.Update(cancelConfirmExpiredMsg{seq: seq - 1})
	next = updated.(model)
	if !next.cancelConfirmActive {
		t.Fatal("stale expiry should not clear active cancel confirmation")
	}

	updated, _ = next.Update(cancelConfirmExpiredMsg{seq: seq})
	next = updated.(model)
	if next.cancelConfirmActive {
		t.Fatal("matching expiry should clear cancel confirmation")
	}
	if !next.pending {
		t.Fatal("expiring the confirmation should not cancel the run")
	}
}

// TestEscCancelConfirmationDisarmsOnInterveningKey mirrors
// TestCtrlCExitConfirmationDisarmsOnInterveningKey: an intervening key other
// than Esc (typing, Ctrl+O, etc.) means the user moved on to something else,
// so a later, unrelated Esc must arm a fresh confirmation instead of
// silently cancelling off a stale press from seconds ago.
func TestEscCancelConfirmationDisarmsOnInterveningKey(t *testing.T) {
	m := newModel(context.Background(), Options{})
	cancelled := false
	m.pending = true
	m.activeRunID = 1
	m.runCancel = func() { cancelled = true }

	updated, _ := m.Update(testKey(tea.KeyEsc))
	next := updated.(model)
	if !next.cancelConfirmActive {
		t.Fatal("first Esc should arm cancel confirmation")
	}
	seq := next.cancelConfirmSeq

	updated, _ = next.Update(testKey('h'))
	next = updated.(model)
	if next.cancelConfirmActive {
		t.Fatal("an intervening non-Esc key should disarm cancel confirmation")
	}
	if next.cancelConfirmSeq == seq {
		t.Fatal("disarming should advance the sequence so a stale expiry tick is ignored")
	}

	updated, _ = next.Update(testKey(tea.KeyEsc))
	next = updated.(model)
	if cancelled {
		t.Fatal("Esc after an intervening key should re-arm, not cancel")
	}
	if !next.cancelConfirmActive {
		t.Fatal("Esc after an intervening key should arm a fresh confirmation")
	}
}

// TestEscCancelConfirmationDisarmsWhenStolenByAskUser: a mid-turn ask_user
// prompt lands between the two Esc presses. The user's second Esc denies the
// questionnaire (an earlier branch in the Esc handler), not a confirm, so it
// must not leave cancelConfirmActive armed for a later, unrelated Esc to
// silently cancel the run.
func TestEscCancelConfirmationDisarmsWhenStolenByAskUser(t *testing.T) {
	m := newModel(context.Background(), Options{})
	cancelled := false
	m.pending = true
	m.activeRunID = 1
	m.runCancel = func() { cancelled = true }

	updated, _ := m.Update(testKey(tea.KeyEsc))
	next := updated.(model)
	if !next.cancelConfirmActive {
		t.Fatal("first Esc should arm cancel confirmation")
	}

	request := agent.AskUserRequest{Questions: []agent.AskUserQuestion{{Question: "name?"}}}
	next.pendingAskUser = &pendingAskUserPrompt{
		request: request,
		answer:  func([]string) {},
		states:  newAskUserStates(request.Questions),
	}

	updated, _ = next.Update(testKey(tea.KeyEsc))
	next = updated.(model)
	if cancelled {
		t.Fatal("an Esc consumed by the ask-user prompt must not cancel the run")
	}
	if next.cancelConfirmActive {
		t.Fatal("an Esc consumed by the ask-user prompt must disarm the stale cancel confirmation")
	}

	updated, _ = next.Update(testKey(tea.KeyEsc))
	next = updated.(model)
	if cancelled {
		t.Fatal("the next Esc should arm a fresh confirmation, not immediately cancel")
	}
	if !next.cancelConfirmActive {
		t.Fatal("the next Esc should arm a fresh confirmation")
	}
}

func TestAgentResponseCompletesStuckPlan(t *testing.T) {
	runningPlan := func() planPanelState {
		var s planPanelState
		s.updateFromItems([]tools.PlanItem{
			{Content: "a", Status: "completed"},
			{Content: "b", Status: "in_progress"},
			{Content: "c", Status: "pending"},
		}, time.Now())
		return s
	}

	t.Run("successful turn reconciles the stuck plan to complete", func(t *testing.T) {
		m := newModel(context.Background(), Options{})
		m.pending = true
		m.activeRunID = 7
		m.plan = runningPlan()
		updated, _ := m.Update(agentResponseMsg{runID: 7, rows: []transcriptRow{{kind: rowAssistant, text: "done", final: true}}})
		if next := updated.(model); !next.plan.isComplete() {
			t.Fatalf("a successful turn should complete the plan, steps=%+v", next.plan.steps)
		}
	})

	t.Run("errored turn leaves the plan incomplete", func(t *testing.T) {
		m := newModel(context.Background(), Options{})
		m.pending = true
		m.activeRunID = 7
		m.plan = runningPlan()
		updated, _ := m.Update(agentResponseMsg{runID: 7, err: errors.New("boom")})
		if next := updated.(model); next.plan.isComplete() {
			t.Error("an errored turn must not force the plan complete")
		}
	})

	t.Run("pending ask_user leaves the plan incomplete", func(t *testing.T) {
		m := newModel(context.Background(), Options{})
		m.pending = true
		m.activeRunID = 7
		m.plan = runningPlan()
		m.pendingAskUser = &pendingAskUserPrompt{}
		updated, _ := m.Update(agentResponseMsg{runID: 7, rows: []transcriptRow{{kind: rowAssistant, text: "done", final: true}}})
		if next := updated.(model); next.plan.isComplete() {
			t.Error("a mid-plan ask_user yield must not force the plan complete")
		}
	})
}

func TestAgentResponseSurfacesRunCompletionWarning(t *testing.T) {
	m := newModel(context.Background(), Options{
		RunCompletionWarning: func() string { return "scratch warning" },
	})
	m.pending = true
	m.activeRunID = 7
	updated, _ := m.Update(agentResponseMsg{runID: 7, rows: []transcriptRow{{kind: rowAssistant, text: "done", final: true}}})
	next := updated.(model)
	if !transcriptContains(next.transcript, "scratch warning") {
		t.Fatalf("expected run completion warning in transcript, got %#v", next.transcript)
	}
}

func TestBeginRunPreparesRunCompletionWarningEachTurn(t *testing.T) {
	prepares := 0
	m := newModel(context.Background(), Options{
		PrepareRunCompletionWarning: func() { prepares++ },
	})

	m = m.beginRun(nil)
	_ = m.beginRun(nil)

	if prepares != 2 {
		t.Fatalf("PrepareRunCompletionWarning called %d times, want once per run", prepares)
	}
}

// TestToolResultDetailPrefersPreview: the card body uses the rich card-only
// Display.Preview on a successful result, falls back to Output when there's no
// preview, and always uses Output (the failure) on an error.
func TestToolResultDetailPrefersPreview(t *testing.T) {
	withPreview := agent.ToolResult{
		Name:    "write_file",
		Status:  tools.StatusOK,
		Output:  "Created x.js (300 lines).",
		Display: tools.Display{Summary: "Created x.js (300 lines).", Kind: "file", Preview: "--- /dev/null\n+++ b/x.js\n@@ -0,0 +1,300 @@\n+const x = 1"},
	}
	if got := toolResultDetail(withPreview); !strings.Contains(got, "+const x = 1") {
		t.Errorf("successful result should use the preview, got %q", got)
	}

	noPreview := agent.ToolResult{Name: "bash", Status: tools.StatusOK, Output: "exit 0"}
	if got := toolResultDetail(noPreview); got != "exit 0" {
		t.Errorf("no preview: want Output, got %q", got)
	}

	errResult := agent.ToolResult{Name: "write_file", Status: tools.StatusError, Output: "Error: permission denied", Display: tools.Display{Preview: "must not show on error"}}
	if got := toolResultDetail(errResult); got != "Error: permission denied" {
		t.Errorf("error result must use Output (the failure), got %q", got)
	}
}

func TestToolResultSessionPayloadKeepsPreviewForResume(t *testing.T) {
	preview := "--- a/calculator.go\n+++ b/calculator.go\n@@ -4,1 +4,1 @@\n-oldValue := 1\n+newValue := 2"
	payload := toolResultSessionPayload(agent.ToolResult{
		ToolCallID: "edit-1",
		Name:       "edit_file",
		Status:     tools.StatusOK,
		Output:     "Successfully edited calculator.go (replaced 1 occurrence).",
		Display:    tools.Display{Preview: preview},
	})
	if got, want := payload["displayPreview"], preview; got != want {
		t.Fatalf("persisted display preview = %#v, want %q", got, want)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal session payload: %v", err)
	}
	rows := transcriptRowsFromSessionEvents([]sessions.Event{{Type: sessions.EventToolResult, Payload: encoded}})
	if len(rows) != 1 || rows[0].detail != preview {
		t.Fatalf("resumed row lost the display preview: %#v", rows)
	}
}

// TestReasoningRefreshesActivityClock: a reasoning delta is live provider output,
// so it must bump lastStreamActivity (else the quiet hint mis-fires mid-think).
func TestReasoningRefreshesActivityClock(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	now := base
	m := model{now: func() time.Time { return now }}
	m.activeRunID = 7
	m.lastStreamActivity = base
	now = base.Add(20 * time.Second)
	updated, _ := m.Update(agentReasoningMsg{runID: 7, delta: "thinking…"})
	if got := updated.(model).lastStreamActivity; !got.Equal(now) {
		t.Errorf("reasoning delta should refresh lastStreamActivity to now, got %v", got)
	}
}

// TestStaleExplanationDropped: a plan-step explanation result from a previous run
// (older planDetailGen) is ignored, so it can't repopulate the cleared cache.
func TestStaleExplanationDropped(t *testing.T) {
	m := model{now: time.Now, planDetailGen: 2}
	m.plan.steps = []planStep{{content: "x", status: "completed"}}

	updated, _ := m.Update(planStepExplanationMsg{stepIndex: 0, key: "k", gen: 1, text: "stale"})
	m = updated.(model)
	if _, ok := m.stepExplanation["k"]; ok {
		t.Error("a stale-generation explanation must not populate the cache")
	}

	updated, _ = m.Update(planStepExplanationMsg{stepIndex: 0, key: "k", gen: 2, text: "fresh"})
	m = updated.(model)
	if m.stepExplanation["k"] != "fresh" {
		t.Errorf("a current-gen explanation should cache, got %q", m.stepExplanation["k"])
	}
}

// TestBeginRunKeepsRunDetailsClosed verifies a new run starts on the focused
// conversation surface and advances the explanation generation.
func TestBeginRunKeepsRunDetailsClosed(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.runDetailsOpen = true
	gen := m.planDetailGen
	m = m.beginRun(nil)
	if m.runDetailsOpen {
		t.Error("beginRun should close run details for the new turn")
	}
	if m.planDetailGen <= gen {
		t.Errorf("beginRun should bump planDetailGen, was %d now %d", gen, m.planDetailGen)
	}
}

func TestStaleAgentResponseAfterCancelIsIgnored(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.pending = false
	m.activeRunID = 0
	m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendUser, text: "new prompt"})

	updated, _ := m.Update(agentResponseMsg{
		runID: 1,
		rows:  []transcriptRow{{kind: rowAssistant, text: "stale response"}},
	})
	next := updated.(model)

	if transcriptContains(next.transcript, "stale response") {
		t.Fatalf("stale response should be ignored, got %#v", next.transcript)
	}
}

func TestAgentResponsePreservesToolResultMetadata(t *testing.T) {
	diff := strings.Join([]string{
		"--- a/file.txt",
		"+++ b/file.txt",
		"@@ -1 +1 @@",
		"-old",
		"+new",
	}, "\n")
	m := newModel(context.Background(), Options{})
	m.pending = true
	m.activeRunID = 7

	updated, _ := m.Update(agentResponseMsg{
		runID: 7,
		rows: []transcriptRow{{
			kind:   rowToolResult,
			text:   "tool result: apply_patch error",
			tool:   "apply_patch",
			status: tools.StatusError,
			detail: diff,
		}},
	})
	next := updated.(model)

	row, ok := findTranscriptRow(next.transcript, rowToolResult)
	if !ok {
		t.Fatalf("expected tool result row, got %#v", next.transcript)
	}
	if row.tool != "apply_patch" || row.status != tools.StatusError || row.detail != diff {
		t.Fatalf("tool result metadata was not preserved: %#v", row)
	}
	rendered := next.renderRow(row, 80, buildRowContext(next.transcript))
	assertContains(t, rendered, "old")
	assertContains(t, rendered, "new")
	assertNotContains(t, rendered, "@@")
}

func TestAgentResponsePreservesPermissionMetadata(t *testing.T) {
	event := agent.PermissionEvent{
		ToolCallID:     "call_1",
		ToolName:       "write_file",
		Action:         agent.PermissionActionPrompt,
		Permission:     "prompt",
		PermissionMode: agent.PermissionModeAsk,
		Autonomy:       "medium",
		SideEffect:     "write",
		Reason:         "Creates or overwrites files.",
		Risk:           sandbox.Risk{Level: sandbox.RiskHigh},
	}
	m := newModel(context.Background(), Options{})
	m.pending = true
	m.activeRunID = 7

	updated, _ := m.Update(agentResponseMsg{
		runID: 7,
		rows:  []transcriptRow{permissionTranscriptRow(event)},
	})
	next := updated.(model)

	row, ok := findTranscriptRow(next.transcript, rowPermission)
	if !ok {
		t.Fatalf("expected permission row, got %#v", next.transcript)
	}
	if row.tool != "write_file" || row.permission == nil || row.permission.ToolCallID != "call_1" {
		t.Fatalf("permission metadata was not preserved: %#v", row)
	}
	rendered := next.renderRow(row, 96, buildRowContext(next.transcript))
	for _, want := range []string{"permission", "write_file", "prompt", "Creates or overwrites"} {
		assertContains(t, rendered, want)
	}
	for _, blocked := range []string{"risk:", "risk=", "mode=", "permission=", "side_effect=", "autonomy="} {
		if strings.Contains(rendered, blocked) {
			t.Fatalf("normal permission row must not render %q, got %q", blocked, rendered)
		}
	}
}

func TestPermissionRequestShowsFocusedPrompt(t *testing.T) {
	request := testPromptPermissionRequest()
	request.Scope = "src/main.go"
	m := newModel(context.Background(), Options{})
	m.pending = true
	m.activeRunID = 7
	m.width = 96

	updated, cmd := m.Update(permissionRequestMsg{
		runID:   7,
		request: request,
	})
	next := updated.(model)

	if cmd != nil {
		t.Fatal("expected permission request to update TUI state synchronously")
	}
	if next.pendingPermission == nil {
		t.Fatalf("expected permission prompt to be pending, got %#v", next)
	}
	if countTranscriptRows(next.transcript, rowPermission) != 1 {
		t.Fatalf("expected permission request to append one permission row, got %#v", next.transcript)
	}
	row, ok := findTranscriptRow(next.transcript, rowPermission)
	if !ok || row.permission == nil || row.permission.Scope != request.Scope {
		t.Fatalf("expected permission row to preserve scope %q, got %#v", request.Scope, row)
	}
	view := plainRender(t, next.View())
	for _, want := range []string{"write_file", "Yes, proceed", "[a]", "these files in this session", "[s]", "don't ask again for this scope", "[y]", "continue without running it", "[d]", "scope: src/main.go", "Creates or overwrites files."} {
		assertContains(t, view, want)
	}
	if strings.Contains(view, "risk:") || strings.Contains(view, "risk=") {
		t.Fatalf("focused permission prompt must not render risk labels, got %q", view)
	}
	if count := strings.Count(view, request.Reason); count != 1 {
		t.Fatalf("focused permission reason rendered %d times, want once:\n%s", count, view)
	}
}

func TestPermissionPromptChoicesResolveDecision(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want permissionDecision
	}{
		{name: "allow", key: "a", want: permissionDecisionAllow},
		{name: "deny", key: "d", want: permissionDecisionDeny},
		{name: "session", key: "s", want: permissionDecisionAllowForSession},
		{name: "always", key: "y", want: permissionDecisionAlwaysAllow},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decisions := []permissionDecision{}
			m := newModel(context.Background(), Options{})
			m.pending = true
			m.activeRunID = 7
			updated, _ := m.Update(permissionRequestMsg{
				runID:   7,
				request: testPromptPermissionRequest(),
				decide: func(decision agent.PermissionDecision) {
					decisions = append(decisions, permissionDecision(decision.Action))
				},
			})
			next := updated.(model)

			updated, cmd := next.Update(testKeyText(tc.key))
			next = updated.(model)

			if cmd != nil {
				t.Fatal("expected permission choice to resolve synchronously")
			}
			if len(decisions) != 1 || decisions[0] != tc.want {
				t.Fatalf("expected decision %q, got %#v", tc.want, decisions)
			}
			if next.pendingPermission != nil {
				t.Fatalf("expected permission prompt to clear after choice, got %#v", next.pendingPermission)
			}
		})
	}
}

func TestPermissionPromptBlocksNormalSubmit(t *testing.T) {
	decisions := []permissionDecision{}
	m := newModel(context.Background(), Options{})
	m.pending = true
	m.activeRunID = 7
	updated, _ := m.Update(permissionRequestMsg{
		runID:   7,
		request: testPromptPermissionRequest(),
		decide: func(decision agent.PermissionDecision) {
			decisions = append(decisions, permissionDecision(decision.Action))
		},
	})
	next := updated.(model)
	next.input.SetValue("second prompt")

	updated, cmd := next.Update(testKey(tea.KeyEnter))
	next = updated.(model)

	if cmd != nil {
		t.Fatal("expected permission confirm to resolve synchronously (no cmd)")
	}
	// Enter confirms the highlighted option (default: approve) -- it must NOT
	// submit the composer's pending text as a new prompt.
	if len(decisions) != 1 || decisions[0] != permissionDecisionAllow {
		t.Fatalf("expected Enter to confirm the default approval option, got %#v", decisions)
	}
	if transcriptContains(next.transcript, "second prompt") {
		t.Fatalf("permission prompt should block normal prompt submit, got %#v", next.transcript)
	}
	if next.pendingPermission != nil {
		t.Fatalf("expected permission prompt to clear after confirm, got %#v", next.pendingPermission)
	}
}

func TestPermissionRowRendersSandboxBlocks(t *testing.T) {
	block := sandbox.Block{
		Code:        sandbox.BlockOutsideWorkspace,
		ToolName:    "write_file",
		Action:      sandbox.ActionDeny,
		Risk:        sandbox.Risk{Level: sandbox.RiskCritical},
		Path:        "../secret.txt",
		Reason:      "writes must stay inside workspace",
		Recoverable: false,
	}
	event := agent.PermissionEvent{
		ToolCallID:     "call_2",
		ToolName:       "write_file",
		Action:         agent.PermissionActionDeny,
		Permission:     "prompt",
		PermissionMode: agent.PermissionModeUnsafe,
		Autonomy:       "high",
		SideEffect:     "write",
		Reason:         "workspace boundary enforced",
		Risk:           sandbox.Risk{Level: sandbox.RiskHigh},
		Block:          &block,
	}

	rendered := newModel(context.Background(), Options{}).renderRow(permissionTranscriptRow(event), 96, buildRowContext(nil))

	for _, want := range []string{"write_file", "denied", "outside workspace", "../secret.txt"} {
		assertContains(t, rendered, want)
	}
	for _, blocked := range []string{"risk:", "risk=", "block=", "mode=", "permission=", "side_effect=", "autonomy="} {
		if strings.Contains(rendered, blocked) {
			t.Fatalf("denied permission row must not render %q, got %q", blocked, rendered)
		}
	}
}

func TestPermissionRowRendersIdenticalEventAndBlockReasonOnce(t *testing.T) {
	const reason = "Reading /home/dev/.nvm requires access outside the workspace."
	event := agent.PermissionEvent{
		ToolCallID:     "call_read",
		ToolName:       "list_directory",
		Action:         agent.PermissionActionDeny,
		DecisionAction: agent.PermissionDecisionDeny,
		SideEffect:     "read",
		Reason:         reason,
		Scope:          "/home/dev/.nvm",
		Block: &sandbox.Block{
			Code:   sandbox.BlockOutsideWorkspace,
			Path:   "/home/dev/.nvm",
			Reason: reason,
		},
	}

	row := permissionTranscriptRow(event)
	if count := strings.Count(event.Reason+"\n"+row.detail, reason); count != 1 {
		t.Fatalf("permission reason represented %d times, want once: detail=%q", count, row.detail)
	}
	for _, want := range []string{"denied by user", "outside workspace", "path: /home/dev/.nvm"} {
		if !strings.Contains(row.detail, want) {
			t.Fatalf("permission detail missing %q: %q", want, row.detail)
		}
	}
}

func TestAppendTranscriptRowDedupesRuntimeRowsByID(t *testing.T) {
	event := agent.PermissionEvent{
		ToolCallID: "call_1",
		ToolName:   "write_file",
		Action:     agent.PermissionActionPrompt,
	}
	rows := initialTranscript()
	rows = appendTranscriptRow(rows, transcriptRow{kind: rowToolCall, id: "call_1", text: "tool call: write_file", tool: "write_file"})
	rows = appendTranscriptRow(rows, permissionTranscriptRow(event))
	rows = appendTranscriptRow(rows, transcriptRow{kind: rowToolResult, id: "call_1", text: "tool result: write_file error", tool: "write_file", status: tools.StatusError})

	rows = appendTranscriptRow(rows, transcriptRow{kind: rowToolCall, id: "call_1", text: "tool call: write_file", tool: "write_file"})
	rows = appendTranscriptRow(rows, permissionTranscriptRow(event))
	rows = appendTranscriptRow(rows, transcriptRow{kind: rowToolResult, id: "call_1", text: "tool result: write_file error", tool: "write_file", status: tools.StatusError})

	if len(rows) != 4 {
		t.Fatalf("expected welcome plus three unique runtime rows, got %#v", rows)
	}
}

func TestAgentEventRenderingMappingCoversRuntimeContract(t *testing.T) {
	surfaces := map[zeroruntime.AgentEventType]string{
		zeroruntime.AgentEventText:       "assistant transcript row",
		zeroruntime.AgentEventToolCall:   "tool call transcript row",
		zeroruntime.AgentEventToolResult: "tool result transcript row",
		zeroruntime.AgentEventThinking:   "deferred: no transcript row until runtime emits thinking deltas",
		zeroruntime.AgentEventUsage:      "usage tracker footer segment",
		zeroruntime.AgentEventPlanUpdate: "system transcript row from /plan",
		zeroruntime.AgentEventError:      "error transcript row",
		zeroruntime.AgentEventTurnEnd:    "control boundary, no transcript row",
	}
	for _, eventType := range []zeroruntime.AgentEventType{
		zeroruntime.AgentEventText,
		zeroruntime.AgentEventToolCall,
		zeroruntime.AgentEventToolResult,
		zeroruntime.AgentEventThinking,
		zeroruntime.AgentEventUsage,
		zeroruntime.AgentEventPlanUpdate,
		zeroruntime.AgentEventError,
		zeroruntime.AgentEventTurnEnd,
	} {
		if strings.TrimSpace(surfaces[eventType]) == "" {
			t.Fatalf("missing TUI rendering surface note for %s", eventType)
		}
	}

	renderedRows := map[zeroruntime.AgentEventType]struct {
		row   transcriptRow
		wants []string
	}{
		zeroruntime.AgentEventText: {
			row:   transcriptRow{kind: rowAssistant, text: "assistant text"},
			wants: []string{"assistant text"},
		},
		zeroruntime.AgentEventToolCall: {
			row: transcriptRow{
				kind:   rowToolCall,
				text:   "tool call: read_file",
				tool:   "read_file",
				detail: "README.md",
			},
			wants: []string{"Read", "README.md"},
		},
		zeroruntime.AgentEventToolResult: {
			row: transcriptRow{
				kind:   rowToolResult,
				text:   "tool result: apply_patch error",
				tool:   "apply_patch",
				status: tools.StatusError,
				detail: strings.Join([]string{
					"--- a/file.txt",
					"+++ b/file.txt",
					"@@ -1 +1 @@",
					"-old",
					"+new",
				}, "\n"),
			},
			wants: []string{"Patched", "file.txt", "old", "new"},
		},
		zeroruntime.AgentEventPlanUpdate: {
			row:   transcriptRow{kind: rowSystem, text: "Plan updated\n- inspect: completed"},
			wants: []string{"Plan updated", "inspect"},
		},
		zeroruntime.AgentEventError: {
			row:   transcriptRow{kind: rowError, text: "provider failed"},
			wants: []string{"provider failed"},
		},
	}
	for eventType, tc := range renderedRows {
		t.Run(string(eventType), func(t *testing.T) {
			rendered := newModel(context.Background(), Options{}).renderRow(tc.row, 96, buildRowContext(nil))
			for _, want := range tc.wants {
				assertContains(t, rendered, want)
			}
		})
	}

	m := newModel(context.Background(), Options{
		ModelName:      "gpt-4.1",
		PermissionMode: agent.PermissionModeAsk,
	})
	m.width = 96
	m, usageRows := m.recordUsageEvent("gpt-4.1", zeroruntime.Usage{InputTokens: 100, OutputTokens: 20})
	if len(usageRows) != 0 {
		t.Fatalf("valid usage should update footer without transcript rows, got %#v", usageRows)
	}
	assertContains(t, m.usageStatusSegment(), "120 tok")
	assertContains(t, m.composerDividerLine(96), "gpt-4.1")
	// Permission mode moved from the composer rule to the status line.
	assertContains(t, m.statusLine(96), "ask")
}

func TestToolResultRowDefaultsEmptyStatusToOK(t *testing.T) {
	text := toolResultRowText(agent.ToolResult{Name: "read_file", Output: "done"})

	if !strings.Contains(text, "read_file ok done") {
		t.Fatalf("expected empty status to render as ok, got %q", text)
	}
}

func TestToolResultRowTruncatesLongOutput(t *testing.T) {
	text := toolResultRowText(agent.ToolResult{Name: "read_file", Output: strings.Repeat("x", tuiToolOutputLimit+20)})

	if !strings.Contains(text, "[truncated]") || len(text) >= tuiToolOutputLimit+80 {
		t.Fatalf("expected truncated tool output, got len=%d text=%q", len(text), text)
	}
}

func TestShiftTabCyclesPermissionMode(t *testing.T) {
	m := newModel(context.Background(), Options{PermissionMode: agent.PermissionModeAuto})
	m.width = 96

	// shift+tab toggles Auto<->Ask only; Unsafe is intentionally NOT reachable by
	// a casual keypress (it disables permission prompts).
	for _, want := range []agent.PermissionMode{
		agent.PermissionModeAsk,
		agent.PermissionModeAuto,
	} {
		updated, cmd := m.Update(testKeyShift(tea.KeyTab))
		m = updated.(model)
		if cmd != nil {
			t.Fatalf("expected shift+tab to cycle mode synchronously, got command")
		}
		if m.permissionMode != want {
			t.Fatalf("expected permission mode %q after shift+tab, got %q", want, m.permissionMode)
		}
		if m.permissionMode == agent.PermissionModeUnsafe {
			t.Fatalf("shift+tab must never land on Unsafe")
		}
	}

	// The rendered status label tracks the cycled mode.
	label, _ := m.modeLabel()
	if label != "auto-approve" {
		t.Fatalf("expected mode label to track cycled mode, got %q", label)
	}
}

func TestShiftTabDoesNotCycleWhileModalsActive(t *testing.T) {
	// Permission modal, ask_user prompt, and an open picker all take precedence:
	// shift+tab must not change the mode while any is up.
	t.Run("permission", func(t *testing.T) {
		m := newModel(context.Background(), Options{PermissionMode: agent.PermissionModeAuto})
		m.pending = true
		m.activeRunID = 7
		updated, _ := m.Update(permissionRequestMsg{runID: 7, request: testPromptPermissionRequest()})
		next := updated.(model)
		updated, _ = next.Update(testKeyShift(tea.KeyTab))
		next = updated.(model)
		if next.permissionMode != agent.PermissionModeAuto {
			t.Fatalf("expected mode unchanged while permission modal is up, got %q", next.permissionMode)
		}
		if next.pendingPermission == nil {
			t.Fatal("expected permission prompt to remain pending")
		}
	})
	t.Run("ask_user", func(t *testing.T) {
		m := newModel(context.Background(), Options{PermissionMode: agent.PermissionModeAuto})
		m.pending = true
		m.activeRunID = 7
		updated, _ := m.Update(askUserRequestMsg{runID: 7, request: testAskUserRequest(), answer: func([]string) {}})
		next := updated.(model)
		updated, _ = next.Update(testKeyShift(tea.KeyTab))
		next = updated.(model)
		if next.permissionMode != agent.PermissionModeAuto {
			t.Fatalf("expected mode unchanged while ask_user prompt is up, got %q", next.permissionMode)
		}
		if next.pendingAskUser == nil {
			t.Fatal("expected ask_user prompt to remain pending")
		}
	})
	t.Run("picker", func(t *testing.T) {
		m := newModel(context.Background(), Options{
			ProviderName:   "openai",
			ModelName:      "gpt-4.1",
			Provider:       &fakeProvider{},
			PermissionMode: agent.PermissionModeAuto,
		})
		m.input.SetValue("/model")
		updated, _ := m.Update(testKey(tea.KeyEnter))
		next := updated.(model)
		if next.picker == nil {
			t.Skip("model picker unavailable in test environment")
		}
		updated, _ = next.Update(testKeyShift(tea.KeyTab))
		next = updated.(model)
		if next.permissionMode != agent.PermissionModeAuto {
			t.Fatalf("expected mode unchanged while picker is open, got %q", next.permissionMode)
		}
	})
}

func TestCtrlCRequiresSecondPressToExit(t *testing.T) {
	m := newModel(context.Background(), Options{ProviderName: "tokenrouter"})

	updated, cmd := m.Update(testKeyCtrl('c'))
	next := updated.(model)

	if next.exiting {
		t.Fatal("first Ctrl+C should not mark model exiting")
	}
	if !next.exitConfirmActive {
		t.Fatal("first Ctrl+C should arm exit confirmation")
	}
	if cmd == nil {
		t.Fatal("first Ctrl+C should schedule confirmation expiry")
	}
	status := plainRender(t, next.statusLine(80))
	if !strings.Contains(status, ctrlCExitConfirmText) {
		t.Fatalf("status line = %q, want exit confirmation", status)
	}
	if strings.Contains(status, "tokenrouter") {
		t.Fatalf("status line = %q, should replace provider while exit confirmation is active", status)
	}

	updated, cmd = next.Update(testKeyCtrl('c'))
	next = updated.(model)
	if !next.exiting {
		t.Fatal("second Ctrl+C should mark model exiting")
	}
	if cmd == nil {
		t.Fatal("second Ctrl+C should return quit command")
	}
}

func TestConfirmedCtrlCCancelsProviderAimlapiOnboarding(t *testing.T) {
	cancelled := false
	m := newModel(context.Background(), Options{ProviderName: "tokenrouter"})
	m.providerWizard = &providerWizardState{
		step: providerWizardStepAimlapi,
		aimlapi: &aimlapiOnboardState{
			topupCancel: func() { cancelled = true },
		},
	}

	updated, _ := m.Update(testKeyCtrl('c'))
	next := updated.(model)
	if cancelled {
		t.Fatal("first Ctrl+C should only arm exit confirmation")
	}

	updated, cmd := next.Update(testKeyCtrl('c'))
	next = updated.(model)
	if !cancelled {
		t.Fatal("confirmed Ctrl+C did not cancel AIMLAPI onboarding")
	}
	if next.providerWizard.aimlapi != nil {
		t.Fatal("confirmed Ctrl+C retained AIMLAPI onboarding state")
	}
	if cmd == nil {
		t.Fatal("confirmed Ctrl+C should return quit command")
	}
}

func TestCtrlCExitConfirmationDisarmsOnInterveningKey(t *testing.T) {
	m := newModel(context.Background(), Options{ProviderName: "tokenrouter"})

	updated, _ := m.Update(testKeyCtrl('c'))
	next := updated.(model)
	if !next.exitConfirmActive {
		t.Fatal("first Ctrl+C should arm exit confirmation")
	}
	seq := next.exitConfirmSeq

	updated, cmd := next.Update(testKey(tea.KeyLeft))
	next = updated.(model)
	if cmd != nil {
		t.Fatal("intervening navigation key should not return a command")
	}
	if next.exitConfirmActive {
		t.Fatal("intervening non-Ctrl+C key should disarm exit confirmation")
	}
	if next.exitConfirmSeq == seq {
		t.Fatal("disarming should advance the sequence so stale expiry ticks are ignored")
	}

	updated, cmd = next.Update(testKeyCtrl('c'))
	next = updated.(model)
	if next.exiting {
		t.Fatal("Ctrl+C after an intervening key should re-arm, not exit")
	}
	if !next.exitConfirmActive {
		t.Fatal("Ctrl+C after an intervening key should arm a fresh confirmation")
	}
	if cmd == nil {
		t.Fatal("fresh Ctrl+C confirmation should schedule expiry")
	}
}

func TestCtrlCClearsComposerBeforeExitConfirmation(t *testing.T) {
	m := newModel(context.Background(), Options{ProviderName: "tokenrouter"})
	m.input.SetValue("draft prompt")
	m.recomputeSuggestions()

	updated, cmd := m.Update(testKeyCtrl('c'))
	next := updated.(model)

	if cmd != nil {
		t.Fatal("Ctrl+C with a draft should clear the composer without scheduling exit confirmation")
	}
	if next.composerValue() != "" {
		t.Fatalf("composer value = %q, want empty after Ctrl+C", next.composerValue())
	}
	if next.exitConfirmActive {
		t.Fatal("Ctrl+C with a draft should not arm exit confirmation")
	}
	if next.exiting {
		t.Fatal("Ctrl+C with a draft should not mark model exiting")
	}
	status := plainRender(t, next.statusLine(80))
	// No exit confirmation armed, so the normal run-state chip shows (the status
	// line carries the permission mode now, not the provider).
	if strings.Contains(status, ctrlCExitConfirmText) || !strings.Contains(status, "auto-approve") {
		t.Fatalf("status line = %q, want the run-state chip with no exit confirmation", status)
	}

	updated, cmd = next.Update(testKeyCtrl('c'))
	next = updated.(model)
	if !next.exitConfirmActive {
		t.Fatal("Ctrl+C on an empty composer should arm exit confirmation")
	}
	if cmd == nil {
		t.Fatal("Ctrl+C on an empty composer should schedule confirmation expiry")
	}
}

func TestCtrlCExitConfirmationExpires(t *testing.T) {
	m := newModel(context.Background(), Options{ProviderName: "tokenrouter"})

	updated, _ := m.Update(testKeyCtrl('c'))
	next := updated.(model)
	seq := next.exitConfirmSeq

	updated, _ = next.Update(exitConfirmExpiredMsg{seq: seq - 1})
	next = updated.(model)
	if !next.exitConfirmActive {
		t.Fatal("stale expiry should not clear active exit confirmation")
	}

	updated, _ = next.Update(exitConfirmExpiredMsg{seq: seq})
	next = updated.(model)
	if next.exitConfirmActive {
		t.Fatal("matching expiry should clear exit confirmation")
	}
	status := plainRender(t, next.statusLine(80))
	// After expiry the warning clears and the normal run-state chip is restored
	// (the status line now shows the permission mode, not the provider).
	if strings.Contains(status, ctrlCExitConfirmText) || !strings.Contains(status, "auto-approve") {
		t.Fatalf("status line after expiry = %q, want the run-state chip restored", status)
	}
}

func TestSystemNotesRenderPlainLinesNotBoxes(t *testing.T) {
	cancel := plainRender(t, renderSystemNote("Run cancelled.", 80))
	if strings.ContainsAny(cancel, "│╭╮╰╯") {
		t.Fatalf("cancellation should be a plain line, not a box: %q", cancel)
	}
	if !strings.Contains(cancel, "Run cancelled.") {
		t.Fatalf("cancellation text missing: %q", cancel)
	}
	// Every other system notice is ALSO a boxless plain line now.
	for _, note := range []string{"Mouse interaction re-enabled.", "Mode set to ask."} {
		got := plainRender(t, renderSystemNote(note, 80))
		if strings.ContainsAny(got, "│╭╮╰╯") {
			t.Fatalf("system notice should be a plain line, not a box: %q", got)
		}
		if !strings.Contains(got, note) {
			t.Fatalf("notice text missing: %q", got)
		}
	}
}

func TestSpecialistCardLinesAreSelectableForCopy(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4"})
	m.width = 120
	row := transcriptRow{kind: rowSpecialist, specialistInfo: &specialistInfo{
		name: "explorer", description: "audit the code", status: specialistCompleted, childSessionID: "sess-x",
	}}
	rendered, selectable := m.renderSelectableSpecialistRow(0, row, 100, buildRowContext(nil), 0)
	if rendered == "" || len(selectable) == 0 {
		t.Fatal("expected a rendered specialist card with selectable lines")
	}
	hasText := false
	for _, sl := range selectable {
		if !sl.specialistCard {
			t.Fatalf("card line lost its specialistCard click flag: %+v", sl)
		}
		if sl.text != "" {
			hasText = true
		}
	}
	if !hasText {
		t.Fatal("specialist card lines must carry text so a dragged selection copies them")
	}
}

func TestLiveReasoningExpandIsCappedAndAligned(t *testing.T) {
	var m model
	m.height = 20 // liveReasoningBodyCap = max(6, 10) = 10
	width := 80
	text := strings.Repeat("thinking about the problem in detail here\n", 30)
	cap := m.liveReasoningBodyCap()

	lines, selectable := m.renderSelectableReasoningBlock(-1, text, true, true, 0, width, 0)
	// The selectable spans MUST stay 1:1 with the displayed lines, or the gutter
	// highlighter lands on the wrong rows.
	if len(lines) != len(selectable) {
		t.Fatalf("display lines (%d) and selectable spans (%d) must align", len(lines), len(selectable))
	}
	// Capped to ~half-screen: header + marker + cap body, not all ~30 lines.
	if len(lines) > cap+3 {
		t.Fatalf("live reasoning not capped: %d lines for cap %d", len(lines), cap)
	}
	if !strings.Contains(plainRender(t, strings.Join(lines, "\n")), "earlier lines") {
		t.Fatalf("expected the '… earlier lines' marker:\n%s", strings.Join(lines, "\n"))
	}
	// The display path must produce the same line count as the selectable path.
	display := renderReasoningBlock(strings.TrimSpace(text), true, width, true, 0, cap)
	if got := len(strings.Split(display, "\n")); got != len(lines) {
		t.Fatalf("display path (%d lines) and selectable path (%d lines) must match", got, len(lines))
	}
}

func TestSelectionHighlightUsesGutterShiftedCoordinate(t *testing.T) {
	// Select screen columns 10..15 on body line 0. With a 4-cell reading gutter the
	// rendered line is "    Hello world" and columns 10-14 are "world", so the
	// highlight must land on "world" — proving it's computed in the SAME shifted
	// coordinate the mouse uses. The old bug painted it on the unshifted line, so it
	// landed gutter cells off.
	var m model
	m.transcriptSelection = transcriptSelectionState{
		active: true,
		anchor: transcriptSelectionPoint{bodyY: 0, x: 10},
		cursor: transcriptSelectionPoint{bodyY: 0, x: 15},
	}
	selectable := []transcriptSelectableLine{{bodyY: 0, rowIndex: 0, text: "Hello world", textStart: 0}}
	item := m.finalizeTranscriptBodyRow("Hello world", selectable, 4, 0)
	styled := strings.Join(item.lines, "\n")
	if !strings.Contains(plainRender(t, styled), "    Hello world") {
		t.Fatalf("expected a 4-cell gutter-padded line, got %q", styled)
	}
	if !strings.Contains(styled, zeroTheme.selection.Render("world")) {
		t.Fatalf("expected 'world' highlighted at the gutter-shifted position, got %q", styled)
	}
}

// TestTranscriptSelectionPaintsHighlightOnceNotTwice guards the "two highlights"
// bug: assistant/user/reasoning rows used to self-paint the selection and
// finalizeTranscriptBodyRow re-painted it, so a single selection lit up twice.
// The highlight must land exactly once.
func TestTranscriptSelectionPaintsHighlightOnceNotTwice(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4"})
	m.width, m.height = 160, 30
	m.altScreen = true
	m.transcript = append(m.transcript, transcriptRow{
		kind: rowAssistant, text: "alpha beta gamma delta epsilon zeta", final: true,
	})
	width := m.chatColumnWidth()

	findRow := func(mm model) (transcriptBodyItem, bool) {
		for _, it := range mm.transcriptBodyItems(width, "", false) {
			if it.kind == transcriptBodyItemRow && it.rowIndex >= 0 {
				return it, true
			}
		}
		return transcriptBodyItem{}, false
	}

	row, ok := findRow(m)
	if !ok {
		t.Fatal("no assistant row item")
	}
	var line transcriptSelectableLine
	for _, sl := range row.render(0).selectable {
		if sl.text != "" {
			line = sl
			break
		}
	}
	if line.text == "" || lipgloss.Width(line.text) < 5 {
		t.Fatalf("expected a selectable text line of width >= 5, got %+v", line)
	}

	// Select the first 5 columns of that line in the same coordinate system mouse
	// selection uses.
	m.transcriptSelection = transcriptSelectionState{
		active: true,
		anchor: transcriptSelectionPoint{bodyY: line.bodyY, x: line.textStart},
		cursor: transcriptSelectionPoint{bodyY: line.bodyY, x: line.textStart + 5},
	}
	row2, ok := findRow(m)
	if !ok {
		t.Fatal("no assistant row item with selection active")
	}
	out := strings.Join(row2.render(0).lines, "\n")

	sentinel := zeroTheme.selection.Render("\x00")
	open := sentinel[:strings.IndexByte(sentinel, 0)]
	if open == "" {
		t.Fatal("selection style emitted no opening sequence to count")
	}
	if got := strings.Count(out, open); got != 1 {
		t.Fatalf("selection highlight painted %d times, want exactly 1 (the two-highlights bug):\n%q", got, out)
	}
}

func TestReasoningAfterToolCardGetsBlankSeparator(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4"})
	m.width, m.height = 120, 40
	m.altScreen = true
	m.transcript = append(m.transcript,
		transcriptRow{kind: rowUser, text: "do it"},
		transcriptRow{kind: rowToolResult, id: "t1", tool: "list_directory", status: tools.StatusOK, detail: "a\nb"},
		transcriptRow{kind: rowReasoning, text: "Considering the next step in detail before acting"},
		transcriptRow{kind: rowToolResult, id: "t2", tool: "read_file", status: tools.StatusOK, detail: "x\ny"},
	)
	items := m.transcriptBodyItems(m.chatColumnWidth(), "", false)
	reasoningIdx := -1
	for i := range items {
		if items[i].rowIndex >= 0 && items[i].rowIndex < len(m.transcript) &&
			m.transcript[items[i].rowIndex].kind == rowReasoning {
			reasoningIdx = i
		}
	}
	if reasoningIdx <= 0 {
		t.Fatal("reasoning item not found in the body")
	}
	if items[reasoningIdx-1].kind != transcriptBodyItemSeparator {
		t.Fatalf("expected a blank separator before the reasoning group, got kind %v", items[reasoningIdx-1].kind)
	}
}

func TestAssistantNarrationBeforeToolCardGetsBlankSeparator(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4"})
	m.width, m.height = 120, 40
	m.altScreen = true
	m.transcript = append(m.transcript,
		transcriptRow{kind: rowUser, text: "run it"},
		transcriptRow{kind: rowAssistant, text: "I'll inspect the existing file, then run it."},
		transcriptRow{kind: rowToolResult, id: "t1", tool: "read_file", status: tools.StatusOK, detail: "File: time_test.py\n\n1→print('x')"},
	)
	items := m.transcriptBodyItems(m.chatColumnWidth(), "", false)
	toolIdx := -1
	for i := range items {
		if items[i].rowIndex >= 0 && items[i].rowIndex < len(m.transcript) &&
			m.transcript[items[i].rowIndex].kind == rowToolResult {
			toolIdx = i
		}
	}
	if toolIdx <= 0 {
		t.Fatal("tool item not found in the body")
	}
	if items[toolIdx-1].kind != transcriptBodyItemSeparator {
		t.Fatalf("expected a blank separator between assistant narration and first tool card, got kind %v", items[toolIdx-1].kind)
	}
}

func TestUserPromptBeforeToolCardGetsBlankSeparator(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4"})
	m.width, m.height = 120, 40
	m.altScreen = true
	m.transcript = append(m.transcript,
		transcriptRow{kind: rowUser, text: "create the landing page"},
		transcriptRow{kind: rowToolResult, id: "t1", tool: "write_file", status: tools.StatusOK, detail: "--- /dev/null\n+++ b/index.html\n@@ -0,0 +1,1 @@\n+<!DOCTYPE html>"},
	)
	items := m.transcriptBodyItems(m.chatColumnWidth(), "", false)
	toolIdx := -1
	for i := range items {
		if items[i].rowIndex >= 0 && items[i].rowIndex < len(m.transcript) &&
			m.transcript[items[i].rowIndex].kind == rowToolResult {
			toolIdx = i
		}
	}
	if toolIdx <= 0 {
		t.Fatal("tool item not found in the body")
	}
	if items[toolIdx-1].kind != transcriptBodyItemSeparator {
		t.Fatalf("expected a blank separator between user prompt and first tool card, got kind %v", items[toolIdx-1].kind)
	}
}

func TestAssistantAfterToolCardGetsRuleSeparator(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4"})
	m.width, m.height = 120, 40
	m.altScreen = true
	m.transcript = append(m.transcript,
		transcriptRow{kind: rowUser, text: "run it"},
		transcriptRow{kind: rowAssistant, text: "I'll run it first."},
		transcriptRow{kind: rowToolResult, id: "t1", tool: "bash", status: tools.StatusOK, detail: "stdout:\nok\nexit_code: 0"},
		transcriptRow{kind: rowAssistant, text: "Done.", final: true},
	)
	items := m.transcriptBodyItems(m.chatColumnWidth(), "", false)
	finalIdx := -1
	for i := range items {
		if items[i].rowIndex >= 0 && items[i].rowIndex < len(m.transcript) &&
			m.transcript[items[i].rowIndex].kind == rowAssistant &&
			m.transcript[items[i].rowIndex].final {
			finalIdx = i
		}
	}
	if finalIdx <= 0 {
		t.Fatal("final assistant item not found in the body")
	}
	if items[finalIdx-1].kind != transcriptBodyItemRule {
		t.Fatalf("expected a rule separator before assistant prose after tool output, got kind %v", items[finalIdx-1].kind)
	}

	body, _ := m.transcriptBody(m.chatColumnWidth(), "")
	got := plainRender(t, body)
	if !strings.Contains(got, "──") || !strings.Contains(got, "Done.") {
		t.Fatalf("expected visible rule before final answer, got:\n%s", got)
	}
}

func TestStreamingAssistantAfterToolCardGetsRuleSeparator(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4"})
	m.width, m.height = 120, 40
	m.altScreen = true
	m.pending = true
	m.streamingText = []byte("Done.")
	m.transcript = append(m.transcript,
		transcriptRow{kind: rowUser, text: "run it"},
		transcriptRow{kind: rowAssistant, text: "I'll run it first."},
		transcriptRow{kind: rowToolResult, id: "t1", tool: "bash", status: tools.StatusOK, detail: "stdout:\nok\nexit_code: 0"},
	)
	items := m.transcriptBodyItems(m.chatColumnWidth(), "", false)
	pendingIdx := -1
	for i := range items {
		if items[i].kind == transcriptBodyItemPendingInterim {
			pendingIdx = i
		}
	}
	if pendingIdx <= 0 {
		t.Fatal("pending interim item not found in the body")
	}
	if items[pendingIdx-1].kind != transcriptBodyItemRule {
		t.Fatalf("expected a live rule separator before streaming assistant text after tool output, got kind %v", items[pendingIdx-1].kind)
	}
}

func TestTranscriptReadingColumnHelpers(t *testing.T) {
	// Wide terminal: no body gutter, full content width.
	if g := transcriptGutter(160); g != 0 {
		t.Fatalf("wide gutter = %d, want 0", g)
	}
	if cw := transcriptContentWidth(160); cw != 160 {
		t.Fatalf("wide contentWidth = %d, want full 160", cw)
	}
	// Very wide terminal: still no gutter, so code/tool blocks use the width.
	if g := transcriptGutter(400); g != 0 {
		t.Fatalf("very wide gutter = %d, want 0", g)
	}
	// Tiny terminal: full width so prose never collapses.
	if g := transcriptGutter(40); g != 0 {
		t.Fatalf("tiny gutter = %d, want 0", g)
	}
	if cw := transcriptContentWidth(40); cw != 40 {
		t.Fatalf("tiny contentWidth = %d, want full 40", cw)
	}
}

func TestTranscriptBodyRowsUseFullWidthAndAlignSelection(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4"})
	m.width, m.height = 160, 30
	m.altScreen = true
	m.transcript = append(m.transcript, transcriptRow{
		kind: rowAssistant, text: strings.Repeat("word ", 60), final: true,
	})

	width := m.chatColumnWidth()
	gutter := transcriptGutter(width)
	if gutter != 0 {
		t.Fatalf("expected no body gutter at width %d, got %d", width, gutter)
	}

	items := m.transcriptBodyItems(width, "", false)
	var row *transcriptBodyItem
	for i := range items {
		if items[i].kind == transcriptBodyItemRow && items[i].rowIndex >= 0 {
			row = &items[i]
		}
	}
	if row == nil {
		t.Fatal("no assistant row item found")
	}
	rendered := row.render(0)

	maxLine := transcriptContentWidth(width) + gutter // content plus any left indent
	sawContent := false
	for _, line := range rendered.lines {
		if w := lipgloss.Width(line); w > maxLine {
			t.Fatalf("body line width %d exceeds chat column %d: %q", w, maxLine, line)
		}
		if strings.TrimSpace(line) != "" {
			sawContent = true
		}
	}
	if !sawContent {
		t.Fatal("expected at least one content line")
	}
	// Selection alignment: text-carrying selectable lines should never start before
	// the rendered text coordinate.
	for _, sl := range rendered.selectable {
		if sl.text != "" && sl.textStart < gutter {
			t.Fatalf("selectable textStart %d < gutter %d — selection would misalign", sl.textStart, gutter)
		}
	}
}

func TestMarkdownAddsBlankBeforeHeadingAndParagraph(t *testing.T) {
	lines := renderAssistantMarkdownText("First paragraph.\n## Heading\nSecond body.", 80, 80, false)
	headingIdx := -1
	for i, l := range lines {
		// Headings render accent+bold+underline (per-grapheme ANSI), so strip styling
		// before matching the text.
		if strings.Contains(plainRender(t, l), "Heading") {
			headingIdx = i
		}
	}
	if headingIdx <= 0 || strings.TrimSpace(lines[headingIdx-1]) != "" {
		t.Fatalf("expected a blank line before the heading, got %#v", lines)
	}
}

func TestComposerIdleHintAndJumpCue(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4"})
	m.altScreen = true
	m.width, m.height = 100, 30
	m.transcript = append(m.transcript, transcriptRow{kind: rowAssistant, text: "hi", final: true})

	for _, test := range []struct {
		name  string
		width int
	}{
		{name: "medium", width: 80},
		{name: "full", width: 100},
	} {
		t.Run(test.name, func(t *testing.T) {
			idle := m
			idle.width = test.width

			// Idle, empty composer, managed mode -> the discoverability hint shows.
			hint := plainRender(t, idle.composerIdleHint())
			if !strings.Contains(hint, "shortcuts") {
				t.Fatalf("expected idle hint, got %q", hint)
			}
			if strings.Contains(hint, "sidebar") {
				t.Fatalf("empty sidebar should not be advertised, got %q", hint)
			}

			withDetails := idle
			withDetails.plan.steps = []planStep{{content: "inspect footer", status: "in_progress"}}
			if hint := plainRender(t, withDetails.composerIdleHint()); !strings.Contains(hint, "Ctrl+B details") {
				t.Fatalf("available run details should be advertised, got %q", hint)
			}
		})
	}
	// Hidden during a run.
	busy := m
	busy.pending = true
	if h := busy.composerIdleHint(); h != "" {
		t.Fatalf("hint should hide during a run, got %q", h)
	}
	// Hidden in inline mode.
	inline := m
	inline.altScreen = false
	if h := inline.composerIdleHint(); h != "" {
		t.Fatalf("hint should hide in inline mode, got %q", h)
	}
	// Jump cue only when scrolled up.
	if c := m.jumpToBottomHint(); c != "" {
		t.Fatalf("jump cue should be empty at the bottom, got %q", c)
	}
	scrolled := m
	scrolled.chatScrollOffset = 5
	if c := plainRender(t, scrolled.jumpToBottomHint()); !strings.Contains(c, "5 more") {
		t.Fatalf("expected jump cue with line count, got %q", c)
	}
	// The footer carrying the hint must never overflow its width.
	w := m.chatColumnWidth()
	for i, line := range strings.Split(plainRender(t, m.footerView(w)), "\n") {
		if got := lipgloss.Width(line); got > w {
			t.Fatalf("footer line %d width %d > %d: %q", i, got, w, line)
		}
	}
}

func assertContains(t *testing.T, text any, want string) {
	t.Helper()

	content := plainRender(t, text)
	if !strings.Contains(content, want) {
		t.Fatalf("expected %q to contain %q", content, want)
	}
}

func assertNotContains(t *testing.T, text any, unwanted string) {
	t.Helper()

	content := plainRender(t, text)
	if strings.Contains(content, unwanted) {
		t.Fatalf("expected %q not to contain %q", content, unwanted)
	}
}

func transcriptContains(rows []transcriptRow, want string) bool {
	for _, row := range rows {
		if strings.Contains(row.text, want) {
			return true
		}
	}
	return false
}

func transcriptText(rows []transcriptRow) string {
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		parts = append(parts, row.text)
		if row.detail != "" {
			parts = append(parts, row.detail)
		}
	}
	return strings.Join(parts, "\n")
}

func countTranscriptRows(rows []transcriptRow, kind rowKind) int {
	count := 0
	for _, row := range rows {
		if row.kind == kind {
			count++
		}
	}
	return count
}

func findTranscriptRow(rows []transcriptRow, kind rowKind) (transcriptRow, bool) {
	for _, row := range rows {
		if row.kind == kind {
			return row, true
		}
	}
	return transcriptRow{}, false
}

func transcriptHasMarkedModelEntry(rows []transcriptRow) bool {
	for _, row := range rows {
		for _, line := range strings.Split(row.text, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "* ") && strings.Contains(trimmed, " (") {
				return true
			}
		}
	}
	return false
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func testPromptPermissionEvent() agent.PermissionEvent {
	return agent.PermissionEvent{
		ToolCallID:     "call_1",
		ToolName:       "write_file",
		Action:         agent.PermissionActionPrompt,
		Permission:     "prompt",
		PermissionMode: agent.PermissionModeAsk,
		Autonomy:       "medium",
		SideEffect:     "write",
		Reason:         "Creates or overwrites files.",
		Risk:           sandbox.Risk{Level: sandbox.RiskHigh},
	}
}

func testPromptPermissionRequest() agent.PermissionRequest {
	event := testPromptPermissionEvent()
	return agent.PermissionRequest{
		ToolCallID:         event.ToolCallID,
		ToolName:           event.ToolName,
		Action:             event.Action,
		Permission:         event.Permission,
		PermissionMode:     event.PermissionMode,
		Autonomy:           event.Autonomy,
		SideEffect:         event.SideEffect,
		Reason:             event.Reason,
		Risk:               event.Risk,
		AvailableDecisions: testAllPermissionDecisions(),
	}
}

func testAllPermissionDecisions() []agent.PermissionDecisionAction {
	return []agent.PermissionDecisionAction{
		agent.PermissionDecisionAllow,
		agent.PermissionDecisionAllowForSession,
		agent.PermissionDecisionAlwaysAllow,
		agent.PermissionDecisionDeny,
	}
}

func testSessionStore(t *testing.T) *sessions.Store {
	t.Helper()

	now := time.Date(2026, 6, 5, 10, 0, 0, 0, time.UTC)
	return sessions.NewStore(sessions.StoreOptions{
		RootDir: t.TempDir(),
		Now: func() time.Time {
			now = now.Add(time.Minute)
			return now
		},
	})
}

func TestNextPermissionModeFoldsUnsafeToAsk(t *testing.T) {
	if got := nextPermissionMode(agent.PermissionModeAuto); got != agent.PermissionModeAsk {
		t.Fatalf("Auto -> %s, want Ask", got)
	}
	if got := nextPermissionMode(agent.PermissionModeAsk); got != agent.PermissionModeAuto {
		t.Fatalf("Ask -> %s, want Auto", got)
	}
	// Unsafe must fold to the STRICTER Ask, never Auto (toggling an Unsafe session
	// must not make it less strict).
	if got := nextPermissionMode(agent.PermissionModeUnsafe); got != agent.PermissionModeAsk {
		t.Fatalf("Unsafe -> %s, want Ask", got)
	}
}

func TestModelNotifierFocusAndCompletion(t *testing.T) {
	var buf bytes.Buffer
	m := model{notifier: notify.New(&buf, notify.Config{Mode: notify.ModeBell, FocusMode: notify.FocusUnfocused})}
	m.notifier.SetFocused(true)

	// Focused → completion under unfocused-mode is silent.
	m.notifier.Notify(notify.Completion, notify.DefaultMessage(notify.Completion))
	if buf.Len() != 0 {
		t.Fatalf("focused should be silent, got %q", buf.String())
	}

	// BlurMsg flips focus; completion now bells.
	updated, _ := m.Update(tea.BlurMsg{})
	m = updated.(model)
	m.notifier.Notify(notify.Completion, notify.DefaultMessage(notify.Completion))
	if buf.String() != "\x07" {
		t.Fatalf("unfocused should bell, got %q", buf.String())
	}

	// FocusMsg flips back.
	updated, _ = m.Update(tea.FocusMsg{})
	m = updated.(model)
	buf.Reset()
	m.notifier.Notify(notify.Completion, "x")
	if buf.Len() != 0 {
		t.Fatalf("refocused should be silent, got %q", buf.String())
	}
}

func TestComposerBlinkStaysSolidWhileTyping(t *testing.T) {
	base := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	now := base
	m := model{
		now:                   func() time.Time { return now },
		terminalFocused:       true,
		lastCharTime:          base,
		composerCursorVisible: true,
		composerBlinkSeq:      1,
		composerBlinkTicking:  true,
	}

	// Each iteration simulates a keystroke (refreshing lastCharTime) followed by
	// a blink tick within the typing-idle threshold: cursor must stay solid
	// rather than toggling off, however many ticks land.
	for i := 0; i < 3; i++ {
		now = now.Add(200 * time.Millisecond)
		m.lastCharTime = now
		updated, _ := m.Update(composerBlinkMsg{seq: m.composerBlinkSeq})
		m = updated.(model)
		if !m.composerCursorVisible {
			t.Fatalf("tick %d: expected cursor to stay visible while typing, got hidden", i)
		}
	}
}

func TestMainComposerDisablesEmbeddedCursorBlink(t *testing.T) {
	m := newModel(t.Context(), Options{})
	if m.input.Styles().Cursor.Blink {
		t.Fatal("embedded composer cursor blink must stay disabled")
	}
}

func TestComposerBlinkHiddenWhileUnfocused(t *testing.T) {
	base := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	m := model{
		now:                   func() time.Time { return base },
		lastCharTime:          base.Add(-time.Hour), // long idle, irrelevant while unfocused
		composerCursorVisible: true,
	}

	updated, _ := m.Update(tea.BlurMsg{})
	m = updated.(model)

	for i := 0; i < 3; i++ {
		updated, _ = m.Update(composerBlinkMsg{})
		m = updated.(model)
		if m.composerCursorVisible {
			t.Fatalf("tick %d: expected cursor to stay hidden while unfocused, got visible", i)
		}
	}
}

func TestComposerBlinkResumesAfterRefocusAndIdle(t *testing.T) {
	base := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	m := model{
		now:                   func() time.Time { return base },
		lastCharTime:          base.Add(-time.Hour), // stale: well past the idle threshold
		composerCursorVisible: true,
	}

	updated, _ := m.Update(tea.BlurMsg{})
	m = updated.(model)
	updated, _ = m.Update(tea.FocusMsg{})
	m = updated.(model)
	if !m.terminalFocused {
		t.Fatal("expected terminalFocused to be true after FocusMsg")
	}

	updated, _ = m.Update(composerBlinkMsg{seq: m.composerBlinkSeq})
	m = updated.(model)
	first := m.composerCursorVisible
	updated, _ = m.Update(composerBlinkMsg{seq: m.composerBlinkSeq})
	m = updated.(model)
	second := m.composerCursorVisible
	if first == second {
		t.Fatalf("expected blink to toggle once idle+focused, got %v then %v", first, second)
	}
}

func TestComposerBlinkRejectsTickFromBeforeRefocus(t *testing.T) {
	base := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	m := model{
		now:                   func() time.Time { return base },
		terminalFocused:       true,
		lastCharTime:          base.Add(-time.Hour),
		composerCursorVisible: true,
		composerBlinkSeq:      5,
		composerBlinkTicking:  true,
	}
	staleSeq := m.composerBlinkSeq

	updated, _ := m.Update(tea.BlurMsg{})
	m = updated.(model)
	updated, refocusCmd := m.Update(tea.FocusMsg{})
	m = updated.(model)
	if refocusCmd == nil || m.composerBlinkSeq == staleSeq {
		t.Fatal("refocus did not re-arm composer blinking with a new sequence")
	}
	visible := m.composerCursorVisible

	updated, staleCmd := m.Update(composerBlinkMsg{seq: staleSeq})
	m = updated.(model)
	if staleCmd != nil || m.composerCursorVisible != visible {
		t.Fatalf("stale tick changed composer state: cmd=%v visible=%v want=%v", staleCmd != nil, m.composerCursorVisible, visible)
	}
}

func TestComposerBlinkTogglesWhenIdleAndFocused(t *testing.T) {
	base := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	m := model{
		now:                   func() time.Time { return base },
		terminalFocused:       true,
		lastCharTime:          base.Add(-time.Hour),
		composerCursorVisible: true,
		composerBlinkSeq:      1,
		composerBlinkTicking:  true,
	}

	updated, _ := m.Update(composerBlinkMsg{seq: m.composerBlinkSeq})
	m = updated.(model)
	if m.composerCursorVisible {
		t.Fatal("expected cursor to toggle off on first idle+focused tick")
	}
	updated, _ = m.Update(composerBlinkMsg{seq: m.composerBlinkSeq})
	m = updated.(model)
	if !m.composerCursorVisible {
		t.Fatal("expected cursor to toggle back on on second idle+focused tick")
	}
}

func TestComposerBlinkSettlesVisibleWithoutRescheduling(t *testing.T) {
	m := model{
		now:                   func() time.Time { return time.Unix(100, 0) },
		terminalFocused:       true,
		lastCharTime:          time.Unix(0, 0),
		composerCursorVisible: true,
		composerBlinkSeq:      1,
		composerBlinkTicking:  true,
	}
	var cmd tea.Cmd
	for range composerBlinkIdleTicksBeforeSettle {
		var updated tea.Model
		updated, cmd = m.Update(composerBlinkMsg{seq: m.composerBlinkSeq})
		m = updated.(model)
	}
	if m.composerBlinkTicking || cmd != nil || !m.composerCursorVisible {
		t.Fatalf("settled cursor = ticking:%v cmd:%v visible:%v", m.composerBlinkTicking, cmd != nil, m.composerCursorVisible)
	}
}

func TestComposerInputRestartsSettledBlink(t *testing.T) {
	m := newModel(t.Context(), Options{})
	m.composerBlinkTicking = false
	m.composerCursorVisible = true
	updated, cmd := m.Update(tea.KeyPressMsg(tea.Key{Code: 'a', Text: "a"}))
	m = updated.(model)
	if !m.composerBlinkTicking || cmd == nil || !m.composerCursorVisible {
		t.Fatalf("restarted cursor = ticking:%v cmd:%v visible:%v", m.composerBlinkTicking, cmd != nil, m.composerCursorVisible)
	}
}

func TestComposerCursorShowsImmediatelyOnKeypress(t *testing.T) {
	// The blink phase may have just hidden the caret when the user starts
	// typing; the typed character must render with a caret immediately, not
	// after the next composerBlinkMsg tick evaluates the typing threshold.
	m := newModel(context.Background(), Options{})
	m.terminalFocused = true
	m.composerCursorVisible = false

	updated, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: 'a', Text: "a"}))
	m = updated.(model)
	if !m.composerCursorVisible {
		t.Fatal("expected caret visible immediately after a keypress, before any blink tick")
	}
}

func TestComposerCursorSyncsImmediatelyOnFocusChange(t *testing.T) {
	// Focus transitions must apply the caret contract synchronously: a blur
	// with no blink tick yet must not leave a visible caret in an unfocused
	// terminal, and a refocus must not leave the caret hidden for a tick.
	m := model{composerCursorVisible: true, terminalFocused: true}

	updated, _ := m.Update(tea.BlurMsg{})
	m = updated.(model)
	if m.composerCursorVisible {
		t.Fatal("expected caret hidden immediately after BlurMsg, before any blink tick")
	}

	updated, _ = m.Update(tea.FocusMsg{})
	m = updated.(model)
	if !m.composerCursorVisible {
		t.Fatal("expected caret visible immediately after FocusMsg, before any blink tick")
	}
}

func TestComposerCursorShowsImmediatelyOnPaste(t *testing.T) {
	// A paste is the same immediate-input transition as a keypress: if the
	// blink phase had just hidden the caret, the freshly pasted composer must
	// render with a solid caret right away, and the typing timestamp must be
	// refreshed so the next blink tick holds solid instead of toggling off a
	// stale idle state.
	m := newModel(context.Background(), Options{})
	m.terminalFocused = true
	m.composerCursorVisible = false
	m.lastCharTime = time.Now().Add(-time.Hour) // stale: well past the idle threshold

	updated, _ := m.Update(tea.PasteMsg{Content: "pasted text"})
	m = updated.(model)
	if !m.composerCursorVisible {
		t.Fatal("expected caret visible immediately after a paste, before any blink tick")
	}
	if time.Since(m.lastCharTime) > time.Minute {
		t.Fatalf("expected the paste to refresh lastCharTime, still %v old", time.Since(m.lastCharTime))
	}
}

func TestCommandArgumentHintFollowsCursorVisibility(t *testing.T) {
	// The argument-hint composer line is an alternate render path that used to
	// paint its caret cell unconditionally, ignoring focus and blink state.
	input := textinput.New()
	input.SetValue("/rewind ")

	visible := commandArgumentHintComposerLine(input, "hint", true)
	hidden := commandArgumentHintComposerLine(input, "hint", false)
	if visible == hidden {
		t.Fatal("expected the rendered hint line to differ between visible and hidden caret states")
	}
	if want := composerCursor(zeroTheme.faint.Render("h")); !strings.Contains(visible, want) {
		t.Fatalf("expected visible-caret render to contain the styled cursor cell, got %q", visible)
	}
	if got := hidden; strings.Contains(got, composerCursor(zeroTheme.faint.Render("h"))) {
		t.Fatalf("expected hidden-caret render to drop the styled cursor cell, got %q", got)
	}
}

func TestScrimViewportLine(t *testing.T) {
	// Blank lines are left untouched (no scrim).
	if got := scrimViewportLine("   ", 10); got != "   " {
		t.Fatalf("blank line should be untouched, got %q", got)
	}
	// Non-blank plain lines keep their text and become faint, so the backdrop stays
	// readable behind the overlay without competing with its focus surface.
	got := scrimViewportLine("transcript content", 40)
	if ansi.Strip(got) != "transcript content" {
		t.Fatalf("scrim must preserve text, got %q", ansi.Strip(got))
	}
	// Styled transcript content must retain its semantic foreground and background
	// colors. The scrim adds faintness around resets rather than flattening a diff
	// or syntax-highlighted line to a single grey style.
	styled := "\x1b[38;2;235;80;110mremoved\x1b[0m \x1b[48;2;35;80;58madded\x1b[0m"
	scrimmed := scrimViewportLine(styled, 40)
	if ansi.Strip(scrimmed) != "removed added" {
		t.Fatalf("scrim must preserve styled line's text, got %q", ansi.Strip(scrimmed))
	}
	for _, sequence := range []string{"\x1b[38;2;235;80;110m", "\x1b[48;2;35;80;58m"} {
		if !strings.Contains(scrimmed, sequence) {
			t.Fatalf("scrim must preserve semantic ANSI sequence %q, got %q", sequence, scrimmed)
		}
	}
	if !strings.Contains(scrimmed, "\x1b[2m") {
		t.Fatalf("scrim must apply faint styling around semantic colors, got %q", scrimmed)
	}
}

func TestOverlayViewportLinesCompositesAndPreservesBackdropText(t *testing.T) {
	width := 40
	lines := make([]string, 9)
	for i := range lines {
		lines[i] = "backdrop transcript row"
	}
	// Narrow, indented overlay so the composited rows keep backdrop in the margins.
	overlay := "          ╭────╮\n          │ pn │\n          ╰────╯"
	out := overlayViewportLines(append([]string{}, lines...), overlay, width)
	joined := ansi.Strip(strings.Join(out, "\n"))
	if !strings.Contains(joined, "pn") {
		t.Fatalf("overlay panel should be composited, got:\n%s", joined)
	}
	// A non-overlaid row keeps its (dimmed) backdrop text.
	if !strings.Contains(ansi.Strip(out[0]), "backdrop transcript row") {
		t.Fatalf("non-overlaid backdrop should survive the scrim, got %q", ansi.Strip(out[0]))
	}
	// The compositing contract: the row carrying the panel must ALSO keep backdrop
	// text outside the panel (left/right margins), not blank it out.
	var panelRow string
	for _, line := range out {
		if strings.Contains(ansi.Strip(line), "pn") {
			panelRow = ansi.Strip(line)
			break
		}
	}
	if panelRow == "" {
		t.Fatal("expected a row containing the panel")
	}
	if !strings.Contains(panelRow, "backdrop") {
		t.Fatalf("overlaid row should keep backdrop margin text alongside the panel, got %q", panelRow)
	}
}

// burstTestModel creates a model with fake provider, advancing clock (60ms/tick),
// and empty composer for burst/paste testing. termuxVersion controls the env var.
func burstTestModel(t *testing.T, termuxVersion string) model {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("TERMUX_VERSION", termuxVersion)
	provider := &fakeProvider{events: []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventText, Content: "ok"},
		{Type: zeroruntime.StreamEventDone},
	}}
	m := newModel(context.Background(), Options{
		Cwd:          t.TempDir(),
		ProviderName: "tokenrouter",
		ModelName:    "MiniMax-M3",
		Provider:     provider,
		Registry:     tools.NewRegistry(),
	})
	base := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	tick := 0
	m.now = func() time.Time {
		tick++
		return base.Add(time.Duration(tick) * 60 * time.Millisecond)
	}
	m.input.SetValue("")
	m.width = 100
	m.height = 30
	return m
}

// typeKeys simulates rapid keypresses into the model, returning the final state.
func typeKeys(m model, keys string) model {
	for _, ch := range keys {
		updated, _ := m.Update(testKeyText(string(ch)))
		m = updated.(model)
	}
	return m
}

// TestTermuxBurstInsertsNewline: under Termux, 3+ rapid chars + Enter inserts newline.
func TestTermuxBurstInsertsNewline(t *testing.T) {
	m := burstTestModel(t, "v0.118.0")
	m = typeKeys(m, "abc")
	updated, _ := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if m.pending {
		t.Fatal("burst should insert newline, not submit")
	}
	if !strings.Contains(m.composerValue(), "\n") {
		t.Fatalf("burst should insert newline into composer, got %q", m.composerValue())
	}
}

// TestTermuxFastTypingSubmits: under Termux, 2 fast chars + Enter still submits.
func TestTermuxFastTypingSubmits(t *testing.T) {
	m := burstTestModel(t, "v0.118.0")
	m = typeKeys(m, "ab")
	updated, cmd := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if !m.pending {
		t.Fatal("fast typing should be pending after submit")
	}
	if cmd == nil {
		t.Fatal("fast typing should submit, got nil cmd")
	}
}

// TestDesktopBurstNotAffected: on desktop (no TERMUX_VERSION), 3 fast chars + Enter submits.
func TestDesktopBurstNotAffected(t *testing.T) {
	m := burstTestModel(t, "")
	m = typeKeys(m, "abc")
	updated, cmd := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if !m.pending {
		t.Fatal("desktop burst should be pending after submit")
	}
	if cmd == nil {
		t.Fatal("desktop burst should submit, got nil cmd")
	}
}

// TestBurstResetAfterSubmit: after submit, burstCount is reset so normal
// typing (2 chars + Enter) still submits without false newline insertion.
func TestBurstResetAfterSubmit(t *testing.T) {
	m := burstTestModel(t, "v0.118.0")
	// Type and submit normally
	m = typeKeys(m, "hi")
	updated, cmd := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if cmd == nil {
		t.Fatal("first submit should start a run")
	}
	// Process the agent response so the model is no longer pending
	resp, _ := m.Update(execCmd(cmd))
	m = resp.(model)
	if m.pending {
		t.Fatal("model should not be pending after agent response")
	}

	// Reset lastKeyTime so the burst tracker sees a clean gap before the
	// second round of typing — avoids coupling to fake-clock progression.
	m.lastKeyTime = time.Time{}

	// After reset, 2 fast chars + Enter should still submit (burstCount < 3)
	m = typeKeys(m, "ok")
	updated, cmd = m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if !m.pending {
		t.Fatal("after burst reset, fast typing should submit")
	}
	if cmd == nil {
		t.Fatal("after burst reset, got nil cmd")
	}
}

// TestMultilineBurstSubmits: with a \n already in the composer, a 2-char
// burst + Enter on desktop should still submit (burstCount < 3), not
// insert another newline.
func TestMultilineBurstSubmits(t *testing.T) {
	m := burstTestModel(t, "")
	// Seed the composer with multiline text via applyComposerKey
	m.composerActive = true
	m.composer.text = "hello\nwor"
	m.composer.cursor = len([]rune(m.composer.text))
	m.input.SetValue(m.composer.text)

	// Type "ld" fast (2 chars) then Enter
	m = typeKeys(m, "ld")
	updated, cmd := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if !m.pending {
		t.Fatal("multiline burst should submit, not insert newline")
	}
	if cmd == nil {
		t.Fatal("multiline burst got nil cmd")
	}
}
