package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/sandbox"
	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

func TestRequestPermissionsTurnGrantAllowsLaterToolAndCleansUp(t *testing.T) {
	workspace := t.TempDir()
	outside := tempDirOutsideDefaultTemp(t)
	target := filepath.Join(outside, "vegetables.txt")
	scope, err := sandbox.NewScope(workspace, nil)
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	engine := sandbox.NewEngine(sandbox.EngineOptions{
		WorkspaceRoot: workspace,
		Policy:        sandbox.DefaultPolicy(),
		Scope:         scope,
	})
	registry := tools.NewRegistry()
	registry.Register(tools.NewRequestPermissionsTool())
	registry.Register(tools.NewScopedWriteFileTool(workspace, scope))
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "grant-1", ToolName: tools.RequestPermissionsToolName},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "grant-1", ArgumentsFragment: `{"reason":"Need to write outside the workspace.","permissions":{"file_system":{"write":[` + quoteJSONString(target) + `]}}}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "grant-1"},
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "write-1", ToolName: "write_file"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "write-1", ArgumentsFragment: `{"path":` + quoteJSONString(target) + `,"content":"carrot\n","overwrite":true}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "write-1"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "done"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}
	var requests []PermissionRequest
	var results []ToolResult
	normalizedProfile, err := sandbox.NormalizeRequestPermissionProfile(sandbox.RequestPermissionProfile{
		FileSystem: &sandbox.FileSystemPermissions{Write: []string{target}},
	}, workspace)
	if err != nil {
		t.Fatal(err)
	}
	expectedGrantProfile, err := sandbox.RequestPermissionGrantProfile(normalizedProfile)
	if err != nil {
		t.Fatal(err)
	}
	expectedWriteRoot := expectedGrantProfile.FileSystem.Write[0]

	result, err := Run(context.Background(), "write the file", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAuto,
		Autonomy:       "medium",
		Cwd:            workspace,
		Sandbox:        engine,
		OnPermissionRequest: func(_ context.Context, request PermissionRequest) (PermissionDecision, error) {
			requests = append(requests, request)
			return PermissionDecision{Action: PermissionDecisionAllow, Reason: "ok"}, nil
		},
		OnToolResult: func(result ToolResult) {
			results = append(results, result)
		},
		MaxTurns: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("final answer = %q, want done", result.FinalAnswer)
	}
	if len(requests) != 1 || requests[0].ToolName != tools.RequestPermissionsToolName {
		t.Fatalf("permission requests = %#v, want one request_permissions prompt", requests)
	}
	if !strings.Contains(requests[0].Scope, "write "+expectedWriteRoot) || strings.Contains(requests[0].Scope, target) {
		t.Fatalf("permission request scope = %q, want widened directory root %q and not file %q", requests[0].Scope, expectedWriteRoot, target)
	}
	if len(results) != 2 || results[0].Status != tools.StatusOK || results[1].Status != tools.StatusOK {
		t.Fatalf("tool results = %#v, want request_permissions and write_file ok", results)
	}
	var grantResponse sandbox.RequestPermissionsResponse
	if err := json.Unmarshal([]byte(results[0].Output), &grantResponse); err != nil {
		t.Fatalf("unmarshal grant response %q: %v", results[0].Output, err)
	}
	if grantResponse.Permissions.FileSystem == nil ||
		len(grantResponse.Permissions.FileSystem.Write) != 1 ||
		grantResponse.Permissions.FileSystem.Write[0] != expectedWriteRoot {
		t.Fatalf("grant response permissions = %#v, want write root %q", grantResponse.Permissions, expectedWriteRoot)
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "carrot\n" {
		t.Fatalf("target content = %q", content)
	}

	decision := engine.Evaluate(context.Background(), sandbox.Request{
		WorkspaceRoot:  workspace,
		ToolName:       "write_file",
		SideEffect:     sandbox.SideEffectWrite,
		Permission:     sandbox.PermissionPrompt,
		PermissionMode: sandbox.PermissionModeAuto,
		Args:           map[string]any{"path": target},
	})
	if decision.Action != sandbox.ActionPrompt {
		t.Fatalf("turn grant should be cleaned up after Run, got decision %#v", decision)
	}
}

func tempDirOutsideDefaultTemp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", ".zero-sandbox-outside-")
	if err != nil {
		t.Fatalf("MkdirTemp outside default temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("Abs(%q): %v", dir, err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return filepath.Clean(abs)
}

func TestInlineAdditionalPermissionsGrantAllowsNetworkCommand(t *testing.T) {
	workspace := t.TempDir()
	engine := sandbox.NewEngine(sandbox.EngineOptions{
		WorkspaceRoot: workspace,
		Policy:        sandbox.DefaultPolicy(),
	})
	args := map[string]any{
		"sandbox_permissions": string(tools.SandboxPermissionsWithAdditionalPermissions),
		"additional_permissions": map[string]any{
			"network": map[string]any{"enabled": true},
		},
	}
	cleanup, err := grantInlineAdditionalPermissions(args, sandbox.PermissionGrantScopeTurn, Options{
		Sandbox: engine,
		Cwd:     workspace,
	})
	if err != nil {
		t.Fatalf("grantInlineAdditionalPermissions: %v", err)
	}
	defer cleanup()

	decision := engine.Evaluate(context.Background(), sandbox.Request{
		WorkspaceRoot:     workspace,
		ToolName:          "exec_command",
		SideEffect:        sandbox.SideEffectShell,
		Permission:        sandbox.PermissionPrompt,
		PermissionGranted: true,
		PermissionMode:    sandbox.PermissionModeAsk,
		Args:              map[string]any{"cmd": "python3 -m http.server 8080 --bind 127.0.0.1"},
	})
	if decision.Action != sandbox.ActionAllow {
		t.Fatalf("inline network grant should allow shell network command, got %#v", decision)
	}
}

func TestInlineAdditionalPermissionsRequiresPromptUnderAutoAllow(t *testing.T) {
	args := map[string]any{
		"sandbox_permissions": string(tools.SandboxPermissionsWithAdditionalPermissions),
		"additional_permissions": map[string]any{
			"network": map[string]any{"enabled": true},
		},
	}
	decision := &sandbox.Decision{Action: sandbox.ActionAllow, AutoAllowed: true}
	if !shouldRequestPermission(tools.NewScopedBashTool(t.TempDir(), nil), args, false, decision) {
		t.Fatal("inline additional permissions must request approval even when a sandboxed shell would otherwise auto-allow")
	}
}

func TestRequestPermissionsDenyReturnsEmptyGrantAndContinues(t *testing.T) {
	workspace := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewRequestPermissionsTool())
	provider := requestPermissionsOnlyProvider(`{"reason":"Need network.","permissions":{"network":{"enabled":true}}}`, "done")
	var results []ToolResult

	result, err := Run(context.Background(), "request network", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAuto,
		Cwd:            workspace,
		Sandbox:        sandbox.NewEngine(sandbox.EngineOptions{WorkspaceRoot: workspace, Policy: sandbox.DefaultPolicy()}),
		OnPermissionRequest: func(context.Context, PermissionRequest) (PermissionDecision, error) {
			return PermissionDecision{Action: PermissionDecisionDeny, Reason: "no"}, nil
		},
		OnToolResult: func(result ToolResult) { results = append(results, result) },
		MaxTurns:     2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("final answer = %q, want done", result.FinalAnswer)
	}
	if len(results) != 1 || results[0].Status != tools.StatusOK {
		t.Fatalf("request_permissions denial should be an ok tool result, got %#v", results)
	}
	var response sandbox.RequestPermissionsResponse
	if err := json.Unmarshal([]byte(results[0].Output), &response); err != nil {
		t.Fatalf("unmarshal response %q: %v", results[0].Output, err)
	}
	if !response.Permissions.Empty() || response.Scope != sandbox.PermissionGrantScopeTurn {
		t.Fatalf("deny response = %#v, want empty turn grant", response)
	}
}

func TestRequestPermissionsStrictReviewResponse(t *testing.T) {
	workspace := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewRequestPermissionsTool())
	provider := requestPermissionsOnlyProvider(`{"reason":"Need read access.","permissions":{"file_system":{"read":[`+quoteJSONString(workspace)+`]}}}`, "done")
	var output string

	_, err := Run(context.Background(), "request read", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAuto,
		Cwd:            workspace,
		Sandbox:        sandbox.NewEngine(sandbox.EngineOptions{WorkspaceRoot: workspace, Policy: sandbox.DefaultPolicy()}),
		OnPermissionRequest: func(context.Context, PermissionRequest) (PermissionDecision, error) {
			return PermissionDecision{Action: PermissionDecisionAllowStrict, Reason: "review commands"}, nil
		},
		OnToolResult: func(result ToolResult) { output = result.Output },
		MaxTurns:     2,
	})
	if err != nil {
		t.Fatal(err)
	}
	var response sandbox.RequestPermissionsResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		t.Fatalf("unmarshal response %q: %v", output, err)
	}
	if !response.StrictAutoReview || response.Scope != sandbox.PermissionGrantScopeTurn {
		t.Fatalf("strict response = %#v, want strict turn grant", response)
	}
	if response.Permissions.FileSystem == nil || len(response.Permissions.FileSystem.Read) != 1 {
		t.Fatalf("strict response permissions = %#v, want read permission", response.Permissions)
	}
}

func requestPermissionsOnlyProvider(arguments string, finalAnswer string) *mockProvider {
	if !strings.HasPrefix(arguments, "{") {
		arguments = "{}"
	}
	return &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "grant-1", ToolName: tools.RequestPermissionsToolName},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "grant-1", ArgumentsFragment: arguments},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "grant-1"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: finalAnswer},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}
}
