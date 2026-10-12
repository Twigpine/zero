// Package provideroauth implements provider-specific OAuth login flows that don't
// fit the generic env-driven engine in internal/oauth — notably OpenRouter's
// bespoke PKCE flow, which mints a normal API key rather than returning an OAuth
// bearer token. These flows are launched from the setup wizard / CLI so a user
// can "log in with the browser" instead of pasting a key.
package provideroauth

import (
	"bytes"
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

const (
	openRouterDefaultBaseURL  = "https://openrouter.ai"
	openRouterAuthPath        = "/auth"
	openRouterKeyExchangePath = "/api/v1/auth/keys"
	openRouterMaxBody         = 1 << 20
)

// OpenRouterOptions configures the OpenRouter login flow.
type OpenRouterOptions struct {
	// BaseURL overrides https://openrouter.ai (for tests).
	BaseURL string
	// HTTPClient performs the key exchange; nil => a client with a sane timeout.
	HTTPClient *http.Client
	// OpenBrowser is invoked with the authorize URL. When nil the URL is only
	// printed (to Out) for the user to open manually.
	OpenBrowser func(authURL string) error
	// Out receives the "open this URL" line; nil => the URL is not printed.
	Out io.Writer
	// Timeout bounds the whole interactive login; 0 => 5 minutes.
	Timeout time.Duration
}

// OpenRouterLogin runs OpenRouter's loopback PKCE flow and returns a freshly
// minted OpenRouter API key. The flow needs no client_id: a local callback
// captures the authorization code, which is exchanged (with the PKCE verifier)
// at /api/v1/auth/keys for a key. PKCE (S256) binds the code to this client, so
// no separate CSRF state is required. The returned key is a normal credential
// the caller stores as the provider's API key.
func OpenRouterLogin(ctx context.Context, opts OpenRouterOptions) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	if base == "" {
		base = openRouterDefaultBaseURL
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	pkce, err := oauth.NewPKCE()
	if err != nil {
		return "", err
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("provideroauth: start loopback listener: %w", err)
	}
	defer func() { _ = listener.Close() }()
	callbackURL := fmt.Sprintf("http://%s/callback", listener.Addr().String())

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	// The shared callback server bounds header reads, force-closes a client
	// still connected when shutdown runs out of time, and waits for Serve to
	// return. The bare http.Server this used to build did none of that, so a
	// client stalled mid-header outlived the login (#1028).
	server := oauth.StartCallbackServer(listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		if code := strings.TrimSpace(r.URL.Query().Get("code")); code != "" {
			_, _ = io.WriteString(w, "OpenRouter authorization complete. You may close this window.")
			select {
			case codeCh <- code:
			default:
			}
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "Authorization failed. You may close this window.")
		select {
		case errCh <- errors.New("provideroauth: callback missing authorization code"):
		default:
		}
	}))
	defer server.Close()

	authURL := base + openRouterAuthPath + "?" + url.Values{
		"callback_url":          {callbackURL},
		"code_challenge":        {pkce.Challenge},
		"code_challenge_method": {oauth.MethodS256},
	}.Encode()
	if opts.Out != nil {
		fmt.Fprintf(opts.Out, "Open this URL to authorize OpenRouter:\n  %s\n", authURL)
	}
	if opts.OpenBrowser != nil {
		if err := opts.OpenBrowser(authURL); err != nil {
			return "", fmt.Errorf("provideroauth: open authorization URL: %w", err)
		}
	}

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return "", err
	case <-ctx.Done():
		return "", fmt.Errorf("provideroauth: timed out waiting for OpenRouter authorization: %w", ctx.Err())
	}

	return openRouterExchange(ctx, client, base, code, pkce.Verifier)
}

// openRouterExchange swaps the authorization code + PKCE verifier for an API key.
func openRouterExchange(ctx context.Context, client *http.Client, base, code, verifier string) (string, error) {
	payload, err := json.Marshal(map[string]string{
		"code":                  code,
		"code_verifier":         verifier,
		"code_challenge_method": oauth.MethodS256,
	})
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+openRouterKeyExchangePath, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")

	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("provideroauth: openrouter key exchange: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(response.Body, openRouterMaxBody))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("provideroauth: openrouter key exchange returned HTTP %d", response.StatusCode)
	}
	var parsed struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("provideroauth: decode openrouter key response: %w", err)
	}
	key := strings.TrimSpace(parsed.Key)
	if key == "" {
		return "", errors.New("provideroauth: openrouter returned an empty key")
	}
	return key, nil
}
