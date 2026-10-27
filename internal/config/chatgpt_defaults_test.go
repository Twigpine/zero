package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureChatGPTDefaultPreservesSavedSelection(t *testing.T) {
	for _, saved := range []string{"", "gpt-explicit-saved"} {
		name := "new profile"
		if saved != "" {
			name = "saved selection"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if saved != "" {
				body, err := json.Marshal(FileConfig{ActiveProvider: "codex", Providers: []ProviderProfile{{Name: "codex", CatalogID: "chatgpt", Model: saved}}})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			ensured, err := EnsureCatalogProvider(path, "chatgpt")
			if err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var cfg FileConfig
			if err := json.Unmarshal(body, &cfg); err != nil {
				t.Fatal(err)
			}
			want := "gpt-6.1-sol"
			if saved != "" {
				want = saved
			}
			if len(cfg.Providers) != 1 || cfg.Providers[0].Model != want {
				t.Fatalf("ChatGPT default/selection = %#v, want model %q", cfg.Providers, want)
			}
			if saved != "" && (ensured.Name != "codex" || ensured.Created) {
				t.Fatalf("saved subscription profile was replaced: %#v", ensured)
			}
		})
	}
}
