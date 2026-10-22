//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// THE CREATION ITSELF HAS TO ESTABLISH THE IDENTITY.
//
// Win32 CreateFile cannot create a directory whatever disposition it is given,
// so the CREATE_NEW attempt fell through to os.Mkdir plus a separate reopen on
// EVERY real creation and the documented creation-handle contract never once
// held. The runtime parent belongs to the ordinary user, who can rename the new
// directory A away and drop an ordinary directory B at the predictable name in
// between, and the ledger then records B for a directory this run never made.
//
// Driving the interleaving is not the point, and a barrier there would only
// prove the window was still measurable. The property is that no reopen exists
// to be raced: whatever the name resolves to afterwards, the recorded identity
// is the object the create returned.
func TestCreatedRuntimeDirIdentityComesFromTheCreation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "created")

	// THE DETERMINISTIC BARRIER. The old shape resolved the pathname a second
	// time after os.Mkdir, and that reopen is the whole window. Counting the
	// resolutions turns "the creation establishes the identity" into something
	// checked: on Windows the create returns the handle, so this must be zero.
	resolutions := 0
	restore := runtimeIdentityAfterCreate
	runtimeIdentityAfterCreate = func(p string) (string, bool) {
		resolutions++
		return restore(p)
	}
	t.Cleanup(func() { runtimeIdentityAfterCreate = restore })

	identity, identified, err := createRuntimeDirIdentified(path)
	if err != nil || !identified {
		t.Fatalf("create: identity=%q identified=%v err=%v", identity, identified, err)
	}
	if resolutions != 0 {
		t.Errorf("the creation resolved the pathname again %d time(s); identity must come from the creation handle", resolutions)
	}
	info, statErr := os.Stat(path)
	if statErr != nil || !info.IsDir() {
		t.Fatalf("no directory was created: err=%v", statErr)
	}

	// Substitute the whole directory the way the parent's owner could, then ask
	// what the name says now. The record must still describe what was created.
	aside := filepath.Join(root, "moved-aside")
	if err := os.Rename(path, aside); err != nil {
		t.Skipf("cannot rename the runtime directory on this filesystem: %v", err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}

	substitute, ok := runtimeDirIdentity(path)
	if !ok {
		t.Fatal("the substitute could not be identified, so the comparison proves nothing")
	}
	if identity == substitute {
		t.Fatal("SETUP INVALID: the substitute has the same identity as the created directory")
	}
	moved, ok := runtimeDirIdentity(aside)
	if !ok {
		t.Fatal("the created directory could not be identified after the rename")
	}
	if identity != moved {
		t.Errorf("the recorded identity %q describes neither the created directory (%q) nor anything this run owns", identity, moved)
	}
}

// A component another process created first must stay "not ours", and it must
// keep saying so through os.IsExist, which is what the caller asks and which
// does not unwrap a %w chain.
func TestCreatedRuntimeDirRefusesAnExistingName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "created")
	if _, _, err := createRuntimeDirIdentified(path); err != nil {
		t.Fatalf("first create: %v", err)
	}
	identity, identified, err := createRuntimeDirIdentified(path)
	if !os.IsExist(err) {
		t.Errorf("creating over an existing directory returned %v, want an IsExist error", err)
	}
	if identified || identity != "" {
		t.Errorf("a refused create still produced an ownership record: identity=%q identified=%v", identity, identified)
	}
}

// THE THREE STATES ARE DIFFERENT FACTS.
//
// "Read it and there was nothing" and "could not read it" both used to arrive as
// existed=false. The stamp writer uses FILE_OVERWRITE_IF and can replace an
// existing stamp even where the read was denied, so that lie let a setup which
// then FAILED delete an attestation it had no record of, leaving the previous
// run's marker pointing at a runtime root it can no longer prove.
func TestRuntimeStampSnapshotSeparatesAbsentPresentAndUnknown(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		root := t.TempDir()
		_, _, prior, state, err := snapshotRuntimeStampBound(root, windowsSandboxRuntimeStampName(testStampPlanHash))
		if err != nil {
			t.Fatalf("a readable root with no stamp is not an error: %v", err)
		}
		if state != runtimeStampAbsent {
			t.Errorf("state = %v, want absent", state)
		}
		if prior != nil {
			t.Errorf("absent produced prior bytes %q", prior)
		}
	})

	t.Run("present", func(t *testing.T) {
		root := t.TempDir()
		want := []byte("prior-attestation")
		if err := os.WriteFile(filepath.Join(root, windowsSandboxRuntimeStampName(testStampPlanHash)), want, 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, prior, state, err := snapshotRuntimeStampBound(root, windowsSandboxRuntimeStampName(testStampPlanHash))
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		if state != runtimeStampPresent {
			t.Errorf("state = %v, want present", state)
		}
		if string(prior) != string(want) {
			t.Errorf("prior = %q, want %q", prior, want)
		}
	})

	// A stamp NAME that cannot be read as a file. The child open is refused for a
	// reason that is emphatically not "not found", which is the whole distinction.
	t.Run("unknown", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, windowsSandboxRuntimeStampName(testStampPlanHash)), 0o700); err != nil {
			t.Fatal(err)
		}
		_, _, prior, state, err := snapshotRuntimeStampBound(root, windowsSandboxRuntimeStampName(testStampPlanHash))
		if err == nil {
			t.Fatal("an unreadable stamp was reported as a successful snapshot")
		}
		if state != runtimeStampUnknown {
			t.Errorf("state = %v, want unknown", state)
		}
		if prior != nil {
			t.Errorf("unknown produced prior bytes %q", prior)
		}
	})
}

// AND UNKNOWN MUST NOT AUTHORIZE COMPENSATION.
//
// The forward mutation is refused before it begins, so this asserts the second
// half: a record that somehow reached compensation with an unproven prior state
// leaves the current stamp alone and says so, rather than deleting it and
// returning with nothing to put back.
func TestUnknownPriorStampIsNeverCompensated(t *testing.T) {
	root := t.TempDir()
	stampPath := filepath.Join(root, windowsSandboxRuntimeStampName(testStampPlanHash))
	current := []byte("the-attestation-of-the-previous-successful-setup")
	if err := os.WriteFile(stampPath, current, 0o600); err != nil {
		t.Fatal(err)
	}
	identity, identified := runtimeDirIdentity(root)
	if !identified {
		t.Fatal("SETUP INVALID: the runtime root could not be identified")
	}

	snapshot := windowsSandboxStampSnapshot{
		path:           stampPath,
		priorState:     runtimeStampUnknown,
		root:           root,
		rootIdentity:   identity,
		rootIdentified: true,
	}
	err := snapshot.restore()
	if err == nil {
		t.Fatal("compensation acted on a prior state it never established, and reported success")
	}

	after, readErr := os.ReadFile(stampPath)
	if readErr != nil {
		t.Fatalf("the existing stamp was destroyed by a rollback that had nothing to restore: %v", readErr)
	}
	if string(after) != string(current) {
		t.Errorf("the existing stamp was rewritten: got %q, want %q", after, current)
	}
}

// And setup refuses BEFORE the ACL and stamp are applied, which is the half that
// keeps the writer from running at all.
func TestSetupRefusesAnUnreadablePriorStamp(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, windowsSandboxRuntimeStampName(testStampPlanHash)), 0o700); err != nil {
		t.Fatal(err)
	}
	snapshot, err := snapshotWindowsSandboxRuntimeStamp(root, testStampPlanHash)
	if err == nil {
		t.Fatal("setup accepted a snapshot that could not read the prior stamp")
	}
	if snapshot.priorState != runtimeStampUnknown {
		t.Errorf("the refused snapshot carried state %v, want unknown", snapshot.priorState)
	}
}
