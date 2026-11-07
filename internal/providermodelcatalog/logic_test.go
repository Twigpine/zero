package providermodelcatalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Twigpine/zero/internal/modelregistry"
	"github.com/Twigpine/zero/internal/providercatalog"
)

func TestLooksLikeCodingModelID(t *testing.T) {
	coding := []string{"gpt-4o", "claude-sonnet-4.5", "o1", "o3-mini", "o4-mini", "qwen2.5-coder", "deepseek-chat", "kimi-k2", "codestral-latest", "grok-code", "tencent/hy3"}
	for _, id := range coding {
		if !LooksLikeCodingModelID(id) {
			t.Errorf("LooksLikeCodingModelID(%q) = false, want true", id)
		}
	}
	nonCoding := []string{"", "   ", "whisper-1", "dall-e-3", "tts-1", "text-embedding-3-large", "omni-moderation-latest", "sora", "random-widget-7"}
	for _, id := range nonCoding {
		if LooksLikeCodingModelID(id) {
			t.Errorf("LooksLikeCodingModelID(%q) = true, want false", id)
		}
	}
}

func TestModelMatchesProvider(t *testing.T) {
	cases := []struct {
		transport providercatalog.Transport
		provider  modelregistry.ProviderKind
		want      bool
	}{
		{providercatalog.TransportOpenAI, modelregistry.ProviderOpenAI, true},
		{providercatalog.TransportOpenAI, modelregistry.ProviderAnthropic, false},
		{providercatalog.TransportAnthropic, modelregistry.ProviderAnthropic, true},
		{providercatalog.TransportAnthropicCompatible, modelregistry.ProviderAnthropic, true},
		{providercatalog.TransportGoogle, modelregistry.ProviderGoogle, true},
		{providercatalog.TransportGoogle, modelregistry.ProviderOpenAI, false},
		{providercatalog.TransportOpenAICompatible, modelregistry.ProviderOpenAI, false}, // default branch: no match
	}
	for _, tc := range cases {
		got := modelMatchesProvider(modelregistry.ModelEntry{Provider: tc.provider}, providercatalog.Descriptor{Transport: tc.transport})
		if got != tc.want {
			t.Errorf("modelMatchesProvider(provider=%q, transport=%q) = %v, want %v", tc.provider, tc.transport, got, tc.want)
		}
	}
}

func TestDefaultedOpenGatewayURL(t *testing.T) {
	if got := defaultedOpenGatewayURL(providercatalog.Descriptor{}, " https://x/v1/models "); got != "https://x/v1/models" {
		t.Fatalf("explicit override = %q, want trimmed override", got)
	}
	if got := defaultedOpenGatewayURL(providercatalog.Descriptor{DefaultBaseURL: "https://gw.example.com/v1"}, ""); got != "https://gw.example.com/v1/models" {
		t.Fatalf("derived = %q", got)
	}
	if got := defaultedOpenGatewayURL(providercatalog.Descriptor{DefaultBaseURL: "::not a url"}, ""); got != "https://opengateway.gitlawb.com/v1/models" {
		t.Fatalf("fallback = %q", got)
	}
}

func TestDefaultedOpenRouterURL(t *testing.T) {
	if got := defaultedOpenRouterURL(providercatalog.Descriptor{}, " https://or.example/api/v1/models "); got != "https://or.example/api/v1/models" {
		t.Fatalf("explicit override = %q, want trimmed override", got)
	}
	if got := defaultedOpenRouterURL(providercatalog.Descriptor{DefaultBaseURL: "https://openrouter.ai/api/v1"}, ""); got != "https://openrouter.ai/api/v1/models" {
		t.Fatalf("derived = %q", got)
	}
	if got := defaultedOpenRouterURL(providercatalog.Descriptor{DefaultBaseURL: "https://openrouter.ai/api/v1?token=x#frag"}, ""); got != "https://openrouter.ai/api/v1/models" {
		t.Fatalf("query/fragment stripped = %q, want clean /models URL", got)
	}
	if got := defaultedOpenRouterURL(providercatalog.Descriptor{DefaultBaseURL: "bad"}, ""); got != "https://openrouter.ai/api/v1/models" {
		t.Fatalf("fallback = %q", got)
	}
}

func TestModelSortLabelAndSortModels(t *testing.T) {
	if got := modelSortLabel(Model{Description: "  GPT-4o  "}); got != "gpt-4o" {
		t.Fatalf("label from description = %q", got)
	}
	if got := modelSortLabel(Model{ID: "Llama-3"}); got != "llama-3" {
		t.Fatalf("label from id = %q", got)
	}
	models := []Model{{ID: "b", Description: "Zeta"}, {ID: "a", Description: "Alpha"}}
	sortModels(models)
	if models[0].Description != "Alpha" {
		t.Fatalf("sortModels did not order by label: %#v", models)
	}
}

func TestFetchJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("Accept header = %q, want application/json", r.Header.Get("Accept"))
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	body, err := fetchJSON(context.Background(), srv.URL, srv.Client())
	if err != nil || string(body) != `{"ok":true}` {
		t.Fatalf("fetchJSON = %q, %v", string(body), err)
	}
	if _, err := fetchJSON(context.Background(), "   ", srv.Client()); err == nil {
		t.Fatal("expected an error for an empty endpoint")
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer bad.Close()
	if _, err := fetchJSON(context.Background(), bad.URL, bad.Client()); err == nil {
		t.Fatal("expected an error for a 500 response")
	}
}

func TestFetchModelsDevAndOpenGatewayOverHTTP(t *testing.T) {
	modelsDev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"openai":{"models":{"gpt-4o":{"id":"gpt-4o"}}}}`))
	}))
	defer modelsDev.Close()
	models, err := FetchModelsDev(context.Background(), "openai", FetchOptions{HTTPClient: modelsDev.Client(), ModelsDevURL: modelsDev.URL})
	if err != nil {
		t.Fatalf("FetchModelsDev error: %v", err)
	}
	if !containsModelID(models, "gpt-4o") {
		t.Fatalf("FetchModelsDev models = %#v, want gpt-4o", models)
	}

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"models":[{"id":"claude-coder"}]}`))
	}))
	defer gateway.Close()
	models, err = FetchOpenGateway(context.Background(), gateway.URL, FetchOptions{HTTPClient: gateway.Client()})
	if err != nil {
		t.Fatalf("FetchOpenGateway error: %v", err)
	}
	if !containsModelID(models, "claude-coder") {
		t.Fatalf("FetchOpenGateway models = %#v, want claude-coder", models)
	}

	// FetchRemote routes the opengateway provider to FetchOpenGateway via the override URL.
	routed, err := FetchRemote(context.Background(), providercatalog.Descriptor{ID: "gitlawb-opengateway"}, FetchOptions{HTTPClient: gateway.Client(), OpenGatewayURL: gateway.URL})
	if err != nil {
		t.Fatalf("FetchRemote error: %v", err)
	}
	if !containsModelID(routed, "claude-coder") {
		t.Fatalf("FetchRemote models = %#v, want claude-coder", routed)
	}

	openrouter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"openai/gpt-4o","name":"GPT-4o","context_length":128000,"supported_parameters":["tools"]}]}`))
	}))
	defer openrouter.Close()
	orModels, err := FetchOpenRouter(context.Background(), openrouter.URL, FetchOptions{HTTPClient: openrouter.Client()})
	if err != nil {
		t.Fatalf("FetchOpenRouter error: %v", err)
	}
	if !containsModelID(orModels, "openai/gpt-4o") {
		t.Fatalf("FetchOpenRouter models = %#v, want openai/gpt-4o", orModels)
	}
	routedOR, err := FetchRemote(context.Background(), providercatalog.Descriptor{ID: "openrouter"}, FetchOptions{HTTPClient: openrouter.Client(), OpenRouterURL: openrouter.URL})
	if err != nil {
		t.Fatalf("FetchRemote openrouter error: %v", err)
	}
	if !containsModelID(routedOR, "openai/gpt-4o") {
		t.Fatalf("FetchRemote openrouter models = %#v, want openai/gpt-4o", routedOR)
	}
}

// TestFetchOpenRouterFallsBackToModelsDev pins independent resilience: when
// openrouter.ai is down, models.dev still supplies a coding list so the picker
// does not error empty.
func TestFetchOpenRouterFallsBackToModelsDev(t *testing.T) {
	openrouter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}))
	defer openrouter.Close()

	modelsDev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"openrouter": {
				"models": {
					"openai/gpt-4.1": {
						"id": "openai/gpt-4.1",
						"name": "GPT-4.1",
						"tool_call": true,
						"limit": {"context": 1048576},
						"cost": {"input": 2, "output": 8},
						"modalities": {"input": ["text"], "output": ["text"]}
					}
				}
			}
		}`))
	}))
	defer modelsDev.Close()

	models, err := FetchOpenRouter(context.Background(), openrouter.URL, FetchOptions{
		HTTPClient:   openrouter.Client(),
		ModelsDevURL: modelsDev.URL,
	})
	if err != nil {
		t.Fatalf("FetchOpenRouter with models.dev fallback: %v", err)
	}
	if !containsModelID(models, "openai/gpt-4.1") {
		t.Fatalf("models = %#v, want models.dev openrouter fallback entry", models)
	}
	if models[0].Source != "models.dev" {
		t.Fatalf("source = %q, want models.dev fallback source", models[0].Source)
	}
}

// TestFetchOpenRouterFallsBackOnMalformedJSON covers the HTTP-200-but-unparseable
// path: a broken openrouter.ai body must still degrade to models.dev instead of
// failing the catalog fetch entirely.
func TestFetchOpenRouterFallsBackOnMalformedJSON(t *testing.T) {
	openrouter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data": not-json`))
	}))
	defer openrouter.Close()

	modelsDev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"openrouter": {
				"models": {
					"openai/gpt-4.1": {
						"id": "openai/gpt-4.1",
						"name": "GPT-4.1",
						"tool_call": true,
						"limit": {"context": 1048576},
						"cost": {"input": 2, "output": 8},
						"modalities": {"input": ["text"], "output": ["text"]}
					}
				}
			}
		}`))
	}))
	defer modelsDev.Close()

	models, err := FetchOpenRouter(context.Background(), openrouter.URL, FetchOptions{
		HTTPClient:   openrouter.Client(),
		ModelsDevURL: modelsDev.URL,
	})
	if err != nil {
		t.Fatalf("FetchOpenRouter malformed JSON fallback: %v", err)
	}
	if !containsModelID(models, "openai/gpt-4.1") {
		t.Fatalf("models = %#v, want models.dev openrouter fallback entry", models)
	}
	if models[0].Source != "models.dev" {
		t.Fatalf("source = %q, want models.dev fallback source", models[0].Source)
	}
}

func containsModelID(models []Model, id string) bool {
	for _, model := range models {
		if model.ID == id {
			return true
		}
	}
	return false
}
