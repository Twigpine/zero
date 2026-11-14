package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/Twigpine/zero/internal/agentsessions"
)

func TestTabbedResumeMouseMatchesFirstLastAndDetailRows(t *testing.T) {
	for _, details := range []bool{false, true} {
		m := mouseTestModel()
		m.width = 120
		items := []pickerItem{sessionRow("one", "zero"), sessionRow("two", "claude-code"), sessionRow("three", "codex")}
		if details {
			for i := range items {
				items[i].Detail = "metadata"
			}
		}
		m.picker = tabbedPicker(items...)
		width := chatWidth(m.width)
		overlay := m.pickerOverlay(width)
		rect := m.overlayMouseRect(len(viewLines(overlay)), width)
		step := 1
		if details {
			step = 2
		}
		for _, index := range []int{0, 2} {
			for offset := 0; offset < step; offset++ {
				target, ok := m.selectGenericPickerAtMouse(testMouseClick(tea.MouseLeft, width/2, rect.y+4+index*step+offset))
				if !ok || target.Index != index {
					t.Fatalf("details=%v index=%d offset=%d selected=%+v ok=%v", details, index, offset, target, ok)
				}
			}
		}
		if _, ok := m.selectGenericPickerAtMouse(testMouseClick(tea.MouseLeft, width/2, rect.y+3)); ok {
			t.Error("separator selected a row")
		}
	}
}

func TestForeignImportInvalidatesPendingAndInstalledPickers(t *testing.T) {
	for _, typed := range []bool{false, true} {
		m, _ := twoResumableSessions(t)
		m, dispatch := m.sessionPickerCmd()
		snapshot := dispatch().(sessionPickerLoadedMsg)
		m.picker = tabbedPicker(sessionRow("foreign", "claude-code"))
		if typed {
			m, _, _ = m.startResumeCommand("claude-code:foreign")
		} else {
			m, _, _ = m.startForeignSessionImport(agentsessions.ForeignSession{Agent: "claude-code", ID: "foreign"})
		}
		if m.picker != nil || m.sessionPickerResultIsCurrent(snapshot) {
			t.Fatal("import retained a pre-import picker")
		}
		m.sessionImportInFlight = false
		if m.sessionPickerResultIsCurrent(snapshot) {
			t.Fatal("old discovery became valid after import ended")
		}
		// A newer picker installed during completion is also invalidated, even
		// when a run has started and prevents the import from switching sessions.
		m.picker = tabbedPicker(sessionRow("foreign", "claude-code"))
		m.pending = true
		m, _ = m.finishForeignSessionImport(foreignSessionImportedMsg{originSession: m.activeSession.SessionID})
		if m.picker != nil {
			t.Fatal("successful import retained an already installed foreign picker")
		}
	}
}
