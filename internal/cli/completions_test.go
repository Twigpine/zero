package cli

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestCompletionsHelpAndRootHelp(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := runWithDeps([]string{"completions", "--help"}, &stdout, &stderr, appDeps{}); code != exitSuccess {
		t.Fatalf("completion help exit code = %d, want %d: %s", code, exitSuccess, stderr.String())
	}
	for _, want := range []string{
		"zero completions <shell>",
		"bash, zsh, fish, powershell, or elvish",
		"source <(zero completions bash)",
		"~/.config/fish/completions/zero.fish",
		"eval (zero completions elvish | slurp)",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("completion help missing %q:\n%s", want, stdout.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	if code := runWithDeps([]string{"--help"}, &stdout, &stderr, appDeps{}); code != exitSuccess {
		t.Fatalf("root help exit code = %d, want %d: %s", code, exitSuccess, stderr.String())
	}
	if !strings.Contains(stdout.String(), "completions Generate shell completion scripts") {
		t.Fatalf("root help does not list completions command:\n%s", stdout.String())
	}
}

func TestCompletionsRejectsMissingUnknownAndExtraShellArguments(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing", args: []string{"completions"}, want: "shell required"},
		{name: "unknown", args: []string{"completions", "nu"}, want: "unsupported shell"},
		{name: "extra", args: []string{"completions", "bash", "extra"}, want: "unexpected completions argument"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			if code := runWithDeps(test.args, &stdout, &stderr, appDeps{}); code != exitUsage {
				t.Fatalf("exit code = %d, want %d: %s", code, exitUsage, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			if !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("stderr = %q, want substring %q", stderr.String(), test.want)
			}
		})
	}
}

func TestCompletionsGeneratesEverySupportedShell(t *testing.T) {
	tests := []struct {
		shell       string
		marker      string
		syntaxShell string
	}{
		{shell: "bash", marker: "complete -F _zero zero", syntaxShell: "bash"},
		{shell: "zsh", marker: "#compdef zero", syntaxShell: "zsh"},
		{shell: "fish", marker: "complete -c zero"},
		{shell: "powershell", marker: "Register-ArgumentCompleter -Native -CommandName zero"},
		{shell: "elvish", marker: "edit:completion:arg-completer[zero]"},
	}
	for _, test := range tests {
		t.Run(test.shell, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			if code := runWithDeps([]string{"completions", test.shell}, &stdout, &stderr, appDeps{}); code != exitSuccess {
				t.Fatalf("exit code = %d, want %d: %s", code, exitSuccess, stderr.String())
			}
			if !strings.Contains(stdout.String(), test.marker) {
				t.Fatalf("%s completion missing marker %q:\n%s", test.shell, test.marker, stdout.String())
			}
			for _, want := range []string{"daemon", "mcp oauth", "sandbox grants", "--output-format"} {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("%s completion missing %q", test.shell, want)
				}
			}
			script := stdout.String()
			switch test.shell {
			case "fish":
				assertBalancedFishBlocks(t, script)
			case "powershell", "elvish":
				assertBalancedBraces(t, script)
			}
			if test.syntaxShell != "" {
				assertNativeShellSyntax(t, test.syntaxShell, script)
			}
		})
	}
}

func assertBalancedBraces(t *testing.T, script string) {
	t.Helper()
	if opens, closes := strings.Count(script, "{"), strings.Count(script, "}"); opens != closes {
		t.Errorf("unbalanced braces: %d opening, %d closing", opens, closes)
	}
}

func assertBalancedFishBlocks(t *testing.T, script string) {
	t.Helper()
	depth := 0
	for _, line := range strings.Split(script, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "function", "for", "switch", "if":
			depth++
		case "end":
			depth--
			if depth < 0 {
				t.Fatal("fish completion closes a block before one is opened")
			}
		}
	}
	if depth != 0 {
		t.Errorf("fish completion has %d unclosed blocks", depth)
	}
}

func assertNativeShellSyntax(t *testing.T, shell, script string) {
	t.Helper()
	path, err := exec.LookPath(shell)
	if err != nil {
		t.Skipf("%s is not installed", shell)
	}
	command := exec.Command(path, "-n")
	command.Stdin = strings.NewReader(script)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("%s syntax check failed: %v\n%s", shell, err, output)
	}
}

func TestCompletionTreeCoversAliasesNestingAndCommonFlags(t *testing.T) {
	contexts := completionContexts(completionRoot)
	byPath := make(map[string][]string, len(contexts))
	for _, context := range contexts {
		byPath[context.path] = context.candidates
	}

	assertCandidates(t, byPath[""], "sessions", "session", "plugins", "plugin", "worktrees", "worktree", "--add-dir", "--theme", "-p", "--prompt")
	assertCandidates(t, byPath["exec"], "--model", "--cwd", "--worktree", "--output-format", "--resume", "--skip-permissions-unsafe")
	assertCandidates(t, byPath["worktrees"], "prepare", "release")
	assertCandidates(t, byPath["worktree"], "prepare", "release")
	assertCandidates(t, byPath["daemon"], "start", "stop", "status", "run", "attach")
	assertCandidates(t, byPath["mcp oauth"], "login", "logout", "status")
	assertCandidates(t, byPath["auth"], "openrouter", "chatgpt", "login", "logout", "status", "refresh", "reset")
	assertCandidates(t, byPath["auth reset"], "--confirm", "--json", "--help")
	assertCandidates(t, byPath["sandbox grants"], "list", "allow", "deny", "revoke", "clear")
	assertCandidates(t, byPath["completions"], "bash", "zsh", "fish", "powershell", "elvish")
	assertCandidates(t, byPath["plugins"], "list", "add", "info", "remove", "rm")
	assertCandidates(t, byPath["plugin"], "list", "add", "info", "remove", "rm")
	// `config` gained the notify subcommand; the completion tree must expose
	// it (and its flags) so shell completion cannot go stale for new CLI
	// surfaces (maintainer review, PR #1001).
	assertCandidates(t, byPath["config"], "notify", "--json", "--help")
	assertCandidates(t, byPath["config notify"], "--mode", "--focus", "--reset", "--json", "--help")
}

func assertCandidates(t *testing.T, got []string, wants ...string) {
	t.Helper()
	set := make(map[string]bool, len(got))
	for _, candidate := range got {
		set[candidate] = true
	}
	for _, want := range wants {
		if !set[want] {
			t.Errorf("candidates %v do not contain %q", got, want)
		}
	}
}

// Dispatch, help, and completions each carried their own list of provider
// subcommands, and they drifted: `repair-config` was dispatched and documented
// while no generated script could complete it — so the recovery command the new
// validation errors point users at was undiscoverable by tab.
//
// completionRoot now builds the providers node from providersSubcommands. This
// holds the other two surfaces to the same inventory, so the next provider
// command cannot ship through one door only.
func TestProvidersSubcommandInventoryMatchesDispatchAndHelp(t *testing.T) {
	var help bytes.Buffer
	if err := writeProvidersHelp(&help); err != nil {
		t.Fatalf("writeProvidersHelp: %v", err)
	}
	helpText := help.String()

	dispatch, err := os.ReadFile("command_center.go")
	if err != nil {
		t.Fatalf("read dispatch source: %v", err)
	}
	dispatchSource := runProvidersDispatchSource(t, string(dispatch))

	contexts := completionContexts(completionRoot)
	var completionCandidates []string
	for _, context := range contexts {
		if context.path == "providers" {
			completionCandidates = context.candidates
		}
	}
	if completionCandidates == nil {
		t.Fatal("completion contexts have no providers path")
	}
	completed := make(map[string]bool, len(completionCandidates))
	for _, candidate := range completionCandidates {
		completed[candidate] = true
	}

	for _, names := range providersSubcommands {
		canonical := names[0]
		// Help documents the canonical spelling; aliases are not separate lines.
		if !strings.Contains(helpText, "zero providers "+canonical) {
			t.Errorf("providers help does not document %q", canonical)
		}
		for _, name := range names {
			if !completed[name] {
				t.Errorf("providers completion context does not offer %q (candidates: %v)", name, completionCandidates)
			}
			// `list`, `current`, and `catalog` fall through to the shared
			// options parser rather than an `if command ==` branch, so they are
			// matched by the final guard instead.
			if !strings.Contains(dispatchSource, `"`+name+`"`) {
				t.Errorf("runProviders does not dispatch %q", name)
			}
		}
	}

	// The reverse direction: a command the completion tree offers but nothing
	// dispatches would be just as broken.
	known := make(map[string]bool)
	for _, names := range providersSubcommands {
		for _, name := range names {
			known[name] = true
		}
	}
	for _, candidate := range completionCandidates {
		if strings.HasPrefix(candidate, "-") {
			continue
		}
		if !known[candidate] {
			t.Errorf("providers completion offers %q, which is not in providersSubcommands", candidate)
		}
	}
}

// runProvidersDispatchSource returns just the body of runProviders, so a name
// mentioned elsewhere in the file cannot satisfy the dispatch assertion.
func runProvidersDispatchSource(t *testing.T, source string) string {
	t.Helper()
	start := strings.Index(source, "func runProviders(args []string")
	if start < 0 {
		t.Fatal("runProviders not found in command_center.go")
	}
	rest := source[start:]
	end := strings.Index(rest, "\nfunc ")
	if end < 0 {
		return rest
	}
	return rest[:end]
}
