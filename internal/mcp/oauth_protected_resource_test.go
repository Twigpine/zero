package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseResourceMetadataURL(t *testing.T) {
	got, found, err := parseResourceMetadataURL([]string{
		`Basic realm="resource_metadata=not-a-parameter"`,
		`Bearer realm="OAuth", scope="read", Resource_Metadata="https://mcp.example/resource-metadata"`,
	})
	if err != nil || !found {
		t.Fatalf("parseResourceMetadataURL() found=%v err=%v", found, err)
	}
	if got != "https://mcp.example/resource-metadata" {
		t.Fatalf("resource metadata URL = %q", got)
	}

	got, found, err = parseResourceMetadataURL([]string{
		`Basic realm="unterminated`,
		`Bearer resource_metadata="https://mcp.example/fallback-metadata"`,
	})
	if err != nil || !found || got != "https://mcp.example/fallback-metadata" {
		t.Fatalf("unrelated malformed challenge blocked discovery: got=%q found=%v err=%v", got, found, err)
	}

	for name, headers := range map[string][]string{
		"unquoted":     {`Bearer resource_metadata=https://mcp.example/metadata`},
		"unterminated": {`Bearer resource_metadata="https://mcp.example/metadata`},
		"conflicting": {
			`Bearer resource_metadata="https://mcp.example/one"`,
			`DPoP resource_metadata="https://mcp.example/two"`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := parseResourceMetadataURL(headers); err == nil {
				t.Fatal("expected malformed resource_metadata rejection")
			}
		})
	}
}

func TestProtectedResourceDiscoveryRejectsInvalidMetadata(t *testing.T) {
	for _, test := range []struct {
		name      string
		challenge func(base string) string
		metadata  func(base string) string
		wantError string
	}{
		{
			name:      "malformed challenge",
			challenge: func(string) string { return `Bearer resource_metadata="unterminated` },
			wantError: "WWW-Authenticate",
		},
		{
			name:      "insecure metadata URL",
			challenge: func(string) string { return `Bearer resource_metadata="http://metadata.example/resource"` },
			wantError: "metadata URL",
		},
		{
			name:      "invalid JSON",
			challenge: func(base string) string { return `Bearer resource_metadata="` + base + `/metadata"` },
			metadata:  func(string) string { return `{` },
			wantError: "decode protected resource metadata",
		},
		{
			name:      "resource mismatch",
			challenge: func(base string) string { return `Bearer resource_metadata="` + base + `/metadata"` },
			metadata: func(base string) string {
				return `{"resource":"` + base + `/other","authorization_servers":["https://auth.example"]}`
			},
			wantError: "does not match",
		},
		{
			name:      "resource leading whitespace mismatch",
			challenge: func(base string) string { return `Bearer resource_metadata="` + base + `/metadata"` },
			metadata: func(base string) string {
				return `{"resource":" ` + base + `/mcp","authorization_servers":["https://auth.example"]}`
			},
			wantError: "does not match",
		},
		{
			name:      "resource trailing whitespace mismatch",
			challenge: func(base string) string { return `Bearer resource_metadata="` + base + `/metadata"` },
			metadata: func(base string) string {
				return `{"resource":"` + base + `/mcp ","authorization_servers":["https://auth.example"]}`
			},
			wantError: "does not match",
		},
		{
			name:      "authorization servers missing",
			challenge: func(base string) string { return `Bearer resource_metadata="` + base + `/metadata"` },
			metadata: func(base string) string {
				return `{"resource":"` + base + `/mcp"}`
			},
			wantError: "authorization_servers",
		},
		{
			name:      "insecure authorization server",
			challenge: func(base string) string { return `Bearer resource_metadata="` + base + `/metadata"` },
			metadata: func(base string) string {
				return `{"resource":"` + base + `/mcp","authorization_servers":["http://auth.example"]}`
			},
			wantError: "authorization server",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				base := "http://" + r.Host
				switch r.URL.Path {
				case "/mcp":
					w.Header().Set("WWW-Authenticate", test.challenge(base))
					w.WriteHeader(http.StatusUnauthorized)
				case "/metadata":
					if test.metadata == nil {
						http.NotFound(w, r)
						return
					}
					_, _ = w.Write([]byte(test.metadata(base)))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			_, _, err := discoverProtectedResourceAuthorizationServer(context.Background(), server.Client(), server.URL+"/mcp")
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %v, want %q", err, test.wantError)
			}
		})
	}
}

func TestProtectedResourceProbeSendsNoCredentials(t *testing.T) {
	var authorization, cookie string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		cookie = r.Header.Get("Cookie")
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("create cookie jar: %v", err)
	}
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	jar.SetCookies(serverURL, []*http.Cookie{{Name: "session", Value: "secret"}})
	client := server.Client()
	client.Jar = jar

	_, found, err := discoverProtectedResourceAuthorizationServer(context.Background(), client, server.URL+"/mcp")
	if err != nil || found {
		t.Fatalf("discovery found=%v err=%v", found, err)
	}
	if authorization != "" || cookie != "" {
		t.Fatalf("probe sent credentials: authorization=%q cookie=%q", authorization, cookie)
	}
}

func TestDirectAuthorizationServerDiscoverySendsNoCookies(t *testing.T) {
	var cookie string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie = r.Header.Get("Cookie")
		base := "http://" + r.Host
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 base,
			"authorization_endpoint": base + "/authorize",
			"token_endpoint":         base + "/token",
		})
	}))
	defer server.Close()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("create cookie jar: %v", err)
	}
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	jar.SetCookies(serverURL, []*http.Cookie{{Name: "session", Value: "secret"}})
	client := server.Client()
	client.Jar = jar

	if _, err := resolveAuthorizationServer(context.Background(), client, server.URL, OAuthConfig{}); err != nil {
		t.Fatalf("resolveAuthorizationServer() error = %v", err)
	}
	if cookie != "" {
		t.Fatalf("direct discovery sent cookie %q", cookie)
	}
}

func TestProtectedResourceDiscoveryRejectsAdvertisedLoopbackMetadata(t *testing.T) {
	var metadataHits atomic.Int64
	client := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		response := &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    request,
		}
		switch request.URL.String() {
		case "https://mcp.example/mcp":
			response.StatusCode = http.StatusUnauthorized
			response.Header.Set("WWW-Authenticate", `Bearer resource_metadata="http://127.0.0.1/metadata"`)
		case "http://127.0.0.1/metadata":
			metadataHits.Add(1)
			response.Body = io.NopCloser(strings.NewReader(`{"resource":"https://mcp.example/mcp","authorization_servers":["https://auth.example"]}`))
		default:
			response.StatusCode = http.StatusNotFound
		}
		return response, nil
	})}

	_, _, err := discoverProtectedResourceAuthorizationServer(context.Background(), client, "https://mcp.example/mcp")
	if err == nil {
		t.Fatal("advertised loopback metadata URL should be rejected")
	}
	if metadataHits.Load() != 0 {
		t.Fatalf("loopback metadata target was fetched %d time(s)", metadataHits.Load())
	}
}

func TestProtectedResourceDiscoveryRejectsAdvertisedLoopbackIssuer(t *testing.T) {
	var issuerHits atomic.Int64
	client := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		response := &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    request,
		}
		switch request.URL.String() {
		case "https://mcp.example/mcp":
			response.StatusCode = http.StatusUnauthorized
			response.Header.Set("WWW-Authenticate", `Bearer resource_metadata="https://mcp.example/metadata"`)
		case "https://mcp.example/metadata":
			response.Body = io.NopCloser(strings.NewReader(`{"resource":"https://mcp.example/mcp","authorization_servers":["http://127.0.0.1"]}`))
		case "http://127.0.0.1/.well-known/oauth-authorization-server":
			issuerHits.Add(1)
			response.Body = io.NopCloser(strings.NewReader(`{"issuer":"http://127.0.0.1","authorization_endpoint":"http://127.0.0.1/authorize","token_endpoint":"http://127.0.0.1/token"}`))
		default:
			response.StatusCode = http.StatusNotFound
		}
		return response, nil
	})}

	_, err := resolveAuthorizationServer(context.Background(), client, "https://mcp.example/mcp", OAuthConfig{})
	if err == nil {
		t.Fatal("advertised loopback issuer should be rejected")
	}
	if issuerHits.Load() != 0 {
		t.Fatalf("loopback issuer target was fetched %d time(s)", issuerHits.Load())
	}
}

func TestProtectedResourceDiscoveryRejectsAdvertisedPrivateTargets(t *testing.T) {
	for _, target := range []string{
		"http://10.0.0.8/metadata",
		"https://169.254.169.254/metadata",
		"https://metadata.google.internal/metadata",
	} {
		t.Run(target, func(t *testing.T) {
			if err := validateAdvertisedOAuthDiscoveryURL("https://mcp.example/mcp", target); err == nil {
				t.Fatalf("advertised target %q should be rejected", target)
			}
		})
	}
}

func TestProtectedDiscoveryPathsSendNoCookies(t *testing.T) {
	var probeCookie, resourceCookie, issuerCookie string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		switch r.URL.Path {
		case "/mcp":
			probeCookie = r.Header.Get("Cookie")
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/metadata"`)
			w.WriteHeader(http.StatusUnauthorized)
		case "/metadata":
			resourceCookie = r.Header.Get("Cookie")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"resource":              base + "/mcp",
				"authorization_servers": []string{base},
			})
		case "/.well-known/oauth-authorization-server":
			issuerCookie = r.Header.Get("Cookie")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                 base,
				"authorization_endpoint": base + "/authorize",
				"token_endpoint":         base + "/token",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("create cookie jar: %v", err)
	}
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	jar.SetCookies(serverURL, []*http.Cookie{{Name: "session", Value: "secret"}})
	client := server.Client()
	client.Jar = jar

	if _, err := resolveAuthorizationServer(context.Background(), client, server.URL+"/mcp", OAuthConfig{}); err != nil {
		t.Fatalf("resolveAuthorizationServer() error = %v", err)
	}
	if probeCookie != "" || resourceCookie != "" || issuerCookie != "" {
		t.Fatalf("discovery sent cookies: probe=%q resource=%q issuer=%q", probeCookie, resourceCookie, issuerCookie)
	}
}

func TestProtectedDiscoveryPreservesExplicitEndpointOverride(t *testing.T) {
	fixture := newPublicProtectedOAuthFixture(t, "", "https://93.184.216.35/token", "", "")
	explicitToken := "http://127.0.0.1:19432/token"
	metadata, err := resolveAuthorizationServer(context.Background(), fixture.client, fixture.resourceURL, OAuthConfig{TokenEndpoint: explicitToken})
	if err != nil {
		t.Fatalf("resolve with explicit endpoint override: %v", err)
	}
	if metadata.TokenEndpoint != explicitToken || metadata.protectTokenEndpoint {
		t.Fatalf("explicit token override lost provenance: endpoint=%q protected=%v", metadata.TokenEndpoint, metadata.protectTokenEndpoint)
	}
}

func TestProtectedDiscoveryRejectsLoopbackRegistrationEndpointBeforeUse(t *testing.T) {
	var registrationHits atomic.Int64
	registration := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		registrationHits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"client_id": "registered-client"})
	}))
	defer registration.Close()
	fixture := newPublicProtectedOAuthFixture(t, registration.URL+"/register", "https://93.184.216.35/token", "", "")

	_, err := Login(context.Background(), LoginOptions{
		ServerName: "registration-ssrf",
		ServerURL:  fixture.resourceURL,
		HTTPClient: fixture.client,
		OpenBrowser: func(string) error {
			return errors.New("authorization browser should not open")
		},
		Timeout: 5 * time.Second,
	})
	if err == nil {
		t.Fatal("protected discovery should reject a loopback registration endpoint")
	}
	if registrationHits.Load() != 0 {
		t.Fatalf("loopback registration endpoint received %d request(s)", registrationHits.Load())
	}
}

func TestProtectedDiscoveryRejectsLoopbackTokenEndpointBeforeUse(t *testing.T) {
	var tokenHits atomic.Int64
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tokenHits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "unexpected",
			"token_type":   "Bearer",
		})
	}))
	defer tokenServer.Close()
	fixture := newPublicProtectedOAuthFixture(t, "", tokenServer.URL+"/token", "", "")

	_, err := Login(context.Background(), LoginOptions{
		ServerName: "token-ssrf",
		ServerURL:  fixture.resourceURL,
		Config:     OAuthConfig{ClientID: "configured-client"},
		HTTPClient: fixture.client,
		OpenBrowser: func(authURL string) error {
			parsed, parseErr := url.Parse(authURL)
			if parseErr != nil {
				return parseErr
			}
			callbackURL := parsed.Query().Get("redirect_uri") + "?code=code&state=" + url.QueryEscape(parsed.Query().Get("state"))
			go func() {
				for attempt := 0; attempt < 20; attempt++ {
					response, requestErr := http.Get(callbackURL)
					if requestErr == nil {
						response.Body.Close()
						return
					}
					time.Sleep(25 * time.Millisecond)
				}
			}()
			return nil
		},
		Timeout: 5 * time.Second,
	})
	if err == nil {
		t.Fatal("protected discovery should reject a loopback token endpoint")
	}
	if tokenHits.Load() != 0 {
		t.Fatalf("loopback token endpoint received %d request(s)", tokenHits.Load())
	}
}

func TestProtectedDiscoveryRegistrationRefusesPublicToPrivateRedirect(t *testing.T) {
	var victimHits atomic.Int64
	victim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		victimHits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"client_id": "redirected-client"})
	}))
	defer victim.Close()
	fixture := newPublicProtectedOAuthFixture(t, "", "https://93.184.216.35/token", victim.URL+"/register", "")

	_, err := Login(context.Background(), LoginOptions{
		ServerName: "registration-redirect",
		ServerURL:  fixture.resourceURL,
		HTTPClient: fixture.client,
		OpenBrowser: func(string) error {
			return errors.New("authorization browser should not open")
		},
		Timeout: 5 * time.Second,
	})
	if err == nil {
		t.Fatal("protected registration redirect should be refused")
	}
	if victimHits.Load() != 0 {
		t.Fatalf("registration redirect reached private victim %d time(s)", victimHits.Load())
	}
}

func TestProtectedDiscoveryTokenRefusesPublicToPrivateRedirect(t *testing.T) {
	var victimHits atomic.Int64
	victim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		victimHits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "redirected-token", "token_type": "Bearer"})
	}))
	defer victim.Close()
	fixture := newPublicProtectedOAuthFixture(t, "", "", "", victim.URL+"/token")

	_, err := Login(context.Background(), LoginOptions{
		ServerName: "token-redirect",
		ServerURL:  fixture.resourceURL,
		Config:     OAuthConfig{ClientID: "configured-client"},
		HTTPClient: fixture.client,
		OpenBrowser: func(authURL string) error {
			parsed, parseErr := url.Parse(authURL)
			if parseErr != nil {
				return parseErr
			}
			callbackURL := parsed.Query().Get("redirect_uri") + "?code=code&state=" + url.QueryEscape(parsed.Query().Get("state"))
			go func() {
				for attempt := 0; attempt < 20; attempt++ {
					response, requestErr := http.Get(callbackURL)
					if requestErr == nil {
						response.Body.Close()
						return
					}
					time.Sleep(25 * time.Millisecond)
				}
			}()
			return nil
		},
		Timeout: 5 * time.Second,
	})
	if err == nil {
		t.Fatal("protected token redirect should be refused")
	}
	if victimHits.Load() != 0 {
		t.Fatalf("token redirect reached private victim %d time(s)", victimHits.Load())
	}
}

type publicProtectedOAuthFixture struct {
	client      *http.Client
	resourceURL string
}

func newPublicProtectedOAuthFixture(t *testing.T, registrationEndpoint string, tokenEndpoint string, registrationRedirect string, tokenRedirect string) publicProtectedOAuthFixture {
	t.Helper()
	var resourceOrigin, issuerOrigin string
	gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resourceHost := strings.TrimPrefix(resourceOrigin, "https://")
		issuerHost := strings.TrimPrefix(issuerOrigin, "https://")
		switch {
		case r.Host == resourceHost && r.URL.Path == "/mcp":
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+resourceOrigin+`/metadata"`)
			w.WriteHeader(http.StatusUnauthorized)
		case r.Host == resourceHost && r.URL.Path == "/metadata":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"resource":              resourceOrigin + "/mcp",
				"authorization_servers": []string{issuerOrigin},
			})
		case r.Host == issuerHost && r.URL.Path == "/.well-known/oauth-authorization-server":
			metadataRegistration := registrationEndpoint
			metadataToken := tokenEndpoint
			if registrationRedirect != "" {
				metadataRegistration = issuerOrigin + "/register"
			}
			if tokenRedirect != "" {
				metadataToken = issuerOrigin + "/token"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                 issuerOrigin,
				"authorization_endpoint": issuerOrigin + "/authorize",
				"token_endpoint":         metadataToken,
				"registration_endpoint":  metadataRegistration,
			})
		case r.Host == issuerHost && r.URL.Path == "/register" && registrationRedirect != "":
			http.Redirect(w, r, registrationRedirect, http.StatusTemporaryRedirect)
		case r.Host == issuerHost && r.URL.Path == "/token" && tokenRedirect != "":
			http.Redirect(w, r, tokenRedirect, http.StatusTemporaryRedirect)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(gateway.Close)
	port := gateway.Listener.Addr().(*net.TCPAddr).Port
	resourceOrigin = fmt.Sprintf("https://93.184.216.34:%d", port)
	issuerOrigin = fmt.Sprintf("https://93.184.216.35:%d", port)

	transport := gateway.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.InsecureSkipVerify = true //nolint:gosec // hermetic public-address routing fixture
	dialer := &net.Dialer{}
	gatewayAddress := gateway.Listener.Addr().String()
	transport.DialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		if err == nil && (host == "93.184.216.34" || host == "93.184.216.35") {
			return dialer.DialContext(ctx, network, gatewayAddress)
		}
		return dialer.DialContext(ctx, network, address)
	}
	return publicProtectedOAuthFixture{
		client:      &http.Client{Transport: transport},
		resourceURL: resourceOrigin + "/mcp",
	}
}

func TestProtectedResourceProbeDoesNotFollowRedirects(t *testing.T) {
	var redirectedHits atomic.Int64
	redirected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectedHits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer redirected.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirected.URL+"/mcp", http.StatusFound)
	}))
	defer source.Close()

	_, found, err := discoverProtectedResourceAuthorizationServer(context.Background(), source.Client(), source.URL+"/mcp")
	if err != nil || found {
		t.Fatalf("redirected probe found=%v err=%v", found, err)
	}
	if redirectedHits.Load() != 0 {
		t.Fatalf("probe followed redirect %d time(s)", redirectedHits.Load())
	}
}

func TestProtectedResourceMetadataDoesNotFollowRedirects(t *testing.T) {
	var redirectedHits atomic.Int64
	redirected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectedHits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resource":              "https://attacker.example/mcp",
			"authorization_servers": []string{"https://attacker.example"},
		})
	}))
	defer redirected.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		switch r.URL.Path {
		case "/mcp":
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/metadata"`)
			w.WriteHeader(http.StatusUnauthorized)
		case "/metadata":
			http.Redirect(w, r, redirected.URL+"/metadata", http.StatusFound)
		}
	}))
	defer source.Close()

	_, _, err := discoverProtectedResourceAuthorizationServer(context.Background(), source.Client(), source.URL+"/mcp")
	if err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("error = %v, want redirect refusal", err)
	}
	if redirectedHits.Load() != 0 {
		t.Fatalf("metadata fetch followed redirect %d time(s)", redirectedHits.Load())
	}
}

func TestResolveAuthorizationServerRejectsProtectedIssuerMismatch(t *testing.T) {
	authorizationServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 "https://unexpected.example",
			"authorization_endpoint": base + "/authorize",
			"token_endpoint":         base + "/token",
		})
	}))
	defer authorizationServer.Close()
	resourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		switch r.URL.Path {
		case "/mcp":
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/metadata"`)
			w.WriteHeader(http.StatusUnauthorized)
		case "/metadata":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"resource":              base + "/mcp",
				"authorization_servers": []string{authorizationServer.URL},
			})
		}
	}))
	defer resourceServer.Close()

	_, err := resolveAuthorizationServer(context.Background(), resourceServer.Client(), resourceServer.URL+"/mcp", OAuthConfig{})
	if err == nil || !strings.Contains(err.Error(), "issuer does not match") {
		t.Fatalf("error = %v, want issuer mismatch", err)
	}
}

func TestResolveAuthorizationServerRejectsMissingProtectedIssuer(t *testing.T) {
	authorizationServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		_ = json.NewEncoder(w).Encode(map[string]any{
			"authorization_endpoint": base + "/authorize",
			"token_endpoint":         base + "/token",
		})
	}))
	defer authorizationServer.Close()
	resourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		switch r.URL.Path {
		case "/mcp":
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/metadata"`)
			w.WriteHeader(http.StatusUnauthorized)
		case "/metadata":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"resource":              base + "/mcp",
				"authorization_servers": []string{authorizationServer.URL},
			})
		}
	}))
	defer resourceServer.Close()

	_, err := resolveAuthorizationServer(context.Background(), resourceServer.Client(), resourceServer.URL+"/mcp", OAuthConfig{})
	if err == nil || !strings.Contains(err.Error(), "issuer") {
		t.Fatalf("error = %v, want missing issuer rejection", err)
	}
}

func TestResolveAuthorizationServerRejectsWhitespaceAlteredProtectedIssuer(t *testing.T) {
	for _, alter := range []struct {
		name  string
		apply func(string) string
	}{
		{name: "leading", apply: func(value string) string { return " " + value }},
		{name: "trailing", apply: func(value string) string { return value + " " }},
	} {
		t.Run(alter.name, func(t *testing.T) {
			authorizationServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				base := "http://" + r.Host
				_ = json.NewEncoder(w).Encode(map[string]any{
					"issuer":                 alter.apply(base),
					"authorization_endpoint": base + "/authorize",
					"token_endpoint":         base + "/token",
				})
			}))
			defer authorizationServer.Close()
			resourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				base := "http://" + r.Host
				switch r.URL.Path {
				case "/mcp":
					w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/metadata"`)
					w.WriteHeader(http.StatusUnauthorized)
				case "/metadata":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"resource":              base + "/mcp",
						"authorization_servers": []string{authorizationServer.URL},
					})
				}
			}))
			defer resourceServer.Close()

			_, err := resolveAuthorizationServer(context.Background(), resourceServer.Client(), resourceServer.URL+"/mcp", OAuthConfig{})
			if err == nil || !strings.Contains(err.Error(), "issuer does not match") {
				t.Fatalf("error = %v, want whitespace issuer mismatch", err)
			}
		})
	}
}

func TestAuthorizationServerMetadataDoesNotFollowRedirects(t *testing.T) {
	var redirectedHits atomic.Int64
	redirected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedHits.Add(1)
		base := "http://" + r.Host
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 base,
			"authorization_endpoint": base + "/authorize",
			"token_endpoint":         base + "/token",
		})
	}))
	defer redirected.Close()
	authorizationServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirected.URL+"/.well-known/oauth-authorization-server", http.StatusFound)
	}))
	defer authorizationServer.Close()
	resourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		switch r.URL.Path {
		case "/mcp":
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/metadata"`)
			w.WriteHeader(http.StatusUnauthorized)
		case "/metadata":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"resource":              base + "/mcp",
				"authorization_servers": []string{authorizationServer.URL},
			})
		}
	}))
	defer resourceServer.Close()

	_, err := resolveAuthorizationServer(context.Background(), resourceServer.Client(), resourceServer.URL+"/mcp", OAuthConfig{})
	if err == nil {
		t.Fatal("authorization metadata redirect should fail")
	}
	if redirectedHits.Load() != 0 {
		t.Fatalf("authorization metadata fetch followed redirect %d time(s)", redirectedHits.Load())
	}
}
