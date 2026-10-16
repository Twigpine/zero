package tui

import (
	"testing"

	"github.com/Gitlawb/zero/internal/config"
)

func TestSavedProviderByNameDistinguishesUnicodeCredentialIdentities(t *testing.T) {
	m := model{
		providerProfile: config.ProviderProfile{Name: "s", Model: "ascii-model"},
		savedProviders: []config.ProviderProfile{
			{Name: "s", Model: "ascii-model"},
			{Name: "ſ", Model: "long-s-model"},
		},
	}

	profile, ok := m.savedProviderByName("ſ")
	if !ok {
		t.Fatal("long-s provider not found")
	}
	if profile.Name != "ſ" || profile.Model != "long-s-model" {
		t.Fatalf("selected provider = %q/%q, want long-s provider", profile.Name, profile.Model)
	}
}
