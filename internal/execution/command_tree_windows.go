//go:build windows

package execution

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type commandTree struct {
	job           windows.Handle
	processHandle windows.Handle
	contained     bool
	ready         chan struct{}
}

func prepareCommandTree(command *exec.Cmd) (*commandTree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("execution: create command job: %w", err)
	}
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	return &commandTree{job: job, ready: make(chan struct{})}, nil
}

func (tree *commandTree) attach(process *os.Process) error {
	defer close(tree.ready)
	if process == nil {
		return nil
	}
	handle, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false,
		uint32(process.Pid),
	)
	if err != nil {
		return fmt.Errorf("open suspended command process: %w", err)
	}
	tree.processHandle = handle
	if err := assignCommandProcessToJob(tree.job, handle); err != nil {
		// Without job containment, a root can exit before cancellation and
		// leave no identity-safe way to find descendants holding output pipes.
		// Fail while it is still suspended so no descendant can escape.
		return fmt.Errorf("assign suspended command process to job: %w", err)
	}
	tree.contained = true
	return resumeProcess(uint32(process.Pid))
}

func (tree *commandTree) cancel() error {
	<-tree.ready
	if tree.contained {
		return windows.TerminateJobObject(tree.job, 1)
	}
	return nil
}

func (tree *commandTree) close() (err error) {
	if tree.job != 0 {
		err = errors.Join(err, windows.CloseHandle(tree.job))
		tree.job = 0
	}
	if tree.processHandle != 0 {
		err = errors.Join(err, windows.CloseHandle(tree.processHandle))
		tree.processHandle = 0
	}
	return err
}

var assignCommandProcessToJob = windows.AssignProcessToJobObject

func resumeProcess(pid uint32) (err error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, windows.CloseHandle(snapshot)) }()

	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	if err := windows.Thread32First(snapshot, &entry); err != nil {
		return err
	}
	for {
		if entry.OwnerProcessID == pid {
			thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if err != nil {
				return err
			}
			_, resumeErr := windows.ResumeThread(thread)
			return errors.Join(resumeErr, windows.CloseHandle(thread))
		}
		if err := windows.Thread32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				return fmt.Errorf("execution: no thread found for suspended process %d", pid)
			}
			return err
		}
	}
}
