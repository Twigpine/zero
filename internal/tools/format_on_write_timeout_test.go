package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A FORMATTER THAT RUNS OUT OF TIME MUST NOT LOOK LIKE ONE THAT SUCCEEDED.
//
// Every other way format-on-write falls back is a standing fact about the
// environment: the toggle is off, the extension has no formatter, the binary is
// not installed. Those are silent because nothing is wrong. A deadline firing is
// different: formatting was configured, available and expected, and the file was
// written unformatted anyway because the machine was slow. Silent, the caller
// believes it wrote canonical style and learns otherwise from a CI format check
// it cannot see, which is the failure this feature exists to prevent.
//
// THE DEADLINE IS SHRUNK RATHER THAN THE FORMATTER SLOWED. A formatter that
// really sleeps has to be killed, and on Windows the batch file that hosts the
// sleep leaves the sleeping grandchild holding the working directory, so the
// test then fails in TempDir cleanup rather than on anything it asserts. An
// expired budget against an ordinary fast formatter reaches the same branch by
// the same route, deterministically and in microseconds.
func TestFormatOnWriteReportsATimeout(t *testing.T) {
	formatter := installFakeFormatter(t, ".fastfmt", "fastfmt", succeedingFormatterScript())
	shortenFormatOnWriteTimeout(t, time.Nanosecond)
	t.Setenv("ZERO_FORMAT_ON_WRITE", "1")

	target := filepath.Join(t.TempDir(), "subject.fastfmt")
	const written = "written   but   not   formatted\n"
	if err := os.WriteFile(target, []byte(written), 0o644); err != nil {
		t.Fatal(err)
	}

	formatting := maybeFormatWrittenFile(context.Background(), target, written)

	if formatting.Content != written {
		t.Fatalf("content = %q, want the bytes that were written", formatting.Content)
	}
	if !formatting.TimedOut {
		t.Fatal("a formatter cut off by the deadline was reported as an ordinary miss, so the write claims a formatting that never happened")
	}
	notice := formatting.notice("subject.fastfmt")
	for _, want := range []string{"subject.fastfmt", "not formatted", formatter} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice %q does not mention %q", notice, want)
		}
	}
}

// And the same formatter inside its budget says nothing at all, or the notice
// above would be reporting the deadline rather than the miss.
func TestFormatOnWriteStaysQuietWhenTheFormatterFinishes(t *testing.T) {
	installFakeFormatter(t, ".fastfmt", "fastfmt", succeedingFormatterScript())
	t.Setenv("ZERO_FORMAT_ON_WRITE", "1")

	target := filepath.Join(t.TempDir(), "subject.fastfmt")
	const written = "written\n"
	if err := os.WriteFile(target, []byte(written), 0o644); err != nil {
		t.Fatal(err)
	}

	formatting := maybeFormatWrittenFile(context.Background(), target, written)
	if formatting.TimedOut {
		t.Error("a formatter that finished was reported as timed out")
	}
	if notice := formatting.notice("subject.fastfmt"); notice != "" {
		t.Errorf("a successful format produced a notice: %q", notice)
	}
}

// The caller cancelling is not this notice's business: that run is already
// being reported as cancelled, and saying the formatter was too slow on top of
// it would be wrong about why.
func TestFormatOnWriteStaysQuietWhenTheCallerCancels(t *testing.T) {
	installFakeFormatter(t, ".fastfmt", "fastfmt", succeedingFormatterScript())
	t.Setenv("ZERO_FORMAT_ON_WRITE", "1")

	target := filepath.Join(t.TempDir(), "subject.fastfmt")
	const written = "written\n"
	if err := os.WriteFile(target, []byte(written), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	formatting := maybeFormatWrittenFile(ctx, target, written)

	if formatting.Content != written {
		t.Fatalf("content = %q, want the bytes that were written", formatting.Content)
	}
	if formatting.TimedOut {
		t.Error("a cancelled run was reported as a formatter timeout")
	}
	if notice := formatting.notice("subject.fastfmt"); notice != "" {
		t.Errorf("a cancelled run produced a notice: %q", notice)
	}
}

// A formatter that runs and refuses the file is also not a timeout. It usually
// means content it could not parse, which the write does not promise to fix,
// and reporting it as a slow machine would be wrong about the cause.
func TestFormatOnWriteStaysQuietWhenTheFormatterFails(t *testing.T) {
	installFakeFormatter(t, ".failfmt", "failfmt", failingFormatterScript())
	t.Setenv("ZERO_FORMAT_ON_WRITE", "1")

	target := filepath.Join(t.TempDir(), "subject.failfmt")
	const written = "unparseable\n"
	if err := os.WriteFile(target, []byte(written), 0o644); err != nil {
		t.Fatal(err)
	}

	formatting := maybeFormatWrittenFile(context.Background(), target, written)
	if formatting.TimedOut {
		t.Error("a formatter that exited non-zero was reported as a timeout")
	}
	if notice := formatting.notice("subject.failfmt"); notice != "" {
		t.Errorf("a failing formatter produced a timeout notice: %q", notice)
	}
}

// A WEDGED FORMATTER MUST BE KILLABLE AND MUST NOT BLOCK PAST THE DEADLINE.
//
// formatWithStdin captures stdout, so Run waits for every holder of that pipe,
// not only the process the deadline kills. On Windows an npm-installed prettier
// is a .cmd hosting node.exe, and a grandchild that keeps the pipe open would
// push the return past the timeout. hardenProcessLifetime installs the tree
// kill and the WaitDelay backstop on both routes; without it the command has
// neither.
func TestFormatOnWriteHardensFormatterLifetime(t *testing.T) {
	t.Setenv("ZERO_FORMAT_ON_WRITE", "1")

	for _, stdin := range []bool{true, false} {
		name := map[bool]string{true: "stdin", false: "staging"}[stdin]
		t.Run(name, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "subject.padfmt")
			installFakeAdapter(t, ".padfmt", "padfmt", stdin)

			var seen *exec.Cmd
			previous := formatterCommandObserver
			formatterCommandObserver = func(command *exec.Cmd) { seen = command }
			defer func() { formatterCommandObserver = previous }()

			_ = maybeFormatWrittenFile(context.Background(), target, "written\n")

			if seen == nil {
				t.Fatal("no formatter command was observed")
			}
			if seen.WaitDelay == 0 {
				t.Error("formatter command has no WaitDelay, so a leaked grandchild can block Wait past the deadline")
			}
			if seen.Cancel == nil {
				t.Error("formatter command has no Cancel, so the deadline cannot kill the formatter tree")
			}
		})
	}
}

// installFakeAdapter registers a formatter for the test's duration. The stdin
// flag chooses the stdin/stdout route over the private staging copy.
func installFakeAdapter(t *testing.T, extension, name string, stdin bool) {
	t.Helper()
	binaryName := name + formatterScriptExtension()
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, binaryName), []byte(succeedingFormatterScript()), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	previous, existed := formatterCommands[extension]
	formatterCommands[extension] = formatterAdapter{argv: []string{binaryName}, stdin: stdin}
	t.Cleanup(func() {
		if existed {
			formatterCommands[extension] = previous
			return
		}
		delete(formatterCommands, extension)
	})
}

// installFakeFormatter puts a formatter on PATH and registers it in the command
// table for the test's duration, returning the binary name the notice carries.
func installFakeFormatter(t *testing.T, extension, name, script string) string {
	t.Helper()
	binaryName := name + formatterScriptExtension()
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, binaryName), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))

	previous, existed := formatterCommands[extension]
	formatterCommands[extension] = formatterAdapter{argv: []string{binaryName}}
	t.Cleanup(func() {
		if existed {
			formatterCommands[extension] = previous
			return
		}
		delete(formatterCommands, extension)
	})
	return binaryName
}

func formatterScriptExtension() string {
	if runtime.GOOS == "windows" {
		return ".bat"
	}
	return ""
}

func succeedingFormatterScript() string {
	if runtime.GOOS == "windows" {
		return "@echo off\r\nexit /b 0\r\n"
	}
	return "#!/bin/sh\nexit 0\n"
}

func failingFormatterScript() string {
	if runtime.GOOS == "windows" {
		return "@echo off\r\nexit /b 3\r\n"
	}
	return "#!/bin/sh\nexit 3\n"
}

// shortenFormatOnWriteTimeout narrows the production deadline for one test.
func shortenFormatOnWriteTimeout(t *testing.T, timeout time.Duration) {
	t.Helper()
	previous := formatOnWriteTimeout
	formatOnWriteTimeout = timeout
	t.Cleanup(func() { formatOnWriteTimeout = previous })
}

// A FORMATTER RUNS ON A PRIVATE STAGING COPY, NEVER ON THE DESTINATION.
//
// Physical formatters are handed a path inside an owner-only sibling directory
// (fsutil.CreatePrivateTempDir), so a killed, cancelled, or clobbering run can
// neither truncate nor half-rewrite the destination. A failure on that staging
// copy therefore keeps the fallback bytes the caller staged, and the
// destination stays byte-for-byte (and inode-for-inode) untouched until the
// single atomic publication in commitFileContents.
func TestFormatOnWriteRunsTheFormatterOnAPrivateCopy(t *testing.T) {
	argRecord := filepath.Join(t.TempDir(), "formatter-arg")
	installFakeFormatter(t, ".clobberfmt", "clobberfmt", recordingClobberingFormatterScript())
	t.Setenv("ZERO_FORMAT_ON_WRITE", "1")
	t.Setenv("ZERO_FORMAT_ARG_RECORD", argRecord)
	requireFormatterClobbers(t, "the bytes the caller wrote\n")

	dir := t.TempDir()
	target := filepath.Join(dir, "subject.clobberfmt")
	const written = "the bytes the caller wrote\n"
	if err := os.WriteFile(target, []byte(written), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}

	formatting := maybeFormatWrittenFile(context.Background(), target, written)

	recorded, err := os.ReadFile(argRecord)
	if err != nil {
		t.Fatalf("formatter never recorded the path it was handed: %v", err)
	}
	handed := strings.TrimSpace(string(recorded))
	if handed == target {
		t.Fatalf("formatter was handed the destination path %q", handed)
	}
	if base := filepath.Base(filepath.Dir(handed)); !strings.HasPrefix(base, ".zero-fmt-") {
		t.Fatalf("formatter path %q is not inside a private staging directory", handed)
	}

	after, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("destination inode changed while the formatter ran on the staging copy")
	}
	onDisk, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != written {
		t.Errorf("destination = %q, want the untouched bytes %q", onDisk, written)
	}
	if formatting.Content != written {
		t.Errorf("content = %q, want the fallback bytes that were written", formatting.Content)
	}
}

// The same isolation must hold on the deadline path, which is the one the
// timeout notice already covers: the disclosure and the file have to agree.
func TestFormatOnWriteKeepsFallbackAndDestinationOnTimeout(t *testing.T) {
	installFakeFormatter(t, ".clobberfmt", "clobberfmt", clobberingFormatterScript())
	shortenFormatOnWriteTimeout(t, time.Nanosecond)
	t.Setenv("ZERO_FORMAT_ON_WRITE", "1")

	dir := t.TempDir()
	target := filepath.Join(dir, "subject.clobberfmt")
	const written = "the bytes the caller wrote\n"
	if err := os.WriteFile(target, []byte(written), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}

	formatting := maybeFormatWrittenFile(context.Background(), target, written)

	after, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("a timed-out formatter replaced the destination inode")
	}
	onDisk, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != written {
		t.Errorf("destination = %q, want the untouched bytes %q", onDisk, written)
	}
	if formatting.Content != written {
		t.Errorf("content = %q, want the fallback bytes that were written", formatting.Content)
	}
	if !formatting.TimedOut {
		t.Error("the deadline path stopped being reported once formatting moved off the destination")
	}
	if notice := formatting.notice("subject.clobberfmt"); !strings.Contains(notice, "not formatted") {
		t.Errorf("notice = %q, want the timeout note", notice)
	}
}

// clobberingFormatterScript truncates the file it is handed and then fails, the
// way an interrupted physical formatter would if it were handed the file.
func clobberingFormatterScript() string {
	if runtime.GOOS == "windows" {
		return "@echo off\r\necho CLOBBERED> %1\r\nexit /b 3\r\n"
	}
	return "#!/bin/sh\necho CLOBBERED > \"$1\"\nexit 3\n"
}

// recordingClobberingFormatterScript is clobberingFormatterScript plus a record
// of the path it was handed, so a test can prove that path is the private
// staging copy and not the destination.
func recordingClobberingFormatterScript() string {
	if runtime.GOOS == "windows" {
		return "@echo off\r\nif defined ZERO_FORMAT_ARG_RECORD echo %1> \"%ZERO_FORMAT_ARG_RECORD%\"\r\necho CLOBBERED> %1\r\nexit /b 3\r\n"
	}
	return "#!/bin/sh\nprintf '%s' \"$1\" > \"$ZERO_FORMAT_ARG_RECORD\"\necho CLOBBERED > \"$1\"\nexit 3\n"
}

// requireFormatterClobbers proves the fixture really does damage the file it is
// handed, on a throwaway copy.
//
// It cannot be checked on the real target: restoration is the behaviour under
// test, so when it works the evidence is gone, and asserting on the target
// afterwards would either pass vacuously or report the opposite of what it saw.
func requireFormatterClobbers(t *testing.T, written string) {
	t.Helper()
	probe := filepath.Join(t.TempDir(), "probe.clobberfmt")
	if err := os.WriteFile(probe, []byte(written), 0o644); err != nil {
		t.Fatal(err)
	}
	binary, err := exec.LookPath("clobberfmt" + formatterScriptExtension())
	if err != nil {
		t.Fatalf("SETUP INVALID: the fake formatter is not on PATH: %v", err)
	}
	_ = exec.Command(binary, probe).Run()
	after, err := os.ReadFile(probe)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "CLOBBERED") {
		t.Fatalf("SETUP INVALID: the fake formatter left %q, so it does not damage the file and restoration is not under test", after)
	}
}
