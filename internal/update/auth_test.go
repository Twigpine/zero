package update

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// releaseJSON returns a minimal GitHub release JSON body.
func releaseJSON(tag string) string {
	return fmt.Sprintf(`{"tag_name":"%s","html_url":"https://example.test/release","assets":[{"name":"%s-linux-x64.tar.gz","browser_download_url":"https://example.test/%s-linux-x64.tar.gz"}]}`, tag, tag, tag)
}

// fakeTransport records the Authorization header from the first request it receives
// and returns a canned response. Used to verify auth is sent for GitHub URLs
// without needing a real TLS server.
type fakeTransport struct {
	gotAuth    string
	statusCode int
	body       string
}

func (ft *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ft.gotAuth = req.Header.Get("Authorization")
	if ft.statusCode == 0 {
		ft.statusCode = 200
	}
	if ft.body == "" {
		ft.body = releaseJSON("v0.2.0")
	}
	return &http.Response{
		StatusCode: ft.statusCode,
		Body:       io.NopCloser(strings.NewReader(ft.body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func TestFetchReleaseSendsAuthToHttpsGithub(t *testing.T) {
	ft := &fakeTransport{}
	old := httpClient
	httpClient = &http.Client{Transport: ft}
	t.Cleanup(func() { httpClient = old })

	t.Setenv(EnvUpdateToken, "zero_token")
	t.Setenv(EnvGitHubToken, "github_fallback")

	_, err := fetchRelease(context.Background(), "https://api.github.com/repos/Gitlawb/zero/releases/latest")
	if err != nil {
		t.Fatalf("fetchRelease error: %v", err)
	}
	if ft.gotAuth != "Bearer zero_token" {
		t.Fatalf("expected Bearer zero_token, got %q", ft.gotAuth)
	}
}

func TestFetchReleaseSendsAuthCaseInsensitive(t *testing.T) {
	ft := &fakeTransport{}
	old := httpClient
	httpClient = &http.Client{Transport: ft}
	t.Cleanup(func() { httpClient = old })

	t.Setenv(EnvUpdateToken, "zero_token")
	t.Setenv(EnvGitHubToken, "")

	_, err := fetchRelease(context.Background(), "https://API.GITHUB.COM/repos/Gitlawb/zero/releases/latest")
	if err != nil {
		t.Fatalf("fetchRelease error: %v", err)
	}
	if ft.gotAuth != "Bearer zero_token" {
		t.Fatalf("expected Bearer zero_token, got %q", ft.gotAuth)
	}
}

func TestFetchReleaseFallsBackToGithubToken(t *testing.T) {
	ft := &fakeTransport{}
	old := httpClient
	httpClient = &http.Client{Transport: ft}
	t.Cleanup(func() { httpClient = old })

	t.Setenv(EnvGitHubToken, "fallback_token")
	t.Setenv(EnvUpdateToken, "")

	_, err := fetchRelease(context.Background(), "https://api.github.com/repos/Gitlawb/zero/releases/latest")
	if err != nil {
		t.Fatalf("fetchRelease error: %v", err)
	}
	if ft.gotAuth != "Bearer fallback_token" {
		t.Fatalf("expected Bearer fallback_token, got %q", ft.gotAuth)
	}
}

func TestFetchReleaseNoAuthToCustomEndpoint(t *testing.T) {
	var gotAuth string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, releaseJSON("v0.2.0"))
	}))
	defer srv.Close()

	old := httpClient
	httpClient = srv.Client()
	t.Cleanup(func() { httpClient = old })

	t.Setenv(EnvUpdateToken, "secret")
	t.Setenv(EnvGitHubToken, "fallback")

	_, err := fetchRelease(context.Background(), srv.URL+"/releases/latest")
	if err != nil {
		t.Fatalf("fetchRelease error: %v", err)
	}
	if gotAuth != "" {
		t.Fatalf("expected no auth for non-GitHub endpoint, got %q", gotAuth)
	}
}

func TestFetchReleaseNoAuthToHttpGithub(t *testing.T) {
	// Verify the scheme guard independently of the hostname guard: an http://
	// api.github.com request must not carry a token even when tokens are
	// configured. Use the transport seam with the logical URL so the
	// githubAPIToken precondition (https + api.github.com) is exercised.
	calls := 0
	var firstAuth, firstURL string
	tr := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			firstAuth = req.Header.Get("Authorization")
			firstURL = req.URL.String()
		}
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(releaseJSON("v0.2.0"))),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})

	old := httpClient
	httpClient = &http.Client{Transport: tr}
	t.Cleanup(func() { httpClient = old })

	t.Setenv(EnvUpdateToken, "secret")
	t.Setenv(EnvGitHubToken, "fallback")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := fetchRelease(ctx, "http://api.github.com/repos/Gitlawb/zero/releases/latest")
	if err != nil {
		t.Fatalf("fetchRelease error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 request, got %d", calls)
	}
	if firstAuth != "" {
		t.Fatalf("auth should not be sent over HTTP even with tokens: got %q", firstAuth)
	}
	if firstURL != "http://api.github.com/repos/Gitlawb/zero/releases/latest" {
		t.Fatalf("unexpected URL %q", firstURL)
	}
}

// roundTripperFunc adapts a function to http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestFetchReleaseNoAuthWhenTokensNotSet(t *testing.T) {
	ft := &fakeTransport{}
	old := httpClient
	httpClient = &http.Client{Transport: ft}
	t.Cleanup(func() { httpClient = old })

	t.Setenv(EnvUpdateToken, "")
	t.Setenv(EnvGitHubToken, "")

	_, err := fetchRelease(context.Background(), "https://api.github.com/repos/Gitlawb/zero/releases/latest")
	if err != nil {
		t.Fatalf("fetchRelease error: %v", err)
	}
	if ft.gotAuth != "" {
		t.Fatalf("expected no auth when no tokens set, got %q", ft.gotAuth)
	}
}

func TestFetchReleaseRefusesRedirectToHttp(t *testing.T) {
	calls := 0
	var firstAuth, firstURL string
	tr := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			firstAuth = req.Header.Get("Authorization")
			firstURL = req.URL.String()
			return &http.Response{
				StatusCode: 301,
				Header:     http.Header{"Location": []string{"http://api.github.com/repos/Gitlawb/zero/releases/latest"}},
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    req,
			}, nil
		}
		t.Fatalf("HTTP downgrade destination should not be contacted: %s %s calls=%d", req.Method, req.URL.String(), calls)
		return nil, fmt.Errorf("unexpected request to HTTP destination")
	})

	old := httpClient
	httpClient = &http.Client{Transport: tr}
	t.Cleanup(func() { httpClient = old })

	t.Setenv(EnvUpdateToken, "secret")
	t.Setenv(EnvGitHubToken, "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := fetchRelease(ctx, "https://api.github.com/repos/Gitlawb/zero/releases/latest")
	if err == nil || !strings.Contains(err.Error(), "refusing redirect") {
		t.Fatalf("expected redirect refusal error, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 transport call, HTTP destination should not be contacted: got %d", calls)
	}
	if firstAuth != "Bearer secret" {
		t.Fatalf("initial HTTPS request should carry Bearer token: got %q", firstAuth)
	}
	if firstURL != "https://api.github.com/repos/Gitlawb/zero/releases/latest" {
		t.Fatalf("unexpected initial URL %q", firstURL)
	}
}

func TestFetchReleaseRejectsUserinfoHostTrick(t *testing.T) {
	// Userinfo trick: "https://api.github.com@evil.example.com/..." — the Go
	// URL parser treats "api.github.com" as userinfo, making Hostname() =
	// "evil.example.com". githubAPIToken sees a non-GitHub host and returns "",
	// so no auth is sent.
	ft := &fakeTransport{}
	old := httpClient
	httpClient = &http.Client{Transport: ft}
	t.Cleanup(func() { httpClient = old })

	t.Setenv(EnvUpdateToken, "secret")
	t.Setenv(EnvGitHubToken, "")

	_, _ = fetchRelease(context.Background(), "https://api.github.com@evil.example.com/repos/Gitlawb/zero/releases/latest")
	if strings.HasPrefix(ft.gotAuth, "Bearer ") {
		t.Fatalf("bearer auth should not be sent when userinfo tricks hostname, got %q", ft.gotAuth)
	}
}

// --- Redirect regression tests (jatmn review) ---

func TestHTTPMirrorRedirectAllowedWithoutCredentials(t *testing.T) {
	// Mirror server redirects HTTP→HTTP (no credentials on original request).
	var gotAuth string
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, releaseJSON("v0.2.0"))
	}))
	defer mirror.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, mirror.URL+"/releases/latest", http.StatusFound)
	}))
	defer origin.Close()

	old := httpClient
	httpClient = origin.Client()
	t.Cleanup(func() { httpClient = old })

	t.Setenv(EnvUpdateToken, "")
	t.Setenv(EnvGitHubToken, "")

	_, err := fetchRelease(context.Background(), origin.URL+"/releases/latest")
	if err != nil {
		t.Fatalf("HTTP mirror redirect should succeed, got: %v", err)
	}
	if gotAuth != "" {
		t.Fatalf("expected no auth on mirror, got %q", gotAuth)
	}
}

func TestHTTPMirrorRedirectAllowedWithDummyTokens(t *testing.T) {
	// Mirror redirect works even with tokens set — tokens are not sent to non-GitHub hosts.
	var gotAuth string
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, releaseJSON("v0.2.0"))
	}))
	defer mirror.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, mirror.URL+"/releases/latest", http.StatusFound)
	}))
	defer origin.Close()

	old := httpClient
	httpClient = origin.Client()
	t.Cleanup(func() { httpClient = old })

	t.Setenv(EnvUpdateToken, "dummy_token")
	t.Setenv(EnvGitHubToken, "dummy_fallback")

	_, err := fetchRelease(context.Background(), origin.URL+"/releases/latest")
	if err != nil {
		t.Fatalf("HTTP mirror redirect should succeed even with tokens, got: %v", err)
	}
	if gotAuth != "" {
		t.Fatalf("expected no auth for non-GitHub mirror, got %q", gotAuth)
	}
}

func TestHTTPSDowngradeBlockedWhenCredentialsPresent(t *testing.T) {
	calls := 0
	var firstAuth string
	tr := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			firstAuth = req.Header.Get("Authorization")
			return &http.Response{
				StatusCode: 301,
				Header:     http.Header{"Location": []string{"http://api.github.com/other"}},
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    req,
			}, nil
		}
		t.Fatalf("HTTP downgrade destination should not be contacted: %s %s calls=%d", req.Method, req.URL.String(), calls)
		return nil, fmt.Errorf("unexpected request")
	})

	old := httpClient
	httpClient = &http.Client{Transport: tr}
	t.Cleanup(func() { httpClient = old })

	t.Setenv(EnvUpdateToken, "secret")
	t.Setenv(EnvGitHubToken, "fallback_should_not_be_used")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := fetchRelease(ctx, "https://api.github.com/repos/Gitlawb/zero/releases/latest")
	if err == nil || !strings.Contains(err.Error(), "refusing redirect") {
		t.Fatalf("expected redirect refusal, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}
	if firstAuth != "Bearer secret" {
		t.Fatalf("expected Bearer secret on first request, got %q", firstAuth)
	}
}

func TestHTTPSSameHostRedirectAllowed(t *testing.T) {
	// HTTPS server redirects to same HTTPS host — should succeed.
	var gotPath string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.URL.Path == "/repos/Gitlawb/zero/releases" {
			http.Redirect(w, r, "/repos/Gitlawb/zero/releases/latest", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, releaseJSON("v0.2.0"))
	}))
	defer srv.Close()

	old := httpClient
	httpClient = srv.Client()
	t.Cleanup(func() { httpClient = old })

	t.Setenv(EnvUpdateToken, "token123")
	t.Setenv(EnvGitHubToken, "")

	_, err := fetchRelease(context.Background(), srv.URL+"/repos/Gitlawb/zero/releases")
	if err != nil {
		t.Fatalf("HTTPS→HTTPS same-host redirect should succeed, got: %v", err)
	}
	if gotPath != "/repos/Gitlawb/zero/releases/latest" {
		t.Fatalf("expected final path /repos/Gitlawb/zero/releases/latest, got %q", gotPath)
	}
}

func TestRedirectLimitEnforced(t *testing.T) {
	count := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		if count > 10 {
			http.Error(w, "too many", http.StatusLoopDetected)
			return
		}
		http.Redirect(w, r, "/loop", http.StatusFound)
	}))
	defer srv.Close()

	old := httpClient
	httpClient = srv.Client()
	t.Cleanup(func() { httpClient = old })

	t.Setenv(EnvUpdateToken, "")
	t.Setenv(EnvGitHubToken, "")

	_, err := fetchRelease(context.Background(), srv.URL+"/loop")
	if err == nil {
		t.Fatal("expected error for redirect loop")
	}
	if !strings.Contains(err.Error(), "10 redirects") {
		t.Fatalf("expected redirect limit error, got: %v", err)
	}
}
