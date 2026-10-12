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
	// LocalServer is set when a command BINDS a local port rather than reaching
	// out: `python -m http.server`, `vite`, `next dev` and friends.
	//
	// Kept distinct from Network instead of folded into it. Listening and
	// fetching are different acts with different consequences, and treating a
	// dev server as egress made ordinary local work prompt for network approval
	// it never needed. The information is preserved rather than dropped, so a
	// caller that does care about inbound can still see it.
	LocalServer bool
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

// pythonLauncherPrograms is every spelling of the Python launcher the analyzer
// accepts. It is a named list rather than a repeated switch case because the
// unparseable fallback in risk.go has to be a superset of it, and the two were
// maintained separately: "py" was accepted here and missing there, so a Windows
// batch spelling the POSIX parser rejects lost the network gate that its
// parseable equivalent receives. TestUnparseableFallbackCoversEveryPythonLauncher
// now derives from this, so a new alias cannot weaken enforcement silently.
var pythonLauncherPrograms = map[string]bool{
	"python":  true,
	"python2": true,
	"python3": true,
	"py":      true,
}

var localServerPrograms = map[string]bool{
	"http-server": true,
	"serve":       true,
	"vite":        true,
	"next":        true,
	"nuxt":        true,
	"astro":       true,
}

// packageManagerServingScripts are the script names that start a dev server
// across npm, pnpm, yarn and bun. Authoritative: risk.go's unparseable fallback
// has to stay a superset of what this flags, and the superset test drives its
// cases off this map so a name added here cannot silently weaken the fallback.
var packageManagerServingScripts = map[string]bool{
	"start":   true,
	"serve":   true,
	"dev":     true,
	"preview": true,
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
		if commandRunsLocalServer(prog, rest) {
			result.LocalServer = true
			// LocalServer is ADDITIVE, not a replacement for Network, and that is
			// the whole of this line.
			//
			// Nothing consumes LocalServer yet: no policy or runner code reads it,
			// so classifying a serving command as local-only did not grant it a
			// scoped host listener, it only removed the network approval it used to
			// get. The command then ran under the default deny profile, which on
			// Linux is a network namespace and on macOS is (deny network*), so
			// `python -m http.server` and `vite` started without a prompt and could
			// not serve a preview to the operator's browser.
			//
			// It is also unsound for the package managers. `npm run dev` is matched
			// by SCRIPT NAME, and the repository decides what `dev` and `predev`
			// actually do; either can curl before anything binds a port. On Windows
			// the approval gate IS the network protection, so inferring "no egress"
			// from a name there lets that egress run unprompted.
			//
			// The classification is kept, because a scoped host-listener path will
			// want it, and the approval path is kept until that path exists.
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
	// A framework CLI stays conservative for Network whatever its subcommand.
	// Binding a port is not egress, but next, vite, nuxt and astro execute the
	// repository's config, plugins and application code during a build as
	// surely as during a serve, and nothing in the subcommand name proves that
	// code makes no requests. On Windows the approval gate IS the network
	// boundary, so an ordinary shell grant must not run `next build` without
	// the separate network decision. LocalServer alone carries the serve/build
	// distinction (commandRunsLocalServer); `python -m http.server` is decided
	// by pythonModuleUsesNetwork below, not by this inventory. Reported by
	// @gnanam1990.
	if localServerPrograms[prog] {
		return true
	}
	if pythonLauncherPrograms[prog] {
		return pythonModuleUsesNetwork(words)
	}
	switch prog {
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
	operands, eatable := commandOperands(words)
	for index := 0; index < subcommandPositions(operands, eatable); index++ {
		subcommand := operands[index]
		if alias, ok := aliases[subcommand]; ok {
			subcommand = alias
		}
		switch subcommand {
		case "install", "add", "ci", "create", "dlx", "publish", "unpublish",
			"login", "logout", "adduser", "whoami", "ping", "audit", "outdated",
			"update", "upgrade", "search", "view", "info", "show", "dist-tag",
			"deprecate", "owner", "org", "team", "token", "profile", "access":
			return true
		// start / serve / dev / preview are handled by packageManagerRunsLocalServer
		// instead. They start a dev server, which binds rather than fetches, and
		// `npm run dev` is the single most common command an agent is asked to run
		// while building something. Classifying it as egress made every one of them
		// stop for a network approval that protected nothing.
		case "exec":
			// Package-manager exec commands may resolve and download a missing
			// package before launching it. An explicit offline flag keeps this path
			// inside the isolated namespace; otherwise request egress up front.
			return true
		}
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
		// http.server is handled by pythonModuleRunsLocalServer instead: it
		// listens, it does not fetch. pip install genuinely reaches out.
		if words[index+1] == "pip" && firstSubcommand(words[index+2:], nil) == "install" {
			return true
		}
	}
	return false
}

// commandRunsLocalServer reports a command that binds a local port.
//
// Separate from commandUsesNetwork on purpose. A dev server is the single most
// common thing an agent is asked to start while building something, and making
// it indistinguishable from `curl` meant every one of them stopped for a
// network approval that protected nobody.
//
// Honest about the edges: some of these do touch the network incidentally, and
// `npm run dev` may install first. What is claimed here is narrow, that BINDING
// is not EGRESS, not that dev tooling is inert. Anything that actually fetches
// still matches commandUsesNetwork through its own program or subcommand.
func commandRunsLocalServer(prog string, args []*syntax.Word) bool {
	words := literalWordTexts(args)
	if localServerPrograms[prog] {
		return frameworkSubcommandRunsLocalServer(prog, words)
	}
	if pythonLauncherPrograms[prog] {
		return pythonModuleRunsLocalServer(words)
	}
	switch prog {
	case "npm", "pnpm", "yarn", "bun":
		return packageManagerRunsLocalServer(words)
	}
	return false
}

// frameworkSubcommandRunsLocalServer decides whether a framework CLI is being
// asked to SERVE or merely to compile.
//
// The program name alone is not enough. `next build`, `vite build`, `nuxt
// generate` and `astro check` bind nothing, and classifying them as servers
// makes the flag mean "some dev tool ran", which is not what a reader of it
// would assume.
//
// Dedicated servers stay unconditional: running http-server or serve IS the
// server. vite is also a server when bare, since bare `vite` starts the dev
// server, whereas the multi-command frameworks print help when bare and so need
// an explicit serving subcommand.
func frameworkSubcommandRunsLocalServer(prog string, words []string) bool {
	switch prog {
	case "http-server", "serve":
		return true
	}
	// The subcommand is not simply the first operand: a global option before it
	// consumes the next word (`vite --config vite.prod.ts build`), and reading
	// that value as the subcommand made the build fall through to the bare-vite
	// rule and count as a server. Walk the positions a subcommand can occupy,
	// the same way the package managers do; see subcommandPositions.
	operands, eatable := commandOperands(words)
	for index := 0; index < subcommandPositions(operands, eatable); index++ {
		switch operands[index] {
		case "dev", "start", "serve", "preview":
			return true
		case "build", "optimize", "generate", "check", "lint", "export":
			return false
		}
	}
	// No recognized subcommand at any position it could occupy: a bare
	// invocation, or only option values. Fall back to what the program does when
	// run bare: vite starts its dev server, while the multi-command frameworks
	// print help.
	return prog == "vite"
}

// packageManagerRunsLocalServer covers `npm run dev` and its siblings across the
// package managers, both as a direct subcommand and behind `run`, and wherever
// global options push the subcommand off its usual position. See
// subcommandPositions for why the subcommand is not simply the first operand.
func packageManagerRunsLocalServer(words []string) bool {
	operands, eatable := commandOperands(words)
	for index := 0; index < subcommandPositions(operands, eatable); index++ {
		if packageManagerServingScripts[operands[index]] {
			return true
		}
		if operands[index] == "run" && index+1 < len(operands) && packageManagerServingScripts[operands[index+1]] {
			return true
		}
	}
	return false
}

// commandOperands returns a command's non-flag words in order, and whether a
// flag came before the first of them. The program name is expected to be
// stripped already.
func commandOperands(words []string) (operands []string, eatable int) {
	// eatable counts the LEADING RUN of operands that each sit immediately after
	// a flag, and so might be that flag's value rather than the subcommand.
	// Adjacency is the whole rule: an option can only consume the word next to
	// it, and the first operand that is NOT preceded by a flag cannot be anyone's
	// value, so the run ends there and the subcommand is at or before it.
	previousWasFlag := false
	counting := true
	for _, word := range words {
		if word == "" {
			continue
		}
		if strings.HasPrefix(word, "-") {
			previousWasFlag = true
			continue
		}
		if isNumericToken(word) {
			previousWasFlag = false
			continue
		}
		if counting {
			if previousWasFlag {
				eatable++
			} else {
				counting = false
			}
		}
		operands = append(operands, word)
		previousWasFlag = false
	}
	return operands, eatable
}

// subcommandPositions reports how many leading operands could be the subcommand.
//
// Skipping flags is not enough, because it does not skip the words they CONSUME.
// `npm --prefix ./web run dev` then resolves to "./web" — the option's VALUE —
// and the invocation stops being recognized at all: the dev server loses the
// network approval its unflagged spelling gets, and `npm --prefix ./web install`
// loses it while genuinely fetching. That is not a contrived spelling; a global
// option before the subcommand is documented usage for every one of these tools
// (`npm --prefix`, `pnpm -C`, `yarn --cwd`, `bun --cwd`).
//
// Which options take a value is deliberately not enumerated. That is four tools
// each free to add one, and a list like that leaks silently the moment they do.
// ADJACENCY IS USED INSTEAD, because it is the actual grammar: an option can only
// consume the word immediately after it. Every leading operand that sits right
// after a flag might be that flag's value, so each one opens another position;
// the first operand NOT preceded by a flag cannot be anyone's value, and the
// window closes there.
//
// A FIXED WINDOW OF TWO WAS THE BUG. It covered exactly one value-taking option
// and no more, so a second one walked straight past it — measured before this
// change:
//
//	npm --prefix ./web install                    -> network (correct)
//	npm --prefix ./web --loglevel warn install    -> NOT network
//	npm --registry https://r.test --prefix ./web install -> NOT network
//	pnpm -C ./web --filter web add left-pad       -> NOT network
//
// all four genuinely fetching. Counting the flags rather than fixing the number
// closes the class at any arity without naming a single option.
//
// The false positives the old cap was protecting against are unaffected, because
// they turn on adjacency too: `npm run build --workspace dev` and `npm run test
// -- --grep start` both put an unflagged operand FIRST, so the window is one
// position and "dev" and "start" are never reached. That is a stronger guarantee
// than the cap gave — it held only because two happened to be short enough.
func subcommandPositions(operands []string, eatable int) int {
	if len(operands) == 0 {
		return 0
	}
	if positions := eatable + 1; positions < len(operands) {
		return positions
	}
	return len(operands)
}

func pythonModuleRunsLocalServer(words []string) bool {
	for index := 0; index < len(words); index++ {
		if words[index] != "-m" || index+1 >= len(words) {
			continue
		}
		if words[index+1] == "http.server" {
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
