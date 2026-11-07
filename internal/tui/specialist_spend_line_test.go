package tui

import (
	"strings"
	"testing"
)

// EACH SUB-AGENT SHOWS ITS TOKEN CONSUMPTION, THEN ITS MODEL — always, without
// a click. Order is deliberate: tokens first, model after.
func TestSpecialistSpendLineIsTokensThenModel(t *testing.T) {
	line := specialistSpendLine(specialistInfo{tokenCount: 284000, model: "glm-5.2"})
	if !strings.Contains(line, "284K") || !strings.Contains(line, "glm-5.2") {
		t.Fatalf("spend line missing tokens or model: %q", line)
	}
	// Tokens must come BEFORE the model.
	if strings.Index(line, "284K") > strings.Index(line, "glm-5.2") {
		t.Fatalf("model appears before tokens: %q", line)
	}
}

// Each piece is optional: a row with only one of them shows just that; a fresh
// row shows nothing rather than a line of zeros.
func TestSpecialistSpendLineOmitsWhatItLacks(t *testing.T) {
	if got := specialistSpendLine(specialistInfo{model: "glm-5.2"}); got != "glm-5.2" {
		t.Fatalf("tokens-absent line = %q, want just the model", got)
	}
	if got := specialistSpendLine(specialistInfo{tokenCount: 1000}); got != "1K tok" {
		t.Fatalf("model-absent line = %q, want just the tokens", got)
	}
	if got := specialistSpendLine(specialistInfo{}); got != "" {
		t.Fatalf("a fresh row produced %q, want empty", got)
	}
}

// THE ROW RENDERS IT. Four workers each show a distinct model and token count in
// the always-visible sidebar, not only when expanded.
func TestTheRowShowsPerAgentTokensAndModel(t *testing.T) {
	m := sidebarTestModel()
	workers := []struct {
		id, job, model string
		tokens         int
	}{
		{"c1", "W1: HTML link extractor", "deepseek-v4-flash", 284000},
		{"c2", "W2: HTTP checker", "kimi-k2.6", 1355600},
	}
	for _, w := range workers {
		m.specialists.start("worker", w.job, w.id, m.now())
		m.specialists.setModel(w.id, w.model)
		m.specialists.setTokens(w.id, w.tokens)
	}
	rendered := stripSidebar(m.sidebarAgentLines(sidebarWidth(m.width)))
	// The token counts ALWAYS show — they come first, so the narrow column never
	// truncates them. A short model name shows in full; a long one shows its
	// start (the whole name is in the click-to-expand), since tokens win the
	// space by design.
	for _, want := range []string{"284K", "kimi-k2.6", "1.4M", "deepseek"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("the always-visible row does not show %q:\n%s", want, rendered)
		}
	}
}
