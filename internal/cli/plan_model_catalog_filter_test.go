package cli

import (
	"testing"

	"github.com/Twigpine/zero/internal/providermodeldiscovery"
)

// A PLAN MUST NOT BE OFFERED A MODEL ITS OWN PROVIDER WILL REFUSE.
//
// providers.New applies providermodelcatalog's per-provider allow-list at client
// creation (validateModelAllowedForProvider), and `providers models` applies it
// before printing. The plan discoverer did not, so on a scoped provider the
// router was handed candidates that could never run: assignment succeeded, the
// panel showed the model, and the task died at dispatch with "provider X does
// not allow model Y" — the same invisible-until-dispatch failure as being
// assigned from another provider's list altogether.
func TestPlanDiscoveryDropsModelsTheProviderWillRefuse(t *testing.T) {
	// opencode-go-anthropic-compatible is scoped to Qwen and MiniMax families.
	found := []providermodeldiscovery.Model{
		{ID: "qwen3.5-coder", ToolCall: true},
		{ID: "claude-sonnet-5", ToolCall: true},
		{ID: "minimax-m2", ToolCall: true},
		{ID: "gpt-5", ToolCall: true},
	}

	got := planModelsFromDiscovered("opencode-go-anthropic-compatible", found)

	ids := map[string]bool{}
	for _, model := range got {
		ids[model.ID] = true
	}
	for _, refused := range []string{"claude-sonnet-5", "gpt-5"} {
		if ids[refused] {
			t.Errorf("%q reached the plan; providers.New refuses it for this provider, so every task assigned it dies at dispatch", refused)
		}
	}
	for _, allowed := range []string{"qwen3.5-coder", "minimax-m2"} {
		if !ids[allowed] {
			t.Errorf("%q was dropped, but this provider serves it", allowed)
		}
	}
}

// An UNSCOPED provider keeps its whole list. The allow-list defaults to
// permitting everything, and a filter that quietly narrowed an ordinary provider
// would remove models a plan is entitled to use.
func TestPlanDiscoveryKeepsEveryModelOnAnUnscopedProvider(t *testing.T) {
	found := []providermodeldiscovery.Model{
		{ID: "grok-4.5", ToolCall: true},
		{ID: "grok-code-fast", ToolCall: true},
		{ID: "claude-sonnet-5", ToolCall: true},
	}
	for _, catalogID := range []string{"xai", "", "  "} {
		got := planModelsFromDiscovered(catalogID, found)
		if len(got) != len(found) {
			t.Errorf("catalogID %q narrowed an unscoped provider from %d models to %d", catalogID, len(found), len(got))
		}
	}
}

// The translation must carry every field the plan chooses between — a model that
// arrives with its costs zeroed is routed as if it were free.
func TestPlanDiscoveryCarriesTheFieldsRoutingReads(t *testing.T) {
	got := planModelsFromDiscovered("xai", []providermodeldiscovery.Model{{
		ID: "grok-4.5", Description: "frontier", ToolCall: true, Reasoning: true,
		InputCost: 3, OutputCost: 15, OutputModalities: []string{"text"},
	}})
	if len(got) != 1 {
		t.Fatalf("got %d models, want 1", len(got))
	}
	model := got[0]
	switch {
	case model.ID != "grok-4.5", model.Description != "frontier":
		t.Errorf("identity lost: %+v", model)
	case !model.ToolCall, !model.Reasoning:
		t.Errorf("capabilities lost, so routing cannot tell this model apart: %+v", model)
	case model.InputCost != 3, model.OutputCost != 15:
		t.Errorf("costs lost, so the cheapest-model rule ranks this as free: %+v", model)
	case len(model.OutputModalities) != 1:
		t.Errorf("modalities lost: %+v", model)
	}
}
