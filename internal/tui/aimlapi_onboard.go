package tui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/aimlapi"
	"github.com/Twigpine/zero/internal/redaction"
)

// aimlapiOnboardState is the shared, event-driven aimlapi.com onboarding sub-flow
// (the spec's Paths A/B: existing key or email) embedded by BOTH TUI surfaces: the first-run setup
// (onboarding.go) and the /provider wizard (provider_wizard.go). It owns all of
// its own state and transitions; the host surface only routes key + async
// messages into it and, on a terminal outcome, reads result{APIKey, Model}.
//
// It is always held behind a pointer so async command results mutate the same
// object regardless of whether the host model is copied by value (setup) or
// pointer (providerWizard).
type aimlapiOnboardState struct {
	step aimlapiOnboardStep
	gen  int // bumped on each user-initiated async op; stale results are dropped

	pathCursor int
	lowCursor  int

	apiKey  string // Path A input, and the key that ends up saved
	email   string
	code    string
	amount  string
	model   string
	baseURL string // resolved inference endpoint written into the saved profile

	autoTopUp   bool // amount screen "auto top up" toggle; defaults to on
	amountField int  // amount screen focus: 0 = amount input, 1 = auto top-up toggle

	sessionToken string // acquired via email code / passwordless; enables top-up
	newAccount   bool   // Path B sign-up: exchange the paid session into a key

	busy    bool
	detail  string // checkout / verification URL
	errText string

	successLines []string // shown on the terminal "done" screen

	topupCh     chan aimlapiTopupEvent
	topupCancel context.CancelFunc

	// resumeSessionToken is the partner-checkout session token of the last top-up
	// attempt. It is retained across a failure so a retry resumes that session
	// (recovering a paid-but-unexchanged key, never re-charging) instead of opening
	// a second checkout; cleared once the session is dead or the top-up succeeds.
	resumeSessionToken string

	// byKey routes the top-up through the API-key-bound flow (Path A pasted key /
	// AIMLAPI_API_KEY) instead of the email session: the top-up funds the account
	// that owns apiKey, with no exchange. paymentSessionID is that flow's idempotency
	// handle, generated once and reused on retry so a re-issued pay never double-charges.
	byKey            bool
	paymentSessionID string
	// payment intent attributes bind resumable checkout state to the normalized
	// amount and auto-top-up selection that created it. Changing either starts a
	// fresh intent instead of silently reusing the old checkout.
	paymentAmountUSDMinor int
	paymentAutoTopUp      bool

	// opCancel cancels the single in-flight account/key request (balance, check,
	// code, passwordless, key-mint). Only one runs at a time (input is gated while
	// busy); it is cancelled when the request completes or the flow is abandoned.
	opCancel context.CancelFunc

	noOpen      bool
	openBrowser func(string) error
}

type aimlapiOnboardStep int

const (
	aimlapiStepPickPath aimlapiOnboardStep = iota
	aimlapiStepKeyInput
	aimlapiStepEmailInput
	aimlapiStepCodeInput
	aimlapiStepLowBalance
	aimlapiStepAmountInput
	aimlapiStepProgress
	aimlapiStepDone
)

// aimlapiOutcome tells the host surface what to do after handling a key/message.
type aimlapiOutcome int

const (
	aimlapiContinue aimlapiOutcome = iota // stay in the sub-flow
	aimlapiDone                           // key acquired: read result and finalize
	aimlapiCancel                         // back out to the host's provider picker
)

func newAimlapiOnboard(openBrowser func(string) error) *aimlapiOnboardState {
	return &aimlapiOnboardState{
		step:      aimlapiStepPickPath,
		amount:    "25",
		autoTopUp: true, // default the "auto top up" toggle to on (matches the mockup)
		// Seed the base URL from the resolved endpoints so a staging / custom
		// AIMLAPI_INFERENCE_URL override is carried into the saved profile for every
		// path (pasted key or top-up), not silently replaced by the catalog default.
		baseURL:     aimlapi.ResolveEndpoints().InferenceBaseURL,
		openBrowser: openBrowser,
	}
}

// ---- async messages -------------------------------------------------------

type aimlapiMsgKind int

const (
	aimlapiMsgKeyValidation aimlapiMsgKind = iota // Path A: validate pasted key via balance endpoint
	aimlapiMsgCheck                               // Path B: does the account exist?
	aimlapiMsgToken                               // Path B: session from code / passwordless
	aimlapiMsgKey                                 // Path B existing: minted key
	aimlapiMsgKeyBalance                          // Path B existing: balance of the minted key
	aimlapiMsgTopup                               // shared: one streamed top-up event
)

// aimlapiOnboardMsg carries one async result back to the owning sub-model. The
// state pointer lets the host route it to the right surface; gen drops results
// from an attempt the user has since abandoned.
type aimlapiOnboardMsg struct {
	state *aimlapiOnboardState
	gen   int
	kind  aimlapiMsgKind

	balance aimlapi.BalanceResult
	check   aimlapi.CheckResult
	token   string
	key     aimlapi.CreatedKey
	topup   aimlapiTopupEvent
	topupOK bool
	err     error
}

func aimlapiOnboardClient() *aimlapi.Client {
	return aimlapi.NewClient(aimlapi.ResolveEndpoints(), nil)
}

// applyAimlapiOnboard routes an async aimlapi.com onboarding result to whichever
// surface (the /provider wizard or the first-run setup) owns the sub-flow. The
// state pointer identifies the owner; a result whose sub-flow was replaced is
// silently dropped.
func (m model) applyAimlapiOnboard(msg aimlapiOnboardMsg) (tea.Model, tea.Cmd) {
	if msg.state == nil {
		return m, nil
	}
	if m.providerWizard != nil && m.providerWizard.aimlapi == msg.state {
		return m.applyProviderWizardAimlapiOnboard(msg)
	}
	if m.setup.aimlapi == msg.state {
		return m.applySetupAimlapiOnboard(msg)
	}
	return m, nil
}

// aimlapiOnboardAnimating reports whether an aimlapi.com onboarding sub-flow (in
// first-run setup or the /provider wizard) is in a state whose spinner needs the
// shared tick loop to keep advancing: a pending async round-trip or the streamed
// top-up progress screen. It lets the tick stay alive during onboarding even
// though no agent run is in flight.
func (m model) aimlapiOnboardAnimating() bool {
	if aimlapiStateAnimating(m.setup.aimlapi) {
		return true
	}
	return m.providerWizard != nil && (m.providerWizard.aimlapiExistingBusy || aimlapiStateAnimating(m.providerWizard.aimlapi))
}

func aimlapiStateAnimating(s *aimlapiOnboardState) bool {
	if s == nil {
		return false
	}
	return s.busy || s.step == aimlapiStepProgress
}

func (s *aimlapiOnboardState) msg(kind aimlapiMsgKind) aimlapiOnboardMsg {
	return aimlapiOnboardMsg{state: s, gen: s.gen, kind: kind}
}

func (s *aimlapiOnboardState) keyBalanceCmd(key string) tea.Cmd {
	m := s.msg(aimlapiMsgKeyBalance)
	ctx := s.opContext()
	return func() tea.Msg {
		res, err := aimlapiOnboardClient().GetBalance(ctx, key)
		m.balance, m.err = res, err
		return m
	}
}

// keyValidationCmd validates a pasted/env key (Path A) via GetBalance, capturing
// both the error and the balance. A low balance can now be topped up safely with
// the key itself — the by-key checkout binds the top-up to this key's own account —
// so applyKeyValidation offers the optional top-up chooser instead of discarding it.
func (s *aimlapiOnboardState) keyValidationCmd(key string) tea.Cmd {
	m := s.msg(aimlapiMsgKeyValidation)
	ctx := s.opContext()
	return func() tea.Msg {
		res, err := aimlapiOnboardClient().GetBalance(ctx, key)
		m.balance, m.err = res, err
		return m
	}
}

func (s *aimlapiOnboardState) checkAccountCmd(email string) tea.Cmd {
	m := s.msg(aimlapiMsgCheck)
	ctx := s.opContext()
	return func() tea.Msg {
		res, err := aimlapiOnboardClient().CheckAccount(ctx, email)
		m.check, m.err = res, err
		return m
	}
}

// sendCodeThenVerifyCmd sends the sign-in code (Path B existing). The verify step
// happens later once the user types the code; this only requests it.
func (s *aimlapiOnboardState) sendCodeCmd(email string) tea.Cmd {
	m := s.msg(aimlapiMsgToken) // token kind reused: no session yet, err-only signal
	ctx := s.opContext()
	return func() tea.Msg {
		err := aimlapiOnboardClient().SendSignInCode(ctx, email)
		m.err = err
		m.token = "" // sentinel: code sent, still need verify
		return m
	}
}

func (s *aimlapiOnboardState) verifyCodeCmd(email, code string) tea.Cmd {
	m := s.msg(aimlapiMsgToken)
	ctx := s.opContext()
	return func() tea.Msg {
		res, err := aimlapiOnboardClient().VerifySignInCode(ctx, email, code)
		m.token, m.err = res.Token, err
		return m
	}
}

func (s *aimlapiOnboardState) passwordlessCmd(email string) tea.Cmd {
	m := s.msg(aimlapiMsgToken)
	ctx := s.opContext()
	return func() tea.Msg {
		res, err := aimlapiOnboardClient().CreatePasswordlessAccount(ctx, email)
		m.token, m.err = res.Token, err
		return m
	}
}

func (s *aimlapiOnboardState) createKeyCmd(token string) tea.Cmd {
	m := s.msg(aimlapiMsgKey)
	ctx := s.opContext()
	return func() tea.Msg {
		res, err := aimlapiOnboardClient().CreateKey(ctx, token, "zero CLI")
		m.key, m.err = res, err
		return m
	}
}

func startAimlapiStreamTopUp(ctx context.Context, options aimlapi.StreamTopUpOptions) chan aimlapiTopupEvent {
	ch := make(chan aimlapiTopupEvent, 16)
	go func() {
		opts := options
		opts.OnStatus = func(_ aimlapi.Status, detail string) {
			ch <- aimlapiTopupEvent{detail: detail}
		}
		opts.OnSession = func(token string) {
			ch <- aimlapiTopupEvent{session: token, hasSession: true}
		}
		result, err := aimlapi.StreamTopUp(ctx, opts)
		ch <- aimlapiTopupEvent{done: true, result: result, err: err}
	}()
	return ch
}

// startAimlapiStreamTopUpByKey runs the API-key-bound top-up (Path A / env key)
// and streams the same progress events, so applyTopup handles both flows uniformly.
func startAimlapiStreamTopUpByKey(ctx context.Context, options aimlapi.StreamTopUpByKeyOptions) chan aimlapiTopupEvent {
	ch := make(chan aimlapiTopupEvent, 16)
	go func() {
		opts := options
		opts.OnStatus = func(_ aimlapi.Status, detail string) {
			ch <- aimlapiTopupEvent{detail: detail}
		}
		opts.OnSession = func(token string) {
			ch <- aimlapiTopupEvent{session: token, hasSession: true}
		}
		result, err := aimlapi.StreamTopUpByKey(ctx, opts)
		ch <- aimlapiTopupEvent{done: true, result: result, err: err}
	}()
	return ch
}

func waitForAimlapiTopupEvent(s *aimlapiOnboardState, gen int, ch chan aimlapiTopupEvent) tea.Cmd {
	return func() tea.Msg {
		event, ok := <-ch
		return aimlapiOnboardMsg{state: s, gen: gen, kind: aimlapiMsgTopup, topup: event, topupOK: ok}
	}
}

// ---- key handling ---------------------------------------------------------

func (s *aimlapiOnboardState) handleKey(msg tea.KeyMsg) (tea.Cmd, aimlapiOutcome) {
	// Esc always abandons the sub-flow; the streamed top-up is cancelled first.
	if keyIs(msg, tea.KeyEsc) {
		s.cancelStream()
		return nil, aimlapiCancel
	}
	// While an async round-trip or a streamed top-up is in flight, swallow input.
	if s.busy {
		return nil, aimlapiContinue
	}
	if s.step == aimlapiStepProgress {
		return nil, aimlapiContinue
	}

	switch s.step {
	case aimlapiStepPickPath:
		return s.handlePickPathKey(msg)
	case aimlapiStepKeyInput:
		return s.handleKeyInputKey(msg)
	case aimlapiStepEmailInput:
		return s.handleEmailInputKey(msg)
	case aimlapiStepCodeInput:
		return s.handleCodeInputKey(msg)
	case aimlapiStepLowBalance:
		return s.handleLowBalanceKey(msg)
	case aimlapiStepAmountInput:
		return s.handleAmountInputKey(msg)
	case aimlapiStepDone:
		if keyIs(msg, tea.KeyLeft) {
			return nil, aimlapiCancel
		}
		if keyIs(msg, tea.KeyEnter) || keyIs(msg, tea.KeyRight) {
			return nil, aimlapiDone
		}
	}
	return nil, aimlapiContinue
}

// handlePickPathKey drives new configuration: create an account or paste a key.
// Reusing an already configured/env credential is a /provider preflight, not a
// third onboarding identity path.
func (s *aimlapiOnboardState) handlePickPathKey(msg tea.KeyMsg) (tea.Cmd, aimlapiOutcome) {
	switch {
	case keyIs(msg, tea.KeyUp) || keyIs(msg, tea.KeyDown) || keyIs(msg, tea.KeyTab):
		s.pathCursor = (s.pathCursor + 1) % 2
	case keyIs(msg, tea.KeyLeft):
		return nil, aimlapiCancel
	case keyIs(msg, tea.KeyEnter) || keyIs(msg, tea.KeyRight):
		s.errText = ""
		if s.pathCursor == 0 {
			s.step = aimlapiStepEmailInput
		} else {
			s.step = aimlapiStepKeyInput
		}
	}
	return nil, aimlapiContinue
}

func (s *aimlapiOnboardState) handleKeyInputKey(msg tea.KeyMsg) (tea.Cmd, aimlapiOutcome) {
	switch {
	case keyIs(msg, tea.KeyLeft):
		s.step = aimlapiStepPickPath
		s.errText = ""
	case keyBackspace(msg):
		s.apiKey = trimLastRune(s.apiKey)
	case keyCtrl(msg, 'u'):
		s.apiKey = ""
		s.errText = ""
	case keyIs(msg, tea.KeyEnter) || keyIs(msg, tea.KeyRight):
		if strings.TrimSpace(s.apiKey) == "" {
			return nil, aimlapiContinue
		}
		s.startBusy()
		return s.keyValidationCmd(strings.TrimSpace(s.apiKey)), aimlapiContinue
	case keyText(msg) != "":
		s.apiKey += keyText(msg)
		s.errText = ""
	}
	return nil, aimlapiContinue
}

func (s *aimlapiOnboardState) handleEmailInputKey(msg tea.KeyMsg) (tea.Cmd, aimlapiOutcome) {
	switch {
	case keyIs(msg, tea.KeyLeft):
		s.step = aimlapiStepPickPath
		s.errText = ""
	case keyBackspace(msg):
		s.email = trimLastRune(s.email)
	case keyCtrl(msg, 'u'):
		s.email = ""
		s.errText = ""
	case keyIs(msg, tea.KeyEnter) || keyIs(msg, tea.KeyRight):
		email := strings.TrimSpace(s.email)
		if !aimlapiValidEmail(email) {
			s.errText = aimlapi.MsgEmailInvalid
			return nil, aimlapiContinue
		}
		s.email = email
		s.startBusy()
		return s.checkAccountCmd(email), aimlapiContinue
	case keyText(msg) != "":
		s.email += keyText(msg)
		s.errText = ""
	}
	return nil, aimlapiContinue
}

func (s *aimlapiOnboardState) handleCodeInputKey(msg tea.KeyMsg) (tea.Cmd, aimlapiOutcome) {
	switch {
	case keyIs(msg, tea.KeyLeft):
		s.step = aimlapiStepEmailInput
		s.errText = ""
	case keyBackspace(msg):
		s.code = trimLastRune(s.code)
	case keyCtrl(msg, 'u'):
		s.code = ""
		s.errText = ""
	case keyIs(msg, tea.KeyEnter) || keyIs(msg, tea.KeyRight):
		if strings.TrimSpace(s.code) == "" {
			return nil, aimlapiContinue
		}
		s.startBusy()
		return s.verifyCodeCmd(s.email, strings.TrimSpace(s.code)), aimlapiContinue
	case keyText(msg) != "":
		s.code += keyText(msg)
		s.errText = ""
	}
	return nil, aimlapiContinue
}

func (s *aimlapiOnboardState) handleLowBalanceKey(msg tea.KeyMsg) (tea.Cmd, aimlapiOutcome) {
	switch {
	case keyIs(msg, tea.KeyUp) || keyIs(msg, tea.KeyDown) || keyIs(msg, tea.KeyTab):
		s.lowCursor = (s.lowCursor + 1) % 2
	case keyIs(msg, tea.KeyEnter) || keyIs(msg, tea.KeyRight):
		if s.lowCursor == 1 { // skip topping up
			return s.finishEverythingRuns(), aimlapiContinue
		}
		// Top up now, against the account that owns the key: Path B existing (email
		// session) or Path A (byKey, bound to the pasted/env key). startTopUp routes
		// to the right flow.
		s.step = aimlapiStepAmountInput
		s.errText = ""
	}
	return nil, aimlapiContinue
}

// handleAmountInputKey drives the "Add credits" screen, which has two focusable
// fields: the dollar amount and the "auto top up" yes/no toggle. Tab/↑/↓ move
// focus between them; Enter continues from either. On the amount field ← is
// ignored so the top-up selection cannot retreat into an invalid previous screen;
// digits edit the value. On the toggle field ←/→/Space flip it.
func (s *aimlapiOnboardState) handleAmountInputKey(msg tea.KeyMsg) (tea.Cmd, aimlapiOutcome) {
	switch {
	case keyIs(msg, tea.KeyLeft) && s.amountField == 0:
		return nil, aimlapiContinue
	case keyIs(msg, tea.KeyTab) || keyIs(msg, tea.KeyUp) || keyIs(msg, tea.KeyDown):
		s.amountField = (s.amountField + 1) % 2
		return nil, aimlapiContinue
	case keyIs(msg, tea.KeyEnter):
		// A valid amount goes straight to the Stripe (card) checkout. startTopUp
		// re-validates and re-prompts on a bad amount.
		return s.startTopUp()
	}
	if s.amountField == 1 {
		// Auto top-up toggle focused: either horizontal arrow or Space flips yes↔no.
		if keyIs(msg, tea.KeyLeft) || keyIs(msg, tea.KeyRight) || keyText(msg) == " " {
			s.autoTopUp = !s.autoTopUp
			s.errText = ""
		}
		return nil, aimlapiContinue
	}
	// Amount field focused.
	switch {
	case keyIs(msg, tea.KeyRight):
		return s.startTopUp()
	case keyBackspace(msg):
		s.amount = trimLastRune(s.amount)
	case keyCtrl(msg, 'u'):
		s.amount = ""
		s.errText = ""
	case keyText(msg) != "":
		s.amount += keyText(msg)
		s.errText = ""
	}
	return nil, aimlapiContinue
}

// appendInput folds pasted text into the sub-flow's currently focused field.
func (s *aimlapiOnboardState) appendInput(content string) {
	if s.busy {
		return
	}
	content = strings.TrimRight(content, "\r\n")
	switch s.step {
	case aimlapiStepKeyInput:
		s.apiKey += content
	case aimlapiStepEmailInput:
		s.email += content
	case aimlapiStepCodeInput:
		s.code += content
	case aimlapiStepAmountInput:
		s.amount += content
	}
}

// ---- async application ----------------------------------------------------

func (s *aimlapiOnboardState) apply(msg aimlapiOnboardMsg) (tea.Cmd, aimlapiOutcome) {
	if msg.gen != s.gen {
		return nil, aimlapiContinue // stale: user moved on
	}
	switch msg.kind {
	case aimlapiMsgKeyValidation:
		return s.applyKeyValidation(msg)
	case aimlapiMsgKeyBalance:
		return s.applyKeyBalance(msg)
	case aimlapiMsgCheck:
		return s.applyCheck(msg)
	case aimlapiMsgToken:
		return s.applyToken(msg)
	case aimlapiMsgKey:
		return s.applyKey(msg)
	case aimlapiMsgTopup:
		return s.applyTopup(msg)
	}
	return nil, aimlapiContinue
}

func (s *aimlapiOnboardState) applyKeyValidation(msg aimlapiOnboardMsg) (tea.Cmd, aimlapiOutcome) {
	s.stopBusy()
	if msg.err != nil {
		if aimlapiIsUnauthorized(msg.err) {
			s.apiKey = ""
			s.errText = aimlapi.MsgAPIKeyInvalid
			s.step = aimlapiStepKeyInput
			return nil, aimlapiContinue
		}
		s.errText = s.safeErrorMessage(msg.err)
		s.step = aimlapiStepKeyInput
		return nil, aimlapiContinue
	}
	s.apiKey = strings.TrimSpace(s.apiKey)
	// A valid key with a low balance can fund its own account via the by-key
	// checkout (bound to this key), so offer the optional top-up chooser.
	if msg.balance.LowBalance {
		s.byKey = true
		s.lowCursor = 0
		s.step = aimlapiStepLowBalance
		return nil, aimlapiContinue
	}
	return s.finishEverythingRuns(), aimlapiContinue
}

func (s *aimlapiOnboardState) applyKeyBalance(msg aimlapiOnboardMsg) (tea.Cmd, aimlapiOutcome) {
	s.stopBusy()
	if msg.err != nil {
		// The key is already minted; a balance read failure shouldn't block it.
		return s.finishEverythingRuns(), aimlapiContinue
	}
	if msg.balance.LowBalance {
		s.lowCursor = 0
		s.step = aimlapiStepLowBalance
		return nil, aimlapiContinue
	}
	return s.finishEverythingRuns(), aimlapiContinue
}

func (s *aimlapiOnboardState) applyCheck(msg aimlapiOnboardMsg) (tea.Cmd, aimlapiOutcome) {
	s.stopBusy()
	if msg.err != nil {
		s.errText = s.safeErrorMessage(msg.err)
		s.step = aimlapiStepEmailInput
		return nil, aimlapiContinue
	}
	switch msg.check.Action {
	case "sign-in":
		s.newAccount = false
		s.startBusy()
		return s.sendCodeCmd(s.email), aimlapiContinue
	case "sign-up":
		// No account yet: passwordless sign-up, then fund via top-up (min $20).
		s.newAccount = true
		s.startBusy()
		return s.passwordlessCmd(s.email), aimlapiContinue
	default:
		s.newAccount = false
		s.errText = aimlapi.MsgAccountActionInvalid
		s.step = aimlapiStepEmailInput
		return nil, aimlapiContinue
	}
}

func (s *aimlapiOnboardState) applyToken(msg aimlapiOnboardMsg) (tea.Cmd, aimlapiOutcome) {
	wasVerifying := s.step == aimlapiStepCodeInput
	s.stopBusy()
	if msg.err != nil {
		if wasVerifying {
			// Stay on the code screen so the user can retry, but only claim the
			// code was wrong for the backend's actual rejection. A timeout, 429,
			// 5xx, or dropped connection keeps the code they typed valid, so surface
			// the real error instead of sending them to re-enter a good code.
			if aimlapiIsInvalidCode(msg.err) {
				s.errText = aimlapi.MsgCodeIncorrect
			} else {
				s.errText = s.safeErrorMessage(msg.err)
			}
			s.step = aimlapiStepCodeInput
			return nil, aimlapiContinue
		}
		// send-code or passwordless sign-up failed — back to the email prompt.
		s.errText = s.safeErrorMessage(msg.err)
		s.step = aimlapiStepEmailInput
		return nil, aimlapiContinue
	}
	if msg.token == "" {
		// Sentinel from sendCodeCmd: the code was sent, collect it now.
		s.step = aimlapiStepCodeInput
		s.code = ""
		s.errText = ""
		return nil, aimlapiContinue
	}
	// We hold a session now.
	s.sessionToken = msg.token
	if s.newAccount {
		// New account: fund it (top-up will exchange for the key).
		s.step = aimlapiStepAmountInput
		s.errText = ""
		return nil, aimlapiContinue
	}
	// Path B existing: mint a persistable key from the session.
	s.startBusy()
	return s.createKeyCmd(s.sessionToken), aimlapiContinue
}

func (s *aimlapiOnboardState) applyKey(msg aimlapiOnboardMsg) (tea.Cmd, aimlapiOutcome) {
	s.stopBusy()
	if msg.err != nil {
		s.errText = s.safeErrorMessage(msg.err)
		s.step = aimlapiStepEmailInput
		return nil, aimlapiContinue
	}
	s.apiKey = msg.key.Key
	s.startBusy()
	return s.keyBalanceCmd(s.apiKey), aimlapiContinue
}

func (s *aimlapiOnboardState) applyTopup(msg aimlapiOnboardMsg) (tea.Cmd, aimlapiOutcome) {
	if !msg.topupOK {
		return nil, aimlapiContinue
	}
	event := msg.topup
	if event.hasSession {
		// Retain (or, on "", drop) the live partner-checkout token so a retry
		// resumes this session rather than opening a second checkout.
		s.resumeSessionToken = event.session
		if strings.TrimSpace(event.session) == "" {
			// An empty token marks a terminal checkout. Its by-key idempotency ID
			// belongs to that dead session too; reusing it would return or reject
			// the old checkout instead of allowing a fresh payment attempt.
			s.paymentSessionID = ""
		}
	}
	if !event.done {
		if strings.TrimSpace(event.detail) != "" {
			s.detail = event.detail
		}
		return waitForAimlapiTopupEvent(s, s.gen, s.topupCh), aimlapiContinue
	}
	s.cancelStream()
	if event.err != nil {
		// Keep resumeSessionToken: the next startTopUp resumes this session (poll a
		// pending payment / recover a paid-but-unexchanged key) instead of charging
		// again.
		s.errText = aimlapi.MsgTopUpFailed
		s.step = aimlapiStepAmountInput
		s.amountField = 0
		return nil, aimlapiContinue
	}
	// Top-up completed; nothing left to resume or re-fund idempotently.
	s.resumeSessionToken = ""
	s.paymentSessionID = ""
	if s.newAccount {
		s.apiKey = event.result.APIKey
	}
	if strings.TrimSpace(event.result.Model) != "" {
		s.model = event.result.Model
	}
	if strings.TrimSpace(event.result.BaseURL) != "" {
		s.baseURL = event.result.BaseURL
	}
	s.successLines = s.topUpSuccessLines()
	s.step = aimlapiStepDone
	return nil, aimlapiContinue
}

// ---- transitions & helpers ------------------------------------------------

func (s *aimlapiOnboardState) startTopUp() (tea.Cmd, aimlapiOutcome) {
	if strings.TrimSpace(s.amount) == "" {
		// An empty field would otherwise fall through to the $25 default inside
		// ParseAmountUSD and silently start a checkout the user never typed.
		s.errText = aimlapi.MsgAmountRequired
		s.step = aimlapiStepAmountInput
		s.amountField = 0
		return nil, aimlapiContinue
	}
	amountUSDMinor, err := aimlapi.ParseAmountUSD(s.amount)
	if err != nil {
		// Surface the actual validation error (below-min / above-max / non-numeric),
		// the same way this sub-flow renders its other errors, instead of a single
		// catch-all "too low".
		s.errText = redaction.ErrorMessage(err, redaction.Options{})
		s.step = aimlapiStepAmountInput
		s.amountField = 0
		return nil, aimlapiContinue
	}
	s.prepareTopUpIntent(amountUSDMinor)
	normalizedAmount := formatUSDMinor(amountUSDMinor)
	s.gen++
	gen := s.gen
	s.detail = ""
	s.errText = ""
	s.step = aimlapiStepProgress
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	s.topupCancel = cancel
	// Both checkout paths use the same intent id. Retain it across ambiguous
	// failures, and replace it only when the amount/auto-top-up intent changes or
	// the backend reports that the prior session is dead.
	if strings.TrimSpace(s.paymentSessionID) == "" {
		id, err := aimlapi.NewPaymentSessionID()
		if err != nil {
			cancel()
			s.errText = aimlapi.MsgTopUpFailed
			s.step = aimlapiStepAmountInput
			s.amountField = 0
			return nil, aimlapiContinue
		}
		s.paymentSessionID = id
	}
	if s.byKey {
		// Path A / env key: fund the account bound to apiKey.
		s.topupCh = startAimlapiStreamTopUpByKey(ctx, aimlapi.StreamTopUpByKeyOptions{
			APIKey:             s.apiKey,
			AmountUSD:          normalizedAmount,
			InferenceBaseURL:   s.baseURL,
			AutoTopUp:          s.autoTopUp,
			ResumeSessionToken: s.resumeSessionToken,
			PaymentSessionID:   s.paymentSessionID,
			OpenBrowser:        s.openBrowser,
			NoOpen:             s.noOpen,
		})
	} else {
		s.topupCh = startAimlapiStreamTopUp(ctx, aimlapi.StreamTopUpOptions{
			SessionToken:       s.sessionToken,
			ResumeSessionToken: s.resumeSessionToken,
			PaymentSessionID:   s.paymentSessionID,
			AmountUSD:          normalizedAmount,
			InferenceBaseURL:   s.baseURL,
			Method:             aimlapi.PaymentMethodCard,
			AutoTopUp:          s.autoTopUp,
			Exchange:           s.newAccount,
			OpenBrowser:        s.openBrowser,
			NoOpen:             s.noOpen,
		})
	}
	return waitForAimlapiTopupEvent(s, gen, s.topupCh), aimlapiContinue
}

func (s *aimlapiOnboardState) prepareTopUpIntent(amountUSDMinor int) {
	if s.paymentAmountUSDMinor != 0 &&
		(s.paymentAmountUSDMinor != amountUSDMinor || s.paymentAutoTopUp != s.autoTopUp) {
		s.resumeSessionToken = ""
		s.paymentSessionID = ""
	}
	s.paymentAmountUSDMinor = amountUSDMinor
	s.paymentAutoTopUp = s.autoTopUp
}

func formatUSDMinor(amount int) string {
	return fmt.Sprintf("%d.%02d", amount/100, amount%100)
}

// finishEverythingRuns lands on the terminal "Everything is ready" screen with the
// key already in hand (Path A ok/skip, or Path B existing after balance/top-up).
func (s *aimlapiOnboardState) finishEverythingRuns() tea.Cmd {
	s.successLines = []string{aimlapi.MsgEverythingRuns}
	s.step = aimlapiStepDone
	return nil
}

// safeErrorMessage strips server-controlled API response bodies before display
// and redacts every credential that can be active in the onboarding state. This
// keeps echoed keys, verification codes, and session bearers out of terminal
// scrollback while retaining the operation and HTTP status for diagnosis.
func (s *aimlapiOnboardState) safeErrorMessage(err error) string {
	var apiErr aimlapi.APIError
	if errors.As(err, &apiErr) {
		err = aimlapi.APIError{Message: apiErr.Message, Status: apiErr.Status}
	}
	return redaction.ErrorMessage(err, redaction.Options{ExtraSecretValues: []string{
		s.apiKey,
		s.email,
		s.code,
		s.sessionToken,
	}})
}

// topUpSuccessLines builds the terminal "done" body. For a new account the
// magic-link note follows the balance line after a blank spacer row; an existing
// key just gets the top-up confirmation.
func (s *aimlapiOnboardState) topUpSuccessLines() []string {
	amount := fmt.Sprintf(aimlapi.MsgTopUpAddedFmt, formatUSDMinor(s.paymentAmountUSDMinor))
	if s.newAccount {
		return []string{
			aimlapi.MsgTopUpSuccess,
			amount,
			"",
			fmt.Sprintf(aimlapi.MsgSuccessMagicLink, s.email),
		}
	}
	return []string{
		aimlapi.MsgTopUpSuccess,
		amount,
	}
}

func (s *aimlapiOnboardState) startBusy() {
	s.gen++
	s.busy = true
	s.errText = ""
}

func (s *aimlapiOnboardState) stopBusy() {
	s.busy = false
	// The result has arrived; release the request's context.
	s.cancelPendingOp()
}

// opContext returns a cancellable context for the next account/key request and
// records its cancel so an abandoned flow (Esc) or the next request can stop the
// in-flight one. Only one request runs at a time, so replacing the prior cancel
// is safe.
func (s *aimlapiOnboardState) opContext() context.Context {
	s.cancelPendingOp()
	ctx, cancel := context.WithCancel(context.Background())
	s.opCancel = cancel
	return ctx
}

func (s *aimlapiOnboardState) cancelPendingOp() {
	if s.opCancel != nil {
		s.opCancel()
		s.opCancel = nil
	}
}

func (s *aimlapiOnboardState) cancelStream() {
	if s.topupCancel != nil {
		s.topupCancel()
		s.topupCancel = nil
	}
	s.topupCh = nil
	// Also stop any pending account/key request the user is abandoning.
	s.cancelPendingOp()
	s.gen++ // drop any late stream/poll events
}

func aimlapiValidEmail(value string) bool {
	return aimlapi.ValidEmail(value)
}

func aimlapiIsUnauthorized(err error) bool {
	var apiErr aimlapi.APIError
	return errors.As(err, &apiErr) &&
		(apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden)
}

// aimlapiIsInvalidCode reports whether err is the backend rejecting the sign-in
// code itself. The backend contract uses 400 for "Invalid or expired code";
// every other status (including auth, throttling and service failures) must be
// surfaced as itself instead of being mislabeled as user input.
func aimlapiIsInvalidCode(err error) bool {
	var apiErr aimlapi.APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusBadRequest
}

// ---- rendering ------------------------------------------------------------

// aimlapiBlockWidth is the centered-block width for the aimlapi.com onboarding
// screens. It mirrors the other first-run setup stages (setupMethodBlockWidth et
// al.) so the sub-flow renders as one centered column in the same visual system
// instead of a left-aligned outlier — never wider than the surface it draws on.
func aimlapiBlockWidth(width int) int {
	if width <= 0 {
		width = 64
	}
	return maxInt(24, minInt(width, 64))
}

// view renders the current sub-flow screen. spinner is the host model's live
// liveness glyph (m.spinnerGlyph()); it drives the animated spinner that stands
// in for the streamed top-up's status lines and for every in-flight async wait.
func (s *aimlapiOnboardState) view(width int, spinner string) []string {
	rowWidth := aimlapiBlockWidth(width)
	var lines []string
	switch s.step {
	case aimlapiStepPickPath:
		lines = s.viewPickPath(width)
	case aimlapiStepKeyInput:
		lines = aimlapiInputScreen(rowWidth, aimlapi.MsgAPIKeyInputPrompt,
			"api key > ", maskedProviderWizardKey(s.apiKey), "paste your key",
			"Your API key will be hidden and verified automatically.")
	case aimlapiStepEmailInput:
		lines = aimlapiInputScreen(rowWidth, aimlapi.MsgEnterEmail,
			"email > ", strings.TrimSpace(s.email), "name@example.com", "")
	case aimlapiStepCodeInput:
		lines = aimlapiInputScreen(rowWidth, fmt.Sprintf(aimlapi.MsgCodeSent, s.email),
			"code > ", strings.TrimSpace(s.code), "6-digit code", "")
	case aimlapiStepLowBalance:
		lines = s.viewLowBalance(width)
	case aimlapiStepAmountInput:
		lines = s.viewAmount(width)
	case aimlapiStepProgress:
		lines = s.viewProgress(width, spinner)
	case aimlapiStepDone:
		lines = s.viewDone(width)
	}
	// A pending async round-trip shows a spinner (not a status line) beneath the
	// current screen, so the wait always reads as live motion.
	if s.busy {
		lines = append(lines, blankSetupBlockLine(rowWidth))
		lines = append(lines, aimlapiSpinnerLine(spinner, rowWidth))
	}
	if s.errText != "" {
		lines = append(lines, blankSetupBlockLine(rowWidth))
		lines = append(lines, aimlapiBlockText(s.errText, zeroTheme.red.Render, rowWidth)...)
	}
	return lines
}

// aimlapiSpinnerLine renders the animated liveness glyph as one block row at the
// shared content indent, in accent colour. It replaces the sub-flow's textual
// status lines: the glyph advances on every spinner tick (the host keeps the tick
// loop alive while the sub-flow is busy or on the progress screen), so it never
// reads as a frozen screen. Under reduced motion the host passes a steady dot.
func aimlapiSpinnerLine(spinner string, width int) string {
	glyph := strings.TrimSpace(spinner)
	if glyph == "" {
		glyph = "•"
	}
	return padSetupLine("  "+zeroTheme.accent.Render(glyph), width)
}

// aimlapiBlockText renders a plain-text message as one or more block rows: the
// text is word-wrapped to the block width (so long copy is never cut off with an
// ellipsis), given the shared two-space content indent, styled, and padded to
// width so every row shares the block's left edge as one centered column.
func aimlapiBlockText(text string, render func(...string) string, width int) []string {
	segments := wrapPlainText(text, maxInt(1, width-2))
	if len(segments) == 0 {
		segments = []string{""}
	}
	lines := make([]string, 0, len(segments))
	for _, segment := range segments {
		lines = append(lines, padSetupLine("  "+render(segment), width))
	}
	return lines
}

// aimlapiPromptLines renders the first line as the primary instruction and any
// following lines as faint supporting copy.
func aimlapiPromptLines(prompt string, width int) []string {
	lines := []string{}
	for index, promptLine := range strings.Split(prompt, "\n") {
		render := zeroTheme.ink.Render
		if index > 0 {
			render = zeroTheme.faint.Render
		}
		lines = append(lines, aimlapiBlockText(promptLine, render, width)...)
	}
	return lines
}

// aimlapiInputScreen renders one of the sub-flow's single-field prompts as a
// centered block with an optional faint footnote.
func aimlapiInputScreen(width int, prompt, inputPrompt, value, placeholder, footnote string) []string {
	lines := []string{}
	// Input prompts use their first line as the instruction and any following
	// lines as supporting copy. Render that supporting copy faint so messages such
	// as "To access aimlapi.com dashboard" read as subtitles, not another heading.
	lines = append(lines, aimlapiPromptLines(prompt, width)...)
	lines = append(lines, blankSetupBlockLine(width))
	lines = append(lines, padSetupLine("  "+providerWizardInputLine(inputPrompt, value, placeholder, maxInt(1, width-2)), width))
	if footnote != "" {
		lines = append(lines, aimlapiBlockText(footnote, zeroTheme.faint.Render, width)...)
	}
	return lines
}

// aimlapiOptionRows renders one selectable option as a marker+label line with an
// optional faint subtitle beneath it, block-padded to width so the picker centers
// as one column. The selected row is marked with ❯ and drawn in bold accent — the
// same treatment the other setup stages use (setupMethodLines).
func aimlapiOptionRows(label, hint string, selected bool, width int) []string {
	marker := "  "
	style := zeroTheme.ink
	if selected {
		marker = "❯ "
		style = zeroTheme.accent.Bold(true)
	}
	rows := []string{padSetupLine(marker+style.Render(label), width)}
	if hint != "" {
		rows = append(rows, padSetupLine("    "+zeroTheme.faint.Render(hint), width))
	}
	return rows
}

func (s *aimlapiOnboardState) viewPickPath(width int) []string {
	rowWidth := aimlapiBlockWidth(width)
	options := []struct{ label, hint string }{
		{aimlapi.MsgPickPathNewUser, aimlapi.MsgPickPathNewHint},
		{aimlapi.MsgPickPathHaveKey, aimlapi.MsgPickPathHaveHint},
	}
	lines := []string{}
	lines = append(lines, aimlapiBlockText(aimlapi.MsgPickPathPrompt, zeroTheme.ink.Render, rowWidth)...)
	lines = append(lines, blankSetupBlockLine(rowWidth))
	for index, option := range options {
		lines = append(lines, aimlapiOptionRows(option.label, option.hint, index == s.pathCursor, rowWidth)...)
	}
	return lines
}

func (s *aimlapiOnboardState) viewLowBalance(width int) []string {
	rowWidth := aimlapiBlockWidth(width)
	lines := []string{}
	lines = append(lines, aimlapiPromptLines(aimlapi.MsgLowBalance, rowWidth)...)
	lines = append(lines, blankSetupBlockLine(rowWidth))
	lines = append(lines, aimlapiOptionRows(aimlapi.MsgLowBalanceTopUp, "", s.lowCursor == 0, rowWidth)...)
	lines = append(lines, aimlapiOptionRows(aimlapi.MsgLowBalanceSkip, "", s.lowCursor == 1, rowWidth)...)
	return lines
}

// viewAmount renders the "Add credits" screen: the dollar amount input and the
// "auto top up" on/off toggle beneath it. The focused field carries the ❯ marker
// (the same idiom as the pick-path / low-balance pickers).
func (s *aimlapiOnboardState) viewAmount(width int) []string {
	rowWidth := aimlapiBlockWidth(width)
	fieldWidth := maxInt(1, rowWidth-4)
	lines := []string{}
	lines = append(lines, aimlapiPromptLines(aimlapi.MsgTopUpPrompt, rowWidth)...)
	lines = append(lines, blankSetupBlockLine(rowWidth))
	lines = append(lines,
		padSetupLine("  "+aimlapiFieldMarker(s.amountField == 0)+
			aimlapiAmountInputLine(strings.TrimSpace(s.amount), "25", fieldWidth, s.amountField == 0), rowWidth),
		blankSetupBlockLine(rowWidth),
		padSetupLine("  "+aimlapiFieldMarker(s.amountField == 1)+
			aimlapiToggleLine("auto top up > ", s.autoTopUp, s.amountField == 1), rowWidth),
	)
	return lines
}

// aimlapiFieldMarker returns the fixed-width focus marker for a form field: a bold
// accent ❯ when selected, two spaces otherwise, so switching focus never shifts
// the surrounding columns.
func aimlapiFieldMarker(selected bool) string {
	if selected {
		return zeroTheme.accent.Bold(true).Render("❯") + " "
	}
	return "  "
}

// aimlapiAmountInputLine highlights only the prompt and cursor while this field
// has focus. The dollar sign and entered amount remain white in either state.
func aimlapiAmountInputLine(value string, placeholder string, width int, focused bool) string {
	promptStyle := zeroTheme.ink
	if focused {
		promptStyle = zeroTheme.accent
	}
	prompt := promptStyle.Render("amount > ") + zeroTheme.ink.Render("$")
	cursor := promptStyle.Render("▌")
	if value == "" {
		return fitStyledLine(prompt+cursor+zeroTheme.faint.Render(placeholder), width)
	}
	return fitStyledLine(prompt+zeroTheme.ink.Render(value)+cursor, width)
}

// aimlapiToggleLine highlights only the prompt on focus. The selected choice is
// white; the other uses the same faint style as supporting copy.
func aimlapiToggleLine(prompt string, on bool, focused bool) string {
	promptStyle := zeroTheme.ink
	if focused {
		promptStyle = zeroTheme.accent
	}
	onText, offText := zeroTheme.faint.Render("on"), zeroTheme.faint.Render("off")
	if on {
		onText = zeroTheme.ink.Render("on")
	} else {
		offText = zeroTheme.ink.Render("off")
	}
	return promptStyle.Render(prompt) + onText + zeroTheme.ink.Render("/") + offText
}

// viewProgress is the streamed top-up screen. Per the design it is a spinner only
// — the per-step status lines (registering / signing in / creating session /
// waiting for payment / issuing key) are gone. The direct checkout link is still
// shown once it arrives, since the user must open it to pay.
func (s *aimlapiOnboardState) viewProgress(width int, spinner string) []string {
	rowWidth := aimlapiBlockWidth(width)
	lines := []string{}
	if link := strings.TrimSpace(s.detail); link != "" {
		lines = append(lines, aimlapiBlockText("Opening checkout in browser...", zeroTheme.ink.Render, rowWidth)...)
		lines = append(lines, blankSetupBlockLine(rowWidth))
		lines = append(lines, aimlapiBlockText(fmt.Sprintf(aimlapi.MsgTopUpBrowserFallback, ""), zeroTheme.faint.Render, rowWidth)...)
		lines = append(lines, blankSetupBlockLine(rowWidth))
		lines = append(lines, aimlapiLinkLines(link, rowWidth)...)
		return lines
	}
	lines = append(lines, aimlapiSpinnerLine(spinner, rowWidth))
	return lines
}

// aimlapiLinkLines wraps a checkout / verification URL inside the centered block
// instead of truncating it, so fallback links stay copyable even when long.
func aimlapiLinkLines(link string, width int) []string {
	remaining := strings.TrimSpace(link)
	measure := maxInt(1, width-2)
	if remaining == "" {
		return nil
	}
	lines := []string{}
	// Keep the scheme and its slashes on separate terminal rows. Otherwise the
	// terminal auto-detects the visible https:// URL and adds its own hover
	// underline even though the TUI emits no hyperlink escape sequences.
	if schemeEnd := strings.Index(remaining, "://"); schemeEnd > 0 {
		head := remaining[:schemeEnd+1]
		lines = append(lines, padSetupLine("  "+zeroTheme.accent.Render(head), width))
		remaining = remaining[schemeEnd+1:]
	}
	for remaining != "" {
		head, tail := splitPlainAtDisplayWidth(remaining, measure)
		if head == "" {
			head, tail = string([]rune(remaining)[0]), string([]rune(remaining)[1:])
		}
		lines = append(lines, padSetupLine("  "+zeroTheme.accent.Render(head), width))
		remaining = tail
	}
	return lines
}

func (s *aimlapiOnboardState) viewDone(width int) []string {
	rowWidth := aimlapiBlockWidth(width)
	lines := []string{}
	for _, line := range s.successLines {
		if line == "" {
			lines = append(lines, blankSetupBlockLine(rowWidth))
			continue
		}
		render := zeroTheme.ink.Render
		if line == aimlapi.MsgEverythingRuns || line == aimlapi.MsgTopUpSuccess ||
			line == fmt.Sprintf(aimlapi.MsgTopUpAddedFmt, strings.TrimSpace(s.amount)) {
			render = zeroTheme.accent.Render
		}
		lines = append(lines, aimlapiBlockText(line, render, rowWidth)...)
	}
	lines = append(lines, blankSetupBlockLine(rowWidth))
	lines = append(lines, aimlapiBlockText("Press Enter to continue.", zeroTheme.faint.Render, rowWidth)...)
	return lines
}
