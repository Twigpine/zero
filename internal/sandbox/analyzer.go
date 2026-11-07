package sandbox

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// AnalysisResult is a static, AST-based assessment of a shell script. It is a
// more precise second opinion than the regex detector in safe_command.go:
// because it walks the parsed command tree, a program name is only counted when
// it is an actual command, never when it appears inside a quoted argument (so
// `echo "git rebase -i"` and `node -e "require('repl').start()"` are clean).
type AnalysisResult struct {
	Interactive bool
	Destructive bool
	Network     bool
	// TooComplex is set when the script cannot be parsed (obfuscated or invalid),
	// so a caller can treat it as higher-risk instead of trusting a clean result.
	TooComplex bool
	// Programs lists the distinct top-level command names found, for diagnostics.
	Programs []string
}

// destructivePrograms are commands that can irrecoverably destroy data.
var destructivePrograms = map[string]bool{
	"mkfs": true, "fdisk": true, "shred": true, "dd": true, "parted": true,
}

var powerShellRemoveItemPrograms = map[string]bool{
	"remove-item": true,
	"ri":          true,
	"rd":          true,
	"rmdir":       true,
	"del":         true,
	"erase":       true,
	"rm":          true,
}

// networkPrograms are commands that perform network egress/ingress.
var networkPrograms = map[string]bool{
	"curl": true, "wget": true, "ssh": true, "scp": true, "sftp": true,
	"rsync": true, "nc": true, "ncat": true, "netcat": true, "telnet": true,
	"ftp": true, "iwr": true, "irm": true, "invoke-webrequest": true,
	"invoke-restmethod": true,
}

var localServerPrograms = map[string]bool{
	"http-server": true,
	"serve":       true,
	"vite":        true,
	"next":        true,
	"nuxt":        true,
	"astro":       true,
}

// AnalyzeCommand parses script and reports interactive/destructive/network usage
// from the shell AST. A script that cannot be parsed yields TooComplex (with no
// other flags set) so the caller can decide how to treat an unanalyzable command.
// maxAnalyzerDepth bounds recursion into `sh -c <payload>` launchers so a
// pathologically nested script cannot cause unbounded work.
const maxAnalyzerDepth = 4

// shellPrograms run their `-c` argument as a fresh command, so the analyzer
// recurses into that payload instead of classifying on the shell name.
var shellPrograms = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "ksh": true, "dash": true,
}

func AnalyzeCommand(script string) AnalysisResult {
	result := AnalysisResult{}
	if strings.TrimSpace(script) == "" {
		return result
	}
	analyzeInto(script, &result, map[string]bool{}, 0)
	return result
}

// astCommandFields parses command with the shell parser and returns each simple
// command as its literal field slice (program + args as text), resolving the
// real command positions across quoting, command substitution, subshells, and
// newline separators — the constructs the hand-written splitter in
// safe_command.go mis-handles (issue #473). It returns nil when the command
// cannot be parsed (e.g. a Windows cmd.exe string), so callers fall through to
// the regex path rather than hard-blocking.
func astCommandFields(command string) [][]string {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return nil
	}
	var commands [][]string
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		// EVERY word must be a static literal, not just the program name.
		// wordText keeps only the literal/quoted parts of a word and silently
		// drops expansions, so a dynamic word anywhere in the call reconstructs
		// to something the shell will never run: `$(printf foo)vim` (runs as
		// `foovim`) collapses to "vim", and `git $(printf foo)rebase -i` (runs
		// as `git foorebase -i`, non-interactive) collapses to `git rebase -i`
		// and fabricates an interactive match. Since the runtime value of an
		// expansion is unknowable here, skip the whole call rather than classify
		// a lossy reconstruction. Skipping is the safe direction: the
		// hand-written passes above already ran, and a missed detection falls
		// through to the normal permission prompt instead of hard-blocking a
		// command the user never wrote.
		for _, word := range call.Args {
			if !isLiteralWord(word) {
				return true
			}
		}
		fields := make([]string, 0, len(call.Args))
		for _, word := range call.Args {
			fields = append(fields, wordText(word))
		}
		commands = append(commands, fields)
		return true
	})
	return commands
}

// isLiteralWord reports whether every part of word is a static literal (bare or
// quoted). A word containing a command substitution, parameter/arithmetic
// expansion, process substitution, etc. is dynamic — its runtime value is
// unknown, so its wordText (a partial literal) must not be trusted as a program
// name.
func isLiteralWord(word *syntax.Word) bool {
	if word == nil {
		return false
	}
	for _, part := range word.Parts {
		switch typed := part.(type) {
		case *syntax.Lit, *syntax.SglQuoted:
		case *syntax.DblQuoted:
			for _, inner := range typed.Parts {
				if _, ok := inner.(*syntax.Lit); !ok {
					return false
				}
			}
		default:
			return false
		}
	}
	return true
}

// analyzeInto parses script and folds its interactive/destructive/network usage
// into result, sharing seen so program names are de-duplicated across recursion.
func analyzeInto(script string, result *AnalysisResult, seen map[string]bool, depth int) {
	file, err := syntax.NewParser().Parse(strings.NewReader(script), "")
	if err != nil {
		result.TooComplex = true
		return
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		// Resolve the real program behind wrapper prefixes (sudo, env, nice, ...)
		// so `sudo rm -rf`, `env curl …`, and `bash -c 'vim x'` are classified on
		// the payload, not the launcher — matching DetectInteractiveCommand.
		prog, rest := effectiveProgram(call.Args)
		if prog == "" {
			return true
		}
		if !seen[prog] {
			seen[prog] = true
			result.Programs = append(result.Programs, prog)
		}
		// `sh -c <payload>` runs the payload as a fresh command; recurse into it so
		// a program hidden behind a shell launcher is still classified.
		if depth < maxAnalyzerDepth && shellPrograms[prog] {
			if payload := dashCPayload(rest); payload != "" {
				analyzeInto(payload, result, seen, depth+1)
			}
		}
		if _, interactive := interactivePrograms[prog]; interactive && !replSuppressed(prog, rest) {
			result.Interactive = true
		}
		if commandUsesNetwork(prog, rest) {
			result.Network = true
		}
		if destructivePrograms[prog] ||
			(prog == "rm" && hasRecursiveForce(rest)) ||
			(powerShellRemoveItemPrograms[prog] && hasPowerShellRecursiveForce(rest)) ||
			(prog == "find" && hasFindDelete(rest)) {
			result.Destructive = true
		}
		return true
	})
}

func commandUsesNetwork(prog string, args []*syntax.Word) bool {
	if networkPrograms[prog] {
		return true
	}
	words := literalWordTexts(args)
	if localServerPrograms[prog] {
		return true
	}
	switch prog {
	case "python", "python2", "python3", "py":
		return pythonModuleUsesNetwork(words)
	case "npm":
		return packageManagerUsesNetwork(words, map[string]string{
			"run":  "run",
			"exec": "exec",
			"x":    "exec",
		})
	case "pnpm":
		return packageManagerUsesNetwork(words, map[string]string{
			"run":  "run",
			"exec": "exec",
			"dlx":  "exec",
		})
	case "yarn":
		return packageManagerUsesNetwork(words, map[string]string{
			"run":  "run",
			"exec": "exec",
			"dlx":  "exec",
		})
	case "bun":
		return packageManagerUsesNetwork(words, map[string]string{
			"run": "run",
			"x":   "exec",
		})
	case "npx":
		return npxUsesNetwork(words)
	case "pip", "pip2", "pip3":
		return firstSubcommand(words, nil) == "install"
	case "go":
		return firstSubcommand(words, nil) == "get"
	case "git":
		return gitUsesNetwork(words)
	case "gh":
		return ghUsesNetwork(words)
	default:
		return false
	}
}

func packageManagerUsesNetwork(words []string, aliases map[string]string) bool {
	if packageManagerOffline(words) {
		return false
	}
	first := firstSubcommand(words, aliases)
	switch first {
	case "install", "add", "ci", "create", "dlx", "publish", "unpublish",
		"login", "logout", "adduser", "whoami", "ping", "audit", "outdated",
		"update", "upgrade", "search", "view", "info", "show", "dist-tag",
		"deprecate", "owner", "org", "team", "token", "profile", "access":
		return true
	case "start", "serve", "dev", "preview":
		return true
	case "run":
		second := secondSubcommand(words)
		return second == "start" || second == "serve" || second == "dev" || second == "preview"
	case "exec":
		// Package-manager exec commands may resolve and download a missing
		// package before launching it. An explicit offline flag keeps this path
		// inside the isolated namespace; otherwise request egress up front.
		return true
	}
	return false
}

func packageManagerOffline(words []string) bool {
	for _, word := range words {
		switch word {
		case "--offline", "--no-network":
			return true
		}
	}
	return false
}

func gitUsesNetwork(words []string) bool {
	switch firstSubcommand(words, nil) {
	case "clone", "fetch", "pull", "push", "ls-remote", "archive":
		return true
	default:
		return false
	}
}

func npxUsesNetwork(_ []string) bool {
	return true
}

func literalWordTexts(args []*syntax.Word) []string {
	words := make([]string, 0, len(args))
	for _, arg := range args {
		words = append(words, strings.ToLower(strings.TrimSpace(wordText(arg))))
	}
	return words
}

func pythonModuleUsesNetwork(words []string) bool {
	for index := 0; index < len(words); index++ {
		if words[index] != "-m" || index+1 >= len(words) {
			continue
		}
		module := words[index+1]
		if module == "http.server" {
			return true
		}
		if module == "pip" && firstSubcommand(words[index+2:], nil) == "install" {
			return true
		}
	}
	return false
}

func ghUsesNetwork(words []string) bool {
	first := firstSubcommand(words, nil)
	if first == "api" {
		return true
	}
	second := secondSubcommand(words)
	return (first == "release" && second == "download") ||
		(first == "repo" && second == "clone")
}

func secondSubcommand(words []string) string {
	firstSeen := false
	for _, word := range words {
		if word == "" || strings.HasPrefix(word, "-") {
			continue
		}
		if !firstSeen {
			firstSeen = true
			continue
		}
		return word
	}
	return ""
}

func firstSubcommand(words []string, aliases map[string]string) string {
	for _, word := range words {
		if word == "" || strings.HasPrefix(word, "-") || isNumericToken(word) {
			continue
		}
		if aliases != nil {
			if alias, ok := aliases[word]; ok {
				return alias
			}
		}
		return word
	}
	return ""
}

// wordText returns the literal text of a shell word, concatenating its plain and
// quoted literal parts (so "vim", 'vim', and vim all yield "vim"). Parts that are
// expansions ($x, $(...)) contribute nothing — the program name is taken as-is.
func wordText(word *syntax.Word) string {
	if word == nil {
		return ""
	}
	var builder strings.Builder
	for _, part := range word.Parts {
		switch typed := part.(type) {
		case *syntax.Lit:
			builder.WriteString(typed.Value)
		case *syntax.SglQuoted:
			builder.WriteString(typed.Value)
		case *syntax.DblQuoted:
			for _, inner := range typed.Parts {
				if lit, ok := inner.(*syntax.Lit); ok {
					builder.WriteString(lit.Value)
				}
			}
		}
	}
	return builder.String()
}

// effectiveProgram resolves the real command behind wrapper prefixes (sudo, env,
// nice, timeout, ...) and their consumed option values in an AST arg list,
// returning the program token and the args that follow it. It mirrors
// firstProgram in safe_command.go. An expansion-only program word ($x) yields ""
// because it cannot be classified statically.
func effectiveProgram(args []*syntax.Word) (string, []*syntax.Word) {
	wrapper := ""
	for index := 0; index < len(args); index++ {
		text := wordText(args[index])
		if text == "" {
			// A dynamic ($x) token in the PROGRAM position can't be classified, so
			// fail closed. But once we're past a wrapper, a dynamic arg is most
			// likely a wrapper flag/value — keep scanning so the literal payload that
			// follows is still classified (e.g. `env "$opts" curl …`).
			if wrapper == "" {
				return "", nil
			}
			continue
		}
		if strings.Contains(text, "=") && !strings.HasPrefix(text, "=") {
			continue // env-assignment prefix (e.g. `env FOO=bar cmd`)
		}
		if strings.HasPrefix(text, "-") {
			// Only consume the next token as a value when the ACTIVE wrapper says
			// this flag takes one; otherwise a valueless flag (e.g. `sudo -n`) would
			// swallow the real payload command (`rm`/`curl`).
			if wrapperConsumesValue(wrapper, text) && index+1 < len(args) {
				index++
			}
			continue
		}
		if isNumericToken(text) {
			continue
		}
		token := normalizeProgramToken(text)
		if wrapperPrograms[token] {
			wrapper = token
			continue
		}
		return token, args[index+1:]
	}
	return "", nil
}

// dashCPayload returns the literal text of the word following the shell's
// command flag in an AST arg list (the command a shell launcher will run), or ""
// when there is none. shellCommandFlag recognizes both a bare `-c`/`--command`
// and a POSIX short-option cluster such as `-ec`, `-lc`, or `-xc`.
func dashCPayload(args []*syntax.Word) string {
	for index := 0; index < len(args); index++ {
		// `--` ends option processing: the remaining words are positional
		// operands, so a later `-ec`/`-c` is not the shell's command flag.
		if wordText(args[index]) == "--" {
			break
		}
		if shellCommandFlag(wordText(args[index])) && index+1 < len(args) {
			return wordText(args[index+1])
		}
	}
	return ""
}

// replSuppressed reports whether a REPL program (python/node/...) was invoked
// non-interactively — with an inline-eval flag or a script argument — mirroring
// nonInteractiveREPLFlags used by the regex detector. Non-REPL interactive
// programs are never suppressed.
func replSuppressed(prog string, args []*syntax.Word) bool {
	flags, isREPL := nonInteractiveREPLFlags[prog]
	if !isREPL {
		return false
	}
	for _, arg := range args {
		text := wordText(arg)
		if text == "" {
			continue
		}
		for _, flag := range flags {
			if text == flag || strings.HasPrefix(text, flag+"=") {
				return true
			}
		}
		// A bare (non-flag) argument is a script path, e.g. `python app.py`.
		if !strings.HasPrefix(text, "-") {
			return true
		}
	}
	return false
}

// hasRecursiveForce reports whether an rm argument list contains both recursive
// and force flags (-rf, -r -f, --recursive --force, ...), the destructive form.
func hasRecursiveForce(args []*syntax.Word) bool {
	recursive, force := false, false
	for _, arg := range args {
		text := wordText(arg)
		switch {
		case text == "--":
			// End-of-options: every later token is an operand (a filename), so a
			// trailing `-rf`/`--force` is literal. `rm -- -rf` deletes a file named
			// "-rf" and must not be treated as the destructive recursive-force form.
			return recursive && force
		case text == "--recursive":
			recursive = true
		case text == "--force":
			force = true
		case strings.HasPrefix(text, "--"):
			// other long flag — ignore
		case strings.HasPrefix(text, "-"):
			for _, char := range text[1:] {
				switch char {
				case 'r', 'R':
					recursive = true
				case 'f':
					force = true
				}
			}
		}
	}
	return recursive && force
}

func hasPowerShellRecursiveForce(args []*syntax.Word) bool {
	recursive, force := false, false
	for _, arg := range args {
		switch strings.ToLower(wordText(arg)) {
		case "-recurse", "-r":
			recursive = true
		case "-force":
			force = true
		}
	}
	return recursive && force
}

func hasFindDelete(args []*syntax.Word) bool {
	for _, arg := range args {
		if wordText(arg) == "-delete" {
			return true
		}
	}
	return false
}
