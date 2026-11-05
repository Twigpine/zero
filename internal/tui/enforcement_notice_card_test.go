package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/tools"
)

const cardNotice = "denyRead is configured, so the Windows sandbox uses the token shape without WRITE_RESTRICTED (#869)"

func resultWithPreviewAndNotice() agent.ToolResult {
	return agent.ToolResult{
		ToolCallID:         "call-1",
		Name:               "edit_file",
		Status:             tools.StatusOK,
		Output:             "Successfully edited x.go (replaced 1 occurrence).",
		Display:            tools.Display{Summary: "Successfully edited x.go.", Kind: "file", Preview: "--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-old\n+new"},
		EnforcementNotices: []string{cardNotice},
	}
}

func renderedCard(row transcriptRow, expanded bool) string {
	row.expanded = expanded
	return renderToolResultCard(row, 100, rowContext{}, cardRenderOptions{bodyCap: 20})
}

func rowForResult(result agent.ToolResult) transcriptRow {
	return transcriptRow{
		kind: rowToolResult, id: "r1", tool: result.Name, status: result.Status,
		text: toolResultRowText(result), detail: toolResultDetail(result),
		enforcementNotices: result.EnforcementNotices,
	}
}

// THE DISCLOSURE HAS TO REACH THE CARD, NOT JUST THE ROW TEXT.
//
// The notice is prepended to ModelOutput, which toolResultRowText carries into
// row.text, and toolCardHead is handed row.text. But the head renders the action
// and target, so the notice went nowhere. Every result with a rich preview (each
// edit and write card) rendered with no disclosure at all.
//
// Collapsed as well as expanded: a trade the operator has to expand a card to
// discover has not been disclosed.
func TestToolCardShowsTheEnforcementDisclosure(t *testing.T) {
	row := rowForResult(resultWithPreviewAndNotice())

	for _, expanded := range []bool{false, true} {
		card := renderedCard(row, expanded)
		if !strings.Contains(card, "WRITE_RESTRICTED") {
			t.Errorf("expanded=%v: the card rendered no enforcement disclosure:\n%s", expanded, card)
		}
	}
}

// THE DISCLOSURE IS DATA, NOT PROSE GLUED ONTO THE DIFF.
//
// row.detail is parsed as a diff by the files panel (planDiffStat,
// perFileDiffStats) and rendered line by line by the file view. Prefixing it
// with the notice would have been the shorter fix and would have corrupted both,
// so the notice travels in its own field and the diff stays a diff.
func TestTheDisclosureDoesNotContaminateTheDiffDetail(t *testing.T) {
	result := resultWithPreviewAndNotice()
	row := rowForResult(result)

	if strings.Contains(row.detail, "WRITE_RESTRICTED") {
		t.Fatalf("the notice leaked into the diff detail, which is parsed as a diff: %q", row.detail)
	}
	adds, dels := planDiffStat(row.detail)
	if adds != 1 || dels != 1 {
		t.Errorf("diff stats changed with the disclosure attached: +%d -%d, want +1 -1", adds, dels)
	}
}

// AND IT HAS TO SURVIVE A RESUME.
//
// The session payload carried the notice only inside the "output" string. The
// rich card is rebuilt from displayPreview, which never had it, so a restored
// transcript lost the disclosure even though the row it replaced had shown it.
func TestRestoredSessionKeepsTheEnforcementDisclosure(t *testing.T) {
	encoded, err := json.Marshal(toolResultSessionPayload(resultWithPreviewAndNotice()))
	if err != nil {
		t.Fatalf("marshal session payload: %v", err)
	}

	rows := transcriptRowsFromSessionEvents([]sessions.Event{{Type: sessions.EventToolResult, Payload: json.RawMessage(encoded)}})
	if len(rows) != 1 {
		t.Fatalf("expected one restored row, got %d", len(rows))
	}
	if len(rows[0].enforcementNotices) == 0 {
		t.Fatal("the restored row carries no enforcement notices, so the resumed transcript lost the disclosure")
	}
	for _, expanded := range []bool{false, true} {
		if card := renderedCard(rows[0], expanded); !strings.Contains(card, "WRITE_RESTRICTED") {
			t.Errorf("expanded=%v: the restored card rendered no disclosure:\n%s", expanded, card)
		}
	}
}

// A result with no notice must not grow card furniture, or every card gains a
// blank line and the disclosure stops standing out.
func TestOrdinaryResultsGainNoNoticeLines(t *testing.T) {
	result := resultWithPreviewAndNotice()
	result.EnforcementNotices = nil
	plain := renderedCard(rowForResult(result), true)

	result.EnforcementNotices = []string{"", "   "}
	blank := renderedCard(rowForResult(result), true)

	if plain != blank {
		t.Errorf("blank notices changed the card:\n--- none ---\n%s\n--- blank ---\n%s", plain, blank)
	}
}

// THE NO-PREVIEW CARD IS THE ONE THE PREVIEW TEST CANNOT SEE.
//
// The disclosure travels in two forms: typed EnforcementNotices, which the card
// renders as its own furniture, and ModelOutput, which has the notice composed
// in. A rich preview is undecorated, so an edit card was right. Every bash and
// exec result, and every error, has no preview and fell back to ModelOutput, so
// row.detail already began with the notice and the card drew it twice: once in
// the notice lines and once at the top of the body.
//
// Both halves are asserted, because a body that lost the notice by losing the
// output would also count once.
func resultWithoutPreviewAndNotice(status tools.Status, output string) agent.ToolResult {
	return agent.ToolResult{
		ToolCallID:         "call-2",
		Name:               "bash",
		Status:             status,
		Output:             output,
		EnforcementNotices: []string{cardNotice},
	}
}

func countNoticeAndBody(t *testing.T, card string, body string) (int, int) {
	t.Helper()
	return strings.Count(card, "WRITE_RESTRICTED"), strings.Count(card, body)
}

func TestNoPreviewCardShowsTheDisclosureExactlyOnce(t *testing.T) {
	cases := []struct {
		name   string
		status tools.Status
		output string
	}{
		{"success", tools.StatusOK, "PROBE-BODY-OK"},
		{"error", tools.StatusError, "PROBE-BODY-ERR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := rowForResult(resultWithoutPreviewAndNotice(tc.status, tc.output))
			for _, expanded := range []bool{false, true} {
				notices, bodies := countNoticeAndBody(t, renderedCard(row, expanded), tc.output)
				if notices != 1 {
					t.Errorf("expanded=%v: disclosure rendered %d times, want exactly 1", expanded, notices)
				}
				if bodies != 1 {
					t.Errorf("expanded=%v: command output rendered %d times, want exactly 1", expanded, bodies)
				}
			}
		})
	}
}

// The durable path had the same mismatch: the payload stores the decorated
// output beside the typed notices, and restoration used that output as the card
// body whenever no distinct preview was stored.
func TestRestoredNoPreviewCardShowsTheDisclosureExactlyOnce(t *testing.T) {
	cases := []struct {
		name   string
		status tools.Status
		output string
	}{
		{"success", tools.StatusOK, "PROBE-BODY-OK"},
		{"error", tools.StatusError, "PROBE-BODY-ERR"},
		// A command that printed nothing under an enforced profile still has a
		// real notice. The stored body is empty, which restoration must treat as
		// present-and-empty rather than absent.
		{"empty output", tools.StatusOK, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(toolResultSessionPayload(resultWithoutPreviewAndNotice(tc.status, tc.output)))
			if err != nil {
				t.Fatal(err)
			}
			rows := transcriptRowsFromSessionEvents([]sessions.Event{{Type: sessions.EventToolResult, Payload: json.RawMessage(encoded)}})
			if len(rows) != 1 {
				t.Fatalf("expected one restored row, got %d", len(rows))
			}
			for _, expanded := range []bool{false, true} {
				card := renderedCard(rows[0], expanded)
				if notices := strings.Count(card, "WRITE_RESTRICTED"); notices != 1 {
					t.Errorf("expanded=%v: restored card rendered the disclosure %d times, want exactly 1:\n%s", expanded, notices, card)
				}
				if tc.output != "" && strings.Count(card, tc.output) != 1 {
					t.Errorf("expanded=%v: restored card rendered the output %d times, want exactly 1:\n%s", expanded, strings.Count(card, tc.output), card)
				}
			}
		})
	}
}

// A CLI-WRITTEN RESULT RESUMED IN THE TUI MUST STILL DISCLOSE, ONCE.
//
// The headless writers and the interactive writer append to the same default
// session store, and the TUI resumes from it. The headless payload used to
// carry only the decorated ModelOutput: no typed notices, no undecorated body.
// On restore the transcript found neither, and for a long result the card is
// collapsed by default, so there was no body to carry the decorated text and no
// notice furniture to draw it. The disclosure the run had shown was simply gone
// from the resumed transcript.
//
// Every writer now goes through agent.ToolResultSessionPayload, so this exercises the
// exact bytes the CLI persists, restores them the way the TUI does, and renders
// the collapsed card, which is the shape the old CLI test could not reach.
func TestHeadlessWrittenCollapsedResultRestoresTheDisclosureExactlyOnce(t *testing.T) {
	var lines []string
	for i := 0; i < cardBodyMaxLines*3; i++ {
		lines = append(lines, fmt.Sprintf("PROBE-LINE-%03d", i))
	}
	result := agent.ToolResult{
		ToolCallID:         "call-cli",
		Name:               "bash",
		Status:             tools.StatusOK,
		Output:             strings.Join(lines, "\n"),
		EnforcementNotices: []string{cardNotice},
	}

	// The CLI's persisted payload IS this function now; encode it as the
	// session store would.
	encoded, err := json.Marshal(agent.ToolResultSessionPayload(result))
	if err != nil {
		t.Fatal(err)
	}
	rows := transcriptRowsFromSessionEvents([]sessions.Event{{Type: sessions.EventToolResult, Payload: json.RawMessage(encoded)}})
	if len(rows) != 1 {
		t.Fatalf("expected one restored row, got %d", len(rows))
	}
	if len(rows[0].enforcementNotices) == 0 {
		t.Fatal("the headless payload carried no typed notices, so the resumed card cannot render the disclosure")
	}

	for _, expanded := range []bool{false, true} {
		card := renderedCard(rows[0], expanded)
		if n := strings.Count(card, "WRITE_RESTRICTED"); n != 1 {
			t.Errorf("expanded=%v: restored headless card rendered the disclosure %d time(s), want exactly 1:\n%s", expanded, n, card)
		}
	}
	// Collapsed is the case that used to lose it: with no body shown there was
	// nothing to carry a decorated notice.
	if card := renderedCard(rows[0], false); !strings.Contains(card, "WRITE_RESTRICTED") {
		t.Errorf("the collapsed restored card has no disclosure at all:\n%s", card)
	}
}
