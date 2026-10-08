//go:build windows

package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWriteStatusFilePreservesExistingProtectedDACL(t *testing.T) {
	dir := t.TempDir()
	secureStatusTestDir(t, dir)
	path := filepath.Join(dir, "daemon.status")
	if err := os.WriteFile(path, []byte(`{"pid":7}`), 0o600); err != nil {
		t.Fatal(err)
	}
	restricted, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;OW)")
	if err != nil {
		t.Skipf("cannot build restrictive DACL: %v", err)
	}
	dacl, _, err := restricted.DACL()
	if err != nil {
		t.Skipf("cannot read restrictive DACL: %v", err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		t.Skipf("cannot apply restrictive DACL: %v", err)
	}
	want := statusDACL(t, path)

	server := &Server{
		startedAt: time.Date(2026, 8, 30, 18, 0, 0, 0, time.UTC),
		opts: ServerOptions{
			Paths:   Paths{Socket: filepath.Join(dir, "daemon.sock"), Status: path},
			Version: 2,
		},
	}
	if err := server.writeStatusFile(); err != nil {
		t.Fatalf("writeStatusFile: %v", err)
	}
	if got := statusDACL(t, path); got != want {
		t.Fatalf("status DACL after replacement = %q, want %q", got, want)
	}
}

func statusDACL(t *testing.T, path string) string {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Skipf("cannot read status DACL: %v", err)
	}
	text := descriptor.String()
	if index := strings.Index(text, "D:"); index >= 0 {
		return text[index:]
	}
	return text
}
