package update

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/release"
)

func TestApplyReturnsNoopWhenUpToDate(t *testing.T) {
	result, err := Apply(context.Background(), Options{
		CurrentVersion: "0.2.0",
		GOOS:           "linux",
		GOARCH:         "amd64",
		Fetch: func(_ context.Context, endpoint string) (Release, error) {
			return releaseForTarget(t, "v0.2.0", "linux", "amd64"), nil
		},
	})
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if result.Applied {
		t.Fatalf("expected Applied=false when already up to date, got %#v", result)
	}
}

func TestApplyStandaloneUpdateReplacesBinary(t *testing.T) {
	binaryName := "zero"
	// macOS ships no optional helper binaries (matching scripts/postinstall.mjs),
	// so there's nothing to refresh there; only linux/windows have one to check.
	optionalName := ""
	switch runtime.GOOS {
	case "windows":
		binaryName = "zero.exe"
		optionalName = "zero-windows-command-runner.exe"
	case "linux":
		optionalName = "zero-seccomp"
	}

	installDir := t.TempDir()
	executablePath := filepath.Join(installDir, binaryName)
	if err := os.WriteFile(executablePath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile executable: %v", err)
	}
	// Only pre-existing optional helpers should be refreshed; an absent one
	// (e.g. the platform's other optional helper) must not be introduced.
	var existingHelperPath string
	if optionalName != "" {
		existingHelperPath = filepath.Join(installDir, optionalName)
		if err := os.WriteFile(existingHelperPath, []byte("old-helper"), 0o755); err != nil {
			t.Fatalf("WriteFile helper: %v", err)
		}
	}

	archiveName := "zero-v0.2.0-linux-x64.tar.gz"
	archiveDir := t.TempDir()
	archivePath := filepath.Join(archiveDir, archiveName)
	writeTestTarGz(t, archivePath, map[string]string{
		"zero":                            "new-binary",
		"zero.exe":                        "new-binary-exe",
		"zero-seccomp":                    "new-helper",
		"zero-windows-command-runner.exe": "new-helper-exe",
	})
	checksum, err := release.SHA256File(archivePath)
	if err != nil {
		t.Fatalf("SHA256File: %v", err)
	}
	checksumText, err := release.FormatSHA256Checksum(checksum, archiveName)
	if err != nil {
		t.Fatalf("FormatSHA256Checksum: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + archiveName:
			http.ServeFile(w, r, archivePath)
		case "/" + archiveName + ".sha256":
			_, _ = w.Write([]byte(checksumText))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	result := Result{
		LatestVersion: "0.2.0",
		ReleaseAsset: AssetCheck{
			Platform:      "linux",
			Arch:          "x64",
			ArchiveName:   archiveName,
			ArchiveURL:    server.URL + "/" + archiveName,
			ChecksumName:  archiveName + ".sha256",
			ChecksumURL:   server.URL + "/" + archiveName + ".sha256",
			ArchiveFound:  true,
			ChecksumFound: true,
			Verified:      true,
		},
	}

	if warnings, err := applyStandaloneUpdate(context.Background(), result, executablePath); err != nil {
		t.Fatalf("applyStandaloneUpdate returned error: %v", err)
	} else if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}

	data, err := os.ReadFile(executablePath)
	if err != nil {
		t.Fatalf("ReadFile executable: %v", err)
	}
	wantBinary := "new-binary"
	if runtime.GOOS == "windows" {
		wantBinary = "new-binary-exe"
	}
	if string(data) != wantBinary {
		t.Fatalf("executable content = %q, want %q", data, wantBinary)
	}

	if optionalName != "" {
		helperData, err := os.ReadFile(existingHelperPath)
		if err != nil {
			t.Fatalf("ReadFile helper: %v", err)
		}
		wantHelper := "new-helper"
		if runtime.GOOS == "windows" {
			wantHelper = "new-helper-exe"
		}
		if string(helperData) != wantHelper {
			t.Fatalf("helper content = %q, want %q", helperData, wantHelper)
		}
	}

	if entries, err := os.ReadDir(installDir); err == nil {
		for _, entry := range entries {
			name := entry.Name()
			// On Windows, replacement leaves a namespaced recovery copy of each
			// replaced binary for identity-bound cleanup by a later update.
			windowsRecovery := runtime.GOOS == "windows" && strings.HasSuffix(name, ".old") &&
				(strings.HasPrefix(name, binaryName+".zero-update-") ||
					optionalName != "" && strings.HasPrefix(name, optionalName+".zero-update-"))
			if name == binaryName || (optionalName != "" && name == optionalName) || windowsRecovery {
				continue
			}
			t.Fatalf("unexpected extra file left in install dir: %s", name)
		}
	}
}

func TestApplyStandaloneUpdateWarnsWhenHelperRefreshFails(t *testing.T) {
	binaryName := "zero"
	optionalName := "zero-seccomp"
	switch runtime.GOOS {
	case "windows":
		binaryName = "zero.exe"
		optionalName = "zero-windows-command-runner.exe"
	case "darwin":
		t.Skip("macOS ships no optional helper binaries to refresh")
	}

	installDir := t.TempDir()
	executablePath := filepath.Join(installDir, binaryName)
	if err := os.WriteFile(executablePath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile executable: %v", err)
	}
	// The helper must already exist for a refresh to be attempted at all.
	existingHelperPath := filepath.Join(installDir, optionalName)
	if err := os.WriteFile(existingHelperPath, []byte("old-helper"), 0o755); err != nil {
		t.Fatalf("WriteFile helper: %v", err)
	}
	// Force the helper's staging to fail. It cannot be provoked from outside any
	// more: the staging location is created exclusively under a name (POSIX: a
	// private directory) that nothing else can predict or occupy, which is the
	// point of the fix. So fail it through the seam instead.
	stubStageBinaryFailure(t, existingHelperPath, errors.New("staging is unavailable in this test"))

	archiveName := "zero-v0.2.0-linux-x64.tar.gz"
	archiveDir := t.TempDir()
	archivePath := filepath.Join(archiveDir, archiveName)
	writeTestTarGz(t, archivePath, map[string]string{
		"zero":                            "new-binary",
		"zero.exe":                        "new-binary-exe",
		"zero-seccomp":                    "new-helper",
		"zero-windows-command-runner.exe": "new-helper-exe",
	})
	checksum, err := release.SHA256File(archivePath)
	if err != nil {
		t.Fatalf("SHA256File: %v", err)
	}
	checksumText, err := release.FormatSHA256Checksum(checksum, archiveName)
	if err != nil {
		t.Fatalf("FormatSHA256Checksum: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + archiveName:
			http.ServeFile(w, r, archivePath)
		case "/" + archiveName + ".sha256":
			_, _ = w.Write([]byte(checksumText))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	result := Result{
		LatestVersion: "0.2.0",
		ReleaseAsset: AssetCheck{
			Platform:      "linux",
			Arch:          "x64",
			ArchiveName:   archiveName,
			ArchiveURL:    server.URL + "/" + archiveName,
			ChecksumName:  archiveName + ".sha256",
			ChecksumURL:   server.URL + "/" + archiveName + ".sha256",
			ArchiveFound:  true,
			ChecksumFound: true,
			Verified:      true,
		},
	}

	warnings, err := applyStandaloneUpdate(context.Background(), result, executablePath)
	if err != nil {
		t.Fatalf("applyStandaloneUpdate returned error: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], optionalName) {
		t.Fatalf("expected one warning mentioning %s, got %v", optionalName, warnings)
	}

	data, err := os.ReadFile(executablePath)
	if err != nil {
		t.Fatalf("ReadFile executable: %v", err)
	}
	wantBinary := "new-binary"
	if runtime.GOOS == "windows" {
		wantBinary = "new-binary-exe"
	}
	if string(data) != wantBinary {
		t.Fatalf("main binary should still be updated despite helper failure: got %q, want %q", data, wantBinary)
	}
}

// TestApplyStandaloneUpdateFailsWhenHelperRefreshReportsTampering covers
// jatmn's #751 P2. An optional helper that merely fails to refresh stays a
// warning — the sandbox degrades fine on a stale helper. Possible tampering is
// different in kind: the helper's path may now hold content the updater could
// not verify, and helpers are resolved from the install directory and executed
// by the sandbox runner, so reporting Applied: true and exiting 0 would hand
// the operator a success while a sibling executable is suspect.
func TestApplyStandaloneUpdateFailsWhenHelperRefreshReportsTampering(t *testing.T) {
	binaryName := "zero"
	optionalName := "zero-seccomp"
	switch runtime.GOOS {
	case "windows":
		binaryName = "zero.exe"
		optionalName = "zero-windows-command-runner.exe"
	case "darwin":
		t.Skip("macOS ships no optional helper binaries to refresh")
	}

	installDir := t.TempDir()
	executablePath := filepath.Join(installDir, binaryName)
	if err := os.WriteFile(executablePath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile executable: %v", err)
	}
	existingHelperPath := filepath.Join(installDir, optionalName)
	if err := os.WriteFile(existingHelperPath, []byte("old-helper"), 0o755); err != nil {
		t.Fatalf("WriteFile helper: %v", err)
	}
	stubStageBinaryFailure(t, existingHelperPath, fmt.Errorf("promote: %w", ErrTargetPossiblyTampered))

	archiveName := "zero-v0.2.0-linux-x64.tar.gz"
	archiveDir := t.TempDir()
	archivePath := filepath.Join(archiveDir, archiveName)
	writeTestTarGz(t, archivePath, map[string]string{
		"zero":                            "new-binary",
		"zero.exe":                        "new-binary-exe",
		"zero-seccomp":                    "new-helper",
		"zero-windows-command-runner.exe": "new-helper-exe",
	})
	checksum, err := release.SHA256File(archivePath)
	if err != nil {
		t.Fatalf("SHA256File: %v", err)
	}
	checksumText, err := release.FormatSHA256Checksum(checksum, archiveName)
	if err != nil {
		t.Fatalf("FormatSHA256Checksum: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + archiveName:
			http.ServeFile(w, r, archivePath)
		case "/" + archiveName + ".sha256":
			_, _ = w.Write([]byte(checksumText))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	result := Result{
		LatestVersion: "0.2.0",
		ReleaseAsset: AssetCheck{
			Platform:      "linux",
			Arch:          "x64",
			ArchiveName:   archiveName,
			ArchiveURL:    server.URL + "/" + archiveName,
			ChecksumName:  archiveName + ".sha256",
			ChecksumURL:   server.URL + "/" + archiveName + ".sha256",
			ArchiveFound:  true,
			ChecksumFound: true,
			Verified:      true,
		},
	}

	warnings, err := applyStandaloneUpdate(context.Background(), result, executablePath)
	if !errors.Is(err, ErrTargetPossiblyTampered) {
		t.Fatalf("applyStandaloneUpdate error = %v, want it to wrap ErrTargetPossiblyTampered", err)
	}
	if !strings.Contains(err.Error(), optionalName) {
		t.Fatalf("error = %v, want it to name the helper %q", err, optionalName)
	}
	if len(warnings) != 0 {
		t.Fatalf("tampering must not be reported as a warning: %v", warnings)
	}
	mainData, readErr := os.ReadFile(executablePath)
	if readErr != nil {
		t.Fatalf("ReadFile main binary: %v", readErr)
	}
	if string(mainData) != "old-binary" {
		t.Fatalf("main binary = %q, want old-binary so a retry still sees the update", mainData)
	}
}

func TestApplyStandaloneUpdateRejectsChecksumMismatch(t *testing.T) {
	binaryName := "zero"
	if runtime.GOOS == "windows" {
		binaryName = "zero.exe"
	}

	installDir := t.TempDir()
	executablePath := filepath.Join(installDir, binaryName)
	if err := os.WriteFile(executablePath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile executable: %v", err)
	}

	archiveName := "zero-v0.2.0-linux-x64.tar.gz"
	archiveDir := t.TempDir()
	archivePath := filepath.Join(archiveDir, archiveName)
	writeTestTarGz(t, archivePath, map[string]string{"zero": "new-binary", "zero.exe": "new-binary-exe"})

	badChecksumText, err := release.FormatSHA256Checksum("0000000000000000000000000000000000000000000000000000000000000000"[:64], archiveName)
	if err != nil {
		t.Fatalf("FormatSHA256Checksum: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + archiveName:
			http.ServeFile(w, r, archivePath)
		case "/" + archiveName + ".sha256":
			_, _ = w.Write([]byte(badChecksumText))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	result := Result{
		ReleaseAsset: AssetCheck{
			Platform:      "linux",
			Arch:          "x64",
			ArchiveName:   archiveName,
			ArchiveURL:    server.URL + "/" + archiveName,
			ChecksumName:  archiveName + ".sha256",
			ChecksumURL:   server.URL + "/" + archiveName + ".sha256",
			ArchiveFound:  true,
			ChecksumFound: true,
			Verified:      true,
		},
	}

	if _, err := applyStandaloneUpdate(context.Background(), result, executablePath); err == nil {
		t.Fatal("expected checksum mismatch error")
	}

	data, err := os.ReadFile(executablePath)
	if err != nil {
		t.Fatalf("ReadFile executable: %v", err)
	}
	if string(data) != "old-binary" {
		t.Fatalf("executable should be untouched after checksum failure, got %q", data)
	}
}

// VerifySHA256Checksum hashes whichever file the checksum text names, which
// isn't necessarily the archive the caller asked to verify. A checksum file
// that correctly verifies against a DIFFERENT (but real, correctly-hashed)
// file must still be rejected, not treated as vouching for the requested
// archive.
func TestVerifyArchiveChecksumRejectsFilenameMismatch(t *testing.T) {
	dir := t.TempDir()

	decoyName := "decoy.tar.gz"
	decoyPath := filepath.Join(dir, decoyName)
	if err := os.WriteFile(decoyPath, []byte("decoy contents"), 0o644); err != nil {
		t.Fatalf("WriteFile decoy: %v", err)
	}
	decoyChecksum, err := release.SHA256File(decoyPath)
	if err != nil {
		t.Fatalf("SHA256File: %v", err)
	}
	checksumText, err := release.FormatSHA256Checksum(decoyChecksum, decoyName)
	if err != nil {
		t.Fatalf("FormatSHA256Checksum: %v", err)
	}
	checksumPath := filepath.Join(dir, "real.tar.gz.sha256")
	if err := os.WriteFile(checksumPath, []byte(checksumText), 0o644); err != nil {
		t.Fatalf("WriteFile checksum: %v", err)
	}

	err = verifyArchiveChecksum(checksumPath, "real.tar.gz")
	if err == nil {
		t.Fatal("expected error when checksum file references a different archive")
	}
	if !strings.Contains(err.Error(), decoyName) || !strings.Contains(err.Error(), "real.tar.gz") {
		t.Fatalf("expected error to name both the referenced file and the expected one, got %q", err)
	}
}
