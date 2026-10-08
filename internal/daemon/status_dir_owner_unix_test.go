//go:build !windows

package daemon

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func secureStatusTestDirPlatform(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}

func broadenStatusTestDirPlatform(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestCheckStatusDirOwnerRejectsMissingMetadata(t *testing.T) {
	err := checkStatusDirOwner(nil, statusDirOwnerTestInfo{})
	if err == nil || !strings.Contains(err.Error(), "metadata is unavailable") {
		t.Fatalf("checkStatusDirOwner error = %v, want unavailable metadata rejection", err)
	}
}

func TestCheckStatusDirOwnerRejectsDifferentUser(t *testing.T) {
	info := statusDirOwnerTestInfo{sys: &syscall.Stat_t{Uid: uint32(os.Geteuid() + 1)}}
	err := checkStatusDirOwner(nil, info)
	if err == nil || !strings.Contains(err.Error(), "not the current user") {
		t.Fatalf("checkStatusDirOwner error = %v, want owner mismatch rejection", err)
	}
}

type statusDirOwnerTestInfo struct {
	sys any
}

func (statusDirOwnerTestInfo) Name() string       { return "." }
func (statusDirOwnerTestInfo) Size() int64        { return 0 }
func (statusDirOwnerTestInfo) Mode() os.FileMode  { return os.ModeDir | 0o700 }
func (statusDirOwnerTestInfo) ModTime() time.Time { return time.Time{} }
func (statusDirOwnerTestInfo) IsDir() bool        { return true }
func (info statusDirOwnerTestInfo) Sys() any      { return info.sys }
