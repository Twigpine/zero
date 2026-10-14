package tui

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/dictation"
)

// Transcriber and Recorder are re-exported so Options and the model can refer to
// the dictation interfaces without importing the package everywhere.
type (
	Transcriber = dictation.Transcriber
	Recorder    = dictation.Recorder
)

// dictationPhase is the recording state machine (§10): idle → recording →
// transcribing → idle. Voice mode (§13.9) changes which key enters it, not the
// machine itself.
type dictationPhase int

const (
	dictIdle dictationPhase = iota
	dictStarting
	dictRecording
	dictTranscribing
)

// dictationController holds the live dictation session. It is a value field on
// the model (threaded through Update by value); the recorder/cancel it captures
// are pointers, so they survive across frames and are shared with the async
// commands that operate on them.
type dictationController struct {
	cfg            config.STTConfig
	build          func(cfg config.STTConfig, preferStreaming bool) (Transcriber, bool, error)
	shutdownServer func(context.Context) error
	keyAvailable   func(provider string) bool
	saveKey        func(provider, key string) error
	phase          dictationPhase
	platform       dictation.Platform

	// downloadRoot is where the auto-download stores the engine + model ("" =
	// auto-download disabled). userConfigPath is where the resolved paths persist.
	downloadRoot   string
	userConfigPath string
	// downloading: a model download is in flight (started from the /stt-model
	// download chooser). downloadStatus is the live progress line shown in the
	// status bar while downloading. browseLoading: the full model list is being
	// fetched; browseVariants caches it once fetched.
	downloading    bool
	downloadStatus string
	browseLoading  bool
	browseVariants []dictation.ModelVariant

	// in-flight session state
	sessionID   int64
	streaming   bool
	recorder    Recorder
	transcriber Transcriber
	ctx         context.Context
	cancel      context.CancelFunc
	streamStop  func() error

	// live streaming render region: [regionStart, regionEnd) in the composer's
	// rune space, replaced wholesale on each partial (§13.8). regionPrefix is a
	// separator space folded into the region so a cancel restores the original
	// text exactly.
	regionActive bool
	regionStart  int
	regionEnd    int
	regionPrefix string
	// regionRendered is the exact text last inserted into [regionStart,
	// regionEnd). A later partial or cancel may remove that range only while it
	// still matches, so stale bounds never consume user replacement text.
	regionRendered string
	// regionAnchor snapshots the text BEFORE the live region, so the next
	// partial can detect edits before the tracked range.
	regionAnchor string

	// waveBars is the recording waveform's recent bar heights (a scrolling ring):
	// audio-reactive from live mic levels on the streaming path, synthetic on the
	// batch path. waveTick drives the batch synthetic animation.
	waveBars []int
	waveTick int

	// voiceModeEnabled repurposes Ctrl+Space into hold-to-record (§13.9).
	voiceModeEnabled bool
	// eventTypesSupported records whether the terminal confirmed key-release
	// reporting (Kitty protocol); eventTypesKnown gates it until the terminal has
	// answered. Determines hold-to-record vs. press-to-toggle for voice mode.
	eventTypesKnown     bool
	eventTypesSupported bool
	// spaceHeld tracks a held Space in hold-to-record mode; voiceStopPending
	// remembers a release that arrived before the recording finished starting.
	spaceHeld        bool
	voiceStopPending bool
}

// dictationStartedMsg reports the outcome of Recorder.Start().
// sessionID must match the dictation controller's current session so a late
// start result after cancel/reset cannot reset a newer recording.
type dictationStartedMsg struct {
	sessionID int64
	err       error
}

// dictationTranscribedMsg carries the final transcript of a batch (or a
// completed streaming) recording.
type dictationTranscribedMsg struct {
	sessionID int64
	text      string
	err       error
	submit    bool
	streaming bool
}

// newDictationController builds the controller from resolved options.
func newDictationController(opts Options) dictationController {
	return dictationController{
		cfg:            opts.STT,
		build:          opts.BuildDictationTranscriber,
		shutdownServer: opts.ShutdownDictationServer,
		keyAvailable:   opts.STTKeyStatus,
		saveKey:        opts.SaveSTTKey,
		platform:       dictation.DetectPlatform(),
		downloadRoot:   opts.STTDownloadRoot,
		userConfigPath: opts.UserConfigPath,
		sessionID:      1,
	}
}

// available reports whether dictation is wired up (a transcriber factory exists).
func (d dictationController) available() bool { return d.build != nil }

// currentModelLabel names the STT model currently configured, for status hints.
func (d dictationController) currentModelLabel() string {
	cfg := d.cfg
	switch cfg.STTProvider() {
	case config.STTProviderLocal:
		if strings.TrimSpace(cfg.LocalModelPath) == "" {
			return "Local (no model set)"
		}
		// "Local" prefix mirrors the "Groq …"/"OpenAI …" cloud labels so the status
		// line always names the backend, not just the model.
		// Prefer a curated variant's friendly name when the path matches one.
		for _, v := range dictation.ModelVariants() {
			if v.DirName != "" && strings.Contains(cfg.LocalModelPath, v.DirName) {
				return "Local · " + v.Label
			}
		}
		return "Local · " + strings.TrimPrefix(filepath.Base(cfg.LocalModelPath), "sherpa-onnx-")
	case config.STTProviderGroq:
		return "Groq " + firstNonEmptyStr(cfg.Model, "whisper-large-v3-turbo")
	case config.STTProviderOpenAI:
		return "OpenAI " + firstNonEmptyStr(cfg.Model, "whisper-1")
	}
	return string(cfg.STTProvider())
}

func firstNonEmptyStr(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// active reports whether a recording/transcription is in flight.
func (d dictationController) active() bool { return d.phase != dictIdle }

// toggleDictation starts a recording when idle and stops-and-transcribes when
// recording. Invoked by the voice-mode Ctrl+Space gesture; a call during
// startup/transcription is ignored (the machine is mid-transition).
func (m model) toggleDictation() (model, tea.Cmd) {
	if !m.dictation.available() {
		return m.appendSystemNotice("Dictation is not configured. See docs/dictation.md to set up a local engine or a Groq/OpenAI key."), nil
	}
	if m.dictation.downloading {
		return m.appendDictationNotice("downloading", "Still setting up the dictation engine — one moment."), nil
	}
	switch m.dictation.phase {
	case dictIdle:
		return m.startDictation()
	case dictRecording:
		return m.stopDictation()
	default:
		// dictStarting / dictTranscribing: mid-transition, ignore.
		return m, nil
	}
}

// toggleVoiceMode flips the /voice hold-to-record gesture. While on,
// Ctrl+Space records and ordinary Space continues to type normally.
func (m model) toggleVoiceMode() (model, tea.Cmd) {
	if !m.dictation.available() {
		return m.appendSystemNotice("Dictation is not configured. See docs/dictation.md to set up a local engine or a Groq/OpenAI key."), nil
	}
	m.dictation.voiceModeEnabled = !m.dictation.voiceModeEnabled
	if m.dictation.voiceModeEnabled {
		return m.showTransientNoticeInline("Voice mode on — "+voiceCaptureUsage+". Run /voice again to turn it off.", transientNoticeSuccess), nil
	}
	// Turning voice off is the "done dictating" signal, so release the warm sherpa
	// streaming server — otherwise a loaded model keeps idling in RAM (and holding
	// its port) until the app exits. It respawns lazily on the next streaming
	// recording. Skip while a recording is still in flight so we don't kill it.
	next := m.showTransientNoticeInline("Voice mode off.", transientNoticeInfo)
	if next.dictation.active() {
		return next, nil
	}
	return next, next.releaseDictationServerCmd()
}

// releaseDictationServerCmd tears down the warm sherpa-onnx streaming server off
// the UI goroutine (Shutdown waits on the process). No-op when no server backend
// is wired or none is running.
func (m model) releaseDictationServerCmd() tea.Cmd {
	shutdown := m.dictation.shutdownServer
	if shutdown == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(ctx)
		return nil
	}
}

// startDictation builds the recorder + transcriber and kicks off capture.
func (m model) startDictation() (model, tea.Cmd) {
	transcriber, streaming, err := m.dictation.build(m.dictation.cfg, m.dictation.wantStreaming())
	if err != nil {
		// A missing local engine on a platform we can auto-download for → point at
		// /stt-model, which offers the download-a-model chooser (a robust single
		// selection rather than a press-again-to-confirm dance).
		if m.dictation.canOfferDownload(err) {
			return m.appendDictationNotice("download-offer",
				"No local dictation model yet. Run /stt-model to choose and download one — or set stt.localModelPath yourself, or pick a cloud provider."), nil
		}
		return m.appendDictationNotice("setup:"+err.Error(), dictationErrorText(err)), nil
	}

	rec := dictation.NewRecorder(dictation.RecorderOptions{
		Platform:           m.dictation.platform,
		SampleRate:         dictation.RequiredSampleRate(transcriber),
		MaxDuration:        m.dictation.maxDuration(),
		SilenceAutoStop:    m.dictation.cfg.SilenceAutoStopEnabled(),
		WindowsAudioDevice: m.dictation.cfg.WindowsAudioDevice,
	})

	base := m.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)

	m.dictation.recorder = rec
	m.dictation.transcriber = transcriber
	m.dictation.ctx = ctx
	m.dictation.cancel = cancel
	m.dictation.streaming = streaming
	m.dictation.phase = dictStarting

	if streaming {
		return m.startStreamingDictation()
	}
	return m, startBatchRecordingCmd(m.dictation.sessionID, rec)
}

// stopDictation ends a recording. For batch it triggers Stop()+Transcribe(); for
// streaming it stops the recorder, which closes the chunk channel and lets the
// in-flight StreamTranscribe return its final text.
func (m model) stopDictation() (model, tea.Cmd) {
	m.dictation.phase = dictTranscribing
	if m.dictation.streaming {
		if m.dictation.streamStop != nil {
			_ = m.dictation.streamStop()
		}
		return m, nil // final text arrives via the streaming command already running
	}
	return m, transcribeBatchCmd(m.dictation.sessionID, m.dictation.ctx, m.dictation.recorder, m.dictation.transcriber, m.dictation.cfg.AutoSubmitEnabled())
}

// cancelDictation aborts an in-flight recording without transcribing. Bound to
// Esc while a session is active.
func (m model) cancelDictation() (model, tea.Cmd) {
	if m.dictation.cancel != nil {
		m.dictation.cancel()
	}
	if m.dictation.streaming && m.dictation.streamStop != nil {
		_ = m.dictation.streamStop()
	} else if m.dictation.recorder != nil && m.dictation.phase != dictTranscribing {
		// Best-effort: stop the capture tool so the microphone is released.
		go func(r Recorder) { _, _ = r.Stop() }(m.dictation.recorder)
	}
	m = m.discardDictationRegion()
	m.dictation.reset()
	return m.showTransientNoticeInline("Dictation cancelled.", transientNoticeInfo), nil
}

// wantStreaming decides whether this recording uses the streaming pipeline:
// enabled in config and supported on this platform (desktop only — Termux's mic
// tool has no stdout mode).
func (d dictationController) wantStreaming() bool {
	return d.cfg.StreamingEnabled() && d.platform.StreamingSupported()
}

func (d dictationController) maxDuration() time.Duration {
	if d.cfg.MaxDurationSeconds > 0 {
		return time.Duration(d.cfg.MaxDurationSeconds) * time.Second
	}
	return dictation.DefaultMaxDuration
}

func (d *dictationController) reset() {
	d.sessionID++
	d.phase = dictIdle
	d.recorder = nil
	d.transcriber = nil
	d.ctx = nil
	d.cancel = nil
	d.streaming = false
	d.streamStop = nil
	d.regionActive = false
	d.regionPrefix = ""
	d.regionRendered = ""
	d.waveBars = nil
	d.waveTick = 0
}

// startBatchRecordingCmd starts the record-to-file capture off the UI goroutine
// (Start spawns a subprocess and may briefly block or fail on a missing tool).
func startBatchRecordingCmd(sessionID int64, rec Recorder) tea.Cmd {
	return func() tea.Msg {
		return dictationStartedMsg{sessionID: sessionID, err: rec.Start()}
	}
}

// transcribeBatchCmd stops the recording, transcribes the audio, and reports the
// final text. Runs off the UI goroutine — Stop waits on the capture tool and
// Transcribe does a network round-trip or a local exec.
func transcribeBatchCmd(sessionID int64, ctx context.Context, rec Recorder, transcriber Transcriber, submit bool) tea.Cmd {
	if ctx == nil {
		ctx = context.Background()
	}
	return func() tea.Msg {
		audio, err := rec.Stop()
		if err != nil {
			return dictationTranscribedMsg{sessionID: sessionID, err: err}
		}
		text, err := transcriber.Transcribe(ctx, audio)
		return dictationTranscribedMsg{sessionID: sessionID, text: text, err: err, submit: submit}
	}
}

// handleDictationStarted transitions to recording (or reports a start failure).
func (m model) handleDictationStarted(msg dictationStartedMsg) (model, tea.Cmd) {
	if msg.sessionID != m.dictation.sessionID {
		// Stale startup from a cancelled session must not reset or re-arm a
		// newer recording that already owns the controller.
		return m, nil
	}
	if msg.err != nil {
		m = m.discardDictationRegion()
		m.dictation.reset()
		text := dictationErrorText(msg.err)
		return m.appendDictationNotice("dicterr:"+text, text), nil
	}
	// A stop or cancel may have already advanced the phase; only arm recording if
	// we are still starting.
	if m.dictation.phase == dictStarting {
		m.dictation.phase = dictRecording
	}
	// A voice-mode Ctrl+Space release that arrived mid-startup asked us to stop as soon
	// as recording began.
	if m.dictation.voiceStopPending && m.dictation.phase == dictRecording {
		m.dictation.voiceStopPending = false
		return m.stopDictation()
	}
	// Kick off the recording waveform animation.
	return m, recTickCmd()
}

// handleDictationTranscribed inserts the final transcript into the composer (or
// submits it when stt.autoSubmit is on), then returns to idle.
func (m model) handleDictationTranscribed(msg dictationTranscribedMsg) (tea.Model, tea.Cmd) {
	if msg.sessionID != m.dictation.sessionID {
		return m, nil
	}
	streaming := m.dictation.streaming || msg.streaming
	m = m.commitDictationRegion()
	// A streaming session can end via a transcriber error (not just a user stop), so
	// tear the capture down here BEFORE reset() drops the handles: stop the recorder
	// (kills the mic subprocess) and cancel the context (unblocks the audio tap).
	// Without this, an error path leaves the mic running and the next attempt hits
	// "already recording". Both are idempotent, so this is safe on the happy path.
	if streaming {
		if m.dictation.streamStop != nil {
			_ = m.dictation.streamStop()
		}
		if m.dictation.cancel != nil {
			m.dictation.cancel()
		}
	}
	m.dictation.reset()

	if errors.Is(msg.err, context.Canceled) {
		// cancelDictation already discarded the live region, reset the
		// session, and posted "Dictation cancelled." A streaming transcriber
		// can still race a buffered event past that cancel and report back a
		// nonempty compose()+context.Canceled; treat that as terminal here,
		// before the auto-submit branch below, so Esc can never fall through
		// to msg.submit and fire the composer's restored pre-existing text.
		return m, nil
	}

	if msg.err != nil {
		// A cloud auth failure (missing/invalid key) is fixable in place: reopen the
		// API-key prompt for the current provider so the user can paste a key and
		// retry, instead of hitting a dead-end "run zero auth" line.
		if next, handled := m.maybeOfferKeyOnAuthError(msg.err); handled {
			return next, nil
		}
		// Partial text may still be worth keeping on a mid-stream failure (§6).
		if strings.TrimSpace(msg.text) != "" && !streaming {
			m = m.insertDictatedText(msg.text)
			return m.appendSystemNotice("Dictation interrupted; kept the partial transcript. " + dictationErrorText(msg.err)), nil
		}
		// Keyed so hammering the dictation gesture against the same unusable model doesn't spam the
		// identical error line over and over.
		text := dictationErrorText(msg.err)
		return m.appendDictationNotice("dicterr:"+text, text), nil
	}
	if strings.TrimSpace(msg.text) == "" {
		if streaming {
			return m, nil // streaming already rendered live; nothing to add
		}
		return m.showTransientNoticeInline("No speech detected.", transientNoticeInfo), nil
	}
	if !streaming {
		m = m.insertDictatedText(msg.text)
	}
	if msg.submit {
		// stt.autoSubmit fires the transcript straight at the agent (off by
		// default — insert-for-review is the safety net for a misheard prompt).
		return m.handleSubmit()
	}
	return m, nil
}

// insertDictatedText inserts transcribed text at the composer cursor, adding a
// separating space when the existing text would otherwise run into it.
func (m model) insertDictatedText(text string) model {
	text = strings.TrimSpace(text)
	if text == "" {
		return m
	}
	state := m.currentComposerState()
	if needsLeadingSpace(state) {
		text = " " + text
	}
	m.setComposerState(insertComposerText(state, text))
	return m
}

func needsLeadingSpace(state composerState) bool {
	runes := []rune(state.text)
	if state.cursor <= 0 || state.cursor > len(runes) {
		return false
	}
	prev := runes[state.cursor-1]
	return prev != ' ' && prev != '\n' && prev != '\t'
}

// dictationStatusChip renders the active-recording indicator for the status
// line: a live animated waveform plus how to stop. Empty when idle.
func (m model) dictationStatusChip() string {
	switch m.dictation.phase {
	case dictStarting, dictRecording:
		wave := zeroTheme.amber.Render("● " + renderWaveBars(m.dictation.waveBars) + " REC")
		return wave + zeroTheme.muted.Render(" · "+m.voiceCaptureStatus()+", Esc to cancel")
	case dictTranscribing:
		return zeroTheme.accent.Render("●") + " " + zeroTheme.muted.Render("transcribing…")
	}
	return ""
}

const (
	waveBarCount    = 14
	recTickInterval = 90 * time.Millisecond
)

// waveRunes maps a 0..8 bar height to a block glyph.
var waveRunes = []rune(" ▁▂▃▄▅▆▇█")

// renderWaveBars draws the scrolling bar heights as block glyphs.
func renderWaveBars(bars []int) string {
	if len(bars) == 0 {
		return strings.Repeat(" ", waveBarCount)
	}
	out := make([]rune, len(bars))
	for i, h := range bars {
		if h < 0 {
			h = 0
		}
		if h > 8 {
			h = 8
		}
		out[i] = waveRunes[h]
	}
	return string(out)
}

// pushLevel appends a new bar height on the right and scrolls the ring left.
func (d *dictationController) pushLevel(height int) {
	nb := make([]int, waveBarCount)
	if len(d.waveBars) == waveBarCount {
		copy(nb, d.waveBars[1:])
	}
	nb[waveBarCount-1] = height
	d.waveBars = nb
}

// levelHeight maps a 0..1 audio level to a 0..8 bar height.
func levelHeight(level float64) int {
	h := int(level*8 + 0.5)
	if h < 0 {
		h = 0
	}
	if h > 8 {
		h = 8
	}
	return h
}

// sttLevelMsg carries a live microphone level (0..1) from the streaming audio
// tap, driving the real audio-reactive waveform.
type sttLevelMsg struct{ level float64 }

// handleDictationLevel pushes a real mic level onto the waveform.
func (m model) handleDictationLevel(msg sttLevelMsg) model {
	if m.dictation.active() {
		m.dictation.pushLevel(levelHeight(msg.level))
	}
	return m
}

// recTickMsg drives the BATCH synthetic waveform (batch records to a file, so
// there is no live audio to react to). The streaming path animates from real
// levels instead (sttLevelMsg) and does not tick.
type recTickMsg struct{}

func recTickCmd() tea.Cmd {
	return tea.Tick(recTickInterval, func(time.Time) tea.Msg { return recTickMsg{} })
}

// syntheticWaveLevel synthesizes an organic 0..8 bar height for the batch
// recording animation (batch records to a file, so there is no live signal to
// react to). Rather than loop a fixed pulse — which reads as mechanical — it
// layers a few incommensurate sinusoids so the envelope wanders and doesn't
// obviously repeat, plus a hash-based per-frame jitter for texture and an
// occasional near-silent dip so it breathes like real speech.
func syntheticWaveLevel(tick int) int {
	t := float64(tick)
	amp := 0.5 +
		0.24*math.Sin(t*0.33) +
		0.15*math.Sin(t*0.12+1.7) +
		0.12*math.Sin(t*0.71+0.4) +
		0.08*math.Sin(t*1.30+2.2) // faster wiggle so it never flatlines
	// Deterministic jitter in ~[-0.09, 0.09] from a cheap integer hash of the tick.
	h := uint32(tick)*2654435761 + 1013904223
	amp += (float64(h>>16&0xffff)/0xffff - 0.5) * 0.18
	// A slow, shallow gate occasionally eases the level down (a spoken pause) without
	// ever flatlining, so the wave keeps breathing instead of humming a fixed shape.
	if gate := math.Sin(t*0.09 + 0.6); gate < -0.5 {
		amp *= 0.55
	}
	switch {
	case amp < 0:
		amp = 0
	case amp > 1:
		amp = 1
	}
	return levelHeight(amp)
}

// handleRecTick advances the batch synthetic waveform and stops when recording ends.
func (m model) handleRecTick() (model, tea.Cmd) {
	if m.dictation.phase == dictRecording || m.dictation.phase == dictStarting {
		m.dictation.waveTick++
		m.dictation.pushLevel(syntheticWaveLevel(m.dictation.waveTick))
		return m, recTickCmd()
	}
	return m, nil
}

// voiceModeIndicator renders the current voice gesture or transcription state.
func (m model) voiceModeIndicator() string {
	if !m.dictation.voiceModeEnabled {
		return ""
	}
	return zeroTheme.accent.Render("🎙 voice") + zeroTheme.muted.Render(" · "+m.voiceCaptureStatus()+" · "+m.dictation.currentModelLabel())
}

// dictationErrorText renders a dictation error for the transcript: a missing-
// tool setup error becomes actionable guidance, everything else is shown plainly.
func dictationErrorText(err error) string {
	var setupErr *dictation.SetupError
	if errors.As(err, &setupErr) {
		return "Dictation unavailable: " + setupErr.Error()
	}
	return fmt.Sprintf("Dictation failed: %v", err)
}
