package providermodelcatalog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/providercatalog"
)

const (
	DefaultModelsDevURL   = "https://models.dev/api.json"
	defaultOpenGatewayURL = "https://opengateway.gitlawb.com/v1/models"
	defaultOpenRouterURL  = "https://openrouter.ai/api/v1/models"
	modelsDevSource       = "models.dev"
	openGatewaySource     = "opengateway"
	openRouterSource      = "openrouter"
)

type FetchOptions struct {
	HTTPClient     *http.Client
	ModelsDevURL   string
	OpenGatewayURL string
	OpenRouterURL  string
}

func FetchRemote(ctx context.Context, provider providercatalog.Descriptor, options FetchOptions) ([]Model, error) {
	switch providercatalog.NormalizeID(provider.ID) {
	case "gitlawb-opengateway":
		return FetchOpenGateway(ctx, defaultedOpenGatewayURL(provider, options.OpenGatewayURL), options)
	case "openrouter":
		return FetchOpenRouter(ctx, defaultedOpenRouterURL(provider, options.OpenRouterURL), options)
	}

	providerID := ModelsDevProviderID(provider)
	if providerID == "" {
		return nil, fmt.Errorf("provider %s does not have a models.dev catalog mapping", provider.ID)
	}
	return FetchModelsDev(ctx, providerID, options)
}

func FetchModelsDev(ctx context.Context, providerID string, options FetchOptions) ([]Model, error) {
	endpoint := strings.TrimSpace(options.ModelsDevURL)
	if endpoint == "" {
		endpoint = DefaultModelsDevURL
	}
	body, err := fetchJSON(ctx, endpoint, options.HTTPClient)
	if err != nil {
		return nil, err
	}
	return ParseModelsDevProvider(body, providerID)
}

func FetchOpenGateway(ctx context.Context, endpoint string, options FetchOptions) ([]Model, error) {
	body, err := fetchJSON(ctx, endpoint, options.HTTPClient)
	if err != nil {
		return nil, err
	}
	return ParseOpenGatewayCatalog(body)
}

// FetchOpenRouter loads OpenRouter's public live model list (GET /api/v1/models).
// Auth is optional for listing; callers may still attach a key for account-scoped
// probes via the live discovery path. When the live host is unreachable or
// returns an unparseable body, fall back to models.dev so the picker still
// degrades instead of failing entirely (both catalog and live probe hit
// openrouter.ai otherwise).
func FetchOpenRouter(ctx context.Context, endpoint string, options FetchOptions) ([]Model, error) {
	body, err := fetchJSON(ctx, endpoint, options.HTTPClient)
	if err == nil {
		models, parseErr := ParseOpenRouterCatalog(body)
		if parseErr == nil {
			return models, nil
		}
		err = parseErr
	}
	fallback, fallbackErr := FetchModelsDev(ctx, "openrouter", options)
	if fallbackErr == nil {
		return fallback, nil
	}
	return nil, fmt.Errorf("%w (models.dev fallback: %v)", err, fallbackErr)
}

func ParseModelsDevProvider(body []byte, providerID string) ([]Model, error) {
	var payload map[string]struct {
		Models map[string]remoteModel `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode models.dev catalog: %w", err)
	}
	providerID = strings.TrimSpace(providerID)
	provider, ok := payload[providerID]
	if !ok {
		return nil, fmt.Errorf("models.dev provider %q not found", providerID)
	}
	models := make([]Model, 0, len(provider.Models))
	for key, item := range provider.Models {
		model := item.toModel(key, modelsDevSource)
		if model.ID == "" || !IsCodingModel(model) {
			continue
		}
		models = append(models, model)
	}
	sortModels(models)
	if len(models) == 0 {
		return nil, fmt.Errorf("models.dev provider %q returned no models", providerID)
	}
	return models, nil
}

// ParseOpenGatewayCatalog parses OpenGateway's live GET /v1/models payload (or the
// older {models:[...]} shape). The gateway already curates what it exposes, so
// every non-empty id is accepted rather than applying the coding-model heuristic.
func ParseOpenGatewayCatalog(body []byte) ([]Model, error) {
	items, err := parseOpenAIStyleModelList(body)
	if err != nil {
		return nil, fmt.Errorf("decode OpenGateway catalog: %w", err)
	}
	models := make([]Model, 0, len(items))
	for _, item := range items {
		model := item.toModel("", openGatewaySource)
		if model.ID == "" {
			continue
		}
		// Drop only clearly non-coding ids if a gateway ever lists them.
		if IsKnownNonCodingModelID(model.ID) {
			continue
		}
		model = annotateFreeModel(model, item)
		models = append(models, model)
	}
	sortModels(models)
	if len(models) == 0 {
		return nil, fmt.Errorf("OpenGateway catalog returned no models")
	}
	return models, nil
}

// ParseOpenRouterCatalog parses OpenRouter's public GET /api/v1/models list and
// keeps coding-capable entries (tools, reasoning, or coding-like ids).
func ParseOpenRouterCatalog(body []byte) ([]Model, error) {
	items, err := parseOpenAIStyleModelList(body)
	if err != nil {
		return nil, fmt.Errorf("decode OpenRouter catalog: %w", err)
	}
	models := make([]Model, 0, len(items))
	for _, item := range items {
		model := item.toModel("", openRouterSource)
		if model.ID == "" || !IsCodingModel(model) {
			continue
		}
		model = annotateFreeModel(model, item)
		models = append(models, model)
	}
	sortModels(models)
	if len(models) == 0 {
		return nil, fmt.Errorf("OpenRouter catalog returned no models")
	}
	return models, nil
}

func parseOpenAIStyleModelList(body []byte) ([]remoteModel, error) {
	var payload struct {
		Models []remoteModel `json:"models"`
		Data   []remoteModel `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if len(payload.Models) > 0 {
		return payload.Models, nil
	}
	return payload.Data, nil
}

func ModelsDevProviderID(provider providercatalog.Descriptor) string {
	switch strings.TrimSpace(provider.ID) {
	case "chatgpt":
		// ChatGPT authenticates against the Codex backend, while its public
		// model metadata is published under the OpenAI provider on models.dev.
		return "openai"
	case "dashscope":
		return "alibaba"
	case "github":
		return "github-models"
	case "moonshot":
		return "moonshotai"
	case "nvidia-nim":
		return "nvidia"
	case "xiaomi-mimo":
		return "xiaomi"
	case "zai-cn":
		return "zai"
	case "minimaxi-cn":
		return "minimax"
	case "fireworks":
		return "fireworks-ai"
	default:
		return strings.TrimSpace(provider.ID)
	}
}

// PublicLiveCatalog reports whether the provider publishes a public live model
// list that Zero should prefer over third-party catalogs (models.dev) and that
// can be fetched without credentials.
func PublicLiveCatalog(providerID string) bool {
	switch providercatalog.NormalizeID(providerID) {
	case "openrouter", "gitlawb-opengateway":
		return true
	default:
		return false
	}
}

type remoteModel struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Description        string `json:"description"`
	ContextWindow      int    `json:"context_window"`
	ContextWindowCamel int    `json:"contextWindow"`
	ContextLength      int    `json:"context_length"`
	MaxContextLength   int    `json:"max_context_length"`
	ToolCall           bool   `json:"tool_call"`
	ToolCallCamel      bool   `json:"toolCall"`
	Tools              bool   `json:"tools"`
	// ReasoningRaw accepts both a boolean (models.dev) and OpenRouter's object form.
	ReasoningRaw        json.RawMessage    `json:"reasoning"`
	ReasoningEfforts    []string           `json:"reasoning_efforts"`
	DefaultReasoning    string             `json:"default_reasoning_effort"`
	ServiceTiers        []string           `json:"service_tiers"`
	AdditionalSpeed     []string           `json:"additional_speed_tiers"`
	DefaultServiceTier  string             `json:"default_service_tier"`
	Free                bool               `json:"free"`
	IsFree              bool               `json:"is_free"`
	InputCost           float64            `json:"input_cost"`
	OutputCost          float64            `json:"output_cost"`
	Tags                []string           `json:"tags"`
	SupportedParameters []string           `json:"supported_parameters"`
	Limit               remoteLimit        `json:"limit"`
	Cost                remoteCost         `json:"cost"`
	Pricing             remotePricing      `json:"pricing"`
	Modalities          remoteModalities   `json:"modalities"`
	Architecture        remoteArchitecture `json:"architecture"`
}

type remotePricing struct {
	Prompt     string `json:"prompt"`
	Completion string `json:"completion"`
}

type remoteLimit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

type remoteCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

type remoteModalities struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

// OpenRouter nests modalities under architecture; OpenGateway uses flat fields.
type remoteArchitecture struct {
	InputModalities  []string `json:"input_modalities"`
	OutputModalities []string `json:"output_modalities"`
}

type remoteReasoningInfo struct {
	Mandatory      bool     `json:"mandatory"`
	DefaultEnabled bool     `json:"default_enabled"`
	Supported      []string `json:"supported_efforts"`
}

func (model remoteModel) toModel(key string, source string) Model {
	id := firstNonEmpty(model.ID, key)
	contextWindow := firstPositive(
		model.ContextWindow,
		model.ContextWindowCamel,
		model.ContextLength,
		model.MaxContextLength,
		model.Limit.Context,
	)
	inputCost := firstPositiveFloat(
		model.InputCost,
		parsePricingString(model.Pricing.Prompt),
		model.Cost.Input,
	)
	outputCost := firstPositiveFloat(
		model.OutputCost,
		parsePricingString(model.Pricing.Completion),
		model.Cost.Output,
	)
	inputModalities := cleanStrings(model.Modalities.Input)
	if len(inputModalities) == 0 {
		inputModalities = cleanStrings(model.Architecture.InputModalities)
	}
	outputModalities := cleanStrings(model.Modalities.Output)
	if len(outputModalities) == 0 {
		outputModalities = cleanStrings(model.Architecture.OutputModalities)
	}
	toolCall := model.ToolCall || model.ToolCallCamel || model.Tools || containsFold(model.SupportedParameters, "tools")
	reasoning := model.supportsReasoning() ||
		containsFold(model.SupportedParameters, "reasoning") ||
		containsFold(model.SupportedParameters, "reasoning_effort") ||
		containsFold(model.SupportedParameters, "include_reasoning")
	reasoningEfforts := model.supportedReasoningEfforts()
	serviceTiers := normalizedServiceTiers(model.ServiceTiers, model.AdditionalSpeed)

	// Prefer the short display name for aggregator catalogs (OpenRouter's
	// description field is multi-sentence marketing copy). models.dev and
	// OpenGateway still prefer an explicit description when present.
	description := firstNonEmpty(model.Description, model.Name)
	if source == openRouterSource {
		description = firstNonEmpty(model.Name, model.Description)
	}

	return Model{
		ID:                     strings.TrimSpace(id),
		Description:            description,
		ContextWindow:          contextWindow,
		ToolCall:               toolCall,
		Reasoning:              reasoning,
		ReasoningEfforts:       reasoningEfforts,
		DefaultReasoningEffort: strings.TrimSpace(model.DefaultReasoning),
		ServiceTiers:           serviceTiers,
		DefaultServiceTier:     normalizeServiceTier(model.DefaultServiceTier),
		InputModalities:        inputModalities,
		OutputModalities:       outputModalities,
		InputCost:              inputCost,
		OutputCost:             outputCost,
		Tags:                   cleanStrings(model.Tags),
		Source:                 source,
	}
}

func normalizedServiceTiers(tiers []string, legacy []string) []string {
	values := append(append([]string{}, tiers...), legacy...)
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

func (model remoteModel) supportedReasoningEfforts() []string {
	efforts := append([]string{}, model.ReasoningEfforts...)
	var info remoteReasoningInfo
	if err := json.Unmarshal(bytes.TrimSpace(model.ReasoningRaw), &info); err == nil {
		efforts = append(efforts, info.Supported...)
	}
	return cleanStrings(efforts)
}

func (model remoteModel) supportsReasoning() bool {
	raw := bytes.TrimSpace(model.ReasoningRaw)
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var flag bool
	if err := json.Unmarshal(raw, &flag); err == nil {
		return flag
	}
	var info remoteReasoningInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return false
	}
	return info.Mandatory || info.DefaultEnabled || len(info.Supported) > 0
}

func annotateFreeModel(model Model, raw remoteModel) Model {
	if !raw.Free && !raw.IsFree && !strings.HasSuffix(strings.ToLower(model.ID), ":free") {
		return model
	}
	if !containsFold(model.Tags, "free") {
		model.Tags = append(append([]string{}, model.Tags...), "free")
	}
	if model.Description != "" && !strings.Contains(strings.ToLower(model.Description), "free") {
		model.Description = model.Description + " (free)"
	}
	return model
}

func fetchJSON(ctx context.Context, endpoint string, client *http.Client) ([]byte, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("model catalog URL is required")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "zero-cli")
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	// OpenRouter's full catalog is ~0.5MB today; keep headroom for growth.
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("model catalog returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func defaultedOpenGatewayURL(provider providercatalog.Descriptor, override string) string {
	if override = strings.TrimSpace(override); override != "" {
		return override
	}
	return defaultedOpenAIModelsURL(provider.DefaultBaseURL, defaultOpenGatewayURL)
}

func defaultedOpenRouterURL(provider providercatalog.Descriptor, override string) string {
	if override = strings.TrimSpace(override); override != "" {
		return override
	}
	return defaultedOpenAIModelsURL(provider.DefaultBaseURL, defaultOpenRouterURL)
}

// defaultedOpenAIModelsURL appends /models to an OpenAI-compatible base URL
// (e.g. https://host/v1 → https://host/v1/models).
func defaultedOpenAIModelsURL(baseURL string, fallback string) string {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fallback
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/models"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func sortModels(models []Model) {
	sort.SliceStable(models, func(i, j int) bool {
		left := modelSortLabel(models[i])
		right := modelSortLabel(models[j])
		if left == right {
			return models[i].ID < models[j].ID
		}
		return left < right
	})
}

func modelSortLabel(model Model) string {
	if label := strings.ToLower(strings.TrimSpace(model.Description)); label != "" {
		return label
	}
	return strings.ToLower(strings.TrimSpace(model.ID))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func firstPositiveFloat(values ...float64) float64 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

// parsePricingString converts OpenRouter/OpenGateway pricing.prompt and
// pricing.completion values (USD per token) into the Model.InputCost/OutputCost
// unit used everywhere else (USD per million tokens, matching models.dev
// cost.input / cost.output). Values <= 0, including OpenRouter's "-1"
// variable/BYOK marker, and non-finite values (NaN/Inf), map to 0 so the
// picker shows no price rather than a bogus number.
func parsePricingString(s string) float64 {
	val, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || val <= 0 || math.IsNaN(val) || math.IsInf(val, 0) {
		return 0
	}
	return val * 1e6
}

func cleanStrings(values []string) []string {
	result := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}
