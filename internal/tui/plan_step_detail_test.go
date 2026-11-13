package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/zeroruntime"
)

// lastCardText returns the text of the most recently appended transcript row —
// the plan-step detail card built by openPlanStepDetail.
func lastCardText(m model) string {
	if len(m.transcript) == 0 {
		return ""
	}
	return m.transcript[len(m.transcript)-1].text
}

// TestPlanStepDetailByStatus: a completed step reads as "what we did" (outcome,
// duration, the model's note, and the captured changes/commands); a pending
// step reads as "what we will do" (forward framing + the planned approach). With
// no provider configured the explanation falls back to the local summary, so the
// structured sections and status framing render synchronously.
func TestPlanStepDetailByStatus(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := model{now: func() time.Time { return t0.Add(5 * time.Minute) }}
	m.plan.steps = []planStep{
		{content: "ship the button", status: "completed", notes: "wired the click handler", startedAt: t0, completedAt: t0.Add(80 * time.Second)},
		{content: "write the docs", status: "pending", notes: "cover the new flag"},
	}
	m.stepWork = map[string][]planStepWork{
		"ship the button": {
			{tool: "edit_file", summary: "edit button.go", detail: "+added line\n-removed line"},
			{tool: "bash", summary: "go build", detail: "exit 0"},
		},
	}

	// Completed step -> "what we did" (local fallback, no provider).
	m, cmd := m.openPlanStepDetail(0)
	if cmd != nil {
		t.Error("no provider: openPlanStepDetail should not dispatch a model call")
	}
	done := lastCardText(m)
	for _, want := range []string{"Done in 1m 20s.", "Built 1 file change and 1 command.", "What we did", "wired the click handler", "Files changed (1)", "Commands run (1)", "what was done in this step"} {
		if !strings.Contains(done, want) {
			t.Errorf("completed card missing %q\n---\n%s", want, done)
		}
	}

	// Pending step -> "what we will do".
	m, _ = m.openPlanStepDetail(1)
	pending := lastCardText(m)
	for _, want := range []string{"What we'll do", "what this step will do", "cover the new flag", "queued"} {
		if !strings.Contains(pending, want) {
			t.Errorf("pending card missing %q\n---\n%s", want, pending)
		}
	}
	if strings.Contains(pending, "Files changed") {
		t.Errorf("pending card should record no work yet:\n%s", pending)
	}
}

// TestPlanStepDetailAIExplanation: clicking a step with a provider shows the
// "Writing explanation…" loading line and dispatches a one-shot model call; the
// returned message updates the card in place with the model's text and caches
// it, so re-clicking the same step is instant with no second call.
func TestPlanStepDetailAIExplanation(t *testing.T) {
	provider := &fakeProvider{events: []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventText, Content: "We added the new button and wired up its click."},
		{Type: zeroruntime.StreamEventDone},
	}}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := model{now: func() time.Time { return t0 }, provider: provider}
	m.plan.steps = []planStep{{content: "ship the button", status: "completed", startedAt: t0, completedAt: t0.Add(time.Minute)}}

	// Click -> immediate loading card + a dispatched model request.
	m, cmd := m.openPlanStepDetail(0)
	if cmd == nil {
		t.Fatal("with a provider, clicking a step should dispatch a model call")
	}
	if loading := lastCardText(m); !strings.Contains(loading, "Writing explanation…") {
		t.Errorf("loading card should show the writing line:\n%s", loading)
	}

	// Run the cmd and feed its message back through Update.
	msg := cmd()
	exp, ok := msg.(planStepExplanationMsg)
	if !ok {
		t.Fatalf("expected planStepExplanationMsg, got %T", msg)
	}
	if exp.err != nil || exp.text == "" {
		t.Fatalf("expected a written explanation, got text=%q err=%v", exp.text, exp.err)
	}
	updated, _ := m.Update(exp)
	m = updated.(model)
	if got := lastCardText(m); !strings.Contains(got, "We added the new button") {
		t.Errorf("card should update in place with the model text:\n%s", got)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("expected exactly one model request, got %d", len(provider.requests))
	}

	// Re-click closes (toggle); click again -> served from cache, no new call.
	m, _ = m.openPlanStepDetail(0)
	m, cmd = m.openPlanStepDetail(0)
	if cmd != nil {
		t.Error("re-opening a cached step should not dispatch a second model call")
	}
	if got := lastCardText(m); !strings.Contains(got, "We added the new button") {
		t.Errorf("cached re-open should show the stored explanation:\n%s", got)
	}
	if len(provider.requests) != 1 {
		t.Errorf("cache should prevent a second model request, got %d", len(provider.requests))
	}
}

// TestPlanStepDetailSecondClickHides: the second click on a step removes the
// detail card — even when the model write-up is still in flight, a late response
// must NOT pop the card back open after the user closed it.
func TestPlanStepDetailSecondClickHides(t *testing.T) {
	provider := &fakeProvider{events: []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventText, Content: "late explanation"},
		{Type: zeroruntime.StreamEventDone},
	}}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := model{now: func() time.Time { return t0 }, provider: provider}
	m.plan.steps = []planStep{{content: "do the thing", status: "completed", startedAt: t0, completedAt: t0.Add(time.Minute)}}
	base := len(m.transcript)

	// First click opens the card (and dispatches the write-up request).
	m, cmd := m.openPlanStepDetail(0)
	if !m.planDetailOpen || len(m.transcript) != base+1 {
		t.Fatalf("first click should open one card: open=%v rows=%d", m.planDetailOpen, len(m.transcript)-base)
	}

	// Second click — before the response lands — must hide it.
	m, _ = m.openPlanStepDetail(0)
	if m.planDetailOpen {
		t.Error("second click should close the card")
	}
	if len(m.transcript) != base {
		t.Errorf("second click should remove the card: rows=%d", len(m.transcript)-base)
	}

	// The in-flight response now arrives — it must not reopen the closed card.
	updated, _ := m.Update(cmd())
	m = updated.(model)
	if m.planDetailOpen {
		t.Error("a late model response must not reopen a closed card")
	}
	if len(m.transcript) != base {
		t.Errorf("late response should not re-add the card: rows=%d", len(m.transcript)-base)
	}
}

// TestCaptureStepNarration: assistant prose is attributed to the in_progress
// step, blank/duplicate segments are dropped, and no step swallows another's.
func TestCaptureStepNarration(t *testing.T) {
	m := model{now: time.Now}
	m.plan.steps = []planStep{{content: "build it", status: "in_progress"}}
	m = m.captureStepNarration("First I'll scaffold the file.")
	m = m.captureStepNarration("First I'll scaffold the file.") // duplicate -> collapsed
	m = m.captureStepNarration("   ")                           // blank -> ignored
	m = m.captureStepNarration("Now I'll wire it up.")
	got := m.stepNarration["build it"]
	if len(got) != 2 {
		t.Fatalf("want 2 narration segments, got %d: %v", len(got), got)
	}
	if got[0] != "First I'll scaffold the file." || got[1] != "Now I'll wire it up." {
		t.Errorf("narration wrong: %v", got)
	}
}

// TestSidebarPlanSelectablesOffsets locks the click-to-step mapping against the
// renderContextSidebar layout: with no agents the AGENTS section is header +
// placeholder (2 lines), then a blank + PLAN header (2 lines), so step 0 sits on
// sidebar line 4.
func TestSidebarPlanSelectablesOffsets(t *testing.T) {
	m := model{now: time.Now}
	m.plan.steps = []planStep{
		{content: "a", status: "completed"},
		{content: "b", status: "in_progress"},
		{content: "c", status: "pending"},
	}
	hits := m.sidebarPlanSelectables(40)
	if len(hits) != 3 {
		t.Fatalf("want 3 hits, got %d", len(hits))
	}
	for i, want := range []int{4, 5, 6} {
		if hits[i].lineOffset != want || hits[i].stepIndex != i {
			t.Errorf("hit %d: want offset %d idx %d, got offset %d idx %d", i, want, i, hits[i].lineOffset, hits[i].stepIndex)
		}
	}
	// Empty plan -> no selectables.
	if got := (model{now: time.Now}).sidebarPlanSelectables(40); got != nil {
		t.Errorf("empty plan: want nil, got %v", got)
	}
}

// TestCaptureStepWork: file mutations AND commands are attributed to the
// in_progress step with their output captured; non-work tools and non-results
// are ignored.
func TestCaptureStepWork(t *testing.T) {
	m := model{now: time.Now}
	m.plan.steps = []planStep{{content: "build it", status: "in_progress"}}
	m = m.captureStepWork(transcriptRow{kind: rowToolResult, tool: "write_file", text: "wrote style.css", detail: "+ body {}"})
	m = m.captureStepWork(transcriptRow{kind: rowToolResult, tool: "bash", text: "ran go build", detail: "exit 0"})
	m = m.captureStepWork(transcriptRow{kind: rowToolResult, tool: "read_file", text: "read x"}) // ignored: not a work tool
	m = m.captureStepWork(transcriptRow{kind: rowToolCall, tool: "write_file", text: "call"})    // ignored: not a result
	work := m.stepWork["build it"]
	if len(work) != 2 {
		t.Fatalf("want 2 captured items (write_file + bash), got %d", len(work))
	}
	if work[0].tool != "write_file" || work[0].detail != "+ body {}" {
		t.Errorf("change item wrong: %+v", work[0])
	}
	if work[1].tool != "bash" || work[1].detail != "exit 0" {
		t.Errorf("command item wrong: %+v", work[1])
	}
	if !isPlanCommandTool("bash") || !isPlanCommandTool("exec_command") || isPlanCommandTool("write_file") {
		t.Errorf("isPlanCommandTool classification wrong")
	}
}

// TestPlanStepDetailToggle: re-clicking the open step hides the card (no
// stacking); clicking a different step switches; at most one card at a time.
func TestPlanStepDetailToggle(t *testing.T) {
	m := model{now: time.Now}
	m.plan.steps = []planStep{
		{content: "a", status: "completed"},
		{content: "b", status: "in_progress"},
	}
	base := len(m.transcript)

	m, _ = m.openPlanStepDetail(0)
	if !m.planDetailOpen || m.planDetailStep != 0 {
		t.Fatalf("first click should open step 0: open=%v step=%d", m.planDetailOpen, m.planDetailStep)
	}
	if len(m.transcript) != base+1 {
		t.Fatalf("first click should add one card: got %d, base %d", len(m.transcript), base)
	}

	m, _ = m.openPlanStepDetail(0)
	if m.planDetailOpen {
		t.Error("re-clicking the same step should close it")
	}
	if len(m.transcript) != base {
		t.Errorf("re-click should net zero growth: got %d, base %d", len(m.transcript), base)
	}

	m, _ = m.openPlanStepDetail(0)
	m, _ = m.openPlanStepDetail(1)
	if !m.planDetailOpen || m.planDetailStep != 1 {
		t.Errorf("clicking a different step should switch: open=%v step=%d", m.planDetailOpen, m.planDetailStep)
	}
	if len(m.transcript) != base+1 {
		t.Errorf("switching steps should keep exactly one card: got %d, base %d", len(m.transcript), base)
	}
}
