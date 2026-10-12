// Package observability provides dependency-free crash capture for the CLI: a
// recovered panic is written to a local crash report (timestamp, label, stack)
// and surfaced to the user as a brief notice instead of a raw stack trace. It is
// the fail-open foundation for remote crash/metrics reporting — a Sentry/OTEL
// adapter can hook the same Recover/report path when configured — without
// pulling those dependencies into the base build.
package observability

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/privatedir"
)

// crashExitCode is returned when a top-level panic is recovered.
const crashExitCode = 1

const crashTempPrefix = ".crash-report-"

// ErrCrashReportCommitted marks an error that happened after a complete crash
// report was published. Callers may still use the returned path and should
// surface the error as a cleanup warning rather than as a failed publication.
var ErrCrashReportCommitted = errors.New("crash report publication committed")

type crashReportCommittedError struct {
	cause error
}

func (err *crashReportCommittedError) Error() string {
	return fmt.Sprintf("%v with warning: %v", ErrCrashReportCommitted, err.cause)
}

func (err *crashReportCommittedError) Unwrap() []error {
	return []error{ErrCrashReportCommitted, err.cause}
}

type crashReportHooks struct {
	beforePublish   func()
	write           func(*os.File, []byte) (int, error)
	link            func(*os.Root, string, string) error
	renameNoReplace func(*os.Root, string, string) error
	remove          func(*os.Root, string) error
}

// FormatCrashReport renders a human-readable crash report.
func FormatCrashReport(label string, recovered any, stack []byte, ts time.Time) string {
	return fmt.Sprintf("zero crash report\ntime:  %s\nlabel: %s\npanic: %v\n\nstack:\n%s\n",
		ts.UTC().Format(time.RFC3339), label, recovered, stack)
}

// WriteCrashReport writes a crash report file into dir and returns its path.
func WriteCrashReport(dir, label string, recovered any, stack []byte, ts time.Time) (string, error) {
	return writeCrashReport(dir, label, recovered, stack, ts, crashReportHooks{})
}

func writeCrashReport(dir, label string, recovered any, stack []byte, ts time.Time, hooks crashReportHooks) (path string, returnErr error) {
	absoluteDir, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve crash report directory: %w", err)
	}
	root, err := openCrashDirectory(dir)
	if err != nil {
		return "", err
	}
	committed := false
	defer func() {
		if err := root.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close crash report directory: %w", err))
		}
		if committed && returnErr != nil && !errors.Is(returnErr, ErrCrashReportCommitted) {
			returnErr = &crashReportCommittedError{cause: returnErr}
		}
		if !committed && returnErr != nil {
			path = ""
		}
	}()

	name := "crash-" + ts.UTC().Format("20060102-150405") + ".log"
	path = filepath.Join(dir, name)
	report, tempName, err := createCrashTemp(root)
	if err != nil {
		return "", err
	}
	defer func() {
		if tempName == "" {
			return
		}
		if err := removeCrashTemp(hooks, root, tempName); err != nil && !errors.Is(err, os.ErrNotExist) {
			returnErr = errors.Join(returnErr, fmt.Errorf("remove temporary crash report: %w", err))
		}
	}()

	data := []byte(FormatCrashReport(label, recovered, stack, ts))
	writeReport := hooks.write
	if writeReport == nil {
		writeReport = func(file *os.File, data []byte) (int, error) { return file.Write(data) }
	}
	written, err := writeReport(report, data)
	if err == nil && written != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		closeErr := report.Close()
		if closeErr != nil {
			return "", errors.Join(fmt.Errorf("write crash report: %w", err), fmt.Errorf("close crash report: %w", closeErr))
		}
		return "", fmt.Errorf("write crash report: %w", err)
	}
	if err := report.Sync(); err != nil {
		closeErr := report.Close()
		if closeErr != nil {
			return "", errors.Join(fmt.Errorf("sync crash report: %w", err), fmt.Errorf("close crash report: %w", closeErr))
		}
		return "", fmt.Errorf("sync crash report: %w", err)
	}
	if err := report.Close(); err != nil {
		return "", fmt.Errorf("close crash report: %w", err)
	}
	if hooks.beforePublish != nil {
		hooks.beforePublish()
	}
	publishName := name
	link := root.Link
	if hooks.link != nil {
		link = func(oldname, newname string) error { return hooks.link(root, oldname, newname) }
	}
	if linkErr := link(tempName, name); linkErr != nil {
		publishName, err = publishCrashFallback(root, name, tempName, hooks.renameNoReplace)
		if err != nil {
			return "", errors.Join(fmt.Errorf("publish crash report with hard link: %w", linkErr), err)
		}
		committed = true
		tempName = ""
	} else {
		committed = true
	}
	path = filepath.Join(dir, publishName)
	if !crashPathUsesRoot(root, absoluteDir) {
		path = ""
	}
	if tempName != "" {
		if err := removeCrashTemp(hooks, root, tempName); err != nil && !errors.Is(err, os.ErrNotExist) {
			tempName = ""
			return path, fmt.Errorf("remove temporary crash report: %w", err)
		}
		tempName = ""
	}
	return path, nil
}

// publishCrashFallback atomically renames the complete staging file to an
// unpredictable unused name. The no-replace operation is the collision check;
// there is no check-then-rename window in which another report can be replaced.
func publishCrashFallback(root *os.Root, timestampName, tempName string, rename func(*os.Root, string, string) error) (string, error) {
	if rename == nil {
		rename = renameNoReplace
	}
	base := strings.TrimSuffix(timestampName, filepath.Ext(timestampName))
	if suffix := strings.TrimPrefix(tempName, crashTempPrefix); suffix != "" && suffix != tempName {
		candidate := base + "-" + suffix + filepath.Ext(timestampName)
		if err := rename(root, tempName, candidate); err == nil {
			return candidate, nil
		} else if !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("publish fallback crash report: %w", err)
		}
	}
	for range 100 {
		var suffix [16]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return "", fmt.Errorf("generate fallback crash report name: %w", err)
		}
		candidate := base + "-" + hex.EncodeToString(suffix[:]) + filepath.Ext(timestampName)
		if err := rename(root, tempName, candidate); err == nil {
			return candidate, nil
		} else if !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("publish fallback crash report: %w", err)
		}
	}
	return "", fmt.Errorf("generate fallback crash report name: exhausted unique names")
}

func removeCrashTemp(hooks crashReportHooks, root *os.Root, name string) error {
	if hooks.remove != nil {
		return hooks.remove(root, name)
	}
	return root.Remove(name)
}

func createCrashTemp(root *os.Root) (*os.File, string, error) {
	for range 100 {
		var suffix [16]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return nil, "", fmt.Errorf("generate temporary crash report name: %w", err)
		}
		name := crashTempPrefix + hex.EncodeToString(suffix[:])
		file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return file, name, nil
		}
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return nil, "", fmt.Errorf("create temporary crash report: %w", err)
	}
	return nil, "", fmt.Errorf("create temporary crash report: exhausted unique names")
}

func crashPathUsesRoot(root *os.Root, dir string) bool {
	bound, err := root.Stat(".")
	if err != nil {
		return false
	}
	current, err := os.Stat(filepath.Clean(dir))
	return err == nil && os.SameFile(bound, current)
}

func openCrashDirectory(dir string) (*os.Root, error) {
	clean := filepath.Clean(dir)
	defaultDir := filepath.Clean(DefaultCrashDir())
	parent := filepath.Dir(clean)
	// The default layout shares ~/.zero with the daemon runtime fallback. Keep
	// both the shared parent and the crash-report child private. Custom crash
	// destinations are hardened at the caller-supplied boundary only.
	if clean == defaultDir && filepath.Base(clean) == "crashes" && filepath.Base(parent) == ".zero" {
		if err := privatedir.Ensure(parent); err != nil {
			return nil, fmt.Errorf("secure crash report parent: %w", err)
		}
	}
	root, err := privatedir.Open(clean)
	if err != nil {
		return nil, fmt.Errorf("secure crash report directory: %w", err)
	}
	return root, nil
}

// DefaultCrashDir is where crash reports are written by default.
func DefaultCrashDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".zero", "crashes")
	}
	return filepath.Join(os.TempDir(), "zero-crashes")
}

// Recover is deferred at a top-level entrypoint. On a panic it captures the
// stack, writes a crash report under dir, prints a brief notice to stderr, and
// sets *code to a crash exit code. It is fail-open: if the report can't be
// written it still reports the crash with the stack inline. No panic escapes.
func Recover(dir, label string, stderr io.Writer, code *int) {
	recovered := recover()
	if recovered == nil {
		return
	}
	stack := debug.Stack()
	reportRecoveredCrash(stderr, code, recovered, stack, func() (string, error) {
		return WriteCrashReport(dir, label, recovered, stack, time.Now())
	})
}

func reportRecoveredCrash(stderr io.Writer, code *int, recovered any, stack []byte, write func() (string, error)) {
	path, err := write()
	if err == nil || errors.Is(err, ErrCrashReportCommitted) {
		if path != "" {
			fmt.Fprintf(stderr, "zero crashed: %v\nA crash report was saved to %s\n", recovered, path)
		} else {
			fmt.Fprintf(stderr, "zero crashed: %v\nA crash report was saved, but its current path could not be determined\n", recovered)
		}
		if err != nil {
			fmt.Fprintf(stderr, "Warning: %v\n", err)
		}
	} else {
		fmt.Fprintf(stderr, "zero crashed: %v\n%s\n", recovered, stack)
	}
	*code = crashExitCode
}
