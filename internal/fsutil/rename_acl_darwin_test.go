//go:build darwin

package fsutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteFileAtomicPreservesOrRefusesNativeACL pins the fail-closed contract
// for a Darwin native (kauth/FILESEC) ACL: replacing the destination either
// carries the ACL over to the new inode, or the replacement is refused and the
// original file is left byte-for-byte intact. What must never happen is a
// successful replacement that silently drops the ACL.
func TestWriteFileAtomicPreservesOrRefusesNativeACL(t *testing.T) {
	if _, err := exec.LookPath("chmod"); err != nil {
		t.Skip("chmod not on PATH")
	}
	if _, err := exec.LookPath("ls"); err != nil {
		t.Skip("ls not on PATH")
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "restricted.txt")
	original := "old"
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	set := exec.Command("chmod", "+a", "user:nobody deny read", target)
	if out, err := set.CombinedOutput(); err != nil {
		t.Skipf("chmod +a failed (ACLs unavailable on this filesystem): %v\n%s", err, out)
	}

	acl, err := readNativeACL(target)
	if err != nil {
		t.Skipf("cannot read the native ACL that chmod +a just set: %v", err)
	}
	if len(acl) == 0 {
		t.Skip("chmod +a reported success but no native ACL is present")
	}
	before, err := exec.Command("ls", "-le", target).CombinedOutput()
	if err != nil {
		t.Fatalf("ls -le before: %v\n%s", err, before)
	}
	if !strings.Contains(string(before), "deny") {
		t.Skipf("named deny entry did not stick; ls -le:\n%s", before)
	}

	replaceErr := WriteFileAtomic(target, []byte("new"), 0o600)
	if replaceErr != nil {
		// Refusal path: the error is acceptable only if the destination was
		// not mutated on the way out.
		got, rerr := os.ReadFile(target)
		if rerr != nil {
			t.Fatalf("WriteFileAtomic failed (%v) and the original is unreadable: %v", replaceErr, rerr)
		}
		if string(got) != original {
			t.Fatalf("WriteFileAtomic failed (%v) but mutated the destination to %q", replaceErr, got)
		}
		return
	}

	// Preservation path: the replacement succeeded, so the deny entry must
	// still be attached to the new inode.
	after, err := readNativeACL(target)
	if err != nil {
		t.Fatalf("WriteFileAtomic succeeded but the native ACL became unreadable: %v", err)
	}
	if len(after) == 0 {
		t.Fatalf("WriteFileAtomic replaced the destination and lost the native ACL")
	}
	afterListing, err := exec.Command("ls", "-le", target).CombinedOutput()
	if err != nil {
		t.Fatalf("ls -le after: %v\n%s", err, afterListing)
	}
	if !strings.Contains(string(afterListing), "deny") {
		t.Fatalf("native ACL deny entry was lost after replacement\nbefore:\n%s\nafter:\n%s", before, afterListing)
	}
}

func TestPreserveNativeACLRemovesInheritedACLWhenSourceHasNone(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "restricted")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("chmod", "-N", target).CombinedOutput(); err != nil {
		t.Fatalf("clear ACL: %s: %v", out, err)
	}
	if out, err := exec.Command("chmod", "+a", "user:nobody allow read,file_inherit,directory_inherit", dir).CombinedOutput(); err != nil {
		t.Skipf("parent ACL unavailable on this filesystem: %s: %v", out, err)
	}
	// Exercise preservation directly so private creation cannot mask a nil/no-op bug.
	staging, err := os.CreateTemp(dir, "inherited-*")
	if err != nil {
		t.Fatal(err)
	}
	defer staging.Close()
	before, err := exec.Command("ls", "-le", staging.Name()).CombinedOutput()
	if err != nil {
		t.Fatalf("ls -le: %v\n%s", err, before)
	}
	if !strings.Contains(string(before), "nobody allow read") {
		// The precondition is filesystem inheritance, not the helper under
		// test: a host whose filesystem does not apply file_inherit entries to
		// new files cannot exercise the absence case at all.
		t.Skipf("filesystem did not inherit the directory ACL onto a new file; ls -le:\n%s", before)
	}
	if err := preserveNativeACL(staging, target); err != nil {
		t.Fatal(err)
	}
	after, err := exec.Command("ls", "-le", staging.Name()).CombinedOutput()
	if err != nil || strings.Contains(string(after), "nobody") {
		t.Fatalf("absence of source ACL was not preserved: %s, %v", after, err)
	}
	if err := WriteFileAtomic(target, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err = exec.Command("ls", "-le", target).CombinedOutput()
	if err != nil || strings.Contains(string(after), "nobody") {
		t.Fatalf("replacement inherited a grant: %s, %v", after, err)
	}
	fresh := filepath.Join(dir, "fresh")
	if err := WriteFileAtomic(fresh, []byte("public"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err = exec.Command("ls", "-le", fresh).CombinedOutput()
	if err != nil || !strings.Contains(string(after), "nobody allow read") {
		t.Fatalf("new file lost inheritance: %s, %v", after, err)
	}
}

func TestPrivateStagingDarwinCreationSuppressesInheritance(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("chmod", "+a", "user:nobody allow read,file_inherit,directory_inherit", dir).CombinedOutput(); err != nil {
		t.Skipf("parent ACL unavailable on this filesystem: %s: %v", out, err)
	}
	count := 0
	previous := privateCreationObserver
	privateCreationObserver = func(path string) {
		count++
		listing, err := exec.Command("ls", "-lde", path).CombinedOutput()
		if err != nil || strings.Contains(string(listing), "nobody") {
			t.Fatalf("initial staging inherited access: %s, %v", listing, err)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("initial staging mode: %v, %v", info, err)
		}
	}
	defer func() { privateCreationObserver = previous }()
	file, err := CreatePrivateTemp(dir, ".zero-tmp-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file.Name()); err != nil {
		t.Fatal(err)
	}
	stagingDir, err := CreatePrivateTempDir(dir, ".zero-fmt-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(stagingDir); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("creation observations: %d", count)
	}
}

func TestPreserveNativeACLReadFailureDoesNotMeanAbsence(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "staging")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := preserveNativeACL(file, filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("unreadable source ACL treated as absent")
	}
}
