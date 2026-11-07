package specialist

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/streamjson"
	"github.com/Twigpine/zero/internal/tools"
)

func writeCapableArgs() map[string]any {
	return map[string]any{
		"name": "fixit",
		"tasks": []any{
			map[string]any{"id": "a", "prompt": "read it"},
			map[string]any{"id": "b", "prompt": "fix it", "depends_on": []any{"a"}, "tools": []any{"write_file"}},
		},
		"budget": map[string]any{"max_workers": float64(1)},
	}
}

func writeCapableTool(t *testing.T, isolate PlanIsolator) *OrchestrateTool {
	t.Helper()
	return &OrchestrateTool{
		PostureActive: func() bool { return true },
		RunTask: func(context.Context, PlanTaskRequest) (TaskResult, error) {
			return TaskResult{Outcome: TaskSucceeded}, nil
		},
		ParentTools: append(PlanReadOnlyToolNames(), "write_file"),
		Isolate:     isolate,
	}
}

// THE ARC, ASSERTED AS ONE THING. A write-capable plan must PROMPT (step 1 gave
// the card something to show) and must be ISOLATED (step 2 gave it somewhere to
// write). Either missing and the relaxation in step 3 was not safe to make.
func TestAWriteCapablePlanPromptsAndIsIsolated(t *testing.T) {
	tool := writeCapableTool(t, nil)

	if got := tool.PermissionForArgs(writeCapableArgs()); got != tools.PermissionPrompt {
		t.Fatalf("permission = %v; a plan that can write must ask", got)
	}
	plan, err := ParsePlan(writeCapableArgs(), Limits{MaxTasks: 20, ParentTools: tool.ParentTools})
	if err != nil {
		t.Fatalf("a write-capable plan must now admit: %v", err)
	}
	if !plan.RequiresIsolation() {
		t.Fatal("a write-capable plan does not require isolation")
	}
	// ...and with no isolator it does not run at all.
	if _, err := resolvePlanWorkspace(context.Background(), plan, nil); err == nil {
		t.Fatal("a write-capable plan ran with no isolation available")
	}
}

// A READ-ONLY PLAN IS UNCHANGED by all of it: no prompt, no worktree. The
// friction is paid only by the plans that earn it.
func TestAReadOnlyPlanStillDoesNotPromptOrIsolate(t *testing.T) {
	tool := writeCapableTool(t, nil)
	args := map[string]any{
		"tasks":  []any{map[string]any{"id": "a", "prompt": "look", "tools": []any{"grep"}}},
		"budget": map[string]any{"max_workers": float64(1)},
	}
	if got := tool.PermissionForArgs(args); got != tools.PermissionAllow {
		t.Fatalf("permission = %v; a read-only plan must not prompt", got)
	}
	plan, err := ParsePlan(args, Limits{MaxTasks: 20, ParentTools: tool.ParentTools})
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	if plan.RequiresIsolation() {
		t.Fatal("a read-only plan asked for a worktree")
	}
}

// THE POSTURE STILL GATES EVERYTHING. PermissionForArgs must deny with the
// posture off whatever the arguments say, or the args-aware permission has
// become a way around the gate.
func TestThePostureGateSurvivesTheArgsAwarePermission(t *testing.T) {
	off := &OrchestrateTool{PostureActive: func() bool { return false }}
	for _, args := range []map[string]any{writeCapableArgs(), {}, {"saved": "x"}} {
		if got := off.PermissionForArgs(args); got != tools.PermissionDeny {
			t.Fatalf("permission = %v with the posture off; it must deny", got)
		}
	}
	if !off.PermanentlyDenied() {
		t.Fatal("with the posture off the tool must report itself permanently denied")
	}
	on := &OrchestrateTool{PostureActive: func() bool { return true }}
	if on.PermanentlyDenied() {
		t.Fatal("with the posture on the tool must not report itself denied")
	}
}

// ERRING TOWARD ASKING. A saved plan's tasks are in a file the permission check
// has not opened, and an unreadable entry could be anything. A wrong guess
// toward prompting costs one prompt; the other way runs write tasks unasked.
func TestAnUnreadablePlanErrsTowardPrompting(t *testing.T) {
	tool := writeCapableTool(t, nil)
	for _, args := range []map[string]any{
		{"saved": "sweep"},
		{"tasks": []any{"not-an-object"}},
		{"tasks": []any{map[string]any{"id": "a", "tools": []any{"something_new"}}}},
	} {
		if got := tool.PermissionForArgs(args); got != tools.PermissionPrompt {
			t.Errorf("args %v gave %v; an unreadable plan must ask", args, got)
		}
	}
}

// WRITE TOOLS ARE AN ALLOW-LIST. "Anything not read-only" would hand a plan
// every future tool the moment it is registered.
func TestOnlyNamedWriteToolsArePermitted(t *testing.T) {
	limits := Limits{MaxTasks: 20, ParentTools: append(PlanGrantableToolNames(), "browser_open", "web_fetch")}
	for _, name := range []string{"browser_open", "web_fetch", "kill_shell", "made_up"} {
		args := map[string]any{
			"tasks":  []any{map[string]any{"id": "a", "prompt": "x", "tools": []any{name}}},
			"budget": map[string]any{"max_workers": float64(1)},
		}
		if _, err := ParsePlan(args, limits); err == nil {
			t.Errorf("tool %q was permitted; the write set is an allow-list", name)
		}
	}
	for _, name := range PlanWriteToolNames() {
		args := map[string]any{
			"tasks":  []any{map[string]any{"id": "a", "prompt": "x", "tools": []any{name}}},
			"budget": map[string]any{"max_workers": float64(1)},
		}
		if _, err := ParsePlan(args, limits); err != nil {
			t.Errorf("write tool %q must be permitted when the parent holds it: %v", name, err)
		}
	}
}

// The refusal NAMES what is available, both halves — a message that says "no"
// without saying "these instead" makes the caller guess.
func TestTheRefusalNamesBothToolSets(t *testing.T) {
	args := map[string]any{
		"tasks":  []any{map[string]any{"id": "a", "prompt": "x", "tools": []any{"browser_open"}}},
		"budget": map[string]any{"max_workers": float64(1)},
	}
	_, err := ParsePlan(args, Limits{MaxTasks: 20, ParentTools: []string{"browser_open", "read_file"}})
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "read_file") || !strings.Contains(err.Error(), "write_file") {
		t.Fatalf("the refusal must name what IS available: %v", err)
	}
}

// A NAMED WRITE TOOL ACTUALLY REACHES THE TASK. Admission permitting what
// dispatch drops would produce a task that validated and then ran with less than
// it asked for — silently.
func TestANamedWriteToolReachesTheTask(t *testing.T) {
	plan, err := ParsePlan(writeCapableArgs(), Limits{MaxTasks: 20, ParentTools: append(PlanReadOnlyToolNames(), "write_file")})
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	var writer Task
	for _, task := range plan.Tasks() {
		if task.ID == "b" {
			writer = task
		}
	}
	granted, err := planToolGrant(writer, append(PlanReadOnlyToolNames(), "write_file"))
	if err != nil {
		t.Fatalf("planToolGrant: %v", err)
	}
	if len(granted) != 1 || granted[0] != "write_file" {
		t.Fatalf("granted %v; the named write tool must reach the task", granted)
	}
}

// ORCHESTRATE MUST NOT BE REMEMBERABLE, and the reason is compositional: a
// read-only plan never prompts, so every prompt this tool produces is for a
// plan that can write — and a plan that can write can be given bash, for which
// the permission layer already refuses to persist an approval.
//
// Remembering "always allow orchestrate" would therefore be a strictly broader
// standing grant than the one that refusal exists to enforce, and it would
// disable the write-plan approval gate permanently with one keystroke.
func TestOrchestrateRefusesAPersistentApproval(t *testing.T) {
	tool := &OrchestrateTool{PostureActive: func() bool { return true }}
	if !tool.RefusesPersistentPermission() {
		t.Fatal("orchestrate allows its approval to be remembered, which permanently disables the write-plan gate")
	}
	// It stays approvable for THIS call, which is the whole point — only the
	// permanent form is withheld, exactly as bash behaves.
	if got := tool.PermissionForArgs(writeCapableArgs()); got != tools.PermissionPrompt {
		t.Fatalf("permission = %v; a write plan must still be approvable per call", got)
	}
	// And bash really is one of the tools a plan may name, which is what makes
	// the argument above true rather than merely plausible.
	granted := false
	for _, name := range PlanWriteToolNames() {
		if name == "bash" {
			granted = true
		}
	}
	if !granted {
		t.Fatal("bash is not in the plan write set; the compositional argument no longer holds and this rule needs rethinking")
	}
}

// THE PROMPT MUST NOT CONTRADICT THE GRANT. A write-capable plan is approved by
// the user and isolated in a worktree, and its task was still told "you have
// read-only tools … do not attempt to modify anything". Handed write_file and
// instructed not to use it, a model that obeys produces a plan that reports
// success and changed nothing.
func TestTheTaskPromptMatchesTheToolsItWasGranted(t *testing.T) {
	readOnly := planTaskManifest("explorer", "", "", []string{"read_file", "grep"})
	if !strings.Contains(readOnly.SystemPrompt, "read-only tools") {
		t.Errorf("a read-only task must still be told so:\n%s", readOnly.SystemPrompt)
	}
	if !strings.Contains(readOnly.SystemPrompt, "do not attempt to modify anything") {
		t.Errorf("a read-only task must still be told not to modify:\n%s", readOnly.SystemPrompt)
	}
	if !strings.Contains(readOnly.Metadata.Description, "Read-only") {
		t.Errorf("description = %q", readOnly.Metadata.Description)
	}

	writable := planTaskManifest("explorer", "", "", []string{"read_file", "write_file"})
	if strings.Contains(writable.SystemPrompt, "read-only tools") {
		t.Errorf("a task granted write_file must not be told its tools are read-only:\n%s", writable.SystemPrompt)
	}
	if strings.Contains(writable.SystemPrompt, "do not attempt to modify anything") {
		t.Errorf("a task granted write_file must not be told not to modify:\n%s", writable.SystemPrompt)
	}
	// THE BOUNDARY, not one phrasing of it. The worktree bounds WHERE a task may
	// write and cannot bound HOW MUCH, so the prompt has to — but asserting my
	// own wording made this a test of the sentence rather than of the property,
	// and it failed the moment the reviewed wording landed instead.
	if !strings.Contains(writable.SystemPrompt, "nothing beyond it") {
		t.Errorf("a write task still needs a boundary the worktree cannot enforce:\n%s", writable.SystemPrompt)
	}
	if strings.Contains(writable.Metadata.Description, "Read-only") {
		t.Errorf("description = %q", writable.Metadata.Description)
	}

	// The obligation to go and look is shared: it is why plan tasks stopped
	// answering from memory, and it applies to a task that writes just as much.
	for name, manifest := range map[string]Manifest{"read-only": readOnly, "writable": writable} {
		if !strings.Contains(manifest.SystemPrompt, "USE THEM") {
			t.Errorf("%s: the prompt must still demand tool use:\n%s", name, manifest.SystemPrompt)
		}
		if !strings.Contains(manifest.SystemPrompt, "file:line") {
			t.Errorf("%s: the prompt must still demand quoted evidence:\n%s", name, manifest.SystemPrompt)
		}
	}
}

// EVERY named write tool flips it, not just write_file — the grant is the fact.
func TestEveryWriteToolFlipsTheTaskPrompt(t *testing.T) {
	for _, name := range PlanWriteToolNames() {
		manifest := planTaskManifest("explorer", "", "", []string{"read_file", name})
		if strings.Contains(manifest.SystemPrompt, "read-only tools") {
			t.Errorf("a task granted %q was told its tools are read-only", name)
		}
	}
	if grantsPlanWriteTool([]string{"read_file", "grep", "glob"}) {
		t.Error("a read-only grant must not read as writable")
	}
	if grantsPlanWriteTool(nil) {
		t.Error("an empty grant must not read as writable")
	}
}

// A WRITE TASK THAT CALLED NO TOOL MUST NOT REPORT SUCCESS.
//
// From a real run: the child emitted seventeen completion tokens reading
// "Creating notes.md now.", made no tool call, wrote nothing — and the plan
// recorded succeeded 1, status completed. A plan that reports work which did not
// happen is worse than one that fails, because nothing downstream can tell.
func TestAWriteTaskThatCalledNoToolIsNotASuccess(t *testing.T) {
	silent := Executor{
		RunChild: func(_ context.Context, _ string, _ []string, _ func(streamjson.Event)) (ChildRunResult, error) {
			// Emits nothing at all — exactly the observed child.
			return ChildRunResult{Started: true}, nil
		},
	}
	run := NewPlanRunner(PlanTaskContext{Executor: silent, Cwd: t.TempDir(), SpecialistName: "explorer"})

	result, err := run(context.Background(), PlanTaskRequest{
		Task:  Task{ID: "w", Prompt: "create notes.md"},
		Tools: []string{"read_file", "write_file"},
	})
	if err != nil {
		t.Fatalf("runner error: %v", err)
	}
	if result.Outcome != TaskFailed {
		t.Fatalf("outcome = %s, want failed: a write task that called no tool changed nothing", result.Outcome)
	}
	for _, want := range []string{"without calling a single one", "changed nothing"} {
		if !strings.Contains(result.Err, want) {
			t.Errorf("the failure must say what happened (missing %q): %s", want, result.Err)
		}
	}
}

// A write task that DID call a tool is unaffected, and a READ-ONLY task that
// called none still succeeds — there the inference is not airtight, and failing
// it would break every task that legitimately answers from its own prompt.
func TestTheNoToolGuardOnlyCatchesSilentWriteTasks(t *testing.T) {
	calling := Executor{
		RunChild: func(_ context.Context, _ string, _ []string, progress func(streamjson.Event)) (ChildRunResult, error) {
			if progress != nil {
				progress(streamjson.Event{Type: streamjson.EventToolCall, Name: "write_file"})
			}
			return ChildRunResult{Started: true}, nil
		},
	}
	silent := Executor{
		RunChild: func(_ context.Context, _ string, _ []string, _ func(streamjson.Event)) (ChildRunResult, error) {
			return ChildRunResult{Started: true}, nil
		},
	}

	wrote := NewPlanRunner(PlanTaskContext{Executor: calling, Cwd: t.TempDir(), SpecialistName: "explorer"})
	res, _ := wrote(context.Background(), PlanTaskRequest{
		Task: Task{ID: "w", Prompt: "write"}, Tools: []string{"write_file"},
	})
	if res.Outcome != TaskSucceeded {
		t.Errorf("a write task that DID call a tool must succeed, got %s: %s", res.Outcome, res.Err)
	}

	readOnly := NewPlanRunner(PlanTaskContext{Executor: silent, Cwd: t.TempDir(), SpecialistName: "explorer"})
	res, _ = readOnly(context.Background(), PlanTaskRequest{
		Task: Task{ID: "r", Prompt: "look"}, Tools: []string{"read_file", "grep"},
	})
	if res.Outcome != TaskSucceeded {
		t.Errorf("a read-only task is not caught by this guard, got %s: %s", res.Outcome, res.Err)
	}
}

// THE CHILD'S AUTONOMY MUST MATCH ITS GRANT, asserted on the argv the RUNNER
// actually launches — not on a value the test computed for itself.
//
// specialistAutonomy maps every non-unsafe parent to "low" (read-only), so a
// write-capable plan task was handed write_file in its manifest and launched at
// an autonomy that never advertises it. From a real run: zero tool calls, no
// file, and the child leaking tool-call fragments into its prose because it
// wanted a tool it could not see.
//
// The first version of this test called BuildArgs with MemberAutonomy computed
// in the test body. It passed with the production line reverted — it was
// asserting the helper, not that the runner consults it.
func TestAWriteTaskIsLaunchedAtARungThatAdvertisesWriting(t *testing.T) {
	autonomyFor := func(granted []string) string {
		t.Helper()
		var captured []string
		executor := Executor{
			RunChild: func(_ context.Context, _ string, args []string, _ func(streamjson.Event)) (ChildRunResult, error) {
				captured = args
				return ChildRunResult{Started: true}, nil
			},
		}
		run := NewPlanRunner(PlanTaskContext{
			Executor:       executor,
			Cwd:            t.TempDir(),
			SpecialistName: "explorer",
			PermissionMode: "ask",
		})
		if _, err := run(context.Background(), PlanTaskRequest{
			Task:  Task{ID: "t", Prompt: "do the thing"},
			Tools: granted,
		}); err != nil {
			t.Fatalf("runner: %v", err)
		}
		for i, arg := range captured {
			if arg == "--auto" && i+1 < len(captured) {
				return captured[i+1]
			}
		}
		t.Fatalf("--auto missing from child argv %v", captured)
		return ""
	}

	if got := autonomyFor([]string{"read_file", "write_file"}); got != "member" {
		t.Errorf("a write-granted task launches at --auto %q; it must be a rung that advertises writing", got)
	}
	// A read-only task is unchanged, so an ordinary plan behaves exactly as before.
	if got := autonomyFor([]string{"read_file", "grep"}); got != "low" {
		t.Errorf("a read-only task launches at --auto %q, want low", got)
	}
}

// A FAILED TASK'S TOKENS WERE BILLED, so the plan must count them.
//
// The error branch computed the usage summary and then returned an ExecResult
// without it, so a child that spent tokens and crashed reported zero: the plan
// budget was never decremented for it and the total under-counted every failure.
func TestAFailedTaskStillReportsWhatItSpent(t *testing.T) {
	executor := Executor{
		RunChild: func(_ context.Context, _ string, _ []string, progress func(streamjson.Event)) (ChildRunResult, error) {
			if progress != nil {
				progress(streamjson.Event{Type: streamjson.EventToolCall, Name: "read_file"})
			}
			// Usage arrives, THEN the child dies.
			prompt, completion, total := 900, 100, 1000
			return ChildRunResult{
				Started: true,
				Events: []streamjson.Event{{
					Type:             streamjson.EventUsage,
					PromptTokens:     &prompt,
					CompletionTokens: &completion,
					TotalTokens:      &total,
				}},
			}, fmt.Errorf("child exited unexpectedly")
		},
	}
	run := NewPlanRunner(PlanTaskContext{Executor: executor, Cwd: t.TempDir(), SpecialistName: "explorer"})
	result, _ := run(context.Background(), PlanTaskRequest{
		Task: Task{ID: "a", Prompt: "look"}, Tools: []string{"read_file"},
	})
	if result.Outcome != TaskFailed {
		t.Fatalf("outcome = %s, want failed", result.Outcome)
	}
	if result.Tokens == 0 {
		t.Error("a task that spent tokens and then failed reported zero; its spend was real")
	}
}

// USAGE IS PRICED AGAINST THE MODEL THAT DID THE WORK. resolvedChildModel is the
// single rule the argv builder and the accounting both read, so the command line
// and the bill cannot disagree about which model ran.
func TestUsageIsAttributedToTheModelTheChildActuallyRan(t *testing.T) {
	named := planTaskManifest("explorer", "claude-haiku-4.5", "", []string{"read_file"})
	if got := resolvedChildModel(named, "gpt-4.1"); got != "claude-haiku-4.5" {
		t.Errorf("a task naming a model must be priced against it, got %q", got)
	}
	inherit := planTaskManifest("explorer", "", "", []string{"read_file"})
	if got := resolvedChildModel(inherit, "gpt-4.1"); got != "gpt-4.1" {
		t.Errorf("an inheriting task is priced against the parent's model, got %q", got)
	}
	// The argv builder must agree, or the two drift.
	argv := appendModelArgs(nil, named, "gpt-4.1", "")
	if !containsArg(argv, "--model", resolvedChildModel(named, "gpt-4.1")) {
		t.Errorf("argv and accounting disagree about the model: %v", argv)
	}
}
