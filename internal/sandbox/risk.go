package sandbox

import (
	"encoding/base64"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
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
	cmdForFSingleQuotedCommandPattern = regexp.MustCompile(`(?is)(?:^|[&|;\r\n]\s*)@?(?:call\s+)?for\s+/f\b(.*?)\bin\s*\(\s*'([^']*)'\s*\)\s+do\b`)
	cmdForFBacktickCommandPattern     = regexp.MustCompile("(?is)(?:^|[&|;\\r\\n]\\s*)@?(?:call\\s+)?for\\s+/f\\b(.*?)\\bin\\s*\\(\\s*`([^`]*)`\\s*\\)\\s+do\\b")
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

// maxUnparseableShellDepth bounds `sh -c <payload>` recursion in the fallback.
// It IS maxAnalyzerDepth rather than a copy of its value: a fallback that gave
// up a level earlier than the parseable path would drop the network category on
// exactly the deeply-nested launcher chains this path exists to fail closed on.
const maxUnparseableShellDepth = maxAnalyzerDepth

// commandResolution is the shared security result for executable argv and
// interpreter source. Unresolved input is distinct from a proven-local command
// and retains the network gate at every caller.
type commandResolution uint8

const (
	commandKnownLocal commandResolution = iota
	commandKnownNetwork
	commandUnresolved
)

func (resolution commandResolution) needsNetworkGate() bool {
	return resolution != commandKnownLocal
}

func commandNetworkResolution(network bool) commandResolution {
	if network {
		return commandKnownNetwork
	}
	return commandKnownLocal
}

func matchesUnparseableNetwork(command string) bool {
	return matchesUnparseableNetworkAt(command, 0)
}

// matchesUnparseableNetworkAt scans each fallback segment for a network program.
// It resolves the segment's real program the same way the parseable path does —
// past environment assignments and wrapper prefixes (sudo, env, timeout, nice,
// xargs, ...) and the option values those wrappers consume — because a fallback
// that only looked at the first token would let `sudo curl …`, `env git fetch …`,
// or `PATH=.:$PATH git push …` through. The point of this path is to fail closed
// on a command too obfuscated to parse; a wrapper prefix is the cheapest possible
// obfuscation.
//
// Resolving the program (rather than matching the network name anywhere in the
// string, as an earlier revision did) is what keeps `git status push` and
// `echo https://example.com/repo.git push` out: a network verb only counts when
// it belongs to a program actually being invoked.
func matchesUnparseableNetworkAt(command string, depth int) bool {
	if depth < maxUnparseableShellDepth {
		for _, payload := range fallbackCMDForFCommands(command) {
			if classifyInterpreterSource(payload, interpreterSourceCMD, depth+1).needsNetworkGate() {
				return true
			}
		}
	}
	command = maskFallbackCMDForFLiteralBackticks(command)
	for _, tokenInfo := range fallbackCommandTokenInfo(command) {
		tokens := fallbackTokenValues(tokenInfo)
		if depth < maxUnparseableShellDepth {
			if split := envSplitCommandFields(tokens); split.recognized {
				if split.executableEnvironmentDependent || split.commandSourceEnvironmentDependent ||
					fallbackBodyUsesNetwork(split.command, depth+1) {
					return true
				}
				continue
			}
		}
		for _, body := range fallbackCommandBodies(tokens) {
			if fallbackBodyUsesNetwork(body, depth) {
				return true
			}
		}
		for _, body := range cmdCommandBodyTokenInfoCandidates(tokenInfo) {
			// Shared with the AST path's cmdLauncherUsesNetwork so a quoted
			// payload cannot be command text on one path and a program name on
			// the other.
			if cmdBodyUsesNetwork(body, depth) {
				return true
			}
		}
	}
	// The segments above were cut under POSIX quoting. CMD honors only `"`, so
	// re-cut the same text under its rules before handing it to the CMD readers;
	// otherwise a quote form CMD ignores hides a separator it acts on.
	if posixSingleQuoteHidesCMDSeparator(command) {
		for _, tokenInfo := range fallbackCMDCommandTokenInfo(command) {
			for _, body := range cmdCommandBodyTokenInfoCandidates(tokenInfo) {
				if cmdBodyUsesNetwork(body, depth) {
					return true
				}
			}
		}
	}
	return false
}

// fallbackCommandBodies resolves the executable position of one segment under
// every command language the text may belong to. The fallback runs on input the
// POSIX parser rejected, and on Windows that input is frequently valid CMD
// source rather than broken sh — `@curl …`, `call curl …`, `if not 1==2 curl …`
// and `start "" curl …` all execute curl under cmd.exe while resolving to a
// non-program token ("@curl", "call", "not", "start") under POSIX rules. Each
// runtime's resolver therefore gets its own shot at the segment; a resolution
// can only ADD the network category, which is the direction this fail-closed
// path is allowed to err in.
func fallbackCommandBodies(tokens []string) [][]string {
	if body := fallbackCommandBodyFields(tokens); len(body) > 0 {
		return [][]string{body}
	}
	return nil
}

// fallbackBodyUsesNetwork classifies one resolved command body, recursing into
// the payloads of launchers that run command text of their own.
func fallbackBodyUsesNetwork(body []string, depth int) bool {
	return resolveCommandArgv(body, depth).needsNetworkGate()
}

// delegatedCommand selects a child argv suffix without rebuilding its tokens.
// The AST caller uses the suffix length to retain the original words (including
// expansions); the fallback caller retains its token text. Selection alone is
// not a local verdict: both callers must recursively resolve the child using
// the shared depth budget. A nil child means the launcher executes no command.
func delegatedCommand(program string, args []string) ([]string, commandResolution, bool) {
	switch program {
	case "busybox":
		child, status := busyboxDelegatedCommand(args)
		return child, status, true
	case "strace":
		child, status := straceDelegatedCommand(args)
		return child, status, true
	default:
		return nil, commandKnownLocal, false
	}
}

// resolveCommandArgv is the bounded, wrapper-aware resolver shared by the AST
// and fallback paths. It distinguishes a proven-local command from network
// access and from argv/source that the supported launcher grammars cannot
// resolve. Only a proven-local result may omit the network gate.
func resolveCommandArgv(body []string, depth int) commandResolution {
	if len(body) == 0 {
		return commandKnownLocal
	}
	if depth > maxAnalyzerDepth {
		return commandUnresolved
	}
	if split := envSplitCommandFields(body); split.recognized {
		if split.executableEnvironmentDependent || split.commandSourceEnvironmentDependent {
			return commandUnresolved
		}
		if len(split.command) == 0 {
			return commandKnownLocal
		}
		return resolveCommandArgv(split.command, depth+1)
	}
	if resolved := commandBodyFields(body); len(resolved) > 0 {
		body = resolved
	} else {
		return commandKnownLocal
	}
	program, args := executableTokenBase(body[0]), body[1:]
	if program == "%comspec%" {
		program = "cmd"
	}
	if fallbackTokenLooksDynamic(body[0]) {
		return commandUnresolved
	}
	if command, status, delegated := delegatedCommand(program, args); delegated {
		if status != commandKnownLocal {
			return status
		}
		if len(command) == 0 {
			return commandKnownLocal
		}
		return resolveCommandArgv(command, depth+1)
	}
	switch program {
	case "eval":
		if len(args) == 0 {
			return commandKnownLocal
		}
		return classifyInterpreterSource(strings.Join(args, " "), interpreterSourcePOSIX, depth+1)
	case "call":
		if len(args) == 0 {
			return commandKnownLocal
		}
		if fallbackTokenLooksDynamic(args[0]) {
			return commandUnresolved
		}
		return resolveCommandArgv(args, depth+1)
	}
	if shellPrograms[program] {
		index, found := shellCommandPayloadIndex(program, args)
		if !found || index >= len(args) {
			return commandKnownLocal
		}
		payload := args[index]
		if fallbackTokenLooksDynamic(payload) {
			return commandUnresolved
		}
		return classifyInterpreterSource(payload, interpreterSourcePOSIX, depth+1)
	}
	if program == "powershell" || program == "pwsh" {
		source := fallbackPowerShellPayload(program, args)
		switch {
		case source.opaque:
			return commandUnresolved
		case source.payload == "":
			return commandKnownLocal
		default:
			return classifyInterpreterSource(source.payload, interpreterSourcePowerShell, depth+1)
		}
	}
	if payload := fallbackCommandInterpreterPayload(program, args); payload != "" {
		resolution := classifyInterpreterSource(payload, interpreterSourceCMD, depth+1)
		if resolution.needsNetworkGate() {
			return resolution
		}
		if fallbackPayloadUsesNetwork(strings.Join(fallbackCommandInterpreterArgs(program, args), " "), depth+1) {
			return commandKnownNetwork
		}
		return commandKnownLocal
	}
	resolution := literalProgramNetworkResolution(program, args)
	if program == "git" && resolution == commandKnownLocal &&
		gitSelectionDynamic(args, func(index int) bool { return fallbackTokenLooksDynamic(args[index]) }) {
		// The fallback never expands these values, so a subcommand it cannot read
		// is not a subcommand it has proven local. See gitSelectionDynamic.
		return commandUnresolved
	}
	return resolution
}

// fallbackTokenLooksDynamic reports whether a token selected as executable
// source still contains an unresolved POSIX or CMD expansion. The fallback
// never expands these values, so treating the spelling as an ordinary unknown
// program would turn "cannot resolve" into "does not use the network."
func fallbackTokenLooksDynamic(token string) bool {
	if strings.ContainsAny(token, "$`") {
		return true
	}
	if containsCMDVariableExpansion(token, '%') || containsCMDVariableExpansion(token, '!') {
		return true
	}
	return containsCMDSingleDelimiterExpansion(token)
}

func containsCMDVariableExpansion(token string, delimiter byte) bool {
	for start := 0; start < len(token); start++ {
		if token[start] != delimiter {
			continue
		}
		endOffset := strings.IndexByte(token[start+1:], delimiter)
		if endOffset < 0 {
			return false
		}
		content := token[start+1 : start+1+endOffset]
		if delimiter == '%' {
			if colon := strings.IndexByte(content, ':'); colon >= 0 {
				content = content[:colon]
			}
		}
		if validCMDVariableName(content) {
			return true
		}
		start += endOffset + 1
	}
	return false
}

func validCMDVariableName(name string) bool {
	if name == "" {
		return false
	}
	for index, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' ||
			(index > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

// containsCMDSingleDelimiterExpansion reports whether a token IS a CMD
// expansion written with one leading percent rather than a matched pair: a FOR
// metavariable (%i, the batch %%i spelling, and the ~-modified %~dpi forms) or
// a batch parameter (%1). containsCMDVariableExpansion cannot see these,
// because it looks for %NAME% and these have no closing delimiter.
//
// CMD substitutes them before launching the command, so in an EXECUTABLE
// position the spelling says nothing about what runs: `for /f %i in (list.txt)
// do %i https://host` executes whatever list.txt names. Reading %i as an
// ordinary unknown program turned "cannot resolve" into "proven local" and
// dropped the network gate.
//
// The reference must BE the token, not merely appear inside it, and the name
// is a single character. A metavariable name is one char by CMD's grammar, so
// `%local` is `%l` followed by literal text rather than a reference, and
// `100%local` does not start with the sigil at all — both are ordinary program
// spellings that keep their proven-local answer. Only executable and
// interpreter-source positions consult this (see the callers of
// fallbackTokenLooksDynamic), so literal FOR set data and an echo-style body's
// arguments keep their precision too: `for %i in (*.txt) do echo %i` still
// resolves through the literal program `echo`.
func containsCMDSingleDelimiterExpansion(token string) bool {
	if !strings.HasPrefix(token, "%") {
		return false
	}
	rest := token[1:]
	// A batch file doubles the sigil (%%i); both spellings reach the same
	// substitution, so step over the second one and read the name.
	rest = strings.TrimPrefix(rest, "%")
	if rest == "" {
		return false
	}
	// %~dpi and friends: the modifier run only ever introduces a metavariable
	// reference, so the '~' alone is conclusive.
	if rest[0] == '~' {
		return true
	}
	if !isCMDMetavariableName(rest[0]) {
		return false
	}
	// One character names the variable; anything else that could continue a
	// name means this token is literal text, not a reference.
	return len(rest) == 1 || !isCMDMetavariableName(rest[1])
}

// isCMDMetavariableName reports whether c can name a FOR metavariable or a
// batch parameter. CMD accepts any single alphanumeric character here, and the
// name is case-sensitive (%i and %I are different variables).
func isCMDMetavariableName(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// fallbackPayloadUsesNetwork classifies CMD payload text, so it reads the text
// under CMD's quoting as well as the shared POSIX tokenization — a `'` CMD
// treats as ordinary text must not hide the separator that follows it.
func fallbackPayloadUsesNetwork(payload string, depth int) bool {
	tokenizations := [][][]fallbackCommandToken{fallbackCommandTokenInfo(payload)}
	if posixSingleQuoteHidesCMDSeparator(payload) {
		tokenizations = append(tokenizations, fallbackCMDCommandTokenInfo(payload))
	}
	for _, tokenInfos := range tokenizations {
		for _, tokenInfo := range tokenInfos {
			if len(tokenInfo) > 0 && strings.EqualFold(tokenInfo[0].value, "start") {
				if body := cmdStartPayloadTokenInfo(tokenInfo[1:]); len(body) > 0 && fallbackBodyUsesNetwork(fallbackTokenValues(body), depth) {
					return true
				}
			}
			for _, body := range cmdCommandBodyTokenInfoCandidates(tokenInfo) {
				if fallbackBodyUsesNetwork(fallbackTokenValues(body), depth) {
					return true
				}
			}
		}
	}
	return false
}

var fallbackLeadingShellKeywords = map[string]bool{
	"!": true, "if": true, "while": true, "until": true, "then": true,
	"do": true, "else": true, "elif": true, "in": true, "coproc": true,
	"{": true, "}": true,
}

// fallbackCommandBodyFields resolves a command at a shell-control boundary.
// In addition to ordinary wrappers, an unparseable compound command can put
// control-flow keywords and redirections before the executable (`then curl`,
// `do wget`, `>out git push`). Those prefixes do not change the program being
// invoked and must not make the fail-closed network check lose sight of it.
func fallbackCommandBodyFields(fields []string) []string {
	// Shell keywords are syntax only at the original command boundary. Do not
	// reinterpret a wrapper's payload as syntax: `command if curl` attempts to
	// execute a program named "if" and does not invoke curl.
	for len(fields) > 0 && fallbackLeadingShellKeywords[strings.ToLower(fields[0])] {
		fields = fields[1:]
	}
	body := commandBodyFields(fields)
	for len(body) > 0 {
		word := strings.ToLower(body[0])
		if consumesRedirectTarget(word) {
			if len(body) == 1 {
				return nil
			}
			body = commandBodyFields(body[2:])
			continue
		}
		if isRedirectToken(word) {
			body = commandBodyFields(body[1:])
			continue
		}
		return body
	}
	return nil
}

// cmdComparisonOperators are CMD's IF comparison operators, which sit between
// the two values being compared (`if %x% equ 1 curl …`).
var cmdComparisonOperators = map[string]bool{
	"equ": true, "neq": true, "lss": true, "leq": true, "gtr": true, "geq": true,
}

type fallbackCommandToken struct {
	value  string
	quoted bool
}

func fallbackTokenValues(tokens []fallbackCommandToken) []string {
	values := make([]string, len(tokens))
	for index := range tokens {
		values[index] = tokens[index].value
	}
	return values
}

func cmdCommandBodyTokenInfoCandidates(fields []fallbackCommandToken) [][]fallbackCommandToken {
	fields = normalizeCMDCommandTokens(fields)
	for len(fields) > 0 {
		fields = trimCMDEchoPrefix(fields)
		if len(fields) == 0 {
			return nil
		}
		switch strings.ToLower(fields[0].value) {
		case "call", "else", "then", "do":
			fields = fields[1:]
		case "cmd", "cmd.exe", "%comspec%":
			payload := cmdInterpreterPayloadTokenInfo(fields[1:])
			if len(payload) == 0 {
				return nil
			}
			fields = payload
		case "start":
			body := cmdStartPayloadTokenInfo(fields[1:])
			if len(body) == 0 {
				return nil
			}
			body = trimCMDEchoPrefix(body)
			if len(body) == 0 {
				return nil
			}
			return [][]fallbackCommandToken{body}
		case "if":
			values := cmdConditionPayload(fallbackTokenValues(fields[1:]))
			if len(values) == 0 {
				return nil
			}
			fields = fields[len(fields)-len(values):]
		case "for":
			values := cmdForPayload(fallbackTokenValues(fields[1:]))
			if len(values) == 0 {
				return nil
			}
			fields = fields[len(fields)-len(values):]
		default:
			return [][]fallbackCommandToken{fields}
		}
	}
	return nil
}

func cmdInterpreterPayloadTokenInfo(fields []fallbackCommandToken) []fallbackCommandToken {
	for index, field := range fields {
		flag := strings.ToLower(field.value)
		if !strings.HasPrefix(flag, "/c") && !strings.HasPrefix(flag, "/k") {
			continue
		}
		if len(field.value) > 2 {
			joined := field
			joined.value = field.value[2:]
			return append([]fallbackCommandToken{joined}, fields[index+1:]...)
		}
		index++
		for index < len(fields) && isCMDInterpreterSwitch(fields[index].value) {
			index++
		}
		return fields[index:]
	}
	return nil
}

func normalizeCMDCommandTokens(fields []fallbackCommandToken) []fallbackCommandToken {
	normalized := make([]fallbackCommandToken, 0, len(fields))
	for _, field := range fields {
		if field.quoted {
			normalized = append(normalized, field)
			continue
		}
		// The shared tokenizer reads `\` as a POSIX escape, so a Windows path that
		// ends in a separator swallows the following word: `start "" /d C:\ curl …`
		// arrives as the single token `C:\ curl`, which lets a value-taking switch
		// consume the executable. CMD has no such escape, so split those apart
		// before resolving CMD's own grammar.
		for _, part := range splitCMDEscapedWhitespace(field.value) {
			normalized = append(normalized, fallbackCommandToken{value: normalizeCMDToken(part)})
		}
	}
	return normalized
}

func splitCMDEscapedWhitespace(value string) []string {
	var parts []string
	start := 0
	for index := 0; index+1 < len(value); index++ {
		if value[index] != '\\' || (value[index+1] != ' ' && value[index+1] != '\t') {
			continue
		}
		if part := value[start : index+1]; part != "" {
			parts = append(parts, part)
		}
		start = index + 2
		index++
	}
	if part := value[start:]; part != "" || len(parts) == 0 {
		parts = append(parts, part)
	}
	return parts
}

// trimCMDEchoPrefixToken strips CMD's echo-suppression prefix from a single
// program token. It is the one-token form of trimCMDEchoPrefix, shared so the
// AST and fallback paths cannot disagree about whether "@curl" names curl.
func trimCMDEchoPrefixToken(token string) string {
	return strings.TrimLeft(strings.TrimSpace(token), "@")
}

func trimCMDEchoPrefix(fields []fallbackCommandToken) []fallbackCommandToken {
	for len(fields) > 0 {
		fields[0].value = strings.TrimLeft(fields[0].value, "@")
		if fields[0].value != "" {
			break
		}
		fields = fields[1:]
	}
	return fields
}

// cmdStartPayloadTokenInfo resolves the command START will launch. START's
// grammar is switches, an optional quoted window title, then more switches:
// `start "" /b curl …` is a real invocation of curl. A single prefix strip left
// `/b` in the executable position, so neither the CMD nor the POSIX resolver
// contributed the network category for that form.
func cmdStartPayloadTokenInfo(fields []fallbackCommandToken) []fallbackCommandToken {
	fields = cmdStartOperandsTokenInfo(fields)
	if len(fields) > 0 && fields[0].quoted {
		fields = cmdStartOperandsTokenInfo(fields[1:])
	}
	return fields
}

func cmdStartOperandsTokenInfo(fields []fallbackCommandToken) []fallbackCommandToken {
	for len(fields) > 0 && strings.HasPrefix(fields[0].value, "/") {
		switch strings.ToLower(fields[0].value) {
		case "/d", "/node", "/affinity":
			if len(fields) < 2 {
				return nil
			}
			fields = fields[2:]
		default:
			fields = fields[1:]
		}
	}
	return fields
}

// cmdConditionPayload skips an IF condition and returns the guarded command.
func cmdConditionPayload(fields []string) []string {
	for len(fields) > 0 {
		word := strings.ToLower(strings.Trim(fields[0], `"`))
		switch {
		case strings.HasPrefix(word, "/"): // /I, /D — case-insensitivity switches
			fields = fields[1:]
		case word == "not":
			fields = fields[1:]
		case word == "errorlevel" || word == "exist" || word == "defined" || word == "cmdextversion":
			// Keyword plus its single operand.
			if len(fields) < 2 {
				return nil
			}
			return fields[2:]
		case len(fields) >= 2 && (fields[1] == "==" || cmdComparisonOperators[strings.ToLower(fields[1])]):
			// A spaced comparison: <value> <operator> <value>.
			if len(fields) < 3 {
				return nil
			}
			return fields[3:]
		case strings.Contains(word, "=="):
			// The whole comparison arrived as one token (`1==1`).
			return fields[1:]
		default:
			// Unknown condition grammar cannot establish a command boundary.
			return nil
		}
	}
	return nil
}

// cmdForPayload returns the command after DO, which is the only part of a FOR
// loop that executes. `for %i in (curl) do echo %i` must not resolve to curl.
//
// DO counts only OUTSIDE the IN (...) set. `do` is an ordinary word inside it,
// and CMD accepts it there: in `for %i in (do x) do curl https://evil.test` the
// first `do` is a set ELEMENT, so selecting it resolved the classifier from `x`
// and never reached the curl that actually runs. Tracking parenthesis depth
// picks the loop keyword instead, and an unbalanced set — which CMD itself
// rejects — yields no body rather than a guessed one.
func cmdForPayload(fields []string) []string {
	depth := 0
	quoted := false
	for index, field := range fields {
		if depth == 0 && strings.EqualFold(strings.Trim(field, `"`), "do") {
			return fields[index+1:]
		}
		depth, quoted = cmdParenDepth(field, depth, quoted)
	}
	return nil
}

// cmdParenDepth advances the parenthesis depth across one token, ignoring
// parentheses inside double quotes. Quote state is carried between tokens
// because a quoted run with a space in it can span them.
func cmdParenDepth(field string, depth int, quoted bool) (int, bool) {
	for _, r := range field {
		switch {
		case r == '"':
			quoted = !quoted
		case quoted:
		case r == '(':
			depth++
		case r == ')':
			if depth > 0 {
				depth--
			}
		}
	}
	return depth, quoted
}

// fallbackCMDForFCommands extracts the command source that CMD FOR /F executes
// from its IN clause. Without usebackq, single quotes denote command text; with
// usebackq, backticks do. The other quote form is literal input and must not be
// classified as an invocation.
func fallbackCMDForFCommands(command string) []string {
	var payloads []string
	commandBoundaries := fallbackCMDCommandBoundaries(command)
	for _, indexes := range cmdForFSingleQuotedCommandPattern.FindAllStringSubmatchIndex(command, -1) {
		if len(indexes) < 6 || !commandBoundaries[indexes[0]] {
			continue
		}
		match := []string{command[indexes[0]:indexes[1]], command[indexes[2]:indexes[3]], command[indexes[4]:indexes[5]]}
		if len(match) == 3 && !cmdForFUsesBackq(match[1]) {
			payloads = append(payloads, match[2])
		}
	}
	for _, indexes := range cmdForFBacktickCommandPattern.FindAllStringSubmatchIndex(command, -1) {
		if len(indexes) < 6 || !commandBoundaries[indexes[0]] {
			continue
		}
		match := []string{command[indexes[0]:indexes[1]], command[indexes[2]:indexes[3]], command[indexes[4]:indexes[5]]}
		if len(match) == 3 && cmdForFUsesBackq(match[1]) {
			payloads = append(payloads, match[2])
		}
	}
	return payloads
}

func cmdForFUsesBackq(options string) bool {
	for _, option := range strings.Fields(strings.Trim(options, ` "`)) {
		if strings.EqualFold(strings.Trim(option, `"`), "usebackq") {
			return true
		}
	}
	return false
}

func maskFallbackCMDForFLiteralBackticks(command string) string {
	masked := []byte(command)
	commandBoundaries := fallbackCMDCommandBoundaries(command)
	for _, indexes := range cmdForFBacktickCommandPattern.FindAllStringSubmatchIndex(command, -1) {
		if len(indexes) < 6 || !commandBoundaries[indexes[0]] ||
			cmdForFUsesBackq(command[indexes[2]:indexes[3]]) {
			continue
		}
		for index := indexes[4]; index < indexes[5]; index++ {
			masked[index] = ' '
		}
	}
	return string(masked)
}

// fallbackCMDCommandBoundaries records whether each byte offset is outside a
// quoted or caret-escaped CMD region. FOR /F regex matches can then check their
// start in O(1) instead of rescanning every preceding byte for every match.
func fallbackCMDCommandBoundaries(command string) []bool {
	boundaries := make([]bool, len(command)+1)
	quoted := false
	escaped := false
	for index := 0; index < len(command); index++ {
		boundaries[index] = !quoted && !escaped
		switch {
		case escaped:
			escaped = false
		case command[index] == '^':
			escaped = true
		case command[index] == '"':
			quoted = !quoted
		}
	}
	boundaries[len(command)] = !quoted && !escaped
	return boundaries
}

func consumesRedirectTarget(word string) bool {
	word = strings.TrimLeft(word, "0123456789")
	return word == ">" || word == ">>" || word == "<" || word == "<<" || word == "<<-" ||
		word == "<<<" || word == "<>" || word == ">|"
}

func isRedirectToken(word string) bool {
	word = strings.TrimLeft(word, "0123456789")
	return strings.HasPrefix(word, ">") || strings.HasPrefix(word, "<")
}

// shellCommandPayloadIndex returns the one argv element a shell launcher will
// execute for -c. It parses only the leading option region: a script operand or
// `--` makes later `-c` text positional, and an invalid option cluster is not
// treated as executable source.
func shellCommandPayloadIndex(program string, args []string) (int, bool) {
	noExecute, dumpStrings := false, false
	for index := 0; index < len(args); index++ {
		option := args[index]
		if option == "--" || option == "-" || option == "+" ||
			(!strings.HasPrefix(option, "-") && !strings.HasPrefix(option, "+")) {
			return 0, false
		}
		if option == "--command" {
			// bash, sh and dash reject --command as an invalid option and run
			// nothing, but it is still read as the command flag, as #1066 does:
			// classifying a payload that never runs costs nothing, and it covers
			// any shell build that accepts the long form.
			if noExecute || dumpStrings {
				return 0, false
			}
			return shellCommandSourceIndex(args, index)
		}
		if strings.HasPrefix(option, "--") {
			if program != "bash" {
				return 0, false
			}
			name := option
			if equals := strings.IndexByte(option, '='); equals >= 0 {
				name = option[:equals]
			}
			switch name {
			case "--dump-po-strings", "--dump-strings":
				return 0, false
			case "--debug", "--debugger", "--login", "--noediting", "--noprofile", "--norc", "--posix",
				"--pretty-print", "--restricted", "--verbose":
				if name != option {
					return 0, false
				}
			case "--init-file", "--rcfile":
				if name == option {
					index++
					if index >= len(args) {
						return 0, false
					}
				}
			case "--help", "--version":
				return 0, false
			default:
				return 0, false
			}
			continue
		}

		validOptions := "abefhkmnptuvxBCEHPTilrsDc"
		valueOptions := "o"
		switch program {
		case "bash":
			valueOptions += "O"
		case "dash", "sh":
			validOptions = "abCefnuvxIimslpc"
		default:
			// ksh/zsh share the common invocation flags used here, including -l.
			validOptions = "abefhkmnptuvxBCilrsc"
		}
		command, values := false, 0
		enable := option[0] == '-'
		for _, flag := range option[1:] {
			switch {
			case flag == 'c':
				command = true
			case flag == 'n':
				noExecute = enable
			case program == "bash" && flag == 'D':
				dumpStrings = true
			case strings.ContainsRune(valueOptions, flag):
				values++
			case strings.ContainsRune(validOptions, flag):
			default:
				return 0, false
			}
		}
		index += values
		if index >= len(args) {
			return 0, false
		}
		if command {
			if noExecute || dumpStrings {
				return 0, false
			}
			return shellCommandSourceIndex(args, index)
		}
	}
	return 0, false
}

// shellCommandSourceIndex returns the index of the command string that follows
// the command flag at args[flagIndex], skipping one `--` between them.
func shellCommandSourceIndex(args []string, flagIndex int) (int, bool) {
	sourceIndex := flagIndex + 1
	if sourceIndex < len(args) && args[sourceIndex] == "--" {
		sourceIndex++
	}
	if sourceIndex >= len(args) {
		return 0, false
	}
	return sourceIndex, true
}

func fallbackCommandInterpreterPayload(program string, args []string) string {
	for index, arg := range args {
		flag := strings.ToLower(arg)
		if program == "cmd" && (strings.HasPrefix(flag, "/c") || strings.HasPrefix(flag, "/k")) {
			if len(arg) > 2 {
				return strings.TrimSpace(strings.Join(append([]string{arg[2:]}, args[index+1:]...), " "))
			}
			index++
			for index < len(args) && isCMDInterpreterSwitch(args[index]) {
				index++
			}
			return strings.Join(args[index:], " ")
		}
	}
	if program == "powershell" || program == "pwsh" {
		return fallbackPowerShellPayload(program, args).payload
	}
	return ""
}

func fallbackCommandInterpreterArgs(program string, args []string) []string {
	for index, arg := range args {
		flag := strings.ToLower(arg)
		if program == "cmd" && (strings.HasPrefix(flag, "/c") || strings.HasPrefix(flag, "/k")) {
			if len(arg) > 2 {
				return append([]string{arg[2:]}, args[index+1:]...)
			}
			index++
			for index < len(args) && isCMDInterpreterSwitch(args[index]) {
				index++
			}
			return args[index:]
		}
	}
	return nil
}

func isCMDInterpreterSwitch(arg string) bool {
	flag := strings.ToLower(arg)
	switch flag {
	case "/d", "/s", "/q", "/a", "/u":
		return true
	default:
		return strings.HasPrefix(flag, "/e:") || strings.HasPrefix(flag, "/f:") || strings.HasPrefix(flag, "/v:")
	}
}

// powerShellPayload describes what a PowerShell host invocation will execute.
//
// The three outcomes are distinct and must not be collapsed: readable source
// (payload), source that exists but cannot be read statically (opaque), and no
// inline source at all (a script File, a version query, an invalid switch).
// Treating the middle case as the last one is how `-EncodedCommand <valid
// base64 of Invoke-WebRequest …>` parsed cleanly and was allowed without a
// network grant.
type powerShellPayload struct {
	payload string
	opaque  bool
	// sourceIndex is where the command source begins in args, or -1 when the
	// invocation carries none. Callers on the AST path use it to check whether
	// the source words are static literals.
	sourceIndex int
}

// maxPowerShellEncodedCommandBytes bounds the attacker-controlled base64 this
// path will decode.
const maxPowerShellEncodedCommandBytes = 64 << 10

func fallbackPowerShellPayload(program string, args []string) powerShellPayload {
	none := powerShellPayload{sourceIndex: -1}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if !strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "/") {
			// Windows PowerShell's first positional parameter is Command. PowerShell
			// 6+ changed pwsh's default to File, a script path rather than source.
			if program == "pwsh" {
				return none
			}
			return powerShellPayload{payload: strings.Join(args[index:], " "), sourceIndex: index}
		}
		flag := strings.TrimLeft(strings.ToLower(arg), "-/")
		if strings.Contains(flag, ":") {
			// The native host does not accept colon-joined option values.
			return none
		}
		switch {
		case program == "pwsh" && (flag == "commandwithargs" || flag == "cwa"):
			if index+1 < len(args) {
				return powerShellPayload{payload: args[index+1], sourceIndex: index + 1}
			}
			return none
		case powerShellFlagAbbreviates(flag, "command"):
			if index+1 < len(args) {
				return powerShellPayload{payload: strings.Join(args[index+1:], " "), sourceIndex: index + 1}
			}
			return none
		case flag == "ec" || powerShellFlagAbbreviates(flag, "encodedcommand"):
			if index+1 >= len(args) {
				return none
			}
			// Encoded source is still source. Decode the bounded, valid forms so a
			// benign payload stays quiet, and fail closed on anything that does not
			// decode rather than reading it as network-free.
			if decoded, ok := decodePowerShellEncodedCommand(args[index+1]); ok {
				return powerShellPayload{payload: decoded, sourceIndex: index + 1}
			}
			return powerShellPayload{opaque: true, sourceIndex: index + 1}
		case powerShellFlagAbbreviates(flag, "file"):
			// A script path, not inline source: nothing here to classify.
			return none
		case program == "pwsh" && (flag == "v" || flag == "version"):
			return none
		case powerShellOptionConsumesValue(program, flag):
			if index+1 >= len(args) {
				return none
			}
			index++
		case powerShellValuelessOption(program, flag):
		case flag == "h" || flag == "help" || flag == "?":
			return none
		default:
			// Unknown host switches terminate option parsing with an error; do not
			// reinterpret a later network-looking argument as PowerShell source.
			return none
		}
	}
	return none
}

// decodePowerShellEncodedCommand decodes the base64 UTF-16LE source carried by
// -EncodedCommand. A value that is not valid base64, not an even number of
// bytes, or larger than the bound is not decodable here and is reported as
// such so the caller can fail closed.
func decodePowerShellEncodedCommand(value string) (string, bool) {
	value = strings.Trim(value, `"'`)
	if value == "" || len(value) > maxPowerShellEncodedCommandBytes {
		return "", false
	}
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return "", false
	}
	if len(raw) == 0 || len(raw)%2 != 0 {
		return "", false
	}
	units := make([]uint16, 0, len(raw)/2)
	for index := 0; index+1 < len(raw); index += 2 {
		units = append(units, uint16(raw[index])|uint16(raw[index+1])<<8)
	}
	decoded := string(utf16.Decode(units))
	if strings.ContainsRune(decoded, 0) || !utf8.ValidString(decoded) {
		return "", false
	}
	return decoded, true
}

func powerShellFlagAbbreviates(flag, fullName string) bool {
	return flag != "" && strings.HasPrefix(fullName, flag)
}

func powerShellOptionConsumesValue(program, flag string) bool {
	switch flag {
	case "configurationfile", "configurationname", "config", "custompipename",
		"encodedarguments", "ea",
		"executionpolicy", "ex", "ep", "inputformat", "inp", "if",
		"outputformat", "o", "of", "psconsolefile", "settingsfile", "settings",
		"windowstyle", "w", "workingdirectory", "wd", "wo":
		return true
	case "version", "v":
		return program == "powershell"
	default:
		return false
	}
}

func powerShellValuelessOption(program, flag string) bool {
	switch flag {
	case "mta", "sta", "noexit", "noe", "nologo", "nol", "noninteractive", "noni",
		"noprofile", "nop":
		return true
	case "interactive", "login", "noprofileloadtime", "sshservermode":
		return program == "pwsh"
	default:
		return false
	}
}

func normalizeCMDToken(token string) string {
	var out strings.Builder
	for index := 0; index < len(token); index++ {
		if token[index] == '^' && index+1 < len(token) {
			index++
		}
		out.WriteByte(token[index])
	}
	return out.String()
}

// executableTokenBase reduces a raw fallback token to a comparable program name.
// It strips quoting, any directory prefix, and a Windows executable suffix, so
// this path recognizes curl.exe and git.cmd exactly as normalizeProgramToken does
// on the parseable path — a token that normalized differently here used to be how
// `curl.exe https://… && "unterminated` lost its network classification.
//
// Drive-relative spellings go through windowsExecutablePathBasename for the same
// reason: `C:git.exe` has no separator to cut on, so a plain basename scan leaves
// `c:git` and the deny never matches a program the parseable path classifies.
func executableTokenBase(token string) string {
	token = strings.Trim(token, `\"'`)
	if basename, ok := windowsExecutablePathBasename(token); ok {
		token = basename
	} else if slash := strings.LastIndexAny(token, `/\`); slash >= 0 {
		token = token[slash+1:]
	}
	return trimExecutableSuffix(strings.ToLower(token))
}

type fallbackDelimiterFrame struct {
	opener rune
	quote  rune
}

// fallbackCommandTokens performs deliberately small shell/cmd tokenization.
// It preserves quoted spaces even when the command's trailing quote is
// unmatched (the condition that sends classification down this fallback).
func fallbackCommandTokens(command string) [][]string {
	infos := fallbackCommandTokenInfo(command)
	commands := make([][]string, len(infos))
	for index := range infos {
		commands[index] = fallbackTokenValues(infos[index])
	}
	return commands
}

func fallbackCommandTokenInfo(command string) [][]fallbackCommandToken {
	return fallbackCommandTokenInfoIn(command, false)
}

// fallbackCMDCommandTokenInfo tokenizes the same text under CMD's quoting rules,
// where `"` is the ONLY quote character.
//
// The POSIX reading of `echo ' & curl https://evil.test` puts `& curl …` inside
// the echo argument, so the segment resolves to echo and classifies local. CMD
// has no single-quote quoting at all: the `'` is literal text, `&` is a command
// separator, and curl runs. The same holds for backticks, which execute nothing
// under CMD. Any text handed to a CMD reader is tokenized both ways so a quote
// form one runtime ignores cannot hide a separator from the other.
func fallbackCMDCommandTokenInfo(command string) [][]fallbackCommandToken {
	return fallbackCommandTokenInfoIn(command, true)
}

// posixSingleQuoteHidesCMDSeparator reports the one disagreement that matters:
// a POSIX single-quoted region — quoted to this tokenizer, ordinary text to CMD
// — containing a CMD command separator. In `echo ' & curl https://evil.test`
// the quote swallows `& curl …` into echo's argument, while cmd.exe runs curl.
//
// Merely containing a `'` is not enough to justify re-reading the text: POSIX
// constructs whose quoted regions hide nothing (`env -S 'printf ok' …`) would
// re-segment into shapes that mean nothing under CMD, and a fail-closed pass
// that fires on them is just noise. `;` is deliberately not a separator here —
// CMD treats it as an argument delimiter, not a command boundary. Double quotes
// quote under both languages, so they hide nothing from either.
func posixSingleQuoteHidesCMDSeparator(command string) bool {
	escaped := false
	var quote rune
	for _, r := range command {
		switch {
		case escaped:
			escaped = false
		case r == '\\':
			escaped = true
		case quote == 0 && (r == '\'' || r == '"'):
			quote = r
		case quote == r:
			quote = 0
		case quote == '\'' && (r == '&' || r == '|'):
			return true
		}
	}
	return false
}

func fallbackCommandTokenInfoIn(command string, cmdQuoting bool) [][]fallbackCommandToken {
	// Command strings commonly preserve cmd.exe's escaped quote spelling.
	command = strings.ReplaceAll(command, `\"`, `"`)
	var commands [][]fallbackCommandToken
	var tokens []fallbackCommandToken
	var word strings.Builder
	var quote rune
	wordQuoted := false
	var delimiters []fallbackDelimiterFrame
	backtick := false
	escaped := false
	flush := func() {
		if word.Len() > 0 || wordQuoted {
			tokens = append(tokens, fallbackCommandToken{value: word.String(), quoted: wordQuoted})
			word.Reset()
			wordQuoted = false
		}
	}
	flushCommand := func() {
		flush()
		if len(tokens) > 0 {
			commands = append(commands, tokens)
			tokens = nil
		}
	}
	runes := []rune(command)
	for index, r := range runes {
		if escaped {
			word.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			word.WriteRune(r)
			escaped = true
			continue
		}
		// Backticks execute inside unquoted and double-quoted text, but are
		// literal inside single quotes. Preserve the surrounding quote while the
		// substitution body is scanned as its own command. CMD has no command
		// substitution, so under its rules a backtick is ordinary text.
		if r == '`' && quote != '\'' && !cmdQuoting {
			flushCommand()
			if backtick {
				if len(delimiters) > 0 && delimiters[len(delimiters)-1].opener == '`' {
					quote = delimiters[len(delimiters)-1].quote
					delimiters = delimiters[:len(delimiters)-1]
				}
			} else {
				delimiters = append(delimiters, fallbackDelimiterFrame{opener: '`', quote: quote})
				quote = 0
			}
			backtick = !backtick
			continue
		}
		if r == '\'' && cmdQuoting {
			// Literal text to CMD, and deliberately NOT a word boundary: the
			// point of this pass is that the quote hides nothing.
			word.WriteRune(r)
			continue
		}
		if r == '\'' || r == '"' {
			switch quote {
			case 0:
				quote = r
				wordQuoted = true
			case r:
				quote = 0
			default:
				word.WriteRune(r)
			}
			continue
		}
		// A verified function declaration makes its brace or subshell body a new
		// executable region. Without this boundary, `f(){ curl ...; }; f` resolves
		// the whole segment to the syntax token `f()` and hides the body.
		if r == '{' && quote == 0 && fallbackFunctionDeclaration(tokens, word.String()) {
			flushCommand()
			continue
		}
		// Command and process substitutions execute even inside double quotes.
		// Ordinary/arithmetic/array parentheses do not: splitting all parens made
		// `${curl}`, `$((curl))`, and `arr=(curl)` look like curl invocations.
		if r == '(' && quote != '\'' {
			current := word.String()
			if quote == 0 && current == "" && fallbackFunctionDeclaration(tokens, "") {
				flushCommand()
				delimiters = append(delimiters, fallbackDelimiterFrame{opener: '(', quote: 0})
				continue
			}
			nextIsParen := index+1 < len(runes) && runes[index+1] == '('
			substitution := (strings.HasSuffix(current, "$") && !nextIsParen) ||
				strings.HasSuffix(current, "<") || strings.HasSuffix(current, ">")
			// CMD conditionals put the command group after condition tokens, e.g.
			// `if 1==1 (curl ...)`; the opening parenthesis is still a command
			// boundary even though it is not the segment's first token.
			grouping := quote == 0 && word.Len() == 0 &&
				(len(tokens) == 0 || startsCMDCommandGroup(fallbackTokenValues(tokens)))
			if substitution || grouping {
				flushCommand()
				delimiters = append(delimiters, fallbackDelimiterFrame{opener: '(', quote: quote})
				quote = 0
				continue
			}
		}
		if r == ')' && quote == 0 && len(delimiters) > 0 && delimiters[len(delimiters)-1].opener == '(' {
			flushCommand()
			quote = delimiters[len(delimiters)-1].quote
			delimiters = delimiters[:len(delimiters)-1]
			continue
		}
		// A case pattern's closing parenthesis starts the command body. It is not
		// paired with an opening command-group parenthesis.
		if r == ')' && quote == 0 && len(tokens) > 0 && tokens[0].value == "case" {
			flushCommand()
			continue
		}
		// A newline separates commands exactly as ;/&/| do. Treating it as mere
		// whitespace kept a multi-line script as one segment, so anything after the
		// first line was scanned as arguments of the first line's program and the
		// program on line two was never resolved.
		if quote == 0 && (r == ';' || r == '&' || r == '|' || r == '\n' || r == '\r') {
			flushCommand()
			continue
		}
		if quote == 0 && (r == ' ' || r == '\t') {
			flush()
			continue
		}
		word.WriteRune(r)
	}
	flushCommand()
	return commands
}

func fallbackFunctionDeclaration(tokens []fallbackCommandToken, current string) bool {
	fields := fallbackTokenValues(tokens)
	if current != "" {
		fields = append(fields, current)
	}
	switch len(fields) {
	case 1:
		return fallbackFunctionNameToken(fields[0])
	case 2:
		return (isShellFunctionName(fields[0]) && fields[1] == "()") ||
			(strings.EqualFold(fields[0], "function") &&
				(fallbackFunctionNameToken(fields[1]) || isShellFunctionName(fields[1])))
	case 3:
		return strings.EqualFold(fields[0], "function") && isShellFunctionName(fields[1]) && fields[2] == "()"
	default:
		return false
	}
}

func fallbackFunctionNameToken(token string) bool {
	return strings.HasSuffix(token, "()") && isShellFunctionName(strings.TrimSuffix(token, "()"))
}

func isShellFunctionName(name string) bool {
	if name == "" {
		return false
	}
	for index, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || (index > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

func startsCMDCommandGroup(tokens []string) bool {
	if len(tokens) == 0 {
		return false
	}
	switch strings.TrimPrefix(strings.ToLower(tokens[0]), "@") {
	case "if", "else":
		return true
	case "for":
		// Parentheses after IN contain data; only the group after DO executes.
		return strings.EqualFold(tokens[len(tokens)-1], "do")
	default:
		return false
	}
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

func requestPaths(request Request) []string {
	paths := []string{}
	// Keep this aligned with the path-arg alias lists the tools accept (see
	// aliasedStringArg in write_file/edit_file/read_file/grep/glob/list). The
	// sandbox gates by arg-key name, so any alias a tool resolves but the sandbox
	// does not inspect would let a model route a write/read around the
	// workspace+symlink boundary.
	for _, key := range []string{"path", "file", "file_path", "filepath", "filename", "cwd", "workdir", "dir", "directory"} {
		if value := argString(request.Args, key); value != "" {
			paths = append(paths, value)
		}
	}
	if request.ToolName == "apply_patch" {
		paths = append(paths, applyPatchRequestPaths(request.Args)...)
	}
	return paths
}

func applyPatchRequestPaths(args map[string]any) []string {
	patch := firstArgString(args, "patch", "diff")
	if patch == "" {
		return nil
	}
	cwd := firstArgString(args, "cwd")
	var paths []string
	for _, path := range applyPatchPaths(patch) {
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
	patch := firstArgString(request.Args, "patch", "diff")
	if patch == "" {
		return nil
	}
	// Only relative traversal is rejected up front. Absolute paths flow through
	// the regular workspace-scope validation below (requestPaths), which accepts
	// one inside the workspace and denies one outside — a model that echoes the
	// absolute path read_file showed it must not be blocked for that alone.
	for _, path := range applyPatchPaths(patch) {
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

func applyPatchPaths(patch string) []string {
	if IsStructuredPatch(patch) {
		return structuredPatchHeaderPaths(patch)
	}
	return patchHeaderPaths(patch)
}

// structuredPatchHeaderPaths is intentionally a small, conservative scanner for
// the sandbox boundary. The executor validates the complete hunk grammar before
// writing; the sandbox only needs every possible target path so it can reject an
// unsafe request before the tool runs.
func structuredPatchHeaderPaths(patch string) []string {
	var paths []string
	for _, line := range strings.Split(strings.ReplaceAll(patch, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		for _, prefix := range []string{"*** Add File: ", "*** Delete File: ", "*** Update File: ", "*** Move to: "} {
			if path, ok := strings.CutPrefix(trimmed, prefix); ok {
				path = strings.ReplaceAll(filepath.ToSlash(strings.TrimSpace(path)), "\\", "/")
				if path != "" {
					paths = append(paths, path)
				}
				break
			}
		}
	}
	return paths
}

func patchHeaderPaths(patch string) []string {
	var paths []string
	oldRemaining, newRemaining := 0, 0
	inHunk := false
	for _, line := range strings.Split(strings.ReplaceAll(patch, "\r\n", "\n"), "\n") {
		if inHunk && (oldRemaining > 0 || newRemaining > 0) {
			switch {
			case strings.HasPrefix(line, "-"):
				oldRemaining--
			case strings.HasPrefix(line, "+"):
				newRemaining--
			case strings.HasPrefix(line, "\\"):
			default:
				oldRemaining--
				newRemaining--
			}
			continue
		}
		inHunk = false
		switch {
		case strings.HasPrefix(line, "diff --git "):
			fields := strings.Fields(line)
			if len(fields) >= 4 {
				paths = append(paths, stripPatchPrefix(fields[2]), stripPatchPrefix(fields[3]))
			}
		case strings.HasPrefix(line, "@@"):
			oldRemaining, newRemaining = parsePatchHunkCounts(line)
			inHunk = oldRemaining > 0 || newRemaining > 0
		case strings.HasPrefix(line, "--- "), strings.HasPrefix(line, "+++ "):
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				paths = append(paths, stripPatchPrefix(fields[1]))
			}
		}
	}
	return paths
}

func parsePatchHunkCounts(line string) (int, int) {
	_, rest, ok := strings.Cut(line, "@@")
	if !ok {
		return 0, 0
	}
	rangeSection := rest
	if before, _, ok := strings.Cut(rest, "@@"); ok {
		rangeSection = before
	}
	old, next := 0, 0
	for _, field := range strings.Fields(rangeSection) {
		switch {
		case strings.HasPrefix(field, "-"):
			old = patchHunkCount(field[1:])
		case strings.HasPrefix(field, "+"):
			next = patchHunkCount(field[1:])
		}
	}
	return old, next
}

func patchHunkCount(spec string) int {
	if _, count, ok := strings.Cut(spec, ","); ok {
		if n, err := strconv.Atoi(count); err == nil {
			return n
		}
		return 0
	}
	return 1
}

func stripPatchPrefix(path string) string {
	path = strings.TrimSpace(path)
	if strings.HasPrefix(path, "a/") || strings.HasPrefix(path, "b/") {
		path = path[2:]
	}
	return filepath.ToSlash(path)
}
