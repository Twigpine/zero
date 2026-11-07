package specialist

import (
	"context"
	"fmt"
	"strings"

	"github.com/Twigpine/zero/internal/tools"
)

const TaskToolName = "Task"

type TaskTool struct {
	executor Executor
}

func NewTaskTool(executor Executor) *TaskTool {
	return &TaskTool{executor: executor}
}

func (tool *TaskTool) Name() string {
	return TaskToolName
}

func (tool *TaskTool) Description() string {
	return "Launch a Zero specialist sub-agent for a focused delegated task."
}

func (tool *TaskTool) Parameters() tools.Schema {
	return tools.Schema{
		Type: "object",
		Properties: map[string]tools.PropertySchema{
			"name": {
				Type:        "string",
				Description: "Specialist name to invoke, such as worker, explorer, or code-review.",
			},
			"prompt": {
				Type:        "string",
				Description: "The focused task to give the specialist.",
			},
			"description": {
				Type:        "string",
				Description: "Short label for the child session.",
			},
			"model": {
				Type: "string",
				Description: "Run THIS sub-agent on a specific model instead of inheriting yours — e.g. a cheap model " +
					"for a scan and a strong one for a judgement. Omit to inherit. The model must be one this provider serves.",
			},
			"run_in_background": {
				Type:        "boolean",
				Description: "Run the specialist in the background and return a task_id immediately.",
				Default:     false,
			},
			"resume": {
				Type:        "string",
				Description: "Existing specialist session id to resume.",
			},
		},
		Required:             []string{"prompt"},
		AdditionalProperties: false,
	}
}

// StreamsChildProgress declares that this tool spawns a child agent run whose
// stream-json events should reach the parent's UI. Previously the agent loop
// inferred this from the tool's NAME; the declaration moves the knowledge to
// the tool that has it.
func (tool *TaskTool) StreamsChildProgress() bool { return true }

func (tool *TaskTool) Safety() tools.Safety {
	return tools.Safety{
		SideEffect:      tools.SideEffectShell,
		Permission:      tools.PermissionPrompt,
		Reason:          "Spawns a Zero specialist sub-agent process.",
		AdvertiseInAuto: true,
	}
}

// PermissionForArgs auto-approves delegating to a READ-ONLY specialist — it can
// only read the workspace, so spawning it is harmless — while keeping the static
// prompt for write-capable specialists (e.g. worker) and for resume. This lets
// the orchestrator delegate exploration/review off a normal prompt without an
// approval wall. Implements tools.ArgsPermissioner.
func (tool *TaskTool) PermissionForArgs(args map[string]any) tools.Permission {
	params, err := parseTaskParameters(args)
	if err != nil {
		return tools.PermissionPrompt
	}
	// Resuming could continue a previously write-capable session; never auto-allow.
	if params.Resume != "" {
		return tools.PermissionPrompt
	}
	if tool.executor.IsReadOnlySpecialist(params.Name) {
		return tools.PermissionAllow
	}
	return tools.PermissionPrompt
}

func (tool *TaskTool) Run(ctx context.Context, args map[string]any) tools.Result {
	return tool.RunWithOptions(ctx, args, tools.RunOptions{})
}

func (tool *TaskTool) RunWithOptions(ctx context.Context, args map[string]any, options tools.RunOptions) tools.Result {
	params, err := parseTaskParameters(args)
	if err != nil {
		return taskError(err)
	}
	result, err := tool.executor.Run(ctx, params, TaskRunOptions{
		ToolCallID:            options.ToolCallID,
		ParentSessionID:       options.SessionID,
		ParentModel:           options.Model,
		ParentReasoningEffort: options.ReasoningEffort,
		CurrentDepth:          options.Depth,
		Cwd:                   options.Cwd,
		// Propagate the parent's permission mode so the child never gains more
		// authority than the parent: a non-unsafe parent yields a non-unsafe
		// child (exec.go specialistAutonomy). Dropping this made every Task
		// sub-agent run at "--auto high" (unsafe) regardless of the parent.
		PermissionMode: options.PermissionMode,
		Progress:       options.Progress,
	})
	if err != nil {
		return taskError(err)
	}
	if result.Result.Meta == nil {
		result.Result.Meta = map[string]string{}
	}
	if result.SessionID != "" {
		result.Result.Meta["session_id"] = result.SessionID
	}
	// A BACKGROUND SPAWN HAS NOT FINISHED, and the caller has to be able to tell.
	//
	// Run returns as soon as the child is launched, with a task_id to poll — so a
	// caller that treats "the Task tool returned" as "the sub-agent is done"
	// reports work as complete that has not started producing any. The TUI did
	// exactly that: four background workers rendered "✓ completed · 0 tool calls
	// · 1s" and the AGENTS panel said "4 finished" while all four were still
	// running, with no specialist_stop recorded for any of them.
	//
	// STRUCTURAL, not inferred from the output prose — the same rule the rest of
	// this package follows for Stalled, ModelRejected and Signal.
	if params.RunInBackground {
		result.Result.Meta["background"] = "true"
	}
	// WHICH MODEL IT ACTUALLY RAN ON. The AGENTS sidebar already renders
	// "on <model>" — sidebar.go — and showed it for plan tasks only, because
	// setModel was called from the plan path alone. A Task sub-agent inherits
	// the parent's model unless its manifest names one, and only the executor
	// knows which of those applied.
	if model := strings.TrimSpace(result.Model); model != "" {
		result.Result.Meta["model"] = model
	}
	return result.Result
}

func parseTaskParameters(args map[string]any) (TaskParameters, error) {
	name, err := optionalTaskString(args, "name")
	if err != nil {
		return TaskParameters{}, err
	}
	prompt, err := optionalTaskString(args, "prompt")
	if err != nil {
		return TaskParameters{}, err
	}
	description, err := optionalTaskString(args, "description")
	if err != nil {
		return TaskParameters{}, err
	}
	resume, err := optionalTaskString(args, "resume")
	if err != nil {
		return TaskParameters{}, err
	}
	runInBackground, err := optionalTaskBool(args, "run_in_background")
	if err != nil {
		return TaskParameters{}, err
	}
	model, err := optionalTaskString(args, "model")
	if err != nil {
		return TaskParameters{}, err
	}
	params := TaskParameters{
		Name:            strings.TrimSpace(name),
		Prompt:          strings.TrimSpace(prompt),
		Description:     strings.TrimSpace(description),
		RunInBackground: runInBackground,
		Resume:          strings.TrimSpace(resume),
		Model:           strings.TrimSpace(model),
	}
	if params.Name == "" && params.Resume == "" {
		return TaskParameters{}, fmt.Errorf("task requires name or resume")
	}
	if params.Prompt == "" {
		return TaskParameters{}, fmt.Errorf("task requires prompt")
	}
	return params, nil
}

func optionalTaskString(args map[string]any, key string) (string, error) {
	if args == nil {
		return "", nil
	}
	value, ok := args[key]
	if !ok || value == nil {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("task %s must be a string", key)
	}
	return text, nil
}

func optionalTaskBool(args map[string]any, key string) (bool, error) {
	if args == nil {
		return false, nil
	}
	value, ok := args[key]
	if !ok || value == nil {
		return false, nil
	}
	flag, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("task %s must be a boolean", key)
	}
	return flag, nil
}

func taskError(err error) tools.Result {
	return tools.Result{Status: tools.StatusError, Output: "Error: " + err.Error()}
}
