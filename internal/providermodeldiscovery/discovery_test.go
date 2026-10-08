package providermodeldiscovery

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/providercatalog"
)

func TestDiscoverOpenAICompatibleModelsFetchesModelsEndpoint(t *testing.T) {
	const apiKey = "sk-live-secret"
	var gotPath string
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"object": "list",
			"data": [
				{"id": "model-b", "object": "model"},
				{"id": "model-a", "object": "model"},
				{"id": "model-a", "object": "model"},
				{"object": "model"}
			]
		}`))
	}))
	defer server.Close()

	models, err := Discover(context.Background(), config.ProviderProfile{
		Name:         "test",
		ProviderKind: config.ProviderKindOpenAICompatible,
		BaseURL:      server.URL + "/v1",
		APIKey:       apiKey,
	}, Options{HTTPClient: server.Client()})
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if gotPath != "/v1/models" {
		t.Fatalf("requested path = %q, want /v1/models", gotPath)
	}
	if gotAuth != "Bearer "+apiKey {
		t.Fatalf("Authorization = %q, want bearer API key", gotAuth)
	}
	if got, want := modelIDs(models), []string{"model-a", "model-b"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("models = %#v, want %#v", got, want)
	}
}

func TestParseModelsResponseCapturesContextAndFree(t *testing.T) {
	models, err := parseModelsResponse([]byte(`{
		"data": [
			{"id": "xiaomi/mimo-v2.5-pro", "name": "MiMo V2.5-Pro", "context_window": 262144},
			{"id": "nvidia/nemotron-3-ultra:free", "name": "Nemotron", "context_length": 128000, "is_free": true}
		]
	}`))
	if err != nil {
		t.Fatalf("parseModelsResponse: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %#v, want 2", models)
	}
	if got, want := modelIDs(models), []string{"nvidia/nemotron-3-ultra:free", "xiaomi/mimo-v2.5-pro"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("models = %#v, want %#v", got, want)
	}
	byID := map[string]Model{}
	for _, model := range models {
		byID[model.ID] = model
	}
	if byID["xiaomi/mimo-v2.5-pro"].ContextWindow != 262144 {
		t.Fatalf("mimo context = %d", byID["xiaomi/mimo-v2.5-pro"].ContextWindow)
	}
	if byID["nvidia/nemotron-3-ultra:free"].Description != "Nemotron (free)" {
		t.Fatalf("free model description = %q, want annotated free label", byID["nvidia/nemotron-3-ultra:free"].Description)
	}
}

func TestParseModelsResponseSupportsChatGPTCatalog(t *testing.T) {
	models, err := parseModelsResponse([]byte(`{
		"models": [
			{"slug":"gpt-5.6-sol","display_name":"GPT-5.6 Sol","description":"Frontier coding model","visibility":"list","context_window":400000,"default_reasoning_level":"high","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"},{"effort":"xhigh"}],"service_tiers":[{"id":"priority"}],"default_service_tier":"standard"},
			{"slug":"gpt-5.6-terra","display_name":"GPT-5.6 Terra","visibility":"list"},
			{"slug":"internal-router","display_name":"Internal","visibility":"hide"},
			{"slug":"server-only","display_name":"Server only","visibility":"none"}
		]
	}`))
	if err != nil {
		t.Fatalf("parseModelsResponse: %v", err)
	}
	if got, want := modelIDs(models), []string{"gpt-5.6-sol", "gpt-5.6-terra"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("models = %#v, want visible ChatGPT models %#v", got, want)
	}
	if models[0].Description != "GPT-5.6 Sol" || models[0].ContextWindow != 400000 {
		t.Fatalf("ChatGPT metadata = %#v", models[0])
	}
	if got := strings.Join(models[0].ReasoningEfforts, ","); got != "low,medium,high,xhigh" {
		t.Fatalf("reasoning efforts = %q", got)
	}
	if models[0].DefaultReasoningEffort != "high" || !models[0].Reasoning {
		t.Fatalf("reasoning metadata = %#v", models[0])
	}
	if got := strings.Join(models[0].ServiceTiers, ","); got != "priority" || models[0].DefaultServiceTier != "standard" {
		t.Fatalf("service tier metadata = %#v", models[0])
	}
}

func TestParseModelsResponseNormalizesLegacyFastTier(t *testing.T) {
	models, err := parseModelsResponse([]byte(`{"data":[{"id":"gpt-test","additional_speed_tiers":["fast","priority"]}]}`))
	if err != nil {
		t.Fatalf("parseModelsResponse: %v", err)
	}
	if got := strings.Join(models[0].ServiceTiers, ","); got != "priority" {
		t.Fatalf("service tiers = %q, want priority", got)
	}
}

func TestMergeChatGPTModelsKeepsLiveOnlyEntries(t *testing.T) {
	models := mergeLiveModels(
		providercatalog.Descriptor{ID: "chatgpt"},
		[]Model{
			{ID: "gpt-5.5", ReasoningEfforts: []string{"low", "high"}, ServiceTiers: []string{"priority"}},
			{ID: "gpt-5.6-sol", ReasoningEfforts: []string{"low", "ultra"}},
		},
		[]Model{{ID: "gpt-5.5", Description: "fallback"}},
	)
	if got, want := strings.Join(modelIDs(models), ","), "gpt-5.5,gpt-5.6-sol"; got != want {
		t.Fatalf("models = %q, want %q", got, want)
	}
	if got := strings.Join(models[0].ReasoningEfforts, ","); got != "low,high" {
		t.Fatalf("merged reasoning efforts = %q", got)
	}
	if got := strings.Join(models[0].ServiceTiers, ","); got != "priority" {
		t.Fatalf("merged service tiers = %q", got)
	}
}

func TestDiscoverCatalogChatGPTMergesOpenAIModelsDevMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api.json":
			_, _ = w.Write([]byte(`{
				"openai": {"models": {
					"gpt-5.6-sol": {
						"id": "gpt-5.6-sol",
						"name": "GPT-5.6 Sol",
						"description": "Frontier coding model",
						"tool_call": true,
						"reasoning": true,
						"reasoning_efforts": ["low", "high", "max"],
						"limit": {"context": 1050000},
						"modalities": {"input": ["text", "image", "pdf"], "output": ["text"]}
					}
				}}
			}`))
		case "/backend-api/codex/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.6-sol"}]}`))
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
	}))
	defer server.Close()

	provider, ok := providercatalog.Get("chatgpt")
	if !ok {
		t.Fatal("ChatGPT provider missing from catalog")
	}
	models, err := DiscoverCatalog(context.Background(), provider, config.ProviderProfile{
		Name:         "chatgpt",
		CatalogID:    "chatgpt",
		ProviderKind: config.ProviderKindOpenAICompatible,
		BaseURL:      server.URL + "/backend-api/codex",
	}, Options{
		HTTPClient:   server.Client(),
		ModelsDevURL: server.URL + "/api.json",
		OAuthResolver: func(context.Context, bool) (string, string, bool, error) {
			return "Authorization", "Bearer test", true, nil
		},
	})
	if err != nil {
		t.Fatalf("DiscoverCatalog returned error: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("models = %#v, want one ChatGPT live model", models)
	}
	model := models[0]
	if model.ID != "gpt-5.6-sol" || model.ContextWindow != 1050000 || !model.ToolCall || !model.Reasoning {
		t.Fatalf("merged ChatGPT model = %#v, want models.dev metadata", model)
	}
	if got := strings.Join(model.InputModalities, ","); got != "text,image,pdf" {
		t.Fatalf("input modalities = %q, want text,image,pdf", got)
	}
	if got := strings.Join(model.ReasoningEfforts, ","); got != "low,high,max" {
		t.Fatalf("reasoning efforts = %q, want low,high,max", got)
	}
}

func TestMergeLiveModelsUsesLiveDefaultsAndReasoningCapability(t *testing.T) {
	models := mergeLiveModels(
		providercatalog.Descriptor{ID: "chatgpt"},
		[]Model{{
			ID:                     "gpt-live",
			Reasoning:              true,
			DefaultReasoningEffort: "high",
			DefaultServiceTier:     "priority",
		}},
		[]Model{{
			ID:                     "gpt-live",
			Reasoning:              false,
			DefaultReasoningEffort: "low",
			DefaultServiceTier:     "standard",
		}},
	)
	if len(models) != 1 {
		t.Fatalf("merged models = %#v, want one model", models)
	}
	got := models[0]
	if !got.Reasoning || got.DefaultReasoningEffort != "high" || got.DefaultServiceTier != "priority" {
		t.Fatalf("merged live metadata = %#v", got)
	}
}

func TestMergeLocalModelsKeepsLiveOnlyEntries(t *testing.T) {
	models := mergeLiveModels(
		providercatalog.Descriptor{ID: "lmstudio", Local: true},
		[]Model{
			{ID: "qwen3-coder-30b", ContextWindow: 32768},
			{ID: "text-embedding-local"},
			{ID: "qwen2.5-coder-32b-instruct", Description: "live description"},
		},
		[]Model{{ID: "qwen2.5-coder-32b-instruct", Description: "catalog description", ToolCall: true}},
	)

	if got, want := strings.Join(modelIDs(models), ","), "qwen3-coder-30b,qwen2.5-coder-32b-instruct"; got != want {
		t.Fatalf("models = %q, want %q", got, want)
	}
	for _, model := range models {
		if model.ID == "qwen2.5-coder-32b-instruct" {
			if model.Description != "catalog description" || !model.ToolCall {
				t.Fatalf("merged catalog metadata = %#v", model)
			}
		}
	}
}

func TestDiscoverCatalogOpenGatewayUsesLiveListWithoutKey(t *testing.T) {
	// Catalog and live endpoints return distinct payloads so the merge must keep
	// live-only ids that are absent from the remote catalog response.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/catalog/v1/models":
			_, _ = w.Write([]byte(`{"data":[
				{"id":"auto","name":"Auto (smart routing)"},
				{"id":"xiaomi/mimo-v2.5-pro","name":"MiMo V2.5-Pro","context_window":262144}
			]}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[
				{"id":"auto","name":"Auto (smart routing)"},
				{"id":"xiaomi/mimo-v2.5-pro","name":"MiMo V2.5-Pro","context_window":262144},
				{"id":"live-only-model","name":"Live Only","context_window":64000}
			]}`))
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	provider := providercatalog.Descriptor{
		ID:             "gitlawb-opengateway",
		Transport:      providercatalog.TransportOpenAICompat,
		DefaultBaseURL: server.URL + "/v1",
		RequiresAuth:   true,
	}
	models, err := DiscoverCatalog(context.Background(), provider, config.ProviderProfile{
		CatalogID:    "gitlawb-opengateway",
		ProviderKind: config.ProviderKindOpenAICompatible,
		BaseURL:      server.URL + "/v1",
		// No API key: public live catalog must still load.
	}, Options{
		HTTPClient:     server.Client(),
		OpenGatewayURL: server.URL + "/catalog/v1/models",
	})
	if err != nil {
		t.Fatalf("DiscoverCatalog: %v", err)
	}
	got := strings.Join(modelIDs(models), ",")
	if !strings.Contains(got, "auto") || !strings.Contains(got, "xiaomi/mimo-v2.5-pro") || !strings.Contains(got, "live-only-model") {
		t.Fatalf("models = %q, want auto + mimo + live-only", got)
	}
	for _, model := range models {
		if model.ID == "xiaomi/mimo-v2.5-pro" && model.ContextWindow != 262144 {
			t.Fatalf("mimo metadata = %#v", model)
		}
		if model.ID == "live-only-model" && model.ContextWindow != 64000 {
			t.Fatalf("live-only metadata = %#v", model)
		}
	}
}

func TestDiscoverCatalogOpenRouterKeepsLiveOnlyModels(t *testing.T) {
	// Catalog omits anthropic/claude-sonnet-4.5 and the generic tools-only id;
	// live retains both so preferLive keeps coding-capable live-only entries
	// (id heuristic + capability flags). No API key: public listing is unauth.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/catalog/api/v1/models":
			_, _ = w.Write([]byte(`{"data":[
				{"id":"openai/gpt-4.1","name":"GPT-4.1","context_length":1048576,"supported_parameters":["tools"]},
				{"id":"text-embedding-3-large","name":"Embedding"}
			]}`))
		case "/api/v1/models", "/v1/models":
			_, _ = w.Write([]byte(`{"data":[
				{"id":"openai/gpt-4.1","name":"GPT-4.1","context_length":1048576,"supported_parameters":["tools"]},
				{"id":"anthropic/claude-sonnet-4.5","name":"Claude Sonnet 4.5","context_length":200000,"supported_parameters":["tools","reasoning"]},
				{"id":"vendor/generic-tools-model","name":"Generic Tools","context_length":32000,"supported_parameters":["tools"]},
				{"id":"text-embedding-3-large","name":"Embedding"}
			]}`))
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	provider := providercatalog.Descriptor{
		ID:             "openrouter",
		Transport:      providercatalog.TransportOpenAICompat,
		DefaultBaseURL: server.URL + "/api/v1",
		RequiresAuth:   true,
	}
	models, err := DiscoverCatalog(context.Background(), provider, config.ProviderProfile{
		CatalogID:    "openrouter",
		ProviderKind: config.ProviderKindOpenAICompatible,
		BaseURL:      server.URL + "/api/v1",
		// No API key: unauthenticated public listing path.
	}, Options{
		HTTPClient:    server.Client(),
		OpenRouterURL: server.URL + "/catalog/api/v1/models",
	})
	if err != nil {
		t.Fatalf("DiscoverCatalog: %v", err)
	}
	got := strings.Join(modelIDs(models), ",")
	if !strings.Contains(got, "openai/gpt-4.1") || !strings.Contains(got, "anthropic/claude-sonnet-4.5") {
		t.Fatalf("models = %q, want live openrouter coding models including live-only claude", got)
	}
	if !strings.Contains(got, "vendor/generic-tools-model") {
		t.Fatalf("models = %q, want capability-marked live-only generic tools model", got)
	}
	if strings.Contains(got, "text-embedding-3-large") {
		t.Fatalf("models = %q, embedding model should be filtered", got)
	}
	for _, model := range models {
		if model.ID == "openai/gpt-4.1" && model.ContextWindow != 1048576 {
			t.Fatalf("gpt-4.1 metadata = %#v, want live context window", model)
		}
		if model.ID == "anthropic/claude-sonnet-4.5" && model.ContextWindow != 200000 {
			t.Fatalf("claude live-only metadata = %#v, want live context window", model)
		}
		if model.ID == "vendor/generic-tools-model" && (!model.ToolCall || model.ContextWindow != 32000) {
			t.Fatalf("generic tools live-only = %#v, want tools + context from live payload", model)
		}
	}
}

func TestDiscoverChatGPTModelsUsesOAuthAndCodexHeaders(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.URL.Path; got != "/backend-api/codex/models" {
			t.Errorf("path = %q, want Codex models endpoint", got)
		}
		if got := r.URL.Query().Get("client_version"); got != chatGPTModelsProtocolVersion {
			t.Errorf("client_version = %q, want %q", got, chatGPTModelsProtocolVersion)
		}
		wantToken := "Bearer old-token"
		wantAccount := "old-account"
		if requests == 2 {
			wantToken = "Bearer refreshed-token"
			wantAccount = "refreshed-account"
		}
		if got := r.Header.Get("Authorization"); got != wantToken {
			t.Errorf("request %d Authorization = %q, want %q", requests, got, wantToken)
		}
		if got := r.Header.Get("chatgpt-account-id"); got != wantAccount {
			t.Errorf("request %d chatgpt-account-id = %q, want %q", requests, got, wantAccount)
		}
		if got := r.Header.Get("originator"); got != "codex_cli_rs" {
			t.Errorf("originator = %q", got)
		}
		if requests == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.4"}]}`))
	}))
	defer server.Close()

	models, err := Discover(context.Background(), config.ProviderProfile{
		CatalogID:    "chatgpt",
		ProviderKind: config.ProviderKindOpenAICompatible,
		BaseURL:      server.URL + "/backend-api/codex",
	}, Options{
		HTTPClient: server.Client(),
		OAuthResolver: func(_ context.Context, force bool) (string, string, bool, error) {
			if force {
				return "Authorization", "Bearer refreshed-token", true, nil
			}
			return "Authorization", "Bearer old-token", true, nil
		},
		CodexAccountResolver: func(context.Context) (string, bool, error) {
			if requests == 0 {
				return "old-account", true, nil
			}
			return "refreshed-account", true, nil
		},
	})
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if requests != 2 || len(models) != 1 || models[0].ID != "gpt-5.4" {
		t.Fatalf("requests = %d, models = %#v", requests, models)
	}
}

func TestDiscoverAIMLAPIModelsSendsAuthAndCustomHeadersWithoutAttribution(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for header, want := range map[string]string{
			"Authorization": "Bearer test-key",
			"X-Trace":       "test",
		} {
			if got := r.Header.Get(header); got != want {
				t.Errorf("%s = %q, want %q", header, got, want)
			}
		}
		// No first-party referral/attribution headers are injected for catalog
		// presets; aimlapi rides through CopyHeaders like every other provider.
		for _, header := range []string{
			"X-AIMLAPI-Partner-ID",
			"X-AIMLAPI-Integration-Repo",
			"X-AIMLAPI-Integration-Version",
		} {
			if got := r.Header.Get(header); got != "" {
				t.Errorf("%s = %q, want no attribution header", header, got)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"openai/gpt-5-chat"}]}`))
	}))
	defer server.Close()

	_, err := Discover(context.Background(), config.ProviderProfile{
		CatalogID:     "aimlapi",
		ProviderKind:  config.ProviderKindOpenAICompatible,
		BaseURL:       server.URL + "/v1",
		APIKey:        "test-key",
		CustomHeaders: map[string]string{"X-Trace": "test"},
	}, Options{HTTPClient: server.Client()})
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
}

func TestDiscoverOpenAICompatibleModelsHonorsAuthHeaderValue(t *testing.T) {
	// A profile can authenticate via a raw auth-header value instead of APIKey;
	// discovery must send it rather than probe unauthenticated.
	const headerValue = "Bearer raw-header-secret"
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
	}))
	defer server.Close()

	if _, err := Discover(context.Background(), config.ProviderProfile{
		Name:            "test",
		ProviderKind:    config.ProviderKindOpenAICompatible,
		BaseURL:         server.URL + "/v1",
		AuthHeaderValue: headerValue,
	}, Options{HTTPClient: server.Client()}); err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if gotAuth != headerValue {
		t.Fatalf("Authorization = %q, want raw auth-header value %q", gotAuth, headerValue)
	}
}

func TestDiscoveryHasCredential(t *testing.T) {
	cases := []struct {
		name    string
		profile config.ProviderProfile
		want    bool
	}{
		{"api key", config.ProviderProfile{APIKey: "sk-x"}, true},
		{"auth header only", config.ProviderProfile{AuthHeaderValue: "Bearer t"}, true},
		{"both", config.ProviderProfile{APIKey: "sk-x", AuthHeaderValue: "Bearer t"}, true},
		{"neither", config.ProviderProfile{}, false},
		{"whitespace only", config.ProviderProfile{APIKey: "  ", AuthHeaderValue: "\t"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := discoveryHasCredential(tc.profile); got != tc.want {
				t.Fatalf("discoveryHasCredential = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDiscoverOpenAICompatibleModelsHandlesBaseURLWithoutVersion(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"data":[{"id":"local-model"}]}`))
	}))
	defer server.Close()

	models, err := Discover(context.Background(), config.ProviderProfile{
		Name:         "local",
		ProviderKind: config.ProviderKindOpenAICompatible,
		BaseURL:      server.URL,
	}, Options{HTTPClient: server.Client()})
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if gotPath != "/models" {
		t.Fatalf("requested path = %q, want /models for provider base URLs without /v1", gotPath)
	}
	if len(models) != 1 || models[0].ID != "local-model" {
		t.Fatalf("models = %#v, want local-model", models)
	}
}

func TestDiscoverOpenAICompatibleModelsRejectsUnsupportedProviders(t *testing.T) {
	_, err := Discover(context.Background(), config.ProviderProfile{
		Name:         "google",
		ProviderKind: config.ProviderKindGoogle,
		BaseURL:      "https://generativelanguage.googleapis.com",
	}, Options{})
	if err == nil || !strings.Contains(err.Error(), "does not expose model discovery") {
		t.Fatalf("Discover error = %v, want unsupported provider message", err)
	}
}

func TestDiscoverAnthropicCompatibleModelsFetchesModelsEndpoint(t *testing.T) {
	const apiKey = "sk-ant-secret"
	var gotPath string
	var gotAPIKey string
	var gotVersion string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{"id": "claude-custom-b", "display_name": "Claude Custom B"},
				{"id": "claude-custom-a", "display_name": "Claude Custom A"},
				{"id": "claude-custom-a", "display_name": "Claude Custom A"},
				{}
			]
		}`))
	}))
	defer server.Close()

	models, err := Discover(context.Background(), config.ProviderProfile{
		Name:         "custom",
		ProviderKind: config.ProviderKindAnthropicCompat,
		BaseURL:      server.URL + "/anthropic",
		APIKey:       apiKey,
	}, Options{HTTPClient: server.Client()})
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if gotPath != "/anthropic/v1/models" {
		t.Fatalf("requested path = %q, want /anthropic/v1/models", gotPath)
	}
	if gotAPIKey != apiKey {
		t.Fatalf("x-api-key = %q, want API key", gotAPIKey)
	}
	if gotVersion == "" {
		t.Fatal("anthropic-version header is required")
	}
	if got, want := modelIDs(models), []string{"claude-custom-a", "claude-custom-b"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("models = %#v, want %#v", got, want)
	}
}

func TestDiscoverOpenAICompatibleModelsRedactsSecretsInErrors(t *testing.T) {
	const apiKey = "sk-live-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key "+apiKey, http.StatusUnauthorized)
	}))
	defer server.Close()

	_, err := Discover(context.Background(), config.ProviderProfile{
		Name:         "test",
		ProviderKind: config.ProviderKindOpenAICompatible,
		BaseURL:      server.URL + "/v1",
		APIKey:       apiKey,
	}, Options{HTTPClient: server.Client()})
	if err == nil {
		t.Fatal("Discover should return an error for non-2xx status")
	}
	if strings.Contains(err.Error(), apiKey) {
		t.Fatalf("error leaked API key: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("error should contain redacted marker, got: %v", err)
	}
}

func TestDiscoverCatalogMergesLiveModelsWithModelsDevMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api.json":
			_, _ = w.Write([]byte(`{
				"openai": {
					"models": {
						"gpt-4.1": {
							"id": "gpt-4.1",
							"name": "GPT-4.1",
							"tool_call": true,
							"reasoning": true,
							"limit": {"context": 1048576}
						},
						"not-enabled": {"id": "not-enabled"}
					}
				}
			}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[
				{"id":"gpt-4.1"},
				{"id":"gpt-image-1"},
				{"id":"text-embedding-3-large"},
				{"id":"not-enabled"}
			]}`))
		default:
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	provider := providercatalog.Descriptor{
		ID:             "openai",
		Transport:      providercatalog.TransportOpenAI,
		DefaultBaseURL: server.URL + "/v1",
		RequiresAuth:   true,
	}
	models, err := DiscoverCatalog(context.Background(), provider, config.ProviderProfile{
		CatalogID:    "openai",
		ProviderKind: config.ProviderKindOpenAI,
		BaseURL:      server.URL + "/v1",
		APIKey:       "sk-live",
	}, Options{HTTPClient: server.Client(), ModelsDevURL: server.URL + "/api.json"})
	if err != nil {
		t.Fatalf("DiscoverCatalog returned error: %v", err)
	}
	if got := strings.Join(modelIDs(models), ","); got != "gpt-4.1" {
		t.Fatalf("models = %s, want live coding model IDs only", got)
	}
	for _, model := range models {
		if model.ID == "gpt-4.1" {
			if model.ContextWindow != 1048576 || !model.ToolCall || !model.Reasoning {
				t.Fatalf("gpt-4.1 metadata = %#v, want models.dev capabilities", model)
			}
			return
		}
	}
	t.Fatal("missing gpt-4.1")
}

func TestLiveModelAllowedWithoutCatalogChecksProviderGateFirst(t *testing.T) {
	// The ModelIDAllowedForProvider check runs before the others.
	// For the restricted provider (opencode-go-anthropic-compatible) a
	// non-allowed model returns false immediately, without reaching the
	// IsKnownNonCodingModelID, Local, or LooksLikeCodingModelID checks.
	restricted := providercatalog.Descriptor{
		ID:    "opencode-go-anthropic-compatible",
		Local: true, // would pass the Local check if we got past the gate
	}

	// A model that isn't qwen/minimax is blocked at the gate, even though
	// Local=true would let any model through on its own.
	if got := liveModelAllowedWithoutCatalog(restricted, "claude-sonnet-4"); got != false {
		t.Fatal("liveModelAllowedWithoutCatalog: want false for claude-sonnet-4 on opencode-go-anthropic-compatible (blocked by ModelIDAllowedForProvider)")
	}

	// A qwen model passes the gate and continues to the remaining checks;
	// it's not a known non-coding model and looks like a coding model, so
	// the result is true.
	if got := liveModelAllowedWithoutCatalog(restricted, "qwen-max"); got != true {
		t.Fatal("liveModelAllowedWithoutCatalog: want true for qwen-max on opencode-go-anthropic-compatible (passes all checks)")
	}

	// A minimax model also passes the gate.
	if got := liveModelAllowedWithoutCatalog(restricted, "minimax-text-01"); got != true {
		t.Fatal("liveModelAllowedWithoutCatalog: want true for minimax-text-01 on opencode-go-anthropic-compatible (passes all checks)")
	}

	// Unrestricted provider: all models pass the gate, so the other checks
	// decide the result. claude-sonnet-4 looks like a coding model → true.
	openAI := providercatalog.Descriptor{ID: "openai"}
	if got := liveModelAllowedWithoutCatalog(openAI, "claude-sonnet-4"); got != true {
		t.Fatal("liveModelAllowedWithoutCatalog: want true for claude-sonnet-4 on openai (unrestricted)")
	}

	// Non-coding model still filtered on an unrestricted provider.
	if got := liveModelAllowedWithoutCatalog(openAI, "text-embedding-3-large"); got != false {
		t.Fatal("liveModelAllowedWithoutCatalog: want false for embedding model on openai")
	}
}

func modelIDs(models []Model) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids
}

// TestDiscoverOllamaContextWindowFetchesFromNativeShowEndpoint: the generic
// /v1/models probe never carries context-window metadata (parseModelsResponse
// only extracts id/description), so a custom/local Ollama model tag with no
// curated-catalog match has no other source for it. This exercises the
// Ollama-native /api/show fallback that fills that gap.
func TestDiscoverOllamaContextWindowFetchesFromNativeShowEndpoint(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model_info": {
				"general.architecture": "qwen2",
				"qwen2.context_length": 131072
			}
		}`))
	}))
	defer server.Close()

	window, err := DiscoverOllamaContextWindow(context.Background(), server.URL+"/v1", "kimi-k2.7-code:cloud", Options{HTTPClient: server.Client()})
	if err != nil {
		t.Fatalf("DiscoverOllamaContextWindow returned error: %v", err)
	}
	if window != 131072 {
		t.Fatalf("context window = %d, want 131072", window)
	}
	if gotPath != "/api/show" {
		t.Fatalf("requested path = %q, want /api/show (not under /v1)", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST", gotMethod)
	}
	if !strings.Contains(gotBody, `"kimi-k2.7-code:cloud"`) {
		t.Fatalf("request body = %q, want it to name the model", gotBody)
	}
}

func TestDiscoverOllamaContextWindowRequiresModelName(t *testing.T) {
	if _, err := DiscoverOllamaContextWindow(context.Background(), "http://localhost:11434/v1", "", Options{}); err == nil {
		t.Fatal("expected an error for an empty model name")
	}
}

func TestDiscoverOllamaContextWindowErrorsWhenShowOmitsContextLength(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model_info": {"general.architecture": "qwen2"}}`))
	}))
	defer server.Close()

	if _, err := DiscoverOllamaContextWindow(context.Background(), server.URL+"/v1", "some-model", Options{HTTPClient: server.Client()}); err == nil {
		t.Fatal("expected an error when no *.context_length key is present")
	}
}
