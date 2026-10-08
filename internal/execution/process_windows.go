//go:build windows

package execution

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

func ConfigureProcessGroup(cmd *exec.Cmd) {}

// KillProcessTree asks taskkill /T to kill pid and descendants it can still
// discover from that live root. If taskkill fails, it falls back to killing pid
// alone but still returns the tree error because descendants may have survived.
// Neither path can recover descendants after the root has exited.
func KillProcessTree(pid int) error {
	return killProcessTree(pid, runTaskkill, killRootProcess)
}

func killProcessTree(pid int, killTree, killRoot func(int) error) error {
	treeErr := killTree(pid)
	if treeErr == nil {
		return nil
	}
	rootErr := killRoot(pid)
	if rootErr != nil {
		return errors.Join(
			fmt.Errorf("kill process tree rooted at %d: %w", pid, treeErr),
			fmt.Errorf("kill root process %d: %w", pid, rootErr),
		)
	}
	return fmt.Errorf("kill process tree rooted at %d (root process killed directly): %w", pid, treeErr)
}

func runTaskkill(pid int) error {
	return exec.Command(taskkillPath(), "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
}

func killRootProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Kill()
}

func TerminateProcessTree(pid int, _, _ time.Duration) error { return KillProcessTree(pid) }

func taskkillPath() string {
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		systemRoot = os.Getenv("windir")
	}
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}
	return filepath.Join(systemRoot, "System32", "taskkill.exe")
}
