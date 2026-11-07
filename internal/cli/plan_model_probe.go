package cli

import (
	"context"
	"strings"
	"time"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/specialist"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// probeTimeout bounds one model's proof. Generous enough for a cold model to
// load and short enough that proving nineteen of them concurrently costs one
// pause rather than a stall — and a model too slow to answer this is not one a
// plan wants either.
const probeTimeout = 20 * time.Second

// planModelProber proves that a provider will actually RUN a model.
//
// DISCOVERY IS AN ADVERTISEMENT AND THIS IS THE RECEIPT. /v1/models reports what
// a provider is willing to name; only a real request reports what it will serve.
// Every model failure this feature has produced lived in that gap:
// grok-4.20-multi-agent-0309 lists like anything else and answers "Multi Agent
// requests are not allowed on chat completions"; a stale list from another
// provider lists perfectly and serves nothing.
//
// The request is deliberately the smallest real one: a single word, one token
// back. It proves the route end to end — credential, endpoint, model id,
// entitlement — which is exactly the set of things a list cannot prove.
func planModelProber(workspaceRoot string, profile config.ProviderProfile, newProvider func(config.ProviderProfile) (zeroruntime.Provider, error)) specialist.ModelProber {
	if newProvider == nil {
		return nil
	}
	return func(ctx context.Context, modelID string) specialist.ModelProbeResult {
		id := strings.TrimSpace(modelID)
		if id == "" {
			return specialist.ModelProbeResult{Verdict: specialist.ProbeUnknown}
		}
		// The LIVE profile, for the same reason discovery re-resolves it: a
		// session that switched provider must be proved against the one it is
		// actually on, not the one it started with.
		target := livePlanProvider(workspaceRoot, profile)
		target.Model = id
		provider, err := newProvider(discoveryCredentialProfile(target))
		if err != nil {
			// Could not even build a client: a fact about this run, not about
			// this model. Unknown keeps the model.
			return specialist.ModelProbeResult{Verdict: specialist.ProbeUnknown, Reason: err.Error()}
		}

		probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		stream, err := provider.StreamCompletion(probeCtx, zeroruntime.CompletionRequest{
			Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "ping"}},
		})
		if err != nil {
			return specialist.ClassifyProbeError(err)
		}
		collected := zeroruntime.CollectStream(probeCtx, stream)
		if collected.Error != "" {
			// A refusal usually arrives HERE rather than from StreamCompletion:
			// the request is accepted, the stream opens, and the provider's
			// complaint about the model comes back as the first event. Reading
			// only the call's own error is how "the model does not exist" was
			// mistaken for a working model.
			return specialist.ClassifyProbeError(errStub(collected.Error))
		}
		return specialist.ModelProbeResult{Verdict: specialist.ProbeServes}
	}
}

// planModelProbeIdentity returns the non-secret routing identity that makes a
// probe verdict true. Model IDs overlap across providers, profiles and custom
// endpoints, so none of those dimensions may share a cache namespace.
func planModelProbeIdentity(workspaceRoot string, profile config.ProviderProfile) func() string {
	return func() string {
		live := livePlanProvider(workspaceRoot, profile)
		return strings.Join([]string{
			strings.TrimSpace(live.Name),
			strings.TrimSpace(live.Provider),
			strings.TrimSpace(string(live.ProviderKind)),
			strings.TrimSpace(live.CatalogID),
			strings.TrimSpace(live.BaseURL),
			strings.TrimSpace(live.APIFormat),
			strings.TrimSpace(live.APIKeyEnv),
		}, "\x00")
	}
}

// errStub carries a provider's message into the classifier, which takes an error
// because every other caller has one.
type errStub string

func (e errStub) Error() string { return string(e) }

// probeStatusWord names a verdict for a person. The three words are deliberately
// distinct: "unreachable" is a fact about the connection, not about the model,
// and reading it as a refusal is how a working model gets dropped on a flaky
// network.
func probeStatusWord(v specialist.ModelProbeVerdict) string {
	switch v {
	case specialist.ProbeServes:
		return "serves"
	case specialist.ProbeRefuses:
		return "refuses"
	default:
		return "unreachable"
	}
}

// firstProbeLine keeps a provider's complaint to one readable line; these arrive
// as multi-line JSON often enough to matter.
func firstProbeLine(s string) string {
	if index := strings.IndexAny(s, "\r\n"); index >= 0 {
		s = s[:index]
	}
	const limit = 120
	if len(s) > limit {
		s = s[:limit] + "…"
	}
	return strings.TrimSpace(s)
}
