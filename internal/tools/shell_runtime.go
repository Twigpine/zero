package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"

	zeroSandbox "github.com/Gitlawb/zero/internal/sandbox"
)

type shellKind string

const (
	shellKindPOSIX      shellKind = "posix"
	shellKindPowerShell shellKind = "powershell"
	shellKindCmd        shellKind = "cmd"
)

type shellRuntime struct {
	GOOS       string
	Executable string
	Syntax     string
	Kind       shellKind
}

type shellIssue struct {
	Kind       string
	Message    string
	Suggestion string
}

const windowsMsysSandboxKind = "windows_msys_sandbox"

const windowsMsysSandboxSuggestion = "MSYS/Cygwin executables and shells (bash, sh) from Git for Windows cannot run under Zero's write-restricted Windows sandbox, and the WSL bash launcher cannot reach the WSL service from it either. This also hits native commands that spawn Git Bash internally: git hooks and git/gh credential helpers can fail this way even though git and gh themselves run fine. Prefer native PowerShell cmdlets or Zero tools (grep, read_file with offset/limit, list_directory, glob). If host-level execution is truly required, rerun with sandbox_permissions: \"require_escalated\" and a narrow justification."

const windowsPowerShellUTF8Prefix = "try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch {}\n"

const windowsPowerShellFailurePrefix = "$ErrorActionPreference = 'Stop'\n"

const windowsPowerShellExitSuffix = "\nif ($null -ne $LASTEXITCODE) { exit $LASTEXITCODE }\n"

var (
	hostShellOnce sync.Once
	hostShell     shellRuntime
)

// windowsMsysProneNames is the single source of truth for POSIX coreutil and
// shell names that commonly resolve to a Git-for-Windows MSYS/Cygwin binary
// rather than a cmd.exe-native command, and so fail under the write-restricted
// Windows sandbox (#458). Every Windows MSYS-detection path (the preflight
// command scan below, the exported MsysProneCommandName, and the
// known-safe-segment guard in internal/agent/command_prefix.go) derives from
// this one set, so they cannot drift out of sync with each other.
var windowsMsysProneNames = map[string]bool{
	"cat": true, "cut": true, "expr": true, "grep": true, "head": true,
	"id": true, "ls": true, "nl": true, "paste": true, "rev": true,
	"seq": true, "stat": true, "tail": true, "tr": true, "uname": true,
	"uniq": true, "wc": true, "which": true, "awk": true, "sed": true,
	"xargs": true,
	// Shells. Git-for-Windows bash.exe/sh.exe are MSYS binaries and die during
	// MSYS runtime init under the restricted token ("couldn't create signal
	// pipe" or "CreateFileMapping <SID>.1", both Win32 error 5), and the
	// System32 WSL bash launcher fails equivalently at a different layer (the
	// restricted token cannot connect to the WSL service:
	// Bash/Service/CreateInstance/E_ACCESSDENIED), so every executable a bare
	// `bash` can resolve to fails under the sandbox.
	"bash": true, "sh": true,
}

var (
	windowsBashStyleCDPattern = regexp.MustCompile(`(?i)(^|[&|;]\s*)cd\s+/(?:[a-ce-z0-9_./~-]|d[a-z0-9_./~-])[a-z0-9_./~-]*`)
	// windowsMsysBinaryPathPattern catches explicit Git-for-Windows / MSYS usr\bin
	// paths. These executables are valid Windows PE files but fail under the
	// write-restricted sandbox with CreateFileMapping ACCESS_DENIED (#458).
	windowsMsysBinaryPathPattern = regexp.MustCompile(`(?i)(?:\\usr\\bin\\|\\mingw64\\bin\\|msys-2\.0\.dll|cygwin1\.dll)`)
)

func detectShellRuntime(goos string) shellRuntime {
	if goos != runtime.GOOS {
		return detectShellRuntimeWithLookup(goos, exec.LookPath, os.Getenv)
	}
	hostShellOnce.Do(func() {
		hostShell = detectShellRuntimeWithProbe(goos, exec.LookPath, os.Getenv, powerShellExecutableUsable)
	})
	return hostShell
}

func detectShellRuntimeWithLookup(goos string, lookPath func(string) (string, error), getenv func(string) string) shellRuntime {
	return detectShellRuntimeWithProbe(goos, lookPath, getenv, func(string) bool { return true })
}

func detectShellRuntimeWithProbe(goos string, lookPath func(string) (string, error), getenv func(string) string, usable func(string) bool) shellRuntime {
	if goos == "windows" {
		for _, candidate := range windowsPowerShellCandidates(getenv) {
			if path, err := lookPath(candidate); err == nil && strings.TrimSpace(path) != "" && usable(path) {
				return shellRuntime{
					GOOS:       goos,
					Executable: path,
					Syntax:     "PowerShell",
					Kind:       shellKindPowerShell,
				}
			}
		}
		return shellRuntime{GOOS: goos, Executable: "cmd.exe", Syntax: "Windows cmd.exe", Kind: shellKindCmd}
	}
	return shellRuntime{GOOS: goos, Executable: "/bin/sh", Syntax: "/bin/sh", Kind: shellKindPOSIX}
}

func powerShellExecutableUsable(path string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, path, "-NoLogo", "-NoProfile", "-Command", "exit 0")
	return command.Run() == nil
}

func windowsPowerShellCandidates(getenv func(string) string) []string {
	candidates := []string{"pwsh.exe", "pwsh"}
	if programFiles := strings.TrimSpace(getenv("ProgramFiles")); programFiles != "" {
		candidates = append(candidates, filepath.Join(programFiles, "PowerShell", "7", "pwsh.exe"))
	}
	candidates = append(candidates, "powershell.exe", "powershell")
	if systemRoot := strings.TrimSpace(getenv("SystemRoot")); systemRoot != "" {
		candidates = append(candidates, filepath.Join(systemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe"))
	}
	return candidates
}

func (shell shellRuntime) arguments(command string) []string {
	switch shell.Kind {
	case shellKindPowerShell:
		script := windowsPowerShellUTF8Prefix + windowsPowerShellFailurePrefix + command + windowsPowerShellExitSuffix
		return []string{"-NoLogo", "-NoProfile", "-Command", script}
	case shellKindCmd:
		return zeroSandbox.WindowsShellArgs(command)
	default:
		return []string{"-c", command}
	}
}

// HostShellCommand returns the shell executable and argv used for a command on
// this host. Keeping this in one place makes agent tools and the user-typed TUI
// shell escape use the same Windows shell and fallback behavior.
func HostShellCommand(command string) (string, []string) {
	shell := detectShellRuntime(runtime.GOOS)
	return shell.Executable, shell.arguments(command)
}

// NewHostShellCommandContext constructs a direct host-shell command. The
// cmd.exe fallback needs a Windows-specific raw command line, while PowerShell
// and POSIX shells use normal argv handling.
func NewHostShellCommandContext(ctx context.Context, commandText string) *exec.Cmd {
	shell := detectShellRuntime(runtime.GOOS)
	command := exec.CommandContext(ctx, shell.Executable, shell.arguments(commandText)...)
	applyWindowsShellCommandLine(command, commandText, false, shell.Kind == shellKindCmd)
	return command
}

func shellGuidanceForGOOS(goos string) string {
	return shellGuidanceForRuntime(detectShellRuntime(goos))
}

func shellGuidanceForRuntime(shell shellRuntime) string {
	if shell.GOOS == "windows" && shell.Kind == shellKindPowerShell {
		guidance := "Uses PowerShell syntax on Windows; prefer cwd/workdir over Set-Location when changing directories. Examples: Get-ChildItem -Force; Get-ChildItem -Recurse -Filter *.go; Get-ChildItem -Recurse | Select-String -Pattern 'TODO'; Get-Process | Where-Object { $_.ProcessName -like '*node*' }; $env:NAME='value'; @'\nprint('hello')\n'@ | python -. Do not invoke Git-for-Windows MSYS/Cygwin executables (bash, sh, grep.exe, sed.exe, head.exe, and similar) inside the restricted sandbox; prefer PowerShell cmdlets or native Zero tools."
		return guidance + legacyPowerShellChainGuidance(shell)
	}
	if shell.GOOS == "windows" {
		return "Uses " + shell.Syntax + " syntax on Windows because PowerShell was unavailable; prefer cwd/workdir over cd when changing directories. To include | & > < or other metacharacters in an argument value, wrap the value in double quotes (e.g. --jq \".a | b\"); single quotes do not protect metacharacters in cmd.exe. MSYS/Cygwin coreutils on PATH (Git for Windows usr\\bin) are not sandbox-compatible; prefer native Zero file tools."
	}
	guidance := "Uses " + shell.Syntax + " syntax."
	if shell.GOOS == "darwin" {
		guidance += " To find or stop a process, use `lsof -i :PORT` (or `lsof -nP -iTCP -sTCP:LISTEN`) for the PID then `kill <pid>`; `ps` and `pgrep` do not work under the sandbox."
	}
	return guidance
}

// HostExecCommandShellGuidance returns the shell guidance appended to
// exec_command's description on this host, or "" where none is appended.
//
// Exported for the per-turn token ratchet in internal/agent. That budget has to
// separate what every host pays for the tool schemas from what THIS host pays
// for its shell guidance, because the guidance varies by shell and by
// PowerShell version, and a single number covering both silently means a
// different thing on each machine.
func HostExecCommandShellGuidance() string {
	return hostExecCommandShellGuidance(runtimeGOOS())
}

// hostExecCommandShellGuidance is the guidance a host running goos appends. It
// is the pair of execCommandDescription, so the two can be checked against each
// other for every platform rather than only for the running one.
func hostExecCommandShellGuidance(goos string) string {
	if goos != "windows" {
		return ""
	}
	return shellGuidanceForGOOS(goos)
}

// HostShellEnvironmentGuidance returns the concise, model-facing shell rule
// for the current host.
func HostShellEnvironmentGuidance() string {
	return hostShellEnvironmentGuidanceForRuntime(detectShellRuntime(runtime.GOOS))
}

func hostShellEnvironmentGuidanceForRuntime(shell shellRuntime) string {
	if shell.GOOS == "windows" && shell.Kind == shellKindPowerShell {
		guidance := "Shell syntax: PowerShell for exec_command/bash tools. Use PowerShell cmdlets and pipelines (Get-ChildItem, Get-Content, Select-String, Select-Object), use $env:NAME='value' for environment variables, and prefer the workdir/cwd argument over Set-Location. Do not invoke Git-for-Windows MSYS binaries (bash, sh, grep.exe, sed.exe, head.exe, and similar) inside the restricted sandbox; use native PowerShell cmdlets or Zero tools instead, or sandbox_permissions require_escalated only when host-level execution is truly required."
		return guidance + legacyPowerShellChainGuidance(shell)
	}
	if shell.GOOS == "windows" {
		return "Shell syntax: Windows cmd.exe syntax for exec_command/bash tools because PowerShell is unavailable. To put | & > < etc inside an arg value, use double quotes around the value, not single quotes. Do not invoke Git-for-Windows MSYS binaries inside the restricted sandbox; use native Zero tools instead. Prefer the workdir/cwd argument over cd."
	}
	return "Shell syntax: /bin/sh syntax for exec_command/bash tools; prefer the workdir/cwd argument instead of cd when changing directories."
}

func legacyPowerShellChainGuidance(shell shellRuntime) string {
	if !shell.isWindowsPowerShell() {
		return ""
	}
	return " This host uses Windows PowerShell 5.1, which does not support && or ||. Use separate statements when execution is unconditional, or if ($?) { ... } and if (-not $?) { ... } for conditional chaining."
}

func (shell shellRuntime) isWindowsPowerShell() bool {
	if shell.Kind != shellKindPowerShell {
		return false
	}
	executable := strings.TrimSpace(shell.Executable)
	if index := strings.LastIndexAny(executable, `\/`); index >= 0 {
		executable = executable[index+1:]
	}
	return strings.EqualFold(executable, "powershell") || strings.EqualFold(executable, "powershell.exe")
}

// MsysProneCommandName reports whether a bare command name commonly resolves to
// a Git-for-Windows MSYS binary that fails under the Windows restricted sandbox.
func MsysProneCommandName(name string) bool {
	return windowsMsysProneNames[strings.ToLower(strings.TrimSpace(name))]
}

func windowsMsysSandboxIssue(message string) *shellIssue {
	return &shellIssue{
		Kind:       windowsMsysSandboxKind,
		Message:    message,
		Suggestion: windowsMsysSandboxSuggestion,
	}
}

// windowsCommandSegments splits a command into cmd.exe-operator-separated
// segments (&, |, and their doubled forms &&/||), respecting double quotes
// (cmd.exe's grouping construct) and the caret (^) escape character, so an
// operator or command name mentioned inside a quoted argument (e.g. a commit
// message or PR comment body), or an operator escaped with ^ (cmd.exe prints
// `echo ^| head` as literal text instead of piping to head), is not mistaken
// for a real segment boundary or invocation. Unlike bash, cmd.exe does not
// treat ; as a statement separator, so it is left as ordinary argument text
// (e.g. `echo foo; head` is a single `echo` invocation with literal args).
func windowsCommandSegments(command string) []string {
	var segments []string
	var current strings.Builder
	inQuotes := false
	runes := []rune(command)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		if !inQuotes && c == '^' && i+1 < len(runes) {
			current.WriteRune(c)
			i++
			current.WriteRune(runes[i])
			continue
		}
		if c == '"' {
			inQuotes = !inQuotes
			current.WriteRune(c)
			continue
		}
		if !inQuotes && (c == '&' || c == '|') {
			if seg := strings.TrimSpace(current.String()); seg != "" {
				segments = append(segments, seg)
			}
			current.Reset()
			continue
		}
		current.WriteRune(c)
	}
	if seg := strings.TrimSpace(current.String()); seg != "" {
		segments = append(segments, seg)
	}
	return segments
}

// firstCommandWord returns the first token of a command segment. A leading
// double-quoted span counts as one token with its quotes stripped, since
// cmd.exe treats a quoted path as a single argument: the command invoked by
// `"C:\Program Files\Git\usr\bin\head.exe" file.txt` is the quoted path, not
// "C:\Program. For an unquoted command, the token ends at whitespace or a
// redirection operator (<, >): cmd.exe accepts redirection attached directly
// to the command name with no separating space (head>out.txt, cat<in.txt), so
// stopping only at whitespace would return "head>out.txt" as one word and
// miss the invoked command.
func firstCommandWord(segment string) string {
	trimmed := strings.TrimSpace(segment)
	if trimmed == "" {
		return ""
	}
	if trimmed[0] == '"' {
		if end := strings.IndexByte(trimmed[1:], '"'); end >= 0 {
			return trimmed[1 : end+1]
		}
		return trimmed[1:]
	}
	if end := strings.IndexFunc(trimmed, isCommandWordBoundary); end >= 0 {
		return trimmed[:end]
	}
	return trimmed
}

func isCommandWordBoundary(r rune) bool {
	return unicode.IsSpace(r) || r == '<' || r == '>'
}

// msysProneCommandWord reports whether word (the first token of a command
// segment, as returned by firstCommandWord) names an MSYS-prone coreutil,
// bare or with a directory prefix and/or .exe suffix (head, head.exe,
// C:\...\usr\bin\head.exe, ...).
func msysProneCommandWord(word string) bool {
	word = strings.Trim(word, `"`)
	if i := strings.LastIndexAny(word, `\/`); i >= 0 {
		word = word[i+1:]
	}
	word = strings.TrimSuffix(strings.ToLower(word), ".exe")
	return MsysProneCommandName(word)
}

func detectShellCommandIssue(command string, goos string) *shellIssue {
	shell := shellRuntime{GOOS: goos, Executable: "/bin/sh", Syntax: "/bin/sh", Kind: shellKindPOSIX}
	if goos == "windows" {
		shell = shellRuntime{GOOS: goos, Executable: "cmd.exe", Syntax: "Windows cmd.exe", Kind: shellKindCmd}
	}
	return detectShellCommandIssueForRuntime(command, shell)
}

func detectShellCommandIssueForRuntime(command string, shell shellRuntime) *shellIssue {
	if shell.GOOS != "windows" {
		return nil
	}
	trimmed := strings.TrimSpace(command)
	// Blank out double-quoted spans before matching, so a `cd /foo`-shaped
	// string that only appears inside a quoted argument value (e.g. a `gh`
	// or `git commit` message) is not mistaken for an actual command cmd.exe
	// would interpret. Then neutralize cmd.exe caret escapes of its own
	// metacharacters (foo^|cat is the literal token foo|cat, not a pipe into
	// cat), so an escaped metachar can't stand in as a fake segment boundary
	// either.
	unquoted := stripCmdCaretEscapes(stripDoubleQuotedSpans(trimmed))
	if shell.Kind == shellKindPowerShell {
		unquoted = stripPowerShellQuotedSpans(trimmed)
		if shell.isWindowsPowerShell() && (strings.Contains(unquoted, "&&") || strings.Contains(unquoted, "||")) {
			return &shellIssue{
				Kind:       "windows_powershell_version",
				Message:    "Command uses && or ||, but this host runs Windows PowerShell 5.1, which does not support those operators.",
				Suggestion: "Use separate statements when execution is unconditional, or if ($?) { ... } and if (-not $?) { ... } for conditional chaining.",
			}
		}
	}
	if windowsBashStyleCDPattern.MatchString(unquoted) {
		if shell.Kind == shellKindPowerShell {
			return &shellIssue{
				Kind:       "windows_shell_syntax",
				Message:    "Command looks like POSIX/Bash syntax, but Zero runs PowerShell commands on this Windows host.",
				Suggestion: "Use the cwd/workdir argument instead of cd and use native PowerShell syntax or Zero tools such as list_directory, read_file, grep, and glob.",
			}
		}
		return &shellIssue{
			Kind:       "windows_shell_syntax",
			Message:    "Command looks like POSIX/Bash syntax, but Zero runs bash tool commands through Windows cmd.exe on this host.",
			Suggestion: "Use the cwd argument instead of cd, use Windows cmd.exe syntax, or use native tools such as list_directory, read_file, grep, and glob.",
		}
	}
	// Check the first word of each operator-separated segment (not the raw
	// text anywhere in the command) against the MSYS binary-path pattern and
	// the single MSYS-prone name set, covering bare names (head), .exe names
	// (head.exe), and directory-prefixed forms (C:\...\head.exe) uniformly.
	// Being segment/word anchored rather than a whole-string regex or scan,
	// neither check matches text that only appears inside a quoted argument
	// (e.g. a commit message or PR comment body discussing head.exe).
	segments := windowsCommandSegments(trimmed)
	if shell.Kind == shellKindPowerShell {
		segments = powerShellCommandSegments(trimmed)
	}
	for _, segment := range segments {
		word := firstCommandWord(segment)
		if windowsMsysBinaryPathPattern.MatchString(word) {
			return windowsMsysSandboxIssue("Command invokes an MSYS/Cygwin binary path that cannot run under Zero's Windows sandbox.")
		}
		if msysProneCommandWord(word) {
			if shell.Kind == shellKindPowerShell && powerShellAliasWord(word) {
				continue
			}
			return windowsMsysSandboxIssue("Command uses a POSIX coreutil (head/tail/grep/cat/...) that commonly resolves to Git-for-Windows MSYS binaries incompatible with the Windows sandbox.")
		}
	}
	return nil
}

// powerShellCommandSegments splits the subset of PowerShell syntax needed for
// command-position checks. It respects single/double quoted strings and the
// backtick escape, then splits on pipeline/statement operators. Complex script
// interpretation remains PowerShell's job; this scanner only determines
// whether a known-incompatible MSYS executable is actually invoked.
func powerShellCommandSegments(command string) []string {
	var segments []string
	var current strings.Builder
	var quote rune
	escaped := false
	for _, c := range command {
		if escaped {
			current.WriteRune(c)
			escaped = false
			continue
		}
		if c == '`' {
			current.WriteRune(c)
			escaped = true
			continue
		}
		if quote != 0 {
			current.WriteRune(c)
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			current.WriteRune(c)
			continue
		}
		if c == ';' || c == '|' || c == '&' {
			if segment := strings.TrimSpace(current.String()); segment != "" {
				segments = append(segments, segment)
			}
			current.Reset()
			continue
		}
		current.WriteRune(c)
	}
	if segment := strings.TrimSpace(current.String()); segment != "" {
		segments = append(segments, segment)
	}
	return segments
}

func powerShellAliasWord(word string) bool {
	trimmed := strings.Trim(word, `"'`)
	if strings.ContainsAny(trimmed, `\/`) || strings.HasSuffix(strings.ToLower(trimmed), ".exe") {
		return false
	}
	switch strings.ToLower(trimmed) {
	case "cat", "ls":
		return true
	default:
		return false
	}
}

func stripPowerShellQuotedSpans(command string) string {
	var b strings.Builder
	b.Grow(len(command))
	var quote rune
	escaped := false
	runes := []rune(command)
	for index := 0; index < len(runes); index++ {
		c := runes[index]
		if escaped {
			b.WriteRune(' ')
			escaped = false
			continue
		}
		if c == '`' {
			b.WriteRune(' ')
			escaped = true
			continue
		}
		if quote != 0 {
			b.WriteRune(' ')
			if c == quote {
				// PowerShell escapes a single quote inside a single-quoted
				// string by doubling it.
				if quote == '\'' && index+1 < len(runes) && runes[index+1] == '\'' {
					index++
					b.WriteRune(' ')
					continue
				}
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			b.WriteRune(' ')
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

// stripDoubleQuotedSpans replaces the contents of every double-quoted span
// (quotes included) with spaces, preserving the string's length and the
// position of unquoted text. cmd.exe treats a double-quoted span as a single
// literal token, so operators/utility names inside one are not real command
// syntax; blanking them out keeps the Windows command-issue regexes anchored
// to text cmd.exe would actually interpret.
func stripDoubleQuotedSpans(command string) string {
	var b strings.Builder
	b.Grow(len(command))
	inQuotes := false
	for _, c := range command {
		if c == '"' {
			inQuotes = !inQuotes
			b.WriteByte(' ')
			continue
		}
		if inQuotes {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

// cmdEscapedMetacharPattern matches a cmd.exe caret escape of one of its own
// metacharacters. cmd.exe treats the escaped character as a literal, not an
// operator, so e.g. `foo^|cat` is the single literal token foo|cat, not a
// pipe into cat.
var cmdEscapedMetacharPattern = regexp.MustCompile(`\^[&|;^<>]`)

// stripCmdCaretEscapes blanks out cmd.exe caret-escape sequences (both
// characters), so an escaped metacharacter cannot be mistaken by the Windows
// command-issue regexes for a real operator/segment boundary.
func stripCmdCaretEscapes(command string) string {
	return cmdEscapedMetacharPattern.ReplaceAllString(command, "  ")
}

// detectShellOutputIssue looks for MSYS runtime crash markers and cmd.exe
// syntax-error text in output only, never in the command that was run. The
// command line is attacker/user-controlled argument text (e.g. a `gh pr
// comment --body` quoting a sample failure), not something the shell
// produced, so treating it as evidence would reintroduce the same
// quoted-text false positives the preflight command-position check exists to
// avoid, just after execution instead of before it.
func detectShellOutputIssue(output string, goos string) *shellIssue {
	shell := shellRuntime{GOOS: goos, Executable: "/bin/sh", Syntax: "/bin/sh", Kind: shellKindPOSIX}
	if goos == "windows" {
		shell = shellRuntime{GOOS: goos, Executable: "cmd.exe", Syntax: "Windows cmd.exe", Kind: shellKindCmd}
	}
	return detectShellOutputIssueForRuntime(output, shell)
}

func detectShellOutputIssueForRuntime(output string, shell shellRuntime) *shellIssue {
	if shell.GOOS != "windows" {
		return nil
	}
	lower := strings.ToLower(output)
	if msysRuntimeFailedInOutput(lower) {
		return windowsMsysSandboxIssue("An MSYS/Cygwin runtime failed under Zero's Windows sandbox (ACCESS_DENIED during MSYS startup).")
	}
	if wslServiceDeniedInOutput(lower) {
		return windowsMsysSandboxIssue("WSL bash could not connect to the WSL service under Zero's Windows sandbox (Bash/Service/CreateInstance/E_ACCESSDENIED).")
	}
	if shell.Kind == shellKindCmd && (strings.Contains(lower, "the syntax of the command is incorrect") ||
		strings.Contains(lower, "is not recognized as an internal or external command")) {
		return &shellIssue{
			Kind:       "windows_shell_syntax",
			Message:    "Windows cmd.exe rejected the command syntax.",
			Suggestion: "Use Windows cmd.exe syntax. Quote args with | using double quotes (e.g. --jq \".a | b\"). Avoid | head; use --jq or PowerShell Select-Object -First N instead. Prefer native tools.",
		}
	}
	return nil
}

func msysRuntimeFailedInOutput(lower string) bool {
	if strings.Contains(lower, "fatal error - createfilemapping") {
		return true
	}
	if strings.Contains(lower, "couldn't create signal pipe") && strings.Contains(lower, "win32 error 5") {
		return true
	}
	if strings.Contains(lower, "cygheap_user::init") && strings.Contains(lower, "fatal error") {
		return true
	}
	if strings.Contains(lower, "usr\\bin\\") && strings.Contains(lower, "fatal error") {
		return true
	}
	if !strings.Contains(lower, "win32 error 5") || !strings.Contains(lower, "terminating") {
		return false
	}
	// Anchor the broad win32-error-5 fallback to an MSYS-specific marker so
	// unrelated access-denied failures are not mislabeled as MSYS sandbox
	// incompatibilities.
	return strings.Contains(lower, `usr\bin\`) ||
		strings.Contains(lower, "cygheap") ||
		strings.Contains(lower, "msys-2.0.dll") ||
		strings.Contains(lower, "cygwin1.dll") ||
		strings.Contains(lower, "[main]")
}

// wslServiceDeniedInOutput matches the WSL bash launcher's failure to open its
// service connection under the restricted token. The launcher writes UTF-16LE
// to its (piped, non-console) stderr, which the byte-based capture renders as
// ASCII interleaved with NUL bytes, so the NULs are stripped before matching.
func wslServiceDeniedInOutput(lower string) bool {
	compact := strings.ReplaceAll(lower, "\x00", "")
	return strings.Contains(compact, "bash/service/") && strings.Contains(compact, "e_accessdenied")
}

func appendShellIssueHint(output string, issue shellIssue) string {
	output = strings.TrimRight(output, "\r\n")
	hint := "[zero] shell issue: " + issue.Message
	if strings.TrimSpace(issue.Suggestion) != "" {
		hint += "\nSuggestion: " + issue.Suggestion
	}
	if output == "" {
		return hint
	}
	return output + "\n" + hint
}
