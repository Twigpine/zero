package providermodeldiscovery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/providercatalog"
	"github.com/Gitlawb/zero/internal/providermodelcatalog"
	"github.com/Gitlawb/zero/internal/providers/openai"
	"github.com/Gitlawb/zero/internal/providers/providerio"
	"github.com/Gitlawb/zero/internal/redaction"
)

const (
	anthropicVersion             = "2023-06-01"
	chatGPTModelsProtocolVersion = "0.146.0"
)

type Model struct {
	ID                     string
	Description            string
	ContextWindow          int
	ToolCall               bool
	Reasoning              bool
	ReasoningEfforts       []string
	DefaultReasoningEffort string
	ServiceTiers           []string
	DefaultServiceTier     string
	InputModalities        []string
	OutputModalities       []string
	InputCost              float64
	OutputCost             float64
	Tags                   []string
	Source                 string
}

type Options struct {
	HTTPClient           *http.Client
	ModelsDevURL         string
	OpenGatewayURL       string
	OpenRouterURL        string
	OAuthResolver        providerio.TokenResolver
	CodexAccountResolver openai.CodexAccountResolver
	UserAgent            string
	ClientVersion        string
}

func DiscoverCatalog(ctx context.Context, provider providercatalog.Descriptor, profile config.ProviderProfile, options Options) ([]Model, error) {
	catalogModels, catalogErr := fetchCatalogModels(ctx, provider, options)
	// OpenRouter and OpenGateway publish public live model lists. Probe them even
	// without credentials so the picker stays current before a key is entered.
	canProbeProvider := modelDiscoveryAllowed(profile) && (!provider.RequiresAuth ||
		discoveryHasCredential(profile) ||
		options.OAuthResolver != nil ||
		publicLiveCatalogProvider(provider, profile))
	if canProbeProvider {
		liveModels, liveErr := Discover(ctx, profile, options)
		if liveErr == nil {
			if merged := mergeLiveModels(provider, liveModels, catalogModels); len(merged) > 0 {
				return merged, nil
			}
			// Live probe returned 200 but its model ids didn't match the catalog, so
			// the merge is empty. Fall through to the curated catalog below instead of
			// returning an empty list that collapses the picker to the bare built-in
			// set (and shows a misleading "no model ids" error) (M11).
		} else if len(catalogModels) == 0 {
			return nil, liveErr
		}
	}
	if len(catalogModels) > 0 {
		return catalogModels, nil
	}
	if catalogErr != nil {
		return nil, catalogErr
	}
	return nil, fmt.Errorf("no provider models discovered")
}

func publicLiveCatalogProvider(provider providercatalog.Descriptor, profile config.ProviderProfile) bool {
	return providermodelcatalog.PublicLiveCatalog(provider.ID) ||
		providermodelcatalog.PublicLiveCatalog(profile.CatalogID)
}

// discoveryHasCredential reports whether the profile carries a usable credential
// for an authenticated /models probe. A profile may authenticate via a raw
// auth-header value instead of APIKey, so treat either as present — consistent
// with config credential checks and zerocommands ProviderSnapshot.APIKeySet.
func discoveryHasCredential(profile config.ProviderProfile) bool {
	return strings.TrimSpace(profile.APIKey) != "" || strings.TrimSpace(profile.AuthHeaderValue) != ""
}

func Discover(ctx context.Context, profile config.ProviderProfile, options Options) ([]Model, error) {
	switch discoveryProviderKind(profile) {
	case config.ProviderKindOpenAI, config.ProviderKindOpenAICompatible:
		return discoverOpenAIModels(ctx, profile, options)
	case config.ProviderKindAnthropic, config.ProviderKindAnthropicCompat:
		return discoverAnthropicModels(ctx, profile, options)
	default:
		return nil, fmt.Errorf("provider %s does not expose model discovery", displayProviderName(profile))
	}
}

// DiscoverOllamaContextWindow asks a local Ollama daemon for a model's context
// length via its native /api/show endpoint. The generic /v1/models probe
// (parseModelsResponse) only ever returns id/description — OpenAI-compatible
// listings don't carry context-window metadata, and a custom/local model tag
// (e.g. a user's own Ollama pull, including a ":cloud"-tagged model proxied
// through the local daemon) has no curated-catalog entry to borrow one from
// either, so modelContextWindow has nothing to show for it without this. Only
// meaningful for the local Ollama provider (baseURL like
// http://localhost:11434/v1) — Ollama Cloud's hosted API is a different
// service and isn't assumed to expose the same endpoint.
func DiscoverOllamaContextWindow(ctx context.Context, baseURL string, model string, options Options) (int, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return 0, fmt.Errorf("model name is required")
	}
	endpoint, err := ollamaShowEndpoint(baseURL)
	if err != nil {
		return 0, err
	}
	payload, err := json.Marshal(struct {
		Model string `json:"model"`
	}{Model: model})
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")

	client := options.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return 0, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return 0, fmt.Errorf("ollama show endpoint returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	return parseOllamaShowContextWindow(body)
}

// ollamaShowEndpoint derives the native Ollama API root from the
// OpenAI-compatible base URL Zero stores for the provider (".../v1") and
// appends /api/show.
func ollamaShowEndpoint(baseURL string) (string, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return "", fmt.Errorf("provider base URL is required for ollama model discovery")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid provider base URL %q", baseURL)
	}
	path := strings.TrimRight(parsed.Path, "/")
	path = strings.TrimSuffix(path, "/v1")
	parsed.Path = strings.TrimRight(path, "/") + "/api/show"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

// parseOllamaShowContextWindow extracts the context length from an
// /api/show response. Ollama reports it under model_info with an
// architecture-prefixed key (e.g. "llama.context_length",
// "qwen2.context_length") — the prefix varies by model family, so this scans
// for any key ending in ".context_length" rather than hardcoding one.
func parseOllamaShowContextWindow(body []byte) (int, error) {
	// model_info mixes value types (strings like "general.architecture", numbers
	// like "*.context_length"), so decode values generically and only try to
	// read a number out of the one key we actually care about.
	var payload struct {
		ModelInfo map[string]any `json:"model_info"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, fmt.Errorf("decode ollama show response: %w", err)
	}
	for key, value := range payload.ModelInfo {
		if !strings.HasSuffix(key, ".context_length") {
			continue
		}
		if window, ok := value.(float64); ok && window > 0 {
			return int(window), nil
		}
	}
	return 0, fmt.Errorf("ollama show response did not report a context length")
}

func discoverOpenAIModels(ctx context.Context, profile config.ProviderProfile, options Options) ([]Model, error) {
	endpoint, err := modelsEndpoint(profile.BaseURL)
	if err != nil {
		return nil, err
	}
	var configure func(*http.Request)
	if providercatalog.NormalizeID(profile.CatalogID) == "chatgpt" {
		parsed, parseErr := url.Parse(endpoint)
		if parseErr != nil {
			return nil, fmt.Errorf("parse ChatGPT models endpoint: %w", parseErr)
		}
		clientVersion := strings.TrimSpace(options.ClientVersion)
		if clientVersion == "" {
			clientVersion = chatGPTModelsProtocolVersion
		}
		query := parsed.Query()
		query.Set("client_version", clientVersion)
		parsed.RawQuery = query.Encode()
		endpoint = parsed.String()
		configure = func(request *http.Request) {
			account := ""
			if options.CodexAccountResolver != nil {
				if resolved, ok, resolveErr := options.CodexAccountResolver(request.Context()); resolveErr == nil && ok {
					account = resolved
				}
			}
			openai.ApplyCodexHeaders(request, account, options.UserAgent)
		}
	}
	return fetchProviderModels(ctx, endpoint, profile, options, providerio.AuthHeaders{
		APIKey:            profile.APIKey,
		DefaultAuthHeader: "Authorization",
		DefaultAuthScheme: "Bearer",
		AuthHeader:        profile.AuthHeader,
		AuthScheme:        profile.AuthScheme,
		AuthHeaderValue:   profile.AuthHeaderValue,
		CustomHeaders:     providerio.CopyHeaders(profile.CustomHeaders),
	}, configure)
}

func discoverAnthropicModels(ctx context.Context, profile config.ProviderProfile, options Options) ([]Model, error) {
	endpoint, err := anthropicModelsEndpoint(profile.BaseURL)
	if err != nil {
		return nil, err
	}
	return fetchProviderModels(ctx, endpoint, profile, options, providerio.AuthHeaders{
		APIKey:            profile.APIKey,
		DefaultAuthHeader: "x-api-key",
		AuthHeader:        profile.AuthHeader,
		AuthScheme:        profile.AuthScheme,
		AuthHeaderValue:   profile.AuthHeaderValue,
		CustomHeaders:     profile.CustomHeaders,
	}, func(request *http.Request) {
		request.Header.Set("anthropic-version", anthropicVersion)
	})
}

func fetchProviderModels(ctx context.Context, endpoint string, profile config.ProviderProfile, options Options, auth providerio.AuthHeaders, configure func(*http.Request)) ([]Model, error) {
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	response, err := providerio.SendWithAuthRetry(ctx, client, http.MethodGet, endpoint, nil, auth, options.OAuthResolver, func(request *http.Request) {
		request.Header.Set("Accept", "application/json")
		if configure != nil {
			configure(request)
		}
	}, 1)
	if err != nil {
		return nil, redactDiscoveryError(err, profile)
	}
	defer response.Body.Close()

	// OpenRouter's full catalog is ~0.5MB today; keep headroom for growth.
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, redactDiscoveryError(err, profile)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, redactDiscoveryError(fmt.Errorf("models endpoint returned %s: %s", response.Status, strings.TrimSpace(string(body))), profile)
	}

	models, err := parseModelsResponse(body)
	if err != nil {
		return nil, redactDiscoveryError(err, profile)
	}
	return models, nil
}

func modelDiscoveryAllowed(profile config.ProviderProfile) bool {
	switch discoveryProviderKind(profile) {
	case config.ProviderKindOpenAI, config.ProviderKindOpenAICompatible, config.ProviderKindAnthropic, config.ProviderKindAnthropicCompat:
		return true
	default:
		return false
	}
}

func discoveryProviderKind(profile config.ProviderProfile) config.ProviderKind {
	kind := config.ProviderKind(strings.TrimSpace(strings.ToLower(string(profile.ProviderKind))))
	if kind == "" {
		kind = config.ProviderKind(strings.TrimSpace(strings.ToLower(profile.Provider)))
	}
	return kind
}

func modelsEndpoint(baseURL string) (string, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return "", fmt.Errorf("provider base URL is required for model discovery")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid provider base URL %q", baseURL)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/models"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func anthropicModelsEndpoint(baseURL string) (string, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return "", fmt.Errorf("provider base URL is required for model discovery")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid provider base URL %q", baseURL)
	}
	path := strings.TrimRight(parsed.Path, "/")
	if strings.HasSuffix(strings.ToLower(path), "/v1") {
		parsed.Path = path + "/models"
	} else {
		parsed.Path = path + "/v1/models"
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

type modelsResponseItem struct {
	ID                 string   `json:"id"`
	Slug               string   `json:"slug"`
	DisplayName        string   `json:"display_name"`
	Name               string   `json:"name"`
	Description        string   `json:"description"`
	Visibility         string   `json:"visibility"`
	ContextWindow      int      `json:"context_window"`
	ContextWindowAlt   int      `json:"contextWindow"`
	ContextLength      int      `json:"context_length"`
	MaxContextLength   int      `json:"max_context_length"`
	Free               bool     `json:"free"`
	IsFree             bool     `json:"is_free"`
	ToolCall           bool     `json:"tool_call"`
	ToolCallCamel      bool     `json:"toolCall"`
	Tools              bool     `json:"tools"`
	Reasoning          bool     `json:"reasoning"`
	ReasoningEfforts   []string `json:"reasoning_efforts"`
	DefaultReasoning   string   `json:"default_reasoning_level"`
	SupportedReasoning []struct {
		Effort string `json:"effort"`
	} `json:"supported_reasoning_levels"`
	ServiceTiers []struct {
		ID string `json:"id"`
	} `json:"service_tiers"`
	AdditionalSpeedTiers []string `json:"additional_speed_tiers"`
	DefaultServiceTier   string   `json:"default_service_tier"`
	SupportedParameters  []string `json:"supported_parameters"`
}

type modelsResponse struct {
	Data   []modelsResponseItem `json:"data"`
	Models []modelsResponseItem `json:"models"`
}

func parseModelsResponse(body []byte) ([]Model, error) {
	var payload modelsResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode models response: %w", err)
	}
	seen := map[string]bool{}
	items := payload.Data
	if len(items) == 0 {
		items = payload.Models
	}
	models := make([]Model, 0, len(items))
	for _, item := range items {
		id := firstNonEmptyDiscovery(item.ID, item.Slug)
		if id == "" || seen[id] {
			continue
		}
		// The ChatGPT catalog includes hidden/internal entries alongside picker
		// models. Generic OpenAI-compatible responses omit visibility entirely.
		if visibility := strings.ToLower(strings.TrimSpace(item.Visibility)); visibility != "" && visibility != "list" {
			continue
		}
		seen[id] = true
		description := firstNonEmptyDiscovery(
			item.DisplayName,
			item.Name,
			item.Description,
		)
		if (item.Free || item.IsFree || strings.HasSuffix(strings.ToLower(id), ":free")) &&
			description != "" &&
			!strings.Contains(strings.ToLower(description), "free") {
			description = description + " (free)"
		}
		contextWindow := firstPositiveDiscovery(
			item.ContextWindow,
			item.ContextWindowAlt,
			item.ContextLength,
			item.MaxContextLength,
		)
		toolCall := item.ToolCall || item.ToolCallCamel || item.Tools ||
			discoveryContainsFold(item.SupportedParameters, "tools")
		reasoning := item.Reasoning ||
			discoveryContainsFold(item.SupportedParameters, "reasoning") ||
			discoveryContainsFold(item.SupportedParameters, "reasoning_effort") ||
			discoveryContainsFold(item.SupportedParameters, "include_reasoning")
		efforts := reasoningEfforts(item)
		serviceTiers := modelServiceTiers(item)
		models = append(models, Model{
			ID:                     id,
			Description:            description,
			ContextWindow:          contextWindow,
			ToolCall:               toolCall,
			Reasoning:              reasoning || len(efforts) > 0,
			ReasoningEfforts:       efforts,
			DefaultReasoningEffort: strings.TrimSpace(item.DefaultReasoning),
			ServiceTiers:           serviceTiers,
			DefaultServiceTier:     normalizeServiceTier(item.DefaultServiceTier),
			Source:                 "live",
		})
	}
	sort.SliceStable(models, func(i, j int) bool {
		return models[i].ID < models[j].ID
	})
	if len(models) == 0 {
		return nil, fmt.Errorf("models endpoint returned no model ids")
	}
	return models, nil
}

func modelServiceTiers(item modelsResponseItem) []string {
	values := append([]string{}, item.AdditionalSpeedTiers...)
	for _, tier := range item.ServiceTiers {
		values = append(values, tier.ID)
	}
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = normalizeServiceTier(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func normalizeServiceTier(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "fast" {
		return "priority"
	}
	return value
}

func reasoningEfforts(item modelsResponseItem) []string {
	efforts := append([]string{}, item.ReasoningEfforts...)
	for _, level := range item.SupportedReasoning {
		efforts = append(efforts, level.Effort)
	}
	seen := map[string]bool{}
	result := make([]string, 0, len(efforts))
	for _, effort := range efforts {
		effort = strings.ToLower(strings.TrimSpace(effort))
		if effort == "" || seen[effort] {
			continue
		}
		seen[effort] = true
		result = append(result, effort)
	}
	return result
}

func fetchCatalogModels(ctx context.Context, provider providercatalog.Descriptor, options Options) ([]Model, error) {
	models, err := providermodelcatalog.FetchRemote(ctx, provider, providermodelcatalog.FetchOptions{
		HTTPClient:     options.HTTPClient,
		ModelsDevURL:   options.ModelsDevURL,
		OpenGatewayURL: options.OpenGatewayURL,
		OpenRouterURL:  options.OpenRouterURL,
	})
	if err != nil {
		return nil, err
	}
	return modelsFromCatalog(models), nil
}

func modelsFromCatalog(models []providermodelcatalog.Model) []Model {
	result := make([]Model, 0, len(models))
	for _, model := range models {
		result = append(result, Model{
			ID:                     model.ID,
			Description:            model.Description,
			ContextWindow:          model.ContextWindow,
			ToolCall:               model.ToolCall,
			Reasoning:              model.Reasoning,
			ReasoningEfforts:       append([]string{}, model.ReasoningEfforts...),
			DefaultReasoningEffort: model.DefaultReasoningEffort,
			ServiceTiers:           append([]string{}, model.ServiceTiers...),
			DefaultServiceTier:     model.DefaultServiceTier,
			InputModalities:        append([]string{}, model.InputModalities...),
			OutputModalities:       append([]string{}, model.OutputModalities...),
			InputCost:              model.InputCost,
			OutputCost:             model.OutputCost,
			Tags:                   append([]string{}, model.Tags...),
			Source:                 model.Source,
		})
	}
	return result
}

func mergeLiveModels(provider providercatalog.Descriptor, liveModels []Model, catalogModels []Model) []Model {
	byID := map[string]Model{}
	for _, model := range catalogModels {
		byID[model.ID] = model
	}
	hasCatalog := len(byID) > 0
	// Aggregators publish the live list as the source of truth. Keep live-only
	// ids even when a remote catalog also loaded, instead of intersecting.
	preferLive := provider.Local ||
		providermodelcatalog.PublicLiveCatalog(provider.ID) ||
		providercatalog.NormalizeID(provider.ID) == "chatgpt"
	result := make([]Model, 0, len(liveModels))
	for _, live := range liveModels {
		if catalog, ok := byID[live.ID]; ok {
			if preferLive {
				if providermodelcatalog.IsKnownNonCodingModelID(catalog.ID) {
					continue
				}
			} else if !providermodelcatalog.IsCodingModel(catalogModelFromDiscovery(catalog)) {
				continue
			}
			// Prefer catalog metadata (tools, cost) but fill gaps from live.
			if catalog.ContextWindow == 0 && live.ContextWindow > 0 {
				catalog.ContextWindow = live.ContextWindow
			}
			if strings.TrimSpace(catalog.Description) == "" && strings.TrimSpace(live.Description) != "" {
				catalog.Description = live.Description
			}
			if len(live.ReasoningEfforts) > 0 {
				catalog.ReasoningEfforts = append([]string{}, live.ReasoningEfforts...)
			}
			if live.DefaultReasoningEffort != "" {
				catalog.DefaultReasoningEffort = live.DefaultReasoningEffort
			}
			if live.Reasoning || len(live.ReasoningEfforts) > 0 || live.DefaultReasoningEffort != "" {
				catalog.Reasoning = true
			}
			if len(live.ServiceTiers) > 0 {
				catalog.ServiceTiers = append([]string{}, live.ServiceTiers...)
			}
			if live.DefaultServiceTier != "" {
				catalog.DefaultServiceTier = live.DefaultServiceTier
			}
			catalog.Source = firstDiscoverySource(catalog.Source, "live")
			result = append(result, catalog)
			continue
		}
		if hasCatalog && !preferLive {
			continue
		}
		if preferLive {
			if providermodelcatalog.IsKnownNonCodingModelID(live.ID) {
				continue
			}
			// OpenRouter keeps coding-capable live-only models (tools/reasoning
			// or coding-like ids). OpenGateway trusts the gateway list.
			if providercatalog.NormalizeID(provider.ID) == "openrouter" &&
				!providermodelcatalog.IsCodingModel(catalogModelFromDiscovery(live)) {
				continue
			}
		} else if !liveModelAllowedWithoutCatalog(provider, live.ID) {
			continue
		}
		live.Source = firstDiscoverySource(live.Source, "live")
		result = append(result, live)
	}
	return result
}

func discoveryContainsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), want) {
			return true
		}
	}
	return false
}

func firstNonEmptyDiscovery(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func firstPositiveDiscovery(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func liveModelAllowedWithoutCatalog(provider providercatalog.Descriptor, id string) bool {
	if !providermodelcatalog.ModelIDAllowedForProvider(provider.ID, id) {
		return false
	}
	if providermodelcatalog.IsKnownNonCodingModelID(id) {
		return false
	}
	if provider.Local || strings.HasPrefix(provider.ID, "custom-") {
		return true
	}
	return providermodelcatalog.LooksLikeCodingModelID(id)
}

func catalogModelFromDiscovery(model Model) providermodelcatalog.Model {
	return providermodelcatalog.Model{
		ID:                     model.ID,
		Description:            model.Description,
		ContextWindow:          model.ContextWindow,
		ToolCall:               model.ToolCall,
		Reasoning:              model.Reasoning,
		ReasoningEfforts:       append([]string{}, model.ReasoningEfforts...),
		DefaultReasoningEffort: model.DefaultReasoningEffort,
		ServiceTiers:           append([]string{}, model.ServiceTiers...),
		DefaultServiceTier:     model.DefaultServiceTier,
		InputModalities:        append([]string{}, model.InputModalities...),
		OutputModalities:       append([]string{}, model.OutputModalities...),
		InputCost:              model.InputCost,
		OutputCost:             model.OutputCost,
		Tags:                   append([]string{}, model.Tags...),
		Source:                 model.Source,
	}
}

func firstDiscoverySource(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func redactDiscoveryError(err error, profile config.ProviderProfile) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s", redaction.RedactString(err.Error(), redaction.Options{ExtraSecretValues: []string{
		profile.APIKey,
		profile.AuthHeaderValue,
	}}))
}

func displayProviderName(profile config.ProviderProfile) string {
	for _, value := range []string{profile.Name, profile.CatalogID, string(profile.ProviderKind), profile.Provider} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return "provider"
}
