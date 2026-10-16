package tools

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Gitlawb/zero/internal/sandbox"
)

// This file is the mandatory, engine-independent half of the daemon-token
// boundary for direct file tools. Registry.Run and RunWithOptions without a
// sandbox engine never reach Engine.Evaluate, so the tools must enforce the
// process-owned credential set themselves.
//
// Reads open through an os.Root tied to the selected workspace/scope root, then
// compare metadata from that same handle with the protected credential set.
// Writes open through the same root, verify the opened identity before
// truncation, and exclusively create new files. Consequently path resolution
// cannot be redirected to the token between authorization and use, while an
// ordinary existing file keeps its inode-level metadata and hard links.

// protectedReadOpen opens path through the workspace root and verifies the
// identity obtained from that same handle before any content is consumed.
func protectedReadOpen(path, workspaceRoot string) (*os.File, os.FileInfo, error) {
	rootPath, relative, err := rootedPathWithin([]string{workspaceRoot}, path)
	if err != nil {
		// Explicitly granted extra roots and spill files have already passed their
		// own canonical containment checks. Bind their final lookup to the
		// canonical parent rather than falling back to an unrestricted os.Open.
		rootPath = filepath.Dir(path)
		relative = filepath.Base(path)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, nil, err
	}
	file, err := root.Open(relative)
	closeErr := root.Close()
	if err != nil {
		return nil, nil, err
	}
	if closeErr != nil {
		file.Close()
		return nil, nil, closeErr
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	exclusions := sandbox.ProtectedCredentialExclusions(workspaceRoot)
	if exclusions.FileHandleExcluded(path, file, info) {
		file.Close()
		return nil, nil, protectedCredentialErr(path, "readable")
	}
	return file, info, nil
}

// ProtectedReadOpen opens path through a rooted handle and rejects any handle
// whose identity belongs to a protected credential. Callers must read from the
// returned handle rather than reopening path.
func ProtectedReadOpen(path, workspaceRoot string) (*os.File, os.FileInfo, error) {
	return protectedReadOpen(path, workspaceRoot)
}

// protectedRootRead is the equivalent for callers that already hold the root,
// notably structured and staged unified patches.
func protectedRootRead(root *os.Root, relative, absolute, workspaceRoot string) (*os.File, os.FileInfo, error) {
	file, err := root.Open(relative)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	exclusions := sandbox.ProtectedCredentialExclusions(workspaceRoot)
	if exclusions.FileHandleExcluded(absolute, file, info) {
		file.Close()
		return nil, nil, protectedCredentialErr(absolute, "readable")
	}
	return file, info, nil
}

// protectedMutationDenied is the early, lexical refusal. Existing files are
// checked again from the handle that writeRootedFile will modify, closing a
// swap race without replacing the file's inode and losing its metadata.
func protectedMutationDenied(path, workspaceRoot string) error {
	exclusions := sandbox.ProtectedCredentialExclusions(workspaceRoot)
	if exclusions.PathExcluded(path) {
		return protectedCredentialErr(path, "writable")
	}
	return nil
}

func writeRootedFile(root *os.Root, relative, absolute, workspaceRoot string, content []byte, mode os.FileMode, createOnly bool) (bool, error) {
	parent := filepath.Dir(relative)
	if err := root.MkdirAll(parent, 0o755); err != nil {
		return false, err
	}
	flags := os.O_WRONLY
	if createOnly {
		flags |= os.O_CREATE | os.O_EXCL
	}
	file, err := root.OpenFile(relative, flags, mode.Perm())
	if err != nil {
		return false, err
	}
	committed := createOnly
	if !createOnly {
		info, statErr := file.Stat()
		if statErr != nil {
			file.Close()
			return false, statErr
		}
		exclusions := sandbox.ProtectedCredentialExclusions(workspaceRoot)
		if exclusions.FileHandleExcluded(absolute, file, info) {
			file.Close()
			return false, protectedCredentialErr(absolute, "writable")
		}
		if err := file.Truncate(0); err != nil {
			file.Close()
			return false, err
		}
		committed = true
	}
	_, writeErr := io.Copy(file, bytes.NewReader(content))
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return committed, errors.Join(writeErr, closeErr)
	}
	return true, nil
}

func protectedCredentialErr(path, verb string) error {
	return fmt.Errorf("%s holds the remote bridge token and is never %s", path, verb)
}
