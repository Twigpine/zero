package usage

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/Twigpine/zero/internal/modelregistry"
	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// The persistence round-trip is LOSSLESS for cost: a cache-heavy + reasoning turn
// priced live reconstructs to the SAME cost from the persisted event. Before the
// cache/reasoning breakdown was persisted, BuildReport billed all input at the
// full rate and dropped reasoning, over-pricing the session.
func TestEventUsageRoundTripPreservesCacheAndReasoningCost(t *testing.T) {
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	model, err := registry.Require("claude-sonnet-4.5")
	if err != nil {
		t.Fatalf("Require: %v", err)
	}
	live := zeroruntime.Usage{
		InputTokens:       500_000,
		CachedInputTokens: 400_000,
		CacheWriteTokens:  50_000,
		OutputTokens:      20_000,
		ReasoningTokens:   10_000,
	}
	liveCost, err := modelregistry.CalculateCost(model, live)
	if err != nil {
		t.Fatalf("CalculateCost: %v", err)
	}

	raw, err := json.Marshal(EventUsagePayload(live))
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	events := []sessions.Event{{
		SessionID: "s1", Sequence: 1, Type: sessions.EventUsage,
		CreatedAt: "2026-06-19T10:00:00Z", Payload: raw,
	}}
	meta := []sessions.Metadata{{SessionID: "s1", ModelID: "claude-sonnet-4.5"}}

	report, err := BuildReport(events, meta, &registry, 0)
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if math.Abs(report.Total.TotalCost-liveCost.TotalCost) > 1e-9 {
		t.Fatalf("reconstructed cost = %v, live = %v (cache/reasoning lost in persistence)", report.Total.TotalCost, liveCost.TotalCost)
	}

	// Regression guard: the old lossy reconstruction (prompt/completion only) bills
	// all 500k input at the full rate — strictly MORE than the true cost.
	lossy, err := modelregistry.CalculateCost(model, zeroruntime.Usage{InputTokens: 500_000, OutputTokens: 20_000})
	if err != nil {
		t.Fatalf("CalculateCost(lossy): %v", err)
	}
	if lossy.TotalCost <= liveCost.TotalCost {
		t.Fatalf("precondition: lossy cost %v should exceed true cost %v", lossy.TotalCost, liveCost.TotalCost)
	}
}

// Zero cache/reasoning fields are omitted so non-cache turns stay compact and
// decode identically to the pre-feature payload.
func TestEventUsagePayloadOmitsZeroFields(t *testing.T) {
	p := EventUsagePayload(zeroruntime.Usage{InputTokens: 1000, OutputTokens: 200})
	for _, k := range []string{"cachedInputTokens", "cacheWriteTokens", "reasoningTokens"} {
		if _, ok := p[k]; ok {
			t.Errorf("expected %q omitted when zero", k)
		}
	}
	if p["promptTokens"] != 1000 || p["completionTokens"] != 200 {
		t.Fatalf("base fields wrong: %#v", p)
	}
}
