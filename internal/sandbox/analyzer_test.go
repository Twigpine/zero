package sandbox

import (
	"strings"
	"testing"
)

func TestAnalyzeCommand(t *testing.T) {
	t.Setenv("ZERO_TEST_ENV_COMMAND", "curl")
	cases := []struct {
		name        string
		script      string
		interactive bool
		destructive bool
		network     bool
		tooComplex  bool
	}{
		{name: "editor", script: "vim foo.txt", interactive: true},
		{name: "pager in pipe", script: "cat log | less", interactive: true},
		{name: "interactive name only inside quotes", script: `echo "vim is a great editor"`, interactive: false},
		{name: "printf quoted arg", script: `printf 'open with vim\n'`, interactive: false},
		{name: "repl suppressed by -e", script: `node -e "require('repl').start()"`, interactive: false},
		{name: "repl suppressed by script", script: "python3 app.py", interactive: false},
		{name: "bare repl", script: "python3", interactive: true},

		{name: "rm recursive force", script: "rm -rf /tmp/x", destructive: true},
		{name: "rm bundled flags reversed", script: "rm -fr ./build", destructive: true},
		{name: "rm without force", script: "rm file.txt", destructive: false},
		{name: "rm inside quoted arg", script: `git commit -m "rm -rf /"`, destructive: false},
		{name: "rm end-of-options literal filename", script: "rm -- -rf", destructive: false},
		{name: "dd", script: "dd if=/dev/zero of=/dev/disk2", destructive: true},
		{name: "find delete", script: "find . -type f -delete", destructive: true},

		// Wrappers are unwrapped to the real payload, not classified on the launcher.
		{name: "sudo wraps rm -rf", script: "sudo rm -rf /tmp/x", destructive: true},
		{name: "env wraps curl", script: "env curl https://x.test", network: true},
		{name: "bash -c wraps editor", script: `bash -c 'vim file'`, interactive: true},
		// POSIX getopt clusters the command flag with other short options; the
		// payload must still be parsed and classified (ZERO-ESC-02).
		{name: "bash -ec wraps destructive payload", script: `bash -ec 'fdisk /dev/sda'`, destructive: true},
		{name: "sh -lc wraps destructive payload", script: `sh -lc 'shred -u secret.txt'`, destructive: true},
		{name: "zsh -xc wraps destructive payload", script: `zsh -xc 'parted /dev/sda mklabel gpt'`, destructive: true},
		{name: "bash -ec wraps network payload", script: `bash -ec 'curl https://x.test'`, network: true},
		{name: "bash --command wraps editor", script: `bash --command 'vim file'`, interactive: true},
		{name: "bash --command wraps destructive payload", script: `bash --command 'fdisk /dev/sda'`, destructive: true},
		// `--` ends option processing, so a `-ec`/`-c` after it is a positional
		// operand, not the shell command flag: the quoted text must NOT be parsed
		// as a payload command and must not trigger its classification.
		{name: "bash -- -ec editor is not a payload", script: `bash -- -ec 'vim file'`, interactive: false},
		{name: "bash -- -ec fdisk is not a payload", script: `bash -- -ec 'fdisk /dev/sda'`, destructive: false},
		{name: "bash -- -c curl is not a payload", script: `bash -- -c 'curl https://x.test'`, network: false},
		{name: "sudo wraps bare repl", script: "sudo python3", interactive: true},
		// A valueless wrapper flag must not swallow the real payload command.
		{name: "sudo -n keeps rm payload", script: "sudo -n rm -rf /tmp/x", destructive: true},
		{name: "sudo -n keeps curl payload", script: "sudo -n curl https://x.test", network: true},
		{name: "sudo -u consumes its value", script: "sudo -u root vim file", interactive: true},
		// Long wrapper flags consume a separate value too (space and = forms).
		{name: "sudo --user space value", script: "sudo --user root vim file", interactive: true},
		{name: "sudo --user= joined value", script: "sudo --user=root vim file", interactive: true},
		{name: "env --unset then curl", script: "env --unset FOO curl https://x.test", network: true},
		// A dynamic ($x) wrapper arg must not hide the literal payload that follows.
		{name: "env dynamic flag then curl", script: `env "$opts" curl https://x.test`, network: true},
		{name: "sudo dynamic flag then rm -rf", script: `sudo "$maybe" rm -rf /tmp/x`, destructive: true},

		{name: "curl", script: "curl https://example.com", network: true},
		{name: "PowerShell iwr alias", script: "iwr https://example.com", network: true},
		{name: "PowerShell irm alias", script: "irm https://example.com", network: true},
		{name: "PowerShell Invoke-WebRequest", script: "Invoke-WebRequest https://example.com", network: true},
		{name: "PowerShell Invoke-RestMethod", script: "Invoke-RestMethod https://example.com", network: true},
		{name: "PowerShell Remove-Item recursive force", script: `Remove-Item -Recurse -Force 'C:\temp\x'`, destructive: true},
		{name: "PowerShell rm recursive force", script: `rm -Recurse -Force 'C:\temp\x'`, destructive: true},
		{name: "PowerShell ri recursive force", script: `ri -Recurse -Force 'C:\temp\x'`, destructive: true},
		{name: "PowerShell rd recursive force", script: `rd -Recurse -Force 'C:\temp\x'`, destructive: true},
		{name: "PowerShell rmdir recursive force", script: `rmdir -Recurse -Force 'C:\temp\x'`, destructive: true},
		{name: "PowerShell del recursive force", script: `del -Recurse -Force 'C:\temp\x'`, destructive: true},
		{name: "PowerShell erase recursive force", script: `erase -Recurse -Force 'C:\temp\x'`, destructive: true},
		{name: "PowerShell ambiguous force abbreviation", script: `Remove-Item -Recurse -f 'C:\temp\x'`, destructive: false},
		{name: "PowerShell Remove-Item without recurse", script: `Remove-Item -Force 'C:\temp\x'`, destructive: false},
		{name: "Windows curl cmd", script: "curl.cmd https://example.com", network: true},
		{name: "Windows curl exe", script: "curl.exe https://example.com", network: true},
		{name: "Windows drive path curl exe", script: `'C:\tools\curl.exe' https://example.com`, network: true},
		{name: "Windows UNC path curl exe", script: `'\\server\share\curl.exe' https://example.com`, network: true},
		{name: "Windows dot relative path curl exe", script: `'.\curl.exe' https://example.com`, network: true},
		{name: "Windows relative path curl exe", script: `'tools\curl.exe' https://example.com`, network: true},
		{name: "Windows drive relative path curl exe", script: `'C:curl.exe' https://example.com`, network: true},
		{name: "Windows npm cmd", script: "npm.cmd install", network: true},
		{name: "wget piped to shell", script: "wget -qO- https://x.test | sh", network: true},
		{name: "python http server", script: "python3 -m http.server 8000", network: true},
		{name: "python pip install", script: "python3 -m pip install requests", network: true},
		{name: "npm install", script: "npm install", network: true},
		{name: "npm ci", script: "npm ci", network: true},
		{name: "npm create", script: "npm create vite@latest .", network: true},
		{name: "npm registry query", script: "npm view typescript version --fetch-retries=0", network: true},
		{name: "npm metadata search", script: "npm search typescript", network: true},
		{name: "npm offline install", script: "npm install --offline", network: false},
		{name: "npm version is offline", script: "npm --version", network: false},
		{name: "npm start", script: "npm start", network: true},
		{name: "npm run dev", script: "npm run dev", network: true},
		{name: "npx http server", script: "npx http-server public -p 8080 -a 127.0.0.1", network: true},
		{name: "direct http server", script: "http-server public -p 8080 -a 127.0.0.1", network: true},
		{name: "direct vite", script: "vite --host 127.0.0.1", network: true},
		{name: "next dev", script: "next dev", network: true},
		{name: "git clone", script: "git clone https://example.com/repo.git", network: true},
		{name: "git fetch", script: "git fetch origin", network: true},
		{name: "git status is offline", script: "git status", network: false},
		{name: "git pull", script: "git pull origin main", network: true},
		{name: "git push custom transport", script: "git push gitlawb://example.com/repo.git main", network: true},
		{name: "git ls-remote", script: "git ls-remote origin", network: true},
		{name: "git remote archive", script: "git archive --remote=origin HEAD", network: true},
		{name: "git remote archive separated value", script: "git archive --remote origin HEAD", network: true},
		// Only --remote leaves the machine; a local archive streams the object
		// store and must not cost a network prompt (issue #703 review).
		{name: "git local archive", script: "git archive HEAD", network: false},
		{name: "git -C local archive", script: "git -C repo archive HEAD -o out.tar", network: false},
		// After `--` every token is a pathspec, so this archives a tree entry
		// named "--remote" from the local object store (issue #703 review).
		{name: "git archive pathspec named --remote", script: "git archive HEAD -- --remote", network: false},
		{name: "git -C archive pathspec named --remote", script: "git -C repo archive HEAD -- --remote=origin", network: false},
		{name: "git archive missing remote operand", script: "git archive HEAD --remote", network: false},
		{name: "git archive post-tree --remote value", script: "git archive HEAD --remote=origin", network: true},
		// A real --remote before the separator still is one.
		{name: "git remote archive with pathspec", script: "git archive --remote=origin HEAD -- src", network: true},
		{name: "git remote archive after output option", script: "git archive -o out.tar --remote origin HEAD", network: true},
		{name: "git remote archive after mtime option", script: "git archive --mtime 2024-01-01 --remote origin HEAD", network: true},
		{name: "git remote archive after format without value", script: "git archive --format --remote=origin HEAD", network: true},
		{name: "git remote archive after mtime without value", script: "git archive --mtime --remote=origin HEAD", network: true},
		{name: "git output consumes --remote", script: "git archive -o --remote HEAD", network: false},
		{name: "git joined output named --remote", script: "git archive --output=--remote HEAD", network: false},
		{name: "git global -C consumes --remote", script: "git -C --remote archive HEAD", network: false},
		{name: "git output consumes separator before remote", script: "git archive -o -- --remote=origin HEAD", network: true},
		{name: "bash clustered login command", script: `bash -lc 'curl https://x.test'`, network: true},
		{name: "bash clustered command erexit", script: `bash -ce 'git push origin main'`, network: true},
		{name: "dash clustered command erexit", script: `dash -ce 'curl https://x.test'`, network: true},
		{name: "bash long command flag", script: `bash --command 'curl https://x.test'`, network: true},
		{name: "sh long command flag before separator", script: `sh --command -- 'curl https://x.test'`, network: true},
		{name: "bash long command flag noexec", script: `bash -n --command 'curl https://x.test'`, network: false},
		{name: "bash joined long command flag", script: `bash --command='curl https://x.test'`, network: false},
		{name: "bash args after command payload are positional", script: `bash -c 'echo ok' curl https://x.test`, network: false},
		{name: "bash script makes later command flag positional", script: `bash /dev/null -c 'curl https://x.test'`, network: false},
		{name: "bash option terminator makes command flag positional", script: `bash -- -c 'curl https://x.test'`, network: false},
		{name: "bash invalid command cluster", script: `bash -Zc 'curl https://x.test'`, network: false},
		{name: "bash noexec command cluster", script: `bash -nc 'curl https://x.test'`, network: false},
		{name: "bash plus option before command", script: `bash +n -c 'curl https://x.test'`, network: true},
		{name: "bash plus command flag", script: `bash +c 'curl https://x.test'`, network: true},
		{name: "bash dump strings does not execute", script: `bash --dump-strings -c 'curl https://x.test'`, network: false},
		{name: "shell dynamic command payload fails closed", script: `sh -c "$ZERO_TEST_SHELL_COMMAND"`, network: true},
		{name: "nested shell dynamic command payload fails closed", script: `sh -c 'sh -c "$1"' sh 'curl https://x.test'`, network: true},
		{name: "shell command terminator before dynamic payload fails closed", script: `PAYLOAD='curl https://x.test'; sh -c -- "$PAYLOAD"`, network: true},
		{name: "env split shell terminator selects payload", script: `env -S 'sh -c' -- 'curl https://x.test'`, network: true},
		{name: "PowerShell slash command", script: `powershell /Command Invoke-WebRequest https://x.test`, network: true},
		{name: "PowerShell abbreviated command", script: `pwsh -co iwr https://x.test`, network: true},
		{name: "PowerShell local command", script: `pwsh /Command Get-ChildItem`, network: false},
		{name: "PowerShell expression evaluation", script: `powershell -Command "Invoke-Expression 'curl https://x.test'"`, network: true},
		{name: "pwsh expression evaluation alias", script: `pwsh -Command "iex 'git push origin main'"`, network: true},
		{name: "PowerShell dynamic expression evaluation", script: `powershell -Command 'iex $PAYLOAD'`, network: true},
		{name: "PowerShell local expression evaluation", script: `powershell -Command "iex 'Write-Output hello'"`, network: false},
		{name: "PowerShell backtick command name fails closed", script: "powershell -Command 'Invoke`-WebRequest https://x.test'", network: true},
		{name: "PowerShell backtick continuation fails closed", script: "powershell -Command 'Invoke`\n-WebRequest https://x.test'", network: true},
		{name: "PowerShell abbreviated execution policy", script: `powershell -ep RemoteSigned curl https://x.test`, network: true},
		{name: "pwsh abbreviated execution policy before command", script: `pwsh -ep Bypass -Command curl https://x.test`, network: true},
		{name: "Windows PowerShell bare command", script: `powershell Invoke-WebRequest https://x.test`, network: true},
		{name: "pwsh bare token is file mode", script: `pwsh Invoke-WebRequest https://x.test`, network: false},
		{name: "PowerShell joined command flag is invalid", script: `powershell -Command:Invoke-WebRequest https://x.test`, network: false},
		{name: "pwsh command with args", script: `pwsh -CommandWithArgs Invoke-WebRequest https://x.test`, network: true},
		{name: "pwsh abbreviated command with args", script: `pwsh -cwa Invoke-RestMethod https://x.test`, network: true},
		{name: "PowerShell invalid startup option", script: `powershell -DefinitelyInvalid Invoke-WebRequest https://x.test`, network: false},
		{name: "env split curl argv", script: `env -S 'curl https://x.test'`, network: true},
		{name: "env joined split git argv", script: `env --split-string='git push origin main'`, network: true},
		{name: "env clustered split option", script: `env -iS 'curl https://x.test'`, network: true},
		{name: "env split introduces split option", script: `env -S '-S "curl https://x.test"'`, network: true},
		{name: "env split introduces nested env", script: `env -S 'env -S "curl https://x.test"'`, network: true},
		{name: "env split underscore separator", script: `env -S 'curl\_https://x.test'`, network: true},
		{name: "env split environment executable", script: `env -S '${ZERO_TEST_ENV_COMMAND} https://x.test'`, network: true},
		{name: "env split metacharacters stay arguments", script: `env -S 'printf x; curl https://x.test'`, network: false},
		{name: "env split trailing args stay arguments", script: `env -S 'printf ok' curl https://x.test`, network: false},
		{name: "env split environment argument", script: `env -S 'printf ${ZERO_TEST_ENV_COMMAND}'`, network: false},
		{name: "env split shell command", script: `env -S 'sh -c "curl https://x.test"'`, network: true},
		{name: "env split argv0 before curl", script: `env -S '--argv0 harmless curl https://x.test'`, network: true},
		{name: "env split argv0 named curl", script: `env -S '--argv0 curl printf ok'`, network: false},
		// GNU env accepts unambiguous long-option abbreviations: "--split" and
		// "--split-strin" name --split-string exactly like the full spelling does,
		// so the launcher's argv reconstruction must not depend on that one spelling.
		{name: "env abbreviated split option", script: `env --split 'curl https://x.test'`, network: true},
		{name: "env abbreviated split option with value", script: `env --split='curl https://x.test'`, network: true},
		{name: "env minimal abbreviated split option", script: `env --s 'curl https://x.test'`, network: true},
		{name: "env near-full abbreviated split option", script: `env --split-strin 'curl https://x.test'`, network: true},
		{name: "env abbreviated split metacharacters stay arguments", script: `env --split 'printf x; curl https://x.test'`, network: false},
		{name: "env non-split long option is not split-string", script: `env --unset FOO curl https://x.test`, network: true},
		{name: "env non-split long option leaves literal payload alone", script: `env --chdir=/tmp printf ok`, network: false},
		{name: "busybox wget applet", script: `busybox wget https://x.test`, network: true},
		{name: "busybox shell command", script: `busybox sh -c 'curl https://x.test'`, network: true},
		{name: "busybox echo network text", script: `busybox echo wget https://x.test`, network: false},
		{name: "busybox option terminator is unresolved", script: `busybox -- curl https://x.test`, network: true},
		{name: "busybox unknown option is unresolved", script: `busybox -x curl https://x.test`, network: true},
		// The applet name comes from a shell expansion this scan cannot read
		// statically; wordText silently drops it, so the AST resolver must fail
		// closed here rather than reading the blanked token as a clean unknown.
		{name: "busybox dynamic applet fails closed", script: `APPLET=curl; busybox "$APPLET" https://x.test`, network: true},
		{name: "busybox literal applet stays classified on content", script: `busybox echo "not a program" https://x.test`, network: false},
		{name: "busybox ordinary env wrapper", script: `busybox env curl https://x.test`, network: true},
		{name: "busybox env split wrapper", script: `busybox env -S 'git push origin main'`, network: true},
		{name: "busybox ordinary env local control", script: `busybox env true`, network: false},
		{name: "strace curl command", script: `strace -f -o trace.log curl https://x.test`, network: true},
		{name: "strace shell command", script: `strace sh -c 'git push origin main'`, network: true},
		{name: "strace trace path before curl", script: `strace -P /tmp curl https://x.test`, network: true},
		{name: "strace trace path named curl", script: `strace -P curl true`, network: false},
		{name: "strace attach and curl", script: `strace -p 123 curl https://x.test`, network: true},
		{name: "strace long trace option", script: `strace --trace network curl https://x.test`, network: true},
		{name: "strace tips traces curl", script: `strace --tips curl https://x.test`, network: true},
		{name: "strace joined tips traces git", script: `strace --tips=full git push origin main`, network: true},
		{name: "strace abbreviated tips traces curl", script: `strace --tip curl https://x.test`, network: true},
		{name: "strace ambiguous long option is unresolved", script: `strace --ver curl https://x.test`, network: true},
		{name: "strace clustered options", script: `strace -fqo trace.log curl https://x.test`, network: true},
		{name: "strace output named curl", script: `strace -o curl true`, network: false},
		{name: "strace invalid option is unresolved", script: `strace --definitely-invalid curl https://x.test`, network: true},
		// The traced command comes from a shell expansion this scan cannot read
		// statically; shared delegation must retain the original AST words,
		// rather than reading the blanked token as a clean unknown command.
		{name: "strace dynamic child fails closed", script: `APPLET=curl; strace "$APPLET" https://x.test`, network: true},
		{name: "strace literal child stays classified on content", script: `strace true "not a program" https://x.test`, network: false},
		{name: "strace ordinary env wrapper", script: `strace env curl https://x.test`, network: true},
		{name: "strace env split wrapper", script: `strace env -S 'git push origin main'`, network: true},
		{name: "strace dynamic env split source", script: `PAYLOAD='curl https://x.test'; strace env -S "$PAYLOAD"`, network: true},
		{name: "strace literal env split source", script: `strace env -S 'printf ok'`, network: false},
		{name: "strace ordinary env local control", script: `strace env true`, network: false},
		{name: "git local commit", script: `git commit -m "local change"`, network: false},
		// git's value-taking global options put their value in the NEXT token, so a
		// generic "first non-dash token" scan reads the value as the subcommand and
		// misses the network verb entirely (issue #703 review).
		{name: "git -C push", script: "git -C repo push origin main", network: true},
		{name: "git -c config push", script: "git -c http.sslVerify=false push origin main", network: true},
		{name: "git --git-dir fetch", script: "git --git-dir /repo/.git fetch origin", network: true},
		{name: "git --work-tree pull", script: "git --work-tree /repo pull origin main", network: true},
		{name: "git --attr-source push", script: "git --attr-source HEAD push origin main", network: true},
		{name: "git -C local commit", script: `git -C repo commit -m "local change"`, network: false},
		{name: "git help topic is offline", script: "git --help push", network: false},
		{name: "git -C help topic is offline", script: "git -C repo --help push", network: false},
		{name: "git -c version topic is offline", script: "git -c user.name=test --version push", network: false},
		{name: "git.exe push", script: "git.exe push origin main", network: true},
		{name: "git.cmd push", script: "git.cmd push origin main", network: true},
		{name: "git.exe local commit", script: `git.exe commit -m "local change"`, network: false},
		{name: "gh release download", script: "gh release download v1.0.0", network: true},
		{name: "no network", script: "ls -la && echo done", network: false},
		{name: "process pattern is not network", script: `pkill -f "python3 -m http.server 8000"`, network: false},
		{name: "process listing is not special-cased", script: "ps aux", network: false},

		{name: "unparseable", script: `'unterminated quote`, tooComplex: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AnalyzeCommand(tc.script)
			if got.Interactive != tc.interactive || got.Destructive != tc.destructive ||
				got.Network != tc.network || got.TooComplex != tc.tooComplex {
				t.Fatalf("AnalyzeCommand(%q) = %#v, want interactive=%v destructive=%v network=%v tooComplex=%v",
					tc.script, got, tc.interactive, tc.destructive, tc.network, tc.tooComplex)
			}
		})
	}
}

func TestAnalyzeCommandFailsClosedAtShellLauncherDepthLimit(t *testing.T) {
	command := "curl https://x.test"
	for range maxAnalyzerDepth + 1 {
		command = "sh -c '" + strings.ReplaceAll(command, "'", `'"'"'`) + "'"
	}
	if got := AnalyzeCommand(command); !got.Network {
		t.Fatalf("AnalyzeCommand(%q) = %#v; want network", command, got)
	}
}

func TestAnalyzeCommandEmptyIsClean(t *testing.T) {
	if got := AnalyzeCommand("   "); got.Interactive || got.Destructive || got.Network || got.TooComplex {
		t.Fatalf("empty script should be clean, got %#v", got)
	}
}

// unescapeDoubleQuoted implements POSIX double-quote escape removal, which the
// parser deliberately leaves to expansion time. For an argv token that is
// harmless; for a shell launcher's -c operand the text IS the next command, so
// keeping `\"` verbatim handed the recursion a fragment and lost everything
// after it.
func TestUnescapeDoubleQuoted(t *testing.T) {
	cases := []struct{ in, want string }{
		{`plain`, `plain`},
		{`sh -c \"curl x\"`, `sh -c "curl x"`},
		{`a\\b`, `a\b`},
		{`\$HOME`, `$HOME`},
		{"\\`cmd\\`", "`cmd`"},
		// A backslash before anything else is literal: a quoted Windows path
		// must survive intact.
		{`C:\Users\me\file.txt`, `C:\Users\me\file.txt`},
		{`C:\temp\n`, `C:\temp\n`},
		{"line\\" + "\n" + "cont", "linecont"},
	}
	for _, testCase := range cases {
		if got := unescapeDoubleQuoted(testCase.in); got != testCase.want {
			t.Errorf("unescapeDoubleQuoted(%q) = %q, want %q", testCase.in, got, testCase.want)
		}
	}
}

// A quoted CMD payload is command SOURCE, but a quoted path containing a space
// is a program name. CMD resolves that ambiguity by trying both, so the
// classifier does too and fails closed if either reading reaches the network.
func TestAnalyzeCommandReadsQuotedCMDPayloadBothWays(t *testing.T) {
	network := []string{
		`cmd /c "git push origin main"`,
		`cmd /c "curl https://evil.test"`,
		`cmd /k "git fetch origin"`,
		`call "git push origin main"`,
		`start "" "curl https://evil.test"`,
		// Program-path reading: the quoted token IS the executable.
		`cmd /c "C:\Program Files\curl\curl.exe"`,
	}
	for _, command := range network {
		if analysis := AnalyzeCommand(command); !analysis.Network {
			t.Errorf("AnalyzeCommand(%q).Network = false, want true", command)
		}
	}
	local := []string{
		`cmd /c "git status"`,
		`cmd /c "echo hello world"`,
		`cmd /c "C:\Program Files\git\bin\git.exe status"`,
		`call "git status"`,
	}
	for _, command := range local {
		if analysis := AnalyzeCommand(command); analysis.Network {
			t.Errorf("AnalyzeCommand(%q).Network = true, want false", command)
		}
	}
}

// GNU env appends the remaining argv to the argv the split string produced, so
// proving the -S operand is literal proves nothing about the whole invocation:
// `env -S 'sh -c' "$PAYLOAD"` runs text this scan cannot read.
func TestAnalyzeCommandFailsClosedOnDynamicArgvAfterEnvSplit(t *testing.T) {
	dynamic := []string{
		`env -S 'git push origin main' "$EXTRA"`,
		`env -S 'printf ok' $PAYLOAD`,
		`env -S 'printf ok' "${PAYLOAD}" https://evil.test`,
		`env -S 'sh -c' "$PAYLOAD"`,
		`env --split-string='printf ok' $PAYLOAD`,
		`sudo env -S 'printf ok' $PAYLOAD`,
	}
	for _, command := range dynamic {
		if analysis := AnalyzeCommand(command); !analysis.Network {
			t.Errorf("AnalyzeCommand(%q).Network = false, want true", command)
		}
	}
	// Trailing LITERAL argv is fully readable and must not cost a prompt.
	literal := []string{
		`env -S 'printf ok' literal args`,
		`env -S 'git status' --`,
		`env -S 'echo hi'`,
	}
	for _, command := range literal {
		if analysis := AnalyzeCommand(command); analysis.Network {
			t.Errorf("AnalyzeCommand(%q).Network = true, want false", command)
		}
	}
}

// eval and CMD's echo-suppression prefix were handled on the fallback path but
// not on the AST path, so the identical text was network only when something
// else in the command happened to defeat the parser.
func TestAnalyzeCommandClassifiesEvalAndEchoPrefixOnParseablePath(t *testing.T) {
	network := []string{
		`eval git push origin main`,
		`eval "curl https://evil.test"`,
		`eval $PAYLOAD`,
		`@curl https://evil.test`,
		`@git push origin main`,
	}
	for _, command := range network {
		analysis := AnalyzeCommand(command)
		if analysis.TooComplex {
			t.Fatalf("AnalyzeCommand(%q) is TooComplex; this test must cover the parseable path", command)
		}
		if !analysis.Network {
			t.Errorf("AnalyzeCommand(%q).Network = false, want true", command)
		}
	}
	for _, command := range []string{`eval echo hi`, `eval git status`, `@echo hello`} {
		if analysis := AnalyzeCommand(command); analysis.Network {
			t.Errorf("AnalyzeCommand(%q).Network = true, want false", command)
		}
	}
}

// send-pack is push's plumbing counterpart and performs the same egress; both
// classification paths read one subcommand list, so covering gitUsesNetwork
// covers the fallback too.
func TestGitSendPackIsNetwork(t *testing.T) {
	for _, command := range []string{
		`git send-pack origin main`,
		`git -C repo send-pack origin main`,
		`git send-pack origin main & rem '`,
	} {
		if analysis := AnalyzeCommand(command); !analysis.Network && !matchesUnparseableNetwork(command) {
			t.Errorf("%q classified as local; send-pack pushes to a remote", command)
		}
	}
}

func TestGitSubcommandPreservesOriginalAndNormalizedSpelling(t *testing.T) {
	selection, ok := GitSubcommand([]string{"-c", "alias.STATUS=!curl", "STATUS"})
	if !ok {
		t.Fatal("GitSubcommand did not resolve the alias-shaped subcommand")
	}
	if selection.Index != 2 || selection.Original != "STATUS" || selection.Normalized != "status" {
		t.Fatalf("GitSubcommand = %#v, want index 2, original STATUS, normalized status", selection)
	}

	lowercase, ok := GitSubcommand([]string{"--attr-source", "HEAD", "status"})
	if !ok || lowercase.Index != 2 || lowercase.Original != "status" || lowercase.Normalized != "status" {
		t.Fatalf("lowercase GitSubcommand = %#v ok=%v", lowercase, ok)
	}
}

// TestGitGlobalOptionConsumesValueFoldsShortCaseDeliberately pins the case-fold
// in GitGlobalOptionConsumesValue. -c and -C are DIFFERENT git options that
// today both take a separate-token value, and the callers normalize the option
// name before asking, so the two necessarily share one answer here. That is
// safe only while their value-taking behavior agrees; if git ever changes one
// of them, this test fails and the distinction has to be restored at the call
// site by passing the original token, not by adding an unreachable uppercase
// case to a switch that compares lowercased input.
func TestGitGlobalOptionConsumesValueFoldsShortCaseDeliberately(t *testing.T) {
	for _, option := range []string{"-c", "-C"} {
		if !GitGlobalOptionConsumesValue(option) {
			t.Fatalf("GitGlobalOptionConsumesValue(%q) = false, want true", option)
		}
	}
	// Long options are all-lowercase in git; the fold must not invent new ones.
	for _, option := range []string{"-d", "-x", "--exec-path", "--not-an-option"} {
		if GitGlobalOptionConsumesValue(option) {
			t.Fatalf("GitGlobalOptionConsumesValue(%q) = true, want false", option)
		}
	}
}

// TestGitSubcommandConsumesDashCOperand proves the -C operand is consumed in
// both the separated and joined spellings, so the subcommand git dispatches is
// the one found. Missing this would make `git -C repo push` resolve `repo` as
// the subcommand, and an unknown subcommand is classified proven-local — the
// network gate would be dropped on a real push.
func TestGitSubcommandConsumesDashCOperand(t *testing.T) {
	for _, tc := range []struct {
		words []string
		want  string
		index int
	}{
		{words: []string{"-C", "repo", "push", "origin", "main"}, want: "push", index: 2},
		{words: []string{"-Crepo", "push", "origin", "main"}, want: "push", index: 1},
		{words: []string{"-C", "repo", "status"}, want: "status", index: 2},
		{words: []string{"-c", "a=b", "push"}, want: "push", index: 2},
	} {
		info, ok := GitSubcommand(tc.words)
		if !ok || info.Normalized != tc.want || info.Index != tc.index {
			t.Fatalf("GitSubcommand(%q) = %+v ok=%v, want %q at %d", tc.words, info, ok, tc.want, tc.index)
		}
	}
	// `git -C repo` runs nothing: the operand is consumed and no subcommand is left.
	if info, ok := GitSubcommand([]string{"-C", "repo"}); ok {
		t.Fatalf("GitSubcommand(-C repo) = %+v, want no subcommand", info)
	}
}
