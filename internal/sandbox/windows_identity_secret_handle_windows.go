//go:build windows

package sandbox

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// createWindowsSecretFileNoFollow creates (or replaces) the secret file as a
// CHILD of a pinned parent handle and returns a handle to it.
//
// The pathname version this replaces resolved the path four separate times in an
// elevated process: MkdirAll, an O_TRUNC open, SetNamedSecurityInfo by name, then
// WriteFile by name. The sandbox home belongs to the invoking user, who is the
// party this sandbox exists to contain, so each of those resolutions was a place
// to swap a component. Worst case an attacker-chosen file gets truncated and then
// has its DACL rewritten by an Administrator; the milder junction-only case still
// plants the deterministic secret somewhere the caller then owns.
//
// One handle, one resolution. The parent chain is built no-follow by the caller
// and its deepest handle is passed in here, the leaf is created relative to that
// handle so its name can never be resolved again, and both the DACL and the
// bytes are applied to the handle rather than to a path.
//
// FILE_OVERWRITE_IF, not FILE_CREATE: an existing secret must be replaced, since
// a stale password makes LogonUser fail in a way that reads as a sandbox bug.
// FILE_OPEN_REPARSE_POINT means a leaf that IS a reparse point comes back as
// itself rather than being followed, and the attribute check below then refuses
// it instead of writing through it.
func createWindowsSecretFileAt(parent windows.Handle, name string) (windows.Handle, error) {
	if err := validateWindowsACLComponent(name); err != nil {
		return 0, fmt.Errorf("secret file name: %w", err)
	}

	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, fmt.Errorf("encode secret file name %s: %w", name, err)
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		RootDirectory: parent,
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	attributes.Length = uint32(unsafe.Sizeof(attributes))

	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	if err := windows.NtCreateFile(
		&handle,
		// WRITE_DAC to lock it down and FILE_WRITE_DATA to fill it, both through
		// this one handle. READ_CONTROL because SetSecurityInfo reads the existing
		// descriptor before replacing it, and FILE_READ_ATTRIBUTES because the
		// reparse-point check below asks the handle what it landed on.
		windows.FILE_WRITE_DATA|windows.FILE_READ_ATTRIBUTES|windows.WRITE_DAC|windows.READ_CONTROL|windows.SYNCHRONIZE,
		&attributes,
		&status,
		nil,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ,
		windows.FILE_OVERWRITE_IF,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0,
		0,
	); err != nil {
		return 0, fmt.Errorf("create secret file %s: %w", name, err)
	}
	if err := rejectWindowsACLReparseHandle(handle, name); err != nil {
		_ = windows.CloseHandle(handle)
		return 0, err
	}
	return handle, nil
}

// lockWindowsSecretHandleToOwner applies the owner-and-SYSTEM-only DACL to an
// open handle instead of to a pathname, so the object being locked is provably
// the object that was just created rather than whatever the name resolves to now.
func lockWindowsSecretHandleToOwner(handle windows.Handle, owner *windows.SID) error {
	acl, err := windowsSecretOwnerACL(owner)
	if err != nil {
		return err
	}
	if err := windows.SetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	); err != nil {
		return fmt.Errorf("lock secret to owner: %w", err)
	}
	return nil
}

// writeWindowsSecretHandle writes the whole payload through the handle.
func writeWindowsSecretHandle(handle windows.Handle, payload []byte) error {
	for written := 0; written < len(payload); {
		var n uint32
		if err := windows.WriteFile(handle, payload[written:], &n, nil); err != nil {
			return fmt.Errorf("write secret: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("write secret: wrote no bytes with %d remaining", len(payload)-written)
		}
		written += int(n)
	}
	return nil
}
