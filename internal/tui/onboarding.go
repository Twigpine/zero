package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/Gitlawb/zero/internal/aimlapi"
	"github.com/Gitlawb/zero/internal/browser"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/oauth"
	"github.com/Gitlawb/zero/internal/providercatalog"
	"github.com/Gitlawb/zero/internal/providermodeldiscovery"
	"github.com/Gitlawb/zero/internal/provideroauth"
	"github.com/Gitlawb/zero/internal/redaction"
)

type setupStage int

const (
	setupStageWelcome setupStage = iota
	setupStageMethod
	setupStageProvider
	setupStageEndpoint
	setupStageName
	setupStageCredentials
	// aimlapi.com onboarding sub-flow, reached when the selected
	// provider is aimlapi. Delegated to the shared aimlapiOnboardState.
	setupStageAimlapi
	setupStageModel
	setupStageSafety
	setupStageReady
)

type setupState struct {
	visible        bool
	required       bool
	configPath     string
	providers      []SetupProviderOption // active list (full, or OAuth-only after the chooser)
	allProviders   []SetupProviderOption // full catalog (restored when leaving the OAuth path)
	selected       int
	selectedMethod int
	oauthMode      bool
	oauthPending   bool
	oauthErr       string
	// Device-code login (RFC 8628) state while an OAuth login is in flight.
	oauthDevice           bool
	deviceUserCode        string
	deviceVerificationURI string
	stage                 setupStage
	err                   string
	baseURL               string
	name                  string
	apiKey                textinput.Model
	models                []providerWizardModel
	modelIndex            int
	modelQuery            string
	modelForID            string
	modelLoad             bool
	modelErr              string
	modelSrc              string
	modelGen              uint64
	// aimlapi holds the shared aimlapi.com onboarding sub-flow while
	// setup is on setupStageAimlapi.
	aimlapi *aimlapiOnboardState
}

// aimlapiTopupEvent is one live update from a running partner checkout, streamed
// over a channel so the TUI can show progress (and the direct checkout link) as
// it happens instead of blocking on a single result. Mirrors openclaude's
// onStatus-driven progress screen.
type aimlapiTopupEvent struct {
	detail string // for opening-checkout this is the direct checkout URL
	done   bool
	result aimlapi.ProvisionedKey
	err    error

	// session carries the live partner-checkout session token (empty string means
	// "drop the retained token"); hasSession distinguishes it from a status update.
	session    string
	hasSession bool
}

// setupOAuthMsg carries the result of a first-run browser OAuth login.
type setupOAuthMsg struct {
	apiKey     string
	tokenLogin bool
	providerID string
	err        error
}

// setupOAuthProviderOptions filters the full provider list to the OAuth-capable
// ones for the OAuth method path. ChatGPT/Claude are not here — they can't do
// real in-app OAuth (use "browse" + a local proxy); see docs/oauth-subscriptions.md.
func setupOAuthProviderOptions(all []SetupProviderOption) []SetupProviderOption {
	out := []SetupProviderOption{}
	for _, option := range all {
		if descriptor, ok := providercatalog.Get(option.ID); ok && descriptor.OAuth {
			out = append(out, option)
		}
	}
	return out
}

type setupModelsDiscoveredMsg struct {
	providerID       string
	gen              uint64
	redactionSecrets []string
	models           []providermodeldiscovery.Model
	err              error
}

func newSetupState(options SetupOptions) setupState {
	providers := append([]SetupProviderOption{}, options.Providers...)
	if len(providers) == 0 {
		providers = []SetupProviderOption{{
			ID:           "openai",
			Name:         "OpenAI",
			DefaultModel: "gpt-4.1",
			EnvVar:       "OPENAI_API_KEY",
			RequiresAuth: true,
		}}
	}
	apiKey := textinput.New()
	apiKey.Prompt = ""
	apiKey.Placeholder = "paste key or leave blank"
	apiKey.EchoMode = textinput.EchoPassword
	apiKey.EchoCharacter = '*'
	// Bubble's Ctrl+V binding reads the clipboard itself. Keep it disabled so
	// terminal bracketed paste (Paste: true) is the single paste path.
	apiKey.KeyMap.Paste.SetEnabled(false)
	apiKey.Focus()
	return setupState{
		visible:      options.Visible,
		required:     options.Required,
		configPath:   strings.TrimSpace(options.ConfigPath),
		providers:    providers,
		allProviders: providers,
		apiKey:       apiKey,
	}
}

func (m model) handleSetupKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.clearMouseSelection()
	// While a browser OAuth login is in flight, ignore input except Ctrl+C (quit)
	// and Esc (cancel back to the OAuth provider list).
	if m.setup.oauthPending {
		switch {
		case keyCtrl(msg, 'c'):
			return m, tea.Quit
		case keyIs(msg, tea.KeyEsc):
			m.setup.oauthPending = false
			m.setup.oauthDevice = false
		}
		return m, nil
	}
	// The aimlapi.com sub-flow owns all of its keys; Ctrl+C still quits.
	if m.setup.stage == setupStageAimlapi {
		if keyCtrl(msg, 'c') {
			if m.setup.aimlapi != nil {
				m.setup.aimlapi.cancelStream()
			}
			return m, tea.Quit
		}
		return m.handleSetupAimlapiKey(msg)
	}
	if m.setupEndpointInputActive() {
		return m.handleSetupEndpointKey(msg)
	}
	if m.setupNameInputActive() {
		return m.handleSetupNameKey(msg)
	}
	if m.setupCredentialInputActive() {
		return m.handleSetupCredentialKey(msg)
	}
	switch {
	case keyCtrl(msg, 'c'):
		return m, tea.Quit
	case keyIs(msg, tea.KeyEsc):
		if m.setup.stage > setupStageWelcome {
			prev := m.previousSetupStage()
			if prev == setupStageMethod {
				m = m.setupReturnToMethod()
			}
			m.setup.stage = prev
			m.setup.err = ""
			return m, nil
		}
		if m.setup.required {
			return m, tea.Quit
		}
		return m.exitSetupToChat()
	case keyIs(msg, tea.KeyLeft):
		if m.setup.stage > setupStageWelcome {
			prev := m.previousSetupStage()
			if prev == setupStageMethod {
				m = m.setupReturnToMethod()
			}
			m.setup.stage = prev
			m.setup.err = ""
		}
		return m, nil
	case keyIs(msg, tea.KeyEnter):
		if m.setup.stage == setupStageMethod || m.setup.stage == setupStageProvider || m.setup.stage == setupStageModel || m.setup.stage == setupStageReady {
			return m.advanceSetup()
		}
		return m, nil
	case keyIs(msg, tea.KeySpace):
		if m.setup.stage < setupStageReady && m.setup.stage != setupStageProvider && m.setup.stage != setupStageModel {
			return m.advanceSetup()
		}
		return m, nil
	case keyIs(msg, tea.KeyUp) || keyCtrl(msg, 'p'):
		switch m.setup.stage {
		case setupStageMethod:
			m.moveSetupMethod(-1)
		case setupStageProvider:
			m.moveSetupProvider(-1)
		case setupStageModel:
			m.moveSetupModel(-1)
		}
		return m, nil
	case keyIs(msg, tea.KeyDown) || keyCtrl(msg, 'n'):
		switch m.setup.stage {
		case setupStageMethod:
			m.moveSetupMethod(1)
		case setupStageProvider:
			m.moveSetupProvider(1)
		case setupStageModel:
			m.moveSetupModel(1)
		}
		return m, nil
	case keyText(msg) != "":
		if m.setup.stage == setupStageModel {
			m.appendSetupModelQuery(keyRunes(msg))
			return m, nil
		}
		switch keyText(msg) {
		case "q":
			return m, tea.Quit
		case "k":
			if m.setup.stage == setupStageProvider {
				m.moveSetupProvider(-1)
			}
		case "j":
			if m.setup.stage == setupStageProvider {
				m.moveSetupProvider(1)
			}
		case "d", "D":
			// Force device-code login for a device-capable OAuth provider.
			if m.setup.stage == setupStageProvider && m.setup.oauthMode && m.setupProviderDescriptor().OAuthDeviceFlow {
				return m.startSetupDeviceLogin(m.setupProviderDescriptor())
			}
		}
		return m, nil
	case keyBackspace(msg):
		if m.setup.stage == setupStageModel {
			m.deleteSetupModelQueryRune()
		}
		return m, nil
	case keyCtrl(msg, 'u'):
		if m.setup.stage == setupStageModel {
			m.setup.modelQuery = ""
			m.setup.modelIndex = 0
		}
		return m, nil
	}

	switch msg.String() {
	case " ":
		if m.setup.stage < setupStageReady && m.setup.stage != setupStageProvider && m.setup.stage != setupStageModel {
			return m.advanceSetup()
		}
	case "q":
		return m, tea.Quit
	case "k":
		switch m.setup.stage {
		case setupStageProvider:
			m.moveSetupProvider(-1)
		case setupStageModel:
			m.moveSetupModel(-1)
		}
	case "j":
		switch m.setup.stage {
		case setupStageProvider:
			m.moveSetupProvider(1)
		case setupStageModel:
			m.moveSetupModel(1)
		}
	}
	return m, nil
}

func (m model) handleSetupPaste(msg tea.PasteMsg) (tea.Model, tea.Cmd) {
	if m.setup.oauthPending {
		return m, nil
	}
	switch {
	case m.setupEndpointInputActive():
		m.appendSetupBaseURL([]rune(msg.Content))
	case m.setupNameInputActive():
		m.appendSetupName([]rune(msg.Content))
	case m.setup.stage == setupStageAimlapi:
		if m.setup.aimlapi != nil {
			m.setup.aimlapi.appendInput(msg.Content)
		}
	case m.setupCredentialInputActive():
		previousAPIKey := m.setup.apiKey.Value()
		var cmd tea.Cmd
		m.setup.apiKey, cmd = m.setup.apiKey.Update(msg)
		if m.setup.apiKey.Value() != previousAPIKey {
			m.setup.modelGen++
			m.resetSetupModels()
		}
		m.setup.err = ""
		return m, cmd
	case m.setup.stage == setupStageModel:
		m.appendSetupModelQuery([]rune(msg.Content))
	}
	return m, nil
}

func (m model) handleSetupMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.setup.oauthPending {
		return m, nil
	}
	// A right-click pastes the clipboard into the focused setup field, mirroring
	// handleMouse (mouse.go) so paste behaves identically in setup mode — routePaste
	// targets the active setup input. Without this branch, setup swallows the
	// right-click and paste never fires.
	if mouseRightPress(msg) {
		return m, pasteFromClipboardCmd()
	}
	if mouseLeftPress(msg) {
		switch m.setup.stage {
		case setupStageProvider:
			if target, ok := m.selectSetupProviderAtMouse(msg); ok {
				if m.repeatMouseSelection(target) {
					m.clearMouseSelection()
					return m.advanceSetup()
				}
				m.lastMouseSelection = target
				return m, nil
			}
		case setupStageModel:
			if target, ok := m.selectSetupModelAtMouse(msg); ok {
				if m.repeatMouseSelection(target) {
					m.clearMouseSelection()
					return m.advanceSetup()
				}
				m.lastMouseSelection = target
				return m, nil
			}
		}
	}

	switch {
	case mouseWheelUp(msg):
		m.clearMouseSelection()
		switch m.setup.stage {
		case setupStageProvider:
			m.moveSetupProvider(-1)
		case setupStageModel:
			m.moveSetupModel(-1)
		}
	case mouseWheelDown(msg):
		m.clearMouseSelection()
		switch m.setup.stage {
		case setupStageProvider:
			m.moveSetupProvider(1)
		case setupStageModel:
			m.moveSetupModel(1)
		}
	}
	return m, nil
}

func (m *model) selectSetupProviderAtMouse(msg tea.MouseMsg) (mouseSelectionTarget, bool) {
	if len(m.setup.providers) == 0 {
		return mouseSelectionTarget{}, false
	}
	width := chatWidth(m.width)
	height := normalizedStartupHeight(m.height)
	rowWidth := setupProviderBlockWidth(width, m.setup.providers)
	if !setupBlockContainsMouseX(mouseX(msg), width, rowWidth) {
		return mouseSelectionTarget{}, false
	}
	maxVisible := setupProviderMaxVisible(height, len(m.setup.providers))
	if maxVisible == 0 {
		return mouseSelectionTarget{}, false
	}
	content := m.setupProviderLines(width, height)
	top := setupContentTop(height, len(content), m.setup.err != "")
	row := mouseY(msg) - top - 2
	if row < 0 || row >= maxVisible {
		return mouseSelectionTarget{}, false
	}
	start := selectableListStart(len(m.setup.providers), maxVisible, m.setup.selected)
	index := start + row
	if index < 0 || index >= len(m.setup.providers) {
		return mouseSelectionTarget{}, false
	}
	m.setup.selected = index
	m.setup.apiKey.SetValue("")
	m.setup.baseURL = ""
	m.setup.name = ""
	m.setup.modelGen++
	m.resetSetupModels()
	return mouseSelectionTarget{Scope: "first-run-provider", Value: m.setup.providers[index].ID, Index: index}, true
}

func (m *model) selectSetupModelAtMouse(msg tea.MouseMsg) (mouseSelectionTarget, bool) {
	if m.setup.modelLoad {
		return mouseSelectionTarget{}, false
	}
	m.ensureSetupModels()
	models := m.setupFilteredModels()
	if len(models) == 0 {
		return mouseSelectionTarget{}, false
	}
	width := chatWidth(m.width)
	height := normalizedStartupHeight(m.height)
	rowWidth := setupModelBlockWidth(width, m.setup.models)
	if !setupBlockContainsMouseX(mouseX(msg), width, rowWidth) {
		return mouseSelectionTarget{}, false
	}
	maxVisible := setupModelMaxVisible(height, len(models))
	if maxVisible == 0 {
		return mouseSelectionTarget{}, false
	}
	m.setup.modelIndex = clampInt(m.setup.modelIndex, 0, len(models)-1)
	content := m.setupModelLines(width, height)
	top := setupContentTop(height, len(content), m.setup.err != "")
	rowStart := 4
	if m.setupModelStatus() != "" {
		rowStart++
	}
	row := mouseY(msg) - top - rowStart
	if row < 0 || row >= maxVisible {
		return mouseSelectionTarget{}, false
	}
	start := selectableListStart(len(models), maxVisible, m.setup.modelIndex)
	index := start + row
	if index < 0 || index >= len(models) {
		return mouseSelectionTarget{}, false
	}
	m.setup.modelIndex = index
	return mouseSelectionTarget{Scope: "first-run-model", Value: models[index].ID, Index: index}, true
}

func setupContentTop(height int, contentLines int, hasError bool) int {
	if hasError {
		contentLines += 2
	}
	return maxInt(0, (height-contentLines-3)/2)
}

func (m model) setupStages() []setupStage {
	stages := []setupStage{setupStageWelcome, setupStageMethod}
	if m.setup.oauthMode {
		// OAuth path skips endpoint/name/credentials because login provides the credential.
		return append(stages, setupStageProvider, setupStageModel, setupStageSafety, setupStageReady)
	}
	stages = append(stages, setupStageProvider)
	if setupProviderIsAimlapi(m.setupProvider()) {
		// Preserve the catalog's normal env-credential path in first-run setup.
		// A ready AIMLAPI_API_KEY needs no guided identity flow: model discovery
		// resolves it at runtime and saveSetupProvider persists APIKeyEnv, not the
		// materialized secret. /provider owns the richer balance/top-up preflight.
		if setupAimlapiEnvKey() != "" {
			return append(stages, setupStageModel, setupStageSafety, setupStageReady)
		}
		stages = append(stages, setupStageAimlapi, setupStageModel, setupStageSafety, setupStageReady)
		return stages
	}
	if setupProviderNeedsEndpoint(m.setupProvider()) {
		stages = append(stages, setupStageEndpoint, setupStageName)
	}
	stages = append(stages, setupStageCredentials, setupStageModel, setupStageSafety, setupStageReady)
	return stages
}

func setupAimlapiEnvKey() string {
	return strings.TrimSpace(os.Getenv("AIMLAPI_API_KEY"))
}

func setupProviderIsAimlapi(option SetupProviderOption) bool {
	return strings.EqualFold(strings.TrimSpace(option.ID), "aimlapi")
}

// setupReturnToMethod resets the OAuth selection when navigating back to the
// connect-method chooser.
func (m model) setupReturnToMethod() model {
	m.setup.oauthMode = false
	m.setup.oauthErr = ""
	m.setup.oauthDevice = false
	m.setup.deviceUserCode = ""
	m.setup.deviceVerificationURI = ""
	m.setup.providers = m.setup.allProviders
	m.setup.selected = 0
	m.resetSetupModels()
	return m
}

// setupMethodOptions returns the connect-method choices for this run, dropping the
// OAuth option when this setup's own provider list has no OAuth-capable entry — so
// the user can't pick OAuth and land on an empty provider list.
func (m model) setupMethodOptions() []providerWizardMethodOption {
	options := providerWizardMethodOptions()
	if len(setupOAuthProviderOptions(m.setup.allProviders)) > 0 {
		return options
	}
	filtered := make([]providerWizardMethodOption, 0, len(options))
	for _, option := range options {
		if option.oauth {
			continue
		}
		filtered = append(filtered, option)
	}
	return filtered
}

func (m *model) moveSetupMethod(delta int) {
	options := m.setupMethodOptions()
	if len(options) == 0 {
		return
	}
	m.setup.selectedMethod = ((m.setup.selectedMethod+delta)%len(options) + len(options)) % len(options)
}

// setupOAuthCmd runs the chosen provider's browser OAuth login off the UI
// goroutine for first-run setup. Mirrors the /provider wizard's flow.
func setupOAuthCmd(provider providercatalog.Descriptor, configPath string) tea.Cmd {
	path := configPath
	switch {
	case provider.OAuthMintsKey:
		return func() tea.Msg {
			if err := preflightOAuthProviderConfig(path, provider.ID); err != nil {
				return setupOAuthMsg{providerID: provider.ID, err: err}
			}
			key, err := provideroauth.OpenRouterLogin(context.Background(), provideroauth.OpenRouterOptions{
				OpenBrowser: browser.OpenURL,
				Timeout:     3 * time.Minute,
			})
			return setupOAuthMsg{apiKey: key, providerID: provider.ID, err: err}
		}
	case provider.ID == "chatgpt":
		return func() tea.Msg {
			err := runProviderChatGPTLogin(path)
			return setupOAuthMsg{tokenLogin: true, providerID: provider.ID, err: err}
		}
	default:
		name := provider.ID
		return func() tea.Msg {
			return setupOAuthMsg{tokenLogin: true, providerID: name, err: runProviderTokenLogin(name, path)}
		}
	}
}

// setupOAuthDeviceMsg carries phase 1 of device-code login (RequestDeviceCode)
// for first-run setup: the user_code + verification URI to display.
type setupOAuthDeviceMsg struct {
	providerID string
	userCode   string
	verifyURL  string
	cfg        oauth.Config
	auth       oauth.DeviceAuth
	err        error
}

func setupDevicePrepareCmd(name string, configPath string) tea.Cmd {
	return func() tea.Msg {
		if err := preflightOAuthProviderConfig(configPath, name); err != nil {
			return setupOAuthDeviceMsg{providerID: name, err: err}
		}
		auth, cfg, err := oauthDevicePrepare(name)
		if err != nil {
			return setupOAuthDeviceMsg{providerID: name, err: err}
		}
		return setupOAuthDeviceMsg{
			providerID: name,
			userCode:   auth.UserCode,
			verifyURL:  oauthDeviceVerifyTarget(auth),
			cfg:        cfg,
			auth:       auth,
		}
	}
}

func setupDevicePollCmd(name string, cfg oauth.Config, auth oauth.DeviceAuth, configPath string) tea.Cmd {
	return func() tea.Msg {
		path := configPath
		if err := preflightOAuthProviderConfig(path, name); err != nil {
			return setupOAuthMsg{tokenLogin: true, providerID: name, err: err}
		}
		return setupOAuthMsg{tokenLogin: true, providerID: name, err: oauthDeviceComplete(name, cfg, auth, path)}
	}
}

// startSetupDeviceLogin begins the device-code flow for the selected OAuth
// provider during first-run setup (phase 1).
func (m model) startSetupDeviceLogin(descriptor providercatalog.Descriptor) (tea.Model, tea.Cmd) {
	if !descriptor.OAuth || !descriptor.OAuthDeviceFlow {
		return m, nil
	}
	m.setup.oauthPending = true
	m.setup.oauthDevice = true
	m.setup.oauthErr = ""
	m.setup.deviceUserCode = ""
	m.setup.deviceVerificationURI = ""
	return m, setupDevicePrepareCmd(descriptor.ID, m.setup.configPath)
}

// applySetupOAuthDeviceCode handles phase 1 of device-code login: show the code,
// then start phase 2 (the token poll). On error the redacted message is shown.
func (m model) applySetupOAuthDeviceCode(msg setupOAuthDeviceMsg) (tea.Model, tea.Cmd) {
	if !m.setup.visible || !m.setup.oauthPending {
		return m, nil
	}
	// Ignore a stale result from a login the user has since replaced with a
	// different provider (an in-flight prepare landing after the switch).
	if msg.providerID != "" && msg.providerID != m.setupProviderDescriptor().ID {
		return m, nil
	}
	if msg.err != nil {
		m.setup.oauthPending = false
		m.setup.oauthDevice = false
		m.setup.oauthErr = redaction.RedactString(msg.err.Error(), redaction.Options{})
		return m, nil
	}
	m.setup.deviceUserCode = msg.userCode
	m.setup.deviceVerificationURI = msg.verifyURL
	return m, setupDevicePollCmd(msg.providerID, msg.cfg, msg.auth, m.setup.configPath)
}

// applySetupOAuth folds an OAuth login result into the first-run setup: on success
// the credential is captured (minted key) or relied upon (stored token) and setup
// jumps to model selection; on failure the redacted error is shown.
func (m model) applySetupOAuth(msg setupOAuthMsg) (tea.Model, tea.Cmd) {
	if !m.setup.visible || !m.setup.oauthPending {
		return m, nil
	}
	// Ignore a stale result for a provider the user has since switched away from,
	// so a late login can't capture a credential against the wrong provider.
	if msg.providerID != "" && msg.providerID != m.setupProviderDescriptor().ID {
		return m, nil
	}
	m.setup.oauthPending = false
	if msg.err != nil {
		m.setup.oauthErr = redaction.RedactString(msg.err.Error(), redaction.Options{})
		return m, nil
	}
	if msg.apiKey != "" {
		m.setup.apiKey.SetValue(msg.apiKey)
	}
	if msg.tokenLogin {
		// Early persist on the Update goroutine (see persistOAuthLoginProvider's
		// threading contract) so quitting setup after the login doesn't lose it;
		// do not advance when that write fails or the stored token would have no
		// reachable provider profile after setup exits.
		if _, err := persistOAuthLoginProvider(m.setup.configPath, msg.providerID); err != nil {
			m.setup.oauthErr = "Signed in, but the provider profile could not be saved: " + redaction.ErrorMessage(err, redaction.Options{})
			return m, nil
		}
	}
	m.setup.oauthErr = ""
	m.setup.err = ""
	m.setup.oauthDevice = false
	m.setup.deviceUserCode = ""
	m.setup.deviceVerificationURI = ""
	m.setup.stage = setupStageModel
	m.resetSetupModels()
	m.setup.modelErr = ""
	m.setup.modelGen++
	cmd := m.setupModelDiscoveryCmd(m.setup.modelGen)
	m.setup.modelLoad = cmd != nil
	return m, cmd
}

// handleSetupAimlapiKey delegates a key press to first-run setup's aimlapi.com
// sub-flow and reacts to its outcome.
func (m model) handleSetupAimlapiKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.setup.aimlapi == nil {
		return m, nil
	}
	cmd, outcome := m.setup.aimlapi.handleKey(msg)
	return m.resolveSetupAimlapi(cmd, outcome)
}

// applySetupAimlapiOnboard routes an async aimlapi.com onboarding result into
// first-run setup's sub-flow and reacts to its outcome.
func (m model) applySetupAimlapiOnboard(msg aimlapiOnboardMsg) (tea.Model, tea.Cmd) {
	if m.setup.aimlapi == nil || m.setup.aimlapi != msg.state {
		return m, nil
	}
	cmd, outcome := m.setup.aimlapi.apply(msg)
	return m.resolveSetupAimlapi(cmd, outcome)
}

// resolveSetupAimlapi lands the sub-flow outcome on first-run setup: a terminal
// key moves the issued key into the setup and jumps to model selection; a cancel
// backs out to the provider list.
func (m model) resolveSetupAimlapi(cmd tea.Cmd, outcome aimlapiOutcome) (tea.Model, tea.Cmd) {
	switch outcome {
	case aimlapiDone:
		m.setup.apiKey.SetValue(m.setup.aimlapi.apiKey)
		m.setup.baseURL = m.setup.aimlapi.baseURL
		m.setup.aimlapi = nil
		m.setup.err = ""
		m.setup.stage = setupStageModel
		m.resetSetupModels()
		m.setup.modelErr = ""
		m.setup.modelGen++
		discoverCmd := m.setupModelDiscoveryCmd(m.setup.modelGen)
		m.setup.modelLoad = discoverCmd != nil
		return m, discoverCmd
	case aimlapiCancel:
		m.setup.aimlapi = nil
		m.setup.err = ""
		m.setup.stage = setupStageProvider
		return m, nil
	}
	// Keep the shared spinner tick alive whenever the sub-flow just entered a busy
	// or progress state, so its animated spinner advances during onboarding.
	return m, tea.Batch(cmd, m.ensureSpinnerTick())
}

func (m model) nextSetupStage() setupStage {
	stages := m.setupStages()
	for index, stage := range stages {
		if stage == m.setup.stage {
			if index+1 < len(stages) {
				return stages[index+1]
			}
			return stage
		}
	}
	return m.setup.stage
}

func (m model) previousSetupStage() setupStage {
	// The aimlapi.com connect flow is terminal: once it has issued a key, its
	// transient state is cleared. Going back from model selection or Safety can
	// otherwise strand the user on an empty intermediate screen. Return to the
	// provider list so choosing aimlapi.com again starts a fresh connect flow.
	if (m.setup.stage == setupStageModel || m.setup.stage == setupStageSafety) &&
		setupProviderIsAimlapi(m.setupProvider()) {
		return setupStageProvider
	}
	stages := m.setupStages()
	for index, stage := range stages {
		if stage == m.setup.stage {
			if index > 0 {
				return stages[index-1]
			}
			return stage
		}
	}
	return m.setup.stage
}

func setupBlockContainsMouseX(x int, width int, blockWidth int) bool {
	if blockWidth <= 0 {
		return false
	}
	left := maxInt(0, (width-blockWidth)/2)
	return x >= left && x < left+blockWidth
}

func (m model) advanceSetup() (tea.Model, tea.Cmd) {
	if m.setup.stage < setupStageReady {
		if m.setup.stage == setupStageMethod {
			options := m.setupMethodOptions()
			m.setup.selectedMethod = clamp(m.setup.selectedMethod, 0, maxInt(0, len(options)-1))
			if len(options) > 0 && options[m.setup.selectedMethod].oauth {
				m.setup.oauthMode = true
				m.setup.providers = setupOAuthProviderOptions(m.setup.allProviders)
			} else {
				m.setup.oauthMode = false
				m.setup.providers = m.setup.allProviders
			}
			m.setup.selected = 0
			m.resetSetupModels()
			m.setup.stage = setupStageProvider
			m.setup.err = ""
			return m, nil
		}
		// OAuth path: advancing from the OAuth provider list starts the browser login.
		if m.setup.stage == setupStageProvider && m.setup.oauthMode {
			descriptor := m.setupProviderDescriptor()
			if descriptor.OAuth {
				// Headless/SSH boxes can't open a browser — use device code there
				// by default (the user can also force it with "d" from the list).
				if descriptor.OAuthDeviceFlow && oauthPreferDeviceFlow() {
					return m.startSetupDeviceLogin(descriptor)
				}
				m.setup.oauthPending = true
				m.setup.oauthDevice = false
				m.setup.oauthErr = ""
				return m, setupOAuthCmd(descriptor, m.setup.configPath)
			}
		}
		if m.setup.stage == setupStageProvider {
			m.setup.apiKey.SetValue("")
			m.setup.baseURL = ""
			m.setup.name = ""
		}
		if m.setup.stage == setupStageEndpoint {
			if err := providerWizardEndpointError(m.setup.baseURL); err != "" {
				m.setup.err = err
				return m, nil
			}
		}
		if m.setup.stage == setupStageModel && m.setup.modelLoad {
			m.setup.err = "Models are still loading."
			return m, nil
		}
		if m.setup.stage == setupStageModel && setupProviderUsesTypedModel(m.setupProvider()) && strings.TrimSpace(m.setup.modelQuery) == "" {
			m.setup.err = "Enter a model name before continuing."
			return m, nil
		}
		if m.setup.stage == setupStageModel && m.setupCurrentModel().ID == "" {
			m.setup.err = "Choose a matching model before continuing."
			return m, nil
		}
		m.setup.stage = m.nextSetupStage()
		m.setup.err = ""
		if m.setup.stage == setupStageAimlapi && m.setup.aimlapi == nil {
			m.setup.aimlapi = newAimlapiOnboard(browser.OpenURL)
		}
		if m.setup.stage == setupStageModel {
			m.resetSetupModels()
			m.setup.modelErr = ""
			m.setup.modelGen++
			cmd := m.setupModelDiscoveryCmd(m.setup.modelGen)
			m.setup.modelLoad = cmd != nil
			return m, cmd
		}
		return m, nil
	}
	return m.completeSetup()
}

func (m *model) moveSetupProvider(delta int) {
	if len(m.setup.providers) == 0 {
		return
	}
	m.setup.selected = ((m.setup.selected+delta)%len(m.setup.providers) + len(m.setup.providers)) % len(m.setup.providers)
	m.setup.apiKey.SetValue("")
	m.setup.baseURL = ""
	m.setup.name = ""
	m.setup.modelGen++
	m.resetSetupModels()
}

func (m model) completeSetup() (tea.Model, tea.Cmd) {
	option := m.setupProvider()
	if option.ID == "" {
		m.setup.err = "No provider option is available."
		return m, nil
	}
	if m.setupSave == nil {
		return m.exitSetupToChat()
	}

	name := m.setupProfileName(option)
	apiKey := m.setupCredentialAPIKey(option)
	// OAuth token login (e.g. xAI): the credential is the OAuth token stored under
	// provider:<id>; name the profile after the provider id so the runtime resolver
	// attaches the bearer, and store no API key.
	if m.setup.oauthMode && !m.setupProviderDescriptor().OAuthMintsKey {
		name = option.ID
		apiKey = ""
	}
	result, err := m.setupSave(SetupSelection{
		CatalogID: option.ID,
		Name:      name,
		BaseURL:   m.setupBaseURL(option),
		Model:     m.setupCurrentModel().ID,
		APIKey:    apiKey,
	})
	if err != nil {
		m.setup.err = err.Error()
		return m, nil
	}

	if result.ConfigPath != "" {
		m.setup.configPath = result.ConfigPath
	}
	if result.Provider.Name != "" {
		m.removedLiveRow = ""
		m.providerProfile = result.Provider
		m.providerName = result.Provider.Name
		m.modelName = result.Provider.Model
		// Export ZERO_PROVIDER alongside the committed profile fields (and the
		// config setupSave already persisted as active). Unlike command_center's
		// switch — which commits everything only after a successful build — setup
		// commits config + profile unconditionally here, so the env must match
		// them unconditionally too: applyEnv makes ZERO_PROVIDER WIN over config,
		// so a stale value from an earlier /model switch left in place would send
		// spawned children to the OLD provider even though config now names the
		// new one. That is the exact D3 gap this closes.
		config.SetActiveProviderEnv(result.Provider.Name)
		if m.newProvider != nil {
			if provider, providerErr := m.newProvider(result.Provider); providerErr == nil {
				m.provider = provider
			}
		}
	}

	return m.exitSetupToChat()
}

func (m *model) resetSetupModels() {
	option := m.setupProvider()
	provider := setupProviderDescriptor(option)
	if setupProviderUsesTypedModel(option) {
		m.setup.models = nil
		m.setup.modelIndex = 0
		m.setup.modelQuery = ""
		m.setup.modelForID = provider.ID
		m.setup.modelLoad = false
		m.setup.modelErr = ""
		m.setup.modelSrc = "manual"
		return
	}
	models := providerWizardModelOptions(provider)
	m.setup.models = models
	m.setup.modelIndex = 0
	m.setup.modelQuery = ""
	m.setup.modelForID = provider.ID
	m.setup.modelLoad = false
	m.setup.modelErr = ""
	m.setup.modelSrc = "fallback"
}

func (m model) setupModelDiscoveryCmd(gen uint64) tea.Cmd {
	option := m.setupProvider()
	provider := setupProviderDescriptor(option)
	if provider.ID == "" || !providercatalog.RuntimeSupported(provider) {
		return nil
	}
	if setupProviderUsesTypedModel(option) {
		return nil
	}
	pastedKey := m.setupCredentialAPIKey(option)
	// A token-login provider (e.g. xAI) keeps its bearer in the OAuth store, not as
	// a pasted key; resolve it so /models is authenticated and the live list shows
	// after sign-in. (OpenRouter mints a key into the credential already.)
	needOAuthToken := strings.TrimSpace(pastedKey) == "" && provider.OAuth && !provider.OAuthMintsKey
	baseSecrets := []string{m.setup.apiKey.Value()}
	discover := m.discoverProviderModels
	if discover == nil {
		discover = func(ctx context.Context, profile config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
			return providermodeldiscovery.DiscoverCatalog(ctx, provider, profile, providermodeldiscovery.Options{})
		}
	}
	providerID := provider.ID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 12*time.Second)
		defer cancel()
		apiKey := pastedKey
		if needOAuthToken {
			if resolved := oauthStoredToken(ctx, providerID); resolved != "" {
				apiKey = resolved
			}
		}
		profile := providerWizardDiscoveryProfile(provider, apiKey, m.setupBaseURL(option))
		secrets := append(append([]string{}, baseSecrets...), apiKey, profile.APIKey)
		models, err := discover(ctx, profile)
		return setupModelsDiscoveredMsg{providerID: providerID, gen: gen, redactionSecrets: secrets, models: models, err: err}
	}
}

func (m model) applySetupModelsDiscovered(msg setupModelsDiscoveredMsg) model {
	if !m.setup.visible || m.setup.stage != setupStageModel || m.setupProviderDescriptor().ID != msg.providerID || m.setup.modelGen != msg.gen {
		return m
	}
	m.setup.modelLoad = false
	m.setup.err = ""
	if msg.err != nil {
		m.setup.modelErr = redaction.RedactString(msg.err.Error(), redaction.Options{ExtraSecretValues: msg.redactionSecrets})
		m.setup.modelSrc = "fallback"
		if len(m.setup.models) == 0 {
			m.resetSetupModels()
		}
		return m
	}
	models := providerWizardModelsFromDiscovery(msg.models)
	if len(models) == 0 {
		m.setup.modelErr = "models endpoint returned no model ids"
		m.setup.modelSrc = "fallback"
		if len(m.setup.models) == 0 {
			m.resetSetupModels()
		}
		return m
	}
	currentID := m.setupCurrentModel().ID
	m.setup.models = models
	m.setup.modelIndex = 0
	m.setup.modelSrc = providerWizardModelsSource(msg.models)
	if m.setup.modelSrc == "" {
		m.setup.modelSrc = "live"
	}
	m.setup.modelErr = ""
	if currentID != "" {
		for index, model := range m.setupFilteredModels() {
			if model.ID == currentID {
				m.setup.modelIndex = index
				break
			}
		}
	}
	return m
}

func (m model) setupProviderDescriptor() providercatalog.Descriptor {
	return setupProviderDescriptor(m.setupProvider())
}

func setupProviderDescriptor(option SetupProviderOption) providercatalog.Descriptor {
	if descriptor, ok := providercatalog.Get(option.ID); ok {
		return descriptor
	}
	descriptor := providercatalog.Descriptor{
		ID:           strings.TrimSpace(option.ID),
		Name:         strings.TrimSpace(option.Name),
		DefaultModel: strings.TrimSpace(option.DefaultModel),
		RequiresAuth: option.RequiresAuth,
		Local:        option.Local,
		AuthEnvVars:  cleanSetupEnvVar(option.EnvVar),
	}
	return descriptor
}

func cleanSetupEnvVar(envVar string) []string {
	if envVar = strings.TrimSpace(envVar); envVar != "" {
		return []string{envVar}
	}
	return nil
}

func (m model) setupCurrentModel() providerWizardModel {
	if setupProviderUsesTypedModel(m.setupProvider()) {
		modelID := strings.TrimSpace(m.setup.modelQuery)
		if modelID == "" {
			return providerWizardModel{Description: "model name required"}
		}
		return providerWizardModel{ID: modelID, Description: "custom model"}
	}
	m.ensureSetupModels()
	models := m.setupFilteredModels()
	if len(models) == 0 {
		return providerWizardModel{Description: "no matching models"}
	}
	index := clamp(m.setup.modelIndex, 0, len(models)-1)
	return models[index]
}

func (m *model) ensureSetupModels() {
	option := m.setupProvider()
	providerID := setupProviderDescriptor(option).ID
	if len(m.setup.models) > 0 && m.setup.modelForID == providerID {
		return
	}
	m.resetSetupModels()
}

func (m model) setupFilteredModels() []providerWizardModel {
	query := strings.ToLower(strings.TrimSpace(m.setup.modelQuery))
	if query == "" {
		return append([]providerWizardModel{}, m.setup.models...)
	}
	models := make([]providerWizardModel, 0, len(m.setup.models))
	for _, model := range m.setup.models {
		if model.matches(query) {
			models = append(models, model)
		}
	}
	return models
}

func (m *model) moveSetupModel(delta int) {
	if m.setup.modelLoad {
		return
	}
	m.ensureSetupModels()
	models := m.setupFilteredModels()
	if len(models) == 0 {
		return
	}
	m.setup.modelIndex = ((m.setup.modelIndex+delta)%len(models) + len(models)) % len(models)
}

func (m *model) appendSetupModelQuery(runes []rune) {
	if m.setup.modelLoad {
		return
	}
	for _, r := range runes {
		if r < 32 || r == 127 {
			continue
		}
		m.setup.modelQuery += string(r)
	}
	m.setup.modelIndex = 0
	m.setup.err = ""
}

func (m *model) deleteSetupModelQueryRune() {
	if m.setup.modelLoad {
		return
	}
	if m.setup.modelQuery == "" {
		return
	}
	runes := []rune(m.setup.modelQuery)
	m.setup.modelQuery = string(runes[:len(runes)-1])
	m.setup.modelIndex = 0
	m.setup.err = ""
}

func (m model) exitSetupToChat() (tea.Model, tea.Cmd) {
	m.setup.visible = false
	m.headerPrinted = false
	m.flushQueue = nil
	m.printInFlight = false
	return m, nil
}

func (m model) setupCredentialInputActive() bool {
	return m.setup.stage == setupStageCredentials && setupProviderAcceptsAPIKey(m.setupProvider())
}

func (m model) setupEndpointInputActive() bool {
	return m.setup.stage == setupStageEndpoint && setupProviderNeedsEndpoint(m.setupProvider())
}

func (m model) setupNameInputActive() bool {
	return m.setup.stage == setupStageName && setupProviderNeedsProfileName(m.setupProvider())
}

func (m model) handleSetupEndpointKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case keyCtrl(msg, 'c'):
		return m, tea.Quit
	case keyIs(msg, tea.KeyEsc) || keyIs(msg, tea.KeyLeft):
		m.setup.stage = m.previousSetupStage()
		m.setup.err = ""
		return m, nil
	case keyIs(msg, tea.KeyEnter):
		return m.advanceSetup()
	case keyText(msg) != "":
		m.appendSetupBaseURL(keyRunes(msg))
	case keyBackspace(msg):
		m.deleteSetupBaseURLRune()
	case keyCtrl(msg, 'u'):
		m.setup.baseURL = ""
		m.setup.err = ""
	}
	return m, nil
}

func (m model) handleSetupNameKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case keyCtrl(msg, 'c'):
		return m, tea.Quit
	case keyIs(msg, tea.KeyEsc) || keyIs(msg, tea.KeyLeft):
		m.setup.stage = m.previousSetupStage()
		m.setup.err = ""
		return m, nil
	case keyIs(msg, tea.KeyEnter):
		return m.advanceSetup()
	case keyText(msg) != "":
		m.appendSetupName(keyRunes(msg))
	case keyBackspace(msg):
		m.deleteSetupNameRune()
	case keyCtrl(msg, 'u'):
		m.setup.name = ""
		m.setup.err = ""
	}
	return m, nil
}

func (m model) handleSetupCredentialKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case keyCtrl(msg, 'c'):
		return m, tea.Quit
	case keyIs(msg, tea.KeyEsc) || keyIs(msg, tea.KeyLeft):
		m.setup.stage = m.previousSetupStage()
		m.setup.err = ""
		return m, nil
	case keyIs(msg, tea.KeyEnter):
		return m.advanceSetup()
	case keyIs(msg, tea.KeyUp) || keyIs(msg, tea.KeyDown):
		return m, nil
	}
	previousAPIKey := m.setup.apiKey.Value()
	var cmd tea.Cmd
	m.setup.apiKey, cmd = m.setup.apiKey.Update(msg)
	if m.setup.apiKey.Value() != previousAPIKey {
		m.setup.modelGen++
		m.resetSetupModels()
	}
	m.setup.err = ""
	return m, cmd
}

func setupProviderAcceptsAPIKey(option SetupProviderOption) bool {
	return option.RequiresAuth && !option.Local
}

func setupProviderNeedsEndpoint(option SetupProviderOption) bool {
	return providerWizardNeedsEndpoint(setupProviderDescriptor(option))
}

func setupProviderNeedsProfileName(option SetupProviderOption) bool {
	return setupProviderNeedsEndpoint(option)
}

func setupProviderUsesTypedModel(option SetupProviderOption) bool {
	return setupProviderNeedsEndpoint(option)
}

func (m model) setupCredentialAPIKey(option SetupProviderOption) string {
	if !setupProviderAcceptsAPIKey(option) {
		return ""
	}
	return strings.TrimSpace(m.setup.apiKey.Value())
}

func (m model) setupBaseURL(option SetupProviderOption) string {
	// aimlapi.com never shows the endpoint field, but its onboarding resolves the
	// inference endpoint (honoring an AIMLAPI_INFERENCE_URL override) onto
	// m.setup.baseURL. Persist that so an override reaches the saved profile instead
	// of being replaced by the catalog default at save time.
	if providerWizardIsAimlapi(setupProviderDescriptor(option)) {
		if baseURL := strings.TrimSpace(m.setup.baseURL); baseURL != "" {
			return baseURL
		}
		return strings.TrimSpace(aimlapi.ResolveEndpoints().InferenceBaseURL)
	}
	if !setupProviderNeedsEndpoint(option) {
		return ""
	}
	return strings.TrimSpace(m.setup.baseURL)
}

func (m model) setupProfileName(option SetupProviderOption) string {
	if !setupProviderNeedsProfileName(option) {
		return ""
	}
	return providerWizardDisplayName(setupProviderDescriptor(option), m.setup.baseURL, m.setup.name)
}

func (m *model) appendSetupBaseURL(runes []rune) {
	for _, r := range runes {
		if r < 32 || r == 127 || unicode.IsSpace(r) {
			continue
		}
		m.setup.baseURL += string(r)
	}
	m.setup.err = ""
}

func (m *model) deleteSetupBaseURLRune() {
	if m.setup.baseURL == "" {
		return
	}
	runes := []rune(m.setup.baseURL)
	m.setup.baseURL = string(runes[:len(runes)-1])
	m.setup.err = ""
}

func (m *model) appendSetupName(runes []rune) {
	for _, r := range runes {
		if r < 32 || r == 127 || unicode.IsSpace(r) {
			continue
		}
		m.setup.name += string(r)
	}
	m.setup.err = ""
}

func (m *model) deleteSetupNameRune() {
	if m.setup.name == "" {
		return
	}
	runes := []rune(m.setup.name)
	m.setup.name = string(runes[:len(runes)-1])
	m.setup.err = ""
}

func (m model) setupProvider() SetupProviderOption {
	if len(m.setup.providers) == 0 {
		return SetupProviderOption{}
	}
	index := clamp(m.setup.selected, 0, len(m.setup.providers)-1)
	return m.setup.providers[index]
}

// setupErrorAffordance returns a faint one-line recovery hint tailored to the
// current setup stage, so a first-run error points the user at the inline fix
// instead of leaving them staring at a red line. Empty for stages whose footer
// already makes recovery obvious. Each hint names only keys the stage's footer
// confirms, so the guidance is always accurate.
func (m model) setupErrorAffordance() string {
	switch m.setup.stage {
	case setupStageEndpoint:
		return "→ edit the endpoint above, then Enter to retry (← to go back)"
	case setupStageCredentials:
		if m.setupCredentialInputActive() {
			return "→ re-enter the key above, then Enter to retry (← to go back)"
		}
	case setupStageName:
		return "→ edit the name above, then Enter to continue (← to go back)"
	case setupStageModel:
		if !m.setup.modelLoad {
			return "→ pick another model with ↑/↓, or type to search"
		}
	}
	return ""
}

func (m model) setupView(width int) string {
	if width <= 0 {
		width = defaultStartupWidth
	}
	height := normalizedStartupHeight(m.height)
	content := m.setupStageLines(width, height)
	if m.setup.err != "" {
		content = append(content, "", zeroTheme.red.Render("error: "+m.setup.err))
		if hint := m.setupErrorAffordance(); hint != "" {
			content = append(content, zeroTheme.faint.Render(hint))
		}
	}
	progress := m.setupProgressText()
	footer := m.setupFooter()

	topGap := maxInt(0, (height-len(content)-3)/2)
	bottomGap := maxInt(0, height-topGap-len(content)-2)
	lines := make([]string, 0, height)
	for i := 0; i < topGap; i++ {
		lines = append(lines, "")
	}
	lines = append(lines, centerSetupLines(content, width)...)
	for i := 0; i < bottomGap; i++ {
		lines = append(lines, "")
	}
	lines = append(lines, centerLine(fitStyledLine(progress, width), width))
	lines = append(lines, centerLine(fitStyledLine(footer, width), width))
	return strings.Join(lines, "\n")
}

func (m model) setupStageLines(width int, height int) []string {
	if m.setup.oauthPending {
		return m.setupOAuthWaitingLines(width)
	}
	switch m.setup.stage {
	case setupStageMethod:
		return m.setupMethodLines(width)
	case setupStageProvider:
		return m.setupProviderLines(width, height)
	case setupStageEndpoint:
		return m.setupEndpointLines(width)
	case setupStageName:
		return m.setupNameLines(width)
	case setupStageCredentials:
		return m.setupCredentialLines(width)
	case setupStageAimlapi:
		if m.setup.aimlapi != nil {
			return m.setup.aimlapi.view(width, m.spinnerGlyph())
		}
		return nil
	case setupStageModel:
		return m.setupModelLines(width, height)
	case setupStageSafety:
		return []string{
			zeroTheme.ink.Bold(true).Render("Safety"),
			"",
			"Zero asks before running shell commands or changing files.",
			"Unsafe mode stays off unless you explicitly enable it.",
			"",
			zeroTheme.faint.Render("Default: ask before risky work."),
		}
	case setupStageReady:
		return m.setupReadyLines(width)
	default:
		return []string{
			zeroTheme.accent.Render("Welcome to Zero"),
			"",
			zeroTheme.ink.Render("A terminal agent for changing real code."),
			zeroTheme.faint.Render("Plan changes, edit with approval, run checks, and resume sessions."),
		}
	}
}

type setupReadyRow struct {
	label string
	value string
}

func (m model) setupReadyLines(width int) []string {
	option := m.setupProvider()
	model := m.setupCurrentModel()
	providerName := displayValue(option.Name, option.ID)
	if name := m.setupProfileName(option); name != "" {
		providerName = name
	}

	rows := []setupReadyRow{{label: "provider", value: providerName}}
	if endpoint := m.setupBaseURL(option); endpoint != "" {
		rows = append(rows, setupReadyRow{label: "endpoint", value: endpoint})
	}
	rows = append(rows,
		setupReadyRow{label: "model", value: displayValue(model.ID, option.DefaultModel)},
		setupReadyRow{label: "credentials", value: m.setupCredentialSummary(option)},
		setupReadyRow{label: "config", value: shortenPath(displayValue(m.setup.configPath, "user config"))},
	)

	lines := []string{
		zeroTheme.ink.Bold(true).Render("Ready"),
		"",
		"Zero will save this setup and open chat.",
		"",
	}

	labelWidth := 0
	for _, row := range rows {
		labelWidth = maxInt(labelWidth, lipgloss.Width(row.label)+1)
	}
	rowWidth := setupReadyRowsWidth(width, labelWidth, rows)
	for _, row := range rows {
		label := fmt.Sprintf("%*s", labelWidth, row.label+":")
		line := "  " + zeroTheme.faint.Render(label) + "  " + zeroTheme.ink.Render(row.value)
		lines = append(lines, padSetupLine(line, rowWidth))
	}

	lines = append(lines,
		"",
		zeroTheme.faint.Render("Later, use /provider, /doctor, or /help anytime."),
	)
	return lines
}

func setupReadyRowsWidth(terminalWidth int, labelWidth int, rows []setupReadyRow) int {
	available := maxInt(28, minInt(terminalWidth-8, 86))
	target := 0
	for _, row := range rows {
		target = maxInt(target, 4+labelWidth+lipgloss.Width(row.value))
	}
	return minInt(target, available)
}

func (m model) setupModelLines(width int, height int) []string {
	if setupProviderUsesTypedModel(m.setupProvider()) {
		return m.setupTypedModelLines(width)
	}
	m.ensureSetupModels()
	if m.setup.modelLoad {
		return m.setupModelLoadingLines(width)
	}
	rowWidth := setupModelBlockWidth(width, m.setup.models)
	models := m.setupFilteredModels()
	maxVisible := setupModelMaxVisible(height, len(models))
	start := selectableListStart(len(models), maxVisible, m.setup.modelIndex)
	lines := []string{
		padSetupLine("  "+zeroTheme.ink.Bold(true).Render("Choose a model"), rowWidth),
		blankSetupBlockLine(rowWidth),
		padSetupLine("  "+m.setupModelSearchLine(rowWidth-2), rowWidth),
	}
	if status := m.setupModelStatus(); status != "" {
		lines = append(lines, padSetupLine("  "+zeroTheme.faint.Render(status), rowWidth))
	}
	lines = append(lines, blankSetupBlockLine(rowWidth))
	if len(models) == 0 {
		lines = append(lines, padSetupLine("  "+zeroTheme.faint.Render("No matching models"), rowWidth))
		return lines
	}
	visibleModels := models[start : start+maxVisible]
	for offset, model := range visibleModels {
		lines = append(lines, m.setupModelRow(rowWidth, start+offset, model))
	}
	detail := setupModelSelectedDetail(m.setupCurrentModel())
	lines = append(lines,
		blankSetupBlockLine(rowWidth),
		padSetupLine("  "+zeroTheme.faint.Render(detail), rowWidth),
	)
	return lines
}

func (m model) setupTypedModelLines(width int) []string {
	option := m.setupProvider()
	rowWidth := setupTextInputBlockWidth(width, option.DefaultModel)
	return []string{
		padSetupLine("  "+zeroTheme.ink.Bold(true).Render("Choose a model"), rowWidth),
		blankSetupBlockLine(rowWidth),
		padSetupLine("  "+zeroTheme.ink.Render("Enter the model ID this endpoint expects."), rowWidth),
		padSetupLine("  "+zeroTheme.faint.Render("Examples: gpt-4.1, claude-sonnet-4-5, llama-3.3-70b"), rowWidth),
		blankSetupBlockLine(rowWidth),
		padSetupLine("  "+providerWizardInputLine("model > ", strings.TrimSpace(m.setup.modelQuery), option.DefaultModel, rowWidth-2), rowWidth),
	}
}

func (m model) setupModelLoadingLines(width int) []string {
	rowWidth := setupModelLoadingBlockWidth(width)
	return []string{
		padSetupLine("  "+zeroTheme.ink.Bold(true).Render("Choose a model"), rowWidth),
		blankSetupBlockLine(rowWidth),
		padSetupLine("  "+zeroTheme.faint.Render("Checking available models..."), rowWidth),
		padSetupLine("  "+zeroTheme.faint.Render("Built-in models will be used if discovery fails."), rowWidth),
	}
}

func setupTextInputBlockWidth(terminalWidth int, values ...string) int {
	available := maxInt(34, minInt(terminalWidth-8, 72))
	target := 42
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			target = maxInt(target, lipgloss.Width("  "+value)+8)
		}
	}
	return minInt(target, available)
}

func setupModelMaxVisible(height int, total int) int {
	if total <= 0 {
		return 0
	}
	maxVisible := height - 12
	if maxVisible < 5 {
		maxVisible = 5
	}
	if maxVisible > 18 {
		maxVisible = 18
	}
	if maxVisible > total {
		return total
	}
	return maxVisible
}

func setupModelLoadingBlockWidth(terminalWidth int) int {
	available := maxInt(34, minInt(terminalWidth-8, 72))
	target := maxInt(lipgloss.Width("  Choose a model"), lipgloss.Width("  Built-in models will be used if discovery fails."))
	return minInt(maxInt(target, 42), available)
}

func setupModelBlockWidth(terminalWidth int, models []providerWizardModel) int {
	available := maxInt(34, minInt(terminalWidth-8, 72))
	target := lipgloss.Width("  Choose a model")
	target = maxInt(target, lipgloss.Width("  search > model name..."))
	for _, model := range models {
		target = maxInt(target, 4+lipgloss.Width(model.displayLabel()))
		if detail := setupModelSelectedDetail(model); detail != "" {
			target = maxInt(target, lipgloss.Width("  "+detail))
		}
	}
	target = maxInt(target, 42)
	return minInt(target, available)
}

func (m model) setupModelSearchLine(width int) string {
	query := strings.TrimSpace(m.setup.modelQuery)
	prompt := zeroTheme.userPrompt.Render("search > ")
	cursor := zeroTheme.accent.Render("▌")
	if query == "" {
		return fitStyledLine(prompt+cursor+zeroTheme.faint.Render("model name..."), width)
	}
	return fitStyledLine(prompt+zeroTheme.ink.Render(query)+cursor, width)
}

func (m model) setupModelStatus() string {
	if m.setup.modelLoad {
		return "Refreshing available models"
	}
	if m.setup.modelErr != "" {
		return "Using built-in model list"
	}
	return ""
}

func (m model) setupModelRow(width int, index int, model providerWizardModel) string {
	selected := index == m.setup.modelIndex
	marker := "  "
	style := zeroTheme.ink
	if selected {
		marker = "❯ "
		style = zeroTheme.accent.Bold(true)
	}
	left := marker + style.Render(model.displayLabel())
	return padSetupLine(left, width)
}

func setupModelSelectedDetail(model providerWizardModel) string {
	parts := []string{}
	if secondary := strings.TrimSpace(model.secondaryText()); secondary != "" && !providerWizardGenericModelDescription(secondary) {
		parts = append(parts, secondary)
	}
	if meta := strings.TrimSpace(model.Meta); meta != "" {
		parts = append(parts, meta)
	}
	return strings.Join(parts, " · ")
}

func (m model) setupMethodLines(width int) []string {
	options := m.setupMethodOptions()
	rowWidth := setupMethodBlockWidth(width, options)
	idx := clamp(m.setup.selectedMethod, 0, maxInt(0, len(options)-1))
	lines := []string{
		padSetupLine("  "+zeroTheme.ink.Bold(true).Render("How do you want to connect?"), rowWidth),
		blankSetupBlockLine(rowWidth),
	}
	for index, option := range options {
		marker := "  "
		style := zeroTheme.ink
		if index == idx {
			marker = "❯ "
			style = zeroTheme.accent.Bold(true)
		}
		lines = append(lines, padSetupLine(marker+style.Render(option.label), rowWidth))
		lines = append(lines, padSetupLine("    "+zeroTheme.faint.Render(option.subtitle), rowWidth))
	}
	return lines
}

func setupMethodBlockWidth(terminalWidth int, options []providerWizardMethodOption) int {
	available := maxInt(34, minInt(terminalWidth-8, 86))
	target := lipgloss.Width("  How do you want to connect?")
	for _, option := range options {
		target = maxInt(target, 2+lipgloss.Width(option.label))
		target = maxInt(target, 4+lipgloss.Width(option.subtitle))
	}
	return minInt(maxInt(target, 42), available)
}

func (m model) setupOAuthWaitingLines(width int) []string {
	provider := m.setupProviderDescriptor()
	name := displayValue(provider.Name, provider.ID)
	var lines []string
	if m.setup.oauthDevice {
		lines = []string{zeroTheme.ink.Bold(true).Render("Device-code sign-in for " + name), ""}
		if m.setup.deviceUserCode == "" {
			lines = append(lines, zeroTheme.faint.Render("Requesting a device code..."))
		} else {
			lines = append(lines,
				"1. On any device, visit:  "+zeroTheme.accent.Render(m.setup.deviceVerificationURI),
				"2. Enter the code:  "+zeroTheme.accent.Bold(true).Render(m.setup.deviceUserCode),
				"",
				zeroTheme.faint.Render("Waiting for authorization..."),
			)
		}
	} else {
		lines = []string{
			zeroTheme.ink.Bold(true).Render("Signing in with " + name),
			"",
			"Opening your browser — approve there, then return here.",
			zeroTheme.faint.Render("Waiting for authorization..."),
			"",
			zeroTheme.faint.Render("If your browser didn't open, run:  " + providerWizardOAuthCLIHint(provider)),
		}
	}
	if m.setup.oauthErr != "" {
		lines = append(lines, "", zeroTheme.red.Render("Sign-in failed: "+m.setup.oauthErr))
	}
	return lines
}

func (m model) setupProviderLines(width int, height int) []string {
	rowWidth := setupProviderBlockWidth(width, m.setup.providers)
	maxVisible := setupProviderMaxVisible(height, len(m.setup.providers))
	start := selectableListStart(len(m.setup.providers), maxVisible, m.setup.selected)
	visibleProviders := m.setup.providers[start : start+maxVisible]
	lines := []string{
		padSetupLine("  "+zeroTheme.ink.Bold(true).Render("Choose a provider"), rowWidth),
		blankSetupBlockLine(rowWidth),
	}
	for index, option := range visibleProviders {
		absoluteIndex := start + index
		marker := "  "
		style := zeroTheme.ink
		if absoluteIndex == m.setup.selected {
			marker = "❯ "
			style = zeroTheme.accent.Bold(true)
		}
		label := displayValue(option.Name, option.ID)
		if option.Recommended {
			label = "★ " + label
		}
		line := marker + style.Render(label)
		if option.Recommended {
			line += zeroTheme.faint.Render("  (recommended)")
		}
		lines = append(lines, padSetupLine(line, rowWidth))
	}
	// A failed OAuth login drops back here with oauthPending cleared, so the
	// waiting screen (which owns the error) is gone — surface it on the list the
	// user lands on so the failure isn't silent and they can retry.
	if m.setup.oauthMode && m.setup.oauthErr != "" {
		lines = append(lines,
			blankSetupBlockLine(rowWidth),
			padSetupLine("  "+zeroTheme.red.Render("Sign-in failed: "+m.setup.oauthErr), rowWidth),
		)
	}
	return lines
}

func (m model) setupEndpointLines(width int) []string {
	option := m.setupProvider()
	provider := setupProviderDescriptor(option)
	rowWidth := setupTextInputBlockWidth(width, providerWizardEndpointPlaceholder(provider), m.setup.baseURL)
	return []string{
		padSetupLine("  "+zeroTheme.ink.Bold(true).Render("Endpoint URL"), rowWidth),
		blankSetupBlockLine(rowWidth),
		padSetupLine("  "+zeroTheme.ink.Render("Enter the API base URL for "+displayValue(option.Name, option.ID)+"."), rowWidth),
		padSetupLine("  "+zeroTheme.faint.Render(providerWizardEndpointHint(provider)), rowWidth),
		blankSetupBlockLine(rowWidth),
		padSetupLine("  "+providerWizardInputLine("url > ", strings.TrimSpace(m.setup.baseURL), providerWizardEndpointPlaceholder(provider), rowWidth-2), rowWidth),
	}
}

func (m model) setupNameLines(width int) []string {
	option := m.setupProvider()
	provider := setupProviderDescriptor(option)
	name := providerWizardDisplayName(provider, m.setup.baseURL, m.setup.name)
	rowWidth := setupTextInputBlockWidth(width, name, m.setup.name)
	return []string{
		padSetupLine("  "+zeroTheme.ink.Bold(true).Render("Provider name"), rowWidth),
		blankSetupBlockLine(rowWidth),
		padSetupLine("  "+zeroTheme.ink.Render("Choose the short label shown in Zero."), rowWidth),
		padSetupLine("  "+zeroTheme.faint.Render("Leave blank to use "+name+"."), rowWidth),
		blankSetupBlockLine(rowWidth),
		padSetupLine("  "+providerWizardInputLine("name > ", strings.TrimSpace(m.setup.name), name, rowWidth-2), rowWidth),
	}
}

func setupProviderMaxVisible(height int, total int) int {
	if total <= 0 {
		return 0
	}
	maxVisible := height - 8
	if maxVisible < 6 {
		maxVisible = 6
	}
	if maxVisible > total {
		return total
	}
	return maxVisible
}

func setupProviderBlockWidth(terminalWidth int, providers []SetupProviderOption) int {
	available := maxInt(24, minInt(terminalWidth-8, 44))
	target := maxInt(lipgloss.Width("  2/6"), lipgloss.Width("  Choose a provider"))
	for _, provider := range providers {
		label := displayValue(provider.Name, provider.ID)
		if provider.Recommended {
			label = "★ " + label + "  (recommended)"
		}
		target = maxInt(target, 2+lipgloss.Width(label))
	}
	target = maxInt(target, 32)
	return minInt(target, available)
}

func blankSetupBlockLine(width int) string {
	if width <= 0 {
		return ""
	}
	return strings.Repeat(" ", width)
}

func (m model) setupCredentialLines(width int) []string {
	option := m.setupProvider()
	lines := []string{
		zeroTheme.ink.Bold(true).Render("Credentials"),
		"",
	}
	if option.Local || !option.RequiresAuth {
		lines = append(lines,
			displayValue(option.Name, option.ID)+" does not need an API key.",
			"Start the local server before sending a prompt.",
		)
		return lines
	}
	envVar := displayValue(option.EnvVar, "the provider API key env var")
	lines = append(lines,
		"Paste your "+displayValue(option.Name, option.ID)+" API key",
		"or leave blank to use "+envVar+" from your shell.",
		"",
		m.setupAPIKeyInputLine(width),
		"",
		zeroTheme.faint.Render("Saved keys stay in your user config."),
		zeroTheme.faint.Render("Blank uses "+envVar+" from your shell."),
	)
	return lines
}

func (m model) setupAPIKeyInputLine(width int) string {
	input := m.setup.apiKey
	if strings.TrimSpace(input.Value()) == "" {
		return zeroTheme.faint.Render(input.Placeholder)
	}
	contentWidth := lipgloss.Width(input.Value())
	if contentWidth == 0 {
		contentWidth = lipgloss.Width(input.Placeholder)
	}
	input.SetWidth(minInt(maxInt(contentWidth, 1), maxInt(1, width-lipgloss.Width(input.Prompt))))
	return input.View()
}

func (m model) setupCredentialSummary(option SetupProviderOption) string {
	// An OAuth token-login provider (e.g. xAI) is authenticated by a stored,
	// refreshable token, not an API key — don't advertise an env var for it on the
	// Ready screen. (Key-minting OAuth like OpenRouter still ends up as a saved key.)
	if m.setup.oauthMode {
		if d, ok := providercatalog.Get(option.ID); ok && d.OAuth && !d.OAuthMintsKey {
			return "OAuth token"
		}
	}
	if !setupProviderAcceptsAPIKey(option) {
		return "not required"
	}
	if m.setupCredentialAPIKey(option) != "" {
		return "saved API key"
	}
	return "env var " + displayValue(option.EnvVar, "provider API key")
}

func (m model) setupFooter() string {
	if m.setup.oauthPending {
		return zeroTheme.faint.Render("Esc cancel")
	}
	switch m.setup.stage {
	case setupStageMethod:
		return zeroTheme.faint.Render("↑/↓ choose   ") + zeroTheme.accent.Render("Enter") + zeroTheme.faint.Render(" continue   q quit")
	case setupStageReady:
		return zeroTheme.accent.Render("Enter") + zeroTheme.faint.Render(" to save and start chat")
	case setupStageEndpoint:
		return zeroTheme.accent.Render("Enter") + zeroTheme.faint.Render(" continue   left back")
	case setupStageName:
		return zeroTheme.faint.Render("name optional   ") + zeroTheme.accent.Render("Enter") + zeroTheme.faint.Render(" continue   left back")
	case setupStageCredentials:
		if m.setupCredentialInputActive() {
			return zeroTheme.faint.Render("paste key optional   ") + zeroTheme.accent.Render("Enter") + zeroTheme.faint.Render(" continue   left back")
		}
		return zeroTheme.accent.Render("Space") + zeroTheme.faint.Render(" to continue")
	case setupStageAimlapi:
		return zeroTheme.faint.Render("type/↑↓ choose   ") + zeroTheme.accent.Render("Enter") + zeroTheme.faint.Render(" continue   left back   Esc cancel")
	case setupStageProvider:
		if m.setup.oauthMode && m.setupProviderDescriptor().OAuthDeviceFlow {
			return zeroTheme.faint.Render("↑/↓ choose   ") + zeroTheme.accent.Render("Enter") + zeroTheme.faint.Render(" sign in   ") + zeroTheme.accent.Render("d") + zeroTheme.faint.Render(" device code   q quit")
		}
		return zeroTheme.faint.Render("↑/↓ choose   ") + zeroTheme.accent.Render("Enter") + zeroTheme.faint.Render(" continue   q quit")
	case setupStageModel:
		if m.setup.modelLoad {
			return zeroTheme.faint.Render("checking models...")
		}
		return zeroTheme.faint.Render("↑/↓ choose   type search   ") + zeroTheme.accent.Render("Enter") + zeroTheme.faint.Render(" continue")
	case setupStageWelcome:
		return zeroTheme.accent.Render("Space") + zeroTheme.faint.Render(" to set up Zero")
	default:
		return zeroTheme.accent.Render("Space") + zeroTheme.faint.Render(" to continue")
	}
}

func centerSetupLines(lines []string, width int) []string {
	fitted := make([]string, 0, len(lines))
	for _, line := range lines {
		fitted = append(fitted, centerLine(fitStyledLine(line, width), width))
	}
	return fitted
}

func (m model) setupProgressText() string {
	stages := m.setupStages()
	position := 0
	for index, stage := range stages {
		if stage == m.setup.stage {
			position = index
			break
		}
	}
	return zeroTheme.faint.Render(fmt.Sprintf("%d/%d", position+1, len(stages)))
}

func padSetupLine(line string, width int) string {
	if width <= 0 {
		return line
	}
	if pad := width - lipgloss.Width(line); pad > 0 {
		return line + strings.Repeat(" ", pad)
	}
	return fitStyledLine(line, width)
}
