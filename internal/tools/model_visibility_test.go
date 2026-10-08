package tools

import "testing"

func TestToolSearchSurfacesBothEditTools(t *testing.T) {
	registry := NewRegistry()
	registry.Register(NewScopedEditFileTool(t.TempDir(), nil))
	registry.Register(NewScopedApplyPatchTool(t.TempDir(), nil))
	search := NewToolSearchTool(registry).(toolSearchTool)

	eager := search.visibleEagerToolNames(nil, nil, "ask")
	if !eager["edit_file"] {
		t.Fatal("tool_search must describe edit_file as available to the model")
	}
	if !eager["apply_patch"] {
		t.Fatal("tool_search must retain apply_patch as an available edit tool")
	}
}

func TestModelVisibleAdvertisesEveryRegisteredTool(t *testing.T) {
	if ModelVisible(nil) {
		t.Fatal("a nil tool must not be visible")
	}
	for _, tool := range []Tool{NewScopedEditFileTool(t.TempDir(), nil), NewScopedApplyPatchTool(t.TempDir(), nil), NewScopedWriteFileTool(t.TempDir(), nil)} {
		if !ModelVisible(tool) {
			t.Fatalf("%s must be visible to the model", tool.Name())
		}
	}
}
