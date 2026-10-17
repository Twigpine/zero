package tui

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/config"
)

func TestProviderCommandShowsConfiguredOnboardingActions(t *testing.T) {
	text := renderProviderCommand(t, Options{
		ProviderName: "openai",
		ModelName:    "gpt-4.1",
		ProviderProfile: config.ProviderProfile{
			Name:         "openai",
			ProviderKind: config.ProviderKindOpenAI,
			BaseURL:      config.OpenAIBaseURL,
			APIKey:       "sk-configured-secret",
			Model:        "gpt-4.1",
		},
	})

	for _, want := range []string{
		"provider: openai",
		"model: gpt-4.1",
		"active: yes",
		"api key: set",
		"zero providers check openai --connectivity",
		"zero providers catalog",
		"zero providers setup openai --set-active",
	} {
		assertContains(t, text, want)
	}
	assertNotContains(t, text, "sk-configured-secret")
}

func TestProviderOnboardingCommandsWhenProfileMissing(t *testing.T) {
	text := renderProviderCommand(t, Options{})

	for _, want := range []string{
		"status: warning",
		"provider: none",
		"profile: not configured",
		"zero providers catalog",
		"zero providers setup openai --set-active",
		"zero providers add openai --api-key-env OPENAI_API_KEY --set-active",
	} {
		assertContains(t, text, want)
	}
}

func TestProviderCommandShowsMissingCredentialAction(t *testing.T) {
	text := renderProviderCommand(t, Options{
		ProviderName: "anthropic",
		ModelName:    "claude-sonnet-4.5",
		ProviderProfile: config.ProviderProfile{
			Name:         "anthropic",
			ProviderKind: config.ProviderKindAnthropic,
			BaseURL:      config.AnthropicBaseURL,
			Model:        "claude-sonnet-4.5",
		},
	})

	for _, want := range []string{
		"provider: anthropic",
		"api key: not set",
		"set ANTHROPIC_API_KEY in your environment",
		"zero providers add anthropic --api-key-env ANTHROPIC_API_KEY --set-active",
		"zero providers check anthropic --connectivity",
	} {
		assertContains(t, text, want)
	}
}

func TestProviderCommandShowsMissingCredentialActionForCompatibleProvider(t *testing.T) {
	text := renderProviderCommand(t, Options{
		ProviderName: "manual-openai-compatible",
		ModelName:    "custom-model",
		ProviderProfile: config.ProviderProfile{
			Name:         "manual-openai-compatible",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://provider.example/v1",
			Model:        "custom-model",
		},
	})

	for _, want := range []string{
		"provider: manual-openai-compatible",
		"api key: not set",
		"set OPENAI_API_KEY in your environment",
		"zero providers add custom-openai-compatible --api-key-env OPENAI_API_KEY --set-active",
	} {
		assertContains(t, text, want)
	}
}

func renderProviderCommand(t *testing.T, options Options) string {
	t.Helper()

	m := newModel(context.Background(), options)
	m.input.SetValue("/provider status")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if cmd != nil {
		t.Fatal("expected /provider to be handled without starting an agent run")
	}
	return transcriptText(next.transcript)
}
