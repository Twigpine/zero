package specialist

import (
	"context"
	"strings"
	"sync"
)

// Proving a model, as opposed to believing a list.
//
// DISCOVERY READS /v1/models, WHICH IS AN ADVERTISEMENT. It reports what a
// provider is willing to name, not what it will actually run, and the gap
// between those is where this feature has repeatedly fallen in:
//
//   - grok-4.20-multi-agent-0309 lists like any other model and answers
//     "Multi Agent requests are not allowed on chat completions". Six tasks were
//     assigned it; all six died.
//   - A model list fetched for one provider while the session had switched to
//     another lists perfectly and serves nothing.
//   - grok-build-0.1 and grok-imagine-video-1.5 list, price high enough to win
//     the strong tier, and fail every task they touch.
//
// The user's answer so far has been a hand-maintained exclude list — three
// entries, each added after a plan died on it. A probe replaces that with
// evidence: one trivial request per candidate, once, and a model that will not
// answer it is not offered to the router.

// ModelProbeVerdict is what a single probe learned.
type ModelProbeVerdict int

const (
	// ProbeUnknown means the probe could not reach a conclusion — a timeout, a
	// transport error, a provider that was briefly unwell. NOT a refusal: the
	// model keeps its place, because "we could not ask" and "it said no" call for
	// opposite responses and conflating them would drop a working model on a
	// flaky network.
	ProbeUnknown ModelProbeVerdict = iota
	// ProbeServes means the model answered.
	ProbeServes
	// ProbeRefuses means the provider said this model will not serve this kind of
	// request — no such model, no access, wrong endpoint. Unambiguous, and the
	// only verdict that removes a candidate.
	ProbeRefuses
)

// maxProbesInFlight caps how many models are probed at once.
//
// Sized for the round trip, not for the catalogue: eight requests overlap enough
// that proving a typical nineteen-model list still costs about three round trips
// rather than nineteen, while a provider advertising hundreds of models no longer
// sees a burst it answers with 429. A 429 classifies as ProbeUnknown, so an
// unbounded fan-out was the worst of both — it paid for every request, learned
// nothing from any of them, and left the plan's own tasks throttled behind it.
const maxProbesInFlight = 8

// ModelProbeResult is one model's verdict and the provider's own words for it.
type ModelProbeResult struct {
	Verdict ModelProbeVerdict
	// Reason is the provider's message, kept verbatim so a report can say WHY a
	// model was dropped rather than only that it was.
	Reason string
}

// ModelProber asks a provider whether it will actually run a model. Supplied as
// a hook for the same reason ModelDiscoverer is: internal/specialist runs on the
// child-execution path and must not drag the provider stack in behind it.
type ModelProber func(ctx context.Context, modelID string) ModelProbeResult

// probeCache remembers verdicts for the life of the process.
//
// ONCE PER MODEL, NOT ONCE PER PLAN. The whole point is that this costs a
// trivial request; paying it again on every plan in a session would turn a
// cheap guarantee into a per-plan tax. Verdicts do not change within a run —
// a provider that refuses a model at 10:00 refuses it at 10:05.
//
// ProbeUnknown is deliberately NOT cached. It records a failure to ask, not an
// answer, and caching it would let one flaky moment exclude a working model for
// the rest of the session.
type probeCache struct {
	mu      sync.Mutex
	results map[probeCacheKey]ModelProbeResult
}

type probeCacheKey struct {
	provider string
	model    string
}

func (cache *probeCache) get(provider, id string) (ModelProbeResult, bool) {
	if cache == nil {
		return ModelProbeResult{}, false
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	result, ok := cache.results[probeCacheKey{provider: provider, model: id}]
	return result, ok
}

func (cache *probeCache) put(provider, id string, result ModelProbeResult) {
	if cache == nil || result.Verdict == ProbeUnknown {
		return
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.results == nil {
		cache.results = map[probeCacheKey]ModelProbeResult{}
	}
	cache.results[probeCacheKey{provider: provider, model: id}] = result
}

// ClassifyProbeError turns a provider's error into a verdict.
//
// THE ONLY PLACE MESSAGE TEXT IS READ, and it is confined here on purpose. Every
// other decision in this package is structural — Stalled is a flag, ModelRejected
// is a flag — because matching prose to decide whether to spend a child is the
// bug class this codebase keeps re-learning. Here there is no structure to use:
// providers return "the model does not exist", "your team does not have access",
// "not allowed on chat completions" as ordinary errors on the same code path as
// a timeout, and the difference genuinely only exists in the words.
//
// So it is written to FAIL TOWARD KEEPING THE MODEL. Anything not recognised is
// ProbeUnknown, which changes nothing. Being wrong in that direction costs one
// failed task; being wrong the other way silently removes a model the user paid
// for.
func ClassifyProbeError(err error) ModelProbeResult {
	if err == nil {
		return ModelProbeResult{Verdict: ProbeServes}
	}
	message := err.Error()
	lower := strings.ToLower(message)
	for _, marker := range []string{
		"does not exist",
		"do not have access",
		"does not have access",
		"not allowed on",
		"model_not_found",
		"unknown model",
		"no such model",
		"unsupported model",
		"not supported for this",
		// BILLED SEPARATELY AND UNAFFORDABLE IS ALSO A REFUSAL. Ollama lists
		// models that its plan does not cover and answers a request for one with
		// "this model uses extra usage only (not included plan usage) and your
		// extra usage balance is empty". The model exists, discovery reports it,
		// and it ranks HIGHEST because ranking is by price — so auto-assign picks
		// it and every task assigned to it dies in seconds having done nothing.
		//
		// Measured on one account: kimi-k3 was picked on four separate days
		// across eight plan tasks, each dying in ~7 seconds with 0 tokens and 0
		// tool calls, and each costing the plan a dispatch and a retry cycle.
		//
		// "EXTRA USAGE ONLY", not bare "extra usage": the same words appear in
		// "add extra usage" inside this very message and would appear in any
		// balance notice that is not a refusal — and this function's rule is that
		// being wrong here silently removes a model the user paid for.
		//
		// SELF-CORRECTING. This is "you cannot afford it right now", not "it does
		// not exist", and the probe cache lives only for the process — so adding
		// balance makes the next session probe again and keep the model.
		"extra usage only",
		"not included plan usage",
		"usage balance is empty",
	} {
		if strings.Contains(lower, marker) {
			return ModelProbeResult{Verdict: ProbeRefuses, Reason: strings.TrimSpace(message)}
		}
	}
	return ModelProbeResult{Verdict: ProbeUnknown, Reason: strings.TrimSpace(message)}
}

// proveModels removes candidates the provider will not actually run.
//
// CONCURRENT AND BOUNDED, in width as well as in time. The point is to pay one
// round trip instead of nineteen, not to open one connection per model: a
// provider listing hundreds of models answered a burst of hundreds of
// simultaneous requests on the session's own key with 429s, which classify as
// ProbeUnknown — so the probe paid for the burst, learned nothing, and left the
// plan's real tasks throttled behind it. maxProbesInFlight is the cap; the
// caller's context still bounds the time, and a probe that outlives its plan's
// patience simply returns ProbeUnknown and the model keeps its place.
//
// Returns the survivors and a note per model removed. The note matters as much
// as the removal: a model vanishing from routing with no explanation is
// indistinguishable from a bug, and the user has spent this week hand-adding
// exactly these ids to an exclude list.
func proveModels(ctx context.Context, models []DiscoveredModel, probe ModelProber, cache *probeCache, providerIdentity ...string) ([]DiscoveredModel, []string) {
	if probe == nil || len(models) == 0 {
		return models, nil
	}

	verdicts := make([]ModelProbeResult, len(models))
	provider := ""
	if len(providerIdentity) > 0 {
		provider = strings.TrimSpace(providerIdentity[0])
	}
	var wait sync.WaitGroup
	// A counting semaphore, not a worker pool: the number of models is usually
	// under the cap, and this way that common case still starts every probe at
	// once with no queue to hand work through.
	inFlight := make(chan struct{}, maxProbesInFlight)
	for index, model := range models {
		if cached, ok := cache.get(provider, model.ID); ok {
			verdicts[index] = cached
			continue
		}
		wait.Add(1)
		go func(index int, id string) {
			defer wait.Done()
			// A panicking prober must not take the plan with it: the worst it can
			// do is leave this model unproven, which keeps it.
			defer func() { _ = recover() }()
			select {
			case inFlight <- struct{}{}:
				defer func() { <-inFlight }()
			case <-ctx.Done():
				// Queued behind the cap when the caller gave up. Unknown keeps
				// the model, which is what every other giving-up path does.
				verdicts[index] = ModelProbeResult{Verdict: ProbeUnknown, Reason: ctx.Err().Error()}
				return
			}
			verdicts[index] = probe(ctx, id)
		}(index, model.ID)
	}
	wait.Wait()

	kept := make([]DiscoveredModel, 0, len(models))
	var notes []string
	for index, model := range models {
		cache.put(provider, model.ID, verdicts[index])
		if verdicts[index].Verdict != ProbeRefuses {
			kept = append(kept, model)
			continue
		}
		note := model.ID + ": this provider will not run it"
		if reason := strings.TrimSpace(verdicts[index].Reason); reason != "" {
			note += " (" + firstLine(reason) + ")"
		}
		notes = append(notes, note)
	}
	// EVERY CANDIDATE REFUSED IS NOT A REASON TO ASSIGN NOTHING — it is a reason
	// to distrust the probe. A provider rejecting its own entire model list is far
	// more likely to be a credential or endpoint problem than nineteen genuinely
	// dead models, and dropping them all would silently disable routing at exactly
	// the moment the user most needs to be told something is wrong.
	if len(kept) == 0 {
		return models, []string{"every discovered model failed its probe, which points at this provider rather than at the models — routing left them in place"}
	}
	return kept, notes
}

// firstLine keeps a provider's message to one line so a note stays readable;
// these arrive as multi-line JSON blobs often enough to matter.
func firstLine(s string) string {
	if index := strings.IndexAny(s, "\r\n"); index >= 0 {
		s = s[:index]
	}
	const limit = 160
	if len(s) > limit {
		s = s[:limit] + "…"
	}
	return strings.TrimSpace(s)
}
