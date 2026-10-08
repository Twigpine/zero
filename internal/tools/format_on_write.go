package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/sandbox"
)

// Format-on-write for the mutating file tools. When enabled, a successful
// edit_file/write_file runs the language's standard formatter on the file it
// just wrote, so the model's output always lands in project-canonical style
// and never fails a CI format check it cannot see. Off by default (set
// ZERO_FORMAT_ON_WRITE=1): auto-reformatting changes bytes the model did not
// write, which strict workflows may not want.
//
// Ordering matters: formatting runs BEFORE the FileTracker re-baseline, and
// the caller records the POST-format content. Formatting after the baseline
// would make the very next edit look like an external modification and trip
// the conflict guard.

// formatOnWriteTimeout bounds one formatter run; a wedged formatter must never
// hang a tool call. On timeout the unformatted write stands, and the caller
// says so: see formatOnWriteResult.
//
// A var rather than a const so a test can shorten it. Nothing outside a test
// assigns to it, and the deadline path was previously unreachable in a test at
// any speed, which is part of why it went unnoticed that it reported nothing.
var formatOnWriteTimeout = 10 * time.Second

// formatOnWriteResult is the content after formatting, plus whether the
// formatting that was supposed to happen actually did.
//
// A TIMEOUT IS NOT THE SAME KIND OF MISS AS THE OTHERS. Every other way this
// falls back is a standing fact about the environment: the toggle is off, the
// extension has no formatter, the binary is not installed. Those are silent on
// purpose, because nothing is wrong and saying so on every write would be
// noise. A deadline firing is different: formatting was configured, available
// and expected, and the file was written unformatted anyway, on a machine that
// was merely slow. Left silent, the caller believes it wrote canonical style
// and finds out from a CI format check it cannot see, which is the thing this
// feature exists to prevent.
type formatOnWriteResult struct {
	Content      string
	ContentKnown bool
	Info         os.FileInfo
	// Formatter is the binary that was run, named in the notice so the user can
	// tell a slow gofmt from a slow prettier.
	Formatter string
	TimedOut  bool
	// RestoreFailed means the file on disk is not known to hold Content.
	//
	// These formatters edit in place, so one that is killed or fails partway
	// can leave the target truncated or half-rewritten: what a dead
	// `prettier --write` leaves behind is not the input and not the output.
	// Returning the written bytes while disk holds something else would put the
	// tracker baseline, the diff preview and the file itself into three
	// different states, so the failure paths write the bytes back. When even
	// that fails the user has to hear about it: it is their file.
	RestoreFailed bool
}

// notice is the line appended to the tool summary when formatting was expected
// and did not happen, and empty in every other case.
func (result formatOnWriteResult) notice(relativePath string) string {
	if result.RestoreFailed {
		return "\n\nWARNING: " + relativePath + " may not hold what was written. " +
			result.Formatter + " was interrupted while rewriting it in place and the " +
			"content could not be written back. Re-read the file before trusting it."
	}
	if !result.TimedOut {
		return ""
	}
	return "\n\nNote: " + relativePath + " was written but not formatted: " +
		result.Formatter + " did not finish within " + formatOnWriteTimeout.String() +
		". The file holds exactly what was written, so a project format check may still flag it."
}

// formatterCommands maps a file extension to the formatter argv; the file path
// is appended as the final argument. Only in-place, config-respecting,
// community-standard formatters — a missing binary silently skips formatting.
var formatterCommands = map[string][]string{
	".go":    {"gofmt", "-w"},
	".rs":    {"rustfmt"},
	".py":    {"ruff", "format", "--quiet"},
	".ts":    {"prettier", "--log-level", "silent", "--write"},
	".tsx":   {"prettier", "--log-level", "silent", "--write"},
	".js":    {"prettier", "--log-level", "silent", "--write"},
	".jsx":   {"prettier", "--log-level", "silent", "--write"},
	".json":  {"prettier", "--log-level", "silent", "--write"},
	".css":   {"prettier", "--log-level", "silent", "--write"},
	".scss":  {"prettier", "--log-level", "silent", "--write"},
	".html":  {"prettier", "--log-level", "silent", "--write"},
	".md":    {"prettier", "--log-level", "silent", "--write"},
	".yaml":  {"prettier", "--log-level", "silent", "--write"},
	".yml":   {"prettier", "--log-level", "silent", "--write"},
	".zig":   {"zig", "fmt"},
	".dart":  {"dart", "format"},
	".tf":    {"terraform", "fmt"},
	".gleam": {"gleam", "format"},
	".sh":    {"shfmt", "-w"},
	".bash":  {"shfmt", "-w"},
	".c":     {"clang-format", "-i"},
	".h":     {"clang-format", "-i"},
	".cpp":   {"clang-format", "-i"},
	".hpp":   {"clang-format", "-i"},
	".cc":    {"clang-format", "-i"},
	".kt":    {"ktlint", "-F"},
	".swift": {"swiftformat"},
	".lua":   {"stylua"},
}

// formatOnWriteEnabled reports whether the opt-in env toggle is set.
func formatOnWriteEnabled() bool {
	value := strings.TrimSpace(os.Getenv("ZERO_FORMAT_ON_WRITE"))
	return value != "" && value != "0" && !strings.EqualFold(value, "false")
}

var runFormatOnWriteCommand = func(ctx context.Context, binaryPath string, arguments []string, directory string) error {
	formatter := exec.CommandContext(ctx, binaryPath, arguments...)
	formatter.Dir = directory
	formatter.Stdin = strings.NewReader("")
	return formatter.Run()
}

var readFormattedFile = readRootedFile

// maybeFormatWrittenFile is the unscoped test-facing wrapper. Production
// callers use maybeFormatWrittenFileScoped so a formatter cannot redirect the
// final read or recovery write outside the configured write roots.
func maybeFormatWrittenFile(ctx context.Context, absolutePath string, writtenContent string) formatOnWriteResult {
	restoreMode := os.FileMode(0o644)
	if info, err := os.Stat(absolutePath); err == nil {
		restoreMode = info.Mode().Perm()
	}
	return maybeFormatWrittenFileScoped(ctx, filepath.Dir(absolutePath), nil, absolutePath, writtenContent, restoreMode)
}

// maybeFormatWrittenFileScoped runs the configured formatter and returns only
// content verified through a descriptor-bound root. Formatter failures restore
// writtenContent through that same root; if restoration or the final read
// fails, ContentKnown is false and callers omit exact diff evidence.
func maybeFormatWrittenFileScoped(ctx context.Context, workspaceRoot string, scope PathScope, absolutePath string, writtenContent string, restoreMode os.FileMode) formatOnWriteResult {
	unformatted := formatOnWriteResult{Content: writtenContent, ContentKnown: true}
	if !formatOnWriteEnabled() {
		return unformatted
	}
	command, ok := formatterCommands[strings.ToLower(filepath.Ext(absolutePath))]
	if !ok {
		return unformatted
	}
	binaryPath, err := exec.LookPath(command[0])
	if err != nil {
		return unformatted
	}
	root, relativePath, err := openFormattedFileRoot(workspaceRoot, scope, absolutePath)
	if err != nil {
		unformatted.ContentKnown = false
		return unformatted
	}
	defer root.Close()

	formatCtx, cancel := context.WithTimeout(ctx, formatOnWriteTimeout)
	defer cancel()
	arguments := append(append([]string(nil), command[1:]...), absolutePath)
	if err := runFormatOnWriteCommand(formatCtx, binaryPath, arguments, filepath.Dir(absolutePath)); err != nil {
		unformatted.Formatter = command[0]
		if restoreErr := restoreFormattedFile(root, relativePath, writtenContent, restoreMode); restoreErr != nil {
			unformatted.RestoreFailed = true
			unformatted.ContentKnown = false
		} else if restored, info, readErr := readFormattedFile(root, relativePath); readErr != nil {
			unformatted.ContentKnown = false
		} else {
			unformatted.Content = string(restored)
			unformatted.Info = info
		}
		if errors.Is(formatCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			unformatted.TimedOut = true
		}
		return unformatted
	}

	formatted, info, err := readFormattedFile(root, relativePath)
	if err != nil {
		unformatted.ContentKnown = false
		return unformatted
	}
	return formatOnWriteResult{Content: string(formatted), ContentKnown: true, Info: info, Formatter: command[0]}
}

func restoreFormattedFile(root *os.Root, relativePath string, content string, mode os.FileMode) error {
	file, err := root.OpenFile(relativePath, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, mode)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(file, content); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// openFormattedFileRoot opens the write root before the formatter runs and
// computes the target relative to that descriptor-bound root. Atomic in-root
// replacement remains valid; a formatter that swaps the target to an escaping
// symlink is rejected when readFormattedFile opens it through the root.
func openFormattedFileRoot(workspaceRoot string, scope PathScope, absolutePath string) (*os.Root, string, error) {
	roots, err := scopedRoots(workspaceRoot, scope)
	if err != nil {
		return nil, "", err
	}
	var firstErr error
	for _, configuredRoot := range roots {
		resolvedRoot, err := filepath.Abs(configuredRoot)
		if err == nil {
			resolvedRoot, err = filepath.EvalSymlinks(resolvedRoot)
		}
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		candidate := sandbox.NormalizePrefixForRoot(absolutePath, resolvedRoot)
		relativePath, err := filepath.Rel(resolvedRoot, candidate)
		if err != nil || relativePath == ".." || strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) || filepath.IsAbs(relativePath) {
			continue
		}
		root, err := os.OpenRoot(resolvedRoot)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		return root, relativePath, nil
	}
	if firstErr != nil {
		return nil, "", firstErr
	}
	return nil, "", fmt.Errorf("%s must stay inside the configured write roots", absolutePath)
}
