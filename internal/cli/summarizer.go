package cli

import (
	"context"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/providers"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// summarizerFactory adapts the resolved profile and the authenticated provider
// builder into agent.Options.Summarizer; the selection rules live in
// providers.CompactionSummarizerFactory, shared with the TUI.
func summarizerFactory(resolved config.ResolvedConfig, newProvider func(config.ProviderProfile) (zeroruntime.Provider, error)) func(context.Context, string) (agent.Provider, error) {
	return providers.CompactionSummarizerFactory(resolved.Provider, resolved.Preferences.CompactionModel, newProvider)
}
