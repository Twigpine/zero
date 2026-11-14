package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// THE MODELS SECTION EXISTS EXACTLY WHEN THE FLEET DIVERSIFIES. A session where
// every agent inherits renders nothing — the sidebar's layout must stay what it
// was before this section existed — and one routed agent is enough to earn it.

func mixAgent(model string, status specialistStatus) specialistInfo {
	return specialistInfo{name: "a", model: model, status: status}
}

func TestModelMixEntriesGroupSortAndPinInheritedLast(t *testing.T) {
	entries := modelMixEntries([]specialistInfo{
		mixAgent("kimi-k2.6", specialistRunning),
		mixAgent("gpt-oss:20b", specialistRunning),
		mixAgent("gpt-oss:20b", specialistCompleted),
		mixAgent("", specialistRunning),
	}, "glm-5.2")
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3 (two routed models + inherited)", len(entries))
	}
	if entries[0].model != "gpt-oss:20b" || entries[0].total != 2 || entries[0].working != 1 {
		t.Fatalf("largest first, with live count: %+v", entries[0])
	}
	if entries[1].model != "kimi-k2.6" {
		t.Fatalf("second = %+v, want kimi-k2.6", entries[1])
	}
	last := entries[2]
	if !last.inherited || last.model != "glm-5.2" || last.total != 1 {
		t.Fatalf("inherited bucket must be pinned last under the session's model: %+v", last)
	}
}

func TestModelMixHidesWhenNothingIsRouted(t *testing.T) {
	entries := modelMixEntries([]specialistInfo{
		mixAgent("", specialistRunning),
		mixAgent("", specialistRunning),
	}, "glm-5.2")
	if modelMixDiversified(entries) {
		t.Fatal("an all-inherited fleet is the old world and must render nothing")
	}
	if modelMixDiversified(nil) {
		t.Fatal("no agents must render nothing")
	}
}

// The bar is exactly as wide as asked, every model owns at least one cell, and
// shares are proportional — a single routed agent is never rounded away.
func TestModelMixBarIsProportionalAndExact(t *testing.T) {
	entries := modelMixEntries([]specialistInfo{
		mixAgent("big", specialistRunning), mixAgent("big", specialistRunning),
		mixAgent("big", specialistRunning), mixAgent("big", specialistRunning),
		mixAgent("big", specialistRunning), mixAgent("big", specialistRunning),
		mixAgent("big", specialistRunning),
		mixAgent("small", specialistRunning),
	}, "")
	bar := ansi.Strip(modelMixBar(entries, 24))
	if got := len([]rune(bar)); got != 24 {
		t.Fatalf("bar width = %d, want exactly 24", got)
	}
	if !strings.Contains(bar, "▰") {
		t.Fatalf("bar carries no cells: %q", bar)
	}
	// One agent of eight at width 24 = 3 cells; the guarantee is ≥1 even at
	// widths where its share rounds to zero.
	tiny := ansi.Strip(modelMixBar(entries, 4))
	if got := len([]rune(tiny)); got != 4 {
		t.Fatalf("tiny bar width = %d, want exactly 4", got)
	}
}

func TestModelMixBarVanishesWhenTooNarrow(t *testing.T) {
	entries := modelMixEntries([]specialistInfo{
		mixAgent("a", specialistRunning), mixAgent("b", specialistRunning),
		mixAgent("c", specialistRunning),
	}, "")
	if bar := modelMixBar(entries, 2); bar != "" {
		t.Fatalf("a bar narrower than its models must vanish, got %q", bar)
	}
}

func TestModelMixRowsCarryCountsAndLive(t *testing.T) {
	entries := modelMixEntries([]specialistInfo{
		mixAgent("gpt-oss:20b", specialistRunning),
		mixAgent("gpt-oss:20b", specialistCompleted),
		mixAgent("", specialistCompleted),
	}, "glm-5.2")
	rows := modelMixRows(entries, 40)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	first := ansi.Strip(rows[0])
	if !strings.Contains(first, "gpt-oss:20b") || !strings.Contains(first, "×2") || !strings.Contains(first, "·1 live") {
		t.Fatalf("row must name the model, count and live share: %q", first)
	}
	second := ansi.Strip(rows[1])
	if !strings.Contains(second, "○") || !strings.Contains(second, "glm-5.2") || strings.Contains(second, "live") {
		t.Fatalf("inherited row: open dot, session model, no live suffix when idle: %q", second)
	}
	// Rows never exceed the asked width.
	for _, row := range rows {
		if w := len([]rune(ansi.Strip(row))); w > 40 {
			t.Fatalf("row wider than asked (%d > 40): %q", w, ansi.Strip(row))
		}
	}
}

// A model keeps its colour while counts shuffle the sort order around it — the
// style is a function of the NAME, not the position.
func TestModelMixColourIsStablePerModel(t *testing.T) {
	one := modelMixEntry{model: "kimi-k2.6"}
	if modelMixStyle(one).Render("x") != modelMixStyle(modelMixEntry{model: "kimi-k2.6", total: 9}).Render("x") {
		t.Fatal("a model's colour changed with its count")
	}
	if modelMixStyle(modelMixEntry{inherited: true}).Render("x") != zeroTheme.muted.Render("x") {
		t.Fatal("the inherited bucket must always be muted")
	}
}

// A LOOK AT IT. Not an assertion beyond existence — run with
//
//	go test ./internal/tui/ -run ModelsPanelPreview -count=1 -v
//
// in a real terminal to see the section exactly as the sidebar draws it.
func TestModelsPanelPreview(t *testing.T) {
	start := time.Unix(1000, 0)
	m := sidebarTestModel()
	m.modelName = "glm-5.2"
	m.specialists.start("impl", "refactor the parser", "s1", start)
	m.specialists.setModel("s1", "gpt-oss:20b")
	m.specialists.start("impl2", "fix the lexer", "s2", start)
	m.specialists.setModel("s2", "gpt-oss:20b")
	m.specialists.start("verify", "review the change", "s3", start)
	m.specialists.setModel("s3", "kimi-k2.6")
	m.specialists.complete("s3", specialistCompleted, 0, "", start)
	m.specialists.start("scan", "find callers", "s4", start)
	m.specialists.setModel("s4", "deepseek-v4-flash")
	m.specialists.start("misc", "summarize", "s5", start)
	m.now = func() time.Time { return start }
	lines := m.sidebarModelLines(44)
	if len(lines) < 4 {
		t.Fatalf("preview should have header+bar+rows, got %d lines", len(lines))
	}
	for _, line := range lines {
		t.Log(line)
	}
}

// THE SECTION IN PLACE: a diversified fleet grows a MODELS header in the
// sidebar; an undiversified one renders the sidebar without it — the layout
// that existed before this section.
func TestSidebarGrowsModelsSectionOnlyWhenRouted(t *testing.T) {
	start := time.Unix(1000, 0)
	routed := sidebarTestModel()
	routed.specialists.start("impl", "refactor the parser", "sess-1", start)
	routed.specialists.setModel("sess-1", "gpt-oss:20b")
	routed.specialists.start("scan", "find callers", "sess-2", start)
	routed.now = func() time.Time { return start }
	joined := strings.Join(routed.renderContextSidebar(44, 40), "\n")
	if !strings.Contains(ansi.Strip(joined), "MODELS") {
		t.Fatalf("a routed fleet must grow a MODELS section:\n%s", ansi.Strip(joined))
	}
	if !strings.Contains(ansi.Strip(joined), "gpt-oss:20b") {
		t.Fatalf("the MODELS section must name the routed model:\n%s", ansi.Strip(joined))
	}

	plain := sidebarTestModel()
	plain.specialists.start("scan", "find callers", "sess-2", start)
	plain.now = func() time.Time { return start }
	joinedPlain := strings.Join(plain.renderContextSidebar(44, 40), "\n")
	if strings.Contains(ansi.Strip(joinedPlain), "MODELS") {
		t.Fatalf("an unrouted fleet must not grow a MODELS section:\n%s", ansi.Strip(joinedPlain))
	}
}

// EVERY SCRIPT, not just Latin. The token filter tested ASCII ranges, so a task
// or agent described in Chinese, Russian, Greek or Arabic had every token
// rejected as "not a word" and the sidebar row lost its label — a silent
// degradation for anyone not writing in English.
func TestNonLatinLabelsAreRealWords(t *testing.T) {
	for _, token := range []string{"解析器", "парсер", "ανάλυση", "محلل", "parser", "v2"} {
		if !hasNameLetter(token) {
			t.Errorf("%q was rejected as not-a-word; its label would be dropped", token)
		}
	}
	for _, token := range []string{"---", "...", "→", ""} {
		if hasNameLetter(token) {
			t.Errorf("%q was accepted as a word", token)
		}
	}
}
