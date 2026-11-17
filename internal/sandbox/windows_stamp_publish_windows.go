//go:build windows

package sandbox

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ONE STEP FROM THE OLD STAMP TO THE NEW ONE.
//
// Commands and zero doctor read the stamp without taking the setup lock. The
// writer used to open the existing stamp with FILE_OVERWRITE_IF, which truncates
// at open time, and only then protect and write it, so a command validating in
// that interval read an empty or partial stamp and refused to start on a setup
// that was still valid, and a process that stopped there left the marker
// pointing at a stamp that could never validate again. Rollback had the same
// interval the other way round: it removed the stamp before recreating the
// previous one. The setup lock orders setup helpers; it cannot protect readers
// that never take it. Reported by @jatmn.
//
// So the new contents are written complete into a fresh file beside the stamp,
// created with the stamp's protected DACL already in place, and that file is
// renamed over the stamp through its own handle. A reader opens the old stamp or
// the new one and nothing in between, and a process that stops anywhere before
// the rename leaves the previous stamp exactly as it was, plus an unreadable
// replacement file nothing looks for.
//
// POSIX rename semantics are what let the replacement land while a reader has
// the old stamp open, and only for a reader that opened it with
// FILE_SHARE_DELETE, which readWindowsSandboxRuntimeStampFile does. os.ReadFile
// does not, and os.Rename's replace is refused against an open destination
// whatever its share mode, so neither is used here. A reader that holds the
// stamp without share-delete, such as an older Zero, gets a short bounded retry
// and then a failed setup with the previous stamp untouched.

// windowsRuntimeStampPublishSeam runs once the replacement is complete and
// before it replaces the stamp, which is the one point where a stopped process
// leaves both files behind. Nil in production.
var windowsRuntimeStampPublishSeam func()

// windowsRuntimeStampReadSeam runs while a validation read holds the stamp open,
// before it reads. Nil in production.
var windowsRuntimeStampReadSeam func()

var (
	// windowsRuntimeStampRenameAttempts bounds the wait for a reader holding the
	// stamp open without FILE_SHARE_DELETE. A validation read takes microseconds,
	// so a holder that outlasts this is not a validation read.
	windowsRuntimeStampRenameAttempts = 40
	windowsRuntimeStampRenameRetry    = 25 * time.Millisecond
)

// fileRenameInformationEx is FileRenameInformationEx, the class that carries
// rename flags. x/sys/windows names only the older FileRenameInformation.
const fileRenameInformationEx = 65

// fileRenameInformation is FILE_RENAME_INFORMATION(_EX). The first field is
// Flags in the extended class and a BOOLEAN ReplaceIfExists in the older one,
// which reads the same for the value 1. The name is a fixed buffer, as in the
// standard library's own Renameat, because a stamp name is short and fixed.
type fileRenameInformation struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [windows.MAX_PATH]uint16
}

// publishWindowsRuntimeStamp replaces the entry name inside directory with
// contents, protected by descriptor, in one step.
func publishWindowsRuntimeStamp(directory windows.Handle, name string, contents []byte, descriptor *windows.SECURITY_DESCRIPTOR) (err error) {
	handle, err := createWindowsStampReplacement(directory, name, descriptor)
	if err != nil {
		return err
	}
	replacement := os.NewFile(uintptr(handle), name)
	published := false
	defer func() {
		if !published {
			// Nothing was published, so the half-built replacement goes. The stamp
			// itself was never opened.
			if discardErr := markForDeletion(handle); discardErr != nil {
				err = errors.Join(err, fmt.Errorf("remove the unpublished sandbox runtime setup stamp: %w", discardErr))
			}
		}
		if closeErr := replacement.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close the sandbox runtime setup stamp: %w", closeErr))
		}
	}()
	if _, err := replacement.Write(contents); err != nil {
		return fmt.Errorf("write sandbox runtime setup stamp: %w", err)
	}
	if err := replacement.Sync(); err != nil {
		return fmt.Errorf("flush sandbox runtime setup stamp: %w", err)
	}
	if windowsRuntimeStampPublishSeam != nil {
		windowsRuntimeStampPublishSeam()
	}
	if err := renameWindowsFileInto(handle, directory, name); err != nil {
		return fmt.Errorf("publish sandbox runtime setup stamp: %w", err)
	}
	published = true
	return nil
}

// createWindowsStampReplacement creates a new, empty file beside name, protected
// from its first instant.
//
// UNPREDICTABLE AND NEW. The runtime root is writable by the sandbox, so a name
// it could guess is a name it could plant first, with a DACL of its own choosing
// and a handle kept open for later; FILE_CREATE refuses anything already there
// instead of opening it.
//
// The descriptor is applied by the create itself, so there is no moment when
// the file carries the root's inherited capability ACE. The handle still gets
// GENERIC_WRITE although the DACL grants the writer's own account read and
// delete only: access to an object created by the same call is granted as
// requested, not checked against the DACL it was just given (probed on Windows
// 11, unelevated, which is the stricter case).
func createWindowsStampReplacement(directory windows.Handle, name string, descriptor *windows.SECURITY_DESCRIPTOR) (windows.Handle, error) {
	for range 8 {
		var suffix [8]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return 0, fmt.Errorf("name the sandbox runtime setup stamp replacement: %w", err)
		}
		objectName, err := windows.NewNTUnicodeString(name + ".new-" + hex.EncodeToString(suffix[:]))
		if err != nil {
			return 0, fmt.Errorf("encode sandbox runtime setup stamp name: %w", err)
		}
		attributes := windows.OBJECT_ATTRIBUTES{
			RootDirectory:      directory,
			ObjectName:         objectName,
			Attributes:         windows.OBJ_CASE_INSENSITIVE,
			SecurityDescriptor: descriptor,
		}
		attributes.Length = uint32(unsafe.Sizeof(attributes))

		var handle windows.Handle
		var iosb windows.IO_STATUS_BLOCK
		err = windows.NtCreateFile(
			&handle,
			windows.GENERIC_WRITE|windows.DELETE|windows.SYNCHRONIZE,
			&attributes,
			&iosb,
			nil,
			windows.FILE_ATTRIBUTE_NORMAL,
			windows.FILE_SHARE_READ,
			windows.FILE_CREATE,
			windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT,
			0,
			0,
		)
		if errors.Is(err, windows.STATUS_OBJECT_NAME_COLLISION) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("create sandbox runtime setup stamp: %w", err)
		}
		return handle, nil
	}
	return 0, errors.New("create sandbox runtime setup stamp: every generated name was already taken")
}

// renameWindowsFileInto renames the open file to name inside directory,
// replacing whatever entry is there. Both ends are handles, so neither the file
// nor the directory is found again by pathname.
func renameWindowsFileInto(file windows.Handle, directory windows.Handle, name string) error {
	nameUTF16, err := windows.UTF16FromString(name)
	if err != nil {
		return err
	}
	var info fileRenameInformation
	if len(nameUTF16)-1 > len(info.FileName) {
		return fmt.Errorf("stamp name %q is too long to rename to", name)
	}
	info.RootDirectory = directory
	info.FileNameLength = uint32((len(nameUTF16) - 1) * 2)
	copy(info.FileName[:], nameUTF16[:len(nameUTF16)-1])

	class := uint32(fileRenameInformationEx)
	info.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	for attempt := 1; ; attempt++ {
		var iosb windows.IO_STATUS_BLOCK
		err = windows.NtSetInformationFile(file, &iosb, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), class)
		if err == nil {
			return nil
		}
		if class == fileRenameInformationEx && windowsRenameExUnsupported(err) {
			// A volume or a Windows build without POSIX rename. The older replace
			// is still one step; it just cannot land while anyone has the stamp
			// open, which the retry below absorbs.
			class = windows.FileRenameInformation
			info.Flags = 1
			continue
		}
		if !windowsStampReaderInTheWay(err) || attempt >= windowsRuntimeStampRenameAttempts {
			return err
		}
		time.Sleep(windowsRuntimeStampRenameRetry)
	}
}

func windowsRenameExUnsupported(err error) bool {
	return errors.Is(err, windows.STATUS_INVALID_INFO_CLASS) ||
		errors.Is(err, windows.STATUS_NOT_SUPPORTED) ||
		errors.Is(err, windows.STATUS_INVALID_PARAMETER) ||
		errors.Is(err, windows.STATUS_INVALID_DEVICE_REQUEST)
}

// windowsStampReaderInTheWay reports the refusals an open stamp produces: a
// sharing violation from the POSIX rename, access denied from the older one.
func windowsStampReaderInTheWay(err error) bool {
	return errors.Is(err, windows.STATUS_SHARING_VIOLATION) || errors.Is(err, windows.STATUS_ACCESS_DENIED)
}

// readWindowsSandboxRuntimeStampFile reads the stamp the way a publish can
// replace it underneath: with FILE_SHARE_DELETE, which os.ReadFile does not
// pass. Without it, a command validating at the moment setup republished would
// block the rename, and setup would fail rather than wait on it.
func readWindowsSandboxRuntimeStampFile(path string) ([]byte, error) {
	file, err := openWindowsSandboxRuntimeStampFile(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	if windowsRuntimeStampReadSeam != nil {
		windowsRuntimeStampReadSeam()
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, &os.PathError{Op: "read", Path: path, Err: err}
	}
	return data, nil
}

func openWindowsSandboxRuntimeStampFile(path string) (*os.File, error) {
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	handle, err := windows.CreateFile(
		pathUTF16,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}
