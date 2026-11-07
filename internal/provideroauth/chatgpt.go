// ChatGPT-specific login flow. See openrouter.go for the package doc.
package provideroauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Twigpine/zero/internal/oauth"
)

// chatgptAccountClaim is the ID-token claim the ChatGPT backend requires as the
// `chatgpt-account-id` request header on every Codex call. OpenAI namespaces it
// under the chatgptAuthClaimNamespace object (NOT at the top level) — see
// extractChatGPTAccountID.
const chatgptAccountClaim = "chatgpt_account_id"

// chatgptAuthClaimNamespace is the custom-claim namespace OpenAI's id_token uses
// for ChatGPT auth claims. The account id lives at
// id_token[chatgptAuthClaimNamespace][chatgptAccountClaim], matching the Codex
// CLI's own parser (it reads exclusively from this nested object).
const chatgptAuthClaimNamespace = "https://api.openai.com/auth"

// chatgptCallbackPort is the fixed loopback port ChatGPT's public Codex CLI
// client registration requires in its redirect_uri. Unlike the OS-assigned
// port other OAuth flows use, this must be exactly 1455.
const chatgptCallbackPort = 1455

// ChatGPTOptions configures the ChatGPT (Codex) login flow.
type ChatGPTOptions struct {
	// Env supplies env-style overrides; nil falls back to the process environment.
	// ZERO_OAUTH_ALLOW_PRESETS must be set in the effective env (or a preset-supplied
	// env block) for the baked-in client_id to be used; otherwise the caller is
	// expected to have configured ZERO_OAUTH_CHATGPT_* explicitly.
	Env map[string]string
	// HTTPClient performs the token exchange; nil => a client with a sane timeout.
	HTTPClient *http.Client
	// OpenBrowser is invoked with the authorize URL. When nil the URL is only
	// printed (to Out) for the user to open manually. Tests inject a function
	// that drives the loopback redirect.
	OpenBrowser func(authURL string) error
	// Out receives the "open this URL" line; nil => the URL is not printed.
	Out io.Writer
	// Timeout bounds the whole interactive login; 0 => 5 minutes.
	Timeout time.Duration
	// Now is the time source; nil => time.Now. Injected by tests.
	Now func() time.Time
}

// ChatGPTLogin runs ChatGPT's standard OAuth loopback flow against the chatgpt
// preset (see internal/oauth/presets.go) and returns a Token whose Account
// field is populated with the `chatgpt_account_id` claim. The bearer itself is
// valid against `https://chatgpt.com/backend-api/codex/responses` for ChatGPT
// Plus/Pro/Business/Enterprise subscribers; the account id is required as a
// header on every Codex call.
//
// The login is the same generic loopback flow `oauth.Manager.Login` runs for
// every other provider; the bespoke part is the ID-token claim extraction.
// Returning the token (rather than persisting it) keeps the function
// composable: the CLI subcommand wraps it with the oauth.Manager's store, so
// the existing token-persistence and refresh-scheduling paths are unchanged.
func ChatGPTLogin(ctx context.Context, opts ChatGPTOptions) (oauth.Token, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	loginCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Resolve the provider config from the env. The chatgpt preset is what
	// makes this work without ZERO_OAUTH_CHATGPT_CLIENT_ID: the resolver applies
	// it whenever ZERO_OAUTH_ALLOW_PRESETS is set, the same path every other
	// built-in preset uses.
	registry := oauth.NewRegistry()
	cfg, flow, err := registry.ResolveConfig("chatgpt", opts.Env)
	if err != nil {
		return oauth.Token{}, fmt.Errorf("provideroauth: chatgpt config: %w", err)
	}
	if flow != oauth.FlowLoopback {
		return oauth.Token{}, fmt.Errorf("provideroauth: chatgpt flow is %q, want loopback (Codex has no device-code path)", flow)
	}
	// Pin the token endpoint from the preset so a caller that supplies env
	// overrides for other fields can't accidentally re-route the code exchange.
	// Discovery is unnecessary here (we already have the endpoints) and would
	// only widen the surface for a misuse.
	if strings.TrimSpace(cfg.TokenEndpoint) == "" {
		return oauth.Token{}, errors.New("provideroauth: chatgpt preset has no token endpoint")
	}
	if strings.TrimSpace(cfg.AuthorizationEndpoint) == "" {
		return oauth.Token{}, errors.New("provideroauth: chatgpt preset has no authorize endpoint")
	}

	pkce, err := oauth.NewPKCE()
	if err != nil {
		return oauth.Token{}, fmt.Errorf("provideroauth: generate PKCE: %w", err)
	}
	state, err := oauth.NewState()
	if err != nil {
		return oauth.Token{}, fmt.Errorf("provideroauth: generate CSRF state: %w", err)
	}
	listener, err := oauth.NewLoopbackListenerOnPort(state, chatgptCallbackPort)
	if err != nil {
		return oauth.Token{}, fmt.Errorf("provideroauth: start loopback listener on port %d (close any other Zero or Codex login already running and retry): %w", chatgptCallbackPort, err)
	}
	defer listener.Close()

	// ChatGPT's client registration requires the redirect_uri host to be
	// "localhost" (not 127.0.0.1) at the fixed callback path. The listener still
	// binds 127.0.0.1 and accepts /auth/callback.
	redirectURI := listener.RedirectURIWithHost("localhost", "/auth/callback")
	// ChatGPT's authorize endpoint requires these extra params (the Codex CLI
	// sends them too) — without them it rejects with authorize_hydra_invalid_request.
	chatgptExtraParams := map[string]string{
		"id_token_add_organizations": "true",
		"codex_cli_simplified_flow":  "true",
		"originator":                 "codex_cli_rs",
	}
	authURL, err := oauth.BuildAuthorizationURL(cfg, pkce, state, redirectURI, chatgptExtraParams)
	if err != nil {
		return oauth.Token{}, fmt.Errorf("provideroauth: build authorize URL: %w", err)
	}
	if opts.Out != nil {
		fmt.Fprintf(opts.Out, "Open this URL to authorize ChatGPT:\n  %s\n", authURL)
	}
	if opts.OpenBrowser != nil {
		if err := opts.OpenBrowser(authURL); err != nil {
			return oauth.Token{}, fmt.Errorf("provideroauth: open authorization URL: %w", err)
		}
	}

	code, err := listener.Wait(loginCtx)
	if err != nil {
		return oauth.Token{}, fmt.Errorf("provideroauth: wait for callback: %w", err)
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}
	token, err := oauth.ExchangeCode(loginCtx, opts.client(), cfg, code, pkce.Verifier, redirectURI, now)
	if err != nil {
		return oauth.Token{}, fmt.Errorf("provideroauth: exchange code: %w", err)
	}
	// Pull the chatgpt-account-id off the ID token so the Codex provider can
	// inject it as a header on every request. The ID token is opaque to the
	// caller; the chatgpt preset's scopes request "openid profile email
	// offline_access" precisely so this claim is included.
	if account, claimErr := extractChatGPTAccountID(token); claimErr != nil {
		// The bearer is still valid; surface the warning but keep the token so
		// the user isn't locked out when OpenAI rotates the claim name. The
		// Codex provider then omits the account-id header (Cloudflare 401s
		// until the user re-auths).
		fmt.Fprintf(opts.OutOrStderr(opts.Out), "warning: could not extract chatgpt-account-id from ID token: %v\n", claimErr)
	} else if account != "" {
		token.Account = account
	}
	return token, nil
}

// client returns the configured HTTP client or a sensible default.
func (o ChatGPTOptions) client() *http.Client {
	if o.HTTPClient != nil {
		return o.HTTPClient
	}
	return &http.Client{Timeout: 60 * time.Second}
}

// OutOrStderr picks opts.Out when non-nil, otherwise io.Discard. Used for
// optional informational lines that should never reach a real os.Stderr (the
// library never touches the process stderr — only the CLI command may).
func (o ChatGPTOptions) OutOrStderr(out io.Writer) io.Writer {
	if out != nil {
		return out
	}
	return io.Discard
}

// extractChatGPTAccountID pulls the `chatgpt_account_id` claim out of the ID
// token. OpenAI nests it under the "https://api.openai.com/auth" claim object
// (id_token[ns][chatgpt_account_id]), NOT at the top level — reading the top
// level returns empty against a real token, which silently drops the required
// chatgpt-account-id header and 401s every Codex call. We read the nested path
// first (matching the Codex CLI's parser) and fall back to a top-level claim
// only for forward-compat. The token is the access-token response's `id_token`
// field, which oauth.ExchangeCode populates when the endpoint returns one.
//
// The ID token is a JWS — three base64url segments separated by dots. We do
// NOT verify the signature: the bearer is already authenticated by TLS to
// auth.openai.com, and the Codex backend's only check on
// `chatgpt-account-id` is "non-empty and matches the bearer". A future
// hardening pass can wire the OIDC JWKS at
// https://auth.openai.com/.well-known/jwks.json and validate the signature
// properly. For now we surface tampering as a parse failure (a JWS whose
// payload is not valid base64-JSON is rejected), which is the same posture
// most CLI OAuth integrations take and matches what `codex` itself does.
//
// Returns ("", nil) when no ID token is present (so older / non-OIDC token
// responses are accepted but the account header is just omitted).
func extractChatGPTAccountID(token oauth.Token) (string, error) {
	raw := strings.TrimSpace(token.IDToken)
	if raw == "" {
		return "", nil
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("ID token is not a JWS (expected 3 segments, got %d)", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode JWS payload: %w", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("parse JWS claims: %w", err)
	}
	// OpenAI namespaces the account id under the "https://api.openai.com/auth"
	// claim object — id_token[ns][chatgpt_account_id] — NOT at the top level.
	// Read the nested path first (this is where every real token puts it, and
	// what the Codex CLI reads); fall back to a bare top-level claim only for
	// forward-compat if OpenAI ever flattens it.
	if ns, ok := claims[chatgptAuthClaimNamespace].(map[string]any); ok {
		if value, ok := ns[chatgptAccountClaim].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value), nil
		}
		// The namespace is present (the scopes request it) but the account id is
		// missing/empty/not-a-string — the claim shape changed. Surface an error so
		// the caller warns and the user re-auths, instead of silently omitting the
		// chatgpt-account-id header and hitting opaque Cloudflare 401s on every Codex
		// call that look identical to an expired token. (AUDIT-L11)
		return "", fmt.Errorf("chatgpt-account-id claim missing or not a non-empty string under %q", chatgptAuthClaimNamespace)
	}
	if value, ok := claims[chatgptAccountClaim].(string); ok {
		return strings.TrimSpace(value), nil
	}
	return "", nil
}
