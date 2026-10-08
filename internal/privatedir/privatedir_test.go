package privatedir

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOpenRejectsFinalDirectorySymlinkWithoutHardeningTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating directory symlinks requires privileges on some Windows runners")
	}
	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "private")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(link); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("Open symlink error = %v, want symbolic-link rejection", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("symlink target permissions = %04o, want unchanged 0755", got)
	}
}
