package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/observability"
	"github.com/Gitlawb/zero/internal/testutil"
)

func TestServeSupportsReadOnlyCustomRuntimeDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission compatibility regression")
	}
	dir, err := os.MkdirTemp("/tmp", "zero-serve-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	paths := Paths{
		Socket: filepath.Join(dir, "daemon.sock"),
		Lock:   filepath.Join(dir, "daemon.lock"),
		Status: filepath.Join(dir, "daemon.status"),
	}
	launcher, _ := seqLauncher(&fakeWorker{pid: 1})
	srv := newTestServerWithPaths(t, launcher, paths)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()
	testutil.WaitFor(t, "file "+paths.Status, func() bool { _, err := os.Stat(paths.Status); return err == nil })
	srv.Shutdown()
	if err := <-serveErr; err != nil {
		t.Fatalf("Serve with 0755 custom runtime directory: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("custom runtime directory permissions = %04o, want unchanged 0755", got)
	}
}

func TestServeKeepsDefaultRootBoundAcrossCoordination(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows may deny renaming a directory with an open AF_UNIX lifecycle handle")
	}
	home, err := os.MkdirTemp("/tmp", "zero-root-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_RUNTIME_DIR", "")
	paths, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(paths.Socket)
	moved := filepath.Join(home, "bound-runtime")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	launcher, _ := seqLauncher(&fakeWorker{pid: 1})
	srv := newTestServerWithPaths(t, launcher, paths)
	srv.opts.beforeSocketBind = func() {
		if err := os.Rename(dir, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	err = srv.Serve()
	if err == nil || !strings.Contains(err.Error(), "changed after it was secured") {
		t.Fatalf("Serve error = %v, want replaced-runtime rejection", err)
	}
	if _, err := os.Stat(filepath.Join(moved, filepath.Base(paths.Lock))); err != nil {
		t.Fatalf("rooted lock was not created in the bound directory: %v", err)
	}
	for _, path := range []string{paths.Lock, paths.Socket, paths.Status} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("substitute runtime entry %s was touched: %v", path, err)
		}
	}
}

func TestServeRemovesSocketBoundAfterFinalRuntimeSwap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows may deny renaming a directory with an open AF_UNIX lifecycle handle")
	}
	home, err := os.MkdirTemp("/tmp", "zero-bind-swap-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_RUNTIME_DIR", "")
	paths, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(paths.Socket)
	moved := filepath.Join(home, "trusted-runtime")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	launcher, _ := seqLauncher(&fakeWorker{pid: 1})
	srv := newTestServerWithPaths(t, launcher, paths)
	srv.opts.afterSocketPreflight = func() {
		if err := os.Rename(dir, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	err = srv.Serve()
	if err == nil || !strings.Contains(err.Error(), "changed after it was secured") {
		t.Fatalf("Serve error = %v, want post-bind runtime replacement rejection", err)
	}
	if _, err := os.Stat(filepath.Join(moved, filepath.Base(paths.Lock))); err != nil {
		t.Fatalf("rooted lock was not retained in the trusted directory: %v", err)
	}
	if _, err := os.Lstat(paths.Socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("substitute runtime retained stale socket: %v", err)
	}
	listener, err := net.Listen("unix", paths.Socket)
	if err != nil {
		t.Fatalf("substitute runtime remained unbindable after rollback: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRuntimeLogHardensDefaultRootBeforeOpen(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows DACL hardening has platform-specific coverage")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_RUNTIME_DIR", "")
	paths, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(paths.Socket)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	file, logPath, err := OpenRuntimeLog(paths)
	if err != nil {
		t.Fatalf("OpenRuntimeLog: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if logPath != filepath.Join(dir, "daemon.log") {
		t.Fatalf("log path = %q", logPath)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got&0o077 != 0 {
		t.Fatalf("default runtime directory permissions = %04o, want owner-only", got)
	}
}

func newTestServer(t *testing.T, launcher Launcher) (*Server, Paths) {
	t.Helper()
	dir := t.TempDir()
	secureStatusTestDir(t, dir)
	paths := Paths{
		Socket: filepath.Join(dir, "d.sock"),
		Lock:   filepath.Join(dir, "d.lock"),
		Status: filepath.Join(dir, "d.status"),
	}
	return newTestServerWithPaths(t, launcher, paths), paths
}

func newTestServerWithPaths(t *testing.T, launcher Launcher, paths Paths) *Server {
	t.Helper()
	pool, err := NewPool(PoolOptions{Size: 2, Launcher: launcher, KillTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	mgr, err := NewSessionManager(SessionManagerOptions{Pool: pool})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	srv, err := NewServer(ServerOptions{Paths: paths, Manager: mgr, Pool: pool})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv
}

func TestServerEndToEnd(t *testing.T) {
	out := []string{`{"type":"event","seq":1}`, `{"type":"event","seq":2}`}
	launcher, _ := seqLauncher(&fakeWorker{pid: 1, out: out, exitCode: 0})
	srv, paths := newTestServer(t, launcher)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()
	testutil.WaitFor(t, "file "+paths.Status, func() bool { _, err := os.Stat(paths.Status); return err == nil })

	// --- run a session and collect its stream-json output ---
	runClient, err := Dial(paths.Socket)
	if err != nil {
		t.Fatalf("Dial(run): %v", err)
	}
	var got []string
	if err := runClient.Run("s1", "", "hello", nil, func(line string) { got = append(got, line) }); err != nil {
		t.Fatalf("Run: %v", err)
	}
	runClient.Close()
	if len(got) != 2 || got[0] != out[0] || got[1] != out[1] {
		t.Fatalf("streamed lines = %v, want %v", got, out)
	}

	// --- status reflects the session ---
	statusClient, err := Dial(paths.Socket)
	if err != nil {
		t.Fatalf("Dial(status): %v", err)
	}
	report, err := statusClient.Status()
	statusClient.Close()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if report.PoolSize != 2 {
		t.Fatalf("status PoolSize = %d, want 2", report.PoolSize)
	}
	foundSession := false
	for _, s := range report.Sessions {
		if s.ID == "s1" {
			foundSession = true
			if s.State != string(SessionDone) {
				t.Fatalf("session s1 state = %s, want done", s.State)
			}
		}
	}
	if !foundSession {
		t.Fatalf("status missing session s1: %+v", report.Sessions)
	}

	// --- attach to the finished session sees the buffered history ---
	attachClient, err := Dial(paths.Socket)
	if err != nil {
		t.Fatalf("Dial(attach): %v", err)
	}
	var attached []string
	if err := attachClient.Attach("s1", func(line string) { attached = append(attached, line) }); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	attachClient.Close()
	if len(attached) != 2 {
		t.Fatalf("attach lines = %v, want 2 buffered", attached)
	}

	// --- graceful stop, daemon cleans up ---
	stopClient, err := Dial(paths.Socket)
	if err != nil {
		t.Fatalf("Dial(stop): %v", err)
	}
	if err := stopClient.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	stopClient.Close()

	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after shutdown")
	}
	// Runtime endpoints are removed on exit. The advisory lock file remains as
	// a stable inode, but the kernel lock on it has been released.
	for _, p := range []string{paths.Socket, paths.Status} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("file %s not cleaned up after shutdown: %v", p, err)
		}
	}
	if _, err := os.Stat(paths.Lock); err != nil {
		t.Fatalf("stable lock file missing after shutdown: %v", err)
	}
}

func TestServePreservesPreviousStatusWhenReplacementFails(t *testing.T) {
	launcher, _ := seqLauncher(&fakeWorker{pid: 1, exitCode: 0})
	dir, err := os.MkdirTemp("", "zero-daemon-status-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	secureStatusTestDir(t, dir)
	paths := Paths{Socket: filepath.Join(dir, "d.sock"), Lock: filepath.Join(dir, "d.lock"), Status: filepath.Join(dir, "d.status")}
	srv := newTestServerWithPaths(t, launcher, paths)
	previous := []byte(`{"pid":7,"socket":"previous","version":1}`)
	if err := os.WriteFile(paths.Status, previous, 0o600); err != nil {
		t.Fatal(err)
	}
	srv.opts.replaceStatusFile = func(*os.Root, string, string) error {
		return errors.New("injected replacement failure")
	}

	if err := srv.Serve(); err == nil || !strings.Contains(err.Error(), "injected replacement failure") {
		t.Fatal("Serve succeeded despite injected status replacement failure")
	}
	got, err := os.ReadFile(paths.Status)
	if err != nil {
		t.Fatalf("previous status was removed after failed publication: %v", err)
	}
	if string(got) != string(previous) {
		t.Fatalf("previous status changed after failed publication: %q", got)
	}
}

func TestServeCleansStatusThroughBoundDirectoryAfterSwap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not permit renaming the directory containing the live socket")
	}
	parent, err := os.MkdirTemp("", "zero-daemon-swap-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	dir := filepath.Join(parent, "live")
	movedDir := filepath.Join(parent, "moved")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	secureStatusTestDir(t, dir)
	paths := Paths{
		Socket: filepath.Join(dir, "d.sock"),
		Lock:   filepath.Join(dir, "d.lock"),
		Status: filepath.Join(dir, "d.status"),
	}
	launcher, _ := seqLauncher(&fakeWorker{pid: 1, exitCode: 0})
	srv := newTestServerWithPaths(t, launcher, paths)
	substitute := []byte(`{"pid":999,"socket":"substitute"}`)
	srv.opts.beforeStatusReplace = func() {
		if err := os.Rename(dir, movedDir); err != nil {
			t.Fatalf("move status directory: %v", err)
		}
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("create substitute directory: %v", err)
		}
		if err := os.WriteFile(paths.Status, substitute, 0o600); err != nil {
			t.Fatalf("write substitute status: %v", err)
		}
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()
	movedStatus := filepath.Join(movedDir, "d.status")
	testutil.WaitFor(t, "file "+movedStatus, func() bool { _, err := os.Stat(movedStatus); return err == nil })
	srv.Shutdown()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after shutdown")
	}
	if _, err := os.Stat(filepath.Join(movedDir, "d.status")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bound status survived cleanup: %v", err)
	}
	got, err := os.ReadFile(paths.Status)
	if err != nil {
		t.Fatalf("substitute status was removed by pathname cleanup: %v", err)
	}
	if string(got) != string(substitute) {
		t.Fatalf("substitute status changed during cleanup: %q", got)
	}
}

func TestServerPublishesDefaultStatusAfterCrashReportCreatesRuntimeDirectory(t *testing.T) {
	home, err := os.MkdirTemp("", "zero-home-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_RUNTIME_DIR", "")

	if _, err := observability.WriteCrashReport(
		observability.DefaultCrashDir(),
		"cli",
		"boom",
		[]byte("stack"),
		time.Now(),
	); err != nil {
		t.Fatalf("WriteCrashReport: %v", err)
	}
	paths, err := DefaultPaths()
	if err != nil {
		t.Fatalf("DefaultPaths: %v", err)
	}
	if paths.Status != filepath.Join(home, ".zero", "daemon.status") {
		t.Fatalf("default status path = %q, want path beneath temporary home", paths.Status)
	}

	launcher, _ := seqLauncher(&fakeWorker{pid: 1})
	srv := newTestServerWithPaths(t, launcher, paths)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()

	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		if _, err := os.Stat(paths.Status); err == nil {
			break
		}
		select {
		case err := <-serveErr:
			t.Fatalf("Serve returned before publishing status: %v", err)
		case <-deadline.C:
			t.Fatal("daemon did not publish its default status file")
		case <-time.After(2 * time.Millisecond):
		}
	}

	srv.Shutdown()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after shutdown")
	}
}

func TestServerSecondInstanceFails(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	launcher, _ := seqLauncher(&fakeWorker{pid: 1, waitCh: block})
	srv1, paths := newTestServer(t, launcher)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv1.Serve() }()
	testutil.WaitFor(t, "file "+paths.Status, func() bool { _, err := os.Stat(paths.Status); return err == nil })

	// A second server on the same paths must refuse to start (single instance).
	pool2, _ := NewPool(PoolOptions{Size: 1, Launcher: launcher})
	mgr2, _ := NewSessionManager(SessionManagerOptions{Pool: pool2})
	srv2, _ := NewServer(ServerOptions{Paths: paths, Manager: mgr2, Pool: pool2})
	if err := srv2.Serve(); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Serve err = %v, want ErrAlreadyRunning", err)
	}

	srv1.Shutdown()
	<-serveErr
}

func TestServerRejectsUnknownCommand(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	launcher, _ := seqLauncher(&fakeWorker{pid: 1, waitCh: block})
	srv, paths := newTestServer(t, launcher)
	go func() { _ = srv.Serve() }()
	defer srv.Shutdown()
	testutil.WaitFor(t, "file "+paths.Status, func() bool { _, err := os.Stat(paths.Status); return err == nil })

	client, err := Dial(paths.Socket)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()
	// Run with an empty session id is rejected with an error frame.
	err = client.Run("", "", "hi", nil, nil)
	if err == nil {
		t.Fatal("run with empty session id must return an error")
	}
}

func TestServerShutdownMakesRetryDelaysDrainTerminal(t *testing.T) {
	cases := []struct {
		name        string
		exitCode    int
		maxAttempts int
	}{
		{name: "backoff", exitCode: 1, maxAttempts: 2},
		{name: "tempfail", exitCode: ExitTempfail, maxAttempts: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			delaying := make(chan struct{})
			pool, err := NewPool(PoolOptions{
				Size:          1,
				MaxAttempts:   tc.maxAttempts,
				KillTimeout:   20 * time.Millisecond,
				TempfailDelay: time.Hour,
				Backoff:       func(int) time.Duration { return time.Hour },
				Log: func(message string) {
					if strings.Contains(message, "retry after") || strings.Contains(message, "restart") {
						select {
						case <-delaying:
						default:
							close(delaying)
						}
					}
				},
				Launcher: func(context.Context, WorkerSpec) (WorkerHandle, error) {
					return &fakeWorker{pid: 1, exitCode: tc.exitCode}, nil
				},
			})
			if err != nil {
				t.Fatalf("NewPool: %v", err)
			}
			mgr, err := NewSessionManager(SessionManagerOptions{Pool: pool})
			if err != nil {
				t.Fatalf("NewSessionManager: %v", err)
			}
			dir := t.TempDir()
			srv, err := NewServer(ServerOptions{
				Paths:   Paths{Socket: filepath.Join(dir, "d.sock"), Lock: filepath.Join(dir, "d.lock"), Status: filepath.Join(dir, "d.status")},
				Manager: mgr,
				Pool:    pool,
			})
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}
			sess, err := mgr.Start(srv.ctx, WorkerSpec{Session: "a"})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			select {
			case <-delaying:
			case <-time.After(2 * time.Second):
				t.Fatal("session did not enter retry delay")
			}
			srv.Shutdown()
			select {
			case <-sess.Done():
				if !errors.Is(sess.Err(), ErrPoolDraining) {
					t.Fatalf("session error = %v, want ErrPoolDraining", sess.Err())
				}
			case <-time.After(2 * time.Second):
				t.Fatal("session did not finish after shutdown")
			}
		})
	}
}
