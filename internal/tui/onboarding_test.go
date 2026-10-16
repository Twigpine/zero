package tui

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/Gitlawb/zero/internal/aimlapi"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/providermodeldiscovery"
)

func TestSetupMethodOptionsDropsOAuthWithoutOAuthProviders(t *testing.T) {
	build := func(providers []SetupProviderOption) model {
		return newModel(context.Background(), Options{
			Setup: SetupOptions{Visible: true, Providers: providers},
		})
	}

	// This setup offers only non-OAuth providers, so the OAuth method must be
	// hidden — otherwise selecting it lands the user on an empty provider list.
	noOAuth := build([]SetupProviderOption{
		{ID: "openai", Name: "OpenAI", EnvVar: "OPENAI_API_KEY", RequiresAuth: true},
		{ID: "ollama", Name: "Ollama Local", Local: true},
	})
	for _, option := range noOAuth.setupMethodOptions() {
		if option.oauth {
			t.Fatal("OAuth method must be hidden when the setup has no OAuth providers")
		}
	}

	// Add an OAuth-capable provider (xai) and the OAuth method returns.
	withOAuth := build([]SetupProviderOption{
		{ID: "xai", Name: "xAI"},
		{ID: "openai", Name: "OpenAI", EnvVar: "OPENAI_API_KEY", RequiresAuth: true},
	})
	hasOAuth := false
	for _, option := range withOAuth.setupMethodOptions() {
		if option.oauth {
			hasOAuth = true
		}
	}
	if !hasOAuth {
		t.Fatal("OAuth method must be offered when the setup has an OAuth provider")
	}
}

func TestAimlapiCheckoutLinkWrapsWithoutTruncation(t *testing.T) {
	link := "https://checkout.stripe.com/c/pay/cs_test_abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmnopqrstuvwxyz0123456789#fidkdWxOYHwnPyd1blpxYHZxWjA0"
	lines := aimlapiLinkLines(link, 32)
	if len(lines) < 2 {
		t.Fatalf("expected long checkout link to wrap, got %d line(s): %#v", len(lines), lines)
	}
	plain := plainRender(t, strings.Join(lines, "\n"))
	for index, line := range strings.Split(plain, "\n") {
		if strings.Contains(line, "://") {
			t.Fatalf("line %d still exposes an auto-detectable URL scheme: %q", index, line)
		}
	}
	if strings.Contains(plain, "…") {
		t.Fatalf("wrapped checkout link should not be truncated:\n%s", plain)
	}
	var joined strings.Builder
	for _, line := range strings.Split(plain, "\n") {
		joined.WriteString(strings.TrimSpace(line))
	}
	if got := joined.String(); got != link {
		t.Fatalf("wrapped checkout link changed:\ngot  %q\nwant %q", got, link)
	}
	for index, line := range strings.Split(plain, "\n") {
		if got := lipgloss.Width(line); got > 32 {
			t.Fatalf("line %d width = %d, want <= 32: %q", index, got, line)
		}
	}
}

func TestAimlapiCheckoutLinkReplacesProgressSpinner(t *testing.T) {
	link := "https://checkout.stripe.com/c/pay/cs_test_abcdefghijklmnopqrstuvwxyz0123456789"
	state := &aimlapiOnboardState{
		step:   aimlapiStepProgress,
		detail: link,
	}
	view := plainRender(t, strings.Join(state.viewProgress(40, "spin"), "\n"))
	assertContains(t, view, "Opening checkout in browser...")
	assertContains(t, view, "https:")
	assertContains(t, view, "//checkout.stripe.com")
	assertNotContains(t, view, "spin")
}

func TestAimlapiEmailInputRejectsIncompleteDomain(t *testing.T) {
	state := &aimlapiOnboardState{
		step:  aimlapiStepEmailInput,
		email: "123@123.",
	}

	cmd, outcome := state.handleKey(testKey(tea.KeyEnter))

	if cmd != nil {
		t.Fatal("invalid email should not start an account request")
	}
	if outcome != aimlapiContinue || state.busy {
		t.Fatalf("invalid email returned outcome=%v busy=%v, want continue/false", outcome, state.busy)
	}
	if state.errText != aimlapi.MsgEmailInvalid {
		t.Fatalf("error = %q, want %q", state.errText, aimlapi.MsgEmailInvalid)
	}
}

func TestAimlapiEverythingReadyBackReturnsToProviderList(t *testing.T) {
	m := newModel(context.Background(), Options{Setup: SetupOptions{
		Visible: true,
		Providers: []SetupProviderOption{
			{ID: "openai", Name: "OpenAI"},
			{ID: "aimlapi", Name: "aimlapi.com"},
		},
	}})
	m.setup.selected = 1
	m.setup.stage = setupStageAimlapi
	m.setup.aimlapi = &aimlapiOnboardState{
		step:         aimlapiStepDone,
		successLines: []string{aimlapi.MsgEverythingRuns},
	}

	updated, cmd := m.Update(testKey(tea.KeyLeft))
	next := updated.(model)

	if cmd != nil {
		t.Fatal("back from Everything is ready should not start a command")
	}
	if next.setup.stage != setupStageProvider {
		t.Fatalf("stage = %v, want provider list", next.setup.stage)
	}
	if next.setup.aimlapi != nil {
		t.Fatal("completed AIMLAPI sub-flow should be cleared when returning to providers")
	}
	if got := next.setupProvider().ID; got != "aimlapi" {
		t.Fatalf("selected provider = %q, want aimlapi", got)
	}
}

func TestSetupAimlapiCtrlCCancelsInFlightCheckout(t *testing.T) {
	cancelled := false
	m := newModel(context.Background(), Options{Setup: SetupOptions{Visible: true}})
	m.setup.stage = setupStageAimlapi
	m.setup.aimlapi = &aimlapiOnboardState{
		step:        aimlapiStepProgress,
		topupCancel: func() { cancelled = true },
	}

	_, cmd := m.Update(testKeyCtrl('c'))
	if !cancelled {
		t.Fatal("Ctrl+C did not cancel the in-flight AIMLAPI checkout")
	}
	if cmd == nil {
		t.Fatal("Ctrl+C should still quit after cancelling the checkout")
	}
}

func TestSetupAimlapiEnvKeyUsesResolvedInferenceEndpoint(t *testing.T) {
	t.Setenv("AIMLAPI_API_KEY", "env-secret")
	t.Setenv("AIMLAPI_INFERENCE_URL", "https://staging.example.test/v1")
	m := newModel(context.Background(), Options{Setup: SetupOptions{
		Visible: true,
		Providers: []SetupProviderOption{{
			ID: "aimlapi", Name: "aimlapi.com", EnvVar: "AIMLAPI_API_KEY", RequiresAuth: true,
		}},
	}})

	if got := m.setupBaseURL(m.setupProvider()); got != "https://staging.example.test/v1" {
		t.Fatalf("setup base URL = %q, want resolved override", got)
	}
}

func TestAimlapiKeyInputUsesAutomaticVerificationHint(t *testing.T) {
	state := &aimlapiOnboardState{step: aimlapiStepKeyInput}
	view := plainRender(t, strings.Join(state.view(64, ""), "\n"))
	assertContains(t, view, "Your API key will be hidden and verified automatically.")
	assertNotContains(t, view, "Pasted keys are hidden")
}

func TestAimlapiPastedKeyStartsBalanceValidation(t *testing.T) {
	state := &aimlapiOnboardState{step: aimlapiStepKeyInput, apiKey: " key_test "}

	cmd, outcome := state.handleKey(testKey(tea.KeyEnter))

	if cmd == nil {
		t.Fatal("pasted key should start balance validation")
	}
	if outcome != aimlapiContinue || !state.busy {
		t.Fatalf("validation returned outcome=%v busy=%v, want continue/true", outcome, state.busy)
	}
}

func TestAimlapiOnboardingErrorsStripBodiesAndRedactActiveSecrets(t *testing.T) {
	const (
		apiKey = "sk-super-secret"
		code   = "654321"
		bearer = "session-super-secret"
		body   = "server echoed sk-super-secret 654321 session-super-secret"
	)
	newState := func(step aimlapiOnboardStep) *aimlapiOnboardState {
		return &aimlapiOnboardState{step: step, apiKey: apiKey, code: code, sessionToken: bearer, busy: true}
	}
	err := aimlapi.APIError{Message: "onboarding request failed", Status: http.StatusInternalServerError, Body: body}
	tests := []struct {
		name  string
		state *aimlapiOnboardState
		apply func(*aimlapiOnboardState)
	}{
		{name: "key validation", state: newState(aimlapiStepKeyInput), apply: func(s *aimlapiOnboardState) { s.applyKeyValidation(aimlapiOnboardMsg{err: err}) }},
		{name: "account check", state: newState(aimlapiStepEmailInput), apply: func(s *aimlapiOnboardState) { s.applyCheck(aimlapiOnboardMsg{err: err}) }},
		{name: "code verification", state: newState(aimlapiStepCodeInput), apply: func(s *aimlapiOnboardState) { s.applyToken(aimlapiOnboardMsg{err: err}) }},
		{name: "key mint", state: newState(aimlapiStepEmailInput), apply: func(s *aimlapiOnboardState) { s.applyKey(aimlapiOnboardMsg{err: err}) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.apply(test.state)
			if strings.Contains(test.state.errText, body) || strings.Contains(test.state.errText, apiKey) ||
				strings.Contains(test.state.errText, code) || strings.Contains(test.state.errText, bearer) {
				t.Fatalf("credential leaked in error: %q", test.state.errText)
			}
			if !strings.Contains(test.state.errText, "HTTP 500") {
				t.Fatalf("safe status missing from error: %q", test.state.errText)
			}
		})
	}
}

func TestAimlapiPastedKeyRejectsUnauthorized(t *testing.T) {
	state := &aimlapiOnboardState{
		step:   aimlapiStepKeyInput,
		apiKey: "bad-key",
		busy:   true,
	}

	_, outcome := state.apply(aimlapiOnboardMsg{
		state: state,
		kind:  aimlapiMsgKeyValidation,
		err:   aimlapi.APIError{Status: http.StatusUnauthorized},
	})

	if outcome != aimlapiContinue || state.busy {
		t.Fatalf("invalid key returned outcome=%v busy=%v, want continue/false", outcome, state.busy)
	}
	if state.step != aimlapiStepKeyInput || state.apiKey != "" {
		t.Fatalf("invalid key left step=%v key=%q, want key input with cleared key", state.step, state.apiKey)
	}
	if state.errText != aimlapi.MsgAPIKeyInvalid {
		t.Fatalf("error = %q, want %q", state.errText, aimlapi.MsgAPIKeyInvalid)
	}
}

func TestAimlapiVerifyCodeLabelsOnlyBadRequestAsIncorrect(t *testing.T) {
	tests := []struct {
		name          string
		err           error
		wantIncorrect bool
	}{
		{name: "invalid code", err: aimlapi.APIError{Status: http.StatusBadRequest}, wantIncorrect: true},
		{name: "throttled", err: aimlapi.APIError{Status: http.StatusTooManyRequests}},
		{name: "server failure", err: aimlapi.APIError{Status: http.StatusInternalServerError}},
		{name: "network failure", err: errors.New("connection reset")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &aimlapiOnboardState{step: aimlapiStepCodeInput, code: "123456", busy: true}
			state.applyToken(aimlapiOnboardMsg{err: tt.err})
			if got := state.errText == aimlapi.MsgCodeIncorrect; got != tt.wantIncorrect {
				t.Fatalf("incorrect-code label = %v, error text %q", got, state.errText)
			}
			if state.step != aimlapiStepCodeInput || state.code != "123456" {
				t.Fatalf("verification retry lost its screen/code: step=%v code=%q", state.step, state.code)
			}
		})
	}
}

func TestAimlapiPastedKeyValidationCompletesForValidKey(t *testing.T) {
	state := &aimlapiOnboardState{
		step:   aimlapiStepKeyInput,
		apiKey: " key_test ",
		busy:   true,
	}

	_, outcome := state.apply(aimlapiOnboardMsg{
		state: state,
		kind:  aimlapiMsgKeyValidation,
	})

	if outcome != aimlapiContinue || state.busy {
		t.Fatalf("valid key returned outcome=%v busy=%v, want continue/false", outcome, state.busy)
	}
	if state.step != aimlapiStepDone || state.apiKey != "key_test" {
		t.Fatalf("valid key left step=%v key=%q, want done with trimmed key", state.step, state.apiKey)
	}
	if len(state.successLines) != 1 || state.successLines[0] != aimlapi.MsgEverythingRuns {
		t.Fatalf("success lines = %#v", state.successLines)
	}
}

func TestAimlapiKeyBalanceLowBalanceOffersTopUp(t *testing.T) {
	state := &aimlapiOnboardState{
		step:   aimlapiStepProgress,
		apiKey: "key_minted",
		busy:   true,
	}

	_, outcome := state.apply(aimlapiOnboardMsg{
		state:   state,
		kind:    aimlapiMsgKeyBalance,
		balance: aimlapi.BalanceResult{LowBalance: true},
	})

	if outcome != aimlapiContinue || state.busy {
		t.Fatalf("low balance returned outcome=%v busy=%v, want continue/false", outcome, state.busy)
	}
	if state.step != aimlapiStepLowBalance || state.lowCursor != 0 {
		t.Fatalf("low balance left step=%v cursor=%d, want low-balance prompt with cursor 0", state.step, state.lowCursor)
	}
}

func TestAimlapiTopUpFailureRetainsSessionForRetry(t *testing.T) {
	state := &aimlapiOnboardState{step: aimlapiStepProgress, paymentSessionID: "payment-live"}
	state.applyTopup(aimlapiOnboardMsg{
		topupOK: true,
		topup:   aimlapiTopupEvent{session: "pcs_resume", hasSession: true},
	})
	state.applyTopup(aimlapiOnboardMsg{
		topupOK: true,
		topup:   aimlapiTopupEvent{done: true, err: errors.New("lost response")},
	})
	if state.resumeSessionToken != "pcs_resume" {
		t.Fatalf("resume token = %q, want retained session", state.resumeSessionToken)
	}
	if state.paymentSessionID != "payment-live" {
		t.Fatalf("payment session id = %q, want retained live id", state.paymentSessionID)
	}
	if state.step != aimlapiStepAmountInput {
		t.Fatalf("step = %v, want retryable amount input", state.step)
	}
}

func TestAimlapiRejectsUnknownAccountAction(t *testing.T) {
	state := &aimlapiOnboardState{step: aimlapiStepProgress, email: "user@example.com", busy: true}
	cmd, outcome := state.applyCheck(aimlapiOnboardMsg{check: aimlapi.CheckResult{Action: "future-action"}})

	if cmd != nil || outcome != aimlapiContinue {
		t.Fatalf("unknown action returned cmd=%v outcome=%v", cmd != nil, outcome)
	}
	if state.step != aimlapiStepEmailInput || state.newAccount || state.busy {
		t.Fatalf("unknown action state = step %v new=%v busy=%v", state.step, state.newAccount, state.busy)
	}
	if state.errText != aimlapi.MsgAccountActionInvalid {
		t.Fatalf("error = %q", state.errText)
	}
}

func TestAimlapiChangedTopUpIntentDropsRetainedCheckout(t *testing.T) {
	tests := []struct {
		name      string
		amount    int
		autoTopUp bool
		wantDrop  bool
	}{
		{name: "same intent", amount: 2500, autoTopUp: true, wantDrop: false},
		{name: "changed amount", amount: 5000, autoTopUp: true, wantDrop: true},
		{name: "changed auto top-up", amount: 2500, autoTopUp: false, wantDrop: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := &aimlapiOnboardState{
				resumeSessionToken:    "pcs_old",
				paymentSessionID:      "payment-old",
				paymentAmountUSDMinor: 2500,
				paymentAutoTopUp:      true,
				autoTopUp:             test.autoTopUp,
			}
			state.prepareTopUpIntent(test.amount)
			dropped := state.resumeSessionToken == "" && state.paymentSessionID == ""
			if dropped != test.wantDrop {
				t.Fatalf("checkout dropped = %v, want %v", dropped, test.wantDrop)
			}
		})
	}
}

func TestAimlapiTopUpSuccessShowsChargedCentAmount(t *testing.T) {
	state := &aimlapiOnboardState{amount: "20.999", paymentAmountUSDMinor: 2100}
	lines := state.topUpSuccessLines()
	if got := strings.Join(lines, "\n"); !strings.Contains(got, "$21.00 has been added") || strings.Contains(got, "20.999") {
		t.Fatalf("success lines do not show normalized charge: %q", got)
	}
}

func TestAimlapiTerminalTopUpDropsByKeyIdempotencyID(t *testing.T) {
	state := &aimlapiOnboardState{
		step:               aimlapiStepProgress,
		resumeSessionToken: "pcs_old",
		paymentSessionID:   "payment-old",
	}
	state.applyTopup(aimlapiOnboardMsg{
		topupOK: true,
		topup: aimlapiTopupEvent{
			done: true, err: errors.New("checkout expired"), session: "", hasSession: true,
		},
	})

	if state.resumeSessionToken != "" || state.paymentSessionID != "" {
		t.Fatalf("terminal checkout retained session ids: resume=%q payment=%q", state.resumeSessionToken, state.paymentSessionID)
	}
	if state.step != aimlapiStepAmountInput {
		t.Fatalf("step = %v, want retryable amount input", state.step)
	}
}

func TestAimlapiOnboardingKeepsSharedTickAlive(t *testing.T) {
	m := newModel(context.Background(), Options{Setup: SetupOptions{Visible: true}})
	m.reducedMotion = false
	m.setup.stage = setupStageAimlapi
	m.setup.aimlapi = &aimlapiOnboardState{step: aimlapiStepProgress}

	// No agent run is in flight, but the aimlapi onboarding sub-flow must keep the
	// shared tick loop alive so its spinner-only progress screen keeps re-rendering.
	updated, cmd := m.Update(m.spinner.Tick())
	next := updated.(model)
	if cmd == nil || !next.spinnerTicking {
		t.Fatal("aimlapi onboarding should keep the shared spinner tick loop alive")
	}
}

func TestAimlapiAmountInputIgnoresLeft(t *testing.T) {
	for _, newAccount := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing account", true: "new account"}[newAccount], func(t *testing.T) {
			state := &aimlapiOnboardState{
				step:       aimlapiStepAmountInput,
				newAccount: newAccount,
				errText:    "keep this error",
			}

			cmd, outcome := state.handleKey(testKey(tea.KeyLeft))

			if cmd != nil {
				t.Fatal("left on the top-up amount screen should not start a command")
			}
			if outcome != aimlapiContinue {
				t.Fatalf("outcome = %v, want aimlapiContinue", outcome)
			}
			if state.step != aimlapiStepAmountInput {
				t.Fatalf("step = %v, want amount input", state.step)
			}
			if state.amountField != 0 || state.errText != "keep this error" {
				t.Fatalf("left changed amount-screen state: field=%d error=%q", state.amountField, state.errText)
			}
		})
	}
}

func TestAimlapiAutoTopUpToggleUsesBothArrowKeys(t *testing.T) {
	for _, test := range []struct {
		name string
		key  rune
	}{
		{name: "left", key: tea.KeyLeft},
		{name: "right", key: tea.KeyRight},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := &aimlapiOnboardState{
				step:        aimlapiStepAmountInput,
				amountField: 1,
				autoTopUp:   true,
			}

			cmd, outcome := state.handleKey(testKey(test.key))

			if cmd != nil || outcome != aimlapiContinue {
				t.Fatalf("arrow returned cmd=%v outcome=%v, want nil/continue", cmd != nil, outcome)
			}
			if state.autoTopUp {
				t.Fatalf("%s did not toggle auto top-up", test.name)
			}
			if state.step != aimlapiStepAmountInput {
				t.Fatalf("step = %v, want amount input", state.step)
			}
		})
	}
}

func TestAimlapiAutoTopUpRendersOnOff(t *testing.T) {
	idle := aimlapiToggleLine("auto top up > ", true, false)
	view := plainRender(t, idle)
	assertContains(t, view, "on/off")
	assertNotContains(t, view, "yes/no")
	if !strings.Contains(idle, zeroTheme.ink.Render("on")) ||
		!strings.Contains(idle, zeroTheme.faint.Render("off")) {
		t.Fatalf("idle toggle should render selected on white and unselected off faint: %q", idle)
	}

	focused := aimlapiToggleLine("auto top up > ", false, true)
	if !strings.Contains(focused, zeroTheme.accent.Render("auto top up > ")) ||
		!strings.Contains(focused, zeroTheme.faint.Render("on")) ||
		!strings.Contains(focused, zeroTheme.ink.Render("off")) {
		t.Fatalf("focused toggle should highlight its prompt, selected off, and dim unselected on: %q", focused)
	}
}

func TestAimlapiPickPathShowsNewUserFirstAndRoutesSelections(t *testing.T) {
	state := &aimlapiOnboardState{step: aimlapiStepPickPath}
	view := plainRender(t, strings.Join(state.viewPickPath(64), "\n"))
	newUserIndex := strings.Index(view, aimlapi.MsgPickPathNewUser)
	haveKeyIndex := strings.Index(view, aimlapi.MsgPickPathHaveKey)
	if newUserIndex < 0 || haveKeyIndex < 0 || newUserIndex >= haveKeyIndex {
		t.Fatalf("new-user option should precede have-key option:\n%s", view)
	}
	state.pathCursor = 0
	state.handlePickPathKey(testKey(tea.KeyEnter))
	if state.step != aimlapiStepEmailInput {
		t.Fatalf("first option routes to %v, want email input", state.step)
	}
	state.step = aimlapiStepPickPath
	state.pathCursor = 1
	state.handlePickPathKey(testKey(tea.KeyEnter))
	if state.step != aimlapiStepKeyInput {
		t.Fatalf("second option routes to %v, want key input", state.step)
	}
}

func TestFirstRunAimlapiEnvSkipsGuidedOnboarding(t *testing.T) {
	t.Setenv("AIMLAPI_API_KEY", "env-runtime-secret")
	m := newModel(context.Background(), Options{Setup: SetupOptions{
		Visible: true,
		Providers: []SetupProviderOption{{
			ID: "aimlapi", Name: "aimlapi.com", DefaultModel: "anthropic/claude-sonnet-5",
		}},
	}})
	m.setup.stage = setupStageProvider
	m.setup.selected = 0

	updated, cmd := m.advanceSetup()
	next := updated.(model)
	if next.setup.stage != setupStageModel {
		t.Fatalf("stage = %v, want model selection", next.setup.stage)
	}
	if next.setup.aimlapi != nil {
		t.Fatal("env credential should not create the guided onboarding state")
	}
	if cmd == nil {
		t.Fatal("env credential should start authenticated model discovery")
	}
}

func TestAimlapiWizardDoesNotFilterDiscoveredModelsByDefault(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.providerWizard = m.newProviderWizard()
	m.providerWizard.selectedProvider = providerWizardProviderIndex(t, m.providerWizard, "aimlapi")
	m.providerWizard.aimlapi = &aimlapiOnboardState{
		apiKey: "sk-issued",
		model:  "anthropic/claude-sonnet-5",
	}

	next, cmd := m.resolveProviderWizardAimlapi(nil, aimlapiDone)
	if next.providerWizard.step != providerWizardStepModel || cmd == nil {
		t.Fatalf("AIMLAPI completion should enter model discovery: step=%v cmd=%v", next.providerWizard.step, cmd != nil)
	}
	if next.providerWizard.modelSearch != "" {
		t.Fatalf("model search = %q, want empty so the full discovered list is visible", next.providerWizard.modelSearch)
	}
}

func TestAimlapiAmountDollarUsesInkStyle(t *testing.T) {
	line := aimlapiAmountInputLine("", "25", 40, true)
	if !strings.Contains(line, zeroTheme.ink.Render("$")) {
		t.Fatalf("amount dollar is not rendered with ink style: %q", line)
	}
	idle := aimlapiAmountInputLine("25", "25", 40, false)
	if !strings.Contains(idle, zeroTheme.ink.Render("amount > ")) ||
		strings.Contains(idle, zeroTheme.accent.Render("amount > ")) {
		t.Fatalf("unfocused amount prompt should be white: %q", idle)
	}
}

func TestAimlapiSafetyBackReturnsToProviderList(t *testing.T) {
	for _, test := range []struct {
		name string
		key  rune
	}{
		{name: "left", key: tea.KeyLeft},
		{name: "escape", key: tea.KeyEsc},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := newModel(context.Background(), Options{Setup: SetupOptions{
				Visible: true,
				Providers: []SetupProviderOption{
					{ID: "openai", Name: "OpenAI"},
					{ID: "aimlapi", Name: "aimlapi.com"},
				},
			}})
			m.setup.selected = 1
			m.setup.stage = setupStageSafety
			m.setup.aimlapi = nil // Cleared after the AIMLAPI connect flow completes.

			updated, cmd := m.Update(testKey(test.key))
			next := updated.(model)

			if cmd != nil {
				t.Fatal("back from Safety should not start a command")
			}
			if next.setup.stage != setupStageProvider {
				t.Fatalf("stage = %v, want provider list", next.setup.stage)
			}
			if got := next.setupProvider().ID; got != "aimlapi" {
				t.Fatalf("selected provider = %q, want aimlapi", got)
			}
		})
	}
}

func TestAimlapiModelBackReturnsToProviderList(t *testing.T) {
	for _, test := range []struct {
		name string
		key  rune
	}{
		{name: "left", key: tea.KeyLeft},
		{name: "escape", key: tea.KeyEsc},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := newModel(context.Background(), Options{Setup: SetupOptions{
				Visible: true,
				Providers: []SetupProviderOption{
					{ID: "openai", Name: "OpenAI"},
					{ID: "aimlapi", Name: "aimlapi.com"},
				},
			}})
			m.setup.selected = 1
			m.setup.stage = setupStageModel
			m.setup.aimlapi = nil // Cleared once AIMLAPI has issued the key.

			updated, cmd := m.Update(testKey(test.key))
			next := updated.(model)

			if cmd != nil {
				t.Fatal("back from AIMLAPI model selection should not start a command")
			}
			if next.setup.stage != setupStageProvider {
				t.Fatalf("stage = %v, want provider list", next.setup.stage)
			}
			if got := next.setupProvider().ID; got != "aimlapi" {
				t.Fatalf("selected provider = %q, want aimlapi", got)
			}
		})
	}
}

func TestSetupCredentialSummaryOAuthToken(t *testing.T) {
	m := newModel(context.Background(), Options{Setup: SetupOptions{Visible: true}})
	m.setup.oauthMode = true
	// xAI logs in with a stored, refreshable OAuth token (not key-minting), so the
	// Ready screen must say "OAuth token", not advertise an env var.
	if got := m.setupCredentialSummary(SetupProviderOption{ID: "xai", EnvVar: "XAI_API_KEY", RequiresAuth: true}); got != "OAuth token" {
		t.Fatalf("xai OAuth summary = %q, want \"OAuth token\"", got)
	}
	// OpenRouter mints a normal API key via OAuth, so it must NOT be labeled a token.
	if got := m.setupCredentialSummary(SetupProviderOption{ID: "openrouter", EnvVar: "OPENROUTER_API_KEY", RequiresAuth: true}); got == "OAuth token" {
		t.Fatalf("openrouter (key-minting) should not show as OAuth token, got %q", got)
	}
}

func TestSetupTakeoverRendersAndCompletes(t *testing.T) {
	var saved SetupSelection
	m := newModel(context.Background(), Options{
		DiscoverProviderModels: func(ctx context.Context, profile config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
			return nil, errors.New("offline")
		},
		Setup: SetupOptions{
			Visible:    true,
			Required:   true,
			ConfigPath: "/tmp/zero/config.json",
			Providers: []SetupProviderOption{
				{ID: "openai", Name: "OpenAI", DefaultModel: "gpt-4.1", EnvVar: "OPENAI_API_KEY", RequiresAuth: true},
				{ID: "ollama", Name: "Ollama Local", DefaultModel: "llama3.1", Local: true},
			},
			Save: func(selection SetupSelection) (SetupResult, error) {
				saved = selection
				return SetupResult{
					ConfigPath: "/tmp/zero/config.json",
					Provider: config.ProviderProfile{
						Name:      selection.CatalogID,
						CatalogID: selection.CatalogID,
						Model:     selection.Model,
					},
				}, nil
			},
		},
	})
	m.width = 100
	m.height = 30

	if view := plainRender(t, m.View()); !strings.Contains(view, "Welcome to Zero") || !strings.Contains(view, "Space to set up Zero") || !strings.Contains(view, "terminal agent for changing real code") {
		t.Fatalf("setup welcome view missing expected text:\n%s", view)
	}

	updated, cmd := m.Update(testKey(tea.KeySpace))
	if cmd != nil {
		t.Fatal("setup navigation should not launch a command")
	}
	m = updated.(model)
	if m.setup.stage != setupStageMethod {
		t.Fatalf("stage = %v, want method chooser", m.setup.stage)
	}
	m.setup.selectedMethod = len(m.setupMethodOptions()) - 1 // API-key / browse path
	updated, _ = m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if m.setup.stage != setupStageProvider {
		t.Fatalf("stage = %v, want provider", m.setup.stage)
	}

	updated, _ = m.Update(testKey(tea.KeyDown))
	m = updated.(model)
	if got := m.setupProvider().ID; got != "ollama" {
		t.Fatalf("selected provider = %q, want ollama", got)
	}

	for m.setup.stage != setupStageReady {
		m = pressSetupContinue(m)
	}
	updated, cmd = m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if cmd != nil {
		t.Fatal("setup completion should stay in the fullscreen chat surface")
	}

	if m.setup.visible {
		t.Fatal("setup should hide after save")
	}
	if saved.CatalogID != "ollama" || saved.Model != "llama3.1" {
		t.Fatalf("saved selection = %#v, want ollama llama3.1", saved)
	}
	if m.providerName != "ollama" || m.modelName != "llama3.1" {
		t.Fatalf("provider state = %q/%q, want ollama/llama3.1", m.providerName, m.modelName)
	}
	if !m.transcriptEmpty() {
		t.Fatalf("setup completion should open the normal empty chat surface, transcript: %#v", m.transcript)
	}
}

func TestSetupTakeoverCustomCompatibleCollectsEndpointNameAndModel(t *testing.T) {
	var saved SetupSelection
	m := newModel(context.Background(), Options{
		Setup: SetupOptions{
			Visible: true,
			Providers: []SetupProviderOption{
				{ID: "custom-openai-compatible", Name: "Custom OpenAI-compatible", DefaultModel: "custom-model", EnvVar: "OPENAI_API_KEY", RequiresAuth: true},
			},
			Save: func(selection SetupSelection) (SetupResult, error) {
				saved = selection
				return SetupResult{
					ConfigPath: "/tmp/zero/config.json",
					Provider: config.ProviderProfile{
						Name:      selection.Name,
						CatalogID: selection.CatalogID,
						BaseURL:   selection.BaseURL,
						Model:     selection.Model,
					},
				}, nil
			},
		},
	})
	m.width = 100
	m.height = 30

	m = pressSetupContinue(m)
	if m.setup.stage != setupStageProvider {
		t.Fatalf("stage = %v, want provider", m.setup.stage)
	}
	m = pressSetupContinue(m)
	if m.setup.stage != setupStageEndpoint {
		t.Fatalf("stage = %v, want endpoint", m.setup.stage)
	}
	view := plainRender(t, m.View())
	assertContains(t, view, "Endpoint URL")
	assertContains(t, view, "url >")
	assertContains(t, view, "https://api.example.com/v1")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if m.setup.stage != setupStageEndpoint {
		t.Fatalf("blank endpoint advanced to %v, want endpoint", m.setup.stage)
	}
	assertContains(t, plainRender(t, m.View()), "enter an endpoint URL")

	updated, _ = m.Update(testKeyText("https://api.minimax.io/v1"))
	m = updated.(model)
	m = pressSetupContinue(m)
	if m.setup.stage != setupStageName {
		t.Fatalf("stage = %v, want name", m.setup.stage)
	}
	view = plainRender(t, m.View())
	assertContains(t, view, "Provider name")
	assertContains(t, view, "name >")
	assertContains(t, view, "minimax")

	m = pressSetupContinue(m)
	if m.setup.stage != setupStageCredentials {
		t.Fatalf("stage = %v, want credentials", m.setup.stage)
	}
	m = pressSetupContinue(m)
	if m.setup.stage != setupStageModel {
		t.Fatalf("stage = %v, want model", m.setup.stage)
	}
	view = plainRender(t, m.View())
	assertContains(t, view, "Choose a model")
	assertContains(t, view, "model >")
	assertContains(t, view, "custom-model")

	updated, _ = m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if m.setup.stage != setupStageModel {
		t.Fatalf("blank model advanced to %v, want model", m.setup.stage)
	}
	assertContains(t, plainRender(t, m.View()), "Enter a model name")

	updated, _ = m.Update(testKeyText("MiniMax-M3"))
	m = updated.(model)
	m = pressSetupContinue(m)
	if m.setup.stage != setupStageSafety {
		t.Fatalf("stage = %v, want safety", m.setup.stage)
	}
	m = pressSetupContinue(m)
	if m.setup.stage != setupStageReady {
		t.Fatalf("stage = %v, want ready", m.setup.stage)
	}
	view = plainRender(t, m.View())
	assertContains(t, view, "provider:  minimax")
	assertContains(t, view, "endpoint:  https://api.minimax.io/v1")
	assertContains(t, view, "model:  MiniMax-M3")

	m = pressSetupContinue(m)
	if m.setup.visible {
		t.Fatal("setup should hide after saving custom provider")
	}
	if saved.CatalogID != "custom-openai-compatible" {
		t.Fatalf("saved CatalogID = %q, want custom-openai-compatible", saved.CatalogID)
	}
	if saved.Name != "minimax" {
		t.Fatalf("saved Name = %q, want minimax", saved.Name)
	}
	if saved.BaseURL != "https://api.minimax.io/v1" {
		t.Fatalf("saved BaseURL = %q, want endpoint", saved.BaseURL)
	}
	if saved.Model != "MiniMax-M3" {
		t.Fatalf("saved Model = %q, want typed model", saved.Model)
	}
}

func TestSetupEndpointAcceptsPastedURL(t *testing.T) {
	m := newModel(context.Background(), Options{
		Setup: SetupOptions{
			Visible: true,
			Providers: []SetupProviderOption{
				{ID: "custom-openai-compatible", Name: "Custom OpenAI-compatible", DefaultModel: "custom-model", EnvVar: "OPENAI_API_KEY", RequiresAuth: true},
			},
		},
	})
	m.width = 100
	m.height = 30

	m = pressSetupContinue(m)
	m = pressSetupContinue(m)
	if m.setup.stage != setupStageEndpoint {
		t.Fatalf("stage = %v, want endpoint", m.setup.stage)
	}

	updated, _ := m.Update(testPaste("https://api.minimax.io/v1\n"))
	m = updated.(model)
	if m.setup.baseURL != "https://api.minimax.io/v1" {
		t.Fatalf("setup baseURL = %q, want pasted endpoint", m.setup.baseURL)
	}

	m = pressSetupContinue(m)
	if m.setup.stage != setupStageName {
		t.Fatalf("stage = %v, want name", m.setup.stage)
	}
}

func TestSetupCompletionResetsChatSurfaceInsideAltScreen(t *testing.T) {
	m := newModel(context.Background(), Options{
		DiscoverProviderModels: func(ctx context.Context, profile config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
			return nil, errors.New("offline")
		},
		Setup: SetupOptions{
			Visible: true,
			Providers: []SetupProviderOption{
				{ID: "openai", Name: "OpenAI", DefaultModel: "gpt-4.1", EnvVar: "OPENAI_API_KEY", RequiresAuth: true},
			},
			Save: func(selection SetupSelection) (SetupResult, error) {
				return SetupResult{
					ConfigPath: "/tmp/zero/config.json",
					Provider: config.ProviderProfile{
						Name:      selection.CatalogID,
						CatalogID: selection.CatalogID,
						Model:     selection.Model,
					},
				}, nil
			},
		},
	})
	m.width = 100
	m.height = 30
	m.setup.stage = setupStageReady
	m.headerPrinted = true
	m.flushQueue = []string{"stale setup title"}
	m.printInFlight = true

	updated, cmd := m.completeSetup()
	m = updated.(model)
	if cmd != nil {
		t.Fatal("setup completion should not exit the alt-screen")
	}
	if m.setup.visible {
		t.Fatal("setup should be hidden")
	}
	if m.headerPrinted {
		t.Fatal("chat header should be reset so the normal surface can render it")
	}
	if len(m.flushQueue) != 0 {
		t.Fatalf("stale setup flush queue should be cleared, got %#v", m.flushQueue)
	}
	if m.printInFlight {
		t.Fatal("stale setup print state should be cleared")
	}
	if !m.transcriptEmpty() {
		t.Fatalf("setup completion should keep the chat empty state, transcript: %#v", m.transcript)
	}
}

func TestSetupTakeoverBlocksPromptSubmission(t *testing.T) {
	m := newModel(context.Background(), Options{
		DiscoverProviderModels: func(ctx context.Context, profile config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
			return nil, errors.New("offline")
		},
		Setup: SetupOptions{
			Visible: true,
			Providers: []SetupProviderOption{
				{ID: "openai", Name: "OpenAI", DefaultModel: "gpt-4.1"},
			},
		},
	})
	m.input.SetValue("run tests")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if cmd != nil {
		t.Fatal("setup enter should not launch an agent run")
	}
	if m.pending {
		t.Fatal("setup enter should not start a prompt")
	}
	if m.setup.stage != setupStageWelcome {
		t.Fatalf("stage = %v, want welcome because Enter is not advertised here", m.setup.stage)
	}
}

func TestSetupRightArrowDoesNotAdvance(t *testing.T) {
	saveCalls := 0
	for _, stage := range []setupStage{
		setupStageWelcome,
		setupStageProvider,
		setupStageCredentials,
		setupStageModel,
		setupStageReady,
	} {
		m := newModel(context.Background(), Options{
			Setup: SetupOptions{
				Visible: true,
				Providers: []SetupProviderOption{
					{ID: "openai", Name: "OpenAI", DefaultModel: "gpt-4.1", EnvVar: "OPENAI_API_KEY", RequiresAuth: true},
				},
				Save: func(selection SetupSelection) (SetupResult, error) {
					saveCalls++
					return SetupResult{}, nil
				},
			},
		})
		m.setup.stage = stage

		updated, cmd := m.Update(testKey(tea.KeyRight))
		m = updated.(model)
		if cmd != nil {
			t.Fatalf("right arrow at stage %v should not return a command", stage)
		}
		if m.setup.stage != stage {
			t.Fatalf("right arrow advanced stage %v to %v", stage, m.setup.stage)
		}
		if !m.setup.visible {
			t.Fatalf("right arrow at stage %v should not hide setup", stage)
		}
	}
	if saveCalls != 0 {
		t.Fatalf("right arrow should not save setup, got %d save calls", saveCalls)
	}
}

func TestSetupProviderMouseWheelChangesSelection(t *testing.T) {
	m := newModel(context.Background(), Options{
		Setup: SetupOptions{
			Visible: true,
			Providers: []SetupProviderOption{
				{ID: "openai", Name: "OpenAI", DefaultModel: "gpt-4.1", EnvVar: "OPENAI_API_KEY", RequiresAuth: true},
				{ID: "anthropic", Name: "Anthropic", DefaultModel: "claude-sonnet-4.5", EnvVar: "ANTHROPIC_API_KEY", RequiresAuth: true},
			},
		},
	})
	m.setup.stage = setupStageProvider

	updated, cmd := m.Update(testMouseWheel(tea.MouseWheelDown, 0, 0))
	m = updated.(model)
	if cmd != nil {
		t.Fatal("provider wheel should not return a command")
	}
	if got := m.setupProvider().ID; got != "anthropic" {
		t.Fatalf("provider after wheel down = %q, want anthropic", got)
	}

	updated, cmd = m.Update(testMouseWheel(tea.MouseWheelUp, 0, 0))
	m = updated.(model)
	if cmd != nil {
		t.Fatal("provider wheel should not return a command")
	}
	if got := m.setupProvider().ID; got != "openai" {
		t.Fatalf("provider after wheel up = %q, want openai", got)
	}
}

func TestSetupModelMouseWheelChangesSelection(t *testing.T) {
	m := newModel(context.Background(), Options{
		Setup: SetupOptions{
			Visible: true,
			Providers: []SetupProviderOption{
				{ID: "groq", Name: "Groq", DefaultModel: "llama-3.3-70b-versatile", EnvVar: "GROQ_API_KEY", RequiresAuth: true},
			},
		},
	})
	m.setup.stage = setupStageModel
	m.resetSetupModels()

	updated, cmd := m.Update(testMouseWheel(tea.MouseWheelDown, 0, 0))
	m = updated.(model)
	if cmd != nil {
		t.Fatal("model wheel should not return a command")
	}
	if got := m.setupCurrentModel().ID; got == "" || got == "llama-3.3-70b-versatile" {
		t.Fatalf("model after wheel down = %q, want non-default model", got)
	}

	updated, cmd = m.Update(testMouseWheel(tea.MouseWheelUp, 0, 0))
	m = updated.(model)
	if cmd != nil {
		t.Fatal("model wheel should not return a command")
	}
	if got := m.setupCurrentModel().ID; got != "llama-3.3-70b-versatile" {
		t.Fatalf("model after wheel up = %q, want default model", got)
	}
}

func TestSetupModelMouseWheelIgnoredWhileLoading(t *testing.T) {
	m := newModel(context.Background(), Options{
		Setup: SetupOptions{
			Visible: true,
			Providers: []SetupProviderOption{
				{ID: "groq", Name: "Groq", DefaultModel: "llama-3.3-70b-versatile", EnvVar: "GROQ_API_KEY", RequiresAuth: true},
			},
		},
	})
	m.setup.stage = setupStageModel
	m.resetSetupModels()
	m.setup.modelLoad = true

	updated, cmd := m.Update(testMouseWheel(tea.MouseWheelDown, 0, 0))
	m = updated.(model)
	if cmd != nil {
		t.Fatal("loading model wheel should not return a command")
	}
	if got := m.setup.modelIndex; got != 0 {
		t.Fatalf("model index after wheel while loading = %d, want 0", got)
	}
}

func TestSetupEnterDoesNotAdvanceSpaceOnlyStages(t *testing.T) {
	saveCalls := 0
	for _, stage := range []setupStage{
		setupStageWelcome,
		setupStageCredentials,
		setupStageSafety,
	} {
		m := newModel(context.Background(), Options{
			Setup: SetupOptions{
				Visible: true,
				Providers: []SetupProviderOption{
					{ID: "ollama", Name: "Ollama Local", DefaultModel: "llama3.1", Local: true},
				},
				Save: func(selection SetupSelection) (SetupResult, error) {
					saveCalls++
					return SetupResult{}, nil
				},
			},
		})
		m.setup.stage = stage

		updated, cmd := m.Update(testKey(tea.KeyEnter))
		m = updated.(model)
		if cmd != nil {
			t.Fatalf("enter at stage %v should not return a command", stage)
		}
		if m.setup.stage != stage {
			t.Fatalf("enter advanced stage %v to %v", stage, m.setup.stage)
		}
		if !m.setup.visible {
			t.Fatalf("enter at stage %v should not hide setup", stage)
		}
	}
	if saveCalls != 0 {
		t.Fatalf("enter on space-only steps should not save setup, got %d save calls", saveCalls)
	}
}

func TestSetupProviderRequiresEnter(t *testing.T) {
	m := newModel(context.Background(), Options{
		DiscoverProviderModels: func(ctx context.Context, profile config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
			return nil, errors.New("offline")
		},
		Setup: SetupOptions{
			Visible: true,
			Providers: []SetupProviderOption{
				{ID: "openai", Name: "OpenAI", DefaultModel: "gpt-4.1", EnvVar: "OPENAI_API_KEY", RequiresAuth: true},
			},
		},
	})
	m.setup.stage = setupStageProvider

	updated, cmd := m.Update(testKey(tea.KeySpace))
	m = updated.(model)
	if cmd != nil {
		t.Fatal("space on provider step should not return a command")
	}
	if m.setup.stage != setupStageProvider {
		t.Fatalf("space on provider step advanced to %v", m.setup.stage)
	}

	updated, cmd = m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if cmd != nil {
		t.Fatal("enter on provider step should not return a command")
	}
	if m.setup.stage != setupStageCredentials {
		t.Fatalf("enter on provider step should advance to credentials, got %v", m.setup.stage)
	}
}

func TestSetupReadyRequiresEnter(t *testing.T) {
	saveCalls := 0
	m := newModel(context.Background(), Options{
		Setup: SetupOptions{
			Visible: true,
			Providers: []SetupProviderOption{
				{ID: "openai", Name: "OpenAI", DefaultModel: "gpt-4.1", EnvVar: "OPENAI_API_KEY", RequiresAuth: true},
			},
			Save: func(selection SetupSelection) (SetupResult, error) {
				saveCalls++
				return SetupResult{
					Provider: config.ProviderProfile{
						Name:      selection.CatalogID,
						CatalogID: selection.CatalogID,
						Model:     selection.Model,
					},
				}, nil
			},
		},
	})
	m.setup.stage = setupStageReady

	updated, cmd := m.Update(testKey(tea.KeySpace))
	m = updated.(model)
	if cmd != nil {
		t.Fatal("space on ready step should not return a command")
	}
	if saveCalls != 0 {
		t.Fatalf("space on ready step should not save setup, got %d calls", saveCalls)
	}
	if !m.setup.visible || m.setup.stage != setupStageReady {
		t.Fatalf("space on ready step should keep setup visible at ready, visible=%v stage=%v", m.setup.visible, m.setup.stage)
	}

	updated, cmd = m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if cmd != nil {
		t.Fatal("enter on ready step should stay in the fullscreen chat surface")
	}
	if saveCalls != 1 {
		t.Fatalf("enter on ready step should save once, got %d calls", saveCalls)
	}
	if m.setup.visible {
		t.Fatal("enter on ready step should open chat")
	}
}

func TestSetupCredentialsAcceptsPastedAPIKeyWithoutRenderingSecret(t *testing.T) {
	const secret = "sk-pasted-secret"
	var saved SetupSelection
	m := newModel(context.Background(), Options{
		DiscoverProviderModels: func(ctx context.Context, profile config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
			return nil, errors.New("offline")
		},
		Setup: SetupOptions{
			Visible: true,
			Providers: []SetupProviderOption{
				{ID: "ollama-cloud", Name: "Ollama Cloud", DefaultModel: "qwen3-coder:480b", EnvVar: "OLLAMA_API_KEY", RequiresAuth: true},
			},
			Save: func(selection SetupSelection) (SetupResult, error) {
				saved = selection
				return SetupResult{
					ConfigPath: "/tmp/zero/config.json",
					Provider: config.ProviderProfile{
						Name:      selection.CatalogID,
						CatalogID: selection.CatalogID,
						Model:     selection.Model,
						APIKey:    selection.APIKey,
					},
				}, nil
			},
		},
	})
	m.width = 96
	m.height = 30
	m.setup.stage = setupStageCredentials

	updated, _ := m.Update(testPaste(secret))
	m = updated.(model)
	view := plainRender(t, m.View())
	if strings.Contains(view, secret) {
		t.Fatalf("setup view leaked pasted API key:\n%s", view)
	}
	if !strings.Contains(view, strings.Repeat("*", len(secret))) {
		t.Fatalf("setup view should show masked API key, got:\n%s", view)
	}

	for m.setup.stage != setupStageReady {
		m = pressSetupContinue(m)
	}
	updated, _ = m.Update(testKey(tea.KeyEnter))
	m = updated.(model)

	if saved.APIKey != secret {
		t.Fatalf("saved APIKey = %q, want pasted secret", saved.APIKey)
	}
	if m.providerProfile.APIKey != secret {
		t.Fatalf("providerProfile APIKey = %q, want pasted secret", m.providerProfile.APIKey)
	}
}

func TestSetupCredentialsCtrlVDoesNotRunClipboardPaste(t *testing.T) {
	m := newModel(context.Background(), Options{
		Setup: SetupOptions{
			Visible: true,
			Providers: []SetupProviderOption{
				{ID: "ollama-cloud", Name: "Ollama Cloud", DefaultModel: "qwen3-coder:480b", EnvVar: "OLLAMA_API_KEY", RequiresAuth: true},
			},
		},
	})
	m.setup.stage = setupStageCredentials
	m.setup.apiKey.SetValue("existing")
	m.setup.apiKey.CursorEnd()

	updated, cmd := m.Update(testKeyCtrl('v'))
	next := updated.(model)

	if cmd != nil {
		t.Fatal("ctrl+v should not run the setup input clipboard paste command")
	}
	if got := next.setup.apiKey.Value(); got != "existing" {
		t.Fatalf("setup API key after ctrl+v = %q, want unchanged", got)
	}
}

func TestSetupModelStepSavesCatalogModelChoice(t *testing.T) {
	var saved SetupSelection
	m := newModel(context.Background(), Options{
		DiscoverProviderModels: func(ctx context.Context, profile config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
			return nil, errors.New("offline")
		},
		Setup: SetupOptions{
			Visible: true,
			Providers: []SetupProviderOption{
				{ID: "groq", Name: "Groq", DefaultModel: "llama-3.3-70b-versatile", EnvVar: "GROQ_API_KEY", RequiresAuth: true},
			},
			Save: func(selection SetupSelection) (SetupResult, error) {
				saved = selection
				return SetupResult{
					Provider: config.ProviderProfile{
						Name:      selection.CatalogID,
						CatalogID: selection.CatalogID,
						Model:     selection.Model,
					},
				}, nil
			},
		},
	})
	m.width = 120
	m.height = 30
	m.setup.stage = setupStageCredentials

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if m.setup.stage != setupStageModel {
		t.Fatalf("stage = %v, want model", m.setup.stage)
	}
	if cmd == nil {
		t.Fatal("entering the model step should start model discovery")
	}
	view := plainRender(t, m.View())
	if !strings.Contains(view, "Checking available models") || strings.Contains(view, "llama-3.3-70b-versatile") {
		t.Fatalf("model step should wait for discovery before showing fallback models:\n%s", view)
	}
	updated, _ = m.Update(cmd())
	m = updated.(model)
	view = plainRender(t, m.View())
	for _, want := range []string{"Choose a model", "llama-3.3-70b-versatile", "openai/gpt-oss-120b"} {
		if !strings.Contains(view, want) {
			t.Fatalf("model step missing %q:\n%s", want, view)
		}
	}

	updated, _ = m.Update(testKey(tea.KeyDown))
	m = updated.(model)
	selected := m.setupCurrentModel().ID
	if selected == "" || selected == "llama-3.3-70b-versatile" {
		t.Fatalf("selected model after down = %q, want a non-default catalog model", selected)
	}
	for m.setup.stage != setupStageReady {
		m = pressSetupContinue(m)
	}
	updated, _ = m.Update(testKey(tea.KeyEnter))
	m = updated.(model)

	if saved.Model != selected {
		t.Fatalf("saved model = %q, want selected model %q", saved.Model, selected)
	}
	if m.modelName != selected {
		t.Fatalf("active model = %q, want %q", m.modelName, selected)
	}
}

func TestSetupModelSearchFiltersAndSavesMatch(t *testing.T) {
	var saved SetupSelection
	m := newModel(context.Background(), Options{
		DiscoverProviderModels: func(ctx context.Context, profile config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
			return nil, errors.New("offline")
		},
		Setup: SetupOptions{
			Visible: true,
			Providers: []SetupProviderOption{
				{ID: "groq", Name: "Groq", DefaultModel: "llama-3.3-70b-versatile", EnvVar: "GROQ_API_KEY", RequiresAuth: true},
			},
			Save: func(selection SetupSelection) (SetupResult, error) {
				saved = selection
				return SetupResult{Provider: config.ProviderProfile{Name: selection.CatalogID, CatalogID: selection.CatalogID, Model: selection.Model}}, nil
			},
		},
	})
	m.setup.stage = setupStageCredentials
	updated, cmd := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if cmd == nil {
		t.Fatal("entering the model step should start model discovery")
	}
	updated, _ = m.Update(cmd())
	m = updated.(model)

	updated, _ = m.Update(testKeyText("oss"))
	m = updated.(model)
	if got := m.setupCurrentModel().ID; got != "openai/gpt-oss-120b" {
		t.Fatalf("filtered model = %q, want openai/gpt-oss-120b", got)
	}
	view := plainRender(t, m.View())
	if !strings.Contains(view, "openai/gpt-oss-120b") || strings.Contains(view, "llama-3.3-70b-versatile") {
		t.Fatalf("model search did not filter to oss models:\n%s", view)
	}
	for m.setup.stage != setupStageReady {
		m = pressSetupContinue(m)
	}
	updated, _ = m.Update(testKey(tea.KeyEnter))
	m = updated.(model)

	if saved.Model != "openai/gpt-oss-120b" {
		t.Fatalf("saved model = %q, want openai/gpt-oss-120b", saved.Model)
	}
}

func TestSetupModelLoadingBlocksSelectionAndSearch(t *testing.T) {
	m := newModel(context.Background(), Options{
		DiscoverProviderModels: func(ctx context.Context, profile config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
			return []providermodeldiscovery.Model{{ID: "live-coder", Description: "Live Coder"}}, nil
		},
		Setup: SetupOptions{
			Visible: true,
			Providers: []SetupProviderOption{
				{ID: "groq", Name: "Groq", DefaultModel: "llama-3.3-70b-versatile", EnvVar: "GROQ_API_KEY", RequiresAuth: true},
			},
		},
	})
	m.width = 120
	m.height = 30
	m.setup.stage = setupStageCredentials

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if cmd == nil {
		t.Fatal("entering the model step should start model discovery")
	}
	view := plainRender(t, m.View())
	if !strings.Contains(view, "Checking available models") || strings.Contains(view, "llama-3.3-70b-versatile") {
		t.Fatalf("loading model step should not render fallback models:\n%s", view)
	}

	updated, _ = m.Update(testKeyText("oss"))
	m = updated.(model)
	if m.setup.modelQuery != "" {
		t.Fatalf("model query while loading = %q, want empty", m.setup.modelQuery)
	}

	updated, _ = m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if m.setup.stage != setupStageModel {
		t.Fatalf("enter while loading advanced stage to %v", m.setup.stage)
	}
	if !strings.Contains(m.setup.err, "still loading") {
		t.Fatalf("loading enter error = %q, want still loading", m.setup.err)
	}
}

func TestSetupModelSearchAcceptsQ(t *testing.T) {
	m := newModel(context.Background(), Options{
		Setup: SetupOptions{
			Visible: true,
			Providers: []SetupProviderOption{
				{ID: "openai", Name: "OpenAI", DefaultModel: "gpt-4.1", EnvVar: "OPENAI_API_KEY", RequiresAuth: true},
			},
		},
	})
	m.setup.stage = setupStageModel
	m.resetSetupModels()

	updated, cmd := m.Update(testKeyText("q"))
	m = updated.(model)
	if cmd != nil {
		t.Fatal("q should search on the model step, not quit setup")
	}
	if m.setup.modelQuery != "q" {
		t.Fatalf("model query = %q, want q", m.setup.modelQuery)
	}
}

func TestSetupModelFooterUsesEnter(t *testing.T) {
	m := newModel(context.Background(), Options{
		Setup: SetupOptions{
			Visible: true,
			Providers: []SetupProviderOption{
				{ID: "openai", Name: "OpenAI", DefaultModel: "gpt-4.1", EnvVar: "OPENAI_API_KEY", RequiresAuth: true},
			},
		},
	})
	m.width = 96
	m.height = 24
	m.setup.stage = setupStageModel

	view := plainRender(t, m.View())
	if !strings.Contains(view, "type search") || !strings.Contains(view, "Enter continue") {
		t.Fatalf("model footer should advertise search and Enter, got:\n%s", view)
	}
	if strings.Contains(view, "Space to continue") {
		t.Fatalf("model footer should not advertise Space, got:\n%s", view)
	}
}

func TestSetupModelSearchPlaceholderPutsCursorBeforeHint(t *testing.T) {
	m := newModel(context.Background(), Options{
		Setup: SetupOptions{
			Visible: true,
			Providers: []SetupProviderOption{
				{ID: "openai", Name: "OpenAI", DefaultModel: "gpt-4.1", EnvVar: "OPENAI_API_KEY", RequiresAuth: true},
			},
		},
	})
	m.setup.stage = setupStageModel

	empty := plainRender(t, m.setupModelSearchLine(60))
	if !strings.Contains(empty, "search > ▌model name...") {
		t.Fatalf("empty search line = %q, want cursor before placeholder", empty)
	}
	if strings.Contains(empty, "model name...▌") {
		t.Fatalf("empty search line should not place cursor after placeholder: %q", empty)
	}

	m.setup.modelQuery = "qwen"
	filled := plainRender(t, m.setupModelSearchLine(60))
	if !strings.Contains(filled, "search > qwen▌") {
		t.Fatalf("filled search line = %q, want cursor after query", filled)
	}
}

func TestSetupModelStepDoesNotSpinWithoutDiscovery(t *testing.T) {
	m := newModel(context.Background(), Options{
		Setup: SetupOptions{
			Visible: true,
			Providers: []SetupProviderOption{
				{ID: "custom-provider", Name: "Custom Provider", DefaultModel: "custom-model"},
			},
		},
	})
	m.setup.stage = setupStageCredentials

	updated, cmd := m.Update(testKey(tea.KeySpace))
	m = updated.(model)
	if cmd != nil {
		t.Fatal("custom setup provider should not start model discovery")
	}
	if m.setup.modelLoad {
		t.Fatal("model step should not show a loading state when no discovery command starts")
	}
}

func TestSetupModelStepUsesDiscoveredModels(t *testing.T) {
	var captured config.ProviderProfile
	m := newModel(context.Background(), Options{
		DiscoverProviderModels: func(ctx context.Context, profile config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
			captured = profile
			return []providermodeldiscovery.Model{
				{ID: "live-coder", Description: "Live Coder", ContextWindow: 128000, ToolCall: true},
				{ID: "live-fast", Description: "Live Fast"},
			}, nil
		},
		Setup: SetupOptions{
			Visible: true,
			Providers: []SetupProviderOption{
				{ID: "ollama", Name: "Ollama Local", DefaultModel: "llama3.1", Local: true},
			},
		},
	})
	m.width = 120
	m.height = 30
	m.setup.stage = setupStageCredentials

	updated, cmd := m.Update(testKey(tea.KeySpace))
	m = updated.(model)
	if m.setup.stage != setupStageModel {
		t.Fatalf("stage = %v, want model", m.setup.stage)
	}
	if cmd == nil {
		t.Fatal("entering setup model step should start discovery")
	}
	view := plainRender(t, m.View())
	if !strings.Contains(view, "Checking available models") || strings.Contains(view, "llama3.1") {
		t.Fatalf("setup should wait for discovered models before showing fallback list:\n%s", view)
	}
	updated, _ = m.Update(cmd())
	m = updated.(model)

	if captured.CatalogID != "ollama" {
		t.Fatalf("discovery profile = %#v, want ollama", captured)
	}
	view = plainRender(t, m.View())
	if !strings.Contains(view, "Live Coder") || !strings.Contains(view, "live-coder") || !strings.Contains(view, "128K ctx") || strings.Contains(view, "details") || strings.Contains(view, "llama3.1") {
		t.Fatalf("setup model step should render discovered models only:\n%s", view)
	}
}

func TestSetupModelDiscoveryDoesNotApplyAfterLeavingModelStep(t *testing.T) {
	m := newModel(context.Background(), Options{
		Setup: SetupOptions{
			Visible: true,
			Providers: []SetupProviderOption{
				{ID: "groq", Name: "Groq", DefaultModel: "llama-3.3-70b-versatile", EnvVar: "GROQ_API_KEY", RequiresAuth: true},
			},
		},
	})
	m.setup.stage = setupStageModel
	m.resetSetupModels()
	m.setup.stage = setupStageSafety

	updated := m.applySetupModelsDiscovered(setupModelsDiscoveredMsg{
		providerID: "groq",
		gen:        m.setup.modelGen,
		models: []providermodeldiscovery.Model{
			{ID: "live-coder", Description: "Live Coder"},
		},
	})

	for _, model := range updated.setup.models {
		if model.ID == "live-coder" {
			t.Fatalf("late discovery result should not replace model selection after leaving step: %#v", updated.setup.models)
		}
	}
}

func TestSetupModelDiscoveryPreservesSelectedModel(t *testing.T) {
	m := newModel(context.Background(), Options{
		Setup: SetupOptions{
			Visible: true,
			Providers: []SetupProviderOption{
				{ID: "groq", Name: "Groq", DefaultModel: "llama-3.3-70b-versatile", EnvVar: "GROQ_API_KEY", RequiresAuth: true},
			},
		},
	})
	m.setup.stage = setupStageModel
	m.resetSetupModels()
	target := "openai/gpt-oss-120b"
	for index, model := range m.setupFilteredModels() {
		if model.ID == target {
			m.setup.modelIndex = index
			break
		}
	}
	if got := m.setupCurrentModel().ID; got != target {
		t.Fatalf("test setup selected %q, want %q", got, target)
	}

	updated := m.applySetupModelsDiscovered(setupModelsDiscoveredMsg{
		providerID: "groq",
		gen:        m.setup.modelGen,
		models: []providermodeldiscovery.Model{
			{ID: "live-coder", Description: "Live Coder"},
			{ID: target, Description: "GPT OSS"},
		},
	})

	if got := updated.setupCurrentModel().ID; got != target {
		t.Fatalf("selected model after discovery = %q, want %q", got, target)
	}
}

func TestSetupModelDiscoveryIgnoresStaleGeneration(t *testing.T) {
	m := newModel(context.Background(), Options{
		Setup: SetupOptions{
			Visible: true,
			Providers: []SetupProviderOption{
				{ID: "groq", Name: "Groq", DefaultModel: "llama-3.3-70b-versatile", EnvVar: "GROQ_API_KEY", RequiresAuth: true},
			},
		},
	})
	m.setup.stage = setupStageModel
	m.resetSetupModels()
	m.setup.modelGen = 2
	m.setup.modelLoad = true

	updated := m.applySetupModelsDiscovered(setupModelsDiscoveredMsg{
		providerID: "groq",
		gen:        1,
		models: []providermodeldiscovery.Model{
			{ID: "live-coder", Description: "Live Coder"},
		},
	})

	if !updated.setup.modelLoad {
		t.Fatal("stale discovery result should not clear the active loading state")
	}
	for _, model := range updated.setup.models {
		if model.ID == "live-coder" {
			t.Fatalf("stale discovery result should not replace model list: %#v", updated.setup.models)
		}
	}
}

func TestSetupModelDiscoveryRedactsRequestAPIKey(t *testing.T) {
	const oldSecret = "old-provider-token"
	m := newModel(context.Background(), Options{
		DiscoverProviderModels: func(ctx context.Context, profile config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
			return nil, errors.New("models failed with " + profile.APIKey)
		},
		Setup: SetupOptions{
			Visible: true,
			Providers: []SetupProviderOption{
				{ID: "ollama-cloud", Name: "Ollama Cloud", DefaultModel: "qwen3-coder:480b", EnvVar: "OLLAMA_API_KEY", RequiresAuth: true},
			},
		},
	})
	m.setup.stage = setupStageCredentials
	m.setup.apiKey.SetValue(oldSecret)

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if cmd == nil {
		t.Fatal("entering model step should start discovery")
	}
	m.setup.apiKey.SetValue("new-provider-token")
	updated, _ = m.Update(cmd())
	m = updated.(model)

	if strings.Contains(m.setup.modelErr, oldSecret) {
		t.Fatalf("model discovery error leaked request API key: %q", m.setup.modelErr)
	}
	if !strings.Contains(m.setup.modelErr, "[REDACTED]") {
		t.Fatalf("model discovery error should redact request API key: %q", m.setup.modelErr)
	}
}

func TestSetupProviderStepOmitsModelDetails(t *testing.T) {
	m := newModel(context.Background(), Options{
		Setup: SetupOptions{
			Visible: true,
			Providers: []SetupProviderOption{
				{ID: "openai", Name: "OpenAI", DefaultModel: "gpt-4.1", EnvVar: "OPENAI_API_KEY", RequiresAuth: true},
				{ID: "anthropic", Name: "Anthropic", DefaultModel: "claude-sonnet-4.5", EnvVar: "ANTHROPIC_API_KEY", RequiresAuth: true},
			},
		},
	})
	m.width = 180
	m.height = 30
	m.setup.stage = setupStageProvider

	foundProviderRow := false
	titleColumn := -1
	providerColumn := -1
	view := plainRender(t, m.View())
	if strings.Contains(view, "Default model:") || strings.Contains(view, "gpt-4.1") || strings.Contains(view, "claude-sonnet-4.5") {
		t.Fatalf("provider step should not render model details:\n%s", view)
	}
	for _, line := range strings.Split(view, "\n") {
		row := strings.TrimSpace(line)
		if strings.Contains(row, "Choose a provider") {
			titleColumn = displayColumn(line, "Choose a provider")
		}
		if !strings.Contains(row, "OpenAI") {
			continue
		}
		foundProviderRow = true
		providerColumn = displayColumn(line, "OpenAI")
		if strings.Contains(row, "gpt-4.1") {
			t.Fatalf("provider row should not render model as a column: %q", row)
		}
		if got := lipgloss.Width(row); got > 44 {
			t.Fatalf("provider row width = %d, want <= 44: %q", got, row)
		}
	}
	if !foundProviderRow {
		t.Fatal("provider row missing from setup view")
	}
	if titleColumn < 0 || titleColumn != providerColumn {
		t.Fatalf("provider title should align with provider names, title column %d provider column %d", titleColumn, providerColumn)
	}
}

func TestSetupProviderSelectionDoesNotShiftBlock(t *testing.T) {
	m := newModel(context.Background(), Options{
		Setup: SetupOptions{
			Visible: true,
			Providers: []SetupProviderOption{
				{ID: "openai", Name: "OpenAI", DefaultModel: "gpt-4.1", EnvVar: "OPENAI_API_KEY", RequiresAuth: true},
				{ID: "anthropic", Name: "Anthropic", DefaultModel: "claude-sonnet-4.5", EnvVar: "ANTHROPIC_API_KEY", RequiresAuth: true},
				{ID: "ollama", Name: "Ollama Local", DefaultModel: "llama3.1", Local: true},
			},
		},
	})
	m.width = 120
	m.height = 30
	m.setup.stage = setupStageProvider

	openAIColumn := displayColumnForVisibleLine(t, m.View(), "OpenAI")
	titleColumn := displayColumnForVisibleLine(t, m.View(), "Choose a provider")

	m.moveSetupProvider(1)
	if got := displayColumnForVisibleLine(t, m.View(), "OpenAI"); got != openAIColumn {
		t.Fatalf("OpenAI column shifted after selecting Anthropic: got %d want %d", got, openAIColumn)
	}
	if got := displayColumnForVisibleLine(t, m.View(), "Choose a provider"); got != titleColumn {
		t.Fatalf("title column shifted after selecting Anthropic: got %d want %d", got, titleColumn)
	}

	m.moveSetupProvider(1)
	if got := displayColumnForVisibleLine(t, m.View(), "OpenAI"); got != openAIColumn {
		t.Fatalf("OpenAI column shifted after selecting Ollama: got %d want %d", got, openAIColumn)
	}
	if got := displayColumnForVisibleLine(t, m.View(), "Choose a provider"); got != titleColumn {
		t.Fatalf("title column shifted after selecting Ollama: got %d want %d", got, titleColumn)
	}
}

func TestSetupProviderLongCatalogUsesVisibleWindow(t *testing.T) {
	providers := make([]SetupProviderOption, 0, 14)
	for index := 0; index < 14; index++ {
		providers = append(providers, SetupProviderOption{
			ID:           "provider",
			Name:         "Provider " + string(rune('A'+index)),
			DefaultModel: "model",
		})
	}
	m := newModel(context.Background(), Options{
		Setup: SetupOptions{
			Visible:   true,
			Providers: providers,
		},
	})
	m.width = 96
	m.height = 18
	m.setup.stage = setupStageProvider

	initial := plainRender(t, m.View())
	if !strings.Contains(initial, "Provider A") || strings.Contains(initial, "Provider N") {
		t.Fatalf("initial provider window should show the first rows only:\n%s", initial)
	}

	m.setup.selected = len(providers) - 1
	scrolled := plainRender(t, m.View())
	if !strings.Contains(scrolled, "Provider N") || strings.Contains(scrolled, "Provider A") {
		t.Fatalf("scrolled provider window should follow the selected row:\n%s", scrolled)
	}
}

func TestSetupOllamaCloudCredentialCopyMentionsAPIKey(t *testing.T) {
	m := newModel(context.Background(), Options{
		Setup: SetupOptions{
			Visible: true,
			Providers: []SetupProviderOption{
				{ID: "ollama-cloud", Name: "Ollama Cloud", DefaultModel: "qwen3-coder:480b", EnvVar: "OLLAMA_API_KEY", RequiresAuth: true},
			},
		},
	})
	m.setup.stage = setupStageCredentials

	view := plainRender(t, m.View())
	for _, want := range []string{"Paste your Ollama Cloud API key", "leave blank to use OLLAMA_API_KEY from your shell", "Saved keys stay in your user config"} {
		if !strings.Contains(view, want) {
			t.Fatalf("credential copy missing %q:\n%s", want, view)
		}
	}
}

func TestSetupCredentialLinesCenterLikeWelcome(t *testing.T) {
	m := newModel(context.Background(), Options{
		Setup: SetupOptions{
			Visible: true,
			Providers: []SetupProviderOption{
				{ID: "ollama-cloud", Name: "Ollama Cloud", DefaultModel: "qwen3-coder:480b", EnvVar: "OLLAMA_API_KEY", RequiresAuth: true},
			},
		},
	})
	m.width = 120
	m.height = 30
	m.setup.stage = setupStageCredentials

	view := m.View()
	assertSetupLineCentered(t, view, "Credentials", m.width)
	assertSetupLineCentered(t, view, "Paste your", m.width)
	assertSetupLineCentered(t, view, "paste key", m.width)
	assertSetupLineCentered(t, view, "Saved keys", m.width)
}

func TestSetupCredentialEmptyInputDoesNotHighlightPlaceholder(t *testing.T) {
	m := newModel(context.Background(), Options{
		Setup: SetupOptions{
			Visible: true,
			Providers: []SetupProviderOption{
				{ID: "ollama-cloud", Name: "Ollama Cloud", DefaultModel: "qwen3-coder:480b", EnvVar: "OLLAMA_API_KEY", RequiresAuth: true},
			},
		},
	})
	m.setup.stage = setupStageCredentials

	line := m.setupAPIKeyInputLine(80)
	plain := plainRender(t, line)
	if plain != "paste key or leave blank" {
		t.Fatalf("empty API key input = %q, want placeholder only", plain)
	}
	if strings.Count(plain, "paste") != 1 {
		t.Fatalf("empty API key input should render placeholder once, got %q", plain)
	}
}

func TestSetupProgressRendersAboveFooter(t *testing.T) {
	m := newModel(context.Background(), Options{
		Setup: SetupOptions{
			Visible: true,
			Providers: []SetupProviderOption{
				{ID: "openai", Name: "OpenAI", DefaultModel: "gpt-4.1", EnvVar: "OPENAI_API_KEY", RequiresAuth: true},
			},
		},
	})
	m.width = 96
	m.height = 24
	m.setup.stage = setupStageProvider

	lines := strings.Split(plainRender(t, m.View()), "\n")
	stepIndex := -1
	footerIndex := -1
	for index, line := range lines {
		if strings.Contains(line, "3/7") {
			stepIndex = index
		}
		if strings.Contains(line, "Enter continue") {
			footerIndex = index
		}
		if strings.Contains(line, "Choose a provider") && strings.Contains(line, "3/7") {
			t.Fatalf("progress should not render in setup body: %q", line)
		}
	}
	if stepIndex < 0 || footerIndex < 0 {
		t.Fatalf("missing setup progress/footer, step=%d footer=%d view:\n%s", stepIndex, footerIndex, strings.Join(lines, "\n"))
	}
	if stepIndex != footerIndex-1 {
		t.Fatalf("progress should render immediately above footer, step line %d footer line %d", stepIndex, footerIndex)
	}
}

func TestSetupReadyFooterUsesEnter(t *testing.T) {
	m := newModel(context.Background(), Options{
		Setup: SetupOptions{
			Visible: true,
			Providers: []SetupProviderOption{
				{ID: "openai", Name: "OpenAI", DefaultModel: "gpt-4.1", EnvVar: "OPENAI_API_KEY", RequiresAuth: true},
			},
		},
	})
	m.width = 96
	m.height = 24
	m.setup.stage = setupStageReady

	view := plainRender(t, m.View())
	if !strings.Contains(view, "Enter to save and start chat") {
		t.Fatalf("ready footer should use Enter, got:\n%s", view)
	}
	if strings.Contains(view, "Space to save and start chat") {
		t.Fatalf("ready footer should not advertise Space, got:\n%s", view)
	}
}

func TestSetupMethodChooserOAuthPath(t *testing.T) {
	m := newModel(context.Background(), Options{Setup: SetupOptions{
		Visible: true,
		Providers: []SetupProviderOption{
			{ID: "openrouter", Name: "OpenRouter", RequiresAuth: true, EnvVar: "OPENROUTER_API_KEY"},
			{ID: "xai", Name: "xAI", RequiresAuth: true, EnvVar: "XAI_API_KEY"},
			{ID: "openai", Name: "OpenAI", RequiresAuth: true, EnvVar: "OPENAI_API_KEY"},
		},
	}})
	m.width = 100
	m.height = 30

	m = pressSetupContinueOnce(m) // Welcome → Method
	if m.setup.stage != setupStageMethod {
		t.Fatalf("stage = %v, want method chooser", m.setup.stage)
	}
	m.setup.selectedMethod = 0 // "Sign in with OAuth"
	updated, _ := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if m.setup.stage != setupStageProvider || !m.setup.oauthMode {
		t.Fatalf("OAuth method should enter the OAuth provider list, got stage=%v oauth=%v", m.setup.stage, m.setup.oauthMode)
	}
	ids := map[string]bool{}
	for _, p := range m.setup.providers {
		ids[p.ID] = true
	}
	if len(m.setup.providers) != 2 || !ids["openrouter"] || !ids["xai"] {
		t.Fatalf("OAuth provider list = %#v, want only openrouter+xai", m.setup.providers)
	}

	// Left returns to the method chooser and clears the OAuth selection.
	updated, _ = m.Update(testKey(tea.KeyLeft))
	m = updated.(model)
	if m.setup.stage != setupStageMethod || m.setup.oauthMode {
		t.Fatalf("retreat should return to method without oauthMode, got stage=%v oauth=%v", m.setup.stage, m.setup.oauthMode)
	}
}

func setupAtOAuthList(t *testing.T) model {
	t.Helper()
	m := newModel(context.Background(), Options{Setup: SetupOptions{
		Visible: true,
		Providers: []SetupProviderOption{
			{ID: "openrouter", Name: "OpenRouter", RequiresAuth: true, EnvVar: "OPENROUTER_API_KEY"},
			{ID: "xai", Name: "xAI", DefaultModel: "grok-4", RequiresAuth: true, EnvVar: "XAI_API_KEY"},
		},
	}})
	m.width = 100
	m.height = 30
	m = pressSetupContinueOnce(m) // Welcome → Method
	m.setup.selectedMethod = 0    // Sign in with OAuth
	updated, _ := m.Update(testKey(tea.KeyEnter))
	return updated.(model)
}

func TestSetupDeviceShortcutStartsDeviceFlow(t *testing.T) {
	m := setupAtOAuthList(t)
	for i, p := range m.setup.providers {
		if p.ID == "xai" {
			m.setup.selected = i
			break
		}
	}
	updated, cmd := m.Update(testKeyText("d"))
	m = updated.(model)
	if !m.setup.oauthPending || !m.setup.oauthDevice {
		t.Fatalf("'d' should start device login (pending=%v device=%v)", m.setup.oauthPending, m.setup.oauthDevice)
	}
	if cmd == nil {
		t.Fatal("'d' should return the device-prepare command")
	}
}

func TestApplySetupOAuthDeviceCodeShowsCodeAndPolls(t *testing.T) {
	m := setupAtOAuthList(t)
	for i, p := range m.setup.providers {
		if p.ID == "xai" {
			m.setup.selected = i
			break
		}
	}
	m.setup.oauthPending = true
	m.setup.oauthDevice = true

	res, cmd := m.applySetupOAuthDeviceCode(setupOAuthDeviceMsg{
		providerID: "xai", userCode: "WXYZ-9", verifyURL: "https://x.ai/device",
	})
	m = res.(model)
	if m.setup.deviceUserCode != "WXYZ-9" || m.setup.deviceVerificationURI != "https://x.ai/device" {
		t.Fatalf("device code not stored: %+v", m.setup)
	}
	if cmd == nil {
		t.Fatal("device-code msg should start the poll command")
	}
	view := strings.Join(m.setupOAuthWaitingLines(72), "\n")
	if !strings.Contains(view, "WXYZ-9") || !strings.Contains(view, "x.ai/device") {
		t.Fatalf("waiting render missing device code/uri:\n%s", view)
	}
}

func TestApplySetupOAuthSuccessAdvancesToModel(t *testing.T) {
	m := newModel(context.Background(), Options{
		DiscoverProviderModels: func(ctx context.Context, profile config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
			return nil, errors.New("offline")
		},
		Setup: SetupOptions{Visible: true, Providers: []SetupProviderOption{
			{ID: "openrouter", Name: "OpenRouter", RequiresAuth: true, EnvVar: "OPENROUTER_API_KEY"},
		}},
	})
	m.width = 100
	m.height = 30
	m = pressSetupContinueOnce(m) // Welcome → Method
	m.setup.selectedMethod = 0
	updated, _ := m.Update(testKey(tea.KeyEnter))
	m = updated.(model) // OAuth provider stage
	m.setup.oauthPending = true

	res, _ := m.applySetupOAuth(setupOAuthMsg{apiKey: "sk-or-minted", providerID: "openrouter"})
	m = res.(model)
	if m.setup.oauthPending {
		t.Fatal("pending should clear after success")
	}
	if m.setup.stage != setupStageModel {
		t.Fatalf("stage = %v, want model after OAuth", m.setup.stage)
	}
	if m.setup.apiKey.Value() != "sk-or-minted" {
		t.Fatalf("minted key not captured: %q", m.setup.apiKey.Value())
	}
}

func pressSetupContinue(m model) model {
	m = pressSetupContinueOnce(m)
	// Transparently skip the connect-method chooser via the API-key/browse path so
	// existing tests keep their Welcome→Provider→… expectations.
	if m.setup.stage == setupStageMethod {
		m.setup.selectedMethod = len(m.setupMethodOptions()) - 1
		m = pressSetupContinueOnce(m)
	}
	return m
}

func pressSetupContinueOnce(m model) model {
	var updated tea.Model
	var cmd tea.Cmd
	if m.setup.stage == setupStageMethod || m.setup.stage == setupStageProvider || m.setupEndpointInputActive() || m.setupNameInputActive() || m.setupCredentialInputActive() || m.setup.stage == setupStageModel || m.setup.stage == setupStageReady {
		updated, cmd = m.Update(testKey(tea.KeyEnter))
	} else {
		updated, cmd = m.Update(testKey(tea.KeySpace))
	}
	m = updated.(model)
	if cmd != nil {
		updated, _ = m.Update(cmd())
		m = updated.(model)
	}
	return m
}

func displayColumnForVisibleLine(t *testing.T, view any, marker string) int {
	t.Helper()
	rendered := plainRender(t, view)
	for _, line := range strings.Split(rendered, "\n") {
		if strings.Contains(line, marker) {
			return displayColumn(line, marker)
		}
	}
	t.Fatalf("marker %q missing from view:\n%s", marker, rendered)
	return -1
}

func displayColumn(line string, marker string) int {
	index := strings.Index(line, marker)
	if index < 0 {
		return -1
	}
	return lipgloss.Width(line[:index])
}

func assertSetupLineCentered(t *testing.T, view any, marker string, width int) {
	t.Helper()
	line := visibleLineForMarker(t, view, marker)
	trimmed := strings.TrimSpace(line)
	start := lipgloss.Width(line[:strings.Index(line, strings.TrimLeft(line, " "))])
	midpoint := start + lipgloss.Width(trimmed)/2
	want := width / 2
	if delta := absInt(midpoint - want); delta > 2 {
		t.Fatalf("line %q midpoint = %d, want near %d (delta %d)", trimmed, midpoint, want, delta)
	}
}

func visibleLineForMarker(t *testing.T, view any, marker string) string {
	t.Helper()
	rendered := plainRender(t, view)
	for _, line := range strings.Split(rendered, "\n") {
		if strings.Contains(line, marker) {
			return line
		}
	}
	t.Fatalf("marker %q missing from view:\n%s", marker, rendered)
	return ""
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

// Completing setup switches the live provider, so it must export ZERO_PROVIDER
// exactly like the /model, /provider, and wizard switch paths — a stale value
// from an earlier switch would otherwise win over config in every spawned
// child (applyEnv) and pin specialists/swarm members to the OLD provider's
// credentials.
func TestCompleteSetupExportsActiveProviderEnv(t *testing.T) {
	t.Setenv(config.ActiveProviderEnv, "stale-previous-provider")
	m := newModel(context.Background(), Options{
		Setup: SetupOptions{
			Visible: true,
			Providers: []SetupProviderOption{
				{ID: "openai", Name: "OpenAI", DefaultModel: "gpt-4.1", EnvVar: "OPENAI_API_KEY", RequiresAuth: true},
			},
			Save: func(selection SetupSelection) (SetupResult, error) {
				return SetupResult{
					Provider: config.ProviderProfile{
						Name:      selection.CatalogID,
						CatalogID: selection.CatalogID,
						Model:     selection.Model,
					},
				}, nil
			},
		},
	})
	m.width = 100
	m.height = 30
	m.setup.stage = setupStageReady
	m.removedLiveRow = "stale-previous-provider"

	updated, _ := m.completeSetup()
	next := updated.(model)

	if next.providerName == "" {
		t.Fatal("setup completion should have set a provider name")
	}
	if got := os.Getenv(config.ActiveProviderEnv); got != next.providerName {
		t.Fatalf("%s = %q after setup save, want %q (children would spawn on the stale provider)", config.ActiveProviderEnv, got, next.providerName)
	}
	if next.removedLiveRow != "" {
		t.Fatalf("removedLiveRow = %q, want cleared after setup commits a new live provider", next.removedLiveRow)
	}
}

func TestApplySetupOAuthTokenPersistFailureStaysOnProvider(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"providers":[{"name":"demo"},{"name":"DEMO"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newModel(context.Background(), Options{
		Setup: SetupOptions{Visible: true, Providers: []SetupProviderOption{
			{ID: "xai", Name: "xAI", RequiresAuth: true},
		}},
	})
	m.setup.stage = setupStageProvider
	m.setup.oauthPending = true
	m.setup.oauthMode = true
	m.setup.configPath = configPath

	updated, cmd := m.applySetupOAuth(setupOAuthMsg{tokenLogin: true, providerID: "xai"})
	next := updated.(model)
	if cmd != nil {
		t.Fatal("failed profile persistence should not start model discovery")
	}
	if next.setup.stage != setupStageProvider {
		t.Fatalf("stage = %v, want provider", next.setup.stage)
	}
	if next.setup.oauthErr == "" || !strings.Contains(next.setup.oauthErr, "could not be saved") {
		t.Fatalf("oauthErr = %q, want profile-save failure", next.setup.oauthErr)
	}
	if !strings.Contains(next.setup.oauthErr, "differ only by case") {
		t.Fatalf("oauthErr = %q, want provider identity validation error", next.setup.oauthErr)
	}
}

func TestSetupDevicePreparePreflightsConfigBeforeRequestingCode(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"providers":[{"name":"demo"},{"name":"DEMO"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	msg := setupDevicePrepareCmd("xai", configPath)().(setupOAuthDeviceMsg)
	if msg.err == nil || !strings.Contains(msg.err.Error(), "differ only by case") {
		t.Fatalf("device-code preparation error = %v, want provider identity validation", msg.err)
	}
}
