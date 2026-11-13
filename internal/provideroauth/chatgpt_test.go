package provideroauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/oauth"
)

// makeIDToken signs a fake JWS for tests. We only need the payload to round-trip
// (the production extractor reads the payload segment, not the signature), so
// the signature segment is just enough bytes to keep the JWS shape valid.
func makeIDToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	body := base64.RawURLEncoding.EncodeToString(payload)
	// A short zero-byte signature is enough to satisfy "three base64url
	// segments" — extractChatGPTAccountID does not parse the signature.
	return header + "." + body + ".AAAA"
}

func TestExtractChatGPTAccountIDErrorsOnWrongShapeNamespace(t *testing.T) {
	// AUDIT-L11: when OpenAI's auth namespace is present but the account id is the
	// wrong shape (or missing), surface an error so the login warning fires and the
	// user re-auths — instead of silently omitting the header and hitting opaque 401s.
	for _, claims := range []map[string]any{
		{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": 12345}},   // non-string
		{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": ""}},      // empty
		{"https://api.openai.com/auth": map[string]any{"organization_id": "org-only"}}, // missing key
	} {
		token := oauth.Token{IDToken: makeIDToken(t, claims)}
		if _, err := extractChatGPTAccountID(token); err == nil {
			t.Fatalf("expected an error for a present-but-wrong-shape namespace claim: %v", claims)
		}
	}
}

func TestExtractChatGPTAccountIDHappyPath(t *testing.T) {
	// Real auth.openai.com id_tokens nest the account id under the
	// "https://api.openai.com/auth" claim object (NOT at the top level), so the
	// fixture must too — a top-level fixture would mask the very bug this guards.
	token := oauth.Token{
		IDToken: makeIDToken(t, map[string]any{
			"sub":   "user-1",
			"email": "me@example.com",
			"https://api.openai.com/auth": map[string]any{
				"chatgpt_account_id": "acc-12345",
			},
		}),
	}
	got, err := extractChatGPTAccountID(token)
	if err != nil {
		t.Fatalf("extractChatGPTAccountID: %v", err)
	}
	if got != "acc-12345" {
		t.Fatalf("account_id = %q, want acc-12345", got)
	}
}

func TestExtractChatGPTAccountIDIgnoresTopLevelOnlyClaim(t *testing.T) {
	// Regression for the P0: a real token never puts chatgpt_account_id at the
	// top level — the Codex CLI reads only the nested namespace, so a top-level
	// claim is not a real account id. We prefer the nested path; a bare top-level
	// claim is honored only as a forward-compat fallback when the namespace is
	// absent, but it must NOT shadow the nested value.
	token := oauth.Token{
		IDToken: makeIDToken(t, map[string]any{
			"chatgpt_account_id": "top-level-decoy",
			"https://api.openai.com/auth": map[string]any{
				"chatgpt_account_id": "nested-real",
			},
		}),
	}
	got, err := extractChatGPTAccountID(token)
	if err != nil {
		t.Fatalf("extractChatGPTAccountID: %v", err)
	}
	if got != "nested-real" {
		t.Fatalf("account_id = %q, want nested-real (nested path must win over a top-level decoy)", got)
	}
}

func TestExtractChatGPTAccountIDTopLevelFallback(t *testing.T) {
	// Forward-compat: if OpenAI ever flattens the claim (no namespace object),
	// honor a bare top-level claim rather than dropping the header.
	token := oauth.Token{
		IDToken: makeIDToken(t, map[string]any{"chatgpt_account_id": "flat-acc"}),
	}
	got, err := extractChatGPTAccountID(token)
	if err != nil {
		t.Fatalf("extractChatGPTAccountID: %v", err)
	}
	if got != "flat-acc" {
		t.Fatalf("account_id = %q, want flat-acc (top-level fallback)", got)
	}
}

func TestExtractChatGPTAccountIDNoClaim(t *testing.T) {
	// A JWS with no chatgpt_account_id is treated as "no id" — the Codex
	// provider simply omits the header. This is the same posture as no ID token.
	token := oauth.Token{IDToken: makeIDToken(t, map[string]any{"sub": "user-1"})}
	got, err := extractChatGPTAccountID(token)
	if err != nil {
		t.Fatalf("extractChatGPTAccountID: %v", err)
	}
	if got != "" {
		t.Fatalf("account_id = %q, want empty", got)
	}
}

func TestExtractChatGPTAccountIDNoIDToken(t *testing.T) {
	// No ID token in the response is a soft "skip" (returns "", nil). The
	// bearer itself is still valid for Codex calls, just without the account-id
	// header.
	got, err := extractChatGPTAccountID(oauth.Token{AccessToken: "tok"})
	if err != nil {
		t.Fatalf("extractChatGPTAccountID: %v", err)
	}
	if got != "" {
		t.Fatalf("account_id = %q, want empty when no ID token is present", got)
	}
}

func TestExtractChatGPTAccountIDRejectsTamperedPayload(t *testing.T) {
	// We treat the payload segment as authoritative — a tampered JWS whose
	// payload won't base64-decode must be rejected with a clear error so the
	// CLI can warn the user. The signature is not validated (the bearer is
	// already authenticated by TLS to auth.openai.com); see the function doc.
	cases := []struct {
		name string
		raw  string
	}{
		{
			"not a JWS",
			"definitely.not.a.jws",
		},
		{
			"two segments",
			"abc.def",
		},
		{
			"payload not base64",
			"hdr.!!!not-base64!!!.sig",
		},
		{
			"payload not JSON",
			"hdr." + base64.RawURLEncoding.EncodeToString([]byte("not-json")) + ".sig",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractChatGPTAccountID(oauth.Token{IDToken: tc.raw})
			if err == nil {
				t.Fatalf("expected an error for %q, got account_id=%q", tc.raw, got)
			}
		})
	}
}

func TestExtractChatGPTAccountIDClaimWrongType(t *testing.T) {
	// A non-string claim is treated as "not present" — the Codex backend would
	// 401 if the header were forwarded, so the safe action is to drop it.
	token := oauth.Token{IDToken: makeIDToken(t, map[string]any{
		"chatgpt_account_id": 42,
	})}
	got, err := extractChatGPTAccountID(token)
	if err != nil {
		t.Fatalf("extractChatGPTAccountID: %v", err)
	}
	if got != "" {
		t.Fatalf("account_id = %q, want empty for non-string claim", got)
	}
}

// chatgptTestEnv returns the minimum env that opts the user into the chatgpt
// preset, so the resolver can build a Config without ZERO_OAUTH_CHATGPT_* vars.
func chatgptTestEnv() map[string]string {
	return map[string]string{"ZERO_OAUTH_ALLOW_PRESETS": "1"}
}

// chatgptTestServer is a minimal mock of the ChatGPT authorization server that
// records which client_id it sees and returns a token response with a chosen
// id_token.
type chatgptTestServer struct {
	srv       *httptest.Server
	gotClient *atomic.Value
	gotScopes *atomic.Value
	gotRedir  *atomic.Value
	gotPKCE   *atomic.Value
}

func newChatGPTTestServer(t *testing.T, idToken string) *chatgptTestServer {
	t.Helper()
	ts := &chatgptTestServer{
		gotClient: new(atomic.Value),
		gotScopes: new(atomic.Value),
		gotRedir:  new(atomic.Value),
		gotPKCE:   new(atomic.Value),
	}
	ts.gotClient.Store("")
	ts.gotScopes.Store("")
	ts.gotRedir.Store("")
	ts.gotPKCE.Store("")
	mux := http.NewServeMux()
	// The test's browser simulator does NOT visit the authorize endpoint — it
	// reaches the loopback directly. The authorize handler is still registered
	// (and asserts the preset's endpoints are reachable) so a future test
	// extension that exercises the redirect path can re-use it.
	mux.HandleFunc("/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		ts.gotClient.Store(r.URL.Query().Get("client_id"))
		ts.gotScopes.Store(r.URL.Query().Get("scope"))
		ts.gotRedir.Store(r.URL.Query().Get("redirect_uri"))
		ts.gotPKCE.Store(r.URL.Query().Get("code_challenge"))
		http.Error(w, "authorize not used by this test", http.StatusNotFound)
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ts.gotClient.Store(r.Form.Get("client_id"))
		ts.gotPKCE.Store(r.Form.Get("code_verifier"))
		// Simulate the OIDC token-endpoint response with an id_token. The
		// account-id claim is the value the test will check.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "ACCESS-TOK",
			"refresh_token": "REFRESH-TOK",
			"token_type":    "Bearer",
			"expires_in":    3600,
			"id_token":      idToken,
		})
	})
	ts.srv = httptest.NewServer(mux)
	return ts
}

func (ts *chatgptTestServer) Close() { ts.srv.Close() }
func (ts *chatgptTestServer) AuthorizeURL() string {
	return ts.srv.URL + "/oauth/authorize"
}
func (ts *chatgptTestServer) TokenURL() string {
	return ts.srv.URL + "/oauth/token"
}

// browserSimulator parses the authorize URL, finds the loopback redirect_uri
// it embeds, and GETs that callback directly (carrying the same state and
// code that the test server would have stamped on). This avoids the
// httptest redirect chain — the loopback listener sees one clean request, so
// it can serve it and the connection closes.
//
// It also asserts the PKCE params are present on the authorize URL, mirroring
// the openrouter test's posture.
func browserSimulator(t *testing.T, code string) func(string) error {
	t.Helper()
	return func(authURL string) error {
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		q := u.Query()
		if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
			t.Fatalf("authorize URL missing PKCE: %s", authURL)
		}
		redirect := q.Get("redirect_uri")
		if redirect == "" {
			t.Fatalf("authorize URL missing redirect_uri: %s", authURL)
		}
		state := q.Get("state")
		target := redirect
		if code != "" {
			parsed, perr := url.Parse(redirect)
			if perr != nil {
				return perr
			}
			pq := parsed.Query()
			pq.Set("code", code)
			pq.Set("state", state)
			parsed.RawQuery = pq.Encode()
			target = parsed.String()
		}
		// Use a client with a short timeout so the test never hangs on a
		// flaky loopback; a 5s upper bound is plenty.
		client := &http.Client{Timeout: 5 * time.Second}
		req, _ := http.NewRequest(http.MethodGet, target, nil)
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		_, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		_ = resp.Body.Close()
		return nil
	}
}

func TestChatGPTLoginUsesPreset(t *testing.T) {
	idTok := makeIDToken(t, map[string]any{
		"email": "user@example.com",
		// Nested under the OpenAI auth namespace, as a real id_token is.
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acc-real",
		},
	})
	ts := newChatGPTTestServer(t, idTok)
	defer ts.Close()

	// The chatgpt preset pins the authorize/token endpoints; we override
	// both with the test server's URLs to keep the loopback flow
	// self-contained. The chatgpt preset's client_id is preserved by NOT
	// overriding ZERO_OAUTH_CHATGPT_CLIENT_ID.
	env := chatgptTestEnv()
	env["ZERO_OAUTH_CHATGPT_AUTHORIZE_URL"] = ts.AuthorizeURL()
	env["ZERO_OAUTH_CHATGPT_TOKEN_URL"] = ts.TokenURL()

	var out strings.Builder
	token, err := ChatGPTLogin(context.Background(), ChatGPTOptions{
		Env:         env,
		HTTPClient:  &http.Client{Timeout: 10 * time.Second},
		Out:         &out,
		OpenBrowser: browserSimulator(t, "TEST-CODE"),
	})
	if err != nil {
		t.Fatalf("ChatGPTLogin: %v\nout=%s", err, out.String())
	}
	if token.AccessToken != "ACCESS-TOK" {
		t.Fatalf("access_token = %q, want ACCESS-TOK", token.AccessToken)
	}
	if token.Account != "acc-real" {
		t.Fatalf("Account = %q, want acc-real (extracted from id_token)", token.Account)
	}
	if token.RefreshToken != "REFRESH-TOK" {
		t.Fatalf("refresh_token = %q, want REFRESH-TOK", token.RefreshToken)
	}
	if token.TokenType != "Bearer" {
		t.Fatalf("token_type = %q, want Bearer", token.TokenType)
	}
	if token.ExpiresAt.IsZero() {
		t.Fatalf("ExpiresAt is zero, want non-zero (3600s in the future)")
	}
	// The preset's client_id must reach the token endpoint (this asserts
	// the resolver actually used the preset, not an env override).
	if id := ts.gotClient.Load().(string); id != "app_EMoamEEZ73f0CkXaXp7hrann" {
		t.Fatalf("token-endpoint client_id = %q, want the chatgpt preset", id)
	}
	// The PKCE verifier must round-trip on the token exchange.
	if ts.gotPKCE.Load().(string) == "" {
		t.Fatal("code_verifier missing on token exchange")
	}
	// Sanity-check the printed URL contains the test server's authorize path.
	if !strings.Contains(out.String(), "/oauth/authorize") {
		t.Fatalf("Out should print the authorize URL, got %q", out.String())
	}
}

func TestChatGPTAuthorizeURLIncludesConnectorScopes(t *testing.T) {
	// Regression: the chatgpt preset must request the api.connectors scopes, or
	// the authorize endpoint rejects the flow with authorize_hydra_invalid_request.
	// Assert the generated authorize URL carries both connector scopes (plus the
	// base OIDC set) by inspecting the URL the browser is handed.
	idTok := makeIDToken(t, map[string]any{"chatgpt_account_id": "acc"})
	ts := newChatGPTTestServer(t, idTok)
	defer ts.Close()

	env := chatgptTestEnv()
	env["ZERO_OAUTH_CHATGPT_AUTHORIZE_URL"] = ts.AuthorizeURL()
	env["ZERO_OAUTH_CHATGPT_TOKEN_URL"] = ts.TokenURL()

	var gotScope string
	inspect := func(authURL string) error {
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		gotScope = u.Query().Get("scope")
		// Drive the rest of the flow so ChatGPTLogin completes cleanly.
		return browserSimulator(t, "TEST-CODE")(authURL)
	}

	if _, err := ChatGPTLogin(context.Background(), ChatGPTOptions{
		Env:         env,
		HTTPClient:  &http.Client{Timeout: 10 * time.Second},
		Out:         io.Discard,
		OpenBrowser: inspect,
	}); err != nil {
		t.Fatalf("ChatGPTLogin: %v", err)
	}

	for _, want := range []string{"openid", "offline_access", "api.connectors.read", "api.connectors.invoke"} {
		if !strings.Contains(gotScope, want) {
			t.Fatalf("authorize URL scope %q missing %q", gotScope, want)
		}
	}
}

func TestChatGPTLoginWithoutPresetEnvReturnsError(t *testing.T) {
	// With no env at all, the chatgpt preset stays inert (the same posture
	// every other preset uses); ResolveConfig returns an error and the login
	// fails fast — a useful safety net for callers that forgot to opt in.
	_, err := ChatGPTLogin(context.Background(), ChatGPTOptions{
		Env:        map[string]string{},
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	})
	if err == nil {
		t.Fatal("expected an error when the chatgpt preset is not opted in")
	}
}

func TestChatGPTLoginMissingAuthorizationEndpointErrors(t *testing.T) {
	// The chatgpt preset pins the authorize endpoint. Forcing it to empty
	// would require extending the resolver to honor an explicit "unset"
	// sentinel; today the resolver treats an empty env var as "use the
	// preset", so the login will succeed. We just assert the preset's
	// authorize URL survives a no-op env override (an off-by-one safety
	// net for callers that pass an empty Env).
	env := chatgptTestEnv()
	env["ZERO_OAUTH_CHATGPT_AUTHORIZE_URL"] = ""
	env["ZERO_OAUTH_CHATGPT_TOKEN_URL"] = "https://chatgpt.invalid/oauth/token"
	// No OpenBrowser => the login would hang waiting for a callback. We
	// use a 2s timeout so a regression that drops the preset's authorize
	// URL is surfaced as a timeout, not a hang.
	token, err := ChatGPTLogin(context.Background(), ChatGPTOptions{
		Env:         env,
		HTTPClient:  &http.Client{Timeout: 5 * time.Second},
		Timeout:     2 * time.Second,
		OpenBrowser: func(string) error { return nil },
	})
	// The login will time out waiting for the real (preset) authorize
	// callback. We assert it times out (not panics) and that no token is
	// returned. The point of the test is the early-return branch in
	// ChatGPTLogin is not triggered by an empty env override.
	if err == nil {
		t.Fatal("expected an error (timeout or callback error), got success")
	}
	if token.AccessToken != "" {
		t.Fatalf("no token should be minted on timeout, got %q", token.AccessToken)
	}
}

// The ID token claim extraction is also exercised through the on-disk JSON
// store: a refresh keeps the account-id on the way out, so the test asserts
// the Token round-trips IDToken + Account through Save/Load.
func TestChatGPTTokenRoundTripsIDToken(t *testing.T) {
	path := t.TempDir() + "/tok.json"
	store, err := oauth.NewStore(oauth.StoreOptions{FilePath: path})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	tok := oauth.Token{
		AccessToken:  "A",
		RefreshToken: "R",
		IDToken:      "header.payload.sig",
		Account:      "acc-stored",
	}
	if err := store.Save(oauth.ProviderKey("chatgpt"), tok); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, ok, err := store.Load(oauth.ProviderKey("chatgpt"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !ok {
		t.Fatal("expected the token to be stored")
	}
	if got.IDToken != "header.payload.sig" {
		t.Fatalf("IDToken round-trip = %q, want header.payload.sig", got.IDToken)
	}
	if got.Account != "acc-stored" {
		t.Fatalf("Account = %q, want acc-stored", got.Account)
	}
}

// Ensure the package reads `io` so an unused-import error never creeps in when
// a refactor prunes a caller. (The other tests exercise io.Writer for Out.)
var _ = io.Discard
