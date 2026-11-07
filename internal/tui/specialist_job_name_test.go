package tui

import "testing"

// A SPECIALIST ROW SHOWS THE ASSIGNED JOB, not the generic specialist type.
//
// Four workers all read "worker" in the panel because the row rendered
// info.name (the type). The job is in the description; this condenses it to
// 1-2 words, stripping the worker label and the "plan task " prefix.
func TestSpecialistJobNameIsTheJobNotTheType(t *testing.T) {
	for _, tc := range []struct {
		name, desc, want string
	}{
		// The screenshot: four "worker" rows become four distinct jobs.
		{"worker", "W1: HTML link extractor", "HTML link"},
		{"worker", "W2: HTTP checker", "HTTP checker"},
		{"worker", "W3: Concurrency pool", "Concurrency pool"},
		{"worker", "W4: CLI + report", "CLI report"},
		// Other label shapes.
		{"worker", "S3 — auth boundary trace", "auth boundary"},
		{"worker", "W12 - fuzz the parser", "fuzz parser"},
		// A plan task keeps its own id after the prefix is stripped.
		{"m4-checker", "plan task m4-checker", "m4-checker"},
		// A verb-first briefing condenses the usual way.
		{"explorer", "Review the current branch", "Review current"},
		// No description: fall back to the type rather than "agent".
		{"worker", "", "worker"},
		// A SENTENCE description is a prompt, not a label: fall back to the name.
		{"a-reltime", "You are auditing package a-reltime", "a-reltime"},
		{"finder", "I will trace the retry watchdog", "finder"},
		// Nothing at all: a safe placeholder, never empty.
		{"", "", "agent"},
	} {
		t.Run(tc.desc+"/"+tc.name, func(t *testing.T) {
			if got := specialistJobName(tc.name, tc.desc); got != tc.want {
				t.Fatalf("specialistJobName(%q, %q) = %q, want %q", tc.name, tc.desc, got, tc.want)
			}
		})
	}
}

// The label strip must not eat a real word that merely looks tag-like.
func TestTheLabelStripLeavesRealWordsAlone(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"W1: HTML extractor", "HTML extractor"},
		// "IPv4" is a word, not a label — no separator follows a bare tag here.
		{"IPv4 address parser", "IPv4 address parser"},
		// A long token is not a label.
		{"Concurrency pool design", "Concurrency pool design"},
		{"plan task lint", "lint"},
	} {
		if got := stripSpecialistLabel(tc.in); got != tc.want {
			t.Errorf("stripSpecialistLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// THE RENDERED ROW shows the job — the screenshot's four "worker" rows become
// four distinct names.
func TestTheAgentsPanelRendersDistinctJobNames(t *testing.T) {
	m := sidebarTestModel()
	jobs := []string{"W1: HTML link extractor", "W2: HTTP checker", "W3: Concurrency pool", "W4: CLI + report"}
	for i, desc := range jobs {
		id := "call_" + string(rune('1'+i))
		m.specialists.start("worker", desc, id, m.now())
	}
	rendered := stripSidebar(m.sidebarAgentLines(sidebarWidth(m.width)))

	// The generic type must not be what identifies a row.
	if n := countOccurrences(rendered, "worker"); n > 0 {
		t.Fatalf("the panel still shows the generic type %d time(s), not the job:\n%s", n, rendered)
	}
	for _, want := range []string{"HTML link", "HTTP checker", "Concurrency pool", "CLI report"} {
		if !containsLine(rendered, want) {
			t.Fatalf("the job %q is not shown:\n%s", want, rendered)
		}
	}
}

func countOccurrences(s, sub string) int {
	n := 0
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			n++
		}
	}
	return n
}
func containsLine(s, sub string) bool { return countOccurrences(s, sub) > 0 }
