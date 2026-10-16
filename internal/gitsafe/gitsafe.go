// Package gitsafe builds the read-only git commands Zero runs on its own, as
// opposed to a git command the user or the model asked for: the status and diff
// calls behind the TUI's changed-files sweep, its pull request diffstat and the
// scratch-file warning, which run without anyone asking for git.
//
// A REPOSITORY CAN MAKE THOSE COMMANDS RUN A PROGRAM. git status and git diff read
// the repository's own .git/config, and several keys there name programs git then
// runs: core.fsmonitor on every status, a clean filter or diff driver assigned
// through .gitattributes whenever a file's content has to be read, and hooks such
// as post-index-change when the index is rewritten. A repository unpacked from an
// archive or handed over as a directory carries that config, and Zero would run it
// on the first turn, although it refuses the same workspace's project hooks and
// plugins until the user trusts it.
//
// The single-key cases are switched off on the command line. Filter and diff
// driver commands are named per driver, so they cannot be switched off without
// knowing the names; a repository whose own config defines any is skipped instead.
// Commands defined in the user's own global config, such as git-lfs, are the
// user's choice and are not affected.
package gitsafe

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// ErrRepositoryDefinesCommands is returned for a repository whose own config
// defines a filter or diff driver command. The caller goes without the git
// information rather than run it.
var ErrRepositoryDefinesCommands = errors.New("the repository's own git config defines a filter or diff command, so Zero does not run git there on its own")

// commandKeys matches the config keys whose value is a program git can run while
// computing status or a diff, and which are named per driver. Git prints section
// and variable names in lower case, so the patterns do not need to fold case.
const commandKeys = `filter\..+\.(clean|smudge|process)|diff\..+\.(command|textconv)|diff\.external`

// worktreeConfigKey makes git also read per-worktree config (config.worktree),
// which a --local read leaves out. Without it git ignores that file.
const worktreeConfigKey = "extensions.worktreeconfig"

const (
	localConfigPattern    = `^(` + commandKeys + `|extensions\.worktreeconfig)$`
	worktreeConfigPattern = `^(` + commandKeys + `)$`
)

// Command returns git with args, run in dir, with the programs a repository can
// configure turned off. It fails with ErrRepositoryDefinesCommands when the
// repository's own config defines a filter or diff driver command, and with git's
// own error when dir is not inside a repository.
//
// Callers that inspect a work tree that may contain submodules should also pass
// --ignore-submodules=dirty: a submodule has its own config, which this does not
// read, and git only runs commands there when it looks inside the submodule.
func Command(ctx context.Context, dir string, args ...string) (*exec.Cmd, error) {
	env := environment()
	defines, err := repositoryDefinesCommands(ctx, dir, env)
	if err != nil {
		return nil, err
	}
	if defines {
		return nil, ErrRepositoryDefinesCommands
	}
	command := exec.CommandContext(ctx, "git", append(disabledPrograms(), args...)...)
	command.Dir = dir
	// No opportunistic index refresh: a read-only call never rewrites the index,
	// so it never triggers the hooks that run when the index is written. Git
	// before 2.15 does not know the variable and ignores it.
	command.Env = append(env, "GIT_OPTIONAL_LOCKS=0")
	return command, nil
}

// disabledPrograms turns off the single-key ways a repository runs a program
// during status or diff. Git ignores a key it does not know, so an older git
// without hooksPath (2.9) or fsmonitor (2.16) accepts these unchanged.
func disabledPrograms() []string {
	return []string{"-c", "core.fsmonitor=false", "-c", "core.hooksPath=" + os.DevNull}
}

// repositoryLocationVariables make git use a given repository, work tree or
// index instead of discovering them from the working directory. Git exports
// them to hooks, so a Zero started from a hook inherits the hook's repository.
var repositoryLocationVariables = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR",
	"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE", "GIT_PREFIX",
}

// environment is Zero's environment without the repository-location variables,
// so git finds the repository from dir. The config check and the command share
// it: checking one repository's config and then running git in another would
// let the second one's commands through.
func environment() []string {
	parent := os.Environ()
	env := make([]string, 0, len(parent)+1)
	for _, entry := range parent {
		name, _, _ := strings.Cut(entry, "=")
		if !isRepositoryLocationVariable(name) {
			env = append(env, entry)
		}
	}
	return env
}

func isRepositoryLocationVariable(name string) bool {
	for _, variable := range repositoryLocationVariables {
		// Windows environment names are case-insensitive.
		if name == variable || (runtime.GOOS == "windows" && strings.EqualFold(name, variable)) {
			return true
		}
	}
	return false
}

// repositoryDefinesCommands reports whether the repository's own config, with
// includes followed, defines a filter or diff driver command. The local config is
// read once, together with the key that turns on per-worktree config, and that
// file is read only when it is on, so most repositories cost one git process.
// Reading config runs nothing.
func repositoryDefinesCommands(ctx context.Context, dir string, env []string) (bool, error) {
	entries, err := readConfig(ctx, dir, env, "--local", localConfigPattern)
	if err != nil {
		return false, err
	}
	worktreeConfig := false
	for _, entry := range entries {
		if entry.key != worktreeConfigKey {
			return true, nil
		}
		worktreeConfig = !configFalse(entry.value)
	}
	if !worktreeConfig {
		return false, nil
	}
	// The extension needs git 2.20, which also has --worktree, so a failure here
	// is real and the caller goes without git.
	entries, err = readConfig(ctx, dir, env, "--worktree", worktreeConfigPattern)
	if err != nil {
		return false, err
	}
	return len(entries) > 0, nil
}

// configFalse reports whether a config value is one of git's spellings of false.
// Anything else, including a key with no value, counts as on, so an unexpected
// spelling costs one more read rather than skipping the file.
func configFalse(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "false", "no", "off", "0":
		return true
	}
	return false
}

type configEntry struct {
	key   string
	value string
}

// readConfig returns the entries in one config scope whose key matches pattern.
// It is a variable so tests can answer for git.
var readConfig = func(ctx context.Context, dir string, env []string, scope string, pattern string) ([]configEntry, error) {
	command := exec.CommandContext(ctx, "git", "config", "-z", scope, "--includes", "--get-regexp", pattern)
	command.Dir = dir
	command.Env = env
	output, err := command.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return nil, nil // no key matched
		}
		return nil, err
	}
	return parseConfigEntries(output), nil
}

// parseConfigEntries splits git config -z output: each entry ends with a NUL,
// and a newline separates the key from its value when there is one.
func parseConfigEntries(output []byte) []configEntry {
	var entries []configEntry
	for _, record := range bytes.Split(output, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		key, value, _ := bytes.Cut(record, []byte{'\n'})
		entries = append(entries, configEntry{key: string(key), value: string(value)})
	}
	return entries
}
