package agent

import (
	"context"
	"testing"

	"github.com/Twigpine/zero/internal/localcontrol"
	"github.com/Twigpine/zero/internal/tools"
)

// THE DISABLED-DRIVER BRANCH, REACHED FOR REAL.
//
// RejectBeforePermission refuses on two separate conditions, and its neighbour
// test constructs empty options, so it returns at the FIRST one (no artifact
// directory configured) and the second is never evaluated. Reverting the
// disabled-driver branch in local_capture.go to a plain errorResult therefore
// left the whole agent and tools suites green.
//
// This is the shape an operator actually produces: an artifact root IS
// configured and one driver IS enabled, and the model asks for an action owned
// by a different, disabled driver. Without provenance that call is read as a
// retriable failure, so it collects a schema hint and can spend the profile's
// failure-streak escalation, for a tool no argument change can enable.
func TestCaptureArtifactDisabledDriverIsAPolicyRefusal(t *testing.T) {
	registry := tools.NewRegistry()
	options := tools.LocalControlArtifactOptions{
		ArtifactsDir: t.TempDir(),
		// Configured and enabled, so the tool as a whole is available and the
		// missing-directory branch cannot fire.
		Browser: localcontrol.BrowserOptions{Enabled: true, Driver: "test-browser", HelperPath: "browser-helper"},
		// Terminal deliberately left disabled.
	}
	for _, tool := range tools.NewLocalControlArtifactTools(options) {
		registry.Register(tool)
	}

	// An enabled driver's action must still be accepted, or the test below would
	// pass for a tool that refuses everything.
	if result := registry.RunWithOptions(context.Background(), "capture_artifact", map[string]any{
		"action": "browser_screenshot", "name": "shot",
	}, tools.RunOptions{PermissionGranted: true}); tools.IsPolicyRefusalResult(result) {
		t.Fatalf("SETUP INVALID: the enabled browser driver was refused as policy: %s", result.Output)
	}

	result := registry.RunWithOptions(context.Background(), "capture_artifact", map[string]any{
		"action": "terminal_snapshot", "name": "snap", "session": "test-session",
	}, tools.RunOptions{PermissionGranted: true})

	if result.Status != tools.StatusError {
		t.Fatalf("SETUP INVALID: the disabled terminal driver did not refuse: %s / %s", result.Status, result.Output)
	}
	if !tools.IsPolicyRefusalResult(result) {
		t.Fatalf("a driver disabled by configuration refuses without provenance, so it reads as a retriable failure: %#v", result.Meta)
	}
	if got := result.Meta["policy_refusal"]; got != tools.PolicyRefusalToolNotEnabled {
		t.Errorf("policy_refusal = %q, want %q", got, tools.PolicyRefusalToolNotEnabled)
	}
	if isRetriableToolError(ToolResult{Status: result.Status, Output: result.ModelOutput(), Meta: result.Meta}) {
		t.Error("the refusal is still classified as retriable, so the model gets a schema hint for a configuration decision")
	}
}

// AND THE EARLY REJECTIONS MUST NOT COLLAPSE INTO ONE ANSWER.
//
// RejectBeforePermission refuses on configuration; argument validation refuses
// on a fixable mistake, and it runs FIRST, which is how the disabled-driver
// branch stayed uncovered. A malformed call must stay an ordinary retriable
// error, because trying again differently is exactly the right response to it.
func TestCaptureArtifactMalformedArgumentsStayRetriable(t *testing.T) {
	registry := tools.NewRegistry()
	for _, tool := range tools.NewLocalControlArtifactTools(tools.LocalControlArtifactOptions{
		ArtifactsDir: t.TempDir(),
		Terminal:     localcontrol.TerminalOptions{Enabled: true, Driver: "test-terminal", HelperPath: "terminal-helper"},
	}) {
		registry.Register(tool)
	}

	// The driver IS enabled, so nothing here is a configuration decision. The
	// call is simply missing the session the action requires.
	result := registry.RunWithOptions(context.Background(), "capture_artifact", map[string]any{
		"action": "terminal_snapshot", "name": "snap",
	}, tools.RunOptions{PermissionGranted: true})

	if result.Status != tools.StatusError {
		t.Fatalf("SETUP INVALID: the malformed call did not fail: %s", result.Output)
	}
	if tools.IsPolicyRefusalResult(result) {
		t.Errorf("a fixable argument mistake was marked a policy refusal, so the model loses the schema hint that would fix it: %s", result.Output)
	}
	if !isRetriableToolError(ToolResult{Status: result.Status, Output: result.ModelOutput(), Meta: result.Meta}) {
		t.Errorf("a fixable argument mistake is not retriable: %s", result.Output)
	}
}
