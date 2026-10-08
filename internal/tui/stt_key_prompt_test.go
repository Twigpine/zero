package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Gitlawb/zero/internal/config"
)

func TestCloudKeyPromptOpensWhenNoKey(t *testing.T) {
	m := model{}
	m.dictation.keyAvailable = func(string) bool { return false } // not authenticated
	m.dictation.saveKey = func(string, string) error { return nil }

	next, text := m.handleSTTModelSelection("groq:whisper-large-v3-turbo")
	if next.sttKeyPrompt == nil {
		t.Fatal("selecting a keyless cloud provider should open the API-key prompt")
	}
	if next.sttKeyPrompt.provider != "groq" || next.sttKeyPrompt.label != "Groq" {
		t.Errorf("prompt = %+v", next.sttKeyPrompt)
	}
	if text != "" {
		t.Errorf("no notice should be appended when the prompt opens, got %q", text)
	}
}

func TestCloudKeyPromptCancelKeepsPreviousProvider(t *testing.T) {
	// Regression: selecting a keyless cloud provider must NOT switch the active
	// provider until the key is actually saved — otherwise cancelling the prompt
	// strands the session on a half-chosen provider carrying the old model.
	m := model{}
	m.dictation.cfg = config.STTConfig{Provider: config.STTProviderGroq, Model: "whisper-large-v3-turbo"}
	m.dictation.keyAvailable = func(string) bool { return false } // OpenAI has no key
	m.dictation.saveKey = func(string, string) error { return nil }

	next, _ := m.handleSTTModelSelection("openai:whisper-1")
	if next.sttKeyPrompt == nil {
		t.Fatal("selecting a keyless cloud provider should open the key prompt")
	}
	if next.dictation.cfg.Provider != config.STTProviderGroq || next.dictation.cfg.Model != "whisper-large-v3-turbo" {
		t.Fatalf("active provider/model changed before commit: %q/%q",
			next.dictation.cfg.Provider, next.dictation.cfg.Model)
	}

	esc, _ := next.handleSTTKeyPromptKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyEsc}))
	got := esc.(model)
	if got.sttKeyPrompt != nil {
		t.Error("Esc should close the prompt")
	}
	if got.dictation.cfg.Provider != config.STTProviderGroq || got.dictation.cfg.Model != "whisper-large-v3-turbo" {
		t.Errorf("after cancel, provider/model = %q/%q; want the working Groq selection preserved",
			got.dictation.cfg.Provider, got.dictation.cfg.Model)
	}
}

func TestCloudKeyPromptOptionalWhenAuthenticated(t *testing.T) {
	// Selecting a provider that already has a key still opens the prompt — but as an
	// OPTIONAL pass, so an invalid/old key can be replaced proactively (without first
	// suffering a failed recording). Esc keeps the saved key and applies the model.
	dir := t.TempDir()
	m := model{userConfigPath: dir + "/config.json"}
	m.dictation.keyAvailable = func(string) bool { return true }
	m.dictation.saveKey = func(string, string) error { return nil }

	next, _ := m.handleSTTModelSelection("groq:whisper-large-v3-turbo")
	if next.sttKeyPrompt == nil || !next.sttKeyPrompt.optional {
		t.Fatalf("expected an optional key prompt, got %+v", next.sttKeyPrompt)
	}

	esc, _ := next.handleSTTKeyPromptKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyEsc}))
	got := esc.(model)
	if got.sttKeyPrompt != nil {
		t.Error("Esc should close the optional prompt")
	}
	if got.dictation.cfg.Provider != config.STTProviderGroq || got.dictation.cfg.Model != "whisper-large-v3-turbo" {
		t.Errorf("model not applied on keep: %q/%q", got.dictation.cfg.Provider, got.dictation.cfg.Model)
	}
	if !strings.Contains(got.transientNotice.text, "Kept your saved Groq API key") {
		t.Error("expected a kept-key notice")
	}
}

func TestCloudKeyPromptOptionalReplaceSavesNewKey(t *testing.T) {
	// Typing a new key in the optional prompt replaces the saved one and applies the model.
	dir := t.TempDir()
	var savedKey string
	m := model{userConfigPath: dir + "/config.json"}
	m.dictation.keyAvailable = func(string) bool { return true }
	m.dictation.saveKey = func(_, k string) error { savedKey = k; return nil }

	next, _ := m.handleSTTModelSelection("groq:whisper-large-v3-turbo")
	m2 := next
	for _, r := range []string{"g", "s", "k", "-", "n", "e", "w"} {
		u, _ := m2.handleSTTKeyPromptKey(tea.KeyPressMsg(tea.Key{Text: r}))
		m2 = u.(model)
	}
	done, _ := m2.handleSTTKeyPromptKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	got := done.(model)
	if savedKey != "gsk-new" {
		t.Errorf("replacement key = %q, want gsk-new", savedKey)
	}
	if got.dictation.cfg.Model != "whisper-large-v3-turbo" {
		t.Errorf("model not applied after replace: %q", got.dictation.cfg.Model)
	}
	if !strings.Contains(got.transientNotice.text, "Saved your Groq API key") {
		t.Error("expected a saved-key notice")
	}
}

func TestCloudKeySavedAndModelApplied(t *testing.T) {
	dir := t.TempDir()
	var savedProvider, savedKey string
	m := model{userConfigPath: dir + "/config.json"}
	m.dictation.saveKey = func(p, k string) error { savedProvider, savedKey = p, k; return nil }
	m.sttKeyPrompt = &sttKeyPromptState{provider: "groq", label: "Groq", modelValue: "groq:whisper-large-v3-turbo", input: "gsk-secret"}

	next, _ := m.submitSTTKey()
	got := next.(model)
	if savedProvider != "groq" || savedKey != "gsk-secret" {
		t.Errorf("saved key = %q/%q", savedProvider, savedKey)
	}
	if got.sttKeyPrompt != nil {
		t.Error("prompt should close after submit")
	}
	if got.dictation.cfg.Model != "whisper-large-v3-turbo" {
		t.Errorf("model not applied: %q", got.dictation.cfg.Model)
	}
	if !strings.Contains(got.transientNotice.text, "Saved your Groq API key") {
		t.Error("expected a saved-key notice")
	}
}

func TestCloudKeyPromptTypingBackspaceAndCancel(t *testing.T) {
	m := model{sttKeyPrompt: &sttKeyPromptState{provider: "groq", label: "Groq"}}
	for _, r := range []string{"a", "b", "c"} {
		next, _ := m.handleSTTKeyPromptKey(tea.KeyPressMsg(tea.Key{Text: r}))
		m = next.(model)
	}
	if m.sttKeyPrompt.input != "abc" {
		t.Fatalf("input = %q, want abc", m.sttKeyPrompt.input)
	}
	back, _ := m.handleSTTKeyPromptKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyBackspace}))
	m = back.(model)
	if m.sttKeyPrompt.input != "ab" {
		t.Errorf("after backspace: %q", m.sttKeyPrompt.input)
	}
	esc, _ := m.handleSTTKeyPromptKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyEsc}))
	if esc.(model).sttKeyPrompt != nil {
		t.Error("Esc should cancel the prompt")
	}
}
