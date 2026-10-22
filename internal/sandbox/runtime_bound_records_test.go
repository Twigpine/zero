package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// THE TWO HALVES OF A ROLLBACK RECORD MUST DESCRIBE ONE OBJECT.
//
// The snapshot used to read the identity through a handle, close it, and then
// re-resolve the pathname to read the stamp. A rename between those pairs one
// directory's identity with another's bytes, and a rollback that correctly
// proves it holds the first then writes the second's contents into it.
//
// The window itself needs an elevated installer racing an unelevated renamer and
// is not reproducible here, so this pins the contract the binding provides: both
// facts come back together, and they agree with the object actually at the path.
func TestStampSnapshotPairsIdentityWithItsOwnBytes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	const contents = "the stamp that belongs to this directory"
	if err := os.WriteFile(windowsSandboxRuntimeStampPath(root, testStampPlanHash), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	identity, identified, prior, state, err := snapshotRuntimeStampBound(root, windowsSandboxRuntimeStampName(testStampPlanHash))
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	existed := state == runtimeStampPresent
	if !identified {
		t.Fatal("the snapshot established no identity for a directory that exists")
	}
	if !existed || string(prior) != contents {
		t.Fatalf("prior stamp = %q existed=%v, want %q", string(prior), existed, contents)
	}
	if direct, ok := runtimeDirIdentity(root); !ok || direct != identity {
		t.Errorf("snapshot identity %q does not describe the directory at the path (%q)", identity, direct)
	}
}

// An absent stamp still establishes the identity, because that came from the
// directory handle and not from the stamp read.
func TestStampSnapshotIdentifiesARootWithNoStamp(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	identity, identified, _, state, err := snapshotRuntimeStampBound(root, windowsSandboxRuntimeStampName(testStampPlanHash))
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	existed := state == runtimeStampPresent
	if !identified || identity == "" {
		t.Error("a root with no stamp established no identity")
	}
	if existed {
		t.Error("reported a prior stamp that does not exist")
	}
}

// A created directory's identity must come from the creation, so the ledger
// cannot record an object this run did not make.
func TestCreatedDirectoryIdentityDescribesWhatWasCreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "created")

	identity, identified, err := createRuntimeDirIdentified(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !identified || identity == "" {
		t.Fatal("creation established no identity")
	}
	if info, statErr := os.Stat(path); statErr != nil || !info.IsDir() {
		t.Fatalf("the directory was not created: %v", statErr)
	}
	if direct, ok := runtimeDirIdentity(path); !ok || direct != identity {
		t.Errorf("creation identity %q does not describe the directory now at the path (%q)", identity, direct)
	}

	// Creating over something that exists is the caller's already-handled signal.
	if _, _, again := createRuntimeDirIdentified(path); !os.IsExist(again) {
		t.Errorf("creating over an existing directory returned %v, want an IsExist error", again)
	}
}
