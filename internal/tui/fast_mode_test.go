package tui

import (
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/providermodeldiscovery"
)

func TestFastCommandTogglesOnlyAdvertisedTier(t *testing.T) {
	m := model{
		modelName:       "gpt-fast",
		providerName:    "ChatGPT",
		providerProfile: config.ProviderProfile{CatalogID: "chatgpt"},
		modelPickerLiveByProvider: map[string][]providermodeldiscovery.Model{
			"chatgpt": {{ID: "gpt-fast", ServiceTiers: []string{"priority"}}},
		},
	}
	m, _ = m.handleFastCommand("")
	if got := m.activeServiceTier(); got != "priority" {
		t.Fatalf("active service tier = %q, want priority", got)
	}
	m, _ = m.handleFastCommand("")
	if got := m.activeServiceTier(); got != "" {
		t.Fatalf("active service tier = %q after second toggle, want off", got)
	}
}

func TestFastCommandRejectsUnsupportedModel(t *testing.T) {
	m := model{
		modelName:       "gpt-standard",
		providerProfile: config.ProviderProfile{CatalogID: "chatgpt"},
		modelPickerLiveByProvider: map[string][]providermodeldiscovery.Model{
			"chatgpt": {{ID: "gpt-standard"}},
		},
		serviceTier: "priority",
	}
	next, text := m.handleFastCommand("")
	if next.activeServiceTier() != "" || next.serviceTier != "" {
		t.Fatalf("unsupported model retained fast tier: %#v", next.serviceTier)
	}
	if !strings.Contains(text, "ChatGPT subscription") {
		t.Fatalf("unsupported response = %q", text)
	}
}

func TestFastCommandRejectsMeteredPriorityTier(t *testing.T) {
	m := model{
		modelName:       "gpt-metered",
		providerProfile: config.ProviderProfile{CatalogID: "openai"},
		modelPickerLiveByProvider: map[string][]providermodeldiscovery.Model{
			"openai": {{ID: "gpt-metered", ServiceTiers: []string{"priority"}}},
		},
		serviceTier: "priority",
	}

	next, text := m.handleFastCommand("")
	if got := next.activeServiceTier(); got != "" {
		t.Fatalf("metered profile forwarded service tier %q", got)
	}
	if next.serviceTier != "" {
		t.Fatalf("metered profile retained service tier %q", next.serviceTier)
	}
	if !strings.Contains(text, "ChatGPT subscription") {
		t.Fatalf("metered response = %q", text)
	}
}

func TestFastCommandDoesNotChangeDuringRun(t *testing.T) {
	m := model{
		pending:         true,
		modelName:       "gpt-fast",
		providerProfile: config.ProviderProfile{CatalogID: "chatgpt"},
		modelPickerLiveByProvider: map[string][]providermodeldiscovery.Model{
			"chatgpt": {{ID: "gpt-fast", ServiceTiers: []string{"priority"}}},
		},
	}
	next, text := m.handleFastCommand("")
	if next.serviceTier != "" || !strings.Contains(text, "Finish or stop") {
		t.Fatalf("pending fast mutation = tier %q text %q", next.serviceTier, text)
	}
}

func TestFastCommandDoesNotInterpretArguments(t *testing.T) {
	m := model{
		modelName:       "gpt-fast",
		providerProfile: config.ProviderProfile{CatalogID: "chatgpt"},
		modelPickerLiveByProvider: map[string][]providermodeldiscovery.Model{
			"chatgpt": {{ID: "gpt-fast", ServiceTiers: []string{"priority"}}},
		},
	}
	next, text := m.handleFastCommand("status")
	if next.serviceTier != "" || !strings.Contains(text, "Use /fast to toggle") {
		t.Fatalf("argument changed fast state: tier %q text %q", next.serviceTier, text)
	}
}
