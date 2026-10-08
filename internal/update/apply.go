package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/release"
)

// ErrTargetPossiblyTampered reports that an executable path may hold content
// this updater could not verify, because a promotion failed and the original
// could not be restored over it. Only the Windows promotion path produces it
// today (see replace_windows.go for why that platform has the gap and the POSIX
// descriptor-bound path does not), but it is declared here so callers on every
// platform can test for it without build tags — the helper-refresh loop below
// must fail the apply on it rather than downgrade it to a warning.
var ErrTargetPossiblyTampered = errors.New("target executable path may hold unverified content after a failed update")

// DefaultDownloadTimeout bounds the archive/checksum download phase of a
// standalone Apply, separately from Options.Timeout (which only covers the
// small release-metadata check), so a stalled connection can't hang forever.
const DefaultDownloadTimeout = 5 * time.Minute

// ApplyResult reports the outcome of Apply.
type ApplyResult struct {
	Result
	Applied       bool          `json:"applied"`
	InstallMethod InstallMethod `json:"installMethod,omitempty"`
	BinaryPath    string        `json:"binaryPath,omitempty"`
	Message       string        `json:"message,omitempty"`
	Warnings      []string      `json:"warnings,omitempty"`
}

// windowsOptionalBinaries/linuxOptionalBinaries mirror the helper binary
// names scripts/postinstall.mjs copies alongside the main binary when
// present, so `zero upgrade` refreshes them too instead of leaving them stale.
var (
	windowsOptionalBinaries = []string{"zero-windows-command-runner.exe", "zero-windows-sandbox-setup.exe"}
	linuxOptionalBinaries   = []string{"zero-linux-sandbox", "zero-seccomp"}
	currentExecutable       = os.Executable
)

// Apply checks for an update and, if one is available, installs it: via
// `npm install -g` for npm-managed installs, or by downloading, verifying,
// and atomically replacing the binary for standalone installs.
func Apply(ctx context.Context, options Options) (ApplyResult, error) {
	checkResult, err := Check(ctx, options)
	if err != nil {
		return ApplyResult{}, err
	}

	executablePath, err := currentExecutable()
	if err != nil {
		return ApplyResult{}, fmt.Errorf("resolve current executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(executablePath); err == nil {
		executablePath = resolved
	}
	method := DetectInstallMethod(executablePath)
	// Only the standalone path rewrites the binary in place, so only it needs the
	// recovery state. npm and Homebrew hand the work to the package manager.
	if method == InstallMethodStandalone {
		if err := preflightRecoveryState(executablePath); err != nil {
			return ApplyResult{}, err
		}
	}
	if !checkResult.UpdateAvailable {
		return ApplyResult{Result: checkResult, Message: "already up to date"}, nil
	}

	switch method {
	case InstallMethodHomebrew:
		// Refused rather than performed. Running `brew upgrade` for the user would
		// touch a package manager they own, and writing the binary directly is
		// worse: the keg is what Homebrew's own records describe, so a self-update
		// leaves brew reporting a version that is no longer installed, and the next
		// `brew upgrade` or `brew reinstall` silently reverts it.
		return ApplyResult{}, fmt.Errorf(
			"this zero was installed with Homebrew (%s); run `brew upgrade zero` instead, so Homebrew's records match the binary on disk",
			executablePath,
		)
	case InstallMethodNpm:
		if err := applyNpmUpdate(ctx); err != nil {
			return ApplyResult{}, err
		}
		return ApplyResult{
			Result:        checkResult,
			Applied:       true,
			InstallMethod: method,
			BinaryPath:    executablePath,
			Message:       fmt.Sprintf("updated via npm to %s", checkResult.LatestVersion),
		}, nil
	default:
		warnings, err := applyStandaloneUpdate(ctx, checkResult, executablePath)
		if err != nil {
			return ApplyResult{}, err
		}
		return ApplyResult{
			Result:        checkResult,
			Applied:       true,
			InstallMethod: method,
			BinaryPath:    executablePath,
			Message:       fmt.Sprintf("updated to %s", checkResult.LatestVersion),
			Warnings:      warnings,
		}, nil
	}
}

// FormatApply renders an ApplyResult as human-readable text.
func FormatApply(result ApplyResult) string {
	if !result.Applied {
		return Format(result.Result)
	}
	lines := []string{
		fmt.Sprintf("[zero] %s (%s -> %s)", result.Message, result.CurrentVersion, result.LatestVersion),
		"Binary: " + result.BinaryPath,
	}
	for _, warning := range result.Warnings {
		lines = append(lines, "Warning: "+warning)
	}
	return strings.Join(lines, "\n")
}

func applyNpmUpdate(ctx context.Context) error {
	npmPath, err := exec.LookPath("npm")
	if err != nil {
		return fmt.Errorf("npm not found on PATH: reinstall with `npm install -g %s@latest`", npmPackageName)
	}
	command := exec.CommandContext(ctx, npmPath, "install", "-g", npmPackageName+"@latest")
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("npm install -g %s@latest: %w", npmPackageName, err)
	}
	return nil
}

func applyStandaloneUpdate(ctx context.Context, result Result, executablePath string) ([]string, error) {
	asset := result.ReleaseAsset
	if !asset.Verified {
		return nil, fmt.Errorf("release asset for %s-%s could not be verified", asset.Platform, asset.Arch)
	}

	tempDir, err := os.MkdirTemp("", "zero-update-*")
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = os.RemoveAll(tempDir)
	}()

	downloadCtx, cancel := context.WithTimeout(ctx, DefaultDownloadTimeout)
	defer cancel()

	archivePath := filepath.Join(tempDir, asset.ArchiveName)
	if err := downloadFile(downloadCtx, asset.ArchiveURL, archivePath); err != nil {
		return nil, fmt.Errorf("download release archive: %w", err)
	}
	checksumPath := filepath.Join(tempDir, asset.ChecksumName)
	if err := downloadFile(downloadCtx, asset.ChecksumURL, checksumPath); err != nil {
		return nil, fmt.Errorf("download release checksum: %w", err)
	}
	if err := verifyArchiveChecksum(checksumPath, asset.ArchiveName); err != nil {
		return nil, err
	}

	extractDir := filepath.Join(tempDir, "extracted")
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		return nil, err
	}
	if err := extractArchive(archivePath, extractDir); err != nil {
		return nil, fmt.Errorf("extract release archive: %w", err)
	}

	binaryName := "zero"
	optionalBinaries := linuxOptionalBinaries
	switch runtime.GOOS {
	case "windows":
		binaryName = "zero.exe"
		optionalBinaries = windowsOptionalBinaries
	case "darwin":
		optionalBinaries = nil
	}

	newBinaryPath, err := findByBasename(extractDir, binaryName)
	if err != nil {
		return nil, err
	}
	if newBinaryPath == "" {
		return nil, fmt.Errorf("release archive did not contain %s", binaryName)
	}

	targetDir := filepath.Dir(executablePath)
	var warnings []string
	for _, name := range optionalBinaries {
		source, err := findByBasename(extractDir, name)
		if err != nil || source == "" {
			continue // optional: the sandbox degrades gracefully without it
		}
		destPath := filepath.Join(targetDir, name)
		if _, err := os.Stat(destPath); err != nil {
			continue // only refresh helpers this install already has
		}
		if err := installBinary(source, destPath); err != nil {
			// An optional helper that simply fails to refresh is a warning: the
			// sandbox degrades gracefully without a newer one, and aborting the
			// whole update over it would be worse than the stale helper.
			//
			// Possible tampering is not that. It means this helper's path may now
			// hold content the updater could not verify, and helpers are resolved
			// from the install directory and EXECUTED by the sandbox runner — so
			// reporting Applied: true and exiting 0 would hand the operator a
			// success while a sibling executable is suspect. Fail the apply and
			// keep errors.Is intact, exactly as the main binary does.
			if errors.Is(err, ErrTargetPossiblyTampered) {
				return nil, fmt.Errorf("update helper %s: %w", name, err)
			}
			warnings = append(warnings, fmt.Sprintf("failed to update helper %s: %v", name, err))
		}
	}
	// Refresh helpers before the main executable. If a helper path may have
	// been tampered with, the running main binary must remain on the old version
	// so the next invocation still sees this release as available and retries
	// the helper instead of returning "already up to date".
	if err := installBinary(newBinaryPath, executablePath); err != nil {
		return nil, err
	}

	return warnings, nil
}

// verifyArchiveChecksum verifies the checksum file at checksumPath and
// cross-checks it names expectedArchiveName. VerifySHA256Checksum hashes
// whichever file the checksum text names, not necessarily the archive we
// downloaded — this mirrors the cross-check VerifyReleaseChecksums already
// does, so a checksum file that names a different file can't be used to
// verify (and thereby vouch for) the wrong bytes before extraction.
func verifyArchiveChecksum(checksumPath string, expectedArchiveName string) error {
	verified, err := release.VerifySHA256Checksum(checksumPath)
	if err != nil {
		return fmt.Errorf("verify release checksum: %w", err)
	}
	if verified.ArchiveName != expectedArchiveName {
		return fmt.Errorf("checksum file references %s, expected %s", verified.ArchiveName, expectedArchiveName)
	}
	return nil
}

// installBinary stages sourcePath next to targetPath (same filesystem, so the
// final rename is atomic) and then swaps it into place.
func installBinary(sourcePath string, targetPath string) error {
	staged, err := stageBinary(sourcePath, targetPath)
	if err != nil {
		return fmt.Errorf("stage %s: %w", filepath.Base(targetPath), err)
	}
	// Registered before the promotion attempt and reached on every failure path,
	// including a mid-write copy error: each attempt now stages under a fresh
	// random name, so a leaked partial file is never reused by the next attempt
	// and would otherwise accumulate release-sized garbage in the install
	// directory. Unverifiable leftovers from a hard crash are preserved rather
	// than removed based only on their public filename shape.
	defer staged.discard()
	if err := staged.promote(targetPath); err != nil {
		return fmt.Errorf("install %s: %w", filepath.Base(targetPath), err)
	}
	return nil
}

// stagedBinary is a freshly created staging object holding the verified release
// bytes, together with the handle it was created through. Promotion is bound to
// that object rather than to the staging PATHNAME: under the threat model of
// #742 a lower-privileged principal that can write in the installation
// directory may replace a sibling entry, so re-resolving the staging path at
// swap time would let it substitute its own file into the executable path after
// the verified bytes were written. Each platform closes that handoff with its
// own primitive — see the promote implementations.
type stagedBinary struct {
	file *os.File
	path string
	// dir is a private staging directory that must be removed with the file
	// (POSIX). Empty on Windows, which binds the swap to the handle instead.
	dir string
	// dirHandle is an open descriptor on dir (POSIX only), bound to that
	// directory's inode rather than its current pathname. promote renames the
	// staged file through it (see stage_other.go) so a principal who can write
	// in the installation directory cannot redirect the final rename by
	// renaming dir aside and recreating a look-alike directory at the same
	// path between the identity check and the rename — a pathname lookup at
	// that point would resolve through the impostor instead. nil on Windows.
	dirHandle *os.File
	// parentHandle is the descriptor-bound parent of dir (POSIX only).
	parentHandle *os.File
	// promoted records that path now IS the installed binary, so discard must
	// not delete it.
	promoted bool
}

// stageBinary creates the staging object for targetPath and writes sourcePath's
// bytes into it through the handle it was created with, leaving that handle open
// for promote.
//
// It is a package var so a test can force a staging failure on every platform:
// the staging location is created exclusively under a name that cannot be known
// in advance, which is exactly what stops a test (like an attacker) from
// occupying it from outside.
var stageBinary = func(sourcePath string, targetPath string) (*stagedBinary, error) {
	staged, err := createStagedBinary(targetPath)
	if err != nil {
		return nil, err
	}
	if err := staged.copyFrom(sourcePath); err != nil {
		staged.discard()
		return nil, err
	}
	return staged, nil
}

// copyFrom writes sourcePath into the staged file through the handle
// createStagedBinary opened. It never reopens by pathname, so the bytes cannot
// land anywhere other than the object that was exclusively created.
func (staged *stagedBinary) copyFrom(sourcePath string) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer func() {
		_ = source.Close()
	}()
	if _, err := io.Copy(staged.file, source); err != nil {
		return err
	}
	return staged.file.Sync()
}

// discard closes the handle(s) and removes what it created, unless the object
// was already promoted into the executable path.
func (staged *stagedBinary) discard() {
	if staged == nil {
		return
	}
	// Removal is bound to the staged object before the handle is released
	// wherever the platform allows it (Windows, via delete-on-close). Once the
	// handle is gone the staging pathname can be re-resolved, and under the
	// writable-install-directory threat model that entry may by then name a file
	// this updater never created.
	staged.discardOpenObject()
	if staged.file != nil {
		_ = staged.file.Close()
	}
	staged.discardPaths()
}

func downloadFile(ctx context.Context, url string, destPath string) error {
	if url == "" {
		return fmt.Errorf("missing download URL")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "zero/update")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer func() {
		_ = response.Body.Close()
	}()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("download failed (%s): %s", response.Status, url)
	}
	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, response.Body)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}
