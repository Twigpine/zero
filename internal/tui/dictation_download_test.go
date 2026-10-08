package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/dictation"
)

// buildFails returns a controller whose build always fails with a setup error,
// as if the local engine/model were missing.
func setupErrController(downloadRoot string) dictationController {
	return dictationController{
		cfg:          config.STTConfig{Provider: config.STTProviderLocal},
		platform:     dictation.DetectPlatform(),
		downloadRoot: downloadRoot,
		build: func(config.STTConfig, bool) (Transcriber, bool, error) {
			return nil, false, &dictation.SetupError{Tool: "local STT model", Hint: "set stt.localModelPath"}
		},
	}
}

func TestF9PointsToSTTModelWhenLocalMissing(t *testing.T) {
	if !dictation.AutoDownloadSupported() {
		t.Skip("no prebuilt engine for this platform")
	}
	m := model{dictation: setupErrController("/tmp/zero-stt")}
	next, _ := m.toggleDictation()
	if !transcriptHasText(next, "/stt-model") {
		t.Error("F9 with a missing local engine should point at /stt-model to download")
	}
}

func TestF9PlainSetupErrorWithoutDownloadRoot(t *testing.T) {
	m := model{dictation: setupErrController("")} // no download root
	next, _ := m.toggleDictation()
	if !transcriptHasText(next, "set stt.localModelPath") {
		t.Error("without a download root, expected the plain setup error")
	}
}

func TestDictationNoticeDedupe(t *testing.T) {
	m := model{}
	m = m.appendDictationNotice("k", "same message")
	m = m.appendDictationNotice("k", "same message")
	m = m.appendDictationNotice("k", "same message")
	n := 0
	for _, row := range m.transcript {
		if row.text == "same message" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("consecutive identical notices should dedupe to 1, got %d", n)
	}
	// A different key breaks the dedupe.
	m = m.appendDictationNotice("other", "another")
	if !transcriptHasText(m, "another") {
		t.Error("a different-keyed notice should still show")
	}
}

func TestDownloadedBatchModelAppliedToConfig(t *testing.T) {
	m := model{dictation: dictationController{cfg: config.STTConfig{Provider: config.STTProviderLocal}}}
	m.dictation.downloading = true
	comp := dictation.EngineComponents{
		BinaryPath: "/x/bin/sherpa-onnx-offline",
		ServerPath: "/x/bin/sherpa-onnx-online-websocket-server",
		ModelPath:  "/x/model",
	}
	got, _ := m.handleDictationDownloaded(dictationDownloadedMsg{components: comp, streaming: false})
	if got.dictation.downloading {
		t.Error("downloading flag should clear")
	}
	if got.dictation.cfg.LocalBinary != comp.BinaryPath || got.dictation.cfg.LocalModelPath != comp.ModelPath {
		t.Errorf("engine paths not applied: %+v", got.dictation.cfg)
	}
	if got.dictation.cfg.StreamingEnabled() {
		t.Error("a batch variant should set streaming off")
	}
	if !strings.Contains(got.transientNotice.text, "ready") {
		t.Error("expected a ready notice")
	}
}

func TestDownloadedStreamingModelSetsStreamingOn(t *testing.T) {
	m := model{dictation: dictationController{cfg: config.STTConfig{Provider: config.STTProviderLocal}}}
	comp := dictation.EngineComponents{BinaryPath: "/x/bin/sherpa-onnx-offline", ModelPath: "/x/model"}
	got, _ := m.handleDictationDownloaded(dictationDownloadedMsg{components: comp, streaming: true})
	if !got.dictation.cfg.StreamingEnabled() {
		t.Error("a streaming variant should set streaming on")
	}
}

func TestSTTDownloadLoadingPickerThenFilled(t *testing.T) {
	// The chooser opens in a single loading state (no curated-then-swap flicker),
	// then handleSTTModelsFetched fills it with the fetched list.
	loading := newSTTDownloadLoadingPicker()
	if loading.kind != pickerSTTDownload || !loading.loading || len(loading.items) != 0 {
		t.Fatalf("loading picker = %+v, want an empty, loading STT-download picker", loading)
	}

	m := model{dictation: dictationController{downloadRoot: "/tmp/x"}}
	m.picker = loading
	fetched := dictation.ModelVariants()
	m = m.handleSTTModelsFetched(sttModelsFetchedMsg{variants: fetched})
	if m.dictation.browseLoading {
		t.Error("browseLoading should clear once the list arrives")
	}
	if m.picker.loading {
		t.Error("the filled picker should no longer be loading")
	}
	if len(m.picker.items) != len(fetched) {
		t.Errorf("filled picker has %d items, want %d", len(m.picker.items), len(fetched))
	}
}

func TestSTTModelsFetchFailureFallsBackToCurated(t *testing.T) {
	// A failed fetch with nothing cached falls back to the curated shortlist so the
	// picker still works offline (never a permanently-empty loading box).
	m := model{dictation: dictationController{downloadRoot: "/tmp/x"}}
	m.picker = newSTTDownloadLoadingPicker()
	m = m.handleSTTModelsFetched(sttModelsFetchedMsg{err: errors.New("offline")})
	if m.picker.loading {
		t.Error("picker should stop loading even on fetch failure")
	}
	if len(m.picker.items) != len(dictation.ModelVariants()) {
		t.Errorf("fallback picker has %d items, want the %d curated variants",
			len(m.picker.items), len(dictation.ModelVariants()))
	}
}

func TestSTTDownloadPickerListsVariants(t *testing.T) {
	m := model{dictation: dictationController{downloadRoot: "/tmp/x"}}
	p := m.newSTTDownloadPicker()
	if p == nil || p.kind != pickerSTTDownload {
		t.Fatal("expected an STT download picker")
	}
	// Exactly the curated download variants — no dead-end "manual" row.
	if len(p.items) != len(dictation.ModelVariants()) {
		t.Errorf("picker has %d items, want %d variants", len(p.items), len(dictation.ModelVariants()))
	}
	for _, it := range p.items {
		if it.Value == "manual" {
			t.Error("the dead-end manual row should be gone")
		}
	}
}

func TestSTTDownloadSelectionStartsDownload(t *testing.T) {
	m := model{dictation: dictationController{downloadRoot: "/tmp/x", cfg: config.STTConfig{Provider: config.STTProviderLocal}}}
	variant := dictation.ModelVariants()[0]
	next, cmd := m.handleSTTDownloadSelection(variant.ID)
	if !next.dictation.downloading {
		t.Error("selecting a variant should start a download")
	}
	if cmd == nil {
		t.Error("expected a download command")
	}
	if !strings.Contains(next.transientNotice.text, variant.Label) {
		t.Error("expected the download-starting notice to name the variant")
	}
}

func TestDownloadFailureReported(t *testing.T) {
	m := model{dictation: dictationController{}}
	m.dictation.downloading = true
	next, _ := m.handleDictationDownloaded(dictationDownloadedMsg{err: errors.New("network down")})
	got := next
	if got.dictation.downloading {
		t.Error("downloading flag should clear on failure")
	}
	if !transcriptHasText(got, "network down") {
		t.Error("expected the failure reported")
	}
}
