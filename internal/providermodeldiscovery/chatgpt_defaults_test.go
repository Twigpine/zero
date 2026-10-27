package providermodeldiscovery

import (
	"context"
	"github.com/Twigpine/zero/internal/config"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestChatGPTDiscoveryUsesCurrentCompatibilityVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("client_version"); got != "0.159.0" {
			t.Errorf("client_version = %q, want documented GPT-6.1 Terra compatibility floor 0.159.0", got)
		}
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-future-coder"}]}`))
	}))
	defer server.Close()
	profile := config.ProviderProfile{
		CatalogID: "chatgpt", ProviderKind: config.ProviderKindOpenAICompatible,
		BaseURL: server.URL + "/backend-api/codex", APIKey: "test-bearer",
	}
	if _, err := Discover(context.Background(), profile, Options{HTTPClient: server.Client()}); err != nil {
		t.Fatal(err)
	}
}
