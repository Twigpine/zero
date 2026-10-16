package tools

import (
	"context"
	"os"
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

	formatting := runTestFormatter(t, context.Background(), target, written)

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

	formatting := runTestFormatter(t, context.Background(), target, written)
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
	formatting := runTestFormatter(t, ctx, target, written)

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

	formatting := runTestFormatter(t, context.Background(), target, written)
	if formatting.TimedOut {
		t.Error("a formatter that exited non-zero was reported as a timeout")
	}
	if notice := formatting.notice("subject.failfmt"); notice != "" {
		t.Errorf("a failing formatter produced a timeout notice: %q", notice)
	}
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
	formatterCommands[extension] = []string{binaryName}
	t.Cleanup(func() {
		if existed {
			formatterCommands[extension] = previous
			return
		}
		delete(formatterCommands, extension)
	})
	return binaryName
}

func runTestFormatter(t *testing.T, ctx context.Context, target, written string) formatOnWriteResult {
	t.Helper()
	root, err := os.OpenRoot(filepath.Dir(target))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	return maybeFormatWrittenFile(ctx, root, filepath.Base(target), target, filepath.Dir(target), nil, written, 0o644)
}

func formatterScriptExtension() string {
	if runtime.GOOS == "windows" {
		return ".bat"
	}
	return ""
}

func succeedingFormatterScript() string {
	if runtime.GOOS == "windows" {
		return "@echo off\r\nmore\r\nexit /b 0\r\n"
	}
	return "#!/bin/sh\ncat\n"
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

// A formatter that fails after emitting partial stdout must not alter the file.
func TestFormatOnWriteDoesNotPublishOutputFromFailedFormatter(t *testing.T) {
	installFakeFormatter(t, ".clobberfmt", "clobberfmt", partialOutputFormatterScript())
	t.Setenv("ZERO_FORMAT_ON_WRITE", "1")

	target := filepath.Join(t.TempDir(), "subject.clobberfmt")
	const written = "the bytes the caller wrote\n"
	if err := os.WriteFile(target, []byte(written), 0o644); err != nil {
		t.Fatal(err)
	}
	formatting := runTestFormatter(t, context.Background(), target, written)
	onDisk, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != written || formatting.Content != written {
		t.Errorf("failed formatter changed content: disk=%q result=%q", onDisk, formatting.Content)
	}
}

func partialOutputFormatterScript() string {
	if runtime.GOOS == "windows" {
		return "@echo off\r\necho CLOBBERED\r\nexit /b 3\r\n"
	}
	return "#!/bin/sh\necho CLOBBERED\nexit 3\n"
}

// A formatter that declines a file with empty stdout and exit 0 must leave the
// written bytes alone. clang-format answers that way over stdin for a path its
// .clang-format-ignore matches; publishing the silence emptied the file while
// write_file reported success.
func TestFormatOnWriteKeepsContentWhenTheFormatterPrintsNothing(t *testing.T) {
	installFakeFormatter(t, ".silentfmt", "silentfmt", silentFormatterScript())
	t.Setenv("ZERO_FORMAT_ON_WRITE", "1")
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const written = "int lib(void) { return 0; }\n"
	for _, step := range []struct {
		name      string
		overwrite bool
	}{{"create", false}, {"overwrite", true}} {
		t.Run(step.name, func(t *testing.T) {
			result := NewScopedWriteFileTool(dir, nil).Run(context.Background(), map[string]any{
				"path": "lib.silentfmt", "content": written, "overwrite": step.overwrite,
			})
			if result.Status != StatusOK {
				t.Fatalf("write failed: %q", result.Output)
			}
			onDisk, err := os.ReadFile(filepath.Join(dir, "lib.silentfmt"))
			if err != nil {
				t.Fatal(err)
			}
			if string(onDisk) != written {
				t.Fatalf("silent formatter replaced the file: disk=%q, want %q", onDisk, written)
			}
		})
	}
}

// The deadline must bound the call even when the formatter is a shim whose
// child keeps the stdout pipe open after the shim is killed. Every
// npm-installed formatter on Windows is a .cmd shim hosting node.exe.
func TestFormatOnWriteDeadlineBoundsAShimmedFormatter(t *testing.T) {
	installFakeFormatter(t, ".shimfmt", "shimfmt", shimmedSlowFormatterScript())
	shortenFormatOnWriteTimeout(t, 500*time.Millisecond)
	t.Setenv("ZERO_FORMAT_ON_WRITE", "1")

	target := filepath.Join(t.TempDir(), "subject.shimfmt")
	const written = "written\n"
	if err := os.WriteFile(target, []byte(written), 0o644); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	formatting := runTestFormatter(t, context.Background(), target, written)
	elapsed := time.Since(started)

	// The grandchild sleeps nine seconds. The deadline plus bashWaitDelay is
	// well under five; waiting on the orphaned pipe is not.
	if elapsed > 5*time.Second {
		t.Fatalf("formatter call took %s; the deadline did not bound it", elapsed)
	}
	if !formatting.TimedOut || formatting.Content != written {
		t.Fatalf("result = %+v, want the written content reported as a timeout", formatting)
	}
}

func silentFormatterScript() string {
	if runtime.GOOS == "windows" {
		return "@echo off\r\nmore >nul\r\nexit /b 0\r\n"
	}
	return "#!/bin/sh\ncat >/dev/null\nexit 0\n"
}

// The sleeping child inherits stdout, as node.exe does under a .cmd shim.
func shimmedSlowFormatterScript() string {
	if runtime.GOOS == "windows" {
		return "@echo off\r\nping -n 10 127.0.0.1\r\nexit /b 0\r\n"
	}
	return "#!/bin/sh\nsleep 9\nexit 0\n"
}
