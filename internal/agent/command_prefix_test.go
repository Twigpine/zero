package agent

import (
	"runtime"
	"testing"

	"github.com/Gitlawb/zero/internal/sandbox"
	"github.com/Gitlawb/zero/internal/tools"
)

func TestProposedCommandPrefixUsesSafeSimpleCommands(t *testing.T) {
	got := proposedCommandPrefix("bash", map[string]any{"command": "go test ./..."})
	want := []string{"go", "test", "./..."}
	if !equalStringSlices(got, want) {
		t.Fatalf("prefix = %#v, want %#v", got, want)
	}
}

func TestProposedCommandPrefixSupportsExecCommand(t *testing.T) {
	got := proposedCommandPrefix("exec_command", map[string]any{"cmd": "go test ./..."})
	want := []string{"go", "test", "./..."}
	if !equalStringSlices(got, want) {
		t.Fatalf("prefix = %#v, want %#v", got, want)
	}
}

func TestProposedCommandPrefixHonorsValidatedRequestedPrefix(t *testing.T) {
	got := proposedCommandPrefix("bash", map[string]any{
		"command":     "git status --short",
		"prefix_rule": []any{"git", "status"},
	})
	want := []string{"git", "status"}
	if !equalStringSlices(got, want) {
		t.Fatalf("prefix = %#v, want %#v", got, want)
	}
}

func TestProposedCommandPrefixSupportsSegmentedCommands(t *testing.T) {
	got := proposedCommandPrefix("bash", map[string]any{"command": "ps aux | head -5"})
	if runtime.GOOS == "windows" {
		// head is MSYS-prone on Windows (#458), so proposedCommandPrefix must
		// not offer "ps aux" as a reusable prefix here: approving it would
		// escalate the whole command, including the uncovered head segment,
		// to bypass the sandbox unreviewed. See
		// TestProposedCommandPrefixRejectsPrefixLeavingUnsafeTailUncovered for
		// the platform-independent regression coverage of this behavior.
		if got != nil {
			t.Fatalf("expected no prefix on Windows because head is MSYS-prone, got %#v", got)
		}
		return
	}
	want := []string{"ps", "aux"}
	if !equalStringSlices(got, want) {
		t.Fatalf("prefix = %#v, want %#v", got, want)
	}
}

// TestProposedCommandPrefixRejectsPrefixLeavingUnsafeTailUncovered guards
// against proposedCommandPrefix offering to approve one segment of a
// multi-segment command (e.g. "ps aux") while a different segment (e.g. "npm
// install") is not known-safe. shellExecutionArgsForApproval escalates the
// entire command once any prefix is approved, so an uncovered unsafe segment
// would bypass the sandbox unreviewed. Uses npm, which is never known-safe on
// any platform, so the assertion does not depend on runtime.GOOS.
func TestProposedCommandPrefixRejectsPrefixLeavingUnsafeTailUncovered(t *testing.T) {
	if got := proposedCommandPrefix("bash", map[string]any{"command": "ps aux && npm install"}); got != nil {
		t.Fatalf("expected no prefix because npm segment is not known-safe, got %#v", got)
	}
}

func TestProposedCommandPrefixHonorsRequestedPrefixAcrossSegments(t *testing.T) {
	got := proposedCommandPrefix("bash", map[string]any{
		"command":     "git status --short && git status --branch",
		"prefix_rule": []any{"git", "status"},
	})
	want := []string{"git", "status"}
	if !equalStringSlices(got, want) {
		t.Fatalf("prefix = %#v, want %#v", got, want)
	}
}

func TestProposedCommandPrefixRejectsRequestedPrefixThatDoesNotCoverSegments(t *testing.T) {
	got := proposedCommandPrefix("bash", map[string]any{
		"command":     "ps aux && npm install",
		"prefix_rule": []any{"ps", "aux"},
	})
	if got != nil {
		t.Fatalf("partial requested prefix should be rejected, got %#v", got)
	}
}

func TestProposedCommandPrefixRejectsUnsafeRequestedPrefix(t *testing.T) {
	got := proposedCommandPrefix("bash", map[string]any{
		"command":     "git status --short",
		"prefix_rule": []any{"git"},
	})
	if got != nil {
		t.Fatalf("broad requested prefix should be rejected, got %#v", got)
	}
}

// The launcher denylist is only worth anything if it holds on the path a user
// actually sees. A model naming its own prefix_rule is that path: before
// launcher names were normalized, "python3.11" was offered as a one-token
// grant, and hasStringPrefix then matched every later python3.11 invocation.
func TestProposedCommandPrefixRejectsLauncherSpellingsInRequestedPrefix(t *testing.T) {
	for _, testCase := range []struct {
		command string
		rule    []any
	}{
		{"python3.11 script.py", []any{"python3.11"}},
		{"python.exe script.py", []any{"python.exe"}},
		{"node.exe app.js", []any{"node.exe"}},
		{"cmd /c whoami", []any{"cmd"}},
		{"busybox sh -c id", []any{"busybox"}},
		{"uv run main.py", []any{"uv", "run"}},
		{"python3.11 -c import_os", []any{"python3.11", "-c"}},
	} {
		got := proposedCommandPrefix("bash", map[string]any{
			"command":     testCase.command,
			"prefix_rule": testCase.rule,
		})
		if got != nil {
			t.Errorf("prefix_rule %v for %q was offered as %#v, want rejected", testCase.rule, testCase.command, got)
		}
	}
}

func TestProposedCommandPrefixStillOffersOrdinaryCommands(t *testing.T) {
	got := proposedCommandPrefix("bash", map[string]any{
		"command":     "cargo build --release",
		"prefix_rule": []any{"cargo", "build"},
	})
	if len(got) != 2 || got[0] != "cargo" || got[1] != "build" {
		t.Fatalf("ordinary prefix rule = %#v, want [cargo build]", got)
	}
}

func TestProposedCommandPrefixRejectsUnsafeShellForms(t *testing.T) {
	cases := []string{
		"cat < in > out",
		"FOO=bar go test",
		"echo $(whoami)",
		"cat *.go",
		"bash -lc go test",
	}
	for _, command := range cases {
		t.Run(command, func(t *testing.T) {
			if got := proposedCommandPrefix("bash", map[string]any{"command": command}); got != nil {
				t.Fatalf("unsafe command got prefix %#v", got)
			}
		})
	}
}

func TestProposedCommandPrefixRejectsUnsafeLaunchers(t *testing.T) {
	cases := []string{
		"find . -type f",
		"xargs rm -rf /tmp/x",
		"timeout 5 go test ./...",
		"nice go test ./...",
		"nohup go test ./...",
		"watch ls",
		"ssh host ls",
		"make test",
		"npm run dev",
		"command git status",
		"eval echo hi",
		"exec echo hi",
		"python script.py",
		"node script.js",
		"./script.sh --flag",
		"/tmp/script.sh --flag",
	}
	for _, command := range cases {
		t.Run(command, func(t *testing.T) {
			if got := proposedCommandPrefix("bash", map[string]any{"command": command}); got != nil {
				t.Fatalf("unsafe launcher got prefix %#v", got)
			}
		})
	}
}

func TestMatchCommandPrefixCoversSegmentedCommandWithSafeTail(t *testing.T) {
	engine := sandbox.NewEngine(sandbox.EngineOptions{WorkspaceRoot: t.TempDir()})
	engine.GrantCommandPrefixForSession("bash", []string{"ps", "aux"})
	// head is MSYS-prone on Windows (#458) and must not count as a known-safe tail.
	command := "ps aux | head -5"
	if runtime.GOOS == "windows" {
		command = "ps aux | echo ok"
	}

	grant, ok, session := matchCommandPrefix("bash", map[string]any{"command": command}, Options{Sandbox: engine})
	if !ok || !session || !equalStringSlices(grant.Prefix, []string{"ps", "aux"}) {
		t.Fatalf("match = %#v ok=%v session=%v, want session ps aux prefix", grant, ok, session)
	}
}

func TestKnownSafeCommandSegmentRejectsMsysProneOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only known-safe MSYS guard")
	}
	for _, command := range [][]string{{"head", "-5"}, {"cat", "file.txt"}, {"grep", "pat"}} {
		if knownSafeCommandSegment(command) {
			t.Fatalf("expected %q to be unsafe on Windows, got known-safe", command)
		}
	}
	if !knownSafeCommandSegment([]string{"echo", "ok"}) {
		t.Fatal("expected echo to remain known-safe on Windows")
	}
	if !tools.MsysProneCommandName("head") {
		t.Fatal("expected head to be MSYS-prone")
	}
}

func TestMatchCommandPrefixRejectsUncoveredSegment(t *testing.T) {
	engine := sandbox.NewEngine(sandbox.EngineOptions{WorkspaceRoot: t.TempDir()})
	engine.GrantCommandPrefixForSession("bash", []string{"ps", "aux"})

	if grant, ok, session := matchCommandPrefix("bash", map[string]any{"command": "ps aux && npm install"}, Options{Sandbox: engine}); ok {
		t.Fatalf("match = %#v session=%v, want no match because npm segment is uncovered", grant, session)
	}
}

func TestProposedCommandPrefixRejectsRequestedUnsafeLauncherPrefix(t *testing.T) {
	got := proposedCommandPrefix("bash", map[string]any{
		"command":     "find . -type f",
		"prefix_rule": []any{"find", "."},
	})
	if got != nil {
		t.Fatalf("unsafe requested launcher prefix should be rejected, got %#v", got)
	}
}
