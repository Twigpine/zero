// Package remote adds an OPT-IN, TLS-only network bridge on top of zero's local
// daemon (internal/daemon). It lets a remote client drive the SAME daemon
// SessionManager/Pool over the network, behind bearer-token authentication and a
// protocol-version floor. The local Unix-socket daemon is unchanged and remains
// the default; nothing here activates unless `zero daemon serve-remote` is run.
//
// Security (fail closed):
//   - TLS is mandatory; the bridge refuses to serve without a cert/key.
//   - A connection is authenticated BEFORE any control frame is dispatched; a
//     failed/absent token closes the connection (after a small backoff) and no
//     session frame is ever processed.
//   - Tokens are compared in constant time and are never logged.
//   - The control-frame size cap and version negotiation from internal/daemon are
//     reused unchanged; oversize/old-version handshakes are rejected.
//   - A remote-driven session runs through the same daemon dispatch, so it stays
//     under the same sandbox + risk model — remote never bypasses local controls.
package remote

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Gitlawb/zero/internal/daemon"
	"github.com/Gitlawb/zero/internal/remotetoken"
)

// Env vars the bridge reads for its bearer token.
const (
	EnvToken             = remotetoken.EnvToken
	EnvTokenFile         = remotetoken.EnvTokenFile
	EnvTokenFileResolved = remotetoken.EnvTokenFileResolved
	EnvTokenFileIdentity = remotetoken.EnvTokenFileIdentity
)

// ErrUnauthorized is returned when a token does not match.
var ErrUnauthorized = errors.New("remote: unauthorized")

// Authenticator verifies a bearer token presented by a remote client.
type Authenticator interface {
	Authenticate(token string) error
}

// TokenAuthenticator compares a presented token against a fixed secret in
// constant time.
type TokenAuthenticator struct {
	token              string
	protectionIdentity string
}

// NewTokenAuthenticator builds a token authenticator, refusing an empty secret
// (fail closed — a bridge must never accept everyone).
func NewTokenAuthenticator(token string) (*TokenAuthenticator, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("remote: a non-empty auth token is required")
	}
	return &TokenAuthenticator{token: token}, nil
}

// Authenticate reports nil when token matches the configured secret.
func (a *TokenAuthenticator) Authenticate(token string) error {
	if subtle.ConstantTimeCompare([]byte(token), []byte(a.token)) == 1 {
		return nil
	}
	return ErrUnauthorized
}

// TokenFromEnv resolves the bridge token from EnvToken, or the file source
// selected by EnvTokenFile. After daemon startup resolution it reads the pinned
// object rather than re-following a mutable configured symlink.
func TokenFromEnv() (string, error) {
	if t := strings.TrimSpace(os.Getenv(EnvToken)); t != "" {
		return t, nil
	}
	if source, selected := remotetoken.SourceFromEnv(); selected {
		data, err := os.ReadFile(source.ReadPath())
		if err != nil {
			return "", fmt.Errorf("remote: read token file: %w", err)
		}
		t := strings.TrimSpace(string(data))
		if t == "" {
			return "", errors.New("remote: token file is empty")
		}
		return t, nil
	}
	return "", fmt.Errorf("remote: set %s or %s", EnvToken, EnvTokenFile)
}

// TokenFromFreshEnv is TokenFromEnv for a caller that has NOT run
// CanonicalizeTokenFileEnv and must not trust an inherited
// ZERO_INTERNAL_DAEMON_REMOTE_TOKEN_FILE_RESOLVED marker.
//
// That marker is a daemon-worker handoff value, not an independently
// authoritative selector: remotetoken.SourceFromEnv binds it to whatever
// ZERO_DAEMON_REMOTE_TOKEN_FILE happens to be set to right now, without proving
// it is THAT value's resolved identity. A one-shot client process — `zero
// daemon link`, or a remote dial — inherits its environment from its own
// parent, which can carry a resolved marker left over from an unrelated prior
// `serve-remote` invocation in the same shell. If the operator then points
// EnvTokenFile at a different file, this client reads the stale target instead:
// it can authenticate with a revoked credential, or fail against a valid one.
//
// remotetoken.ResolveSource ignores the marker and always resolves fresh, which
// is exactly the property a caller that never canonicalized needs.
func TokenFromFreshEnv() (string, error) {
	if t := strings.TrimSpace(os.Getenv(EnvToken)); t != "" {
		return t, nil
	}
	source, selected, err := remotetoken.ResolveSource()
	if err != nil {
		return "", fmt.Errorf("remote: %w", err)
	}
	if !selected {
		return "", fmt.Errorf("remote: set %s or %s", EnvToken, EnvTokenFile)
	}
	data, err := os.ReadFile(source.ReadPath())
	if err != nil {
		return "", fmt.Errorf("remote: read token file: %w", err)
	}
	t := strings.TrimSpace(string(data))
	if t == "" {
		return "", errors.New("remote: token file is empty")
	}
	return t, nil
}

// CanonicalizeTokenFileEnv records both identities of the selected token file
// before workers start: the operator-configured absolute spelling and the
// symlink-resolved object this daemon authenticated against. Keeping both
// prevents worker CWD changes from retargeting a relative value without losing
// the configured authority boundary across replacement and restart.
//
// It is a deliberate no-op when EnvToken supplies the token: TokenFromEnv
// prefers the inline value, so an unused (even dangling) file pointer must not
// change the outcome or fail the start.
func CanonicalizeTokenFileEnv() error {
	source, selected, err := remotetoken.ResolveSource()
	if err != nil {
		return fmt.Errorf("remote: %w", err)
	}
	if !selected {
		return nil
	}
	if err := remotetoken.PersistSource(source); err != nil {
		return fmt.Errorf("remote: persist token file source: %w", err)
	}
	return nil
}

// NewAuthenticatorFromEnv opens a file token once, derives protection identity
// from that handle, reads authentication bytes from the same handle, and only
// then publishes the trusted worker handoff. This prevents atomic replacement
// of the configured name from retiring the startup inode's aliases.
func NewAuthenticatorFromEnv() (*TokenAuthenticator, error) {
	if token := strings.TrimSpace(os.Getenv(EnvToken)); token != "" {
		return NewTokenAuthenticator(token)
	}
	source, selected, err := remotetoken.ResolveSource()
	if err != nil {
		return nil, fmt.Errorf("remote: %w", err)
	}
	if !selected {
		return nil, fmt.Errorf("remote: set %s or %s", EnvToken, EnvTokenFile)
	}
	file, err := os.Open(source.ReadPath())
	if err != nil {
		return nil, fmt.Errorf("remote: read token file: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("remote: token file must be a regular file")
	}
	identity, ok := remotetoken.IdentityOfFile(file)
	if !ok {
		return nil, errors.New("remote: token file identity is unavailable on this platform")
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("remote: read token file: %w", err)
	}
	auth, err := NewTokenAuthenticator(string(data))
	if err != nil {
		return nil, err
	}
	auth.protectionIdentity = identity
	source.Identity = identity
	if err := remotetoken.PersistSource(source); err != nil {
		return nil, fmt.Errorf("remote: persist token file source: %w", err)
	}
	return auth, nil
}

// Attestation is an optional post-token hook (e.g. workload attestation). The
// default is a no-op; a deployment can supply a stricter implementation.
type Attestation interface {
	Verify(meta map[string]string) error
}

type noopAttestation struct{}

func (noopAttestation) Verify(map[string]string) error { return nil }

// Connection modes negotiated in the auth handshake. The default (empty) is a
// daemon session, preserving the original behavior; "bundle" requests a one-shot
// git-bundle upload instead of a session.
const (
	ModeSession = "session"
	ModeBundle  = "bundle"
)

// authRequest is the first frame a remote client sends (before the daemon
// hello). Token is never logged.
type authRequest struct {
	Token   string            `json:"token"`
	Version int               `json:"version"`
	Meta    map[string]string `json:"meta,omitempty"`
	// Mode selects the connection's purpose: "" / "session" => a daemon session
	// (default), "bundle" => a git-bundle upload. Unknown modes are rejected.
	Mode string `json:"mode,omitempty"`
}

// authResponse is the bridge's reply to the auth handshake.
type authResponse struct {
	OK      bool   `json:"ok"`
	Version int    `json:"version,omitempty"`
	Message string `json:"message,omitempty"`
}

func writeAuthRequest(w io.Writer, req authRequest) error {
	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}
	return daemon.WriteFrame(w, daemon.KindCtrl, payload)
}

func readAuthRequest(r io.Reader) (authRequest, error) {
	kind, payload, err := daemon.ReadFrame(r)
	if err != nil {
		return authRequest{}, err
	}
	if kind != daemon.KindCtrl {
		return authRequest{}, errors.New("remote: expected auth frame")
	}
	var req authRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return authRequest{}, fmt.Errorf("remote: decode auth request: %w", err)
	}
	return req, nil
}

func writeAuthResponse(w io.Writer, resp authResponse) error {
	payload, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	return daemon.WriteFrame(w, daemon.KindCtrl, payload)
}

func readAuthResponse(r io.Reader) (authResponse, error) {
	kind, payload, err := daemon.ReadFrame(r)
	if err != nil {
		return authResponse{}, err
	}
	if kind != daemon.KindCtrl {
		return authResponse{}, errors.New("remote: expected auth response frame")
	}
	var resp authResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		return authResponse{}, fmt.Errorf("remote: decode auth response: %w", err)
	}
	return resp, nil
}
