package dictation

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/Gitlawb/zero/internal/execution"
)

// commandSpec describes one capture-process invocation. Argv is always
// discrete — never a shell string (the same invariant documented in
// internal/imageinput/clipboard.go).
type commandSpec struct {
	name string
	args []string
	// wantStdout wires up a stdout pipe (streaming capture reads raw PCM).
	wantStdout bool
	// stopViaStdin, when non-empty, makes graceful stop write this to the
	// process's stdin instead of signaling — ffmpeg finalizes cleanly on "q",
	// and Windows has no os.Interrupt delivery anyway.
	stopViaStdin string
}

// processHandle is a started capture process. Injectable via processStarter so
// unit tests fake OS-boundary behavior, mirroring internal/lsp's serverStarter
// seam (this package's clipboard.go-style shell-outs get the test seam that
// file lacks).
type processHandle interface {
	// StopGracefully asks the tool to finish and flush output (SIGINT, or the
	// spec's stdin stop text). The recorder falls back to Kill on timeout.
	StopGracefully() error
	Wait() error
	Kill() error
}

// processStarter launches a capture process, returning its handle and — when
// the spec asks for it — its stdout pipe.
type processStarter func(spec commandSpec) (processHandle, io.ReadCloser, error)

// commandOutputRunner runs a short one-shot command to completion and returns
// its combined output. Used for helper invocations that aren't recordings
// (Termux start/stop, Windows ffmpeg device listing) and the offline
// transcription exec — ctx lets a caller cancel/timeout a wedged process.
type commandOutputRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

func startProcess(spec commandSpec) (processHandle, io.ReadCloser, error) {
	cmd := exec.Command(spec.name, spec.args...)
	var stdout io.ReadCloser
	if spec.wantStdout {
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			return nil, nil, err
		}
		stdout = pipe
	}
	var stdin io.WriteCloser
	if spec.stopViaStdin != "" {
		pipe, err := cmd.StdinPipe()
		if err != nil {
			return nil, nil, err
		}
		stdin = pipe
	}
	if err := cmd.Start(); err != nil {
		// StdoutPipe/StdinPipe allocate fds the runtime only reclaims via
		// Wait; on Start failure the process is never established, so close
		// any pipes we already opened to avoid leaking them.
		if stdout != nil {
			_ = stdout.Close()
		}
		if stdin != nil {
			_ = stdin.Close()
		}
		return nil, nil, err
	}
	return &realProcess{cmd: cmd, stdin: stdin, stopText: spec.stopViaStdin}, stdout, nil
}

type realProcess struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stopText string
}

func (p *realProcess) StopGracefully() error {
	if p.stopText != "" && p.stdin != nil {
		_, err := io.WriteString(p.stdin, p.stopText)
		closeErr := p.stdin.Close()
		if err != nil {
			return err
		}
		return closeErr
	}
	// os.Interrupt is how arecord/sox finalize their WAV header. Unsupported
	// on Windows, but the only Windows tool (ffmpeg) stops via stdin above.
	return p.cmd.Process.Signal(os.Interrupt)
}

func (p *realProcess) Wait() error { return p.cmd.Wait() }

func (p *realProcess) Kill() error { return p.cmd.Process.Kill() }

func runCommandOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := execution.RunCommand(ctx, cmd)
	return out.Bytes(), err
}

// waitWithTimeout waits for the process, killing it if it ignores the graceful
// stop for longer than the grace window. Capture tools exit on SIGINT/"q"
// almost instantly; a hang here means a wedged device driver, and a leaked
// recording process holds the microphone open.
func waitWithTimeout(proc processHandle, grace time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- proc.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(grace):
		_ = proc.Kill()
		return <-done
	}
}
