package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/dictation"
)

// sttDownloadProgressMsg carries a live download-status line (with a
// percentage) into the TUI, injected from the download goroutine via
// runtimeMessageSink.
type sttDownloadProgressMsg struct{ status string }

// dictationDownloadedMsg reports the outcome of an auto-download.
type dictationDownloadedMsg struct {
	components dictation.EngineComponents
	err        error
	// streaming is the chosen variant's pipeline (a streaming transducer vs a
	// batch model), applied to the config after a successful download.
	streaming bool
}

// canOfferDownload reports whether a failed build should point the user at the
// /stt-model download chooser: the failure is a missing-setup error, the user is
// on the local provider, this platform has a prebuilt engine, and a download
// root is configured.
func (d dictationController) canOfferDownload(err error) bool {
	var setupErr *dictation.SetupError
	return d.downloadRoot != "" && !d.downloading &&
		d.cfg.STTProvider() == config.STTProviderLocal &&
		dictation.AutoDownloadSupported() &&
		errors.As(err, &setupErr)
}

// startVariantDownload fetches the sherpa-onnx engine + the chosen model
// variant, reporting progress live, then persists and applies the resolved paths.
func (m model) startVariantDownload(v dictation.ModelVariant) (model, tea.Cmd) {
	if m.dictation.downloading {
		return m.showTransientNoticeInline("Dictation setup is already in progress.", transientNoticeWarning), nil
	}
	m.dictation.downloading = true
	sink := m.runtimeMessageSink
	root := m.dictation.downloadRoot
	version := m.dictation.cfg.EngineVersion
	base := m.ctx
	if base == nil {
		base = context.Background()
	}
	m.dictation.downloadStatus = "Starting download…"
	m = m.showTransientNoticeInline(fmt.Sprintf("Setting up local dictation (%s) — progress is in the status line.", v.Label), transientNoticeInfo)
	return m, func() tea.Msg {
		comp, err := dictation.EnsureLocalEngine(base, dictation.DownloadOptions{
			DestRoot:          root,
			EngineVersion:     version,
			ModelAssetName:    v.AssetName,
			ModelPinnedDigest: v.Digest,
			ModelDirName:      v.DirName,
			ModelLabel:        v.Label,
			Progress: func(status string) {
				if sink != nil {
					sink(sttDownloadProgressMsg{status: status})
				}
			},
		})
		return dictationDownloadedMsg{components: comp, err: err, streaming: v.Streaming}
	}
}

// handleDictationDownloadProgress updates the single live status line (shown in
// the status bar) — not a new transcript line per update.
func (m model) handleDictationDownloadProgress(msg sttDownloadProgressMsg) model {
	if m.dictation.downloading {
		m.dictation.downloadStatus = msg.status
	}
	return m
}

// handleDictationDownloaded persists and applies the downloaded engine, or
// reports the failure.
func (m model) handleDictationDownloaded(msg dictationDownloadedMsg) (model, tea.Cmd) {
	m.dictation.downloading = false
	m.dictation.downloadStatus = ""
	if msg.err != nil {
		return m.appendSystemNotice("Dictation setup failed: " + dictationErrorText(msg.err)), nil
	}
	m, err := m.applyEngineComponents(msg.components, msg.streaming)
	if err != nil {
		return m.appendSystemNotice("Downloaded the engine, but couldn't save the config: " + err.Error()), nil
	}
	return m.showTransientNoticeInline("Local dictation is ready. Run /voice to start dictating.", transientNoticeSuccess), nil
}

// applyEngineComponents persists and applies resolved engine/model paths to the
// live config (shared by a fresh download and the already-installed fast path).
func (m model) applyEngineComponents(comp dictation.EngineComponents, streaming bool) (model, error) {
	if path := m.dictation.userConfigPath; path != "" {
		if _, err := config.SetSTTLocalEngine(path, comp.BinaryPath, comp.ServerPath, comp.ModelPath, streaming); err != nil {
			return m, err
		}
	}
	m.dictation.cfg.Provider = config.STTProviderLocal
	m.dictation.cfg.LocalBinary = comp.BinaryPath
	m.dictation.cfg.LocalServerBinary = comp.ServerPath
	m.dictation.cfg.LocalModelPath = comp.ModelPath
	s := streaming
	m.dictation.cfg.Streaming = &s
	return m, nil
}

// appendDictationNotice appends a transcript notice, suppressing an immediate
// repeat of the same message — the fix for hammering the dictation gesture against an unusable
// model (or in voice mode) spamming the identical line on every keypress. It is
// stateless: it simply skips the append when the message equals the most recent
// transcript line, so an unrelated line in between lets it show again.
func (m model) appendDictationNotice(key, text string) model {
	_ = key
	if n := len(m.transcript); n > 0 && strings.TrimSpace(m.transcript[n-1].text) == strings.TrimSpace(text) {
		return m
	}
	return m.appendSystemNotice(text)
}
