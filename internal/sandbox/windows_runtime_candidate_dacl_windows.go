//go:build windows

package sandbox

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// SETUP'S PREPARATION IS A MUTATION, AND IT HAPPENS BEFORE THE TRANSACTION.
//
// ensureWindowsSandboxRuntimeCandidates calls EnsurePrivateDir on every
// candidate, existing ones included, and on Windows that replaces the DACL with
// a protected owner-and-SYSTEM descriptor. A root a previous setup already
// granted the capability SID on loses that grant right there, before the ACL
// transaction has snapshotted anything. If setup then fails on the second
// candidate, or the transaction fails and restores its own baseline, the old
// grant is gone: the marker still says setup succeeded and every command fails
// runtime-capability verification. So the DACLs are snapshotted before the
// preparation touches them and put back on any failure that follows, which
// makes the preparation part of the recoverable operation without giving up
// the ownership and no-follow checks EnsurePrivateDir performs.
type windowsRuntimeCandidateDACL struct {
	Path       string
	Descriptor *windows.SECURITY_DESCRIPTOR
	Protected  bool
	TargetID   windowsFileIdentity
}

// snapshotWindowsRuntimeCandidateDACLs records the DACL and identity of every
// candidate that exists. A missing candidate has nothing to preserve.
func snapshotWindowsRuntimeCandidateDACLs(roots []string) ([]windowsRuntimeCandidateDACL, error) {
	var snapshots []windowsRuntimeCandidateDACL
	for _, root := range roots {
		handle, _, err := openWindowsACLTarget(root)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("snapshot runtime root %s before setup: %w", root, err)
		}
		snapshot, err := snapshotWindowsRuntimeCandidateDACL(handle, root)
		_ = windows.CloseHandle(handle)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, nil
}

func snapshotWindowsRuntimeCandidateDACL(handle windows.Handle, root string) (windowsRuntimeCandidateDACL, error) {
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return windowsRuntimeCandidateDACL{}, fmt.Errorf("read runtime root DACL for %s: %w", root, err)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return windowsRuntimeCandidateDACL{}, fmt.Errorf("read runtime root descriptor control for %s: %w", root, err)
	}
	identity, err := windowsIdentityOfHandle(handle)
	if err != nil {
		return windowsRuntimeCandidateDACL{}, fmt.Errorf("identify runtime root %s: %w", root, err)
	}
	return windowsRuntimeCandidateDACL{
		Path:       root,
		Descriptor: descriptor,
		Protected:  control&windows.SE_DACL_PROTECTED != 0,
		TargetID:   identity,
	}, nil
}

// restoreWindowsRuntimeCandidateDACLs puts each recorded DACL back on the
// object it was read from, protection state included, and refuses an object
// that is no longer the one recorded.
func restoreWindowsRuntimeCandidateDACLs(snapshots []windowsRuntimeCandidateDACL) error {
	var errs []error
	for _, snapshot := range snapshots {
		dacl, _, err := snapshot.Descriptor.DACL()
		if err != nil {
			errs = append(errs, fmt.Errorf("read recorded DACL for %s: %w", snapshot.Path, err))
			continue
		}
		handle, _, err := openWindowsACLTarget(snapshot.Path)
		if err != nil {
			errs = append(errs, fmt.Errorf("re-open runtime root %s to restore its DACL: %w", snapshot.Path, err))
			continue
		}
		got, identityErr := windowsIdentityOfHandle(handle)
		if identityErr != nil || got != snapshot.TargetID {
			_ = windows.CloseHandle(handle)
			errs = append(errs, fmt.Errorf("refusing to restore the DACL of %s: it is no longer the object setup found", snapshot.Path))
			continue
		}
		flags := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
		if snapshot.Protected {
			flags = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
		}
		if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, flags, nil, nil, dacl, nil); err != nil {
			errs = append(errs, fmt.Errorf("restore the DACL of %s: %w", snapshot.Path, err))
		}
		_ = windows.CloseHandle(handle)
	}
	return errors.Join(errs...)
}
