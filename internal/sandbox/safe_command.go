package sandbox

import (
	"runtime"
	"strings"
)

// InteractiveCommandResult describes the outcome of inspecting a shell command
// for interactive programs that would hang a non-interactive agent (the agent
// has no TTY to type into, so an editor/pager/REPL would block until timeout).
type InteractiveCommandResult struct {
	// Interactive is true when the command launches a program that waits for
	// terminal input the agent cannot supply.
	Interactive bool
	// Command is the matched program/segment (e.g. "vim", "git rebase -i").
	Command string
	// Reason is a short human-readable explanation of why it would hang.
	Reason string
	// Suggestion is an actionable non-interactive alternative.
	Suggestion string
}

// interactiveProgram pairs a detected program with the guidance shown to the agent.
type interactiveProgram struct {
	reason     string
	suggestion string
	// windowsSuggestion overrides suggestion when goos == "windows", for cases
	// where the POSIX suggestion (cat/head/tail/ps, etc.) has no cmd.exe
	// equivalent and would just trade one broken command for another.
	windowsSuggestion string
	// windowsOnly limits the match to GOOS == "windows" (e.g. notepad).
	windowsOnly bool
}

// interactivePrograms maps a bare command name to its non-interactive guidance.
// These programs open a TTY session and block forever without one.
var interactivePrograms = map[string]interactiveProgram{
	// Editors.
	"vim":   {reason: "vim is a full-screen editor that waits for keystrokes", suggestion: "Use a non-interactive edit (the edit_file/write_file tools) or `sed -i`/`printf` to modify files."},
	"vi":    {reason: "vi is a full-screen editor that waits for keystrokes", suggestion: "Use a non-interactive edit (the edit_file/write_file tools) or `sed -i` to modify files."},
	"nvim":  {reason: "nvim is a full-screen editor that waits for keystrokes", suggestion: "Use a non-interactive edit (the edit_file/write_file tools) or `sed -i` to modify files."},
	"nano":  {reason: "nano is a full-screen editor that waits for keystrokes", suggestion: "Use a non-interactive edit (the edit_file/write_file tools) or `sed -i` to modify files."},
	"emacs": {reason: "emacs opens an interactive session", suggestion: "Use `emacs --batch` for scripting, or the edit_file/write_file tools."},
	"pico":  {reason: "pico is a full-screen editor that waits for keystrokes", suggestion: "Use the edit_file/write_file tools or `sed -i`."},
	// Pagers.
	"less": {
		reason:            "less is a pager that waits for navigation keys",
		suggestion:        "Use `cat`, `head`, or `tail -n N` to print file contents non-interactively.",
		windowsSuggestion: "Use `type` to print file contents non-interactively, or the read_file tool with offset/limit for a partial view.",
	},
	"more": {
		reason:            "more is a pager that waits for navigation keys",
		suggestion:        "Use `cat`, `head`, or `tail -n N` to print file contents non-interactively.",
		windowsSuggestion: "Use `type` to print file contents non-interactively, or the read_file tool with offset/limit for a partial view.",
	},
	"most": {
		reason:            "most is a pager that waits for navigation keys",
		suggestion:        "Use `cat`, `head`, or `tail -n N` to print file contents non-interactively.",
		windowsSuggestion: "Use `type` to print file contents non-interactively, or the read_file tool with offset/limit for a partial view.",
	},
	// Process/system monitors.
	"top":   {reason: "top runs a live full-screen dashboard until you quit it", suggestion: "Use `ps aux` (optionally `| head`) for a one-shot snapshot.", windowsSuggestion: "Use `tasklist` for a one-shot process snapshot."},
	"htop":  {reason: "htop runs a live full-screen dashboard until you quit it", suggestion: "Use `ps aux` (optionally `| head`) for a one-shot snapshot.", windowsSuggestion: "Use `tasklist` for a one-shot process snapshot."},
	"btop":  {reason: "btop runs a live full-screen dashboard until you quit it", suggestion: "Use `ps aux` (optionally `| head`) for a one-shot snapshot.", windowsSuggestion: "Use `tasklist` for a one-shot process snapshot."},
	"btm":   {reason: "btm runs a live full-screen dashboard until you quit it", suggestion: "Use `ps aux` for a one-shot snapshot.", windowsSuggestion: "Use `tasklist` for a one-shot process snapshot."},
	"watch": {reason: "watch re-runs a command on a loop until interrupted", suggestion: "Run the underlying command once instead of wrapping it in `watch`."},
	// Language REPLs (only interactive when invoked with no script/expression).
	"python":  {reason: "python with no script drops into an interactive REPL", suggestion: "Run `python script.py` or `python -c '<code>'`."},
	"python3": {reason: "python3 with no script drops into an interactive REPL", suggestion: "Run `python3 script.py` or `python3 -c '<code>'`."},
	"node":    {reason: "node with no script drops into an interactive REPL", suggestion: "Run `node script.js` or `node -e '<code>'`."},
	"irb":     {reason: "irb is the interactive Ruby REPL", suggestion: "Run `ruby script.rb` or `ruby -e '<code>'`."},
	"ruby":    {reason: "ruby with no script may drop into an interactive session", suggestion: "Run `ruby script.rb` or `ruby -e '<code>'`."},
	"pry":     {reason: "pry is an interactive Ruby REPL", suggestion: "Run `ruby script.rb` instead."},
	"php":     {reason: "php with no script (-a) opens an interactive shell", suggestion: "Run `php script.php` or `php -r '<code>'`."},
	"ghci":    {reason: "ghci is the interactive Haskell REPL", suggestion: "Use `runghc script.hs` instead."},
	// Database / remote clients (interactive when no command/query is supplied).
	"psql":      {reason: "psql opens an interactive SQL prompt", suggestion: "Pass a query with `psql -c '<sql>'` or a file with `psql -f file.sql`."},
	"mysql":     {reason: "mysql opens an interactive SQL prompt", suggestion: "Pass a query with `mysql -e '<sql>'` or a file with `mysql < file.sql`."},
	"sqlite3":   {reason: "sqlite3 with no SQL opens an interactive prompt", suggestion: "Pass SQL inline: `sqlite3 db.sqlite '<sql>'`."},
	"redis-cli": {reason: "redis-cli with no command opens an interactive prompt", suggestion: "Pass the command inline: `redis-cli GET key`."},
	"mongo":     {reason: "mongo opens an interactive shell", suggestion: "Pass `--eval '<js>'` or a script file."},
	"mongosh":   {reason: "mongosh opens an interactive shell", suggestion: "Pass `--eval '<js>'` or a script file."},
	// Remote/terminal sessions (interactive when no remote command is supplied).
	"ssh":    {reason: "ssh with no remote command opens an interactive login shell", suggestion: "Append the command to run remotely: `ssh host 'command'`."},
	"telnet": {reason: "telnet opens an interactive session", suggestion: "Use `curl`/`nc` with piped input for scripted access."},
	"ftp":    {reason: "ftp opens an interactive session", suggestion: "Use `curl`/`wget` for scripted transfers."},
	"sftp":   {reason: "sftp opens an interactive session", suggestion: "Use `scp` for scripted transfers."},
	// Debuggers.
	"gdb":  {reason: "gdb opens an interactive debugger prompt", suggestion: "Use `gdb -batch -ex '<cmd>'` for scripted debugging."},
	"lldb": {reason: "lldb opens an interactive debugger prompt", suggestion: "Use `lldb --batch -o '<cmd>'` for scripted debugging."},
	// Fuzzy finders / selectors.
	"fzf":  {reason: "fzf is an interactive fuzzy finder", suggestion: "Use `grep`/`rg` to filter non-interactively."},
	"peco": {reason: "peco is an interactive selector", suggestion: "Use `grep`/`rg` to filter non-interactively."},
	// Windows-only interactive launchers.
	"notepad": {reason: "notepad opens a GUI editor", suggestion: "Use the edit_file/write_file tools instead.", windowsOnly: true},
}

// replPrograms only hang when no script/expression argument is provided. The
// listed flags switch them into non-interactive mode and should suppress the
// guard.
var nonInteractiveREPLFlags = map[string][]string{
	"python":  {"-c", "-m"},
	"python3": {"-c", "-m"},
	"node":    {"-e", "--eval", "-p", "--print", "-v", "--check", "-h"},
	"ruby":    {"-e"},
	"php":     {"-r", "-f"},
	"psql":    {"-c", "--command", "-f", "--file", "-l", "--list"},
	"mysql":   {"-e", "--execute"},
	"mongo":   {"--eval", "-f", "--file"},
	"mongosh": {"--eval", "-f", "--file"},
}

// infoExitFlags make ANY repl/interactive program print a message and exit
// instead of opening a prompt. `--version` and `--help` are unambiguous across
// these programs (unlike, say, `-v`, which is "version" for node but "verbose"
// for mysql), so they universally suppress the interactive guard — without them
// a harmless `node --version` / `python3 --version` was wrongly blocked.
var infoExitFlags = map[string]bool{
	"--version": true,
	"--help":    true,
}

// interactiveSegments are multi-word interactive invocations. The detector
// matches them as substrings (after normalizing whitespace) so flags like
// `git rebase -i` or `tail -f` are caught even mid-pipeline.
var interactiveSegments = []struct {
	match             string
	command           string
	reason            string
	suggestion        string
	windowsSuggestion string
}{
	{match: "git rebase -i", command: "git rebase -i", reason: "interactive rebase opens an editor for the todo list", suggestion: "Use a non-interactive rebase (`git rebase <base>`) or scripted `git rebase --onto`, and resolve via `git rebase --continue`."},
	{match: "git rebase --interactive", command: "git rebase -i", reason: "interactive rebase opens an editor for the todo list", suggestion: "Use a non-interactive rebase (`git rebase <base>`)."},
	{match: "git add -i", command: "git add -i", reason: "interactive add opens a selection prompt", suggestion: "Stage paths explicitly: `git add <path>`."},
	{match: "git add -p", command: "git add -p", reason: "interactive patch staging opens a prompt", suggestion: "Stage paths explicitly: `git add <path>`."},
	{match: "git commit -p", command: "git commit -p", reason: "interactive patch commit opens a prompt", suggestion: "Stage with `git add <path>` then `git commit -m`."},
	{match: "tail -f", command: "tail -f", reason: "tail -f follows a file forever", suggestion: "Use `tail -n N <file>` for a bounded read.", windowsSuggestion: "Read the file with the read_file tool (offset/limit), or `type <file>` for the whole file."},
	{match: "tail --follow", command: "tail -f", reason: "tail --follow follows a file forever", suggestion: "Use `tail -n N <file>` for a bounded read.", windowsSuggestion: "Read the file with the read_file tool (offset/limit), or `type <file>` for the whole file."},
	{match: "journalctl -f", command: "journalctl -f", reason: "journalctl -f streams logs forever", suggestion: "Use `journalctl -n N` for a bounded read."},
	{match: "kubectl logs -f", command: "kubectl logs -f", reason: "kubectl logs -f streams logs forever", suggestion: "Drop -f and use `kubectl logs --tail=N`."},
	{match: "docker logs -f", command: "docker logs -f", reason: "docker logs -f streams logs forever", suggestion: "Drop -f and use `docker logs --tail N`."},
	{match: "docker attach", command: "docker attach", reason: "docker attach joins an interactive container session", suggestion: "Use `docker exec <id> <command>` for one-shot execution."},
}

// DetectInteractiveCommand inspects a shell command for interactive programs
// that would block a non-interactive agent. goos selects platform-specific
// rules (pass "" to use the host runtime.GOOS).
func DetectInteractiveCommand(command string, goos string) InteractiveCommandResult {
	return detectInteractiveCommandAt(command, goos, 0)
}

func detectInteractiveCommandAt(command string, goos string, depth int) InteractiveCommandResult {
	if depth > maxAnalyzerDepth {
		return InteractiveCommandResult{
			Interactive: true,
			Command:     "nested shell launcher",
			Reason:      "shell launcher nesting exceeds the safe inspection limit, so its payload cannot be proven non-interactive",
			Suggestion:  "Simplify the nested shell command or run its non-interactive payload directly.",
		}
	}
	command = strings.TrimSpace(command)
	if command == "" {
		return InteractiveCommandResult{}
	}
	if goos == "" {
		goos = runtime.GOOS
	}

	normalized := normalizeWhitespace(command)

	// Multi-word interactive invocations (flags/subcommands) take priority so
	// the more specific message wins. Match only at a real command boundary —
	// the start of a shell segment (after leading env-assignments and wrapper
	// prefixes) — so the segment text appearing inside a quoted argument (e.g.
	// `echo "git rebase -i ..."`) is NOT a false positive.
	for _, segment := range splitShellSegments(normalized) {
		body := strings.ToLower(commandBody(strings.Fields(segment)))
		for _, seg := range interactiveSegments {
			if body == seg.match || strings.HasPrefix(body, seg.match+" ") {
				suggestion := seg.suggestion
				if goos == "windows" && seg.windowsSuggestion != "" {
					suggestion = seg.windowsSuggestion
				}
				return InteractiveCommandResult{
					Interactive: true,
					Command:     seg.command,
					Reason:      seg.reason,
					Suggestion:  suggestion,
				}
			}
		}
	}

	// Inspect each shell segment (split on &&, ||, ;, |) so an interactive
	// program hidden behind an operator is still caught.
	for _, segment := range splitShellSegments(normalized) {
		fields := strings.Fields(segment)
		first := firstProgram(fields)
		if first == "" {
			continue
		}
		// `sh -c <payload>` / `bash -c <payload>` runs the payload as a fresh
		// command; recurse into it so an interactive program inside the payload
		// is detected (e.g. `sh -c 'vim x'`).
		if payload := shellDashCPayload(first, fields); payload != "" {
			if inner := detectInteractiveCommandAt(payload, goos, depth+1); inner.Interactive {
				return inner
			}
			// strings.Fields cannot preserve a quoted multi-word payload. If its
			// best-effort read is clean, keep going so the AST pass below can
			// inspect the actual payload rather than treating truncation as safe.
		}
		program, ok := interactivePrograms[first]
		if !ok {
			continue
		}
		if program.windowsOnly && goos != "windows" {
			continue
		}
		if hasNonInteractiveFlag(first, fields) {
			continue
		}
		suggestion := program.suggestion
		if goos == "windows" && program.windowsSuggestion != "" {
			suggestion = program.windowsSuggestion
		}
		return InteractiveCommandResult{
			Interactive: true,
			Command:     first,
			Reason:      program.reason,
			Suggestion:  suggestion,
		}
	}

	// AST second opinion (issue #473): the hand-written segment split above
	// mis-handles interactive programs hidden by unusual quoting, command
	// substitution, subshells, or newline separators. Re-extract the real simple
	// commands from the parsed shell tree and apply the SAME full pipeline
	// (interactive segments, sh -c payload recursion, per-program lookup), so a
	// bypass is caught while every program stays classified exactly as before
	// (ssh with a trailing command, python -c, etc. remain allowed). A command
	// the parser cannot handle (Windows cmd.exe, obfuscation) yields no commands
	// and falls through unchanged — the guard never hard-blocks on a parse error.
	for _, fields := range astCommandFields(command) {
		if result, ok := inspectCommandFields(fields, goos, depth); ok {
			return result
		}
	}

	return InteractiveCommandResult{}
}

// inspectCommandFields applies the full interactive-detection pipeline to one
// already-split command's fields: the multi-word interactive segments, then the
// `sh -c <payload>` recursion, then the per-program lookup with its
// non-interactive suppressions (hasNonInteractiveFlag covers REPL flags and
// trailing-command clients like ssh). It mirrors the hand-written passes above
// so an AST-extracted command is classified identically to a plainly-split one.
func inspectCommandFields(fields []string, goos string, depth int) (InteractiveCommandResult, bool) {
	body := strings.ToLower(commandBody(fields))
	for _, seg := range interactiveSegments {
		if body == seg.match || strings.HasPrefix(body, seg.match+" ") {
			suggestion := seg.suggestion
			if goos == "windows" && seg.windowsSuggestion != "" {
				suggestion = seg.windowsSuggestion
			}
			return InteractiveCommandResult{Interactive: true, Command: seg.command, Reason: seg.reason, Suggestion: suggestion}, true
		}
	}
	first := firstProgram(fields)
	if first == "" {
		return InteractiveCommandResult{}, false
	}
	start := programIndex(first, fields)
	args := fields[start+1:]
	payloadIndex, hasPayload := shellCommandPayloadIndex(first, args)
	if hasPayload && payloadIndex < len(args) {
		// astCommandFields already removed the launcher's outer quoting. Do not
		// trim again: trailing quotes can belong to a nested launcher's payload.
		payload := args[payloadIndex]
		if inner := detectInteractiveCommandAt(payload, goos, depth+1); inner.Interactive {
			return inner, true
		}
		return InteractiveCommandResult{}, false
	}
	program, ok := interactivePrograms[first]
	if !ok || (program.windowsOnly && goos != "windows") {
		return InteractiveCommandResult{}, false
	}
	if hasNonInteractiveFlag(first, fields) {
		return InteractiveCommandResult{}, false
	}
	suggestion := program.suggestion
	if goos == "windows" && program.windowsSuggestion != "" {
		suggestion = program.windowsSuggestion
	}
	return InteractiveCommandResult{Interactive: true, Command: first, Reason: program.reason, Suggestion: suggestion}, true
}

// wrapperPrograms are launcher prefixes that precede the real program. After
// one of these we keep scanning for the actual executable.
var wrapperPrograms = map[string]bool{
	"sudo": true, "command": true, "env": true, "nohup": true, "time": true,
	"exec": true, "doas": true, "nice": true, "timeout": true, "stdbuf": true,
	"setsid": true, "ionice": true, "xargs": true,
}

// wrapperValueOptionsByProg lists, PER wrapper program, the options that consume
// the FOLLOWING token as their value (both short `-u` and long `--user` spellings,
// e.g. `sudo -u root` / `sudo --user root` / `timeout -s KILL`). It is keyed by
// wrapper because the same flag means different things to different launchers —
// `sudo -n` is valueless (non-interactive) while `nice -n` takes an adjustment —
// so a single global set would wrongly swallow the real program after a valueless
// flag (e.g. skip `rm` in `sudo -n rm -rf`). The `--flag=value` spelling is a
// single token and needs no entry; only the space-separated form consumes a value.
var wrapperValueOptionsByProg = map[string]map[string]bool{
	"sudo":    {"-u": true, "--user": true, "-g": true, "--group": true, "-p": true, "--prompt": true, "-C": true, "--close-from": true, "-r": true, "--role": true, "-T": true, "--command-timeout": true, "-U": true, "--other-user": true, "-h": true, "--host": true, "-D": true, "--chdir": true, "-R": true, "--chroot": true},
	"doas":    {"-u": true, "-C": true},
	"env":     {"-u": true, "--unset": true, "-S": true, "--split-string": true, "-C": true, "--chdir": true, "-a": true, "--argv0": true},
	"timeout": {"-s": true, "--signal": true, "-k": true, "--kill-after": true},
	"nice":    {"-n": true, "--adjustment": true},
	"ionice":  {"-c": true, "--class": true, "-n": true, "--classdata": true, "-p": true, "--pid": true},
	"stdbuf":  {"-i": true, "--input": true, "-o": true, "--output": true, "-e": true, "--error": true},
	"xargs":   {"-a": true, "--arg-file": true, "-d": true, "--delimiter": true, "-E": true, "-I": true, "--replace": true, "-L": true, "--max-lines": true, "-n": true, "--max-args": true, "-P": true, "--max-procs": true, "-s": true, "--max-chars": true},
	"exec":    {"-a": true, "--argv0": true},
}

// wrapperConsumesValue reports whether option is a value-consuming flag of the
// active wrapper. With no active wrapper (or an unknown one, or a flag not listed
// for it) it returns false, so token scanning never skips a token that might be
// the real program. A `--flag=value` token carries its own value, so it never
// consumes the next token.
func wrapperConsumesValue(wrapper, option string) bool {
	if strings.Contains(option, "=") {
		return false
	}
	opts, ok := wrapperValueOptionsByProg[wrapper]
	return ok && opts[option]
}

// firstProgram returns the first executable name in a segment, skipping leading
// environment-variable assignments (FOO=bar cmd), wrapper prefixes (sudo, env,
// nice, timeout, stdbuf, setsid, ionice, xargs, ...), and the option tokens that
// belong to those wrappers (e.g. `sudo -u root`, `env -i`, `timeout 5`).
func firstProgram(fields []string) string {
	wrapper := ""
	for index := 0; index < len(fields); index++ {
		field := fields[index]
		if strings.Contains(field, "=") && !strings.HasPrefix(field, "=") {
			// Environment assignment prefix; keep scanning.
			continue
		}
		// An option token (or a bare numeric argument such as `timeout 5`)
		// belongs to a preceding wrapper, not the program; skip it, and also
		// skip the value of an option that the active wrapper consumes.
		if strings.HasPrefix(field, "-") {
			if wrapperConsumesValue(wrapper, field) && index+1 < len(fields) {
				index++
			}
			continue
		}
		if isNumericToken(field) {
			continue
		}
		token := normalizeProgramToken(field)
		if wrapperPrograms[token] {
			// Wrapper prefix; the real program follows. Track it so its options'
			// value-consumption is interpreted in its own context.
			wrapper = token
			continue
		}
		return token
	}
	return ""
}

// commandBody returns the segment's command portion with leading
// environment-variable assignments (FOO=bar) and wrapper prefixes (sudo, env,
// nice, timeout, ...) and their consumed option values removed, joined back
// into a string. It lets interactive-segment matching anchor on the real
// command boundary (e.g. `sudo git rebase -i` -> "git rebase -i") instead of
// matching the segment text anywhere as a raw substring.
func commandBody(fields []string) string {
	return strings.Join(commandBodyFields(fields), " ")
}

// commandBodyFields is commandBody's scan, returning the fields from the first
// real command token onward (nil when the segment is nothing but assignments and
// wrappers). Callers that need to reason about the program and its arguments
// separately — the unparseable-command fallback in risk.go — use this rather
// than re-splitting commandBody's joined string, which would lose the token
// boundaries the fallback tokenizer worked to preserve.
func commandBodyFields(fields []string) []string {
	wrapper := ""
	for index := 0; index < len(fields); index++ {
		field := fields[index]
		if strings.Contains(field, "=") && !strings.HasPrefix(field, "=") && !strings.HasPrefix(field, "-") {
			continue
		}
		if strings.HasPrefix(field, "-") {
			if wrapperConsumesValue(wrapper, field) && index+1 < len(fields) {
				index++
			}
			continue
		}
		if isNumericToken(field) {
			continue
		}
		if token := normalizeProgramToken(field); wrapperPrograms[token] {
			wrapper = token
			continue
		}
		// First real command token: the body starts here.
		return fields[index:]
	}
	return nil
}

// envSplitCommandFields resolves GNU env's -S/--split-string launcher when env
// appears at an actual command position (possibly behind another wrapper). The
// split string is argv syntax, not shell source, so metacharacters remain inert
// unless the resulting executable is itself a shell with -c.
type envSplitCommandResult struct {
	command                           []string
	recognized                        bool
	executableEnvironmentDependent    bool
	commandSourceEnvironmentDependent bool
}

func envSplitCommandFields(fields []string) envSplitCommandResult {
	wrapper := ""
	for index := 0; index < len(fields); index++ {
		field := fields[index]
		if strings.Contains(field, "=") && !strings.HasPrefix(field, "=") && !strings.HasPrefix(field, "-") {
			continue
		}
		if strings.HasPrefix(field, "-") {
			if wrapperConsumesValue(wrapper, field) && index+1 < len(fields) {
				index++
			}
			continue
		}
		if isNumericToken(field) {
			continue
		}
		token := normalizeProgramToken(field)
		if token == "env" {
			return envSplitCommand(fields[index+1:])
		}
		if wrapperPrograms[token] {
			wrapper = token
			continue
		}
		return envSplitCommandResult{}
	}
	return envSplitCommandResult{}
}

func envSplitCommand(args []string) envSplitCommandResult {
	current := append([]string(nil), args...)
	dependent := make([]bool, len(current))
	recognized := false
	for rewrite := 0; rewrite < 16; rewrite++ {
		commandIndex := len(current)
		rewritten := false
		for index := 0; index < len(current); index++ {
			arg := current[index]
			if arg == "--" {
				commandIndex = index + 1
				break
			}
			if strings.Contains(arg, "=") && !strings.HasPrefix(arg, "=") && !strings.HasPrefix(arg, "-") {
				continue
			}
			value, consumed, retainedOption, split := envSplitOption(current, index)
			if split {
				recognized = true
				if consumed == 0 {
					return envSplitCommandResult{recognized: true}
				}
				fields, fieldDependencies, ok := splitEnvString(value)
				if !ok {
					// The split string is executable argv this scan cannot read.
					// "Cannot inspect" is not "does not use the network": env still
					// runs whatever the shell expanded into it, so keep the gate.
					return envSplitCommandResult{recognized: true, executableEnvironmentDependent: true}
				}
				replacement := fields
				replacementDependencies := fieldDependencies
				if retainedOption != "" {
					replacement = append([]string{retainedOption}, replacement...)
					replacementDependencies = append([]bool{false}, replacementDependencies...)
				}
				current = append(append(append([]string(nil), current[:index]...), replacement...), current[index+consumed:]...)
				dependent = append(append(append([]bool(nil), dependent[:index]...), replacementDependencies...), dependent[index+consumed:]...)
				rewritten = true
				break
			}
			if strings.HasPrefix(arg, "-") {
				if wrapperConsumesValue("env", arg) {
					index++
				}
				continue
			}
			commandIndex = index
			break
		}
		if rewritten {
			continue
		}
		if !recognized {
			return envSplitCommandResult{}
		}
		if commandIndex >= len(current) {
			return envSplitCommandResult{recognized: true}
		}
		command := current[commandIndex:]
		// Preserve a nested env invocation so fallbackBodyUsesNetwork can process
		// its own -S grammar. Other ordinary wrappers can be resolved directly.
		if normalizeProgramToken(command[0]) != "env" {
			command = commandBodyFields(command)
			commandIndex = len(current) - len(command)
		}
		commandDependencies := dependent[commandIndex:]
		return envSplitCommandResult{
			command:                           command,
			recognized:                        true,
			executableEnvironmentDependent:    len(commandDependencies) > 0 && commandDependencies[0],
			commandSourceEnvironmentDependent: envSplitCommandSourceDependent(command, commandDependencies),
		}
	}
	// Excessive rewrites are attacker-controlled ambiguity. Keep the network
	// gate rather than recursing without a bound.
	return envSplitCommandResult{recognized: true, executableEnvironmentDependent: true}
}

func envSplitCommandSourceDependent(command []string, dependent []bool) bool {
	if len(command) == 0 || len(command) != len(dependent) {
		return false
	}
	program := normalizeProgramToken(command[0])
	if dependent[0] {
		return true
	}
	args, argDependent := command[1:], dependent[1:]
	if shellPrograms[program] {
		if index, ok := shellCommandPayloadIndex(program, args); ok && index < len(argDependent) {
			return argDependent[index]
		}
		return false
	}
	if program == "powershell" || program == "pwsh" || program == "cmd" ||
		program == "call" || program == "start" || program == "env" {
		for _, value := range argDependent {
			if value {
				return true
			}
		}
	}
	return false
}

// envSplitLongOptionName is GNU env's only long option beginning with "s", so
// any unambiguous prefix of it ("--s", "--split", "--split-strin", ...) names
// the same option under GNU getopt_long's abbreviation rule.
const envSplitLongOptionName = "split-string"

// matchesEnvSplitLongOption reports whether name (the text of a "--name" or
// "--name=value" option, with "--" and any "=value" suffix already stripped)
// is an unambiguous abbreviation of --split-string. Modelling the runtime
// option grammar here, rather than the single exact spelling, matters: `env
// --split 'curl https://…'` is accepted by GNU env exactly like
// `--split-string`, and treating only the full spelling as recognized let a
// shortened form fall through to ordinary wrapper handling with no network
// classification at all.
func matchesEnvSplitLongOption(name string) bool {
	if name == "" {
		return false
	}
	return strings.HasPrefix(envSplitLongOptionName, name)
}

func envSplitOption(args []string, index int) (value string, consumed int, retainedOption string, ok bool) {
	arg := args[index]
	switch {
	case arg == "-S":
		if index+1 >= len(args) {
			return "", 0, "", true
		}
		return args[index+1], 2, "", true
	case strings.HasPrefix(arg, "--"):
		name := arg[2:]
		hasValue := false
		optionValue := ""
		if equals := strings.IndexByte(name, '='); equals >= 0 {
			hasValue = true
			optionValue = name[equals+1:]
			name = name[:equals]
		}
		if !matchesEnvSplitLongOption(name) {
			return "", 0, "", false
		}
		if hasValue {
			return optionValue, 1, "", true
		}
		if index+1 >= len(args) {
			return "", 0, "", true
		}
		return args[index+1], 2, "", true
	case strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--"):
		position := strings.IndexByte(arg[1:], 'S')
		if position < 0 {
			return "", 0, "", false
		}
		position++
		for _, option := range arg[1:position] {
			if option != 'i' && option != '0' && option != 'v' {
				return "", 0, "", false
			}
		}
		retained := ""
		if position > 1 {
			retained = "-" + arg[1:position]
		}
		if position+1 < len(arg) {
			return arg[position+1:], 1, retained, true
		}
		if index+1 >= len(args) {
			return "", 0, "", true
		}
		return args[index+1], 2, retained, true
	default:
		return "", 0, "", false
	}
}

func splitEnvString(value string) ([]string, []bool, bool) {
	var fields []string
	var dependent []bool
	var word strings.Builder
	var quote rune
	started := false
	wordDependent := false
	flush := func() {
		if started {
			fields = append(fields, word.String())
			dependent = append(dependent, wordDependent)
			word.Reset()
			started = false
			wordDependent = false
		}
	}
	runes := []rune(value)
	for index := 0; index < len(runes); index++ {
		r := runes[index]
		if r == '\\' {
			if index+1 >= len(runes) {
				return nil, nil, false
			}
			index++
			escaped := runes[index]
			if quote == '\'' && escaped != '\\' && escaped != '\'' {
				word.WriteRune('\\')
				word.WriteRune(escaped)
				started = true
				continue
			}
			switch escaped {
			case 'c':
				if quote != 0 {
					return nil, nil, false
				}
				flush()
				return fields, dependent, true
			case '_':
				if quote == '"' {
					word.WriteRune(' ')
					started = true
				} else {
					flush()
				}
			case 'f':
				word.WriteRune('\f')
				started = true
			case 'n':
				word.WriteRune('\n')
				started = true
			case 'r':
				word.WriteRune('\r')
				started = true
			case 't':
				word.WriteRune('\t')
				started = true
			case 'v':
				word.WriteRune('\v')
				started = true
			case '#', '$', '"', '\'', '\\':
				word.WriteRune(escaped)
				started = true
			default:
				return nil, nil, false
			}
			continue
		}
		if r == '\'' || r == '"' {
			if quote == 0 {
				quote = r
				started = true
				continue
			}
			if quote == r {
				quote = 0
				continue
			}
		}
		if r == '$' && quote != '\'' {
			if index+2 >= len(runes) || runes[index+1] != '{' {
				return nil, nil, false
			}
			end := index + 2
			for end < len(runes) && runes[end] != '}' {
				end++
			}
			if end >= len(runes) || !validEnvSplitVariableName(string(runes[index+2:end])) {
				return nil, nil, false
			}
			word.WriteString(string(runes[index : end+1]))
			started = true
			wordDependent = true
			index = end
			continue
		}
		if r == '#' && quote == 0 && !started {
			break
		}
		if quote == 0 && (r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f') {
			flush()
			continue
		}
		word.WriteRune(r)
		started = true
	}
	if quote != 0 {
		return nil, nil, false
	}
	flush()
	return fields, dependent, true
}

func validEnvSplitVariableName(name string) bool {
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

func busyboxDelegatedCommand(args []string) ([]string, commandResolution) {
	if len(args) == 0 {
		return nil, commandKnownLocal
	}
	switch args[0] {
	case "--help", "--list", "--list-full", "--install", "--show":
		return nil, commandKnownLocal
	}
	if strings.HasPrefix(args[0], "-") {
		return nil, commandUnresolved
	}
	if fallbackTokenLooksDynamic(args[0]) {
		return nil, commandUnresolved
	}
	return args, commandKnownLocal
}

var (
	straceRequiredLongOptions = map[string]bool{
		"--columns": true, "--detach-on": true, "--env": true, "--interruptible": true,
		"--stack-trace-frame-limit": true, "--syscall-limit": true, "--output": true,
		"--summary-syscall-overhead": true, "--attach": true, "--trace-path": true,
		"--string-limit": true, "--summary-sort-by": true, "--user": true,
		"--summary-columns": true, "--const-print-style": true, "--argv0": true,
		"--color": true, "--trace": true, "--trace-fds": true, "--abbrev": true,
		"--verbose": true, "--raw": true, "--signals": true, "--status": true,
		"--read": true, "--write": true, "--fault": true, "--inject": true,
		"--kvm": true, "--decode-pids": true,
	}
	straceOptionalLongOptions = map[string]bool{
		"--daemonize": true, "--daemonised": true, "--daemonized": true,
		"--stack-trace": true, "--stack-traces": true, "--relative-timestamps": true,
		"--absolute-timestamps": true, "--timestamps": true, "--syscall-times": true,
		"--strings-in-hex": true, "--tips": true, "--namespace": true,
		"--quiet": true, "--silent": true, "--silence": true, "--decode-fds": true,
		"--secontext": true,
	}
	straceValuelessLongOptions = map[string]bool{
		"--output-append-mode": true, "--summary-only": true, "--summary": true,
		"--debug": true, "--follow-forks": true, "--output-separately": true,
		"--instruction-pointer": true, "--kill-on-exit": true, "--syscall-number": true,
		"--arg-names": true, "--no-abbrev": true, "--summary-wall-clock": true,
		"--pidns-translation": true, "--successful-only": true, "--failed-only": true,
		"--failing-only": true, "--seccomp-bpf": true, "--always-show-pid": true,
	}
)

type straceLongOptionKind uint8

const (
	straceLongOptionInvalid straceLongOptionKind = iota
	straceLongOptionRequired
	straceLongOptionOptional
	straceLongOptionValueless
	straceLongOptionTerminal
)

func straceDelegatedCommand(args []string) ([]string, commandResolution) {
	index, status := straceChildResolution(args)
	if status != commandKnownLocal || index < 0 || index >= len(args) {
		return nil, status
	}
	if fallbackTokenLooksDynamic(args[index]) {
		return nil, commandUnresolved
	}
	return args[index:], commandKnownLocal
}

func straceChildResolution(args []string) (int, commandResolution) {
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch arg {
		case "--":
			if index+1 >= len(args) {
				return -1, commandKnownLocal
			}
			return index + 1, commandKnownLocal
		case "-":
			return index, commandKnownLocal
		}
		if strings.HasPrefix(arg, "--") {
			name, hasValue := arg, false
			if equals := strings.IndexByte(arg, '='); equals >= 0 {
				name, hasValue = arg[:equals], true
			}
			kind, ok := resolveStraceLongOption(name)
			if !ok {
				return -1, commandUnresolved
			}
			switch kind {
			case straceLongOptionTerminal:
				return -1, commandKnownLocal
			case straceLongOptionRequired:
				if !hasValue {
					index++
					if index >= len(args) {
						return -1, commandUnresolved
					}
				}
			case straceLongOptionOptional:
				// GNU getopt_long accepts optional values only as --option=value.
			case straceLongOptionValueless:
				if hasValue {
					return -1, commandUnresolved
				}
			default:
				return -1, commandUnresolved
			}
			continue
		}
		if strings.HasPrefix(arg, "-") {
			cluster := []rune(arg[1:])
			for clusterIndex, option := range cluster {
				switch {
				case option == 'h' || option == 'V':
					return -1, commandKnownLocal
				case strings.ContainsRune("abeEIoOpPsSuUX", option):
					if clusterIndex+1 == len(cluster) {
						index++
						if index >= len(args) {
							return -1, commandUnresolved
						}
					}
					clusterIndex = len(cluster)
				case strings.ContainsRune("AcCdDfFiknNqrtTvwxyYzZ", option):
				default:
					return -1, commandUnresolved
				}
				if clusterIndex == len(cluster) {
					break
				}
			}
			continue
		}
		return index, commandKnownLocal
	}
	return -1, commandKnownLocal
}

func resolveStraceLongOption(name string) (straceLongOptionKind, bool) {
	if kind := exactStraceLongOption(name); kind != straceLongOptionInvalid {
		return kind, true
	}
	matchedKind := straceLongOptionInvalid
	matches := 0
	visit := func(options map[string]bool, kind straceLongOptionKind) {
		for option := range options {
			if strings.HasPrefix(option, name) {
				matchedKind = kind
				matches++
			}
		}
	}
	visit(straceRequiredLongOptions, straceLongOptionRequired)
	visit(straceOptionalLongOptions, straceLongOptionOptional)
	visit(straceValuelessLongOptions, straceLongOptionValueless)
	for _, option := range []string{"--help", "--version"} {
		if strings.HasPrefix(option, name) {
			matchedKind = straceLongOptionTerminal
			matches++
		}
	}
	return matchedKind, matches == 1
}

func exactStraceLongOption(name string) straceLongOptionKind {
	switch {
	case straceRequiredLongOptions[name]:
		return straceLongOptionRequired
	case straceOptionalLongOptions[name]:
		return straceLongOptionOptional
	case straceValuelessLongOptions[name]:
		return straceLongOptionValueless
	case name == "--help" || name == "--version":
		return straceLongOptionTerminal
	default:
		return straceLongOptionInvalid
	}
}

// isNumericToken reports whether a token is purely digits (e.g. the duration
// argument of `timeout 5`), so wrapper-argument scanning can skip it.
func isNumericToken(field string) bool {
	if field == "" {
		return false
	}
	for _, r := range field {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// shellDashCPayload returns the command string passed to `sh -c`/`bash -c`
// (and other POSIX shells) so the caller can recurse into it, or "" when the
// segment is not a `<shell> -c <payload>` invocation. The payload is returned
// with one layer of surrounding quotes stripped.
func shellDashCPayload(program string, fields []string) string {
	switch program {
	case "sh", "bash", "zsh", "ksh", "dash":
	default:
		return ""
	}
	start := programIndex(program, fields)
	if start < 0 {
		return ""
	}
	args := fields[start+1:]
	if payloadIndex, found := shellCommandPayloadIndex(program, args); found && payloadIndex < len(args) {
		return strings.Trim(args[payloadIndex], `"'`)
	}
	return ""
}

// normalizeProgramToken reduces a raw command token to a bare, lowercased program
// name: it strips shell quoting/escaping characters (", ', `, \) wherever they
// appear in the token (including embedded ones like `vi\m` or `v"i"m`), strips
// leading command-substitution markers, removes any directory prefix (so
// /usr/bin/vim and C:\tools\vim.exe match "vim"), removes Windows executable
// suffixes, and lowercases. This closes path/quote/substitution evasions of the
// detector and keeps curl.exe/npm.cmd equivalent to curl/npm for risk analysis.
func normalizeProgramToken(field string) string {
	token := strings.TrimSpace(field)
	token = strings.TrimLeft(token, "$(")
	token = strings.TrimRight(token, ")")
	// PowerShell commonly invokes drive-letter, UNC, and relative paths. Extract
	// their basename while backslashes still carry path-separator meaning;
	// removing them first would turn C:\tools\curl.exe into c:toolscurl.exe and
	// bypass the canonical curl risk entry.
	pathToken := stripChars(token, "\"'`")
	if windowsPathBasename, ok := windowsExecutablePathBasename(pathToken); ok {
		token = windowsPathBasename
	} else {
		// Strip shell quoting/escaping characters (", ', `, \) wherever they
		// appear in the token — surrounding, embedded, or as a mid-word escape
		// — so "vim", v"i"m, 'v'im, and vi\m all collapse to the program name.
		token = stripChars(token, "\"'`\\")
		// Strip a directory prefix so /usr/bin/vim reduces to the basename.
		if i := strings.LastIndex(token, "/"); i >= 0 {
			token = token[i+1:]
		}
	}
	return trimExecutableSuffix(strings.ToLower(token))
}

// executableSuffixes are the Windows executable extensions stripped from a
// program token so curl.exe, git.cmd, and curl all normalize to one name. Both
// the parseable path (normalizeProgramToken) and the unparseable fallback
// (executableTokenBase) strip the same set; a token that normalizes differently
// on the two paths is exactly how a command slips past the fallback.
var executableSuffixes = []string{".exe", ".cmd", ".bat", ".com"}

// trimExecutableSuffix removes one trailing Windows executable extension from an
// already-lowercased token.
func trimExecutableSuffix(token string) string {
	for _, suffix := range executableSuffixes {
		if strings.HasSuffix(token, suffix) {
			return strings.TrimSuffix(token, suffix)
		}
	}
	return token
}

func windowsExecutablePathBasename(token string) (string, bool) {
	drivePath := len(token) >= 3 &&
		((token[0] >= 'a' && token[0] <= 'z') || (token[0] >= 'A' && token[0] <= 'Z')) &&
		token[1] == ':'
	uncPath := strings.HasPrefix(token, `\\`)
	explicitRelativePath := strings.HasPrefix(token, `.\`) || strings.HasPrefix(token, `..\`)
	backslashRelativePath := strings.ContainsRune(token, '\\') && hasWindowsExecutableSuffix(token)
	if !drivePath && !uncPath && !explicitRelativePath && !backslashRelativePath {
		return "", false
	}
	index := strings.LastIndexAny(token, `\/`)
	if index >= 0 {
		if index+1 >= len(token) {
			return "", false
		}
		return token[index+1:], true
	}
	if drivePath && len(token) > 2 {
		return token[2:], true
	}
	return "", false
}

func hasWindowsExecutableSuffix(token string) bool {
	lowered := strings.ToLower(token)
	for _, suffix := range executableSuffixes {
		if strings.HasSuffix(lowered, suffix) {
			return true
		}
	}
	return false
}

// stripChars returns s with every rune in cutset removed.
func stripChars(s, cutset string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(cutset, r) {
			return -1
		}
		return r
	}, s)
}

// hasNonInteractiveFlag reports whether a REPL-style program was invoked in a
// non-interactive way (an inline expression/script flag or a positional script
// argument), in which case it will not hang.
func hasNonInteractiveFlag(program string, fields []string) bool {
	flags, isREPL := nonInteractiveREPLFlags[program]
	if !isREPL {
		// SSH and friends are interactive only with no trailing command. If
		// there is an argument that is not an option, treat it as a remote
		// command/host+command and let it through for ssh-like programs.
		return hasTrailingCommand(program, fields)
	}
	// Find the program's own index, then inspect the args after it.
	start := programIndex(program, fields)
	if start < 0 {
		return false
	}
	for _, arg := range fields[start+1:] {
		// Universal print-and-exit flags (`--version`/`--help`) make the program
		// print and quit rather than open a prompt, for every repl program.
		base := arg
		if i := strings.IndexByte(base, '='); i >= 0 {
			base = base[:i]
		}
		if infoExitFlags[base] {
			return true
		}
		for _, flag := range flags {
			if arg == flag || strings.HasPrefix(arg, flag+"=") {
				return true
			}
		}
		// A positional (non-flag) argument means a script path was supplied.
		if !strings.HasPrefix(arg, "-") {
			return true
		}
	}
	return false
}

// hasTrailingCommand handles ssh/telnet/db clients: presence of a trailing
// non-option argument (beyond the host) implies a one-shot command rather than
// an interactive session.
func hasTrailingCommand(program string, fields []string) bool {
	start := programIndex(program, fields)
	if start < 0 {
		return false
	}
	args := fields[start+1:]
	switch program {
	case "ssh":
		// ssh <host> <command...>: a host plus at least one more token.
		positional := 0
		for _, arg := range args {
			if !strings.HasPrefix(arg, "-") {
				positional++
			}
		}
		return positional >= 2
	case "sqlite3":
		// sqlite3 <db> <sql>: a db plus an SQL argument.
		positional := 0
		for _, arg := range args {
			if !strings.HasPrefix(arg, "-") {
				positional++
			}
		}
		return positional >= 2
	case "redis-cli":
		// redis-cli <command ...>: any positional command token.
		for _, arg := range args {
			if !strings.HasPrefix(arg, "-") {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func programIndex(program string, fields []string) int {
	for index, field := range fields {
		// Normalize each field the SAME way firstProgram does (basename + strip
		// quotes/escapes + lowercase) so a full-path invocation like
		// /usr/bin/python or /bin/bash matches the normalized program name —
		// otherwise hasNonInteractiveFlag / shellDashCPayload can't locate the
		// program and mis-classify (false positives and missed detections).
		if normalizeProgramToken(field) == program {
			return index
		}
	}
	return -1
}

// splitShellSegments splits a command on the common shell operators (&&, ||, ;,
// |) and command-substitution boundaries ($(...), `...`) so each pipeline/list
// element — and any interactive program hidden inside a substitution (e.g.
// `echo $(vim x)`) — can be inspected independently.
//
// It is quote-aware, which the previous strings.Replacer was not: an operator
// inside quotes is a literal, not a separator, so `git commit -m "use top | less"`
// and `echo "a; vim b"` no longer split mid-argument and falsely flag less/vim.
// The quoting rules mirror the shell:
//   - single quotes make everything literal (no operator OR substitution inside);
//   - double quotes keep $(...)/`...` substitution active but make |, ;, &&
//     literal;
//   - unquoted text splits on everything.
//
// Real, unquoted operators still split, so a genuinely interactive command behind
// a separator is still caught (no new false negatives).
func splitShellSegments(command string) []string {
	segments := make([]string, 0)
	var current strings.Builder
	flush := func() {
		if seg := strings.TrimSpace(current.String()); seg != "" {
			segments = append(segments, seg)
		}
		current.Reset()
	}

	inSingle := false
	inDouble := false
	// substStack saves the quoting state entering each command substitution (`$(`
	// or a backtick) so the substitution runs in a fresh quoting context (POSIX)
	// and the outer quoting is restored when it closes. backtick marks which
	// delimiter opened the frame so `)` only closes `$(` frames and a backtick only
	// closes a backtick frame.
	type substFrame struct {
		inSingle, inDouble bool
		backtick           bool
	}
	var substStack []substFrame
	runes := []rune(command)
	for i := 0; i < len(runes); i++ {
		c := runes[i]

		// Inside single quotes everything is literal until the closing quote.
		if inSingle {
			current.WriteRune(c)
			if c == '\'' {
				inSingle = false
			}
			continue
		}
		// Outside single quotes a backslash escapes the next rune (the shell treats
		// \X as a literal X), so an escaped quote or operator must not toggle quoting
		// or manufacture a segment boundary — e.g. echo foo\|less or "a \" b".
		if c == '\\' && i+1 < len(runes) {
			current.WriteRune(c)
			current.WriteRune(runes[i+1])
			i++
			continue
		}
		if c == '\'' && !inDouble {
			inSingle = true
			current.WriteRune(c)
			continue
		}
		if c == '"' {
			inDouble = !inDouble
			current.WriteRune(c)
			continue
		}

		// Command substitution boundaries. `$(` opens a substitution in a fresh
		// quoting context; its matching `)` closes it and restores the outer
		// quoting. A `)` is only a boundary when it actually closes an active,
		// UNQUOTED substitution — a `)` inside quotes (e.g. echo $(printf "a)
		// less") or a bare "a) less") is literal and must not split.
		switch {
		case c == '`':
			// A backtick opens a substitution, or closes the active one if the top
			// frame is itself a backtick. Saving/restoring quote state means inner
			// separators in `echo "`a | less`"` are seen instead of staying hidden
			// by the outer double quotes.
			if n := len(substStack); n > 0 && substStack[n-1].backtick {
				prev := substStack[n-1]
				substStack = substStack[:n-1]
				inSingle, inDouble = prev.inSingle, prev.inDouble
				flush()
				continue
			}
			flush()
			substStack = append(substStack, substFrame{inSingle, inDouble, true})
			inSingle, inDouble = false, false
			continue
		case c == '$' && i+1 < len(runes) && runes[i+1] == '(':
			flush()
			substStack = append(substStack, substFrame{inSingle, inDouble, false})
			inSingle, inDouble = false, false
			i++ // consume the '('
			continue
		case c == ')' && len(substStack) > 0 && !substStack[len(substStack)-1].backtick && !inSingle && !inDouble:
			prev := substStack[len(substStack)-1]
			substStack = substStack[:len(substStack)-1]
			inSingle, inDouble = prev.inSingle, prev.inDouble
			flush()
			continue
		}

		// Control operators are literal inside double quotes; only split on them
		// when fully unquoted. Match the original operator set exactly: ;, | (which
		// also covers ||), and && — never a lone & (background).
		if !inDouble {
			switch {
			case c == ';' || c == '|':
				flush()
				continue
			case c == '&' && i+1 < len(runes) && runes[i+1] == '&':
				flush()
				i++
				continue
			}
		}

		current.WriteRune(c)
	}
	flush()
	return segments
}

func normalizeWhitespace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}
