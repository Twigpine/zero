package tui

import (
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/specialist"
)

// Task prompts are model-authored and reach the terminal through the inline
// panel, the sidebar rows and the detail pane. The realistic path is indirect
// prompt injection: poisoned file or web content echoed into orchestrate args.
// Cutting at the first newline is not enough — every other control byte, ESC
// included, would survive into the rendered row.
func TestPlanTaskSummaryStripsControlSequences(t *testing.T) {
	for name, prompt := range map[string]string{
		"ANSI colour":  "run tests \x1b[31mred\x1b[0m",
		"OSC title":    "run tests \x1b]0;pwned\x07",
		"bare escape":  "run \x1btests",
		"carriage ret": "run tests\rOVERWRITTEN",
		"backspace":    "run tests\x08\x08\x08pwn",
		"bell":         "run tests\x07",
	} {
		t.Run(name, func(t *testing.T) {
			got := planTaskSummary(specialist.Task{ID: "t", Prompt: prompt})
			for _, r := range got {
				if r < 0x20 || r == 0x7f {
					t.Fatalf("summary %q still carries control byte %q", got, r)
				}
			}
			if strings.Contains(got, "\x1b") {
				t.Fatalf("summary %q still carries ESC", got)
			}
		})
	}
}

// The ordinary case must survive intact — sanitizing is not an excuse to mangle
// a normal prompt.
func TestPlanTaskSummaryKeepsOrdinaryText(t *testing.T) {
	got := planTaskSummary(specialist.Task{ID: "t", Prompt: "  run the unit tests for internal/tui  "})
	if got != "run the unit tests for internal/tui" {
		t.Errorf("summary = %q, want the trimmed prompt", got)
	}
}
