package modelregistry

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

type ProviderKind string

const (
	ProviderOpenAI           ProviderKind = "openai"
	ProviderAnthropic        ProviderKind = "anthropic"
	ProviderGoogle           ProviderKind = "google"
	ProviderOpenAICompatible ProviderKind = "openai-compatible"
)

type ReasoningEffort string

const (
	ReasoningEffortNone    ReasoningEffort = "none"
	ReasoningEffortMinimal ReasoningEffort = "minimal"
	ReasoningEffortLow     ReasoningEffort = "low"
	ReasoningEffortMedium  ReasoningEffort = "medium"
	ReasoningEffortHigh    ReasoningEffort = "high"
	ReasoningEffortXHigh   ReasoningEffort = "xhigh"
	// ReasoningEffortMax is the top tier some providers expose above xhigh (e.g.
	// newer Anthropic models). Defined so a capability catalog that lists "max"
	// has a constant to map to; no curated model lists it yet.
	ReasoningEffortMax ReasoningEffort = "max"
	// ReasoningEffortUltra is advertised by models whose provider exposes an
	// additional tier above max.
	ReasoningEffortUltra ReasoningEffort = "ultra"
)

type ModelCapability string

const (
	ModelCapabilityChat         ModelCapability = "chat"
	ModelCapabilityStreaming    ModelCapability = "streaming"
	ModelCapabilityToolCalling  ModelCapability = "tool-calling"
	ModelCapabilityVision       ModelCapability = "vision"
	ModelCapabilityJSONMode     ModelCapability = "json-mode"
	ModelCapabilityReasoning    ModelCapability = "reasoning"
	ModelCapabilitySystemPrompt ModelCapability = "system-prompt"
	ModelCapabilityPromptCache  ModelCapability = "prompt-cache"
	ModelCapabilityLongContext  ModelCapability = "long-context"
)

type ModelCapabilities []ModelCapability

type ModelStatus string

const (
	ModelStatusActive     ModelStatus = "active"
	ModelStatusPreview    ModelStatus = "preview"
	ModelStatusDeprecated ModelStatus = "deprecated"
)

type ContextLimits struct {
	ContextWindow   int
	MaxOutputTokens int
}

type ModelCost struct {
	Currency              string
	Unit                  string
	InputPerMillion       float64
	OutputPerMillion      float64
	CachedInputPerMillion float64
	// CacheWritePerMillion is the cache-creation (cache-write) rate, billed at a
	// premium over input by providers that support it (Anthropic ~1.25x input).
	// Zero means "not priced separately" — cache-write tokens fall back to the
	// input rate, preserving prior behavior for models without the rate.
	CacheWritePerMillion float64
	Tiers                []ModelCostTier
	Source               string
	SourceLastVerified   string
	Notes                []string
}

type ModelCostTier struct {
	UpToInputTokens       int
	InputPerMillion       float64
	OutputPerMillion      float64
	CachedInputPerMillion float64
	CacheWritePerMillion  float64
	Note                  string
}

type ModelEntry struct {
	ID               string
	DisplayName      string
	APIModel         string
	Provider         ProviderKind
	APIProviders     []ProviderKind
	ContextLimits    ContextLimits
	ReasoningEfforts []ReasoningEffort
	// DefaultReasoningEffort is the effort used when the caller does not specify
	// one (must be a member of ReasoningEfforts, or empty for non-reasoning models).
	DefaultReasoningEffort ReasoningEffort
	Capabilities           ModelCapabilities
	Cost                   ModelCost
	Status                 ModelStatus
	Aliases                []string
	// MatchPatterns are regular expressions that resolve fuzzy user input to this
	// model (e.g. `sonnet[^a-z0-9]*4[.\s]?5` -> the canonical id).
	MatchPatterns []string
	// Deprecation, when set, redirects this model to a replacement.
	Deprecation *DeprecationRule
	// UpgradeTargetID, when set, names the stronger model this one escalates to
	// during mid-run escalation. Empty means no escalation target (e.g. top-tier
	// models). Resolved and availability-checked by Registry.UpgradeTarget.
	UpgradeTargetID string
	Description     string
}

// DeprecationRule describes how a deprecated model is phased out and what to use
// instead. FallbackID is required; the date/warning fields are advisory.
type DeprecationRule struct {
	FallbackID string // model id to redirect to
	SoftDate   string // ISO-8601: warn but keep working from this date
	HardDate   string // ISO-8601: treat as fully retired from this date
	WarningMsg string // user-facing notice
}

// Clone returns a deep copy of the rule (or nil).
func (rule *DeprecationRule) Clone() *DeprecationRule {
	if rule == nil {
		return nil
	}
	copied := *rule
	return &copied
}

func (model ModelEntry) Validate() error {
	if strings.TrimSpace(model.ID) == "" {
		return fmt.Errorf("model id is required")
	}
	if strings.TrimSpace(model.DisplayName) == "" {
		return fmt.Errorf("model display name is required")
	}
	if strings.TrimSpace(model.APIModel) == "" {
		return fmt.Errorf("api model is required")
	}
	if !ValidPrimaryProviderKind(model.Provider) {
		return fmt.Errorf("unknown primary provider %q", model.Provider)
	}
	if model.ContextLimits.ContextWindow <= 0 {
		return fmt.Errorf("context window must be positive")
	}
	if model.ContextLimits.MaxOutputTokens <= 0 {
		return fmt.Errorf("max output tokens must be positive")
	}
	if model.ContextLimits.MaxOutputTokens > model.ContextLimits.ContextWindow {
		return fmt.Errorf("max output tokens cannot exceed context window")
	}
	if len(model.Capabilities) == 0 {
		return fmt.Errorf("at least one model capability is required")
	}
	for _, capability := range model.Capabilities {
		if !ValidModelCapability(capability) {
			return fmt.Errorf("unknown model capability %q", capability)
		}
	}
	for _, effort := range model.ReasoningEfforts {
		if !ValidReasoningEffort(effort) {
			return fmt.Errorf("unknown reasoning effort %q", effort)
		}
	}
	if model.DefaultReasoningEffort != "" {
		if !ValidReasoningEffort(model.DefaultReasoningEffort) {
			return fmt.Errorf("unknown default reasoning effort %q", model.DefaultReasoningEffort)
		}
		// The default must be one the model actually supports, else
		// EffectiveReasoningEffort would hand back an unsupported effort.
		supported := false
		for _, effort := range model.ReasoningEfforts {
			if effort == model.DefaultReasoningEffort {
				supported = true
				break
			}
		}
		if !supported {
			return fmt.Errorf("default reasoning effort %q is not in the model's supported efforts", model.DefaultReasoningEffort)
		}
	}
	if model.Deprecation != nil && strings.TrimSpace(model.Deprecation.FallbackID) == "" {
		return fmt.Errorf("deprecation rule requires a fallback id")
	}
	if err := model.Cost.Validate(); err != nil {
		return err
	}
	if !ValidModelStatus(model.Status) {
		return fmt.Errorf("unknown model status %q", model.Status)
	}
	if len(model.Aliases) == 0 {
		return fmt.Errorf("at least one model alias is required")
	}
	for _, alias := range model.Aliases {
		if strings.TrimSpace(alias) == "" {
			return fmt.Errorf("model aliases cannot be blank")
		}
	}
	for _, provider := range model.APIProviders {
		if !ValidRuntimeProviderKind(provider) {
			return fmt.Errorf("unknown api provider %q", provider)
		}
	}
	if len(model.APIProviders) > 0 && !model.AllowsProvider(model.Provider) {
		return fmt.Errorf("primary provider %q not allowed by api providers", model.Provider)
	}
	return nil
}

func (cost ModelCost) Validate() error {
	if cost.Currency != "USD" {
		return fmt.Errorf("model cost currency must be USD")
	}
	if cost.Unit != "per_1m_tokens" {
		return fmt.Errorf("model cost unit must be per_1m_tokens")
	}
	if cost.InputPerMillion < 0 || cost.OutputPerMillion < 0 || cost.CachedInputPerMillion < 0 {
		return fmt.Errorf("model cost values must be non-negative")
	}
	if len(cost.Tiers) == 0 {
		if cost.InputPerMillion == 0 && cost.OutputPerMillion == 0 {
			return fmt.Errorf("model cost must include base pricing or pricing tiers")
		}
		if cost.InputPerMillion == 0 || cost.OutputPerMillion == 0 {
			return fmt.Errorf("model cost base input and output rates must be positive")
		}
	}
	if err := validateCostTiers(cost.Tiers); err != nil {
		return err
	}
	if strings.TrimSpace(cost.Source) == "" {
		return fmt.Errorf("model cost source is required")
	}
	sourceLastVerified := strings.TrimSpace(cost.SourceLastVerified)
	if sourceLastVerified == "" {
		return fmt.Errorf("model cost source last verified date is required")
	}
	if _, err := time.Parse("2006-01-02", sourceLastVerified); err != nil {
		return fmt.Errorf("model cost source last verified date must use YYYY-MM-DD format")
	}
	return nil
}

func validateCostTiers(tiers []ModelCostTier) error {
	seenFallback := false
	for index, tier := range tiers {
		if tier.UpToInputTokens < 0 {
			return fmt.Errorf("model cost tier input ceiling must be non-negative")
		}
		if tier.InputPerMillion <= 0 || tier.OutputPerMillion <= 0 {
			return fmt.Errorf("model cost tier input and output rates must be positive")
		}
		if tier.CachedInputPerMillion < 0 {
			return fmt.Errorf("model cost tier cached input rate must be non-negative")
		}
		if tier.UpToInputTokens == 0 {
			if seenFallback {
				return fmt.Errorf("model cost tiers can include only one fallback tier")
			}
			if index != len(tiers)-1 {
				return fmt.Errorf("model cost fallback tier must be last")
			}
			seenFallback = true
		}
	}
	return nil
}

func (model ModelEntry) Supports(capability ModelCapability) bool {
	for _, candidate := range model.Capabilities {
		if candidate == capability {
			return true
		}
	}
	return false
}

func (model ModelEntry) AllowsProvider(provider ProviderKind) bool {
	if len(model.APIProviders) == 0 {
		return model.Provider == provider
	}
	for _, candidate := range model.APIProviders {
		if candidate == provider {
			return true
		}
	}
	return false
}

type Registry struct {
	entries  map[string]ModelEntry
	models   []ModelEntry
	patterns []compiledMatch
}

type compiledMatch struct {
	re      *regexp.Regexp
	modelID string
}

func NewRegistry(entries []ModelEntry) (Registry, error) {
	registry := Registry{
		entries: make(map[string]ModelEntry),
		models:  make([]ModelEntry, 0, len(entries)),
	}
	seenModelIDs := make(map[string]struct{})
	for _, entry := range entries {
		if err := entry.Validate(); err != nil {
			return Registry{}, fmt.Errorf("invalid model %q: %w", entry.ID, err)
		}
		clonedEntry := cloneModelEntry(entry)
		modelID := normalizePattern(entry.ID)
		if modelID == "" {
			return Registry{}, fmt.Errorf("model id is required")
		}
		if _, ok := seenModelIDs[modelID]; ok {
			return Registry{}, fmt.Errorf("duplicate model id %q", modelID)
		}
		seenModelIDs[modelID] = struct{}{}
		if err := registry.register(entry.ID, clonedEntry); err != nil {
			return Registry{}, err
		}
		if err := registry.register(entry.APIModel, clonedEntry); err != nil {
			return Registry{}, err
		}
		for _, alias := range entry.Aliases {
			if err := registry.register(alias, clonedEntry); err != nil {
				return Registry{}, err
			}
		}
		for _, pattern := range entry.MatchPatterns {
			re, err := regexp.Compile(pattern)
			if err != nil {
				return Registry{}, fmt.Errorf("invalid match pattern %q for model %q: %w", pattern, entry.ID, err)
			}
			registry.patterns = append(registry.patterns, compiledMatch{re: re, modelID: clonedEntry.ID})
		}
		registry.models = append(registry.models, clonedEntry)
	}
	// Cross-entry validation (needs every model registered first): a deprecation
	// rule's FallbackID must resolve to a real model, otherwise ResolveWithFallback
	// would silently ignore the rule at runtime instead of failing loudly here.
	for _, entry := range registry.models {
		if entry.Deprecation == nil {
			continue
		}
		fallbackID := strings.TrimSpace(entry.Deprecation.FallbackID)
		if fallbackID == "" {
			continue
		}
		if _, ok := registry.Get(fallbackID); !ok {
			return Registry{}, fmt.Errorf("model %q deprecation fallback %q does not resolve to a known model", entry.ID, fallbackID)
		}
	}
	// A non-empty UpgradeTargetID must resolve to a known model. Otherwise
	// UpgradeTarget would silently disable escalation at runtime (a catalog typo)
	// instead of failing loudly here — mirroring the deprecation fallback check.
	for _, entry := range registry.models {
		targetID := strings.TrimSpace(entry.UpgradeTargetID)
		if targetID == "" {
			continue
		}
		if _, ok := registry.Get(targetID); !ok {
			return Registry{}, fmt.Errorf("model %q upgrade target %q does not resolve to a known model", entry.ID, targetID)
		}
	}
	return registry, nil
}

func (registry Registry) Get(pattern string) (ModelEntry, bool) {
	entry, ok := registry.entries[normalizePattern(pattern)]
	if !ok {
		return ModelEntry{}, false
	}
	return cloneModelEntry(entry), true
}

func (registry Registry) register(pattern string, entry ModelEntry) error {
	normalized := normalizePattern(pattern)
	if normalized == "" {
		return nil
	}
	if existing, ok := registry.entries[normalized]; ok && existing.ID != entry.ID {
		return fmt.Errorf("duplicate model lookup key %q for %q and %q", normalized, existing.ID, entry.ID)
	}
	registry.entries[normalized] = entry
	return nil
}

func normalizePattern(pattern string) string {
	return strings.ToLower(strings.TrimSpace(pattern))
}

func ValidPrimaryProviderKind(provider ProviderKind) bool {
	switch provider {
	case ProviderOpenAI, ProviderAnthropic, ProviderGoogle:
		return true
	default:
		return false
	}
}

func ValidRuntimeProviderKind(provider ProviderKind) bool {
	return ValidPrimaryProviderKind(provider) || provider == ProviderOpenAICompatible
}

func ValidReasoningEffort(effort ReasoningEffort) bool {
	switch effort {
	case ReasoningEffortNone, ReasoningEffortMinimal, ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh, ReasoningEffortXHigh, ReasoningEffortMax, ReasoningEffortUltra:
		return true
	default:
		return false
	}
}

func ValidModelCapability(capability ModelCapability) bool {
	switch capability {
	case ModelCapabilityChat, ModelCapabilityStreaming, ModelCapabilityToolCalling, ModelCapabilityVision, ModelCapabilityJSONMode, ModelCapabilityReasoning, ModelCapabilitySystemPrompt, ModelCapabilityPromptCache, ModelCapabilityLongContext:
		return true
	default:
		return false
	}
}

func ValidModelStatus(status ModelStatus) bool {
	switch status {
	case ModelStatusActive, ModelStatusPreview, ModelStatusDeprecated:
		return true
	default:
		return false
	}
}
