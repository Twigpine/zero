package agent

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

func TestOptionsDeferThresholdFieldExists(t *testing.T) {
	options := Options{DeferThreshold: 10}
	if options.DeferThreshold != 10 {
		t.Fatalf("expected DeferThreshold 10, got %d", options.DeferThreshold)
	}
}

func TestToolResultLoadedToolsField(t *testing.T) {
	result := ToolResult{LoadedTools: []string{"Alpha", "Beta"}}
	if len(result.LoadedTools) != 2 || result.LoadedTools[0] != "Alpha" || result.LoadedTools[1] != "Beta" {
		t.Fatalf("expected LoadedTools [Alpha Beta], got %#v", result.LoadedTools)
	}
	// Default zero value is nil for an ordinary result.
	if (ToolResult{}).LoadedTools != nil {
		t.Fatalf("expected nil LoadedTools by default")
	}
}

// loadSignalTool returns Meta["load_tools"] like tool_search does, so we can
// assert executeToolCall lifts it into ToolResult.LoadedTools.
type loadSignalTool struct{ value string }

func (t loadSignalTool) Name() string        { return "load_signal" }
func (t loadSignalTool) Description() string { return "emits a load_tools signal" }
func (t loadSignalTool) Parameters() tools.Schema {
	return tools.Schema{Type: "object", AdditionalProperties: false}
}
func (t loadSignalTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectRead, Permission: tools.PermissionAllow}
}
func (t loadSignalTool) Run(_ context.Context, _ map[string]any) tools.Result {
	return tools.Result{Status: tools.StatusOK, Output: "ok"}
}
func (t loadSignalTool) RunWithOptions(_ context.Context, _ map[string]any, _ tools.RunOptions) tools.Result {
	return tools.Result{Status: tools.StatusOK, Output: "ok", Meta: map[string]string{"load_tools": t.value}}
}

func TestExecuteToolCallLiftsLoadTools(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(loadSignalTool{value: " Alpha , Beta ,, "})

	result, abortErr := executeToolCall(
		context.Background(),
		registry,
		ToolCall{ID: "c1", Name: "load_signal", Arguments: ""},
		PermissionModeAuto,
		Options{},
	)
	if abortErr != nil {
		t.Fatalf("unexpected abort error: %v", abortErr)
	}
	want := []string{"Alpha", "Beta"}
	if len(result.LoadedTools) != len(want) {
		t.Fatalf("expected LoadedTools %#v, got %#v", want, result.LoadedTools)
	}
	for i := range want {
		if result.LoadedTools[i] != want[i] {
			t.Fatalf("expected LoadedTools %#v, got %#v", want, result.LoadedTools)
		}
	}
}

func TestExecuteToolCallNoLoadToolsMetaLeavesNil(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(secretEmittingTool{output: "plain"})

	result, abortErr := executeToolCall(
		context.Background(),
		registry,
		ToolCall{ID: "c1", Name: "leak", Arguments: ""},
		PermissionModeAuto,
		Options{},
	)
	if abortErr != nil {
		t.Fatalf("unexpected abort error: %v", abortErr)
	}
	if result.LoadedTools != nil {
		t.Fatalf("expected nil LoadedTools for a tool with no load_tools meta, got %#v", result.LoadedTools)
	}
}

// fakeDeferredTool is deferred-eligible (implements Deferred() bool) like an MCP
// tool wrapper, so partitionTools counts and (when active) hides it.
type fakeDeferredTool struct {
	name string
	desc string
}

func (t fakeDeferredTool) Name() string        { return t.name }
func (t fakeDeferredTool) Description() string { return t.desc }
func (t fakeDeferredTool) Parameters() tools.Schema {
	return tools.Schema{Type: "object", AdditionalProperties: false}
}
func (t fakeDeferredTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectRead, Permission: tools.PermissionAllow}
}
func (t fakeDeferredTool) Run(_ context.Context, _ map[string]any) tools.Result {
	return tools.Result{Status: tools.StatusOK, Output: "ok"}
}
func (t fakeDeferredTool) Deferred() bool { return true }

// fakeToolSearchTool stands in for component D's tool_search (a non-deferred
// builtin) so the inactive path can assert it is dropped.
type fakeToolSearchTool struct{}

func (fakeToolSearchTool) Name() string        { return "tool_search" }
func (fakeToolSearchTool) Description() string { return "load deferred tool schemas" }
func (fakeToolSearchTool) Parameters() tools.Schema {
	return tools.Schema{Type: "object", AdditionalProperties: false}
}
func (fakeToolSearchTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectNone, Permission: tools.PermissionAllow, AdvertiseInAuto: true}
}
func (fakeToolSearchTool) Run(_ context.Context, _ map[string]any) tools.Result {
	return tools.Result{Status: tools.StatusOK, Output: "ok"}
}

func toolDefinitionByName(definitions []zeroruntime.ToolDefinition, name string) *zeroruntime.ToolDefinition {
	for i := range definitions {
		if definitions[i].Name == name {
			return &definitions[i]
		}
	}
	return nil
}

func assertNoDeferredDiscoveryMessage(t *testing.T, request zeroruntime.CompletionRequest) {
	t.Helper()
	for _, message := range request.Messages {
		if message.Role == zeroruntime.MessageRoleUser && strings.Contains(message.Content, "Deferred tools:") {
			t.Fatalf("deferred discovery must not be appended as a user message: %q", message.Content)
		}
	}
}

func TestPartitionToolsInactiveIsByteIdenticalAndDropsToolSearch(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(root, nil))
	registry.Register(fakeDeferredTool{name: "mcp__srv__a", desc: "tool a"})
	registry.Register(fakeToolSearchTool{})

	options := Options{DeferThreshold: 0} // 0 => deferral disabled => inactive path.

	// DeferThreshold 0 => deferral disabled => inactive path.
	exposed, reminder := partitionTools(registry, PermissionModeAuto, options, map[string]bool{})

	if reminder != "" {
		t.Fatalf("expected empty reminder on inactive path, got %q", reminder)
	}

	// Strong byte-identity assertion: build the reference the LEGACY way (the old
	// toolDefinitions construction — every visible/advertised tool's full schema,
	// EXCEPT tool_search, alpha-sorted by name) and require an exact DeepEqual. This
	// pins that the inactive partition produces the pre-deferral output verbatim,
	// not merely the same set of names.
	reference := legacyToolDefinitions(registry, PermissionModeAuto, options)
	if !reflect.DeepEqual(exposed, reference) {
		t.Fatalf("inactive partition not byte-identical to legacy toolDefinitions:\n got %#v\nwant %#v", exposed, reference)
	}

	// Belt-and-suspenders: tool_search is dropped, the deferred tool keeps its full
	// schema, and only the expected names are present.
	for _, def := range exposed {
		if def.Name == "tool_search" {
			t.Fatalf("tool_search must be dropped on inactive path, got %#v", exposed)
		}
	}
	wantNames := map[string]bool{"read_file": true, "mcp__srv__a": true}
	if len(exposed) != len(wantNames) {
		t.Fatalf("expected %d exposed tools, got %d: %#v", len(wantNames), len(exposed), exposed)
	}
	for _, def := range exposed {
		if !wantNames[def.Name] {
			t.Fatalf("unexpected exposed tool %q", def.Name)
		}
		if def.Name == "mcp__srv__a" && def.Parameters["type"] != "object" {
			t.Fatalf("expected full schema for deferred tool on inactive path, got %#v", def.Parameters)
		}
	}
}

// legacyToolDefinitions reconstructs the PRE-deferral tool-list builder: every
// tool that is visible (passes the operator filters) and advertised for the mode,
// EXCEPT tool_search, rendered with its full schema and alpha-sorted by name. It
// is the byte-identity reference for the inactive partition path.
func legacyToolDefinitions(registry *tools.Registry, permissionMode PermissionMode, options Options) []zeroruntime.ToolDefinition {
	definitions := make([]zeroruntime.ToolDefinition, 0)
	for _, tool := range registry.All() {
		if !ToolVisible(tool, permissionMode, options.EnabledTools, options.DisabledTools) {
			continue
		}
		if tool.Name() == tools.ToolSearchToolName {
			continue
		}
		definitions = append(definitions, zeroruntime.ToolDefinition{
			Name:        tool.Name(),
			Description: tool.Description(),
			Parameters:  schemaToRuntimeMap(tool.Parameters()),
		})
	}
	sort.Slice(definitions, func(left, right int) bool {
		return definitions[left].Name < definitions[right].Name
	})
	return definitions
}

// Below-threshold-but-eligible (count < threshold) is also inactive.
func TestPartitionToolsBelowThresholdInactive(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(fakeDeferredTool{name: "mcp__srv__a", desc: "a"})
	registry.Register(fakeDeferredTool{name: "mcp__srv__b", desc: "b"})

	exposed, reminder := partitionTools(registry, PermissionModeAuto, Options{DeferThreshold: 10}, map[string]bool{})
	if reminder != "" {
		t.Fatalf("expected empty reminder below threshold, got %q", reminder)
	}
	if len(exposed) != 2 {
		t.Fatalf("expected both deferred tools exposed below threshold, got %#v", exposed)
	}
}

// FIX 6(a): a deferred tool removed by DisabledTools must NOT count toward the
// eligible total. With N deferred registered and threshold == N, disabling one
// drops the surviving eligible count to N-1 < threshold, so the partition takes
// the INACTIVE path (empty discovery text, all VISIBLE tools exposed with full
// schemas, the disabled one filtered out entirely).
func TestPartitionToolsDisabledDeferredDropsBelowThresholdInactive(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(fakeDeferredTool{name: "mcp__srv__alpha", desc: "alpha"})
	registry.Register(fakeDeferredTool{name: "mcp__srv__beta", desc: "beta"})

	exposed, reminder := partitionTools(registry, PermissionModeAuto, Options{
		DeferThreshold: 2,
		DisabledTools:  []string{"mcp__srv__beta"},
	}, map[string]bool{})

	// Eligible drops to 1 (< threshold 2) => inactive: empty reminder.
	if reminder != "" {
		t.Fatalf("expected inactive path (empty reminder) when a disable drops eligible below threshold, got %q", reminder)
	}
	// Only the surviving visible tool is exposed, with its FULL schema; the disabled
	// tool is filtered out entirely.
	if len(exposed) != 1 || exposed[0].Name != "mcp__srv__alpha" {
		t.Fatalf("expected only mcp__srv__alpha exposed on inactive path, got %#v", exposed)
	}
	if exposed[0].Parameters["type"] != "object" {
		t.Fatalf("surviving deferred tool must keep its full schema on inactive path, got %#v", exposed[0].Parameters)
	}
}

// FIX 6(b): with deferral ACTIVE, a DisabledTools-hidden deferred tool must never
// influence discovery NOR be exposed — it is filtered out before the partition
// even considers it.
func TestPartitionToolsActiveExcludesDisabledDeferredFromDiscoveryAndExposed(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(fakeDeferredTool{name: "mcp__srv__alpha", desc: "alpha"})
	registry.Register(fakeDeferredTool{name: "mcp__srv__beta", desc: "beta"})
	registry.Register(fakeDeferredTool{name: "mcp__srv__gamma", desc: "gamma"})
	registry.Register(fakeToolSearchTool{}) // usable loader => deferral can activate

	// 3 deferred, disable beta => 2 surviving eligible, threshold 2 => active.
	exposed, discovery := partitionTools(registry, PermissionModeAuto, Options{
		DeferThreshold: 2,
		DisabledTools:  []string{"mcp__srv__beta"},
	}, map[string]bool{})

	if discovery == "" {
		t.Fatalf("expected active path with discovery text for the unloaded tools")
	}
	if strings.Contains(discovery, "mcp__srv__beta") {
		t.Fatalf("disabled deferred tool must not appear in discovery text, got %q", discovery)
	}
	for _, def := range exposed {
		if def.Name == "mcp__srv__beta" {
			t.Fatalf("disabled deferred tool must not be exposed, got %#v", exposed)
		}
	}
	search := toolDefinitionByName(exposed, tools.ToolSearchToolName)
	if search == nil {
		t.Fatalf("tool_search must be exposed on active path, got %#v", exposed)
	}
	if search.Description != discovery {
		t.Fatalf("tool_search description must carry discovery text\n got: %q\nwant: %q", search.Description, discovery)
	}
	if !strings.Contains(discovery, "mcp") {
		t.Fatalf("discovery text must list the surviving deferred source, got %q", discovery)
	}
}

func TestPartitionToolsActiveHidesUnloadedExposesLoaded(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(root, nil)) // non-deferred builtin
	registry.Register(fakeToolSearchTool{})                   // non-deferred, must stay exposed
	registry.Register(fakeDeferredTool{name: "mcp__srv__alpha", desc: "alpha tool"})
	registry.Register(fakeDeferredTool{name: "mcp__srv__beta", desc: "beta tool"})

	loaded := map[string]bool{"mcp__srv__alpha": true}

	// 2 eligible deferred tools, threshold 2 => active.
	exposed, discovery := partitionTools(registry, PermissionModeAuto, Options{DeferThreshold: 2}, loaded)

	exposedNames := map[string]bool{}
	for _, def := range exposed {
		exposedNames[def.Name] = true
	}
	if !exposedNames["read_file"] {
		t.Fatalf("expected builtin read_file exposed, got %#v", exposed)
	}
	if !exposedNames["tool_search"] {
		t.Fatalf("expected tool_search exposed on active path, got %#v", exposed)
	}
	if !exposedNames["mcp__srv__alpha"] {
		t.Fatalf("expected loaded deferred tool exposed, got %#v", exposed)
	}
	if exposedNames["mcp__srv__beta"] {
		t.Fatalf("unloaded deferred tool must be hidden from exposed, got %#v", exposed)
	}
	if discovery == "" {
		t.Fatalf("expected non-empty discovery text for the hidden tool")
	}
	search := toolDefinitionByName(exposed, tools.ToolSearchToolName)
	if search == nil {
		t.Fatalf("expected tool_search exposed on active path, got %#v", exposed)
	}
	if search.Description != discovery || !strings.Contains(search.Description, "mcp") {
		t.Fatalf("tool_search description must carry discovery source, got %q", search.Description)
	}
	if !strings.Contains(search.Description, "mcp__srv__alpha") {
		t.Fatalf("stable tool_search catalog must retain already-loaded tool names, got %q", search.Description)
	}
	if !strings.Contains(search.Description, "mcp__srv__beta") {
		t.Fatalf("tool_search description must list exact hidden tool names, got %q", search.Description)
	}
}

func TestPartitionToolsActiveNothingHiddenKeepsStableCatalog(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(fakeDeferredTool{name: "mcp__srv__alpha", desc: "alpha"})
	registry.Register(fakeDeferredTool{name: "mcp__srv__beta", desc: "beta"})
	registry.Register(fakeToolSearchTool{}) // usable loader => deferral can activate

	loaded := map[string]bool{"mcp__srv__alpha": true, "mcp__srv__beta": true}
	exposed, reminder := partitionTools(registry, PermissionModeAuto, Options{DeferThreshold: 2}, loaded)

	exposedNames := map[string]bool{}
	for _, def := range exposed {
		exposedNames[def.Name] = true
	}
	if !exposedNames["mcp__srv__alpha"] || !exposedNames["mcp__srv__beta"] {
		t.Fatalf("expected both loaded deferred tools exposed, got %#v", exposed)
	}
	if !exposedNames["tool_search"] {
		t.Fatalf("expected tool_search exposed on active path, got %#v", exposed)
	}
	if !strings.Contains(reminder, "mcp__srv__alpha") || !strings.Contains(reminder, "mcp__srv__beta") {
		t.Fatalf("stable catalog must retain all deferred names after loading, got %q", reminder)
	}
}

func TestRunLoadsDeferredToolThenAdvertisesNextTurn(t *testing.T) {
	registry := tools.NewRegistry()
	// load_signal asks the loop to load mcp__srv__alpha next turn.
	registry.Register(loadSignalTool{value: "mcp__srv__alpha"})
	registry.Register(fakeDeferredTool{name: "mcp__srv__alpha", desc: "alpha tool"})
	registry.Register(fakeDeferredTool{name: "mcp__srv__beta", desc: "beta tool"})
	// A real, usable tool_search must be registered for deferral to activate
	// (otherwise the loop falls back to eager so it never strands the loader).
	registry.Register(tools.NewToolSearchTool(registry))

	provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{
		{ // turn 1: call load_signal
			{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "c1", ToolName: "load_signal"},
			{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "c1"},
			{Type: zeroruntime.StreamEventDone},
		},
		{ // turn 2: final answer
			{Type: zeroruntime.StreamEventText, Content: "done"},
			{Type: zeroruntime.StreamEventDone},
		},
	}}

	result, err := Run(context.Background(), "go", provider, Options{
		Registry:       registry,
		DeferThreshold: 2, // 2 deferred tools => active
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("expected two turns, got %d", len(provider.requests))
	}

	// Turn 1: alpha is hidden (not advertised), and discovery lives on the
	// tool_search definition rather than a trailing user message.
	turn1Tools := map[string]bool{}
	for _, def := range provider.requests[0].Tools {
		turn1Tools[def.Name] = true
	}
	if turn1Tools["mcp__srv__alpha"] {
		t.Fatalf("turn 1 must not advertise the not-yet-loaded deferred tool")
	}
	assertNoDeferredDiscoveryMessage(t, provider.requests[0])
	search1 := toolDefinitionByName(provider.requests[0].Tools, tools.ToolSearchToolName)
	if search1 == nil {
		t.Fatalf("turn 1 must expose tool_search, got %#v", provider.requests[0].Tools)
	}
	if !strings.Contains(search1.Description, "Tool discovery") || !strings.Contains(search1.Description, "mcp") {
		t.Fatalf("tool_search description must carry deferred discovery, got %q", search1.Description)
	}

	// Turn 2: alpha is now loaded and advertised with a full schema.
	turn2Tools := map[string]bool{}
	for _, def := range provider.requests[1].Tools {
		turn2Tools[def.Name] = true
	}
	if !turn2Tools["mcp__srv__alpha"] {
		t.Fatalf("turn 2 must advertise the loaded deferred tool, got %#v", provider.requests[1].Tools)
	}
	if turn2Tools["mcp__srv__beta"] {
		t.Fatalf("beta was never loaded; it must stay hidden in turn 2")
	}

	// The discovery text must NOT persist into the returned message history.
	for _, m := range result.Messages {
		if m.Role == zeroruntime.MessageRoleUser && strings.Contains(m.Content, "Tool discovery") {
			t.Fatalf("deferred discovery must not be persisted in result.Messages")
		}
	}
}

// TestRunReactiveRetryKeepsLoadedDeferredToolAndDiscovery drives a mid-run
// context-limit error that triggers reactive compaction+retry while deferral is
// ACTIVE and a deferred tool is already loaded. The retried turn must NOT be
// degraded to the empty-loaded/no-discovery state: it must still advertise the
// loaded tool's FULL schema and keep tool_search discovery for hidden tools.
func TestRunReactiveRetryKeepsLoadedDeferredToolAndDiscovery(t *testing.T) {
	registry := tools.NewRegistry()
	// load_signal asks the loop to load mcp__srv__alpha next turn.
	registry.Register(loadSignalTool{value: "mcp__srv__alpha"})
	registry.Register(fakeDeferredTool{name: "mcp__srv__alpha", desc: "alpha tool"})
	registry.Register(fakeDeferredTool{name: "mcp__srv__beta", desc: "beta tool"})
	// A real, usable tool_search must be registered for deferral to activate
	// (otherwise the loop falls back to eager so it never strands the loader).
	registry.Register(tools.NewToolSearchTool(registry))

	// Request indices (mockProvider plays one turn per request, in order):
	//   0: turn 1 — calls load_signal (loads mcp__srv__alpha for later turns)
	//   1: turn 2 — emits a context-limit error MID-stream -> reactive recover
	//   2: the summarize call inside Compact (must return non-empty text)
	//   3: the RETRY request rebuilt after compaction — asserted below
	provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{
		{ // turn 1: call load_signal
			{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "c1", ToolName: "load_signal"},
			{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "c1"},
			{Type: zeroruntime.StreamEventDone},
		},
		{ // turn 2: mid-stream context-limit error -> reactive compaction
			{Type: zeroruntime.StreamEventError, Error: "prompt is too long: 250000 tokens > 200000 maximum"},
		},
		{ // summarize call inside Compact
			{Type: zeroruntime.StreamEventText, Content: "SUMMARY"},
			{Type: zeroruntime.StreamEventDone},
		},
		{ // retry of turn 2 after compaction: final answer
			{Type: zeroruntime.StreamEventText, Content: "done"},
			{Type: zeroruntime.StreamEventDone},
		},
	}}

	// A large user prompt so the elided middle is big enough that reactive
	// compaction actually shrinks the history (recover only retries when it can
	// shrink). ContextWindow is set high enough that PROACTIVE compaction (which
	// fires at 0.8 * window at the top of a turn) does NOT trigger first — so the
	// only summarize call is the reactive one, keeping the request sequence
	// predictable: turn1, errored turn2, summarize, retry.
	bigPrompt := strings.Repeat("work on this task. ", 2000)
	result, err := Run(context.Background(), bigPrompt, provider, Options{
		Registry:               registry,
		DeferThreshold:         2, // 2 deferred tools => active
		ContextWindow:          1_000_000,
		CompactionPreserveLast: 2,
	})
	if err != nil {
		t.Fatalf("run returned error: %v", err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("expected final answer from the retried turn, got %q", result.FinalAnswer)
	}
	// 4 provider calls: turn1, errored turn2, summarize, retry.
	if len(provider.requests) != 4 {
		t.Fatalf("expected 4 provider requests (turn1, errored turn2, summarize, retry), got %d", len(provider.requests))
	}

	retry := provider.requests[3]

	// The retry must advertise the loaded deferred tool with its FULL schema —
	// NOT re-hide it as the empty-loaded partition would.
	var alpha *zeroruntime.ToolDefinition
	for i := range retry.Tools {
		if retry.Tools[i].Name == "mcp__srv__alpha" {
			alpha = &retry.Tools[i]
		}
		if retry.Tools[i].Name == "mcp__srv__beta" {
			t.Fatalf("retry must keep the never-loaded deferred tool hidden, got %#v", retry.Tools)
		}
	}
	if alpha == nil {
		t.Fatalf("retry must still advertise the loaded deferred tool mcp__srv__alpha, got %#v", retry.Tools)
	}
	if alpha.Parameters["type"] != "object" {
		t.Fatalf("retry must advertise the loaded tool's FULL schema, got %#v", alpha.Parameters)
	}

	assertNoDeferredDiscoveryMessage(t, retry)
	search := toolDefinitionByName(retry.Tools, tools.ToolSearchToolName)
	if search == nil {
		t.Fatalf("retry must expose tool_search for still-hidden tools, got %#v", retry.Tools)
	}
	if !strings.Contains(search.Description, "Tool discovery") || !strings.Contains(search.Description, "mcp") {
		t.Fatalf("retry tool_search description must carry deferred discovery, got %q", search.Description)
	}

	// The discovery text must NOT persist into the returned message history, even on
	// the reactive-retry path.
	for _, m := range result.Messages {
		if m.Role == zeroruntime.MessageRoleUser && strings.Contains(m.Content, "Tool discovery") {
			t.Fatalf("deferred discovery must not be persisted in result.Messages")
		}
	}
}

// connectErrorProvider returns a connect-time error (StreamCompletion itself
// returns a non-nil error) on the request at index errAtRequest, exercising the
// FIRST reactive-recovery block in the loop (loop.go:99). Every other request
// streams the corresponding turn's events. It mirrors mockProvider's recording so
// the rebuilt retry request can be asserted.
type connectErrorProvider struct {
	turns        [][]zeroruntime.StreamEvent
	errAtRequest int
	errText      string
	requests     []zeroruntime.CompletionRequest
}

func (provider *connectErrorProvider) StreamCompletion(_ context.Context, request zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	provider.requests = append(provider.requests, request)
	index := len(provider.requests) - 1
	if index == provider.errAtRequest {
		return nil, errors.New(provider.errText)
	}
	events := []zeroruntime.StreamEvent{{Type: zeroruntime.StreamEventDone}}
	if index < len(provider.turns) {
		events = provider.turns[index]
	}
	ch := make(chan zeroruntime.StreamEvent, len(events))
	for _, event := range events {
		ch <- event
	}
	close(ch)
	return ch, nil
}

// TestRunConnectTimeReactiveRetryKeepsLoadedDeferredToolAndDiscovery mirrors
// TestRunReactiveRetryKeepsLoadedDeferredToolAndDiscovery but drives the
// CONNECT-TIME error path: StreamCompletion itself returns a non-nil
// context-limit error (rather than a mid-stream StreamEventError). With deferral
// ACTIVE and a deferred tool already loaded, the rebuilt retry request must still
// advertise the loaded tool's FULL schema AND carry tool_search discovery —
// i.e. the first reactive block reuses exposed, not the empty-loaded
// partition.
func TestRunConnectTimeReactiveRetryKeepsLoadedDeferredToolAndDiscovery(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(loadSignalTool{value: "mcp__srv__alpha"})
	registry.Register(fakeDeferredTool{name: "mcp__srv__alpha", desc: "alpha tool"})
	registry.Register(fakeDeferredTool{name: "mcp__srv__beta", desc: "beta tool"})
	// A real, usable tool_search must be registered for deferral to activate
	// (otherwise the loop falls back to eager so it never strands the loader).
	registry.Register(tools.NewToolSearchTool(registry))

	// Request indices:
	//   0: turn 1 — calls load_signal (loads mcp__srv__alpha for later turns)
	//   1: turn 2 — StreamCompletion returns a connect-time context-limit error
	//   2: the summarize call inside Compact (must return non-empty text)
	//   3: the RETRY request rebuilt after compaction — asserted below
	provider := &connectErrorProvider{
		errAtRequest: 1,
		errText:      "prompt is too long: 250000 tokens > 200000 maximum",
		turns: [][]zeroruntime.StreamEvent{
			{ // turn 1: call load_signal
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "c1", ToolName: "load_signal"},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "c1"},
				{Type: zeroruntime.StreamEventDone},
			},
			nil, // index 1: replaced by the connect-time error
			{ // index 2: summarize call inside Compact
				{Type: zeroruntime.StreamEventText, Content: "SUMMARY"},
				{Type: zeroruntime.StreamEventDone},
			},
			{ // index 3: retry of turn 2 after compaction: final answer
				{Type: zeroruntime.StreamEventText, Content: "done"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}

	bigPrompt := strings.Repeat("work on this task. ", 2000)
	result, err := Run(context.Background(), bigPrompt, provider, Options{
		Registry:               registry,
		DeferThreshold:         2, // 2 deferred tools => active
		ContextWindow:          1_000_000,
		CompactionPreserveLast: 2,
	})
	if err != nil {
		t.Fatalf("run returned error: %v", err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("expected final answer from the retried turn, got %q", result.FinalAnswer)
	}
	// 4 provider calls: turn1, errored-connect turn2, summarize, retry.
	if len(provider.requests) != 4 {
		t.Fatalf("expected 4 provider requests (turn1, errored turn2, summarize, retry), got %d", len(provider.requests))
	}

	retry := provider.requests[3]

	// The retry must advertise the loaded deferred tool with its FULL schema and
	// keep the never-loaded one hidden.
	var alpha *zeroruntime.ToolDefinition
	for i := range retry.Tools {
		if retry.Tools[i].Name == "mcp__srv__alpha" {
			alpha = &retry.Tools[i]
		}
		if retry.Tools[i].Name == "mcp__srv__beta" {
			t.Fatalf("retry must keep the never-loaded deferred tool hidden, got %#v", retry.Tools)
		}
	}
	if alpha == nil {
		t.Fatalf("retry must still advertise the loaded deferred tool mcp__srv__alpha, got %#v", retry.Tools)
	}
	if alpha.Parameters["type"] != "object" {
		t.Fatalf("retry must advertise the loaded tool's FULL schema, got %#v", alpha.Parameters)
	}

	assertNoDeferredDiscoveryMessage(t, retry)
	search := toolDefinitionByName(retry.Tools, tools.ToolSearchToolName)
	if search == nil {
		t.Fatalf("retry must expose tool_search for still-hidden tools, got %#v", retry.Tools)
	}
	if !strings.Contains(search.Description, "Tool discovery") || !strings.Contains(search.Description, "mcp") {
		t.Fatalf("retry tool_search description must carry deferred discovery, got %q", search.Description)
	}

	// The discovery text must NOT persist into the returned message history.
	for _, m := range result.Messages {
		if m.Role == zeroruntime.MessageRoleUser && strings.Contains(m.Content, "Tool discovery") {
			t.Fatalf("deferred discovery must not be persisted in result.Messages")
		}
	}
}

// TestAllowlistedDeferredToolsKeepToolSearchReachable is the FIX 1+2 dead-end
// regression: N deferred tools are registered alongside tool_search, the operator
// allowlists ONLY the N deferred names (NOT tool_search), and DeferThreshold == N.
// Deferral must ACTIVATE, the partition must STILL expose tool_search (the gateway
// to the allowlisted tools), and a tool_search call must be DISPATCH-ALLOWED.
func TestAllowlistedDeferredToolsKeepToolSearchReachable(t *testing.T) {
	registry := tools.NewRegistry()
	deferredNames := []string{"mcp__srv__alpha", "mcp__srv__beta"}
	for _, name := range deferredNames {
		registry.Register(fakeDeferredTool{name: name, desc: name + " tool"})
	}
	// The REAL tool_search (so partitionTools can look it up by name and dispatch
	// can run it through registry.RunWithOptions).
	registry.Register(tools.NewToolSearchTool(registry))

	options := Options{
		DeferThreshold: len(deferredNames), // threshold == N => active
		// Allowlist the deferred tools but NOT tool_search.
		EnabledTools: append([]string{}, deferredNames...),
	}

	exposed, discovery := partitionTools(registry, PermissionModeAuto, options, map[string]bool{})

	// Deferral active => non-empty tool_search discovery for hidden deferred tools.
	if discovery == "" {
		t.Fatalf("expected deferral active (non-empty discovery) at threshold N, got empty")
	}
	if !strings.Contains(discovery, "tool_search") {
		t.Fatalf("discovery must instruct the model to call tool_search, got %q", discovery)
	}

	// FIX 2(a): tool_search is exposed even though the allowlist omits it.
	exposedNames := map[string]bool{}
	for _, def := range exposed {
		exposedNames[def.Name] = true
	}
	if !exposedNames["tool_search"] {
		t.Fatalf("tool_search must be exposed on the active path despite the allowlist omitting it, got %#v", exposed)
	}
	search := toolDefinitionByName(exposed, tools.ToolSearchToolName)
	if search == nil || search.Description != discovery {
		t.Fatalf("tool_search description must carry discovery text, got %#v discovery=%q", search, discovery)
	}
	// The allowlisted-but-unloaded deferred tools are hidden (not exposed).
	for _, name := range deferredNames {
		if exposedNames[name] {
			t.Fatalf("unloaded deferred tool %q must be hidden when active, got %#v", name, exposed)
		}
	}

	// FIX 2(b): a tool_search call must be DISPATCH-ALLOWED (not rejected by the
	// allowlist that omits it). It runs through the registry and reports a load.
	result, abortErr := executeToolCall(
		context.Background(),
		registry,
		ToolCall{ID: "c1", Name: "tool_search", Arguments: `{"query":"select:mcp__srv__alpha"}`},
		PermissionModeAuto,
		options,
	)
	if abortErr != nil {
		t.Fatalf("unexpected abort error: %v", abortErr)
	}
	if result.Status != tools.StatusOK {
		t.Fatalf("tool_search call was rejected: status=%s output=%q", result.Status, result.Output)
	}
	if result.DenialReason == DenialFiltered {
		t.Fatalf("tool_search must not be denied by the allowlist, got DenialFiltered: %q", result.Output)
	}
	if len(result.LoadedTools) != 1 || result.LoadedTools[0] != "mcp__srv__alpha" {
		t.Fatalf("tool_search call must load mcp__srv__alpha, got LoadedTools=%#v", result.LoadedTools)
	}
}

// TestDisabledToolSearchStaysHiddenAndRejectedWhenActive verifies an explicit
// DisabledTools entry for tool_search is STILL honored on the active path: the
// loader is not exposed and a call to it is rejected (FIX 2 exempts the allowlist
// only, never the denylist).
func TestDisabledToolSearchFallsBackToEager(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(fakeDeferredTool{name: "mcp__srv__alpha", desc: "alpha"})
	registry.Register(fakeDeferredTool{name: "mcp__srv__beta", desc: "beta"})
	registry.Register(tools.NewToolSearchTool(registry))

	options := Options{
		DeferThreshold: 2,
		DisabledTools:  []string{"tool_search"},
	}

	// tool_search is explicitly disabled, so it can never run. Deferral must NOT
	// activate — otherwise the loop would hide the deferred tools behind a loader
	// the dispatch gate rejects (an inescapable dead-end). Expect the eager /
	// inactive fallback: every deferred tool exposed with its full schema, no
	// tool_search definition, and an empty reminder.
	exposed, reminder := partitionTools(registry, PermissionModeAuto, options, map[string]bool{})
	if reminder != "" {
		t.Fatalf("inactive fallback must emit no reminder, got %q", reminder)
	}
	exposedNames := make(map[string]bool, len(exposed))
	for _, def := range exposed {
		exposedNames[def.Name] = true
	}
	if exposedNames["tool_search"] {
		t.Fatalf("tool_search must not be advertised when disabled, got %#v", exposed)
	}
	if !exposedNames["mcp__srv__alpha"] || !exposedNames["mcp__srv__beta"] {
		t.Fatalf("deferred tools must be exposed eagerly when deferral is inactive, got %#v", exposed)
	}

	// The deferred tools themselves stay directly callable (not stranded behind a
	// disabled loader).
	result, abortErr := executeToolCall(
		context.Background(),
		registry,
		ToolCall{ID: "c1", Name: "mcp__srv__alpha", Arguments: `{}`},
		PermissionModeAuto,
		options,
	)
	if abortErr != nil {
		t.Fatalf("unexpected abort error: %v", abortErr)
	}
	if result.Status != tools.StatusOK {
		t.Fatalf("deferred tool must be callable under eager fallback, got status=%s output=%q", result.Status, result.Output)
	}
}

// fakeDeferredMutatorTool is a deferred-eligible tool with mutating Safety
// (SideEffectWrite), standing in for a real write/mutator MCP tool that would
// be hidden behind tool_search once deferral activates.
type fakeDeferredMutatorTool struct{ name string }

func (t fakeDeferredMutatorTool) Name() string        { return t.name }
func (t fakeDeferredMutatorTool) Description() string { return "mutates the workspace, deferred" }
func (t fakeDeferredMutatorTool) Parameters() tools.Schema {
	return tools.Schema{Type: "object", AdditionalProperties: false}
}
func (t fakeDeferredMutatorTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectWrite, Permission: tools.PermissionPrompt, Reason: "mutates"}
}
func (t fakeDeferredMutatorTool) Run(_ context.Context, _ map[string]any) tools.Result {
	return tools.Result{Status: tools.StatusOK, Output: "mutated"}
}
func (t fakeDeferredMutatorTool) Deferred() bool { return true }

// TestPlanModeToolSearchNeverLeaksDeferredMutatorSchema guards the concern
// raised in PR #642 review (jatmn, P2): that tool_search filters deferred
// candidates only by EnabledTools/DisabledTools, not by plan-mode visibility,
// so a plan-mode model could call `tool_search select:<deferred write tool>`
// and receive that tool's full schema even though a direct call to it is
// correctly denied the following turn.
//
// tool_search's own Safety is SideEffectNone, and ToolAdvertisedForPermissionMode (the
// same gate executeToolCall uses to deny a direct call) requires
// SideEffect==Read to advertise a tool in plan mode. That means tool_search
// itself is never advertised, never activates deferral (loaderUsable in
// partitionToolsCached requires ToolAdvertised(loader, permissionMode)), and
// is denied at dispatch like any other hidden tool if a stale/adversarial
// call reaches it anyway — so a deferred mutator's schema can never reach the
// model through tool_search while in plan mode. This test pins that
// end-to-end: tool_search is absent from the advertised tool list, and a
// forced call to it is denied before rendering any tool schema.
func TestPlanModeToolSearchNeverLeaksDeferredMutatorSchema(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewReadFileTool(root))
	registry.Register(fakeDeferredMutatorTool{name: "mcp__srv__mutate"})
	registry.Register(fakeDeferredMutatorTool{name: "mcp__srv__mutate2"})
	registry.Register(tools.NewToolSearchTool(registry))

	provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{
		{ // turn 1: force a call to tool_search even though it should not be advertised.
			{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "c1", ToolName: tools.ToolSearchToolName},
			{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "c1", ArgumentsFragment: `{"query":"select:mcp__srv__mutate"}`},
			{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "c1"},
			{Type: zeroruntime.StreamEventDone},
		},
		{ // turn 2: final answer.
			{Type: zeroruntime.StreamEventText, Content: "done"},
			{Type: zeroruntime.StreamEventDone},
		},
	}}

	result, err := Run(context.Background(), "plan", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModePlan,
		DeferThreshold: 2, // 2 deferred mutators registered => eligible for deferral.
		MaxTurns:       2,
	})
	if err != nil {
		t.Fatal(err)
	}

	// tool_search (and the deferred mutators) must not be advertised in plan
	// mode's turn 1 tool list at all.
	for _, def := range provider.requests[0].Tools {
		if def.Name == tools.ToolSearchToolName {
			t.Fatalf("plan mode must not advertise tool_search, got %#v", provider.requests[0].Tools)
		}
		if def.Name == "mcp__srv__mutate" || def.Name == "mcp__srv__mutate2" {
			t.Fatalf("plan mode must not advertise a deferred mutator, got %#v", provider.requests[0].Tools)
		}
	}

	// The forced call must be denied outright, never a loaded-schema result:
	// the tool result must not mention the mutator's name or carry a
	// load_tools signal.
	var toolMessage string
	for _, message := range result.Messages {
		if message.Role == zeroruntime.MessageRoleTool {
			toolMessage = message.Content
			break
		}
	}
	if !strings.Contains(toolMessage, "not available in plan mode") {
		t.Fatalf("expected tool_search call denied in plan mode, got %q", toolMessage)
	}
	if strings.Contains(toolMessage, "mcp__srv__mutate") {
		t.Fatalf("denial must not leak the deferred mutator's name/schema, got %q", toolMessage)
	}
}
