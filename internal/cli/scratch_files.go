package cli

import (
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Twigpine/zero/internal/gitsafe"
)

type scratchFileBaseline map[string]bool

// scratchFileSnapshot records the untracked scratch-like files that already
// existed before a run starts. Completion warnings compare against this baseline
// so Zero only reports files newly left behind by the run, including files made
// by shell commands that bypass the file tools.
func scratchFileSnapshot(workspaceRoot string) scratchFileBaseline {
	untracked, ok := gitUntrackedFiles(workspaceRoot)
	if !ok {
		return nil
	}
	baseline := make(scratchFileBaseline, len(untracked))
	for _, path := range untracked {
		if scratchLikePath(path) {
			baseline[path] = true
		}
	}
	return baseline
}

// scratchFileWarning builds a completion-time warning listing scratch/debug-like
// files that became untracked during this run. It deliberately does not warn for
// every new untracked file: brand-new deliverables are a normal coding-task
// result, while names such as `_debug.py`, `_fix_test.py`, or `scratch.js` are
// the class of throwaway artifacts called out in issue #551.
//
// Returns "" when there is nothing to report: git is unavailable,
// workspaceRoot isn't a git repo, no baseline was captured, or no new
// scratch-like untracked files remain.
func scratchFileWarning(workspaceRoot string, baseline scratchFileBaseline) string {
	if baseline == nil {
		return ""
	}
	untracked, ok := gitUntrackedFiles(workspaceRoot)
	if !ok {
		return ""
	}

	var scratchFiles []string
	for _, path := range untracked {
		if baseline[path] || !scratchLikePath(path) {
			continue
		}
		baseline[path] = true
		scratchFiles = append(scratchFiles, path)
	}
	if len(scratchFiles) == 0 {
		return ""
	}

	displayPaths := make([]string, len(scratchFiles))
	for i, path := range scratchFiles {
		displayPaths[i] = filepath.ToSlash(path)
	}
	plural := "s"
	if len(displayPaths) == 1 {
		plural = ""
	}
	return "This run left " + strconv.Itoa(len(displayPaths)) + " new scratch/debug-like file" + plural + " untracked in git: " +
		strings.Join(displayPaths, ", ") + ". Review before committing/staging."
}

func gitUntrackedFiles(workspaceRoot string) ([]string, bool) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// gitsafe keeps the workspace's own git config from running a program, and
	// --ignore-submodules=dirty keeps git out of submodule work trees.
	command, err := gitsafe.Command(ctx, workspaceRoot, "status", "--porcelain", "-z", "--untracked-files=all", "--ignore-submodules=dirty")
	if err != nil {
		return nil, false
	}
	output, err := command.Output()
	if err != nil {
		// Not a git repo (or git failed for some other reason) — nothing
		// reliable to report against, so stay silent rather than guess.
		return nil, false
	}

	var untracked []string
	for _, entry := range strings.Split(string(output), "\x00") {
		if !strings.HasPrefix(entry, "?? ") {
			continue
		}
		relative := strings.TrimPrefix(entry, "?? ")
		untracked = append(untracked, relative)
	}
	return untracked, true
}

func scratchLikePath(path string) bool {
	name := strings.ToLower(filepath.Base(filepath.ToSlash(path)))
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	if strings.HasPrefix(name, "scratch") || strings.HasPrefix(name, "debug") || strings.HasPrefix(name, "tmp") || strings.HasPrefix(name, "temp") || strings.HasPrefix(name, "repro") {
		return true
	}
	for _, marker := range []string{"scratch", "debug", "tmp", "temp", "repro", "fix_test"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}
