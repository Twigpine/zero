//go:build !windows

package swarm

import (
	"os"
	"path/filepath"
	"testing"
)

func makeRedirectedLockPath(t *testing.T, root, target string) string {
	t.Helper()
	lockPath := filepath.Join(root, "redirected.lock")
	if err := os.Symlink(target, lockPath); err != nil {
		t.Fatalf("create redirected lock path: %v", err)
	}
	return lockPath
}
