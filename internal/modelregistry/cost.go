package modelregistry

import (
	"fmt"
	"math"
	"sort"

	"github.com/Twigpine/zero/internal/zeroruntime"
)

const tokensPerMillion = 1_000_000

type CostBreakdown struct {
	ModelID           string
	Provider          ProviderKind
	Currency          string
	InputTokens       int
	CachedInputTokens int
	CacheWriteTokens  int
	OutputTokens      int
	ReasoningTokens   int
	InputCost         float64
	CachedInputCost   float64
	CacheWriteCost    float64
	OutputCost        float64
	TotalCost         float64
	PricingTier       *ModelCostTier
}

func (registry Registry) EstimateCost(pattern string, usage zeroruntime.Usage) (CostBreakdown, error) {
	model, err := registry.Require(pattern)
	if err != nil {
		return CostBreakdown{}, err
	}
	return CalculateCost(model, usage)
}

func CalculateCost(model ModelEntry, usage zeroruntime.Usage) (CostBreakdown, error) {
	inputTokens, err := nonNegativeUsage(usage.EffectiveInputTokens(), "input tokens")
	if err != nil {
		return CostBreakdown{}, err
	}
	outputTokens, err := nonNegativeUsage(usage.EffectiveOutputTokens(), "output tokens")
	if err != nil {
		return CostBreakdown{}, err
	}
	reasoningTokens, err := nonNegativeUsage(usage.ReasoningTokens, "reasoning tokens")
	if err != nil {
		return CostBreakdown{}, err
	}
	requestedCachedInputTokens, err := nonNegativeUsage(usage.CachedInputTokens, "cached input tokens")
	if err != nil {
		return CostBreakdown{}, err
	}
	if requestedCachedInputTokens > inputTokens {
		requestedCachedInputTokens = inputTokens
	}
	requestedCacheWriteTokens, err := nonNegativeUsage(usage.CacheWriteTokens, "cache write tokens")
	if err != nil {
		return CostBreakdown{}, err
	}
	// Cache-read and cache-write are disjoint subsets of the input.
	if requestedCacheWriteTokens > inputTokens-requestedCachedInputTokens {
		requestedCacheWriteTokens = inputTokens - requestedCachedInputTokens
		if requestedCacheWriteTokens < 0 {
			requestedCacheWriteTokens = 0
		}
	}

	tier, err := selectCostTier(model.Cost, inputTokens)
	if err != nil {
		return CostBreakdown{}, err
	}

	inputRate, outputRate, cachedRate, cacheWriteRate, err := costRates(model.Cost, tier)
	if err != nil {
		return CostBreakdown{}, err
	}

	cachedInputTokens := 0
	if cachedRate > 0 {
		cachedInputTokens = requestedCachedInputTokens
	}
	// Only split cache-write tokens out at the premium rate when one is
	// configured; otherwise they stay billed at the full input rate (the prior
	// behavior for every model that lacks a cache-write rate).
	cacheWriteTokens := 0
	if cacheWriteRate > 0 {
		cacheWriteTokens = requestedCacheWriteTokens
	}
	uncachedInputTokens := inputTokens - cachedInputTokens - cacheWriteTokens
	if uncachedInputTokens < 0 {
		uncachedInputTokens = 0
	}
	inputCost := costForTokens(uncachedInputTokens, inputRate)
	cachedInputCost := costForTokens(cachedInputTokens, cachedRate)
	cacheWriteCost := costForTokens(cacheWriteTokens, cacheWriteRate)
	outputCost := costForTokens(outputTokens, outputRate)

	breakdown := CostBreakdown{
		ModelID:           model.ID,
		Provider:          model.Provider,
		Currency:          model.Cost.Currency,
		InputTokens:       inputTokens,
		CachedInputTokens: cachedInputTokens,
		CacheWriteTokens:  cacheWriteTokens,
		OutputTokens:      outputTokens,
		ReasoningTokens:   reasoningTokens,
		InputCost:         inputCost,
		CachedInputCost:   cachedInputCost,
		CacheWriteCost:    cacheWriteCost,
		OutputCost:        outputCost,
		TotalCost:         inputCost + cachedInputCost + cacheWriteCost + outputCost,
	}
	if tier != nil {
		tierCopy := *tier
		breakdown.PricingTier = &tierCopy
	}
	return breakdown, nil
}

func FormatCostUSD(cost float64) (string, error) {
	if math.IsNaN(cost) || math.IsInf(cost, 0) || cost < 0 {
		return "", fmt.Errorf("invalid model cost: %v", cost)
	}
	if cost > 0 && cost < 0.01 {
		return fmt.Sprintf("$%.6f", cost), nil
	}
	return fmt.Sprintf("$%.4f", cost), nil
}

func selectCostTier(cost ModelCost, inputTokens int) (*ModelCostTier, error) {
	if len(cost.Tiers) == 0 {
		return nil, nil
	}

	tiers := append([]ModelCostTier{}, cost.Tiers...)
	sort.SliceStable(tiers, func(left int, right int) bool {
		leftBound := tiers[left].UpToInputTokens
		rightBound := tiers[right].UpToInputTokens
		if leftBound == 0 {
			return false
		}
		if rightBound == 0 {
			return true
		}
		return leftBound < rightBound
	})

	for _, tier := range tiers {
		if tier.UpToInputTokens > 0 && inputTokens <= tier.UpToInputTokens {
			return &tier, nil
		}
	}
	for _, tier := range tiers {
		if tier.UpToInputTokens == 0 {
			return &tier, nil
		}
	}
	return nil, fmt.Errorf("no model cost tier covers %d input tokens", inputTokens)
}

func costRates(cost ModelCost, tier *ModelCostTier) (float64, float64, float64, float64, error) {
	inputRate := cost.InputPerMillion
	outputRate := cost.OutputPerMillion
	cachedRate := cost.CachedInputPerMillion
	cacheWriteRate := cost.CacheWritePerMillion
	if tier != nil {
		inputRate = tier.InputPerMillion
		outputRate = tier.OutputPerMillion
		cachedRate = tier.CachedInputPerMillion
		cacheWriteRate = tier.CacheWritePerMillion
	}
	if !validRate(inputRate) || inputRate == 0 {
		return 0, 0, 0, 0, fmt.Errorf("missing model input pricing rate")
	}
	if !validRate(outputRate) || outputRate == 0 {
		return 0, 0, 0, 0, fmt.Errorf("missing model output pricing rate")
	}
	if !validRate(cachedRate) {
		return 0, 0, 0, 0, fmt.Errorf("invalid model cached input pricing rate")
	}
	// Cache-write is optional; 0 means "not priced separately". Only reject a
	// genuinely invalid (NaN/Inf/negative) rate.
	if !validRate(cacheWriteRate) {
		return 0, 0, 0, 0, fmt.Errorf("invalid model cache write pricing rate")
	}
	return inputRate, outputRate, cachedRate, cacheWriteRate, nil
}

func costForTokens(tokens int, perMillionRate float64) float64 {
	return (float64(tokens) / tokensPerMillion) * perMillionRate
}

func nonNegativeUsage(value int, label string) (int, error) {
	if value < 0 {
		return 0, fmt.Errorf("%s must be non-negative", label)
	}
	return value, nil
}

func validRate(rate float64) bool {
	return !math.IsNaN(rate) && !math.IsInf(rate, 0) && rate >= 0
}
