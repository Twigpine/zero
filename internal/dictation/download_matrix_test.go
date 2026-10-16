package dictation

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
	"time"
)

// The crash rows of the recovery plan's Acceptance Examples, as code. Each row
// drives the REAL promotion into the on-disk state a stop at one step leaves,
// then runs recovery twice and asserts both halves the table asserts: which
// install is live, and where every other copy of an install ended up. Nothing
// here plants a holder by hand, so the fixture is whatever the write path
// actually writes.
//
// A second pass is asserted because recovery keeps no memory: a copy retained on
// pass one must not be reclassified on pass two, and a copy deleted on pass one
// must not come back.

// copyDisposition is where one copy of an install ends up. The five values are
// the only terminal states the table uses, so a row that means "retained" has to
// say which kind of retention, and a delete can never be written down as
// anything else.
type copyDisposition int

const (
	// copyDeleted: gone from disk, under neither prefix.
	copyDeleted copyDisposition = iota
	// copyAtHolder: still under the name the promotion allocated for it.
	copyAtHolder
	// copyAtKept: moved under the Kept prefix, where the scan does not look.
	copyAtKept
	// copyInPlaceUnowned: still at its name and carrying no marker, so nothing
	// on disk attributes it to this code.
	copyInPlaceUnowned
	// copyAtDest: restored, so the copy's content is the live install and its
	// holder is gone.
	copyAtDest
)

// dictationCrashRow is one row of the table. Each of these rows leaves exactly
// one holder, and the assertion compares the WHOLE set of copies beside the
// destination against that one, so a row can never pass while a second copy
// nobody expected sits beside it.
type dictationCrashRow struct {
	id string
	// arrange runs the real promotion with faults injected to stop it at one
	// step. The faults are unwound before recovery runs, so recovery sees a
	// crashed install and not a failing filesystem.
	arrange  func(t *testing.T, txn *destTxn, stage, dest string)
	wantLive string
	wantCopy copyDisposition
	// report1 and report2 assert the copy is named in the recovery report on
	// that pass. A copy recovery retains but never names is one an operator
	// cannot find, which the table counts as a failure of the row.
	report1 bool
	report2 bool
}

// arrangeInterruptedPromotion stops the promotion between its two renames: the
// previous install is aside in the holder, the publish failed, and the restore
// failed too, so the destination is absent and the holder has the only copy of
// it. D2 and D5 differ in how the writer got here and are identical on disk,
// which is why they share one arrangement.
func arrangeInterruptedPromotion(t *testing.T, txn *destTxn, stage, dest string) {
	t.Helper()
	injectFault(t, "rename", func(args ...string) bool {
		return filepath.Base(args[1]) == filepath.Base(dest)
	}, errors.New("injected publish and restore failure"))
	if err := promoteStagedDir(txn, stage, dest, "engine", nil); err == nil {
		t.Fatal("a promotion whose publish and restore both fail must report an error")
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatalf("the interrupted promotion should leave %s absent, got %v", dest, err)
	}
}

func dictationCrashRows() []dictationCrashRow {
	return []dictationCrashRow{
		{
			// A stop between the holder and its marker leaves a directory
			// nothing on disk attributes. Deleting it would need proof this code
			// wrote it, and the marker is that proof, so it is retained and
			// named rather than reaped on the strength of its name.
			id: "D1",
			arrange: func(t *testing.T, txn *destTxn, stage, dest string) {
				injectFault(t, "createTemp", func(args ...string) bool {
					return args[1] == holderMarkerFile+"-*"
				}, errors.New("injected marker temp failure"))
				injectFault(t, "removeAll", nil, errors.New("injected cleanup failure"))
				if err := promoteStagedDir(txn, stage, dest, "engine", nil); err == nil {
					t.Fatal("a promotion whose marker cannot be written must fail")
				}
			},
			wantLive: "old",
			wantCopy: copyInPlaceUnowned,
			report1:  true,
			report2:  true,
		},
		{
			// The marker landed and the set-aside did not, so the holder is
			// owned and holds no copy of any install. Nothing can be lost by
			// removing it, and leaving it is what accumulates scratch forever.
			id: "D1b",
			arrange: func(t *testing.T, txn *destTxn, stage, dest string) {
				injected := errors.New("injected set-aside failure")
				injectFault(t, "rename", func(args ...string) bool {
					return filepath.Base(args[1]) == "install"
				}, injected)
				injectFault(t, "removeAll", nil, errors.New("injected cleanup failure"))
				if err := promoteStagedDir(txn, stage, dest, "engine", nil); !errors.Is(err, injected) {
					t.Fatalf("promote = %v, want the injected set-aside failure", err)
				}
			},
			wantLive: "old",
			wantCopy: copyDeleted,
		},
		{
			// The previous install is aside and the destination is absent, so
			// the holder has the only copy of it and it has to come back. An
			// offline caller has no download to fall back on.
			id:       "D2",
			arrange:  arrangeInterruptedPromotion,
			wantLive: "old",
			wantCopy: copyAtDest,
		},
		{
			// The publish landed and the flag did not. Without the flag there is
			// no evidence the copy in the holder was superseded, so it may not
			// be deleted; parking it is what keeps it out of the next pass's way
			// without dropping the last copy of an install.
			id: "D3",
			arrange: func(t *testing.T, txn *destTxn, stage, dest string) {
				injectFault(t, "create", func(args ...string) bool {
					return filepath.Base(args[0]) == committedFile
				}, errors.New("injected commit flag failure"))
				if err := promoteStagedDir(txn, stage, dest, "engine", nil); err != nil {
					t.Fatalf("a published install must be reported as success: %v", err)
				}
			},
			wantLive: "new",
			wantCopy: copyAtKept,
			report1:  true,
		},
		{
			// The publish failed and the restore put the install back, so the
			// holder holds no copy of anything: owned and empty.
			id: "D4",
			arrange: func(t *testing.T, txn *destTxn, stage, dest string) {
				injected := errors.New("injected publish failure")
				injectFault(t, "rename", func(args ...string) bool {
					return filepath.Base(args[0]) == filepath.Base(stage)
				}, injected)
				injectFault(t, "removeAll", nil, errors.New("injected cleanup failure"))
				if err := promoteStagedDir(txn, stage, dest, "engine", nil); !errors.Is(err, injected) {
					t.Fatalf("promote = %v, want the injected publish failure", err)
				}
			},
			wantLive: "old",
			wantCopy: copyDeleted,
		},
		{
			// The writer's own retain branch: publish failed, restore failed,
			// the copy was kept and named in the error. On disk this is D2, and
			// recovery must not tell them apart, because nothing on disk does.
			id:       "D5",
			arrange:  arrangeInterruptedPromotion,
			wantLive: "old",
			wantCopy: copyAtDest,
		},
		{
			// The flag is there, so the copy in the holder is provably
			// superseded by the install now live at the destination. This is the
			// one and only shape that licenses deleting a copy of an install.
			id: "D6",
			arrange: func(t *testing.T, txn *destTxn, stage, dest string) {
				injectFault(t, "removeAll", nil, errors.New("injected cleanup failure"))
				if err := promoteStagedDir(txn, stage, dest, "engine", nil); err != nil {
					t.Fatalf("a cleanup failure must not fail the install: %v", err)
				}
			},
			wantLive: "new",
			wantCopy: copyDeleted,
		},
	}
}

func TestDictationCrashMatrix(t *testing.T) {
	for _, row := range dictationCrashRows() {
		t.Run(row.id, func(t *testing.T) {
			root := t.TempDir()
			dest := filepath.Join(root, "engine-dir")
			stagedTree(t, dest, "old")
			// One handle for the whole row: the lock is per open file
			// description, so a second acquire in this process would contend
			// with the first rather than nest.
			txn := lockFor(t, dest)
			stage := stagedTree(t, filepath.Join(root, "stage"), "new")
			// The faults belong to the crash, not to recovery: restoring the
			// seam here is what makes the next lines a recovery over a real
			// filesystem rather than over a failing one.
			func() {
				saved := holderFS
				defer func() { holderFS = saved }()
				row.arrange(t, txn, stage, dest)
			}()
			holder := soleHolder(t, dest)

			for pass, wantReport := range []bool{row.report1, row.report2} {
				var reported []string
				restoreInterruptedPromotion(txn, dest, testPublished, func(m string) {
					reported = append(reported, m)
				})
				assertDictationTerminalState(t, dest, holder, row.wantCopy, row.wantLive, pass+1)
				if wantReport {
					assertCopyReported(t, reported, holderSeqDigitsOf(t, dest, holder), pass+1)
				}
			}
		})
	}
}

// assertDictationTerminalState asserts both halves of a row: the live install,
// and the full set of copies still beside the destination under either prefix.
// The set is compared whole rather than one path at a time, so an extra copy
// nobody expected fails the row instead of going unnoticed.
func assertDictationTerminalState(t *testing.T, dest, holder string, want copyDisposition, wantLive string, pass int) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dest, "engine"))
	switch {
	case wantLive == "":
		if err == nil {
			t.Errorf("pass %d: the destination should be absent, it holds %q", pass, got)
		}
	case err != nil || string(got) != wantLive:
		t.Errorf("pass %d: dest engine = %q (err %v), want %q", pass, got, err, wantLive)
	}

	var wantPaths []string
	switch want {
	case copyAtHolder, copyInPlaceUnowned:
		wantPaths = []string{holder}
	case copyAtKept:
		wantPaths = []string{keptHolderName(t, dest, holder)}
	}
	base := filepath.Base(dest)
	assertCopySet(t, filepath.Dir(dest), []string{base + holderSuffix, base + keptSuffix}, wantPaths, pass)

	switch want {
	case copyInPlaceUnowned:
		// Retained is not the whole claim: the reason it is retained is that
		// nothing on disk attributes it, and a row that stopped checking that
		// would pass over a copy recovery could have proved was its own.
		if _, err := readHolderMarker(holder); !errors.Is(err, errMarkerMissing) {
			t.Errorf("pass %d: %s should carry no marker, readHolderMarker = %v", pass, holder, err)
		}
	case copyAtDest:
		if _, err := os.Stat(dest); err != nil {
			t.Errorf("pass %d: the copy should have been restored to the destination: %v", pass, err)
		}
	}
}

// keptHolderName is the name a parked copy takes, derived the way parkKeptHolder
// derives it: the same sequence under the Kept prefix.
func keptHolderName(t *testing.T, dest, holder string) string {
	t.Helper()
	name := filepath.Base(holder)
	cut := strings.LastIndex(name, holderSuffix)
	if cut < 0 {
		t.Fatalf("holder name %q carries no holder suffix", name)
	}
	return filepath.Join(filepath.Dir(holder), name[:cut]+keptSuffix+name[cut+len(holderSuffix):])
}

// holderSeqDigitsOf is the sequence a copy carries in its name. It survives a
// park, so a report assertion keyed on it holds whether the copy is still under
// the holder prefix or has moved under the Kept one.
func holderSeqDigitsOf(t *testing.T, dest, holder string) string {
	t.Helper()
	seq, ok := holderStamp(dest, holder)
	if !ok {
		t.Fatalf("holder name %q carries no sequence", filepath.Base(holder))
	}
	return fmt.Sprintf("%020d", seq)
}

// A copy recovery keeps and never names is one an operator cannot find, so the
// report is half of every retain row rather than a nicety.
func assertCopyReported(t *testing.T, reported []string, seq string, pass int) {
	t.Helper()
	if !slices.ContainsFunc(reported, func(m string) bool { return strings.Contains(m, seq) }) {
		t.Errorf("pass %d: the retained copy (sequence %s) should be named in the report, got %v", pass, seq, reported)
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
// A directory that only carries the generated name is a sibling someone renamed
// or an unrelated install, and recovery restoring from one, or reaping one, is
// the whole class of defect the marker exists to close. Each case here is a
// single check dropped: the marker, its kind, its sequence, its destination.
func TestOwnershipCannotBeForgedBySiblingNames(t *testing.T) {
	const forgedSeq = 7
	for _, tc := range []struct {
		name   string
		marker *txnMarker
	}{
		{name: "no marker at all"},
		{name: "another site's kind", marker: &txnMarker{Kind: "bundle-extract", Dest: "engine-dir", Seq: forgedSeq}},
		{name: "sequence disagrees with the name", marker: &txnMarker{Kind: holderMarkerKind, Dest: "engine-dir", Seq: forgedSeq + 1}},
		{name: "destination is another install", marker: &txnMarker{Kind: holderMarkerKind, Dest: "model-dir", Seq: forgedSeq}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dest := filepath.Join(root, "engine-dir")
			// Built outside and renamed in, so nothing about it was ever written
			// by this code: the name is the only thing it shares with a holder,
			// and it is shaped like a restorable one.
			unrelated := filepath.Join(t.TempDir(), "unrelated")
			if err := os.MkdirAll(filepath.Join(unrelated, "install"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(unrelated, "install", "engine"), []byte("forged"), 0o644); err != nil {
				t.Fatal(err)
			}
			forged := fmt.Sprintf("%s%s%020d%s", dest, holderSuffix, forgedSeq, holderSeqSuffix)
			if err := os.Rename(unrelated, forged); err != nil {
				t.Fatal(err)
			}
			if tc.marker != nil {
				if err := writeHolderMarker(forged, *tc.marker); err != nil {
					t.Fatal(err)
				}
			}
			txn := lockFor(t, dest)

			for pass := 1; pass <= 2; pass++ {
				restoreInterruptedPromotion(txn, dest, testPublished, nil)
				got, err := os.ReadFile(filepath.Join(forged, "install", "engine"))
				if err != nil || string(got) != "forged" {
					t.Fatalf("pass %d: the forged directory must be left exactly as it was, got %q (err %v)", pass, got, err)
				}
				// A restore, a park, or a reap all show up here: any of them
				// either creates a destination or moves the directory.
				assertNoOtherEntries(t, root, filepath.Base(forged), installLockDir)
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

// ---- the regression net: recovery's own steps, and a live promotion --------

// deleteWatch records every removeAll recovery asks for, and flags the ones no
// classification licenses: a holder that still holds a copy of an install with
// no commit flag beside it. It is asserted AT THE SEAM rather than off the disk
// because a fault-injected removeAll leaves the copy exactly where a removeAll
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
	real := holderFS
	t.Cleanup(func() { holderFS = real })
	holderFS.removeAll = func(path string) error {
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

// holdsUncommittedCopy reports whether path still holds a copy of an install
// that nothing proves was published over. Recovery may park one of these
// forever; it may never delete one, whatever step just failed. At this site that
// copy is the only offline copy of an engine or a model.
func holdsUncommittedCopy(path string) bool {
	if _, err := os.Stat(filepath.Join(path, "install")); err != nil {
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
	root := t.TempDir()
	dest := filepath.Join(root, "engine-dir")
	uncommitted := plantHolder(t, dest, 100, "v1", false)
	committed := plantHolder(t, dest, 200, "v2", true)
	scratch := plantScratchHolder(t, dest, 50)

	var watch deleteWatch
	func() {
		saved := holderFS
		defer func() { holderFS = saved }()
		watch.install(t)
		for _, path := range []string{committed, scratch, uncommitted} {
			if err := holderFS.removeAll(path); err != nil {
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

// plantScratchHolder plants an owned holder that holds no copy of any install:
// the state a crash between the marker and the set-aside leaves. It is the one
// shape besides a committed copy that recovery may delete.
func plantScratchHolder(t *testing.T, destDir string, seq int64) string {
	t.Helper()
	holder := holderNamed(destDir, seq)
	if err := os.MkdirAll(holder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeHolderMarker(holder, txnMarker{Kind: holderMarkerKind, Dest: filepath.Base(destDir), Seq: seq}); err != nil {
		t.Fatal(err)
	}
	return holder
}

func holderNamed(destDir string, seq int64) string {
	return fmt.Sprintf("%s%s%020d%s", destDir, holderSuffix, seq, holderSeqSuffix)
}

func keptNamed(destDir string, seq int64) string {
	return fmt.Sprintf("%s%s%020d%s", destDir, keptSuffix, seq, holderSeqSuffix)
}

// recoveryStep is one filesystem step of a recovery pass, with the fault that
// fails it. Each is matched where it is reached rather than by call ordinal, so
// a change in how many times recovery stats a holder cannot silently move which
// call the row is failing.
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

func dictationRecoverySteps() []recoveryStep {
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
					return args[0] == filepath.Join(newest, "install")
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
					return strings.Contains(filepath.Base(args[1]), keptSuffix)
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
func TestDictationRecoveryStepFailures(t *testing.T) {
	for _, step := range dictationRecoverySteps() {
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
				root := t.TempDir()
				dest := filepath.Join(root, "engine-dir")
				// Two uncommitted usable copies and one owned holder that holds
				// nothing, with the destination gone: the state that drives
				// every step in the list through a real decision. One copy is
				// restored, one is parked, and the empty holder is the only
				// delete on the happy path, so a step that fails has something
				// to corrupt.
				older := plantHolder(t, dest, 100, "v1", false)
				newest := plantHolder(t, dest, 200, "v2", false)
				scratch := plantScratchHolder(t, dest, 50)
				txn := lockFor(t, dest)

				var watch deleteWatch
				pass := func(faulty bool) func() bool {
					saved := holderFS
					defer func() { holderFS = saved }()
					fired := func() bool { return true }
					if faulty {
						fired = step.inject(t, newest)
					}
					// After the fault, so the watch sees the calls the fault is
					// failing rather than only the ones it lets through.
					watch.install(t)
					restoreInterruptedPromotion(txn, dest, testPublished, nil)
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
					if dictationStepTerminal(dest, older, newest, scratch) {
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
				// wherever the failed passes left the destination.
				pass(false)

				all, denied := watch.counts()
				if len(denied) > 0 {
					t.Errorf("no step failure may produce a delete of an uncommitted copy, got %v", denied)
				}
				if len(all) == 0 {
					t.Fatal("the watch saw no delete at all, so it proves nothing about this pass")
				}
				assertDictationStepTerminalState(t, dest, older, newest, scratch)
			})
		}
	}
}

// dictationStepTerminal reports whether the fixture reached the state a clean
// run reaches. It is a predicate rather than an assertion because it is asked
// twice for opposite reasons: a faulted pass must NOT have got here, and the
// pass after the fault is gone must have.
func dictationStepTerminal(dest, older, newest, scratch string) bool {
	got, err := os.ReadFile(filepath.Join(dest, "engine"))
	if err != nil || string(got) != "v2" {
		return false
	}
	kept, ok := keptHolderPath(older)
	if !ok {
		return false
	}
	if _, err := os.Stat(filepath.Join(kept, "install", "engine")); err != nil {
		return false
	}
	for _, gone := range []string{newest, scratch} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			return false
		}
	}
	return true
}

// assertDictationStepTerminalState is where the fixture above has to end up once
// nothing is failing: the newest copy live, its own holder gone, the older one
// kept under the Kept prefix with its install intact, and the empty holder
// reaped. Content is read back rather than names checked, because a park that
// moved a name and lost the install passes every name assertion.
func assertDictationStepTerminalState(t *testing.T, dest, older, newest, scratch string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dest, "engine"))
	if err != nil || string(got) != "v2" {
		t.Errorf("dest engine = %q (err %v), want the newest copy %q", got, err, "v2")
	}
	parked := keptName(t, dest, older)
	kept, err := os.ReadFile(filepath.Join(parked, "install", "engine"))
	if err != nil || string(kept) != "v1" {
		t.Errorf("the parked copy's engine = %q (err %v), want %q", kept, err, "v1")
	}
	for _, gone := range []string{newest, scratch} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s should be gone once recovery finished: %v", gone, err)
		}
	}
	base := filepath.Base(dest)
	assertCopySet(t, filepath.Dir(dest), []string{base + holderSuffix, base + keptSuffix}, []string{parked}, 3)
}

// gateAt parks the write path at one named boundary: it signals when the call
// whose arguments match is reached, holds that call until release, and passes
// every other call through. blockStep gates EVERY call to a step, which for a
// rename would park a promotion at the rename inside its marker write rather
// than at the set-aside or the publish, so a matched gate is what puts the
// writer at the boundary a row is actually about.
func gateAt(t *testing.T, match func(from, to string) bool) (reached <-chan struct{}, release func()) {
	t.Helper()
	real := holderFS
	t.Cleanup(func() { holderFS = real })
	arrived := make(chan struct{})
	gate := make(chan struct{})
	var arrive, open sync.Once
	release = func() { open.Do(func() { close(gate) }) }
	t.Cleanup(release)
	holderFS.rename = func(from, to string) error {
		if match(from, to) {
			arrive.Do(func() { close(arrived) })
			<-gate
		}
		return real.rename(from, to)
	}
	return arrived, release
}

// signalRemoveAll reports when a removeAll of a matching path is reached. It
// wraps whatever is already installed, so layering it over blockStep signals
// BEFORE the call parks rather than after it is released.
func signalRemoveAll(t *testing.T, match func(path string) bool) <-chan struct{} {
	t.Helper()
	installed := holderFS
	t.Cleanup(func() { holderFS = installed })
	arrived := make(chan struct{})
	var arrive sync.Once
	holderFS.removeAll = func(path string) error {
		if match(path) {
			arrive.Do(func() { close(arrived) })
		}
		return installed.removeAll(path)
	}
	return arrived
}

// A recovery pass and a live promotion for one destination, running at the same
// time, at each of the three boundaries where the destination's only copy is in
// a holder. Nothing here asserts that recovery wins: the claim is that neither
// side destroys the other's copy, that the promotion that was already running
// completes, and that the recovering side either waits the lock out or is told
// the install is in progress. The Install lock is what makes that true, and this
// is the only test that drives both sides of it at once, under -race, through
// one package-level seam.
func TestDictationRecoveryRacesALivePromotion(t *testing.T) {
	for _, tc := range []struct {
		name string
		gate func(t *testing.T, root, dest string) (reached <-chan struct{}, release func())
	}{
		{
			// The previous install is on its way into the holder: for that
			// instant the destination still holds it and the holder is empty.
			name: "set-aside",
			gate: func(t *testing.T, root, dest string) (<-chan struct{}, func()) {
				return gateAt(t, func(from, to string) bool {
					return from == dest && filepath.Base(to) == "install"
				})
			},
		},
		{
			// The destination is absent and the holder has the only copy of the
			// previous install. A recovering pass that acted here would restore
			// that copy over the publish this promotion is in the middle of.
			name: "publish",
			gate: func(t *testing.T, root, dest string) (<-chan struct{}, func()) {
				return gateAt(t, func(from, to string) bool {
					return to == dest && filepath.Base(from) == "stage"
				})
			},
		},
		{
			// The publish landed and the commit flag is written, so the copy in
			// the holder is superseded and the promotion is deleting it. A
			// recovering pass that reached the same holder would be deleting it
			// too.
			name: "reap",
			gate: func(t *testing.T, root, dest string) (<-chan struct{}, func()) {
				release := blockStep(t, "removeAll")
				base := filepath.Base(dest)
				return signalRemoveAll(t, func(path string) bool {
					return strings.HasPrefix(filepath.Base(path), base+holderSuffix)
				}), release
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dest := filepath.Join(root, "engine-dir")
			stagedTree(t, dest, "old")
			stage := stagedTree(t, filepath.Join(root, "stage"), "new")
			txn := lockFor(t, dest)

			var watch deleteWatch
			watch.install(t)
			reached, release := tc.gate(t, root, dest)

			var promoteErr error
			promoted := make(chan struct{})
			go func() {
				defer close(promoted)
				promoteErr = promoteStagedDir(txn, stage, dest, "engine", nil)
				// The recovering side is waiting on this lock, and the only
				// thing that ends its wait is the promotion letting go of it.
				txn.release()
			}()
			<-reached

			var recovered bool
			var lockErr error
			var reports []string
			done := make(chan struct{})
			go func() {
				defer close(done)
				lockErr = withDestinationLock(context.Background(), root, filepath.Base(dest), testPublished, func(own *destTxn) error {
					recovered = true
					restoreInterruptedPromotion(own, dest, testPublished, reporterFor(&reports))
					return nil
				})
			}()
			release()
			<-promoted
			<-done

			if promoteErr != nil {
				t.Errorf("the promotion that was already running must finish: %v", promoteErr)
			}
			// Waited, or was told the install is in progress. Either is a pass;
			// acting on the destination while the promotion held it is not.
			if !recovered && !errors.Is(lockErr, errInstallInProgress) {
				t.Errorf("the recovering side should have waited or reported the install in progress, got %v", lockErr)
			}
			got, err := os.ReadFile(filepath.Join(dest, "engine"))
			if err != nil || string(got) != "new" {
				t.Errorf("dest engine = %q (err %v), want the promotion's own install %q", got, err, "new")
			}
			if _, denied := watch.counts(); len(denied) > 0 {
				t.Errorf("neither side may delete an uncommitted copy, got %v", denied)
			}
			// The promotion cleaned up after itself, and recovery left nothing
			// of its own behind.
			base := filepath.Base(dest)
			assertCopySet(t, root, []string{base + holderSuffix, base + keptSuffix}, nil, 1)
		})
	}
}

// ---- the rows no single crash of the writer can reach ----------------------

// The X rows of the Acceptance Examples. They need arrangements the six crash
// shapes cannot produce: a held lock, more than one candidate, a destination
// that exists and cannot serve, a restore that fails, or a legacy directory
// planted by hand. Everything else about them is the crash table's contract:
// the whole set of copies beside the destination is compared, not one path at a
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
	// silent is the fragments it must NOT name. A sibling that merely collides
	// with the prefix is not a copy of this install, and reporting on it would
	// tell an operator their own directory is recovery's residue.
	silent []string
}

type dictationXRow struct {
	id string
	// arrange plants the state and returns what each pass must produce, plus,
	// where the row needs one, the fault that makes the row's situation happen
	// during a given pass. They come back together so both can close over the
	// paths the arrangement planted.
	arrange func(t *testing.T, root, dest string) (want func(pass int) xWant, impede func(t *testing.T, pass int) func())
	// recover overrides how the pass is driven, for the one row whose subject is
	// the lock itself rather than what is on disk.
	recover func(t *testing.T, root, dest string, pass int) []string
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
	saved := holderFS
	injectFault(t, step, match, errors.New("injected recovery failure"))
	return func() { holderFS = saved }
}

// plantSibling creates a directory beside the destination that shares the holder
// prefix and is not a holder: someone else's directory, which recovery has to
// leave alone and not report on.
func plantSibling(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "notes"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func dictationXRows() []dictationXRow {
	return []dictationXRow{
		{
			// A destination that exists and cannot serve is not a reason to keep
			// a usable copy out of it, and the husk is not this code's to delete
			// either: it moves into a fresh sequenced holder of its own and is
			// parked from there.
			id: "X1",
			arrange: func(t *testing.T, root, dest string) (func(int) xWant, func(*testing.T, int) func()) {
				plantHolder(t, dest, 100, "v1", false)
				plantUnusableDest(t, dest)
				kept := keptNamed(dest, 101)
				return func(pass int) xWant {
					w := xWant{
						live:   xFile{"engine", "v1"},
						copies: []xCopy{{final: kept, file: filepath.Join("install", "bin", "README"), content: "partial"}},
					}
					if pass == 1 {
						w.report = []string{kept}
					}
					return w
				}, nil
			},
		},
		{
			// A Kept backup is permanent. A later publish is not evidence about
			// it: the commit flag goes into the holder the publishing
			// transaction set aside, and a Kept backup is never that holder.
			id: "X2",
			arrange: func(t *testing.T, root, dest string) (func(int) xWant, func(*testing.T, int) func()) {
				stagedTree(t, dest, "live")
				kept := plantKeptHolder(t, dest, 500, "unproven")
				return bothPasses(xWant{
					live:   xFile{"engine", "live"},
					copies: []xCopy{{final: kept, file: filepath.Join("install", "engine"), content: "unproven"}},
				}), nil
			},
		},
		{
			// The newest usable copy cannot be put back. Falling through to the
			// older one would put an install at the destination that the next
			// pass reads as having published over the newer copy, which is how a
			// retained copy turns into a deleted one two passes later.
			id: "X3",
			arrange: func(t *testing.T, root, dest string) (func(int) xWant, func(*testing.T, int) func()) {
				older := plantHolder(t, dest, 100, "v1", false)
				newest := plantHolder(t, dest, 200, "v2", false)
				want := bothPasses(xWant{
					copies: []xCopy{
						{final: older, file: filepath.Join("install", "engine"), content: "v1"},
						{final: newest, file: filepath.Join("install", "engine"), content: "v2"},
					},
					report: []string{newest},
				})
				return want, func(t *testing.T, pass int) func() {
					return faultDuring(t, "rename", func(args ...string) bool {
						return args[0] == filepath.Join(newest, "install")
					})
				}
			},
		},
		{
			// Unusable is a selection decision, not a delete: the newest copy is
			// skipped for one that can serve and is then kept like any other
			// copy nothing proves was superseded.
			id: "X4",
			arrange: func(t *testing.T, root, dest string) (func(int) xWant, func(*testing.T, int) func()) {
				plantHolder(t, dest, 100, "v1", false)
				newest := plantHolder(t, dest, 200, "v2", false)
				if err := os.Rename(filepath.Join(newest, "install", "engine"), filepath.Join(newest, "install", "partial")); err != nil {
					t.Fatal(err)
				}
				kept := keptName(t, dest, newest)
				return func(pass int) xWant {
					w := xWant{
						live:   xFile{"engine", "v1"},
						copies: []xCopy{{final: kept, file: filepath.Join("install", "partial"), content: "v2"}},
					}
					if pass == 1 {
						w.report = []string{kept}
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
			arrange: func(t *testing.T, root, dest string) (func(int) xWant, func(*testing.T, int) func()) {
				holder := plantHolder(t, dest, 100, "v1", false)
				want := bothPasses(xWant{
					copies: []xCopy{{final: holder, file: filepath.Join("install", "engine"), content: "v1"}},
					report: []string{holder},
				})
				return want, func(t *testing.T, pass int) func() {
					return faultDuring(t, "stat", func(args ...string) bool {
						return args[0] == filepath.Join(holder, committedFile)
					})
				}
			},
		},
		{
			// A destination another process holds is one recovery knows nothing
			// about: on disk a live promotion mid-swap and a crashed one are the
			// same thing, and the Install lock is the only thing that tells them
			// apart. The lock is per open file description, so the handle this
			// test holds during pass one is what a second process's hold looks
			// like to the recovering side.
			id: "X6",
			arrange: func(t *testing.T, root, dest string) (func(int) xWant, func(*testing.T, int) func()) {
				holder := plantHolder(t, dest, 100, "v1", false)
				return func(pass int) xWant {
					if pass == 1 {
						return xWant{
							copies: []xCopy{{final: holder, file: filepath.Join("install", "engine"), content: "v1"}},
						}
					}
					return xWant{live: xFile{"engine", "v1"}}
				}, nil
			},
			recover: recoverAroundAHeldLock,
		},
		{
			// A name that shares the prefix and fails the grammar carries no
			// order at all, so it is not a candidate for anything: not restored,
			// not parked, not deleted. At this site it is not reported either,
			// because the prefix is a suffix of someone's own directory name and
			// telling an operator their notes are recovery's residue is worse
			// than saying nothing.
			id: "X8",
			arrange: func(t *testing.T, root, dest string) (func(int) xWant, func(*testing.T, int) func()) {
				sibling := plantSibling(t, dest+holderSuffix+"notes", "mine")
				return bothPasses(xWant{
					copies: []xCopy{{final: sibling, file: "notes", content: "mine"}},
					silent: []string{sibling},
				}), nil
			},
		},
		{
			// The destination went away outside this code and the only copy left
			// carries a commit flag. Committed is second in selection, never
			// excluded from it: a copy that was superseded by an install that is
			// no longer there is still a copy of something.
			id: "X9",
			arrange: func(t *testing.T, root, dest string) (func(int) xWant, func(*testing.T, int) func()) {
				plantHolder(t, dest, 100, "v1", true)
				return bothPasses(xWant{live: xFile{"engine", "v1"}}), nil
			},
		},
		{
			// Two copies, one destination: the newest goes back and the older is
			// kept rather than deleted, because nothing proves anything
			// published over it either.
			id: "X10",
			arrange: func(t *testing.T, root, dest string) (func(int) xWant, func(*testing.T, int) func()) {
				older := plantHolder(t, dest, 100, "v1", false)
				plantHolder(t, dest, 200, "v2", false)
				kept := keptName(t, dest, older)
				return func(pass int) xWant {
					w := xWant{
						live:   xFile{"engine", "v2"},
						copies: []xCopy{{final: kept, file: filepath.Join("install", "engine"), content: "v1"}},
					}
					if pass == 1 {
						w.report = []string{kept}
					}
					return w
				}, nil
			},
		},
		{
			// v0.8.0 residue: a holder-shaped name with no sequence in it. It
			// cannot be ordered against anything, so it is left exactly where it
			// is, and like any other unstampable sibling at this site it is not
			// reported.
			id: "X11",
			arrange: func(t *testing.T, root, dest string) (func(int) xWant, func(*testing.T, int) func()) {
				residue := plantSibling(t, dest+holderSuffix+"1699999999", "resid")
				return bothPasses(xWant{
					copies: []xCopy{{final: residue, file: "notes", content: "resid"}},
					silent: []string{residue},
				}), nil
			},
		},
		{
			// Same as X1 with the only candidate committed. The husk still moves
			// aside first, and the copy that replaces it is the committed one,
			// because there is no uncommitted copy to prefer.
			id: "X12",
			arrange: func(t *testing.T, root, dest string) (func(int) xWant, func(*testing.T, int) func()) {
				plantHolder(t, dest, 100, "v1", true)
				plantUnusableDest(t, dest)
				kept := keptNamed(dest, 101)
				return func(pass int) xWant {
					w := xWant{
						live:   xFile{"engine", "v1"},
						copies: []xCopy{{final: kept, file: filepath.Join("install", "bin", "README"), content: "partial"}},
					}
					if pass == 1 {
						w.report = []string{kept}
					}
					return w
				}, nil
			},
		},
		{
			// Nothing beside it can replace the husk, so it is not taken apart:
			// an operator with an unusable install directory has strictly more
			// than one with no install directory at all.
			id: "X13",
			arrange: func(t *testing.T, root, dest string) (func(int) xWant, func(*testing.T, int) func()) {
				holder := plantHolder(t, dest, 100, "v1", false)
				if err := os.Rename(filepath.Join(holder, "install", "engine"), filepath.Join(holder, "install", "partial")); err != nil {
					t.Fatal(err)
				}
				plantUnusableDest(t, dest)
				kept := keptName(t, dest, holder)
				return func(pass int) xWant {
					w := xWant{
						live:   xFile{filepath.Join("bin", "README"), "partial"},
						copies: []xCopy{{final: kept, file: filepath.Join("install", "partial"), content: "v1"}},
					}
					if pass == 1 {
						w.report = []string{"no usable install"}
					}
					return w
				}, nil
			},
		},
		{
			// The husk was already aside when the restore failed, so recovery
			// owes the destination its husk back: the row's whole claim is that
			// the destination ends the pass exactly as it was found, with the
			// candidate still where it was and the set-aside holder, now empty,
			// gone.
			id: "X14",
			arrange: func(t *testing.T, root, dest string) (func(int) xWant, func(*testing.T, int) func()) {
				holder := plantHolder(t, dest, 100, "v1", false)
				plantUnusableDest(t, dest)
				want := bothPasses(xWant{
					live:   xFile{filepath.Join("bin", "README"), "partial"},
					copies: []xCopy{{final: holder, file: filepath.Join("install", "engine"), content: "v1"}},
					report: []string{holder},
				})
				return want, func(t *testing.T, pass int) func() {
					return faultDuring(t, "rename", func(args ...string) bool {
						return args[0] == filepath.Join(holder, "install") && args[1] == dest
					})
				}
			},
		},
	}
}

// recoverAroundAHeldLock drives X6 through the production entry point rather
// than calling the reconcile directly: the lock is the row's subject, and
// withDestinationLock is the only place a caller finds out it could not have it.
// Pass one runs with the destination's lock held by a handle this test owns,
// which is what a second process holding it looks like from here; pass two runs
// with it free.
func recoverAroundAHeldLock(t *testing.T, root, dest string, pass int) []string {
	t.Helper()
	var reports []string
	recovered := false
	if pass == 1 {
		held := lockFor(t, dest)
		defer held.release()
		// Long enough that a slow machine does not turn a wait into a skip by
		// accident, short enough that the row does not sit on the default
		// two-minute budget for a lock nothing is going to release.
		saved := installLockWait
		installLockWait = 200 * time.Millisecond
		defer func() { installLockWait = saved }()
		err := withDestinationLock(context.Background(), root, filepath.Base(dest), testPublished, func(own *destTxn) error {
			recovered = true
			return nil
		})
		if recovered {
			t.Fatal("the lock was held for the whole pass, so recovery must not have taken it")
		}
		if !errors.Is(err, errInstallInProgress) {
			t.Fatalf("a held destination should report the install in progress, got %v", err)
		}
		return []string{err.Error()}
	}
	if err := withDestinationLock(context.Background(), root, filepath.Base(dest), testPublished, func(own *destTxn) error {
		restoreInterruptedPromotion(own, dest, testPublished, reporterFor(&reports))
		return nil
	}); err != nil {
		t.Fatalf("the lock is free on this pass: %v", err)
	}
	return reports
}

func TestDictationXMatrix(t *testing.T) {
	for _, row := range dictationXRows() {
		t.Run(row.id, func(t *testing.T) {
			root := t.TempDir()
			dest := filepath.Join(root, "engine-dir")
			want, impede := row.arrange(t, root, dest)
			var txn *destTxn
			if row.recover == nil {
				// One handle for the whole row: the lock is per open file
				// description, so a second acquire in this process would
				// contend with the first rather than nest.
				txn = lockFor(t, dest)
			}

			var watch deleteWatch
			for pass := 1; pass <= 2; pass++ {
				var reports []string
				func() {
					saved := holderFS
					defer func() { holderFS = saved }()
					if impede != nil {
						defer impede(t, pass)()
					}
					// After the impediment, so a delete the injected fault
					// would have failed is still seen as a delete that was
					// asked for.
					watch.install(t)
					if row.recover != nil {
						reports = row.recover(t, root, dest, pass)
						return
					}
					restoreInterruptedPromotion(txn, dest, testPublished, reporterFor(&reports))
				}()
				assertDictationXState(t, dest, want(pass), reports, pass)
			}
			if _, denied := watch.counts(); len(denied) > 0 {
				t.Errorf("no row may delete a copy nothing proves was superseded, got %v", denied)
			}
		})
	}
}

// assertDictationXState asserts a row's whole terminal state: the destination,
// the content of every copy at the name it is supposed to be at, the full set of
// directories under both prefixes, and the report. Content rather than names
// alone, because a park that moved a name and lost the install passes every name
// assertion there is.
func assertDictationXState(t *testing.T, dest string, want xWant, reports []string, pass int) {
	t.Helper()
	if want.live.name == "" {
		if _, err := os.Lstat(dest); !os.IsNotExist(err) {
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
	base := filepath.Base(dest)
	assertCopySet(t, filepath.Dir(dest), []string{base + holderSuffix, base + keptSuffix}, paths, pass)
	for _, fragment := range want.report {
		if !slices.ContainsFunc(reports, func(m string) bool { return strings.Contains(m, fragment) }) {
			t.Errorf("pass %d: the report should name %q, got %v", pass, fragment, reports)
		}
	}
	for _, fragment := range want.silent {
		if slices.ContainsFunc(reports, func(m string) bool { return strings.Contains(m, fragment) }) {
			t.Errorf("pass %d: the report should say nothing about %q, got %v", pass, fragment, reports)
		}
	}
}
