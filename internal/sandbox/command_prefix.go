package sandbox

import (
	"strings"
	"sync"
)

type commandPrefixGrantSet struct {
	mu     sync.Mutex
	grants []CommandPrefixGrant
}

type CommandPrefixGrant struct {
	ToolName   string   `json:"toolName"`
	Prefix     []string `json:"prefix"`
	ApprovedAt string   `json:"approvedAt,omitempty"`
	Reason     string   `json:"reason,omitempty"`
	Session    bool     `json:"session,omitempty"`
}

type CommandPrefixInput struct {
	ToolName string
	Prefix   []string
	Reason   string
}

func newCommandPrefixGrantSet() *commandPrefixGrantSet {
	return &commandPrefixGrantSet{}
}

func (set *commandPrefixGrantSet) add(grant CommandPrefixGrant) {
	if set == nil || grant.ToolName == "" || len(grant.Prefix) == 0 {
		return
	}
	set.mu.Lock()
	defer set.mu.Unlock()
	for _, existing := range set.grants {
		if existing.ToolName == grant.ToolName && sameStringSlice(existing.Prefix, grant.Prefix) {
			return
		}
	}
	grant.Prefix = append([]string(nil), grant.Prefix...)
	set.grants = append(set.grants, grant)
}

func (set *commandPrefixGrantSet) match(toolName string, command []string) (CommandPrefixGrant, bool) {
	if set == nil || toolName == "" || len(command) == 0 {
		return CommandPrefixGrant{}, false
	}
	set.mu.Lock()
	defer set.mu.Unlock()
	for _, grant := range set.grants {
		if grant.ToolName == toolName && hasStringPrefix(command, grant.Prefix) {
			grant.Prefix = append([]string(nil), grant.Prefix...)
			return grant, true
		}
	}
	return CommandPrefixGrant{}, false
}

func hasStringPrefix(values []string, prefix []string) bool {
	if len(prefix) == 0 || len(prefix) > len(values) {
		return false
	}
	for index := range prefix {
		if values[index] != prefix[index] {
			return false
		}
	}
	return true
}

func NormalizeCommandPrefix(prefix []string) ([]string, bool) {
	cleaned := make([]string, 0, len(prefix))
	for _, part := range prefix {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, false
		}
		cleaned = append(cleaned, part)
	}
	if unsafeCommandPrefix(cleaned) {
		return nil, false
	}
	return cleaned, true
}

func ValidCommandPrefix(prefix []string) bool {
	_, ok := NormalizeCommandPrefix(prefix)
	return ok
}

var bannedCommandPrefixSuggestions = [][]string{
	{"python3"},
	{"python3", "-"},
	{"python3", "-c"},
	{"python"},
	{"python", "-"},
	{"python", "-c"},
	{"py"},
	{"py", "-3"},
	{"pythonw"},
	{"pyw"},
	{"pypy"},
	{"pypy3"},
	{"git"},
	{"bash"},
	{"bash", "-lc"},
	{"sh"},
	{"sh", "-c"},
	{"sh", "-lc"},
	{"zsh"},
	{"zsh", "-lc"},
	{"/bin/zsh"},
	{"/bin/zsh", "-lc"},
	{"/bin/bash"},
	{"/bin/bash", "-lc"},
	{"pwsh"},
	{"pwsh", "-Command"},
	{"pwsh", "-c"},
	{"powershell"},
	{"powershell", "-Command"},
	{"powershell", "-c"},
	{"powershell.exe"},
	{"powershell.exe", "-Command"},
	{"powershell.exe", "-c"},
	{"env"},
	{"sudo"},
	{"node"},
	{"node", "-e"},
	{"perl"},
	{"perl", "-e"},
	{"ruby"},
	{"ruby", "-e"},
	{"php"},
	{"php", "-r"},
	{"lua"},
	{"lua", "-e"},
	{"osascript"},
}

func unsafeCommandPrefix(prefix []string) bool {
	if len(prefix) == 0 {
		return true
	}
	for _, part := range prefix {
		if unsafeCommandPrefixPart(part) {
			return true
		}
	}
	normalized := append([]string(nil), prefix...)
	normalized[0] = normalizeLauncherName(normalized[0])
	for _, banned := range bannedCommandPrefixSuggestions {
		if sameStringSlice(prefix, banned) || sameStringSlice(normalized, banned) {
			return true
		}
	}
	return unsafeCommandPrefixLauncher(prefix[0])
}

func unsafeCommandPrefixPart(part string) bool {
	part = strings.TrimSpace(part)
	return part == "" ||
		strings.ContainsAny(part, "\x00\r\n*?[]{}") ||
		strings.Contains(part, "$(") ||
		strings.Contains(part, "`")
}

func unsafeCommandPrefixLauncher(program string) bool {
	// A path is already refused. ":" and "~" are refused for the same reason:
	// on Windows they introduce names that resolve to a different file than the
	// one matched here, and no list can keep up with them. "PYTHON~1.EXE" is
	// python.exe under an 8.3 short name, and 8.3 truncates the stem, so
	// "POWERS~1.EXE" cannot be recognized by name at all; "python.exe::$DATA"
	// names the same executable through its default stream. Refusing the shape
	// costs a permission prompt on names no real command uses.
	if strings.ContainsAny(program, `/\:~`) {
		return true
	}
	name := normalizeLauncherName(program)
	return bannedLauncherName(name) || bannedLauncherName(strings.TrimRight(name, "0123456789"))
}

func bannedLauncherName(name string) bool {
	switch name {
	case "bash", "sh", "zsh", "dash", "ash", "ksh", "csh", "tcsh", "fish", "busybox",
		"pwsh", "powershell", "cmd", "wsl",
		"env", "sudo", "sudoedit", "doas", "su", "run0", "osascript",
		"command", "eval", "exec", "time",
		"find", "xargs", "timeout", "nice", "nohup", "watch", "setsid", "stdbuf", "ionice",
		"ssh", "make", "npm", "npx", "pnpm", "yarn", "bunx",
		"python", "py", "pythonw", "pyw", "pypy", "uv", "uvx",
		"node", "nodejs", "perl", "ruby", "php", "lua", "deno", "bun":
		return true
	default:
		return false
	}
}

// normalizeLauncherName reduces a program name to the launcher it actually runs
// so the list above cannot be stepped around by spelling the same interpreter
// differently: "python3.11", "python.exe" and "PYTHON3" all reach it as a name
// the list matches. internal/agent's commandName already normalizes case and
// Windows executable extensions on the allow side; this side matched raw, so
// every versioned or .exe-suffixed launcher validated as an ordinary command
// and persisted as a prefix grant that auto-allows every later invocation.
//
// A name is only ever narrowed toward an existing entry, so the failure
// direction is an extra permission prompt, never a silent grant.
func normalizeLauncherName(program string) string {
	name := strings.ToLower(strings.TrimSpace(program))
	// Windows discards trailing dots and spaces when resolving a filename, so
	// "python." starts python.exe. Drop them before anything matches on the
	// result, on every platform: the deny side must not depend on which OS is
	// reading the grant, since the grants file travels with a synced home
	// directory.
	name = strings.TrimRight(name, ". ")
	for _, suffix := range []string{".exe", ".cmd", ".bat", ".com", ".ps1"} {
		if strings.HasSuffix(name, suffix) {
			name = strings.TrimSuffix(name, suffix)
			name = strings.TrimRight(name, ". ")
			break
		}
	}
	return trimLauncherVersion(trimLauncherABISuffix(trimLauncherBuildSuffix(name)))
}

// trimLauncherBuildSuffix drops the build-channel suffixes distributions and
// upstreams append to an otherwise unchanged interpreter, so "python3.11-dbg",
// "bash-static" and "pwsh-preview" are the launchers they say they are. The set
// is closed on purpose: a general "-word" strip would also swallow
// "node-gyp", "python3-config" and "ruby-lsp", which are ordinary tools a user
// should still be able to approve.
func trimLauncherBuildSuffix(name string) string {
	for _, suffix := range []string{"-dbg", "-debug", "-static", "-nightly", "-preview", "-beta"} {
		if strings.HasSuffix(name, suffix) {
			return strings.TrimSuffix(name, suffix)
		}
	}
	return name
}

// trimLauncherABISuffix drops CPython's ABI flags from a versioned interpreter
// name, so "python3.7m" and "python3.6dm" reduce to their version, and the
// free-threaded builds "python3.13t" and "python3.13td" reduce to theirs. The
// flags are only removed when a digit sits underneath them, which leaves
// ordinary names that happen to end in those letters alone: "sha256sum" keeps
// its "sum", "zstd" its "td", "cat" its "t".
func trimLauncherABISuffix(name string) string {
	trimmed := strings.TrimRight(name, "dmut")
	if trimmed == name || trimmed == "" {
		return name
	}
	if last := trimmed[len(trimmed)-1]; last < '0' || last > '9' {
		return name
	}
	return trimmed
}

// trimLauncherVersion drops a trailing "<separator><digits>" version from a
// program name, so "python3.11" becomes "python3" and "python2.7" becomes
// "python2". Digits without a separator are part of the name, which keeps
// "base64", "7z" and "sha256sum" intact; the caller handles the "python3" shape
// by also testing the digit-stripped form.
func trimLauncherVersion(name string) string {
	for {
		trimmed := strings.TrimRight(name, "0123456789")
		if trimmed == name || trimmed == "" {
			return name
		}
		if !strings.HasSuffix(trimmed, ".") && !strings.HasSuffix(trimmed, "-") {
			return name
		}
		next := trimmed[:len(trimmed)-1]
		if next == "" {
			return name
		}
		name = next
	}
}

func sameStringSlice(left []string, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
