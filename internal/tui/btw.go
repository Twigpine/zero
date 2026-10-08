package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/usage"
)

const (
	btwSessionTag = "btw"
	// btwRunIDGap keeps the hidden parent's current run below the first side run.
	// A parent cannot start another user turn while its surface is hidden; the
	// monotonic btwRunIDSeq separately prevents collisions across BTW re-entry.
	btwRunIDGap = 1_000_000

	btwContextBoundary = `You are in an isolated side conversation. The inherited session history is reference context only.
Do not continue or complete earlier tasks, plans, tool calls, approvals, or requests. Only instructions submitted after this boundary are active.
Answer questions and explore lightly. Do not modify workspace state unless the user explicitly asks for that mutation in this side conversation.
Nothing from this side conversation will be merged into the main session.`
)

// btwState keeps the main surface alive while an isolated side conversation is
// visible. The parent model continues receiving its own run messages, but its
// transcript and session events stay hidden until the user returns.
type btwState struct {
	active           bool
	parent           *model
	sideRunIDBase    int
	parentNeedsInput bool
}

func (m model) handleBTWCommand(question string) (model, tea.Cmd) {
	question = strings.TrimSpace(question)
	if m.btw.active {
		if question != "" {
			return m.appendSystemNotice("A BTW conversation is already active. Run /btw with no question to return to the main session."), nil
		}
		return m.leaveBTW()
	}
	if m.compactInFlight {
		return m.appendSystemNotice("Compaction is running. Wait for it to finish before opening a BTW conversation."), nil
	}
	if m.exiting {
		return m.appendSystemNotice("Zero is waiting for the current run to finish saving before exit. A BTW conversation cannot start now."), nil
	}
	if m.activeSession.SessionID == "" {
		return m.appendSystemNotice("Start the main session with a prompt before opening a BTW conversation."), nil
	}
	if m.sessionStore == nil {
		return m.appendSystemNotice("BTW conversation unavailable: no session store configured."), nil
	}

	fork, err := m.sessionStore.Fork(m.activeSession.SessionID, sessions.ForkInput{
		SessionKind: sessions.SessionKindSide,
		Title:       btwTitle(m.activeSession.Title),
		Cwd:         m.cwd,
		ModelID:     m.modelName,
		Provider:    m.providerName,
		Tag:         btwSessionTag,
	})
	if err != nil {
		return m.appendSystemNotice("Could not open BTW conversation: " + err.Error()), nil
	}
	events, err := m.resumeEvents(fork.SessionID)
	if err != nil {
		return m.appendSystemNotice("Could not load BTW conversation: " + err.Error()), nil
	}

	// Foreground loops are session-scoped. Stop them only after the side session
	// is ready, so a failed fork cannot destroy work in the main session.
	parent := m
	clearedLoops := 0
	if updated, cleared := parent.clearLoopsForSessionSwitch(); cleared > 0 {
		parent = updated
		clearedLoops = cleared
		parent.transcript = reduceTranscript(parent.transcript, transcriptAction{
			kind: actionAppendSystem,
			text: fmt.Sprintf("Stopped %d loop(s) before opening the BTW conversation.", cleared),
		})
	}
	parent.btw = btwState{}
	parent.goalContinuationsSuspended = true
	// A scrollback print that was already scheduled may acknowledge after the
	// side surface is active. The hidden model does not receive that unscoped
	// acknowledgement, so clear its print latch and rebuild on return.
	parent.printInFlight = false
	parent.flushQueue = nil

	side := parent
	side.activeSession = fork
	side.sessionEvents = events
	side.transcript = initialTranscript()
	side.transcript = appendRow(side.transcript, rowSystem, "BTW conversation · isolated from the main session · /btw or Ctrl+C to return")
	if clearedLoops > 0 {
		side.transcript = appendRow(side.transcript, rowSystem, fmt.Sprintf("Stopped %d main-session loop(s) before opening this conversation.", clearedLoops))
	}
	side.transcript = appendTranscriptRowsDedup(side.transcript, transcriptRowsFromSessionEvents(events))
	side.printInFlight = false
	side.flushQueue = nil
	side.resetFlushFrontier("· btw conversation ·")
	side.pending = false
	side.exiting = false
	side.runCancel = nil
	side.activeRunID = 0
	side.runID = maxInt(parent.runID+btwRunIDGap, parent.btwRunIDSeq)
	side.flushRunIDs = map[int]string{}
	side.liveUsageCounts = map[int]int{}
	side.usageTracker = usage.NewTracker(usage.TrackerOptions{Registry: &side.modelCatalog, Now: side.now})
	side.lastUsage = usage.Normalized{}
	side.lastUsageSeen = false
	side.unpricedRequests = 0
	side.unpricedTokens = 0
	side.compactRequests = 0
	side.compactFrame = 0
	side.lastCompactResult = nil
	side.lastCompactError = ""
	side.turnLatencySum = 0
	side.turnLatencyCount = 0
	side.turnTTFTSum = 0
	side.turnTTFTCount = 0
	side.pendingPermission = nil
	side.pendingAskUser = nil
	side.pendingSpecReview = nil
	side.queuedMessage = ""
	side.lastPrompt = ""
	side.lastImages = nil
	side.lastImageLabels = nil
	side.lastDocuments = nil
	side.inputHistory = nil
	side.historyIdx = 0
	side.historyDraft = ""
	if question == "" {
		side.pendingImages = nil
		side.pendingImageLabels = nil
		side.pendingImageThumbnails = nil
		side.pendingDocuments = nil
	}
	side.loops = nil
	side.activeLoopID = ""
	side.loopTicking = false
	side.specialists.clear()
	side.plan.clear()
	side.planDetailGen++
	side.streamingText = nil
	side.streamingReasoning = ""
	side.streamingReasoningExpanded = false
	side.clearStreamingToolCall()
	side.resetStreamingFade()
	side.btw = btwState{
		active:        true,
		parent:        &parent,
		sideRunIDBase: side.runID,
	}

	if question == "" {
		return side, nil
	}
	return side.launchPrompt(question)
}

func (m model) leaveBTW() (model, tea.Cmd) {
	if !m.btw.active || m.btw.parent == nil {
		return m, nil
	}
	if m.pending {
		return m.appendSystemNotice("A BTW response is still running. Press Esc twice to cancel it, or wait for it to finish before returning."), nil
	}
	if len(m.flushRunIDs) > 0 {
		return m.appendSystemNotice("The cancelled BTW response is still saving its session events. Wait for it to finish before returning."), nil
	}
	if m.compactInFlight {
		return m.appendSystemNotice("BTW compaction is still running. Wait for it to finish before returning."), nil
	}
	m, _ = m.clearLoopsForSessionSwitch()
	parent := *m.btw.parent
	parent.goalContinuationsSuspended = false
	parent.btwRunIDSeq = maxInt(parent.btwRunIDSeq, m.runID)
	parent.btw = btwState{}
	// A hidden parent completion may have scheduled an unscoped git-sweep result
	// that landed on the side surface. Re-run it after restoring the parent so
	// its FILES sidebar cannot stay permanently marked in-flight or stale.
	parent.gitSweepInFlight = false
	var sweepCmd tea.Cmd
	parent, sweepCmd = parent.maybeGitSweep()
	// The side surface owns its own spinner tick chain. If the hidden parent is
	// still active, explicitly restart that chain after restoring it.
	parent.spinnerTicking = false
	var spinnerCmd tea.Cmd
	if parent.pending || parent.compactInFlight || parent.doctorInFlight || parent.sidebarHasAgents() || parent.aimlapiOnboardAnimating() {
		parent.spinnerTicking = true
		spinnerCmd = parent.spinner.Tick
	}
	parent.transcript = reduceTranscript(parent.transcript, transcriptAction{
		kind: actionAppendSystem,
		text: "Returned from the isolated BTW conversation. Its messages were not added to this session.",
	})
	parent.resetFlushFrontier("· returned from btw ·")
	var goalCmd tea.Cmd
	parent, goalCmd = parent.launchGoalContinuationIfReady()
	return parent, batchCommands(sweepCmd, spinnerCmd, goalCmd)
}

func btwCommandUnavailable(command parsedCommand) bool {
	arg := strings.ToLower(strings.TrimSpace(command.text))
	switch command.kind {
	case commandNew, commandResume, commandRename, commandSpec, commandLoop, commandGoal,
		commandRewind, commandCompact, commandSTTModel, commandMCP:
		return true
	case commandModel:
		return arg != "list" && arg != "ls"
	case commandProvider:
		return arg != "status"
	case commandTurns, commandProfile:
		return arg != "" && arg != "status"
	case commandTheme:
		return arg != "list"
	case commandConfig:
		return arg != ""
	default:
		return false
	}
}

// resizeBTWParent carries terminal geometry into the hidden parent. Window-size
// messages have no run ID, so normal BTW routing intentionally leaves them on
// the visible side surface; copying the layout fields prevents stale wrapping
// after the parent is restored.
func (m model) resizeBTWParent(msg tea.WindowSizeMsg) model {
	if !m.btw.active || m.btw.parent == nil {
		return m
	}
	parent := *m.btw.parent
	parent.width = msg.Width
	parent.height = msg.Height
	parent = parent.clearHover()
	parent.lineAges = nil
	parent.lastStreamActivity = parent.now()
	parent.input.SetWidth(maxInt(20, chatWidth(msg.Width)-14))
	m.btw.parent = &parent
	return m
}

func btwTitle(parent string) string {
	parent = strings.TrimSpace(parent)
	if parent == "" {
		return "BTW conversation"
	}
	return parent + " (BTW)"
}

// routeBTWParentMessage lets a main-session run finish while the side surface is
// active. Side run IDs start far above the captured parent's counter, so messages
// below that boundary unambiguously belong to the hidden parent model.
func (m model) routeBTWParentMessage(msg tea.Msg) (model, tea.Cmd, bool) {
	if !m.btw.active || m.btw.parent == nil {
		return m, nil, false
	}
	if title, ok := msg.(sessionTitleGeneratedMsg); ok {
		if title.sessionID != m.btw.parent.activeSession.SessionID {
			return m, nil, false
		}
		return m.routeBTWMessageToParent(msg)
	}
	runID, ok := btwMessageRunID(msg)
	if !ok || runID <= 0 || runID >= m.btw.sideRunIDBase {
		return m, nil, false
	}
	return m.routeBTWMessageToParent(msg)
}

func (m model) routeBTWMessageToParent(msg tea.Msg) (model, tea.Cmd, bool) {
	parentNext, cmd := m.btw.parent.updateModel(msg)
	parent, ok := parentNext.(model)
	if !ok {
		return m, cmd, true
	}
	parent.btw = btwState{}
	m.btw.parent = &parent
	switch msg.(type) {
	case permissionRequestMsg, askUserRequestMsg:
		if !m.btw.parentNeedsInput {
			m.btw.parentNeedsInput = true
			m = m.appendSystemNotice("The main session needs your input. Return with /btw or Ctrl+C to respond.")
		}
	case agentResponseMsg:
		m.btw.parentNeedsInput = parent.pendingPermission != nil || parent.pendingAskUser != nil
	}
	return m, cmd, true
}

func btwMessageRunID(msg tea.Msg) (int, bool) {
	switch typed := msg.(type) {
	case agentTextMsg:
		return typed.runID, true
	case agentReasoningMsg:
		return typed.runID, true
	case agentUsageMsg:
		return typed.runID, true
	case agentResponseMsg:
		return typed.runID, true
	case agentRowMsg:
		return typed.runID, true
	case toolCallStreamStartMsg:
		return typed.runID, true
	case interactiveExecStartMsg:
		return typed.runID, true
	case terminalAutoAttachTickMsg:
		return typed.runID, true
	case toolCallStreamDeltaMsg:
		return typed.runID, true
	case planUpdateMsg:
		return typed.runID, true
	case specialistStartMsg:
		return typed.runID, true
	case specialistCompleteMsg:
		return typed.runID, true
	case specialistProgressMsg:
		return typed.runID, true
	case swarmSessionsMsg:
		return typed.runID, true
	case permissionRequestMsg:
		return typed.runID, true
	case askUserRequestMsg:
		return typed.runID, true
	case recapGeneratedMsg:
		return typed.runID, true
	default:
		return 0, false
	}
}
