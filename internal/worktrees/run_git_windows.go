//go:build windows

package worktrees

import (
	"os/exec"
	"time"
)

// worktreeWaitDelay bounds how long Wait blocks for the child's stdout/stderr
// pipes to drain after the process exits or its context is cancelled, so a
// leaked grandchild cannot hang the parent past cancel/timeout. Var (not const)
// so tests can shorten it.
var worktreeWaitDelay = 2 * time.Second

// hardenWorktreeGit sets WaitDelay so a leaked grandchild cannot block Wait
// indefinitely. Windows lacks POSIX process groups; tree-killing is not wired
// for this path, so the default Cancel (Process.Kill) plus WaitDelay is used.
func hardenWorktreeGit(command *exec.Cmd) {
	command.WaitDelay = worktreeWaitDelay
}
