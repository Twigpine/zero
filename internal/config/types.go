package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

const OpenAIBaseURL = "https://api.openai.com/v1"
const AnthropicBaseURL = "https://api.anthropic.com"
const GoogleBaseURL = "https://generativelanguage.googleapis.com"

type ProviderKind string

const (
	ProviderKindOpenAI           ProviderKind = "openai"
	ProviderKindAnthropic        ProviderKind = "anthropic"
	ProviderKindAnthropicCompat  ProviderKind = "anthropic-compatible"
	ProviderKindGoogle           ProviderKind = "google"
	ProviderKindOpenAICompatible ProviderKind = "openai-compatible"
)

type ProviderProfile struct {
	Name         string       `json:"name"`
	Provider     string       `json:"provider,omitempty"`
	ProviderKind ProviderKind `json:"provider_kind,omitempty"`
	CatalogID    string       `json:"catalogID,omitempty"`
	BaseURL      string       `json:"baseURL,omitempty"`
	APIKey       string       `json:"apiKey,omitempty"`
	APIKeyEnv    string       `json:"apiKeyEnv,omitempty"`
	// APIKeyStored marks that this provider's API key lives in the encrypted
	// credential store (internal/credstore), not inline in APIKey. The effective
	// key is loaded from the store at provider-build time; config.json holds only
	// this marker, never the secret.
	APIKeyStored    bool              `json:"apiKeyStored,omitempty"`
	APIFormat       string            `json:"apiFormat,omitempty"`
	AuthHeader      string            `json:"authHeader,omitempty"`
	AuthScheme      string            `json:"authScheme,omitempty"`
	AuthHeaderValue string            `json:"authHeaderValue,omitempty"`
	CustomHeaders   map[string]string `json:"customHeaders,omitempty"`
	Model           string            `json:"model,omitempty"`
	ParseThinkTags  *bool             `json:"parseThinkTags,omitempty"`
	Description     string            `json:"description,omitempty"`
}

func HasProviderProfile(profile ProviderProfile) bool {
	return strings.TrimSpace(profile.Name) != "" ||
		strings.TrimSpace(profile.Provider) != "" ||
		strings.TrimSpace(string(profile.ProviderKind)) != "" ||
		strings.TrimSpace(profile.CatalogID) != "" ||
		strings.TrimSpace(profile.BaseURL) != "" ||
		strings.TrimSpace(profile.APIKey) != "" ||
		strings.TrimSpace(profile.APIKeyEnv) != "" ||
		strings.TrimSpace(profile.APIFormat) != "" ||
		strings.TrimSpace(profile.AuthHeader) != "" ||
		strings.TrimSpace(profile.AuthScheme) != "" ||
		strings.TrimSpace(profile.AuthHeaderValue) != "" ||
		profile.CustomHeaders != nil ||
		strings.TrimSpace(profile.Model) != "" ||
		profile.ParseThinkTags != nil ||
		strings.TrimSpace(profile.Description) != ""
}

type SandboxConfig struct {
	// Enabled turns the sandbox off when set to false (`"sandbox": {"enabled":
	// false}`). A pointer distinguishes an explicit false (disable) from an
	// omitted key (keep the default: enabled). Honored from the GLOBAL user
	// config and CLI only — deliberately NOT project config, so a cloned repo
	// cannot disable the sandbox that constrains it. nil / true keep enforcement.
	Enabled *bool `json:"enabled,omitempty"`
	// Network controls whether shell commands classified as network-touching
	// (curl, git push, package installs, …) are allowed: "allow" or "deny".
	// Empty keeps the built-in default (deny). Without this knob the engine's
	// hard-coded NetworkDeny was unreachable from any config surface.
	Network string `json:"network,omitempty"`
	// AdditionalWriteRoots lists directories outside the workspace the sandbox
	// allows writes in. Each entry must be an existing directory; entries are
	// normalized (~-expanded, absolutized, symlink-resolved) at startup and an
	// invalid entry fails the run. Honored from the GLOBAL user config and CLI
	// flags only — deliberately not project config, so a cloned repo cannot
	// grant itself write access. Session-only grants use /add-dir instead.
	AdditionalWriteRoots []string `json:"additionalWriteRoots,omitempty"`
	// BlockUnixSockets, when true, asks the Linux sandbox helper to install a
	// best-effort seccomp filter that denies AF_UNIX socket creation inside the
	// sandboxed command. Off by default; ignored on non-Linux backends.
	BlockUnixSockets bool `json:"blockUnixSockets,omitempty"`
	// MonitorDenials, when true on macOS, tails the unified log for this run's
	// sandbox denials and appends them to a command's stderr so blocked operations
	// are visible to the agent. Off by default. No-op on platforms/OS versions that
	// do not deliver seatbelt denials to the queryable log.
	MonitorDenials bool `json:"monitorDenials,omitempty"`
}

type NotifyConfig struct {
	Mode      string `json:"mode,omitempty"`
	FocusMode string `json:"focusMode,omitempty"`
}

type ToolsConfig struct {
	DeferThreshold    int `json:"deferThreshold,omitempty"`
	deferThresholdSet bool
}

type PreferencesConfig struct {
	FavoriteModels []string `json:"favoriteModels,omitempty"`
	// RecentModels is the short automatic history of provider+model pairs the
	// user has switched to via /model, newest first. Unlike FavoriteModels
	// (manual pins), this list is maintained automatically on every switch and
	// capped to MaxRecentModels entries. See RecentModelEntry.
	RecentModels []RecentModelEntry `json:"recentModels,omitempty"`
	// Theme is the persisted TUI palette preference — "auto" or a registered theme
	// name (e.g. "dracula"). Applied at startup below the --theme flag and
	// ZERO_THEME, so a /theme choice survives restart. Empty = unset (defaults auto).
	Theme string `json:"theme,omitempty"`
	// Pet is the terminal companion selected through /pets. Empty leaves pets
	// off until the user chooses one; "disabled" is the explicit off state.
	Pet string `json:"pet,omitempty"`
	// Recaps is a tri-state: nil (unset) defaults to ON; an explicit false means
	// the user turned idle recaps off. A *bool is its own tri-state, so no
	// custom unmarshal is needed (unlike ToolsConfig.DeferThreshold's int).
	Recaps *bool `json:"recaps,omitempty"`
	// KeepFinishedAgents makes the AGENTS panel KEEP finished sub-agents instead
	// of dropping them 1.5s after they complete.
	//
	// The drop is the default (PR #345, "agents never disappear"), and the panel
	// has a click-to-toggle to override it per session — but the toggle resets
	// every session. This is the standing preference: set true and finished
	// agents stay for the whole session, every session, without clicking.
	//
	// A *bool tri-state like Recaps: nil is unset (the drop-after-linger
	// default), true keeps them, false is the explicit drop.
	KeepFinishedAgents *bool `json:"keepFinishedAgents,omitempty"`
	// CompactionModel routes compaction summarization calls to this model
	// instead of the session's main model (summaries at main-model prices are
	// the most expensive recurring event in long runs). Empty = automatic: a
	// curated cheap model on official endpoints, the main model elsewhere.
	// "main" forces the main model. ZERO_COMPACTION_MODEL overrides.
	CompactionModel string `json:"compactionModel,omitempty"`
}

// RecentModelEntry is one provider-qualified model selection recorded in
// Preferences.RecentModels. Provider is the saved provider profile's Name (not
// a display label), so a recent entry can be resolved back to a concrete
// profile the same way the /model picker's cross-provider rows are.
type RecentModelEntry struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// MaxRecentModels caps the persisted/displayed recent-selection history.
const MaxRecentModels = 5

// RecapsEnabled reports whether idle recaps are on. Unset defaults to ON.
func (p PreferencesConfig) RecapsEnabled() bool {
	return p.Recaps == nil || *p.Recaps
}

// KeepsFinishedAgents reports the standing preference for the AGENTS panel.
//
// Defaults FALSE, unlike RecapsEnabled which defaults true: dropping finished
// agents after a linger is the established product default (PR #345), so an
// unset preference keeps it. Only an explicit true makes them stay.
func (p PreferencesConfig) KeepsFinishedAgents() bool {
	return p.KeepFinishedAgents != nil && *p.KeepFinishedAgents
}

// KeyBindingDef defines one key binding string (e.g. "ctrl+o") that the TUI
// can remap. An empty string means "use the built-in default" for that action.
type KeyBindingDef string

// KeyBindingsConfig holds the subset of TUI keybindings that users may remap
// via config.json. Each field defaults to a sensible built-in when empty.
type KeyBindingsConfig struct {
	// ToggleDetailed toggles the detailed transcript view (default: ctrl+o).
	ToggleDetailed KeyBindingDef `json:"toggleDetailed,omitempty"`
	// ToggleMouse toggles mouse capture release (default: ctrl+e).
	ToggleMouse KeyBindingDef `json:"toggleMouse,omitempty"`
	// CycleReasoning cycles through reasoning effort levels (default: ctrl+t).
	CycleReasoning KeyBindingDef `json:"cycleReasoning,omitempty"`
	// TogglePlan is retained for configuration compatibility. Plan updates render
	// in the transcript and no longer have a persistent panel to toggle.
	TogglePlan KeyBindingDef `json:"togglePlan,omitempty"`
	// ToggleSidebar toggles the right context sidebar (default: ctrl+b).
	ToggleSidebar KeyBindingDef `json:"toggleSidebar,omitempty"`
}

// STTProviderKind is the batch (or streaming) transcription backend selector.
// Validated at config load time against the known set, so a typo fails loudly
// with the valid options rather than silently falling back to a default.
type STTProviderKind string

const (
	STTProviderLocal    STTProviderKind = "local"
	STTProviderGroq     STTProviderKind = "groq"
	STTProviderOpenAI   STTProviderKind = "openai"
	STTProviderDeepgram STTProviderKind = "deepgram"
)

// SessionsConfig holds settings for Zero's saved sessions.
//
// READ FROM THE USER'S OWN CONFIG ONLY. It decides what `zero sessions prune`
// deletes, and sessions belong to the user rather than to a repository, so a
// project's .zero/config.json has no say: the resolver does not merge it, and
// the prune command reads it straight from the user config file.
type SessionsConfig struct {
	// RetentionDays is the cutoff `zero sessions prune` uses when it is given no
	// --older-than. Unset (0) means prune needs an explicit cutoff; nothing is
	// ever removed without one being asked for.
	RetentionDays int `json:"retentionDays,omitempty"`
}

// STTConfig configures speech-to-text dictation. All fields are optional; empty
// values take the documented defaults. Booleans that need a real tri-state
// (distinguishing "unset" from "false") use *bool, matching PreferencesConfig.Recaps.
type STTConfig struct {
	// Provider is the batch transcription backend: "local" (sherpa-onnx-offline,
	// the default), "groq", or "openai". Termux and the fallback everywhere use
	// this path.
	Provider STTProviderKind `json:"provider,omitempty"`
	// StreamProvider is the streaming backend on desktop: "local" (sherpa-onnx
	// websocket server, default), "deepgram", or "openai" (Realtime).
	StreamProvider STTProviderKind `json:"streamProvider,omitempty"`
	// Streaming enables the live-transcript pipeline on platforms that support
	// it (desktop). Defaults to on; set false to always use the batch pipeline.
	Streaming *bool `json:"streaming,omitempty"`
	// Model overrides the cloud batch model (e.g. "whisper-large-v3-turbo" for
	// Groq, "whisper-1" for OpenAI). Empty uses the provider default.
	Model string `json:"model,omitempty"`
	// StreamModel overrides the cloud streaming model (Deepgram/OpenAI Realtime).
	StreamModel string `json:"streamModel,omitempty"`
	// LocalModelPath is the sherpa-onnx model directory for local transcription
	// (batch and streaming). Required to use a local provider.
	LocalModelPath string `json:"localModelPath,omitempty"`
	// LocalBinary overrides the offline binary name/path (default
	// "sherpa-onnx-offline"); LocalServerBinary overrides the streaming server
	// (default "sherpa-onnx-online-websocket-server"). Both looked up on PATH.
	LocalBinary       string `json:"localBinary,omitempty"`
	LocalServerBinary string `json:"localServerBinary,omitempty"`
	// LocalServerPort is the localhost port for the sherpa-onnx websocket server
	// (default 6006).
	LocalServerPort int `json:"localServerPort,omitempty"`
	// EngineVersion selects the sherpa-onnx release the auto-download fetches
	// ("" → a pinned known-good default; "latest" or any release tag also works,
	// so a newer engine needs no Zero update). Verified against the release's
	// published SHA256 digest either way.
	EngineVersion string `json:"engineVersion,omitempty"`
	// NumThreads sets the local engine's thread count (0 = engine default).
	NumThreads int `json:"numThreads,omitempty"`
	// Language optionally constrains recognition (ISO-639-1, e.g. "en"). Empty =
	// auto-detect.
	Language string `json:"language,omitempty"`
	// MaxDurationSeconds is the hard runaway-recording cap on every platform
	// (0 = default 300s).
	MaxDurationSeconds int `json:"maxDurationSeconds,omitempty"`
	// SilenceAutoStop ends a recording ~2s after the signal goes quiet, as a
	// backstop for a forgotten manual stop (not VAD-triggered start). Defaults on.
	SilenceAutoStop *bool `json:"silenceAutoStop,omitempty"`
	// AutoSubmit fires the transcript at the agent instead of inserting it for
	// review. Defaults OFF — insert-for-review is the safety net for a misheard
	// prompt (§3).
	AutoSubmit *bool `json:"autoSubmit,omitempty"`
	// WindowsAudioDevice names the dshow capture device (auto-detected when empty).
	WindowsAudioDevice string `json:"windowsAudioDevice,omitempty"`
}

// STTProvider returns the configured batch provider, defaulting to local.
func (c STTConfig) STTProvider() STTProviderKind {
	if c.Provider == "" {
		return STTProviderLocal
	}
	return c.Provider
}

// STTStreamProvider returns the configured streaming provider, defaulting to local.
func (c STTConfig) STTStreamProvider() STTProviderKind {
	if c.StreamProvider == "" {
		return STTProviderLocal
	}
	return c.StreamProvider
}

// StreamingEnabled reports whether the live pipeline is on. Unset defaults ON.
func (c STTConfig) StreamingEnabled() bool {
	return c.Streaming == nil || *c.Streaming
}

// SilenceAutoStopEnabled reports whether trailing-silence auto-stop is on.
// Unset defaults ON.
func (c STTConfig) SilenceAutoStopEnabled() bool {
	return c.SilenceAutoStop == nil || *c.SilenceAutoStop
}

// AutoSubmitEnabled reports whether transcripts are auto-fired. Unset defaults OFF.
func (c STTConfig) AutoSubmitEnabled() bool {
	return c.AutoSubmit != nil && *c.AutoSubmit
}

// Empty reports whether the STT config carries no user-set values (so it can be
// omitted from a marshaled config).
func (c STTConfig) Empty() bool {
	return c.Provider == "" && c.StreamProvider == "" && c.Streaming == nil &&
		c.Model == "" && c.StreamModel == "" && c.LocalModelPath == "" &&
		c.LocalBinary == "" && c.LocalServerBinary == "" && c.LocalServerPort == 0 &&
		c.EngineVersion == "" && c.NumThreads == 0 && c.Language == "" &&
		c.MaxDurationSeconds == 0 && c.SilenceAutoStop == nil && c.AutoSubmit == nil &&
		c.WindowsAudioDevice == ""
}

// LocalControlConfig controls local browser/desktop/terminal automation helpers.
// Helpers are discovered lazily by the tool that needs them; no setup command or
// background probe is required during startup.
type LocalControlConfig struct {
	Enabled      bool                     `json:"enabled,omitempty"`
	Browser      LocalControlDriverConfig `json:"browser,omitempty"`
	Desktop      LocalControlDriverConfig `json:"desktop,omitempty"`
	Terminal     LocalControlDriverConfig `json:"terminal,omitempty"`
	ArtifactsDir string                   `json:"artifactsDir,omitempty"`
	enabledSet   bool
}

type LocalControlDriverConfig struct {
	Enabled    bool   `json:"enabled,omitempty"`
	HelperPath string `json:"helperPath,omitempty"`
	Driver     string `json:"driver,omitempty"`
	enabledSet bool
}

func (cfg LocalControlConfig) BrowserEnabled() bool {
	return !cfg.Disabled() && (!cfg.Browser.enabledSet || cfg.Browser.Enabled)
}

func (cfg LocalControlConfig) DesktopEnabled() bool {
	return !cfg.Disabled() && cfg.Desktop.enabledSet && cfg.Desktop.Enabled
}

func (cfg LocalControlConfig) TerminalEnabled() bool {
	return !cfg.Disabled() && (!cfg.Terminal.enabledSet || cfg.Terminal.Enabled)
}

func (cfg LocalControlConfig) Disabled() bool {
	return cfg.enabledSet && !cfg.Enabled
}

func (cfg LocalControlConfig) Empty() bool {
	return !cfg.Enabled &&
		!cfg.enabledSet &&
		cfg.ArtifactsDir == "" &&
		cfg.Browser.Empty() &&
		cfg.Desktop.Empty() &&
		cfg.Terminal.Empty()
}

func (cfg LocalControlDriverConfig) Empty() bool {
	return !cfg.Enabled &&
		!cfg.enabledSet &&
		cfg.HelperPath == "" &&
		cfg.Driver == ""
}

// SwarmConfig tunes the multi-agent swarm. MaxTeamSize caps how many members run
// concurrently per team; 0 uses the built-in default (8). Spawns past the cap
// queue and launch as slots free, so lowering it bounds parallelism (and provider
// load / rate-limit pressure) without dropping work.
type SwarmConfig struct {
	MaxTeamSize int `json:"maxTeamSize,omitempty"`
}

// ProfilesConfig gates the execution-profile catalog.
// PlanModelsConfig pins or forbids models for a plan's tasks. Every field is
// optional; an empty config leaves the automatic choice untouched.
type PlanModelsConfig struct {
	// Scan / Implement / Verify pin a model per task role. A pinned role skips
	// discovery entirely, so it works on a provider that reports no prices —
	// which is most of them.
	Scan      string `json:"scan,omitempty"`
	Implement string `json:"implement,omitempty"`
	Verify    string `json:"verify,omitempty"`
	// Router names the model that DECIDES which model runs each task, by reading
	// the task text instead of matching verbs. Empty uses the strongest model
	// discovery found. It costs one extra call on that model before the plan
	// starts, which is why a plan too small to benefit skips it and why every
	// way it can fail falls back to the keyword classifier.
	Router string `json:"router,omitempty"`
	// RouterGuidance is your own advice to the router, in plain words, added to
	// the built-in guidance rather than replacing it.
	//
	// It exists because the code cannot know your models. Price is the only
	// ordering discovery gives, and on real accounts it lies in both directions:
	// one provider's most expensive model was a build preview that failed every
	// task it touched, another reports no prices at all so the ranking collapses
	// to alphabetical. You know which model reasons well and which is quick and
	// shallow; this is where you say so, once, instead of per prompt.
	//
	// Example: "kimi-k2.6 is the best reasoner here — prefer it for judgements.
	// qwen3.5:397b is slow; use it only when correctness really matters."
	RouterGuidance string `json:"routerGuidance,omitempty"`
	// AutoAssign turns per-task model selection on for EVERY plan, so it does not
	// have to be asked for in each prompt.
	//
	// The tool argument still wins when a plan supplies one, so a plan can say
	// auto_assign:false and be believed. Absent from config it stays off: turning
	// it on changes which model does the work and what a plan costs, and that is
	// a decision for the person paying, made once here rather than inferred.
	AutoAssign bool `json:"autoAssign,omitempty"`
	// Exclude removes models from every tier by exact id. For the ones that are
	// eligible on paper and wrong in practice: an image model that ranks highest
	// on price, or a preview build that should not be judging anything.
	Exclude []string `json:"exclude,omitempty"`
	// MinSize is the smallest model, in BILLIONS of parameters, worth routing a
	// task to. A provider that lists tiny models (a 135M or 1B toy) alongside
	// decent ones would otherwise see the toy chosen as the "cheapest" tier for a
	// scan; this floor drops any model KNOWN to be smaller, so a task lands on a
	// decent model instead. Size is read from the model id ("qwen3-coder:480b").
	//
	// A model whose id names no size is NOT dropped — an unknown size is not
	// evidence of a small one, and most cloud ids carry none. And if the floor
	// would leave a provider with no models at all, it is ignored: a plan running
	// on a small model beats a plan that cannot run. 0 means no floor.
	MinSize float64 `json:"minSize,omitempty"`
	// TopModels caps how many of the most capable discovered models a plan may
	// route to, after exclusions and the size floor. A provider listing twenty
	// models otherwise puts its SMALLEST on the cheap tier and hands the router
	// twenty candidates; keeping the best ten narrows both to models worth a
	// sub-agent. Unset (0) is the built-in default of 10; raise it to widen the
	// field. A pin outside the list still applies — pins are validated against
	// what the provider serves, not against this shortlist.
	TopModels int `json:"topModels,omitempty"`
}

type ProfilesConfig struct {
	// DisableZeromaxing turns the zeromaxing posture off for this workspace, so
	// /effort zeromaxing, /profile zeromaxing and --exec-profile zeromaxing are
	// refused with a reason.
	//
	// Deliberately a DISABLE-only boolean, mirroring Sandbox.BlockUnixSockets
	// and the tighten-only Sandbox.Network rule: a project .zero/config.json may
	// set it true, but can never set it back to false, so a cloned repo cannot
	// switch a cost multiplier ON for whoever opens it. Only global user config
	// may leave it off. See mergeProjectConfig.
	DisableZeromaxing bool `json:"disableZeromaxing,omitempty"`
	// PlanSize is the zeromaxing plan-size tier: how many tasks one plan may
	// contain. One of small, medium (the default), large, unrestricted — see
	// plan_size.go for the numbers and for why this stays a hard cap rather than
	// an advisory warning.
	//
	// Project config may only TIGHTEN it, for the same reason DisableZeromaxing
	// is disable-only: a cloned repo must not be able to raise a cost ceiling for
	// whoever opens it. See mergeProjectConfig.
	PlanSize string `json:"planSize,omitempty"`
	// RequirePlanKeyword makes the orchestrate tool refuse a plan unless the
	// turn's own user text asks for one ("run a plan for ...", "fan out ...").
	//
	// Off by default, and that is a real trade. Once the posture is on the tool
	// exists for the rest of the session, and everything the model reads shares
	// context with the user's instructions — file contents, PR comments, MCP
	// output. An imperative sentence in any of them reads like an instruction to
	// the tool that spends the most. On by default would refuse a plan for every
	// user whose phrasing happens not to match, which is the call this codebase
	// already made for auto_assign.
	//
	// Project config may only ENABLE it, never switch it back off: turning a
	// safety gate off is a downgrade a cloned repo must not be able to make for
	// whoever opens it. See mergeProjectConfig.
	RequirePlanKeyword bool `json:"requirePlanKeyword,omitempty"`
	// Memory enables the durable note store: the `memory` and `memory_write`
	// tools, reading and writing .zero/memory.
	//
	// USER CONFIG ONLY. memory_write is a write primitive pointed at the
	// workspace, and project config is not trust-gated — a cloned repo must not
	// be able to hand the agent a new way to write into it. mergeProjectConfig
	// ignores this field entirely, the same answer PlanModels gets.
	Memory bool `json:"memory,omitempty"`
	// PlanModels states which models a plan's tasks may run on, overriding the
	// automatic choice auto_assign would make from provider discovery.
	//
	// It exists because the automatic choice ranks by PRICE, and price is a proxy
	// that fails in both directions on real accounts: an xAI account put a build
	// preview on the verify tier because it was the most expensive thing there,
	// and an Ollama account reports no prices at all, so the ranking collapses to
	// alphabetical order. Neither is fixable with a better heuristic — the person
	// with the account knows which model is strongest and the code does not.
	//
	// PROJECT CONFIG MAY ONLY EXCLUDE, never pin — see mergeProjectConfig. An
	// exclusion removes a candidate and can only lower what a plan spends; a pin
	// is the opposite, and a cloned repo pinning all three roles to the priciest
	// model on the account would raise cost for whoever opened it. That is the
	// same hazard PlanSize and DisableZeromaxing guard against.
	PlanModels PlanModelsConfig `json:"planModels,omitempty"`
}

func (cfg *ToolsConfig) UnmarshalJSON(data []byte) error {
	type rawTools struct {
		DeferThreshold *int `json:"deferThreshold"`
	}
	var raw rawTools
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	cfg.DeferThreshold = 0
	cfg.deferThresholdSet = false
	if raw.DeferThreshold != nil {
		cfg.DeferThreshold = *raw.DeferThreshold
		cfg.deferThresholdSet = true
	}
	return nil
}

type FileConfig struct {
	ModelRPM            map[string]int     `json:"modelRPM,omitempty"`
	ActiveProvider      string             `json:"activeProvider,omitempty"`
	Providers           []ProviderProfile  `json:"providers,omitempty"`
	MaxTurns            int                `json:"maxTurns,omitempty"`
	MCP                 MCPConfig          `json:"mcp,omitempty"`
	Sandbox             SandboxConfig      `json:"sandbox,omitempty"`
	Notify              NotifyConfig       `json:"notify,omitempty"`
	Tools               ToolsConfig        `json:"tools,omitempty"`
	Swarm               SwarmConfig        `json:"swarm,omitempty"`
	Profiles            ProfilesConfig     `json:"profiles,omitempty"`
	Preferences         PreferencesConfig  `json:"preferences,omitempty"`
	KeyBindings         KeyBindingsConfig  `json:"keybindings,omitempty"`
	LocalControl        LocalControlConfig `json:"localControl,omitempty"`
	STT                 STTConfig          `json:"stt,omitempty"`
	CrossSessionInbound string             `json:"crossSessionInbound,omitempty"`
	Sessions            SessionsConfig     `json:"sessions,omitempty"`
	// maxTurnsSet records that some merge source supplied a positive maxTurns —
	// i.e. the value is configured, not the built-in default. Unexported like
	// Tools.deferThresholdSet: merge bookkeeping, not a config key.
	maxTurnsSet bool
	// Extra preserves top-level fields written by newer Zero versions or
	// extensions so a read-modify-write through this version is non-destructive.
	Extra map[string]json.RawMessage `json:"-"`
}

func (cfg FileConfig) MarshalJSON() ([]byte, error) {
	type rawConfig struct {
		ModelRPM            map[string]int      `json:"modelRPM,omitempty"`
		ActiveProvider      string              `json:"activeProvider,omitempty"`
		Providers           []ProviderProfile   `json:"providers,omitempty"`
		MaxTurns            int                 `json:"maxTurns,omitempty"`
		MCP                 MCPConfig           `json:"mcp,omitempty"`
		Sandbox             SandboxConfig       `json:"sandbox,omitempty"`
		Notify              NotifyConfig        `json:"notify,omitempty"`
		Tools               ToolsConfig         `json:"tools,omitempty"`
		Swarm               SwarmConfig         `json:"swarm,omitempty"`
		Profiles            ProfilesConfig      `json:"profiles,omitempty"`
		Preferences         PreferencesConfig   `json:"preferences,omitempty"`
		KeyBindings         KeyBindingsConfig   `json:"keybindings,omitempty"`
		LocalControl        *LocalControlConfig `json:"localControl,omitempty"`
		STT                 *STTConfig          `json:"stt,omitempty"`
		CrossSessionInbound string              `json:"crossSessionInbound,omitempty"`
		Sessions            *SessionsConfig     `json:"sessions,omitempty"`
	}
	raw := rawConfig{
		ModelRPM:            cfg.ModelRPM,
		ActiveProvider:      cfg.ActiveProvider,
		Providers:           cfg.Providers,
		MaxTurns:            cfg.MaxTurns,
		MCP:                 cfg.MCP,
		Sandbox:             cfg.Sandbox,
		Notify:              cfg.Notify,
		Tools:               cfg.Tools,
		Swarm:               cfg.Swarm,
		Profiles:            cfg.Profiles,
		Preferences:         cfg.Preferences,
		KeyBindings:         cfg.KeyBindings,
		CrossSessionInbound: cfg.CrossSessionInbound,
	}
	if !cfg.LocalControl.Empty() {
		raw.LocalControl = &cfg.LocalControl
	}
	if !cfg.STT.Empty() {
		raw.STT = &cfg.STT
	}
	if cfg.Sessions != (SessionsConfig{}) {
		raw.Sessions = &cfg.Sessions
	}
	known, err := json.Marshal(raw)
	if err != nil || len(cfg.Extra) == 0 {
		return known, err
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(known, &merged); err != nil {
		return nil, err
	}
	for key, value := range cfg.Extra {
		if !fileConfigKnownJSONKey(key) {
			merged[key] = value
		}
	}
	return json.Marshal(merged)
}

func fileConfigKnownJSONKey(key string) bool {
	if strings.EqualFold(key, "mcpServers") || strings.EqualFold(key, "mcp_servers") {
		return true
	}
	for _, field := range knownJSONFields(reflect.TypeOf(FileConfig{})) {
		if strings.EqualFold(key, field.canonical) {
			return true
		}
	}
	return false
}

type ResolveOptions struct {
	UserConfigPath    string
	ProjectConfigPath string
	ProviderCommand   string
	Env               map[string]string
	Overrides         Overrides
	// ExcludeProject drops the project config layer (ProjectConfigPath) from MCP
	// resolution when the workspace is untrusted, so a cloned repo's ./.zero/config.json
	// cannot spawn stdio MCP servers. It is fail-closed: only a trusted workspace sets
	// it false. Mirrors the ExcludeProject option hooks and plugins already honor.
	ExcludeProject bool
}

type Overrides struct {
	ActiveProvider      string
	Providers           []ProviderProfile
	Provider            ProviderProfile
	MaxTurns            int
	MCP                 MCPConfig
	Sandbox             SandboxConfig
	Notify              NotifyConfig
	Tools               ToolsConfig
	KeyBindings         KeyBindingsConfig
	LocalControl        LocalControlConfig
	STT                 STTConfig
	CrossSessionInbound string
}

type ResolvedConfig struct {
	ModelRPM       map[string]int
	ActiveProvider string
	Providers      []ProviderProfile
	Provider       ProviderProfile
	MaxTurns       int
	// MaxTurnsSet reports whether MaxTurns came from an explicit source (a
	// config file, a provider command, ZERO_MAX_TURNS, or CLI overrides) rather
	// than the built-in default — callers that need a different default can
	// tell the two apart.
	MaxTurnsSet         bool
	MCP                 MCPConfig
	Sandbox             SandboxConfig
	Notify              NotifyConfig
	Tools               ToolsConfig
	Swarm               SwarmConfig
	Profiles            ProfilesConfig
	Preferences         PreferencesConfig
	KeyBindings         KeyBindingsConfig
	LocalControl        LocalControlConfig
	STT                 STTConfig
	CrossSessionInbound string
}

type MCPConfig struct {
	Servers map[string]MCPServerConfig `json:"servers,omitempty"`
}

type MCPServerConfig struct {
	Type     string            `json:"type,omitempty"`
	Command  string            `json:"command,omitempty"`
	Args     []string          `json:"args,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	URL      string            `json:"url,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Auth     string            `json:"auth,omitempty"`
	OAuth    *MCPOAuthConfig   `json:"oauth,omitempty"`
	Disabled bool              `json:"disabled,omitempty"`
	// ProjectConfigured marks servers touched by project config. It is runtime
	// metadata, not persisted config.
	ProjectConfigured bool `json:"-"`
	disabledSet       bool
	// configured is true when the user's config JSON declared an object for
	// this server at all (i.e. UnmarshalJSON ran for it), regardless of which
	// fields it set or what values they hold. A built-in default seeded by
	// DefaultMCPServers() is never unmarshaled from JSON, so it starts false;
	// any explicit entry in the user/project file — even one that happens to
	// repeat a default's exact field values (e.g. re-declaring Exa's
	// default URL) — sets it true. IsUnconfiguredDefault checks this alongside
	// a resolved-value comparison, so redeclaring default values verbatim still
	// counts as user-configured.
	configured bool
}

// MCPOAuthConfig describes how to authenticate to a remote MCP server using an
// OAuth 2.0 + PKCE authorization-code flow. Endpoints may be discovered from the
// authorization server's metadata document; explicit values here override or
// fill in anything discovery cannot provide. Client credentials are optional
// when the server supports dynamic client registration.
type MCPOAuthConfig struct {
	ClientID              string   `json:"clientID,omitempty"`
	ClientSecret          string   `json:"clientSecret,omitempty"`
	Scopes                []string `json:"scopes,omitempty"`
	AuthorizationEndpoint string   `json:"authorizationEndpoint,omitempty"`
	TokenEndpoint         string   `json:"tokenEndpoint,omitempty"`
	RegistrationEndpoint  string   `json:"registrationEndpoint,omitempty"`
	IssuerURL             string   `json:"issuerURL,omitempty"`
}

func (cfg *FileConfig) UnmarshalJSON(data []byte) error {
	type rawConfig struct {
		ModelRPM            map[string]int             `json:"modelRPM"`
		ActiveProvider      string                     `json:"activeProvider"`
		Providers           []ProviderProfile          `json:"providers"`
		MaxTurns            int                        `json:"maxTurns"`
		MCP                 MCPConfig                  `json:"mcp"`
		Sandbox             SandboxConfig              `json:"sandbox"`
		Notify              NotifyConfig               `json:"notify"`
		Tools               ToolsConfig                `json:"tools"`
		Swarm               SwarmConfig                `json:"swarm"`
		Profiles            ProfilesConfig             `json:"profiles"`
		Preferences         PreferencesConfig          `json:"preferences"`
		KeyBindings         KeyBindingsConfig          `json:"keybindings"`
		LocalControl        LocalControlConfig         `json:"localControl"`
		STT                 STTConfig                  `json:"stt"`
		CrossSessionInbound string                     `json:"crossSessionInbound"`
		Sessions            SessionsConfig             `json:"sessions"`
		MCPServers          map[string]MCPServerConfig `json:"mcpServers"`
		MCPServersSnake     map[string]MCPServerConfig `json:"mcp_servers"`
	}

	var raw rawConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var extra map[string]json.RawMessage
	if err := json.Unmarshal(data, &extra); err != nil {
		return err
	}
	for key := range extra {
		if fileConfigKnownJSONKey(key) {
			delete(extra, key)
		}
	}
	if len(extra) == 0 {
		extra = nil
	}
	cfg.ActiveProvider = raw.ActiveProvider
	limits, err := normalizeModelRPM(raw.ModelRPM)
	if err != nil {
		return err
	}
	cfg.ModelRPM = limits
	cfg.Providers = raw.Providers
	// A negative maxTurns is unambiguously invalid; without this it would be
	// silently dropped by the `MaxTurns > 0` merge gates and fall back to the
	// default, hiding a misconfiguration. (0 is left as-is: with omitempty it is
	// indistinguishable from "unset" and means "use the default".) The CLI flag
	// rejects 0 too because there an explicit "0" is distinguishable from absent.
	if raw.MaxTurns < 0 {
		return fmt.Errorf("invalid maxTurns %d: must be >= 0", raw.MaxTurns)
	}
	cfg.MaxTurns = raw.MaxTurns
	cfg.MCP = raw.MCP
	cfg.Sandbox = raw.Sandbox
	cfg.Notify = raw.Notify
	cfg.Tools = raw.Tools
	cfg.Swarm = raw.Swarm
	cfg.Profiles = raw.Profiles
	cfg.Preferences = raw.Preferences
	cfg.KeyBindings = raw.KeyBindings
	cfg.LocalControl = raw.LocalControl
	cfg.STT = raw.STT
	cfg.CrossSessionInbound = raw.CrossSessionInbound
	if raw.Sessions.RetentionDays < 0 {
		return fmt.Errorf("invalid sessions.retentionDays %d: must be >= 0", raw.Sessions.RetentionDays)
	}
	cfg.Sessions = raw.Sessions
	cfg.Extra = extra
	if cfg.MCP.Servers == nil && (len(raw.MCPServers) > 0 || len(raw.MCPServersSnake) > 0) {
		cfg.MCP.Servers = map[string]MCPServerConfig{}
	}
	for name, server := range raw.MCPServers {
		cfg.MCP.Servers[name] = server
	}
	for name, server := range raw.MCPServersSnake {
		if _, exists := cfg.MCP.Servers[name]; exists {
			return fmt.Errorf("MCP server %q is defined in both mcpServers and mcp_servers; mcp_servers would override mcpServers", name)
		}
		cfg.MCP.Servers[name] = server
	}
	return nil
}

func (cfg *LocalControlConfig) UnmarshalJSON(data []byte) error {
	type rawLocalControl struct {
		Enabled      *bool                    `json:"enabled"`
		Browser      LocalControlDriverConfig `json:"browser"`
		Desktop      LocalControlDriverConfig `json:"desktop"`
		Terminal     LocalControlDriverConfig `json:"terminal"`
		ArtifactsDir string                   `json:"artifactsDir"`
	}
	var raw rawLocalControl
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*cfg = LocalControlConfig{
		Browser:      raw.Browser,
		Desktop:      raw.Desktop,
		Terminal:     raw.Terminal,
		ArtifactsDir: raw.ArtifactsDir,
	}
	if raw.Enabled != nil {
		cfg.Enabled = *raw.Enabled
		cfg.enabledSet = true
	}
	return nil
}

func (cfg LocalControlConfig) MarshalJSON() ([]byte, error) {
	type rawLocalControl struct {
		Enabled      *bool                     `json:"enabled,omitempty"`
		Browser      *LocalControlDriverConfig `json:"browser,omitempty"`
		Desktop      *LocalControlDriverConfig `json:"desktop,omitempty"`
		Terminal     *LocalControlDriverConfig `json:"terminal,omitempty"`
		ArtifactsDir string                    `json:"artifactsDir,omitempty"`
	}
	raw := rawLocalControl{
		ArtifactsDir: cfg.ArtifactsDir,
	}
	if cfg.enabledSet || cfg.Enabled {
		raw.Enabled = &cfg.Enabled
	}
	if !cfg.Browser.Empty() {
		raw.Browser = &cfg.Browser
	}
	if !cfg.Desktop.Empty() {
		raw.Desktop = &cfg.Desktop
	}
	if !cfg.Terminal.Empty() {
		raw.Terminal = &cfg.Terminal
	}
	return json.Marshal(raw)
}

func (cfg *LocalControlDriverConfig) UnmarshalJSON(data []byte) error {
	type rawDriver struct {
		Enabled    *bool  `json:"enabled"`
		HelperPath string `json:"helperPath"`
		Driver     string `json:"driver"`
	}
	var raw rawDriver
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*cfg = LocalControlDriverConfig{
		HelperPath: raw.HelperPath,
		Driver:     raw.Driver,
	}
	if raw.Enabled != nil {
		cfg.Enabled = *raw.Enabled
		cfg.enabledSet = true
	}
	return nil
}

func (cfg LocalControlDriverConfig) MarshalJSON() ([]byte, error) {
	type rawDriver struct {
		Enabled    *bool  `json:"enabled,omitempty"`
		HelperPath string `json:"helperPath,omitempty"`
		Driver     string `json:"driver,omitempty"`
	}
	raw := rawDriver{
		HelperPath: cfg.HelperPath,
		Driver:     cfg.Driver,
	}
	if cfg.enabledSet || cfg.Enabled {
		raw.Enabled = &cfg.Enabled
	}
	return json.Marshal(raw)
}

// MarshalJSON writes "disabled" whenever it was explicitly set, not only when
// true. A read-modify-write of config.json (SetTheme, SetActiveProvider, ...)
// would otherwise drop `"disabled": false`, which turns a default that ships
// disabled (aimlapi) back off and lets project config override a user's
// explicit enable (see mergeMCPServer).
func (server MCPServerConfig) MarshalJSON() ([]byte, error) {
	type rawServer struct {
		Type     string            `json:"type,omitempty"`
		Command  string            `json:"command,omitempty"`
		Args     []string          `json:"args,omitempty"`
		Env      map[string]string `json:"env,omitempty"`
		URL      string            `json:"url,omitempty"`
		Headers  map[string]string `json:"headers,omitempty"`
		Auth     string            `json:"auth,omitempty"`
		OAuth    *MCPOAuthConfig   `json:"oauth,omitempty"`
		Disabled *bool             `json:"disabled,omitempty"`
	}
	raw := rawServer{
		Type:    server.Type,
		Command: server.Command,
		Args:    server.Args,
		Env:     server.Env,
		URL:     server.URL,
		Headers: server.Headers,
		Auth:    server.Auth,
		OAuth:   server.OAuth,
	}
	if server.disabledSet || server.Disabled {
		raw.Disabled = &server.Disabled
	}
	return json.Marshal(raw)
}

func (server *MCPServerConfig) UnmarshalJSON(data []byte) error {
	type rawServer struct {
		Type     string            `json:"type"`
		Command  string            `json:"command"`
		Args     []string          `json:"args"`
		Env      map[string]string `json:"env"`
		URL      string            `json:"url"`
		Headers  map[string]string `json:"headers"`
		Auth     string            `json:"auth"`
		OAuth    *MCPOAuthConfig   `json:"oauth"`
		Disabled *bool             `json:"disabled"`
	}

	var raw rawServer
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	server.Type = raw.Type
	server.Command = raw.Command
	server.Args = raw.Args
	server.Env = raw.Env
	server.URL = raw.URL
	server.Headers = raw.Headers
	server.Auth = raw.Auth
	server.OAuth = raw.OAuth
	server.Disabled = false
	server.disabledSet = false
	if raw.Disabled != nil {
		server.Disabled = *raw.Disabled
		server.disabledSet = true
	}
	// This method only runs when the user's JSON actually has an object for this
	// server key (built-in defaults are seeded as Go struct literals, never
	// unmarshaled), so reaching here at all means the user configured it.
	server.configured = true
	return nil
}

func (profile *ProviderProfile) UnmarshalJSON(data []byte) error {
	type rawProfile struct {
		Name                 string            `json:"name"`
		Provider             string            `json:"provider"`
		ProviderKind         string            `json:"provider_kind"`
		ProviderKindCamel    string            `json:"providerKind"`
		CatalogID            string            `json:"catalogID"`
		CatalogIDSnake       string            `json:"catalog_id"`
		BaseURL              string            `json:"baseURL"`
		BaseURLSnake         string            `json:"base_url"`
		APIKey               string            `json:"apiKey"`
		APIKeySnake          string            `json:"api_key"`
		APIKeyEnv            string            `json:"apiKeyEnv"`
		APIKeyEnvSnake       string            `json:"api_key_env"`
		APIKeyStored         bool              `json:"apiKeyStored"`
		APIKeyStoredSnake    bool              `json:"api_key_stored"`
		APIFormat            string            `json:"apiFormat"`
		APIFormatSnake       string            `json:"api_format"`
		AuthHeader           string            `json:"authHeader"`
		AuthHeaderSnake      string            `json:"auth_header"`
		AuthScheme           string            `json:"authScheme"`
		AuthSchemeSnake      string            `json:"auth_scheme"`
		AuthHeaderValue      string            `json:"authHeaderValue"`
		AuthHeaderValueSnake string            `json:"auth_header_value"`
		CustomHeaders        map[string]string `json:"customHeaders"`
		CustomHeadersSnake   map[string]string `json:"custom_headers"`
		Model                string            `json:"model"`
		ModelID              string            `json:"model_id"`
		ParseThinkTags       *bool             `json:"parseThinkTags"`
		ParseThinkTagsSnake  *bool             `json:"parse_think_tags"`
		Description          string            `json:"description"`
	}

	var raw rawProfile
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	profile.Name = strings.TrimSpace(raw.Name)
	profile.Provider = strings.TrimSpace(raw.Provider)
	profile.ProviderKind = ProviderKind(firstNonEmpty(raw.ProviderKind, raw.ProviderKindCamel, raw.Provider))
	profile.CatalogID = strings.TrimSpace(firstNonEmpty(raw.CatalogID, raw.CatalogIDSnake))
	profile.BaseURL = strings.TrimSpace(firstNonEmpty(raw.BaseURL, raw.BaseURLSnake))
	profile.APIKey = firstNonEmpty(raw.APIKey, raw.APIKeySnake)
	profile.APIKeyEnv = strings.TrimSpace(firstNonEmpty(raw.APIKeyEnv, raw.APIKeyEnvSnake))
	profile.APIKeyStored = raw.APIKeyStored || raw.APIKeyStoredSnake
	profile.APIFormat = strings.TrimSpace(firstNonEmpty(raw.APIFormat, raw.APIFormatSnake))
	profile.AuthHeader = strings.TrimSpace(firstNonEmpty(raw.AuthHeader, raw.AuthHeaderSnake))
	profile.AuthScheme = strings.TrimSpace(firstNonEmpty(raw.AuthScheme, raw.AuthSchemeSnake))
	profile.AuthHeaderValue = firstNonEmpty(raw.AuthHeaderValue, raw.AuthHeaderValueSnake)
	profile.CustomHeaders = firstNonNilMap(raw.CustomHeaders, raw.CustomHeadersSnake)
	profile.Model = strings.TrimSpace(firstNonEmpty(raw.Model, raw.ModelID))
	profile.ParseThinkTags = firstNonNilBool(raw.ParseThinkTags, raw.ParseThinkTagsSnake)
	profile.Description = strings.TrimSpace(raw.Description)
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func firstNonNilMap(values ...map[string]string) map[string]string {
	for _, value := range values {
		if value != nil {
			return copyStringMap(value)
		}
	}
	return nil
}

func firstNonNilBool(values ...*bool) *bool {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func copyStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	copied := make(map[string]string, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return copied
}
