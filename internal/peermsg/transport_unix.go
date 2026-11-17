//go:build !windows

package peermsg

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

type unixTransport struct{}

const unixSocketPathMax = 103

func newPlatformTransport() localTransport { return unixTransport{} }

func (unixTransport) Endpoint(root, nonce string, pid int) (string, error) {
	dir := filepath.Join(root, "sockets")
	dir, err := canonicalPrivateDir(dir)
	if err != nil {
		return "", fmt.Errorf("peer messaging: create socket directory: %w", err)
	}
	path := filepath.Join(dir, fmt.Sprintf("%d-%s.sock", pid, nonce))
	// macOS has the smallest supported sockaddr_un path (103 usable bytes).
	if len(path) > unixSocketPathMax {
		dir = filepath.Join(os.TempDir(), fmt.Sprintf("zero-peers-%d", os.Getuid()))
		dir, err = canonicalPrivateDir(dir)
		if err != nil {
			return "", fmt.Errorf("peer messaging: create fallback socket directory: %w", err)
		}
		path = filepath.Join(dir, fmt.Sprintf("%d-%s.sock", pid, nonce))
	}
	if len(path) > unixSocketPathMax {
		return "", fmt.Errorf("peer messaging: socket path is too long: %s", path)
	}
	return path, nil
}

func canonicalPrivateDir(path string) (string, error) {
	canonical, err := canonicalRuntimePath(path)
	if err != nil {
		return "", err
	}
	if err := EnsurePrivateDir(canonical); err != nil {
		return "", err
	}
	return canonical, nil
}

func (unixTransport) Listen(endpoint string) (net.Listener, error) {
	_ = os.Remove(endpoint)
	listener, err := net.Listen("unix", endpoint)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(endpoint, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(endpoint)
		return nil, err
	}
	return listener, nil
}

func (unixTransport) Dial(ctx context.Context, endpoint string) (net.Conn, error) {
	dialer := net.Dialer{Timeout: 5 * time.Second}
	return dialer.DialContext(ctx, "unix", endpoint)
}

func (unixTransport) Remove(endpoint string) error {
	err := os.Remove(endpoint)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
