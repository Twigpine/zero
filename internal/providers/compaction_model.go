package providers

import (
	"context"
	"os"
	"strings"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// Compaction summarization model selection.
//
// Compaction summaries used to run on the session's main model — near a 200k
// window one summarization is a ~100k-token input call at full price, the
// single most expensive recurring event in a long run. A cheap model handles
// the "dense factual summary" task fine, so summarization is routed to one
// whenever the target is knowable.
//
// Resolution order: ZERO_COMPACTION_MODEL env > preferences.compactionModel
// config > a curated per-kind default (OFFICIAL endpoints only) > "" (use the
// main model). The literal value "main" at any level forces the main model.
// A wrong choice is never fatal: the agent falls back to the main provider on
// the first summarizer failure for the rest of the run.

// CompactionModelEnv overrides the compaction summarization model.
const CompactionModelEnv = "ZERO_COMPACTION_MODEL"

// compactionMainSentinel disables the dedicated summarizer explicitly.
const compactionMainSentinel = "main"

// Curated cheap summarizer defaults, applied only for provider kinds whose
// model catalog is guaranteed (official endpoints). Custom / *-compatible
// endpoints get NO default — their catalogs are unknowable, and a guaranteed
// failed call per run is worse than main-model prices.
const (
	defaultAnthropicCompactionModel = "claude-haiku-4-5-20251001"
	defaultGoogleCompactionModel    = "gemini-2.5-flash-lite"
	defaultOpenAICompactionModel    = "gpt-4.1-mini"
)

// CompactionModelID returns the model that compaction summarization calls
// should use for the given profile, or "" to use the session's main model.
// preference is the resolved config value (preferences.compactionModel).
func CompactionModelID(profile config.ProviderProfile, preference string) string {
	value := strings.TrimSpace(os.Getenv(CompactionModelEnv))
	if value == "" {
		value = strings.TrimSpace(preference)
	}
	if value != "" {
		if strings.EqualFold(value, compactionMainSentinel) {
			return ""
		}
		return value
	}
	return defaultCompactionModel(profile)
}

func defaultCompactionModel(profile config.ProviderProfile) string {
	metadata, err := ResolveRuntimeMetadata(profile, Options{})
	if err != nil {
		return ""
	}
	var candidate string
	switch metadata.ProviderKind {
	case config.ProviderKindAnthropic:
		candidate = defaultAnthropicCompactionModel
	case config.ProviderKindGoogle:
		candidate = defaultGoogleCompactionModel
	case config.ProviderKindOpenAI:
		candidate = defaultOpenAICompactionModel
	default:
		return ""
	}
	// Session already runs on the cheap model: a dedicated summarizer buys
	// nothing and would only add a second provider instance.
	if strings.EqualFold(strings.TrimSpace(metadata.APIModel), candidate) {
		return ""
	}
	return candidate
}

// CompactionSummarizerFactory returns the lazy constructor the agent loop
// calls for its dedicated compaction summarizer (agent.Options.Summarizer).
// The compactor passes the main model in force at that moment — on the first
// paid compaction and again after a mid-run model switch — so the choice
// tracks escalations: a run that starts on the cheap model gets no dedicated
// summarizer, and gains one once it escalates to a pricier main model.
// Returning (nil, nil) means "summarize on the main provider". The factory
// itself is nil only when no provider builder exists. Shared by the exec and
// TUI front ends so the selection rules cannot drift between them.
func CompactionSummarizerFactory(base config.ProviderProfile, preference string, newProvider func(config.ProviderProfile) (zeroruntime.Provider, error)) func(context.Context, string) (zeroruntime.Provider, error) {
	if newProvider == nil {
		return nil
	}
	return func(_ context.Context, mainModelID string) (zeroruntime.Provider, error) {
		profile := base
		if strings.TrimSpace(mainModelID) != "" {
			profile.Model = mainModelID
		}
		modelID := CompactionModelID(profile, preference)
		if modelID == "" {
			return nil, nil
		}
		profile.Model = modelID
		return newProvider(profile)
	}
}
