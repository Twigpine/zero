//go:build windows

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWindowsRestrictedTokenRealSandboxSmoke(t *testing.T) {
	if os.Getenv("ZERO_SANDBOX_REAL_SMOKE") != "1" {
		t.Skip("set ZERO_SANDBOX_REAL_SMOKE=1 to run real Windows sandbox smoke tests")
	}
	setupExe := realSmokeExecutable(t, "ZERO_WINDOWS_SANDBOX_SETUP_EXE", WindowsSandboxSetupName)
	runnerExe := realSmokeExecutable(t, "ZERO_WINDOWS_COMMAND_RUNNER_EXE", WindowsSandboxCommandRunnerName)

	root := t.TempDir()
	sandboxHome := filepath.Join(root, ".zero-sandbox")
	profile := PermissionProfile{
		FileSystem: FileSystemPolicy{
			Kind:                 FileSystemRestricted,
			ReadRoots:            []string{root},
			WriteRoots:           []WritableRoot{{Root: root, ProtectedMetadataNames: []string{".git", ".zero", ".agents"}}},
			IncludePlatformRoots: true,
			AllowTemp:            true,
		},
		Network: NetworkPolicy{Mode: NetworkDeny},
	}
	config := WindowsSandboxCommandArgsOptions{
		SandboxHome:       sandboxHome,
		CommandCWD:        root,
		WorkspaceRoots:    []string{root},
		PermissionProfile: profile,
		SandboxLevel:      WindowsSandboxLevelRestrictedToken,
	}
	runWindowsRealSmokeSetup(t, setupExe, WindowsSandboxSetupArgsOptions{
		SandboxHome:       sandboxHome,
		CommandCWD:        root,
		WorkspaceRoots:    []string{root},
		PermissionProfile: profile,
	})
	t.Cleanup(func() {
		cleanupProfile := profile
		cleanupProfile.Network = NetworkPolicy{Mode: NetworkAllow}
		runWindowsRealSmokeSetup(t, setupExe, WindowsSandboxSetupArgsOptions{
			SandboxHome:       sandboxHome,
			CommandCWD:        root,
			WorkspaceRoots:    []string{root},
			PermissionProfile: cleanupProfile,
		})
	})

	writeMarker := filepath.Join(root, "write-ok.txt")
	runWindowsRealSmokeCommand(t, runnerExe, config, []string{
		"powershell.exe",
		"-NoLogo",
		"-NoProfile",
		"-NonInteractive",
		"-ExecutionPolicy",
		"Bypass",
		"-Command",
		fmt.Sprintf("Set-Content -LiteralPath %s -Value ok", powershellSingleQuote(writeMarker)),
	}, 0)
	if bytes, err := os.ReadFile(writeMarker); err != nil || strings.TrimSpace(string(bytes)) != "ok" {
		t.Fatalf("sandboxed write marker = %q, %v; want ok", bytes, err)
	}

	// SID broadening is disabled, so the restricted-SID list never includes
	// Users/Authenticated Users. The write grant those groups hold on
	// C:\Users\Public must not be reachable through the restricted-SID check.
	// Pin that a write there fails: an independent shared-writable directory
	// outside every workspace write root.
	publicDir := os.Getenv("PUBLIC")
	if publicDir == "" {
		t.Log("PUBLIC is not set; skipping C:\\Users\\Public write-jail probe")
	} else {
		publicProbe := allocateSharedDirectoryProbe(t, publicDir, "elevated-public")
		runWindowsRealSmokeCommand(t, runnerExe, config, publicProbe.DeniedWriteCommand(), deniedWriteExitCode)
		if _, err := os.Stat(publicProbe.Path()); err == nil {
			t.Fatalf("Windows sandbox allowed a write to the shared C:\\Users\\Public directory")
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat public marker: %v", err)
		}
	}

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen loopback for Windows network smoke: %v", err)
	}
	defer listener.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			_ = conn.Close()
			accepted <- struct{}{}
			return
		}
	}()

	networkAllowedMarker := filepath.Join(root, "network-allowed.txt")
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("parse listener address %q: %v", listener.Addr(), err)
	}
	script := fmt.Sprintf(`
$marker = %s
$client = [System.Net.Sockets.TcpClient]::new()
$connect = $client.BeginConnect('127.0.0.1', %s, $null, $null)
if ($connect.AsyncWaitHandle.WaitOne(1500, $false)) {
  try {
    $client.EndConnect($connect)
    Set-Content -LiteralPath $marker -Value allowed
    exit 42
  } catch {
    exit 0
  } finally {
    $client.Close()
  }
}
$client.Close()
exit 0
`, powershellSingleQuote(networkAllowedMarker), port)
	runWindowsRealSmokeCommand(t, runnerExe, config, []string{
		"powershell.exe",
		"-NoLogo",
		"-NoProfile",
		"-NonInteractive",
		"-ExecutionPolicy",
		"Bypass",
		"-Command",
		script,
	}, 0)
	if _, err := os.Stat(networkAllowedMarker); err == nil {
		t.Fatalf("Windows sandbox allowed loopback network connection under network deny")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat network marker: %v", err)
	}
	select {
	case <-accepted:
		t.Fatalf("Windows sandbox loopback listener accepted a denied connection")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestWindowsUnelevatedRealSandboxSmoke exercises the unelevated tier end to
// end WITHOUT running the elevated setup helper: the command runner applies
// the workspace ACLs itself, a write inside the workspace succeeds under the
// restricted token, and a write outside every granted root is denied. Unlike
// the elevated smoke above it needs no Administrator terminal.
func TestWindowsUnelevatedRealSandboxSmoke(t *testing.T) {
	if os.Getenv("ZERO_SANDBOX_REAL_SMOKE") != "1" {
		t.Skip("set ZERO_SANDBOX_REAL_SMOKE=1 to run real Windows sandbox smoke tests")
	}
	runnerExe := realSmokeExecutable(t, "ZERO_WINDOWS_COMMAND_RUNNER_EXE", WindowsSandboxCommandRunnerName)

	root := t.TempDir()
	outside := t.TempDir()
	privateDir := filepath.Join(root, "private")
	if err := os.MkdirAll(privateDir, 0o700); err != nil {
		t.Fatalf("MkdirAll private: %v", err)
	}
	secretFile := filepath.Join(privateDir, "secret.txt")
	if err := os.WriteFile(secretFile, []byte("super-secret"), 0o600); err != nil {
		t.Fatalf("WriteFile secret: %v", err)
	}

	sandboxHome := filepath.Join(root, ".zero-sandbox")
	// Success path: restricted FS write-jail with no DenyRead. Non-empty DenyRead
	// is unsupported on both restricted-token tiers under the narrow SID set
	// (PR #640); the rejection probe below covers that separately.
	profile := PermissionProfile{
		FileSystem: FileSystemPolicy{
			Kind:                 FileSystemRestricted,
			ReadRoots:            []string{root},
			WriteRoots:           []WritableRoot{{Root: root, ProtectedMetadataNames: []string{".git", ".zero", ".agents"}}},
			IncludePlatformRoots: true,
			AllowTemp:            true,
		},
		Network: NetworkPolicy{Mode: NetworkDeny},
	}
	config := WindowsSandboxCommandArgsOptions{
		SandboxHome:       sandboxHome,
		CommandCWD:        root,
		WorkspaceRoots:    []string{root},
		PermissionProfile: profile,
		SandboxLevel:      WindowsSandboxLevelUnelevated,
	}

	// cmd.exe rather than powershell.exe: managed PowerShell cannot initialize
	// its crypto provider under a write-restricted token on some hosts
	// (0x8009001d, the same restricted-token limitation the runner documents for
	// Schannel), and the write-jail assertion only needs a native shell.
	insideMarker := filepath.Join(root, "unelevated-write-ok.txt")
	runWindowsRealSmokeCommand(t, runnerExe, config, []string{
		"cmd.exe", "/d", "/s", "/c", "echo ok>" + insideMarker,
	}, 0)
	if bytes, err := os.ReadFile(insideMarker); err != nil || strings.TrimSpace(string(bytes)) != "ok" {
		t.Fatalf("unelevated sandboxed write marker = %q, %v; want ok", bytes, err)
	}
	if _, err := os.Stat(WindowsUnelevatedSetupMarkerPath(sandboxHome)); err != nil {
		t.Fatalf("expected the unelevated setup marker to be recorded: %v", err)
	}

	// DenyRead is unsupported on both restricted-token tiers under the narrow
	// SID set (PR #640): the runner must reject before launch rather than
	// attempting a fully restricted token that cannot load system tools.
	denyReadConfig := config
	denyReadConfig.PermissionProfile.FileSystem.DenyRead = []string{privateDir}
	runWindowsRealSmokeCommandExpectError(t, runnerExe, denyReadConfig, []string{
		"cmd.exe", "/d", "/s", "/c", "type " + secretFile,
	}, "DenyRead", "not supported")
	// The secret must remain readable from the host; the sandbox never ran.
	if data, err := os.ReadFile(secretFile); err != nil || string(data) != "super-secret" {
		t.Fatalf("host secret file after rejected DenyRead launch: %q, %v", data, err)
	}

	outsideMarker := filepath.Join(outside, "unelevated-write-denied.txt")
	runWindowsRealSmokeCommand(t, runnerExe, config, []string{
		"cmd.exe", "/d", "/s", "/c", "echo leaked>" + outsideMarker,
	}, 1)
	if _, err := os.Stat(outsideMarker); err == nil {
		t.Fatalf("unelevated sandbox allowed a write outside every granted root")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat outside marker: %v", err)
	}

	// Verify write to C:\ProgramData is blocked
	programData := os.Getenv("ProgramData")
	if programData != "" {
		programDataProbe := allocateSharedDirectoryProbe(t, programData, "unelevated-programdata")
		runWindowsRealSmokeCommand(t, runnerExe, config, programDataProbe.DeniedWriteCommand(), deniedWriteExitCode)
		if _, err := os.Stat(programDataProbe.Path()); err == nil {
			t.Fatalf("unelevated sandbox allowed a write to ProgramData shared directory")
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat ProgramData marker: %v", err)
		}
	}
}

// TestWindowsRestrictedTokenNestedPipeCapture pins the fix in
// broadenWindowsRestrictedTokenDefaultDacl (windows_token_windows.go): a
// nested subprocess spawned FROM WITHIN the sandboxed process, with its
// output captured via a pipe, must succeed. Before the fix, the sandboxed
// process's own attempt to create a pipe for such a subprocess failed with
// ERROR_ACCESS_DENIED (Win32 error 5) — the WRITE_RESTRICTED token's extra
// access check has no restricted-SID match against a freshly created pipe's
// default security descriptor. This is exactly the pattern any tool that
// shells out internally and captures output hits (`gh` invoking `git`, for
// one; cmd.exe's own FOR /F does the identical CreatePipe+CreateProcess
// dance internally, so it reproduces the bug with no external dependency
// beyond cmd.exe itself).
func TestWindowsRestrictedTokenNestedPipeCapture(t *testing.T) {
	if os.Getenv("ZERO_SANDBOX_REAL_SMOKE") != "1" {
		t.Skip("set ZERO_SANDBOX_REAL_SMOKE=1 to run real Windows sandbox smoke tests")
	}
	runnerExe := realSmokeExecutable(t, "ZERO_WINDOWS_COMMAND_RUNNER_EXE", WindowsSandboxCommandRunnerName)

	root := t.TempDir()
	sandboxHome := filepath.Join(root, ".zero-sandbox")
	profile := PermissionProfile{
		FileSystem: FileSystemPolicy{
			Kind:                 FileSystemRestricted,
			ReadRoots:            []string{root},
			WriteRoots:           []WritableRoot{{Root: root, ProtectedMetadataNames: []string{".git", ".zero", ".agents"}}},
			IncludePlatformRoots: true,
			AllowTemp:            true,
		},
		Network: NetworkPolicy{Mode: NetworkDeny},
	}
	config := WindowsSandboxCommandArgsOptions{
		SandboxHome:       sandboxHome,
		CommandCWD:        root,
		WorkspaceRoots:    []string{root},
		PermissionProfile: profile,
		SandboxLevel:      WindowsSandboxLevelUnelevated,
	}

	marker := filepath.Join(root, "nested-pipe-marker.txt")
	script := filepath.Join(root, "nested-pipe.cmd")
	// FOR /F drives cmd.exe's own internal CreatePipe+CreateProcess capture
	// of the quoted command's output — the exact mechanism this fix targets.
	// The full path to whoami.exe sidesteps PATH resolution inside the
	// sandboxed process's minimal environment.
	scriptBody := "for /F %%i in ('C:\\Windows\\System32\\whoami.exe') do echo %%i> " + marker + "\r\n"
	if err := os.WriteFile(script, []byte(scriptBody), 0o644); err != nil {
		t.Fatalf("write nested-pipe script: %v", err)
	}

	runWindowsRealSmokeCommand(t, runnerExe, config, []string{
		"cmd.exe", "/d", "/s", "/c", script,
	}, 0)
	captured, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read nested-pipe marker: %v", err)
	}
	if strings.TrimSpace(string(captured)) == "" {
		t.Fatalf("nested-pipe marker is empty; FOR /F failed to capture the subprocess's output")
	}
}

// TestWindowsRestrictedTokenPowerShell exercises the default Windows shell
// shape end to end under the write-restricted token. It covers PowerShell
// initialization, a native pipeline (which creates IPC objects), UTF-8 output,
// and workspace write enforcement.
func TestWindowsRestrictedTokenPowerShell(t *testing.T) {
	if os.Getenv("ZERO_SANDBOX_REAL_SMOKE") != "1" {
		t.Skip("set ZERO_SANDBOX_REAL_SMOKE=1 to run real Windows sandbox smoke tests")
	}
	runnerExe := realSmokeExecutable(t, "ZERO_WINDOWS_COMMAND_RUNNER_EXE", WindowsSandboxCommandRunnerName)
	powerShell := realSmokePowerShell(t)

	root := t.TempDir()
	outside := t.TempDir()
	sandboxHome := filepath.Join(root, ".zero-sandbox")
	profile := PermissionProfile{
		FileSystem: FileSystemPolicy{
			Kind:                 FileSystemRestricted,
			ReadRoots:            []string{root},
			WriteRoots:           []WritableRoot{{Root: root}},
			IncludePlatformRoots: true,
			AllowTemp:            true,
		},
		Network: NetworkPolicy{Mode: NetworkDeny},
	}
	config := WindowsSandboxCommandArgsOptions{
		SandboxHome:       sandboxHome,
		CommandCWD:        root,
		WorkspaceRoots:    []string{root},
		PermissionProfile: profile,
		SandboxLevel:      WindowsSandboxLevelUnelevated,
	}

	insideMarker := filepath.Join(root, "powershell-ok.txt")
	script := "$ErrorActionPreference='Stop'; " +
		"[Console]::OutputEncoding=[System.Text.Encoding]::UTF8; " +
		"$values = 1,2,3 | ForEach-Object { $_ * 2 }; " +
		"[IO.File]::WriteAllText(" + powershellSingleQuote(insideMarker) + ", (($values -join ',') + ' ✓'))"
	runWindowsRealSmokeCommand(t, runnerExe, config, []string{
		powerShell, "-NoLogo", "-NoProfile", "-Command", script,
	}, 0)
	if bytes, err := os.ReadFile(insideMarker); err != nil || strings.TrimSpace(string(bytes)) != "2,4,6 ✓" {
		t.Fatalf("PowerShell sandbox marker = %q, %v; want pipeline output", bytes, err)
	}

	outsideMarker := filepath.Join(outside, "powershell-denied.txt")
	deniedScript := "$ErrorActionPreference='Stop'; " +
		"[IO.File]::WriteAllText(" + powershellSingleQuote(outsideMarker) + ", 'leaked')"
	runWindowsRealSmokeCommand(t, runnerExe, config, []string{
		powerShell, "-NoLogo", "-NoProfile", "-Command", deniedScript,
	}, 1)
	if _, err := os.Stat(outsideMarker); err == nil {
		t.Fatal("sandboxed PowerShell wrote outside every granted root")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat outside PowerShell marker: %v", err)
	}
}

func realSmokePowerShell(t *testing.T) string {
	t.Helper()
	for _, candidate := range []string{"pwsh.exe", "pwsh", "powershell.exe", "powershell"} {
		if path, err := exec.LookPath(candidate); err == nil {
			return path
		}
	}
	t.Skip("PowerShell is unavailable")
	return ""
}

func realSmokeExecutable(t *testing.T, envKey string, fallbackName string) string {
	t.Helper()
	if path := os.Getenv(envKey); path != "" {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s=%q is not usable: %v", envKey, path, err)
		}
		return path
	}
	path, err := exec.LookPath(fallbackName)
	if err != nil {
		t.Skipf("%s is not available and %s is unset", fallbackName, envKey)
	}
	return path
}

func runWindowsRealSmokeSetup(t *testing.T, setupExe string, options WindowsSandboxSetupArgsOptions) {
	t.Helper()
	args, err := BuildWindowsSandboxSetupArgs(options)
	if err != nil {
		t.Fatalf("BuildWindowsSandboxSetupArgs: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, setupExe, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Windows sandbox setup failed: %v\n%s", err, output)
	}
}

func runWindowsRealSmokeCommand(t *testing.T, runnerExe string, base WindowsSandboxCommandArgsOptions, command []string, wantCode int) {
	t.Helper()
	base.Command = command
	args, err := BuildWindowsSandboxCommandArgs(base)
	if err != nil {
		t.Fatalf("BuildWindowsSandboxCommandArgs: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, runnerExe, args...)
	output, err := cmd.CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == wantCode {
		return
	}
	if err != nil {
		t.Fatalf("Windows sandbox command failed: %v\n%s", err, output)
	}
	if wantCode != 0 {
		t.Fatalf("Windows sandbox command exit code = 0, want %d\n%s", wantCode, output)
	}
}

// runWindowsRealSmokeCommandExpectError runs the command runner and requires a
// non-zero exit whose combined output contains each want substring (used for
// explicit unsupported-mode rejections rather than sandboxed command failures).
func runWindowsRealSmokeCommandExpectError(t *testing.T, runnerExe string, base WindowsSandboxCommandArgsOptions, command []string, wantSubstr ...string) {
	t.Helper()
	base.Command = command
	args, err := BuildWindowsSandboxCommandArgs(base)
	if err != nil {
		t.Fatalf("BuildWindowsSandboxCommandArgs: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, runnerExe, args...)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("Windows sandbox command timed out: %v\n%s", ctx.Err(), output)
	}
	if err == nil {
		t.Fatalf("Windows sandbox command exit code = 0, want error containing %v\n%s", wantSubstr, output)
	}
	text := string(output)
	for _, want := range wantSubstr {
		if !strings.Contains(text, want) {
			t.Fatalf("Windows sandbox command error missing %q: %v\n%s", want, err, output)
		}
	}
}

// The write jail must hold on a path whose DACL grants Everyone write access.
//
// A WRITE_RESTRICTED token runs TWO checks for a write and needs both to pass:
// the normal one against its enabled SIDs, and a second against its RESTRICTED
// SID list. The jail is built on the second check only succeeding where Zero has
// explicitly ACL'd a capability SID. Putting the World SID (S-1-1-0) in the
// restricted list breaks that globally — every principal is a member of
// Everyone, so on a DACL that grants Everyone write, the restricted half passes
// for free and confinement collapses back to the ordinary user's own
// permissions, which is precisely the boundary the sandbox exists to be
// stricter than.
//
// The runner already states this rule for the SIDs it refuses to add ("None of
// the granted SIDs can be added to the restricted list without collapsing the
// write jail"); Everyone was in the list anyway, since the original sandbox
// baseline.
//
// Realistic rather than theoretical: administrators open share roots with
// Everyone:F, and third-party installers ship loose ACLs. It needs no privilege,
// no symlink and no race. Checked and ruled out: C:\Users\Public\Documents
// grants BATCH, not Everyone, so the common "public folder" case is not this.
func TestWindowsRestrictedTokenDeniesWritesToEveryoneWritablePaths(t *testing.T) {
	if os.Getenv("ZERO_SANDBOX_REAL_SMOKE") != "1" {
		t.Skip("set ZERO_SANDBOX_REAL_SMOKE=1 to run real Windows sandbox smoke tests")
	}
	runnerExe := realSmokeExecutable(t, "ZERO_WINDOWS_COMMAND_RUNNER_EXE", WindowsSandboxCommandRunnerName)

	root := t.TempDir()
	outside := t.TempDir()
	everyoneDir := filepath.Join(outside, "everyone-writable")
	if err := os.MkdirAll(everyoneDir, 0o700); err != nil {
		t.Fatalf("MkdirAll everyone-writable: %v", err)
	}
	// Granted through the production ACL applier, so the hostile DACL is built
	// the same way a real one is rather than by a test-only shortcut.
	snapshot, applied, err := applyWindowsACLPathGroup(windowsACLPathGroup{
		Path: everyoneDir,
		Entries: []WindowsACLEntry{{
			Action:     WindowsACLAllowWrite,
			Path:       everyoneDir,
			Capability: "S-1-1-0",
		}},
	})
	if err != nil {
		t.Fatalf("grant Everyone write: %v", err)
	}
	if !applied {
		t.Fatal("precondition: the Everyone grant did not apply, so there is no bypass to test for")
	}
	t.Cleanup(func() { _ = rollbackWindowsACLSnapshots([]windowsACLSnapshot{snapshot}) })

	sandboxHome := filepath.Join(root, ".zero-sandbox")
	// No DenyRead on purpose: that is what makes the runner choose the
	// WRITE_RESTRICTED token, which is the default posture and the one whose
	// restricted-SID list this test is about.
	profile := PermissionProfile{
		FileSystem: FileSystemPolicy{
			Kind:                 FileSystemRestricted,
			ReadRoots:            []string{root},
			WriteRoots:           []WritableRoot{{Root: root, ProtectedMetadataNames: []string{".git", ".zero", ".agents"}}},
			IncludePlatformRoots: true,
			AllowTemp:            true,
		},
		Network: NetworkPolicy{Mode: NetworkDeny},
	}
	config := WindowsSandboxCommandArgsOptions{
		SandboxHome:       sandboxHome,
		CommandCWD:        root,
		WorkspaceRoots:    []string{root},
		PermissionProfile: profile,
		SandboxLevel:      WindowsSandboxLevelUnelevated,
	}

	// Granted root first. A jail that denies everything would satisfy the two
	// assertions below while being useless, and this separates the two outcomes.
	insideMarker := filepath.Join(root, "write-ok.txt")
	runWindowsRealSmokeCommand(t, runnerExe, config, []string{
		"cmd.exe", "/d", "/s", "/c", "echo ok>" + insideMarker,
	}, 0)
	if contents, err := os.ReadFile(insideMarker); err != nil || strings.TrimSpace(string(contents)) != "ok" {
		t.Fatalf("sandboxed write to a granted root = %q, %v; want ok", contents, err)
	}

	// Control: an ordinary directory outside every granted root. This is the
	// behaviour the Everyone case must match.
	controlMarker := filepath.Join(outside, "control-denied.txt")
	runWindowsRealSmokeCommand(t, runnerExe, config, deniedWriteCommand(controlMarker), deniedWriteExitCode)
	if _, err := os.Stat(controlMarker); err == nil {
		t.Fatal("precondition: the sandbox allowed a write to an ordinary path outside every granted root, so this test cannot measure the Everyone case")
	}

	// The assertion. Same as the control in every respect except the DACL.
	everyoneMarker := filepath.Join(everyoneDir, "everyone-denied.txt")
	runWindowsRealSmokeCommand(t, runnerExe, config, deniedWriteCommand(everyoneMarker), deniedWriteExitCode)
	if _, err := os.Stat(everyoneMarker); err == nil {
		t.Error("the sandbox wrote outside every granted root because the path grants Everyone write; the restricted-SID list is satisfied by a SID every principal carries")
	} else if !os.IsNotExist(err) {
		t.Errorf("stat the Everyone-writable marker: %v", err)
	}
}

// deniedWriteExitCode is chosen so a denial cannot be confused with the runner
// failing to start the command at all.
//
// The obvious spelling of these assertions is `echo leaked>path` expecting exit
// 1, but the runner itself exits 1 for its own errors — a failed marker
// validation, a bad argument, a token it could not build. A test written that
// way passes both when the sandbox denies the write and when nothing ever ran,
// and the second case proves nothing while looking identical to success. On a
// test whose whole job is catching a confinement regression, that is a failure
// mode to design out rather than hope about.
//
// 77 is arbitrary beyond being outside the range the runner produces for itself.
const deniedWriteExitCode = 77

// deniedWriteCommand attempts a write and reports deniedWriteExitCode when the
// redirect is refused, so the exit code also proves cmd.exe actually ran.
func deniedWriteCommand(marker string) []string {
	return []string{"cmd.exe", "/d", "/c", "echo leaked>" + cmdQuote(marker) + " || exit " + strconv.Itoa(deniedWriteExitCode)}
}

func cmdQuote(path string) string {
	return `"` + strings.ReplaceAll(path, `"`, `\"`) + `"`
}

type sharedDirectoryProbe struct {
	path   string
	marker string
}

func allocateSharedDirectoryProbe(t testing.TB, dir, prefix string) *sharedDirectoryProbe {
	t.Helper()
	if dir == "" {
		t.Skip("shared directory path is not set")
	}
	var probePath string
	for attempt := 0; attempt < 50; attempt++ {
		candidate := filepath.Join(dir, fmt.Sprintf("zero-smoke-%s-%d-%d-%d.txt", prefix, os.Getpid(), time.Now().UnixNano(), attempt))
		if _, err := os.Lstat(candidate); os.IsNotExist(err) {
			probePath = candidate
			break
		}
	}
	if probePath == "" {
		t.Fatalf("allocate shared directory probe in %s: failed to find unused filename", dir)
	}
	marker := fmt.Sprintf("leaked-%s-%d-%d", prefix, os.Getpid(), time.Now().UnixNano())
	p := &sharedDirectoryProbe{path: probePath, marker: marker}
	t.Cleanup(func() {
		p.cleanup(t)
	})
	return p
}

func (p *sharedDirectoryProbe) Path() string {
	return p.path
}

func (p *sharedDirectoryProbe) Marker() string {
	return p.marker
}

func (p *sharedDirectoryProbe) DeniedWriteCommand() []string {
	return []string{"cmd.exe", "/d", "/c", "echo " + p.marker + ">" + cmdQuote(p.path) + " || exit " + strconv.Itoa(deniedWriteExitCode)}
}

func (p *sharedDirectoryProbe) cleanup(t testing.TB) {
	t.Helper()
	data, err := os.ReadFile(p.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("cleanup probe read %s: %v", p.path, err)
		}
		return
	}
	if strings.TrimSpace(string(data)) == p.marker {
		if err := os.Remove(p.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("cleanup probe remove %s: %v", p.path, err)
		}
	}
}

func powershellSingleQuote(value string) string {
	out := "'"
	for _, r := range value {
		if r == '\'' {
			out += "''"
		} else {
			out += string(r)
		}
	}
	return out + "'"
}

func TestSharedDirectoryProbeLifecycle(t *testing.T) {
	dir := t.TempDir()

	// 1. Two simultaneous allocations produce distinct non-colliding paths and markers.
	probe1 := allocateSharedDirectoryProbe(t, dir, "p1")
	probe2 := allocateSharedDirectoryProbe(t, dir, "p2")
	if probe1.Path() == probe2.Path() {
		t.Fatalf("expected distinct probe paths, got %q and %q", probe1.Path(), probe2.Path())
	}
	if probe1.Marker() == probe2.Marker() {
		t.Fatalf("expected distinct probe markers, got %q and %q", probe1.Marker(), probe2.Marker())
	}

	// 2. Pre-existing unrelated file is never selected or deleted.
	unrelatedFile := filepath.Join(dir, "unrelated.txt")
	if err := os.WriteFile(unrelatedFile, []byte("preserve me"), 0o600); err != nil {
		t.Fatalf("write unrelated file: %v", err)
	}
	probe3 := allocateSharedDirectoryProbe(t, dir, "p3")
	probe3.cleanup(t)
	if data, err := os.ReadFile(unrelatedFile); err != nil || string(data) != "preserve me" {
		t.Fatalf("unrelated file was modified or deleted: data=%q, err=%v", data, err)
	}

	// 3. Interrupted / no-create path: cleanup on absent file succeeds quietly.
	probe4 := allocateSharedDirectoryProbe(t, dir, "p4")
	probe4.cleanup(t)

	// 4. Unexpected write created during test with matching marker is cleaned up.
	probe5 := allocateSharedDirectoryProbe(t, dir, "p5")
	if err := os.WriteFile(probe5.Path(), []byte(probe5.Marker()), 0o600); err != nil {
		t.Fatalf("write probe5 file: %v", err)
	}
	probe5.cleanup(t)
	if _, err := os.Lstat(probe5.Path()); !os.IsNotExist(err) {
		t.Fatalf("expected probe5 to be cleaned up after creation, stat err=%v", err)
	}

	// 5. File at probe path with foreign/unmatched content is NOT removed.
	probe6 := allocateSharedDirectoryProbe(t, dir, "p6")
	if err := os.WriteFile(probe6.Path(), []byte("unrelated-foreign-content"), 0o600); err != nil {
		t.Fatalf("write probe6 file: %v", err)
	}
	probe6.cleanup(t)
	if _, err := os.Lstat(probe6.Path()); err != nil {
		t.Fatalf("expected probe6 with foreign content to be preserved, stat err=%v", err)
	}
	_ = os.Remove(probe6.Path())
}
