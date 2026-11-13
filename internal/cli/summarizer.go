package cli

import (
	"context"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/providers"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// summarizerFactory adapts the resolved profile and the authenticated provider
// builder into agent.Options.Summarizer; the selection rules live in
// providers.CompactionSummarizerFactory, shared with the TUI.
func summarizerFactory(resolved config.ResolvedConfig, newProvider func(config.ProviderProfile) (zeroruntime.Provider, error)) func(context.Context, string) (agent.Provider, error) {
	return providers.CompactionSummarizerFactory(resolved.Provider, resolved.Preferences.CompactionModel, newProvider)
}
