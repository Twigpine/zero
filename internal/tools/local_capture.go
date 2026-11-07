package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Twigpine/zero/internal/imageinput"
	"github.com/Twigpine/zero/internal/zeroruntime"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Twigpine/zero/internal/localcontrol"
)

const defaultArtifactBaseName = "capture"

type LocalControlArtifactOptions struct {
	Browser      localcontrol.BrowserOptions
	Desktop      localcontrol.DesktopOptions
	Terminal     localcontrol.TerminalOptions
	ArtifactsDir string
}

func NewLocalControlArtifactTools(options LocalControlArtifactOptions) []Tool {
	return []Tool{newCaptureArtifactTool(options)}
}

type captureArtifactTool struct {
	baseTool
	browser      localcontrol.Browser
	desktop      localcontrol.Desktop
	terminal     localcontrol.Terminal
	artifactsDir string
}

func newCaptureArtifactTool(options LocalControlArtifactOptions) Tool {
	enabled := strings.TrimSpace(options.ArtifactsDir) != "" && (options.Browser.Enabled || options.Desktop.Enabled || options.Terminal.Enabled)
	return captureArtifactTool{
		baseTool: baseTool{
			name:        "capture_artifact",
			deferred:    true,
			description: "Capture local browser, desktop, or terminal state into the configured artifact directory.",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"action":    {Type: "string", Description: "Capture action to run.", Enum: captureArtifactActionNames()},
					"name":      {Type: "string", Description: "Optional artifact basename. Path separators are ignored."},
					"full":      {Type: "boolean", Description: "Capture a full-page browser screenshot for browser_screenshot.", Default: false},
					"annotate":  {Type: "boolean", Description: "Annotate browser screenshots with interactive element labels.", Default: false},
					"pid":       {Type: "integer", Description: "Target process id for desktop_window_state.", Minimum: intPtr(1)},
					"window_id": {Type: "integer", Description: "Target window id for desktop_window_state.", Minimum: intPtr(1)},
					"query":     {Type: "string", Description: "Optional desktop snapshot query/filter."},
					"session":   {Type: "string", Description: "Terminal session id for terminal_snapshot, or desktop session id for desktop_window_state."},
					"trim":      {Type: "boolean", Description: "Trim trailing blank terminal screen lines for terminal_snapshot.", Default: true},
				},
				Required:             []string{"action"},
				AdditionalProperties: false,
			},
			safety: localControlSafety(enabled, SideEffectLocalControl, "Captures local browser, desktop, or terminal state into the configured artifact directory."),
			// Captures write artifacts under the configured directory.
			capabilities: ToolCapabilities{Effect: EffectWorkspaceWrite, ThreadSafe: false},
		},
		browser:      localcontrol.NewBrowser(options.Browser),
		desktop:      localcontrol.NewDesktop(options.Desktop),
		terminal:     localcontrol.NewTerminal(options.Terminal),
		artifactsDir: options.ArtifactsDir,
	}
}

func (tool captureArtifactTool) RejectBeforePermission(args map[string]any) (Result, bool) {
	request, err := captureArtifactArgs(args)
	if err != nil {
		return errorResult("Error: Invalid arguments for capture_artifact: " + err.Error()), true
	}
	if strings.TrimSpace(tool.artifactsDir) == "" {
		return errorResult("Error: capture_artifact is disabled because no artifact directory is configured."), true
	}
	if !tool.actionEnabled(request.action) {
		return errorResult("Error: Local control driver for " + request.action + " is disabled."), true
	}
	return Result{}, false
}

func (tool captureArtifactTool) Run(ctx context.Context, args map[string]any) Result {
	request, err := captureArtifactArgs(args)
	if err != nil {
		return errorResult("Error: Invalid arguments for capture_artifact: " + err.Error())
	}
	if !tool.actionEnabled(request.action) {
		return errorResult("Error: Local control driver for " + request.action + " is disabled.")
	}
	if strings.TrimSpace(tool.artifactsDir) == "" {
		return errorResult("Error: capture_artifact is disabled because no artifact directory is configured.")
	}

	path, err := artifactOutputPath(tool.artifactsDir, request.name, request.extension())
	if err != nil {
		return errorResult("Error: Failed to prepare artifact path: " + err.Error())
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return errorResult("Error: Failed to create artifact directory: " + err.Error())
	}

	switch request.action {
	case "browser_screenshot":
		return tool.captureBrowserScreenshot(ctx, request, path)
	case "browser_pdf":
		return tool.captureBrowserPDF(ctx, request, path)
	case "desktop_screenshot":
		return tool.captureDesktopScreenshot(ctx, request, path)
	case "desktop_window_state":
		return tool.captureDesktopWindowState(ctx, request, path)
	case "terminal_snapshot":
		return tool.captureTerminalSnapshot(ctx, request, path)
	default:
		return errorResult("Error: Unsupported capture action " + request.action)
	}
}

func (tool captureArtifactTool) actionEnabled(action string) bool {
	switch actionDriver(action) {
	case "browser":
		return tool.browser.Enabled()
	case "desktop":
		return tool.desktop.Enabled()
	case "terminal":
		return tool.terminal.Enabled()
	default:
		return false
	}
}

func (tool captureArtifactTool) captureBrowserScreenshot(ctx context.Context, request captureArtifactRequest, path string) Result {
	commandArgs := []string{"screenshot"}
	if request.full {
		commandArgs = append(commandArgs, "--full")
	}
	if request.annotate {
		commandArgs = append(commandArgs, "--annotate")
	}
	commandArgs = append(commandArgs, path)
	return tool.captureHelperArtifact(ctx, tool.browser, "browser", request, path, commandArgs)
}

func (tool captureArtifactTool) captureBrowserPDF(ctx context.Context, request captureArtifactRequest, path string) Result {
	return tool.captureHelperArtifact(ctx, tool.browser, "browser", request, path, []string{"pdf", path})
}

func (tool captureArtifactTool) captureDesktopScreenshot(ctx context.Context, request captureArtifactRequest, path string) Result {
	payload, err := json.Marshal(map[string]any{"out_file": path})
	if err != nil {
		return errorResult("Error: Failed to encode desktop screenshot input: " + err.Error())
	}
	return tool.captureHelperArtifact(ctx, tool.desktop, "desktop", request, path, []string{"screenshot", string(payload)})
}

func (tool captureArtifactTool) captureDesktopWindowState(ctx context.Context, request captureArtifactRequest, path string) Result {
	input := map[string]any{"pid": request.pid, "window_id": request.windowID}
	if strings.TrimSpace(request.query) != "" {
		input["query"] = request.query
	}
	if strings.TrimSpace(request.session) != "" {
		input["session"] = request.session
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return errorResult("Error: Failed to encode desktop window state input: " + err.Error())
	}
	return tool.captureHelperArtifact(ctx, tool.desktop, "desktop", request, path, []string{"get_window_state", string(payload), "--screenshot-out-file", path})
}

func (tool captureArtifactTool) captureTerminalSnapshot(ctx context.Context, request captureArtifactRequest, path string) Result {
	commandArgs := []string{"-s", request.session, "snapshot"}
	if request.trim {
		commandArgs = append(commandArgs, "--trim")
	}
	result, err := tool.terminal.Run(ctx, commandArgs...)
	output := result.Output()
	if err != nil {
		return captureErrorResult("terminal", request, result, err)
	}
	if err := os.WriteFile(path, []byte(output), 0o600); err != nil {
		return errorResult("Error: Failed to write terminal artifact: " + err.Error())
	}
	if err := writeArtifactMetadata(path, artifactMetadata{
		Action: request.action,
		Driver: "terminal",
		Path:   path,
		Args:   commandArgs,
	}); err != nil {
		return errorResult("Error: Failed to write artifact metadata: " + err.Error())
	}
	return captureOKResult("terminal", request, path, result)
}

func (tool captureArtifactTool) captureHelperArtifact(ctx context.Context, runner localControlRunner, driver string, request captureArtifactRequest, path string, args []string) Result {
	result, err := runner.Run(ctx, args...)
	if err != nil {
		return captureErrorResult(driver, request, result, err)
	}
	if _, err := os.Stat(path); err != nil {
		return errorResult("Error: Helper completed but artifact was not written: " + err.Error())
	}
	if err := writeArtifactMetadata(path, artifactMetadata{
		Action: request.action,
		Driver: driver,
		Path:   path,
		Args:   args,
	}); err != nil {
		return errorResult("Error: Failed to write artifact metadata: " + err.Error())
	}
	return captureOKResult(driver, request, path, result)
}

type captureArtifactRequest struct {
	action   string
	name     string
	full     bool
	annotate bool
	pid      int
	windowID int
	query    string
	session  string
	trim     bool
}

func (request captureArtifactRequest) extension() string {
	switch request.action {
	case "browser_pdf":
		return ".pdf"
	case "terminal_snapshot":
		return ".txt"
	default:
		return ".png"
	}
}

var captureArtifactActions = map[string]bool{
	"browser_screenshot":   true,
	"browser_pdf":          true,
	"desktop_screenshot":   true,
	"desktop_window_state": true,
	"terminal_snapshot":    true,
}

func captureArtifactActionNames() []string {
	names := make([]string, 0, len(captureArtifactActions))
	for name := range captureArtifactActions {
		names = append(names, name)
	}
	sortStrings(names)
	return names
}

func captureArtifactArgs(args map[string]any) (captureArtifactRequest, error) {
	action, err := stringArg(args, "action", "", true)
	if err != nil {
		return captureArtifactRequest{}, err
	}
	action = strings.ToLower(strings.TrimSpace(action))
	if !captureArtifactActions[action] {
		return captureArtifactRequest{}, fmt.Errorf("action must be one of: %s", strings.Join(captureArtifactActionNames(), ", "))
	}
	name, err := stringArgWithEmpty(args, "name", "", false, false)
	if err != nil {
		return captureArtifactRequest{}, err
	}
	request := captureArtifactRequest{action: action, name: name, trim: true}
	switch action {
	case "browser_screenshot":
		request.full, err = boolArg(args, "full", false)
		if err != nil {
			return captureArtifactRequest{}, err
		}
		request.annotate, err = boolArg(args, "annotate", false)
		if err != nil {
			return captureArtifactRequest{}, err
		}
	case "desktop_window_state":
		request.pid, err = requiredIntArg(args, "pid", 1, 0)
		if err != nil {
			return captureArtifactRequest{}, err
		}
		request.windowID, err = requiredIntArg(args, "window_id", 1, 0)
		if err != nil {
			return captureArtifactRequest{}, err
		}
		request.query, err = stringArgWithEmpty(args, "query", "", false, false)
		if err != nil {
			return captureArtifactRequest{}, err
		}
		request.session, err = stringArgWithEmpty(args, "session", "", false, false)
		if err != nil {
			return captureArtifactRequest{}, err
		}
	case "terminal_snapshot":
		request.session, err = stringArg(args, "session", "", true)
		if err != nil {
			return captureArtifactRequest{}, err
		}
		request.trim, err = boolArg(args, "trim", true)
		if err != nil {
			return captureArtifactRequest{}, err
		}
	}
	return request, nil
}

func actionDriver(action string) string {
	switch {
	case strings.HasPrefix(action, "browser_"):
		return "browser"
	case strings.HasPrefix(action, "desktop_"):
		return "desktop"
	case strings.HasPrefix(action, "terminal_"):
		return "terminal"
	default:
		return ""
	}
}

type artifactMetadata struct {
	Action    string    `json:"action"`
	Driver    string    `json:"driver"`
	Path      string    `json:"path"`
	Args      []string  `json:"args"`
	CreatedAt time.Time `json:"createdAt"`
}

func writeArtifactMetadata(path string, metadata artifactMetadata) error {
	metadata.CreatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path+".json", data, 0o600)
}

func captureOKResult(driver string, request captureArtifactRequest, path string, helperResult localcontrol.CommandResult) Result {
	output := "Artifact captured: " + path
	helperOutput := strings.TrimSpace(helperResult.Output())
	if helperOutput != "" {
		output += "\n\n" + helperOutput
	}
	// Attach the artifact itself. Without this the model is told a screenshot
	// exists and cannot look at it, which is the whole point of asking for one.
	// A PDF or an unreadable file simply yields no image and the text stands on
	// its own, so a capture is never failed over the attachment.
	var images []zeroruntime.ImageBlock
	if image, err := imageinput.LoadFile(path, ""); err == nil {
		images = []zeroruntime.ImageBlock{image}
	}
	return Result{
		Status: StatusOK,
		Output: output,
		Images: images,
		Meta: map[string]string{
			"artifact_path": path,
			"action":        request.action,
			"driver":        driver,
		},
		ChangedFiles: []string{path, path + ".json"},
		Display: Display{
			Summary: request.action + " captured",
			Kind:    "artifact",
		},
	}
}

func captureErrorResult(driver string, request captureArtifactRequest, result localcontrol.CommandResult, err error) Result {
	output := "Error running " + driver + " helper: " + err.Error()
	if helperOutput := strings.TrimSpace(result.Output()); helperOutput != "" {
		output += "\n\n" + helperOutput
	}
	meta := map[string]string{
		"action": request.action,
		"driver": driver,
	}
	if result.ExitCode != 0 {
		meta["exit_code"] = strconv.Itoa(result.ExitCode)
	}
	return Result{
		Status: StatusError,
		Output: output,
		Meta:   meta,
		Display: Display{
			Summary: request.action + " failed",
			Kind:    "artifact",
		},
	}
}

var artifactNameCleanPattern = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func artifactOutputPath(dir string, name string, extension string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", fmt.Errorf("artifact directory is not configured")
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	base := sanitizeArtifactName(name)
	if base == "" {
		base = defaultArtifactBaseName + "-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	}
	if extension != "" {
		if ext := filepath.Ext(base); ext != "" {
			base = strings.TrimSuffix(base, ext)
		}
		if base == "" {
			base = defaultArtifactBaseName + "-" + time.Now().UTC().Format("20060102T150405.000000000Z")
		}
		base += extension
	}
	path := filepath.Join(absDir, base)
	rel, err := filepath.Rel(absDir, path)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("artifact path escapes artifact directory")
	}
	return path, nil
}

func sanitizeArtifactName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.TrimPrefix(name, ".")
	name = artifactNameCleanPattern.ReplaceAllString(name, "-")
	name = strings.Trim(name, ".-_")
	return name
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
