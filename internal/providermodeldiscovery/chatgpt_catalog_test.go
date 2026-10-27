package providermodeldiscovery

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/providercatalog"
)

func chatGPTTestProfile(baseURL string) (providercatalog.Descriptor, config.ProviderProfile) {
	provider, _ := providercatalog.Get("chatgpt")
	return provider, config.ProviderProfile{
		Name: "subscription", CatalogID: "chatgpt", ProviderKind: config.ProviderKindOpenAICompatible,
		BaseURL: baseURL + "/backend-api/codex", APIKey: "test-bearer",
	}
}

func TestChatGPTCatalogRequiresLiveDiscovery(t *testing.T) {
	for _, response := range []string{"unauthorized", "empty", "non-coding"} {
		t.Run(response, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api.json" {
					_, _ = w.Write([]byte(`{"openai":{"models":{"gpt-stale":{"id":"gpt-stale","tool_call":true}}}}`))
					return
				}
				switch response {
				case "unauthorized":
					http.Error(w, "login expired", http.StatusUnauthorized)
				case "empty":
					_, _ = w.Write([]byte(`{"models":[]}`))
				case "non-coding":
					_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-image-1"}]}`))
				}
			}))
			defer server.Close()
			provider, profile := chatGPTTestProfile(server.URL)
			models, err := DiscoverCatalog(context.Background(), provider, profile, Options{
				HTTPClient: server.Client(), ModelsDevURL: server.URL + "/api.json",
			})
			if err == nil || len(models) != 0 {
				t.Fatalf("failed subscription discovery returned models=%v err=%v; public metadata must not claim account availability", modelIDs(models), err)
			}
		})
	}
}

func TestChatGPTCatalogProbesLiveBeforeOptionalMetadata(t *testing.T) {
	var liveSeen atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api.json" {
			if !liveSeen.Load() {
				t.Error("optional public metadata was fetched before the subscription catalog")
			}
			http.Error(w, "metadata unavailable", http.StatusServiceUnavailable)
			return
		}
		liveSeen.Store(true)
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-future-coder","visibility":"list","context_window":123456}]}`))
	}))
	defer server.Close()
	provider, profile := chatGPTTestProfile(server.URL)
	models, err := DiscoverCatalog(context.Background(), provider, profile, Options{
		HTTPClient: server.Client(), ModelsDevURL: server.URL + "/api.json",
	})
	if err != nil || len(models) != 1 || models[0].ID != "gpt-future-coder" || models[0].ContextWindow != 123456 {
		t.Fatalf("live models lost when metadata failed: models=%v err=%v", models, err)
	}
}

func TestChatGPTCatalogBoundsOptionalMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api.json" {
			<-r.Context().Done()
			return
		}
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-future-coder"}]}`))
	}))
	defer server.Close()
	provider, profile := chatGPTTestProfile(server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	models, err := DiscoverCatalog(ctx, provider, profile, Options{
		HTTPClient: server.Client(), ModelsDevURL: server.URL + "/api.json",
	})
	if err != nil || strings.Join(modelIDs(models), ",") != "gpt-future-coder" {
		t.Fatalf("slow metadata prevented live results: models=%v err=%v", modelIDs(models), err)
	}
}

func TestChatGPTDiscoveryRedactsRefreshedOAuthBearer(t *testing.T) {
	const oldToken, newToken = "opaque-old-subscription", "opaque-new-subscription"
	for _, split := range []string{"", "\x00", "\x1b", "\u0085", "\x1b[31m"} {
		t.Run(fmt.Sprintf("split-%x", split), func(t *testing.T) {
			var requests int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				body := "expired " + oldToken
				if requests > 1 {
					body = "expired opaque-" + split + "new-subscription and " + oldToken
				}
				http.Error(w, body, http.StatusUnauthorized)
			}))
			defer server.Close()
			_, profile := chatGPTTestProfile(server.URL)
			profile.APIKey = ""
			_, err := Discover(context.Background(), profile, Options{
				HTTPClient: server.Client(),
				OAuthResolver: func(_ context.Context, force bool) (string, string, bool, error) {
					token := oldToken
					if force {
						token = newToken
					}
					return "Authorization", "Bearer " + token, true, nil
				},
			})
			if err == nil || requests != 2 {
				t.Fatalf("expected failed refresh retry: requests=%d err=%v", requests, err)
			}
			if strings.Contains(err.Error(), oldToken) || strings.Contains(err.Error(), "new-subscription") {
				t.Fatalf("discovery error exposed OAuth bearer: %v", err)
			}
		})
	}
}
