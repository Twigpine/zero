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

	"github.com/Twigpine/zero/internal/execution"
	zeroSandbox "github.com/Twigpine/zero/internal/sandbox"
	"github.com/Twigpine/zero/internal/secrets"
)

const defaultBashTimeoutMS = 120000
const maxBashTimeoutMS = 600000

type bashTool struct {
	baseTool
	workspaceRoot string
	scope         PathScope
}

func (bashTool) outputCategory(args map[string]any) outputCategory {
	command, _ := bashCommandArg(args)
	return shellOutputCategory(command)
}

func bashCommandArg(args map[string]any) (string, error) {
	return aliasedStringArg(args, []string{"command", "cmd", "script", "shell"}, "", true, false)
}

func NewBashTool(workspaceRoot string) Tool {
	return NewScopedBashTool(workspaceRoot, nil)
}

func NewScopedBashTool(workspaceRoot string, scope PathScope) Tool {
	shellGuidance := shellGuidanceForGOOS(runtime.GOOS)
	return bashTool{
		baseTool: baseTool{
			name:        "bash",
			description: "Execute a shell command inside the workspace (or an explicitly granted extra directory) after permission is granted. " + shellGuidance,
			deferred:    true,
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"command":             {Type: "string", Description: "Shell command to execute using the host shell. " + shellGuidance},
					"cwd":                 {Type: "string", Description: "Directory to run the command in. Relative paths stay in the workspace; use an absolute path to run in a granted extra directory. Defaults to workspace root. Prefer cwd over cd when changing directories.", Default: "."},
					"timeout_ms":          {Type: "integer", Description: "Command timeout in milliseconds.", Default: defaultBashTimeoutMS, Minimum: intPtr(1), Maximum: intPtr(maxBashTimeoutMS)},
					"sandbox_permissions": {Type: "string", Enum: []string{string(SandboxPermissionsUseDefault), string(SandboxPermissionsWithAdditionalPermissions), string(SandboxPermissionsRequireEscalated)}, Description: sandboxPermissionsDescription, Default: string(SandboxPermissionsUseDefault)},
					"additional_permissions": {
						Type:        "object",
						Description: additionalPermissionsDescription,
						Properties:  additionalPermissionsProperties(),
					},
					"justification": {Type: "string", Description: "User-facing approval question for `require_escalated`; omit otherwise."},
					"prefix_rule":   {Type: "array", Items: &PropertySchema{Type: "string"}, Description: "Reusable approval prefix for this command, only with `sandbox_permissions: \"require_escalated\"`; keep it narrow, for example [\"git\", \"pull\"]."},
				},
				Required:             []string{"command"},
				AdditionalProperties: false,
			},
			safety: promptSafety(SideEffectShell, "Shell commands can read, write, or execute programs."),
			// One-shot shell can rewrite the workspace (and more). Serialized
			// as WorkspaceWrite so external-mutator audits include bash; not
			// session-bound Interactive (that is reserved for PTY/terminal tools).
			capabilities: ToolCapabilities{Effect: EffectWorkspaceWrite, ThreadSafe: false, ResourceKeys: directoryResourceKeys},
		},
		workspaceRoot: normalizeWorkspaceRoot(workspaceRoot),
		scope:         scope,
	}
}

func (tool bashTool) Run(ctx context.Context, args map[string]any) Result {
	return tool.run(ctx, args, nil, true)
}

func (tool bashTool) RunWithSandbox(ctx context.Context, args map[string]any, engine *zeroSandbox.Engine) Result {
	return tool.run(ctx, args, engine, true)
}

func (tool bashTool) RunWithOptions(ctx context.Context, args map[string]any, options RunOptions) Result {
	return tool.run(ctx, args, options.Sandbox, false)
}

func (tool bashTool) run(ctx context.Context, args map[string]any, engine *zeroSandbox.Engine, directBudget bool) Result {
	commandText, err := bashCommandArg(args)
	if err != nil {
		return errorResult("Error: Invalid arguments for bash: " + err.Error())
	}
	cwd, err := stringArg(args, "cwd", ".", false)
	if err != nil {
		return errorResult("Error: Invalid arguments for bash: " + err.Error())
	}
	timeoutMS, err := intArg(args, "timeout_ms", defaultBashTimeoutMS, 1, maxBashTimeoutMS)
	if err != nil {
		return errorResult("Error: Invalid arguments for bash: " + err.Error())
	}
	sandboxPermissions, err := sandboxPermissionsArg(args)
	if err != nil {
		return errorResult("Error: Invalid arguments for bash: " + err.Error())
	}
	// Resolve the command engine before the MSYS preflight check so an
	// approved require_escalated call (commandEngine == nil, truly
	// unsandboxed) can actually bypass the MSYS guard instead of being
	// hard-blocked by the same check it was meant to escalate past.
	commandEngine := commandEngineForSandboxPermissions(engine, sandboxPermissions)
	if issue := detectShellCommandIssueForRuntime(commandText, detectShellRuntime(runtime.GOOS)); issue != nil && !msysGuardBypassed(issue, commandEngine) {
		return shellIssueBlockResult(*issue)
	}

	// Pre-execution safety: refuse interactive commands (editors, pagers, REPLs,
	// remote shells, etc.) that would hang the non-interactive agent until the
	// timeout fires. This runs before the command is built or launched.
	if interactive := zeroSandbox.DetectInteractiveCommand(commandText, runtime.GOOS); interactive.Interactive {
		return interactiveBlockResult(interactive)
	}

	absoluteCwd, relativeCwd, err := resolveScopedPath(tool.workspaceRoot, tool.scope, cwd)
	if err != nil {
		return errorResult("Error running bash: " + err.Error())
	}

	commandCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMS)*time.Millisecond)
	defer cancel()

	meta := map[string]string{
		"cwd":        relativeCwd,
		"timeout_ms": strconv.Itoa(timeoutMS),
	}
	if commandEngine == nil && sandboxPermissions == SandboxPermissionsRequireEscalated {
		meta["sandbox_permissions"] = string(SandboxPermissionsRequireEscalated)
	}
	command, plan, err := buildBashCommand(commandCtx, commandText, absoluteCwd, commandEngine)
	if err != nil {
		meta["exit_code"] = "-1"
		return Result{
			Status: StatusError,
			Output: "Error preparing sandboxed bash command: " + err.Error(),
			Meta:   meta,
		}
	}
	defer plan.Cleanup()
	addSandboxMeta(meta, plan)
	executionRequest := execExecutionRequest(command, plan, absoluteCwd, false)
	changeObserver := execution.NewChangeObserver(plan.WorkspaceRoot)

	// Bound the capture so a command with runaway output (`cat huge.log`, `yes`)
	// can't grow Zero's memory before truncation: only the head+tail each stream
	// will ever surface to the model are retained, the middle is discarded as it
	// streams, and the true size is counted for the truncation marker.
	stdout := newBoundedBuffer(bashCaptureBudgetBytes, bashCaptureBudgetBytes)
	stderr := newBoundedBuffer(bashCaptureBudgetBytes, bashCaptureBudgetBytes)
	command.Stdout = stdout
	command.Stderr = stderr

	// Kill the shell as a process group on timeout and bound the post-kill I/O
	// wait, so a backgrounded child cannot outlive the command or hang Run().
	hardenProcessLifetime(command)

	// Capture sandbox denials when the plan opted in (macOS + Policy.MonitorDenials).
	// A no-op when MonitorTag is empty, so the default path is unchanged.
	monitor := zeroSandbox.StartDenialMonitor(context.Background(), plan.MonitorTag)
	err = command.Run()
	exitCode := commandExitCode(err)
	adapterReport, reportErr := plan.ExecutionReport()
	meta["exit_code"] = strconv.Itoa(exitCode)
	stdoutText := stdout.retained()
	stderrRetained := stderr.retained()
	stderrText := appendSandboxBlocks(stderrRetained, monitor.Stop())
	// Sandbox blocks are extra model-visible stderr bytes appended after capture;
	// count them toward the true total so the budget/marker stay accurate.
	stderrTotal := stderr.total + (len(stderrText) - len(stderrRetained))

	if errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
		result := Result{
			Status: StatusError,
			Output: fmt.Sprintf("Error: Command timed out after %dms.", timeoutMS),
			Meta:   meta,
		}
		return withBashExecution(result, executionRequest, plan, exitCode, adapterReport, reportErr, changeObserver.Changes(), true)
	}
	if err != nil {
		if exitCode < 0 {
			result := Result{
				Status: StatusError,
				Output: "Error executing command: " + err.Error(),
				Meta:   meta,
			}
			return withBashExecution(result, executionRequest, plan, exitCode, adapterReport, reportErr, changeObserver.Changes(), false)
		}
		if adapterReport.Denial != nil {
			markStructuredSandboxDenial(meta, *adapterReport.Denial)
		}
		outText, errText, truncated := prepareBashOutput(stdoutText, stdout.total, stderrText, stderrTotal, meta, directBudget)
		result := Result{
			Status:    StatusError,
			Output:    formatBashOutputWithShellHint(outText, errText, exitCode, meta),
			Truncated: truncated,
			Meta:      meta,
		}
		return withBashExecution(result, executionRequest, plan, exitCode, adapterReport, reportErr, changeObserver.Changes(), false)
	}

	if adapterReport.Denial != nil {
		markStructuredSandboxDenial(meta, *adapterReport.Denial)
	}
	outText, errText, truncated := prepareBashOutput(stdoutText, stdout.total, stderrText, stderrTotal, meta, directBudget)
	result := Result{
		Status:    StatusOK,
		Output:    formatBashOutput(outText, errText, exitCode),
		Truncated: truncated,
		Meta:      meta,
	}
	return withBashExecution(result, executionRequest, plan, exitCode, adapterReport, reportErr, changeObserver.Changes(), false)
}

func withBashExecution(result Result, request execution.Request, plan zeroSandbox.CommandPlan, exitCode int, report execution.AdapterReport, reportErr error, changes []execution.Change, timedOut bool) Result {
	input := execToolResultInput{
		exited:      true,
		exitCode:    exitCode,
		enforcement: executionEnforcement(plan),
		request:     request,
		report:      report,
		reportErr:   reportErr,
		changes:     changes,
	}
	outcome := execExecutionOutcome(input)
	if timedOut && report.Denial == nil && reportErr == nil {
		outcome.State = execution.StateFailed
		outcome.Kind = execution.OutcomeTimedOut
	}
	result.ExecutionRequest = &request
	result.ExecutionOutcome = &outcome
	result.ChangedFiles = executionChangedFiles(changes)
	result.ChangeSummaries = executionChangeSummaries(changes)
	return result
}

func commandEngineForSandboxPermissions(engine *zeroSandbox.Engine, sandboxPermissions SandboxPermissionOverride) *zeroSandbox.Engine {
	if sandboxPermissions == SandboxPermissionsRequireEscalated && (engine == nil || engine.UnsandboxedExecutionAllowed()) {
		return nil
	}
	return engine
}

// msysGuardBypassed reports whether a windows_msys_sandbox preflight issue no
// longer applies because sandbox_permissions: require_escalated actually
// resolved to unsandboxed, host-level execution (commandEngine is nil only
// then, per commandEngineForSandboxPermissions). MSYS coreutils fail because
// of the sandbox's restricted token/handles, so running outside it removes
// the failure mode this guard exists for. Any other issue kind (e.g. a real
// cmd.exe syntax problem) still blocks regardless of escalation, since
// running outside the sandbox does not fix a syntax error.
func msysGuardBypassed(issue *shellIssue, commandEngine *zeroSandbox.Engine) bool {
	return issue.Kind == windowsMsysSandboxKind && commandEngine == nil
}

// appendSandboxBlocks appends a <sandbox_blocks> block listing the denials
// the sandbox log monitor captured, so the model can see what was blocked. With no
// blocks the stderr is returned unchanged.
func appendSandboxBlocks(stderr string, blocks []string) string {
	if len(blocks) == 0 {
		return stderr
	}
	var builder strings.Builder
	builder.WriteString("<sandbox_blocks>\n")
	for _, block := range blocks {
		builder.WriteString(block)
		builder.WriteString("\n")
	}
	builder.WriteString("</sandbox_blocks>")
	if strings.TrimSpace(stderr) == "" {
		return builder.String()
	}
	return stderr + "\n" + builder.String()
}

func shellIssueBlockResult(issue shellIssue) Result {
	return Result{
		Status: StatusError,
		Output: appendShellIssueHint("", issue),
		Meta: map[string]string{
			"exit_code":   "-1",
			"shell_issue": issue.Kind,
		},
		Display: Display{
			Summary: issue.Message,
			Kind:    "shell",
		},
	}
}

// buildBashCommand returns the exec.Cmd and the sandbox plan for running
// commandText. PowerShell is preferred on Windows, with cmd.exe retained as
// the fallback. Only cmd.exe needs the raw command-line override.
func buildBashCommand(ctx context.Context, commandText string, absoluteCwd string, engine *zeroSandbox.Engine) (*exec.Cmd, zeroSandbox.CommandPlan, error) {
	hostShell := detectShellRuntime(runtime.GOOS)
	spec := zeroSandbox.CommandSpec{
		Name: hostShell.Executable,
		Args: hostShell.arguments(commandText),
		Dir:  absoluteCwd,
	}
	if engine != nil {
		command, plan, err := engine.CommandContext(ctx, spec)
		if err == nil {
			applyWindowsShellCommandLine(command, commandText, plan.Wrapped, hostShell.Kind == shellKindCmd)
		}
		return command, plan, err
	}
	directPolicy := zeroSandbox.DefaultPolicy()
	directPolicy.Mode = zeroSandbox.ModeDisabled
	directPolicy.Network = zeroSandbox.NetworkAllow
	plan := zeroSandbox.CommandPlan{
		Backend: zeroSandbox.Backend{
			Name:    zeroSandbox.BackendUnavailable,
			Message: "sandbox engine not provided",
		},
		TargetBackend:     zeroSandbox.BackendNone,
		WorkspaceRoot:     absoluteCwd,
		Policy:            directPolicy,
		PermissionProfile: zeroSandbox.PermissionProfileFromPolicy(absoluteCwd, directPolicy, nil),
		Wrapped:           false,
		EnforcementLevel:  zeroSandbox.EnforcementDisabled,
		Name:              spec.Name,
		Args:              spec.Args,
		Dir:               spec.Dir,
	}
	command := exec.CommandContext(ctx, spec.Name, spec.Args...)
	command.Dir = spec.Dir
	applyWindowsShellCommandLine(command, commandText, plan.Wrapped, hostShell.Kind == shellKindCmd)
	return command, plan, nil
}

func addSandboxMeta(meta map[string]string, plan zeroSandbox.CommandPlan) {
	if plan.Backend.Name == "" {
		return
	}
	meta["sandbox_backend"] = string(plan.Backend.Name)
	if plan.TargetBackend != "" {
		meta["sandbox_target_backend"] = string(plan.TargetBackend)
	}
	meta["sandbox_wrapped"] = strconv.FormatBool(plan.Wrapped)
	if plan.EnforcementLevel != "" {
		meta["sandbox_enforcement_level"] = string(plan.EnforcementLevel)
	}
	if plan.DowngradeReason != "" {
		meta["sandbox_downgrade_reason"] = plan.DowngradeReason
	}
	meta["sandbox_requires_platform"] = strconv.FormatBool(plan.RequiresPlatformSandbox)
	if plan.Backend.Message != "" {
		meta["sandbox_message"] = plan.Backend.Message
	}
	if plan.SandboxDir != "" {
		meta["sandbox_cwd"] = plan.SandboxDir
	}
}

// interactiveBlockResult builds the structured tool Result returned when a
// command is refused before execution because it would hang the agent. The
// block is surfaced both in Output (clearly delimited) and in Meta/Display
// so downstream consumers and the TUI can render it consistently.
func interactiveBlockResult(detection zeroSandbox.InteractiveCommandResult) Result {
	message := fmt.Sprintf(
		"Error: Blocked interactive command %q before execution: %s. This would hang the non-interactive agent.\nSuggestion: %s",
		detection.Command, detection.Reason, detection.Suggestion,
	)
	return Result{
		Status: StatusError,
		Output: message,
		Meta: map[string]string{
			"exit_code":    "-1",
			"safety_block": "interactive_command",
			"safety_cmd":   detection.Command,
		},
		Display: Display{
			Summary: "Blocked interactive command: " + detection.Command,
			Kind:    "shell",
		},
	}
}

func commandExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode()
	}
	return -1
}

func formatBashOutput(stdout string, stderr string, exitCode int) string {
	parts := []string{}
	stdout = strings.TrimRight(stdout, "\r\n")
	stderr = strings.TrimRight(stderr, "\r\n")
	// Redact high-confidence secrets a command may have printed, so they are not
	// echoed back into the model context (additive to the configured-key scrub
	// done at the registry boundary).
	stdout, outFindings := secrets.Redact(stdout)
	stderr, errFindings := secrets.Redact(stderr)
	if stdout != "" {
		parts = append(parts, "stdout:\n"+stdout)
	}
	if stderr != "" {
		parts = append(parts, "stderr:\n"+stderr)
	}
	if exitCode != 0 {
		parts = append(parts, fmt.Sprintf("exit_code: %d", exitCode))
	}
	if n := len(outFindings) + len(errFindings); n > 0 {
		parts = append(parts, fmt.Sprintf("[zero] redacted %d likely secret(s) from this output before showing it.", n))
	}
	if len(parts) == 0 {
		return "Command completed with no output."
	}
	return strings.Join(parts, "\n")
}

// bashOutputBudgetBytes caps each of stdout/stderr shown to the model (32 KiB
// ≈ 8k tokens per stream). bash is the one tool that can emit unbounded output
// (`cat large.log`, `find /`, verbose test runs); a chatty build log at the old
// 96 KiB budget injected ~24k tokens per call and was then re-billed on every
// following turn. Head+tail truncation keeps both the start and the end of an
// oversized stream, since build/test failures usually surface at the tail, and
// the full captured text is spilled to disk so nothing is lost — the model
// greps/reads the spill instead of re-running the command.
const bashOutputBudgetBytes = 32 * 1024

// bashCaptureBudgetBytes bounds the head and tail retained IN MEMORY per
// stream during capture. Deliberately larger than the emit budget: the extra
// retained bytes never reach the transcript, but they make the spill file —
// the model's recovery path — cover 6× more of the output.
const bashCaptureBudgetBytes = 96 * 1024

// prepareBashOutput keeps direct callers on the established positional budget.
// Registry calls retain the existing bounded capture but leave final semantic
// reduction and spill creation to the post-redaction registry boundary.
func prepareBashOutput(out string, outTotal int, errStr string, errTotal int, meta map[string]string, directBudget bool) (string, string, bool) {
	if directBudget {
		return budgetBashCapture(out, outTotal, errStr, errTotal, meta)
	}
	outText := sectionWithCaptureGap(out, outTotal)
	errText := sectionWithCaptureGap(errStr, errTotal)
	truncated := outTotal > len(out) || errTotal > len(errStr)
	if meta != nil {
		meta["raw_bytes"] = strconv.Itoa(outTotal + errTotal)
		meta["emitted_bytes"] = strconv.Itoa(len(outText) + len(errText))
		meta["estimated_tokens"] = strconv.Itoa(estimatedTokensFromBytes(len(outText) + len(errText)))
		if truncated {
			meta["truncated"] = "true"
			meta["truncation_reason"] = "capture_budget"
		}
	}
	return outText, errText, truncated
}

// budgetBashCapture is budgetBashOutput for the streaming-capture path: outTotal
// and errTotal are the true byte counts (from boundedBuffer.total), which may
// exceed the retained strings when the middle was dropped during capture. Meta's
// raw_bytes therefore reflects everything the command produced, not just what was
// kept in memory.
func budgetBashCapture(out string, outTotal int, errStr string, errTotal int, meta map[string]string) (string, string, bool) {
	outText, outRaw, outTrunc := truncateHeadTailWithTotal(out, outTotal, bashOutputBudgetBytes)
	errText, errRaw, errTrunc := truncateHeadTailWithTotal(errStr, errTotal, bashOutputBudgetBytes)
	truncated := outTrunc || errTrunc
	if truncated {
		if spillPath := spillBashStreams(out, outTotal, errStr, errTotal); spillPath != "" {
			hint := "\n[zero] captured output saved to " + spillPath + " (grep or read_file it instead of re-running)"
			if errTrunc {
				errText += hint
			} else {
				outText += hint
			}
			if meta != nil {
				meta["spill_path"] = spillPath
			}
		}
	}
	if meta != nil {
		emitted := len(outText) + len(errText)
		meta["raw_bytes"] = strconv.Itoa(outRaw + errRaw)
		meta["emitted_bytes"] = strconv.Itoa(emitted)
		meta["estimated_tokens"] = strconv.Itoa(estimatedTokensFromBytes(emitted))
		if truncated {
			meta["truncated"] = "true"
		}
	}
	return outText, errText, truncated
}

// spillBashStreams writes the retained (capture-bounded) stdout and stderr to
// the spill directory as one sectioned file and returns its path, or "" when
// spilling fails. The spill holds up to bashCaptureBudgetBytes of head and
// tail per stream — everything zero kept in memory, which is more than the
// emit budget shows the model but not necessarily the whole output. When the
// capture itself dropped the middle of a stream (total exceeds the retained
// bytes), a gap marker is inserted at the head/tail junction so the spilled
// log never reads as contiguous when it is not.
func spillBashStreams(stdout string, stdoutTotal int, stderr string, stderrTotal int) string {
	stdout = sectionWithCaptureGap(stdout, stdoutTotal)
	stderr = sectionWithCaptureGap(stderr, stderrTotal)
	var combined strings.Builder
	combined.Grow(len(stdout) + len(stderr) + 64)
	combined.WriteString("### stdout\n")
	combined.WriteString(stdout)
	if !strings.HasSuffix(stdout, "\n") {
		combined.WriteString("\n")
	}
	combined.WriteString("### stderr\n")
	combined.WriteString(stderr)
	return spillTruncatedOutput("bash", combined.String())
}

// sectionWithCaptureGap marks the point where boundedBuffer dropped the middle
// of a stream. When total exceeds the retained bytes, the retained text is the
// frozen head (bashCaptureBudgetBytes, always full once overflow happened)
// followed immediately by the rolling tail — the junction sits at the head cap,
// snapped back to a rune boundary.
func sectionWithCaptureGap(text string, total int) string {
	if total <= len(text) || len(text) <= bashCaptureBudgetBytes {
		return text
	}
	head := utf8Prefix(text, bashCaptureBudgetBytes)
	marker := fmt.Sprintf("\n[zero] capture gap: %d bytes omitted from the middle of this stream\n", total-len(text))
	return head + marker + text[len(head):]
}

// boundedBuffer is an io.Writer that retains at most headCap bytes from the start
// and tailCap bytes from the end of a stream while counting the total written, so
// a command emitting unbounded output (`cat huge.log`, `yes`) cannot grow Zero's
// memory: the middle is discarded as it arrives instead of buffered whole and then
// truncated. total records the full size for the truncation marker even though the
// middle is never held. Not safe for concurrent writes; exec drives Stdout and
// Stderr from separate goroutines, so each stream gets its own buffer.
type boundedBuffer struct {
	head    []byte
	headCap int
	tail    []byte
	tailCap int
	total   int
}

func newBoundedBuffer(headCap, tailCap int) *boundedBuffer {
	return &boundedBuffer{headCap: headCap, tailCap: tailCap}
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.total += n
	// Fill the head until it reaches headCap; the head is written once and frozen.
	if len(b.head) < b.headCap {
		take := b.headCap - len(b.head)
		if take > len(p) {
			take = len(p)
		}
		b.head = append(b.head, p[:take]...)
		p = p[take:]
	}
	// Anything past the head feeds a rolling tail that keeps only the last tailCap
	// bytes. Compact once it grows past 2×tailCap so the append stays amortized O(1)
	// while memory never exceeds ~2×tailCap.
	if len(p) > 0 && b.tailCap > 0 {
		b.tail = append(b.tail, p...)
		if len(b.tail) > 2*b.tailCap {
			b.tail = append(b.tail[:0], b.tail[len(b.tail)-b.tailCap:]...)
		}
	}
	return n, nil
}

// retained returns the kept head+tail bytes (marker-less) as a string. The junction
// between head and tail lands in the middle, which the display budget trims away;
// callers that need the true size read total separately.
func (b *boundedBuffer) retained() string {
	if len(b.tail) > b.tailCap {
		// Not yet compacted since the last overflow; expose only the last tailCap.
		return string(b.head) + string(b.tail[len(b.tail)-b.tailCap:])
	}
	return string(b.head) + string(b.tail)
}

// truncateHeadTailWithTotal head+tail-truncates value to maxBytes, using total —
// the full original byte count — for the "N bytes omitted" marker and the raw
// count. total may exceed len(value) when the middle was already discarded during
// bounded capture (boundedBuffer): value then holds only the retained head+tail,
// and this trims it to the display budget while still reporting the true total.
func truncateHeadTailWithTotal(value string, total, maxBytes int) (string, int, bool) {
	if maxBytes <= 0 || total <= maxBytes {
		return value, total, false
	}
	marker := fmt.Sprintf("\n[zero] output truncated: %d bytes omitted from the middle — redirect to a file and read_file a range for the full text\n", total-maxBytes)
	budget := maxBytes - len(marker)
	if budget < 0 {
		budget = 0
	}
	head := budget / 2
	tail := budget - head
	return utf8Prefix(value, head) + marker + utf8Suffix(value, tail), total, true
}

func formatBashOutputWithShellHint(stdout string, stderr string, exitCode int, meta map[string]string) string {
	output := formatBashOutput(stdout, stderr, exitCode)
	if issue := detectShellOutputIssueForRuntime(stdout+"\n"+stderr, detectShellRuntime(runtime.GOOS)); issue != nil {
		meta["shell_issue"] = issue.Kind
		output = appendShellIssueHint(output, *issue)
	}
	return output
}
