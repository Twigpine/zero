package tui

import (
	"context"

	tea "charm.land/bubbletea/v2"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/mcp"
	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/peermsg"
	"github.com/Gitlawb/zero/internal/providerhealth"
	"github.com/Gitlawb/zero/internal/providermodeldiscovery"
	"github.com/Gitlawb/zero/internal/sandbox"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/skills"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/usage"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// Options configures the reusable Zero terminal UI shell.
type Options struct {
	Cwd                  string
	Version              string // CLI build version, shown on the home screen; empty hides it
	UserConfigPath       string
	DoctorUserConfigPath string
	ProjectConfigPath    string
	ProviderName         string
	ModelName            string
	ProviderProfile      config.ProviderProfile
	SavedProviders       []config.ProviderProfile // all configured providers, for the /model multi-provider list
	FavoriteModels       []string
	RecentModels         []config.RecentModelEntry
	RecapsEnabled        bool
	// CompactionModel is the resolved preferences.compactionModel value; see
	// providers.CompactionModelID for how it combines with the env override
	// and the curated cheap defaults.
	CompactionModel             string
	Provider                    zeroruntime.Provider
	NewProvider                 func(config.ProviderProfile) (zeroruntime.Provider, error)
	NewTurnSessionProvider      func(config.ProviderProfile, zeroruntime.Provider) zeroruntime.TurnSessionProvider
	ProbeProviderHealth         func(context.Context, providerhealth.Options) providerhealth.Result
	DiscoverProviderModels      func(context.Context, config.ProviderProfile) ([]providermodeldiscovery.Model, error)
	DiscoverOllamaContextWindow func(ctx context.Context, baseURL string, model string) (int, error)
	RuntimeMessageSink          func(tea.Msg)
	PrepareRunCompletionWarning func()
	RunCompletionWarning        func() string
	Registry                    *tools.Registry
	// AwaitToolReadiness gives prompt-critical integration startup a bounded
	// chance to publish its tools before this turn snapshots the registry. The
	// wait runs inside the asynchronous agent command, so the TUI stays usable.
	AwaitToolReadiness  func(context.Context)
	SessionStore        *sessions.Store
	SandboxStore        *sandbox.GrantStore
	MCPConfig           config.MCPConfig
	MCPPermissionStore  *mcp.PermissionStore
	MCPTokenStore       *mcp.TokenStore
	MCPCommand          func(context.Context, []string) MCPCommandResult
	SandboxSetupCommand func(context.Context) SandboxSetupCommandResult
	UsageTracker        *usage.Tracker
	SessionCompactor    SessionCompactor
	PrService           *PrService
	PeerService         *peermsg.Service

	AgentOptions agent.Options
	// LoadSkills returns the installed skills (default skills dir merged with any
	// plugin skill roots), bodies included, for /skills and direct /<skill-name>
	// invocation. Called lazily per use so newly installed skills are picked up
	// without a restart. Nil means the session has no skills wiring (skills stay
	// model-pulled via the skill tool only).
	LoadSkills func() []skills.Skill
	// AllowEscalation opts this session into mid-run model escalation, set from
	// --allow-escalation. It wires the model switchers onto every turn's options;
	// the escalate_model tool itself is registered by the caller on the same flag.
	// Both halves are required: the tool without the switchers is inert, and the
	// switchers without the tool are unreachable.
	AllowEscalation bool
	PermissionMode  agent.PermissionMode
	ReasoningEffort modelregistry.ReasoningEffort
	ResponseStyle   string
	// Theme is the operator's palette preference: "auto" (default), a built-in
	// ("dark"/"light"), or a registered color theme. Set from the --theme flag;
	// falls back to ZERO_THEME, then the persisted SavedTheme, then auto.
	Theme string
	// SavedTheme is the theme persisted in user config (Preferences.Theme). Applied
	// at startup below --theme and ZERO_THEME, so a /theme choice survives restart.
	SavedTheme string
	// SavedPet is the persisted terminal companion id. Empty means no pet has
	// been selected yet; "disabled" records an explicit opt-out.
	SavedPet  string
	UserAgent string

	// Notify configures completion / awaiting-input notifications.
	Notify config.NotifyConfig

	// KeyBindings configures remappable TUI keybindings. An empty/zero
	// KeyBindingsConfig means "use built-in defaults" for each action.
	KeyBindings config.KeyBindingsConfig

	// STT configures speech-to-text dictation (§ docs/dictation.md).
	STT config.STTConfig
	// BuildDictationTranscriber constructs the transcriber for the current STT
	// config. It lives in the CLI layer because it resolves provider API keys
	// (credstore + env) and base URLs; the TUI only calls it when a recording
	// starts. preferStreaming asks for the streaming backend; the returned
	// `streaming` bool reports whether streaming is actually available (false
	// falls the caller back to the batch pipeline). Nil disables dictation (the
	// keybinding shows a "not configured" hint). cfg is passed each call (not
	// captured) so a mid-session config change — e.g. the auto-download writing
	// localModelPath — takes effect on the next recording.
	BuildDictationTranscriber func(cfg config.STTConfig, preferStreaming bool) (t Transcriber, streaming bool, err error)

	// ShutdownDictationServer tears down the warm sherpa-onnx streaming server (if
	// one was started), called alongside the LSP manager's shutdown on exit. Nil
	// when dictation is not wired.
	ShutdownDictationServer func(context.Context) error

	// STTDownloadRoot is where the auto-download stores the sherpa-onnx engine
	// and model (e.g. ~/.config/zero/stt). Empty disables auto-download (the F9
	// setup message then only points at manual setup / cloud providers).
	STTDownloadRoot string

	// STTKeyStatus reports whether an API key is already resolvable for a cloud
	// STT provider ("groq"/"openai"/"deepgram"). Nil disables the inline key
	// prompt (dictation then just shows the "run zero auth" setup error).
	STTKeyStatus func(provider string) bool
	// SaveSTTKey stores an API key for a cloud STT provider in the credential
	// store, so the inline prompt can capture and persist it.
	SaveSTTKey func(provider, key string) error

	// AltScreen tells the model it is running inside Bubble Tea's alternate
	// screen. Run sets this for the interactive app; tests can leave it false
	// to exercise the native scrollback renderer.
	AltScreen bool

	// Setup configures the first-run/setup takeover. It is shown before the
	// normal chat surface when Visible is true.
	Setup SetupOptions
}

type MCPCommandResult struct {
	Config   config.MCPConfig
	Output   string
	Error    string
	ExitCode int
}

type SandboxSetupCommandResult struct {
	Output   string
	Error    string
	ExitCode int
}

// SetupOptions configures the guided first-run provider setup takeover.
type SetupOptions struct {
	Visible    bool
	Required   bool
	ConfigPath string
	Providers  []SetupProviderOption
	Save       func(SetupSelection) (SetupResult, error)
}

// SetupProviderOption is one provider choice offered by the setup takeover.
type SetupProviderOption struct {
	ID           string
	Name         string
	DefaultModel string
	EnvVar       string
	RequiresAuth bool
	Local        bool
	Recommended  bool
}

// SetupSelection is the user's setup choice.
type SetupSelection struct {
	CatalogID string
	Name      string
	BaseURL   string
	Model     string
	APIKey    string
}

// SetupResult describes a completed setup write.
type SetupResult struct {
	ConfigPath string
	Provider   config.ProviderProfile
}
