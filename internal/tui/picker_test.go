package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/oauth"
	"github.com/Gitlawb/zero/internal/providercatalog"
	"github.com/Gitlawb/zero/internal/providermodeldiscovery"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

func TestModelPickerDetectsOllamaCloudFromBaseURL(t *testing.T) {
	m := newModel(context.Background(), Options{
		ProviderName: "custom-openai-compatible",
		ModelName:    "minimax-m3",
		ProviderProfile: config.ProviderProfile{
			Name:         "custom-openai-compatible",
			CatalogID:    "custom-openai-compatible",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://ollama.com/v1",
			APIKeyEnv:    "OLLAMA_API_KEY",
			Model:        "minimax-m3",
		},
	})

	picker := m.newModelPicker()
	if picker == nil {
		t.Fatal("expected model picker")
	}
	groups := pickerGroups(picker.items)
	if !contains(groups, "Ollama Cloud") {
		t.Fatalf("picker groups = %#v, want Ollama Cloud", groups)
	}
	got := pickerValues(picker.items)
	if !contains(got, "qwen3-coder:480b") {
		t.Fatalf("picker values = %#v, want Ollama Cloud models", got)
	}
	if contains(got, "custom-model") {
		t.Fatalf("picker should not show custom-openai-compatible fallback when URL is Ollama Cloud: %#v", got)
	}
}

func TestModelPickerChatGPTDiscoveryUsesMatchingOAuthAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth-tokens.json")
	t.Setenv("ZERO_OAUTH_STORAGE", "file")
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", path)
	store, err := oauth.NewStore(oauth.StoreOptions{FilePath: path})
	if err != nil {
		t.Fatalf("oauth store: %v", err)
	}
	if err := store.Save(oauth.ProviderKey("chatgpt"), oauth.Token{
		AccessToken: "live-token",
		Account:     "workspace-77",
		ExpiresAt:   time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("save OAuth token: %v", err)
	}

	m := model{userAgent: "zero/test"}
	options := m.modelPickerDiscoveryOptions(config.ProviderProfile{Name: "codex", CatalogID: "chatgpt"})
	if options.OAuthResolver == nil || options.CodexAccountResolver == nil {
		t.Fatal("ChatGPT model discovery must carry both bearer and account resolvers")
	}
	header, bearer, ok, err := options.OAuthResolver(context.Background(), false)
	if err != nil || !ok || header != "Authorization" || bearer != "Bearer live-token" {
		t.Fatalf("discovery bearer = (%q, %q, %v, %v)", header, bearer, ok, err)
	}
	account, ok, err := options.CodexAccountResolver(context.Background())
	if err != nil || !ok || account != "workspace-77" {
		t.Fatalf("discovery account = (%q, %v, %v)", account, ok, err)
	}
	if options.UserAgent != "zero/test" {
		t.Fatalf("discovery user agent = %q", options.UserAgent)
	}
}

func TestModelPickerRefreshesLiveModelsForActiveProvider(t *testing.T) {
	var captured config.ProviderProfile
	m := newModel(context.Background(), Options{
		ProviderName: "ollama-cloud",
		ModelName:    "minimax-m3",
		ProviderProfile: config.ProviderProfile{
			Name:         "ollama-cloud",
			CatalogID:    "ollama-cloud",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://ollama.com/v1",
			APIKey:       "ollama-key",
			Model:        "minimax-m3",
		},
		DiscoverProviderModels: func(ctx context.Context, profile config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
			captured = profile
			return []providermodeldiscovery.Model{
				{ID: "live-cloud-a", Description: "Live Cloud A"},
				{ID: "live-cloud-b", Description: "Live Cloud B"},
			}, nil
		},
	})
	m.input.SetValue("/model")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if next.picker == nil {
		t.Fatal("expected model picker to open")
	}
	if cmd == nil {
		t.Fatal("opening /model for an active provider should start model discovery")
	}
	updated, _ = next.Update(cmd())
	next = updated.(model)

	if captured.CatalogID != "ollama-cloud" {
		t.Fatalf("discovery profile catalog = %q, want ollama-cloud", captured.CatalogID)
	}
	got := pickerValues(next.picker.items)
	if !contains(got, "live-cloud-a") || !contains(got, "live-cloud-b") {
		t.Fatalf("picker values = %#v, want live cloud models", got)
	}
}

func TestModelPickerTreatsCustomOpenAIEndpointAsCustomProvider(t *testing.T) {
	var captured config.ProviderProfile
	m := newModel(context.Background(), Options{
		ProviderName: "openai",
		ModelName:    "custom-coder",
		ProviderProfile: config.ProviderProfile{
			Name:         "openai",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://proxy.example.test/v1",
			APIKey:       "proxy-key",
			Model:        "custom-coder",
		},
		DiscoverProviderModels: func(ctx context.Context, profile config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
			captured = profile
			return []providermodeldiscovery.Model{
				{ID: "custom-coder-plus", Description: "Custom Coder Plus"},
			}, nil
		},
	})
	m.input.SetValue("/model")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if next.picker == nil {
		t.Fatal("expected model picker to open")
	}
	if cmd == nil {
		t.Fatal("opening /model for a custom endpoint should start model discovery")
	}
	updated, _ = next.Update(cmd())
	next = updated.(model)

	if captured.CatalogID != "custom-openai-compatible" || captured.ProviderKind != config.ProviderKindOpenAICompatible {
		t.Fatalf("discovery profile = %#v, want custom OpenAI-compatible identity", captured)
	}
	groups := pickerGroups(next.picker.items)
	if !contains(groups, "Custom OpenAI-compatible") {
		t.Fatalf("picker groups = %#v, want custom provider group", groups)
	}
	got := pickerValues(next.picker.items)
	if !contains(got, "custom-coder-plus") {
		t.Fatalf("picker values = %#v, want discovered custom model", got)
	}
	if contains(got, "gpt-4.1") {
		t.Fatalf("custom endpoint picker should not fall back to official OpenAI models: %#v", got)
	}
}

func TestModelPickerShowsLoadingUntilDiscoveryCompletes(t *testing.T) {
	m := newModel(context.Background(), Options{
		ProviderName: "ollama-cloud",
		ModelName:    "minimax-m3",
		ProviderProfile: config.ProviderProfile{
			Name:         "ollama-cloud",
			CatalogID:    "ollama-cloud",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://ollama.com/v1",
			APIKey:       "ollama-key",
			Model:        "minimax-m3",
		},
		DiscoverProviderModels: func(ctx context.Context, profile config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
			return []providermodeldiscovery.Model{
				{ID: "live-cloud-a", Description: "Live Cloud A"},
				{ID: "live-cloud-b", Description: "Live Cloud B"},
			}, nil
		},
	})
	m.input.SetValue("/model")
	updated, cmd := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if cmd == nil {
		t.Fatal("expected opening the model picker to start background discovery")
	}
	// The list shows immediately (no blocking overlay) before discovery returns.
	immediate := plainRender(t, m.pickerOverlay(100))
	assertNotContains(t, immediate, "Checking available models...")
	assertNotContains(t, immediate, "Live Cloud A")
	if m.picker == nil {
		t.Fatal("picker should be open immediately")
	}

	// When the provider's discovery returns, its section shows the live models.
	updated, _ = m.Update(modelPickerModelsDiscoveredMsg{
		providerID: "ollama-cloud",
		models:     []providermodeldiscovery.Model{{ID: "live-cloud-a", Description: "Live Cloud A"}},
	})
	m = updated.(model)
	loaded := plainRender(t, m.pickerOverlay(100))
	assertContains(t, loaded, "Live Cloud A")
}

func TestModelPickerMetadataOmitsCredentialEnv(t *testing.T) {
	m := newModel(context.Background(), Options{
		ProviderName: "ollama-cloud",
		ModelName:    "minimax-m3",
		ProviderProfile: config.ProviderProfile{
			Name:         "ollama-cloud",
			CatalogID:    "ollama-cloud",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://ollama.com/v1",
			APIKeyEnv:    "OLLAMA_API_KEY",
			Model:        "minimax-m3",
		},
	})
	m.modelPickerLiveByProvider = map[string][]providermodeldiscovery.Model{
		"ollama-cloud": {
			{
				ID:            "cogito-2.1:671b",
				ContextWindow: 163840,
				ToolCall:      true,
				Reasoning:     true,
			},
		},
	}
	m.picker = m.newModelPicker()
	if m.picker == nil {
		t.Fatal("expected model picker")
	}
	target := pickerIndex(m.picker.items, "cogito-2.1:671b")
	if target < 0 {
		t.Fatalf("expected cogito model in picker, got %#v", pickerValues(m.picker.items))
	}
	m.picker.selected = target

	view := plainRender(t, m.pickerOverlay(100))
	assertContains(t, view, "163K ctx")
	assertContains(t, view, "tools")
	assertContains(t, view, "reasoning")
	assertNotContains(t, view, "OLLAMA_API_KEY")
	for _, item := range m.picker.items {
		assertNotContains(t, item.Meta, "API_KEY")
	}
}

func TestModelPickerFallsBackWhenDiscoveryFails(t *testing.T) {
	m := newModel(context.Background(), Options{
		ProviderName: "ollama-cloud",
		ModelName:    "minimax-m3",
		ProviderProfile: config.ProviderProfile{
			Name:         "ollama-cloud",
			CatalogID:    "ollama-cloud",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://ollama.com/v1",
			APIKey:       "ollama-key",
			Model:        "minimax-m3",
		},
		DiscoverProviderModels: func(ctx context.Context, profile config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
			return nil, errors.New("offline")
		},
	})
	m.input.SetValue("/model")
	updated, cmd := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if cmd == nil {
		t.Fatal("expected opening the model picker to start discovery")
	}
	// A failed discovery leaves the static catalog list in place — no crash, no block.
	updated, _ = m.Update(modelPickerModelsDiscoveredMsg{providerID: "ollama-cloud", models: nil, err: errors.New("offline")})
	m = updated.(model)
	view := plainRender(t, m.pickerOverlay(100))
	assertNotContains(t, view, "Checking available models...")
	if m.picker == nil || len(m.picker.items) == 0 {
		t.Fatal("static catalog list should remain after a failed discovery")
	}
}

func TestModelPickerAppliesLiveDiscoveredModelID(t *testing.T) {
	var captured config.ProviderProfile
	m := newModel(context.Background(), Options{
		ProviderName: "ollama-cloud",
		ModelName:    "minimax-m3",
		Provider:     &fakeProvider{},
		ProviderProfile: config.ProviderProfile{
			Name:         "ollama-cloud",
			CatalogID:    "ollama-cloud",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://ollama.com/v1",
			APIKey:       "ollama-key",
			Model:        "minimax-m3",
		},
		NewProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			captured = profile
			return &fakeProvider{}, nil
		},
	})
	m.modelPickerLiveByProvider = map[string][]providermodeldiscovery.Model{"ollama-cloud": {{ID: "glm-5.1", Description: "GLM 5.1"}}}
	m.picker = m.newModelPicker()
	m.picker.selected = pickerIndex(m.picker.items, "glm-5.1")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if captured.Model != "glm-5.1" {
		t.Fatalf("captured model = %q, want glm-5.1", captured.Model)
	}
	if next.modelName != "glm-5.1" {
		t.Fatalf("active model = %q, want glm-5.1", next.modelName)
	}
}

func TestModelSwitchNormalizesDetectedOllamaCloudProfile(t *testing.T) {
	var captured config.ProviderProfile
	m := newModel(context.Background(), Options{
		ProviderName: "custom-openai-compatible",
		ModelName:    "minimax-m3",
		Provider:     &fakeProvider{},
		ProviderProfile: config.ProviderProfile{
			Name:         "custom-openai-compatible",
			CatalogID:    "custom-openai-compatible",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://ollama.com/v1",
			APIKeyEnv:    "OPENAI_API_KEY",
			Model:        "minimax-m3",
		},
		NewProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			captured = profile
			return &fakeProvider{}, nil
		},
	})
	m.modelPickerLiveByProvider = map[string][]providermodeldiscovery.Model{"ollama-cloud": {{ID: "glm-5.1", Description: "GLM 5.1"}}}
	m.input.SetValue("/model glm-5.1")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if captured.Name != "ollama-cloud" || captured.CatalogID != "ollama-cloud" {
		t.Fatalf("captured provider identity = %#v, want ollama-cloud", captured)
	}
	if captured.APIKeyEnv != "OLLAMA_API_KEY" {
		t.Fatalf("captured APIKeyEnv = %q, want OLLAMA_API_KEY", captured.APIKeyEnv)
	}
	if captured.Model != "glm-5.1" {
		t.Fatalf("captured model = %q, want glm-5.1", captured.Model)
	}
	if next.providerName != "ollama-cloud" {
		t.Fatalf("providerName = %q, want ollama-cloud", next.providerName)
	}
}

func TestModelPickerSearchFiltersModels(t *testing.T) {
	m := newModel(context.Background(), Options{
		ProviderName: "ollama-cloud",
		ModelName:    "minimax-m3",
		ProviderProfile: config.ProviderProfile{
			Name:         "ollama-cloud",
			CatalogID:    "ollama-cloud",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://ollama.com/v1",
			APIKeyEnv:    "OLLAMA_API_KEY",
			Model:        "minimax-m3",
		},
	})
	m.picker = m.newModelPicker()

	updated, _ := m.Update(testKeyText("qwen"))
	next := updated.(model)
	if next.picker.query != "qwen" {
		t.Fatalf("picker query = %q, want qwen", next.picker.query)
	}
	view := plainRender(t, next.pickerOverlay(100))
	assertContains(t, view, "search > qwen")
	assertContains(t, view, "Qwen")
	assertNotContains(t, view, "Minimax M3")
}

func TestModelPickerFavoriteShortcutTogglesSelectedModel(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")
	m := newModel(context.Background(), Options{
		UserConfigPath: configPath,
		ProviderName:   "ollama-cloud",
		ModelName:      "minimax-m3",
		ProviderProfile: config.ProviderProfile{
			Name:         "ollama-cloud",
			CatalogID:    "ollama-cloud",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://ollama.com/v1",
			APIKeyEnv:    "OLLAMA_API_KEY",
			Model:        "minimax-m3",
		},
	})
	m.picker = m.newModelPicker()
	if m.picker == nil {
		t.Fatal("expected model picker")
	}
	target := pickerIndex(m.picker.items, "qwen3-coder:480b")
	if target < 0 {
		t.Fatalf("expected qwen3-coder:480b in picker, got %#v", pickerValues(m.picker.items))
	}
	m.picker.selected = target

	updated, _ := m.Update(testKeyCtrl('f'))
	next := updated.(model)
	if !next.favoriteModels["qwen3-coder:480b"] {
		t.Fatalf("favorite map = %#v, want qwen3-coder:480b favorited", next.favoriteModels)
	}
	if next.picker.items[0].Group != "Favorites" || next.picker.items[0].Value != "qwen3-coder:480b" {
		t.Fatalf("first picker item = %#v, want favorite group row", next.picker.items[0])
	}
	persisted := readTUIConfigFixture(t, configPath)
	if len(persisted.Preferences.FavoriteModels) != 1 || persisted.Preferences.FavoriteModels[0] != "qwen3-coder:480b" {
		t.Fatalf("persisted FavoriteModels = %#v, want qwen3-coder:480b", persisted.Preferences.FavoriteModels)
	}

	updated, _ = next.Update(testKeyCtrl('f'))
	next = updated.(model)
	if next.favoriteModels["qwen3-coder:480b"] {
		t.Fatalf("favorite map = %#v, want qwen3-coder:480b unfavorited", next.favoriteModels)
	}
	if len(next.picker.items) > 0 && next.picker.items[0].Group == "Favorites" {
		t.Fatalf("favorites group should be gone after unfavorite, got first item %#v", next.picker.items[0])
	}
	persisted = readTUIConfigFixture(t, configPath)
	if len(persisted.Preferences.FavoriteModels) != 0 {
		t.Fatalf("persisted FavoriteModels = %#v, want empty after unfavorite", persisted.Preferences.FavoriteModels)
	}
}

func TestModelPickerLoadsFavoriteModelsFromOptions(t *testing.T) {
	m := newModel(context.Background(), Options{
		ProviderName:    "ollama-cloud",
		ModelName:       "minimax-m3",
		FavoriteModels:  []string{"qwen3-coder:480b"},
		ProviderProfile: config.ProviderProfile{Name: "ollama-cloud", CatalogID: "ollama-cloud", Model: "minimax-m3"},
	})
	picker := m.newModelPicker()
	if picker == nil {
		t.Fatal("expected model picker")
	}
	if picker.items[0].Group != "Favorites" || picker.items[0].Value != "qwen3-coder:480b" {
		t.Fatalf("first picker item = %#v, want persisted favorite first", picker.items[0])
	}
}

func TestModelPickerShowsRecentThenActiveProviderCatalog(t *testing.T) {
	m := newModel(context.Background(), Options{
		ProviderName: "openrouter",
		ModelName:    "google/gemini-2.5-pro",
		ProviderProfile: config.ProviderProfile{
			Name:      "openrouter",
			CatalogID: "openrouter",
			Model:     "google/gemini-2.5-pro",
			APIKeyEnv: "OPENROUTER_API_KEY",
			Provider:  string(config.ProviderKindOpenAICompatible),
			BaseURL:   "https://openrouter.ai/api/v1",
			APIFormat: "chat-completions",
		},
	})

	picker := m.newModelPicker()
	if picker == nil {
		t.Fatal("expected a model picker")
	}
	if picker.items[0].Group != "Recent" {
		t.Fatalf("first picker group = %q, want Recent", picker.items[0].Group)
	}
	if picker.items[0].Value != "google/gemini-2.5-pro" {
		t.Fatalf("first picker value = %q, want active recent model", picker.items[0].Value)
	}
	if picker.items[1].Group != "OpenRouter" {
		t.Fatalf("second picker group = %q, want OpenRouter", picker.items[1].Group)
	}
	got := pickerValues(picker.items)
	if !contains(got, "anthropic/claude-sonnet-4.5") || !contains(got, "minimax/minimax-m2.1") {
		t.Fatalf("active provider catalog missing expected OpenRouter models: %#v", got)
	}
	if contains(got, "claude-haiku-4.5") {
		t.Fatalf("picker should not include unrelated global Anthropic registry model under OpenRouter: %#v", got)
	}
}

func TestModelPickerAppliesActiveProviderCatalogModelID(t *testing.T) {
	var captured config.ProviderProfile
	m := newModel(context.Background(), Options{
		ProviderName: "openrouter",
		ModelName:    "google/gemini-2.5-pro",
		Provider:     &fakeProvider{},
		ProviderProfile: config.ProviderProfile{
			Name:         "openrouter",
			CatalogID:    "openrouter",
			ProviderKind: config.ProviderKindOpenAICompatible,
			Model:        "google/gemini-2.5-pro",
			APIKeyEnv:    "OPENROUTER_API_KEY",
			BaseURL:      "https://openrouter.ai/api/v1",
			APIFormat:    "chat-completions",
		},
		NewProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			captured = profile
			return &fakeProvider{}, nil
		},
	})
	m.input.SetValue("/model openai/gpt-4.1")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if next.pending {
		t.Fatal("expected /model to be handled without starting a run")
	}
	if captured.Model != "openai/gpt-4.1" {
		t.Fatalf("captured model = %q, want raw OpenRouter model ID", captured.Model)
	}
	if next.modelName != "openai/gpt-4.1" {
		t.Fatalf("active model = %q, want raw OpenRouter model ID", next.modelName)
	}
	if !strings.Contains(next.transientNotice.text, "openai/gpt-4.1") {
		t.Fatalf("model switch notice = %q", next.transientNotice.text)
	}
}

// recentModelPairsForPicker pins the active provider+model first (even before
// it is recorded to history), then follows persisted history in order,
// de-duplicating by provider+model pair and capping to config.MaxRecentModels
// (issue: /model picker's "Recent" section only ever showed the active model).
func TestRecentModelPairsForPickerPinsActiveDedupesAndCaps(t *testing.T) {
	m := model{
		providerName: "openrouter",
		modelName:    "google/gemini-2.5-pro",
		recentModels: []config.RecentModelEntry{
			{Provider: "openrouter", Model: "google/gemini-2.5-pro"}, // dup of active: collapses
			{Provider: "openrouter", Model: "a"},
			{Provider: "openrouter", Model: "b"},
			{Provider: "openrouter", Model: "c"},
			{Provider: "openrouter", Model: "d"},
			{Provider: "openrouter", Model: "e"}, // beyond the cap: dropped
		},
	}
	got := m.recentModelPairsForPicker()
	want := []config.RecentModelEntry{
		{Provider: "openrouter", Model: "google/gemini-2.5-pro"},
		{Provider: "openrouter", Model: "a"},
		{Provider: "openrouter", Model: "b"},
		{Provider: "openrouter", Model: "c"},
		{Provider: "openrouter", Model: "d"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("recentModelPairsForPicker() = %#v, want %#v", got, want)
	}
	if len(got) != config.MaxRecentModels {
		t.Fatalf("len = %d, want config.MaxRecentModels (%d)", len(got), config.MaxRecentModels)
	}
}

func TestRecentModelPairsForPickerCanonicalizesChatGPTOpenAIModelAliases(t *testing.T) {
	m := newModel(context.Background(), Options{
		ProviderName: "chatgpt",
		ModelName:    "gpt-5.6-sol",
		ProviderProfile: config.ProviderProfile{
			Name:      "chatgpt",
			CatalogID: "chatgpt",
		},
		RecentModels: []config.RecentModelEntry{
			{Provider: "chatgpt", Model: "openai/gpt-5.6-sol"},
			{Provider: "chatgpt", Model: "gpt-5.6-terra"},
		},
	})

	got := m.recentModelPairsForPicker()
	want := []config.RecentModelEntry{
		{Provider: "chatgpt", Model: "gpt-5.6-sol"},
		{Provider: "chatgpt", Model: "gpt-5.6-terra"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("recentModelPairsForPicker() = %#v, want %#v", got, want)
	}
}

// The picker's cross-group de-dup must key on provider+model, not model id
// alone: the same model id can be offered by two different providers, and
// those are distinct, independently selectable rows (issue: recents should
// "de-duplicate by provider+model pair, not model name alone").
func TestAssembleModelPickerItemsDedupesByProviderAndModelPairNotModelIDAlone(t *testing.T) {
	m := model{}
	recent := []pickerItem{
		{Value: "shared-model", OwnerProvider: "provider-a"},
		{Value: "shared-model", OwnerProvider: "provider-b"},
	}
	items := m.assembleModelPickerItems(recent, nil)
	if len(items) != 2 {
		t.Fatalf("expected both provider-a and provider-b rows to survive de-dup, got %#v", items)
	}
	owners := map[string]bool{}
	for _, item := range items {
		owners[item.OwnerProvider] = true
	}
	if !owners["provider-a"] || !owners["provider-b"] {
		t.Fatalf("expected rows from both providers, got %#v", items)
	}
}

// Favorites keep the pre-provider-aware semantics: one row per favorited
// model ID, even when that model ID is offered by multiple providers. Recent
// and Catalog must also skip any model ID already surfaced under Favorites,
// so a favorited model doesn't reappear in a second group. Recent/Catalog
// still de-dup among themselves by provider+model so cross-provider rows stay
// distinct.
func TestAssembleModelPickerItemsFavoritesDedupByModelIDAndHideFromOtherGroups(t *testing.T) {
	m := model{favoriteModels: map[string]bool{"shared-model": true}}
	recent := []pickerItem{
		{Value: "shared-model", OwnerProvider: "provider-a"},
		{Value: "shared-model", OwnerProvider: "provider-b"},
	}
	catalog := []pickerItem{
		{Value: "shared-model", OwnerProvider: "provider-a"},
		{Value: "shared-model", OwnerProvider: "provider-c"},
		{Value: "other-model", OwnerProvider: "provider-a"},
	}
	items := m.assembleModelPickerItems(recent, catalog)

	var favorites []pickerItem
	for _, item := range items {
		if item.Group == "Favorites" {
			favorites = append(favorites, item)
		}
	}
	if len(favorites) != 1 {
		t.Fatalf("expected exactly one Favorites row for shared-model, got %#v", favorites)
	}
	// The deduped Favorites row keeps whichever occurrence comes first in
	// recent+catalog order — here provider-a from recent[0]. Pin that down
	// explicitly rather than leaving it implicit.
	if favorites[0].OwnerProvider != "provider-a" {
		t.Fatalf("expected surviving favorite to keep first-seen provider %q, got %#v", "provider-a", favorites[0])
	}
	for _, item := range items {
		if item.Group != "Favorites" && item.Value == "shared-model" {
			t.Fatalf("favorited model id leaked into %s: %#v", item.Group, item)
		}
	}
	var otherCatalog []pickerItem
	for _, item := range items {
		if item.Group != "Favorites" && item.Group != "Recent" && item.Value == "other-model" {
			otherCatalog = append(otherCatalog, item)
		}
	}
	if len(otherCatalog) != 1 {
		t.Fatalf("expected other-model to appear once in Catalog, got %#v", items)
	}
}

// registryModelPickerItem must tag rows with OwnerProvider so
// pickerItemDedupKey keys them by provider+model instead of falling back to
// Value alone. This matters most for the plain-registry fallback path (no
// saved providers resolved any models — newModelPicker lists the full
// registry directly via registryModelPickerItem), where two registry entries
// sharing a model id but offered by different providers would otherwise
// collide and silently drop one row.
func TestRegistryModelPickerItemSetsOwnerProvider(t *testing.T) {
	entry := testModelEntry("shared-model", 128000, nil)
	entry.Provider = modelregistry.ProviderAnthropic
	item := registryModelPickerItem(entry, "Catalog")
	if item.OwnerProvider != string(modelregistry.ProviderAnthropic) {
		t.Fatalf("expected OwnerProvider %q, got %q", modelregistry.ProviderAnthropic, item.OwnerProvider)
	}

	// Two catalog rows for the same model id from different providers must
	// survive assembleModelPickerItems as distinct rows, not collide on Value.
	entryA := testModelEntry("shared-model", 128000, nil)
	entryA.Provider = modelregistry.ProviderAnthropic
	entryB := testModelEntry("shared-model", 128000, nil)
	entryB.Provider = modelregistry.ProviderOpenAI
	catalog := []pickerItem{
		registryModelPickerItem(entryA, "Catalog"),
		registryModelPickerItem(entryB, "Catalog"),
	}
	m := model{}
	items := m.assembleModelPickerItems(nil, catalog)
	if len(items) != 2 {
		t.Fatalf("expected both provider rows to survive de-dup, got %#v", items)
	}
}

// End-to-end: the picker's "Recent" section shows the active model plus prior
// history, newest first, with the active entry deduped against a stale copy
// already present in history.
func TestModelPickerRecentSectionShowsHistoryPastTheActiveModel(t *testing.T) {
	m := newModel(context.Background(), Options{
		ProviderName: "openrouter",
		ModelName:    "google/gemini-2.5-pro",
		ProviderProfile: config.ProviderProfile{
			Name:      "openrouter",
			CatalogID: "openrouter",
			Model:     "google/gemini-2.5-pro",
			APIKeyEnv: "OPENROUTER_API_KEY",
			Provider:  string(config.ProviderKindOpenAICompatible),
			BaseURL:   "https://openrouter.ai/api/v1",
			APIFormat: "chat-completions",
		},
		RecentModels: []config.RecentModelEntry{
			{Provider: "openrouter", Model: "minimax/minimax-m2.1"},
			{Provider: "openrouter", Model: "google/gemini-2.5-pro"},
		},
	})

	picker := m.newModelPicker()
	if picker == nil {
		t.Fatal("expected a model picker")
	}
	recentValues := []string{}
	for _, item := range picker.items {
		if item.Group != "Recent" {
			break
		}
		recentValues = append(recentValues, item.Value)
	}
	want := []string{"google/gemini-2.5-pro", "minimax/minimax-m2.1"}
	if !reflect.DeepEqual(recentValues, want) {
		t.Fatalf("Recent section values = %#v, want %#v (active pinned first, history deduped)", recentValues, want)
	}
}

// Typed /model switches must record + persist recent history, moving a
// re-selected pair back to the front rather than leaving a stale duplicate.
func TestModelCommandRecordsAndPersistsRecentHistory(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")
	m := newModel(context.Background(), Options{
		UserConfigPath: configPath,
		ProviderName:   "openrouter",
		ModelName:      "google/gemini-2.5-pro",
		Provider:       &fakeProvider{},
		ProviderProfile: config.ProviderProfile{
			Name:      "openrouter",
			CatalogID: "openrouter",
			Model:     "google/gemini-2.5-pro",
			APIKeyEnv: "OPENROUTER_API_KEY",
			Provider:  string(config.ProviderKindOpenAICompatible),
			BaseURL:   "https://openrouter.ai/api/v1",
			APIFormat: "chat-completions",
		},
		NewProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			return &fakeProvider{}, nil
		},
	})

	m.input.SetValue("/model anthropic/claude-sonnet-4.5")
	updated, _ := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if m.modelName != "anthropic/claude-sonnet-4.5" {
		t.Fatalf("modelName = %q, want the switch to apply", m.modelName)
	}

	m.input.SetValue("/model minimax/minimax-m2.1")
	updated, _ = m.Update(testKey(tea.KeyEnter))
	m = updated.(model)

	want := []config.RecentModelEntry{
		{Provider: "openrouter", Model: "minimax/minimax-m2.1"},
		{Provider: "openrouter", Model: "anthropic/claude-sonnet-4.5"},
		{Provider: "openrouter", Model: "google/gemini-2.5-pro"},
	}
	if !reflect.DeepEqual(m.recentModels, want) {
		t.Fatalf("recentModels = %#v, want %#v", m.recentModels, want)
	}
	persisted := readTUIConfigFixture(t, configPath)
	if !reflect.DeepEqual(persisted.Preferences.RecentModels, want) {
		t.Fatalf("persisted RecentModels = %#v, want %#v", persisted.Preferences.RecentModels, want)
	}

	// Re-selecting the oldest pair moves it to the front instead of leaving a
	// stale duplicate further down the list.
	m.input.SetValue("/model google/gemini-2.5-pro")
	updated, _ = m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	want = []config.RecentModelEntry{
		{Provider: "openrouter", Model: "google/gemini-2.5-pro"},
		{Provider: "openrouter", Model: "minimax/minimax-m2.1"},
		{Provider: "openrouter", Model: "anthropic/claude-sonnet-4.5"},
	}
	if !reflect.DeepEqual(m.recentModels, want) {
		t.Fatalf("recentModels after re-selecting = %#v, want %#v", m.recentModels, want)
	}
	// Also check the persisted copy for this reorder/dedupe case (the trickiest
	// path): if recordRecentModel(s)'s in-memory reordering ever diverges from
	// SetRecentModels/NormalizeRecentModels' disk-write semantics, this is the
	// case most likely to expose it.
	persisted = readTUIConfigFixture(t, configPath)
	if !reflect.DeepEqual(persisted.Preferences.RecentModels, want) {
		t.Fatalf("persisted RecentModels after re-selecting = %#v, want %#v", persisted.Preferences.RecentModels, want)
	}
}

// switchProviderModel (the picker's cross-provider path) must also record
// history, tagged with the provider actually switched to.
func TestSwitchProviderModelRecordsRecentHistory(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")
	if _, err := config.UpsertProvider(configPath, config.ProviderProfile{Name: "openai", CatalogID: "openai", Model: "gpt-5.1"}, true); err != nil {
		t.Fatalf("seed openai provider: %v", err)
	}
	if _, err := config.UpsertProvider(configPath, config.ProviderProfile{Name: "ollama", CatalogID: "ollama", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "http://localhost:11434/v1", Model: "kimi-k2.7-code:cloud"}, false); err != nil {
		t.Fatalf("seed ollama provider: %v", err)
	}
	m := newModel(context.Background(), Options{
		UserConfigPath:  configPath,
		ProviderName:    "openai",
		ModelName:       "gpt-5.1",
		Provider:        &fakeProvider{},
		ProviderProfile: config.ProviderProfile{Name: "openai", CatalogID: "openai", Model: "gpt-5.1"},
		SavedProviders: []config.ProviderProfile{
			{Name: "openai", CatalogID: "openai", Model: "gpt-5.1"},
			{Name: "ollama", CatalogID: "ollama", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "http://localhost:11434/v1", Model: "kimi-k2.7-code:cloud"},
		},
		NewProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return &fakeProvider{}, nil
		},
	})

	next, status, _, _, _ := m.switchProviderModel("ollama", "kimi-k2.7-code:cloud")
	wantStatus := "Model\nSwitched to ollama · kimi-k2.7-code:cloud · saved"
	if status != wantStatus {
		t.Fatalf("switchProviderModel() status = %q, want %q (a mismatch here means the switch itself failed, not the recentModels assertion below)", status, wantStatus)
	}

	want := []config.RecentModelEntry{
		{Provider: "ollama", Model: "kimi-k2.7-code:cloud"},
		{Provider: "openai", Model: "gpt-5.1"},
	}
	if !reflect.DeepEqual(next.recentModels, want) {
		t.Fatalf("recentModels = %#v, want %#v", next.recentModels, want)
	}
	persisted := readTUIConfigFixture(t, configPath)
	if !reflect.DeepEqual(persisted.Preferences.RecentModels, want) {
		t.Fatalf("persisted RecentModels = %#v, want %#v", persisted.Preferences.RecentModels, want)
	}
}

func TestSwitchProviderModelReportsLockedConfigAsNotSavedWithoutPartialSelection(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")
	if _, err := config.UpsertProvider(configPath, config.ProviderProfile{Name: "openai", CatalogID: "openai", Model: "gpt-5.1"}, true); err != nil {
		t.Fatalf("seed openai provider: %v", err)
	}
	if _, err := config.UpsertProvider(configPath, config.ProviderProfile{
		Name: "ollama", CatalogID: "ollama", ProviderKind: config.ProviderKindOpenAICompatible,
		BaseURL: "http://localhost:11434/v1", Model: "old-model",
	}, false); err != nil {
		t.Fatalf("seed ollama provider: %v", err)
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read seeded config: %v", err)
	}
	unlock, err := config.LockFile(configPath)
	if err != nil {
		t.Fatalf("hold config lock: %v", err)
	}
	t.Cleanup(func() {
		if err := unlock(); err != nil {
			t.Errorf("release config lock: %v", err)
		}
	})

	t.Setenv("ZERO_PROVIDER", "openai")
	m := newModel(context.Background(), Options{
		UserConfigPath:  configPath,
		ProviderName:    "openai",
		ModelName:       "gpt-5.1",
		Provider:        &fakeProvider{},
		ProviderProfile: config.ProviderProfile{Name: "openai", CatalogID: "openai", Model: "gpt-5.1"},
		SavedProviders: []config.ProviderProfile{
			{Name: "openai", CatalogID: "openai", Model: "gpt-5.1"},
			{Name: "ollama", CatalogID: "ollama", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "http://localhost:11434/v1", Model: "old-model"},
		},
		NewProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return &fakeProvider{}, nil
		},
	})

	started := time.Now()
	next, status, switched, _, persistErr := m.switchProviderModel("ollama", "new-model")
	if elapsed := time.Since(started); elapsed > 12*time.Second {
		t.Fatalf("cross-provider switch blocked for %s; want one bounded persistence attempt", elapsed)
	}
	if !switched || next.providerName != "ollama" || next.modelName != "new-model" {
		t.Fatalf("in-session switch was lost: switched=%v provider=%q model=%q", switched, next.providerName, next.modelName)
	}
	if persistErr == nil || !strings.Contains(persistErr.Error(), "timed out acquiring config lock") {
		t.Fatalf("persistence error = %v, want config lock timeout", persistErr)
	}
	if !strings.Contains(status, "not saved (") || !strings.Contains(status, "timed out acquiring config lock") {
		t.Fatalf("switch status = %q, want concrete not-saved lock failure", status)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config after failed switch: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("failed switch partially changed config\nbefore: %s\nafter: %s", before, after)
	}
}

func TestCrossProviderPickerShowsPersistenceFailure(t *testing.T) {
	root := t.TempDir()
	blockedParent := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blockedParent, []byte("block config directory creation"), 0o600); err != nil {
		t.Fatalf("write blocking parent: %v", err)
	}
	m := newModel(context.Background(), Options{
		UserConfigPath:  filepath.Join(blockedParent, "config.json"),
		ProviderName:    "openai",
		ModelName:       "gpt-5.1",
		Provider:        &fakeProvider{},
		ProviderProfile: config.ProviderProfile{Name: "openai", CatalogID: "openai", Model: "gpt-5.1"},
		SavedProviders: []config.ProviderProfile{
			{Name: "openai", CatalogID: "openai", Model: "gpt-5.1"},
			{Name: "ollama", CatalogID: "ollama", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "http://localhost:11434/v1", Model: "old-model"},
		},
		NewProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return &fakeProvider{}, nil
		},
	})
	m.picker = &commandPicker{
		kind:  pickerModel,
		items: []pickerItem{{Value: "new-model", OwnerProvider: "ollama"}},
	}

	updated, _ := m.choosePicker()
	next := updated.(model)
	if next.providerName != "ollama" || next.modelName != "new-model" {
		t.Fatalf("in-session picker switch was lost: provider=%q model=%q", next.providerName, next.modelName)
	}
	if !strings.Contains(next.transientNotice.text, "not saved (") {
		t.Fatalf("picker notice = %q, want persistence failure", next.transientNotice.text)
	}
	if next.transientNotice.tone != transientNoticeWarning {
		t.Fatalf("picker notice tone = %v, want warning", next.transientNotice.tone)
	}
}

// TestActiveProviderPickerShowsPersistenceFailure is the same-provider half of
// TestCrossProviderPickerShowsPersistenceFailure. A picker row owned by the
// ACTIVE provider goes through handleModelCommand, which reports a failed
// persistence attempt inside its status text — and choosePicker discards that
// text in favour of its own applied-notice. Without the outcome plumbed through
// as a value, the user saw an unqualified success for a selection the config
// never received, and only found out when the old model came back on restart.
//
// The elapsed-time bound is the second assertion, not decoration: the recent
// model history is written under the same lock, so a version that still tried
// to persist it after the selection write failed would pay TWO full lock
// timeouts for one keystroke.
func TestActiveProviderPickerShowsPersistenceFailure(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")
	if _, err := config.UpsertProvider(configPath, config.ProviderProfile{
		Name: "openrouter", CatalogID: "openrouter", Model: "google/gemini-2.5-pro",
	}, true); err != nil {
		t.Fatalf("seed openrouter provider: %v", err)
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read seeded config: %v", err)
	}
	unlock, err := config.LockFile(configPath)
	if err != nil {
		t.Fatalf("hold config lock: %v", err)
	}
	t.Cleanup(func() {
		if err := unlock(); err != nil {
			t.Errorf("release config lock: %v", err)
		}
	})

	m := newModel(context.Background(), Options{
		UserConfigPath: configPath,
		ProviderName:   "openrouter",
		ModelName:      "google/gemini-2.5-pro",
		Provider:       &fakeProvider{},
		ProviderProfile: config.ProviderProfile{
			Name:      "openrouter",
			CatalogID: "openrouter",
			Model:     "google/gemini-2.5-pro",
			APIKeyEnv: "OPENROUTER_API_KEY",
			Provider:  string(config.ProviderKindOpenAICompatible),
			BaseURL:   "https://openrouter.ai/api/v1",
			APIFormat: "chat-completions",
		},
		NewProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return &fakeProvider{}, nil
		},
	})
	// OwnerProvider left blank: the row resolves against the active provider,
	// which is the branch that reaches handleModelCommand.
	m.picker = &commandPicker{
		kind:  pickerModel,
		items: []pickerItem{{Value: "anthropic/claude-sonnet-4.5"}},
	}

	started := time.Now()
	updated, _ := m.choosePicker()
	elapsed := time.Since(started)
	next := updated.(model)

	if next.modelName != "anthropic/claude-sonnet-4.5" {
		t.Fatalf("in-session picker selection was lost: model=%q", next.modelName)
	}
	if !strings.Contains(next.transientNotice.text, "not saved (") ||
		!strings.Contains(next.transientNotice.text, "timed out acquiring config lock") {
		t.Fatalf("picker notice = %q, want a concrete not-saved lock failure", next.transientNotice.text)
	}
	if next.transientNotice.tone != transientNoticeWarning {
		t.Fatalf("picker notice tone = %v, want warning", next.transientNotice.tone)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("selection blocked for %s; want a single bounded persistence attempt", elapsed)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config after failed selection: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("failed selection changed config\nbefore: %s\nafter: %s", before, after)
	}
}

// TestTypedModelCommandShowsPersistenceFailure is the typed half of the same
// gap: on a committed switch, dispatchCommand also replaces handleModelCommand's
// status text with the applied-notice, so "· not saved (…)" was dropped there
// too. An unwritable config path fails the persistence attempt immediately,
// which keeps this case off the ten-second lock timeout the picker test needs.
func TestTypedModelCommandShowsPersistenceFailure(t *testing.T) {
	root := t.TempDir()
	blockedParent := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blockedParent, []byte("block config directory creation"), 0o600); err != nil {
		t.Fatalf("write blocking parent: %v", err)
	}
	m := newModel(context.Background(), Options{
		UserConfigPath: filepath.Join(blockedParent, "config.json"),
		ProviderName:   "openrouter",
		ModelName:      "google/gemini-2.5-pro",
		Provider:       &fakeProvider{},
		ProviderProfile: config.ProviderProfile{
			Name:      "openrouter",
			CatalogID: "openrouter",
			Model:     "google/gemini-2.5-pro",
			APIKeyEnv: "OPENROUTER_API_KEY",
			Provider:  string(config.ProviderKindOpenAICompatible),
			BaseURL:   "https://openrouter.ai/api/v1",
			APIFormat: "chat-completions",
		},
		NewProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return &fakeProvider{}, nil
		},
	})
	m.input.SetValue("/model anthropic/claude-sonnet-4.5")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if next.modelName != "anthropic/claude-sonnet-4.5" {
		t.Fatalf("in-session switch was lost: model=%q", next.modelName)
	}
	if !strings.Contains(next.transientNotice.text, "not saved (") {
		t.Fatalf("notice = %q, want persistence failure", next.transientNotice.text)
	}
	if next.transientNotice.tone != transientNoticeWarning {
		t.Fatalf("notice tone = %v, want warning", next.transientNotice.tone)
	}
}

func TestModelCommandAcceptsManualModelForCustomProvider(t *testing.T) {
	var captured config.ProviderProfile
	m := newModel(context.Background(), Options{
		ProviderName: "custom-openai-compatible",
		ModelName:    "custom-model",
		Provider:     &fakeProvider{},
		ProviderProfile: config.ProviderProfile{
			Name:         "custom-openai-compatible",
			CatalogID:    "custom-openai-compatible",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://proxy.example.test/v1",
			APIKey:       "proxy-key",
			Model:        "custom-model",
		},
		NewProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			captured = profile
			return &fakeProvider{}, nil
		},
	})
	m.input.SetValue("/model qwen-custom-latest")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if next.pending {
		t.Fatal("expected /model to be handled without starting a run")
	}
	if captured.Model != "qwen-custom-latest" {
		t.Fatalf("captured model = %q, want manual custom model", captured.Model)
	}
	if next.modelName != "qwen-custom-latest" {
		t.Fatalf("active model = %q, want manual custom model", next.modelName)
	}
	if transcriptContains(next.transcript, "unknown Zero model") {
		t.Fatalf("manual custom model should not be rejected, got %#v", next.transcript)
	}
}

func TestModelPickerOpensAndCancels(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "claude-sonnet-4.5"})
	m.input.SetValue("/model")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if cmd != nil {
		t.Fatal("opening the model picker should not start a run")
	}
	if m.picker == nil || m.picker.kind != pickerModel {
		t.Fatalf("expected an open model picker, got %#v", m.picker)
	}

	// Esc cancels the picker without touching the run or transcript.
	updated, _ = m.Update(testKey(tea.KeyEsc))
	m = updated.(model)
	if m.picker != nil {
		t.Fatal("Esc should close the picker")
	}
}

func TestModelPickerNavigatesAndChoosesAppliesHandler(t *testing.T) {
	next := &fakeProvider{}
	m := newModel(context.Background(), Options{
		ProviderName:    "anthropic",
		ModelName:       "claude-sonnet-4.5",
		Provider:        &fakeProvider{},
		ProviderProfile: anthropicTestProfile("claude-sonnet-4.5"),
		NewProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			return next, nil
		},
		DiscoverProviderModels: func(ctx context.Context, profile config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
			return []providermodeldiscovery.Model{
				{ID: "claude-haiku-4.5", Description: "Claude Haiku 4.5"},
			}, nil
		},
	})
	m.input.SetValue("/model")
	updated, cmd := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if m.picker == nil {
		t.Fatal("expected model picker open")
	}
	if cmd == nil {
		t.Fatal("expected opening the model picker to start discovery")
	}
	updated, _ = m.Update(cmd())
	m = updated.(model)

	// Point the picker at a concrete, different model in the same provider family
	// and choose it (cross-provider switches require a matching profile).
	target := -1
	for i, item := range m.picker.items {
		if item.Value == "claude-haiku-4.5" {
			target = i
			break
		}
	}
	if target < 0 {
		t.Fatal("expected claude-haiku-4.5 in the model picker")
	}
	m.picker.selected = target

	updated, _ = m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if m.picker != nil {
		t.Fatal("choosing should close the picker")
	}
	if m.modelName != "claude-haiku-4.5" {
		t.Fatalf("expected model switched to claude-haiku-4.5 via handler, got %q", m.modelName)
	}
	if !strings.Contains(m.transientNotice.text, "Model:") {
		t.Fatalf("model picker notice = %q", m.transientNotice.text)
	}
}

func TestEffortPickerOpensForSupportedModel(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "claude-sonnet-4.5"})
	m.input.SetValue("/effort")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if m.picker == nil || m.picker.kind != pickerEffort {
		t.Fatalf("expected an open effort picker, got %#v", m.picker)
	}
	// "auto" is always offered as the first option.
	if len(m.picker.items) == 0 || m.picker.items[0].Value != "auto" {
		t.Fatalf("expected auto as the first effort option, got %#v", m.picker.items)
	}

	// Choose the highlighted effort; the handler stores the preference.
	for i, item := range m.picker.items {
		if item.Value == "high" {
			m.picker.selected = i
		}
	}
	updated, _ = m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if m.reasoningEffort != "high" {
		t.Fatalf("expected effort applied via handler, got %q", m.reasoningEffort)
	}
}

func TestThemeCommandOpensPicker(t *testing.T) {
	// Bare /theme opens the theme popup (contained preview on move, apply on Enter),
	// like /model and /effort. Full preview/commit/cancel behavior is covered in
	// theme_picker_test.go; here we just pin that the no-arg command opens it.
	defer applyTheme(themeDark, true)
	m := newModel(context.Background(), Options{})
	m.input.SetValue("/theme")
	updated, _ := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if m.picker == nil || m.picker.kind != pickerTheme {
		t.Fatalf("expected the theme picker to open, got %#v", m.picker)
	}
}

func TestPickersRefuseToOpenWhileRunPending(t *testing.T) {
	// A picker opened while a run is in flight would have its selection refused
	// after the run, so opening it at all is misleading. Each no-arg picker command
	// must no-op into a brief "while a run is in progress" message instead.
	cases := []struct {
		name    string
		command string
	}{
		{name: "model", command: "/model"},
		{name: "effort", command: "/effort"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newModel(context.Background(), Options{
				ModelName: "claude-sonnet-4.5",
			})
			m.pending = true
			m.input.SetValue(tc.command)

			updated, cmd := m.Update(testKey(tea.KeyEnter))
			next := updated.(model)
			if cmd != nil {
				t.Fatalf("%s while pending should not start a run", tc.command)
			}
			if next.picker != nil {
				t.Fatalf("%s should not open a picker while a run is in progress, got %#v", tc.command, next.picker)
			}
			if !transcriptContains(next.transcript, "while a run is in progress") {
				t.Fatalf("%s should explain it can't change settings while a run is in progress, got %q", tc.command, transcriptText(next.transcript))
			}
			if !next.pending {
				t.Fatalf("%s must not clear the in-flight run", tc.command)
			}
		})
	}
}

func TestPickerRenders(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "claude-sonnet-4.5"})
	m.width, m.height = 96, 30
	m.input.SetValue("/model")
	updated, _ := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if !strings.Contains(viewString(m.View()), "Choose a model") {
		t.Fatal("view should render the picker title")
	}
}

func TestPickerOverlayCapsVisibleRows(t *testing.T) {
	items := make([]pickerItem, 0, 20)
	for i := range 20 {
		items = append(items, pickerItem{Label: fmt.Sprintf("model-%02d", i), Value: fmt.Sprintf("model-%02d", i)})
	}
	m := newModel(context.Background(), Options{})
	m.picker = &commandPicker{
		kind:     pickerModel,
		title:    "Choose a model",
		items:    items,
		allItems: append([]pickerItem{}, items...),
		selected: 15,
	}

	got := plainRender(t, m.pickerOverlay(120))
	if !strings.Contains(got, "Choose a model") || !strings.Contains(got, "model-15") {
		t.Fatalf("picker overlay should render selected window, got %q", got)
	}
	if strings.Contains(got, "model-00") || strings.Contains(got, "model-09") {
		t.Fatalf("picker overlay should cap visible rows around selection, got %q", got)
	}
}

func pickerValues(items []pickerItem) []string {
	values := make([]string, 0, len(items))
	for _, item := range items {
		values = append(values, item.Value)
	}
	return values
}

func TestModelPickerListsAllSavedProviders(t *testing.T) {
	m := newModel(context.Background(), Options{
		ProviderName:    "openai",
		ModelName:       "gpt-5.1",
		ProviderProfile: config.ProviderProfile{Name: "openai", CatalogID: "openai", Model: "gpt-5.1"},
		SavedProviders: []config.ProviderProfile{
			{Name: "openai", CatalogID: "openai", Model: "gpt-5.1"},
			{Name: "xai", CatalogID: "xai", Model: "grok-4"},
		},
	})
	picker := m.newModelPicker()
	if picker == nil {
		t.Fatal("expected model picker")
	}
	// More than one provider section is listed (not just the active provider).
	if groups := pickerGroups(picker.items); len(groups) < 2 {
		t.Fatalf("expected multiple provider groups, got %#v", groups)
	}
	// Models from both saved providers are present and tagged with their owner so
	// selection can switch providers.
	owners := map[string]bool{}
	for _, item := range picker.items {
		if item.OwnerProvider != "" {
			owners[strings.ToLower(item.OwnerProvider)] = true
		}
	}
	for _, want := range []string{"openai", "xai"} {
		if !owners[want] {
			t.Fatalf("expected models owned by %q; owners=%v", want, owners)
		}
	}
}

func pickerGroups(items []pickerItem) []string {
	groups := []string{}
	seen := map[string]bool{}
	for _, item := range items {
		if item.Group == "" || seen[item.Group] {
			continue
		}
		seen[item.Group] = true
		groups = append(groups, item.Group)
	}
	return groups
}

func pickerIndex(items []pickerItem, value string) int {
	for index, item := range items {
		if item.Value == value {
			return index
		}
	}
	return -1
}

func readTUIConfigFixture(t *testing.T, path string) config.FileConfig {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg config.FileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	return cfg
}

func TestEffortPickerOpensForModelWithoutEffortControls(t *testing.T) {
	// glm-5.1 is not in the hard-coded registry, so availableReasoningEfforts is
	// empty. /effort should still open a picker (offering auto only) instead of
	// rendering a static "Effort / available: none for active model" status card.
	m := newModel(context.Background(), Options{ModelName: "glm-5.1"})
	m.input.SetValue("/effort")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if m.picker == nil || m.picker.kind != pickerEffort {
		t.Fatalf("expected an open effort picker, got %#v", m.picker)
	}
	if len(m.picker.items) != 1 || m.picker.items[0].Value != "auto" {
		t.Fatalf("expected [auto] as the only effort option on an unsupported model, got %#v", m.picker.items)
	}
	if m.picker.title != "select reasoning effort" {
		t.Fatalf("picker title = %q, want %q", m.picker.title, "select reasoning effort")
	}
}

func TestEffortPickerPrefersLiveProviderReasoningLevels(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "future-reasoner"})
	m.providerProfile = config.ProviderProfile{CatalogID: "test-provider"}
	m.modelPickerLiveByProvider = map[string][]providermodeldiscovery.Model{
		"test-provider": {{
			ID:               "future-reasoner",
			Reasoning:        true,
			ReasoningEfforts: []string{"low", "high", "xhigh", "max", "ultra", "unknown", "high"},
		}},
	}

	got := m.availableReasoningEfforts()
	want := []modelregistry.ReasoningEffort{
		modelregistry.ReasoningEffortLow,
		modelregistry.ReasoningEffortHigh,
		modelregistry.ReasoningEffortXHigh,
		modelregistry.ReasoningEffortMax,
		modelregistry.ReasoningEffortUltra,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reasoning efforts = %#v, want %#v", got, want)
	}
}

func TestEffortPickerDoesNotFallbackWhenLiveModelAdvertisesNoSelectableEfforts(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-5.5"})
	m.providerProfile = config.ProviderProfile{CatalogID: "test-provider"}
	m.modelPickerLiveByProvider = map[string][]providermodeldiscovery.Model{
		"test-provider": {{ID: "gpt-5.5", ReasoningEfforts: []string{"none"}}},
	}

	if got := m.availableReasoningEfforts(); len(got) != 0 {
		t.Fatalf("live model with only none exposed efforts %#v, want none", got)
	}
}

func TestEffortPickerAutoSelectionKeepsEffortUnset(t *testing.T) {
	// Picking "auto" on a model without effort controls clears any stale
	// preference and emits the success status text (handleEffortCommand("auto")).
	m := newModel(context.Background(), Options{ModelName: "glm-5.1"})
	m.reasoningEffort = modelregistry.ReasoningEffortHigh
	m.input.SetValue("/effort")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if m.picker == nil {
		t.Fatal("expected the effort picker to open")
	}

	updated, _ = m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if m.picker != nil {
		t.Fatal("enter should close the picker")
	}
	if m.reasoningEffort != "" {
		t.Fatalf("auto selection should clear reasoning effort, got %q", m.reasoningEffort)
	}
}

func TestModelContextWindowResolution(t *testing.T) {
	// Registered model → the exact registry window (compare to the registry value so
	// this doesn't break on a benign catalog update).
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		t.Fatalf("load default registry: %v", err)
	}
	m := model{modelCatalog: registry}
	entry, ok := registry.Resolve("gpt-4.1")
	if !ok || entry.ContextLimits.ContextWindow <= 0 {
		t.Fatalf("gpt-4.1 should resolve a positive window in the registry")
	}
	if got := m.modelContextWindow("gpt-4.1"); got != entry.ContextLimits.ContextWindow {
		t.Fatalf("registered model window = %d, want %d", got, entry.ContextLimits.ContextWindow)
	}
	// Unknown model → 0 (display shows no denominator); compaction applies its own
	// fallback via AgentContextWindow.
	if got := m.modelContextWindow("totally-unknown-proxy-model"); got != 0 {
		t.Fatalf("unknown model should resolve 0 for display, got %d", got)
	}
	if got := modelregistry.AgentContextWindow(m.modelContextWindow("totally-unknown-proxy-model")); got != modelregistry.FallbackContextWindow {
		t.Fatalf("compaction window for unknown model should be %d, got %d", modelregistry.FallbackContextWindow, got)
	}
	// Live-discovered window wins for an unregistered model.
	m.modelPickerLiveByProvider = map[string][]providermodeldiscovery.Model{
		"xai": {{ID: "grok-4", ContextWindow: 256000}},
	}
	if got := m.modelContextWindow("grok-4"); got != 256000 {
		t.Fatalf("discovered window should win for unregistered model, got %d", got)
	}
	// Empty name → 0 (no window).
	if got := m.modelContextWindow(""); got != 0 {
		t.Fatalf("empty model name should yield 0, got %d", got)
	}
	// A custom/local Ollama model tag has no curated-catalog entry and its
	// generic /v1/models listing carries no window either — the only source is
	// the Ollama-specific /api/show probe, landed via ollamaContextWindowByModel.
	m.ollamaContextWindowByModel = map[string]int{"kimi-k2.7-code:cloud": 131072}
	if got := m.modelContextWindow("kimi-k2.7-code:cloud"); got != 131072 {
		t.Fatalf("ollama-discovered window should be used for an unregistered model, got %d", got)
	}
}

// TestOllamaContextWindowDiscoveryCmdScopedToLocalOllama: the /api/show probe
// only makes sense against a local Ollama daemon (catalog ID "ollama") — a
// different provider like "ollama-cloud" is a distinct hosted service this
// endpoint isn't assumed to exist on, and every other provider already has
// its context window covered by the curated catalog or /v1/models discovery.
func TestOllamaContextWindowDiscoveryCmdScopedToLocalOllama(t *testing.T) {
	m := model{ctx: context.Background()}

	ollama, ok := providercatalog.Get("ollama")
	if !ok {
		t.Fatal("expected \"ollama\" to be a registered catalog descriptor")
	}
	if cmd := m.ollamaContextWindowDiscoveryCmd(ollama, "http://localhost:11434/v1", "kimi-k2.7-code:cloud"); cmd == nil {
		t.Fatal("expected a discovery command for the local ollama provider")
	}

	ollamaCloud, ok := providercatalog.Get("ollama-cloud")
	if !ok {
		t.Fatal("expected \"ollama-cloud\" to be a registered catalog descriptor")
	}
	if cmd := m.ollamaContextWindowDiscoveryCmd(ollamaCloud, "https://ollama.com/v1", "qwen3-coder:480b"); cmd != nil {
		t.Fatal("ollama-cloud is a different hosted service; must not probe /api/show against it")
	}

	if cmd := m.ollamaContextWindowDiscoveryCmd(ollama, "", "kimi-k2.7-code:cloud"); cmd != nil {
		t.Fatal("an empty base URL should yield no command")
	}
	if cmd := m.ollamaContextWindowDiscoveryCmd(ollama, "http://localhost:11434/v1", ""); cmd != nil {
		t.Fatal("an empty model name should yield no command")
	}
}

// TestOllamaContextWindowDiscoveredMsgPopulatesMap verifies the Update()
// handler for the async probe's result actually reaches modelContextWindow's
// fallback source, and that a failed/zero probe leaves the map untouched
// rather than caching a bogus zero.
func TestOllamaContextWindowDiscoveredMsgPopulatesMap(t *testing.T) {
	m := newModel(context.Background(), Options{})

	updated, cmd := m.Update(ollamaContextWindowDiscoveredMsg{modelName: "kimi-k2.7-code:cloud", contextWindow: 131072})
	next := updated.(model)
	if cmd != nil {
		t.Fatal("applying a discovered window should not schedule further work")
	}
	if got := next.modelContextWindow("kimi-k2.7-code:cloud"); got != 131072 {
		t.Fatalf("modelContextWindow after discovery = %d, want 131072", got)
	}

	updated, _ = next.Update(ollamaContextWindowDiscoveredMsg{modelName: "other-model", contextWindow: 0, err: errors.New("boom")})
	next = updated.(model)
	if got := next.modelContextWindow("other-model"); got != 0 {
		t.Fatalf("a failed probe must not populate a window, got %d", got)
	}
}

// TestSwitchProviderModelWarmsDiscoveryForTheNewProvider: switching providers
// mid-session (e.g. via the /model picker) previously fired no discovery at
// all for the newly active provider — Init() only warms the provider active
// at launch — so the context gauge stayed blank for the rest of the session
// unless /model happened to be reopened separately. Switching to Ollama
// specifically must also warm the /api/show probe, the only source of a
// context window for custom/local Ollama models.
func TestSwitchProviderModelWarmsDiscoveryForTheNewProvider(t *testing.T) {
	m := newModel(context.Background(), Options{
		ProviderName:    "openai",
		ModelName:       "gpt-5.1",
		Provider:        &fakeProvider{},
		ProviderProfile: config.ProviderProfile{Name: "openai", CatalogID: "openai", Model: "gpt-5.1"},
		SavedProviders: []config.ProviderProfile{
			{Name: "openai", CatalogID: "openai", Model: "gpt-5.1"},
			{Name: "ollama", CatalogID: "ollama", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "http://localhost:11434/v1", Model: "kimi-k2.7-code:cloud"},
		},
		NewProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return &fakeProvider{}, nil
		},
	})

	next, text, ok, cmd, _ := m.switchProviderModel("ollama", "kimi-k2.7-code:cloud")
	if !ok || !strings.Contains(text, "Switched to ollama") {
		t.Fatalf("switch notice = %q (ok=%v), want a committed switch", text, ok)
	}
	if next.modelName != "kimi-k2.7-code:cloud" || next.providerName != "ollama" {
		t.Fatalf("model/provider not switched: modelName=%q providerName=%q", next.modelName, next.providerName)
	}
	if cmd == nil {
		t.Fatal("switching to ollama should warm both the generic and ollama-specific discovery commands")
	}
}

func TestNormalizeProfileForProviderPreservesCustomName(t *testing.T) {
	openai, ok := providercatalog.Get("openai")
	if !ok {
		t.Fatal("openai descriptor missing from catalog")
	}

	// A first-party provider set up with a custom profile name and the baseURL at
	// the catalog default. normalizeIdentity is true here (baseURL matches), but
	// the real name must survive: it is the credential-store key, so clobbering it
	// dropped the saved key and 401'd every /model switch (issue #440).
	m := model{providerProfile: config.ProviderProfile{
		Name:         "my-openai",
		CatalogID:    "openai",
		BaseURL:      openai.DefaultBaseURL,
		APIKeyStored: true,
	}}
	got := m.normalizeProfileForProvider(openai)
	if got.Name != "my-openai" {
		t.Fatalf("Name = %q, want it preserved as %q (credential store is keyed by Name)", got.Name, "my-openai")
	}
	if got.CatalogID != "openai" {
		t.Fatalf("CatalogID = %q, want it canonicalized to %q", got.CatalogID, "openai")
	}
}

func TestNormalizeProfileForProviderCanonicalizesPlaceholderName(t *testing.T) {
	openai, ok := providercatalog.Get("openai")
	if !ok {
		t.Fatal("openai descriptor missing from catalog")
	}

	// Empty and generic placeholder names still get canonicalized to the catalog
	// id — those carry no meaningful credential-store key to preserve.
	for _, name := range []string{"", "custom-openai-compatible"} {
		m := model{providerProfile: config.ProviderProfile{
			Name:      name,
			BaseURL:   openai.DefaultBaseURL,
			CatalogID: name,
		}}
		got := m.normalizeProfileForProvider(openai)
		if got.Name != "openai" {
			t.Fatalf("Name = %q for placeholder %q, want canonicalized to %q", got.Name, name, "openai")
		}
	}
}

// TestSwitchProviderModelUsesOAuthLoginWithoutInliningBearer: switching to a
// token-login provider (ChatGPT) must pass the credential gate on the stored
// OAuth login and hand newProvider a KEYLESS profile. Inlining the bearer as
// APIKey makes HasConfiguredCredential true, which strips the OAuth login
// candidates at build time — the client is then constructed without the bearer
// resolver and login key, dropping token refresh and the Codex
// `chatgpt-account-id` header, so every call 401s.
func TestSwitchProviderModelUsesOAuthLoginWithoutInliningBearer(t *testing.T) {
	tokensPath := filepath.Join(t.TempDir(), "oauth-tokens.json")
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", tokensPath)
	store, err := oauth.NewStore(oauth.StoreOptions{})
	if err != nil {
		t.Fatalf("oauth store: %v", err)
	}
	if err := store.Save(oauth.ProviderKey("chatgpt"), oauth.Token{AccessToken: "bearer-123", TokenType: "Bearer"}); err != nil {
		t.Fatalf("save token: %v", err)
	}

	var built config.ProviderProfile
	m := newModel(context.Background(), Options{
		ProviderName:    "opengateway",
		ModelName:       "some-model",
		Provider:        &fakeProvider{},
		ProviderProfile: config.ProviderProfile{Name: "opengateway", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://gateway.example.com/v1", Model: "some-model"},
		SavedProviders: []config.ProviderProfile{
			{Name: "opengateway", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://gateway.example.com/v1", Model: "some-model"},
			{Name: "chatgpt", CatalogID: "chatgpt", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://chatgpt.com/backend-api/codex", Model: "gpt-5.5"},
		},
		NewProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			built = profile
			return &fakeProvider{}, nil
		},
	})

	next, text, ok, _, _ := m.switchProviderModel("chatgpt", "gpt-5.5")
	if !ok || !strings.Contains(text, "Switched to chatgpt") {
		t.Fatalf("switch should succeed on the stored OAuth login, got %q (ok=%v)", text, ok)
	}
	if next.providerName != "chatgpt" || next.modelName != "gpt-5.5" {
		t.Fatalf("switch did not commit: provider=%q model=%q", next.providerName, next.modelName)
	}
	if built.APIKey != "" {
		t.Fatalf("OAuth bearer must not be inlined as APIKey (suppresses the runtime resolver), got %q", built.APIKey)
	}
}

// TestSwitchProviderModelStillRejectsProviderWithNoCredential: with no key, no
// header, no local runtime, and no OAuth login, the gate must keep refusing.
func TestSwitchProviderModelStillRejectsProviderWithNoCredential(t *testing.T) {
	tokensPath := filepath.Join(t.TempDir(), "oauth-tokens.json")
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", tokensPath)

	m := newModel(context.Background(), Options{
		ProviderName:    "opengateway",
		ModelName:       "some-model",
		Provider:        &fakeProvider{},
		ProviderProfile: config.ProviderProfile{Name: "opengateway", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://gateway.example.com/v1", Model: "some-model"},
		SavedProviders: []config.ProviderProfile{
			{Name: "opengateway", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://gateway.example.com/v1", Model: "some-model"},
			{Name: "chatgpt", CatalogID: "chatgpt", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://chatgpt.com/backend-api/codex", Model: "gpt-5.5"},
		},
		NewProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			t.Fatal("newProvider must not run for a credential-less provider")
			return nil, nil
		},
	})

	_, text, ok, _, _ := m.switchProviderModel("chatgpt", "gpt-5.5")
	if ok || !strings.Contains(text, "no usable credential") {
		t.Fatalf("expected the credential gate to refuse, got %q (ok=%v)", text, ok)
	}
}
