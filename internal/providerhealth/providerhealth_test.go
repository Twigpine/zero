package providerhealth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/config"
)

func TestProbeConfigOnlyMissingProviderFails(t *testing.T) {
	result := Probe(context.Background(), Options{})

	if result.Status != StatusFail {
		t.Fatalf("Status = %q, want %q", result.Status, StatusFail)
	}
	check := result.Check("provider.config")
	if check == nil || check.Status != StatusFail {
		t.Fatalf("missing provider.config failure: %#v", result.Checks)
	}
}

func TestProbeConnectivityOpenAIModelsEndpointPasses(t *testing.T) {
	var gotPath string
	var gotAuth string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(strings.NewReader(`{"data":[{"id":"gpt-4.1"}]}`)),
			Header:     make(http.Header),
		}, nil
	})}

	result := Probe(context.Background(), Options{
		Profile: config.ProviderProfile{
			Name:         "openai-test",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://api.example.com/v1",
			APIKey:       "sk-test-secret",
			Model:        "gpt-4.1",
		},
		Connectivity: true,
		HTTPClient:   client,
		Resolver:     staticResolver{addr: netip.MustParseAddr("93.184.216.34")},
	})

	if result.Status != StatusPass {
		t.Fatalf("Status = %q, want pass: %#v", result.Status, result.Checks)
	}
	if gotPath != "/v1/models" {
		t.Fatalf("probe path = %q, want /v1/models", gotPath)
	}
	if gotAuth != "Bearer sk-test-secret" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	check := result.Check("provider.connectivity")
	if check == nil || check.Status != StatusPass {
		t.Fatalf("missing connectivity pass: %#v", result.Checks)
	}
}

func TestProbeConnectivityClassifiesAndRedactsAuthError(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Status:     "401 Unauthorized",
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"bad key sk-test-secret"}}`)),
			Header:     make(http.Header),
		}, nil
	})}

	result := Probe(context.Background(), Options{
		Profile: config.ProviderProfile{
			Name:         "openai-test",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://api.example.com/v1",
			APIKey:       "sk-test-secret",
			Model:        "custom-model",
		},
		Connectivity: true,
		HTTPClient:   client,
		Resolver:     staticResolver{addr: netip.MustParseAddr("93.184.216.34")},
	})

	if result.Status != StatusFail {
		t.Fatalf("Status = %q, want fail: %#v", result.Status, result.Checks)
	}
	check := result.Check("provider.connectivity")
	if check == nil {
		t.Fatalf("missing connectivity check: %#v", result.Checks)
	}
	if check.Category != CategoryAuth {
		t.Fatalf("Category = %q, want %q", check.Category, CategoryAuth)
	}
	if strings.Contains(check.Message, "sk-test-secret") {
		t.Fatalf("secret leaked in message: %q", check.Message)
	}
}

func TestProbeResolvedKindRequiresAuthWhenUnset(t *testing.T) {
	called := false
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("network should not be reached")
	})}

	result := Probe(context.Background(), Options{
		Profile: config.ProviderProfile{
			Name:  "openai-test",
			Model: "gpt-4.1",
		},
		Connectivity: true,
		HTTPClient:   client,
	})

	if result.Status != StatusFail {
		t.Fatalf("Status = %q, want fail: %#v", result.Status, result.Checks)
	}
	check := result.Check("provider.auth")
	if check == nil || check.Status != StatusFail || check.Category != CategoryAuth {
		t.Fatalf("provider.auth = %#v, want auth failure", check)
	}
	if called {
		t.Fatal("HTTP client was called after missing credentials")
	}
}

func TestProbeResolvedKindConnectivityAuthErrorRedactsSecret(t *testing.T) {
	authHeader := make(chan string, 1)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		select {
		case authHeader <- r.Header.Get("Authorization"):
		default:
		}
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Status:     "401 Unauthorized",
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"bad key sk-test-secret"}}`)),
			Header:     make(http.Header),
		}, nil
	})}

	result := Probe(context.Background(), Options{
		Profile: config.ProviderProfile{
			Name:    "openai-test",
			BaseURL: "https://api.example.com/v1",
			APIKey:  "sk-test-secret",
			Model:   "custom-model",
		},
		Connectivity: true,
		HTTPClient:   client,
		Resolver:     staticResolver{addr: netip.MustParseAddr("93.184.216.34")},
	})

	if result.Status != StatusFail {
		t.Fatalf("Status = %q, want fail: %#v", result.Status, result.Checks)
	}
	select {
	case got := <-authHeader:
		if got != "Bearer sk-test-secret" {
			t.Fatalf("Authorization = %q", got)
		}
	default:
		t.Fatal("expected connectivity probe request")
	}
	check := result.Check("provider.connectivity")
	if check == nil || check.Category != CategoryAuth {
		t.Fatalf("provider.connectivity = %#v, want auth failure", check)
	}
	if strings.Contains(check.Message, "sk-test-secret") {
		t.Fatalf("secret leaked in message: %q", check.Message)
	}
}

func TestProbeConnectivityUsesOAuthCredential(t *testing.T) {
	var gotAuth string
	resolverCalled := false
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotAuth = r.Header.Get("Authorization")
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(strings.NewReader(`{"data":[]}`)),
			Header:     make(http.Header),
		}, nil
	})}

	result := Probe(context.Background(), Options{
		Profile: config.ProviderProfile{
			Name:         "xai",
			CatalogID:    "xai",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://api.example.com/v1",
			Model:        "grok-4",
		},
		Connectivity: true,
		HTTPClient:   client,
		Resolver:     staticResolver{addr: netip.MustParseAddr("93.184.216.34")},
		OAuthResolver: func(context.Context, bool) (string, string, bool, error) {
			resolverCalled = true
			return "Authorization", "Bearer oauth-test-token", true, nil
		},
	})

	if result.Status != StatusPass {
		t.Fatalf("Status = %q, want pass: %#v", result.Status, result.Checks)
	}
	if !resolverCalled {
		t.Fatal("OAuth resolver was not called")
	}
	if gotAuth != "Bearer oauth-test-token" {
		t.Fatalf("Authorization = %q, want OAuth bearer", gotAuth)
	}
}

func TestProbeConnectivityOAuthCredentialsAreRedacted(t *testing.T) {
	const (
		staticKey  = "STATICKEYVALUE1234567890abcdef"
		oauthToken = "hf_VASANTHSENTINEL0011223344"
	)
	var authHeaders []string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Status:     "401 Unauthorized",
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"invalid credentials ` + staticKey + ` and ` + oauthToken + `"}}`)),
			Header:     make(http.Header),
		}, nil
	})}

	result := Probe(context.Background(), Options{
		Profile: config.ProviderProfile{
			Name:         "xai",
			CatalogID:    "xai",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://api.example.com/v1",
			APIKey:       staticKey,
			Model:        "grok-4",
		},
		Connectivity: true,
		HTTPClient:   client,
		Resolver:     staticResolver{addr: netip.MustParseAddr("93.184.216.34")},
		OAuthResolver: func(context.Context, bool) (string, string, bool, error) {
			return "Authorization", "Bearer " + oauthToken, true, nil
		},
	})

	if len(authHeaders) != 2 {
		t.Fatalf("Authorization headers = %#v, want initial request and one refresh retry", authHeaders)
	}
	for _, header := range authHeaders {
		if header != "Bearer "+oauthToken {
			t.Fatalf("Authorization = %q, want OAuth bearer", header)
		}
	}
	check := result.Check("provider.connectivity")
	if check == nil || check.Category != CategoryAuth {
		t.Fatalf("provider.connectivity = %#v, want auth failure", check)
	}
	if strings.Contains(check.Message, staticKey) {
		t.Fatalf("static API key leaked in message: %q", check.Message)
	}
	if strings.Contains(check.Message, oauthToken) {
		t.Fatalf("OAuth token leaked in message: %q", check.Message)
	}
	if strings.Count(check.Message, "[REDACTED]") != 2 {
		t.Fatalf("expected both credentials to be redacted, got %q", check.Message)
	}
}

func TestProbeWithoutConnectivityRejectsUnavailableOAuthCredential(t *testing.T) {
	resolverCalled := false
	result := Probe(context.Background(), Options{
		Profile: config.ProviderProfile{
			Name:         "xai",
			CatalogID:    "xai",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://api.example.com/v1",
			Model:        "grok-4",
		},
		OAuthResolver: func(context.Context, bool) (string, string, bool, error) {
			resolverCalled = true
			return "", "", false, nil
		},
	})

	if !resolverCalled {
		t.Fatal("OAuth resolver was not called")
	}
	if result.Status != StatusFail {
		t.Fatalf("Status = %q, want fail: %#v", result.Status, result.Checks)
	}
	check := result.Check("provider.auth")
	if check == nil || check.Status != StatusFail || check.Category != CategoryAuth {
		t.Fatalf("provider.auth = %#v, want auth failure", check)
	}
}

func TestProbeConnectivityRefreshesOAuthCredentialAfterUnauthorized(t *testing.T) {
	var authHeaders []string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		status := http.StatusOK
		body := `{"data":[]}`
		if len(authHeaders) == 1 {
			status = http.StatusUnauthorized
			body = `{"error":{"message":"expired"}}`
		}
		return &http.Response{
			StatusCode: status,
			Status:     http.StatusText(status),
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})}

	var refreshFlags []bool
	result := Probe(context.Background(), Options{
		Profile: config.ProviderProfile{
			Name:         "xai",
			CatalogID:    "xai",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://api.example.com/v1",
			Model:        "grok-4",
		},
		Connectivity: true,
		HTTPClient:   client,
		Resolver:     staticResolver{addr: netip.MustParseAddr("93.184.216.34")},
		OAuthResolver: func(_ context.Context, forceRefresh bool) (string, string, bool, error) {
			refreshFlags = append(refreshFlags, forceRefresh)
			token := "stale-token"
			if forceRefresh {
				token = "fresh-token"
			}
			return "Authorization", "Bearer " + token, true, nil
		},
	})

	if result.Status != StatusPass {
		t.Fatalf("Status = %q, want pass after refresh: %#v", result.Status, result.Checks)
	}
	if len(refreshFlags) != 2 || refreshFlags[0] || !refreshFlags[1] {
		t.Fatalf("OAuth refresh flags = %#v, want [false true]", refreshFlags)
	}
	if len(authHeaders) != 2 || authHeaders[0] != "Bearer stale-token" || authHeaders[1] != "Bearer fresh-token" {
		t.Fatalf("Authorization headers = %#v, want stale then fresh bearer", authHeaders)
	}
}

func TestProbeConnectivityClassifiesTimeout(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})}

	result := Probe(context.Background(), Options{
		Profile: config.ProviderProfile{
			Name:         "openai-test",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://example.invalid/v1",
			APIKey:       "sk-test-secret",
			Model:        "custom-model",
		},
		Connectivity: true,
		HTTPClient:   client,
		Resolver:     staticResolver{addr: netip.MustParseAddr("93.184.216.34")},
		Timeout:      time.Millisecond,
	})

	check := result.Check("provider.connectivity")
	if check == nil || check.Category != CategoryTimeout {
		t.Fatalf("connectivity check = %#v, want timeout category", check)
	}
}

func TestProbeConnectivityAllowsLocalhostForLocalProvider(t *testing.T) {
	// AUDIT-H1: a user-configured local provider (loopback base_url) must be reachable
	// — the probe no longer blocks it pre-network, so `zero setup <local> --verify` /
	// doctor / providers check can confirm a running Ollama/LM Studio.
	called := false
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
	})}

	result := Probe(context.Background(), Options{
		Profile: config.ProviderProfile{
			Name:         "local",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "http://localhost:11434/v1",
			APIKey:       "sk-test-secret",
			Model:        "local-model",
		},
		Connectivity: true,
		HTTPClient:   client,
	})

	if !called {
		t.Fatal("HTTP client should be reached for a user-configured localhost provider (loopback no longer pre-blocked)")
	}
	check := result.Check("provider.connectivity")
	if check == nil || check.Status != StatusPass {
		t.Fatalf("connectivity check = %#v, want pass for a reachable local provider", check)
	}
}

func TestProbeConnectivityBlocksPrivateResolvedHostBeforeNetwork(t *testing.T) {
	called := false
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("network should not be reached")
	})}

	result := Probe(context.Background(), Options{
		Profile: config.ProviderProfile{
			Name:         "private",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://private.example/v1",
			APIKey:       "sk-test-secret",
			Model:        "custom-model",
		},
		Connectivity: true,
		HTTPClient:   client,
		Resolver:     staticResolver{addr: netip.MustParseAddr("10.0.0.5")},
	})

	check := result.Check("provider.connectivity")
	if check == nil || check.Status != StatusFail || check.Category != CategoryNetwork {
		t.Fatalf("connectivity check = %#v, want blocked private-network failure", check)
	}
	if called {
		t.Fatal("HTTP client was called for a blocked private resolved host")
	}
}

func TestProbeConnectivityServiceUnavailableFails(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Status:     "503 Service Unavailable",
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"maintenance"}}`)),
			Header:     make(http.Header),
		}, nil
	})}

	result := Probe(context.Background(), Options{
		Profile: config.ProviderProfile{
			Name:         "custom",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://api.example.com/v1",
			APIKey:       "sk-test-secret",
			Model:        "custom-model",
		},
		Connectivity: true,
		HTTPClient:   client,
		Resolver:     staticResolver{addr: netip.MustParseAddr("93.184.216.34")},
	})

	check := result.Check("provider.connectivity")
	if result.Status != StatusFail || check == nil || check.Category != CategoryProvider {
		t.Fatalf("result = %#v, connectivity = %#v, want provider failure", result, check)
	}
}

func TestProbeConnectivityContextCanceledIsNetworkFailure(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, context.Canceled
	})}

	result := Probe(context.Background(), Options{
		Profile: config.ProviderProfile{
			Name:         "custom",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://api.example.com/v1",
			APIKey:       "sk-test-secret",
			Model:        "custom-model",
		},
		Connectivity: true,
		HTTPClient:   client,
		Resolver:     staticResolver{addr: netip.MustParseAddr("93.184.216.34")},
	})

	check := result.Check("provider.connectivity")
	if check == nil || check.Category != CategoryNetwork {
		t.Fatalf("connectivity check = %#v, want network category for context.Canceled", check)
	}
}

func TestPrimaryCheckPrefersFailuresOverPassingConnectivity(t *testing.T) {
	result := Result{Checks: []Check{
		{ID: "provider.connectivity", Status: StatusPass, Message: "reachable"},
		{ID: "provider.auth", Status: StatusFail, Message: "missing auth"},
	}}

	if got := result.PrimaryCheck(); got == nil || got.ID != "provider.auth" {
		t.Fatalf("PrimaryCheck = %#v, want provider.auth failure", got)
	}
}

func TestProbeConnectivityUnsupportedTransportWarnsWithoutNetwork(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network should not be reached")
	})}

	result := Probe(context.Background(), Options{
		Profile: config.ProviderProfile{
			Name:      "bedrock",
			CatalogID: "bedrock",
			Model:     "anthropic.claude-3-5-sonnet-20241022-v2:0",
		},
		Connectivity: true,
		HTTPClient:   client,
	})

	check := result.Check("provider.runtime")
	if check == nil || check.Category != CategoryUnsupported {
		t.Fatalf("runtime check = %#v, want unsupported category", check)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

type staticResolver struct {
	addr netip.Addr
	err  error
}

func (resolver staticResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	if resolver.err != nil {
		return nil, resolver.err
	}
	return []netip.Addr{resolver.addr}, nil
}
