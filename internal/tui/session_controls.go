package tui

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/execprofile"
	"github.com/Twigpine/zero/internal/modelregistry"
	"github.com/Twigpine/zero/internal/providercatalog"
	"github.com/Twigpine/zero/internal/providermodeldiscovery"
	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/usage"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

var responseStyles = []string{"balanced", "concise", "explanatory", "review"}

const tuiCompactionPreserveLast = 8
const compactStatusRowID = "compact/status"

var compactFrames = []string{"⠂", "⠒", "⠲", "⠴"}

type SessionCompactor interface {
	CompactSession(context.Context, CompactRequest) (CompactResult, error)
}

type CompactRequest struct {
	SessionID             string
	ModelName             string
	ContextWindow         int
	SessionEventCount     int
	EstimatedTokens       int
	VisibleTranscriptRows int
	CompactRequests       int
}

type CompactResult struct {
	Compacted    bool
	BeforeTokens int
	AfterTokens  int
	Summary      string
}

type compactResultMsg struct {
	result             CompactResult
	err                error
	activeSession      sessions.Metadata
	sessionEvents      []sessions.Event
	transcript         []transcriptRow
	hasSessionSnapshot bool
}

func (m model) handleEffortCommand(args string) (model, string) {
	args = strings.TrimSpace(strings.ToLower(args))
	if args == "" || args == "list" {
		return m, m.effortText()
	}
	// The zeromaxing posture is selected through the effort namespace as well as
	// /profile, so it is handled HERE — beside "auto", with an early return,
	// BEFORE the ReasoningEffort conversion below. It is a posture name, not a
	// provider effort level: it must never become a ReasoningEffort value and
	// must never reach a provider request.
	//
	// This delegates to handleProfileCommand rather than re-applying the knobs,
	// so "/effort zeromaxing and /profile zeromaxing resolve identically" is
	// true by construction instead of by two implementations a test hopes agree.
	if args == execprofile.Name {
		// Same idle-session rule /profile enforces, and for the same reason: this
		// delegates to handleProfileCommand, which mutates the turn budget, the
		// self-correct setting and the shared orchestrate gate. The budget
		// propagates to sub-agents spawned later in the same run, so changing it
		// mid-run leaves one turn running under two different budgets. Reaching
		// the same mutation through the effort namespace must not skip the guard
		// the profile namespace applies.
		if m.pending {
			return m, `Effort
Finish or stop the current run before switching to the zeromaxing posture.`
		}
		return m.handleProfileCommand(execprofile.Name)
	}
	if args == "auto" {
		// "auto" is the effort namespace's off switch, so under zeromaxing it
		// also leaves the posture (scheduling the one-shot exit notice and
		// restoring the displaced knobs). Scoped to zeromaxing on purpose: under
		// fast/thorough, /effort auto keeps its existing meaning of "clear the
		// effort", and reverting there would wrongly drop the whole profile.
		if m.execProfileName == execprofile.Name {
			// The SAME idle-session rule entering the posture enforces, because
			// this is the same mutation in reverse: revertExecProfile moves the
			// turn budget, the self-correct setting and the shared orchestrate
			// gate, and doing that mid-run leaves one turn running under two
			// different budgets. Entering through /effort zeromaxing is guarded;
			// leaving through /effort auto must not be the unguarded door.
			if m.pending {
				return m, `Effort
Finish or stop the current run before leaving the zeromaxing posture.`
			}
			m = m.revertExecProfile()
			return m, m.profileText()
		}
		m.reasoningEffort = ""
		m = m.markProfileEffortTouched()
		return m, m.effortStatusCard("auto", "Reasoning effort selection will follow the active model/provider defaults.")
	}

	// NOTE: "max" is deliberately NOT handled here. ValidReasoningEffort already
	// accepts ReasoningEffortMax (models.go), so /effort max continues to parse
	// as a raw provider level and to fail reasoningEffortAllowed on every
	// current model — exactly as before this posture existed. That spelling is
	// RESERVED for a real provider rung once one ships; claiming it for a Zero
	// posture would burn the name. TestEffortMaxReservationUnchanged pins it.
	requested := modelregistry.ReasoningEffort(args)
	if !modelregistry.ValidReasoningEffort(requested) {
		return m, m.effortStatusCard(args, "Unknown reasoning effort: "+args)
	}
	efforts, known := m.availableReasoningEffortsKnown()
	// A model the catalog VOUCHES for is authoritative: an empty ring there
	// genuinely means no reasoning controls, and a level outside the ring is
	// genuinely unsupported. A model with no catalog entry is a different case
	// — a custom endpoint the catalog cannot vouch for either way — and
	// refusing there is a false negative. The headless path already forwards in
	// exactly that case ("no support claim can be made for it"), and since the
	// bug-1 fix the profile fill does too; this is the third consumer of the
	// same question, and it used to answer it differently from the other two.
	switch {
	case known && len(efforts) == 0:
		return m, m.effortStatusCard("", "Active model does not expose reasoning effort controls.")
	case known && !reasoningEffortAllowed(efforts, requested):
		return m, m.effortStatusCard(string(requested),
			fmt.Sprintf("Reasoning effort %q is not supported by %s.", requested, displayValue(m.modelName, "the active model")))
	}

	m.reasoningEffort = requested
	m = m.markProfileEffortTouched()
	if !known {
		// Deliberately says MAY reject, not "is ignored". The claim that an
		// unsupported value is silently ignored could not be verified against a
		// real provider, and this repo contradicts itself about it:
		// modelregistry/catalog.go says unknown fields are ignored, while
		// providers/factory.go, providers/openai/provider.go and
		// providers/openai/types.go all record strict openai-compatible
		// gateways rejecting them with a 400 — DisablePromptCacheKey exists
		// precisely because one did. Promising "ignored" would be an unverified
		// reassurance on the path where it fails.
		return m, m.effortStatusCard(string(requested),
			fmt.Sprintf("Stored for this session and forwarded to %s. This model is not in Zero's catalog, so Zero cannot confirm it accepts that level — a provider that validates the parameter may reject the request.",
				displayValue(m.modelName, "the active model")))
	}
	return m, m.effortStatusCard(string(requested), "Reasoning effort preference is stored for this TUI session.")
}

func (m model) handleFastCommand(args string) (model, string) {
	if strings.TrimSpace(args) != "" {
		return m, m.fastStatusText("Use /fast to toggle fast mode for supported ChatGPT subscription models.")
	}
	if m.pending {
		return m, m.fastStatusText("Finish or stop the current run before changing fast mode.")
	}
	if !m.fastModeSupported() {
		m.serviceTier = ""
		return m, m.fastStatusText("Fast mode is available only for ChatGPT subscription models that advertise it.")
	}
	if m.activeServiceTier() == "priority" {
		m.serviceTier = ""
	} else {
		m.serviceTier = "priority"
	}
	return m, m.fastStatusText("Fast mode applies to the next request for this ChatGPT subscription model.")
}

func (m model) fastStatusText(detail string) string {
	state := "off"
	if m.activeServiceTier() == "priority" {
		state = "on"
	}
	fields := []commandField{
		{Key: "fast mode", Value: state},
		{Key: "model", Value: displayValue(m.modelName, "none")},
	}
	if detail == "" {
		detail = "Use /fast to toggle fast mode for supported ChatGPT subscription models."
	}
	return renderCommandCardTranscript(commandCard{
		Title:   "Fast",
		Summary: []string{"fast mode: " + state},
		Sections: []commandCardSection{{
			Title:  "State",
			Fields: fields,
			Lines:  []string{detail},
		}},
	})
}

func (m model) activeServiceTier() string {
	if m.serviceTier == "priority" && m.fastModeSupported() {
		return "priority"
	}
	return ""
}

func (m model) fastModeSupported() bool {
	// Priority is a ChatGPT subscription capability. Never forward it for
	// metered provider profiles, even if a third-party catalog advertises it.
	if providercatalog.NormalizeID(m.providerProfile.CatalogID) != "chatgpt" {
		return false
	}
	for _, model := range m.discoveredModelsForActiveProvider() {
		if !strings.EqualFold(strings.TrimSpace(model.ID), strings.TrimSpace(m.modelName)) {
			continue
		}
		for _, tier := range model.ServiceTiers {
			if normalizeTUIServiceTier(tier) == "priority" {
				return true
			}
		}
		return false
	}
	return false
}

func (m model) discoveredModelsForActiveProvider() []providermodeldiscovery.Model {
	for _, key := range []string{m.providerProfile.CatalogID, m.providerProfile.Name, m.providerName} {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if models := m.modelPickerLiveByProvider[key]; len(models) > 0 {
			return models
		}
	}
	return nil
}

func normalizeTUIServiceTier(tier string) string {
	tier = strings.ToLower(strings.TrimSpace(tier))
	if tier == "fast" {
		return "priority"
	}
	return tier
}

// effortStatusCard renders the small inline confirmation card shown after a
// /effort <value> mutation (set, auto, unknown, unsupported). The body is a
// lime-bordered card so the transition from "picker open" -> "card collapsed"
// reads as the same surface the picker came from, instead of a separate grey
// status block.
func (m model) effortStatusCard(value string, detail string) string {
	active := strings.TrimSpace(value)
	if active == "" {
		active = "auto"
	}
	fields := []commandField{
		{Key: "active effort", Value: active},
		{Key: "model", Value: displayValue(m.modelName, "none")},
	}
	return renderCommandCardTranscript(commandCard{
		Title:   "Effort",
		Summary: []string{"active effort: " + active},
		Sections: []commandCardSection{{
			Title:  "State",
			Fields: fields,
			Lines:  []string{detail},
		}},
	})
}

func (m model) effortText() string {
	efforts := m.availableReasoningEfforts()
	fields := []commandField{
		{Key: "active effort", Value: m.effortDisplay()},
		{Key: "model", Value: displayValue(m.modelName, "none")},
	}
	_, ringKnown := m.availableReasoningEffortsKnown()
	actions := []string{"use /effort <value> to switch", "/effort auto to clear"}
	if !m.zeromaxingDisabled {
		// Not offered when the workspace disabled it: an action line that
		// suggests a command the run will refuse is worse than no line.
		actions = append(actions, "/effort "+execprofile.Name+" for the maximal posture")
	}
	// The SAME resolved-state line /profile status renders, so the two surfaces
	// cannot disagree about what actually reaches the provider.
	stateLines := append([]string{m.resolvedPostureLine()}, m.zeromaxingNotes()...)
	if len(efforts) == 0 {
		// "none" and "unknown" are different answers and used to render the
		// same. A user on a custom endpoint was told the model has no reasoning
		// controls, when the truth is that Zero has no entry for it — and levels
		// set there ARE forwarded.
		available, summary := "none for active model", "no reasoning controls on this model"
		if !ringKnown {
			available = "not listed — model is not in Zero's catalog"
			summary = "levels are not listed for this model; they can still be set and are forwarded, but the provider may reject them"
		}
		fields = append(fields, commandField{Key: "available", Value: available})
		if settable := m.settableEfforts(); len(settable) > 0 {
			fields = append(fields, commandField{Key: "you can set", Value: strings.Join(settable, ", ")})
		}
		return renderCommandCardTranscript(commandCard{
			Title:    "Effort",
			Summary:  []string{"active effort: " + m.effortDisplay(), summary},
			Sections: []commandCardSection{{Title: "State", Fields: fields, Lines: stateLines}},
			Actions:  actions,
		})
	}
	fields = append(fields, commandField{Key: "available", Value: joinReasoningEfforts(efforts)})
	if settable := m.settableEfforts(); len(settable) > 0 {
		fields = append(fields, commandField{Key: "you can set", Value: strings.Join(settable, ", ")})
	}
	return renderCommandCardTranscript(commandCard{
		Title:    "Effort",
		Summary:  []string{"active effort: " + m.effortDisplay(), fmt.Sprintf("%d supported level(s)", len(efforts))},
		Sections: []commandCardSection{{Title: "State", Fields: fields, Lines: stateLines}},
		Actions:  actions,
	})
}

// markProfileEffortTouched records an explicit effort choice made while a
// profile is active: the touched bit protects the choice from a later profile
// revert, and disarming the policy's RestoreDefaultEffort stops a mid-run
// escalation from clearing an effort the user pinned by hand.
func (m model) markProfileEffortTouched() model {
	if m.execProfileName == "" {
		return m
	}
	m.execProfileEffortTouched = true
	return m.setProfileEffortRestore(false)
}

// setProfileEffortRestore rewrites the armed policy's RestoreDefaultEffort on
// a clone (an in-flight run may hold the current pointer), like the /turns
// turn-target disarm. No-op when no escalation policy is armed or the flag
// already matches.
func (m model) setProfileEffortRestore(restore bool) model {
	if m.agentOptions.Profile == nil || m.agentOptions.Profile.Escalate == nil ||
		m.agentOptions.Profile.Escalate.RestoreDefaultEffort == restore {
		return m
	}
	policy := *m.agentOptions.Profile
	escalate := *policy.Escalate
	escalate.RestoreDefaultEffort = restore
	policy.Escalate = &escalate
	m.agentOptions.Profile = &policy
	return m
}

// reconcileEffortForModelSwitch applies the effort rules for a model switch.
// efforts is the destination's supported ring and ringKnown whether that ring
// is authoritative (a catalog entry, whose ring may be legitimately empty) or
// missing knowledge (a live-discovered/custom model the catalog cannot vouch
// for either way).
//
// First the pre-existing generic rule: a KNOWN-unsupported preference is
// dropped — the TUI run path forwards the effort to the provider unfiltered,
// so it must not survive the switch, for a profile-touched effort exactly as
// for a plain one. An UNKNOWN ring never drops an explicit preference: the
// model may well support it, matching the cross-provider picker's behavior
// for custom models. When a drop voids a choice living under an active
// profile (the profile's own fill or a user override of it), the profile's
// effort bookkeeping resets so a choice that no longer exists stops binding
// and no stale applied/restore state remains.
//
// Then the profile rule: an active, untouched profile re-derives its fill for
// the destination (conservative in both directions — the fill only ever
// applies where support is known). The second return reports whether an
// unsupported preference was dropped and nothing refilled (the caller's
// "(unsupported preference reset)" display case).
func (m model) reconcileEffortForModelSwitch(efforts []modelregistry.ReasoningEffort, ringKnown bool) (model, bool) {
	dropped := false
	if ringKnown && m.reasoningEffort != "" && !reasoningEffortAllowed(efforts, m.reasoningEffort) {
		m.reasoningEffort = ""
		dropped = true
		if m.execProfileName != "" {
			m.execProfileEffortTouched = false
			m.execProfileAppliedEffort = ""
			m = m.setProfileEffortRestore(false)
		}
	}
	m = m.reconcileProfileAfterModelSwitch(efforts, ringKnown)
	return m, dropped && m.reasoningEffort == ""
}

// reconcileProfileAfterModelSwitch re-derives an active profile's effort fill
// for the model the session just switched to, whose supported ring is passed
// in. The fill is per-model, not a session preference: fast selected on a
// model without "low" fills nothing, and switching to a model that supports
// it should behave exactly like selecting the profile there (and the reverse
// switch must not keep sending a level the destination does not support). An
// explicitly touched effort is the user's choice and is never reconciled; the
// escalation's RestoreDefaultEffort tracks whether the profile currently
// governs the effort.
func (m model) reconcileProfileAfterModelSwitch(efforts []modelregistry.ReasoningEffort, ringKnown bool) model {
	if m.execProfileName == "" || m.execProfileEffortTouched {
		return m
	}
	profile, ok := execprofile.Lookup(m.execProfileName)
	if !ok || profile.ReasoningEffort == "" {
		return m
	}
	want := modelregistry.ReasoningEffort(profile.ReasoningEffort)
	// The SAME rule the selection door uses. Calling reasoningEffortAllowed
	// directly here treated an uncatalogued model's empty ring as a refusal, so
	// switching to a model that selection would have filled silently dropped
	// the posture's effort and reported it as unraised.
	supported := profileEffortAppliesOn(m.modelName, efforts, ringKnown, want)
	filled := m.execProfileAppliedEffort != ""
	switch {
	case supported && !filled && m.reasoningEffort == "":
		m.reasoningEffort = want
		m.execProfileAppliedEffort = want
		m.execProfileEffortUnraised = "" // the destination model can take it after all
		m = m.setProfileEffortRestore(true)
	case !supported && filled && m.reasoningEffort == m.execProfileAppliedEffort:
		m.reasoningEffort = ""
		m.execProfileAppliedEffort = ""
		// Record WHY, exactly as the initial fill does. This is the SIBLING call
		// path of the fill site in handleProfileCommand: a switch that drops the
		// profile's effort is the same honesty case, arriving through the other
		// door, and the two must agree or the status output claims a raise the
		// run is not making.
		m.execProfileEffortUnraised = want
		m = m.setProfileEffortRestore(false)
	case !supported && !filled:
		// Never filled and still unsupported: keep the reason fresh for the
		// destination model rather than leaving a stale one from the source.
		m.execProfileEffortUnraised = want
	}
	return m
}

func (m model) availableReasoningEfforts() []modelregistry.ReasoningEffort {
	if strings.TrimSpace(m.modelName) == "" {
		return nil
	}
	if efforts, found := m.discoveredReasoningEfforts(); found {
		return efforts
	}
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		return nil
	}
	return registry.ReasoningEfforts(m.modelName)
}

func (m model) discoveredReasoningEfforts() ([]modelregistry.ReasoningEffort, bool) {
	return reasoningEffortsFromModels(m.discoveredModelsForActiveProvider(), m.modelName)
}

func reasoningEffortsFromModels(models []providermodeldiscovery.Model, modelName string) ([]modelregistry.ReasoningEffort, bool) {
	for _, discovered := range models {
		if !strings.EqualFold(strings.TrimSpace(discovered.ID), strings.TrimSpace(modelName)) {
			continue
		}
		seen := map[modelregistry.ReasoningEffort]bool{}
		efforts := make([]modelregistry.ReasoningEffort, 0, len(discovered.ReasoningEfforts))
		for _, raw := range discovered.ReasoningEfforts {
			effort := modelregistry.ReasoningEffort(strings.ToLower(strings.TrimSpace(raw)))
			if effort == modelregistry.ReasoningEffortNone || !modelregistry.ValidReasoningEffort(effort) || seen[effort] {
				continue
			}
			seen[effort] = true
			efforts = append(efforts, effort)
		}
		return efforts, true
	}
	return nil, false
}

// availableReasoningEffortsKnown returns the active model's effort ring AND
// whether that ring is AUTHORITATIVE — i.e. the model has a catalog entry, so an
// empty ring genuinely means "no reasoning controls" rather than "we have never
// heard of this model".
//
// The distinction already existed at the model-SWITCH site (command_center.go
// passes `target.entry != nil` as ringKnown) but not at the profile FILL site,
// which used the plain allowed-check and so treated an unknown model as a model
// that had refused. The headless path makes the opposite call — an unknown model
// forwards the requested effort as-is, "since no support claim can be made for
// it" — so the two surfaces disagreed about the same model.
func (m model) availableReasoningEffortsKnown() ([]modelregistry.ReasoningEffort, bool) {
	name := strings.TrimSpace(m.modelName)
	if name == "" {
		return nil, false
	}
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		return nil, false
	}
	_, known := registry.Get(name)
	return registry.ReasoningEfforts(name), known
}

// profileEffortAppliesOn is THE rule for whether a profile may fill `want` on a
// model. It fills when the model is KNOWN to support the level, and also when
// the ring is not authoritative: an unknown endpoint may well support it, and
// declining would be a false negative that silently drops the posture's effort.
// Only a model the catalog vouches for as lacking the level blocks the fill.
//
// ONE function, called by BOTH doors — selecting a profile on a model, and
// switching to a model with a profile already active. They previously used
// different predicates and disagreed on exactly the uncatalogued case, so the
// same user on the same model got a different effort depending on which door
// they came through. Two copies of a rule drift (invariant 5);
// TestProfileEffortDoorsAgree pins that these cannot.
func profileEffortAppliesOn(modelName string, efforts []modelregistry.ReasoningEffort, ringKnown bool, want modelregistry.ReasoningEffort) bool {
	// No model selected: there is nothing to make a support claim about, so the
	// profile does not fill. Distinct from an UNKNOWN model, which is a real
	// endpoint that may well accept the level — conflating the two was the
	// over-reach the pre-existing fast-posture test caught.
	if strings.TrimSpace(modelName) == "" {
		return false
	}
	if reasoningEffortAllowed(efforts, want) {
		return true
	}
	// A catalog entry is authoritative: an empty ring there genuinely means the
	// model has no reasoning controls, so do not fill. Without an entry the
	// catalog cannot vouch either way, and declining would be a false negative
	// that silently drops the posture's effort — the headless path forwards it
	// in exactly this case ("no support claim can be made for it").
	return !ringKnown
}

// profileEffortApplies is the selection door: it resolves the active model's
// ring and applies the shared rule.
func (m model) profileEffortApplies(want modelregistry.ReasoningEffort) bool {
	efforts, known := m.availableReasoningEffortsKnown()
	return profileEffortAppliesOn(m.modelName, efforts, known, want)
}

func (m model) effortDisplay() string {
	if m.reasoningEffort == "" {
		return "auto"
	}
	return string(m.reasoningEffort)
}

// cycleReasoningEffort advances the reasoning-effort ring:
// auto ("") -> first supported -> ... -> last supported -> auto. No-op (model
// unchanged) when the active model exposes no effort controls, so Ctrl+T stays
// quiet on non-reasoning models. Called from the Ctrl+T key case — a rare
// keypress — so the DefaultRegistry() lookup inside availableReasoningEfforts
// is fine here, but MUST NOT be called from the render path (the registry is
// rebuilt on every call).
func (m model) cycleReasoningEffort() (model, tea.Cmd) {
	efforts := m.availableReasoningEfforts()
	if len(efforts) == 0 {
		return m, nil
	}
	// Every branch below changes the effort; a cycle while a profile is active
	// is an explicit user choice that must survive both a later profile revert
	// and any mid-run escalation.
	m = m.markProfileEffortTouched()
	if m.reasoningEffort == "" {
		m.reasoningEffort = efforts[0]
		return m, nil
	}
	idx := reasoningEffortIndex(efforts, m.reasoningEffort)
	if idx == -1 || idx == len(efforts)-1 {
		m.reasoningEffort = "" // wrap to auto
		return m, nil
	}
	m.reasoningEffort = efforts[idx+1]
	return m, nil
}

func reasoningEffortAllowed(efforts []modelregistry.ReasoningEffort, want modelregistry.ReasoningEffort) bool {
	for _, effort := range efforts {
		if effort == want {
			return true
		}
	}
	return false
}

// reasoningEffortIndex returns the position of want in efforts, or -1. Sibling
// to reasoningEffortAllowed; used by cycleReasoningEffort to find the current
// slot in the model's supported ring.
func reasoningEffortIndex(efforts []modelregistry.ReasoningEffort, want modelregistry.ReasoningEffort) int {
	for index, effort := range efforts {
		if effort == want {
			return index
		}
	}
	return -1
}

// settableEfforts lists what /effort will actually accept right now.
//
// DISTINCT from "available", which reports what the CATALOG vouches for. The
// two answer different questions and used to be conflated into one line, so a
// user on a model Zero has no entry for was told "not listed" and shown nothing
// they could type — even though low/medium/high are settable there and ARE
// forwarded, which is the whole point of the catalog-authority rule.
//
// zeromaxing belongs here because it is selected through this namespace, and it
// was missing from every model's list even though the actions line offered it.
// It is omitted when the workspace disabled the posture, so the card never
// advertises something that will be refused.
func (m model) settableEfforts() []string {
	efforts, known := m.availableReasoningEffortsKnown()
	var values []string
	switch {
	case known:
		// The catalog is AUTHORITATIVE, including when its ring is empty:
		// gpt-4o genuinely has no reasoning controls, and handleEffortCommand
		// refuses every level there. Offering low/medium/high would advertise
		// something the command rejects — the card and the command answering
		// the same question differently, which is the shape this codebase keeps
		// producing.
		values = append(values, splitReasoningEfforts(efforts)...)
	case strings.TrimSpace(m.modelName) != "":
		// No catalog entry: Zero cannot vouch either way, and the headless path
		// forwards these in exactly this case. Offering them is honest; the
		// summary line already says the provider may reject them.
		values = append(values, "low", "medium", "high")
	}
	if !m.zeromaxingDisabled {
		values = append(values, execprofile.Name)
	}
	return values
}

func splitReasoningEfforts(efforts []modelregistry.ReasoningEffort) []string {
	values := make([]string, 0, len(efforts))
	for _, effort := range efforts {
		values = append(values, string(effort))
	}
	return values
}

func joinReasoningEfforts(efforts []modelregistry.ReasoningEffort) string {
	values := make([]string, 0, len(efforts))
	for _, effort := range efforts {
		values = append(values, string(effort))
	}
	return strings.Join(values, ", ")
}

func (m model) handleStyleCommand(args string) (model, string) {
	args = strings.TrimSpace(strings.ToLower(args))
	if args == "" || args == "list" {
		return m, m.styleText()
	}
	if !responseStyleAllowed(args) {
		return m, "Style\nUnknown response style: " + args
	}
	m.responseStyle = args
	return m, strings.Join([]string{
		"Style",
		"active style: " + m.responseStyle,
		"Style preference is stored for this TUI session.",
	}, "\n")
}

func (m model) styleText() string {
	return renderCommandOutput(commandOutput{
		Title:  "Style",
		Status: commandStatusOK,
		Sections: []commandSection{{
			Title: "State",
			Lines: []string{
				"active style: " + m.responseStyle,
				"available: " + strings.Join(responseStyles, ", "),
			},
		}},
		Hints: []string{"use /style <value> to update this TUI session"},
	})
}

func defaultedResponseStyle(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if responseStyleAllowed(value) {
		return value
	}
	return defaultResponseStyle
}

func (m model) handleSelfCorrectCommand(args string) (model, string) {
	args = strings.TrimSpace(strings.ToLower(args))
	switch args {
	case "", "status":
		return m, m.selfCorrectText()
	case "on", "tests", "full":
		m.selfCorrectTests = true
	case "off", "lsp":
		m.selfCorrectTests = false
	default:
		return m, "Self-correct\nUsage: /selfcorrect [status|on|off|tests|full|lsp]"
	}
	// An explicit choice while a profile is active must survive a later
	// profile revert, even when it lands on the value the profile applied
	// (e.g. /selfcorrect off then on under thorough).
	if m.execProfileName != "" {
		m.execProfileSelfCorrectTouched = true
	}
	return m, m.selfCorrectText()
}

// maxTurnsCeiling caps the per-session /turns budget so a typo (e.g. /turns 99999)
// can't set an absurd ceiling; real multi-step tasks fit comfortably under it. Shared
// with config so applyEnv enforces the same bound on an inherited ZERO_MAX_TURNS.
const maxTurnsCeiling = config.MaxTurnsCeiling

func (m model) handleTurnsCommand(args string) (model, string) {
	args = strings.TrimSpace(args)
	if args == "" || strings.EqualFold(args, "status") {
		return m, m.turnsText()
	}
	n, err := strconv.Atoi(args)
	if err != nil || n < 1 {
		return m, "Turns\nUsage: /turns <n> — set the per-run tool-turn budget for this session (a positive number, e.g. /turns 150)."
	}
	if n > maxTurnsCeiling {
		n = maxTurnsCeiling
	}
	m.agentOptions.MaxTurns = n
	// Propagate the budget to spawned sub-agents / swarm members (which inherit the
	// environment) so a delegated task gets the same budget, not config.json's default.
	config.SetMaxTurnsEnv(n)
	// An explicit /turns while a profile is active is a pinned budget: mirror
	// headless exec's rule (an explicit --max-turns leaves nothing displaced)
	// by disarming the escalation's turn target, so a mid-run escalation can
	// never raise the budget the user just pinned. The other escalation
	// triggers stay armed. Mark the knob touched so a later profile revert
	// leaves this choice alone even if n coincides with the profile's value.
	if m.execProfileName != "" {
		m.execProfileTurnsTouched = true
		if m.agentOptions.Profile != nil && m.agentOptions.Profile.Escalate != nil {
			policy := *m.agentOptions.Profile
			escalate := *policy.Escalate
			escalate.MaxTurns = 0
			policy.Escalate = &escalate
			m.agentOptions.Profile = &policy
		}
	}
	return m, m.turnsText()
}

// handleProfileCommand shows or switches the session's execution profile: a
// loop-posture bundle (turn budget, reasoning effort, self-correction,
// escalation triggers) applied to the NEXT run, composing with the selected
// model rather than replacing it. Switching first restores whatever the
// previous profile displaced (profiles never stack), and balanced IS that
// restore: the empty profile, so selecting it just clears the posture.
func (m model) handleProfileCommand(args string) (model, string) {
	args = strings.TrimSpace(args)
	if args == "" || strings.EqualFold(args, "status") {
		return m, m.profileText()
	}
	profile, ok := execprofile.Lookup(args)
	if !ok {
		return m, "Profile\nUsage: /profile [status|" + strings.Join(execprofile.Names(), "|") + "] — switch the next run's loop posture."
	}
	// The SAME rule the headless exec path applies, not a second copy of it: a
	// workspace that disabled zeromaxing must refuse it identically here.
	// Refuse BEFORE reverting, so a rejected switch leaves the active profile
	// untouched rather than silently dropping the session to balanced.
	if refusal := execprofile.SelectionRefusal(profile, m.zeromaxingDisabled); refusal != "" {
		return m, "Profile\nCannot use " + profile.Name + ": " + refusal + "."
	}
	m = m.revertExecProfile()
	if profile.Name == execprofile.Balanced.Name {
		return m, m.profileText()
	}
	// Turn budget: displace the session's current budget. The displaced value
	// becomes the escalation restore target, exactly like headless exec, and
	// the new budget propagates to spawned sub-agents the same way /turns does.
	displacedMaxTurns := 0
	if profile.MaxTurns > 0 {
		displacedMaxTurns = m.agentOptions.MaxTurns
		m.agentOptions.MaxTurns = profile.MaxTurns
		m.execProfileAppliedMaxTurns = profile.MaxTurns
		m.execProfileDisplacedMaxTurns = displacedMaxTurns
		config.SetMaxTurnsEnv(profile.MaxTurns)
	}
	// Spend budget. Deliberately NOT exported to the environment the way the turn
	// budget is: a child inheriting the parent's ceiling would be handed the whole
	// budget each, so a plan of ten tasks could spend ten times it. The parent's
	// bound is the parent's.
	//
	// No displacement bookkeeping either, because nothing else sets this — there
	// is no /tokens command to preserve, so revert simply clears it.
	if profile.MaxTokens > 0 {
		m.agentOptions.MaxTokens = profile.MaxTokens
	}
	// Reasoning effort: fill only when the session is on auto AND the active
	// model supports the profile's level, mirroring exec's supported-effort
	// gating. An explicit user choice always wins over the profile.
	m.execProfileEffortUnraised = ""
	if want := modelregistry.ReasoningEffort(profile.ReasoningEffort); want != "" && m.reasoningEffort == "" {
		if m.profileEffortApplies(want) {
			m.reasoningEffort = want
			m.execProfileAppliedEffort = want
		} else {
			// Degrade honestly. The headless path already tells the user via
			// reasoningEffortNotice; the TUI used to skip the fill in SILENCE,
			// which is the same rule applied to one of two call paths. Record
			// the level we could not raise so the status output states it — the
			// rest of the posture (turn budget, self-correct) still applies.
			m.execProfileEffortUnraised = want
		}
	}
	// Self-correction is presence-only: a profile can arm it but never disarm
	// an explicit /selfcorrect choice.
	if profile.SelfCorrect && !m.selfCorrectTests {
		m.selfCorrectTests = true
		m.execProfileArmedSelfCorrect = true
	}
	m.agentOptions.Profile = profile.Policy(displacedMaxTurns, m.execProfileAppliedEffort != "")
	m.execProfileName = profile.Name
	if profile.IsZeromaxing() {
		m.zeromaxing = agent.ZeromaxingEntering
	}
	m.agentOptions.Zeromaxing = m.zeromaxing
	// The orchestrate tool reads this. Written through the SHARED gate pointer,
	// which is what a run's cloned registry also holds — see
	// TestClonedRegistrySharesTheGatePointer.
	m.zeromaxingGate.Set(m.zeromaxingActive())
	return m, m.profileText()
}

// revertExecProfile restores what the active profile displaced. Each knob is
// restored only when the user has not explicitly touched it since the profile
// was applied (the touched bits, set by /turns, /effort, Ctrl+T, and
// /selfcorrect) AND it still holds the value the profile applied — so manual
// overrides survive the revert even when they coincide with the profile's own
// value (e.g. /turns 30 under fast, or /selfcorrect off-then-on under
// thorough).
func (m model) revertExecProfile() model {
	if m.execProfileName == "" {
		return m
	}
	// The spend budget comes only from the profile, so removing the profile
	// removes it. Unconditional because nothing else can have set it.
	m.agentOptions.MaxTokens = 0
	if !m.execProfileTurnsTouched && m.execProfileAppliedMaxTurns > 0 && m.agentOptions.MaxTurns == m.execProfileAppliedMaxTurns {
		m.agentOptions.MaxTurns = m.execProfileDisplacedMaxTurns
		if m.execProfileDisplacedMaxTurns > 0 {
			config.SetMaxTurnsEnv(m.execProfileDisplacedMaxTurns)
		} else {
			// The profile's apply exported its budget to ZERO_MAX_TURNS (for
			// sub-agents), and SetMaxTurnsEnv ignores zero — so restoring a
			// zero displaced budget must clear the env explicitly or spawned
			// children would keep the removed profile's budget.
			_ = os.Unsetenv(config.MaxTurnsEnv)
		}
	}
	if !m.execProfileEffortTouched && m.execProfileAppliedEffort != "" && m.reasoningEffort == m.execProfileAppliedEffort {
		m.reasoningEffort = ""
	}
	if !m.execProfileSelfCorrectTouched && m.execProfileArmedSelfCorrect && m.selfCorrectTests {
		m.selfCorrectTests = false
	}
	// The FOURTH knob: the posture itself. Leaving zeromaxing must schedule
	// exactly one exit reminder for the next run — and leaving anything else
	// must not, which is why this is gated on the profile that was actually
	// active rather than on "a profile was active". revertExecProfile is
	// knob-by-knob, and a knob added without a line here is the classic miss.
	if m.execProfileName == execprofile.Name {
		m.zeromaxing = agent.ZeromaxingExiting
	}
	m.agentOptions.Zeromaxing = m.zeromaxing
	// Leaving the posture takes the tool away with it. Exiting is already
	// "off" for zeromaxingActive, so this clears the gate.
	m.zeromaxingGate.Set(m.zeromaxingActive())
	m.agentOptions.Profile = nil
	m.execProfileName = ""
	m.execProfileDisplacedMaxTurns = 0
	m.execProfileAppliedMaxTurns = 0
	m.execProfileAppliedEffort = ""
	m.execProfileEffortUnraised = ""
	m.execProfileArmedSelfCorrect = false
	m.execProfileTurnsTouched = false
	m.execProfileEffortTouched = false
	m.execProfileSelfCorrectTouched = false
	return m
}

// resolvedPostureLine is the unambiguous one-line answer to "what is actually
// in effect right now": the effort that will reach the provider, the active
// profile, and the turn budget. Both /effort status and /profile status render
// it, so the two surfaces can never report different resolved state — showing
// just "zeromaxing" would hide that the effort the provider receives is "high".
func (m model) resolvedPostureLine() string {
	profile := m.execProfileName
	if profile == "" {
		profile = execprofile.Balanced.Name + " (default)"
	}
	return fmt.Sprintf("effort: %s  ·  profile: %s  ·  turns: %d",
		m.effortDisplay(), profile, m.agentOptions.MaxTurns)
}

// zeromaxingNotes returns the honest-delta lines for the active posture: what
// it really changes, and anything it could NOT apply on this model.
func (m model) zeromaxingNotes() []string {
	if m.execProfileName != execprofile.Name {
		return nil
	}
	// ONE source. The effort clause used to live here as a second, separate
	// line while Delta carried its own fixed "unchanged" claim, and the two
	// could contradict each other — they did, in real use. Now every clause
	// comes from one DeltaState, so exactly one effort statement exists.
	return []string{execprofile.Delta(execprofile.DeltaState{
		CurrentMaxTurns: m.execProfileDisplacedMaxTurns,
		Effort:          m.effortTransition(),
		SelfCorrect:     m.selfCorrectTransition(),
	})}
}

// effortTransition reports what the posture did to reasoning effort on this
// model, from the session's live state.
func (m model) effortTransition() execprofile.EffortTransition {
	switch {
	case m.execProfileEffortUnraised != "":
		return execprofile.EffortNotSupported
	case m.execProfileAppliedEffort != "":
		return execprofile.EffortRaised
	default:
		// The posture wanted to fill and did not, and did not record a refusal:
		// the caller already had an effort of their own.
		return execprofile.EffortKeptExplicit
	}
}

// profileText is the /profile status card: ALIGNED KEY-VALUE ROWS, each fact
// said once. The previous version stacked three renderers — a labelled list,
// resolvedPostureLine, and the posture delta sentence — so the profile name
// appeared twice and the effort and turn budget three times each; a status
// card that repeats itself reads as noise, and this one was called irritating
// to its face. The delta facts survive as short clauses on the row they
// qualify ("was 80", "raised by the posture") instead of a second paragraph.
func (m model) profileText() string {
	name := m.execProfileName
	if name == "" {
		name = execprofile.Balanced.Name + " (default)"
	}
	// KEY VALUE, single space — not padded columns: compactCommandOutputText
	// collapses every whitespace run at render, so alignment written here would
	// silently disappear. The single-space form is what actually reaches the
	// screen, and what the tests assert.
	row := func(key, value string) string { return key + " " + value }

	turns := strconv.Itoa(m.agentOptions.MaxTurns)
	effort := m.effortDisplay()
	verify := "lsp only"
	if m.selfCorrectTests {
		verify = "lsp + tests"
	}
	if m.execProfileName == execprofile.Name {
		if was := m.execProfileDisplacedMaxTurns; was > 0 && was != m.agentOptions.MaxTurns {
			turns += fmt.Sprintf(" · was %d", was)
		}
		turns += " · inherited by sub-agents"
		switch m.effortTransition() {
		case execprofile.EffortRaised:
			effort += " · raised by the posture"
		case execprofile.EffortNotSupported:
			// NAME THE LEVEL. "could not raise it" states a failure without its
			// object; the recorded unraised level is exactly what the user needs
			// to know this model refused.
			effort += " · NOT raised — " + string(m.execProfileEffortUnraised) + " is unsupported on this model"
		}
		// The verify row carries its TRANSITION, tracked from live state — a
		// user who turns self-correct back off must not keep reading a raise
		// the session no longer has. Worded without "raised" so the one-effort-
		// transition invariant (TestBothStatusSurfacesShowResolvedState) counts
		// the effort claim alone.
		switch m.selfCorrectTransition() {
		case execprofile.SelfCorrectRaised:
			verify = "lsp → tests (posture)"
		case execprofile.SelfCorrectAlreadyOn:
			verify = "unchanged (tests) — already on"
		case execprofile.SelfCorrectOverridden:
			verify = "lsp only — your /selfcorrect off overrides the posture"
		}
	}
	parts := []string{
		row("profile", name),
		row("turns", turns),
		row("effort", effort),
		row("verify", verify),
	}
	if m.agentOptions.Profile != nil && m.agentOptions.Profile.Escalate != nil {
		target := "keeps the pinned turn budget"
		if t := m.agentOptions.Profile.Escalate.MaxTurns; t > 0 {
			target = fmt.Sprintf("→ %d turns", t)
		}
		// The headless-only clause is a real asymmetry, not decoration: the
		// uncertain-completion trigger cannot fire interactively, and a user
		// arming escalation in the TUI deserves to know one of its tripwires
		// is not live here.
		parts = append(parts, row("escalate", "armed · one-shot on a failure streak · "+target+" · uncertain-completion trigger is headless-only"))
	}
	// ONE LINE. Four key-value rows still made a five-line card for what is a
	// single sentence of state; a status readout the user asked for by name
	// needs no section header and no hint advertising the command they just
	// typed. Every fact keeps its clause; the renderer wraps when narrow.
	return renderCommandOutput(commandOutput{
		Title:  "Profile",
		Status: commandStatusOK,
		Sections: []commandSection{{
			Lines: []string{strings.Join(parts, " · ")},
		}},
	})
}

func (m model) turnsText() string {
	return renderCommandOutput(commandOutput{
		Title:  "Turns",
		Status: commandStatusOK,
		Sections: []commandSection{{
			Title: "State",
			Lines: []string{fmt.Sprintf("max tool-turns per run: %d", m.agentOptions.MaxTurns)},
		}},
		Hints: []string{fmt.Sprintf("/turns <n> sets this session's tool-turn budget (max %d); raise it for long multi-step tasks like delegation/swarm runs", maxTurnsCeiling)},
	})
}

func (m model) selfCorrectText() string {
	depth := "LSP diagnostics only — fast, scoped to changed files"
	if m.selfCorrectTests {
		depth = "LSP diagnostics + project test plan (e.g. go test ./...) — slower, whole-repo"
	}
	return renderCommandOutput(commandOutput{
		Title:  "Self-correct",
		Status: commandStatusOK,
		Sections: []commandSection{{
			Title: "State",
			Lines: []string{
				"post-edit verification: " + depth,
				"auto-fix vs report-only: follows the active permission mode",
			},
		}},
		Hints: []string{"/selfcorrect on adds the full test suite; /selfcorrect off returns to LSP-only (default)"},
	})
}

func responseStyleAllowed(value string) bool {
	for _, style := range responseStyles {
		if value == style {
			return true
		}
	}
	return false
}

func (m model) handleCompactCommand(args string) (model, string, tea.Cmd) {
	args = strings.TrimSpace(strings.ToLower(args))
	if args == "status" {
		return m, m.compactText(false), nil
	}
	// Bare "/compact" already triggers compaction; accept "now" too since that is
	// what users reach for when they see the context gauge climbing.
	if args != "" && args != "now" {
		return m, "Compact\nusage: /compact [status|now]", nil
	}
	if m.compactInFlight {
		return m, m.compactText(true), nil
	}
	m.compactRequests++
	m.lastCompactError = ""
	m.lastCompactResult = nil
	if m.sessionCompactor == nil && !m.hasSessionBackedCompactor() {
		return m, m.compactText(true), nil
	}
	m.compactInFlight = true
	m.compactFrame = 0
	return m, m.compactText(true), tea.Batch(m.runCompact(), m.spinner.Tick)
}

func (m model) runCompact() tea.Cmd {
	request := m.compactRequest()
	if m.sessionCompactor != nil {
		compactor := m.sessionCompactor
		ctx := m.ctx
		return func() tea.Msg {
			result, err := compactor.CompactSession(ctx, request)
			return compactResultMsg{result: result, err: err}
		}
	}
	return func() tea.Msg {
		next, result, err := m.compactActiveSession()
		if err != nil {
			return compactResultMsg{err: err}
		}
		return compactResultMsg{
			result:             result,
			activeSession:      next.activeSession,
			sessionEvents:      append([]sessions.Event{}, next.sessionEvents...),
			transcript:         append([]transcriptRow{}, next.transcript...),
			hasSessionSnapshot: true,
		}
	}
}

// handleRewindCommand restores workspace files to a checkpoint and truncates the
// session log. "/rewind" or "/rewind latest" undoes the most recent checkpoint;
// "/rewind <n>" rewinds to a specific event sequence.
func (m model) handleRewindCommand(args string) (model, string, tea.Cmd) {
	if m.sessionStore == nil || m.activeSession.SessionID == "" {
		return m, "Rewind\nno active session to rewind.", nil
	}
	if m.pending {
		return m, "Rewind\ncannot rewind while a run is in progress.", nil
	}
	// A cancelled run's late flush hasn't appended its checkpoint events yet:
	// ApplyRewind would prune those checkpoint blobs as unreferenced, then the
	// flush would re-append pre-rewind events after the rewind marker.
	if len(m.flushRunIDs) > 0 {
		return m, "Rewind\ncannot rewind while a cancelled run is still flushing — retry in a moment.", nil
	}
	arg := strings.TrimSpace(strings.ToLower(args))
	target, err := m.resolveRewindTarget(arg)
	if err != nil {
		return m, "Rewind\n" + err.Error(), nil
	}

	// Workspace files may be touched or restored starting with ApplyRewind:
	// Invalidate file view cache immediately so any partial or full disk mutation
	// cannot serve stale pre-rewind snapshots regardless of subsequent step failures.
	// The purge advances the cache generation, which retires the open view's
	// snapshot and any in-flight load without a view-local revision bump.
	defaultFileViewCache.invalidateUnknownScope()

	report, err := m.sessionStore.ApplyRewind(m.activeSession.SessionID, m.cwd, target)
	if err != nil {
		var refreshCmd tea.Cmd
		if m.fileView.active && m.fileView.mode == fileViewFull {
			m, refreshCmd = m.startFileViewRefreshCmd(m.chatColumnWidth())
		}
		return m, "Rewind\n" + err.Error(), refreshCmd
	}

	// ApplyRewind truncated the persisted event log, restored files, and appended a
	// rewind marker — so reload the in-memory session state. Otherwise the dropped
	// events would still (a) be re-sent to the agent as ContextEvents on the next
	// prompt (sessionPrompt sends m.sessionEvents) and (b) linger in the transcript.
	// A reload FAILURE must be surfaced (and the stale in-memory context dropped),
	// not ignored — silently keeping m.sessionEvents would re-send rewound-away
	// events on the next prompt, defeating the rewind.
	if meta, getErr := m.sessionStore.Get(m.activeSession.SessionID); getErr == nil {
		m.activeSession = *meta
	}
	events, readErr := m.sessionStore.ReadEvents(m.activeSession.SessionID)
	if readErr != nil {
		m.sessionEvents = nil // drop stale context so it can't reach the next prompt
		var refreshCmd tea.Cmd
		if m.fileView.active && m.fileView.mode == fileViewFull {
			m, refreshCmd = m.startFileViewRefreshCmd(m.chatColumnWidth())
		}
		return m, fmt.Sprintf("Rewind\nrewound to sequence %d, but reloading the session failed (in-memory context cleared): %s", target, readErr.Error()), refreshCmd
	}
	m.sessionEvents = append([]sessions.Event{}, events...)
	rows := initialTranscript()
	rows = appendTranscriptRowsDedup(rows, transcriptRowsFromSessionEvents(events))
	m.transcript = rows
	// The rebuilt (post-rewind) transcript flushes fresh below a divider; the
	// pre-rewind scrollback above it stays, as scrollback cannot be un-printed.
	m.resetFlushFrontier("· rewound ·")

	var refreshCmd tea.Cmd
	if m.fileView.active && m.fileView.mode == fileViewFull {
		m, refreshCmd = m.startFileViewRefreshCmd(m.chatColumnWidth())
	}

	summary := fmt.Sprintf("Rewound to sequence %d\n%d file(s) restored, %d deleted, %d skipped.",
		target, report.FilesRestored, report.FilesDeleted, len(report.Skipped))
	if len(report.Skipped) > 0 {
		summary += "\nskipped (not recoverable): " + strings.Join(report.Skipped, ", ")
	}
	return m, summary, refreshCmd
}

// resolveRewindTarget maps a /rewind argument to a keep-through event sequence.
func (m model) resolveRewindTarget(arg string) (int, error) {
	events, err := m.sessionStore.ReadEvents(m.activeSession.SessionID)
	if err != nil {
		return 0, err
	}
	if arg == "" || arg == "latest" {
		lastCheckpoint := 0
		for _, ev := range events {
			if ev.Type == sessions.EventSessionCheckpoint && ev.Sequence > lastCheckpoint {
				lastCheckpoint = ev.Sequence
			}
		}
		if lastCheckpoint == 0 {
			return 0, fmt.Errorf("no checkpoints to rewind")
		}
		return lastCheckpoint - 1, nil // undo the most recent checkpoint
	}
	seq, err := strconv.Atoi(arg)
	if err != nil || seq < 0 {
		return 0, fmt.Errorf("usage: /rewind [latest|<sequence>]")
	}
	return seq, nil
}

func (m model) compactText(requested bool) string {
	if m.compactInFlight {
		return m.compactRunningText()
	}
	if requested && m.lastCompactResult != nil {
		return compactCompleteText(*m.lastCompactResult)
	}
	status := commandStatusInfo
	if requested {
		status = commandStatusWarning
		if m.lastCompactResult != nil {
			status = commandStatusOK
		}
		if m.lastCompactError != "" {
			status = commandStatusBlocked
		}
	}
	request := m.compactRequest()
	sections := []commandSection{
		{
			Title: "State",
			Lines: []string{
				"summary: " + m.compactionStatus(),
				"requested: " + boolText(m.compactRequests > 0),
				"model: " + displayValue(request.ModelName, "none"),
				"context window: " + compactContextWindowText(request.ContextWindow),
				"auto compaction: model adaptive at ~80% of context window",
				fmt.Sprintf("estimated transcript: %d tokens", request.EstimatedTokens),
				fmt.Sprintf("visible transcript rows: %d", request.VisibleTranscriptRows),
				m.compactabilityText(request),
			},
		},
	}
	if m.lastCompactResult != nil {
		sections = append(sections, commandSection{
			Title: "Result",
			Lines: compactResultLines(*m.lastCompactResult),
		})
	} else if m.lastCompactError != "" {
		sections = append(sections, commandSection{
			Title: "Result",
			Lines: []string{
				"compacted: no",
				"error: " + m.lastCompactError,
			},
		})
	} else if requested && m.sessionCompactor == nil {
		sections = append(sections, commandSection{
			Title: "Result",
			Lines: []string{
				"compacted: no",
				"reason: manual compactor unavailable",
			},
		})
	}
	return renderCommandOutput(commandOutput{
		Title:    "Compact",
		Status:   status,
		Sections: sections,
		Hints:    []string{"manual compaction affects this TUI session when a compactor is available"},
	})
}

func (m model) compactRunningText() string {
	return strings.Join([]string{
		"Compressing session",
		"Keep editing your draft; press Enter after compression finishes to send.",
		m.compactAnimationLine(),
	}, "\n")
}

func compactCompleteText(result CompactResult) string {
	lines := []string{
		"Compression complete",
		"Session summary saved.",
		"Ready for the next prompt.",
	}
	if result.Compacted && result.BeforeTokens > 0 && result.AfterTokens > 0 && result.AfterTokens < result.BeforeTokens {
		lines = append(lines, fmt.Sprintf("Estimated context reduced by %s tokens.", formatContextWindow(result.BeforeTokens-result.AfterTokens)))
	}
	return strings.Join(lines, "\n")
}

func (m model) compactionStatus() string {
	if m.compactInFlight {
		return "compacting context"
	}
	if m.lastCompactResult != nil {
		if m.lastCompactResult.Compacted {
			return "compacted manually"
		}
		return "manual compactor completed without changes"
	}
	if m.lastCompactError != "" {
		return "manual compaction failed"
	}
	if m.compactRequests > 0 {
		return "requested, awaiting manual compactor"
	}
	return "not compacted"
}

func (m model) compactRequest() CompactRequest {
	return CompactRequest{
		SessionID:             strings.TrimSpace(m.activeSession.SessionID),
		ModelName:             strings.TrimSpace(m.modelName),
		ContextWindow:         m.modelContextWindow(m.modelName),
		SessionEventCount:     len(m.sessionEvents),
		EstimatedTokens:       estimateTranscriptTokens(m.transcript),
		VisibleTranscriptRows: len(m.transcript),
		CompactRequests:       m.compactRequests,
	}
}

func (m model) compactabilityText(request CompactRequest) string {
	if m.sessionCompactor == nil && !m.hasSessionBackedCompactor() {
		return "compactable: no (manual compactor unavailable)"
	}
	if m.sessionCompactor == nil && request.SessionEventCount <= tuiCompactionPreserveLast {
		return "compactable: no (not enough session history yet)"
	}
	if request.ContextWindow <= 0 {
		return "compactable: no (unknown model context window)"
	}
	if request.EstimatedTokens <= 0 {
		return "compactable: no (empty transcript)"
	}
	return "compactable: yes"
}

func (m model) hasSessionBackedCompactor() bool {
	return m.sessionStore != nil && strings.TrimSpace(m.activeSession.SessionID) != ""
}

func (m model) compactActiveSession() (model, CompactResult, error) {
	if m.sessionStore == nil || strings.TrimSpace(m.activeSession.SessionID) == "" {
		return m, CompactResult{}, fmt.Errorf("no active session to compact")
	}
	beforeEvents := append([]sessions.Event{}, m.sessionEvents...)
	beforeTokens := estimateTranscriptTokens(m.transcript)
	plan, err := m.sessionStore.PlanCompaction(m.activeSession.SessionID, sessions.CompactionOptions{
		PreserveLast: tuiCompactionPreserveLast,
		SkipPrompt:   true,
	})
	if err != nil {
		return m, CompactResult{}, err
	}
	if plan.CompactableCount == 0 {
		return m, CompactResult{
			Compacted:    false,
			BeforeTokens: beforeTokens,
			AfterTokens:  beforeTokens,
			Summary:      "not enough session history to compact yet",
		}, nil
	}

	rawEvents, err := m.sessionStore.ReadEvents(m.activeSession.SessionID)
	if err != nil {
		return m, CompactResult{}, fmt.Errorf("read session events for compaction: %w", err)
	}
	compactableEvents, err := sessionEventsForRefs(rawEvents, plan.CompactableEvents)
	if err != nil {
		return m, CompactResult{}, err
	}
	summaryResult, err := m.summarizeCompactionPlan(plan, sessions.CompactionMessages(compactableEvents))
	if err != nil {
		return m, CompactResult{}, err
	}
	// PromptChars counts the text content sent to the provider: the shared
	// system instruction plus projected message/tool payloads. Provider protocol
	// framing is intentionally excluded.
	plan.PromptChars = len(agent.CompactionSummaryInstructions) + summaryResult.ProjectedChars
	plan.Truncated = summaryResult.Truncated
	if _, err := m.sessionStore.RecordCompaction(m.activeSession.SessionID, sessions.RecordCompactionInput{
		Plan:    plan,
		Summary: summaryResult.SummaryText,
	}); err != nil {
		return m, CompactResult{}, err
	}
	if meta, err := m.sessionStore.Get(m.activeSession.SessionID); err == nil && meta != nil {
		m.activeSession = *meta
	}
	events, err := m.sessionStore.ReadRehydratedEvents(m.activeSession.SessionID)
	if err != nil {
		m.sessionEvents = nil
		return m, CompactResult{}, fmt.Errorf("reload compacted session: %w", err)
	}
	m.sessionEvents = append([]sessions.Event{}, events...)
	rows := initialTranscript()
	rows = appendTranscriptRowsDedup(rows, transcriptRowsFromSessionEvents(events))
	m.transcript = rows
	m.resetFlushFrontier("· compacted ·")

	return m, CompactResult{
		Compacted:    len(events) < len(beforeEvents)+1,
		BeforeTokens: beforeTokens,
		AfterTokens:  estimateTranscriptTokens(m.transcript),
		Summary:      summaryResult.SummaryText,
	}, nil
}

func sessionEventsForRefs(events []sessions.Event, refs []sessions.EventRef) ([]sessions.Event, error) {
	byID := make(map[string]sessions.Event, len(events))
	for _, event := range events {
		byID[event.ID] = event
	}
	selected := make([]sessions.Event, 0, len(refs))
	for _, ref := range refs {
		event, ok := byID[ref.ID]
		if !ok || event.Sequence != ref.Sequence || event.Type != ref.Type {
			return nil, fmt.Errorf("session changed while compaction was being planned; retry /compact")
		}
		selected = append(selected, event)
	}
	return selected, nil
}

func (m model) summarizeCompactionPlan(plan sessions.CompactionPlan, messages []zeroruntime.Message) (agent.CompactionSummaryResult, error) {
	return agent.SummarizeCompactionMessages(messages, func(projected []zeroruntime.Message) (string, error) {
		if m.provider == nil {
			return deterministicCompactionSummary(plan), nil
		}
		stream, streamErr := m.provider.StreamCompletion(m.ctx, zeroruntime.CompletionRequest{
			Messages: append([]zeroruntime.Message{{Role: zeroruntime.MessageRoleSystem, Content: agent.CompactionSummaryInstructions}}, projected...),
		})
		if streamErr != nil {
			return "", fmt.Errorf("summarize compacted session: %w", streamErr)
		}
		collected := zeroruntime.CollectStream(m.ctx, stream)
		if collected.Error != "" {
			return "", fmt.Errorf("summarize compacted session: %s", collected.Error)
		}
		result := strings.TrimSpace(collected.Text)
		if result == "" {
			return "", fmt.Errorf("summarize compacted session: empty summary")
		}
		return result, nil
	})
}

func deterministicCompactionSummary(plan sessions.CompactionPlan) string {
	lines := []string{
		fmt.Sprintf("Compacted earlier session context: %d event(s) summarized, %d recent event(s) preserved.", plan.CompactableCount, plan.PreservedCount),
	}
	if len(plan.CompactableEvents) > 0 {
		first := plan.CompactableEvents[0]
		last := plan.CompactableEvents[len(plan.CompactableEvents)-1]
		lines = append(lines, fmt.Sprintf("Compacted range: #%d %s through #%d %s.", first.Sequence, first.Type, last.Sequence, last.Type))
	}
	if len(plan.PreservedEvents) > 0 {
		first := plan.PreservedEvents[0]
		last := plan.PreservedEvents[len(plan.PreservedEvents)-1]
		lines = append(lines, fmt.Sprintf("Preserved recent range: #%d %s through #%d %s.", first.Sequence, first.Type, last.Sequence, last.Type))
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func compactContextWindowText(window int) string {
	if window <= 0 {
		return "unknown"
	}
	return formatContextWindow(window) + " tokens"
}

func estimateTranscriptTokens(rows []transcriptRow) int {
	total := 0
	for _, row := range rows {
		text := row.text + "\n" + row.detail + "\n" + row.tool + "\n" + row.arg
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		total += agent.ApproxTextTokens(text) + 4
	}
	return total
}

func compactResultLines(result CompactResult) []string {
	lines := []string{
		"compacted: " + boolText(result.Compacted),
	}
	if result.BeforeTokens > 0 {
		lines = append(lines, fmt.Sprintf("before: %d tokens", result.BeforeTokens))
	}
	if result.AfterTokens > 0 {
		lines = append(lines, fmt.Sprintf("after: %d tokens", result.AfterTokens))
	}
	if summary := strings.TrimSpace(result.Summary); summary != "" {
		lines = append(lines, "summary: "+summary)
	}
	return lines
}

func (m model) compactAnimationLine() string {
	frame := compactFrames[m.compactFrame%len(compactFrames)]
	return frame + " Compressing history..."
}

func (m model) setCompactStatusRow(text string) model {
	row := transcriptRow{kind: rowSystem, id: compactStatusRowID, text: text}
	for i := len(m.transcript) - 1; i >= 0; i-- {
		if m.transcript[i].id == compactStatusRowID {
			m.transcript[i] = row
			// Settled alt-screen rows are cached across frames; force a rebuild
			// so an in-place update (e.g. the compact spinner tick) is reflected
			// immediately instead of serving the stale cached snapshot.
			if i < m.flushed {
				m.altScreenSettledWidth = 0
			}
			return m
		}
	}
	m.transcript = appendTranscriptRow(m.transcript, row)
	return m
}

func (m model) recordUsageEvent(modelID string, event zeroruntime.Usage) (model, []transcriptRow) {
	if m.usageTracker == nil || strings.TrimSpace(modelID) == "" {
		return m, nil
	}
	normalized, runtimeUsage, err := usage.Normalize(event)
	if err != nil {
		return m, []transcriptRow{{kind: rowError, text: "usage: " + err.Error()}}
	}
	m.lastUsage = normalized
	m.lastUsageSeen = true
	if _, err := m.usageTracker.Record(usage.RecordInput{
		ModelID: modelID,
		Usage:   runtimeUsage,
		Source:  "tui",
	}); err != nil {
		if isUnpricedUsageError(err) {
			m.unpricedRequests++
			m.unpricedTokens += normalized.TotalTokens
			return m, nil
		}
		return m, []transcriptRow{{kind: rowError, text: "usage: " + err.Error()}}
	}
	return m, nil
}

func (m model) latestUsageTokens(summary usage.Summary) int {
	if m.lastUsageSeen && m.lastUsage.TotalTokens > 0 {
		return m.lastUsage.TotalTokens
	}
	if summary.LastRecord != nil && summary.LastRecord.Usage.TotalTokens > 0 {
		return summary.LastRecord.Usage.TotalTokens
	}
	return m.unpricedTokens
}

func isUnpricedUsageError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"unknown zero model",
		"missing model input pricing rate",
		"missing model output pricing rate",
		"invalid model cached input pricing rate",
		"no model cost tier covers",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func (m model) usageSummaryText() string {
	if m.usageTracker == nil {
		return "usage unavailable"
	}
	summary := m.usageTracker.Summary()
	if summary.RecordCount == 0 && m.unpricedRequests == 0 {
		return "no usage yet"
	}
	if summary.RecordCount == 0 {
		return formatUnpricedUsage(m.unpricedRequests, m.unpricedTokens)
	}
	if m.unpricedRequests == 0 {
		return usage.FormatSummary(summary)
	}
	return usage.FormatSummary(summary) + "; " + formatUnpricedUsage(m.unpricedRequests, m.unpricedTokens)
}

// cacheEfficiencyText reports the session's prompt-cache hit rate for /context so a
// user can see whether cache reads are actually saving work. A low rate across
// several turns usually means the cacheable prefix is churning (e.g. a tool list
// that shifts mid-session), so it flags that case to make slowness diagnosable.
func (m model) cacheEfficiencyText() string {
	if m.usageTracker == nil {
		return "usage unavailable"
	}
	summary := m.usageTracker.Summary()
	if summary.InputTokens <= 0 {
		return "n/a"
	}
	text := usage.FormatCacheEfficiency(summary)
	if summary.RecordCount > 5 && summary.CacheHitRate() < 0.5 {
		text += " — low; prefix may be unstable"
	}
	return text
}

func formatUnpricedUsage(requests int, tokens int) string {
	requestLabel := "requests"
	if requests == 1 {
		requestLabel = "request"
	}
	return fmt.Sprintf("%d %s, %d tokens, cost unavailable", requests, requestLabel, tokens)
}

// selfCorrectTransition reports what the posture is ACTUALLY doing to post-edit
// verification right now — not what it did at selection time, because
// /selfcorrect can be used afterwards and the status output must not keep
// claiming a raise the session no longer has.
//
// execProfileArmedSelfCorrect records that the profile turned it on; combined
// with the live selfCorrectTests bit that distinguishes all three cases.
func (m model) selfCorrectTransition() execprofile.SelfCorrectTransition {
	switch {
	case !m.selfCorrectTests:
		// The posture wants it on, so it being off means the user turned it
		// back off explicitly.
		return execprofile.SelfCorrectOverridden
	case m.execProfileArmedSelfCorrect:
		return execprofile.SelfCorrectRaised
	default:
		return execprofile.SelfCorrectAlreadyOn
	}
}

// zeromaxingChipLabel is the footer indicator for the zeromaxing posture. Kept
// as a constant so the view and its test assert the same bytes.
//
// LOWERCASE, like every other footer label beside it — "ask", "high", the model
// name. Shouting it was the badge's job, and the badge is gone; the word earns
// attention now by moving rather than by being the one thing in caps.
const zeromaxingChipLabel = "zeromaxing"

// advanceZeromaxing retires the one-shot notices once the run that carried them
// has finished.
//
// Entering -> Active: the enter notice fired on that run's first turn, so every
// later run reports the posture as already on (its first turn gets still-on,
// not a second enter).
//
// Exiting -> Off: the exit notice fired once and the posture is now simply
// gone; leaving the state at Exiting would re-announce the exit on every
// subsequent run.
//
// Active and Off are terminal here — this is called after EVERY run, including
// runs with no posture at all, so it must be a no-op for them.
func (m model) advanceZeromaxing() model {
	switch m.zeromaxing {
	case agent.ZeromaxingEntering:
		m.zeromaxing = agent.ZeromaxingActive
	case agent.ZeromaxingExiting:
		m.zeromaxing = agent.ZeromaxingOff
	default:
		return m
	}
	m.agentOptions.Zeromaxing = m.zeromaxing
	return m
}

// zeromaxingActive reports whether the session currently holds the posture, for
// the footer chip. Exiting is deliberately excluded: the posture is already off,
// and the pending notice is only the announcement of that.
func (m model) zeromaxingActive() bool {
	return m.zeromaxing == agent.ZeromaxingEntering || m.zeromaxing == agent.ZeromaxingActive
}
