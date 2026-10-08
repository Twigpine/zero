package worktrees

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type GitRunner func(context.Context, string, ...string) (CommandResult, error)

type CommandResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

type Options struct {
	Cwd     string
	Name    string
	BaseDir string
	Env     map[string]string
	Now     func() time.Time
	RunGit  GitRunner
	// LeasePID, when positive, records the owning process in the worktree
	// lock reason. A lease carrying a PID is recoverable: if that process
	// dies without releasing (SIGKILL, crash, power loss), Clean can expire
	// the lease instead of skipping the locked worktree forever. Callers
	// whose worktree outlives their process (zero worktrees prepare hands
	// the path to an external owner) leave it zero for a persistent lock
	// that only an explicit release clears.
	LeasePID int
}

type Result struct {
	Name         string `json:"name"`
	Path         string `json:"path"`
	RepoRoot     string `json:"repoRoot"`
	SourceBranch string `json:"sourceBranch,omitempty"`
	SourceCommit string `json:"sourceCommit,omitempty"`
	Reused       bool   `json:"reused"`
	// LockAcquired reports whether this Prepare call took the worktree lock.
	// It is false when a reused worktree was already locked by another live
	// caller; releasing that lease is that caller's responsibility, not ours.
	LockAcquired bool `json:"lockAcquired"`
}

var worktreeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,80}$`)

func Prepare(ctx context.Context, options Options) (Result, error) {
	cwd, err := resolveCwd(options.Cwd)
	if err != nil {
		return Result{}, err
	}
	runGit := options.RunGit
	if runGit == nil {
		runGit = defaultRunGit
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	name := strings.TrimSpace(options.Name)
	if name == "" {
		name = defaultWorktreeName(now())
	}
	if err := validateName(name); err != nil {
		return Result{}, err
	}

	// Clean up stale worktrees (older than 24 hours) automatically to prevent
	// disk space leaks. Only after the request itself validated: a rejected
	// command (for example an invalid --name) must not have destructive
	// cleanup side effects before reporting its error.
	if options.RunGit == nil {
		_ = Clean(ctx, options, 24*time.Hour)
	}

	repoRoot, err := gitOutput(ctx, runGit, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return Result{}, fmt.Errorf("not a git repository: %w", err)
	}
	repoRoot = filepath.Clean(repoRoot)
	// Key the per-repository worktree bucket off the MAIN worktree's root, not
	// repoRoot (which is whichever worktree - main or a linked one - this call
	// happened to run from): git worktree list --porcelain always reports the
	// main worktree first, regardless of invocation location, so this keeps
	// Prepare and Release's ownership key derivation in agreement (see
	// verifyZeroOwnedWorktree) even when Prepare runs from a linked worktree.
	// Without it, a worktree prepared from a linked checkout hashes a
	// different repoKey than Release computes, and its lease can never be
	// cleared.
	primaryRoot, err := primaryWorktreeRoot(ctx, runGit, repoRoot)
	if err != nil {
		return Result{}, err
	}
	branch, _ := gitOutput(ctx, runGit, repoRoot, "rev-parse", "--abbrev-ref", "HEAD")
	commit, _ := gitOutput(ctx, runGit, repoRoot, "rev-parse", "--short", "HEAD")

	baseDir := strings.TrimSpace(options.BaseDir)
	if baseDir == "" {
		baseDir, err = DefaultBaseDir(options.Env)
		if err != nil {
			return Result{}, err
		}
	}
	baseDir, err = filepath.Abs(baseDir)
	if err != nil {
		return Result{}, fmt.Errorf("resolve worktree dir: %w", err)
	}

	repoDir := filepath.Join(baseDir, "zero-worktree-"+repoKey(primaryRoot))
	target := filepath.Join(repoDir, name)
	result := Result{
		Name:         name,
		Path:         target,
		RepoRoot:     primaryRoot,
		SourceBranch: branch,
		SourceCommit: commit,
	}
	reused, err := inspectTarget(target)
	if err != nil {
		return Result{}, err
	}
	if reused {
		sameRepo, err := sameGitCommonDir(ctx, runGit, repoRoot, target)
		if err != nil {
			return Result{}, fmt.Errorf("inspect existing worktree repository: %w", err)
		}
		if !sameRepo {
			return Result{}, fmt.Errorf("worktree path already exists for a different git repository: %s", target)
		}
		result.Reused = true
		// A reused worktree may have been released by a prior run's exit, and
		// an unlocked target is exposed to Clean's staleness heuristic while
		// this caller is still using it, so re-establish the lease here. A
		// lock already held by a live owner means another run is still using
		// the path: two live runs must not share one supposedly isolated
		// checkout, so reject it. A Zero lease whose recorded PID is dead
		// (SIGKILL, crash) is reclaimable the same way Clean recovers it —
		// otherwise a crashed `zero exec --worktree --worktree-name X` bricks
		// that name until someone runs release by hand.
		acquired, err := lockWorktree(ctx, runGit, repoRoot, target, options.LeasePID)
		if err != nil {
			return Result{}, err
		}
		if !acquired {
			reclaimed, reclaimErr := reclaimDeadOwnerLease(ctx, runGit, repoRoot, target)
			if reclaimErr != nil {
				return Result{}, reclaimErr
			}
			if reclaimed {
				acquired, err = lockWorktree(ctx, runGit, repoRoot, target, options.LeasePID)
				if err != nil {
					return Result{}, err
				}
			}
		}
		if !acquired {
			return Result{}, fmt.Errorf("worktree %s is locked by another active run; release it with `zero worktrees release %s` if that run is finished, or use a different --name", target, target)
		}
		result.LockAcquired = true
		// Touch the worktree directory so Clean's mtime-based staleness check
		// doesn't prune a worktree that's actively in use by the current process.
		// Long-running tasks that don't write new files (e.g. waiting on a model)
		// would otherwise accumulate stale mtimes and be wrongly force-removed.
		if err := os.Chtimes(target, time.Now(), time.Now()); err != nil {
			_, unlockErr := gitOutput(ctx, runGit, repoRoot, "worktree", "unlock", target)
			return Result{}, errors.Join(err, unlockErr)
		}
		if err := writeOwnershipMarker(ctx, runGit, target); err != nil {
			_, unlockErr := gitOutput(ctx, runGit, repoRoot, "worktree", "unlock", target)
			return Result{}, errors.Join(err, unlockErr)
		}
		return result, nil
	}
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		return Result{}, fmt.Errorf("create worktree directory: %w", err)
	}
	commandResult, err := runGit(ctx, repoRoot, "worktree", "add", "--detach", target, "HEAD")
	if err != nil {
		return Result{}, fmt.Errorf("create git worktree: %w", err)
	}
	if commandResult.ExitCode != 0 {
		message := strings.TrimSpace(firstNonEmpty(commandResult.Stderr, commandResult.Stdout))
		if message == "" {
			message = fmt.Sprintf("git worktree add exited with code %d", commandResult.ExitCode)
		}
		return Result{}, fmt.Errorf("create git worktree: %s", message)
	}
	// Lock the worktree so Clean's mtime+dirty staleness heuristic (which
	// cannot tell "abandoned" apart from "clean but waiting on a model,
	// network, or user for a long time") never force-removes a worktree zero
	// itself is still using. entry.locked already makes Clean skip a worktree
	// a human locked by hand; locking here extends that same protection to
	// the worktrees zero creates. An "already locked" answer on a worktree
	// this call just created means another process raced us to it: treat that
	// exactly like the reuse collision above rather than claiming a lease
	// this call never acquired.
	acquired, err := lockWorktree(ctx, runGit, repoRoot, target, options.LeasePID)
	if err != nil {
		// This call created target and nothing else can own it yet (a
		// concurrent racer would have made lockWorktree return acquired=false,
		// not an error), so a failure here (not "already locked", an actual
		// git failure) must not leave an unleased, newly created worktree
		// behind: Clean cannot distinguish it from live-but-not-yet-touched
		// work, so it would sit as a disk leak until the staleness window
		// passes. Best-effort: report a removal failure alongside the
		// original lock error rather than letting it mask the leak.
		// gitOutput (not a raw runGit call) so a nonzero exit code is treated
		// as a failure: defaultRunGit returns a nil error alongside a nonzero
		// CommandResult.ExitCode for a failed git invocation.
		if _, removeErr := gitOutput(ctx, runGit, repoRoot, "worktree", "remove", "--force", target); removeErr != nil {
			return Result{}, errors.Join(err, fmt.Errorf("clean up worktree after failed lock: %w", removeErr))
		}
		return Result{}, err
	}
	if !acquired {
		return Result{}, fmt.Errorf("worktree %s is locked by another active run; release it with `zero worktrees release %s` if that run is finished, or use a different --name", target, target)
	}
	result.LockAcquired = true
	if err := writeOwnershipMarker(ctx, runGit, target); err != nil {
		_, unlockErr := gitOutput(ctx, runGit, repoRoot, "worktree", "unlock", target)
		_, removeErr := gitOutput(ctx, runGit, repoRoot, "worktree", "remove", "--force", target)
		return Result{}, errors.Join(err, unlockErr, removeErr)
	}
	return result, nil
}

// leaseReasonPrefix marks a lock Zero itself created (vs a human `git
// worktree lock`); a "(pid N)" suffix makes the lease recoverable by Clean
// once the owning process is gone.
const leaseReasonPrefix = "zero: active task worktree"

// leaseReason renders the lock reason for a Zero lease, embedding the owning
// PID when the caller's worktree lifetime is bound to its process.
func leaseReason(pid int) string {
	if pid > 0 {
		return fmt.Sprintf("%s (pid %d)", leaseReasonPrefix, pid)
	}
	return leaseReasonPrefix
}

// leasePID extracts the owning PID from a Zero lease reason. ok=false for
// human locks and PID-less Zero leases (external prepare callers), which
// only an explicit release may clear.
func leasePID(reason string) (int, bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(reason), leaseReasonPrefix+" (pid ")
	if !found {
		return 0, false
	}
	digits, found := strings.CutSuffix(rest, ")")
	if !found {
		return 0, false
	}
	pid, err := strconv.Atoi(digits)
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// processAlive reports whether pid is a live process. It fails closed: any
// ambiguity (permission denied, platform limits) counts as alive, so an
// uncertain answer can only keep a lease, never expire one. Detection is
// platform-specific (see osProcessAlive in worktrees_posix.go /
// worktrees_windows.go): os.FindProcess alone cannot tell a dead PID from a
// live one on Windows, since OpenProcess can still succeed for a process that
// has already exited but whose handle has not been fully released.
func processAlive(pid int) bool {
	if pid <= 0 {
		return true
	}
	return osProcessAlive(pid)
}

// lockWorktree takes the Clean-protection lease on target via `git worktree
// lock`. It reports whether this call acquired the lease: a lock already held
// by someone else is left in place and reported as not acquired, so the
// caller knows the matching Release belongs to the lease's original owner.
func lockWorktree(ctx context.Context, runGit GitRunner, repoRoot string, target string, pid int) (bool, error) {
	lockResult, err := runGit(ctx, repoRoot, "worktree", "lock", "--reason", leaseReason(pid), target)
	if err != nil {
		return false, fmt.Errorf("lock git worktree: %w", err)
	}
	if lockResult.ExitCode == 0 {
		return true, nil
	}
	message := strings.TrimSpace(firstNonEmpty(lockResult.Stderr, lockResult.Stdout))
	if strings.Contains(message, "already locked") {
		return false, nil
	}
	if message == "" {
		message = fmt.Sprintf("git worktree lock exited with code %d", lockResult.ExitCode)
	}
	return false, fmt.Errorf("lock git worktree: %s", message)
}

// reclaimDeadOwnerLease unlocks target when its porcelain entry is a Zero
// lease whose recorded PID is provably dead. Live owners, human locks, and
// PID-less Zero leases are left alone. Returns true only when an unlock ran
// successfully so the caller can retry lockWorktree.
func reclaimDeadOwnerLease(ctx context.Context, runGit GitRunner, repoRoot string, target string) (bool, error) {
	output, err := gitOutput(ctx, runGit, repoRoot, "worktree", "list", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("list git worktrees for lease reclaim: %w", err)
	}
	want := canonicalizePath(target)
	for _, entry := range parseWorktreeList(output) {
		if canonicalizePath(entry.path) != want {
			continue
		}
		if !entry.locked {
			return false, nil
		}
		pid, ok := leasePID(entry.lockReason)
		if !ok || processAlive(pid) {
			return false, nil
		}
		if _, err := gitOutput(ctx, runGit, repoRoot, "worktree", "unlock", entry.path); err != nil {
			return false, fmt.Errorf("reclaim dead-owner lease on %s: %w", entry.path, err)
		}
		return true, nil
	}
	return false, nil
}

// Release unlocks a worktree that Prepare locked, via `git worktree unlock`,
// making it eligible for Clean's staleness check again. Zero itself only
// knows a worktree's use is over when its own process created it and is now
// exiting (zero exec --worktree calls this via defer); zero worktrees
// prepare hands the path to an external caller with no defined end-of-life,
// so that caller must run `zero worktrees release <path>` itself once done.
// Until it does, the worktree stays locked and Clean will never touch it,
// which is the safe default over silently guessing at liveness from mtimes.
func Release(ctx context.Context, options Options, path string) error {
	runGit := options.RunGit
	if runGit == nil {
		runGit = defaultRunGit
	}
	// git needs a working directory that still exists to run in. path itself
	// normally works (it's the worktree being unlocked), but if a caller
	// manually deleted a locked worktree directory instead of releasing it
	// first, path is gone; fall back to options.Cwd (the main repo) so the
	// leaked, now-orphaned lock can still be unlocked and later pruned.
	dir := path
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if cwd, cwdErr := resolveCwd(options.Cwd); cwdErr == nil {
			dir = cwd
		}
	}
	matchedPath, err := verifyZeroOwnedWorktree(ctx, runGit, dir, path)
	if err != nil {
		if errors.Is(err, errAlreadyUnlocked) {
			return nil
		}
		return err
	}
	if _, err := gitOutput(ctx, runGit, dir, "worktree", "unlock", matchedPath); err != nil {
		return fmt.Errorf("unlock git worktree: %w", err)
	}
	return nil
}

// zeroOwnerMarkerFile is the name of the ownership marker Prepare writes into
// a worktree's own private git admin directory (`git rev-parse
// --absolute-git-dir`), never into the working tree itself: the working tree
// is what `git status` inspects for staleness/dirtiness, and a marker living
// there would either show up as untracked noise (defeating Clean's dirty
// check) or need a .gitignore entry this package has no business adding to a
// caller's repository. The admin directory survives even after the worktree
// directory itself is deleted by hand (git only forgets it on `worktree
// prune`), and is not something `git worktree add` populates on its own, so
// its presence is what actually proves Zero's own Prepare created a given
// worktree - unlike the public zero-worktree-<repoKey> path convention and
// the leaseReasonPrefix lock-reason string, both of which a user can
// reproduce by hand for a worktree of the same repository.
const zeroOwnerMarkerFile = "zero-owner"

// zeroOwnerMarkerContent is the marker's fixed body. Its value carries no
// meaning beyond "Prepare wrote this"; the file's mere presence at the
// expected location is the signal Release and Clean check.
const zeroOwnerMarkerContent = "zero: this worktree was created by `zero worktrees prepare`\n"

// writeOwnershipMarker persists the ownership marker for target, a worktree
// path that must still exist on disk (Prepare calls this immediately after
// creating or re-locking it). Overwriting an existing marker is harmless and
// lets a worktree Prepare created before this marker existed self-heal the
// next time Prepare reuses it.
func writeOwnershipMarker(ctx context.Context, runGit GitRunner, target string) error {
	gitDir, err := gitOutput(ctx, runGit, target, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return fmt.Errorf("resolve worktree git dir: %w", err)
	}
	// Atomic replace: Clean may read the same path from another process while
	// Prepare re-marks a reused worktree. Write-then-rename so a concurrent
	// reader never observes a truncated file.
	destination := filepath.Join(gitDir, zeroOwnerMarkerFile)
	temporary, err := os.CreateTemp(gitDir, zeroOwnerMarkerFile+".tmp-*")
	if err != nil {
		return fmt.Errorf("write worktree ownership marker: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write worktree ownership marker: %w", err)
	}
	if _, err := temporary.WriteString(zeroOwnerMarkerContent); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write worktree ownership marker: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("write worktree ownership marker: %w", err)
	}
	if err := os.Rename(temporaryName, destination); err != nil {
		return fmt.Errorf("write worktree ownership marker: %w", err)
	}
	return nil
}

// hasOwnershipMarker reports whether target's own git admin directory carries
// the marker writeOwnershipMarker persists. It fails closed: any error other
// than the marker simply not existing is returned rather than treated as
// "not owned so it's fine to skip," since callers otherwise use false to mean
// "safe to leave alone."
func hasOwnershipMarker(ctx context.Context, runGit GitRunner, target string) (bool, error) {
	gitDir, err := gitOutput(ctx, runGit, target, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return false, fmt.Errorf("resolve worktree git dir: %w", err)
	}
	content, err := os.ReadFile(filepath.Join(gitDir, zeroOwnerMarkerFile))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("read worktree ownership marker: %w", err)
	}
	return string(content) == zeroOwnerMarkerContent, nil
}

// isLegacyZeroWorktree verifies whether a worktree lacking zero-owner was
// created by a pre-upgrade version of Zero for this repository.
func isLegacyZeroWorktree(ctx context.Context, runGit GitRunner, target string, repoDir string, entry worktreeEntry) bool {
	if !isUnderDir(canonicalizePath(target), repoDir) {
		return false
	}
	if entry.locked {
		if !strings.HasPrefix(strings.TrimSpace(entry.lockReason), leaseReasonPrefix) {
			return false
		}
	}
	gitDir, err := gitOutput(ctx, runGit, target, "rev-parse", "--absolute-git-dir")
	if err != nil || strings.TrimSpace(gitDir) == "" {
		return false
	}
	return true
}

// verifyZeroOwnedWorktree confirms path has a zero-worktree-<repoKey> ancestor
// directory component, is a registered worktree of the repository, and (when
// locked) carries a lock reason Zero itself set, so Release cannot be used to
// clear the lock on a worktree a user (or another tool) manages by hand: the
// command is documented as releasing a worktree `prepare` created, not an
// arbitrary git worktree lock. The ancestor-component check stands in for
// reconstructing and comparing a full repoDir, because Release has no
// reliable way to know which --dir a long-gone Prepare call used (the CLI
// never threads BaseDir through to Release, and a custom --dir is not
// recorded anywhere the lock/unlock path can read back); the repoKey
// component is Prepare's actual ownership signature regardless of which
// directory it was created under. The repository root for that key, and the
// lock reason for the ownership check, both come from the same `git worktree
// list --porcelain` call, which works whether dir is the worktree itself or
// the main repository (its first entry is always the main working tree, from
// any worktree, regardless of git-dir layout), so this needs no branching on
// which of Release's two cwd cases is in play.
func verifyZeroOwnedWorktree(ctx context.Context, runGit GitRunner, dir string, path string) (string, error) {
	output, err := gitOutput(ctx, runGit, dir, "worktree", "list", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("resolve repository for %s: %w", path, err)
	}
	entries := parseWorktreeList(output)
	if len(entries) == 0 {
		return "", fmt.Errorf("resolve repository for %s: git worktree list returned no entries", path)
	}
	// entries[0].path is always the main worktree (see primaryWorktreeRoot),
	// which is what Prepare now also keys its repoKey off, so this and Prepare
	// agree regardless of which worktree either call ran from.
	want := "zero-worktree-" + repoKey(entries[0].path)

	target := canonicalizePath(path)

	hasZeroComponent := false
	for _, component := range strings.Split(target, string(filepath.Separator)) {
		if component == want {
			hasZeroComponent = true
			break
		}
	}
	if !hasZeroComponent {
		return "", fmt.Errorf("refusing to release %s: not a zero-managed worktree (expected an ancestor directory named %q)", path, want)
	}

	// Require a registered porcelain entry before unlocking. Matching only
	// the public zero-worktree-<repoKey> path component would let Release
	// clear a manual lock on any same-named path under that directory.
	// Path comparison uses canonicalizePath so a lexical user argument
	// (macOS /var vs /private/var, symlink --worktree-dir) still matches
	// the physical spelling git worktree list reports.
	var matchedPath string
	for _, entry := range entries {
		if canonicalizePath(entry.path) != target {
			continue
		}
		matchedPath = entry.path
		if !entry.locked {
			// Already unlocked: nothing to release. Treat as success so a
			// double release is a no-op rather than a git "not locked" error.
			return "", errAlreadyUnlocked
		}
		if !strings.HasPrefix(entry.lockReason, leaseReasonPrefix) {
			return "", fmt.Errorf("refusing to release %s: locked with reason %q, not a zero lease", path, entry.lockReason)
		}
		if info, statErr := os.Stat(entry.path); statErr == nil && info.IsDir() {
			owned, err := hasOwnershipMarker(ctx, runGit, entry.path)
			if err != nil {
				return "", fmt.Errorf("verify worktree ownership for %s: %w", path, err)
			}
			if !owned {
				return "", fmt.Errorf("refusing to release %s: missing zero ownership marker (not created by `zero worktrees prepare`)", path)
			}
		}
		break
	}
	if matchedPath == "" {
		return "", fmt.Errorf("refusing to release %s: not a registered worktree of this repository", path)
	}
	return matchedPath, nil
}

// errAlreadyUnlocked is a sentinel for a Zero-managed path whose git lock is
// already clear; Release returns nil without calling unlock.
var errAlreadyUnlocked = errors.New("worktree already unlocked")

// canonicalizePath returns the physical, cleaned form of path when it can be
// resolved (EvalSymlinks), otherwise Abs+Clean. Used so comparisons against
// `git worktree list --porcelain` (which reports physical paths) succeed for
// lexical or symlinked user spellings of the same location.
func canonicalizePath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	abs = filepath.Clean(abs)
	// path itself does not exist (the documented release -C recovery: the
	// worktree directory was deleted by hand before releasing it), so
	// EvalSymlinks above had nothing to resolve. If an ANCESTOR of path is
	// itself a symlink (a symlinked --worktree-dir), the plain Abs+Clean
	// fallback keeps that ancestor's logical spelling, which never matches
	// git's physical-path porcelain entry, and the lock can never be
	// released. Resolve symlinks through the nearest existing ancestor
	// instead and reattach the missing tail.
	return resolveThroughNearestExistingAncestor(abs)
}

// resolveThroughNearestExistingAncestor walks up from path until it finds an
// existing ancestor directory, resolves that ancestor's symlinks, and
// reattaches path's missing remainder onto the resolved ancestor.
func resolveThroughNearestExistingAncestor(path string) string {
	var missing []string
	current := path
	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			result := filepath.Clean(resolved)
			for i := len(missing) - 1; i >= 0; i-- {
				result = filepath.Join(result, missing[i])
			}
			return result
		}
		parent := filepath.Dir(current)
		if parent == current {
			// Reached the filesystem root without finding an existing
			// ancestor; nothing left to resolve symlinks through.
			return path
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func DefaultBaseDir(env map[string]string) (string, error) {
	if runtime.GOOS == "windows" {
		if localAppData := strings.TrimSpace(envValue(env, "LOCALAPPDATA")); localAppData != "" {
			return filepath.Join(localAppData, "zero", "worktrees"), nil
		}
		if profile := strings.TrimSpace(envValue(env, "USERPROFILE")); profile != "" {
			return filepath.Join(profile, "AppData", "Local", "zero", "worktrees"), nil
		}
	}

	if stateHome := strings.TrimSpace(envValue(env, "XDG_STATE_HOME")); stateHome != "" {
		return filepath.Join(stateHome, "zero", "worktrees"), nil
	}
	home := strings.TrimSpace(firstNonEmpty(envValue(env, "HOME"), envValue(env, "USERPROFILE")))
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve user home: %w", err)
		}
	}
	return filepath.Join(home, ".local", "state", "zero", "worktrees"), nil
}

func resolveCwd(cwd string) (string, error) {
	if strings.TrimSpace(cwd) == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve cwd: %w", err)
		}
	}
	absolute, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("resolve cwd: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("cwd must be an existing directory: %s", absolute)
	}
	return filepath.Clean(absolute), nil
}

func validateName(name string) error {
	if !worktreeNamePattern.MatchString(name) || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("invalid worktree name %q: use letters, numbers, dots, dashes, or underscores", name)
	}
	return nil
}

func inspectTarget(target string) (bool, error) {
	info, err := os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("inspect worktree path: %w", err)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("worktree path already exists and is not a directory: %s", target)
	}
	if _, err := os.Stat(filepath.Join(target, ".git")); err == nil {
		return true, nil
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return false, fmt.Errorf("inspect worktree directory: %w", err)
	}
	if len(entries) != 0 {
		return false, fmt.Errorf("worktree path already exists and is not empty: %s", target)
	}
	return false, nil
}

func gitOutput(ctx context.Context, runGit GitRunner, dir string, args ...string) (string, error) {
	result, err := runGit(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		message := strings.TrimSpace(firstNonEmpty(result.Stderr, result.Stdout))
		if message == "" {
			message = fmt.Sprintf("git exited with code %d", result.ExitCode)
		}
		return "", fmt.Errorf("%s", message)
	}
	return strings.TrimSpace(result.Stdout), nil
}

// primaryWorktreeRoot returns the repository's main worktree path. `git
// worktree list --porcelain` always reports the main worktree first (its
// first entry), regardless of which linked worktree the command runs from -
// which is what lets Prepare and verifyZeroOwnedWorktree agree on one
// repoKey for the same repository even when invoked from different
// worktrees of it.
func primaryWorktreeRoot(ctx context.Context, runGit GitRunner, dir string) (string, error) {
	output, err := gitOutput(ctx, runGit, dir, "worktree", "list", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("resolve repository root: %w", err)
	}
	entries := parseWorktreeList(output)
	if len(entries) == 0 {
		return "", fmt.Errorf("resolve repository root: git worktree list returned no entries")
	}
	return entries[0].path, nil
}

func sameGitCommonDir(ctx context.Context, runGit GitRunner, sourceDir string, targetDir string) (bool, error) {
	sourceCommonDir, err := gitCommonDir(ctx, runGit, sourceDir)
	if err != nil {
		return false, err
	}
	targetCommonDir, err := gitCommonDir(ctx, runGit, targetDir)
	if err != nil {
		return false, err
	}
	return sourceCommonDir == targetCommonDir, nil
}

func gitCommonDir(ctx context.Context, runGit GitRunner, dir string) (string, error) {
	value, err := gitOutput(ctx, runGit, dir, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(dir, value)
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve git common dir: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve git common dir: %w", err)
	}
	return filepath.Clean(resolved), nil
}

// newHardenedCommand builds a subprocess that dies as one unit on cancel.
//
// Extracted so a test can exercise the SAME constructor defaultRunGit uses. A
// test that only called hardenWorktreeGit would prove the helper sets three
// fields and prove nothing about whether anything calls it — which is the
// wiring gap this feature has produced four times.
func newHardenedCommand(ctx context.Context, dir string, name string, args ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir
	// Pin C locale so English phrases callers match on (e.g. "already locked"
	// in lockWorktree) stay parseable under localized git installs.
	//
	// Set HERE rather than in defaultRunGit, so every command built through this
	// constructor is parseable, not just the one path that happened to need it
	// first.
	command.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	// Without this a cancelled git is signalled but its Wait can still block on
	// pipes a grandchild holds open. git is short-lived and rarely forks, so
	// this is a small risk — but the worktree path is now reached by a plan,
	// and a plan can be cancelled at any moment by /plans stop or by the run
	// ending.
	hardenWorktreeGit(command)
	return command
}

func defaultRunGit(ctx context.Context, dir string, args ...string) (CommandResult, error) {
	command := newHardenedCommand(ctx, dir, "git", args...)
	// Capture stdout and stderr separately: callers parse Stdout for values
	// (rev-parse output) and prefer Stderr for error messages. CombinedOutput
	// merged the two, letting git's stderr warnings pollute parsed output and
	// leaving CommandResult.Stderr always empty.
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	exitCode := 0
	if err != nil {
		exitCode = -1
		if exitError, ok := err.(*exec.ExitError); ok {
			exitCode = exitError.ExitCode()
			err = nil
		}
	}
	return CommandResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: exitCode}, err
}

func defaultWorktreeName(now time.Time) string {
	return "task-" + now.UTC().Format("20060102-150405")
}

func repoKey(repoRoot string) string {
	sum := sha1.Sum([]byte(filepath.Clean(repoRoot)))
	hash := hex.EncodeToString(sum[:])[:10]
	base := filepath.Base(repoRoot)
	base = strings.ToLower(base)
	base = strings.Trim(regexp.MustCompile(`[^a-z0-9._-]+`).ReplaceAllString(base, "-"), "-._")
	if base == "" {
		base = "repo"
	}
	return base + "-" + hash
}

func envValue(env map[string]string, key string) string {
	if env != nil {
		return env[key]
	}
	return os.Getenv(key)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// Clean prunes any zero-owned git worktrees older than maxAge to prevent disk space leaks.
func Clean(ctx context.Context, options Options, maxAge time.Duration) error {
	cwd, err := resolveCwd(options.Cwd)
	if err != nil {
		return err
	}
	runGit := options.RunGit
	if runGit == nil {
		runGit = defaultRunGit
	}
	repoRoot, err := gitOutput(ctx, runGit, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("not a git repository: %w", err)
	}
	repoRoot = filepath.Clean(repoRoot)

	baseDir := strings.TrimSpace(options.BaseDir)
	if baseDir == "" {
		baseDir, err = DefaultBaseDir(options.Env)
		if err != nil {
			return err
		}
	}
	baseDir, err = filepath.Abs(baseDir)
	if err != nil {
		return err
	}
	// git worktree list --porcelain reports each worktree's PHYSICAL location,
	// resolving any symlink component (for example a --worktree-dir that is
	// itself a symlink, or a symlinked ancestor). Comparing entry paths against
	// a merely-absolute (but not symlink-resolved) baseDir would then reject
	// every worktree Prepare actually created under a symlinked base, leaving
	// them permanently unprunable. EvalSymlinks failing (most commonly because
	// the directory does not exist yet) just means there is nothing under it
	// to prune, so falling back to the plain absolute path is safe.
	if resolved, err := filepath.EvalSymlinks(baseDir); err == nil {
		baseDir = resolved
	}

	output, err := gitOutput(ctx, runGit, repoRoot, "worktree", "list", "--porcelain")
	if err != nil {
		return fmt.Errorf("list git worktrees: %w", err)
	}
	entries := parseWorktreeList(output)
	if len(entries) == 0 {
		return fmt.Errorf("list git worktrees: git worktree list returned no entries")
	}
	// Prepare only ever creates worktrees under this per-repository subtree
	// (mirroring the repoDir it computes). Scoping pruning to baseDir itself
	// would authorize deleting a worktree a user manages by hand in the same
	// directory, which Zero never created and has no business force-removing.
	//
	// The bucket must be keyed off the MAIN worktree's root
	// (entries[0].path, per primaryWorktreeRoot), not repoRoot: repoRoot is
	// --show-toplevel for whichever worktree Clean itself runs from, which is
	// a linked checkout's own root when Clean (or the Prepare call that
	// auto-invokes it) runs there. Prepare keys repoDir off the same
	// entries[0].path, so using repoRoot here instead would compute a
	// different repoDir than Prepare's and filter out every actual
	// zero-owned worktree for this repository whenever either call runs from
	// a linked worktree.
	repoDir := filepath.Join(baseDir, "zero-worktree-"+repoKey(entries[0].path))

	cutoff := time.Now().Add(-maxAge)
	var lastErr error
	for _, entry := range entries {
		// Compare against the physical spelling of the entry path. Git's
		// porcelain listing is normally already physical, but test fixtures
		// and some symlink layouts can leave a logical spelling that would
		// otherwise fail the containment check against the resolved repoDir.
		// Git commands still receive entry.path (the registered spelling).
		entryPath := canonicalizePath(entry.path)
		// Only prune worktrees zero created for this repository (i.e. inside
		// repoDir), using a path-boundary-safe comparison so a sibling
		// directory that merely shares repoDir as a string prefix (e.g.
		// "<repoDir>-other") can't match.
		if !isUnderDir(entryPath, repoDir) {
			continue
		}

		// A locked worktree is never a prune candidate - with one recovery
		// carve-out: a Zero lease whose recorded owner process is provably
		// dead (SIGKILL, crash, power loss skipped the deferred release).
		// Without it, an abnormal exit leaves the lock in place forever and
		// Clean can never reclaim the disk. Human locks and PID-less Zero
		// leases (external prepare callers) are always honored.
		expiredLease := false
		if entry.locked {
			pid, ok := leasePID(entry.lockReason)
			if !ok || processAlive(pid) {
				continue
			}
			expiredLease = true
		}

		// Prefer the physical path for filesystem probes when the entry
		// resolves; fall back to the registered spelling if it does not
		// (for example a prunable entry whose directory is already gone).
		statPath := entryPath
		info, err := os.Stat(statPath)
		if err != nil && statPath != entry.path {
			statPath = entry.path
			info, err = os.Stat(statPath)
		}
		if err != nil {
			if os.IsNotExist(err) {
				if expiredLease {
					// Match the on-disk recovery path: a failed unlock leaves
					// the lock in place and must not be reported as success, or
					// the orphan is skipped silently on every later Clean pass.
					if _, unlockErr := gitOutput(ctx, runGit, repoRoot, "worktree", "unlock", entry.path); unlockErr != nil {
						lastErr = errors.Join(lastErr, fmt.Errorf("recover expired lease %s: %w", entry.path, unlockErr))
						continue
					}
				}
				_, _ = runGit(ctx, repoRoot, "worktree", "prune")
			}
			continue
		}
		if !info.IsDir() {
			continue
		}

		if worktreeIsStale(statPath, cutoff) {
			// Ownership / legacy status first: the dirty probe's includeIgnored
			// flag depends on it. An explicit release (unlocked, marker present)
			// is a completion signal, so ignored residue is reclaimable. An
			// expired lease is not (task may have died mid-work). A legacy
			// pre-upgrade worktree never had Release at all — unlocked was its
			// only state — so it gets the same benefit of the doubt as a
			// crashed lease: ignored files block removal. Probing dirty before
			// this would treat every legacy unlocked worktree as released and
			// delete gitignored contents (e.g. .env) on first post-upgrade Clean.
			owned, err := hasOwnershipMarker(ctx, runGit, statPath)
			if err != nil {
				continue
			}
			legacy := false
			if !owned {
				legacy = isLegacyZeroWorktree(ctx, runGit, statPath, repoDir, entry)
			}
			includeIgnored := expiredLease || legacy
			if worktreeIsDirty(ctx, runGit, statPath, includeIgnored) {
				// A stale mtime only means nothing changed at the worktree's
				// top level or below recently; it does not mean the task
				// holding it is done. Uncommitted or untracked changes are
				// still live work waiting on a model, network, or user, so
				// force-removing here would discard it. Skip until it either
				// gets committed/cleaned (no longer dirty) or unlocked.
				continue
			}
			if !owned {
				if legacy {
					if writeErr := writeOwnershipMarker(ctx, runGit, statPath); writeErr == nil {
						owned = true
					}
				}
			}
			if !owned {
				continue
			}
			if err := preserveUnreachableWorktreeHead(ctx, runGit, repoRoot, statPath); err != nil {
				lastErr = errors.Join(lastErr, fmt.Errorf("preserve worktree HEAD %s: %w", entry.path, err))
				continue
			}
			if expiredLease {
				// Recover the dead owner's lease only once every removal
				// guard has passed, so a failed unlock (or a later guard)
				// leaves the lock in place rather than half-recovered.
				if _, err := gitOutput(ctx, runGit, repoRoot, "worktree", "unlock", entry.path); err != nil {
					lastErr = errors.Join(lastErr, fmt.Errorf("recover expired lease %s: %w", entry.path, err))
					continue
				}
			}
			// gitOutput (not a raw runGit call) so a nonzero exit code is
			// reported as a failure: defaultRunGit deliberately returns a nil
			// error alongside a nonzero CommandResult.ExitCode for a failed git
			// invocation, so checking only the returned error would silently
			// treat a failed removal (busy, permission denied) as success.
			if _, err := gitOutput(ctx, runGit, repoRoot, "worktree", "remove", "--force", entry.path); err != nil {
				// errors.Join, not a plain overwrite: multiple stale worktrees can
				// fail removal in the same Clean pass (locking, permissions), and
				// only reporting the last one would hide the others from the caller.
				lastErr = errors.Join(lastErr, fmt.Errorf("remove worktree %s: %w", entry.path, err))
			}
		}
	}

	_, _ = runGit(ctx, repoRoot, "worktree", "prune")
	return lastErr
}

type worktreeEntry struct {
	path       string
	locked     bool
	lockReason string
}

// parseWorktreeList reads `git worktree list --porcelain` output into one
// entry per worktree. Entries are delimited by their own "worktree <path>"
// line rather than by blank-line blocks, so this tolerates both real git
// output (attribute lines plus a blank-line separator) and a minimal listing
// with no separators.
func parseWorktreeList(output string) []worktreeEntry {
	var entries []worktreeEntry
	var current *worktreeEntry
	for _, line := range strings.Split(output, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			if current != nil {
				entries = append(entries, *current)
			}
			current = &worktreeEntry{path: canonicalizePath(strings.TrimPrefix(line, "worktree "))}
		case current != nil && (line == "locked" || strings.HasPrefix(line, "locked ")):
			current.locked = true
			current.lockReason = strings.TrimSpace(strings.TrimPrefix(line, "locked"))
		}
	}
	if current != nil {
		entries = append(entries, *current)
	}
	return entries
}

// isUnderDir reports whether path is dir itself or a descendant of it. Unlike
// a bare strings.HasPrefix(path, dir), this rejects a sibling that merely
// shares dir as a string prefix (e.g. dir "/a/base" must not match
// "/a/base-other"), and filepath.Rel makes the comparison Windows-correct.
func isUnderDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// preserveUnreachableWorktreeHead guards against `git worktree remove --force`
// silently discarding a commit: Prepare creates every worktree with `worktree
// add --detach`, so its HEAD is a plain commit, not a branch, and nothing
// outside that worktree's own administrative files points at it. If a task
// committed its result there and the worktree goes stale before that commit
// is otherwise referenced (merged, pushed, cherry-picked), force-removing it
// deletes the only ref keeping the commit reachable - it becomes immediately
// eligible for git gc, exactly as if it had never been committed. This checks
// whether some OTHER ref in the repository already contains worktreePath's
// HEAD; if none does, it creates a durable ref for it in the main
// repository's refs namespace before the caller proceeds to remove the
// worktree, so the commit survives (visible under refs/zero/orphaned-worktree)
// even after the worktree itself is gone.
func preserveUnreachableWorktreeHead(ctx context.Context, runGit GitRunner, repoRoot, worktreePath string) error {
	// --verify --quiet distinguishes unborn/missing HEAD (exit 1, no output)
	// from real probe failures (other nonzero codes). Treating every failure
	// as "nothing to preserve" used to authorize force-removal when HEAD was
	// simply unreadable.
	result, err := runGit(ctx, worktreePath, "rev-parse", "--verify", "--quiet", "HEAD")
	if err != nil {
		return fmt.Errorf("resolve worktree HEAD: %w", err)
	}
	if result.ExitCode != 0 {
		if result.ExitCode == 1 {
			// Unborn or missing HEAD: no commit to keep alive.
			return nil
		}
		message := strings.TrimSpace(firstNonEmpty(result.Stderr, result.Stdout))
		if message == "" {
			message = fmt.Sprintf("git exited with code %d", result.ExitCode)
		}
		return fmt.Errorf("resolve worktree HEAD: %s", message)
	}
	head := strings.TrimSpace(result.Stdout)
	if head == "" {
		return nil
	}
	contained, err := gitOutput(ctx, runGit, repoRoot, "for-each-ref", "--contains="+head, "--count=1", "--format=%(refname)")
	if err != nil {
		return fmt.Errorf("check ref reachability for %s: %w", head, err)
	}
	if strings.TrimSpace(contained) != "" {
		// Already reachable from some branch/tag; the worktree's own HEAD is
		// redundant and removal cannot orphan the commit.
		return nil
	}
	refName := "refs/zero/orphaned-worktree/" + head
	if _, err := gitOutput(ctx, runGit, repoRoot, "update-ref", refName, head); err != nil {
		return fmt.Errorf("preserve unreachable commit %s: %w", head, err)
	}
	return nil
}

// worktreeIsStale reports whether every file under root was last modified
// before cutoff. A directory's own mtime only changes when an entry is
// added, removed, or renamed directly inside it, not when a long-running
// task edits files deeper in the tree, so checking root's mtime alone can
// mistake an actively-used worktree for stale. Walking the tree and bailing
// out on the first recent entry avoids that false positive.
//
// Any inspection failure (a WalkDir error, or a DirEntry that can't report its
// own info) fails closed: it's treated the same as "not stale," never as
// "stale," so an incomplete inspection can't authorize a forced removal.
func worktreeIsStale(root string, cutoff time.Time) bool {
	stale := true
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			stale = false
			return filepath.SkipAll
		}
		info, err := d.Info()
		if err != nil {
			stale = false
			return filepath.SkipAll
		}
		if info.ModTime().After(cutoff) {
			stale = false
			return filepath.SkipAll
		}
		return nil
	})
	return stale && walkErr == nil
}

// worktreeIsDirty reports whether a worktree has changes that block a forced
// removal, via `git status --porcelain` run inside it. A task can hold a
// worktree with live, unpushed work while it waits on a model, network, or
// user for far longer than the staleness window, without writing to the tree
// again in that time; mtime alone can't distinguish that from an abandoned
// one, but a dirty working tree still can.
//
// includeIgnored additionally counts files matched by .gitignore
// (credentials, generated drafts, task artifacts) as live data. It is set
// when the worktree's owner never signaled completion (an expired crashed
// lease): such files may be all a dead task left behind. It is clear for an
// explicitly released worktree, where ignored residue like node_modules or
// build output would otherwise make the released checkout unreclaimable at
// every age - release is the owner's statement that the task is done.
//
// An inspection failure fails closed, treating it as dirty rather than clean:
// an incomplete check must not authorize a forced removal.
func worktreeIsDirty(ctx context.Context, runGit GitRunner, path string, includeIgnored bool) bool {
	args := []string{"status", "--porcelain"}
	if includeIgnored {
		args = append(args, "--ignored")
	}
	output, err := gitOutput(ctx, runGit, path, args...)
	if err != nil {
		return true
	}
	return output != ""
}
