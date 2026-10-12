package tools

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Gitlawb/zero/internal/execution"
	"github.com/Gitlawb/zero/internal/sandbox"
)

func TestIndependentExecCommandConstructorsShareDefaultManager(t *testing.T) {
	root := execTestRoot(t)
	execTool := NewScopedExecCommandTool(root, nil, nil)
	writeTool := NewWriteStdinTool(nil)

	start := execTool.Run(context.Background(), map[string]any{
		"cmd":           helperCommand("sleep"),
		"yield_time_ms": 10,
	})
	if start.Status != StatusOK {
		t.Fatalf("exec_command start status = %s: %s", start.Status, start.Output)
	}
	sessionID, err := strconv.Atoi(start.Meta["session_id"])
	if err != nil {
		t.Fatalf("session_id is not numeric: %v", err)
	}

	poll := writeTool.Run(context.Background(), map[string]any{
		"session_id":    sessionID,
		"yield_time_ms": 30000,
	})
	if poll.Status != StatusOK {
		t.Fatalf("write_stdin poll status = %s: %s", poll.Status, poll.Output)
	}
	if poll.Meta["exit_code"] != "0" {
		t.Fatalf("expected shared manager to find completed session, got meta=%#v output=%q", poll.Meta, poll.Output)
	}
}

func TestExecCommandToolDescribesHostStateEscalation(t *testing.T) {
	tool := NewScopedExecCommandTool(t.TempDir(), nil, nil)
	schema := tool.Parameters()
	descriptionParts := []string{tool.Description()}
	for _, property := range schema.Properties {
		descriptionParts = append(descriptionParts, property.Description)
	}
	description := strings.ToLower(strings.Join(descriptionParts, " "))
	for _, want := range []string{
		"sandbox_permissions",
		"require_escalated",
		"host/global process",
		"sandbox namespaces",
	} {
		if !strings.Contains(description, want) {
			t.Fatalf("expected exec_command escalation guidance %q, got %q", want, description)
		}
	}
}

func TestExecCommandToolDescribesHostShellSyntax(t *testing.T) {
	tool := NewScopedExecCommandTool(t.TempDir(), nil, nil)
	schema := tool.Parameters()
	descriptionParts := []string{tool.Description()}
	for _, property := range schema.Properties {
		descriptionParts = append(descriptionParts, property.Description)
	}
	description := strings.ToLower(strings.Join(descriptionParts, " "))

	if runtime.GOOS == "windows" {
		shell := detectShellRuntime(runtime.GOOS)
		if shell.Kind == shellKindPowerShell {
			for _, want := range []string{"powershell", "cwd", "get-childitem", "select-string", "$env:name"} {
				if !strings.Contains(description, want) {
					t.Fatalf("expected Windows PowerShell guidance %q in exec_command description, got %q", want, description)
				}
			}
		} else if !strings.Contains(description, "cmd.exe") || !strings.Contains(description, "cwd") {
			t.Fatalf("expected Windows cmd.exe fallback guidance in exec_command description, got %q", description)
		}
	}
}

func TestExecCommandReturnsSessionAndWriteStdinPollsCompletion(t *testing.T) {
	root := execTestRoot(t)
	manager := newExecSessionManager()
	execTool := NewScopedExecCommandTool(root, nil, manager)
	writeTool := NewWriteStdinTool(manager)

	start := execTool.Run(context.Background(), map[string]any{
		"cmd":           helperCommand("sleep"),
		"yield_time_ms": 10,
	})
	if start.Status != StatusOK {
		t.Fatalf("exec_command start status = %s: %s", start.Status, start.Output)
	}
	if start.Meta["session_id"] == "" {
		t.Fatalf("expected running session metadata, got %#v output=%q", start.Meta, start.Output)
	}
	if start.ExecutionOutcome == nil || start.ExecutionOutcome.State != execution.StateRetained || start.ExecutionOutcome.Kind != execution.OutcomeRunning {
		t.Fatalf("running execution outcome = %#v, want retained/running", start.ExecutionOutcome)
	}
	if start.ExecutionOutcome.ProcessID != start.Meta["session_id"] {
		t.Fatalf("process id = %q, want session id %q", start.ExecutionOutcome.ProcessID, start.Meta["session_id"])
	}
	if !strings.Contains(start.Output, `chars "\u0003"`) {
		t.Fatalf("running session output should explain Ctrl-C cleanup, got %q", start.Output)
	}
	sessionID, err := strconv.Atoi(start.Meta["session_id"])
	if err != nil {
		t.Fatalf("session_id is not numeric: %v", err)
	}

	poll := writeTool.Run(context.Background(), map[string]any{
		"session_id":    sessionID,
		"yield_time_ms": 30000,
	})
	if poll.Status != StatusOK {
		t.Fatalf("write_stdin poll status = %s: %s", poll.Status, poll.Output)
	}
	if !strings.Contains(poll.Output, "woke up") {
		t.Fatalf("expected final command output, got %q", poll.Output)
	}
	if poll.Meta["exit_code"] != "0" {
		t.Fatalf("expected exit_code 0, got %#v", poll.Meta)
	}
	if poll.ExecutionOutcome == nil || poll.ExecutionOutcome.State != execution.StateCompleted || poll.ExecutionOutcome.Kind != execution.OutcomeSuccess {
		t.Fatalf("completed execution outcome = %#v, want completed/success", poll.ExecutionOutcome)
	}
}

func TestExecCommandRequireEscalatedBypassesNativeSandboxAfterApproval(t *testing.T) {
	root := execTestRoot(t)
	manager := newExecSessionManager()
	registry := NewRegistry()
	registry.Register(NewScopedExecCommandTool(root, nil, manager))
	engine := sandbox.NewEngine(sandbox.EngineOptions{
		WorkspaceRoot: root,
		Policy:        sandbox.DefaultPolicy(),
		Backend: sandbox.Backend{
			Name:            sandbox.BackendLinuxBwrap,
			Available:       true,
			Executable:      "/nonexistent/zero-linux-sandbox-stub",
			CommandWrapping: true,
			NativeIsolation: true,
		},
	})

	result := registry.RunWithOptions(context.Background(), ExecCommandToolName, map[string]any{
		"cmd":                 helperCommand("success"),
		"sandbox_permissions": string(SandboxPermissionsRequireEscalated),
	}, RunOptions{
		PermissionGranted: true,
		Sandbox:           engine,
		PermissionMode:    string(sandbox.PermissionModeAsk),
	})

	if result.Status != StatusOK || !strings.Contains(result.Output, "hello from bash") {
		t.Fatalf("expected approved require_escalated exec_command to run direct, got %s: %q", result.Status, result.Output)
	}
	if result.Meta["sandbox_wrapped"] == "true" {
		t.Fatalf("require_escalated exec_command must not be wrapped; meta=%#v", result.Meta)
	}
	if result.ExecutionRequest == nil || !executionRequestHasCapability(*result.ExecutionRequest, execution.CapabilityUnrestricted, "host") {
		t.Fatalf("escalated execution request must record unrestricted host capability: %#v", result.ExecutionRequest)
	}
	if !executionRequestHasCapability(*result.ExecutionRequest, execution.CapabilityExternalNetwork, "") {
		t.Fatalf("escalated execution request must record external network capability: %#v", result.ExecutionRequest)
	}
}

// TestExecCommandRequireEscalatedBypassesMsysGuardAfterApproval mirrors
// TestBashToolRequireEscalatedMsysGuard for exec_command: the MSYS sandbox
// guard exists only because MSYS/Cygwin coreutils fail under the
// write-restricted sandbox, so once require_escalated is actually approved
// (unsandboxed execution), the guard must not block the same command it was
// meant to let escalate past.
func TestExecCommandRequireEscalatedBypassesMsysGuardAfterApproval(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only MSYS sandbox guard")
	}
	root := execTestRoot(t)
	manager := newExecSessionManager()
	registry := NewRegistry()
	registry.Register(NewScopedExecCommandTool(root, nil, manager))
	engine := sandbox.NewEngine(sandbox.EngineOptions{
		WorkspaceRoot: root,
		Policy:        sandbox.DefaultPolicy(),
		Backend:       sandbox.Backend{Name: sandbox.BackendUnavailable, Message: "native sandbox unavailable"},
	})
	msysCommand := "cat somefile.txt"
	if detectShellRuntime(runtime.GOOS).Kind == shellKindPowerShell {
		// cat is a native Get-Content alias in PowerShell, so use an executable
		// name that still exercises the MSYS guard.
		msysCommand = "grep pattern somefile.txt"
	}

	result := registry.RunWithOptions(context.Background(), ExecCommandToolName, map[string]any{
		"cmd":                 msysCommand,
		"sandbox_permissions": string(SandboxPermissionsRequireEscalated),
	}, RunOptions{
		PermissionGranted: true,
		Sandbox:           engine,
		PermissionMode:    string(sandbox.PermissionModeAsk),
	})

	// Assert on the preflight block sentinel (exit_code "-1", set only by
	// shellIssueBlockResult) rather than shell_issue: once the guard is
	// bypassed, the command actually runs, and its real, PATH-dependent output
	// could otherwise trip the unrelated
	// post-execution detectShellOutputIssue heuristic and make this
	// assertion flaky for reasons unrelated to the guard under test.
	if result.Meta["exit_code"] == "-1" {
		t.Fatalf("expected approved require_escalated to bypass the MSYS guard, got blocked: %#v", result)
	}
}

func TestExecCommandReturnsExitCodeWhenCommandCompletesDuringInitialYield(t *testing.T) {
	root := execTestRoot(t)
	manager := newExecSessionManager()
	execTool := NewScopedExecCommandTool(root, nil, manager)

	result := execTool.Run(context.Background(), map[string]any{
		"cmd":           helperCommand("success"),
		"yield_time_ms": 30000,
	})
	if result.Status != StatusOK {
		t.Fatalf("exec_command status = %s: %s", result.Status, result.Output)
	}
	if result.Meta["session_id"] != "" {
		t.Fatalf("completed command must not return session_id, got %#v", result.Meta)
	}
	if result.Meta["exit_code"] != "0" {
		t.Fatalf("exit_code = %#v, want 0", result.Meta)
	}
	if manager.Len() != 0 {
		t.Fatalf("completed command should be removed immediately, manager has %d sessions", manager.Len())
	}
	if result.ExecutionOutcome == nil || result.ExecutionOutcome.State != execution.StateCompleted || result.ExecutionOutcome.Kind != execution.OutcomeSuccess {
		t.Fatalf("execution outcome = %#v, want completed/success", result.ExecutionOutcome)
	}
}

func TestExecCommandClampsOversizedYieldInsteadOfRejecting(t *testing.T) {
	result := NewScopedExecCommandTool(t.TempDir(), nil, newExecSessionManager()).Run(context.Background(), map[string]any{
		"cmd":           helperCommand("success"),
		"yield_time_ms": 120000,
	})
	if result.Status != StatusOK || result.Meta["exit_code"] != "0" {
		t.Fatalf("oversized yield should be clamped, got status=%s meta=%#v output=%q", result.Status, result.Meta, result.Output)
	}
}

func TestExecCommandReportsWorkspaceChanges(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell syntax")
	}
	root := execTestRoot(t)
	result := NewScopedExecCommandTool(root, nil, newExecSessionManager()).Run(context.Background(), map[string]any{
		"cmd":           "mkdir -p src node_modules/pkg && printf 'export {}' > src/main.ts && printf generated > node_modules/pkg/index.js",
		"yield_time_ms": 30000,
	})
	if result.Status != StatusOK {
		t.Fatalf("exec_command status = %s: %s", result.Status, result.Output)
	}
	if len(result.ChangedFiles) != 1 || result.ChangedFiles[0] != "src/main.ts" {
		t.Fatalf("ChangedFiles = %#v, want source file without generated tree", result.ChangedFiles)
	}
	if result.ExecutionOutcome == nil || len(result.ExecutionOutcome.Changes) != 2 {
		t.Fatalf("typed changes = %#v, want source file and bounded generated-tree summary", result.ExecutionOutcome)
	}
	if len(result.ChangeSummaries) != 1 || result.ChangeSummaries[0].Path != "node_modules/" || !result.ChangeSummaries[0].Aggregated {
		t.Fatalf("ChangeSummaries = %#v, want node_modules aggregate", result.ChangeSummaries)
	}
}

func TestExecCommandApplicationFailureHasTypedOutcome(t *testing.T) {
	result := NewScopedExecCommandTool(t.TempDir(), nil, newExecSessionManager()).Run(context.Background(), map[string]any{
		"cmd":           helperCommand("fail"),
		"yield_time_ms": 30000,
	})

	if result.Status != StatusError {
		t.Fatalf("status = %s, want error", result.Status)
	}
	if result.ExecutionOutcome == nil || result.ExecutionOutcome.State != execution.StateFailed || result.ExecutionOutcome.Kind != execution.OutcomeApplicationFailure {
		t.Fatalf("execution outcome = %#v, want failed/application_failure", result.ExecutionOutcome)
	}
	wantExitCode := helperFailureExitCode()
	if result.ExecutionOutcome.Exit == nil || result.ExecutionOutcome.Exit.Code != wantExitCode {
		t.Fatalf("execution exit = %#v, want code %d", result.ExecutionOutcome.Exit, wantExitCode)
	}
}

func TestExecCommandUsesStructuredAdapterDenial(t *testing.T) {
	scope := filepath.Join(t.TempDir(), ".zero")
	result := execToolResult(execToolResultInput{
		commandText: "mkdir .zero",
		exited:      true,
		exitCode:    1,
		enforcement: execution.Enforcement{Backend: string(sandbox.BackendLinuxBwrap), Level: string(sandbox.EnforcementNative)},
		report: execution.AdapterReport{Denial: &execution.Denial{
			Capability:  execution.Capability{Kind: execution.CapabilityProtectedMetadata, Scope: scope},
			Source:      execution.DenialSourceConfiguredPolicy,
			Reason:      "protected workspace metadata cannot be created",
			Recoverable: true,
			NextAction:  execution.DenialNextActionRequestApproval,
		}},
	})

	if result.ExecutionOutcome == nil || result.ExecutionOutcome.State != execution.StateDenied || result.ExecutionOutcome.Kind != execution.OutcomeEnforcementDenied {
		t.Fatalf("execution outcome = %#v, want denied/enforcement_denied", result.ExecutionOutcome)
	}
	if result.ExecutionOutcome.Denial == nil || result.ExecutionOutcome.Denial.Capability.Scope != scope {
		t.Fatalf("structured denial = %#v, want exact scope %q", result.ExecutionOutcome.Denial, scope)
	}
	if result.Meta["sandbox_denial_capability"] != string(execution.CapabilityProtectedMetadata) || result.Meta["sandbox_denial_scope"] != scope {
		t.Fatalf("compatibility metadata lost structured denial: %#v", result.Meta)
	}
}

func TestExecCommandDoesNotInferNativeSandboxDenialFromOutput(t *testing.T) {
	result := execToolResult(execToolResultInput{
		commandText: "touch \"$HOME/probe\"",
		output:      "touch: cannot touch '/home/user/probe': Read-only file system",
		exited:      true,
		exitCode:    1,
		enforcement: execution.Enforcement{Backend: string(sandbox.BackendLinuxBwrap), Level: string(sandbox.EnforcementNative)},
	})

	if result.ExecutionOutcome == nil || result.ExecutionOutcome.State != execution.StateFailed || result.ExecutionOutcome.Kind != execution.OutcomeApplicationFailure {
		t.Fatalf("execution outcome = %#v, want failed/application_failure", result.ExecutionOutcome)
	}
	if result.ExecutionOutcome.Denial != nil {
		t.Fatalf("command output created a typed denial: %#v", result.ExecutionOutcome.Denial)
	}
	if result.Meta[SandboxLikelyDeniedMeta] == "true" {
		t.Fatalf("command output created sandbox-denial metadata: %#v", result.Meta)
	}
}

func executionRequestHasCapability(request execution.Request, kind execution.CapabilityKind, scope string) bool {
	for _, capability := range request.Capabilities {
		if capability.Kind == kind && (scope == "" || capability.Scope == scope) {
			return true
		}
	}
	return false
}

func TestExecCommandForegroundServerReturnsSessionAndServesHTTP(t *testing.T) {
	root := execTestRoot(t)
	manager := newExecSessionManager()
	execTool := NewScopedExecCommandTool(root, nil, manager)
	writeTool := NewWriteStdinTool(manager)

	start := execTool.Run(context.Background(), map[string]any{
		"cmd":           helperCommand("http-server"),
		"yield_time_ms": 500,
	})
	if start.Status != StatusOK {
		t.Fatalf("exec_command start status = %s: %s", start.Status, start.Output)
	}
	sessionID, err := strconv.Atoi(start.Meta["session_id"])
	if err != nil {
		t.Fatalf("foreground server should return session_id, meta=%#v output=%q", start.Meta, start.Output)
	}
	addr := parseListeningAddress(start.Output)
	if addr == "" {
		t.Fatalf("server output did not include listening address: %q", start.Output)
	}
	t.Cleanup(func() {
		writeTool.Run(context.Background(), map[string]any{
			"session_id": sessionID,
			"chars":      "\u0003",
		})
	})

	response, err := http.Get("http://" + addr)
	if err != nil {
		t.Fatalf("foreground exec server was not reachable at %s: %v; output=%q", addr, err, start.Output)
	}
	defer response.Body.Close()
	bytes, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(bytes) != "zero-server-ok" {
		t.Fatalf("server response = %q", string(bytes))
	}
}

func parseListeningAddress(output string) string {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		for index, field := range fields {
			if field == "listening" && index+1 < len(fields) {
				return strings.TrimSpace(fields[index+1])
			}
		}
	}
	return ""
}

func TestExecCommandReapsFinishedUnpolledSession(t *testing.T) {
	root := execTestRoot(t)
	manager := execution.NewProcessManager(execution.ProcessManagerOptions{CompletedRetention: 10 * time.Millisecond})
	execTool := NewScopedExecCommandTool(root, nil, manager)

	start := execTool.Run(context.Background(), map[string]any{
		"cmd":           helperCommand("sleep"),
		"yield_time_ms": 10,
	})
	if start.Status != StatusOK {
		t.Fatalf("exec_command start status = %s: %s", start.Status, start.Output)
	}
	sessionID, err := strconv.Atoi(start.Meta["session_id"])
	if err != nil {
		t.Fatalf("session_id is not numeric: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := manager.Snapshot(sessionID); !ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %d was not reaped; manager has %d sessions", sessionID, manager.Len())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestExecToolResultSurfacesBufferTruncationOutsideByteBudget(t *testing.T) {
	hugeOutput := strings.Repeat("y", maxExecOutputBufferBytes+1024)

	result := execToolResult(execToolResultInput{
		commandText:           "cmd",
		output:                hugeOutput,
		outputBufferTruncated: true,
		sessionID:             1,
		exited:                false,
		maxOutputTokens:       1, // the schema's own declared minimum
	})

	if !strings.Contains(result.Output, execOutputBufferTruncatedMessage) {
		t.Fatalf("result output should contain the buffer-truncation notice even at the smallest max_output_tokens, got %q", result.Output[:min(len(result.Output), 200)])
	}
	if result.Meta["output_buffer_truncated"] != "true" {
		t.Fatalf("result meta should flag output_buffer_truncated, got %#v", result.Meta)
	}
	if !result.Truncated {
		t.Fatal("result should report Truncated when the buffer dropped output")
	}
}

// TestCollectRespectsDeadlineUnderContinuousOutput asserts collect() returns
// close to its requested deadline even while output keeps arriving. Before
// the corresponding fix, the deadline was only checked in the branch reached
// when drainString returns empty — a background process producing output
// fast/continuously enough to keep the buffer perpetually non-empty could
// theoretically starve that check indefinitely. This synthetic writer
// (8 goroutines, tight loop) did not reliably reproduce that starvation under
// Go's scheduler in practice — the reader still won the race often enough —
// so this test doesn't prove the old code could hang; it just pins down the
// intended behavior (bounded by wait) going forward.
func resilientTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "zero-exec-interrupt-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() {
		deadline := time.Now().Add(5 * time.Second)
		for {
			if err := os.RemoveAll(dir); err == nil {
				return
			}
			if time.Now().After(deadline) {
				// Best-effort: a leaked temp dir is not worth failing the test.
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	return dir
}

func TestWriteStdinInterruptTerminatesSession(t *testing.T) {
	root := resilientTempDir(t)
	manager := newExecSessionManager()
	execTool := NewScopedExecCommandTool(root, nil, manager)
	writeTool := NewWriteStdinTool(manager)

	start := execTool.Run(context.Background(), map[string]any{
		"cmd":           helperCommand("long-sleep"),
		"yield_time_ms": 10,
	})
	if start.Status != StatusOK {
		t.Fatalf("exec_command start status = %s: %s", start.Status, start.Output)
	}
	sessionID, err := strconv.Atoi(start.Meta["session_id"])
	if err != nil {
		t.Fatalf("session_id is not numeric: %v", err)
	}

	// The operation under test: write_stdin "\x03" must itself terminate the
	// session (exec_command.go's Ctrl-C branch). This is what the regression
	// guards — terminating the session here directly would let the test pass even
	// if that branch were deleted.
	interrupted := writeTool.Run(context.Background(), map[string]any{
		"session_id":    sessionID,
		"chars":         "\x03",
		"yield_time_ms": 30000,
	})

	if interrupted.Status != StatusOK {
		t.Fatalf("interrupted session status = %s: %s", interrupted.Status, interrupted.Output)
	}
	if interrupted.Meta["session_id"] != "" {
		t.Fatalf("interrupted session should not remain running, meta=%#v output=%q", interrupted.Meta, interrupted.Output)
	}
	if interrupted.Meta["exit_code"] == "" {
		t.Fatalf("interrupted session should report exit_code, meta=%#v output=%q", interrupted.Meta, interrupted.Output)
	}
	if interrupted.Meta["interrupted"] != "true" {
		t.Fatalf("interrupted session should report interrupted metadata, meta=%#v output=%q", interrupted.Meta, interrupted.Output)
	}
}

func TestWriteStdinRejectsInputForNonTTYSession(t *testing.T) {
	root := execTestRoot(t)
	manager := newExecSessionManager()
	execTool := NewScopedExecCommandTool(root, nil, manager)
	writeTool := NewWriteStdinTool(manager)

	start := execTool.Run(context.Background(), map[string]any{
		"cmd":           helperCommand("long-sleep"),
		"yield_time_ms": 10,
	})
	if start.Status != StatusOK {
		t.Fatalf("exec_command start status = %s: %s", start.Status, start.Output)
	}
	sessionID, err := strconv.Atoi(start.Meta["session_id"])
	if err != nil {
		t.Fatalf("session_id is not numeric: %v", err)
	}

	result := writeTool.Run(context.Background(), map[string]any{
		"session_id":    sessionID,
		"chars":         "hello\n",
		"yield_time_ms": 10,
	})
	if result.Status != StatusError {
		t.Fatalf("write_stdin status = %s, want error", result.Status)
	}
	if !strings.Contains(result.Output, "does not accept stdin") {
		t.Fatalf("unexpected output: %q", result.Output)
	}
	manager.Stop(sessionID)
}

// execTestRoot is t.TempDir with a cleanup that tolerates Windows still holding
// the directory.
//
// A terminated process does not release its handles the instant the session
// reports it exited, and Windows refuses to remove a directory with an open
// handle where POSIX does not. t.TempDir's own cleanup removes once and fails
// the test on a sharing violation, which turns ordinary teardown timing into a
// test failure with nothing actually wrong underneath it.
func execTestRoot(t *testing.T) string {
	t.Helper()

	root, err := os.MkdirTemp("", "zero-exec-test-")
	if err != nil {
		t.Fatalf("create test root: %v", err)
	}
	t.Cleanup(func() {
		deadline := time.Now().Add(10 * time.Second)
		for {
			if err := os.RemoveAll(root); err == nil {
				return
			}
			if time.Now().After(deadline) {
				// Deliberately not a failure. The directory is under the OS temp
				// root and the process holding it has already been terminated, so
				// the worst case is a stale directory, not a broken test.
				t.Logf("test root %s still held after the cleanup deadline; leaving it", root)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	return root
}

func TestWriteStdinStopIntentTerminatesNonTTYSession(t *testing.T) {
	for _, chars := range []string{`\u0003`, "exit\n"} {
		root := execTestRoot(t)
		manager := newExecSessionManager()
		execTool := NewScopedExecCommandTool(root, nil, manager)
		writeTool := NewWriteStdinTool(manager)

		start := execTool.Run(context.Background(), map[string]any{
			"cmd":           helperCommand("long-sleep"),
			"yield_time_ms": 10,
		})
		if start.Status != StatusOK {
			t.Fatalf("exec_command start status = %s: %s", start.Status, start.Output)
		}
		sessionID, err := strconv.Atoi(start.Meta["session_id"])
		if err != nil {
			t.Fatalf("session_id is not numeric: %v", err)
		}

		// Re-send the stop until the session reports it has exited, rather than
		// waiting a fixed slice and asserting on whatever is true by then.
		//
		// A single 1s yield used to be enough because the session ran under
		// cmd.exe. Under PowerShell the tree is slower to stand up and tear down,
		// and the FIRST stop in a fresh process pays the interpreter's cold start,
		// so it routinely returns with termination already under way but not
		// finished. The old assertion read that as "the stop did not work" and
		// failed, intermittently, on whichever of the two inputs happened to run
		// first. CI never saw it because the runners have PowerShell 7 while a
		// stock Windows box only has 5.1.
		//
		// Repeating the same input rather than polling with empty chars is
		// deliberate: Terminate is a no-op against an already-dead tree, so it is
		// safe to repeat, and it keeps the interrupt flag that an empty-chars poll
		// would drop, which is what the interrupted assertion below needs.
		var result Result
		// Every poll's output is kept, not just the last one. Each Continue
		// returns only what the session produced since the previous collection,
		// so a marker printed just before the poll that observes the exit lands
		// in the earlier result and would be invisible to a check against the
		// final one alone.
		var collected strings.Builder
		deadline := time.Now().Add(30 * time.Second)
		for {
			result = writeTool.Run(context.Background(), map[string]any{
				"session_id":    sessionID,
				"chars":         chars,
				"yield_time_ms": 500,
			})
			collected.WriteString(result.Output)
			if result.Status != StatusOK {
				t.Fatalf("stop input %q status = %s: %s", chars, result.Status, result.Output)
			}
			if result.Meta["session_id"] == "" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("stop input %q left the session running past the deadline, meta=%#v output=%q", chars, result.Meta, result.Output)
			}
		}
		if result.Meta["exit_code"] == "" {
			t.Fatalf("stop input %q should report exit_code, meta=%#v output=%q", chars, result.Meta, result.Output)
		}
		if result.Meta["interrupted"] != "true" {
			t.Fatalf("stop input %q should report interrupted metadata, meta=%#v output=%q", chars, result.Meta, result.Output)
		}
		// The session has to have been killed rather than allowed to finish.
		// Without this the retry loop makes the test unfalsifiable: the helper
		// sleeps 5s, well inside the deadline, so a terminate that did nothing at
		// all would let the sleep end on its own and every assertion above would
		// still hold. The interrupted flag does not close the hole either, since
		// it echoes the request rather than the outcome. The helper prints this
		// line only on the way out of a completed sleep, so its absence is what
		// separates the two, and it says nothing about how long the loop took.
		if strings.Contains(collected.String(), "long sleep finished") {
			t.Fatalf("stop input %q let the session run to completion instead of terminating it, output=%q", chars, collected.String())
		}
	}
}

func TestShouldInterruptExecSession(t *testing.T) {
	cases := []struct {
		chars string
		tty   bool
		want  bool
	}{
		{chars: "\x03", tty: false, want: true},
		{chars: `\u0003`, tty: false, want: true},
		{chars: `\\u0003`, tty: false, want: true},
		{chars: "^C", tty: false, want: true},
		{chars: "ctrl-c", tty: false, want: true},
		{chars: "control-c", tty: false, want: true},
		{chars: "sigint", tty: false, want: true},
		{chars: "interrupt", tty: false, want: true},
		{chars: "q", tty: false, want: true},
		{chars: "quit", tty: false, want: true},
		{chars: "exit\n", tty: false, want: true},
		{chars: "stop", tty: false, want: true},
		{chars: "kill", tty: false, want: true},
		{chars: "terminate", tty: false, want: true},
		{chars: "exit\n", tty: true, want: false},
		{chars: "quit", tty: true, want: false},
		{chars: "hello\n", tty: false, want: false},
		{chars: "hello\n", tty: true, want: false},
	}
	for _, tc := range cases {
		if got := shouldInterruptExecSession(tc.chars, tc.tty); got != tc.want {
			t.Fatalf("shouldInterruptExecSession(%q, tty=%v) = %v, want %v", tc.chars, tc.tty, got, tc.want)
		}
	}
}

func TestExecCommandTTYSessionAcceptsInputOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("pty transport is currently implemented for linux")
	}
	root := execTestRoot(t)
	manager := newExecSessionManager()
	execTool := NewScopedExecCommandTool(root, nil, manager)
	writeTool := NewWriteStdinTool(manager)

	start := execTool.Run(context.Background(), map[string]any{
		"cmd":           "read line; echo got:$line",
		"tty":           true,
		"yield_time_ms": 10,
	})
	if start.Status != StatusOK {
		t.Fatalf("exec_command start status = %s: %s", start.Status, start.Output)
	}
	if start.Meta["tty"] != "true" {
		t.Fatalf("expected tty metadata, got %#v output=%q", start.Meta, start.Output)
	}
	sessionID, err := strconv.Atoi(start.Meta["session_id"])
	if err != nil {
		t.Fatalf("session_id is not numeric: %v", err)
	}

	result := writeTool.Run(context.Background(), map[string]any{
		"session_id":    sessionID,
		"chars":         "hello\n",
		"yield_time_ms": 1000,
	})
	if result.Status != StatusOK {
		t.Fatalf("write_stdin status = %s: %s", result.Status, result.Output)
	}
	if !strings.Contains(result.Output, "got:hello") {
		t.Fatalf("expected PTY input output, got %q", result.Output)
	}
	if result.Meta["exit_code"] != "0" {
		t.Fatalf("expected exited session, got meta=%#v output=%q", result.Meta, result.Output)
	}
}

func TestExecSessionSnapshotsAndStopAll(t *testing.T) {
	root := execTestRoot(t)
	manager := newExecSessionManager()
	execTool := NewScopedExecCommandTool(root, nil, manager).(execCommandTool)

	start := execTool.Run(context.Background(), map[string]any{
		"cmd":           helperCommand("long-sleep"),
		"yield_time_ms": 10,
	})
	if start.Status != StatusOK {
		t.Fatalf("exec_command start status = %s: %s", start.Status, start.Output)
	}
	sessionID, err := strconv.Atoi(start.Meta["session_id"])
	if err != nil {
		t.Fatalf("session_id is not numeric: %v", err)
	}

	snapshots := execTool.ExecSessions()
	if len(snapshots) != 1 {
		t.Fatalf("expected one session snapshot, got %#v", snapshots)
	}
	if snapshots[0].ID != sessionID || snapshots[0].Command == "" || snapshots[0].Status != "running" {
		t.Fatalf("unexpected snapshot: %#v", snapshots[0])
	}

	stopped := execTool.StopAllExecSessions()
	if len(stopped) != 1 || stopped[0] != sessionID {
		t.Fatalf("StopAllExecSessions = %#v, want [%d]", stopped, sessionID)
	}
}

func TestWriteStdinPermissionForArgs(t *testing.T) {
	tool := NewWriteStdinTool(newExecSessionManager()).(writeStdinTool)
	for _, args := range []map[string]any{
		{"session_id": 1},
		{"session_id": 1, "chars": ""},
		{"session_id": 1, "chars": "\x03"},
	} {
		if got := tool.PermissionForArgs(args); got != PermissionAllow {
			t.Fatalf("PermissionForArgs(%#v) = %s, want allow", args, got)
		}
	}
	if got := tool.PermissionForArgs(map[string]any{"session_id": 1, "chars": "exit\n"}); got != PermissionPrompt {
		t.Fatalf("non-empty stdin PermissionForArgs = %s, want prompt", got)
	}
}

func TestRegistryHonorsWriteStdinArgumentPermission(t *testing.T) {
	registry := NewRegistry()
	registry.Register(NewWriteStdinTool(newExecSessionManager()))

	poll := registry.Run(context.Background(), WriteStdinToolName, map[string]any{"session_id": 9999})
	if poll.Status != StatusError || !strings.Contains(poll.Output, "still-running exec_command") {
		t.Fatalf("empty poll should reach tool without permission prompt, got status=%s output=%q", poll.Status, poll.Output)
	}

	send := registry.Run(context.Background(), WriteStdinToolName, map[string]any{
		"session_id": 9999,
		"chars":      "exit\n",
	})
	if send.Status != StatusError || !strings.Contains(send.Output, "Permission required for write_stdin") {
		t.Fatalf("non-empty stdin should require permission, got status=%s output=%q", send.Status, send.Output)
	}
}

func TestWriteStdinReportsUnknownSession(t *testing.T) {
	result := NewWriteStdinTool(newExecSessionManager()).Run(context.Background(), map[string]any{
		"session_id": 1234,
	})
	if result.Status != StatusError {
		t.Fatalf("status = %s, want error", result.Status)
	}
	// The message must lead with recovery guidance and still identify the id.
	if !strings.Contains(result.Output, "still-running exec_command") {
		t.Fatalf("unknown-session error must carry recovery guidance, got: %q", result.Output)
	}
	if !strings.Contains(result.Output, "1234") {
		t.Fatalf("unknown-session error must name the offending id, got: %q", result.Output)
	}
}

// The runtime rejects session_id < 1 (intArg min 1); the advertised schema must
// say the same so a model sees the constraint before it probes id 0.
func TestWriteStdinSchemaPinsSessionIDMinimum(t *testing.T) {
	schema := NewWriteStdinTool(nil).(writeStdinTool).parameters
	prop, ok := schema.Properties["session_id"]
	if !ok {
		t.Fatal("session_id property missing from write_stdin schema")
	}
	if prop.Minimum == nil || *prop.Minimum != 1 {
		t.Fatalf("session_id schema Minimum = %v, want 1 to match the runtime floor", prop.Minimum)
	}
}

// A missing, zero, negative, or non-integer session_id all mean the model has no
// live session, so write_stdin returns the SAME recovery guidance the no-live-
// session case uses (start a session with exec_command, or edit files directly),
// not a terse "session_id must be at least 1" that gives no way forward and names
// a minimum that nudges the model to probe ids 1, 2, 3... Sharing one id-invariant
// message also keeps a single repeated-failure signature, so any mix of these
// mistakes accumulates toward the halt instead of resetting the streak (#749).
func TestWriteStdinRequiresPositiveSessionID(t *testing.T) {
	tool := NewWriteStdinTool(newExecSessionManager())
	want := UnknownExecSessionError(0)
	for name, args := range map[string]map[string]any{
		"missing":     {},
		"nil":         {"session_id": nil},
		"zero":        {"session_id": 0},
		"negative":    {"session_id": -3},
		"non-integer": {"session_id": "abc"},
	} {
		t.Run(name, func(t *testing.T) {
			result := tool.Run(context.Background(), args)
			if result.Status != StatusError {
				t.Fatalf("status = %s, want error", result.Status)
			}
			if result.Output != want {
				t.Fatalf("output = %q,\n want the recovery message %q", result.Output, want)
			}
			if strings.Contains(result.Output, "must be at least 1") {
				t.Fatalf("still returns the terse minimum error: %q", result.Output)
			}
		})
	}
}

func TestTruncateExecOutputPreservesUTF8(t *testing.T) {
	output := strings.Repeat("界", 20)
	truncated, ok := truncateExecOutput(output, 2)
	if !ok {
		t.Fatal("expected output to truncate")
	}
	if !strings.Contains(truncated, "[zero] output truncated") {
		t.Fatalf("missing truncation marker: %q", truncated)
	}
	if !utf8.ValidString(truncated) {
		t.Fatalf("truncated output is not valid UTF-8: %q", truncated)
	}
}

func TestFormatExecCommandOutputTTYAttachHint(t *testing.T) {
	running := formatExecCommandOutput("", 1007, false, 0, false, true)
	if !strings.Contains(running, "/attach 1007 reopens it") || !strings.Contains(running, "Do not end your turn while it is running") {
		t.Fatalf("tty running session should point at /attach and keep-poll guidance: %q", running)
	}
	exited := formatExecCommandOutput("", 1007, true, 0, false, true)
	if strings.Contains(exited, "/attach") {
		t.Fatalf("exited session should not mention /attach: %q", exited)
	}
	pipes := formatExecCommandOutput("", 1007, false, 0, false, false)
	if strings.Contains(pipes, "/attach") {
		t.Fatalf("non-tty session should not mention /attach: %q", pipes)
	}
}

func TestExecCommandInteractiveBlockSuggestsTTY(t *testing.T) {
	tool := NewScopedExecCommandTool(t.TempDir(), nil, newExecSessionManager())
	result := tool.Run(context.Background(), map[string]any{"cmd": "ssh host.example.com"})
	if result.Status != StatusError || result.Meta["safety_block"] != "interactive_command" {
		t.Fatalf("expected interactive safety block, got meta=%#v output=%q", result.Meta, result.Output)
	}
	if !strings.Contains(result.Output, "tty:true") {
		t.Fatalf("block output should suggest tty:true: %q", result.Output)
	}
}

func TestExecCommandTTYSkipsInteractiveBlock(t *testing.T) {
	tool := NewScopedExecCommandTool(t.TempDir(), nil, newExecSessionManager())
	// ssh to a bogus host fails fast on its own; the point is that the
	// interactive guard must not fire ahead of the tty path.
	result := tool.Run(context.Background(), map[string]any{
		"cmd": "ssh host.example.com", "tty": true, "yield_time_ms": 250,
	})
	if result.Meta["safety_block"] == "interactive_command" {
		t.Fatalf("tty:true should not hit the interactive block: %#v", result.Meta)
	}
}

func TestFormatExecCommandOutputNoNewPrivilegesHint(t *testing.T) {
	blocked := formatExecCommandOutput(`sudo: The "no new privileges" flag is set`, 1007, true, 1, false, true)
	if !strings.Contains(blocked, `sandbox_permissions "require_escalated"`) {
		t.Fatalf("no_new_privs failure should hint at require_escalated: %q", blocked)
	}
	normal := formatExecCommandOutput("some other error", 1007, true, 1, false, true)
	if strings.Contains(normal, "require_escalated") {
		t.Fatalf("unrelated failure should not hint at require_escalated: %q", normal)
	}
	blockedOK := formatExecCommandOutput(`sudo: The "no new privileges" flag is set`, 1007, true, 0, false, true)
	if strings.Contains(blockedOK, "require_escalated") {
		t.Fatalf("zero-exit output should not hint at require_escalated: %q", blockedOK)
	}
}
