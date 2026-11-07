package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// A skipped or cancelled task is NOT an error. specialistCancelled fell into
// specialistStatusString's default arm, so every task skipped because its
// dependency failed rendered "error" — a plan with one real failure showed
// three.
func TestCancelledTasksDoNotRenderAsErrors(t *testing.T) {
	if got := specialistStatusString(specialistCancelled); got != "cancelled" {
		t.Fatalf("specialistCancelled renders as %q, want %q", got, "cancelled")
	}
	if got := specialistStatusString(specialistError); got != "error" {
		t.Fatalf("a real error must still say so, got %q", got)
	}
	// Fail closed: an unmapped status is an error, not something benign.
	if got := specialistStatusString(specialistStatus(99)); got != "error" {
		t.Fatalf("an unknown status must fail closed to %q, got %q", "error", got)
	}
}

// The card claimed an exit code it did not have. A plan task's failure carries
// no exit code, so the zero value rendered "error (exit code 0)" directly above
// a body reading "Subagent failed (exit 4)" — the card contradicting its own
// detail.
func TestTheCardOnlyClaimsAnExitCodeItHas(t *testing.T) {
	now := time.Now()
	withoutCode := specialistInfo{
		name: "trace", status: specialistError, errorMsg: "Subagent failed (exit 4)",
		startedAt: now, completedAt: now,
	}
	m := model{now: func() time.Time { return now }}
	rendered := m.renderSpecialistCard(withoutCode, 80)
	if strings.Contains(rendered, "exit code 0") {
		t.Fatalf("a failure with no exit code must not claim one:\n%s", rendered)
	}
	if !strings.Contains(rendered, "error") {
		t.Fatalf("it is still an error:\n%s", rendered)
	}

	withCode := withoutCode
	withCode.exitCode = 4
	if !strings.Contains(m.renderSpecialistCard(withCode, 80), "exit code 4") {
		t.Fatal("a real exit code must still be shown")
	}
}

// The rollup always read "0 tokens" because nothing populated tokenCount. A
// number that looks measured and is not is worse than no number.
func TestTheSummaryOmitsATokenTotalNobodyMeasured(t *testing.T) {
	now := time.Now()
	unmeasured := []specialistInfo{{name: "a", status: specialistCompleted, startedAt: now, completedAt: now}}
	if strings.Contains(renderSpecialistSummary(unmeasured, "*"), "0 tokens") {
		t.Fatal("the rollup must omit a token total nobody populated")
	}

	measured := []specialistInfo{{name: "a", status: specialistCompleted, tokenCount: 130135, startedAt: now, completedAt: now}}
	if !strings.Contains(renderSpecialistSummary(measured, "*"), "tokens") {
		t.Fatal("a measured total must still be reported")
	}
}

// A plan task's spend reaches its card, so the rollup adds up to what the plan
// reports rather than to zero.
func TestAPlanTasksSpendReachesItsCard(t *testing.T) {
	m := model{now: func() time.Time { return time.Unix(1000, 0) }}
	m.activeRunID = 1
	for _, msg := range []tea.Msg{
		planTaskStartMsg{runID: 1, taskID: "a", cardKey: "plantask_1"},
		planTaskDoneMsg{runID: 1, taskID: "a", cardKey: "plantask_1", dispatched: true,
			status: specialistCompleted, outcome: "succeeded", sessionID: "specialist_aaa", tokens: 150},
	} {
		updated, _ := m.Update(msg)
		m = updated.(model)
	}
	info, ok := m.specialists.getBySessionID("specialist_aaa")
	if !ok {
		t.Fatal("the task's card is missing")
	}
	if info.tokenCount != 150 {
		t.Fatalf("tokenCount = %d, want the task's real spend", info.tokenCount)
	}
}

// The sidebar said "no active plan" while the panel below it showed one
// mid-flight. Two surfaces contradicting each other about the same session.
func TestTheSidebarDoesNotDenyARunningPlan(t *testing.T) {
	m := model{now: func() time.Time { return time.Unix(1000, 0) }}
	m.orchestrate.admit(diamondAdmitted(), m.now())
	m.orchestrate.markStarted("a", "root", "", "", m.now())

	// Drives the REAL sidebar assembly, not the line helper: the helper
	// returning the right string proves nothing if the PLAN section never calls
	// it, which is the shape this feature keeps producing.
	rendered := stripANSILines(m.renderContextSidebar(40, 30))
	if strings.Contains(rendered, "no active plan") {
		t.Fatalf("the sidebar denies a plan that is running:\n%s", rendered)
	}
	// The section carries the progress count and the tasks themselves. The
	// plan's NAME lives on the panel and in the detail view — the column is 26
	// to 40 cells wide and a name would cost a task row.
	if !strings.Contains(rendered, "PLAN") || !strings.Contains(rendered, "0/4") {
		t.Errorf("the PLAN header does not show progress:\n%s", rendered)
	}
	for _, id := range []string{"a", "b", "c", "d"} {
		if !strings.Contains(rendered, id) {
			t.Errorf("the sidebar does not list task %q:\n%s", id, rendered)
		}
	}
}

// ...and with no plan at all it still says so.
func TestTheSidebarStillSaysWhenThereIsNoPlan(t *testing.T) {
	m := model{now: func() time.Time { return time.Unix(1000, 0) }}
	if !strings.Contains(stripANSILines(m.renderContextSidebar(40, 30)), "no active plan") {
		t.Fatal("with no plan the sidebar must still say so")
	}
}

func stripANSILines(lines []string) string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, ansi.Strip(line))
	}
	return strings.Join(out, "\n")
}
