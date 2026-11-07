package remote

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/Twigpine/zero/internal/daemon"
)

const defaultDialTimeout = 15 * time.Second

// RemoteConfig configures a remote dial.
type RemoteConfig struct {
	// Address is host:port of the remote bridge (required).
	Address string
	// Token is the bearer token (required).
	Token string
	// CACertFile, when set, is the only CA trusted for the server cert (use for a
	// self-signed bridge). Empty => system roots. InsecureSkipVerify is never used.
	CACertFile string
	// ServerName overrides the TLS/SNI verification name; empty => host of Address.
	ServerName string
	// Timeout bounds the dial + auth handshake; 0 => default.
	Timeout time.Duration
}

// DialRemote establishes a verified TLS connection to a remote bridge,
// authenticates with the bearer token, and returns a daemon.Client ready for
// Run/Attach/Status — the same client the local socket uses, so remote and local
// share one protocol. The server certificate is always verified (never
// InsecureSkipVerify).
func DialRemote(cfg RemoteConfig) (*daemon.Client, error) {
	conn, err := dialAuthenticated(cfg, ModeSession)
	if err != nil {
		return nil, err
	}
	client, err := daemon.NewClientConn(conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return client, nil
}

// dialAuthenticated establishes a verified TLS connection, performs the
// bearer-token auth handshake for the given mode, and returns the live conn with
// its deadline cleared (ready for the subsequent daemon handshake or bundle
// stream). The server certificate is always verified (never InsecureSkipVerify).
func dialAuthenticated(cfg RemoteConfig, mode string) (net.Conn, error) {
	address := strings.TrimSpace(cfg.Address)
	if address == "" {
		return nil, errors.New("remote: address is required")
	}
	token := strings.TrimSpace(cfg.Token)
	if token == "" {
		return nil, errors.New("remote: a non-empty token is required")
	}
	tlsConfig, err := clientTLSConfig(cfg)
	if err != nil {
		return nil, err
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultDialTimeout
	}
	// Context-aware TLS dial (tls.DialWithDialer is deprecated and not
	// cancellation-aware). The context bounds the dial + TLS handshake only; the
	// returned conn is independent of it for the subsequent auth/stream I/O.
	dialCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	dialer := &tls.Dialer{NetDialer: &net.Dialer{}, Config: tlsConfig}
	conn, err := dialer.DialContext(dialCtx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("remote: dial %s: %w", address, err)
	}
	// Bound the auth handshake; the daemon handshake + stream run without a deadline.
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if err := writeAuthRequest(conn, authRequest{Token: token, Version: daemon.ProtoVersion, Mode: mode}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("remote: send auth: %w", err)
	}
	resp, err := readAuthResponse(conn)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("remote: auth handshake: %w", err)
	}
	if !resp.OK {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: %s", ErrUnauthorized, resp.Message)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// clientTLSConfig builds a verifying client TLS config. It never disables
// verification; for a self-signed bridge, set CACertFile to the bridge's cert.
func clientTLSConfig(cfg RemoteConfig) (*tls.Config, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	serverName := strings.TrimSpace(cfg.ServerName)
	if serverName == "" {
		if host, _, err := net.SplitHostPort(cfg.Address); err == nil {
			serverName = host
		}
	}
	tc.ServerName = serverName
	if ca := strings.TrimSpace(cfg.CACertFile); ca != "" {
		pem, err := os.ReadFile(ca)
		if err != nil {
			return nil, fmt.Errorf("remote: read CA cert: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("remote: CA cert file contained no certificates")
		}
		tc.RootCAs = pool
	}
	return tc, nil
}
