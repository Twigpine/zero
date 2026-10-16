package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Gitlawb/zero/internal/aimlapi"
	"github.com/Gitlawb/zero/internal/browser"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/providercatalog"
	"github.com/Gitlawb/zero/internal/providermodeldiscovery"
	"github.com/Gitlawb/zero/internal/redaction"
)

type providerModelsDiscoveredMsg struct {
	providerID string
	token      int
	models     []providermodeldiscovery.Model
	err        error
	// secrets are redacted from any surfaced error (e.g. a resolved OAuth token
	// used to authenticate discovery, which must never be logged or shown).
	secrets []string
}

type aimlapiExistingBalanceMsg struct {
	wizard  *providerWizardState
	gen     int
	balance aimlapi.BalanceResult
	err     error
}

func (m model) advanceProviderWizard() (model, tea.Cmd) {
	if m.providerWizard == nil {
		return m, nil
	}
	// OAuth path: advancing from the OAuth provider list starts the browser/device
	// login instead of the key/endpoint flow.
	if m.providerWizard.step == providerWizardStepProvider && m.providerWizard.oauthMode && m.providerWizard.currentProvider().OAuth {
		provider := m.providerWizard.currentProvider()
		// Headless/SSH boxes can't open a browser — use device code there by
		// default (the user can also force it with "d" from the list).
		if provider.OAuthDeviceFlow && oauthPreferDeviceFlow() {
			return m.startProviderDeviceLogin()
		}
		attemptID := m.providerWizard.beginOAuthAttempt(false)
		return m, providerWizardOAuthCmdFor(provider, attemptID, m.userConfigPath)
	}
	// A non-OAuth provider that already has a key in the credential store: offer
	// keep/replace/remove before re-entering credentials.
	if m.providerWizard.step == providerWizardStepProvider && !m.providerWizard.oauthMode {
		if providerWizardIsAimlapi(m.providerWizard.currentProvider()) {
			if profile, runtimeKey, ok := m.existingAimlapiConfiguration(); ok {
				m.providerWizard.aimlapiExistingProfile = profile
				m.providerWizard.aimlapiRuntimeKey = runtimeKey
				m.providerWizard.aimlapiConfiguredCursor = 0
				m.providerWizard.err = ""
				m.providerWizard.step = providerWizardStepAimlapiConfigured
				return m, nil
			}
			m.providerWizard.enterAimlapi()
			return m, nil
		}
		if name, ok, err := m.wizardProviderStoredKey(m.providerWizard.currentProvider()); err != nil {
			m.providerWizard.err = redaction.ErrorMessage(err, redaction.Options{})
			return m, nil
		} else if ok {
			// Generic/custom providers (custom-openai-compatible etc.) all share
			// the same CatalogID — matching on CatalogID would block creating a
			// second instance. Skip ManageKey and fall through to the shared
			// advance() path below; the user can overwrite by re-entering the
			// same name or create a new one with a different name.
			if !providerWizardNeedsEndpoint(m.providerWizard.currentProvider()) {
				m.providerWizard.manageProviderName = name
				m.providerWizard.manageKeyCursor = 0
				m.providerWizard.err = ""
				m.providerWizard.step = providerWizardStepManageKey
				return m, nil
			}
			m.providerWizard.manageProviderName = ""
		}
	}
	previous := m.providerWizard.step
	m.providerWizard.advance()
	if m.providerWizard.step == providerWizardStepModel && previous != providerWizardStepModel {
		return m, m.providerModelDiscoveryCmd()
	}
	return m, nil
}

// existingAimlapiConfiguration returns a usable persisted credential source and
// its current runtime key. The active aimlapi profile wins; otherwise the first
// usable saved profile is used. A bare AIMLAPI_API_KEY is represented as an env
// profile so choosing it never snapshots the secret into the credential store.
func (m model) existingAimlapiConfiguration() (config.ProviderProfile, string, bool) {
	descriptor, descriptorErr := providercatalog.Require("aimlapi")
	profiles := append([]config.ProviderProfile(nil), m.savedProviders...)
	activeName := strings.TrimSpace(m.providerProfile.Name)
	if activeName != "" {
		for index, profile := range profiles {
			// Selecting the live session's own row: exact persisted spelling.
			if strings.TrimSpace(profile.Name) == activeName && aimlapiProfile(profile) {
				profiles[0], profiles[index] = profiles[index], profiles[0]
				break
			}
		}
	}
	var store config.APIKeyGetter
	if strings.TrimSpace(m.userConfigPath) != "" {
		store, _ = config.ProviderKeyStoreAt(filepath.Dir(m.userConfigPath))
	}
	for _, persisted := range profiles {
		if !aimlapiProfile(persisted) {
			continue
		}
		// The guided balance/top-up client talks to AIMLAPI's production account
		// API. Never send a credential saved for a proxy, staging service, or other
		// custom inference endpoint to that API. Such profiles remain available to
		// the normal provider runtime; they are simply ineligible for this preflight.
		if descriptorErr != nil || (strings.TrimSpace(persisted.BaseURL) != "" && !sameProviderBaseURL(persisted.BaseURL, descriptor.DefaultBaseURL)) {
			continue
		}
		resolved := persisted
		if strings.TrimSpace(resolved.APIKey) == "" && strings.TrimSpace(resolved.APIKeyEnv) != "" {
			resolved.APIKey = strings.TrimSpace(os.Getenv(resolved.APIKeyEnv))
		}
		resolved = config.ApplyStoredAPIKey(resolved, store)
		if key := strings.TrimSpace(resolved.APIKey); key != "" {
			// ResolvedConfig materializes APIKeyEnv into APIKey for runtime use.
			// Strip that materialized value (and any loaded stored key) from the
			// persisted copy so finalization retains the reference, not the secret.
			if strings.TrimSpace(persisted.APIKeyEnv) != "" || persisted.APIKeyStored {
				persisted.APIKey = ""
			}
			return persisted, key, true
		}
	}
	if key := strings.TrimSpace(os.Getenv("AIMLAPI_API_KEY")); key != "" {
		if descriptorErr == nil {
			profile := providerWizardProfile(
				descriptor,
				descriptor.DefaultModel,
				"",
				aimlapi.ResolveEndpoints().InferenceBaseURL,
				"",
			)
			return profile, key, true
		}
	}
	return config.ProviderProfile{}, "", false
}

func aimlapiProfile(profile config.ProviderProfile) bool {
	catalogID := strings.TrimSpace(profile.CatalogID)
	if catalogID != "" {
		return config.SameProviderIdentity(catalogID, "aimlapi")
	}
	return config.SameProviderIdentity(profile.Name, "aimlapi")
}

func (m model) checkExistingAimlapiBalance() (model, tea.Cmd) {
	wizard := m.providerWizard
	if wizard == nil || wizard.step != providerWizardStepAimlapiConfigured || wizard.aimlapiExistingBusy {
		return m, nil
	}
	wizard.aimlapiExistingBusy = true
	wizard.err = ""
	wizard.aimlapiExistingGen++
	gen := wizard.aimlapiExistingGen
	key := wizard.aimlapiRuntimeKey
	endpoints := aimlapi.ResolveEndpoints()
	if baseURL := strings.TrimSpace(wizard.aimlapiExistingProfile.BaseURL); baseURL != "" {
		endpoints.InferenceBaseURL = baseURL
	} else if descriptor, ok := providercatalog.Get("aimlapi"); ok {
		// An empty stored BaseURL means the catalog endpoint. Pin it explicitly so
		// a process-wide inference override cannot redirect this saved key.
		endpoints.InferenceBaseURL = descriptor.DefaultBaseURL
	}
	wizard.baseURL = endpoints.InferenceBaseURL
	ctx, cancel := context.WithTimeout(m.ctx, 12*time.Second)
	wizard.aimlapiExistingCancel = cancel
	cmd := func() tea.Msg {
		balance, err := aimlapi.NewClient(endpoints, nil).GetBalance(ctx, key)
		return aimlapiExistingBalanceMsg{wizard: wizard, gen: gen, balance: balance, err: err}
	}
	return m, tea.Batch(cmd, m.ensureSpinnerTick())
}

func (m model) applyExistingAimlapiBalance(msg aimlapiExistingBalanceMsg) (model, tea.Cmd) {
	wizard := m.providerWizard
	if wizard == nil || wizard != msg.wizard || wizard.step != providerWizardStepAimlapiConfigured || msg.gen != wizard.aimlapiExistingGen {
		return m, nil
	}
	if wizard.aimlapiExistingCancel != nil {
		wizard.aimlapiExistingCancel()
		wizard.aimlapiExistingCancel = nil
	}
	wizard.aimlapiExistingBusy = false
	if msg.err != nil {
		if aimlapiIsUnauthorized(msg.err) {
			wizard.err = "The existing AIMLAPI API key is invalid. Choose Configure again."
		} else {
			wizard.err = redaction.ErrorMessage(msg.err, redaction.Options{ExtraSecretValues: []string{wizard.aimlapiRuntimeKey}})
		}
		return m, nil
	}
	wizard.apiKey = wizard.aimlapiRuntimeKey
	if msg.balance.LowBalance {
		state := newAimlapiOnboard(browser.OpenURL)
		state.apiKey = wizard.aimlapiRuntimeKey
		state.baseURL = wizard.baseURL
		state.byKey = true
		state.step = aimlapiStepLowBalance
		wizard.aimlapi = state
		wizard.step = providerWizardStepAimlapi
		return m, nil
	}
	wizard.step = providerWizardStepModel
	return m, m.providerModelDiscoveryCmd()
}

func (m model) providerModelDiscoveryCmd() tea.Cmd {
	wizard := m.providerWizard
	if wizard == nil {
		return nil
	}
	provider := wizard.currentProvider()
	if !providerWizardCatalogDiscoveryAllowed(provider) {
		return nil
	}
	if providerWizardUsesTypedModel(provider) {
		return nil
	}
	pastedKey := wizard.apiKey
	// A token-login provider (e.g. xAI) stores its bearer in the OAuth store, not
	// as a pasted key; resolve it so /models is authenticated and the live list
	// shows after sign-in. (OpenRouter mints a key into wizard.apiKey already.)
	needOAuthToken := strings.TrimSpace(pastedKey) == "" && provider.OAuth && !provider.OAuthMintsKey
	discover := m.discoverProviderModels
	if discover == nil {
		discover = func(ctx context.Context, profile config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
			return providermodeldiscovery.DiscoverCatalog(ctx, provider, profile, providermodeldiscovery.Options{})
		}
	}

	wizard.modelLoading = true
	wizard.modelLoadError = ""
	wizard.discoveryToken++
	token := wizard.discoveryToken
	providerID := provider.ID
	baseURL := wizard.baseURL
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 12*time.Second)
		defer cancel()
		apiKey := pastedKey
		if needOAuthToken {
			if resolved := oauthStoredToken(ctx, providerID); resolved != "" {
				apiKey = resolved
			}
		}
		profile := providerWizardDiscoveryProfile(provider, apiKey, baseURL)
		models, err := discover(ctx, profile)
		return providerModelsDiscoveredMsg{providerID: providerID, token: token, models: models, err: err, secrets: []string{apiKey, profile.APIKey}}
	}
}

func (m model) applyProviderModelsDiscovered(msg providerModelsDiscoveredMsg) model {
	wizard := m.providerWizard
	if wizard == nil || wizard.step != providerWizardStepModel || wizard.currentProvider().ID != msg.providerID || msg.token != wizard.discoveryToken {
		return m
	}
	wizard.modelLoading = false
	if msg.err != nil {
		wizard.modelLoadError = redaction.RedactString(msg.err.Error(), redaction.Options{ExtraSecretValues: append([]string{wizard.apiKey}, msg.secrets...)})
		wizard.modelSource = "fallback"
		wizard.refreshModels()
		return m
	}
	models := providerWizardModelsFromDiscovery(msg.models)
	if len(models) == 0 {
		wizard.modelLoadError = "models endpoint returned no model ids"
		wizard.modelSource = "fallback"
		wizard.refreshModels()
		return m
	}
	wizard.models = models
	wizard.selectedModel = 0
	wizard.modelSource = providerWizardModelsSource(msg.models)
	if wizard.modelSource == "" {
		wizard.modelSource = "live"
	}
	wizard.modelLoadError = ""
	return m
}

func providerWizardModelsFromDiscovery(models []providermodeldiscovery.Model) []providerWizardModel {
	result := make([]providerWizardModel, 0, len(models))
	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		result = append(result, providerWizardModel{
			ID:          id,
			Description: firstProviderDisplayValue(model.Description, "live model"),
			Meta:        providerWizardModelMeta(model.ContextWindow, model.ToolCall, model.Reasoning, model.InputCost, model.OutputCost, model.Tags),
		})
	}
	return result
}

func providerWizardModelsSource(models []providermodeldiscovery.Model) string {
	for _, model := range models {
		if source := strings.TrimSpace(model.Source); source != "" {
			return source
		}
	}
	return ""
}

func providerWizardDiscoveryProfile(provider providercatalog.Descriptor, apiKey string, baseURL string) config.ProviderProfile {
	profile := providerWizardProfile(provider, provider.DefaultModel, apiKey, baseURL, "")
	if strings.TrimSpace(profile.APIKey) == "" && strings.TrimSpace(profile.APIKeyEnv) != "" {
		profile.APIKey = strings.TrimSpace(os.Getenv(profile.APIKeyEnv))
	}
	return profile
}

func providerWizardCatalogDiscoveryAllowed(provider providercatalog.Descriptor) bool {
	return providercatalog.RuntimeSupported(provider)
}
