package tools

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/Gitlawb/zero/internal/localcontrol"
)

const localBrowserOutputBudgetBytes = 128 * 1024

type localControlRunner interface {
	Run(ctx context.Context, args ...string) (localcontrol.CommandResult, error)
}

type browserAppLauncher interface {
	LaunchBrowserApp(ctx context.Context, request localcontrol.BrowserAppLaunchRequest) (localcontrol.BrowserAppLaunchResult, error)
}

func NewLocalBrowserTools(options localcontrol.BrowserOptions) []Tool {
	return []Tool{
		newBrowserInstallTool(options),
		newBrowserLaunchTool(options),
		newBrowserConnectTool(options),
		newBrowserOpenTool(options),
		newBrowserSnapshotTool(options),
		newBrowserClickTool(options),
		newBrowserTypeTool(options),
		newBrowserPressTool(options),
		newBrowserActionTool(options),
	}
}

type browserInstallTool struct {
	baseTool
	browser localcontrol.Browser
}

func newBrowserInstallTool(options localcontrol.BrowserOptions) Tool {
	return browserInstallTool{
		baseTool: baseTool{
			name:        "browser_install",
			deferred:    true,
			description: "Install the local browser runtime used by browser automation.",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"with_deps": {Type: "boolean", Description: "Also install Linux system dependencies when supported by the helper.", Default: false},
				},
				AdditionalProperties: false,
			},
			safety:       localControlSafety(options.Enabled, SideEffectLocalBrowser, "Downloads browser runtime files for local browser automation."),
			capabilities: ToolCapabilities{Effect: EffectInteractive, ThreadSafe: false, ResourceKeys: sessionResourceKeys},
		},
		browser: localcontrol.NewBrowser(options),
	}
}

func (tool browserInstallTool) Run(ctx context.Context, args map[string]any) Result {
	withDeps, err := boolArg(args, "with_deps", false)
	if err != nil {
		return errorResult("Error: Invalid arguments for browser_install: " + err.Error())
	}
	commandArgs := []string{"install"}
	if withDeps {
		commandArgs = append(commandArgs, "--with-deps")
	}
	return browserCommandResult(ctx, tool.browser, "install", commandArgs)
}

type browserLaunchTool struct {
	baseTool
	browser  localcontrol.Browser
	launcher browserAppLauncher
}

func newBrowserLaunchTool(options localcontrol.BrowserOptions) Tool {
	return newBrowserLaunchToolWithLauncher(options, localcontrol.NewBrowserAppLauncher(localcontrol.BrowserAppLaunchOptions{}))
}

func newBrowserLaunchToolWithLauncher(options localcontrol.BrowserOptions, launcher browserAppLauncher) Tool {
	return browserLaunchTool{
		baseTool: baseTool{
			name:        "browser_launch",
			deferred:    true,
			description: "Launch a supported local Chromium/Electron app with Chrome DevTools enabled and attach browser automation to it. Use this for installed Electron apps such as Discord instead of launching GUI apps through shell commands.",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"app":           {Type: "string", Description: "Supported local app to launch.", Enum: []string{"discord"}},
					"debug_port":    {Type: "integer", Description: "Local Chrome DevTools port to expose.", Default: localcontrol.DefaultDevToolsPort, Minimum: intPtr(1024), Maximum: intPtr(65535)},
					"stop_existing": {Type: "boolean", Description: "Stop the supported app before relaunching it with DevTools enabled.", Default: true},
					"wait":          {Type: "boolean", Description: "Wait until the DevTools endpoint responds before returning.", Default: true},
					"connect":       {Type: "boolean", Description: "Attach browser automation to the DevTools endpoint after launch.", Default: true},
				},
				Required:             []string{"app"},
				AdditionalProperties: false,
			},
			safety:       localControlSafety(options.Enabled, SideEffectLocalBrowser, "Launches a supported local Chromium/Electron app with DevTools enabled and attaches local browser automation."),
			capabilities: ToolCapabilities{Effect: EffectInteractive, ThreadSafe: false, ResourceKeys: sessionResourceKeys},
		},
		browser:  localcontrol.NewBrowser(options),
		launcher: launcher,
	}
}

func (tool browserLaunchTool) RejectBeforePermission(args map[string]any) (Result, bool) {
	if _, err := browserLaunchRequestArg(args); err != nil {
		return errorResult("Error: Invalid arguments for browser_launch: " + err.Error()), true
	}
	return Result{}, false
}

func (tool browserLaunchTool) Run(ctx context.Context, args map[string]any) Result {
	request, err := browserLaunchRequestArg(args)
	if err != nil {
		return errorResult("Error: Invalid arguments for browser_launch: " + err.Error())
	}
	result, err := tool.launcher.LaunchBrowserApp(ctx, request)
	if err != nil {
		return errorResult("Error launching browser app: " + err.Error())
	}
	output := fmt.Sprintf("Launched %s with DevTools at %s (pid %d).", result.App, result.DevToolsURL, result.PID)
	connect, err := boolArg(args, "connect", true)
	if err != nil {
		return errorResult("Error: Invalid arguments for browser_launch: " + err.Error())
	}
	if !connect {
		return Result{
			Status: StatusOK,
			Output: output + "\n\nNext call browser_connect with target " + strconv.Itoa(result.DebugPort) + ".",
			Meta: map[string]string{
				"browser_command": "launch",
				"app":             result.App,
				"debug_port":      strconv.Itoa(result.DebugPort),
				"pid":             strconv.Itoa(result.PID),
			},
			Display: Display{Summary: "launch completed", Kind: "browser"},
		}
	}
	connectResult := browserCommandResult(ctx, tool.browser, "connect", []string{"connect", strconv.Itoa(result.DebugPort)})
	if connectResult.Status == StatusOK {
		connectResult.Output = output + "\n\nBrowser automation is already connected; use browser_snapshot next.\n\n" + connectResult.Output
	} else {
		connectResult.Output = output + "\n\n" + connectResult.Output
	}
	if connectResult.Meta == nil {
		connectResult.Meta = map[string]string{}
	}
	connectResult.Meta["app"] = result.App
	connectResult.Meta["debug_port"] = strconv.Itoa(result.DebugPort)
	connectResult.Meta["pid"] = strconv.Itoa(result.PID)
	return connectResult
}

type browserConnectTool struct {
	baseTool
	browser localcontrol.Browser
}

func newBrowserConnectTool(options localcontrol.BrowserOptions) Tool {
	return browserConnectTool{
		baseTool: baseTool{
			name:        "browser_connect",
			deferred:    true,
			description: "Attach local browser automation to an existing Chrome/Electron DevTools endpoint. For Electron apps, first launch the app with --remote-debugging-port=<port>, then connect to that port instead of using desktop control.",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"target": {Type: "string", Description: "CDP target: a port such as 9222, host:port, http(s) URL, ws(s) URL, or DevTools websocket URL."},
				},
				Required:             []string{"target"},
				AdditionalProperties: false,
			},
			safety:       localControlSafety(options.Enabled, SideEffectLocalBrowser, "Attaches to a local browser or Electron app exposed over the Chrome DevTools Protocol."),
			capabilities: ToolCapabilities{Effect: EffectInteractive, ThreadSafe: false, ResourceKeys: sessionResourceKeys},
		},
		browser: localcontrol.NewBrowser(options),
	}
}

func (tool browserConnectTool) RejectBeforePermission(args map[string]any) (Result, bool) {
	if _, err := browserConnectTargetArg(args); err != nil {
		return errorResult("Error: Invalid arguments for browser_connect: " + err.Error()), true
	}
	return Result{}, false
}

func (tool browserConnectTool) Run(ctx context.Context, args map[string]any) Result {
	target, err := browserConnectTargetArg(args)
	if err != nil {
		return errorResult("Error: Invalid arguments for browser_connect: " + err.Error())
	}
	return browserCommandResult(ctx, tool.browser, "connect", []string{"connect", target})
}

type browserOpenTool struct {
	baseTool
	browser localcontrol.Browser
}

func newBrowserOpenTool(options localcontrol.BrowserOptions) Tool {
	return browserOpenTool{
		baseTool: baseTool{
			name:        "browser_open",
			deferred:    true,
			description: "Open a URL in the local browser automation session.",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"url": {Type: "string", Description: "HTTP or HTTPS URL to open. Bare hosts are treated as https URLs."},
				},
				Required:             []string{"url"},
				AdditionalProperties: false,
			},
			safety:       localControlSafety(options.Enabled, SideEffectNetwork, "Opens a URL through the local browser automation helper. For already-running Chrome/Electron apps, use browser_connect instead. If first use reports a missing browser runtime, run browser_install once."),
			capabilities: ToolCapabilities{Effect: EffectInteractive, ThreadSafe: false, ResourceKeys: sessionResourceKeys},
		},
		browser: localcontrol.NewBrowser(options),
	}
}

func (tool browserOpenTool) RejectBeforePermission(args map[string]any) (Result, bool) {
	if _, err := browserOpenURLArg(args); err != nil {
		return errorResult("Error: Invalid arguments for browser_open: " + err.Error()), true
	}
	return Result{}, false
}

func (tool browserOpenTool) Run(ctx context.Context, args map[string]any) Result {
	rawURL, err := browserOpenURLArg(args)
	if err != nil {
		return errorResult("Error: Invalid arguments for browser_open: " + err.Error())
	}
	return browserCommandResult(ctx, tool.browser, "open", []string{"open", rawURL})
}

type browserSnapshotTool struct {
	baseTool
	browser localcontrol.Browser
}

func newBrowserSnapshotTool(options localcontrol.BrowserOptions) Tool {
	return browserSnapshotTool{
		baseTool: baseTool{
			name:        "browser_snapshot",
			deferred:    true,
			description: "Return an accessibility snapshot from the current local browser automation session.",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"interactive":                {Type: "boolean", Description: "Return interactive elements only.", Default: true},
					"include_cursor_interactive": {Type: "boolean", Description: "Include cursor-interactive elements such as onclick containers.", Default: false},
					"compact":                    {Type: "boolean", Description: "Use compact snapshot output.", Default: false},
					"depth":                      {Type: "integer", Description: "Optional tree depth limit.", Minimum: intPtr(0), Maximum: intPtr(20)},
					"selector":                   {Type: "string", Description: "Optional CSS selector to scope the snapshot."},
				},
				AdditionalProperties: false,
			},
			safety:       localControlSafety(options.Enabled, SideEffectLocalBrowser, "Reads the current local browser automation session state."),
			capabilities: ToolCapabilities{Effect: EffectInteractive, ThreadSafe: false, ResourceKeys: sessionResourceKeys},
		},
		browser: localcontrol.NewBrowser(options),
	}
}

func (tool browserSnapshotTool) Run(ctx context.Context, args map[string]any) Result {
	commandArgs, err := browserSnapshotCommandArgs(args)
	if err != nil {
		return errorResult("Error: Invalid arguments for browser_snapshot: " + err.Error())
	}
	return browserCommandResult(ctx, tool.browser, "snapshot", commandArgs)
}

func (tool browserSnapshotTool) RejectBeforePermission(args map[string]any) (Result, bool) {
	if _, err := browserSnapshotCommandArgs(args); err != nil {
		return errorResult("Error: Invalid arguments for browser_snapshot: " + err.Error()), true
	}
	return Result{}, false
}

type browserClickTool struct {
	baseTool
	browser localcontrol.Browser
}

func newBrowserClickTool(options localcontrol.BrowserOptions) Tool {
	return browserClickTool{
		baseTool: baseTool{
			name:        "browser_click",
			deferred:    true,
			description: "Click a ref from browser_snapshot in the current local browser automation session.",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"ref": {Type: "string", Description: "Element ref from browser_snapshot, for example e114."},
				},
				Required:             []string{"ref"},
				AdditionalProperties: false,
			},
			safety:       localControlSafety(options.Enabled, SideEffectLocalBrowser, "Clicks an element in the local browser automation session."),
			capabilities: ToolCapabilities{Effect: EffectInteractive, ThreadSafe: false, ResourceKeys: sessionResourceKeys},
		},
		browser: localcontrol.NewBrowser(options),
	}
}

func (tool browserClickTool) RejectBeforePermission(args map[string]any) (Result, bool) {
	if _, err := browserRefArg(args); err != nil {
		return errorResult("Error: Invalid arguments for browser_click: " + err.Error()), true
	}
	return Result{}, false
}

func (tool browserClickTool) Run(ctx context.Context, args map[string]any) Result {
	ref, err := browserRefArg(args)
	if err != nil {
		return errorResult("Error: Invalid arguments for browser_click: " + err.Error())
	}
	return browserCommandResult(ctx, tool.browser, "click", []string{"click", ref})
}

type browserTypeTool struct {
	baseTool
	browser localcontrol.Browser
}

func newBrowserTypeTool(options localcontrol.BrowserOptions) Tool {
	return browserTypeTool{
		baseTool: baseTool{
			name:        "browser_type",
			deferred:    true,
			description: "Type text into a ref from browser_snapshot in the current local browser automation session.",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"ref":  {Type: "string", Description: "Element ref from browser_snapshot, for example e114."},
					"text": {Type: "string", Description: "Text to type into the target element."},
				},
				Required:             []string{"ref", "text"},
				AdditionalProperties: false,
			},
			safety:       localControlSafety(options.Enabled, SideEffectLocalBrowser, "Types text into an element in the local browser automation session."),
			capabilities: ToolCapabilities{Effect: EffectInteractive, ThreadSafe: false, ResourceKeys: sessionResourceKeys},
		},
		browser: localcontrol.NewBrowser(options),
	}
}

func (tool browserTypeTool) RejectBeforePermission(args map[string]any) (Result, bool) {
	if _, _, err := browserTypeArgs(args); err != nil {
		return errorResult("Error: Invalid arguments for browser_type: " + err.Error()), true
	}
	return Result{}, false
}

func (tool browserTypeTool) Run(ctx context.Context, args map[string]any) Result {
	ref, text, err := browserTypeArgs(args)
	if err != nil {
		return errorResult("Error: Invalid arguments for browser_type: " + err.Error())
	}
	return browserCommandResult(ctx, tool.browser, "type", []string{"type", ref, text})
}

type browserPressTool struct {
	baseTool
	browser localcontrol.Browser
}

func newBrowserPressTool(options localcontrol.BrowserOptions) Tool {
	return browserPressTool{
		baseTool: baseTool{
			name:        "browser_press",
			deferred:    true,
			description: "Press a keyboard key in the current local browser automation session.",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"key": {Type: "string", Description: "Key to press, for example Enter, Escape, or Control+L."},
				},
				Required:             []string{"key"},
				AdditionalProperties: false,
			},
			safety:       localControlSafety(options.Enabled, SideEffectLocalBrowser, "Presses a key in the local browser automation session."),
			capabilities: ToolCapabilities{Effect: EffectInteractive, ThreadSafe: false, ResourceKeys: sessionResourceKeys},
		},
		browser: localcontrol.NewBrowser(options),
	}
}

func (tool browserPressTool) RejectBeforePermission(args map[string]any) (Result, bool) {
	if _, err := browserKeyArg(args); err != nil {
		return errorResult("Error: Invalid arguments for browser_press: " + err.Error()), true
	}
	return Result{}, false
}

func (tool browserPressTool) Run(ctx context.Context, args map[string]any) Result {
	key, err := browserKeyArg(args)
	if err != nil {
		return errorResult("Error: Invalid arguments for browser_press: " + err.Error())
	}
	return browserCommandResult(ctx, tool.browser, "press", []string{"press", key})
}

type browserActionTool struct {
	baseTool
	browser localcontrol.Browser
}

func browserSnapshotCommandArgs(args map[string]any) ([]string, error) {
	interactive, err := boolArg(args, "interactive", true)
	if err != nil {
		return nil, err
	}
	includeCursorInteractive, err := boolArg(args, "include_cursor_interactive", false)
	if err != nil {
		return nil, err
	}
	compact, err := boolArg(args, "compact", false)
	if err != nil {
		return nil, err
	}
	depth, err := intArg(args, "depth", 0, 0, 20)
	if err != nil {
		return nil, err
	}
	selector, err := stringArgWithEmpty(args, "selector", "", false, false)
	if err != nil {
		return nil, err
	}
	selector = strings.TrimSpace(selector)
	if err := validateBrowserSnapshotSelector(selector); err != nil {
		return nil, err
	}

	commandArgs := []string{"snapshot"}
	if interactive {
		commandArgs = append(commandArgs, "-i")
	}
	if includeCursorInteractive {
		commandArgs = append(commandArgs, "-C")
	}
	if compact {
		commandArgs = append(commandArgs, "-c")
	}
	if depth > 0 {
		commandArgs = append(commandArgs, "-d", strconv.Itoa(depth))
	}
	if selector != "" {
		commandArgs = append(commandArgs, "-s", selector)
	}
	return commandArgs, nil
}

func newBrowserActionTool(options localcontrol.BrowserOptions) Tool {
	return browserActionTool{
		baseTool: baseTool{
			name:        "browser_action",
			deferred:    true,
			description: "Run an allowed action against the current local browser automation session.",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"command": {Type: "string", Description: "Allowed browser command to run.", Enum: browserActionCommandNames()},
					"args":    {Type: "array", Items: &PropertySchema{Type: "string"}, Description: "Command arguments. Use refs from browser_snapshot where required."},
				},
				Required:             []string{"command"},
				AdditionalProperties: false,
			},
			safety:       localControlSafety(options.Enabled, SideEffectLocalBrowser, "Interacts with the local browser automation session."),
			capabilities: ToolCapabilities{Effect: EffectInteractive, ThreadSafe: false, ResourceKeys: sessionResourceKeys},
		},
		browser: localcontrol.NewBrowser(options),
	}
}

func (tool browserActionTool) RejectBeforePermission(args map[string]any) (Result, bool) {
	if _, _, err := browserActionArgs(args); err != nil {
		return errorResult("Error: Invalid arguments for browser_action: " + err.Error()), true
	}
	return Result{}, false
}

func (tool browserActionTool) Run(ctx context.Context, args map[string]any) Result {
	command, commandArgs, err := browserActionArgs(args)
	if err != nil {
		return errorResult("Error: Invalid arguments for browser_action: " + err.Error())
	}
	return browserCommandResult(ctx, tool.browser, command, commandArgs)
}

type browserActionSpec struct {
	argv []string
	min  int
	max  int
}

var browserActionSpecs = map[string]browserActionSpec{
	"back":                 {argv: []string{"back"}, min: 0, max: 0},
	"forward":              {argv: []string{"forward"}, min: 0, max: 0},
	"reload":               {argv: []string{"reload"}, min: 0, max: 0},
	"close":                {argv: []string{"close"}, min: 0, max: 0},
	"connect":              {argv: []string{"connect"}, min: 1, max: 1},
	"click":                {argv: []string{"click"}, min: 1, max: 1},
	"dblclick":             {argv: []string{"dblclick"}, min: 1, max: 1},
	"fill":                 {argv: []string{"fill"}, min: 2, max: 2},
	"type":                 {argv: []string{"type"}, min: 2, max: 2},
	"press":                {argv: []string{"press"}, min: 1, max: 1},
	"keyboard_type":        {argv: []string{"keyboard", "type"}, min: 1, max: 1},
	"keyboard_insert_text": {argv: []string{"keyboard", "inserttext"}, min: 1, max: 1},
	"hover":                {argv: []string{"hover"}, min: 1, max: 1},
	"check":                {argv: []string{"check"}, min: 1, max: 1},
	"uncheck":              {argv: []string{"uncheck"}, min: 1, max: 1},
	"select":               {argv: []string{"select"}, min: 2, max: 2},
	"scroll":               {argv: []string{"scroll"}, min: 2, max: 2},
	"scroll_into_view":     {argv: []string{"scrollintoview"}, min: 1, max: 1},
	"drag":                 {argv: []string{"drag"}, min: 2, max: 2},
	"wait":                 {argv: []string{"wait"}, min: 1, max: 3},
	"get_text":             {argv: []string{"get", "text"}, min: 1, max: 1},
	"get_html":             {argv: []string{"get", "html"}, min: 1, max: 1},
	"get_value":            {argv: []string{"get", "value"}, min: 1, max: 1},
	"get_attr":             {argv: []string{"get", "attr"}, min: 2, max: 2},
	"get_title":            {argv: []string{"get", "title"}, min: 0, max: 0},
	"get_url":              {argv: []string{"get", "url"}, min: 0, max: 0},
	"tab":                  {argv: []string{"tab"}, min: 0, max: 3},
	"eval":                 {argv: []string{"eval"}, min: 1, max: 1},
	"dialog_accept":        {argv: []string{"dialog", "accept"}, min: 0, max: 1},
	"dialog_dismiss":       {argv: []string{"dialog", "dismiss"}, min: 0, max: 0},
}

func browserActionCommandNames() []string {
	names := make([]string, 0, len(browserActionSpecs))
	for name := range browserActionSpecs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func browserActionArgs(args map[string]any) (string, []string, error) {
	command, err := stringArg(args, "command", "", true)
	if err != nil {
		return "", nil, err
	}
	command, ok := NormalizedBrowserActionCommand(command)
	if !ok {
		return "", nil, fmt.Errorf("command must be one of: %s", strings.Join(browserActionCommandNames(), ", "))
	}
	spec := browserActionSpecs[command]
	values, err := stringArrayArg(args, "args")
	if err != nil {
		return "", nil, err
	}
	if len(values) < spec.min || len(values) > spec.max {
		if spec.min == spec.max {
			return "", nil, fmt.Errorf("%s requires %d args", command, spec.min)
		}
		return "", nil, fmt.Errorf("%s requires between %d and %d args", command, spec.min, spec.max)
	}
	commandArgs, err := browserActionCommandArgs(command, spec, values)
	if err != nil {
		return "", nil, err
	}
	return command, commandArgs, nil
}

// NormalizedBrowserActionCommand returns the exact action that browser_action
// will execute after normalizing its command argument. ACP uses it only for a
// permission title, so an unrecognised command is never reflected as text that
// the tool would reject.
func NormalizedBrowserActionCommand(command string) (string, bool) {
	command = strings.ToLower(strings.TrimSpace(command))
	_, ok := browserActionSpecs[command]
	return command, ok
}

func browserActionCommandArgs(command string, spec browserActionSpec, values []string) ([]string, error) {
	switch command {
	case "connect":
		target, err := browserConnectTargetFromString(values[0])
		if err != nil {
			return nil, err
		}
		commandArgs := append([]string{}, spec.argv...)
		return append(commandArgs, target), nil
	case "click", "dblclick", "hover", "check", "uncheck", "scroll_into_view", "get_text", "get_html", "get_value":
		ref, err := browserRefFromString(values[0])
		if err != nil {
			return nil, err
		}
		commandArgs := append([]string{}, spec.argv...)
		return append(commandArgs, ref), nil
	case "fill", "type", "select":
		ref, err := browserRefFromString(values[0])
		if err != nil {
			return nil, err
		}
		commandArgs := append([]string{}, spec.argv...)
		commandArgs = append(commandArgs, ref)
		commandArgs = append(commandArgs, values[1:]...)
		return commandArgs, nil
	case "drag":
		fromRef, err := browserRefFromString(values[0])
		if err != nil {
			return nil, err
		}
		toRef, err := browserRefFromString(values[1])
		if err != nil {
			return nil, err
		}
		commandArgs := append([]string{}, spec.argv...)
		return append(commandArgs, fromRef, toRef), nil
	case "press":
		key, err := browserKeyFromString(values[0])
		if err != nil {
			return nil, err
		}
		commandArgs := append([]string{}, spec.argv...)
		return append(commandArgs, key), nil
	case "get_attr":
		ref, err := browserRefFromString(values[0])
		if err != nil {
			return nil, err
		}
		attr, err := browserSingleToken("attr", values[1])
		if err != nil {
			return nil, err
		}
		commandArgs := append([]string{}, spec.argv...)
		return append(commandArgs, ref, attr), nil
	case "scroll":
		x, err := browserIntegerToken("x", values[0])
		if err != nil {
			return nil, err
		}
		y, err := browserIntegerToken("y", values[1])
		if err != nil {
			return nil, err
		}
		commandArgs := append([]string{}, spec.argv...)
		return append(commandArgs, x, y), nil
	case "tab":
		commandArgs := append([]string{}, spec.argv...)
		for _, value := range values {
			arg, err := browserSingleToken("tab argument", value)
			if err != nil {
				return nil, err
			}
			commandArgs = append(commandArgs, arg)
		}
		return commandArgs, nil
	default:
		commandArgs := append([]string{}, spec.argv...)
		commandArgs = append(commandArgs, values...)
		return commandArgs, nil
	}
}

func browserOpenURLArg(args map[string]any) (string, error) {
	rawURL, err := stringArg(args, "url", "", true)
	if err != nil {
		return "", err
	}
	return NormalizeBrowserOpenURL(rawURL)
}

// NormalizeBrowserOpenURL applies the browser_open URL rules before execution.
// Keeping this exported within the internal package lets permission displays
// describe the same destination the browser helper will open.
func NormalizeBrowserOpenURL(rawURL string) (string, error) {
	normalized := strings.TrimSpace(rawURL)
	if !strings.Contains(normalized, "://") {
		normalized = "https://" + normalized
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("url must use http or https")
	}
	if strings.TrimSpace(parsed.Host) == "" {
		return "", fmt.Errorf("url must include a host")
	}
	return parsed.String(), nil
}

func browserConnectTargetArg(args map[string]any) (string, error) {
	target, err := stringArg(args, "target", "", true)
	if err != nil {
		return "", err
	}
	return browserConnectTargetFromString(target)
}

func browserConnectTargetFromString(target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", fmt.Errorf("target is required")
	}
	if strings.ContainsAny(target, " \t\r\n;") {
		return "", fmt.Errorf("target must not contain whitespace or semicolons")
	}
	if strings.HasPrefix(target, "-") {
		return "", fmt.Errorf("target must be a CDP port or URL, not an option")
	}
	if port, err := strconv.Atoi(target); err == nil && port >= 1 && port <= 65535 {
		return target, nil
	}
	host, err := browserConnectTargetHost(target)
	if err != nil {
		return "", err
	}
	if !isLoopbackHost(host) {
		return "", fmt.Errorf("target must be a bare port or loopback host")
	}
	return target, nil
}

func browserConnectTargetHost(target string) (string, error) {
	parsed, err := url.Parse(target)
	if err == nil && parsed.Host != "" {
		if !validBrowserPort(parsed.Port()) {
			return "", fmt.Errorf("target URL must include a valid port")
		}
		return parsed.Hostname(), nil
	}
	if !strings.Contains(target, "://") {
		parsed, err = url.Parse("http://" + target)
		if err == nil && parsed.Host != "" {
			if !validBrowserPort(parsed.Port()) {
				return "", fmt.Errorf("target must include a valid port")
			}
			return parsed.Hostname(), nil
		}
	}
	return "", fmt.Errorf("target must be a bare port or loopback host:port/URL")
}

func validBrowserPort(raw string) bool {
	if raw == "" {
		return false
	}
	port, err := strconv.Atoi(raw)
	return err == nil && port >= 1 && port <= 65535
}

func isLoopbackHost(host string) bool {
	host = strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func browserRefArg(args map[string]any) (string, error) {
	ref, err := stringArg(args, "ref", "", true)
	if err != nil {
		return "", err
	}
	return browserRefFromString(ref)
}

func browserRefFromString(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("ref is required")
	}
	if strings.ContainsAny(ref, " \t\r\n;") {
		return "", fmt.Errorf("ref must be a single browser_snapshot ref")
	}
	if strings.HasPrefix(ref, "-") {
		return "", fmt.Errorf("ref must be a browser_snapshot ref, not an option")
	}
	return ref, nil
}

func browserTypeArgs(args map[string]any) (string, string, error) {
	ref, err := browserRefArg(args)
	if err != nil {
		return "", "", err
	}
	text, err := stringArg(args, "text", "", true)
	if err != nil {
		return "", "", err
	}
	if text == "" {
		return "", "", fmt.Errorf("text must be a non-empty string")
	}
	return ref, text, nil
}

func browserKeyArg(args map[string]any) (string, error) {
	key, err := stringArg(args, "key", "", true)
	if err != nil {
		return "", err
	}
	return browserKeyFromString(key)
}

func browserKeyFromString(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", fmt.Errorf("key is required")
	}
	if strings.ContainsAny(key, " \t\r\n;") {
		return "", fmt.Errorf("key must be a single key name")
	}
	if strings.HasPrefix(key, "-") {
		return "", fmt.Errorf("key must be a key name, not an option")
	}
	return key, nil
}

func browserSingleToken(name string, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	if strings.ContainsAny(value, " \t\r\n;") {
		return "", fmt.Errorf("%s must be a single token", name)
	}
	if strings.HasPrefix(value, "-") {
		return "", fmt.Errorf("%s must not be an option", name)
	}
	return value, nil
}

func browserIntegerToken(name string, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	if _, err := strconv.Atoi(value); err != nil {
		return "", fmt.Errorf("%s must be an integer", name)
	}
	return value, nil
}

func validateBrowserSnapshotSelector(selector string) error {
	if selector == "" {
		return nil
	}
	if strings.Contains(selector, "]*") {
		return fmt.Errorf("selector appears invalid: remove the trailing wildcard after the attribute selector")
	}
	if strings.ContainsAny(selector, "\r\n") {
		return fmt.Errorf("selector must be a single line")
	}
	return nil
}

func browserLaunchRequestArg(args map[string]any) (localcontrol.BrowserAppLaunchRequest, error) {
	app, err := stringArg(args, "app", "", true)
	if err != nil {
		return localcontrol.BrowserAppLaunchRequest{}, err
	}
	app = strings.ToLower(strings.TrimSpace(app))
	switch app {
	case "discord":
	default:
		return localcontrol.BrowserAppLaunchRequest{}, fmt.Errorf("app must be one of: discord")
	}
	port, err := intArg(args, "debug_port", localcontrol.DefaultDevToolsPort, 1024, 65535)
	if err != nil {
		return localcontrol.BrowserAppLaunchRequest{}, err
	}
	stopExisting, err := boolArg(args, "stop_existing", true)
	if err != nil {
		return localcontrol.BrowserAppLaunchRequest{}, err
	}
	wait, err := boolArg(args, "wait", true)
	if err != nil {
		return localcontrol.BrowserAppLaunchRequest{}, err
	}
	if _, err := boolArg(args, "connect", true); err != nil {
		return localcontrol.BrowserAppLaunchRequest{}, err
	}
	return localcontrol.BrowserAppLaunchRequest{
		App:          app,
		DebugPort:    port,
		StopExisting: stopExisting,
		Wait:         wait,
	}, nil
}

func stringArrayArg(args map[string]any, key string) ([]string, error) {
	value, ok := args[key]
	if !ok || value == nil {
		return nil, nil
	}
	raw, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array of strings", key)
	}
	values := make([]string, 0, len(raw))
	for index, item := range raw {
		text, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("%s[%d] must be a string", key, index)
		}
		values = append(values, text)
	}
	return values, nil
}

func localControlSafety(enabled bool, sideEffect SideEffect, reason string) Safety {
	if !enabled {
		return Safety{
			SideEffect: sideEffect,
			Permission: PermissionDeny,
			Reason:     "Local control is disabled.",
		}
	}
	return promptSafety(sideEffect, reason)
}

func browserCommandResult(ctx context.Context, browser localcontrol.Browser, command string, args []string) Result {
	return localControlCommandResult(ctx, browser, "browser", command, args)
}

func localControlCommandResult(ctx context.Context, runner localControlRunner, kind string, command string, args []string) Result {
	result, err := runner.Run(ctx, args...)
	output := result.Output()
	budget := applyOutputBudget(output, localBrowserOutputBudgetBytes, "narrow the local-control query or request a scoped snapshot")
	meta := outputBudgetMeta(budget)
	meta[kind+"_command"] = command
	if result.ExitCode != 0 {
		meta["exit_code"] = strconv.Itoa(result.ExitCode)
	}
	if err != nil {
		message := "Error running " + kind + " helper: " + err.Error()
		if strings.TrimSpace(budget.Output) != "" {
			message += "\n\n" + budget.Output
		}
		if kind == "browser" && command != "install" && browserInstallHintApplies(err.Error()+"\n"+budget.Output) {
			message += "\n\nIf this is a first-use browser runtime error, call browser_install once, then retry the browser command."
		}
		return Result{
			Status:    StatusError,
			Output:    message,
			Truncated: budget.Truncated,
			Meta:      meta,
			Display: Display{
				Summary: command + " failed",
				Kind:    kind,
			},
		}
	}
	if strings.TrimSpace(budget.Output) == "" {
		budget.Output = kind + " command completed."
	}
	return Result{
		Status:    StatusOK,
		Output:    budget.Output,
		Truncated: budget.Truncated,
		Meta:      meta,
		Display: Display{
			Summary: command + " completed",
			Kind:    kind,
		},
	}
}

func browserInstallHintApplies(message string) bool {
	lower := strings.ToLower(message)
	if strings.Contains(lower, "cdp error") {
		return false
	}
	if strings.Contains(lower, "was not found on path") {
		return true
	}
	if strings.Contains(lower, "executable doesn't exist") {
		return true
	}
	if strings.Contains(lower, "browser runtime") {
		return true
	}
	if strings.Contains(lower, "please run") && strings.Contains(lower, "install") && strings.Contains(lower, "browser") {
		return true
	}
	if strings.Contains(lower, "could not find") && (strings.Contains(lower, "chromium") || strings.Contains(lower, "chrome") || strings.Contains(lower, "browser")) {
		return true
	}
	return false
}
