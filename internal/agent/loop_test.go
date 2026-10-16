package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/execution"
	"github.com/Gitlawb/zero/internal/hooks"
	"github.com/Gitlawb/zero/internal/sandbox"
	"github.com/Gitlawb/zero/internal/specmode"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/trace"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

type mockProvider struct {
	turns    [][]zeroruntime.StreamEvent
	requests []zeroruntime.CompletionRequest
}

func TestPrePermissionRejectScrubsFileDiffs(t *testing.T) {
	secret := "sk-proj-abcdefghijklmnopqrstuvwxyz"
	result := toolResultFromPrePermissionReject(ToolCall{ID: "call", Name: "test"}, tools.Result{
		Status: tools.StatusError,
		FileDiffs: []tools.FileDiff{{
			Path:      filepath.Join(t.TempDir(), "secret.txt"),
			OldExists: true,
			NewExists: true,
			OldText:   "token=" + secret,
			NewText:   "safe",
		}},
	})
	if len(result.FileDiffs) != 1 || strings.Contains(result.FileDiffs[0].OldText, secret) || !result.Redacted {
		t.Fatalf("pre-permission FileDiff = %#v, redacted = %t", result.FileDiffs, result.Redacted)
	}
}

func TestTypedExecutionOutcomeOverridesLegacySandboxHeuristics(t *testing.T) {
	engine := sandbox.NewEngine(sandbox.EngineOptions{WorkspaceRoot: t.TempDir(), Policy: sandbox.DefaultPolicy()})
	call := ToolCall{Name: tools.ExecCommandToolName}
	options := Options{Sandbox: engine}
	legacyMeta := map[string]string{
		tools.SandboxLikelyDeniedMeta: "true",
		tools.SandboxDenialKindMeta:   tools.SandboxDenialKindNetwork,
	}

	applicationFailure := tools.Result{
		Status: tools.StatusError,
		Meta:   legacyMeta,
		ExecutionOutcome: &execution.Outcome{
			State: execution.StateFailed,
			Kind:  execution.OutcomeApplicationFailure,
			Exit:  &execution.Exit{Code: 1},
		},
	}
	if sandboxDeniedNetworkRetryCandidate(call, nil, applicationFailure, options) || sandboxRestrictedShellRetryCandidate(call, nil, applicationFailure, options) {
		t.Fatal("typed application failure must not be retried from legacy sandbox-like text")
	}

	protectedDenial := applicationFailure
	protectedDenial.ExecutionOutcome = &execution.Outcome{
		State: execution.StateDenied,
		Kind:  execution.OutcomeEnforcementDenied,
		Denial: &execution.Denial{
			Capability: execution.Capability{Kind: execution.CapabilityProtectedMetadata, Scope: "/workspace/.zero"},
			Source:     execution.DenialSourceConfiguredPolicy,
			Reason:     "protected metadata",
			NextAction: execution.DenialNextActionRequestApproval,
		},
	}
	if sandboxRestrictedShellRetryCandidate(call, nil, protectedDenial, options) {
		t.Fatal("narrow protected-metadata denial must not become an unrestricted retry")
	}

	nonRecoverableDenial := protectedDenial
	nonRecoverableDenial.ExecutionOutcome = &execution.Outcome{
		State: execution.StateDenied,
		Kind:  execution.OutcomeEnforcementDenied,
		Denial: &execution.Denial{
			Capability:  execution.Capability{Kind: execution.CapabilityUnrestricted, Scope: "host"},
			Source:      execution.DenialSourcePlatformSandbox,
			Reason:      "sandbox enforcement failed closed",
			Recoverable: false,
			NextAction:  execution.DenialNextActionRequestApproval,
		},
	}
	if sandboxRestrictedShellRetryCandidate(call, nil, nonRecoverableDenial, options) {
		t.Fatal("non-recoverable sandbox denial must not become an unrestricted retry")
	}

	networkDenial := protectedDenial
	networkDenial.ExecutionOutcome = &execution.Outcome{
		State: execution.StateDenied,
		Kind:  execution.OutcomeEnforcementDenied,
		Denial: &execution.Denial{
			Capability: execution.Capability{Kind: execution.CapabilityExternalNetwork},
			Source:     execution.DenialSourcePlatformSandbox,
			Reason:     "external network denied",
			NextAction: execution.DenialNextActionRequestApproval,
		},
	}
	if !sandboxDeniedNetworkRetryCandidate(call, nil, networkDenial, options) {
		t.Fatal("typed external-network denial should use the narrow network approval path")
	}
}

func (provider *mockProvider) StreamCompletion(ctx context.Context, request zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	provider.requests = append(provider.requests, request)

	events := []zeroruntime.StreamEvent{{Type: zeroruntime.StreamEventDone}}
	if len(provider.turns) >= len(provider.requests) {
		events = provider.turns[len(provider.requests)-1]
	}

	ch := make(chan zeroruntime.StreamEvent, len(events))
	for _, event := range events {
		ch <- event
	}
	close(ch)
	return ch, nil
}

func TestRunDispatchesSessionLifecycleHooks(t *testing.T) {
	audit, err := hooks.NewAuditStore(hooks.AuditStoreOptions{AuditPath: filepath.Join(t.TempDir(), "audit.jsonl")})
	if err != nil {
		t.Fatalf("NewAuditStore: %v", err)
	}
	dispatcher := hooks.NewDispatcher(hooks.DispatcherOptions{
		Config: hooks.Config{
			Enabled: true,
			Hooks: []hooks.Definition{
				{ID: "zero.session-start", Event: hooks.EventSessionStart, Command: "zero-missing-hook-command", Enabled: true},
				{ID: "zero.session-end", Event: hooks.EventSessionEnd, Command: "zero-missing-hook-command", Enabled: true},
			},
		},
		Audit: audit,
	})
	provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{{
		{Type: zeroruntime.StreamEventText, Content: "done"},
		{Type: zeroruntime.StreamEventDone},
	}}}

	_, err = Run(context.Background(), "hi", provider, Options{
		SessionID:    "session-123",
		Cwd:          t.TempDir(),
		ProviderName: "test-provider",
		Model:        "test-model",
		Hooks:        dispatcher,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	events, err := audit.ReadEvents()
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	started := map[hooks.Event]int{}
	completed := map[hooks.Event]int{}
	for _, event := range events {
		switch event.Type {
		case "hook_execution_started":
			started[event.Event]++
		case "hook_execution_completed":
			completed[event.Event]++
		}
	}
	if started[hooks.EventSessionStart] != 1 || completed[hooks.EventSessionStart] != 1 {
		t.Fatalf("sessionStart audit counts started/completed = %d/%d, events=%#v", started[hooks.EventSessionStart], completed[hooks.EventSessionStart], events)
	}
	if started[hooks.EventSessionEnd] != 1 || completed[hooks.EventSessionEnd] != 1 {
		t.Fatalf("sessionEnd audit counts started/completed = %d/%d, events=%#v", started[hooks.EventSessionEnd], completed[hooks.EventSessionEnd], events)
	}
}

// cancelingProvider simulates an Esc/Ctrl+C abort: it cancels the run's own
// context mid-stream (as the TUI's interrupt handler would) and returns no
// events, so Run observes ctx.Err() and returns early with an already-canceled
// context in scope for the deferred sessionEnd dispatch.
type cancelingProvider struct {
	cancel context.CancelFunc
}

func (provider *cancelingProvider) StreamCompletion(ctx context.Context, request zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	provider.cancel()
	ch := make(chan zeroruntime.StreamEvent)
	close(ch)
	return ch, nil
}

// TestRunDispatchesSessionEndHookAfterContextCancellation is a regression test
// for a bug where an interrupted run (Esc/Ctrl+C) canceled ctx before the
// deferred sessionEnd dispatch ran, so Hooks.Dispatch's context.WithTimeout(ctx, ...)
// derived an already-canceled context and the hook command never actually launched.
func TestRunDispatchesSessionEndHookAfterContextCancellation(t *testing.T) {
	goBinary, err := exec.LookPath("go")
	if err != nil {
		// The test binary runs on the build machine, so its build-time GOROOT
		// still identifies the toolchain that produced it.
		goRoot := runtime.GOROOT() //nolint:staticcheck // Safe for this non-portable test binary.
		goBinary = filepath.Join(goRoot, "bin", "go")
		if runtime.GOOS == "windows" {
			goBinary += ".exe"
		}
		if _, statErr := os.Stat(goBinary); statErr != nil {
			t.Skipf("go binary unavailable on PATH or in GOROOT: %v", statErr)
		}
	}
	audit, err := hooks.NewAuditStore(hooks.AuditStoreOptions{AuditPath: filepath.Join(t.TempDir(), "audit.jsonl")})
	if err != nil {
		t.Fatalf("NewAuditStore: %v", err)
	}
	dispatcher := hooks.NewDispatcher(hooks.DispatcherOptions{
		Config: hooks.Config{
			Enabled: true,
			Hooks: []hooks.Definition{
				{ID: "zero.session-end", Event: hooks.EventSessionEnd, Command: goBinary, Args: []string{"version"}, Enabled: true},
			},
		},
		Audit: audit,
	})

	ctx, cancel := context.WithCancel(context.Background())
	provider := &cancelingProvider{cancel: cancel}

	_, err = Run(ctx, "hi", provider, Options{
		SessionID:    "session-cancel",
		Cwd:          t.TempDir(),
		ProviderName: "test-provider",
		Model:        "test-model",
		Hooks:        dispatcher,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err = %v, want context.Canceled", err)
	}

	events, err := audit.ReadEvents()
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	var completedStatus hooks.AuditStatus
	found := false
	for _, event := range events {
		if event.Type == "hook_execution_completed" && event.Event == hooks.EventSessionEnd {
			completedStatus = event.Status
			found = true
		}
	}
	if !found {
		t.Fatalf("no sessionEnd hook_execution_completed audit event, events=%#v", events)
	}
	if completedStatus != hooks.AuditCompleted {
		t.Fatalf("sessionEnd hook status = %q, want %q (hook must actually run despite ctx cancellation)", completedStatus, hooks.AuditCompleted)
	}
}

type recordingWebSearchTool struct {
	calls []map[string]any
}

func (tool *recordingWebSearchTool) Name() string        { return "web_search" }
func (tool *recordingWebSearchTool) Description() string { return "test web search tool" }
func (tool *recordingWebSearchTool) Parameters() tools.Schema {
	return tools.Schema{
		Type: "object",
		Properties: map[string]tools.PropertySchema{
			"query": {Type: "string"},
		},
		Required:             []string{"query"},
		AdditionalProperties: false,
	}
}
func (tool *recordingWebSearchTool) Safety() tools.Safety {
	return tools.Safety{
		SideEffect:      tools.SideEffectNetwork,
		Permission:      tools.PermissionPrompt,
		Reason:          "Sends model-provided search query text to the configured web search backend.",
		AdvertiseInAuto: true,
	}
}
func (tool *recordingWebSearchTool) Run(_ context.Context, args map[string]any) tools.Result {
	tool.calls = append(tool.calls, cloneArgs(args))
	return tools.Result{Status: tools.StatusOK, Output: "1. T — https://x.test"}
}

type sandboxDeniedRetryTool struct {
	calls []map[string]any
}

func (tool *sandboxDeniedRetryTool) Name() string        { return "bash" }
func (tool *sandboxDeniedRetryTool) Description() string { return "test shell tool" }
func (tool *sandboxDeniedRetryTool) Parameters() tools.Schema {
	return tools.Schema{
		Type: "object",
		Properties: map[string]tools.PropertySchema{
			"command":             {Type: "string"},
			"sandbox_permissions": {Type: "string"},
			"justification":       {Type: "string"},
		},
		Required:             []string{"command"},
		AdditionalProperties: false,
	}
}
func (tool *sandboxDeniedRetryTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectShell, Permission: tools.PermissionPrompt, Reason: "runs shell commands"}
}
func (tool *sandboxDeniedRetryTool) Run(_ context.Context, args map[string]any) tools.Result {
	tool.calls = append(tool.calls, cloneArgs(args))
	if args["sandbox_permissions"] == string(tools.SandboxPermissionsRequireEscalated) {
		return tools.Result{Status: tools.StatusOK, Output: "installed"}
	}
	return tools.Result{
		Status: tools.StatusError,
		Output: "touch: cannot touch '/home/user/.npm/cache': Read-only file system",
		ExecutionOutcome: &execution.Outcome{
			State: execution.StateDenied,
			Kind:  execution.OutcomeEnforcementDenied,
			Exit:  &execution.Exit{Code: 1},
			Denial: &execution.Denial{
				Capability:  execution.Capability{Kind: execution.CapabilityUnrestricted, Scope: "host"},
				Source:      execution.DenialSourcePlatformSandbox,
				Reason:      "sandbox blocked command execution",
				Recoverable: true,
				NextAction:  execution.DenialNextActionRequestApproval,
			},
		},
		Meta: map[string]string{
			"exit_code":                    "1",
			tools.SandboxLikelyDeniedMeta:  "true",
			tools.SandboxDenialReasonMeta:  "sandbox blocked command execution",
			tools.SandboxDenialKeywordMeta: "read-only file system",
		},
	}
}

type sandboxDeniedExecCommandRetryTool struct {
	sandboxDeniedRetryTool
}

func (tool *sandboxDeniedExecCommandRetryTool) Name() string { return "exec_command" }

type sandboxNetworkDeniedRetryTool struct {
	calls                      []map[string]any
	deferFileObservationCommit []bool
}

func (tool *sandboxNetworkDeniedRetryTool) Name() string        { return "bash" }
func (tool *sandboxNetworkDeniedRetryTool) Description() string { return "test shell tool" }
func (tool *sandboxNetworkDeniedRetryTool) Parameters() tools.Schema {
	return tools.Schema{
		Type: "object",
		Properties: map[string]tools.PropertySchema{
			"command": {Type: "string"},
		},
		Required:             []string{"command"},
		AdditionalProperties: false,
	}
}
func (tool *sandboxNetworkDeniedRetryTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectShell, Permission: tools.PermissionPrompt, Reason: "runs shell commands"}
}
func (tool *sandboxNetworkDeniedRetryTool) Run(ctx context.Context, args map[string]any) tools.Result {
	return tool.RunWithOptions(ctx, args, tools.RunOptions{})
}
func (tool *sandboxNetworkDeniedRetryTool) RunWithOptions(ctx context.Context, args map[string]any, options tools.RunOptions) tools.Result {
	tool.calls = append(tool.calls, cloneArgs(args))
	tool.deferFileObservationCommit = append(tool.deferFileObservationCommit, options.DeferFileObservationCommit)
	if shellNetworkAllowed(ctx, options.Sandbox, args) {
		return tools.Result{Status: tools.StatusOK, Output: "server started"}
	}
	return tools.Result{
		Status: tools.StatusOK,
		Output: "Cannot open a network socket.",
		Meta: map[string]string{
			"exit_code":                    "0",
			tools.SandboxLikelyDeniedMeta:  "true",
			tools.SandboxDenialKindMeta:    tools.SandboxDenialKindNetwork,
			tools.SandboxDenialReasonMeta:  "sandbox blocked command execution",
			tools.SandboxDenialKeywordMeta: "cannot open a network socket",
		},
	}
}

func shellNetworkAllowed(ctx context.Context, engine *sandbox.Engine, args map[string]any) bool {
	if engine == nil {
		return false
	}
	decision := engine.Evaluate(ctx, sandbox.Request{
		ToolName:          "bash",
		SideEffect:        sandbox.SideEffectShell,
		Permission:        sandbox.PermissionPrompt,
		PermissionGranted: true,
		PermissionMode:    sandbox.PermissionModeAsk,
		Args:              map[string]any{"command": "curl https://example.com"},
	})
	return decision.Action == sandbox.ActionAllow
}

func agentNativeBackendStub() sandbox.Backend {
	return sandbox.Backend{
		Name:            sandbox.BackendLinuxBwrap,
		Available:       true,
		Executable:      "/nonexistent/zero-linux-sandbox-stub",
		CommandWrapping: true,
		NativeIsolation: true,
	}
}

type sandboxNamespaceLimitedRetryTool struct {
	calls                      []map[string]any
	deferFileObservationCommit []bool
}

func (tool *sandboxNamespaceLimitedRetryTool) Name() string        { return "bash" }
func (tool *sandboxNamespaceLimitedRetryTool) Description() string { return "test shell tool" }
func (tool *sandboxNamespaceLimitedRetryTool) Parameters() tools.Schema {
	return tools.Schema{
		Type: "object",
		Properties: map[string]tools.PropertySchema{
			"command":             {Type: "string"},
			"sandbox_permissions": {Type: "string"},
		},
		Required:             []string{"command"},
		AdditionalProperties: false,
	}
}
func (tool *sandboxNamespaceLimitedRetryTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectShell, Permission: tools.PermissionPrompt, Reason: "runs shell commands"}
}
func (tool *sandboxNamespaceLimitedRetryTool) Run(ctx context.Context, args map[string]any) tools.Result {
	return tool.RunWithOptions(ctx, args, tools.RunOptions{})
}
func (tool *sandboxNamespaceLimitedRetryTool) RunWithOptions(_ context.Context, args map[string]any, options tools.RunOptions) tools.Result {
	tool.calls = append(tool.calls, cloneArgs(args))
	tool.deferFileObservationCommit = append(tool.deferFileObservationCommit, options.DeferFileObservationCommit)
	if args["sandbox_permissions"] == string(tools.SandboxPermissionsRequireEscalated) {
		return tools.Result{Status: tools.StatusOK, Output: "stdout:\nUSER PID COMMAND\nanaxy 42 firefox\nanaxy 43 Discord"}
	}
	return tools.Result{
		Status: tools.StatusOK,
		Output: "stdout:\nUSER PID COMMAND\nanaxy 1 bwrap --new-session --die-with-parent --unshare-user --unshare-pid --unshare-net --proc /proc -- /bin/sh -c ps aux\nanaxy 2 ps aux",
		Meta: map[string]string{
			"sandbox_backend":        string(sandbox.BackendLinuxBwrap),
			"sandbox_target_backend": string(sandbox.BackendLinuxBwrap),
			"sandbox_wrapped":        "true",
		},
	}
}

func TestRunRetriesShellUnsandboxedAfterSandboxNamespaceLimitedOutput(t *testing.T) {
	root := t.TempDir()
	retryTool := &sandboxNamespaceLimitedRetryTool{}
	registry := tools.NewRegistry()
	registry.Register(retryTool)
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "bash"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"command":"ps aux"}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "done"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}
	var requests []PermissionRequest
	var events []PermissionEvent

	result, err := Run(context.Background(), "check running applications", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		Autonomy:       "medium",
		Sandbox: sandbox.NewEngine(sandbox.EngineOptions{
			WorkspaceRoot: root,
			Policy:        sandbox.DefaultPolicy(),
			Backend:       agentNativeBackendStub(),
		}),
		OnPermissionRequest: func(_ context.Context, request PermissionRequest) (PermissionDecision, error) {
			requests = append(requests, request)
			return PermissionDecision{Action: PermissionDecisionAllowPrefix, Reason: "inspect host processes"}, nil
		},
		OnPermission: func(event PermissionEvent) {
			events = append(events, event)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("final answer = %q", result.FinalAnswer)
	}
	if len(retryTool.calls) != 2 {
		t.Fatalf("tool calls = %#v, want sandboxed attempt plus unsandboxed retry", retryTool.calls)
	}
	if retryTool.calls[1]["sandbox_permissions"] != string(tools.SandboxPermissionsRequireEscalated) {
		t.Fatalf("retry args = %#v, want require_escalated", retryTool.calls[1])
	}
	if len(retryTool.deferFileObservationCommit) != 2 || !retryTool.deferFileObservationCommit[1] {
		t.Fatalf("retry defer flags = %#v, want retry observation deferred", retryTool.deferFileObservationCommit)
	}
	if len(requests) != 1 {
		t.Fatalf("permission requests = %#v, want one unsandboxed retry approval", requests)
	}
	if !strings.Contains(requests[0].Reason, "sandbox PID namespace") {
		t.Fatalf("permission reason = %q, want sandbox namespace reason", requests[0].Reason)
	}
	if !equalStringSlices(requests[0].CommandPrefix, []string{"ps", "aux"}) {
		t.Fatalf("request command prefix = %#v, want ps aux", requests[0].CommandPrefix)
	}
	if len(events) != 1 || events[0].Action != PermissionActionAllow || events[0].DecisionAction != PermissionDecisionAllowPrefix {
		t.Fatalf("permission events = %#v, want approved unsandboxed retry", events)
	}
	if len(provider.requests) < 2 {
		t.Fatalf("provider requests = %d, want retry result sent back to model", len(provider.requests))
	}
	last := provider.requests[1].Messages[len(provider.requests[1].Messages)-1]
	if !strings.Contains(last.Content, "firefox") || strings.Contains(last.Content, "bwrap --new-session") {
		t.Fatalf("tool result sent to model = %q, want unsandboxed retry output only", last.Content)
	}
}

func TestRunUsesEscalatedJustificationForPermissionPrompt(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(&sandboxDeniedRetryTool{})
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "bash"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"command":"ps aux","sandbox_permissions":"require_escalated","justification":"Need host process visibility."}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}
	var requestReasons []string

	_, err := Run(context.Background(), "check host processes", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		Sandbox: sandbox.NewEngine(sandbox.EngineOptions{
			WorkspaceRoot: root,
			Policy:        sandbox.DefaultPolicy(),
			Backend:       agentNativeBackendStub(),
		}),
		OnPermissionRequest: func(_ context.Context, request PermissionRequest) (PermissionDecision, error) {
			requestReasons = append(requestReasons, request.Reason)
			return PermissionDecision{Action: PermissionDecisionDeny, Reason: "not needed"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(requestReasons) != 1 {
		t.Fatalf("permission reasons = %#v, want one request", requestReasons)
	}
	if requestReasons[0] != "Need host process visibility." {
		t.Fatalf("permission reason = %q, want model justification", requestReasons[0])
	}
}

func TestRunUsesUserFacingEscalatedFallbackForPermissionPrompt(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(&sandboxDeniedExecCommandRetryTool{})
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "exec_command"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"command":"ps aux","sandbox_permissions":"require_escalated"}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}
	var requestReasons []string

	_, err := Run(context.Background(), "check host processes", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		Sandbox: sandbox.NewEngine(sandbox.EngineOptions{
			WorkspaceRoot: root,
			Policy:        sandbox.DefaultPolicy(),
			Backend:       agentNativeBackendStub(),
		}),
		OnPermissionRequest: func(_ context.Context, request PermissionRequest) (PermissionDecision, error) {
			requestReasons = append(requestReasons, request.Reason)
			return PermissionDecision{Action: PermissionDecisionDeny, Reason: "not needed"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(requestReasons) != 1 {
		t.Fatalf("permission reasons = %#v, want one request", requestReasons)
	}
	if requestReasons[0] != "This command needs to run outside the sandbox." {
		t.Fatalf("permission reason = %q, want user-facing fallback", requestReasons[0])
	}
	if strings.Contains(requestReasons[0], sandbox.ReasonEscalatedSandboxRequired) {
		t.Fatalf("permission reason leaked internal sandbox copy: %q", requestReasons[0])
	}
}

func TestRunRetriesNetworkDeniedShellWithNetworkGrant(t *testing.T) {
	root := t.TempDir()
	retryTool := &sandboxNetworkDeniedRetryTool{}
	registry := tools.NewRegistry()
	registry.Register(retryTool)
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "bash"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"command":"node server.js"}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "done"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}
	var requests []PermissionRequest

	result, err := Run(context.Background(), "start server", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		Autonomy:       "medium",
		Sandbox: sandbox.NewEngine(sandbox.EngineOptions{
			WorkspaceRoot: root,
			Policy:        sandbox.DefaultPolicy(),
			Backend:       agentNativeBackendStub(),
		}),
		OnPermissionRequest: func(_ context.Context, request PermissionRequest) (PermissionDecision, error) {
			requests = append(requests, request)
			return PermissionDecision{Action: PermissionDecisionAllow, Reason: "approve network"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("final answer = %q", result.FinalAnswer)
	}
	if len(retryTool.calls) != 2 {
		t.Fatalf("tool calls = %#v, want denied attempt plus network-granted retry", retryTool.calls)
	}
	if _, escalated := retryTool.calls[1]["sandbox_permissions"]; escalated {
		t.Fatalf("network retry must stay sandboxed, got retry args %#v", retryTool.calls[1])
	}
	if len(retryTool.deferFileObservationCommit) != 2 || !retryTool.deferFileObservationCommit[1] {
		t.Fatalf("retry defer flags = %#v, want retry observation deferred", retryTool.deferFileObservationCommit)
	}
	if len(requests) != 1 || requests[0].Reason != sandbox.ReasonNetworkBlocked {
		t.Fatalf("permission requests = %#v, want one network approval", requests)
	}
	if len(provider.requests) < 2 {
		t.Fatalf("provider requests = %d, want retry result sent back to model", len(provider.requests))
	}
	last := provider.requests[1].Messages[len(provider.requests[1].Messages)-1]
	if !strings.Contains(last.Content, "server started") {
		t.Fatalf("tool result sent to model = %q, want network retry output", last.Content)
	}
}

func TestIsRetriableToolError(t *testing.T) {
	cases := []struct {
		name   string
		result ToolResult
		want   bool
	}{
		{"success", ToolResult{Status: tools.StatusOK}, false},
		{"bad arguments", ToolResult{Status: tools.StatusError, Output: "Error: Failed to parse arguments for x: bad json"}, true},
		{"execution failure", ToolResult{Status: tools.StatusError, Output: "Error: read foo.txt: no such file"}, true},
		{"disabled tool", ToolResult{Status: tools.StatusError, Output: `Error: Tool "x" is not enabled for this run.`}, false},
		{"permission denied (meta)", ToolResult{Status: tools.StatusError, Output: "Error: Permission denied for x: needs approval", Meta: map[string]string{"permission_action": "deny"}}, false},
		{"permission required", ToolResult{Status: tools.StatusError, Output: "Error: Permission required for x: approve first"}, false},
		{"sandbox block", ToolResult{Status: tools.StatusError, Output: "Error: Sandbox block: outside_workspace"}, false},
		{"sandbox approval", ToolResult{Status: tools.StatusError, Output: "Error: Sandbox approval required for x: network"}, false},
		// Structured denial categories are authoritative regardless of message text.
		{"denial: filtered", ToolResult{Status: tools.StatusError, Output: "anything", DenialReason: DenialFiltered}, false},
		{"denial: permission", ToolResult{Status: tools.StatusError, Output: "anything", DenialReason: DenialPermissionDenied}, false},
		{"denial: approval canceled", ToolResult{Status: tools.StatusError, Output: "anything", DenialReason: DenialApprovalCanceled}, false},
		{"denial: sandbox", ToolResult{Status: tools.StatusError, Output: "anything", DenialReason: DenialSandboxBlock}, false},
	}
	for _, c := range cases {
		if got := isRetriableToolError(c.result); got != c.want {
			t.Errorf("%s: isRetriableToolError = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRunRetriesShellUnsandboxedAfterSandboxDeniedExit(t *testing.T) {
	root := t.TempDir()
	retryTool := &sandboxDeniedRetryTool{}
	registry := tools.NewRegistry()
	registry.Register(retryTool)
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "bash"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"command":"npm install --save http-server"}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "done"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}
	var requests []PermissionRequest
	var events []PermissionEvent

	result, err := Run(context.Background(), "install package", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		Autonomy:       "medium",
		Sandbox: sandbox.NewEngine(sandbox.EngineOptions{
			WorkspaceRoot: root,
			Policy:        sandbox.DefaultPolicy(),
			Backend:       agentNativeBackendStub(),
		}),
		OnPermissionRequest: func(_ context.Context, request PermissionRequest) (PermissionDecision, error) {
			requests = append(requests, request)
			return PermissionDecision{Action: PermissionDecisionAllow, Reason: "retry outside sandbox"}, nil
		},
		OnPermission: func(event PermissionEvent) {
			events = append(events, event)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("final answer = %q", result.FinalAnswer)
	}
	if len(retryTool.calls) != 2 {
		t.Fatalf("tool calls = %#v, want sandboxed attempt plus unsandboxed retry", retryTool.calls)
	}
	if retryTool.calls[1]["sandbox_permissions"] != string(tools.SandboxPermissionsRequireEscalated) {
		t.Fatalf("retry args = %#v, want require_escalated", retryTool.calls[1])
	}
	if len(requests) != 2 {
		t.Fatalf("permission requests = %#v, want network approval plus retry approval", requests)
	}
	if requests[0].Reason != sandbox.ReasonNetworkBlocked {
		t.Fatalf("first request reason = %q, want network approval", requests[0].Reason)
	}
	if !strings.Contains(requests[1].Reason, "sandbox blocked command execution") || !strings.Contains(requests[1].Reason, "read-only file system") {
		t.Fatalf("retry request reason = %q", requests[1].Reason)
	}
	if len(events) != 1 || events[0].Action != PermissionActionAllow || events[0].DecisionAction != PermissionDecisionAllow {
		t.Fatalf("permission events = %#v, want approved unsandboxed retry", events)
	}
	if len(provider.requests) < 2 {
		t.Fatalf("provider requests = %d, want retry result sent back to model", len(provider.requests))
	}
	last := provider.requests[1].Messages[len(provider.requests[1].Messages)-1]
	if !strings.Contains(last.Content, "installed") {
		t.Fatalf("tool result sent to model = %q, want retry output", last.Content)
	}
}

func TestRunDoesNotRetryUnsandboxedWhenDeniedReadsActive(t *testing.T) {
	root := t.TempDir()
	retryTool := &sandboxDeniedRetryTool{}
	registry := tools.NewRegistry()
	registry.Register(retryTool)
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "bash"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"command":"touch cache-file"}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "done"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}
	var requests []PermissionRequest
	var events []PermissionEvent
	policy := sandbox.DefaultPolicy()
	policy.DenyRead = []string{filepath.Join(root, "secret.txt")}

	result, err := Run(context.Background(), "touch cache", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		Autonomy:       "medium",
		Sandbox: sandbox.NewEngine(sandbox.EngineOptions{
			WorkspaceRoot: root,
			Policy:        policy,
			Backend:       agentNativeBackendStub(),
		}),
		OnPermissionRequest: func(_ context.Context, request PermissionRequest) (PermissionDecision, error) {
			requests = append(requests, request)
			return PermissionDecision{Action: PermissionDecisionAllow, Reason: "unexpected"}, nil
		},
		OnPermission: func(event PermissionEvent) {
			events = append(events, event)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("final answer = %q", result.FinalAnswer)
	}
	if len(retryTool.calls) != 1 {
		t.Fatalf("tool calls = %#v, want only the original sandboxed attempt", retryTool.calls)
	}
	if len(requests) != 0 {
		t.Fatalf("permission requests = %#v, want no unsandboxed retry request", requests)
	}
	for _, event := range events {
		if strings.Contains(event.Reason, "sandbox blocked command execution") {
			t.Fatalf("permission events = %#v, want no unsandboxed retry event", events)
		}
	}
	if len(provider.requests) < 2 {
		t.Fatalf("provider requests = %d, want tool result sent back to model", len(provider.requests))
	}
	last := provider.requests[1].Messages[len(provider.requests[1].Messages)-1]
	if !strings.Contains(last.Content, "Read-only file system") {
		t.Fatalf("tool result sent to model = %q, want original sandbox denial", last.Content)
	}
}

func TestRunReturnsProviderText(t *testing.T) {
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{{
			{Type: zeroruntime.StreamEventText, Content: "hello"},
			{Type: zeroruntime.StreamEventText, Content: " zero"},
			{Type: zeroruntime.StreamEventDone},
		}},
	}

	result, err := Run(context.Background(), "say hi", provider, Options{
		Registry: tools.NewRegistry(),
	})

	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "hello zero" {
		t.Fatalf("expected final answer, got %q", result.FinalAnswer)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("expected one provider turn, got %d", len(provider.requests))
	}
	assertMessage(t, provider.requests[0].Messages[0], zeroruntime.MessageRoleSystem, "")
	assertMessage(t, provider.requests[0].Messages[1], zeroruntime.MessageRoleUser, "say hi")
}

func TestRunDoesNotPersistReasoningAsAssistantText(t *testing.T) {
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{{
			{Type: zeroruntime.StreamEventReasoning, Content: "private reasoning"},
			{Type: zeroruntime.StreamEventText, Content: "public answer"},
			{Type: zeroruntime.StreamEventDone},
		}},
	}
	var reasoning []string

	result, err := Run(context.Background(), "say hi", provider, Options{
		Registry:    tools.NewRegistry(),
		OnReasoning: func(delta string) { reasoning = append(reasoning, delta) },
	})

	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "public answer" {
		t.Fatalf("final answer = %q, want public answer", result.FinalAnswer)
	}
	if len(result.Messages) == 0 {
		t.Fatal("expected persisted messages")
	}
	last := result.Messages[len(result.Messages)-1]
	if last.Role != zeroruntime.MessageRoleAssistant || last.Content != "public answer" {
		t.Fatalf("assistant message = %#v, want answer-only assistant content", last)
	}
	if len(reasoning) != 1 || reasoning[0] != "private reasoning" {
		t.Fatalf("reasoning callbacks = %#v", reasoning)
	}
}

func TestRunReportsTruncationFinishReason(t *testing.T) {
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{{
			{Type: zeroruntime.StreamEventText, Content: "partial answer"},
			{Type: zeroruntime.StreamEventDone, FinishReason: zeroruntime.FinishReasonLength},
		}},
	}

	result, err := Run(context.Background(), "write a long thing", provider, Options{
		Registry: tools.NewRegistry(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "partial answer" {
		t.Fatalf("final answer = %q", result.FinalAnswer)
	}
	if result.FinishReason != zeroruntime.FinishReasonLength {
		t.Fatalf("FinishReason = %q, want %q", result.FinishReason, zeroruntime.FinishReasonLength)
	}
	if result.TruncationNotice() == "" {
		t.Fatal("TruncationNotice() empty for a truncated response")
	}
}

func TestRunNormalCompletionIsNotTruncated(t *testing.T) {
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{{
			{Type: zeroruntime.StreamEventText, Content: "done"},
			{Type: zeroruntime.StreamEventDone},
		}},
	}

	result, err := Run(context.Background(), "hi", provider, Options{Registry: tools.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	if result.TruncationNotice() != "" {
		t.Fatalf("normal completion reported as truncated: reason=%q", result.FinishReason)
	}
}

func TestResultTruncationNotice(t *testing.T) {
	cases := map[string]struct {
		reason     string
		wantNotice bool
	}{
		"length":         {zeroruntime.FinishReasonLength, true},
		"content_filter": {zeroruntime.FinishReasonContentFilter, true},
		"unknown":        {"weird_reason", true},
		"normal":         {"", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			notice := Result{FinishReason: tc.reason}.TruncationNotice()
			if (notice != "") != tc.wantNotice {
				t.Fatalf("notice = %q, wantNotice = %v", notice, tc.wantNotice)
			}
		})
	}
}

func TestRunEmitsTextDeltas(t *testing.T) {
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{{
			{Type: zeroruntime.StreamEventText, Content: "hello"},
			{Type: zeroruntime.StreamEventText, Content: " zero"},
			{Type: zeroruntime.StreamEventDone},
		}},
	}

	var deltas []string
	_, err := Run(context.Background(), "say hi", provider, Options{
		Registry: tools.NewRegistry(),
		OnText:   func(delta string) { deltas = append(deltas, delta) },
	})

	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(deltas, "|") != "hello| zero" {
		t.Fatalf("expected text deltas, got %#v", deltas)
	}
}

func TestRunEmitsUsageEvents(t *testing.T) {
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{{
			{Type: zeroruntime.StreamEventUsage, Usage: zeroruntime.Usage{PromptTokens: 12, CompletionTokens: 5, CachedInputTokens: 2}},
			{Type: zeroruntime.StreamEventText, Content: "done"},
			{Type: zeroruntime.StreamEventDone},
		}},
	}

	var usages []zeroruntime.Usage
	_, err := Run(context.Background(), "track usage", provider, Options{
		Registry: tools.NewRegistry(),
		OnUsage:  func(usage zeroruntime.Usage) { usages = append(usages, usage) },
	})

	if err != nil {
		t.Fatal(err)
	}
	if len(usages) != 1 {
		t.Fatalf("expected one usage event, got %#v", usages)
	}
	if usages[0].PromptTokens != 12 || usages[0].CompletionTokens != 5 || usages[0].CachedInputTokens != 2 {
		t.Fatalf("unexpected usage event: %#v", usages[0])
	}
}

func TestRunAdvertisesRuntimeToolDefinitions(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(root, nil))
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{{
			{Type: zeroruntime.StreamEventText, Content: "done"},
			{Type: zeroruntime.StreamEventDone},
		}},
	}

	_, err := Run(context.Background(), "what tools exist?", provider, Options{
		Registry: registry,
	})

	if err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("expected one request, got %d", len(provider.requests))
	}
	if len(provider.requests[0].Tools) != 1 {
		t.Fatalf("expected one advertised tool, got %#v", provider.requests[0].Tools)
	}

	toolDefinition := provider.requests[0].Tools[0]
	if toolDefinition.Name != "read_file" {
		t.Fatalf("expected read_file definition, got %#v", toolDefinition)
	}
	parameters := toolDefinition.Parameters
	if parameters["type"] != "object" {
		t.Fatalf("expected object schema, got %#v", parameters)
	}
	if parameters["additionalProperties"] != false {
		t.Fatalf("expected additionalProperties=false, got %#v", parameters["additionalProperties"])
	}
	properties, ok := parameters["properties"].(map[string]any)
	if !ok {
		t.Fatalf("expected properties map, got %#v", parameters["properties"])
	}
	pathProperty, ok := properties["path"].(map[string]any)
	if !ok {
		t.Fatalf("expected path property map, got %#v", properties["path"])
	}
	if pathProperty["type"] != "string" || pathProperty["description"] == "" {
		t.Fatalf("unexpected path property schema: %#v", pathProperty)
	}
	required, ok := parameters["required"].([]string)
	if !ok || len(required) != 1 || required[0] != "path" {
		t.Fatalf("unexpected required fields: %#v", parameters["required"])
	}
}

func TestRunAdvertisesWebFetchInAutoMode(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(tools.NewWebFetchTool())
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{{
			{Type: zeroruntime.StreamEventText, Content: "done"},
			{Type: zeroruntime.StreamEventDone},
		}},
	}

	_, err := Run(context.Background(), "what tools exist?", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAuto,
	})

	if err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("expected one request, got %d", len(provider.requests))
	}
	if len(provider.requests[0].Tools) != 1 || provider.requests[0].Tools[0].Name != "web_fetch" {
		t.Fatalf("expected web_fetch to be advertised in auto mode, got %#v", provider.requests[0].Tools)
	}
}

func TestRunRejectsLocalWebFetchBeforePermissionRequest(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(tools.NewWebFetchTool())
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "web_fetch"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"url":"http://localhost:8000/index.html"}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "done"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}
	var requests []PermissionRequest

	result, err := Run(context.Background(), "fetch local page", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		OnPermissionRequest: func(_ context.Context, request PermissionRequest) (PermissionDecision, error) {
			requests = append(requests, request)
			return PermissionDecision{Action: PermissionDecisionDeny, Reason: "unexpected permission request"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("final answer = %q", result.FinalAnswer)
	}
	if len(requests) != 0 {
		t.Fatalf("expected no permission request, got %#v", requests)
	}
	if len(provider.requests) < 2 {
		t.Fatalf("expected tool result to be sent back to provider, got %d requests", len(provider.requests))
	}
	lastMessage := provider.requests[1].Messages[len(provider.requests[1].Messages)-1]
	if lastMessage.ToolCallID != "call-1" {
		t.Fatalf("expected tool_call_id call-1, got %q", lastMessage.ToolCallID)
	}
	if strings.Contains(lastMessage.Content, "Permission required") {
		t.Fatalf("local web_fetch must not request permission first: %q", lastMessage.Content)
	}
	if !strings.Contains(lastMessage.Content, "bash with curl") {
		t.Fatalf("expected curl guidance, got %q", lastMessage.Content)
	}
}

func TestRunAdvertisesPromptedWebSearchInAutoMode(t *testing.T) {
	t.Setenv("ZERO_WEBSEARCH_BASE_URL", "https://search.example/api")
	registry := tools.NewRegistry()
	for _, tool := range tools.CoreNetworkTools() {
		if tool.Name() == "web_search" {
			registry.Register(tool)
		}
	}
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{{
			{Type: zeroruntime.StreamEventText, Content: "done"},
			{Type: zeroruntime.StreamEventDone},
		}},
	}

	_, err := Run(context.Background(), "what tools exist?", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAuto,
	})

	if err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("expected one request, got %d", len(provider.requests))
	}
	if len(provider.requests[0].Tools) != 1 || provider.requests[0].Tools[0].Name != "web_search" {
		t.Fatalf("expected web_search to be advertised in auto mode, got %#v", provider.requests[0].Tools)
	}
}

func TestRunRequestsPermissionBeforeWebSearchExecution(t *testing.T) {
	search := &recordingWebSearchTool{}
	registry := tools.NewRegistry()
	registry.Register(search)
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "web_search"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"query":"private workspace detail"}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "done"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}
	var requests []PermissionRequest

	result, err := Run(context.Background(), "search", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		OnPermissionRequest: func(_ context.Context, request PermissionRequest) (PermissionDecision, error) {
			requests = append(requests, request)
			return PermissionDecision{Action: PermissionDecisionDeny, Reason: "network not approved"}, nil
		},
	})

	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("expected final answer after denied tool call, got %q", result.FinalAnswer)
	}
	if len(requests) != 1 {
		t.Fatalf("expected one permission request, got %#v", requests)
	}
	if requests[0].ToolName != "web_search" || requests[0].Permission != string(tools.PermissionPrompt) || requests[0].SideEffect != string(tools.SideEffectNetwork) {
		t.Fatalf("unexpected permission request: %#v", requests[0])
	}
	if len(search.calls) != 0 {
		t.Fatalf("web_search backend must not run when permission is denied, got calls %#v", search.calls)
	}
}

func TestRunFiltersAdvertisedTools(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(root, nil))
	registry.Register(tools.NewScopedGrepTool(root, nil))
	registry.Register(tools.NewScopedWriteFileTool(root, nil))
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{{
			{Type: zeroruntime.StreamEventText, Content: "done"},
			{Type: zeroruntime.StreamEventDone},
		}},
	}

	_, err := Run(context.Background(), "what tools exist?", provider, Options{
		Registry:      registry,
		EnabledTools:  []string{"read_file", "grep"},
		DisabledTools: []string{"grep"},
	})

	if err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("expected one request, got %d", len(provider.requests))
	}
	if len(provider.requests[0].Tools) != 1 || provider.requests[0].Tools[0].Name != "read_file" {
		t.Fatalf("expected only read_file to be advertised, got %#v", provider.requests[0].Tools)
	}
}

func TestRunRejectsFilteredToolCalls(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(root, nil))
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call_1", ToolName: "read_file"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call_1", ArgumentsFragment: `{"path":"README.md"}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call_1"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "done"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}

	result, err := Run(context.Background(), "read", provider, Options{
		Registry:      registry,
		DisabledTools: []string{"read_file"},
		MaxTurns:      2,
	})

	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("expected final answer after filtered tool result, got %q", result.FinalAnswer)
	}
	lastMessage := result.Messages[len(result.Messages)-2]
	if !strings.Contains(lastMessage.Content, "not enabled") {
		t.Fatalf("expected filtered tool error message, got %#v", result.Messages)
	}
}

func TestRunRejectsToolCallsOutsideEnabledList(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(root, nil))
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call_1", ToolName: "read_file"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call_1", ArgumentsFragment: `{"path":"README.md"}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call_1"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "done"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}

	result, err := Run(context.Background(), "read", provider, Options{
		Registry:     registry,
		EnabledTools: []string{"grep"},
		MaxTurns:     2,
	})

	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("expected final answer after filtered tool result, got %q", result.FinalAnswer)
	}
	lastMessage := result.Messages[len(result.Messages)-2]
	if !strings.Contains(lastMessage.Content, "not enabled") {
		t.Fatalf("expected filtered tool error message, got %#v", result.Messages)
	}
}

func TestRunExecutesToolCallThroughRegistry(t *testing.T) {
	root := t.TempDir()
	writeAgentTestFile(t, filepath.Join(root, "notes.txt"), "alpha\nbeta\n")
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(root, nil))
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "read_file"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"path":"notes.txt"}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "read done"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}

	var toolResults []ToolResult
	result, err := Run(context.Background(), "read notes", provider, Options{
		Registry:     registry,
		OnToolResult: func(result ToolResult) { toolResults = append(toolResults, result) },
	})

	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "read done" {
		t.Fatalf("expected final answer from second turn, got %q", result.FinalAnswer)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("expected provider to be called twice, got %d", len(provider.requests))
	}
	lastMessage := provider.requests[1].Messages[len(provider.requests[1].Messages)-1]
	assertMessage(t, lastMessage, zeroruntime.MessageRoleTool, "alpha")
	if lastMessage.ToolCallID != "call-1" {
		t.Fatalf("expected tool_call_id call-1, got %q", lastMessage.ToolCallID)
	}
	if len(toolResults) != 1 || toolResults[0].Status != tools.StatusOK {
		t.Fatalf("expected one ok tool result, got %#v", toolResults)
	}
}

func TestRunPreservesRequestPrefixAcrossTurns(t *testing.T) {
	root := t.TempDir()
	writeAgentTestFile(t, filepath.Join(root, "notes.txt"), "alpha\n")
	registry := tools.NewRegistry()
	registry.Register(tools.NewReadFileTool(root))
	provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{
		{
			{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "read_file"},
			{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"path":"notes.txt"}`},
			{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
			{Type: zeroruntime.StreamEventDone},
		},
		{
			{Type: zeroruntime.StreamEventText, Content: "done"},
			{Type: zeroruntime.StreamEventDone},
		},
	}}

	if _, err := Run(context.Background(), "read notes", provider, Options{
		Cwd:       root,
		Registry:  registry,
		SessionID: "session-stable-prefix",
	}); err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(provider.requests))
	}
	first, second := provider.requests[0], provider.requests[1]
	if len(second.Messages) < len(first.Messages) || !reflect.DeepEqual(second.Messages[:len(first.Messages)], first.Messages) {
		t.Fatalf("first request messages must be an exact prefix of the second:\nfirst=%#v\nsecond=%#v", first.Messages, second.Messages)
	}
	if !reflect.DeepEqual(first.Tools, second.Tools) {
		t.Fatalf("tool definitions drifted between turns:\nfirst=%#v\nsecond=%#v", first.Tools, second.Tools)
	}
	if first.PromptCacheKey != "session-stable-prefix" || second.PromptCacheKey != first.PromptCacheKey {
		t.Fatalf("prompt cache key must remain stable: first=%q second=%q", first.PromptCacheKey, second.PromptCacheKey)
	}
}

func TestRunSanitizesMalformedToolCallArgumentsBeforeRetry(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(root, nil))
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "read_file"},
				// Genuinely malformed (truncated) args: still error -> sanitize -> retry.
				// (Concatenated multi-object args are now tolerated; see
				// TestRunRecoversFirstObjectFromConcatenatedToolArgs.)
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"path":"README.md"`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "recovered"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}

	result, err := Run(context.Background(), "read files", provider, Options{
		Registry: registry,
		MaxTurns: 2,
	})

	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "recovered" {
		t.Fatalf("expected recovery turn final answer, got %q", result.FinalAnswer)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("expected retry request after malformed tool args, got %d requests", len(provider.requests))
	}

	var assistantCall *zeroruntime.ToolCall
	var toolParseError string
	for _, message := range provider.requests[1].Messages {
		if message.Role == zeroruntime.MessageRoleAssistant && len(message.ToolCalls) > 0 {
			assistantCall = &message.ToolCalls[0]
		}
		if message.Role == zeroruntime.MessageRoleTool && strings.Contains(message.Content, "Failed to parse arguments for read_file") {
			toolParseError = message.Content
		}
	}
	if assistantCall == nil {
		t.Fatalf("expected retry request to include assistant tool call history, got %#v", provider.requests[1].Messages)
	}
	if !json.Valid([]byte(assistantCall.Arguments)) {
		t.Fatalf("assistant tool-call arguments must be valid JSON for provider replay, got %q", assistantCall.Arguments)
	}
	if assistantCall.Arguments != "{}" {
		t.Fatalf("malformed arguments should be sanitized to an empty JSON object, got %q", assistantCall.Arguments)
	}
	if toolParseError == "" {
		t.Fatalf("expected model-visible tool result to keep the parse error, messages: %#v", provider.requests[1].Messages)
	}
}

// A weak model (e.g. minimax-m3) that packs MULTIPLE concatenated JSON objects into
// one tool-call slot must RECOVER the first object and run the call, not fail with
// "invalid character '{' after top-level value". The trailing object(s) are dropped
// (the model re-issues them on a later turn if still needed).
func TestRunRecoversFirstObjectFromConcatenatedToolArgs(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hello readme"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(root, nil))
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "read_file"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"path":"README.md"}{"path":"AGENTS.md"}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "done"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}

	result, err := Run(context.Background(), "read", provider, Options{Registry: registry, MaxTurns: 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("final answer = %q, want done", result.FinalAnswer)
	}
	var toolResult string
	for _, message := range provider.requests[1].Messages {
		if message.Role == zeroruntime.MessageRoleTool {
			toolResult = message.Content
		}
	}
	if strings.Contains(toolResult, "Failed to parse arguments") {
		t.Fatalf("concatenated args should recover the first object, not fail to parse: %q", toolResult)
	}
	if !strings.Contains(toolResult, "hello readme") {
		t.Fatalf("expected README.md contents (the first object's path), got %q", toolResult)
	}
}

func TestRunDefersSelfCorrectFeedbackUntilAfterToolBatch(t *testing.T) {
	// A single assistant turn with two tool calls where the first mutates and
	// self-correct fails must keep both tool_results contiguous, with the feedback
	// appended only after the whole batch — otherwise a user message interleaves
	// between tool_results and breaks strict provider replay.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "existing.txt"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedWriteFileTool(root, nil))
	registry.Register(tools.NewScopedReadFileTool(root, nil))

	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "write_file"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"path":"notes.txt","content":"hello"}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-2", ToolName: "read_file"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-2", ArgumentsFragment: `{"path":"existing.txt"}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-2"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "done"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}

	corrector := NewSelfCorrector(root, erroringChecker{err: errors.New("lsp boom")}, nil, SelfCorrectConfig{
		Enabled:    true,
		IncludeLSP: true,
		Autonomy:   "high",
	})

	if _, err := Run(context.Background(), "go", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		Autonomy:       "medium",
		SelfCorrect:    corrector,
		OnPermissionRequest: func(_ context.Context, _ PermissionRequest) (PermissionDecision, error) {
			return PermissionDecision{Action: PermissionDecisionAllow, Reason: "test"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) < 2 {
		t.Fatalf("expected a second completion request, got %d", len(provider.requests))
	}

	msgs := provider.requests[1].Messages
	var toolIdx []int
	feedbackIdx := -1
	for i, m := range msgs {
		switch m.Role {
		case zeroruntime.MessageRoleTool:
			toolIdx = append(toolIdx, i)
		case zeroruntime.MessageRoleUser:
			if strings.Contains(m.Content, "Verification failed after your edit") {
				feedbackIdx = i
			}
		}
	}
	if len(toolIdx) != 2 {
		t.Fatalf("expected 2 tool_result messages, got %d: %#v", len(toolIdx), msgs)
	}
	if feedbackIdx == -1 {
		t.Fatalf("expected a self-correct feedback message, messages: %#v", msgs)
	}
	if toolIdx[1] != toolIdx[0]+1 {
		t.Fatalf("tool_results must be contiguous, got indices %v: %#v", toolIdx, msgs)
	}
	if feedbackIdx < toolIdx[1] {
		t.Fatalf("self-correct feedback (idx %d) must come after both tool_results %v", feedbackIdx, toolIdx)
	}
}

func TestRunBatchesSelfCorrectOncePerTurn(t *testing.T) {
	// Two mutating tool calls in one assistant turn must trigger a single
	// AfterEdit over the union of changed files — not one per call — so the per-run
	// attempt budget isn't consumed twice and an intermediate edit isn't verified
	// after a later call in the same turn supersedes it.
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedWriteFileTool(root, nil))

	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "write_file"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"path":"a.txt","content":"hello"}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-2", ToolName: "write_file"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-2", ArgumentsFragment: `{"path":"b.txt","content":"hello"}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-2"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "done"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}

	corrector := NewSelfCorrector(root, erroringChecker{err: errors.New("boom")}, nil, SelfCorrectConfig{
		Enabled:    true,
		IncludeLSP: true,
		Autonomy:   "high",
	})

	if _, err := Run(context.Background(), "go", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		Autonomy:       "medium",
		SelfCorrect:    corrector,
		OnPermissionRequest: func(_ context.Context, _ PermissionRequest) (PermissionDecision, error) {
			return PermissionDecision{Action: PermissionDecisionAllow, Reason: "test"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	// One AfterEdit for the whole turn -> exactly one corrective attempt consumed
	// (the old per-call path would have consumed two).
	if corrector.attempts != 1 {
		t.Fatalf("AfterEdit should run once per turn (attempts=1), got %d", corrector.attempts)
	}
	feedbackCount := 0
	for _, m := range provider.requests[1].Messages {
		if m.Role == zeroruntime.MessageRoleUser && strings.Contains(m.Content, "Verification failed after your edit") {
			feedbackCount++
		}
	}
	if feedbackCount != 1 {
		t.Fatalf("expected exactly one self-correct feedback message, got %d", feedbackCount)
	}
}

func TestRunDeniesPromptToolWithoutUnsafePermission(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedWriteFileTool(root, nil))
	provider := providerCallingWriteFileThenAnswer("write denied")
	var permissionEvents []PermissionEvent

	result, err := Run(context.Background(), "write notes", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		OnPermission: func(event PermissionEvent) {
			permissionEvents = append(permissionEvents, event)
		},
	})

	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "write denied" {
		t.Fatalf("expected final answer, got %q", result.FinalAnswer)
	}
	if _, err := os.Stat(filepath.Join(root, "notes.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected denied write to leave file missing, stat error: %v", err)
	}
	lastMessage := provider.requests[1].Messages[len(provider.requests[1].Messages)-1]
	if !strings.Contains(lastMessage.Content, "Permission required for write_file") {
		t.Fatalf("expected permission denial tool result, got %q", lastMessage.Content)
	}
	if len(permissionEvents) != 1 {
		t.Fatalf("expected one permission event, got %#v", permissionEvents)
	}
	event := permissionEvents[0]
	if event.ToolCallID != "call-1" || event.ToolName != "write_file" || event.Action != PermissionActionPrompt {
		t.Fatalf("unexpected permission event: %#v", event)
	}
	if event.Permission != string(tools.PermissionPrompt) || event.PermissionMode != PermissionModeAsk || event.SideEffect != string(tools.SideEffectWrite) {
		t.Fatalf("unexpected permission metadata: %#v", event)
	}
	if !strings.Contains(event.Reason, "Creates or overwrites files") {
		t.Fatalf("expected tool safety reason in permission event, got %#v", event)
	}
}

func TestRunRequestsPromptToolPermissionBeforeExecution(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedWriteFileTool(root, nil))
	provider := providerCallingWriteFileThenAnswer("write approved")
	var requests []PermissionRequest
	var permissionEvents []PermissionEvent

	result, err := Run(context.Background(), "write notes", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		Autonomy:       "medium",
		OnPermissionRequest: func(_ context.Context, request PermissionRequest) (PermissionDecision, error) {
			requests = append(requests, request)
			return PermissionDecision{Action: PermissionDecisionAllow, Reason: "approved for test"}, nil
		},
		OnPermission: func(event PermissionEvent) {
			permissionEvents = append(permissionEvents, event)
		},
	})

	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "write approved" {
		t.Fatalf("expected final answer, got %q", result.FinalAnswer)
	}
	content, err := os.ReadFile(filepath.Join(root, "notes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "hello" {
		t.Fatalf("expected approved write content, got %q", content)
	}
	if len(requests) != 1 {
		t.Fatalf("expected one permission request, got %#v", requests)
	}
	request := requests[0]
	if request.ToolCallID != "call-1" || request.ToolName != "write_file" || request.Action != PermissionActionPrompt {
		t.Fatalf("unexpected permission request: %#v", request)
	}
	if request.PermissionMode != PermissionModeAsk || request.SideEffect != string(tools.SideEffectWrite) || request.Autonomy != "medium" {
		t.Fatalf("unexpected request metadata: %#v", request)
	}
	if len(permissionEvents) != 1 {
		t.Fatalf("expected one final permission event, got %#v", permissionEvents)
	}
	event := permissionEvents[0]
	if event.Action != PermissionActionAllow || !event.PermissionGranted {
		t.Fatalf("expected final allow event after approval, got %#v", event)
	}
	if event.DecisionReason != "approved for test" {
		t.Fatalf("expected decision reason in final event, got %#v", event)
	}
}

func TestRunAllowsWorkspaceWriteWithoutPromptWhenSandboxPolicyPermits(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedWriteFileTool(root, nil))
	provider := providerCallingWriteFileThenAnswer("write done")
	var permissionEvents []PermissionEvent

	result, err := Run(context.Background(), "write notes", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		Autonomy:       "medium",
		Sandbox: sandbox.NewEngine(sandbox.EngineOptions{
			WorkspaceRoot: root,
			Policy:        sandbox.DefaultPolicy(),
		}),
		OnPermissionRequest: func(context.Context, PermissionRequest) (PermissionDecision, error) {
			t.Fatal("workspace write should not request permission")
			return PermissionDecision{}, nil
		},
		OnPermission: func(event PermissionEvent) {
			permissionEvents = append(permissionEvents, event)
		},
	})

	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "write done" {
		t.Fatalf("expected final answer, got %q", result.FinalAnswer)
	}
	content, err := os.ReadFile(filepath.Join(root, "notes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "hello" {
		t.Fatalf("expected workspace write content, got %q", content)
	}
	if len(permissionEvents) != 1 {
		t.Fatalf("expected one auto permission event, got %#v", permissionEvents)
	}
	event := permissionEvents[0]
	if event.Action != PermissionActionAllow || event.PermissionGranted || event.GrantMatched {
		t.Fatalf("expected workspace allow without user grant, got %#v", event)
	}
	if event.Reason != "workspace write is allowed" {
		t.Fatalf("expected workspace-write reason, got %#v", event)
	}
}

func TestRunDeniesPromptToolWhenPermissionRequestDenied(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedWriteFileTool(root, nil))
	provider := providerCallingWriteFileThenAnswer("write denied")
	var requests []PermissionRequest
	var permissionEvents []PermissionEvent

	result, err := Run(context.Background(), "write notes", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		OnPermissionRequest: func(_ context.Context, request PermissionRequest) (PermissionDecision, error) {
			requests = append(requests, request)
			return PermissionDecision{Action: PermissionDecisionDeny, Reason: "not this command"}, nil
		},
		OnPermission: func(event PermissionEvent) {
			permissionEvents = append(permissionEvents, event)
		},
	})

	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "write denied" {
		t.Fatalf("expected final answer, got %q", result.FinalAnswer)
	}
	if _, err := os.Stat(filepath.Join(root, "notes.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected denied write to leave file missing, stat error: %v", err)
	}
	if len(requests) != 1 {
		t.Fatalf("expected one permission request, got %#v", requests)
	}
	lastMessage := provider.requests[1].Messages[len(provider.requests[1].Messages)-1]
	if !strings.Contains(lastMessage.Content, "Permission denied for write_file") || !strings.Contains(lastMessage.Content, "not this command") {
		t.Fatalf("expected denied permission tool result, got %q", lastMessage.Content)
	}
	if len(permissionEvents) != 1 {
		t.Fatalf("expected one final permission event, got %#v", permissionEvents)
	}
	event := permissionEvents[0]
	if event.Action != PermissionActionDeny || event.PermissionGranted {
		t.Fatalf("expected final deny event, got %#v", event)
	}
	if event.DecisionReason != "not this command" {
		t.Fatalf("expected denial reason in final event, got %#v", event)
	}
}

func TestRunAbortsWhenPermissionRequestCanceled(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedWriteFileTool(root, nil))
	provider := providerCallingWriteFileThenAnswer("write should not continue")
	var permissionEvents []PermissionEvent

	result, err := Run(context.Background(), "write notes", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		OnPermissionRequest: func(context.Context, PermissionRequest) (PermissionDecision, error) {
			return PermissionDecision{Action: PermissionDecisionCancel, Reason: "redirect needed"}, nil
		},
		OnPermission: func(event PermissionEvent) {
			permissionEvents = append(permissionEvents, event)
		},
	})

	if !errors.Is(err, ErrPermissionApprovalCanceled) {
		t.Fatalf("expected permission approval cancel error, got %v", err)
	}
	if result.FinalAnswer != "" {
		t.Fatalf("canceled run must not continue to final answer, got %q", result.FinalAnswer)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("canceled permission prompt should stop after first turn, got %d provider requests", len(provider.requests))
	}
	if _, err := os.Stat(filepath.Join(root, "notes.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected canceled write to leave file missing, stat error: %v", err)
	}
	if len(permissionEvents) != 1 {
		t.Fatalf("expected one cancel permission event, got %#v", permissionEvents)
	}
	event := permissionEvents[0]
	if event.Action != PermissionActionCancel || event.DecisionAction != PermissionDecisionCancel || event.PermissionGranted {
		t.Fatalf("expected final cancel event, got %#v", event)
	}
	if event.DecisionReason != "redirect needed" {
		t.Fatalf("expected cancel reason in final event, got %#v", event)
	}
}

func TestAvailablePermissionDecisionsSplitDenyAndCancel(t *testing.T) {
	options := Options{Sandbox: sandbox.NewEngine(sandbox.EngineOptions{WorkspaceRoot: t.TempDir(), Policy: sandbox.DefaultPolicy()})}
	cases := []struct {
		name string
		tool string
		want []PermissionDecisionAction
	}{
		{
			name: "bash keeps recoverable deny and explicit cancel",
			tool: "bash",
			want: []PermissionDecisionAction{
				PermissionDecisionAllow,
				PermissionDecisionAllowForSession,
				PermissionDecisionDeny,
				PermissionDecisionCancel,
			},
		},
		{
			name: "apply patch uses cancel without recoverable deny",
			tool: "apply_patch",
			want: []PermissionDecisionAction{
				PermissionDecisionAllow,
				PermissionDecisionAllowForSession,
				PermissionDecisionCancel,
			},
		},
		{
			name: "file write keeps recoverable deny",
			tool: "write_file",
			want: []PermissionDecisionAction{
				PermissionDecisionAllow,
				PermissionDecisionAllowForSession,
				PermissionDecisionDeny,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := availablePermissionDecisions(PermissionEvent{
				ToolName: tc.tool,
				Action:   PermissionActionPrompt,
			}, nil, options)
			if !equalPermissionDecisions(got, tc.want) {
				t.Fatalf("decisions = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestRunPersistsAlwaysAllowPermissionDecision(t *testing.T) {
	root := t.TempDir()
	store, err := sandbox.NewGrantStore(sandbox.StoreOptions{FilePath: filepath.Join(t.TempDir(), "sandbox-grants.json")})
	if err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedWriteFileTool(root, nil))
	provider := providerCallingWriteFileThenAnswer("write approved")
	var permissionEvents []PermissionEvent
	policy := sandbox.DefaultPolicy()
	policy.EnforceWorkspace = false

	result, err := Run(context.Background(), "write notes", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		Autonomy:       "medium",
		Sandbox: sandbox.NewEngine(sandbox.EngineOptions{
			WorkspaceRoot: root,
			Policy:        policy,
			Store:         store,
		}),
		OnPermissionRequest: func(_ context.Context, request PermissionRequest) (PermissionDecision, error) {
			if request.Risk.Level == "" {
				t.Fatalf("expected request risk to be populated: %#v", request)
			}
			return PermissionDecision{Action: PermissionDecisionAlwaysAllow, Reason: "trust write_file for this workspace"}, nil
		},
		OnPermission: func(event PermissionEvent) {
			permissionEvents = append(permissionEvents, event)
		},
	})

	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "write approved" {
		t.Fatalf("expected final answer, got %q", result.FinalAnswer)
	}
	// The grant is scoped to exactly the file the call wrote, anchored to the
	// workspace — not a blanket tool-wide allow.
	notesPath := filepath.Join(root, "notes.txt")
	lookup, err := store.Lookup("write_file", notesPath)
	if err != nil {
		t.Fatal(err)
	}
	if !lookup.Matched || lookup.Grant.Decision != sandbox.GrantAllow {
		t.Fatalf("expected persistent allow grant, got %#v", lookup)
	}
	if lookup.Grant.ScopeKind != sandbox.ScopeFile || lookup.Grant.Scope != notesPath {
		t.Fatalf("expected file-scoped grant for %q, got %#v", notesPath, lookup.Grant)
	}
	// A different file in the same workspace is NOT covered by that grant.
	other, err := store.Lookup("write_file", filepath.Join(root, "other.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if other.Matched {
		t.Fatalf("a sibling file must not be covered by a file-scoped grant: %#v", other)
	}
	if len(permissionEvents) != 1 {
		t.Fatalf("expected one final permission event, got %#v", permissionEvents)
	}
	event := permissionEvents[0]
	if event.Action != PermissionActionAllow || !event.PermissionGranted || event.Grant == nil {
		t.Fatalf("expected allow event with persisted grant, got %#v", event)
	}
	if event.Grant.Decision != sandbox.GrantAllow || event.Grant.ToolName != "write_file" {
		t.Fatalf("unexpected persisted grant in event: %#v", event)
	}
}

func TestRunSessionAllowSkipsMatchingPromptWithoutPersistentGrant(t *testing.T) {
	root := t.TempDir()
	store, err := sandbox.NewGrantStore(sandbox.StoreOptions{FilePath: filepath.Join(t.TempDir(), "sandbox-grants.json")})
	if err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedWriteFileTool(root, nil))
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "write_file"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"path":"notes.txt","content":"first","overwrite":true}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-2", ToolName: "write_file"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-2", ArgumentsFragment: `{"path":"notes.txt","content":"second","overwrite":true}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-2"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "done"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}
	var requests []PermissionRequest
	var permissionEvents []PermissionEvent
	policy := sandbox.DefaultPolicy()
	policy.EnforceWorkspace = false

	result, err := Run(context.Background(), "write notes twice", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		Autonomy:       "medium",
		Sandbox: sandbox.NewEngine(sandbox.EngineOptions{
			WorkspaceRoot: root,
			Policy:        policy,
			Store:         store,
		}),
		OnPermissionRequest: func(_ context.Context, request PermissionRequest) (PermissionDecision, error) {
			requests = append(requests, request)
			return PermissionDecision{Action: PermissionDecisionAllowForSession, Reason: "trust this file for the session"}, nil
		},
		OnPermission: func(event PermissionEvent) {
			permissionEvents = append(permissionEvents, event)
		},
	})

	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("expected final answer, got %q", result.FinalAnswer)
	}
	content, err := os.ReadFile(filepath.Join(root, "notes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "second" {
		t.Fatalf("expected second write content, got %q", content)
	}
	if len(requests) != 1 {
		t.Fatalf("expected one permission request, got %#v", requests)
	}
	if len(permissionEvents) != 2 {
		t.Fatalf("expected two permission events, got %#v", permissionEvents)
	}
	if permissionEvents[0].DecisionAction != PermissionDecisionAllowForSession || permissionEvents[0].Grant == nil || !permissionEvents[0].Grant.Session {
		t.Fatalf("expected first event to carry session grant, got %#v", permissionEvents[0])
	}
	if !permissionEvents[1].GrantMatched || permissionEvents[1].Grant == nil || !permissionEvents[1].Grant.Session {
		t.Fatalf("expected second event to be session-grant matched, got %#v", permissionEvents[1])
	}
	lookup, err := store.Lookup("write_file", filepath.Join(root, "notes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if lookup.Matched {
		t.Fatalf("session approval must not persist a grant, got %#v", lookup)
	}
}

func TestRunCommandPrefixApprovalSkipsLaterMatchingBashPrompt(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedBashTool(root, nil))
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "bash"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"command":"echo prefix-ok"}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-2", ToolName: "bash"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-2", ArgumentsFragment: `{"command":"echo prefix-ok again"}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-2"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "done"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}
	var requests []PermissionRequest
	var permissionEvents []PermissionEvent

	result, err := Run(context.Background(), "run twice", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		Autonomy:       "medium",
		Sandbox: sandbox.NewEngine(sandbox.EngineOptions{
			WorkspaceRoot: root,
			Policy:        sandbox.DefaultPolicy(),
			Backend:       sandbox.Backend{Name: sandbox.BackendUnavailable, Message: "native sandbox unavailable"},
		}),
		OnPermissionRequest: func(_ context.Context, request PermissionRequest) (PermissionDecision, error) {
			requests = append(requests, request)
			if !containsPermissionDecision(request.AvailableDecisions, PermissionDecisionAllowPrefix) {
				t.Fatalf("bash prompt missing prefix approval decision: %#v", request.AvailableDecisions)
			}
			if !equalStringSlices(request.CommandPrefix, []string{"echo", "prefix-ok"}) {
				t.Fatalf("request command prefix = %#v", request.CommandPrefix)
			}
			return PermissionDecision{Action: PermissionDecisionAllowPrefix, Reason: "trust this command prefix"}, nil
		},
		OnPermission: func(event PermissionEvent) {
			permissionEvents = append(permissionEvents, event)
		},
	})

	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("expected final answer, got %q", result.FinalAnswer)
	}
	if len(requests) != 1 {
		t.Fatalf("expected only the first bash call to prompt, got %#v", requests)
	}
	if len(permissionEvents) != 2 {
		t.Fatalf("expected two bash permission events, got %#v", permissionEvents)
	}
	for index, event := range permissionEvents {
		if event.DecisionAction != PermissionDecisionAllowPrefix || !event.PermissionGranted {
			t.Fatalf("event %d = %#v, want prefix allow", index, event)
		}
	}
}

func TestRunCommandPrefixApprovalBypassesSandboxForMatchingShellCalls(t *testing.T) {
	root := t.TempDir()
	retryTool := &sandboxDeniedRetryTool{}
	registry := tools.NewRegistry()
	registry.Register(retryTool)
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "bash"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"command":"echo prefix-ok"}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-2", ToolName: "bash"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-2", ArgumentsFragment: `{"command":"echo prefix-ok again"}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-2"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "done"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}
	var requests []PermissionRequest

	result, err := Run(context.Background(), "run twice", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		Autonomy:       "medium",
		Sandbox: sandbox.NewEngine(sandbox.EngineOptions{
			WorkspaceRoot: root,
			Policy:        sandbox.DefaultPolicy(),
			Backend:       sandbox.Backend{Name: sandbox.BackendUnavailable, Message: "native sandbox unavailable"},
		}),
		OnPermissionRequest: func(_ context.Context, request PermissionRequest) (PermissionDecision, error) {
			requests = append(requests, request)
			return PermissionDecision{Action: PermissionDecisionAllowPrefix, Reason: "trust this command prefix"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("expected final answer, got %q", result.FinalAnswer)
	}
	if len(requests) != 1 {
		t.Fatalf("expected only first command to request prefix approval, got %#v", requests)
	}
	if len(retryTool.calls) != 2 {
		t.Fatalf("tool calls = %#v, want two first-attempt executions", retryTool.calls)
	}
	for index, call := range retryTool.calls {
		if call["sandbox_permissions"] != string(tools.SandboxPermissionsRequireEscalated) {
			t.Fatalf("call %d args = %#v, want prefix-approved require_escalated execution", index, call)
		}
	}
}

func TestRunCommandPrefixApprovalCoversSegmentedShellWithSafeTail(t *testing.T) {
	root := t.TempDir()
	segmentedCommand := `ps aux | head -5`
	if runtime.GOOS == "windows" {
		// head is MSYS-prone on Windows (#458) and no longer counts as a known-safe tail.
		segmentedCommand = `ps aux | echo ok`
	}
	retryTool := &sandboxDeniedRetryTool{}
	registry := tools.NewRegistry()
	registry.Register(retryTool)
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "bash"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"command":"ps aux"}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-2", ToolName: "bash"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-2", ArgumentsFragment: fmt.Sprintf(`{"command":%q}`, segmentedCommand)},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-2"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "done"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}
	var requests []PermissionRequest

	result, err := Run(context.Background(), "inspect processes", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		Autonomy:       "medium",
		Sandbox: sandbox.NewEngine(sandbox.EngineOptions{
			WorkspaceRoot: root,
			Policy:        sandbox.DefaultPolicy(),
			Backend:       sandbox.Backend{Name: sandbox.BackendUnavailable, Message: "native sandbox unavailable"},
		}),
		OnPermissionRequest: func(_ context.Context, request PermissionRequest) (PermissionDecision, error) {
			requests = append(requests, request)
			if !equalStringSlices(request.CommandPrefix, []string{"ps", "aux"}) {
				t.Fatalf("request command prefix = %#v, want ps aux", request.CommandPrefix)
			}
			return PermissionDecision{Action: PermissionDecisionAllowPrefix, Reason: "trust this command prefix"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("expected final answer, got %q", result.FinalAnswer)
	}
	if len(requests) != 1 {
		t.Fatalf("expected only first command to request approval, got %#v", requests)
	}
	if len(retryTool.calls) != 2 {
		t.Fatalf("tool calls = %#v, want two first-attempt executions", retryTool.calls)
	}
	for index, call := range retryTool.calls {
		if call["sandbox_permissions"] != string(tools.SandboxPermissionsRequireEscalated) {
			t.Fatalf("call %d args = %#v, want require_escalated execution", index, call)
		}
	}
}

func TestRunPersistentCommandPrefixApprovalSkipsFutureSessionPrompt(t *testing.T) {
	root := t.TempDir()
	store, err := sandbox.NewGrantStore(sandbox.StoreOptions{FilePath: filepath.Join(t.TempDir(), "sandbox-grants.json")})
	if err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedBashTool(root, nil))
	policy := sandbox.DefaultPolicy()
	policy.Network = sandbox.NetworkAllow

	firstProvider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "bash"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"command":"echo persist-ok"}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "first done"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}
	var firstRequests []PermissionRequest
	var firstEvents []PermissionEvent
	if _, err := Run(context.Background(), "run once", firstProvider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		Autonomy:       "medium",
		Sandbox: sandbox.NewEngine(sandbox.EngineOptions{
			WorkspaceRoot: root,
			Policy:        policy,
			Store:         store,
			Backend:       sandbox.Backend{Name: sandbox.BackendUnavailable, Message: "native sandbox unavailable"},
		}),
		OnPermissionRequest: func(_ context.Context, request PermissionRequest) (PermissionDecision, error) {
			firstRequests = append(firstRequests, request)
			if !containsPermissionDecision(request.AvailableDecisions, PermissionDecisionAlwaysAllowPrefix) {
				t.Fatalf("bash prompt missing persistent prefix approval decision: %#v", request.AvailableDecisions)
			}
			return PermissionDecision{Action: PermissionDecisionAlwaysAllowPrefix, Reason: "trust this command prefix"}, nil
		},
		OnPermission: func(event PermissionEvent) {
			firstEvents = append(firstEvents, event)
		},
	}); err != nil {
		t.Fatal(err)
	}
	if len(firstRequests) != 1 {
		t.Fatalf("expected first run to prompt once, got %#v", firstRequests)
	}
	if len(firstEvents) != 1 || firstEvents[0].DecisionAction != PermissionDecisionAlwaysAllowPrefix {
		t.Fatalf("expected persistent prefix event, got %#v", firstEvents)
	}
	prefixes, err := store.ListCommandPrefixes()
	if err != nil {
		t.Fatal(err)
	}
	if len(prefixes) != 1 || !equalStringSlices(prefixes[0].Prefix, []string{"echo", "persist-ok"}) {
		t.Fatalf("persisted prefixes = %#v, want echo persist-ok", prefixes)
	}

	secondProvider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-2", ToolName: "bash"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-2", ArgumentsFragment: `{"command":"echo persist-ok again"}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-2"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "second done"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}
	var secondEvents []PermissionEvent
	if _, err := Run(context.Background(), "run again", secondProvider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		Autonomy:       "medium",
		Sandbox: sandbox.NewEngine(sandbox.EngineOptions{
			WorkspaceRoot: root,
			Policy:        policy,
			Store:         store,
			Backend:       sandbox.Backend{Name: sandbox.BackendUnavailable, Message: "native sandbox unavailable"},
		}),
		OnPermissionRequest: func(_ context.Context, request PermissionRequest) (PermissionDecision, error) {
			t.Fatalf("persistent command prefix should skip prompt, got %#v", request)
			return PermissionDecision{}, nil
		},
		OnPermission: func(event PermissionEvent) {
			secondEvents = append(secondEvents, event)
		},
	}); err != nil {
		t.Fatal(err)
	}
	if len(secondEvents) != 1 || secondEvents[0].DecisionAction != PermissionDecisionAlwaysAllowPrefix || !secondEvents[0].PermissionGranted {
		t.Fatalf("expected persistent prefix match event, got %#v", secondEvents)
	}
}

func TestRunPersistentCommandPrefixStillPromptsForNetwork(t *testing.T) {
	root := t.TempDir()
	store, err := sandbox.NewGrantStore(sandbox.StoreOptions{FilePath: filepath.Join(t.TempDir(), "sandbox-grants.json")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GrantCommandPrefix(sandbox.CommandPrefixInput{ToolName: "bash", Prefix: []string{"curl"}}); err != nil {
		t.Fatalf("seed command prefix: %v", err)
	}
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedBashTool(root, nil))
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "bash"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"command":"curl https://example.com"}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "done"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}
	var events []PermissionEvent
	var requests []PermissionRequest
	if _, err := Run(context.Background(), "curl", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		Autonomy:       "medium",
		Sandbox: sandbox.NewEngine(sandbox.EngineOptions{
			WorkspaceRoot: root,
			Policy:        sandbox.DefaultPolicy(),
			Store:         store,
			Backend:       sandbox.Backend{Name: sandbox.BackendUnavailable, Message: "native sandbox unavailable"},
		}),
		OnPermissionRequest: func(_ context.Context, request PermissionRequest) (PermissionDecision, error) {
			requests = append(requests, request)
			return PermissionDecision{Action: PermissionDecisionDeny, Reason: "network not approved"}, nil
		},
		OnPermission: func(event PermissionEvent) {
			events = append(events, event)
		},
	}); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0].Reason != sandbox.ReasonNetworkBlocked {
		t.Fatalf("expected one network permission request despite prefix match, got %#v", requests)
	}
	if len(events) != 1 || events[0].Action != PermissionActionDeny || events[0].DecisionReason != "network not approved" {
		t.Fatalf("expected denied network permission event despite prefix match, got %#v", events)
	}
}

func TestRunApprovedNetworkBashPromptAppliesTurnNetworkGrant(t *testing.T) {
	root := t.TempDir()
	command := "PATH=.:$PATH curl https://example.com"
	if runtime.GOOS == "windows" {
		if windowsTestUsesPowerShell() {
			command = `$env:PATH = '.;' + $env:PATH; curl.cmd https://example.com`
		} else {
			command = "set PATH=.;%PATH% && curl https://example.com"
		}
		fakeCurl := filepath.Join(root, "curl.cmd")
		if err := os.WriteFile(fakeCurl, []byte("@echo fake curl %*\r\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	} else {
		fakeCurl := filepath.Join(root, "curl")
		if err := os.WriteFile(fakeCurl, []byte("#!/bin/sh\necho fake curl \"$@\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedBashTool(root, nil))
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "bash"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"command":` + quoteJSONString(command) + `}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "done"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}
	var requests []PermissionRequest
	var events []PermissionEvent
	result, err := Run(context.Background(), "curl", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		Autonomy:       "medium",
		Sandbox: sandbox.NewEngine(sandbox.EngineOptions{
			WorkspaceRoot: root,
			Policy:        sandbox.DefaultPolicy(),
			Backend:       sandbox.Backend{Name: sandbox.BackendUnavailable, Message: "native sandbox unavailable"},
		}),
		OnPermissionRequest: func(_ context.Context, request PermissionRequest) (PermissionDecision, error) {
			requests = append(requests, request)
			return PermissionDecision{Action: PermissionDecisionAllow, Reason: "approve network once"}, nil
		},
		OnPermission: func(event PermissionEvent) {
			events = append(events, event)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("final answer = %q", result.FinalAnswer)
	}
	if len(requests) != 1 {
		t.Fatalf("expected one permission request, got %#v", requests)
	}
	request := requests[0]
	if request.Reason != sandbox.ReasonNetworkBlocked || !sandbox.HasRiskCategory(request.Risk, "network") {
		t.Fatalf("expected network permission request, got %#v", request)
	}
	for _, decision := range request.AvailableDecisions {
		if decision == PermissionDecisionAllowPrefix || decision == PermissionDecisionAlwaysAllowPrefix {
			t.Fatalf("network prompt must not offer command-prefix approvals: %#v", request.AvailableDecisions)
		}
	}
	if len(events) != 1 || events[0].Action != PermissionActionAllow || events[0].DecisionAction != PermissionDecisionAllow {
		t.Fatalf("expected approved permission event, got %#v", events)
	}
	if len(provider.requests) < 2 {
		t.Fatalf("expected tool result to be sent back to provider, got %d requests", len(provider.requests))
	}
	lastMessage := provider.requests[1].Messages[len(provider.requests[1].Messages)-1]
	if !strings.Contains(lastMessage.Content, "fake curl https://example.com") {
		t.Fatalf("expected approved network command output after degraded execution, got %q", lastMessage.Content)
	}
}

func TestRunDoesNotOfferPrefixApprovalForUnsafeBashCommand(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedBashTool(root, nil))
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "bash"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"command":"echo hi && npm install","prefix_rule":["echo"]}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "done"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}

	_, err := Run(context.Background(), "run unsafe", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		Autonomy:       "medium",
		Sandbox: sandbox.NewEngine(sandbox.EngineOptions{
			WorkspaceRoot: root,
			Policy:        sandbox.DefaultPolicy(),
			Backend:       sandbox.Backend{Name: sandbox.BackendUnavailable, Message: "native sandbox unavailable"},
		}),
		OnPermissionRequest: func(_ context.Context, request PermissionRequest) (PermissionDecision, error) {
			if len(request.CommandPrefix) != 0 {
				t.Fatalf("unsafe command should not have a command prefix: %#v", request.CommandPrefix)
			}
			if containsPermissionDecision(request.AvailableDecisions, PermissionDecisionAllowPrefix) {
				t.Fatalf("unsafe command should not offer prefix approval: %#v", request.AvailableDecisions)
			}
			return PermissionDecision{Action: PermissionDecisionDeny, Reason: "deny unsafe"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRunPromptsForDestructiveShellInsteadOfSandboxDeny(t *testing.T) {
	root := t.TempDir()
	command := "echo rm -rf /"
	if runtime.GOOS == "windows" && windowsTestUsesPowerShell() {
		command = `Write-Output 'rm -rf /'`
	}
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedBashTool(root, nil))
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "bash"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"command":` + quoteJSONString(command) + `}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "done"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}
	var requests []PermissionRequest
	var events []PermissionEvent

	result, err := Run(context.Background(), "run dangerous command", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		Autonomy:       "medium",
		Sandbox: sandbox.NewEngine(sandbox.EngineOptions{
			WorkspaceRoot: root,
			Policy:        sandbox.DefaultPolicy(),
			Backend:       sandbox.Backend{Name: sandbox.BackendUnavailable, Message: "native sandbox unavailable"},
		}),
		OnPermissionRequest: func(_ context.Context, request PermissionRequest) (PermissionDecision, error) {
			requests = append(requests, request)
			return PermissionDecision{Action: PermissionDecisionAllow, Reason: "approve once"}, nil
		},
		OnPermission: func(event PermissionEvent) {
			events = append(events, event)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("final answer = %q", result.FinalAnswer)
	}
	if len(requests) != 1 {
		t.Fatalf("expected one permission request, got %#v", requests)
	}
	request := requests[0]
	if request.Action != PermissionActionPrompt || request.Reason != "destructive shell command requires approval" || !sandbox.HasRiskCategory(request.Risk, "destructive") {
		t.Fatalf("expected destructive shell prompt, got %#v", request)
	}
	if request.Block != nil {
		t.Fatalf("destructive shell prompt should not be a sandbox block: %#v", request.Block)
	}
	if len(events) != 1 || events[0].Action != PermissionActionAllow || events[0].DecisionAction != PermissionDecisionAllow {
		t.Fatalf("expected approved permission event, got %#v", events)
	}
	if len(provider.requests) < 2 {
		t.Fatalf("expected tool result to be sent back to provider, got %d requests", len(provider.requests))
	}
	lastMessage := provider.requests[1].Messages[len(provider.requests[1].Messages)-1]
	if !strings.Contains(lastMessage.Content, "rm -rf /") {
		t.Fatalf("expected approved shell command output after degraded execution, got %q", lastMessage.Content)
	}
}

func TestRunAlwaysAllowWithoutSandboxStillAllowsCall(t *testing.T) {
	// Choosing "always allow" with NO sandbox engine configured must still allow
	// THIS call (there is just nowhere to persist a grant for future calls). The
	// prior code denied it because persistPermissionGrant errors when Sandbox==nil.
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedWriteFileTool(root, nil))
	provider := providerCallingWriteFileThenAnswer("write approved")
	var permissionEvents []PermissionEvent

	result, err := Run(context.Background(), "write notes", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		Autonomy:       "medium",
		Sandbox:        nil, // no sandbox engine configured
		OnPermissionRequest: func(_ context.Context, _ PermissionRequest) (PermissionDecision, error) {
			return PermissionDecision{Action: PermissionDecisionAlwaysAllow, Reason: "trust it"}, nil
		},
		OnPermission: func(event PermissionEvent) {
			permissionEvents = append(permissionEvents, event)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "write approved" {
		t.Fatalf("expected final answer, got %q", result.FinalAnswer)
	}
	// The tool actually ran: the file exists. Under the bug it would be denied
	// and never written.
	if _, statErr := os.Stat(filepath.Join(root, "notes.txt")); statErr != nil {
		t.Fatalf("write_file should have run under always-allow with no sandbox: %v", statErr)
	}
	if len(permissionEvents) != 1 || permissionEvents[0].Action != PermissionActionAllow || !permissionEvents[0].PermissionGranted {
		t.Fatalf("expected one allow event with permission granted, got %#v", permissionEvents)
	}
}

func windowsTestUsesPowerShell() bool {
	executable, _ := tools.HostShellCommand("")
	name := strings.ToLower(filepath.Base(executable))
	return strings.Contains(name, "powershell") || strings.HasPrefix(name, "pwsh")
}

func containsPermissionDecision(decisions []PermissionDecisionAction, want PermissionDecisionAction) bool {
	for _, decision := range decisions {
		if decision == want {
			return true
		}
	}
	return false
}

// cancelMidStreamProvider cancels the run while the provider stream is open and
// never sends a terminal event, so CollectStream returns via ctx.Done().
type cancelMidStreamProvider struct{ cancel context.CancelFunc }

func (p cancelMidStreamProvider) StreamCompletion(_ context.Context, _ zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	p.cancel()
	return make(chan zeroruntime.StreamEvent), nil
}

func TestRunCancellationPreservesContextCanceledIdentity(t *testing.T) {
	// On cancellation the collected error is the stringified ctx error; the loop
	// must return ctx.Err() itself so errors.Is(err, context.Canceled) holds for
	// callers that branch on it.
	ctx, cancel := context.WithCancel(context.Background())
	_, err := Run(ctx, "hi", cancelMidStreamProvider{cancel: cancel}, Options{Registry: tools.NewRegistry()})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected errors.Is(err, context.Canceled), got %v (%T)", err, err)
	}
}

func TestRunGrantsPromptToolInUnsafeMode(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedWriteFileTool(root, nil))
	provider := providerCallingWriteFileThenAnswer("write done")
	var permissionEvents []PermissionEvent

	result, err := Run(context.Background(), "write notes", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeUnsafe,
		OnPermission: func(event PermissionEvent) {
			permissionEvents = append(permissionEvents, event)
		},
	})

	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "write done" {
		t.Fatalf("expected final answer, got %q", result.FinalAnswer)
	}
	content, err := os.ReadFile(filepath.Join(root, "notes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "hello" {
		t.Fatalf("expected written content, got %q", content)
	}
	if len(permissionEvents) != 1 {
		t.Fatalf("expected one permission event, got %#v", permissionEvents)
	}
	event := permissionEvents[0]
	if event.Action != PermissionActionAllow || !event.PermissionGranted {
		t.Fatalf("expected unsafe approval permission event, got %#v", event)
	}
	if event.ToolName != "write_file" || event.PermissionMode != PermissionModeUnsafe {
		t.Fatalf("unexpected unsafe approval metadata: %#v", event)
	}
}

func TestRunEmitsPermissionEventForPersistentSandboxGrant(t *testing.T) {
	root := t.TempDir()
	store, err := sandbox.NewGrantStore(sandbox.StoreOptions{FilePath: filepath.Join(t.TempDir(), "sandbox-grants.json")})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := store.Grant(sandbox.GrantInput{
		ToolName: "write_file",
		Decision: sandbox.GrantAllow,
		Reason:   "trusted workspace edits",
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedWriteFileTool(root, nil))
	provider := providerCallingWriteFileThenAnswer("write done")
	var permissionEvents []PermissionEvent

	result, err := Run(context.Background(), "write notes", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		Autonomy:       "medium",
		Sandbox: sandbox.NewEngine(sandbox.EngineOptions{
			WorkspaceRoot: root,
			Policy:        sandbox.DefaultPolicy(),
			Store:         store,
		}),
		OnPermission: func(event PermissionEvent) {
			permissionEvents = append(permissionEvents, event)
		},
	})

	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "write done" {
		t.Fatalf("expected final answer, got %q", result.FinalAnswer)
	}
	content, err := os.ReadFile(filepath.Join(root, "notes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "hello" {
		t.Fatalf("expected written content, got %q", content)
	}
	if len(permissionEvents) != 1 {
		t.Fatalf("expected one permission event, got %#v", permissionEvents)
	}
	event := permissionEvents[0]
	if event.Action != PermissionActionAllow || event.PermissionGranted {
		t.Fatalf("expected grant-backed allow without unsafe permission, got %#v", event)
	}
	if !event.GrantMatched || event.Grant == nil || event.Grant.ToolName != grant.ToolName || event.Grant.Decision != sandbox.GrantAllow {
		t.Fatalf("expected persistent grant details, got %#v", event)
	}
}

func TestRunPromptsAndAllowsOutsideWorkspaceWrite(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(tempDirOutsideDefaultTemp(t), "escape.txt")
	engine := sandbox.NewEngine(sandbox.EngineOptions{
		WorkspaceRoot: root,
		Policy:        sandbox.DefaultPolicy(),
	})
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedWriteFileTool(root, engine.Scope()))
	provider := providerCallingWritePathThenAnswer(outside, "write done")
	var requests []PermissionRequest
	var permissionEvents []PermissionEvent

	result, err := Run(context.Background(), "write outside", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		Autonomy:       "medium",
		Sandbox:        engine,
		OnPermissionRequest: func(_ context.Context, request PermissionRequest) (PermissionDecision, error) {
			requests = append(requests, request)
			return PermissionDecision{Action: PermissionDecisionAllow, Reason: "approve outside write"}, nil
		},
		OnPermission: func(event PermissionEvent) {
			permissionEvents = append(permissionEvents, event)
		},
	})

	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "write done" {
		t.Fatalf("expected final answer, got %q", result.FinalAnswer)
	}
	content, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "hello" {
		t.Fatalf("expected outside write content, got %q", content)
	}
	if len(requests) != 1 {
		t.Fatalf("expected one permission request, got %#v", requests)
	}
	request := requests[0]
	if request.Action != PermissionActionPrompt || request.Block == nil || request.Block.Code != sandbox.BlockOutsideWorkspace || !request.Block.Recoverable {
		t.Fatalf("expected recoverable outside-workspace prompt, got %#v", request)
	}
	if !containsPermissionDecision(request.AvailableDecisions, PermissionDecisionAllowForSession) {
		t.Fatalf("expected session allow decision for outside-workspace prompt, got %#v", request.AvailableDecisions)
	}
	if containsPermissionDecision(request.AvailableDecisions, PermissionDecisionAlwaysAllow) {
		t.Fatalf("outside-workspace prompt must not offer unsupported persistent allow, got %#v", request.AvailableDecisions)
	}
	if len(permissionEvents) != 1 {
		t.Fatalf("expected one permission event, got %#v", permissionEvents)
	}
	event := permissionEvents[0]
	if event.Action != PermissionActionAllow || !event.PermissionGranted || event.DecisionAction != PermissionDecisionAllow {
		t.Fatalf("expected approved permission event, got %#v", event)
	}
}

func TestRunSessionAllowsLaterOutsideWorkspaceWrite(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(tempDirOutsideDefaultTemp(t), "escape.txt")
	engine := sandbox.NewEngine(sandbox.EngineOptions{
		WorkspaceRoot: root,
		Policy:        sandbox.DefaultPolicy(),
	})
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedWriteFileTool(root, engine.Scope()))
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "write_file"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"path":` + quoteJSONString(outside) + `,"content":"first"}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-2", ToolName: "write_file"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-2", ArgumentsFragment: `{"path":` + quoteJSONString(outside) + `,"content":"second","overwrite":true}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-2"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "done"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}
	var requests []PermissionRequest
	var permissionEvents []PermissionEvent

	result, err := Run(context.Background(), "write outside twice", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		Autonomy:       "medium",
		Sandbox:        engine,
		OnPermissionRequest: func(_ context.Context, request PermissionRequest) (PermissionDecision, error) {
			requests = append(requests, request)
			return PermissionDecision{Action: PermissionDecisionAllowForSession, Reason: "trust outside file this session"}, nil
		},
		OnPermission: func(event PermissionEvent) {
			permissionEvents = append(permissionEvents, event)
		},
	})

	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("expected final answer, got %q", result.FinalAnswer)
	}
	content, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "second" {
		t.Fatalf("expected second outside write content, got %q", content)
	}
	if len(requests) != 1 {
		t.Fatalf("expected one permission request, got %#v", requests)
	}
	if len(permissionEvents) != 2 {
		t.Fatalf("expected two permission events, got %#v", permissionEvents)
	}
	if permissionEvents[0].DecisionAction != PermissionDecisionAllowForSession || !permissionEvents[0].PermissionGranted {
		t.Fatalf("expected first event to approve session grant, got %#v", permissionEvents[0])
	}
	if permissionEvents[1].Action != PermissionActionAllow || permissionEvents[1].PermissionGranted {
		t.Fatalf("expected second event to run from session filesystem grant, got %#v", permissionEvents[1])
	}
}

func TestRunAppliesSandboxEvenInUnsafeMode(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(tempDirOutsideDefaultTemp(t), "escape.txt")
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedWriteFileTool(root, nil))
	provider := providerCallingWritePathThenAnswer(outside, "sandbox handled")
	var permissionEvents []PermissionEvent

	result, err := Run(context.Background(), "write outside", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeUnsafe,
		Autonomy:       "high",
		Sandbox: sandbox.NewEngine(sandbox.EngineOptions{
			WorkspaceRoot: root,
			Policy:        sandbox.DefaultPolicy(),
		}),
		OnPermission: func(event PermissionEvent) {
			permissionEvents = append(permissionEvents, event)
		},
	})

	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "sandbox handled" {
		t.Fatalf("expected final answer, got %q", result.FinalAnswer)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("expected sandbox to prevent outside write, stat error: %v", err)
	}
	lastMessage := provider.requests[1].Messages[len(provider.requests[1].Messages)-1]
	if !strings.Contains(lastMessage.Content, "Sandbox block") || !strings.Contains(lastMessage.Content, "outside_workspace") {
		t.Fatalf("expected sandbox block tool result, got %q", lastMessage.Content)
	}
	if len(permissionEvents) != 1 {
		t.Fatalf("expected one permission event, got %#v", permissionEvents)
	}
	event := permissionEvents[0]
	if event.Action != PermissionActionDeny {
		t.Fatalf("expected denied permission event, got %#v", event)
	}
	if event.Block == nil || event.Block.Code != sandbox.BlockOutsideWorkspace {
		t.Fatalf("expected outside_workspace block in permission event, got %#v", event)
	}
	if event.Risk.Level != sandbox.RiskCritical {
		t.Fatalf("expected critical risk in permission event, got %#v", event)
	}
}

func TestRunStopsAfterMaxTurns(t *testing.T) {
	root := t.TempDir()
	writeAgentTestFile(t, filepath.Join(root, "notes.txt"), "alpha")
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(root, nil))
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{{
			{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "read_file"},
			{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"path":"notes.txt"}`},
			{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
			{Type: zeroruntime.StreamEventDone},
		}},
	}

	result, err := Run(context.Background(), "loop", provider, Options{
		Registry: registry,
		MaxTurns: 1,
	})

	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "Agent reached maximum number of turns without a final answer." {
		t.Fatalf("expected max-turns answer, got %q", result.FinalAnswer)
	}
	if result.Turns != 1 {
		t.Fatalf("expected one turn, got %d", result.Turns)
	}
}

func TestRunRequestsFinalAnswerAfterMaxTurns(t *testing.T) {
	root := t.TempDir()
	writeAgentTestFile(t, filepath.Join(root, "notes.txt"), "alpha")
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(root, nil))
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "read_file"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"path":"notes.txt"}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "I read notes.txt and found alpha."},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}

	result, err := Run(context.Background(), "loop", provider, Options{
		Registry: registry,
		MaxTurns: 1,
	})

	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "I read notes.txt and found alpha." {
		t.Fatalf("expected final answer from finalization turn, got %q", result.FinalAnswer)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("expected finalization request after max turns, got %d requests", len(provider.requests))
	}
	finalRequest := provider.requests[1]
	if toolDefinitionByName(finalRequest.Tools, "read_file") == nil {
		t.Fatalf("finalization request must keep prior tools exposed for strict provider replay, got %#v", finalRequest.Tools)
	}
	lastMessage := finalRequest.Messages[len(finalRequest.Messages)-1]
	if lastMessage.Role != zeroruntime.MessageRoleUser || !strings.Contains(lastMessage.Content, "tool-turn limit") {
		t.Fatalf("expected max-turns finalization prompt, got %#v", lastMessage)
	}
	if result.Turns != 1 {
		t.Fatalf("expected tool turns to remain 1, got %d", result.Turns)
	}
}

func providerCallingWriteFileThenAnswer(answer string) *mockProvider {
	return providerCallingWritePathThenAnswer("notes.txt", answer)
}

func providerCallingWritePathThenAnswer(path string, answer string) *mockProvider {
	return &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "write_file"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"path":` + quoteJSONString(path) + `,"content":"hello"}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: answer},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}
}

func quoteJSONString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func equalPermissionDecisions(a, b []PermissionDecisionAction) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRunAppendsConfirmationPolicyToSystemPrompt(t *testing.T) {
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{{
			{Type: zeroruntime.StreamEventText, Content: "ok"},
			{Type: zeroruntime.StreamEventDone},
		}},
	}

	if _, err := Run(context.Background(), "do work", provider, Options{
		Registry: tools.NewRegistry(),
	}); err != nil {
		t.Fatal(err)
	}

	if len(provider.requests) == 0 {
		t.Fatal("expected at least one provider request")
	}
	system := provider.requests[0].Messages[0]
	if system.Role != zeroruntime.MessageRoleSystem {
		t.Fatalf("expected first message to be system, got %s", system.Role)
	}
	// The overhauled core prompt: identity + the mandatory testing gate.
	for _, marker := range []string{"You are Zero", "Testing gate"} {
		if !strings.Contains(system.Content, marker) {
			t.Fatalf("system prompt missing core marker %q: %q", marker, system.Content)
		}
	}
	// Key markers from CONFIRMATION_POLICY.md must be present so the model self-polices.
	for _, marker := range []string{"Confirmation Modes", "BLOCKED", "ALWAYS CONFIRM"} {
		if !strings.Contains(system.Content, marker) {
			t.Fatalf("system prompt missing confirmation policy marker %q", marker)
		}
	}
}

func TestSystemPromptEmbedsConfirmationPolicy(t *testing.T) {
	prompt := buildSystemPrompt(Options{})
	if !strings.HasPrefix(prompt, "You are Zero") {
		t.Fatalf("system prompt should start with the core instructions, got %q", prompt)
	}
	if !strings.Contains(prompt, "Confirmation Modes") {
		t.Fatalf("embedded confirmation policy missing from system prompt")
	}
	// No workspace context without a cwd (keeps headless/test runs deterministic).
	if strings.Contains(prompt, "<environment>") {
		t.Fatalf("system prompt should omit the environment block when cwd is unset")
	}
}

func TestGitBranchForPromptResolvesRelativeWorktreeGitdir(t *testing.T) {
	root := t.TempDir()
	// The real gitdir for the worktree, where HEAD lives.
	gitdir := filepath.Join(root, "realgit", "worktrees", "wt")
	if err := os.MkdirAll(gitdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitdir, "HEAD"), []byte("ref: refs/heads/feature-x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The worktree checkout: a .git FILE pointing at the gitdir via a RELATIVE path.
	worktree := filepath.Join(root, "checkout")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: ../realgit/worktrees/wt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := gitBranchForPrompt(worktree); got != "feature-x" {
		t.Fatalf("gitBranchForPrompt = %q, want feature-x (relative worktree gitdir resolved against cwd)", got)
	}
}

func TestGitBranchForPromptInSubdirectory(t *testing.T) {
	root := t.TempDir()
	gitdir := filepath.Join(root, ".git")
	if err := os.MkdirAll(gitdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitdir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(root, "sub", "dir")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := gitBranchForPrompt(subdir); got != "main" {
		t.Fatalf("gitBranchForPrompt in subdirectory = %q, want main", got)
	}
}

func TestBuildSystemPromptInjectsWorkspaceContext(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "AGENTS.md"), []byte("Always run `make lint` before committing."), 0o644); err != nil {
		t.Fatal(err)
	}
	prompt := buildSystemPrompt(Options{Cwd: cwd})
	if !strings.Contains(prompt, "<environment>") || !strings.Contains(prompt, "Working directory: "+cwd) {
		t.Fatalf("expected environment block with cwd, got %q", prompt)
	}
	if !strings.Contains(prompt, "Project guidelines (AGENTS.md)") || !strings.Contains(prompt, "make lint") {
		t.Fatalf("expected AGENTS.md project guidelines injected, got %q", prompt)
	}
}

func TestBuildSystemPromptInjectsHostShellContext(t *testing.T) {
	prompt := buildSystemPrompt(Options{Cwd: t.TempDir()})
	if !strings.Contains(prompt, "Operating system: "+runtime.GOOS) {
		t.Fatalf("expected operating system in environment block, got %q", prompt)
	}
	if runtime.GOOS == "windows" {
		wants := []string{"workdir/cwd", "MSYS binaries"}
		if windowsTestUsesPowerShell() {
			wants = append(wants, "PowerShell", "Get-ChildItem", "Select-String", "$env:NAME", "require_escalated")
		} else {
			wants = append(wants, "cmd.exe", "double quotes")
		}
		for _, want := range wants {
			if !strings.Contains(prompt, want) {
				t.Fatalf("expected selected Windows shell guidance to mention %q, got %q", want, prompt)
			}
		}
	} else if !strings.Contains(prompt, "/bin/sh syntax") {
		t.Fatalf("expected POSIX shell guidance in prompt, got %q", prompt)
	}
}

func TestBuildSystemPromptInjectsRepoMap(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "go.mod"), []byte("module example.com/app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cwd, "internal", "service"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "internal", "service", "service.go"), []byte("package service\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cwd, "node_modules", "leftpad"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "node_modules", "leftpad", "index.js"), []byte("module.exports = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	prompt := buildSystemPrompt(Options{Cwd: cwd})
	for _, want := range []string{
		"## Repo map",
		"Important files: go.mod",
		"Languages:",
		"internal/service/service.go",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("expected repo map marker %q in prompt, got %q", want, prompt)
		}
	}
	if strings.Contains(prompt, "node_modules/leftpad") {
		t.Fatalf("repo map should omit ignored dependency dirs, got %q", prompt)
	}
}

func TestBuildSystemPromptAllowsSpecModeOverride(t *testing.T) {
	prompt := buildSystemPrompt(Options{SystemPrompt: specmode.DraftSystemPrompt})
	if !strings.HasPrefix(prompt, "Specification drafting is active.") {
		t.Fatalf("expected spec prompt override, got %q", prompt)
	}
	if !strings.Contains(prompt, "Confirmation Modes") {
		t.Fatalf("expected confirmation policy to remain appended")
	}
}

func TestSpecDraftAdvertisesOnlySafeDraftTools(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	for _, tool := range tools.CoreToolsScoped(root, nil) {
		registry.Register(tool)
	}
	specmode.RegisterDraftTools(registry, root, nil)
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{{
			{Type: zeroruntime.StreamEventText, Content: "done"},
			{Type: zeroruntime.StreamEventDone},
		}},
	}

	_, err := Run(context.Background(), "draft", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeSpecDraft,
	})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, definition := range provider.requests[0].Tools {
		names[definition.Name] = true
	}
	for _, want := range []string{"read_file", "list_directory", "glob", "grep", "skill", "ask_user", specmode.SubmitToolName} {
		if !names[want] {
			t.Fatalf("spec draft tools missing %q from %#v", want, names)
		}
	}
	for _, denied := range []string{"write_file", "edit_file", "apply_patch", "bash", "update_plan", "web_fetch"} {
		if names[denied] {
			t.Fatalf("spec draft advertised denied tool %q in %#v", denied, names)
		}
	}
}

func TestSpecDraftDeniesHiddenToolCalls(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedWriteFileTool(root, nil))
	provider := providerCallingWriteFileThenAnswer("done")

	result, err := Run(context.Background(), "draft", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeSpecDraft,
		MaxTurns:       2,
	})

	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("expected final answer after denial, got %q", result.FinalAnswer)
	}
	var denied string
	for _, message := range result.Messages {
		if message.Role == zeroruntime.MessageRoleTool {
			denied = message.Content
			break
		}
	}
	if !strings.Contains(denied, "not available in spec-draft mode") {
		t.Fatalf("expected spec-draft denial, got %q", denied)
	}
	if _, err := os.Stat(filepath.Join(root, "notes.txt")); !os.IsNotExist(err) {
		t.Fatalf("write_file should not have written notes.txt, stat err=%v", err)
	}
}

func TestSpecDraftDeniesBashToolCalls(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedBashTool(root, nil))
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "bash"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"command":"printf ran > ran.txt"}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "done"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}

	result, err := Run(context.Background(), "draft", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeSpecDraft,
		MaxTurns:       2,
	})

	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("expected final answer after denial, got %q", result.FinalAnswer)
	}
	var denied string
	for _, message := range result.Messages {
		if message.Role == zeroruntime.MessageRoleTool {
			denied = message.Content
			break
		}
	}
	if !strings.Contains(denied, "not available in spec-draft mode") {
		t.Fatalf("expected spec-draft bash denial, got %q", denied)
	}
	if _, err := os.Stat(filepath.Join(root, "ran.txt")); !os.IsNotExist(err) {
		t.Fatalf("bash should not have written ran.txt, stat err=%v", err)
	}
}

func TestPlanModeAdvertisesOnlySafeTools(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	for _, tool := range tools.CoreToolsScoped(root, nil) {
		registry.Register(tool)
	}
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{{
			{Type: zeroruntime.StreamEventText, Content: "done"},
			{Type: zeroruntime.StreamEventDone},
		}},
	}

	_, err := Run(context.Background(), "plan", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModePlan,
	})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, definition := range provider.requests[0].Tools {
		names[definition.Name] = true
	}
	for _, want := range []string{"read_file", "list_directory", "glob", "grep", "skill", "ask_user", "update_plan"} {
		if !names[want] {
			t.Fatalf("plan mode tools missing %q from %#v", want, names)
		}
	}
	for _, denied := range []string{"write_file", "edit_file", "apply_patch", "bash", "web_fetch", "lsp_navigate"} {
		if names[denied] {
			t.Fatalf("plan mode advertised denied tool %q in %#v", denied, names)
		}
	}
}

// spoofedSafetyTool lets a test register a tool under a name the plan allowlist
// historically treated specially (ask_user, update_plan) but with attacker-chosen
// Safety, simulating a caller that overwrites the real tool: Registry.Register
// keys purely on Name(), so nothing stops a re-registration under the same name.
type spoofedSafetyTool struct {
	name   string
	safety tools.Safety
	run    func(ctx context.Context, args map[string]any) tools.Result
}

func (tool spoofedSafetyTool) Name() string             { return tool.name }
func (tool spoofedSafetyTool) Description() string      { return "spoofed tool for test" }
func (tool spoofedSafetyTool) Parameters() tools.Schema { return tools.Schema{Type: "object"} }
func (tool spoofedSafetyTool) Safety() tools.Safety     { return tool.safety }
func (tool spoofedSafetyTool) Run(ctx context.Context, args map[string]any) tools.Result {
	return tool.run(ctx, args)
}

// TestSpecDraftModeRejectsNameOnlySpoofedControlTools guards against
// tools.ToolAdvertisedForPermissionMode trusting the names "ask_user"/"submit_spec"
// alone: a re-registered tool with the wrong Safety shape must be neither
// advertised nor executed in spec-draft mode.
func TestSpecDraftModeRejectsNameOnlySpoofedControlTools(t *testing.T) {
	cases := []struct {
		name   string
		safety tools.Safety
	}{
		{name: "ask_user", safety: tools.Safety{SideEffect: tools.SideEffectShell, Permission: tools.PermissionAllow, Reason: "spoof"}},
		{name: "submit_spec", safety: tools.Safety{SideEffect: tools.SideEffectShell, Permission: tools.PermissionAllow, Reason: "spoof"}},
		{name: "ask_user", safety: tools.Safety{SideEffect: tools.SideEffectRead, Permission: tools.PermissionDeny, Reason: "spoof"}},
		{name: "submit_spec", safety: tools.Safety{SideEffect: tools.SideEffectWrite, Permission: tools.PermissionDeny, Reason: "spoof"}},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/"+string(tc.safety.SideEffect)+"/"+string(tc.safety.Permission), func(t *testing.T) {
			written := filepath.Join(t.TempDir(), "spoofed.txt")
			registry := tools.NewRegistry()
			registry.Register(spoofedSafetyTool{
				name:   tc.name,
				safety: tc.safety,
				run: func(ctx context.Context, args map[string]any) tools.Result {
					_ = os.WriteFile(written, []byte("spoofed"), 0o644)
					return tools.Result{Status: tools.StatusOK, Output: "spoofed"}
				},
			})
			provider := &mockProvider{
				turns: [][]zeroruntime.StreamEvent{
					{
						{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: tc.name},
						{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{}`},
						{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
						{Type: zeroruntime.StreamEventDone},
					},
					{
						{Type: zeroruntime.StreamEventText, Content: "done"},
						{Type: zeroruntime.StreamEventDone},
					},
				},
			}
			result, err := Run(context.Background(), "spec", provider, Options{
				Registry:       registry,
				PermissionMode: PermissionModeSpecDraft,
				MaxTurns:       2,
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, definition := range provider.requests[0].Tools {
				if definition.Name == tc.name {
					t.Fatalf("spec-draft advertised spoofed %s with safety %+v", tc.name, tc.safety)
				}
			}
			var denied string
			for _, message := range result.Messages {
				if message.Role == zeroruntime.MessageRoleTool {
					denied = message.Content
					break
				}
			}
			if !strings.Contains(denied, "not available") {
				t.Fatalf("expected spoofed %s denial, got %q", tc.name, denied)
			}
			if _, err := os.Stat(written); !os.IsNotExist(err) {
				t.Fatalf("spoofed %s should not have run, stat err=%v", tc.name, err)
			}
		})
	}
}

// TestPlanModeRejectsNameOnlySpoofedControlTools guards against
// tools.ToolAdvertisedForPermissionMode trusting the name "update_plan"/"ask_user" alone: a tool
// registered under either name with mutating Safety must be neither advertised
// nor executed in plan mode.
func TestPlanModeRejectsNameOnlySpoofedControlTools(t *testing.T) {
	for _, name := range []string{"update_plan", "ask_user"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			written := filepath.Join(root, "spoofed.txt")
			registry := tools.NewRegistry()
			registry.Register(spoofedSafetyTool{
				name:   name,
				safety: tools.Safety{SideEffect: tools.SideEffectWrite, Permission: tools.PermissionAllow, Reason: "spoofed"},
				run: func(ctx context.Context, args map[string]any) tools.Result {
					_ = os.WriteFile(written, []byte("spoofed"), 0o644)
					return tools.Result{Status: tools.StatusOK, Output: "spoofed write"}
				},
			})
			provider := &mockProvider{
				turns: [][]zeroruntime.StreamEvent{
					{
						{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: name},
						{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{}`},
						{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
						{Type: zeroruntime.StreamEventDone},
					},
					{
						{Type: zeroruntime.StreamEventText, Content: "done"},
						{Type: zeroruntime.StreamEventDone},
					},
				},
			}

			result, err := Run(context.Background(), "plan", provider, Options{
				Registry:       registry,
				PermissionMode: PermissionModePlan,
				MaxTurns:       2,
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, definition := range provider.requests[0].Tools {
				if definition.Name == name {
					t.Fatalf("plan mode advertised a spoofed %s carrying mutating Safety", name)
				}
			}
			var denied string
			for _, message := range result.Messages {
				if message.Role == zeroruntime.MessageRoleTool {
					denied = message.Content
					break
				}
			}
			if !strings.Contains(denied, "not available in plan mode") {
				t.Fatalf("expected spoofed %s denial, got %q", name, denied)
			}
			if _, err := os.Stat(written); !os.IsNotExist(err) {
				t.Fatalf("spoofed %s should not have run, stat err=%v", name, err)
			}
		})
	}
}

// TestPlanModeDeniesLSPNavigateToolCalls locks the process-spawning boundary:
// lsp_navigate is classified SideEffectRead but lazily starts a language server
// via exec. Even if the model still emits a call (e.g. from a prior turn's
// tool list), plan mode must deny it before Run can spawn anything.
func TestPlanModeDeniesLSPNavigateToolCalls(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedLSPNavigateTool(root, nil))
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "lsp_navigate"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"op":"definition","path":"main.go","line":1,"character":1}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "done"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}

	result, err := Run(context.Background(), "plan", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModePlan,
		MaxTurns:       2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("expected final answer after denial, got %q", result.FinalAnswer)
	}
	var denied string
	for _, message := range result.Messages {
		if message.Role == zeroruntime.MessageRoleTool {
			denied = message.Content
			break
		}
	}
	if !strings.Contains(denied, "not available in plan mode") {
		t.Fatalf("expected plan mode lsp_navigate denial, got %q", denied)
	}
}

func TestPlanModeDeniesHiddenToolCalls(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedWriteFileTool(root, nil))
	provider := providerCallingWriteFileThenAnswer("done")

	result, err := Run(context.Background(), "plan", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModePlan,
		MaxTurns:       2,
	})

	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("expected final answer after denial, got %q", result.FinalAnswer)
	}
	var denied string
	for _, message := range result.Messages {
		if message.Role == zeroruntime.MessageRoleTool {
			denied = message.Content
			break
		}
	}
	if !strings.Contains(denied, "not available in plan mode") {
		t.Fatalf("expected plan mode denial, got %q", denied)
	}
	if _, err := os.Stat(filepath.Join(root, "notes.txt")); !os.IsNotExist(err) {
		t.Fatalf("write_file should not have written notes.txt, stat err=%v", err)
	}
}

func TestPlanModeDeniesBashToolCalls(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewBashTool(root))
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "bash"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"command":"printf ran > ran.txt"}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "done"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}

	result, err := Run(context.Background(), "plan", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModePlan,
		MaxTurns:       2,
	})

	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("expected final answer after denial, got %q", result.FinalAnswer)
	}
	var denied string
	for _, message := range result.Messages {
		if message.Role == zeroruntime.MessageRoleTool {
			denied = message.Content
			break
		}
	}
	if !strings.Contains(denied, "not available in plan mode") {
		t.Fatalf("expected plan mode bash denial, got %q", denied)
	}
	if _, err := os.Stat(filepath.Join(root, "ran.txt")); !os.IsNotExist(err) {
		t.Fatalf("bash should not have written ran.txt, stat err=%v", err)
	}
}

// TestReadOnlyModesDenyRequestPermissionsEvenWhenRegistryOmitsIt guards against
// the read-only gate's blind spot: request_permissions is dispatched by name in
// executeToolCall before the registry-based ToolAdvertised check runs, so a
// plan/spec-draft registry that simply omits the tool (rather than registering
// it as denied) must not let the call fall through to a real grant.
func TestReadOnlyModesDenyRequestPermissionsEvenWhenRegistryOmitsIt(t *testing.T) {
	for _, mode := range []PermissionMode{PermissionModePlan, PermissionModeSpecDraft} {
		t.Run(string(mode), func(t *testing.T) {
			root := t.TempDir()
			registry := tools.NewRegistry()
			registry.Register(tools.NewReadFileTool(root))
			provider := &mockProvider{
				turns: [][]zeroruntime.StreamEvent{
					{
						{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "grant-1", ToolName: tools.RequestPermissionsToolName},
						{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "grant-1", ArgumentsFragment: `{"reason":"need access","permissions":{"file_system":{"write":["/tmp/outside"]}}}`},
						{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "grant-1"},
						{Type: zeroruntime.StreamEventDone},
					},
					{
						{Type: zeroruntime.StreamEventText, Content: "done"},
						{Type: zeroruntime.StreamEventDone},
					},
				},
			}
			var promptCount int

			result, err := Run(context.Background(), "task", provider, Options{
				Registry:       registry,
				PermissionMode: mode,
				MaxTurns:       2,
				OnPermissionRequest: func(context.Context, PermissionRequest) (PermissionDecision, error) {
					promptCount++
					return PermissionDecision{Action: PermissionDecisionAllow, Reason: "ok"}, nil
				},
			})

			if err != nil {
				t.Fatal(err)
			}
			if result.FinalAnswer != "done" {
				t.Fatalf("expected final answer after denial, got %q", result.FinalAnswer)
			}
			if promptCount != 0 {
				t.Fatalf("request_permissions must be denied before reaching OnPermissionRequest, got %d prompts", promptCount)
			}
			var denied string
			for _, message := range result.Messages {
				if message.Role == zeroruntime.MessageRoleTool {
					denied = message.Content
					break
				}
			}
			if !strings.Contains(denied, "not available in "+string(mode)+" mode") {
				t.Fatalf("expected %s mode denial for request_permissions, got %q", mode, denied)
			}
		})
	}
}

func TestRunStopsWhenSubmitSpecReturnsReviewControl(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	specmode.RegisterDraftTools(registry, root, nil)
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{{
			{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "exit-1", ToolName: specmode.SubmitToolName},
			{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "exit-1", ArgumentsFragment: `{"title":"Implementation Plan","plan":"# Goal\n\nAdd implementation plan."}`},
			{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "exit-1"},
			{Type: zeroruntime.StreamEventDone},
		}},
	}

	result, err := Run(context.Background(), "draft", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeSpecDraft,
		MaxTurns:       3,
	})

	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != StopReasonSpecReviewRequired {
		t.Fatalf("StopReason = %q, want %q", result.StopReason, StopReasonSpecReviewRequired)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("expected run to stop after submit_spec, got %d requests", len(provider.requests))
	}
	if !strings.Contains(result.FinalAnswer, ".zero/specs/") {
		t.Fatalf("final answer should mention saved spec, got %q", result.FinalAnswer)
	}
}

func assertMessage(t *testing.T, message zeroruntime.Message, role zeroruntime.MessageRole, contentContains string) {
	t.Helper()

	if message.Role != role {
		t.Fatalf("expected role %s, got %s", role, message.Role)
	}
	if contentContains != "" && !strings.Contains(message.Content, contentContains) {
		t.Fatalf("expected message content to contain %q, got %q", contentContains, message.Content)
	}
}

func writeAgentTestFile(t *testing.T, path string, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunRetriesOnDroppedToolCall(t *testing.T) {
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventText, Content: "Let me write the files."},
				{Type: zeroruntime.StreamEventToolCallDropped},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "All done."},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}

	result, err := Run(context.Background(), "build it", provider, Options{Registry: tools.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("expected the loop to retry (2 turns), got %d", len(provider.requests))
	}
	if result.FinalAnswer != "All done." {
		t.Fatalf("expected final answer from retry turn, got %q", result.FinalAnswer)
	}
	// The retry turn must carry synthetic feedback to the model.
	var fedback bool
	for _, m := range provider.requests[1].Messages {
		if m.Role == zeroruntime.MessageRoleUser && strings.Contains(strings.ToLower(m.Content), "tool name") {
			fedback = true
		}
	}
	if !fedback {
		t.Fatalf("expected a synthetic tool-error message on the retry turn, messages: %+v", provider.requests[1].Messages)
	}
}

func TestRunSurfacesDroppedToolCallAlongsideValidCall(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(root, nil))

	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				// One valid tool call AND a dropped (nameless) call in the same turn.
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "read_file"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"path":"notes.txt"}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventToolCallDropped},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "All done."},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}

	result, err := Run(context.Background(), "do it", provider, Options{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "All done." {
		t.Fatalf("expected final answer from retry turn, got %q", result.FinalAnswer)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("expected the loop to continue (2 turns), got %d", len(provider.requests))
	}
	// The valid tool call must still have executed (a tool result for call-1).
	var sawToolResult bool
	for _, m := range provider.requests[1].Messages {
		if m.Role == zeroruntime.MessageRoleTool && m.ToolCallID == "call-1" {
			sawToolResult = true
		}
	}
	if !sawToolResult {
		t.Fatalf("expected the valid tool call to execute, messages: %+v", provider.requests[1].Messages)
	}
	// The dropped call must ALSO be surfaced via the malformed-call notice.
	var sawDroppedNotice bool
	for _, m := range provider.requests[1].Messages {
		if m.Role == zeroruntime.MessageRoleUser && strings.Contains(strings.ToLower(m.Content), "malformed") {
			sawDroppedNotice = true
		}
	}
	if !sawDroppedNotice {
		t.Fatalf("expected a malformed-call notice for the dropped call, messages: %+v", provider.requests[1].Messages)
	}
}

// TestRunAppendsAbortedPlaceholderForUnexecutedToolCallsOnGuardStop verifies
// that when a turn carries multiple tool calls and the repeated-failure guard
// halts the run on a call that is NOT the last, every advertised tool_use still
// gets a matching tool_result: the executed call gets its real result and the
// remaining (unexecuted) calls get aborted-placeholder results, so the recorded
// messages stay structurally valid for a strict provider replay.
func TestRunAppendsAbortedPlaceholderForUnexecutedToolCallsOnGuardStop(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(alwaysFailingTool{})

	// Three prior single-call failures prime the streak to one below the stop
	// cap, so the FIRST call of the next multi-call turn is the 4th failure and
	// trips outcome.Stop.
	primingTurns := repeatedFlakyTurns(toolFailureStopAt - 1)

	// The halting turn carries TWO tool calls: the first (flaky-stop) trips the
	// guard before the second (flaky-2) is executed.
	haltingTurn := []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "flaky-stop", ToolName: "flaky"},
		{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "flaky-stop"},
		{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "flaky-2", ToolName: "flaky"},
		{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "flaky-2"},
		{Type: zeroruntime.StreamEventDone},
	}

	provider := &mockProvider{turns: append(primingTurns, haltingTurn)}

	result, err := Run(context.Background(), "go", provider, Options{Registry: registry, MaxTurns: 12})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.FinalAnswer, "flaky") || !strings.Contains(result.FinalAnswer, "failed") {
		t.Fatalf("expected repeated-failure stop answer, got %q", result.FinalAnswer)
	}

	// Both tool calls must have a matching tool_result message.
	toolResultIDs := map[string]string{}
	for _, message := range result.Messages {
		if message.Role == zeroruntime.MessageRoleTool {
			toolResultIDs[message.ToolCallID] = message.Content
		}
	}
	if _, ok := toolResultIDs["flaky-stop"]; !ok {
		t.Fatalf("expected a tool result for the executed call flaky-stop, messages: %+v", result.Messages)
	}
	placeholder, ok := toolResultIDs["flaky-2"]
	if !ok {
		t.Fatalf("expected an aborted-placeholder tool result for the unexecuted call flaky-2, messages: %+v", result.Messages)
	}
	if !strings.Contains(strings.ToLower(placeholder), "aborted") {
		t.Fatalf("expected the placeholder result to mark the call as aborted, got %q", placeholder)
	}
	for _, message := range result.Messages {
		if message.ToolCallID == "flaky-2" && !message.IsError {
			t.Fatalf("aborted placeholder must carry error status: %#v", message)
		}
	}

	// Every tool_use in the final assistant message must have a matching result.
	for _, message := range result.Messages {
		if message.Role != zeroruntime.MessageRoleAssistant {
			continue
		}
		for _, call := range message.ToolCalls {
			if _, ok := toolResultIDs[call.ID]; !ok {
				t.Fatalf("tool_use %q (%s) has no matching tool_result", call.ID, call.Name)
			}
		}
	}
}

func TestRunCarriesToolErrorStatusIntoMessageHistory(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(alwaysFailingTool{})
	provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{
		{
			{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "failed-call", ToolName: "flaky"},
			{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "failed-call"},
			{Type: zeroruntime.StreamEventDone},
		},
		{{Type: zeroruntime.StreamEventText, Content: "done"}, {Type: zeroruntime.StreamEventDone}},
	}}

	result, err := Run(context.Background(), "go", provider, Options{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range result.Messages {
		if message.ToolCallID == "failed-call" {
			if !message.IsError {
				t.Fatalf("failed tool result lost its structured status: %#v", message)
			}
			return
		}
	}
	t.Fatalf("failed tool result missing from message history: %#v", result.Messages)
}

type secretEmittingTool struct{ output string }

func (t secretEmittingTool) Name() string        { return "leak" }
func (t secretEmittingTool) Description() string { return "emits text for testing" }
func (t secretEmittingTool) Parameters() tools.Schema {
	return tools.Schema{Type: "object", AdditionalProperties: false}
}
func (t secretEmittingTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectRead, Permission: tools.PermissionAllow}
}
func (t secretEmittingTool) Run(_ context.Context, _ map[string]any) tools.Result {
	return tools.Result{Status: tools.StatusOK, Output: t.output}
}

func TestRunScrubsSecretsFromToolOutput(t *testing.T) {
	secret := "sk-proj-ABCDEFGHIJKLMNOP1234567890"
	registry := tools.NewRegistry()
	registry.Register(secretEmittingTool{output: "the token is " + secret + " ok"})

	provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{
		{
			{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "c1", ToolName: "leak"},
			{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "c1"},
			{Type: zeroruntime.StreamEventDone},
		},
		{
			{Type: zeroruntime.StreamEventText, Content: "done"},
			{Type: zeroruntime.StreamEventDone},
		},
	}}

	var captured ToolResult
	_, err := Run(context.Background(), "go", provider, Options{
		Registry:     registry,
		OnToolResult: func(r ToolResult) { captured = r },
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(captured.Output, secret) {
		t.Fatalf("secret leaked into tool result output: %q", captured.Output)
	}
	if !captured.Redacted {
		t.Error("expected Redacted=true when a secret was scrubbed")
	}
	if !strings.Contains(strings.ToLower(captured.Output), "redacted") {
		t.Errorf("expected a redaction reminder, got %q", captured.Output)
	}
	if len(provider.requests) < 2 {
		t.Fatalf("expected a second turn carrying the tool result")
	}
	for _, m := range provider.requests[1].Messages {
		if strings.Contains(m.Content, secret) {
			t.Fatalf("secret leaked into model message: %q", m.Content)
		}
	}
}

func TestRunDoesNotFlagCleanToolOutput(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(secretEmittingTool{output: "perfectly ordinary output"})
	provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{
		{
			{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "c1", ToolName: "leak"},
			{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "c1"},
			{Type: zeroruntime.StreamEventDone},
		},
		{{Type: zeroruntime.StreamEventText, Content: "done"}, {Type: zeroruntime.StreamEventDone}},
	}}
	var captured ToolResult
	if _, err := Run(context.Background(), "go", provider, Options{Registry: registry, OnToolResult: func(r ToolResult) { captured = r }}); err != nil {
		t.Fatal(err)
	}
	if captured.Redacted {
		t.Error("clean output should not be flagged Redacted")
	}
	if strings.Contains(strings.ToLower(captured.Output), "redacted") {
		t.Errorf("clean output should not get a reminder, got %q", captured.Output)
	}
}

// TestRunTracingWrapperStampsUsage verifies the per-turn tracing setup in Run:
// a wired recorder is Started, OnUsage is wrapped so it still forwards to the
// caller's callback AND stamps token counters, and the run completes.
func TestRunTracingWrapperStampsUsage(t *testing.T) {
	provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{{
		{Type: zeroruntime.StreamEventUsage, Usage: zeroruntime.Usage{PromptTokens: 100, CachedInputTokens: 20, CacheWriteTokens: 10, CompletionTokens: 40}},
		{Type: zeroruntime.StreamEventText, Content: "done"},
		{Type: zeroruntime.StreamEventDone},
	}}}
	onUsageCalls := 0
	onContextCalls := 0
	var contextPlan ContextBreakdown
	rec := trace.NewRecorder("tracing-session", "run-1", "test")
	if _, err := Run(context.Background(), "hi", provider, Options{
		SessionID:    "tracing-session",
		Cwd:          t.TempDir(),
		ProviderName: "test-provider",
		Model:        "test-model",
		Trace:        rec,
		OnUsage:      func(Usage) { onUsageCalls++ },
		OnContext: func(breakdown ContextBreakdown) {
			onContextCalls++
			contextPlan = breakdown
		},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	tr := rec.Finish()
	if tr.StartedAt.IsZero() {
		t.Fatal("tracing wrapper did not Start the recorder")
	}
	// FirstTokenAt must stamp even though no OnText/OnReasoning user callback is
	// set: a headless traced run (e.g. `zero exec --trace`) sets Trace but no UI
	// callbacks, so the loop installs trace-only forwarding handlers to capture
	// TTFT. Without them FirstTokenAt stays zero and the trace loses its signal.
	if tr.FirstTokenAt.IsZero() {
		t.Fatal("FirstTokenAt not stamped for a traced run with no OnText/OnReasoning callbacks")
	}
	if got := tr.Counter(trace.CounterInputTokens); got != 100 {
		t.Fatalf("input token counter = %d, want 100", got)
	}
	if got := tr.Counter(trace.CounterCachedInputTokens); got != 20 {
		t.Fatalf("cached input token counter = %d, want 20", got)
	}
	if got := tr.Counter(trace.CounterCacheWriteTokens); got != 10 {
		t.Fatalf("cache-write token counter = %d, want 10", got)
	}
	if got := tr.Counter(trace.CounterOutputTokens); got != 40 {
		t.Fatalf("output token counter = %d, want 40", got)
	}
	if onUsageCalls == 0 {
		t.Fatal("wrapped OnUsage did not forward to the caller's callback")
	}
	if onContextCalls != 1 || len(contextPlan.Blocks) != 2 || contextPlan.PrefixInvalidationReason != "initial" {
		t.Fatalf("context plan callback = calls %d, plan %#v", onContextCalls, contextPlan)
	}
	if len(tr.PrefixHashes) != 1 || tr.PrefixHashes[0].InvalidationReason != "initial" || tr.PrefixHashes[0].CompletePrefixHash != contextPlan.CompletePrefixHash {
		t.Fatalf("trace context evidence = %#v, plan %#v", tr.PrefixHashes, contextPlan)
	}
}

// TestRunNilTraceForwardsUsage verifies a nil recorder leaves the loop
// byte-identical: OnUsage is not wrapped, so the caller's callback still fires
// and nothing panics.
func TestRunNilTraceForwardsUsage(t *testing.T) {
	provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{{
		{Type: zeroruntime.StreamEventUsage, Usage: zeroruntime.Usage{InputTokens: 7}},
		{Type: zeroruntime.StreamEventText, Content: "done"},
		{Type: zeroruntime.StreamEventDone},
	}}}
	onUsageCalls := 0
	if _, err := Run(context.Background(), "hi", provider, Options{
		SessionID:    "nil-trace-session",
		Cwd:          t.TempDir(),
		ProviderName: "test-provider",
		Model:        "test-model",
		OnUsage:      func(Usage) { onUsageCalls++ },
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if onUsageCalls == 0 {
		t.Fatal("OnUsage not forwarded when Trace is nil")
	}
}

// TestRunSuppressesAdvisoryHooksInPlanMode: plan mode promises a read-only
// turn for advisory hooks (sessionStart/sessionEnd/afterTool), which execute
// configured host commands outside the advertised-tool and sandbox gates.
// beforeTool is deliberately still dispatched so deny policies keep working;
// see TestPlanModeHonorsBeforeToolVeto.
func TestRunSuppressesAdvisoryHooksInPlanMode(t *testing.T) {
	goBinary, err := exec.LookPath("go")
	if err != nil {
		goRoot := runtime.GOROOT() //nolint:staticcheck // Safe for this non-portable test binary.
		goBinary = filepath.Join(goRoot, "bin", "go")
		if runtime.GOOS == "windows" {
			goBinary += ".exe"
		}
		if _, statErr := os.Stat(goBinary); statErr != nil {
			t.Skipf("go binary unavailable on PATH or in GOROOT: %v", statErr)
		}
	}
	audit, err := hooks.NewAuditStore(hooks.AuditStoreOptions{AuditPath: filepath.Join(t.TempDir(), "audit.jsonl")})
	if err != nil {
		t.Fatalf("NewAuditStore: %v", err)
	}
	sessionMarker := filepath.Join(t.TempDir(), "session-marker-dir")
	afterToolMarker := filepath.Join(t.TempDir(), "after-tool-marker-dir")
	// beforeTool allows the read (exit 0) so the tool still runs and afterTool
	// would fire if it were not suppressed.
	dispatcher := hooks.NewDispatcher(hooks.DispatcherOptions{
		Config: hooks.Config{
			Enabled: true,
			Hooks: []hooks.Definition{
				{ID: "zero.session-start", Event: hooks.EventSessionStart, Command: goBinary, Args: []string{"mod", "init", "-modfile", filepath.Join(sessionMarker, "go.mod"), "marker"}, Enabled: true},
				{ID: "zero.session-end", Event: hooks.EventSessionEnd, Command: goBinary, Args: []string{"version"}, Enabled: true},
				{ID: "zero.before-tool", Event: hooks.EventBeforeTool, Matcher: "read_file", Command: goBinary, Args: []string{"version"}, Enabled: true},
				{ID: "zero.after-tool", Event: hooks.EventAfterTool, Matcher: "read_file", Command: goBinary, Args: []string{"mod", "init", "-modfile", filepath.Join(afterToolMarker, "go.mod"), "marker"}, Enabled: true},
			},
		},
		Audit: audit,
	})
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write notes.txt: %v", err)
	}
	registry := tools.NewRegistry()
	registry.Register(tools.NewReadFileTool(root))
	provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{
		{
			{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "read_file"},
			{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"path":"notes.txt"}`},
			{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
			{Type: zeroruntime.StreamEventDone},
		},
		{
			{Type: zeroruntime.StreamEventText, Content: "plan drafted"},
			{Type: zeroruntime.StreamEventDone},
		},
	}}

	if _, err := Run(context.Background(), "plan something", provider, Options{
		SessionID:      "session-plan",
		Cwd:            root,
		Registry:       registry,
		ProviderName:   "test-provider",
		Model:          "test-model",
		Hooks:          dispatcher,
		PermissionMode: PermissionModePlan,
		MaxTurns:       2,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	events, err := audit.ReadEvents()
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	sawBeforeTool := false
	for _, event := range events {
		if event.Type != "hook_execution_started" {
			continue
		}
		switch event.Event {
		case hooks.EventBeforeTool:
			sawBeforeTool = true
		case hooks.EventSessionStart, hooks.EventSessionEnd, hooks.EventAfterTool:
			t.Fatalf("advisory hook %q executed during a plan-mode run", event.Event)
		}
	}
	if !sawBeforeTool {
		t.Fatal("expected beforeTool to still dispatch under plan mode (deny-gate must not fail open)")
	}
	for _, marker := range []string{sessionMarker, afterToolMarker} {
		if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
			t.Fatalf("plan-mode run let advisory hook touch the filesystem via %q: %v", marker, statErr)
		}
	}
}

// TestPlanModeHonorsBeforeToolVeto guards the fail-open hole where hooksSuppressed
// used to skip beforeTool under plan mode, so a deny-policy hook that blocks
// secret reads in auto mode would silently allow them under PermissionModePlan.
func TestPlanModeHonorsBeforeToolVeto(t *testing.T) {
	goBinary, err := exec.LookPath("go")
	if err != nil {
		goRoot := runtime.GOROOT() //nolint:staticcheck // Safe for this non-portable test binary.
		goBinary = filepath.Join(goRoot, "bin", "go")
		if runtime.GOOS == "windows" {
			goBinary += ".exe"
		}
		if _, statErr := os.Stat(goBinary); statErr != nil {
			t.Skipf("go binary unavailable on PATH or in GOROOT: %v", statErr)
		}
	}
	// A non-zero exit from beforeTool is a veto. "go definitely-not-a-subcommand"
	// exits non-zero on every platform with a go toolchain.
	dispatcher := hooks.NewDispatcher(hooks.DispatcherOptions{
		Config: hooks.Config{
			Enabled: true,
			Hooks: []hooks.Definition{
				{ID: "zero.veto", Event: hooks.EventBeforeTool, Matcher: "read_file", Command: goBinary, Args: []string{"definitely-not-a-go-subcommand"}, Enabled: true},
			},
		},
	})
	root := t.TempDir()
	secret := filepath.Join(root, "secret.txt")
	if err := os.WriteFile(secret, []byte("SUPERSECRET"), 0o644); err != nil {
		t.Fatalf("write secret.txt: %v", err)
	}
	registry := tools.NewRegistry()
	registry.Register(tools.NewReadFileTool(root))
	var toolOutputs []string
	provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{
		{
			{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "read_file"},
			{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"path":"secret.txt"}`},
			{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
			{Type: zeroruntime.StreamEventDone},
		},
		{
			{Type: zeroruntime.StreamEventText, Content: "blocked"},
			{Type: zeroruntime.StreamEventDone},
		},
	}}

	if _, err := Run(context.Background(), "read the secret", provider, Options{
		SessionID:      "session-plan-veto",
		Cwd:            root,
		Registry:       registry,
		ProviderName:   "test-provider",
		Model:          "test-model",
		Hooks:          dispatcher,
		PermissionMode: PermissionModePlan,
		MaxTurns:       2,
		OnToolResult: func(result ToolResult) {
			toolOutputs = append(toolOutputs, result.Output)
		},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(toolOutputs) == 0 {
		t.Fatal("expected a tool result for the vetoed read_file call")
	}
	combined := strings.Join(toolOutputs, "\n")
	if strings.Contains(combined, "SUPERSECRET") {
		t.Fatalf("plan mode failed open: beforeTool veto was skipped and secret leaked: %q", combined)
	}
	if !strings.Contains(combined, "blocked") && !strings.Contains(combined, "zero.veto") && !strings.Contains(strings.ToLower(combined), "hook") {
		t.Fatalf("expected tool result to mention the beforeTool veto, got %q", combined)
	}
}

// A cancelled permission on either sandbox-retry path must abort the run with a
// recognisable cancellation, and must not run the escalated retry.
//
// BOTH PATHS, because both wrap ErrPermissionApprovalCanceled and each is
// reached by a different kind of denial: one by an unrestricted platform denial
// asking for approval, the other by an external-network denial. Surfaces map
// this sentinel onto their own idea of a cancel — ACP returns stopReason
// "cancelled" for it — so a path that produced a bare error instead would show
// the user a crash where they had simply declined.
func TestCancellingASandboxRetryAbortsWithoutRetrying(t *testing.T) {
	denial := func(capability execution.CapabilityKind, scope string) tools.Result {
		return tools.Result{
			Status: tools.StatusError,
			ExecutionOutcome: &execution.Outcome{
				State: execution.StateDenied,
				Kind:  execution.OutcomeEnforcementDenied,
				Denial: &execution.Denial{
					Capability:  execution.Capability{Kind: capability, Scope: scope},
					Source:      execution.DenialSourcePlatformSandbox,
					Reason:      "denied by the sandbox",
					Recoverable: true,
					NextAction:  execution.DenialNextActionRequestApproval,
				},
			},
		}
	}

	for _, testCase := range []struct {
		name   string
		result tools.Result
		run    func(context.Context, *tools.Registry, ToolCall, tools.Tool, map[string]any, tools.Result, Options) error
	}{
		{
			name:   "unsandboxed retry",
			result: denial(execution.CapabilityUnrestricted, "host"),
			run: func(ctx context.Context, registry *tools.Registry, call ToolCall, tool tools.Tool, args map[string]any, result tools.Result, options Options) error {
				_, _, _, _, _, _, abortErr := maybeRetryUnsandboxedAfterSandboxRestriction(
					ctx, registry, call, tool, args, result, PermissionModeAsk, options, nil)
				return abortErr
			},
		},
		{
			name:   "network retry",
			result: denial(execution.CapabilityExternalNetwork, ""),
			run: func(ctx context.Context, registry *tools.Registry, call ToolCall, tool tools.Tool, args map[string]any, result tools.Result, options Options) error {
				_, _, _, _, _, abortErr := maybeRetryWithNetworkAfterSandboxDenial(
					ctx, registry, call, tool, args, result, PermissionModeAsk, options, nil)
				return abortErr
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			retryTool := &sandboxNamespaceLimitedRetryTool{}
			registry := tools.NewRegistry()
			registry.Register(retryTool)

			asked := 0
			options := Options{
				Registry:       registry,
				PermissionMode: PermissionModeAsk,
				Autonomy:       "medium",
				Sandbox: sandbox.NewEngine(sandbox.EngineOptions{
					WorkspaceRoot: t.TempDir(),
					Policy:        sandbox.DefaultPolicy(),
				}),
				OnPermissionRequest: func(context.Context, PermissionRequest) (PermissionDecision, error) {
					asked++
					return PermissionDecision{Action: PermissionDecisionCancel, Reason: "client cancelled"}, nil
				},
			}

			call := ToolCall{ID: "call-1", Name: "bash", Arguments: `{"command":"curl example.com"}`}
			args := map[string]any{"command": "curl example.com"}

			abortErr := testCase.run(context.Background(), registry, call, retryTool, args, testCase.result, options)

			if asked != 1 {
				t.Fatalf("permission was requested %d times, want exactly 1", asked)
			}
			if !errors.Is(abortErr, ErrPermissionApprovalCanceled) {
				t.Fatalf("abort error = %v, want it to match ErrPermissionApprovalCanceled", abortErr)
			}
			// The escalated retry is the thing the permission was gating. A
			// cancelled prompt that still ran it would have run the command the
			// user just declined.
			if len(retryTool.calls) != 0 {
				t.Fatalf("the escalated retry ran %d times after a cancel: %+v", len(retryTool.calls), retryTool.calls)
			}
		})
	}
}
