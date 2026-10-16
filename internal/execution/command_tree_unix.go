//go:build !windows

package execution

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

type commandTree struct {
	mu          sync.Mutex
	ready       chan struct{}
	readyOnce   sync.Once
	groupID     int
	anchor      *exec.Cmd
	anchorInput io.WriteCloser
	signal      func(int, syscall.Signal) error
	canceled    bool
	cancelErr   error
	closed      bool
}

func prepareCommandTree(command *exec.Cmd) (*commandTree, error) {
	// Keep the group leader alive until cleanup so an exited command cannot
	// leave a reusable PID as the only identity for its live descendants.
	anchor := exec.Command("/bin/sh", "-c", "read _")
	anchor.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	anchorInput, err := anchor.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := anchor.Start(); err != nil {
		_ = anchorInput.Close()
		return nil, err
	}

	tree := &commandTree{
		ready:       make(chan struct{}),
		groupID:     anchor.Process.Pid,
		anchor:      anchor,
		anchorInput: anchorInput,
		signal:      syscall.Kill,
	}
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.Setpgid = true
	command.SysProcAttr.Pgid = tree.groupID
	return tree, nil
}

func (tree *commandTree) attach(*os.Process) error {
	tree.readyOnce.Do(func() { close(tree.ready) })
	return nil
}

func (tree *commandTree) cancel() error {
	<-tree.ready
	tree.mu.Lock()
	defer tree.mu.Unlock()
	if tree.closed || tree.canceled || tree.groupID <= 1 {
		return tree.cancelErr
	}
	tree.canceled = true
	if err := tree.signal(-tree.groupID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		tree.cancelErr = err
	}
	return tree.cancelErr
}

func (tree *commandTree) close() error {
	tree.mu.Lock()
	defer tree.mu.Unlock()
	if tree.closed {
		return nil
	}
	tree.closed = true
	if tree.anchorInput != nil {
		_ = tree.anchorInput.Close()
		tree.anchorInput = nil
	}
	if tree.anchor != nil {
		_ = tree.anchor.Wait()
		tree.anchor = nil
	}
	tree.groupID = 0
	return nil
}
