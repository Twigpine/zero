package tui

import (
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/agent"
)

func planPermissionRequest(args map[string]any) agent.PermissionRequest {
	return agent.PermissionRequest{
		ToolName:   "orchestrate",
		SideEffect: "shell",
		Reason:     "Runs a plan of read-only specialist sub-agents.",
		Args:       args,
		AvailableDecisions: []agent.PermissionDecisionAction{
			agent.PermissionDecisionAllow, agent.PermissionDecisionDeny,
		},
	}
}

func samplePlanArgs() map[string]any {
	return map[string]any{
		"name":        "sweep",
		"description": "look at the tier",
		"tasks": []any{
			map[string]any{"id": "by_name", "prompt": "find where it is defined"},
			map[string]any{"id": "by_use", "prompt": "find where it is read", "tools": []any{"grep", "glob"}},
			map[string]any{"id": "join", "prompt": "combine", "depends_on": []any{"by_use", "by_name"}},
		},
		"budget": map[string]any{"max_workers": float64(1), "max_tokens": float64(5000)},
	}
}

// THE DEFECT THIS EXISTS FOR: a twenty-task plan and a one-task plan produced
// the identical card, because nothing read Args.
func TestThePermissionCardShowsWhatThePlanWillDo(t *testing.T) {
	card, _ := renderFocusedPermissionPrompt(planPermissionRequest(samplePlanArgs()), 0, false, "", 100)
	for _, want := range []string{"sweep", "3 task(s)", "by_name", "by_use", "join", "5000 tokens"} {
		if !strings.Contains(card, want) {
			t.Errorf("the card must show %q:\n%s", want, card)
		}
	}
	// The DEPENDENCY and the TOOLS are half of what an approval decides.
	if !strings.Contains(card, "after by_name,by_use") {
		t.Errorf("the card must show what a task waits on:\n%s", card)
	}
	// Sorted, so the same grant always reads the same way on the card.
	if !strings.Contains(card, "glob grep") {
		t.Errorf("the card must show the tools a task asked for:\n%s", card)
	}
}

// TWO DIFFERENT PLANS MUST PRODUCE TWO DIFFERENT CARDS. Asserting a card
// contains a string proves the string is there; this proves the card is about
// the plan.
func TestTwoPlansDoNotRenderTheSameCard(t *testing.T) {
	small, _ := renderFocusedPermissionPrompt(planPermissionRequest(map[string]any{
		"name":   "small",
		"tasks":  []any{map[string]any{"id": "a", "prompt": "one thing"}},
		"budget": map[string]any{"max_workers": float64(1)},
	}), 0, false, "", 100)
	big, _ := renderFocusedPermissionPrompt(planPermissionRequest(samplePlanArgs()), 0, false, "", 100)
	if small == big {
		t.Fatal("a one-task plan and a three-task plan rendered identically")
	}
}

// EVERY OTHER PROMPT IS UNCHANGED. A tool with no registered renderer must
// produce exactly the card it produced before, or this change is a rewrite of
// every permission prompt in the product.
func TestAToolWithoutARendererIsUnchanged(t *testing.T) {
	for _, name := range []string{"bash", "write_file", "web_fetch", "edit_file", ""} {
		request := agent.PermissionRequest{
			ToolName:   name,
			SideEffect: "shell",
			Reason:     "does a thing",
			Scope:      "src/main.go",
			// Args PRESENT, so the test proves the renderer lookup is what gates
			// this rather than the absence of arguments.
			Args: map[string]any{"path": "src/main.go", "tasks": []any{map[string]any{"id": "x"}}},
		}
		if lines := permissionDetailLines(request, 100); len(lines) != 0 {
			t.Errorf("tool %q rendered detail it has no renderer for: %v", name, lines)
		}
	}
}

// A PLAN CANNOT PUSH THE OPTIONS OFF SCREEN. A card a plan can make taller than
// the terminal is a card whose choices an attacker can hide.
func TestALargePlanCannotPushTheOptionsOffTheCard(t *testing.T) {
	tasks := make([]any, 0, 60)
	for i := 0; i < 60; i++ {
		tasks = append(tasks, map[string]any{"id": strings.Repeat("t", i%8+1), "prompt": "x"})
	}
	args := map[string]any{"tasks": tasks, "budget": map[string]any{"max_workers": float64(1)}}
	lines := planPermissionDetail(args, 100)
	if len(lines) > permissionDetailMaxRows+4 {
		t.Fatalf("a 60-task plan drew %d detail lines; it must stay bounded", len(lines))
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "more not shown") {
		t.Fatalf("a truncated plan must say so:\n%s", joined)
	}
	// ...and the options are still on the card.
	card, offsets := renderFocusedPermissionPrompt(planPermissionRequest(args), 0, false, "", 100)
	if len(offsets) == 0 {
		t.Fatal("the options were lost")
	}
	// The deny option, by the label it actually carries.
	if !strings.Contains(card, "No, continue without running it") {
		t.Fatalf("the options were pushed off the card:\n%s", card)
	}
}

// UNTRUSTED INPUT. Every value came from the model, so nothing it can put in a
// plan may move the cursor, clear the screen, or forge a second card.
func TestModelSuppliedTextCannotRewriteTheCard(t *testing.T) {
	nasty := "evil\x1b[2J\x1b[H FORGED\r\nsecond line\ttab"
	args := map[string]any{
		"name":   nasty,
		"tasks":  []any{map[string]any{"id": nasty, "prompt": nasty}},
		"budget": map[string]any{"max_workers": float64(1)},
	}
	// THE SANITISER, checked on its own. The rendered lines legitimately carry
	// lipgloss's own escapes, so asserting "no \x1b in the output" would fail
	// against correct code and pass against nothing useful — the question is
	// what survives of the MODEL's text.
	clean := sanitizeCardText(nasty)
	if strings.ContainsAny(clean, "\x1b\r\n\t") {
		t.Fatalf("a control character survived sanitising: %q", clean)
	}
	if strings.Contains(clean, "second line") {
		t.Fatalf("a newline let the model add its own line: %q", clean)
	}
	if !strings.Contains(clean, "evil") {
		t.Fatalf("sanitising removed the readable text too: %q", clean)
	}

	// ...and nothing the model supplied reaches a rendered line as a control
	// character or as an extra row.
	lines := planPermissionDetail(args, 100)
	for _, line := range lines {
		if strings.ContainsAny(line, "\r\n") {
			t.Fatalf("a rendered line carries a control character: %q", line)
		}
	}
	if joined := strings.Join(lines, "\n"); !strings.Contains(joined, "evil") {
		t.Fatalf("the readable text was lost: %q", joined)
	}
}

// A MALFORMED TASK IS SHOWN AS MALFORMED, never dropped. A task the card
// silently omits is a task the user approved without seeing.
func TestAMalformedTaskIsShownNotDropped(t *testing.T) {
	args := map[string]any{
		"tasks":  []any{map[string]any{"id": "good", "prompt": "fine"}, "not-an-object", float64(7)},
		"budget": map[string]any{"max_workers": float64(1)},
	}
	joined := strings.Join(planPermissionDetail(args, 100), "\n")
	if !strings.Contains(joined, "3 task(s)") {
		t.Fatalf("the count must include unreadable entries: %s", joined)
	}
	if strings.Count(joined, "unreadable task entry") != 2 {
		t.Fatalf("both unreadable entries must be shown: %s", joined)
	}
}

// AN UNBOUNDED BUDGET IS NAMED. "no token limit" is the thing worth seeing on an
// approval; omitting it reads as though a limit exists.
func TestAnUnboundedBudgetSaysSo(t *testing.T) {
	args := map[string]any{
		"tasks":  []any{map[string]any{"id": "a", "prompt": "x"}},
		"budget": map[string]any{"max_workers": float64(1)},
	}
	joined := strings.Join(planPermissionDetail(args, 100), "\n")
	if !strings.Contains(joined, "no token limit") {
		t.Fatalf("an unbounded plan must say so: %s", joined)
	}
}

// A SAVED plan's tasks live on disk, not in the arguments. The card says which
// plan and where to read it rather than inventing a summary of tasks it cannot
// see.
func TestASavedPlanReferenceSaysWhereToReadIt(t *testing.T) {
	joined := strings.Join(planPermissionDetail(map[string]any{"saved": "sweep"}, 100), "\n")
	if !strings.Contains(joined, "saved plan: sweep") || !strings.Contains(joined, "/plans show sweep") {
		t.Fatalf("a saved reference must point at the plan: %s", joined)
	}
}

// Narrow terminals must not panic or produce garbage.
func TestPermissionDetailSurvivesNarrowWidths(t *testing.T) {
	for _, width := range []int{0, 1, 10, 24, 40, 200} {
		lines := planPermissionDetail(samplePlanArgs(), width)
		if len(lines) == 0 {
			t.Fatalf("width %d produced no detail", width)
		}
		for _, line := range lines {
			if strings.ContainsAny(line, "\n\r") {
				t.Fatalf("width %d produced a multi-line entry: %q", width, line)
			}
		}
	}
}

// THE CARD DESCRIBES THE BUDGET ADMISSION WILL ENFORCE. Admission accepts a
// numeric string ("Some models send numbers as strings"), but the card decoded
// the raw arguments itself and read "500000" as absent — so the user approved
// "no token limit" for a plan that would stop at 500,000.
func TestPlanCardShowsANumericStringBudgetAsEnforced(t *testing.T) {
	args := samplePlanArgs()
	args["budget"] = map[string]any{"max_workers": "2", "max_tokens": "500000", "max_wall_seconds": "600"}
	joined := strings.Join(planPermissionDetail(args, 120), "\n")
	if strings.Contains(joined, "no token limit") {
		t.Fatalf("the card claims no token limit for a plan bounded at 500000:\n%s", joined)
	}
	for _, want := range []string{"500000 tokens", "600s wall"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("card is missing %q:\n%s", want, joined)
		}
	}
}

// A budget admission will refuse is shown as refused, not as a plausible bound.
func TestPlanCardFlagsABudgetAdmissionWillRefuse(t *testing.T) {
	args := samplePlanArgs()
	args["budget"] = map[string]any{"max_workers": float64(1), "max_tokens": 1.5}
	joined := strings.Join(planPermissionDetail(args, 160), "\n")
	if !strings.Contains(joined, "invalid budget") {
		t.Fatalf("a fractional max_tokens was not flagged:\n%s", joined)
	}
}

// BACKGROUND IS A TOP-LEVEL DIRECTIVE. The card looked for it inside budget,
// where no call puts it, so it never told the user a plan would outlive the
// turn — for an inline plan or a saved one.
func TestPlanCardSaysWhenAPlanRunsInTheBackground(t *testing.T) {
	inline := samplePlanArgs()
	inline["background"] = true
	if joined := strings.Join(planPermissionDetail(inline, 160), "\n"); !strings.Contains(joined, "background") {
		t.Fatalf("an inline background plan's card does not say so:\n%s", joined)
	}
	saved := map[string]any{"saved": "sweep", "background": true}
	if joined := strings.Join(planPermissionDetail(saved, 160), "\n"); !strings.Contains(joined, "background") {
		t.Fatalf("a saved background plan's card does not say so:\n%s", joined)
	}
}
