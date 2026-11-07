package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// WORK BEFORE A PLAN EXISTS MUST BE VISIBLE.
//
// Auto-assignment runs ahead of admission: a /models call, a probe of every
// candidate, and when routing is on a full child run on the strongest model.
// Tens of seconds with no plan, so no panel and no rows — a foreground run looks
// frozen at exactly the moment it is doing the most.
func TestPreflightStatusIsShownAndThenCleared(t *testing.T) {
	m := model{now: func() time.Time { return time.Unix(1000, 0) }}
	m.activeRunID = 3
	m.width, m.height = 140, 40
	m.altScreen = true
	m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowUser, text: "hello"})

	updated, _ := m.Update(planPreflightMsg{runID: 3, status: "listing this provider's models"})
	next, ok := updated.(model)
	if !ok {
		t.Fatalf("Update returned %T", updated)
	}
	if next.planPreflight == "" {
		t.Fatal("the preflight status never reached the model")
	}
	rendered := stripANSILines(next.renderContextSidebar(34, 28))
	if !strings.Contains(rendered, "listing this provider") {
		t.Errorf("the status is invisible to the user:\n%s", rendered)
	}

	// CLEARED BY AN EMPTY STATUS, or it outlives the work it describes.
	cleared, _ := next.Update(planPreflightMsg{runID: 3, status: ""})
	after, _ := cleared.(model)
	if after.planPreflight != "" {
		t.Errorf("the status was not cleared: %q", after.planPreflight)
	}
	if strings.Contains(stripANSILines(after.renderContextSidebar(34, 28)), "listing this provider") {
		t.Error("a cleared status is still rendered")
	}
}

// A STATUS FROM A FINISHED RUN MUST NOT LINGER OVER THE NEXT ONE — the same
// stale-run guard every other plan message carries.
func TestAPreflightStatusFromAStaleRunIsDropped(t *testing.T) {
	m := model{now: func() time.Time { return time.Unix(1000, 0) }}
	m.activeRunID = 7
	updated, _ := m.Update(planPreflightMsg{runID: 6, status: "from an older run"})
	next, _ := updated.(model)
	if next.planPreflight != "" {
		t.Errorf("a stale run's status was accepted: %q", next.planPreflight)
	}
}

// AND THE BRIDGE MUST ACTUALLY EMIT IT. The tests above hand Update a message
// and prove the model and sidebar handle one — they prove nothing about whether
// anything ever sends it. A mutation gutting the bridge's send passed both.
func TestTheBridgeEmitsPreflightStatusToTheSurface(t *testing.T) {
	var sent []tea.Msg
	bridge := NewPlanProgressBridge()
	bridge.Attach(func(msg tea.Msg) { sent = append(sent, msg) }, 4, nil, "")

	bridge.PlanPreflight("checking which models this provider will run…")
	bridge.PlanPreflight("")

	var statuses []string
	for _, msg := range sent {
		if typed, ok := msg.(planPreflightMsg); ok {
			if typed.runID != 4 {
				t.Errorf("preflight carried run %d, want 4", typed.runID)
			}
			statuses = append(statuses, typed.status)
		}
	}
	if len(statuses) != 2 {
		t.Fatalf("the bridge emitted %d preflight message(s), want 2: %#v", len(statuses), sent)
	}
	if statuses[0] == "" {
		t.Error("the status text was dropped on the way to the surface")
	}
	// THE CLEAR MUST TRAVEL TOO, or the status outlives the work it describes.
	if statuses[1] != "" {
		t.Errorf("the clearing message carried %q instead of an empty status", statuses[1])
	}
}
