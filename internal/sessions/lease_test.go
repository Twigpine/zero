package sessions

import (
	"runtime"
	"testing"
	"time"
)

// collectGarbage runs the collector and gives finalizers time to run, so a file
// left only to a Store that is gone would be closed by the time it returns.
func collectGarbage() {
	for i := 0; i < 5; i++ {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
	}
}

// A lease lasts for the life of the process, not of the Store that took it.
// PrepareExec takes the calling or forked session's lease on a Store the exec
// run never touches again, so a lease that went with the Store would be dropped
// once the garbage collector closed its file, and prune could remove a session
// the run still uses.
func TestLeaseOutlivesTheStoreThatTookIt(t *testing.T) {
	for _, take := range []struct {
		name string
		do   func(store *Store) error
	}{
		{"create with a parent", func(store *Store) error {
			_, err := store.Create(CreateInput{SessionID: "new", ParentSessionID: "parent"})
			return err
		}},
		{"exec calling session", func(store *Store) error {
			_, err := PrepareExec(PrepareExecOptions{Store: store, SessionID: "new", CallingSessionID: "parent"})
			return err
		}},
		{"hold", func(store *Store) error {
			store.Hold("parent")
			return nil
		}},
	} {
		t.Run(take.name, func(t *testing.T) {
			root := t.TempDir()
			createFinishedSession(t, root, "parent", "2026-06-01T00:00:00Z", "")
			// The Store is unreachable once this returns, as PrepareExec's is in
			// an exec run.
			func() {
				store := NewStore(StoreOptions{RootDir: root, Now: fixedClock("2026-06-01T00:00:00Z")})
				if err := take.do(store); err != nil {
					t.Fatalf("%s: %v", take.name, err)
				}
			}()
			collectGarbage()

			report, err := pruneStore(root).Prune(PruneOptions{OlderThan: thirtyDays, DryRun: true})
			if err != nil {
				t.Fatalf("dry run: %v", err)
			}
			if reason := keptReason(report, "parent"); reason != PruneKeptOpen {
				t.Fatalf("parent kept for %q once the Store that took its lease was collected, want %q", reason, PruneKeptOpen)
			}
		})
	}
}

// Stores in one process share the lease on a session, and it lasts until the
// last of them releases it.
func TestLeaseHeldByTwoStoresLastsUntilBothRelease(t *testing.T) {
	root := t.TempDir()
	createFinishedSession(t, root, "shared", "2026-06-01T00:00:00Z", "")
	first := NewStore(StoreOptions{RootDir: root})
	second := NewStore(StoreOptions{RootDir: root})
	first.Hold("shared")
	second.Hold("shared")

	// Releasing twice from one Store gives up only that Store's hold.
	first.Release("shared")
	first.Release("shared")
	if release, locked, err := pruneStore(root).HoldExclusive("shared"); err != nil || locked {
		release()
		t.Fatalf("prune took a session another Store still holds: locked=%v, %v", locked, err)
	}

	second.Release("shared")
	release, locked, err := pruneStore(root).HoldExclusive("shared")
	if err != nil || !locked {
		t.Fatalf("prune could not take the session after both Stores released it: locked=%v, %v", locked, err)
	}
	release()

	// Held again after the last release, the lease is taken afresh.
	first.Hold("shared")
	if release, locked, err := pruneStore(root).HoldExclusive("shared"); err != nil || locked {
		release()
		t.Fatalf("prune took a session held again after it was released: locked=%v, %v", locked, err)
	}
	first.Release("shared")
}
