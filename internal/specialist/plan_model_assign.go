package specialist

import (
	"context"
	"strings"
)

// DiscoveredModel is the minimum a plan needs in order to choose between models.
//
// A LOCAL TYPE, not providermodeldiscovery.Model, because this package must not
// import the provider stack: discovery drags in config and providercatalog, and
// specialist is on the child-execution path where those have no business. The
// caller — which already holds a provider profile — adapts.
type DiscoveredModel struct {
	ID          string
	Description string
	ToolCall    bool
	Reasoning   bool
	InputCost   float64
	OutputCost  float64
	// OutputModalities is what the model emits. A plan task needs text; a real
	// run assigned grok-imagine-video-1.5 to the verify stage purely because it
	// was the most expensive thing on the account.
	OutputModalities []string
}

// ModelDiscoverer reports the models the ACTIVE provider can serve. Supplied by
// the surface that owns the provider profile; nil means auto-assignment is
// simply unavailable, which is the honest default for a headless run.
type ModelDiscoverer func(ctx context.Context) ([]DiscoveredModel, error)

// ModelPreferences is what the user has said about which models plans may use.
// A LOCAL type again: specialist must not import config, and the four strings it
// needs do not justify the dependency.
type ModelPreferences struct {
	Scan      string
	Implement string
	Verify    string
	Exclude   []string
	// AutoAssign makes per-task model selection the default for every plan. The
	// tool argument still overrides it in both directions.
	AutoAssign bool
	// Router names the model that DECIDES the per-task assignment by reading the
	// tasks, instead of the keyword classifier guessing from verbs. Empty falls
	// back to the strongest model discovery found; routing is skipped entirely
	// when neither is available.
	Router string
	// RouterGuidance is the operator's own advice to the router, added to the
	// built-in guidance. Empty changes nothing.
	RouterGuidance string
	// MinSize is the smallest model, in billions of parameters, worth routing a
	// task to. A model KNOWN to be smaller (its id names a size below this) is
	// dropped from every tier, so a task lands on a decent model rather than a
	// toy. A model of unknown size is kept; 0 is no floor. Named to match the
	// config field it carries from (see the prefs-carry guard test).
	MinSize float64
	// TopModels is how many of the MOST CAPABLE models a plan may route to. A
	// provider can list far more than a plan can sensibly use, and ranking the
	// whole field makes the cheap tier the smallest thing it happens to serve.
	// 0 means the default (defaultTopRankedModels); a provider offering fewer
	// keeps all of them. Pins are checked against the served set, not this list,
	// so naming a model outside the top N still routes to it. Named to match the
	// config field it carries from (see the prefs-carry guard test).
	TopModels int
}

func (prefs ModelPreferences) excluded(id string) bool {
	for _, name := range prefs.Exclude {
		if strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(id)) {
			return true
		}
	}
	return false
}

// pinned returns the model the user fixed for a role, if any.
func (prefs ModelPreferences) pinned(role TaskRole) string {
	switch role {
	case TaskRoleScan:
		return strings.TrimSpace(prefs.Scan)
	case TaskRoleImplement:
		return strings.TrimSpace(prefs.Implement)
	case TaskRoleVerify:
		return strings.TrimSpace(prefs.Verify)
	default:
		return ""
	}
}

// modelTiers is the three-way split a plan assigns from. Any of them may be
// empty, and an empty tier means "inherit the parent's model" rather than a
// guess at a substitute.
type modelTiers struct {
	cheap    string
	balanced string
	strong   string
}

// buildModelTiers ranks the provider's tool-calling models by cost.
//
// COST, not curated adjectives, as the primary signal. A description saying
// "fast" or "flagship" is marketing text that varies per provider and changes
// without notice; the price is a number the provider commits to, and it orders
// the same way capability does in practice. Descriptions are consulted only to
// break the degenerate cases below.
//
// Tool calling is a hard filter, not a preference: a plan task that cannot call
// a tool cannot do a plan task's job — it is precisely the "answer from memory"
// failure the task prompt exists to prevent.
// A discovered model is CANONICALISED where the registry knows it and taken as
// given where it does not. Discovery is the authority here: it asked this
// provider what it serves. Requiring curation as well is what made auto-assign
// do nothing on an xAI account, whose Grok models the picker lists in full and
// the registry has never heard of.
// rankedEligibleModels is the candidate set, cheapest first: every model that
// can do a plan task's job, after exclusions.
//
// SHARED WITH THE ROUTER on purpose. Offering the router a wider set than the
// tiers were built from would let it pick something the tier logic had already
// judged unusable — a video model, an excluded id — and the served-set check
// would then drop it silently, leaving the task on the classifier's choice with
// no explanation.
func rankedEligibleModels(models []DiscoveredModel, prefs ModelPreferences) []DiscoveredModel {
	// TOOL CALLING IS A FILTER ONLY WHEN THE PROVIDER SAYS ANYTHING ABOUT IT.
	//
	// DiscoveredModel.ToolCall is a bool, so "cannot call tools" and "this
	// provider's /models says nothing about capabilities" arrive identically —
	// and plenty of providers publish a bare id list. Requiring it outright made
	// auto_assign silently assign nothing there, which is what a real run showed:
	// three models discovered, none marked, every task left on the parent's.
	//
	// So the flag NARROWS the field when some model claims it, and is ignored
	// when none does. The parent is itself calling tools on this provider, which
	// is better evidence than an absent field.
	anyToolCaller := false
	for _, model := range models {
		if model.ToolCall {
			anyToolCaller = true
			break
		}
	}
	eligible := make([]DiscoveredModel, 0, len(models))
	for _, model := range models {
		if strings.TrimSpace(model.ID) == "" {
			continue
		}
		if !emitsText(model) || prefs.excluded(model.ID) {
			continue
		}
		if anyToolCaller && !model.ToolCall {
			continue
		}
		canonical, err := resolveTaskModel(model.ID)
		if err != nil || canonical == "" {
			continue
		}
		model.ID = canonical
		eligible = append(eligible, model)
	}
	// Drop models KNOWN to be below the decency floor, so a task lands on a decent
	// model rather than a toy. Fail-open: never leaves a plan with no models.
	eligible = applyMinSizeFloor(eligible, prefs.MinSize)

	// Least-to-most capable, so buildModelTiers reads cheap/balanced/strong off
	// the ends. Priced providers rank by cost exactly as before; a free provider
	// ranks by the size in the id instead of falling through to alphabetical.
	sortModelsByCapability(eligible)

	// THEN THE SHORTLIST. Ranking the whole field is what makes the cheap tier
	// the smallest thing the provider happens to serve — on a provider listing
	// twenty models, that is a toy doing a scan while nineteen better ones sit
	// unused, and the router is handed all twenty to choose between. Keeping the
	// most capable N narrows both, so the tiers span the good models rather than
	// everything. A PIN IS UNAFFECTED: pins are validated against the provider's
	// served set, never against this list, so naming a model outside the top N
	// still routes to it — the user's explicit instruction outranks a heuristic.
	return applyTopRank(eligible, prefs.TopModels)
}

func buildModelTiers(models []DiscoveredModel, prefs ModelPreferences) modelTiers {
	eligible := rankedEligibleModels(models, prefs)
	if len(eligible) == 0 {
		return modelTiers{}
	}

	tiers := modelTiers{
		cheap:    eligible[0].ID,
		balanced: eligible[len(eligible)/2].ID,
		strong:   eligible[len(eligible)-1].ID,
	}
	// A provider offering one model gives every task the same one, which is
	// exactly today's behaviour and is the right answer rather than a failure.
	if len(eligible) == 1 {
		return tiers
	}
	// Prefer a REASONING model for the strong tier when one exists and the most
	// expensive model is not already one — the verify stage is what the tier is
	// for, and price alone can put a large non-reasoning model on top.
	if !mostExpensiveReasons(eligible) {
		for i := len(eligible) - 1; i >= 0; i-- {
			if eligible[i].Reasoning {
				tiers.strong = eligible[i].ID
				break
			}
		}
	}
	return tiers
}

func mostExpensiveReasons(sorted []DiscoveredModel) bool {
	return sorted[len(sorted)-1].Reasoning
}

// modelForRole picks the tier a role should run on. An empty result means
// "inherit", which is what every unassignable case degrades to.
//
// A PIN WINS OUTRIGHT, and is not filtered by anything above: it is a statement
// about a model the user has, not a candidate to be ranked. That is what makes
// it work on a provider reporting no prices and no capabilities, where every
// automatic signal is absent — which is most of them.
func (tiers modelTiers) modelForRoleWith(role TaskRole, prefs ModelPreferences, served map[string]bool) string {
	// A PIN THE ACTIVE PROVIDER DOES NOT SERVE IS NOT A PIN, it is a leftover.
	//
	// Pins name models, and models belong to providers. Switching provider —
	// which a user does when an account runs out of quota, mid-session — leaves
	// every pin naming something the new provider has never heard of. Applied
	// anyway, that turned a provider switch into "every plan fails": four tasks
	// dead with `model "grok-4.3" not found` before one had run.
	//
	// Checked only when discovery actually answered. A provider that lists
	// nothing is not evidence that a pin is wrong, and pins working WITHOUT
	// discovery is the whole reason they exist.
	if pin := prefs.pinned(role); pin != "" {
		if len(served) == 0 || servedContains(served, pin) {
			return pin
		}
	}
	return tiers.modelForRole(role)
}

// servedModels indexes what discovery reported, for checking pins against it.
//
// EVERY FORM OF EACH ID IS INDEXED, not just the one discovery happened to print,
// because the things compared against this map are written by people and by other
// layers that spell the same model differently.
//
// The map held raw discovery ids alone while three callers probed it with other
// forms: the provider-mismatch guard with the session's model, pin validation and
// the router pin with whatever the user typed in config. A session on "sonnet 4.5"
// against a provider listing "claude-sonnet-4.5", or "glm-5.2" against
// "glm-5.2:latest", produced a MISS — and the mismatch guard reads a miss as "this
// list belongs to a different provider", so auto-assignment silently switched
// itself off, or refused the plan outright when it had been asked for explicitly.
// The user saw single-model routing and no reason for it.
//
// Indexing every form can only ADD matches, never remove one, so a setup that
// works today cannot start failing because of this.
func servedModels(models []DiscoveredModel) map[string]bool {
	if len(models) == 0 {
		return nil
	}
	served := make(map[string]bool, len(models)*2)
	for _, model := range models {
		for _, form := range modelIDForms(model.ID) {
			served[form] = true
		}
	}
	return served
}

// servedContains asks whether a provider serves a model, comparing every spelling
// of the probe against every spelling of what was discovered.
//
// EMPTY MEANS UNKNOWN, NOT ABSENT — a provider that lists nothing is not evidence
// that a pin is wrong, and pins working without discovery is the whole reason they
// exist. Callers that must distinguish "not served" from "nothing discovered"
// check len(served) themselves; this answers only the membership question.
func servedContains(served map[string]bool, id string) bool {
	for _, form := range modelIDForms(id) {
		if served[form] {
			return true
		}
	}
	return false
}

// modelIDForms enumerates the spellings one model id can legitimately take:
// the string itself, its registry-canonical form, and the same without an
// Ollama-style ":latest" tag. Duplicates are fine — callers only test membership.
func modelIDForms(id string) []string {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return nil
	}
	forms := []string{trimmed}
	if canonical, err := resolveTaskModel(trimmed); err == nil && canonical != "" && canonical != trimmed {
		forms = append(forms, canonical)
	}
	// ":latest" is a TAG, not part of the model's identity — Ollama prints it,
	// people do not type it, and the two name the same weights.
	for _, form := range append([]string(nil), forms...) {
		if base, tagged := strings.CutSuffix(form, ":latest"); tagged && base != "" {
			forms = append(forms, base)
		}
	}
	return forms
}

func (tiers modelTiers) modelForRole(role TaskRole) string {
	switch role {
	case TaskRoleScan:
		return tiers.cheap
	case TaskRoleImplement:
		return tiers.balanced
	case TaskRoleVerify:
		return tiers.strong
	default:
		return ""
	}
}

// assignModelsToTaskArgs fills in a model for tasks that did not name one, and
// returns the args to parse plus a human-readable note per assignment.
//
// IT WORKS ON THE ARGS, BEFORE ParsePlan, and that is the whole design. Every
// downstream property then falls out for free: the assigned model is validated
// by the same constructor as a hand-written one, it round-trips through
// Plan.Args() into a saved plan, and resume re-admits exactly what ran. Applying
// it to a parsed Plan instead would have meant a second write path into the one
// object ParsePlan exists to be the sole author of.
//
// A task that NAMED a model is never touched — an explicit choice outranks a
// guess, always.
// assignModelsToTaskArgs fills in a model for tasks that named none.
//
// routed, when non-empty, is a model per task id decided by the router; it wins
// over the classifier for the tasks it covers and leaves the rest to it. Pins
// still outrank both — a pin is the user's own instruction, and neither a
// keyword nor another model overrules that.
func assignModelsToTaskArgs(tasks []any, tiers modelTiers, prefs ModelPreferences, served map[string]bool, routed map[string]string) ([]any, []string) {
	// Pins alone are enough: with every role pinned, a provider that reports
	// nothing usable still assigns.
	if tiers == (modelTiers{}) && prefs.pinned(TaskRoleScan) == "" &&
		prefs.pinned(TaskRoleImplement) == "" && prefs.pinned(TaskRoleVerify) == "" {
		return tasks, nil
	}
	var notes []string
	out := make([]any, 0, len(tasks))
	for _, raw := range tasks {
		fields, ok := raw.(map[string]any)
		if !ok {
			// Not an object: leave it exactly as it came so ParsePlan reports
			// the real shape error rather than one this function invented.
			out = append(out, raw)
			continue
		}
		if strings.TrimSpace(planString(fields, "model")) != "" {
			out = append(out, raw)
			continue
		}
		task := Task{
			ID:     planString(fields, "id"),
			Prompt: planString(fields, "prompt"),
			Tools:  planStrings(fields, "tools"),
		}
		role := classifyTaskRole(task)
		model := tiers.modelForRoleWith(role, prefs, served)
		byRouter := false
		if pin := prefs.pinned(role); pin == "" || (len(served) > 0 && !servedContains(served, pin)) {
			if chosen := routed[task.ID]; chosen != "" {
				model, byRouter = chosen, true
			}
		}
		if model == "" {
			out = append(out, raw)
			continue
		}
		// COPIED, not mutated. These maps come from the tool call's decoded
		// arguments and, on the saved-plan path, from a stored plan the caller
		// may still hold; writing through would edit someone else's data.
		clone := make(map[string]any, len(fields)+1)
		for key, value := range fields {
			clone[key] = value
		}
		clone["model"] = model
		out = append(out, clone)

		// ONE APPEND, then only the wording differs. This branch once carried its
		// own clone-and-append, so a routed task was emitted TWICE and ParsePlan
		// rejected the plan with "task id appears more than once" — every plan of
		// three or more tasks, because routing does not run below that. The test
		// missed it by collecting results into a map keyed by task id, where a
		// duplicate silently overwrites its twin.
		if byRouter {
			notes = append(notes, task.ID+": routed → "+model)
			continue
		}
		note := task.ID + ": " + string(role) + " → " + model
		switch pin := prefs.pinned(role); pin {
		case "":
		case model:
			note += " (pinned)"
		default:
			// Say WHY the pin was passed over, or a user who configured one and
			// sees a different model has no way to find out.
			note += " (pin " + pin + " not served by this provider)"
		}
		notes = append(notes, note)
	}
	return out, notes
}

// emitsText reports whether a model can produce what a plan task must produce.
//
// Ranking by price assumes every candidate is doing the same job. An account
// with image or video models breaks that assumption at the top end, where the
// verify tier looks: a real run picked grok-imagine-video-1.5 as the strongest
// model on the account, and every task depending on it was doomed before it
// started.
//
// The declared modality decides it when the provider reports one. When it does
// not — and many do not — the name is the only signal left. That is a heuristic
// and is written as one: it can only ever EXCLUDE a candidate, so the cost of
// being wrong is a model not chosen, never a plan wired to a model that cannot
// answer.
func emitsText(model DiscoveredModel) bool {
	for _, modality := range model.OutputModalities {
		if strings.EqualFold(strings.TrimSpace(modality), "text") {
			return true
		}
	}
	if len(model.OutputModalities) > 0 {
		return false
	}
	name := strings.ToLower(model.ID + " " + model.Description)
	for _, marker := range []string{"image", "video", "audio", "speech", "tts", "whisper", "embed", "rerank", "moderation", "imagine", "diffusion"} {
		if strings.Contains(name, marker) {
			return false
		}
	}
	return true
}
