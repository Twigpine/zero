package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Gitlawb/zero/internal/background"
	"github.com/Gitlawb/zero/internal/daemon"
	"github.com/Gitlawb/zero/internal/daemon/remote"
)

// runDaemon dispatches the `zero daemon ...` subcommands. The daemon supervises a
// pool of headless `zero exec` workers and routes sessions to them over an
// owner-only local control socket. It is an ADDITIVE surface — interactive and
// one-shot exec are unchanged.
func runDaemon(args []string, stdout io.Writer, stderr io.Writer, _ appDeps) int {
	if len(args) == 0 {
		return writeDaemonUsage(stderr, exitUsage)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "start":
		return runDaemonStart(rest, stdout, stderr)
	case "stop":
		return runDaemonStop(rest, stdout, stderr)
	case "status":
		return runDaemonStatus(rest, stdout, stderr)
	case "run":
		return runDaemonRun(rest, stdout, stderr)
	case "attach":
		return runDaemonAttach(rest, stdout, stderr)
	case "serve-remote":
		return runDaemonServeRemote(rest, stdout, stderr)
	case "link":
		return runDaemonLink(rest, stdout, stderr)
	case "-h", "--help", "help":
		return writeDaemonUsage(stdout, exitSuccess)
	default:
		fmt.Fprintf(stderr, "unknown daemon subcommand %q\n", sub)
		return writeDaemonUsage(stderr, exitUsage)
	}
}

func writeDaemonUsage(w io.Writer, code int) int {
	fmt.Fprint(w, `Usage: zero daemon <command>

Commands:
  start [--foreground]      Start the daemon (background by default).
  stop                      Gracefully stop the running daemon.
  status                    Show daemon / pool / session status.
  run --session <id> [--cwd <dir>] [--prompt <text>] [exec flags...]
                            Create/route a session and stream its output.
  attach <session>          Attach to a running session's stream.
  serve-remote --addr <host:port> --tls-cert <f> --tls-key <f> [--bundle-dir <d>]
                            Serve an opt-in, TLS-only network bridge to this
                            daemon. Requires a bearer token in $ZERO_DAEMON_REMOTE_TOKEN
                            (or $ZERO_DAEMON_REMOTE_TOKEN_FILE). --bundle-dir enables
                            git-bundle uploads, extracted into per-link work trees.
  link --remote <host:port> --repo <dir> --id <name> [--out <file>]
                            Upload repo's git history to the remote as a bundle and
                            print the extracted remote path. --out saves a session
                            link file (0600). Accepts --token/--ca-cert/--server-name.
  link --show <file>        Print a saved session link.

run and attach accept --remote <host:port> [--token <t>] [--ca-cert <f>]
[--server-name <name>] to drive a remote daemon over the bridge instead of the
local socket. Use the link's remote path as --cwd to run against a linked repo.
`)
	return code
}

// runDaemonStart starts the daemon. Without --foreground it spawns a detached
// background process running the foreground daemon and returns once it is up.
func runDaemonStart(args []string, stdout io.Writer, stderr io.Writer) int {
	foreground := false
	for _, a := range args {
		switch a {
		case "--foreground", "-f":
			foreground = true
		case "-h", "--help":
			return writeDaemonUsage(stdout, exitSuccess)
		default:
			return writeExecUsageError(stderr, fmt.Sprintf("unknown flag %q for daemon start", a))
		}
	}
	paths, err := daemon.DefaultPaths()
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	if foreground {
		return runDaemonForeground(paths, stdout, stderr)
	}
	return runDaemonStartDetached(paths, stdout, stderr)
}

// runDaemonForeground runs the daemon in this process until SIGINT/SIGTERM. Each
// worker is a headless `zero exec` child with the sandbox re-entrancy markers
// scrubbed (NewExecLauncher) so it establishes its own sandbox.
func runDaemonForeground(paths daemon.Paths, stdout io.Writer, stderr io.Writer) int {
	launcher, err := daemon.NewExecLauncher(daemon.ExecLauncherConfig{})
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	logf := func(line string) { fmt.Fprintln(stderr, "[daemon] "+line) }
	pool, err := daemon.NewPool(daemon.PoolOptions{Launcher: launcher, Log: logf})
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	mgr, err := daemon.NewSessionManager(daemon.SessionManagerOptions{Pool: pool})
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	srv, err := daemon.NewServer(daemon.ServerOptions{Paths: paths, Manager: mgr, Pool: pool, Log: logf})
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		srv.Shutdown()
	}()

	if err := srv.Serve(); err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	return exitSuccess
}

// runDaemonStartDetached spawns the foreground daemon as a detached background
// process (its own process group, output to a log file) and waits for it to bind.
func runDaemonStartDetached(paths daemon.Paths, stdout io.Writer, stderr io.Writer) int {
	if daemonReachable(paths) {
		fmt.Fprintln(stdout, "zero daemon is already running")
		return exitSuccess
	}
	exe, err := os.Executable()
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	logFile, logPath, err := daemon.OpenRuntimeLog(paths)
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	defer logFile.Close()

	cmd := exec.Command(exe, "daemon", "start", "--foreground")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	background.ConfigureChildProcessGroup(cmd) // own process group: outlives this shell
	if err := startAndAwaitDaemonProcess(cmd, func() bool { return daemonReachable(paths) }, 5*time.Second, 25*time.Millisecond); err != nil {
		if errors.Is(err, errDaemonStartTimeout) {
			return writeAppError(stderr, err.Error()+"; see "+logPath, exitCrash)
		}
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	fmt.Fprintf(stdout, "zero daemon started (socket %s)\n", paths.Socket)
	return exitSuccess
}

var errDaemonStartTimeout = errors.New("daemon did not come up within timeout")

func startAndAwaitDaemonProcess(cmd *exec.Cmd, reachable func() bool, timeout time.Duration, pollInterval time.Duration) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	if waitForDaemonReadiness(reachable, timeout, pollInterval) {
		if err := cmd.Process.Release(); err != nil {
			cleanupErr := terminateAndReapDaemonProcess(cmd)
			if cleanupErr != nil {
				return fmt.Errorf("release daemon process: %w (cleanup failed: %v)", err, cleanupErr)
			}
			return fmt.Errorf("release daemon process: %w", err)
		}
		return nil
	}
	if err := terminateAndReapDaemonProcess(cmd); err != nil {
		return fmt.Errorf("%w; cleanup failed: %v", errDaemonStartTimeout, err)
	}
	return errDaemonStartTimeout
}

func waitForDaemonReadiness(reachable func() bool, timeout time.Duration, pollInterval time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if reachable() {
			return true
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		time.Sleep(min(pollInterval, remaining))
	}
	// The loop's last reachable() call already ran immediately before this one
	// (right after the final Sleep, with the same deadline check separating
	// them), so this adds no extra waiting time and is not a grace window for a
	// slow-starting daemon — it only re-checks the same instant in case
	// reachable() itself is non-deterministic. To tolerate a genuinely slow
	// start, raise the timeout passed to startAndAwaitDaemonProcess instead.
	return reachable()
}

func terminateAndReapDaemonProcess(cmd *exec.Cmd) error {
	return background.TerminateCommand(cmd)
}

func runDaemonStop(args []string, stdout io.Writer, stderr io.Writer) int {
	if helpRequested(args) {
		return writeDaemonUsage(stdout, exitSuccess)
	}
	if len(args) > 0 {
		return writeExecUsageError(stderr, "daemon stop takes no arguments")
	}
	paths, err := daemon.DefaultPaths()
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	client, err := daemon.Dial(paths.Socket)
	if err != nil {
		fmt.Fprintln(stdout, "zero daemon is not running")
		return exitSuccess
	}
	defer client.Close()
	if err := client.Shutdown(); err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	fmt.Fprintln(stdout, "zero daemon stopped")
	return exitSuccess
}

func runDaemonStatus(args []string, stdout io.Writer, stderr io.Writer) int {
	if helpRequested(args) {
		return writeDaemonUsage(stdout, exitSuccess)
	}
	if len(args) > 0 {
		return writeExecUsageError(stderr, "daemon status takes no arguments")
	}
	paths, err := daemon.DefaultPaths()
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	client, err := daemon.Dial(paths.Socket)
	if err != nil {
		fmt.Fprintln(stdout, "zero daemon is not running")
		return exitSuccess
	}
	defer client.Close()
	report, err := client.Status()
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	fmt.Fprintf(stdout, "daemon pid=%d version=%d socket=%s\n", report.PID, report.Version, report.Socket)
	fmt.Fprintf(stdout, "pool size=%d busy=%d queue=%d\n", report.PoolSize, len(report.Workers), report.QueueDepth)
	for _, s := range report.Sessions {
		fmt.Fprintf(stdout, "  session %s state=%s lines=%d\n", s.ID, s.State, s.Lines)
	}
	return exitSuccess
}

func runDaemonRun(args []string, stdout io.Writer, stderr io.Writer) int {
	session, cwd, prompt := "", "", ""
	var remoteFlags remoteDialFlags
	var forward []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		value := func() (string, bool) {
			if i+1 >= len(args) {
				return "", false
			}
			i++
			return args[i], true
		}
		switch {
		case a == "-h" || a == "--help":
			return writeDaemonUsage(stdout, exitSuccess)
		case a == "--remote":
			v, ok := value()
			if !ok {
				return writeExecUsageError(stderr, "--remote requires a value")
			}
			remoteFlags.Addr = v
		case strings.HasPrefix(a, "--remote="):
			remoteFlags.Addr = strings.TrimPrefix(a, "--remote=")
		case a == "--token":
			v, ok := value()
			if !ok {
				return writeExecUsageError(stderr, "--token requires a value")
			}
			remoteFlags.Token = v
		case strings.HasPrefix(a, "--token="):
			remoteFlags.Token = strings.TrimPrefix(a, "--token=")
		case a == "--ca-cert":
			v, ok := value()
			if !ok {
				return writeExecUsageError(stderr, "--ca-cert requires a value")
			}
			remoteFlags.CACert = v
		case strings.HasPrefix(a, "--ca-cert="):
			remoteFlags.CACert = strings.TrimPrefix(a, "--ca-cert=")
		case a == "--server-name":
			v, ok := value()
			if !ok {
				return writeExecUsageError(stderr, "--server-name requires a value")
			}
			remoteFlags.ServerName = v
		case strings.HasPrefix(a, "--server-name="):
			remoteFlags.ServerName = strings.TrimPrefix(a, "--server-name=")
		case a == "--session":
			v, ok := value()
			if !ok {
				return writeExecUsageError(stderr, "--session requires a value")
			}
			session = v
		case strings.HasPrefix(a, "--session="):
			session = strings.TrimPrefix(a, "--session=")
		case a == "--cwd":
			v, ok := value()
			if !ok {
				return writeExecUsageError(stderr, "--cwd requires a value")
			}
			cwd = v
		case strings.HasPrefix(a, "--cwd="):
			cwd = strings.TrimPrefix(a, "--cwd=")
		case a == "--prompt" || a == "-p":
			v, ok := value()
			if !ok {
				return writeExecUsageError(stderr, "--prompt requires a value")
			}
			prompt = v
		case strings.HasPrefix(a, "--prompt="):
			prompt = strings.TrimPrefix(a, "--prompt=")
		default:
			// Forwarded verbatim to the worker `zero exec` (reuses exec's own flag
			// parsing for per-session run options).
			forward = append(forward, a)
		}
	}
	if strings.TrimSpace(session) == "" {
		return writeExecUsageError(stderr, "daemon run requires --session <id>")
	}
	if prompt == "" && len(forward) == 0 {
		return writeExecUsageError(stderr, "daemon run requires --prompt <text> or exec args")
	}
	client, err := dialForCLI(remoteFlags)
	if err != nil {
		return writeAppError(stderr, daemonDialError(remoteFlags, err), exitCrash)
	}
	defer client.Close()
	if err := client.Run(session, cwd, prompt, forward, func(line string) { fmt.Fprintln(stdout, line) }); err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	return exitSuccess
}

func runDaemonAttach(args []string, stdout io.Writer, stderr io.Writer) int {
	session := ""
	var remoteFlags remoteDialFlags
	extra := 0
	for i := 0; i < len(args); i++ {
		a := args[i]
		value := func() (string, bool) {
			if i+1 >= len(args) {
				return "", false
			}
			i++
			return args[i], true
		}
		switch {
		case a == "-h" || a == "--help":
			return writeDaemonUsage(stdout, exitSuccess)
		case a == "--remote":
			v, ok := value()
			if !ok {
				return writeExecUsageError(stderr, "--remote requires a value")
			}
			remoteFlags.Addr = v
		case strings.HasPrefix(a, "--remote="):
			remoteFlags.Addr = strings.TrimPrefix(a, "--remote=")
		case a == "--token":
			v, ok := value()
			if !ok {
				return writeExecUsageError(stderr, "--token requires a value")
			}
			remoteFlags.Token = v
		case strings.HasPrefix(a, "--token="):
			remoteFlags.Token = strings.TrimPrefix(a, "--token=")
		case a == "--ca-cert":
			v, ok := value()
			if !ok {
				return writeExecUsageError(stderr, "--ca-cert requires a value")
			}
			remoteFlags.CACert = v
		case strings.HasPrefix(a, "--ca-cert="):
			remoteFlags.CACert = strings.TrimPrefix(a, "--ca-cert=")
		case a == "--server-name":
			v, ok := value()
			if !ok {
				return writeExecUsageError(stderr, "--server-name requires a value")
			}
			remoteFlags.ServerName = v
		case strings.HasPrefix(a, "--server-name="):
			remoteFlags.ServerName = strings.TrimPrefix(a, "--server-name=")
		case session == "":
			session = a
		default:
			extra++
		}
	}
	if strings.TrimSpace(session) == "" {
		return writeExecUsageError(stderr, "daemon attach requires a <session> id")
	}
	if extra > 0 {
		return writeExecUsageError(stderr, "daemon attach accepts exactly one <session> id")
	}
	client, err := dialForCLI(remoteFlags)
	if err != nil {
		return writeAppError(stderr, daemonDialError(remoteFlags, err), exitCrash)
	}
	defer client.Close()
	if err := client.Attach(session, func(line string) { fmt.Fprintln(stdout, line) }); err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	return exitSuccess
}

// daemonDialError tailors the connect-failure message for local vs remote.
func daemonDialError(flags remoteDialFlags, err error) string {
	if strings.TrimSpace(flags.Addr) != "" {
		return "failed to reach remote daemon at " + flags.Addr + ": " + err.Error()
	}
	return "zero daemon is not running (start it with `zero daemon start`)"
}

// runDaemonServeRemote starts the local daemon plus an opt-in, TLS-only network
// bridge. TLS and a bearer token are mandatory (fail closed): it refuses to
// start without a cert/key pair and a token from the environment.
func runDaemonServeRemote(args []string, stdout io.Writer, stderr io.Writer) int {
	addr, certFile, keyFile, bundleDir := "", "", "", ""
	minVersion, maxConns := 0, 0
	for i := 0; i < len(args); i++ {
		a := args[i]
		value := func() (string, bool) {
			if i+1 >= len(args) {
				return "", false
			}
			i++
			return args[i], true
		}
		switch {
		case a == "-h" || a == "--help":
			return writeDaemonUsage(stdout, exitSuccess)
		case a == "--addr":
			v, ok := value()
			if !ok {
				return writeExecUsageError(stderr, "--addr requires a value")
			}
			addr = v
		case strings.HasPrefix(a, "--addr="):
			addr = strings.TrimPrefix(a, "--addr=")
		case a == "--tls-cert":
			v, ok := value()
			if !ok {
				return writeExecUsageError(stderr, "--tls-cert requires a value")
			}
			certFile = v
		case strings.HasPrefix(a, "--tls-cert="):
			certFile = strings.TrimPrefix(a, "--tls-cert=")
		case a == "--tls-key":
			v, ok := value()
			if !ok {
				return writeExecUsageError(stderr, "--tls-key requires a value")
			}
			keyFile = v
		case strings.HasPrefix(a, "--tls-key="):
			keyFile = strings.TrimPrefix(a, "--tls-key=")
		case a == "--min-version":
			v, ok := value()
			if !ok {
				return writeExecUsageError(stderr, "--min-version requires a value")
			}
			minVersion = atoiOrZero(v)
		case a == "--max-conns":
			v, ok := value()
			if !ok {
				return writeExecUsageError(stderr, "--max-conns requires a value")
			}
			maxConns = atoiOrZero(v)
		case a == "--bundle-dir":
			v, ok := value()
			if !ok {
				return writeExecUsageError(stderr, "--bundle-dir requires a value")
			}
			bundleDir = v
		case strings.HasPrefix(a, "--bundle-dir="):
			bundleDir = strings.TrimPrefix(a, "--bundle-dir=")
		default:
			return writeExecUsageError(stderr, fmt.Sprintf("unknown flag %q for daemon serve-remote", a))
		}
	}
	if strings.TrimSpace(addr) == "" {
		return writeExecUsageError(stderr, "daemon serve-remote requires --addr <host:port>")
	}
	// Fail closed: TLS cert/key + a bearer token are mandatory.
	tlsConfig, err := remote.ServerTLSConfig(certFile, keyFile)
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	token, err := remote.TokenFromEnv()
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	auth, err := remote.NewTokenAuthenticator(token)
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	paths, err := daemon.DefaultPaths()
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	launcher, err := daemon.NewExecLauncher(daemon.ExecLauncherConfig{})
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	logf := func(line string) { fmt.Fprintln(stderr, "[daemon] "+line) }
	pool, err := daemon.NewPool(daemon.PoolOptions{Launcher: launcher, Log: logf})
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	mgr, err := daemon.NewSessionManager(daemon.SessionManagerOptions{Pool: pool})
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	srv, err := daemon.NewServer(daemon.ServerOptions{Paths: paths, Manager: mgr, Pool: pool, Log: logf})
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	bridge, err := remote.NewBridge(remote.BridgeOptions{
		Server: srv, Authenticator: auth, MinVersion: minVersion, MaxConnections: maxConns, BundleDir: bundleDir, Log: logf,
	})
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}

	// Serve the local control socket too, so local clients keep working.
	go func() {
		if serveErr := srv.Serve(); serveErr != nil {
			logf("local serve error: " + serveErr.Error())
		}
	}()

	serveErr := make(chan error, 1)
	go func() { serveErr <- bridge.ListenAndServeTLS(addr, tlsConfig) }()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	fmt.Fprintf(stdout, "zero daemon remote bridge listening on %s (TLS)\n", addr)
	select {
	case <-sigCh:
		srv.Shutdown()
		_ = bridge.Close()
		<-serveErr // wait for the accept loop to unwind
		return exitSuccess
	case err := <-serveErr:
		// Bind/serve failed before any signal (e.g. address in use).
		srv.Shutdown()
		return writeAppError(stderr, err.Error(), exitCrash)
	}
}

// runDaemonLink uploads a repo's git history to a remote bridge as a bundle
// (link without --show), or prints a saved session link (--show <file>).
func runDaemonLink(args []string, stdout io.Writer, stderr io.Writer) int {
	var addr, repo, id, token, caCert, serverName, out, show string
	// flags maps each "--name" to the string it sets; both "--name v" and
	// "--name=v" forms are accepted.
	flags := map[string]*string{
		"--remote": &addr, "--repo": &repo, "--id": &id, "--token": &token,
		"--ca-cert": &caCert, "--server-name": &serverName, "--out": &out, "--show": &show,
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-h" || a == "--help" {
			return writeDaemonUsage(stdout, exitSuccess)
		}
		name, inlineVal, hasInline := a, "", false
		if eq := strings.IndexByte(a, '='); eq >= 0 {
			name, inlineVal, hasInline = a[:eq], a[eq+1:], true
		}
		dst, ok := flags[name]
		if !ok {
			return writeExecUsageError(stderr, fmt.Sprintf("unknown flag %q for daemon link", a))
		}
		if hasInline {
			*dst = inlineVal
			continue
		}
		if i+1 >= len(args) {
			return writeExecUsageError(stderr, fmt.Sprintf("%s requires a value", name))
		}
		i++
		*dst = args[i]
	}

	if strings.TrimSpace(show) != "" {
		link, err := remote.LoadSessionLink(show)
		if err != nil {
			return writeAppError(stderr, err.Error(), exitCrash)
		}
		fmt.Fprintf(stdout, "link %s -> %s on %s\n", link.LinkID, link.RemotePath, link.Address)
		return exitSuccess
	}

	if strings.TrimSpace(addr) == "" || strings.TrimSpace(repo) == "" || strings.TrimSpace(id) == "" {
		return writeExecUsageError(stderr, "daemon link requires --remote, --repo, and --id (or --show <file>)")
	}
	if strings.TrimSpace(token) == "" {
		token, _ = remote.TokenFromEnv() // best effort; UploadRepoBundle rejects an empty token
	}
	link, err := remote.UploadRepoBundle(remote.RemoteConfig{
		Address:    addr,
		Token:      token,
		CACertFile: caCert,
		ServerName: serverName,
	}, repo, id)
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	fmt.Fprintf(stdout, "uploaded %s; remote repo at %s\n", link.LinkID, link.RemotePath)
	fmt.Fprintf(stdout, "run it with: zero daemon run --remote %s --cwd %s ...\n", link.Address, link.RemotePath)
	if strings.TrimSpace(out) != "" {
		if err := link.Save(out); err != nil {
			return writeAppError(stderr, err.Error(), exitCrash)
		}
		fmt.Fprintf(stdout, "saved session link to %s\n", out)
	}
	return exitSuccess
}

// remoteDialFlags holds the optional flags that redirect run/attach to a remote
// daemon. When Addr is empty the local control socket is used.
type remoteDialFlags struct {
	Addr       string
	Token      string
	CACert     string
	ServerName string
}

// dialForCLI returns a daemon.Client for either the local socket (Addr empty) or
// a remote bridge (Addr set). For remote, the token falls back to the env.
func dialForCLI(flags remoteDialFlags) (*daemon.Client, error) {
	if strings.TrimSpace(flags.Addr) == "" {
		paths, err := daemon.DefaultPaths()
		if err != nil {
			return nil, err
		}
		return daemon.Dial(paths.Socket)
	}
	token := strings.TrimSpace(flags.Token)
	if token == "" {
		token, _ = remote.TokenFromEnv() // best effort; DialRemote rejects an empty token
	}
	return remote.DialRemote(remote.RemoteConfig{
		Address:    flags.Addr,
		Token:      token,
		CACertFile: flags.CACert,
		ServerName: flags.ServerName,
	})
}

func atoiOrZero(s string) int {
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// daemonReachable reports whether a daemon is accepting connections (a successful
// handshake). Used for the single-instance check and start-up wait.
func daemonReachable(paths daemon.Paths) bool {
	client, err := daemon.Dial(paths.Socket)
	if err != nil {
		return false
	}
	_ = client.Close()
	return true
}

func helpRequested(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			return true
		}
	}
	return false
}
