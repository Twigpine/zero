package tui

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/planmode"
	"github.com/Gitlawb/zero/internal/tools"
)

func TestPlanEditorPreservesLiteralBackslashes(t *testing.T) {
	m := newPlanCommandTestModel(t, t.TempDir(), agent.PermissionModePlan)
	// Use actual editor bytes, not the formatter, to exercise the ambiguity:
	// two literal leading backslashes used to decode as one escaped backslash.
	initial := "<!-- zero-plan-format: 2 -->\n1. [pending] first\n"
	if _, err := planmode.WritePlan(m.cwd, m.activeSession.SessionID, initial); err != nil {
		t.Fatal(err)
	}
	staged, finish, err := planmode.StageEditor(m.cwd, m.activeSession.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer finish(false)
	edit := initial + "   " + `\\server\share` + "\n   " + `\src\file` + "\n   Notes: path\n   " + `\\notes\share` + "\n"
	if err := os.WriteFile(staged, []byte(edit), 0600); err != nil {
		t.Fatal(err)
	}
	msg := finishPlanEditor(m.cwd, m.activeSession.SessionID, staged, finish, nil)
	if msg.err != nil {
		t.Fatal(msg.err)
	}
	updated, _ := m.Update(msg)
	m = updated.(model)
	items, _, err := m.reloadPlanFromFile()
	if err != nil {
		t.Fatal(err)
	}
	want := "first\n" + `\\server\share` + "\n" + `\src\file`
	// Locate the edited item independently of header support so the old-code
	// regression fails on the lost backslash itself, not an extra header item.
	var edited tools.PlanItem
	for _, item := range items {
		if strings.HasPrefix(item.Content, "first") {
			edited = item
		}
	}
	if edited.Content != want || edited.Notes != "path\n"+`\\notes\share` {
		t.Fatalf("editor reload changed literal backslashes: content %q, notes %q", edited.Content, edited.Notes)
	}
	// Persist/reload again through the legacy serializer too: accepting new
	// editor text must not make older plan serialization lose its meaning.
	if _, err := planmode.WritePlan(m.cwd, m.activeSession.SessionID, formatPlanItems(items)); err != nil {
		t.Fatal(err)
	}
	again, _, err := m.reloadPlanFromFile()
	if err != nil || !planItemsEqual(items, again) {
		t.Fatalf("second editor round trip changed content: %#v, %v", again, err)
	}
}

func TestPlanFileFormatPreservesLegacyAndLiteralContinuations(t *testing.T) {
	items := []tools.PlanItem{{Content: "first\n" + `\\server\share` + "\n Notes: literal\n| literal\n\nend", Status: "pending", Notes: "note\n" + `\notes` + "\n| more"}}
	for _, format := range []func([]tools.PlanItem) string{formatPlanItems, formatPlanFile} {
		encoded := format(items)
		for _, file := range []string{encoded, strings.ReplaceAll(encoded, "\n", "\r\n")} {
			got := parsePlanFileLines(file)
			if !planItemsEqual(got, items) {
				t.Fatalf("plan file changed content: %#v, want %#v", got, items)
			}
		}
	}
	if got := parsePlanFileLines(formatPlanFile(nil)); len(got) != 0 {
		t.Fatalf("format marker became a plan step: %#v", got)
	}
}

func TestPlanEditorConvertsLegacyWithoutInventingAnEdit(t *testing.T) {
	m := newPlanCommandTestModel(t, t.TempDir(), agent.PermissionModePlan)
	items := []tools.PlanItem{{Content: "first\n" + `\\server\share`, Status: "pending"}}
	tool, _ := m.registry.Get("update_plan")
	tool.(planFileReloader).SetPlan(items)
	if _, err := planmode.WritePlan(m.cwd, m.activeSession.SessionID, formatPlanItems(items)); err != nil {
		t.Fatal(err)
	}
	staged, finish, err := stagePlanForEditor(context.Background(), m.cwd, m.activeSession.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer finish(false)
	data, err := os.ReadFile(staged)
	if err != nil || !strings.HasPrefix(string(data), planFileFormatMarker+"\n") {
		t.Fatalf("editor did not receive versioned content: %q, %v", data, err)
	}
	if !planItemsEqual(parsePlanFileLines(string(data)), items) {
		t.Fatalf("conversion changed legacy content: %q", data)
	}
	msg := finishPlanEditor(m.cwd, m.activeSession.SessionID, staged, finish, nil)
	if msg.err != nil || msg.outcome == nil || msg.outcome.Edited || msg.outcome.Reload {
		t.Fatalf("unchanged converted editor counted as an edit: %#v, %v", msg.outcome, msg.err)
	}
	count := len(m.sessionEvents)
	updated, _ := m.Update(msg)
	m = updated.(model)
	if len(m.sessionEvents) != count || !planItemsEqual(tool.(currentPlanReader).CurrentPlan(), items) {
		t.Fatal("conversion changed the accepted plan or invented an authored event")
	}
}
