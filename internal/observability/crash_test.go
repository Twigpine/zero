package observability

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestWriteAndFormatCrashReport(t *testing.T) {
	dir := t.TempDir()
	ts := time.Date(2026, 6, 8, 10, 30, 0, 0, time.UTC)
	path, err := WriteCrashReport(dir, "cli", "boom", []byte("goroutine 1 [running]:\nmain.x()"), ts)
	if err != nil {
		t.Fatalf("WriteCrashReport: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	report := string(data)
	for _, want := range []string{"boom", "cli", "2026-06-08T10:30:00Z", "goroutine 1"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q:\n%s", want, report)
		}
	}
}

func TestWriteCrashReportKeepsTwoReportsFromTheSameSecond(t *testing.T) {
	dir := t.TempDir()
	ts := time.Date(2026, 8, 30, 18, 0, 0, 0, time.UTC)
	first, err := WriteCrashReport(dir, "cli", "first panic", []byte("first stack"), ts)
	if err != nil {
		t.Fatal(err)
	}
	second, err := WriteCrashReport(dir, "cli", "second panic", []byte("second stack"), ts)
	if err != nil {
		t.Fatalf("second report in the same second: %v", err)
	}
	if first == second {
		t.Fatalf("same-second reports shared path %q", first)
	}
	firstData, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	secondData, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(firstData), "first panic") || strings.Contains(string(firstData), "second panic") {
		t.Fatalf("first report was replaced: %q", firstData)
	}
	if !strings.Contains(string(secondData), "second panic") {
		t.Fatalf("second report was not persisted: %q", secondData)
	}
}

func TestWriteCrashReportCreatesPrivateDefaultDirectories(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	dir := DefaultCrashDir()
	if dir != filepath.Join(home, ".zero", "crashes") {
		t.Fatalf("DefaultCrashDir = %q, want path beneath temporary home", dir)
	}
	if _, err := WriteCrashReport(dir, "cli", "boom", []byte("stack"), time.Now()); err != nil {
		t.Fatalf("WriteCrashReport: %v", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	for _, path := range []string{filepath.Join(home, ".zero"), dir} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got&0o077 != 0 {
			t.Fatalf("directory %s permissions = %04o, want owner-only", path, got)
		}
	}
}

func TestWriteCrashReportHardensPreexistingDefaultDirectories(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows DACL migration is covered by the daemon integration test")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := DefaultCrashDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(home, ".zero"), dir} {
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := WriteCrashReport(dir, "cli", "boom", []byte("stack"), time.Now()); err != nil {
		t.Fatalf("WriteCrashReport: %v", err)
	}
	for _, path := range []string{filepath.Join(home, ".zero"), dir} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got&0o077 != 0 {
			t.Fatalf("directory %s permissions = %04o after migration, want owner-only", path, got)
		}
	}
}

func TestWriteCrashReportBindsDirectoryDuringSwap(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "live")
	movedDir := filepath.Join(parent, "moved")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	ts := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	var swapErr error
	path, err := writeCrashReport(dir, "cli", "secret panic", []byte("secret stack"), ts, crashReportHooks{
		beforePublish: func() {
			swapErr = os.Rename(dir, movedDir)
			if swapErr != nil {
				return
			}
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatalf("create substitute crash directory: %v", err)
			}
		},
	})
	if err != nil {
		t.Fatalf("writeCrashReport: %v", err)
	}
	if swapErr != nil {
		if runtime.GOOS != "windows" {
			t.Fatalf("swap crash directory: %v", swapErr)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("report missing after Windows blocked directory swap: %v", err)
		}
		return
	}

	if path != "" {
		t.Fatalf("writeCrashReport returned stale path %q after directory swap", path)
	}
	reportName := "crash-" + ts.UTC().Format("20060102-150405") + ".log"
	data, err := os.ReadFile(filepath.Join(movedDir, reportName))
	if err != nil {
		t.Fatalf("read report through originally bound directory: %v", err)
	}
	if !strings.Contains(string(data), "secret panic") {
		t.Fatalf("report in bound directory missing panic: %q", data)
	}
	if _, err := os.Stat(filepath.Join(dir, reportName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("substitute directory received crash report: %v", err)
	}
}

func TestWriteCrashReportPublishesOnlyCompleteContent(t *testing.T) {
	dir := t.TempDir()
	ts := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	finalPath := filepath.Join(dir, "crash-20260824-120000.log")
	staged := make(chan struct{})
	release := make(chan struct{})
	result := make(chan crashWriteResult, 1)
	go func() {
		path, err := writeCrashReport(dir, "cli", "boom", []byte("complete stack"), ts, crashReportHooks{
			beforePublish: func() {
				close(staged)
				<-release
			},
		})
		result <- crashWriteResult{path: path, err: err}
	}()
	select {
	case <-staged:
	case written := <-result:
		t.Fatalf("writeCrashReport returned before publication hook: %v", written.err)
	case <-time.After(time.Second):
		t.Fatal("writeCrashReport did not reach publication hook")
	}
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	if _, err := os.Stat(finalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("final report visible before complete publication: %v", err)
	}
	close(release)
	written := <-result
	if written.err != nil {
		t.Fatalf("writeCrashReport: %v", written.err)
	}
	data, err := os.ReadFile(written.path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "complete stack") {
		t.Fatalf("published report is incomplete: %q", data)
	}
}

func TestWriteCrashReportRemovesTempAfterWriteFailure(t *testing.T) {
	dir := t.TempDir()
	injected := errors.New("injected write failure")
	_, err := writeCrashReport(dir, "cli", "boom", []byte("stack"), time.Now(), crashReportHooks{
		write: func(*os.File, []byte) (int, error) { return 0, injected },
	})
	if !errors.Is(err, injected) {
		t.Fatalf("writeCrashReport error = %v, want injected failure", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("crash directory contains partial files after failure: %v", entries)
	}
}

func TestWriteCrashReportDoesNotOverwriteExistingReport(t *testing.T) {
	dir := t.TempDir()
	ts := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(dir, "crash-20260824-120000.log")
	if err := os.WriteFile(path, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	published, err := WriteCrashReport(dir, "cli", "boom", []byte("stack"), ts)
	if err != nil {
		t.Fatalf("WriteCrashReport with existing timestamp: %v", err)
	}
	if published == path {
		t.Fatal("new report reused the occupied timestamp path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "existing" {
		t.Fatalf("existing report overwritten: %q", data)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("crash directory should contain existing and uniquely published reports: %v", entries)
	}
}

func TestWriteCrashReportPreservesPathAfterCommittedCleanupWarning(t *testing.T) {
	dir := t.TempDir()
	ts := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	injected := errors.New("injected cleanup failure")
	path, err := writeCrashReport(dir, "cli", "boom", []byte("complete stack"), ts, crashReportHooks{
		remove: func(*os.Root, string) error { return injected },
	})
	if !errors.Is(err, ErrCrashReportCommitted) || !errors.Is(err, injected) {
		t.Fatalf("writeCrashReport error = %v, want committed cleanup warning", err)
	}
	wantPath := filepath.Join(dir, "crash-20260824-120000.log")
	if path != wantPath {
		t.Fatalf("writeCrashReport path = %q, want %q", path, wantPath)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read committed crash report: %v", readErr)
	}
	if !strings.Contains(string(data), "complete stack") {
		t.Fatalf("committed crash report is incomplete: %q", data)
	}
}

func TestWriteCrashReportDoesNotReturnStalePathWithCleanupWarning(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "live")
	movedDir := filepath.Join(parent, "moved")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected cleanup failure")
	ts := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	var swapErr error
	path, err := writeCrashReport(dir, "cli", "boom", []byte("complete stack"), ts, crashReportHooks{
		beforePublish: func() {
			swapErr = os.Rename(dir, movedDir)
			if swapErr == nil {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatalf("create substitute crash directory: %v", err)
				}
			}
		},
		remove: func(*os.Root, string) error { return injected },
	})
	if swapErr != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("Windows kept the open crash directory from being renamed: %v", swapErr)
		}
		t.Fatalf("swap crash directory: %v", swapErr)
	}
	if !errors.Is(err, ErrCrashReportCommitted) || !errors.Is(err, injected) {
		t.Fatalf("writeCrashReport error = %v, want committed cleanup warning", err)
	}
	if path != "" {
		t.Fatalf("writeCrashReport returned stale path %q after directory swap", path)
	}
	report := filepath.Join(movedDir, "crash-20260824-120000.log")
	data, readErr := os.ReadFile(report)
	if readErr != nil {
		t.Fatalf("read committed report through moved directory: %v", readErr)
	}
	if !strings.Contains(string(data), "complete stack") {
		t.Fatalf("committed report is incomplete: %q", data)
	}
}

func TestWriteCrashReportFallsBackWhenHardLinksAreUnavailable(t *testing.T) {
	dir := t.TempDir()
	ts := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	unsupported := errors.New("hard links unavailable")
	var collisionName string
	path, err := writeCrashReport(dir, "cli", "boom", []byte("complete stack"), ts, crashReportHooks{
		link: func(root *os.Root, oldname, _ string) error {
			suffix := strings.TrimPrefix(oldname, crashTempPrefix)
			collisionName = "crash-20260824-120000-" + suffix + ".log"
			if err := root.WriteFile(collisionName, []byte("existing"), 0o600); err != nil {
				t.Fatalf("create fallback-name collision: %v", err)
			}
			return unsupported
		},
	})
	if err != nil {
		t.Fatalf("writeCrashReport fallback: %v", err)
	}
	if path == filepath.Join(dir, collisionName) {
		t.Fatalf("fallback replaced the existing report %q", collisionName)
	}
	if !strings.HasPrefix(filepath.Base(path), "crash-20260824-120000-") {
		t.Fatalf("fallback path = %q, want timestamp and random suffix", path)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read fallback crash report: %v", readErr)
	}
	if !strings.Contains(string(data), "complete stack") {
		t.Fatalf("fallback crash report is incomplete: %q", data)
	}
	existing, readErr := os.ReadFile(filepath.Join(dir, collisionName))
	if readErr != nil {
		t.Fatalf("read existing collision: %v", readErr)
	}
	if string(existing) != "existing" {
		t.Fatalf("existing fallback collision was overwritten: %q", existing)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 2 {
		t.Fatalf("crash directory contains partial files after fallback: %v", entries)
	}
}

func TestWriteCrashReportFallbackDoesNotReplaceCommitTimeCollision(t *testing.T) {
	dir := t.TempDir()
	ts := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	unsupported := errors.New("hard links unavailable")
	var collisionName string
	attempts := 0
	path, err := writeCrashReport(dir, "cli", "boom", []byte("complete stack"), ts, crashReportHooks{
		link: func(*os.Root, string, string) error { return unsupported },
		renameNoReplace: func(root *os.Root, oldname, newname string) error {
			attempts++
			if attempts == 1 {
				collisionName = newname
				if err := root.WriteFile(newname, []byte("concurrent report"), 0o600); err != nil {
					t.Fatalf("create commit-time collision: %v", err)
				}
			}
			return renameNoReplace(root, oldname, newname)
		},
	})
	if err != nil {
		t.Fatalf("writeCrashReport fallback: %v", err)
	}
	if attempts < 2 {
		t.Fatalf("fallback rename attempts = %d, want collision retry", attempts)
	}
	if filepath.Base(path) == collisionName {
		t.Fatalf("fallback replaced the concurrent report %q", collisionName)
	}
	existing, err := os.ReadFile(filepath.Join(dir, collisionName))
	if err != nil {
		t.Fatal(err)
	}
	if string(existing) != "concurrent report" {
		t.Fatalf("concurrent report overwritten: %q", existing)
	}
	published, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(published), "complete stack") {
		t.Fatalf("published fallback is incomplete: %q", published)
	}
}

type crashWriteResult struct {
	path string
	err  error
}

func TestRecoverCapturesPanic(t *testing.T) {
	dir := t.TempDir()
	var stderr bytes.Buffer
	code := 0

	func() {
		defer Recover(dir, "test", &stderr, &code)
		panic("kaboom")
	}()

	if code != crashExitCode {
		t.Fatalf("exit code = %d, want %d", code, crashExitCode)
	}
	if !strings.Contains(stderr.String(), "zero crashed") || !strings.Contains(stderr.String(), "kaboom") {
		t.Fatalf("missing crash notice: %q", stderr.String())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("expected one crash report written, got %d", len(entries))
	}
}

func TestRecoverNoPanicIsNoop(t *testing.T) {
	dir := t.TempDir()
	var stderr bytes.Buffer
	code := 7
	func() {
		defer Recover(dir, "test", &stderr, &code)
	}()
	if code != 7 {
		t.Fatalf("code changed without a panic: %d", code)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected output without a panic: %q", stderr.String())
	}
}

func TestReportRecoveredCrashReportsCommittedCleanupWarning(t *testing.T) {
	var stderr bytes.Buffer
	code := 0
	injected := errors.New("injected cleanup failure")
	reportRecoveredCrash(&stderr, &code, "kaboom", []byte("secret stack"), func() (string, error) {
		return "/tmp/crash.log", &crashReportCommittedError{cause: injected}
	})

	if code != crashExitCode {
		t.Fatalf("exit code = %d, want %d", code, crashExitCode)
	}
	output := stderr.String()
	for _, want := range []string{"kaboom", "saved to /tmp/crash.log", "Warning:", injected.Error()} {
		if !strings.Contains(output, want) {
			t.Fatalf("crash notice missing %q: %q", want, output)
		}
	}
	if strings.Contains(output, "secret stack") {
		t.Fatalf("committed report warning fell back to inline stack: %q", output)
	}
}
