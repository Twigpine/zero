package gitsafe

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

type configCall struct {
	scope string
	env   []string
}

// answerConfig stands in for git's config read and records each call.
func answerConfig(t *testing.T, answer func(scope string) ([]configEntry, error)) *[]configCall {
	t.Helper()
	var calls []configCall
	previous := readConfig
	readConfig = func(_ context.Context, _ string, env []string, scope string, _ string) ([]configEntry, error) {
		calls = append(calls, configCall{scope: scope, env: env})
		return answer(scope)
	}
	t.Cleanup(func() { readConfig = previous })
	return &calls
}

func noEntries(string) ([]configEntry, error) { return nil, nil }

func scopes(calls *[]configCall) []string {
	var read []string
	for _, call := range *calls {
		read = append(read, call.scope)
	}
	return read
}

// effectiveEnv returns the value a child process gets for name: os/exec keeps
// the last entry for a name, compared without case on Windows.
func effectiveEnv(env []string, name string) (string, bool) {
	value, found := "", false
	for _, entry := range env {
		key, rest, ok := strings.Cut(entry, "=")
		if ok && (key == name || (runtime.GOOS == "windows" && strings.EqualFold(key, name))) {
			value, found = rest, true
		}
	}
	return value, found
}

func TestCommandTurnsOffRepositoryPrograms(t *testing.T) {
	answerConfig(t, noEntries)
	t.Setenv("GIT_OPTIONAL_LOCKS", "1")
	dir := t.TempDir()

	command, err := Command(context.Background(), dir, "status", "--porcelain")
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	want := []string{"git", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=" + os.DevNull, "status", "--porcelain"}
	if !reflect.DeepEqual(command.Args, want) {
		t.Fatalf("args = %q, want %q", command.Args, want)
	}
	if command.Dir != dir {
		t.Fatalf("dir = %q, want %q", command.Dir, dir)
	}
	// An inherited GIT_OPTIONAL_LOCKS must not turn the index refresh back on.
	if value, _ := effectiveEnv(command.Env, "GIT_OPTIONAL_LOCKS"); value != "0" {
		t.Fatalf("effective GIT_OPTIONAL_LOCKS = %q, want 0", value)
	}
}

func TestCommandDropsRepositoryLocationVariables(t *testing.T) {
	// Turn per-worktree config on so both reads happen and both are checked.
	calls := answerConfig(t, func(scope string) ([]configEntry, error) {
		if scope == "--local" {
			return []configEntry{{key: worktreeConfigKey, value: "true"}}, nil
		}
		return nil, nil
	})
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	for _, name := range repositoryLocationVariables {
		t.Setenv(name, elsewhere)
	}
	t.Setenv("ZERO_GITSAFE_TEST_KEPT", "kept")

	command, err := Command(context.Background(), t.TempDir(), "status")
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	for _, name := range repositoryLocationVariables {
		if value, found := effectiveEnv(command.Env, name); found {
			t.Fatalf("%s=%q reached git", name, value)
		}
	}
	if value, _ := effectiveEnv(command.Env, "ZERO_GITSAFE_TEST_KEPT"); value != "kept" {
		t.Fatalf("an unrelated variable was dropped: %q", value)
	}
	// The config check has to look at the repository the command runs in, so it
	// gets the same environment, minus only the lock variable added afterwards.
	if got := scopes(calls); !reflect.DeepEqual(got, []string{"--local", "--worktree"}) {
		t.Fatalf("config reads = %q, want --local then --worktree", got)
	}
	for _, call := range *calls {
		if !reflect.DeepEqual(call.env, command.Env[:len(command.Env)-1]) {
			t.Fatalf("the %s config read ran with a different environment than the command", call.scope)
		}
	}
}

func TestLocationVariableNamesFoldCaseOnlyOnWindows(t *testing.T) {
	if !isRepositoryLocationVariable("GIT_DIR") {
		t.Fatal("GIT_DIR not recognised")
	}
	if got, want := isRepositoryLocationVariable("Git_Dir"), runtime.GOOS == "windows"; got != want {
		t.Fatalf("Git_Dir recognised = %v, want %v on %s", got, want, runtime.GOOS)
	}
	for _, name := range []string{"GIT_DIRECTORY", "MY_GIT_DIR", "GIT_CONFIG_GLOBAL", ""} {
		if isRepositoryLocationVariable(name) {
			t.Fatalf("%q treated as a repository-location variable", name)
		}
	}
}

func TestCommandRefusesRepositoryThatDefinesCommands(t *testing.T) {
	enabled := configEntry{key: worktreeConfigKey, value: "true"}
	command := configEntry{key: "filter.x.clean", value: "x"}
	cases := map[string]map[string][]configEntry{
		"local":                     {"--local": {command}},
		"local after the extension": {"--local": {enabled, command}},
		"per-worktree":              {"--local": {enabled}, "--worktree": {{key: "diff.x.textconv", value: "x"}}},
	}
	for name, answers := range cases {
		t.Run(name, func(t *testing.T) {
			answerConfig(t, func(scope string) ([]configEntry, error) { return answers[scope], nil })
			got, err := Command(context.Background(), t.TempDir(), "status")
			if !errors.Is(err, ErrRepositoryDefinesCommands) {
				t.Fatalf("err = %v, want ErrRepositoryDefinesCommands", err)
			}
			if got != nil {
				t.Fatal("a command came back for a repository that defines commands")
			}
		})
	}
}

func TestCommandReadsPerWorktreeConfigOnlyWhenItIsOn(t *testing.T) {
	cases := map[string]struct {
		local []configEntry
		want  []string
	}{
		"no extension":            {nil, []string{"--local"}},
		"extension off":           {[]configEntry{{key: worktreeConfigKey, value: "false"}}, []string{"--local"}},
		"extension off as no":     {[]configEntry{{key: worktreeConfigKey, value: "No"}}, []string{"--local"}},
		"extension on":            {[]configEntry{{key: worktreeConfigKey, value: "true"}}, []string{"--local", "--worktree"}},
		"extension with no value": {[]configEntry{{key: worktreeConfigKey}}, []string{"--local", "--worktree"}},
		"turned off again":        {[]configEntry{{key: worktreeConfigKey, value: "true"}, {key: worktreeConfigKey, value: "off"}}, []string{"--local"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			calls := answerConfig(t, func(scope string) ([]configEntry, error) {
				if scope == "--local" {
					return tc.local, nil
				}
				return nil, nil
			})
			if _, err := Command(context.Background(), t.TempDir(), "status"); err != nil {
				t.Fatalf("Command: %v", err)
			}
			if got := scopes(calls); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("config reads = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCommandFailsWhenConfigCannotBeRead(t *testing.T) {
	unreadable := errors.New("not a git repository")
	for _, failing := range []string{"--local", "--worktree"} {
		t.Run(failing, func(t *testing.T) {
			answerConfig(t, func(scope string) ([]configEntry, error) {
				if scope == failing {
					return nil, unreadable
				}
				return []configEntry{{key: worktreeConfigKey, value: "true"}}, nil
			})
			if _, err := Command(context.Background(), t.TempDir(), "status"); !errors.Is(err, unreadable) {
				t.Fatalf("err = %v, want the config read error", err)
			}
		})
	}
}

// Go's regexp stands in for git's extended regex here; the patterns use nothing
// the two treat differently.
func TestConfigPatterns(t *testing.T) {
	local := regexp.MustCompile(localConfigPattern)
	worktree := regexp.MustCompile(worktreeConfigPattern)
	for _, key := range []string{
		"filter.lfs.clean", "filter.lfs.smudge", "filter.lfs.process", "filter.a.b.clean",
		"diff.word.textconv", "diff.tool.command", "diff.external",
	} {
		if !local.MatchString(key) || !worktree.MatchString(key) {
			t.Errorf("%s is not matched in both scopes", key)
		}
	}
	if !local.MatchString(worktreeConfigKey) || worktree.MatchString(worktreeConfigKey) {
		t.Errorf("%s belongs to the local read only", worktreeConfigKey)
	}
	for _, key := range []string{
		"filter.lfs.required", "diff.word.cachetextconv", "diff.word.binary",
		"core.fsmonitor", "diff.external.x", "diff.renames", "extensions.worktreeconfigx",
	} {
		if local.MatchString(key) || worktree.MatchString(key) {
			t.Errorf("%s is matched", key)
		}
	}
}

func TestParseConfigEntries(t *testing.T) {
	got := parseConfigEntries([]byte("filter.x.clean\nrun this\nand that\x00extensions.worktreeconfig\x00"))
	want := []configEntry{
		{key: "filter.x.clean", value: "run this\nand that"},
		{key: "extensions.worktreeconfig"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entries = %+v, want %+v", got, want)
	}
}

// Real git: a fresh repository with nothing configured, the same repository with
// per-worktree config turned on, and a directory that is not a repository. This
// pins the output format and exit codes readConfig relies on; the command keys
// themselves are covered above.
func TestCommandAgainstRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repo := t.TempDir()
	outside := t.TempDir()
	// Both share a parent; stop git from finding an enclosing repository there.
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(repo))
	// Run as if from a hook in another repository. The setup below has to reach
	// repo, as Command does, and never the repository these name.
	decoy := t.TempDir()
	t.Setenv("GIT_DIR", filepath.Join(decoy, ".git"))
	t.Setenv("GIT_WORK_TREE", decoy)
	decoyUntouched := func(step string) {
		t.Helper()
		if _, err := os.Stat(filepath.Join(decoy, ".git")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s reached the repository GIT_DIR names instead of repo: %v", step, err)
		}
	}
	git := func(args ...string) {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", repo}, args...)...)
		command.Env = environment()
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	git("init", "-q")
	decoyUntouched("git init")
	gitRead := readConfig
	var read []string
	readConfig = func(ctx context.Context, dir string, env []string, scope string, pattern string) ([]configEntry, error) {
		read = append(read, scope)
		return gitRead(ctx, dir, env, scope, pattern)
	}
	t.Cleanup(func() { readConfig = gitRead })

	command, err := Command(context.Background(), repo, "status", "--porcelain")
	if err != nil {
		t.Fatalf("Command in a plain repository: %v", err)
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git status: %v: %s", err, output)
	}
	if !reflect.DeepEqual(read, []string{"--local"}) {
		t.Fatalf("config reads in a plain repository = %q, want --local only", read)
	}

	if !gitAtLeast(t, 2, 20) {
		t.Log("skipping the per-worktree case: it needs git 2.20 for extensions.worktreeConfig")
	} else {
		git("config", "extensions.worktreeConfig", "true")
		decoyUntouched("git config")
		read = nil
		if _, err := Command(context.Background(), repo, "status"); err != nil {
			t.Fatalf("Command with per-worktree config on: %v", err)
		}
		if !reflect.DeepEqual(read, []string{"--local", "--worktree"}) {
			t.Fatalf("config reads with per-worktree config on = %q, want --local then --worktree", read)
		}
	}

	if _, err := Command(context.Background(), outside, "status"); err == nil {
		t.Fatal("Command outside a repository succeeded")
	}
}

// gitAtLeast reports whether the git on PATH is at least major.minor.
func gitAtLeast(t *testing.T, major, minor int) bool {
	t.Helper()
	output, err := exec.Command("git", "version").Output()
	if err != nil {
		t.Fatalf("git version: %v", err)
	}
	var gotMajor, gotMinor int
	if _, err := fmt.Sscanf(strings.TrimPrefix(strings.TrimSpace(string(output)), "git version "), "%d.%d", &gotMajor, &gotMinor); err != nil {
		t.Fatalf("parse %q: %v", output, err)
	}
	return gotMajor > major || (gotMajor == major && gotMinor >= minor)
}
