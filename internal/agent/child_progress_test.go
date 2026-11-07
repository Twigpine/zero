package agent

import (
	"context"
	"testing"

	"github.com/Twigpine/zero/internal/streamjson"
	"github.com/Twigpine/zero/internal/tools"
)

// progressProbeTool records whether the loop handed it a progress callback.
// streams mirrors what a real tool declares via tools.ChildProgressStreamer.
type progressProbeTool struct {
	name    string
	streams bool
	got     *bool
}

func (t *progressProbeTool) Name() string        { return t.name }
func (t *progressProbeTool) Description() string { return "probe" }
func (t *progressProbeTool) Parameters() tools.Schema {
	return tools.Schema{Type: "object", Properties: map[string]tools.PropertySchema{}}
}
func (t *progressProbeTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectRead, Permission: tools.PermissionAllow}
}
func (t *progressProbeTool) Run(context.Context, map[string]any) tools.Result {
	*t.got = false
	return tools.Result{Status: tools.StatusOK, Output: "ok"}
}
func (t *progressProbeTool) RunWithOptions(_ context.Context, _ map[string]any, options tools.RunOptions) tools.Result {
	*t.got = options.Progress != nil
	return tools.Result{Status: tools.StatusOK, Output: "ok"}
}

// StreamsChildProgress makes this type implement tools.ChildProgressStreamer;
// the flag drives the answer. A tool that never declares at all is modelled by
// silentProbeTool below, which genuinely does not implement the interface.
func (t *progressProbeTool) StreamsChildProgress() bool { return t.streams }

// silentProbeTool does NOT implement tools.ChildProgressStreamer at all — the
// state every tool in the tree is in today except Task and orchestrate.
type silentProbeTool struct {
	name string
	got  *bool
}

func (t *silentProbeTool) Name() string        { return t.name }
func (t *silentProbeTool) Description() string { return "probe" }
func (t *silentProbeTool) Parameters() tools.Schema {
	return tools.Schema{Type: "object", Properties: map[string]tools.PropertySchema{}}
}
func (t *silentProbeTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectRead, Permission: tools.PermissionAllow}
}
func (t *silentProbeTool) Run(context.Context, map[string]any) tools.Result {
	*t.got = false
	return tools.Result{Status: tools.StatusOK, Output: "ok"}
}
func (t *silentProbeTool) RunWithOptions(_ context.Context, _ map[string]any, options tools.RunOptions) tools.Result {
	*t.got = options.Progress != nil
	return tools.Result{Status: tools.StatusOK, Output: "ok"}
}

func runProbe(t *testing.T, tool tools.Tool, got *bool, withSink bool) bool {
	t.Helper()
	registry := tools.NewRegistry()
	registry.Register(tool)
	options := Options{}
	if withSink {
		options.OnToolProgress = func(string, streamjson.Event) {}
	}
	result, err := executeToolCall(context.Background(),
		registry,
		ToolCall{ID: "call_1", Name: tool.Name(), Arguments: "{}"},
		PermissionModeUnsafe,
		options)
	if err != nil {
		t.Fatalf("executeToolCall: %v", err)
	}
	if result.Status == "" {
		t.Fatalf("probe produced no result")
	}
	return *got
}

// THE PARITY OBLIGATION for un-gating the progress path.
//
// The relationship asserted here is EQUALITY, per RULES.md §3: a tool's
// progress wiring before and after this change must be the same for every tool
// that already had it and every tool that already lacked it. Only a tool that
// newly DECLARES the interface may change.
//
// The name gate is gone, so the guarantee cannot be "Task still works" — it has
// to be stated in terms of the declaration, which is what these cases do.
func TestProgressCallbackFollowsTheDeclarationNotTheName(t *testing.T) {
	t.Run("a declaring tool receives it (Task's behaviour, preserved)", func(t *testing.T) {
		var got bool
		// Named something other than "Task" ON PURPOSE: under the old name gate
		// this case failed, which is the whole point of the change.
		if !runProbe(t, &progressProbeTool{name: "spawner", streams: true, got: &got}, &got, true) {
			t.Fatal("a tool declaring StreamsChildProgress must receive the callback")
		}
	})

	t.Run("a tool named Task that does NOT declare gets nothing", func(t *testing.T) {
		var got bool
		// The inverse of the old behaviour, and the reason a name is the wrong
		// key: identity is not capability.
		if runProbe(t, &silentProbeTool{name: "Task", got: &got}, &got, true) {
			t.Fatal("the callback must follow the declaration, not the name")
		}
	})

	t.Run("a non-declaring tool receives nothing (every other tool, unchanged)", func(t *testing.T) {
		var got bool
		if runProbe(t, &silentProbeTool{name: "read_file", got: &got}, &got, true) {
			t.Fatal("un-gating must not start handing a callback to tools that never had one")
		}
	})

	t.Run("a declaring tool that answers false gets nothing", func(t *testing.T) {
		var got bool
		if runProbe(t, &progressProbeTool{name: "spawner", streams: false, got: &got}, &got, true) {
			t.Fatal("StreamsChildProgress() == false must be honoured")
		}
	})

	t.Run("no sink means no callback, whatever the tool declares", func(t *testing.T) {
		var got bool
		if runProbe(t, &progressProbeTool{name: "spawner", streams: true, got: &got}, &got, false) {
			t.Fatal("without OnToolProgress there is nothing to forward to")
		}
	})
}
