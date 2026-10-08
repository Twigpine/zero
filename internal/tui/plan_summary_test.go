package tui

import (
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/tools"
)

func TestRenderPlanUpdateCardUsesTranscriptChecklist(t *testing.T) {
	detail := "Current Plan:\n1. [completed] Explore workspace\n2. [in_progress] Create pages\n3. [pending] Build CSS\n4. [pending] Add JS\n5. [failed] Verify"
	got, ok := renderPlanUpdateCard(detail, 80)
	if !ok {
		t.Fatal("well-formed plan output should render as a checklist")
	}
	plain := plainRender(t, got)
	for _, want := range []string{"Updated Plan", "✔ Explore workspace", "□ Create pages", "□ Build CSS", "✗ Verify"} {
		if !strings.Contains(plain, want) {
			t.Errorf("checklist missing %q: %q", want, plain)
		}
	}
	if strings.Contains(plain, "Current Plan:") || strings.Contains(plain, "[in_progress]") {
		t.Errorf("tool-wire format must not leak into the transcript: %q", plain)
	}
}

func TestRenderPlanUpdateCardLeavesMalformedOutputToGenericCard(t *testing.T) {
	if _, ok := renderPlanUpdateCard("unexpected error text", 80); ok {
		t.Error("malformed plan output should fall back to the generic tool card")
	}
	if _, ok := renderPlanUpdateCard("1. [pending] disguised diagnostic", 80); ok {
		t.Error("numbered status lines without a Current Plan header must fall back to the generic card")
	}
	if _, ok := renderPlanUpdateCard("Current Plan:\n1. [invalid] Step", 80); ok {
		t.Error("unknown plan statuses must fall back to the generic card")
	}
	if _, ok := renderPlanUpdateCard("Current Plan:\n1. [pending] Step\nError: persistence failed", 80); ok {
		t.Error("trailing diagnostics must fall back to the generic card")
	}
	if _, ok := renderPlanUpdateCard("Error: persistence failed\nCurrent Plan:\n1. [pending] Step", 80); ok {
		t.Error("diagnostics before Current Plan must fall back to the generic card")
	}
}

func TestUpdatePlanResultRendersChecklistInTranscript(t *testing.T) {
	m := limeTestModel()
	row := transcriptRow{
		kind:   rowToolResult,
		tool:   "update_plan",
		status: tools.StatusOK,
		detail: "Current Plan:\n1. [completed] Inspect the workspace\n2. [in_progress] Add the feature\n3. [pending] Run the tests",
	}
	got := plainRender(t, m.renderRow(row, 72, buildRowContext([]transcriptRow{row})))
	for _, want := range []string{"Updated Plan", "✔ Inspect the workspace", "□ Add the feature", "□ Run the tests"} {
		if !strings.Contains(got, want) {
			t.Errorf("transcript plan missing %q: %q", want, got)
		}
	}
}

func TestFooterDoesNotPinActivePlanAboveComposer(t *testing.T) {
	m := newModel(t.Context(), Options{ModelName: "gpt-4"})
	m.plan.steps = []planStep{{content: "Keep the composer clear", status: "in_progress"}}
	footer := plainRender(t, m.footerView(80))
	if strings.Contains(footer, "Keep the composer clear") || strings.Contains(footer, "Ctrl+P details") {
		t.Fatalf("active plan must live in the transcript, not the footer: %q", footer)
	}
}
