package planmode

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// readPlanFile reads path by walking components under the plan storage base
// with a true no-follow open on every component (openat(O_NOFOLLOW) on Unix,
// NtCreateFile with OBJ_DONT_REPARSE on Windows). Intermediate directories and
// the final name are opened relative to the previous handle, so a concurrent
// symlink or reparse-point swap cannot redirect the read outside the storage
// tree and cannot replace a regular plan file with an in-root symlink between
// a pre-open Lstat and Open.
//
// A symlink final component is refused even when its target would stay inside
// the base: durable plan files are plain files, and reading through a link
// would re-introduce a replace-with-symlink race against the intended path.
// os.Root.Open is intentionally not used: it follows in-root symlinks after
// O_NOFOLLOW fails (checkSymlink), which is exactly the race we refuse.
func readPlanFile(base, path string) ([]byte, error) {
	rel, err := relWithinBase(base, path)
	if err != nil {
		return nil, err
	}
	file, err := openPlanUnderBase(base, rel, path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

// errPlanSymlink is the stable refusal message for final and intermediate
// symlink / reparse-point components. ReadPlan matches on "is a symlink".
func errPlanSymlink(path string) error {
	return fmt.Errorf("plan file %s is a symlink; refusing to read through it", path)
}

// relWithinBase returns path relative to base after both are cleaned to
// absolute form, rejecting any lexical escape. The relative name is what the
// no-follow walk opens; absolute pathname open is intentionally not used.
func relWithinBase(base, path string) (string, error) {
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(absBase, absPath)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("plan path %s escapes plan storage root %s", path, base)
	}
	if rel == "." {
		return "", fmt.Errorf("plan path %s is the storage root, not a file", path)
	}
	return rel, nil
}

// relComponents splits a storage-relative path into single-component names for
// a handle-relative openat/NtCreateFile walk. ".." and absolute forms are
// rejected even though relWithinBase already filters them, so the walker stays
// closed under a malicious or miscomputed relative name.
func relComponents(rel string) ([]string, error) {
	rel = filepath.Clean(rel)
	if rel == "." {
		return nil, fmt.Errorf("plan path is the storage root, not a file")
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("plan path escapes plan storage root")
	}
	if filepath.IsAbs(rel) {
		return nil, fmt.Errorf("plan path must be relative to the storage root")
	}
	slash := filepath.ToSlash(rel)
	raw := strings.Split(slash, "/")
	parts := make([]string, 0, len(raw))
	for _, p := range raw {
		if p == "" || p == "." {
			continue
		}
		if p == ".." {
			return nil, fmt.Errorf("plan path escapes plan storage root")
		}
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("plan path is the storage root, not a file")
	}
	return parts, nil
}
