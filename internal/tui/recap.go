package tui

import (
	"context"
	"errors"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

const (
	// recapIdleDelay keeps recap generation off the critical path of ordinary
	// turns. A recap is useful when returning to a quiet session, not immediately
	// after reading the answer that it would otherwise repeat.
	recapIdleDelay = 4 * time.Minute
	// recapTimeout bounds a single recap generation so a hung provider can never
	// wedge the background command.
	recapTimeout = 30 * time.Second
	// recapMaxContextChars bounds the recent conversation excerpt sent to the
	// provider. Goal and plan hints are added separately.
	recapMaxContextChars = 4800
	recapMaxRows         = 8
)

const recapSystemPrompt = "The user is returning to an idle coding session. In under 40 words and 1-2 plain sentences, state the overall goal or current task, then the next actionable step. Reply with only the recap: no markdown, preamble, or trailing question."

// recapIdleMsg marks the end of an inactivity window. seq makes abandoned
// timers cheap: any subsequent user activity or run invalidates the message.
type recapIdleMsg struct {
	runID int
	seq   int
}

// recapGeneratedMsg carries the outcome of a background recap generation back to
// the Update loop.
type recapGeneratedMsg struct {
	runID int
	seq   int
	recap string
	err   error
}

// cleanGeneratedRecap normalizes a raw model response into a single recap line:
// the first non-empty line, surrounding markup stripped.
func cleanGeneratedRecap(raw string) string {
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if trimmed := strings.Trim(line, " \t\r\n\"'`*#"); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// generateRecap asks the provider for a compact orientation note using bounded
// session context. It is an isolated side request with no tools.
func generateRecap(ctx context.Context, provider zeroruntime.Provider, sessionContext string) (string, error) {
	if provider == nil {
		return "", errors.New("no provider configured")
	}
	sessionContext = strings.TrimSpace(sessionContext)
	if sessionContext == "" {
		return "", errors.New("no session context to recap")
	}
	request := zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{
			{Role: zeroruntime.MessageRoleSystem, Content: recapSystemPrompt},
			{Role: zeroruntime.MessageRoleUser, Content: sessionContext},
		},
	}
	stream, err := provider.StreamCompletion(ctx, request)
	if err != nil {
		return "", err
	}
	collected := zeroruntime.CollectStreamWithOptions(ctx, stream, zeroruntime.CollectOptions{})
	if collected.Error != "" {
		return "", errors.New(collected.Error)
	}
	recap := cleanGeneratedRecap(collected.Text)
	if recap == "" {
		return "", errors.New("model returned no usable recap")
	}
	return recap, nil
}

// recapContext builds the bounded orientation material used by the idle recap.
// Goal and plan state are authoritative; recent user/final-assistant rows fill
// in enough conversational context when neither exists.
func (m model) recapContext() string {
	var sections []string
	if goal := m.activeSession.Goal; goal != nil && strings.TrimSpace(goal.Objective) != "" {
		sections = append(sections, "Overall goal: "+strings.TrimSpace(goal.Objective)+"\nGoal status: "+string(goal.Status))
	}
	if current := strings.TrimSpace(currentStepContent(m.plan.steps)); current != "" {
		sections = append(sections, "Current task: "+current)
	}

	rows := make([]string, 0, recapMaxRows)
	used := 0
	for i := len(m.transcript) - 1; i >= 0 && len(rows) < recapMaxRows && used < recapMaxContextChars; i-- {
		row := m.transcript[i]
		role := ""
		switch {
		case row.kind == rowUser:
			role = "User"
		case row.kind == rowAssistant && row.final:
			role = "Assistant"
		default:
			continue
		}
		content := strings.TrimSpace(row.text)
		if content == "" {
			continue
		}
		content = cutRunes(content, recapMaxContextChars-used)
		rows = append(rows, role+": "+content)
		used += len([]rune(content))
	}
	for left, right := 0, len(rows)-1; left < right; left, right = left+1, right-1 {
		rows[left], rows[right] = rows[right], rows[left]
	}
	if len(rows) > 0 {
		sections = append(sections, "Recent conversation:\n"+strings.Join(rows, "\n"))
	}
	return strings.Join(sections, "\n\n")
}

// generateRecapCmd builds the cancellable background command that generates an
// idle recap off the Update goroutine.
func (m model) generateRecapCmd(ctx context.Context, cancel context.CancelFunc, runID, seq int) tea.Cmd {
	provider := m.provider
	sessionContext := m.recapContext()
	return func() tea.Msg {
		defer cancel()
		recap, err := generateRecap(ctx, provider, sessionContext)
		return recapGeneratedMsg{runID: runID, seq: seq, recap: recap, err: err}
	}
}

// maybeScheduleIdleRecap arms a one-shot inactivity window after a successful
// turn. It performs no provider work until that window actually elapses.
func (m model) maybeScheduleIdleRecap(runID int, answer string) (model, tea.Cmd) {
	if !m.recapsEnabled || m.provider == nil || strings.TrimSpace(answer) == "" {
		return m, nil
	}
	m.recapIdleArmed = true
	m.recapIdleRunID = runID
	return m.scheduleIdleRecap(runID)
}

func (m model) scheduleIdleRecap(runID int) (model, tea.Cmd) {
	m.recapSeq++
	seq := m.recapSeq
	ctx, cancel := context.WithCancel(context.Background())
	m.recapTimerCancel = cancel
	return m, func() tea.Msg {
		timer := time.NewTimer(recapIdleDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
			return recapIdleMsg{runID: runID, seq: seq}
		case <-ctx.Done():
			return nil
		}
	}
}

// cancelIdleRecap invalidates pending timers, aborts provider work, and clears
// the ephemeral orientation note as soon as the user resumes activity.
func (m model) cancelIdleRecap() model {
	m.recapIdleArmed = false
	m.recapIdleRunID = 0
	return m.clearIdleRecapWork()
}

func (m model) clearIdleRecapWork() model {
	m.recapSeq++
	if m.recapTimerCancel != nil {
		m.recapTimerCancel()
		m.recapTimerCancel = nil
	}
	if m.recapCancel != nil {
		m.recapCancel()
		m.recapCancel = nil
	}
	m.recapRunning = false
	m.idleRecap = ""
	return m
}

// resetIdleRecapAfterActivity restarts the inactivity window for a completed
// turn. Activity still cancels in-flight generation and clears the ephemeral
// note, but scrolling or reading no longer disables recaps until another turn.
func (m model) resetIdleRecapAfterActivity() (model, tea.Cmd) {
	armed := m.recapIdleArmed
	runID := m.recapIdleRunID
	m = m.clearIdleRecapWork()
	if !armed || !m.recapsEnabled || m.provider == nil || runID != m.runID {
		return m, nil
	}
	return m.scheduleIdleRecap(runID)
}

// handleRecapIdle starts generation only if the same run is still genuinely
// idle. Busy UI, a draft, a modal, or newer activity silently abandons it.
func (m model) handleRecapIdle(msg recapIdleMsg) (model, tea.Cmd) {
	if msg.seq != m.recapSeq || msg.runID != m.runID || !m.recapsEnabled ||
		m.provider == nil || m.pending || m.compactInFlight || m.recapRunning ||
		strings.TrimSpace(m.composerValue()) != "" || !m.noBlockingModal() {
		return m, nil
	}
	if strings.TrimSpace(m.recapContext()) == "" {
		return m, nil
	}
	m.recapTimerCancel = nil
	ctx, cancel := context.WithTimeout(context.Background(), recapTimeout)
	m.recapCancel = cancel
	m.recapRunning = true
	return m, m.generateRecapCmd(ctx, cancel, msg.runID, msg.seq)
}

// handleRecapGenerated publishes an ephemeral orientation note above the
// composer. Stale, cancelled, failed, or now-busy results are silent.
func (m model) handleRecapGenerated(msg recapGeneratedMsg) (model, tea.Cmd) {
	if msg.seq != m.recapSeq || msg.runID != m.runID {
		return m, nil
	}
	m.recapCancel = nil
	m.recapRunning = false
	m.recapIdleArmed = false
	m.recapIdleRunID = 0
	recap := strings.TrimSpace(msg.recap)
	if msg.err != nil || recap == "" || m.pending || m.compactInFlight ||
		strings.TrimSpace(m.composerValue()) != "" || !m.noBlockingModal() {
		return m, nil
	}
	m.idleRecap = recap
	return m, nil
}

// handleConfigCommand applies a "/config <setting> <value>" toggle and returns a
// status string. Currently only "recaps on|off". A failed persist surfaces an
// error instead of falsely reporting success.
func (m model) handleConfigCommand(arg string) (model, string) {
	switch arg {
	case "recaps on", "recaps off":
		m.recapsEnabled = arg == "recaps on"
		if !m.recapsEnabled {
			m = m.cancelIdleRecap()
		}
		if err := m.persistRecapsEnabled(); err != nil {
			return m, "Config\nFailed to save recaps preference: " + err.Error()
		}
		return m, "Config\nrecaps: " + onOff(m.recapsEnabled)
	default:
		return m, "Config\nUnknown setting: " + arg + " (use: recaps on|off)"
	}
}

// persistRecapsEnabled writes the recap preference to the user config, mirroring
// persistFavoriteModels. A no-op when there is no user config path.
func (m model) persistRecapsEnabled() error {
	if strings.TrimSpace(m.userConfigPath) == "" {
		return nil
	}
	_, err := config.SetRecapsEnabled(m.userConfigPath, m.recapsEnabled)
	return err
}
