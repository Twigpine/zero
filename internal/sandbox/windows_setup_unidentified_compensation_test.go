package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AN IDENTITY THAT WAS NEVER ESTABLISHED IS NOT PERMISSION TO MUTATE.
//
// Both capture sites discarded runtimeDirIdentity's success flag, so a root that
// could not be opened when setup began was indistinguishable from one with no
// identity, and both mutation sites read the empty string as "skip the check".
// Compensation then wrote to, or removed from, whatever answered to the pathname
// afterwards. That is elevated compensation reaching an object this run cannot
// prove it touched.
func TestStampCompensationRefusesAnUnidentifiedRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "zero", "runtime", "v1", "abcd")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create the runtime root: %v", err)
	}
	stamp := windowsSandboxRuntimeStampPath(root, testStampPlanHash)
	const foreign = "belongs to whoever put it here"
	if err := os.WriteFile(stamp, []byte(foreign), 0o600); err != nil {
		t.Fatalf("seed the stamp: %v", err)
	}

	// The shape the old capture produced when runtimeDirIdentity failed.
	snapshot := windowsSandboxStampSnapshot{
		path:           stamp,
		prior:          []byte("this run's stamp"),
		priorState:     runtimeStampPresent,
		root:           root,
		rootIdentified: false,
	}

	err := snapshot.restore()
	if err == nil {
		t.Fatal("compensation proceeded with an identity it never established")
	}
	if !strings.Contains(err.Error(), "could not be identified") {
		t.Errorf("the refusal does not say why: %v", err)
	}

	// The object is untouched, which is the half that matters.
	after, readErr := os.ReadFile(stamp)
	if readErr != nil {
		t.Fatalf("read the stamp back: %v", readErr)
	}
	if string(after) != foreign {
		t.Errorf("compensation rewrote a stamp it could not prove was this run's: %q", string(after))
	}
}

// The same rule for the directory removal, where the mutation is a delete.
func TestDirectoryCompensationRefusesAnUnidentifiedDirectory(t *testing.T) {
	parent := t.TempDir()
	created := filepath.Join(parent, "zero")
	if err := os.MkdirAll(created, 0o700); err != nil {
		t.Fatalf("create the directory: %v", err)
	}

	rollback := windowsRuntimeRootRollback{
		created: []windowsCreatedRuntimeDir{{path: created, identified: false}},
	}
	err := rollback.run()
	if err == nil {
		t.Fatal("an unidentified directory was removed by pathname")
	}
	if !strings.Contains(err.Error(), "could not be identified") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	if _, statErr := os.Stat(created); statErr != nil {
		t.Errorf("the directory was removed despite the unproven identity: %v", statErr)
	}
}

// And a root that is simply ABSENT is not the same case: there is no object to
// confuse, nothing of a previous run to put back, and the created-directory
// rollback owns the cleanup. Refusing here would make every fresh setup report a
// compensation failure.
func TestStampCompensationStaysQuietWhenTheRootIsAbsent(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "zero", "runtime", "v1", "abcd")
	snapshot := windowsSandboxStampSnapshot{
		path:           windowsSandboxRuntimeStampPath(root, testStampPlanHash),
		root:           root,
		rootIdentified: false,
	}
	if err := snapshot.restore(); err != nil {
		t.Fatalf("an absent root was treated as a compensation failure: %v", err)
	}
}
