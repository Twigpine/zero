package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/oauth"
)

// ServerAuthOAuth is the value of an MCP server's auth field that selects the
// OAuth 2.0 + PKCE authorization-code flow.
const ServerAuthOAuth = "oauth"

const defaultLoginTimeout = 3 * time.Minute

// MCP OAuth delegates its transport/identity-agnostic engine to internal/oauth
// (PKCE, RFC 8414 discovery, authorize-URL build, token exchange/refresh) so the
// two share one implementation. MCP keeps its own LoginOptions/Login
// orchestration, OAuthConfig/StoredToken types, loopback handling, CLI, and
// on-disk token format — all behavior-preserving. The shared engine also adds an
// https-only token-endpoint guard (loopback exempt), a hardening MCP inherits.

// pkceToParams converts the shared PKCE pair to MCP's local type.
func pkceToParams(p oauth.PKCE) pkceParams {
	return pkceParams{Verifier: p.Verifier, Challenge: p.Challenge, Method: p.Method}
}

// pkceToOAuth converts MCP's local PKCE type back to the shared type.
func pkceToOAuth(p pkceParams) oauth.PKCE {
	return oauth.PKCE{Verifier: p.Verifier, Challenge: p.Challenge, Method: p.Method}
}

// configFor builds the shared oauth.Config from MCP's OAuthConfig + resolved
// endpoints for a flow.
func configFor(cfg OAuthConfig, metadata authServerMetadata) oauth.Config {
	return oauth.Config{
		ClientID:              cfg.ClientID,
		ClientSecret:          cfg.ClientSecret,
		Scopes:                cfg.Scopes,
		AuthorizationEndpoint: metadata.AuthorizationEndpoint,
		TokenEndpoint:         metadata.TokenEndpoint,
	}
}

// tokenToStored converts a shared oauth.Token to MCP's StoredToken (MCP does not
// use the Account field).
func tokenToStored(t oauth.Token) StoredToken {
	return StoredToken{
		AccessToken:  t.AccessToken,
		RefreshToken: t.RefreshToken,
		TokenType:    t.TokenType,
		Scopes:       t.Scopes,
		ExpiresAt:    t.ExpiresAt,
	}
}

// OAuthConfig describes how to authenticate to a remote MCP server using OAuth.
// Endpoints may be discovered through RFC 9728 protected-resource metadata and
// RFC 8414 authorization-server metadata; explicit values override or fill in
// anything discovery cannot provide.
type OAuthConfig struct {
	ClientID              string
	ClientSecret          string
	Scopes                []string
	AuthorizationEndpoint string
	TokenEndpoint         string
	RegistrationEndpoint  string
	// IssuerURL overrides the base URL used for metadata discovery. When empty
	// the MCP server URL is used.
	IssuerURL string
}

// authServerMetadata is the subset of the OAuth 2.0 authorization server
// metadata document that the flow consumes.
type authServerMetadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	RegistrationEndpoint  string   `json:"registration_endpoint"`
	ScopesSupported       []string `json:"scopes_supported"`

	protectedResourceURL         string
	protectAuthorizationEndpoint bool
	protectTokenEndpoint         bool
	protectRegistrationEndpoint  bool
}

// pkceParams holds a PKCE verifier/challenge pair.
type pkceParams struct {
	Verifier  string
	Challenge string
	Method    string
}

// LoginOptions configures a single interactive authorization-code login.
type LoginOptions struct {
	ServerName string
	ServerURL  string
	Config     OAuthConfig
	HTTPClient *http.Client
	// OpenBrowser is invoked with the authorization URL. The default prints the
	// URL; tests inject a function that drives the loopback redirect.
	OpenBrowser func(authURL string) error
	Timeout     time.Duration
	Now         func() time.Time
}

// authorizationFlow carries the per-login state shared across helpers.
type authorizationFlow struct {
	httpClient *http.Client
	metadata   authServerMetadata
	config     OAuthConfig
	pkce       pkceParams
	state      string
	now        func() time.Time
}

// discoverAuthorizationServer fetches the RFC 8414 authorization server metadata
// at the well-known path under baseURL, via the shared engine.
func discoverAuthorizationServer(ctx context.Context, client *http.Client, baseURL string) (authServerMetadata, error) {
	meta, err := oauth.DiscoverAuthorizationServer(ctx, withoutDiscoveryRedirects(client), baseURL)
	if err != nil {
		return authServerMetadata{}, err
	}
	return authServerMetadata{
		Issuer:                meta.Issuer,
		AuthorizationEndpoint: meta.AuthorizationEndpoint,
		TokenEndpoint:         meta.TokenEndpoint,
		RegistrationEndpoint:  meta.RegistrationEndpoint,
		ScopesSupported:       meta.ScopesSupported,
	}, nil
}

// validateResolvedEndpoints applies the shared https/loopback scheme rule to
// every non-empty endpoint in the resolved metadata. Protected-resource
// discovery adds its stricter provenance-aware network rule below.
func validateResolvedEndpoints(metadata authServerMetadata) error {
	for _, ep := range []struct{ kind, url string }{
		{"authorization", metadata.AuthorizationEndpoint},
		{"token", metadata.TokenEndpoint},
		{"registration", metadata.RegistrationEndpoint},
	} {
		if strings.TrimSpace(ep.url) == "" {
			continue
		}
		if err := oauth.ValidateEndpointURL(ep.url); err != nil {
			return fmt.Errorf("mcp oauth: %s endpoint: %w", ep.kind, err)
		}
	}
	return nil
}

func validateProtectedDiscoveredEndpoints(ctx context.Context, metadata authServerMetadata) error {
	if metadata.protectedResourceURL == "" {
		return nil
	}
	policy, err := newOAuthDiscoveryNetworkPolicy(metadata.protectedResourceURL)
	if err != nil {
		return err
	}
	for _, endpoint := range []struct {
		kind      string
		url       string
		protected bool
	}{
		{kind: "authorization", url: metadata.AuthorizationEndpoint, protected: metadata.protectAuthorizationEndpoint},
		{kind: "token", url: metadata.TokenEndpoint, protected: metadata.protectTokenEndpoint},
		{kind: "registration", url: metadata.RegistrationEndpoint, protected: metadata.protectRegistrationEndpoint},
	} {
		if !endpoint.protected || strings.TrimSpace(endpoint.url) == "" {
			continue
		}
		parsed, parseErr := url.Parse(endpoint.url)
		if parseErr != nil || parsed.Hostname() == "" {
			return fmt.Errorf("mcp oauth: %s endpoint: %w", endpoint.kind, errUnsafeOAuthDiscoveryTarget)
		}
		if err := policy.validateLiteralHost(parsed.Hostname()); err != nil {
			return fmt.Errorf("mcp oauth: %s endpoint: %w", endpoint.kind, err)
		}
		if _, err := policy.resolve(ctx, parsed.Hostname()); err != nil {
			return fmt.Errorf("mcp oauth: %s endpoint: %w", endpoint.kind, err)
		}
	}
	return nil
}

// resolveAuthorizationServer follows protected-resource metadata when the MCP
// endpoint advertises it, falls back to direct authorization-server discovery
// for legacy providers, then applies explicit config overrides.
func resolveAuthorizationServer(ctx context.Context, client *http.Client, baseURL string, cfg OAuthConfig) (authServerMetadata, error) {
	// When the config supplies both the authorization and token endpoints
	// directly, skip network discovery entirely: there is nothing to discover,
	// and a hung/blocked discovery call (e.g. offline, or an unreachable issuer
	// in tests) must not gate an otherwise fully-specified login. Discovery stays
	// the fallback for any endpoint the config leaves blank.
	if strings.TrimSpace(cfg.AuthorizationEndpoint) != "" && strings.TrimSpace(cfg.TokenEndpoint) != "" {
		metadata := authServerMetadata{
			AuthorizationEndpoint: strings.TrimSpace(cfg.AuthorizationEndpoint),
			TokenEndpoint:         strings.TrimSpace(cfg.TokenEndpoint),
			RegistrationEndpoint:  strings.TrimSpace(cfg.RegistrationEndpoint),
		}
		if err := validateResolvedEndpoints(metadata); err != nil {
			return authServerMetadata{}, err
		}
		return metadata, nil
	}

	discoveryClient := withoutDiscoveryRedirects(client)
	discoveryBase := strings.TrimSpace(cfg.IssuerURL)
	protectedResourceDiscovery := false
	if discoveryBase == "" {
		issuer, found, protectedErr := discoverProtectedResourceAuthorizationServer(ctx, discoveryClient, baseURL)
		if protectedErr != nil {
			return authServerMetadata{}, protectedErr
		}
		if found {
			discoveryBase = issuer
			protectedResourceDiscovery = true
			discoveryClient, protectedErr = newAdvertisedOAuthDiscoveryClient(discoveryClient, baseURL)
			if protectedErr != nil {
				return authServerMetadata{}, protectedErr
			}
		} else {
			discoveryBase = baseURL
		}
	}

	metadata, err := discoverAuthorizationServer(ctx, discoveryClient, discoveryBase)
	if err != nil {
		// Discovery failures are non-fatal when the config supplies the endpoints
		// directly; otherwise surface the discovery error.
		metadata = authServerMetadata{}
	}
	if protectedResourceDiscovery {
		if strings.TrimSpace(metadata.Issuer) == "" {
			return authServerMetadata{}, errors.New("mcp oauth: authorization server metadata is missing its required issuer")
		}
		if metadata.Issuer != discoveryBase {
			return authServerMetadata{}, errors.New("mcp oauth: authorization server metadata issuer does not match protected resource metadata")
		}
	}

	if endpoint := strings.TrimSpace(cfg.AuthorizationEndpoint); endpoint != "" {
		metadata.AuthorizationEndpoint = endpoint
	}
	if endpoint := strings.TrimSpace(cfg.TokenEndpoint); endpoint != "" {
		metadata.TokenEndpoint = endpoint
	}
	if endpoint := strings.TrimSpace(cfg.RegistrationEndpoint); endpoint != "" {
		metadata.RegistrationEndpoint = endpoint
	}
	if protectedResourceDiscovery {
		metadata.protectedResourceURL = strings.TrimSpace(baseURL)
		metadata.protectAuthorizationEndpoint = strings.TrimSpace(cfg.AuthorizationEndpoint) == ""
		metadata.protectTokenEndpoint = strings.TrimSpace(cfg.TokenEndpoint) == ""
		metadata.protectRegistrationEndpoint = strings.TrimSpace(cfg.RegistrationEndpoint) == ""
	}

	if strings.TrimSpace(metadata.AuthorizationEndpoint) == "" {
		return authServerMetadata{}, errors.New("no authorization endpoint discovered or configured")
	}
	if strings.TrimSpace(metadata.TokenEndpoint) == "" {
		return authServerMetadata{}, errors.New("no token endpoint discovered or configured")
	}
	if err := validateResolvedEndpoints(metadata); err != nil {
		return authServerMetadata{}, err
	}
	if err := validateProtectedDiscoveredEndpoints(ctx, metadata); err != nil {
		return authServerMetadata{}, err
	}
	return metadata, nil
}

// newPKCE generates a high-entropy code verifier and its S256 challenge via the
// shared engine.
func newPKCE() (pkceParams, error) {
	p, err := oauth.NewPKCE()
	if err != nil {
		return pkceParams{}, err
	}
	return pkceToParams(p), nil
}

func newState() (string, error) {
	return oauth.NewState()
}

// authorizationURL builds the authorization request URL via the shared engine.
func (flow *authorizationFlow) authorizationURL(redirectURI string) (string, error) {
	return oauth.BuildAuthorizationURL(configFor(flow.config, flow.metadata), pkceToOAuth(flow.pkce), flow.state, redirectURI, nil)
}

// parseCallback validates the redirect query and returns the authorization
// code. It rejects a mismatched state (CSRF) and surfaces provider errors.
func (flow *authorizationFlow) parseCallback(values url.Values) (string, error) {
	if got := values.Get("state"); got != flow.state {
		return "", errors.New("OAuth callback state mismatch: possible CSRF, login aborted")
	}
	if providerErr := strings.TrimSpace(values.Get("error")); providerErr != "" {
		description := strings.TrimSpace(values.Get("error_description"))
		if description != "" {
			return "", fmt.Errorf("authorization server returned error %q: %s", providerErr, description)
		}
		return "", fmt.Errorf("authorization server returned error %q", providerErr)
	}
	code := strings.TrimSpace(values.Get("code"))
	if code == "" {
		return "", errors.New("OAuth callback missing authorization code")
	}
	return code, nil
}

// exchangeCode swaps an authorization code + PKCE verifier for tokens via the
// shared engine.
func (flow *authorizationFlow) exchangeCode(ctx context.Context, code string, redirectURI string) (StoredToken, error) {
	token, err := oauth.ExchangeCode(ctx, flow.httpClient, configFor(flow.config, flow.metadata), code, flow.pkce.Verifier, redirectURI, flow.now)
	if err != nil {
		return StoredToken{}, err
	}
	return tokenToStored(token), nil
}

// refreshAccessToken exchanges a refresh token for a fresh access token via the
// shared engine. A response that omits a new refresh token preserves the
// previous one.
func refreshAccessToken(ctx context.Context, client *http.Client, cfg OAuthConfig, current StoredToken, now func() time.Time) (StoredToken, error) {
	token, err := oauth.Refresh(ctx, client, oauth.Config{
		ClientID:      cfg.ClientID,
		ClientSecret:  cfg.ClientSecret,
		Scopes:        cfg.Scopes,
		TokenEndpoint: cfg.TokenEndpoint,
	}, oauth.Token{AccessToken: current.AccessToken, RefreshToken: current.RefreshToken, TokenType: current.TokenType, Scopes: current.Scopes, ExpiresAt: current.ExpiresAt}, now)
	if err != nil {
		return StoredToken{}, err
	}
	return tokenToStored(token), nil
}

// registerClient performs dynamic client registration against the registration
// endpoint and returns the issued client_id and optional client_secret.
func registerClient(ctx context.Context, client *http.Client, registrationEndpoint string, redirectURI string, scopes []string) (string, string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	payload := map[string]any{
		"client_name":                "zero",
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	}
	if len(scopes) > 0 {
		payload["scope"] = strings.Join(scopes, " ")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, registrationEndpoint, strings.NewReader(string(body)))
	if err != nil {
		return "", "", err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")

	response, err := client.Do(request)
	if err != nil {
		return "", "", fmt.Errorf("client registration failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", "", fmt.Errorf("client registration returned HTTP %d", response.StatusCode)
	}
	var registered struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&registered); err != nil {
		return "", "", fmt.Errorf("decode client registration response: %w", err)
	}
	if strings.TrimSpace(registered.ClientID) == "" {
		return "", "", errors.New("client registration returned no client_id")
	}
	return registered.ClientID, registered.ClientSecret, nil
}

// Login runs the full OAuth 2.0 + PKCE authorization-code flow: it discovers (or
// falls back to configured) endpoints, optionally registers a client, starts a
// loopback redirect listener, opens the authorization URL, validates the
// callback state, and exchanges the code for tokens. Tokens are returned and are
// never logged.
func Login(ctx context.Context, options LoginOptions) (StoredToken, error) {
	if err := ValidateServerName(options.ServerName); err != nil {
		return StoredToken{}, err
	}
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = defaultLoginTimeout
	}
	// One deadline bounds the WHOLE interactive login — discovery, optional client
	// registration, the callback wait, and the code exchange — so a hung
	// metadata/registration/token endpoint can't block the command forever (the
	// CLI passes a non-cancelable context).
	loginCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cfg := options.Config
	metadata, err := resolveAuthorizationServer(loginCtx, httpClient, options.ServerURL, cfg)
	if err != nil {
		return StoredToken{}, err
	}

	// Bind the loopback redirect listener first so the redirect URI is known
	// before client registration and authorization URL construction.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return StoredToken{}, fmt.Errorf("start loopback redirect listener: %w", err)
	}
	defer listener.Close()
	redirectURI := fmt.Sprintf("http://%s/callback", listener.Addr().String())
	protectedEndpointClient := httpClient
	if metadata.protectRegistrationEndpoint || metadata.protectTokenEndpoint {
		protectedEndpointClient, err = newAdvertisedOAuthDiscoveryClient(httpClient, metadata.protectedResourceURL)
		if err != nil {
			return StoredToken{}, err
		}
	}
	tokenClient := httpClient
	if metadata.protectTokenEndpoint {
		tokenClient = protectedEndpointClient
	}

	if strings.TrimSpace(cfg.ClientID) == "" {
		if registration := strings.TrimSpace(metadata.RegistrationEndpoint); registration != "" {
			registrationClient := httpClient
			if metadata.protectRegistrationEndpoint {
				registrationClient = protectedEndpointClient
			}
			clientID, clientSecret, regErr := registerClient(loginCtx, registrationClient, registration, redirectURI, cfg.Scopes)
			if regErr != nil {
				return StoredToken{}, regErr
			}
			cfg.ClientID = clientID
			if clientSecret != "" {
				cfg.ClientSecret = clientSecret
			}
		}
	}
	if strings.TrimSpace(cfg.ClientID) == "" {
		return StoredToken{}, errors.New("no client_id configured and dynamic registration unavailable")
	}

	pkce, err := newPKCE()
	if err != nil {
		return StoredToken{}, err
	}
	state, err := newState()
	if err != nil {
		return StoredToken{}, err
	}

	flow := &authorizationFlow{
		httpClient: tokenClient,
		metadata:   metadata,
		config:     cfg,
		pkce:       pkce,
		state:      state,
		now:        now,
	}

	authURL, err := flow.authorizationURL(redirectURI)
	if err != nil {
		return StoredToken{}, err
	}

	type callbackResult struct {
		code string
		err  error
	}
	resultChan := make(chan callbackResult, 1)
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/callback" {
				http.NotFound(w, r)
				return
			}
			code, parseErr := flow.parseCallback(r.URL.Query())
			if parseErr != nil {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, "Authorization failed. You may close this window.")
			} else {
				_, _ = io.WriteString(w, "Authorization complete. You may close this window.")
			}
			select {
			case resultChan <- callbackResult{code: code, err: parseErr}:
			default:
			}
		}),
	}
	go func() { _ = server.Serve(listener) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	open := options.OpenBrowser
	if open == nil {
		open = func(string) error { return nil }
	}
	if err := open(authURL); err != nil {
		return StoredToken{}, fmt.Errorf("open authorization URL: %w", err)
	}

	select {
	case result := <-resultChan:
		if result.err != nil {
			return StoredToken{}, result.err
		}
		token, err := flow.exchangeCode(loginCtx, result.code, redirectURI)
		if err != nil {
			return StoredToken{}, err
		}
		return token, nil
	case <-loginCtx.Done():
		return StoredToken{}, fmt.Errorf("timed out waiting for OAuth authorization callback: %w", loginCtx.Err())
	}
}
