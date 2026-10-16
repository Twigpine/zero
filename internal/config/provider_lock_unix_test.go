//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProviderWriteLockReleaseDoesNotRemoveReplacement(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	lockPath := filepath.Join(dir, ".zero-provider-write.lock")
	release, err := lockProviderWrite(configPath)
	if err != nil {
		t.Fatal(err)
	}

	originalPath := lockPath + ".original"
	if err := os.Rename(lockPath, originalPath); err != nil {
		_ = release()
		t.Fatal(err)
	}
	const replacement = "new-holder"
	if err := os.WriteFile(lockPath, []byte(replacement), 0o600); err != nil {
		_ = release()
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != replacement {
		t.Fatalf("replacement lock = %q, want %q preserved", data, replacement)
	}
}
