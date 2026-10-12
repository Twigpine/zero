package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/Gitlawb/zero/internal/background"
	"github.com/Gitlawb/zero/internal/sandbox"
)

// reentrancyMarkers are the env vars zero sets on a command it has ALREADY
// wrapped in a sandbox. The daemon MUST strip them from every worker's
// environment: a worker that inherited them would trip zero's re-entrancy guard
// (sandbox.IsAlreadySandboxed needs BOTH) and run with a pass-through plan —
// i.e. UNSANDBOXED — silently defeating the sandbox. Stripping them forces each
// worker to (re)establish its own sandbox. This is the daemon's #1 security
// invariant: it must NOT bypass the sandbox.
var reentrancyMarkers = []string{sandbox.EnvSandboxed, sandbox.EnvSandboxBackend}

// scrubWorkerEnv returns env with the sandbox re-entrancy markers removed. Every
// other variable is preserved — including the ZERO_SANDBOX_* policy config the
// worker reads to rebuild its own policy — so the sandbox policy is PROPAGATED,
// not bypassed. The caller's slice is never mutated.
func scrubWorkerEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		drop := false
		for _, marker := range reentrancyMarkers {
			if strings.HasPrefix(kv, marker+"=") {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

// execWorker is a WorkerHandle backed by a `zero exec` child process speaking
// stream-json on stdout.
type execWorker struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	lines  Lines
	pid    int
}

func (w *execWorker) Stdout() Lines { return w.lines }
func (w *execWorker) Pid() int      { return w.pid }

func (w *execWorker) Wait() (int, error) {
	err := w.cmd.Wait()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return -1, err
}

func (w *execWorker) Kill() error {
	if w.cmd.Process == nil {
		return nil
	}
	// The group the worker was launched into, not one rediscovered from its PID:
	// the pool may call this after the worker has exited and been waited, and its
	// children can still be running. See background.TerminateCommandGroup.
	return background.TerminateCommandGroup(w.cmd)
}

// readerLines adapts a bufio.Reader to the Lines interface. Unlike a capped
// bufio.Scanner it imposes NO per-line size limit, so a legitimately large
// stream-json line from our own trusted worker (a big final answer or tool
// result) is never dropped. io.EOF ends the stream; a trailing line without a
// newline is still yielded.
type readerLines struct {
	r *bufio.Reader
}

func (rl *readerLines) Next() (string, bool, error) {
	line, err := rl.r.ReadString('\n')
	line = strings.TrimRight(line, "\r\n")
	if err != nil {
		if errors.Is(err, io.EOF) {
			if line != "" {
				return line, true, nil // final line with no trailing newline
			}
			return "", false, nil
		}
		return "", false, err
	}
	return line, true, nil
}

// ExecLauncherConfig configures the production worker launcher.
type ExecLauncherConfig struct {
	// Executable is the zero binary to spawn (defaults to os.Executable()).
	Executable string
	// BaseArgs are prepended before the per-session args (defaults to the headless
	// stream-json exec invocation `exec --output-format stream-json`).
	BaseArgs []string
	// Env is the base environment for workers (defaults to os.Environ()). The
	// re-entrancy markers are always scrubbed from it.
	Env []string
}

// NewExecLauncher builds a Launcher that spawns `zero exec` workers which speak
// stream-json on stdout. Each worker:
//   - runs with the re-entrancy markers SCRUBBED from its env (so it establishes
//     its own sandbox; the daemon never bypasses the sandbox), while the rest of
//     the policy config/env is propagated;
//   - is placed in its own process group (background.ConfigureChildProcessGroup)
//     so Kill / drain can terminate it and any children cleanly;
//   - has its per-session working directory (spec.Cwd) and flags (spec.Args)
//     applied. The pool does the os/exec itself and uses internal/background only
//     for process-group setup + cross-platform terminate.
func NewExecLauncher(cfg ExecLauncherConfig) (Launcher, error) {
	exe := cfg.Executable
	if exe == "" {
		resolved, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("daemon: locate zero executable: %w", err)
		}
		exe = resolved
	}
	baseArgs := cfg.BaseArgs
	if baseArgs == nil {
		baseArgs = []string{"exec", "--output-format", "stream-json"}
	}
	baseEnv := cfg.Env
	if baseEnv == nil {
		baseEnv = os.Environ()
	}
	env := scrubWorkerEnv(baseEnv)

	return func(ctx context.Context, spec WorkerSpec) (WorkerHandle, error) {
		args := make([]string, 0, len(baseArgs)+len(spec.Args))
		args = append(args, baseArgs...)
		args = append(args, spec.Args...)

		cmd := exec.CommandContext(ctx, exe, args...)
		cmd.Dir = spec.Cwd
		cmd.Env = env
		cmd.Stdin = nil // the prompt is passed via flags; no streamed stdin input
		background.ConfigureChildProcessGroup(cmd)
		// CommandContext's default cancel sends os.Process.Kill to the LEADER only,
		// orphaning the process group we just configured (a stuck worker's children
		// would survive ctx cancellation). Terminate the whole group instead, the
		// same launch-time group terminate Kill() uses (D11). Cancel runs while Wait
		// is in progress, so it must not reap.
		cmd.Cancel = func() error {
			return background.TerminateCommandGroup(cmd)
		}

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, fmt.Errorf("daemon: worker stdout pipe: %w", err)
		}
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("daemon: start worker: %w", err)
		}

		return &execWorker{
			cmd:    cmd,
			stdout: stdout,
			// A bufio.Reader (not a capped Scanner) so a legitimately large stream-json
			// line from our own trusted worker is never dropped — the 1 MiB cap is only
			// correct for untrusted network frames (protocol.go), not this pipe (D1).
			lines: &readerLines{r: bufio.NewReaderSize(stdout, 64*1024)},
			pid:   cmd.Process.Pid,
		}, nil
	}, nil
}
