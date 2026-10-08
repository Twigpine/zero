package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

type completionNode struct {
	names    []string
	flags    []string
	children []completionNode
}

type completionContext struct {
	path        string
	candidates  []string
	transitions map[string]string
}

var completionRoot = completionNode{
	flags: []string{"-h", "--help", "-v", "--version", "-p", "--prompt", "--add-dir", "--allow-escalation", "--theme", "--skip-permissions-unsafe"},
	children: []completionNode{
		{names: []string{"exec"}, flags: []string{
			"-h", "--help", "-f", "--file", "--image", "--add-dir", "--mode", "-m", "--model",
			"--use-spec", "--spec-model", "--spec-reasoning-effort", "--plan", "--permission-mode",
			"--max-turns", "--exec-profile",
			"--auto", "--enabled-tools", "--disabled-tools", "--list-tools", "--profile", "-r",
			"--reasoning-effort", "-C", "--cwd", "-w", "--worktree", "--worktree-dir", "-i",
			"--input-format", "-o", "--output-format", "--prompt", "--resume", "--fork",
			"--calling-session-id", "--calling-tool-use-id", "--tag", "--depth", "--session-title",
			"--init-session-id", "--skip-permissions-unsafe", "--allow-escalation", "--self-correct",
			"--notify", "--no-notify", "--no-completion-gate",
		}},
		{
			names:    []string{"completions"},
			flags:    []string{"-h", "--help"},
			children: leafNodes("bash", "zsh", "fish", "powershell", "elvish"),
		},
		{names: []string{"daemon"}, children: leafNodes("start", "stop", "status", "run", "attach", "serve-remote", "link")},
		{names: []string{"setup"}},
		{names: []string{"config"}},
		{names: []string{"models"}, children: []completionNode{{names: []string{"list", "ls"}}}},
		{names: []string{"providers"}, children: []completionNode{
			{names: []string{"current"}}, {names: []string{"list"}}, {names: []string{"catalog"}},
			{names: []string{"add"}}, {names: []string{"check"}}, {names: []string{"use"}},
			{names: []string{"remove", "rm"}}, {names: []string{"rename"}}, {names: []string{"setup"}},
			{names: []string{"detect"}}, {names: []string{"models"}},
		}},
		{names: []string{"doctor"}},
		{names: []string{"context"}},
		{names: []string{"repo-map", "repomap"}},
		{names: []string{"search", "find"}},
		{names: []string{"sessions", "session"}, children: leafNodes("list", "children", "lineage", "tree", "rewind-plan", "rewind", "compact-plan")},
		{names: []string{"spec"}, children: leafNodes("show", "approve", "reject")},
		{names: []string{"init"}},
		{names: []string{"specialists", "specialist"}, children: []completionNode{
			{names: []string{"list"}}, {names: []string{"show"}}, {names: []string{"create"}},
			{names: []string{"delete", "rm"}}, {names: []string{"edit"}}, {names: []string{"path"}},
		}},
		{names: []string{"plugins", "plugin"}, children: []completionNode{
			{names: []string{"list"}}, {names: []string{"add"}}, {names: []string{"info"}}, {names: []string{"remove", "rm"}},
		}},
		{names: []string{"backends", "backend"}, children: leafNodes("doctor")},
		{names: []string{"skills", "skill"}, children: []completionNode{
			{names: []string{"list"}}, {names: []string{"add"}}, {names: []string{"info"}}, {names: []string{"remove", "rm"}},
		}},
		{names: []string{"tools", "tool"}, children: leafNodes("make", "list")},
		{names: []string{"hooks"}, children: []completionNode{
			{names: []string{"list"}}, {names: []string{"add"}}, {names: []string{"remove", "rm"}},
			{names: []string{"enable"}}, {names: []string{"disable"}},
		}},
		{names: []string{"mcp"}, children: []completionNode{
			{names: []string{"add"}}, {names: []string{"remove", "rm"}}, {names: []string{"enable"}},
			{names: []string{"disable"}}, {names: []string{"check"}}, {names: []string{"list"}},
			{names: []string{"permissions"}, children: leafNodes("list", "revoke", "clear")},
			{names: []string{"tools"}, children: leafNodes("list")},
			{names: []string{"oauth"}, children: leafNodes("login", "logout", "status")},
		}},
		{names: []string{"auth"}, children: append(leafNodes("openrouter", "chatgpt", "login", "logout", "status", "refresh"),
			completionNode{names: []string{"reset"}, flags: []string{"--confirm", "--json", "--help"}})},
		{names: []string{"sandbox"}, children: []completionNode{
			{names: []string{"policy"}}, {names: []string{"setup"}}, {names: []string{"check"}},
			{names: []string{"grants"}, children: leafNodes("list", "allow", "deny", "revoke", "clear")},
		}},
		{names: []string{"update"}},
		{names: []string{"upgrade"}},
		{names: []string{"worktrees", "worktree"}, children: leafNodes("prepare", "release")},
		{names: []string{"verify"}},
		{names: []string{"trust"}, children: leafNodes("list", "remove")},
		{names: []string{"eval"}, children: leafNodes("validate", "run", "bench")},
		{names: []string{"changes", "change"}, children: []completionNode{
			{names: []string{"inspect", "status"}}, {names: []string{"commit"}}, {names: []string{"push"}},
			{names: []string{"pr", "pull-request"}},
		}},
		{names: []string{"usage"}, children: leafNodes("report")},
		{names: []string{"cron"}, children: leafNodes("add", "list", "rm", "pause", "resume", "run")},
		{names: []string{"repo-info", "repoinfo"}},
		{names: []string{"serve"}},
		{names: []string{"acp"}},
		{names: []string{"help"}},
		{names: []string{"version"}},
	},
}

func leafNodes(names ...string) []completionNode {
	nodes := make([]completionNode, 0, len(names))
	for _, name := range names {
		nodes = append(nodes, completionNode{names: []string{name}})
	}
	return nodes
}

func runCompletions(args []string, stdout io.Writer, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		if err := writeCompletionsHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if len(args) == 0 {
		return writeExecUsageError(stderr, "shell required. Use `zero completions <bash|zsh|fish|powershell|elvish>`.")
	}
	if len(args) != 1 {
		return writeExecUsageError(stderr, fmt.Sprintf("unexpected completions argument %q", args[1]))
	}

	contexts := completionContexts(completionRoot)
	var err error
	switch strings.ToLower(strings.TrimSpace(args[0])) {
	case "bash":
		err = writeBashCompletions(stdout, contexts)
	case "zsh":
		err = writeZshCompletions(stdout, contexts)
	case "fish":
		err = writeFishCompletions(stdout, contexts)
	case "powershell":
		err = writePowerShellCompletions(stdout, contexts)
	case "elvish":
		err = writeElvishCompletions(stdout, contexts)
	default:
		return writeExecUsageError(stderr, fmt.Sprintf("unsupported shell %q; expected bash, zsh, fish, powershell, or elvish", args[0]))
	}
	if err != nil {
		return exitCrash
	}
	return exitSuccess
}

func writeCompletionsHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Generate shell completion scripts.

Usage:
  zero completions <shell>

Arguments:
  <shell>  Target shell: bash, zsh, fish, powershell, or elvish

Examples:
  source <(zero completions bash)
  source <(zero completions zsh)
  zero completions fish > ~/.config/fish/completions/zero.fish
  zero completions powershell >> $PROFILE
  eval (zero completions elvish | slurp)

Flags:
  -h, --help  Show this help
`)
	return err
}

func completionContexts(root completionNode) []completionContext {
	contexts := []completionContext{}
	var visit func(completionNode, []string)
	visit = func(node completionNode, paths []string) {
		candidates := append([]string{}, node.flags...)
		transitions := map[string]string{}
		for _, child := range node.children {
			candidates = append(candidates, child.names...)
			for _, path := range paths {
				for _, name := range child.names {
					childPath := strings.TrimSpace(path + " " + name)
					transitions[path+"|"+name] = childPath
				}
			}
		}
		candidates = uniqueStrings(candidates)
		for _, path := range paths {
			contexts = append(contexts, completionContext{path: path, candidates: candidates, transitions: transitions})
		}
		for _, child := range node.children {
			childPaths := make([]string, 0, len(paths)*len(child.names))
			for _, path := range paths {
				for _, name := range child.names {
					childPaths = append(childPaths, strings.TrimSpace(path+" "+name))
				}
			}
			visit(child, childPaths)
		}
	}
	visit(root, []string{""})
	sort.SliceStable(contexts, func(i, j int) bool { return contexts[i].path < contexts[j].path })
	return contexts
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func allTransitions(contexts []completionContext) map[string]string {
	transitions := map[string]string{}
	for _, context := range contexts {
		for key, value := range context.transitions {
			transitions[key] = value
		}
	}
	return transitions
}

func sortedTransitionKeys(transitions map[string]string) []string {
	keys := make([]string, 0, len(transitions))
	for key := range transitions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func shellWords(values []string) string {
	return strings.Join(values, " ")
}

func writeBashCompletions(w io.Writer, contexts []completionContext) error {
	if _, err := fmt.Fprintln(w, "# bash completion for zero\n_zero() {\n  local cur context token i candidates\n  cur=\"${COMP_WORDS[COMP_CWORD]}\"\n  context=\"\""); err != nil {
		return err
	}
	if err := writePOSIXContextLoop(w, contexts, "  for ((i = 1; i < COMP_CWORD; i++)); do", "    token=\"${COMP_WORDS[i]}\"", "  done"); err != nil {
		return err
	}
	if err := writePOSIXCandidateCase(w, contexts, "  "); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w, "  COMPREPLY=( $(compgen -W \"$candidates\" -- \"$cur\") )\n}\ncomplete -F _zero zero")
	return err
}

func writeZshCompletions(w io.Writer, contexts []completionContext) error {
	if _, err := fmt.Fprintln(w, "#compdef zero\n_zero() {\n  local context token i\n  local -a candidates\n  context=\"\""); err != nil {
		return err
	}
	if err := writePOSIXContextLoop(w, contexts, "  for ((i = 2; i < CURRENT; i++)); do", "    token=\"${words[i]}\"", "  done"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "  case \"$context\" in"); err != nil {
		return err
	}
	for _, context := range contexts {
		if _, err := fmt.Fprintf(w, "    %q) candidates=(%s) ;;\n", context.path, quotedWords(context.candidates)); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w, "  esac\n  compadd -- \"${candidates[@]}\"\n}\ncompdef _zero zero")
	return err
}

func writePOSIXContextLoop(w io.Writer, contexts []completionContext, start string, token string, end string) error {
	if _, err := fmt.Fprintln(w, start); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, token); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "    case \"$context|$token\" in"); err != nil {
		return err
	}
	transitions := allTransitions(contexts)
	for _, key := range sortedTransitionKeys(transitions) {
		if _, err := fmt.Fprintf(w, "      %q) context=%q ;;\n", key, transitions[key]); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w, "    esac"); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w, end)
	return err
}

func writePOSIXCandidateCase(w io.Writer, contexts []completionContext, indent string) error {
	if _, err := fmt.Fprintln(w, indent+"case \"$context\" in"); err != nil {
		return err
	}
	for _, context := range contexts {
		if _, err := fmt.Fprintf(w, "%s  %q) candidates=%q ;;\n", indent, context.path, shellWords(context.candidates)); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w, indent+"esac")
	return err
}

func quotedWords(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, fmt.Sprintf("%q", value))
	}
	return strings.Join(quoted, " ")
}

func writeFishCompletions(w io.Writer, contexts []completionContext) error {
	if _, err := fmt.Fprintln(w, "# fish completion for zero\nfunction __zero_completion_context\n    set -l context ''\n    set -l tokens (commandline -opc)"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "    for token in $tokens[2..-1]\n        switch \"$context|$token\""); err != nil {
		return err
	}
	transitions := allTransitions(contexts)
	for _, key := range sortedTransitionKeys(transitions) {
		if _, err := fmt.Fprintf(w, "            case %q\n                set context %q\n", key, transitions[key]); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w, "        end\n    end\n    if test -z \"$context\"\n        echo __root__\n    else\n        echo \"$context\"\n    end\nend"); err != nil {
		return err
	}
	for _, context := range contexts {
		path := context.path
		if path == "" {
			path = "__root__"
		}
		if _, err := fmt.Fprintf(w, "complete -c zero -f -n 'test (__zero_completion_context) = %q' -a %q\n", path, shellWords(context.candidates)); err != nil {
			return err
		}
	}
	return nil
}

func writePowerShellCompletions(w io.Writer, contexts []completionContext) error {
	if _, err := fmt.Fprintln(w, "# PowerShell completion for zero\nRegister-ArgumentCompleter -Native -CommandName zero -ScriptBlock {\n    param($wordToComplete, $commandAst, $cursorPosition)\n    $context = ''\n    $elements = @($commandAst.CommandElements)\n    $limit = $elements.Count\n    if ($wordToComplete -ne '' -and $limit -gt 1) { $limit-- }\n    for ($i = 1; $i -lt $limit; $i++) {\n        $token = $elements[$i].Extent.Text\n        switch (\"$context|$token\") {"); err != nil {
		return err
	}
	transitions := allTransitions(contexts)
	for _, key := range sortedTransitionKeys(transitions) {
		if _, err := fmt.Fprintf(w, "            '%s' { $context = '%s'; break }\n", key, transitions[key]); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w, "        }\n    }\n    $candidates = switch ($context) {"); err != nil {
		return err
	}
	for _, context := range contexts {
		if _, err := fmt.Fprintf(w, "        '%s' { @(%s) }\n", context.path, powershellWords(context.candidates)); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w, "    }\n    $candidates | Where-Object { $_ -like \"$wordToComplete*\" } | ForEach-Object {\n        [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_)\n    }\n}")
	return err
}

func powershellWords(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, "'"+strings.ReplaceAll(value, "'", "''")+"'")
	}
	return strings.Join(quoted, ", ")
}

func writeElvishCompletions(w io.Writer, contexts []completionContext) error {
	if _, err := fmt.Fprintln(w, "# Elvish completion for zero\nset edit:completion:arg-completer[zero] = {|@args|\n    var context = ''\n    for token $args[1..-1] {"); err != nil {
		return err
	}
	transitions := allTransitions(contexts)
	for index, key := range sortedTransitionKeys(transitions) {
		keyword := "if"
		if index > 0 {
			keyword = "} elif"
		}
		if _, err := fmt.Fprintf(w, "        %s (eq $context'|'$token %q) {\n            set context = %q\n", keyword, key, transitions[key]); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w, "        }\n    }"); err != nil {
		return err
	}
	for index, context := range contexts {
		keyword := "if"
		if index > 0 {
			keyword = "} elif"
		}
		if _, err := fmt.Fprintf(w, "    %s (eq $context %q) {\n        put %s\n", keyword, context.path, shellWords(context.candidates)); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w, "    }\n}")
	return err
}
