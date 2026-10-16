package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Gitlawb/zero/internal/aimlapi"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/providercatalog"
	"github.com/Gitlawb/zero/internal/providermodeldiscovery"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

func TestProviderCommandOpensOnboardingWizard(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.input.SetValue("/provider")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd != nil {
		t.Fatal("expected /provider to open the onboarding wizard without starting a run")
	}
	if next.providerWizard == nil {
		t.Fatal("expected provider wizard to be open")
	}
	if next.providerWizard.step != providerWizardStepMethod {
		t.Fatalf("wizard step = %v, want connect-method chooser", next.providerWizard.step)
	}
	if len(next.transcript) != len(m.transcript) {
		t.Fatalf("/provider should not append transcript output when opening wizard")
	}
	// The wizard opens on the connect-method chooser, with OAuth listed first.
	view := plainRender(t, next.View())
	for _, want := range []string{
		"Provider setup",
		"How do you want to connect?",
		"Sign in with OAuth",
		"Paste an API key",
	} {
		assertContains(t, view, want)
	}

	// Choosing the API-key method reveals the full provider catalog.
	next.providerWizard.selectedMethod = len(providerWizardMethodOptions()) - 1
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	listView := plainRender(t, next.View())
	for _, want := range []string{"Choose provider", "aimlapi.com", "GitLawb OpenGateway", "OpenAI", "Anthropic", "Google", "OpenRouter", "Ollama"} {
		assertContains(t, listView, want)
	}
}

func TestProviderWizardStartsAtFirstProvider(t *testing.T) {
	m := newModel(context.Background(), Options{
		ProviderName: "ollama-cloud",
		ModelName:    "cogito-2.1:671b",
	})
	m.providerProfile = config.ProviderProfile{
		Name:      "ollama-cloud",
		CatalogID: "ollama-cloud",
		Model:     "cogito-2.1:671b",
	}

	next := openProviderWizardForTest(t, m)
	if next.providerWizard.selectedProvider != 0 {
		t.Fatalf("selected provider = %d, want first provider", next.providerWizard.selectedProvider)
	}
	if got, want := next.providerWizard.currentProvider().ID, next.providerWizard.providers[0].ID; got != want {
		t.Fatalf("current provider = %q, want first provider %q", got, want)
	}
}

func TestProviderWizardReplacesEmptyStateWordmark(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.width = 120
	m.height = 34
	m.input.SetValue("/provider")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	next.width = m.width
	next.height = m.height

	view := plainRender(t, next.View())
	assertContains(t, view, "Provider setup")
	assertNotContains(t, view, emptyStateTagline)
	assertNotContains(t, view, "███████")
}

func TestProviderWizardUsesRuntimeProviderCatalog(t *testing.T) {
	wizard := newModel(context.Background(), Options{}).newProviderWizard()
	got := map[string]bool{}
	for _, provider := range wizard.providers {
		got[provider.ID] = true
		if !providercatalog.RuntimeSupported(provider) {
			t.Fatalf("wizard included unsupported provider %q", provider.ID)
		}
	}

	for _, provider := range providercatalog.All() {
		if !providercatalog.RuntimeSupported(provider) {
			continue
		}
		if !got[provider.ID] {
			t.Fatalf("wizard omitted runtime catalog provider %q", provider.ID)
		}
	}
	for _, unsupported := range []string{"bedrock", "vertex"} {
		if got[unsupported] {
			t.Fatalf("wizard should not include unsupported provider %q", unsupported)
		}
	}
}

func TestProviderWizardModelsAreProviderScoped(t *testing.T) {
	tests := []struct {
		provider string
		want     []string
		notWant  []string
	}{
		{
			provider: "ollama",
			want:     []string{"llama3.1", "qwen2.5-coder:32b"},
			notWant:  []string{"gpt-4.1", "gpt-5", "openai/gpt-4.1"},
		},
		{
			provider: "groq",
			want:     []string{"llama-3.3-70b-versatile", "openai/gpt-oss-120b"},
			notWant:  []string{"gpt-4.1", "claude-sonnet-4.5"},
		},
		{
			provider: "mistral",
			want:     []string{"mistral-large-latest", "codestral-latest"},
			notWant:  []string{"gpt-4.1", "claude-sonnet-4.5"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			descriptor, ok := providercatalog.Get(tt.provider)
			if !ok {
				t.Fatalf("provider %q missing from catalog", tt.provider)
			}
			models := providerWizardModelOptions(descriptor)
			got := map[string]bool{}
			for _, model := range models {
				got[model.ID] = true
			}
			for _, want := range tt.want {
				if !got[want] {
					t.Fatalf("%s models missing %q; got %#v", tt.provider, want, providerWizardModelIDs(models))
				}
			}
			for _, notWant := range tt.notWant {
				if got[notWant] {
					t.Fatalf("%s models should not include %q; got %#v", tt.provider, notWant, providerWizardModelIDs(models))
				}
			}
		})
	}
}

func TestProviderWizardAdvancesProviderAPIKeyAndModelSteps(t *testing.T) {
	// Pin the credential state so the footer assertion does not depend on the
	// ambient environment.
	t.Setenv("ANTHROPIC_API_KEY", "")

	m := newModel(context.Background(), Options{})
	m = openProviderWizardForTest(t, m)

	m.providerWizard.selectedProvider = providerWizardProviderIndex(t, m.providerWizard, "anthropic")
	next := m
	if got := next.providerWizard.currentProvider().ID; got != "anthropic" {
		t.Fatalf("after down, selected provider = %q, want anthropic", got)
	}
	clearProviderAuthEnvForTest(t, next.providerWizard.currentProvider())

	updated, _ := next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if next.providerWizard.step != providerWizardStepCredential {
		t.Fatalf("wizard step = %v, want credential", next.providerWizard.step)
	}
	view := plainRender(t, next.View())
	for _, want := range []string{
		"Paste API key",
		"ANTHROPIC_API_KEY",
		"Enter continue",
	} {
		assertContains(t, view, want)
	}
	assertNotContains(t, view, "zero providers add anthropic")

	updated, cmd := next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if next.providerWizard.step != providerWizardStepModel {
		t.Fatalf("wizard step = %v, want model", next.providerWizard.step)
	}
	if cmd == nil {
		t.Fatal("entering model step should start live model discovery")
	}
	view = plainRender(t, next.View())
	assertContains(t, view, "Checking available models")
	assertNotContains(t, view, "claude-sonnet-4.5")

	updated, _ = next.Update(providerModelsDiscoveredMsg{
		providerID: "anthropic",
		token:      next.providerWizard.discoveryToken,
		models: []providermodeldiscovery.Model{{
			ID:          "claude-sonnet-4.5",
			Description: "claude-sonnet-4.5",
		}},
	})
	next = updated.(model)
	view = plainRender(t, next.View())
	for _, want := range []string{
		"Choose a model",
		"claude-sonnet-4.5",
	} {
		assertContains(t, view, want)
	}

	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if next.providerWizard.step != providerWizardStepDone {
		t.Fatalf("wizard step = %v, want done", next.providerWizard.step)
	}
	view = plainRender(t, next.View())
	for _, want := range []string{
		"Ready to connect",
		"Provider    Anthropic",
		"Model       claude-sonnet-4.5",
		"Credential  ANTHROPIC_API_KEY env var",
		"Press Enter to save and start using this provider.",
	} {
		assertContains(t, view, want)
	}
	assertNotContains(t, view, "zero providers check")
}

func TestProviderWizardSupportsLeftAndGuardedRightNavigation(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m = openProviderWizardForTest(t, m)

	updated, _ := m.Update(testKey(tea.KeyRight))
	next := updated.(model)
	if next.providerWizard.step != providerWizardStepCredential {
		t.Fatalf("right from provider step = %v, want credential", next.providerWizard.step)
	}

	updated, _ = next.Update(testKey(tea.KeyLeft))
	next = updated.(model)
	if next.providerWizard.step != providerWizardStepProvider {
		t.Fatalf("left from credential step = %v, want provider", next.providerWizard.step)
	}

	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	clearProviderAuthEnvForTest(t, next.providerWizard.currentProvider())
	updated, _ = next.Update(testKey(tea.KeyRight))
	next = updated.(model)
	if next.providerWizard.step != providerWizardStepCredential {
		t.Fatalf("right from empty credential step = %v, want credential", next.providerWizard.step)
	}

	updated, _ = next.Update(testKeyText("sk-test"))
	next = updated.(model)
	updated, cmd := next.Update(testKey(tea.KeyRight))
	next = updated.(model)
	if next.providerWizard.step != providerWizardStepModel {
		t.Fatalf("right from entered credential step = %v, want model", next.providerWizard.step)
	}
	if cmd == nil {
		t.Fatal("right from entered credential should start live model discovery")
	}

	updated, _ = next.Update(testKey(tea.KeyRight))
	next = updated.(model)
	if next.providerWizard.step != providerWizardStepModel {
		t.Fatalf("right while loading step = %v, want model", next.providerWizard.step)
	}

	updated, _ = next.Update(providerModelsDiscoveredMsg{
		providerID: next.providerWizard.currentProvider().ID,
		token:      next.providerWizard.discoveryToken,
		models: []providermodeldiscovery.Model{{
			ID:          next.providerWizard.currentProvider().DefaultModel,
			Description: next.providerWizard.currentProvider().DefaultModel,
		}},
	})
	next = updated.(model)
	updated, _ = next.Update(testKey(tea.KeyRight))
	next = updated.(model)
	if next.providerWizard.step != providerWizardStepDone {
		t.Fatalf("right from model step = %v, want ready", next.providerWizard.step)
	}

	updated, _ = next.Update(testKey(tea.KeyLeft))
	next = updated.(model)
	if next.providerWizard.step != providerWizardStepModel {
		t.Fatalf("left from ready step = %v, want model", next.providerWizard.step)
	}
}

func TestProviderWizardRightAllowsExistingCredentialEnv(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-env")
	m := newModel(context.Background(), Options{})
	m = openProviderWizardForTest(t, m)

	m.providerWizard.selectedProvider = providerWizardProviderIndex(t, m.providerWizard, "openai")
	next := m
	if got := next.providerWizard.currentProvider().ID; got != "openai" {
		t.Fatalf("after down, selected provider = %q, want openai", got)
	}
	updated, _ := next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if next.providerWizard.step != providerWizardStepCredential {
		t.Fatalf("enter from provider step = %v, want credential", next.providerWizard.step)
	}

	updated, cmd := next.Update(testKey(tea.KeyRight))
	next = updated.(model)
	if next.providerWizard.step != providerWizardStepModel {
		t.Fatalf("right with env credential step = %v, want model", next.providerWizard.step)
	}
	if cmd == nil {
		t.Fatal("right with env credential should start live model discovery")
	}
}

func TestProviderWizardCustomCompatibleProviderCollectsEndpointAndModel(t *testing.T) {
	var captured config.ProviderProfile
	m := newModel(context.Background(), Options{
		NewProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			captured = profile
			return &fakeProvider{}, nil
		},
	})
	m = openProviderWizardForTest(t, m)
	m.providerWizard.selectedProvider = providerWizardProviderIndex(t, m.providerWizard, "custom-openai-compatible")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd != nil {
		t.Fatal("custom endpoint step should not start model discovery")
	}
	if next.providerWizard.step != providerWizardStepEndpoint {
		t.Fatalf("custom provider first step = %v, want endpoint", next.providerWizard.step)
	}
	view := plainRender(t, next.View())
	for _, want := range []string{
		"Endpoint URL",
		"url >",
		"https://api.example.com/v1",
		"3 endpoint",
	} {
		assertContains(t, view, want)
	}

	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if next.providerWizard.step != providerWizardStepEndpoint {
		t.Fatalf("blank endpoint advanced to %v, want endpoint", next.providerWizard.step)
	}
	assertContains(t, plainRender(t, next.View()), "enter an endpoint URL")

	updated, _ = next.Update(testKeyText("https://proxy.example/v1"))
	next = updated.(model)
	updated, cmd = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if cmd != nil {
		t.Fatal("endpoint step should not start model discovery")
	}
	if next.providerWizard.step != providerWizardStepName {
		t.Fatalf("endpoint step advanced to %v, want name", next.providerWizard.step)
	}
	view = plainRender(t, next.View())
	for _, want := range []string{
		"Provider name",
		"name >",
		"proxy",
	} {
		assertContains(t, view, want)
	}

	updated, cmd = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if cmd != nil {
		t.Fatal("name step should not start model discovery")
	}
	if next.providerWizard.step != providerWizardStepCredential {
		t.Fatalf("name step advanced to %v, want credential", next.providerWizard.step)
	}

	updated, cmd = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if cmd != nil {
		t.Fatal("custom model step should not discover against the placeholder endpoint")
	}
	if next.providerWizard.step != providerWizardStepModel {
		t.Fatalf("credential step advanced to %v, want model", next.providerWizard.step)
	}
	view = plainRender(t, next.View())
	for _, want := range []string{
		"Model name",
		"model >",
		"custom-model",
	} {
		assertContains(t, view, want)
	}

	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if next.providerWizard.step != providerWizardStepModel {
		t.Fatalf("blank model advanced to %v, want model", next.providerWizard.step)
	}
	assertContains(t, plainRender(t, next.View()), "enter a model name")

	updated, _ = next.Update(testKeyText("my-custom-model"))
	next = updated.(model)
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if next.providerWizard.step != providerWizardStepDone {
		t.Fatalf("model step advanced to %v, want ready", next.providerWizard.step)
	}
	view = plainRender(t, next.View())
	assertContains(t, view, "Endpoint    https://proxy.example/v1")
	assertContains(t, view, "Name        proxy")
	assertContains(t, view, "Model       my-custom-model")

	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if next.providerWizard != nil {
		t.Fatal("saving custom provider should close the wizard")
	}
	if captured.CatalogID != "custom-openai-compatible" || captured.ProviderKind != config.ProviderKindOpenAICompatible {
		t.Fatalf("captured provider identity = %#v", captured)
	}
	if captured.Name != "proxy" {
		t.Fatalf("captured Name = %q, want derived endpoint name", captured.Name)
	}
	if captured.BaseURL != "https://proxy.example/v1" {
		t.Fatalf("captured BaseURL = %q, want custom endpoint", captured.BaseURL)
	}
	if captured.Model != "my-custom-model" {
		t.Fatalf("captured Model = %q, want typed model", captured.Model)
	}
	// A custom endpoint left with no credential means "this endpoint needs no
	// auth" (e.g. a local llama.cpp server) — it must not be stamped with the
	// catalog's generic OPENAI_API_KEY placeholder, or the saved profile gets
	// treated as missing a credential and filtered out of /model on the next
	// run (issue #555).
	if captured.APIKeyEnv != "" {
		t.Fatalf("captured APIKeyEnv = %q, want empty for a no-auth custom endpoint", captured.APIKeyEnv)
	}
}

func TestProviderWizardAcceptsPastedEndpointURL(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m = openProviderWizardForTest(t, m)
	m.providerWizard.selectedProvider = providerWizardProviderIndex(t, m.providerWizard, "custom-openai-compatible")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if next.providerWizard.step != providerWizardStepEndpoint {
		t.Fatalf("custom provider first step = %v, want endpoint", next.providerWizard.step)
	}

	updated, _ = next.Update(testPaste("https://proxy.example/v1\n"))
	next = updated.(model)
	if next.providerWizard.baseURL != "https://proxy.example/v1" {
		t.Fatalf("wizard baseURL = %q, want pasted endpoint", next.providerWizard.baseURL)
	}

	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if next.providerWizard.step != providerWizardStepName {
		t.Fatalf("endpoint step advanced to %v, want name", next.providerWizard.step)
	}
}

func TestProviderWizardCustomCompatibleProviderRejectsRemoteHTTP(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m = openProviderWizardForTest(t, m)
	m.providerWizard.selectedProvider = providerWizardProviderIndex(t, m.providerWizard, "custom-openai-compatible")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if next.providerWizard.step != providerWizardStepEndpoint {
		t.Fatalf("custom provider first step = %v, want endpoint", next.providerWizard.step)
	}

	updated, _ = next.Update(testKeyText("http://api.example.com/v1"))
	next = updated.(model)
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if next.providerWizard.step != providerWizardStepEndpoint {
		t.Fatalf("remote http endpoint advanced to %v, want endpoint", next.providerWizard.step)
	}
	assertContains(t, plainRender(t, next.View()), "endpoint URL must use https:// unless it is local loopback")
}

func TestProviderWizardCustomCompatibleProviderDerivesIPName(t *testing.T) {
	var captured config.ProviderProfile
	m := newModel(context.Background(), Options{
		NewProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			captured = profile
			return &fakeProvider{}, nil
		},
	})
	m = openProviderWizardForTest(t, m)
	m.providerWizard.selectedProvider = providerWizardProviderIndex(t, m.providerWizard, "custom-openai-compatible")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	updated, _ = next.Update(testKeyText("https://127.0.0.1:1234/v1"))
	next = updated.(model)
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if next.providerWizard.step != providerWizardStepName {
		t.Fatalf("endpoint step advanced to %v, want name", next.providerWizard.step)
	}
	assertContains(t, plainRender(t, next.View()), "ip-127-0-0-1")

	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	updated, _ = next.Update(testKeyText("local-test-model"))
	next = updated.(model)
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if next.providerWizard.step != providerWizardStepDone {
		t.Fatalf("model step advanced to %v, want ready", next.providerWizard.step)
	}
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if next.providerWizard != nil {
		t.Fatal("saving custom provider should close the wizard")
	}
	if captured.Name != "ip-127-0-0-1" {
		t.Fatalf("captured Name = %q, want sanitized IP name", captured.Name)
	}
	if captured.BaseURL != "https://127.0.0.1:1234/v1" {
		t.Fatalf("captured BaseURL = %q, want IP endpoint", captured.BaseURL)
	}
	if captured.Model != "local-test-model" {
		t.Fatalf("captured Model = %q, want typed model", captured.Model)
	}
}

func TestProviderWizardSkipsAPIKeyForLocalProvidersAndEscCloses(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m = openProviderWizardForTest(t, m)
	m.providerWizard.selectedProvider = providerWizardProviderIndex(t, m.providerWizard, "ollama")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if next.providerWizard.step != providerWizardStepModel {
		t.Fatalf("local provider step = %v, want model", next.providerWizard.step)
	}
	view := plainRender(t, next.View())
	if strings.Contains(view, "Add API key") {
		t.Fatalf("local provider should skip API key step, got view:\n%s", view)
	}
	if strings.Contains(view, "Paste API key") {
		t.Fatalf("local provider should skip API key step, got view:\n%s", view)
	}
	assertContains(t, view, "Checking available models")
	assertNotContains(t, view, "llama3.1")

	updated, _ = next.Update(testKey(tea.KeyEsc))
	next = updated.(model)
	if next.providerWizard != nil {
		t.Fatal("Esc should close provider wizard")
	}
}

func TestProviderWizardAcceptsPastedAPIKeyWithoutRenderingSecret(t *testing.T) {
	const secret = "AIza-secret-123"
	m := newModel(context.Background(), Options{})
	m = openProviderWizardForTest(t, m)
	m.providerWizard.selectedProvider = providerWizardProviderIndex(t, m.providerWizard, "google")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if next.providerWizard.step != providerWizardStepCredential {
		t.Fatalf("wizard step = %v, want credential", next.providerWizard.step)
	}

	updated, _ = next.Update(testPaste(secret))
	next = updated.(model)
	if next.providerWizard.apiKey != secret {
		t.Fatalf("wizard api key was not captured from paste")
	}
	view := plainRender(t, next.View())
	for _, want := range []string{"Paste API key", "api key >", "saved in your user config"} {
		assertContains(t, view, want)
	}
	assertNotContains(t, view, secret)
}

func TestProviderWizardAppliesPastedKeyToCurrentSession(t *testing.T) {
	const secret = "AIza-secret-123"
	var captured config.ProviderProfile
	m := newModel(context.Background(), Options{
		NewProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			captured = profile
			return &fakeProvider{}, nil
		},
	})
	m = openProviderWizardForTest(t, m)
	m.providerWizard.selectedProvider = providerWizardProviderIndex(t, m.providerWizard, "google")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	updated, _ = next.Update(testPaste(secret))
	next = updated.(model)
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if next.providerWizard.step != providerWizardStepModel {
		t.Fatalf("wizard step = %v, want model", next.providerWizard.step)
	}
	next = finishProviderWizardModelDiscoveryForTest(t, next)
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if next.providerWizard.step != providerWizardStepDone {
		t.Fatalf("wizard step = %v, want done", next.providerWizard.step)
	}
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)

	if next.providerWizard != nil {
		t.Fatal("successful provider apply should close the wizard")
	}
	if captured.CatalogID != "google" || captured.ProviderKind != config.ProviderKindGoogle {
		t.Fatalf("captured profile provider = %#v, want google", captured)
	}
	if captured.APIKey != secret {
		t.Fatalf("captured API key = %q, want pasted secret", captured.APIKey)
	}
	if captured.APIKeyEnv != "" {
		t.Fatalf("captured APIKeyEnv = %q, want empty when using pasted key", captured.APIKeyEnv)
	}
	if next.providerProfile.APIKey != secret || next.providerName != "google" {
		t.Fatalf("model provider state was not updated: provider=%q profile=%#v", next.providerName, next.providerProfile)
	}
}

func TestProviderWizardPersistsPastedKeyToUserConfig(t *testing.T) {
	const secret = "ollama-secret-123"
	// Encrypted-file backend in the temp config dir keeps the test off the real keychain.
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")
	var captured config.ProviderProfile
	m := newModel(context.Background(), Options{
		UserConfigPath: configPath,
		NewProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			captured = profile
			return &fakeProvider{}, nil
		},
	})
	m = openProviderWizardForTest(t, m)
	m.providerWizard.selectedProvider = providerWizardProviderIndex(t, m.providerWizard, "ollama-cloud")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	updated, _ = next.Update(testPaste(secret))
	next = updated.(model)
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	next = finishProviderWizardModelDiscoveryForTest(t, next)
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	updated, _ = next.Update(testKey(tea.KeyEnter))
	_ = updated.(model)

	if captured.APIKey != secret {
		t.Fatalf("captured APIKey = %q, want pasted secret", captured.APIKey)
	}
	persisted := readProviderWizardConfigFixture(t, configPath)
	if persisted.ActiveProvider != "ollama-cloud" {
		t.Fatalf("active provider = %q, want ollama-cloud", persisted.ActiveProvider)
	}
	if len(persisted.Providers) != 1 {
		t.Fatalf("providers length = %d, want 1", len(persisted.Providers))
	}
	profile := persisted.Providers[0]
	if profile.Name != "ollama-cloud" || profile.CatalogID != "ollama-cloud" {
		t.Fatalf("persisted provider identity = %#v, want ollama-cloud", profile)
	}
	// Capture flip: the secret lives in the credential store, not config.json.
	if profile.APIKey != "" || profile.APIKeyEnv != "" {
		t.Fatalf("config must not persist the key: APIKey %q APIKeyEnv %q", profile.APIKey, profile.APIKeyEnv)
	}
	if !profile.APIKeyStored {
		t.Fatal("expected APIKeyStored marker in persisted config")
	}
	store, err := config.ProviderKeyStoreAt(filepath.Dir(configPath))
	if err != nil {
		t.Fatal(err)
	}
	if key, ok, _ := store.Get("ollama-cloud"); !ok || key != secret {
		t.Fatalf("stored key = %q,%v; want the pasted secret in the credential store", key, ok)
	}
}

func TestProviderWizardRejectsCaseVariantBeforeCredentialCapture(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"activeProvider":"work","providers":[{"name":"work","apiKeyStored":true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := config.ProviderKeyStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("work", "OLD"); err != nil {
		t.Fatal(err)
	}

	m := newModel(context.Background(), Options{
		UserConfigPath: configPath,
		NewProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return &fakeProvider{}, nil
		},
	})
	m = openProviderWizardForTest(t, m)
	m.providerWizard.selectedProvider = providerWizardProviderIndex(t, m.providerWizard, "ollama-cloud")
	m.providerWizard.profileName = "WORK"
	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	updated, _ = next.Update(testPaste("NEW"))
	next = updated.(model)
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	next = finishProviderWizardModelDiscoveryForTest(t, next)
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)

	if next.providerWizard == nil {
		t.Fatal("rejected wizard unexpectedly closed")
	}
	if !strings.Contains(next.providerWizard.err, `provider "WORK" already exists as "work"`) {
		t.Fatalf("wizard error = %q, want case-variant collision", next.providerWizard.err)
	}
	after, readErr := os.ReadFile(configPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(after) != string(before) {
		t.Fatalf("rejected wizard rewrote config\nbefore: %s\nafter: %s", before, after)
	}
	if key, ok, getErr := store.Get("work"); getErr != nil || !ok || key != "OLD" {
		t.Fatalf("existing credential = %q,%v,%v; want OLD,true,nil", key, ok, getErr)
	}
}

func TestProviderWizardUsesAPIKeyEnvForCurrentSessionWithoutPersistingSecret(t *testing.T) {
	const secret = "ollama-env-secret"
	t.Setenv("OLLAMA_API_KEY", secret)
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")
	var captured config.ProviderProfile
	m := newModel(context.Background(), Options{
		UserConfigPath: configPath,
		NewProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			captured = profile
			return &fakeProvider{}, nil
		},
	})
	m = openProviderWizardForTest(t, m)
	m.providerWizard.selectedProvider = providerWizardProviderIndex(t, m.providerWizard, "ollama-cloud")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	next = finishProviderWizardModelDiscoveryForTest(t, next)
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)

	if captured.APIKey != secret {
		t.Fatalf("runtime APIKey = %q, want env secret", captured.APIKey)
	}
	if captured.APIKeyEnv != "OLLAMA_API_KEY" {
		t.Fatalf("runtime APIKeyEnv = %q, want OLLAMA_API_KEY", captured.APIKeyEnv)
	}
	if next.providerProfile.APIKey != "" {
		t.Fatalf("session providerProfile persisted APIKey = %q, want empty", next.providerProfile.APIKey)
	}
	if next.providerProfile.APIKeyEnv != "OLLAMA_API_KEY" {
		t.Fatalf("session providerProfile APIKeyEnv = %q, want OLLAMA_API_KEY", next.providerProfile.APIKeyEnv)
	}
	persisted := readProviderWizardConfigFixture(t, configPath)
	if len(persisted.Providers) != 1 {
		t.Fatalf("providers length = %d, want 1", len(persisted.Providers))
	}
	profile := persisted.Providers[0]
	if profile.APIKey != "" {
		t.Fatalf("persisted APIKey = %q, want empty", profile.APIKey)
	}
	if profile.APIKeyEnv != "OLLAMA_API_KEY" {
		t.Fatalf("persisted APIKeyEnv = %q, want OLLAMA_API_KEY", profile.APIKeyEnv)
	}
}

func TestProviderWizardUsesLiveDiscoveredModels(t *testing.T) {
	var captured config.ProviderProfile
	m := newModel(context.Background(), Options{
		DiscoverProviderModels: func(ctx context.Context, profile config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
			captured = profile
			return []providermodeldiscovery.Model{{ID: "live-b"}, {ID: "live-a"}}, nil
		},
	})
	m = openProviderWizardForTest(t, m)
	m.providerWizard.selectedProvider = providerWizardProviderIndex(t, m.providerWizard, "ollama")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if next.providerWizard.step != providerWizardStepModel {
		t.Fatalf("wizard step = %v, want model", next.providerWizard.step)
	}
	if cmd == nil {
		t.Fatal("entering model step should start live model discovery")
	}
	view := plainRender(t, next.View())
	assertContains(t, view, "Checking available models")
	assertNotContains(t, view, "llama3.1")

	updated, _ = next.Update(testKey(tea.KeyEnter))
	waiting := updated.(model)
	if waiting.providerWizard.step != providerWizardStepModel {
		t.Fatalf("enter while loading step = %v, want model", waiting.providerWizard.step)
	}
	assertContains(t, plainRender(t, waiting.View()), "Models are still loading.")

	msg := cmd()
	updated, _ = next.Update(msg)
	next = updated.(model)

	if captured.CatalogID != "ollama" {
		t.Fatalf("discovery profile = %#v, want ollama", captured)
	}
	if got := providerWizardModelIDs(next.providerWizard.models); strings.Join(got, ",") != "live-b,live-a" {
		t.Fatalf("wizard models = %#v, want live discovered models", got)
	}
	view = plainRender(t, next.View())
	assertNotContains(t, view, "models: live")
	assertContains(t, view, "search > \u258cmodel name...")
	assertNotContains(t, view, "gpt-4.1")
}

func TestProviderWizardIgnoresStaleDiscoveryForSameProvider(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m = openProviderWizardForTest(t, m)
	m.providerWizard.selectedProvider = providerWizardProviderIndex(t, m.providerWizard, "ollama")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd == nil {
		t.Fatal("first model entry should start discovery")
	}
	staleToken := next.providerWizard.discoveryToken

	updated, _ = next.Update(testKey(tea.KeyLeft))
	next = updated.(model)
	if next.providerWizard.step != providerWizardStepProvider {
		t.Fatalf("left from local model step = %v, want provider", next.providerWizard.step)
	}

	updated, cmd = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if cmd == nil {
		t.Fatal("second model entry should start discovery")
	}
	currentToken := next.providerWizard.discoveryToken
	if currentToken == staleToken {
		t.Fatalf("discovery token did not advance: %d", currentToken)
	}

	updated, _ = next.Update(providerModelsDiscoveredMsg{
		providerID: "ollama",
		token:      staleToken,
		models: []providermodeldiscovery.Model{{
			ID: "stale-model",
		}},
	})
	next = updated.(model)
	if got := providerWizardModelIDs(next.providerWizard.models); containsString(got, "stale-model") {
		t.Fatalf("stale discovery response applied: %#v", got)
	}
	if !next.providerWizard.modelLoading {
		t.Fatal("stale discovery response should not clear the active loading state")
	}

	updated, _ = next.Update(providerModelsDiscoveredMsg{
		providerID: "ollama",
		token:      currentToken,
		models: []providermodeldiscovery.Model{{
			ID: "fresh-model",
		}},
	})
	next = updated.(model)
	if got := providerWizardModelIDs(next.providerWizard.models); !containsString(got, "fresh-model") {
		t.Fatalf("fresh discovery response did not apply: %#v", got)
	}
}

func TestProviderWizardKeepsFallbackModelsWhenLiveDiscoveryFails(t *testing.T) {
	m := newModel(context.Background(), Options{
		DiscoverProviderModels: func(context.Context, config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
			return nil, errors.New("offline")
		},
	})
	m = openProviderWizardForTest(t, m)
	m.providerWizard.selectedProvider = providerWizardProviderIndex(t, m.providerWizard, "ollama")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd == nil {
		t.Fatal("entering model step should start live model discovery")
	}
	view := plainRender(t, next.View())
	assertContains(t, view, "Checking available models")
	assertNotContains(t, view, "llama3.1")

	updated, _ = next.Update(cmd())
	next = updated.(model)

	if got := providerWizardModelIDs(next.providerWizard.models); !containsString(got, "llama3.1") {
		t.Fatalf("wizard models = %#v, want fallback model llama3.1", got)
	}
	view = plainRender(t, next.View())
	assertContains(t, view, "Using built-in model list")
	assertNotContains(t, view, "offline")
}

func TestProviderWizardRendersDiscoveredModelMetadata(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m = openProviderWizardForTest(t, m)
	m.providerWizard.selectedProvider = providerWizardProviderIndex(t, m.providerWizard, "openai")
	m.providerWizard.step = providerWizardStepModel

	next := m.applyProviderModelsDiscovered(providerModelsDiscoveredMsg{
		providerID: "openai",
		models: []providermodeldiscovery.Model{{
			ID:            "gpt-4.1",
			Description:   "GPT-4.1",
			ContextWindow: 1048576,
			ToolCall:      true,
			Reasoning:     true,
			InputCost:     2,
			OutputCost:    8,
			Source:        "models.dev",
		}},
	})

	view := plainRender(t, next.View())
	assertNotContains(t, view, "models: models.dev")
	assertNotContains(t, view, "models: OpenGateway")
	assertContains(t, view, "GPT-4.1")
	assertContains(t, view, "1M ctx")
	assertContains(t, view, "tools")
	assertContains(t, view, "reasoning")
}

func TestProviderWizardModelStepUsesFriendlyNamesAndStaysCompact(t *testing.T) {
	wizard := &providerWizardState{
		step:        providerWizardStepModel,
		modelSource: "models.dev",
		models:      providerWizardManyModelsForTest(18),
	}
	wizard.models[0] = providerWizardModel{
		ID:          "x-ai/grok-4.3",
		Description: "Grok 4.3",
		Meta:        "1M ctx · tools · reasoning",
	}

	view := plainRender(t, strings.Join(wizard.renderModelStep(84), "\n"))
	assertContains(t, view, "search > \u258cmodel name...")
	assertContains(t, view, "\u258c")
	assertNotContains(t, view, "SearchÃ")
	assertNotContains(t, view, "Searchâ")
	assertContains(t, view, "Grok 4.3")
	assertContains(t, view, "x-ai/grok-4.3 · 1M ctx · tools · reasoning")
	assertNotContains(t, view, "❯ x-ai/grok-4.3")
	assertNotContains(t, view, "more models")
	assertNotContains(t, view, "Model 17")
}

func TestProviderWizardModelSearchFiltersAndAppliesRawModelID(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.providerWizard = &providerWizardState{
		step:        providerWizardStepModel,
		modelSource: "models.dev",
		providers: []providercatalog.Descriptor{{
			ID:                  "openrouter",
			Name:                "OpenRouter",
			Transport:           providercatalog.TransportOpenAICompatible,
			DefaultBaseURL:      "https://openrouter.ai/api/v1",
			DefaultModel:        "openai/gpt-4.1",
			AuthEnvVars:         []string{"OPENROUTER_API_KEY"},
			RequiresAuth:        true,
			SupportedAPIFormats: []providercatalog.APIFormat{providercatalog.APIFormatOpenAIChatCompletions},
		}},
		models: []providerWizardModel{
			{ID: "openai/gpt-4.1", Description: "GPT-4.1", Meta: "1M ctx · tools"},
			{ID: "deepseek/deepseek-chat", Description: "DeepSeek Chat", Meta: "64K ctx · tools"},
			{ID: "deepseek/deepseek-v3.2", Description: "DeepSeek V3.2", Meta: "128K ctx · tools"},
		},
	}

	updated, _ := m.Update(testKeyText("deep"))
	next := updated.(model)

	if next.providerWizard.modelSearch != "deep" {
		t.Fatalf("model search = %q, want deep", next.providerWizard.modelSearch)
	}
	if got := next.providerWizard.currentModel().ID; got != "deepseek/deepseek-chat" {
		t.Fatalf("current model ID = %q, want raw OpenRouter ID", got)
	}
	view := plainRender(t, next.View())
	assertContains(t, view, "DeepSeek Chat")
	assertContains(t, view, "DeepSeek V3.2")
	assertNotContains(t, view, "GPT-4.1")
}

func TestProviderWizardBlocksAdvanceWhenModelSearchHasNoMatches(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.providerWizard = &providerWizardState{
		step: providerWizardStepModel,
		providers: []providercatalog.Descriptor{{
			ID:                  "openrouter",
			Name:                "OpenRouter",
			Transport:           providercatalog.TransportOpenAICompatible,
			DefaultBaseURL:      "https://openrouter.ai/api/v1",
			DefaultModel:        "openai/gpt-4.1",
			AuthEnvVars:         []string{"OPENROUTER_API_KEY"},
			RequiresAuth:        true,
			SupportedAPIFormats: []providercatalog.APIFormat{providercatalog.APIFormatOpenAIChatCompletions},
		}},
		models: []providerWizardModel{
			{ID: "openai/gpt-4.1", Description: "GPT-4.1"},
			{ID: "deepseek/deepseek-chat", Description: "DeepSeek Chat"},
		},
	}

	updated, _ := m.Update(testKeyText("nomatch"))
	next := updated.(model)
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)

	if next.providerWizard.step != providerWizardStepModel {
		t.Fatalf("wizard advanced to %v, want model step", next.providerWizard.step)
	}
	if next.providerWizard.err == "" {
		t.Fatal("expected model search error")
	}
	if got := next.providerWizard.currentModel().ID; got != "" {
		t.Fatalf("current model ID = %q, want empty when search has no matches", got)
	}
	view := plainRender(t, next.View())
	assertContains(t, view, "no matching models")
	assertContains(t, view, "choose a matching model")
}

func openProviderWizardForTest(t *testing.T, m model) model {
	t.Helper()
	m.input.SetValue("/provider")
	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if next.providerWizard == nil {
		t.Fatal("expected provider wizard to be open")
	}
	// The wizard now opens on the connect-method chooser. These tests exercise the
	// API-key / browse path, so select that method and advance into the provider
	// list (the OAuth path is covered by the OAuth-specific tests).
	if next.providerWizard.step == providerWizardStepMethod {
		options := providerWizardMethodOptions()
		next.providerWizard.selectedMethod = len(options) - 1 // last = "browse / API key"
		u2, _ := next.Update(testKey(tea.KeyEnter))
		next = u2.(model)
		if next.providerWizard == nil || next.providerWizard.step != providerWizardStepProvider {
			t.Fatalf("expected provider step after choosing API-key method, got %#v", next.providerWizard)
		}
	}
	return next
}

func finishProviderWizardModelDiscoveryForTest(t *testing.T, m model) model {
	t.Helper()
	if m.providerWizard == nil {
		t.Fatal("expected provider wizard to be open")
	}
	if m.providerWizard.step != providerWizardStepModel {
		t.Fatalf("wizard step = %v, want model", m.providerWizard.step)
	}
	provider := m.providerWizard.currentProvider()
	modelID := firstProviderDisplayValue(provider.DefaultModel, "test-model")
	updated, _ := m.Update(providerModelsDiscoveredMsg{
		providerID: provider.ID,
		token:      m.providerWizard.discoveryToken,
		models: []providermodeldiscovery.Model{{
			ID:          modelID,
			Description: modelID,
		}},
	})
	return updated.(model)
}

func providerWizardManyModelsForTest(count int) []providerWizardModel {
	models := make([]providerWizardModel, 0, count)
	for index := 0; index < count; index++ {
		models = append(models, providerWizardModel{
			ID:          fmt.Sprintf("provider/model-%02d", index),
			Description: fmt.Sprintf("Model %02d", index),
			Meta:        "tools",
		})
	}
	return models
}

func providerWizardProviderIndex(t *testing.T, wizard *providerWizardState, id string) int {
	t.Helper()
	for index, provider := range wizard.providers {
		if provider.ID == id {
			return index
		}
	}
	t.Fatalf("provider %q not found in wizard providers", id)
	return 0
}

func clearProviderAuthEnvForTest(t *testing.T, provider providercatalog.Descriptor) {
	t.Helper()
	for _, env := range provider.AuthEnvVars {
		env = strings.TrimSpace(env)
		if env != "" {
			t.Setenv(env, "")
		}
	}
}

func providerWizardModelIDs(models []providerWizardModel) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids
}

func TestWizardProviderStoredKey(t *testing.T) {
	m := model{savedProviders: []config.ProviderProfile{
		{Name: "acme", CatalogID: "acme-cloud", APIKeyStored: true},
		{Name: "nokey"},
	}}
	if name, ok := m.wizardProviderStoredKey(providercatalog.Descriptor{Name: "acme"}); !ok || name != "acme" {
		t.Fatalf("match by name = %q,%v", name, ok)
	}
	if name, ok := m.wizardProviderStoredKey(providercatalog.Descriptor{ID: "acme-cloud"}); !ok || name != "acme" {
		t.Fatalf("match by catalog id = %q,%v", name, ok)
	}
	if _, ok := m.wizardProviderStoredKey(providercatalog.Descriptor{Name: "nokey"}); ok {
		t.Fatal("provider without a stored key must not match")
	}
}

func TestProviderWizardManageKeyRemove(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{"providers":[{"name":"work","apiKeyStored":true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := config.ProviderKeyStoreAt(filepath.Dir(configPath))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("work", "sk-secret"); err != nil {
		t.Fatal(err)
	}

	m := newModel(context.Background(), Options{UserConfigPath: configPath})
	m.providerWizard = &providerWizardState{step: providerWizardStepManageKey, manageProviderName: "WORK", manageKeyCursor: 2}
	next, _ := m.applyManageKeyChoice()
	if next.providerWizard != nil {
		t.Fatal("remove should close the wizard")
	}
	if _, ok, _ := store.Get("work"); ok {
		t.Fatal("remove should delete the normalized key from the credential store")
	}
	if cfg := readProviderWizardConfigFixture(t, configPath); cfg.Providers[0].APIKeyStored {
		t.Fatal("case-variant removal left apiKeyStored set")
	}
}

func TestProviderWizardManageKeyRemoveReportsCleanupFailures(t *testing.T) {
	newRemovalModel := func(t *testing.T) model {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(`{"providers":[{"name":"work","apiKeyStored":true}]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		m := newModel(context.Background(), Options{UserConfigPath: path})
		m.providerWizard = &providerWizardState{step: providerWizardStepManageKey, manageProviderName: "work", manageKeyCursor: 2}
		return m
	}

	t.Run("stored key deletion", func(t *testing.T) {
		m := newRemovalModel(t)
		m.deleteProviderKey = func(string, string) (bool, error) {
			return false, errors.New("injected delete failure")
		}
		next, _ := m.applyManageKeyChoice()
		if next.providerWizard == nil || !strings.Contains(next.providerWizard.err, "Stored key removal failed") {
			t.Fatalf("wizard did not remain open with deletion error: %+v", next.providerWizard)
		}
		// Marker first, secret second: a failed delete leaves an orphaned secret
		// that nothing reads, never a marker claiming a key that is gone.
		if cfg := readProviderWizardConfigFixture(t, next.userConfigPath); cfg.Providers[0].APIKeyStored {
			t.Fatal("marker must be cleared before the secret delete is attempted")
		}
	})

	t.Run("persisted marker cleanup", func(t *testing.T) {
		m := newRemovalModel(t)
		m.deleteProviderKey = func(string, string) (bool, error) { return true, nil }
		m.clearProviderKeyStored = func(string, string) (bool, error) {
			return false, errors.New("injected marker failure")
		}
		next, _ := m.applyManageKeyChoice()
		if next.providerWizard == nil || !strings.Contains(next.providerWizard.err, "Stored key marker cleanup failed") {
			t.Fatalf("wizard did not remain open with marker error: %+v", next.providerWizard)
		}
		if cfg := readProviderWizardConfigFixture(t, next.userConfigPath); !cfg.Providers[0].APIKeyStored {
			t.Fatal("injected marker failure unexpectedly changed config")
		}
	})
}

func TestProviderWizardManageKeyReplaceAndKeep(t *testing.T) {
	m := newModel(context.Background(), Options{UserConfigPath: filepath.Join(t.TempDir(), "config.json")})

	m.providerWizard = &providerWizardState{step: providerWizardStepManageKey, manageProviderName: "acme", manageKeyCursor: 1}
	next, _ := m.applyManageKeyChoice()
	if next.providerWizard == nil || next.providerWizard.step != providerWizardStepCredential {
		t.Fatal("replace should route to the credential step")
	}

	m.providerWizard = &providerWizardState{step: providerWizardStepManageKey, manageProviderName: "acme", manageKeyCursor: 0}
	next, _ = m.applyManageKeyChoice()
	if next.providerWizard != nil {
		t.Fatal("keep should close the wizard without changes")
	}
}

func TestProviderManagerEscCancelsEmbeddedAimlapiFlow(t *testing.T) {
	opCtx, opCancel := context.WithCancel(context.Background())
	topupCtx, topupCancel := context.WithCancel(context.Background())
	state := &aimlapiOnboardState{
		step:        aimlapiStepProgress,
		opCancel:    opCancel,
		topupCancel: topupCancel,
	}
	m := model{providerWizard: &providerWizardState{
		manage:  true,
		step:    providerWizardStepAimlapi,
		aimlapi: state,
	}}

	next, _ := m.handleProviderWizardKey(testKey(tea.KeyEsc))
	if next.providerWizard == nil || next.providerWizard.step != providerWizardStepManage {
		t.Fatalf("manager escape did not return to provider list: %+v", next.providerWizard)
	}
	if next.providerWizard.aimlapi != nil {
		t.Fatal("manager escape retained embedded aimlapi state")
	}
	select {
	case <-opCtx.Done():
	default:
		t.Fatal("pending account/key operation was not cancelled")
	}
	select {
	case <-topupCtx.Done():
	default:
		t.Fatal("pending top-up stream was not cancelled")
	}
}

func TestAdvanceProviderWizardCustomSkipsManageKey(t *testing.T) {
	tests := []struct {
		name      string
		catalogID string
		transport providercatalog.Transport
	}{
		{
			name:      "custom-openai-compatible",
			catalogID: "custom-openai-compatible",
			transport: providercatalog.TransportOpenAICompatible,
		},
		{
			name:      "custom-anthropic-compatible",
			catalogID: "custom-anthropic-compatible",
			transport: providercatalog.TransportAnthropicCompatible,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := model{savedProviders: []config.ProviderProfile{
				{Name: "my-gateway", CatalogID: tt.catalogID, APIKeyStored: true},
			}}
			m.providerWizard = &providerWizardState{
				step: providerWizardStepProvider,
				providers: []providercatalog.Descriptor{
					{ID: tt.catalogID, Name: tt.name, Transport: tt.transport},
				},
				selectedProvider: 0,
			}

			next, _ := m.advanceProviderWizard()
			if next.providerWizard == nil {
				t.Fatal("wizard should not be nil after advancing from provider step")
			}
			if next.providerWizard.step == providerWizardStepManageKey {
				t.Fatal("custom provider should skip ManageKey and route to endpoint, got ManageKey")
			}
			if next.providerWizard.step != providerWizardStepEndpoint {
				t.Fatalf("custom provider should route to endpoint step, got step: %v", next.providerWizard.step)
			}
		})
	}
}

func TestAdvanceProviderWizardNamedShowsManageKey(t *testing.T) {
	m := model{savedProviders: []config.ProviderProfile{
		{Name: "openai", CatalogID: "openai", APIKeyStored: true},
	}}
	m.providerWizard = &providerWizardState{
		step: providerWizardStepProvider,
		providers: []providercatalog.Descriptor{
			{ID: "openai", Name: "OpenAI", Transport: providercatalog.TransportOpenAI},
		},
		selectedProvider: 0,
	}

	next, _ := m.advanceProviderWizard()
	if next.providerWizard == nil {
		t.Fatal("wizard should not be nil after advancing from provider step")
	}
	if next.providerWizard.step != providerWizardStepManageKey {
		t.Fatalf("named provider should route to ManageKey step, got step: %v", next.providerWizard.step)
	}
}

func TestExistingAimlapiConfigurationUsesEnvWithoutCapturingLiteral(t *testing.T) {
	t.Setenv("AIMLAPI_API_KEY", "aimlapi-env-secret")
	m := model{savedProviders: []config.ProviderProfile{{
		Name: "aimlapi", CatalogID: "aimlapi", APIKeyEnv: "AIMLAPI_API_KEY",
		// ResolvedConfig materializes the env value here before the TUI sees it.
		APIKey: "aimlapi-env-secret",
	}}}

	profile, runtimeKey, ok := m.existingAimlapiConfiguration()
	if !ok {
		t.Fatal("expected AIMLAPI_API_KEY to be detected")
	}
	if runtimeKey != "aimlapi-env-secret" {
		t.Fatalf("runtime key = %q", runtimeKey)
	}
	if profile.APIKeyEnv != "AIMLAPI_API_KEY" || profile.APIKey != "" || profile.APIKeyStored {
		t.Fatalf("env credential source was not preserved: %+v", profile)
	}
}

func TestExistingAimlapiEnvironmentFallbackUsesResolvedEndpoint(t *testing.T) {
	t.Setenv("AIMLAPI_API_KEY", "aimlapi-env-secret")
	t.Setenv("AIMLAPI_INFERENCE_URL", "https://staging.example.test/v1")

	profile, runtimeKey, ok := (model{}).existingAimlapiConfiguration()
	if !ok || runtimeKey != "aimlapi-env-secret" {
		t.Fatalf("configuration = (%+v, %q, %v)", profile, runtimeKey, ok)
	}
	if profile.BaseURL != "https://staging.example.test/v1" {
		t.Fatalf("BaseURL = %q, want resolved override", profile.BaseURL)
	}
	if profile.APIKeyEnv != "AIMLAPI_API_KEY" || profile.APIKey != "" {
		t.Fatalf("credential source changed: %+v", profile)
	}
	if len(profile.CustomHeaders) != 0 {
		t.Fatalf("catalog headers leaked to custom endpoint: %#v", profile.CustomHeaders)
	}
}

func TestExistingAimlapiBalanceUsesProfileEndpointAndCancelsOnAbandon(t *testing.T) {
	started := make(chan struct{})
	done := make(chan struct{})
	canonical := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(done)
	}))
	defer canonical.Close()
	override := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("production profile key was sent to process override: %s", r.URL)
	}))
	defer override.Close()
	t.Setenv("AIMLAPI_INFERENCE_URL", override.URL)

	wizard := &providerWizardState{
		step: providerWizardStepAimlapiConfigured,
		aimlapiExistingProfile: config.ProviderProfile{
			Name: "aimlapi", CatalogID: "aimlapi", BaseURL: canonical.URL,
		},
		aimlapiRuntimeKey: "production-secret",
	}
	m := model{ctx: context.Background(), providerWizard: wizard}
	_, cmd := m.checkExistingAimlapiBalance()
	result := make(chan tea.Msg, 1)
	go func() { result <- execCmd(cmd) }()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("profile endpoint did not receive the balance request")
	}
	wizard.clearAimlapiExisting()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("abandoning the preflight did not cancel the balance request")
	}
	select {
	case <-result:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled balance command did not return")
	}
}

func TestProviderWizardAimlapiPartnerOverrideOnlyOnCanonicalEndpoint(t *testing.T) {
	provider, err := providercatalog.Require("aimlapi")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIMLAPI_PARTNER_ID", "part_override")

	canonical := providerWizardProfile(provider, provider.DefaultModel, "", provider.DefaultBaseURL, "")
	if got := canonical.CustomHeaders[aimlapi.PartnerHeaderName]; got != "part_override" {
		t.Fatalf("canonical partner header = %q, want part_override", got)
	}
	custom := providerWizardProfile(provider, provider.DefaultModel, "", "https://staging.example.test/v1", "")
	if len(custom.CustomHeaders) != 0 {
		t.Fatalf("partner headers leaked to custom endpoint: %#v", custom.CustomHeaders)
	}
}

func TestExistingAimlapiConfigurationResolvesStoredKey(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := config.ProviderKeyStoreAt(filepath.Dir(configPath))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("work", "aimlapi-stored-secret"); err != nil {
		t.Fatal(err)
	}
	m := model{
		userConfigPath: configPath,
		savedProviders: []config.ProviderProfile{{Name: "work", CatalogID: "aimlapi", APIKeyStored: true}},
	}

	profile, runtimeKey, ok := m.existingAimlapiConfiguration()
	if !ok || runtimeKey != "aimlapi-stored-secret" {
		t.Fatalf("stored configuration = (%+v, %q, %v)", profile, runtimeKey, ok)
	}
	if !profile.APIKeyStored || profile.APIKey != "" || profile.APIKeyEnv != "" {
		t.Fatalf("stored credential source was not preserved: %+v", profile)
	}
}

func TestExistingAimlapiConfigurationSkipsCustomEndpointCredential(t *testing.T) {
	t.Setenv("AIMLAPI_API_KEY", "")
	m := model{savedProviders: []config.ProviderProfile{{
		Name:      "aimlapi-staging",
		CatalogID: "aimlapi",
		BaseURL:   "https://staging.example.test/v1",
		APIKey:    "staging-secret",
	}}}

	profile, runtimeKey, ok := m.existingAimlapiConfiguration()
	if ok || profile.Name != "" || runtimeKey != "" {
		t.Fatalf("custom endpoint credential entered production preflight: (%+v, %q, %v)", profile, runtimeKey, ok)
	}
}

func TestAdvanceAimlapiWithExistingCredentialShowsConfiguredPreflight(t *testing.T) {
	t.Setenv("AIMLAPI_API_KEY", "aimlapi-env-secret")
	provider, err := providercatalog.Require("aimlapi")
	if err != nil {
		t.Fatal(err)
	}
	m := model{providerWizard: &providerWizardState{
		step:      providerWizardStepProvider,
		providers: []providercatalog.Descriptor{provider},
	}}

	next, _ := m.advanceProviderWizard()
	if next.providerWizard.step != providerWizardStepAimlapiConfigured {
		t.Fatalf("step = %v, want configured preflight", next.providerWizard.step)
	}
	if next.providerWizard.aimlapiExistingProfile.APIKeyEnv != "AIMLAPI_API_KEY" {
		t.Fatalf("profile = %+v", next.providerWizard.aimlapiExistingProfile)
	}
	if !maps.Equal(next.providerWizard.aimlapiExistingProfile.CustomHeaders, provider.CustomHeaders) {
		t.Fatalf("catalog headers were not preserved: %#v", next.providerWizard.aimlapiExistingProfile.CustomHeaders)
	}
}

func TestAimlapiConfigureAgainUsesTwoPathOnboarding(t *testing.T) {
	wizard := &providerWizardState{
		step:                    providerWizardStepAimlapiConfigured,
		aimlapiConfiguredCursor: 1,
		aimlapiExistingProfile:  config.ProviderProfile{Name: "aimlapi", CatalogID: "aimlapi", APIKeyEnv: "AIMLAPI_API_KEY"},
		aimlapiRuntimeKey:       "secret",
	}
	m := model{providerWizard: wizard}

	next, _ := m.handleProviderWizardKey(testKey(tea.KeyEnter))
	if next.providerWizard.step != providerWizardStepAimlapi || next.providerWizard.aimlapi == nil {
		t.Fatalf("configure again did not enter onboarding: %+v", next.providerWizard)
	}
	if next.providerWizard.aimlapiExistingProfile.Name != "" || next.providerWizard.aimlapiRuntimeKey != "" {
		t.Fatal("configure again must discard the old credential context")
	}
}

func TestExistingAimlapiLowBalanceReusesByKeyTopUp(t *testing.T) {
	wizard := &providerWizardState{
		step:                   providerWizardStepAimlapiConfigured,
		baseURL:                "https://validated.example.test/v1",
		aimlapiExistingProfile: config.ProviderProfile{Name: "aimlapi", CatalogID: "aimlapi", BaseURL: "https://validated.example.test/v1", APIKeyEnv: "AIMLAPI_API_KEY"},
		aimlapiRuntimeKey:      "secret",
		aimlapiExistingBusy:    true,
		aimlapiExistingGen:     4,
	}
	m := model{providerWizard: wizard}
	next, _ := m.applyExistingAimlapiBalance(aimlapiExistingBalanceMsg{
		wizard: wizard,
		gen:    4,
		balance: aimlapi.BalanceResult{
			LowBalance: true,
		},
	})
	if next.providerWizard.step != providerWizardStepAimlapi || next.providerWizard.aimlapi == nil {
		t.Fatalf("low balance did not enter aimlapi top-up: %+v", next.providerWizard)
	}
	if !next.providerWizard.aimlapi.byKey || next.providerWizard.aimlapi.step != aimlapiStepLowBalance || next.providerWizard.aimlapi.apiKey != "secret" {
		t.Fatalf("top-up state = %+v", next.providerWizard.aimlapi)
	}
	if next.providerWizard.aimlapi.baseURL != "https://validated.example.test/v1" {
		t.Fatalf("top-up endpoint = %q, want validated endpoint", next.providerWizard.aimlapi.baseURL)
	}
}

func TestApplyExistingAimlapiPreservesEnvCredentialSource(t *testing.T) {
	t.Setenv("AIMLAPI_API_KEY", "runtime-secret")
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"providers":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := providercatalog.Descriptor{
		ID: "aimlapi", Name: "aimlapi.com", Transport: providercatalog.TransportOpenAICompatible,
		CustomHeaders: map[string]string{"X-AIMLAPI-Partner-ID": "partner"},
	}
	m := model{
		userConfigPath: configPath,
		providerWizard: &providerWizardState{
			step:          providerWizardStepDone,
			providers:     []providercatalog.Descriptor{provider},
			models:        []providerWizardModel{{ID: "model/new"}},
			selectedModel: 0,
			modelSource:   "live",
			aimlapiExistingProfile: config.ProviderProfile{
				Name: "aimlapi", CatalogID: "aimlapi", APIKeyEnv: "AIMLAPI_API_KEY", Model: "model/old",
				CustomHeaders: maps.Clone(provider.CustomHeaders),
			},
			aimlapiRuntimeKey: "runtime-secret",
		},
	}

	next, _ := m.applyProviderWizard()
	if next.providerWizard != nil {
		t.Fatal("successful apply should close the wizard")
	}
	cfg := readProviderWizardConfigFixture(t, configPath)
	if len(cfg.Providers) != 1 {
		t.Fatalf("providers = %+v", cfg.Providers)
	}
	got := cfg.Providers[0]
	if got.APIKeyEnv != "AIMLAPI_API_KEY" || got.APIKey != "" || got.APIKeyStored {
		t.Fatalf("credential source changed: %+v", got)
	}
	if got.Model != "model/new" {
		t.Fatalf("model = %q, want model/new", got.Model)
	}
	if !maps.Equal(got.CustomHeaders, provider.CustomHeaders) {
		t.Fatalf("custom headers changed: %#v", got.CustomHeaders)
	}
}

func readProviderWizardConfigFixture(t *testing.T, path string) config.FileConfig {
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

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// Applying the wizard switches the live provider, so it must export
// ZERO_PROVIDER exactly like the /model and /provider switch paths — a stale
// value from an earlier switch would otherwise win over config in every
// spawned child (applyEnv) and pin specialists/swarm members to the OLD
// provider's credentials.
func TestApplyProviderWizardExportsActiveProviderEnv(t *testing.T) {
	t.Setenv(config.ActiveProviderEnv, "stale-previous-provider")
	m := newModel(context.Background(), Options{})
	// Isolate the SUCCESS path's persist to a temp config (the default empty
	// path would skip persist; a future default must never reach the real user
	// config), and stub the build so the full commit sequence runs.
	m.userConfigPath = filepath.Join(t.TempDir(), "config.json")
	m.newProvider = func(config.ProviderProfile) (zeroruntime.Provider, error) {
		return &fakeProvider{}, nil
	}
	m.providerWizard = &providerWizardState{
		step:        providerWizardStepModel,
		profileName: "acme-wizard",
		providers: []providercatalog.Descriptor{{
			ID:                  "openrouter",
			Name:                "OpenRouter",
			Transport:           providercatalog.TransportOpenAICompatible,
			DefaultBaseURL:      "https://openrouter.ai/api/v1",
			DefaultModel:        "openai/gpt-4.1",
			AuthEnvVars:         []string{"OPENROUTER_API_KEY"},
			RequiresAuth:        true,
			SupportedAPIFormats: []providercatalog.APIFormat{providercatalog.APIFormatOpenAIChatCompletions},
		}},
		models: []providerWizardModel{{ID: "openai/gpt-4.1", Description: "GPT-4.1"}},
	}

	updated, _ := m.applyProviderWizard()
	next := updated

	if next.providerName == "" {
		t.Fatal("wizard apply should have set a provider name")
	}
	if got := os.Getenv(config.ActiveProviderEnv); got != next.providerName {
		t.Fatalf("%s = %q after wizard apply, want %q (children would spawn on the stale provider)", config.ActiveProviderEnv, got, next.providerName)
	}
}

func TestApplyProviderWizardReplacesRemovedLiveRowIdentity(t *testing.T) {
	t.Setenv(config.ActiveProviderEnv, "work")
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"providers":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newModel(context.Background(), Options{
		ProviderName:    "work",
		ProviderProfile: config.ProviderProfile{Name: "work"},
		UserConfigPath:  configPath,
	})
	m.removedLiveRow = "work"
	m.newProvider = func(config.ProviderProfile) (zeroruntime.Provider, error) {
		return &fakeProvider{}, nil
	}
	m.providerWizard = &providerWizardState{
		step:        providerWizardStepModel,
		profileName: "openai",
		providers: []providercatalog.Descriptor{{
			ID:             "openai",
			Name:           "OpenAI",
			Transport:      providercatalog.TransportOpenAICompatible,
			DefaultBaseURL: "https://api.openai.com/v1",
			DefaultModel:   "gpt-4.1",
		}},
		models: []providerWizardModel{{ID: "gpt-4.1"}},
	}

	updated, _ := m.applyProviderWizard()
	next := updated
	if next.removedLiveRow != "" {
		t.Fatalf("removedLiveRow = %q, want cleared after wizard commits a new live provider", next.removedLiveRow)
	}
	next, _ = next.openProviderManager()
	if got := next.providerWizard.manageActiveName; got != "openai" {
		t.Fatalf("manageActiveName = %q, want wizard-committed provider openai", got)
	}

	for index, row := range next.providerWizard.manageRows {
		if row.profile.Name == "openai" {
			next.providerWizard.manageCursor = index
			break
		}
	}
	profile, ok := next.providerWizard.currentManagerRow()
	if !ok || profile.profile.Name != "openai" {
		t.Fatalf("wizard-committed provider missing from manager rows: %+v", next.providerWizard.manageRows)
	}
	next.providerWizard.beginProviderEdit(profile.profile, profile.owner)
	next.providerWizard.editDraft.Name = "renamed-openai"
	next, _ = next.saveManagerEdit()
	if next.providerName != "renamed-openai" {
		t.Fatalf("providerName = %q after rename, want renamed-openai", next.providerName)
	}
	if got := os.Getenv(config.ActiveProviderEnv); got != "renamed-openai" {
		t.Fatalf("%s = %q after rename, want renamed-openai", config.ActiveProviderEnv, got)
	}

	next.providerWizard.manageCursor = 0
	next, _ = next.deleteManagerSelection()
	if next.removedLiveRow != "renamed-openai" {
		t.Fatalf("removedLiveRow = %q after delete, want renamed live row", next.removedLiveRow)
	}
}

// On a config PERSIST failure, applyProviderWizard must leave live state fully
// unchanged — the chat must NOT already be running on the new provider while the
// status line and the ZERO_PROVIDER export (which pins spawned children) still
// point at the old one. Build and persist are staged into locals; nothing is
// committed unless both succeed.
func TestApplyProviderWizardPersistFailureLeavesLiveStateUnchanged(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file") // never touch the real OS keychain: apiKey is secured before the persist fails
	t.Setenv(config.ActiveProviderEnv, "old-provider")

	// A config path whose parent is a regular FILE, so writeConfigFile's MkdirAll
	// fails and UpsertProvider returns an error.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed blocker file: %v", err)
	}
	brokenConfigPath := filepath.Join(blocker, "config.json")

	oldProvider := &fakeProvider{}
	newProvider := &fakeProvider{}
	m := newModel(context.Background(), Options{})
	m.provider = oldProvider
	m.providerProfile = config.ProviderProfile{Name: "old-provider"}
	m.providerName = "old-provider"
	m.userConfigPath = brokenConfigPath
	m.newProvider = func(config.ProviderProfile) (zeroruntime.Provider, error) { return newProvider, nil }
	m.providerWizard = &providerWizardState{
		step:        providerWizardStepModel,
		profileName: "acme-new",
		providers: []providercatalog.Descriptor{{
			ID:                  "openrouter",
			Name:                "OpenRouter",
			Transport:           providercatalog.TransportOpenAICompatible,
			DefaultBaseURL:      "https://openrouter.ai/api/v1",
			DefaultModel:        "openai/gpt-4.1",
			AuthEnvVars:         []string{"OPENROUTER_API_KEY"},
			RequiresAuth:        true,
			SupportedAPIFormats: []providercatalog.APIFormat{providercatalog.APIFormatOpenAIChatCompletions},
		}},
		models: []providerWizardModel{{ID: "openai/gpt-4.1", Description: "GPT-4.1"}},
		apiKey: "sk-new",
	}

	updated, _ := m.applyProviderWizard()
	next := updated

	if next.providerWizard == nil || next.providerWizard.err == "" {
		t.Fatal("a persist failure must surface a wizard error and keep the wizard open")
	}
	if next.provider != oldProvider {
		t.Fatal("live provider must NOT be swapped when the persist fails")
	}
	if next.providerName != "old-provider" {
		t.Fatalf("providerName = %q, want it unchanged on persist failure", next.providerName)
	}
	if got := os.Getenv(config.ActiveProviderEnv); got != "old-provider" {
		t.Fatalf("%s = %q, want it unchanged on persist failure (children would diverge from parent)", config.ActiveProviderEnv, got)
	}
}

func TestProviderSearchFiltersByNameIDAndAlias(t *testing.T) {
	wizard := &providerWizardState{
		step: providerWizardStepProvider,
		providers: []providercatalog.Descriptor{
			{ID: "openai", Name: "OpenAI", Aliases: []string{"chatgpt"}},
			{ID: "anthropic", Name: "Anthropic", Aliases: []string{"claude"}},
			{ID: "google", Name: "Google", Aliases: []string{"gemini"}},
			{ID: "groq", Name: "Groq", Aliases: []string{}},
		},
	}

	tests := []struct {
		query string
		want  []string
	}{
		{"openai", []string{"openai"}},
		{"anthropic", []string{"anthropic"}},
		{"claude", []string{"anthropic"}},
		{"gemini", []string{"google"}},
		{"chatgpt", []string{"openai"}},
		{"groq", []string{"groq"}},
		{"GROQ", []string{"groq"}},
		{"nomatch", nil},
		{"", []string{"openai", "anthropic", "google", "groq"}},
	}

	for _, tt := range tests {
		t.Run("query="+tt.query, func(t *testing.T) {
			wizard.providerSearch = tt.query
			got := wizard.filteredProviders()
			if len(got) != len(tt.want) {
				t.Fatalf("filteredProviders(%q) = %d results, want %d", tt.query, len(got), len(tt.want))
			}
			for i, id := range tt.want {
				if got[i].ID != id {
					t.Fatalf("filteredProviders(%q)[%d] = %q, want %q", tt.query, i, got[i].ID, id)
				}
			}
		})
	}
}

func TestProviderSearchResetsSelectedProviderOnEdit(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m = openProviderWizardForTest(t, m)

	// Move to a later provider, then type a search query.
	m.providerWizard.selectedProvider = 3
	updated, _ := m.Update(testKeyText("anth"))
	next := updated.(model)
	if next.providerWizard.selectedProvider != 0 {
		t.Fatalf("selectedProvider after typing = %d, want 0 (reset on edit)", next.providerWizard.selectedProvider)
	}
}

func TestProviderSearchFilteredAdvanceSelectsCorrectProvider(t *testing.T) {
	wizard := &providerWizardState{
		step: providerWizardStepProvider,
		providers: []providercatalog.Descriptor{
			{ID: "openai", Name: "OpenAI", AuthEnvVars: []string{"OPENAI_API_KEY"}, RequiresAuth: true},
			{ID: "anthropic", Name: "Anthropic", AuthEnvVars: []string{"ANTHROPIC_API_KEY"}, RequiresAuth: true},
			{ID: "google", Name: "Google", AuthEnvVars: []string{"GOOGLE_API_KEY"}, RequiresAuth: true},
			{ID: "groq", Name: "Groq", AuthEnvVars: []string{"GROQ_API_KEY"}, RequiresAuth: true},
		},
		selectedProvider: 0,
	}

	// Type a search that matches exactly "groq".
	wizard.providerSearch = "groq"
	filtered := wizard.filteredProviders()
	if len(filtered) != 1 || filtered[0].ID != "groq" {
		t.Fatalf("expected exactly groq in filtered results, got %v", providerWizardIDs(filtered))
	}

	// currentProvider() via filtered list should be groq.
	if got := wizard.currentProvider().ID; got != "groq" {
		t.Fatalf("currentProvider() = %q, want groq", got)
	}
}

func TestProviderSearchEmptyMatchEnterIsNoop(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m = openProviderWizardForTest(t, m)

	// Type a query that matches nothing.
	updated, _ := m.Update(testKeyText("zzzzz"))
	next := updated.(model)
	if len(next.providerWizard.filteredProviders()) != 0 {
		t.Fatal("expected zero filtered providers")
	}

	// Enter should be a no-op.
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if next.providerWizard.step != providerWizardStepProvider {
		t.Fatalf("wizard step = %v, want provider (empty match Enter should be no-op)", next.providerWizard.step)
	}
	if next.providerWizard.providerSearch != "zzzzz" {
		t.Fatalf("providerSearch = %q, want query preserved on noop", next.providerWizard.providerSearch)
	}
}

func TestProviderSearchClearRestoresFullList(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m = openProviderWizardForTest(t, m)

	initialCount := len(m.providerWizard.filteredProviders())
	if initialCount == 0 {
		t.Fatal("expected providers in the wizard")
	}

	// Type a search query.
	updated, _ := m.Update(testKeyText("open"))
	next := updated.(model)
	filtered := next.providerWizard.filteredProviders()
	if len(filtered) >= initialCount {
		t.Fatalf("search should narrow results: got %d, want < %d", len(filtered), initialCount)
	}

	// Ctrl+U clears the search.
	updated, _ = next.Update(testKeyCtrl('u'))
	next = updated.(model)
	restored := next.providerWizard.filteredProviders()
	if len(restored) != initialCount {
		t.Fatalf("after clear, filtered count = %d, want %d", len(restored), initialCount)
	}
}

func providerWizardIDs(descriptors []providercatalog.Descriptor) []string {
	ids := make([]string, len(descriptors))
	for i, d := range descriptors {
		ids[i] = d.ID
	}
	return ids
}

// A provider that returns prose blurbs instead of display names must not
// collapse distinct models into identical rows. Reproduces the reported picker
// where two "Open-weight GPT model..." and two "Kimi multimodal agent model..."
// rows were indistinguishable, with the ID visible only for the highlighted one.
func TestProviderWizardModelRowsStayDistinctWithProseDescriptions(t *testing.T) {
	wizard := &providerWizardState{
		step:        providerWizardStepModel,
		modelSource: "models.dev",
		models: []providerWizardModel{
			{ID: "gpt-oss-120b", Description: "Open-weight GPT model for self-hosted reasoning and instruction-following workloads"},
			{ID: "gpt-oss-20b", Description: "Open-weight GPT model for self-hosted reasoning and instruction-following workloads"},
			{ID: "kimi-k2-thinking", Description: "Kimi multimodal agent model for visual understanding, coding, and planning"},
			{ID: "kimi-k2-instruct", Description: "Kimi multimodal agent model for visual understanding, coding, and planning"},
			{ID: "x-ai/grok-4.3", Description: "Grok 4.3"},
		},
	}
	view := plainRender(t, strings.Join(wizard.renderModelStep(96), "\n"))

	// every prose-described model is identified by its unique ID
	for _, id := range []string{"gpt-oss-120b", "gpt-oss-20b", "kimi-k2-thinking", "kimi-k2-instruct"} {
		if !strings.Contains(view, id) {
			t.Errorf("model %q is not identifiable in the list:\n%s", id, view)
		}
	}
	// a genuine display name is still preferred over the raw id
	if !strings.Contains(view, "Grok 4.3") {
		t.Errorf("friendly display name was lost:\n%s", view)
	}
	// the prose blurb must not be a row label
	if strings.Contains(view, "❯ Open-weight GPT model") {
		t.Errorf("prose blurb still used as a row label:\n%s", view)
	}
}

// The disk write is only half the removal: the live session still holds
// APIKeyStored:true until it is reconciled, so /providers and a re-entered
// wizard would keep offering keep/replace for a key that is gone.
func TestProviderWizardManageKeyRemoveSyncsSessionState(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"providers":[{"name":"work","apiKeyStored":true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := config.ProviderKeyStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("work", "sk-secret"); err != nil {
		t.Fatal(err)
	}
	profile := config.ProviderProfile{Name: "work", APIKeyStored: true}
	m := newModel(context.Background(), Options{
		UserConfigPath:  configPath,
		ProviderName:    "work",
		ProviderProfile: profile,
		SavedProviders:  []config.ProviderProfile{profile},
	})
	m.providerWizard = &providerWizardState{step: providerWizardStepManageKey, manageProviderName: "WORK", manageKeyCursor: 2}

	next, _ := m.applyManageKeyChoice()

	if next.providerProfile.APIKeyStored {
		t.Fatal("live profile still claims a stored key after removal")
	}
	if len(next.savedProviders) != 1 || next.savedProviders[0].APIKeyStored {
		t.Fatalf("saved providers not reconciled: %+v", next.savedProviders)
	}
	// The caller's copy must be untouched: savedProviders is mutated by copy.
	if !m.savedProviders[0].APIKeyStored {
		t.Fatal("session sync mutated the pre-removal model's slice in place")
	}
}

// wizardProviderStoredKey answers a credential question, so it must use the
// store's normalization: strings.EqualFold folds "s" and Unicode long-s into
// one identity that the store keeps apart.
func TestWizardProviderStoredKeyDistinguishesUnicodeIdentities(t *testing.T) {
	m := model{savedProviders: []config.ProviderProfile{
		{Name: "ſ", APIKeyStored: true},
	}}
	if name, ok := m.wizardProviderStoredKey(providercatalog.Descriptor{Name: "s", ID: "s"}); ok {
		t.Fatalf("latin-s descriptor matched the long-s profile %q", name)
	}
	name, ok := m.wizardProviderStoredKey(providercatalog.Descriptor{Name: "ſ", ID: "ſ"})
	if !ok || name != "ſ" {
		t.Fatalf("long-s descriptor = %q, %v; want its own profile", name, ok)
	}
}
