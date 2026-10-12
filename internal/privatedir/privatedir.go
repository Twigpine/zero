// Package privatedir creates and safely hardens current-user-owned state
// directories without applying permission changes through a path-only check.
package privatedir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Ensure creates path when needed and enforces owner-only access. Existing
// directories are hardened only after ownership is verified through a bound
// directory handle; foreign-owned paths fail closed.
func Ensure(path string) error {
	root, err := Open(path)
	if err != nil {
		return err
	}
	if err := root.Close(); err != nil {
		return fmt.Errorf("close private directory: %w", err)
	}
	return nil
}

// Open creates path when needed, enforces owner-only access, and returns a
// traversal-resistant handle that callers can retain across subsequent I/O.
func Open(path string) (*os.Root, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve private directory: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("create private directory: %w", err)
	}
	before, err := os.Lstat(absolute)
	if err != nil {
		return nil, fmt.Errorf("inspect private directory entry: %w", err)
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("private directory entry is a symbolic link")
	}
	if !before.IsDir() {
		return nil, fmt.Errorf("private directory path is not a directory")
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, fmt.Errorf("open private directory: %w", err)
	}
	opened, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("inspect opened private directory: %w", err)
	}
	after, err := os.Lstat(absolute)
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("reinspect private directory entry: %w", err)
	}
	if after.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, after) {
		_ = root.Close()
		return nil, fmt.Errorf("private directory entry changed while opening")
	}
	if err := harden(root); err != nil {
		closeErr := root.Close()
		if closeErr != nil {
			return nil, errors.Join(err, fmt.Errorf("close private directory: %w", closeErr))
		}
		return nil, err
	}
	current, err := os.Lstat(absolute)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, current) {
		closeErr := root.Close()
		if err == nil {
			err = fmt.Errorf("private directory entry changed while hardening")
		} else {
			err = fmt.Errorf("verify private directory entry after hardening: %w", err)
		}
		if closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close private directory: %w", closeErr))
		}
		return nil, err
	}
	return root, nil
}
