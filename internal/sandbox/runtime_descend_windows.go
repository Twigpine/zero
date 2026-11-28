//go:build windows

package sandbox

import (
	"errors"
	"fmt"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// createRuntimeTailHandleRelative creates the owned tail of the runtime root
// beneath base, one component at a time, from retained handles.
//
// THE NAME IS NOT AUTHORIZATION. The previous shape found the deepest existing
// ancestor with os.Stat and then created each missing component by opening its
// parent BY NAME. Both follow a junction, so a local user could replace the
// predictable owned "zero" component with a junction after the pre-check,
// point it anywhere, let elevated setup create runtime\v1\<hash> beneath that
// target, and put the original back before the post-check. The post-check then
// saw an ordinary path, the plan applied normally, and rollback reported the
// separately created target only as identity-mismatched residue.
//
// So the base is opened by name exactly once, and everything below it is
// addressed relative to a handle with FILE_OPEN_REPARSE_POINT: an existing
// component is opened no-follow and refused if it is a link, and a missing one
// is created relative to its parent's handle and identified from the handle the
// create returned. No component name is resolved twice, so there is no interval
// for a swap to land in, and a junction placed at any owned component is seen
// as the link it is rather than followed.
//
// Redirected cache and TEMP locations ABOVE the owned tail are still allowed:
// they are part of base, which is the operator's business. The restriction is
// on the zero/runtime/v1/<hash> components Zero itself owns.
func createRuntimeTailHandleRelative(base string, tail []string) ([]windowsCreatedRuntimeDir, error) {
	created, parent, err := createRuntimeTailRetainingHandle(base, tail)
	if parent != 0 {
		_ = windows.CloseHandle(parent)
	}
	return created, err
}

// createRuntimeTailRetainingHandle is the same descent, but it hands the caller
// the retained handle to the deepest component instead of closing it.
//
// Lease acquisition needs that handle: the lease file is a sibling of the
// runtime root, so creating it by pathname would re-resolve the owned components
// the descent just verified, which is the whole interval this design removes.
// The caller owns the returned handle and must close it, including when an error
// is returned alongside a non-zero handle.
func createRuntimeTailRetainingHandle(base string, tail []string) ([]windowsCreatedRuntimeDir, windows.Handle, error) {
	if runtimeBaseOpenedByName != nil {
		runtimeBaseOpenedByName(base)
	}
	parent, err := openWindowsDirectoryByName(base)
	if err != nil {
		return nil, 0, fmt.Errorf("open sandbox runtime base %s: %w", base, err)
	}

	if runtimeDescentBarrier != nil {
		runtimeDescentBarrier()
	}

	var created []windowsCreatedRuntimeDir
	for _, path := range tail {
		name := filepath.Base(path)
		// Exists already: another process won the race, or it was there before
		// setup began. Opened no-follow, so a junction here is refused rather than
		// descended, and it is not ours to record.
		existing, openErr := openWindowsChildNoFollow(parent, name,
			windows.FILE_READ_ATTRIBUTES|windows.FILE_TRAVERSE, windows.FILE_DIRECTORY_FILE)
		if openErr == nil {
			_ = windows.CloseHandle(parent)
			parent = existing
			continue
		}
		if !isWindowsNotFound(openErr) {
			return created, parent, fmt.Errorf("inspect sandbox runtime component %s: %w", path, openErr)
		}
		handle, createErr := createWindowsChildDirectory(parent, name)
		if createErr != nil {
			if errors.Is(createErr, windows.STATUS_OBJECT_NAME_COLLISION) {
				// Created between the open and the create by someone else. Same
				// answer as the os.IsExist branch the caller already had: not ours,
				// but not a failure either. Reopen no-follow so the descent
				// continues through a verified object rather than a name.
				existing, reopenErr := openWindowsChildNoFollow(parent, name,
					windows.FILE_READ_ATTRIBUTES|windows.FILE_TRAVERSE, windows.FILE_DIRECTORY_FILE)
				if reopenErr != nil {
					return created, parent, fmt.Errorf("inspect sandbox runtime component %s after a concurrent create: %w", path, reopenErr)
				}
				_ = windows.CloseHandle(parent)
				parent = existing
				continue
			}
			return created, parent, fmt.Errorf("create sandbox runtime root %s: %w", path, createErr)
		}
		identity, idErr := handleRuntimeIdentity(handle)
		if idErr == nil && runtimeCreationFailure != nil {
			idErr = runtimeCreationFailure(path)
		}
		if idErr != nil {
			// CREATED, AND ABOUT TO BE FORGOTTEN. The ledger entry is appended on
			// the next line, so returning here left a directory this run had made
			// with nothing above holding a record of it: setup reports failure and
			// leaves it behind, and if an ancestor WAS recorded, that ancestor's
			// compensation then fails on a directory it cannot explain.
			//
			// Undone through the handle rather than published to the ledger,
			// because the identity the ledger needs is the thing that just failed.
			err := fmt.Errorf("identify the sandbox runtime directory created at %s: %w", path, idErr)
			if undoErr := removeWindowsObjectByHandle(handle); undoErr != nil {
				err = fmt.Errorf("%w; and the directory this run created there could not be removed: %w", err, undoErr)
			}
			_ = windows.CloseHandle(handle)
			return created, parent, err
		}
		created = append(created, windowsCreatedRuntimeDir{path: path, identity: identity, identified: true})
		_ = windows.CloseHandle(parent)
		parent = handle
	}
	return created, parent, nil
}
