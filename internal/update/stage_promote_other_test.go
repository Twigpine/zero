//go:build !windows

package update

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestPromoteRefusesASubstitutedStagingEntry is the regression test for the live
// handoff half of #742: randomizing the staging name and creating it exclusively
// stops PRE-creation, but not substitution after the verified bytes are written.
// POSIX cannot rename by descriptor, so staging happens inside a private 0700
// directory the attacker cannot write to, and promote additionally verifies the
// entry still names the object it wrote before renaming it into place.
func TestPromoteRefusesASubstitutedStagingEntry(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero")
	if err := os.WriteFile(targetPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	sourcePath := filepath.Join(t.TempDir(), "new-binary")
	if err := os.WriteFile(sourcePath, []byte("verified-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}

	staged, err := stageBinary(sourcePath, targetPath)
	if err != nil {
		t.Fatalf("stageBinary: %v", err)
	}
	defer staged.discard()

	// First line of defence: the staging directory is private, so a principal who
	// can write in the installation directory cannot reach the entry at all.
	info, err := os.Stat(staged.dir)
	if err != nil {
		t.Fatalf("Stat staging directory: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("staging directory mode = %#o, want 0700", perm)
	}

	// Second line of defence: rehearse the substitution anyway (the test runs as
	// the directory's owner, so it can do what an attacker cannot) and require
	// promote to refuse instead of installing the substitute.
	substitute := filepath.Join(staged.dir, "substitute")
	if err := os.WriteFile(substitute, []byte("attacker-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile substitute: %v", err)
	}
	if err := os.Rename(substitute, staged.path); err != nil {
		t.Fatalf("Rename substitute over the staging entry: %v", err)
	}

	if err := staged.promote(targetPath); err == nil {
		t.Fatal("promote installed a substituted staging entry, want a refusal")
	}
	installed, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("ReadFile target: %v", err)
	}
	if string(installed) != "old-binary" {
		t.Fatalf("target = %q, want the original binary left in place", installed)
	}
}

// TestPromoteSurvivesAncestorDirectoryReplacement is the regression test for a
// review finding on PR #751: the private staging directory's 0700 mode
// protects its CONTENTS from a principal who can write in the installation
// directory, but not its own directory ENTRY — that principal can still
// rename the staging directory itself out of the way and recreate a
// look-alike at the same path, with an attacker file at the same basename,
// in the gap between verifyStagedIdentity returning and the final rename
// running. A plain pathname rename would re-resolve through the impostor at
// that point; promote must instead stay bound to the exact directory whose
// identity was already checked, via the directory descriptor opened when the
// staging directory was created.
func TestPromoteSurvivesAncestorDirectoryReplacement(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero")
	if err := os.WriteFile(targetPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	sourcePath := filepath.Join(t.TempDir(), "new-binary")
	if err := os.WriteFile(sourcePath, []byte("verified-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}

	staged, err := stageBinary(sourcePath, targetPath)
	if err != nil {
		t.Fatalf("stageBinary: %v", err)
	}
	discarded := false
	defer func() {
		if !discarded {
			staged.discard()
		}
	}()

	// Rehearse the ancestor swap: move the real staging directory aside (the
	// test runs as its owner, so it can do what a merely writable-parent
	// attacker — who only needs rename rights on dir, not on the staging
	// directory's own contents — can also do), then recreate a look-alike at
	// the original path with an attacker file at the same basename.
	base := filepath.Base(staged.path)
	movedDir := staged.dir + "-moved"
	if err := os.Rename(staged.dir, movedDir); err != nil {
		t.Fatalf("Rename staging directory aside: %v", err)
	}
	if err := os.Mkdir(staged.dir, 0o700); err != nil {
		t.Fatalf("Mkdir impostor staging directory: %v", err)
	}
	impostorFile := filepath.Join(staged.dir, base)
	if err := os.WriteFile(impostorFile, []byte("attacker-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile impostor file: %v", err)
	}

	if err := staged.promote(targetPath); err != nil {
		t.Fatalf("promote: %v", err)
	}
	installed, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("ReadFile target: %v", err)
	}
	if string(installed) != "verified-binary" {
		t.Fatalf("target = %q, want the verified bytes from the original staging directory", installed)
	}
	staged.discard()
	discarded = true
	// The impostor must be left untouched: promote should never have looked at
	// it, let alone consumed or removed it during deferred cleanup.
	if impostor, err := os.ReadFile(impostorFile); err != nil {
		t.Fatalf("ReadFile impostor file: %v", err)
	} else if string(impostor) != "attacker-binary" {
		t.Fatalf("impostor file = %q, want it left untouched", impostor)
	}
}

func TestCreateStagedBinaryRejectsDirectoryReplacementBeforeOpen(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero")
	var impostorMarker string
	original := openStagingDirectory
	openStagingDirectory = func(parentFD int, name string) (int, error) {
		path := filepath.Join(dir, name)
		if err := os.Rename(path, path+"-original"); err != nil {
			t.Fatalf("Rename original staging directory: %v", err)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatalf("Mkdir impostor: %v", err)
		}
		impostorMarker = filepath.Join(path, "keep")
		if err := os.WriteFile(impostorMarker, []byte("attacker"), 0o600); err != nil {
			t.Fatalf("WriteFile impostor marker: %v", err)
		}
		return original(parentFD, name)
	}
	defer func() { openStagingDirectory = original }()

	if staged, err := createStagedBinary(targetPath); err == nil {
		staged.discard()
		t.Fatal("createStagedBinary accepted a directory replaced before open")
	}
	if marker, err := os.ReadFile(impostorMarker); err != nil {
		t.Fatalf("substituted directory was removed during failed creation: %v", err)
	} else if string(marker) != "attacker" {
		t.Fatalf("substituted directory marker = %q, want it preserved", marker)
	}
}

func TestCreateStagedBinaryCleansUpAfterDirectoryOpenFailure(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero")
	original := openStagingDirectory
	openStagingDirectory = func(parentFD int, name string) (int, error) {
		return -1, errors.New("forced open failure")
	}
	defer func() { openStagingDirectory = original }()

	if staged, err := createStagedBinary(targetPath); err == nil {
		staged.discard()
		t.Fatal("createStagedBinary succeeded despite a forced directory-open failure")
	}
	assertNoStagingLeftovers(t, dir)
}

// TestInstallBinaryInstallsVerifiedBytes is the success control: the ordinary
// path must still install the staged bytes, executable, with nothing left over.
func TestInstallBinaryInstallsVerifiedBytes(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero")
	if err := os.WriteFile(targetPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	sourcePath := filepath.Join(t.TempDir(), "new-binary")
	if err := os.WriteFile(sourcePath, []byte("verified-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}

	if err := installBinary(sourcePath, targetPath); err != nil {
		t.Fatalf("installBinary: %v", err)
	}
	installed, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("ReadFile installed: %v", err)
	}
	if string(installed) != "verified-binary" {
		t.Fatalf("installed binary = %q, want the verified bytes", installed)
	}
	info, err := os.Stat(targetPath)
	if err != nil {
		t.Fatalf("Stat installed: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("installed binary mode = %#o, want the executable bit set", info.Mode().Perm())
	}
	assertNoStagingLeftovers(t, dir)
}

// TestInstallBinaryCleansUpWhenStagingFails covers the cleanup ordering: a
// failure after the staging object exists must not leave it behind, because each
// attempt now uses a fresh random name that the next attempt never reuses.
func TestInstallBinaryCleansUpWhenStagingFails(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "zero")
	if err := os.WriteFile(targetPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}

	if err := installBinary(filepath.Join(t.TempDir(), "missing-source"), targetPath); err == nil {
		t.Fatal("installBinary with an unreadable source succeeded, want error")
	}
	assertNoStagingLeftovers(t, dir)
}
