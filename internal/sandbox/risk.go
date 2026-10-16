package sandbox

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	// destructiveCommandPattern matches the highest-risk shell forms:
	//   - rm -rf (with combined/reordered r/f flags) targeting /, $HOME (bare,
	//     quoted, or ${HOME} braced), ~, or *, with an optional `--` before the
	//     target. Each target alternative tolerates optional surrounding quotes
	//     so `rm -rf "/"` / `rm -rf '/'` cannot slip past the gate.
	//   - chmod with combined/reordered flags and an octal-or-777 mode applied
	//     RECURSIVELY (a -R/-r flag) or to root / a sensitive SYSTEM tree
	//     (/, /etc, /usr, /bin, /var, … — e.g. chmod -Rf 777 /, chmod -R 0777 /,
	//     chmod 777 -R /etc, chmod 777 /etc). A single-file chmod 777 — including
	//     an absolute non-system path like `chmod 777 /tmp/build.sh` or a relative
	//     `chmod 777 script.sh` — is intentionally NOT flagged; the intent is
	//     recursive/directory-tree or system-tree chmod.
	//   - mkfs, dd if=, chown -R.
	destructiveCommandPattern = regexp.MustCompile(`(?i)(\brm\s+(-[A-Za-z]*r[A-Za-z]*f|-rf|-fr)\s+(--\s+)?["']?(\$\{?HOME\}?|/|~|\*)["']?|\bmkfs\b|\bdd\s+if=|\bchmod\s+(-[A-Za-z]*[rR][A-Za-z]*\s+)+0?777\b|\bchmod\s+(-\S+\s+)*0?777\s+-[A-Za-z]*[rR][A-Za-z]*\b|\bchmod\s+(-\S+\s+)*0?777\s+["']?/(\s|$|["']|(etc|usr|bin|sbin|lib|lib64|var|boot|opt|root|sys|proc|dev)\b)|\bchown\s+-R\b)`)
	// pipedInstallerPattern matches the fetch-and-execute idiom: a remote fetch
	// (curl/wget/fetch/aria2c) piped into a POSIX shell, with or without a space
	// and across sh/bash/zsh/ksh/dash (so `curl x|sh`, `wget url | bash`, `| zsh`).
	// A purely local pipe into a shell (e.g. `printf … | sh`, `cat ./s | bash`)
	// is NOT a piped installer and must not be flagged.
	pipedInstallerPattern = regexp.MustCompile(`(?i)\b(curl|wget|fetch|aria2c)\b[^|]*\|\s*(ba|z|k|da)?sh\b`)
	// unparseableNetworkPattern is used only after the shell parser fails. At
	// that point the command is already marked too complex, so this intentionally
	// favors catching obvious network programs over proving exact shell syntax.
	unparseableNetworkPattern = regexp.MustCompile(`(?i)\b(curl|wget|fetch|aria2c|ssh|scp|sftp|rsync|nc|ncat|netcat|telnet|ftp|npx|http-server|vite|next|nuxt|astro)\b|\b(npm|pnpm|yarn|bun|pip|pip2|pip3)\s+(install|add|publish|login|start|serve|dev|preview|run\s+(start|serve|dev|preview)|exec|x|dlx)\b|\bgo\s+get\b|\bgit\s+clone\b|\bpython(2|3)?\s+-m\s+(http\.server|pip\s+install)\b|\bgh\s+(api|repo\s+clone|release\s+download)\b`)
	// destructiveExtraPatterns hold high-severity patterns that the legacy
	// destructiveCommandPattern does not already cover. Folded in from the
	// blueprint safe_bash.go without duplicating existing matches.
	destructiveExtraPatterns = []*regexp.Regexp{
		// Fork bomb (and minor spacing variants).
		regexp.MustCompile(`:\s*\(\s*\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:`),
		// Writing to a raw block device (dd of=, redirect to /dev/sdX, etc.).
		regexp.MustCompile(`(?i)>\s*/dev/(sd[a-z]+\d*|nvme\d+n\d+(p\d+)?|hd[a-z]+\d*|xvd[a-z]+\d*|mmcblk\d+)`),
		regexp.MustCompile(`(?i)\bof=/dev/(sd[a-z]+\d*|nvme\d+n\d+(p\d+)?|hd[a-z]+\d*|xvd[a-z]+\d*|mmcblk\d+)`),
		// rm targeting a dangerous root (/, /*, ~, $HOME, *) with ANY mix of
		// short/long flags (incl. --no-preserve-root) in any order, an optional
		// `--` separator, and optional surrounding quotes — so e.g.
		// `rm --no-preserve-root -rf -- "/"` and `rm --no-preserve-root -rf "/"`
		// cannot slip past the gate.
		regexp.MustCompile(`(?i)\brm\s+(-{1,2}\S+\s+)*(--\s+)?["']?(/\*?|~|\$\{?HOME\}?|\*)["']?(\s|$)`),
		// mkfs.<fstype> form (e.g. mkfs.ext4) not caught by the bare \bmkfs\b above when followed by a dot.
		regexp.MustCompile(`(?i)\bmkfs\.[a-z0-9]+\b`),
	}
)

func matchesDestructive(command string) bool {
	if destructiveCommandPattern.MatchString(command) {
		return true
	}
	for _, pattern := range destructiveExtraPatterns {
		if pattern.MatchString(command) {
			return true
		}
	}
	return false
}

func matchesUnparseableNetwork(command string) bool {
	return unparseableNetworkPattern.MatchString(command)
}

func Classify(request Request) Risk {
	return classifyWithScope(request, nil)
}

func classifyWithScope(request Request, scope *Scope) Risk {
	categories := map[string]bool{}
	level := RiskLow
	add := func(category string, risk RiskLevel) {
		categories[category] = true
		if riskRank(risk) > riskRank(level) {
			level = risk
		}
	}

	switch NormalizeSideEffect(request.SideEffect) {
	case SideEffectRead:
		add("read", RiskLow)
	case SideEffectWrite:
		add("write", RiskMedium)
	case SideEffectShell:
		add("shell", RiskHigh)
	case SideEffectNetwork:
		add("network", RiskHigh)
	case SideEffectLocalControl:
		add("local_control", RiskHigh)
	case SideEffectLocalBrowser:
		add("local_browser", RiskHigh)
	case SideEffectLocalDesktop:
		add("local_desktop", RiskHigh)
	case SideEffectLocalTerminal:
		add("local_terminal", RiskHigh)
	case SideEffectOutOfWorkspace:
		add("out_of_workspace", RiskCritical)
	case SideEffectNone:
		// Control-only tool (e.g. escalate_model): no read/write/shell/network
		// effect, so it contributes no side-effect risk category and stays low.
	}

	// The bash tool accepts the command under any of these aliases; resolve the
	// first non-empty so destructive/network/piped-installer classification
	// cannot be bypassed by choosing a different alias key.
	command := firstArgString(request.Args, "command", "cmd", "script", "shell")
	if command != "" {
		if matchesDestructive(command) {
			add("destructive", RiskCritical)
		}
		if pipedInstallerPattern.MatchString(command) {
			add("piped_installer", RiskCritical)
		}
		// AST second opinion (analyzer.go): walks the parsed shell tree, so it
		// catches destructive/network programs the regexes miss — e.g. shred,
		// fdisk, parted, and commands hidden behind sudo/env wrappers or a
		// `sh -c <payload>` launcher — and flags an unparseable (obfuscated)
		// script as elevated risk. It only ADDS categories, so a benign,
		// parseable command is classified exactly as before.
		analysis := AnalyzeCommand(command)
		if analysis.Network {
			add("network", RiskCritical)
		}
		if analysis.TooComplex && matchesUnparseableNetwork(command) {
			add("network", RiskCritical)
		}
		if analysis.Destructive {
			add("destructive", RiskCritical)
		}
		if analysis.TooComplex {
			add("unparseable_command", RiskHigh)
		}
	}

	for _, path := range requestPaths(request) {
		if filepath.IsAbs(path) {
			add("absolute_path", RiskMedium)
		}
		if path == ".." || strings.HasPrefix(filepath.ToSlash(filepath.Clean(path)), "../") {
			add("path_escape", RiskCritical)
		}
		if request.WorkspaceRoot != "" {
			var block *pathBlock
			if scope != nil {
				block = scope.validate(path)
			} else {
				block = validateWorkspacePath(request.WorkspaceRoot, path)
			}
			if block != nil {
				switch block.Code {
				case BlockSymlinkTraversal:
					add("symlink_traversal", RiskCritical)
				default:
					add("out_of_workspace", RiskCritical)
				}
			}
		}
	}

	names := make([]string, 0, len(categories))
	for category := range categories {
		names = append(names, category)
	}
	sort.Strings(names)
	return Risk{
		Level:      level,
		Categories: names,
		Reason:     riskReason(level, names),
	}
}

func HasRiskCategory(risk Risk, category string) bool {
	for _, candidate := range risk.Categories {
		if candidate == category {
			return true
		}
	}
	return false
}

func riskRank(level RiskLevel) int {
	switch level {
	case RiskLow:
		return 0
	case RiskMedium:
		return 1
	case RiskHigh:
		return 2
	case RiskCritical:
		return 3
	default:
		return 0
	}
}

func riskReason(level RiskLevel, categories []string) string {
	if len(categories) == 0 {
		return string(level)
	}
	return fmt.Sprintf("%s risk: %s", level, strings.Join(categories, ", "))
}

func argString(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	value, ok := args[key]
	if !ok || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case fmt.Stringer:
		return strings.TrimSpace(typed.String())
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

// firstArgString returns the first non-empty argument value among keys.
func firstArgString(args map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := argString(args, key); value != "" {
			return value
		}
	}
	return ""
}

func firstExactStringArg(args map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := args[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

// pathArgKeys is the alias list the sandbox inspects for path-carrying tool
// arguments. Keep it aligned with the alias lists the tools themselves accept
// (see aliasedStringArg in write_file/edit_file/read_file/grep/glob/list): the
// sandbox gates by arg-key name, so any alias a tool resolves but the sandbox
// does not inspect would let a model route a write/read around the
// workspace+symlink boundary.
var pathArgKeys = []string{"path", "file", "file_path", "filepath", "filename", "cwd", "workdir", "dir", "directory"}

func requestPaths(request Request) []string {
	paths := []string{}
	for _, key := range pathArgKeys {
		// Gate on the EXACT bytes, because that is what the tool will open:
		// aliasedStringArg does not trim, so a trimming gate inspects a
		// different pathname than the one that gets read. A credential file
		// whose name carries meaningful whitespace ("bridge-token " on Unix)
		// was protected under its real spelling while the gate checked the
		// trimmed name and allowed the read.
		//
		// The trimmed spelling is still emitted when it differs, so the gate
		// never inspects LESS than it did before this became exact — a
		// whitespace-padded argument is now checked both ways rather than only
		// the way the tool does not use.
		value, ok := request.Args[key].(string)
		if !ok || value == "" {
			continue
		}
		paths = append(paths, value)
		if trimmed := strings.TrimSpace(value); trimmed != "" && trimmed != value {
			paths = append(paths, trimmed)
		}
	}
	if request.ToolName == "apply_patch" {
		paths = append(paths, applyPatchRequestPaths(request)...)
	}
	return paths
}

func applyPatchRequestPaths(request Request) []string {
	if request.PatchPaths == nil {
		return nil
	}
	// apply_patch consumes cwd as exact pathname data. Trimming here would make
	// the gate derive targets under a different directory than the tool applies them.
	cwd := firstExactStringArg(request.Args, "cwd")
	var paths []string
	for _, path := range request.PatchPaths {
		if path == "" || path == "/dev/null" {
			continue
		}
		if cwd != "" && filepath.Clean(cwd) != "." && !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		paths = append(paths, path)
	}
	return paths
}

func applyPatchPathBlock(request Request) *pathBlock {
	if request.ToolName != "apply_patch" {
		return nil
	}
	patch := firstExactStringArg(request.Args, "patch", "diff")
	if patch == "" {
		return nil
	}
	if request.PatchPaths == nil {
		return &pathBlock{
			Code:   BlockDenied,
			Reason: "patch paths were not supplied by the apply_patch executor",
		}
	}
	// Only relative traversal is rejected up front. Absolute paths flow through
	// the regular workspace-scope validation below (requestPaths), which accepts
	// one inside the workspace and denies one outside — a model that echoes the
	// absolute path read_file showed it must not be blocked for that alone.
	for _, path := range request.PatchPaths {
		if path == "" || path == "/dev/null" {
			continue
		}
		if path == ".." || strings.HasPrefix(path, "../") {
			return &pathBlock{
				Code:   BlockOutsideWorkspace,
				Path:   path,
				Reason: fmt.Sprintf("patch path %q must stay inside the workspace", path),
			}
		}
	}
	return nil
}

// structuredPatchMarkerPattern is the single classifier for structured-patch
// markers, shared by the sandbox boundary and the apply_patch tool (which
// imports this package). It accepts the canonical "*** Begin Patch" /
// "*** End Patch" and the decorated spellings models emit ("*** Begin Patch ***",
// "***Begin Patch", trailing whitespace). Both sides must agree: a spelling the
// tool would apply but the sandbox did not recognise would make the sandbox
// scan the patch as a unified diff, extract no targets, and validate nothing.
var structuredPatchMarkerPattern = regexp.MustCompile(`^\*{3}\s*(Begin|End) Patch\s*\**\s*$`)

// StructuredPatchMarker classifies a line as the "begin" or "end" marker of a
// structured patch, or "" when it is neither.
func StructuredPatchMarker(line string) string {
	match := structuredPatchMarkerPattern.FindStringSubmatch(strings.TrimSpace(line))
	if match == nil {
		return ""
	}
	return strings.ToLower(match[1])
}

// IsStructuredPatch reports whether patch opens with a structured begin marker.
// The tool applies exactly the patches this returns true for, so the sandbox
// extracts structured header paths for exactly the same set.
func IsStructuredPatch(patch string) bool {
	first, _, _ := strings.Cut(strings.TrimSpace(strings.TrimPrefix(patch, "\ufeff")), "\n")
	return StructuredPatchMarker(first) == "begin"
}

func parseDiffGitPaths(line string) (string, string, bool) {
	if line == "" {
		return "", "", false
	}
	if strings.HasPrefix(line, "\"") {
		source, rest, ok := consumeGitPath(line)
		if !ok || len(rest) < 2 || rest[0] != ' ' {
			return "", "", false
		}
		destination, ok := parseWholeGitPath(rest[1:])
		return source, destination, ok && validDiffGitPaths(source, destination)
	}

	// A quoted destination uniquely separates the operands even when the source
	// contains ordinary spaces.
	for separator := strings.Index(line, " \""); separator >= 0; {
		if destination, ok := parseWholeGitPath(line[separator+1:]); ok {
			source := line[:separator]
			if validDiffGitPaths(source, destination) {
				return source, destination, true
			}
		}
		next := strings.Index(line[separator+1:], " \"")
		if next < 0 {
			break
		}
		separator += next + 1
	}

	type candidate struct{ source, destination string }
	var candidates []candidate
	for separator := range len(line) {
		if line[separator] != ' ' {
			continue
		}
		source, destination := line[:separator], line[separator+1:]
		if validDiffGitPaths(source, destination) {
			candidates = append(candidates, candidate{source: source, destination: destination})
		}
	}
	var prefixed []candidate
	for _, candidate := range candidates {
		if hasDefaultGitPrefixes(candidate.source, candidate.destination) {
			prefixed = append(prefixed, candidate)
		}
	}
	if len(prefixed) == 1 {
		return prefixed[0].source, prefixed[0].destination, true
	}
	var same []candidate
	for _, candidate := range candidates {
		if candidate.source == candidate.destination {
			same = append(same, candidate)
		}
	}
	if len(same) == 1 {
		return same[0].source, same[0].destination, true
	}
	if len(candidates) == 1 {
		return candidates[0].source, candidates[0].destination, true
	}
	return "", "", false
}

func parseWholeGitPath(input string) (string, bool) {
	if input == "" {
		return "", false
	}
	if input[0] != '"' {
		return input, true
	}
	path, rest, ok := consumeGitPath(input)
	return path, ok && rest == ""
}

func validDiffGitPaths(source, destination string) bool {
	return source != "" && destination != ""
}

func hasDefaultGitPrefixes(source, destination string) bool {
	return len(source) > 2 && len(destination) > 2 &&
		strings.HasPrefix(source, "a/") && strings.HasPrefix(destination, "b/")
}

func normalizeDiffGitPaths(source, destination string) (string, string) {
	if hasDefaultGitPrefixes(source, destination) {
		return source[2:], destination[2:]
	}
	return source, destination
}

// Extended copy/rename headers consume the whole unquoted remainder as the
// pathname, including leading spaces. For a C-quoted name Git consumes the
// first complete quoted string, so trailing header text is not pathname data.
func parseExtendedGitPath(input string) (string, bool) {
	if input == "" {
		return "", false
	}
	if input[0] != '"' {
		return input, true
	}
	path, rest, ok := consumeGitPath(input)
	return path, ok && rest == ""
}

// consumeGitPath reads one diff --git path operand. Git uses C-style quoted
// strings for paths containing characters that need escaping; strconv.Unquote
// handles its escaped quotes, backslashes, control characters, and octal bytes.
func consumeGitPath(input string) (string, string, bool) {
	if input == "" {
		return "", "", false
	}
	if input[0] != '"' {
		end := strings.IndexAny(input, " \t")
		if end < 0 {
			return input, "", true
		}
		return input[:end], input[end:], true
	}

	escaped := false
	for i := 1; i < len(input); i++ {
		switch {
		case escaped:
			escaped = false
		case input[i] == '\\':
			escaped = true
		case input[i] == '"':
			path, err := strconv.Unquote(input[:i+1])
			if err != nil {
				return "", "", false
			}
			return path, input[i+1:], true
		}
	}
	return "", "", false
}

// patchFileHeaderPath mirrors the apply_patch parser's handling of unified-diff
// file headers: paths may be C-quoted and may contain spaces, while an optional
// timestamp is separated by a tab.
//
// Only the tab is header formatting. Git's own parser takes every remaining byte
// of an unquoted operand as pathname data, so a name with a leading or trailing
// space is written verbatim; trimming here would make the token gate evaluate
// `bridge-token` while git patches the live `bridge-token ` beside it.
func patchFileHeaderPath(line string) (string, bool) {
	if len(line) < len("--- ") {
		return "", false
	}
	rest := line[len("--- "):] // "--- " and "+++ " are both 4 bytes
	if tab := strings.IndexByte(rest, '\t'); tab >= 0 {
		rest = rest[:tab]
	}
	if strings.HasPrefix(rest, `"`) {
		path, remainder, ok := consumeGitPath(rest)
		if !ok || remainder != "" {
			return "", false
		}
		return path, true
	}
	return rest, rest != ""
}

// The apply_patch executor uses the exported parsers below for every pathname
// operand. They carry one contract:
// every byte of a header's pathname operand is pathname data. Only structural
// formatting is removed — the fixed header prefix, a tab-separated timestamp, a
// C-quoted operand's quoting, and matching a/ b/ prefixes. Nothing is trimmed,
// case-folded, or shell-split. A consumer that needs a different spelling must
// derive it from these bytes rather than re-reading the header.

// PatchFileHeaderPath returns the pathname named by a unified-diff "--- " or
// "+++ " file header line, reporting false when the header is not a form this
// parser can interpret exactly. An empty pathname is returned as ("", true):
// the header carries no target, which is not the same as an unreadable one.
func PatchFileHeaderPath(line string) (string, bool) {
	return patchFileHeaderPath(line)
}

// ExtendedGitHeaderPath returns the pathname named by a git extended header —
// "rename from ", "rename to ", "copy from ", "copy to " — given the operand
// that follows the header's fixed prefix.
func ExtendedGitHeaderPath(operand string) (string, bool) {
	return parseExtendedGitPath(operand)
}

// DiffGitPaths returns the source and destination named by the operand section
// of a "diff --git " line (the text after the prefix), with a matching a/ and
// b/ prefix pair removed. It reports false for operands whose split between the
// two pathnames is ambiguous.
func DiffGitPaths(operands string) (string, string, bool) {
	source, destination, ok := parseDiffGitPaths(operands)
	if !ok {
		return "", "", false
	}
	source, destination = normalizeDiffGitPaths(source, destination)
	return source, destination, true
}

// StripPatchPrefix removes a single leading a/ or b/ from a unified-diff path
// and normalizes separators to "/", preserving every other byte.
func StripPatchPrefix(path string) string {
	return stripPatchPrefix(path)
}

func stripPatchPrefix(path string) string {
	if strings.HasPrefix(path, "a/") || strings.HasPrefix(path, "b/") {
		path = path[2:]
	}
	return filepath.ToSlash(path)
}
