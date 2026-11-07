package remote

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/daemon"
)

// ---- fake daemon worker (the daemon's test fakes are unexported) -------------

type fakeLines struct{ ch <-chan string }

func (l fakeLines) Next() (string, bool, error) {
	line, ok := <-l.ch
	return line, ok, nil
}

type fakeWorker struct {
	ch  chan string
	pid int
}

func (w *fakeWorker) Stdout() daemon.Lines { return fakeLines{ch: w.ch} }
func (w *fakeWorker) Wait() (int, error)   { return 0, nil }
func (w *fakeWorker) Kill() error          { return nil }
func (w *fakeWorker) Pid() int             { return w.pid }

// staticLauncher emits the given lines then ends the stream.
func staticLauncher(lines ...string) daemon.Launcher {
	return func(_ context.Context, _ daemon.WorkerSpec) (daemon.WorkerHandle, error) {
		ch := make(chan string, len(lines))
		for _, l := range lines {
			ch <- l
		}
		close(ch)
		return &fakeWorker{ch: ch, pid: 1}, nil
	}
}

// blockingLauncher returns a worker whose stream stays open until release is
// closed, so a session (and its bridge connection slot) is held open.
func blockingLauncher(release <-chan struct{}) daemon.Launcher {
	return func(_ context.Context, _ daemon.WorkerSpec) (daemon.WorkerHandle, error) {
		ch := make(chan string)
		go func() {
			<-release
			close(ch)
		}()
		return &fakeWorker{ch: ch, pid: 1}, nil
	}
}

func newBridgeServer(t *testing.T, launcher daemon.Launcher) *daemon.Server {
	t.Helper()
	dir := t.TempDir()
	pool, err := daemon.NewPool(daemon.PoolOptions{Size: 4, Launcher: launcher, KillTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	mgr, err := daemon.NewSessionManager(daemon.SessionManagerOptions{Pool: pool})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	srv, err := daemon.NewServer(daemon.ServerOptions{
		Paths:   daemon.Paths{Socket: filepath.Join(dir, "s.sock"), Lock: filepath.Join(dir, "s.lock"), Status: filepath.Join(dir, "s.status")},
		Manager: mgr,
		Pool:    pool,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(srv.Shutdown)
	return srv
}

// genTestCert writes a self-signed cert/key (valid for 127.0.0.1) to temp files.
func genTestCert(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "zero-remote-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         true,
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	certOut, _ := os.Create(certFile)
	_ = pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	_ = certOut.Close()
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyOut, _ := os.Create(keyFile)
	_ = pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	_ = keyOut.Close()
	return certFile, keyFile
}

// startBridge starts a TLS bridge on 127.0.0.1:0 and returns its address + cert.
func startBridge(t *testing.T, srv *daemon.Server, opts BridgeOptions) (addr, caCert string) {
	t.Helper()
	certFile, keyFile := genTestCert(t)
	tlsConfig, err := ServerTLSConfig(certFile, keyFile)
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}
	opts.Server = srv
	bridge, err := NewBridge(opts)
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	tlsLn := tls.NewListener(ln, tlsConfig)
	go func() { _ = bridge.Serve(tlsLn) }()
	t.Cleanup(func() { _ = tlsLn.Close() })
	return ln.Addr().String(), certFile
}

func TestBridgeRunStatusRoundTrip(t *testing.T) {
	srv := newBridgeServer(t, staticLauncher(`{"type":"event","seq":1}`, `{"type":"event","seq":2}`))
	auth, _ := NewTokenAuthenticator("tok")
	addr, ca := startBridge(t, srv, BridgeOptions{Authenticator: auth})

	client, err := DialRemote(RemoteConfig{Address: addr, Token: "tok", CACertFile: ca})
	if err != nil {
		t.Fatalf("DialRemote: %v", err)
	}
	var got []string
	if err := client.Run("sess-1", "", "hello", nil, func(l string) { got = append(got, l) }); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = client.Close()
	if len(got) != 2 || got[0] != `{"type":"event","seq":1}` {
		t.Fatalf("run output = %v", got)
	}

	// Status over a fresh remote connection (same protocol as local).
	statusClient, err := DialRemote(RemoteConfig{Address: addr, Token: "tok", CACertFile: ca})
	if err != nil {
		t.Fatalf("DialRemote(status): %v", err)
	}
	report, err := statusClient.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	_ = statusClient.Close()
	if report == nil || report.PID == 0 {
		t.Fatalf("status report = %+v", report)
	}
}

func TestBridgeRejectsBadToken(t *testing.T) {
	srv := newBridgeServer(t, staticLauncher())
	auth, _ := NewTokenAuthenticator("correct")
	addr, ca := startBridge(t, srv, BridgeOptions{Authenticator: auth, AuthFailDelay: -1})

	_, err := DialRemote(RemoteConfig{Address: addr, Token: "wrong", CACertFile: ca})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("DialRemote(bad token) err = %v, want ErrUnauthorized", err)
	}
}

func TestBridgeRejectsLowVersion(t *testing.T) {
	srv := newBridgeServer(t, staticLauncher())
	auth, _ := NewTokenAuthenticator("tok")
	// Require a version higher than the client speaks (daemon.ProtoVersion).
	addr, ca := startBridge(t, srv, BridgeOptions{Authenticator: auth, MinVersion: daemon.ProtoVersion + 1, AuthFailDelay: -1})

	_, err := DialRemote(RemoteConfig{Address: addr, Token: "tok", CACertFile: ca})
	if err == nil {
		t.Fatal("DialRemote with a below-min version must be rejected")
	}
}

func TestBridgeRequiresVerifiableCert(t *testing.T) {
	srv := newBridgeServer(t, staticLauncher())
	auth, _ := NewTokenAuthenticator("tok")
	addr, _ := startBridge(t, srv, BridgeOptions{Authenticator: auth})
	// No CA configured + a self-signed server cert => verification must fail
	// (NEVER InsecureSkipVerify).
	if _, err := DialRemote(RemoteConfig{Address: addr, Token: "tok"}); err == nil {
		t.Fatal("DialRemote must reject an untrusted self-signed cert")
	}
}

func TestListenAndServeTLSRequiresConfig(t *testing.T) {
	srv := newBridgeServer(t, staticLauncher())
	auth, _ := NewTokenAuthenticator("tok")
	bridge, _ := NewBridge(BridgeOptions{Server: srv, Authenticator: auth})
	if err := bridge.ListenAndServeTLS("127.0.0.1:0", nil); err == nil {
		t.Fatal("ListenAndServeTLS(nil) must refuse to serve plaintext")
	}
}

func TestListenAndServeTLSServesAndCloses(t *testing.T) {
	srv := newBridgeServer(t, staticLauncher(`{"type":"event","seq":1}`))
	auth, _ := NewTokenAuthenticator("tok")
	certFile, keyFile := genTestCert(t)
	tlsConfig, err := ServerTLSConfig(certFile, keyFile)
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}
	bridge, err := NewBridge(BridgeOptions{Server: srv, Authenticator: auth})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	// Bind an ephemeral port, discover it, then serve via ListenAndServeTLS by
	// passing the same addr (it rebinds) — instead we bind here to learn the port
	// and serve through the public entry on that exact address.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	serveErr := make(chan error, 1)
	go func() { serveErr <- bridge.ListenAndServeTLS(addr, tlsConfig) }()

	// Wait until it accepts a connection, then drive one run.
	var client *daemon.Client
	deadline := time.Now().Add(2 * time.Second)
	for {
		client, err = DialRemote(RemoteConfig{Address: addr, Token: "tok", CACertFile: certFile, Timeout: 300 * time.Millisecond})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("bridge never came up: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var got []string
	if err := client.Run("s", "", "hi", nil, func(l string) { got = append(got, l) }); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = client.Close()
	if len(got) != 1 {
		t.Fatalf("got %d lines, want 1", len(got))
	}
	// Close stops the accept loop; a second Close is a no-op (not an error).
	if err := bridge.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := bridge.Close(); err != nil {
		t.Fatalf("second Close should be a no-op, got: %v", err)
	}
	select {
	case <-serveErr: // ListenAndServeTLS returned after Close
	case <-time.After(2 * time.Second):
		t.Fatal("ListenAndServeTLS did not return after Close")
	}
}

func TestBridgeResumeAfterReconnect(t *testing.T) {
	srv := newBridgeServer(t, staticLauncher(`{"type":"event","seq":1}`, `{"type":"event","seq":2}`))
	auth, _ := NewTokenAuthenticator("tok")
	addr, ca := startBridge(t, srv, BridgeOptions{Authenticator: auth})

	// First connection runs the session to completion.
	c1, err := DialRemote(RemoteConfig{Address: addr, Token: "tok", CACertFile: ca})
	if err != nil {
		t.Fatalf("DialRemote 1: %v", err)
	}
	if err := c1.Run("sess-resume", "", "hi", nil, func(string) {}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = c1.Close()

	// A new connection re-attaches and replays the buffered session output —
	// the dropped first connection did not lose the session.
	c2, err := DialRemote(RemoteConfig{Address: addr, Token: "tok", CACertFile: ca})
	if err != nil {
		t.Fatalf("DialRemote 2: %v", err)
	}
	defer c2.Close()
	var replayed []string
	if err := c2.Attach("sess-resume", func(l string) { replayed = append(replayed, l) }); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if len(replayed) != 2 {
		t.Fatalf("re-attach replayed %d lines, want 2", len(replayed))
	}
}

func TestBridgeConnectionCap(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv := newBridgeServer(t, blockingLauncher(release))
	auth, _ := NewTokenAuthenticator("tok")
	addr, ca := startBridge(t, srv, BridgeOptions{Authenticator: auth, MaxConnections: 1})

	// First connection holds the only slot with a blocking run.
	c1, err := DialRemote(RemoteConfig{Address: addr, Token: "tok", CACertFile: ca})
	if err != nil {
		t.Fatalf("DialRemote 1: %v", err)
	}
	defer c1.Close()
	runErr := make(chan error, 1)
	go func() { runErr <- c1.Run("sess-block", "", "hi", nil, func(string) {}) }()

	// Wait until the slot is in use, then a second dial must be refused.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c2, err := DialRemote(RemoteConfig{Address: addr, Token: "tok", CACertFile: ca, Timeout: 500 * time.Millisecond})
		if err != nil {
			return // refused as expected
		}
		_ = c2.Close()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("second connection was not refused at the connection cap")
}
