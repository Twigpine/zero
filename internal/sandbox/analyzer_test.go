package sandbox

import "testing"

func TestAnalyzeCommand(t *testing.T) {
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

func TestAnalyzeCommandEmptyIsClean(t *testing.T) {
	if got := AnalyzeCommand("   "); got.Interactive || got.Destructive || got.Network || got.TooComplex {
		t.Fatalf("empty script should be clean, got %#v", got)
	}
}
