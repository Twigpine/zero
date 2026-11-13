// Package oauth is zero's reusable OAuth 2.0 engine. It generalizes the
// authorization-code + PKCE flow that internal/mcp already uses for MCP servers
// into a transport/identity-agnostic core, and adds the pieces provider login
// needs: a device-authorization (RFC 8628) grant, a provider registry, a
// namespaced + refreshing token store, and a proactive refresh scheduler.
//
// Security invariants (enforced throughout, never relaxed):
//   - PKCE S256 is mandatory on every authorization-code flow; "plain" is refused.
//   - A per-flow CSRF state is generated and verified on the callback.
//   - The loopback callback server binds 127.0.0.1 only, on an OS-assigned port,
//     serves a single request, then closes.
//   - The token endpoint must be https (loopback exempt) before any credential is
//     sent — see validateTokenEndpoint.
//   - Tokens, codes, and verifiers are never logged; error bodies are redacted.
//   - Token files are 0600, base-confined, and file-locked.
//
// This package holds NO vendor secrets: provider client IDs and endpoints come
// from config/env by default, so no third-party OAuth client identity is used
// unless the operator opts in. A small set of built-in presets (e.g. xAI's public
// client) exists for convenience but is OFF by default and only consulted when
// ZERO_OAUTH_ALLOW_PRESETS is set; env always overrides a preset.
package oauth

import (
	"errors"
	"strings"
	"time"
)

// Token holds the credentials issued by an authorization server. The token
// fields are sensitive: callers must never log them, and the store persists them
// 0600. It mirrors the on-disk shape internal/mcp uses so the two stay
// format-compatible.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	Scopes       []string  `json:"scopes,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	// Account is an optional non-secret identifier (email / account id) shown in
	// status output; never a credential.
	Account string `json:"account,omitempty"`
	// IDToken is the OIDC ID token returned alongside the access token (when the
	// `openid` scope was requested). It is a JWS and may carry claims (such as
	// `chatgpt_account_id`) that the request path needs as headers. Treated as
	// sensitive like the access token: never logged, persisted 0600.
	IDToken string `json:"id_token,omitempty"`
	// TokenEndpoint, ClientID and ClientSecret record what a login learned that
	// config does not carry (a discovered token endpoint, a dynamically
	// registered client) so a later refresh can reuse it. ClientSecret is
	// sensitive like the tokens. ProtectedTokenEndpoint marks an endpoint
	// advertised by the server, which must be re-checked against the public
	// network policy before a refresh posts credentials to it.
	TokenEndpoint          string `json:"token_endpoint,omitempty"`
	ProtectedTokenEndpoint bool   `json:"protected_token_endpoint,omitempty"`
	ClientID               string `json:"client_id,omitempty"`
	ClientSecret           string `json:"client_secret,omitempty"`
}

// Expired reports whether the token has an expiry that is at or before now.
// A zero ExpiresAt means "no known expiry" and is treated as not expired.
func (t Token) Expired(now time.Time) bool {
	return !t.ExpiresAt.IsZero() && !t.ExpiresAt.After(now)
}

// NeedsRefresh reports whether the token is expired or falls within buffer of
// expiry (so a proactive/on-demand refresh should run). A token with no expiry
// never needs a refresh on a timer.
func (t Token) NeedsRefresh(now time.Time, buffer time.Duration) bool {
	if t.ExpiresAt.IsZero() {
		return false
	}
	return !t.ExpiresAt.After(now.Add(buffer))
}

// Config describes how to talk to one authorization server for a flow.
// Endpoints may be discovered (RFC 8414) and overridden here.
type Config struct {
	ClientID     string
	ClientSecret string
	Scopes       []string

	AuthorizationEndpoint       string
	TokenEndpoint               string
	DeviceAuthorizationEndpoint string
	RegistrationEndpoint        string
	// IssuerURL is the base for metadata discovery when endpoints are not set.
	IssuerURL string

	// ExtraAuthParams are appended to the authorization URL (e.g. login_hint).
	ExtraAuthParams map[string]string
}

// Errors returned by the engine. Callers can match these with errors.Is.
var (
	// ErrPKCEDowngrade is returned if a flow is asked to use anything but S256.
	ErrPKCEDowngrade = errors.New("oauth: PKCE S256 is mandatory; plain is refused")
	// ErrStateMismatch is returned when the callback state does not match (CSRF).
	ErrStateMismatch = errors.New("oauth: callback state mismatch; possible CSRF, login aborted")
	// ErrInsecureTokenEndpoint is returned when a credential would be sent over a
	// non-https, non-loopback endpoint.
	ErrInsecureTokenEndpoint = errors.New("oauth: refusing to send credential to a non-https token endpoint")
	// ErrUnsafeRedirect is returned when a credential-bearing OAuth POST (token
	// exchange/refresh, device authorization, or device-token poll) is redirected.
	// Following it would replay the form body — codes, PKCE verifiers, refresh
	// tokens, client secrets — to a target that never passed endpoint validation.
	ErrUnsafeRedirect = errors.New("oauth: refusing to follow a redirect from a credential endpoint")
	// ErrNoRefreshToken is returned when a refresh is attempted without one.
	ErrNoRefreshToken = errors.New("oauth: no refresh token available")
	// ErrAuthorizationPending is the RFC 8628 "keep polling" signal.
	ErrAuthorizationPending = errors.New("oauth: authorization pending")
	// ErrSlowDown is the RFC 8628 "increase the poll interval" signal.
	ErrSlowDown = errors.New("oauth: slow down")
)

// trimmed is a tiny helper used across the package.
func trimmed(s string) string { return strings.TrimSpace(s) }
