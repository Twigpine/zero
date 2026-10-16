package tools

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/fsutil"
	"github.com/Gitlawb/zero/internal/sandbox"
)

// Format-on-write for the mutating file tools. When enabled, a successful
// edit_file/write_file formats staged content before the single atomic
// publication, so the model's output always lands in project-canonical style
// and never fails a CI format check it cannot see. Off by default (set
// ZERO_FORMAT_ON_WRITE=1): auto-reformatting changes bytes the model did not
// write, which strict workflows may not want.
//
// Ordering matters: formatting runs on an isolated temporary copy BEFORE
// publication and BEFORE the FileTracker re-baseline. The caller records the
// POST-format content that was actually published. Formatting the destination
// in place after publication would reintroduce partial-file writes.

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
	Content string
	// Formatter is the binary that was run, named in the notice so the user can
	// tell a slow gofmt from a slow prettier.
	Formatter string
	TimedOut  bool
	// Skipped is set when a formatter was found but not run. Empty when
	// formatting was not applicable, ran, or failed for another recorded reason.
	Skipped string
}

// notice is the line appended to the tool summary when formatting was expected
// and did not happen (timeout or a refused workspace binary), and empty in
// every other case.
func (result formatOnWriteResult) notice(relativePath string) string {
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

// formatterAdapter describes how one formatter is run.
//
// STDIN ADAPTERS READ THE WRITTEN BYTES ON STDIN AND PRINT THE FORMATTED RESULT
// TO STDOUT. That route lets filenameFlag carry the logical destination path,
// so a rule written against the file's own path is aimed at the name the
// caller used instead of a random staging name. Aiming the rule at that name
// does not make a path exclusion apply to a file that does not exist yet; see
// formatterCommands. Physical adapters have no such option: they are handed a
// private staging copy inside an owner-only sibling directory (including
// auxiliary files) and keep the destination directory as their working
// directory.
type formatterAdapter struct {
	argv []string
	// stdin selects the stdin/stdout route. When false the private staging path
	// is appended to argv.
	stdin bool
	// filenameFlag is the option that supplies the logical destination to a
	// stdin adapter. It is emitted as "--flag=<target>" and empty when the
	// formatter resolves its configuration from the working directory alone.
	filenameFlag string
	// stdinArg is appended after filenameFlag, and must not also be stored in
	// argv: a "-" kept in argv would be emitted before the filename flag.
	// For Ruff, "-" serves as an explicit, defensive marker ensuring input is read
	// from standard input when invoked as `ruff format --stdin-filename=<path> -`.
	// Empty for formatters that read stdin when given no path.
	stdinArg string
}

// formatterCommands maps a file extension to its formatter. A missing binary
// silently skips formatting.
//
// PRETTIER IS INVOKED WITH --no-config ON PURPOSE. Prettier resolves config
// relative to the target file. Its JavaScript configs (.prettierrc.js,
// .prettierrc.cjs, prettier.config.js) are MODULES: loading one runs arbitrary
// code from the workspace, and a malicious dependency pulled by a plugin runs
// too. A declarative config can still name a plugin by path, so it is not a
// safe subset. That code would execute with Zero's full privileges, outside the
// Landlock/seccomp confinement applied to shell commands.
//
// PATH EXCLUSIONS DO NOT APPLY ON THE FIRST WRITE OF A NEW FILE. The stdin
// adapters pass the logical destination, but a formatter that consults its
// ignore file only when that path already exists (clang-format and
// .clang-format-ignore) formats a not-yet-created file even when the path
// matches an exclusion. An overwrite still finds the previous file at that
// path, so the same rule can apply. TestFormatOnWritePathIgnoreDoesNotApplyToNewFiles
// pins this split; it is the contract, not an oversight.
var formatterCommands = map[string]formatterAdapter{
	".go":    {argv: []string{"gofmt"}, stdin: true},
	".rs":    {argv: []string{"rustfmt"}, stdin: true},
	".py":    {argv: []string{"ruff", "format", "--quiet"}, stdin: true, filenameFlag: "--stdin-filename", stdinArg: "-"},
	".ts":    {argv: []string{"prettier", "--log-level", "silent", "--no-config"}, stdin: true, filenameFlag: "--stdin-filepath"},
	".tsx":   {argv: []string{"prettier", "--log-level", "silent", "--no-config"}, stdin: true, filenameFlag: "--stdin-filepath"},
	".js":    {argv: []string{"prettier", "--log-level", "silent", "--no-config"}, stdin: true, filenameFlag: "--stdin-filepath"},
	".jsx":   {argv: []string{"prettier", "--log-level", "silent", "--no-config"}, stdin: true, filenameFlag: "--stdin-filepath"},
	".json":  {argv: []string{"prettier", "--log-level", "silent", "--no-config"}, stdin: true, filenameFlag: "--stdin-filepath"},
	".css":   {argv: []string{"prettier", "--log-level", "silent", "--no-config"}, stdin: true, filenameFlag: "--stdin-filepath"},
	".scss":  {argv: []string{"prettier", "--log-level", "silent", "--no-config"}, stdin: true, filenameFlag: "--stdin-filepath"},
	".html":  {argv: []string{"prettier", "--log-level", "silent", "--no-config"}, stdin: true, filenameFlag: "--stdin-filepath"},
	".md":    {argv: []string{"prettier", "--log-level", "silent", "--no-config"}, stdin: true, filenameFlag: "--stdin-filepath"},
	".yaml":  {argv: []string{"prettier", "--log-level", "silent", "--no-config"}, stdin: true, filenameFlag: "--stdin-filepath"},
	".yml":   {argv: []string{"prettier", "--log-level", "silent", "--no-config"}, stdin: true, filenameFlag: "--stdin-filepath"},
	".zig":   {argv: []string{"zig", "fmt", "--stdin"}, stdin: true},
	".dart":  {argv: []string{"dart", "format"}, stdin: true, filenameFlag: "--stdin-name"},
	".tf":    {argv: []string{"terraform", "fmt", "-"}, stdin: true},
	".gleam": {argv: []string{"gleam", "format", "--stdin"}, stdin: true},
	".sh":    {argv: []string{"shfmt"}, stdin: true, filenameFlag: "--filename"},
	".bash":  {argv: []string{"shfmt"}, stdin: true, filenameFlag: "--filename"},
	".c":     {argv: []string{"clang-format"}, stdin: true, filenameFlag: "--assume-filename"},
	".h":     {argv: []string{"clang-format"}, stdin: true, filenameFlag: "--assume-filename"},
	".cpp":   {argv: []string{"clang-format"}, stdin: true, filenameFlag: "--assume-filename"},
	".hpp":   {argv: []string{"clang-format"}, stdin: true, filenameFlag: "--assume-filename"},
	".cc":    {argv: []string{"clang-format"}, stdin: true, filenameFlag: "--assume-filename"},
	".kt":    {argv: []string{"ktlint", "--log-level=none", "-F", "--stdin"}, stdin: true, filenameFlag: "--stdin-path"},
	".swift": {argv: []string{"swiftformat"}, stdin: true, filenameFlag: "--stdinpath"},
	".lua":   {argv: []string{"stylua", "-"}, stdin: true, filenameFlag: "--stdin-filepath"},
}

// formatterCommandObserver, when non-nil, receives each formatter command after
// its lifetime bounds are applied. Tests use it to assert that a wedged
// formatter can be killed and cannot block Wait past the deadline.
var formatterCommandObserver func(*exec.Cmd)

// formatOnWriteEnabled reports whether the opt-in env toggle is set.
func formatOnWriteEnabled() bool {
	value := strings.TrimSpace(os.Getenv("ZERO_FORMAT_ON_WRITE"))
	return value != "" && value != "0" && !strings.EqualFold(value, "false")
}

// maybeFormatWrittenFile is the unscoped test-facing wrapper. Production
// callers use maybeFormatWrittenFileScoped so the formatter cannot operate
// outside the configured write roots.
func maybeFormatWrittenFile(ctx context.Context, absolutePath string, writtenContent string) formatOnWriteResult {
	return maybeFormatWrittenFileScoped(ctx, filepath.Dir(absolutePath), nil, absolutePath, writtenContent)
}

// maybeFormatWrittenFileScoped runs the configured formatter over a transient
// copy of the written bytes (stdin, or a private staging file) and returns the
// bytes to publish. The destination is never opened or rewritten here. If
// absolutePath does not resolve inside one of the scope's write roots,
// formatting is refused and the written bytes pass through unchanged.
func maybeFormatWrittenFileScoped(ctx context.Context, workspaceRoot string, scope PathScope, absolutePath string, writtenContent string) formatOnWriteResult {
	unformatted := formatOnWriteResult{Content: writtenContent}
	if !formatOnWriteEnabled() {
		return unformatted
	}
	adapter, ok := formatterCommands[strings.ToLower(filepath.Ext(absolutePath))]
	if !ok {
		return unformatted
	}
	binaryPath, err := exec.LookPath(adapter.argv[0])
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
		return unformatted
	}
	defer root.Close()
	workDir := filepath.Join(root.Name(), filepath.Dir(relativePath))
	if adapter.stdin {
		return formatWithStdin(ctx, adapter, binaryPath, absolutePath, writtenContent, workDir)
	}
	return formatWithStaging(ctx, adapter, binaryPath, absolutePath, writtenContent, workDir)
}

// formatterBinaryInWriteRoots reports whether the resolved formatter binary
// lives under one of the configured write roots (the workspace or an /add-dir
// root). Both sides are symlink-resolved before comparison so an aliased or
// linked path cannot hide a workspace-planted binary.
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

// formatWithStaging runs a physical formatter on a private copy of the written
// bytes, never on the destination. The copy keeps the destination's basename
// so filename-derived formatter behaviour still applies, and any scribble from
// a killed or failing run lands there, not in the user's file.
func formatWithStaging(ctx context.Context, adapter formatterAdapter, binaryPath, absolutePath, writtenContent, dir string) formatOnWriteResult {
	unformatted := formatOnWriteResult{Content: writtenContent}
	stagingDir, err := fsutil.CreatePrivateTempDir(dir, ".zero-fmt-*")
	if err != nil {
		return unformatted
	}
	defer func() { _ = os.RemoveAll(stagingDir) }()
	staging, err := os.OpenFile(filepath.Join(stagingDir, filepath.Base(absolutePath)), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return unformatted
	}
	stagingName := staging.Name()
	defer func() { _ = os.Remove(stagingName) }()
	if _, err := staging.WriteString(writtenContent); err != nil {
		_ = staging.Close()
		return unformatted
	}
	if err := staging.Close(); err != nil {
		return unformatted
	}
	formatCtx, cancel := context.WithTimeout(ctx, formatOnWriteTimeout)
	defer cancel()
	arguments := append(append([]string(nil), adapter.argv[1:]...), stagingName)
	formatter := exec.CommandContext(formatCtx, binaryPath, arguments...)
	formatter.Dir = dir
	formatter.Stdin = strings.NewReader("")
	formatter.Env = sandbox.ScrubSensitiveEnv(os.Environ())
	hardenProcessLifetime(formatter)
	if formatterCommandObserver != nil {
		formatterCommandObserver(formatter)
	}
	if err := formatter.Run(); err != nil {
		unformatted.Formatter = adapter.argv[0]
		if errors.Is(formatCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			unformatted.TimedOut = true
		}
		return unformatted
	}
	formatted, err := os.ReadFile(stagingName)
	if err != nil {
		return unformatted
	}
	return formatOnWriteResult{Content: string(formatted), Formatter: adapter.argv[0]}
}

// formatWithStdin runs a formatter over stdin, supplies the logical
// destination through the adapter's filename flag when it has one, and reads
// the formatted bytes from stdout. The empty-output guard keeps a formatter
// that ignores the path (and so prints nothing) from publishing an empty file.
func formatWithStdin(ctx context.Context, adapter formatterAdapter, binaryPath, absolutePath, writtenContent, dir string) formatOnWriteResult {
	unformatted := formatOnWriteResult{Content: writtenContent}
	formatCtx, cancel := context.WithTimeout(ctx, formatOnWriteTimeout)
	defer cancel()
	arguments := append([]string(nil), adapter.argv[1:]...)
	if adapter.filenameFlag != "" {
		arguments = append(arguments, adapter.filenameFlag+"="+absolutePath)
	}
	if adapter.stdinArg != "" {
		arguments = append(arguments, adapter.stdinArg)
	}
	formatter := exec.CommandContext(formatCtx, binaryPath, arguments...)
	formatter.Dir = dir
	formatter.Stdin = strings.NewReader(writtenContent)
	formatter.Env = sandbox.ScrubSensitiveEnv(os.Environ())
	hardenProcessLifetime(formatter)
	if formatterCommandObserver != nil {
		formatterCommandObserver(formatter)
	}
	var stdout bytes.Buffer
	formatter.Stdout = &stdout
	if err := formatter.Run(); err != nil {
		unformatted.Formatter = adapter.argv[0]
		if errors.Is(formatCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			unformatted.TimedOut = true
		}
		return unformatted
	}
	if stdout.Len() == 0 && writtenContent != "" {
		// A formatter that produced nothing for non-empty input (an ignored
		// path on some CLI versions) must not publish an empty file.
		return unformatted
	}
	return formatOnWriteResult{Content: stdout.String(), Formatter: adapter.argv[0]}
}
