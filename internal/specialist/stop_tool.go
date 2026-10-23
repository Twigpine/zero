package specialist

import (
	"context"
	"fmt"
	"strings"

	"github.com/Twigpine/zero/internal/background"
	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/tools"
)

type StopTool struct {
	manager     *background.Manager
	managerFunc BackgroundManagerFunc
	// SessionStore and LifecycleHooks participate in the shared, deduped stop
	// transition so an explicitly stopped recovered task produces exactly one
	// specialistStop hook, the same as a launcher-owned exit.
	SessionStore   *sessions.Store
	LifecycleHooks *LifecycleHooks
}

type stopParameters struct {
	TaskID string
}

func newStopToolWithManagerFunc(managerFunc BackgroundManagerFunc, sessionStore *sessions.Store, lifecycleHooks *LifecycleHooks) *StopTool {
	return &StopTool{managerFunc: managerFunc, SessionStore: sessionStore, LifecycleHooks: lifecycleHooks}
}

func (tool *StopTool) Name() string {
	return "TaskStop"
}

func (tool *StopTool) Description() string {
	return "Stop a running background Zero specialist task."
}

func (tool *StopTool) Parameters() tools.Schema {
	return tools.Schema{
		Type: "object",
		Properties: map[string]tools.PropertySchema{
			"task_id": {
				Type:        "string",
				Description: "Background task id returned by Task.",
			},
		},
		Required:             []string{"task_id"},
		AdditionalProperties: false,
	}
}

func (tool *StopTool) Safety() tools.Safety {
	return tools.Safety{
		SideEffect:      tools.SideEffectShell,
		Permission:      tools.PermissionPrompt,
		Reason:          "Terminates a background specialist process.",
		AdvertiseInAuto: true,
	}
}

func (tool *StopTool) Run(ctx context.Context, args map[string]any) tools.Result {
	params, err := parseStopParameters(args)
	if err != nil {
		return taskError(err)
	}
	manager, err := tool.backgroundManager()
	if err != nil {
		return taskError(err)
	}
	if err := manager.Kill(params.TaskID); err != nil {
		return taskError(err)
	}
	// A recovered task (launcher process gone) may only ever be reaped here: the
	// in-memory onExit callback does not exist in this process, and a later
	// TaskOutput poll is optional. Record the terminal transition now so the
	// specialistStop hook fires exactly once through the shared durable claim.
	// recordBackgroundTaskAccounting is a no-op for non-specialist tasks and for
	// tasks that are still running (Kill raced a status change).
	if task, ok := manager.Get(params.TaskID); ok && task.Type == specialistTaskType {
		Executor{
			SessionStore:   tool.SessionStore,
			LifecycleHooks: tool.LifecycleHooks,
		}.recordBackgroundTaskAccounting(task, taskStreamSummary(task))
	}
	return tools.Result{
		Status: tools.StatusOK,
		Output: fmt.Sprintf("task_id: %s\nstatus: killed", params.TaskID),
		Meta: map[string]string{
			"task_id": params.TaskID,
			"status":  string(background.StatusKilled),
		},
	}
}

func (tool *StopTool) backgroundManager() (*background.Manager, error) {
	if tool.manager != nil {
		return tool.manager, nil
	}
	if tool.managerFunc != nil {
		return tool.managerFunc()
	}
	return nil, fmt.Errorf("background manager is not configured")
}

func parseStopParameters(args map[string]any) (stopParameters, error) {
	taskID, err := optionalTaskString(args, "task_id")
	if err != nil {
		return stopParameters{}, err
	}
	params := stopParameters{TaskID: strings.TrimSpace(taskID)}
	if params.TaskID == "" {
		return stopParameters{}, fmt.Errorf("task stop requires task_id")
	}
	return params, nil
}
