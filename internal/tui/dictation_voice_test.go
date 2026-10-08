package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Gitlawb/zero/internal/config"
)

type fakeTUITranscriber struct{}

func (fakeTUITranscriber) Transcribe(context.Context, []byte) (string, error) { return "", nil }
func (fakeTUITranscriber) StreamTranscribe(context.Context, <-chan []byte, func(string, bool)) (string, error) {
	return "", nil
}

// batchOnlyController builds a controller whose recordings take the batch path
// (streaming disabled), so startDictation returns an unexecuted command instead
// of exec'ing a real capture tool.
func batchOnlyController() dictationController {
	streamOff := false
	return dictationController{
		cfg:      config.STTConfig{Streaming: &streamOff},
		platform: "linux",
		build: func(config.STTConfig, bool) (Transcriber, bool, error) {
			return fakeTUITranscriber{}, false, nil
		},
	}
}

func TestToggleVoiceModeFlips(t *testing.T) {
	m := model{dictation: batchOnlyController()}
	next, _ := m.toggleVoiceMode()
	if !next.dictation.voiceModeEnabled {
		t.Fatal("first /voice should enable voice mode")
	}
	if next.transientNotice.text != "Voice mode on — hold Ctrl+Space to record; press it to toggle where key releases are unavailable. Run /voice again to turn it off." {
		t.Errorf("voice-mode-on notice = %q", next.transientNotice.text)
	}
	if transcriptHasText(next, "Voice mode on") {
		t.Error("voice-mode-on confirmation should not be persisted in the transcript")
	}
	off, _ := next.toggleVoiceMode()
	if off.dictation.voiceModeEnabled {
		t.Error("second /voice should disable voice mode")
	}
}

func TestVoiceModeUnavailableWithoutFactory(t *testing.T) {
	m := model{} // no build factory
	next, _ := m.toggleVoiceMode()
	if next.dictation.voiceModeEnabled {
		t.Error("voice mode must not enable when dictation is unavailable")
	}
	if !transcriptHasText(next, "not configured") {
		t.Error("expected a not-configured hint")
	}
}

func TestKeyboardEnhancementsRecorded(t *testing.T) {
	m := model{dictation: batchOnlyController()}
	// Flags with the event-types bit unset → not supported.
	m = m.handleKeyboardEnhancements(tea.KeyboardEnhancementsMsg{Flags: 0})
	if !m.dictation.eventTypesKnown || m.dictation.eventTypesSupported {
		t.Error("Flags=0 should be known-but-unsupported")
	}
}

func TestVoiceCaptureKeyIgnoresLockModifiers(t *testing.T) {
	for _, mod := range []tea.KeyMod{
		tea.ModCtrl | tea.ModCapsLock,
		tea.ModCtrl | tea.ModNumLock,
		tea.ModCtrl | tea.ModCapsLock | tea.ModNumLock,
	} {
		if !voiceCaptureKey(tea.KeyPressMsg(tea.Key{Code: tea.KeySpace, Mod: mod})) {
			t.Fatalf("Ctrl+Space with lock modifiers %v was not recognized", mod)
		}
	}
}

func TestVoiceCaptureHoldStartsRecording(t *testing.T) {
	m := model{dictation: batchOnlyController()}
	m.dictation.voiceModeEnabled = true
	m.dictation.eventTypesSupported = true

	press := tea.KeyPressMsg(tea.Key{Code: tea.KeySpace, Mod: tea.ModCtrl})
	next, cmd := m.handleVoiceCapturePress(press)
	if next.dictation.phase != dictStarting {
		t.Fatalf("Ctrl+Space press should start recording (phase=%d)", next.dictation.phase)
	}
	if !next.dictation.spaceHeld {
		t.Error("spaceHeld should be set in hold mode")
	}
	if cmd == nil {
		t.Error("expected a start command")
	}
}

func TestVoiceCaptureHoldIgnoresRepeat(t *testing.T) {
	m := model{dictation: batchOnlyController()}
	m.dictation.voiceModeEnabled = true
	m.dictation.eventTypesSupported = true
	m.dictation.phase = dictRecording
	m.dictation.spaceHeld = true

	repeat := tea.KeyPressMsg(tea.Key{Code: tea.KeySpace, Mod: tea.ModCtrl, IsRepeat: true})
	next, _ := m.handleVoiceCapturePress(repeat)
	if next.dictation.phase != dictRecording {
		t.Error("auto-repeat while held must not restart or change phase")
	}
}

func TestVoiceCaptureReleaseStops(t *testing.T) {
	m := model{dictation: batchOnlyController()}
	m.dictation.voiceModeEnabled = true
	m.dictation.eventTypesSupported = true
	m.dictation.phase = dictRecording
	m.dictation.spaceHeld = true

	next, _ := m.handleVoiceCaptureRelease()
	if next.dictation.phase != dictTranscribing {
		t.Errorf("release should stop recording → transcribing (phase=%d)", next.dictation.phase)
	}
	if next.dictation.spaceHeld {
		t.Error("spaceHeld should clear on release")
	}
}

func TestVoiceReleaseDuringStartupDefersStop(t *testing.T) {
	m := model{dictation: batchOnlyController()}
	m.dictation.voiceModeEnabled = true
	m.dictation.eventTypesSupported = true
	m.dictation.phase = dictStarting
	m.dictation.spaceHeld = true

	next, _ := m.handleVoiceCaptureRelease()
	if !next.dictation.voiceStopPending {
		t.Error("a release during startup should defer the stop")
	}
	// When the recording finishes starting, the deferred stop fires.
	after, _ := next.handleDictationStarted(dictationStartedMsg{})
	if after.dictation.phase != dictTranscribing {
		t.Errorf("deferred stop should transition to transcribing (phase=%d)", after.dictation.phase)
	}
}

func TestVoiceCaptureToggleFallback(t *testing.T) {
	m := model{dictation: batchOnlyController()}
	m.dictation.voiceModeEnabled = true
	m.dictation.eventTypesSupported = false // no release events → toggle fallback

	press := tea.KeyPressMsg(tea.Key{Code: tea.KeySpace, Mod: tea.ModCtrl})
	next, _ := m.handleVoiceCapturePress(press)
	if next.dictation.phase != dictStarting {
		t.Errorf("toggle-mode Ctrl+Space should start recording (phase=%d)", next.dictation.phase)
	}
}

func TestVoiceModeCtrlSpaceReleaseStopsHoldRecording(t *testing.T) {
	m := newModel(t.Context(), Options{})
	m.dictation = batchOnlyController()
	m.dictation.voiceModeEnabled = true
	m.dictation.eventTypesSupported = true
	m.dictation.phase = dictRecording
	m.dictation.spaceHeld = true

	updated, _ := m.Update(tea.KeyReleaseMsg(tea.Key{Code: tea.KeySpace, Mod: tea.ModCtrl}))
	next := updated.(model)
	if next.dictation.phase != dictTranscribing || next.dictation.spaceHeld {
		t.Fatalf("Ctrl+Space release did not stop hold recording: phase=%d held=%v", next.dictation.phase, next.dictation.spaceHeld)
	}
}

func TestVoiceModeCaptureStopsWhenEitherChordKeyIsReleased(t *testing.T) {
	for _, test := range []struct {
		name string
		key  tea.Key
	}{
		{name: "left Ctrl first", key: tea.Key{Code: tea.KeyLeftCtrl}},
		{name: "right Ctrl first", key: tea.Key{Code: tea.KeyRightCtrl}},
		{name: "Space after Ctrl", key: tea.Key{Code: tea.KeySpace}},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := newModel(t.Context(), Options{})
			m.dictation = batchOnlyController()
			m.dictation.voiceModeEnabled = true
			m.dictation.eventTypesSupported = true
			m.dictation.phase = dictRecording
			m.dictation.spaceHeld = true

			updated, _ := m.Update(tea.KeyReleaseMsg(test.key))
			next := updated.(model)
			if next.dictation.phase != dictTranscribing || next.dictation.spaceHeld {
				t.Fatalf("release did not stop hold recording: phase=%d held=%v", next.dictation.phase, next.dictation.spaceHeld)
			}
		})
	}
}

func TestVoiceModeIndicatorDescribesTierAndPhase(t *testing.T) {
	for _, test := range []struct {
		name          string
		releaseEvents bool
		phase         dictationPhase
		spaceHeld     bool
		want          string
	}{
		{name: "toggle idle", want: "press Ctrl+Space to record"},
		{name: "hold idle", releaseEvents: true, want: "hold Ctrl+Space to record"},
		{name: "toggle recording", phase: dictRecording, want: "press Ctrl+Space to stop"},
		{name: "hold recording", releaseEvents: true, phase: dictRecording, spaceHeld: true, want: "release Ctrl+Space to stop"},
		{name: "transcribing", phase: dictTranscribing, want: "transcribing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := model{dictation: batchOnlyController()}
			m.dictation.voiceModeEnabled = true
			m.dictation.eventTypesSupported = test.releaseEvents
			m.dictation.phase = test.phase
			m.dictation.spaceHeld = test.spaceHeld
			if got := m.voiceModeIndicator(); !strings.Contains(strings.ToLower(got), strings.ToLower(test.want)) {
				t.Fatalf("voice indicator = %q, want %q", got, test.want)
			}
		})
	}
}

func TestDictationStatusChipUsesVoiceCaptureShortcut(t *testing.T) {
	for _, test := range []struct {
		name          string
		releaseEvents bool
		want          string
	}{
		{name: "toggle", want: "press Ctrl+Space to stop"},
		{name: "hold", releaseEvents: true, want: "release Ctrl+Space to stop"},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := model{dictation: batchOnlyController()}
			m.dictation.voiceModeEnabled = true
			m.dictation.eventTypesSupported = test.releaseEvents
			m.dictation.phase = dictRecording
			if got := m.dictationStatusChip(); !strings.Contains(got, test.want) {
				t.Fatalf("dictation status = %q, want %q", got, test.want)
			}
		})
	}
}

func TestVoiceCommandDescriptionExplainsBothTerminalTiers(t *testing.T) {
	for _, command := range commandDefinitions {
		if command.kind == commandVoice {
			for _, want := range []string{"hold Ctrl+Space", "press it to toggle", "key releases"} {
				if !strings.Contains(command.description, want) {
					t.Fatalf("voice command description = %q, want %q", command.description, want)
				}
			}
			return
		}
	}
	t.Fatal("voice command definition not found")
}

func TestVoiceModePlainSpaceTypesNormally(t *testing.T) {
	for _, releaseEvents := range []bool{false, true} {
		t.Run(map[bool]string{false: "toggle", true: "hold"}[releaseEvents], func(t *testing.T) {
			m := newModel(t.Context(), Options{})
			m.dictation = batchOnlyController()
			m.dictation.voiceModeEnabled = true
			m.dictation.eventTypesSupported = releaseEvents

			updated, cmd := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeySpace}))
			next := updated.(model)
			if got := next.composerValue(); got != " " {
				t.Fatalf("plain Space composer = %q, want one space", got)
			}
			if next.dictation.phase != dictIdle || cmd != nil {
				t.Fatalf("plain Space started dictation: phase=%d cmd=%v", next.dictation.phase, cmd != nil)
			}
		})
	}
}

func TestVoiceModeCtrlSpaceStartsAcrossTerminalTiers(t *testing.T) {
	for _, releaseEvents := range []bool{false, true} {
		t.Run(map[bool]string{false: "toggle", true: "hold"}[releaseEvents], func(t *testing.T) {
			m := newModel(t.Context(), Options{})
			m.dictation = batchOnlyController()
			m.dictation.voiceModeEnabled = true
			m.dictation.eventTypesSupported = releaseEvents

			updated, cmd := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeySpace, Mod: tea.ModCtrl}))
			next := updated.(model)
			if next.dictation.phase != dictStarting || cmd == nil {
				t.Fatalf("Ctrl+Space did not start dictation: phase=%d cmd=%v", next.dictation.phase, cmd != nil)
			}
			if got := next.composerValue(); got != "" {
				t.Fatalf("Ctrl+Space typed into composer: %q", got)
			}
		})
	}
}

func TestVoiceModeTypingPreservesSpacesAcrossTerminalTiers(t *testing.T) {
	for _, releaseEvents := range []bool{false, true} {
		t.Run(map[bool]string{false: "toggle", true: "hold"}[releaseEvents], func(t *testing.T) {
			m := newModel(t.Context(), Options{})
			m.dictation = batchOnlyController()
			m.dictation.voiceModeEnabled = true
			m.dictation.eventTypesSupported = releaseEvents
			for _, key := range []tea.Key{
				{Code: 'f', Text: "f"},
				{Code: 'i', Text: "i"},
				{Code: 'x', Text: "x"},
				{Code: tea.KeySpace},
				{Code: 't', Text: "t"},
				{Code: 'h', Text: "h"},
				{Code: 'e', Text: "e"},
			} {
				updated, _ := m.Update(tea.KeyPressMsg(key))
				m = updated.(model)
			}
			if got := m.composerValue(); got != "fix the" {
				t.Fatalf("composer = %q, want %q", got, "fix the")
			}
			if m.dictation.phase != dictIdle {
				t.Fatalf("typing started dictation: phase=%d", m.dictation.phase)
			}
		})
	}
}
