package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Twigpine/zero/internal/localcontrol"
)

func NewLocalDesktopTools(options localcontrol.DesktopOptions) []Tool {
	return []Tool{
		newDesktopWindowsTool(options),
		newDesktopSnapshotTool(options),
		newDesktopActionTool(options),
	}
}

type desktopWindowsTool struct {
	baseTool
	desktop localcontrol.Desktop
}

func newDesktopWindowsTool(options localcontrol.DesktopOptions) Tool {
	return desktopWindowsTool{
		baseTool: baseTool{
			name:        "desktop_windows",
			deferred:    true,
			description: "List native desktop windows through the local desktop automation helper.",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"pid": {Type: "integer", Description: "Optional process id to list windows for.", Minimum: intPtr(1)},
				},
				AdditionalProperties: false,
			},
			safety:       localControlSafety(options.Enabled, SideEffectLocalDesktop, "Reads native desktop window metadata."),
			capabilities: ToolCapabilities{Effect: EffectInteractive, ThreadSafe: false},
		},
		desktop: localcontrol.NewDesktop(options),
	}
}

func (tool desktopWindowsTool) RejectBeforePermission(args map[string]any) (Result, bool) {
	if _, err := desktopWindowsInput(args); err != nil {
		return errorResult("Error: Invalid arguments for desktop_windows: " + err.Error()), true
	}
	return Result{}, false
}

func (tool desktopWindowsTool) Run(ctx context.Context, args map[string]any) Result {
	input, err := desktopWindowsInput(args)
	if err != nil {
		return errorResult("Error: Invalid arguments for desktop_windows: " + err.Error())
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return errorResult("Error: Failed to encode desktop_windows input: " + err.Error())
	}
	return desktopCommandResult(ctx, tool.desktop, "list_windows", []string{"list_windows", string(payload)})
}

type desktopSnapshotTool struct {
	baseTool
	desktop localcontrol.Desktop
}

func newDesktopSnapshotTool(options localcontrol.DesktopOptions) Tool {
	return desktopSnapshotTool{
		baseTool: baseTool{
			name:        "desktop_snapshot",
			deferred:    true,
			description: "Read a native desktop window accessibility snapshot through the local desktop automation helper.",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"pid":       {Type: "integer", Description: "Target process id.", Minimum: intPtr(1)},
					"window_id": {Type: "integer", Description: "Target window id.", Minimum: intPtr(1)},
					"query":     {Type: "string", Description: "Optional query/filter for the snapshot."},
					"session":   {Type: "string", Description: "Optional desktop automation session id."},
				},
				Required:             []string{"pid", "window_id"},
				AdditionalProperties: false,
			},
			safety:       localControlSafety(options.Enabled, SideEffectLocalDesktop, "Reads a native desktop window state."),
			capabilities: ToolCapabilities{Effect: EffectInteractive, ThreadSafe: false},
		},
		desktop: localcontrol.NewDesktop(options),
	}
}

func (tool desktopSnapshotTool) RejectBeforePermission(args map[string]any) (Result, bool) {
	if _, err := desktopSnapshotInput(args); err != nil {
		return errorResult("Error: Invalid arguments for desktop_snapshot: " + err.Error()), true
	}
	return Result{}, false
}

func (tool desktopSnapshotTool) Run(ctx context.Context, args map[string]any) Result {
	input, err := desktopSnapshotInput(args)
	if err != nil {
		return errorResult("Error: Invalid arguments for desktop_snapshot: " + err.Error())
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return errorResult("Error: Failed to encode desktop_snapshot input: " + err.Error())
	}
	return desktopCommandResult(ctx, tool.desktop, "get_window_state", []string{"get_window_state", string(payload)})
}

type desktopActionTool struct {
	baseTool
	desktop localcontrol.Desktop
}

func newDesktopActionTool(options localcontrol.DesktopOptions) Tool {
	return desktopActionTool{
		baseTool: baseTool{
			name:        "desktop_action",
			deferred:    true,
			description: "Run an allowed native desktop action through the local desktop automation helper.",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"command": {Type: "string", Description: "Allowed desktop command to run.", Enum: desktopActionCommandNames()},
					"input":   {Type: "object", Description: "JSON object passed to the desktop command."},
				},
				Required:             []string{"command", "input"},
				AdditionalProperties: false,
			},
			safety:       localControlSafety(options.Enabled, SideEffectLocalDesktop, "Interacts with native desktop windows."),
			capabilities: ToolCapabilities{Effect: EffectInteractive, ThreadSafe: false},
		},
		desktop: localcontrol.NewDesktop(options),
	}
}

func (tool desktopActionTool) RejectBeforePermission(args map[string]any) (Result, bool) {
	if _, _, err := desktopActionArgs(args); err != nil {
		return errorResult("Error: Invalid arguments for desktop_action: " + err.Error()), true
	}
	return Result{}, false
}

func (tool desktopActionTool) Run(ctx context.Context, args map[string]any) Result {
	command, payload, err := desktopActionArgs(args)
	if err != nil {
		return errorResult("Error: Invalid arguments for desktop_action: " + err.Error())
	}
	return desktopCommandResult(ctx, tool.desktop, command, []string{command, payload})
}

var desktopActionCommands = map[string]bool{
	"start_session": true,
	"end_session":   true,
	"launch_app":    true,
	"click":         true,
	"type_text":     true,
	"press_key":     true,
	"hotkey":        true,
	"scroll":        true,
	"drag":          true,
}

func desktopActionCommandNames() []string {
	names := make([]string, 0, len(desktopActionCommands))
	for name := range desktopActionCommands {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func desktopActionArgs(args map[string]any) (string, string, error) {
	command, err := stringArg(args, "command", "", true)
	if err != nil {
		return "", "", err
	}
	command = strings.ToLower(strings.TrimSpace(command))
	if !desktopActionCommands[command] {
		return "", "", fmt.Errorf("command must be one of: %s", strings.Join(desktopActionCommandNames(), ", "))
	}
	input, err := objectArg(args, "input", true)
	if err != nil {
		return "", "", err
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return "", "", err
	}
	return command, string(payload), nil
}

func desktopWindowsInput(args map[string]any) (map[string]any, error) {
	input := map[string]any{}
	if _, ok := args["pid"]; ok {
		pid, err := intArg(args, "pid", 0, 1, 0)
		if err != nil {
			return nil, err
		}
		input["pid"] = pid
	}
	return input, nil
}

func desktopSnapshotInput(args map[string]any) (map[string]any, error) {
	input, err := desktopWindowInput(args)
	if err != nil {
		return nil, err
	}
	for _, key := range []string{"query", "session"} {
		value, err := stringArgWithEmpty(args, key, "", false, false)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(value) != "" {
			input[key] = value
		}
	}
	return input, nil
}

func desktopWindowInput(args map[string]any) (map[string]any, error) {
	pid, err := requiredIntArg(args, "pid", 1, 0)
	if err != nil {
		return nil, err
	}
	windowID, err := requiredIntArg(args, "window_id", 1, 0)
	if err != nil {
		return nil, err
	}
	return map[string]any{"pid": pid, "window_id": windowID}, nil
}

func requiredIntArg(args map[string]any, key string, min int, max int) (int, error) {
	if _, ok := args[key]; !ok {
		return 0, fmt.Errorf("%s is required", key)
	}
	return intArg(args, key, 0, min, max)
}

func objectArg(args map[string]any, key string, required bool) (map[string]any, error) {
	value, ok := args[key]
	if !ok || value == nil {
		if required {
			return nil, fmt.Errorf("%s is required", key)
		}
		return map[string]any{}, nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object", key)
	}
	return object, nil
}

func desktopCommandResult(ctx context.Context, desktop localcontrol.Desktop, command string, args []string) Result {
	return localControlCommandResult(ctx, desktop, "desktop", command, args)
}
