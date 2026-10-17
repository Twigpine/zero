package modelregistry

import (
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/zeroruntime"
)

func TestRegistryEstimatesCostFromNormalizedUsage(t *testing.T) {
	registry, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry returned error: %v", err)
	}

	cost, err := registry.EstimateCost("gpt-4.1", zeroruntime.Usage{
		InputTokens:       1_000_000,
		CachedInputTokens: 100_000,
		OutputTokens:      500_000,
	})
	if err != nil {
		t.Fatalf("EstimateCost returned error: %v", err)
	}

	if cost.ModelID != "gpt-4.1" || cost.Provider != ProviderOpenAI {
		t.Fatalf("cost identity = %#v, want gpt-4.1/openai", cost)
	}
	assertClose(t, cost.InputCost, 1.8)
	assertClose(t, cost.CachedInputCost, 0.05)
	assertClose(t, cost.OutputCost, 4)
	assertClose(t, cost.TotalCost, 5.85)
}

func TestRegistryCostSupportsAliasesAndFullyCachedInput(t *testing.T) {
	registry, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry returned error: %v", err)
	}

	cost, err := registry.EstimateCost("openai:gpt-4.1-mini", zeroruntime.Usage{
		InputTokens:       1_000_000,
		CachedInputTokens: 1_000_000,
	})
	if err != nil {
		t.Fatalf("EstimateCost returned error: %v", err)
	}

	if cost.InputCost != 0 {
		t.Fatalf("InputCost = %v, want 0 for fully cached input", cost.InputCost)
	}
	assertClose(t, cost.CachedInputCost, 0.1)
	assertClose(t, cost.TotalCost, 0.1)
}

func TestRegistryCostUsesPromptAndCompletionAliases(t *testing.T) {
	registry, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry returned error: %v", err)
	}

	cost, err := registry.EstimateCost("haiku-3.5", zeroruntime.Usage{
		PromptTokens:     2_000,
		CompletionTokens: 1_000,
	})
	if err != nil {
		t.Fatalf("EstimateCost returned error: %v", err)
	}

	if cost.InputTokens != 2_000 || cost.OutputTokens != 1_000 {
		t.Fatalf("usage = %#v, want prompt/completion aliases", cost)
	}
	assertClose(t, cost.TotalCost, 0.0056)
}

func TestRegistryCostIgnoresCachedInputWithoutCachePricing(t *testing.T) {
	registry, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry returned error: %v", err)
	}

	cost, err := registry.EstimateCost("gpt-4-turbo", zeroruntime.Usage{
		InputTokens:       1_000,
		CachedInputTokens: 1_000,
		OutputTokens:      1_000,
	})
	if err != nil {
		t.Fatalf("EstimateCost returned error: %v", err)
	}

	if cost.CachedInputTokens != 0 || cost.CachedInputCost != 0 {
		t.Fatalf("cached input should be ignored for uncached model pricing: %#v", cost)
	}
	assertClose(t, cost.InputCost, 0.01)
	assertClose(t, cost.OutputCost, 0.03)
}

func TestRegistryCostTreatsReasoningAsOutputBreakdown(t *testing.T) {
	registry, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry returned error: %v", err)
	}

	withReasoning, err := registry.EstimateCost("gpt-4.1", zeroruntime.Usage{
		InputTokens:     1_000,
		OutputTokens:    1_000,
		ReasoningTokens: 400,
	})
	if err != nil {
		t.Fatalf("EstimateCost(withReasoning) returned error: %v", err)
	}
	plain, err := registry.EstimateCost("gpt-4.1", zeroruntime.Usage{
		InputTokens:  1_000,
		OutputTokens: 1_000,
	})
	if err != nil {
		t.Fatalf("EstimateCost(plain) returned error: %v", err)
	}

	if withReasoning.OutputCost != plain.OutputCost || withReasoning.TotalCost != plain.TotalCost {
		t.Fatalf("reasoning should be a breakdown of output cost, with=%#v plain=%#v", withReasoning, plain)
	}
}

func TestRegistryCostSelectsTierForLongContextPricing(t *testing.T) {
	registry, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry returned error: %v", err)
	}

	shortPrompt, err := registry.EstimateCost("gemini-2.5-pro", zeroruntime.Usage{
		InputTokens:  200_000,
		OutputTokens: 1_000,
	})
	if err != nil {
		t.Fatalf("EstimateCost(shortPrompt) returned error: %v", err)
	}
	longPrompt, err := registry.EstimateCost("gemini-2.5-pro", zeroruntime.Usage{
		InputTokens:  200_001,
		OutputTokens: 1_000,
	})
	if err != nil {
		t.Fatalf("EstimateCost(longPrompt) returned error: %v", err)
	}

	if shortPrompt.PricingTier == nil || shortPrompt.PricingTier.InputPerMillion != 1.25 {
		t.Fatalf("short tier = %#v, want input rate 1.25", shortPrompt.PricingTier)
	}
	if longPrompt.PricingTier == nil || longPrompt.PricingTier.InputPerMillion != 2.5 {
		t.Fatalf("long tier = %#v, want input rate 2.5", longPrompt.PricingTier)
	}
	if longPrompt.TotalCost <= shortPrompt.TotalCost {
		t.Fatalf("long prompt cost %v should be greater than short prompt cost %v", longPrompt.TotalCost, shortPrompt.TotalCost)
	}
}

func TestCostFormattingAndValidation(t *testing.T) {
	got, err := FormatCostUSD(0.000123)
	if err != nil {
		t.Fatalf("FormatCostUSD small returned error: %v", err)
	}
	if got != "$0.000123" {
		t.Fatalf("FormatCostUSD small = %q, want $0.000123", got)
	}
	got, err = FormatCostUSD(1.23456)
	if err != nil {
		t.Fatalf("FormatCostUSD regular returned error: %v", err)
	}
	if got != "$1.2346" {
		t.Fatalf("FormatCostUSD regular = %q, want $1.2346", got)
	}
	if _, err := FormatCostUSD(-1); err == nil {
		t.Fatal("FormatCostUSD should reject negative values")
	}

	registry, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry returned error: %v", err)
	}
	_, err = registry.EstimateCost("gpt-4.1", zeroruntime.Usage{InputTokens: -1})
	if err == nil {
		t.Fatal("EstimateCost should reject negative usage")
	}
	if !strings.Contains(err.Error(), "input tokens") {
		t.Fatalf("negative usage error = %q, want input tokens", err.Error())
	}
}

func assertClose(t *testing.T, got float64, want float64) {
	t.Helper()

	diff := got - want
	if diff < 0 {
		diff = -diff
	}
	if diff > 0.0000001 {
		t.Fatalf("got %v, want %v", got, want)
	}
}
