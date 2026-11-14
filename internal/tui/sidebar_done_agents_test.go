package tui

import (
	"strings"
	"testing"
	"time"
)

// THE HEADER AND THE BODY MUST NOT CONTRADICT EACH OTHER ABOUT THE SAME RUN.
//
// sidebarSpecialists drops a finished agent once it is past its linger, so the
// AGENTS section has no lines to render. doneAgentCount counts exactly those
// agents, so the header says "▸2 done". A completed two-task plan therefore drew
//
//	AGENTS  ▸2 done
//	  no agents spawned
//
// which is the header reporting the session and the placeholder reporting the
// emptiness of a filtered list, in the same box. The count is the true one: two
// agents ran. The placeholder was the sentence that had to change, and it names
// the toggle that brings them back rather than merely going quiet.
func allFinishedAgentsModel(t *testing.T) model {
	t.Helper()
	m := sidebarTestModel()
	start := time.Unix(1000, 0)
	m.specialists.start("by_name", "find definitions", "sess-1", start)
	m.specialists.start("by_use", "find call sites", "sess-2", start)
	m.specialists.complete("sess-1", specialistCompleted, 0, "", start)
	m.specialists.complete("sess-2", specialistCompleted, 0, "", start)
	// Past the linger, which is what drops them from the list while leaving them
	// in the count.
	m.now = func() time.Time { return start.Add(10 * sidebarAgentLinger) }
	return m
}

func TestAFinishedPlanDoesNotClaimNoAgentsWereSpawned(t *testing.T) {
	m := allFinishedAgentsModel(t)

	if done := m.doneAgentCount(); done != 2 {
		t.Fatalf("setup: expected 2 finished agents, got %d", done)
	}
	width := sidebarWidth(m.width)
	if len(m.sidebarAgentLines(width)) != 0 {
		t.Fatal("setup: the agents are still listed, so the contradiction cannot arise")
	}

	rendered := strings.Join(m.renderContextSidebar(width, 24), "\n")
	header := ansiStripLine(m.sidebarAgentHeader(width))
	if !strings.Contains(header, "2 done") {
		t.Fatalf("setup: the header no longer reports the count: %q", header)
	}
	if strings.Contains(ansiStripLine(rendered), "no agents spawned") {
		t.Fatalf("the header says %q while the body says no agents were spawned:\n%s",
			strings.TrimSpace(header), ansiStripLine(rendered))
	}
	// And it must say something USEFUL — the count, and how to see them.
	plain := ansiStripLine(rendered)
	if !strings.Contains(plain, "2 finished") {
		t.Fatalf("the placeholder does not report what actually ran:\n%s", plain)
	}
	if !strings.Contains(plain, "click to show") {
		t.Fatalf("the placeholder does not name the toggle that reveals them:\n%s", plain)
	}
}

// The original sentence is still correct when it is true, and that is the whole
// distinction: an empty section means "none ran" only when none did.
func TestASessionThatSpawnedNothingStillSaysSo(t *testing.T) {
	m := sidebarTestModel()
	m.now = func() time.Time { return time.Unix(1000, 0) }

	if m.doneAgentCount() != 0 {
		t.Fatal("setup: this session has finished agents")
	}
	plain := ansiStripLine(strings.Join(m.renderContextSidebar(sidebarWidth(m.width), 24), "\n"))
	if !strings.Contains(plain, "no agents spawned") {
		t.Fatalf("a session that spawned nothing must say so:\n%s", plain)
	}
}

// Singular reads as English rather than as "1 finished agents".
func TestOneFinishedAgentIsReportedInTheSingular(t *testing.T) {
	if got := hiddenAgentsPlaceholder(1); !strings.HasPrefix(got, "1 finished") {
		t.Fatalf("one finished agent renders as %q", got)
	}
	if got := hiddenAgentsPlaceholder(0); got != "no agents spawned" {
		t.Fatalf("none renders as %q", got)
	}
}
