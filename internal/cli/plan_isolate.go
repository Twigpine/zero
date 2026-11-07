package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Twigpine/zero/internal/specialist"
	"github.com/Twigpine/zero/internal/worktrees"
)

// The isolator: a git worktree for a plan that may write.
//
// It reuses internal/worktrees, the same package `zero exec --worktree` already
// uses, rather than shelling out to git again. That matters beyond tidiness —
// worktrees.Prepare already handles name validation, the base directory, repo
// discovery from a subdirectory, and the difference between a checkout and a
// worktree. A second implementation would eventually disagree with the first
// about which of those a plan gets.
//
// NOT DELETED ON RELEASE. A plan that wrote something produced work the user has
// not seen yet, and removing the tree would remove the only copy of it. Release
// therefore keeps the tree and drops only the lease, exactly as
// `zero exec --worktree` does when it exits: the lease protects a tree while a
// run is USING it, and worktrees.Clean never removes an unlocked tree that holds
// uncommitted work (and preserves an unreachable HEAD before removing a clean
// one), so the retained output survives without a lock nobody can clear.
//
// ONE TREE PER EXECUTION. The name carries a per-run suffix, so a second run of
// the same plan neither collides with the first run's tree nor starts on top of
// output the user has not reviewed yet. With a fixed name and a PID-less lease
// that Release never dropped, every rerun of a write-capable plan was refused as
// "locked by another active run" until someone released it by hand.

// planWorktreeName derives a worktree name from a plan name.
//
// worktrees.Prepare enforces its own allow-list on the result, so this only has
// to produce a candidate; anything it cannot clean is handed over as a prefix
// and Prepare supplies the rest. Deliberately NOT a sanitiser that promises
// safety — the guarantee lives in one place, where it is tested.
func planWorktreeName(planName string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_':
			return r
		default:
			return -1
		}
	}, planName)
	cleaned = strings.TrimSpace(cleaned)
	if cleaned == "" {
		return "plan"
	}
	if len(cleaned) > 40 {
		cleaned = cleaned[:40]
	}
	return "plan-" + cleaned
}

// newPlanIsolator returns an isolator rooted at the run's workspace, or nil when
// this run cannot isolate at all.
//
// nil is the meaningful answer, not an error: specialist refuses a plan that
// requires isolation when the isolator is nil, with a reason. Returning a
// non-nil isolator that always fails would move the same refusal later and make
// it read like a malfunction.
func newPlanIsolator(workspaceRoot string) specialist.PlanIsolator {
	return newPlanIsolatorWith(workspaceRoot, planIsolatorDeps{})
}

// planIsolatorDeps are the seams a test needs to drive the isolator against a
// real repository without touching the user's state directory or the clock.
type planIsolatorDeps struct {
	baseDir string
	now     func() time.Time
	suffix  func() string
}

func newPlanIsolatorWith(workspaceRoot string, deps planIsolatorDeps) specialist.PlanIsolator {
	if strings.TrimSpace(workspaceRoot) == "" {
		return nil
	}
	now := deps.now
	if now == nil {
		now = time.Now
	}
	suffix := deps.suffix
	if suffix == nil {
		suffix = randomPlanWorktreeSuffix
	}
	return func(ctx context.Context, planName string) (specialist.PlanWorkspace, error) {
		prepared, err := worktrees.Prepare(ctx, worktrees.Options{
			Cwd:     workspaceRoot,
			Name:    planExecutionWorktreeName(planName, now(), suffix()),
			BaseDir: deps.baseDir,
			// The lease is bound to THIS process, like `zero exec --worktree`:
			// if Zero dies before Release runs, Clean can expire it instead of
			// skipping the tree forever.
			LeasePID: os.Getpid(),
		})
		if err != nil {
			return specialist.PlanWorkspace{}, err
		}
		if strings.TrimSpace(prepared.Path) == "" {
			// Defence in depth against a Prepare that reports success with no
			// path: specialist refuses an un-isolated workspace, and this makes
			// sure it sees one rather than an empty string it might read as the
			// parent's own directory.
			return specialist.PlanWorkspace{}, fmt.Errorf("prepared worktree has no path")
		}
		// Only the lease this call took is ours to drop. A tree another live
		// caller already held reports LockAcquired=false, and releasing it would
		// expose that caller's workspace to Clean.
		release := func() {}
		if prepared.LockAcquired {
			var once sync.Once
			release = func() {
				once.Do(func() {
					_ = worktrees.Release(context.Background(), worktrees.Options{Cwd: workspaceRoot}, prepared.Path)
				})
			}
		}
		return specialist.PlanWorkspace{
			Path:     prepared.Path,
			Isolated: true,
			Describe: fmt.Sprintf("worktree %s at %s", prepared.Name, prepared.Path),
			// Release drops the lease and KEEPS the tree: the plan's output is
			// unreviewed work, and Clean leaves an unlocked tree with uncommitted
			// changes alone.
			Release: release,
			// Abort drops the lease on a tree the plan never received. It is
			// fresh and untouched, so leaving it unlocked lets Clean reclaim it.
			Abort: release,
		}, nil
	}
}

// planExecutionWorktreeName is the per-execution worktree name: the plan-derived
// prefix plus a UTC timestamp and a random suffix, so two runs of one plan —
// even in the same second — never share a tree.
func planExecutionWorktreeName(planName string, at time.Time, suffix string) string {
	name := planWorktreeName(planName) + "-" + at.UTC().Format("20060102-150405")
	if suffix = strings.TrimSpace(suffix); suffix != "" {
		name += "-" + suffix
	}
	return name
}

func randomPlanWorktreeSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}
