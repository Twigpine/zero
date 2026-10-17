package specialist

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Twigpine/zero/internal/background"
	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/streamjson"
	"github.com/Twigpine/zero/internal/tools"
)

const (
	defaultTaskOutputPollInterval = 500 * time.Millisecond
	defaultTaskOutputTimeout      = 30 * time.Second
	maxTaskOutputTimeout          = 20 * time.Minute
)

type OutputTool struct {
	manager        *background.Manager
	managerFunc    BackgroundManagerFunc
	SessionStore   *sessions.Store
	PollInterval   time.Duration
	DefaultTimeout time.Duration
	MaxTimeout     time.Duration
}

type outputParameters struct {
	TaskID    string
	Block     bool
	TimeoutMS int
}

func newOutputToolWithManagerFunc(managerFunc BackgroundManagerFunc, sessionStore *sessions.Store) *OutputTool {
	return &OutputTool{managerFunc: managerFunc, SessionStore: sessionStore}
}

func (tool *OutputTool) Name() string {
	return "TaskOutput"
}

func (tool *OutputTool) Description() string {
	return "Poll the output of a background Zero specialist task."
}

func (tool *OutputTool) Parameters() tools.Schema {
	return tools.Schema{
		Type: "object",
		Properties: map[string]tools.PropertySchema{
			"task_id": {
				Type:        "string",
				Description: "Background task id returned by Task.",
			},
			"block": {
				Type:        "boolean",
				Description: "Wait for the task to finish before returning, up to timeout.",
				Default:     false,
			},
			"timeout": {
				Type:        "integer",
				Description: "Maximum wait time in milliseconds when block is true.",
				Default:     30000,
			},
		},
		Required:             []string{"task_id"},
		AdditionalProperties: false,
	}
}

func (tool *OutputTool) Safety() tools.Safety {
	return tools.Safety{
		SideEffect: tools.SideEffectRead,
		Permission: tools.PermissionAllow,
		Reason:     "Reads a background specialist task output file.",
	}
}

func (tool *OutputTool) Run(ctx context.Context, args map[string]any) tools.Result {
	params, err := parseOutputParameters(args)
	if err != nil {
		return taskError(err)
	}
	manager, err := tool.backgroundManager()
	if err != nil {
		return taskError(err)
	}
	task, ok := manager.Get(params.TaskID)
	if !ok {
		return taskError(fmt.Errorf("background task not found: %s", params.TaskID))
	}
	if !params.Block {
		return tool.readOutput(task)
	}
	return tool.blockOnOutput(ctx, manager, task.ID, params.TimeoutMS)
}

func (tool *OutputTool) blockOnOutput(ctx context.Context, manager *background.Manager, taskID string, timeoutMS int) tools.Result {
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := tool.timeout(timeoutMS)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	interval := tool.pollInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		task, ok := manager.Get(taskID)
		if !ok {
			return taskError(fmt.Errorf("background task not found: %s", taskID))
		}
		if task.Status != background.StatusRunning {
			return tool.readOutput(task)
		}
		select {
		case <-ctx.Done():
			return tool.readLatestOutput(manager, taskID)
		case <-timer.C:
			return tool.readLatestOutput(manager, taskID)
		case <-ticker.C:
		}
	}
}

func (tool *OutputTool) readLatestOutput(manager *background.Manager, taskID string) tools.Result {
	task, ok := manager.Get(taskID)
	if !ok {
		return taskError(fmt.Errorf("background task not found: %s", taskID))
	}
	return tool.readOutput(task)
}

func (tool *OutputTool) readOutput(task background.Task) tools.Result {
	data, err := os.ReadFile(task.OutputFile)
	if err != nil {
		return taskError(err)
	}
	dataString := string(data)
	summary, rawLines := summarizeTaskData(dataString, task.ExitCode)
	if task.Status != background.StatusRunning {
		Executor{SessionStore: tool.SessionStore}.recordBackgroundTaskAccounting(task, summary)
	}
	return tools.Result{
		Status: tools.StatusOK,
		Output: formatTaskOutputSummary(task, summary, rawLines),
		Meta: map[string]string{
			"task_id": string(task.ID),
			"status":  string(task.Status),
		},
	}
}

func (tool *OutputTool) backgroundManager() (*background.Manager, error) {
	if tool.manager != nil {
		return tool.manager, nil
	}
	if tool.managerFunc != nil {
		return tool.managerFunc()
	}
	return nil, fmt.Errorf("background manager is not configured")
}

func (tool *OutputTool) pollInterval() time.Duration {
	if tool.PollInterval > 0 {
		return tool.PollInterval
	}
	return defaultTaskOutputPollInterval
}

func (tool *OutputTool) timeout(timeoutMS int) time.Duration {
	timeout := time.Duration(timeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = tool.DefaultTimeout
	}
	if timeout <= 0 {
		timeout = defaultTaskOutputTimeout
	}
	maxTimeout := tool.MaxTimeout
	if maxTimeout <= 0 {
		maxTimeout = maxTaskOutputTimeout
	}
	if timeout > maxTimeout {
		return maxTimeout
	}
	return timeout
}

func parseOutputParameters(args map[string]any) (outputParameters, error) {
	taskID, err := optionalTaskString(args, "task_id")
	if err != nil {
		return outputParameters{}, err
	}
	block, err := optionalTaskBool(args, "block")
	if err != nil {
		return outputParameters{}, err
	}
	timeout, err := optionalTaskInt(args, "timeout")
	if err != nil {
		return outputParameters{}, err
	}
	params := outputParameters{
		TaskID:    strings.TrimSpace(taskID),
		Block:     block,
		TimeoutMS: timeout,
	}
	if params.TaskID == "" {
		return outputParameters{}, fmt.Errorf("task output requires task_id")
	}
	return params, nil
}

func optionalTaskInt(args map[string]any, key string) (int, error) {
	if args == nil {
		return 0, nil
	}
	value, ok := args[key]
	if !ok || value == nil {
		return 0, nil
	}
	switch number := value.(type) {
	case int:
		return number, nil
	case int64:
		return int(number), nil
	case float64:
		if number != float64(int(number)) {
			return 0, fmt.Errorf("task %s must be an integer", key)
		}
		return int(number), nil
	default:
		return 0, fmt.Errorf("task %s must be an integer", key)
	}
}

func formatTaskOutputSummary(task background.Task, summary StreamResult, rawLines []string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "task_id: %s\n", task.ID)
	fmt.Fprintf(&builder, "status: %s\n", task.Status)
	if task.SpecialistName != "" {
		fmt.Fprintf(&builder, "specialist: %s\n", task.SpecialistName)
	}
	if task.Description != "" {
		fmt.Fprintf(&builder, "description: %s\n", task.Description)
	}
	if task.PID > 0 {
		fmt.Fprintf(&builder, "pid: %d\n", task.PID)
	}
	if !task.StartedAt.IsZero() {
		fmt.Fprintf(&builder, "started_at: %s\n", task.StartedAt.Format(time.RFC3339))
	}
	if !task.CompletedAt.IsZero() {
		fmt.Fprintf(&builder, "completed_at: %s\n", task.CompletedAt.Format(time.RFC3339))
		fmt.Fprintf(&builder, "exit_code: %d\n", task.ExitCode)
		if task.ExitCode < 0 {
			// A negative exit code means the background child was terminated by a
			// signal rather than exiting (the int-only launcher can't carry the
			// signal name). Give the same actionable hint the foreground path does.
			builder.WriteString("note: terminated by a signal — killed before it finished. Likely causes: the OS out-of-memory killer (common when many sub-agents run in parallel — try fewer), a timeout, or cancellation.\n")
		}
	}

	if summary.Text != "" {
		fmt.Fprintf(&builder, "\noutput:\n%s\n", summary.Text)
	}
	if len(summary.Tools) > 0 {
		fmt.Fprintf(&builder, "\ntools: %s\n", strings.Join(summary.Tools, ", "))
	}
	if len(summary.Errors) > 0 {
		fmt.Fprintf(&builder, "\nerrors: %s\n", strings.Join(summary.Errors, "; "))
	}
	if len(rawLines) > 0 {
		fmt.Fprintf(&builder, "\nraw:\n%s\n", strings.Join(rawLines, "\n"))
	}
	if summary.Text == "" && len(summary.Tools) == 0 && len(summary.Errors) == 0 && len(rawLines) == 0 {
		builder.WriteString("\noutput: <empty>\n")
	}
	return strings.TrimSpace(builder.String())
}

func summarizeTaskData(data string, exitCode int) (StreamResult, []string) {
	events := []streamjson.Event{}
	rawLines := []string{}
	for _, line := range strings.Split(data, "\n") {
		rawLine := strings.TrimSuffix(line, "\r")
		trimmed := strings.TrimSpace(rawLine)
		if trimmed == "" {
			continue
		}
		var event streamjson.Event
		if err := json.Unmarshal([]byte(trimmed), &event); err != nil || event.Type == "" {
			rawLines = append(rawLines, rawLine)
			continue
		}
		events = append(events, event)
	}
	return SummarizeStream(events, exitCode), rawLines
}
