package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/oauth"
	"github.com/Gitlawb/zero/internal/providers/providerio"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

func TestNewCreatesOpenAIProviderWithFactoryOptions(t *testing.T) {
	transport := &captureTransport{
		responseBody: "data: [DONE]\n\n",
	}
	client := &http.Client{Transport: transport}

	provider, err := New(config.ProviderProfile{
		Name:         "custom",
		ProviderKind: config.ProviderKindOpenAICompatible,
		BaseURL:      "https://provider.example/v1/",
		APIKey:       "sk-factory",
		Model:        "factory-model",
	}, Options{
		HTTPClient: client,
		UserAgent:  "zero-factory-test",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("StreamCompletion() error = %v", err)
	}
	for range stream {
	}

	if transport.request == nil {
		t.Fatal("HTTP client was not used")
	}
	if transport.request.URL.String() != "https://provider.example/v1/chat/completions" {
		t.Fatalf("request URL = %q, want provider base URL", transport.request.URL.String())
	}
	if transport.request.Header.Get("Authorization") != "Bearer sk-factory" {
		t.Fatalf("Authorization = %q, want bearer token", transport.request.Header.Get("Authorization"))
	}
	if transport.request.Header.Get("User-Agent") != "zero-factory-test" {
		t.Fatalf("User-Agent = %q, want factory user agent", transport.request.Header.Get("User-Agent"))
	}
}

func TestNewUsesMiniMaxCompatibleEndpoints(t *testing.T) {
	tests := []struct {
		name         string
		catalogID    string
		providerKind config.ProviderKind
		baseURL      string
		responseBody string
		wantURL      string
	}{
		{
			name:         "global Anthropic",
			catalogID:    "minimax",
			providerKind: config.ProviderKindAnthropicCompat,
			responseBody: "data: {\"type\":\"message_stop\"}\n\n",
			wantURL:      "https://api.minimax.io/anthropic/v1/messages",
		},
		{
			name:         "China Anthropic",
			catalogID:    "minimaxi-cn",
			providerKind: config.ProviderKindAnthropicCompat,
			responseBody: "data: {\"type\":\"message_stop\"}\n\n",
			wantURL:      "https://api.minimaxi.com/anthropic/v1/messages",
		},
		{
			name:         "global OpenAI",
			catalogID:    "custom-openai-compatible",
			providerKind: config.ProviderKindOpenAICompatible,
			baseURL:      "https://api.minimax.io/v1",
			responseBody: "data: [DONE]\n\n",
			wantURL:      "https://api.minimax.io/v1/chat/completions",
		},
		{
			name:         "China OpenAI",
			catalogID:    "custom-openai-compatible",
			providerKind: config.ProviderKindOpenAICompatible,
			baseURL:      "https://api.minimaxi.com/v1",
			responseBody: "data: [DONE]\n\n",
			wantURL:      "https://api.minimaxi.com/v1/chat/completions",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport := &captureTransport{responseBody: test.responseBody}
			provider, err := New(config.ProviderProfile{
				Name:         test.name,
				CatalogID:    test.catalogID,
				ProviderKind: test.providerKind,
				BaseURL:      test.baseURL,
				APIKey:       "sk-minimax",
				Model:        "MiniMax-M3",
			}, Options{HTTPClient: &http.Client{Transport: transport}})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
				Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hello"}},
			})
			if err != nil {
				t.Fatalf("StreamCompletion() error = %v", err)
			}
			for range stream {
			}

			if transport.request == nil {
				t.Fatal("HTTP client was not used")
			}
			if got := transport.request.URL.String(); got != test.wantURL {
				t.Fatalf("request URL = %q, want %q", got, test.wantURL)
			}
		})
	}
}

func TestNewPassesOpenGatewayHY3ModelThrough(t *testing.T) {
	transport := &captureTransport{
		responseBody: "data: [DONE]\n\n",
	}
	client := &http.Client{Transport: transport}

	provider, err := New(config.ProviderProfile{
		Name:         "opengateway",
		CatalogID:    "gitlawb-opengateway",
		ProviderKind: config.ProviderKindOpenAICompatible,
		APIKey:       "ogw_live_test",
		Model:        "tencent/hy3",
	}, Options{HTTPClient: client})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("StreamCompletion() error = %v", err)
	}
	for range stream {
	}

	if transport.request.URL.String() != "https://opengateway.gitlawb.com/v1/chat/completions" {
		t.Fatalf("request URL = %q, want OpenGateway chat completions", transport.request.URL.String())
	}
	var body map[string]any
	if err := json.NewDecoder(transport.body()).Decode(&body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if body["model"] != "tencent/hy3" {
		t.Fatalf("model = %q, want tencent/hy3 passthrough", body["model"])
	}

	metadata, err := ResolveRuntimeMetadata(config.ProviderProfile{
		Name:         "opengateway",
		CatalogID:    "gitlawb-opengateway",
		ProviderKind: config.ProviderKindOpenAICompatible,
		Model:        "tencent/hy3",
	}, Options{})
	if err != nil {
		t.Fatalf("ResolveRuntimeMetadata() error = %v", err)
	}
	if metadata.ProviderKind != config.ProviderKindOpenAICompatible || metadata.APIModel != "tencent/hy3" {
		t.Fatalf("runtime metadata = %#v, want OpenAI-compatible tencent/hy3", metadata)
	}
}

func TestNewThreadsCustomProviderHeaders(t *testing.T) {
	transport := &captureTransport{
		responseBody: "data: [DONE]\n\n",
	}
	client := &http.Client{Transport: transport}

	provider, err := New(config.ProviderProfile{
		Name:          "gateway",
		ProviderKind:  config.ProviderKindOpenAICompatible,
		BaseURL:       "https://gateway.example/v1",
		APIKey:        "sk-gateway",
		AuthHeader:    "X-API-Key",
		AuthScheme:    "Token",
		CustomHeaders: map[string]string{"HTTP-Referer": "https://zero.dev"},
		Model:         "gateway-model",
	}, Options{HTTPClient: client})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("StreamCompletion() error = %v", err)
	}
	for range stream {
	}

	if transport.request.Header.Get("Authorization") != "" {
		t.Fatalf("Authorization = %q, want empty when custom auth header is set", transport.request.Header.Get("Authorization"))
	}
	if transport.request.Header.Get("X-API-Key") != "Token sk-gateway" {
		t.Fatalf("X-API-Key = %q, want custom auth header", transport.request.Header.Get("X-API-Key"))
	}
	if transport.request.Header.Get("HTTP-Referer") != "https://zero.dev" {
		t.Fatalf("HTTP-Referer = %q, want custom provider header", transport.request.Header.Get("HTTP-Referer"))
	}
}

func TestNewAIMLAPIProviderSendsEndpointAndAuthWithoutAttribution(t *testing.T) {
	transport := &captureTransport{responseBody: "data: [DONE]\n\n"}
	provider, err := New(config.ProviderProfile{
		Name:          "aimlapi",
		CatalogID:     "aimlapi",
		ProviderKind:  config.ProviderKindOpenAICompatible,
		APIKey:        "aimlapi-test-key",
		Model:         "openai/gpt-5-chat",
		CustomHeaders: map[string]string{"X-Trace": "test"},
	}, Options{HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("StreamCompletion() error = %v", err)
	}
	for range stream {
	}

	if transport.request == nil {
		t.Fatal("HTTP client was not used")
	}
	if got := transport.request.URL.String(); got != "https://api.aimlapi.com/v1/chat/completions" {
		t.Fatalf("request URL = %q, want AI/ML API endpoint", got)
	}
	for header, want := range map[string]string{
		"Authorization": "Bearer aimlapi-test-key",
		"X-Trace":       "test",
	} {
		if got := transport.request.Header.Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
	// No first-party referral/attribution headers are injected for catalog
	// presets; aimlapi rides through CopyHeaders like every other provider.
	for _, header := range []string{
		"X-AIMLAPI-Partner-ID",
		"X-AIMLAPI-Integration-Repo",
		"X-AIMLAPI-Integration-Version",
	} {
		if got := transport.request.Header.Get(header); got != "" {
			t.Fatalf("%s = %q, want no attribution header", header, got)
		}
	}
}

func TestNewSupportsOpenAIProviderKind(t *testing.T) {
	provider, err := New(config.ProviderProfile{
		Name:         "openai",
		ProviderKind: config.ProviderKindOpenAI,
		Model:        "gpt-test",
	}, Options{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if provider == nil {
		t.Fatal("New() returned nil provider")
	}
}

// TestPromptCacheKeyOnlyOnOfficialOpenAI locks in #624: session-backed TUI
// turns always carry a PromptCacheKey, but openai-compatible gateways (NVIDIA
// NIM, strict local proxies) reject the OpenAI-only prompt_cache_key field.
// The factory must omit it for openai-compatible profiles while still
// forwarding it for official OpenAI so multi-turn cache routing stays intact.
func TestPromptCacheKeyOnlyOnOfficialOpenAI(t *testing.T) {
	requestWithSession := zeroruntime.CompletionRequest{
		Messages:       []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hello"}},
		PromptCacheKey: "sess_tui_123",
	}

	for _, tc := range []struct {
		name         string
		kind         config.ProviderKind
		wantCacheKey bool
	}{
		{name: "openai", kind: config.ProviderKindOpenAI, wantCacheKey: true},
		{name: "openai-compatible", kind: config.ProviderKindOpenAICompatible, wantCacheKey: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := &captureTransport{responseBody: "data: [DONE]\n\n"}
			provider, err := New(config.ProviderProfile{
				Name:         "test",
				ProviderKind: tc.kind,
				BaseURL:      "https://provider.example/v1",
				APIKey:       "sk-test",
				Model:        "test-model",
			}, Options{HTTPClient: &http.Client{Transport: transport}})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			stream, err := provider.StreamCompletion(context.Background(), requestWithSession)
			if err != nil {
				t.Fatalf("StreamCompletion() error = %v", err)
			}
			for range stream {
			}
			var body map[string]any
			if err := json.NewDecoder(transport.body()).Decode(&body); err != nil {
				t.Fatalf("decode request body: %v", err)
			}
			_, hasKey := body["prompt_cache_key"]
			if hasKey != tc.wantCacheKey {
				t.Fatalf("prompt_cache_key present = %v, want %v; body = %#v", hasKey, tc.wantCacheKey, body)
			}
			if tc.wantCacheKey && body["prompt_cache_key"] != "sess_tui_123" {
				t.Fatalf("prompt_cache_key = %#v, want sess_tui_123", body["prompt_cache_key"])
			}
		})
	}
}

func TestParseThinkTagsForProfileUsesConservativeDefaultsAndOverride(t *testing.T) {
	openAICompatible := resolvedProfile{providerKind: config.ProviderKindOpenAICompatible, apiModel: "qwen3-coder:480b"}
	if !parseThinkTagsForProfile(config.ProviderProfile{}, openAICompatible) {
		t.Fatal("qwen3 OpenAI-compatible model should parse inline think tags by default")
	}
	minimaxM27 := resolvedProfile{providerKind: config.ProviderKindOpenAICompatible, apiModel: "MiniMax-M2.7"}
	if !parseThinkTagsForProfile(config.ProviderProfile{}, minimaxM27) {
		t.Fatal("MiniMax-M2.7 OpenAI-compatible model should parse inline think tags by default")
	}

	generic := resolvedProfile{providerKind: config.ProviderKindOpenAICompatible, apiModel: "factory-model"}
	if parseThinkTagsForProfile(config.ProviderProfile{}, generic) {
		t.Fatal("generic OpenAI-compatible model should preserve literal think tags by default")
	}

	official := resolvedProfile{providerKind: config.ProviderKindOpenAI, apiModel: "gpt-4.1"}
	if parseThinkTagsForProfile(config.ProviderProfile{}, official) {
		t.Fatal("official OpenAI model should preserve literal think tags by default")
	}

	enabled := true
	if !parseThinkTagsForProfile(config.ProviderProfile{ParseThinkTags: &enabled}, generic) {
		t.Fatal("explicit parseThinkTags=true should enable inline think parsing")
	}

	disabled := false
	if parseThinkTagsForProfile(config.ProviderProfile{ParseThinkTags: &disabled}, openAICompatible) {
		t.Fatal("explicit parseThinkTags=false should disable inline think parsing")
	}
}

func TestNewResolvesKnownModelToAPIModelAndProvider(t *testing.T) {
	transport := &captureTransport{
		responseBody: "data: {\"type\":\"message_stop\"}\n\n",
	}
	client := &http.Client{Transport: transport}

	provider, err := New(config.ProviderProfile{
		Name:   "claude",
		APIKey: "sk-ant",
		Model:  "claude-sonnet-4.5",
	}, Options{
		HTTPClient: client,
		UserAgent:  "zero-factory-test",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("StreamCompletion() error = %v", err)
	}
	for range stream {
	}

	if transport.request == nil {
		t.Fatal("HTTP client was not used")
	}
	if transport.request.URL.String() != "https://api.anthropic.com/v1/messages" {
		t.Fatalf("request URL = %q, want Anthropic Messages API", transport.request.URL.String())
	}
	if transport.request.Header.Get("x-api-key") != "sk-ant" {
		t.Fatalf("x-api-key = %q, want Anthropic key", transport.request.Header.Get("x-api-key"))
	}
	if transport.request.Header.Get("User-Agent") != "zero-factory-test" {
		t.Fatalf("User-Agent = %q, want factory user agent", transport.request.Header.Get("User-Agent"))
	}
	var body map[string]any
	if err := json.NewDecoder(transport.body()).Decode(&body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if body["model"] != "claude-sonnet-4-5-20250929" {
		t.Fatalf("model = %q, want registry API model", body["model"])
	}
	if body["max_tokens"] != float64(64000) {
		t.Fatalf("max_tokens = %#v, want registry output ceiling", body["max_tokens"])
	}
}

func TestNewCreatesGeminiProviderFromFactoryOptions(t *testing.T) {
	transport := &captureTransport{
		responseBody: "data: {}\n\n",
	}
	client := &http.Client{Transport: transport}

	provider, err := New(config.ProviderProfile{
		Name:         "google",
		ProviderKind: config.ProviderKindGoogle,
		APIKey:       "sk-google",
		Model:        "gemini-2.5-flash",
	}, Options{
		HTTPClient: client,
		UserAgent:  "zero-factory-test",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("StreamCompletion() error = %v", err)
	}
	for range stream {
	}

	if transport.request == nil {
		t.Fatal("HTTP client was not used")
	}
	wantURL := "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse"
	if transport.request.URL.String() != wantURL {
		t.Fatalf("request URL = %q, want %s", transport.request.URL.String(), wantURL)
	}
	if transport.request.Header.Get("x-goog-api-key") != "sk-google" {
		t.Fatalf("x-goog-api-key = %q, want Google key", transport.request.Header.Get("x-goog-api-key"))
	}
	var body map[string]any
	if err := json.NewDecoder(transport.body()).Decode(&body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	generationConfig := body["generationConfig"].(map[string]any)
	if generationConfig["maxOutputTokens"] != float64(65536) {
		t.Fatalf("maxOutputTokens = %#v, want registry output ceiling", generationConfig["maxOutputTokens"])
	}
}

func TestNewRejectsMismatchedOfficialProviderAndKnownModel(t *testing.T) {
	_, err := New(config.ProviderProfile{
		Name:         "openai",
		ProviderKind: config.ProviderKindOpenAI,
		Model:        "claude-sonnet-4.5",
	}, Options{})
	if err == nil {
		t.Fatal("New() error = nil, want provider/model mismatch")
	}
	if !strings.Contains(err.Error(), "belongs to anthropic, not openai") {
		t.Fatalf("error = %q, want model/provider mismatch", err.Error())
	}
}

func TestNewRejectsUnsupportedProviderKind(t *testing.T) {
	_, err := New(config.ProviderProfile{
		Name:         "bad",
		ProviderKind: "bedrock",
		Model:        "model",
	}, Options{})
	if err == nil {
		t.Fatal("New() error = nil, want unsupported kind error")
	}
	if !strings.Contains(err.Error(), `unsupported provider kind "bedrock"`) {
		t.Fatalf("error = %q, want unsupported provider kind", err.Error())
	}
}

func TestNewRoutesChatGPTCatalogToCodexProvider(t *testing.T) {
	// Isolate the OAuth token store to an empty temp path so the factory reads no
	// stored login — otherwise this test picks up the developer's real chatgpt
	// OAuth token and the "want empty chatgpt-account-id" assertion fails locally
	// (it still passes in CI, where no login is stored). Mirrors the isolation in
	// TestNewRoutesChatGPTCatalogWithStoredAccountID.
	t.Setenv("ZERO_OAUTH_STORAGE", "file")
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", t.TempDir()+"/tokens.json")

	transport := &captureTransport{
		responseBody: "data: [DONE]\n\n",
	}
	client := &http.Client{Transport: transport}

	provider, err := New(config.ProviderProfile{
		Name:      "chatgpt",
		CatalogID: "chatgpt",
		Model:     "gpt-5",
	}, Options{
		HTTPClient: client,
		UserAgent:  "zero-factory-test",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("StreamCompletion() error = %v", err)
	}
	for range stream {
	}
	if transport.request == nil {
		t.Fatal("HTTP client was not used")
	}
	// The chatgpt catalog's baseURL is the Codex backend. The Codex
	// provider targets the Responses API at `{baseURL}/responses`, not
	// `/chat/completions` (a chat-completions body on this path would 404
	// or be misrouted by the Codex gateway).
	if !strings.HasSuffix(transport.request.URL.Path, "/responses") {
		t.Fatalf("request URL path = %q, want .../responses", transport.request.URL.Path)
	}
	wantHost := "chatgpt.com"
	if !strings.Contains(transport.request.URL.Host, wantHost) {
		t.Fatalf("request URL host = %q, want the Codex backend (chatgpt.com)", transport.request.URL.Host)
	}
	// The Codex-required headers must be present even when the OAuth token
	// has no account id (the AccountResolver returns ok=false in that case,
	// so the chatgpt-account-id header is just omitted, not wrongly set).
	if got := transport.request.Header.Get("originator"); got != "codex_cli_rs" {
		t.Fatalf("originator = %q, want codex_cli_rs", got)
	}
	if got := transport.request.Header.Get("chatgpt-account-id"); got != "" {
		t.Fatalf("chatgpt-account-id = %q, want empty when no OAuth login is stored", got)
	}
}

func TestNewRoutesChatGPTCatalogWithStoredAccountID(t *testing.T) {
	// The factory reads the stored OAuth token's Account field for the
	// chatgpt-account-id header, from the login key the CALLER supplies in
	// Options.OAuthLoginKey (the same key the bearer resolver bound). Seed a
	// token in an isolated temp store, then pass that key.
	store, err := newOAuthStoreForTest(t)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Save(oauth.ProviderKey("chatgpt"), oauth.Token{
		AccessToken: "tok-1",
		Account:     "acc-stored-42",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	transport := &captureTransport{
		responseBody: "data: [DONE]\n\n",
	}
	provider, err := New(config.ProviderProfile{
		Name:      "chatgpt",
		CatalogID: "chatgpt",
		Model:     "gpt-5",
	}, Options{
		HTTPClient:    &http.Client{Transport: transport},
		UserAgent:     "zero-factory-test",
		OAuthLoginKey: oauth.ProviderKey("chatgpt"),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("StreamCompletion() error = %v", err)
	}
	for range stream {
	}
	if got := transport.request.Header.Get("chatgpt-account-id"); got != "acc-stored-42" {
		t.Fatalf("chatgpt-account-id = %q, want acc-stored-42", got)
	}
}

func TestIsCodexCatalog(t *testing.T) {
	cases := []struct {
		catalogID string
		want      bool
	}{
		{"chatgpt", true},
		{"ChatGPT", true},
		{"openai", false},
		{"", false},
		{"chatgpt-proxy", false}, // the local proxy catalog stays on the openai path
	}
	for _, tc := range cases {
		got := isCodexCatalog(config.ProviderProfile{CatalogID: tc.catalogID}, resolvedProfile{})
		if got != tc.want {
			t.Errorf("isCodexCatalog(%q) = %v, want %v", tc.catalogID, got, tc.want)
		}
	}
}

type captureTransport struct {
	request      *http.Request
	requestBody  string
	responseBody string
}

func (transport *captureTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.request = request
	if request.Body != nil {
		body, _ := io.ReadAll(request.Body)
		transport.requestBody = string(body)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(transport.responseBody)),
		Request:    request,
	}, nil
}

func (transport *captureTransport) body() io.Reader {
	return strings.NewReader(transport.requestBody)
}

// newOAuthStoreForTest pins the OAuth token store to a plain temp FILE and
// returns a Store on it. Pinning ZERO_OAUTH_STORAGE matters as much as the
// path: an inherited "keyring" value would send NewStore to the OS keychain
// and ignore ZERO_OAUTH_TOKENS_PATH entirely, making the test read/write the
// developer's real logins. Exists so the chatgpt factory tests can seed a
// token without copying the path-handling dance from internal/cli.
func newOAuthStoreForTest(t *testing.T) (*oauth.Store, error) {
	t.Helper()
	t.Setenv("ZERO_OAUTH_STORAGE", "file")
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", t.TempDir()+"/tokens.json")
	return oauth.NewStore(oauth.StoreOptions{})
}

// codexAccountForKey reads the chatgpt-account-id from the token stored under a
// FIXED key — the key the caller (cli.oauthLoginForProfile) already bound for the
// bearer token, passed through providers.Options.OAuthLoginKey. The account is
// therefore always read from the same login that issued the bearer (no second,
// independent selection). It re-reads per call, so an in-place token refresh (new
// account claim under the SAME key) is picked up; an empty key (no OAuth login)
// or a missing/account-less token yields "".
func TestCodexAccountForKey(t *testing.T) {
	store, err := newOAuthStoreForTest(t)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Save(oauth.ProviderKey("chatgpt"), oauth.Token{AccessToken: "tok-1", Account: "acc-catalog-7"}); err != nil {
		t.Fatalf("Save chatgpt: %v", err)
	}
	// A different login WITHOUT an account claim, to prove the read stays on the
	// requested key and never crosses to another login's account.
	if err := store.Save(oauth.ProviderKey("codex"), oauth.Token{AccessToken: "tok-codex"}); err != nil {
		t.Fatalf("Save codex: %v", err)
	}

	if got := codexAccountForKey(oauth.ProviderKey("chatgpt")); got != "acc-catalog-7" {
		t.Fatalf("account = %q, want acc-catalog-7", got)
	}
	// The bound key's token has no account → "", NOT the other login's account.
	if got := codexAccountForKey(oauth.ProviderKey("codex")); got != "" {
		t.Fatalf("account = %q, want empty (must not cross to another login's account)", got)
	}
	// Empty key (no OAuth login) → header omitted.
	if got := codexAccountForKey(""); got != "" {
		t.Fatalf("account for empty key = %q, want empty", got)
	}
	// Unknown key → "".
	if got := codexAccountForKey(oauth.ProviderKey("nope")); got != "" {
		t.Fatalf("account for unknown key = %q, want empty", got)
	}

	// An in-place refresh under the bound key IS reflected per request.
	if err := store.Save(oauth.ProviderKey("chatgpt"), oauth.Token{AccessToken: "tok-refreshed", Account: "acc-rotated"}); err != nil {
		t.Fatalf("refresh chatgpt: %v", err)
	}
	if got := codexAccountForKey(oauth.ProviderKey("chatgpt")); got != "acc-rotated" {
		t.Fatalf("account = %q, want acc-rotated (in-place refresh must be picked up)", got)
	}
}

// TestNewRejectsModelOutsideProviderAllowlist guards the PR #544 P2 fix:
// opencode-go-anthropic-compatible only accepts Qwen and MiniMax model IDs,
// so a Claude model routed through this catalog-backed profile must be rejected
// at runtime, not silently forwarded to https://opencode.ai/zen/go/v1/messages.
func TestNewRejectsModelOutsideProviderAllowlist(t *testing.T) {
	_, err := New(config.ProviderProfile{
		Name:      "opencode-go-anthropic",
		CatalogID: "opencode-go-anthropic-compatible",
		APIKey:    "sk-test",
		Model:     "claude-sonnet-4.5",
	}, Options{})
	if err == nil {
		t.Fatal("New() error = nil, want claude-sonnet-4.5 rejected for opencode-go-anthropic-compatible")
	}
	if !strings.Contains(err.Error(), "opencode-go-anthropic-compatible") {
		t.Fatalf("error = %q, want it to name the provider", err.Error())
	}
	if !strings.Contains(err.Error(), "claude-sonnet-4.5") {
		t.Fatalf("error = %q, want it to name the rejected model", err.Error())
	}
}

// TestNewRejectsRawModelOutsideProviderAllowlist covers the registry-miss
// branch of resolveProfile: an unknown raw model id (not in the model registry)
// typed by the user into a restricted-provider profile must still be rejected
// rather than passthrough-sent.
func TestNewRejectsRawModelOutsideProviderAllowlist(t *testing.T) {
	_, err := New(config.ProviderProfile{
		Name:      "opencode-go-anthropic",
		CatalogID: "opencode-go-anthropic-compatible",
		APIKey:    "sk-test",
		Model:     "totally-custom-not-in-allowlist-12345",
	}, Options{})
	if err == nil {
		t.Fatal("New() error = nil, want unknown raw model rejected for opencode-go-anthropic-compatible")
	}
	if !strings.Contains(err.Error(), "does not allow model") {
		t.Fatalf("error = %q, want allowlist rejection", err.Error())
	}
	if !strings.Contains(err.Error(), "totally-custom-not-in-allowlist-12345") {
		t.Fatalf("error = %q, want the raw model name to appear in the error", err.Error())
	}
}

// TestNewAllowsCuratedModelsForRestrictedProvider proves the gate is not
// over-eager: Qwen and MiniMax curated models pass through unchanged for
// opencode-go-anthropic-compatible.
func TestNewAllowsCuratedModelsForRestrictedProvider(t *testing.T) {
	for _, model := range []string{"minimax-m3", "qwen3.7-plus", "qwen3.7-max", "minimax-m2.7"} {
		_, err := New(config.ProviderProfile{
			Name:      "opencode-go-anthropic",
			CatalogID: "opencode-go-anthropic-compatible",
			APIKey:    "sk-test",
			Model:     model,
		}, Options{})
		if err != nil {
			t.Fatalf("New() error = %v, want curated model %q to be allowed", err, model)
		}
	}
}

// TestNewAllowsAnyModelForUnrestrictedProvider is the regression guard:
// unrestricted providers (no catalog or default catalog id) must continue to
// accept any model id, including those outside the opencode allowlist.
func TestNewAllowsAnyModelForUnrestrictedProvider(t *testing.T) {
	_, err := New(config.ProviderProfile{
		Name:         "anthropic",
		ProviderKind: config.ProviderKindAnthropic,
		APIKey:       "sk-ant",
		Model:        "claude-sonnet-4.5",
	}, Options{})
	if err != nil {
		t.Fatalf("New() error = %v, want unrestricted anthropic provider to accept claude-sonnet-4.5", err)
	}
}

// TestResolveRuntimeMetadataRejectsModelOutsideProviderAllowlist guards the
// read-only metadata path (called from TUI command_center, exec, provider
// health, context report). The metadata path must enforce the same gate as
// New() so a config-time override cannot escape through it.
func TestResolveRuntimeMetadataRejectsModelOutsideProviderAllowlist(t *testing.T) {
	_, err := ResolveRuntimeMetadata(config.ProviderProfile{
		Name:      "opencode-go-anthropic",
		CatalogID: "opencode-go-anthropic-compatible",
		APIKey:    "sk-test",
		Model:     "claude-sonnet-4.5",
	}, Options{})
	if err == nil {
		t.Fatal("ResolveRuntimeMetadata error = nil, want claude-sonnet-4.5 rejected")
	}
	if !strings.Contains(err.Error(), "opencode-go-anthropic-compatible") {
		t.Fatalf("error = %q, want it to name the provider", err.Error())
	}
	if !strings.Contains(err.Error(), "claude-sonnet-4.5") {
		t.Fatalf("error = %q, want it to name the rejected model", err.Error())
	}
}

func TestOpenCodeSessionHeader(t *testing.T) {
	const sessionHeader = "x-opencode-session"
	tests := []struct {
		name       string
		profile    config.ProviderProfile
		response   string
		wantHeader bool
		wantAuth   map[string]string
	}{
		{
			name: "catalog opencode-go default URL",
			profile: config.ProviderProfile{
				Name:         "opencode-go",
				CatalogID:    "opencode-go",
				ProviderKind: config.ProviderKindOpenAICompatible,
				APIKey:       "sk-oc-go",
				Model:        "deepseek-v4-pro",
			},
			response:   "data: [DONE]\n\n",
			wantHeader: true,
			wantAuth:   map[string]string{"Authorization": "Bearer sk-oc-go"},
		},
		{
			name: "catalog opencode-go-anthropic-compatible",
			profile: config.ProviderProfile{
				Name:         "opencode-go-anthropic",
				CatalogID:    "opencode-go-anthropic-compatible",
				ProviderKind: config.ProviderKindAnthropicCompat,
				APIKey:       "sk-oc-ant",
				Model:        "minimax-m3",
			},
			response:   "data: {\"type\":\"message_stop\"}\n\n",
			wantHeader: true,
			wantAuth:   map[string]string{"x-api-key": "sk-oc-ant"},
		},
		{
			name: "catalog opencode Zen",
			profile: config.ProviderProfile{
				Name:         "opencode-zen",
				CatalogID:    "opencode",
				ProviderKind: config.ProviderKindOpenAICompatible,
				APIKey:       "sk-zen",
				Model:        "test-model",
			},
			response:   "data: [DONE]\n\n",
			wantHeader: true,
			wantAuth:   map[string]string{"Authorization": "Bearer sk-zen"},
		},
		{
			name: "renamed catalog profile with custom proxy URL",
			profile: config.ProviderProfile{
				Name:         "my-opencode",
				CatalogID:    "opencode-go",
				ProviderKind: config.ProviderKindOpenAICompatible,
				BaseURL:      "https://proxy.example/v1",
				APIKey:       "sk-proxy",
				Model:        "test-model",
			},
			response:   "data: [DONE]\n\n",
			wantHeader: true,
			wantAuth:   map[string]string{"Authorization": "Bearer sk-proxy"},
		},
		{
			name: "catalog-less OpenAI-compatible OpenCode Go URL",
			profile: config.ProviderProfile{
				Name:         "manual-go",
				ProviderKind: config.ProviderKindOpenAICompatible,
				BaseURL:      "https://opencode.ai/zen/go/v1",
				APIKey:       "sk-manual",
				Model:        "test-model",
			},
			response:   "data: [DONE]\n\n",
			wantHeader: true,
			wantAuth:   map[string]string{"Authorization": "Bearer sk-manual"},
		},
		{
			name: "catalog-less Anthropic-compatible OpenCode Go URL",
			profile: config.ProviderProfile{
				Name:         "manual-go-anthropic",
				ProviderKind: config.ProviderKindAnthropicCompat,
				BaseURL:      "https://opencode.ai/zen/go",
				APIKey:       "sk-manual-ant",
				Model:        "test-model",
			},
			response:   "data: {\"type\":\"message_stop\"}\n\n",
			wantHeader: true,
			wantAuth:   map[string]string{"x-api-key": "sk-manual-ant"},
		},
		{
			name: "official OpenAI",
			profile: config.ProviderProfile{
				Name:         "openai",
				ProviderKind: config.ProviderKindOpenAI,
				APIKey:       "sk-openai",
				Model:        "test-model",
			},
			response:   "data: [DONE]\n\n",
			wantHeader: false,
			wantAuth:   map[string]string{"Authorization": "Bearer sk-openai"},
		},
		{
			name: "generic OpenAI-compatible",
			profile: config.ProviderProfile{
				Name:         "generic",
				ProviderKind: config.ProviderKindOpenAICompatible,
				BaseURL:      "https://provider.example/v1",
				APIKey:       "sk-generic",
				Model:        "test-model",
			},
			response:   "data: [DONE]\n\n",
			wantHeader: false,
			wantAuth:   map[string]string{"Authorization": "Bearer sk-generic"},
		},
		{
			name: "official Anthropic",
			profile: config.ProviderProfile{
				Name:         "anthropic",
				ProviderKind: config.ProviderKindAnthropic,
				APIKey:       "sk-ant",
				Model:        "test-model",
			},
			response:   "data: {\"type\":\"message_stop\"}\n\n",
			wantHeader: false,
			wantAuth:   map[string]string{"x-api-key": "sk-ant"},
		},
		{
			name: "lookalike subdomain is not OpenCode",
			profile: config.ProviderProfile{
				Name:         "deceptive-host",
				ProviderKind: config.ProviderKindOpenAICompatible,
				BaseURL:      "https://opencode.ai.example/zen/go/v1",
				APIKey:       "sk-fake",
				Model:        "test-model",
			},
			response:   "data: [DONE]\n\n",
			wantHeader: false,
			wantAuth:   map[string]string{"Authorization": "Bearer sk-fake"},
		},
		{
			name: "lookalike path is not OpenCode",
			profile: config.ProviderProfile{
				Name:         "deceptive-path",
				ProviderKind: config.ProviderKindOpenAICompatible,
				BaseURL:      "https://example.com/opencode.ai/zen/go",
				APIKey:       "sk-fake",
				Model:        "test-model",
			},
			response:   "data: [DONE]\n\n",
			wantHeader: false,
			wantAuth:   map[string]string{"Authorization": "Bearer sk-fake"},
		},
		{
			name: "zenith path is not Zen",
			profile: config.ProviderProfile{
				Name:         "zenith",
				ProviderKind: config.ProviderKindOpenAICompatible,
				BaseURL:      "https://opencode.ai/zenith",
				APIKey:       "sk-zenith",
				Model:        "test-model",
			},
			response:   "data: [DONE]\n\n",
			wantHeader: false,
			wantAuth:   map[string]string{"Authorization": "Bearer sk-zenith"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport := &captureTransport{responseBody: test.response}
			provider, err := New(test.profile, Options{
				HTTPClient: &http.Client{Transport: transport},
				UserAgent:  "zero-test",
			})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
				Messages:       []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hello"}},
				PromptCacheKey: "conversation-a",
			})
			if err != nil {
				t.Fatalf("StreamCompletion() error = %v", err)
			}
			drainStream(t, stream)
			if transport.request == nil {
				t.Fatal("HTTP client was not used")
			}
			got := transport.request.Header.Get(sessionHeader)
			if test.wantHeader && got != "conversation-a" {
				t.Fatalf("%s = %q, want %q", sessionHeader, got, "conversation-a")
			}
			if !test.wantHeader && got != "" {
				t.Fatalf("%s = %q, want no session header", sessionHeader, got)
			}
			for header, want := range test.wantAuth {
				if got := transport.request.Header.Get(header); got != want {
					t.Fatalf("%s = %q, want %q", header, got, want)
				}
			}
			if got := transport.request.Header.Get("User-Agent"); got != "zero-test" {
				t.Fatalf("User-Agent = %q, want zero-test", got)
			}
			if test.profile.ProviderKind == config.ProviderKindOpenAICompatible {
				var body map[string]any
				if err := json.NewDecoder(transport.body()).Decode(&body); err != nil {
					t.Fatalf("decode request body: %v", err)
				}
				if _, ok := body["prompt_cache_key"]; ok {
					t.Fatalf("openai-compatible body carried prompt_cache_key: %#v", body["prompt_cache_key"])
				}
			}
		})
	}
}

func TestOpenCodeSessionHeaderFollowsConversationKey(t *testing.T) {
	transport := &recordingTransport{status: http.StatusOK, body: "data: [DONE]\n\n"}
	profile := config.ProviderProfile{
		Name:         "opencode-go",
		CatalogID:    "opencode-go",
		ProviderKind: config.ProviderKindOpenAICompatible,
		APIKey:       "sk-oc",
		Model:        "test-model",
	}
	options := Options{HTTPClient: &http.Client{Transport: transport}, UserAgent: "zero-test"}
	provider, err := New(profile, options)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	for _, key := range []string{"conversation-a", "conversation-a", "conversation-b"} {
		stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
			Messages:       []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hello"}},
			PromptCacheKey: key,
		})
		if err != nil {
			t.Fatalf("StreamCompletion() error = %v", err)
		}
		drainStream(t, stream)
	}
	got := transport.sessionHeaders()
	want := []string{"conversation-a", "conversation-a", "conversation-b"}
	if len(got) != len(want) {
		t.Fatalf("captured %d requests, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("request %d x-opencode-session = %q, want %q", i, got[i], want[i])
		}
	}

	reconstructed, err := New(profile, options)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	stream, err := reconstructed.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages:       []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hello"}},
		PromptCacheKey: "conversation-a",
	})
	if err != nil {
		t.Fatalf("StreamCompletion() error = %v", err)
	}
	drainStream(t, stream)
	got = transport.sessionHeaders()
	if last := got[len(got)-1]; last != "conversation-a" {
		t.Fatalf("reconstructed provider sent %s = %q, want conversation-a", "x-opencode-session", last)
	}
}

func TestOpenCodeSessionHeaderCustomOverride(t *testing.T) {
	custom := map[string]string{"X-Opencode-Session": "explicit-session"}
	profile := config.ProviderProfile{
		Name:          "opencode-go",
		CatalogID:     "opencode-go",
		ProviderKind:  config.ProviderKindOpenAICompatible,
		APIKey:        "sk-oc",
		Model:         "test-model",
		CustomHeaders: custom,
	}
	transport := &captureTransport{responseBody: "data: [DONE]\n\n"}
	provider, err := New(profile, Options{HTTPClient: &http.Client{Transport: transport}, UserAgent: "zero-test"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages:       []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hello"}},
		PromptCacheKey: "conversation-a",
	})
	if err != nil {
		t.Fatalf("StreamCompletion() error = %v", err)
	}
	drainStream(t, stream)
	if got := transport.request.Header.Get("x-opencode-session"); got != "explicit-session" {
		t.Fatalf("x-opencode-session = %q, want explicit custom value", got)
	}
	if len(custom) != 1 || custom["X-Opencode-Session"] != "explicit-session" {
		t.Fatalf("profile CustomHeaders mutated: %#v", custom)
	}
}

func TestOpenCodeSessionHeaderFallback(t *testing.T) {
	transport := &recordingTransport{status: http.StatusOK, body: "data: [DONE]\n\n"}
	profile := config.ProviderProfile{
		Name:         "opencode-go",
		CatalogID:    "opencode-go",
		ProviderKind: config.ProviderKindOpenAICompatible,
		APIKey:       "sk-oc",
		Model:        "test-model",
	}
	options := Options{HTTPClient: &http.Client{Transport: transport}}
	first, err := New(profile, options)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	second, err := New(profile, options)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	request := zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hello"}},
	}
	for _, provider := range []zeroruntime.Provider{first, first, second} {
		stream, err := provider.StreamCompletion(context.Background(), request)
		if err != nil {
			t.Fatalf("StreamCompletion() error = %v", err)
		}
		drainStream(t, stream)
	}
	headers := transport.sessionHeaders()
	if len(headers) != 3 {
		t.Fatalf("captured %d requests, want 3", len(headers))
	}
	if headers[0] == "" || headers[1] != headers[0] {
		t.Fatalf("fallback headers = %q, %q — want one stable nonempty value per provider", headers[0], headers[1])
	}
	if headers[2] == headers[0] {
		t.Fatalf("distinct provider instances shared fallback %q", headers[0])
	}
}

func TestOpenCodeSessionHeaderDefaultsUserAgent(t *testing.T) {
	transport := &captureTransport{responseBody: "data: [DONE]\n\n"}
	provider, err := New(config.ProviderProfile{
		Name:         "opencode-go",
		CatalogID:    "opencode-go",
		ProviderKind: config.ProviderKindOpenAICompatible,
		APIKey:       "sk-oc",
		Model:        "test-model",
	}, Options{HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("StreamCompletion() error = %v", err)
	}
	drainStream(t, stream)
	if got := transport.request.Header.Get("User-Agent"); got != "zero" {
		t.Fatalf("User-Agent = %q, want zero", got)
	}
}

func TestOpenCodeSessionHeaderConcurrent(t *testing.T) {
	transport := &recordingTransport{status: http.StatusOK, body: "data: [DONE]\n\n"}
	provider, err := New(config.ProviderProfile{
		Name:         "opencode-go",
		CatalogID:    "opencode-go",
		ProviderKind: config.ProviderKindOpenAICompatible,
		APIKey:       "sk-oc",
		Model:        "test-model",
	}, Options{HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	var wg sync.WaitGroup
	var errsMu sync.Mutex
	var errs []string
	for _, session := range []string{"session-a", "session-b"} {
		wg.Add(1)
		go func(session string) {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
					Messages:       []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: fmt.Sprintf("req-%s-%d", session, i)}},
					PromptCacheKey: session,
				})
				if err != nil {
					errsMu.Lock()
					errs = append(errs, err.Error())
					errsMu.Unlock()
					return
				}
				for event := range stream {
					if event.Type == zeroruntime.StreamEventError {
						errsMu.Lock()
						errs = append(errs, event.Error)
						errsMu.Unlock()
					}
				}
			}
		}(session)
	}
	wg.Wait()
	for _, errText := range errs {
		t.Errorf("concurrent stream error: %s", errText)
	}

	requests := transport.recorded()
	if len(requests) != 10 {
		t.Fatalf("captured %d requests, want 10", len(requests))
	}
	for _, request := range requests {
		var payload struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal([]byte(request.body), &payload); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if len(payload.Messages) == 0 {
			t.Fatal("request carried no user message")
		}
		content := payload.Messages[0].Content
		want := "session-a"
		if strings.Contains(content, "session-b") {
			want = "session-b"
		}
		if got := request.header.Get("x-opencode-session"); got != want {
			t.Fatalf("request for %q carried x-opencode-session %q, want %q", content, got, want)
		}
	}
}

func TestOpenCodeSessionHeaderAuthRetry(t *testing.T) {
	for _, test := range []struct {
		name     string
		profile  config.ProviderProfile
		response string
	}{
		{
			name: "openai-compatible",
			profile: config.ProviderProfile{
				Name:         "opencode-go",
				CatalogID:    "opencode-go",
				ProviderKind: config.ProviderKindOpenAICompatible,
				APIKey:       "sk-oc",
				Model:        "test-model",
			},
			response: "data: [DONE]\n\n",
		},
		{
			name: "anthropic-compatible",
			profile: config.ProviderProfile{
				Name:         "opencode-go-anthropic",
				CatalogID:    "opencode-go-anthropic-compatible",
				ProviderKind: config.ProviderKindAnthropicCompat,
				APIKey:       "sk-oc-ant",
				Model:        "minimax-m3",
			},
			response: "data: {\"type\":\"message_stop\"}\n\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			transport := &recordingTransport{status: http.StatusOK, body: test.response, unauthorized: 1}
			var calls atomic.Int32
			resolver := providerio.TokenResolver(func(context.Context, bool) (string, string, bool, error) {
				n := calls.Add(1)
				return "Authorization", fmt.Sprintf("Bearer tok-%d", n), true, nil
			})
			provider, err := New(test.profile, Options{
				HTTPClient:    &http.Client{Transport: transport},
				OAuthResolver: resolver,
			})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
				Messages:       []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hello"}},
				PromptCacheKey: "conversation-a",
			})
			if err != nil {
				t.Fatalf("StreamCompletion() error = %v", err)
			}
			drainStream(t, stream)
			requests := transport.recorded()
			if len(requests) != 2 {
				t.Fatalf("captured %d requests, want 2 (401 + retry)", len(requests))
			}
			for i, request := range requests {
				if got := request.header.Get("x-opencode-session"); got != "conversation-a" {
					t.Fatalf("attempt %d x-opencode-session = %q, want conversation-a", i, got)
				}
			}
			if first, second := requests[0].header.Get("Authorization"), requests[1].header.Get("Authorization"); first == second {
				t.Fatalf("Authorization not refreshed across retry: %q", first)
			}
		})
	}
}

func drainStream(t *testing.T, stream <-chan zeroruntime.StreamEvent) {
	t.Helper()
	for event := range stream {
		if event.Type == zeroruntime.StreamEventError {
			t.Fatalf("stream error: %s", event.Error)
		}
	}
}

type recordedRequest struct {
	header http.Header
	body   string
}

type recordingTransport struct {
	mu           sync.Mutex
	requests     []recordedRequest
	status       int
	body         string
	unauthorized int
}

func (transport *recordingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	var body string
	if request.Body != nil {
		data, _ := io.ReadAll(request.Body)
		body = string(data)
	}
	transport.mu.Lock()
	transport.requests = append(transport.requests, recordedRequest{header: request.Header.Clone(), body: body})
	attempt := len(transport.requests)
	transport.mu.Unlock()

	status := transport.status
	if attempt <= transport.unauthorized {
		status = http.StatusUnauthorized
	}
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d", status),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(transport.body)),
		Request:    request,
	}, nil
}

func (transport *recordingTransport) recorded() []recordedRequest {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return append([]recordedRequest(nil), transport.requests...)
}

func (transport *recordingTransport) sessionHeaders() []string {
	requests := transport.recorded()
	headers := make([]string, 0, len(requests))
	for _, request := range requests {
		headers = append(headers, request.header.Get("x-opencode-session"))
	}
	return headers
}
