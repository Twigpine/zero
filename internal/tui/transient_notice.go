package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbletea/v2"
)

func (m model) modelAppliedNotice() string {
	return "Model: " + displayValue(m.modelName, "none") + " · " + displayValue(m.providerName, "default provider") + " · effort " + m.effortDisplay()
}

// modelAppliedNoticeFor renders the applied-notice for a committed model
// switch, qualifying it when the switch was not durably saved. Both entry
// points — typed /model and the picker — show this notice INSTEAD of the
// handler status text that carries the same qualifier, so a persistence
// failure is invisible on either path unless it is rendered here.
func (m model) modelAppliedNoticeFor(persistErr error) (string, transientNoticeTone) {
	if persistErr != nil {
		return m.modelAppliedNotice() + " · not saved (" + persistErr.Error() + ")", transientNoticeWarning
	}
	return m.modelAppliedNotice(), transientNoticeSuccess
}

func (m model) effortAppliedNotice() string {
	return "Reasoning effort: " + m.effortDisplay()
}

func (m model) fastAppliedNotice() string {
	if m.activeServiceTier() == "priority" {
		return "Fast mode: on"
	}
	return "Fast mode: off"
}

func (m model) selfCorrectAppliedNotice() string {
	if m.selfCorrectTests {
		return "Self-correction: on"
	}
	return "Self-correction: off"
}

func (m model) turnsAppliedNotice() string {
	return fmt.Sprintf("Turn budget: %d", m.agentOptions.MaxTurns)
}

func (m model) profileAppliedNotice() string {
	return "Profile: " + displayValue(m.execProfileName, "balanced")
}

// transientNoticeDuration is deliberately long enough to read at a glance but
// short enough that routine command confirmations do not become transcript
// clutter. Details, actions, and diagnostics remain normal transcript rows.
const transientNoticeDuration = 4 * time.Second

type transientNoticeTone uint8

const (
	transientNoticeInfo transientNoticeTone = iota
	transientNoticeSuccess
	transientNoticeWarning
)

type transientNotice struct {
	text string
	tone transientNoticeTone
}

// transientNoticeExpiredMsg is sequence-gated so an older timer can never
// clear a newer notice that replaced it.
type transientNoticeExpiredMsg struct {
	seq int
}

// showTransientNotice presents one brief, replaceable confirmation above the
// composer. It intentionally does not append a transcript row or session event:
// callers use it only for short, non-actionable outcomes.
func (m model) showTransientNotice(text string, tone transientNoticeTone) (model, tea.Cmd) {
	m, shown := m.setTransientNotice(text, tone)
	if !shown {
		return m, nil
	}
	seq := m.transientNoticeSeq
	m.transientNoticeTimerSeq = seq
	return m, transientNoticeExpiryCmd(seq)
}

// showTransientNoticeInline is for handlers that cannot return a tea.Cmd. The
// outer Update path notices the new sequence and schedules its one-shot expiry.
func (m model) showTransientNoticeInline(text string, tone transientNoticeTone) model {
	m, _ = m.setTransientNotice(text, tone)
	return m
}

func transientNoticeExpiryCmd(seq int) tea.Cmd {
	return tea.Tick(transientNoticeDuration, func(time.Time) tea.Msg {
		return transientNoticeExpiredMsg{seq: seq}
	})
}

func (m model) ensureTransientNoticeTimer() (model, tea.Cmd) {
	if m.transientNotice.text == "" || m.transientNoticeTimerSeq == m.transientNoticeSeq {
		return m, nil
	}
	m.transientNoticeTimerSeq = m.transientNoticeSeq
	return m, transientNoticeExpiryCmd(m.transientNoticeSeq)
}

func (m model) setTransientNotice(text string, tone transientNoticeTone) (model, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return m, false
	}
	m.transientNotice = transientNotice{
		text: text,
		tone: tone,
	}
	m.transientNoticeSeq++
	return m, true
}

func (m model) transientNoticeLine(width int) string {
	notice := m.transientNotice
	if strings.TrimSpace(notice.text) == "" {
		return ""
	}
	// Keep confirmations as a quiet, text-only footer flash. A status dot makes
	// routine outcomes read like a permanent run-state badge and competes with
	// the composer status line below.
	style := zeroTheme.muted
	switch notice.tone {
	case transientNoticeSuccess:
		style = zeroTheme.ink
	case transientNoticeWarning:
		style = zeroTheme.amber
	}
	return fitStyledLine("  "+style.Render(notice.text), width)
}
