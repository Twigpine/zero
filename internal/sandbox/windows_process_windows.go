//go:build windows

package sandbox

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func runWindowsCommandAsUser(token windows.Token, config WindowsSandboxCommandConfig) (int, error) {
	if len(config.Command) == 0 {
		return 1, errorsNewWindowsProcess("missing command")
	}
	commandLine, err := windowsCommandLine(config.Command)
	if err != nil {
		return 1, err
	}
	commandLinePtr, err := windows.UTF16PtrFromString(commandLine)
	if err != nil {
		return 1, fmt.Errorf("encode command line: %w", err)
	}
	cwdPtr, err := windows.UTF16PtrFromString(config.CommandCWD)
	if err != nil {
		return 1, fmt.Errorf("encode command cwd: %w", err)
	}
	envBlock, err := windowsEnvironmentBlock(config.Env)
	if err != nil {
		return 1, err
	}
	desktopPtr, err := windows.UTF16PtrFromString(`winsta0\default`)
	if err != nil {
		return 1, fmt.Errorf("encode desktop: %w", err)
	}

	stdin := windows.Handle(os.Stdin.Fd())
	stdout := windows.Handle(os.Stdout.Fd())
	stderr := windows.Handle(os.Stderr.Fd())
	for _, handle := range []windows.Handle{stdin, stdout, stderr} {
		if err := windows.SetHandleInformation(handle, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
			return 1, fmt.Errorf("make stdio handle inheritable: %w", err)
		}
	}

	var startup windows.StartupInfo
	startup.Cb = uint32(unsafe.Sizeof(startup))
	startup.Desktop = desktopPtr
	startup.Flags = windows.STARTF_USESTDHANDLES
	startup.StdInput = stdin
	startup.StdOutput = stdout
	startup.StdErr = stderr
	var process windows.ProcessInformation
	envPtr := &envBlock[0]
	// Claim the report side channel BEFORE the launch. Publishing can fail, and
	// after CreateProcessAsUser succeeds a running child exists whether or not the
	// fact can be recorded; taking the file first moves that failure to a point
	// where there is still nothing to own.
	report, err := openWindowsExecutionReport(config.ExecutionReportPath)
	if err != nil {
		return 1, err
	}
	published := false
	defer func() { report.close(published) }()
	if err := windows.CreateProcessAsUser(
		token,
		nil,
		commandLinePtr,
		nil,
		nil,
		true,
		windows.CREATE_UNICODE_ENVIRONMENT|windows.CREATE_SUSPENDED,
		envPtr,
		cwdPtr,
		&startup,
		&process,
	); err != nil {
		return 1, fmt.Errorf("CreateProcessAsUser: %w", err)
	}
	defer windows.CloseHandle(process.Process)
	defer windows.CloseHandle(process.Thread)
	// THE TRANSITION ONLY THIS PROCESS CAN SEE. Everything above can fail with the
	// helper already running, and the parent's exec.Cmd.Process cannot tell those
	// failures apart from a real sandboxed launch. The restricted child exists as
	// of this line, so this is where the fact is published.
	//
	// CREATED SUSPENDED, SO REPORTING CANNOT LOSE A RACE IT IS IN. The child
	// inherits the MCP pipes. Created runnable, it could answer initialize, emit a
	// malformed response, or close stdout before this helper was next scheduled,
	// and a parent reading the report at that moment would see the empty file the
	// open above created and cache "no child" about a server that was already
	// running unconfined. It could also fail to publish AFTER a runnable child had
	// begun making external changes, and reaping the child then does not undo the
	// work it did.
	//
	// A suspended child has executed nothing. Publish first and resume second, and
	// the absence of a report becomes a fact rather than a race: every failure
	// between creation and resume terminates a process that never ran, so "no child
	// launched" is true when the parent reads it.
	//
	// AND THE RECORD HAS TO MEAN THE SAME THING ON THE WAY BACK OUT. The report
	// is published before the resume, so a failure between the two leaves a report
	// saying a child launched about a process that is being reaped without ever
	// having run. publishThenResume unwinds the record on that path, which is
	// what keeps "no child launched" true for the parent on every failure before
	// the child could execute, not just the ones before the write.
	published, err = publishThenResume(report, func() error {
		_, err := windows.ResumeThread(process.Thread)
		return err
	})
	if err != nil {
		terminateSuspendedWindowsChild(process.Process)
		return 1, err
	}
	if _, err := windows.WaitForSingleObject(process.Process, windows.INFINITE); err != nil {
		return 1, fmt.Errorf("wait for sandboxed process: %w", err)
	}
	var exitCode uint32
	if err := windows.GetExitCodeProcess(process.Process, &exitCode); err != nil {
		return 1, fmt.Errorf("read sandboxed process exit code: %w", err)
	}
	return int(exitCode), nil
}

func windowsCommandLine(args []string) (string, error) {
	if len(args) == 0 {
		return "", errorsNewWindowsProcess("missing command")
	}
	for _, arg := range args {
		if strings.ContainsRune(arg, 0) {
			return "", errorsNewWindowsProcess("command argument contains NUL")
		}
	}
	// Zero's cmd.exe shell invocation needs its final element (the shell
	// command text) reaching cmd.exe raw, not escaped like a normal argv
	// element: see WindowsShellCommandLine for why. Every other command this
	// runner might launch is escaped normally below.
	if commandLine, ok := windowsShellCommandLineFromArgs(args); ok {
		return commandLine, nil
	}
	out := make([]string, 0, len(args))
	for _, arg := range args {
		out = append(out, syscall.EscapeArg(arg))
	}
	return strings.Join(out, " "), nil
}

func windowsEnvironmentBlock(env map[string]string) ([]uint16, error) {
	keys := make([]string, 0, len(env))
	for key := range env {
		if strings.TrimSpace(key) == "" {
			continue
		}
		if strings.ContainsRune(key, 0) || strings.ContainsRune(env[key], 0) {
			return nil, errorsNewWindowsProcess("environment contains NUL")
		}
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		left := strings.ToUpper(keys[i])
		right := strings.ToUpper(keys[j])
		if left == right {
			return keys[i] < keys[j]
		}
		return left < right
	})
	block := make([]uint16, 0)
	for _, key := range keys {
		entry, err := syscall.UTF16FromString(key + "=" + env[key])
		if err != nil {
			return nil, fmt.Errorf("encode environment variable %s: %w", key, err)
		}
		block = append(block, entry...)
	}
	block = append(block, 0)
	return block, nil
}

func errorsNewWindowsProcess(message string) error {
	return fmt.Errorf("windows sandbox process: %s", message)
}
