package planmode

import (
	"fmt"
	"os"
	"time"
)

// writePlanFile creates intermediate directories and replaces path under base
// with content using a true no-follow, handle-relative walk (openat/mkdirat/
// renameat on Unix; NtCreateFile with OBJ_DONT_REPARSE on Windows). A
// concurrent intermediate symlink or reparse-point swap cannot redirect the
// create or rename outside the storage tree.
//
// The storage base itself is created with pathname MkdirAll: it is the walk
// root, not a component under attacker control inside the plans tree. Every
// component under base is then created/opened handle-relative with no-follow.
//
// The durable write is atomic temp+rename relative to the parent directory
// handle. The temp name is PID plus nanoseconds (predictable); O_EXCL /
// FILE_CREATE refuses a colliding or pre-planted path at the final component.
func writePlanFile(base, path, content string) error {
	if err := os.MkdirAll(base, 0o700); err != nil {
		return fmt.Errorf("create plan directory: %w", err)
	}
	if err := os.Chmod(base, 0o700); err != nil {
		return fmt.Errorf("restrict plan directory permissions: %w", err)
	}
	rel, err := relWithinBase(base, path)
	if err != nil {
		return err
	}
	return writePlanUnderBase(base, rel, path, content)
}

// errPlanSymlinkWrite is the stable refusal for final and intermediate
// symlink / reparse-point components on the write path. WritePlan matches on
// "is a symlink".
func errPlanSymlinkWrite(path string) error {
	return fmt.Errorf("plan file %s is a symlink; refusing to write through it", path)
}

// planTempName returns a sibling temp leaf name for atomic replace. The
// suffix is PID plus nanoseconds (predictable, not random); exclusivity of
// the create is what refuses a colliding or pre-planted path.
func planTempName(finalName string) string {
	return fmt.Sprintf("%s.tmp-%d-%d", finalName, os.Getpid(), time.Now().UnixNano())
}
