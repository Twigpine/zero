//go:build linux || darwin || freebsd || netbsd

package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// swapXattrSeams replaces the xattr syscall seams for the duration of a test
// and restores them on cleanup.
func swapXattrSeams(t *testing.T) {
	t.Helper()
	priorList, priorGet, priorSet, priorRemove := xattrListFunc, xattrGetFunc, xattrSetFunc, xattrRemoveFunc
	t.Cleanup(func() {
		xattrListFunc, xattrGetFunc, xattrSetFunc, xattrRemoveFunc = priorList, priorGet, priorSet, priorRemove
	})
}

// A denied xattr set is fatal even for security.selinux. The previous code
// swallowed EACCES/EPERM for that name, which let WriteFileAtomic publish a
// replacement whose SELinux label had been silently dropped.
func TestPreserveXattrsFailsClosedOnSELinuxDenial(t *testing.T) {
	swapXattrSeams(t)
	xattrListFunc = func(string) ([]string, error) { return []string{"security.selinux"}, nil }
	xattrGetFunc = func(string, string) ([]byte, error) {
		return []byte("system_u:object_r:etc_t:s0"), nil
	}
	xattrSetFunc = func(int, string, []byte, int) error { return unix.EACCES }

	file, err := os.CreateTemp(t.TempDir(), "xattr-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	err = preserveXattrs(file, "source")
	if err == nil {
		t.Fatal("a denied security.selinux set must fail closed")
	}
	if !errors.Is(err, unix.EACCES) {
		t.Fatalf("error %v does not wrap EACCES", err)
	}
}

// A set failure on any attribute aborts the atomic replacement, removes the
// staging file, and leaves the destination bytes intact.
func TestWriteFileAtomicFailsClosedWhenXattrPreservationFails(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(target, "user.zero_test", []byte("value"), 0); err != nil {
		t.Skipf("user xattrs unavailable: %v", err)
	}

	swapXattrSeams(t)
	xattrSetFunc = func(int, string, []byte, int) error { return unix.EACCES }

	err := WriteFileAtomic(target, []byte("replacement"), 0o600)
	if err == nil {
		t.Fatal("WriteFileAtomic must fail when authorization metadata cannot be preserved")
	}
	if !errors.Is(err, unix.EACCES) {
		t.Fatalf("error %v does not wrap EACCES", err)
	}
	if got, readErr := os.ReadFile(target); readErr != nil || string(got) != "original" {
		t.Fatalf("destination mutated to %q, readErr=%v", got, readErr)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".zero-tmp-") {
			t.Fatalf("staging file %q was left behind", entry.Name())
		}
	}
}

func TestPreserveXattrsFailsClosedWhenListedAttributeGetReturnsENOTSUP(t *testing.T) {
	swapXattrSeams(t)
	xattrListFunc = func(string) ([]string, error) { return []string{posixACLAccessXattr}, nil }
	xattrGetFunc = func(string, string) ([]byte, error) { return nil, unix.EOPNOTSUPP }

	file, err := os.CreateTemp(t.TempDir(), "xattr-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	err = preserveXattrs(file, "source")
	if !errors.Is(err, unix.EOPNOTSUPP) {
		t.Fatalf("listed attribute read returning EOPNOTSUPP must fail closed, got %v", err)
	}
}

func TestPreserveXattrsFailsClosedWhenInheritedACLRemoveReturnsENOTSUPUnlessAbsent(t *testing.T) {
	t.Run("fails closed when absence on replacement is not proven", func(t *testing.T) {
		swapXattrSeams(t)
		xattrListFunc = func(string) ([]string, error) { return nil, nil }
		xattrRemoveFunc = func(int, string) error { return unix.EOPNOTSUPP }
		xattrGetFunc = func(string, string) ([]byte, error) { return []byte("inherited-acl"), nil }

		file, err := os.CreateTemp(t.TempDir(), "xattr-*")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()

		err = preserveXattrs(file, "source")
		if !errors.Is(err, unix.EOPNOTSUPP) {
			t.Fatalf("unproven ACL removal returning EOPNOTSUPP must fail closed, got %v", err)
		}
	})

	t.Run("succeeds when replacement is proven not to carry ACL xattr", func(t *testing.T) {
		swapXattrSeams(t)
		xattrListFunc = func(string) ([]string, error) { return nil, nil }
		xattrRemoveFunc = func(int, string) error { return unix.EOPNOTSUPP }
		xattrGetFunc = func(string, string) ([]byte, error) { return nil, unix.ENODATA }

		file, err := os.CreateTemp(t.TempDir(), "xattr-*")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()

		if err := preserveXattrs(file, "source"); err != nil {
			t.Fatalf("expected success when replacement has no ACL xattr, got %v", err)
		}
	})
}
