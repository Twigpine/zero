package tools

import (
	"context"
	"strings"
	"testing"

	zeroSandbox "github.com/Twigpine/zero/internal/sandbox"
)

const testDenyReadNotice = "denyRead is set, so the restricted token drops WRITE_RESTRICTED and the workspace write jail no longer confines writes outside it (#869)."

// noticeCarryingTool stands in for a command tool whose plan carried an
// enforcement notice. It writes the notice the way addSandboxMeta does, which is
// the only thing the production paths do with it.
type noticeCarryingTool struct{}

func (noticeCarryingTool) Name() string        { return "bash" }
func (noticeCarryingTool) Description() string { return "test shell tool" }
func (noticeCarryingTool) Parameters() Schema {
	return Schema{
		Type:                 "object",
		Properties:           map[string]PropertySchema{"command": {Type: "string"}},
		Required:             []string{"command"},
		AdditionalProperties: false,
	}
}
func (noticeCarryingTool) Safety() Safety {
	return Safety{SideEffect: SideEffectRead, Permission: PermissionAllow, Reason: "reads files"}
}
func (noticeCarryingTool) Run(context.Context, map[string]any) Result {
	meta := map[string]string{}
	addSandboxMeta(meta, zeroSandbox.CommandPlan{
		Backend: zeroSandbox.Backend{Name: zeroSandbox.BackendWindowsRestrictedToken},
		Notes:   []string{testDenyReadNotice},
	})
	return Result{
		Status:  StatusOK,
		Output:  "hello from the command",
		Meta:    meta,
		Display: Display{Summary: "ran the command", Kind: "shell"},
	}
}

// THE DISCLOSURE HAS TO REACH A HUMAN AND A MODEL, NOT A METADATA MAP.
//
// The first version of this wrote sandbox_notices into Result.Meta and stopped
// there. Nothing in production reads those keys, ModelOutput and HumanDisplay
// never consult Meta, and the durable history drops it, so a Windows user who
// configured deny_read could take the non-WRITE_RESTRICTED token, lose write
// confinement, and see nothing but ordinary command output. Metadata is
// side-band data, not a disclosure channel.
func TestEnforcementNoticeReachesTheModelAndTheDisplay(t *testing.T) {
	registry := NewRegistry()
	registry.Register(noticeCarryingTool{})

	result := registry.RunWithOptions(context.Background(), "bash", map[string]any{
		"command": "echo hello",
	}, RunOptions{PermissionGranted: true})

	if result.Status != StatusOK {
		t.Fatalf("tool failed: %s", result.Output)
	}

	model := result.ModelOutput()
	if !strings.Contains(model, "#869") {
		t.Errorf("the model-facing result does not carry the disclosure, so the agent proceeds unaware:\n%s", model)
	}
	if !strings.Contains(model, "hello from the command") {
		t.Errorf("the notice displaced the actual output:\n%s", model)
	}
	// PREPENDED, because the output budget trims from the end and a disclosure
	// that survives only on short results is not a disclosure.
	if !strings.HasPrefix(strings.TrimSpace(model), testDenyReadNotice) {
		t.Errorf("the notice is not in front of the output, so a trimmed result can lose it:\n%s", model)
	}

	display := result.HumanDisplay()
	if !strings.Contains(display.Summary, "#869") {
		t.Errorf("the interactive display does not carry the disclosure, so the operator sees nothing: %q", display.Summary)
	}

	// Kept in metadata too, for integrations reading the result JSON.
	if result.Meta[sandboxNoticesMeta] == "" {
		t.Errorf("the metadata copy was dropped: %#v", result.Meta)
	}
}

// A result with nothing to disclose must be untouched, or every command grows a
// blank line and the presence of a notice stops meaning anything.
func TestResultsWithoutNoticesAreUnchanged(t *testing.T) {
	result := Result{Status: StatusOK, Output: "plain output", Display: Display{Summary: "did a thing"}}

	if got := result.ModelOutput(); got != "plain output" {
		t.Errorf("model output = %q, want it untouched", got)
	}
	if got := result.HumanDisplay().Summary; got != "did a thing" {
		t.Errorf("display summary = %q, want it untouched", got)
	}
}

// Whitespace-only notices are not notices. Guards against a plan that carries an
// empty entry putting a blank line in front of every result.
func TestBlankNoticesDoNotAlterTheResult(t *testing.T) {
	result := Result{
		Status:             StatusOK,
		Output:             "plain output",
		EnforcementNotices: []string{"", "   "},
	}
	if got := result.ModelOutput(); got != "plain output" {
		t.Errorf("model output = %q, want it untouched", got)
	}
}
