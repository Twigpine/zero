package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/errhint"
	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

const goalContinuationPrompt = "Continue pursuing the active goal. Review the existing session context, make concrete progress, and use update_goal when the objective is complete or genuinely blocked."

func (m model) handleGoalCommand(args string) (model, tea.Cmd) {
	action, rest := splitGoalCommand(args)
	if rest != "" && (action == "status" || action == "pause" || action == "resume" || action == "clear") {
		return m.appendGoalError("Usage: /goal " + action), nil
	}
	switch action {
	case "status":
		return m.appendGoalStatus(), nil
	case "pause":
		if m.activeSession.Goal == nil {
			return m.appendGoalError("No goal is set for this session."), nil
		}
		if m.pending {
			m.cancelRun()
			return m, nil
		}
		return m.setGoalStatus(sessions.GoalStatusPaused, "paused by user"), nil
	case "resume":
		if m.pending {
			return m.appendGoalError("A run is already in progress."), nil
		}
		if m.activeSession.Goal == nil {
			return m.appendGoalError("No goal is set for this session."), nil
		}
		if m.activeSession.Goal.Status == sessions.GoalStatusBudgetLimited &&
			m.activeSession.Goal.TokenBudget > 0 &&
			m.activeSession.Goal.TokensUsed >= m.activeSession.Goal.TokenBudget {
			return m.appendGoalError("The token budget is exhausted. Increase it with /goal edit --tokens N <objective>."), nil
		}
		m = m.setGoalStatus(sessions.GoalStatusActive, "")
		return m.launchGoalContinuationIfReady()
	case "clear":
		if m.activeSession.Goal == nil {
			return m.appendGoalError("No goal is set for this session."), nil
		}
		if m.pending {
			m.cancelRun()
		}
		updated, event, err := m.sessionStore.ClearGoal(m.activeSession.SessionID)
		if err != nil {
			return m.appendGoalError(err.Error()), nil
		}
		m.activeSession = updated
		m.sessionEvents = append(m.sessionEvents, event)
		m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowSystem, text: "Goal cleared."})
		return m, nil
	case "edit":
		objective, budget, err := parseGoalObjective(rest)
		if err != nil {
			return m.appendGoalError(err.Error()), nil
		}
		if m.activeSession.Goal == nil {
			return m.appendGoalError("No goal is set for this session."), nil
		}
		if m.pending {
			return m.appendGoalError("Pause the current run before editing its goal."), nil
		}
		if !goalBudgetSpecified(rest) {
			budget = m.activeSession.Goal.TokenBudget
		}
		updated, event, err := m.sessionStore.EditGoal(m.activeSession.SessionID, objective, budget)
		if err != nil {
			return m.appendGoalError(err.Error()), nil
		}
		m.activeSession = updated
		m.sessionEvents = append(m.sessionEvents, event)
		message := "Goal updated and resumed: " + objective
		if updated.Goal.Status == sessions.GoalStatusBudgetLimited {
			message = "Goal updated, but its token budget is still exhausted. Increase the budget or use --tokens 0 for no goal-specific limit."
		}
		m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowSystem, text: message})
		return m.launchGoalContinuationIfReady()
	case "create":
		if m.pending {
			return m.appendGoalError("A run is already in progress."), nil
		}
		objective, budget, err := parseGoalObjective(rest)
		if err != nil {
			return m.appendGoalError(err.Error()), nil
		}
		if m.activeSession.Goal != nil {
			return m.appendGoalError("This session already has a goal. Use /goal edit <objective> or /goal clear first."), nil
		}
		m, err = m.ensureActiveSession(objective)
		if err != nil {
			return m.appendGoalError("session create error: " + err.Error()), nil
		}
		updated, event, err := m.sessionStore.CreateGoal(m.activeSession.SessionID, objective, budget)
		if err != nil {
			return m.appendGoalError(err.Error()), nil
		}
		m.activeSession = updated
		m.sessionEvents = append(m.sessionEvents, event)
		return m.launchPrompt(objective)
	default:
		return m.appendGoalError("Unknown action. Use /goal, /goal pause, /goal resume, /goal edit <objective>, or /goal clear."), nil
	}
}

func splitGoalCommand(args string) (string, string) {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" || strings.EqualFold(trimmed, "status") {
		return "status", ""
	}
	first, rest, _ := strings.Cut(trimmed, " ")
	switch strings.ToLower(first) {
	case "status", "pause", "resume", "clear":
		return strings.ToLower(first), strings.TrimSpace(rest)
	case "edit":
		return "edit", strings.TrimSpace(rest)
	default:
		return "create", trimmed
	}
}

func parseGoalObjective(input string) (string, int, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", 0, fmt.Errorf("goal objective is required")
	}
	budget := 0
	if strings.HasPrefix(input, "--tokens") {
		fields := strings.Fields(input)
		if len(fields) < 3 || fields[0] != "--tokens" {
			return "", 0, fmt.Errorf("usage: /goal [--tokens N] <objective>")
		}
		value, err := strconv.Atoi(fields[1])
		if err != nil || value < 0 {
			return "", 0, fmt.Errorf("goal token budget must be a non-negative integer")
		}
		budget = value
		input = strings.TrimSpace(strings.Join(fields[2:], " "))
	}
	if input == "" {
		return "", 0, fmt.Errorf("goal objective is required")
	}
	return input, budget, nil
}

func goalBudgetSpecified(input string) bool {
	return strings.HasPrefix(strings.TrimSpace(input), "--tokens")
}

func (m model) appendGoalStatus() model {
	goal := m.activeSession.Goal
	if goal == nil {
		m.transcript = appendTranscriptRow(m.transcript, transcriptRow{
			kind: rowSystem,
			text: "Goal\nstatus: none\nStart one with /goal <objective>.",
		})
		return m
	}
	budget := "unlimited"
	if goal.TokenBudget > 0 {
		budget = fmt.Sprintf("%d / %d tokens", goal.TokensUsed, goal.TokenBudget)
	} else if goal.TokensUsed > 0 {
		budget = fmt.Sprintf("%d tokens used", goal.TokensUsed)
	}
	lines := []string{
		"Goal",
		"status: " + string(goal.Status),
		"objective: " + goal.Objective,
		"budget: " + budget,
		fmt.Sprintf(
			"automatic continuations: %d / %d",
			goal.ContinuationCount,
			goalContinuationLimit(goal),
		),
	}
	if goal.StatusReason != "" {
		lines = append(lines, "reason: "+goal.StatusReason)
	}
	m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowSystem, text: strings.Join(lines, "\n")})
	return m
}

func goalContinuationLimit(goal *sessions.Goal) int {
	if goal != nil && goal.ContinuationLimit > 0 {
		return goal.ContinuationLimit
	}
	return sessions.GoalMaxConsecutiveContinuations
}

func (m model) goalFooterSummary() string {
	if m.activeSession.Goal == nil {
		return ""
	}
	return "goal " + string(m.activeSession.Goal.Status)
}

func (m model) appendGoalError(message string) model {
	m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowError, text: "Goal: " + message})
	return m
}

func (m model) setGoalStatus(status sessions.GoalStatus, reason string) model {
	if m.sessionStore == nil || m.activeSession.SessionID == "" || m.activeSession.Goal == nil {
		return m.appendGoalError("Goal state is unavailable.")
	}
	updated, event, err := m.sessionStore.UpdateGoal(m.activeSession.SessionID, status, reason)
	if err != nil {
		return m.appendGoalError(err.Error())
	}
	m.activeSession = updated
	m.sessionEvents = append(m.sessionEvents, event)
	m.transcript = appendTranscriptRow(m.transcript, transcriptRow{
		kind: rowSystem,
		text: "Goal " + string(status) + ".",
	})
	return m
}

func (m model) goalRegistry() *tools.Registry {
	registry := cloneToolRegistry(m.registry)
	if m.activeSession.SessionID == "" {
		return registry
	}
	for _, tool := range tools.NewGoalTools(m.sessionStore, m.activeSession.SessionID) {
		registry.Register(tool)
	}
	return registry
}

func (m model) goalSystemPrompt(base string) string {
	goal := m.activeSession.Goal
	if goal == nil {
		return base
	}
	instruction := fmt.Sprintf(
		"Persistent goal for this session:\nObjective: %s\nStatus: %s\n"+
			"When the status is active, keep pursuing this objective across turns. "+
			"Call update_goal with complete only after the objective is genuinely achieved, "+
			"or blocked with a concrete reason when progress requires user input or an external change.",
		goal.Objective,
		goal.Status,
	)
	if strings.TrimSpace(base) == "" {
		return instruction
	}
	return base + "\n\n" + instruction
}

func (m model) launchGoalContinuationIfReady() (model, tea.Cmd) {
	goal := m.activeSession.Goal
	if goal == nil || goal.Status != sessions.GoalStatusActive || m.pending ||
		m.compactInFlight || m.exiting || m.provider == nil ||
		m.goalContinuationsSuspended {
		return m, nil
	}
	updated, event, reserved, err := m.sessionStore.ReserveGoalContinuation(m.activeSession.SessionID)
	if err != nil {
		return m.appendGoalError("reserve automatic continuation: " + err.Error()), nil
	}
	m.activeSession = updated
	if event != nil {
		m.sessionEvents = append(m.sessionEvents, *event)
	}
	if !reserved {
		if event != nil && updated.Goal != nil && updated.Goal.Status == sessions.GoalStatusPaused {
			m.transcript = appendTranscriptRow(m.transcript, transcriptRow{
				kind: rowSystem,
				text: fmt.Sprintf(
					"Goal paused after %d automatic continuations. Review progress, then use /goal resume to continue.",
					goalContinuationLimit(updated.Goal),
				),
			})
		}
		return m, nil
	}
	goal = updated.Goal
	m.transcript = appendTranscriptRow(m.transcript, transcriptRow{
		kind: rowSystem,
		text: "Continuing goal: " + goal.Objective,
	})
	prompt := m.sessionPrompt(goalContinuationPrompt)
	m, err = m.appendSessionEvent(sessions.EventMessage, map[string]any{
		"role":    "goal",
		"content": goalContinuationPrompt,
	})
	if err != nil {
		return m.appendGoalError("session record error: " + err.Error()), nil
	}
	runCtx, cancel := context.WithCancel(m.ctx)
	m = m.beginRun(cancel)
	return m, tea.Batch(m.runAgent(m.activeRunID, runCtx, prompt, nil), m.spinner.Tick)
}

func (m model) reconcileGoalAfterRun(usageEvents []zeroruntime.Usage, runErr error) model {
	if m.sessionStore == nil || m.activeSession.SessionID == "" {
		return m
	}
	loaded, err := m.sessionStore.Get(m.activeSession.SessionID)
	if err != nil || loaded == nil {
		if err != nil {
			return m.appendGoalError("reload after run: " + err.Error())
		}
		return m
	}
	if loaded.Goal == nil {
		return m
	}
	tokens := 0
	for _, event := range usageEvents {
		tokens += event.TotalTokens()
	}
	if tokens > 0 {
		updated, _, addErr := m.sessionStore.AddGoalUsage(loaded.SessionID, tokens)
		if addErr != nil {
			return m.appendGoalError("account usage: " + addErr.Error())
		}
		loaded = &updated
	}
	if runErr != nil && loaded.Goal.Status == sessions.GoalStatusActive {
		status := sessions.GoalStatusPaused
		reason := "run stopped after an error"
		if errhint.Classify(runErr) == errhint.RateLimit {
			status = sessions.GoalStatusUsageLimited
			reason = "provider usage limit reached"
		}
		updated, _, updateErr := m.sessionStore.UpdateGoal(
			loaded.SessionID,
			status,
			reason,
		)
		if updateErr != nil {
			return m.appendGoalError("pause after error: " + updateErr.Error())
		}
		loaded = &updated
		message := "Goal paused because the run stopped with an error. Use /goal resume to continue."
		if status == sessions.GoalStatusUsageLimited {
			message = "Goal stopped at the provider usage limit. Use /goal resume when capacity is available."
		}
		m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowSystem, text: message})
	}
	m.activeSession = *loaded
	if events, readErr := m.sessionStore.ReadEvents(loaded.SessionID); readErr == nil {
		m.sessionEvents = events
	}
	return m
}
