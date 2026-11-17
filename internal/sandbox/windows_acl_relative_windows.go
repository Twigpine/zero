//go:build windows

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Handle-relative directory operations.
//
// WHY THESE EXIST. Every pathname-based call re-resolves the whole path inside
// the kernel at the moment it runs. So verifying a component and then creating
// through it are two separate resolutions of the same string, and a workspace
// owner can swap an ancestor for a junction in the gap between them: setup
// verifies, the attacker swaps, setup creates, and the object lands outside the
// approved tree. Checking again afterwards is too late, because the thing has
// already been created somewhere it should not be.
//
// A HANDLE pins the object rather than the name. Once a directory is open, that
// handle keeps referring to the same directory however the path is later
// rearranged, so creating a child relative to it cannot be redirected. os.Mkdir,
// os.OpenFile and os.RemoveAll are pathname-based by construction with no
// relative form on Windows, which is why this drops to NtCreateFile with
// OBJECT_ATTRIBUTES.RootDirectory.

// IO_STATUS_BLOCK.Information values for a create/open, named because "2 means
// it was created" is not something a reader should have to look up.
const (
	windowsFileOpened  uintptr = 1
	windowsFileCreated uintptr = 2
)

// windowsACLDirectoryShare is the share mode every open here uses. A sandbox
// tree is live, so refusing to share would fail on any directory something else
// happens to have open: a denial of service on ourselves rather than a security
// property.
const windowsACLDirectoryShare = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE

// windowsFileIdentity is the kernel's own answer to "is this the same object",
// independent of what it is currently called.
//
// It exists because rollback cannot hold the anchor handle open for the whole
// sandbox lifetime (see rollbackWindowsACLSnapshots), so it has to re-open the
// anchor by pathname, and a pathname can be made to name a different object.
// Crucially that substitution needs NO reparse point: rename the real directory
// aside and create an ordinary directory wearing its name, and every no-follow
// check still passes because nothing anywhere is a link. Comparing the volume
// and file index catches it, because those identify the object the kernel
// actually opened.
type windowsFileIdentity struct {
	Volume    uint32
	IndexHigh uint32
	IndexLow  uint32
}

func (identity windowsFileIdentity) empty() bool {
	return identity == windowsFileIdentity{}
}

// windowsIdentityOfHandle reads the identity of an already-open object.
func windowsIdentityOfHandle(handle windows.Handle) (windowsFileIdentity, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return windowsFileIdentity{}, fmt.Errorf("read windows file identity: %w", err)
	}
	return windowsFileIdentity{
		Volume:    info.VolumeSerialNumber,
		IndexHigh: info.FileIndexHigh,
		IndexLow:  info.FileIndexLow,
	}, nil
}

// openWindowsACLDirectoryNoFollow opens an existing directory by pathname,
// refusing to traverse or land on a reparse point.
//
// This is the ANCHOR for a handle-relative walk: the one pathname resolution
// that has to happen, with everything below it relative to the handle it
// returns. FILE_FLAG_OPEN_REPARSE_POINT stops the final component being
// followed, and verifyWindowsACLTargetNotRedirected then confirms no ancestor
// redirected either, because GetFinalPathNameByHandle answers for the whole
// resolved path.
func openWindowsACLDirectoryNoFollow(path string) (windows.Handle, error) {
	utf16Path, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, fmt.Errorf("encode windows ACL directory %s: %w", path, err)
	}
	handle, err := windows.CreateFile(
		utf16Path,
		windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windowsACLDirectoryShare,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		// Errno.Is maps the not-found codes to os.ErrNotExist, so a caller
		// walking up to find the deepest existing ancestor keeps working.
		return 0, fmt.Errorf("open windows ACL directory %s: %w", path, err)
	}
	if err := verifyWindowsACLHandleIsCleanDirectory(handle, path); err != nil {
		_ = windows.CloseHandle(handle)
		return 0, err
	}
	return handle, nil
}

// openWindowsACLDirectoryNoFollowWithIdentity is the anchor open plus the
// identity a later rollback needs in order to prove it re-opened the same
// object.
func openWindowsACLDirectoryNoFollowWithIdentity(path string) (windows.Handle, windowsFileIdentity, error) {
	handle, err := openWindowsACLDirectoryNoFollow(path)
	if err != nil {
		return 0, windowsFileIdentity{}, err
	}
	identity, err := windowsIdentityOfHandle(handle)
	if err != nil {
		_ = windows.CloseHandle(handle)
		return 0, windowsFileIdentity{}, fmt.Errorf("identify windows ACL directory %s: %w", path, err)
	}
	return handle, identity, nil
}

// reopenWindowsACLDirectoryAsIdentity re-opens an anchor by pathname and refuses
// it unless the kernel says it is the object that was opened before.
//
// Used only by rollback. See windowsFileIdentity for why the pathname alone is
// not enough, and rollbackWindowsACLSnapshots for why a handle cannot simply be
// held instead.
func reopenWindowsACLDirectoryAsIdentity(path string, want windowsFileIdentity) (windows.Handle, error) {
	handle, err := openWindowsACLDirectoryNoFollow(path)
	if err != nil {
		return 0, err
	}
	if want.empty() {
		return handle, nil
	}
	got, err := windowsIdentityOfHandle(handle)
	if err != nil {
		_ = windows.CloseHandle(handle)
		return 0, fmt.Errorf("identify windows ACL directory %s: %w", path, err)
	}
	if got != want {
		_ = windows.CloseHandle(handle)
		return 0, fmt.Errorf("refusing to unwind under %s: it is no longer the directory setup created into, so something replaced it since (possible path swap during elevated setup)", path)
	}
	return handle, nil
}

// createWindowsACLChildDirectory creates one directory directly beneath parent,
// or opens it when it already exists, and reports which happened.
//
// name must be a single component; see validateWindowsACLComponent for why that
// is enforced rather than assumed. The kernel resolves it relative to the parent
// HANDLE, so nothing above it is consulted and nothing above it can be swapped
// underneath us.
//
// created is true only when this call made the directory, which the rollback
// needs: removing one that already existed would delete a user's data over a
// failure that had nothing to do with it.
func createWindowsACLChildDirectory(parent windows.Handle, name string) (handle windows.Handle, created bool, err error) {
	if err := validateWindowsACLComponent(name); err != nil {
		return 0, false, err
	}
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, false, fmt.Errorf("encode windows ACL directory component %s: %w", name, err)
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		RootDirectory: parent,
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	attributes.Length = uint32(unsafe.Sizeof(attributes))

	var status windows.IO_STATUS_BLOCK
	if err := windows.NtCreateFile(
		&handle,
		windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE|windows.DELETE,
		&attributes,
		&status,
		nil,
		windows.FILE_ATTRIBUTE_DIRECTORY,
		windowsACLDirectoryShare,
		windows.FILE_OPEN_IF,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0,
		0,
	); err != nil {
		return 0, false, fmt.Errorf("create windows ACL directory component %s: %w", name, err)
	}
	// FILE_OPEN_IF means an EXISTING child is opened rather than created, and
	// FILE_OPEN_REPARSE_POINT means a junction is opened AS the junction. Without
	// this check that junction becomes the parent of the next level and every
	// create beneath it lands wherever it points, which is the mid-walk swap this
	// whole file exists to stop.
	if err := rejectWindowsACLReparseHandle(handle, name); err != nil {
		_ = windows.CloseHandle(handle)
		return 0, false, err
	}
	return handle, status.Information == windowsFileCreated, nil
}

// openWindowsACLChildDirectory opens an EXISTING directory beneath parent and
// never creates one.
//
// Rollback walks back down the chain it created, and it must not conjure a
// component that has since been removed: FILE_OPEN_IF would recreate it, and
// then the unwind would delete a directory setup never made. FILE_OPEN is the
// whole difference from createWindowsACLChildDirectory.
func openWindowsACLChildDirectory(parent windows.Handle, name string) (handle windows.Handle, err error) {
	if err := validateWindowsACLComponent(name); err != nil {
		return 0, err
	}
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, fmt.Errorf("encode windows ACL directory component %s: %w", name, err)
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		RootDirectory: parent,
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	attributes.Length = uint32(unsafe.Sizeof(attributes))

	// Ask for the least that still allows passing through and checking the
	// reparse attribute. This runs during rollback, on directories whose ACEs
	// have already been applied, so every extra right is another way for the
	// unwind to be refused by the very ACL it is unwinding. Notably absent:
	// SYNCHRONIZE, and with it FILE_SYNCHRONOUS_IO_NONALERT, since nothing is
	// read or written through this handle.
	var status windows.IO_STATUS_BLOCK
	if err := windows.NtCreateFile(
		&handle,
		windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES,
		&attributes,
		&status,
		nil,
		windows.FILE_ATTRIBUTE_DIRECTORY,
		windowsACLDirectoryShare,
		windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT,
		0,
		0,
	); err != nil {
		return 0, fmt.Errorf("open windows ACL directory component %s: %w", name, err)
	}
	if err := rejectWindowsACLReparseHandle(handle, name); err != nil {
		_ = windows.CloseHandle(handle)
		return 0, err
	}
	return handle, nil
}

// createWindowsACLChildFile creates an empty file directly beneath parent, or
// opens it when it already exists, and reports which happened.
//
// The counterpart to createWindowsACLChildDirectory for the one materialized
// target that must be a FILE: .git/config, where creating a directory instead
// would break `git init` outright rather than merely mis-ACL it.
//
// FILE_OPEN_IF rather than FILE_CREATE deliberately. The pathname version this
// replaces used O_CREATE|O_EXCL and then tolerated os.ErrExist, so a racing
// creator winning was fine. FILE_CREATE's collision status is
// STATUS_OBJECT_NAME_COLLISION, which errors.Is(err, os.ErrExist) does NOT
// match, so porting it literally would have turned that tolerated race into a
// hard failure. FILE_OPEN_IF keeps the old behaviour and reports the truth in
// created, which rollback needs so it never deletes a file it did not make.
func createWindowsACLChildFile(parent windows.Handle, name string) (created bool, identity windowsFileIdentity, err error) {
	if err := validateWindowsACLComponent(name); err != nil {
		return false, windowsFileIdentity{}, err
	}
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return false, windowsFileIdentity{}, fmt.Errorf("encode windows ACL file component %s: %w", name, err)
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
		windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		&attributes,
		&status,
		nil,
		windows.FILE_ATTRIBUTE_NORMAL,
		windowsACLDirectoryShare,
		windows.FILE_OPEN_IF,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0,
		0,
	); err != nil {
		return false, windowsFileIdentity{}, fmt.Errorf("create windows ACL file component %s: %w", name, err)
	}
	if err := rejectWindowsACLReparseHandle(handle, name); err != nil {
		_ = windows.CloseHandle(handle)
		return false, windowsFileIdentity{}, err
	}
	createdNow := status.Information == windowsFileCreated
	identity, err = windowsIdentityOfHandle(handle)
	if err != nil {
		_ = windows.CloseHandle(handle)
		return createdNow, windowsFileIdentity{}, fmt.Errorf("identify windows ACL file component %s: %w", name, err)
	}
	if err := windows.CloseHandle(handle); err != nil {
		return createdNow, identity, fmt.Errorf("close windows ACL file component %s: %w", name, err)
	}
	return createdNow, identity, nil
}

// deleteWindowsACLChildDirectory removes one directory directly beneath parent.
//
// The counterpart to the create above, and the reason rollback cannot use
// os.RemoveAll: that takes a pathname, so an ancestor swapped to a junction
// AFTER the object was created sends the recursive delete somewhere else and
// takes unrelated trees with it. Resolving relative to the parent handle makes
// that impossible.
//
// A missing child is not an error: rollback runs on failure paths where the
// object may never have been created.
func deleteWindowsACLChildDirectory(parent windows.Handle, name string) error {
	return deleteWindowsACLChild(parent, name, true, windowsFileIdentity{})
}

// deleteWindowsACLChildDirectoryAs and deleteWindowsACLChildFileAs remove the
// child only if the object at name is still want. The identity is read from
// the very handle the delete disposition is set on, so a replacement renamed
// in between a check and the unlink cannot be the thing removed.
func deleteWindowsACLChildDirectoryAs(parent windows.Handle, name string, want windowsFileIdentity) error {
	return deleteWindowsACLChild(parent, name, true, want)
}

func deleteWindowsACLChildFileAs(parent windows.Handle, name string, want windowsFileIdentity) error {
	return deleteWindowsACLChild(parent, name, false, want)
}

// deleteWindowsACLChildFile removes one file directly beneath parent, for the
// materialized .git/config carveout. Rollback picks between this and the
// directory form from the shape recorded at materialization time rather than by
// stat-ing the pathname, because a stat is another pathname resolution and this
// whole file exists to avoid those.
func deleteWindowsACLChildFile(parent windows.Handle, name string) error {
	return deleteWindowsACLChild(parent, name, false, windowsFileIdentity{})
}

// deleteWindowsACLChild opens one child relative to parent and deletes it by
// SETTING ITS DISPOSITION, not with FILE_DELETE_ON_CLOSE.
//
// That distinction is the whole point, and it was measured rather than assumed.
// FILE_DELETE_ON_CLOSE defers the removal to cleanup, where a non-empty
// directory makes it fail with nothing to report it to: NtCreateFile returns
// success, CloseHandle returns success, and the directory is still there. A
// rollback built on it would report success while leaving materialized state on
// disk, which is strictly worse than the os.RemoveAll it replaces, because
// os.RemoveAll at least removed it.
//
// NtSetInformationFile answers synchronously and to the caller, so a non-empty
// directory comes back as STATUS_DIRECTORY_NOT_EMPTY. Leaving residue is
// acceptable, since the alternative is a recursive delete through a pathname an
// attacker may control; lying about having removed it is not.
//
// FILE_DIRECTORY_FILE / FILE_NON_DIRECTORY_FILE also make the open refuse an
// object of the wrong shape rather than deleting it.
func deleteWindowsACLChild(parent windows.Handle, name string, directory bool, want windowsFileIdentity) error {
	if err := validateWindowsACLComponent(name); err != nil {
		return err
	}
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return fmt.Errorf("encode windows ACL component %s: %w", name, err)
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		RootDirectory: parent,
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	attributes.Length = uint32(unsafe.Sizeof(attributes))

	shapeOption := uint32(windows.FILE_NON_DIRECTORY_FILE)
	shapeAttribute := uint32(windows.FILE_ATTRIBUTE_NORMAL)
	if directory {
		shapeOption = windows.FILE_DIRECTORY_FILE
		shapeAttribute = windows.FILE_ATTRIBUTE_DIRECTORY
	}

	// DELETE alone. Asking for SYNCHRONIZE as well would make this fail on any
	// object already carrying a deny-read ACE, because FILE_GENERIC_READ and
	// FILE_GENERIC_EXECUTE both include SYNCHRONIZE, and rollback exists
	// precisely to undo objects that have just been ACL'd. Nothing is read or
	// written through this handle, so synchronous IO is not needed either.
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	if err := windows.NtCreateFile(
		&handle,
		windows.DELETE|windows.FILE_READ_ATTRIBUTES,
		&attributes,
		&status,
		nil,
		shapeAttribute,
		windowsACLDirectoryShare,
		windows.FILE_OPEN,
		shapeOption|windows.FILE_OPEN_REPARSE_POINT,
		0,
		0,
	); err != nil {
		if isWindowsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open windows ACL component %s for delete: %w", name, err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	if !want.empty() {
		got, err := windowsIdentityOfHandle(handle)
		if err != nil {
			return fmt.Errorf("identify windows ACL component %s before delete: %w", name, err)
		}
		if got != want {
			return fmt.Errorf("refusing to remove %s: it is no longer the object setup created", name)
		}
	}

	// One BOOLEAN: FILE_DISPOSITION_INFORMATION.DeleteFile = TRUE.
	disposition := byte(1)
	var setStatus windows.IO_STATUS_BLOCK
	if err := windows.NtSetInformationFile(
		handle,
		&setStatus,
		&disposition,
		uint32(unsafe.Sizeof(disposition)),
		windows.FileDispositionInformation,
	); err != nil {
		return fmt.Errorf("delete windows ACL component %s: %w", name, err)
	}
	return nil
}

// rejectWindowsACLReparseHandle refuses a handle that landed on a reparse point.
//
// Deliberately NOT verifyWindowsACLTargetNotRedirected: that one compares the
// handle's resolved path against an expected pathname, which is exactly the
// pathname dependency the handle-relative walk removes. A child opened relative
// to a pinned parent is in the right place by construction even when the
// pathname no longer leads there, so comparing paths would reject correct, safe
// creates whenever the tree was legitimately renamed. The attribute is the only
// thing worth checking here.
func rejectWindowsACLReparseHandle(handle windows.Handle, name string) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return fmt.Errorf("inspect windows ACL component %s: %w", name, err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("refusing to work through reparse-point component %s: possible path swap during elevated setup", name)
	}
	return nil
}

// verifyWindowsACLHandleIsCleanDirectory rejects a handle that landed on a
// reparse point, or on an object other than the path asked for.
func verifyWindowsACLHandleIsCleanDirectory(handle windows.Handle, path string) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return fmt.Errorf("inspect windows ACL directory %s: %w", path, err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("refusing to materialize under reparse-point path component %s: possible path swap during elevated setup", path)
	}
	return verifyWindowsACLTargetNotRedirected(handle, path)
}

// isWindowsNotExist reports a missing-object error from either side of the API.
// NtCreateFile returns NTSTATUS values, which do not map to os.ErrNotExist the
// way the Win32 error codes do.
func isWindowsNotExist(err error) bool {
	if err == nil {
		return false
	}
	if os.IsNotExist(err) {
		return true
	}
	var status windows.NTStatus
	if errors.As(err, &status) {
		return status == windows.STATUS_OBJECT_NAME_NOT_FOUND || status == windows.STATUS_OBJECT_PATH_NOT_FOUND
	}
	return false
}
