package modelregistry

import (
	"math"
	"testing"

	"github.com/Twigpine/zero/internal/zeroruntime"
)

// A premium cache-write rate prices cache-creation tokens separately from the
// uncached input and the discounted cache-read.
func TestCalculateCostCacheWritePremium(t *testing.T) {
	model := ModelEntry{
		ID: "test-cw", Provider: ProviderAnthropic,
		Cost: ModelCost{Currency: "USD", InputPerMillion: 10, OutputPerMillion: 30, CachedInputPerMillion: 1, CacheWritePerMillion: 12.5},
	}
	// 1M total input = 600k uncached + 300k cache-read + 100k cache-write.
	usage := zeroruntime.Usage{InputTokens: 1_000_000, CachedInputTokens: 300_000, CacheWriteTokens: 100_000, OutputTokens: 200_000}
	cost, err := CalculateCost(model, usage)
	if err != nil {
		t.Fatal(err)
	}
	want := 6.00 /*uncached 600k@10*/ + 0.30 /*read 300k@1*/ + 1.25 /*write 100k@12.5*/ + 6.00 /*out 200k@30*/
	if math.Abs(cost.TotalCost-want) > 1e-9 {
		t.Fatalf("total = %v, want %v", cost.TotalCost, want)
	}
	if cost.CacheWriteTokens != 100_000 || math.Abs(cost.CacheWriteCost-1.25) > 1e-9 {
		t.Fatalf("cacheWrite = %d tok / $%v", cost.CacheWriteTokens, cost.CacheWriteCost)
	}
}

// Without a configured cache-write rate, cache-write tokens fall back to the full
// input rate (the prior behavior) — no regression for models lacking the rate.
func TestCalculateCostCacheWriteFallsBackToInputRate(t *testing.T) {
	model := ModelEntry{ID: "test", Provider: ProviderOpenAI, Cost: ModelCost{Currency: "USD", InputPerMillion: 10, OutputPerMillion: 30}}
	usage := zeroruntime.Usage{InputTokens: 1_000_000, CacheWriteTokens: 100_000, OutputTokens: 0}
	cost, err := CalculateCost(model, usage)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(cost.TotalCost-10.0) > 1e-9 { // all 1M input at 10/1e6
		t.Fatalf("total = %v, want 10.0 (cache-write folds into input when unpriced)", cost.TotalCost)
	}
	if cost.CacheWriteTokens != 0 {
		t.Fatalf("cache-write should not split out when unpriced, got %d", cost.CacheWriteTokens)
	}
}

// Anthropic models derive cache-write = 1.25x input from the helper.
func TestAnthropicModelsDeriveCacheWriteRate(t *testing.T) {
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatal(err)
	}
	m, err := reg.Require("claude-sonnet-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := m.Cost.CacheWritePerMillion, 3.0*1.25; math.Abs(got-want) > 1e-9 {
		t.Fatalf("sonnet cache-write rate = %v, want %v", got, want)
	}
}
