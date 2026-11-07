package zerocommands

import (
	"sort"
	"strings"

	"github.com/Twigpine/zero/internal/sandbox"
)

// SandboxPolicySnapshot is the typed view of the live sandbox policy
// as it is exposed to the TUI render path, the headless
// `zero sandbox policy --json` command, and PR/CI automation. The
// snapshot is a strict superset of the on-disk policy shape: every
// field of sandbox.Policy is copied verbatim, and an EffectiveMode
// field is added so consumers do not have to translate the empty
// string into the enforced default themselves.
type SandboxPolicySnapshot struct {
	Mode             string `json:"mode"`
	Network          string `json:"network"`
	EnforceWorkspace bool   `json:"enforceWorkspace"`
	EffectiveMode    string `json:"effectiveMode,omitempty"`
}

// SandboxRiskSnapshot is the typed view of a sandbox.Risk as it is
// exposed to the TUI render path and PR/CI automation. The snapshot
// copies Level and Reason verbatim and sorts Categories
// alphabetically so the JSON output is deterministic.
type SandboxRiskSnapshot struct {
	Level      string   `json:"level"`
	Categories []string `json:"categories"`
	Reason     string   `json:"reason"`
}

// SandboxBlockSnapshot is the typed view of a sandbox.Block
// as it is exposed to the TUI render path, the audit log, and
// PR/CI automation. The Path field is preserved because the
// operator needs it to triage an out-of-workspace or symlink
// traversal block.
type SandboxBlockSnapshot struct {
	Code        string `json:"code"`
	ToolName    string `json:"toolName,omitempty"`
	Action      string `json:"action"`
	Path        string `json:"path,omitempty"`
	Reason      string `json:"reason"`
	Recoverable bool   `json:"recoverable"`
}

// SandboxBackendSnapshot is the typed view of a sandbox.Backend as
// it is exposed to the TUI render path, the headless
// `zero sandbox policy --json` command, and PR/CI automation. The
// snapshot is a strict superset of the on-disk backend shape: every
// field of sandbox.Backend is copied verbatim, and a NativeIsolation
// field is preserved so the JSON shape matches what consumers
// already expect.
type SandboxBackendSnapshot struct {
	Name            string `json:"name"`
	Available       bool   `json:"available"`
	Platform        string `json:"platform,omitempty"`
	Fallback        bool   `json:"fallback"`
	CommandWrapping bool   `json:"commandWrapping"`
	NativeIsolation bool   `json:"nativeIsolation"`
	Executable      string `json:"executable,omitempty"`
	Message         string `json:"message,omitempty"`
}

// SandboxPlanSnapshot is the typed view of a sandbox.BackendPlan as
// it is exposed to the TUI render path, the headless
// `zero sandbox policy --json` command, and PR/CI automation. The
// snapshot bundles the policy, the backend, and the human-readable
// restriction warnings so the operator gets one payload describing
// the entire sandbox posture.
//
// WriteRoots lists every directory the sandbox allows writes in
// (workspace root first, then user-granted extras). No current
// builder populates it — SandboxPlanSnapshotFromPlan leaves it
// unset because a BackendPlan carries no engine scope. Callers
// that hold the live engine can set it from engine.Scope().Roots();
// omitempty keeps the JSON shape stable when unset.
type SandboxPlanSnapshot struct {
	Policy        SandboxPolicySnapshot  `json:"policy"`
	Backend       SandboxBackendSnapshot `json:"backend"`
	Restrictions  []string               `json:"restrictions"`
	WorkspaceRoot string                 `json:"workspaceRoot,omitempty"`
	WriteRoots    []string               `json:"writeRoots,omitempty"`
}

// SandboxDecisionSnapshot is the typed view of a live sandbox.Decision
// as it is exposed to the TUI render path, the audit log, and
// PR/CI automation. The snapshot includes the resolved grant
// (when the decision was driven by a persistent grant) and the
// block (when the decision was a deny) so consumers do not
// have to reach back into the sandbox package to render the
// outcome.
type SandboxDecisionSnapshot struct {
	Action       string                `json:"action"`
	Reason       string                `json:"reason,omitempty"`
	Risk         SandboxRiskSnapshot   `json:"risk"`
	GrantMatched bool                  `json:"grantMatched"`
	Grant        *SandboxGrantSnapshot `json:"grant,omitempty"`
	Block        *SandboxBlockSnapshot `json:"block,omitempty"`
}

// SandboxPolicySnapshotFromPolicy converts a sandbox.Policy into its
// typed snapshot. The EffectiveMode field is the resolved policy
// mode: an empty Mode falls back to ModeEnforce, which is the
// runtime default. Consumers can read EffectiveMode to render the
// "what is actually in effect" line without re-implementing the
// default.
func SandboxPolicySnapshotFromPolicy(policy sandbox.Policy) SandboxPolicySnapshot {
	mode := strings.TrimSpace(string(policy.Mode))
	if mode == "" {
		mode = string(sandbox.ModeEnforce)
	}
	return SandboxPolicySnapshot{
		Mode:             string(policy.Mode),
		Network:          string(policy.Network),
		EnforceWorkspace: policy.EnforceWorkspace,
		EffectiveMode:    mode,
	}
}

// SandboxRiskSnapshotFromRisk converts a sandbox.Risk into its
// typed snapshot. The Categories slice is sorted alphabetically
// and copied so the snapshot does not alias the input.
func SandboxRiskSnapshotFromRisk(risk sandbox.Risk) SandboxRiskSnapshot {
	categories := append([]string(nil), risk.Categories...)
	sort.Strings(categories)
	return SandboxRiskSnapshot{
		Level:      string(risk.Level),
		Categories: categories,
		Reason:     strings.TrimSpace(risk.Reason),
	}
}

// SandboxBlockSnapshotFromBlock converts a sandbox.Block
// into its typed snapshot. A nil input returns a zero snapshot so
// the helper is safe to call when the decision did not produce a
// block.
func SandboxBlockSnapshotFromBlock(block *sandbox.Block) *SandboxBlockSnapshot {
	if block == nil {
		return nil
	}
	return &SandboxBlockSnapshot{
		Code:        string(block.Code),
		ToolName:    strings.TrimSpace(block.ToolName),
		Action:      string(block.Action),
		Path:        strings.TrimSpace(block.Path),
		Reason:      strings.TrimSpace(block.Reason),
		Recoverable: block.Recoverable,
	}
}

// SandboxBackendSnapshotFromBackend converts a sandbox.Backend into
// its typed snapshot. Every field is copied verbatim.
func SandboxBackendSnapshotFromBackend(backend sandbox.Backend) SandboxBackendSnapshot {
	return SandboxBackendSnapshot{
		Name:            string(backend.Name),
		Available:       backend.Available,
		Platform:        strings.TrimSpace(backend.Platform),
		Fallback:        backend.Fallback,
		CommandWrapping: backend.CommandWrapping,
		NativeIsolation: backend.NativeIsolation,
		Executable:      strings.TrimSpace(backend.Executable),
		Message:         strings.TrimSpace(backend.Message),
	}
}

// SandboxPlanSnapshotFromPlan converts a sandbox.BackendPlan into its
// typed snapshot. Each entry of the Restrictions slice is trimmed,
// the slice is sorted alphabetically, and the result is copied so
// the snapshot does not alias the input. WriteRoots is left unset
// because a BackendPlan carries no engine scope; callers that hold
// the live engine can populate it from engine.Scope().Roots().
func SandboxPlanSnapshotFromPlan(plan sandbox.BackendPlan) SandboxPlanSnapshot {
	restrictions := make([]string, 0, len(plan.Restrictions))
	for _, restriction := range plan.Restrictions {
		if trimmed := strings.TrimSpace(restriction); trimmed != "" {
			restrictions = append(restrictions, trimmed)
		}
	}
	sort.Strings(restrictions)
	return SandboxPlanSnapshot{
		Policy:        SandboxPolicySnapshotFromPolicy(plan.Policy),
		Backend:       SandboxBackendSnapshotFromBackend(plan.Backend),
		Restrictions:  restrictions,
		WorkspaceRoot: strings.TrimSpace(plan.WorkspaceRoot),
	}
}

// SandboxDecisionSnapshotFromDecision converts a sandbox.Decision
// into its typed snapshot. The optional Grant and Block fields
// are converted with their respective helpers so the snapshot
// always carries the same JSON shape regardless of the decision
// outcome.
func SandboxDecisionSnapshotFromDecision(decision sandbox.Decision) SandboxDecisionSnapshot {
	snapshot := SandboxDecisionSnapshot{
		Action:       string(decision.Action),
		Reason:       strings.TrimSpace(decision.Reason),
		Risk:         SandboxRiskSnapshotFromRisk(decision.Risk),
		GrantMatched: decision.GrantMatched,
	}
	if decision.Grant != nil {
		grant := SandboxGrantSnapshotFromGrant(*decision.Grant)
		snapshot.Grant = &grant
	}
	snapshot.Block = SandboxBlockSnapshotFromBlock(decision.Block)
	return snapshot
}
