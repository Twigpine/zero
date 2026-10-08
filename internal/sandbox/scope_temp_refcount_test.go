package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

// scopeOutsideDefaults builds a Scope directly instead of through NewScope.
//
// NewScope adds the system temp directory as a write root, so a target under
// t.TempDir() is already covered and AddTemporaryRead returns before it ever
// touches the refcount. A test built that way exercises none of this and passes
// against the bug — which is how the first version of this test passed.
func scopeOutsideDefaults(t *testing.T) (*Scope, string) {
	t.Helper()
	workspace := t.TempDir()
	target := filepath.Join(t.TempDir(), "shared")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	// Built directly, NOT via NewScope: real directories are needed because
	// normalizeScopeRoot requires them to exist, but NewScope's default temp
	// write root would then cover the target and short-circuit the path.
	return &Scope{workspaceRoot: workspace}, target
}

// The refcount and the root list have to move together.
//
// Release used to delete the refcount, drop the lock, then strip the root in a
// second acquisition. In that window the root was still in readRoots but no
// longer in tempReads, so a concurrent AddTemporaryRead read it as a PERMANENT
// root and handed its caller a no-op undo — then the release stripped it and
// that caller silently lost access it believed it held.
func TestReleasingATemporaryReadCannotStripALiveGrant(t *testing.T) {
	scope, target := scopeOutsideDefaults(t)

	// First holder establishes the root.
	first, releaseFirst, err := scope.AddTemporaryRead(target)
	if err != nil {
		t.Fatalf("AddTemporaryRead: %v", err)
	}
	if len(scope.tempReads) == 0 {
		t.Fatal("precondition: the grant should be refcounted, not covered by a default root")
	}

	// Second holder takes a reference on the same root.
	second, releaseSecond, err := scope.AddTemporaryRead(target)
	if err != nil {
		t.Fatalf("AddTemporaryRead (second): %v", err)
	}

	// The first holder leaving must not revoke the second's access.
	releaseFirst()
	if block := scope.validateRead(second); block != nil {
		t.Fatalf("the second holder lost its grant when the first released: %v", block)
	}

	// Only the last release retires the root.
	releaseSecond()
	if block := scope.validateRead(first); block == nil {
		t.Error("the root outlived its last holder")
	}
}

// Under contention the same invariant has to hold: while a grant is live, its
// root is readable.
func TestTemporaryReadGrantsSurviveConcurrentReleases(t *testing.T) {
	scope, target := scopeOutsideDefaults(t)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []string

	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				granted, undo, err := scope.AddTemporaryRead(target)
				if err != nil {
					mu.Lock()
					failures = append(failures, "AddTemporaryRead: "+err.Error())
					mu.Unlock()
					return
				}
				if block := scope.validateRead(granted); block != nil {
					mu.Lock()
					failures = append(failures, "root not readable while a grant was live")
					mu.Unlock()
				}
				undo()
			}
		}()
	}
	wg.Wait()

	if len(failures) > 0 {
		t.Fatalf("%d failures, first: %s", len(failures), failures[0])
	}
}

// A read covered by a TEMPORARY WRITE root must take a reference too.
//
// permanentWriteRootCoversLocked scans extraRoots, and AddTemporaryWrite appends
// temporary write roots there — so "covered by a write root" does not mean
// "covered permanently". Without a reference the write holder's cleanup silently
// revoked the reader's access: the same defect the refcount closes, reached
// across the read/write boundary instead of within one side of it.
func TestAReadCoveredByATemporaryWriteSurvivesItsRelease(t *testing.T) {
	workspace := t.TempDir()
	outer := filepath.Join(t.TempDir(), "outer")
	inner := filepath.Join(outer, "inner")
	if err := os.MkdirAll(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	// Built directly for the same reason scopeOutsideDefaults is: NewScope's
	// default temp write root would cover the fixture and short-circuit this.
	scope := &Scope{workspaceRoot: workspace}

	_, releaseWrite, err := scope.AddTemporaryWrite(outer)
	if err != nil {
		t.Fatalf("temporary write: %v", err)
	}
	readRoot, releaseRead, err := scope.AddTemporaryRead(inner)
	if err != nil {
		t.Fatalf("temporary read: %v", err)
	}

	covered := func() bool {
		for _, root := range scope.ReadRoots() {
			if pathWithinRoot(root, readRoot) {
				return true
			}
		}
		return false
	}
	if !covered() {
		t.Fatal("the reader did not hold access before any release")
	}

	// The WRITE holder finishes first; the reader has not released.
	releaseWrite()
	if !covered() {
		t.Errorf("the write holder's cleanup revoked a live read grant on %s", readRoot)
	}

	// And once the reader releases too, the root is genuinely gone.
	releaseRead()
	if covered() {
		t.Errorf("the root outlived its last holder: %v", scope.ReadRoots())
	}
}

func hasExtraRoot(scope *Scope, root string) bool {
	for _, existing := range writeRootsBeyondWorkspace(scope) {
		if existing == root {
			return true
		}
	}
	return false
}

// A PERMANENT GRANT MUST OUTLIVE THE TEMPORARY ONE IT LANDED ON TOP OF.
//
// extraRoots holds temporary write roots alongside permanent ones, so Add asked
// only "is this already covered" — and a session-scoped grant made while a
// temporary holder happened to cover the path recorded nothing, then vanished
// when that holder released. Outliving the request that prompted it is the
// entire difference between Add and AddTemporaryWrite.
func TestAPermanentGrantSurvivesTheTemporaryOneItCovered(t *testing.T) {
	t.Run("write, same root", func(t *testing.T) {
		scope, _, outside := scopeOutsideRoots(t)
		_, releaseTemp, err := scope.AddTemporaryWrite(outside)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := scope.Add(outside); err != nil {
			t.Fatal(err)
		}
		releaseTemp()
		if !hasExtraRoot(scope, outside) {
			t.Error("the temporary holder's release revoked a permanent write grant")
		}
	})

	t.Run("read, same root", func(t *testing.T) {
		scope, _, outside := scopeOutsideRoots(t)
		_, releaseTemp, err := scope.AddTemporaryRead(outside)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := scope.AddRead(outside); err != nil {
			t.Fatal(err)
		}
		releaseTemp()
		if !hasReadRoot(scope, outside) {
			t.Error("the temporary holder's release revoked a permanent read grant")
		}
	})

	// The nested shape: a BROAD temporary root covering a NARROW permanent one.
	// Promoting in place cannot help here — the narrow root has to be recorded
	// in its own right, or it goes when the broad one does.
	t.Run("broad temporary over narrow permanent", func(t *testing.T) {
		scope, _, outside := scopeOutsideRoots(t)
		inner := filepath.Join(outside, "inner")
		if err := os.MkdirAll(inner, 0o700); err != nil {
			t.Fatal(err)
		}
		_, releaseBroad, err := scope.AddTemporaryWrite(outside)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := scope.Add(inner); err != nil {
			t.Fatal(err)
		}
		releaseBroad()
		covered := false
		for _, existing := range writeRootsBeyondWorkspace(scope) {
			if pathWithinRoot(existing, inner) {
				covered = true
			}
		}
		if !covered {
			t.Error("a narrower permanent grant died with the broader temporary root it sat under")
		}
	})

	// THE READ MIRROR OF BOTH SHAPES ABOVE. AddRead makes two claims that the
	// subtests so far only made for Add: the loop over extraRoots skips coverage
	// that is a temporary WRITE, and the loop over readRoots skips coverage that
	// is a temporary READ. Each claim is a separate branch, and neither was
	// exercised from the read side.
	t.Run("permanent read under a temporary write", func(t *testing.T) {
		scope, _, outside := scopeOutsideRoots(t)
		_, releaseWrite, err := scope.AddTemporaryWrite(outside)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := scope.AddRead(outside); err != nil {
			t.Fatal(err)
		}
		releaseWrite()
		if !hasReadRoot(scope, outside) {
			t.Error("a temporary write holder's release revoked a permanent read grant")
		}
	})

	t.Run("broad temporary read over narrow permanent read", func(t *testing.T) {
		scope, _, outside := scopeOutsideRoots(t)
		inner := filepath.Join(outside, "inner")
		if err := os.MkdirAll(inner, 0o700); err != nil {
			t.Fatal(err)
		}
		_, releaseBroad, err := scope.AddTemporaryRead(outside)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := scope.AddRead(inner); err != nil {
			t.Fatal(err)
		}
		releaseBroad()
		if block := scope.validateRead(filepath.Join(inner, "audit-me.go")); block != nil {
			t.Errorf("a narrower permanent read died with the broader temporary read it sat under: %v", block)
		}
	})
}

// ONE HOLDER'S UNDO DROPS ONE HOLDER'S REFERENCE, HOWEVER OFTEN IT IS CALLED.
//
// The count flooring at zero stops it going negative; it does not stop a
// holder's second call consuming somebody else's reference. With two readers,
// calling the first's undo twice took the count 2 -> 1 -> 0 and removed a root
// the second was still using — the exact revocation this refcount exists to
// prevent, reached through a duplicate call rather than a sibling's cleanup.
// Deferred cleanups in a retry path are how a real caller does this by accident.
func TestOneHoldersUndoIsIdempotent(t *testing.T) {
	scope, workspace, outside := scopeOutsideRoots(t)
	_, undoFirst, err := scope.AddTemporaryRead(outside)
	if err != nil {
		t.Fatal(err)
	}
	_, undoSecond, err := scope.AddTemporaryRead(outside)
	if err != nil {
		t.Fatal(err)
	}
	undoFirst()
	undoFirst()
	undoFirst()
	if !hasReadRoot(scope, outside) {
		t.Fatal("repeated calls to one holder's undo revoked a live holder's access")
	}
	undoSecond()
	if hasReadRoot(scope, outside) {
		t.Error("the root outlived its last holder")
	}

	// The write side takes the same rule. Built directly for the same reason the
	// helper is: NewScope would seed the system temp directory as a permanent
	// write root, which already covers this fixture and makes the grant vacuous.
	writeScope := &Scope{workspaceRoot: workspace}
	_, undoWriteFirst, err := writeScope.AddTemporaryWrite(outside)
	if err != nil {
		t.Fatal(err)
	}
	_, undoWriteSecond, err := writeScope.AddTemporaryWrite(outside)
	if err != nil {
		t.Fatal(err)
	}
	undoWriteFirst()
	undoWriteFirst()
	if !hasExtraRoot(writeScope, outside) {
		t.Fatal("repeated calls to one write holder's undo revoked a live holder's access")
	}
	undoWriteSecond()
	if hasExtraRoot(writeScope, outside) {
		t.Error("the write root outlived its last holder")
	}
}

// temporaryWriteFixture builds a nested pair of real directories and a scope
// that covers neither: an OUTER directory a temporary write grant will take,
// and an INNER one below it that a temporary read grant will ask for.
//
// Built directly rather than through NewScope, for the reason
// scopeOutsideDefaults documents: NewScope seeds the system temp directory as a
// PERMANENT write root, which would cover the whole fixture and send every
// branch under test down the "genuinely permanent" path instead. Both paths are
// returned symlink-resolved because normalizeScopeRoot resolves what it stores,
// and on macOS the fixture's /var spelling is a symlink to /private/var.
func temporaryWriteFixture(t *testing.T) (scope *Scope, workspace, outer, inner string) {
	t.Helper()
	outer = filepath.Join(t.TempDir(), "outer")
	inner = filepath.Join(outer, "inner")
	if err := os.MkdirAll(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	workspace = resolvedFixturePath(t, t.TempDir())
	return &Scope{workspaceRoot: workspace}, workspace, resolvedFixturePath(t, outer), resolvedFixturePath(t, inner)
}

func resolvedFixturePath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolve fixture %s: %v", path, err)
	}
	return resolved
}

// A READER MUST NOT INHERIT ITS WRITER'S AUTHORITY.
//
// The reader's dependency on a covering TEMPORARY WRITE root was recorded as a
// reference on that root — one reference standing for both the lifetime and the
// capability. Release the WRITER first and the reference kept the path in
// extraRoots, which is the list Roots() feeds validate(), so the surviving
// read-only holder could WRITE anywhere below a write grant that had already
// ended — including paths it never asked to read.
//
// This is TestAReadGrantIsReadableButNotWritable's property for temporary
// grants: read authority stays read authority, whichever holder leaves first.
func TestAReaderOutlivingItsWriterKeepsReadAuthorityOnly(t *testing.T) {
	t.Run("the writer releases first", func(t *testing.T) {
		scope, workspace, outer, inner := temporaryWriteFixture(t)

		writeRoot, releaseWrite, err := scope.AddTemporaryWrite(outer)
		if err != nil {
			t.Fatalf("temporary write: %v", err)
		}
		if writeRoot != outer {
			t.Fatalf("temporary write granted %q, want %q", writeRoot, outer)
		}
		readRoot, releaseRead, err := scope.AddTemporaryRead(inner)
		if err != nil {
			t.Fatalf("temporary read: %v", err)
		}
		if readRoot != inner {
			t.Fatalf("temporary read granted %q, want %q", readRoot, inner)
		}
		// The reader holds a read root of its own and NO reference on the write
		// root. Both halves matter: the first is what survives the writer, the
		// second is what stops the write root surviving with it.
		//
		// Errorf, not Fatalf: these say WHY the behavior below breaks, and a
		// bookkeeping check that aborts the run hides the behavior it explains.
		scope.mu.RLock()
		writeHolders := scope.tempWrites[outer]
		readHolders := scope.tempReads[inner]
		scope.mu.RUnlock()
		if writeHolders != 1 {
			t.Errorf("tempWrites[%s] = %d, want 1: the reader took a reference on the WRITE root", outer, writeHolders)
		}
		if readHolders != 1 {
			t.Errorf("tempReads[%s] = %d, want 1: the reader kept no read root of its own", inner, readHolders)
		}

		// The WRITE holder finishes; only the read-only holder is left.
		releaseWrite()

		if got := writeRootsBeyondWorkspace(scope); len(got) != 0 {
			t.Errorf("ExtraRoots() = %v, want none: the reader held the write root open", got)
		}
		if got, want := scope.ReadRoots(), []string{workspace, inner}; !slices.Equal(got, want) {
			t.Errorf("ReadRoots() = %v, want %v", got, want)
		}

		target := filepath.Join(inner, "audit-me.go")
		if block := scope.validateRead(target); block != nil {
			t.Errorf("the writer's cleanup revoked a live read grant on %q: %v", target, block)
		}
		block := scope.validate(target)
		if block == nil {
			t.Fatalf("a read-only holder may still WRITE %q after the write grant ended", target)
		}
		if block.Code != BlockOutsideWorkspace {
			t.Errorf("write block code = %q, want %q", block.Code, BlockOutsideWorkspace)
		}
		if block.Path != target {
			t.Errorf("write block path = %q, want %q", block.Path, target)
		}
		// The escalation was never confined to the path the reader asked for —
		// a borrowed reference carries the whole covering root — so a sibling
		// the reader never named has to be denied too.
		sibling := filepath.Join(outer, "sibling.txt")
		if block := scope.validate(sibling); block == nil {
			t.Errorf("a read-only holder of %q may still WRITE %q, which it never asked to read", inner, sibling)
		}

		releaseRead()
		if got, want := scope.ReadRoots(), []string{workspace}; !slices.Equal(got, want) {
			t.Errorf("ReadRoots() = %v, want %v: the read root outlived its last holder", got, want)
		}
	})

	// A CONTROL, NOT COVERAGE FOR THE FINDING. This subtest passes on the
	// unfixed tree too — the escalation needs the WRITER to go first, which the
	// subtest above exercises. It earns its place by pinning that the opposite
	// release order was not broken while fixing the dangerous one, and it is
	// labelled so nobody counts it as evidence the finding is closed.
	t.Run("the reader releases first (control: passes unfixed)", func(t *testing.T) {
		scope, _, outer, inner := temporaryWriteFixture(t)

		_, releaseWrite, err := scope.AddTemporaryWrite(outer)
		if err != nil {
			t.Fatalf("temporary write: %v", err)
		}
		_, releaseRead, err := scope.AddTemporaryRead(inner)
		if err != nil {
			t.Fatalf("temporary read: %v", err)
		}

		// The other order, which the fix must not break: the writer keeps FULL
		// authority for as long as it holds the grant.
		releaseRead()
		buildLog := filepath.Join(outer, "build.log")
		if block := scope.validate(buildLog); block != nil {
			t.Fatalf("the reader's cleanup revoked the live write grant on %q: %v", buildLog, block)
		}
		if got, want := writeRootsBeyondWorkspace(scope), []string{outer}; !slices.Equal(got, want) {
			t.Errorf("ExtraRoots() = %v, want %v", got, want)
		}

		releaseWrite()
		if block := scope.validate(buildLog); block == nil {
			t.Errorf("the write root %q outlived its only holder", outer)
		}
		if block := scope.validateRead(filepath.Join(inner, "audit-me.go")); block == nil {
			t.Errorf("the read root %q outlived its only holder", inner)
		}
	})
}

// A NARROWER WRITE HOLDER MUST NOT INHERIT ITS NEIGHBOUR'S TREE.
//
// The write-side twin of the read-side escalation above, and the same root
// cause: a nested holder used to take a reference on the BROADER temporary root
// that happened to cover it. That kept the nested holder alive, which was the
// point, but it kept the covering root alive to do it — and extraRoots is what
// validate() authorises writes from. Once the broad holder released, a caller
// that had asked for one subdirectory held write authority over the whole tree,
// siblings included. Reported by CodeRabbit.
func TestANestedWriteHolderKeepsOnlyItsOwnSubtree(t *testing.T) {
	scope, _, outsideBase := scopeOutsideRoots(t)
	outer := filepath.Join(outsideBase, "outer")
	inner := filepath.Join(outer, "inner")
	sibling := filepath.Join(outer, "sibling")
	for _, dir := range []string{outer, inner, sibling} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	_, releaseOuter, err := scope.AddTemporaryWrite(outer)
	if err != nil {
		t.Fatalf("temporary write on the broad root: %v", err)
	}
	_, releaseInner, err := scope.AddTemporaryWrite(inner)
	if err != nil {
		t.Fatalf("temporary write on the nested root: %v", err)
	}

	// THE BROAD HOLDER GOES FIRST — the nested holder outliving it is what
	// exposes whose authority it is actually holding.
	releaseOuter()

	if block := scope.validate(filepath.Join(inner, "mine.txt")); block != nil {
		t.Errorf("the nested holder lost the subtree it asked for: %v", block.Reason)
	}
	if block := scope.validate(filepath.Join(sibling, "not-mine.txt")); block == nil {
		t.Error("the nested holder can write a SIBLING it never asked for: the broad holder's authority outlived the broad holder")
	}
	if block := scope.validate(filepath.Join(outer, "not-mine.txt")); block == nil {
		t.Error("the nested holder can write the covering root itself after its holder released")
	}

	releaseInner()
	if block := scope.validate(filepath.Join(inner, "mine.txt")); block == nil {
		t.Error("the nested root outlived its only holder")
	}
}

// The opposite order, as a control: the nested holder releasing first must not
// disturb the broad holder that is still live.
func TestANestedWriteHolderReleasingFirstLeavesTheBroadGrant(t *testing.T) {
	scope, _, outsideBase := scopeOutsideRoots(t)
	outer := filepath.Join(outsideBase, "outer")
	inner := filepath.Join(outer, "inner")
	for _, dir := range []string{outer, inner} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	_, releaseOuter, err := scope.AddTemporaryWrite(outer)
	if err != nil {
		t.Fatal(err)
	}
	_, releaseInner, err := scope.AddTemporaryWrite(inner)
	if err != nil {
		t.Fatal(err)
	}

	releaseInner()
	if block := scope.validate(filepath.Join(inner, "still-ok.txt")); block != nil {
		t.Errorf("the nested holder's release revoked the broad holder's coverage: %v", block.Reason)
	}
	releaseOuter()
	if block := scope.validate(filepath.Join(inner, "gone.txt")); block == nil {
		t.Error("the tree outlived every holder")
	}
}

// TWO HOLDERS OF THE SAME ROOT SHARE ONE ENTRY. Only an exact match shares —
// a narrower request must not, which is what the escalation test above pins.
func TestTwoHoldersOfTheSameWriteRootShareOneEntry(t *testing.T) {
	scope, _, outsideBase := scopeOutsideRoots(t)
	target := filepath.Join(outsideBase, "shared")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	_, releaseFirst, err := scope.AddTemporaryWrite(target)
	if err != nil {
		t.Fatal(err)
	}
	_, releaseSecond, err := scope.AddTemporaryWrite(target)
	if err != nil {
		t.Fatal(err)
	}

	releaseFirst()
	if block := scope.validate(filepath.Join(target, "still-held.txt")); block != nil {
		t.Errorf("one holder's release revoked the other's grant on the same root: %v", block.Reason)
	}
	releaseSecond()
	if block := scope.validate(filepath.Join(target, "gone.txt")); block == nil {
		t.Error("the shared root outlived both holders")
	}
}

// writeRootsBeyondWorkspace is Roots() without the workspace entry.
//
// These tests need to see which BEYOND-WORKSPACE write roots a scope currently
// holds. There was an ExtraRoots() accessor that answered exactly this, but it
// had no production caller — it belongs to #829's child-grant propagation, whose
// consumer side is not on this branch, and @jatmn was right that carrying it here
// leaves unrelated production surface in a PR scoped to refcounting. The
// inspection is a test concern, so it lives in the tests.
func writeRootsBeyondWorkspace(scope *Scope) []string {
	all := scope.Roots()
	if len(all) == 0 {
		return nil
	}
	return append([]string(nil), all[1:]...)
}
