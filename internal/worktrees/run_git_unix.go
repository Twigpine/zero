//go:build !windows

package worktrees

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// worktreeWaitDelay bounds how long Wait blocks for the child's stdout/stderr
// pipes to drain after the process exits or its context is cancelled, so a
// leaked grandchild cannot hang the parent past cancel/timeout. Var (not const)
// so tests can shorten it.
var worktreeWaitDelay = 2 * time.Second

// hardenWorktreeGit makes a git subprocess killable as a single unit. Setpgid
// puts the child into its own process group, so on cancel/timeout we signal the
// whole group (negative pid) — any long-running git subprocess dies with it
// instead of being orphaned. WaitDelay is the backstop if a grandchild still
// holds a pipe after the group is killed. Must be called before command.Run.
func hardenWorktreeGit(command *exec.Cmd) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.Setpgid = true
	command.WaitDelay = worktreeWaitDelay
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		// Negative pid targets the whole process group led by the child.
		if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return nil
	}
}
