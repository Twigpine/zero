//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// seedRuntimeLeaseTree creates the owned tail and the lease the way a first run
// does, then removes just the lease file so a test can put something else at that
// name. The tree has to exist, or cleanup's descent has nothing to open and the
// test would pass for the wrong reason.
func seedRuntimeLeaseTree(t *testing.T, root string) string {
	t.Helper()
	lease, _, err := prepareSandboxRuntimeLeaseRecording(root)
	if err != nil {
		t.Skipf("cannot seed a runtime lease here: %v", err)
	}
	lease.release()
	leasePath := sandboxRuntimeLeasePath(root)
	if err := os.Remove(leasePath); err != nil {
		t.Fatalf("SETUP INVALID: cannot clear the seeded lease: %v", err)
	}
	return leasePath
}

// plantFileLink puts a FILE symbolic link at leasePath pointing at an ordinary
// file, and returns the target.
//
// A file link, not a junction: a junction is a directory and FILE_NON_DIRECTORY_FILE
// already refuses it, so a junction here would pass against the defect and prove
// nothing. Creating one needs SeCreateSymbolicLinkPrivilege, which an ordinary
// unelevated account does not hold, so this skips there and runs on CI.
func plantFileLink(t *testing.T, leasePath string) string {
	t.Helper()
	target := filepath.Join(t.TempDir(), "target.lease")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, leasePath); err != nil {
		t.Skipf("cannot create a file symbolic link here, which needs SeCreateSymbolicLinkPrivilege: %v", err)
	}
	// SETUP: it really is a reparse point, or nothing below is about links.
	info, err := os.Lstat(leasePath)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("SETUP INVALID: %s is not a link (%v, %v)", leasePath, info, err)
	}
	return target
}

// assertTargetUnlocked proves nobody holds a lock on the link's target, which is
// the object the defect made the two sides fight over.
func assertTargetUnlocked(t *testing.T, target string) {
	t.Helper()
	file, err := os.OpenFile(target, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open the link target: %v", err)
	}
	defer func() { _ = file.Close() }()
	var overlapped windows.Overlapped
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
	if err := windows.LockFileEx(windows.Handle(file.Fd()), flags, 0, 1, 0, &overlapped); err != nil {
		t.Fatalf("the link target is locked, so the refusal happened after the lease had already been taken on it: %v", err)
	}
	_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped)
}

// FILE_OPEN_REPARSE_POINT SAYS DO NOT FOLLOW, NOT REFUSE.
//
// The rooted opener passed that flag and FILE_NON_DIRECTORY_FILE and treated the
// pair as a guarantee. It is not: the flag returns a handle to the LINK, and
// FILE_NON_DIRECTORY_FILE excludes directories rather than non-directory reparse
// objects. A file symbolic link at <digest>.lease was opened, wrapped and locked
// as though it were the lease.
//
// That matters because cleanup opened the same name by pathname with no
// no-follow flag at all, so it locked the link's TARGET. Two holders, two
// objects, both calls succeeding, and cleanup free to RemoveAll a runtime root a
// live command was using.
func TestSharedLeaseRefusesAFileLinkAtTheLeaseName(t *testing.T) {
	cacheRoot := t.TempDir()
	root := leaseRootUnder(t, cacheRoot)
	leasePath := seedRuntimeLeaseTree(t, root)
	target := plantFileLink(t, leasePath)

	lease, _, err := prepareSandboxRuntimeLeaseRecording(root)
	if lease != nil {
		lease.release()
	}
	if err == nil {
		t.Fatal("shared acquisition accepted a file link at the lease name, so it is holding a lock on an object cleanup does not see")
	}
	if !strings.Contains(err.Error(), "reparse point") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
	assertTargetUnlocked(t, target)
}

// And cleanup refuses the same object, so it cannot decide the root is free by
// locking something else.
func TestCleanupLeaseRefusesAFileLinkAtTheLeaseName(t *testing.T) {
	cacheRoot := t.TempDir()
	root := leaseRootUnder(t, cacheRoot)
	leasePath := seedRuntimeLeaseTree(t, root)
	target := plantFileLink(t, leasePath)

	lease, inUse, err := tryAcquireSandboxRuntimeCleanupLease(root)
	if lease != nil {
		lease.release()
	}
	if err == nil {
		t.Fatalf("cleanup accepted a file link at the lease name (inUse=%t); it would treat a lock on %s as proof the runtime root is free to delete", inUse, target)
	}
	if !strings.Contains(err.Error(), "reparse point") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
	assertTargetUnlocked(t, target)
}

// CONTROL: THE TWO SIDES REALLY DO COORDINATE ON AN ORDINARY LEASE.
//
// Without this, refusing everything would satisfy the two tests above. This one
// runs everywhere, including on an unelevated box with no symbolic-link
// privilege, so the mutual-exclusion contract stays pinned even where the link
// cases skip.
func TestASharedLeaseMakesCleanupReportInUse(t *testing.T) {
	cacheRoot := t.TempDir()
	root := leaseRootUnder(t, cacheRoot)

	held, _, err := prepareSandboxRuntimeLeaseRecording(root)
	if err != nil {
		t.Skipf("cannot acquire a runtime lease here: %v", err)
	}

	lease, inUse, err := tryAcquireSandboxRuntimeCleanupLease(root)
	if lease != nil {
		lease.release()
	}
	if err != nil {
		t.Fatalf("cleanup failed against a legitimately held lease: %v", err)
	}
	if !inUse {
		t.Fatal("cleanup reported the runtime root free while a shared lease was held; it would delete a tree a live command is using")
	}

	held.release()

	lease, inUse, err = tryAcquireSandboxRuntimeCleanupLease(root)
	if err != nil {
		t.Fatalf("cleanup failed after the shared lease was released: %v", err)
	}
	if inUse {
		t.Fatal("cleanup still reported the runtime root in use after the only holder released it, so reclamation never happens")
	}
	if lease == nil {
		t.Fatal("cleanup reported the root free but handed back no lease")
	}
	lease.release()
}

// CONTROL: a legitimate lease is still SHARED between processes.
//
// The classification must not turn the shared lease into an exclusive one, or
// two concurrent commands on one workspace would stop working.
func TestAnOrdinaryLeaseIsStillSharedByTwoHolders(t *testing.T) {
	cacheRoot := t.TempDir()
	root := leaseRootUnder(t, cacheRoot)

	first, _, err := prepareSandboxRuntimeLeaseRecording(root)
	if err != nil {
		t.Skipf("cannot acquire a runtime lease here: %v", err)
	}
	defer first.release()

	second, _, err := prepareSandboxRuntimeLeaseRecording(root)
	if err != nil {
		t.Fatalf("a second holder was refused an existing ordinary lease: %v", err)
	}
	second.release()
}

// reparseGUIDDataBuffer is the non-Microsoft form of REPARSE_GUID_DATA_BUFFER.
//
// Setting a tag with the Microsoft bit clear needs only write access to the file,
// unlike a symbolic link, so this runs on an ordinary unelevated account and the
// classification stays pinned where the link cases skip.
type reparseGUIDDataBuffer struct {
	ReparseTag        uint32
	ReparseDataLength uint16
	Reserved          uint16
	ReparseGUID       windows.GUID
	Data              [16]byte
}

// plantGenericReparsePoint turns an ordinary file at path into a reparse object.
func plantGenericReparsePoint(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name,
		windows.GENERIC_WRITE|windows.FILE_WRITE_ATTRIBUTES, 0, nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Skipf("cannot open %s to set a reparse tag: %v", path, err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()

	buffer := reparseGUIDDataBuffer{
		// Bit 31 clear, so this is a third-party tag and needs no privilege.
		ReparseTag:        0x00000042,
		ReparseDataLength: 16,
		ReparseGUID:       windows.GUID{Data1: 0x5ee0e5f1, Data2: 0x1a11, Data3: 0x4d3b, Data4: [8]byte{0x9a, 0x77, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06}},
	}
	size := uint32(unsafe.Sizeof(buffer))
	var returned uint32
	if err := windows.DeviceIoControl(handle, windowsFSCTLSetReparsePoint,
		(*byte)(unsafe.Pointer(&buffer)), size, nil, 0, &returned, nil); err != nil {
		t.Skipf("cannot set a generic reparse tag here: %v", err)
	}
}

const windowsFSCTLSetReparsePoint = 0x000900A4

// ANY REPARSE OBJECT AT THE LEASE NAME IS REFUSED, NOT ONLY A SYMBOLIC LINK.
//
// The requirement is that the name denotes an ordinary file, because that is what
// makes the shared holder and cleanup lock the same thing. Keying on the link
// shape instead would leave every other reparse tag accepted, and it would leave
// this untested on any machine without the symbolic-link privilege.
//
// WHAT THIS CASE CANNOT SHOW, so nobody reads a local pass as more than it is:
// an unknown third-party tag is unresolvable, so the OLD pathname cleanup fails
// on it too, with ERROR_CANT_ACCESS_FILE rather than by classifying anything. It
// therefore pins that both sites refuse, and the reason check below is what
// separates a refusal from an accident. Only the symbolic-link cases above show
// the half that matters most, a pathname open SUCCEEDING on the target, and those
// need the privilege. Read their result in CI, not here.
func TestLeaseRefusesAnyReparseObjectAtTheLeaseName(t *testing.T) {
	for name, acquire := range map[string]func(string) error{
		"shared": func(root string) error {
			lease, _, err := prepareSandboxRuntimeLeaseRecording(root)
			if lease != nil {
				lease.release()
			}
			return err
		},
		"cleanup": func(root string) error {
			lease, _, err := tryAcquireSandboxRuntimeCleanupLease(root)
			if lease != nil {
				lease.release()
			}
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			cacheRoot := t.TempDir()
			root := leaseRootUnder(t, cacheRoot)
			leasePath := seedRuntimeLeaseTree(t, root)
			plantGenericReparsePoint(t, leasePath)

			// SETUP: the attribute really is set, or this asserts nothing.
			info, err := os.Lstat(leasePath)
			if err != nil || info.Mode()&os.ModeIrregular == 0 && info.Mode()&os.ModeSymlink == 0 {
				attrs, statErr := windowsFileAttributes(leasePath)
				if statErr != nil || attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
					t.Fatalf("SETUP INVALID: %s is not a reparse object (attrs=%#x err=%v)", leasePath, attrs, statErr)
				}
			}

			err = acquire(root)
			if err == nil {
				t.Fatalf("%s acquisition accepted a reparse object at the lease name", name)
			}
			if !strings.Contains(err.Error(), "reparse point") {
				t.Errorf("refused for the wrong reason: %v", err)
			}
		})
	}
}

func windowsFileAttributes(path string) (uint32, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return windows.GetFileAttributes(name)
}
