package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// The session's provider spelling can differ from the persisted row's
// ("openai" vs a saved "OpenAI"). Activation matches credential identity while
// the model write matches the row exactly, so the model write has to use the
// resolved spelling or it silently persists nothing.
func TestModelPersistenceUsesResolvedPersistedSpelling(t *testing.T) {
	newConfig := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, "config.json")
		if err := os.WriteFile(path, []byte(`{"activeProvider":"OpenAI","providers":[{"name":"OpenAI","catalogID":"openai","model":"gpt-5.1"},{"name":"ollama","catalogID":"ollama","provider_kind":"openai-compatible","baseURL":"http://localhost:11434/v1","model":"m1"}]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("persistSelectedModel", func(t *testing.T) {
		configPath := newConfig(t)
		m := newModel(context.Background(), Options{UserConfigPath: configPath})
		// The session spelling "openai" addresses the persisted "OpenAI" row.
		persisted, persistedName, err := m.persistSelectedModel(config.ProviderProfile{Name: "openai", Model: "gpt-5.5"})
		if err != nil {
			t.Fatal(err)
		}
		if !persisted {
			t.Fatal("persistSelectedModel reported no write for a persisted case variant")
		}
		// The returned spelling is what the caller mirrors into savedProviders,
		// so it has to be the row's own — not the session's.
		if persistedName != "OpenAI" {
			t.Fatalf("persisted row = %q, want the row's spelling OpenAI", persistedName)
		}
		cfg := readTUIConfigFixture(t, configPath)
		if cfg.Providers[0].Model != "gpt-5.5" {
			t.Fatalf("model = %q, want gpt-5.5 written to the OpenAI row", cfg.Providers[0].Model)
		}
	})

	t.Run("switchProviderModel", func(t *testing.T) {
		configPath := newConfig(t)
		saved := []config.ProviderProfile{
			{Name: "OpenAI", CatalogID: "openai", Model: "gpt-5.1", APIKey: "sk-test"},
			{Name: "ollama", CatalogID: "ollama", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "http://localhost:11434/v1", Model: "m1"},
		}
		m := newModel(context.Background(), Options{
			UserConfigPath:  configPath,
			ProviderName:    "ollama",
			ModelName:       "m1",
			Provider:        &fakeProvider{},
			ProviderProfile: saved[1],
			SavedProviders:  saved,
			NewProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
				return &fakeProvider{}, nil
			},
		})

		// "openai" is the picker row's owner spelling, not the persisted one.
		if _, status, ok, _, err := m.switchProviderModel("openai", "gpt-5.5"); !ok || err != nil {
			t.Fatalf("switch to a case-variant provider spelling failed: %s", status)
		}
		cfg := readTUIConfigFixture(t, configPath)
		if cfg.ActiveProvider != "OpenAI" {
			t.Fatalf("activeProvider = %q, want the row's spelling OpenAI", cfg.ActiveProvider)
		}
		if cfg.Providers[0].Model != "gpt-5.5" {
			t.Fatalf("model = %q, want gpt-5.5 persisted onto the OpenAI row", cfg.Providers[0].Model)
		}
	})
}

// The provider manager's rows and the picker's model sections are built from
// savedProviders, not from the live profile: a switch that updates the client
// and config.json without mirroring the list leaves those surfaces showing the
// previous model until the TUI restarts and re-resolves providers from config.
func TestModelSwitchSyncsSavedProviders(t *testing.T) {
	newSession := func(t *testing.T) model {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(`{"activeProvider":"OpenAI","providers":[{"name":"OpenAI","catalogID":"openai","model":"gpt-5.1"},{"name":"ollama","catalogID":"ollama","provider_kind":"openai-compatible","baseURL":"http://localhost:11434/v1","model":"m1"}]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		saved := []config.ProviderProfile{
			{Name: "OpenAI", CatalogID: "openai", Model: "gpt-5.1", APIKey: "sk-test"},
			{Name: "ollama", CatalogID: "ollama", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "http://localhost:11434/v1", Model: "m1"},
		}
		return newModel(context.Background(), Options{
			UserConfigPath:  path,
			ProviderName:    "ollama",
			ModelName:       "m1",
			Provider:        &fakeProvider{},
			ProviderProfile: saved[1],
			SavedProviders:  saved,
			NewProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
				return &fakeProvider{}, nil
			},
		})
	}

	t.Run("switchProviderModel", func(t *testing.T) {
		m := newSession(t)
		// "openai" is the picker row's owner spelling, not the persisted one:
		// the mirror must land on the row SetProviderModel actually wrote.
		next, status, ok, _, err := m.switchProviderModel("openai", "gpt-5.5")
		if !ok || err != nil {
			t.Fatalf("switch failed: %s", status)
		}
		if next.savedProviders[0].Model != "gpt-5.5" {
			t.Fatalf("savedProviders model = %q, want the switched gpt-5.5 without a restart", next.savedProviders[0].Model)
		}
		if next.savedProviders[1].Model != "m1" {
			t.Fatalf("switch touched an unrelated row: %+v", next.savedProviders[1])
		}
		// The manager renders each row's model straight off this list.
		if meta := providerManagerRowMeta(next.savedProviders[0]); !strings.Contains(meta, "gpt-5.5") {
			t.Fatalf("manager row meta = %q, want the switched model", meta)
		}
	})

	t.Run("a separately resolved spelling stays session only", func(t *testing.T) {
		m := newSession(t)
		// Resolve preserves a user row's spelling. A concrete lowercase row
		// beside persisted OpenAI is project/env-derived, not a session alias.
		m.savedProviders[0].Name = "openai"

		next, status, ok, _, err := m.switchProviderModel("openai", "gpt-5.5")
		if !ok || err != nil {
			t.Fatalf("switch failed: %s (err=%v)", status, err)
		}
		if next.providerProfile.Model != "gpt-5.5" || !strings.Contains(status, "session only") {
			t.Fatalf("expected a session-only model switch, status=%q", status)
		}
		cfg := readTUIConfigFixture(t, next.userConfigPath)
		if cfg.Providers[0].Name != "OpenAI" || cfg.Providers[0].Model != "gpt-5.1" {
			t.Fatal("project spelling changed the persisted user row")
		}
	})

	t.Run("handleModelCommand", func(t *testing.T) {
		m := newSession(t)
		// Exercise the production caller that owns both persistence and the
		// savedProviders mirror; calling the two helpers separately would stay
		// green if their production pairing were removed.
		m.providerName = "openai"
		m.providerProfile = m.savedProviders[0]
		m.modelName = m.providerProfile.Model
		next, status, err := m.handleModelCommand("gpt-4.1-mini")
		if err != nil {
			t.Fatal(err)
		}
		if next.savedProviders[0].Model != "gpt-4.1-mini" {
			t.Fatalf("savedProviders model = %q, want gpt-4.1-mini; status=%q", next.savedProviders[0].Model, status)
		}
		if next.savedProviders[1].Model != "m1" {
			t.Fatalf("mirror touched an unrelated row: %+v", next.savedProviders[1])
		}
	})
}

// The switch deliberately continues in-session when config.json cannot be
// updated, so the note is the only thing telling the user the two now disagree.
// Silence here would read as a saved switch that was never persisted.
func TestSwitchProviderModelReportsPersistenceFailures(t *testing.T) {
	saved := []config.ProviderProfile{
		{Name: "OpenAI", CatalogID: "openai", Model: "gpt-5.1", APIKey: "sk-test"},
		{Name: "ollama", CatalogID: "ollama", ProviderKind: config.ProviderKindOpenAICompatible, BaseURL: "http://localhost:11434/v1", Model: "m1"},
	}
	newSwitchModel := func(t *testing.T, configJSON string) model {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(configJSON), 0o600); err != nil {
			t.Fatal(err)
		}
		return newModel(context.Background(), Options{
			UserConfigPath:  path,
			ProviderName:    "ollama",
			ModelName:       "m1",
			Provider:        &fakeProvider{},
			ProviderProfile: saved[1],
			SavedProviders:  saved,
			NewProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
				return &fakeProvider{}, nil
			},
		})
	}

	t.Run("unreadable config", func(t *testing.T) {
		m := newSwitchModel(t, `{"providers":[`) // invalid JSON
		next, status, ok, _, err := m.switchProviderModel("OpenAI", "gpt-5.5")
		if err == nil {
			t.Fatal("unreadable config must return a persistence error")
		}
		if !ok {
			t.Fatalf("the in-session switch must still succeed: %s", status)
		}
		if !strings.Contains(status, "config.json could not be read") {
			t.Fatalf("status = %q, want a persistence note", status)
		}
		if strings.Count(status, err.Error()) != 1 {
			t.Fatal("persistence error must appear exactly once in the switch status")
		}
		// The session did switch, which is exactly why the note has to be there.
		if next.providerName != "OpenAI" {
			t.Fatalf("providerName = %q, want OpenAI", next.providerName)
		}
	})

	t.Run("ambiguous rows block the write", func(t *testing.T) {
		// Duplicate case variants pass the persisted gate but make the write
		// itself unresolvable.
		m := newSwitchModel(t, `{"providers":[{"name":"OpenAI"},{"name":"openai"}]}`)
		_, status, ok, _, err := m.switchProviderModel("OpenAI", "gpt-5.5")
		if err == nil {
			t.Fatal("invalid config must return a persistence error")
		}
		if !ok {
			t.Fatalf("the in-session switch must still succeed: %s", status)
		}
		if !strings.Contains(status, "config.json was not updated") {
			t.Fatalf("status = %q, want the active-provider persistence note", status)
		}
		if strings.Count(status, err.Error()) != 1 {
			t.Fatal("persistence error must appear exactly once in the switch status")
		}
	})

	t.Run("env-derived provider stays silent", func(t *testing.T) {
		// No row to update is not a failure, so it must not produce a note.
		m := newSwitchModel(t, `{"providers":[{"name":"ollama","model":"m1"}]}`)
		_, status, ok, _, err := m.switchProviderModel("OpenAI", "gpt-5.5")
		if !ok || err != nil {
			t.Fatalf("switch failed: %s", status)
		}
		if strings.Contains(status, "Note:") {
			t.Fatalf("status = %q, want no note for a provider with no persisted row", status)
		}
	})
}
