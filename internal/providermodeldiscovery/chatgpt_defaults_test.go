package providermodeldiscovery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Twigpine/zero/internal/config"
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
	models, err := Discover(context.Background(), profile, Options{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "gpt-future-coder" {
		t.Fatalf("models = %#v, want only gpt-future-coder", models)
	}
}
