package update

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The self-updater installs npmPackageName and recognises installs by it, so
// it must be the name the release actually publishes. A rename that misses
// this constant leaves `zero upgrade` reinstalling the old package.
func TestNpmPackageNameMatchesPackageJSON(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "package.json"))
	if err != nil {
		t.Fatalf("ReadFile package.json: %v", err)
	}
	var pkg struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		t.Fatalf("Unmarshal package.json: %v", err)
	}
	if pkg.Name != npmPackageName {
		t.Fatalf("package.json name = %q, npmPackageName = %q; they must match", pkg.Name, npmPackageName)
	}
}

func TestDetectInstallMethodStandaloneByDefault(t *testing.T) {
	dir := t.TempDir()
	exePath := filepath.Join(dir, "zero")
	if err := os.WriteFile(exePath, []byte("binary"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if method := DetectInstallMethod(exePath); method != InstallMethodStandalone {
		t.Fatalf("DetectInstallMethod = %q, want standalone", method)
	}
}

func TestDetectInstallMethodNpmViaMarkerFile(t *testing.T) {
	dir := t.TempDir()
	exePath := filepath.Join(dir, "zero")
	if err := os.WriteFile(exePath, []byte("binary"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".zero-binary-version"), []byte("0.1.0\n"), 0o644); err != nil {
		t.Fatalf("WriteFile marker: %v", err)
	}
	if method := DetectInstallMethod(exePath); method != InstallMethodNpm {
		t.Fatalf("DetectInstallMethod = %q, want npm", method)
	}
}

func TestDetectInstallMethodNpmViaPackageJSON(t *testing.T) {
	dir := t.TempDir()
	exePath := filepath.Join(dir, "zero")
	if err := os.WriteFile(exePath, []byte("binary"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"@twigpine/zero","version":"0.5.0-linux-x64","os":["linux"],"cpu":["x64"]}`), 0o644); err != nil {
		t.Fatalf("WriteFile package.json: %v", err)
	}
	if method := DetectInstallMethod(exePath); method != InstallMethodNpm {
		t.Fatalf("DetectInstallMethod = %q, want npm", method)
	}
}

func TestDetectInstallMethodDoesNotTreatRepositoryManifestAsNpmBinary(t *testing.T) {
	dir := t.TempDir()
	exePath := filepath.Join(dir, "zero")
	if err := os.WriteFile(exePath, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"@twigpine/zero","bin":{"zero":"bin/zero.js"},"os":["linux","darwin","win32","android"],"cpu":["x64","arm64"]}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if method := DetectInstallMethod(exePath); method != InstallMethodStandalone {
		t.Fatalf("DetectInstallMethod = %q, want standalone for repository wrapper manifest", method)
	}
}

func TestDetectInstallMethodIgnoresUnrelatedPackageJSON(t *testing.T) {
	dir := t.TempDir()
	exePath := filepath.Join(dir, "zero")
	if err := os.WriteFile(exePath, []byte("binary"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"some-other-package"}`), 0o644); err != nil {
		t.Fatalf("WriteFile package.json: %v", err)
	}
	if method := DetectInstallMethod(exePath); method != InstallMethodStandalone {
		t.Fatalf("DetectInstallMethod = %q, want standalone", method)
	}
}

// The bridge release published under the old name must recognise itself, so
// `zero upgrade` can tell it to switch instead of reinstalling the same build.
func TestDetectInstallMethodRecognisesALegacyPlatformPayload(t *testing.T) {
	dir := t.TempDir()
	exePath := filepath.Join(dir, "zero")
	if err := os.WriteFile(exePath, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"@gitlawb/zero","version":"0.10.0-linux-x64","os":["linux"],"cpu":["x64"]}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if method := DetectInstallMethod(exePath); method != InstallMethodNpmLegacy {
		t.Fatalf("DetectInstallMethod = %q, want %q", method, InstallMethodNpmLegacy)
	}
}

// A binary the first-run downloader fetched sits next to the wrapper's own
// package.json, which is the only place the installed name is recorded.
func TestDetectInstallMethodRecognisesALegacyDownloadedBinary(t *testing.T) {
	dir := t.TempDir()
	exePath := filepath.Join(dir, "zero")
	if err := os.WriteFile(exePath, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".zero-binary-version"), []byte("0.10.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"@gitlawb/zero","bin":{"zero":"bin/zero.js"},"os":["linux","darwin","win32","android"],"cpu":["x64","arm64"]}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if method := DetectInstallMethod(exePath); method != InstallMethodNpmLegacy {
		t.Fatalf("DetectInstallMethod = %q, want %q", method, InstallMethodNpmLegacy)
	}
}

// Without the downloader's marker, a wrapper-shaped manifest under the old
// name is a checkout, not an install, exactly as for the current name.
func TestDetectInstallMethodDoesNotTreatALegacyWrapperManifestAsAnInstall(t *testing.T) {
	dir := t.TempDir()
	exePath := filepath.Join(dir, "zero")
	if err := os.WriteFile(exePath, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"@gitlawb/zero","bin":{"zero":"bin/zero.js"},"os":["linux","darwin","win32","android"],"cpu":["x64","arm64"]}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if method := DetectInstallMethod(exePath); method != InstallMethodStandalone {
		t.Fatalf("DetectInstallMethod = %q, want standalone", method)
	}
}
