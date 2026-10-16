package installtest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestUnixInstallerScriptMatchesReleaseContracts(t *testing.T) {
	script := readRepoText(t, "scripts/install.sh")

	if !strings.HasPrefix(script, "#!/usr/bin/env bash") {
		t.Fatalf("install.sh shebang = %q, want bash", strings.SplitN(script, "\n", 2)[0])
	}
	containsAll(t, script, []string{
		"set -euo pipefail",
		`ZERO_REPO="${ZERO_REPO:-Twigpine/zero}"`,
		`ZERO_INSTALL_DIR="${ZERO_INSTALL_DIR:-$HOME/.local/bin}"`,
		`archive_name="zero-v${version}-${platform}-${arch}.tar.gz"`,
		`checksum_name="${archive_name}.sha256"`,
		"curl --fail --location --show-error --silent --header 'Accept: application/vnd.github+json'",
		`verify_checksum "$checksum_name"`,
		`tar -xzf "$archive_path" -C "$extract_dir"`,
		`find_extracted_binary "$extract_dir"`,
		`cp "$binary_path" "$ZERO_INSTALL_DIR/zero"`,
		`copy_optional_file "zero-linux-sandbox"`,
		`copy_optional_file "zero-seccomp"`,
		`copy_optional_dir "helpers"`,
	})
}

func TestPowerShellInstallerScriptMatchesWindowsReleaseContracts(t *testing.T) {
	script := readRepoText(t, "scripts/install.ps1")

	containsAll(t, script, []string{
		`[string]$Repository = $(if ($env:ZERO_REPO)`,
		`Join-Path $env:LOCALAPPDATA "zero\bin"`,
		`$archiveName = "zero-v$releaseVersion-windows-$arch.zip"`,
		`$checksumName = "$archiveName.sha256"`,
		`Get-FileHash -Path $archivePath -Algorithm SHA256`,
		`Expand-Archive -Path $archivePath -DestinationPath $extractDir -Force`,
		`Find-ZeroExtractedFile -Root $extractDir -FileName $fileName`,
		`"zero-windows-command-runner.exe"`,
		`"zero-windows-sandbox-setup.exe"`,
		`Copy-Item -Path $sourcePath -Destination (Join-Path $InstallDir $fileName) -Force`,
		`Find-ZeroOptionalExtractedDirectory -Root $extractDir -DirectoryName "helpers"`,
		`Copy-Item -Path $helpersPath -Destination $targetHelpersPath -Recurse -Force`,
	})
}

func TestUnixInstallerInstallsFromPrefixedReleaseArchiveWithoutNetwork(t *testing.T) {
	fixture := newUnixInstallFixture(t)
	stdout, stderr, err := runUnixInstaller(t, fixture)
	if err != nil {
		t.Fatalf("install.sh failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if got := strings.TrimSpace(stderr); got != "" {
		t.Fatalf("install.sh stderr = %q, want empty", got)
	}
	if !strings.Contains(stdout, "Installed "+filepath.Join(fixture.installDir, "zero")) {
		t.Fatalf("install.sh stdout missing install path:\n%s", stdout)
	}

	installed := readFile(t, filepath.Join(fixture.installDir, "zero"))
	if !strings.Contains(string(installed), "mock-zero") {
		t.Fatalf("installed binary = %q, want mock-zero script", string(installed))
	}
	if _, err := os.Stat(filepath.Join(fixture.installDir, "helpers", "node_modules", ".bin", "agent-browser")); err != nil {
		t.Fatalf("installed helper package missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fixture.installDir, "zero-linux-sandbox")); err != nil {
		t.Fatalf("installed linux sandbox helper missing: %v", err)
	}
}

func TestUnixInstallerRejectsChecksumMismatchWithoutNetwork(t *testing.T) {
	fixture := newUnixInstallFixture(t)
	writeFile(t, fixture.checksumPath, []byte(strings.Repeat("0", 64)+"  "+fixture.archiveName+"\n"), 0o644)

	stdout, stderr, err := runUnixInstaller(t, fixture)
	if err == nil {
		t.Fatalf("install.sh succeeded with a bad checksum\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	output := stdout + "\n" + stderr
	if !strings.Contains(output, fixture.archiveName) {
		t.Fatalf("checksum failure output missing archive name:\n%s", output)
	}
	if !strings.Contains(output, "FAILED") || !strings.Contains(strings.ToLower(output), "checksum") {
		t.Fatalf("checksum failure output missing checksum mismatch detail:\n%s", output)
	}
	if _, err := os.Stat(filepath.Join(fixture.installDir, "zero")); !os.IsNotExist(err) {
		t.Fatalf("installed binary exists after checksum failure: %v", err)
	}
}

type unixInstallFixture struct {
	bash         string
	mockBin      string
	installDir   string
	archiveName  string
	archivePath  string
	checksumPath string
}

func newUnixInstallFixture(t *testing.T) unixInstallFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix installer smoke does not run on Windows")
	}
	bash := requireCommand(t, "bash")
	tar := requireCommand(t, "tar")

	releasePlatform := unixReleasePlatform(t)
	releaseArch := unixReleaseArch(t)
	packageName := fmt.Sprintf("zero-v0.1.0-%s-%s", releasePlatform, releaseArch)
	archiveName := packageName + ".tar.gz"
	checksumName := archiveName + ".sha256"
	root := t.TempDir()
	fixture := unixInstallFixture{
		bash:         bash,
		mockBin:      filepath.Join(root, "bin"),
		installDir:   filepath.Join(root, "install"),
		archiveName:  archiveName,
		archivePath:  filepath.Join(root, "release", archiveName),
		checksumPath: filepath.Join(root, "release", checksumName),
	}
	packageDir := filepath.Join(root, "package", packageName)

	mustMkdirAll(t, fixture.mockBin)
	mustMkdirAll(t, packageDir)
	mustMkdirAll(t, filepath.Dir(fixture.archivePath))
	mustMkdirAll(t, fixture.installDir)
	mustMkdirAll(t, filepath.Join(packageDir, "helpers", "node_modules", ".bin"))
	writeFile(t, filepath.Join(packageDir, "zero"), []byte("#!/usr/bin/env sh\necho mock-zero\n"), 0o755)
	writeFile(t, filepath.Join(packageDir, "zero-linux-sandbox"), []byte("#!/usr/bin/env sh\n"), 0o755)
	writeFile(t, filepath.Join(packageDir, "zero-seccomp"), []byte("#!/usr/bin/env sh\n"), 0o755)
	mustMkdirAll(t, filepath.Join(packageDir, "helpers", "node_modules", "agent-browser", "bin"))
	writeFile(t, filepath.Join(packageDir, "helpers", "node_modules", "agent-browser", "bin", "agent-browser.js"), []byte("#!/usr/bin/env node\n"), 0o755)
	if err := os.Symlink("../agent-browser/bin/agent-browser.js", filepath.Join(packageDir, "helpers", "node_modules", ".bin", "agent-browser")); err != nil {
		t.Fatalf("Symlink agent-browser helper: %v", err)
	}
	writeFile(t, filepath.Join(packageDir, "helpers", "node_modules", ".bin", "tuistory"), []byte("#!/usr/bin/env sh\n"), 0o755)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tarCommand := exec.CommandContext(ctx, tar, "-C", filepath.Join(root, "package"), "-czf", fixture.archivePath, packageName)
	if output, err := tarCommand.CombinedOutput(); err != nil {
		t.Fatalf("tar failed: %v\n%s", err, output)
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("tar timed out: %v", err)
	}

	archiveBytes := readFile(t, fixture.archivePath)
	sum := sha256.Sum256(archiveBytes)
	writeFile(t, fixture.checksumPath, []byte(fmt.Sprintf("%x  %s\n", sum, archiveName)), 0o644)

	mockCurl := fmt.Sprintf(`#!/usr/bin/env sh
set -eu
output=""
url=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output)
      output="$2"
      shift 2
      ;;
    --output=*)
      output="${1#--output=}"
      shift
      ;;
    --header)
      shift 2
      ;;
    --header=*)
      shift
      ;;
    --fail|--location|--show-error|--silent)
      shift
      ;;
    *)
      url="$1"
      shift
      ;;
  esac
done

case "$url" in
  */%s)
    cp %s "$output"
    ;;
  */%s)
    cp %s "$output"
    ;;
  *)
    echo "unexpected url: $url" >&2
    exit 2
    ;;
esac
`, archiveName, shellQuote(fixture.archivePath), checksumName, shellQuote(fixture.checksumPath))
	writeFile(t, filepath.Join(fixture.mockBin, "curl"), []byte(mockCurl), 0o755)

	return fixture
}

func runUnixInstaller(t *testing.T, fixture unixInstallFixture) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, fixture.bash, filepath.Join(repoRoot(t), "scripts/install.sh"), "--version", "0.1.0", "--install-dir", fixture.installDir)
	command.Dir = repoRoot(t)
	command.Env = append(os.Environ(),
		"PATH="+fixture.mockBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"ZERO_GITHUB_BASE_URL=https://example.test",
		"ZERO_REPO=Twigpine/zero",
	)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr

	runErr := command.Run()
	if err := ctx.Err(); err != nil {
		t.Fatalf("install.sh timed out: %v", err)
	}
	return stdout.String(), stderr.String(), runErr
}

func unixReleasePlatform(t *testing.T) string {
	t.Helper()
	switch runtime.GOOS {
	case "linux":
		return "linux"
	case "darwin":
		return "macos"
	default:
		t.Skipf("Unix installer smoke does not support GOOS=%s", runtime.GOOS)
		return ""
	}
}

func unixReleaseArch(t *testing.T) string {
	t.Helper()
	switch runtime.GOARCH {
	case "amd64", "x86_64":
		return "x64"
	case "arm64":
		return "arm64"
	default:
		t.Skipf("Unix installer smoke does not support GOARCH=%s", runtime.GOARCH)
		return ""
	}
}

func containsAll(t *testing.T, value string, wants []string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(value, want) {
			t.Fatalf("expected content to contain %q", want)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func readRepoText(t *testing.T, relativePath string) string {
	t.Helper()
	return string(readFile(t, filepath.Join(repoRoot(t), relativePath)))
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	return data
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", path, err)
	}
}

func writeFile(t *testing.T, path string, content []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
	if mode&0o111 != 0 {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatalf("Chmod %s: %v", path, err)
		}
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

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
