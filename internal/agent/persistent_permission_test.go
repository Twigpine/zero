package agent

import (
	"context"
	"testing"

	"github.com/Twigpine/zero/internal/sandbox"
	"github.com/Twigpine/zero/internal/tools"
)

// refusingTool declares that its approval must never be remembered.
type refusingTool struct{ name string }

func (t refusingTool) Name() string                                   { return t.name }
func (refusingTool) Description() string                              { return "" }
func (refusingTool) Parameters() tools.Schema                         { return tools.Schema{} }
func (refusingTool) Safety() tools.Safety                             { return tools.Safety{Permission: tools.PermissionPrompt} }
func (refusingTool) Run(context.Context, map[string]any) tools.Result { return tools.Result{} }
func (refusingTool) RefusesPersistentPermission() bool                { return true }

// ordinaryTool declares nothing, so the name list decides for it.
type ordinaryTool struct{ name string }

func (t ordinaryTool) Name() string                                   { return t.name }
func (ordinaryTool) Description() string                              { return "" }
func (ordinaryTool) Parameters() tools.Schema                         { return tools.Schema{} }
func (ordinaryTool) Safety() tools.Safety                             { return tools.Safety{Permission: tools.PermissionPrompt} }
func (ordinaryTool) Run(context.Context, map[string]any) tools.Result { return tools.Result{} }

// A TOOL'S OWN REFUSAL WINS over the name list.
//
// The list refuses bash, exec_command, write_stdin and apply_patch because
// "always allow" is too broad a standing grant for them. It cannot describe a
// tool that RUNS those — and orchestrate can be given bash by a plan, so
// remembering it would be strictly broader than the refusal the list enforces.
func TestAToolCanRefuseToBeRemembered(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(refusingTool{name: "refuses"})
	registry.Register(ordinaryTool{name: "ordinary"})
	options := Options{Registry: registry}

	if permissionSupportsPersistentDecision("refuses", options) {
		t.Fatal("a tool that refuses persistence was offered it")
	}
	if !permissionSupportsPersistentDecision("ordinary", options) {
		t.Fatal("an ordinary tool lost its persistent approval")
	}
}

// The NAME LIST still applies to tools that declare nothing, so this adds a
// rule rather than replacing one.
func TestTheExistingNameRefusalsAreUnchanged(t *testing.T) {
	options := Options{Registry: tools.NewRegistry()}
	for _, name := range []string{"bash", "exec_command", "write_stdin", "apply_patch"} {
		if permissionSupportsPersistentDecision(name, options) {
			t.Errorf("%q must not be persistently approvable", name)
		}
	}
	for _, name := range []string{"read_file", "write_file", "edit_file"} {
		if !permissionSupportsPersistentDecision(name, options) {
			t.Errorf("%q lost its persistent approval", name)
		}
	}
}

// A nil registry must not crash and must not start refusing everything — the
// name list is the fallback, not an empty answer.
func TestPersistentDecisionsSurviveANilRegistry(t *testing.T) {
	if permissionSupportsPersistentDecision("bash", Options{}) {
		t.Fatal("bash became persistable with no registry")
	}
	if !permissionSupportsPersistentDecision("read_file", Options{}) {
		t.Fatal("read_file lost persistence with no registry")
	}
}

// THE CONSEQUENCE, at the decision list a prompt actually offers. Asserting the
// predicate alone would pass against a caller that ignored it.
func TestARefusingToolIsNeverOfferedAlwaysAllow(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(refusingTool{name: "refuses"})
	registry.Register(ordinaryTool{name: "ordinary"})
	engine := sandbox.NewEngine(sandbox.EngineOptions{})
	options := Options{Registry: registry, Sandbox: engine}

	offered := func(name string) bool {
		event := PermissionEvent{ToolName: name, Action: PermissionActionPrompt}
		for _, decision := range availablePermissionDecisions(event, nil, options) {
			if decision == PermissionDecisionAlwaysAllow {
				return true
			}
		}
		return false
	}
	if offered("refuses") {
		t.Fatal("a refusing tool was offered always-allow in the prompt")
	}
	if !engine.CanPersistGrants() {
		t.Skip("this engine cannot persist grants, so the positive case is unreachable")
	}
	if !offered("ordinary") {
		t.Fatal("an ordinary tool lost always-allow; the refusal is being applied to everything")
	}
}
