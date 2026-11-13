package tools

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Twigpine/zero/internal/localcontrol"
)

func NewLocalTerminalTools(options localcontrol.TerminalOptions) []Tool {
	return []Tool{newTerminalSessionTool(options)}
}

type terminalSessionTool struct {
	baseTool
	terminal localcontrol.Terminal
}

func newTerminalSessionTool(options localcontrol.TerminalOptions) Tool {
	return terminalSessionTool{
		baseTool: baseTool{
			name:        "terminal_session",
			deferred:    true,
			description: "Launch or control a local virtual terminal session through the local terminal automation helper.",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"action":     {Type: "string", Description: "Terminal session action.", Enum: terminalActionNames()},
					"session":    {Type: "string", Description: "Terminal session id."},
					"command":    {Type: "string", Description: "Command to launch for action=launch."},
					"text":       {Type: "string", Description: "Text to type for action=type, or text to submit before Enter for action=send_line."},
					"key":        {Type: "string", Description: "Single key or key chord for action=press, such as enter, tab, or ctrl-c."},
					"keys":       {Type: "array", Items: &PropertySchema{Type: "string"}, Description: "Sequence of keys or key chords for action=press, such as [\"tab\", \"enter\"] or [\"ctrl-c\", \"enter\"]."},
					"pattern":    {Type: "string", Description: "Text or regex pattern for action=wait."},
					"timeout_ms": {Type: "integer", Description: "Wait timeout in milliseconds.", Minimum: intPtr(1)},
					"cols":       {Type: "integer", Description: "Terminal columns for action=launch.", Default: 120, Minimum: intPtr(20), Maximum: intPtr(300)},
					"rows":       {Type: "integer", Description: "Terminal rows for action=launch.", Default: 36, Minimum: intPtr(5), Maximum: intPtr(120)},
					"trim":       {Type: "boolean", Description: "Trim trailing blank screen lines for action=snapshot.", Default: true},
					"cursor":     {Type: "boolean", Description: "Include the visible cursor in action=snapshot output.", Default: false},
					"all":        {Type: "boolean", Description: "Return all buffered terminal output for action=read without advancing the read cursor.", Default: false},
					"follow":     {Type: "boolean", Description: "Block until new process output arrives for action=read.", Default: false},
				},
				Required:             []string{"action", "session"},
				AdditionalProperties: false,
			},
			safety:       localControlSafety(options.Enabled, SideEffectLocalTerminal, "Launches or interacts with a local virtual terminal session."),
			capabilities: ToolCapabilities{Effect: EffectInteractive, ThreadSafe: false, ResourceKeys: processResourceKeys},
		},
		terminal: localcontrol.NewTerminal(options),
	}
}

func (tool terminalSessionTool) RejectBeforePermission(args map[string]any) (Result, bool) {
	if _, err := terminalSessionPlan(args); err != nil {
		return errorResult("Error: Invalid arguments for terminal_session: " + err.Error()), true
	}
	return Result{}, false
}

func (tool terminalSessionTool) Run(ctx context.Context, args map[string]any) Result {
	plan, err := terminalSessionPlan(args)
	if err != nil {
		return errorResult("Error: Invalid arguments for terminal_session: " + err.Error())
	}
	return terminalCommandResult(ctx, tool.terminal, plan)
}

var terminalActions = map[string]bool{
	"launch":    true,
	"read":      true,
	"type":      true,
	"press":     true,
	"send_line": true,
	"wait":      true,
	"wait_idle": true,
	"snapshot":  true,
	"close":     true,
}

func terminalActionNames() []string {
	names := make([]string, 0, len(terminalActions))
	for name := range terminalActions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

type terminalCommandPlan struct {
	action   string
	commands [][]string
}

func terminalSessionPlan(args map[string]any) (terminalCommandPlan, error) {
	action, err := stringArg(args, "action", "", true)
	if err != nil {
		return terminalCommandPlan{}, err
	}
	action = strings.ToLower(strings.TrimSpace(action))
	if !terminalActions[action] {
		return terminalCommandPlan{}, fmt.Errorf("action must be one of: %s", strings.Join(terminalActionNames(), ", "))
	}
	session, err := stringArg(args, "session", "", true)
	if err != nil {
		return terminalCommandPlan{}, err
	}

	switch action {
	case "launch":
		command, err := stringArg(args, "command", "", true)
		if err != nil {
			return terminalCommandPlan{}, err
		}
		cols, err := intArg(args, "cols", 120, 20, 300)
		if err != nil {
			return terminalCommandPlan{}, err
		}
		rows, err := intArg(args, "rows", 36, 5, 120)
		if err != nil {
			return terminalCommandPlan{}, err
		}
		return singleTerminalCommand(action, "launch", command, "-s", session, "--cols", strconv.Itoa(cols), "--rows", strconv.Itoa(rows)), nil
	case "type":
		text, err := stringArgWithEmpty(args, "text", "", true, true)
		if err != nil {
			return terminalCommandPlan{}, err
		}
		return singleTerminalCommand(action, "-s", session, "type", text), nil
	case "send_line":
		text, err := stringArgWithEmpty(args, "text", "", false, true)
		if err != nil {
			return terminalCommandPlan{}, err
		}
		commands := make([][]string, 0, 2)
		if text != "" {
			commands = append(commands, []string{"-s", session, "type", text})
		}
		commands = append(commands, []string{"-s", session, "press", "enter"})
		return terminalCommandPlan{action: action, commands: commands}, nil
	case "press":
		commands, err := terminalPressCommands(args, session)
		if err != nil {
			return terminalCommandPlan{}, err
		}
		return terminalCommandPlan{action: action, commands: commands}, nil
	case "wait":
		pattern, err := stringArg(args, "pattern", "", true)
		if err != nil {
			return terminalCommandPlan{}, err
		}
		command := []string{"-s", session, "wait", pattern}
		if _, ok := args["timeout_ms"]; ok {
			timeoutMS, err := intArg(args, "timeout_ms", 0, 1, 0)
			if err != nil {
				return terminalCommandPlan{}, err
			}
			command = append(command, "--timeout", strconv.Itoa(timeoutMS))
		}
		return terminalCommandPlan{action: action, commands: [][]string{command}}, nil
	case "wait_idle":
		command := []string{"-s", session, "wait-idle"}
		if _, ok := args["timeout_ms"]; ok {
			timeoutMS, err := intArg(args, "timeout_ms", 0, 1, 0)
			if err != nil {
				return terminalCommandPlan{}, err
			}
			command = append(command, "--timeout", strconv.Itoa(timeoutMS))
		}
		return terminalCommandPlan{action: action, commands: [][]string{command}}, nil
	case "snapshot":
		trim, err := boolArg(args, "trim", true)
		if err != nil {
			return terminalCommandPlan{}, err
		}
		cursor, err := boolArg(args, "cursor", false)
		if err != nil {
			return terminalCommandPlan{}, err
		}
		command := []string{"-s", session, "snapshot"}
		if trim {
			command = append(command, "--trim")
		}
		if !cursor {
			command = append(command, "--no-cursor")
		}
		return terminalCommandPlan{action: action, commands: [][]string{command}}, nil
	case "read":
		readAll, err := boolArg(args, "all", false)
		if err != nil {
			return terminalCommandPlan{}, err
		}
		trim, err := boolArg(args, "trim", true)
		if err != nil {
			return terminalCommandPlan{}, err
		}
		follow, err := boolArg(args, "follow", false)
		if err != nil {
			return terminalCommandPlan{}, err
		}
		command := []string{"-s", session, "read"}
		if readAll {
			command = append(command, "--all")
		}
		if trim {
			command = append(command, "--trim")
		}
		if follow {
			command = append(command, "--follow")
		}
		if _, ok := args["timeout_ms"]; ok {
			timeoutMS, err := intArg(args, "timeout_ms", 0, 1, 0)
			if err != nil {
				return terminalCommandPlan{}, err
			}
			command = append(command, "--timeout", strconv.Itoa(timeoutMS))
		}
		return terminalCommandPlan{action: action, commands: [][]string{command}}, nil
	case "close":
		return singleTerminalCommand(action, "-s", session, "close"), nil
	default:
		return terminalCommandPlan{}, fmt.Errorf("unsupported action %q", action)
	}
}

func singleTerminalCommand(action string, args ...string) terminalCommandPlan {
	return terminalCommandPlan{action: action, commands: [][]string{append([]string(nil), args...)}}
}

func terminalPressCommands(args map[string]any, session string) ([][]string, error) {
	if raw, ok := args["keys"]; ok && raw != nil {
		items, err := stringArrayArg(map[string]any{"keys": raw}, "keys")
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			return nil, fmt.Errorf("keys must include at least one key")
		}
		commands := make([][]string, 0, len(items))
		for index, item := range items {
			keys, err := terminalKeyChord(item)
			if err != nil {
				return nil, fmt.Errorf("keys[%d]: %w", index, err)
			}
			commands = append(commands, append([]string{"-s", session, "press"}, keys...))
		}
		return commands, nil
	}
	key, err := stringArg(args, "key", "", true)
	if err != nil {
		return nil, err
	}
	keys, err := terminalKeyChord(key)
	if err != nil {
		return nil, err
	}
	return [][]string{append([]string{"-s", session, "press"}, keys...)}, nil
}

func terminalKeyChord(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("key must be a non-empty string")
	}
	if key, ok := normalizeTerminalKey(raw); ok {
		return []string{key}, nil
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '+' || r == '-' || r == ' ' || r == '\t'
	})
	keys := make([]string, 0, len(parts))
	for _, part := range parts {
		key, ok := normalizeTerminalKey(part)
		if !ok {
			return nil, fmt.Errorf("unsupported key %q", part)
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("key must be a non-empty string")
	}
	return keys, nil
}

func normalizeTerminalKey(raw string) (string, bool) {
	key := strings.ToLower(strings.TrimSpace(raw))
	if key == "" {
		return "", false
	}
	collapsed := strings.NewReplacer("_", "", "-", "", " ", "").Replace(key)
	switch collapsed {
	case "control":
		return "ctrl", true
	case "command", "cmd", "super":
		return "meta", true
	case "option":
		return "alt", true
	case "return", "newline":
		return "enter", true
	case "escape":
		return "esc", true
	case "spacebar":
		return "space", true
	case "del":
		return "delete", true
	case "backspace", "bksp":
		return "backspace", true
	case "arrowup":
		return "up", true
	case "arrowdown":
		return "down", true
	case "arrowleft":
		return "left", true
	case "arrowright":
		return "right", true
	case "pageup", "pgup":
		return "pageup", true
	case "pagedown", "pgdn":
		return "pagedown", true
	}

	switch key {
	case "ctrl", "alt", "shift", "meta", "up", "down", "left", "right", "home", "end", "pageup", "pagedown", "enter", "return", "esc", "tab", "space", "backspace", "delete", "insert":
		if key == "return" {
			return "enter", true
		}
		return key, true
	}
	if len(key) == 1 {
		ch := key[0]
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') {
			return key, true
		}
	}
	if len(key) >= 2 && key[0] == 'f' {
		n, err := strconv.Atoi(key[1:])
		if err == nil && n >= 1 && n <= 12 {
			return key, true
		}
	}
	return "", false
}

func terminalCommandResult(ctx context.Context, terminal localcontrol.Terminal, plan terminalCommandPlan) Result {
	if len(plan.commands) == 1 {
		return localControlCommandResult(ctx, terminal, "terminal", plan.action, plan.commands[0])
	}

	outputs := make([]string, 0, len(plan.commands))
	meta := map[string]string{"terminal_command": plan.action}
	for index, args := range plan.commands {
		result, err := terminal.Run(ctx, args...)
		output := strings.TrimSpace(result.Output())
		if output != "" {
			outputs = append(outputs, output)
		}
		if result.ExitCode != 0 {
			meta["exit_code"] = strconv.Itoa(result.ExitCode)
		}
		if err != nil {
			message := "Error running terminal helper step " + strconv.Itoa(index+1) + " for " + plan.action + ": " + err.Error()
			if output != "" {
				message += "\n\n" + output
			}
			return Result{
				Status: StatusError,
				Output: message,
				Meta:   meta,
				Display: Display{
					Summary: plan.action + " failed",
					Kind:    "terminal",
				},
			}
		}
	}
	output := strings.Join(outputs, "\n")
	if output == "" {
		output = "terminal command completed."
	}
	return Result{
		Status: StatusOK,
		Output: output,
		Meta:   meta,
		Display: Display{
			Summary: plan.action + " completed",
			Kind:    "terminal",
		},
	}
}
