//go:build linux

package sandbox

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Twigpine/zero/internal/execution"
	"golang.org/x/sys/unix"
)

const protectedCreatePollInterval = 10 * time.Millisecond

// An absent protected path cannot be mounted read-only without making it
// appear inside the child. The monitor preserves workspace path fidelity and
// treats the kernel's create/move event as the denial signal. The path can
// briefly appear on the host before the command is killed and it is removed;
// parsing the event ensures a fast create+unlink is still reported.

type protectedCreateMonitor struct {
	targets   []string
	stderr    io.Writer
	inotifyFD int
	watches   map[int]string
	targetSet map[string]struct{}
	stop      chan struct{}
	done      chan struct{}
	violation atomic.Bool
	once      sync.Once
	path      string
}

type synchronizedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (writer *synchronizedWriter) Write(data []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.writer.Write(data)
}

func runLinuxSandboxWithProtectedCreateMonitor(bwrapPath string, plan linuxSandboxBwrapPlan, reportPath string, stderr io.Writer) int {
	if stderr == nil {
		stderr = io.Discard
	}
	safeStderr := &synchronizedWriter{writer: stderr}
	monitor, err := newProtectedCreateMonitor(plan.ProtectedCreateTargets, safeStderr)
	if err != nil {
		fmt.Fprintln(safeStderr, LinuxSandboxHelperName+": prepare protected metadata monitor: "+err.Error())
		return 125
	}

	command := exec.Command(bwrapPath, plan.Args...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = safeStderr
	command.Env = os.Environ()
	if err := command.Start(); err != nil {
		monitor.close()
		fmt.Fprintln(safeStderr, LinuxSandboxHelperName+": start bubblewrap: "+err.Error())
		return 126
	}

	monitor.start(func() {
		_ = command.Process.Kill()
	})
	waitErr := command.Wait()
	violated, target := monitor.stopAndCleanup()
	if violated {
		report := execution.AdapterReport{Denial: &execution.Denial{
			Capability:  execution.Capability{Kind: execution.CapabilityProtectedMetadata, Scope: target},
			Source:      execution.DenialSourceConfiguredPolicy,
			Reason:      "protected workspace metadata cannot be created by a sandboxed command",
			Recoverable: true,
			NextAction:  execution.DenialNextActionRequestApproval,
		}}
		if err := writeLinuxExecutionReport(reportPath, report); err != nil {
			fmt.Fprintln(safeStderr, LinuxSandboxHelperName+": write structured policy report: "+err.Error())
		}
		return 1
	}
	if waitErr == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return exitErr.ExitCode()
	}
	fmt.Fprintln(safeStderr, LinuxSandboxHelperName+": wait for bubblewrap: "+waitErr.Error())
	return 126
}

func newProtectedCreateMonitor(targets []string, stderr io.Writer) (*protectedCreateMonitor, error) {
	monitor := &protectedCreateMonitor{
		targets:   dedupeStrings(targets),
		stderr:    stderr,
		inotifyFD: -1,
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		watches:   make(map[int]string),
		targetSet: make(map[string]struct{}),
	}
	for _, target := range monitor.targets {
		target = filepath.Clean(target)
		monitor.targetSet[target] = struct{}{}
		if _, err := os.Lstat(target); err == nil {
			return nil, fmt.Errorf("protected metadata path appeared before sandbox start: %s", target)
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("inspect protected metadata path %s: %w", target, err)
		}
	}
	monitor.inotifyFD, monitor.watches = openProtectedCreateInotify(monitor.targets)
	return monitor, nil
}

func openProtectedCreateInotify(targets []string) (int, map[int]string) {
	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		return -1, nil
	}
	parents := make(map[string]struct{}, len(targets))
	watches := make(map[int]string, len(targets))
	for _, target := range targets {
		parent := filepath.Dir(target)
		if _, exists := parents[parent]; exists {
			continue
		}
		parents[parent] = struct{}{}
		watch, err := unix.InotifyAddWatch(fd, parent, unix.IN_CREATE|unix.IN_MOVED_TO|unix.IN_DELETE_SELF|unix.IN_MOVE_SELF)
		if err != nil {
			continue
		}
		watches[watch] = parent
	}
	if len(watches) == 0 {
		_ = unix.Close(fd)
		return -1, nil
	}
	return fd, watches
}

func (monitor *protectedCreateMonitor) start(onViolation func()) {
	go func() {
		defer close(monitor.done)
		for {
			monitor.scanAndRemove(onViolation)
			monitor.wait(onViolation)
			select {
			case <-monitor.stop:
				return
			default:
			}
		}
	}()
}

func (monitor *protectedCreateMonitor) wait(onViolation func()) {
	if monitor.inotifyFD < 0 {
		select {
		case <-monitor.stop:
		case <-time.After(protectedCreatePollInterval):
		}
		return
	}
	pollFD := []unix.PollFd{{Fd: int32(monitor.inotifyFD), Events: unix.POLLIN}}
	_, _ = unix.Poll(pollFD, int(protectedCreatePollInterval/time.Millisecond))
	if pollFD[0].Revents&unix.POLLIN != 0 {
		monitor.drainInotifyEvents(onViolation)
	}
}

func (monitor *protectedCreateMonitor) drainInotifyEvents(onViolation func()) {
	var buffer [4096]byte
	const eventHeaderSize = 16
	for {
		read, err := unix.Read(monitor.inotifyFD, buffer[:])
		if err != nil {
			return
		}
		for offset := 0; offset+eventHeaderSize <= read; {
			watch := int(int32(binary.NativeEndian.Uint32(buffer[offset : offset+4])))
			mask := binary.NativeEndian.Uint32(buffer[offset+4 : offset+8])
			nameLength := int(binary.NativeEndian.Uint32(buffer[offset+12 : offset+16]))
			next := offset + eventHeaderSize + nameLength
			if nameLength < 0 || next > read {
				break
			}
			name := strings.TrimRight(string(buffer[offset+eventHeaderSize:next]), "\x00")
			if parent, ok := monitor.watches[watch]; ok && mask&(unix.IN_CREATE|unix.IN_MOVED_TO) != 0 && name != "" {
				target := filepath.Clean(filepath.Join(parent, name))
				if _, protected := monitor.targetSet[target]; protected {
					monitor.recordViolation(target, onViolation)
					removeProtectedCreateTarget(target)
				}
			}
			offset = next
		}
	}
}

func (monitor *protectedCreateMonitor) scanAndRemove(onViolation func()) {
	for _, target := range monitor.targets {
		if _, err := os.Lstat(target); os.IsNotExist(err) {
			continue
		} else if err != nil {
			continue
		}
		monitor.recordViolation(target, onViolation)
		removeProtectedCreateTarget(target)
	}
}

func (monitor *protectedCreateMonitor) recordViolation(target string, onViolation func()) {
	monitor.once.Do(func() {
		monitor.violation.Store(true)
		monitor.path = target
		fmt.Fprintln(monitor.stderr, "sandbox blocked creation of protected workspace metadata path "+target)
		onViolation()
	})
}

func removeProtectedCreateTarget(target string) bool {
	for attempt := 0; attempt < 100; attempt++ {
		_, err := os.Lstat(target)
		if os.IsNotExist(err) {
			return attempt > 0
		}
		if err != nil {
			return false
		}
		err = os.RemoveAll(target)
		if err == nil || os.IsNotExist(err) {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

func (monitor *protectedCreateMonitor) stopAndCleanup() (bool, string) {
	close(monitor.stop)
	<-monitor.done
	monitor.scanAndRemove(func() {})
	monitor.close()
	return monitor.violation.Load(), monitor.path
}

func (monitor *protectedCreateMonitor) close() {
	if monitor.inotifyFD >= 0 {
		_ = unix.Close(monitor.inotifyFD)
		monitor.inotifyFD = -1
	}
}

func writeLinuxExecutionReport(path string, report execution.AdapterReport) error {
	if path == "" {
		return errors.New("policy report path is unavailable")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	encoderErr := json.NewEncoder(file).Encode(report)
	closeErr := file.Close()
	if encoderErr != nil {
		_ = os.Remove(path)
		return encoderErr
	}
	return closeErr
}
