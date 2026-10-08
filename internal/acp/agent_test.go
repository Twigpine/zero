package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/providermodeldiscovery"
	"github.com/Gitlawb/zero/internal/sandbox"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// fakeProvider streams a canned assistant message and ends the turn — enough to
// drive the real agent.Run loop without a live model.
type fakeProvider struct{ text string }

func (f fakeProvider) StreamCompletion(_ context.Context, _ zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	ch := make(chan zeroruntime.StreamEvent, 4)
	go func() {
		defer close(ch)
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventText, Content: f.text}
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}
	}()
	return ch, nil
}

type acpDeferredPromptTool struct{ name string }

func (t acpDeferredPromptTool) Name() string           { return t.name }
func (acpDeferredPromptTool) Description() string      { return "deferred MCP test tool" }
func (acpDeferredPromptTool) Parameters() tools.Schema { return tools.Schema{Type: "object"} }
func (acpDeferredPromptTool) Deferred() bool           { return true }
func (acpDeferredPromptTool) Run(context.Context, map[string]any) tools.Result {
	return tools.Result{Status: tools.StatusOK, Output: "ok"}
}
func (acpDeferredPromptTool) Safety() tools.Safety {
	return tools.Safety{
		SideEffect: tools.SideEffectNetwork, Permission: tools.PermissionPrompt,
		AdvertiseInAuto: true,
	}
}

type captureToolsProvider struct {
	requests chan zeroruntime.CompletionRequest
}

func (p captureToolsProvider) StreamCompletion(_ context.Context, request zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	p.requests <- request
	ch := make(chan zeroruntime.StreamEvent, 2)
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventText, Content: "done"}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}
	close(ch)
	return ch, nil
}

func testDeps(t *testing.T) Deps {
	t.Helper()
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	return Deps{
		ResolveConfig: func(_ string, o config.Overrides) (config.ResolvedConfig, error) {
			model := "fake-model"
			if o.Provider.Model != "" {
				model = o.Provider.Model
			}
			return config.ResolvedConfig{
				Provider: config.ProviderProfile{Name: "fake", Model: model},
				MaxTurns: 4,
			}, nil
		},
		NewProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return fakeProvider{text: "Hello from ZERO"}, nil
		},
		RunAgent: agent.Run,
		BuildWorkspace: func(context.Context, string, config.ResolvedConfig, agent.PermissionMode) (*Workspace, error) {
			r := tools.NewRegistry()
			r.Register(tools.NewUpdatePlanTool())
			return &Workspace{Registry: r}, nil
		},
		ResolveWorkspaceRoot: func(cwd string) (string, error) { return cwd, nil },
		Store:                store,
		AgentInfo:            Implementation{Name: "zero", Version: "test"},
	}
}

// clientHarness wires a client Conn to an Agent over in-memory pipes and collects
// session/update text chunks.
type clientHarness struct {
	agent         *Agent
	client        *Conn
	updates       chan string
	notifications chan ContentChunk
	// tools carries replayed and live tool-call notifications. They are a
	// different wire shape from a message chunk, so decoding them into
	// ContentChunk would silently blank every field the assertions need.
	tools chan ToolCallUpdate
	stop  func()
}

func newHarness(t *testing.T, deps Deps) *clientHarness {
	t.Helper()
	ar, bw := io.Pipe() // agent -> client
	br, aw := io.Pipe() // client -> agent
	agentConn := NewConn(ar, aw)
	client := NewConn(br, bw)
	a := NewAgent(agentConn, deps)

	h := &clientHarness{agent: a, client: client, updates: make(chan string, 128), notifications: make(chan ContentChunk, 128), tools: make(chan ToolCallUpdate, 128)}
	client.HandleNotify(MethodSessionUpdate, func(_ context.Context, params json.RawMessage) {
		// Decode the discriminator ALONE first. ContentChunk and ToolCallUpdate
		// disagree on the shape of "content" (a block vs an array of them), so
		// decoding straight into ContentChunk makes every tool_call_update fail
		// to unmarshal and vanish — which reads exactly like a notification that
		// was never sent.
		var kind struct {
			Update struct {
				SessionUpdate string `json:"sessionUpdate"`
			} `json:"update"`
		}
		if json.Unmarshal(params, &kind) != nil {
			return
		}
		if kind.Update.SessionUpdate == UpdateToolCall || kind.Update.SessionUpdate == UpdateToolCallUpdate {
			var toolProbe struct {
				Update ToolCallUpdate `json:"update"`
			}
			if json.Unmarshal(params, &toolProbe) == nil {
				h.tools <- toolProbe.Update
			}
			return
		}
		var probe struct {
			Update ContentChunk `json:"update"`
		}
		if json.Unmarshal(params, &probe) != nil {
			return
		}
		if probe.Update.SessionUpdate == UpdateAgentMessageChunk || probe.Update.SessionUpdate == UpdateUserMessageChunk {
			h.notifications <- probe.Update
		}
		if probe.Update.SessionUpdate == UpdateAgentMessageChunk {
			h.updates <- probe.Update.Content.Text
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = a.Serve(ctx) }()
	go func() { _ = client.Serve(ctx) }()
	h.stop = func() {
		cancel()
		_ = aw.Close()
		_ = bw.Close()
	}
	return h
}

func TestACPEndToEndPrompt(t *testing.T) {
	h := newHarness(t, testDeps(t))
	defer h.stop()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// initialize
	var initRes InitializeResult
	if err := h.client.Call(ctx, MethodInitialize, InitializeParams{ProtocolVersion: ProtocolVersion}, &initRes); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if initRes.ProtocolVersion != ProtocolVersion {
		t.Fatalf("protocol version = %d", initRes.ProtocolVersion)
	}
	if !initRes.AgentCapabilities.LoadSession || !initRes.AgentCapabilities.PromptCapabilities.Image ||
		initRes.AgentCapabilities.SessionCapabilities == nil || initRes.AgentCapabilities.SessionCapabilities.List == nil || initRes.AgentCapabilities.SessionCapabilities.Resume == nil {
		t.Fatalf("unexpected capabilities: %+v", initRes.AgentCapabilities)
	}

	// session/new
	var newRes NewSessionResult
	if err := h.client.Call(ctx, MethodSessionNew, NewSessionParams{Cwd: t.TempDir(), McpServers: []McpServer{}}, &newRes); err != nil {
		t.Fatalf("session/new: %v", err)
	}
	if newRes.SessionID == "" {
		t.Fatal("session/new returned empty sessionId")
	}
	if newRes.Modes == nil || newRes.Modes.CurrentModeID != string(agent.PermissionModeAuto) {
		t.Fatalf("expected auto mode, got %+v", newRes.Modes)
	}
	if len(newRes.ConfigOptions) != 2 || newRes.ConfigOptions[0].ID != configIDModel || newRes.ConfigOptions[0].CurrentValue != "fake-model" {
		t.Fatalf("model config option = %+v, want fake-model fallback", newRes.ConfigOptions)
	}
	if newRes.ConfigOptions[1].ID != configIDMode || newRes.ConfigOptions[1].CurrentValue != string(agent.PermissionModeAuto) {
		t.Fatalf("mode config option = %+v", newRes.ConfigOptions[1])
	}

	// session/prompt
	var promptRes PromptResult
	if err := h.client.Call(ctx, MethodSessionPrompt, PromptParams{
		SessionID: newRes.SessionID,
		Prompt:    []ContentBlock{TextBlock("hi")},
	}, &promptRes); err != nil {
		t.Fatalf("session/prompt: %v", err)
	}
	if promptRes.StopReason != StopEndTurn {
		t.Fatalf("stopReason = %q, want %q", promptRes.StopReason, StopEndTurn)
	}

	// The streamed agent_message_chunk(s) should carry the assistant text.
	if got := drainText(t, h.updates); !strings.Contains(got, "Hello from ZERO") {
		t.Fatalf("streamed text = %q, want it to contain the assistant message", got)
	}
}

func TestACPListsOnlyResumableSessionMetadata(t *testing.T) {
	deps := testDeps(t)
	deps.ResolveWorkspaceRoot = func(cwd string) (string, error) { return filepath.Clean(cwd), nil }
	workspaceA := t.TempDir()
	workspaceB := t.TempDir()
	for _, input := range []sessions.CreateInput{
		{SessionID: "desktop-a", Title: "First", Cwd: workspaceA, ModelID: "model-a"},
		{SessionID: "desktop-b", Title: "Second", Cwd: workspaceB, ModelID: "model-b"},
		{SessionID: "child-run", SessionKind: sessions.SessionKindChild, Title: "Internal child", Cwd: workspaceA},
	} {
		if _, err := deps.Store.Create(input); err != nil {
			t.Fatalf("create %s: %v", input.SessionID, err)
		}
	}

	h := newHarness(t, deps)
	defer h.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var all ListSessionsResult
	if err := h.client.Call(ctx, MethodSessionList, ListSessionsParams{}, &all); err != nil {
		t.Fatalf("session/list: %v", err)
	}
	if len(all.Sessions) != 2 {
		t.Fatalf("all sessions = %+v, want two resumable sessions", all.Sessions)
	}
	byID := make(map[string]SessionInfo, len(all.Sessions))
	for _, item := range all.Sessions {
		byID[item.SessionID] = item
	}
	if _, found := byID["child-run"]; found {
		t.Fatal("agent-owned child session leaked into the desktop session picker")
	}
	// The fixture's spelling is the INPUT; the resolver's is the OUTPUT the
	// listing promises. Under a TMPDIR carrying a ".." component t.TempDir()
	// keeps that spelling while the test resolver cleans it, and comparing the
	// two representations failed a correct result. Expected values describe the
	// output contract. Reported by @jatmn.
	if got := byID["desktop-a"]; got.Title != "First" || got.Cwd != filepath.Clean(workspaceA) || got.Meta == nil || got.Meta.ModelID != "model-a" || got.Meta.CreatedAt == "" || got.UpdatedAt == "" {
		t.Fatalf("desktop-a summary = %+v", got)
	}

	var filtered ListSessionsResult
	if err := h.client.Call(ctx, MethodSessionList, ListSessionsParams{Cwd: workspaceB}, &filtered); err != nil {
		t.Fatalf("filtered session/list: %v", err)
	}
	if len(filtered.Sessions) != 1 || filtered.Sessions[0].SessionID != "desktop-b" {
		t.Fatalf("filtered sessions = %+v, want only desktop-b", filtered.Sessions)
	}

	var equivalent ListSessionsResult
	equivalentPath := workspaceB + string(os.PathSeparator) + "."
	if err := h.client.Call(ctx, MethodSessionList, ListSessionsParams{Cwd: equivalentPath}, &equivalent); err != nil {
		t.Fatalf("equivalent-path session/list: %v", err)
	}
	if len(equivalent.Sessions) != 1 || equivalent.Sessions[0].SessionID != "desktop-b" {
		t.Fatalf("equivalent-path sessions = %+v, want only desktop-b", equivalent.Sessions)
	}

	err := h.client.Call(ctx, MethodSessionList, ListSessionsParams{Cursor: "not-issued"}, &ListSessionsResult{})
	var rpcErr *rpcError
	if !errors.As(err, &rpcErr) || rpcErr.Code != codeInvalidParams {
		t.Fatalf("invalid cursor error = %v, want invalid params", err)
	}
}

func TestACPLoadReplaysHistoryAndResumeDoesNot(t *testing.T) {
	deps := testDeps(t)
	workspace := t.TempDir()
	created, err := deps.Store.Create(sessions.CreateInput{SessionID: "replay-session", Title: "Replay", Cwd: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deps.Store.AppendEvents(created.SessionID, []sessions.AppendEventInput{
		{Type: sessions.EventMessage, Payload: map[string]any{"role": "user", "content": "first user"}},
		{Type: sessions.EventMessage, Payload: map[string]any{"role": "assistant", "content": "first answer"}},
		{Type: sessions.EventMessage, Payload: map[string]any{"role": "user", "content": "second user"}},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	loader := newHarness(t, deps)
	var loaded LoadSessionResult
	if err := loader.client.Call(ctx, MethodSessionLoad, LoadSessionParams{SessionID: created.SessionID, Cwd: workspace, McpServers: []McpServer{}}, &loaded); err != nil {
		t.Fatalf("session/load: %v", err)
	}
	wantKinds := []string{UpdateUserMessageChunk, UpdateAgentMessageChunk, UpdateUserMessageChunk}
	wantText := []string{"first user", "first answer", "second user"}
	seenIDs := map[string]bool{}
	firstLoadIDs := make([]string, 0, len(wantKinds))
	for i := range wantKinds {
		select {
		case update := <-loader.notifications:
			if update.SessionUpdate != wantKinds[i] || update.Content.Text != wantText[i] {
				t.Fatalf("history update %d = %+v", i, update)
			}
			if update.MessageID == "" || seenIDs[update.MessageID] {
				t.Fatalf("history update %d has missing/duplicate message id %q", i, update.MessageID)
			}
			seenIDs[update.MessageID] = true
			firstLoadIDs = append(firstLoadIDs, update.MessageID)
		case <-ctx.Done():
			t.Fatalf("history update %d was not replayed", i)
		}
	}
	loader.stop()
	secondLoader := newHarness(t, deps)
	if err := secondLoader.client.Call(ctx, MethodSessionLoad, LoadSessionParams{SessionID: created.SessionID, Cwd: workspace, McpServers: []McpServer{}}, &LoadSessionResult{}); err != nil {
		t.Fatalf("second session/load: %v", err)
	}
	for i, wantID := range firstLoadIDs {
		select {
		case update := <-secondLoader.notifications:
			if update.MessageID != wantID {
				t.Fatalf("second load message id %d = %q, want stable %q", i, update.MessageID, wantID)
			}
		case <-ctx.Done():
			t.Fatalf("second load history update %d was not replayed", i)
		}
	}
	secondLoader.stop()

	resumer := newHarness(t, deps)
	defer resumer.stop()
	if err := resumer.client.Call(ctx, MethodSessionResume, ResumeSessionParams{SessionID: created.SessionID, Cwd: workspace, McpServers: []McpServer{}}, &ResumeSessionResult{}); err != nil {
		t.Fatalf("session/resume: %v", err)
	}
	select {
	case update := <-resumer.notifications:
		t.Fatalf("session/resume replayed history: %+v", update)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestACPLoadAndResumeStayBoundToThePersistedWorkspace(t *testing.T) {
	deps := testDeps(t)
	workspaceA := t.TempDir()
	workspaceB := t.TempDir()
	created, err := deps.Store.Create(sessions.CreateInput{SessionID: "workspace-bound", Cwd: workspaceA})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, deps)
	defer h.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, method := range []string{MethodSessionLoad, MethodSessionResume} {
		err := h.client.Call(ctx, method, LoadSessionParams{SessionID: created.SessionID, Cwd: workspaceB, McpServers: []McpServer{}}, &LoadSessionResult{})
		var rpcErr *rpcError
		if !errors.As(err, &rpcErr) || rpcErr.Code != codeInvalidParams || !strings.Contains(rpcErr.Message, "persisted workspace") {
			t.Fatalf("%s with mismatched cwd error = %v, want persisted-workspace invalid params", method, err)
		}
	}

	missing, err := deps.Store.Create(sessions.CreateInput{SessionID: "workspace-missing"})
	if err != nil {
		t.Fatal(err)
	}
	err = h.client.Call(ctx, MethodSessionResume, ResumeSessionParams{SessionID: missing.SessionID, Cwd: workspaceA, McpServers: []McpServer{}}, &ResumeSessionResult{})
	var rpcErr *rpcError
	if !errors.As(err, &rpcErr) || rpcErr.Code != codeInvalidParams || !strings.Contains(rpcErr.Message, "no persisted workspace") {
		t.Fatalf("resume with missing persisted cwd error = %v, want invalid params", err)
	}
}

func TestACPModelConfigOptionsCatalogSelectionAndLoad(t *testing.T) {
	deps := testDeps(t)
	deps.ResolveConfig = func(_ string, o config.Overrides) (config.ResolvedConfig, error) {
		model := "gpt-5.5"
		if o.Provider.Model != "" {
			model = o.Provider.Model
		}
		return config.ResolvedConfig{Provider: config.ProviderProfile{
			Name: "ChatGPT", CatalogID: "chatgpt", Model: model,
		}}, nil
	}
	deps.DiscoverModels = func(_ context.Context, _ config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
		return []providermodeldiscovery.Model{
			{ID: " gpt-5.4-mini ", Description: "Fast"},
			{ID: "gpt-5.5", Description: "duplicate configured model"},
		}, nil
	}
	h := newHarness(t, deps)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	workspace := t.TempDir()
	var created NewSessionResult
	if err := h.client.Call(ctx, MethodSessionNew, NewSessionParams{Cwd: workspace}, &created); err != nil {
		t.Fatalf("session/new: %v", err)
	}
	option := created.ConfigOptions[0]
	if option.CurrentValue != "gpt-5.5" || len(option.Options) < 2 {
		t.Fatalf("new model option = %+v", option)
	}
	for _, choice := range option.Options {
		if choice.Name != choice.Value {
			t.Fatalf("model choice name = %q, want model id %q", choice.Name, choice.Value)
		}
	}
	var selected SetSessionConfigOptionResult
	if err := h.client.Call(ctx, MethodSessionSetConfigOption, SetSessionConfigOptionParams{
		SessionID: created.SessionID, ConfigID: configIDModel, Value: "  gpt-5.4-mini  ",
	}, &selected); err != nil {
		t.Fatalf("set_config_option: %v", err)
	}
	if got := selected.ConfigOptions[0].CurrentValue; got != "gpt-5.4-mini" {
		t.Fatalf("selected model = %q", got)
	}
	if err := h.client.Call(ctx, MethodSessionSetConfigOption, SetSessionConfigOptionParams{
		SessionID: created.SessionID, ConfigID: configIDModel, Value: "not-advertised",
	}, &SetSessionConfigOptionResult{}); err == nil {
		t.Fatal("unknown standard model selection was accepted")
	}
	h.stop()
	h = newHarness(t, deps)
	defer h.stop()
	var loaded LoadSessionResult
	if err := h.client.Call(ctx, MethodSessionLoad, LoadSessionParams{SessionID: created.SessionID, Cwd: workspace}, &loaded); err != nil {
		t.Fatalf("session/load: %v", err)
	}
	if len(loaded.ConfigOptions) != 2 || loaded.ConfigOptions[0].CurrentValue != "gpt-5.4-mini" {
		t.Fatalf("load model option = %+v", loaded.ConfigOptions)
	}
	if loaded.ConfigOptions[1].CurrentValue != string(agent.PermissionModeAuto) {
		t.Fatalf("load mode option = %+v", loaded.ConfigOptions[1])
	}
}

func TestACPModelDiscoveryFailureUsesConfiguredFallbackOnly(t *testing.T) {
	deps := testDeps(t)
	deps.ResolveConfig = func(_ string, _ config.Overrides) (config.ResolvedConfig, error) {
		return config.ResolvedConfig{Provider: config.ProviderProfile{
			Name: "ChatGPT", CatalogID: "chatgpt", Model: " configured-model ",
		}}, nil
	}
	deps.DiscoverModels = func(context.Context, config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
		return nil, fmt.Errorf("discovery unavailable")
	}
	h := newHarness(t, deps)
	defer h.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var created NewSessionResult
	if err := h.client.Call(ctx, MethodSessionNew, NewSessionParams{Cwd: t.TempDir()}, &created); err != nil {
		t.Fatalf("session/new: %v", err)
	}
	option := created.ConfigOptions[0]
	if option.CurrentValue != "configured-model" || len(option.Options) != 1 || option.Options[0].Value != "configured-model" {
		t.Fatalf("fallback model option = %+v", option)
	}
}

func TestACPCustomProviderAllowsUnadvertisedModel(t *testing.T) {
	deps := testDeps(t)
	deps.ResolveConfig = func(_ string, o config.Overrides) (config.ResolvedConfig, error) {
		model := "configured-model"
		if o.Provider.Model != "" {
			model = o.Provider.Model
		}
		return config.ResolvedConfig{Provider: config.ProviderProfile{
			Name: "Custom", CatalogID: "custom-openai-compatible", Model: model,
		}}, nil
	}
	h := newHarness(t, deps)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	workspace := t.TempDir()
	var created NewSessionResult
	if err := h.client.Call(ctx, MethodSessionNew, NewSessionParams{Cwd: workspace}, &created); err != nil {
		t.Fatalf("session/new: %v", err)
	}
	var selected SetSessionConfigOptionResult
	if err := h.client.Call(ctx, MethodSessionSetConfigOption, SetSessionConfigOptionParams{
		SessionID: created.SessionID, ConfigID: configIDModel, Value: " vendor/model ",
	}, &selected); err != nil {
		t.Fatalf("set custom model: %v", err)
	}
	if got := selected.ConfigOptions[0].CurrentValue; got != "vendor/model" {
		t.Fatalf("custom model = %q", got)
	}
	h.stop()
	h = newHarness(t, deps)
	defer h.stop()
	var loaded LoadSessionResult
	if err := h.client.Call(ctx, MethodSessionLoad, LoadSessionParams{SessionID: created.SessionID, Cwd: workspace}, &loaded); err != nil {
		t.Fatalf("session/load: %v", err)
	}
	option := loaded.ConfigOptions[0]
	if option.CurrentValue != "vendor/model" || !modelChoiceExists(option.Options, "vendor/model") {
		t.Fatalf("loaded custom model option = %+v", option)
	}
}

func TestACPModelDiscoveryFiltersProviderIncompatibleModels(t *testing.T) {
	a := &Agent{deps: Deps{
		ResolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{Provider: config.ProviderProfile{
				CatalogID: "opencode-go-anthropic-compatible", Model: "minimax-m3",
			}}, nil
		},
		DiscoverModels: func(context.Context, config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
			return []providermodeldiscovery.Model{{ID: "qwen3.7-plus"}, {ID: "claude-sonnet-4.5"}}, nil
		},
	}}
	_, options, restricted, err := a.resolveModelChoices(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !restricted || !modelChoiceExists(options, "qwen3.7-plus") || modelChoiceExists(options, "claude-sonnet-4.5") {
		t.Fatalf("filtered model options = %+v, restricted=%v", options, restricted)
	}
}

func TestACPModelDiscoveryHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := &Agent{deps: Deps{
		ResolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{Provider: config.ProviderProfile{Model: "configured-model"}}, nil
		},
		DiscoverModels: func(context.Context, config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
			return nil, context.Canceled
		},
	}}
	if _, _, _, err := a.resolveModelChoices(ctx, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("resolve error = %v, want context canceled", err)
	}
}

func TestACPConfigOptionWireSchema(t *testing.T) {
	b, err := json.Marshal(SessionConfigOption{
		ID: "model", Name: "Model", Description: "desc", Category: "model",
		Type: configOptionTypeSelect, CurrentValue: "m1",
		Options: []SessionConfigOptionValue{{Value: "m1", Name: "m1", Description: "choice"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"type", "category", "currentValue", "options"} {
		if _, ok := wire[key]; !ok {
			t.Errorf("wire field %q absent: %s", key, b)
		}
	}
	if _, ok := wire["value"]; ok {
		t.Errorf("obsolete option value field present: %s", b)
	}
	if _, ok := wire["values"]; ok {
		t.Errorf("obsolete values field present: %s", b)
	}
	choice := wire["options"].([]any)[0].(map[string]any)
	if choice["value"] != "m1" {
		t.Errorf("options[].value = %#v", choice["value"])
	}
}

func TestACPUnknownSessionPromptErrors(t *testing.T) {
	h := newHarness(t, testDeps(t))
	defer h.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := h.client.Call(ctx, MethodSessionPrompt, PromptParams{SessionID: "nope", Prompt: []ContentBlock{TextBlock("x")}}, &PromptResult{})
	if err == nil {
		t.Fatal("expected error for unknown session")
	}
}

func TestACPSetModeUpdatesSession(t *testing.T) {
	h := newHarness(t, testDeps(t))
	defer h.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var newRes NewSessionResult
	if err := h.client.Call(ctx, MethodSessionNew, NewSessionParams{Cwd: t.TempDir(), McpServers: []McpServer{}}, &newRes); err != nil {
		t.Fatalf("session/new: %v", err)
	}
	// auto/ask are accepted.
	if err := h.client.Call(ctx, MethodSessionSetMode, SetSessionModeParams{SessionID: newRes.SessionID, ModeID: string(agent.PermissionModeAsk)}, &SetSessionModeResult{}); err != nil {
		t.Fatalf("set_mode ask: %v", err)
	}
	var configured SetSessionConfigOptionResult
	if err := h.client.Call(ctx, MethodSessionSetConfigOption, SetSessionConfigOptionParams{
		SessionID: newRes.SessionID, ConfigID: configIDMode, Value: string(agent.PermissionModeAuto),
	}, &configured); err != nil {
		t.Fatalf("set_config_option mode: %v", err)
	}
	if got := configured.ConfigOptions[1].CurrentValue; got != string(agent.PermissionModeAuto) {
		t.Fatalf("configured mode = %q", got)
	}
	// Plan is accepted: it only narrows capability (read-only), so unlike Unsafe
	// there is no elevation risk in letting a client select it.
	if err := h.client.Call(ctx, MethodSessionSetMode, SetSessionModeParams{SessionID: newRes.SessionID, ModeID: string(agent.PermissionModePlan)}, &SetSessionModeResult{}); err != nil {
		t.Fatalf("set_mode plan: %v", err)
	}
	// Configuration path is a separate contract from MethodSessionSetMode: the
	// mode option is advertised via configOptions and applied by handleSetConfigOption.
	var planConfigured SetSessionConfigOptionResult
	if err := h.client.Call(ctx, MethodSessionSetConfigOption, SetSessionConfigOptionParams{
		SessionID: newRes.SessionID, ConfigID: configIDMode, Value: string(agent.PermissionModePlan),
	}, &planConfigured); err != nil {
		t.Fatalf("set_config_option plan: %v", err)
	}
	if got := planConfigured.ConfigOptions[1].CurrentValue; got != string(agent.PermissionModePlan) {
		t.Fatalf("configured mode after plan = %q, want plan", got)
	}
	hasPlanOption := false
	for _, opt := range planConfigured.ConfigOptions[1].Options {
		if opt.Value == string(agent.PermissionModePlan) {
			hasPlanOption = true
			break
		}
	}
	if !hasPlanOption {
		t.Fatalf("config mode options missing plan: %#v", planConfigured.ConfigOptions[1].Options)
	}
	// Unsafe must be rejected over ACP — a client can't self-grant no-prompt host access.
	if err := h.client.Call(ctx, MethodSessionSetMode, SetSessionModeParams{SessionID: newRes.SessionID, ModeID: string(agent.PermissionModeUnsafe)}, &SetSessionModeResult{}); err == nil {
		t.Fatal("expected Unsafe mode to be rejected over ACP")
	}
	// An unknown mode must be rejected.
	if err := h.client.Call(ctx, MethodSessionSetMode, SetSessionModeParams{SessionID: newRes.SessionID, ModeID: "bogus"}, &SetSessionModeResult{}); err == nil {
		t.Fatal("expected error for unknown mode")
	}
}

// TestACPPlanModeWiresPermissionModeIntoAgentOptions confirms selecting "plan"
// over ACP actually reaches agent.Options.PermissionMode for the next turn —
// the same gap this test's TUI counterpart covers for /plan on.
func TestACPPlanModeWiresPermissionModeIntoAgentOptions(t *testing.T) {
	deps := testDeps(t)
	var captured agent.Options
	deps.RunAgent = func(_ context.Context, _ string, _ zeroruntime.Provider, opts agent.Options) (agent.Result, error) {
		captured = opts
		return agent.Result{FinalAnswer: "ok"}, nil
	}

	h := newHarness(t, deps)
	defer h.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var newRes NewSessionResult
	if err := h.client.Call(ctx, MethodSessionNew, NewSessionParams{Cwd: t.TempDir(), McpServers: []McpServer{}}, &newRes); err != nil {
		t.Fatalf("session/new: %v", err)
	}
	if err := h.client.Call(ctx, MethodSessionSetMode, SetSessionModeParams{SessionID: newRes.SessionID, ModeID: string(agent.PermissionModePlan)}, &SetSessionModeResult{}); err != nil {
		t.Fatalf("set_mode plan: %v", err)
	}
	if err := h.client.Call(ctx, MethodSessionPrompt, PromptParams{SessionID: newRes.SessionID, Prompt: []ContentBlock{TextBlock("plan it out")}}, &PromptResult{}); err != nil {
		t.Fatalf("session/prompt: %v", err)
	}
	if captured.PermissionMode != agent.PermissionModePlan {
		t.Fatalf("agent.Options.PermissionMode = %q, want plan", captured.PermissionMode)
	}
}

// TestACPRunTurnWiresSandboxAndScopedRegistry proves the sandbox engine and the
// scoped registry from BuildWorkspace actually reach agent.Options — i.e. ACP
// shell tools run confined, not unconfined on the host.
func TestACPRunTurnWiresSandboxAndScopedRegistry(t *testing.T) {
	deps := testDeps(t)
	reg := tools.NewRegistry()
	reg.Register(tools.NewUpdatePlanTool())
	engine := sandbox.NewEngine(sandbox.EngineOptions{WorkspaceRoot: t.TempDir()})
	deps.BuildWorkspace = func(context.Context, string, config.ResolvedConfig, agent.PermissionMode) (*Workspace, error) {
		return &Workspace{Registry: reg, Sandbox: engine}, nil
	}
	var captured agent.Options
	deps.RunAgent = func(_ context.Context, _ string, _ zeroruntime.Provider, opts agent.Options) (agent.Result, error) {
		captured = opts
		return agent.Result{FinalAnswer: "ok"}, nil
	}

	h := newHarness(t, deps)
	defer h.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var newRes NewSessionResult
	if err := h.client.Call(ctx, MethodSessionNew, NewSessionParams{Cwd: t.TempDir(), McpServers: []McpServer{}}, &newRes); err != nil {
		t.Fatalf("session/new: %v", err)
	}
	if err := h.client.Call(ctx, MethodSessionPrompt, PromptParams{SessionID: newRes.SessionID, Prompt: []ContentBlock{TextBlock("hi")}}, &PromptResult{}); err != nil {
		t.Fatalf("session/prompt: %v", err)
	}
	if captured.Sandbox != engine {
		t.Fatal("sandbox engine was not wired into agent.Options (shell tools would run unconfined)")
	}
	if captured.Registry != reg {
		t.Fatal("scoped registry was not wired into agent.Options")
	}
}

func TestACPRunTurnUsesWorkspaceDeferThresholdAtProviderBoundary(t *testing.T) {
	for _, tc := range []struct {
		name      string
		threshold int
		want      []string
	}{
		{name: "explicit zero disables", threshold: 0, want: []string{"mcp_docs_alpha", "mcp_docs_beta"}},
		{name: "below threshold stays eager", threshold: 3, want: []string{"mcp_docs_alpha", "mcp_docs_beta"}},
		{name: "at threshold defers", threshold: 2, want: []string{tools.ToolSearchToolName}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := make(chan zeroruntime.CompletionRequest, 4)
			deps := testDeps(t)
			deps.NewProvider = func(config.ProviderProfile) (zeroruntime.Provider, error) {
				return captureToolsProvider{requests: requests}, nil
			}
			deps.BuildWorkspace = func(context.Context, string, config.ResolvedConfig, agent.PermissionMode) (*Workspace, error) {
				registry := tools.NewRegistry()
				registry.Register(acpDeferredPromptTool{name: "mcp_docs_alpha"})
				registry.Register(acpDeferredPromptTool{name: "mcp_docs_beta"})
				registry.Register(tools.NewToolSearchTool(registry))
				return &Workspace{Registry: registry, DeferThreshold: tc.threshold}, nil
			}

			h := newHarness(t, deps)
			defer h.stop()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var created NewSessionResult
			if err := h.client.Call(ctx, MethodSessionNew, NewSessionParams{Cwd: t.TempDir()}, &created); err != nil {
				t.Fatal(err)
			}
			if err := h.client.Call(ctx, MethodSessionPrompt, PromptParams{SessionID: created.SessionID, Prompt: []ContentBlock{TextBlock("hi")}}, &PromptResult{}); err != nil {
				t.Fatal(err)
			}
			request := <-requests
			got := make([]string, 0, len(request.Tools))
			for _, definition := range request.Tools {
				got = append(got, definition.Name)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("provider tools = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestACPRunTurnEmitsWorkspaceSetupNotices(t *testing.T) {
	deps := testDeps(t)
	deps.BuildWorkspace = func(context.Context, string, config.ResolvedConfig, agent.PermissionMode) (*Workspace, error) {
		registry := tools.NewRegistry()
		registry.Register(tools.NewUpdatePlanTool())
		return &Workspace{Registry: registry, Notices: []string{"MCP server docs unavailable, skipped: timeout"}}, nil
	}
	h := newHarness(t, deps)
	defer h.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var created NewSessionResult
	if err := h.client.Call(ctx, MethodSessionNew, NewSessionParams{Cwd: t.TempDir()}, &created); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Call(ctx, MethodSessionPrompt, PromptParams{SessionID: created.SessionID, Prompt: []ContentBlock{TextBlock("hi")}}, &PromptResult{}); err != nil {
		t.Fatal(err)
	}
	got := drainText(t, h.updates)
	if !strings.Contains(got, "[zero warning] MCP server docs unavailable, skipped: timeout") || !strings.Contains(got, "Hello from ZERO") {
		t.Fatalf("streamed text = %q", got)
	}
}

// TestACPRunTurnClosesWorkspace proves a per-turn workspace never outlives the
// agent run. This is particularly important for MCP: its registry owns live
// client connections and, for stdio servers, child processes.
func TestACPRunTurnClosesWorkspaceAfterSuccessAndFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		runErr error
	}{
		{name: "success"},
		{name: "agent failure", runErr: errors.New("provider interrupted")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := testDeps(t)
			closed := 0
			deps.BuildWorkspace = func(context.Context, string, config.ResolvedConfig, agent.PermissionMode) (*Workspace, error) {
				registry := tools.NewRegistry()
				registry.Register(tools.NewUpdatePlanTool())
				return &Workspace{Registry: registry, Cleanup: func() error {
					closed++
					return nil
				}}, nil
			}
			deps.RunAgent = func(context.Context, string, zeroruntime.Provider, agent.Options) (agent.Result, error) {
				return agent.Result{FinalAnswer: "ok"}, tc.runErr
			}
			h := newHarness(t, deps)
			defer h.stop()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var created NewSessionResult
			if err := h.client.Call(ctx, MethodSessionNew, NewSessionParams{Cwd: t.TempDir()}, &created); err != nil {
				t.Fatalf("session/new: %v", err)
			}
			err := h.client.Call(ctx, MethodSessionPrompt, PromptParams{SessionID: created.SessionID, Prompt: []ContentBlock{TextBlock("hello")}}, &PromptResult{})
			if tc.runErr == nil && err != nil {
				t.Fatalf("session/prompt: %v", err)
			}
			if tc.runErr != nil && err == nil {
				t.Fatal("session/prompt succeeded after agent failure")
			}
			if closed != 1 {
				t.Fatalf("workspace cleanup calls = %d, want 1", closed)
			}
		})
	}
}

// TestACPRejectsInvalidCwd confirms session/new fails when the workspace root
// resolver rejects the client cwd (e.g. filesystem root).
func TestACPRejectsInvalidCwd(t *testing.T) {
	deps := testDeps(t)
	deps.ResolveWorkspaceRoot = func(string) (string, error) {
		return "", fmt.Errorf("cwd must not be the filesystem root")
	}
	h := newHarness(t, deps)
	defer h.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.client.Call(ctx, MethodSessionNew, NewSessionParams{Cwd: "/", McpServers: []McpServer{}}, &NewSessionResult{}); err == nil {
		t.Fatal("expected session/new to reject an invalid cwd")
	}
}

func TestACPPromptWarnsWhenTurnPersistenceFails(t *testing.T) {
	deps := testDeps(t)
	h := newHarness(t, deps)
	defer h.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var newRes NewSessionResult
	if err := h.client.Call(ctx, MethodSessionNew, NewSessionParams{Cwd: t.TempDir(), McpServers: []McpServer{}}, &newRes); err != nil {
		t.Fatalf("session/new: %v", err)
	}
	metadataPath := filepath.Join(deps.Store.RootDir, newRes.SessionID, sessions.MetadataFile)
	if err := os.Remove(metadataPath); err != nil {
		t.Fatalf("remove metadata: %v", err)
	}

	var promptRes PromptResult
	if err := h.client.Call(ctx, MethodSessionPrompt, PromptParams{
		SessionID: newRes.SessionID,
		Prompt:    []ContentBlock{TextBlock("hi")},
	}, &promptRes); err != nil {
		t.Fatalf("session/prompt: %v", err)
	}
	if promptRes.StopReason != StopEndTurn {
		t.Fatalf("stopReason = %q, want %q", promptRes.StopReason, StopEndTurn)
	}
	got := drainTextUntil(t, h.updates, func(text string) bool {
		return strings.Contains(text, "Hello from ZERO") &&
			strings.Contains(text, "Could not save session history")
	})
	if !strings.Contains(got, "Could not save session history") {
		t.Fatalf("streamed text = %q, want persistence warning", got)
	}
}

func TestACPHardTurnFailureDoesNotCommitHistory(t *testing.T) {
	deps := testDeps(t)
	deps.RunAgent = func(_ context.Context, _ string, _ zeroruntime.Provider, opts agent.Options) (agent.Result, error) {
		call := agent.ToolCall{ID: "failed-call", Name: "read_file", Arguments: `{"path":"a.go"}`}
		opts.OnToolCall(call)
		opts.OnToolResult(agent.ToolResult{ToolCallID: call.ID, Name: call.Name, Status: tools.StatusOK, Output: "content"})
		return agent.Result{FinalAnswer: "partial answer"}, errors.New("injected hard run failure")
	}

	h := newHarness(t, deps)
	defer h.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var created NewSessionResult
	if err := h.client.Call(ctx, MethodSessionNew, NewSessionParams{Cwd: t.TempDir()}, &created); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Call(ctx, MethodSessionPrompt, PromptParams{
		SessionID: created.SessionID, Prompt: []ContentBlock{TextBlock("failed question")},
	}, &PromptResult{}); err == nil {
		t.Fatal("hard RunAgent failure was reported as a successful turn")
	}

	sess := h.agent.session(created.SessionID)
	if sess == nil || len(sess.snapshotHistory()) != 0 {
		t.Fatalf("failed turn changed live history: %+v", sess)
	}
	events, err := deps.Store.ReadEvents(created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("failed turn changed durable history: %+v", events)
	}
	loader := newHarness(t, deps)
	defer loader.stop()
	if err := loader.client.Call(ctx, MethodSessionLoad, LoadSessionParams{
		SessionID: created.SessionID, Cwd: sess.cwd,
	}, &LoadSessionResult{}); err != nil {
		t.Fatalf("fresh session/load: %v", err)
	}
	loaded := loader.agent.session(created.SessionID)
	if loaded == nil || len(loaded.snapshotHistory()) != 0 {
		t.Fatalf("fresh load disagreed with the uncommitted turn: %+v", loaded)
	}
}

func TestACPCancelledTurnCommitsAtOutcomeBoundary(t *testing.T) {
	deps := testDeps(t)
	deps.RunAgent = func(context.Context, string, zeroruntime.Provider, agent.Options) (agent.Result, error) {
		return agent.Result{}, context.Canceled
	}
	h := newHarness(t, deps)
	defer h.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var created NewSessionResult
	if err := h.client.Call(ctx, MethodSessionNew, NewSessionParams{Cwd: t.TempDir()}, &created); err != nil {
		t.Fatal(err)
	}
	var result PromptResult
	if err := h.client.Call(ctx, MethodSessionPrompt, PromptParams{
		SessionID: created.SessionID, Prompt: []ContentBlock{TextBlock("cancelled question")},
	}, &result); err != nil {
		t.Fatalf("cancelled turn returned an RPC error: %v", err)
	}
	if result.StopReason != StopCancelled {
		t.Fatalf("stop reason = %q, want %q", result.StopReason, StopCancelled)
	}
	sess := h.agent.session(created.SessionID)
	if got := sess.snapshotHistory(); len(got) != 1 || got[0].user != "cancelled question" {
		t.Fatalf("cancelled turn was not committed to live history: %+v", got)
	}
	events, err := deps.Store.ReadEvents(created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != sessions.EventMessage {
		t.Fatalf("cancelled turn was not committed to durable history: %+v", events)
	}
}

func TestACPLoadWarnsWhenHistoryReadFails(t *testing.T) {
	deps := testDeps(t)
	cwd := t.TempDir()
	meta, err := deps.Store.Create(sessions.CreateInput{Title: "ACP session", Cwd: cwd})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	eventsPath := filepath.Join(deps.Store.RootDir, meta.SessionID, sessions.EventsFile)
	if err := os.WriteFile(eventsPath, []byte("{bad json}\n"), 0o600); err != nil {
		t.Fatalf("write corrupt events: %v", err)
	}
	h := newHarness(t, deps)
	defer h.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := h.client.Call(ctx, MethodSessionLoad, LoadSessionParams{SessionID: meta.SessionID, Cwd: cwd, McpServers: []McpServer{}}, &LoadSessionResult{}); err != nil {
		t.Fatalf("session/load: %v", err)
	}
	got := drainTextUntil(t, h.updates, func(text string) bool {
		return strings.Contains(text, "Could not load session history")
	})
	if !strings.Contains(got, "Could not load session history") {
		t.Fatalf("streamed text = %q, want load warning", got)
	}
}

func TestACPSameConnectionReloadRefreshesRecoveredHistory(t *testing.T) {
	deps := testDeps(t)
	cwd := t.TempDir()
	meta, err := deps.Store.Create(sessions.CreateInput{Title: "ACP session", Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	eventsPath := filepath.Join(deps.Store.RootDir, meta.SessionID, sessions.EventsFile)
	if err := os.WriteFile(eventsPath, []byte("{bad json}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prompts := make(chan string, 1)
	deps.RunAgent = func(_ context.Context, prompt string, _ zeroruntime.Provider, _ agent.Options) (agent.Result, error) {
		prompts <- prompt
		return agent.Result{FinalAnswer: "continued"}, nil
	}

	h := newHarness(t, deps)
	defer h.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	params := LoadSessionParams{SessionID: meta.SessionID, Cwd: cwd}
	if err := h.client.Call(ctx, MethodSessionLoad, params, &LoadSessionResult{}); err != nil {
		t.Fatalf("initial best-effort load: %v", err)
	}
	select {
	case <-h.notifications: // discard the expected best-effort load warning
	case <-ctx.Done():
		t.Fatal("initial best-effort load did not surface its warning")
	}
	if err := os.WriteFile(eventsPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := deps.Store.AppendEvents(meta.SessionID, []sessions.AppendEventInput{
		{Type: sessions.EventMessage, Payload: map[string]any{"role": "user", "content": "recovered question"}},
		{Type: sessions.EventMessage, Payload: map[string]any{"role": "assistant", "content": "recovered answer"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Call(ctx, MethodSessionLoad, params, &LoadSessionResult{}); err != nil {
		t.Fatalf("load after storage repair: %v", err)
	}
	for index, want := range []struct {
		kind string
		text string
	}{
		{UpdateUserMessageChunk, "recovered question"},
		{UpdateAgentMessageChunk, "recovered answer"},
	} {
		select {
		case update := <-h.notifications:
			if update.SessionUpdate != want.kind || update.Content.Text != want.text {
				t.Fatalf("replayed history %d = %+v, want %s %q", index, update, want.kind, want.text)
			}
		case <-ctx.Done():
			t.Fatalf("replayed history %d never arrived", index)
		}
	}
	if err := h.client.Call(ctx, MethodSessionPrompt, PromptParams{
		SessionID: meta.SessionID, Prompt: []ContentBlock{TextBlock("continue")},
	}, &PromptResult{}); err != nil {
		t.Fatalf("prompt after repaired reload: %v", err)
	}
	select {
	case prompt := <-prompts:
		if !strings.Contains(prompt, "recovered question") || !strings.Contains(prompt, "recovered answer") {
			t.Fatalf("same-connection reload discarded recovered history:\n%s", prompt)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

// drainText collects streamed chunks for a short window and concatenates them.
func drainText(t *testing.T, ch <-chan string) string {
	t.Helper()
	return drainTextUntil(t, ch, func(text string) bool {
		return strings.Contains(text, "Hello from ZERO")
	})
}

func drainTextUntil(t *testing.T, ch <-chan string, done func(string) bool) string {
	t.Helper()
	var b strings.Builder
	deadline := time.After(2 * time.Second)
	for {
		select {
		case s := <-ch:
			b.WriteString(s)
			if done(b.String()) {
				return b.String()
			}
		case <-deadline:
			return b.String()
		}
	}
}

// normalisingResolver reproduces what ResolveWorkspaceRoot actually does — abs,
// Clean, and a stat that the path exists — WITHOUT resolving symlinks or folding
// case, which is the behaviour that makes two spellings of one directory produce
// two different roots.
//
// The package's own testDeps resolver is the identity function. Under it the
// workspace guard degenerates to "are these two strings different", fed two
// unrelated temp directories, so it can only ever answer yes: the rejection
// direction is pinned and the acceptance direction is asserted nowhere.
func normalisingResolver(t *testing.T) func(string) (string, error) {
	t.Helper()
	return func(cwd string) (string, error) {
		absolute, err := filepath.Abs(cwd)
		if err != nil {
			return "", err
		}
		absolute = filepath.Clean(absolute)
		info, err := os.Stat(absolute)
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			return "", errors.New("workspace is not a directory")
		}
		return absolute, nil
	}
}

// ONE DIRECTORY UNDER TWO SPELLINGS IS ONE WORKSPACE.
//
// A session persisted from the TUI has to stay resumable from an editor holding
// a different spelling of the same project folder — the two processes most
// likely to disagree about spelling, and the case this feature exists for. It
// failed closed, so it blocked legitimate resumes rather than admitting foreign
// ones, but session/list filtered by the other spelling returned nothing, which
// makes it an invisible failure rather than a reported one.
func TestACPResumesAcrossTwoSpellingsOfOneWorkspace(t *testing.T) {
	real := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	directoryAlias(t, real, alias)
	// The premise: the resolver really does produce two different strings.
	resolve := normalisingResolver(t)
	realRoot, err := resolve(real)
	if err != nil {
		t.Fatal(err)
	}
	aliasRoot, err := resolve(alias)
	if err != nil {
		t.Fatal(err)
	}
	if realRoot == aliasRoot {
		t.Skipf("this filesystem folds the alias away (%q == %q); the guard cannot be exercised", realRoot, aliasRoot)
	}

	deps := testDeps(t)
	deps.ResolveWorkspaceRoot = resolve
	created, err := deps.Store.Create(sessions.CreateInput{SessionID: "aliased-workspace", Cwd: real})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, deps)
	defer h.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// ACCEPTANCE: resume and load under the OTHER spelling must both work.
	for _, method := range []string{MethodSessionLoad, MethodSessionResume} {
		var result LoadSessionResult
		if err := h.client.Call(ctx, method, LoadSessionParams{SessionID: created.SessionID, Cwd: alias, McpServers: []McpServer{}}, &result); err != nil {
			t.Errorf("%s under an alias of the persisted workspace failed: %v", method, err)
		}
	}

	// And session/list filtered by the alias must still find it.
	var listed ListSessionsResult
	if err := h.client.Call(ctx, MethodSessionList, ListSessionsParams{Cwd: alias}, &listed); err != nil {
		t.Fatalf("session/list: %v", err)
	}
	found := false
	for _, item := range listed.Sessions {
		if item.SessionID == created.SessionID {
			found = true
		}
	}
	if !found {
		t.Errorf("session/list filtered by an alias of its own workspace returned %d sessions without it", len(listed.Sessions))
	}

	// REJECTION still holds: a genuinely different directory is refused.
	other := t.TempDir()
	err = h.client.Call(ctx, MethodSessionResume, ResumeSessionParams{SessionID: created.SessionID, Cwd: other, McpServers: []McpServer{}}, &ResumeSessionResult{})
	var rpcErr *rpcError
	if !errors.As(err, &rpcErr) || rpcErr.Code != codeInvalidParams {
		t.Errorf("resume from an unrelated workspace = %v, want invalid params", err)
	}
}

func TestACPResumeAppliesStandaloneSessionKindPolicy(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind sessions.SessionKind
		want bool
	}{
		{name: "regular", kind: "", want: true},
		{name: "fork", kind: sessions.SessionKindFork, want: true},
		{name: "child", kind: sessions.SessionKindChild},
		{name: "side", kind: sessions.SessionKindSide},
		{name: "spec draft", kind: sessions.SessionKindSpecDraft},
		{name: "spec impl", kind: sessions.SessionKindSpecImpl},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := testDeps(t)
			workspace := t.TempDir()
			created, err := deps.Store.Create(sessions.CreateInput{
				SessionID: "kind-policy", Cwd: workspace, SessionKind: tc.kind,
			})
			if err != nil {
				t.Fatal(err)
			}
			h := newHarness(t, deps)
			defer h.stop()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err = h.client.Call(ctx, MethodSessionResume, ResumeSessionParams{
				SessionID: created.SessionID, Cwd: workspace,
			}, &ResumeSessionResult{})
			if tc.want {
				if err != nil {
					t.Fatalf("session/resume: %v", err)
				}
				return
			}
			var rpcErr *rpcError
			if !errors.As(err, &rpcErr) || rpcErr.Code != codeInvalidParams {
				t.Fatalf("session/resume = %v, want invalid params", err)
			}
			if err := h.client.Call(ctx, MethodSessionPrompt, PromptParams{
				SessionID: created.SessionID, Prompt: []ContentBlock{TextBlock("must stay closed")},
			}, &PromptResult{}); err == nil {
				t.Fatal("rejected subordinate session became promptable")
			}
			if err := h.client.Call(ctx, MethodSessionLoad, LoadSessionParams{
				SessionID: created.SessionID, Cwd: workspace,
			}, &LoadSessionResult{}); err != nil {
				t.Fatalf("session/load should retain render-only access: %v", err)
			}
		})
	}
}

// THE WIRE KEYS ARE AN EXTERNAL CONTRACT, not internal names.
//
// Nothing pinned them, so renaming a Go field — or dropping an omitempty —
// would break every client and leave the suite green. These are the keys this
// feature adds to the protocol; a change here is a change to what editors
// consume.
func TestSessionWireKeysAreStable(t *testing.T) {
	marshalled := func(v any) map[string]any {
		t.Helper()
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	has := func(where string, got map[string]any, want ...string) {
		t.Helper()
		for _, key := range want {
			if _, ok := got[key]; !ok {
				t.Errorf("%s is missing the wire key %q; got %v", where, key, keysOf(got))
			}
		}
	}

	has("SessionInfo", marshalled(SessionInfo{
		SessionID: "s1", Cwd: "/w", Title: "t", UpdatedAt: "now",
		Meta: &SessionInfoMeta{ModelID: "m", CreatedAt: "then"},
	}), "sessionId", "cwd", "title", "updatedAt", "_meta")

	has("SessionInfoMeta", marshalled(SessionInfoMeta{ModelID: "m", CreatedAt: "then"}),
		"modelId", "createdAt")

	has("ListSessionsResult", marshalled(ListSessionsResult{
		Sessions: []SessionInfo{}, NextCursor: "c",
	}), "sessions", "nextCursor")

	has("ListSessionsParams", marshalled(ListSessionsParams{Cwd: "/w", Cursor: "c"}),
		"cwd", "cursor")

	// sessionCapabilities is omitempty, so it must appear when SET — a client
	// discovers list/resume support through it.
	has("AgentCapabilities", marshalled(AgentCapabilities{
		LoadSession:         true,
		SessionCapabilities: &SessionCapabilities{List: &struct{}{}, Resume: &struct{}{}},
	}), "loadSession", "promptCapabilities", "sessionCapabilities")

	capabilities := marshalled(SessionCapabilities{List: &struct{}{}, Resume: &struct{}{}})
	has("SessionCapabilities", capabilities, "list", "resume")
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// directoryAlias makes `alias` a second name for `target`.
//
// A JUNCTION ON WINDOWS, not a symlink. os.Symlink needs a privilege an ordinary
// Windows session does not hold, so a test built on it SKIPS there — and Windows
// is where junctions exist and where this bug came from. The guard would have
// been verified only on the two platforms that never had the problem. mklink /J
// needs no privilege, which is also how the defect was found.
func directoryAlias(t *testing.T, target, alias string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", alias, target).CombinedOutput(); err != nil {
			t.Skipf("cannot create a junction: %v %s", err, out)
		}
		return
	}
	if err := os.Symlink(target, alias); err != nil {
		t.Skipf("cannot create a directory alias here: %v", err)
	}
}

// A SESSION WITH NO PERSISTED WORKSPACE IS NOT RESUMABLE, SO IT IS NOT LISTED.
//
// activatePersistedSession refuses a session whose Cwd is empty, but the listing
// advertised it anyway — offering the client a menu entry that only fails when
// taken. Both halves are asserted together, because the listing is only correct
// relative to what resume will accept.
func TestSessionListOmitsSessionsWithoutAWorkspace(t *testing.T) {
	deps := testDeps(t)
	workspace := t.TempDir()
	usable, err := deps.Store.Create(sessions.CreateInput{SessionID: "has-cwd", Cwd: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deps.Store.Create(sessions.CreateInput{SessionID: "no-cwd"}); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, deps)
	defer h.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var listed ListSessionsResult
	if err := h.client.Call(ctx, MethodSessionList, ListSessionsParams{}, &listed); err != nil {
		t.Fatal(err)
	}
	sawUsable := false
	for _, item := range listed.Sessions {
		if item.SessionID == "no-cwd" {
			t.Errorf("session/list advertised a session with no persisted workspace")
		}
		if item.SessionID == usable.SessionID {
			sawUsable = true
		}
	}
	if !sawUsable {
		t.Errorf("session/list dropped a usable session while filtering; got %d", len(listed.Sessions))
	}

	// The other half of the contract: resuming the omitted one really does fail,
	// which is why omitting it is right rather than merely tidy.
	err = h.client.Call(ctx, MethodSessionResume, ResumeSessionParams{SessionID: "no-cwd", Cwd: workspace, McpServers: []McpServer{}}, &ResumeSessionResult{})
	var rpcErr *rpcError
	if !errors.As(err, &rpcErr) || !strings.Contains(rpcErr.Message, "persisted workspace") {
		t.Errorf("resume of a workspace-less session = %v, want a persisted-workspace error", err)
	}
}

// EVERY LISTED SESSION IS ONE THE CLIENT CAN ACTUALLY TAKE.
//
// Resolving the persisted workspace only when a cwd filter was supplied left two
// shapes on the menu that resume then refuses: a session whose workspace has
// since been deleted, and a legacy entry holding a relative path — reported as
// cwd "." although ACP requires SessionInfo.cwd to be absolute.
func TestSessionListResolvesEveryWorkspace(t *testing.T) {
	deps := testDeps(t)
	deps.ResolveWorkspaceRoot = normalisingResolver(t)

	gone := t.TempDir()
	if _, err := deps.Store.Create(sessions.CreateInput{SessionID: "gone-ws", Cwd: gone}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	if _, err := deps.Store.Create(sessions.CreateInput{SessionID: "relative-ws", Cwd: "."}); err != nil {
		t.Fatal(err)
	}
	live := t.TempDir()
	if _, err := deps.Store.Create(sessions.CreateInput{SessionID: "live-ws", Cwd: live}); err != nil {
		t.Fatal(err)
	}

	h := newHarness(t, deps)
	defer h.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var listed ListSessionsResult
	if err := h.client.Call(ctx, MethodSessionList, ListSessionsParams{}, &listed); err != nil {
		t.Fatal(err)
	}

	seen := map[string]string{}
	for _, item := range listed.Sessions {
		seen[item.SessionID] = item.Cwd
	}
	if _, listedGone := seen["gone-ws"]; listedGone {
		t.Error("a session whose workspace no longer exists was advertised; resume would refuse it")
	}
	if _, listedLive := seen["live-ws"]; !listedLive {
		t.Error("a usable session was dropped while filtering unusable ones")
	}
	// THE RELATIVE ENTRY IS DROPPED. An earlier revision of this test asserted the
	// opposite — that "." be normalised and kept — which reads as the generous
	// choice but resolves the stored path against whatever directory this ACP
	// server was started in. That does not recover the session's workspace; it
	// invents one, and then advertises the invention as fact, so a conversation
	// created for one project becomes resumable against another project's files,
	// configuration and tools. The original base is not knowable from the
	// metadata, so no entry is the only honest answer.
	if invented, listedRelative := seen["relative-ws"]; listedRelative {
		t.Errorf("a relative persisted workspace was listed as %q, rebased onto the current process directory", invented)
	}
	// EVERY reported cwd is absolute, which is the contract clients rely on.
	for id, cwd := range seen {
		if !filepath.IsAbs(cwd) {
			t.Errorf("session %s was listed with a relative cwd %q; ACP requires an absolute path", id, cwd)
		}
	}
}

func TestResumeRefusesARelativePersistedWorkspace(t *testing.T) {
	// OMITTING THE ENTRY FROM THE LISTING IS NOT THE WHOLE FIX. A client can ask
	// to resume any id it already knows, and a stored "." still resolves against
	// this process's directory — so a request naming that directory would be told
	// it matched, and the conversation bound to a workspace it never belonged to.
	// Both doors need the same lock.
	deps := testDeps(t)
	deps.ResolveWorkspaceRoot = normalisingResolver(t)
	if _, err := deps.Store.Create(sessions.CreateInput{SessionID: "relative-ws", Cwd: "."}); err != nil {
		t.Fatal(err)
	}

	h := newHarness(t, deps)
	defer h.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The directory the stored "." rebases onto. Naming it explicitly is what
	// makes this test reach the persisted-cwd guard: a request with no cwd is now
	// refused earlier, for a different reason, and would pass either way.
	here, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	// BOTH ACTIVATING METHODS, not just one. session/load and session/resume are
	// separate entry points that today share activatePersistedSession — so a test
	// through either passes while the guard holds. Naming both here is what keeps
	// that true: if resume is ever given its own path, this fails rather than
	// silently covering half the surface it claims to.
	elsewhere := t.TempDir()
	var out LoadSessionResult
	for _, method := range []string{MethodSessionLoad, MethodSessionResume} {
		// The invented root offered back as the answer: without the guard the two
		// sides agree, because both were produced by resolving "." here.
		err := h.client.Call(ctx, method, LoadSessionParams{SessionID: "relative-ws", Cwd: here}, &out)
		var rpcErr *rpcError
		if !errors.As(err, &rpcErr) || !strings.Contains(rpcErr.Message, "not an absolute path") {
			t.Errorf("%s of a relative persisted workspace into the ACP process directory %q = %v, want a persisted-workspace error", method, here, err)
		}
		// And an unrelated workspace must not be accepted as its home either.
		if err := h.client.Call(ctx, method, ResumeSessionParams{SessionID: "relative-ws", Cwd: elsewhere}, &out); err == nil {
			t.Errorf("%s of a relative persisted workspace into %q succeeded", method, elsewhere)
		}
	}
}

// processDirectoryResolver mirrors internal/cli/exec.go resolveWorkspaceRoot,
// the resolver the real ACP surface is wired with: a blank cwd becomes the
// directory the process was started in, a relative one is joined onto it. base
// stands in for that directory so the test never depends on where `go test`
// happens to run.
func processDirectoryResolver(t *testing.T, base string) func(string) (string, error) {
	t.Helper()
	return func(cwd string) (string, error) {
		resolved := strings.TrimSpace(cwd)
		if resolved == "" {
			resolved = base
		} else if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(base, resolved)
		}
		resolved = filepath.Clean(resolved)
		info, err := os.Stat(resolved)
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			return "", errors.New("workspace is not a directory")
		}
		return resolved, nil
	}
}

// A REQUEST THAT NAMES NO WORKSPACE ACTIVATES NOTHING.
//
// ResumeSessionParams is an alias for LoadSessionParams, and cwd is a plain
// string on both, so JSON decoding cannot tell an omitted field from an empty
// one. The blank case used to be answered with the persisted cwd, which made
// {"sessionId":"known"} — a request carrying no workspace at all — a complete
// and successful activation. A relative cwd was worse than useless rather than
// rejected: it was joined onto whatever directory the editor spawned `zero acp`
// in, so it could agree with the persisted root by accident and hand the session
// over anyway.
//
// The cases go over the wire as raw JSON on purpose. Marshalling a Go struct
// always emits "cwd":"" and would never exercise the absent field, which is
// where the defect lives.
func TestActivationRequiresAnAbsoluteRequestWorkspace(t *testing.T) {
	deps := testDeps(t)
	processDir := t.TempDir()
	workspace := filepath.Join(processDir, "project")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	deps.ResolveWorkspaceRoot = processDirectoryResolver(t, processDir)
	if _, err := deps.Store.Create(sessions.CreateInput{SessionID: "persisted", Cwd: workspace}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		params  json.RawMessage
		message string
	}{
		// The finding: cwd absent from the wire entirely.
		{"omitted", json.RawMessage(`{"sessionId":"persisted","mcpServers":[]}`), "cwd is required and must be an absolute path"},
		{"empty", json.RawMessage(`{"sessionId":"persisted","cwd":"","mcpServers":[]}`), "cwd is required and must be an absolute path"},
		{"whitespace", json.RawMessage(`{"sessionId":"persisted","cwd":"   ","mcpServers":[]}`), "cwd is required and must be an absolute path"},
		// "project" resolves onto processDir and MATCHES the persisted root, so
		// this is the relative case that used to be accepted, not merely mis-erroring.
		{"relative", json.RawMessage(`{"sessionId":"persisted","cwd":"project","mcpServers":[]}`), "cwd must be an absolute path: project"},
		{"dot relative", json.RawMessage(`{"sessionId":"persisted","cwd":"./project","mcpServers":[]}`), "cwd must be an absolute path: ./project"},
	}

	// BOTH ACTIVATING METHODS. They share activatePersistedSession today; naming
	// both keeps this honest if resume is ever given its own path.
	for _, method := range []string{MethodSessionLoad, MethodSessionResume} {
		for _, tc := range cases {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				h := newHarness(t, deps)
				defer h.stop()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()

				var out LoadSessionResult
				err := h.client.Call(ctx, method, tc.params, &out)
				var rpcErr *rpcError
				if !errors.As(err, &rpcErr) {
					t.Fatalf("%s %s = %v, want a JSON-RPC error", method, tc.params, err)
				}
				if rpcErr.Code != codeInvalidParams {
					t.Errorf("%s %s error code = %d, want %d", method, tc.params, rpcErr.Code, codeInvalidParams)
				}
				if rpcErr.Message != tc.message {
					t.Errorf("%s %s error = %q, want %q", method, tc.params, rpcErr.Message, tc.message)
				}
				// AND THE SESSION IS NOT LIVE. The error alone would not prove it:
				// activation publishes the session before it returns, so a refusal
				// that still registered would leave session/prompt and session/set_mode
				// working on a workspace the client never named.
				modeErr := h.client.Call(ctx, MethodSessionSetMode,
					SetSessionModeParams{SessionID: "persisted", ModeID: string(agent.PermissionModeAuto)},
					&SetSessionModeResult{})
				if !errors.As(modeErr, &rpcErr) || rpcErr.Message != "unknown session: persisted" {
					t.Errorf("after a refused %s the session was live: set_mode = %v, want %q", method, modeErr, "unknown session: persisted")
				}
			})
		}
	}

	// The same request with the workspace spelled out is what the client is
	// expected to send, and it still works.
	h := newHarness(t, deps)
	defer h.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out LoadSessionResult
	if err := h.client.Call(ctx, MethodSessionResume, ResumeSessionParams{SessionID: "persisted", Cwd: workspace, McpServers: []McpServer{}}, &out); err != nil {
		t.Fatalf("session/resume with an absolute cwd: %v", err)
	}
}

// session/new roots a BRAND NEW session, and its sandbox and file-tool
// confinement, at the client's cwd. The same absent-field decoding applied
// there: {"mcpServers":[]} created a session rooted at the ACP process's own
// directory and persisted that invented path as the session's workspace.
func TestSessionNewRequiresAnAbsoluteWorkspace(t *testing.T) {
	deps := testDeps(t)
	processDir := t.TempDir()
	deps.ResolveWorkspaceRoot = processDirectoryResolver(t, processDir)
	if err := os.MkdirAll(filepath.Join(processDir, "project"), 0o755); err != nil {
		t.Fatal(err)
	}

	h := newHarness(t, deps)
	defer h.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, tc := range []struct {
		params  json.RawMessage
		message string
	}{
		{json.RawMessage(`{"mcpServers":[]}`), "cwd is required and must be an absolute path"},
		{json.RawMessage(`{"cwd":"","mcpServers":[]}`), "cwd is required and must be an absolute path"},
		{json.RawMessage(`{"cwd":"project","mcpServers":[]}`), "cwd must be an absolute path: project"},
	} {
		var created NewSessionResult
		err := h.client.Call(ctx, MethodSessionNew, tc.params, &created)
		var rpcErr *rpcError
		if !errors.As(err, &rpcErr) || rpcErr.Code != codeInvalidParams || rpcErr.Message != tc.message {
			t.Errorf("session/new %s = %v, want invalid params %q", tc.params, err, tc.message)
		}
		if created.SessionID != "" {
			t.Errorf("session/new %s created session %q despite refusing the request", tc.params, created.SessionID)
		}
	}

	listed, err := deps.Store.ListResumable()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Errorf("refused session/new requests persisted %d session(s); the first is rooted at %q", len(listed), listed[0].Cwd)
	}
}

// The cwd filter is the one ACP leaves optional, so blank still means "no
// filter" — but a relative one would be rebased onto the ACP process directory
// and answer about a workspace the client never named.
func TestSessionListRejectsARelativeCwdFilter(t *testing.T) {
	deps := testDeps(t)
	processDir := t.TempDir()
	workspace := filepath.Join(processDir, "project")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	deps.ResolveWorkspaceRoot = processDirectoryResolver(t, processDir)
	if _, err := deps.Store.Create(sessions.CreateInput{SessionID: "persisted", Cwd: workspace}); err != nil {
		t.Fatal(err)
	}

	h := newHarness(t, deps)
	defer h.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var out ListSessionsResult
	err := h.client.Call(ctx, MethodSessionList, ListSessionsParams{Cwd: "project"}, &out)
	var rpcErr *rpcError
	if !errors.As(err, &rpcErr) || rpcErr.Code != codeInvalidParams || rpcErr.Message != "cwd must be an absolute path: project" {
		t.Errorf("session/list with a relative cwd filter = %v, want invalid params %q", err, "cwd must be an absolute path: project")
	}

	// Omitting the filter is still legal, and still lists the session.
	if err := h.client.Call(ctx, MethodSessionList, ListSessionsParams{}, &out); err != nil {
		t.Fatalf("session/list without a filter: %v", err)
	}
	if len(out.Sessions) != 1 || out.Sessions[0].SessionID != "persisted" {
		t.Errorf("unfiltered session/list = %+v, want the one persisted session", out.Sessions)
	}
}

// A COMPACTED SESSION MUST RESUME AS THE CONVERSATION IT BECAME, NOT THE ONE IT
// WAS. This session's first three turns were compacted into one summary. Reading
// the raw log would replay those superseded turns and drop the summary; reading
// the rehydrated view without projecting EventCompaction would drop the summary
// and the turns both. Load and resume must agree, so both are asserted.
func TestACPCompactedHistoryReplacesCompactedTurnsWithTheirSummary(t *testing.T) {
	deps := testDeps(t)
	workspace := t.TempDir()
	created, err := deps.Store.Create(sessions.CreateInput{SessionID: "compacted-session", Title: "Compacted", Cwd: workspace})
	if err != nil {
		t.Fatal(err)
	}
	appended, err := deps.Store.AppendEvents(created.SessionID, []sessions.AppendEventInput{
		{Type: sessions.EventMessage, Payload: map[string]any{"role": "user", "content": "compacted question"}},
		{Type: sessions.EventMessage, Payload: map[string]any{"role": "assistant", "content": "compacted answer"}},
		{Type: sessions.EventMessage, Payload: map[string]any{"role": "user", "content": "surviving question"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(appended) != 3 {
		t.Fatalf("appended %d events, want 3", len(appended))
	}
	const summary = "Earlier: the user asked about scope roots and got an answer."
	if _, err := deps.Store.AppendEvents(created.SessionID, []sessions.AppendEventInput{{
		Type: sessions.EventCompaction,
		Payload: map[string]any{
			"summary":      summary,
			"preserveLast": 1,
			"compactableEvents": []map[string]any{
				{"id": appended[0].ID, "sequence": appended[0].Sequence},
				{"id": appended[1].ID, "sequence": appended[1].Sequence},
			},
		},
	}}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	loader := newHarness(t, deps)
	if err := loader.client.Call(ctx, MethodSessionLoad, LoadSessionParams{SessionID: created.SessionID, Cwd: workspace, McpServers: []McpServer{}}, &LoadSessionResult{}); err != nil {
		t.Fatalf("session/load: %v", err)
	}
	wantKinds := []string{UpdateAgentMessageChunk, UpdateUserMessageChunk}
	wantText := []string{summary, "surviving question"}
	for i := range wantKinds {
		select {
		case update := <-loader.notifications:
			if update.SessionUpdate != wantKinds[i] || update.Content.Text != wantText[i] {
				t.Fatalf("replayed update %d = %+v, want %s %q", i, update, wantKinds[i], wantText[i])
			}
		case <-ctx.Done():
			t.Fatalf("replayed update %d never arrived", i)
		}
	}
	// The compacted-away turns must NOT also be replayed after the summary.
	select {
	case update := <-loader.notifications:
		t.Fatalf("a compacted-away turn was replayed: %+v", update)
	case <-time.After(150 * time.Millisecond):
	}
	loader.stop()

	// Resume takes the same history. Its prompt context is what proves it: the
	// summary must be in there and the compacted originals must not, so the
	// prompt the agent loop actually receives is captured rather than inferred.
	prompts := make(chan string, 4)
	realRun := deps.RunAgent
	deps.RunAgent = func(ctx context.Context, prompt string, provider zeroruntime.Provider, opts agent.Options) (agent.Result, error) {
		prompts <- prompt
		return realRun(ctx, prompt, provider, opts)
	}
	resumer := newHarness(t, deps)
	defer resumer.stop()
	if err := resumer.client.Call(ctx, MethodSessionResume, ResumeSessionParams{SessionID: created.SessionID, Cwd: workspace, McpServers: []McpServer{}}, &ResumeSessionResult{}); err != nil {
		t.Fatalf("session/resume: %v", err)
	}
	if err := resumer.client.Call(ctx, MethodSessionPrompt, PromptParams{
		SessionID: created.SessionID,
		Prompt:    []ContentBlock{{Type: "text", Text: "carry on"}},
	}, &PromptResult{}); err != nil {
		t.Fatalf("session/prompt: %v", err)
	}
	prompt := <-prompts
	if !strings.Contains(prompt, summary) {
		t.Fatalf("resumed prompt lost the compaction summary:\n%s", prompt)
	}
	if strings.Contains(prompt, "compacted answer") {
		t.Fatalf("resumed prompt replayed a compacted-away turn:\n%s", prompt)
	}
}

// Tool activity is part of the transcript a client redraws on session/load, and
// a result is only renderable if it pairs with its call by the SAME id. Resume
// stays replay-free.
func TestACPLoadReplaysToolCallsPairedByTheirStoredOccurrence(t *testing.T) {
	deps := testDeps(t)
	workspace := t.TempDir()
	created, err := deps.Store.Create(sessions.CreateInput{SessionID: "tool-replay-session", Title: "Tools", Cwd: workspace})
	if err != nil {
		t.Fatal(err)
	}
	appended, err := deps.Store.AppendEvents(created.SessionID, []sessions.AppendEventInput{
		{Type: sessions.EventMessage, Payload: map[string]any{"role": "user", "content": "read the file"}},
		{Type: sessions.EventToolCall, Payload: map[string]any{"name": "read_file", "toolCallId": "call-77", "arguments": `{"path":"scope.go"}`}},
		{Type: sessions.EventToolResult, Payload: map[string]any{"name": "read_file", "toolCallId": "call-77", "status": "ok", "output": "package sandbox", "changedFiles": []string{"scope.go", "scope_test.go"}}},
		// An older record spells the id "id" rather than "toolCallId".
		{Type: sessions.EventToolCall, Payload: map[string]any{"name": "bash", "id": "call-88", "arguments": `{"command":"go build"}`}},
		{Type: sessions.EventToolResult, Payload: map[string]any{"name": "bash", "id": "call-88", "status": "error", "output": "build failed"}},
		// A start with no result is an interrupted call, not a completed one.
		{Type: sessions.EventToolCall, Payload: map[string]any{"name": "grep", "toolCallId": "call-99", "arguments": `{"pattern":"TODO"}`}},
		// No id at all: unpairable, so it must be skipped rather than replayed.
		{Type: sessions.EventToolCall, Payload: map[string]any{"name": "orphan", "arguments": "{}"}},
		// An identified result is still unpairable when its start is absent.
		{Type: sessions.EventToolResult, Payload: map[string]any{"name": "orphan", "toolCallId": "missing-call", "status": "ok", "output": "must not replay"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	loader := newHarness(t, deps)
	if err := loader.client.Call(ctx, MethodSessionLoad, LoadSessionParams{SessionID: created.SessionID, Cwd: workspace, McpServers: []McpServer{}}, &LoadSessionResult{}); err != nil {
		t.Fatalf("session/load: %v", err)
	}
	type want struct {
		kind, id, status string
		locations        []string
	}
	wants := []want{
		{UpdateToolCall, replayToolCallID(appended[1].ID), ToolStatusInProgress, nil},
		{UpdateToolCallUpdate, replayToolCallID(appended[1].ID), ToolStatusCompleted, []string{"scope.go", "scope_test.go"}},
		{UpdateToolCall, replayToolCallID(appended[3].ID), ToolStatusInProgress, nil},
		{UpdateToolCallUpdate, replayToolCallID(appended[3].ID), ToolStatusFailed, nil},
		{UpdateToolCall, replayToolCallID(appended[5].ID), ToolStatusInProgress, nil},
	}
	for i, w := range wants {
		select {
		case update := <-loader.tools:
			if update.SessionUpdate != w.kind || update.ToolCallID != w.id || update.Status != w.status {
				t.Fatalf("tool update %d = %+v, want %s/%s/%s", i, update, w.kind, w.id, w.status)
			}
			if len(update.Locations) != len(w.locations) {
				t.Fatalf("tool update %d locations = %+v, want %v", i, update.Locations, w.locations)
			}
			for j, path := range w.locations {
				if update.Locations[j].Path != path {
					t.Fatalf("tool update %d location %d = %+v, want %q", i, j, update.Locations[j], path)
				}
			}
		case <-ctx.Done():
			t.Fatalf("tool update %d never arrived", i)
		}
	}
	select {
	case update := <-loader.tools:
		t.Fatalf("an unpairable tool call was replayed: %+v", update)
	case <-time.After(150 * time.Millisecond):
	}
	loader.stop()

	resumer := newHarness(t, deps)
	defer resumer.stop()
	if err := resumer.client.Call(ctx, MethodSessionResume, ResumeSessionParams{SessionID: created.SessionID, Cwd: workspace, McpServers: []McpServer{}}, &ResumeSessionResult{}); err != nil {
		t.Fatalf("session/resume: %v", err)
	}
	select {
	case update := <-resumer.tools:
		t.Fatalf("session/resume replayed tool activity: %+v", update)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestACPLoadDisambiguatesRepeatedStoredToolIDs(t *testing.T) {
	deps := testDeps(t)
	workspace := t.TempDir()
	created, err := deps.Store.Create(sessions.CreateInput{SessionID: "repeated-tool-ids", Cwd: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deps.Store.AppendEvents(created.SessionID, []sessions.AppendEventInput{
		{Type: sessions.EventToolCall, Payload: map[string]any{"name": "read_file", "toolCallId": "gemini_tool_1", "arguments": `{"path":"a.go"}`}},
		{Type: sessions.EventToolResult, Payload: map[string]any{"name": "read_file", "toolCallId": "gemini_tool_1", "status": "ok", "output": "alpha"}},
		{Type: sessions.EventToolCall, Payload: map[string]any{"name": "grep", "toolCallId": "gemini_tool_1", "arguments": `{"pattern":"TODO"}`}},
		{Type: sessions.EventToolResult, Payload: map[string]any{"name": "grep", "toolCallId": "gemini_tool_1", "status": "error", "output": "not found"}},
	}); err != nil {
		t.Fatal(err)
	}

	load := func() []ToolCallUpdate {
		t.Helper()
		h := newHarness(t, deps)
		defer h.stop()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.client.Call(ctx, MethodSessionLoad, LoadSessionParams{
			SessionID: created.SessionID, Cwd: workspace,
		}, &LoadSessionResult{}); err != nil {
			t.Fatalf("session/load: %v", err)
		}
		updates := make([]ToolCallUpdate, 0, 4)
		for len(updates) < 4 {
			select {
			case update := <-h.tools:
				updates = append(updates, update)
			case <-ctx.Done():
				t.Fatalf("received %d tool updates, want four", len(updates))
			}
		}
		return updates
	}

	first := load()
	if first[0].ToolCallID == "" || first[0].ToolCallID != first[1].ToolCallID {
		t.Fatalf("first call/result pairing = %q/%q", first[0].ToolCallID, first[1].ToolCallID)
	}
	if first[2].ToolCallID == "" || first[2].ToolCallID != first[3].ToolCallID {
		t.Fatalf("second call/result pairing = %q/%q", first[2].ToolCallID, first[3].ToolCallID)
	}
	if first[0].ToolCallID == first[2].ToolCallID {
		t.Fatalf("separate invocations reused replay identity %q", first[0].ToolCallID)
	}
	second := load()
	for index := range first {
		if second[index].ToolCallID != first[index].ToolCallID {
			t.Fatalf("load two identity %d = %q, want stable %q", index, second[index].ToolCallID, first[index].ToolCallID)
		}
	}
}

// Drive the callback-to-store boundary through session/prompt rather than
// seeding replay-shaped events by hand. A fresh agent must then reconstruct the
// same call/result pair and changed-file locations from the durable log.
func TestACPPromptPersistsToolActivityForFreshLoad(t *testing.T) {
	deps := testDeps(t)
	deps.RunAgent = func(_ context.Context, _ string, _ zeroruntime.Provider, opts agent.Options) (agent.Result, error) {
		call := agent.ToolCall{ID: "call-live", Name: "edit_file", Arguments: `{"path":"a.go"}`}
		result := agent.ToolResult{
			ToolCallID:   call.ID,
			Name:         call.Name,
			Status:       tools.StatusOK,
			Output:       "updated",
			ChangedFiles: []string{"a.go", "b.go"},
		}
		opts.OnToolCall(call)
		opts.OnToolResult(result)
		return agent.Result{FinalAnswer: "done"}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	workspace := t.TempDir()
	writer := newHarness(t, deps)
	var created NewSessionResult
	if err := writer.client.Call(ctx, MethodSessionNew, NewSessionParams{Cwd: workspace}, &created); err != nil {
		t.Fatalf("session/new: %v", err)
	}
	if err := writer.client.Call(ctx, MethodSessionPrompt, PromptParams{
		SessionID: created.SessionID,
		Prompt:    []ContentBlock{{Type: "text", Text: "edit the files"}},
	}, &PromptResult{}); err != nil {
		t.Fatalf("session/prompt: %v", err)
	}
	writer.stop()

	events, err := deps.Store.ReadEvents(created.SessionID)
	if err != nil {
		t.Fatalf("read persisted prompt events: %v", err)
	}
	wantTypes := []sessions.EventType{
		sessions.EventMessage,
		sessions.EventToolCall,
		sessions.EventToolResult,
		sessions.EventMessage,
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("persisted %d events, want %d: %+v", len(events), len(wantTypes), events)
	}
	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Fatalf("event %d type = %s, want %s", i, events[i].Type, want)
		}
	}
	rawResult, err := json.Marshal(events[2].Payload)
	if err != nil {
		t.Fatal(err)
	}
	var storedResult struct {
		ToolCallID   string   `json:"toolCallId"`
		ChangedFiles []string `json:"changedFiles"`
	}
	if err := json.Unmarshal(rawResult, &storedResult); err != nil {
		t.Fatal(err)
	}
	if storedResult.ToolCallID != "call-live" || strings.Join(storedResult.ChangedFiles, ",") != "a.go,b.go" {
		t.Fatalf("stored tool result = %+v", storedResult)
	}

	loader := newHarness(t, deps)
	defer loader.stop()
	if err := loader.client.Call(ctx, MethodSessionLoad, LoadSessionParams{SessionID: created.SessionID, Cwd: workspace}, &LoadSessionResult{}); err != nil {
		t.Fatalf("fresh session/load: %v", err)
	}
	nextReplay := func(label string) ToolCallUpdate {
		t.Helper()
		select {
		case update := <-loader.tools:
			return update
		case <-ctx.Done():
			t.Fatalf("replayed %s never arrived: %v", label, ctx.Err())
			return ToolCallUpdate{}
		}
	}
	start := nextReplay("tool start")
	result := nextReplay("tool result")
	replayID := replayToolCallID(events[1].ID)
	if start.SessionUpdate != UpdateToolCall || start.ToolCallID != replayID || start.Status != ToolStatusInProgress {
		t.Fatalf("replayed tool start = %+v", start)
	}
	if result.SessionUpdate != UpdateToolCallUpdate || result.ToolCallID != replayID || result.Status != ToolStatusCompleted {
		t.Fatalf("replayed tool result = %+v", result)
	}
	if len(result.Locations) != 2 || result.Locations[0].Path != "a.go" || result.Locations[1].Path != "b.go" {
		t.Fatalf("replayed tool locations = %+v", result.Locations)
	}
}

// A COMPLETED TURN IS ONE STORE BATCH; ITS FAILURE IS ONE FAILURE. Persistence
// used to append the buffered turn one event per call, so a failure part-way
// left a durable prefix and the rule was "never append a dependent suffix after
// a failed prerequisite". With the turn committed as a single batch there is
// no suffix to protect: nothing of the turn is durable, the turn still reports
// its real outcome, the warning is raised, and a fresh load finds no history.
func TestACPPersistenceFailureCommitsNothingOfTheTurn(t *testing.T) {
	deps := testDeps(t)
	batches := 0
	deps.PersistEvents = func(sessionID string, inputs []sessions.AppendEventInput) error {
		batches++
		return errors.New("injected append failure")
	}
	deps.RunAgent = func(_ context.Context, _ string, _ zeroruntime.Provider, opts agent.Options) (agent.Result, error) {
		call := agent.ToolCall{ID: "call-prefix", Name: "read_file", Arguments: `{"path":"a.go"}`}
		opts.OnToolCall(call)
		opts.OnToolResult(agent.ToolResult{ToolCallID: call.ID, Name: call.Name, Status: tools.StatusOK, Output: "content"})
		return agent.Result{FinalAnswer: "done"}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	workspace := t.TempDir()
	writer := newHarness(t, deps)
	var created NewSessionResult
	if err := writer.client.Call(ctx, MethodSessionNew, NewSessionParams{Cwd: workspace}, &created); err != nil {
		t.Fatal(err)
	}
	var result PromptResult
	if err := writer.client.Call(ctx, MethodSessionPrompt, PromptParams{
		SessionID: created.SessionID, Prompt: []ContentBlock{TextBlock("read it")},
	}, &result); err != nil {
		t.Fatalf("session/prompt: %v", err)
	}
	if result.StopReason != StopEndTurn {
		t.Fatalf("stopReason = %q, want %q: a persistence failure is not a failed turn", result.StopReason, StopEndTurn)
	}
	got := drainTextUntil(t, writer.updates, func(text string) bool {
		return strings.Contains(text, "Could not save session history")
	})
	if !strings.Contains(got, "Could not save session history") {
		t.Fatalf("streamed text = %q, want persistence warning", got)
	}
	writer.stop()
	if batches != 1 {
		t.Fatalf("persistence attempts = %d, want the whole turn offered once as one batch", batches)
	}
	events, err := deps.Store.ReadEvents(created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("durable events = %+v, want none: a failed batch must not leave a partial turn", events)
	}

	// A fresh instance loads the session and replays nothing: no orphan call,
	// no answer whose prerequisite never landed.
	loader := newHarness(t, deps)
	defer loader.stop()
	if err := loader.client.Call(ctx, MethodSessionLoad, LoadSessionParams{
		SessionID: created.SessionID, Cwd: workspace,
	}, &LoadSessionResult{}); err != nil {
		t.Fatalf("fresh session/load: %v", err)
	}
	select {
	case update := <-loader.tools:
		t.Fatalf("a tool update from the failed turn was replayed: %+v", update)
	case <-time.After(100 * time.Millisecond):
	}
}

// TWO INSTANCES, ONE STORE: EACH TURN'S RECORDS STAY TOGETHER. An ACP
// instance's turnMu serializes prompts within that instance only; two
// instances that loaded the same session share nothing but the store, so the
// store batch is the only thing standing between their turns. When the turn
// was committed one event per call, another writer could land between two of
// them and a fresh load paired user A with nothing and answer A with nothing.
//
// The seam here does not batch anything itself: it OBSERVES what production
// hands it — the complete buffered turn — holds both writers at the commit
// boundary until each has arrived, and then delegates to the real
// Store.AppendEvents batch under the real session lock. Reverting persistTurn
// to singleton writes fails this deterministically: the seam then sees
// one-event batches, before any interleaving has to be provoked at all.
func TestACPCompletedTurnsFromTwoInstancesStayContiguous(t *testing.T) {
	deps := testDeps(t)
	workspace := t.TempDir()
	created, err := deps.Store.Create(sessions.CreateInput{SessionID: "shared-session", Title: "Shared", Cwd: workspace})
	if err != nil {
		t.Fatal(err)
	}

	const writers = 2
	var mu sync.Mutex
	arrived := 0
	release := make(chan struct{})
	deps.PersistEvents = func(sessionID string, inputs []sessions.AppendEventInput) error {
		// The whole turn, or the contiguity guarantee is already gone.
		if len(inputs) != 4 {
			t.Errorf("persist batch carried %d events, want the complete 4-event turn", len(inputs))
		}
		mu.Lock()
		arrived++
		if arrived == writers {
			close(release)
		}
		mu.Unlock()
		<-release
		_, err := deps.Store.AppendEvents(sessionID, inputs)
		return err
	}
	deps.RunAgent = func(_ context.Context, prompt string, _ zeroruntime.Provider, opts agent.Options) (agent.Result, error) {
		who := "A"
		if strings.Contains(prompt, "prompt B") {
			who = "B"
		}
		call := agent.ToolCall{ID: "call-" + who, Name: "read_file", Arguments: `{"path":"` + who + `.go"}`}
		opts.OnToolCall(call)
		opts.OnToolResult(agent.ToolResult{ToolCallID: call.ID, Name: call.Name, Status: tools.StatusOK, Output: "content " + who})
		return agent.Result{FinalAnswer: "answer " + who}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	instanceA := newHarness(t, deps)
	defer instanceA.stop()
	instanceB := newHarness(t, deps)
	defer instanceB.stop()
	for _, h := range []*clientHarness{instanceA, instanceB} {
		if err := h.client.Call(ctx, MethodSessionLoad, LoadSessionParams{SessionID: created.SessionID, Cwd: workspace, McpServers: []McpServer{}}, &LoadSessionResult{}); err != nil {
			t.Fatalf("session/load: %v", err)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for _, turn := range []struct {
		h      *clientHarness
		prompt string
	}{{instanceA, "prompt A"}, {instanceB, "prompt B"}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- turn.h.client.Call(ctx, MethodSessionPrompt, PromptParams{
				SessionID: created.SessionID, Prompt: []ContentBlock{TextBlock(turn.prompt)},
			}, &PromptResult{})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("session/prompt: %v", err)
		}
	}

	events, err := deps.Store.ReadEvents(created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 8 {
		t.Fatalf("durable events = %d, want 8", len(events))
	}
	// Each turn is a contiguous 4-record group in stored order.
	for start := 0; start < 8; start += 4 {
		group := events[start : start+4]
		user := payloadString(group[0].Payload, "content")
		who := strings.TrimPrefix(user, "prompt ")
		if group[0].Type != sessions.EventMessage || group[1].Type != sessions.EventToolCall ||
			group[2].Type != sessions.EventToolResult || group[3].Type != sessions.EventMessage {
			t.Fatalf("group at %d has types %s %s %s %s", start, group[0].Type, group[1].Type, group[2].Type, group[3].Type)
		}
		if payloadString(group[1].Payload, "toolCallId") != "call-"+who ||
			payloadString(group[2].Payload, "toolCallId") != "call-"+who ||
			payloadString(group[3].Payload, "content") != "answer "+who {
			t.Fatalf("another writer's records landed inside turn %s: %+v", who, group)
		}
	}

	// A fresh instance restores two correctly paired turns from that log.
	deps.PersistEvents = nil
	fresh := newHarness(t, deps)
	defer fresh.stop()
	if err := fresh.client.Call(ctx, MethodSessionLoad, LoadSessionParams{SessionID: created.SessionID, Cwd: workspace, McpServers: []McpServer{}}, &LoadSessionResult{}); err != nil {
		t.Fatalf("fresh session/load: %v", err)
	}
	prompts := make(chan string, 1)
	realRun := deps.RunAgent
	_ = realRun
	deps.RunAgent = func(ctx context.Context, prompt string, provider zeroruntime.Provider, opts agent.Options) (agent.Result, error) {
		prompts <- prompt
		return agent.Result{FinalAnswer: "ok"}, nil
	}
	seeded := newHarness(t, deps)
	defer seeded.stop()
	if err := seeded.client.Call(ctx, MethodSessionResume, ResumeSessionParams{SessionID: created.SessionID, Cwd: workspace, McpServers: []McpServer{}}, &ResumeSessionResult{}); err != nil {
		t.Fatalf("session/resume: %v", err)
	}
	if err := seeded.client.Call(ctx, MethodSessionPrompt, PromptParams{SessionID: created.SessionID, Prompt: []ContentBlock{TextBlock("carry on")}}, &PromptResult{}); err != nil {
		t.Fatalf("session/prompt: %v", err)
	}
	prompt := <-prompts
	for _, who := range []string{"A", "B"} {
		if !strings.Contains(prompt, "prompt "+who) || !strings.Contains(prompt, "answer "+who) {
			t.Fatalf("restored prompt lost turn %s:\n%s", who, prompt)
		}
	}
	if strings.Contains(prompt, "content A") || strings.Contains(prompt, "content B") {
		t.Fatalf("historical tool output entered the model prompt:\n%s", prompt)
	}
}

func TestACPCompactionFailureFallsBackToRawHistory(t *testing.T) {
	deps := testDeps(t)
	workspace := t.TempDir()
	created, err := deps.Store.Create(sessions.CreateInput{SessionID: "bad-compaction", Cwd: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deps.Store.AppendEvents(created.SessionID, []sessions.AppendEventInput{
		{Type: sessions.EventMessage, Payload: map[string]any{"role": "user", "content": "raw question"}},
		{Type: sessions.EventMessage, Payload: map[string]any{"role": "assistant", "content": "raw answer"}},
		// JSON-valid but semantically invalid: rehydration rejects the missing summary.
		{Type: sessions.EventCompaction, Payload: map[string]any{}},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	loader := newHarness(t, deps)
	if err := loader.client.Call(ctx, MethodSessionLoad, LoadSessionParams{
		SessionID: created.SessionID, Cwd: workspace,
	}, &LoadSessionResult{}); err != nil {
		t.Fatalf("session/load: %v", err)
	}
	loadedText := drainTextUntil(t, loader.updates, func(text string) bool {
		return strings.Contains(text, "raw answer") && strings.Contains(text, "Raw session history was restored")
	})
	if !strings.Contains(loadedText, "raw answer") || !strings.Contains(loadedText, "Raw session history was restored") {
		t.Fatalf("load output = %q", loadedText)
	}
	loader.stop()

	prompts := make(chan string, 1)
	realRun := deps.RunAgent
	deps.RunAgent = func(ctx context.Context, prompt string, provider zeroruntime.Provider, opts agent.Options) (agent.Result, error) {
		prompts <- prompt
		return realRun(ctx, prompt, provider, opts)
	}
	resumer := newHarness(t, deps)
	defer resumer.stop()
	if err := resumer.client.Call(ctx, MethodSessionResume, ResumeSessionParams{
		SessionID: created.SessionID, Cwd: workspace,
	}, &ResumeSessionResult{}); err != nil {
		t.Fatalf("session/resume: %v", err)
	}
	select {
	case update := <-resumer.notifications:
		t.Fatalf("session/resume emitted transcript-shaped fallback warning: %+v", update)
	case <-time.After(100 * time.Millisecond):
	}
	if err := resumer.client.Call(ctx, MethodSessionPrompt, PromptParams{
		SessionID: created.SessionID, Prompt: []ContentBlock{TextBlock("continue")},
	}, &PromptResult{}); err != nil {
		t.Fatalf("session/prompt: %v", err)
	}
	select {
	case prompt := <-prompts:
		if !strings.Contains(prompt, "raw question") || !strings.Contains(prompt, "raw answer") {
			t.Fatalf("resumed prompt lost raw fallback history:\n%s", prompt)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

// sessionCapabilities is omitempty. The positive case is asserted above; this is
// the other half — an agent that does NOT support list/resume must omit the key
// entirely rather than send a null a client could read as "supported". Raised by
// CodeRabbit.
func TestSessionCapabilitiesAreOmittedWhenUnset(t *testing.T) {
	encoded, err := json.Marshal(AgentCapabilities{LoadSession: true})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["sessionCapabilities"]; ok {
		t.Fatalf("unset sessionCapabilities was still serialized: %s", encoded)
	}
}

// A RESUME THAT CANNOT RESTORE ITS CONTEXT MUST FAIL, NOT SUCCEED QUIETLY.
// Resume's entire contract is "carry on from where this left off". If the events
// file is unreadable, the old behaviour registered the session anyway and
// returned success: the client held a live session ID under the old identity
// whose next prompt ran as a fresh conversation. Nothing on the wire said so.
//
// Load keeps the best-effort policy deliberately — it replays what it can and
// warns — so both halves are asserted here to keep the two from being
// accidentally unified again.
func TestACPResumeFailsWhenHistoryCannotBeRestored(t *testing.T) {
	deps := testDeps(t)
	cwd := t.TempDir()
	meta, err := deps.Store.Create(sessions.CreateInput{Title: "ACP session", Cwd: cwd})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	eventsPath := filepath.Join(deps.Store.RootDir, meta.SessionID, sessions.EventsFile)
	if err := os.WriteFile(eventsPath, []byte("{bad json}\n"), 0o600); err != nil {
		t.Fatalf("write corrupt events: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resumer := newHarness(t, deps)
	defer resumer.stop()
	err = resumer.client.Call(ctx, MethodSessionResume, ResumeSessionParams{SessionID: meta.SessionID, Cwd: cwd, McpServers: []McpServer{}}, &ResumeSessionResult{})
	if err == nil {
		t.Fatal("session/resume reported success despite unreadable history")
	}
	// And the session must not have been published: a prompt against it has to
	// be refused, not silently answered with no context.
	if err := resumer.client.Call(ctx, MethodSessionPrompt, PromptParams{
		SessionID: meta.SessionID,
		Prompt:    []ContentBlock{TextBlock("carry on")},
	}, &PromptResult{}); err == nil {
		t.Fatal("a session whose resume failed was still promptable")
	}

	// Load is the deliberate exception and still opens.
	loader := newHarness(t, deps)
	defer loader.stop()
	if err := loader.client.Call(ctx, MethodSessionLoad, LoadSessionParams{SessionID: meta.SessionID, Cwd: cwd, McpServers: []McpServer{}}, &LoadSessionResult{}); err != nil {
		t.Fatalf("session/load must stay best-effort, got: %v", err)
	}
}

func TestACPResumeRejectsMissingPopulatedEventLog(t *testing.T) {
	deps := testDeps(t)
	cwd := t.TempDir()
	populated, err := deps.Store.Create(sessions.CreateInput{SessionID: "missing-populated-log", Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deps.Store.AppendEvent(populated.SessionID, sessions.AppendEventInput{
		Type: sessions.EventMessage, Payload: map[string]any{"role": "user", "content": "retain me"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(deps.Store.RootDir, populated.SessionID, sessions.EventsFile)); err != nil {
		t.Fatal(err)
	}

	h := newHarness(t, deps)
	defer h.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.client.Call(ctx, MethodSessionResume, ResumeSessionParams{
		SessionID: populated.SessionID, Cwd: cwd,
	}, &ResumeSessionResult{}); err == nil {
		t.Fatal("session/resume accepted missing populated history")
	}
	if err := h.client.Call(ctx, MethodSessionPrompt, PromptParams{
		SessionID: populated.SessionID, Prompt: []ContentBlock{TextBlock("must stay closed")},
	}, &PromptResult{}); err == nil {
		t.Fatal("failed missing-history resume became promptable")
	}

	empty, err := deps.Store.Create(sessions.CreateInput{SessionID: "valid-empty-log", Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.client.Call(ctx, MethodSessionResume, ResumeSessionParams{
		SessionID: empty.SessionID, Cwd: cwd,
	}, &ResumeSessionResult{}); err != nil {
		t.Fatalf("session/resume rejected valid empty history: %v", err)
	}
}

// payloadString reads one string field from a stored event's payload, which is
// raw JSON once it has been read back from disk.
func payloadString(payload any, key string) string {
	raw, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	var decoded map[string]any
	if json.Unmarshal(raw, &decoded) != nil {
		return ""
	}
	value, _ := decoded[key].(string)
	return value
}
