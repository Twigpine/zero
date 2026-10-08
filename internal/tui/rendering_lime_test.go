package tui

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/sandbox"
	"github.com/Gitlawb/zero/internal/streamjson"
	"github.com/Gitlawb/zero/internal/tools"
)

// ansiPattern strips SGR styling and OSC sequences (hyperlinks) so
// assertions run against the visible text.
var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;]*m|\x1b\][^\a\x1b]*(?:\a|\x1b\\)`)

// plainRender strips styling so assertions run against text, not styled
// bytes. (Without a TTY lipgloss already renders plain; this keeps the tests
// honest either way.)
func plainRender(t *testing.T, rendered any) string {
	t.Helper()
	return ansiPattern.ReplaceAllString(renderContent(rendered), "")
}

func limeTestModel() model {
	return newModel(context.Background(), Options{ProviderName: "test-provider", ModelName: "test-model"})
}

// Already-styled markdown lines (highlighted code, headings, tables carry real
// ANSI) must pass through styleAssistantMarkdownLine VERBATIM, not get their
// escape bytes treated as runes and the whole line re-wrapped in base.Render —
// that double-wrapping inflated SGR density and let a downstream truncation slice
// mid-escape, leaking "[38;2;…" / "[1;4;…" garbage into the visible transcript.
func TestStyleAssistantMarkdownLinePassesAnsiVerbatim(t *testing.T) {
	old := lipgloss.Writer.Profile
	lipgloss.Writer.Profile = colorprofile.TrueColor
	defer func() { lipgloss.Writer.Profile = old }()

	in := "\x1b[31mtoken\x1b[0m" // an already-colored code token
	out := styleAssistantMarkdownLine(in, lipgloss.NewStyle().Bold(true))

	if out != in {
		t.Fatalf("already-styled input must pass through verbatim:\nin:  %q\nout: %q", in, out)
	}
}

// Task and TaskOutput render through specialist cards. update_plan hides only
// while in flight; its successful result is the durable transcript checklist.
func TestToolCallCardSuppressedInTranscript(t *testing.T) {
	for _, name := range []string{"Task", "update_plan", "TaskOutput", "tool_search"} {
		if !toolCallCardSuppressedInTranscript(name) {
			t.Errorf("%q call should be suppressed from the transcript", name)
		}
	}
	for _, name := range []string{"read_file", "write_file", "edit_file", "bash", "swarm_spawn"} {
		if toolCallCardSuppressedInTranscript(name) {
			t.Errorf("%q must still show its transcript card", name)
		}
	}
}

func TestSuppressedToolFailuresRemainVisible(t *testing.T) {
	for _, name := range []string{"Task", "TaskOutput", "tool_search"} {
		if !toolResultCardSuppressedInTranscript(name, tools.StatusOK) {
			t.Errorf("successful %q result should be suppressed", name)
		}
		if toolResultCardSuppressedInTranscript(name, tools.StatusError) {
			t.Errorf("failed %q result must remain visible", name)
		}
	}
	if toolResultCardSuppressedInTranscript("update_plan", tools.StatusOK) {
		t.Error("successful update_plan result must render as a transcript checklist")
	}
}

func TestUserRowRendersPromptGutter(t *testing.T) {
	m := limeTestModel()
	row := transcriptRow{kind: rowUser, text: "add a --version flag"}
	got := plainRender(t, m.renderRow(row, 96, buildRowContext(nil)))
	if !strings.Contains(got, "\n▌  add a --version flag") {
		t.Fatalf("user row = %q, want rail-prefixed text", got)
	}
}

func TestRunningToolCardUsesLiveRailOnlyForActiveRun(t *testing.T) {
	m := limeTestModel()
	m.pending = true
	m.activeRunID = 4
	row := transcriptRow{kind: rowToolCall, id: "call-1", runID: 4, tool: "read_file", detail: "main.go"}
	active := plainRender(t, m.renderRunningToolCard(row, 80, buildRowContext([]transcriptRow{row}), cardRenderOptions{}))
	if !strings.HasPrefix(active, "│ ") {
		t.Fatalf("active card should begin with a live rail, got %q", active)
	}
	if !strings.Contains(active, "Reading") {
		t.Fatalf("active card should retain its action label, got %q", active)
	}

	m.pending = false
	inactive := plainRender(t, m.renderRunningToolCard(row, 80, buildRowContext([]transcriptRow{row}), cardRenderOptions{}))
	if strings.HasPrefix(inactive, "│ ") {
		t.Fatalf("inactive card should not retain a live rail, got %q", inactive)
	}

	m.pending = true
	stale := row
	stale.runID = m.activeRunID + 1
	staleCard := plainRender(t, m.renderRunningToolCard(stale, 80, buildRowContext([]transcriptRow{stale}), cardRenderOptions{}))
	if strings.HasPrefix(staleCard, "│ ") {
		t.Fatalf("a stale run card should not receive a live rail, got %q", staleCard)
	}
}

func TestAskUserQuestionnaireNamesWaitingForAnswer(t *testing.T) {
	prompt := pendingAskUserPrompt{
		request: agent.AskUserRequest{
			Header: "Quick question",
			Questions: []agent.AskUserQuestion{{
				Question: "Which task should Zero handle first?",
				Options:  []string{"Work helper"},
			}},
		},
		states: newAskUserStates([]agent.AskUserQuestion{{
			Question: "Which task should Zero handle first?",
			Options:  []string{"Work helper"},
		}}),
	}
	got := plainRender(t, renderAskUserQuestionnaire(prompt, "", 80))
	for _, want := range []string{"Quick question", "waiting for your answer"} {
		if !strings.Contains(got, want) {
			t.Fatalf("questionnaire missing %q:\n%s", want, got)
		}
	}
}

func TestAskUserWaitingStateFitsNarrowCardWithoutTitle(t *testing.T) {
	const width = 12
	got := renderAskUserWaitingState("", width, func(style lipgloss.Style) lipgloss.Style { return style })
	if visible := lipgloss.Width(got); visible > width-4 {
		t.Fatalf("waiting state width = %d, want at most %d: %q", visible, width-4, plainRender(t, got))
	}
}

func TestTranscriptSeparatesUserPromptFromContinuation(t *testing.T) {
	m := limeTestModel()
	m.headerPrinted = true
	m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowUser, text: "hey"})
	m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowReasoning, text: "private thought"})

	body, _ := m.transcriptBody(96, "")
	got := plainRender(t, body)
	lines := strings.Split(got, "\n")
	if len(lines) < 4 || !strings.HasPrefix(lines[1], "▌  hey") || lines[2] != "" || !strings.HasPrefix(lines[3], "▸ Thought") {
		t.Fatalf("transcript body should keep a small gap before thought, got:\n%s", got)
	}
}

func TestCommandCardRowRendersAsTitledCard(t *testing.T) {
	m := limeTestModel()
	row := transcriptRow{
		kind: rowSystem,
		text: renderCommandCardTranscript(commandCard{
			Title:   "Tools",
			Summary: []string{"2 registered", "registered catalog"},
			Sections: []commandCardSection{
				{
					Title: "Registry",
					Fields: []commandField{
						{Key: "registered", Value: "2"},
					},
				},
				{
					Title: "Available",
					Lines: []string{
						commandBullet("bash"),
						commandBullet("read_file"),
					},
				},
			},
			Actions: []string{"/mcp manage servers", "/permissions manage access"},
		}),
	}

	got := plainRender(t, m.renderRow(row, 80, buildRowContext(nil)))
	lines := strings.Split(got, "\n")
	if len(lines) < 3 {
		t.Fatalf("command card rendered too few lines:\n%s", got)
	}
	if !strings.Contains(lines[0], "Tools") {
		t.Fatalf("command card title should render in the border, got:\n%s", got)
	}
	if strings.Contains(got, "│ Tools") {
		t.Fatalf("command card should not render the title as muted body text, got:\n%s", got)
	}
	for _, want := range []string{
		"2 registered | registered catalog",
		"Registry",
		"registered  2",
		"Available",
		"- bash",
		"- read_file",
		"actions: /mcp manage servers | /permissions manage access",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("command card missing %q in:\n%s", want, got)
		}
	}
}

func TestEffortCommandCardRendersAsTitledCard(t *testing.T) {
	m := limeTestModel()
	row := transcriptRow{
		kind: rowSystem,
		text: renderCommandCardTranscript(commandCard{
			Title:   "Effort",
			Summary: []string{"active effort: auto", "3 supported level(s)"},
			Sections: []commandCardSection{{
				Title: "State",
				Fields: []commandField{
					{Key: "active effort", Value: "auto"},
					{Key: "model", Value: "claude-sonnet-4.5"},
					{Key: "available", Value: "low, medium, high"},
				},
			}},
			Actions: []string{"use /effort <value> to switch", "/effort auto to clear"},
		}),
	}

	got := plainRender(t, m.renderRow(row, 80, buildRowContext(nil)))
	if !strings.Contains(got, "Effort") {
		t.Fatalf("effort card should render title in the border, got:\n%s", got)
	}
	for _, want := range []string{
		"State",
		"active effort",
		"auto",
		"claude-sonnet-4.5",
		"low, medium, high",
		"actions: use /effort <value> to switch | /effort auto to clear",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("effort card missing %q in:\n%s", want, got)
		}
	}
	// Status card for unsupported models should still surface the no-controls
	// hint rather than falling back to the grey commandOutput panel.
	row.text = renderCommandCardTranscript(commandCard{
		Title:   "Effort",
		Summary: []string{"active effort: auto", "no reasoning controls on this model"},
		Sections: []commandCardSection{{
			Title: "State",
			Fields: []commandField{
				{Key: "active effort", Value: "auto"},
				{Key: "model", Value: "glm-5.1"},
				{Key: "available", Value: "none for active model"},
			},
		}},
		Actions: []string{"use /effort <value> to switch", "/effort auto to clear"},
	})
	got = plainRender(t, m.renderRow(row, 80, buildRowContext(nil)))
	if !strings.Contains(got, "none for active model") {
		t.Fatalf("effort unsupported card should render no-controls row, got:\n%s", got)
	}
	if strings.Contains(got, "status: warning") {
		t.Fatalf("effort card should not carry commandOutput status text, got:\n%s", got)
	}
}

func TestCommandCardRowTrimsIndentedActionsLabel(t *testing.T) {
	m := limeTestModel()
	row := transcriptRow{
		kind: rowSystem,
		text: commandCardTranscriptPrefix + strings.Join([]string{
			"Tools",
			"2 registered | registered catalog",
			"  actions: /mcp manage servers | /permissions manage access",
		}, "\n"),
	}

	got := plainRender(t, m.renderRow(row, 80, buildRowContext(nil)))
	if strings.Contains(got, "actions:  actions:") {
		t.Fatalf("command card duplicated indented actions label:\n%s", got)
	}
	if !strings.Contains(got, "actions: /mcp manage servers | /permissions manage access") {
		t.Fatalf("command card missing actions line:\n%s", got)
	}
}

func TestInterimBlockShowsStreamingTextWithCursor(t *testing.T) {
	m := limeTestModel()
	m.pending = true
	m.streamingText = []byte("I'll add a --version flag")
	got := plainRender(t, m.interimBlock(96))
	if !strings.Contains(got, "I'll add a --version flag") || !strings.Contains(got, "▌") {
		t.Fatalf("interim block = %q, want streamed text with trailing cursor", got)
	}

	// Before the first delta the block falls back to the liveness spinner.
	m.streamingText = nil
	if got := plainRender(t, m.interimBlock(96)); !strings.Contains(got, "Working") {
		t.Fatalf("empty interim block = %q, want the liveness label", got)
	}
}

func TestInterimBlockRendersStreamingMarkdownTable(t *testing.T) {
	m := limeTestModel()
	m.pending = true
	m.streamingText = []byte(strings.Join([]string{
		"Here's the comparison:",
		"",
		"| Category | System A | System B |",
		"|---|---|---|",
		"| **Label** | Alpha | Beta |",
	}, "\n"))

	rendered := m.interimBlock(72)
	got := plainRender(t, rendered)
	for _, unwanted := range []string{"|---", "**Label**"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("streaming markdown table leaked source syntax %q in:\n%s", unwanted, got)
		}
	}
	for _, want := range []string{"Category", "Label", " │ ", "─┼─", "▌"} {
		if !strings.Contains(got, want) {
			t.Fatalf("streaming markdown table missing %q in:\n%s", want, got)
		}
	}
	for index, line := range strings.Split(rendered, "\n") {
		if gotWidth := lipgloss.Width(line); gotWidth > 72 {
			t.Fatalf("line %d width = %d, want <= 72: %q", index, gotWidth, line)
		}
	}
}

func TestMarkdownInlineRendersBoldControlCodes(t *testing.T) {
	got := renderMarkdownInline("plain **h** __bold text__ `code` *em*")
	for _, want := range []string{markdownBoldStart + "h" + markdownBoldEnd, markdownBoldStart + "bold text" + markdownBoldEnd} {
		if !strings.Contains(got, want) {
			t.Fatalf("inline markdown render missing bold segment %q in %q", want, got)
		}
	}
	plain := ansiPattern.ReplaceAllString(got, "")
	if plain != "plain h bold text code em" {
		t.Fatalf("inline markdown plain text = %q, want markers stripped", plain)
	}
}

func TestMarkdownInlinePreservesLiteralStarsAndCodeBoundaries(t *testing.T) {
	got := renderMarkdownInline("literal * stays and **bold `not bold` bold**")
	plain := ansiPattern.ReplaceAllString(got, "")
	if plain != "literal * stays and bold not bold bold" {
		t.Fatalf("inline markdown plain text = %q, want literal star and stripped markers", plain)
	}
	if strings.Contains(got, markdownBoldStart+"not bold"+markdownBoldEnd) {
		t.Fatalf("inline code span inherited bold styling in %q", got)
	}
	for _, want := range []string{
		markdownBoldStart + "bold " + markdownBoldEnd,
		"not bold",
		markdownBoldStart + " bold" + markdownBoldEnd,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("inline markdown render missing %q in %q", want, got)
		}
	}
}

func TestMarkdownInlineKeepsCodingLiterals(t *testing.T) {
	tests := map[string]string{
		"`a.*b.*c`":                  "a.*b.*c",
		"`*.go` and `*.py`":          "*.go and *.py",
		"`x__y__z`":                  "x__y__z",
		"the __init__ method":        "the __init__ method",
		"run as __main__":            "run as __main__",
		"compute 2 * 3 * 4":          "compute 2 * 3 * 4",
		"keep a*b*c literal":         "keep a*b*c literal",
		"glob *.go outside code too": "glob *.go outside code too",
	}
	for input, want := range tests {
		got := ansiPattern.ReplaceAllString(renderMarkdownInline(input), "")
		if got != want {
			t.Fatalf("inline markdown for %q = %q, want %q", input, got, want)
		}
	}
}

func TestMarkdownHorizontalRulesRenderAsDividers(t *testing.T) {
	lines := renderAssistantMarkdownText(strings.Join([]string{
		"Before",
		"---",
		"After",
		"***",
		"Done",
	}, "\n"), 40, 40, true)
	got := plainRender(t, strings.Join(lines, "\n"))
	if strings.Contains(got, "\n---\n") || strings.Contains(got, "\n***\n") {
		t.Fatalf("horizontal rules should not leak raw markdown markers:\n%s", got)
	}
	if strings.Count(got, strings.Repeat("─", 40)) != 2 {
		t.Fatalf("horizontal rules should render as divider lines, got:\n%s", got)
	}
}

func TestMarkdownHorizontalRulesDoNotEatListsOrDiffHeaders(t *testing.T) {
	lines := renderAssistantMarkdownText(strings.Join([]string{
		"- item",
		"--- a/file.go",
		"+++ b/file.go",
	}, "\n"), 60, 60, false)
	got := plainRender(t, strings.Join(lines, "\n"))
	for _, want := range []string{"- item", "--- a/file.go", "+++ b/file.go"} {
		if !strings.Contains(got, want) {
			t.Fatalf("markdown rule parser should leave %q alone, got:\n%s", want, got)
		}
	}
	if strings.Contains(got, strings.Repeat("─", 16)) {
		t.Fatalf("list/diff-looking text should not render as a divider:\n%s", got)
	}
}

func TestMarkdownTableHeaderRendersBold(t *testing.T) {
	lines := renderAssistantMarkdownText(strings.Join([]string{
		"| Feature | System A | System B |",
		"|---|---|---|",
		"| Mode | Alpha | Beta |",
	}, "\n"), 72, 72, true)
	got := strings.Join(lines, "\n")
	for _, want := range []string{
		markdownBoldStart + "Feature" + markdownBoldEnd,
		markdownBoldStart + "System A" + markdownBoldEnd,
		markdownBoldStart + "System B" + markdownBoldEnd,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("markdown table header missing bold segment %q in:\n%s", want, got)
		}
	}
}

func TestMarkdownTableRendersOuterBorder(t *testing.T) {
	lines := renderAssistantMarkdownText(strings.Join([]string{
		"| Feature | System A |",
		"|---|---|",
		"| Mode | Alpha |",
	}, "\n"), 80, 80, true)
	got := strings.Join(lines, "\n")
	for _, want := range []string{"╭", "┬", "╮", "├", "┼", "┤", "╰", "┴", "╯"} {
		if !strings.Contains(got, want) {
			t.Fatalf("markdown table border missing %q in:\n%s", want, got)
		}
	}
}

func TestMarkdownTableAddsBodyRulesForWrappedRows(t *testing.T) {
	lines := renderAssistantMarkdownText(strings.Join([]string{
		"| Feature | System A | System B |",
		"|---|---|---|",
		"| Long Description | This cell contains enough neutral words to wrap across multiple lines. | This other cell also wraps across multiple lines for layout testing. |",
		"| Short Row | Alpha | Beta |",
	}, "\n"), 72, 72, true)
	got := strings.Join(lines, "\n")
	ruleCount := countMarkdownRuleLines(got)
	if ruleCount < 2 {
		t.Fatalf("wrapped markdown table should add body rules, got %d in:\n%s", ruleCount, got)
	}
}

func TestMarkdownTableKeepsCompactRowsClean(t *testing.T) {
	lines := renderAssistantMarkdownText(strings.Join([]string{
		"| Feature | System A | System B |",
		"|---|---|---|",
		"| Count | one | two |",
		"| Mode | Alpha | Beta |",
	}, "\n"), 96, 96, true)
	got := strings.Join(lines, "\n")
	ruleCount := countMarkdownRuleLines(got)
	if ruleCount != 1 {
		t.Fatalf("compact markdown table should only have header rule, got %d in:\n%s", ruleCount, got)
	}
}

func TestMarkdownTableAddsBodyRulesForManyRows(t *testing.T) {
	lines := renderAssistantMarkdownText(strings.Join([]string{
		"| Feature | System A | System B |",
		"|---|---|---|",
		"| Count | one | two |",
		"| Mode | Alpha | Beta |",
		"| Scope | local | remote |",
		"| State | ready | done |",
	}, "\n"), 160, 160, true)
	got := strings.Join(lines, "\n")
	ruleCount := countMarkdownRuleLines(got)
	if ruleCount < 2 {
		t.Fatalf("multi-row markdown table should add body rules even when wide, got %d in:\n%s", ruleCount, got)
	}
}

func TestMarkdownTableConvertsHtmlBreaks(t *testing.T) {
	lines := renderAssistantMarkdownText(strings.Join([]string{
		"| Field | Value |",
		"|---|---|",
		"| Price | first<br>second<BR />third<br/>fourth |",
	}, "\n"), 96, 96, true)
	got := strings.Join(lines, "\n")
	for _, unwanted := range []string{"<br>", "<BR />", "<br/>"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("markdown table leaked html break %q in:\n%s", unwanted, got)
		}
	}
	for _, want := range []string{"first", "second", "third", "fourth"} {
		if !strings.Contains(got, want) {
			t.Fatalf("markdown table missing break segment %q in:\n%s", want, got)
		}
	}
}

func countMarkdownRuleLines(rendered string) int {
	count := 0
	for _, line := range strings.Split(rendered, "\n") {
		if strings.Contains(line, "─┼─") {
			count++
		}
	}
	return count
}

func TestMarkdownPlainTextStripsRenderControls(t *testing.T) {
	lines := renderAssistantMarkdownPlainText(strings.Join([]string{
		"| **Feature** | System A |",
		"|---|---|",
		"| **Mode** | Alpha |",
	}, "\n"), 72, 72)
	got := strings.Join(lines, "\n")
	for _, unwanted := range []string{markdownBoldStart, markdownBoldEnd, "**"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("plain markdown render leaked %q in:\n%s", unwanted, got)
		}
	}
	if ansiPattern.MatchString(got) {
		t.Fatalf("plain markdown render leaked ANSI escapes in:\n%s", got)
	}
	for _, want := range []string{"Feature", "Mode", "│", "─┼─"} {
		if !strings.Contains(got, want) {
			t.Fatalf("plain markdown render missing %q in:\n%s", want, got)
		}
	}
}

func TestSelectableAssistantRowKeepsMarkdownSemanticsPlain(t *testing.T) {
	m := limeTestModel()
	row := transcriptRow{
		kind: rowAssistant,
		text: strings.Join([]string{
			"| Feature | System A | System B |",
			"|---|---|---|",
			"| **Mode** | Alpha | Beta |",
		}, "\n"),
		final: true,
	}

	rendered, selectable := m.renderSelectableAssistantRow(0, row, 72, 0)
	got := plainRender(t, rendered)
	for _, want := range []string{"Feature", "System A", "System B", "Mode", "│", "─┼─"} {
		if !strings.Contains(got, want) {
			t.Fatalf("selectable assistant markdown row missing %q in:\n%s", want, got)
		}
	}
	for _, line := range selectable {
		for _, unwanted := range []string{markdownBoldStart, markdownBoldEnd, "**"} {
			if strings.Contains(line.text, unwanted) {
				t.Fatalf("selectable metadata leaked %q in %#v", unwanted, line)
			}
		}
		if ansiPattern.MatchString(line.text) {
			t.Fatalf("selectable metadata leaked ANSI escapes in %#v", line)
		}
	}
}

func TestStripMarkdownRenderControlsStripsAllANSI(t *testing.T) {
	// Bold markers that renderAssistantMarkdownText embeds for prose formatting.
	withBold := "\x1b[1mhello\x1b[22m world"
	if got := stripMarkdownRenderControls(withBold); got != "hello world" {
		t.Fatalf("stripMarkdownRenderControls(%q) = %q, want %q", withBold, got, "hello world")
	}
	// Color sequences from highlightCodeAuto for fenced code blocks.
	withColor := "\x1b[38;2;236;236;238mdef\x1b[0m hello():"
	if got := stripMarkdownRenderControls(withColor); got != "def hello():" {
		t.Fatalf("stripMarkdownRenderControls(%q) = %q, want %q", withColor, got, "def hello():")
	}
	// Sequences that combine both (bold + underline + color).
	withBoldColor := "\x1b[1;4;38;2;236;236;238mW\x1b[m"
	if got := stripMarkdownRenderControls(withBoldColor); got != "W" {
		t.Fatalf("stripMarkdownRenderControls(%q) = %q, want %q", withBoldColor, got, "W")
	}
}

func TestSelectedTranscriptTextStripsANSIFromHighlightedCode(t *testing.T) {
	m := limeTestModel()
	m.width, m.height = 120, 40
	m.altScreen = true
	m.headerPrinted = true
	code := `package main

import "fmt"

func main() {
	fmt.Println("hello")
}`
	row := transcriptRow{kind: rowAssistant, text: "First, here's the code:\n\n```go\n" + code + "\n```\n\nThat's it.", final: true}
	m.transcript = append(m.transcript, row)
	m.flushed = len(m.transcript)

	// The highlighted code block contains ANSI color sequences from chroma;
	// verify none survive in the selectable text used for clipboard copy.
	_, selectable := m.renderSelectableAssistantRow(0, row, 72, 0)
	for _, line := range selectable {
		if ansiPattern.MatchString(line.text) {
			t.Fatalf("selectable text leaked ANSI escapes: %q", line.text)
		}
	}
}

func TestFinalAnswerRendersPlainTextAndCompletionLine(t *testing.T) {
	m := limeTestModel()
	row := transcriptRow{
		kind:        rowAssistant,
		text:        "Done — the CLI now prints its version.",
		final:       true,
		turnTools:   2,
		turnElapsed: 8400 * time.Millisecond,
	}
	got := plainRender(t, m.renderRow(row, 96, buildRowContext(nil)))
	if strings.Contains(got, "│") || strings.Contains(got, "●") {
		t.Fatalf("final row = %q, must not carry assistant rail or done glyph", got)
	}
	if !strings.Contains(got, "Done — the CLI now prints its version.") {
		t.Fatalf("final row = %q, want final answer text", got)
	}
	if strings.Contains(got, "completed in") || strings.Contains(got, "worked for") {
		t.Fatalf("short final row = %q, must not carry a completion bookend", got)
	}
}

func TestReasoningRowIsCollapsedByDefaultAndExpands(t *testing.T) {
	m := limeTestModel()
	row := transcriptRow{kind: rowReasoning, text: "\n\nprivate **chain**\nsecond line"}

	collapsed := plainRender(t, m.renderRow(row, 80, buildRowContext(nil)))
	if !strings.Contains(collapsed, "▸ Thought") {
		t.Fatalf("collapsed reasoning row missing summary:\n%s", collapsed)
	}
	if strings.Contains(collapsed, "private chain") {
		t.Fatalf("collapsed reasoning row leaked content:\n%s", collapsed)
	}

	row.expanded = true
	expanded := plainRender(t, m.renderRow(row, 80, buildRowContext(nil)))
	if !strings.Contains(expanded, "▾ Thought") || !strings.Contains(expanded, "private chain") {
		t.Fatalf("expanded reasoning row missing content:\n%s", expanded)
	}
	if strings.Contains(expanded, "**") {
		t.Fatalf("expanded reasoning row leaked markdown markers:\n%s", expanded)
	}
	lines := strings.Split(expanded, "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[1]) == "" {
		t.Fatalf("expanded reasoning row should start content immediately after header:\n%s", expanded)
	}
}

func TestReasoningRowShowsElapsedWhenKnown(t *testing.T) {
	m := limeTestModel()
	row := transcriptRow{
		kind:        rowReasoning,
		text:        "private thought",
		turnElapsed: 2300 * time.Millisecond,
	}

	collapsed := plainRender(t, m.renderRow(row, 80, buildRowContext(nil)))
	if !strings.Contains(collapsed, "▸ Thought for 2.3s") {
		t.Fatalf("collapsed reasoning row missing elapsed:\n%s", collapsed)
	}

	row.expanded = true
	expanded := plainRender(t, m.renderRow(row, 80, buildRowContext(nil)))
	if !strings.Contains(expanded, "▾ Thought for 2.3s") {
		t.Fatalf("expanded reasoning row missing elapsed:\n%s", expanded)
	}
}

func TestSelectableExpandedReasoningRowsAreClamped(t *testing.T) {
	m := limeTestModel()
	const width = 28
	row := transcriptRow{
		kind:     rowReasoning,
		text:     strings.Repeat("unbroken", 10),
		expanded: true,
	}

	rendered, _ := m.renderSelectableReasoningRow(0, row, width, 0)
	for _, line := range strings.Split(rendered, "\n") {
		if got := lipgloss.Width(plainRender(t, line)); got > width {
			t.Fatalf("line width = %d, want <= %d:\n%s", got, width, rendered)
		}
	}
}

func TestFinalAnswerRendersMarkdownTableForTerminal(t *testing.T) {
	m := limeTestModel()
	row := transcriptRow{
		kind: rowAssistant,
		text: strings.Join([]string{
			"Here's the comparison:",
			"",
			"| Category | System A | System B |",
			"|---|---|---|",
			"| **Label** | Alpha | Beta |",
			"| **Status** | ready | ready |",
		}, "\n"),
		final: true,
	}

	rendered := m.renderRow(row, 72, buildRowContext(nil))
	got := plainRender(t, rendered)
	for _, unwanted := range []string{"|---", "**Label**", "| **Status**"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("markdown table leaked source syntax %q in:\n%s", unwanted, got)
		}
	}
	for _, want := range []string{"Category", "Label", "System A", "System B"} {
		if !strings.Contains(got, want) {
			t.Fatalf("markdown table render missing %q in:\n%s", want, got)
		}
	}
	if !strings.Contains(got, " │ ") || !strings.Contains(got, "─┼─") {
		t.Fatalf("markdown table should render terminal table separators, got:\n%s", got)
	}
	if strings.Contains(got, "worked for") {
		t.Fatalf("short markdown final row = %q, must not carry a terminator", got)
	}
	for index, line := range strings.Split(rendered, "\n") {
		if gotWidth := lipgloss.Width(line); gotWidth > 72 {
			t.Fatalf("line %d width = %d, want <= 72: %q", index, gotWidth, line)
		}
	}
}

func TestFinalAnswerRendersLongThreeColumnMarkdownTables(t *testing.T) {
	m := limeTestModel()
	row := transcriptRow{
		kind: rowAssistant,
		text: strings.Join([]string{
			"| Aspect | System A | System B |",
			"|---|---|---|",
			"| **Long Row** | This cell contains detailed neutral text that should wrap cleanly inside the column. | This cell contains another neutral description that should wrap cleanly too. |",
		}, "\n"),
		final: true,
	}

	rendered := m.renderRow(row, 92, buildRowContext(nil))
	got := plainRender(t, rendered)
	for _, want := range []string{
		"Aspect",
		"Long Row",
		"System A",
		"System B",
		"detailed neutral",
		"another neutral",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("long three-column table missing %q in:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "─┼─") || strings.Contains(got, "  System A:") {
		t.Fatalf("long three-column table should render as a table, got:\n%s", got)
	}
	for index, line := range strings.Split(rendered, "\n") {
		if gotWidth := lipgloss.Width(line); gotWidth > 92 {
			t.Fatalf("line %d width = %d, want <= 92: %q", index, gotWidth, line)
		}
	}
}

func TestFinalAnswerCleansTableEmphasisAndInlineBullets(t *testing.T) {
	m := limeTestModel()
	row := transcriptRow{
		kind: rowAssistant,
		text: strings.Join([]string{
			"| Aspect | System A | System B |",
			"|---|---|---|",
			"| **Core Detail** | This neutral phrase emphasizes clarity *by design*. | This neutral phrase stays broadly compatible. |",
			"| **Capabilities** | - First capability: Generally reliable. - Second capability: Supports larger examples. | - Third capability: Broad task coverage. - Fourth capability: Strong integrations. |",
		}, "\n"),
		final: true,
	}

	rendered := m.renderRow(row, 92, buildRowContext(nil))
	got := plainRender(t, rendered)
	for _, unwanted := range []string{"*by design*", "Generally reliable. - Second capability:"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("markdown table leaked %q in:\n%s", unwanted, got)
		}
	}
	for _, want := range []string{
		"neutral phrase emphasizes",
		"clarity by design.",
		"- First capability:",
		"- Second capability:",
		"- Third capability:",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("markdown table missing %q in:\n%s", want, got)
		}
	}
	for index, line := range strings.Split(rendered, "\n") {
		if gotWidth := lipgloss.Width(line); gotWidth > 92 {
			t.Fatalf("line %d width = %d, want <= 92: %q", index, gotWidth, line)
		}
	}
}

func TestFinalAnswerRendersCrowdedMarkdownTablesAsTables(t *testing.T) {
	m := limeTestModel()
	row := transcriptRow{
		kind: rowAssistant,
		text: strings.Join([]string{
			"| Feature | System A | System B | Notes |",
			"|---|---|---|---|",
			"| **Long Description** | This neutral cell is intentionally long enough to wrap across lines. | This other neutral cell is also long enough to wrap across lines. | Notes should remain attached to the correct row. |",
			"| **Short Description** | Alpha | Beta | Both columns stay aligned. |",
		}, "\n"),
		final: true,
	}

	rendered := m.renderRow(row, 92, buildRowContext(nil))
	got := plainRender(t, rendered)
	for _, want := range []string{
		"Feature",
		"System A",
		"System B",
		"Notes",
		"Long Description",
		"intentionally long",
		"other neutral",
		"correct row",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("crowded markdown table missing %q in:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "─┼─") || strings.Contains(got, "  Notes:") {
		t.Fatalf("crowded table should render as a table, got:\n%s", got)
	}
	for index, line := range strings.Split(rendered, "\n") {
		if gotWidth := lipgloss.Width(line); gotWidth > 92 {
			t.Fatalf("line %d width = %d, want <= 92: %q", index, gotWidth, line)
		}
	}
}

// Short turns get no terminator (the next user prompt is the separator); only a
// long turn earns a faint "worked for …" bookend.
func TestDoneLineOnlyOnLongTurns(t *testing.T) {
	short := plainRender(t, renderAssistantRow(transcriptRow{final: true, text: "hi", turnElapsed: 2 * time.Second}, 80))
	if strings.Contains(short, "worked for") {
		t.Fatalf("short turn should have no bookend, got: %q", short)
	}
	long := plainRender(t, renderAssistantRow(transcriptRow{final: true, text: "hi", turnElapsed: 90 * time.Second}, 80))
	if !strings.Contains(long, "worked for") {
		t.Fatalf("long turn should show a 'worked for' bookend, got: %q", long)
	}
}

func TestInterimAssistantRowRendersAsPlainProse(t *testing.T) {
	m := limeTestModel()
	row := transcriptRow{kind: rowAssistant, text: "No provider configured."}
	got := plainRender(t, m.renderRow(row, 96, buildRowContext(nil)))
	if strings.Contains(got, "│") || strings.Contains(got, "●") {
		t.Fatalf("non-final assistant row = %q, must not carry activity chrome", got)
	}
	if !strings.Contains(got, "No provider configured.") {
		t.Fatalf("non-final assistant row should carry the body text, got %q", got)
	}
}

// TestNarrationSelectionGeometry locks narration column math: interim prose and
// final answers both start at textStart 0, no line overflows the width, and copy
// metadata stays free of display-only chrome.
func TestNarrationSelectionGeometry(t *testing.T) {
	m := limeTestModel()
	narr := transcriptRow{kind: rowAssistant, text: "Now the stylesheet. " + strings.Repeat("word ", 60)}
	rendered, metas := m.renderSelectableAssistantRow(0, narr, 90, 0)

	if strings.Contains(plainRender(t, rendered), "●") {
		t.Fatalf("narration display should not carry the old bullet:\n%s", rendered)
	}
	if len(metas) < 2 {
		t.Fatalf("expected the long narration to wrap to multiple lines, got %d", len(metas))
	}
	for i, meta := range metas {
		if meta.textStart != 0 {
			t.Errorf("meta %d textStart = %d, want 0", i, meta.textStart)
		}
		if strings.Contains(meta.text, "●") {
			t.Errorf("meta %d copy text should be chrome-free, got %q", i, meta.text)
		}
	}
	for _, line := range strings.Split(rendered, "\n") {
		if w := lipgloss.Width(line); w > 90 {
			t.Errorf("narration line width %d exceeds 90: %q", w, line)
		}
	}

	final := transcriptRow{kind: rowAssistant, text: "All done.", final: true}
	frendered, fmetas := m.renderSelectableAssistantRow(0, final, 90, 0)
	if strings.Contains(plainRender(t, frendered), "●") {
		t.Errorf("final answer must not carry the narration bullet:\n%s", frendered)
	}
	if len(fmetas) > 0 && fmetas[0].textStart != 0 {
		t.Errorf("final answer meta textStart = %d, want 0", fmetas[0].textStart)
	}
}

func TestErrorRowRendersTintedNoteAndErrorDoneLine(t *testing.T) {
	m := limeTestModel()
	row := transcriptRow{kind: rowError, text: "provider exploded", final: true, turnTools: 1}
	got := plainRender(t, m.renderRow(row, 60, buildRowContext(nil)))
	if !strings.Contains(got, "╭") || !strings.Contains(got, "provider exploded") {
		t.Fatalf("error row = %q, want bordered note", got)
	}
	if strings.Contains(got, "● error") {
		t.Fatalf("error row = %q, must not carry a second done-line (the bordered note already signals failure)", got)
	}
}

func TestSystemNoteRendersPlainLine(t *testing.T) {
	m := limeTestModel()
	row := transcriptRow{kind: rowSystem, text: "Mode set to ask."}
	got := plainRender(t, m.renderRow(row, 60, buildRowContext(nil)))
	// System notices are plain marked lines now, not boxes.
	if strings.ContainsAny(got, "╭╮╰╯│") {
		t.Fatalf("system row = %q, want a plain line (no box border)", got)
	}
	if !strings.Contains(got, "Mode set to ask.") {
		t.Fatalf("system row = %q, want the notice text unchanged", got)
	}
}

func TestRunningToolCardShowsHeadAndSpinnerSlot(t *testing.T) {
	m := limeTestModel()
	m.pending = true
	row := transcriptRow{kind: rowToolCall, id: "call_1", tool: "grep", detail: "internal/cli"}
	got := plainRender(t, m.renderRow(row, 80, buildRowContext(nil)))
	if !strings.Contains(got, "Searching") || !strings.Contains(got, "internal/cli") {
		t.Fatalf("running card = %q, want action label and target in head", got)
	}
	if strings.HasPrefix(got, "│ ") || strings.Contains(got, "\n│ ") {
		t.Fatalf("running card = %q, must not carry the old left rail", got)
	}
	if strings.ContainsAny(got, "╭╮╰╯") {
		t.Fatalf("running card = %q, must not carry box-border corners", got)
	}
}

func TestResolvedToolCallCollapsesIntoResultCard(t *testing.T) {
	rows := []transcriptRow{
		{kind: rowToolCall, id: "call_1", tool: "read_file", detail: "README.md"},
		{kind: rowToolResult, id: "call_1", tool: "read_file", status: tools.StatusOK, detail: "File: README.md\n\n1: # Zero"},
	}
	rc := buildRowContext(rows)
	if !rc.skip(rows[0]) {
		t.Fatal("a tool call with a result must collapse into the result card")
	}
	if rc.skip(rows[1]) {
		t.Fatal("the result row itself must render")
	}
}

func TestDiffCardBodyRendersCountsNumbersAndCap(t *testing.T) {
	m := limeTestModel()
	diff := strings.Join([]string{
		"--- internal/cli/root.go",
		"+++ internal/cli/root.go",
		"@@ -0,0 +1,3 @@",
		"+package cli",
		"+",
		"+++var Version = \"dev\"",
	}, "\n")
	row := transcriptRow{kind: rowToolResult, id: "call_1", tool: "write_file", status: tools.StatusOK, detail: diff}
	styled := m.renderRow(row, 80, buildRowContext(nil))
	for _, want := range []string{zeroTheme.diffAdd.Render("+3"), zeroTheme.diffDel.Render("-0")} {
		if !strings.Contains(styled, want) {
			t.Fatalf("diff card count tag should color additions/deletions, missing styled %q in:\n%s", want, styled)
		}
	}
	got := plainRender(t, styled)
	for _, want := range []string{"Added", "internal/cli/root.go", "(+3 -0)", "package cli", "++var Version = \"dev\"", "   1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("diff card = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "write_file") || strings.Contains(got, "NEW FILE") {
		t.Fatalf("diff card = %q, must not expose raw tool name or duplicate new-file tag", got)
	}
	if strings.Contains(got, "@@") {
		t.Fatalf("diff card should hide raw hunk metadata, got %q", got)
	}

	// The 16-line cap keeps long diffs bounded.
	long := []string{"+++ b/big.go", "@@ -0,0 +1,40 @@"}
	for i := 0; i < 40; i++ {
		long = append(long, "+line")
	}
	row.detail = strings.Join(long, "\n")
	got = plainRender(t, m.renderRow(row, 80, buildRowContext(nil)))
	if !strings.Contains(got, "more lines") {
		t.Fatalf("long diff card should cap at %d lines with a trailer, got %q", cardBodyMaxLines, got)
	}
}

func TestEditedDiffCardHidesHunkHeader(t *testing.T) {
	m := limeTestModel()
	diff := strings.Join([]string{
		"--- a/time_test.py",
		"+++ b/time_test.py",
		"@@ -1,3 +1,3 @@",
		" from datetime import datetime",
		"",
		"-print(datetime.now().strftime(\"%Y-%m-%d %H:%M:%S\"))",
		"+print(f\"Current system time: {datetime.now().strftime('%Y-%m-%d %H:%M:%S')}\")",
	}, "\n")
	row := transcriptRow{kind: rowToolResult, id: "call_1", tool: "edit_file", status: tools.StatusOK, detail: diff}
	got := plainRender(t, m.renderRow(row, 96, buildRowContext(nil)))
	for _, want := range []string{"Edited", "time_test.py", "(+1 -1)", "from datetime import datetime", "   3 −", "   3 +"} {
		if !strings.Contains(got, want) {
			t.Fatalf("edited diff card = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "@@") {
		t.Fatalf("edited diff card should hide raw hunk metadata, got %q", got)
	}
}

func TestEditedDiffCardHandlesBareBlankHunkContext(t *testing.T) {
	m := limeTestModel()
	diff := strings.Join([]string{
		"--- a/time_test.py",
		"+++ b/time_test.py",
		"@@ -1,3 +1,3 @@",
		" from datetime import datetime",
		"",
		"-print(datetime.now())",
		"+print(f\"Current time: {datetime.now()}\")",
	}, "\n")
	row := transcriptRow{kind: rowToolResult, id: "call_1", tool: "edit_file", status: tools.StatusOK, detail: diff}
	got := plainRender(t, m.renderRow(row, 96, buildRowContext(nil)))
	for _, want := range []string{"Edited", "time_test.py", "(+1 -1)", "from datetime import datetime", "   3 −", "   3 +"} {
		if !strings.Contains(got, want) {
			t.Fatalf("edited diff card = %q, missing %q", got, want)
		}
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.TrimSpace(line) == "2" {
			return
		}
	}
	t.Fatalf("edited diff card = %q, missing blank context line 2", got)
}

func TestReadCardBodyShowsExploredSummary(t *testing.T) {
	m := limeTestModel()
	// Mirrors the real read_file output shape: "N→<text>".
	detail := "File: internal/agent/loop.go\n\n12→func Run() {\n13→}\n"
	row := transcriptRow{kind: rowToolResult, id: "call_1", tool: "read_file", status: tools.StatusOK, detail: detail}
	rc := buildRowContext([]transcriptRow{{kind: rowToolCall, id: "call_1", tool: "read_file", detail: "internal/agent/loop.go"}})
	got := plainRender(t, m.renderRow(row, 80, rc))
	for _, want := range []string{"Explored", "└ Read", "internal/agent/loop.go"} {
		if !strings.Contains(got, want) {
			t.Fatalf("read card = %q, missing %q", got, want)
		}
	}
	if !strings.Contains(got, "▸ details") {
		t.Fatalf("read card = %q, missing expand affordance", got)
	}
	if strings.Contains(got, "L12") || strings.Contains(got, "func Run()") || strings.Contains(got, "  12 ") {
		t.Fatalf("read card = %q, must not dump read_file body into the transcript", got)
	}
	if strings.Contains(got, "read_file") {
		t.Fatalf("read card = %q, must not expose raw tool name", got)
	}

	row.expanded = true
	expanded := plainRender(t, m.renderRow(row, 80, rc))
	for _, want := range []string{"Explored", "└ Read", "internal/agent/loop.go", "func Run()", "13→"} {
		if !strings.Contains(expanded, want) {
			t.Fatalf("expanded read card = %q, missing %q", expanded, want)
		}
	}
}

func TestBashCardBodyShowsCommandOutputAndExit(t *testing.T) {
	m := limeTestModel()
	detail := "stdout:\nok build\nstderr:\nwarning: slow\nexit_code: 1"
	row := transcriptRow{kind: rowToolResult, id: "call_1", tool: "bash", status: tools.StatusError, detail: detail}
	rc := buildRowContext([]transcriptRow{{kind: rowToolCall, id: "call_1", tool: "bash", detail: "go build ./..."}})
	got := plainRender(t, m.renderRow(row, 80, rc))
	for _, want := range []string{"Ran", "go build ./...", "ok build", "warning: slow", "exit 1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("bash card = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "❯") || strings.Contains(got, "stdout:") || strings.Contains(got, "exit_code:") {
		t.Fatalf("bash card = %q, must restyle section markers", got)
	}
	if strings.Contains(got, "├ ok build") || strings.Contains(got, "└ warning: slow") {
		t.Fatalf("multi-line command output should render as a simple output block, got:\n%s", got)
	}
	if !strings.Contains(got, "│ ok build") || !strings.Contains(got, "│ warning: slow") {
		t.Fatalf("multi-line command output should stay grouped under the command, got:\n%s", got)
	}
}

func TestBashCardBodyKeepsSingleLineOutputCompact(t *testing.T) {
	m := limeTestModel()
	detail := "stdout:\nJS syntax: OK\nexit_code: 0"
	row := transcriptRow{kind: rowToolResult, id: "call_1", tool: "bash", status: tools.StatusOK, detail: detail}
	rc := buildRowContext([]transcriptRow{{kind: rowToolCall, id: "call_1", tool: "bash", detail: `node -e "console.log('ok')"`}})
	got := plainRender(t, m.renderRow(row, 80, rc))
	if !strings.Contains(got, "└ JS syntax: OK") {
		t.Fatalf("single-line command output should keep compact child marker, got:\n%s", got)
	}
}

func TestToolCardHeadCollapsesMultilineCommand(t *testing.T) {
	m := limeTestModel()
	command := "node -e \"\nconst fs = require('fs')\nconsole.log('ok')\n\""
	detail := "stdout:\nJS syntax: OK\nexit_code: 0"
	row := transcriptRow{kind: rowToolResult, id: "call_1", tool: "bash", status: tools.StatusOK, detail: detail}
	rc := buildRowContext([]transcriptRow{{kind: rowToolCall, id: "call_1", tool: "bash", detail: command}})
	got := plainRender(t, m.renderRow(row, 120, rc))
	if strings.Contains(got, "\nconst fs") || strings.Contains(got, "\n\"") {
		t.Fatalf("multi-line command should not break the card header, got:\n%s", got)
	}
	if !strings.Contains(got, "Ran node -e") || !strings.Contains(got, "const fs = require") {
		t.Fatalf("multi-line command summary should stay visible on one line, got:\n%s", got)
	}
}

func TestToolCardHeadStylesShellCommandButNotCommandOutput(t *testing.T) {
	previous := zeroTheme
	defer func() { zeroTheme = previous }()
	_, zeroTheme = themeForMode("nord", true)

	head := toolCardHead("bash", "gofmt -w calculator.go && go run . divide 9 2", "", "", "", "", false, zeroTheme.green, false, 120, cardRenderOptions{})
	if !strings.Contains(head, "\x1b[") {
		t.Fatalf("shell command head should be styled, got %q", head)
	}
	if plain := ansiPattern.ReplaceAllString(head, ""); !strings.Contains(plain, "Ran gofmt -w calculator.go && go run . divide 9 2") {
		t.Fatalf("shell command head changed visible text to %q", plain)
	}
}

func TestExecCommandCardBodyShowsSessionAndExit(t *testing.T) {
	m := limeTestModel()
	runningDetail := "output:\nServing HTTP on 0.0.0.0 port 8000\nsession_id: 1000\nUse write_stdin with session_id 1000 to poll, send input, or interrupt it."
	running := transcriptRow{kind: rowToolResult, id: "call_1", tool: "exec_command", status: tools.StatusOK, detail: runningDetail}
	rc := buildRowContext([]transcriptRow{{kind: rowToolCall, id: "call_1", tool: "exec_command", detail: "python3 -m http.server 8000"}})
	got := plainRender(t, m.renderRow(running, 90, rc))
	for _, want := range []string{"Ran", "python3 -m http.server 8000", "Serving HTTP", "session 1000"} {
		if !strings.Contains(got, want) {
			t.Fatalf("exec_command card = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "❯") || strings.Contains(got, "Use write_stdin") || strings.Contains(got, "session_id:") {
		t.Fatalf("exec_command card = %q, must restyle session markers", got)
	}

	exited := transcriptRow{kind: rowToolResult, id: "call_2", tool: "write_stdin", status: tools.StatusOK, detail: "output:\ndone\nexit_code: 0"}
	got = plainRender(t, m.renderRow(exited, 90, buildRowContext(nil)))
	if !strings.Contains(got, "done") {
		t.Fatalf("write_stdin card = %q, missing output", got)
	}
	if strings.Contains(got, "exit 0") {
		t.Fatalf("write_stdin card = %q, must not show successful exit", got)
	}
	for _, want := range []string{"done"} {
		if !strings.Contains(got, want) {
			t.Fatalf("write_stdin card = %q, missing %q", got, want)
		}
	}

	interrupted := transcriptRow{kind: rowToolResult, id: "call_3", tool: "write_stdin", status: tools.StatusOK, detail: "output:\n127.0.0.1 GET / HTTP/1.1 200\ninterrupted: true\nexit_code: -1"}
	got = plainRender(t, m.renderRow(interrupted, 90, buildRowContext(nil)))
	if !strings.Contains(got, "interrupted") || strings.Contains(got, "exit -1") {
		t.Fatalf("interrupted write_stdin card = %q", got)
	}
}

func TestLocalControlCardsUseFriendlyCompactLabels(t *testing.T) {
	m := limeTestModel()

	open := transcriptRow{kind: rowToolResult, id: "call_1", tool: "browser_open", status: tools.StatusOK, detail: "✓ ZERO - terminal agent\nhttp://localhost:8080/"}
	openRC := buildRowContext([]transcriptRow{{kind: rowToolCall, id: "call_1", tool: "browser_open", detail: "http://localhost:8080"}})
	got := plainRender(t, m.renderRow(open, 100, openRC))
	for _, want := range []string{"Opened", "http://localhost:8080", "ZERO - terminal agent"} {
		if !strings.Contains(got, want) {
			t.Fatalf("browser_open card missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "browser_open") || strings.Contains(got, "http://localhost:8080/\n") {
		t.Fatalf("browser_open card should not expose raw tool names or repeat the URL body:\n%s", got)
	}

	snapshotDetail := strings.Repeat("button \"Item\"\n", 31)
	snapshot := transcriptRow{kind: rowToolResult, id: "call_2", tool: "browser_snapshot", status: tools.StatusOK, detail: snapshotDetail}
	got = plainRender(t, m.renderRow(snapshot, 100, buildRowContext(nil)))
	for _, want := range []string{"Captured", "snapshot"} {
		if !strings.Contains(got, want) {
			t.Fatalf("browser_snapshot card missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "browser_snapshot") || strings.Contains(got, "click to expand") || strings.Contains(got, "button \"Item\"") {
		t.Fatalf("browser_snapshot card should stay compact and friendly:\n%s", got)
	}

	artifact := transcriptRow{kind: rowToolResult, id: "call_3", tool: "capture_artifact", status: tools.StatusOK, detail: "Artifact captured: /tmp/zero-artifacts/page.png\n\nhelper wrote screenshot"}
	got = plainRender(t, m.renderRow(artifact, 100, buildRowContext(nil)))
	for _, want := range []string{"Captured", "page.png"} {
		if !strings.Contains(got, want) {
			t.Fatalf("capture_artifact card missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "capture_artifact") || strings.Contains(got, "Artifact captured:") || strings.Contains(got, "helper wrote screenshot") {
		t.Fatalf("capture_artifact card should summarize the artifact instead of dumping helper output:\n%s", got)
	}
}

func TestPermissionRowsUseFriendlyLocalControlLabels(t *testing.T) {
	m := limeTestModel()
	row := transcriptRow{kind: rowPermission, id: "call_1", permission: &agent.PermissionEvent{
		ToolCallID: "call_1",
		ToolName:   "browser_open",
		Action:     agent.PermissionActionPrompt,
	}}
	got := plainRender(t, m.renderRow(row, 100, buildRowContext(nil)))
	if !strings.Contains(got, "Open browser") {
		t.Fatalf("permission row should use local-control display name:\n%s", got)
	}
	if strings.Contains(got, "browser_open") {
		t.Fatalf("permission row should not expose raw local-control tool name:\n%s", got)
	}
}

func TestToolCallSummaryDescribesExecSessions(t *testing.T) {
	cases := []struct {
		event streamjson.Event
		want  string
	}{
		{
			event: streamjson.Event{Name: "exec_command", Args: map[string]any{"cmd": "python3 -m http.server 8000"}},
			want:  "python3 -m http.server 8000",
		},
		{
			event: streamjson.Event{Name: "exec_command", Args: map[string]any{"cmd": "node -e \"\nconsole.log('ok')\n\""}},
			want:  `node -e " console.log('ok') "`,
		},
		{
			event: streamjson.Event{Name: "write_stdin", Args: map[string]any{"session_id": 1000}},
			want:  "poll session 1000",
		},
		{
			event: streamjson.Event{Name: "write_stdin", Args: map[string]any{"session_id": float64(1001), "chars": "\x03"}},
			want:  "interrupt session 1001",
		},
		{
			event: streamjson.Event{Name: "write_stdin", Args: map[string]any{"session_id": "1002", "chars": "q"}},
			want:  "send input to session 1002",
		},
	}
	for _, tc := range cases {
		if got := toolCallSummary(tc.event); got != tc.want {
			t.Fatalf("toolCallSummary(%#v) = %q, want %q", tc.event, got, tc.want)
		}
	}
}

func TestGrepCardBodyShowsExploredSummary(t *testing.T) {
	m := limeTestModel()
	detail := "internal/cli/root.go:41: fs := flag.NewFlagSet\ninternal/cli/app.go:12: flag.Parse()"
	row := transcriptRow{kind: rowToolResult, id: "call_1", tool: "grep", status: tools.StatusOK, detail: detail}
	rc := buildRowContext([]transcriptRow{{kind: rowToolCall, id: "call_1", tool: "grep", detail: "internal/cli", arg: "flag"}})
	got := plainRender(t, m.renderRow(row, 90, rc))
	for _, want := range []string{"Explored", "└ Search", "flag"} {
		if !strings.Contains(got, want) {
			t.Fatalf("grep card = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "internal/cli/root.go:41") || strings.Contains(got, "2 matches") {
		t.Fatalf("grep card = %q, must not expose raw search matches", got)
	}
}

func TestToolBodyRendererRegistryCoversCoreCards(t *testing.T) {
	for _, name := range []string{"read_file", "bash", "grep", "unknown_tool"} {
		if toolBodyRendererFor(name) == nil {
			t.Fatalf("expected renderer for %s", name)
		}
	}
}

func TestBashCardBodyShowsShellIssueHint(t *testing.T) {
	m := limeTestModel()
	detail := strings.Join([]string{
		"stderr:",
		"The syntax of the command is incorrect.",
		"exit_code: 1",
		"[zero] shell issue: Windows cmd.exe rejected the command syntax.",
		"Suggestion: Use the cwd argument instead of cd.",
	}, "\n")
	row := transcriptRow{kind: rowToolResult, id: "call_1", tool: "bash", status: tools.StatusError, detail: detail}
	rc := buildRowContext([]transcriptRow{{kind: rowToolCall, id: "call_1", tool: "bash", detail: "cd /d/tmp/zero-pr-158 && ls -la"}})
	got := plainRender(t, m.renderRow(row, 96, rc))
	for _, want := range []string{"shell issue", "Windows cmd.exe", "Suggestion:", "cwd"} {
		if !strings.Contains(got, want) {
			t.Fatalf("bash shell issue card = %q, missing %q", got, want)
		}
	}
}

func TestToolCardMarksAutoApprovedCalls(t *testing.T) {
	m := limeTestModel()
	rows := []transcriptRow{
		{kind: rowToolCall, id: "call_1", tool: "edit_file", detail: "main.go"},
		{kind: rowPermission, id: "call_1", permission: &agent.PermissionEvent{
			ToolCallID: "call_1", ToolName: "edit_file", Action: agent.PermissionActionAllow, GrantMatched: true,
		}},
		{kind: rowToolResult, id: "call_1", tool: "edit_file", status: tools.StatusOK, detail: "ok"},
	}
	rc := buildRowContext(rows)
	got := plainRender(t, m.renderRow(rows[2], 80, rc))
	if strings.Contains(got, "[auto]") {
		t.Fatalf("auto-approved card = %q, must not carry the removed [auto] tag", got)
	}

	// A prompted-then-allowed call was a manual decision: no auto tag.
	manual := []transcriptRow{
		{kind: rowToolCall, id: "call_2", tool: "bash", detail: "rm -rf ./tmp"},
		{kind: rowPermission, id: "call_2", permission: &agent.PermissionEvent{
			ToolCallID: "call_2", ToolName: "bash", Action: agent.PermissionActionPrompt,
		}},
		{kind: rowPermission, id: "call_2", permission: &agent.PermissionEvent{
			ToolCallID: "call_2", ToolName: "bash", Action: agent.PermissionActionAllow,
		}},
		{kind: rowToolResult, id: "call_2", tool: "bash", status: tools.StatusOK, detail: "ok"},
	}
	rcManual := buildRowContext(manual)
	if got := plainRender(t, m.renderRow(manual[3], 80, rcManual)); strings.Contains(got, "[auto]") {
		t.Fatalf("manually-approved card = %q, must not carry [auto]", got)
	}
}

func TestComposerLineTracksRunState(t *testing.T) {
	m := limeTestModel()
	m.input.SetValue("add a flag")
	if got := plainRender(t, m.composerLine(96)); strings.Contains(got, "run ↵") {
		t.Fatalf("idle composer = %q, should not show run hint", got)
	}

	m.pending = true
	if got := plainRender(t, m.composerLine(96)); strings.Contains(got, "esc stop") || strings.Contains(got, "esc to interrupt") {
		t.Fatalf("pending composer = %q, should not show stop hint", got)
	}

	m.input.SetValue("")
	if got := plainRender(t, m.composerLine(96)); !strings.Contains(got, composerPlaceholder) || strings.Contains(got, "running") || strings.Contains(got, "stop") || strings.Contains(got, "interrupt") {
		t.Fatalf("pending empty composer = %q, should show normal placeholder without run status text", got)
	}
}

func TestComposerLineShowsRequiredCommandArgumentHint(t *testing.T) {
	m := limeTestModel()
	m.input.SetWidth(40)
	m.input.SetValue("/spec")
	m.input.CursorEnd()
	if got := plainRender(t, m.composerLine(96)); !strings.Contains(got, "/spec [task]") {
		t.Fatalf("composer line = %q, want /spec argument hint", got)
	}

	m.input.SetValue("/spec ")
	m.input.CursorEnd()
	if got := plainRender(t, m.composerLine(96)); !strings.Contains(got, "/spec [task]") || strings.Contains(got, "/spec  [task]") {
		t.Fatalf("composer line = %q, want /spec argument hint", got)
	}

	m.input.SetValue("/find ")
	m.input.CursorEnd()
	if got := plainRender(t, m.composerLine(96)); !strings.Contains(got, "/find") || !strings.Contains(got, "[query]") {
		t.Fatalf("composer line = %q, want /find query hint", got)
	}

	m.input.SetValue("/spec fix this")
	m.input.CursorEnd()
	if got := plainRender(t, m.composerLine(96)); strings.Contains(got, "[task]") {
		t.Fatalf("composer line = %q, should hide hint once an argument is present", got)
	}

	m.input.SetValue("/model ")
	m.input.CursorEnd()
	if got := plainRender(t, m.composerLine(96)); strings.Contains(got, "[list|id]") {
		t.Fatalf("composer line = %q, should not hint optional arguments", got)
	}
}

func TestComposerBoxFramesInputAndBottomModelLabel(t *testing.T) {
	m := limeTestModel()
	m.input.SetValue("add a flag")

	got := plainRender(t, m.composerBox(96))
	// The box bottom rule shows the model only; the permission mode moved to the
	// status line below the box.
	for _, want := range []string{"╭", "│", "❯ add a flag", "╰", "test-model"} {
		if !strings.Contains(got, want) {
			t.Fatalf("composer box = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "auto-approve") {
		t.Fatalf("composer box = %q, should not show the mode (moved to status line)", got)
	}
	if strings.Contains(got, "run ↵") {
		t.Fatalf("composer box = %q, should not show run hint", got)
	}
	assertRenderedLineWidths(t, got, 96)
}

func TestComposerBoxWrapsLongPrompt(t *testing.T) {
	m := limeTestModel()
	m.input.SetValue("Create a book library dashboard page with the Bootstrap 5.3 theme displaying a grid of book cards showing cover images, titles, authors, and reading progress bars.")
	m.input.CursorEnd()

	got := plainRender(t, m.composerBox(72))
	if !strings.Contains(got, "Bootstrap 5.3") || !strings.Contains(got, "reading progress bars") {
		t.Fatalf("composer box should show wrapped long prompt, got:\n%s", got)
	}
	if lineCount := len(strings.Split(got, "\n")); lineCount < 5 {
		t.Fatalf("composer box line count = %d, want wrapped multi-line box:\n%s", lineCount, got)
	}
	assertRenderedLineWidths(t, got, 72)
}

func TestComposerBoxCapsLongPromptHeightAroundCursor(t *testing.T) {
	m := limeTestModel()
	m.input.SetValue(strings.Repeat("alpha beta gamma delta ", 12) + "final words")
	m.input.CursorEnd()

	got := plainRender(t, m.composerBox(44))
	if lineCount := len(strings.Split(got, "\n")); lineCount != composerMaxVisibleLines+2 {
		t.Fatalf("composer box line count = %d, want %d:\n%s", lineCount, composerMaxVisibleLines+2, got)
	}
	if !strings.Contains(got, "final words") {
		t.Fatalf("composer box should keep cursor-adjacent tail visible, got:\n%s", got)
	}
	if !strings.Contains(got, "❯") {
		t.Fatalf("composer box should keep the prompt marker visible when capped, got:\n%s", got)
	}
	assertRenderedLineWidths(t, got, 44)
}

func TestMalformedAskUserToolResultIsHiddenFromChatSurface(t *testing.T) {
	m := limeTestModel()
	row := transcriptRow{
		kind:   rowToolResult,
		id:     "call_bad",
		tool:   "ask_user",
		status: tools.StatusError,
		text:   "tool result: ask_user error",
		detail: "Error: Invalid arguments for ask_user: question 1 question is required",
	}
	if got := plainRender(t, m.renderRow(row, 96, buildRowContext([]transcriptRow{row}))); strings.TrimSpace(got) != "" {
		t.Fatalf("malformed ask_user result should stay internal, rendered %q", got)
	}
}

func TestMalformedToolArgumentResultIsHiddenFromChatSurface(t *testing.T) {
	m := limeTestModel()
	row := transcriptRow{
		kind:   rowToolResult,
		id:     "call_bad",
		tool:   "read_file",
		status: tools.StatusError,
		text:   "tool result: read_file error",
		detail: "Error: Failed to parse arguments for read_file: invalid character '{' after top-level value",
	}
	if got := plainRender(t, m.renderRow(row, 96, buildRowContext([]transcriptRow{row}))); strings.TrimSpace(got) != "" {
		t.Fatalf("malformed tool argument result should stay internal, rendered %q", got)
	}
}

func TestStatusLineGroups(t *testing.T) {
	m := limeTestModel()
	got := plainRender(t, m.statusLine(110))
	// Status line shows the run-state chip (permission mode), NOT the provider,
	// surface, or model — those live in the title bar / composer rule.
	if !strings.Contains(got, "● auto-approve") {
		t.Fatalf("status line = %q, missing the permission-mode chip", got)
	}
	if strings.Contains(got, "interactive") || strings.Contains(got, "test-model") || strings.Contains(got, "test-provider") {
		t.Fatalf("status line = %q, should not include surface, model, or provider", got)
	}
	divider := plainRender(t, m.composerDividerLine(110))
	if !strings.Contains(divider, "test-model") {
		t.Fatalf("composer divider = %q, missing the model label", divider)
	}
	if strings.Contains(divider, "auto-approve") {
		t.Fatalf("composer divider = %q, should not show the mode (moved to status line)", divider)
	}
}

func TestStatusLineShowsEffortWhenSet(t *testing.T) {
	m := limeTestModel()
	m.reasoningEffort = modelregistry.ReasoningEffortHigh
	status := plainRender(t, m.statusLine(110))
	if !strings.Contains(status, "high") {
		t.Fatalf("status line = %q, missing effort segment when set", status)
	}
}

func TestStatusLineOmitsEffortWhenAuto(t *testing.T) {
	m := limeTestModel()
	// reasoningEffort == "" (auto) by default — the effort segment is omitted.
	status := plainRender(t, m.statusLine(110))
	for _, effort := range []string{"low", "medium", "high", "minimal", "xhigh", "none"} {
		if strings.Contains(status, effort) {
			t.Fatalf("status line = %q, should omit effort segment when auto", status)
		}
	}
}

func TestTitleBarShowsWorkspaceAndModel(t *testing.T) {
	m := limeTestModel()
	m.width = 120
	m.cwd = "/workspace/zero"
	m.gitBranch = "main"
	got := plainRender(t, m.titleBar(120))
	for _, want := range []string{" main", "/workspace/zero", "test-provider/test-model"} {
		if !strings.Contains(got, want) {
			t.Fatalf("title bar = %q, missing %q", got, want)
		}
	}
	for _, notWant := range []string{" 0 ", "zero /"} {
		if strings.Contains(got, notWant) {
			t.Fatalf("title bar = %q, should not include old brand cluster %q", got, notWant)
		}
	}
}

func TestTitleBarHighlightsBranchOverWorkspace(t *testing.T) {
	oldProfile := lipgloss.Writer.Profile
	lipgloss.Writer.Profile = colorprofile.TrueColor
	t.Cleanup(func() {
		lipgloss.Writer.Profile = oldProfile
	})

	m := limeTestModel()
	m.cwd = "/workspace/zero"
	m.gitBranch = "main"
	got := m.titleWorkspaceSegment()

	highlightedBranch := zeroTheme.muted.Render("") + " " + zeroTheme.muted.Render("main")
	recessedWorkspace := zeroTheme.faint.Render("/workspace/zero")
	for _, want := range []string{highlightedBranch, recessedWorkspace} {
		if !strings.Contains(got, want) {
			t.Fatalf("title workspace segment = %q, missing styled segment %q", got, want)
		}
	}
	if faintBranch := zeroTheme.faint.Render("") + " " + zeroTheme.faint.Render("main"); strings.Contains(got, faintBranch) {
		t.Fatalf("title workspace segment = %q, branch should use highlighted title colour", got)
	}
}

func TestGenericCustomProviderDisplayUsesEndpointName(t *testing.T) {
	m := newModel(context.Background(), Options{
		ProviderName: "custom-openai-compatible",
		ModelName:    "MiniMax-M3",
		ProviderProfile: config.ProviderProfile{
			Name:      "custom-openai-compatible",
			CatalogID: "custom-openai-compatible",
			BaseURL:   "https://api.minimax.io/v1",
			Model:     "MiniMax-M3",
		},
	})

	// The derived provider label lives in the title bar (no longer duplicated in
	// the status line, which now shows the run-state chip instead).
	title := plainRender(t, m.titleBar(120))
	if !strings.Contains(title, "minimax/MiniMax-M3") {
		t.Fatalf("title bar = %q, want derived custom provider label", title)
	}
	if strings.Contains(title, "custom-openai-compatible/MiniMax-M3") {
		t.Fatalf("title bar = %q, should not show generic custom catalog id", title)
	}
}

func TestFormatContextWindow(t *testing.T) {
	cases := map[int]string{200000: "200K", 1000000: "1M", 128000: "128K", 0: ""}
	for window, want := range cases {
		if got := formatContextWindow(window); got != want {
			t.Fatalf("formatContextWindow(%d) = %q, want %q", window, got, want)
		}
	}
}

// --- Regression tests for review-confirmed Stage 2 findings -----------------

func TestWrapPlainTextHandlesWideRunes(t *testing.T) {
	// CJK prose has no spaces: one giant double-width "word". This used to
	// panic (rune-count slicing) or emit lines ~2x the measure.
	lines := wrapPlainText(strings.Repeat("漢", 40), 20)
	if len(lines) == 0 {
		t.Fatal("expected wrapped output")
	}
	for _, line := range lines {
		if w := displayWidth(line); w > 20 {
			t.Fatalf("wrapped CJK line width %d exceeds measure 20: %q", w, line)
		}
	}
}

func displayWidth(line string) int {
	width := 0
	for _, glyph := range line {
		width += len([]rune{glyph})
		if glyph > 0x2E80 { // rough CJK double-width check for the test
			width++
		}
	}
	return width
}

func TestToolCardLinesAllSameWidth(t *testing.T) {
	m := limeTestModel()
	detail := "internal/cli/root.go:41: fs := flag.NewFlagSet"
	row := transcriptRow{kind: rowToolResult, id: "c", tool: "grep", status: tools.StatusOK, detail: detail}
	card := plainRender(t, m.renderRow(row, 60, buildRowContext(nil)))
	for _, line := range strings.Split(card, "\n") {
		if got := len([]rune(line)); got != 60 {
			t.Fatalf("card line width %d, want 60: %q", got, line)
		}
	}
}

func TestToolResultCardRendersInlineWithoutRail(t *testing.T) {
	m := limeTestModel()
	row := transcriptRow{
		kind:   rowToolResult,
		id:     "c",
		tool:   "read_file",
		status: tools.StatusOK,
		detail: "File: README.md\n\n1: # Zero\n2: line two",
	}
	card := plainRender(t, m.renderRow(row, 80, buildRowContext(nil)))
	lines := strings.Split(card, "\n")
	// No line carries the old left rail or box borders.
	for i, line := range lines {
		if strings.HasPrefix(line, "│ ") {
			t.Fatalf("line %d = %q, must not carry left-rule prefix", i, line)
		}
		if strings.ContainsAny(line, "╭╮╰╯") || strings.HasSuffix(line, "│") {
			t.Fatalf("line %d = %q, must not carry box borders", i, line)
		}
	}
	// The status glyph still sits on the head line.
	if !strings.Contains(lines[0], "•") {
		t.Fatalf("head line = %q, want the status glyph on the head line", lines[0])
	}
}

func TestCancelRunClearsStreamingText(t *testing.T) {
	m := limeTestModel()
	m.pending = true
	m.activeRunID = 3
	m.streamingText = []byte("partial answer from a doomed run")
	m.cancelRun()
	if len(m.streamingText) != 0 {
		t.Fatalf("cancelRun must clear streamingText, got %q", string(m.streamingText))
	}
}

func TestOrphanToolCardDoesNotAnimateOnLaterRuns(t *testing.T) {
	m := limeTestModel()
	// A call row from run 1 that never resolved; run 2 is now live.
	orphan := transcriptRow{kind: rowToolCall, id: "old", tool: "bash", runID: 1}
	m.pending = true
	m.activeRunID = 2
	got := plainRender(t, m.renderRow(orphan, 60, buildRowContext(nil)))
	if !strings.Contains(got, "…") {
		t.Fatalf("orphaned call card = %q, want static … placeholder, not a live spinner", got)
	}
}

func TestDiffPreambleLinesCarryNoGutterNumbers(t *testing.T) {
	m := limeTestModel()
	detail := strings.Join([]string{
		"stdout:",
		"diff --git a/foo.txt b/foo.txt",
		"index 1111111..2222222 100644",
		"--- a/foo.txt",
		"+++ b/foo.txt",
		"@@ -1,2 +1,2 @@",
		" alpha",
		"-old",
		`\ No newline at end of file`,
		"+new",
	}, "\n")
	row := transcriptRow{kind: rowToolResult, id: "c", tool: "bash", status: tools.StatusOK, detail: detail}
	got := plainRender(t, m.renderRow(row, 80, buildRowContext(nil)))
	if strings.Contains(got, "   0") {
		t.Fatalf("diff card = %q, preamble lines must not be numbered from 0", got)
	}
	// "+new" is the second line of the new file: the no-newline marker must
	// not have advanced the counter past 2.
	if !strings.Contains(got, "   2 + new") {
		t.Fatalf("diff card = %q, expected +new numbered 2", got)
	}
}

func TestSuggestionDigitsTypeNormallyWhilePending(t *testing.T) {
	m := limeTestModel()
	// /clear mid-run leaves the transcript empty while pending, so digits must
	// keep typing into the composer.
	m.pending = true
	m = typeRunes(t, m, "1")
	if got := m.input.Value(); got != "1" {
		t.Fatalf("digit while pending should type normally, got %q", got)
	}
}

func TestGrepCardHeadShowsTargetAndPatternColumns(t *testing.T) {
	m := limeTestModel()
	rows := []transcriptRow{
		{kind: rowToolCall, id: "c", tool: "grep", detail: "internal/cli", arg: `flag\.|RegisterFlag`},
		{kind: rowToolResult, id: "c", tool: "grep", status: tools.StatusOK, detail: "internal/cli/root.go:41: fs := flag.NewFlagSet"},
	}
	rc := buildRowContext(rows)
	got := plainRender(t, m.renderRow(rows[1], 110, rc))
	for _, want := range []string{"Explored", "└ Search", `flag\.|RegisterFlag`} {
		if !strings.Contains(got, want) {
			t.Fatalf("grep card = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "internal/cli/root.go:41") {
		t.Fatalf("grep card = %q, must not dump search matches", got)
	}
	if exploreTargetLooksLikePath("grep", `flag\.|RegisterFlag`) {
		t.Fatal("grep regex arguments must not be treated as paths")
	}
}

// --- Stage 3: interactive surfaces ------------------------------------------

func TestFocusedPermissionCardShowsBadgeAndKeys(t *testing.T) {
	request := agent.PermissionRequest{
		ToolName:           "edit_file",
		Reason:             "writes internal/agent/exec.go",
		SideEffect:         "write",
		Risk:               sandbox.Risk{Level: sandbox.RiskMedium},
		AvailableDecisions: testAllPermissionDecisions(),
	}
	card, offsets := renderFocusedPermissionPrompt(request, 0, false, "", 80)
	got := plainRender(t, card)
	for _, want := range []string{"PERMISSION", "edit_file", "writes internal/agent/exec.go", "Yes, proceed", "[a]", "this session", "[s]", "don't ask again", "[y]", "continue without running it", "[d]", "[esc]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("permission card = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "risk:") {
		t.Fatalf("normal permission prompt must not render risk labels, got %q", got)
	}
	if len(offsets) != len(permissionOptions(request)) {
		t.Fatalf("offsets = %d, want one per option (%d)", len(offsets), len(permissionOptions(request)))
	}
}

func TestPermissionPromptCollapsesAfterDecision(t *testing.T) {
	m := limeTestModel()
	prompt := transcriptRow{kind: rowPermission, id: "call_1", permission: &agent.PermissionEvent{
		ToolCallID: "call_1", ToolName: "bash", Action: agent.PermissionActionPrompt,
	}}
	allowed := transcriptRow{kind: rowPermission, id: "call_1", permission: &agent.PermissionEvent{
		ToolCallID: "call_1", ToolName: "bash", Action: agent.PermissionActionAllow,
	}}
	rc := buildRowContext([]transcriptRow{prompt, allowed})

	if !rc.skip(prompt) {
		t.Fatal("a decided prompt row must collapse away")
	}
	if rc.skip(allowed) {
		t.Fatal("manual allow rows should remain as audit lines")
	}
	if got := plainRender(t, m.renderRow(allowed, 80, rc)); !strings.Contains(got, "allowed once · bash") {
		t.Fatalf("manual allow = %q, want allowed once · bash", got)
	}

	session := transcriptRow{kind: rowPermission, id: "call_session", permission: &agent.PermissionEvent{
		ToolCallID: "call_session", ToolName: "bash", Action: agent.PermissionActionAllow, DecisionAction: agent.PermissionDecisionAllowForSession,
	}}
	rcSession := buildRowContext([]transcriptRow{
		{kind: rowPermission, id: "call_session", permission: &agent.PermissionEvent{ToolCallID: "call_session", ToolName: "bash", Action: agent.PermissionActionPrompt}},
		session,
	})
	if rcSession.skip(session) {
		t.Fatal("session allow rows should remain as audit lines")
	}
	if got := plainRender(t, m.renderRow(session, 80, rcSession)); !strings.Contains(got, "allowed for session · bash") {
		t.Fatalf("session allow = %q, want allowed for session · bash", got)
	}

	grant := &agent.PermissionEvent{ToolCallID: "call_2", ToolName: "bash", Action: agent.PermissionActionAllow}
	grant.Grant = &sandbox.Grant{ToolName: "bash"}
	always := transcriptRow{kind: rowPermission, id: "call_2", permission: grant}
	promptTwo := transcriptRow{kind: rowPermission, id: "call_2", permission: &agent.PermissionEvent{
		ToolCallID: "call_2", ToolName: "bash", Action: agent.PermissionActionPrompt,
	}}
	rcTwo := buildRowContext([]transcriptRow{promptTwo, always})
	if rcTwo.skip(always) {
		t.Fatal("always allow rows should remain as audit lines")
	}
	if got := plainRender(t, m.renderRow(always, 80, rcTwo)); !strings.Contains(got, "always · bash") {
		t.Fatalf("always allow = %q, want always · bash", got)
	}

	denied := transcriptRow{kind: rowPermission, id: "call_3", permission: &agent.PermissionEvent{
		ToolCallID: "call_3", ToolName: "bash", Action: agent.PermissionActionDeny,
	}}
	if got := plainRender(t, m.renderRow(denied, 80, buildRowContext(nil))); !strings.Contains(got, "denied · bash") {
		t.Fatalf("deny = %q, want denied · bash", got)
	}
}

func TestUnpromptedAllowRowsCollapseIntoAutoTag(t *testing.T) {
	allow := transcriptRow{kind: rowPermission, id: "call_1", permission: &agent.PermissionEvent{
		ToolCallID: "call_1", ToolName: "edit_file", Action: agent.PermissionActionAllow,
	}}
	rc := buildRowContext([]transcriptRow{allow})
	if !rc.skip(allow) {
		t.Fatal("an unprompted (auto) allow row must collapse")
	}
}

func TestModelPickerRowsCarryCapabilityMeta(t *testing.T) {
	m := limeTestModel()
	picker := m.newModelPicker()
	if picker == nil {
		t.Fatal("expected a model picker")
	}
	withMeta := 0
	withCapability := 0
	for _, item := range picker.items {
		if strings.Contains(item.Meta, "K") || strings.Contains(item.Meta, "M") {
			withMeta++
		}
		if strings.Contains(item.Meta, "tools") || strings.Contains(item.Meta, "reasoning") || strings.Contains(item.Meta, "vision") {
			withCapability++
		}
		if strings.Contains(item.Meta, "API_KEY") {
			t.Fatalf("picker item %q leaked credential env metadata: %q", item.Value, item.Meta)
		}
		if !item.Remote && !item.Local {
			continue
		}
	}
	if withMeta == 0 {
		t.Fatalf("expected catalog models to expose ctx metadata, got %#v", picker.items[:minInt(3, len(picker.items))])
	}
	if withCapability == 0 {
		t.Fatalf("expected catalog models to expose capability metadata, got %#v", picker.items[:minInt(3, len(picker.items))])
	}

	m.picker = picker
	got := plainRender(t, m.pickerOverlay(100))
	for _, want := range []string{"Choose a model", "Enter select", "Ctrl+F favorite", "❯"} {
		if !strings.Contains(got, want) {
			t.Fatalf("picker overlay = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "API_KEY") {
		t.Fatalf("picker overlay leaked credential env metadata: %q", got)
	}
}

func TestModelPickerRowOmitsProviderTag(t *testing.T) {
	// The provider is shown as a section header above each group, so a row renders
	// just the model label — no repeated right-aligned provider tag.
	item := pickerItem{Label: "Claude Sonnet 4.6", Value: "claude-sonnet-4-6", Provider: "anthropic", Remote: true}
	got := plainRender(t, renderModelPickerRow(60, false, item))
	if !strings.Contains(got, "Claude Sonnet 4.6") {
		t.Fatalf("row = %q, missing model label", got)
	}
	if strings.Contains(got, "anthropic") {
		t.Fatalf("row = %q, should not repeat the provider tag (now a section header)", got)
	}
}

func TestModelPickerItemsCarryProviderTag(t *testing.T) {
	m := limeTestModel()
	picker := m.newModelPicker()
	if picker == nil {
		t.Fatal("expected a model picker")
	}
	tagged := 0
	for _, item := range picker.items {
		if strings.TrimSpace(item.Provider) != "" {
			tagged++
		}
	}
	if tagged == 0 {
		t.Fatalf("expected catalog models to carry a provider tag, got %#v", picker.items[:minInt(3, len(picker.items))])
	}
}

func TestSpecReviewCardShowsBadgePathAndKeys(t *testing.T) {
	got := plainRender(t, renderFocusedSpecReviewPrompt(pendingSpecReviewPrompt{
		SpecFilePath: "/repo/specs/zero-1.md",
		RelativePath: "specs/zero-1.md",
	}, 80))
	for _, want := range []string{"SPEC REVIEW", "specs/zero-1.md", "[a] approve", "[r] reject", "[e] edit file", "[esc] cancel"} {
		if !strings.Contains(got, want) {
			t.Fatalf("spec review card = %q, missing %q", got, want)
		}
	}
}

func TestPermissionCollapseIsRunScoped(t *testing.T) {
	// Some providers synthesize repeating ToolCallIDs (provider_tool_1 in
	// every turn). A decision from run 2 must not collapse run 1's undecided
	// prompt or steal its [auto]/hint attribution.
	runOnePrompt := transcriptRow{kind: rowPermission, id: "provider_tool_1", runID: 1, permission: &agent.PermissionEvent{
		ToolCallID: "provider_tool_1", ToolName: "bash", Action: agent.PermissionActionPrompt,
	}}
	runTwoDecision := transcriptRow{kind: rowPermission, id: "provider_tool_1", runID: 2, permission: &agent.PermissionEvent{
		ToolCallID: "provider_tool_1", ToolName: "bash", Action: agent.PermissionActionAllow,
	}}
	rc := buildRowContext([]transcriptRow{runOnePrompt, runTwoDecision})

	if rc.skip(runOnePrompt) {
		t.Fatal("run 1's undecided prompt must not collapse on run 2's decision")
	}
	// Run 2's allow had no prompt in run 2, so it reads as auto there.
	if !rc.skip(runTwoDecision) {
		t.Fatal("run 2's unprompted allow should fold into its tool card's [auto] tag")
	}

	// Same-run prompt+decision collapses the prompt but keeps the manual decision
	// as an audit line.
	sameRunPrompt := transcriptRow{kind: rowPermission, id: "gemini_tool_1", runID: 3, permission: &agent.PermissionEvent{
		ToolCallID: "gemini_tool_1", ToolName: "bash", Action: agent.PermissionActionPrompt,
	}}
	sameRunAllow := transcriptRow{kind: rowPermission, id: "gemini_tool_1", runID: 3, permission: &agent.PermissionEvent{
		ToolCallID: "gemini_tool_1", ToolName: "bash", Action: agent.PermissionActionAllow,
	}}
	rcSame := buildRowContext([]transcriptRow{sameRunPrompt, sameRunAllow})
	if !rcSame.skip(sameRunPrompt) {
		t.Fatal("a same-run decided prompt must collapse")
	}
	if rcSame.skip(sameRunAllow) {
		t.Fatal("the same-run manual decision line must render")
	}
}

func TestSessionsCardFieldsAreSanitized(t *testing.T) {
	inputs := []string{
		"evil\x1ftitle",
		"line\nbreak",
		"line\rbreak",
		"nul\x00byte",
		"esc\x1b[31mred",
		"csi\x1b[2J\x1b[H",
		"osc\x1b]0;pwned\a",
		"bell\aring",
		"back\bspace",
		"tab\there",
		"vert\vtab",
		"nel\u0085here",
	}
	for _, input := range inputs {
		got := sanitizeCardField(input)
		for _, r := range got {
			if unicode.IsControl(r) {
				t.Fatalf("sanitizeCardField(%q) left control rune %q in %q", input, r, got)
			}
		}
	}
}

func TestCommandPaletteKeepsDescriptionInsideOverlay(t *testing.T) {
	m := limeTestModel()
	m.input.SetValue("/effort")
	m.recomputeSuggestions()
	if !m.commandPaletteOpen || len(m.suggestions) != 1 || m.suggestions[0].Name != "/effort" {
		t.Fatalf("setup: expected a single /effort suggestion, got palette=%v matches=%#v", m.commandPaletteOpen, m.suggestions)
	}
	overlay := plainRender(t, m.suggestionOverlay(96))
	if !strings.Contains(overlay, "reasoning effort") {
		t.Fatalf("palette description = %q", overlay)
	}
	footer := plainRender(t, m.footerView(96))
	if strings.Contains(footer, "reasoning effort") {
		t.Fatalf("composer footer duplicated palette description: %q", footer)
	}
}
