package cli

import (
	"fmt"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/dictation"
	"github.com/Gitlawb/zero/internal/tui"
)

// Default batch cloud models (§8). Overridable via stt.model.
const (
	defaultGroqSTTModel   = "whisper-large-v3-turbo"
	defaultOpenAISTTModel = "whisper-1"
	groqSTTBaseURL        = "https://api.groq.com/openai/v1"
)

// newDictationTranscriberFactory returns the closure the TUI calls to build a
// transcriber when a recording starts. It lives here, not in the TUI, because it
// resolves provider API keys (credstore + env + configured providers) — logic
// that belongs with the rest of the CLI's credential handling.
//
// serverMgr is the shared, session-long sherpa-onnx streaming server; it is
// spawned lazily on the first local-streaming recording and torn down on exit.
//
// preferStreaming asks for the streaming backend; the returned bool reports
// whether streaming was actually built (false → the caller uses the batch
// pipeline). A misconfigured streaming backend surfaces its own SetupError.
func newDictationTranscriberFactory(resolved config.ResolvedConfig, userConfigPath string, serverMgr *dictation.ServerManager) func(config.STTConfig, bool) (tui.Transcriber, bool, error) {
	return func(stt config.STTConfig, preferStreaming bool) (tui.Transcriber, bool, error) {
		// stt is passed in (not captured) so a mid-session change — e.g. the F9
		// auto-download writing localModelPath — takes effect on the next call.
		if serverMgr != nil {
			serverMgr.SetModelPath(stt.LocalModelPath)
		}
		if preferStreaming {
			t, err := buildStreamingTranscriber(stt, resolved, userConfigPath, serverMgr)
			switch {
			case err == nil && t != nil:
				return t, true, nil
			case err != nil && stt.STTStreamProvider() != config.STTProviderLocal:
				// An explicitly-chosen cloud streaming backend that failed to build
				// (e.g. a missing key) should surface its own error, not silently
				// fall back to batch — the user asked for that backend specifically.
				return nil, true, err
			}
			// The default local streaming backend isn't set up: fall through to the
			// batch pipeline so a cloud-batch provider (or a batch-only setup) still
			// works instead of erroring on a streaming server the user never asked for.
		}
		t, err := buildBatchTranscriber(stt, resolved, userConfigPath)
		return t, false, err
	}
}

// buildStreamingTranscriber constructs the configured streaming backend, or
// returns (nil, nil) when streaming is not applicable so the caller falls back
// to batch.
func buildStreamingTranscriber(stt config.STTConfig, resolved config.ResolvedConfig, userConfigPath string, serverMgr *dictation.ServerManager) (dictation.Transcriber, error) {
	switch stt.STTStreamProvider() {
	case config.STTProviderDeepgram:
		key := resolveSTTKey(resolved, userConfigPath, dictation.StreamProviderDeepgram, "DEEPGRAM_API_KEY")
		return dictation.NewDeepgramTranscriber(dictation.DeepgramConfig{
			APIKey:   key,
			Model:    stt.StreamModel,
			Language: stt.Language,
		})
	case config.STTProviderOpenAI:
		key := resolveSTTKey(resolved, userConfigPath, dictation.ProviderOpenAI, "OPENAI_API_KEY")
		return dictation.NewOpenAIRealtimeTranscriber(dictation.OpenAIRealtimeConfig{
			APIKey: key,
			Model:  stt.StreamModel,
		})
	default: // local streaming via the warm sherpa-onnx server
		// STTStreamProvider() defaults to "local" whenever streamProvider is unset —
		// including when the user picked a CLOUD batch provider (Groq/OpenAI) and
		// never touched streaming. Don't silently stream through the local sherpa
		// server in that case: local streaming is only appropriate when it was
		// explicitly requested (streamProvider: "local") OR the batch provider is
		// itself local. Otherwise report "not applicable" so the factory falls back
		// to the cloud batch pipeline the user actually chose.
		localWanted := stt.StreamProvider == config.STTProviderLocal || stt.STTProvider() == config.STTProviderLocal
		if !localWanted {
			return nil, nil
		}
		// The local streaming transcriber validates the model only lazily (at
		// connect time), so gate on the model path here — with none set, report
		// "not applicable" (nil, nil) so the factory falls back to the batch
		// pipeline instead of committing to a server that can't start.
		if serverMgr == nil || strings.TrimSpace(stt.LocalModelPath) == "" {
			return nil, nil
		}
		return dictation.NewLocalStreamingTranscriber(serverMgr), nil
	}
}

// newDictationServerManager builds the warm sherpa-onnx streaming server manager
// from config. Construction is side-effect free — the server spawns lazily on
// the first local-streaming recording — so it is always safe to create.
func newDictationServerManager(stt config.STTConfig) *dictation.ServerManager {
	return dictation.NewServerManager(dictation.ServerConfig{
		Binary:     stt.LocalServerBinary,
		ModelPath:  stt.LocalModelPath,
		Port:       stt.LocalServerPort,
		NumThreads: stt.NumThreads,
	})
}

// buildBatchTranscriber constructs the configured batch transcriber, resolving
// credentials for the cloud providers.
func buildBatchTranscriber(stt config.STTConfig, resolved config.ResolvedConfig, userConfigPath string) (dictation.Transcriber, error) {
	switch stt.STTProvider() {
	case config.STTProviderGroq:
		key := resolveSTTKey(resolved, userConfigPath, dictation.ProviderGroq, "GROQ_API_KEY")
		model := stt.Model
		if model == "" {
			model = defaultGroqSTTModel
		}
		return dictation.NewCloudTranscriber(dictation.CloudConfig{
			Provider: dictation.ProviderGroq,
			BaseURL:  groqSTTBaseURL,
			APIKey:   key,
			Model:    model,
			Language: stt.Language,
		})
	case config.STTProviderOpenAI:
		key := resolveSTTKey(resolved, userConfigPath, dictation.ProviderOpenAI, "OPENAI_API_KEY")
		model := stt.Model
		if model == "" {
			model = defaultOpenAISTTModel
		}
		return dictation.NewCloudTranscriber(dictation.CloudConfig{
			Provider: dictation.ProviderOpenAI,
			BaseURL:  config.OpenAIBaseURL,
			APIKey:   key,
			Model:    model,
			Language: stt.Language,
		})
	default: // local
		return dictation.NewLocalTranscriber(dictation.LocalConfig{
			Binary:     stt.LocalBinary,
			ModelPath:  stt.LocalModelPath,
			NumThreads: stt.NumThreads,
		})
	}
}

// sttEnvVar returns the conventional environment variable a cloud STT provider's
// key is read from.
func sttEnvVar(provider string) string {
	switch provider {
	case dictation.ProviderGroq:
		return "GROQ_API_KEY"
	case dictation.ProviderOpenAI: // == StreamProviderOpenAI ("openai")
		return "OPENAI_API_KEY"
	case dictation.StreamProviderDeepgram:
		return "DEEPGRAM_API_KEY"
	}
	return ""
}

// newSTTKeyStatus returns a predicate reporting whether a cloud STT provider
// already has a resolvable API key (configured provider, credstore, or env).
func newSTTKeyStatus(resolved config.ResolvedConfig, userConfigPath string) func(string) bool {
	return func(provider string) bool {
		return resolveSTTKey(resolved, userConfigPath, provider, sttEnvVar(provider)) != ""
	}
}

// newSaveSTTKey returns a function that stores a cloud STT provider's API key in
// the encrypted credential store, so the inline prompt can persist it.
func newSaveSTTKey(userConfigPath string) func(string, string) error {
	return func(provider, key string) error {
		if strings.TrimSpace(userConfigPath) == "" {
			return fmt.Errorf("no config path to store the key")
		}
		return config.StoreProviderCredential(userConfigPath, provider, strings.TrimSpace(key))
	}
}

// resolveSTTKey finds an API key for an STT cloud provider, reusing Zero's
// existing credentials (§2 — "zero new credential UI"). Precedence: a matching
// configured provider's resolved key, then the encrypted credential store, then
// the provider's conventional environment variable.
func resolveSTTKey(resolved config.ResolvedConfig, userConfigPath, provider, envVar string) string {
	if key := keyFromConfiguredProviders(resolved, provider); key != "" {
		return key
	}
	if userConfigPath != "" {
		if store, err := config.ProviderKeyStoreAt(filepath.Dir(userConfigPath)); err == nil {
			if key, ok, _ := store.Get(provider); ok && strings.TrimSpace(key) != "" {
				return strings.TrimSpace(key)
			}
		}
	}
	return strings.TrimSpace(os.Getenv(envVar))
}

// keyFromConfiguredProviders returns the resolved API key of a configured
// provider that belongs to the given cloud provider (matched by catalog id,
// provider name/kind, or base-URL host), or "" when none is configured.
func keyFromConfiguredProviders(resolved config.ResolvedConfig, provider string) string {
	provider = strings.ToLower(provider)
	profiles := append([]config.ProviderProfile{resolved.Provider}, resolved.Providers...)
	for _, p := range profiles {
		if strings.TrimSpace(p.APIKey) == "" {
			continue
		}
		if providerMatches(p, provider) {
			return strings.TrimSpace(p.APIKey)
		}
	}
	return ""
}

func providerMatches(p config.ProviderProfile, provider string) bool {
	provider = strings.ToLower(provider)
	// Match on stable identifiers (catalog id, provider name/kind, profile
	// name) rather than raw BaseURL substring matching: a Groq profile's
	// BaseURL (api.groq.com/openai/v1) contains "openai", so a substring
	// check can hand the wrong key to an OpenAI STT lookup and fail auth.
	for _, f := range []string{
		strings.ToLower(p.CatalogID),
		strings.ToLower(p.Provider),
		strings.ToLower(string(p.ProviderKind)),
		strings.ToLower(p.Name),
	} {
		if f != "" && strings.Contains(f, provider) {
			return true
		}
	}
	// BaseURL: compare the parsed host only, not a raw substring on the
	// full URL (which lets "openai" match api.groq.com/openai/v1).
	if u, err := neturl.Parse(p.BaseURL); err == nil && u.Host != "" {
		if host := strings.ToLower(u.Host); strings.Contains(host, provider) {
			return true
		}
	}
	return false
}
