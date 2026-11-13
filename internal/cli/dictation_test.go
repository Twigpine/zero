package cli

import (
	"errors"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/dictation"
)

func TestFactoryFallsBackToBatchWhenLocalStreamingNotConfigured(t *testing.T) {
	t.Setenv("GROQ_API_KEY", "gsk-test")
	resolved := config.ResolvedConfig{STT: config.STTConfig{
		Provider: config.STTProviderGroq, // cloud batch
		// StreamProvider unset → defaults to local, which has no model configured.
	}}
	factory := newDictationTranscriberFactory(resolved, "", newDictationServerManager(resolved.STT))

	tr, streaming, err := factory(resolved.STT, true) // prefer streaming
	if err != nil {
		t.Fatalf("expected batch fallback, got error: %v", err)
	}
	if streaming {
		t.Error("should have fallen back to batch (streaming=false)")
	}
	if tr == nil {
		t.Error("expected a Groq batch transcriber")
	}
}

func TestFactoryCloudProviderIgnoresLeftoverLocalModel(t *testing.T) {
	// Regression: pick local once (LocalModelPath gets set), then switch to a
	// cloud batch provider. streamProvider stays unset (defaults to "local"), so
	// the streaming factory must NOT fire up the sherpa server on the leftover
	// path — it should fall back to the cloud batch pipeline the user chose.
	t.Setenv("GROQ_API_KEY", "gsk-test")
	stt := config.STTConfig{
		Provider:       config.STTProviderGroq, // cloud batch, chosen now
		LocalModelPath: "/leftover/model/dir",  // set by an earlier local selection
	}
	resolved := config.ResolvedConfig{STT: stt}
	factory := newDictationTranscriberFactory(resolved, "", newDictationServerManager(stt))

	tr, streaming, err := factory(stt, true) // prefer streaming
	if err != nil {
		t.Fatalf("expected cloud batch fallback, got error: %v", err)
	}
	if streaming {
		t.Error("a cloud provider must not stream through the local sherpa server")
	}
	if tr == nil {
		t.Error("expected a Groq batch transcriber")
	}
}

func TestFactorySurfacesExplicitCloudStreamingError(t *testing.T) {
	// Deepgram chosen explicitly but no key resolvable → surface the setup error.
	resolved := config.ResolvedConfig{STT: config.STTConfig{
		StreamProvider: config.STTProviderDeepgram,
	}}
	factory := newDictationTranscriberFactory(resolved, "", nil)

	_, streaming, err := factory(resolved.STT, true)
	if err == nil {
		t.Fatal("expected a setup error for missing Deepgram key")
	}
	if !streaming {
		t.Error("an explicit cloud streaming choice should report streaming=true even on error")
	}
	var setupErr *dictation.SetupError
	if !errors.As(err, &setupErr) {
		t.Errorf("want *SetupError, got %v", err)
	}
}

func TestFactoryLocalBatchSetupErrorWhenNoModel(t *testing.T) {
	resolved := config.ResolvedConfig{STT: config.STTConfig{Provider: config.STTProviderLocal}}
	factory := newDictationTranscriberFactory(resolved, "", newDictationServerManager(resolved.STT))

	_, _, err := factory(resolved.STT, true)
	var setupErr *dictation.SetupError
	if !errors.As(err, &setupErr) {
		t.Fatalf("want *SetupError guiding local model setup, got %v", err)
	}
}

func TestResolveSTTKeyPrefersConfiguredProvider(t *testing.T) {
	resolved := config.ResolvedConfig{
		Providers: []config.ProviderProfile{
			{Name: "groq", CatalogID: "groq", BaseURL: "https://api.groq.com/openai/v1", APIKey: "gsk-configured"},
		},
	}
	if got := resolveSTTKey(resolved, "", dictation.ProviderGroq, "GROQ_API_KEY"); got != "gsk-configured" {
		t.Errorf("key = %q, want the configured provider's key", got)
	}
}
