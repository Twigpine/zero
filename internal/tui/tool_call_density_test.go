package tui

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/sandbox"
	"github.com/Gitlawb/zero/internal/tools"
)

var sandboxBlockForTest = sandbox.Block{
	Code:     sandbox.BlockOutsideWorkspace,
	ToolName: "write_file",
	Action:   sandbox.ActionDeny,
	Reason:   "writes must stay inside workspace",
}

// --- Fix 1: permissionEventIsNoteworthy ---------------------------------------

func TestPermissionEventNoteworthySuppressesSilentAutoApprove(t *testing.T) {
	// action=allow, no decisionAction → a silent auto-approve (auto mode, or a
	// pre-granted scope that matched). Not noteworthy: the tool card speaks for it.
	event := agent.PermissionEvent{
		ToolName:       "mcp_filesystem_create_directory",
		Action:         agent.PermissionActionAllow,
		PermissionMode: agent.PermissionModeAuto,
		GrantMatched:   true,
	}
	if permissionEventIsNoteworthy(event) {
		t.Fatal("silent auto-approve should not be noteworthy")
	}
}

func TestPermissionEventNoteworthyKeepsRealDecisions(t *testing.T) {
	cases := []struct {
		name  string
		event agent.PermissionEvent
	}{
		{"prompt", agent.PermissionEvent{Action: agent.PermissionActionPrompt}},
		{"deny", agent.PermissionEvent{Action: agent.PermissionActionDeny}},
		{"cancel", agent.PermissionEvent{Action: agent.PermissionActionCancel}},
		{"allow-once-decided", agent.PermissionEvent{Action: agent.PermissionActionAllow, DecisionAction: agent.PermissionDecisionAllow}},
		{"always", agent.PermissionEvent{Action: agent.PermissionActionAllow, DecisionAction: agent.PermissionDecisionAlwaysAllow}},
		{"session", agent.PermissionEvent{Action: agent.PermissionActionAllow, DecisionAction: agent.PermissionDecisionAllowForSession}},
		{"blocked", agent.PermissionEvent{Action: agent.PermissionActionAllow, Block: &sandboxBlockForTest}},
	}
	for _, tc := range cases {
		if !permissionEventIsNoteworthy(tc.event) {
			t.Fatalf("%s: expected noteworthy permission event", tc.name)
		}
	}
}

// --- Fix 2+4: displayPath -----------------------------------------------------

func TestDisplayPathRelativizesUnderCwd(t *testing.T) {
	// Use a real OS-absolute path (t.TempDir is absolute on every platform) so
	// filepath.IsAbs holds on Windows too (where "/work" is NOT absolute).
	cwd := t.TempDir()
	abs := filepath.Join(cwd, "examples", "calc", "calc.go")
	if got := displayPath(cwd, abs); got != "examples/calc/calc.go" {
		t.Fatalf("displayPath under cwd = %q, want examples/calc/calc.go", got)
	}
}

func TestDisplayPathAbbreviatesHome(t *testing.T) {
	home := t.TempDir()
	restore := userHomeDir
	userHomeDir = func() (string, error) { return home, nil }
	defer func() { userHomeDir = restore }()

	abs := filepath.Join(home, "projects", "zero", "main.go")
	if got := displayPath(t.TempDir(), abs); got != "~/projects/zero/main.go" {
		t.Fatalf("displayPath under home = %q, want ~/projects/zero/main.go", got)
	}
}

func TestDisplayPathTruncatesExternalToTail(t *testing.T) {
	// A path that is OS-absolute but under neither cwd nor home → tail segments.
	// Build it under a temp root, then point cwd/home elsewhere so neither matches.
	external := t.TempDir() // absolute on every platform
	abs := filepath.Join(external, "vendor", "deep", "pkg", "calc", "calc.go")

	restore := userHomeDir
	userHomeDir = func() (string, error) { return t.TempDir(), nil } // unrelated home
	defer func() { userHomeDir = restore }()

	got := displayPath(t.TempDir(), abs) // unrelated cwd
	if got != "…/pkg/calc/calc.go" {
		t.Fatalf("displayPath external = %q, want …/pkg/calc/calc.go (last 3 segments)", got)
	}
}

func TestDisplayPathLeavesRelativeInputUntouched(t *testing.T) {
	if got := displayPath("/work/zero", "examples/calc.go"); got != "examples/calc.go" {
		t.Fatalf("relative input = %q, want examples/calc.go", got)
	}
}

// --- Fix 3: collapse redundant success bodies ---------------------------------

func TestRedundantConfirmationCollapsesSuccessBody(t *testing.T) {
	m := limeTestModel()
	row := transcriptRow{
		kind:   rowToolResult,
		id:     "c1",
		tool:   "mcp_filesystem_create_directory",
		status: tools.StatusOK,
		detail: "Successfully created directory /work/zero/examples/calc",
	}
	card := plainRender(t, m.renderRow(row, 80, buildRowContext(nil)))
	lines := strings.Split(strings.TrimRight(card, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("redundant-confirmation card should be one line, got %d:\n%s", len(lines), card)
	}
	if strings.Contains(card, "Successfully created directory") {
		t.Fatalf("confirmation body should be suppressed (head already says it), got:\n%s", card)
	}
}

func TestExplorationOutputSummarizesBody(t *testing.T) {
	m := limeTestModel()
	rows := []transcriptRow{{
		kind:   rowToolCall,
		id:     "c2",
		tool:   "grep",
		detail: "internal/cli",
		arg:    "flag.NewFlagSet",
	}, {
		kind:   rowToolResult,
		id:     "c2",
		tool:   "grep",
		status: tools.StatusOK,
		detail: "internal/cli/root.go:41: fs := flag.NewFlagSet",
	}}
	card := plainRender(t, m.renderRow(rows[1], 80, buildRowContext(rows)))
	for _, want := range []string{"Explored", "Search", "flag.NewFlagSet"} {
		if !strings.Contains(card, want) {
			t.Fatalf("exploration card missing %q:\n%s", want, card)
		}
	}
	if strings.Contains(card, "internal/cli/root.go:41") {
		t.Fatalf("exploration card must not dump raw grep output, got:\n%s", card)
	}
}

func TestErrorConfirmationKeepsBody(t *testing.T) {
	m := limeTestModel()
	// Even if the text looks like a confirmation verb, an error keeps its body.
	row := transcriptRow{
		kind:   rowToolResult,
		id:     "c3",
		tool:   "write_file",
		status: tools.StatusError,
		detail: "Created nothing — permission denied",
	}
	card := plainRender(t, m.renderRow(row, 80, buildRowContext(nil)))
	if !strings.Contains(card, "permission denied") {
		t.Fatalf("error body must be kept, got:\n%s", card)
	}
}

func TestConsecutiveExplorationResultsGroup(t *testing.T) {
	m := limeTestModel()
	m.headerPrinted = true
	m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowUser, text: "go"})
	m.transcript = appendTranscriptRow(m.transcript, transcriptRow{
		kind: rowToolResult, id: "a", tool: "grep", status: tools.StatusOK,
		detail: "internal/a.go:1: hit",
	})
	m.transcript = appendTranscriptRow(m.transcript, transcriptRow{
		kind: rowToolResult, id: "b", tool: "grep", status: tools.StatusOK,
		detail: "internal/b.go:2: hit",
	})

	body, _ := m.transcriptBody(96, "")
	got := plainRender(t, body)
	if strings.Count(got, "Explored") != 1 {
		t.Fatalf("adjacent exploration results should group into one card, got:\n%s", got)
	}
	if strings.Count(got, "Search") != 2 || !strings.Contains(got, "├ Search") || !strings.Contains(got, "└ Search") {
		t.Fatalf("grouped exploration card should show both searches, got:\n%s", got)
	}
	if strings.Contains(got, "details") {
		t.Fatalf("grouped exploration card must not advertise an inert details action, got:\n%s", got)
	}
}

// looksLikeRedundantConfirmation unit coverage (verb table + multiline guard).
func TestLooksLikeRedundantConfirmation(t *testing.T) {
	yes := []string{
		"Created examples/calc.go (45 bytes).",
		"Overwrote main.go (12 bytes).",
		"Successfully created directory /a/b/c",
		"Successfully wrote to notes.txt",
		"Deleted old.txt",
	}
	no := []string{
		"",
		"File: README.md\n1: # Zero", // multi-line read
		"internal/cli/root.go:41: fs := flag.NewSet", // grep hit
		"3 matches found",  // not a known verb
		"--- a/x\n+++ b/x", // diff
	}
	for _, s := range yes {
		if !looksLikeRedundantConfirmation(s) {
			t.Errorf("expected redundant confirmation: %q", s)
		}
	}
	for _, s := range no {
		if looksLikeRedundantConfirmation(s) {
			t.Errorf("expected NOT redundant: %q", s)
		}
	}
}
