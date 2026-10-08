//go:build windows

package swarm

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func makeRedirectedLockPath(t *testing.T, root, target string) string {
	t.Helper()
	linkDir := filepath.Join(root, "redirected")
	out, err := exec.Command("cmd", "/c", "mklink", "/J", linkDir, filepath.Dir(target)).CombinedOutput()
	if err != nil {
		t.Fatalf("create redirected lock junction: %v %s", err, strings.TrimSpace(string(out)))
	}
	return filepath.Join(linkDir, filepath.Base(target))
}
