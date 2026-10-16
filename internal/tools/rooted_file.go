package tools

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// readRootedFile returns bytes and identity from the same opened object, and
// verifies that the rooted path still names that object after the read. Root.Open
// binds symlink containment to use rather than relying on a pathname pre-check.
func readRootedFile(root *os.Root, relativePath string) ([]byte, os.FileInfo, error) {
	file, openedInfo, err := protectedRootRead(root, relativePath, filepath.Join(root.Name(), relativePath), root.Name())
	if err != nil {
		return nil, nil, err
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()
	if !openedInfo.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s is not a regular file", relativePath)
	}
	content, err := io.ReadAll(file)
	if err != nil {
		return nil, nil, err
	}
	pathInfo, err := root.Stat(relativePath)
	if err != nil || !os.SameFile(openedInfo, pathInfo) {
		return nil, nil, fmt.Errorf("%w: path identity changed", errFileChangedDuringWrite)
	}
	if err := file.Close(); err != nil {
		return nil, nil, err
	}
	closed = true
	return content, openedInfo, nil
}
