package agent

import (
	"testing"

	"github.com/Twigpine/zero/internal/tools"
)

// UNCATEGORIZED REFUSALS MUST STILL COUNT.
//
// The halt guard asked DenialReason != DenialNone, but a category is only
// attached where a TYPED denial is built. A headless run leaves
// OnPermissionRequest nil, so the loop never reaches that branch and the
// registry returns a bare "Error: Permission required ..." with no category. A
// sandbox preflight denial on a non-shell tool loses its SandboxDecision in the
// conversion and arrives as an uncategorized "Sandbox block".
//
// Both were counted as SUCCESS, which cleared the very record the guard
// accumulates, so the same refused call could repeat to MaxTurns. That is the
// loop this PR exists to stop.
func TestUncategorizedPolicyRefusalsAreCounted(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		result ToolResult
	}{
		{
			// Still uncategorized in the DenialReason sense: a headless run never
			// reaches the branch that builds a typed denial. It carries the
			// registry's pre-execution marker instead, which is what makes it
			// classifiable without reading the message.
			name: "headless prompt refusal carries no denial category",
			result: ToolResult{
				Status: tools.StatusError,
				Output: `Error: Permission required for bash: The tool is marked "prompt" and was not executed.`,
				Meta:   map[string]string{tools.PolicyRefusalMeta: tools.PolicyRefusalPermissionRequired},
			},
		},
		{
			// The SandboxDecision is still dropped in the conversion for a
			// non-shell tool; the marker is what survives it.
			name: "sandbox preflight denial loses its decision in conversion",
			result: ToolResult{
				Status: tools.StatusError,
				Output: "Sandbox block: write outside the workspace",
				Meta:   map[string]string{tools.PolicyRefusalMeta: tools.PolicyRefusalSandboxDenied},
			},
		},
		{
			name:   "an explicitly denied permission action",
			result: ToolResult{Status: tools.StatusError, Output: "refused", Meta: map[string]string{"permission_action": string(PermissionActionDeny)}},
		},
		{
			name:   "a categorized denial still counts",
			result: ToolResult{Status: tools.StatusError, Output: "denied", DenialReason: DenialFiltered},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if !isPolicyRefusal(testCase.result) {
				t.Fatal("refusal was treated as a success, so the guard's record is cleared and the call can repeat to MaxTurns")
			}
			if isRetriableToolError(testCase.result) {
				t.Error("a policy refusal is not retriable: repeating it verbatim cannot help")
			}
		})
	}
}

// An ordinary failure is still retriable and still not a refusal, or the guard
// would halt on things the model can legitimately fix by trying again.
func TestOrdinaryFailuresAreNotPolicyRefusals(t *testing.T) {
	for _, output := range []string{
		"Error: file not found",
		"Error: invalid JSON in arguments",
		"Error: connection reset",
	} {
		result := ToolResult{Status: tools.StatusError, Output: output}
		if isPolicyRefusal(result) {
			t.Errorf("%q was classified as a policy refusal", output)
		}
		if !isRetriableToolError(result) {
			t.Errorf("%q should stay retriable", output)
		}
	}
}
