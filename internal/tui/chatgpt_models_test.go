package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/modelregistry"
	"github.com/Twigpine/zero/internal/oauth"
	"github.com/Twigpine/zero/internal/providercatalog"
	"github.com/Twigpine/zero/internal/providermodeldiscovery"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

func isolateSubscriptionTest(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME"} {
		t.Setenv(name, root)
	}
	t.Setenv("ZERO_OAUTH_STORAGE", "file")
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", filepath.Join(root, "oauth.json"))
	t.Setenv("ZERO_DISABLE_MODELS_FETCH", "1")
}

func subscriptionTestModel(t *testing.T) model {
	t.Helper()
	isolateSubscriptionTest(t)
	return newModel(context.Background(), Options{
		Cwd: t.TempDir(), UserConfigPath: filepath.Join(t.TempDir(), "config.json"),
		ProviderName: "subscription", ModelName: "gpt-existing",
		ProviderProfile: config.ProviderProfile{
			Name: "subscription", CatalogID: "chatgpt", Model: "gpt-existing",
			ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "https://chatgpt.com/backend-api/codex",
		},
	})
}

func TestSubscriptionModelListIncludesDiscoveredModels(t *testing.T) {
	m := subscriptionTestModel(t)
	m = m.applyModelPickerModelsDiscovered(modelPickerModelsDiscoveredMsg{
		providerID: "chatgpt", models: []providermodeldiscovery.Model{{ID: "gpt-future-coder", Description: "Future coder"}},
	})
	text := m.modelListText()
	for _, want := range []string{"gpt-future-coder", "subscription", "live"} {
		if !strings.Contains(text, want) {
			t.Fatalf("/model list lacks %q after live discovery:\n%s", want, text)
		}
	}
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if target, ok := m.resolveModelSwitchTarget(registry, "gpt-future-coder"); !ok || target.modelID != "gpt-future-coder" {
		t.Fatal("listed subscription model is not selectable")
	}
}

func TestSubscriptionPickerReportsDiscoveryFailureAndRecovery(t *testing.T) {
	m := subscriptionTestModel(t)
	m.picker = m.newModelPicker()
	m = m.applyModelPickerModelsDiscovered(modelPickerModelsDiscoveredMsg{
		providerID: "chatgpt", err: errors.New("expired login"),
	})
	text := m.modelPickerOverlay(100)
	for _, want := range []string{"expired login", "fallback", "Ctrl+R"} {
		if !strings.Contains(text, want) {
			t.Fatalf("picker hides %q after discovery failure:\n%s", want, text)
		}
	}
	m = m.applyModelPickerModelsDiscovered(modelPickerModelsDiscoveredMsg{
		providerID: "chatgpt", models: []providermodeldiscovery.Model{{ID: "gpt-future-coder"}},
	})
	if text := m.modelPickerOverlay(100); strings.Contains(text, "expired login") || strings.Contains(text, "unverified") {
		t.Fatalf("successful refresh kept fallback warning:\n%s", text)
	}
}

func TestSubscriptionFallbackSuggestionsAreUnverified(t *testing.T) {
	m := subscriptionTestModel(t)
	m.picker = m.newModelPicker()
	if text := m.modelPickerOverlay(110); !strings.Contains(text, "unverified") {
		t.Fatalf("subscription fallback is not labeled unverified:\n%s", text)
	}
}

func TestSubscriptionModelListRefreshesWithoutOpeningPicker(t *testing.T) {
	m := subscriptionTestModel(t)
	m.discoverProviderModels = func(_ context.Context, profile config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
		if profile.Name != "subscription" || profile.CatalogID != "chatgpt" {
			t.Fatalf("discovery lost profile identity: %#v", profile)
		}
		return []providermodeldiscovery.Model{{ID: "gpt-future-coder"}}, nil
	}
	m.input.SetValue("/model list")
	updated, cmd := m.Update(testKey(tea.KeyEnter))
	if cmd == nil {
		t.Fatal("/model list must discover the subscription even before /model is opened")
	}
	updated, _ = updated.(model).Update(cmd())
	next := updated.(model)
	if !transcriptContains(next.transcript, "gpt-future-coder") || next.providerName != "subscription" {
		t.Fatal("/model list omitted the refreshed model or changed providers")
	}
}

func TestSubscriptionDiscoveredModelSwitchKeepsOAuthProvider(t *testing.T) {
	m := subscriptionTestModel(t)
	m.userConfigPath = "" // This fixture models an unsaved session profile.
	m.modelPickerLiveByProvider = map[string][]providermodeldiscovery.Model{
		"subscription": {{ID: "gpt-future-coder"}},
	}
	var rebuilt config.ProviderProfile
	m.newProvider = func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
		rebuilt = profile
		return &fakeProvider{}, nil
	}
	next, _, err := m.handleModelCommand("openai/gpt-future-coder")
	if err != nil {
		t.Fatal(err)
	}
	if next.modelName != "gpt-future-coder" || next.providerName != "subscription" || rebuilt.CatalogID != "chatgpt" || rebuilt.BaseURL != m.providerProfile.BaseURL {
		t.Fatalf("switch lost its subscription route: model=%q provider=%q profile=%#v", next.modelName, next.providerName, rebuilt)
	}
}

func TestSubscriptionPickerKeepsAccountErrorsIndependent(t *testing.T) {
	m := subscriptionTestModel(t)
	m.savedProviders = []config.ProviderProfile{m.providerProfile, {
		Name: "second-account", CatalogID: "chatgpt", BaseURL: m.providerProfile.BaseURL,
		ProviderKind: config.ProviderKindOpenAICompatible,
	}}
	m.picker = m.newModelPicker()
	m = m.applyModelPickerModelsDiscovered(modelPickerModelsDiscoveredMsg{
		providerID: "chatgpt", endpointKey: "subscription", err: errors.New("expired subscription"),
	})
	m = m.applyModelPickerModelsDiscovered(modelPickerModelsDiscoveredMsg{
		providerID: "chatgpt", endpointKey: "second-account", models: []providermodeldiscovery.Model{{ID: "gpt-second-account"}},
	})
	if !strings.Contains(m.modelPickerOverlay(100), "expired subscription") {
		t.Fatal("another account's success cleared the first account's failure")
	}
}

func TestSubscriptionModelsDoNotCrossAccountProfiles(t *testing.T) {
	m := subscriptionTestModel(t)
	m = m.applyModelPickerModelsDiscovered(modelPickerModelsDiscoveredMsg{
		providerID: "chatgpt", endpointKey: "second-account", models: []providermodeldiscovery.Model{{ID: "gpt-second-account"}},
	})
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.resolveModelSwitchTarget(registry, "gpt-second-account"); ok {
		t.Fatal("another account's entitlement leaked into the active account")
	}
}

func TestSubscriptionPickerRetriesFromKeyboard(t *testing.T) {
	m := subscriptionTestModel(t)
	m.picker = m.newModelPicker()
	m = m.applyModelPickerModelsDiscovered(modelPickerModelsDiscoveredMsg{
		providerID: "chatgpt", endpointKey: "subscription", err: errors.New("expired subscription"),
	})
	m.discoverProviderModels = func(context.Context, config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
		return []providermodeldiscovery.Model{{ID: "gpt-refreshed"}}, nil
	}
	updated, cmd := m.Update(testKeyCtrl('r'))
	if cmd == nil || updated.(model).picker.query != "" {
		t.Fatal("Ctrl+R must retry discovery without changing the search")
	}
	updated, _ = updated.(model).Update(cmd())
	next := updated.(model)
	if strings.Contains(next.modelPickerOverlay(100), "expired subscription") || !contains(pickerValues(next.picker.items), "gpt-refreshed") {
		t.Fatal("successful keyboard retry did not refresh models and clear its account's error")
	}
}

func TestSubscriptionPickerRedactsDiscoveryErrors(t *testing.T) {
	const secret = "opaque-subscription-bearer"
	for _, split := range []string{"", "\x00", "\x1b", "\u0085", "\x1b[31m"} {
		t.Run(fmt.Sprintf("split-%x", split), func(t *testing.T) {
			m := subscriptionTestModel(t)
			m.picker = m.newModelPicker()
			m = m.applyModelPickerModelsDiscovered(modelPickerModelsDiscoveredMsg{
				providerID: "chatgpt", err: errors.New("request failed with opaque-" + split + "subscription-bearer"), secrets: []string{secret},
			})
			if text := m.modelPickerOverlay(100); strings.Contains(text, "subscription-bearer") || !strings.Contains(text, "request failed") {
				t.Fatalf("discovery error was not safely rendered: %s", text)
			}
		})
	}
}

func TestSubscriptionPickerIgnoresSupersededRefresh(t *testing.T) {
	m := subscriptionTestModel(t)
	m.modelPickerDiscoveryGeneration = 2
	m.picker = m.newModelPicker()
	updated, _ := m.Update(modelPickerModelsDiscoveredMsg{
		providerID: "chatgpt", endpointKey: "subscription", generation: 2,
		models: []providermodeldiscovery.Model{{ID: "gpt-current-result"}},
	})
	updated, _ = updated.(model).Update(modelPickerModelsDiscoveredMsg{
		providerID: "chatgpt", endpointKey: "subscription", generation: 1, err: errors.New("old refresh failed"),
	})
	next := updated.(model)
	if strings.Contains(next.modelPickerOverlay(100), "old refresh failed") {
		t.Fatal("a superseded request overwrote the current discovery status")
	}
}

type subscriptionTransport func(*http.Request) (*http.Response, error)

func (f subscriptionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSubscriptionSetupDiscoveryUsesOAuthAccount(t *testing.T) {
	for _, entryPoint := range []string{"wizard", "setup"} {
		t.Run(entryPoint, func(t *testing.T) {
			m := subscriptionTestModel(t)
			store, err := oauth.NewStore(oauth.StoreOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Save(oauth.ProviderKey("chatgpt"), oauth.Token{
				AccessToken: "test-subscription-bearer", Account: "test-subscription-account", ExpiresAt: time.Now().Add(time.Hour),
			}); err != nil {
				t.Fatal(err)
			}
			previousTransport := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = previousTransport })
			var liveRequests int
			http.DefaultTransport = subscriptionTransport(func(r *http.Request) (*http.Response, error) {
				body := `{}`
				if r.URL.Host == "chatgpt.com" {
					liveRequests++
					if r.Header.Get("Authorization") != "Bearer test-subscription-bearer" || r.Header.Get("chatgpt-account-id") != "test-subscription-account" {
						t.Error("setup discovery did not use the bearer and account from the same OAuth login")
					}
					body = `{"models":[{"slug":"gpt-future-coder"}]}`
				} else if r.URL.Host != "models.dev" {
					t.Fatalf("unexpected external request: %s", r.URL.Host)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			var cmd tea.Cmd
			if entryPoint == "wizard" {
				provider, _ := providercatalog.Get("chatgpt")
				m.providerWizard = &providerWizardState{providers: []providercatalog.Descriptor{provider}, step: providerWizardStepModel}
				cmd = m.providerModelDiscoveryCmd()
			} else {
				m.setup.providers = []SetupProviderOption{{ID: "chatgpt"}}
				cmd = m.setupModelDiscoveryCmd(1)
			}
			if cmd == nil {
				t.Fatal("missing setup discovery command")
			}
			switch msg := cmd().(type) {
			case providerModelsDiscoveredMsg:
				if msg.err != nil || len(msg.models) != 1 {
					t.Fatalf("wizard discovery failed: %v", msg.err)
				}
			case setupModelsDiscoveredMsg:
				if msg.err != nil || len(msg.models) != 1 {
					t.Fatalf("first-run discovery failed: %v", msg.err)
				}
			default:
				t.Fatalf("unexpected discovery message %T", msg)
			}
			if liveRequests != 1 {
				t.Fatalf("live discovery requests = %d, want 1", liveRequests)
			}
		})
	}
}
