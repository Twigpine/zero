package zerocommands

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/sandbox"
)

func TestSandboxPolicySnapshotFromPolicyFillsEffectiveMode(t *testing.T) {
	enforced := SandboxPolicySnapshotFromPolicy(sandbox.Policy{
		Mode:             sandbox.ModeEnforce,
		Network:          sandbox.NetworkDeny,
		EnforceWorkspace: true,
	})
	if enforced.EffectiveMode != string(sandbox.ModeEnforce) {
		t.Fatalf("EffectiveMode = %q, want %q", enforced.EffectiveMode, sandbox.ModeEnforce)
	}
	if !enforced.EnforceWorkspace {
		t.Fatalf("boolean fields not preserved: %#v", enforced)
	}

	disabled := SandboxPolicySnapshotFromPolicy(sandbox.Policy{Mode: sandbox.ModeDisabled})
	if disabled.EffectiveMode != string(sandbox.ModeDisabled) {
		t.Fatalf("disabled EffectiveMode = %q, want %q", disabled.EffectiveMode, sandbox.ModeDisabled)
	}

	defaulted := SandboxPolicySnapshotFromPolicy(sandbox.Policy{})
	if defaulted.EffectiveMode != string(sandbox.ModeEnforce) {
		t.Fatalf("defaulted EffectiveMode = %q, want %q (runtime default)", defaulted.EffectiveMode, sandbox.ModeEnforce)
	}
	if defaulted.Mode != "" {
		t.Fatalf("defaulted Mode = %q, want empty (untouched)", defaulted.Mode)
	}
}

func TestSandboxRiskSnapshotFromRiskSortsCategoriesAndTrimsReason(t *testing.T) {
	risk := sandbox.Risk{
		Level:      sandbox.RiskHigh,
		Categories: []string{"shell", "absolute_path", "destructive"},
		Reason:     "  destructive shell command  ",
	}
	snapshot := SandboxRiskSnapshotFromRisk(risk)
	if snapshot.Level != string(sandbox.RiskHigh) {
		t.Fatalf("Level = %q, want %q", snapshot.Level, sandbox.RiskHigh)
	}
	if snapshot.Reason != "destructive shell command" {
		t.Fatalf("Reason not trimmed: %q", snapshot.Reason)
	}
	want := []string{"absolute_path", "destructive", "shell"}
	if len(snapshot.Categories) != len(want) {
		t.Fatalf("Categories length = %d, want %d", len(snapshot.Categories), len(want))
	}
	for index, category := range want {
		if snapshot.Categories[index] != category {
			t.Fatalf("Categories[%d] = %q, want %q", index, snapshot.Categories[index], category)
		}
	}

	snapshot = SandboxRiskSnapshotFromRisk(sandbox.Risk{Level: sandbox.RiskLow, Categories: nil})
	if snapshot.Categories != nil {
		t.Fatalf("expected nil Categories for nil input, got %#v", snapshot.Categories)
	}
}

func TestSandboxBlockSnapshotFromBlockHandlesNilAndTrims(t *testing.T) {
	if got := SandboxBlockSnapshotFromBlock(nil); got != nil {
		t.Fatalf("expected nil snapshot for nil block, got %#v", got)
	}

	block := &sandbox.Block{
		Code:        sandbox.BlockOutsideWorkspace,
		ToolName:    "  write_file  ",
		Action:      sandbox.ActionDeny,
		Path:        "  /etc/hosts  ",
		Reason:      "  path escapes workspace  ",
		Recoverable: true,
	}
	snapshot := SandboxBlockSnapshotFromBlock(block)
	if snapshot == nil {
		t.Fatal("expected non-nil snapshot for non-nil block")
	}
	if snapshot.Code != string(sandbox.BlockOutsideWorkspace) {
		t.Fatalf("Code = %q, want %q", snapshot.Code, sandbox.BlockOutsideWorkspace)
	}
	if snapshot.ToolName != "write_file" {
		t.Fatalf("ToolName not trimmed: %q", snapshot.ToolName)
	}
	if snapshot.Path != "/etc/hosts" {
		t.Fatalf("Path not trimmed: %q", snapshot.Path)
	}
	if snapshot.Reason != "path escapes workspace" {
		t.Fatalf("Reason not trimmed: %q", snapshot.Reason)
	}
	if !snapshot.Recoverable {
		t.Fatal("Recoverable should round-trip")
	}
}

func TestSandboxBackendSnapshotFromBackendCopiesAllFields(t *testing.T) {
	backend := sandbox.Backend{
		Name:            sandbox.BackendLinuxBwrap,
		Available:       true,
		Platform:        "linux",
		Fallback:        false,
		CommandWrapping: true,
		NativeIsolation: true,
		Executable:      "  bwrap  ",
		Message:         "  bubblewrap is available  ",
	}
	snapshot := SandboxBackendSnapshotFromBackend(backend)
	if snapshot.Name != string(sandbox.BackendLinuxBwrap) {
		t.Fatalf("Name = %q, want %q", snapshot.Name, sandbox.BackendLinuxBwrap)
	}
	if !snapshot.Available || !snapshot.CommandWrapping || !snapshot.NativeIsolation {
		t.Fatalf("boolean fields not preserved: %#v", snapshot)
	}
	if snapshot.Executable != "bwrap" {
		t.Fatalf("Executable not trimmed: %q", snapshot.Executable)
	}
	if snapshot.Message != "bubblewrap is available" {
		t.Fatalf("Message not trimmed: %q", snapshot.Message)
	}
}

func TestSandboxPlanSnapshotFromPlanSortsRestrictionsAndTrimsRoot(t *testing.T) {
	plan := sandbox.BackendPlan{
		Backend: sandbox.Backend{Name: sandbox.BackendMacOSSeatbelt, Platform: "darwin"},
		Policy:  sandbox.Policy{Mode: sandbox.ModeEnforce, Network: sandbox.NetworkDeny},
		Restrictions: []string{
			"  native process isolation unavailable on darwin  ",
			"network access requires approval",
		},
		WorkspaceRoot: "  /repo  ",
	}
	snapshot := SandboxPlanSnapshotFromPlan(plan)
	if snapshot.Policy.EffectiveMode != string(sandbox.ModeEnforce) {
		t.Fatalf("Policy.EffectiveMode = %q, want %q", snapshot.Policy.EffectiveMode, sandbox.ModeEnforce)
	}
	if snapshot.Backend.Name != string(sandbox.BackendMacOSSeatbelt) {
		t.Fatalf("Backend.Name = %q, want %q", snapshot.Backend.Name, sandbox.BackendMacOSSeatbelt)
	}
	if snapshot.WorkspaceRoot != "/repo" {
		t.Fatalf("WorkspaceRoot not trimmed: %q", snapshot.WorkspaceRoot)
	}
	if len(snapshot.Restrictions) != 2 {
		t.Fatalf("Restrictions length = %d, want 2", len(snapshot.Restrictions))
	}
	if snapshot.Restrictions[0] != "native process isolation unavailable on darwin" {
		t.Fatalf("Restrictions[0] not trimmed: %q", snapshot.Restrictions[0])
	}
	if snapshot.Restrictions[1] != "network access requires approval" {
		t.Fatalf("Restrictions[1] = %q, want %q", snapshot.Restrictions[1], snapshot.Restrictions[1])
	}
}

func TestSandboxPlanSnapshotWriteRootsJSON(t *testing.T) {
	snapshot := SandboxPlanSnapshot{WriteRoots: []string{"/ws", "/extra"}}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	if !strings.Contains(string(encoded), `"writeRoots":["/ws","/extra"]`) {
		t.Fatalf("expected writeRoots to flow through to JSON, got %q", string(encoded))
	}

	bare := SandboxPlanSnapshotFromPlan(sandbox.BackendPlan{WorkspaceRoot: "/repo"})
	encoded, err = json.Marshal(bare)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	if strings.Contains(string(encoded), `"writeRoots"`) {
		t.Fatalf("writeRoots should be omitted when unset (JSON shape stable), got %q", string(encoded))
	}
}

func TestSandboxDecisionSnapshotFromDecisionAllowBranchHasNoBlock(t *testing.T) {
	decision := sandbox.Decision{
		Action: sandbox.ActionAllow,
		Reason: "tool safety allows execution",
		Risk:   sandbox.Risk{Level: sandbox.RiskLow, Categories: []string{"read"}, Reason: "low risk: read"},
	}
	snapshot := SandboxDecisionSnapshotFromDecision(decision)
	if snapshot.Action != string(sandbox.ActionAllow) {
		t.Fatalf("Action = %q, want %q", snapshot.Action, sandbox.ActionAllow)
	}
	if snapshot.GrantMatched {
		t.Fatal("GrantMatched should be false when no grant was matched")
	}
	if snapshot.Grant != nil {
		t.Fatalf("Grant should be nil for grantless decision, got %#v", snapshot.Grant)
	}
	if snapshot.Block != nil {
		t.Fatalf("Block should be nil for allow decision, got %#v", snapshot.Block)
	}
	if snapshot.Risk.Level != string(sandbox.RiskLow) {
		t.Fatalf("Risk.Level = %q, want %q", snapshot.Risk.Level, sandbox.RiskLow)
	}
}

func TestSandboxDecisionSnapshotFromDecisionPersistentDenyCarriesGrantAndBlock(t *testing.T) {
	decision := sandbox.Decision{
		Action:       sandbox.ActionDeny,
		Reason:       "persistent sandbox deny grant matched",
		Risk:         sandbox.Risk{Level: sandbox.RiskHigh, Categories: []string{"shell"}, Reason: "high risk: shell"},
		GrantMatched: true,
		Grant: &sandbox.Grant{
			ToolName:   "bash",
			Decision:   sandbox.GrantDeny,
			ApprovedAt: "2026-06-04T10:00:00Z",
			Reason:     "user marked bash as destructive",
		},
		Block: &sandbox.Block{
			Code:        sandbox.BlockPersistentDeny,
			ToolName:    "bash",
			Action:      sandbox.ActionDeny,
			Reason:      "persistent sandbox deny grant matched",
			Recoverable: true,
		},
	}
	snapshot := SandboxDecisionSnapshotFromDecision(decision)
	if !snapshot.GrantMatched {
		t.Fatal("GrantMatched should be true")
	}
	if snapshot.Grant == nil {
		t.Fatal("Grant should be present")
	}
	if snapshot.Grant.ToolName != "bash" {
		t.Fatalf("Grant.ToolName = %q, want bash", snapshot.Grant.ToolName)
	}
	if snapshot.Grant.Decision != string(sandbox.GrantDeny) {
		t.Fatalf("Grant.Decision = %q, want %q", snapshot.Grant.Decision, sandbox.GrantDeny)
	}
	if snapshot.Block == nil {
		t.Fatal("Block should be present on deny decision")
	}
	if snapshot.Block.Code != string(sandbox.BlockPersistentDeny) {
		t.Fatalf("Block.Code = %q, want %q", snapshot.Block.Code, sandbox.BlockPersistentDeny)
	}
}

func TestSandboxDecisionSnapshotJSONShapeIsStable(t *testing.T) {
	snapshot := SandboxDecisionSnapshot{
		Action: string(sandbox.ActionPrompt),
		Reason: "tool requires approval before execution",
		Risk: SandboxRiskSnapshot{
			Level:      string(sandbox.RiskHigh),
			Categories: []string{"shell"},
			Reason:     "high risk: shell",
		},
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	decoded := map[string]any{}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v", err)
	}
	for _, key := range []string{"action", "reason", "risk", "grantMatched"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("expected key %q in decision JSON, got %q", key, string(encoded))
		}
	}
	if _, ok := decoded["block"]; ok {
		t.Fatalf("block should be omitted when nil, got %q", string(encoded))
	}
	if _, ok := decoded["grant"]; ok {
		t.Fatalf("grant should be omitted when nil, got %q", string(encoded))
	}
}

func TestSandboxPolicySnapshotJSONOmitsEmptyEffectiveMode(t *testing.T) {
	policy := SandboxPolicySnapshot{Mode: string(sandbox.ModeEnforce), Network: string(sandbox.NetworkDeny)}
	policy.EffectiveMode = ""
	encoded, err := json.Marshal(policy)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	if strings.Contains(string(encoded), `"effectiveMode"`) {
		t.Fatalf("expected effectiveMode omitted when empty, got %q", string(encoded))
	}
}
