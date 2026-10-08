package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/dictation"
)

func TestDictationTranscribedInsertsIntoComposer(t *testing.T) {
	m := model{}
	m.setComposerState(composerState{text: "hello", cursor: 5})
	m.dictation.phase = dictTranscribing

	next, _ := m.handleDictationTranscribed(dictationTranscribedMsg{text: "world"})
	got := next.(model)
	if got.composer.text != "hello world" {
		t.Errorf("composer = %q, want 'hello world'", got.composer.text)
	}
	if got.dictation.active() {
		t.Error("dictation should be idle after transcription")
	}
}

func TestDictationTranscribedEmptyIsNoticed(t *testing.T) {
	m := model{}
	m.dictation.phase = dictTranscribing
	next, _ := m.handleDictationTranscribed(dictationTranscribedMsg{text: "   "})
	got := next.(model)
	if got.composer.text != "" {
		t.Errorf("composer should stay empty, got %q", got.composer.text)
	}
	if got.transientNotice.text != "No speech detected." {
		t.Errorf("no-speech notice = %q", got.transientNotice.text)
	}
	if transcriptHasText(got, "No speech detected") {
		t.Error("no-speech confirmation should not be persisted in the transcript")
	}
}

func TestDictationTranscribedSetupErrorGuides(t *testing.T) {
	m := model{}
	m.dictation.phase = dictTranscribing
	err := &dictation.SetupError{Tool: "arecord", Hint: "install alsa-utils"}
	next, _ := m.handleDictationTranscribed(dictationTranscribedMsg{err: err})
	got := next.(model)
	if !transcriptHasText(got, "install alsa-utils") {
		t.Error("expected setup guidance in the transcript")
	}
}

func TestDictationAuthErrorReopensKeyPrompt(t *testing.T) {
	// A cloud auth failure should offer an inline key prompt for the current
	// provider (preserving its model), not dead-end on "run zero auth".
	m := model{}
	m.dictation.cfg = config.STTConfig{Provider: config.STTProviderGroq, Model: "whisper-large-v3-turbo"}
	m.dictation.saveKey = func(string, string) error { return nil }
	m.dictation.phase = dictTranscribing

	err := &dictation.AuthError{Provider: "groq", Message: "auth error: your API key is missing or invalid"}
	next, _ := m.handleDictationTranscribed(dictationTranscribedMsg{err: err})
	got := next.(model)
	if got.sttKeyPrompt == nil {
		t.Fatal("an auth failure should reopen the API-key prompt")
	}
	if got.sttKeyPrompt.provider != "groq" || got.sttKeyPrompt.modelValue != "groq:whisper-large-v3-turbo" {
		t.Errorf("key prompt = %+v, want groq / groq:whisper-large-v3-turbo", got.sttKeyPrompt)
	}
}

func TestDictationNonAuthErrorDoesNotPrompt(t *testing.T) {
	m := model{}
	m.dictation.cfg = config.STTConfig{Provider: config.STTProviderGroq}
	m.dictation.saveKey = func(string, string) error { return nil }
	m.dictation.phase = dictTranscribing

	next, _ := m.handleDictationTranscribed(dictationTranscribedMsg{err: errors.New("network unreachable")})
	got := next.(model)
	if got.sttKeyPrompt != nil {
		t.Error("a non-auth error must not open the key prompt")
	}
	if !transcriptHasText(got, "network unreachable") {
		t.Error("expected the raw error in the transcript")
	}
}

func TestDictationStartedFailureResets(t *testing.T) {
	m := model{}
	m.dictation.sessionID = 1
	m.dictation.phase = dictStarting
	m.dictation.streaming = false
	got, _ := m.handleDictationStarted(dictationStartedMsg{sessionID: 1, err: errors.New("mic busy")})
	if got.dictation.active() {
		t.Error("a start failure should reset to idle")
	}
	if !transcriptHasText(got, "mic busy") {
		t.Error("expected the start error in the transcript")
	}
}

func TestDictationStartedArmsRecording(t *testing.T) {
	m := model{}
	m.dictation.sessionID = 1
	m.dictation.phase = dictStarting
	got, _ := m.handleDictationStarted(dictationStartedMsg{sessionID: 1})
	if got.dictation.phase != dictRecording {
		t.Error("a successful start should advance to recording")
	}
}

// TestStaleDictationStartedIgnoredWhileLive pins the session gate on batch
// startup messages. A late Start() result after cancel must not reset() a
// newer recording (success would leave dictStarting with no recorder; failure
// would kill the live session).
func TestStaleDictationStartedIgnoredWhileLive(t *testing.T) {
	// Active recording on session 2.
	m := model{}
	m.dictation.sessionID = 2
	m.dictation.phase = dictRecording
	m.dictation.streaming = true
	m.setComposerState(composerState{text: "live session text", cursor: len("live session text")})

	// Stale failure from session 1 must not reset the live controller.
	afterFail, _ := m.handleDictationStarted(dictationStartedMsg{
		sessionID: 1,
		err:       errors.New("mic busy from previous start"),
	})
	if afterFail.dictation.phase != dictRecording || afterFail.dictation.sessionID != 2 {
		t.Fatalf("stale start failure: phase=%v session=%d, want recording/2", afterFail.dictation.phase, afterFail.dictation.sessionID)
	}
	if transcriptHasText(afterFail, "mic busy from previous start") {
		t.Fatal("stale start failure must not post an error notice")
	}

	// Stale success from session 1 must not leave the live session stuck mid-start.
	m2 := model{}
	m2.dictation.sessionID = 2
	m2.dictation.phase = dictRecording
	afterOK, _ := m2.handleDictationStarted(dictationStartedMsg{sessionID: 1})
	if afterOK.dictation.phase != dictRecording || afterOK.dictation.sessionID != 2 {
		t.Fatalf("stale start success: phase=%v session=%d, want recording/2", afterOK.dictation.phase, afterOK.dictation.sessionID)
	}
}

func TestDictationUnavailableShowsHint(t *testing.T) {
	m := model{} // no build factory
	next, _ := m.toggleDictation()
	if !transcriptHasText(next, "not configured") {
		t.Error("expected a 'not configured' hint when dictation is unavailable")
	}
}

func TestDictationStreamingPartialReplacesRegion(t *testing.T) {
	m := model{}
	m.setComposerState(composerState{text: "note:", cursor: 5})
	m.dictation.phase = dictRecording
	m.dictation.streaming = true

	m = m.handleDictationPartial(sttPartialMsg{text: "the quick"})
	if m.composer.text != "note: the quick" {
		t.Fatalf("after first partial: %q", m.composer.text)
	}
	// A longer cumulative partial replaces the previous region wholesale.
	m = m.handleDictationPartial(sttPartialMsg{text: "the quick brown fox"})
	if m.composer.text != "note: the quick brown fox" {
		t.Fatalf("after second partial: %q", m.composer.text)
	}
	// Cancel discards the streamed region.
	m2 := m.discardDictationRegion()
	if m2.composer.text != "note:" {
		t.Errorf("discard should restore original text, got %q", m2.composer.text)
	}
}

func TestDictationCanceledStreamRaceDoesNotAutoSubmit(t *testing.T) {
	// Esc can race an already-buffered realtime event: cancelDictation discards
	// the live region and resets state synchronously, but the streaming
	// goroutine's dictationTranscribedMsg (a nonempty compose() alongside
	// context.Canceled) can still arrive afterward. With stt.autoSubmit on,
	// that must never fall through to msg.submit and fire the composer's
	// restored pre-existing text.
	// Matching sessionID is required so this hits the context.Canceled branch
	// rather than only the stale-session gate (session 0 after cancel would be
	// dropped before that branch runs).
	m := model{}
	m.dictation.sessionID = 1
	m.setComposerState(composerState{text: "existing prompt", cursor: len("existing prompt")})
	m.dictation.phase = dictRecording
	m.dictation.streaming = true

	m = m.handleDictationPartial(sttPartialMsg{sessionID: 1, text: "half-formed transcript"})
	if m.composer.text != "existing prompt half-formed transcript" {
		t.Fatalf("partial did not render into composer: %q", m.composer.text)
	}

	m, _ = m.cancelDictation()
	if m.composer.text != "existing prompt" {
		t.Fatalf("cancel should restore the pre-existing composer text, got %q", m.composer.text)
	}

	// The already-buffered event's message arrives after the cancel above.
	next, cmd := m.handleDictationTranscribed(dictationTranscribedMsg{
		sessionID: m.dictation.sessionID,
		text:      "half-formed transcript",
		err:       context.Canceled,
		submit:    true,
		streaming: true,
	})
	got := next.(model)
	if got.composer.text != "existing prompt" {
		t.Errorf("a raced cancellation must not auto-submit the restored composer text, got %q", got.composer.text)
	}
	if cmd != nil {
		t.Error("a raced cancellation must not return a submit command")
	}
}

// TestStaleDictationPartialIgnoredWhileLive pins the session-ID gate on
// handleDictationPartial while a new session is still recording. Cancel-then-
// stale cases are already covered by the phase guard; this case needs a live
// phase so only the session check drops the ghost partial.
func TestStaleDictationPartialIgnoredWhileLive(t *testing.T) {
	m := model{}
	m.dictation.sessionID = 2
	m.dictation.phase = dictRecording
	m.dictation.streaming = true
	m.setComposerState(composerState{text: "user typed this", cursor: len("user typed this")})

	got := m.handleDictationPartial(sttPartialMsg{
		sessionID: 1,
		text:      "ghost text from session 1",
	})
	if got.composer.text != "user typed this" {
		t.Fatalf("stale partial while live: got %q, want %q", got.composer.text, "user typed this")
	}
}

func TestDictationCommitKeepsStreamedText(t *testing.T) {
	m := model{}
	m.setComposerState(composerState{text: "", cursor: 0})
	m.dictation.phase = dictRecording
	m.dictation.streaming = true
	m = m.handleDictationPartial(sttPartialMsg{text: "final words"})
	got, _ := m.handleDictationTranscribed(dictationTranscribedMsg{text: "final words", streaming: true})
	if got.(model).composer.text != "final words" {
		t.Errorf("committed streamed text lost: %q", got.(model).composer.text)
	}
}

func TestVoiceOffReleasesWarmServer(t *testing.T) {
	shutdownCalls := 0
	m := model{}
	m.dictation.build = func(config.STTConfig, bool) (Transcriber, bool, error) { return nil, false, nil } // makes available()
	m.dictation.shutdownServer = func(context.Context) error { shutdownCalls++; return nil }

	on, _ := m.toggleVoiceMode()
	if !on.dictation.voiceModeEnabled {
		t.Fatal("voice should be on after first toggle")
	}
	off, cmd := on.toggleVoiceMode()
	if off.dictation.voiceModeEnabled {
		t.Fatal("voice should be off after second toggle")
	}
	if cmd == nil {
		t.Fatal("turning voice off should return a server-release command")
	}
	cmd() // run it
	if shutdownCalls != 1 {
		t.Errorf("shutdown called %d times, want 1", shutdownCalls)
	}
}

func TestVoiceOffMidRecordingKeepsServer(t *testing.T) {
	m := model{}
	m.dictation.build = func(config.STTConfig, bool) (Transcriber, bool, error) { return nil, false, nil }
	m.dictation.shutdownServer = func(context.Context) error {
		t.Error("must not tear down the server while a recording is in flight")
		return nil
	}
	m.dictation.voiceModeEnabled = true
	m.dictation.phase = dictRecording
	if _, cmd := m.toggleVoiceMode(); cmd != nil {
		t.Error("no server-release command should be issued mid-recording")
	}
}

func TestWantStreamingGatedByConfigAndPlatform(t *testing.T) {
	streamOff := false
	d := dictationController{cfg: config.STTConfig{Streaming: &streamOff}, platform: dictation.PlatformLinux}
	if d.wantStreaming() {
		t.Error("streaming:false in config should disable streaming")
	}
	d = dictationController{platform: dictation.PlatformTermux}
	if d.wantStreaming() {
		t.Error("Termux has no streaming capture, wantStreaming must be false")
	}
	d = dictationController{platform: dictation.PlatformLinux}
	if !d.wantStreaming() {
		t.Error("desktop default should want streaming")
	}
}

func TestNeedsLeadingSpace(t *testing.T) {
	if needsLeadingSpace(composerState{text: "hi", cursor: 2}) != true {
		t.Error("cursor after non-space should need a leading space")
	}
	if needsLeadingSpace(composerState{text: "hi ", cursor: 3}) != false {
		t.Error("cursor after a space should not need one")
	}
	if needsLeadingSpace(composerState{text: "", cursor: 0}) != false {
		t.Error("empty composer should not need a leading space")
	}
}

func TestDictationBatchCtxNonNil(t *testing.T) {
	d := dictationController{}
	if got := transcribeBatchCtx(d.ctx); got == nil {
		t.Error("batch ctx should never be nil")
	}
}

// transcribeBatchCtx mirrors the nil-guard in transcribeBatchCmd for testing.
func transcribeBatchCtx(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func transcriptHasText(m model, substr string) bool {
	for _, row := range m.transcript {
		if strings.Contains(row.text, substr) {
			return true
		}
	}
	return false
}

func TestWaveformRendersLevels(t *testing.T) {
	// Higher levels render taller bars; the ring scrolls left as levels arrive.
	var d dictationController
	d.pushLevel(levelHeight(1.0)) // loud → tall
	d.pushLevel(levelHeight(0.0)) // silent → short
	bars := renderWaveBars(d.waveBars)
	if len([]rune(bars)) != waveBarCount {
		t.Errorf("waveform width = %d, want %d", len([]rune(bars)), waveBarCount)
	}
	// The newest (rightmost) bar reflects the last pushed level (silent → space/low).
	runes := []rune(bars)
	if runes[waveBarCount-1] != waveRunes[0] {
		t.Errorf("last bar should be the silent level, got %q", string(runes[waveBarCount-1]))
	}
}

func TestDictationLevelDrivesWave(t *testing.T) {
	m := model{}
	m.dictation.phase = dictRecording
	m = m.handleDictationLevel(sttLevelMsg{level: 0.9})
	if len(m.dictation.waveBars) != waveBarCount {
		t.Fatalf("a mic level should populate the waveform ring")
	}
	if m.dictation.waveBars[waveBarCount-1] != levelHeight(0.9) {
		t.Error("the newest bar should match the received mic level")
	}
}

func TestHandleRecTickOnlyAnimatesWhileRecording(t *testing.T) {
	m := model{}
	m.dictation.phase = dictRecording
	next, cmd := m.handleRecTick()
	if next.dictation.waveTick != 1 || cmd == nil {
		t.Error("recording should advance the batch synthetic wave and reschedule")
	}
	m.dictation.phase = dictIdle
	_, cmd = m.handleRecTick()
	if cmd != nil {
		t.Error("idle should stop ticking")
	}
}

func TestCurrentModelLabel(t *testing.T) {
	// Cloud provider + model.
	d := dictationController{cfg: config.STTConfig{Provider: config.STTProviderGroq, Model: "whisper-large-v3-turbo"}}
	if got := d.currentModelLabel(); got != "Groq whisper-large-v3-turbo" {
		t.Errorf("groq label = %q", got)
	}
	// Cloud provider with default model.
	d = dictationController{cfg: config.STTConfig{Provider: config.STTProviderOpenAI}}
	if got := d.currentModelLabel(); got != "OpenAI whisper-1" {
		t.Errorf("openai default label = %q", got)
	}
	// Local with a downloaded curated variant → "Local · <friendly name>".
	kroko := dictation.ModelVariants()[0] // Kroko streaming
	d = dictationController{cfg: config.STTConfig{Provider: config.STTProviderLocal, LocalModelPath: "/home/x/.config/zero/stt/" + kroko.DirName + "/sherpa-onnx-...-kroko"}}
	if got := d.currentModelLabel(); got != "Local · "+kroko.Label {
		t.Errorf("local kroko label = %q, want %q", got, "Local · "+kroko.Label)
	}
	// Local with no model.
	d = dictationController{cfg: config.STTConfig{Provider: config.STTProviderLocal}}
	if got := d.currentModelLabel(); got != "Local (no model set)" {
		t.Errorf("no-model label = %q", got)
	}
}

func TestStaleDictationCompletionIgnoredAfterCancel(t *testing.T) {
	m := model{}
	m.setComposerState(composerState{text: "original composer text", cursor: 22})
	m.dictation.sessionID = 1
	m.dictation.phase = dictRecording

	m, _ = m.cancelDictation()

	if m.composer.text != "original composer text" {
		t.Fatalf("composer text = %q, want 'original composer text'", m.composer.text)
	}

	// Stale partial message from session 1 must be ignored
	stalePartial := sttPartialMsg{
		sessionID: 1,
		text:      "stale partial that should be ignored",
	}
	mPartial := m.handleDictationPartial(stalePartial)
	if mPartial.composer.text != "original composer text" {
		t.Fatalf("composer text changed to %q after stale partial", mPartial.composer.text)
	}

	staleMsg := dictationTranscribedMsg{
		sessionID: 1,
		text:      "stale text that should be ignored",
		submit:    true,
		streaming: true,
	}
	afterStale, cmd := m.handleDictationTranscribed(staleMsg)
	got := afterStale.(model)

	if got.composer.text != "original composer text" {
		t.Fatalf("composer text changed to %q after stale completion", got.composer.text)
	}
	if cmd != nil {
		t.Fatalf("expected no command for stale completion, got %v", cmd)
	}
}

func TestStaleDictationSessionZeroIgnoredAfterCancel(t *testing.T) {
	m := model{}
	m.setComposerState(composerState{text: "initial composer text", cursor: 21})
	m.dictation.sessionID = 0
	m.dictation.phase = dictRecording

	m, _ = m.cancelDictation()

	// Session 0 was reset to session 1
	if m.dictation.sessionID != 1 {
		t.Fatalf("dictation.sessionID = %d after cancel, want 1", m.dictation.sessionID)
	}

	// Stale partial from session 0 must be ignored
	stalePartial := sttPartialMsg{
		sessionID: 0,
		text:      "stale partial from session 0",
	}
	mPartial := m.handleDictationPartial(stalePartial)
	if mPartial.composer.text != "initial composer text" {
		t.Fatalf("composer text changed to %q after stale session 0 partial", mPartial.composer.text)
	}

	// Stale completion from session 0 must be ignored
	staleMsg := dictationTranscribedMsg{
		sessionID: 0,
		text:      "stale completion from session 0",
		submit:    true,
		streaming: true,
	}
	afterStale, cmd := m.handleDictationTranscribed(staleMsg)
	got := afterStale.(model)

	if got.composer.text != "initial composer text" {
		t.Fatalf("composer text changed to %q after stale session 0 completion", got.composer.text)
	}
	if cmd != nil {
		t.Fatalf("expected no command for stale session 0 completion, got %v", cmd)
	}
}
