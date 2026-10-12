package sandbox

import "testing"

func TestAnalyzeCommand(t *testing.T) {
	cases := []struct {
		name        string
		script      string
		interactive bool
		destructive bool
		network     bool
		localServer bool
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
		{name: "python http server", script: "python3 -m http.server 8000", network: true, localServer: true},
		{name: "python pip install", script: "python3 -m pip install requests", network: true},
		{name: "npm install", script: "npm install", network: true},
		{name: "npm ci", script: "npm ci", network: true},
		{name: "npm create", script: "npm create vite@latest .", network: true},
		{name: "npm registry query", script: "npm view typescript version --fetch-retries=0", network: true},
		{name: "npm metadata search", script: "npm search typescript", network: true},
		{name: "npm offline install", script: "npm install --offline", network: false},
		{name: "npm version is offline", script: "npm --version", network: false},
		{name: "npm start", script: "npm start", network: true, localServer: true},
		{name: "npm run dev", script: "npm run dev", network: true, localServer: true},
		{name: "npx http server", script: "npx http-server public -p 8080 -a 127.0.0.1", network: true},
		{name: "direct http server", script: "http-server public -p 8080 -a 127.0.0.1", network: true, localServer: true},
		{name: "direct vite", script: "vite --host 127.0.0.1", network: true, localServer: true},
		{name: "next dev", script: "next dev", network: true, localServer: true},
		{name: "git clone", script: "git clone https://example.com/repo.git", network: true},
		{name: "git fetch", script: "git fetch origin", network: true},
		{name: "git status is offline", script: "git status", network: false},
		{name: "gh release download", script: "gh release download v1.0.0", network: true},
		{name: "no network", script: "ls -la && echo done", network: false},
		{name: "process pattern is not network", script: `pkill -f "python3 -m http.server 8000"`, network: false},
		{name: "process listing is not special-cased", script: "ps aux", network: false},

		// Binding a port is not reaching out. These four are the shape that made
		// ordinary local work stop for a network approval it never needed, and the
		// fetching siblings beside them are what must keep asking.
		{name: "pnpm dev binds", script: "pnpm dev", network: true, localServer: true},
		{name: "yarn serve binds", script: "yarn serve", network: true, localServer: true},
		{name: "bun run preview binds", script: "bun run preview", network: true, localServer: true},
		{name: "pnpm install still fetches", script: "pnpm install", network: true},
		{name: "yarn add still fetches", script: "yarn add left-pad", network: true},
		{name: "npm publish still fetches", script: "npm publish", network: true},
		{name: "python pip install still fetches", script: "python3 -m pip install requests", network: true},

		// A global option BEFORE the subcommand is documented usage for all four
		// package managers, and the subcommand scan skipped the flag but not the
		// word it consumed — so the option's VALUE was read as the subcommand and
		// both the dev server and the genuine fetch fell out of their categories.
		{name: "npm prefix then run dev", script: "npm --prefix ./web run dev", network: true, localServer: true},
		{name: "pnpm dir then run dev", script: "pnpm -C ./web run dev", network: true, localServer: true},
		{name: "yarn cwd then dev", script: "yarn --cwd ./web dev", network: true, localServer: true},
		{name: "npm prefix then install still fetches", script: "npm --prefix ./web install", network: true},
		{name: "npm registry then install still fetches", script: "npm --registry https://registry.test install", network: true},
		{name: "pnpm filter then add still fetches", script: "pnpm --filter web add left-pad", network: true},
		// The candidate window stops at two operands. Past that, "dev" and "start"
		// are arguments to something else and reading them as the subcommand would
		// stop an ordinary build for a network approval.
		{name: "npm run build with a workspace named dev", script: "npm run build --workspace dev", network: false},
		{name: "npm run test forwarding a start flag", script: "npm run test -- --grep start", network: false},

		{name: "unparseable", script: `'unterminated quote`, tooComplex: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AnalyzeCommand(tc.script)
			if got.Interactive != tc.interactive || got.Destructive != tc.destructive ||
				got.Network != tc.network || got.LocalServer != tc.localServer || got.TooComplex != tc.tooComplex {
				t.Fatalf("AnalyzeCommand(%q) = %#v, want interactive=%v destructive=%v network=%v localServer=%v tooComplex=%v",
					tc.script, got, tc.interactive, tc.destructive, tc.network, tc.localServer, tc.tooComplex)
			}
		})
	}
}

func TestAnalyzeCommandEmptyIsClean(t *testing.T) {
	if got := AnalyzeCommand("   "); got.Interactive || got.Destructive || got.Network || got.TooComplex {
		t.Fatalf("empty script should be clean, got %#v", got)
	}
}

// A FLAGGED INVOCATION CLASSIFIES LIKE ITS UNFLAGGED SELF, AT ANY ARITY.
//
// Skipping flags is not enough, because it does not skip the words they CONSUME:
// `npm --prefix ./web install` resolved to "./web", the option's VALUE, and the
// invocation stopped being recognised at all — so a command genuinely fetching
// from the network lost the approval its plain spelling gets.
//
// The first fix offered a fixed window of two positions, which covered exactly
// ONE value-taking option. A second one walked straight past it. All four of
// these were measured wrong before the window was replaced:
//
//	npm --prefix ./web --loglevel warn install
//	npm --registry https://r.test --prefix ./web install
//	pnpm -C ./web --filter web add left-pad
//	yarn --cwd ./web --verbose install
//
// Adjacency decides now — an option can only eat the word beside it — so the
// arity is counted rather than assumed, and no option is ever named. Pairing
// each flagged form against its unflagged baseline is what makes that a property
// rather than a list of examples: a change that shifts BOTH still fails.
func TestAFlaggedInvocationClassifiesLikeItsUnflaggedSelf(t *testing.T) {
	for _, pair := range []struct{ bare, flagged string }{
		{"npm install", "npm --prefix ./web --loglevel warn install"},
		{"npm install", "npm --registry https://r.test --prefix ./web install"},
		{"npm install", "npm --prefix=./web --loglevel=warn install"},
		{"npm install", "npm -w a -w b --loglevel warn install"},
		{"npm run dev", "npm --prefix ./web --loglevel warn run dev"},
		{"npm run deploy", "npm --prefix ./web --loglevel warn run deploy"},
		{"pnpm add left-pad", "pnpm -C ./web --filter web add left-pad"},
		{"yarn install", "yarn --cwd ./web --verbose install"},
	} {
		bare, flagged := AnalyzeCommand(pair.bare), AnalyzeCommand(pair.flagged)
		if bare.Network != flagged.Network {
			t.Errorf("%q needs network=%v but %q reports %v: an option value was read as the subcommand",
				pair.bare, bare.Network, pair.flagged, flagged.Network)
		}
		if bare.LocalServer != flagged.LocalServer {
			t.Errorf("%q localServer=%v but %q reports %v", pair.bare, bare.LocalServer, pair.flagged, flagged.LocalServer)
		}
	}
}

// AND THE WINDOW STILL CLOSES ON AN UNFLAGGED OPERAND. Widening it by arity must
// not reach words that belong to something else: "dev" and "start" here are
// arguments to a script, not subcommands. They are safe for a structural reason
// rather than a lucky one — the FIRST operand is unflagged, so it cannot be any
// option's value and the window is one position wide however many flags follow.
func TestAnUnflaggedFirstOperandClosesTheSubcommandWindow(t *testing.T) {
	for _, command := range []string{
		"npm run build --workspace dev",
		"npm run test -- --grep start",
		"npm run build --filter start",
	} {
		if got := AnalyzeCommand(command); got.LocalServer {
			t.Errorf("%q read an argument as a dev server", command)
		}
	}

	// THE RUN HAS TO BE CONTIGUOUS, not merely a count of flags anywhere in the
	// line. Counting every flag would let TRAILING options widen the window back
	// over words they have nothing to do with, and these three flip to false
	// positives the moment it does — measured:
	//
	//	npm run --grep foo --tag dev            -> network AND localServer
	//	npm run test --reporter x --filter dev  -> network
	//	npm run build --a x --b start           -> network
	//
	// In each, the first operand is unflagged, so nothing after it can be an
	// option's value however many options follow.
	for _, command := range []string{
		"npm run --grep foo --tag dev",
		"npm run test --reporter x --filter dev",
		"npm run build --a x --b start",
	} {
		if got := AnalyzeCommand(command); got.Network || got.LocalServer {
			t.Errorf("%q was widened past its unflagged first operand: network=%v localServer=%v",
				command, got.Network, got.LocalServer)
		}
	}
}

// A DIRECTORY NAMED dev OR start IS READ AS A SCRIPT NAME, and that is pinned
// rather than fixed. The ambiguity is the point of the window: with `npm --prefix
// dev install` the analyzer genuinely cannot tell whether "dev" is the option's
// value or the subcommand, so it offers both readings and takes the union.
//
// It costs nothing here. Network is already true and correct — the install
// really does fetch — and LocalServer only implies Network, so the union is
// conservative in the safe direction. Removing the ambiguity would mean naming
// which options take values, which is the enumeration this design exists to
// avoid.
func TestADirectoryNamedLikeAScriptIsReadConservatively(t *testing.T) {
	for _, command := range []string{
		"npm --prefix dev install",
		"npm --prefix start install",
	} {
		got := AnalyzeCommand(command)
		if !got.Network {
			t.Errorf("%q is an install and must still need network", command)
		}
		if !got.LocalServer {
			t.Errorf("%q no longer takes the conservative reading; if that is deliberate, update this test", command)
		}
	}
}
