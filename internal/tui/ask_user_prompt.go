package tui

import (
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/agent"
)

// ask_user_prompt.go drives the interactive ask-user questionnaire that renders in
// the composer region. A multi-question prompt is a row of tabs (one per question
// plus a trailing Confirm tab): Tab/Shift+Tab switch questions, answers are given in
// any order, and Confirm submits them all through the loop's answer channel. A
// single-select question with options is a picker (↑↓ + Enter, cursor on the
// recommended option) with a trailing "type your own" entry that drops into the
// composer free-text; a multi-select or open-ended question is plain free-text.

const askUserTypeMyOwnLabel = "Type your own answer"

// askUserAnswerState is the per-question state in a multi-question prompt.
type askUserAnswerState struct {
	cursor   int    // selector index over [options..., type-my-own]
	typing   bool   // free-text mode for this question
	typed    string // free-text preserved across tab switches
	answer   string // committed answer ("" until answered)
	answered bool
}

// newAskUserStates builds the initial per-question state: single-select questions
// with options open in the picker resting on the recommended option; multi-select
// and open-ended questions start in free-text.
func newAskUserStates(questions []agent.AskUserQuestion) []askUserAnswerState {
	states := make([]askUserAnswerState, len(questions))
	for index, question := range questions {
		if len(question.Options) == 0 || question.MultiSelect {
			states[index].typing = true
			continue
		}
		states[index].cursor = recommendedAskUserIndex(question)
	}
	return states
}

// recommendedAskUserIndex is the cursor the picker rests on by default: the
// recommended option when it matches one, else the first option.
func recommendedAskUserIndex(question agent.AskUserQuestion) int {
	if question.Recommended == "" {
		return 0
	}
	for index, option := range question.Options {
		if option == question.Recommended {
			return index
		}
	}
	return 0
}

// askUserSelectableCount is the options count plus the trailing "type your own" row.
func askUserSelectableCount(question agent.AskUserQuestion) int {
	return len(question.Options) + 1
}

func clampAskUserCursor(cursor, n int) int {
	if n <= 0 {
		return 0
	}
	if cursor < 0 {
		return 0
	}
	if cursor >= n {
		return n - 1
	}
	return cursor
}

// confirmTabIndex is the index of the trailing Confirm tab (== number of questions).
func (p *pendingAskUserPrompt) confirmTabIndex() int {
	return len(p.request.Questions)
}

// onConfirmTab reports whether the active tab is the Confirm tab.
func (p *pendingAskUserPrompt) onConfirmTab() bool {
	return p.active >= len(p.request.Questions)
}

// activeQuestion returns the active question and its state when the active tab is a
// question (not the Confirm tab).
func (p *pendingAskUserPrompt) activeQuestion() (agent.AskUserQuestion, *askUserAnswerState, bool) {
	if p == nil || p.active < 0 || p.active >= len(p.request.Questions) || p.active >= len(p.states) {
		return agent.AskUserQuestion{}, nil, false
	}
	return p.request.Questions[p.active], &p.states[p.active], true
}

// askUserSwitchTab persists the current question's typed text, moves the active tab
// to target (wrapping over [0, confirmTab]), and loads the target's typed text into
// the composer input (or clears it for a picker / the Confirm tab).
func (m model) askUserSwitchTab(target int) model {
	pending := m.pendingAskUser
	if pending == nil {
		return m
	}
	confirm := pending.confirmTabIndex()
	switch {
	case target < 0:
		target = confirm
	case target > confirm:
		target = 0
	}
	if _, state, ok := pending.activeQuestion(); ok && state.typing {
		state.typed = m.input.Value()
	}
	pending.active = target
	if target < confirm && pending.states[target].typing {
		m.input.SetValue(pending.states[target].typed)
	} else {
		m.input.SetValue("")
	}
	return m
}

// moveAskUserTab cycles the active tab by delta (Tab / Shift+Tab). A no-op for a
// single-question prompt, which has no tab strip / Confirm tab — so Tab can't move
// it into the hidden Confirm state.
func (m model) moveAskUserTab(delta int) model {
	if m.pendingAskUser == nil || len(m.pendingAskUser.request.Questions) <= 1 {
		return m
	}
	return m.askUserSwitchTab(m.pendingAskUser.active + delta)
}

// moveAskUserCursor moves the option cursor for the active question (picker mode).
func (m model) moveAskUserCursor(delta int) model {
	pending := m.pendingAskUser
	if pending == nil {
		return m
	}
	question, state, ok := pending.activeQuestion()
	if !ok || state.typing {
		return m
	}
	n := askUserSelectableCount(question)
	cursor := (clampAskUserCursor(state.cursor, n) + delta) % n
	if cursor < 0 {
		cursor += n
	}
	state.cursor = cursor
	return m
}

// confirmAskUser handles Enter: on a question it commits the highlighted option or
// the typed text and advances to the next tab (or switches to free-text when "type
// your own" is chosen); on the Confirm tab it submits all answers.
func (m model) confirmAskUser() (tea.Model, tea.Cmd) {
	pending := m.pendingAskUser
	if pending == nil {
		return m, nil
	}
	if pending.onConfirmTab() {
		return m.submitAskUser()
	}
	question, state, _ := pending.activeQuestion()
	if state.typing {
		m.recordActiveAnswer(strings.TrimSpace(m.input.Value()))
		return m.afterAskUserAnswer()
	}
	cursor := clampAskUserCursor(state.cursor, askUserSelectableCount(question))
	if cursor >= len(question.Options) {
		// "type your own" — switch this question into free-text.
		state.typing = true
		m.input.SetValue(state.typed)
		return m, nil
	}
	m.recordActiveAnswer(question.Options[cursor])
	return m.afterAskUserAnswer()
}

// afterAskUserAnswer runs after an answer is committed: a single-question prompt
// submits immediately (no Confirm step); a multi-question prompt advances to the
// next tab (landing on Confirm after the last question).
func (m model) afterAskUserAnswer() (tea.Model, tea.Cmd) {
	if m.pendingAskUser == nil {
		return m, nil
	}
	if len(m.pendingAskUser.request.Questions) <= 1 {
		return m.submitAskUser()
	}
	return m.advanceAskUserTab(), nil
}

// recordActiveAnswer commits an answer for the active question.
func (m model) recordActiveAnswer(answer string) {
	if _, state, ok := m.pendingAskUser.activeQuestion(); ok {
		state.answer = answer
		state.answered = true
	}
}

// advanceAskUserTab moves to the next tab after committing an answer (the Confirm
// tab follows the last question), so answering straight through lands on Confirm.
func (m model) advanceAskUserTab() model {
	if m.pendingAskUser == nil {
		return m
	}
	return m.askUserSwitchTab(m.pendingAskUser.active + 1)
}

// escapeAskUser handles Esc: from a single-select "type your own" it steps back to
// that question's picker; otherwise it dismisses the questionnaire, delivering
// whatever has been answered so the agent loop unblocks (unanswered stay empty).
func (m model) escapeAskUser() (tea.Model, tea.Cmd) {
	pending := m.pendingAskUser
	if pending == nil {
		return m, nil
	}
	if question, state, ok := pending.activeQuestion(); ok && state.typing && !question.MultiSelect && len(question.Options) > 0 {
		state.typing = false
		state.typed = ""
		state.cursor = clampAskUserCursor(state.cursor, askUserSelectableCount(question))
		m.input.SetValue("")
		return m, nil
	}
	return m.submitAskUser()
}

// submitAskUser delivers the committed answers (one per question, "" for any left
// unanswered) through the loop's answer channel and restores the composer.
func (m model) submitAskUser() (tea.Model, tea.Cmd) {
	pending := m.pendingAskUser
	if pending == nil {
		return m, nil
	}
	answers := make([]string, len(pending.states))
	for index, state := range pending.states {
		answers[index] = state.answer
	}
	if pending.answer != nil {
		pending.answer(answers)
	}
	m.pendingAskUser = nil
	m.clearComposer()
	m.clearSuggestions()
	return m, nil
}

// askUserTabTitle is the short tab label for a question: its Header if set, else a
// trimmed-to-fit question, else a positional fallback.
func askUserTabTitle(question agent.AskUserQuestion, index int) string {
	if title := strings.TrimSpace(question.Header); title != "" {
		return title
	}
	question.Question = strings.TrimSpace(question.Question)
	if question.Question == "" {
		return "Question " + strconv.Itoa(index+1)
	}
	const maxLen = 18
	runes := []rune(question.Question)
	if len(runes) > maxLen {
		return strings.TrimSpace(string(runes[:maxLen])) + "…"
	}
	return question.Question
}
