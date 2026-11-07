package specialist

import (
	"fmt"
	"strings"

	"github.com/Twigpine/zero/internal/modelregistry"
)

// resolveTaskModel canonicalises a task's requested model. An empty request is
// not an error: it means "inherit the parent's model", which is what every task
// did before this existed.
//
// AN UNKNOWN MODEL PASSES THROUGH rather than being refused, and that is a
// deliberate reversal. The curated registry holds thirteen models — OpenAI,
// Anthropic, Google — while a provider serves whatever it serves: an xAI account
// offers half a dozen Grok models, none of them curated, every one of which the
// picker already lists with its context window, tool support and price. Refusing
// what the registry has not heard of made this feature unusable on exactly the
// providers people run.
//
// What is lost is a typo caught at admission. What replaces it is not nothing:
// the child resolves the model through its own provider config and fails with
// "zero model X belongs to <provider>, not <provider>" (providers/factory.go),
// so a wrong name is still reported — one layer later, by the component that
// actually knows what this provider can serve.
//
// ResolveWithFallback, NOT ResolveID, and the difference decides whether the
// plan and the run agree:
//
//   - ResolveID reaches Get, a lookup in a map of normalized keys. Registered
//     aliases resolve; PATTERNS do not, because those live in a separate
//     regex table Get never consults. So "sonnet 4.5" would be refused here and
//     accepted by the child.
//   - Nor does Get apply the deprecation redirect. A deprecated id would be
//     admitted, echoed back into Plan.Args(), written to the saved plan and shown
//     in the panel — while the child, which resolves through ResolveWithFallback,
//     quietly ran the replacement. The plan would say one model and the run would
//     use another, with nothing anywhere to reveal it.
//
// Returning entry.ID rather than the caller's string is the other half of that:
// the id is what reaches argv, so storing anything else re-opens the same gap
// one layer up.
func resolveTaskModel(name string) (string, error) {
	requested := strings.TrimSpace(name)
	if requested == "" {
		return "", nil
	}
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		return "", fmt.Errorf("load the model registry: %w", err)
	}
	entry, _, ok := registry.ResolveWithFallback(requested)
	if !ok {
		// Not curated: the provider is the authority on whether it exists.
		return requested, nil
	}
	return entry.ID, nil
}

// modelTakesExplicitEffort reports whether an explicit reasoning-effort value can
// safely be sent for this model.
//
// The registry is the only thing that can answer it. The child clamps a
// requested effort with EffectiveReasoningEffort ONLY for models it can look up;
// for anything else it forwards the value untouched, and a provider that does
// not accept the parameter rejects the whole request. So a model the registry
// has never seen gets no effort at all, and runs at whatever the provider
// defaults to — which is what it would have done anyway before per-task models
// existed.
func modelTakesExplicitEffort(model string) bool {
	trimmed := strings.TrimSpace(model)
	if trimmed == "" {
		return false
	}
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		return false
	}
	entry, _, ok := registry.ResolveWithFallback(trimmed)
	if !ok {
		return false
	}
	return modelregistry.EffectiveReasoningEffort(entry, modelregistry.ReasoningEffortHigh) != modelregistry.ReasoningEffortNone
}
