package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/sandbox"
	"github.com/Twigpine/zero/internal/tools"
)

type normalizationRecordingShellTool struct {
	calls []map[string]any
}

func (tool *normalizationRecordingShellTool) Name() string { return "bash" }
func (tool *normalizationRecordingShellTool) Description() string {
	return "record normalized shell arguments"
}
func (tool *normalizationRecordingShellTool) Parameters() tools.Schema {
	return tools.Schema{Type: "object", AdditionalProperties: true}
}
func (tool *normalizationRecordingShellTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectShell, Permission: tools.PermissionAllow}
}
func (tool *normalizationRecordingShellTool) Run(_ context.Context, args map[string]any) tools.Result {
	tool.calls = append(tool.calls, cloneArgs(args))
	return tools.Result{Status: tools.StatusOK}
}

// A shell call that sets sandbox_permissions: with_additional_permissions but
// omits (or malforms) additional_permissions can never succeed: the same
// validation runs again at grant time regardless of the user's decision. It
// must be rejected as a plain tool error BEFORE any permission prompt, not
// surfaced as a confusing "approved but denied" prompt outcome.
func TestExecuteToolCallRejectsMissingAdditionalPermissionsBeforePrompt(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedBashTool(root, nil))

	promptCalls := 0
	result, err := executeToolCall(
		context.Background(),
		registry,
		ToolCall{
			ID:        "c1",
			Name:      "bash",
			Arguments: `{"command":"echo hi","sandbox_permissions":"with_additional_permissions"}`,
		},
		PermissionModeAuto,
		Options{
			OnPermissionRequest: func(ctx context.Context, request PermissionRequest) (PermissionDecision, error) {
				promptCalls++
				return PermissionDecision{Action: PermissionDecisionAllow}, nil
			},
		},
	)
	if err != nil {
		t.Fatalf("executeToolCall: %v", err)
	}
	if result.Status != tools.StatusError {
		t.Fatalf("status = %q, want error", result.Status)
	}
	if promptCalls != 0 {
		t.Fatalf("OnPermissionRequest called %d times, want 0 (must reject before prompting)", promptCalls)
	}
}

// The mismatched-flag case (additional_permissions present without the
// sandbox_permissions flag) must be rejected the same way.
func TestExecuteToolCallRejectsAdditionalPermissionsWithoutFlagBeforePrompt(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedBashTool(root, nil))

	promptCalls := 0
	result, err := executeToolCall(
		context.Background(),
		registry,
		ToolCall{
			ID:        "c1",
			Name:      "bash",
			Arguments: `{"command":"echo hi","additional_permissions":{"network":{"enabled":true}}}`,
		},
		PermissionModeAuto,
		Options{
			OnPermissionRequest: func(ctx context.Context, request PermissionRequest) (PermissionDecision, error) {
				promptCalls++
				return PermissionDecision{Action: PermissionDecisionAllow}, nil
			},
		},
	)
	if err != nil {
		t.Fatalf("executeToolCall: %v", err)
	}
	if result.Status != tools.StatusError {
		t.Fatalf("status = %q, want error", result.Status)
	}
	if promptCalls != 0 {
		t.Fatalf("OnPermissionRequest called %d times, want 0 (must reject before prompting)", promptCalls)
	}
}

// The rejection message must show a concrete, correctly-shaped example: a
// model that gets this wrong once (attaching sandbox_permissions out of habit
// from an earlier call, without a valid payload) needs enough in the error to
// self-correct on retry instead of repeating the identical mistake.
func TestExecuteToolCallMissingAdditionalPermissionsErrorIsActionable(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedBashTool(root, nil))

	result, err := executeToolCall(
		context.Background(),
		registry,
		ToolCall{
			ID:        "c1",
			Name:      "bash",
			Arguments: `{"command":"ping example.com","sandbox_permissions":"with_additional_permissions"}`,
		},
		PermissionModeAuto,
		Options{},
	)
	if err != nil {
		t.Fatalf("executeToolCall: %v", err)
	}
	if !strings.Contains(result.Output, `{"network": {"enabled": true}}`) {
		t.Fatalf("output = %q, want a concrete additional_permissions example", result.Output)
	}
	assertNamesOmitRemedy(t, "missing additional_permissions", result.Output)
}

// A well-formed additional_permissions payload must still reach the normal
// permission prompt: this fix only rejects malformed payloads.
func TestExecuteToolCallStillPromptsForValidAdditionalPermissions(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedBashTool(root, nil))

	promptCalls := 0
	_, err := executeToolCall(
		context.Background(),
		registry,
		ToolCall{
			ID:   "c1",
			Name: "bash",
			Arguments: `{"command":"echo hi","sandbox_permissions":"with_additional_permissions",` +
				`"additional_permissions":{"network":{"enabled":true}}}`,
		},
		PermissionModeAuto,
		Options{
			OnPermissionRequest: func(ctx context.Context, request PermissionRequest) (PermissionDecision, error) {
				promptCalls++
				return PermissionDecision{Action: PermissionDecisionDeny}, nil
			},
		},
	)
	if err != nil {
		t.Fatalf("executeToolCall: %v", err)
	}
	if promptCalls != 1 {
		t.Fatalf("OnPermissionRequest called %d times, want exactly 1 for a valid elevation request", promptCalls)
	}
}

// runShellCall drives one shell tool call through the real registry (and so
// through Registry.RunWithOptions) exactly as the agent loop would.
func runShellCall(t *testing.T, root string, arguments string) ToolResult {
	t.Helper()
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedExecCommandTool(root, nil, nil))
	result, err := executeToolCall(
		context.Background(),
		registry,
		ToolCall{ID: "c1", Name: "exec_command", Arguments: arguments},
		PermissionModeUnsafe,
		Options{Cwd: root},
	)
	if err != nil {
		t.Fatalf("executeToolCall: %v", err)
	}
	return result
}

// TestAdditionalPermissionsRejectionsConverge is the regression guard for
// #845/#847. A model that attaches a permission object to a command needing
// none used to be told only "set sandbox_permissions", which pushed it into the
// OTHER invalid shape (flag set, payload empty) and its "include a permission"
// error pushed it straight back — the reported loop that burned turns on
// `git branch --show-current`.
//
// The contract this pins is convergence, not tolerance: both shapes are still
// REJECTED (nothing here grants access), but each rejection must name the one
// follow-up call that works, and that follow-up must actually succeed.
func TestAdditionalPermissionsRejectionsConverge(t *testing.T) {
	root := t.TempDir()
	const readOnly = `"cmd":"git --version"`

	// Step 1 of the reported loop: a permission object with no flag.
	first := runShellCall(t, root, `{`+readOnly+`,"additional_permissions":{"network":{"enabled":true}}}`)
	if first.Status != tools.StatusError {
		t.Fatalf("unflagged additional_permissions status = %q, want error (rejecting is correct)", first.Status)
	}
	assertNamesOmitRemedy(t, "unflagged additional_permissions", first.Output)

	// Step 2 of the reported loop: the model keeps the flag but empties the
	// payload. This must not be the dead end it was.
	second := runShellCall(t, root, `{`+readOnly+`,"sandbox_permissions":"with_additional_permissions"}`)
	if second.Status != tools.StatusError {
		t.Fatalf("flag without payload status = %q, want error", second.Status)
	}
	assertNamesOmitRemedy(t, "flag without payload", second.Output)

	// The call both errors point at must actually work.
	converged := runShellCall(t, root, `{`+readOnly+`}`)
	if converged.Status != tools.StatusOK {
		t.Fatalf("the call both errors instruct the model to make failed: status=%q output=%q", converged.Status, converged.Output)
	}
}

// assertNamesOmitRemedy pins the actionable half of the error text: the message
// must spell out that BOTH fields can simply be dropped. "Set the flag" or
// "include a permission" alone is what made the two errors chase each other.
func assertNamesOmitRemedy(t *testing.T, label string, output string) {
	t.Helper()
	if !strings.Contains(output, "omit both sandbox_permissions and additional_permissions") {
		t.Fatalf("%s error = %q, want it to say to omit both fields entirely", label, output)
	}
	if !strings.Contains(output, "retry") {
		t.Fatalf("%s error = %q, want it to tell the model to retry", label, output)
	}
}

// The empty-payload rejection carries the same remedy. It is pinned on the
// validator directly because normalizeNoopAdditionalPermissions absorbs the
// empty payload before executeToolCall reaches this branch;
// inlineAdditionalPermissionsProfile is still the shared source of truth that
// buildPermissionEvent and the grant path re-run, so its text must converge too.
func TestEmptyAdditionalPermissionsErrorNamesOmitRemedy(t *testing.T) {
	args := map[string]any{
		"cmd":                 "git --version",
		"sandbox_permissions": string(tools.SandboxPermissionsWithAdditionalPermissions),
		"additional_permissions": map[string]any{
			"network":     map[string]any{"enabled": false},
			"file_system": map[string]any{"read": []any{}, "write": []any{}},
		},
	}
	_, requested, err := inlineAdditionalPermissionsProfile(args, t.TempDir())
	if !requested {
		t.Fatal("requested = false, want true for an explicit with_additional_permissions call")
	}
	if err == nil {
		t.Fatal("an empty additional_permissions payload was accepted; it must still be rejected")
	}
	assertNamesOmitRemedy(t, "empty payload", err.Error())
}

// A provider that materializes every optional property may send
// "additional_permissions": null. That grants nothing and carries no flag, so it
// is the field omitted — rejecting it stalled an ordinary command behind an
// error the model could not act on.
func TestExecuteToolCallTreatsNullAdditionalPermissionsAsOmitted(t *testing.T) {
	result := runShellCall(t, t.TempDir(), `{"cmd":"git --version","additional_permissions":null}`)
	if result.Status != tools.StatusOK {
		t.Fatalf("null additional_permissions was rejected: status=%q output=%q", result.Status, result.Output)
	}
}

// A null payload alongside the flag must still be rejected, and must land on
// the same error as an absent payload rather than being quietly downgraded.
func TestExecuteToolCallRejectsFlaggedNullAdditionalPermissions(t *testing.T) {
	result := runShellCall(t, t.TempDir(),
		`{"cmd":"git --version","sandbox_permissions":"with_additional_permissions","additional_permissions":null}`)
	if result.Status != tools.StatusError {
		t.Fatalf("flagged null status = %q, want error", result.Status)
	}
	if !strings.Contains(result.Output, "no additional_permissions object was provided") {
		t.Fatalf("output = %q, want the missing-payload error", result.Output)
	}
	assertNamesOmitRemedy(t, "flagged null", result.Output)
}

func TestNormalizeProviderExpandedEmptyAdditionalPermissions(t *testing.T) {
	for _, mode := range []string{
		string(tools.SandboxPermissionsUseDefault),
		string(tools.SandboxPermissionsWithAdditionalPermissions),
	} {
		args := map[string]any{
			"command":             "./zero --version",
			"sandbox_permissions": mode,
			"additional_permissions": map[string]any{
				"file_system": map[string]any{
					"deny_read": []any{},
					"entries":   []any{},
					"read":      []any{},
					"write":     []any{},
				},
				"network": map[string]any{"enabled": false},
			},
		}
		normalizeNoopAdditionalPermissions(args, t.TempDir(), nil)
		if _, exists := args["additional_permissions"]; exists {
			t.Fatalf("mode %q retained empty additional_permissions: %#v", mode, args)
		}
		if got := args["sandbox_permissions"]; got != string(tools.SandboxPermissionsUseDefault) {
			t.Fatalf("mode %q normalized to %q, want use_default", mode, got)
		}
	}
}

func TestExecuteToolCallNormalizesProviderExpandedEmptyAdditionalPermissions(t *testing.T) {
	tool := &normalizationRecordingShellTool{}
	registry := tools.NewRegistry()
	registry.Register(tool)

	result, err := executeToolCall(
		context.Background(),
		registry,
		ToolCall{
			ID:   "c1",
			Name: "bash",
			Arguments: `{"command":"./zero --version","sandbox_permissions":"with_additional_permissions",` +
				`"additional_permissions":{"file_system":{"deny_read":[],"entries":[],"read":[],"write":[]},"network":{"enabled":false}}}`,
		},
		PermissionModeAuto,
		Options{Cwd: t.TempDir()},
	)
	if err != nil {
		t.Fatalf("executeToolCall: %v", err)
	}
	if result.Status != tools.StatusOK {
		t.Fatalf("provider-expanded empty permissions were rejected: %s", result.Output)
	}
	if len(tool.calls) != 1 {
		t.Fatalf("shell tool calls = %d, want 1", len(tool.calls))
	}
	args := tool.calls[0]
	if _, exists := args["additional_permissions"]; exists {
		t.Fatalf("executeToolCall retained empty additional_permissions: %#v", args)
	}
	if got := args["sandbox_permissions"]; got != string(tools.SandboxPermissionsUseDefault) {
		t.Fatalf("sandbox mode = %q, want use_default", got)
	}
}

func TestNormalizeAdditionalPermissionsPreservesRealRequest(t *testing.T) {
	args := map[string]any{
		"sandbox_permissions": string(tools.SandboxPermissionsWithAdditionalPermissions),
		"additional_permissions": map[string]any{
			"network": map[string]any{"enabled": true},
		},
	}
	normalizeNoopAdditionalPermissions(args, t.TempDir(), nil)
	if _, exists := args["additional_permissions"]; !exists {
		t.Fatal("non-empty additional_permissions was removed")
	}
	if got := args["sandbox_permissions"]; got != string(tools.SandboxPermissionsWithAdditionalPermissions) {
		t.Fatalf("sandbox mode changed to %q", got)
	}
}

func TestNormalizeAdditionalPermissionsRemovesWorkspaceReadAlreadyCoveredBySandbox(t *testing.T) {
	root := t.TempDir()
	engine := sandbox.NewEngine(sandbox.EngineOptions{
		WorkspaceRoot: root,
		Policy:        sandbox.DefaultPolicy(),
	})
	args := map[string]any{
		"command":             "./zero --version",
		"sandbox_permissions": string(tools.SandboxPermissionsWithAdditionalPermissions),
		"additional_permissions": map[string]any{
			"file_system": map[string]any{
				"entries": []any{
					map[string]any{
						"access": "read",
						"path": map[string]any{
							"type": "path",
							"path": ".",
						},
					},
				},
				"deny_read": []any{},
				"read":      []any{},
				"write":     []any{},
			},
			"network": map[string]any{"enabled": false},
		},
	}

	normalizeNoopAdditionalPermissions(args, root, engine)

	if _, exists := args["additional_permissions"]; exists {
		t.Fatalf("already-covered additional_permissions was retained: %#v", args)
	}
	if got := args["sandbox_permissions"]; got != string(tools.SandboxPermissionsUseDefault) {
		t.Fatalf("sandbox mode changed to %q", got)
	}
}
