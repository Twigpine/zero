package sandbox

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// scopeOutsideRoots builds a workspace and an unrelated directory that no
// default write root already covers.
//
// NOT t.TempDir(). /tmp and $TMPDIR are default write roots, so a temporary
// grant for a path under one is a no-op — every assertion here would pass
// against code that grants nothing at all. That is RULES §2.3 in its exact
// form, and it caught the first version of this test.
// scopeOutsideRoots builds a workspace and a directory outside it, both under
// test-owned storage, and returns a Scope that treats the second as genuinely
// outside.
//
// THE FIXTURE PROBLEM AND ITS ACTUAL CAUSE. NewScope seeds the system temporary
// directory as a permanent write root, so an ordinary t.TempDir() grant is
// covered before any temporary grant exists and every assertion built on it is
// vacuous. The first fix for that reached into os.UserHomeDir() to find ground
// the defaults do not already own — which worked, and bought two new problems:
// a hermetic runner may expose HOME read-only, so the tests failed at MkdirTemp
// before asserting anything; and on an ordinary machine they wrote fixtures into
// the developer's real home and left zeromax-scope-* debris behind whenever a
// run was interrupted. Reported by @jatmn.
//
// The cause is the seeded defaults, not the location, so the Scope is built
// DIRECTLY here — the same pattern scopeOutsideDefaults and temporaryWriteFixture
// already use. With no defaults seeded, t.TempDir() is outside by construction,
// the fixture is hermetic, and nothing touches HOME.
//
// Paths are symlink-resolved because the scope stores resolved roots and macOS
// hands out /var/... temp directories that resolve to /private/var/...; an
// unresolved fixture path compares unequal to the root the scope just recorded.
func scopeOutsideRoots(t *testing.T) (scope *Scope, workspace string, outside string) {
	t.Helper()
	workspace = resolvedFixturePath(t, t.TempDir())
	outside = filepath.Join(resolvedFixturePath(t, t.TempDir()), "outside")
	// FATAL, NOT SKIP. Every caller of this helper asserts an AUTHORIZATION
	// boundary, so a skipped fixture means that assertion never ran and the build
	// reported green anyway. t.TempDir() already failed the test if it could not
	// provide storage, so a subdirectory failing under it is a broken machine.
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatalf("cannot prepare %s: %v", outside, err)
	}
	scope = &Scope{workspaceRoot: workspace}
	// PROVED AGAINST THIS SCOPE, not against production's default list. The point
	// of the helper is that a grant here changes something, and the only thing
	// that can answer that is the scope the test will actually use. Checking
	// defaultTempWriteRoots() instead asks about a scope nobody builds here — and
	// says "prove nothing" for a t.TempDir() path that this scope, seeded with no
	// defaults, genuinely does not cover.
	//
	// Asserting the NEGATIVE up front is also what keeps every caller honest: it
	// establishes that the target begins unauthorised, so a later success is the
	// grant's doing and not the fixture's placement.
	if scope.validate(filepath.Join(outside, "probe.txt")) == nil {
		t.Fatalf("%s is writable before any grant exists, so a temporary grant here would prove nothing", outside)
	}
	if scope.validateRead(filepath.Join(outside, "probe.txt")) == nil {
		t.Fatalf("%s is readable before any grant exists, so a temporary read grant here would prove nothing", outside)
	}
	return scope, workspace, outside
}

func hasReadRoot(scope *Scope, root string) bool {
	for _, existing := range scope.ReadRoots() {
		if existing == root {
			return true
		}
	}
	return false
}

// THE DEFECT: one holder's cleanup revoked another's access. Two read-only
// tools in the same parallel batch, both blocked on the same directory, is
// exactly this — and read-only tools are the ones the batch runs concurrently.
func TestATemporaryReadSurvivesASiblingsCleanup(t *testing.T) {
	scope, _, outside := scopeOutsideRoots(t)
	// The probe that the fix must make unnecessary: without it this reads
	// true -> false.
	if hasReadRoot(scope, outside) {
		t.Fatal("the outside root is already covered; this test proves nothing")
	}

	_, undoA, err := scope.AddTemporaryRead(outside)
	if err != nil {
		t.Fatal(err)
	}
	_, undoB, err := scope.AddTemporaryRead(outside)
	if err != nil {
		t.Fatal(err)
	}
	if !hasReadRoot(scope, outside) {
		t.Fatal("the grant did not take effect")
	}

	undoA()
	if !hasReadRoot(scope, outside) {
		t.Fatal("A's cleanup revoked the root while B still held a grant")
	}
	undoB()
	if hasReadRoot(scope, outside) {
		t.Fatal("the root outlived its last holder")
	}
}

// A NARROWER REQUEST KEEPS ITS OWN SUBTREE AND NOTHING WIDER.
//
// THIS TEST USED TO ASSERT THE OPPOSITE, and the earlier version is worth
// recording because it is how the escalation survived a review round. It was
// written to prove a LIFETIME property — the broad holder's cleanup must not
// revoke a live narrow holder — and it proved that by asserting the BROAD root
// was still present afterwards. That is the defect stated as a requirement: the
// narrow holder was keeping its neighbour's whole tree alive in order to keep
// itself alive, so the surviving reader could read siblings it never asked for.
//
// Lifetime and capability are two properties and one reference cannot carry
// both. The assertions below name them separately: the requested path stays
// readable while its holder lives, AND a sibling under the released broad root
// is denied as soon as the holder that authorised it exits. Reported by @jatmn.
func TestANarrowerReaderKeepsOnlyItsOwnSubtree(t *testing.T) {
	scope, _, outside := scopeOutsideRoots(t)
	nested := filepath.Join(outside, "nested")
	sibling := filepath.Join(outside, "sibling")
	for _, dir := range []string{nested, sibling} {
		// FATAL, NOT SKIP. These directories are the fixture, not a precondition
		// the environment might reasonably withhold — skipping on them reports a
		// broken setup as a passing run.
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	_, undoBroad, err := scope.AddTemporaryRead(outside)
	if err != nil {
		t.Fatal(err)
	}
	_, undoNarrow, err := scope.AddTemporaryRead(nested)
	if err != nil {
		t.Fatal(err)
	}

	// THE BROAD HOLDER GOES FIRST. The narrow holder outliving it is what exposes
	// whose authority it is actually holding.
	undoBroad()
	if block := scope.validateRead(filepath.Join(nested, "mine.txt")); block != nil {
		t.Errorf("the broad holder's cleanup revoked the subtree the narrow holder asked for: %v", block.Reason)
	}
	if block := scope.validateRead(filepath.Join(sibling, "not-mine.txt")); block == nil {
		t.Error("the narrow reader can read a SIBLING it never asked for: the broad holder's authority outlived the broad holder")
	}
	if block := scope.validateRead(filepath.Join(outside, "not-mine.txt")); block == nil {
		t.Error("the narrow reader can read the covering root itself after its holder released")
	}

	undoNarrow()
	if block := scope.validateRead(filepath.Join(nested, "mine.txt")); block == nil {
		t.Error("the nested root outlived its only holder")
	}
}

// The opposite order, as a control: the narrow reader releasing first must not
// disturb the broad reader that is still live.
func TestANarrowerReaderReleasingFirstLeavesTheBroadGrant(t *testing.T) {
	scope, _, outside := scopeOutsideRoots(t)
	nested := filepath.Join(outside, "nested")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	_, undoBroad, err := scope.AddTemporaryRead(outside)
	if err != nil {
		t.Fatal(err)
	}
	_, undoNarrow, err := scope.AddTemporaryRead(nested)
	if err != nil {
		t.Fatal(err)
	}

	undoNarrow()
	if block := scope.validateRead(filepath.Join(nested, "still-ok.txt")); block != nil {
		t.Errorf("the narrow holder's release revoked the broad holder's coverage: %v", block.Reason)
	}
	undoBroad()
	if block := scope.validateRead(filepath.Join(nested, "gone.txt")); block == nil {
		t.Error("the tree outlived every holder")
	}
}

// TWO READERS OF THE SAME ROOT SHARE ONE ENTRY. Only an exact match shares; a
// narrower one never does, which is what the escalation test above pins.
func TestTwoReadersOfTheSameRootShareOneEntry(t *testing.T) {
	scope, _, outside := scopeOutsideRoots(t)
	_, undoFirst, err := scope.AddTemporaryRead(outside)
	if err != nil {
		t.Fatal(err)
	}
	_, undoSecond, err := scope.AddTemporaryRead(outside)
	if err != nil {
		t.Fatal(err)
	}

	undoFirst()
	if block := scope.validateRead(filepath.Join(outside, "still-held.txt")); block != nil {
		t.Errorf("one reader's release revoked the other's grant on the same root: %v", block.Reason)
	}
	undoSecond()
	if block := scope.validateRead(filepath.Join(outside, "gone.txt")); block == nil {
		t.Error("the shared root outlived both holders")
	}
}

// A NARROW PERMANENT READ IS NOT BORROWED FROM A BROAD TEMPORARY ONE. The old
// scan walked readRoots in order and stopped at the first entry covering the
// path, so a broad TEMPORARY root earlier in the slice shadowed a narrower
// PERMANENT one later — and the caller took a reference it did not need on a
// grant that was about to end.
func TestAPermanentReadIsNotShadowedByABroadTemporaryOne(t *testing.T) {
	scope, _, outside := scopeOutsideRoots(t)
	nested := filepath.Join(outside, "nested")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	// The broad TEMPORARY grant is taken first, so it sits earlier in readRoots.
	_, undoBroad, err := scope.AddTemporaryRead(outside)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scope.AddRead(nested); err != nil {
		t.Fatal(err)
	}

	undoBroad()
	if block := scope.validateRead(filepath.Join(nested, "mine.txt")); block != nil {
		t.Errorf("a PERMANENT read grant died with a broad temporary one that merely preceded it: %v", block.Reason)
	}
}

// A PERMANENT root is not refcounted and must never be removed by a temporary
// holder's cleanup — the undo for a request it already covers is genuinely
// nothing.
func TestATemporaryGrantNeverRevokesAPermanentRoot(t *testing.T) {
	scope, _, outside := scopeOutsideRoots(t)
	if _, err := scope.AddRead(outside); err != nil {
		t.Fatal(err)
	}
	_, undo, err := scope.AddTemporaryRead(outside)
	if err != nil {
		t.Fatal(err)
	}
	undo()
	if !hasReadRoot(scope, outside) {
		t.Fatal("a temporary holder's cleanup removed a permanent read root")
	}
}

// The same rule for WRITE roots, or a write grant keeps the bug reads no longer
// have.
func TestATemporaryWriteSurvivesASiblingsCleanup(t *testing.T) {
	scope, _, outside := scopeOutsideRoots(t)
	covered := func() bool {
		for _, existing := range scope.Roots() {
			if existing == outside {
				return true
			}
		}
		return false
	}
	_, undoA, err := scope.AddTemporaryWrite(outside)
	if err != nil {
		t.Fatal(err)
	}
	_, undoB, err := scope.AddTemporaryWrite(outside)
	if err != nil {
		t.Fatal(err)
	}
	if !covered() {
		t.Fatal("the write grant did not take effect")
	}
	undoA()
	if !covered() {
		t.Fatal("A's cleanup revoked the write root while B still held a grant")
	}
	undoB()
	if covered() {
		t.Fatal("the write root outlived its last holder")
	}
}

// UNDER REAL CONCURRENCY, which is the shape the parallel tool batch produces:
// eight holders taking and releasing the same root in overlapping windows. The
// root must be present the whole time at least one holder has it, and gone once
// the last releases.
func TestConcurrentHoldersOfOneRoot(t *testing.T) {
	scope, _, outside := scopeOutsideRoots(t)

	const holders = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	release := make(chan struct{})
	// TWO PER HOLDER, because each one can report at both hasReadRoot checks. At
	// one slot per holder the channel fills after eight reports, the ninth send
	// blocks forever, wg.Done() is never reached and wg.Wait() hangs — so the
	// test would DEADLOCK rather than fail in exactly the case it exists to
	// catch, since nothing drains failures until after the wait.
	failures := make(chan string, 2*holders)
	// Signalled by EVERY holder once its own AddTemporaryRead has returned,
	// failure included: the gate below waits for all of them, so a holder that
	// returned early without signalling would hang this test rather than fail it.
	acquired := make(chan struct{}, holders)
	for i := 0; i < holders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, undo, err := scope.AddTemporaryRead(outside)
			acquired <- struct{}{}
			if err != nil {
				failures <- err.Error()
				return
			}
			// Every holder must SEE its own grant for as long as it holds it.
			if !hasReadRoot(scope, outside) {
				failures <- "a holder could not see the root it had just been granted"
			}
			<-release
			if !hasReadRoot(scope, outside) {
				failures <- "a holder lost the root while still holding it"
			}
			undo()
		}()
	}
	close(start)
	// EVERY holder, not the first one. Spinning on hasReadRoot only waited for
	// somebody to hold the root, and the spin is tight enough to win that race
	// against the goroutines still being scheduled: measured over 200 runs, 194
	// of them peaked at a single simultaneous holder and none ever reached
	// eight. A test named for concurrent holders was testing one holder at a
	// time, and would have passed against an implementation with no refcount at
	// all. Counting the acquisitions makes the overlap real instead of hoped for.
	for i := 0; i < holders; i++ {
		<-acquired
	}
	close(release)
	wg.Wait()
	close(failures)
	for failure := range failures {
		t.Fatal(failure)
	}
	if hasReadRoot(scope, outside) {
		t.Fatal("the root outlived every holder")
	}
	// AND THE BOOKKEEPING IS EMPTY, not merely invisible. hasReadRoot answers
	// what a caller can see; a refcount entry left behind at zero is not visible
	// that way and would still leak, one map entry per acquire/release cycle.
	scope.mu.Lock()
	leftover, stillCounted := scope.tempReads[outside]
	scope.mu.Unlock()
	if stillCounted {
		t.Fatalf("the refcount table kept an entry for %q after every holder released: %v", outside, leftover)
	}
}
