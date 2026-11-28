package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/execution"
	zeroSandbox "github.com/Twigpine/zero/internal/sandbox"
)

// runSandboxExec runs ONE command through the real sandbox and exits with its
// status.
//
// This exists because until now the sandbox could only be exercised through a
// full agent turn with a model in the loop. `zero sandbox policy` reports what
// the posture would be and `zero sandbox check` evaluates a hypothetical
// decision, but nothing actually ran a command and let you look at what
// happened on disk afterwards. The practical result is that enforcement is
// covered almost entirely by tests asserting the shape of an ACL plan, and
// almost not at all by tests asserting a write was refused.
//
// A plan can be perfectly correct and never reach the filesystem. That is not
// hypothetical here: the .git rename guard was emitted correctly by the planner
// and silently skipped by the applier, and four tests covering the plan all
// passed while the ACE was absent from disk. Something that runs the real
// binary and then stats the file is the only thing that catches that class.
//
// Deliberately NOT a debug curiosity: it takes the same path a shell tool
// takes, the engine's decision first and then the engine's command plan, so
// what it proves is what users get. It prints the decision, then the resolved
// backend and enforcement level, to stderr before running, so a harness can
// assert the sandbox was actually engaged rather than quietly downgraded.
func runSandboxExec(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	command, err := parseSandboxExecArgs(args)
	if err != nil {
		if errors.Is(err, errSandboxExecHelp) {
			if writeErr := writeSandboxExecHelp(stdout); writeErr != nil {
				return exitCrash
			}
			return exitSuccess
		}
		return writeExecUsageError(stderr, err.Error())
	}

	workspaceRoot, err := resolveWorkspaceRoot("", deps)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	resolved, err := deps.resolveConfig(workspaceRoot, config.Overrides{})
	if err != nil {
		return writeAppError(stderr, err.Error(), exitProvider)
	}
	policy := applyConfiguredSandboxPolicy(zeroSandbox.DefaultPolicy(), resolved.Sandbox)

	scope, err := zeroSandbox.NewScope(workspaceRoot, resolved.Sandbox.AdditionalWriteRoots)
	if err != nil {
		return writeAppError(stderr, fmt.Sprintf("resolve sandbox write roots: %v", err), exitCrash)
	}

	// Through the ENGINE, not a SandboxManager built here.
	//
	// The engine is what a real tool call goes through, and it does more than
	// hand the manager a request: it resolves the permission profile, calls
	// prepareSandboxRuntime, and folds the runtime state into that profile
	// before planning. Building a manager directly skipped all of it, so the
	// command ran without the runtime write root, and cache or temp writes could
	// pass or fail differently from the sandboxed command this exists to imitate.
	// A harness that exercises the wrong path is worse than no harness, because
	// its result still reads as evidence.
	engine := zeroSandbox.NewEngine(zeroSandbox.EngineOptions{
		WorkspaceRoot: workspaceRoot,
		Policy:        policy,
		Scope:         scope,
		Backend:       deps.selectSandboxBackend(zeroSandbox.BackendOptions{}),
		// The same scrub list a real tool call gets. Without it the engine keeps an
		// empty set, and only the hardcoded names plus the provider catalog's own
		// AuthEnvVars are removed from the child's environment: a key named by
		// `apiKeyEnv` in the user's config would be scrubbed for every sandboxed
		// tool call and handed to this one. Reproducing the production environment
		// is the entire point of the command, and a credential is the last part of
		// it that may differ.
		SensitiveEnvKeys: providerSensitiveEnvKeys(resolved),
	})

	// The CLI's shared shutdown context, so Ctrl+C and a directed SIGTERM both
	// arrive here rather than only at whatever the terminal happens to signal.
	runCtx, stopSignals := signalContext()
	defer stopSignals()

	// THE DECISION COMES BEFORE THE PLAN, as it does for every real tool call.
	//
	// A shell tool is evaluated by the engine and only then planned
	// (tools.Registry.RunWithOptions). This went straight to BuildCommandPlan, so
	// it took "the same path a shell tool takes" only from the second step on, and
	// every refusal that lives in Evaluate did not exist here. The one that
	// matters most is the nested-repository guard: a workspace governed by an
	// ancestor repository carries NO git carveouts, on the strength of that guard
	// refusing to create a repository there, and `zero sandbox exec -- git init`
	// created one under the plain workspace grant with a writable config and
	// hooks. A harness that runs what production would have refused does not
	// prove what users get. Reported by @jatmn.
	//
	// PermissionGranted is true because the person at the terminal typed the
	// command, which is the approval a prompt asks a session user for. It lifts
	// exactly what an approval lifts in a session and nothing else: a denial is
	// still a denial, and an approved network command still runs inside a sandbox
	// that has the network off.
	decision := sandboxExecDecision(runCtx, engine, workspaceRoot, command)
	fmt.Fprintf(stderr, "sandbox: decision=%s reason=%s\n", decision.Action, decision.Reason)
	if decision.Action == zeroSandbox.ActionDeny {
		return writeAppError(stderr, decision.ErrorString(), exitCrash)
	}

	plan, err := engine.BuildCommandPlan(zeroSandbox.CommandSpec{
		Name: command[0],
		Args: command[1:],
		Dir:  workspaceRoot,
		Env:  os.Environ(),
	})
	if err != nil {
		return writeAppError(stderr, fmt.Sprintf("build sandbox command plan: %v", err), exitCrash)
	}
	// The plan owns resources beyond its construction: on Linux it allocates a
	// policy-report file and registers the removal here, so without this every
	// invocation leaves a /tmp/zero-sandbox-report-* behind.
	defer plan.Cleanup()

	// Printed before the command runs and on stderr, so it survives a command
	// that writes to stdout and stays greppable by a test harness. A downgrade
	// is reported loudly for the same reason: a smoke test that passes because
	// the sandbox quietly stood down is worse than no smoke test.
	fmt.Fprintf(stderr, "sandbox: backend=%s enforcement=%s wrapped=%t workspace=%s\n",
		plan.Backend.Name, plan.EnforcementLevel, plan.Wrapped, plan.WorkspaceRoot)
	if strings.TrimSpace(plan.DowngradeReason) != "" {
		fmt.Fprintf(stderr, "sandbox: DOWNGRADED: %s\n", plan.DowngradeReason)
	}

	return runSandboxPlannedCommand(runCtx, plan, stdout, stderr)
}

// sandboxExecToolName is the tool a session would have run this command with.
// Grants and the engine's shell rules are keyed by it.
const sandboxExecToolName = "bash"

// sandboxExecDecision asks the engine the question a session asks before it
// plans a shell command, for the argv this invocation was given.
func sandboxExecDecision(ctx context.Context, engine *zeroSandbox.Engine, workspaceRoot string, argv []string) zeroSandbox.Decision {
	return engine.Evaluate(ctx, zeroSandbox.Request{
		WorkspaceRoot:     workspaceRoot,
		ToolName:          sandboxExecToolName,
		SideEffect:        zeroSandbox.SideEffectShell,
		PermissionGranted: true,
		PermissionMode:    zeroSandbox.PermissionModeAsk,
		Args:              map[string]any{"command": sandboxExecCommandText(argv)},
		Reason:            "zero sandbox exec",
	})
}

// sandboxExecCommandText renders argv as the command text the engine analyses.
//
// The engine classifies a shell tool by parsing its command string, so argv has
// to become one without changing what it says. A word made only of characters
// the shell does not interpret is left bare, which keeps `git init` reading as a
// session would have written it. Anything else is single-quoted, so an argument
// such as "git init" stays ONE word and is not mistaken for a command, and a
// payload handed to `sh -c` reaches the analyser as the payload it is.
func sandboxExecCommandText(argv []string) string {
	words := make([]string, 0, len(argv))
	for _, word := range argv {
		words = append(words, sandboxExecQuoteWord(word))
	}
	return strings.Join(words, " ")
}

func sandboxExecQuoteWord(word string) string {
	if word == "" {
		return "''"
	}
	bare := true
	for _, r := range word {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("_@%+=:,./-", r):
		default:
			bare = false
		}
	}
	if bare {
		return word
	}
	// A single quote cannot appear inside single quotes, so it closes them, is
	// written escaped, and opens them again.
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

const (
	// sandboxExecShutdownGrace is how long a cancelled child has to exit on its
	// own after the graceful request, before it is killed outright.
	sandboxExecShutdownGrace = 5 * time.Second
	// sandboxExecShutdownPoll is how often that wait re-checks, matching the
	// background package's termination policy.
	sandboxExecShutdownPoll = 50 * time.Millisecond
)

func runSandboxPlannedCommand(ctx context.Context, plan zeroSandbox.CommandPlan, stdout io.Writer, stderr io.Writer) int {
	// CANCELLING THE WRAPPER HAS TO REACH THE COMMAND.
	//
	// This started the backend wrapper with a bare exec.Command().Run(): no
	// context, no forwarding, no shutdown path. A terminal masks it, because
	// terminals signal the whole foreground process group, but a supervisor or
	// task runner that sends SIGTERM to the zero sandbox exec PID killed only
	// Zero. The wrapper and everything under it kept running, doing filesystem
	// and network work after the caller considered the task cancelled, and the
	// deferred plan cleanup never ran because the process was gone.
	//
	// DELIBERATELY NOT execution.ConfigureProcessGroup. Putting the child in its
	// own process group closes this hole and opens a worse one: it severs every
	// kernel-delivered group signal, so a supervisor escalating to a group kill,
	// and a terminal delivering Ctrl+C to its foreground group, would stop
	// reaching the child. That trades a path that works today for one that does
	// not. Keeping the child in Zero's group leaves group delivery unchanged,
	// while the directed-signal case, which reached nothing at all before, now
	// reaches the child.
	process := exec.CommandContext(ctx, plan.Name, plan.Args...)
	// TWO PHASES, AND THE FIRST ONE HAS TO EXIST.
	//
	// The comments here used to describe a graceful signal followed by escalation
	// while the code did neither: Cancel called KillProcessTree, which is SIGKILL
	// on Unix and taskkill /T /F on Windows, and WaitDelay only starts counting
	// AFTER Cancel returns, so it could never postpone a kill that had already
	// happened. A directed SIGTERM reached the child as 137 rather than 143, with
	// no chance to flush output, drop a lock file, or run a shutdown handler.
	//
	// TerminateProcessTree is the two-phase policy the background package already
	// uses: SIGTERM, poll for the grace, then SIGKILL what is still alive. On
	// Windows it is the single-phase kill, which is correct rather than lazy,
	// because there is no signal to send a child that owns no console.
	process.Cancel = func() error {
		return execution.TerminateProcessTree(process.Process.Pid, sandboxExecShutdownGrace, sandboxExecShutdownPoll)
	}
	// A backstop only. Cancel above already waits out the grace and escalates, so
	// this bounds the case where the tree is gone but Wait is still blocked on an
	// inherited pipe a grandchild holds open.
	process.WaitDelay = sandboxExecShutdownGrace + sandboxExecShutdownPoll
	process.Dir = plan.Dir
	if process.Dir == "" {
		process.Dir = plan.WorkspaceRoot
	}
	// SPECIFIED-EMPTY IS NOT UNSPECIFIED.
	//
	// exec.Cmd treats a nil Env as "inherit this process's entire environment",
	// which is a different statement from "run with no variables". The plan owns
	// its environment: directCommandEnv and scrubSensitiveEnv return a slice they
	// built, and that slice is non-nil with length zero when every entry was
	// sensitive. Testing length collapsed those two states and turned the strictest
	// possible answer into the loosest one.
	//
	// Not reachable through `zero sandbox exec` today, because the child
	// environment is os.Environ() and an environment holding only sensitive keys
	// has no %AppData%, so config resolution fails before the planner runs. Fixed
	// anyway: the guard is one assignment, and the next caller that hands the plan
	// a deliberately narrow environment would inherit everything instead, silently.
	if plan.Env != nil {
		process.Env = plan.Env
	}
	process.Stdin = os.Stdin
	process.Stdout = stdout
	process.Stderr = stderr

	if err := process.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// The command's own status, not ours. A harness asserting "the write
			// was refused" needs the refusal's exit code, not a wrapper's.
			// A SIGNALED CHILD HAS NO EXIT CODE TO REPORT. ExitCode() answers -1
			// there, and os.Exit truncates that to 255, so a child that took SIGTERM
			// became indistinguishable from one that chose to exit 255. Fold the
			// signal into the conventional 128+n a shell would report, which needs
			// the ProcessState rather than the integer.
			if status, signaled := signaledExitStatus(exitErr.ProcessState); signaled {
				return status
			}
			return exitErr.ExitCode()
		}
		fmt.Fprintf(stderr, "sandbox exec: %v\n", err)
		return exitCrash
	}
	return exitSuccess
}

var errSandboxExecHelp = errors.New("help requested")

// parseSandboxExecArgs takes everything after `--` as the command, so the
// command's own flags are never mistaken for ours.
func parseSandboxExecArgs(args []string) ([]string, error) {
	if len(args) == 0 {
		return nil, errors.New("usage: zero sandbox exec -- <command> [args...]")
	}
	// Only the FIRST token is ours to interpret. Everything from the second
	// onwards belongs to the child, help flags included.
	//
	// Written as a straight-line decision on args[0] rather than a loop, because
	// a loop here is a lie. An earlier version scanned for `--` "up front" and
	// said so in its comment, but every branch of its body returned, so it only
	// ever examined index 0 and the promised scan did not exist. The rule below
	// is what that code actually implemented, and it is the rule we want: it is
	// the wrapper's own prefix that can ask for the wrapper's help, and the
	// prefix is at most one token long.
	switch args[0] {
	case "--":
		// Everything after the separator is the command, verbatim, including
		// `--help`. That is the documented contract: `zero sandbox exec -- cmd
		// --help` must run cmd's help, not ours.
		command := args[1:]
		if len(command) == 0 {
			return nil, errors.New("usage: zero sandbox exec -- <command> [args...]")
		}
		return command, nil
	case "-h", "--help", "help":
		return nil, errSandboxExecHelp
	}
	// The separator-less form, tolerated for interactive use: the first token is
	// the command, so nothing after it is ours to read. Without this,
	// `zero sandbox exec mycmd --help` printed OUR help and never ran mycmd,
	// which is the same contract break as reading past `--`. The help text still
	// shows the separator, because anything with a leading dash needs it.
	return args, nil
}

func writeSandboxExecHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero sandbox exec -- <command> [args...]

Runs one command through the real sandbox and exits with its status.

Everything after the -- separator is the command, so its own flags are not
parsed as Zero's. The command is evaluated the way a session evaluates a shell
tool call before anything is planned: the decision is written to stderr, and a
command a session refuses is refused here too, with the same reason. Running it
from a terminal counts as the approval a session would prompt for, and lifts
only what that approval lifts. The resolved backend and enforcement level are
then written to stderr before the command runs, and a downgrade is reported
there explicitly.

Examples:
  zero sandbox exec -- cmd /c echo hello
  zero sandbox exec -- powershell -Command "Set-Content out.txt x"

`)
	return err
}
