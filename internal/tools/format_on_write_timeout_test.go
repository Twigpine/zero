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

// A FAILED FORMATTER MUST NOT LEAVE THE FILE HALF-REWRITTEN.
//
// These commands edit in place, so one killed by the deadline, killed by the
// caller, or exiting partway through its own rewrite can leave the target
// truncated: neither the input nor the output. Returning the written bytes on
// top of that would leave the tracker baseline and the diff preview describing
// a file that is not on disk, and the next edit would compare against content
// the file does not have.
func TestFormatOnWriteRestoresTheFileWhenTheFormatterFails(t *testing.T) {
	installFakeFormatter(t, ".clobberfmt", "clobberfmt", clobberingFormatterScript())
	t.Setenv("ZERO_FORMAT_ON_WRITE", "1")

	target := filepath.Join(t.TempDir(), "subject.clobberfmt")
	const written = "the bytes the caller wrote\n"
	if err := os.WriteFile(target, []byte(written), 0o644); err != nil {
		t.Fatal(err)
	}
	requireFormatterClobbers(t, written)

	formatting := maybeFormatWrittenFile(context.Background(), target, written)

	onDisk, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != written {
		t.Errorf("file on disk = %q, want the bytes that were written back", onDisk)
	}
	if formatting.Content != written {
		t.Errorf("content = %q, want the bytes that were written", formatting.Content)
	}
	if formatting.RestoreFailed {
		t.Error("restoration was reported as failed on a writable file")
	}
}

// And the same for a run cut off by the deadline, which is the case the notice
// already covers: the disclosure and the file have to agree.
func TestFormatOnWriteRestoresTheFileOnTimeout(t *testing.T) {
	installFakeFormatter(t, ".clobberfmt", "clobberfmt", clobberingFormatterScript())
	shortenFormatOnWriteTimeout(t, time.Nanosecond)
	t.Setenv("ZERO_FORMAT_ON_WRITE", "1")

	target := filepath.Join(t.TempDir(), "subject.clobberfmt")
	const written = "the bytes the caller wrote\n"
	if err := os.WriteFile(target, []byte(written), 0o644); err != nil {
		t.Fatal(err)
	}
	requireFormatterClobbers(t, written)

	formatting := maybeFormatWrittenFile(context.Background(), target, written)

	onDisk, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != written {
		t.Errorf("file on disk = %q, want the bytes that were written back", onDisk)
	}
	if !formatting.TimedOut {
		t.Error("the deadline path stopped being reported once restoration was added")
	}
	if notice := formatting.notice("subject.clobberfmt"); !strings.Contains(notice, "not formatted") {
		t.Errorf("notice = %q, want the timeout note", notice)
	}
}

// clobberingFormatterScript truncates the file it is handed and then fails, the
// way an interrupted in-place formatter leaves a partial rewrite.
func clobberingFormatterScript() string {
	if runtime.GOOS == "windows" {
		return "@echo off\r\necho CLOBBERED> %1\r\nexit /b 3\r\n"
	}
	return "#!/bin/sh\necho CLOBBERED > \"$1\"\nexit 3\n"
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
