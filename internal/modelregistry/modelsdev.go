package modelregistry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Live models.dev overlay for the curated catalog. The hand-maintained
// DefaultModelEntries list is the source of truth for identity — ids, aliases,
// match patterns, deprecations, escalation targets — but its VOLATILE facts
// (context window, max output tokens, per-million pricing) go stale between
// releases. When a cached snapshot of https://models.dev/api.json is present,
// those fields are refreshed from it at registry construction; everything else
// stays curated. The overlay never adds models (an auto-added entry would lack
// aliases, match patterns, and provider wiring) and never touches the network
// on the registry hot path — fetching happens only in the explicit background
// refresh, cached to disk with a TTL.

const (
	modelsDevDefaultURL = "https://models.dev/api.json"
	// modelsDevRefreshAfter is how old the cache may get before a background
	// refresh re-fetches it.
	modelsDevRefreshAfter = 24 * time.Hour
	// modelsDevMaxAge is the oldest cache still applied as an overlay. Beyond
	// this the curated catalog (updated with the binary) is likely fresher than
	// the snapshot, so a stale file is ignored rather than trusted.
	modelsDevMaxAge      = 7 * 24 * time.Hour
	modelsDevFetchLimit  = 32 << 20 // 32MiB guard on the response body
	modelsDevFetchWindow = 15 * time.Second
)

// modelsDevModel is the subset of a models.dev model record the overlay uses.
type modelsDevModel struct {
	Limit struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
	Cost struct {
		Input      float64 `json:"input"`
		Output     float64 `json:"output"`
		CacheRead  float64 `json:"cache_read"`
		CacheWrite float64 `json:"cache_write"`
	} `json:"cost"`
}

// modelsDevProvider matches one provider object in api.json.
type modelsDevProvider struct {
	Models map[string]modelsDevModel `json:"models"`
}

// parseModelsDev decodes an api.json document into provider-slug -> api-model
// -> record. api.json is a top-level object keyed by provider id.
func parseModelsDev(data []byte) (map[string]map[string]modelsDevModel, error) {
	var doc map[string]modelsDevProvider
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("modelregistry: parse models.dev: %w", err)
	}
	providers := make(map[string]map[string]modelsDevModel, len(doc))
	for slug, provider := range doc {
		if len(provider.Models) == 0 {
			continue
		}
		providers[slug] = provider.Models
	}
	if len(providers) == 0 {
		return nil, fmt.Errorf("modelregistry: models.dev document has no providers")
	}
	return providers, nil
}

// modelsDevSlugs maps an entry's provider kind to the models.dev provider ids
// worth checking. Only first-party slugs are used: router entries on models.dev
// carry generic or stale numbers.
func modelsDevSlugs(kind ProviderKind) []string {
	switch kind {
	case ProviderAnthropic:
		return []string{"anthropic"}
	case ProviderOpenAI:
		return []string{"openai"}
	case ProviderGoogle:
		return []string{"google", "google-vertex"}
	default:
		return nil
	}
}

// applyModelsDevOverrides refreshes each curated entry's context limits and
// base pricing from the snapshot, when the snapshot knows the model. Tiered
// pricing is never touched: models.dev has no tier data, and mixing a live
// base rate with curated tiers would misprice the tier boundaries.
func applyModelsDevOverrides(entries []ModelEntry, providers map[string]map[string]modelsDevModel) []ModelEntry {
	if len(providers) == 0 {
		return entries
	}
	for i := range entries {
		entry := &entries[i]
		var record modelsDevModel
		found := false
		for _, slug := range modelsDevSlugs(entry.Provider) {
			if models, ok := providers[slug]; ok {
				if candidate, ok := models[strings.TrimSpace(entry.APIModel)]; ok {
					record = candidate
					found = true
					break
				}
			}
		}
		if !found {
			continue
		}
		if record.Limit.Context > 0 {
			entry.ContextLimits.ContextWindow = record.Limit.Context
		}
		if record.Limit.Output > 0 {
			entry.ContextLimits.MaxOutputTokens = record.Limit.Output
		}
		if len(entry.Cost.Tiers) == 0 && record.Cost.Input > 0 && record.Cost.Output > 0 {
			entry.Cost.InputPerMillion = record.Cost.Input
			entry.Cost.OutputPerMillion = record.Cost.Output
			if record.Cost.CacheRead > 0 {
				entry.Cost.CachedInputPerMillion = record.Cost.CacheRead
			}
			if record.Cost.CacheWrite > 0 {
				entry.Cost.CacheWritePerMillion = record.Cost.CacheWrite
			}
			entry.Cost.Source = "models.dev/api.json (cached)"
		}
	}
	return entries
}

// modelsDevCachePath returns the on-disk cache location. ZERO_MODELS_CACHE_PATH
// overrides it (used by tests and unusual setups).
func modelsDevCachePath() (string, error) {
	if override := strings.TrimSpace(os.Getenv("ZERO_MODELS_CACHE_PATH")); override != "" {
		return override, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "zero", "modelsdev.json"), nil
}

var (
	modelsDevOnce    sync.Once
	modelsDevCached  map[string]map[string]modelsDevModel
	modelsDevEnabled atomic.Bool
)

// EnableModelsDevOverlay opts the process into applying the cached models.dev
// snapshot on top of the curated catalog. The CLI entrypoint calls it; library
// consumers and tests that never do get the curated catalog byte-identical to
// before, so hermetic tests can't be perturbed by a cache file on the machine.
// ZERO_DISABLE_MODELS_FETCH disables both the overlay and the fetch.
func EnableModelsDevOverlay() {
	modelsDevEnabled.Store(true)
}

// cachedModelsDevProviders loads the cached snapshot once per process. Not
// enabled, missing, stale (> modelsDevMaxAge), or malformed all yield nil and
// the curated catalog is used untouched. Read once deliberately:
// DefaultRegistry is called on hot paths (pickers, cost views) and must not
// re-stat the file every time; a background refresh benefits the NEXT process.
func cachedModelsDevProviders() map[string]map[string]modelsDevModel {
	if !modelsDevEnabled.Load() || strings.TrimSpace(os.Getenv("ZERO_DISABLE_MODELS_FETCH")) != "" {
		return nil
	}
	modelsDevOnce.Do(func() {
		path, err := modelsDevCachePath()
		if err != nil {
			return
		}
		info, err := os.Stat(path)
		if err != nil || time.Since(info.ModTime()) > modelsDevMaxAge {
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return
		}
		providers, err := parseModelsDev(data)
		if err != nil {
			return
		}
		modelsDevCached = providers
	})
	return modelsDevCached
}

// startModelsDevRefresh starts a background RefreshModelsDevCache. Tests
// replace it to see whether a refresh was started.
var startModelsDevRefresh = func() {
	go func() { _ = RefreshModelsDevCache(context.Background()) }()
}

// StartModelsDevRefresh refreshes the models.dev cache in the background when
// the fetch is on. ZERO_DISABLE_MODELS_FETCH is read here, before anything is
// started: a refresh goroutine reads the environment when it runs, so one
// started by a caller that had turned the fetch off (a test, through t.Setenv)
// could run after that setting was restored and fetch into the real cache.
func StartModelsDevRefresh() {
	if strings.TrimSpace(os.Getenv("ZERO_DISABLE_MODELS_FETCH")) != "" {
		return
	}
	startModelsDevRefresh()
}

// RefreshModelsDevCache fetches models.dev/api.json into the on-disk cache
// when the cache is missing or older than modelsDevRefreshAfter. It is safe to
// call fire-and-forget from startup (use a goroutine); it never affects the
// current process's registry (see cachedModelsDevProviders). Disabled entirely
// by ZERO_DISABLE_MODELS_FETCH. The URL can be overridden with ZERO_MODELS_URL.
func RefreshModelsDevCache(ctx context.Context) error {
	if strings.TrimSpace(os.Getenv("ZERO_DISABLE_MODELS_FETCH")) != "" {
		return nil
	}
	path, err := modelsDevCachePath()
	if err != nil {
		return err
	}
	if info, err := os.Stat(path); err == nil && time.Since(info.ModTime()) < modelsDevRefreshAfter {
		return nil
	}

	url := strings.TrimSpace(os.Getenv("ZERO_MODELS_URL"))
	if url == "" {
		url = modelsDevDefaultURL
	}
	fetchCtx, cancel := context.WithTimeout(ctx, modelsDevFetchWindow)
	defer cancel()
	request, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "zero-models-refresh/0.1")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("modelregistry: models.dev fetch: HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, modelsDevFetchLimit+1))
	if err != nil {
		return err
	}
	if len(data) > modelsDevFetchLimit {
		return fmt.Errorf("modelregistry: models.dev response is %d bytes, exceeds %d byte limit", len(data), modelsDevFetchLimit)
	}
	// Validate before persisting: a bad body must never clobber a good cache.
	if _, err := parseModelsDev(data); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), "modelsdev-*.json")
	if err != nil {
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		_ = os.Remove(temp.Name())
		return err
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(temp.Name())
		return err
	}
	return os.Rename(temp.Name(), path)
}
