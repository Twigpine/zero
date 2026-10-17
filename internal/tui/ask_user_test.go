package tui

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/agent"
)

// testAskUserRequest is a two-question request used by model_test.go too.
func testAskUserRequest() agent.AskUserRequest {
	return agent.AskUserRequest{
		ToolCallID: "call_1",
		Header:     "Need a couple of details",
		Questions: []agent.AskUserQuestion{
			{Question: "Which framework?", Options: []string{"React", "Vue"}},
			{Question: "TypeScript?"},
		},
	}
}

func newAskUserModel(t *testing.T, request agent.AskUserRequest, answers *[][]string) model {
	t.Helper()
	m := newModel(context.Background(), Options{})
	m.pending = true
	m.activeRunID = 7
	m.width = 96
	updated, _ := m.Update(askUserRequestMsg{
		runID:   7,
		request: request,
		answer:  func(values []string) { *answers = append(*answers, values) },
	})
	return updated.(model)
}

func askUserSingle(options []string, recommended string) agent.AskUserRequest {
	return agent.AskUserRequest{
		ToolCallID: "call_single",
		Questions:  []agent.AskUserQuestion{{Question: "Pick one", Options: options, Recommended: recommended}},
	}
}

func askUserTwoQuestions() agent.AskUserRequest {
	return agent.AskUserRequest{
		ToolCallID: "call_2q",
		Questions: []agent.AskUserQuestion{
			{Question: "Framework?", Header: "FW", Options: []string{"React", "Vue"}},
			{Question: "TypeScript?", Header: "TS", Options: []string{"Yes", "No"}},
		},
	}
}

// --- single question (no Confirm step) -------------------------------------

func TestAskUserSinglePickerDefaultsToRecommendedAndSubmits(t *testing.T) {
	var answers [][]string
	next := newAskUserModel(t, askUserSingle([]string{"Postgres", "SQLite", "MySQL"}, "SQLite"), &answers)

	if next.pendingAskUser == nil || next.pendingAskUser.states[0].typing {
		t.Fatalf("expected picker mode for a question with options, got %#v", next.pendingAskUser)
	}
	if next.pendingAskUser.states[0].cursor != 1 {
		t.Fatalf("expected cursor on the recommended option (index 1), got %d", next.pendingAskUser.states[0].cursor)
	}
	view := next.View()
	for _, want := range []string{"Postgres", "SQLite", "MySQL", "(recommended)", askUserTypeMyOwnLabel} {
		assertContains(t, view, want)
	}
	// A single question has no tab row / Confirm step.
	assertNotContains(t, view, "Confirm")

	updated, _ := next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if next.pendingAskUser != nil {
		t.Fatalf("single question should submit on selection, still pending: %#v", next.pendingAskUser)
	}
	if len(answers) != 1 || len(answers[0]) != 1 || answers[0][0] != "SQLite" {
		t.Fatalf("expected [SQLite], got %#v", answers)
	}
}

func TestAskUserSingleTypeMyOwnSubmitsTypedText(t *testing.T) {
	var answers [][]string
	next := newAskUserModel(t, askUserSingle([]string{"Postgres", "SQLite", "MySQL"}, "SQLite"), &answers)

	// Move from SQLite (1) to the "type your own" entry (index 3).
	for i := 0; i < 2; i++ {
		updated, _ := next.Update(testKey(tea.KeyDown))
		next = updated.(model)
	}
	if next.pendingAskUser.states[0].cursor != 3 {
		t.Fatalf("expected cursor on type-your-own (index 3), got %d", next.pendingAskUser.states[0].cursor)
	}
	updated, _ := next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if !next.pendingAskUser.states[0].typing {
		t.Fatal("expected 'type your own' to switch into free-text")
	}
	next.input.SetValue("CockroachDB")
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if len(answers) != 1 || answers[0][0] != "CockroachDB" {
		t.Fatalf("expected typed answer [CockroachDB], got %#v", answers)
	}
}

func TestAskUserTypingInPickerSwitchesToFreeText(t *testing.T) {
	var answers [][]string
	next := newAskUserModel(t, askUserSingle([]string{"Postgres", "SQLite", "MySQL"}, "SQLite"), &answers)

	updated, _ := next.Update(testKeyText("M"))
	next = updated.(model)
	if !next.pendingAskUser.states[0].typing {
		t.Fatal("a printable keystroke should switch the picker into free-text")
	}
	if next.input.Value() != "M" {
		t.Fatalf("the keystroke should be captured, got %q", next.input.Value())
	}
	next.input.SetValue("MariaDB")
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if len(answers) != 1 || answers[0][0] != "MariaDB" {
		t.Fatalf("expected typed answer [MariaDB], got %#v", answers)
	}
}

func TestAskUserSingleNoOptionsIsFreeText(t *testing.T) {
	var answers [][]string
	next := newAskUserModel(t, agent.AskUserRequest{
		ToolCallID: "c",
		Questions:  []agent.AskUserQuestion{{Question: "Describe the behavior"}},
	}, &answers)

	if next.pendingAskUser == nil || !next.pendingAskUser.states[0].typing {
		t.Fatalf("expected free-text for a no-options question, got %#v", next.pendingAskUser)
	}
	next.input.SetValue("free-form answer")
	updated, _ := next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if len(answers) != 1 || answers[0][0] != "free-form answer" {
		t.Fatalf("expected [free-form answer], got %#v", answers)
	}
}

func TestAskUserMultiSelectIsFreeTextWithSuggestions(t *testing.T) {
	var answers [][]string
	next := newAskUserModel(t, agent.AskUserRequest{
		ToolCallID: "c",
		Questions:  []agent.AskUserQuestion{{Question: "Which checks?", Options: []string{"lint", "test", "typecheck"}, MultiSelect: true}},
	}, &answers)

	if !next.pendingAskUser.states[0].typing {
		t.Fatalf("multi-select must use free-text, got %#v", next.pendingAskUser)
	}
	assertContains(t, next.View(), "suggested:")
	assertContains(t, next.View(), "lint")
	next.input.SetValue("lint, typecheck")
	updated, _ := next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if len(answers) != 1 || answers[0][0] != "lint, typecheck" {
		t.Fatalf("expected verbatim multi-answer, got %#v", answers)
	}
}

func TestAskUserTypeMyOwnEscReturnsToPicker(t *testing.T) {
	var answers [][]string
	next := newAskUserModel(t, askUserSingle([]string{"Postgres", "SQLite", "MySQL"}, "SQLite"), &answers)

	for i := 0; i < 2; i++ { // to type-your-own
		updated, _ := next.Update(testKey(tea.KeyDown))
		next = updated.(model)
	}
	updated, _ := next.Update(testKey(tea.KeyEnter)) // into free-text
	next = updated.(model)
	next.input.SetValue("scratch")

	updated, _ = next.Update(testKey(tea.KeyEsc)) // back to picker
	next = updated.(model)
	if next.pendingAskUser == nil {
		t.Fatal("Esc from type-your-own must not dismiss the prompt")
	}
	if next.pendingAskUser.states[0].typing {
		t.Fatal("Esc from type-your-own must return to the picker")
	}
	if len(answers) != 0 {
		t.Fatalf("Esc back to the picker must not deliver answers, got %#v", answers)
	}
	// Pick a real option from the picker.
	updated, _ = next.Update(testKey(tea.KeyUp)) // 3 -> 2 (MySQL)
	next = updated.(model)
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if len(answers) != 1 || answers[0][0] != "MySQL" {
		t.Fatalf("expected [MySQL] after returning to picker, got %#v", answers)
	}
}

// A single-question prompt has no tab strip / Confirm tab, so Tab / Shift+Tab must
// be no-ops (not advance into the hidden Confirm state).
func TestAskUserSingleQuestionTabIsNoOp(t *testing.T) {
	var answers [][]string
	next := newAskUserModel(t, askUserSingle([]string{"A", "B"}, "A"), &answers)
	if next.pendingAskUser.active != 0 {
		t.Fatalf("expected to start on the single question, active=%d", next.pendingAskUser.active)
	}
	updated, _ := next.Update(testKey(tea.KeyTab))
	next = updated.(model)
	if next.pendingAskUser == nil || next.pendingAskUser.active != 0 {
		t.Fatalf("Tab on a single-question prompt must be a no-op, active=%d", next.pendingAskUser.active)
	}
	updated, _ = next.Update(testKeyShift(tea.KeyTab))
	next = updated.(model)
	if next.pendingAskUser.active != 0 {
		t.Fatalf("Shift+Tab on a single-question prompt must be a no-op, active=%d", next.pendingAskUser.active)
	}
}

// --- multi-question (tabs + Confirm) ---------------------------------------

func TestAskUserMultiQuestionTabbedSubmit(t *testing.T) {
	var answers [][]string
	next := newAskUserModel(t, askUserTwoQuestions(), &answers)

	// Q1: select React (cursor 0), advances to Q2.
	updated, _ := next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if next.pendingAskUser == nil || next.pendingAskUser.active != 1 {
		t.Fatalf("expected to advance to Q2 (active=1), got %#v", next.pendingAskUser)
	}
	if len(answers) != 0 {
		t.Fatalf("must not deliver before Confirm, got %#v", answers)
	}
	// Q2: select Yes (cursor 0), advances to Confirm tab.
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if !next.pendingAskUser.onConfirmTab() {
		t.Fatalf("expected to land on the Confirm tab, active=%d", next.pendingAskUser.active)
	}
	assertContains(t, next.View(), "Review and submit")
	// Confirm: submit all.
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if next.pendingAskUser != nil {
		t.Fatalf("Confirm should submit, still pending: %#v", next.pendingAskUser)
	}
	if len(answers) != 1 || len(answers[0]) != 2 || answers[0][0] != "React" || answers[0][1] != "Yes" {
		t.Fatalf("expected [React Yes], got %#v", answers)
	}
}

func TestAskUserTabSwitchesQuestions(t *testing.T) {
	var answers [][]string
	next := newAskUserModel(t, askUserTwoQuestions(), &answers)

	if next.pendingAskUser.active != 0 {
		t.Fatalf("expected to start on Q1, got active=%d", next.pendingAskUser.active)
	}
	updated, _ := next.Update(testKey(tea.KeyTab))
	next = updated.(model)
	if next.pendingAskUser.active != 1 {
		t.Fatalf("Tab should move to Q2, got active=%d", next.pendingAskUser.active)
	}
	updated, _ = next.Update(testKey(tea.KeyTab))
	next = updated.(model)
	if !next.pendingAskUser.onConfirmTab() {
		t.Fatalf("Tab should move to the Confirm tab, got active=%d", next.pendingAskUser.active)
	}
	updated, _ = next.Update(testKey(tea.KeyTab))
	next = updated.(model)
	if next.pendingAskUser.active != 0 {
		t.Fatalf("Tab should wrap to Q1, got active=%d", next.pendingAskUser.active)
	}
	updated, _ = next.Update(testKeyShift(tea.KeyTab))
	next = updated.(model)
	if !next.pendingAskUser.onConfirmTab() {
		t.Fatalf("Shift+Tab should wrap back to the Confirm tab, got active=%d", next.pendingAskUser.active)
	}
}

func TestAskUserEscDismissDeliversPartialAnswers(t *testing.T) {
	var answers [][]string
	next := newAskUserModel(t, askUserTwoQuestions(), &answers)

	// Answer Q1, advance to Q2, then Esc (Q2 is a picker, so Esc dismisses).
	updated, _ := next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	updated, _ = next.Update(testKey(tea.KeyEsc))
	next = updated.(model)
	if next.pendingAskUser != nil {
		t.Fatalf("Esc on a picker should dismiss, still pending: %#v", next.pendingAskUser)
	}
	if len(answers) != 1 || len(answers[0]) != 2 || answers[0][0] != "React" || answers[0][1] != "" {
		t.Fatalf("expected partial [React, \"\"], got %#v", answers)
	}
	if !next.pending {
		t.Fatal("dismiss cancels only the questionnaire; the run keeps running")
	}
}

// --- rendering -------------------------------------------------------------

func TestAskUserMultiQuestionShowsTabsAndOptions(t *testing.T) {
	var answers [][]string
	next := newAskUserModel(t, agent.AskUserRequest{
		ToolCallID: "call_v",
		Questions: []agent.AskUserQuestion{
			{Question: "Which framework?", Header: "Framework", Options: []string{"React", "Vue"}},
			{Question: "TypeScript?", Header: "TypeScript"},
		},
	}, &answers)
	view := next.View()
	for _, want := range []string{"Framework", "TypeScript", "Confirm", "Which framework?", "React", "Vue"} {
		assertContains(t, view, want)
	}
}

func TestAskUserOptionDescriptionsRender(t *testing.T) {
	var answers [][]string
	next := newAskUserModel(t, agent.AskUserRequest{
		ToolCallID: "call_d",
		Questions: []agent.AskUserQuestion{{
			Question:           "Which decade?",
			Options:            []string{"1980s", "1990s"},
			OptionDescriptions: []string{"Synth-pop, hair metal", "Grunge, Britpop"},
			Recommended:        "1980s",
		}},
	}, &answers)
	view := next.View()
	for _, want := range []string{"1980s", "Synth-pop, hair metal", "1990s", "Grunge, Britpop"} {
		assertContains(t, view, want)
	}
}

// --- regressions preserved -------------------------------------------------

func TestAskUserPromptBlocksNormalSubmit(t *testing.T) {
	var answers [][]string
	next := newAskUserModel(t, askUserTwoQuestions(), &answers)
	next.input.SetValue("/help")

	updated, _ := next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if transcriptContains(next.transcript, "Available commands") {
		t.Fatalf("ask_user must capture Enter, not run commands: %#v", next.transcript)
	}
	if next.pendingAskUser == nil {
		t.Fatal("expected the prompt to remain pending after answering Q1 of 2")
	}
}

func TestAskUserRequestClearsComposerDraft(t *testing.T) {
	var answers [][]string
	m := newModel(context.Background(), Options{})
	m.pending = true
	m.activeRunID = 7
	m = typeRunes(t, m, "hidden followup")
	if !m.composerActive || m.composerValue() == "" {
		t.Fatalf("setup expected an active composer draft, got active=%v value=%q", m.composerActive, m.composerValue())
	}
	updated, _ := m.Update(askUserRequestMsg{
		runID:   7,
		request: agent.AskUserRequest{ToolCallID: "c", Questions: []agent.AskUserQuestion{{Question: "Proceed?"}}},
		answer:  func(values []string) { answers = append(answers, values) },
	})
	next := updated.(model)
	if next.composerActive || next.composerValue() != "" {
		t.Fatalf("ask_user should clear the composer draft, active=%v value=%q", next.composerActive, next.composerValue())
	}
	next.input.SetValue("yes")
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if len(answers) != 1 || answers[0][0] != "yes" {
		t.Fatalf("expected answer to use ask_user input only, got %#v", answers)
	}
	if transcriptContains(next.transcript, "hidden followup") {
		t.Fatalf("hidden composer draft leaked into transcript: %#v", next.transcript)
	}
}

func TestAskUserRequestClearsStaleSuggestions(t *testing.T) {
	var answers [][]string
	m := newModel(context.Background(), Options{})
	m.pending = true
	m.activeRunID = 7
	m.suggestions = []commandSuggestion{{Name: "/model", Desc: "Pick a model."}}
	m.suggestionIdx = 0
	m.suggestionsAreFiles = true

	updated, _ := m.Update(askUserRequestMsg{
		runID:   7,
		request: agent.AskUserRequest{ToolCallID: "c", Questions: []agent.AskUserQuestion{{Question: "Proceed?"}}},
		answer:  func(values []string) { answers = append(answers, values) },
	})
	next := updated.(model)
	if len(next.suggestions) != 0 || next.suggestionsAreFiles {
		t.Fatalf("ask_user should clear stale suggestions, got %#v files=%v", next.suggestions, next.suggestionsAreFiles)
	}
}

func TestAskUserEmptyRequestResolvesImmediately(t *testing.T) {
	var answers [][]string
	m := newModel(context.Background(), Options{})
	m.pending = true
	m.activeRunID = 7
	updated, _ := m.Update(askUserRequestMsg{
		runID:   7,
		request: agent.AskUserRequest{ToolCallID: "c"},
		answer:  func(values []string) { answers = append(answers, values) },
	})
	next := updated.(model)
	if next.pendingAskUser != nil {
		t.Fatalf("an empty request must not open a prompt, got %#v", next.pendingAskUser)
	}
	if len(answers) != 1 {
		t.Fatalf("an empty request should resolve immediately, got %#v", answers)
	}
}

func TestAskUserStaleRequestResolvesEmpty(t *testing.T) {
	var answers [][]string
	m := newModel(context.Background(), Options{})
	m.pending = true
	m.activeRunID = 7 // current run

	// Message for stale runID = 5
	updated, _ := m.Update(askUserRequestMsg{
		runID:   5,
		request: agent.AskUserRequest{ToolCallID: "c", Questions: []agent.AskUserQuestion{{Question: "Proceed?"}}},
		answer:  func(values []string) { answers = append(answers, values) },
	})
	next := updated.(model)
	if next.pendingAskUser != nil {
		t.Fatal("a stale request must not create a pending prompt")
	}
	if len(answers) != 1 || answers[0] != nil {
		t.Fatalf("expected stale request callback to resolve immediately with nil, got: %v", answers)
	}
}

func TestAskUserCompleteClearsAndResolves(t *testing.T) {
	var answers [][]string
	m := newModel(context.Background(), Options{})
	m.pending = true
	m.activeRunID = 7

	// Start a prompt
	updated, _ := m.Update(askUserRequestMsg{
		runID:   7,
		request: agent.AskUserRequest{ToolCallID: "c", Questions: []agent.AskUserQuestion{{Question: "Proceed?"}}},
		answer:  func(values []string) { answers = append(answers, values) },
	})
	next := updated.(model)
	if next.pendingAskUser == nil {
		t.Fatal("expected pendingAskUser to be populated")
	}

	// Receive run completion event before prompt is manually answered
	finalized, _ := next.Update(agentResponseMsg{
		runID: 7,
		err:   context.Canceled,
	})
	finalModel := finalized.(model)
	if finalModel.pendingAskUser != nil {
		t.Fatal("pendingAskUser must be cleared on response/cancel")
	}
	if len(answers) != 1 || answers[0] != nil {
		t.Fatalf("expected pendingAskUser answer callback to be resolved with nil, got: %v", answers)
	}
}
