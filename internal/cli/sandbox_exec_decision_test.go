package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/sandbox"
)

// governedNestedWorkspace is a workspace inside somebody else's repository: an
// ancestor carries .git and the workspace itself does not. Setup plans no git
// carveouts for it, which is why a repository must not be created there.
func governedNestedWorkspace(t *testing.T) (ancestor, workspace string) {
	t.Helper()
	ancestor = t.TempDir()
	if err := os.MkdirAll(filepath.Join(ancestor, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	workspace = filepath.Join(ancestor, "packages", "app")
	if err := os.MkdirAll(filepath.Join(workspace, "sub dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	return ancestor, workspace
}

func sandboxExecTestDeps(workspace string) appDeps {
	return appDeps{
		getwd: func() (string, error) { return workspace, nil },
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{}, nil
		},
		// No native runner. A command that got past the decision would run
		// unwrapped, which is exactly what makes a missing refusal visible on
		// disk instead of only in a return value.
		selectSandboxBackend: func(sandbox.BackendOptions) sandbox.Backend {
			return sandbox.Backend{Name: sandbox.BackendUnavailable, Fallback: true, Message: "no native sandbox in this test"}
		},
	}
}

// `zero sandbox exec` REFUSES WHAT A SESSION WOULD REFUSE.
//
// The command is documented as taking the path a shell tool takes. A shell tool
// is evaluated by the engine and then planned; this went straight to the plan,
// so nothing that lives in Evaluate applied. In a workspace governed by an
// ancestor repository that is the nested-repository guard, and that guard is the
// only thing standing in for the git carveouts setup deliberately does not plan
// there. `zero sandbox exec -- git init` created the repository, writable config
// and hooks included.
//
// Driven through the real CLI entry point, with a backend that would run the
// command unwrapped if it were let through, and checked on disk: the refusal has
// to come with the reason an operator can act on, and no repository may exist
// afterwards. Reported by @jatmn.
func TestSandboxExecRefusesToCreateARepositoryInAGovernedWorkspace(t *testing.T) {
	for _, testCase := range []struct {
		name string
		argv []string
		made []string
	}{
		{name: "git init", argv: []string{"git", "init"}, made: []string{".git"}},
		{name: "git init behind -C and a quoted directory", argv: []string{"git", "-C", "sub dir", "init"}, made: []string{"sub dir/.git"}},
		{name: "git init behind a shell launcher", argv: []string{"sh", "-c", "git init"}, made: []string{".git"}},
		{name: "git init behind env", argv: []string{"env", "GIT_TRACE=0", "git", "init"}, made: []string{".git"}},
		{name: "git clone", argv: []string{"git", "clone", "https://example.invalid/repo.git", "vendored"}, made: []string{"vendored"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, workspace := governedNestedWorkspace(t)
			var stdout, stderr bytes.Buffer
			args := append([]string{"sandbox", "exec", "--"}, testCase.argv...)
			exitCode := runWithDeps(args, &stdout, &stderr, sandboxExecTestDeps(workspace))

			for _, made := range testCase.made {
				if _, err := os.Lstat(filepath.Join(workspace, filepath.FromSlash(made))); err == nil {
					t.Errorf("%s exists after the command: the repository was created under the plain workspace grant", made)
				}
			}
			if exitCode == exitSuccess {
				t.Errorf("exit = %d for a command a session refuses; stderr:\n%s", exitCode, stderr.String())
			}
			if !strings.Contains(stderr.String(), "decision=deny") {
				t.Errorf("stderr does not report the decision:\n%s", stderr.String())
			}
			if !strings.Contains(stderr.String(), "sits inside an existing git repository") {
				t.Errorf("the refusal does not carry the nested-repository reason and its remedy:\n%s", stderr.String())
			}
			if strings.Contains(stderr.String(), "backend=") {
				t.Errorf("a command plan was built for a refused command:\n%s", stderr.String())
			}
		})
	}
}

// The decision is the engine's, asked the way a session asks it. These are the
// controls: what must NOT be refused, so the gate above is not simply refusing
// git, or refusing this workspace.
func TestSandboxExecDecisionLeavesOrdinaryCommandsAlone(t *testing.T) {
	_, governed := governedNestedWorkspace(t)
	standalone := t.TempDir()

	for _, testCase := range []struct {
		name      string
		workspace string
		argv      []string
	}{
		{name: "an ordinary git command in the governed workspace", workspace: governed, argv: []string{"git", "status"}},
		{name: "the words git init as one argument", workspace: governed, argv: []string{"echo", "git init"}},
		{name: "git init in a workspace nobody governs", workspace: standalone, argv: []string{"git", "init"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			engine := sandbox.NewEngine(sandbox.EngineOptions{
				WorkspaceRoot: testCase.workspace,
				Policy:        sandbox.DefaultPolicy(),
				Backend:       sandbox.Backend{Name: sandbox.BackendUnavailable, Fallback: true},
			})
			decision := sandboxExecDecision(context.Background(), engine, testCase.workspace, testCase.argv)
			if decision.Action == sandbox.ActionDeny {
				t.Fatalf("%q was refused: %s", strings.Join(testCase.argv, " "), decision.ErrorString())
			}
		})
	}
}

// ARGV BECOMES COMMAND TEXT WITHOUT CHANGING WHAT IT SAYS. The engine classifies
// a shell command by parsing its text, so the rendering decides what the
// classifier sees. One argument has to stay one word, and a payload handed to a
// shell launcher has to arrive as that payload.
func TestSandboxExecCommandTextKeepsArgvIntact(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		argv    []string
		want    string
		gitInit bool
	}{
		{name: "bare words stay bare", argv: []string{"git", "init"}, want: "git init", gitInit: true},
		{name: "a spaced argument is one word", argv: []string{"echo", "git init"}, want: "echo 'git init'"},
		{name: "a launcher payload is analysed as a payload", argv: []string{"sh", "-c", "git init"}, want: "sh -c 'git init'", gitInit: true},
		{name: "a quote inside an argument", argv: []string{"printf", "it's"}, want: `printf 'it'\''s'`},
		{name: "substitution syntax is inert", argv: []string{"echo", "$(git init)"}, want: "echo '$(git init)'"},
		{name: "an empty argument survives", argv: []string{"printf", ""}, want: "printf ''"},
		{name: "a directory with a space", argv: []string{"git", "-C", "sub dir", "init"}, want: "git -C 'sub dir' init", gitInit: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			text := sandboxExecCommandText(testCase.argv)
			if text != testCase.want {
				t.Fatalf("command text = %q, want %q", text, testCase.want)
			}
			analysis := sandbox.AnalyzeCommand(text)
			if analysis.TooComplex {
				t.Fatalf("%q does not parse, so the classifier would treat a plain argv as obfuscated", text)
			}
			if analysis.GitInit != testCase.gitInit {
				t.Errorf("%q: GitInit = %v, want %v", text, analysis.GitInit, testCase.gitInit)
			}
		})
	}
}
