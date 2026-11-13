package providermodeldiscovery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/providercatalog"
)

func TestDiscoverCatalogFallsBackWhenLiveIDsMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api.json":
			_, _ = w.Write([]byte(`{"openai":{"models":{"gpt-4.1":{"id":"gpt-4.1","name":"GPT-4.1","tool_call":true,"reasoning":true,"limit":{"context":1048576}}}}}`))
		case "/v1/models":
			// 200, but live model IDs don't match any curated coding model, so the
			// merge is empty (M11): the result must fall back to the curated catalog,
			// not collapse to an empty list.
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-image-1"},{"id":"text-embedding-3-large"}]}`))
		default:
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	provider := providercatalog.Descriptor{ID: "openai", Transport: providercatalog.TransportOpenAI, DefaultBaseURL: server.URL + "/v1", RequiresAuth: true}
	models, err := DiscoverCatalog(context.Background(), provider, config.ProviderProfile{
		CatalogID: "openai", ProviderKind: config.ProviderKindOpenAI, BaseURL: server.URL + "/v1", APIKey: "sk-live",
	}, Options{HTTPClient: server.Client(), ModelsDevURL: server.URL + "/api.json"})
	if err != nil {
		t.Fatalf("DiscoverCatalog returned error: %v", err)
	}
	if got := strings.Join(modelIDs(models), ","); got != "gpt-4.1" {
		t.Fatalf("models = %q, want curated catalog fallback (gpt-4.1) instead of an empty list", got)
	}
}

// Concentrate's /v1/models lists dated snapshots as ids and keeps rolling names
// such as deepseek-v4-flash only in "aliases", which discovery does not read.
// Onboarding keeps the default selected only when it matches a live id, so the
// preset's default must be an id, or setup lands on whatever model sorts first.
func TestDiscoverCatalogConcentrateKeepsDefaultModelInLiveList(t *testing.T) {
	var liveHits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api.json":
			_, _ = w.Write([]byte(`{}`))
		case "/v1/models":
			liveHits.Add(1)
			_, _ = w.Write([]byte(`{"object":"list","data":[
				{"id":"claude-fable-5","slug":"claude-fable-5","max_input_tokens":1000000},
				{"id":"deepseek-v4-flash-0731","slug":"deepseek-v4-flash-0731","aliases":["deepseek-v4-flash"],"max_input_tokens":1048576},
				{"id":"deepseek-v4-pro","slug":"deepseek-v4-pro","max_input_tokens":1048576},
				{"id":"kimi-k3","slug":"kimi-k3","max_input_tokens":1048576},
				{"id":"glm-5.3","slug":"glm-5.3","max_input_tokens":1048576}
			]}`))
		default:
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	provider, ok := providercatalog.Get("concentrate")
	if !ok {
		t.Fatal("provider concentrate missing from catalog")
	}
	provider.DefaultBaseURL = server.URL + "/v1"
	models, err := DiscoverCatalog(context.Background(), provider, config.ProviderProfile{
		CatalogID: "concentrate", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: server.URL + "/v1", APIKey: "sk-test",
	}, Options{HTTPClient: server.Client(), ModelsDevURL: server.URL + "/api.json"})
	if err != nil {
		t.Fatalf("DiscoverCatalog returned error: %v", err)
	}
	if liveHits.Load() == 0 {
		t.Fatal("DiscoverCatalog never requested /v1/models")
	}
	// claude-fable-5 is only in the live response, never in the curated list:
	// finding it proves the result is the live list, not the curated fallback
	// (which also contains the default and would pass on its own).
	got := "," + strings.Join(modelIDs(models), ",") + ","
	if !strings.Contains(got, ",claude-fable-5,") {
		t.Fatalf("models = %q, want the live Concentrate list (with live-only claude-fable-5), not the curated fallback", strings.Trim(got, ","))
	}
	if !strings.Contains(got, ","+provider.DefaultModel+",") {
		t.Fatalf("default model %q missing from discovered models %q", provider.DefaultModel, strings.Trim(got, ","))
	}
}
