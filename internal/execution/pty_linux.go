//go:build linux

package execution

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func startPTYProcess(command *exec.Cmd, output io.Writer) (io.WriteCloser, func(), error) {
	master, slave, err := openPTY()
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = master.Close(); _ = slave.Close() }
	command.Stdin, command.Stdout, command.Stderr = slave, slave, slave
	hardenPTYProcess(command)
	if err := command.Start(); err != nil {
		cleanup()
		return nil, nil, err
	}
	_ = slave.Close()
	copied := make(chan struct{})
	go func() { defer close(copied); _, _ = io.Copy(output, master) }()
	return master, func() {
		select {
		case <-copied:
		case <-time.After(processWaitDelay):
		}
		_ = master.Close()
	}, nil
}

func openPTY() (*os.File, *os.File, error) {
	masterFD, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	master := os.NewFile(uintptr(masterFD), "/dev/ptmx")
	if err := unix.IoctlSetPointerInt(masterFD, unix.TIOCSPTLCK, 0); err != nil {
		_ = master.Close()
		return nil, nil, err
	}
	// A fresh PTY defaults to a 0x0 window; seed a sane size so the process
	// never sees zero dimensions before an attached client reports its own.
	_ = unix.IoctlSetWinsize(masterFD, unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: 80})
	pts, err := unix.IoctlGetInt(masterFD, unix.TIOCGPTN)
	if err != nil {
		_ = master.Close()
		return nil, nil, err
	}
	slaveName := fmt.Sprintf("/dev/pts/%d", pts)
	slaveFD, err := unix.Open(slaveName, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = master.Close()
		return nil, nil, err
	}
	return master, os.NewFile(uintptr(slaveFD), slaveName), nil
}

func hardenPTYProcess(command *exec.Cmd) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.Setsid = true
	command.SysProcAttr.Setctty = true
	command.SysProcAttr.Ctty = 0
	command.WaitDelay = processWaitDelay
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return nil
	}
}

// resizePTY reports a new window size to the PTY behind an interactive
// process's stdin writer, so full-screen attached terminals drive the
// session's own line wrapping.
func resizePTY(w io.Writer, cols, rows int) error {
	file, ok := w.(interface{ Fd() uintptr })
	if !ok {
		return errors.New("pty transport is unavailable on this platform")
	}
	return unix.IoctlSetWinsize(int(file.Fd()), unix.TIOCSWINSZ, &unix.Winsize{
		Row: uint16(rows), Col: uint16(cols),
	})
}
