package tui

import (
	"context"
	"os"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/providercatalog"
	"github.com/Gitlawb/zero/internal/providermodelcatalog"
	"github.com/Gitlawb/zero/internal/providermodeldiscovery"
	"github.com/Gitlawb/zero/internal/providers"
)

// pickerKind identifies which command a picker selection feeds back into.
type pickerKind int

const (
	pickerModel pickerKind = iota
	pickerEffort
	pickerSession
	pickerTheme
	pickerSkill
	pickerSTTModel
	pickerSTTDownload
	pickerPet
)

// pickerItem is one selectable row: Label is shown, Value is passed to the
// underlying command handler when chosen. Meta is the optional right-aligned
// readout (ctx window · capabilities); the dot flags mark provider locality
// for model rows (accent = remote, blue = local). Detail is an optional faint
// second line under the row (project · model · size for sessions), rendered
// only on medium-and-wider tiers where the extra line reads cleanly.
type pickerItem struct {
	Group    string
	Label    string
	Value    string
	Meta     string
	Detail   string
	Provider string // display tag (catalog id / locality)
	// OwnerProvider is the saved provider profile name a model belongs to, so the
	// /model picker can switch providers when a model from a non-active provider is
	// chosen. Empty for non-model items.
	OwnerProvider string
	Remote        bool
	Local         bool
	Favorite      bool
}

// commandPicker is a generic single-select overlay reused by /model and /effort
// (invoked with no argument). It owns only list state; the chosen
// value is applied through the existing command handlers.
type commandPicker struct {
	kind     pickerKind
	title    string
	items    []pickerItem
	allItems []pickerItem
	query    string
	selected int
	// loading marks a picker still fetching its rows (e.g. the STT model list from
	// GitHub): the overlay shows a "fetching…" line instead of "no matching items".
	loading bool
}

func (p *commandPicker) move(delta int) {
	n := len(p.items)
	if n == 0 {
		return
	}
	p.selected = ((p.selected+delta)%n + n) % n
}

func (p *commandPicker) current() (pickerItem, bool) {
	if p.selected < 0 || p.selected >= len(p.items) {
		return pickerItem{}, false
	}
	return p.items[p.selected], true
}

func (p *commandPicker) appendQuery(runes []rune) {
	for _, r := range runes {
		if r < 32 {
			continue
		}
		p.query += string(r)
	}
	p.applyQuery()
}

func (p *commandPicker) deleteQueryRune() {
	if p.query == "" {
		return
	}
	runes := []rune(p.query)
	p.query = string(runes[:len(runes)-1])
	p.applyQuery()
}

func (p *commandPicker) applyQuery() {
	source := p.allItems
	if len(source) == 0 {
		source = p.items
	}
	query := strings.ToLower(strings.TrimSpace(p.query))
	if query == "" {
		p.items = append([]pickerItem{}, source...)
		p.selected = clampInt(p.selected, 0, maxInt(0, len(p.items)-1))
		return
	}

	// Rank matches (exact < prefix < contains < subsequence) instead of a flat
	// substring filter, so the closest match to "sonnet 4.5" lands at the top
	// rather than buried behind scrolling. Groups stay contiguous — a group is
	// ordered by its best-matching item and never split into two header blocks —
	// so the grouped model picker still renders one header per provider.
	type entry struct {
		item  pickerItem
		score int
		order int
	}
	groupFirst := map[string]int{}
	groupBest := map[string]int{}
	entries := make([]entry, 0, len(source))
	for index, item := range source {
		score, ok := scorePickerItem(item, query)
		if !ok {
			continue
		}
		if _, seen := groupFirst[item.Group]; !seen {
			groupFirst[item.Group] = index
		}
		if best, seen := groupBest[item.Group]; !seen || score < best {
			groupBest[item.Group] = score
		}
		entries = append(entries, entry{item: item, score: score, order: index})
	}
	sort.SliceStable(entries, func(a, b int) bool {
		ga, gb := entries[a].item.Group, entries[b].item.Group
		if ga != gb {
			// Most-relevant group first; ties keep original group appearance order so
			// a group is never scattered across the list.
			if groupBest[ga] != groupBest[gb] {
				return groupBest[ga] < groupBest[gb]
			}
			return groupFirst[ga] < groupFirst[gb]
		}
		// Within a group: best match first, then original order.
		if entries[a].score != entries[b].score {
			return entries[a].score < entries[b].score
		}
		return entries[a].order < entries[b].order
	})
	filtered := make([]pickerItem, 0, len(entries))
	for _, e := range entries {
		filtered = append(filtered, e.item)
	}
	p.items = filtered
	p.selected = 0
}

// scorePickerItem ranks an item against a lowercased query; lower is better, and
// ok is false when it doesn't match at all. Tiers mirror scoreFileSuggestion:
// exact/prefix/contains on the label (what the user reads) beat matches deeper in
// the joined haystack, and a fuzzy subsequence is the last-resort match.
func scorePickerItem(item pickerItem, query string) (int, bool) {
	label := strings.ToLower(item.Label)
	hay := strings.ToLower(strings.Join([]string{item.Group, item.Label, item.Value, item.Meta, item.Detail}, " "))
	switch {
	case label == query:
		return 0, true
	case strings.HasPrefix(label, query):
		return 20, true
	case strings.Contains(label, query):
		return 40, true
	case strings.HasPrefix(hay, query):
		return 60, true
	case strings.Contains(hay, query):
		return 80, true
	default:
		if gap, ok := fuzzySubsequenceGap(hay, query); ok {
			return 120 + gap, true
		}
		return 0, false
	}
}

// newModelPicker lists active (non-deprecated) models, preselecting the active
// one. Returns nil when the catalog is unavailable so the caller falls back to
// the plain status text.
func (m model) newModelPicker() *commandPicker {
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		return nil
	}
	activeModel := strings.TrimSpace(m.modelName)
	activeProvider := strings.TrimSpace(m.providerName)
	recent := []pickerItem{}
	for _, pair := range m.recentModelPairsForPicker() {
		recent = append(recent, m.modelPickerRecentItem(registry, pair.Provider, pair.Model))
	}
	catalog := []pickerItem{}
	// List every saved provider's models, grouped per provider (one contiguous
	// section each), so /model shows all configured providers and you can switch
	// across them. Dedup by group so two profiles resolving to the same provider
	// don't produce a repeated section.
	seenGroup := map[string]bool{}
	for _, profile := range m.modelPickerProviders() {
		descriptor, hasDescriptor := m.descriptorForProfile(profile)
		group := modelPickerProviderGroup(profile, descriptor, hasDescriptor)
		if seenGroup[group] {
			continue
		}
		seenGroup[group] = true
		catalog = append(catalog, m.savedProviderModelPickerItems(profile, activeProvider, activeModel)...)
	}
	if len(catalog) == 0 {
		// No saved providers resolved any models: fall back to the full registry.
		// Only skip the active model when the entry's own provider also matches the
		// active provider — a different provider offering the same model id is a
		// distinct, independently selectable row (same provider-aware de-dup this
		// PR applies everywhere else), not a duplicate of the pinned "Recent" row.
		for _, entry := range registry.List(modelregistry.ListOptions{}) {
			if entry.ID == activeModel && (activeProvider == "" || strings.EqualFold(string(entry.Provider), activeProvider)) {
				continue
			}
			catalog = append(catalog, registryModelPickerItem(entry, "Catalog"))
		}
	}
	items := m.assembleModelPickerItems(recent, catalog)
	if len(items) == 0 {
		return nil
	}
	return &commandPicker{kind: pickerModel, title: "Choose a model", items: items, allItems: append([]pickerItem{}, items...), selected: 0}
}

// newSkillPicker lists the installed skills for browse-and-run: type to filter,
// arrow keys to select, Enter runs the chosen skill immediately. Selection is by
// the skill's EXACT name (item.Value), so even skills the slash path cannot
// reach — names shadowed by a builtin or user command, or names that don't fit
// a slash token — are runnable here: picking from the Skills picker is
// unambiguous. Returns nil when no skills are installed (the caller falls back
// to the install-hint card).
func (m model) newSkillPicker() *commandPicker {
	installed := m.installedSkills()
	items := make([]pickerItem, 0, len(installed))
	for _, skill := range installed {
		name := strings.TrimSpace(skill.Name)
		if name == "" {
			continue
		}
		label := name
		if slash := skillSlashName(name); slash != "" {
			// Show the slash form where one exists — it doubles as documentation
			// of the direct "/name [args]" invocation.
			label = "/" + slash
		}
		items = append(items, pickerItem{Label: label, Value: name, Meta: strings.TrimSpace(skill.Description)})
	}
	if len(items) == 0 {
		return nil
	}
	return &commandPicker{kind: pickerSkill, title: "pick a skill · ⏎ fills /name — add your request", items: items, allItems: append([]pickerItem{}, items...), selected: 0}
}

// modelPickerProviders returns the providers to list in /model: all saved
// providers, falling back to the active profile when none were threaded in.
func (m model) modelPickerProviders() []config.ProviderProfile {
	if len(m.savedProviders) > 0 {
		return m.savedProviders
	}
	if config.HasProviderProfile(m.providerProfile) {
		return []config.ProviderProfile{m.providerProfile}
	}
	return nil
}

// savedProviderModelPickerItems lists one saved provider's models as a group,
// tagging each with the owning provider so selection can switch providers. The
// active provider prefers its live-discovered models when available.
func (m model) savedProviderModelPickerItems(profile config.ProviderProfile, activeProvider, activeModel string) []pickerItem {
	providerName := strings.TrimSpace(profile.Name)
	isActive := providerName != "" && strings.EqualFold(providerName, activeProvider)
	descriptor, hasDescriptor := m.descriptorForProfile(profile)
	group := modelPickerProviderGroup(profile, descriptor, hasDescriptor)

	var raw []pickerItem
	switch {
	case hasDescriptor && len(m.modelPickerLiveByProvider[descriptor.ID]) > 0:
		for _, model := range m.modelPickerLiveByProvider[descriptor.ID] {
			if strings.TrimSpace(model.ID) == "" {
				continue
			}
			raw = append(raw, discoveredModelPickerItem(descriptor, model, group))
		}
	case hasDescriptor && descriptor.Local:
		// Local providers (e.g. Ollama) only have the models you've actually pulled.
		// Never fall back to the static catalog — that would list models you don't
		// have. Until live discovery returns them, this provider shows no rows.
	case hasDescriptor:
		for _, model := range providermodelcatalog.Models(descriptor) {
			if strings.TrimSpace(model.ID) == "" {
				continue
			}
			raw = append(raw, providerModelPickerItem(descriptor, model, group))
		}
	default:
		// Custom provider without a catalog: surface its single configured model.
		if mdl := strings.TrimSpace(profile.Model); mdl != "" {
			raw = append(raw, pickerItem{Label: modelPickerDisplayName(mdl, ""), Value: mdl})
		}
	}

	out := make([]pickerItem, 0, len(raw))
	for _, item := range raw {
		// The active model is already shown under "Recent" for the active provider.
		if isActive && item.Value == activeModel {
			continue
		}
		item.Group = group
		item.OwnerProvider = providerName
		out = append(out, item)
	}
	return out
}

// descriptorForProfile resolves a profile's catalog descriptor the same way the
// active provider is resolved — by CatalogID, then base URL, then a synthesized
// custom descriptor, then name/kind candidates — so every saved provider (not just
// the active one) lists its models.
func (m model) descriptorForProfile(profile config.ProviderProfile) (providercatalog.Descriptor, bool) {
	if descriptor, ok := providercatalog.Get(profile.CatalogID); ok && !genericProviderCatalogID(descriptor.ID) {
		return descriptor, true
	}
	if descriptor, ok := providerDescriptorByBaseURL(profile.BaseURL); ok {
		return descriptor, true
	}
	if descriptor, ok := customProviderDescriptorForProfile(profile); ok {
		return descriptor, true
	}
	for _, candidate := range []string{profile.Name, profile.Provider, string(profile.ProviderKind)} {
		if descriptor, ok := providercatalog.Get(candidate); ok {
			return descriptor, true
		}
	}
	return providercatalog.Descriptor{}, false
}

func modelPickerProviderGroup(profile config.ProviderProfile, descriptor providercatalog.Descriptor, hasDescriptor bool) string {
	if hasDescriptor && strings.TrimSpace(descriptor.Name) != "" {
		return descriptor.Name
	}
	if name := strings.TrimSpace(profile.Name); name != "" {
		return name
	}
	return "Provider"
}

func (m model) openModelPicker() (model, tea.Cmd) {
	picker := m.newModelPicker()
	if picker == nil {
		return m, nil
	}
	m.picker = picker
	m.clearModelPickerLoadState()
	// Live-discover every usable provider's real models in the background. The list
	// shows immediately from the static catalog and each provider's section refreshes
	// as its discovery returns — no blocking overlay.
	return m, m.modelPickerDiscoveryCmds()
}

// modelPickerDiscoveryCmds dispatches a live model-discovery command for each
// usable provider (deduped by catalog descriptor), so /model shows the same real
// models the provider-setup wizard discovers.
func (m model) modelPickerDiscoveryCmds() tea.Cmd {
	cmds := []tea.Cmd{}
	seen := map[string]bool{}
	for _, profile := range m.modelPickerProviders() {
		descriptor, ok := m.descriptorForProfile(profile)
		if !ok || seen[descriptor.ID] {
			continue
		}
		seen[descriptor.ID] = true
		if cmd := m.modelPickerProviderDiscoveryCmd(descriptor, profile); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// modelPickerProviderDiscoveryCmd discovers one provider's live models, resolving
// its credential the same way the wizard does: a stored/inline/env key, or a stored
// OAuth bearer for token-login providers (e.g. xAI).
func (m model) modelPickerProviderDiscoveryCmd(descriptor providercatalog.Descriptor, profile config.ProviderProfile) tea.Cmd {
	authed := profile
	if store, err := m.providerKeyStore(); err == nil {
		authed = config.ApplyStoredAPIKey(authed, store)
	}
	key := strings.TrimSpace(authed.APIKey)
	if key == "" && strings.TrimSpace(authed.APIKeyEnv) != "" {
		key = strings.TrimSpace(os.Getenv(authed.APIKeyEnv))
	}
	needOAuth := key == "" && descriptor.OAuth && !descriptor.OAuthMintsKey
	discover := m.discoverProviderModels
	if discover == nil {
		discoveryOptions := m.modelPickerDiscoveryOptions(authed)
		discover = func(ctx context.Context, p config.ProviderProfile) ([]providermodeldiscovery.Model, error) {
			return providermodeldiscovery.DiscoverCatalog(ctx, descriptor, p, discoveryOptions)
		}
	}
	providerID := descriptor.ID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 8*time.Second)
		defer cancel()
		k := key
		if needOAuth {
			if resolved := oauthStoredToken(ctx, providerID); resolved != "" {
				k = resolved
			}
		}
		models, err := discover(ctx, providerWizardDiscoveryProfile(descriptor, k, authed.BaseURL))
		return modelPickerModelsDiscoveredMsg{providerID: providerID, models: models, err: err}
	}
}

func (m model) modelPickerDiscoveryOptions(profile config.ProviderProfile) providermodeldiscovery.Options {
	oauthResolver, loginKey := providers.OAuthLoginForProfile(profile)
	return providermodeldiscovery.Options{
		OAuthResolver:        oauthResolver,
		CodexAccountResolver: providers.CodexAccountResolverForLogin(loginKey),
		UserAgent:            m.userAgent,
	}
}

func (m model) modelPickerIsLoading() bool {
	return m.picker != nil && m.picker.kind == pickerModel && m.modelPickerLoading
}

func (m *model) clearModelPickerLoadState() {
	m.modelPickerLoading = false
	m.modelPickerLoadingProviderID = ""
	m.modelPickerLoadError = ""
}

// pickerItemDedupKey identifies a picker row for cross-group de-duplication.
// Rows tagged with an owning provider are keyed by provider+model, not model
// id alone — the same model id can be offered by multiple providers (e.g. a
// model name available from both a direct provider and an OpenAI-compatible
// gateway), and those are distinct, independently selectable rows.
func pickerItemDedupKey(item pickerItem) string {
	if owner := strings.TrimSpace(item.OwnerProvider); owner != "" {
		return strings.ToLower(owner) + "\x00" + item.Value
	}
	return item.Value
}

func (m model) assembleModelPickerItems(recent []pickerItem, catalog []pickerItem) []pickerItem {
	result := []pickerItem{}
	// Favorites keep the pre-provider-aware semantics: one row per favorited
	// model ID, regardless of how many providers offer it. Track seen favorite
	// model IDs by Value alone so a model favorited once doesn't surface twice
	// just because it's in multiple provider catalogs.
	favoriteSeen := map[string]bool{}
	addFavorites := func(items []pickerItem) {
		for _, item := range items {
			if item.Value == "" || !m.favoriteModels[item.Value] || favoriteSeen[item.Value] {
				continue
			}
			item.Group = "Favorites"
			item.Favorite = true
			result = append(result, item)
			favoriteSeen[item.Value] = true
		}
	}
	addFavorites(recent)
	addFavorites(catalog)
	// Recent and Catalog use the provider-aware de-dup key so the same model ID
	// offered by different providers shows as distinct, independently selectable
	// rows. They still skip any model ID already surfaced under Favorites, so a
	// favorited model doesn't appear in a second group (preserving the prior
	// "favorited models are hidden from the rest" behavior).
	seen := map[string]bool{}
	for _, item := range recent {
		if item.Value == "" || favoriteSeen[item.Value] {
			continue
		}
		key := pickerItemDedupKey(item)
		if seen[key] {
			continue
		}
		item.Group = "Recent"
		item.Favorite = m.favoriteModels[item.Value]
		result = append(result, item)
		seen[key] = true
	}
	for _, item := range catalog {
		if item.Value == "" || favoriteSeen[item.Value] {
			continue
		}
		key := pickerItemDedupKey(item)
		if seen[key] {
			continue
		}
		item.Favorite = m.favoriteModels[item.Value]
		result = append(result, item)
		seen[key] = true
	}
	return result
}

// recentModelPairsForPicker returns the provider+model pairs to show under
// "Recent", newest first: the active provider/model is always pinned first
// (even before it has been recorded to history), followed by persisted
// history, de-duplicated by provider+model pair and capped to
// config.MaxRecentModels.
func (m model) recentModelPairsForPicker() []config.RecentModelEntry {
	pairs := make([]config.RecentModelEntry, 0, len(m.recentModels)+1)
	pairs = append(pairs, config.RecentModelEntry{Provider: m.providerName, Model: m.modelName})
	pairs = append(pairs, m.recentModels...)
	for index := range pairs {
		pairs[index] = m.canonicalRecentModelPair(pairs[index])
	}
	return config.NormalizeRecentModels(pairs)
}

// canonicalRecentModelPair collapses legacy OpenAI-prefixed model IDs for a
// ChatGPT profile. ChatGPT's Codex endpoint uses bare model IDs, while older
// hand-entered configuration could retain the equivalent openai/<id> spelling.
// Keep this picker-only so OpenAI-compatible gateway IDs remain untouched.
func (m model) canonicalRecentModelPair(pair config.RecentModelEntry) config.RecentModelEntry {
	pair.Provider = strings.TrimSpace(pair.Provider)
	pair.Model = strings.TrimSpace(pair.Model)
	if !m.recentPairUsesChatGPT(pair.Provider) {
		return pair
	}
	if len(pair.Model) >= len("openai/") && strings.EqualFold(pair.Model[:len("openai/")], "openai/") {
		pair.Model = strings.TrimSpace(pair.Model[len("openai/"):])
	}
	return pair
}

func (m model) recentPairUsesChatGPT(providerName string) bool {
	if profile, ok := m.savedProviderByName(providerName); ok {
		if descriptor, hasDescriptor := m.descriptorForProfile(profile); hasDescriptor {
			return providercatalog.NormalizeID(descriptor.ID) == "chatgpt"
		}
	}
	if !strings.EqualFold(strings.TrimSpace(providerName), strings.TrimSpace(m.providerName)) {
		return false
	}
	if descriptor, ok := m.activeProviderDescriptor(); ok {
		return providercatalog.NormalizeID(descriptor.ID) == "chatgpt"
	}
	return providercatalog.NormalizeID(m.providerProfile.CatalogID) == "chatgpt"
}

// modelPickerRecentItem resolves one "Recent" row for a provider+model pair,
// tagging it with OwnerProvider so choosing it can switch providers (like any
// other cross-provider picker row) even when it isn't the active provider.
func (m model) modelPickerRecentItem(registry modelregistry.Registry, providerName, modelID string) pickerItem {
	providerName = strings.TrimSpace(providerName)
	if entry, ok := registry.Resolve(modelID); ok {
		item := registryModelPickerItem(entry, "Recent")
		item.Value = modelID
		item.OwnerProvider = providerName
		return item
	}
	if profile, ok := m.savedProviderByName(providerName); ok {
		if descriptor, hasDescriptor := m.descriptorForProfile(profile); hasDescriptor {
			for _, model := range providermodelcatalog.Models(descriptor) {
				if model.ID == modelID {
					item := providerModelPickerItem(descriptor, model, "Recent")
					item.Value = modelID
					item.OwnerProvider = profile.Name
					return item
				}
			}
			item := providerModelPickerItem(descriptor, providermodelcatalog.Model{ID: modelID}, "Recent")
			item.Value = modelID
			item.OwnerProvider = profile.Name
			return item
		}
	}
	return pickerItem{Group: "Recent", Label: modelPickerDisplayName(modelID, ""), Value: modelID, OwnerProvider: providerName}
}

func registryModelPickerItem(entry modelregistry.ModelEntry, group string) pickerItem {
	item := pickerItem{
		Group: group,
		Label: firstProviderDisplayValue(entry.DisplayName, entry.ID),
		Value: entry.ID,
		// OwnerProvider defaults to the registry entry's canonical provider so
		// cross-group de-dup (pickerItemDedupKey) can distinguish the same model
		// id offered by different providers even in the plain-registry fallback
		// path (no saved providers resolved any models). Callers that resolve a
		// specific provider profile (e.g. modelPickerRecentItem) overwrite this
		// with the profile name right after calling this constructor.
		OwnerProvider: string(entry.Provider),
	}
	item.Meta = registryModelPickerMeta(entry)
	if descriptor, ok := providercatalog.Get(string(entry.Provider)); ok {
		applyProviderPickerMeta(&item, descriptor)
	}
	return item
}

func providerModelPickerItem(provider providercatalog.Descriptor, model providermodelcatalog.Model, group string) pickerItem {
	item := pickerItem{
		Group: group,
		Label: modelPickerDisplayName(model.ID, model.Description),
		Value: model.ID,
	}
	item.Meta = providerWizardModelMeta(model.ContextWindow, model.ToolCall, model.Reasoning, model.InputCost, model.OutputCost, model.Tags)
	applyProviderPickerMeta(&item, provider)
	return item
}

func discoveredModelPickerItem(provider providercatalog.Descriptor, model providermodeldiscovery.Model, group string) pickerItem {
	item := pickerItem{
		Group: group,
		Label: modelPickerDisplayName(model.ID, model.Description),
		Value: model.ID,
	}
	item.Meta = providerWizardModelMeta(model.ContextWindow, model.ToolCall, model.Reasoning, model.InputCost, model.OutputCost, model.Tags)
	applyProviderPickerMeta(&item, provider)
	return item
}

func applyProviderPickerMeta(item *pickerItem, provider providercatalog.Descriptor) {
	item.Remote = !provider.Local
	item.Local = provider.Local
	item.Provider = providerPickerTag(provider)
}

// providerPickerTag is the short, lowercase provider slug shown right-aligned on
// each model row (e.g. "anthropic", "deepseek", "ollama"). Prefers the catalog
// ID since it is already the stable lowercase identifier; falls back to a
// lowercased display name for descriptors that only carry one.
func providerPickerTag(provider providercatalog.Descriptor) string {
	if id := strings.TrimSpace(provider.ID); id != "" {
		return id
	}
	return strings.ToLower(strings.TrimSpace(provider.Name))
}

func registryModelPickerMeta(entry modelregistry.ModelEntry) string {
	parts := []string{}
	if ctx := formatContextWindow(entry.ContextLimits.ContextWindow); ctx != "" {
		parts = append(parts, ctx+" ctx")
	}
	if entry.Supports(modelregistry.ModelCapabilityToolCalling) {
		parts = append(parts, "tools")
	}
	if entry.Supports(modelregistry.ModelCapabilityReasoning) {
		parts = append(parts, "reasoning")
	}
	if entry.Supports(modelregistry.ModelCapabilityVision) {
		parts = append(parts, "vision")
	}
	return strings.Join(parts, " · ")
}

func modelPickerDisplayName(id string, description string) string {
	// The row title is the model NAME, not its marketing blurb. Catalog/discovery
	// descriptions (from models.dev) are full sentences, and several models share
	// the same one — using them as titles showed sentence-long rows that looked like
	// exact duplicates and hid the actual id (which only survived in the meta line).
	// Prefer the (prettified) id; fall back to a non-generic description only when
	// there is no id to name the row.
	id = strings.TrimSpace(id)
	if id == "" {
		if description = strings.TrimSpace(description); description != "" && !providerWizardGenericModelDescription(description) {
			return description
		}
		return "model"
	}
	name := id
	if slash := strings.LastIndex(name, "/"); slash >= 0 && slash < len(name)-1 {
		name = name[slash+1:]
	}
	name = strings.NewReplacer("-", " ", "_", " ", ":", " ").Replace(name)
	words := strings.Fields(name)
	for index, word := range words {
		words[index] = modelPickerTitleWord(word)
	}
	return strings.Join(words, " ")
}

func modelPickerTitleWord(word string) string {
	if word == "" {
		return ""
	}
	lower := strings.ToLower(word)
	switch lower {
	case "api", "gpt", "glm", "vl":
		return strings.ToUpper(lower)
	default:
		if strings.HasPrefix(lower, "gpt") || strings.HasPrefix(lower, "glm") {
			return strings.ToUpper(lower[:3]) + word[3:]
		}
		return strings.ToUpper(word[:1]) + word[1:]
	}
}

func (m model) activeProviderDescriptor() (providercatalog.Descriptor, bool) {
	return m.descriptorForProfile(m.providerProfile)
}

func customProviderDescriptorForProfile(profile config.ProviderProfile) (providercatalog.Descriptor, bool) {
	if descriptor, ok := providercatalog.Get(profile.CatalogID); ok && genericProviderCatalogID(descriptor.ID) {
		return descriptor, true
	}

	baseURL := strings.TrimSpace(profile.BaseURL)
	if baseURL == "" {
		return providercatalog.Descriptor{}, false
	}
	switch profileProviderKind(profile) {
	case config.ProviderKindOpenAICompatible:
		if !sameNormalizedProviderBaseURL(baseURL, config.OpenAIBaseURL) {
			return providercatalog.Get("custom-openai-compatible")
		}
	case config.ProviderKindAnthropicCompat:
		if !sameNormalizedProviderBaseURL(baseURL, config.AnthropicBaseURL) {
			return providercatalog.Get("custom-anthropic-compatible")
		}
	case config.ProviderKindOpenAI:
		if !sameNormalizedProviderBaseURL(baseURL, config.OpenAIBaseURL) {
			return providercatalog.Get("custom-openai-compatible")
		}
	case config.ProviderKindAnthropic:
		if !sameNormalizedProviderBaseURL(baseURL, config.AnthropicBaseURL) {
			return providercatalog.Get("custom-anthropic-compatible")
		}
	}
	return providercatalog.Descriptor{}, false
}

func profileProviderKind(profile config.ProviderProfile) config.ProviderKind {
	if kind := strings.TrimSpace(string(profile.ProviderKind)); kind != "" {
		return config.ProviderKind(strings.ToLower(kind))
	}
	if provider := strings.TrimSpace(profile.Provider); provider != "" {
		return config.ProviderKind(strings.ToLower(provider))
	}
	return ""
}

func providerDescriptorByBaseURL(baseURL string) (providercatalog.Descriptor, bool) {
	normalized := normalizeProviderBaseURL(baseURL)
	if normalized == "" {
		return providercatalog.Descriptor{}, false
	}
	for _, descriptor := range providercatalog.All() {
		if genericProviderCatalogID(descriptor.ID) {
			continue
		}
		if normalizeProviderBaseURL(descriptor.DefaultBaseURL) == normalized {
			return descriptor, true
		}
	}
	return providercatalog.Descriptor{}, false
}

func normalizeProviderBaseURL(baseURL string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(baseURL)), "/")
}

func sameNormalizedProviderBaseURL(left string, right string) bool {
	return normalizeProviderBaseURL(left) == normalizeProviderBaseURL(right)
}

func genericProviderCatalogID(id string) bool {
	return strings.HasPrefix(strings.TrimSpace(id), "custom-")
}

type modelPickerModelsDiscoveredMsg struct {
	providerID string
	models     []providermodeldiscovery.Model
	err        error
}

// ollamaContextWindowDiscoveredMsg carries the result of an async /api/show
// probe against a local Ollama daemon (see ollamaContextWindowDiscoveryCmd).
// A zero contextWindow (or a non-nil err) means the probe found nothing
// usable and the map is left untouched, not zeroed out.
type ollamaContextWindowDiscoveredMsg struct {
	modelName     string
	contextWindow int
	err           error
}

// ollamaContextWindowDiscoveryCmd probes a local Ollama daemon's native
// /api/show endpoint for modelName's context length. Scoped to the local
// Ollama provider only (catalog ID "ollama", not "ollama-cloud" — a
// different hosted service this endpoint isn't assumed to exist on): the
// generic /v1/models discovery has no source for this metadata for
// custom/local model tags, so without this the context gauge just never
// shows for them (see modelContextWindow).
func (m model) ollamaContextWindowDiscoveryCmd(descriptor providercatalog.Descriptor, baseURL string, modelName string) tea.Cmd {
	if descriptor.ID != "ollama" {
		return nil
	}
	modelName = strings.TrimSpace(modelName)
	baseURL = strings.TrimSpace(baseURL)
	if modelName == "" || baseURL == "" {
		return nil
	}
	discover := m.discoverOllamaContextWindow
	if discover == nil {
		discover = func(ctx context.Context, baseURL string, model string) (int, error) {
			return providermodeldiscovery.DiscoverOllamaContextWindow(ctx, baseURL, model, providermodeldiscovery.Options{})
		}
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 8*time.Second)
		defer cancel()
		window, err := discover(ctx, baseURL, modelName)
		return ollamaContextWindowDiscoveredMsg{modelName: modelName, contextWindow: window, err: err}
	}
}

func (m model) normalizeProfileForProvider(provider providercatalog.Descriptor) config.ProviderProfile {
	profile := m.providerProfile
	normalizeIdentity := profileMatchesProviderBaseURL(profile, provider) ||
		genericProviderCatalogID(profile.Name) ||
		genericProviderCatalogID(profile.CatalogID) ||
		strings.TrimSpace(profile.Name) == "" ||
		strings.TrimSpace(profile.CatalogID) == ""
	if normalizeIdentity {
		// Only canonicalize an empty or generic placeholder name — never clobber a
		// real user-provided profile name. The credential store is keyed by
		// profile.Name (SecureProviderProfile stores under Name), so rewriting it
		// here made the later profileWithCredential lookup miss the saved key and
		// rebuild a keyless provider — a 401 on every /model switch for any profile
		// named differently from its catalog id (e.g. "my-openai").
		if strings.TrimSpace(profile.Name) == "" || genericProviderCatalogID(profile.Name) {
			profile.Name = provider.ID
		}
		profile.CatalogID = provider.ID
	}
	if strings.TrimSpace(profile.BaseURL) == "" {
		profile.BaseURL = provider.DefaultBaseURL
	}
	if strings.TrimSpace(string(profile.ProviderKind)) == "" {
		profile.ProviderKind = providerWizardProviderKind(provider)
	}
	if strings.TrimSpace(profile.APIFormat) == "" {
		profile.APIFormat = providerWizardAPIFormat(provider)
	}
	if len(provider.AuthEnvVars) > 0 && (strings.TrimSpace(profile.APIKeyEnv) == "" || normalizeIdentity) {
		profile.APIKeyEnv = provider.AuthEnvVars[0]
	}
	if strings.TrimSpace(profile.APIKey) == "" && strings.TrimSpace(profile.APIKeyEnv) != "" {
		profile.APIKey = strings.TrimSpace(os.Getenv(profile.APIKeyEnv))
	}
	return profile
}

func profileMatchesProviderBaseURL(profile config.ProviderProfile, provider providercatalog.Descriptor) bool {
	baseURL := normalizeProviderBaseURL(profile.BaseURL)
	return baseURL != "" && baseURL == normalizeProviderBaseURL(provider.DefaultBaseURL)
}

func (m model) applyModelPickerModelsDiscovered(msg modelPickerModelsDiscoveredMsg) model {
	m.modelPickerLoading = false
	m.modelPickerLoadingProviderID = ""
	if msg.err != nil || len(msg.models) == 0 {
		return m
	}
	if m.modelPickerLiveByProvider == nil {
		m.modelPickerLiveByProvider = map[string][]providermodeldiscovery.Model{}
	}
	m.modelPickerLiveByProvider[msg.providerID] = append([]providermodeldiscovery.Model{}, msg.models...)
	// Rebuild the open picker so this provider's section shows its live models,
	// preserving the current query + selection.
	if m.picker != nil && m.picker.kind == pickerModel {
		selectedValue := ""
		query := m.picker.query
		if item, ok := m.picker.current(); ok {
			selectedValue = item.Value
		}
		m.picker = m.newModelPicker()
		if m.picker != nil {
			m.picker.query = query
			m.picker.applyQuery()
			m.selectPickerValue(selectedValue)
		}
	}
	return m
}

func (m model) toggleModelFavorite() model {
	if m.picker == nil || m.picker.kind != pickerModel {
		return m
	}
	item, ok := m.picker.current()
	if !ok || strings.TrimSpace(item.Value) == "" {
		return m
	}
	if m.favoriteModels == nil {
		m.favoriteModels = map[string]bool{}
	}
	if m.favoriteModels[item.Value] {
		delete(m.favoriteModels, item.Value)
	} else {
		m.favoriteModels[item.Value] = true
	}
	if err := m.persistFavoriteModels(); err != nil {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendError, text: "favorite save error: " + err.Error()})
	}
	selectedValue := item.Value
	query := m.picker.query
	m.picker = m.newModelPicker()
	m.picker.query = query
	m.picker.applyQuery()
	m.selectPickerValue(selectedValue)
	return m
}

func (m model) persistFavoriteModels() error {
	if strings.TrimSpace(m.userConfigPath) == "" {
		return nil
	}
	_, err := config.SetFavoriteModels(m.userConfigPath, favoriteModelValues(m.favoriteModels))
	return err
}

// recordRecentModels prepends the given provider+model pairs (in order, so the
// last pair ends up frontmost) to the in-memory recent-selection history,
// de-duplicates by provider+model pair (moving a re-selected pair back to the
// front rather than leaving a stale older copy), caps the result, and persists
// it in a single normalize+write regardless of how many pairs are given. Model
// switches record both the outgoing and incoming pair for one logical switch —
// otherwise the model a session started on would silently drop out of "Recent"
// the moment you switch away from it once — and batching avoids two
// synchronous read-modify-write disk operations for a single user action.
// Persistence is skipped when there is no user config path, but the in-memory
// history is still updated so the picker reflects the selection for the rest
// of the session. Returns the receiver unchanged (no re-normalization, no
// write) when every pair has a blank model id.
func (m model) recordRecentModels(pairs ...config.RecentModelEntry) model {
	m, changed := m.updateRecentModels(pairs...)
	if !changed {
		return m
	}
	if path := strings.TrimSpace(m.userConfigPath); path != "" {
		if _, err := config.SetRecentModels(path, m.recentModels); err != nil {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendError, text: "recent model save error: " + err.Error()})
		}
	}
	return m
}

// updateRecentModels performs recordRecentModels' in-memory half. A model
// switch whose selection transaction failed still belongs in this session's
// picker history, but must not make another blocking persistence attempt while
// the same config lock is unavailable.
func (m model) updateRecentModels(pairs ...config.RecentModelEntry) (model, bool) {
	entries := append([]config.RecentModelEntry{}, m.recentModels...)
	changed := false
	for _, pair := range pairs {
		modelID := strings.TrimSpace(pair.Model)
		if modelID == "" {
			continue
		}
		changed = true
		entries = append([]config.RecentModelEntry{{Provider: strings.TrimSpace(pair.Provider), Model: modelID}}, entries...)
	}
	if !changed {
		return m, false
	}
	m.recentModels = normalizeRecentModelEntries(entries)
	return m, true
}

// normalizeRecentModelEntries trims, drops entries with no model id,
// de-duplicates by provider+model pair (keeping the first/newest occurrence),
// and caps the result to config.MaxRecentModels. Delegates to
// config.NormalizeRecentModels so options loaded outside of config.Resolve
// (e.g. tests constructing Options directly) can never drift from the
// persisted-config normalization rules.
func normalizeRecentModelEntries(entries []config.RecentModelEntry) []config.RecentModelEntry {
	return config.NormalizeRecentModels(entries)
}

func favoriteModelSet(models []string) map[string]bool {
	if len(models) == 0 {
		return nil
	}
	favorites := map[string]bool{}
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		favorites[model] = true
	}
	if len(favorites) == 0 {
		return nil
	}
	return favorites
}

func favoriteModelValues(favorites map[string]bool) []string {
	values := make([]string, 0, len(favorites))
	for model, favorite := range favorites {
		model = strings.TrimSpace(model)
		if !favorite || model == "" {
			continue
		}
		values = append(values, model)
	}
	sort.Strings(values)
	return values
}

func (m *model) selectPickerValue(value string) {
	if m.picker == nil || value == "" {
		return
	}
	for index, item := range m.picker.items {
		if item.Value == value {
			m.picker.selected = index
			return
		}
	}
}

// newEffortPicker lists the reasoning efforts the active model supports plus an
// "auto" option, preselecting the current preference. When the model exposes no
// effort controls, still returns a single "auto" picker so the user gets the
// popup affordance on /effort instead of a static status card; handleEffortCommand
// reports "Active model does not expose reasoning effort controls" if they pick
// anything other than auto.
func (m model) newEffortPicker() *commandPicker {
	efforts := m.availableReasoningEfforts()
	items := []pickerItem{{Label: "auto", Value: "auto"}}
	selected := 0
	if m.reasoningEffort == "" {
		selected = 0
	}
	for _, effort := range efforts {
		items = append(items, pickerItem{Label: string(effort), Value: string(effort)})
		if m.reasoningEffort != "" && effort == m.reasoningEffort {
			selected = len(items) - 1
		}
	}
	return &commandPicker{kind: pickerEffort, title: "select reasoning effort", items: items, selected: selected}
}

// newThemePicker lists the terminal-native system theme followed by named palettes
// in one flat list. Old dark/light preferences migrate to System rather than
// appearing as choices: they would imply a terminal-canvas change that Zero
// deliberately does not make.
func (m model) newThemePicker() *commandPicker {
	items := make([]pickerItem, 0, len(themeModes))
	selected := 0
	for _, name := range themeModes {
		item := pickerItem{Label: name, Value: name}
		if name == string(themeSystem) {
			item.Label = "System"
		} else if entry, ok := lookupTheme(name); ok {
			item.Label = entry.Label
		}
		items = append(items, item)
		if name == string(m.themeMode) {
			selected = len(items) - 1
		}
	}
	// allItems lets the query filter restore rows on Backspace (one-way narrowing
	// otherwise, since applyQuery falls back to the current items without it).
	return &commandPicker{kind: pickerTheme, title: "Choose a theme", items: items, allItems: append([]pickerItem{}, items...), selected: selected}
}

// pickerMoved advances the open picker's cursor by delta. Theme candidates render
// only in the picker preview; their active palette is applied only after Enter.
// Safe to call with no picker open. Callers mutate through m.picker (a pointer),
// so the value receiver is fine.
func (m model) pickerMoved(delta int) (model, tea.Cmd) {
	if m.picker == nil {
		return m, nil
	}
	m.picker.move(delta)
	if m.picker.kind == pickerPet {
		return m.schedulePetPreview()
	}
	return m, nil
}
