package sandbox

import (
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// THE NESTED-REPOSITORY GUARD CLASSIFIES THE COMMAND GIT RUNS, NOT THE ONE
// TYPED. Git rewrites its argument vector before choosing a builtin: an alias
// defined on the command line, in the environment or through --config-env is
// split with shell-style quoting, its own leading global options are applied,
// and the result is looked up again until a builtin is reached. Reading the
// literal token, or the first whitespace-separated field of an alias value,
// let `git -c alias.a='-c alias.b=init b' a .` and `git -c 'alias.a="init"' a .`
// create a repository the workspace profile carves nothing out for. What
// follows models that rewrite, and where the model runs out it refuses rather
// than guesses: under this guard the safe answer to "could this make a
// repository" is yes. Reported by @gnanam1990.

// gitAliasTable maps a lowercased alias name to every definition the command
// supplies for it. Git keeps one value per key, the last one written, and the
// order in which the environment, -c and --config-env feed that list is not
// something this guard needs to reproduce: a definition git would discard but
// which reaches init here costs one refusal, while the opposite mistake costs
// a repository. So every definition is kept, and any of them reaching init
// refuses. For the same reason an alias that shadows a builtin, which git
// ignores, is followed here rather than skipped: this file carries no list of
// git's commands, and refusing `-c alias.status=init status` loses nothing.
type gitAliasTable map[string][]gitAliasDefinition

// gitAliasDefinition is one alias value. unknown marks a definition whose text
// cannot be read statically, such as --config-env=alias.x=VAR or a
// GIT_CONFIG_VALUE_n written as $var: git will run whatever it holds, and
// this guard cannot say what that is.
type gitAliasDefinition struct {
	text    string
	unknown bool
}

func (table gitAliasTable) define(name, text string) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return
	}
	table[name] = append(table[name], gitAliasDefinition{text: strings.ToLower(text)})
}

func (table gitAliasTable) defineUnknown(name string) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return
	}
	table[name] = append(table[name], gitAliasDefinition{unknown: true})
}

// defineSetting records a `key=value` config setting when key names an alias.
// readable is false when the value is only known by reference, which makes the
// alias unknown rather than absent. A setting without "=" is a boolean in git
// (`alias.x` expands to "true") and defines nothing worth following.
func (table gitAliasTable) defineSetting(setting string, readable bool) {
	name, value, found := strings.Cut(setting, "=")
	if !found {
		return
	}
	alias, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(name)), "alias.")
	if !ok {
		return
	}
	if !readable {
		table.defineUnknown(alias)
		return
	}
	table.define(alias, value)
}

// splitGitAliasExpansion tokenizes an alias value the way git's split_cmdline
// does: whitespace separates words, a single or double quote opens a quoted
// span anywhere inside a word, and a backslash outside single quotes escapes
// the next byte. An unclosed quote or a trailing backslash is a fatal error in
// git ("bad alias string"), so ok is false and nothing would have run.
func splitGitAliasExpansion(text string) (argv []string, ok bool) {
	var current strings.Builder
	var quoted byte
	inWord := false
	for index := 0; index < len(text); index++ {
		c := text[index]
		switch {
		case quoted == 0 && isGitSpace(c):
			if inWord {
				argv = append(argv, current.String())
				current.Reset()
				inWord = false
			}
		case quoted == 0 && (c == '\'' || c == '"'):
			quoted = c
			inWord = true
		case c == quoted:
			quoted = 0
		default:
			if c == '\\' && quoted != '\'' {
				index++
				if index >= len(text) {
					return nil, false
				}
				c = text[index]
			}
			current.WriteByte(c)
			inWord = true
		}
	}
	if quoted != 0 {
		return nil, false
	}
	if inWord {
		argv = append(argv, current.String())
	}
	return argv, true
}

func isGitSpace(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	}
	return false
}

// gitAliasExpansionSubcommand runs a split expansion through git's own
// handling of global options inside an alias and returns the subcommand git
// would look up next. Inside an expansion git accepts -c and --config-env,
// which push settings the next lookup sees (that is how `-c alias.b=init b`
// reaches init), and the pager switches, which change nothing that runs. An
// option that changes the environment (-C, --git-dir, --bare, ...) is fatal
// inside an alias, and an expansion made only of options is an empty alias;
// both return ok false. So does an option this guard does not model, because
// "git would refuse this" and "this guard cannot read this" end the same way.
func gitAliasExpansionSubcommand(argv []string, aliases gitAliasTable) (subcommand string, ok bool) {
	for index := 0; index < len(argv); index++ {
		word := argv[index]
		if word == "" {
			return "", false
		}
		if !strings.HasPrefix(word, "-") {
			return word, true
		}
		switch {
		case word == "-c", word == "--config-env":
			if index+1 >= len(argv) {
				return "", false
			}
			index++
			aliases.defineSetting(argv[index], word == "-c")
		case strings.HasPrefix(word, "--config-env="):
			aliases.defineSetting(strings.TrimPrefix(word, "--config-env="), false)
		case word == "-p", word == "--paginate", word == "-P", word == "--no-pager":
		default:
			return "", false
		}
	}
	return "", false
}

// gitSubcommandCreatesRepository reports whether subcommand, resolved through
// aliases the way git resolves it, is init, init-db or clone. The chain is
// followed to its end with the names on the current path kept, so a cycle is a
// refusal here the way "alias loop detected" is fatal in git. A shell alias
// runs a program this analyzer cannot classify; an expansion git cannot split,
// cannot run, or that leaves no subcommand behind is the same case, and so is
// a definition whose text is not readable. Every one of those refuses.
func gitSubcommandCreatesRepository(subcommand string, aliases gitAliasTable) bool {
	return gitAliasPathCreatesRepository(subcommand, aliases, nil)
}

func gitAliasPathCreatesRepository(subcommand string, aliases gitAliasTable, path []string) bool {
	switch subcommand {
	case "init", "init-db", "clone":
		return true
	}
	definitions, ok := aliases[subcommand]
	if !ok {
		return false
	}
	for _, seen := range path {
		if seen == subcommand {
			return true
		}
	}
	path = append(path, subcommand)
	for _, definition := range definitions {
		if definition.unknown || strings.HasPrefix(definition.text, "!") {
			return true
		}
		argv, ok := splitGitAliasExpansion(definition.text)
		if !ok {
			return true
		}
		next, ok := gitAliasExpansionSubcommand(argv, aliases)
		if !ok {
			return true
		}
		if gitAliasPathCreatesRepository(next, aliases, path) {
			return true
		}
	}
	return false
}

// gitInlineAliases records the alias definitions supplied ahead of the
// subcommand: `-c alias.NAME=EXPANSION`, and `--config-env alias.NAME=VAR` in
// both spellings, whose value lives in a variable this analyzer does not read.
// Git accepts no attached `-cNAME=VALUE` spelling ("unknown option"), so none
// is recognised here. Other global options that take a value are stepped over
// the way gitSubcommand steps over them, so a value that happens to look like
// a subcommand does not end the scan early.
func gitInlineAliases(aliases gitAliasTable, words []string) {
	for index := 0; index < len(words); index++ {
		word := words[index]
		if word == "" {
			continue
		}
		if !strings.HasPrefix(word, "-") {
			return
		}
		switch {
		case word == "-c", word == "--config-env":
			if index+1 < len(words) {
				index++
				aliases.defineSetting(words[index], word == "-c")
			}
		case strings.HasPrefix(word, "--config-env="):
			aliases.defineSetting(strings.TrimPrefix(word, "--config-env="), false)
		case strings.HasPrefix(word, "--") && strings.Contains(word, "="):
		case gitGlobalOptionsTakingValue[word]:
			index++
		}
	}
}

// shellAssignment is one NAME=value the command hands to git's environment.
// readable is false when the value is an expansion this analyzer cannot see.
type shellAssignment struct {
	value    string
	readable bool
}

// callAssignments collects the assignments in force for the program: the
// shell prefix (FOO=bar git ...) and the NAME=value words an env wrapper
// forwards (env FOO=bar git ...), which sit ahead of the program word.
func callAssignments(call *syntax.CallExpr, rest []*syntax.Word) map[string]shellAssignment {
	assignments := map[string]shellAssignment{}
	if call == nil {
		return assignments
	}
	for _, assign := range call.Assigns {
		if assign == nil || assign.Name == nil {
			continue
		}
		readable := !assign.Append && assign.Array == nil && assign.Index == nil &&
			(assign.Value == nil || isLiteralWord(assign.Value))
		assignments[assign.Name.Value] = shellAssignment{value: wordText(assign.Value), readable: readable}
	}
	program := len(call.Args) - len(rest) - 1
	for index := 0; index < program && index < len(call.Args); index++ {
		name, value, found := strings.Cut(wordText(call.Args[index]), "=")
		if !found || !isEnvName(name) {
			continue
		}
		assignments[name] = shellAssignment{value: value, readable: isLiteralWord(call.Args[index])}
	}
	return assignments
}

func isEnvName(name string) bool {
	if name == "" {
		return false
	}
	for index, r := range name {
		switch {
		case r == '_', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && index > 0:
		default:
			return false
		}
	}
	return true
}

// gitEnvironmentAliases records the alias definitions the command hands git
// through its environment, which git reads exactly as it reads -c:
// GIT_CONFIG_COUNT with GIT_CONFIG_KEY_n and GIT_CONFIG_VALUE_n, and
// GIT_CONFIG_PARAMETERS. It reports false when a definition this guard needs
// cannot be read: a count, a key or the parameter list written as an
// expansion, or the value of an alias key written that way. A count git would
// reject, or a numbered key with no value, is reported the same way; git dies
// on those, so refusing them costs nothing.
func gitEnvironmentAliases(aliases gitAliasTable, env map[string]shellAssignment) bool {
	if parameters, ok := env["GIT_CONFIG_PARAMETERS"]; ok {
		if !parameters.readable {
			return false
		}
		settings, ok := splitGitConfigParameters(parameters.value)
		if !ok {
			return false
		}
		for _, setting := range settings {
			aliases.defineSetting(setting, true)
		}
	}
	count, ok := env["GIT_CONFIG_COUNT"]
	if !ok {
		return true
	}
	if !count.readable {
		return false
	}
	n, err := strconv.Atoi(strings.TrimSpace(count.value))
	if err != nil || n < 0 || n > 1024 {
		return false
	}
	for index := 0; index < n; index++ {
		key, ok := env["GIT_CONFIG_KEY_"+strconv.Itoa(index)]
		if !ok || !key.readable {
			return false
		}
		value, ok := env["GIT_CONFIG_VALUE_"+strconv.Itoa(index)]
		if !ok {
			return false
		}
		aliases.defineSetting(key.value+"="+value.value, value.readable)
	}
	return true
}

// splitGitConfigParameters dequotes GIT_CONFIG_PARAMETERS the way git does:
// whitespace-separated entries, each single-quoted, with `'\”` and `'\!'`
// standing for a quote or a bang inside one. Anything else is "bogus format"
// in git, a fatal error, so ok is false.
func splitGitConfigParameters(text string) (settings []string, ok bool) {
	index := 0
	for {
		for index < len(text) && isGitSpace(text[index]) {
			index++
		}
		if index >= len(text) {
			return settings, true
		}
		if text[index] != '\'' {
			return nil, false
		}
		index++
		var current strings.Builder
		for {
			if index >= len(text) {
				return nil, false
			}
			c := text[index]
			index++
			if c != '\'' {
				current.WriteByte(c)
				continue
			}
			if index+2 < len(text) && text[index] == '\\' && (text[index+1] == '\'' || text[index+1] == '!') && text[index+2] == '\'' {
				current.WriteByte(text[index+1])
				index += 3
				continue
			}
			if index >= len(text) || isGitSpace(text[index]) {
				settings = append(settings, current.String())
				break
			}
			return nil, false
		}
	}
}
