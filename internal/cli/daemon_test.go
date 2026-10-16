package cli

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/background"
)

// isolateDaemonPaths points DefaultPaths at a temp dir so the test never touches
// a real daemon on the dev machine.
func isolateDaemonPaths(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
}

func runDaemonCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runDaemon(args, &out, &errb, appDeps{})
	return code, out.String(), errb.String()
}

func TestDaemonUsage(t *testing.T) {
	code, _, _ := runDaemonCLI(t)
	if code != exitUsage {
		t.Fatalf("no-args exit = %d, want exitUsage", code)
	}
	code, out, _ := runDaemonCLI(t, "--help")
	if code != exitSuccess || !strings.Contains(out, "Usage: zero daemon") {
		t.Fatalf("--help exit=%d out=%q", code, out)
	}
	for _, want := range []string{
		"macOS shell commands require the inline token",
		"file only when it has no hard-link aliases",
		"shell-writable root shares its filesystem",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("--help does not state %q in the file-token shell contract: %q", want, out)
		}
	}
}

func TestDaemonUnknownSubcommand(t *testing.T) {
	code, _, errb := runDaemonCLI(t, "frobnicate")
	if code != exitUsage {
		t.Fatalf("unknown subcommand exit = %d, want exitUsage", code)
	}
	if !strings.Contains(errb, "unknown daemon subcommand") {
		t.Fatalf("stderr = %q, want unknown-subcommand message", errb)
	}
}

func TestDaemonRunRequiresSession(t *testing.T) {
	isolateDaemonPaths(t)
	code, _, errb := runDaemonCLI(t, "run", "--prompt", "hi")
	if code != exitUsage {
		t.Fatalf("run without --session exit = %d, want exitUsage", code)
	}
	if !strings.Contains(errb, "--session") {
		t.Fatalf("stderr = %q, want a --session hint", errb)
	}
}

func TestDaemonRunRequiresPromptOrArgs(t *testing.T) {
	isolateDaemonPaths(t)
	code, _, errb := runDaemonCLI(t, "run", "--session", "s1")
	if code != exitUsage {
		t.Fatalf("run without prompt/args exit = %d, want exitUsage", code)
	}
	if !strings.Contains(errb, "--prompt") {
		t.Fatalf("stderr = %q, want a --prompt hint", errb)
	}
}

func TestDaemonStopWhenNotRunning(t *testing.T) {
	isolateDaemonPaths(t)
	code, out, _ := runDaemonCLI(t, "stop")
	if code != exitSuccess {
		t.Fatalf("stop (not running) exit = %d, want exitSuccess", code)
	}
	if !strings.Contains(out, "not running") {
		t.Fatalf("stop output = %q, want 'not running'", out)
	}
}

func TestDaemonStatusWhenNotRunning(t *testing.T) {
	isolateDaemonPaths(t)
	code, out, _ := runDaemonCLI(t, "status")
	if code != exitSuccess {
		t.Fatalf("status (not running) exit = %d, want exitSuccess", code)
	}
	if !strings.Contains(out, "not running") {
		t.Fatalf("status output = %q, want 'not running'", out)
	}
}

func TestDaemonAttachRequiresSession(t *testing.T) {
	isolateDaemonPaths(t)
	code, _, errb := runDaemonCLI(t, "attach")
	if code != exitUsage {
		t.Fatalf("attach without session exit = %d, want exitUsage", code)
	}
	if !strings.Contains(errb, "session") {
		t.Fatalf("stderr = %q, want a session hint", errb)
	}
}

func TestDaemonRunWhenNotRunning(t *testing.T) {
	isolateDaemonPaths(t)
	code, _, errb := runDaemonCLI(t, "run", "--session", "s1", "--prompt", "hello")
	if code != exitCrash {
		t.Fatalf("run (no daemon) exit = %d, want exitCrash", code)
	}
	if !strings.Contains(errb, "not running") {
		t.Fatalf("stderr = %q, want 'not running'", errb)
	}
}

func TestDaemonSubcommandsRejectExtraArgs(t *testing.T) {
	isolateDaemonPaths(t)
	cases := [][]string{
		{"stop", "oops"},
		{"status", "oops"},
		{"attach", "s1", "extra"},
	}
	for _, args := range cases {
		code, _, errb := runDaemonCLI(t, args...)
		if code != exitUsage {
			t.Fatalf("%v exit = %d, want exitUsage (reject extra args); stderr=%q", args, code, errb)
		}
	}
}

func TestWaitForDaemonReadinessChecksTimeoutBoundary(t *testing.T) {
	checks := 0
	ready := waitForDaemonReadiness(func() bool {
		checks++
		return checks == 2
	}, 0, time.Millisecond)
	if !ready {
		t.Fatal("daemon that became reachable at the timeout boundary was not detected")
	}
	if checks != 2 {
		t.Fatalf("reachability checks = %d, want initial and final checks", checks)
	}
}

func TestTerminateAndReapDaemonProcess(t *testing.T) {
	cmd := daemonDetachedChildCommand("hang")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}

	if err := terminateAndReapDaemonProcess(cmd); err != nil {
		t.Fatalf("terminate and reap helper process: %v", err)
	}
	if cmd.ProcessState == nil {
		t.Fatalf("helper process was not reaped: state=%v", cmd.ProcessState)
	}
	if cmd.ProcessState.Success() {
		t.Fatalf("helper process unexpectedly exited successfully: %v", cmd.ProcessState)
	}
}

func TestStartAndAwaitDaemonProcessTimeoutTerminatesAndReaps(t *testing.T) {
	cmd := daemonDetachedChildCommand("hang")
	if err := startAndAwaitDaemonProcess(cmd, func() bool { return false }, 0, time.Millisecond); !errors.Is(err, errDaemonStartTimeout) {
		t.Fatalf("start and await error = %v, want timeout", err)
	}
	if cmd.ProcessState == nil {
		t.Fatal("timed-out helper process was not reaped")
	}
}

func TestStartAndAwaitDaemonProcessReleasesReadyChild(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child-finished")
	cmd := daemonDetachedChildCommand("mark", "ZERO_TEST_DAEMON_CHILD_MARKER="+marker)
	if err := startAndAwaitDaemonProcess(cmd, func() bool { return true }, time.Second, time.Millisecond); err != nil {
		t.Fatalf("start and await ready helper: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("released helper process did not remain alive to write its marker")
}

func daemonDetachedChildCommand(mode string, extraEnv ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=TestDaemonDetachedChildProcess")
	cmd.Env = append(os.Environ(), "ZERO_TEST_DAEMON_DETACHED_CHILD="+mode)
	cmd.Env = append(cmd.Env, extraEnv...)
	background.ConfigureChildProcessGroup(cmd)
	return cmd
}

func TestDaemonDetachedChildProcess(t *testing.T) {
	switch os.Getenv("ZERO_TEST_DAEMON_DETACHED_CHILD") {
	case "":
		return
	case "mark":
		if err := os.WriteFile(os.Getenv("ZERO_TEST_DAEMON_CHILD_MARKER"), []byte("ready"), 0o600); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func TestDaemonServeRemoteCanonicalizesTokenFileBeforeStartingWorkers(t *testing.T) {
	isolateDaemonPaths(t)
	certFile, keyFile := writeDaemonTestCertificate(t)
	startDir := t.TempDir()
	t.Chdir(startDir)
	if err := os.WriteFile("token", []byte("bridge-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZERO_DAEMON_REMOTE_TOKEN", "")
	t.Setenv("ZERO_DAEMON_REMOTE_TOKEN_FILE", "token")
	t.Setenv("ZERO_INTERNAL_DAEMON_REMOTE_TOKEN_FILE_RESOLVED", "")
	t.Setenv("ZERO_INTERNAL_DAEMON_REMOTE_TOKEN_FILE_IDENTITY", "")

	code, _, _ := runDaemonCLI(t, "serve-remote", "--addr", "127.0.0.1:not-a-port", "--tls-cert", certFile, "--tls-key", keyFile)
	if code != exitCrash {
		t.Fatalf("serve-remote exit = %d, want bind failure", code)
	}
	configured, err := filepath.Abs(filepath.Join(startDir, "token"))
	if err != nil {
		t.Fatalf("Abs(token): %v", err)
	}
	resolved, err := filepath.EvalSymlinks(configured)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", configured, err)
	}
	if got := os.Getenv("ZERO_DAEMON_REMOTE_TOKEN_FILE"); got != configured {
		t.Fatalf("ZERO_DAEMON_REMOTE_TOKEN_FILE = %q, want configured path %q", got, configured)
	}
	if got := os.Getenv("ZERO_INTERNAL_DAEMON_REMOTE_TOKEN_FILE_RESOLVED"); got != resolved {
		t.Fatalf("resolved token source = %q, want %q", got, resolved)
	}
}

type daemonTestWriter func([]byte) (int, error)

func (write daemonTestWriter) Write(data []byte) (int, error) { return write(data) }

func TestDaemonServeRemoteJoinsLocalServeBeforeReturningBindError(t *testing.T) {
	certFile, keyFile := writeDaemonTestCertificate(t)
	// Make local Serve fail without opening a socket on any platform.
	runtimeFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(runtimeFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", runtimeFile)
	t.Setenv("ZERO_DAEMON_REMOTE_TOKEN", "test-token")
	localLogging := make(chan struct{})
	releaseLocal := make(chan struct{})
	localFinished := make(chan struct{})
	returned := make(chan int, 1)
	var terminalError string
	stderr := daemonTestWriter(func(data []byte) (int, error) {
		if strings.Contains(string(data), "local serve error:") {
			close(localLogging)
			<-releaseLocal
			close(localFinished)
		} else {
			terminalError += string(data)
		}
		return len(data), nil
	})
	// Do not let the main goroutine reach the TLS failure select until its
	// local server goroutine is demonstrably still inside the logging callback.
	stdout := daemonTestWriter(func(data []byte) (int, error) {
		select {
		case <-localLogging:
			return len(data), nil
		case <-time.After(5 * time.Second):
			return 0, io.ErrNoProgress
		}
	})
	go func() {
		returned <- runDaemonServeRemote([]string{"--addr", "127.0.0.1:not-a-port", "--tls-cert", certFile, "--tls-key", keyFile}, stdout, stderr)
	}()
	select {
	case <-localLogging:
	case <-time.After(5 * time.Second):
		close(releaseLocal)
		t.Fatal("local Serve did not reach the synchronized failure")
	}
	select {
	case code := <-returned:
		close(releaseLocal)
		<-localFinished
		t.Fatalf("serve-remote returned %d while local Serve was still logging", code)
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseLocal)
	select {
	case code := <-returned:
		if code != exitCrash || !strings.Contains(terminalError, "not-a-port") {
			t.Fatalf("original TLS bind error was lost: code=%d stderr=%q", code, terminalError)
		}
		select {
		case <-localFinished:
		default:
			t.Fatal("local Serve callback outlived serve-remote")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve-remote did not finish after releasing local Serve")
	}
}

func writeDaemonTestCertificate(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "zero-daemon-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	certOut, err := os.Create(certFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatal(err)
	}
	if err := certOut.Close(); err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyOut, err := os.Create(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		t.Fatal(err)
	}
	if err := keyOut.Close(); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}
