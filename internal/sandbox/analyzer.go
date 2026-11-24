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
	"curl": true, "wget": true, "fetch": true, "aria2c": true,
	"ssh": true, "scp": true, "sftp": true,
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
		if resolveASTCommandNetwork(call.Args, depth).needsNetworkGate() {
			result.Network = true
		}
		prog, rest := effectiveProgram(call.Args)
		if prog == "" {
			return true
		}
		if !seen[prog] {
			seen[prog] = true
			result.Programs = append(result.Programs, prog)
		}
		if _, interactive := interactivePrograms[prog]; interactive && !replSuppressed(prog, rest) {
			result.Interactive = true
		}
		if destructivePrograms[prog] ||
			(prog == "rm" && hasRecursiveForce(rest)) ||
			(powerShellRemoveItemPrograms[prog] && hasPowerShellRecursiveForce(rest)) ||
			(prog == "find" && hasFindDelete(rest)) {
			result.Destructive = true
		}
		if shellPrograms[prog] {
			if index, found := shellCommandPayloadIndex(prog, wordTexts(rest)); found && index < len(rest) &&
				isLiteralWord(rest[index]) && depth < maxAnalyzerDepth {
				analyzeInto(wordText(rest[index]), result, seen, depth+1)
			}
		}
		return true
	})
}

// resolveASTCommandNetwork maps shell AST words onto the same tri-state argv
// resolver used by the unparseable fallback. Literal invocations take exactly
// one path. For dynamic words, the literal portions still establish known
// network programs, while executable or interpreter-source expansions remain
// unresolved and therefore keep the network gate.
func resolveASTCommandNetwork(words []*syntax.Word, depth int) commandResolution {
	if depth > maxAnalyzerDepth {
		return commandUnresolved
	}
	if fields, ok := literalCallFields(words); ok {
		program, args := effectiveProgram(words)
		switch program {
		case "cmd", "call", "start", "%comspec%":
			return commandNetworkResolution(cmdLauncherUsesNetwork(program, args, depth))
		default:
			return resolveCommandArgv(fields, depth)
		}
	}
	if envSplitSourceDynamic(words) {
		return commandUnresolved
	}
	program, args := effectiveProgram(words)
	if program == "" {
		return commandUnresolved
	}
	textArgs := wordTexts(args)
	if child, status, delegated := delegatedCommand(program, textArgs); delegated {
		if status != commandKnownLocal {
			return status
		}
		// Require literal launcher options and executable selection before
		// trusting the suffix boundary. Preserve all remaining AST words so
		// another wrapper or interpreter can inspect its own source operands.
		start := len(args) - len(child)
		selectionEnd := len(words) - len(child)
		if len(child) > 0 {
			selectionEnd++
		}
		if _, literal := literalCallFields(words[:selectionEnd]); !literal {
			return commandUnresolved
		}
		if len(child) == 0 {
			return commandKnownLocal
		}
		return resolveASTCommandNetwork(args[start:], depth+1)
	}
	resolution := resolveCommandArgv(append([]string{program}, textArgs...), depth)
	switch {
	case shellPrograms[program]:
		index, found := shellCommandPayloadIndex(program, textArgs)
		if !found || index >= len(args) {
			return resolution
		}
		if !isLiteralWord(args[index]) {
			return commandUnresolved
		}
	case program == "powershell" || program == "pwsh":
		source := fallbackPowerShellPayload(program, textArgs)
		if source.opaque || powerShellSourceDynamic(source, args) {
			return commandUnresolved
		}
	case program == "eval":
		if _, ok := literalCallFields(args); !ok {
			return commandUnresolved
		}
	case program == "cmd" || program == "call" || program == "start" || program == "%comspec%":
		if cmdLauncherUsesNetwork(program, args, depth) {
			return commandKnownNetwork
		}
	case program == "git" && resolution == commandKnownLocal:
		if gitSelectionDynamic(textArgs, func(index int) bool { return !isLiteralWord(args[index]) }) {
			return commandUnresolved
		}
	}
	return resolution
}

// gitSelectionDynamic reports whether an expansion this scan cannot read could
// choose what git actually does: the SUBCOMMAND itself, or `git archive`'s
// --remote, the one option that turns a local tree export into a request to
// another host.
//
// wordText drops an expansion, so the reconstructed argv shows a subcommand git
// may never run: `git $VERB origin main` reads as `git origin main` — an
// unrecognized, therefore local, subcommand — while the shell runs `git push`.
// A dynamic word BEFORE the subcommand counts too, because it can shift which
// token git reads as the subcommand: an empty expansion or one that word-splits
// changes the position, even as the value of a global option that normally
// consumes it.
//
// Past the subcommand nothing else can change the answer, so ordinary forms like
// `git commit -m "$MESSAGE"` stay proven-local and keep their quiet path.
//
// dynamic reports whether the word at an index is unreadable. The AST path
// supplies isLiteralWord and the fallback supplies fallbackTokenLooksDynamic, so
// neither can classify a shape the other fails closed on.
func gitSelectionDynamic(words []string, dynamic func(index int) bool) bool {
	invocation := parseGitInvocation(words)
	limit := len(words)
	if invocation.kind == gitCommandSubcommand {
		limit = min(invocation.subcommandIndex+1, len(words))
	}
	for index := range limit {
		if dynamic(index) {
			return true
		}
	}
	if invocation.kind != gitCommandSubcommand || invocation.subcommand != "archive" {
		return false
	}
	for index := invocation.subcommandIndex + 1; index < len(words); index++ {
		if dynamic(index) {
			return true
		}
	}
	return false
}

// literalProgramNetworkResolution classifies a program after launcher argv has
// been resolved. Wrapper, delegated-child, and interpreter-source grammars live
// in resolveCommandArgv so AST and fallback paths cannot select different
// children before reaching this program-specific table.
func literalProgramNetworkResolution(prog string, words []string) commandResolution {
	originalWords := words
	prog = normalizeProgramToken(trimCMDEchoPrefixToken(prog))
	normalized := make([]string, len(words))
	for index := range words {
		normalized[index] = strings.ToLower(strings.TrimSpace(words[index]))
	}
	words = normalized
	if networkPrograms[prog] || localServerPrograms[prog] {
		return commandKnownNetwork
	}
	switch prog {
	case "python", "python2", "python3", "py":
		return commandNetworkResolution(pythonModuleUsesNetwork(words))
	case "npm":
		return commandNetworkResolution(packageManagerUsesNetwork(words, map[string]string{
			"run":  "run",
			"exec": "exec",
			"x":    "exec",
		}))
	case "pnpm":
		return commandNetworkResolution(packageManagerUsesNetwork(words, map[string]string{
			"run":  "run",
			"exec": "exec",
			"dlx":  "exec",
		}))
	case "yarn":
		return commandNetworkResolution(packageManagerUsesNetwork(words, map[string]string{
			"run":  "run",
			"exec": "exec",
			"dlx":  "exec",
		}))
	case "bun":
		return commandNetworkResolution(packageManagerUsesNetwork(words, map[string]string{
			"run": "run",
			"x":   "exec",
		}))
	case "npx":
		return commandNetworkResolution(npxUsesNetwork(words))
	case "pip", "pip2", "pip3":
		return commandNetworkResolution(firstSubcommand(words, nil) == "install")
	case "go":
		return commandNetworkResolution(firstSubcommand(words, nil) == "get")
	case "git":
		return gitNetworkResolution(originalWords)
	case "gh":
		return commandNetworkResolution(ghUsesNetwork(words))
	default:
		return commandKnownLocal
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

func gitNetworkResolution(words []string) commandResolution {
	invocation := parseGitInvocation(words)
	if invocation.kind != gitCommandSubcommand {
		// No subcommand at all, or a global option that makes git print locally
		// and exit before any subcommand runs.
		return commandKnownLocal
	}
	// Git builtin names and aliases are case-sensitive. A case-distinct spelling
	// such as STATUS can therefore select an arbitrary alias even though status
	// is a known local builtin. Classification may normalize to recognize known
	// builtins, but it must not report an alias-shaped invocation as proven local.
	if invocation.subcommandOriginal != invocation.subcommand {
		return commandUnresolved
	}
	switch invocation.subcommand {
	// send-pack is push's plumbing counterpart — `git send-pack origin main`
	// performs exactly the egress `git push` does, so leaving it out gave the
	// same operation two different answers depending on which spelling was
	// used.
	case "clone", "fetch", "pull", "push", "ls-remote", "send-pack":
		return commandKnownNetwork
	case "archive":
		// `git archive HEAD` streams a tree out of the local object store and needs
		// no egress at all; only `--remote=<repo>` sends the request to another
		// host. Classifying every archive as network cost a proactive network
		// prompt on a purely local command.
		return commandNetworkResolution(gitTargetsRemoteArchive(words, invocation.subcommandIndex))
	default:
		return commandKnownLocal
	}
}

// gitTargetsRemoteArchive reports whether an archive subcommand has an active
// --remote option. Git accepts archive options after positional operands, so
// this must keep scanning until an unconsumed `--`. Value-taking options are
// consumed first: in `archive -o -- --remote=origin HEAD`, `-o` owns the first
// `--` and the later remote is active; in `archive -o --remote HEAD`, --remote
// is only the output filename.
func gitTargetsRemoteArchive(words []string, subcommandIndex int) bool {
	for index := subcommandIndex + 1; index < len(words); index++ {
		word := strings.ToLower(words[index])
		switch {
		case word == "--":
			return false
		case strings.HasPrefix(word, "--remote="):
			return true
		case word == "--remote":
			return index+1 < len(words)
		case gitArchivePreliminaryOptionConsumesValue(word):
			index++
		}
	}
	return false
}

// gitArchivePreliminaryOptionConsumesValue models the first parse-options pass
// in git archive. That pass consumes only transport/output options and retains
// format/prefix/mtime options for a later parser, so those later options must
// not hide an immediately following --remote.
func gitArchivePreliminaryOptionConsumesValue(option string) bool {
	if strings.Contains(option, "=") {
		return false
	}
	switch option {
	case "-o", "--output", "--exec":
		return true
	default:
		return false
	}
}

// gitCommandKind distinguishes the three outcomes of reading a git command
// line, which callers must treat differently.
type gitCommandKind int

const (
	// gitCommandNone: no subcommand was found (`git`, `git -C repo`).
	gitCommandNone gitCommandKind = iota
	// gitCommandTerminalGlobal: a global option that makes git print locally
	// and exit — no subcommand runs, whatever words follow it.
	gitCommandTerminalGlobal
	// gitCommandSubcommand: a subcommand git will actually execute.
	gitCommandSubcommand
)

type gitInvocation struct {
	kind gitCommandKind
	// subcommand is set only for gitCommandSubcommand.
	subcommand string
	// subcommandOriginal preserves the exact token Git will dispatch. Git alias
	// lookup is case-sensitive, so authorization callers must not replace it
	// with subcommand's normalized classifier spelling.
	subcommandOriginal string
	// subcommandIndex is the position in the original argument slice.
	subcommandIndex int
	// terminalOption is set only for gitCommandTerminalGlobal.
	terminalOption string
}

// gitTerminalGlobalOptions are git's global options that print something from
// the local installation and exit. Everything after one of them is help/version
// output text, not a command: `git -C repo --help push` prints git-push's
// manual page without contacting a remote, so a subcommand scan that walked
// past them would classify a purely local command as network.
// Bare `--exec-path` belongs here for the same reason: git documents it as
// `--exec-path[=<path>]`, so without an inline value it prints the compiled-in
// exec path and exits. `git --exec-path /tmp push` neither reads /tmp as the
// option's value nor runs push, so treating it as a value-taking option made a
// local informational command request egress.
var gitTerminalGlobalOptions = map[string]bool{
	"-h": true, "--help": true,
	"-v": true, "--version": true,
	"--html-path": true, "--man-path": true, "--info-path": true,
	"--list-cmds": true, "--exec-path": true,
}

// parseGitInvocation resolves what a git command line actually does, past git's
// GLOBAL options. It is the single reader both classification paths use — the
// AST path through gitUsesNetwork and the unparseable fallback through
// matchesUnparseableGitNetwork — so the two cannot disagree about an option, as
// they did while each carried its own skip list.
//
// The generic firstSubcommand cannot do this job: git's value-taking globals put
// their value in the next token, so scanning for the first non-dash token returns
// that value instead — `git -C repo push origin main` looked like the subcommand
// "repo" and so classified as no-network, dropping the proactive network prompt
// for the most common form of the command. internal/agent/command_prefix.go
// resolves the same option set for its own prefix matching.
func parseGitInvocation(words []string) gitInvocation {
	for index := 0; index < len(words); index++ {
		original := words[index]
		word := strings.ToLower(original)
		if word == "" {
			continue
		}
		if gitTerminalGlobalOptions[word] || strings.HasPrefix(word, "--list-cmds=") {
			return gitInvocation{kind: gitCommandTerminalGlobal, terminalOption: word}
		}
		if strings.HasPrefix(word, "-") {
			// A joined value (--git-dir=/x, -C/x) is one token and needs no skip;
			// a separated one puts its value in the next token.
			if GitGlobalOptionConsumesValue(word) {
				index++
			}
			continue
		}
		if isNumericToken(word) {
			continue
		}
		return gitInvocation{
			kind:               gitCommandSubcommand,
			subcommand:         word,
			subcommandOriginal: original,
			subcommandIndex:    index,
		}
	}
	return gitInvocation{kind: gitCommandNone}
}

// GitGlobalOptionConsumesValue lists git's global options whose value is a
// separate token. It is shared with internal/agent's command-prefix parser so
// the two security-sensitive scans cannot drift — internal/agent reaches this
// through GitSubcommand, which resolves the whole grammar (terminal globals
// included) in one place, rather than importing the terminal-global table
// separately.
//
// `--exec-path` is deliberately absent: its value is inline-only
// (`--exec-path=<path>`), and the bare spelling is terminal — see
// gitTerminalGlobalOptions.
func GitGlobalOptionConsumesValue(option string) bool {
	// The comparison is on the NORMALIZED name, so every case below must be
	// written lowercase — an uppercase case here would be unreachable rather
	// than protective. -c (config override, `-c name=value`) and -C (run as if
	// started in <path>) are different git options that both take a
	// separate-token value, so folding them onto one case is correct today and
	// `git -C repo push` consumes repo either way.
	//
	// If their value-taking behavior ever diverges, this cannot be fixed by
	// re-adding a -C case: the callers normalize before calling, so the
	// distinction has to be restored at the call site by passing the original
	// token. TestGitGlobalOptionConsumesValueFoldsShortCaseDeliberately pins
	// the fold so that change is a deliberate one.
	switch strings.ToLower(option) {
	case "-c", "--attr-source", "--config-env", "--git-dir", "--namespace", "--super-prefix", "--work-tree":
		return true
	default:
		return false
	}
}

// GitSubcommandInfo is the shared result of resolving Git's global-option
// grammar. Original is the exact token Git dispatches; Normalized is for
// conservative classification only. Authorization must compare Original
// against an explicit allowlist because Git alias lookup is case-sensitive.
type GitSubcommandInfo struct {
	Index      int
	Original   string
	Normalized string
}

// GitSubcommand resolves the git subcommand a command line will actually run,
// or reports that none runs — no subcommand is present (e.g. bare `git`, or
// `git -C repo` with nothing after it), or a terminal global like --help/
// --version made everything after it non-executed help/version text instead.
func GitSubcommand(words []string) (GitSubcommandInfo, bool) {
	invocation := parseGitInvocation(words)
	if invocation.kind != gitCommandSubcommand {
		return GitSubcommandInfo{}, false
	}
	return GitSubcommandInfo{
		Index:      invocation.subcommandIndex,
		Original:   invocation.subcommandOriginal,
		Normalized: invocation.subcommand,
	}, true
}

func npxUsesNetwork(_ []string) bool {
	return true
}

func wordTexts(args []*syntax.Word) []string {
	words := make([]string, 0, len(args))
	for _, arg := range args {
		words = append(words, strings.TrimSpace(wordText(arg)))
	}
	return words
}

// powerShellSourceDynamic reports whether the words carrying a PowerShell
// host's command source contain an expansion. wordText silently drops those,
// so `powershell -Command "$PAYLOAD"` otherwise extracts an empty payload and
// classifies as clean while the host runs whatever the shell expanded.
func powerShellSourceDynamic(source powerShellPayload, args []*syntax.Word) bool {
	if source.sourceIndex < 0 {
		return false
	}
	for index := source.sourceIndex; index < len(args); index++ {
		if !isLiteralWord(args[index]) {
			return true
		}
	}
	return false
}

// envSplitSourceDynamic reports whether an env invocation takes its
// -S/--split-string argv from a word this scan cannot resolve statically.
//
// The literal reconstruction used by envSplitCommandFields is an optimization,
// not a proof of safety: `PAYLOAD='curl https://…'; env -S "$PAYLOAD"` runs the
// expanded argv, so an unreadable operand must classify as network-sensitive
// rather than fall through to wrapper handling that reports no executable.
func envSplitSourceDynamic(args []*syntax.Word) bool {
	texts := make([]string, len(args))
	for index, arg := range args {
		texts[index] = wordText(arg)
	}
	start, ok := envArgumentStart(texts)
	if !ok {
		return false
	}
	seenSplit := false
	for index := start; index < len(texts); index++ {
		text := texts[index]
		if text == "--" {
			return false
		}
		if _, _, _, split := envSplitOption([]string{text}, 0); split {
			// A joined operand (-S"$PAYLOAD") reconstructs to the option alone, so
			// the option token itself carries the dynamic source.
			if !isLiteralWord(args[index]) {
				return true
			}
			seenSplit = true
			continue
		}
		if !isLiteralWord(args[index]) {
			if seenSplit {
				return true
			}
			continue
		}
		if seenSplit {
			// The -S operand is literal, but that is only proof about the SPLIT
			// STRING, not about the invocation: GNU env appends the remaining
			// argv to the argv the split string produced, so
			// `env -S 'sh -c' "$PAYLOAD"` runs an argument this scan cannot
			// read. Keep scanning; a dynamic token anywhere after the split
			// string extends the command unknowably and fails closed. Trailing
			// LITERAL tokens are fine — the ordinary literal path reads the
			// whole reconstructed argv, this one included.
			for rest := index + 1; rest < len(args); rest++ {
				if !isLiteralWord(args[rest]) {
					return true
				}
			}
			return false
		}
		if strings.Contains(text, "=") && !strings.HasPrefix(text, "=") && !strings.HasPrefix(text, "-") {
			continue
		}
		if strings.HasPrefix(text, "-") {
			if wrapperConsumesValue("env", text) && index+1 < len(texts) {
				index++
			}
			continue
		}
		// A command position was reached without a split string; ordinary wrapper
		// resolution classifies this invocation.
		return false
	}
	return false
}

// envArgumentStart returns the index just past an `env` program token, allowing
// the wrapper prefixes (sudo, nice, ...) that may precede it.
func envArgumentStart(texts []string) (int, bool) {
	wrapper := ""
	for index := 0; index < len(texts); index++ {
		text := texts[index]
		if text == "" {
			if wrapper == "" {
				return 0, false
			}
			continue
		}
		if strings.Contains(text, "=") && !strings.HasPrefix(text, "=") && !strings.HasPrefix(text, "-") {
			continue
		}
		if strings.HasPrefix(text, "-") {
			if wrapperConsumesValue(wrapper, text) && index+1 < len(texts) {
				index++
			}
			continue
		}
		if isNumericToken(text) {
			continue
		}
		token := normalizeProgramToken(text)
		if token == "env" {
			return index + 1, true
		}
		if wrapperPrograms[token] {
			wrapper = token
			continue
		}
		return 0, false
	}
	return 0, false
}

func literalCallFields(args []*syntax.Word) ([]string, bool) {
	fields := make([]string, 0, len(args))
	for _, arg := range args {
		if !isLiteralWord(arg) {
			return nil, false
		}
		fields = append(fields, wordText(arg))
	}
	return fields, true
}

func cmdLauncherUsesNetwork(program string, args []*syntax.Word, depth int) bool {
	switch program {
	case "cmd", "call", "start", "%comspec%":
	default:
		return false
	}
	tokens := make([]fallbackCommandToken, 0, len(args)+1)
	tokens = append(tokens, fallbackCommandToken{value: program})
	for _, arg := range args {
		if !isLiteralWord(arg) {
			return true
		}
		tokens = append(tokens, fallbackCommandToken{
			value:  wordText(arg),
			quoted: literalWordIsQuoted(arg),
		})
	}
	for _, body := range cmdCommandBodyTokenInfoCandidates(tokens) {
		if cmdBodyUsesNetwork(body, depth) {
			return true
		}
	}
	return false
}

// cmdBodyUsesNetwork classifies one CMD payload candidate.
//
// A single QUOTED token is genuinely ambiguous: `cmd /c "git push origin main"`
// is a command line, while `cmd /c "C:\Program Files\curl.exe"` is one program
// path that happens to contain a space. CMD itself resolves that by trying the
// path first and falling back to parsing it as a command line, so this
// classifies it BOTH ways and fails closed if either reading reaches the
// network. Treating it only as a program name — taking its basename — is what
// made `git push origin main` look like an unrecognized executable.
func cmdBodyUsesNetwork(body []fallbackCommandToken, depth int) bool {
	if len(body) == 0 {
		return false
	}
	if fallbackBodyUsesNetwork(fallbackTokenValues(body), depth) {
		return true
	}
	if len(body) == 1 && body[0].quoted && strings.ContainsAny(body[0].value, " \t") {
		return classifyInterpreterSource(body[0].value, interpreterSourceCMD, depth+1).needsNetworkGate()
	}
	return false
}

func literalWordIsQuoted(word *syntax.Word) bool {
	if word == nil || len(word.Parts) == 0 {
		return false
	}
	switch word.Parts[0].(type) {
	case *syntax.SglQuoted, *syntax.DblQuoted:
		return true
	default:
		return false
	}
}

// classifyCommandText is the single entry point for interpreter SOURCE — a
// string another program will run as a command line — as opposed to argv
// tokens naming a program to execute. It is the difference between
// `cmd /c "git push origin main"` meaning "run the command line git push
// origin main" and meaning "run the program named `git push origin main`";
// reading the second as the first is what let quoted one-liners through.
//
// Every launcher that carries command text routes through here: CMD /c and
// /k, CALL, start, eval, shell -c/--command, PowerShell -Command, and the
// fallback tokenizer's equivalents. It re-tokenizes the payload rather than
// taking its basename, runs BOTH the AST scan and the unparseable matcher (a
// CMD one-liner is legitimately not POSIX, so the parser failing on it is
// expected, not proof of safety), and fails closed when the text cannot be
// read at all.
type interpreterSourceLanguage uint8

const (
	interpreterSourcePOSIX interpreterSourceLanguage = iota
	interpreterSourceCMD
	interpreterSourcePowerShell
)

// classifyInterpreterSource is the single bounded classifier for text a
// launcher will execute. Language-specific syntax that the shared parser cannot
// faithfully interpret is unresolved rather than silently treated as local.
func classifyInterpreterSource(payload string, language interpreterSourceLanguage, depth int) commandResolution {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return commandKnownLocal
	}
	if depth > maxAnalyzerDepth {
		return commandUnresolved
	}
	if language == interpreterSourcePowerShell {
		if unreadable := powerShellSourceUnreadable(payload); unreadable != "" {
			return commandUnresolved
		}
		if resolution, found := classifyPowerShellExpressionEvaluation(payload, depth); found {
			return resolution
		}
	}
	result := AnalysisResult{}
	analyzeInto(payload, &result, map[string]bool{}, depth)
	if result.Network || matchesUnparseableNetworkAt(payload, depth) {
		return commandKnownNetwork
	}
	return commandKnownLocal
}

// powerShellBlockPunctuation opens or closes a PowerShell script block or
// statement block. Every block-structured form the language has — try/catch/
// finally, if/else, foreach, for, while, do, switch, function, trap, param, and
// a script block handed to ForEach-Object or the call operator — is written
// with braces, so their presence is the single signal that the source has
// structure the readers below do not model.
const powerShellBlockPunctuation = "{}"

// powerShellSourceUnreadable reports why PowerShell source cannot be proven
// local by the readers classifyInterpreterSource runs, or "" when it can.
//
// Those readers are the POSIX AST scan and the unparseable-command matcher.
// Neither models PowerShell grammar. They happen to read a simple
// `Verb-Noun -Arg value` line correctly because it tokenizes like a POSIX
// command, and that precision is worth keeping — but "the POSIX reader found no
// network program in this PowerShell source" is not evidence the source is
// local. `try { Invoke-WebRequest https://host } catch {}` is valid PowerShell
// that performs the request, and neither reader models try/catch, so it drew no
// network category at all.
//
// A conservative unresolved result is the contract here: only source a reader
// can actually parse and prove local may skip the gate. Unresolved costs a
// prompt on a shell request, not a denial, so the failure mode of being wrong
// in this direction is a question rather than a broken command.
func powerShellSourceUnreadable(payload string) string {
	if strings.ContainsRune(payload, '`') {
		// Backtick escaping and line continuation change PowerShell token
		// boundaries but have no equivalent in the POSIX/CMD readers.
		return "backtick escaping"
	}
	if strings.ContainsAny(payload, powerShellBlockPunctuation) {
		return "script or statement block"
	}
	return ""
}

// classifyPowerShellExpressionEvaluation handles the PowerShell edge that
// executes a string as a second PowerShell program. The shared POSIX reader can
// locate a simple Invoke-Expression/iex call, but it cannot infer source from an
// expression that constructs the argument. Only one literal single-quoted
// operand is therefore recursively classified; every other evaluator shape is
// unresolved and retains the network gate.
func classifyPowerShellExpressionEvaluation(payload string, depth int) (commandResolution, bool) {
	file, err := syntax.NewParser().Parse(strings.NewReader(payload), "")
	if err != nil {
		return commandUnresolved, powerShellExpressionEvaluatorToken(payload)
	}

	found := false
	resolution := commandKnownLocal
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 || !isPowerShellExpressionEvaluator(wordText(call.Args[0])) {
			return true
		}
		found = true
		if len(call.Args) != 2 {
			resolution = commandUnresolved
			return true
		}
		parts := call.Args[1].Parts
		if len(parts) != 1 {
			resolution = commandUnresolved
			return true
		}
		quoted, ok := parts[0].(*syntax.SglQuoted)
		if !ok {
			resolution = commandUnresolved
			return true
		}
		nested := classifyInterpreterSource(quoted.Value, interpreterSourcePowerShell, depth+1)
		if nested == commandKnownNetwork || resolution == commandKnownNetwork {
			resolution = commandKnownNetwork
		} else if nested == commandUnresolved {
			resolution = commandUnresolved
		}
		return true
	})
	return resolution, found
}

func powerShellExpressionEvaluatorToken(payload string) bool {
	for _, command := range fallbackCommandTokenInfo(payload) {
		for _, token := range command {
			if !token.quoted && isPowerShellExpressionEvaluator(token.value) {
				return true
			}
		}
	}
	return false
}

func isPowerShellExpressionEvaluator(token string) bool {
	return strings.EqualFold(token, "Invoke-Expression") || strings.EqualFold(token, "iex")
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
// unescapeDoubleQuoted applies POSIX double-quote escape removal to the literal
// text the parser preserves verbatim inside a double-quoted word.
//
// The parser keeps `\"` as two characters because escape removal is the
// shell's job at expansion time, not the parser's. For an argv token that is
// harmless; for the `-c` operand of a shell launcher it is not, because the
// text IS the next command. Without this, one level of
// `sh -c "sh -c \"curl …\""` handed the recursion the fragment `\"sh` — the
// rest of the payload silently dropped — and the nested curl disappeared from
// classification entirely at nesting level 3.
//
// Inside double quotes a backslash is special ONLY before $, `, ", \, and a
// newline; everywhere else it is a literal backslash and must be preserved, or
// a Windows path inside quotes would lose its separators.
func unescapeDoubleQuoted(value string) string {
	if !strings.Contains(value, `\`) {
		return value
	}
	var builder strings.Builder
	builder.Grow(len(value))
	for index := 0; index < len(value); index++ {
		if value[index] != '\\' || index+1 >= len(value) {
			builder.WriteByte(value[index])
			continue
		}
		switch next := value[index+1]; next {
		case '$', '`', '"', '\\':
			builder.WriteByte(next)
			index++
		case '\n':
			// A quoted line continuation removes both characters.
			index++
		default:
			builder.WriteByte('\\')
		}
	}
	return builder.String()
}

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
					builder.WriteString(unescapeDoubleQuoted(lit.Value))
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
