package remote

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/Gitlawb/zero/internal/daemon"
)

const (
	defaultMaxConnections   = 32
	defaultHandshakeTimeout = 10 * time.Second
	defaultAuthFailDelay    = 250 * time.Millisecond
	// defaultMaxBundleBytes caps a single uploaded git bundle (256 MiB) so a
	// remote peer cannot exhaust the host's disk.
	defaultMaxBundleBytes = 256 << 20
)

// Bridge serves authenticated remote connections and drives the local daemon's
// control dispatch for each one.
type Bridge struct {
	server           *daemon.Server
	auth             Authenticator
	attest           Attestation
	minVersion       int
	handshakeTimeout time.Duration
	authFailDelay    time.Duration
	bundleDir        string
	maxBundleBytes   int64
	log              func(string)
	sem              chan struct{}

	mu       sync.Mutex
	listener net.Listener
}

// BridgeOptions configures a Bridge.
type BridgeOptions struct {
	// Server is the local daemon to drive (required).
	Server *daemon.Server
	// Authenticator verifies bearer tokens (required).
	Authenticator Authenticator
	// Attestation is an optional post-token hook; nil => no-op.
	Attestation Attestation
	// MinVersion is the lowest control-protocol version accepted; 0 =>
	// daemon.ProtoVersion.
	MinVersion int
	// MaxConnections bounds concurrent remote connections; 0 => default.
	MaxConnections int
	// HandshakeTimeout bounds the auth handshake; 0 => default.
	HandshakeTimeout time.Duration
	// AuthFailDelay slows brute-force attempts; <0 => none, 0 => default.
	AuthFailDelay time.Duration
	// BundleDir is the directory under which uploaded git bundles are extracted
	// into per-link working trees. Empty disables bundle uploads entirely (a
	// bundle-mode connection is then refused — opt-in, fail closed).
	BundleDir string
	// MaxBundleBytes caps a single uploaded bundle; 0 => default.
	MaxBundleBytes int64
	Log            func(string)
}

// NewBridge validates options and builds a Bridge.
func NewBridge(opts BridgeOptions) (*Bridge, error) {
	if opts.Server == nil {
		return nil, errors.New("remote: bridge requires a daemon server")
	}
	if opts.Authenticator == nil {
		return nil, errors.New("remote: bridge requires an authenticator")
	}
	minVersion := opts.MinVersion
	if minVersion <= 0 {
		minVersion = daemon.ProtoVersion
	}
	maxConns := opts.MaxConnections
	if maxConns <= 0 {
		maxConns = defaultMaxConnections
	}
	handshakeTimeout := opts.HandshakeTimeout
	if handshakeTimeout <= 0 {
		handshakeTimeout = defaultHandshakeTimeout
	}
	authFailDelay := opts.AuthFailDelay
	switch {
	case authFailDelay < 0:
		authFailDelay = 0
	case authFailDelay == 0:
		authFailDelay = defaultAuthFailDelay
	}
	attest := opts.Attestation
	if attest == nil {
		attest = noopAttestation{}
	}
	maxBundleBytes := opts.MaxBundleBytes
	if maxBundleBytes <= 0 {
		maxBundleBytes = defaultMaxBundleBytes
	}
	bundleDir := strings.TrimSpace(opts.BundleDir)
	if bundleDir != "" {
		// A previous run may have died mid-swap, leaving a link's only tree in a
		// staging dir. Nothing serves yet, so repair before the first upload.
		recoverBundleDir(bundleDir, func(format string, args ...any) {
			if opts.Log != nil {
				opts.Log(fmt.Sprintf(format, args...))
			}
		})
	}
	return &Bridge{
		server:           opts.Server,
		auth:             opts.Authenticator,
		attest:           attest,
		minVersion:       minVersion,
		handshakeTimeout: handshakeTimeout,
		authFailDelay:    authFailDelay,
		bundleDir:        bundleDir,
		maxBundleBytes:   maxBundleBytes,
		log:              opts.Log,
		sem:              make(chan struct{}, maxConns),
	}, nil
}

func (b *Bridge) logf(format string, args ...any) {
	if b.log != nil {
		b.log(fmt.Sprintf(format, args...))
	}
}

// ListenAndServeTLS binds a TLS listener on addr and serves until the listener
// is closed (via Close). It refuses to serve without a TLS config (no plaintext
// remote bridge).
func (b *Bridge) ListenAndServeTLS(addr string, tlsConfig *tls.Config) error {
	if tlsConfig == nil {
		return errors.New("remote: TLS config is required; refusing to serve a plaintext remote bridge")
	}
	listener, err := tls.Listen("tcp", addr, tlsConfig)
	if err != nil {
		return fmt.Errorf("remote: listen %s: %w", addr, err)
	}
	b.mu.Lock()
	b.listener = listener
	b.mu.Unlock()
	return b.Serve(listener)
}

// Close stops the listener started by ListenAndServeTLS, causing Serve to
// return. Safe to call before serving or more than once.
func (b *Bridge) Close() error {
	b.mu.Lock()
	listener := b.listener
	b.listener = nil // clear so a repeat Close is a no-op, not a closed-listener error
	b.mu.Unlock()
	if listener != nil {
		return listener.Close()
	}
	return nil
}

// Serve accepts connections from listener until it returns an error (e.g. it is
// closed). Each connection is authenticated, then handed to the daemon's control
// dispatch. Connections beyond MaxConnections are refused immediately.
func (b *Bridge) Serve(listener net.Listener) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		select {
		case b.sem <- struct{}{}:
		default:
			// At capacity: refuse fast (fail closed) rather than queueing.
			b.logf("remote: refused connection (at capacity)")
			_ = conn.Close()
			continue
		}
		go func() {
			defer func() { <-b.sem }()
			b.handle(conn)
		}()
	}
}

// handle authenticates conn then drives the daemon dispatch. It always closes
// conn (directly on a failed handshake, or via ServeConn on success).
func (b *Bridge) handle(conn net.Conn) {
	// Bound the auth handshake so a stalled peer cannot hold a connection slot.
	_ = conn.SetDeadline(time.Now().Add(b.handshakeTimeout))
	req, err := readAuthRequest(conn)
	if err != nil {
		_ = conn.Close()
		return
	}
	if req.Version < b.minVersion {
		b.deny(conn, "unsupported protocol version")
		return
	}
	if err := b.auth.Authenticate(req.Token); err != nil {
		b.deny(conn, "unauthorized")
		return
	}
	if err := b.attest.Verify(req.Meta); err != nil {
		b.deny(conn, "attestation failed")
		return
	}
	// Resolve the connection mode before accepting it. Validating here (before the
	// success response) lets the bridge fail closed: an unknown mode, or a bundle
	// upload when bundle transfer is disabled, is denied without side effects.
	mode := req.Mode
	if mode == "" {
		mode = ModeSession
	}
	switch mode {
	case ModeSession:
	case ModeBundle:
		if b.bundleDir == "" {
			b.deny(conn, "bundle transfer not enabled")
			return
		}
	default:
		b.deny(conn, "unsupported mode")
		return
	}
	if err := writeAuthResponse(conn, authResponse{OK: true, Version: daemon.ProtoVersion}); err != nil {
		_ = conn.Close()
		return
	}
	// Clear the handshake deadline: a session may stream, and a bundle upload may
	// transfer many megabytes, for a long time.
	_ = conn.SetDeadline(time.Time{})
	switch mode {
	case ModeBundle:
		b.handleBundle(conn) // receives + extracts the bundle, then closes conn
	default:
		b.server.ServeConn(conn) // performs the daemon handshake + one command, then closes conn
	}
}

// deny rejects an unauthenticated connection after a small backoff (to slow
// brute force). The reason is generic and never includes any token material.
func (b *Bridge) deny(conn net.Conn, message string) {
	if b.authFailDelay > 0 {
		time.Sleep(b.authFailDelay)
	}
	_ = writeAuthResponse(conn, authResponse{OK: false, Message: message})
	_ = conn.Close()
}

// ServerTLSConfig builds a server TLS config from a cert/key pair, refusing if
// either is missing (TLS is mandatory for the remote bridge).
func ServerTLSConfig(certFile, keyFile string) (*tls.Config, error) {
	certFile = strings.TrimSpace(certFile)
	keyFile = strings.TrimSpace(keyFile)
	if certFile == "" || keyFile == "" {
		return nil, errors.New("remote: a TLS cert and key are required for serve-remote")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("remote: load TLS keypair: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}
