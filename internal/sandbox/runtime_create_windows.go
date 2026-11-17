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

// createRuntimeDirIdentified creates path and returns the identity of the
// directory it created, read from the handle the creation itself returned.
//
// THE CREATION MUST BE THE THING THAT ESTABLISHES IDENTITY. os.Mkdir followed by
// runtimeDirIdentity(path) is two resolutions of one name: Mkdir creates A, and
// the reopen can land on a B substituted in between, so the rollback ledger
// records B's identity for a directory this run never made. Compensation then
// correctly proves it holds B and deletes it, while A keeps this run's ACL and
// stamp under a name nothing is tracking. The runtime parent belongs to the
// ordinary user, so that substitution needs no privilege.
//
// Win32 CreateFile cannot create a directory at all, whatever disposition it is
// given, so an earlier attempt to do this with CREATE_NEW fell through to the
// Mkdir-plus-reopen path on EVERY real creation and the documented handle
// contract never once held. NtCreateFile with FILE_CREATE and
// FILE_DIRECTORY_FILE does create one, and returns the handle to it.
//
// If the atomic create cannot be completed, this returns an error rather than
// manufacturing an ownership record from a reopen: an unidentified create must
// stop setup before privileged state is applied, not enter the ledger as though
// it were proven.
func createRuntimeDirIdentified(path string) (string, bool, error) {
	clean := filepath.Clean(path)
	parentPath, leaf := filepath.Split(clean)
	leaf = filepath.Clean(leaf)
	parentPath = filepath.Clean(parentPath)
	if leaf == "" || leaf == "." || parentPath == clean {
		return "", false, fmt.Errorf("sandbox runtime path %s has no component to create", path)
	}

	// The parent is either a directory that already existed when the missing
	// components were computed, or one this same loop created a moment ago.
	parent, err := openWindowsDirectoryByName(parentPath)
	if err != nil {
		return "", false, fmt.Errorf("open sandbox runtime parent %s: %w", parentPath, err)
	}
	defer windows.CloseHandle(parent)

	handle, err := createWindowsChildDirectory(parent, leaf)
	if err != nil {
		// A collision is the same signal os.Mkdir gives for a component another
		// process won the race to create, and the caller already treats that as
		// "not ours" (windows_setup.go). Returned as a *PathError so BOTH
		// os.IsExist, which the caller uses and which does not unwrap %w, and
		// errors.Is recognize it; wrapping with %w alone would have turned a
		// benign race into a hard setup failure.
		if errors.Is(err, windows.STATUS_OBJECT_NAME_COLLISION) {
			return "", false, &os.PathError{Op: "mkdir", Path: clean, Err: os.ErrExist}
		}
		return "", false, fmt.Errorf("create sandbox runtime directory %s: %w", clean, err)
	}
	defer windows.CloseHandle(handle)

	identity, idErr := handleRuntimeIdentity(handle)
	if idErr != nil {
		return "", false, fmt.Errorf("identify the sandbox runtime directory created at %s: %w", clean, idErr)
	}
	return identity, true, nil
}

// removeWindowsObjectByHandle deletes the object this handle is open on.
//
// THROUGH THE HANDLE, NOT THE NAME. Every caller is on a failure path partway
// through a creation, and the one object it may remove is the one it just made
// and is still holding. Re-resolving the name to delete it is the substitution
// hole this whole descent exists to close, and here it would be a pathname
// authoritative delete of somebody else's directory.
//
// The handle needs DELETE access, which the creating opens above ask for.
func removeWindowsObjectByHandle(handle windows.Handle) error {
	var iosb windows.IO_STATUS_BLOCK
	deleteIt := byte(1)
	return windows.NtSetInformationFile(handle, &iosb, &deleteIt, 1, windows.FileDispositionInformation)
}

// createWindowsChildDirectory creates exactly one directory component beneath
// parent and returns the handle to the object it created.
//
// Relative to a handle, so no ancestor is re-resolved and there is no interval
// for a swap to land in. FILE_CREATE fails rather than opening anything that is
// already there, so the returned handle cannot describe a pre-existing object,
// and FILE_OPEN_REPARSE_POINT keeps the failure honest if one is.
func createWindowsChildDirectory(parent windows.Handle, name string) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, fmt.Errorf("encode sandbox runtime component %s: %w", name, err)
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		RootDirectory: parent,
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	attributes.Length = uint32(unsafe.Sizeof(attributes))

	var handle windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	// DELETE because the caller has to be able to undo this creation through the
	// handle it just got, without going back through the name. See
	// removeWindowsObjectByHandle.
	err = windows.NtCreateFile(
		&handle,
		windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.FILE_TRAVERSE|windows.SYNCHRONIZE,
		&attributes,
		&iosb,
		nil,
		windows.FILE_ATTRIBUTE_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_CREATE,
		windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT,
		0,
		0,
	)
	if err != nil {
		return 0, err
	}
	return handle, nil
}
