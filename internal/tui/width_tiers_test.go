package tui

import (
	"context"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/sandbox"
	"github.com/Twigpine/zero/internal/tools"
)

// The spec's adaptive acceptance criteria: a table across widths asserting
// which segments render at each tier.
func TestWidthTierSegments(t *testing.T) {
	diff := strings.Join([]string{
		"+++ b/internal/cli/root.go",
		"@@ -1,2 +1,2 @@",
		" package cli",
		"-var old = 1",
		"+var new = 2",
	}, "\n")

	cases := []struct {
		width      int
		wantCtx    bool // header context window ("200K")
		wantCwd    bool // header cwd segment
		wantArg    bool // tool-card arg column
		wantGutter bool // diff line-number gutter
	}{
		{width: 58, wantCtx: false, wantCwd: false, wantArg: false, wantGutter: false},
		{width: 70, wantCtx: false, wantCwd: false, wantArg: false, wantGutter: false},
		{width: 80, wantCtx: false, wantCwd: true, wantArg: false, wantGutter: true},
		{width: 100, wantCtx: true, wantCwd: true, wantArg: true, wantGutter: true},
		{width: 120, wantCtx: true, wantCwd: true, wantArg: true, wantGutter: true},
	}

	for _, tc := range cases {
		m := newModel(context.Background(), Options{
			Cwd:          "/Users/dev/zero-project-workspace",
			ProviderName: "anthropic",
			ModelName:    "claude-sonnet-4.5",
		})
		m.width, m.height = tc.width, 30

		title := plainRender(t, m.titleBar(tc.width))
		if got := strings.Contains(title, "200K"); got != tc.wantCtx {
			t.Errorf("width %d: title ctx = %v, want %v (%q)", tc.width, got, tc.wantCtx, title)
		}
		if got := strings.Contains(title, "zero-project-workspace"); got != tc.wantCwd {
			t.Errorf("width %d: title cwd = %v, want %v (%q)", tc.width, got, tc.wantCwd, title)
		}

		rows := []transcriptRow{
			{kind: rowToolCall, id: "c", tool: "custom_tool", detail: "internal/cli", arg: "RegisterFlag"},
			{kind: rowToolResult, id: "c", tool: "custom_tool", status: tools.StatusOK, detail: "ok"},
		}
		rc := buildRowContext(rows)
		card := plainRender(t, m.renderRow(rows[1], tc.width, rc))
		if got := strings.Contains(card, "RegisterFlag"); got != tc.wantArg {
			t.Errorf("width %d: card arg column = %v, want %v (%q)", tc.width, got, tc.wantArg, card)
		}

		diffRow := transcriptRow{kind: rowToolResult, id: "d", tool: "edit_file", status: tools.StatusOK, detail: diff}
		diffCard := plainRender(t, m.renderRow(diffRow, tc.width, buildRowContext(nil)))
		if got := strings.Contains(diffCard, "   2 + var new = 2") || strings.Contains(diffCard, "   2 +"); got != tc.wantGutter {
			t.Errorf("width %d: diff gutter = %v, want %v (%q)", tc.width, got, tc.wantGutter, diffCard)
		}

		status := plainRender(t, m.statusLine(tc.width))
		// Status line now carries the run-state chip (permission mode), NOT the
		// surface or model (those live in the title bar / composer rule).
		if strings.Contains(status, "interactive") || strings.Contains(status, "claude-sonnet-4.5") {
			t.Errorf("width %d: status should not include surface or model (%q)", tc.width, status)
		}
		if !strings.Contains(status, "auto-approve") {
			t.Errorf("width %d: status should show the permission mode (%q)", tc.width, status)
		}
		divider := plainRender(t, m.composerDividerLine(tc.width))
		// The composer rule is model-only now; mode/effort moved to the status line.
		if !strings.Contains(divider, "claude-sonnet-4.5") {
			t.Errorf("width %d: composer divider must keep the model label (%q)", tc.width, divider)
		}
		if strings.Contains(divider, "auto-approve") {
			t.Errorf("width %d: composer divider should no longer show the mode (%q)", tc.width, divider)
		}
	}
}

func TestTinyTierSingleSegmentAndRailLessCards(t *testing.T) {
	m := newModel(context.Background(), Options{ProviderName: "anthropic", ModelName: "claude-sonnet-4.5"})
	m.width, m.height = 40, 20

	status := plainRender(t, m.statusLine(40))
	// Tiny status shows the permission-mode chip (the safety-relevant run-state),
	// not the provider or model.
	if !strings.Contains(status, "auto-approve") {
		t.Fatalf("tiny status = %q, want the permission-mode chip", status)
	}
	if strings.Contains(status, "anthropic") || strings.Contains(status, "claude-sonnet-4.5") {
		t.Fatalf("tiny status = %q, want mode only (no provider/model)", status)
	}
	divider := plainRender(t, m.composerDividerLine(40))
	if !strings.Contains(divider, "claude-sonnet-4.5") {
		t.Fatalf("tiny composer divider = %q, want the model label", divider)
	}
	if strings.Contains(divider, "auto-approve") {
		t.Fatalf("tiny composer divider = %q, mode should have moved to the status line", divider)
	}

	row := transcriptRow{kind: rowToolResult, id: "c", tool: "grep", status: tools.StatusOK, detail: "a.go:1: x"}
	card := plainRender(t, m.renderRow(row, 40, buildRowContext(nil)))
	for _, line := range strings.Split(card, "\n") {
		if strings.HasPrefix(line, "│") || strings.HasSuffix(line, "│") {
			t.Fatalf("tiny card keeps side borders: %q", line)
		}
	}
}

func TestTitleBarKeepsWorkspaceWithLongBranchAndModel(t *testing.T) {
	m := newModel(context.Background(), Options{
		Cwd:          "/workspace/zero",
		ProviderName: "ollama-cloud",
		ModelName:    "cogito-2.1:671b-extra-long-model-name",
	})
	m.gitBranch = "feat/tui-assistant-response-cleanup"

	got := plainRender(t, m.titleBar(108))
	for _, want := range []string{"", "/workspace/zero", "feat/tui-assistant-response-cleanup", "ollama-cloud/cogito-2.1:671b-extra-long-model-name"} {
		if !strings.Contains(got, want) {
			t.Fatalf("title bar = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, " 0 ") {
		t.Fatalf("title bar = %q, should not include old badge", got)
	}
	for index, line := range strings.Split(got, "\n") {
		if width := lipgloss.Width(line); width > 108 {
			t.Fatalf("title line %d width = %d, want <= 108: %q", index, width, line)
		}
	}
}

func TestComposerDividerRendersMetaAtExactFit(t *testing.T) {
	m := newModel(context.Background(), Options{
		ModelName:      "gpt-4o",
		PermissionMode: agent.PermissionModeAsk,
	})
	// The divider meta is model-only now (mode/effort moved to the status line).
	meta := zeroTheme.muted.Render("gpt-4o")
	width := lipgloss.Width(meta) + 4

	got := plainRender(t, m.composerDividerLine(width))
	if !strings.Contains(got, "gpt-4o") {
		t.Fatalf("exact-fit composer divider = %q, want the model label", got)
	}
}

// The spec's hard rendering invariant: never emit a styled line wider than
// the terminal, across the whole frame at every tier — including the empty
// state, ask-user rows, permission details, and pending image chips, which
// each overflowed at some width before being fitted.
func TestViewNeverExceedsTerminalWidth(t *testing.T) {
	diff := "+++ b/a.go\n@@ -1,1 +1,1 @@\n-old line that is reasonably long for the card\n+new line that is reasonably long for the card"
	for _, width := range []int{24, 40, 58, 70, 80, 100, 120} {
		m := newModel(context.Background(), Options{
			Cwd:          "/Users/dev/zero-project-workspace",
			ProviderName: "anthropic",
			ModelName:    "claude-sonnet-4.5",
		})
		m.width, m.height = width, 24

		// Empty state first: the centered tagline/hint must also fit.
		for index, line := range strings.Split(viewString(m.View()), "\n") {
			if got := lipgloss.Width(line); got > width {
				t.Fatalf("width %d: empty-state line %d is %d cells wide: %q", width, index, got, line)
			}
		}

		m.pendingImageLabels = []string{"Screenshot 2026-06-10 at 09.41.13.png", "Screenshot 2026-06-10 at 09.44.02.png"}
		m.transcript = append(m.transcript,
			transcriptRow{kind: rowUser, text: "please change the longest line in the file to something even longer than before"},
			transcriptRow{kind: rowToolCall, id: "c1", tool: "grep", detail: "internal/cli", arg: "RegisterFlag|flag\\."},
			transcriptRow{kind: rowToolResult, id: "c1", tool: "grep", status: tools.StatusOK, detail: "internal/cli/root.go:41: fs := flag.NewFlagSet(\"zero\", flag.ContinueOnError)"},
			transcriptRow{kind: rowToolResult, id: "c2", tool: "edit_file", status: tools.StatusOK, detail: diff},
			transcriptRow{kind: rowSystem, text: "Mode set to ask."},
			transcriptRow{kind: rowAskUser, id: "ask1", text: "ask_user: which of these very long alternative naming schemes should the new flag adopt", detail: "1. choose between --version and --print-version  (--version, --print-version, keep both and alias them)"},
			transcriptRow{kind: rowPermission, id: "p1", permission: &permissionEventLongDetailFixture},
			transcriptRow{kind: rowAssistant, text: "Done — the change is in.", final: true, turnTools: 2},
		)

		for index, line := range strings.Split(viewString(m.View()), "\n") {
			if got := lipgloss.Width(line); got > width {
				t.Fatalf("width %d: frame line %d is %d cells wide: %q", width, index, got, line)
			}
		}
	}
}

var permissionEventLongDetailFixture = agent.PermissionEvent{
	ToolCallID:     "p1",
	ToolName:       "bash",
	Action:         agent.PermissionActionPrompt,
	Permission:     "prompt",
	PermissionMode: agent.PermissionModeAsk,
	SideEffect:     "runs `go test ./... -timeout 600s` in /Users/dev/zero-project-workspace with network access",
	Reason:         "command writes outside the workspace and downloads modules from the network proxy",
	Risk:           sandbox.Risk{Level: sandbox.RiskMedium},
}
