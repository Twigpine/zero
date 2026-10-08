//go:build unix

package sandbox

import (
	"path/filepath"
	"syscall"
	"testing"
)

func TestGitMetadataWriteCarveoutsPreservesSpecialGitEntryBehavior(t *testing.T) {
	root := t.TempDir()
	gitPath := filepath.Join(root, ".git")
	if err := syscall.Mkfifo(gitPath, 0o644); err != nil {
		t.Skip(err)
	}
	mustEqualCarveouts(t, gitMetadataWriteCarveouts(root), []string{
		filepath.Join(gitPath, "hooks"),
		filepath.Join(gitPath, "config"),
	})
}
