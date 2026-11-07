//go:build !windows

package fsutil

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWriteFileAtomicCallsChownWithDestOwner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dest")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	var gotUID, gotGID int
	called := false
	orig := posixChown
	t.Cleanup(func() { posixChown = orig })
	posixChown = func(f *os.File, uid, gid int) error {
		called = true
		gotUID, gotGID = uid, gid
		return orig(f, uid, gid)
	}
	if err := WriteFileAtomic(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("posixChown was not called; owner would be the writer inode")
	}
	if gotUID != int(stat.Uid) || gotGID != int(stat.Gid) {
		t.Fatalf("chown uid/gid = %d/%d, want %d/%d", gotUID, gotGID, stat.Uid, stat.Gid)
	}
}
