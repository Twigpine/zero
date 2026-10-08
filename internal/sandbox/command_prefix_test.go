package sandbox

import "testing"

func TestUnsafeCommandPrefixLauncherIgnoresVersionAndExtension(t *testing.T) {
	for _, program := range []string{
		"python3", "python", "node", "git", "sh", "bash", "sudo",
		"python3.11", "python3.12", "python2.7", "python3.13.1",
		"python.exe", "python3.exe", "node.exe", "git.exe", "bash.exe",
		"PYTHON3.11", "Node.EXE",
		"cmd", "cmd.exe", "wsl", "wsl.exe",
		// Windows resolves these to the same executable.
		"python.", "python3.", "python3.11.", "python.exe.", "PYTHON.",
		"python.cmd", "python.bat", "python.com", "python.ps1",
		"PYTHON~1.EXE", "POWERS~1.EXE", "python.exe::$DATA",
		// Versioned and ABI-suffixed interpreters.
		"python3.7m", "python3.6dm", "pythonw3.11", "python-3.11",
		// Free-threaded CPython (PEP 703) ships beside the GIL build.
		"python3.13t", "python3.13t.exe", "python3.14t", "python3.13td",
		// Distribution and build-channel spellings of the same interpreter.
		"nodejs", "nodejs.exe", "node-nightly", "python3-dbg", "python3.11-dbg",
		"python3.13t-dbg", "python3-debug", "python3.11-debug", "node-beta",
		"pwsh-preview", "bash-static", "sudoedit",
		// Twins of npm and npx, which the list already refuses.
		"pnpm", "yarn", "bunx", "pnpm.cmd", "yarn.exe",
		"perl5.36", "ruby3.2", "php8", "lua5.4", "node22.exe", "bash5",
	} {
		if !unsafeCommandPrefix([]string{program}) {
			t.Errorf("unsafeCommandPrefix([%q]) = false, want true", program)
		}
	}
}

func TestUnsafeCommandPrefixKeepsOrdinaryCommandsGrantable(t *testing.T) {
	for _, prefix := range [][]string{
		{"cargo", "build"},
		{"go", "test"},
		{"ls", "-la"},
		{"rg", "--json"},
		{"7z", "l"},
		{"base64", "-d"},
		{"kubectl", "get", "pods"},
		{"docker", "ps"},
		{"sha256sum", "file"},
		{"gcc-13", "-c"},
		{"node-gyp", "build"},
		{"python3-config", "--includes"},
		{"s3cmd", "ls"},
		{"cat", "file"},
		{"zstd", "-d"},
		{"sqlite3", "db"},
		{"yt-dlp", "url"},
		{"perl-doc", "-h"},
		{"php-fpm", "-t"},
		{"ruby-lsp", "--version"},
		{"lua-language-server", "--check"},
		{"nodemon", "app.js"},
	} {
		if unsafeCommandPrefix(prefix) {
			t.Errorf("unsafeCommandPrefix(%q) = true, want false", prefix)
		}
	}
}
func TestNormalizeLauncherName(t *testing.T) {
	for _, testCase := range []struct{ in, want string }{
		{"python3.11", "python3"},
		{"python2.7", "python2"},
		{"pypy3", "pypy3"},
		{"node.exe", "node"},
		{"POWERSHELL.EXE", "powershell"},
		{"7z", "7z"},
		{"base64", "base64"},
		{"sha256sum", "sha256sum"},
		{"python.", "python"},
		{"python3.11.exe.", "python3"},
		{"python3.7m", "python3"},
		{"python3.6dm", "python3"},
		{"python3.13t", "python3"},
		{"python3.13td", "python3"},
		{"python3.11-dbg", "python3"},
		{"node-nightly", "node"},
		{"node-gyp", "node-gyp"},
		{"python3-config", "python3-config"},
		{"zstd", "zstd"},
		{"cat", "cat"},
		{"cargo", "cargo"},
		{"", ""},
	} {
		if got := normalizeLauncherName(testCase.in); got != testCase.want {
			t.Errorf("normalizeLauncherName(%q) = %q, want %q", testCase.in, got, testCase.want)
		}
	}
}

// Session-scoped grants are the agent's other entry point (loop.go grants them
// for a single turn without touching the store), so they must refuse the same
// launcher spellings the persisted path does.
func TestGrantCommandPrefixForSessionRefusesLauncherVariants(t *testing.T) {
	engine := &Engine{commandPrefixes: newCommandPrefixGrantSet()}
	engine.GrantCommandPrefixForSession("bash", []string{"cargo", "build"})
	if _, ok := engine.LookupCommandPrefixForSession("bash", []string{"cargo", "build", "--release"}); !ok {
		t.Fatal("CONTROL BROKEN: an ordinary session prefix grant did not match")
	}
	for _, prefix := range [][]string{
		{"python3.11", "-c"},
		{"python.exe", "-c"},
		{"cmd", "/c"},
	} {
		engine.GrantCommandPrefixForSession("bash", prefix)
		command := append(append([]string(nil), prefix...), "whatever")
		if _, ok := engine.LookupCommandPrefixForSession("bash", command); ok {
			t.Errorf("session grant %q matched %q, want refused", prefix, command)
		}
	}
}
