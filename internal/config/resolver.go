package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/Gitlawb/zero/internal/aimlapi"
	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/notify"
	"github.com/Gitlawb/zero/internal/providercatalog"
	"github.com/Gitlawb/zero/internal/sandbox"
)

// ErrNoActiveProvider marks a resolve failure caused solely by a missing or
// unresolvable active provider. The interactive TUI treats this as "needs
// onboarding" (drop into the setup wizard) rather than a fatal config error.
var ErrNoActiveProvider = errors.New("no active provider configured")

// ErrProviderRequiresModel marks a resolve failure caused solely by the active
// provider missing a model with no catalog default to fall back on (custom
// openai-/anthropic-compatible endpoints — Zero cannot guess a gateway's model).
// Like ErrNoActiveProvider, the interactive TUI treats it as "needs onboarding"
// and drops into the setup wizard so the user can fix it; headless commands
// (zero config, zero exec) still fail with the actionable message.
var ErrProviderRequiresModel = errors.New("provider requires model")

// setupFixableError tags an error with a sentinel for errors.Is WITHOUT
// changing its message: the sentinel's own text must not prefix what the user
// sees (the wrapped message is already complete and actionable).
type setupFixableError struct {
	err      error
	sentinel error
}

func (e *setupFixableError) Error() string { return e.err.Error() }

func (e *setupFixableError) Unwrap() []error { return []error{e.err, e.sentinel} }

// defaultMaxTurns is the per-run tool-turn budget when none is configured. 30 was
// too low for real multi-step agentic work (agents ran out mid-task before reaching
// later steps); 50 was still too low for larger tasks spanning several files (agents
// hit the ceiling and stopped with a "remaining work" summary instead of finishing).
// Raise per-session with /turns.
const defaultMaxTurns = 80

// MaxTurnsCeiling caps the per-run tool-turn budget so a stray env value or typo
// can't set an absurd ceiling. Shared between applyEnv (read site) and the /turns
// command (write site) so the bound holds even if the env is set by a raw shell.
const MaxTurnsCeiling = 500

// defaultDeferThreshold is the number of deferred-eligible (MCP) tools at which
// Zero collapses their full JSON schemas into compact `tool_search` reminder
// lines instead of advertising every schema on every turn. MCP tool schemas run
// 300-600 tokens each, so eagerly shipping even a small server's toolset wastes
// thousands of input tokens per message. Kept low so a typical single-server set
// (Exa-style, a handful of tools) defers and stays cheap; the only cost is one
// `tool_search` round-trip before a deferred tool's first use, after which it is
// loaded for the rest of the run. Override per-config with tools.deferThreshold
// (set 0 to always advertise every schema, e.g. for a model without tool_search).
const defaultDeferThreshold = 3

func Resolve(options ResolveOptions) (ResolvedConfig, error) {
	cfg := FileConfig{
		MaxTurns: defaultMaxTurns,
	}

	if options.UserConfigPath != "" {
		fileConfig, err := loadConfigFile(options.UserConfigPath)
		if err != nil {
			return ResolvedConfig{}, err
		}
		mergeConfig(&cfg, fileConfig)
	}
	if options.ProjectConfigPath != "" {
		fileConfig, err := loadConfigFile(options.ProjectConfigPath)
		if err != nil {
			return ResolvedConfig{}, err
		}
		if err := mergeProjectConfig(&cfg, fileConfig); err != nil {
			return ResolvedConfig{}, err
		}
	}

	applyEnv(&cfg, options.Env)

	if options.ProviderCommand != "" {
		commandConfig, err := LoadProviderCommand(options.ProviderCommand)
		if err != nil {
			return ResolvedConfig{}, err
		}
		// Sandbox.Enabled is NOT accepted from a provider command, for the same
		// reason project config cannot set it (see mergeProjectConfig): only
		// global config and the CLI may turn the sandbox off. A provider command
		// is an arbitrary executable named in config, and LoadProviderCommand
		// parses its stdout into a full FileConfig — so without this, a command
		// returning a valid provider plus {"sandbox":{"enabled":false}} would
		// disable the very sandbox meant to constrain what it can do. Cleared
		// here rather than in mergeConfig because that helper is shared with the
		// trusted global-config merge, which must keep honouring the setting.
		commandConfig.Sandbox.Enabled = nil
		commandConfig.CrossSessionInbound = ""
		mergeConfig(&cfg, commandConfig)
	}

	applyOverrides(&cfg, options.Overrides)

	if !cfg.Tools.deferThresholdSet && cfg.Tools.DeferThreshold == 0 {
		cfg.Tools.DeferThreshold = defaultDeferThreshold
	}
	if cfg.Tools.DeferThreshold < 0 {
		return ResolvedConfig{}, fmt.Errorf("invalid tools.deferThreshold %d: must be >= 0", cfg.Tools.DeferThreshold)
	}

	if cfg.Swarm.MaxTeamSize < 0 {
		return ResolvedConfig{}, fmt.Errorf("invalid swarm.maxTeamSize %d: must be >= 0 (0 uses the default)", cfg.Swarm.MaxTeamSize)
	}

	if network := strings.TrimSpace(cfg.Sandbox.Network); network != "" {
		switch sandbox.NetworkMode(network) {
		case sandbox.NetworkAllow, sandbox.NetworkDeny:
		default:
			return ResolvedConfig{}, fmt.Errorf("invalid sandbox.network %q: expected allow or deny", network)
		}
	}
	if mode := strings.TrimSpace(cfg.Notify.Mode); mode != "" {
		switch notify.Mode(mode) {
		case notify.ModeOff, notify.ModeBell, notify.ModeNotify, notify.ModeBoth:
		default:
			return ResolvedConfig{}, fmt.Errorf("invalid notify.mode %q: expected off, bell, notify, or both", mode)
		}
	}
	if focusMode := strings.TrimSpace(cfg.Notify.FocusMode); focusMode != "" {
		switch notify.FocusMode(focusMode) {
		case notify.FocusUnfocused, notify.FocusAlways, notify.FocusFocused:
		default:
			return ResolvedConfig{}, fmt.Errorf("invalid notify.focusMode %q: expected unfocused, always, or focused", focusMode)
		}
	}
	if inbound := strings.TrimSpace(cfg.CrossSessionInbound); inbound != "" {
		switch inbound {
		case "accept", "hold", "refuse":
		default:
			return ResolvedConfig{}, fmt.Errorf("invalid crossSessionInbound %q: expected accept, hold, or refuse", inbound)
		}
	}
	if err := validateSTTConfig(cfg.STT); err != nil {
		return ResolvedConfig{}, err
	}

	providers, active, err := normalizeProviders(cfg.Providers, cfg.ActiveProvider, options.Env)
	if err != nil {
		// On ErrNoActiveProvider, providers may still hold the successfully
		// normalized (but active-less) profile list — keep it so a caller can fall
		// back to an already-configured usable provider instead of treating this
		// like a config with nothing set up at all.
		return ResolvedConfig{Providers: providers}, err
	}

	return ResolvedConfig{
		ActiveProvider:      active.Name,
		Providers:           providers,
		Provider:            active,
		MaxTurns:            cfg.MaxTurns,
		MaxTurnsSet:         cfg.maxTurnsSet,
		MCP:                 cfg.MCP,
		Sandbox:             cfg.Sandbox,
		Notify:              cfg.Notify,
		Tools:               cfg.Tools,
		Swarm:               cfg.Swarm,
		Preferences:         cfg.Preferences,
		KeyBindings:         cfg.KeyBindings,
		LocalControl:        cfg.LocalControl,
		STT:                 cfg.STT,
		CrossSessionInbound: cfg.CrossSessionInbound,
	}, nil
}

func ResolveMCP(options ResolveOptions) (MCPConfig, error) {
	// Seed Zero's built-in default MCP servers (e.g. keyless Exa for free,
	// no-setup web search/scrape) BEFORE merging user/project config, so the user
	// can override any field or disable a default by writing over it.
	cfg := FileConfig{MCP: MCPConfig{Servers: DefaultMCPServers()}}

	if options.UserConfigPath != "" {
		fileConfig, err := loadConfigFile(options.UserConfigPath)
		if err != nil {
			return MCPConfig{}, err
		}
		// User config is higher-trust than project config: it may re-enable a
		// server the user disabled (a user-level disable is sticky, but the user
		// scope itself may lift it).
		mergeMCPConfig(&cfg.MCP, fileConfig.MCP, true)
		// Reconcile config written against a default Zero has since retired,
		// here while the user layer is the only one merged — so a carried-over
		// disable counts as a user-level decision the project layer cannot lift.
		migrateRetiredDefaultMCPServers(&cfg.MCP)
	}
	// Drop the project layer when the workspace is untrusted, so a cloned repo's
	// ./.zero/config.json cannot register (and spawn) MCP servers. Fail-closed:
	// only a trusted workspace clears ExcludeProject. Defaults and user config still load.
	if options.ProjectConfigPath != "" && !options.ExcludeProject {
		fileConfig, err := loadConfigFile(options.ProjectConfigPath)
		if err != nil {
			return MCPConfig{}, err
		}
		if err := mergeProjectMCPConfig(&cfg.MCP, fileConfig.MCP); err != nil {
			return MCPConfig{}, err
		}
	}
	mergeMCPConfig(&cfg.MCP, options.Overrides.MCP, true)
	return cfg.MCP, nil
}

func loadConfigFile(path string) (FileConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}

	var cfg FileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
	}
	return cfg, nil
}

func mergeConfig(dst *FileConfig, src FileConfig) {
	if activeProvider := strings.TrimSpace(src.ActiveProvider); activeProvider != "" {
		dst.ActiveProvider = activeProvider
	}
	if src.MaxTurns > 0 {
		dst.MaxTurns = src.MaxTurns
		dst.maxTurnsSet = true
	}
	for _, provider := range src.Providers {
		mergeProvider(dst, provider)
	}
	mergeMCPConfig(&dst.MCP, src.MCP, true)
	if src.Sandbox.Enabled != nil {
		dst.Sandbox.Enabled = src.Sandbox.Enabled
	}
	if network := strings.TrimSpace(src.Sandbox.Network); network != "" {
		dst.Sandbox.Network = network
	}
	dst.Sandbox.AdditionalWriteRoots = unionStrings(dst.Sandbox.AdditionalWriteRoots, src.Sandbox.AdditionalWriteRoots)
	if src.Sandbox.BlockUnixSockets {
		dst.Sandbox.BlockUnixSockets = true
	}
	if src.Sandbox.MonitorDenials {
		dst.Sandbox.MonitorDenials = true
	}
	if mode := strings.TrimSpace(src.Notify.Mode); mode != "" {
		dst.Notify.Mode = mode
	}
	if focusMode := strings.TrimSpace(src.Notify.FocusMode); focusMode != "" {
		dst.Notify.FocusMode = focusMode
	}
	if src.Tools.deferThresholdSet {
		dst.Tools.DeferThreshold = src.Tools.DeferThreshold
		dst.Tools.deferThresholdSet = true
	}
	if src.Swarm.MaxTeamSize != 0 {
		dst.Swarm.MaxTeamSize = src.Swarm.MaxTeamSize
	}
	if src.Preferences.FavoriteModels != nil {
		dst.Preferences.FavoriteModels = normalizeFavoriteModels(src.Preferences.FavoriteModels)
	}
	if src.Preferences.RecentModels != nil {
		dst.Preferences.RecentModels = NormalizeRecentModels(src.Preferences.RecentModels)
	}
	if src.Preferences.Recaps != nil {
		dst.Preferences.Recaps = src.Preferences.Recaps
	}
	if strings.TrimSpace(src.Preferences.Theme) != "" {
		dst.Preferences.Theme = strings.TrimSpace(src.Preferences.Theme)
	}
	if strings.TrimSpace(src.Preferences.Pet) != "" {
		dst.Preferences.Pet = strings.TrimSpace(src.Preferences.Pet)
	}
	mergeLocalControlConfig(&dst.LocalControl, src.LocalControl)
	mergeKeyBindings(&dst.KeyBindings, src.KeyBindings)
	mergeSTTConfig(&dst.STT, src.STT)
	if inbound := strings.TrimSpace(src.CrossSessionInbound); inbound != "" {
		dst.CrossSessionInbound = inbound
	}
}

func mergeProjectConfig(dst *FileConfig, src FileConfig) error {
	if activeProvider := strings.TrimSpace(src.ActiveProvider); activeProvider != "" {
		dst.ActiveProvider = activeProvider
	}
	if src.MaxTurns > 0 {
		dst.MaxTurns = src.MaxTurns
		dst.maxTurnsSet = true
	}
	for _, provider := range src.Providers {
		candidate := providerMergeCandidate(*dst, provider)
		if err := validateProjectProviderMerge(provider, candidate); err != nil {
			return err
		}
		mergeProvider(dst, provider)
	}
	if err := mergeProjectMCPConfig(&dst.MCP, src.MCP); err != nil {
		return err
	}
	// Sandbox.Enabled is intentionally NOT merged from project config: a cloned
	// repo's .zero/config.json must not be able to disable the sandbox that
	// constrains it. Only global config and CLI can turn the sandbox off.
	//
	// Sandbox.AdditionalWriteRoots is intentionally NOT merged from project
	// config: a cloned repo's .zero/config.json must not be able to grant
	// itself write access outside the workspace. Global config and CLI flags
	// are the only config sources for write roots.
	//
	// Sandbox.Network from project config may only TIGHTEN (→ "deny"), never
	// WEAKEN (→ "allow"). A malicious repo must not be able to open network
	// access for shell commands. Matches the AdditionalWriteRoots posture.
	if network := strings.TrimSpace(src.Sandbox.Network); network != "" {
		// Normalize: anything that isn't literally "allow" counts as deny.
		if sandbox.NormalizeNetworkMode(sandbox.NetworkMode(network)) != sandbox.NetworkAllow {
			dst.Sandbox.Network = network
		}
		// Silently ignore "allow" from project config — not an error, just
		// a privilege the project scope does not have.
	}
	if src.Sandbox.BlockUnixSockets {
		dst.Sandbox.BlockUnixSockets = true
	}
	if src.Sandbox.MonitorDenials {
		dst.Sandbox.MonitorDenials = true
	}
	if mode := strings.TrimSpace(src.Notify.Mode); mode != "" {
		dst.Notify.Mode = mode
	}
	if focusMode := strings.TrimSpace(src.Notify.FocusMode); focusMode != "" {
		dst.Notify.FocusMode = focusMode
	}
	if src.Tools.deferThresholdSet {
		dst.Tools.DeferThreshold = src.Tools.DeferThreshold
		dst.Tools.deferThresholdSet = true
	}
	if src.Swarm.MaxTeamSize != 0 {
		dst.Swarm.MaxTeamSize = src.Swarm.MaxTeamSize
	}
	if inbound := strings.TrimSpace(src.CrossSessionInbound); inboundPolicyRank(inbound) > inboundPolicyRank(dst.CrossSessionInbound) {
		dst.CrossSessionInbound = inbound
	}
	mergeKeyBindings(&dst.KeyBindings, src.KeyBindings)
	// Local control is intentionally user-config/override only. A cloned project
	// must not be able to make browser, desktop, or terminal automation tools
	// appear in the model's tool surface.
	return nil
}

func mergeProvider(cfg *FileConfig, provider ProviderProfile) {
	provider.Name = providerMergeName(*cfg, provider)

	for index := range cfg.Providers {
		if cfg.Providers[index].Name == provider.Name {
			cfg.Providers[index] = mergeProfile(cfg.Providers[index], provider)
			return
		}
	}
	cfg.Providers = append(cfg.Providers, provider)
}

func providerMergeCandidate(cfg FileConfig, provider ProviderProfile) ProviderProfile {
	provider.Name = providerMergeName(cfg, provider)
	for _, existing := range cfg.Providers {
		if existing.Name == provider.Name {
			return mergeProfile(existing, provider)
		}
	}
	return provider
}

func providerMergeName(cfg FileConfig, provider ProviderProfile) string {
	name := strings.TrimSpace(provider.Name)
	if name == "" {
		name = strings.TrimSpace(cfg.ActiveProvider)
	}
	if name == "" {
		name = string(ProviderKindOpenAI)
	}
	return name
}

func validateProjectProviderMerge(project ProviderProfile, candidate ProviderProfile) error {
	if strings.TrimSpace(project.APIKeyEnv) != "" &&
		projectEndpointNeedsCredentialGuard(candidate) &&
		!projectAPIKeyEnvAllowed(candidate, project.APIKeyEnv) {
		return providerError(candidate, "project provider %s cannot bind apiKeyEnv to custom provider endpoint", candidate.Name)
	}
	if strings.TrimSpace(project.BaseURL) != "" &&
		projectEndpointNeedsCredentialGuard(candidate) &&
		hasInheritedProviderCredentialMaterial(project, candidate) &&
		!projectBaseURLAllowed(candidate) {
		return providerError(candidate, "project provider %s cannot override baseURL for a credentialed custom provider endpoint", candidate.Name)
	}
	return nil
}

func projectEndpointNeedsCredentialGuard(profile ProviderProfile) bool {
	kind := effectiveProviderKind(profile)
	switch kind {
	case ProviderKindOpenAICompatible, ProviderKindAnthropicCompat:
		return strings.TrimSpace(profile.BaseURL) != "" || strings.TrimSpace(profile.CatalogID) != ""
	case ProviderKindOpenAI:
		return profile.BaseURL != "" && !isOfficialOpenAIBaseURL(profile.BaseURL)
	case ProviderKindAnthropic:
		return profile.BaseURL != "" && !isOfficialAnthropicBaseURL(profile.BaseURL)
	case ProviderKindGoogle:
		return profile.BaseURL != "" && !isOfficialGoogleBaseURL(profile.BaseURL)
	default:
		return strings.TrimSpace(profile.BaseURL) != ""
	}
}

func projectAPIKeyEnvAllowed(profile ProviderProfile, envName string) bool {
	descriptor, ok := catalogDescriptorForProfile(profile)
	if !ok {
		return false
	}
	baseURL := strings.TrimSpace(profile.BaseURL)
	if baseURL == "" {
		baseURL = descriptor.DefaultBaseURL
	}
	if !sameBaseURL(baseURL, descriptor.DefaultBaseURL) {
		return false
	}
	return containsStringFold(descriptor.AuthEnvVars, envName)
}

func projectBaseURLAllowed(profile ProviderProfile) bool {
	descriptor, ok := catalogDescriptorForProfile(profile)
	return ok && sameBaseURL(profile.BaseURL, descriptor.DefaultBaseURL)
}

func hasInheritedProviderCredentialMaterial(project ProviderProfile, candidate ProviderProfile) bool {
	if strings.TrimSpace(project.APIKey) == "" && strings.TrimSpace(candidate.APIKey) != "" {
		return true
	}
	if strings.TrimSpace(project.APIKeyEnv) == "" && strings.TrimSpace(candidate.APIKeyEnv) != "" {
		return true
	}
	if strings.TrimSpace(project.AuthHeaderValue) == "" && strings.TrimSpace(candidate.AuthHeaderValue) != "" {
		return true
	}
	if project.CustomHeaders == nil && hasCustomHeaderMaterial(candidate.CustomHeaders) {
		return true
	}
	return false
}

func hasCustomHeaderMaterial(headers map[string]string) bool {
	for _, value := range headers {
		if strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

func effectiveProviderKind(profile ProviderProfile) ProviderKind {
	if kind := ProviderKind(strings.TrimSpace(strings.ToLower(string(profile.ProviderKind)))); kind != "" {
		return kind
	}
	if provider := strings.TrimSpace(strings.ToLower(profile.Provider)); provider != "" {
		return ProviderKind(provider)
	}
	if descriptor, ok := catalogDescriptorForProfile(profile); ok {
		return providerKindForCatalogTransport(descriptor.Transport)
	}
	return ""
}

// catalogDefaultModel returns the default model of a catalog provider ("" when
// the id is unknown), used to fill a missing profile.Model for the official-API
// kinds so a hand-written profile without a model resolves instead of bricking
// every command that resolves config up front (zero config, bare zero setup).
func catalogDefaultModel(catalogID string) string {
	descriptor, ok := providercatalog.Get(catalogID)
	if !ok {
		return ""
	}
	return strings.TrimSpace(descriptor.DefaultModel)
}

func catalogDescriptorForProfile(profile ProviderProfile) (providercatalog.Descriptor, bool) {
	catalogID := providercatalog.NormalizeID(profile.CatalogID)
	if catalogID == "" {
		return providercatalog.Descriptor{}, false
	}
	descriptor, err := providercatalog.Require(catalogID)
	if err != nil {
		return providercatalog.Descriptor{}, false
	}
	return descriptor, true
}

func containsStringFold(values []string, target string) bool {
	target = strings.TrimSpace(target)
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), target) {
			return true
		}
	}
	return false
}

func mergeProfile(base ProviderProfile, next ProviderProfile) ProviderProfile {
	if next.Name != "" {
		base.Name = next.Name
	}
	if next.Provider != "" {
		base.Provider = next.Provider
	}
	if next.ProviderKind != "" {
		base.ProviderKind = next.ProviderKind
	}
	if next.CatalogID != "" {
		base.CatalogID = next.CatalogID
	}
	if next.BaseURL != "" {
		base.BaseURL = next.BaseURL
	}
	if next.APIKey != "" {
		base.APIKey = next.APIKey
	}
	if next.APIKeyEnv != "" {
		base.APIKeyEnv = next.APIKeyEnv
	}
	if next.APIFormat != "" {
		base.APIFormat = next.APIFormat
	}
	if next.AuthHeader != "" {
		base.AuthHeader = next.AuthHeader
	}
	if next.AuthScheme != "" {
		base.AuthScheme = next.AuthScheme
	}
	if next.AuthHeaderValue != "" {
		base.AuthHeaderValue = next.AuthHeaderValue
	}
	if next.CustomHeaders != nil {
		base.CustomHeaders = copyStringMap(next.CustomHeaders)
	}
	if next.Model != "" {
		base.Model = next.Model
	}
	if next.Description != "" {
		base.Description = next.Description
	}
	// A nil *bool means "unset"; only an explicit value overrides. Without this the
	// parseThinkTags setting was silently dropped on every profile merge (L17).
	if next.ParseThinkTags != nil {
		base.ParseThinkTags = next.ParseThinkTags
	}
	return base
}

// ActiveProviderEnv selects the active provider profile by name (read in applyEnv).
const ActiveProviderEnv = "ZERO_PROVIDER"

// SetActiveProviderEnv exports the active provider name to the process environment
// so a spawned child process (which inherits the environment) resolves the SAME
// provider profile — and therefore the same credentials (env key / stored key /
// OAuth) — as its parent. Without this a sub-agent re-resolves config.json's
// default provider and can land on one whose credentials don't match the parent's
// live selection, failing auth the instant it spawns. A blank name CLEARS the
// variable: switching back to an unnamed/default profile must not keep exporting a
// stale provider to children.
func SetActiveProviderEnv(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		_ = os.Unsetenv(ActiveProviderEnv)
		return
	}
	_ = os.Setenv(ActiveProviderEnv, name)
}

// MaxTurnsEnv overrides the per-run tool-turn budget by name (read in applyEnv).
const MaxTurnsEnv = "ZERO_MAX_TURNS"

// SetMaxTurnsEnv exports the per-run tool-turn budget to the process environment so
// a spawned child (sub-agent / swarm member, which inherits the environment) runs
// with the SAME budget the user set via /turns. Without it a child re-resolves
// config.json's default and a large delegated task can exhaust its turns mid-run
// (exit 4 / max-turns). No-op for n <= 0.
func SetMaxTurnsEnv(n int) {
	if n > 0 {
		_ = os.Setenv(MaxTurnsEnv, strconv.Itoa(n))
	}
}

func applyEnv(cfg *FileConfig, env map[string]string) {
	activeProvider := strings.TrimSpace(envValue(env, ActiveProviderEnv))
	if activeProvider != "" {
		cfg.ActiveProvider = activeProvider
	}
	if maxTurns := strings.TrimSpace(envValue(env, MaxTurnsEnv)); maxTurns != "" {
		if n, err := strconv.Atoi(maxTurns); err == nil && n > 0 {
			if n > MaxTurnsCeiling {
				n = MaxTurnsCeiling
			}
			cfg.MaxTurns = n
			cfg.maxTurnsSet = true
		}
	}

	applyProviderEnv(cfg, ProviderKindOpenAI, envProfile{
		Name:    string(ProviderKindOpenAI),
		APIKey:  envValue(env, "OPENAI_API_KEY"),
		BaseURL: envValue(env, "OPENAI_BASE_URL"),
		Model:   envValue(env, "OPENAI_MODEL"),
	})
	applyProviderEnv(cfg, ProviderKindAnthropic, envProfile{
		Name:    string(ProviderKindAnthropic),
		APIKey:  envValue(env, "ANTHROPIC_API_KEY"),
		BaseURL: envValue(env, "ANTHROPIC_BASE_URL"),
		Model:   envValue(env, "ANTHROPIC_MODEL"),
	})
	applyProviderEnv(cfg, ProviderKindGoogle, envProfile{
		Name:    string(ProviderKindGoogle),
		APIKey:  firstNonEmpty(envValue(env, "GEMINI_API_KEY"), envValue(env, "GOOGLE_API_KEY")),
		BaseURL: firstNonEmpty(envValue(env, "GEMINI_BASE_URL"), envValue(env, "GOOGLE_BASE_URL")),
		Model:   firstNonEmpty(envValue(env, "GEMINI_MODEL"), envValue(env, "GOOGLE_MODEL")),
	})
}

type envProfile struct {
	Name    string
	APIKey  string
	BaseURL string
	Model   string
}

func applyProviderEnv(cfg *FileConfig, providerKind ProviderKind, env envProfile) {
	apiKey := strings.TrimSpace(env.APIKey)
	baseURL := strings.TrimSpace(env.BaseURL)
	model := strings.TrimSpace(env.Model)
	if apiKey == "" && baseURL == "" && model == "" {
		return
	}

	profile := ProviderProfile{
		Name:         providerEnvTargetName(cfg, providerKind, env.Name),
		ProviderKind: providerKind,
		APIKey:       apiKey,
		BaseURL:      baseURL,
		Model:        model,
	}
	if profile.Name == "" {
		profile.Name = string(providerKind)
	}
	// A non-official baseURL from the environment signals a proxy/gateway, so promote
	// the first-party kind to its -compatible transport (which accepts a custom URL).
	// Env-only by design: a custom baseURL in a project config.json is left as-is and
	// rejected by Resolve, so an untrusted project can't redirect a first-party key to
	// another host.
	if providerKind == ProviderKindOpenAI && baseURL != "" && !isOfficialOpenAIBaseURL(baseURL) {
		profile.ProviderKind = ProviderKindOpenAICompatible
	}
	if providerKind == ProviderKindAnthropic && baseURL != "" && !isOfficialAnthropicBaseURL(baseURL) {
		profile.ProviderKind = ProviderKindAnthropicCompat
	}
	// When the env supplies only credentials (no baseURL) for a provider name that
	// already exists, don't force the standard transport kind — a same-named
	// openai-compatible/anthropic-compatible proxy or gateway must keep its kind.
	// An empty kind makes mergeProfile preserve the existing provider's kind (H2).
	if baseURL == "" {
		for _, existing := range cfg.Providers {
			if strings.TrimSpace(existing.Name) == profile.Name {
				profile.ProviderKind = ""
				break
			}
		}
	}
	mergeProvider(cfg, profile)
}

func providerEnvTargetName(cfg *FileConfig, providerKind ProviderKind, fallback string) string {
	if activeName := strings.TrimSpace(cfg.ActiveProvider); activeName != "" {
		for _, provider := range cfg.Providers {
			if strings.TrimSpace(provider.Name) != activeName {
				continue
			}
			if normalizedProfileKind(provider) == providerKind {
				return activeName
			}
			break
		}
	}

	for _, provider := range cfg.Providers {
		if normalizedProfileKind(provider) != providerKind {
			continue
		}
		if name := strings.TrimSpace(provider.Name); name != "" {
			return name
		}
	}

	return strings.TrimSpace(fallback)
}

func normalizedProfileKind(profile ProviderProfile) ProviderKind {
	kind := strings.TrimSpace(string(profile.ProviderKind))
	if kind == "" {
		kind = strings.TrimSpace(profile.Provider)
	}
	return ProviderKind(strings.ToLower(kind))
}

func envValue(env map[string]string, key string) string {
	if env != nil {
		return env[key]
	}
	return os.Getenv(key)
}

func applyOverrides(cfg *FileConfig, overrides Overrides) {
	if activeProvider := strings.TrimSpace(overrides.ActiveProvider); activeProvider != "" {
		cfg.ActiveProvider = activeProvider
	}
	if overrides.MaxTurns > 0 {
		cfg.MaxTurns = overrides.MaxTurns
		cfg.maxTurnsSet = true
	}
	if overrides.Sandbox.Enabled != nil {
		cfg.Sandbox.Enabled = overrides.Sandbox.Enabled
	}
	if overrides.Sandbox.BlockUnixSockets {
		cfg.Sandbox.BlockUnixSockets = true
	}
	if overrides.Sandbox.MonitorDenials {
		cfg.Sandbox.MonitorDenials = true
	}
	if mode := strings.TrimSpace(overrides.Notify.Mode); mode != "" {
		cfg.Notify.Mode = mode
	}
	if focusMode := strings.TrimSpace(overrides.Notify.FocusMode); focusMode != "" {
		cfg.Notify.FocusMode = focusMode
	}
	if overrides.Tools.deferThresholdSet || overrides.Tools.DeferThreshold != 0 {
		cfg.Tools.DeferThreshold = overrides.Tools.DeferThreshold
		cfg.Tools.deferThresholdSet = true
	}
	mergeLocalControlConfig(&cfg.LocalControl, overrides.LocalControl)
	mergeKeyBindings(&cfg.KeyBindings, overrides.KeyBindings)
	mergeSTTConfig(&cfg.STT, overrides.STT)
	if inbound := strings.TrimSpace(overrides.CrossSessionInbound); inbound != "" {
		cfg.CrossSessionInbound = inbound
	}
	for _, provider := range overrides.Providers {
		mergeProvider(cfg, provider)
	}
	if hasProviderFields(overrides.Provider) {
		mergeProvider(cfg, overrides.Provider)
	}
	mergeMCPConfig(&cfg.MCP, overrides.MCP, true)
}

func inboundPolicyRank(policy string) int {
	switch strings.TrimSpace(policy) {
	case "refuse":
		return 3
	case "hold":
		return 2
	case "accept":
		return 0
	default:
		return 1 // permission-mode parity
	}
}

func mergeLocalControlConfig(dst *LocalControlConfig, src LocalControlConfig) {
	if src.enabledSet {
		dst.Enabled = src.Enabled
		dst.enabledSet = true
	}
	mergeLocalControlDriverConfig(&dst.Browser, src.Browser)
	mergeLocalControlDriverConfig(&dst.Desktop, src.Desktop)
	mergeLocalControlDriverConfig(&dst.Terminal, src.Terminal)
	if artifactsDir := strings.TrimSpace(src.ArtifactsDir); artifactsDir != "" {
		dst.ArtifactsDir = artifactsDir
	}
}

func mergeLocalControlDriverConfig(dst *LocalControlDriverConfig, src LocalControlDriverConfig) {
	if src.enabledSet {
		dst.Enabled = src.Enabled
		dst.enabledSet = true
	}
	if helperPath := strings.TrimSpace(src.HelperPath); helperPath != "" {
		dst.HelperPath = helperPath
	}
	if driver := strings.TrimSpace(src.Driver); driver != "" {
		dst.Driver = driver
	}
}

func mergeKeyBindings(dst *KeyBindingsConfig, src KeyBindingsConfig) {
	if src.ToggleDetailed != "" {
		dst.ToggleDetailed = src.ToggleDetailed
	}
	if src.ToggleMouse != "" {
		dst.ToggleMouse = src.ToggleMouse
	}
	if src.CycleReasoning != "" {
		dst.CycleReasoning = src.CycleReasoning
	}
	if src.TogglePlan != "" {
		dst.TogglePlan = src.TogglePlan
	}
	if src.ToggleSidebar != "" {
		dst.ToggleSidebar = src.ToggleSidebar
	}
}

// mergeSTTConfig overlays any set field of src onto dst. Each field carries its
// own "unset" sentinel (empty string, 0, or nil *bool), matching how the other
// section mergers detect intent.
func mergeSTTConfig(dst *STTConfig, src STTConfig) {
	if src.Provider != "" {
		dst.Provider = src.Provider
	}
	if src.StreamProvider != "" {
		dst.StreamProvider = src.StreamProvider
	}
	if src.Streaming != nil {
		dst.Streaming = src.Streaming
	}
	if src.Model != "" {
		dst.Model = src.Model
	}
	if src.StreamModel != "" {
		dst.StreamModel = src.StreamModel
	}
	if src.LocalModelPath != "" {
		dst.LocalModelPath = src.LocalModelPath
	}
	if src.LocalBinary != "" {
		dst.LocalBinary = src.LocalBinary
	}
	if src.LocalServerBinary != "" {
		dst.LocalServerBinary = src.LocalServerBinary
	}
	if src.LocalServerPort != 0 {
		dst.LocalServerPort = src.LocalServerPort
	}
	if src.EngineVersion != "" {
		dst.EngineVersion = src.EngineVersion
	}
	if src.NumThreads != 0 {
		dst.NumThreads = src.NumThreads
	}
	if src.Language != "" {
		dst.Language = src.Language
	}
	if src.MaxDurationSeconds != 0 {
		dst.MaxDurationSeconds = src.MaxDurationSeconds
	}
	if src.SilenceAutoStop != nil {
		dst.SilenceAutoStop = src.SilenceAutoStop
	}
	if src.AutoSubmit != nil {
		dst.AutoSubmit = src.AutoSubmit
	}
	if src.WindowsAudioDevice != "" {
		dst.WindowsAudioDevice = src.WindowsAudioDevice
	}
}

// validateSTTConfig rejects unknown provider/streamProvider values and a
// negative maxDurationSeconds at load time — a clear startup error naming the
// bad value and the valid options, never a silent fallback the user did not ask
// for (§11a).
func validateSTTConfig(cfg STTConfig) error {
	if cfg.Provider != "" {
		switch cfg.Provider {
		case STTProviderLocal, STTProviderGroq, STTProviderOpenAI:
		default:
			return fmt.Errorf("invalid stt.provider %q: expected local, groq, or openai", cfg.Provider)
		}
	}
	if cfg.StreamProvider != "" {
		switch cfg.StreamProvider {
		case STTProviderLocal, STTProviderDeepgram, STTProviderOpenAI:
		default:
			return fmt.Errorf("invalid stt.streamProvider %q: expected local, deepgram, or openai", cfg.StreamProvider)
		}
	}
	if cfg.MaxDurationSeconds < 0 {
		return fmt.Errorf("invalid stt.maxDurationSeconds %d: must be >= 0 (0 uses the default)", cfg.MaxDurationSeconds)
	}
	if cfg.NumThreads < 0 {
		return fmt.Errorf("invalid stt.numThreads %d: must be >= 0 (0 uses the engine default)", cfg.NumThreads)
	}
	if cfg.LocalServerPort < 0 || cfg.LocalServerPort > 65535 {
		return fmt.Errorf("invalid stt.localServerPort %d: must be between 1 and 65535 (0 uses the default)", cfg.LocalServerPort)
	}
	return nil
}

func hasProviderFields(profile ProviderProfile) bool {
	return HasProviderProfile(profile)
}

// unionStrings appends the values of extra that are not already present in
// base, preserving order. Used for additive config keys like
// sandbox.additionalWriteRoots where a later layer must not erase earlier
// grants.
func unionStrings(base []string, extra []string) []string {
	seen := make(map[string]struct{}, len(base))
	for _, value := range base {
		seen[value] = struct{}{}
	}
	for _, value := range extra {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		base = append(base, value)
	}
	return base
}

type normalizeOptions struct {
	// defaultModels fills a missing profile.Model for the official-API provider
	// kinds (openai from the model registry; anthropic/google from their catalog
	// descriptors). Off for provider-command configs (command.go), which must
	// surface exactly what the external command returned rather than inventing
	// a model behind its back.
	defaultModels bool
}

func normalizeProviders(providers []ProviderProfile, activeName string, envMaps ...map[string]string) ([]ProviderProfile, ProviderProfile, error) {
	return normalizeProvidersWithOptions(providers, activeName, normalizeOptions{defaultModels: true}, envMaps...)
}

func normalizeProvidersWithoutModelDefaults(providers []ProviderProfile, activeName string, envMaps ...map[string]string) ([]ProviderProfile, ProviderProfile, error) {
	return normalizeProvidersWithOptions(providers, activeName, normalizeOptions{}, envMaps...)
}

func normalizeProvidersWithOptions(providers []ProviderProfile, activeName string, options normalizeOptions, envMaps ...map[string]string) ([]ProviderProfile, ProviderProfile, error) {
	activeName = strings.TrimSpace(activeName)
	var env map[string]string
	if len(envMaps) > 0 {
		env = envMaps[0]
	}
	if len(providers) == 0 {
		if activeName != "" {
			return nil, ProviderProfile{}, fmt.Errorf("%w: active provider %q not found", ErrNoActiveProvider, activeName)
		}
		return []ProviderProfile{}, ProviderProfile{}, nil
	}

	if activeName == "" && len(providers) == 1 {
		activeName = providers[0].Name
	}

	normalized := make([]ProviderProfile, 0, len(providers))
	var active ProviderProfile
	activeFound := false
	for _, provider := range providers {
		next, err := normalizeProvider(provider, env, options)
		if err != nil {
			// One unresolvable provider (e.g. a profile referencing a provider preset
			// this build doesn't ship) must NOT brick the whole app — drop it and keep
			// the rest. Only the ACTIVE provider failing is fatal, since the run can't
			// proceed without it.
			if strings.TrimSpace(provider.Name) == activeName {
				return nil, ProviderProfile{}, err
			}
			continue
		}
		normalized = append(normalized, next)
		if next.Name == activeName {
			active = next
			activeFound = true
		}
	}

	if !activeFound {
		// Return the successfully normalized list alongside the error (rather than
		// nil) so a caller like the interactive TUI can still fall back to an
		// already-configured, usable provider instead of forcing a full
		// re-onboarding wizard just because none was marked active.
		return normalized, ProviderProfile{}, fmt.Errorf("%w: active provider %q not found", ErrNoActiveProvider, activeName)
	}
	if active.Model == "" {
		return nil, ProviderProfile{}, &setupFixableError{
			err:      providerError(active, "provider %s requires model — add \"model\" to its entry in config.json, or re-run: zero setup <catalog-id> --model <model>", active.Name),
			sentinel: ErrProviderRequiresModel,
		}
	}

	return normalized, active, nil
}

func normalizeProvider(profile ProviderProfile, env map[string]string, options normalizeOptions) (ProviderProfile, error) {
	profile.Name = strings.TrimSpace(profile.Name)
	profile.Provider = strings.TrimSpace(profile.Provider)
	profile.ProviderKind = ProviderKind(strings.TrimSpace(strings.ToLower(string(profile.ProviderKind))))
	profile.CatalogID = providercatalog.NormalizeID(profile.CatalogID)
	profile.BaseURL = strings.TrimSpace(profile.BaseURL)
	explicitBaseURL := profile.BaseURL != ""
	profile.APIKeyEnv = strings.TrimSpace(profile.APIKeyEnv)
	profile.APIFormat = strings.TrimSpace(profile.APIFormat)
	profile.AuthHeader = strings.TrimSpace(profile.AuthHeader)
	profile.AuthScheme = strings.TrimSpace(profile.AuthScheme)
	profile.AuthHeaderValue = strings.TrimSpace(profile.AuthHeaderValue)
	profile.Model = strings.TrimSpace(profile.Model)

	if profile.Name == "" {
		profile.Name = string(ProviderKindOpenAI)
	}
	if profile.CatalogID != "" {
		descriptor, err := providercatalog.Require(profile.CatalogID)
		if err != nil {
			return ProviderProfile{}, providerError(profile, "%s", err.Error())
		}
		if !providercatalog.RuntimeSupported(descriptor) {
			return ProviderProfile{}, providerError(profile, "provider %q uses transport %q: %s", descriptor.ID, descriptor.Transport, providercatalog.RuntimeUnsupportedReason(descriptor))
		}
		applyCatalogDescriptor(&profile, descriptor, explicitBaseURL)
	}
	if profile.ProviderKind == "" && profile.Provider != "" {
		profile.ProviderKind = ProviderKind(strings.ToLower(profile.Provider))
	}
	if profile.ProviderKind == "" {
		profile.ProviderKind = ProviderKindOpenAI
	}
	if strings.TrimSpace(profile.APIKey) == "" && profile.APIKeyEnv != "" {
		profile.APIKey = strings.TrimSpace(envValue(env, profile.APIKeyEnv))
	}

	switch profile.ProviderKind {
	case ProviderKindOpenAI:
		if profile.BaseURL == "" || isOfficialOpenAIBaseURL(profile.BaseURL) {
			profile.BaseURL = OpenAIBaseURL
			if options.defaultModels && profile.Model == "" {
				profile.Model = modelregistry.DefaultModelID
			}
			return profile, nil
		}
		return ProviderProfile{}, providerError(profile, "openai provider %s requires official baseURL %s", profile.Name, OpenAIBaseURL)
	case ProviderKindOpenAICompatible:
		if profile.BaseURL == "" {
			return ProviderProfile{}, providerError(profile, "openai-compatible provider %s requires baseURL", profile.Name)
		}
		if isOfficialOpenAIBaseURL(profile.BaseURL) {
			return ProviderProfile{}, providerError(profile, "openai-compatible provider %s requires custom baseURL", profile.Name)
		}
		return profile, nil
	case ProviderKindAnthropic:
		if profile.BaseURL == "" || isOfficialAnthropicBaseURL(profile.BaseURL) {
			profile.BaseURL = AnthropicBaseURL
			if options.defaultModels && profile.Model == "" {
				profile.Model = catalogDefaultModel("anthropic")
			}
			return profile, nil
		}
		return ProviderProfile{}, providerError(profile, "anthropic provider %s requires official baseURL %s", profile.Name, AnthropicBaseURL)
	case ProviderKindAnthropicCompat:
		if profile.BaseURL == "" {
			return ProviderProfile{}, providerError(profile, "anthropic-compatible provider %s requires baseURL", profile.Name)
		}
		if isOfficialAnthropicBaseURL(profile.BaseURL) {
			return ProviderProfile{}, providerError(profile, "anthropic-compatible provider %s requires custom baseURL", profile.Name)
		}
		return profile, nil
	case ProviderKindGoogle:
		if profile.BaseURL == "" || isOfficialGoogleBaseURL(profile.BaseURL) {
			profile.BaseURL = GoogleBaseURL
			if options.defaultModels && profile.Model == "" {
				profile.Model = catalogDefaultModel("google")
			}
			return profile, nil
		}
		return ProviderProfile{}, providerError(profile, "google provider %s requires official baseURL %s", profile.Name, GoogleBaseURL)
	default:
		return ProviderProfile{}, providerError(profile, "unknown provider kind %q for provider %s", profile.ProviderKind, profile.Name)
	}
}

func applyCatalogDescriptor(profile *ProviderProfile, descriptor providercatalog.Descriptor, explicitBaseURL bool) {
	if descriptor.ID != "" {
		profile.CatalogID = descriptor.ID
	}
	if profile.ProviderKind == "" && strings.TrimSpace(profile.Provider) == "" {
		profile.ProviderKind = providerKindForCatalogTransport(descriptor.Transport)
	}
	if profile.BaseURL == "" {
		profile.BaseURL = descriptor.DefaultBaseURL
	}
	if profile.Model == "" {
		profile.Model = descriptor.DefaultModel
	}
	if profile.APIKeyEnv == "" && len(descriptor.AuthEnvVars) > 0 && (!explicitBaseURL || sameBaseURL(profile.BaseURL, descriptor.DefaultBaseURL)) {
		profile.APIKeyEnv = descriptor.AuthEnvVars[0]
	}
	canonicalCatalogEndpoint := !explicitBaseURL || sameBaseURL(profile.BaseURL, descriptor.DefaultBaseURL)
	if len(descriptor.CustomHeaders) > 0 && canonicalCatalogEndpoint {
		catalogHeaders := descriptor.CustomHeaders
		if strings.EqualFold(strings.TrimSpace(descriptor.ID), "aimlapi") {
			catalogHeaders = aimlapi.WithResolvedPartnerHeader(catalogHeaders)
		}
		merged := copyStringMap(catalogHeaders)
		for key, value := range profile.CustomHeaders {
			// Header names are case-insensitive. Preserve the catalog spelling while
			// replacing its value so request construction cannot see two colliding
			// map entries whose eventual winner depends on iteration order.
			existingKey := ""
			for candidate := range merged {
				if strings.EqualFold(candidate, key) {
					existingKey = candidate
					break
				}
			}
			if existingKey != "" {
				merged[existingKey] = value
				continue
			}
			merged[key] = value
		}
		profile.CustomHeaders = merged
	} else if strings.EqualFold(strings.TrimSpace(descriptor.ID), "aimlapi") && !canonicalCatalogEndpoint {
		// AIMLAPI attribution is owned by the catalog endpoint. A profile can retain
		// those generated headers after its base URL is edited; strip their names
		// before sending requests to an arbitrary staging/proxy host while preserving
		// unrelated headers explicitly supplied by the user.
		for profileKey := range profile.CustomHeaders {
			for catalogKey := range descriptor.CustomHeaders {
				if strings.EqualFold(profileKey, catalogKey) {
					delete(profile.CustomHeaders, profileKey)
					break
				}
			}
		}
	}
}

func sameBaseURL(left string, right string) bool {
	return strings.EqualFold(
		strings.TrimRight(strings.TrimSpace(left), "/"),
		strings.TrimRight(strings.TrimSpace(right), "/"),
	)
}

func isOfficialAnthropicBaseURL(baseURL string) bool {
	normalized := strings.TrimSpace(baseURL)
	if strings.TrimRight(normalized, "/") == strings.TrimRight(AnthropicBaseURL, "/") {
		return true
	}
	parsed, err := url.Parse(normalized)
	if err != nil || parsed.Host == "" {
		return false
	}
	official, err := url.Parse(AnthropicBaseURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(parsed.Host, official.Host)
}

func isOfficialGoogleBaseURL(baseURL string) bool {
	normalized := strings.TrimSpace(baseURL)
	if strings.TrimRight(normalized, "/") == strings.TrimRight(GoogleBaseURL, "/") {
		return true
	}
	parsed, err := url.Parse(normalized)
	if err != nil || parsed.Host == "" {
		return false
	}
	official, err := url.Parse(GoogleBaseURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(parsed.Host, official.Host)
}

func providerKindForCatalogTransport(transport providercatalog.Transport) ProviderKind {
	switch transport {
	case providercatalog.TransportOpenAI:
		return ProviderKindOpenAI
	case providercatalog.TransportAnthropic:
		return ProviderKindAnthropic
	case providercatalog.TransportAnthropicCompatible:
		return ProviderKindAnthropicCompat
	case providercatalog.TransportGoogle:
		return ProviderKindGoogle
	case providercatalog.TransportOpenAICompatible:
		return ProviderKindOpenAICompatible
	default:
		return ProviderKind(strings.ToLower(string(transport)))
	}
}

func isOfficialOpenAIBaseURL(baseURL string) bool {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	return baseURL == "" ||
		baseURL == "openai" ||
		baseURL == strings.TrimRight(OpenAIBaseURL, "/")
}

func providerError(profile ProviderProfile, format string, args ...any) error {
	message := fmt.Sprintf(format, args...)
	if profile.APIKey != "" {
		message += " (apiKey=[REDACTED])"
	}
	return fmt.Errorf("%s", redactSecrets(message, profile.APIKey, profile.AuthHeaderValue))
}
