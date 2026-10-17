package sessions

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// A fork made after prune's plan, by a process that has exited since, is not in
// the plan and holds nothing. It still keeps its parent: once prune holds that
// parent exclusively it looks for children the plan did not know about.
func TestPruneKeepsAParentForkedAfterThePlan(t *testing.T) {
	root := t.TempDir()
	createFinishedSession(t, root, "parent", "2026-06-01T00:00:00Z", "")

	forked := false
	prunePlannedSeam = func() {
		other := NewStore(StoreOptions{RootDir: root, Now: fixedClock("2026-09-25T00:00:00Z")})
		if _, err := other.Fork("parent", ForkInput{SessionID: "late"}); err != nil {
			t.Errorf("fork after the plan: %v", err)
			return
		}
		forked = true
		// The forking process exits, and its leases go with it.
		other.Release("late")
		other.Release("parent")
	}
	defer func() { prunePlannedSeam = nil }()

	report, err := pruneStore(root).Prune(PruneOptions{OlderThan: thirtyDays})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if !forked {
		t.Fatal("SETUP INVALID: the fork after the plan never happened")
	}
	if reason := keptReason(report, "parent"); reason != PruneKeptParent {
		t.Errorf("parent kept for %q, want %q (removed %v)", reason, PruneKeptParent, pruneIDs(report.Removed))
	}
	if lineage, err := pruneStore(root).Lineage("late"); err != nil || len(lineage) != 2 {
		t.Fatalf("the late fork's lineage is broken: %d entries, %v", len(lineage), err)
	}
}

// A dry run decides what a real run would, including what only shows once a
// session is held for removal: a fork made after the plan, a write since the
// plan, and a process that opened the session since the plan. Each case runs
// both ways from the same start, and both reports must be the one wanted.
func TestPruneDryRunDecidesLikeARealRunAtRemoval(t *testing.T) {
	cases := []struct {
		name string
		// create lays out the sessions. afterPlan changes them once the plan is
		// made, and returns what to undo when Prune has finished.
		create    func(t *testing.T, root string)
		afterPlan func(t *testing.T, root string) (undo func())
		want      string
	}{
		{
			name: "a fork made after the plan",
			create: func(t *testing.T, root string) {
				createFinishedSession(t, root, "parent", "2026-06-01T00:00:00Z", "")
			},
			afterPlan: func(t *testing.T, root string) func() {
				other := NewStore(StoreOptions{RootDir: root, Now: fixedClock("2026-09-25T00:00:00Z")})
				if _, err := other.Fork("parent", ForkInput{SessionID: "late"}); err != nil {
					t.Errorf("fork after the plan: %v", err)
				}
				other.Release("late")
				other.Release("parent")
				return func() {}
			},
			want: "removed [] kept [parent: " + PruneKeptParent + "] failed []",
		},
		{
			name: "a write since the plan",
			create: func(t *testing.T, root string) {
				createFinishedSession(t, root, "parent", "2026-06-01T00:00:00Z", "")
				createFinishedSession(t, root, "child", "2026-06-15T00:00:00Z", "parent")
			},
			afterPlan: func(t *testing.T, root string) func() {
				rewriteUpdatedAt(t, root, "child", "2026-09-25T23:00:00Z")
				return func() {}
			},
			want: "removed [] kept [child: " + PruneKeptUpdated + ", parent: " + PruneKeptParent + "] failed []",
		},
		{
			name: "a session opened since the plan",
			create: func(t *testing.T, root string) {
				createFinishedSession(t, root, "opened", "2026-06-01T00:00:00Z", "")
				createFinishedSession(t, root, "idle", "2026-06-02T00:00:00Z", "")
			},
			afterPlan: func(t *testing.T, root string) func() {
				other := NewStore(StoreOptions{RootDir: root})
				if busy := other.hold("opened"); busy {
					t.Error("SETUP INVALID: the session was busy when opened after the plan")
				}
				return func() { other.Release("opened") }
			},
			want: "removed [idle] kept [opened: " + PruneKeptOpen + "] failed []",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() { prunePlannedSeam = nil }()
			for _, dryRun := range []bool{false, true} {
				root := t.TempDir()
				tc.create(t, root)
				before := sessionDirNames(t, root)
				var undo func()
				prunePlannedSeam = func() { undo = tc.afterPlan(t, root) }
				report, err := pruneStore(root).Prune(PruneOptions{OlderThan: thirtyDays, DryRun: dryRun})
				prunePlannedSeam = nil
				if undo == nil {
					t.Fatal("SETUP INVALID: nothing changed after the plan")
				}
				undo()
				if err != nil {
					t.Fatalf("dry run %v: Prune: %v", dryRun, err)
				}
				if got := pruneSummary(report); got != tc.want {
					t.Errorf("dry run %v: the report is %s, want %s", dryRun, got, tc.want)
				}
				if !dryRun {
					continue
				}
				for _, id := range before {
					if _, err := os.Stat(pruneStore(root).metadataPath(id)); err != nil {
						t.Errorf("the dry run removed the metadata of %s: %v", id, err)
					}
				}
			}
		})
	}
}

// pruneSummary puts what a report decided on one line, in report order.
func pruneSummary(report PruneReport) string {
	kept := []string{}
	for _, entry := range report.Kept {
		kept = append(kept, entry.SessionID+": "+entry.Reason)
	}
	return fmt.Sprintf("removed %v kept [%s] failed %v", pruneIDs(report.Removed), strings.Join(kept, ", "), pruneIDs(report.Failed))
}

// Prune removes the metadata first and unlinks lease.lock after it. A process
// that picked the session a moment earlier and only now takes its lease creates
// a fresh lease.lock in the directory prune is emptying, and locks it with
// nothing to contend with. Continuing the session, or creating one under it, is
// refused all the same, and prune still removes the directory.
func TestContinuingASessionPruneIsRemovingIsRefused(t *testing.T) {
	root := t.TempDir()
	createFinishedSession(t, root, "old", "2026-06-01T00:00:00Z", "")
	other := NewStore(StoreOptions{RootDir: root})

	reached := false
	pruneRemoveDirSeam = func(id string, _ bool) {
		if id != "old" {
			return
		}
		reached = true
		if _, err := os.Stat(other.metadataPath("old")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("SETUP INVALID: the metadata is still there at the seam: %v", err)
		}
		if err := other.HoldToContinue("old"); !errors.Is(err, ErrPruning) || !strings.Contains(err.Error(), "was removed") {
			t.Errorf("continue a session prune is removing: err = %v, want it refused as removed", err)
		}
		if events, err := readExecContextEvents(other, "old"); !errors.Is(err, ErrPruning) || events != nil {
			t.Errorf("exec context read of a session prune is removing: %d events, err = %v, want it refused", len(events), err)
		}
		if _, err := other.Create(CreateInput{SessionID: "child", ParentSessionID: "old"}); !errors.Is(err, ErrPruning) {
			t.Errorf("create a session under one prune is removing: err = %v, want it refused", err)
		}
	}
	defer func() { pruneRemoveDirSeam = nil }()

	report, err := pruneStore(root).Prune(PruneOptions{OlderThan: thirtyDays})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if !reached {
		t.Fatal("SETUP INVALID: prune never reached the directory removal")
	}
	if strings.Join(pruneIDs(report.Removed), ",") != "old" || len(report.Failed) != 0 {
		t.Errorf("removed %v, failed %v: the refused continuation must not keep prune from removing the directory", pruneIDs(report.Removed), pruneIDs(report.Failed))
	}
	if sessionDirExists(t, root, "old") || sessionDirExists(t, root, "child") {
		t.Errorf("left behind: old=%v child=%v", sessionDirExists(t, root, "old"), sessionDirExists(t, root, "child"))
	}

	// Once prune has finished, the session the caller picked is simply gone, and
	// continuing it is refused the same way.
	if err := other.HoldToContinue("old"); !errors.Is(err, ErrPruning) {
		t.Errorf("continue a session prune removed: err = %v, want it refused", err)
	}
	// A caller that picked nothing still reads a missing session as empty.
	events, present, err := other.ReadRehydratedEventsWithPresence("never-existed")
	if err != nil || present || len(events) != 0 {
		t.Errorf("rehydrated read of a session that never existed = %d events, present=%v, err=%v; want empty", len(events), present, err)
	}
}
