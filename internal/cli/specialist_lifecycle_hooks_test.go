package cli

import (
	"io"
	"testing"

	"github.com/Gitlawb/zero/internal/hooks"
	"github.com/Gitlawb/zero/internal/specialist"
	"github.com/Gitlawb/zero/internal/tools"
)

func TestRegisterSpecialistToolsWiresLifecycleHooksIntoTaskOutput(t *testing.T) {
	registry := tools.NewRegistry()
	runtime, err := registerSpecialistTools(registry, t.TempDir(), 0)
	if err != nil {
		t.Fatalf("registerSpecialistTools returned error: %v", err)
	}
	t.Cleanup(func() { closeSpecialistRuntime(io.Discard, runtime) })

	if runtime.lifecycleHooks == nil {
		t.Fatal("lifecycleHooks is nil after registerSpecialistTools")
	}
	raw, ok := registry.Get("TaskOutput")
	if !ok {
		t.Fatal("TaskOutput tool not registered")
	}
	outputTool, ok := raw.(*specialist.OutputTool)
	if !ok {
		t.Fatalf("TaskOutput type = %T, want *specialist.OutputTool", raw)
	}
	if outputTool.LifecycleHooks != runtime.lifecycleHooks {
		t.Fatal("TaskOutput LifecycleHooks is not the shared registerSpecialistTools bridge")
	}

	dispatcher := hooks.NewDispatcher(hooks.DispatcherOptions{})
	runtime.attachLifecycleHooks(dispatcher)
	if runtime.lifecycleHooks.Dispatch == nil {
		t.Fatal("attachLifecycleHooks left Dispatch nil")
	}
	if outputTool.LifecycleHooks.Dispatch == nil {
		t.Fatal("TaskOutput did not observe Dispatch after attachLifecycleHooks")
	}
}
