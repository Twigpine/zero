package provideronboarding

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLocalRuntimeCandidatesCoverOllamaLMStudioAndAtomicChat(t *testing.T) {
	candidates := LocalRuntimeCandidates()
	if len(candidates) == 0 {
		t.Fatalf("LocalRuntimeCandidates() returned no candidates")
	}
	byCatalog := map[string]LocalRuntime{}
	for _, candidate := range candidates {
		byCatalog[candidate.CatalogID] = candidate
	}
	ollama, ok := byCatalog["ollama"]
	if !ok {
		t.Fatalf("expected an ollama candidate, got %#v", candidates)
	}
	if !strings.Contains(ollama.BaseURL, "11434") {
		t.Fatalf("ollama candidate must probe default port 11434, got %q", ollama.BaseURL)
	}
	if ollama.RequiresKey {
		t.Fatalf("ollama candidate must not require an API key: %#v", ollama)
	}
	lmstudio, ok := byCatalog["lmstudio"]
	if !ok {
		t.Fatalf("expected an lmstudio candidate, got %#v", candidates)
	}
	if !strings.Contains(lmstudio.BaseURL, "1234") {
		t.Fatalf("lmstudio candidate must probe default port 1234, got %q", lmstudio.BaseURL)
	}
	if lmstudio.RequiresKey {
		t.Fatalf("lmstudio candidate must not require an API key: %#v", lmstudio)
	}
	atomicChat, ok := byCatalog["atomic-chat-local"]
	if !ok {
		t.Fatalf("expected an atomic-chat-local candidate, got %#v", candidates)
	}
	if atomicChat.BaseURL != "http://127.0.0.1:1337/v1" {
		t.Fatalf("atomic-chat-local candidate BaseURL = %q, want http://127.0.0.1:1337/v1", atomicChat.BaseURL)
	}
	if atomicChat.DefaultModel != "local-model" {
		t.Fatalf("atomic-chat-local candidate DefaultModel = %q, want local-model", atomicChat.DefaultModel)
	}
	if atomicChat.RequiresKey {
		t.Fatalf("atomic-chat-local candidate must not require an API key: %#v", atomicChat)
	}
	// The hosted atomic-chat preset stays remote and key-gated, so it must never
	// be probed as a local runtime.
	if _, ok := byCatalog["atomic-chat"]; ok {
		t.Fatalf("hosted atomic-chat must not be a local-runtime candidate, got %#v", candidates)
	}
}

func TestDetectLocalRuntimesReportsReachableRuntime(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"llama3.1"}]}`))
	}))
	defer server.Close()

	detected := DetectLocalRuntimes(context.Background(), LocalDetectOptions{
		HTTPClient: server.Client(),
		Candidates: []LocalRuntime{{
			CatalogID: "ollama",
			Name:      "Ollama Local",
			BaseURL:   server.URL + "/v1",
		}},
	})
	if len(detected) != 1 {
		t.Fatalf("DetectLocalRuntimes() = %#v, want one reachable runtime", detected)
	}
	if !detected[0].Reachable {
		t.Fatalf("runtime should be reachable: %#v", detected[0])
	}
	if len(detected[0].Models) == 0 || detected[0].Models[0] != "llama3.1" {
		t.Fatalf("expected discovered model list, got %#v", detected[0].Models)
	}
}

// A local runtime serves whichever model the user loaded, so the adopt command
// must pin the id the probe saw. Atomic Chat pulls its catalog from Hugging
// Face, so the served id is an arbitrary repo id and never the "local-model"
// placeholder the catalog carries as DefaultModel.
func TestSetupActionPinsProbedModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"unsloth/Qwen3-Coder-30B-A3B-Instruct-GGUF"}]}`))
	}))
	defer server.Close()

	detected := DetectLocalRuntimes(context.Background(), LocalDetectOptions{
		HTTPClient: server.Client(),
		Candidates: []LocalRuntime{{
			CatalogID:    "atomic-chat-local",
			Name:         "Atomic Chat Local",
			BaseURL:      server.URL + "/v1",
			DefaultModel: "local-model",
		}},
	})
	if len(detected) != 1 {
		t.Fatalf("DetectLocalRuntimes() = %#v, want one reachable runtime", detected)
	}
	if got := detected[0].AdoptModel(); got != "unsloth/Qwen3-Coder-30B-A3B-Instruct-GGUF" {
		t.Fatalf("AdoptModel() = %q, want the probed id, not the catalog placeholder", got)
	}
	action := detected[0].SetupAction()
	if !strings.Contains(action.Command, "--model unsloth/Qwen3-Coder-30B-A3B-Instruct-GGUF") {
		t.Fatalf("SetupAction command must pin the probed model, got %q", action.Command)
	}
	if strings.Contains(action.Command, "local-model") {
		t.Fatalf("SetupAction command must not persist the catalog placeholder, got %q", action.Command)
	}
}

// With no model IDs, Atomic Chat needs guidance instead of a failing command.
func TestSetupActionOmitsModelWhenProbeFoundNone(t *testing.T) {
	runtime := DetectedLocalRuntime{
		LocalRuntime: LocalRuntime{CatalogID: "atomic-chat-local", Name: "Atomic Chat Local", BaseURL: "http://127.0.0.1:1337/v1", DefaultModel: "local-model"},
		Reachable:    true,
	}
	if got := runtime.AdoptModel(); got != "" {
		t.Fatalf("AdoptModel() = %q, want empty", got)
	}
	action := runtime.SetupAction()
	if strings.Contains(action.Command, "providers add") {
		t.Fatalf("atomic-chat-local with no served model must not advertise a bare add command, got %q", action.Command)
	}
}

func TestSetupActionPreservesModelsWithSpaces(t *testing.T) {
	runtime := DetectedLocalRuntime{
		LocalRuntime: LocalRuntime{CatalogID: "atomic-chat-local", Name: "Atomic Chat Local"},
		Models:       []string{"my loaded model"},
	}
	if got := runtime.AdoptModel(); got != "my loaded model" {
		t.Fatalf("AdoptModel() = %q", got)
	}
	if got := runtime.SetupAction().Command; !strings.Contains(got, `--model "my loaded model"`) {
		t.Fatalf("model with spaces not preserved: %q", got)
	}
}

func TestSetupActionOmitsUnsafeCommands(t *testing.T) {
	for _, id := range []string{"$(touch pwned)", "`id`", "x&calc", "a \" & calc & \"b", "a;b", "a|b", "a'b", "a>b", "%PATH%", "!PATH!", "@args", "line\ncommand", "a\u201db"} {
		t.Run(id, func(t *testing.T) {
			runtime := DetectedLocalRuntime{LocalRuntime: LocalRuntime{CatalogID: "atomic-chat-local", Name: "Atomic Chat Local"}, Models: []string{id, "safe-model"}}
			if runtime.AdoptModel() != id {
				t.Fatal("discovered model must not be silently changed")
			}
			action := runtime.SetupAction()
			if action.Command != "" || !strings.Contains(action.Detail, "zero setup") {
				t.Fatalf("unsafe command must be replaced with interactive guidance: %#v", action)
			}
		})
	}
}

func TestSetupActionRejectsAtomicPlaceholder(t *testing.T) {
	runtime := DetectedLocalRuntime{LocalRuntime: LocalRuntime{CatalogID: "atomic-chat-local", DefaultModel: "local-model"}, Models: []string{"local-model"}}
	if runtime.AdoptModel() != "" || runtime.SetupAction().Command != "" {
		t.Fatal("placeholder must not be advertised as adoptable")
	}
	runtime.Models = append(runtime.Models, "loaded/model")
	if runtime.AdoptModel() != "loaded/model" {
		t.Fatal("real model must be selected instead of placeholder")
	}
}

// A server that advertises the catalog default alongside other ids keeps the
// default, so an existing Ollama setup does not silently switch models.
func TestAdoptModelPrefersCatalogDefaultWhenServed(t *testing.T) {
	runtime := DetectedLocalRuntime{
		LocalRuntime: LocalRuntime{CatalogID: "ollama", Name: "Ollama Local", DefaultModel: "llama3.1"},
		Reachable:    true,
		Models:       []string{"qwen3:8b", "llama3.1"},
	}
	if got := runtime.AdoptModel(); got != "llama3.1" {
		t.Fatalf("AdoptModel() = %q, want the catalog default when the server serves it", got)
	}
}

func TestAdoptModelChatEligibilityAndOllamaDefault(t *testing.T) {
	tests := []struct {
		name, provider, defaultModel string
		models                       []string
		want                         string
	}{
		{"skip embedding", "atomic-chat-local", "local-model", []string{"text-embedding-local", "qwen3-coder-30b"}, "qwen3-coder-30b"},
		{"reject nonchat default", "lmstudio", "text-embedding-local", []string{"text-embedding-local", "chat-model"}, "chat-model"},
		{"latest alias", "ollama", "llama3.1", []string{"qwen3:8b", "llama3.1:latest"}, "llama3.1:latest"},
		{"exact before alias", "ollama", "llama3.1", []string{"llama3.1:latest", "llama3.1"}, "llama3.1"},
		{"absent default", "ollama", "llama3.1", []string{"text-embedding-local", "qwen3:8b", "other-chat"}, "qwen3:8b"},
		{"different tag", "ollama", "llama3.1", []string{"qwen3:8b", "llama3.1:custom"}, "qwen3:8b"},
		{"explicit default tag", "ollama", "llama3.1:custom", []string{"qwen3:8b", "llama3.1:latest"}, "qwen3:8b"},
		{"other runtime", "lmstudio", "llama3.1", []string{"qwen3:8b", "llama3.1:latest"}, "qwen3:8b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			detected := DetectedLocalRuntime{LocalRuntime: LocalRuntime{CatalogID: tt.provider, DefaultModel: tt.defaultModel}, Models: tt.models}
			if got := detected.AdoptModel(); got != tt.want {
				t.Fatalf("AdoptModel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSetupActionOmitsKnownNonChatOnlyModels(t *testing.T) {
	for _, provider := range []string{"atomic-chat-local", "lmstudio", "ollama"} {
		for _, defaultModel := range []string{"local-model", "text-embedding-local"} {
			detected := DetectedLocalRuntime{LocalRuntime: LocalRuntime{CatalogID: provider, DefaultModel: defaultModel}, Models: []string{"text-embedding-local", "whisper-1"}}
			action := detected.SetupAction()
			if detected.AdoptModel() != "" || action.Command != "" || !strings.Contains(action.Detail, "zero providers detect") {
				t.Fatalf("%s must offer guidance without adopting non-chat models: %+v", provider, action)
			}
		}
	}
}

func TestDetectLocalRuntimesSkipsUnreachableRuntime(t *testing.T) {
	// A client whose transport always fails simulates a closed local port.
	failing := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})}

	detected := DetectLocalRuntimes(context.Background(), LocalDetectOptions{
		HTTPClient: failing,
		Candidates: []LocalRuntime{{
			CatalogID: "ollama",
			Name:      "Ollama Local",
			BaseURL:   "http://127.0.0.1:11434/v1",
		}},
	})
	if len(detected) != 0 {
		t.Fatalf("DetectLocalRuntimes() = %#v, want no reachable runtimes", detected)
	}
}

func TestDetectLocalRuntimesIgnoresServerErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	detected := DetectLocalRuntimes(context.Background(), LocalDetectOptions{
		HTTPClient: server.Client(),
		Candidates: []LocalRuntime{{
			CatalogID: "lmstudio",
			Name:      "LM Studio",
			BaseURL:   server.URL + "/v1",
		}},
	})
	if len(detected) != 0 {
		t.Fatalf("a 5xx local response must not count as a reachable runtime: %#v", detected)
	}
}

func TestLocalRuntimeActionOffersNoKeySetup(t *testing.T) {
	runtime := DetectedLocalRuntime{LocalRuntime: LocalRuntime{
		CatalogID: "ollama",
		Name:      "Ollama Local",
		BaseURL:   "http://localhost:11434/v1",
	}, Reachable: true}

	action := runtime.SetupAction()
	if !strings.Contains(action.Command, "zero providers add ollama") {
		t.Fatalf("setup command should add the ollama provider, got %q", action.Command)
	}
	if strings.Contains(action.Command, "--api-key-env") {
		t.Fatalf("local runtime setup must not require an API key env, got %q", action.Command)
	}
	if !strings.Contains(strings.ToLower(action.Detail), "no api key") {
		t.Fatalf("setup detail should advertise the no-key path, got %q", action.Detail)
	}
}

func TestDetectLocalRuntimesAppliesDefaultTimeout(t *testing.T) {
	// A handler that blocks past the configured timeout must be treated as
	// unreachable rather than hanging the wizard.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	detected := DetectLocalRuntimes(context.Background(), LocalDetectOptions{
		HTTPClient: server.Client(),
		Timeout:    20 * time.Millisecond,
		Candidates: []LocalRuntime{{
			CatalogID: "ollama",
			Name:      "Ollama Local",
			BaseURL:   server.URL + "/v1",
		}},
	})
	if len(detected) != 0 {
		t.Fatalf("a runtime slower than the timeout must be skipped: %#v", detected)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestAtomicDetectionWithoutUsableModelsOmitsAdoption(t *testing.T) {
	for _, body := range []string{`{"data":[]}`, `not json`, strings.Repeat(" ", 256*1024) + `{"data":[{"id":"real"}]}`, `{"data":[{"id":"local-model"}]}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
		detected := DetectLocalRuntimes(context.Background(), LocalDetectOptions{HTTPClient: server.Client(), Candidates: []LocalRuntime{{CatalogID: "atomic-chat-local", BaseURL: server.URL + "/v1", DefaultModel: "local-model"}}})
		server.Close()
		if len(detected) != 1 || detected[0].SetupAction().Command != "" || detected[0].SetupAction().Detail == "" {
			t.Fatalf("model-less detection advertised a failing action: %+v", detected)
		}
	}
}
