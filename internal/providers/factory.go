package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/oauth"
	"github.com/Gitlawb/zero/internal/providercatalog"
	"github.com/Gitlawb/zero/internal/providermodelcatalog"
	"github.com/Gitlawb/zero/internal/providers/anthropic"
	"github.com/Gitlawb/zero/internal/providers/gemini"
	"github.com/Gitlawb/zero/internal/providers/openai"
	"github.com/Gitlawb/zero/internal/providers/providerio"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// Options configures provider construction.
type Options struct {
	UserAgent     string
	HTTPClient    *http.Client
	ModelRegistry *modelregistry.Registry
	// OAuthResolver, when set, lets the provider authenticate model calls with an
	// OAuth bearer token (preferred over the API key). nil => API-key auth only.
	// Applied to the OpenAI, Anthropic, and Google (Gemini) providers.
	OAuthResolver providerio.TokenResolver
	// OAuthLoginKey is the credential-store key OAuthResolver bound to (empty when
	// there is no OAuth login). It is the SAME selection the bearer resolver made,
	// resolved ONCE by the caller so the Codex chatgpt-account-id header reads its
	// account from the exact login that issued the bearer token — never a second,
	// independent lookup that could pick a different login (a backend-rejected
	// mismatch).
	OAuthLoginKey string
}

// New creates a runtime provider for a resolved provider profile.
func New(profile config.ProviderProfile, options Options) (zeroruntime.Provider, error) {
	resolved, err := resolveProfile(profile, options)
	if err != nil {
		return nil, err
	}

	// The ChatGPT (Codex) catalog requires the Codex-flavored provider: the
	// Codex backend (chatgpt.com/backend-api/codex) 401s on every request that
	// does not carry the `originator` + `chatgpt-account-id` headers, so a
	// plain openai.New would always fail. We branch off the catalog id here
	// (not the provider kind) so other "openai-compatible" providers keep
	// using the openai.New path unchanged.
	if isCodexCatalog(profile, resolved) {
		return newCodexProvider(profile, resolved, options)
	}

	sessionHeader := sessionHeaderForProfile(profile, resolved)
	if sessionHeader != "" && strings.TrimSpace(options.UserAgent) == "" {
		options.UserAgent = "zero"
	}

	switch resolved.providerKind {
	case config.ProviderKindOpenAI, config.ProviderKindOpenAICompatible:
		// prompt_cache_key is an OpenAI-only chat-completions field. Strict
		// openai-compatible gateways (NVIDIA NIM, etc.) reject it with a 400
		// instead of ignoring unknown parameters — so omit it for every
		// openai-compatible profile. Official OpenAI keeps the field so
		// multi-turn sessions still route to a cached-prefix replica.
		return openai.New(openai.Options{
			APIKey:                profile.APIKey,
			BaseURL:               resolved.baseURL,
			Model:                 resolved.apiModel,
			AuthHeader:            profile.AuthHeader,
			AuthScheme:            profile.AuthScheme,
			AuthHeaderValue:       profile.AuthHeaderValue,
			CustomHeaders:         providerio.CopyHeaders(profile.CustomHeaders),
			OAuthResolver:         options.OAuthResolver,
			MaxTokens:             resolved.maxOutputTokens,
			HTTPClient:            options.HTTPClient,
			UserAgent:             options.UserAgent,
			ParseThinkTags:        parseThinkTagsForProfile(profile, resolved),
			DisablePromptCacheKey: resolved.providerKind == config.ProviderKindOpenAICompatible,
			SessionHeader:         sessionHeader,
		})
	case config.ProviderKindAnthropic, config.ProviderKindAnthropicCompat:
		return anthropic.New(anthropic.Options{
			APIKey:          profile.APIKey,
			BaseURL:         resolved.baseURL,
			Model:           resolved.apiModel,
			AuthHeader:      profile.AuthHeader,
			AuthScheme:      profile.AuthScheme,
			AuthHeaderValue: profile.AuthHeaderValue,
			CustomHeaders:   providerio.CopyHeaders(profile.CustomHeaders),
			OAuthResolver:   options.OAuthResolver,
			MaxTokens:       resolved.maxOutputTokens,
			HTTPClient:      options.HTTPClient,
			UserAgent:       options.UserAgent,
			SessionHeader:   sessionHeader,
		})
	case config.ProviderKindGoogle:
		return gemini.New(gemini.Options{
			APIKey:          profile.APIKey,
			BaseURL:         resolved.baseURL,
			Model:           resolved.apiModel,
			AuthHeader:      profile.AuthHeader,
			AuthScheme:      profile.AuthScheme,
			AuthHeaderValue: profile.AuthHeaderValue,
			CustomHeaders:   providerio.CopyHeaders(profile.CustomHeaders),
			OAuthResolver:   options.OAuthResolver,
			MaxTokens:       resolved.maxOutputTokens,
			HTTPClient:      options.HTTPClient,
			UserAgent:       options.UserAgent,
		})
	default:
		return nil, fmt.Errorf("unsupported provider kind %q", resolved.providerKind)
	}
}

// NewTurnSessionProvider builds the default TurnSessionProvider for a resolved
// profile: it constructs the provider exactly as New does, then wraps it with a
// ProviderCapabilities projection computed from the same model-registry entry
// New already resolves. The default session's Stream is the provider's
// StreamCompletion, so runtime behavior is identical to using New directly.
func NewTurnSessionProvider(profile config.ProviderProfile, options Options) (zeroruntime.TurnSessionProvider, error) {
	provider, err := New(profile, options)
	if err != nil {
		return nil, err
	}
	caps, err := resolveCapabilities(profile, options)
	if err != nil {
		return nil, err
	}
	return zeroruntime.NewProviderTurnSessionProvider(provider, caps), nil
}

// resolveCapabilities projects the resolved profile's model-registry entry into
// the flat zeroruntime.ProviderCapabilities (zeroruntime cannot reference
// modelregistry types — modelregistry imports zeroruntime, so a typed field
// would form an import cycle). A model absent from the registry yields only the
// resolved API model id and max-output tokens; the rest stays zero (unknown).
func resolveCapabilities(profile config.ProviderProfile, options Options) (zeroruntime.ProviderCapabilities, error) {
	resolved, err := resolveProfile(profile, options)
	if err != nil {
		return zeroruntime.ProviderCapabilities{}, err
	}
	caps := zeroruntime.ProviderCapabilities{
		Model:           resolved.apiModel,
		MaxOutputTokens: resolved.maxOutputTokens,
	}
	registry, err := defaultRegistry(options.ModelRegistry)
	if err != nil {
		return zeroruntime.ProviderCapabilities{}, err
	}
	if entry, ok := registry.Get(strings.TrimSpace(profile.Model)); ok {
		caps.ContextWindow = entry.ContextLimits.ContextWindow
		caps.SupportsVision = entry.Supports(modelregistry.ModelCapabilityVision)
		caps.SupportsReasoning = entry.Supports(modelregistry.ModelCapabilityReasoning)
		caps.SupportsPromptCache = entry.Supports(modelregistry.ModelCapabilityPromptCache)
		// Read efforts through Registry.ReasoningEfforts (not the raw entry) so
		// the projection matches what the /effort picker and the run-time
		// resolver advertise — including the name-based fallback for catalog
		// entries that enumerate no efforts of their own.
		for _, effort := range registry.ReasoningEfforts(entry.ID) {
			caps.ReasoningEfforts = append(caps.ReasoningEfforts, string(effort))
		}
	}
	return caps, nil
}

func parseThinkTagsForProfile(profile config.ProviderProfile, resolved resolvedProfile) bool {
	if profile.ParseThinkTags != nil {
		return *profile.ParseThinkTags
	}
	if resolved.providerKind != config.ProviderKindOpenAICompatible {
		return false
	}
	return modelMayEmitThinkTags(resolved.apiModel)
}

func modelMayEmitThinkTags(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	for _, marker := range []string{
		"deepseek-r1",
		"deepseek-reasoner",
		"gpt-oss",
		"glm-z1",
		"kimi-k2-thinking",
		"magistral",
		"minimax-m2",
		"minimax-m3",
		"nemotron",
		"qwen3",
		"qwq",
		"reasoner",
		"reasoning",
		"thinking",
	} {
		if strings.Contains(model, marker) {
			return true
		}
	}
	return false
}

type resolvedProfile struct {
	providerKind    config.ProviderKind
	apiModel        string
	baseURL         string
	maxOutputTokens int
}

// RuntimeMetadata describes the provider identity and concrete API model used
// after Zero model aliases and provider-kind defaults are resolved.
type RuntimeMetadata struct {
	ProviderKind config.ProviderKind
	APIModel     string
}

// ResolveRuntimeMetadata returns the provider kind and API model that New would
// use for a profile, without constructing a network client.
func ResolveRuntimeMetadata(profile config.ProviderProfile, options Options) (RuntimeMetadata, error) {
	resolved, err := resolveProfile(profile, options)
	if err != nil {
		return RuntimeMetadata{}, err
	}
	return RuntimeMetadata{
		ProviderKind: resolved.providerKind,
		APIModel:     resolved.apiModel,
	}, nil
}

func resolveProfile(profile config.ProviderProfile, options Options) (resolvedProfile, error) {
	model := strings.TrimSpace(profile.Model)
	if model == "" {
		return resolvedProfile{}, fmt.Errorf("provider %s requires model", profile.Name)
	}
	providerKind, explicitProvider := explicitProviderKind(profile)
	registry, err := defaultRegistry(options.ModelRegistry)
	if err != nil {
		return resolvedProfile{}, err
	}

	// baseURL resolves in this order: profile override → catalog default.
	// The catalog default makes the chatgpt Codex backend Just Work when a
	// user runs `zero` with a `catalogID: chatgpt` profile; the user can
	// still pin their own URL (e.g. a self-hosted Codex gateway or a local
	// OAuth proxy) and it wins.
	baseURL := strings.TrimSpace(profile.BaseURL)
	if baseURL == "" {
		if descriptor, ok := providercatalog.Get(profile.CatalogID); ok {
			baseURL = strings.TrimSpace(descriptor.DefaultBaseURL)
		}
	}

	if entry, ok := registry.Get(model); ok {
		modelProvider := configKind(entry.Provider)
		// Adopt the registry entry's provider only when the caller did not pin one.
		// (The old `|| isImplicitOpenAI(...)` clause was dead: explicitProvider==true
		// means ProviderKind or Provider is set, but isImplicitOpenAI required both
		// empty, so it could never add a case.)
		if !explicitProvider {
			providerKind = modelProvider
		}
		if providerKind == config.ProviderKindOpenAICompatible {
			if !entry.AllowsProvider(modelregistry.ProviderOpenAICompatible) {
				return resolvedProfile{}, fmt.Errorf("zero model %s belongs to %s, not %s", entry.ID, entry.Provider, modelregistry.ProviderOpenAICompatible)
			}
		} else if providerKind == config.ProviderKindAnthropicCompat {
			if !entry.AllowsProvider(modelregistry.ProviderAnthropic) {
				return resolvedProfile{}, fmt.Errorf("zero model %s belongs to %s, not %s", entry.ID, entry.Provider, providerKind)
			}
		} else if providerKind != modelProvider {
			return resolvedProfile{}, fmt.Errorf("zero model %s belongs to %s, not %s", entry.ID, entry.Provider, providerKind)
		}
		if err := validateModelAllowedForProvider(profile, entry.ID); err != nil {
			return resolvedProfile{}, err
		}
		return resolvedProfile{
			providerKind:    providerKind,
			apiModel:        entry.APIModel,
			baseURL:         baseURL,
			maxOutputTokens: entry.ContextLimits.MaxOutputTokens,
		}, nil
	}

	if providerKind == "" {
		providerKind = config.ProviderKindOpenAI
	}
	if err := validateModelAllowedForProvider(profile, model); err != nil {
		return resolvedProfile{}, err
	}
	return resolvedProfile{
		providerKind: providerKind,
		apiModel:     model,
		baseURL:      baseURL,
	}, nil
}

// validateModelAllowedForProvider enforces provider-scoped model allowlists at
// runtime resolution time. It delegates to
// providermodelcatalog.ModelIDAllowedForProvider, which encodes the per-provider
// rules in one place; restricted providers (such as
// opencode-go-anthropic-compatible) only accept a curated subset of model
// families, while the default branch makes unrestricted providers a no-op. This
// guards both New() (TUI /model, headless --model overrides) and
// ResolveRuntimeMetadata (the read-only metadata path) so a catalog-backed
// profile cannot persist or send a model outside its scoped allowlist.
func validateModelAllowedForProvider(profile config.ProviderProfile, model string) error {
	providerID := strings.TrimSpace(profile.CatalogID)
	if providermodelcatalog.ModelIDAllowedForProvider(providerID, model) {
		return nil
	}
	if providerID == "" {
		providerID = strings.TrimSpace(profile.Name)
	}
	if providerID == "" {
		providerID = "provider"
	}
	return fmt.Errorf("provider %s does not allow model %s", providerID, strings.TrimSpace(model))
}

func explicitProviderKind(profile config.ProviderProfile) (config.ProviderKind, bool) {
	providerKind := config.ProviderKind(strings.TrimSpace(strings.ToLower(string(profile.ProviderKind))))
	if providerKind != "" {
		return providerKind, true
	}
	provider := strings.TrimSpace(strings.ToLower(profile.Provider))
	if provider != "" {
		return config.ProviderKind(provider), true
	}
	return "", false
}

func configKind(provider modelregistry.ProviderKind) config.ProviderKind {
	return config.ProviderKind(provider)
}

func defaultRegistry(registry *modelregistry.Registry) (modelregistry.Registry, error) {
	if registry != nil {
		return *registry, nil
	}
	return modelregistry.DefaultRegistry()
}

// isCodexCatalog reports whether the profile targets the ChatGPT Codex
// backend, which requires the Codex-flavored openai provider. The check uses
// the catalog id (not the provider kind) so the openai-compatible path is
// unaffected for other "openai-compatible" providers — a profile that
// happens to use a /v1 baseURL pointing at chatgpt.com without the chatgpt
// catalog id is the user's explicit misconfiguration, and the openai
// provider's standard error path surfaces it.
func isCodexCatalog(profile config.ProviderProfile, _ resolvedProfile) bool {
	return providercatalog.NormalizeID(profile.CatalogID) == "chatgpt"
}

func sessionHeaderForProfile(profile config.ProviderProfile, resolved resolvedProfile) string {
	switch providercatalog.NormalizeID(profile.CatalogID) {
	case "opencode", "opencode-go", "opencode-go-anthropic-compatible":
		return "x-opencode-session"
	}
	endpoint, err := url.Parse(resolved.baseURL)
	if err == nil && strings.EqualFold(endpoint.Hostname(), "opencode.ai") && (endpoint.Path == "/zen" || strings.HasPrefix(endpoint.Path, "/zen/")) {
		return "x-opencode-session"
	}
	return ""
}

// newCodexProvider builds a Codex-flavored openai provider for the chatgpt
// catalog. The Codex headers (`originator`, `chatgpt-account-id`) are
// injected by the CodexProvider wrapper. The `chatgpt-account-id` is read from
// the stored OAuth token's Account field on every request so a refresh that
// rotates the token (and its account claim) takes effect immediately — a static
// AccountID captured at construction would go stale on the first refresh.
//
// The *login* the header reads from is options.OAuthLoginKey — the SAME key the
// bearer resolver bound, resolved ONCE by the caller. The bearer resolver
// refreshes the token in place under that key; reading the account from the same
// key keeps header and token on one login for the provider's life. Resolving the
// key independently here (a second oauth.FirstStored) could, after a transient
// per-candidate load error or a mid-session `zero auth login`, pick a different
// login than the bearer — a mismatch the backend rejects.
func newCodexProvider(profile config.ProviderProfile, resolved resolvedProfile, options Options) (zeroruntime.Provider, error) {
	accountKey := options.OAuthLoginKey
	return openai.NewCodexProvider(openai.CodexOptions{
		Options: openai.Options{
			BaseURL:         resolved.baseURL,
			Model:           resolved.apiModel,
			AuthHeader:      profile.AuthHeader,
			AuthScheme:      profile.AuthScheme,
			AuthHeaderValue: profile.AuthHeaderValue,
			CustomHeaders:   profile.CustomHeaders,
			OAuthResolver:   options.OAuthResolver,
			MaxTokens:       resolved.maxOutputTokens,
			HTTPClient:      options.HTTPClient,
			UserAgent:       options.UserAgent,
			ParseThinkTags:  parseThinkTagsForProfile(profile, resolved),
		},
		// The chatgpt catalog overrides this with the Codex baseURL
		// (https://chatgpt.com/backend-api/codex) when the user does not
		// pin one, so this branch only needs to handle a user-supplied
		// override. The codex provider's constructor derives the
		// `/responses` endpoint from BaseURL, so the factory stays out of
		// the path.
		AccountResolver: CodexAccountResolverForLogin(accountKey),
	})
}

// codexAccountForKey reads the chatgpt_account_id from the token currently
// stored under key. Called per-request (via the resolver) so a refresh that
// rotates the token — saved back in place under the same key — takes effect
// immediately without restarting the agent. Returns "" when key is empty (no
// OAuth login), or the token is absent or carries no account claim.
func codexAccountForKey(key string) string {
	if key == "" {
		return ""
	}
	store, err := oauth.NewStore(oauth.StoreOptions{})
	if err != nil {
		return ""
	}
	token, ok, err := store.Load(key)
	if err != nil || !ok {
		return ""
	}
	return strings.TrimSpace(token.Account)
}

// CodexAccountResolverForLogin returns the same per-request account resolver
// used by the runtime provider. Auxiliary Codex requests use this rather than
// independently selecting or parsing an OAuth login, which could mismatch the
// bearer selected by oauthLoginForProfile.
func CodexAccountResolverForLogin(key string) openai.CodexAccountResolver {
	return func(context.Context) (string, bool, error) {
		account := codexAccountForKey(key)
		return account, account != "", nil
	}
}
