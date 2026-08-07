package planmode

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// readPlanFile reads path by opening the plan storage base as an os.Root and
// opening the file relative to that handle. Intermediate directory components
// and the final name are resolved with the root's traversal-resistant open
// (openat/RESOLVE_BENEATH on Unix, handle-relative opens on Windows), so a
// concurrent symlink or reparse-point swap under the base cannot redirect the
// read outside the storage tree. Final-component-only O_NOFOLLOW is not enough
// for that property.
//
// A symlink final component is refused even when its target would stay inside
// the root: durable plan files are plain files, and reading through a link
// would re-introduce a replace-with-symlink race against the intended path.
func readPlanFile(base, path string) ([]byte, error) {
	rel, err := relWithinBase(base, path)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	info, err := root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("plan file %s is a symlink; refusing to read through it", path)
	}
	file, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

// relWithinBase returns path relative to base after both are cleaned to
// absolute form, rejecting any lexical escape. The relative name is what
// os.Root opens; absolute pathname open is intentionally not used.
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
