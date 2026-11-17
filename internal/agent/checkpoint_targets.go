package agent

import "github.com/Twigpine/zero/internal/tools"

// CheckpointTargets returns the workspace-relative paths a tool call will mutate,
// resolving its arguments exactly as executeToolCall does. Freeform apply_patch
// input is normalized through the registered built-in tool, so absolute headers
// are rewritten relative to the selected write root and cwd is set: checkpoint
// capture and execution therefore agree on the files at stake.
//
// Best-effort: nil when the call has no capture targets or its arguments cannot
// be resolved. Checkpoint capture never fails a run.
func CheckpointTargets(workspaceRoot string, registry *tools.Registry, call ToolCall) []string {
	if registry == nil {
		return nil
	}
	args := map[string]any{}
	if call.Freeform {
		tool, ok := registry.Get(call.Name)
		if !ok || !tools.IsBuiltInApplyPatch(tool) {
			return nil
		}
		prepared, err := tools.PrepareFreeformApplyPatchArguments(tool, call.Arguments)
		if err != nil {
			return nil
		}
		args = prepared
	} else if call.Arguments != "" {
		if err := decodeToolArguments(call.Arguments, &args); err != nil {
			return nil
		}
	}
	return tools.MutationTargets(workspaceRoot, call.Name, args)
}
