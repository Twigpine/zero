package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/modelregistry"
)

func TestEscalateModelToolMetadata(t *testing.T) {
	tool := NewEscalateModelTool()

	if tool.Name() != "escalate_model" {
		t.Fatalf("name = %q, want escalate_model", tool.Name())
	}
	if tool.Description() == "" {
		t.Fatal("description must not be empty")
	}

	safety := tool.Safety()
	if safety.SideEffect != SideEffectNone {
		t.Fatalf("side effect = %q, want none", safety.SideEffect)
	}
	if safety.Permission != PermissionAllow {
		t.Fatalf("permission = %q, want allow", safety.Permission)
	}
	if !safety.AdvertiseInAuto {
		t.Fatal("escalate_model must advertise in auto")
	}

	schema := tool.Parameters()
	if schema.Type != "object" {
		t.Fatalf("schema type = %q, want object", schema.Type)
	}
	if schema.AdditionalProperties {
		t.Fatal("schema must disallow additional properties")
	}
	if len(schema.Required) != 0 {
		t.Fatalf("escalate_model must have no required args, got %v", schema.Required)
	}
	if _, ok := schema.Properties["reason"]; !ok {
		t.Fatal("schema must advertise an optional reason property")
	}
}

// A non-string reason argument is a soft error, not a panic.
func TestEscalateModelToolRejectsNonStringReason(t *testing.T) {
	tool := NewEscalateModelTool()
	res := tool.Run(context.Background(), map[string]any{"reason": 42})
	if res.Status != StatusError {
		t.Fatalf("status = %q, want error for non-string reason", res.Status)
	}
}

// Resolving against the real catalog: a known mid-tier model escalates to its
// configured stronger target and signals it via Meta["escalate_to_model"].
func TestEscalateModelRunSignalsTarget(t *testing.T) {
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	want, ok := registry.UpgradeTarget("claude-haiku-4.5")
	if !ok {
		t.Skip("catalog has no upgrade target seeded for claude-haiku-4.5")
	}

	tool := newEscalateModelTool(registry)
	res := tool.RunWithOptions(context.Background(), map[string]any{"reason": "task is harder than expected"}, RunOptions{Model: "claude-haiku-4.5"})

	if res.Status != StatusOK {
		t.Fatalf("status = %q, want ok", res.Status)
	}
	if got := res.Meta["escalate_to_model"]; got != want.ID {
		t.Fatalf("meta escalate_to_model = %q, want %q", got, want.ID)
	}
	if !strings.Contains(res.Output, "claude-haiku-4.5") || !strings.Contains(res.Output, want.ID) {
		t.Fatalf("output should name both models, got %q", res.Output)
	}
	if !strings.Contains(res.Output, "Escalating") {
		t.Fatalf("output should announce escalation, got %q", res.Output)
	}
}

// An explicitly empty reason (and an absent reason) is accepted: reason is
// informational only, so it must never fail the escalation. Both forms still
// return the escalation result with the target signalled via Meta.
func TestEscalateModelRunAcceptsEmptyReason(t *testing.T) {
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	target, ok := registry.UpgradeTarget("claude-haiku-4.5")
	if !ok {
		t.Skip("catalog has no upgrade target seeded for claude-haiku-4.5")
	}
	tool := newEscalateModelTool(registry)

	cases := []struct {
		name string
		args map[string]any
	}{
		{name: "explicit empty reason", args: map[string]any{"reason": ""}},
		{name: "absent reason", args: map[string]any{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := tool.RunWithOptions(context.Background(), tc.args, RunOptions{Model: "claude-haiku-4.5"})
			if res.Status != StatusOK {
				t.Fatalf("status = %q output = %q, want ok (empty reason must not error)", res.Status, res.Output)
			}
			if got := res.Meta["escalate_to_model"]; got != target.ID {
				t.Fatalf("meta escalate_to_model = %q, want %q", got, target.ID)
			}
		})
	}
}

// A top-tier model (no upgrade target), an unknown model, and an empty model all
// produce an informational, no-meta, OK result — the loop must not switch.
func TestEscalateModelRunNoTarget(t *testing.T) {
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	tool := newEscalateModelTool(registry)

	cases := []struct {
		name      string
		model     string
		wantInOut string
	}{
		{name: "top-tier", model: "claude-opus-4.1", wantInOut: "claude-opus-4.1"},
		{name: "unknown", model: "not-a-real-model", wantInOut: "not-a-real-model"},
		{name: "empty", model: "", wantInOut: "current model"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := tool.RunWithOptions(context.Background(), map[string]any{}, RunOptions{Model: tc.model})

			if res.Status != StatusOK {
				t.Fatalf("status = %q, want ok", res.Status)
			}
			if _, ok := res.Meta["escalate_to_model"]; ok {
				t.Fatalf("no-target result must carry no escalate_to_model meta, got %v", res.Meta)
			}
			if !strings.Contains(res.Output, "no escalation performed") {
				t.Fatalf("output should be informational, got %q", res.Output)
			}
			if !strings.Contains(res.Output, tc.wantInOut) {
				t.Fatalf("output should name %q, got %q", tc.wantInOut, res.Output)
			}
		})
	}
}

// Sanity: the empty-args path (no reason) is valid and does not switch when on a
// top-tier model.
func TestEscalateModelRunDefaultsToNoEscalation(t *testing.T) {
	tool := newEscalateModelTool(modelregistry.Registry{})
	res := tool.RunWithOptions(context.Background(), map[string]any{}, RunOptions{Model: "claude-opus-4.1"})
	if res.Status != StatusOK {
		t.Fatalf("status = %q, want ok", res.Status)
	}
	if len(res.Meta) != 0 {
		t.Fatalf("empty registry must never signal a target, got meta %v", res.Meta)
	}
}

// End-to-end through the Registry: RunWithOptions must route to the tool's
// optionsAwareTool branch (threading options.Model) and, because the tool is
// PermissionAllow, run without a permission grant.
func TestEscalateModelThroughRegistryDispatch(t *testing.T) {
	modelReg, err := modelregistry.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	target, ok := modelReg.UpgradeTarget("claude-haiku-4.5")
	if !ok {
		t.Skip("catalog has no upgrade target seeded for claude-haiku-4.5")
	}

	reg := NewRegistry()
	reg.Register(newEscalateModelTool(modelReg))

	res := reg.RunWithOptions(context.Background(), "escalate_model", map[string]any{}, RunOptions{
		Model: "claude-haiku-4.5",
	})
	if res.Status != StatusOK {
		t.Fatalf("status = %q output = %q", res.Status, res.Output)
	}
	if got := res.Meta["escalate_to_model"]; got != target.ID {
		t.Fatalf("meta escalate_to_model = %q, want %q", got, target.ID)
	}
	if !strings.Contains(res.Output, target.ID) {
		t.Fatalf("output should name target %q, got %q", target.ID, res.Output)
	}
}
