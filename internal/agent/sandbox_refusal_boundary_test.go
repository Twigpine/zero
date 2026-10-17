package agent

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/sandbox"
	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// deniedWriteTool records every execution. Its body must never run: the sandbox
// refuses the call at the registry boundary, before the tool is reached.
type deniedWriteTool struct{ ran int }

func (t *deniedWriteTool) Name() string        { return "write_file" }
func (t *deniedWriteTool) Description() string { return "test write tool" }
func (t *deniedWriteTool) Parameters() tools.Schema {
	return tools.Schema{
		Type:       "object",
		Properties: map[string]tools.PropertySchema{"path": {Type: "string"}},
		Required:   []string{"path"},
	}
}

func (t *deniedWriteTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectWrite, Permission: tools.PermissionAllow, Reason: "writes files"}
}

func (t *deniedWriteTool) Run(context.Context, map[string]any) tools.Result {
	t.ran++
	return tools.Result{Status: tools.StatusOK, Output: "wrote"}
}

// THROUGH THE REAL SANDBOX, NOT A FAKE STANDING WHERE IT WOULD BE.
//
// The neighbouring uncategorized test deliberately hand-builds the shape a
// refusal has when no category survives, which is a fact about the LOOP. This
// one covers the producer: a real sandbox.Engine evaluation denies the call,
// the registry turns that into a categorized refusal, and the guard keys on the
// category. Without it, reverting refusalResult(..., PolicyRefusalSandboxDenied)
// in registry.go to a plain errorResult left both the agent and tools suites
// green, so nothing depended on the marker at all.
//
// Every assertion here fails if a different link breaks: ran proves the body was
// skipped, the request count proves the CATEGORY bound halted it rather than the
// content-blind one, and the wording proves the provenance survived conversion.
func TestRunHaltsARealSandboxDenialOnTheCategoryBound(t *testing.T) {
	workspace := t.TempDir()
	forbidden := t.TempDir()
	policy := sandbox.DefaultPolicy()
	// Applies whenever the sandbox is enforcing, so this does not depend on the
	// workspace boundary being reachable from a synthetic request.
	policy.DenyWrite = []string{forbidden}

	tool := &deniedWriteTool{}
	registry := tools.NewRegistry()
	registry.Register(tool)

	// A different path every call. Note that errorSignature truncates at 80
	// characters and the message opens with a fixed 40-character prefix followed
	// by a long temp root, so the signatures collide here anyway: the request
	// count below pins WHICH bound halted the run, but it does not by itself
	// prove the streak was held by the category. The wording assertion is the one
	// that discriminates, and it is the one that fails when the marker is gone.
	turns := make([][]zeroruntime.StreamEvent, 0, toolFailureStopAt+4)
	for i := range toolFailureStopAt + 4 {
		target := filepath.Join(forbidden, "escape-"+strconv.Itoa(i)+".txt")
		turns = append(turns, toolTurn("c"+strconv.Itoa(i), "write_file", `{"path":`+strconv.Quote(target)+`}`))
	}
	provider := &mockProvider{turns: turns}

	result, err := Run(context.Background(), "write the files", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		MaxTurns:       len(turns) + 5,
		Cwd:            workspace,
		Sandbox: sandbox.NewEngine(sandbox.EngineOptions{
			WorkspaceRoot: workspace,
			Policy:        policy,
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	if tool.ran != 0 {
		t.Errorf("the tool body ran %d times; the sandbox must refuse before execution", tool.ran)
	}
	if len(provider.requests) != toolFailureStopAt {
		t.Errorf("the run made %d turns, want the category bound at %d; a categorized refusal whose prose varies must still halt on its category",
			len(provider.requests), toolFailureStopAt)
	}
	if want := toolFailureStopAnswer("write_file", toolFailureStopAt, toolFailureCauseSameRefusal); result.FinalAnswer != want {
		t.Errorf("final answer =\n  %q\nwant\n  %q", result.FinalAnswer, want)
	}
	if !strings.Contains(result.FinalAnswer, "was refused") {
		t.Errorf("the refusal provenance did not survive to the answer: %q", result.FinalAnswer)
	}
}
