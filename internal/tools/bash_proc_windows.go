//go:build windows

package tools

import (
	"os/exec"
	"syscall"
	"time"

	"github.com/Twigpine/zero/internal/execution"
	zeroSandbox "github.com/Twigpine/zero/internal/sandbox"
)

// bashWaitDelay bounds how long Wait blocks for the I/O pipes to drain after the
// process has exited or the context's Cancel has run, so a backgrounded child
// holding the pipes cannot make Run() hang past the timeout. Var (not const) so
// tests can shorten it.
var bashWaitDelay = 2 * time.Second

// hardenProcessLifetime makes a Windows shell command killable as a process
// tree. cmd.exe starts helper commands as child processes, so killing only the
// shell can leave a long-running child alive and holding cwd/temp handles after
// Zero exits.
func hardenProcessLifetime(command *exec.Cmd) {
	command.WaitDelay = bashWaitDelay
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return execution.KillProcessTree(command.Process.Pid)
	}
}

// applyWindowsShellCommandLine overrides the cmd.exe fallback's raw child
// command line so commandText reaches cmd.exe unescaped. PowerShell consumes
// ordinary argv quoting and must not take this path. Wrapped commands are
// handled after unwrapping by the Windows sandbox runner.
func applyWindowsShellCommandLine(command *exec.Cmd, commandText string, wrapped bool, cmdFallback bool) {
	if wrapped || !cmdFallback {
		return
	}
	command.SysProcAttr = &syscall.SysProcAttr{CmdLine: zeroSandbox.WindowsShellCommandLine(commandText)}
}
