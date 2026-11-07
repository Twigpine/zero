package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/sessions"
)

// A RESTORED AGENT CARD SHOWED 153722867m16s.
//
// That is exactly math.MaxInt64 nanoseconds — Go's largest time.Duration —
// produced by subtracting the ZERO time. specialistInfoFromPayload rebuilt rows
// on resume without a timestamp, and the card computed m.now().Sub(startedAt)
// with no zero guard. Every agent in a resumed session rendered ~292 years,
// beside "0 tool calls", which reads as sub-agents that did nothing at all.
func TestARestoredAgentCardDoesNotRenderTheMaxDuration(t *testing.T) {
	const maxDuration = time.Duration(1<<63 - 1)

	// The shape that produced it: a row rebuilt with no start time.
	unset := specialistInfo{name: "worker", description: "W1", childSessionID: "c1", status: specialistCompleted}
	if got := time.Since(unset.startedAt); got != maxDuration {
		t.Fatalf("setup: a zero start no longer overflows (%v); this test guards the wrong thing", got)
	}

	m := sidebarTestModel()
	card := m.renderSpecialistCard(unset, 80)
	if strings.Contains(card, "153722867m") {
		t.Fatalf("the card still renders the clamped max duration:\n%s", card)
	}
	// 292 years in any spelling is wrong; nothing near it may appear.
	for _, absurd := range []string{"2562047h", "153722867"} {
		if strings.Contains(card, absurd) {
			t.Fatalf("the card renders an overflowed elapsed (%s):\n%s", absurd, card)
		}
	}
}

// AND THE TIMESTAMP MUST ACTUALLY BE RESTORED, not merely guarded away. The
// event carries CreatedAt; the restore path simply never read it.
func TestResumeRestoresAnAgentsStartAndEndTimes(t *testing.T) {
	start := time.Date(2026, 8, 4, 14, 1, 14, 0, time.UTC)
	info := specialistInfoFromPayload(map[string]any{
		"childSessionId": "c1", "specialist": "worker", "description": "W1: HTML link extractor",
	}, start)
	if info == nil {
		t.Fatal("the payload produced no card")
	}
	if !info.startedAt.Equal(start) {
		t.Fatalf("startedAt = %v, want the event's own time %v", info.startedAt, start)
	}
}

// A malformed or absent timestamp is UNKNOWN, and the guard then reports no
// elapsed rather than 292 years.
func TestAnUnparseableEventTimeIsZeroNotTheYearOne(t *testing.T) {
	for _, raw := range []string{"", "not-a-time", "2026-08-04 14:01:14"} {
		if got := sessionEventTime(sessions.Event{CreatedAt: raw}); !got.IsZero() {
			t.Fatalf("CreatedAt %q parsed to %v, want zero", raw, got)
		}
	}
	valid := sessionEventTime(sessions.Event{CreatedAt: "2026-08-04T14:01:14Z"})
	if valid.IsZero() {
		t.Fatal("a valid RFC3339 timestamp was discarded")
	}
}
