package specialist

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// A plan that isolated into a tree its own tasks cannot read.
//
// THE MEASURED FAILURE. A six-task research plan was started from a directory
// that happened to be a git repo with ONE tracked file. Five tasks asked for
// read_file/grep/glob; the sixth also asked for exec_command, which is not in
// planReadOnlyTools — so RequiresIsolation flipped for the WHOLE plan and every
// task was moved into a worktree containing that one file. The tasks had been
// pointed at absolute paths in a different tree, which was now outside their
// sandbox, and three of them died reporting "I cannot". About 143,000 tokens,
// and the sixth task succeeded only because asking for exec_command had raised
// its autonomy far enough to read back out.
//
// Isolation itself is right: write tasks belong in a worktree. What was missing
// is that nothing checked the worktree could serve the plan before dispatching
// six tasks into it. resolvePlanWorkspace already refuses when isolation is
// UNAVAILABLE; this refuses when it is available and useless.
//
// FAILS TOWARD RUNNING THE PLAN. A false refusal blocks work that would have
// succeeded, so this only fires on evidence that cannot be a guess: an absolute
// path, written in a task's own prompt, that EXISTS on disk, and that is outside
// the workspace the task will run in. All three, or nothing happens. A path that
// does not exist is somebody else's error; a relative path resolves against the
// workspace and is not this function's business; a plan that never isolated
// keeps the parent's tree and cannot have this problem at all.

// absolutePathPattern finds absolute paths in prose — POSIX (/a/b) and Windows
// (C:\a\b, C:/a/b) forms, with either separator throughout, because a task
// prompt written on Windows names Windows paths and a POSIX-only pattern found
// none of them: the reach check silently never fired there. The character class
// includes '~' for Windows 8.3 short names (RUNNER~1), which real CI paths
// contain.
//
// Deliberately loose, because the EXISTENCE check below is what makes a match
// meaningful. It will happily match "/settings" out of a URL and "/v1/pay" out
// of a sentence; neither exists on disk, so neither survives — and a
// backslash-shaped candidate on POSIX fails the same Lstat.
var absolutePathPattern = regexp.MustCompile(`(?:[A-Za-z]:)?[/\\][A-Za-z0-9._+@~-]+(?:[/\\][A-Za-z0-9._+@~-]+)+`)

// unreachablePlanPaths lists paths the plan's own prompts name that the plan's
// workspace cannot reach. Empty for every plan that is fine — including every
// plan that did not isolate.
func unreachablePlanPaths(plan Plan, workspace PlanWorkspace) []string {
	if !workspace.Isolated || strings.TrimSpace(workspace.Path) == "" {
		return nil
	}
	root := resolvedPath(workspace.Path)
	seen := map[string]bool{}
	var unreachable []string
	for _, task := range plan.Tasks() {
		for _, candidate := range absolutePathPattern.FindAllString(task.Prompt, -1) {
			path, ok := existingPath(candidate)
			if !ok || seen[path] {
				continue
			}
			seen[path] = true
			if !pathInsideRoot(root, resolvedPath(path)) {
				unreachable = append(unreachable, path)
			}
		}
	}
	sort.Strings(unreachable)
	return unreachable
}

// existingPath reports the path if something is really there.
//
// Trailing punctuation is retried once, because a path at the end of a sentence
// arrives as "…/plan.go." and the bare Stat would miss it — a false NEGATIVE,
// which is the safe direction but a cheap one to reduce.
func existingPath(candidate string) (string, bool) {
	trimmed := strings.TrimRight(candidate, ".,;:)]}\"'")
	if _, err := os.Lstat(candidate); err == nil {
		// AN EXACT HIT CAN BE A WINDOWS QUIRK, not a real dotted filename: NTFS
		// ignores trailing dots, so Lstat("…/streamer.go.") succeeds for the
		// file named "…/streamer.go" and the sentence's full stop came back as
		// part of the path. When the trimmed spelling names the SAME file,
		// prefer it — it is the canonical name on both platforms. A POSIX file
		// genuinely named with a trailing dot keeps its exact spelling: its
		// trimmed sibling either does not exist or is a different file.
		if trimmed != candidate && trimmed != "" && sameLstatFile(candidate, trimmed) {
			return trimmed, true
		}
		return candidate, true
	}
	if trimmed == candidate || trimmed == "" {
		return "", false
	}
	if _, err := os.Lstat(trimmed); err == nil {
		return trimmed, true
	}
	return "", false
}

// sameLstatFile reports whether two paths name the same file, false on any
// error — an unanswerable comparison must not rewrite a spelling.
func sameLstatFile(a, b string) bool {
	infoA, err := os.Lstat(a)
	if err != nil {
		return false
	}
	infoB, err := os.Lstat(b)
	if err != nil {
		return false
	}
	return os.SameFile(infoA, infoB)
}

// resolvedPath follows symlinks so /var and /private/var are the same place.
// Falls back to a lexical clean when it cannot resolve, which is the honest
// answer for a path that exists but cannot be walked.
func resolvedPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}

// pathInsideRoot reports containment, SEPARATOR-AWARE: "/a/b" does not contain
// "/a/bc", which a bare strings.HasPrefix would get wrong.
//
// BOTH separators are boundaries on every platform. Keying on
// filepath.Separator alone made the check answer false for every /-separated
// pair on Windows — the whole reach guard quietly never contained anything —
// and prompts freely mix the two spellings of the same path.
func pathInsideRoot(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	if path == root {
		return true
	}
	root = strings.TrimRight(root, `/\`)
	if root == "" || !strings.HasPrefix(path, root) || len(path) <= len(root) {
		return false
	}
	next := path[len(root)]
	return next == '/' || next == '\\'
}

// THE SECOND SHAPE OF THE SAME FAILURE, and the one the absolute-path check
// above misses entirely.
//
// A later run of the same plan named its targets RELATIVELY — "internal/
// specialist/", "internal/swarm/" — plus a commit sha. Not one absolute path
// anywhere, so unreachablePlanPaths found nothing and six tasks were dispatched
// into a worktree holding a single file. The child said it plainly: "no Go files
// anywhere in the workspace… the only reachable commit is 3177684c".
//
// A relative path is far weaker evidence than an absolute one: prose is full of
// slash-shaped things, and a path that does not exist yet is exactly what a
// scaffolding task is for. So this fires only on a CONJUNCTION that a working
// plan cannot satisfy:
//
//   - the plan isolated, and
//   - it names at least two path-shaped tokens, and
//   - NOT ONE of them exists in the workspace, and
//   - the workspace is bare.
//
// A plan genuinely about its own tree resolves at least one token. A scaffolding
// plan creating new files runs in a worktree of a real repo, which is not bare.
// Both conditions have to be wrong at once, which is what the measured failure
// looked like and what an ordinary run does not.

// relativePathPattern matches path-shaped tokens.
//
// DELIBERATELY NARROWER THAN "has a slash", because "and/or", "read/write" and
// "input/output" are not paths and appear in prompts constantly. A token
// qualifies only if it ends in a slash, carries a file extension, or has three
// or more segments — which those three do not.
var relativePathPattern = regexp.MustCompile(`\b[A-Za-z0-9._-]+/(?:[A-Za-z0-9._-]+/)*[A-Za-z0-9._-]*`)

// urlPattern strips URLs before scanning, so "https://ollama.com/settings" does
// not contribute "ollama.com/settings" as a path.
var urlPattern = regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9+.-]*://\S+`)

// bareWorkspaceMaxEntries is how few visible entries make a workspace "bare".
// The measured failure had exactly one — gauntlet_test.txt. A checkout a plan
// could actually work in has many more, so this does not need to be near the
// boundary and deliberately is not.
const bareWorkspaceMaxEntries = 2

// planNamesNothingPresent reports the tokens a plan names when NONE of them are
// in the workspace and the workspace is bare.
func planNamesNothingPresent(plan Plan, workspace PlanWorkspace) ([]string, bool) {
	if !workspace.Isolated || strings.TrimSpace(workspace.Path) == "" {
		return nil, false
	}
	if !workspaceIsBare(workspace.Path) {
		return nil, false
	}
	seen := map[string]bool{}
	var named []string
	for _, task := range plan.Tasks() {
		text := urlPattern.ReplaceAllString(task.Prompt, " ")
		for _, token := range relativePathPattern.FindAllString(text, -1) {
			token = strings.TrimRight(token, ".,;:)]}\"'")
			if !pathShapedToken(token) || seen[token] {
				continue
			}
			seen[token] = true
			// ANY hit ends it: the workspace is serving the plan after all, and
			// bare or not, this is not the failure being detected.
			if _, err := os.Lstat(filepath.Join(workspace.Path, filepath.FromSlash(token))); err == nil {
				return nil, false
			}
			named = append(named, token)
		}
	}
	if len(named) < 2 {
		return nil, false
	}
	sort.Strings(named)
	return named, true
}

// pathShapedToken keeps only tokens that read as file paths rather than as
// ordinary prose containing a slash.
func pathShapedToken(token string) bool {
	if token == "" || strings.HasPrefix(token, "/") {
		return false
	}
	// A SLASH IS REQUIRED HERE, not merely guaranteed by the caller. The pattern
	// that feeds this always includes one, so leaning on that would work — and
	// would leave a helper that answers "yes" for "main.go", correct only by
	// accident and wrong the moment anything else calls it.
	if !strings.Contains(token, "/") {
		return false
	}
	if strings.HasSuffix(token, "/") {
		return true
	}
	segments := strings.Split(token, "/")
	if len(segments) >= 3 {
		return true
	}
	return strings.Contains(segments[len(segments)-1], ".")
}

// workspaceIsBare reports whether the workspace holds almost nothing. Dot
// entries are skipped: a worktree always has .git, and counting it would make
// every bare tree look occupied by one.
func workspaceIsBare(root string) bool {
	entries, err := os.ReadDir(root)
	if err != nil {
		// UNREADABLE IS NOT BARE. Refusing a plan because a directory could not
		// be listed would turn a transient filesystem error into a refusal.
		return false
	}
	visible := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		visible++
		if visible > bareWorkspaceMaxEntries {
			return false
		}
	}
	return true
}

// refuseBareWorkspace is the relative-path refusal, and it leads with the fact
// that decides it: the workspace is empty of everything the plan mentions.
func refuseBareWorkspace(plan Plan, workspace PlanWorkspace, named []string) error {
	shown := named
	const maxShown = 3
	more := ""
	if len(shown) > maxShown {
		more = fmt.Sprintf(" (and %d more)", len(shown)-maxShown)
		shown = shown[:maxShown]
	}
	because := ""
	if taskID, tool := planIsolationTrigger(plan); taskID != "" {
		because = fmt.Sprintf(" The plan runs in a worktree because task %q asks for %q, which is not a read-only tool — and isolation applies to every task, not just that one.", taskID, tool)
	}
	return fmt.Errorf(
		"%s would run in an isolated worktree at %s that contains none of what its tasks name — not %s — and holds almost nothing at all. "+
			"Every task would search an empty tree.%s "+
			"Either start the run from the tree those paths are in, or remove the write tool so the plan stays read-only and runs where its parent does",
		describePlan(plan), workspace.Path, strings.Join(shown, ", ")+more, because)
}

// planIsolationTrigger names the first task and tool that made the plan need a
// worktree, so the refusal can say WHY it isolated rather than leaving the
// author to work it out. Isolation is a plan-level property and one task's
// grant decides it for all of them, which is exactly the surprising part.
func planIsolationTrigger(plan Plan) (taskID, tool string) {
	for _, task := range plan.Tasks() {
		for _, name := range task.Tools {
			if !planReadOnlyTools[name] {
				return task.ID, name
			}
		}
	}
	return "", ""
}

// refuseUnreachableWorkspace turns the finding into the error a caller sees.
//
// IT NAMES BOTH FIXES, because the author cannot be expected to know that one
// task's tool grant moved every other task's workspace: run from the tree the
// paths are in, or drop the tool that forced isolation and let the plan stay
// where its parent is.
func refuseUnreachableWorkspace(plan Plan, workspace PlanWorkspace, unreachable []string) error {
	shown := unreachable
	const maxShown = 3
	more := ""
	if len(shown) > maxShown {
		more = fmt.Sprintf(" (and %d more)", len(shown)-maxShown)
		shown = shown[:maxShown]
	}
	because := ""
	if taskID, tool := planIsolationTrigger(plan); taskID != "" {
		because = fmt.Sprintf(" The plan runs in a worktree because task %q asks for %q, which is not a read-only tool — and isolation applies to every task, not just that one.", taskID, tool)
	}
	return fmt.Errorf(
		"%s would run in an isolated worktree at %s, and its tasks name %s that the worktree does not contain: %s%s.%s "+
			"Every task reading those paths would be refused by the sandbox. "+
			"Either start the run from the tree those paths are in, or remove the write tool so the plan stays read-only and runs where its parent does",
		describePlan(plan), workspace.Path, pluralPaths(len(unreachable)), strings.Join(shown, ", "), more, because)
}

func pluralPaths(n int) string {
	if n == 1 {
		return "a path"
	}
	return "paths"
}

// describePlan names a plan for an error message.
//
// `name` is OPTIONAL on the orchestrate tool, so Plan.Name() is legitimately
// empty — and a refusal that interpolated it read `plan "" would run in an
// isolated worktree`, which names nothing and reads like a fault in the reader's
// own plan. An unnamed plan is described by what it is instead.
func describePlan(plan Plan) string {
	if name := strings.TrimSpace(plan.Name()); name != "" {
		return fmt.Sprintf("plan %q", name)
	}
	return fmt.Sprintf("this unnamed %d-task plan", plan.TaskCount())
}
