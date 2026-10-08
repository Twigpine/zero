package tui

import (
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/tools"
)

func TestCollapseRepeatedStatusCardDropsDuplicate(t *testing.T) {
	detail := "Swarm status (team default): 4 task(s) — 4 running"
	rows := []transcriptRow{
		{kind: rowAssistant, text: "let me check"},
		{kind: rowToolCall, tool: "swarm_status"},
		{kind: rowToolResult, tool: "swarm_status", detail: detail},
		{kind: rowToolCall, tool: "swarm_status"}, // the new check's call row
	}
	out := collapseRepeatedStatusCard(rows, transcriptRow{kind: rowToolResult, tool: "swarm_status", detail: detail})
	if len(out) != 2 {
		t.Fatalf("duplicate status pair should collapse to 2 rows, got %d: %+v", len(out), out)
	}
	if out[0].kind != rowAssistant || out[1].kind != rowToolCall {
		t.Fatalf("collapse kept the wrong rows: %+v", out)
	}
}

func TestCollapseRepeatedStatusCardKeepsChangedState(t *testing.T) {
	rows := []transcriptRow{
		{kind: rowToolCall, tool: "swarm_status"},
		{kind: rowToolResult, tool: "swarm_status", detail: "4 running"},
		{kind: rowToolCall, tool: "swarm_status"},
	}
	out := collapseRepeatedStatusCard(rows, transcriptRow{kind: rowToolResult, tool: "swarm_status", detail: "2 running, 2 done"})
	if len(out) != 3 {
		t.Fatalf("a changed status must not collapse, got %d rows", len(out))
	}
}

func TestCollapseRepeatedStatusCardIgnoresInterveningContent(t *testing.T) {
	rows := []transcriptRow{
		{kind: rowToolCall, tool: "swarm_status"},
		{kind: rowToolResult, tool: "swarm_status", detail: "4 running"},
		{kind: rowReasoning, text: "thinking"},
		{kind: rowToolCall, tool: "swarm_status"},
	}
	out := collapseRepeatedStatusCard(rows, transcriptRow{kind: rowToolResult, tool: "swarm_status", detail: "4 running"})
	if len(out) != 4 {
		t.Fatalf("intervening content must prevent collapse, got %d rows", len(out))
	}
}

func TestToolResultCollapsesLongOutputByDefault(t *testing.T) {
	m := transcriptViewTestModel()
	long := numberedLines(cardBodyMaxLines + 10)
	rc := buildRowContext(nil)

	row := transcriptRow{kind: rowToolResult, id: "t1", tool: "mcp_exa_web_search_exa", status: tools.StatusOK, detail: long}
	collapsed := plainRender(t, m.renderRow(row, m.width, rc))
	if strings.Contains(collapsed, "line-005") {
		t.Errorf("collapsed card must hide the body, got:\n%s", collapsed)
	}
	if !strings.Contains(collapsed, "click to expand") {
		t.Errorf("collapsed card must show the expand hint, got:\n%s", collapsed)
	}

	row.expanded = true
	expanded := plainRender(t, m.renderRow(row, m.width, rc))
	if !strings.Contains(expanded, "line-005") {
		t.Errorf("expanded card must show the body, got:\n%s", expanded)
	}
}

func TestToolResultShortOutputStaysInline(t *testing.T) {
	m := transcriptViewTestModel()
	row := transcriptRow{kind: rowToolResult, id: "t2", tool: "mcp_exa_web_search_exa", status: tools.StatusOK, detail: numberedLines(3)}
	out := plainRender(t, m.renderRow(row, m.width, buildRowContext(nil)))
	if strings.Contains(out, "click to expand") {
		t.Errorf("short output must not collapse, got:\n%s", out)
	}
	if !strings.Contains(out, "line-003") {
		t.Errorf("short output must render inline, got:\n%s", out)
	}
}

func TestDiffToolOutputNeverCollapses(t *testing.T) {
	m := transcriptViewTestModel()
	long := numberedLines(cardBodyMaxLines + 10)
	for _, tool := range []string{"edit_file", "apply_patch", "write_file"} {
		row := transcriptRow{kind: rowToolResult, id: "d", tool: tool, status: tools.StatusOK, detail: long}
		out := plainRender(t, m.renderRow(row, m.width, buildRowContext(nil)))
		if strings.Contains(out, "click to expand") {
			t.Errorf("%s output must stay reviewable, not collapse:\n%s", tool, out)
		}
	}
}

func TestToggleTranscriptRowTogglesToolResult(t *testing.T) {
	m := transcriptViewTestModel()
	m.transcript = []transcriptRow{{kind: rowToolResult, id: "t", tool: "custom_tool"}}
	if m.transcript[0].expanded {
		t.Fatal("tool result must default to collapsed")
	}
	m = m.toggleTranscriptRow(0)
	if !m.transcript[0].expanded {
		t.Fatal("toggle should expand the tool result")
	}
	m = m.toggleTranscriptRow(0)
	if m.transcript[0].expanded {
		t.Fatal("toggle should collapse the tool result again")
	}
}

func TestToolResultRowExposesClickToggle(t *testing.T) {
	m := transcriptViewTestModel()
	row := transcriptRow{kind: rowToolResult, id: "t", tool: "custom_tool", status: tools.StatusOK, detail: numberedLines(cardBodyMaxLines + 5)}
	_, selectable := m.renderSelectableToolResultRow(0, row, m.width, buildRowContext(nil), 0)
	if len(selectable) < 1 || !selectable[0].toggle {
		t.Fatalf("tool result head must be a clickable toggle line, got %#v", selectable)
	}
	if len(selectable) < 2 || selectable[1].text == "" {
		t.Fatalf("tool result visible body/footer must stay selectable, got %#v", selectable)
	}
}

func TestAlwaysExpandedToolResultHeaderIsNotToggleable(t *testing.T) {
	m := transcriptViewTestModel()
	for _, tool := range []string{"edit_file", "apply_patch", "write_file"} {
		row := transcriptRow{
			kind:   rowToolResult,
			id:     "diff",
			tool:   tool,
			status: tools.StatusOK,
			detail: "--- a/example.go\n+++ b/example.go\n@@ -1 +1 @@\n-old\n+new",
		}
		_, selectable := m.renderSelectableToolResultRow(0, row, m.width, buildRowContext(nil), 0)
		if len(selectable) == 0 {
			t.Fatalf("%s: expected a visible diff result row", tool)
		}
		if selectable[0].toggle {
			t.Fatalf("%s header must not advertise a collapse toggle: %#v", tool, selectable[0])
		}
	}
}

func TestToolResultOutputCannotCreateToggle(t *testing.T) {
	m := transcriptViewTestModel()
	row := transcriptRow{
		kind:   rowToolResult,
		id:     "custom",
		tool:   "custom_tool",
		status: tools.StatusOK,
		detail: "the file literally says: click to expand",
	}
	_, selectable := m.renderSelectableToolResultRow(0, row, m.width, buildRowContext(nil), 0)
	if len(selectable) == 0 {
		t.Fatal("expected a visible tool result row")
	}
	if selectable[0].toggle {
		t.Fatalf("tool-output text must not create a header toggle: %#v", selectable[0])
	}
}

func TestCollapsedExploreToolResultHeaderIsToggleable(t *testing.T) {
	m := transcriptViewTestModel()
	row := transcriptRow{
		kind:   rowToolResult,
		id:     "read",
		tool:   "read_file",
		status: tools.StatusOK,
		detail: numberedLines(cardBodyMaxLines + 5),
	}
	_, selectable := m.renderSelectableToolResultRow(0, row, m.width, buildRowContext(nil), 0)
	if len(selectable) == 0 {
		t.Fatal("expected a visible explore result row")
	}
	if !selectable[0].toggle {
		t.Fatalf("collapsed explore card header must remain toggleable: %#v", selectable[0])
	}
}
