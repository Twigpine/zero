package tools

import (
	"context"
	"errors"
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
// A TIMEOUT IS NOT THE SAME KIND OF MISS AS THE OTHERS. The toggle being off,
// an extension with no formatter, or a binary that is not installed are
// standing facts about the environment. Those stay silent, because nothing is
// wrong and saying so on every write would be noise. A deadline firing is
// different: formatting was configured, available and expected, and the file
// was written unformatted anyway, on a machine that was merely slow. Left
// silent, the caller believes it wrote canonical style and finds out from a CI
// format check it cannot see, which is the thing this feature exists to
// prevent. A formatter binary that resolves inside a write root is also
// reported: skipping it is a security decision, and a silent skip looks like
// format-on-write simply stopped working.
type formatOnWriteResult struct {
	Content      string
	ContentKnown bool
	Info         os.FileInfo
	// Formatter is the binary that was run, named in the notice so the user can
	// tell a slow gofmt from a slow prettier.
	Formatter string
	TimedOut  bool
	// Skipped is set when a formatter was found but not run. Empty when
	// formatting was not applicable, ran, or failed for another recorded reason.
	Skipped string
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
// and did not happen (timeout, restore failure, or a refused workspace binary),
// and empty in every other case.
func (result formatOnWriteResult) notice(relativePath string) string {
	if result.RestoreFailed {
		return "\n\nWARNING: " + relativePath + " may not hold what was written. " +
			result.Formatter + " was interrupted while rewriting it in place and the " +
			"content could not be written back. Re-read the file before trusting it."
	}
	if result.TimedOut {
		return "\n\nNote: " + relativePath + " was written but not formatted: " +
			result.Formatter + " did not finish within " + formatOnWriteTimeout.String() +
			". The file holds exactly what was written, so a project format check may still flag it."
	}
	if result.Skipped != "" {
		return "\n\nNote: " + relativePath + " was written but not formatted: " + result.Skipped + "."
	}
	return ""
}

// formatSkippedInsideWriteRoot is the formatOnWriteResult.Skipped value when
// the resolved formatter binary lives under a granted write root.
const formatSkippedInsideWriteRoot = "skipped, binary resolves inside the workspace"

// formatterCommands maps a file extension to the formatter argv; the file path
// is appended as the final argument. Only in-place, community-standard
// formatters — a missing binary silently skips formatting.
//
// PRETTIER IS INVOKED WITH --no-config ON PURPOSE. Prettier resolves config
// relative to the target file. Its JavaScript configs (.prettierrc.js,
// .prettierrc.cjs, prettier.config.js) are MODULES: loading one runs arbitrary
// code from the workspace, and a malicious dependency pulled by a plugin runs
// too. A declarative config can still name a plugin by path, so it is not a
// safe subset. That code would execute with Zero's full privileges, outside the
// Landlock/seccomp confinement applied to shell commands.
// Prettier therefore runs on its built-in defaults, and project
// configuration is not honored: .prettierrc, .prettierrc.json, .prettierrc.yaml,
// prettier.config.js, the prettier key in package.json, and .editorconfig are all
// ignored by --no-config. The remaining formatters keep honoring their own
// non-executable project config.
var formatterCommands = map[string][]string{
	".go":    {"gofmt", "-w"},
	".rs":    {"rustfmt"},
	".py":    {"ruff", "format", "--quiet"},
	".ts":    {"prettier", "--log-level", "silent", "--no-config", "--write"},
	".tsx":   {"prettier", "--log-level", "silent", "--no-config", "--write"},
	".js":    {"prettier", "--log-level", "silent", "--no-config", "--write"},
	".jsx":   {"prettier", "--log-level", "silent", "--no-config", "--write"},
	".json":  {"prettier", "--log-level", "silent", "--no-config", "--write"},
	".css":   {"prettier", "--log-level", "silent", "--no-config", "--write"},
	".scss":  {"prettier", "--log-level", "silent", "--no-config", "--write"},
	".html":  {"prettier", "--log-level", "silent", "--no-config", "--write"},
	".md":    {"prettier", "--log-level", "silent", "--no-config", "--write"},
	".yaml":  {"prettier", "--log-level", "silent", "--no-config", "--write"},
	".yml":   {"prettier", "--log-level", "silent", "--no-config", "--write"},
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
	// The formatter may evaluate project files (plugins, configs) as code. Do
	// not hand it Zero's credential-bearing environment: scrub the same keys the
	// sandbox strips from shell commands while preserving PATH, HOME, and the
	// platform variables a formatter needs to start.
	formatter.Env = sandbox.ScrubSensitiveEnv(os.Environ())
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
	// A binary that resolves inside a write root is repository-controlled: npm
	// run puts node_modules/.bin on PATH, so a cloned project can shadow
	// prettier (or gofmt) with its own executable and have it run with Zero's
	// privileges. Skipping formatting is the safe fallback; running an
	// untrusted formatter is never worth canonical bytes. The skip is recorded
	// on the result: a silent return looks like format-on-write stopped working.
	if roots, rootsErr := scopedRoots(workspaceRoot, scope); rootsErr == nil && formatterBinaryInWriteRoots(binaryPath, roots) {
		unformatted.Skipped = formatSkippedInsideWriteRoot
		return unformatted
	}
	root, relativePath, err := openScopedWriteRoot(workspaceRoot, scope, absolutePath)
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

// formatterBinaryInWriteRoots reports whether the resolved formatter binary
// lives under one of the configured write roots (the workspace or an /add-dir
// root). Both sides are symlink-resolved before comparison so an aliased or
// linked path cannot hide a workspace-planted binary. A binary that cannot be
// resolved is treated as untrusted, because trust cannot be established.
func formatterBinaryInWriteRoots(binaryPath string, roots []string) bool {
	resolvedBinary, err := filepath.Abs(binaryPath)
	if err != nil {
		return false
	}
	if evaluatedBinary, err := filepath.EvalSymlinks(resolvedBinary); err == nil {
		resolvedBinary = evaluatedBinary
	}
	for _, root := range roots {
		resolvedRoot, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		if evaluated, err := filepath.EvalSymlinks(resolvedRoot); err == nil {
			resolvedRoot = evaluated
		}
		relative, err := filepath.Rel(resolvedRoot, resolvedBinary)
		if err != nil {
			continue
		}
		if relative == "." {
			return true
		}
		if relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative) {
			return true
		}
	}
	return false
}
