package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/providercatalog"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// The in-session /provider wizard persists straight through config.UpsertProvider,
// bypassing the CLI providers add/setup guard, so it re-applies the same rule via
// isAtomicLocalPlaceholderModel before saving. Persisting the "local-model"
// placeholder (or an empty model) for atomic-chat-local yields a profile that
// fails on its first request.
func TestIsAtomicLocalPlaceholderModel(t *testing.T) {
	cases := []struct {
		name         string
		providerID   string
		model        string
		defaultModel string
		want         bool
	}{
		{"placeholder", "atomic-chat-local", "local-model", "local-model", true},
		{"empty", "atomic-chat-local", "", "local-model", true},
		{"whitespace only", "atomic-chat-local", "   ", "local-model", true},
		{"real served model", "atomic-chat-local", "unsloth/gemma-4-E2B-it-GGUF", "local-model", false},
		{"other local provider is untouched", "lmstudio", "local-model", "local-model", false},
		{"remote atomic-chat is untouched", "atomic-chat", "gpt-4.1", "gpt-4.1", false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := isAtomicLocalPlaceholderModel(tt.providerID, tt.model, tt.defaultModel); got != tt.want {
				t.Fatalf("isAtomicLocalPlaceholderModel(%q, %q, %q) = %v, want %v",
					tt.providerID, tt.model, tt.defaultModel, got, tt.want)
			}
		})
	}
}

func TestApplyProviderWizardRejectsAtomicPlaceholderBeforePersist(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", root)
	t.Setenv("XDG_CACHE_HOME", root)
	t.Setenv("APPDATA", root)
	t.Setenv("LOCALAPPDATA", root)
	t.Setenv("ZERO_PROVIDER", "original")
	descriptor, err := providercatalog.Require("atomic-chat-local")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.json")
	original := []byte(`{"providers":[]}`)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	built := false
	m := model{
		userConfigPath: path,
		providerName:   "original",
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			built = true
			return nil, nil
		},
		providerWizard: &providerWizardState{
			step:        providerWizardStepDone,
			providers:   []providercatalog.Descriptor{descriptor},
			models:      []providerWizardModel{{ID: "local-model"}},
			modelSource: "live",
		},
	}
	next, _ := m.applyProviderWizard()
	if next.providerWizard == nil || !strings.Contains(next.providerWizard.err, "placeholder") || built {
		t.Fatalf("placeholder reached provider construction or persistence: built=%v wizard=%+v", built, next.providerWizard)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(original) || next.providerName != "original" || os.Getenv("ZERO_PROVIDER") != "original" {
		t.Fatalf("rejected setup mutated configuration: data=%q err=%v", data, err)
	}
}
