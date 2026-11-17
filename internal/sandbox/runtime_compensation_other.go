//go:build !windows

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// runtimeCompensationSwapSeam exists so the shared compensation code compiles
// everywhere. Only the Windows build closes a check-then-mutate window, because
// only there does setup run elevated against a tree an unelevated process can
// rename.
var runtimeCompensationSwapSeam func()

func compensateRuntimeStampBound(root string, identity string, name string, prior []byte, existed bool) error {
	current, ok := runtimeDirIdentity(root)
	if !ok {
		return fmt.Errorf("identify the sandbox runtime root %s for stamp compensation", root)
	}
	if current != identity {
		return fmt.Errorf("sandbox runtime root %s is no longer the directory this setup stamped; "+
			"leaving the replacement untouched, and the original still carries this run's stamp", root)
	}
	if runtimeCompensationSwapSeam != nil {
		runtimeCompensationSwapSeam()
	}
	path := filepath.Join(root, name)
	if !existed {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove sandbox runtime setup stamp written by this run: %w", err)
		}
		return nil
	}
	if err := os.WriteFile(path, prior, 0o600); err != nil {
		return fmt.Errorf("restore the previous sandbox runtime setup stamp: %w", err)
	}
	return nil
}

func removeCreatedRuntimeDirBound(path string, identity string) error {
	current, ok := runtimeDirIdentity(path)
	if !ok {
		// ONLY ABSENCE MEANS THERE IS NOTHING TO UNDO. Any lookup failure used to
		// return here, so a directory that exists but could not be identified was
		// reported as cleanly removed while it stayed on disk. The Windows build
		// already draws this line, and the rollback it serves promises to report
		// what it could not remove. Reported by CodeRabbit.
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("sandbox runtime root %s was created by this run but could not be identified for removal; leaving it in place", path)
	}
	if current != identity {
		return fmt.Errorf("sandbox runtime root %s is no longer the directory this run created; "+
			"leaving the replacement in place", path)
	}
	if runtimeCompensationSwapSeam != nil {
		runtimeCompensationSwapSeam()
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove sandbox runtime root %s created by this run: %w", path, err)
	}
	return nil
}
