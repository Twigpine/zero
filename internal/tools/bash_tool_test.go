package tools

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/execution"
	"github.com/Gitlawb/zero/internal/sandbox"
)

func TestMain(m *testing.M) {
	if len(os.Args) >= 3 && os.Args[1] == "--zero-bash-helper" {
		if os.Args[2] == "echo-arg" {
			// Echoes back exactly the one argument it received, to verify a
			// quoted, space-and-slash-containing argument (the same shape as
			// `python -c "print(15 / 3)"`) survives the shell invocation
			// intact instead of being truncated/corrupted.
			if len(os.Args) < 4 {
				fmt.Fprintln(os.Stderr, "echo-arg requires exactly one argument")
				os.Exit(1)
			}
			fmt.Println(os.Args[3])
			return
		}
		runBashToolHelper(os.Args[2])
		return
	}

	os.Exit(m.Run())
}

func runBashToolHelper(command string) {
	switch command {
	case "success":
		fmt.Println("hello from bash")
	case "stderr":
		fmt.Fprintln(os.Stderr, "warning from bash")
	case "fail":
		fmt.Println("before failure")
		fmt.Fprintln(os.Stderr, "failure details")
		os.Exit(7)
	case "pwd":
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(cwd)
	case "sleep":
		time.Sleep(250 * time.Millisecond)
		fmt.Println("woke up")
	case "long-sleep":
		time.Sleep(5 * time.Second)
		fmt.Println("long sleep finished")
	case "http-server", "delayed-http-server":
		if command == "delayed-http-server" {
			time.Sleep(time.Second)
		}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("listening", listener.Addr().String())
		server := &http.Server{
			Handler: http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				_, _ = response.Write([]byte("zero-server-ok"))
			}),
		}
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown helper command")
		os.Exit(2)
	}
}

func TestCoreToolsExposeShellTools(t *testing.T) {
	toolset := CoreToolsScoped(t.TempDir(), nil)
	byName := make(map[string]Tool, len(toolset))
	for _, tool := range toolset {
		byName[tool.Name()] = tool
	}

	for _, name := range []string{"exec_command", "write_stdin", "bash"} {
		tool, ok := byName[name]
		if !ok {
			t.Fatalf("expected core tools to include %s", name)
		}
		if tool.Safety().SideEffect != SideEffectShell {
			t.Fatalf("%s side effect = %s, want shell", name, tool.Safety().SideEffect)
		}
		wantPermission := PermissionPrompt
		if tool.Safety().Permission != wantPermission {
			t.Fatalf("%s permission = %s, want %s", name, tool.Safety().Permission, wantPermission)
		}
		if (name == "exec_command" || name == "write_stdin") && !tool.Safety().AdvertiseInAuto {
			t.Fatalf("%s should stay visible in auto mode so process creation and interaction are both available", name)
		}
	}
}

func TestBashToolDescribesHostShellSyntax(t *testing.T) {
	tool := NewScopedBashTool(t.TempDir(), nil)
	schema := tool.Parameters()
	descriptionParts := []string{tool.Description()}
	for _, property := range schema.Properties {
		descriptionParts = append(descriptionParts, property.Description)
	}
	description := strings.ToLower(strings.Join(descriptionParts, " "))
	if !strings.Contains(description, "sandbox_permissions") ||
		!strings.Contains(description, "require_escalated") ||
		!strings.Contains(description, "host/global process") ||
		!strings.Contains(description, "sandbox namespaces") {
		t.Fatalf("expected sandbox escalation guidance in bash description, got %q", description)
	}

	if runtime.GOOS == "windows" {
		shell := detectShellRuntime(runtime.GOOS)
		if shell.Kind == shellKindPowerShell {
			for _, want := range []string{"powershell", "cwd", "get-childitem", "select-string", "$env:name"} {
				if !strings.Contains(description, want) {
					t.Fatalf("expected Windows PowerShell guidance %q in bash description, got %q", want, description)
				}
			}
		} else if !strings.Contains(description, "cmd.exe") || !strings.Contains(description, "cwd") {
			t.Fatalf("expected Windows cmd.exe fallback guidance in bash description, got %q", description)
		}
		return
	}
	if !strings.Contains(description, "/bin/sh") {
		t.Fatalf("expected /bin/sh guidance in bash description, got %q", description)
	}
}

func TestDetectShellCommandIssueFlagsWindowsBashisms(t *testing.T) {
	issue := detectShellCommandIssue(`cd /d/tmp/zero-pr-158 && ls -la`, "windows")
	if issue == nil {
		t.Fatal("expected Windows bash-style cd command to be flagged")
	}
	for _, want := range []string{"Windows cmd.exe", "cwd", "list_directory"} {
		if !strings.Contains(issue.Message+" "+issue.Suggestion, want) {
			t.Fatalf("expected issue to mention %q, got %#v", want, issue)
		}
	}
}

func TestDetectShellCommandIssueAllowsWindowsCDSwitch(t *testing.T) {
	issue := detectShellCommandIssue(`cd /d D:\tmp\zero-pr-158 && dir`, "windows")
	if issue != nil {
		t.Fatalf("expected valid Windows cd /d switch to pass, got %#v", issue)
	}
}

func TestDetectShellCommandIssueFlagsWindowsBadCommands(t *testing.T) {
	for _, command := range []string{
		`echo false ls -la`,
		`echo list -items`,
		`powershell -NoProfile -Command "Write-Output ls -la"`,
	} {
		if issue := detectShellCommandIssue(command, "windows"); issue != nil {
			t.Fatalf("expected incidental ls text to pass for %q, got %#v", command, issue)
		}
	}

	for _, command := range []string{
		`ls -la`,
		`cd C:\tmp && ls -la`,
		`cd C:\tmp && ls`,
	} {
		if issue := detectShellCommandIssue(command, "windows"); issue == nil {
			t.Fatalf("expected actual ls command to be flagged for %q", command)
		}
	}

	// Common Unix filters used for truncation (e.g. gh output) must be caught.
	for _, command := range []string{
		`gh pr view 465 | head -200`,
		`some-cmd | tail -n 10`,
		`cat file | head -5`,
		`ps aux | head`,
		// Issue #467's exact reported case: `| cat` alone (no other POSIX
		// utility in the pipeline) must also be flagged before execution.
		`gh pr view 465 --json reviews,comments -q '.reviews | length' 2>&1 | cat`,
		`some-cmd | cat`,
	} {
		if issue := detectShellCommandIssue(command, "windows"); issue == nil {
			t.Fatalf("expected POSIX filter command to be flagged for %q", command)
		}
	}
}

func TestDetectShellCommandIssueFlagsPipedPosixUtilities(t *testing.T) {
	for _, command := range []string{
		`git log --oneline -- pinokio_compat.py | head`,
		`git blame -L 40,46 pinokio_compat.py --format=%author | grep summary`,
		`cat file.txt | wc -l`,
		`some_command | tail -n 20`,
	} {
		issue := detectShellCommandIssue(command, "windows")
		if issue == nil {
			t.Fatalf("expected POSIX utility pipeline to be flagged for %q", command)
		}
		if issue.Kind != "windows_msys_sandbox" {
			t.Fatalf("expected windows_msys_sandbox for %q, got %q", command, issue.Kind)
		}
	}
}

func TestDetectShellCommandIssueIgnoresQuotedPipeText(t *testing.T) {
	for _, command := range []string{
		`gh pr comment --body "please do not use | cat here"`,
		`git commit -m "grep for TODO later"`,
		`git commit -m "fix | cat pipe"`,
	} {
		if issue := detectShellCommandIssue(command, "windows"); issue != nil {
			t.Fatalf("expected POSIX-utility text inside a quoted argument to pass for %q, got %#v", command, issue)
		}
	}

	// The same utility names, actually piped (unquoted), must still be flagged.
	for _, command := range []string{
		`echo test | cat`,
		`echo test | grep TODO`,
	} {
		if issue := detectShellCommandIssue(command, "windows"); issue == nil {
			t.Fatalf("expected unquoted POSIX utility pipeline to still be flagged for %q", command)
		}
	}
}

func TestDetectShellCommandIssueIgnoresCaretEscapedMetachars(t *testing.T) {
	for _, command := range []string{
		`foo^|cat`,
		`echo a^&cat`,
		`echo a^;cat`,
	} {
		if issue := detectShellCommandIssue(command, "windows"); issue != nil {
			t.Fatalf("expected caret-escaped metachar to read as a literal for %q, got %#v", command, issue)
		}
	}

	// The same shape, unescaped, must still be flagged.
	if issue := detectShellCommandIssue(`foo|cat`, "windows"); issue == nil {
		t.Fatal("expected unescaped pipe into cat to still be flagged")
	}
}

func TestDetectShellCommandIssueSuggestionOmitsBlockedMore(t *testing.T) {
	issue := detectShellCommandIssue(`some_command | wc -l`, "windows")
	if issue == nil {
		t.Fatal("expected POSIX utility pipeline to be flagged")
	}
	if strings.Contains(issue.Suggestion, "more") {
		t.Fatalf("suggestion should not recommend more, which is itself blocked as an interactive pager on Windows: %q", issue.Suggestion)
	}
}

func TestDetectShellCommandIssueAllowsUnrelatedCommands(t *testing.T) {
	for _, command := range []string{
		`git log --oneline`,
		`dir /b`,
		`findstr /r "summary" file.txt`,
		`echo header text`,
		`tail-log.sh`,
		`grep-cli --version`,
		`mytool.exe --help`,
	} {
		if issue := detectShellCommandIssue(command, "windows"); issue != nil {
			t.Fatalf("expected unrelated command to pass for %q, got %#v", command, issue)
		}
	}
}

// Operator/utility-name text that only appears inside a quoted argument must
// not be mistaken for real shell syntax — e.g. `echo "foo; ls -la"` is a
// harmless echo, not a bash-style `ls` invocation, and quoted `head`/`grep`
// text isn't a piped POSIX utility.
func TestDetectShellCommandIssueIgnoresQuotedContent(t *testing.T) {
	for _, command := range []string{
		`echo "foo; ls -la"`,
		`echo "run head over the file"`,
		`echo 'ls -la is a common bash command'`,
		`echo "grep for the pattern"`,
	} {
		if issue := detectShellCommandIssue(command, "windows"); issue != nil {
			t.Fatalf("expected quoted content to be ignored for %q, got %#v", command, issue)
		}
	}

	// Sanity: the same utility names still get flagged when they're NOT quoted.
	// cmd.exe (unlike bash) does not treat ; as a statement separator, so
	// `echo "foo" ; ls -la` is a single echo invocation with literal
	// arguments, not a real ls invocation, and must not be flagged.
	if issue := detectShellCommandIssue(`git log | head`, "windows"); issue == nil {
		t.Fatalf("expected unquoted shell syntax to still be flagged for %q", `git log | head`)
	}
}

func TestDetectShellOutputIssueAddsWindowsSyntaxHint(t *testing.T) {
	issue := detectShellOutputIssue("The syntax of the command is incorrect.", "windows")
	if issue == nil {
		t.Fatal("expected Windows syntax error to get shell guidance")
	}
	rendered := appendShellIssueHint("stderr:\nThe syntax of the command is incorrect.\nexit_code: 1", *issue)
	for _, want := range []string{"[zero] shell issue:", "Windows cmd.exe", "Suggestion:"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("expected rendered hint to contain %q, got %q", want, rendered)
		}
	}
}

func TestRegistryBlocksBashWithoutGrant(t *testing.T) {
	registry := NewRegistry()
	registry.Register(NewScopedBashTool(t.TempDir(), nil))

	result := registry.Run(context.Background(), "bash", map[string]any{
		"command": helperCommand("success"),
	})

	if result.Status != StatusError {
		t.Fatalf("expected error status, got %s", result.Status)
	}
	if !strings.Contains(result.Output, "Permission required for bash") {
		t.Fatalf("expected permission error, got %q", result.Output)
	}
}

func TestBashToolRunsCommandInWorkspace(t *testing.T) {
	root := t.TempDir()

	result := NewScopedBashTool(root, nil).Run(context.Background(), map[string]any{
		"command": helperCommand("success"),
	})

	if result.Status != StatusOK {
		t.Fatalf("expected ok status, got %s: %s", result.Status, result.Output)
	}
	if !strings.Contains(result.Output, "stdout:\nhello from bash") {
		t.Fatalf("expected stdout in output, got %q", result.Output)
	}
	if result.Meta["exit_code"] != "0" {
		t.Fatalf("expected exit_code metadata 0, got %q", result.Meta["exit_code"])
	}
	if result.Meta["cwd"] != "." {
		t.Fatalf("expected cwd metadata ., got %q", result.Meta["cwd"])
	}
}

func TestBashToolReportsWorkspaceChangesAfterFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell syntax")
	}
	root := t.TempDir()
	result := NewBashTool(root).Run(context.Background(), map[string]any{
		"command": "printf partial > partial.txt; exit 9",
	})
	if result.Status != StatusError {
		t.Fatalf("expected application failure, got %s: %s", result.Status, result.Output)
	}
	if len(result.ChangedFiles) != 1 || result.ChangedFiles[0] != "partial.txt" {
		t.Fatalf("ChangedFiles = %#v, want partial.txt", result.ChangedFiles)
	}
	if result.ExecutionOutcome == nil || len(result.ExecutionOutcome.Changes) != 1 || result.ExecutionOutcome.Changes[0].Kind != execution.ChangeCreated {
		t.Fatalf("typed changes = %#v, want created partial file", result.ExecutionOutcome)
	}
}

// A command with runaway output must not be buffered whole in memory: the capture
// is bounded to head+tail, yet raw_bytes still reports the true (much larger) size.
// If capture were unbounded this would balloon Zero's memory before truncation.
func TestBashToolBoundsRunawayOutputCapture(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell pipeline (yes | head)")
	}
	const produced = 500000 // ~5× the 96 KiB budget

	result := NewScopedBashTool(t.TempDir(), nil).Run(context.Background(), map[string]any{
		"command": fmt.Sprintf("yes ABCDEFGH | head -c %d", produced),
	})

	if result.Status != StatusOK {
		t.Fatalf("expected ok status, got %s: %s", result.Status, result.Output)
	}
	if !result.Truncated || result.Meta["truncated"] != "true" {
		t.Fatalf("runaway output should be flagged truncated, meta=%v", result.Meta)
	}
	if !strings.Contains(result.Output, "output truncated") {
		t.Fatalf("expected a truncation marker in output")
	}
	// The emitted (model-visible) text is capped near the budget...
	if len(result.Output) > bashOutputBudgetBytes+1024 {
		t.Fatalf("emitted %d bytes far exceeds budget %d", len(result.Output), bashOutputBudgetBytes)
	}
	// ...but raw_bytes reflects the full stream, proving the total was counted while
	// only a bounded slice was ever held (raw >> the ~2×budget retention cap).
	raw, err := strconv.Atoi(result.Meta["raw_bytes"])
	if err != nil {
		t.Fatalf("raw_bytes not an int: %q", result.Meta["raw_bytes"])
	}
	if raw < produced {
		t.Fatalf("raw_bytes = %d, want >= %d (full stream counted)", raw, produced)
	}
}

func TestBashToolUsesRequestedCwd(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	result := NewScopedBashTool(root, nil).Run(context.Background(), map[string]any{
		"command": helperCommand("pwd"),
		"cwd":     "nested",
	})

	if result.Status != StatusOK {
		t.Fatalf("expected ok status, got %s: %s", result.Status, result.Output)
	}
	normalizedOutput := filepath.ToSlash(strings.TrimSpace(result.Output))
	if !strings.Contains(normalizedOutput, "stdout:\n") || !strings.HasSuffix(normalizedOutput, "/nested") {
		t.Fatalf("expected command to run in nested cwd, got %q", result.Output)
	}
	if result.Meta["cwd"] != "nested" {
		t.Fatalf("expected cwd metadata nested, got %q", result.Meta["cwd"])
	}
}

func TestBashToolRejectsCwdOutsideWorkspace(t *testing.T) {
	outside := t.TempDir()

	result := NewScopedBashTool(t.TempDir(), nil).Run(context.Background(), map[string]any{
		"command": helperCommand("success"),
		"cwd":     outside,
	})

	if result.Status != StatusError {
		t.Fatalf("expected error status, got %s", result.Status)
	}
	if !strings.Contains(result.Output, "must stay inside the workspace") {
		t.Fatalf("expected workspace error, got %q", result.Output)
	}
}

func TestBashToolReturnsNonzeroExitAsError(t *testing.T) {
	result := NewScopedBashTool(t.TempDir(), nil).Run(context.Background(), map[string]any{
		"command": helperCommand("fail"),
	})

	if result.Status != StatusError {
		t.Fatalf("expected error status, got %s", result.Status)
	}
	wantExitCode := strconv.Itoa(helperFailureExitCode())
	for _, want := range []string{"stdout:\nbefore failure", "stderr:\nfailure details", "exit_code: " + wantExitCode} {
		if !strings.Contains(result.Output, want) {
			t.Fatalf("expected output to contain %q, got %q", want, result.Output)
		}
	}
	if result.Meta["exit_code"] != wantExitCode {
		t.Fatalf("expected exit_code metadata %s, got %q", wantExitCode, result.Meta["exit_code"])
	}
	if result.ExecutionOutcome == nil || result.ExecutionOutcome.State != execution.StateFailed || result.ExecutionOutcome.Kind != execution.OutcomeApplicationFailure {
		t.Fatalf("execution outcome = %#v, want failed/application_failure", result.ExecutionOutcome)
	}
}

func TestBashToolTimesOut(t *testing.T) {
	result := NewScopedBashTool(t.TempDir(), nil).Run(context.Background(), map[string]any{
		"command":    helperCommand("sleep"),
		"timeout_ms": 20,
	})

	if result.Status != StatusError {
		t.Fatalf("expected error status, got %s", result.Status)
	}
	if !strings.Contains(result.Output, "timed out after 20ms") {
		t.Fatalf("expected timeout error, got %q", result.Output)
	}
	if result.Meta["timeout_ms"] != "20" {
		t.Fatalf("expected timeout_ms metadata 20, got %q", result.Meta["timeout_ms"])
	}
	if result.ExecutionOutcome == nil || result.ExecutionOutcome.Kind != execution.OutcomeTimedOut {
		t.Fatalf("execution outcome = %#v, want timed_out", result.ExecutionOutcome)
	}
}

func TestBashToolTimeoutKillsBackgroundChildren(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group kill is POSIX-only")
	}
	// Shorten the post-kill I/O drain so this regression doesn't bake several
	// seconds into every run; the child sleep below is correspondingly short.
	defer func(prev time.Duration) { bashWaitDelay = prev }(bashWaitDelay)
	bashWaitDelay = 200 * time.Millisecond

	root := t.TempDir()
	sentinel := filepath.Join(root, "leaked")
	// A backgrounded child sleeps past the timeout, then drops a sentinel. `wait`
	// keeps the foreground shell alive so the deadline fires while the child is
	// still running. Without process-group kill the child is orphaned: it
	// survives, eventually writes the sentinel, and (because it inherited the
	// stdout pipe) blocks Run() until it exits.
	const childSleep = time.Second
	command := fmt.Sprintf("(sleep 1; touch %s) & wait", shellQuote(sentinel))

	start := time.Now()
	result := NewScopedBashTool(root, nil).Run(context.Background(), map[string]any{
		"command":    command,
		"timeout_ms": 300,
	})
	elapsed := time.Since(start)

	if result.Status != StatusError {
		t.Fatalf("expected timeout error status, got %s: %q", result.Status, result.Output)
	}
	if elapsed > time.Second {
		t.Fatalf("Run blocked %s past the 300ms timeout; background child held the pipes", elapsed)
	}

	// Give the child more than its sleep to fire if it survived the timeout.
	time.Sleep(childSleep + 500*time.Millisecond)
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatalf("background child survived the timeout and wrote %s", sentinel)
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected error stat-ing sentinel: %v", err)
	}
}

func TestRegistryRunsWithDegradedUnavailableNativeSandbox(t *testing.T) {
	root := t.TempDir()
	registry := NewRegistry()
	registry.Register(NewScopedBashTool(root, nil))
	engine := sandbox.NewEngine(sandbox.EngineOptions{
		WorkspaceRoot: root,
		Policy:        sandbox.DefaultPolicy(),
		Backend:       sandbox.Backend{Name: sandbox.BackendUnavailable, Message: "native sandbox unavailable"},
	})

	result := registry.RunWithOptions(context.Background(), "bash", map[string]any{
		"command": helperCommand("success"),
	}, RunOptions{
		PermissionGranted: true,
		Sandbox:           engine,
		PermissionMode:    string(sandbox.PermissionUnsafe),
		Autonomy:          "medium",
	})

	if result.Status != StatusOK || !strings.Contains(result.Output, "hello from bash") {
		t.Fatalf("expected degraded command to run, got %s: %s", result.Status, result.Output)
	}
	if result.Meta["sandbox_wrapped"] != "false" || result.Meta["sandbox_enforcement_level"] != string(sandbox.EnforcementDegraded) {
		t.Fatalf("sandbox metadata = %#v, want degraded direct plan", result.Meta)
	}
}

// TestBashToolRequireEscalatedMsysGuard covers the boundary of the fix for an
// escalation-vs-preflight ordering bug: the MSYS sandbox guard exists only
// because MSYS/Cygwin coreutils fail under the write-restricted sandbox, so
// once sandbox_permissions: require_escalated is actually approved (running
// the command unsandboxed), the guard must get out of the way, but an
// unrelated windows_shell_syntax issue (a real cmd.exe syntax problem, not a
// sandbox interaction) must still block regardless of escalation.
func TestBashToolRequireEscalatedMsysGuard(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only MSYS sandbox guard")
	}
	root := t.TempDir()
	newEngine := func() *sandbox.Engine {
		return sandbox.NewEngine(sandbox.EngineOptions{
			WorkspaceRoot: root,
			Policy:        sandbox.DefaultPolicy(),
			Backend:       sandbox.Backend{Name: sandbox.BackendUnavailable, Message: "native sandbox unavailable"},
		})
	}
	registry := NewRegistry()
	registry.Register(NewScopedBashTool(root, nil))
	msysCommand := "cat somefile.txt"
	if detectShellRuntime(runtime.GOOS).Kind == shellKindPowerShell {
		// cat is a native Get-Content alias in PowerShell. Use a name that
		// still resolves to an incompatible Git-for-Windows MSYS executable.
		msysCommand = "grep pattern somefile.txt"
	}

	t.Run("default sandboxing still blocks an MSYS-prone command", func(t *testing.T) {
		result := registry.RunWithOptions(context.Background(), "bash", map[string]any{
			"command": msysCommand,
		}, RunOptions{
			PermissionGranted: true,
			Sandbox:           newEngine(),
			PermissionMode:    string(sandbox.PermissionModeAsk),
		})
		if result.Meta["shell_issue"] != "windows_msys_sandbox" {
			t.Fatalf("expected windows_msys_sandbox block without escalation, got %#v", result)
		}
	})

	t.Run("approved require_escalated bypasses the MSYS guard", func(t *testing.T) {
		result := registry.RunWithOptions(context.Background(), "bash", map[string]any{
			"command":             msysCommand,
			"sandbox_permissions": string(SandboxPermissionsRequireEscalated),
		}, RunOptions{
			PermissionGranted: true,
			Sandbox:           newEngine(),
			PermissionMode:    string(sandbox.PermissionModeAsk),
		})
		// Assert on the preflight block sentinel (exit_code "-1", set only by
		// shellIssueBlockResult) rather than shell_issue: once the guard is
		// bypassed, "cat somefile.txt" actually runs, and its real,
		// PATH-dependent output could otherwise trip the unrelated
		// post-execution detectShellOutputIssue heuristic and make this
		// assertion flaky for reasons unrelated to the guard under test.
		if result.Meta["exit_code"] == "-1" {
			t.Fatalf("expected approved require_escalated to bypass the MSYS guard, got blocked: %#v", result)
		}
	})

	t.Run("approved require_escalated still blocks an unrelated syntax issue", func(t *testing.T) {
		result := registry.RunWithOptions(context.Background(), "bash", map[string]any{
			// No `&&` here on purpose. The POSIX-style path is what this case is
			// about, and on Windows PowerShell 5.1 a chained command trips the
			// separate windows_powershell_version preflight first, which shadows
			// the block being asserted. That only shows up on 5.1: the runners
			// have PowerShell 7, where && is valid and the version check does not
			// fire, so the substitution kept this green in CI while failing on the
			// shell a stock Windows box actually gets.
			"command":             `cd /d/tmp/zero-pr-158`,
			"sandbox_permissions": string(SandboxPermissionsRequireEscalated),
		}, RunOptions{
			PermissionGranted: true,
			Sandbox:           newEngine(),
			PermissionMode:    string(sandbox.PermissionModeAsk),
		})
		if result.Meta["shell_issue"] != "windows_shell_syntax" {
			t.Fatalf("expected windows_shell_syntax block to still apply under require_escalated, got %#v", result)
		}
	})
}

// TestBashToolIgnoresMsysMarkersInCommandArgumentsAfterFailure guards the
// post-execution MSYS heuristic against treating the command line itself as
// evidence. The helper command below fails for a reason unrelated to MSYS
// (a plain non-zero exit), but its own argument text quotes an MSYS crash
// marker the way a PR-comment or commit-message argument might. Before the
// fix, detectShellOutputIssue scanned command+output together and would have
// misdiagnosed this as an MSYS sandbox failure even though the real
// stdout/stderr never mentions MSYS at all.
func TestBashToolIgnoresMsysMarkersInCommandArgumentsAfterFailure(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only MSYS output guard")
	}
	root := t.TempDir()
	// The helper only reads argv[2] ("fail"); this trailing quoted argument is
	// otherwise unused by the helper but still part of the command line text.
	command := helperCommand("fail") + ` "fatal error - CreateFileMapping S-1-5-21, Win32 error 5. Terminating. cygheap_user::init"`

	result := NewScopedBashTool(root, nil).Run(context.Background(), map[string]any{
		"command": command,
	})

	wantExitCode := strconv.Itoa(helperFailureExitCode())
	if result.Status != StatusError || result.Meta["exit_code"] != wantExitCode {
		t.Fatalf("expected the helper failure exit %s, got %s: %#v", wantExitCode, result.Status, result)
	}
	if result.Meta["shell_issue"] == "windows_msys_sandbox" {
		t.Fatalf("expected the MSYS marker in the command's own argument text to be ignored, got %#v", result)
	}
}

func TestBashToolRunsWithDegradedUnavailableNativeSandbox(t *testing.T) {
	root := t.TempDir()
	policy := sandbox.DefaultPolicy()
	engine := sandbox.NewEngine(sandbox.EngineOptions{
		WorkspaceRoot: root,
		Policy:        policy,
		Backend:       sandbox.Backend{Name: sandbox.BackendUnavailable, Message: "native sandbox unavailable"},
	})

	result := NewScopedBashTool(root, nil).(interface {
		RunWithSandbox(context.Context, map[string]any, *sandbox.Engine) Result
	}).RunWithSandbox(context.Background(), map[string]any{
		"command": helperCommand("success"),
	}, engine)

	if result.Status != StatusOK || !strings.Contains(result.Output, "hello from bash") {
		t.Fatalf("expected degraded command to run, got %s: %q", result.Status, result.Output)
	}
	if result.Meta["sandbox_wrapped"] != "false" || result.Meta["sandbox_enforcement_level"] != string(sandbox.EnforcementDegraded) {
		t.Fatalf("sandbox metadata = %#v, want degraded direct plan", result.Meta)
	}
}

func TestBashToolBuildsWrappedSandboxExecCommand(t *testing.T) {
	root := t.TempDir()
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", root, err)
	}
	engine := sandbox.NewEngine(sandbox.EngineOptions{
		WorkspaceRoot: root,
		Policy:        sandbox.DefaultPolicy(),
		Backend: sandbox.Backend{
			Name:       sandbox.BackendMacOSSeatbelt,
			Available:  true,
			Executable: "/usr/bin/sandbox-exec",
		},
	})

	command, plan, err := buildBashCommand(context.Background(), "pwd", root, engine)
	if err != nil {
		t.Fatalf("buildBashCommand: %v", err)
	}
	if command.Path != "/usr/bin/sandbox-exec" || !plan.Wrapped {
		t.Fatalf("command path = %q plan = %#v, want wrapped sandbox-exec", command.Path, plan)
	}
	if len(command.Args) < 5 || command.Args[1] != "-p" || !strings.Contains(command.Args[2], "(deny network*)") {
		t.Fatalf("sandbox-exec args = %#v, want inline profile", command.Args)
	}
	if command.Dir != resolvedRoot || plan.SandboxDir != resolvedRoot {
		t.Fatalf("dirs = command %q plan %q, want root", command.Dir, plan.SandboxDir)
	}
}

func TestBashToolRunsWithHostSandboxBackendWhenAvailable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows sandbox adapter is owned by the Windows integration slice")
	}
	backend := sandbox.SelectBackend(sandbox.BackendOptions{})
	if !backend.Available || backend.Name == sandbox.BackendUnavailable {
		t.Skipf("host sandbox backend unavailable: %s", backend.Message)
	}
	root := t.TempDir()
	engine := sandbox.NewEngine(sandbox.EngineOptions{
		WorkspaceRoot: root,
		Policy:        sandbox.DefaultPolicy(),
		Backend:       backend,
	})

	result := NewScopedBashTool(root, nil).(interface {
		RunWithSandbox(context.Context, map[string]any, *sandbox.Engine) Result
	}).RunWithSandbox(context.Background(), map[string]any{
		"command": "printf sandbox-ok",
	}, engine)

	if result.Status != StatusOK {
		t.Fatalf("expected host sandbox command to run, got %s: %s; meta=%#v", result.Status, result.Output, result.Meta)
	}
	if !strings.Contains(result.Output, "sandbox-ok") {
		t.Fatalf("expected sandbox command output, got %q", result.Output)
	}
	if result.Meta["sandbox_backend"] != string(backend.Name) || result.Meta["sandbox_wrapped"] != "true" {
		t.Fatalf("sandbox metadata = %#v, want wrapped host backend %s", result.Meta, backend.Name)
	}
}

func TestBashToolBlocksInteractiveCommandBeforeExecution(t *testing.T) {
	root := t.TempDir()

	result := NewScopedBashTool(root, nil).Run(context.Background(), map[string]any{
		"command": "vim main.go",
	})

	if result.Status != StatusError {
		t.Fatalf("expected error status for interactive command, got %s: %s", result.Status, result.Output)
	}
	if !strings.Contains(result.Output, "interactive") {
		t.Fatalf("expected interactive guard message, got %q", result.Output)
	}
	if !strings.Contains(result.Output, "edit_file") && !strings.Contains(result.Output, "sed") {
		t.Fatalf("expected actionable suggestion, got %q", result.Output)
	}
	// The command must NOT have run: exit_code metadata should mark a pre-exec block.
	if result.Meta["exit_code"] != "-1" {
		t.Fatalf("expected exit_code -1 (not executed), got %q", result.Meta["exit_code"])
	}
	if result.Meta["safety_block"] != "interactive_command" {
		t.Fatalf("expected safety_block metadata, got %#v", result.Meta)
	}
	if result.Display.Kind != "shell" || !strings.Contains(result.Display.Summary, "Blocked") {
		t.Fatalf("expected blocked display annotation, got %#v", result.Display)
	}
}

func TestBashToolBlocksInteractiveCommandThroughSandbox(t *testing.T) {
	root := t.TempDir()
	engine := sandbox.NewEngine(sandbox.EngineOptions{
		WorkspaceRoot: root,
		Policy:        sandbox.DefaultPolicy(),
		Backend:       sandbox.Backend{Name: sandbox.BackendUnavailable, Message: "native sandbox unavailable"},
	})

	result := NewScopedBashTool(root, nil).(interface {
		RunWithSandbox(context.Context, map[string]any, *sandbox.Engine) Result
	}).RunWithSandbox(context.Background(), map[string]any{
		"command": "less /etc/hosts",
	}, engine)

	if result.Status != StatusError {
		t.Fatalf("expected error status, got %s: %s", result.Status, result.Output)
	}
	// The suggested non-interactive alternative is platform-specific: cat
	// doesn't exist on Windows cmd.exe, so the guard suggests `type` there
	// instead (see safe_command.go's windowsSuggestion).
	wantSuggestion := "cat"
	if runtime.GOOS == "windows" {
		wantSuggestion = "type"
	}
	if !strings.Contains(result.Output, "interactive") || !strings.Contains(result.Output, wantSuggestion) {
		t.Fatalf("expected pager guard message with %q suggestion, got %q", wantSuggestion, result.Output)
	}
}

func TestBashToolAllowsNonInteractiveCommand(t *testing.T) {
	root := t.TempDir()

	result := NewScopedBashTool(root, nil).Run(context.Background(), map[string]any{
		"command": helperCommand("success"),
	})

	if result.Status != StatusOK {
		t.Fatalf("expected ok status for non-interactive command, got %s: %s", result.Status, result.Output)
	}
	if result.Meta["safety_block"] != "" {
		t.Fatalf("did not expect a safety block, got %#v", result.Meta)
	}
}

// TestBashToolPreservesEmbeddedQuotesOnWindows pins a real, previously-broken
// case: passing commandText as a normal exec.Cmd argument makes Go wrap it in
// an outer pair of quotes (it contains spaces) with its own quotes escaped as
// \", and cmd.exe's /C remainder parsing strips the first and last literal
// quote character in that remainder without undoing the backslash-escaping -
// corrupting a command whose own text contains embedded double quotes,
// exactly the shape of `python -c "print(15 / 3)"`, `git commit -m
// "message"`, `node -e "..."`. Before the fix, the helper below received a
// truncated, mis-quoted argument (starting with a stray literal `"`) instead
// of the text between the quotes.
func TestBashToolPreservesEmbeddedQuotesOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("cmd.exe /S/C quote-remainder parsing is Windows-specific")
	}
	root := t.TempDir()
	executable := shellQuote(os.Args[0])
	// Space and a slash: the two characters whose position in the original
	// bug report's SyntaxError ("print(15, truncated right before the /")
	// showed exactly where the corruption happened.
	commandText := executable + ` --zero-bash-helper echo-arg "hello / world"`

	result := NewScopedBashTool(root, nil).Run(context.Background(), map[string]any{
		"command": commandText,
	})

	if result.Status != StatusOK {
		t.Fatalf("expected ok status, got %s: %s", result.Status, result.Output)
	}
	if !strings.Contains(result.Output, "hello / world") {
		t.Fatalf("expected the embedded-quote argument to survive intact, got %q", result.Output)
	}
}

// TestBashToolRunsCommandLineForLoopSyntax pins cmd.exe command-line syntax,
// not batch-file syntax: a `for %i in (...) do ...` loop with a single
// percent sign is valid typed directly at a cmd.exe prompt (or via
// `cmd /C "..."`), but requires %%i inside an actual .bat/.cmd FILE, because
// batch files perform an extra pass of percent-substitution before FOR ever
// runs, consuming the single percent so the loop variable never binds. An
// earlier version of this fix ran commandText from a temporary .cmd file
// instead of passing it straight through to cmd.exe's /C remainder, which
// silently broke single-percent syntax like this.
func TestBashToolRunsCommandLineForLoopSyntax(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("cmd.exe command-line vs batch-file parsing is Windows-specific")
	}
	if detectShellRuntime(runtime.GOOS).Kind != shellKindCmd {
		t.Skip("cmd.exe fallback is not selected")
	}
	root := t.TempDir()

	result := NewScopedBashTool(root, nil).Run(context.Background(), map[string]any{
		"command": "for %i in (1 2 3) do echo %i",
	})

	if result.Status != StatusOK {
		t.Fatalf("expected ok status, got %s: %s", result.Status, result.Output)
	}
	for _, want := range []string{"1", "2", "3"} {
		if !strings.Contains(result.Output, want) {
			t.Fatalf("expected the for-loop to expand %%i to %q, got %q", want, result.Output)
		}
	}
}

func helperCommand(name string) string {
	executable := shellQuote(os.Args[0])
	return executable + " --zero-bash-helper " + name
}

func helperFailureExitCode() int {
	return 7
}

func shellQuote(value string) string {
	if runtime.GOOS == "windows" {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func TestAppendSandboxBlocks(t *testing.T) {
	if got := appendSandboxBlocks("err output", nil); got != "err output" {
		t.Fatalf("no blocks must leave stderr unchanged, got %q", got)
	}
	got := appendSandboxBlocks("err output", []string{"deny file-write-create /etc/x"})
	for _, want := range []string{"err output", "<sandbox_blocks>", "deny file-write-create /etc/x", "</sandbox_blocks>"} {
		if !strings.Contains(got, want) {
			t.Fatalf("annotated stderr missing %q:\n%s", want, got)
		}
	}
	if block := appendSandboxBlocks("", []string{"deny x"}); strings.HasPrefix(block, "\n") {
		t.Fatalf("empty stderr must not yield a leading newline: %q", block)
	}
}
