//go:build windows

package hooks

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestLoadConfigRejectsDanglingJunctionParent(t *testing.T) {
	dir := t.TempDir()
	workspace := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "junction-target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(workspace, ".zero")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("mklink /J unavailable: %v (%s)", err, out)
	}
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}

	result, err := LoadConfig(LoadOptions{
		UserConfigPath:    filepath.Join(dir, "missing-user-hooks.json"),
		ProjectConfigPath: filepath.Join(link, "hooks.json"),
	})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("LoadConfig error = %v, want ErrInvalidConfig", err)
	}
	if !hasHookDiagnostic(result.Diagnostics, DiagnosticIO, "", "") {
		t.Fatalf("missing dangling-junction diagnostic: %#v", result.Diagnostics)
	}
}
