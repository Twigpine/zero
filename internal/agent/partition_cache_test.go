package agent

import (
	"context"
	"reflect"
	"testing"

	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// countingSchemaTool records how many times its Parameters() schema is read, so a
// test can prove the definition cache avoids re-rendering across turns.
type countingSchemaTool struct {
	name  string
	calls *int
}

func (t countingSchemaTool) Name() string        { return t.name }
func (t countingSchemaTool) Description() string { return "counts schema reads" }
func (t countingSchemaTool) Parameters() tools.Schema {
	*t.calls++
	return tools.Schema{Type: "object", AdditionalProperties: false, Properties: map[string]tools.PropertySchema{
		"x": {Type: "string"},
	}}
}
func (t countingSchemaTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectRead, Permission: tools.PermissionAllow}
}
func (t countingSchemaTool) Run(_ context.Context, _ map[string]any) tools.Result {
	return tools.Result{Status: tools.StatusOK}
}

func TestRuntimeToolDefinitionMarksOnlyBuiltInApplyPatchFreeform(t *testing.T) {
	builtIn := runtimeToolDefinition(tools.NewScopedApplyPatchTool(t.TempDir(), nil))
	if builtIn.Type != zeroruntime.ToolDefinitionFreeform {
		t.Fatalf("built-in apply_patch type = %q, want freeform", builtIn.Type)
	}

	calls := 0
	custom := runtimeToolDefinition(countingSchemaTool{name: "apply_patch", calls: &calls})
	if custom.Type != "" {
		t.Fatalf("custom apply_patch type = %q, want legacy function shape", custom.Type)
	}
}

// The cache renders a tool's schema once and reuses it across calls, and its
// output is identical to the uncached path.
func TestPartitionToolsCacheRendersOnceAndMatchesUncached(t *testing.T) {
	calls := 0
	registry := tools.NewRegistry()
	registry.Register(countingSchemaTool{name: "alpha", calls: &calls})
	registry.Register(countingSchemaTool{name: "beta", calls: &calls})

	uncached, _ := partitionTools(registry, PermissionModeAuto, Options{}, map[string]bool{})

	base := calls
	cache := map[string]zeroruntime.ToolDefinition{}
	first, _ := partitionToolsCached(registry, PermissionModeAuto, Options{}, map[string]bool{}, cache)
	rendersAfterFirst := calls - base
	second, _ := partitionToolsCached(registry, PermissionModeAuto, Options{}, map[string]bool{}, cache)
	rendersAfterSecond := calls - base

	if rendersAfterFirst != 2 {
		t.Fatalf("first cached call should render 2 tools once each, rendered %d times", rendersAfterFirst)
	}
	if rendersAfterSecond != 2 {
		t.Fatalf("second cached call must reuse the cache (no new schema reads), total renders = %d", rendersAfterSecond)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("cached calls must return identical definitions")
	}
	if !reflect.DeepEqual(first, uncached) {
		t.Fatal("cached output must match the uncached partitionTools output")
	}
}
