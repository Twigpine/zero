//go:build windows

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// acquireRuntimeLeaseRooted creates and locks the lease file for root without
// ever resolving an owned component by name.
//
// THE LEASE IS ACQUIRED BEFORE ANYTHING ELSE, SO IT CANNOT BE THE WEAK LINK.
//
// Provisioning already descends from the fixed cache or TEMP base through
// retained no-follow handles, because a predictable owned component is exactly
// what an ordinary same-account process can replace with a junction. Lease
// acquisition ran first and did neither: it checked the components for aliases,
// then called os.MkdirAll on the parent by pathname and opened
// "<root>.lease" by pathname, both of which follow. A junction dropped on
// "zero", "runtime" or "v1" between the check and either call put elevated
// setup's first writes inside somebody else's tree, and restoring the component
// afterwards left the later handle-relative provisioning working on the
// legitimate tree so no post-check ever saw it.
//
// The lease file is a SIBLING of the runtime root rather than one of its owned
// components, so refuseAliasedRuntimeComponents never inspected it at all. Here
// it is created relative to the retained handle for the directory that contains
// it, which is the deepest owned component, so its name is resolved exactly once
// and relative to a verified object.
//
// Only the components above the leaf are created. The leaf itself belongs to
// provisioning, which records it for rollback; creating it here would mean two
// owners for one directory.
func acquireRuntimeLeaseRooted(root string) (*sandboxRuntimeLease, []windowsCreatedRuntimeDir, error) {
	base, components, owned := windowsSandboxRuntimeOwnedTail(root)
	if !owned {
		// Fail rather than fall back to the pathname walk: the walk is the defect.
		return nil, nil, fmt.Errorf("acquire sandbox runtime lease for %s: %w", root, errRuntimeTailNotOwned)
	}
	if len(components) == 0 {
		return nil, nil, fmt.Errorf("acquire sandbox runtime lease for %s: %w", root, errRuntimeTailNotOwned)
	}
	// The base is the operator's, and may legitimately be a redirected cache or
	// TEMP location, so it is created and opened by name exactly as provisioning
	// does. Everything below it is Zero's and is addressed by handle.
	if runtimeLeasePreCreateBarrier != nil {
		runtimeLeasePreCreateBarrier()
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create sandbox runtime base: %w", err)
	}

	// The lease sits beside the leaf, so the deepest directory needed here is the
	// leaf's parent.
	parents := components[:len(components)-1]
	tail := make([]string, 0, len(parents))
	current := base
	for _, component := range parents {
		current = filepath.Join(current, component)
		tail = append(tail, current)
	}

	// THE WHOLE ACQUISITION RETRIES, NOT JUST THE LOCK. A contender that waited
	// on the lease while cleanup held it exclusively can wake on an object cleanup
	// has since marked for deletion, and by then cleanup has compensated the tail
	// this run created as well. Re-taking the lock alone would lock a fresh file
	// beside a leaf that is gone, so a stale lock sends the caller back through
	// tail creation. The ledger accumulates across attempts: a directory this run
	// made on an earlier pass and cleanup did not remove is still this run's to
	// compensate.
	var created []windowsCreatedRuntimeDir
	for attempt := 1; ; attempt++ {
		made, parent, err := createRuntimeTailRetainingHandle(base, tail)
		created = appendCreatedRuntimeDirs(created, made)
		if err != nil {
			if parent != 0 {
				_ = windows.CloseHandle(parent)
			}
			return nil, created, err
		}
		handle, madeLease, err := acquireSharedRuntimeLeaseAt(parent, filepath.Base(sandboxRuntimeLeasePath(root)))
		_ = windows.CloseHandle(parent)
		if err == nil {
			return &sandboxRuntimeLease{handle: handle, root: root, createdFile: madeLease}, created, nil
		}
		if errors.Is(err, errRuntimeLeaseReplaced) && attempt < runtimeLeaseAcquireAttempts {
			continue
		}
		return nil, created, fmt.Errorf("acquire sandbox runtime lease: %w", err)
	}
}

// acquireSharedRuntimeLeaseAt is acquireSharedRuntimeLease with the file named
// relative to a directory handle instead of by full path.
func acquireSharedRuntimeLeaseAt(parent windows.Handle, name string) (runtimeLeaseHandle, bool, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return runtimeLeaseHandle{}, false, fmt.Errorf("encode sandbox runtime lease name %s: %w", name, err)
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		RootDirectory: parent,
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	attributes.Length = uint32(unsafe.Sizeof(attributes))

	var handle windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	// FILE_OPEN_IF because the lease is shared: whoever gets there first creates
	// it. FILE_NON_DIRECTORY_FILE so a directory under that name is refused.
	//
	// FILE_OPEN_REPARSE_POINT IS NOT A REFUSAL. It says do not follow, so the call
	// returns a handle to the LINK, and FILE_NON_DIRECTORY_FILE excludes
	// directories rather than non-directory reparse objects. A file symbolic link
	// planted at <digest>.lease was therefore opened and locked as if it were the
	// lease. No-follow and classification are two requirements; the flag is only
	// the first, and the handle is asked for the second below.
	err = windows.NtCreateFile(
		&handle,
		windows.DELETE|windows.GENERIC_READ|windows.GENERIC_WRITE|windows.SYNCHRONIZE,
		&attributes,
		&iosb,
		nil,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN_IF,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT,
		0,
		0,
	)
	if err != nil {
		return runtimeLeaseHandle{}, false, err
	}
	// FILE_CREATED here, rather than a Stat before the call, because only the
	// create itself can distinguish the file it made from one that arrived a
	// moment earlier.
	created := iosb.Information == windowsFileCreatedDisposition
	// And every failure below has already created the file when that is true,
	// while returning no lease object to carry the fact, so each undoes its own
	// creation instead of leaving an unrecorded artifact in a directory
	// compensation then cannot empty.
	if runtimeCreationFailure != nil {
		if injected := runtimeCreationFailure(name); injected != nil {
			injected = undoCreatedRuntimeLease(created, handle, injected)
			_ = windows.CloseHandle(handle)
			return runtimeLeaseHandle{}, false, injected
		}
	}
	if err := refuseReparseRuntimeLeaseHandle(handle, name); err != nil {
		err = undoCreatedRuntimeLease(created, handle, err)
		_ = windows.CloseHandle(handle)
		return runtimeLeaseHandle{}, false, err
	}
	file := os.NewFile(uintptr(handle), name)
	if file == nil {
		err := undoCreatedRuntimeLease(created, handle, fmt.Errorf("wrap the sandbox runtime lease handle for %s", name))
		_ = windows.CloseHandle(handle)
		return runtimeLeaseHandle{}, false, err
	}
	lease := runtimeLeaseHandle{file: file}
	if err := windows.LockFileEx(windows.Handle(file.Fd()), 0, 0, 1, 0, &lease.overlapped); err != nil {
		err = undoCreatedRuntimeLease(created, windows.Handle(file.Fd()), err)
		_ = file.Close()
		return runtimeLeaseHandle{}, false, err
	}
	// THE LOCK IS ON AN OBJECT, THE COORDINATION IS ON A NAME. LockFileEx waits
	// while cleanup holds the exclusive lock, and cleanup marks the lease for
	// deletion before it lets go; this handle was opened with FILE_SHARE_DELETE
	// precisely so cleanup could. A waiter then locks a delete-pending object and
	// returns a lease that coordinates with nobody: once this handle closes the
	// name is free, the next cleanup creates a fresh file there, locks it
	// unopposed, and reports the root unused while this holder is still running
	// in it. So the lock is only a lease once the locked object is still what the
	// name resolves to. Reported by @gnanam1990.
	current, err := runtimeLeaseIsCurrentAt(parent, name, windows.Handle(file.Fd()))
	if err != nil {
		_ = file.Close()
		return runtimeLeaseHandle{}, false, err
	}
	if !current {
		// Not undone, whatever created says: the object is already going, and the
		// name now belongs to whoever comes next. Closing this handle is what lets
		// the name go so the retry can take a current one.
		_ = file.Close()
		return runtimeLeaseHandle{}, false, errRuntimeLeaseReplaced
	}
	return lease, created, nil
}

// runtimeLeaseIsCurrentAt reports whether the locked lease is still the object
// the lease name resolves to under parent.
//
// Both answers come from handles: the locked file's own identity, and a fresh
// no-follow open of the name relative to the retained parent, asked for nothing
// but attributes. A name that is gone or delete-pending, or that resolves to a
// different volume and file index, means the coordination moved on while this
// call was waiting.
func runtimeLeaseIsCurrentAt(parent windows.Handle, name string, locked windows.Handle) (bool, error) {
	var lockedInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(locked, &lockedInfo); err != nil {
		return false, fmt.Errorf("inspect the locked sandbox runtime lease %s: %w", name, err)
	}
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return false, fmt.Errorf("encode sandbox runtime lease name %s: %w", name, err)
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		RootDirectory: parent,
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	attributes.Length = uint32(unsafe.Sizeof(attributes))
	var probe windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(
		&probe,
		windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		&attributes,
		&iosb,
		nil,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT,
		0,
		0,
	)
	if err != nil {
		if errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND) || errors.Is(err, windows.STATUS_DELETE_PENDING) {
			return false, nil
		}
		return false, fmt.Errorf("inspect the sandbox runtime lease entry %s: %w", name, err)
	}
	defer func() { _ = windows.CloseHandle(probe) }()
	var entryInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(probe, &entryInfo); err != nil {
		return false, fmt.Errorf("inspect the sandbox runtime lease entry %s: %w", name, err)
	}
	return lockedInfo.VolumeSerialNumber == entryInfo.VolumeSerialNumber &&
		lockedInfo.FileIndexHigh == entryInfo.FileIndexHigh &&
		lockedInfo.FileIndexLow == entryInfo.FileIndexLow, nil
}

// undoCreatedRuntimeLease removes a lease file this call created, when a later
// step means no lease object will be returned to carry that fact.
//
// Through the handle, and only when this call is the one that created the file:
// a lease that was already there belongs to whoever is holding it.
func undoCreatedRuntimeLease(created bool, handle windows.Handle, cause error) error {
	if !created {
		return cause
	}
	if undoErr := removeWindowsObjectByHandle(handle); undoErr != nil {
		return fmt.Errorf("%w; and the lease file this run created could not be removed: %w", cause, undoErr)
	}
	return cause
}

// refuseReparseRuntimeLeaseHandle proves the opened lease is an ordinary file.
//
// Asked of the HANDLE, not the name, so there is no second resolution for a
// substitution to land in. This is the check the directory descent already makes
// in openWindowsChildNoFollow; the lease carried the no-follow flag and not the
// classification that has to go with it.
func refuseReparseRuntimeLeaseHandle(handle windows.Handle, name string) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return fmt.Errorf("inspect the sandbox runtime lease %s: %w", name, err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("refusing to use the sandbox runtime lease at %s: a reparse point here means the shared and exclusive holders would lock different objects", name)
	}
	return nil
}

// openRuntimeLeaseParentRooted opens, and never creates, the directory holding
// the lease for root.
//
// Cleanup's side of the contract. It descends the same owned components from the
// same base with the same no-follow opens acquisition uses, so both sides resolve
// the lease name relative to a verified directory rather than by pathname.
//
// It creates nothing. A runtime tree that is not there has no lease to take, and
// cleanup rebuilding the tree in order to lock it would be inventing the thing it
// is about to remove.
func openRuntimeLeaseParentRooted(root string) (windows.Handle, error) {
	base, components, owned := windowsSandboxRuntimeOwnedTail(root)
	if !owned || len(components) == 0 {
		return 0, fmt.Errorf("open the sandbox runtime lease parent for %s: %w", root, errRuntimeTailNotOwned)
	}
	parent, err := openWindowsDirectoryByName(base)
	if err != nil {
		return 0, fmt.Errorf("open sandbox runtime base %s: %w", base, err)
	}
	// The lease is a SIBLING of the leaf, so the deepest directory needed here is
	// the leaf's parent, exactly as in acquisition.
	for _, name := range components[:len(components)-1] {
		child, openErr := openWindowsChildNoFollow(parent, name,
			windows.FILE_READ_ATTRIBUTES|windows.FILE_TRAVERSE, windows.FILE_DIRECTORY_FILE)
		if openErr != nil {
			_ = windows.CloseHandle(parent)
			return 0, openErr
		}
		_ = windows.CloseHandle(parent)
		parent = child
	}
	return parent, nil
}

// tryAcquireExclusiveRuntimeLeaseRooted is cleanup's acquisition, resolved the
// way acquisition resolves it.
func tryAcquireExclusiveRuntimeLeaseRooted(root string) (runtimeLeaseHandle, bool, error) {
	parent, err := openRuntimeLeaseParentRooted(root)
	if err != nil {
		return runtimeLeaseHandle{}, false, err
	}
	defer func() { _ = windows.CloseHandle(parent) }()

	name := filepath.Base(sandboxRuntimeLeasePath(root))
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return runtimeLeaseHandle{}, false, fmt.Errorf("encode sandbox runtime lease name %s: %w", name, err)
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		RootDirectory: parent,
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	attributes.Length = uint32(unsafe.Sizeof(attributes))

	for attempt := 1; ; attempt++ {
		var handle windows.Handle
		var iosb windows.IO_STATUS_BLOCK
		// FILE_OPEN_IF matches what cleanup did by pathname with O_CREATE: a
		// runtime root whose lease file is gone is held by nobody, and creating the
		// empty lease is how that is expressed.
		err = windows.NtCreateFile(
			&handle,
			windows.DELETE|windows.GENERIC_READ|windows.GENERIC_WRITE|windows.SYNCHRONIZE,
			&attributes,
			&iosb,
			nil,
			windows.FILE_ATTRIBUTE_NORMAL,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			windows.FILE_OPEN_IF,
			windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT,
			0,
			0,
		)
		if err != nil {
			return runtimeLeaseHandle{}, false, err
		}
		created := iosb.Information == windowsFileCreatedDisposition
		if err := refuseReparseRuntimeLeaseHandle(handle, name); err != nil {
			err = undoCreatedRuntimeLease(created, handle, err)
			_ = windows.CloseHandle(handle)
			return runtimeLeaseHandle{}, false, err
		}
		file := os.NewFile(uintptr(handle), name)
		if file == nil {
			err := undoCreatedRuntimeLease(created, handle, fmt.Errorf("wrap the sandbox runtime lease handle for %s", name))
			_ = windows.CloseHandle(handle)
			return runtimeLeaseHandle{}, false, err
		}
		lease := runtimeLeaseHandle{file: file}
		flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
		if err := windows.LockFileEx(windows.Handle(file.Fd()), flags, 0, 1, 0, &lease.overlapped); err != nil {
			if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
				// Somebody holds it, so it is not this call's to remove, whatever the
				// open reported. A file this call created cannot reach here anyway.
				_ = file.Close()
				return runtimeLeaseHandle{}, true, nil
			}
			err = undoCreatedRuntimeLease(created, windows.Handle(file.Fd()), err)
			_ = file.Close()
			return runtimeLeaseHandle{}, false, err
		}
		// The exclusive side has the same window in miniature: between its open
		// and its lock another cleanup can mark the object for deletion, and an
		// exclusive lock on that object would then remove a tree a holder of the
		// replacement is entering. Same check, same answer: only the current
		// object counts, and a stale one is reopened rather than reasoned about.
		current, err := runtimeLeaseIsCurrentAt(parent, name, windows.Handle(file.Fd()))
		if err != nil {
			_ = file.Close()
			return runtimeLeaseHandle{}, false, err
		}
		if !current {
			_ = file.Close()
			if attempt < runtimeLeaseAcquireAttempts {
				continue
			}
			return runtimeLeaseHandle{}, false, errRuntimeLeaseReplaced
		}
		return lease, false, nil
	}
}

// windowsFileCreatedDisposition is the IO_STATUS_BLOCK Information value that
// says NtCreateFile made the file rather than opening one that was there.
// x/sys/windows does not export it.
const windowsFileCreatedDisposition = 2
