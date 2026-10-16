package tui

import (
	"context"

	tea "charm.land/bubbletea/v2"

	"github.com/Gitlawb/zero/internal/dictation"
)

// sttPartialMsg carries an incremental streaming transcript into the TUI. It is
// injected from the streaming goroutine via runtimeMessageSink — the same
// mechanism agent text deltas use (§6). text is the cumulative best transcript
// so far (not a delta); final marks a settled segment.
type sttPartialMsg struct {
	sessionID int64
	text      string
	final     bool
}

// startStreamingDictation begins continuous capture and launches the streaming
// transcription command. StartStreaming spawns the capture process
// synchronously, so a failure here is immediate; on success we are already
// recording (no separate "started" round-trip).
func (m model) startStreamingDictation() (model, tea.Cmd) {
	chunks, stop, err := m.dictation.recorder.StartStreaming()
	if err != nil {
		m.dictation.reset()
		return m.appendSystemNotice(dictationErrorText(err)), nil
	}
	m.dictation.streamStop = stop
	m.dictation.phase = dictRecording

	sink := m.runtimeMessageSink
	ctx := m.dictation.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	transcriber := m.dictation.transcriber
	submit := m.dictation.cfg.AutoSubmitEnabled()
	sessionID := m.dictation.sessionID

	// Tap the audio: compute a mic level per chunk for the live waveform, then
	// forward the chunk to the transcriber. A small buffer keeps the tap from
	// stalling capture if the transcriber briefly lags.
	tapped := make(chan []byte, 16)
	go func() {
		defer close(tapped)
		for chunk := range chunks {
			if sink != nil {
				sink(sttLevelMsg{level: dictation.ChunkLevel(chunk)})
			}
			// Also select on ctx.Done so this goroutine can't block forever on the
			// send if StreamTranscribe exits early (e.g. a sherpa startup failure) and
			// stops draining `tapped` — the session's cancel unblocks and drains it.
			select {
			case tapped <- chunk:
			case <-ctx.Done():
				return
			}
		}
	}()

	streamCmd := func() tea.Msg {
		onPartial := func(text string, final bool) {
			if sink != nil {
				sink(sttPartialMsg{sessionID: sessionID, text: text, final: final})
			}
		}
		text, err := transcriber.StreamTranscribe(ctx, tapped, onPartial)
		return dictationTranscribedMsg{sessionID: sessionID, text: text, err: err, submit: submit, streaming: true}
	}
	// Streaming drives the waveform from real levels (no synthetic tick needed).
	return m, streamCmd
}

// handleDictationPartial renders a cumulative partial transcript into the
// composer, replacing the previously-rendered live region wholesale so the text
// builds up in place as the user keeps talking.
func (m model) handleDictationPartial(msg sttPartialMsg) model {
	if msg.sessionID != m.dictation.sessionID {
		return m
	}
	// Ignore stragglers that arrive after the session ended (cancel/final).
	if m.dictation.phase != dictRecording && m.dictation.phase != dictTranscribing {
		return m
	}
	m.applyStreamingText(msg.text)
	return m
}

func (m *model) applyStreamingText(text string) {
	state := m.currentComposerState()
	stateRunes := []rune(state.text)
	if !m.dictation.regionActive {
		m.dictation.regionActive = true
		m.dictation.regionStart = state.cursor
		m.dictation.regionEnd = state.cursor
		m.dictation.regionPrefix = ""
		// Anchor the prefix text before the live region. A later partial removes
		// the tracked range only while both this prefix and the rendered region
		// still match.
		m.dictation.regionAnchor = string(stateRunes[:state.cursor])
		if needsLeadingSpace(state) {
			// Fold the separator into the region so a cancel removes it too.
			m.dictation.regionPrefix = " "
		}
	} else if !m.dictation.liveRegionMatches(stateRunes) {
		// The composer changed before or inside the tracked range. Its old
		// bounds no longer prove which bytes belong to dictation, so preserve
		// everything and insert the next partial as a fresh live region.
		m.dictation.regionStart = state.cursor
		m.dictation.regionEnd = state.cursor
		m.dictation.regionPrefix = ""
		m.dictation.regionAnchor = string(stateRunes[:state.cursor])
		if needsLeadingSpace(state) {
			m.dictation.regionPrefix = " "
		}
	}
	// Replace [regionStart, regionEnd) with prefix + the new cumulative text.
	rendered := m.dictation.regionPrefix + text
	cleared := deleteComposerRange(state, m.dictation.regionStart, m.dictation.regionEnd)
	// Preserve the user's cursor if they moved it outside the live region
	// (e.g. to edit elsewhere in the composer) — yanking it to regionStart on
	// every partial would steal their focus mid-edit.
	if cleared.cursor < m.dictation.regionStart || cleared.cursor > m.dictation.regionEnd {
		// keep cleared.cursor
	} else {
		cleared.cursor = m.dictation.regionStart
	}
	updated := insertComposerText(cleared, rendered)
	m.dictation.regionEnd = m.dictation.regionStart + len([]rune(rendered))
	m.dictation.regionRendered = rendered
	m.setComposerState(updated)
}

func (d dictationController) liveRegionMatches(stateRunes []rune) bool {
	return d.regionStart >= 0 && d.regionEnd >= d.regionStart && d.regionEnd <= len(stateRunes) &&
		string(stateRunes[:d.regionStart]) == d.regionAnchor &&
		string(stateRunes[d.regionStart:d.regionEnd]) == d.regionRendered
}

// commitDictationRegion keeps the streamed text in the composer and stops
// tracking it as a live region (used on successful completion — the final
// transcript equals the last partial already rendered).
func (m model) commitDictationRegion() model {
	m.dictation.regionActive = false
	m.dictation.regionRendered = ""
	return m
}

// discardDictationRegion removes the live streamed text from the composer (used
// on cancel — the user aborted, so the half-formed transcript is dropped).
func (m model) discardDictationRegion() model {
	if m.dictation.regionActive {
		state := m.currentComposerState()
		if m.dictation.liveRegionMatches([]rune(state.text)) {
			m.setComposerState(deleteComposerRange(state, m.dictation.regionStart, m.dictation.regionEnd))
		}
		m.dictation.regionActive = false
		m.dictation.regionRendered = ""
	}
	return m
}
