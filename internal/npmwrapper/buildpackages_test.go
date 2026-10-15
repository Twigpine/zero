package npmwrapper

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestBuildPlatformPackagesAssemblesPublishPayloads runs the release-time
// assembly script against a fixture linux-x64 archive shaped like the output
// of `zero-release package` and checks the npm publish payloads it emits:
// the platform payload (suffixed version, os/cpu gate, pack-safe helper
// shims, pruned agent-browser binaries) and the wrapper (no scripts, no
// dependencies, full optionalDependencies alias matrix).
func TestBuildPlatformPackagesAssemblesPublishPayloads(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses POSIX symlinks and shell scripts")
	}
	node := requireNode(t)
	tar := requireCommand(t, "tar")
	version := packageVersion(t)

	root := writePlatformArchiveFixture(t, tar, version)
	outDir := filepath.Join(root, "out")
	runBuildPlatformPackages(t, node, root, outDir)

	platformDir := filepath.Join(outDir, "platforms", "zero-linux-x64")
	var platformPkg struct {
		Name    string   `json:"name"`
		Version string   `json:"version"`
		OS      []string `json:"os"`
		CPU     []string `json:"cpu"`
		Scripts map[string]string
	}
	unmarshalJSONFile(t, filepath.Join(platformDir, "package.json"), &platformPkg)
	if platformPkg.Name != "@twigpine/zero" {
		t.Fatalf("platform package name = %q, want @twigpine/zero (same-name suffixed versions share one trusted publisher)", platformPkg.Name)
	}
	if want := version + "-linux-x64"; platformPkg.Version != want {
		t.Fatalf("platform package version = %q, want %q", platformPkg.Version, want)
	}
	if len(platformPkg.OS) != 1 || platformPkg.OS[0] != "linux" || len(platformPkg.CPU) != 1 || platformPkg.CPU[0] != "x64" {
		t.Fatalf("platform package os/cpu = %v/%v, want [linux]/[x64]", platformPkg.OS, platformPkg.CPU)
	}
	if len(platformPkg.Scripts) != 0 {
		t.Fatalf("platform package has lifecycle scripts: %v", platformPkg.Scripts)
	}

	info, err := os.Stat(filepath.Join(platformDir, "zero"))
	if err != nil {
		t.Fatalf("platform package binary missing: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("platform package binary is not executable: %v", info.Mode())
	}
	for _, excluded := range []string{"README.md", "VERSION", "bin"} {
		if _, err := os.Stat(filepath.Join(platformDir, excluded)); !os.IsNotExist(err) {
			t.Fatalf("platform package still contains wrapper-owned %q", excluded)
		}
	}
	// npm only ships a license file it finds in the packed directory, so the
	// assembly must place the repo LICENSE into every payload.
	if _, err := os.Stat(filepath.Join(platformDir, "LICENSE")); err != nil {
		t.Fatalf("platform package missing LICENSE: %v", err)
	}

	// npm pack drops symlinks, so the shim must now be a regular executable file.
	shimPath := filepath.Join(platformDir, "helpers", "node_modules", ".bin", "agent-browser")
	shimInfo, err := os.Lstat(shimPath)
	if err != nil {
		t.Fatalf("agent-browser shim missing: %v", err)
	}
	if shimInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("agent-browser shim is still a symlink; npm pack would drop it")
	}
	if shimInfo.Mode()&0o111 == 0 {
		t.Fatalf("agent-browser shim is not executable: %v", shimInfo.Mode())
	}
	shim, err := os.ReadFile(shimPath)
	if err != nil {
		t.Fatalf("ReadFile shim: %v", err)
	}
	if !strings.Contains(string(shim), "../agent-browser/bin/agent-browser.js") {
		t.Fatalf("shim does not exec the vendored helper: %q", string(shim))
	}
	if _, err := os.Stat(filepath.Join(platformDir, "helpers", "node_modules", ".bin", "tuistory")); err != nil {
		t.Fatalf("tuistory shim missing: %v", err)
	}

	// Pruning: only the linux-x64 (glibc + musl) agent-browser binaries survive.
	prunedBinDir := filepath.Join(platformDir, "helpers", "node_modules", "agent-browser", "bin")
	entries, err := os.ReadDir(prunedBinDir)
	if err != nil {
		t.Fatalf("ReadDir pruned bin: %v", err)
	}
	got := []string{}
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	want := map[string]bool{
		"agent-browser.js":             true,
		"agent-browser-linux-x64":      true,
		"agent-browser-linux-musl-x64": true,
	}
	if len(got) != len(want) {
		t.Fatalf("pruned agent-browser bin = %v, want exactly %v", got, want)
	}
	for _, name := range got {
		if !want[name] {
			t.Fatalf("pruning left unexpected %q (full set: %v)", name, got)
		}
	}

	wrapperDir := filepath.Join(outDir, "wrapper")
	var wrapperPkg struct {
		Name         string            `json:"name"`
		Version      string            `json:"version"`
		Scripts      map[string]string `json:"scripts"`
		Dependencies map[string]string `json:"dependencies"`
		Optional     map[string]string `json:"optionalDependencies"`
	}
	unmarshalJSONFile(t, filepath.Join(wrapperDir, "package.json"), &wrapperPkg)
	if wrapperPkg.Version != version {
		t.Fatalf("wrapper version = %q, want %q", wrapperPkg.Version, version)
	}
	if len(wrapperPkg.Scripts) != 0 {
		t.Fatalf("published wrapper has lifecycle scripts: %v", wrapperPkg.Scripts)
	}
	if len(wrapperPkg.Dependencies) != 0 {
		t.Fatalf("published wrapper has dependencies: %v (helpers are vendored in the platform payloads)", wrapperPkg.Dependencies)
	}
	wantAliases := map[string]string{
		"@twigpine/zero-darwin-arm64": "npm:@twigpine/zero@" + version + "-darwin-arm64",
		"@twigpine/zero-darwin-x64":   "npm:@twigpine/zero@" + version + "-darwin-x64",
		"@twigpine/zero-linux-arm64":  "npm:@twigpine/zero@" + version + "-linux-arm64",
		"@twigpine/zero-linux-x64":    "npm:@twigpine/zero@" + version + "-linux-x64",
		"@twigpine/zero-win32-x64":    "npm:@twigpine/zero@" + version + "-win32-x64",
	}
	if len(wrapperPkg.Optional) != len(wantAliases) {
		t.Fatalf("wrapper optionalDependencies = %v, want %v", wrapperPkg.Optional, wantAliases)
	}
	for alias, spec := range wantAliases {
		if wrapperPkg.Optional[alias] != spec {
			t.Fatalf("wrapper optionalDependencies[%q] = %q, want %q", alias, wrapperPkg.Optional[alias], spec)
		}
	}
	for _, file := range []string{"bin/zero.js", "scripts/postinstall.mjs", "README.md", "LICENSE"} {
		if _, err := os.Stat(filepath.Join(wrapperDir, filepath.FromSlash(file))); err != nil {
			t.Fatalf("wrapper payload missing %s: %v", file, err)
		}
	}
}

// writePlatformArchiveFixture writes a linux-x64 release archive shaped like
// the output of `zero-release package`, with its .sha256 sidecar, into a new
// directory and returns that directory.
func writePlatformArchiveFixture(t *testing.T, tar string, version string) string {
	t.Helper()
	root := t.TempDir()
	staging := filepath.Join(root, "staging")

	// Payload the platform package must keep.
	writeFixtureFile(t, filepath.Join(staging, "zero"), "#!/usr/bin/env sh\necho fixture-zero\n", 0o755)
	writeFixtureFile(t, filepath.Join(staging, "zero-linux-sandbox"), "#!/usr/bin/env sh\n", 0o755)
	writeFixtureFile(t, filepath.Join(staging, "zero-seccomp"), "#!/usr/bin/env sh\n", 0o755)
	// Wrapper-owned files the platform package must exclude.
	writeFixtureFile(t, filepath.Join(staging, "package.json"), `{"name":"@twigpine/zero"}`, 0o644)
	writeFixtureFile(t, filepath.Join(staging, "README.md"), "readme\n", 0o644)
	writeFixtureFile(t, filepath.Join(staging, "VERSION"), version+"\n", 0o644)
	writeFixtureFile(t, filepath.Join(staging, "bin", "zero.js"), "#!/usr/bin/env node\n", 0o755)
	// Vendored helpers tree with npm's symlink .bin shims and agent-browser's
	// multi-platform binary set.
	agentBrowserDir := filepath.Join(staging, "helpers", "node_modules", "agent-browser")
	writeFixtureFile(t, filepath.Join(agentBrowserDir, "package.json"), `{"name":"agent-browser"}`, 0o644)
	writeFixtureFile(t, filepath.Join(agentBrowserDir, "bin", "agent-browser.js"), "#!/usr/bin/env node\nconsole.log('fixture agent-browser')\n", 0o644)
	for _, name := range []string{
		"agent-browser-linux-x64",
		"agent-browser-linux-musl-x64",
		"agent-browser-linux-arm64",
		"agent-browser-darwin-arm64",
		"agent-browser-darwin-x64",
		"agent-browser-win32-x64.exe",
	} {
		writeFixtureFile(t, filepath.Join(agentBrowserDir, "bin", name), "native\n", 0o755)
	}
	tuistoryDir := filepath.Join(staging, "helpers", "node_modules", "tuistory")
	writeFixtureFile(t, filepath.Join(tuistoryDir, "dist", "cli.js"), "#!/usr/bin/env node\n", 0o644)
	binDir := filepath.Join(staging, "helpers", "node_modules", ".bin")
	mustMkdirAllFixture(t, binDir)
	if err := os.Symlink("../agent-browser/bin/agent-browser.js", filepath.Join(binDir, "agent-browser")); err != nil {
		t.Fatalf("Symlink agent-browser shim: %v", err)
	}
	if err := os.Symlink("../tuistory/dist/cli.js", filepath.Join(binDir, "tuistory")); err != nil {
		t.Fatalf("Symlink tuistory shim: %v", err)
	}

	// Archive with the payload at the archive root, like zero-release's
	// createArchive, plus the sha256 sidecar the script verifies against.
	assetName := fmt.Sprintf("zero-v%s-linux-x64.tar.gz", version)
	archivePath := filepath.Join(root, assetName)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, tar, "-C", staging, "-czf", archivePath, ".").CombinedOutput(); err != nil {
		t.Fatalf("tar failed: %v\n%s", err, output)
	}
	archiveBytes, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatalf("ReadFile archive: %v", err)
	}
	digest := sha256.Sum256(archiveBytes)
	writeFixtureFile(t, archivePath+".sha256", fmt.Sprintf("%x  %s\n", digest, assetName), 0o644)
	return root
}

func runBuildPlatformPackages(t *testing.T, node string, artifactsDir string, outDir string, extraArgs ...string) {
	t.Helper()
	script := filepath.Join(repoRoot(t), "scripts", "npm", "build-platform-packages.mjs")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	args := append([]string{script, "--artifacts-dir", artifactsDir, "--out-dir", outDir, "--only", "linux-x64"}, extraArgs...)
	command := exec.CommandContext(ctx, node, args...)
	command.Env = append(os.Environ(), "NODE_OPTIONS=")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build-platform-packages failed: %v\n%s", err, output)
	}
}

// The final release under the pre-rename name is the same artifacts assembled
// with --package-name. Every place a payload names its package must follow it:
// a wrapper that kept the current name in one alias would resolve a package
// npm never installed.
func TestBuildPlatformPackagesAssemblesUnderAnotherPackageName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses POSIX symlinks and shell scripts")
	}
	node := requireNode(t)
	tar := requireCommand(t, "tar")
	version := packageVersion(t)
	root := writePlatformArchiveFixture(t, tar, version)
	outDir := filepath.Join(root, "out")
	runBuildPlatformPackages(t, node, root, outDir, "--package-name", "@gitlawb/zero")

	platformManifest := filepath.Join(outDir, "platforms", "zero-linux-x64", "package.json")
	var platformPkg struct {
		Name        string `json:"name"`
		Version     string `json:"version"`
		Description string `json:"description"`
	}
	unmarshalJSONFile(t, platformManifest, &platformPkg)
	if platformPkg.Name != "@gitlawb/zero" || platformPkg.Version != version+"-linux-x64" {
		t.Fatalf("platform package = %s@%s, want @gitlawb/zero@%s-linux-x64", platformPkg.Name, platformPkg.Version, version)
	}

	wrapperManifest := filepath.Join(outDir, "wrapper", "package.json")
	var wrapperPkg struct {
		Name       string            `json:"name"`
		Version    string            `json:"version"`
		Optional   map[string]string `json:"optionalDependencies"`
		Repository struct {
			URL string `json:"url"`
		} `json:"repository"`
	}
	unmarshalJSONFile(t, wrapperManifest, &wrapperPkg)
	if wrapperPkg.Name != "@gitlawb/zero" || wrapperPkg.Version != version {
		t.Fatalf("wrapper = %s@%s, want @gitlawb/zero@%s", wrapperPkg.Name, wrapperPkg.Version, version)
	}
	if len(wrapperPkg.Optional) != 5 {
		t.Fatalf("wrapper optionalDependencies = %v, want the full five-platform matrix", wrapperPkg.Optional)
	}
	for alias, spec := range wrapperPkg.Optional {
		platform := strings.TrimPrefix(alias, "@gitlawb/zero-")
		if platform == alias || spec != "npm:@gitlawb/zero@"+version+"-"+platform {
			t.Fatalf("optionalDependencies[%q] = %q, want npm:@gitlawb/zero@%s-<platform> under an @gitlawb/zero-<platform> alias", alias, spec, version)
		}
	}
	// Provenance is checked against the repository the workflow runs in, which
	// is the same for both names.
	if !strings.Contains(wrapperPkg.Repository.URL, "github.com/Twigpine/zero") {
		t.Fatalf("wrapper repository = %q, want the Twigpine/zero repository npm checks provenance against", wrapperPkg.Repository.URL)
	}
	for _, manifest := range []string{platformManifest, wrapperManifest} {
		data, err := os.ReadFile(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "@twigpine/zero") {
			t.Fatalf("%s still names @twigpine/zero:\n%s", manifest, data)
		}
	}
}

func TestBuildPlatformPackagesRejectsAnInvalidPackageName(t *testing.T) {
	node := requireNode(t)
	script := filepath.Join(repoRoot(t), "scripts", "npm", "build-platform-packages.mjs")
	ctx, cancel := context.WithTimeout(context.Background(), nodeWrapperTimeout())
	defer cancel()
	command := exec.CommandContext(ctx, node, script, "--artifacts-dir", t.TempDir(), "--package-name", "../zero")
	command.Env = append(os.Environ(), "NODE_OPTIONS=")
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("build-platform-packages accepted --package-name ../zero:\n%s", output)
	}
	if !strings.Contains(string(output), "is not an npm package name") {
		t.Fatalf("output does not explain the rejection: %s", output)
	}
}

func unmarshalJSONFile(t *testing.T, path string, target any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatalf("Unmarshal %s: %v", path, err)
	}
}

func writeFixtureFile(t *testing.T, path string, content string, mode os.FileMode) {
	t.Helper()
	mustMkdirAllFixture(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
	if mode&0o111 != 0 {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatalf("Chmod %s: %v", path, err)
		}
	}
}

func mustMkdirAllFixture(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", path, err)
	}
}

func requireCommand(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s is not available", name)
	}
	return path
}
