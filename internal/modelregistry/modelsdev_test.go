package modelregistry

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sampleModelsDev = `{
  "anthropic": {
    "id": "anthropic",
    "models": {
      "claude-sonnet-4-5-20250929": {
        "limit": {"context": 1000000, "output": 64000},
        "cost": {"input": 3.5, "output": 17.5, "cache_read": 0.35, "cache_write": 4.4}
      }
    }
  },
  "google": {
    "id": "google",
    "models": {
      "gemini-2.5-pro": {
        "limit": {"context": 2097152, "output": 65536},
        "cost": {"input": 9.99, "output": 9.99}
      }
    }
  }
}`

func TestParseModelsDev(t *testing.T) {
	providers, err := parseModelsDev([]byte(sampleModelsDev))
	if err != nil {
		t.Fatal(err)
	}
	record, ok := providers["anthropic"]["claude-sonnet-4-5-20250929"]
	if !ok {
		t.Fatal("expected anthropic sonnet record")
	}
	if record.Limit.Context != 1_000_000 || record.Cost.Input != 3.5 {
		t.Fatalf("unexpected record: %+v", record)
	}
	if _, err := parseModelsDev([]byte(`{}`)); err == nil {
		t.Fatal("empty document must be rejected")
	}
	if _, err := parseModelsDev([]byte(`not json`)); err == nil {
		t.Fatal("malformed document must be rejected")
	}
}

func TestApplyModelsDevOverrides(t *testing.T) {
	// Point the cache at a non-existent file so DefaultModelEntries returns the
	// pure curated catalog, then apply the sample snapshot explicitly.
	t.Setenv("ZERO_MODELS_CACHE_PATH", filepath.Join(t.TempDir(), "absent.json"))
	resetModelsDevCacheForTest()
	t.Cleanup(resetModelsDevCacheForTest)

	providers, err := parseModelsDev([]byte(sampleModelsDev))
	if err != nil {
		t.Fatal(err)
	}
	entries := applyModelsDevOverrides(DefaultModelEntries(), providers)

	var sonnet, geminiPro, opus ModelEntry
	for _, entry := range entries {
		switch entry.ID {
		case "claude-sonnet-4.5":
			sonnet = entry
		case "gemini-2.5-pro":
			geminiPro = entry
		case "claude-opus-4.1":
			opus = entry
		}
	}

	// Known model: limits and base pricing refreshed from the snapshot.
	if sonnet.ContextLimits.ContextWindow != 1_000_000 || sonnet.ContextLimits.MaxOutputTokens != 64_000 {
		t.Fatalf("sonnet limits not overridden: %+v", sonnet.ContextLimits)
	}
	if sonnet.Cost.InputPerMillion != 3.5 || sonnet.Cost.OutputPerMillion != 17.5 || sonnet.Cost.CacheWritePerMillion != 4.4 {
		t.Fatalf("sonnet cost not overridden: %+v", sonnet.Cost)
	}
	if sonnet.Cost.Source != "models.dev/api.json (cached)" {
		t.Fatalf("sonnet cost source not marked: %q", sonnet.Cost.Source)
	}

	// Tiered pricing is curated: limits refresh, cost must NOT (gemini-2.5-pro
	// has curated tiers and the snapshot's flat 9.99 would misprice them).
	if geminiPro.ContextLimits.ContextWindow != 2_097_152 {
		t.Fatalf("gemini limits not overridden: %+v", geminiPro.ContextLimits)
	}
	if geminiPro.Cost.InputPerMillion == 9.99 || len(geminiPro.Cost.Tiers) == 0 {
		t.Fatalf("tiered cost must stay curated: %+v", geminiPro.Cost)
	}

	// Model absent from the snapshot: untouched.
	if opus.ContextLimits.ContextWindow != 200_000 || opus.Cost.InputPerMillion != 15 {
		t.Fatalf("opus must be untouched: %+v %+v", opus.ContextLimits, opus.Cost)
	}
}

func TestRefreshModelsDevCacheFetchesAndCaches(t *testing.T) {
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(sampleModelsDev))
	}))
	defer server.Close()

	cachePath := filepath.Join(t.TempDir(), "modelsdev.json")
	t.Setenv("ZERO_MODELS_CACHE_PATH", cachePath)
	t.Setenv("ZERO_MODELS_URL", server.URL)
	t.Setenv("ZERO_DISABLE_MODELS_FETCH", "")

	if err := RefreshModelsDevCache(t.Context()); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("expected 1 fetch, got %d", hits)
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("cache file missing: %v", err)
	}
	// Fresh cache: second call must not re-fetch.
	if err := RefreshModelsDevCache(t.Context()); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("fresh cache must skip the fetch, got %d hits", hits)
	}
}

func TestRefreshModelsDevCacheRejectsBadBodyWithoutClobbering(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer server.Close()

	cachePath := filepath.Join(t.TempDir(), "modelsdev.json")
	if err := os.WriteFile(cachePath, []byte(sampleModelsDev), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(cachePath, stale, stale); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZERO_MODELS_CACHE_PATH", cachePath)
	t.Setenv("ZERO_MODELS_URL", server.URL)

	if err := RefreshModelsDevCache(t.Context()); err == nil {
		t.Fatal("bad body must return an error")
	}
	content, err := os.ReadFile(cachePath)
	if err != nil || string(content) != sampleModelsDev {
		t.Fatal("bad fetch must not clobber the existing cache")
	}
}

func TestRefreshModelsDevCacheRejectsOversizedResponse(t *testing.T) {
	// Repro: valid JSON + padding up to the limit + one extra byte.
	oversized := sampleModelsDev + strings.Repeat(" ", modelsDevFetchLimit-len(sampleModelsDev)+1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(oversized))
	}))
	defer server.Close()

	cachePath := filepath.Join(t.TempDir(), "modelsdev.json")
	if err := os.WriteFile(cachePath, []byte(sampleModelsDev), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(cachePath, stale, stale); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZERO_MODELS_CACHE_PATH", cachePath)
	t.Setenv("ZERO_MODELS_URL", server.URL)
	t.Setenv("ZERO_DISABLE_MODELS_FETCH", "")

	if err := RefreshModelsDevCache(t.Context()); err == nil {
		t.Fatal("oversized response must return an error")
	}
	content, err := os.ReadFile(cachePath)
	if err != nil || string(content) != sampleModelsDev {
		t.Fatal("oversized fetch must not clobber the existing cache")
	}
}

func TestRefreshModelsDevCacheDisabledByEnv(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("fetch must not happen when disabled")
	}))
	defer server.Close()
	t.Setenv("ZERO_MODELS_CACHE_PATH", filepath.Join(t.TempDir(), "modelsdev.json"))
	t.Setenv("ZERO_MODELS_URL", server.URL)
	t.Setenv("ZERO_DISABLE_MODELS_FETCH", "1")
	if err := RefreshModelsDevCache(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestCachedModelsDevProvidersIgnoresStaleCache(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "modelsdev.json")
	if err := os.WriteFile(cachePath, []byte(sampleModelsDev), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-modelsDevMaxAge - time.Hour)
	if err := os.Chtimes(cachePath, stale, stale); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZERO_MODELS_CACHE_PATH", cachePath)
	resetModelsDevCacheForTest()
	t.Cleanup(resetModelsDevCacheForTest)
	EnableModelsDevOverlay()

	if providers := cachedModelsDevProviders(); providers != nil {
		t.Fatal("stale cache must be ignored")
	}
}

func TestCachedModelsDevProvidersRequiresOptIn(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "modelsdev.json")
	if err := os.WriteFile(cachePath, []byte(sampleModelsDev), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZERO_MODELS_CACHE_PATH", cachePath)
	resetModelsDevCacheForTest()
	t.Cleanup(resetModelsDevCacheForTest)

	// Without EnableModelsDevOverlay a fresh, valid cache must still be ignored:
	// library consumers and hermetic tests get the pure curated catalog.
	if providers := cachedModelsDevProviders(); providers != nil {
		t.Fatal("overlay must be opt-in")
	}
}

func TestDefaultModelEntriesAppliesFreshCache(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "modelsdev.json")
	if err := os.WriteFile(cachePath, []byte(sampleModelsDev), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZERO_MODELS_CACHE_PATH", cachePath)
	resetModelsDevCacheForTest()
	t.Cleanup(resetModelsDevCacheForTest)
	EnableModelsDevOverlay()

	for _, entry := range DefaultModelEntries() {
		if entry.ID == "claude-sonnet-4.5" {
			if entry.ContextLimits.ContextWindow != 1_000_000 {
				t.Fatalf("fresh cache must overlay the registry: %+v", entry.ContextLimits)
			}
			return
		}
	}
	t.Fatal("claude-sonnet-4.5 not found")
}

// With the fetch off, no refresh is started at all, so none is left running to
// read the environment after a test has restored it and fetch into the real
// cache.
func TestStartModelsDevRefreshStartsNothingWhenTheFetchIsOff(t *testing.T) {
	started := 0
	previous := startModelsDevRefresh
	startModelsDevRefresh = func() { started++ }
	t.Cleanup(func() { startModelsDevRefresh = previous })

	t.Setenv("ZERO_DISABLE_MODELS_FETCH", "1")
	StartModelsDevRefresh()
	if started != 0 {
		t.Fatalf("started %d refreshes with the fetch off, want none", started)
	}

	t.Setenv("ZERO_DISABLE_MODELS_FETCH", "")
	StartModelsDevRefresh()
	if started != 1 {
		t.Fatalf("started %d refreshes with the fetch on, want 1", started)
	}
}
