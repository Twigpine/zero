//go:build linux

package fsutil

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFileAtomicPreservesRestrictivePOSIXACL(t *testing.T) {
	if _, err := exec.LookPath("setfacl"); err != nil {
		t.Skip("setfacl not on PATH; POSIX ACL preservation is not exercised on this host")
	}
	if _, err := exec.LookPath("getfacl"); err != nil {
		t.Skip("getfacl not on PATH; POSIX ACL preservation is not exercised on this host")
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "restricted.txt")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	set := exec.Command("setfacl", "-m", "u:65534:---", target)
	if out, err := set.CombinedOutput(); err != nil {
		t.Skipf("setfacl failed (filesystem may lack ACL support): %v\n%s", err, out)
	}

	before, err := exec.Command("getfacl", "-cp", target).CombinedOutput()
	if err != nil {
		t.Fatalf("getfacl before: %v\n%s", err, before)
	}
	if !namedUserACLDenied(before) {
		t.Skipf("named-user ACL did not stick; getfacl:\n%s", before)
	}

	if err := WriteFileAtomic(target, []byte("new"), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "new" {
		t.Fatalf("content = %q, want %q", got, "new")
	}

	after, err := exec.Command("getfacl", "-cp", target).CombinedOutput()
	if err != nil {
		t.Fatalf("getfacl after: %v\n%s", err, after)
	}
	if !namedUserACLDenied(after) {
		t.Fatalf("restrictive named-user ACL was lost after replacement\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func namedUserACLDenied(listing []byte) bool {
	return bytes.Contains(listing, []byte("user:65534:---")) ||
		bytes.Contains(listing, []byte("user:nobody:---")) ||
		bytes.Contains(listing, []byte("user:nfsnobody:---"))
}

func namedUserACLPresent(listing []byte) bool {
	return bytes.Contains(listing, []byte("user:65534:")) ||
		bytes.Contains(listing, []byte("user:nobody:")) ||
		bytes.Contains(listing, []byte("user:nfsnobody:"))
}

func TestWriteFileAtomicDropsInheritedAccessACL(t *testing.T) {
	if _, err := exec.LookPath("setfacl"); err != nil {
		t.Skip("setfacl not on PATH; POSIX ACL inheritance is not exercised on this host")
	}
	if _, err := exec.LookPath("getfacl"); err != nil {
		t.Skip("getfacl not on PATH; POSIX ACL inheritance is not exercised on this host")
	}

	base := t.TempDir()
	dir := filepath.Join(base, "shared")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("setfacl", "-d", "-m", "u:65534:r--", dir).CombinedOutput(); err != nil {
		t.Skipf("setfacl default ACL failed (filesystem may lack ACL support): %v\n%s", err, out)
	}

	target := filepath.Join(dir, "plain.txt")
	if err := os.WriteFile(target, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("setfacl", "-b", target).CombinedOutput(); err != nil {
		t.Skipf("setfacl -b failed: %v\n%s", err, out)
	}
	before, err := exec.Command("getfacl", "-cp", target).CombinedOutput()
	if err != nil {
		t.Fatalf("getfacl before: %v\n%s", err, before)
	}
	if namedUserACLPresent(before) {
		t.Skipf("source still has a named-user ACL; cannot assert inheritance removal\ngetfacl:\n%s", before)
	}

	if err := WriteFileAtomic(target, []byte("new"), 0o640); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	after, err := exec.Command("getfacl", "-cp", target).CombinedOutput()
	if err != nil {
		t.Fatalf("getfacl after: %v\n%s", err, after)
	}
	if namedUserACLPresent(after) {
		t.Fatalf("access ACL inherited from the directory default survived the replacement\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// Observe the creation boundary, not only the metadata-ready or publication state.
func TestPrivateStagingCreationMasksInheritedPOSIXGrants(t *testing.T) {
	for _, commandName := range []string{"setfacl", "getfacl"} {
		if _, err := exec.LookPath(commandName); err != nil {
			t.Skip(commandName + " not installed")
		}
	}
	for _, denied := range []bool{false, true} {
		t.Run(map[bool]string{false: "source-without-acl", true: "source-named-deny"}[denied], func(t *testing.T) {
			dir := t.TempDir()
			if out, err := exec.Command("setfacl", "-d", "-m", "u:65534:r-x", dir).CombinedOutput(); err != nil {
				t.Fatalf("default ACL: %s: %v", out, err)
			}
			target := filepath.Join(dir, "target")
			mode := os.FileMode(0o640)
			if denied {
				mode = 0o644
			}
			if err := os.WriteFile(target, []byte("old"), mode); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command("setfacl", "-b", target).CombinedOutput(); err != nil {
				t.Fatalf("clear ACL: %s: %v", out, err)
			}
			if err := os.Chmod(target, mode); err != nil {
				t.Fatal(err)
			}
			if denied {
				if out, err := exec.Command("setfacl", "-m", "u:65534:---", target).CombinedOutput(); err != nil {
					t.Fatalf("deny ACL: %s: %v", out, err)
				}
			}
			info, err := os.Stat(target)
			if err != nil || info.Mode().Perm() != mode {
				t.Fatalf("fixture destination mode: %v, %v, want %04o", info, err, mode)
			}
			count := 0
			previous := privateCreationObserver
			privateCreationObserver = func(path string) {
				count++
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm()&0o077 != 0 {
					t.Fatalf("initial staging exposed inherited access: mode %04o", info.Mode().Perm())
				}
				acl, err := exec.Command("getfacl", "-cpn", path).CombinedOutput()
				if err != nil {
					t.Fatalf("creation ACL: %s: %v", acl, err)
				}
				if !strings.Contains(string(acl), "mask::---") {
					t.Fatalf("initial inherited named grant is not masked: %s", acl)
				}
				if !info.IsDir() && info.Size() != 0 {
					t.Fatal("creation observer ran after content write")
				}
			}
			t.Cleanup(func() { privateCreationObserver = previous })
			if err := WriteFileAtomic(target, []byte("new"), mode); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("replace failed")
			err = writeFileAtomic(target, []byte("must not land"), mode, func(string, string) error { return failure })
			if !errors.Is(err, failure) {
				t.Fatal(err)
			}
			got, err := os.ReadFile(target)
			if err != nil || string(got) != "new" {
				t.Fatalf("destination = %q, %v", got, err)
			}
			acl, err := exec.Command("getfacl", "-cpn", target).CombinedOutput()
			if err != nil {
				t.Fatal(err)
			}
			if denied && !namedUserACLDenied(acl) || !denied && namedUserACLPresent(acl) {
				t.Fatalf("destination ACL changed: %s", acl)
			}
			fmtDir, err := CreatePrivateTempDir(dir, ".zero-fmt-*")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(fmtDir); err != nil {
				t.Fatal(err)
			}
			if count != 3 {
				t.Fatalf("creation observations = %d, want 3", count)
			}
			leftovers, err := filepath.Glob(filepath.Join(dir, ".zero-*"))
			if err != nil || len(leftovers) != 0 {
				t.Fatalf("staging leftovers: %v, %v", leftovers, err)
			}
			// Genuinely new publications still receive the ordinary inherited ACL.
			fresh := filepath.Join(dir, "fresh")
			if err := WriteFileAtomic(fresh, []byte("public"), 0o644); err != nil {
				t.Fatal(err)
			}
			acl, err = exec.Command("getfacl", "-cpn", fresh).CombinedOutput()
			if err != nil || !namedUserACLPresent(acl) || strings.Contains(string(acl), "#effective:---") {
				t.Fatalf("new file lost normal inheritance: %s, %v", acl, err)
			}
		})
	}
}
