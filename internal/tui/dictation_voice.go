package tui

import (
	tea "charm.land/bubbletea/v2"
)

const voiceCaptureUsage = "hold Ctrl+Space to record; press it to toggle where key releases are unavailable"

// Voice mode's Ctrl+Space gesture — the (only) dictation trigger. Two terminal
// tiers:
//
//  1. The terminal confirms key-release reporting (Ghostty/Kitty/WezTerm, …):
//     Ctrl+Space press starts recording and release stops it — true
//     hold-to-record, with ordinary Space left available for typing.
//  2. The terminal does not confirm release events: Ctrl+Space falls back to
//     press-to-toggle (press to start, press again to stop) — a deliberately
//     simpler, robust fallback than inferring release from key-repeat timing
//     (racy in a terminal). Legacy NUL input is decoded as Ctrl+Space by the
//     terminal input layer, so the same matcher covers both tiers.
//
// Only active while voice mode is on; the rest of dispatch is built on
// KeyPressMsg and is untouched.

// handleKeyboardEnhancements records the terminal's confirmed keyboard
// capabilities, so voice mode knows whether hold-to-record is available.
func (m model) handleKeyboardEnhancements(msg tea.KeyboardEnhancementsMsg) model {
	m.dictation.eventTypesKnown = true
	m.dictation.eventTypesSupported = msg.SupportsEventTypes()
	return m
}

func voiceCaptureKey(msg tea.KeyMsg) bool {
	mod := msg.Key().Mod &^ (tea.ModCapsLock | tea.ModNumLock)
	return keyIs(msg, tea.KeySpace) && mod == tea.ModCtrl
}

// voiceCaptureReleaseKey matches either half of the held Ctrl+Space chord. A
// terminal may report Ctrl releasing first, or report the later Space release
// without ModCtrl because Ctrl is no longer down.
func voiceCaptureReleaseKey(msg tea.KeyMsg) bool {
	return keyIs(msg, tea.KeySpace) || keyIs(msg, tea.KeyLeftCtrl) || keyIs(msg, tea.KeyRightCtrl)
}

func (m model) voiceCaptureStatus() string {
	switch m.dictation.phase {
	case dictStarting, dictRecording:
		if m.dictation.eventTypesSupported {
			return "release Ctrl+Space to stop"
		}
		return "press Ctrl+Space to stop"
	case dictTranscribing:
		return "transcribing…"
	default:
		if m.dictation.eventTypesSupported {
			return "hold Ctrl+Space to record"
		}
		return "press Ctrl+Space to record"
	}
}

// handleVoiceCapturePress handles Ctrl+Space while voice mode is on.
func (m model) handleVoiceCapturePress(msg tea.KeyMsg) (model, tea.Cmd) {
	if !m.dictation.eventTypesSupported {
		// Tier 2: no release events — press-to-toggle.
		return m.toggleDictation()
	}
	// Tier 1: hold-to-record. Ignore auto-repeat presses while the key is held.
	if msg.Key().IsRepeat {
		return m, nil
	}
	if m.dictation.phase == dictIdle {
		m.dictation.spaceHeld = true
		m.dictation.voiceStopPending = false
		return m.startDictation()
	}
	return m, nil // already recording; the release will stop it
}

// handleVoiceCaptureRelease stops a hold-to-record session when Ctrl+Space is released.
func (m model) handleVoiceCaptureRelease() (model, tea.Cmd) {
	if !m.dictation.spaceHeld {
		return m, nil
	}
	m.dictation.spaceHeld = false
	switch m.dictation.phase {
	case dictRecording:
		return m.stopDictation()
	case dictStarting:
		// Released before the recording finished starting; stop as soon as it does.
		m.dictation.voiceStopPending = true
		return m, nil
	}
	return m, nil
}
