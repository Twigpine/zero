package sandbox

import (
	"strings"
	"testing"
)

func TestDetectInteractiveCommandBlocksEditors(t *testing.T) {
	cases := []struct {
		name         string
		command      string
		wantCmd      string
		wantSuggHint string
	}{
		{name: "vim", command: "vim main.go", wantCmd: "vim", wantSuggHint: "non-interactive"},
		{name: "nano", command: "nano notes.txt", wantCmd: "nano"},
		{name: "less pager", command: "less /var/log/syslog", wantCmd: "less", wantSuggHint: "cat"},
		{name: "python repl", command: "python", wantCmd: "python", wantSuggHint: "-c"},
		{name: "node repl", command: "node", wantCmd: "node", wantSuggHint: "-e"},
		// -v is "verbose" for mysql (NOT "version"), so it must still open the
		// prompt — the info-exit allowance is for unambiguous --version/--help only.
		{name: "mysql verbose not version", command: "mysql -v", wantCmd: "mysql", wantSuggHint: "-e"},
		{name: "ssh interactive", command: "ssh host.example.com", wantCmd: "ssh"},
		{name: "top", command: "top", wantCmd: "top"},
		{name: "git rebase interactive", command: "git rebase -i HEAD~3", wantCmd: "git rebase -i"},
		{name: "tail follow", command: "tail -f app.log", wantCmd: "tail -f"},
		{name: "env prefix vim", command: "EDITOR=vim FOO=bar vim file", wantCmd: "vim"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := DetectInteractiveCommand(tc.command, "linux")
			if !result.Interactive {
				t.Fatalf("DetectInteractiveCommand(%q) = not interactive, want interactive", tc.command)
			}
			if result.Command != tc.wantCmd {
				t.Fatalf("matched command = %q, want %q", result.Command, tc.wantCmd)
			}
			if result.Suggestion == "" {
				t.Fatalf("expected an actionable suggestion for %q", tc.command)
			}
			if tc.wantSuggHint != "" && !strings.Contains(strings.ToLower(result.Suggestion), strings.ToLower(tc.wantSuggHint)) {
				t.Fatalf("suggestion %q does not mention %q", result.Suggestion, tc.wantSuggHint)
			}
		})
	}
}

func TestDetectInteractiveCommandAllowsNonInteractive(t *testing.T) {
	cases := []string{
		"",
		"ls -la",
		"go test ./...",
		"python -c 'print(1)'",
		"python3 script.py",
		"node -e 'console.log(1)'",
		"node build.js",
		"node --version",
		"node -v",
		"node --check app.js",
		"node --help",
		"node --version && npm --version",
		"python3 --version",
		"php --version",
		"cat file.txt",
		"git rebase --continue",
		"git status",
		"tail -n 50 app.log",
		"ssh host 'uptime'",
		"grep -r foo .",
		// After the `--` separator the following tokens are positional operands,
		// not shell flags, so `-ec` must not be read as the `-c` command flag.
		"bash -- -ec 'vim file.txt'",
		"sh -- -c 'less /var/log/syslog'",
	}
	for _, command := range cases {
		t.Run(command, func(t *testing.T) {
			result := DetectInteractiveCommand(command, "linux")
			if result.Interactive {
				t.Fatalf("DetectInteractiveCommand(%q) = interactive (%q), want allowed", command, result.Command)
			}
		})
	}
}

func TestDetectInteractiveCommandHonorsWindows(t *testing.T) {
	// edit and notepad are Windows-only interactive launchers.
	if result := DetectInteractiveCommand("notepad config.ini", "windows"); !result.Interactive {
		t.Fatalf("expected notepad to be interactive on windows")
	}
	if result := DetectInteractiveCommand("notepad config.ini", "linux"); result.Interactive {
		t.Fatalf("notepad should not be treated as interactive on linux")
	}
}

func TestDetectInteractiveCommandSuggestsWindowsAlternativesOnWindows(t *testing.T) {
	for _, tc := range []struct {
		command string
		avoid   []string
		want    string
	}{
		{command: "more file.txt", avoid: []string{"cat", "head", "tail"}, want: "type"},
		{command: "less file.txt", avoid: []string{"cat", "head", "tail"}, want: "type"},
		{command: "most file.txt", avoid: []string{"cat", "head", "tail"}, want: "type"},
		{command: "top", avoid: []string{"ps aux"}, want: "tasklist"},
		{command: "htop", avoid: []string{"ps aux"}, want: "tasklist"},
		{command: "btop", avoid: []string{"ps aux"}, want: "tasklist"},
		{command: "btm", avoid: []string{"ps aux"}, want: "tasklist"},
		{command: "tail -f app.log", avoid: []string{"tail -n"}, want: "read_file"},
	} {
		result := DetectInteractiveCommand(tc.command, "windows")
		if !result.Interactive {
			t.Fatalf("DetectInteractiveCommand(%q, windows) = not interactive, want interactive", tc.command)
		}
		for _, bad := range tc.avoid {
			if strings.Contains(result.Suggestion, bad) {
				t.Fatalf("DetectInteractiveCommand(%q, windows).Suggestion = %q, want it to avoid POSIX-only %q", tc.command, result.Suggestion, bad)
			}
		}
		if !strings.Contains(result.Suggestion, tc.want) {
			t.Fatalf("DetectInteractiveCommand(%q, windows).Suggestion = %q, want it to mention %q", tc.command, result.Suggestion, tc.want)
		}
	}

	// The same commands on Linux should keep the original POSIX suggestion.
	result := DetectInteractiveCommand("more file.txt", "linux")
	if !strings.Contains(result.Suggestion, "cat") {
		t.Fatalf("DetectInteractiveCommand(more, linux).Suggestion = %q, want the POSIX suggestion unchanged", result.Suggestion)
	}
}

// TestDetectInteractiveCommandPagerSuggestionIsPlatformSpecific covers a fix
// for suggesting POSIX-only tools (cat/head/tail) as the escape hatch for a
// blocked pager on Windows, where cmd.exe has none of them.
func TestDetectInteractiveCommandPagerSuggestionIsPlatformSpecific(t *testing.T) {
	for _, pager := range []string{"less", "more", "most"} {
		linux := DetectInteractiveCommand(pager+" notes.txt", "linux")
		if !linux.Interactive || !strings.Contains(linux.Suggestion, "cat") {
			t.Fatalf("%s on linux: expected cat/head/tail suggestion, got %+v", pager, linux)
		}
		windows := DetectInteractiveCommand(pager+" notes.txt", "windows")
		if !windows.Interactive {
			t.Fatalf("%s on windows: expected interactive block, got %+v", pager, windows)
		}
		if strings.Contains(windows.Suggestion, "cat") || strings.Contains(windows.Suggestion, "tail") {
			t.Fatalf("%s on windows: suggestion should not name POSIX-only tools, got %q", pager, windows.Suggestion)
		}
		if !strings.Contains(windows.Suggestion, "type") {
			t.Fatalf("%s on windows: expected type suggestion, got %q", pager, windows.Suggestion)
		}
	}
}

func TestDetectInteractiveCommandFindsAcrossSeparators(t *testing.T) {
	// Interactive commands hidden after a shell operator should still be caught.
	for _, command := range []string{
		"git pull && vim conflict.txt",
		"echo hi; less log.txt",
		"true | nano",
	} {
		result := DetectInteractiveCommand(command, "linux")
		if !result.Interactive {
			t.Fatalf("DetectInteractiveCommand(%q) = not interactive, want interactive", command)
		}
	}
}

// Finding 3: firstProgram must skip additional wrappers (nice/timeout/stdbuf/
// setsid/ionice/xargs), skip leading option tokens for sudo/env, and recurse
// into `sh -c`/`bash -c <payload>`.
func TestDetectInteractiveThroughWrappersAndShellC(t *testing.T) {
	cases := []struct {
		name    string
		command string
		wantCmd string
	}{
		{name: "nice", command: "nice vim file.txt", wantCmd: "vim"},
		{name: "timeout", command: "timeout 5 vim file.txt", wantCmd: "vim"},
		{name: "stdbuf", command: "stdbuf -oL vim file.txt", wantCmd: "vim"},
		{name: "setsid", command: "setsid vim file.txt", wantCmd: "vim"},
		{name: "ionice", command: "ionice -c3 vim file.txt", wantCmd: "vim"},
		{name: "xargs", command: "xargs vim", wantCmd: "vim"},
		{name: "sudo with option", command: "sudo -u root vim file.txt", wantCmd: "vim"},
		{name: "sudo with long option value", command: "sudo --user root vim file.txt", wantCmd: "vim"},
		{name: "sudo with long option joined value", command: "sudo --user=root vim file.txt", wantCmd: "vim"},
		{name: "env with assignment option", command: "env -i EDITOR=x vim file.txt", wantCmd: "vim"},
		{name: "sh -c payload", command: "sh -c 'vim file.txt'", wantCmd: "vim"},
		{name: "bash -c payload", command: `bash -c "less /var/log/syslog"`, wantCmd: "less"},
		// POSIX getopt clusters the command flag with other short options; the
		// payload must still be located and recursed into (ZERO-ESC-02).
		{name: "sh -ec grouped flags", command: "sh -ec 'vim file.txt'", wantCmd: "vim"},
		{name: "bash -lc grouped flags", command: `bash -lc "less /var/log/syslog"`, wantCmd: "less"},
		{name: "zsh -xc grouped flags", command: "zsh -xc 'nano notes.txt'", wantCmd: "nano"},
		{name: "bash --command long flag", command: `bash --command "less /var/log/syslog"`, wantCmd: "less"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := DetectInteractiveCommand(tc.command, "linux")
			if !result.Interactive {
				t.Fatalf("DetectInteractiveCommand(%q) = not interactive, want interactive", tc.command)
			}
			if result.Command != tc.wantCmd {
				t.Fatalf("matched command = %q, want %q", result.Command, tc.wantCmd)
			}
		})
	}
}

// Audit ZERO-ESC-02: the shared shell-command-flag helper must honor POSIX
// getopt short-option clustering, so a command hidden behind `bash -ec`/
// `sh -lc`/`zsh -xc` (and the legacy `--command`) is still recognized, while
// unrelated options and option values are not mistaken for the flag.
func TestShellCommandFlag(t *testing.T) {
	cases := []struct {
		arg  string
		want bool
	}{
		{arg: "-c", want: true},
		{arg: "-ec", want: true},
		{arg: "-lc", want: true},
		{arg: "-xc", want: true},
		{arg: "-ce", want: true},
		{arg: "--command", want: true},
		{arg: "-xec", want: true},
		{arg: "-e", want: false},
		{arg: "-o", want: false},
		{arg: "-O", want: false},
		// `-o`/`-O` consume the rest of the cluster as their value, so a later
		// `c` is that value (e.g. `-o c`), not the command flag.
		{arg: "-oc", want: false},
		{arg: "-Oc", want: false},
		{arg: "--norc", want: false},
		{arg: "--command=payload", want: false},
		{arg: "-", want: false},
		{arg: "", want: false},
		{arg: "c", want: false},
	}
	for _, tc := range cases {
		if got := shellCommandFlag(tc.arg); got != tc.want {
			t.Errorf("shellCommandFlag(%q) = %v, want %v", tc.arg, got, tc.want)
		}
	}
}

// Audit: the `--` separator ends option processing, so a `-c`/`-ec` token
// appearing after it is a positional operand, not the shell command flag. A
// payload hidden behind `bash -- -ec '...'` must not be recursed into.
func TestShellDashCPayloadStopsAtDashDash(t *testing.T) {
	cases := []struct {
		name   string
		fields []string
		want   string
	}{
		{name: "dashdash before clustered flag", fields: []string{"bash", "--", "-ec", "vim file.txt"}, want: ""},
		{name: "dashdash before bare flag", fields: []string{"sh", "--", "-c", "less file"}, want: ""},
		{name: "real flag before dashdash", fields: []string{"bash", "-c", "vim file.txt", "--"}, want: "vim file.txt --"},
		{name: "no dashdash grouped flag", fields: []string{"bash", "-ec", "vim file.txt"}, want: "vim file.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shellDashCPayload(tc.fields[0], tc.fields); got != tc.want {
				t.Fatalf("shellDashCPayload(%v) = %q, want %q", tc.fields, got, tc.want)
			}
		})
	}
}

// quote/escape characters embedded INSIDE the program token (e.g. `vi\m`,
// `v"i"m`, `'v'im`), not just surrounding it.
func TestDetectInteractiveStripsEmbeddedQuotingFromToken(t *testing.T) {
	cases := []struct {
		name    string
		command string
		wantCmd string
	}{
		{name: "mid-token backslash", command: `vi\m file.txt`, wantCmd: "vim"},
		{name: "embedded double quotes", command: `v"i"m file.txt`, wantCmd: "vim"},
		{name: "leading single quote split", command: `'v'im file.txt`, wantCmd: "vim"},
		{name: "escaped less", command: `le\ss /var/log/syslog`, wantCmd: "less"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := DetectInteractiveCommand(tc.command, "linux")
			if !result.Interactive {
				t.Fatalf("DetectInteractiveCommand(%q) = not interactive, want interactive", tc.command)
			}
			if result.Command != tc.wantCmd {
				t.Fatalf("matched command = %q, want %q", result.Command, tc.wantCmd)
			}
		})
	}
}

// Audit finding (LOW): interactive SEGMENTS (e.g. "git rebase -i", "tail -f")
// must match only on a real command/segment boundary, not anywhere as a raw
// substring of the whole command. Otherwise the text appearing inside a quoted
// argument produces a false positive.
func TestDetectInteractiveSegmentBoundary(t *testing.T) {
	// False positives: the segment text appears only inside an argument/quotes.
	allowed := []string{
		`echo "git rebase -i is interactive"`,
		`grep "tail -f" notes.txt`,
		`echo run docker attach later`,
		`printf 'kubectl logs -f streams'`,
		// A literal ')' that does not close a $(...) must not split the command
		// into a fake `less` segment (regression for unconditional ')' splitting).
		`echo "a) less"`,
		`echo "(done) vim later"`,
	}
	for _, cmd := range allowed {
		if got := DetectInteractiveCommand(cmd, "linux"); got.Interactive {
			t.Errorf("expected %q NOT to be flagged interactive (got %q)", cmd, got.Command)
		}
	}
	// True positives must still be caught at a real boundary.
	blocked := []struct {
		command string
		wantCmd string
	}{
		{`git rebase -i HEAD~3`, "git rebase -i"},
		{`tail -f app.log`, "tail -f"},
		{`git pull && git rebase -i HEAD~2`, "git rebase -i"},
		{`docker logs -f mycontainer`, "docker logs -f"},
	}
	for _, tc := range blocked {
		got := DetectInteractiveCommand(tc.command, "linux")
		if !got.Interactive || got.Command != tc.wantCmd {
			t.Errorf("DetectInteractiveCommand(%q) = (%v,%q), want interactive %q", tc.command, got.Interactive, got.Command, tc.wantCmd)
		}
	}
}

func TestSplitShellSegmentsParenOnlySplitsInsideSubstitution(t *testing.T) {
	// A literal ')' that does not close a $(...) must not be a segment boundary,
	// otherwise text like "a) less" splits into a fake `less` segment.
	if segs := splitShellSegments(`echo "a) less"`); len(segs) != 1 {
		t.Fatalf(`splitShellSegments(echo "a) less") = %#v, want a single segment`, segs)
	}
	// A real $(...) still isolates the substituted command so it can be analyzed.
	segs := splitShellSegments(`echo $(less foo)`)
	found := false
	for _, s := range segs {
		if s == "less foo" {
			found = true
		}
	}
	if !found {
		t.Fatalf(`splitShellSegments(echo $(less foo)) = %#v, want a "less foo" segment`, segs)
	}
	// Nested substitutions track depth correctly: the inner ')' closes $(b ...),
	// the outer ')' closes $(a ...), and neither leaks an empty/garbage segment.
	nested := splitShellSegments(`a $(b $(c) d) e`)
	for _, s := range nested {
		if s == "" {
			t.Fatalf("nested substitution produced an empty segment: %#v", nested)
		}
	}

	// A ')' inside a double-quoted argument WITHIN a substitution is literal and
	// must not close the $(...) early — otherwise a fake `less` segment appears.
	for _, seg := range splitShellSegments(`echo $(printf "a) less")`) {
		if seg == "less\"" || seg == "less" || strings.HasPrefix(seg, "less") {
			t.Fatalf(`a quoted ')' closed the substitution early, producing %q in %#v`, seg, splitShellSegments(`echo $(printf "a) less")`))
		}
	}

	// A real substitution still spanning double quotes (`"$(vim)"`) isolates the
	// inner command so an interactive program inside it is still caught.
	foundVim := false
	for _, seg := range splitShellSegments(`echo "$(vim x)"`) {
		if seg == "vim x" {
			foundVim = true
		}
	}
	if !foundVim {
		t.Fatalf(`splitShellSegments(echo "$(vim x)") must isolate "vim x", got %#v`, splitShellSegments(`echo "$(vim x)"`))
	}
}

func TestSplitShellSegmentsIsEscapeAware(t *testing.T) {
	// An escaped operator outside quotes is literal and must not split.
	if segs := splitShellSegments(`echo foo\|less`); len(segs) != 1 {
		t.Fatalf(`splitShellSegments(echo foo\|less) = %#v, want a single segment`, segs)
	}
	// An escaped quote inside double quotes must not toggle quoting, so the '|'
	// stays quoted and does not manufacture a fake `less` segment.
	if segs := splitShellSegments(`printf "use \"| less"`); len(segs) != 1 {
		t.Fatalf(`splitShellSegments(printf "use \"| less") = %#v, want a single segment`, segs)
	}
	// A real, unescaped operator still splits.
	if segs := splitShellSegments(`a | less`); len(segs) != 2 {
		t.Fatalf(`splitShellSegments(a | less) = %#v, want two segments`, segs)
	}
}

func TestDetectInteractiveBypasses(t *testing.T) {
	blocked := []string{
		"/usr/bin/vim file.txt",     // absolute path
		"\"vim\" file.txt",          // double-quoted program
		"'vim' file.txt",            // single-quoted program
		"echo $(vim file.txt)",      // command substitution
		"echo `vim file.txt`",       // backtick substitution
		"echo \"`true | less`\"",    // backtick in double quotes hides an inner pager
		"echo \"$(true | less)\"",   // $() in double quotes hides an inner pager
		"/bin/less /var/log/syslog", // absolute pager
	}
	for _, cmd := range blocked {
		if got := DetectInteractiveCommand(cmd, "linux"); !got.Interactive {
			t.Errorf("expected %q to be detected as interactive", cmd)
		}
	}
	// must NOT over-block legitimate non-interactive commands
	allowed := []string{
		"python script.py",   // script, not REPL
		"cat vim.txt",        // file named vim, not the editor
		"grep ssh config.go", // 'ssh' as a search term
		"echo hello",
	}
	for _, cmd := range allowed {
		if got := DetectInteractiveCommand(cmd, "linux"); got.Interactive {
			t.Errorf("expected %q NOT to be flagged interactive (got %q)", cmd, got.Command)
		}
	}
}

// Audit finding (MED): splitShellSegments must be quote-aware. A shell operator
// inside quotes is a literal argument, not a separator, so it must not split the
// command and falsely flag a quoted program name — while real, unquoted operators
// must still split (no new false negatives).
func TestDetectInteractiveQuoteAwareSeparators(t *testing.T) {
	allowed := []string{
		`git commit -m "use top | less"`, // | inside double quotes
		`echo "a; vim b"`,                // ; inside double quotes
		`echo 'pipe it: a | less'`,       // | inside single quotes
		`git commit -m "vim && nano"`,    // && inside double quotes
	}
	for _, cmd := range allowed {
		if got := DetectInteractiveCommand(cmd, "linux"); got.Interactive {
			t.Errorf("expected %q NOT to be flagged (operator is quoted), got %q", cmd, got.Command)
		}
	}

	blocked := []struct {
		command string
		wantCmd string
	}{
		{`echo hi | less`, "less"},            // real unquoted pipe
		{`echo "safe" | vim`, "vim"},          // real pipe after a quoted arg
		{`git commit -m "msg"; vim x`, "vim"}, // real ; after a quoted arg
		{`echo "$(vim x)"`, "vim"},            // substitution still active in double quotes
	}
	for _, tc := range blocked {
		got := DetectInteractiveCommand(tc.command, "linux")
		if !got.Interactive || got.Command != tc.wantCmd {
			t.Errorf("DetectInteractiveCommand(%q) = (%v,%q), want interactive %q", tc.command, got.Interactive, got.Command, tc.wantCmd)
		}
	}
}

func TestDetectInteractiveMongoEvalAndFullPaths(t *testing.T) {
	cases := []struct {
		command     string
		interactive bool
	}{
		{"mongo --eval 'db.test.find()'", false},
		{"mongosh --eval 'db.test.find()'", false},
		{"mongo", true},
		{"/usr/bin/python script.py", false}, // full-path program + script arg -> not a REPL
		{"/bin/bash -c 'vim file'", true},    // full-path shell -c with nested interactive program
	}
	for _, tc := range cases {
		got := DetectInteractiveCommand(tc.command, "linux").Interactive
		if got != tc.interactive {
			t.Errorf("DetectInteractiveCommand(%q).Interactive = %v, want %v", tc.command, got, tc.interactive)
		}
	}
}

// The hand-written segment splitter misses interactive programs hidden by
// constructs only a real shell parser resolves (a newline separator collapsed
// to a space; a brace group that shifts the real command position). The AST
// second opinion must catch them (issue #473).
func TestDetectInteractiveCommandCatchesParserBypasses(t *testing.T) {
	for _, cmd := range []string{
		"echo hi\nvim file.txt", // newline separator collapsed to a space by the regex path
		"{ vim file.txt; }",     // brace group hides the real program position
	} {
		got := DetectInteractiveCommand(cmd, "linux")
		if !got.Interactive {
			t.Errorf("DetectInteractiveCommand(%q) = not interactive, want caught (parser bypass)", cmd)
			continue
		}
		if got.Command != "vim" {
			t.Errorf("DetectInteractiveCommand(%q).Command = %q, want vim", cmd, got.Command)
		}
	}
}

// The AST second opinion must not flag an interactive program NAME that appears
// only inside a quoted argument (not a real command position) — the parser
// distinguishes program from argument, so this must stay non-interactive.
func TestDetectInteractiveCommandNoFalsePositiveOnQuotedArgument(t *testing.T) {
	got := DetectInteractiveCommand(`echo "please run vim later"`, "linux")
	if got.Interactive {
		t.Fatalf("interactive name inside a quoted argument must not be flagged, got %#v", got)
	}
}

// The AST second opinion must run the FULL pipeline — multi-word interactive
// segments and sh -c payload recursion, not just the per-program lookup — so a
// grouped/nested bypass is caught; and it must not fabricate a program name from
// a command substitution concatenated with a literal (issue #473, review).
func TestDetectInteractiveCommandASTPipelineBoundaries(t *testing.T) {
	interactive := []struct{ cmd, wantCmd string }{
		{"{ git rebase -i HEAD~1; }", "git rebase -i"}, // multi-word segment in a brace group
		{"{ sh -c 'vim file.txt'; }", "vim"},           // sh -c payload in a brace group
	}
	for _, tc := range interactive {
		got := DetectInteractiveCommand(tc.cmd, "linux")
		if !got.Interactive || got.Command != tc.wantCmd {
			t.Errorf("DetectInteractiveCommand(%q) = %+v, want interactive Command=%q", tc.cmd, got, tc.wantCmd)
		}
	}
}

// The AST pass must not fabricate a program name from a dynamic (non-literal)
// program word: `$(printf '%s' foo)vim` runs as `foovim`, not vim. Tested at the
// extractor so the hand-written pass (which pre-empts DetectInteractiveCommand
// with its own, separate handling of that construct) does not mask the guard.
func TestAstCommandFieldsSkipsDynamicProgram(t *testing.T) {
	for _, fields := range astCommandFields("$(printf '%s' foo)vim file.txt") {
		if firstProgram(fields) == "vim" {
			t.Fatalf("astCommandFields fabricated a vim command from a substitution: %v", fields)
		}
	}
}

// The literalness guard covers every word, not just the program name. A dynamic
// ARGUMENT is dropped by wordText the same way a dynamic program word is, so
// `git $(printf foo)rebase -i HEAD~1` (which runs as `git foorebase -i`, a
// non-existent and non-interactive subcommand) would otherwise be reconstructed
// as `git rebase -i` and blocked — a false positive on a command the user never
// wrote. Asserted through DetectInteractiveCommand as well as the extractor,
// because the whole point is what the caller ends up blocking.
func TestAstCommandFieldsSkipsDynamicArgument(t *testing.T) {
	const dynamic = "{ git $(printf foo)rebase -i HEAD~1 ; }"
	for _, fields := range astCommandFields(dynamic) {
		if firstProgram(fields) == "git" {
			t.Fatalf("astCommandFields reconstructed a git command from a dynamic argument: %v", fields)
		}
	}
	if got := DetectInteractiveCommand(dynamic, "linux"); got.Interactive {
		t.Errorf("DetectInteractiveCommand(%q) = %+v, want not interactive", dynamic, got)
	}
	// The genuine form must still be detected — the guard skips lossy
	// reconstructions, it does not weaken real detection.
	const genuine = "{ git rebase -i HEAD~1 ; }"
	if got := DetectInteractiveCommand(genuine, "linux"); !got.Interactive || got.Command != "git rebase -i" {
		t.Errorf("DetectInteractiveCommand(%q) = %+v, want interactive Command=%q", genuine, got, "git rebase -i")
	}
}
