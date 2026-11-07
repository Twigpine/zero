package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/specialist"
	"github.com/Twigpine/zero/internal/tools"
)

// readOnlyRegistry is the tool set a PLAN TASK actually holds — pure reads, no
// ask_user and no request_permissions.
//
// Deliberately not CoreReadOnlyToolsScoped, which despite its name also carries
// ask_user and request_permissions (both EffectInteractive). A run holding
// those genuinely can prompt, so the policy applies to it and the gate keeps
// it; that is why this helper narrows to the plan grant instead.
//
// IT READS THE AUTHORITATIVE LIST. It used to enumerate its own copy, and the
// copy differed from specialist.PlanReadOnlyToolNames by exactly one entry —
// update_plan — which is the entry that broke this. The test asserted the
// policy was dropped for a grant that was not the grant, and passed for the
// whole period a real plan child carried the policy. A test that builds its own
// version of the thing under test cannot catch the thing under test changing.
func readOnlyRegistry(t *testing.T) *tools.Registry {
	t.Helper()
	planTools := map[string]bool{}
	for _, name := range specialist.PlanReadOnlyToolNames() {
		planTools[name] = true
	}
	registry := tools.NewRegistry()
	registered := 0
	for _, tool := range tools.CoreReadOnlyToolsScoped(t.TempDir(), nil) {
		if planTools[tool.Name()] {
			registry.Register(tool)
			registered++
		}
	}
	// WHAT THIS CAN AND CANNOT PROVE, said plainly. A grant name that lives
	// outside the scoped core set is invisible here — update_plan was exactly
	// that, which is why re-adding it does not fail these tests. This half
	// proves "a run holding these read-only tools drops the policy"; the other
	// half, that every name in the grant IS read-only by capability, is
	// TestEveryPlanGrantToolIsReadOnlyByCapability in internal/cli, against the
	// real registry. Neither alone is the guarantee.
	if registered == 0 {
		t.Fatal("no plan-grant tool was registered; this fixture proves nothing")
	}
	return registry
}

func mutatingRegistry(t *testing.T) *tools.Registry {
	t.Helper()
	registry := readOnlyRegistry(t)
	for _, tool := range tools.CoreWriteToolsScoped(t.TempDir(), nil) {
		registry.Register(tool)
	}
	return registry
}

// An interactive tool is not a read: a run that can prompt keeps the policy.
func TestAnInteractiveToolKeepsTheConfirmationPolicy(t *testing.T) {
	registry := tools.NewRegistry()
	for _, tool := range tools.CoreReadOnlyToolsScoped(t.TempDir(), nil) {
		registry.Register(tool)
	}
	prompt := buildSystemPrompt(Options{
		Registry: registry, PermissionMode: PermissionModeAuto, Cwd: t.TempDir(),
	})
	if !strings.Contains(prompt, "Confirmation Policy") {
		t.Fatal("ask_user and request_permissions can prompt, so the policy still governs this run")
	}
}

// A run that cannot change anything carries no confirmation policy. Measured at
// ~2,527 of a plan child's 7,648 prompt tokens — policy for capabilities the
// child provably lacks, multiplied by every task in a plan.
func TestAReadOnlyRunDropsTheConfirmationPolicy(t *testing.T) {
	prompt := buildSystemPrompt(Options{
		Registry:       readOnlyRegistry(t),
		PermissionMode: PermissionModeAuto,
		Cwd:            t.TempDir(),
	})
	for _, marker := range []string{"Confirmation Policy", "ALWAYS CONFIRM", "PRE-APPROVAL"} {
		if strings.Contains(prompt, marker) {
			t.Errorf("a read-only run still carries %q", marker)
		}
	}
}

// THE FAIL-CLOSED DIRECTION, which is the one that matters: anything that can
// change something keeps the policy.
func TestAnyMutatingCapabilityKeepsTheConfirmationPolicy(t *testing.T) {
	cases := map[string]Options{
		"write tools present": {Registry: mutatingRegistry(t), PermissionMode: PermissionModeAuto},
		"no registry at all":  {PermissionMode: PermissionModeAuto},
		"empty registry":      {Registry: tools.NewRegistry(), PermissionMode: PermissionModeAuto},
	}
	for name, options := range cases {
		options.Cwd = t.TempDir()
		if !strings.Contains(buildSystemPrompt(options), "Confirmation Policy") {
			t.Errorf("%s: the confirmation policy was dropped; this path must fail closed", name)
		}
	}
}

// A tool whose effect is not positively read-only keeps the policy, even
// alongside read tools. MCP tools, plugin tools and the specialist tools are all
// EffectUnknown, so an ordinary session is unaffected by this change.
func TestAnUnknownEffectToolKeepsTheConfirmationPolicy(t *testing.T) {
	registry := readOnlyRegistry(t)
	registry.Register(&silentProbeTool{name: "mystery", got: new(bool)})

	prompt := buildSystemPrompt(Options{
		Registry: registry, PermissionMode: PermissionModeAuto, Cwd: t.TempDir(),
	})
	if !strings.Contains(prompt, "Confirmation Policy") {
		t.Fatal("a tool of unknown effect must keep the policy — unknown is not read-only")
	}
}

// A tool hidden by the run's operator filters does not count: what matters is
// what the run can actually reach, judged by the same gate the loop advertises
// through.
func TestFilteredOutMutatorsDoNotKeepThePolicy(t *testing.T) {
	registry := mutatingRegistry(t)
	var mutators []string
	for _, tool := range registry.All() {
		if tools.CapabilitiesOf(tool).Effect != tools.EffectReadOnly {
			mutators = append(mutators, tool.Name())
		}
	}
	if len(mutators) == 0 {
		t.Fatal("setup: expected the write tools to register")
	}

	prompt := buildSystemPrompt(Options{
		Registry:      registry,
		DisabledTools: mutators,
		Cwd:           t.TempDir(),
	})
	if strings.Contains(prompt, "Confirmation Policy") {
		t.Fatal("every mutator is filtered out of this run, so the policy has nothing to govern")
	}
}

// THE FAIL-OPEN THIS FUNCTION FIRST SHIPPED WITH, pinned.
//
// The gate originally used ToolVisible, which applies the permission mode's
// ADVERTISING rules on top of the operator filters. A write tool that auto mode
// does not advertise is still held and still callable once approved — so a run
// launched with --enabled-tools read_file,grep,glob,write_file counted as
// read-only and lost its confirmation policy. Exactly inverse to the point.
//
// Found by driving the binary; every unit test at the time passed.
func TestAnUnadvertisedMutatorStillKeepsThePolicy(t *testing.T) {
	registry := mutatingRegistry(t)
	for _, mode := range []PermissionMode{PermissionModeAuto, PermissionModeAsk, "", PermissionModeSpecDraft} {
		prompt := buildSystemPrompt(Options{
			Registry: registry, PermissionMode: mode, Cwd: t.TempDir(),
		})
		if !strings.Contains(prompt, "Confirmation Policy") {
			t.Errorf("permission mode %q: a held-but-unadvertised mutator dropped the policy", mode)
		}
	}
}

// The gate asks whether the run HOLDS a mutator, so the permission mode must
// not change the answer. A mode-dependent system prompt would also move the
// cache prefix between modes for no reason.
func TestThePolicyDecisionIsIndependentOfPermissionMode(t *testing.T) {
	for _, registry := range []*tools.Registry{readOnlyRegistry(t), mutatingRegistry(t)} {
		var first string
		for index, mode := range []PermissionMode{PermissionModeAuto, PermissionModeAsk, PermissionModeUnsafe, ""} {
			prompt := buildSystemPrompt(Options{Registry: registry, PermissionMode: mode, Cwd: t.TempDir()})
			has := strings.Contains(prompt, "Confirmation Policy")
			if index == 0 {
				first = fmt.Sprint(has)
				continue
			}
			if fmt.Sprint(has) != first {
				t.Fatalf("permission mode %q changed the policy decision", mode)
			}
		}
	}
}

// THE INTERACTION between this gate and the additivity guarantee.
//
// The gate reads the registered tool set, so registering a posture-gated tool
// changed the prompt even with the posture OFF — breaking the guarantee that
// registering it is invisible. Caught by
// TestPostureOffPrefixUnchangedByRegisteringTheTool, which is what that test is
// for. A tool the run can never call now says nothing about what the run can
// do.
func TestAPermanentlyDeniedToolDoesNotChangeThePrompt(t *testing.T) {
	base := readOnlyRegistry(t)
	withDenied := readOnlyRegistry(t)
	withDenied.Register(&deniedProbeTool{name: "gated"})

	// ONE cwd for both: the prompt embeds the working directory, so two
	// t.TempDir() calls would differ for a reason that has nothing to do with
	// the tool set.
	cwd := t.TempDir()
	before := buildSystemPrompt(Options{Registry: base, Cwd: cwd})
	after := buildSystemPrompt(Options{Registry: withDenied, Cwd: cwd})
	if before != after {
		t.Fatalf("registering a permanently denied tool changed the prompt (%d -> %d bytes)",
			len(before), len(after))
	}
}

// ...but a tool that varies its permission by argument is never ruled out from
// its static safety, however that reads. Fail closed.
func TestAnArgsPermissionedToolIsNeverTreatedAsDenied(t *testing.T) {
	registry := readOnlyRegistry(t)
	registry.Register(&argsPermissionedProbeTool{deniedProbeTool{name: "varies"}})

	if !strings.Contains(buildSystemPrompt(Options{Registry: registry, Cwd: t.TempDir()}), "Confirmation Policy") {
		t.Fatal("a tool whose permission depends on its arguments must never be ruled out statically")
	}
}

type deniedProbeTool struct{ name string }

func (t *deniedProbeTool) Name() string        { return t.name }
func (t *deniedProbeTool) Description() string { return "gated" }
func (t *deniedProbeTool) Parameters() tools.Schema {
	return tools.Schema{Type: "object", Properties: map[string]tools.PropertySchema{}}
}
func (t *deniedProbeTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectShell, Permission: tools.PermissionDeny}
}
func (t *deniedProbeTool) Run(context.Context, map[string]any) tools.Result {
	return tools.Result{Status: tools.StatusOK}
}

type argsPermissionedProbeTool struct{ deniedProbeTool }

func (t *argsPermissionedProbeTool) PermissionForArgs(map[string]any) tools.Permission {
	return tools.PermissionAllow
}
