package tools

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Gitlawb/zero/internal/execution"
	zeroSandbox "github.com/Gitlawb/zero/internal/sandbox"
)

const (
	ExecCommandToolName       = "exec_command"
	WriteStdinToolName        = "write_stdin"
	defaultExecYieldTimeMS    = 10000
	defaultPollYieldTimeMS    = 5000
	maxExecYieldTimeMS        = 30000
	maxPollYieldTimeMS        = 300000
	defaultMaxOutputTokens    = 10000
	maxExecOutputTokenRequest = 200000
	// maxExecOutputBufferBytes caps the undrained output an unpolled session can
	// accumulate. Without a cap, a long-lived background session nobody polls
	// again (e.g. a dev server left running after its initiating run was
	// cancelled) grows this buffer forever as long as the process keeps writing
	// output, with no ceiling — this previously ran a session's memory into the
	// tens of gigabytes over several hours and got the whole zero process
	// OOM-killed by the OS.
	maxExecOutputBufferBytes         = 2 * 1024 * 1024
	execOutputBufferTruncatedMessage = "[zero] output buffer truncated: undrained output exceeded 2MiB, oldest output dropped"
)

type execSessionManager = execution.ProcessManager

func newExecSessionManager() *execSessionManager {
	return execution.NewProcessManager(execution.ProcessManagerOptions{})
}

var defaultExecSessionManager = newExecSessionManager()

type ExecSessionSnapshot = execution.ProcessSnapshot

type ExecSessionController interface {
	ExecSessions() []ExecSessionSnapshot
	ExecSession(id int) (ExecSessionSnapshot, bool)
	StopExecSession(id int) bool
	StopAllExecSessions() []int
	WriteExecSessionInput(id int, data []byte) error
	ResizeExecSession(id int, cols, rows int) error
}

type execCommandTool struct {
	baseTool
	workspaceRoot string
	scope         PathScope
	manager       *execSessionManager
}

func (execCommandTool) outputCategory(args map[string]any) outputCategory {
	command, _ := execCommandArg(args)
	return shellOutputCategory(command)
}

func execCommandArg(args map[string]any) (string, error) {
	return aliasedStringArg(args, []string{"cmd", "command", "script", "shell"}, "", true, false)
}

func NewExecCommandTool(workspaceRoot string, manager *execSessionManager) Tool {
	return NewScopedExecCommandTool(workspaceRoot, nil, manager)
}

func NewScopedExecCommandTool(workspaceRoot string, scope PathScope, manager *execSessionManager) Tool {
	if manager == nil {
		manager = defaultExecSessionManager
	}
	return execCommandTool{
		baseTool: baseTool{
			name:        ExecCommandToolName,
			description: execCommandDescription(runtimeGOOS()),
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"cmd":                 {Type: "string", Description: "Shell command to execute."},
					"workdir":             {Type: "string", Description: "Working directory for the command. Defaults to the turn cwd.", Default: "."},
					"cwd":                 {Type: "string", Description: "Alias for workdir. Prefer workdir.", Default: "."},
					"yield_time_ms":       {Type: "integer", Description: "Wait before yielding output. Defaults to 10000 ms; effective range is 250-30000 ms.", Default: defaultExecYieldTimeMS, Minimum: intPtr(1), Maximum: intPtr(maxExecYieldTimeMS)},
					"max_output_tokens":   {Type: "integer", Description: "Output token budget. Defaults to 10000 tokens; larger requests may be capped by policy.", Default: defaultMaxOutputTokens, Minimum: intPtr(1), Maximum: intPtr(maxExecOutputTokenRequest)},
					"sandbox_permissions": {Type: "string", Enum: []string{string(SandboxPermissionsUseDefault), string(SandboxPermissionsWithAdditionalPermissions), string(SandboxPermissionsRequireEscalated)}, Description: sandboxPermissionsDescription, Default: string(SandboxPermissionsUseDefault)},
					"additional_permissions": {
						Type:        "object",
						Description: additionalPermissionsDescription,
						Properties:  additionalPermissionsProperties(),
					},
					"justification": {Type: "string", Description: "User-facing approval question for `require_escalated`; omit otherwise."},
					"prefix_rule":   {Type: "array", Items: &PropertySchema{Type: "string"}, Description: "Reusable approval prefix for this command, only with `sandbox_permissions: \"require_escalated\"`; keep it narrow, for example [\"git\", \"pull\"]."},
					"tty":           {Type: "boolean", Description: "True allocates a PTY for the command; false or omitted uses plain pipes. Use true for commands that may prompt for input (sudo, ssh, interactive installers): the user can then type into the session directly. Setuid tools such as sudo also need sandbox_permissions \"require_escalated\", since the sandbox blocks privilege escalation.", Default: false},
				},
				Required:             []string{"cmd"},
				AdditionalProperties: false,
			},
			safety: Safety{
				SideEffect:      SideEffectShell,
				Permission:      PermissionPrompt,
				Reason:          "Shell commands can read, write, or execute programs.",
				AdvertiseInAuto: true,
			},
			// PTY/shell session — never concurrent.
			capabilities: ToolCapabilities{Effect: EffectInteractive, ThreadSafe: false, ResourceKeys: processResourceKeys},
		},
		workspaceRoot: normalizeWorkspaceRoot(workspaceRoot),
		scope:         scope,
		manager:       manager,
	}
}

func (tool execCommandTool) Run(ctx context.Context, args map[string]any) Result {
	return tool.run(ctx, args, nil, true)
}

func (tool execCommandTool) RunWithSandbox(ctx context.Context, args map[string]any, engine *zeroSandbox.Engine) Result {
	return tool.run(ctx, args, engine, true)
}

func (tool execCommandTool) RunWithOptions(ctx context.Context, args map[string]any, options RunOptions) Result {
	return tool.run(ctx, args, options.Sandbox, false)
}

func (tool execCommandTool) ExecSessions() []ExecSessionSnapshot {
	return tool.manager.List()
}

func (tool execCommandTool) ExecSession(id int) (ExecSessionSnapshot, bool) {
	return tool.manager.Snapshot(id)
}

func (tool execCommandTool) StopExecSession(id int) bool {
	return tool.manager.Stop(id)
}

func (tool execCommandTool) StopAllExecSessions() []int {
	return tool.manager.StopAll()
}

func (tool execCommandTool) WriteExecSessionInput(id int, data []byte) error {
	return tool.manager.WriteInput(id, data)
}

func (tool execCommandTool) ResizeExecSession(id int, cols, rows int) error {
	return tool.manager.ResizeInput(id, cols, rows)
}

func (tool execCommandTool) run(ctx context.Context, args map[string]any, engine *zeroSandbox.Engine, directBudget bool) Result {
	commandText, err := execCommandArg(args)
	if err != nil {
		return errorResult("Error: Invalid arguments for exec_command: " + err.Error())
	}
	workdir, err := aliasedStringArg(args, []string{"workdir", "cwd", "dir", "directory"}, ".", false, true)
	if err != nil {
		return errorResult("Error: Invalid arguments for exec_command: " + err.Error())
	}
	yieldTimeMS, err := intArg(args, "yield_time_ms", defaultExecYieldTimeMS, 1, 0)
	if err != nil {
		return errorResult("Error: Invalid arguments for exec_command: " + err.Error())
	}
	maxOutputTokens, err := intArg(args, "max_output_tokens", defaultMaxOutputTokens, 1, maxExecOutputTokenRequest)
	if err != nil {
		return errorResult("Error: Invalid arguments for exec_command: " + err.Error())
	}
	ttyRequested, err := boolArg(args, "tty", false)
	if err != nil {
		return errorResult("Error: Invalid arguments for exec_command: " + err.Error())
	}
	sandboxPermissions, err := sandboxPermissionsArg(args)
	if err != nil {
		return errorResult("Error: Invalid arguments for exec_command: " + err.Error())
	}
	// Resolve the command engine before the MSYS preflight check so an
	// approved require_escalated call (commandEngine == nil, truly
	// unsandboxed) can actually bypass the MSYS guard instead of being
	// hard-blocked by the same check it was meant to escalate past.
	commandEngine := commandEngineForSandboxPermissions(engine, sandboxPermissions)
	if issue := detectShellCommandIssueForRuntime(commandText, detectShellRuntime(runtimeGOOS())); issue != nil && !msysGuardBypassed(issue, commandEngine) {
		return shellIssueBlockResult(*issue)
	}
	// tty:true exists exactly to run prompting commands, so the
	// non-interactive guard steps aside and the PTY path gets the session.
	if !ttyRequested {
		if interactive := zeroSandbox.DetectInteractiveCommand(commandText, runtimeGOOS()); interactive.Interactive {
			result := interactiveBlockResult(interactive)
			result.Output += "\nRerun with tty:true if the user should interact with it."
			return result
		}
	}
	absoluteCwd, relativeCwd, err := resolveScopedPath(tool.workspaceRoot, tool.scope, workdir)
	if err != nil {
		return errorResult("Error running exec_command: " + err.Error())
	}

	commandCtx, cancel := context.WithCancel(context.Background())
	command, plan, err := buildBashCommand(commandCtx, commandText, absoluteCwd, commandEngine)
	if err != nil {
		cancel()
		return errorResult("Error starting exec_command: " + err.Error())
	}
	executionRequest := execExecutionRequest(command, plan, absoluteCwd, ttyRequested)
	if err := executionRequest.Validate(); err != nil {
		plan.Cleanup()
		cancel()
		return errorResult("Error starting exec_command: prepare execution request: " + err.Error())
	}
	meta := map[string]string{}
	addSandboxMeta(meta, plan)
	monitor := zeroSandbox.StartDenialMonitor(context.Background(), plan.MonitorTag)
	processResult, err := tool.manager.Start(ctx, execution.ProcessStart{
		Prepared: execution.PreparedCommand{
			Command:     command,
			Enforcement: executionEnforcement(plan),
			Report:      plan.ExecutionReport,
			Cleanup: func() {
				plan.Cleanup()
				cancel()
			},
		},
		Request: executionRequest, CommandText: commandText, RelativeCwd: relativeCwd,
		TTY: ttyRequested, Metadata: meta,
		AfterWait: func() []byte {
			blocks := monitor.Stop()
			if len(blocks) == 0 {
				return nil
			}
			return []byte(appendSandboxBlocks("", blocks))
		},
	}, time.Duration(yieldTimeMS)*time.Millisecond)
	if err != nil {
		_ = monitor.Stop()
		return errorResult("Error starting exec_command: " + err.Error())
	}
	return execToolResultWithBudget(execToolResultInput{
		commandText: processResult.CommandText, output: processResult.Output,
		outputBufferTruncated: processResult.OutputTruncated, sessionID: processResult.ProcessID,
		exitCode: processResult.ExitCode, exited: processResult.Exited, relativeCwd: processResult.RelativeCwd,
		tty: processResult.TTY, request: processResult.Request, enforcement: processResult.Enforcement,
		report: processResult.Report, reportErr: processResult.ReportErr, changes: processResult.Changes,
		sandboxMeta:     processResult.Metadata,
		maxOutputTokens: maxOutputTokens,
	}, directBudget)
}

func executionEnforcement(plan zeroSandbox.CommandPlan) execution.Enforcement {
	return execution.Enforcement{
		Backend:         string(plan.TargetBackend),
		Level:           string(plan.EnforcementLevel),
		Degraded:        plan.EnforcementLevel == zeroSandbox.EnforcementDegraded,
		DowngradeReason: plan.DowngradeReason,
	}
}

type writeStdinTool struct {
	baseTool
	manager *execSessionManager
}

// UnknownExecSessionError is the result returned when write_stdin targets a
// session_id with no live exec session. The stable recovery guidance leads and
// the numeric id trails on purpose: the agent's repeated-failure guard keys on
// a normalized, truncated prefix of the error string, so keeping the id out of
// that prefix makes a model probing ids 1, 2, 3, … produce ONE signature that
// finally trips the halt instead of a fresh signature per id — while also
// telling the model how to actually recover. See TestUnknownExecSessionErrorSignatureIsIDInvariant
// in internal/agent, which pins this invariant against the real guard.
func UnknownExecSessionError(sessionID int) string {
	return fmt.Sprintf("Error: write_stdin needs a session_id returned by a still-running exec_command; do not guess or probe session ids. Start a process with exec_command, or use write_file/edit_file/apply_patch for file changes. (no live session %d)", sessionID)
}

func (writeStdinTool) outputCategory(map[string]any) outputCategory {
	return outputCategoryProcess
}

func NewWriteStdinTool(manager *execSessionManager) Tool {
	if manager == nil {
		manager = defaultExecSessionManager
	}
	return writeStdinTool{
		baseTool: baseTool{
			name:        WriteStdinToolName,
			description: "Writes characters to an existing unified exec session and returns recent output.",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"session_id":        {Type: "integer", Description: "Identifier of a running unified exec session, as returned by exec_command. Never guess or probe ids.", Minimum: intPtr(1)},
					"chars":             {Type: "string", Description: "Bytes to write to stdin. Defaults to empty, which polls without writing.", Default: ""},
					"yield_time_ms":     {Type: "integer", Description: "Wait before yielding output. Non-empty writes default to 250 ms and cap at 30000 ms; empty polls wait 5000-300000 ms by default.", Default: defaultPollYieldTimeMS, Minimum: intPtr(1), Maximum: intPtr(maxPollYieldTimeMS)},
					"max_output_tokens": {Type: "integer", Description: "Output token budget. Defaults to 10000 tokens; larger requests may be capped by policy.", Default: defaultMaxOutputTokens, Minimum: intPtr(1), Maximum: intPtr(maxExecOutputTokenRequest)},
				},
				Required:             []string{"session_id"},
				AdditionalProperties: false,
			},
			safety: Safety{
				SideEffect:      SideEffectShell,
				Permission:      PermissionPrompt,
				Reason:          "Sending stdin can drive an existing shell process beyond the original command; empty polling and Ctrl-C interrupts are allowed automatically.",
				AdvertiseInAuto: true,
			},
			// Writes to a retained process stdin — process interaction.
			capabilities: ToolCapabilities{Effect: EffectInteractive, ThreadSafe: false, ResourceKeys: processResourceKeys},
		},
		manager: manager,
	}
}

func (tool writeStdinTool) PermissionForArgs(args map[string]any) Permission {
	raw, ok := args["chars"]
	if !ok || raw == nil {
		return PermissionAllow
	}
	chars, ok := raw.(string)
	if !ok {
		return PermissionPrompt
	}
	if chars == "" || chars == "\x03" {
		return PermissionAllow
	}
	return PermissionPrompt
}

func (tool writeStdinTool) Run(ctx context.Context, args map[string]any) Result {
	return tool.RunWithOptions(ctx, args, RunOptions{})
}

func (tool writeStdinTool) RunWithOptions(ctx context.Context, args map[string]any, _ RunOptions) Result {
	// A missing, non-integer, or < 1 session_id all mean the same thing: the model
	// has no live session to write to. Route them to the SAME recovery guidance the
	// no-live-session case uses (UnknownExecSessionError) instead of a terse
	// "session_id must be at least 1". The terse error gives no way forward and, by
	// naming the minimum, nudges the model to try 1, 2, 3... — the exact id-probing
	// #749 works to suppress. The recovery message instead tells it to start a
	// session or edit files directly, and because that message is id-invariant, the
	// missing/zero/non-integer entry points and id-probing collapse to ONE
	// repeated-failure signature, so any mix of them accumulates toward the halt
	// rather than resetting the streak on each class of mistake.
	value, present := args["session_id"]
	sessionID, sessionErr := intArg(args, "session_id", 0, 1, 0)
	if !present || value == nil || sessionErr != nil {
		return errorResult(UnknownExecSessionError(sessionID))
	}
	chars, err := stringArgWithEmpty(args, "chars", "", false, true)
	if err != nil {
		return errorResult("Error: Invalid arguments for write_stdin: " + err.Error())
	}
	defaultYieldTimeMS := defaultPollYieldTimeMS
	if chars != "" {
		defaultYieldTimeMS = 250
	}
	yieldTimeMS, err := intArg(args, "yield_time_ms", defaultYieldTimeMS, 1, 0)
	if err != nil {
		return errorResult("Error: Invalid arguments for write_stdin: " + err.Error())
	}
	maxOutputTokens, err := intArg(args, "max_output_tokens", defaultMaxOutputTokens, 1, maxExecOutputTokenRequest)
	if err != nil {
		return errorResult("Error: Invalid arguments for write_stdin: " + err.Error())
	}
	snapshot, ok := tool.manager.Snapshot(sessionID)
	if !ok {
		return errorResult(UnknownExecSessionError(sessionID))
	}
	interrupted := chars != "" && shouldInterruptExecSession(chars, snapshot.TTY)
	processResult, err := tool.manager.Continue(ctx, execution.ProcessContinue{
		ProcessID: sessionID, Input: []byte(chars), Interrupt: interrupted,
		Wait: time.Duration(yieldTimeMS) * time.Millisecond,
	})
	if errors.Is(err, execution.ErrProcessNotFound) {
		return errorResult(UnknownExecSessionError(sessionID))
	}
	if errors.Is(err, execution.ErrProcessStdinDisabled) {
		return errorResult(fmt.Sprintf("Error: exec session_id %d does not accept stdin. Use empty chars to poll, or send chars \"\\u0003\" to interrupt/stop it.", sessionID))
	}
	if err != nil {
		return errorResult("Error writing to exec session: " + err.Error())
	}
	return execToolResult(execToolResultInput{
		commandText: processResult.CommandText, output: processResult.Output,
		outputBufferTruncated: processResult.OutputTruncated, sessionID: processResult.ProcessID,
		exitCode: processResult.ExitCode, exited: processResult.Exited, relativeCwd: processResult.RelativeCwd,
		tty: processResult.TTY, interrupted: processResult.Interrupted, request: processResult.Request,
		enforcement: processResult.Enforcement, report: processResult.Report, reportErr: processResult.ReportErr,
		changes: processResult.Changes, sandboxMeta: processResult.Metadata,
		maxOutputTokens: maxOutputTokens,
	})
}

func shouldInterruptExecSession(chars string, tty bool) bool {
	if strings.Contains(chars, "\x03") {
		return true
	}
	normalized := strings.ToLower(strings.TrimSpace(chars))
	normalizedNoSpace := strings.ReplaceAll(normalized, " ", "")
	switch normalizedNoSpace {
	case `\u0003`, `\\u0003`, `\x03`, `\\x03`, "^c", "ctrl-c", "control-c", "sigint", "interrupt":
		return true
	}
	if tty {
		return false
	}
	switch normalized {
	case "q", "quit", "exit", "stop", "kill", "terminate":
		return true
	}
	return false
}

type execToolResultInput struct {
	commandText string
	output      string
	// outputBufferTruncated is true when the session's undrained output buffer
	// had to drop bytes to stay within maxExecOutputBufferBytes since it was
	// last collected — data that is gone for good, unlike the head/tail
	// truncation below (which the caller can recover by polling again or
	// raising max_output_tokens).
	outputBufferTruncated bool
	sessionID             int
	exitCode              int
	exited                bool
	relativeCwd           string
	tty                   bool
	interrupted           bool
	request               execution.Request
	enforcement           execution.Enforcement
	sandboxMeta           map[string]string
	report                execution.AdapterReport
	reportErr             error
	changes               []execution.Change
	maxOutputTokens       int
}

func execToolResult(input execToolResultInput) Result {
	return execToolResultWithBudget(input, true)
}

func execToolResultWithBudget(input execToolResultInput, directBudget bool) Result {
	output := input.output
	truncated := false
	if directBudget {
		output, truncated = truncateExecOutput(input.output, input.maxOutputTokens)
	}
	meta := map[string]string{
		"cwd": input.relativeCwd,
		"tty": strconv.FormatBool(input.tty),
	}
	for key, value := range input.sandboxMeta {
		meta[key] = value
	}
	outcome := execExecutionOutcome(input)
	if input.exited {
		meta["exit_code"] = strconv.Itoa(input.exitCode)
		if input.interrupted {
			meta["interrupted"] = "true"
		}
		if input.report.Denial != nil {
			markStructuredSandboxDenial(meta, *input.report.Denial)
		}
	} else {
		meta["session_id"] = strconv.Itoa(input.sessionID)
	}
	if input.outputBufferTruncated {
		meta["output_buffer_truncated"] = "true"
	}

	status := StatusOK
	if input.exited && input.exitCode != 0 && !input.interrupted {
		status = StatusError
	}
	body := formatExecCommandOutput(output, input.sessionID, input.exited, input.exitCode, input.interrupted, input.tty)
	if status == StatusError && input.exited && !input.interrupted {
		if issue := detectShellOutputIssueForRuntime(output, detectShellRuntime(runtimeGOOS())); issue != nil {
			meta["shell_issue"] = issue.Kind
			body = appendShellIssueHint(body, *issue)
		}
	}
	if input.outputBufferTruncated {
		// Appended after truncateExecOutput's own head/tail slicing, not
		// embedded in the text that goes through it — a marker inside that
		// text can land in the discarded middle or get chopped at a small
		// max_output_tokens budget. Appending here guarantees it survives.
		body += "\n" + execOutputBufferTruncatedMessage
	}
	return Result{
		Status:           status,
		Output:           body,
		Truncated:        truncated || input.outputBufferTruncated,
		Meta:             meta,
		ExecutionRequest: &input.request,
		ExecutionOutcome: &outcome,
		ChangedFiles:     executionChangedFiles(input.changes),
		ChangeSummaries:  executionChangeSummaries(input.changes),
		Display: Display{
			Summary: execDisplaySummary(input.commandText, input.sessionID, input.exited, input.exitCode),
			Kind:    "shell",
		},
	}
}

func execExecutionRequest(command *exec.Cmd, plan zeroSandbox.CommandPlan, cwd string, tty bool) execution.Request {
	mode := execution.ModeCaptured
	if tty {
		mode = execution.ModeInteractive
	}
	workspaceRoot := strings.TrimSpace(plan.WorkspaceRoot)
	if workspaceRoot == "" {
		workspaceRoot = cwd
	}
	workspaceRoots := []string{workspaceRoot}
	capabilities := []execution.Capability{{Kind: execution.CapabilityProcessSpawn}}
	if plan.PermissionProfile.FileSystem.Kind == zeroSandbox.FileSystemUnrestricted {
		capabilities = append(capabilities, execution.Capability{Kind: execution.CapabilityUnrestricted, Scope: "host"})
	}
	for _, root := range plan.PermissionProfile.FileSystem.ReadRoots {
		capabilities = append(capabilities, execution.Capability{Kind: execution.CapabilityRead, Scope: root})
	}
	for _, root := range plan.PermissionProfile.FileSystem.WriteRoots {
		capabilities = append(capabilities, execution.Capability{Kind: execution.CapabilityWorkspaceWrite, Scope: root.Root})
	}
	if plan.PermissionProfile.Network.Mode == zeroSandbox.NetworkAllow || plan.Policy.Network == zeroSandbox.NetworkAllow {
		capabilities = append(capabilities, execution.Capability{Kind: execution.CapabilityExternalNetwork})
	} else if plan.TargetBackend == zeroSandbox.BackendLinuxBwrap {
		capabilities = append(capabilities, execution.Capability{Kind: execution.CapabilityIsolatedLoopback, Scope: "sandbox"})
	}
	args := append([]string(nil), command.Args...)
	name := command.Path
	if len(args) > 0 {
		name = args[0]
		args = args[1:]
	}
	return execution.Request{
		Origin:           execution.OriginInteractiveCommand,
		Mode:             mode,
		Command:          execution.Command{Name: name, Args: args},
		WorkingDirectory: cwd,
		WorkspaceRoots:   workspaceRoots,
		Capabilities:     capabilities,
		Approval:         execution.ApprovalContext{PolicyVersion: execution.PolicyVersion},
	}
}

func execExecutionOutcome(input execToolResultInput) execution.Outcome {
	enforcement := input.enforcement
	if !input.exited {
		return execution.Outcome{
			State:       execution.StateRetained,
			Kind:        execution.OutcomeRunning,
			ProcessID:   strconv.Itoa(input.sessionID),
			Enforcement: enforcement,
		}
	}
	exit := &execution.Exit{Code: input.exitCode}
	if input.reportErr != nil {
		return execution.Outcome{State: execution.StateFailed, Kind: execution.OutcomeSandboxSetupFailure, Exit: exit, Enforcement: enforcement, Changes: input.changes}
	}
	if input.report.Denial != nil {
		denial := *input.report.Denial
		return execution.Outcome{State: execution.StateDenied, Kind: execution.OutcomeEnforcementDenied, Exit: exit, Denial: &denial, Enforcement: enforcement, Changes: input.changes}
	}
	if input.interrupted {
		return execution.Outcome{State: execution.StateCancelled, Kind: execution.OutcomeCancelled, Exit: exit, Enforcement: enforcement, Changes: input.changes}
	}
	if input.exitCode == 0 {
		return execution.Outcome{State: execution.StateCompleted, Kind: execution.OutcomeSuccess, Exit: exit, Enforcement: enforcement, Changes: input.changes}
	}
	return execution.Outcome{State: execution.StateFailed, Kind: execution.OutcomeApplicationFailure, Exit: exit, Enforcement: enforcement, Changes: input.changes}
}

func executionChangedFiles(changes []execution.Change) []string {
	if len(changes) == 0 {
		return nil
	}
	paths := make([]string, 0, len(changes))
	for _, change := range changes {
		if !change.Aggregated && strings.TrimSpace(change.Path) != "" {
			paths = append(paths, change.Path)
		}
	}
	return paths
}

func executionChangeSummaries(changes []execution.Change) []execution.Change {
	var summaries []execution.Change
	for _, change := range changes {
		if change.Aggregated && strings.TrimSpace(change.Path) != "" {
			summaries = append(summaries, change)
		}
	}
	return summaries
}

func formatExecCommandOutput(output string, sessionID int, exited bool, exitCode int, interrupted bool, tty bool) string {
	output = strings.TrimRight(output, "\r\n")
	parts := []string{}
	if output != "" {
		parts = append(parts, "output:\n"+output)
	}
	if exited {
		if output == "" {
			if interrupted {
				parts = append(parts, "Command interrupted.")
			} else {
				parts = append(parts, "Command completed with no output.")
			}
		}
		if interrupted {
			parts = append(parts, "interrupted: true")
		}
		parts = append(parts, fmt.Sprintf("exit_code: %d", exitCode))
		if exitCode != 0 && strings.Contains(output, "no new privileges") {
			parts = append(parts, `Hint: this command needs sandbox_permissions "require_escalated" (the sandbox blocks setuid); retry with it and tty:true if it prompts.`)
		}
	} else {
		if output == "" {
			parts = append(parts, "Command is still running.")
		}
		parts = append(parts, fmt.Sprintf("session_id: %d", sessionID))
		parts = append(parts, fmt.Sprintf("Use write_stdin with session_id %d and empty chars to poll; send chars \"\\u0003\" to interrupt/stop it.", sessionID))
		if tty {
			parts = append(parts, fmt.Sprintf("This session has a terminal and the user can already type into it (it opened in their TUI; /attach %d reopens it). If it is waiting on a password or confirmation, say so in one line, then keep polling with write_stdin (empty chars, yield_time_ms 60000) until it exits. Do not end your turn while it is running, and never ask for the password in chat.", sessionID))
		}
	}
	return strings.Join(parts, "\n")
}

func truncateExecOutput(output string, maxOutputTokens int) (string, bool) {
	return truncateExecOutputSpill(output, maxOutputTokens, "exec_command")
}

// truncateExecOutputSpill keeps a head/tail window of the output within the
// token budget and, on truncation, spills the full output to disk so the model
// can grep/read the elided middle instead of re-running the command with a
// bigger budget. The spill is best-effort: when it fails the notice simply
// omits the file hint.
func truncateExecOutputSpill(output string, maxOutputTokens int, toolName string) (string, bool) {
	if maxOutputTokens <= 0 {
		maxOutputTokens = defaultMaxOutputTokens
	}
	maxBytes := maxOutputTokens * 4
	if len(output) <= maxBytes {
		return output, false
	}
	notice := "\n[zero] output truncated\n"
	if spillPath := spillTruncatedOutput(toolName, output); spillPath != "" {
		notice = "\n[zero] output truncated — full output saved to " + spillPath + " (grep or read_file it instead of re-running)\n"
	}
	head := maxBytes / 2
	tail := maxBytes - head
	return utf8Prefix(output, head) + notice + utf8Suffix(output, tail), true
}

func utf8Prefix(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	for maxBytes > 0 && !utf8.RuneStart(value[maxBytes]) {
		maxBytes--
	}
	return value[:maxBytes]
}

func utf8Suffix(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	start := len(value) - maxBytes
	for start < len(value) && !utf8.RuneStart(value[start]) {
		start++
	}
	return value[start:]
}

func execDisplaySummary(commandText string, sessionID int, exited bool, exitCode int) string {
	commandText = strings.TrimSpace(commandText)
	if commandText == "" {
		commandText = "command"
	}
	if exited {
		return fmt.Sprintf("%s exited with code %d", commandText, exitCode)
	}
	return fmt.Sprintf("%s still running as session %d", commandText, sessionID)
}

func runtimeGOOS() string {
	return runtime.GOOS
}

// execCommandDescription is exec_command's description as a host running goos
// would see it. The shell guidance is appended on Windows only.
//
// Taking goos as an argument rather than reading it is what lets the guidance
// contract be checked for every platform from any one of them. internal/agent's
// token ratchet removes exactly this appended text before charging the schemas,
// so the condition here and hostExecCommandShellGuidance have to agree on all
// three platforms, not only on whichever one the test happens to run on.
func execCommandDescription(goos string) string {
	description := "Runs a command in a PTY, returning output or a session ID for ongoing interaction."
	if goos == "windows" {
		description += "\n\n" + shellGuidanceForGOOS(goos)
	}
	return description
}
