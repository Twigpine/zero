package providerhealth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
)

func TestProbeConfigOnlyValidProviderPassesWithoutNetwork(t *testing.T) {
	called := false
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("network should not be reached")
	})}

	result := Probe(context.Background(), Options{
		Profile: config.ProviderProfile{
			Name:         "local",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "http://localhost:11434/v1",
			Model:        "local-model",
		},
		HTTPClient: client,
	})

	if result.Status != StatusPass {
		t.Fatalf("Status = %q, want %q: %#v", result.Status, StatusPass, result.Checks)
	}
	if called {
		t.Fatal("HTTP client was called during config-only probe")
	}
}

func TestProbeConfigValidationClassifiesMissingModel(t *testing.T) {
	result := Probe(context.Background(), Options{
		Profile: config.ProviderProfile{
			Name:         "custom",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://example.invalid/v1",
		},
	})

	check := result.Check("provider.config")
	if check == nil || check.Category != CategoryConfig || check.Status != StatusFail {
		t.Fatalf("provider.config = %#v, want config failure", check)
	}
}

func TestProbeRedactsTopLevelAndDetailURLs(t *testing.T) {
	result := Probe(context.Background(), Options{
		Profile: config.ProviderProfile{
			Name:         "custom",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://user:base-secret@example.invalid/v1?api_key=query-secret",
			Model:        "custom-model",
		},
	})

	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal health result: %v", err)
	}
	rendered := string(raw)
	for _, secret := range []string{"base-secret", "query-secret"} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("secret %q leaked in health result: %s", secret, rendered)
		}
	}
	if strings.Contains(rendered, "user:") {
		t.Fatalf("URL userinfo leaked in health result: %s", rendered)
	}
}

func TestProbeConnectivityClassifiesRateLimitAsWarning(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Status:     "429 Too Many Requests",
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"slow down"}}`)),
			Header:     make(http.Header),
		}, nil
	})}

	result := Probe(context.Background(), Options{
		Profile: config.ProviderProfile{
			Name:         "custom",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://example.invalid/v1",
			APIKey:       "sk-test-secret",
			Model:        "custom-model",
		},
		Connectivity: true,
		HTTPClient:   client,
		Resolver:     staticResolver{addr: netip.MustParseAddr("93.184.216.34")},
	})

	check := result.Check("provider.connectivity")
	if result.Status != StatusWarn || check == nil || check.Category != CategoryRateLimit {
		t.Fatalf("result = %#v, connectivity = %#v, want rate limit warning", result, check)
	}
}

func TestProbeConnectivityClassifiesNetworkError(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: no route to host")
	})}

	result := Probe(context.Background(), Options{
		Profile: config.ProviderProfile{
			Name:         "custom",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://example.invalid/v1",
			APIKey:       "sk-test-secret",
			Model:        "custom-model",
		},
		Connectivity: true,
		HTTPClient:   client,
		Resolver:     staticResolver{addr: netip.MustParseAddr("93.184.216.34")},
	})

	check := result.Check("provider.connectivity")
	if check == nil || check.Category != CategoryNetwork || check.Status != StatusFail {
		t.Fatalf("connectivity check = %#v, want network failure", check)
	}
}

func TestProbeConnectivityRedactsProviderErrorDetails(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Status:     "502 Bad Gateway",
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"upstream saw sk-test-secret and custom-secret-token"}}`)),
			Header:     make(http.Header),
		}, nil
	})}

	result := Probe(context.Background(), Options{
		Profile: config.ProviderProfile{
			Name:         "custom",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://example.invalid/v1",
			APIKey:       "sk-test-secret",
			Model:        "custom-model",
			CustomHeaders: map[string]string{
				"X-Api-Key": "custom-secret-token",
			},
		},
		Connectivity: true,
		HTTPClient:   client,
		Resolver:     staticResolver{addr: netip.MustParseAddr("93.184.216.34")},
	})

	check := result.Check("provider.connectivity")
	if check == nil || check.Category != CategoryProvider {
		t.Fatalf("connectivity check = %#v, want provider error", check)
	}
	rendered := check.Message + " " + fmt.Sprint(check.Details)
	if strings.Contains(rendered, "sk-test-secret") {
		t.Fatalf("secret leaked in connectivity check: %s", rendered)
	}
	if strings.Contains(rendered, "custom-secret-token") {
		t.Fatalf("custom header secret leaked in connectivity check: %s", rendered)
	}
}

func TestOverrideHealthEndpointOpenGateway(t *testing.T) {
	got, ok := overrideHealthEndpoint(config.ProviderProfile{CatalogID: "gitlawb-opengateway"}, "https://opengateway.gitlawb.com/v1")
	if !ok {
		t.Fatal("expected OpenGateway to override the health endpoint")
	}
	if got != "https://opengateway.gitlawb.com/health" {
		t.Fatalf("OpenGateway health endpoint = %q, want host-root /health", got)
	}

	if _, ok := overrideHealthEndpoint(config.ProviderProfile{CatalogID: "openai"}, "https://api.openai.com/v1"); ok {
		t.Fatal("non-OpenGateway providers must not override the health endpoint")
	}
	if _, ok := overrideHealthEndpoint(config.ProviderProfile{CatalogID: "gitlawb-opengateway"}, "::not a url"); ok {
		t.Fatal("an unparseable base URL must not produce an override")
	}
}
