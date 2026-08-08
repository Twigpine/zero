//go:build !unix && !windows

package planmode

import (
	"fmt"
	"os"
	"path/filepath"
)

// writePlanUnderBase is a best-effort fallback for platforms without openat /
// OBJ_DONT_REPARSE primitives. It uses os.Root for create and rename so the
// walk stays rooted at base and each intermediate component is created with a
// single Root.Mkdir. os.Root still resolves in-root symlinks, so this is
// weaker than the openat / OBJ_DONT_REPARSE paths. Zero's supported targets
// are Unix and Windows.
func writePlanUnderBase(base, rel, displayPath, content string) error {
	parts, err := relComponents(rel)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return fmt.Errorf("create plan directory: %w", err)
	}
	defer root.Close()

	// Create intermediate directories one component at a time so a missing
	// parent does not require pathname MkdirAll outside the root.
	dirRel := "."
	for i := 0; i < len(parts)-1; i++ {
		if dirRel == "." {
			dirRel = parts[i]
		} else {
			dirRel = filepath.Join(dirRel, parts[i])
		}
		if err := root.Mkdir(dirRel, 0o700); err != nil && !os.IsExist(err) {
			return fmt.Errorf("create plan directory: %w", err)
		}
	}

	final := parts[len(parts)-1]
	var finalRel string
	if dirRel == "." {
		finalRel = final
	} else {
		finalRel = filepath.Join(dirRel, final)
	}
	if info, err := root.Lstat(finalRel); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errPlanSymlinkWrite(displayPath)
	}

	tmpName := planTempName(final)
	var tmpRel string
	if dirRel == "." {
		tmpRel = tmpName
	} else {
		tmpRel = filepath.Join(dirRel, tmpName)
	}
	file, err := root.OpenFile(tmpRel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("write plan file: %w", err)
	}
	if _, err := file.WriteString(content); err != nil {
		file.Close()
		_ = root.Remove(tmpRel)
		return fmt.Errorf("write plan file: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = root.Remove(tmpRel)
		return fmt.Errorf("write plan file: %w", err)
	}
	if err := root.Rename(tmpRel, finalRel); err != nil {
		_ = root.Remove(tmpRel)
		return fmt.Errorf("replace plan file: %w", err)
	}
	return nil
}
