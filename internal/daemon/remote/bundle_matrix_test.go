package remote

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// The crash rows of the recovery plan's Acceptance Examples, as code. Each row
// drives the REAL extract into the on-disk state a stop at one step leaves, then
// runs recovery twice and asserts both halves the table asserts: which tree is
// live, and where every other copy of a tree ended up. Nothing here plants a
// staging dir by hand, so the fixture is whatever the write path actually
// writes; a hand-built one would only prove recovery agrees with the test's idea
// of the write path.
//
// A second pass is asserted because recovery keeps no memory: a copy retained on
// pass one must not be reclassified on pass two, and a copy deleted on pass one
// must not come back.

// copyDisposition is where one copy of a tree ends up. The five values are the
// only terminal states the table uses, so a row that means "retained" has to say
// which kind of retention, and a delete can never be written down as anything
// else.
type copyDisposition int

const (
	// copyDeleted: gone from disk, under neither prefix.
	copyDeleted copyDisposition = iota
	// copyAtStaging: still under the name the extract allocated for it.
	copyAtStaging
	// copyAtKept: moved under the Kept prefix, where the scan does not look.
	copyAtKept
	// copyInPlaceUnowned: still at its name and carrying no marker, so nothing
	// on disk attributes it to this code.
	copyInPlaceUnowned
	// copyAtDest: restored, so the copy's content is the live tree and its
	// directory is gone.
	copyAtDest
)

// bundleCrashRow is one row of the table. Each of these rows leaves exactly one
// staged copy, and the assertion compares the WHOLE set of copies on disk
// against that one, so a row can never pass while a second copy nobody expected
// sits beside it.
type bundleCrashRow struct {
	id string
	// arrange runs the real extract with faults injected to stop it at one step.
	// The faults are unwound before recovery runs, so recovery sees a crashed
	// tree and not a failing filesystem.
	arrange  func(t *testing.T, dir, dest string)
	wantLive string
	wantCopy copyDisposition
	// report1 and report2 assert the copy is named in the recovery report on
	// that pass. A copy recovery retains but never names is one an operator
	// cannot find, which the table counts as a failure of the row.
	report1 bool
	report2 bool
}

// arrangeInterruptedSwap stops the extract between its two swap renames: the
// prior tree is aside in backup, the publish failed, and the restore failed too,
// so the destination is absent and that backup is the only copy of it. S4 and S7
// differ in how the writer got here and are identical on disk, which is why they
// share one arrangement.
func arrangeInterruptedSwap(t *testing.T, dir, dest string) {
	t.Helper()
	injected := errors.New("injected swap failure")
	injectFault(t, "rename", func(args ...string) bool {
		from := filepath.Base(args[0])
		return from == "repo" || from == "backup"
	}, injected)
	if err := extractBundle(context.Background(), testBundle(t, "a.txt", "v1"), dest, nil); err == nil {
		t.Fatal("an extract whose publish and restore both fail must report an error")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("the interrupted swap should leave %s absent, got %v", dest, err)
	}
}

func bundleCrashRows() []bundleCrashRow {
	return []bundleCrashRow{
		{
			// A stop between the directory and its marker leaves a directory
			// nothing on disk attributes. Deleting it would need proof this code
			// wrote it, and the marker is that proof, so it is retained and
			// named rather than reaped on the strength of its name.
			id: "S1",
			arrange: func(t *testing.T, dir, dest string) {
				injectFault(t, "createTemp", func(args ...string) bool {
					return args[1] == stagingMarkerFile+"-*"
				}, errors.New("injected marker temp failure"))
				injectFault(t, "removeAll", nil, errors.New("injected cleanup failure"))
				if err := extractBundle(context.Background(), testBundle(t, "a.txt", "v1"), dest, nil); err == nil {
					t.Fatal("an extract whose marker cannot be written must fail")
				}
			},
			wantLive: "v0",
			wantCopy: copyInPlaceUnowned,
			report1:  true,
			report2:  true,
		},
		{
			// The marker landed and the clone did not, so the directory is owned
			// and holds no copy of any tree. Nothing can be lost by removing it,
			// and leaving it is what accumulates scratch forever.
			id: "S2",
			arrange: func(t *testing.T, dir, dest string) {
				injectFault(t, "removeAll", nil, errors.New("injected cleanup failure"))
				if err := extractBundle(context.Background(), filepath.Join(dir, "missing.bundle"), dest, nil); err == nil {
					t.Fatal("cloning a bundle that does not exist must fail")
				}
			},
			wantLive: "v0",
			wantCopy: copyDeleted,
		},
		{
			// The prior tree is aside and the destination is absent, so the
			// staged copy is the only copy of it and has to come back.
			id:       "S4",
			arrange:  arrangeInterruptedSwap,
			wantLive: "v0",
			wantCopy: copyAtDest,
		},
		{
			// The publish landed and the flag did not. Without the flag there is
			// no evidence the copy in backup was superseded, so it may not be
			// deleted; parking it is what keeps it out of the next pass's way
			// without dropping the last copy of a tree.
			id: "S5",
			arrange: func(t *testing.T, dir, dest string) {
				injectFault(t, "create", func(args ...string) bool {
					return filepath.Base(args[0]) == committedFile
				}, errors.New("injected commit flag failure"))
				if err := extractBundle(context.Background(), testBundle(t, "a.txt", "v1"), dest, nil); err != nil {
					t.Fatalf("a committed publish must be reported as success: %v", err)
				}
			},
			wantLive: "v1",
			wantCopy: copyAtKept,
			report1:  true,
		},
		{
			// The publish failed and the restore put the tree back, so the
			// directory holds no copy of anything: owned and empty.
			id: "S6",
			arrange: func(t *testing.T, dir, dest string) {
				injected := errors.New("injected publish failure")
				injectFault(t, "rename", func(args ...string) bool {
					return filepath.Base(args[0]) == "repo"
				}, injected)
				injectFault(t, "removeAll", nil, errors.New("injected cleanup failure"))
				if err := extractBundle(context.Background(), testBundle(t, "a.txt", "v1"), dest, nil); !errors.Is(err, injected) {
					t.Fatalf("extract = %v, want the injected publish failure", err)
				}
			},
			wantLive: "v0",
			wantCopy: copyDeleted,
		},
		{
			// The writer's own retain branch: publish failed, restore failed, the
			// copy was kept and named in the error. On disk this is S4, and
			// recovery must not tell them apart, because nothing on disk does.
			id:       "S7",
			arrange:  arrangeInterruptedSwap,
			wantLive: "v0",
			wantCopy: copyAtDest,
		},
		{
			// The flag is there, so the copy in backup is provably superseded by
			// the tree now live at the destination. This is the one and only
			// shape that licenses deleting a copy of a tree.
			id: "S8",
			arrange: func(t *testing.T, dir, dest string) {
				injectFault(t, "removeAll", nil, errors.New("injected cleanup failure"))
				if err := extractBundle(context.Background(), testBundle(t, "a.txt", "v1"), dest, nil); err != nil {
					t.Fatalf("a cleanup failure must be logged, not returned: %v", err)
				}
			},
			wantLive: "v1",
			wantCopy: copyDeleted,
		},
	}
}

func TestBundleCrashMatrix(t *testing.T) {
	for _, row := range bundleCrashRows() {
		t.Run(row.id, func(t *testing.T) {
			dir := t.TempDir()
			dest := filepath.Join(dir, "proj-1")
			if err := extractBundle(context.Background(), testBundle(t, "a.txt", "v0"), dest, nil); err != nil {
				t.Fatalf("seed extract: %v", err)
			}
			// The faults belong to the crash, not to recovery: restoring the
			// seam here is what makes the next lines a recovery over a real
			// filesystem rather than over a failing one.
			func() {
				saved := stagingFS
				defer func() { stagingFS = saved }()
				row.arrange(t, dir, dest)
			}()
			staging := soleStaging(t, dir)

			for pass, wantReport := range []bool{row.report1, row.report2} {
				var logged []string
				recoverBundleDir(dir, func(format string, args ...any) {
					logged = append(logged, fmt.Sprintf(format, args...))
				})
				assertBundleTerminalState(t, dir, dest, staging, row.wantCopy, row.wantLive, pass+1)
				if wantReport {
					assertCopyReported(t, logged, stagingSeqDigitsOf(t, staging), pass+1)
				}
			}
		})
	}
}

// assertBundleTerminalState asserts both halves of a row: the live tree, and the
// full set of copies still on disk under either prefix. The set is compared
// whole rather than one path at a time, so an extra copy nobody expected fails
// the row instead of going unnoticed.
func assertBundleTerminalState(t *testing.T, dir, dest, staging string, want copyDisposition, wantLive string, pass int) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dest, "a.txt"))
	switch {
	case wantLive == "":
		if err == nil {
			t.Errorf("pass %d: the destination should be absent, it holds %q", pass, got)
		}
	case err != nil || string(got) != wantLive:
		t.Errorf("pass %d: dest a.txt = %q (err %v), want %q", pass, got, err, wantLive)
	}

	var wantPaths []string
	switch want {
	case copyAtStaging, copyInPlaceUnowned:
		wantPaths = []string{staging}
	case copyAtKept:
		wantPaths = []string{parkedStaging(staging)}
	}
	assertCopySet(t, dir, []string{stagingPrefix, keptPrefix}, wantPaths, pass)

	switch want {
	case copyInPlaceUnowned:
		// Retained is not the whole claim: the reason it is retained is that
		// nothing on disk attributes it, and a row that stopped checking that
		// would pass over a copy recovery could have proved was its own.
		if _, err := readMarker(staging); !errors.Is(err, errMarkerMissing) {
			t.Errorf("pass %d: %s should carry no marker, readMarker = %v", pass, staging, err)
		}
	case copyAtDest:
		if _, err := os.Stat(dest); err != nil {
			t.Errorf("pass %d: the copy should have been restored to the destination: %v", pass, err)
		}
	}
}

// stagingSeqDigitsOf is the sequence a copy carries in its name. It survives a
// park, so a report assertion keyed on it holds whether the copy is still under
// the staging prefix or has moved under the Kept one.
func stagingSeqDigitsOf(t *testing.T, staging string) string {
	t.Helper()
	base := filepath.Base(staging)
	digits := strings.TrimSuffix(strings.TrimPrefix(base, stagingPrefix), stagingSeqSuffix)
	if len(digits) != stagingSeqDigits {
		t.Fatalf("staging name %q carries no sequence", base)
	}
	return digits
}

// A copy recovery keeps and never names is one an operator cannot find, so the
// report is half of every retain row rather than a nicety.
func assertCopyReported(t *testing.T, logged []string, seq string, pass int) {
	t.Helper()
	if !slices.ContainsFunc(logged, func(m string) bool { return strings.Contains(m, seq) }) {
		t.Errorf("pass %d: the retained copy (sequence %s) should be named in the report, got %v", pass, seq, logged)
	}
}

// assertCopySet compares every directory under the given prefixes against the
// paths the row expects, resolving both sides through EvalSymlinks because the
// temp root is a symlink on macOS and a path built from t.TempDir would then
// never equal one read back out of the directory.
func assertCopySet(t *testing.T, parent string, prefixes, want []string, pass int) {
	t.Helper()
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(entry.Name(), prefix) {
				got = append(got, resolvedPath(t, filepath.Join(parent, entry.Name())))
				break
			}
		}
	}
	wantResolved := make([]string, 0, len(want))
	for _, path := range want {
		wantResolved = append(wantResolved, resolvedPath(t, path))
	}
	slices.Sort(got)
	slices.Sort(wantResolved)
	if !slices.Equal(got, wantResolved) {
		t.Errorf("pass %d: copies on disk = %v, want %v", pass, got, wantResolved)
	}
}

// resolvedPath resolves symlinks so the two sides of a path comparison are the
// same path. A path that does not exist cannot be resolved, and its cleaned form
// is what the comparison then reports.
func resolvedPath(t *testing.T, path string) string {
	t.Helper()
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

// Ownership is a name plus a marker that agrees with it, and neither half alone.
// A directory that only carries the generated name is a legacy work tree or a
// sibling someone renamed, and recovery restoring from one, or reaping one, is
// the whole class of defect the marker exists to close. Each case here is a
// single check dropped: the marker, its kind, its sequence, its destination.
func TestOwnershipCannotBeForgedBySiblingNames(t *testing.T) {
	const forgedSeq = 7
	for _, tc := range []struct {
		name   string
		marker *txnMarker
	}{
		{name: "no marker at all"},
		{name: "another site's kind", marker: &txnMarker{Kind: "dictation-promote", Dest: "proj-1", Seq: forgedSeq}},
		{name: "sequence disagrees with the name", marker: &txnMarker{Kind: txnKindBundleExtract, Dest: "proj-1", Seq: forgedSeq + 1}},
		{name: "destination is not a link id", marker: &txnMarker{Kind: txnKindBundleExtract, Dest: ".hidden", Seq: forgedSeq}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// Built outside and renamed in, so nothing about it was ever written
			// by this code: the name is the only thing it shares with a staging
			// dir, and it is shaped like a restorable one.
			unrelated := filepath.Join(t.TempDir(), "unrelated")
			if err := os.MkdirAll(filepath.Join(unrelated, "backup", ".git"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(unrelated, "backup", "a.txt"), []byte("forged"), 0o644); err != nil {
				t.Fatal(err)
			}
			forged := filepath.Join(dir, fmt.Sprintf("%s%020d%s", stagingPrefix, forgedSeq, stagingSeqSuffix))
			if err := os.Rename(unrelated, forged); err != nil {
				t.Fatal(err)
			}
			if tc.marker != nil {
				if err := writeMarker(forged, *tc.marker); err != nil {
					t.Fatal(err)
				}
			}

			for pass := 1; pass <= 2; pass++ {
				recoverBundleDir(dir, nil)
				got, err := os.ReadFile(filepath.Join(forged, "backup", "a.txt"))
				if err != nil || string(got) != "forged" {
					t.Fatalf("pass %d: the forged directory must be left exactly as it was, got %q (err %v)", pass, got, err)
				}
				// A restore, a park, or a reap all show up here: any of them
				// either creates a destination or moves the directory.
				assertNoOtherEntries(t, dir, filepath.Base(forged), lockDirName)
			}
		})
	}
}

// assertNoOtherEntries fails when anything but the named entries is in dir. It
// is how "nothing was owned" is asserted without naming every way ownership
// could have been acted on: a restore, a park, and a reap all change this set.
func assertNoOtherEntries(t *testing.T, dir string, allowed ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !slices.Contains(allowed, entry.Name()) {
			t.Errorf("%s should hold only %v, found %s", dir, allowed, entry.Name())
		}
	}
}

// ---- the regression net: recovery's own steps, and a live extract ----------

// deleteWatch records every removeAll recovery asks for, and flags the ones no
// classification licenses: a directory that still holds a set-aside tree with no
// commit flag beside it. It is asserted AT THE SEAM rather than off the disk
// because a fault-injected removeAll leaves the tree exactly where a removeAll
// that was never requested does, so a row reading the surviving files could not
// tell a recovery that did the right thing from one that tried to do the wrong
// thing and was stopped by the injected fault.
type deleteWatch struct {
	mu     sync.Mutex
	all    []string
	denied []string
}

// install swaps removeAll for a recorder, restoring the seam in t.Cleanup. The
// order matters: only a watch installed AFTER a fault sees the calls that fault
// is failing, and cleanups unwind in reverse, so the seam still ends the test as
// the real filesystem.
func (w *deleteWatch) install(t *testing.T) {
	t.Helper()
	real := stagingFS
	t.Cleanup(func() { stagingFS = real })
	stagingFS.removeAll = func(path string) error {
		w.mu.Lock()
		w.all = append(w.all, path)
		if holdsUncommittedCopy(path) {
			w.denied = append(w.denied, path)
		}
		w.mu.Unlock()
		return real.removeAll(path)
	}
}

func (w *deleteWatch) counts() (all, denied []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.all), slices.Clone(w.denied)
}

// holdsUncommittedCopy reports whether path still holds a copy of a tree that
// nothing proves was published over. Recovery may park one of these forever; it
// may never delete one, whatever step just failed.
func holdsUncommittedCopy(path string) bool {
	if _, err := os.Stat(filepath.Join(path, "backup")); err != nil {
		return false
	}
	_, err := os.Stat(filepath.Join(path, committedFile))
	return os.IsNotExist(err)
}

// The watch is the whole regression net below, so it gets its own proof: a net
// that cannot see the delete it exists to catch would make every row under it
// pass for the wrong reason. The three shapes are the two deletes recovery is
// licensed to make and the one it is not.
func TestDeleteWatchSeesOnlyTheDeleteRecoveryMayNotMake(t *testing.T) {
	dir := t.TempDir()
	uncommitted := stageBackup(t, dir, "", "proj-1", "v1", 100)
	committed := stageBackup(t, dir, "", "proj-1", "v2", 200)
	markCommitted(t, committed)
	scratch := stageScratch(t, dir, "proj-1", 50)

	var watch deleteWatch
	func() {
		saved := stagingFS
		defer func() { stagingFS = saved }()
		watch.install(t)
		for _, path := range []string{committed, scratch, uncommitted} {
			if err := stagingFS.removeAll(path); err != nil {
				t.Fatal(err)
			}
		}
	}()

	all, denied := watch.counts()
	if len(all) != 3 {
		t.Errorf("the watch should have seen all three deletes, got %v", all)
	}
	if !slices.Equal(denied, []string{uncommitted}) {
		t.Errorf("only the uncommitted copy is a violation, got %v", denied)
	}
}

// recoveryStep is one filesystem step of a recovery pass, with the fault that
// fails it. Each is matched where it is reached rather than by call ordinal, so
// a change in how many times recovery stats a directory cannot silently move
// which call the row is failing.
type recoveryStep struct {
	name string
	// inject installs the row's fault and returns whether that fault was ever
	// actually called. The return is not bookkeeping: a row that injects a
	// failure into a step the pass never reaches asserts only that a clean run
	// works, which every other row already proves.
	inject func(t *testing.T, newest string) (fired func() bool)
}

// firedFault wraps a row's match so the row can report whether its fault was
// reached. A nil match means every call of the step, matching injectFault's own
// contract.
func firedFault(match func(args ...string) bool) (wrapped func(args ...string) bool, fired func() bool) {
	var hit atomic.Bool
	return func(args ...string) bool {
		if match != nil && !match(args...) {
			return false
		}
		hit.Store(true)
		return true
	}, func() bool { return hit.Load() }
}

func bundleRecoverySteps() []recoveryStep {
	failed := errors.New("injected recovery failure")
	simple := func(step string) func(*testing.T, string) func() bool {
		return func(t *testing.T, _ string) func() bool {
			match, fired := firedFault(nil)
			injectFault(t, step, match, failed)
			return fired
		}
	}
	return []recoveryStep{
		{name: "readDir", inject: simple("readDir")},
		{name: "stat", inject: simple("stat")},
		{name: "marker read", inject: simple("readFile")},
		{
			name: "restore rename",
			inject: func(t *testing.T, newest string) func() bool {
				match, fired := firedFault(func(args ...string) bool {
					return args[0] == filepath.Join(newest, "backup")
				})
				injectFault(t, "rename", match, failed)
				return fired
			},
		},
		{name: "removeAll", inject: simple("removeAll")},
		{
			name: "park rename",
			inject: func(t *testing.T, _ string) func() bool {
				match, fired := firedFault(func(args ...string) bool {
					return strings.HasPrefix(filepath.Base(args[1]), keptPrefix)
				})
				injectFault(t, "rename", match, failed)
				return fired
			},
		},
	}
}

// Every recovery step, failed on pass one, on pass two, and on both. Two claims
// per case, and the first is the one the whole branch exists for: no failure of
// any step ever produces a delete of a copy nothing proves was superseded. The
// second is that a fault only DELAYS recovery: a later pass with the fault gone
// reaches the same terminal state a clean run reaches, so a step that failed
// once has not left the destination in a state recovery can no longer reason
// about.
func TestBundleRecoveryStepFailures(t *testing.T) {
	for _, step := range bundleRecoverySteps() {
		// A standalone "pass two" variant used to sit between these two. It ran
		// pass one clean, which reaches the terminal state, so pass two had no
		// candidate left and the fault it injected was never called: the row
		// asserted a clean run and nothing else. The both-passes row is the one
		// that exercises a fault on pass two, because pass one fails first and
		// leaves work behind for it.
		for _, when := range []struct {
			name   string
			faulty [2]bool
		}{
			{name: "pass one", faulty: [2]bool{true, false}},
			{name: "both passes", faulty: [2]bool{true, true}},
		} {
			t.Run(step.name+"/"+when.name, func(t *testing.T) {
				dir := t.TempDir()
				dest := filepath.Join(dir, "proj-1")
				// Two uncommitted usable copies and one owned scratch dir, with
				// the destination gone: the state that drives every step in the
				// list through a real decision. One copy is restored, one is
				// parked, and the scratch is the only delete on the happy path,
				// so a step that fails has something to corrupt.
				older := stageBackup(t, dir, "", "proj-1", "v1", 100)
				newest := stageBackup(t, dir, "", "proj-1", "v2", 200)
				scratch := stageScratch(t, dir, "proj-1", 50)

				var watch deleteWatch
				pass := func(faulty bool) func() bool {
					saved := stagingFS
					defer func() { stagingFS = saved }()
					fired := func() bool { return true }
					if faulty {
						fired = step.inject(t, newest)
					}
					// After the fault, so the watch sees the calls the fault
					// is failing rather than only the ones it lets through.
					watch.install(t)
					recoverBundleDir(dir, discardLog)
					return fired
				}
				firedOne := pass(when.faulty[0])
				if when.faulty[0] {
					if !firedOne() {
						// The pass never called the step this row fails, so the
						// row ran a clean recovery under a fault that does not
						// exist and every assertion below it is vacuous.
						t.Fatalf("the injected %s failure was never called on pass one, so this row proves nothing", step.name)
					}
					if bundleStepTerminal(dir, dest, older, newest, scratch) {
						// Same defect from the other side: a fault that was
						// called and changed nothing left the pass free to
						// finish.
						t.Errorf("the injected %s failure left the pass free to finish, so this row proves nothing", step.name)
					}
				}
				firedTwo := pass(when.faulty[1])
				if when.faulty[1] && !firedTwo() {
					t.Fatalf("the injected %s failure was never called on pass two, so this row proves nothing", step.name)
				}
				// The fault is gone, and recovery has to be able to finish from
				// wherever the failed passes left the directory.
				pass(false)

				all, denied := watch.counts()
				if len(denied) > 0 {
					t.Errorf("no step failure may produce a delete of an uncommitted copy, got %v", denied)
				}
				if len(all) == 0 {
					t.Fatal("the watch saw no delete at all, so it proves nothing about this pass")
				}
				assertBundleStepTerminalState(t, dir, dest, older, newest, scratch)
			})
		}
	}
}

// bundleStepTerminal reports whether the fixture reached the state a clean run
// reaches. It is a predicate rather than an assertion because it is asked twice
// for opposite reasons: a faulted pass must NOT have got here, and the pass
// after the fault is gone must have.
func bundleStepTerminal(dir, dest, older, newest, scratch string) bool {
	got, err := os.ReadFile(filepath.Join(dest, "a.txt"))
	if err != nil || string(got) != "v2" {
		return false
	}
	if _, err := os.Stat(filepath.Join(parkedStaging(older), "backup", "a.txt")); err != nil {
		return false
	}
	for _, gone := range []string{newest, scratch} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			return false
		}
	}
	return true
}

// assertBundleStepTerminalState is where the fixture above has to end up once
// nothing is failing: the newest copy live, its own directory gone, the older
// one kept under the Kept prefix with its tree intact, and the scratch dir
// reaped. Content is read back rather than names checked, because a park that
// moved a name and lost the tree passes every name assertion.
func assertBundleStepTerminalState(t *testing.T, dir, dest, older, newest, scratch string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dest, "a.txt"))
	if err != nil || string(got) != "v2" {
		t.Errorf("dest a.txt = %q (err %v), want the newest copy %q", got, err, "v2")
	}
	parked := parkedStaging(older)
	kept, err := os.ReadFile(filepath.Join(parked, "backup", "a.txt"))
	if err != nil || string(kept) != "v1" {
		t.Errorf("the parked copy's a.txt = %q (err %v), want %q", kept, err, "v1")
	}
	for _, gone := range []string{newest, scratch} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s should be gone once recovery finished: %v", gone, err)
		}
	}
	assertCopySet(t, dir, []string{stagingPrefix, keptPrefix}, []string{parked}, 3)
}

// gateAt parks the write path at one named boundary: it signals when the call
// whose arguments match is reached, holds that call until release, and passes
// every other call through. blockStep gates EVERY call to a step, which for a
// rename would park an extract at the rename inside its marker write rather than
// at the set-aside or the publish, so a matched gate is what puts the writer at
// the boundary a row is actually about.
func gateAt(t *testing.T, match func(from, to string) bool) (reached <-chan struct{}, release func()) {
	t.Helper()
	real := stagingFS
	t.Cleanup(func() { stagingFS = real })
	arrived := make(chan struct{})
	gate := make(chan struct{})
	var arrive, open sync.Once
	release = func() { open.Do(func() { close(gate) }) }
	t.Cleanup(release)
	stagingFS.rename = func(from, to string) error {
		if match(from, to) {
			arrive.Do(func() { close(arrived) })
			<-gate
		}
		return real.rename(from, to)
	}
	return arrived, release
}

// signalRemoveAll reports when a removeAll of path is reached. It wraps whatever
// is already installed, so layering it over blockStep signals BEFORE the call
// parks rather than after it is released.
func signalRemoveAll(t *testing.T, match func(path string) bool) <-chan struct{} {
	t.Helper()
	installed := stagingFS
	t.Cleanup(func() { stagingFS = installed })
	arrived := make(chan struct{})
	var arrive sync.Once
	stagingFS.removeAll = func(p string) error {
		if match(p) {
			arrive.Do(func() { close(arrived) })
		}
		return installed.removeAll(p)
	}
	return arrived
}

// A recovery pass and a live extract for one destination, running at the same
// time, at each of the three boundaries where the destination's only copy is in
// a staging dir. Nothing here asserts that recovery wins: the claim is that
// neither side destroys the other's copy, that the extract that was already
// running completes, and that the recovering side either waits or says out loud
// that it skipped. The lock is what makes that true, and this is the only test
// that drives both sides of it at once, under -race, through one package-level
// seam.
func TestBundleRecoveryRacesALivePromotion(t *testing.T) {
	for _, tc := range []struct {
		name string
		// gate parks the extract at this row's boundary and reports when it is
		// there. staging is the dir the extract allocated, known only once the
		// extract is under way for the reap row, which is why this takes dir.
		gate func(t *testing.T, dir, dest string) (reached <-chan struct{}, release func())
	}{
		{
			// The prior tree is on its way into the staging dir: for that
			// instant the destination still holds it and the staging dir holds
			// nothing.
			name: "set-aside",
			gate: func(t *testing.T, dir, dest string) (<-chan struct{}, func()) {
				return gateAt(t, func(from, to string) bool {
					return from == dest && filepath.Base(to) == "backup"
				})
			},
		},
		{
			// The destination is absent and the staging dir holds the only copy
			// of the prior tree. A recovering pass that acted here would restore
			// that copy over the publish this extract is in the middle of.
			name: "publish",
			gate: func(t *testing.T, dir, dest string) (<-chan struct{}, func()) {
				return gateAt(t, func(from, to string) bool {
					return filepath.Base(from) == "repo" && to == dest
				})
			},
		},
		{
			// The publish landed and the commit flag is written, so the copy in
			// the staging dir is superseded and the extract is deleting it. A
			// recovering pass that reached the same directory would be deleting
			// it too.
			name: "reap",
			gate: func(t *testing.T, dir, dest string) (<-chan struct{}, func()) {
				release := blockStep(t, "removeAll")
				// Any staging dir under dir: the extract's reap is the only
				// removeAll either side makes on this path, since the seed
				// extract finished before the gate was installed.
				return signalRemoveAll(t, func(path string) bool {
					return filepath.Dir(path) == dir && strings.HasPrefix(filepath.Base(path), stagingPrefix)
				}), release
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dest := filepath.Join(dir, "proj-1")
			if err := extractBundle(context.Background(), testBundle(t, "a.txt", "v0"), dest, nil); err != nil {
				t.Fatalf("seed extract: %v", err)
			}
			// Built before the seam is gated: git writes it through os, but a
			// gate installed around a step this still reaches would park the
			// fixture rather than the extract under test.
			bundle := testBundle(t, "a.txt", "v1")

			var watch deleteWatch
			watch.install(t)
			reached, release := tc.gate(t, dir, dest)

			var extractErr error
			extracted := make(chan struct{})
			go func() {
				defer close(extracted)
				extractErr = extractBundle(context.Background(), bundle, dest, nil)
			}()
			<-reached

			var logs []string
			recovered := make(chan struct{})
			go func() {
				defer close(recovered)
				var mu sync.Mutex
				recoverBundleDir(dir, func(format string, args ...any) {
					mu.Lock()
					defer mu.Unlock()
					logs = append(logs, fmt.Sprintf(format, args...))
				})
			}()
			<-recovered
			release()
			<-extracted

			if extractErr != nil {
				t.Errorf("the extract that was already running must finish: %v", extractErr)
			}
			// Skipped, not raced: the extract holds the destination's lock from
			// before its first rename until after its last delete, and the
			// recovering pass is required to say so rather than act.
			if !logged(logs, "holds proj-1") {
				t.Errorf("the recovering pass should report the destination it skipped, got %v", logs)
			}
			got, err := os.ReadFile(filepath.Join(dest, "a.txt"))
			if err != nil || string(got) != "v1" {
				t.Errorf("dest a.txt = %q (err %v), want the extract's own tree %q", got, err, "v1")
			}
			if _, denied := watch.counts(); len(denied) > 0 {
				t.Errorf("neither side may delete an uncommitted copy, got %v", denied)
			}
			// The extract cleaned up after itself, and recovery left nothing of
			// its own behind.
			assertCopySet(t, dir, []string{stagingPrefix, keptPrefix}, nil, 1)
		})
	}
}

// ---- the rows no single crash of the writer can reach ----------------------

// The X rows of the Acceptance Examples. They need arrangements the six crash
// shapes cannot produce: a held lock, more than one candidate, a destination
// that exists and cannot serve, a restore that fails, or a legacy directory
// planted by hand. Everything else about them is the crash table's contract:
// the whole set of copies under both prefixes is compared, not one path at a
// time, and every row runs twice because recovery keeps no memory.

// xFile is the destination after a pass. An empty name means it must be absent,
// which is a real terminal state here and not a missing expectation.
type xFile struct {
	name    string
	content string
}

// xCopy is one copy and where it must be when the pass ends. final is written
// out rather than derived from a disposition, because these rows retain copies
// under three different names (its own, the Kept one, a freshly allocated one)
// and a derived name would encode the test's guess at the rule the row exists to
// check. An empty final is a delete.
type xCopy struct {
	final   string
	file    string
	content string
}

type xWant struct {
	live   xFile
	copies []xCopy
	// report is the fragments this pass's report must name. A copy recovery
	// keeps and never names is one an operator cannot find.
	report []string
}

type bundleXRow struct {
	id string
	// arrange plants the state and returns what each pass must produce, plus,
	// where the row needs one, the fault or held lock that makes the row's
	// situation happen during a given pass. They come back together so both can
	// close over the paths the arrangement planted.
	arrange func(t *testing.T, dir, dest string) (want func(pass int) xWant, impede func(t *testing.T, pass int) func())
}

func stagingNamed(dir string, seq int64) string {
	return filepath.Join(dir, fmt.Sprintf("%s%020d%s", stagingPrefix, seq, stagingSeqSuffix))
}

func keptNamed(dir string, seq int64) string {
	return filepath.Join(dir, fmt.Sprintf("%s%020d%s", keptPrefix, seq, stagingSeqSuffix))
}

// bothPasses is the common case: a row whose two passes look identical, because
// pass one reached a terminal state and pass two has nothing left to do.
func bothPasses(w xWant) func(int) xWant {
	return func(int) xWant { return w }
}

// faultDuring installs one fault for the length of a pass and returns the undo.
// The pass-scoped restore is the point: a row that fails a step on pass one only
// needs the seam back before pass two runs.
func faultDuring(t *testing.T, step string, match func(args ...string) bool) func() {
	t.Helper()
	saved := stagingFS
	injectFault(t, step, match, errors.New("injected recovery failure"))
	return func() { stagingFS = saved }
}

func bundleXRows() []bundleXRow {
	return []bundleXRow{
		{
			// A destination that exists and cannot serve is not a reason to keep
			// a usable copy out of it, and the husk is not this code's to delete
			// either: it moves into a fresh sequenced directory of its own and
			// is parked from there.
			id: "X1",
			arrange: func(t *testing.T, dir, dest string) (func(int) xWant, func(*testing.T, int) func()) {
				stageBackup(t, dir, "", "proj-1", "v1", 100)
				plantUnusableDest(t, dir, "proj-1", "husk")
				return func(pass int) xWant {
					w := xWant{
						live:   xFile{"a.txt", "v1"},
						copies: []xCopy{{final: keptNamed(dir, 101), file: filepath.Join("backup", "husk.txt"), content: "husk"}},
					}
					if pass == 1 {
						w.report = []string{"keeping the tree that could not serve"}
					}
					return w
				}, nil
			},
		},
		{
			// A Kept backup is permanent. A later publish is not evidence about
			// it: the commit flag goes into the copy the publishing transaction
			// set aside, and a Kept backup is never that copy.
			id: "X2",
			arrange: func(t *testing.T, dir, dest string) (func(int) xWant, func(*testing.T, int) func()) {
				plantUsableDest(t, dir, "proj-1", "live")
				kept := plantKeptBackup(t, dir, "proj-1", "unproven", 500)
				return bothPasses(xWant{
					live:   xFile{"a.txt", "live"},
					copies: []xCopy{{final: kept, file: filepath.Join("backup", "a.txt"), content: "unproven"}},
				}), nil
			},
		},
		{
			// The newest usable copy cannot be put back. Falling through to the
			// older one would put a tree at the destination that the next pass
			// reads as having published over the newer copy, which is how a
			// retained copy turns into a deleted one two passes later.
			id: "X3",
			arrange: func(t *testing.T, dir, dest string) (func(int) xWant, func(*testing.T, int) func()) {
				older := stageBackup(t, dir, "", "proj-1", "v1", 100)
				newest := stageBackup(t, dir, "", "proj-1", "v2", 200)
				want := bothPasses(xWant{
					copies: []xCopy{
						{final: older, file: filepath.Join("backup", "a.txt"), content: "v1"},
						{final: newest, file: filepath.Join("backup", "a.txt"), content: "v2"},
					},
					report: []string{newest},
				})
				return want, func(t *testing.T, pass int) func() {
					return faultDuring(t, "rename", func(args ...string) bool {
						return args[0] == filepath.Join(newest, "backup")
					})
				}
			},
		},
		{
			// Unusable is a selection decision, not a delete: the newest copy is
			// skipped for one that can serve and is then kept like any other
			// copy nothing proves was superseded.
			id: "X4",
			arrange: func(t *testing.T, dir, dest string) (func(int) xWant, func(*testing.T, int) func()) {
				stageBackup(t, dir, "", "proj-1", "v1", 100)
				newest := stageBackup(t, dir, "", "proj-1", "v2", 200)
				if err := os.RemoveAll(filepath.Join(newest, "backup", ".git")); err != nil {
					t.Fatal(err)
				}
				return func(pass int) xWant {
					w := xWant{
						live:   xFile{"a.txt", "v1"},
						copies: []xCopy{{final: parkedStaging(newest), file: filepath.Join("backup", "a.txt"), content: "v2"}},
					}
					if pass == 1 {
						w.report = []string{newest}
					}
					return w
				}, nil
			},
		},
		{
			// Unreadable is not unusable. A filesystem fault says nothing about
			// the copy, so the whole destination stops rather than ruling on the
			// copies that could be read while the newest one could not.
			id: "X5",
			arrange: func(t *testing.T, dir, dest string) (func(int) xWant, func(*testing.T, int) func()) {
				cand := stageBackup(t, dir, "", "proj-1", "v1", 100)
				want := bothPasses(xWant{
					copies: []xCopy{{final: cand, file: filepath.Join("backup", "a.txt"), content: "v1"}},
					report: []string{cand},
				})
				return want, func(t *testing.T, pass int) func() {
					return faultDuring(t, "stat", func(args ...string) bool {
						return args[0] == filepath.Join(cand, "backup")
					})
				}
			},
		},
		{
			// A destination someone else holds is one recovery knows nothing
			// about: on disk a live extract mid-swap and a crashed one are the
			// same thing, and the lock is the only thing that tells them apart.
			id: "X6",
			arrange: func(t *testing.T, dir, dest string) (func(int) xWant, func(*testing.T, int) func()) {
				cand := stageBackup(t, dir, "", "proj-1", "v1", 100)
				want := func(pass int) xWant {
					if pass == 1 {
						return xWant{
							copies: []xCopy{{final: cand, file: filepath.Join("backup", "a.txt"), content: "v1"}},
							report: []string{"holds proj-1"},
						}
					}
					return xWant{live: xFile{"a.txt", "v1"}}
				}
				return want, func(t *testing.T, pass int) func() {
					if pass != 1 {
						return func() {}
					}
					return lockExtract(dest)
				}
			},
		},
		{
			// A published work tree can sit under the exact generated name, so
			// the marker beside it proves nothing: the work-tree veto runs first
			// and the directory is left alone whatever the marker says.
			id: "X7",
			arrange: func(t *testing.T, dir, dest string) (func(int) xWant, func(*testing.T, int) func()) {
				legacy := stagingNamed(dir, 300)
				if err := os.MkdirAll(filepath.Join(legacy, ".git"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(legacy, "a.txt"), []byte("legacy"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := writeMarker(legacy, txnMarker{Kind: txnKindBundleExtract, Dest: "proj-1", Seq: 300}); err != nil {
					t.Fatal(err)
				}
				return bothPasses(xWant{
					copies: []xCopy{{final: legacy, file: "a.txt", content: "legacy"}},
					report: []string{"holds a work tree"},
				}), nil
			},
		},
		{
			// A name that shares the prefix and fails the grammar carries no
			// order at all, so it is not a candidate for anything: not restored,
			// not parked, not deleted. It is still reported, because at this
			// site a dot-prefixed sibling nothing attributes is residue an
			// operator has to be able to see.
			id: "X8",
			arrange: func(t *testing.T, dir, dest string) (func(int) xWant, func(*testing.T, int) func()) {
				sibling := stageBackup(t, dir, "foo", "proj-1", "sib", 0)
				return bothPasses(xWant{
					copies: []xCopy{{final: sibling, file: filepath.Join("backup", "a.txt"), content: "sib"}},
					report: []string{"does not carry a name this code writes"},
				}), nil
			},
		},
		{
			// The destination went away outside this code and the only copy left
			// carries a commit flag. Committed is second in selection, never
			// excluded from it: a copy that was superseded by a tree that is no
			// longer there is still a copy of something.
			id: "X9",
			arrange: func(t *testing.T, dir, dest string) (func(int) xWant, func(*testing.T, int) func()) {
				markCommitted(t, stageBackup(t, dir, "", "proj-1", "v1", 100))
				return bothPasses(xWant{live: xFile{"a.txt", "v1"}}), nil
			},
		},
		{
			// Two copies, one destination: the newest goes back and the older is
			// kept rather than deleted, because nothing proves anything
			// published over it either.
			id: "X10",
			arrange: func(t *testing.T, dir, dest string) (func(int) xWant, func(*testing.T, int) func()) {
				older := stageBackup(t, dir, "", "proj-1", "v1", 100)
				stageBackup(t, dir, "", "proj-1", "v2", 200)
				return func(pass int) xWant {
					w := xWant{
						live:   xFile{"a.txt", "v2"},
						copies: []xCopy{{final: parkedStaging(older), file: filepath.Join("backup", "a.txt"), content: "v1"}},
					}
					if pass == 1 {
						w.report = []string{older}
					}
					return w
				}, nil
			},
		},
		{
			// v0.8.0 staged under os.MkdirTemp, whose suffix carries no sequence
			// at all, and a hand-planted name can fail the grammar in the other
			// direction. Neither is orderable, so both are retained in place and
			// reported rather than guessed at.
			id: "X11",
			arrange: func(t *testing.T, dir, dest string) (func(int) xWant, func(*testing.T, int) func()) {
				residue := stageBackup(t, dir, "3921749", "proj-1", "resid", 0)
				tied := stageBackup(t, dir, "0000000000000000000x"+stagingSeqSuffix, "proj-1", "tied", 0)
				return bothPasses(xWant{
					copies: []xCopy{
						{final: residue, file: filepath.Join("backup", "a.txt"), content: "resid"},
						{final: tied, file: filepath.Join("backup", "a.txt"), content: "tied"},
					},
					report: []string{"does not carry a name this code writes"},
				}), nil
			},
		},
		{
			// Same as X1 with the only candidate committed. The husk still moves
			// aside first, and the copy that replaces it is the committed one,
			// because there is no uncommitted copy to prefer.
			id: "X12",
			arrange: func(t *testing.T, dir, dest string) (func(int) xWant, func(*testing.T, int) func()) {
				markCommitted(t, stageBackup(t, dir, "", "proj-1", "v1", 100))
				plantUnusableDest(t, dir, "proj-1", "husk")
				return func(pass int) xWant {
					w := xWant{
						live:   xFile{"a.txt", "v1"},
						copies: []xCopy{{final: keptNamed(dir, 101), file: filepath.Join("backup", "husk.txt"), content: "husk"}},
					}
					if pass == 1 {
						w.report = []string{"keeping the tree that could not serve"}
					}
					return w
				}, nil
			},
		},
		{
			// Nothing beside it can replace the husk, so it is not taken apart:
			// an operator with an unusable destination has strictly more than
			// one with no destination at all.
			id: "X13",
			arrange: func(t *testing.T, dir, dest string) (func(int) xWant, func(*testing.T, int) func()) {
				cand := stageBackup(t, dir, "", "proj-1", "v1", 100)
				if err := os.RemoveAll(filepath.Join(cand, "backup", ".git")); err != nil {
					t.Fatal(err)
				}
				plantUnusableDest(t, dir, "proj-1", "husk")
				return func(pass int) xWant {
					w := xWant{
						live:   xFile{"husk.txt", "husk"},
						copies: []xCopy{{final: parkedStaging(cand), file: filepath.Join("backup", "a.txt"), content: "v1"}},
					}
					if pass == 1 {
						w.report = []string{"no usable staged copy"}
					}
					return w
				}, nil
			},
		},
		{
			// The husk was already aside when the restore failed, so recovery
			// owes the destination its husk back: the row's whole claim is that
			// the destination ends the pass exactly as it was found, with the
			// candidate still where it was and the set-aside directory, now
			// empty, gone.
			id: "X14",
			arrange: func(t *testing.T, dir, dest string) (func(int) xWant, func(*testing.T, int) func()) {
				cand := stageBackup(t, dir, "", "proj-1", "v1", 100)
				plantUnusableDest(t, dir, "proj-1", "husk")
				want := bothPasses(xWant{
					live:   xFile{"husk.txt", "husk"},
					copies: []xCopy{{final: cand, file: filepath.Join("backup", "a.txt"), content: "v1"}},
					report: []string{cand},
				})
				return want, func(t *testing.T, pass int) func() {
					return faultDuring(t, "rename", func(args ...string) bool {
						return args[0] == filepath.Join(cand, "backup") && args[1] == dest
					})
				}
			},
		},
	}
}

func TestBundleXMatrix(t *testing.T) {
	for _, row := range bundleXRows() {
		t.Run(row.id, func(t *testing.T) {
			dir := t.TempDir()
			dest := filepath.Join(dir, "proj-1")
			want, impede := row.arrange(t, dir, dest)

			var watch deleteWatch
			for pass := 1; pass <= 2; pass++ {
				var logs []string
				func() {
					saved := stagingFS
					defer func() { stagingFS = saved }()
					if impede != nil {
						defer impede(t, pass)()
					}
					// After the impediment, so a delete the injected fault
					// would have failed is still seen as a delete that was
					// asked for.
					watch.install(t)
					logs = recoverAndLog(t, dir)
				}()
				assertBundleXState(t, dir, dest, want(pass), logs, pass)
			}
			if _, denied := watch.counts(); len(denied) > 0 {
				t.Errorf("no row may delete a copy nothing proves was superseded, got %v", denied)
			}
		})
	}
}

// assertBundleXState asserts a row's whole terminal state: the destination, the
// content of every copy at the name it is supposed to be at, the full set of
// directories under both prefixes, and the report. Content rather than names
// alone, because a park that moved a name and lost the tree passes every name
// assertion there is.
func assertBundleXState(t *testing.T, dir, dest string, want xWant, logs []string, pass int) {
	t.Helper()
	if want.live.name == "" {
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Errorf("pass %d: %s should be absent: %v", pass, dest, err)
		}
	} else {
		got, err := os.ReadFile(filepath.Join(dest, want.live.name))
		if err != nil || string(got) != want.live.content {
			t.Errorf("pass %d: dest %s = %q (err %v), want %q", pass, want.live.name, got, err, want.live.content)
		}
	}
	var paths []string
	for _, c := range want.copies {
		if c.final == "" {
			continue
		}
		paths = append(paths, c.final)
		got, err := os.ReadFile(filepath.Join(c.final, c.file))
		if err != nil || string(got) != c.content {
			t.Errorf("pass %d: %s = %q (err %v), want %q", pass, filepath.Join(c.final, c.file), got, err, c.content)
		}
	}
	assertCopySet(t, dir, []string{stagingPrefix, keptPrefix}, paths, pass)
	for _, fragment := range want.report {
		if !logged(logs, fragment) {
			t.Errorf("pass %d: the report should name %q, got %v", pass, fragment, logs)
		}
	}
}
