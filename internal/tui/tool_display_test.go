package tui

import (
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
)

func TestToolDisplayNameCleansMCPNames(t *testing.T) {
	cases := map[string]string{
		"mcp_exa_web_search_exa": "web search",
		"mcp_exa_web_fetch_exa":  "web fetch",
		"mcp_foo_bar":            "bar",
		"write_file":             "Create",
		"edit_file":              "Edit",
		"read_file":              "Read",
		"bash":                   "Run",
		"browser_open":           "Open browser",
		"browser_snapshot":       "Browser snapshot",
		"capture_artifact":       "Capture",
		"web_search":             "web_search", // built-in, unchanged
	}
	for in, want := range cases {
		if got := toolDisplayName(in); got != want {
			t.Errorf("toolDisplayName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestToolCardHeadShowsCleanMCPName(t *testing.T) {
	m := transcriptViewTestModel()
	row := transcriptRow{kind: rowToolResult, id: "t", tool: "mcp_exa_web_search_exa", status: tools.StatusOK, detail: "short result"}
	out := plainRender(t, m.renderRow(row, m.width, buildRowContext(nil)))
	if strings.Contains(out, "mcp_exa_web_search_exa") {
		t.Errorf("card head must not show the raw MCP tool name:\n%s", out)
	}
	if !strings.Contains(out, "web search") {
		t.Errorf("card head should show the clean 'web search' label:\n%s", out)
	}
}

func TestToolResultCardShowsCompactOutputAndRawArtifact(t *testing.T) {
	row := transcriptRow{
		kind:   rowToolResult,
		id:     "compact",
		tool:   "exec_command",
		status: tools.StatusOK,
		detail: "output:\n[zero] 20 passing package lines omitted\nexit_code: 0",
		meta: map[string]string{
			"truncated":  "true",
			"spill_path": "/tmp/zero-output.log",
			"output_budget_estimated_original_tokens": "4200",
			"output_budget_estimated_retained_tokens": "180",
		},
	}
	rendered := plainRender(t, (model{}).renderRow(row, 100, buildRowContext([]transcriptRow{row})))
	for _, want := range []string{"compact 4.2k→180 tok", "raw saved"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered compact result missing %q:\n%s", want, rendered)
		}
	}
}
