package tui

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/oauth"
	"github.com/Twigpine/zero/internal/providermodeldiscovery"
)

func seedOAuthToken(t *testing.T, providerID, access string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tok.json")
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", path)
	store, err := oauth.NewStore(oauth.StoreOptions{FilePath: path})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Save(oauth.ProviderKey(providerID), oauth.Token{AccessToken: access, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("seed token: %v", err)
	}
}

func TestOAuthStoredTokenReadsStoredLogin(t *testing.T) {
	seedOAuthToken(t, "xai", "live-grok-token")

	if got := oauthStoredToken(context.Background(), "xai"); got != "live-grok-token" {
		t.Fatalf("oauthStoredToken(xai) = %q, want live-grok-token", got)
	}
	// No login for another provider → empty (no error, no panic).
	if got := oauthStoredToken(context.Background(), "openrouter"); got != "" {
		t.Fatalf("oauthStoredToken(openrouter) = %q, want empty", got)
	}
}

// After an xAI OAuth login the bearer lives in the token store, not as a pasted
// key — discovery must resolve it so /models is authenticated and the live list
// shows (matching how the user picks a model after sign-in).
func TestProviderWizardDiscoveryUsesOAuthToken(t *testing.T) {
	seedOAuthToken(t, "xai", "live-grok-token")

	var gotKey string
	m := mouseTestModel()
	m.discoverProviderModels = func(ctx context.Context, profile config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
		gotKey = profile.APIKey
		return []providermodeldiscovery.Model{{ID: "grok-4"}}, nil
	}
	m.providerWizard = m.newProviderWizard()
	m.providerWizard.oauthMode = true
	m.providerWizard.providers = providerWizardOAuthDescriptors()
	for i, d := range m.providerWizard.providers {
		if d.ID == "xai" {
			m.providerWizard.selectedProvider = i
		}
	}
	m.providerWizard.step = providerWizardStepModel

	cmd := m.providerModelDiscoveryCmd()
	if cmd == nil {
		t.Fatal("expected a discovery command for xai")
	}
	msg, ok := cmd().(providerModelsDiscoveredMsg)
	if !ok {
		t.Fatal("unexpected discovery message type")
	}
	if msg.err != nil {
		t.Fatalf("discovery error: %v", msg.err)
	}
	if gotKey != "live-grok-token" {
		t.Fatalf("discovery profile APIKey = %q, want the resolved OAuth token", gotKey)
	}
}
