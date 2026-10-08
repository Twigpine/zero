package tui

import (
	"context"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/Gitlawb/zero/internal/aimlapi"
	"github.com/Gitlawb/zero/internal/browser"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/oauth"
	"github.com/Gitlawb/zero/internal/providercatalog"
	"github.com/Gitlawb/zero/internal/provideroauth"
	"github.com/Gitlawb/zero/internal/redaction"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// providerWizardOAuthMsg carries the result of an in-wizard browser OAuth login.
// apiKey is set when the flow mints a key (OpenRouter); tokenLogin is set when the
// flow stored an OAuth token in the oauth store (xAI) that the runtime resolver
// will attach — in that case no key is needed on the profile.
type providerWizardOAuthMsg struct {
	providerID string
	attemptID  int
	apiKey     string
	tokenLogin bool
	err        error
}

// applyProviderWizardOAuth folds an OAuth login result into the wizard: on
// success the minted key fills the credential and the wizard advances; on failure
// the (redacted) error is shown and the user can retry or paste a key.
func (m model) applyProviderWizardOAuth(msg providerWizardOAuthMsg) (model, tea.Cmd) {
	if m.providerWizard == nil || !m.providerWizard.oauthResultMatches(msg.providerID, msg.attemptID) {
		return m, nil
	}
	m.providerWizard.oauthPending = false
	if msg.err != nil {
		m.providerWizard.oauthErr = redaction.ErrorMessage(msg.err, redaction.Options{})
		return m, nil
	}
	if msg.apiKey != "" {
		m.providerWizard.apiKey = msg.apiKey
	}
	if msg.tokenLogin {
		// Persist the profile HERE, on the Update goroutine (see
		// persistOAuthLoginProvider for the threading contract), and only mirror
		// it into the in-memory saved list when the write succeeded — a mirror of
		// a profile that never reached disk shows a phantom provider that works
		// this session and silently vanishes on restart.
		if err := persistOAuthLoginProvider(m.userConfigPath, msg.providerID); err != nil {
			m.providerWizard.oauthErr = "Signed in, but the provider profile could not be saved: " + redaction.ErrorMessage(err, redaction.Options{})
			return m, nil
		}
		m.savedProviders = appendOAuthLoginProfile(m.savedProviders, msg.providerID)
	}
	// OAuth succeeded (key minted, or a refreshable token stored for the runtime
	// resolver). Skip the endpoint/credential steps and go straight to model
	// selection.
	m.providerWizard.err = ""
	m.providerWizard.oauthErr = ""
	m.providerWizard.oauthDevice = false
	m.providerWizard.deviceUserCode = ""
	m.providerWizard.deviceVerificationURI = ""
	m.providerWizard.step = providerWizardStepModel
	return m, m.providerModelDiscoveryCmd()
}

// applyProviderWizardAimlapiOnboard routes an async aimlapi.com onboarding result
// into the wizard's shared sub-flow and reacts to its outcome.
func (m model) applyProviderWizardAimlapiOnboard(msg aimlapiOnboardMsg) (model, tea.Cmd) {
	wizard := m.providerWizard
	if wizard == nil || wizard.aimlapi == nil || wizard.aimlapi != msg.state {
		return m, nil
	}
	cmd, outcome := wizard.aimlapi.apply(msg)
	return m.resolveProviderWizardAimlapi(cmd, outcome)
}

// handleProviderWizardAimlapiKey delegates a key press to the wizard's aimlapi.com
// sub-flow and reacts to its outcome.
func (m model) handleProviderWizardAimlapiKey(msg tea.KeyMsg) (model, tea.Cmd) {
	wizard := m.providerWizard
	if wizard == nil || wizard.aimlapi == nil {
		return m, nil
	}
	cmd, outcome := wizard.aimlapi.handleKey(msg)
	return m.resolveProviderWizardAimlapi(cmd, outcome)
}

// resolveProviderWizardAimlapi lands the sub-flow's outcome on the wizard: on a
// terminal key it moves the freshly acquired key into the wizard and advances to
// model selection (reusing the standard finalize); on cancel it backs out.
func (m model) resolveProviderWizardAimlapi(cmd tea.Cmd, outcome aimlapiOutcome) (model, tea.Cmd) {
	wizard := m.providerWizard
	switch outcome {
	case aimlapiDone:
		wizard.apiKey = wizard.aimlapi.apiKey
		wizard.baseURL = wizard.aimlapi.baseURL
		wizard.aimlapi = nil
		wizard.err = ""
		wizard.step = providerWizardStepModel
		return m, m.providerModelDiscoveryCmd()
	case aimlapiCancel:
		wizard.aimlapi = nil
		wizard.err = ""
		wizard.step = providerWizardStepProvider
		return m, nil
	}
	// Keep the shared spinner tick alive whenever the sub-flow just entered a busy
	// or progress state, so its animated spinner advances during onboarding.
	return m, tea.Batch(cmd, m.ensureSpinnerTick())
}

// applyProviderWizardDeviceCode handles phase 1 of device-code login: show the
// user_code + verification URI, then kick off phase 2 (the token poll). On error
// the redacted message is surfaced and the login is abandoned.
func (m model) applyProviderWizardDeviceCode(msg providerWizardDeviceCodeMsg) (model, tea.Cmd) {
	if m.providerWizard == nil || !m.providerWizard.oauthResultMatches(msg.providerID, msg.attemptID) {
		return m, nil
	}
	if msg.err != nil {
		m.providerWizard.oauthPending = false
		m.providerWizard.oauthDevice = false
		m.providerWizard.oauthErr = redaction.ErrorMessage(msg.err, redaction.Options{})
		return m, nil
	}
	m.providerWizard.deviceUserCode = msg.userCode
	m.providerWizard.deviceVerificationURI = msg.verifyURL
	return m, providerWizardDevicePollCmd(msg.providerID, msg.attemptID, msg.cfg, msg.auth)
}

// providerWizardSupportsOAuth reports whether the credential step should offer a
// browser "Log in with OAuth" option for this provider. Only providers whose
// OAuth flow yields a credential usable directly (OpenRouter mints an API key)
// are offered — subscription providers (ChatGPT/Claude) use the proxy preset, and
// API-key-only providers just paste a key.
func providerWizardSupportsOAuth(provider providercatalog.Descriptor) bool {
	return provider.OAuth
}

// providerWizardOAuthCmdFor runs the chosen provider's browser OAuth login off the
// UI goroutine and reports the outcome. OpenRouter mints an API key; ChatGPT
// (Codex) needs the bespoke flow that extracts the `chatgpt_account_id` claim
// from the ID token and stores it on the saved token so the Codex provider can
// inject it as a header on every request; other OAuth providers (xAI) run the
// generic engine login which stores a refreshable token.
func providerWizardOAuthCmdFor(provider providercatalog.Descriptor, attemptID int) tea.Cmd {
	providerID := provider.ID
	switch {
	case provider.OAuthMintsKey:
		return func() tea.Msg {
			key, err := provideroauth.OpenRouterLogin(context.Background(), provideroauth.OpenRouterOptions{
				OpenBrowser: browser.OpenURL,
				Timeout:     3 * time.Minute,
			})
			return providerWizardOAuthMsg{providerID: providerID, attemptID: attemptID, apiKey: key, err: err}
		}
	case providerID == "chatgpt":
		return func() tea.Msg {
			err := runProviderChatGPTLogin()
			return providerWizardOAuthMsg{providerID: providerID, attemptID: attemptID, tokenLogin: true, err: err}
		}
	default:
		return func() tea.Msg {
			return providerWizardOAuthMsg{providerID: providerID, attemptID: attemptID, tokenLogin: true, err: runProviderTokenLogin(providerID)}
		}
	}
}

// runProviderChatGPTLogin runs the ChatGPT (Codex) bespoke login (which
// extracts the `chatgpt_account_id` claim from the ID token and stores it on
// the token's Account field) and persists the resulting token via the oauth
// store. The runtime resolver then attaches the bearer to Codex calls and the
// Codex provider reads the Account field for the `chatgpt-account-id` header.
func runProviderChatGPTLogin() error {
	env := buildOAuthPresetEnv()
	token, err := provideroauth.ChatGPTLogin(context.Background(), provideroauth.ChatGPTOptions{
		Env:         env,
		HTTPClient:  &http.Client{Timeout: 60 * time.Second},
		OpenBrowser: browser.OpenURL,
		Timeout:     3 * time.Minute,
	})
	if err != nil {
		return err
	}
	store, err := oauth.NewStore(oauth.StoreOptions{})
	if err != nil {
		return err
	}
	return store.Save(oauth.ProviderKey("chatgpt"), token)
}

// appendOAuthLoginProfile mirrors the profile persistOAuthLoginProvider wrote to
// config into an in-memory saved-provider list, skipping when a profile already
// serves the catalog entry (by name or catalog id).
func appendOAuthLoginProfile(saved []config.ProviderProfile, providerID string) []config.ProviderProfile {
	descriptor, ok := providercatalog.Get(providerID)
	if !ok {
		return saved
	}
	for _, profile := range saved {
		if strings.EqualFold(strings.TrimSpace(profile.CatalogID), descriptor.ID) ||
			strings.EqualFold(strings.TrimSpace(profile.Name), descriptor.ID) {
			return saved
		}
	}
	return append(saved, config.ProviderProfile{
		Name:         descriptor.ID,
		ProviderKind: providerWizardProviderKind(descriptor),
		CatalogID:    descriptor.ID,
		BaseURL:      descriptor.DefaultBaseURL,
		Model:        descriptor.DefaultModel,
	})
}

// persistOAuthLoginProvider guarantees the stored login is reachable from the
// provider list even when the wizard is abandoned before the model step: the
// token alone is invisible — no profile means no entry in /provider, /model, or
// `zero providers use`, which reads as the login having vanished.
//
// MUST run on the Update goroutine (call it from a message handler, never from
// inside a tea.Cmd): config.json writes are unlocked read-modify-writes, and a
// background login — a device-code poll can complete minutes later — racing a
// UI-thread write would silently clobber it (lost update). The returned error
// lets the caller surface a failed write instead of showing a provider that
// never reached disk.
func persistOAuthLoginProvider(configPath string, catalogID string) error {
	if strings.TrimSpace(configPath) == "" {
		return nil
	}
	_, err := config.EnsureCatalogProvider(configPath, catalogID)
	return err
}

// buildOAuthPresetEnv layers the process env with the preset opt-in so a
// `zero` launch from a TUI session can use the baked-in ChatGPT client_id
// without requiring the user to export ZERO_OAUTH_ALLOW_PRESETS themselves.
func buildOAuthPresetEnv() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if eq := strings.IndexByte(kv, '='); eq > 0 {
			env[kv[:eq]] = kv[eq+1:]
		}
	}
	env["ZERO_OAUTH_ALLOW_PRESETS"] = "1"
	return env
}

// runProviderTokenLogin runs the generic OAuth engine login for a provider that
// has a built-in preset (e.g. xAI), storing a refreshable token under
// provider:<name>. The runtime resolver then attaches it to model calls.
func runProviderTokenLogin(name string) error {
	store, err := oauth.NewStore(oauth.StoreOptions{})
	if err != nil {
		return err
	}
	manager, err := oauth.NewManager(oauth.ManagerOptions{
		Store:       store,
		HTTPClient:  &http.Client{Timeout: 60 * time.Second},
		OpenBrowser: browser.OpenURL,
		// The user explicitly chose to sign in with this provider's OAuth, so opt
		// into its baked-in preset (e.g. xAI's public client_id); without this the
		// config never resolves and the browser never opens.
		AllowPresets: true,
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	_, err = manager.Login(ctx, oauth.LoginOptions{Provider: name})
	return err
}

// providerWizardDeviceCodeMsg carries the result of phase 1 (RequestDeviceCode):
// the user_code + verification URI to display, plus the cfg/auth to poll with.
type providerWizardDeviceCodeMsg struct {
	providerID string
	attemptID  int
	userCode   string
	verifyURL  string
	cfg        oauth.Config
	auth       oauth.DeviceAuth
	err        error
}

// providerWizardDevicePrepareCmd runs phase 1 of the device-code login off the UI
// goroutine and reports the code to display (or an error).
func providerWizardDevicePrepareCmd(name string, attemptID int) tea.Cmd {
	return func() tea.Msg {
		auth, cfg, err := oauthDevicePrepare(name)
		if err != nil {
			return providerWizardDeviceCodeMsg{providerID: name, attemptID: attemptID, err: err}
		}
		return providerWizardDeviceCodeMsg{
			providerID: name,
			attemptID:  attemptID,
			userCode:   auth.UserCode,
			verifyURL:  oauthDeviceVerifyTarget(auth),
			cfg:        cfg,
			auth:       auth,
		}
	}
}

// providerWizardDevicePollCmd runs phase 2 (poll for the token + store) off the
// UI goroutine and reports completion as a regular OAuth result.
func providerWizardDevicePollCmd(name string, attemptID int, cfg oauth.Config, auth oauth.DeviceAuth) tea.Cmd {
	return func() tea.Msg {
		return providerWizardOAuthMsg{providerID: name, attemptID: attemptID, tokenLogin: true, err: oauthDeviceComplete(name, cfg, auth)}
	}
}

// startProviderDeviceLogin begins the device-code flow for the selected OAuth
// provider (phase 1). Used by the headless default and the "d" shortcut.
func (m model) startProviderDeviceLogin() (model, tea.Cmd) {
	provider := m.providerWizard.currentProvider()
	if !provider.OAuth || !provider.OAuthDeviceFlow {
		return m, nil
	}
	attemptID := m.providerWizard.beginOAuthAttempt(true)
	return m, providerWizardDevicePrepareCmd(provider.ID, attemptID)
}

const maxProviderWizardProvidersVisible = 10
const maxProviderWizardModelsVisible = 10
const providerWizardMinWidth = 48
const providerWizardProviderWidth = 64
const providerWizardMediumWidth = 86
const providerWizardModelWidth = 92

type providerWizardStep int

const (
	providerWizardStepMethod providerWizardStep = iota
	providerWizardStepProvider
	providerWizardStepManageKey
	providerWizardStepEndpoint
	providerWizardStepName
	providerWizardStepAimlapiConfigured
	providerWizardStepAimlapi
	providerWizardStepCredential
	providerWizardStepModel
	providerWizardStepDone
	// Manager steps (provider_manager.go): the list-first /provider surface.
	// They live on the same state struct so every overlay gate, key route, and
	// mouse hook that already checks m.providerWizard covers them for free.
	providerWizardStepManage
	providerWizardStepEditMenu
	providerWizardStepEditValue
)

// providerWizardMethodOption is a row in the "How do you want to connect?" step.
type providerWizardMethodOption struct {
	oauth    bool
	label    string
	subtitle string
}

// providerWizardMethodOptions returns the connect-method rows. OAuth is listed
// first (and is the default) when any OAuth-capable provider exists.
func providerWizardMethodOptions() []providerWizardMethodOption {
	options := []providerWizardMethodOption{}
	if len(providercatalog.OAuthProviders()) > 0 {
		options = append(options, providerWizardMethodOption{
			oauth:    true,
			label:    "Sign in with OAuth",
			subtitle: "One-click browser login, no API key to copy (OpenRouter, xAI, ChatGPT, Hugging Face).",
		})
	}
	options = append(options, providerWizardMethodOption{
		oauth:    false,
		label:    "Paste an API key / browse providers",
		subtitle: "Any of 20+ providers, a local model, or a subscription via proxy.",
	})
	return options
}

// providerWizardOAuthDescriptors returns the OAuth-capable providers as the
// wizard's provider list (used after the OAuth method is chosen). ChatGPT/Claude
// are deliberately not here — they can't do real in-app OAuth (see
// docs/oauth-subscriptions.md); use "browse" + a local proxy for those.
func providerWizardOAuthDescriptors() []providercatalog.Descriptor {
	return providercatalog.OAuthProviders()
}

type providerWizardModel struct {
	ID          string
	Description string
	Meta        string
}

type providerWizardState struct {
	step             providerWizardStep
	providers        []providercatalog.Descriptor
	selectedProvider int
	models           []providerWizardModel
	selectedModel    int
	providerSearch   string
	modelSearch      string
	baseURL          string
	profileName      string
	apiKey           string
	err              string
	modelSource      string
	modelLoading     bool
	modelLoadError   string
	discoveryToken   int
	selectedMethod   int
	// Manage-key step: shown when the selected provider already has a stored key.
	manageKeyCursor    int
	manageProviderName string
	// aimlapiExistingProfile is the persisted credential source selected by the
	// "Use existing configuration" preflight. aimlapiRuntimeKey is resolved only
	// for balance/top-up/model requests; it must never replace APIKeyEnv or an
	// APIKeyStored marker when the profile is saved again.
	aimlapiConfiguredCursor int
	aimlapiExistingProfile  config.ProviderProfile
	aimlapiRuntimeKey       string
	aimlapiExistingBusy     bool
	aimlapiExistingGen      int
	aimlapiExistingCancel   context.CancelFunc
	oauthMode               bool
	oauthPending            bool
	oauthAttemptID          int
	oauthErr                string
	// Device-code login (RFC 8628) state while an OAuth login is in flight.
	oauthDevice           bool
	deviceUserCode        string
	deviceVerificationURI string
	// aimlapi holds the shared aimlapi.com onboarding sub-flow while
	// the wizard is on providerWizardStepAimlapi.
	aimlapi *aimlapiOnboardState
	// Manager state (provider_manager.go): the list-first /provider surface.
	manage           bool
	manageRows       []providerManagerRow
	manageCursor     int
	manageDeleting   bool
	manageStatus     string
	manageCredGen    int
	manageActiveName string
	// Edit state: field-level editor for one saved profile.
	editOriginal config.ProviderProfile
	editDraft    config.ProviderProfile
	editCursor   int
	editField    providerEditField
	editBuffer   string
}

func (m model) newProviderWizard() *providerWizardState {
	providers := providerWizardProviders()
	wizard := &providerWizardState{
		step:             providerWizardStepMethod,
		providers:        providers,
		selectedProvider: 0,
	}
	wizard.refreshModels()
	return wizard
}

func providerWizardProviders() []providercatalog.Descriptor {
	providers := []providercatalog.Descriptor{}
	for _, descriptor := range providercatalog.All() {
		if !providercatalog.RuntimeSupported(descriptor) {
			continue
		}
		providers = append(providers, descriptor)
	}
	return providers
}

func (wizard *providerWizardState) currentProvider() providercatalog.Descriptor {
	if wizard == nil {
		return providercatalog.Descriptor{}
	}
	if wizard.step == providerWizardStepProvider {
		providers := wizard.filteredProviders()
		if len(providers) == 0 {
			return providercatalog.Descriptor{}
		}
		wizard.selectedProvider = clampInt(wizard.selectedProvider, 0, len(providers)-1)
		return providers[wizard.selectedProvider]
	}
	if len(wizard.providers) == 0 {
		return providercatalog.Descriptor{}
	}
	wizard.selectedProvider = clampInt(wizard.selectedProvider, 0, len(wizard.providers)-1)
	return wizard.providers[wizard.selectedProvider]
}

func (wizard *providerWizardState) beginOAuthAttempt(device bool) int {
	wizard.oauthAttemptID++
	wizard.oauthPending = true
	wizard.oauthDevice = device
	wizard.oauthErr = ""
	wizard.deviceUserCode = ""
	wizard.deviceVerificationURI = ""
	return wizard.oauthAttemptID
}

func (wizard *providerWizardState) oauthResultMatches(providerID string, attemptID int) bool {
	if wizard == nil || !wizard.oauthPending || strings.TrimSpace(providerID) == "" {
		return false
	}
	return wizard.currentProvider().ID == providerID && wizard.oauthAttemptID == attemptID
}

// resetAimlapiOnboard drops any in-flight aimlapi.com sub-flow (cancelling a
// running top-up stream) when the selected provider changes.
func (wizard *providerWizardState) resetAimlapiOnboard() {
	if wizard.aimlapi != nil {
		wizard.aimlapi.cancelStream()
		wizard.aimlapi = nil
	}
	wizard.clearAimlapiExisting()
}

// enterAimlapi starts the shared aimlapi.com onboarding sub-flow.
func (wizard *providerWizardState) enterAimlapi() {
	wizard.clearAimlapiExisting()
	wizard.aimlapi = newAimlapiOnboard(browser.OpenURL)
	wizard.step = providerWizardStepAimlapi
}

func (wizard *providerWizardState) clearAimlapiExisting() {
	if wizard.aimlapiExistingCancel != nil {
		wizard.aimlapiExistingCancel()
		wizard.aimlapiExistingCancel = nil
	}
	wizard.aimlapiExistingProfile = config.ProviderProfile{}
	wizard.aimlapiRuntimeKey = ""
	wizard.aimlapiExistingBusy = false
	wizard.aimlapiConfiguredCursor = 0
}

func (wizard *providerWizardState) currentModel() providerWizardModel {
	if wizard == nil {
		return providerWizardModel{}
	}
	if providerWizardUsesTypedModel(wizard.currentProvider()) {
		modelID := strings.TrimSpace(wizard.modelSearch)
		if modelID == "" {
			return providerWizardModel{Description: "model name required"}
		}
		return providerWizardModel{ID: modelID, Description: "custom model"}
	}
	wizard.refreshModels()
	models := wizard.filteredModels()
	if len(models) == 0 {
		return providerWizardModel{Description: "no matching models"}
	}
	wizard.selectedModel = clampInt(wizard.selectedModel, 0, len(models)-1)
	return models[wizard.selectedModel]
}

func (wizard *providerWizardState) move(delta int) {
	if wizard == nil {
		return
	}
	switch wizard.step {
	case providerWizardStepMethod:
		options := providerWizardMethodOptions()
		if len(options) == 0 {
			return
		}
		wizard.selectedMethod = ((wizard.selectedMethod+delta)%len(options) + len(options)) % len(options)
	case providerWizardStepProvider:
		providers := wizard.filteredProviders()
		if len(providers) == 0 {
			return
		}
		wizard.selectedProvider = ((wizard.selectedProvider+delta)%len(providers) + len(providers)) % len(providers)
		wizard.selectedModel = 0
		wizard.modelSearch = ""
		wizard.baseURL = ""
		wizard.profileName = ""
		wizard.apiKey = ""
		wizard.err = ""
		wizard.modelSource = ""
		wizard.modelLoading = false
		wizard.modelLoadError = ""
		wizard.oauthPending = false
		wizard.oauthErr = ""
		wizard.resetAimlapiOnboard()
		wizard.refreshModels()
	case providerWizardStepModel:
		wizard.refreshModels()
		models := wizard.filteredModels()
		if len(models) == 0 {
			return
		}
		wizard.selectedModel = ((wizard.selectedModel+delta)%len(models) + len(models)) % len(models)
	}
}

func (wizard *providerWizardState) advance() {
	if wizard == nil {
		return
	}
	switch wizard.step {
	case providerWizardStepMethod:
		options := providerWizardMethodOptions()
		wizard.selectedMethod = clampInt(wizard.selectedMethod, 0, maxInt(0, len(options)-1))
		wizard.err = ""
		if len(options) > 0 && options[wizard.selectedMethod].oauth {
			wizard.oauthMode = true
			wizard.providers = providerWizardOAuthDescriptors()
		} else {
			wizard.oauthMode = false
			wizard.providers = providerWizardProviders()
		}
		wizard.selectedProvider = 0
		wizard.refreshModels()
		wizard.step = providerWizardStepProvider
	case providerWizardStepProvider:
		// In OAuth mode, advancing starts the browser/device login (dispatched by
		// advanceProviderWizard at the model level), not the key/endpoint flow.
		if wizard.oauthMode {
			return
		}
		if len(wizard.filteredProviders()) == 0 {
			return
		}
		// Resolve the selected provider's index in the full list before clearing
		// the search — selectedProvider is an index into the filtered slice, and
		// clearing the search swaps to the full list.
		if selected := wizard.currentProvider(); selected.ID != "" {
			for i, p := range wizard.providers {
				if p.ID == selected.ID {
					wizard.selectedProvider = i
					break
				}
			}
		}
		wizard.providerSearch = ""
		wizard.refreshModels()
		wizard.err = ""
		if providerWizardNeedsEndpoint(wizard.currentProvider()) {
			wizard.step = providerWizardStepEndpoint
		} else if providerWizardIsAimlapi(wizard.currentProvider()) {
			wizard.enterAimlapi()
		} else if providerWizardNeedsCredential(wizard.currentProvider()) {
			wizard.step = providerWizardStepCredential
		} else {
			wizard.step = providerWizardStepModel
		}
	case providerWizardStepEndpoint:
		wizard.err = ""
		if err := providerWizardEndpointError(wizard.baseURL); err != "" {
			wizard.err = err
			return
		}
		wizard.step = providerWizardStepName
	case providerWizardStepName:
		wizard.err = ""
		if providerWizardIsAimlapi(wizard.currentProvider()) {
			wizard.enterAimlapi()
		} else if providerWizardNeedsCredential(wizard.currentProvider()) {
			wizard.step = providerWizardStepCredential
		} else {
			wizard.step = providerWizardStepModel
		}
	case providerWizardStepCredential:
		wizard.err = ""
		wizard.step = providerWizardStepModel
	case providerWizardStepModel:
		wizard.err = ""
		if providerWizardUsesTypedModel(wizard.currentProvider()) {
			if strings.TrimSpace(wizard.modelSearch) == "" {
				wizard.err = "enter a model name before continuing"
				return
			}
			wizard.step = providerWizardStepDone
			return
		}
		if wizard.modelLoading {
			wizard.err = "Models are still loading."
			return
		}
		wizard.refreshModels()
		if len(wizard.filteredModels()) == 0 {
			wizard.err = "choose a matching model before continuing"
			return
		}
		wizard.step = providerWizardStepDone
	case providerWizardStepDone:
		wizard.step = providerWizardStepProvider
	}
}

func (wizard *providerWizardState) retreat() {
	if wizard == nil {
		return
	}
	wizard.err = ""
	switch wizard.step {
	case providerWizardStepProvider:
		wizard.oauthMode = false
		wizard.oauthErr = ""
		wizard.step = providerWizardStepMethod
	case providerWizardStepEndpoint:
		wizard.step = providerWizardStepProvider
		wizard.providerSearch = ""
	case providerWizardStepName:
		wizard.step = providerWizardStepEndpoint
	case providerWizardStepAimlapi:
		wizard.resetAimlapiOnboard()
		if providerWizardNeedsProfileName(wizard.currentProvider()) {
			wizard.step = providerWizardStepName
		} else if providerWizardNeedsEndpoint(wizard.currentProvider()) {
			wizard.step = providerWizardStepEndpoint
		} else {
			wizard.step = providerWizardStepProvider
		}
	case providerWizardStepCredential:
		if providerWizardNeedsProfileName(wizard.currentProvider()) {
			wizard.step = providerWizardStepName
		} else if providerWizardNeedsEndpoint(wizard.currentProvider()) {
			wizard.step = providerWizardStepEndpoint
		} else {
			wizard.step = providerWizardStepProvider
		}
	case providerWizardStepModel:
		if providerWizardIsAimlapi(wizard.currentProvider()) {
			if aimlapiProfile(wizard.aimlapiExistingProfile) {
				wizard.step = providerWizardStepAimlapiConfigured
			} else {
				wizard.step = providerWizardStepProvider
			}
		} else if providerWizardNeedsCredential(wizard.currentProvider()) {
			wizard.step = providerWizardStepCredential
		} else if providerWizardNeedsEndpoint(wizard.currentProvider()) {
			wizard.step = providerWizardStepEndpoint
		} else {
			wizard.step = providerWizardStepProvider
		}
	case providerWizardStepDone:
		wizard.step = providerWizardStepModel
	}
}

func (wizard *providerWizardState) refreshModels() {
	if wizard == nil {
		return
	}
	provider := wizard.currentProvider()
	if providerWizardUsesTypedModel(provider) {
		return
	}
	if wizard.modelSource != "" && wizard.modelSource != "fallback" {
		wizard.selectedModel = clampInt(wizard.selectedModel, 0, maxInt(0, len(wizard.models)-1))
		return
	}
	models := providerWizardModelOptions(provider)
	if sameProviderWizardModels(wizard.models, models) {
		wizard.selectedModel = clampInt(wizard.selectedModel, 0, maxInt(0, len(models)-1))
		return
	}
	wizard.models = models
	wizard.selectedModel = 0
	wizard.modelSource = "fallback"
}

func sameProviderWizardModels(a, b []providerWizardModel) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index].ID != b[index].ID {
			return false
		}
	}
	return true
}

func providerWizardNeedsCredential(provider providercatalog.Descriptor) bool {
	return provider.RequiresAuth && !provider.Local && len(provider.AuthEnvVars) > 0
}

func providerWizardIsAimlapi(provider providercatalog.Descriptor) bool {
	return strings.EqualFold(strings.TrimSpace(provider.ID), "aimlapi")
}

func providerWizardNeedsEndpoint(provider providercatalog.Descriptor) bool {
	switch provider.ID {
	case "custom-openai-compatible", "custom-anthropic-compatible":
		return true
	default:
		return false
	}
}

func providerWizardUsesTypedModel(provider providercatalog.Descriptor) bool {
	return providerWizardNeedsEndpoint(provider)
}

func providerWizardNeedsProfileName(provider providercatalog.Descriptor) bool {
	return providerWizardNeedsEndpoint(provider)
}

func (m model) handleProviderWizardKey(msg tea.KeyMsg) (model, tea.Cmd) {
	if m.providerWizard == nil {
		return m, nil
	}
	// A manager-launched add flow honors the manager's "Esc walks back one
	// level" contract on EVERY step (not just the first): Esc returns to the
	// provider list instead of destroying the overlay — and with it the user's
	// cursor and status — from a step deep in the flow. An in-flight OAuth login
	// is abandoned the same way; bumping the attempt id makes its late result
	// stale so applyProviderWizardOAuth drops it.
	if m.providerWizard.manage && !m.providerWizard.managerStep() && keyIs(msg, tea.KeyEsc) {
		wizard := m.providerWizard
		if wizard.oauthPending {
			wizard.oauthPending = false
			wizard.oauthAttemptID++
		}
		wizard.oauthDevice = false
		wizard.deviceUserCode = ""
		wizard.deviceVerificationURI = ""
		// This branch preempts the aimlapi.com key delegation below, so cancel the
		// embedded sub-flow here too — otherwise its in-flight CreateKey/checkout
		// keeps running against a background context and mints a key we discard.
		wizard.resetAimlapiOnboard()
		wizard.err = ""
		wizard.oauthErr = ""
		wizard.step = providerWizardStepManage
		return m, nil
	}
	// While a browser/device OAuth login is in flight, ignore input except Esc,
	// which abandons the wizard (the background flow times out and is dropped).
	if m.providerWizard.oauthPending {
		if keyIs(msg, tea.KeyEsc) {
			m.providerWizard = nil
		}
		return m, nil
	}
	// Manager steps (list / edit) own their keys entirely.
	if m.providerWizard.managerStep() {
		return m.handleProviderManagerKey(msg)
	}
	// Manage-key step: keep/replace/remove a provider's stored key.
	if m.providerWizard.step == providerWizardStepManageKey {
		switch {
		case keyIs(msg, tea.KeyEsc) || keyIs(msg, tea.KeyLeft):
			m.providerWizard.step = providerWizardStepProvider
			m.providerWizard.err = ""
			return m, nil
		case keyIs(msg, tea.KeyUp):
			m.providerWizard.manageKeyCursor = (m.providerWizard.manageKeyCursor + 2) % 3
			return m, nil
		case keyIs(msg, tea.KeyDown) || keyIs(msg, tea.KeyTab):
			m.providerWizard.manageKeyCursor = (m.providerWizard.manageKeyCursor + 1) % 3
			return m, nil
		case keyIs(msg, tea.KeyEnter):
			return m.applyManageKeyChoice()
		}
		return m, nil
	}
	// On the OAuth provider list, "d" forces device-code login for a device-capable
	// provider (xAI) — useful on a desktop when the browser flow won't work.
	if m.providerWizard.step == providerWizardStepProvider && m.providerWizard.oauthMode &&
		(keyText(msg) == "d" || keyText(msg) == "D") &&
		m.providerWizard.currentProvider().OAuthDeviceFlow {
		return m.startProviderDeviceLogin()
	}
	if m.providerWizard.step == providerWizardStepProvider {
		switch {
		case keyText(msg) != "":
			m.providerWizard.appendProviderSearch(keyRunes(msg))
			return m, nil
		case keyBackspace(msg):
			m.providerWizard.deleteProviderSearchRune()
			return m, nil
		case keyCtrl(msg, 'u'):
			m.providerWizard.providerSearch = ""
			m.providerWizard.selectedProvider = 0
			return m, nil
		}
	}
	if m.providerWizard.step == providerWizardStepEndpoint {
		switch {
		case keyText(msg) != "":
			m.providerWizard.appendBaseURL(keyRunes(msg))
			return m, nil
		case keyBackspace(msg):
			m.providerWizard.deleteBaseURLRune()
			return m, nil
		case keyCtrl(msg, 'u'):
			m.providerWizard.baseURL = ""
			m.providerWizard.err = ""
			return m, nil
		case keyIs(msg, tea.KeyLeft):
			m.providerWizard.retreat()
			return m, nil
		case keyIs(msg, tea.KeyRight):
			if m.providerWizard.canAdvanceWithRight() {
				return m.advanceProviderWizard()
			}
			return m, nil
		case keyIs(msg, tea.KeyEnter):
			return m.advanceProviderWizard()
		}
	}
	if m.providerWizard.step == providerWizardStepName {
		switch {
		case keyText(msg) != "":
			m.providerWizard.appendProfileName(keyRunes(msg))
			return m, nil
		case keyBackspace(msg):
			m.providerWizard.deleteProfileNameRune()
			return m, nil
		case keyCtrl(msg, 'u'):
			m.providerWizard.profileName = ""
			m.providerWizard.err = ""
			return m, nil
		case keyIs(msg, tea.KeyLeft):
			m.providerWizard.retreat()
			return m, nil
		case keyIs(msg, tea.KeyRight) || keyIs(msg, tea.KeyEnter):
			return m.advanceProviderWizard()
		}
	}
	if m.providerWizard.step == providerWizardStepAimlapi {
		return m.handleProviderWizardAimlapiKey(msg)
	}
	if m.providerWizard.step == providerWizardStepAimlapiConfigured {
		if m.providerWizard.aimlapiExistingBusy {
			if keyIs(msg, tea.KeyEsc) || keyIs(msg, tea.KeyLeft) {
				m.providerWizard.aimlapiExistingGen++
				m.providerWizard.clearAimlapiExisting()
				m.providerWizard.step = providerWizardStepProvider
			}
			return m, nil
		}
		switch {
		case keyIs(msg, tea.KeyEsc):
			m.providerWizard = nil
		case keyIs(msg, tea.KeyLeft):
			m.providerWizard.clearAimlapiExisting()
			m.providerWizard.step = providerWizardStepProvider
		case keyIs(msg, tea.KeyUp) || keyIs(msg, tea.KeyDown) || keyIs(msg, tea.KeyTab):
			m.providerWizard.aimlapiConfiguredCursor = (m.providerWizard.aimlapiConfiguredCursor + 1) % 2
		case keyIs(msg, tea.KeyEnter) || keyIs(msg, tea.KeyRight):
			if m.providerWizard.aimlapiConfiguredCursor == 0 {
				return m.checkExistingAimlapiBalance()
			}
			m.providerWizard.enterAimlapi()
		}
		return m, nil
	}
	if m.providerWizard.step == providerWizardStepCredential {
		switch {
		case keyIs(msg, tea.KeyEsc):
			m.providerWizard = nil
			return m, nil
		case keyCtrl(msg, 'o'):
			if providerWizardSupportsOAuth(m.providerWizard.currentProvider()) {
				provider := m.providerWizard.currentProvider()
				attemptID := m.providerWizard.beginOAuthAttempt(false)
				return m, providerWizardOAuthCmdFor(provider, attemptID)
			}
			return m, nil
		case keyText(msg) != "":
			m.providerWizard.appendAPIKey(keyRunes(msg))
			return m, nil
		case keyBackspace(msg):
			m.providerWizard.deleteAPIKeyRune()
			return m, nil
		case keyCtrl(msg, 'u'):
			m.providerWizard.apiKey = ""
			return m, nil
		case keyIs(msg, tea.KeyLeft):
			m.providerWizard.retreat()
			return m, nil
		case keyIs(msg, tea.KeyRight):
			if m.providerWizard.canAdvanceWithRight() {
				return m.advanceProviderWizard()
			}
			return m, nil
		case keyIs(msg, tea.KeyEnter):
			return m.advanceProviderWizard()
		}
		return m, nil
	}
	if m.providerWizard.step == providerWizardStepModel {
		switch {
		case keyText(msg) != "":
			m.providerWizard.appendModelSearch(keyRunes(msg))
			return m, nil
		case keyBackspace(msg):
			m.providerWizard.deleteModelSearchRune()
			return m, nil
		case keyCtrl(msg, 'u'):
			m.providerWizard.modelSearch = ""
			m.providerWizard.selectedModel = 0
			return m, nil
		}
	}
	switch {
	case keyIs(msg, tea.KeyEsc):
		m.providerWizard = nil
	case keyIs(msg, tea.KeyUp):
		m.providerWizard.move(-1)
	case keyIs(msg, tea.KeyDown) || keyIs(msg, tea.KeyTab):
		m.providerWizard.move(1)
	case keyIs(msg, tea.KeyLeft):
		m.providerWizard.retreat()
	case keyIs(msg, tea.KeyRight):
		if m.providerWizard.canAdvanceWithRight() {
			return m.advanceProviderWizard()
		}
	case keyIs(msg, tea.KeyEnter):
		if m.providerWizard.step == providerWizardStepDone {
			return m.applyProviderWizard()
		}
		return m.advanceProviderWizard()
	}
	return m, nil
}

func (m model) handleProviderWizardPaste(content string) (model, tea.Cmd) {
	if m.providerWizard == nil || m.providerWizard.oauthPending {
		return m, nil
	}
	switch m.providerWizard.step {
	case providerWizardStepProvider:
		m.providerWizard.appendProviderSearch([]rune(content))
	case providerWizardStepEndpoint:
		m.providerWizard.appendBaseURL([]rune(content))
	case providerWizardStepName:
		m.providerWizard.appendProfileName([]rune(content))
	case providerWizardStepAimlapi:
		if m.providerWizard.aimlapi != nil {
			m.providerWizard.aimlapi.appendInput(content)
		}
	case providerWizardStepCredential:
		m.providerWizard.appendAPIKey([]rune(content))
	case providerWizardStepModel:
		m.providerWizard.appendModelSearch([]rune(content))
	case providerWizardStepEditValue:
		for _, r := range content {
			if r == '\n' || r == '\r' || r == '\t' {
				continue
			}
			m.providerWizard.editBuffer += string(r)
		}
	}
	return m, nil
}

func (wizard *providerWizardState) canAdvanceWithRight() bool {
	if wizard == nil {
		return false
	}
	switch wizard.step {
	case providerWizardStepMethod:
		return len(providerWizardMethodOptions()) > 0
	case providerWizardStepProvider:
		return strings.TrimSpace(wizard.currentProvider().ID) != ""
	case providerWizardStepEndpoint:
		return providerWizardEndpointError(wizard.baseURL) == ""
	case providerWizardStepName:
		return true
	case providerWizardStepCredential:
		return wizard.credentialReadyForRight()
	case providerWizardStepModel:
		if providerWizardUsesTypedModel(wizard.currentProvider()) {
			return strings.TrimSpace(wizard.modelSearch) != ""
		}
		if wizard.modelLoading {
			return false
		}
		wizard.refreshModels()
		return len(wizard.filteredModels()) > 0
	default:
		return false
	}
}

func (wizard *providerWizardState) credentialReadyForRight() bool {
	if strings.TrimSpace(wizard.apiKey) != "" {
		return true
	}
	provider := wizard.currentProvider()
	if !providerWizardNeedsCredential(provider) {
		return true
	}
	for _, env := range provider.AuthEnvVars {
		if strings.TrimSpace(os.Getenv(strings.TrimSpace(env))) != "" {
			return true
		}
	}
	return false
}

func (wizard *providerWizardState) appendAPIKey(runes []rune) {
	for _, r := range runes {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			continue
		}
		wizard.apiKey += string(r)
	}
	wizard.err = ""
}

func (wizard *providerWizardState) deleteAPIKeyRune() {
	if wizard.apiKey == "" {
		return
	}
	runes := []rune(wizard.apiKey)
	wizard.apiKey = string(runes[:len(runes)-1])
	wizard.err = ""
}

func (wizard *providerWizardState) appendBaseURL(runes []rune) {
	for _, r := range runes {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			continue
		}
		wizard.baseURL += string(r)
	}
	wizard.err = ""
}

func (wizard *providerWizardState) deleteBaseURLRune() {
	if wizard.baseURL == "" {
		return
	}
	runes := []rune(wizard.baseURL)
	wizard.baseURL = string(runes[:len(runes)-1])
	wizard.err = ""
}

func (wizard *providerWizardState) appendProfileName(runes []rune) {
	for _, r := range runes {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			continue
		}
		wizard.profileName += string(r)
	}
	wizard.err = ""
}

func (wizard *providerWizardState) deleteProfileNameRune() {
	if wizard.profileName == "" {
		return
	}
	runes := []rune(wizard.profileName)
	wizard.profileName = string(runes[:len(runes)-1])
	wizard.err = ""
}

func (wizard *providerWizardState) appendModelSearch(runes []rune) {
	for _, r := range runes {
		if unicode.IsControl(r) {
			continue
		}
		wizard.modelSearch += string(r)
	}
	wizard.selectedModel = 0
}

func (wizard *providerWizardState) deleteModelSearchRune() {
	if wizard.modelSearch == "" {
		return
	}
	runes := []rune(wizard.modelSearch)
	wizard.modelSearch = string(runes[:len(runes)-1])
	wizard.selectedModel = 0
}

func (wizard *providerWizardState) appendProviderSearch(runes []rune) {
	for _, r := range runes {
		if unicode.IsControl(r) {
			continue
		}
		wizard.providerSearch += string(r)
	}
	wizard.selectedProvider = 0
}

func (wizard *providerWizardState) deleteProviderSearchRune() {
	if wizard.providerSearch == "" {
		return
	}
	runes := []rune(wizard.providerSearch)
	wizard.providerSearch = string(runes[:len(runes)-1])
	wizard.selectedProvider = 0
}

func (wizard *providerWizardState) filteredProviders() []providercatalog.Descriptor {
	if wizard == nil {
		return nil
	}
	query := strings.ToLower(strings.TrimSpace(wizard.providerSearch))
	if query == "" {
		return append([]providercatalog.Descriptor{}, wizard.providers...)
	}
	providers := make([]providercatalog.Descriptor, 0, len(wizard.providers))
	for _, provider := range wizard.providers {
		if providerMatchesQuery(provider, query) {
			providers = append(providers, provider)
		}
	}
	return providers
}

func providerMatchesQuery(provider providercatalog.Descriptor, query string) bool {
	if query == "" {
		return true
	}
	haystack := strings.ToLower(strings.Join([]string{provider.ID, provider.Name, strings.Join(provider.Aliases, " ")}, " "))
	return strings.Contains(haystack, query)
}

func (m model) applyProviderWizard() (model, tea.Cmd) {
	wizard := m.providerWizard
	if wizard == nil {
		return m, nil
	}
	provider := wizard.currentProvider()
	modelChoice := wizard.currentModel()
	profile := providerWizardProfile(provider, modelChoice.ID, wizard.apiKey, wizard.baseURL, wizard.profileName)
	runtimeProfile := providerWizardRuntimeProfile(profile)
	usingExistingAimlapi := aimlapiProfile(wizard.aimlapiExistingProfile)
	preserveExistingCredentialReference := false
	if usingExistingAimlapi {
		// Preserve the configured credential source exactly. The resolved key is
		// runtime-only and must not turn APIKeyEnv or APIKeyStored into a captured
		// secret merely because the user changed models or topped up.
		profile = wizard.aimlapiExistingProfile
		profile.Model = modelChoice.ID
		runtimeProfile = profile
		runtimeProfile.APIKey = wizard.aimlapiRuntimeKey
		preserveExistingCredentialReference = strings.TrimSpace(profile.APIKeyEnv) != "" || profile.APIKeyStored
	}

	// This wizard saves directly, bypassing the CLI add/setup validation.
	// Refuse Atomic Chat's fallback before constructing or persisting a provider.
	if isAtomicLocalPlaceholderModel(provider.ID, profile.Model, provider.DefaultModel) {
		wizard.err = "Atomic Chat serves a locally loaded model. Load a model in Atomic Chat, then pick it here; the 'local-model' placeholder will not work."
		return m, nil
	}

	// Build and persist into LOCALS first, committing live state only once BOTH
	// succeed. A persist failure (read-only config, disk full) must not leave the
	// chat running on the new provider while the status line, m.providerProfile,
	// and the ZERO_PROVIDER export (which pins spawned children) still point at
	// the old one — parent and children would silently run on different providers.
	var nextProvider zeroruntime.Provider
	if m.newProvider != nil {
		built, err := m.newProvider(runtimeProfile)
		if err != nil {
			wizard.err = redaction.RedactString(err.Error(), redaction.Options{ExtraSecretValues: []string{profile.APIKey, runtimeProfile.APIKey}})
			return m, nil
		}
		nextProvider = built
	}
	if strings.TrimSpace(m.userConfigPath) != "" {
		// Capture flip: move the freshly entered key into the encrypted credential
		// store before persisting, so config.json never holds the cleartext. The
		// provider was already built above from runtimeProfile, which has the key.
		secret := profile.APIKey
		if !preserveExistingCredentialReference {
			profile = config.SecureProviderProfile(profile, m.userConfigPath)
		}
		if _, err := config.UpsertProvider(m.userConfigPath, profile, true); err != nil {
			wizard.err = redaction.RedactString(err.Error(), redaction.Options{ExtraSecretValues: []string{secret, profile.APIKey}})
			return m, nil // nothing committed to live state yet
		}
	}

	// Both succeeded — commit the live provider, profile, model, and the child
	// env export together, so they can never disagree.
	if nextProvider != nil {
		m.provider = nextProvider
	}
	m.providerProfile = profile
	m.providerName = profile.Name
	m.modelName = profile.Model
	// Keep the in-memory saved list in sync so the provider manager and /model
	// picker show the new profile without a restart.
	m.savedProviders = upsertSavedProviderProfile(m.savedProviders, profile)
	// Keep sub-agent child processes on the same provider we just switched to —
	// same as the /model and /provider switch paths (command_center.go). Without
	// this, a ZERO_PROVIDER exported by an earlier switch stays pointing at the
	// OLD provider and wins over config in every spawned child (applyEnv), so
	// specialists/swarm members run on the wrong provider's credentials.
	config.SetActiveProviderEnv(profile.Name)
	m.providerWizard = nil
	return m, nil
}

// wizardProviderStoredKey reports the saved provider name that has a key in the
// credential store matching the wizard-selected descriptor, so the wizard can offer
// keep/replace/remove instead of forcing a new key entry.
func (m model) wizardProviderStoredKey(provider providercatalog.Descriptor) (string, bool) {
	for _, profile := range m.savedProviders {
		if !profile.APIKeyStored {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(profile.Name), strings.TrimSpace(provider.Name)) ||
			strings.EqualFold(strings.TrimSpace(profile.CatalogID), strings.TrimSpace(provider.ID)) ||
			strings.EqualFold(strings.TrimSpace(profile.Name), strings.TrimSpace(provider.ID)) {
			return profile.Name, true
		}
	}
	return "", false
}

// applyManageKeyChoice acts on the keep/replace/remove selection. Keep closes the
// wizard (nothing changes); Replace routes to credential entry (overwrites on save);
// Remove deletes the stored key and its marker.
func (m model) applyManageKeyChoice() (model, tea.Cmd) {
	wizard := m.providerWizard
	if wizard == nil {
		return m, nil
	}
	name := strings.TrimSpace(wizard.manageProviderName)
	switch wizard.manageKeyCursor {
	case 1: // Replace
		wizard.apiKey = ""
		wizard.err = ""
		wizard.step = providerWizardStepCredential
		return m, nil
	case 2: // Remove
		if strings.TrimSpace(m.userConfigPath) != "" {
			if store, err := config.ProviderKeyStoreAt(filepath.Dir(m.userConfigPath)); err == nil {
				_, _ = store.Delete(name)
			}
			_, _ = config.ClearProviderKeyStored(m.userConfigPath, name)
		} else {
			_, _ = config.ForgetProviderKey(name)
		}
		m.providerWizard = nil
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Provider\nRemoved the stored key for " + name + ". Re-add it any time with /provider."})
		return m, nil
	default: // Keep
		m.providerWizard = nil
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Provider\nKept the saved key for " + name + "."})
		return m, nil
	}
}

func providerWizardRuntimeProfile(profile config.ProviderProfile) config.ProviderProfile {
	runtimeProfile := profile
	if strings.TrimSpace(runtimeProfile.APIKey) == "" && strings.TrimSpace(runtimeProfile.APIKeyEnv) != "" {
		runtimeProfile.APIKey = strings.TrimSpace(os.Getenv(runtimeProfile.APIKeyEnv))
	}
	return runtimeProfile
}

func (m model) providerWizardOverlay(width int) string {
	if m.providerWizard == nil {
		return ""
	}
	return m.providerWizard.render(width, m.spinnerGlyph())
}

// render's spinner glyph is used only by the embedded aimlapi.com onboarding
// sub-view; it is the shared global run spinner (spinnerGlyph()), same as everywhere.
func (wizard *providerWizardState) render(width int, spinner string) string {
	if wizard == nil {
		return ""
	}
	overlayWidth := providerWizardOverlayWidth(width, wizard.step)
	innerWidth := maxInt(20, overlayWidth-4)

	lines := []string{
		zeroTheme.faint.Render(providerWizardStepLine(wizard)),
		zeroTheme.line.Render(strings.Repeat("─", innerWidth)),
	}
	if wizard.err != "" {
		lines = append(lines, zeroTheme.red.Render("error: "+wizard.err), "")
	}
	if wizard.oauthPending {
		lines = append(lines, wizard.renderOAuthWaiting(innerWidth)...)
		lines = append(lines,
			zeroTheme.line.Render(strings.Repeat("─", innerWidth)),
			zeroTheme.faint.Render("Esc cancel"),
		)
		block := styledBlockFillTitle(overlayWidth, "Provider setup", lines, zeroTheme.lineStrong, lipgloss.NewStyle())
		if width > overlayWidth {
			return indentBlock(block, (width-overlayWidth)/2)
		}
		return block
	}
	switch wizard.step {
	case providerWizardStepMethod:
		lines = append(lines, wizard.renderMethodStep(innerWidth)...)
	case providerWizardStepProvider:
		lines = append(lines, wizard.renderProviderStep(innerWidth)...)
	case providerWizardStepManageKey:
		lines = append(lines, wizard.renderManageKeyStep(innerWidth)...)
	case providerWizardStepEndpoint:
		lines = append(lines, wizard.renderEndpointStep(innerWidth)...)
	case providerWizardStepName:
		lines = append(lines, wizard.renderNameStep(innerWidth)...)
	case providerWizardStepAimlapiConfigured:
		lines = append(lines, wizard.renderAimlapiConfiguredStep(innerWidth, spinner)...)
	case providerWizardStepAimlapi:
		if wizard.aimlapi != nil {
			lines = append(lines, wizard.aimlapi.view(innerWidth, spinner)...)
		}
	case providerWizardStepCredential:
		lines = append(lines, wizard.renderCredentialStep(innerWidth)...)
	case providerWizardStepModel:
		lines = append(lines, wizard.renderModelStep(innerWidth)...)
	case providerWizardStepDone:
		lines = append(lines, wizard.renderDoneStep(innerWidth)...)
	case providerWizardStepManage:
		lines = append(lines, wizard.renderManageStep(innerWidth)...)
	case providerWizardStepEditMenu:
		lines = append(lines, wizard.renderEditMenuStep(innerWidth)...)
	case providerWizardStepEditValue:
		lines = append(lines, wizard.renderEditValueStep(innerWidth)...)
	}
	lines = append(lines,
		zeroTheme.line.Render(strings.Repeat("─", innerWidth)),
		zeroTheme.faint.Render(wizard.footer()),
	)

	title := "Provider setup"
	if wizard.managerStep() {
		title = "Providers"
	}
	block := styledBlockFillTitle(overlayWidth, title, lines, zeroTheme.lineStrong, lipgloss.NewStyle())
	if width > overlayWidth {
		return indentBlock(block, (width-overlayWidth)/2)
	}
	return block
}

func (wizard *providerWizardState) footer() string {
	text := wizard.footerText()
	// Inside a manager-launched add flow, Esc walks back to the provider list
	// (see handleProviderWizardKey) — the hint must match.
	if wizard.manage && !wizard.managerStep() {
		text = strings.ReplaceAll(text, "Esc close", "Esc back")
	}
	return text
}

func (wizard *providerWizardState) footerText() string {
	canRight := wizard.canAdvanceWithRight()
	switch wizard.step {
	case providerWizardStepManage:
		if wizard.manageDeleting {
			return "Enter/y delete   Esc/n cancel"
		}
		if len(wizard.manageRows) == 0 {
			return "a add   Esc close"
		}
		return "↑/↓ move   Enter activate   a add   e edit   d delete   Esc close"
	case providerWizardStepEditMenu:
		return "↑/↓ move   Enter edit field / save   Esc back"
	case providerWizardStepEditValue:
		return "type to edit   Enter apply   Ctrl+U clear   Esc back"
	case providerWizardStepMethod:
		return "↑/↓ move   Enter/→ continue   Esc close"
	case providerWizardStepProvider:
		if wizard.oauthMode && wizard.currentProvider().OAuthDeviceFlow {
			return "↑/↓ move   Enter sign in   d device code   ← back   Esc close"
		}
		if canRight {
			return "↑/↓ move   Enter/→ continue   ← back   Esc close"
		}
		return "↑/↓ move   Enter continue   ← back   Esc close"
	case providerWizardStepManageKey:
		return "↑/↓ move   Enter select   Esc back"
	case providerWizardStepEndpoint:
		if canRight {
			return "Enter/→ continue   ← back   Esc close"
		}
		return "Enter continue   ← back   Esc close"
	case providerWizardStepName:
		return "Enter/→ continue   ← back   Esc close"
	case providerWizardStepAimlapiConfigured:
		return "↑/↓ move   Enter/→ select   ← back   Esc close"
	case providerWizardStepAimlapi:
		return "type/↑↓ choose   Enter/→ continue   ← back   Esc close"
	case providerWizardStepModel:
		if canRight {
			return "↑/↓ move   Enter/→ continue   ← back   Esc close"
		}
		return "↑/↓ move   Enter continue   ← back   Esc close"
	case providerWizardStepDone:
		return "Enter save   ← back   Esc close"
	default:
		if canRight {
			return "Enter/→ continue   ← back   Esc close"
		}
		return "Enter continue   ← back   Esc close"
	}
}

func providerWizardOverlayWidth(width int, step providerWizardStep) int {
	if width <= 0 {
		return providerWizardProviderWidth
	}
	target := providerWizardMediumWidth
	switch step {
	case providerWizardStepProvider:
		target = providerWizardProviderWidth
	case providerWizardStepModel, providerWizardStepManage, providerWizardStepAimlapi, providerWizardStepAimlapiConfigured:
		target = providerWizardModelWidth
	}
	target = minInt(target, width)
	if target < providerWizardMinWidth {
		return width
	}
	return target
}

func providerWizardStepLine(wizard *providerWizardState) string {
	if wizard == nil {
		return ""
	}
	if wizard.managerStep() {
		switch wizard.step {
		case providerWizardStepManage:
			return pluralCount(len(wizard.manageRows), "saved provider")
		default:
			return "edit provider"
		}
	}
	step := wizard.step
	type stepLabel struct {
		step  providerWizardStep
		label string
	}
	steps := []stepLabel{
		{providerWizardStepMethod, "1 method"},
		{providerWizardStepProvider, "2 provider"},
	}
	switch {
	case wizard.oauthMode:
		// OAuth path skips endpoint/name/key entirely.
		steps = append(steps,
			stepLabel{providerWizardStepModel, "3 model"},
			stepLabel{providerWizardStepDone, "4 ready"},
		)
	case providerWizardIsAimlapi(wizard.currentProvider()):
		connectStep := providerWizardStepAimlapi
		if aimlapiProfile(wizard.aimlapiExistingProfile) {
			connectStep = providerWizardStepAimlapiConfigured
		}
		steps = append(steps,
			stepLabel{connectStep, "3 connect"},
			stepLabel{providerWizardStepModel, "4 model"},
			stepLabel{providerWizardStepDone, "5 ready"},
		)
	case providerWizardNeedsEndpoint(wizard.currentProvider()):
		steps = append(steps,
			stepLabel{providerWizardStepEndpoint, "3 endpoint"},
			stepLabel{providerWizardStepName, "4 name"},
			stepLabel{providerWizardStepCredential, "5 key"},
			stepLabel{providerWizardStepModel, "6 model"},
			stepLabel{providerWizardStepDone, "7 ready"},
		)
	default:
		steps = append(steps,
			stepLabel{providerWizardStepCredential, "3 key"},
			stepLabel{providerWizardStepModel, "4 model"},
			stepLabel{providerWizardStepDone, "5 ready"},
		)
	}
	parts := make([]string, 0, len(steps))
	for _, item := range steps {
		if item.step == step {
			parts = append(parts, "["+item.label+"]")
		} else {
			parts = append(parts, item.label)
		}
	}
	return strings.Join(parts, "  ")
}

// renderMethodStep renders the "How do you want to connect?" chooser.
func (wizard *providerWizardState) renderMethodStep(width int) []string {
	options := providerWizardMethodOptions()
	wizard.selectedMethod = clampInt(wizard.selectedMethod, 0, maxInt(0, len(options)-1))
	lines := []string{zeroTheme.accent.Render("How do you want to connect?")}
	for index, option := range options {
		surface := transparentSurface
		marker := surface(zeroTheme.faintest).Render("  ")
		if index == wizard.selectedMethod {
			surface = zeroTheme.onSel
			marker = surface(zeroTheme.accent).Render("❯ ")
		}
		lines = append(lines, fitStyledLine(marker+surface(zeroTheme.ink).Render(option.label), width))
		lines = append(lines, fitStyledLine("    "+zeroTheme.faint.Render(option.subtitle), width))
	}
	return lines
}

func (wizard *providerWizardState) renderAimlapiConfiguredStep(width int, spinner string) []string {
	lines := []string{zeroTheme.accent.Render("aimlapi.com is already configured")}
	if wizard.aimlapiExistingBusy {
		glyph := strings.TrimSpace(spinner)
		if glyph == "" {
			glyph = "•"
		}
		return append(lines, "", zeroTheme.faint.Render(glyph+" checking balance"))
	}
	options := []struct{ label, hint string }{
		{"Use existing configuration", "Continue with your saved API key"},
		{"Configure again", "Set up a new key or switch accounts"},
	}
	for index, option := range options {
		marker := "  "
		style := zeroTheme.ink
		if index == wizard.aimlapiConfiguredCursor {
			marker = "❯ "
			style = zeroTheme.accent.Bold(true)
		}
		lines = append(lines, marker+style.Render(option.label))
		lines = append(lines, "    "+zeroTheme.faint.Render(option.hint))
	}
	return lines
}

// renderManageKeyStep shows keep/replace/remove for a provider that already has a
// key in the encrypted credential store.
func (wizard *providerWizardState) renderManageKeyStep(width int) []string {
	name := strings.TrimSpace(wizard.manageProviderName)
	if name == "" {
		name = wizard.currentProvider().Name
	}
	options := []struct{ label, subtitle string }{
		{"Keep current key", "A key is already saved (encrypted) for " + name + "."},
		{"Replace key", "Enter a new key; it overwrites the stored one."},
		{"Remove key", "Delete the stored key for " + name + "."},
	}
	wizard.manageKeyCursor = clampInt(wizard.manageKeyCursor, 0, len(options)-1)
	lines := []string{zeroTheme.accent.Render(name + " already has a saved key")}
	for index, option := range options {
		surface := transparentSurface
		marker := surface(zeroTheme.faintest).Render("  ")
		if index == wizard.manageKeyCursor {
			surface = zeroTheme.onSel
			marker = surface(zeroTheme.accent).Render("❯ ")
		}
		lines = append(lines, fitStyledLine(marker+surface(zeroTheme.ink).Render(option.label), width))
		lines = append(lines, fitStyledLine("    "+zeroTheme.faint.Render(option.subtitle), width))
	}
	return lines
}

// renderOAuthWaiting renders the in-flight browser/device login screen.
func (wizard *providerWizardState) renderOAuthWaiting(width int) []string {
	provider := wizard.currentProvider()
	name := provider.Name
	if name == "" {
		name = "the provider"
	}
	if wizard.oauthDevice {
		lines := []string{
			zeroTheme.accent.Render("Device-code sign-in for " + name),
			"",
		}
		if wizard.deviceUserCode == "" {
			return append(lines, fitStyledLine(zeroTheme.faint.Render("Requesting a device code..."), width))
		}
		return append(lines,
			fitStyledLine(zeroTheme.ink.Render("1. On any device, visit:  ")+zeroTheme.accent.Render(wizard.deviceVerificationURI), width),
			fitStyledLine(zeroTheme.ink.Render("2. Enter the code:  ")+zeroTheme.accent.Bold(true).Render(wizard.deviceUserCode), width),
			"",
			fitStyledLine(zeroTheme.faint.Render("Waiting for authorization..."), width),
		)
	}
	return []string{
		zeroTheme.accent.Render("Signing in with " + name),
		"",
		fitStyledLine(zeroTheme.ink.Render("Opening your browser — approve there, then return here."), width),
		fitStyledLine(zeroTheme.faint.Render("Waiting for authorization..."), width),
		"",
		fitStyledLine(zeroTheme.faint.Render("If your browser didn't open, run:  "+providerWizardOAuthCLIHint(provider)), width),
	}
}

// providerWizardOAuthCLIHint gives the equivalent CLI command for a provider's
// OAuth login (fallback when the browser doesn't open).
func providerWizardOAuthCLIHint(provider providercatalog.Descriptor) string {
	if provider.OAuthMintsKey {
		return "zero auth openrouter"
	}
	return "zero auth login " + provider.ID
}

func (wizard *providerWizardState) renderProviderStep(width int) []string {
	header := "Choose provider"
	if wizard.oauthMode {
		header = "Choose an OAuth provider"
	}
	lines := []string{zeroTheme.accent.Render(header)}
	lines = append(lines, wizard.renderProviderSearch(width))
	providers := wizard.filteredProviders()
	if len(providers) == 0 {
		lines = append(lines, zeroTheme.faint.Render("  no matching providers"))
		return lines
	}
	maxVisible := minInt(maxProviderWizardProvidersVisible, len(providers))
	start := selectableListStart(len(providers), maxVisible, wizard.selectedProvider)
	for offset, provider := range providers[start : start+maxVisible] {
		lines = append(lines, wizard.renderSelectableProvider(width, start+offset, provider))
	}
	// A failed OAuth attempt leaves the wizard on this list (it does not advance),
	// so the error must render here or the click looks like a silent no-op. The
	// credential step renders its own copy for the ctrl+o path.
	if wizard.oauthMode && wizard.oauthErr != "" {
		lines = append(lines, "", zeroTheme.red.Render("OAuth login failed: "+wizard.oauthErr))
		if hint := providerWizardOAuthErrHint(wizard.currentProvider()); hint != "" {
			lines = append(lines, zeroTheme.faint.Render(hint))
		}
	}
	return lines
}

func (wizard *providerWizardState) renderProviderSearch(width int) string {
	query := strings.TrimSpace(wizard.providerSearch)
	return providerWizardInputLine("search > ", query, "provider name, id, or alias...", width)
}

// providerWizardOAuthErrHint returns a provider-specific next step for a failed
// OAuth login. Hugging Face ships no public client_id (unlike xAI), so a login
// can only work once the operator registers an app and supplies its client_id.
func providerWizardOAuthErrHint(provider providercatalog.Descriptor) string {
	if strings.EqualFold(provider.ID, "huggingface") {
		return "Hugging Face needs your own app: create one at " +
			"https://huggingface.co/settings/applications/new, then set ZERO_OAUTH_HUGGINGFACE_CLIENT_ID."
	}
	return ""
}

func (wizard *providerWizardState) renderSelectableProvider(width int, index int, provider providercatalog.Descriptor) string {
	selected := index == wizard.selectedProvider
	surface := transparentSurface
	marker := surface(zeroTheme.faintest).Render("  ")
	if selected {
		surface = zeroTheme.onSel
		marker = surface(zeroTheme.accent).Render("❯ ")
	}
	name := provider.Name
	if provider.Recommended {
		name = "★ " + name
	}
	left := marker + surface(zeroTheme.ink).Render(name)
	if badge := providerWizardBadge(provider); badge != "" {
		left += surface(zeroTheme.faint).Render("   " + badge)
	}
	return fitStyledLine(left, width)
}

// providerWizardBadge is the faint hint shown next to a provider row. The
// recommended provider takes precedence over the OAuth hint.
func providerWizardBadge(provider providercatalog.Descriptor) string {
	if provider.Recommended {
		return "(recommended)"
	}
	return providerWizardOAuthBadge(provider)
}

// providerWizardOAuthBadge is the faint mode hint shown next to OAuth providers.
func providerWizardOAuthBadge(provider providercatalog.Descriptor) string {
	if !provider.OAuth {
		return ""
	}
	if provider.OAuthMintsKey {
		return "browser sign-in · creates a key"
	}
	if provider.OAuthDeviceFlow {
		return "browser or device code"
	}
	return "browser sign-in"
}

func (wizard *providerWizardState) renderEndpointStep(width int) []string {
	provider := wizard.currentProvider()
	input := providerWizardInputLine("url > ", wizard.baseURL, providerWizardEndpointPlaceholder(provider), width)
	return []string{
		zeroTheme.accent.Render("Endpoint URL"),
		zeroTheme.ink.Render("Enter the API base URL for " + provider.Name + "."),
		zeroTheme.faint.Render(providerWizardEndpointHint(provider)),
		input,
	}
}

func providerWizardEndpointPlaceholder(provider providercatalog.Descriptor) string {
	if provider.Transport == providercatalog.TransportAnthropicCompatible {
		return "https://api.example.com/anthropic"
	}
	return "https://api.example.com/v1"
}

func providerWizardEndpointHint(provider providercatalog.Descriptor) string {
	if provider.Transport == providercatalog.TransportAnthropicCompatible {
		return "Use the base URL before /v1/messages."
	}
	return "Use the base URL before /chat/completions."
}

func (wizard *providerWizardState) renderNameStep(width int) []string {
	name := providerWizardDisplayName(wizard.currentProvider(), wizard.baseURL, wizard.profileName)
	return []string{
		zeroTheme.accent.Render("Provider name"),
		zeroTheme.ink.Render("Choose the short label shown in the status bar."),
		zeroTheme.faint.Render("Leave blank to use " + name + "."),
		providerWizardInputLine("name > ", strings.TrimSpace(wizard.profileName), name, width),
	}
}

func (wizard *providerWizardState) renderCredentialStep(width int) []string {
	provider := wizard.currentProvider()
	oauth := providerWizardSupportsOAuth(provider)

	env := firstProviderDisplayValue(provider.AuthEnvVars...)
	value := zeroTheme.accent.Render("▌") + zeroTheme.faint.Render("paste key here")
	if wizard.apiKey != "" {
		value = zeroTheme.ink.Render(maskedProviderWizardKey(wizard.apiKey)) + zeroTheme.accent.Render("▌")
	}
	input := zeroTheme.userPrompt.Render("api key > ") + value
	lines := []string{
		zeroTheme.accent.Render("Paste API key"),
		zeroTheme.ink.Render(providerWizardCredentialInstruction(env)),
		input,
		zeroTheme.faint.Render("Pasted keys are hidden and saved in your user config."),
	}
	if oauth {
		lines = append(lines, zeroTheme.accent.Render("or  ctrl+o  to log in with OAuth in the browser (no key needed)"))
	}
	if wizard.oauthErr != "" {
		lines = append(lines, zeroTheme.red.Render("OAuth login failed: "+wizard.oauthErr))
	}
	return lines
}

func providerWizardCredentialInstruction(env string) string {
	if env = strings.TrimSpace(env); env != "" {
		return "Paste a key, or leave blank to use " + env + "."
	}
	return "Paste a key, or leave blank to use your shell env."
}

func (wizard *providerWizardState) renderModelStep(width int) []string {
	if providerWizardUsesTypedModel(wizard.currentProvider()) {
		return wizard.renderTypedModelStep(width)
	}
	if wizard.modelLoading {
		return wizard.renderModelLoadingStep(width)
	}
	lines := []string{zeroTheme.accent.Render("Choose a model")}
	if status := wizard.modelStatusText(); status != "" {
		lines = append(lines, zeroTheme.faint.Render(status))
	}
	lines = append(lines, wizard.renderModelSearch(width))
	wizard.refreshModels()
	models := wizard.filteredModels()
	if len(models) == 0 {
		lines = append(lines, zeroTheme.faint.Render("  no matching models"))
		return lines
	}
	maxVisible := minInt(maxProviderWizardModelsVisible, len(models))
	wizard.selectedModel = clampInt(wizard.selectedModel, 0, len(models)-1)
	start := selectableListStart(len(models), maxVisible, wizard.selectedModel)
	for offset, model := range models[start : start+maxVisible] {
		lines = append(lines, wizard.renderSelectableModel(width, start+offset, model))
	}
	if detail := providerWizardModelDetail(wizard.currentModel()); detail != "" {
		lines = append(lines, fitStyledLine(zeroTheme.faint.Render("  "+detail), width))
	}
	return lines
}

func (wizard *providerWizardState) renderModelLoadingStep(width int) []string {
	return []string{
		zeroTheme.accent.Render("Choose a model"),
		"",
		fitStyledLine(zeroTheme.faint.Render("Checking available models..."), width),
		fitStyledLine(zeroTheme.faint.Render("Built-in models will be used if discovery fails."), width),
	}
}

func (wizard *providerWizardState) renderModelSearch(width int) string {
	query := strings.TrimSpace(wizard.modelSearch)
	return providerWizardInputLine("search > ", query, "model name...", width)
}

func providerWizardInputLine(promptText string, value string, placeholder string, width int) string {
	prompt := zeroTheme.userPrompt.Render("search > ")
	cursor := zeroTheme.accent.Render("▌")
	if promptText != "" {
		prompt = zeroTheme.userPrompt.Render(promptText)
	}
	if value == "" {
		return fitStyledLine(prompt+cursor+zeroTheme.faint.Render(placeholder), width)
	}
	return fitStyledLine(prompt+zeroTheme.ink.Render(value)+cursor, width)
}

func (wizard *providerWizardState) renderTypedModelStep(width int) []string {
	provider := wizard.currentProvider()
	return []string{
		zeroTheme.accent.Render("Model name"),
		zeroTheme.ink.Render("Enter the model ID this endpoint expects."),
		zeroTheme.faint.Render("Examples: gpt-4.1, claude-sonnet-4-5, llama-3.3-70b"),
		providerWizardInputLine("model > ", strings.TrimSpace(wizard.modelSearch), provider.DefaultModel, width),
	}
}

func (wizard *providerWizardState) modelStatusText() string {
	if wizard.modelLoadError != "" {
		return "Using built-in model list"
	}
	return ""
}

func (wizard *providerWizardState) renderSelectableModel(width int, index int, model providerWizardModel) string {
	selected := index == wizard.selectedModel
	surface := transparentSurface
	marker := surface(zeroTheme.faintest).Render("  ")
	if selected {
		surface = zeroTheme.onSel
		marker = surface(zeroTheme.accent).Render("❯ ")
	}
	left := marker + surface(zeroTheme.ink).Render(model.displayLabel())
	return fitStyledLine(left, width)
}

func providerWizardModelDetail(model providerWizardModel) string {
	parts := []string{}
	if secondary := strings.TrimSpace(model.secondaryText()); secondary != "" && !providerWizardGenericModelDescription(secondary) {
		parts = append(parts, secondary)
	}
	if meta := strings.TrimSpace(model.Meta); meta != "" {
		parts = append(parts, meta)
	}
	return strings.Join(parts, " · ")
}

func (wizard *providerWizardState) filteredModels() []providerWizardModel {
	if wizard == nil {
		return nil
	}
	query := strings.ToLower(strings.TrimSpace(wizard.modelSearch))
	if query == "" {
		return append([]providerWizardModel{}, wizard.models...)
	}
	models := make([]providerWizardModel, 0, len(wizard.models))
	for _, model := range wizard.models {
		if model.matches(query) {
			models = append(models, model)
		}
	}
	return models
}

// A model Description is only a good row label when it reads like a display
// NAME — "Grok 4.3" for x-ai/grok-4.3 — which is what the field is for, and why
// the row prefers it over an ugly raw ID.
//
// Providers do not all honour that. Aggregators commonly return a prose blurb
// instead ("Fast DeepSeek model for efficient chat, coding help, and agent
// loops"), which fails as a label twice over: it overflows the row, and because
// such blurbs are written per model FAMILY rather than per model, several
// DIFFERENT models collapse to byte-identical rows. A picker then offers two
// visually identical entries with the distinguishing ID visible only in the
// detail line of whichever one is highlighted.
//
// providerWizardGenericModelDescription cannot catch these: it rejects
// descriptions that are vague ("live model", "catalog default"), whereas these
// are specific — just shared and sentence-shaped. So the test is shape, not
// wording: a name is short and few-worded; anything longer is prose, and the ID
// (unique by construction) makes the better label.
const (
	providerWizardMaxModelNameChars = 28
	providerWizardMaxModelNameWords = 4
)

func providerWizardDescriptionIsDisplayName(description string) bool {
	trimmed := strings.TrimSpace(description)
	if trimmed == "" || providerWizardGenericModelDescription(trimmed) {
		return false
	}
	if len(trimmed) > providerWizardMaxModelNameChars {
		return false
	}
	return len(strings.Fields(trimmed)) <= providerWizardMaxModelNameWords
}

func (model providerWizardModel) displayLabel() string {
	if providerWizardDescriptionIsDisplayName(model.Description) {
		return strings.TrimSpace(model.Description)
	}
	if id := strings.TrimSpace(model.ID); id != "" {
		return id
	}
	// ID-less sentinel rows built in currentModel ("model name required",
	// "no matching models") would otherwise render blank.
	return strings.TrimSpace(model.Description)
}

func (model providerWizardModel) secondaryText() string {
	if model.displayLabel() != model.ID {
		return model.ID
	}
	return model.Description
}

func (model providerWizardModel) matches(query string) bool {
	if query == "" {
		return true
	}
	haystack := strings.ToLower(strings.Join([]string{model.ID, model.Description, model.Meta}, " "))
	return strings.Contains(haystack, query)
}

func providerWizardGenericModelDescription(description string) bool {
	switch strings.ToLower(strings.TrimSpace(description)) {
	case "", "catalog default", "catalog model", "custom endpoint model", "live model":
		return true
	default:
		return strings.HasSuffix(strings.ToLower(strings.TrimSpace(description)), " model")
	}
}

func (wizard *providerWizardState) renderDoneStep(width int) []string {
	provider := wizard.currentProvider()
	model := wizard.currentModel()
	lines := []string{
		zeroTheme.accent.Render("Ready to connect"),
		"",
		zeroTheme.ink.Render("Provider    " + provider.Name),
	}
	if providerWizardNeedsEndpoint(provider) {
		lines = append(lines, zeroTheme.ink.Render("Endpoint    "+strings.TrimSpace(wizard.baseURL)))
	}
	lines = append(lines,
		zeroTheme.ink.Render("Name        "+providerWizardDisplayName(provider, wizard.baseURL, wizard.profileName)),
		zeroTheme.ink.Render("Model       "+model.ID),
		zeroTheme.ink.Render("Credential  "+providerWizardCredentialLabel(provider, wizard.apiKey)),
		"",
		zeroTheme.faint.Render("Press Enter to save and start using this provider."),
	)
	return lines
}

func providerWizardCredentialLabel(provider providercatalog.Descriptor, apiKey string) string {
	if strings.TrimSpace(apiKey) != "" {
		return "pasted key"
	}
	// Mirror providerWizardProfile: a custom endpoint left blank is saved with
	// no APIKeyEnv, so the summary must not claim it will read one — that
	// would misdescribe the profile actually being saved (issue #555 follow-up).
	if env := firstProviderDisplayValue(provider.AuthEnvVars...); provider.RequiresAuth && env != "" && !provider.Custom {
		return env + " env var"
	}
	return "not required"
}

func maskedProviderWizardKey(value string) string {
	count := len([]rune(value))
	if count == 0 {
		return ""
	}
	if count > 24 {
		count = 24
	}
	return strings.Repeat("*", count)
}

// isAtomicLocalPlaceholderModel rejects Atomic Chat's nonfunctional fallback.
func isAtomicLocalPlaceholderModel(providerID string, model string, defaultModel string) bool {
	if providerID != "atomic-chat-local" {
		return false
	}
	resolved := strings.TrimSpace(model)
	return resolved == "" || resolved == strings.TrimSpace(defaultModel)
}

func providerWizardProfile(provider providercatalog.Descriptor, model string, apiKey string, baseURL string, profileName string) config.ProviderProfile {
	resolvedBaseURL := firstProviderDisplayValue(strings.TrimSpace(baseURL), provider.DefaultBaseURL)
	profile := config.ProviderProfile{
		Name:         providerWizardDisplayName(provider, baseURL, profileName),
		ProviderKind: providerWizardProviderKind(provider),
		CatalogID:    provider.ID,
		BaseURL:      resolvedBaseURL,
		APIFormat:    providerWizardAPIFormat(provider),
		Model:        firstProviderDisplayValue(model, provider.DefaultModel),
	}
	// Catalog custom headers (e.g. aimlapi.com's partner attribution) belong to the
	// catalog endpoint: the resolver only applies them when the base URL is the
	// default, so a profile built against a staging/proxy/custom override must not
	// bake them in either — otherwise attribution leaks to an arbitrary host.
	if sameProviderBaseURL(resolvedBaseURL, provider.DefaultBaseURL) {
		profile.CustomHeaders = maps.Clone(provider.CustomHeaders)
		if providerWizardIsAimlapi(provider) {
			profile.CustomHeaders = aimlapi.WithResolvedPartnerHeader(profile.CustomHeaders)
		}
	}
	if apiKey = strings.TrimSpace(apiKey); apiKey != "" {
		profile.APIKey = apiKey
	} else if env := firstProviderDisplayValue(provider.AuthEnvVars...); provider.RequiresAuth && env != "" && !provider.Custom {
		// Custom endpoints don't get a guessed env var default: RequiresAuth on
		// these two catalog entries is just the wizard's template default (it
		// doesn't know whether the user's own endpoint needs auth), so a blank
		// credential step here means "this endpoint needs no auth," not "read
		// OPENAI_API_KEY/ANTHROPIC_API_KEY at runtime."
		profile.APIKeyEnv = env
	}
	return profile
}

// sameProviderBaseURL reports whether two base URLs address the same endpoint,
// normalizing case and a trailing slash — mirroring the resolver's sameBaseURL so
// the wizard bakes catalog headers on exactly the endpoints the resolver would.
func sameProviderBaseURL(left string, right string) bool {
	return strings.EqualFold(
		strings.TrimRight(strings.TrimSpace(left), "/"),
		strings.TrimRight(strings.TrimSpace(right), "/"),
	)
}

func providerWizardEndpointError(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "enter an endpoint URL before continuing"
	}
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "enter a valid endpoint URL including https://"
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "endpoint URL must start with http:// or https://"
	}
	if parsed.Scheme == "http" && !providerWizardIsLoopbackHost(parsed.Hostname()) {
		return "endpoint URL must use https:// unless it is local loopback or a private network address"
	}
	return ""
}

func providerWizardIsLoopbackHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}

func providerWizardDisplayName(provider providercatalog.Descriptor, baseURL string, profileName string) string {
	if name := strings.TrimSpace(profileName); name != "" {
		return name
	}
	if providerWizardNeedsProfileName(provider) {
		return providerWizardNameFromBaseURL(baseURL)
	}
	return provider.ID
}

func providerWizardNameFromBaseURL(baseURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Host == "" {
		return "custom"
	}
	host := strings.ToLower(parsed.Hostname())
	if ip := net.ParseIP(host); ip != nil {
		return providerWizardSanitizedName("ip-" + ip.String())
	}
	host = strings.TrimPrefix(host, "api.")
	host = strings.TrimPrefix(host, "gateway.")
	parts := strings.Split(host, ".")
	if len(parts) >= 2 {
		return parts[len(parts)-2]
	}
	if host != "" {
		return strings.ReplaceAll(host, ":", "-")
	}
	return "custom"
}

func providerWizardSanitizedName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	lastDash := false
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			builder.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash && builder.Len() > 0 {
			builder.WriteRune('-')
			lastDash = true
		}
	}
	return strings.Trim(builder.String(), "-")
}

func providerWizardProviderKind(provider providercatalog.Descriptor) config.ProviderKind {
	switch provider.Transport {
	case providercatalog.TransportOpenAI:
		return config.ProviderKindOpenAI
	case providercatalog.TransportAnthropic:
		return config.ProviderKindAnthropic
	case providercatalog.TransportAnthropicCompatible:
		return config.ProviderKindAnthropicCompat
	case providercatalog.TransportGoogle:
		return config.ProviderKindGoogle
	case providercatalog.TransportOpenAICompatible:
		return config.ProviderKindOpenAICompatible
	default:
		return config.ProviderKind(strings.ToLower(string(provider.Transport)))
	}
}

func providerWizardAPIFormat(provider providercatalog.Descriptor) string {
	if provider.Transport == providercatalog.TransportOpenAI || provider.Transport == providercatalog.TransportOpenAICompatible {
		return string(providercatalog.APIFormatOpenAIChatCompletions)
	}
	if len(provider.SupportedAPIFormats) == 0 {
		return ""
	}
	return string(provider.SupportedAPIFormats[0])
}
