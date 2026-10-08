package tui

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/specmode"
	"github.com/Gitlawb/zero/internal/tools"
)

func (m model) handleSpecCommand(task string) (tea.Model, tea.Cmd) {
	task = strings.TrimSpace(task)
	if task == "" {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Usage: /spec <task>"})
		return m, nil
	}
	// m.exiting guards the post-Ctrl+C flush window: starting a run there would
	// let the deferred tea.Quit kill it mid-flight and orphan its checkpoints —
	// the same gate handleSubmit applies to plain prompts.
	if m.pending || m.exiting {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendError, text: "Cannot start spec mode while a run is active."})
		return m, nil
	}
	if m.pendingSpecReview != nil {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Review the pending spec before drafting another one."})
		return m, nil
	}
	if m.provider == nil {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendAssistant, text: "No provider configured."})
		return m, nil
	}

	m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendUser, text: "/spec " + task})
	var err error
	m, err = m.createSpecDraftSession(task)
	if err != nil {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendError, text: "session create error: " + err.Error()})
		return m, nil
	}
	m, err = m.appendSessionEvent(sessions.EventMessage, map[string]any{
		"role":    "user",
		"content": task,
	})
	if err != nil {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendError, text: "session record error: " + err.Error()})
	}

	turnImages := m.pendingImages
	if len(turnImages) > 0 && !m.modelSupportsVisionTUI() {
		name := m.modelName
		if name == "" {
			name = "the active model"
		}
		m.transcript = reduceTranscript(m.transcript, transcriptAction{
			kind: actionAppendSystem,
			text: fmt.Sprintf("Model %s does not support image input; ignoring %d image(s).", name, len(turnImages)),
		})
		turnImages = nil
	}
	m.pendingImages = nil
	m.pendingImageLabels = nil
	m.pendingImageThumbnails = nil

	specRegistry := cloneToolRegistry(m.registry)
	specmode.RegisterDraftTools(specRegistry, m.cwd, m.now)
	runCtx, cancel := context.WithCancel(m.ctx)
	m = m.beginRun(cancel)
	return m, tea.Batch(m.runAgentWithOptions(m.activeRunID, runCtx, task, turnImages, tuiAgentRunOptions{
		registry:       specRegistry,
		permissionMode: agent.PermissionModeSpecDraft,
		systemPrompt:   specmode.DraftSystemPrompt,
		specDraft:      true,
	}), m.spinner.Tick)
}

func (m model) createSpecDraftSession(task string) (model, error) {
	session, err := m.sessionStore.Create(sessions.CreateInput{
		SessionKind:        sessions.SessionKindSpecDraft,
		Title:              tuiSessionTitle(task),
		Cwd:                m.cwd,
		ModelID:            m.modelName,
		Provider:           m.providerName,
		SpecDraftModelID:   m.modelName,
		SpecDraftReasoning: string(m.reasoningEffort),
	})
	if err != nil {
		return m, err
	}
	m.activeSession = session
	m.sessionEvents = []sessions.Event{}
	m = m.syncPeerIdentity()
	return m, nil
}

func tuiSpecReviewFromToolResult(result agent.ToolResult, draftSessionID string) (pendingSpecReviewPrompt, bool) {
	if result.Name != specmode.SubmitToolName || result.Meta["control"] != specmode.ControlSpecReviewRequired {
		return pendingSpecReviewPrompt{}, false
	}
	return pendingSpecReviewPrompt{
		SpecID:         strings.TrimSpace(result.Meta["specId"]),
		SpecTitle:      strings.TrimSpace(result.Meta["specTitle"]),
		SpecFilePath:   strings.TrimSpace(result.Meta["specFilePath"]),
		RelativePath:   strings.TrimSpace(result.Meta["relativePath"]),
		DraftSessionID: strings.TrimSpace(draftSessionID),
	}, true
}

func (m model) activateSpecReview(review pendingSpecReviewPrompt) model {
	updated, event, err := m.sessionStore.RecordSpec(review.DraftSessionID, sessions.RecordSpecInput{
		SpecID:             review.SpecID,
		SpecFilePath:       review.SpecFilePath,
		SpecStatus:         sessions.SpecStatusDraft,
		SpecDraftModelID:   m.modelName,
		SpecDraftReasoning: string(m.reasoningEffort),
	})
	if err != nil {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendError, text: "spec record error: " + err.Error()})
		return m
	}
	if m.activeSession.SessionID == updated.SessionID {
		m.activeSession = updated
		m.sessionEvents = append(m.sessionEvents, event)
	}
	m.pendingSpecReview = &review
	m.clearSuggestions()
	m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: specReviewSummary(review)})
	return m
}

func (m model) handleSpecReviewKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch strings.ToLower(msg.String()) {
	case "a":
		return m.approveSpecReview()
	case "r":
		return m.rejectSpecReview("")
	case "e":
		review := m.pendingSpecReview
		if review == nil {
			return m, nil
		}
		m.transcript = reduceTranscript(m.transcript, transcriptAction{
			kind: actionAppendSystem,
			text: "Edit the saved spec file, then press a to approve or r to reject.\npath: " + reviewDisplayPath(*review),
		})
		return m, nil
	case "c":
		return m.cancelSpecReview()
	default:
		return m, nil
	}
}

func (m model) approveSpecReview() (tea.Model, tea.Cmd) {
	review := m.pendingSpecReview
	if review == nil {
		return m, nil
	}
	if m.provider == nil {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendAssistant, text: "No provider configured."})
		return m, nil
	}
	body, path, err := specmode.LoadSpecFile(m.cwd, review.SpecFilePath)
	if err != nil {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendError, text: "spec read error: " + err.Error()})
		return m, nil
	}
	prompt := specmode.ImplementationPrompt(body, path, review.DraftSessionID, "")
	impl, events, err := m.sessionStore.EnsureSpecImplementation(sessions.EnsureSpecImplementationInput{
		Title:               specImplementationTitle(*review),
		Cwd:                 m.cwd,
		ModelID:             m.modelName,
		Provider:            m.providerName,
		RootSessionID:       review.DraftSessionID,
		SpecID:              review.SpecID,
		SpecFilePath:        path,
		SpecDraftModelID:    m.modelName,
		SpecDraftReasoning:  string(m.reasoningEffort),
		SpecSourceSessionID: review.DraftSessionID,
		Prompt:              prompt,
	})
	if err != nil {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendError, text: "session create error: " + err.Error()})
		return m, nil
	}
	if _, _, err := m.sessionStore.RecordSpec(review.DraftSessionID, sessions.RecordSpecInput{
		SpecID:             review.SpecID,
		SpecFilePath:       path,
		SpecStatus:         sessions.SpecStatusApproved,
		SpecDraftModelID:   m.modelName,
		SpecDraftReasoning: string(m.reasoningEffort),
		SpecImplSessionID:  impl.SessionID,
	}); err != nil {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendError, text: "spec approve error: " + err.Error()})
		return m, nil
	}
	m.pendingSpecReview = nil
	m.activeSession = impl
	m.sessionEvents = append([]sessions.Event{}, events...)
	m = m.syncPeerIdentity()
	m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Spec approved. Starting implementation session " + impl.SessionID + "."})
	runCtx, cancel := context.WithCancel(m.ctx)
	m = m.beginRun(cancel)
	// Seed the streaming-text fade state for the spec-impl run. The normal
	// launchPrompt path lets the first agentTextMsg do this; the spec-impl path
	// calls runAgent directly, so we seed explicitly. The first incoming delta
	// will re-stamp the in-progress entry and the fade will start naturally.
	m.resetStreamingFade()
	m.fadeActive = true
	return m, tea.Batch(m.runAgent(m.activeRunID, runCtx, prompt, nil), m.spinner.Tick)
}

func (m model) rejectSpecReview(reason string) (tea.Model, tea.Cmd) {
	review := m.pendingSpecReview
	if review == nil {
		return m, nil
	}
	updated, event, err := m.sessionStore.RecordSpec(review.DraftSessionID, sessions.RecordSpecInput{
		SpecID:             review.SpecID,
		SpecFilePath:       review.SpecFilePath,
		SpecStatus:         sessions.SpecStatusRejected,
		SpecDraftModelID:   m.modelName,
		SpecDraftReasoning: string(m.reasoningEffort),
		SpecRejectReason:   reason,
	})
	if err != nil {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendError, text: "spec reject error: " + err.Error()})
		return m, nil
	}
	if m.activeSession.SessionID == updated.SessionID {
		m.activeSession = updated
		m.sessionEvents = append(m.sessionEvents, event)
	}
	m.pendingSpecReview = nil
	m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Spec rejected. Use /spec <task> to draft again."})
	return m.launchQueuedMessageIfReady()
}

func (m model) cancelSpecReview() (tea.Model, tea.Cmd) {
	m.pendingSpecReview = nil
	m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Spec review canceled. The draft remains saved."})
	return m.launchQueuedMessageIfReady()
}

func cloneToolRegistry(registry *tools.Registry) *tools.Registry {
	return registry.Clone()
}

// renderFocusedSpecReviewPrompt draws the spec-review gate in the shared card
// language (badge + body + key chips) with line borders. Key handling lives
// in handleSpecReviewKey, unchanged.
func renderFocusedSpecReviewPrompt(review pendingSpecReviewPrompt, width int) string {
	fill := zeroTheme.onPanel
	actions := zeroTheme.badge.Render(" [a] approve ") +
		fill(zeroTheme.ink).Render(" ") +
		fill(zeroTheme.red).Render("[r]") + fill(zeroTheme.ink).Render(" reject ") +
		fill(zeroTheme.accent).Render("[e]") + fill(zeroTheme.ink).Render(" edit file ") +
		fill(zeroTheme.faint).Render("[esc] cancel")
	lines := []string{
		zeroTheme.badge.Render(" SPEC REVIEW "),
		fill(zeroTheme.faint).Render("path: ") + fill(zeroTheme.ink).Render(reviewDisplayPath(review)),
		actions,
	}
	return styledBlockFill(width, lines, zeroTheme.line, zeroTheme.panel)
}

func specReviewSummary(review pendingSpecReviewPrompt) string {
	return renderCommandOutput(commandOutput{
		Title:  "Spec draft ready",
		Status: commandStatusOK,
		Sections: []commandSection{{
			Title: "Review",
			Lines: []string{
				"spec: " + review.SpecID,
				"path: " + reviewDisplayPath(review),
				"keys: a approve, r reject, e edit file, esc cancel",
			},
		}},
	})
}

func reviewDisplayPath(review pendingSpecReviewPrompt) string {
	if strings.TrimSpace(review.RelativePath) != "" {
		return review.RelativePath
	}
	return review.SpecFilePath
}

func specImplementationTitle(review pendingSpecReviewPrompt) string {
	title := strings.TrimSpace(review.SpecTitle)
	if title == "" {
		title = strings.TrimSpace(review.SpecID)
	}
	if title == "" {
		return "Spec implementation"
	}
	title = cutRunes(title, tuiSessionTitleLimit)
	return title + " implementation"
}
