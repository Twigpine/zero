package provideronboarding

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/providercatalog"
	"github.com/Gitlawb/zero/internal/providermodelcatalog"
)

// LocalRuntime describes a local, OpenAI-compatible model server that ZERO can
// adopt with no API key. These are the runtimes the launch audience is most
// likely to already be running (Ollama, LM Studio), so the first-run wizard
// auto-detects them on their default ports and offers them with no key step.
type LocalRuntime struct {
	CatalogID    string
	Name         string
	BaseURL      string
	DefaultModel string
	RequiresKey  bool
}

// DetectedLocalRuntime is a LocalRuntime that has been probed. Reachable reports
// whether its default endpoint answered the probe; Models is the list of model
// ids the runtime advertised (best-effort, empty when the probe could not parse
// a model list).
type DetectedLocalRuntime struct {
	LocalRuntime
	Reachable bool
	Models    []string
}

// LocalDetectOptions configures DetectLocalRuntimes. All fields are optional;
// the zero value probes the built-in candidates with a short default timeout
// using http.DefaultClient. Tests inject HTTPClient and Candidates.
type LocalDetectOptions struct {
	HTTPClient *http.Client
	Timeout    time.Duration
	Candidates []LocalRuntime
}

const defaultLocalProbeTimeout = 400 * time.Millisecond

// LocalRuntimeCandidates returns the built-in local-runtime candidates derived
// from the provider catalog. Only catalog entries flagged Local are returned, so
// the default ports stay the single source of truth (catalog.go).
func LocalRuntimeCandidates() []LocalRuntime {
	candidates := make([]LocalRuntime, 0, 2)
	for _, descriptor := range providercatalog.All() {
		if !descriptor.Local {
			continue
		}
		candidates = append(candidates, LocalRuntime{
			CatalogID:    descriptor.ID,
			Name:         descriptor.Name,
			BaseURL:      descriptor.DefaultBaseURL,
			DefaultModel: descriptor.DefaultModel,
			RequiresKey:  descriptor.RequiresAuth,
		})
	}
	return candidates
}

// DetectLocalRuntimes probes each candidate's default endpoint and returns only
// the runtimes that answered successfully. A runtime is "reachable" when its
// OpenAI-compatible models endpoint returns a 2xx; transport errors (closed
// port), timeouts, and non-2xx responses are treated as "not running" and the
// candidate is dropped. Detection never errors — a machine with nothing running
// locally simply yields an empty slice.
func DetectLocalRuntimes(ctx context.Context, options LocalDetectOptions) []DetectedLocalRuntime {
	candidates := options.Candidates
	if len(candidates) == 0 {
		candidates = LocalRuntimeCandidates()
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = defaultLocalProbeTimeout
	}
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}

	detected := make([]DetectedLocalRuntime, 0, len(candidates))
	for _, candidate := range candidates {
		models, reachable := probeLocalRuntime(ctx, client, timeout, candidate)
		if !reachable {
			continue
		}
		detected = append(detected, DetectedLocalRuntime{
			LocalRuntime: candidate,
			Reachable:    true,
			Models:       models,
		})
	}
	return detected
}

// SetupAction returns the no-key onboarding action for a detected local runtime.
func (runtime DetectedLocalRuntime) SetupAction() Action {
	descriptor := providercatalog.Descriptor{ID: runtime.CatalogID, RequiresAuth: false}
	model := runtime.AdoptModel()
	name := strings.TrimSpace(runtime.Name)
	if name == "" {
		name = runtime.CatalogID
	}
	// Do not fall back to an unserved/default model when all advertised IDs
	// were rejected. Preserve other runtimes' existing empty-response behavior.
	if model == "" && (runtime.CatalogID == "atomic-chat-local" || len(runtime.Models) > 0) {
		return Action{
			Label:  "Load a chat model",
			Detail: "Detected " + name + " on " + runtime.BaseURL + " but no usable chat model ID was discovered. Load a chat model in " + name + ", then run zero providers detect again.",
		}
	}
	command := SetupCommandWithModel(descriptor, runtime.Name, model, true)
	if command == "" {
		return Action{
			Label:  "Use interactive setup",
			Detail: "A setup value cannot be safely included in a command for all supported shells. Run zero setup and select or enter the model there.",
		}
	}
	detail := "Detected " + name + " on " + runtime.BaseURL + " — no API key required."
	if model != "" {
		detail = "Detected " + name + " on " + runtime.BaseURL + " serving " + model + " — no API key required."
	}
	return Action{
		Label:   "Use local runtime",
		Command: command,
		Detail:  detail,
	}
}

// AdoptModel returns an advertised model eligible for automatic chat adoption,
// preferring the catalog default when served. Shell syntax is handled when
// rendering the command, so eligible IDs are not silently replaced for quoting.
func (runtime DetectedLocalRuntime) AdoptModel() string {
	want := strings.TrimSpace(runtime.DefaultModel)
	first := ""
	defaultAlias := ""
	for _, raw := range runtime.Models {
		id := strings.TrimSpace(raw)
		if id == "" || providermodelcatalog.IsKnownNonCodingModelID(id) || runtime.CatalogID == "atomic-chat-local" && id == "local-model" {
			continue
		}
		if id == want {
			return id
		}
		// Ollama's untagged default is equivalent to its :latest spelling.
		// Retain the advertised ID and prefer an exact match if one follows.
		if runtime.CatalogID == "ollama" && want != "" && !strings.Contains(want, ":") && id == want+":latest" {
			defaultAlias = id
		}
		if first == "" {
			first = id
		}
	}
	if defaultAlias != "" {
		return defaultAlias
	}
	return first
}

func probeLocalRuntime(ctx context.Context, client *http.Client, timeout time.Duration, candidate LocalRuntime) ([]string, bool) {
	endpoint := localModelsEndpoint(candidate.BaseURL)
	if endpoint == "" {
		return nil, false
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, false
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, false
	}
	defer func() {
		_ = response.Body.Close()
	}()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, false
	}
	return decodeLocalModelIDs(response.Body), true
}

// decodeLocalModelIDs best-effort parses the OpenAI-compatible models list
// ({"data":[{"id":"..."}]}). The body is bounded so a misbehaving local server
// cannot stream unbounded data into the wizard. A parse failure is not fatal:
// the runtime is still reachable, we just return no model ids.
func decodeLocalModelIDs(body io.Reader) []string {
	data, err := io.ReadAll(io.LimitReader(body, 256*1024))
	if err != nil {
		return nil
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil
	}
	ids := make([]string, 0, len(parsed.Data))
	seen := map[string]bool{}
	for _, entry := range parsed.Data {
		id := strings.TrimSpace(entry.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil
	}
	return ids
}

// localModelsEndpoint resolves the OpenAI-compatible "list models" path for a
// base URL. Local base URLs in the catalog already end in /v1, so the endpoint
// is baseURL + "/models"; a trailing slash is trimmed so we never emit "//".
func localModelsEndpoint(baseURL string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmed == "" {
		return ""
	}
	return trimmed + "/models"
}
