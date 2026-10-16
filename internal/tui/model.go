package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/doctor"
	"github.com/Gitlawb/zero/internal/errhint"
	"github.com/Gitlawb/zero/internal/lsp"
	internalmcp "github.com/Gitlawb/zero/internal/mcp"
	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/notify"
	"github.com/Gitlawb/zero/internal/peermsg"
	"github.com/Gitlawb/zero/internal/providerhealth"
	"github.com/Gitlawb/zero/internal/providermodeldiscovery"
	"github.com/Gitlawb/zero/internal/providers"
	"github.com/Gitlawb/zero/internal/providers/providerio"
	"github.com/Gitlawb/zero/internal/sandbox"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/skills"
	"github.com/Gitlawb/zero/internal/streamjson"
	"github.com/Gitlawb/zero/internal/terminalpet"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/usage"
	"github.com/Gitlawb/zero/internal/usercommands"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

const tuiToolOutputLimit = 240
const defaultResponseStyle = "concise"
const chatWheelScrollLines = 5

// activeAnimationFrameInterval keeps active-only status motion smooth without
// running a timer when Zero is idle. It also drives the shared liveness spinner.
const activeAnimationFrameInterval = time.Second / 30
const ctrlCExitConfirmDuration = 3 * time.Second
const ctrlCExitConfirmText = "Press Ctrl+C again to exit"

// escCancelConfirmDuration/escCancelConfirmText guard Esc cancelling a running
// turn, mirroring ctrlCExitConfirmDuration/ctrlCExitConfirmText's pattern (same
// window, same amber footer treatment) so the two "stop it" keybinds feel
// consistent. Without this, a single stray Esc — e.g. meant to dismiss a
// suggestion overlay that had already closed — silently threw away an
// in-progress run with no chance to reconsider.
const escCancelConfirmDuration = 3 * time.Second
const escCancelConfirmText = "Press Esc again to cancel"

// dragEdgeScrollInterval/dragEdgeScrollStep drive the smooth-glide auto-scroll
// while a drag holds past the transcript edge (see edgeScrollDelta). A small step
// on a short, steady cadence reads as a smooth continuous scroll; the wheel-scroll
// tick size (chatWheelScrollLines) would jump too far per step for that.
const dragEdgeScrollInterval = 70 * time.Millisecond
const dragEdgeScrollStep = 1

type model struct {
	ctx                  context.Context
	cwd                  string
	appVersion           string
	startupNotices       []string               // Options.StartupNotices, said again when a session is resumed
	userCommands         []usercommands.Command // file-sourced /commands (.zero/commands)
	loadSkills           func() []skills.Skill  // lazy installed-skills loader for /skills + /<skill-name>
	userConfigPath       string
	doctorUserConfigPath string
	projectConfigPath    string
	gitBranch            string
	providerName         string
	modelName            string
	modelCatalog         modelregistry.Registry

	// removedLiveRow retains ownership while a deleted row's client keeps running.
	removedLiveRow  string
	providerProfile config.ProviderProfile
	savedProviders  []config.ProviderProfile
	provider        zeroruntime.Provider
	// allowEscalation mirrors Options.AllowEscalation: it gates the per-run model
	// switchers, and the caller gates the escalate_model tool on the same flag.
	allowEscalation             bool
	modelRPM                    *zeroruntime.ModelRPMLimiter
	newProvider                 func(config.ProviderProfile) (zeroruntime.Provider, error)
	newTurnSessionProvider      func(config.ProviderProfile, zeroruntime.Provider) zeroruntime.TurnSessionProvider
	probeProviderHealth         func(context.Context, providerhealth.Options) providerhealth.Result
	discoverProviderModels      func(context.Context, config.ProviderProfile) ([]providermodeldiscovery.Model, error)
	discoverOllamaContextWindow func(ctx context.Context, baseURL string, model string) (int, error)
	deleteProviderKey           func(configPath, provider string) (bool, error)
	clearProviderKeyStored      func(configPath, provider string) (bool, error)
	registry                    *tools.Registry
	awaitToolReadiness          func(context.Context)
	// lspManager is created once per session and reused across prompts so gopls (and
	// other language servers) stay warm — a fresh manager per run would cold-start
	// the server on the first edit of every turn. Nil when cwd is unknown; runs then
	// fall back to a per-run manager. Torn down in quit().
	lspManager           *lsp.Manager
	sessionStore         *sessions.Store
	peerService          *peermsg.Service
	peerInbox            []peermsg.InboundMessage
	peerApprovalQueue    []peermsg.InboundMessage
	peerPendingApproval  *peermsg.InboundMessage
	sandboxStore         *sandbox.GrantStore
	mcpConfig            config.MCPConfig
	mcpPermissionStore   *internalmcp.PermissionStore
	mcpTokenStore        *internalmcp.TokenStore
	mcpCommand           func(context.Context, []string) MCPCommandResult
	sandboxSetupCommand  func(context.Context) SandboxSetupCommandResult
	mcpViewStateCache    MCPViewState
	mcpViewStateReady    bool
	mcpCommandSeq        int
	mcpCommandCancel     context.CancelFunc
	sandboxSetupSeq      int
	sandboxSetupInFlight bool
	doctorCommandSeq     int
	doctorInFlight       bool
	doctorFrame          int
	activeSession        sessions.Metadata
	pendingSessionTitle  string
	sessionEvents        []sessions.Event
	btw                  btwState
	// btwRunIDSeq is the highest run ID issued by any completed or abandoned BTW
	// surface. It survives returning to the parent so a late message from an old
	// side run can never match a run in a later BTW conversation.
	btwRunIDSeq int
	// titledSessions records session ids for which a model-generated title has
	// already been attempted this process, so a finished turn re-fires the title
	// generator at most once per session (even before its async result lands).
	// Lazily initialized.
	titledSessions              map[string]bool
	renamePrompt                *sessionRenamePrompt
	usageTracker                *usage.Tracker
	sessionCompactor            SessionCompactor
	prService                   *PrService
	prState                     PrState
	prWatcherStop               func()
	runtimeMessageSink          func(tea.Msg)
	prepareRunCompletionWarning func()
	runCompletionWarning        func() string
	agentOptions                agent.Options
	notifier                    *notify.Notifier
	permissionMode              agent.PermissionMode
	// permissionModeBeforePlan holds whatever mode was active when /plan on
	// entered PermissionModePlan, so /plan off can restore it exactly (mirrors
	// the execProfile displaced/applied pattern below).
	permissionModeBeforePlan agent.PermissionMode
	selfCorrectTests         bool
	reasoningEffort          modelregistry.ReasoningEffort
	serviceTier              string
	// Active execution profile (set by /profile; applies to the NEXT run).
	// The displaced/applied pairs let a switch or /profile balanced restore
	// exactly what the profile replaced while leaving later manual overrides
	// (/turns, Ctrl+T, /selfcorrect) alone: each knob is only reverted when it
	// still holds the value the profile applied.
	execProfileName              string
	execProfileDisplacedMaxTurns int
	execProfileAppliedMaxTurns   int
	execProfileAppliedEffort     modelregistry.ReasoningEffort
	execProfileArmedSelfCorrect  bool
	// The touched bits record explicit user choices made while a profile is
	// active (/turns, /effort, Ctrl+T, /selfcorrect); a touched knob is never
	// reverted, even when its value coincides with what the profile applied.
	execProfileTurnsTouched       bool
	execProfileEffortTouched      bool
	execProfileSelfCorrectTouched bool
	responseStyle                 string
	petClient                     *terminalpet.Client
	petRenderer                   *terminalpet.ImageRenderer
	attachmentRenderers           []*terminalpet.ImageRenderer
	petEntries                    map[string]terminalpet.Entry
	petID                         string
	petName                       string
	petAnimation                  *terminalpet.Animation
	petPreview                    *terminalpet.Animation
	petPreviewSlug                string
	petPreviewError               string
	petPreviewLoading             bool
	petPreviewSeq                 uint64
	petPreviewCancel              context.CancelFunc
	petRequestedSlug              string
	petPhase                      int
	petTickSeq                    uint64
	petPlaybackState              terminalpet.State
	petClickAnimationIndex        int
	petOutcome                    terminalpet.State
	petOutcomeAt                  time.Time
	petLayoutRendering            bool
	petPositionSet                bool
	petPositionX                  int
	petPositionY                  int
	petDragActive                 bool
	petDragMoved                  bool
	petDragStartedDocked          bool
	petDragOffsetX                int
	petDragOffsetY                int
	petDragTargetX                int
	petDragTargetY                int
	petDragTargetOffsetX          int
	petDragTargetOffsetY          int
	petPositionOffsetX            int
	petPositionOffsetY            int
	petCellPixelWidth             int
	petCellPixelHeight            int
	petPixelDrag                  bool
	petPixelAnchorSet             bool
	petDragOffsetPixelX           int
	petDragOffsetPixelY           int
	petDragState                  terminalpet.State
	petLastClickAt                time.Time
	keyBindings                   keyBindings
	themeMode                     themeMode // palette preference: system (default) or named palette
	hasDarkBg                     bool      // last terminal background-detection result, if one is delivered
	// notifyMode and notifyFocusMode track the user's in-session notify
	// preference (from options.Notify, updated by /notify). The notifier built
	// at startup is independent, so changes apply on the NEXT permission prompt.
	notifyMode string
	// notifyConfiguredMode holds the RAW configured mode ("" when the user
	// never set one) so state output can label the effective default as
	// "(default)" — mirroring the CLI's `mode: (default)` for the same state
	// (maintainer review, PR #1001). Cleared once the user picks a mode.
	notifyConfiguredMode string
	notifyFocusMode      string
	userAgent            string
	compactRequests      int
	compactInFlight      bool
	compactFrame         int
	lastCompactResult    *CompactResult
	lastCompactError     string
	unpricedRequests     int
	unpricedTokens       int
	lastUsage            usage.Normalized
	lastUsageSeen        bool
	// turnLatencySum / turnLatencyCount accumulate completed-run wall time so
	// /context can show a rolling average turn latency (the "is it slow?" signal).
	// Reset by /new.
	turnLatencySum     time.Duration
	turnLatencyCount   int
	turnTTFTSum        time.Duration
	turnTTFTCount      int
	transcript         []transcriptRow
	transcriptDetailed bool
	helpOverlay        bool // the `?` keyboard-shortcut overlay is open
	// leaderHelpOverlay is the Ctrl+X ? modal listing every leader slash chord.
	leaderHelpOverlay bool
	// leaderPending is true after Ctrl+X until a second key, Esc, or timeout
	// resolves the chord (see leader.go). leaderSeq invalidates a stale tick.
	leaderPending          bool
	leaderSeq              int
	transcriptBodyHeights  *transcriptBodyHeightCache
	input                  textinput.Model
	composer               composerState
	composerActive         bool
	composerCursorVisible  bool
	composerBlinkSeq       int
	composerBlinkIdleTicks int
	composerBlinkTicking   bool
	composerPastePreviews  []composerPastePreview
	composerSelection      composerSelectionState
	dictation              dictationController
	sttKeyPrompt           *sttKeyPromptState
	terminalAttach         *terminalAttachState
	terminalAutoAttach     *terminalAutoAttachState
	terminalAttachSeen     map[int]bool
	// plan holds the sticky plan panel state (steps, expansion, timings)
	// synced from the update_plan tool. See plan_panel.go.
	plan            planPanelState
	specialists     specialistTracker
	stepWork        map[string][]planStepWork // file mutations + commands captured per in_progress plan step, for the clickable step detail
	stepNarration   map[string][]string       // the agent's own prose narration captured per in_progress plan step, for the step detail's explanation
	planDetailOpen  bool                      // a plan-step detail card is currently shown (click-to-toggle)
	planDetailStep  int                       // which step index the shown detail card is for
	planDetailGen   int                       // bumped each run; an in-flight explanation result from an older gen is dropped
	stepExplanation map[string]string         // model-written step write-ups, keyed by planStepExplanationKey, cached so re-clicking is instant
	subchat         subchatState
	altScreen       bool
	setup           setupState
	setupSave       func(SetupSelection) (SetupResult, error)
	// spinner animates the turn-level activity glyph. Its tick is started with
	// each run and stops itself once pending clears (the TickMsg is simply not
	// forwarded), so an idle UI schedules no timers.
	spinner spinner.Model
	// spinnerPhase advances once per spinner tick while a run is in flight. It
	// drives only bounded secondary motion such as the streaming caret and agent
	// lifecycle fades; it never creates a second live spinner.
	spinnerPhase int
	// spinnerTicking tracks whether the spinner's self-scheduling tick loop is
	// currently alive, so a kick (ensureSpinnerTick) never double-issues the tick
	// when the loop is already running. Set true whenever a Tick cmd is returned
	// from the TickMsg handler / beginRun, cleared when the handler stops the loop.
	spinnerTicking bool
	pending        bool
	// turnStartedAt is when the in-flight run began; the working status line
	// renders the live elapsed time from it so a long or stalled turn never looks
	// like a frozen terminal (for ANY provider, not just slow ones). Zero = idle.
	turnStartedAt time.Time
	// turnTimer is shared with the agent command so both the live status and the
	// settled "worked for" duration exclude time blocked on a user permission.
	turnTimer *activeTurnTimer
	// lastCharTime tracks when the last non-Enter key was received, for paste detection.
	lastCharTime time.Time
	// lastKeyTime tracks every keypress timestamp for burst calculation.
	lastKeyTime time.Time
	// burstCount counts consecutive keypresses within 100ms (paste mode).
	burstCount int
	// terminalFocused tracks whether the terminal window currently has focus, per
	// tea.FocusMsg/tea.BlurMsg. Defaults to true since many terminals/multiplexers
	// never send focus events at all, and defaulting to "unfocused" would wrongly
	// hide the cursor for those users.
	terminalFocused bool
	queuedMessage   string
	// loops holds the session's active /loop definitions (see loop.go). activeLoopID
	// tags the in-flight run when it is a loop iteration (empty = a user turn), so the
	// completion seam knows whether to advance a loop. loopSeq invalidates a stale
	// pending poll tick when loops are stopped; loopTicking guards against scheduling
	// a second poll ticker. loopLeavePrompt arms a one-shot confirm before /clear or
	// /quit while loops are active (see handleSubmit).
	loops           []*loopState
	activeLoopID    string
	loopSeq         int
	loopCounter     int
	loopTicking     bool
	loopLeavePrompt commandKind
	// goalContinuationsSuspended keeps a hidden parent from launching autonomous
	// work while the user is in an isolated BTW conversation.
	goalContinuationsSuspended bool
	exiting                    bool
	runCancel                  context.CancelFunc
	runID                      int
	activeRunID                int
	// flushRunIDs holds the ids of runs cancelled while still in flight, mapped
	// to the session they were recording into AT CANCEL TIME. Each cancelled
	// agent goroutine keeps running to completion and returns its accumulated
	// sessionEvents (including EventSessionCheckpoint payloads captured before
	// each mutating tool) in a final agentResponseMsg. activeRunID is already
	// zeroed by then, so without this the message would be dropped and the
	// checkpoint blobs already written to disk would be orphaned (breaking
	// /rewind). It is a MAP (not a single id) so a second cancel before the
	// first goroutine returns doesn't overwrite/lose the first run's pending
	// flush; the recorded session id keeps the late flush out of whatever
	// session is active by then (e.g. after /resume), which would otherwise
	// contaminate the new session's log with the old run's events. The
	// agentResponseMsg handler persists each such run's session events (only) so
	// the checkpoints stay referenced, then removes the id.
	flushRunIDs     map[int]string
	liveUsageCounts map[int]int
	// swarmSessionMap maps a swarm task id to its member's durable child session
	// id (carried up by swarm_collect's Meta), so the AGENTS sidebar rows can drill
	// into a member's conversation. Persists across turns; only completed members
	// have an entry.
	swarmSessionMap   map[string]string
	pendingPermission *pendingPermissionPrompt
	pendingAskUser    *pendingAskUserPrompt
	pendingSpecReview *pendingSpecReviewPrompt
	width             int
	height            int
	// runDetailsOpen keeps the optional run summary focused without permanently
	// shrinking the conversation surface.
	runDetailsOpen bool
	sidebarHidden  bool
	// selectedFile is the touched file selected from a file summary: its edit
	// cards tint in the chat (rowTouchesSelectedFile) and a second click opens
	// the drill-in file view. "" when nothing is selected; Esc clears.
	selectedFile string
	// fileView is the drill-in view for a touched file (file_view.go): while
	// active the chat column's body shows the file's diff/content instead of the
	// transcript, mirroring the subchat drill-in.
	fileView fileViewState
	// Git-sweep state (files_git_sweep.go): the startup snapshot of already-dirty
	// paths (nil until Init's sweep answers), the newly dirty files discovered by
	// live sweeps (bash/subagent mutations that carry no changedFiles), the
	// single-flight guard, and the "not a git repo / no git" latch.
	gitFileBaseline     map[string]bool
	gitTouched          []gitSweepFile
	gitSweepInFlight    bool
	gitSweepUnavailable bool
	// swarmDoneAt records when each swarm member was first seen finished (done/
	// failed) in a swarm_status report, so the sidebar can linger it briefly with a
	// fading ✓ before dropping it (a smooth exit, not an abrupt pop). Stamped in the
	// spinner tick; keyed by member id. Always non-nil (initialised in newModel).
	swarmDoneAt      map[string]time.Time
	now              func() time.Time
	chatScrollOffset int
	// chatBodyLines is the live body's line count at the last update; used to pin
	// the viewport (hold the read position) when content streams in while the user
	// has scrolled up. 0 means "at the bottom / not pinned".
	chatBodyLines int

	// Flush-frontier state (see flush.go). In inline mode, transcript[:flushed]
	// is already in native scrollback. Alt-screen mode advances the same
	// frontier, but keeps the settled prefix as cached body items so fullscreen
	// scrolling still exposes the complete transcript without rebuilding it on
	// every frame.
	// flushedAny gates the first turn-separator blank line; flushQueue/
	// printInFlight serialize ordered scrollback prints; headerPrinted records
	// the one-time inline title-bar print at startup.
	flushed                  int
	flushedAny               bool
	flushedPreviousKind      rowKind
	flushedHavePreviousKind  bool
	flushQueue               []string
	printInFlight            bool
	headerPrinted            bool
	altScreenSettledItems    []transcriptBodyItem
	altScreenSettledWidth    int
	altScreenSettledFrontier int

	// Composer input history (shell-style ↑/↓ recall of submitted inputs).
	// lastPrompt is the verbatim text of the most recent submitted prompt, so
	// /retry can resend it and /edit can recall it into the composer.
	lastPrompt string
	// lastImages/lastImageLabels/lastDocuments remember the attachments consumed
	// by the most recent submitted prompt. launchPrompt clears the pending queues
	// once a turn is sent, so /retry re-stages these to reproduce the exact same
	// request — otherwise a vision/PDF-backed prompt would silently retry as
	// text-only and answer a different task. They share the underlying image bytes
	// with the sent turn (never mutated in place), so no deep copy is needed.
	lastImages      []zeroruntime.ImageBlock
	lastImageLabels []string
	lastDocuments   []pendingDocument
	// historyIdx == len(inputHistory) means "not navigating"; historyDraft
	// preserves whatever was typed before recall started.
	inputHistory []string
	historyIdx   int
	historyDraft string

	// streamingText is the live assistant text for the current segment, accumulated
	// as []byte so each delta is an O(1) amortized append instead of the O(n²) that
	// string += delta incurs across a long generation. Read via streamingTextString().
	// A []byte (not strings.Builder) because the model is copied by value on every
	// Update, which would trip strings.Builder's copy check.
	streamingText              []byte
	streamingReasoning         string // live provider reasoning for the current segment
	streamingReasoningExpanded bool
	// turnStreamedRunes accumulates every reasoning+answer rune streamed in the
	// current turn so the working line can show a live, monotonic token estimate.
	// It is NOT reset at segment boundaries (where streamingText/Reasoning clear),
	// only at turn start (beginRun), so the count climbs across a multi-tool turn
	// instead of snapping back to zero after each tool call.
	turnStreamedRunes int
	// Streaming-text fade state. lineAges is keyed to LOGICAL lines of
	// streamingText (one entry per \n in the accumulated text), and
	// lastStreamActivity is the time of the most recent delta (used for
	// the in-progress last line — the one the model is currently typing
	// into). fadeActive is true from the first agentTextMsg of a run
	// until the matching agentResponseMsg, and gates both the per-line
	// fade application in interimBlock and the streamingFadeTick
	// re-render loop. The state is reset on stream end, on cancel, and
	// on terminal resize (where the visual line count may change and
	// per-line ages are no longer meaningful).
	lineAges           []time.Time
	lastStreamActivity time.Time
	fadeActive         bool
	fadeDisabled       bool // streaming fade off (ZERO_NO_FADE / SSH / tmux / low-color / reduced motion)
	// streamClearDisabled turns off the full-redraw-on-streamed-newline
	// workaround for terminals that render scroll regions correctly
	// (ZERO_NO_STREAM_CLEAR=1). lastStreamClear rate-limits the redraws the
	// workaround schedules so heavy streaming output (code, logs, diffs)
	// coalesces to a bounded number of repaints per second instead of one
	// per newline. pendingStreamClear tracks a newline that arrived while
	// throttled: the redraw it would have triggered is deferred (flushed by
	// a scheduled streamClearFlushMsg, or at stream end) instead of dropped
	// outright, so a throttled newline that happens to be the last one of
	// the turn still gets its caret repaired.
	streamClearDisabled bool
	lastStreamClear     time.Time
	pendingStreamClear  bool
	reducedMotion       bool // ZERO_REDUCED_MOTION / no-TTY: static spinner glyph, no fade
	// In-progress tool call whose arguments are streaming (a file being written),
	// shown live by streamingToolCallView so a long write/edit isn't a frozen
	// spinner. Cleared when the call completes (next text/turn) — see updateModel.
	// streamCallDecoder decodes the streamed args incrementally (O(1) per delta).
	streamCallID      string
	streamCallName    string
	streamCallDecoder *streamingDecoder

	// Slash-command autocomplete (purely additive UI state). suggestions is the
	// live match list for the current "/token"; suggestionIdx is the highlighted
	// row. commandPaletteOpen keeps a zero-match command search active so invalid
	// query text stays in the palette instead of leaking into the composer.
	// filePaletteOpen does the same for a trailing "@token" file search.
	suggestions        []commandSuggestion
	suggestionIdx      int
	commandPaletteOpen bool
	filePaletteOpen    bool
	// suggestionsAreFiles is true when the overlay is showing "@file" matches
	// rather than "/command" matches, so completion inserts a path token instead
	// of replacing the whole input.
	suggestionsAreFiles bool
	// suggestionsAreSpecialists is true when the overlay is showing leading
	// "@specialist" matches; completion inserts "@name " and the submit path
	// expands the mention into a Task-delegation directive (launchPrompt).
	suggestionsAreSpecialists bool
	lastMouseSelection        mouseSelectionTarget
	mouseCapture              bool
	// mouseReleased, when true, forces terminal mouse capture OFF so the user can
	// drag-select and copy text natively (Ctrl+E toggles it). App mouse features
	// (clickable suggestions, right-click paste, transcript select) pause while on.
	mouseReleased         bool
	transcriptSelection   transcriptSelectionState
	transcriptInteraction *transcriptRenderInteraction
	// hover identifies the single clickable row (if any) currently under the
	// mouse cursor with no button pressed, so it renders in a distinct style —
	// the visual cue that it's clickable. Requires AllMotion mouse reporting
	// (see wantsMouseCapture) since idle cursor movement carries no button.
	hover         hoverTarget
	copyStatus    string
	copyStatusSeq int
	// transientNotice is a single, replaceable confirmation shown above the
	// composer. Unlike copyStatus it is shared by lightweight slash commands
	// and is never persisted into the transcript.
	transientNotice         transientNotice
	transientNoticeSeq      int
	transientNoticeTimerSeq int
	exitConfirmActive       bool
	exitConfirmSeq          int
	// cancelConfirmActive/cancelConfirmSeq mirror exitConfirmActive/exitConfirmSeq
	// (same seq-gated tea.Tick pattern) but guard a DIFFERENT action: Esc
	// cancelling a running turn. The two are deliberately separate state (not a
	// shared flag) since they're different actions with different consequences
	// (quit the app vs. cancel the current run) that are armed by different
	// keys — Ctrl+C and Esc respectively.
	cancelConfirmActive bool
	cancelConfirmSeq    int
	// edgeScrollDelta drives the smooth-glide auto-scroll while a drag holds past
	// the transcript's top/bottom edge: 0 when idle, else the signed per-tick step
	// (matches transcriptEdgeScrollDelta's sign convention). A self-scheduling
	// tea.Tick chain (see dragEdgeScrollTickCmd) keeps stepping it at a fixed small
	// increment regardless of whether new raw mouse-motion events arrive — a
	// terminal only reports motion on actual cursor movement, so without a timer
	// the scroll would stop dead the instant the physical mouse holds still, even
	// while parked past the edge. edgeScrollSeq invalidates a stale in-flight tick
	// (mirroring exitConfirmSeq/copyStatusSeq) whenever the drag moves back into
	// the body, releases, or the chain is otherwise stopped.
	edgeScrollDelta int
	edgeScrollSeq   int
	// edgeScrollMouseX is the column the tick chain keeps extending the selection
	// at — captured from the triggering drag since a timer tick carries no mouse
	// position of its own.
	edgeScrollMouseX int

	// picker, when non-nil, is an open interactive selector overlay (/model,
	// /effort with no argument). It captures ↑/↓/Enter/Esc and applies
	// the chosen value through the existing command handlers.
	picker         *commandPicker
	providerWizard *providerWizardState
	mcpManager     *mcpManagerState
	mcpAddWizard   *mcpAddWizardState
	favoriteModels map[string]bool
	// recentModels is the automatic history of provider+model switches, newest
	// first, capped to config.MaxRecentModels. Unlike favoriteModels (manual
	// pins), this is maintained by recordRecentModel on every successful
	// switch and persisted via config.SetRecentModels.
	recentModels                 []config.RecentModelEntry
	recapsEnabled                bool // idle orientation note (config: recaps on|off)
	recapSeq                     int
	recapRunning                 bool
	recapCancel                  context.CancelFunc
	recapTimerCancel             context.CancelFunc
	recapIdleArmed               bool
	recapIdleRunID               int
	idleRecap                    string
	compactionModel              string // preferences.compactionModel (see providers.CompactionModelID)
	modelPickerLoading           bool
	modelPickerLoadingProviderID string
	modelPickerLoadError         string
	// modelPickerLiveByProvider holds live-discovered models per provider (keyed by
	// catalog descriptor ID), so /model shows each provider's real current models —
	// the same list the provider-setup wizard discovers — not the static catalog.
	modelPickerLiveByProvider map[string][]providermodeldiscovery.Model
	// ollamaContextWindowByModel holds context-window sizes fetched from a local
	// Ollama daemon's native /api/show endpoint (keyed by model name), for
	// custom/local models that have no curated-catalog entry and whose
	// OpenAI-compatible /v1/models listing doesn't carry that metadata at all —
	// see modelContextWindow.
	ollamaContextWindowByModel map[string]int

	// pendingImages holds image attachments staged by /image for the next user
	// turn; pendingImageLabels are their display names (base(path)) for the chip
	// row. Both are cleared after a prompt is submitted (or /image clear). nil =
	// no attachments = today's text-only behavior exactly.
	pendingImages      []zeroruntime.ImageBlock
	pendingImageLabels []string
	// pendingImageThumbnails are decoded previews for a bounded gallery of staged
	// images. They are only rendered by terminals with an inline-image protocol;
	// every other terminal continues to use the compact text attachment row.
	pendingImageThumbnails []*terminalpet.Animation

	// pendingDocuments holds PDF text layers staged by /image for the next user
	// turn; the text is prepended to the prompt as a preamble at submit time and
	// the slice is cleared (or by /image clear). nil = no documents staged.
	pendingDocuments []pendingDocument

	// captureRunImages, when set, is invoked with the images a run is launched
	// with. Nil in production; used by tests to assert image threading without a
	// real provider round-trip.
	captureRunImages func([]zeroruntime.ImageBlock)

	// captureRunSupportsVision, when set, is invoked with the vision-support
	// predicate wired into the run's agent options. Nil in production; used by
	// tests to assert snapshot isolation and discovery scoping.
	captureRunSupportsVision func(func(string) bool)
}

type agentTextMsg struct {
	runID int
	delta string
}

// streamClearThrottle is the minimum gap between full-screen stream-clear
// redraws. Newlines that arrive inside this window mark a deferred clear
// (pendingStreamClear) instead of firing immediately, so heavy streaming
// output coalesces to ~10 repaints/second while still guaranteeing a
// eventual caret repair.
const streamClearThrottle = 100 * time.Millisecond

// streamClearFlushMsg fires once, roughly when the stream-clear throttle
// window (see lastStreamClear) has elapsed, to flush a ClearScreen that a
// throttled newline deferred rather than fired directly. It's a no-op if
// nothing is pending by the time it lands (the common case, since most
// throttled newlines are followed by another one that flushes them first).
type streamClearFlushMsg struct{}

// scheduleStreamClearFlush returns a one-shot command that delivers a
// streamClearFlushMsg after d. Used to guarantee a deferred stream-clear
// redraw is eventually flushed even if no later newline or stream-end event
// does it first (see the streamClearFlushMsg case in Update).
func scheduleStreamClearFlush(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return streamClearFlushMsg{} })
}

type exitConfirmExpiredMsg struct {
	seq int
}

// cancelConfirmExpiredMsg mirrors exitConfirmExpiredMsg for the Esc
// cancel-a-run confirmation (see cancelConfirmActive).
type cancelConfirmExpiredMsg struct {
	seq int
}

// dragEdgeScrollTickMsg advances the smooth-glide auto-scroll one step (see
// edgeScrollDelta). seq must match m.edgeScrollSeq or the tick is stale (the
// drag moved back into the body, released, or was otherwise stopped since this
// tick was scheduled) and is silently dropped — the self-scheduling chain simply
// doesn't reschedule itself, so it terminates rather than ticking forever.
type dragEdgeScrollTickMsg struct {
	seq int
}

// toolCallStreamStartMsg / toolCallStreamDeltaMsg carry a tool call's live
// argument stream from the agent goroutine to the update loop, so a file being
// written renders as it streams (see streamingToolCallView).
type toolCallStreamStartMsg struct {
	runID int
	id    string
	name  string
}

type toolCallStreamDeltaMsg struct {
	runID    int
	id       string
	fragment string
}

type agentReasoningMsg struct {
	runID int
	delta string
}

type agentUsageMsg struct {
	runID   int
	modelID string
	usage   zeroruntime.Usage
}

type agentResponseMsg struct {
	runID       int
	rows        []transcriptRow
	usageEvents []zeroruntime.Usage
	// usageModelID is the model in force when the run ended. usageModelIDs is
	// the model in force when each usageEvents entry fired: a mid-run
	// escalation changes it partway through the run, and billing the events
	// before the switch to the escalated model would be as wrong as billing
	// the ones after it to the starting model. Read through usageModelIDAt.
	usageModelID  string
	usageModelIDs []string
	sessionEvents []pendingSessionEvent
	specReview    *pendingSpecReviewPrompt
	err           error
	goalAware     bool
	// Turn metadata for settled rows that do not otherwise carry it.
	turnTools   int
	turnElapsed time.Duration
	// ttft is time-to-first-token for the turn (0 when nothing streamed — a
	// tool-only or errored turn). Set only on the success path.
	ttft time.Duration
}

// usageModelIDAt is the model in force when usageEvents[index] fired. The
// per-event record wins; usageModelID is the fallback for a message built
// without one, which is what every constructor before escalation produced.
func (msg agentResponseMsg) usageModelIDAt(index int) string {
	if index < len(msg.usageModelIDs) && msg.usageModelIDs[index] != "" {
		return msg.usageModelIDs[index]
	}
	return msg.usageModelID
}

type peerMessageMsg struct {
	message peermsg.InboundMessage
	admit   chan<- bool
}

type peerStatusMsg struct{ event peermsg.StatusEvent }

type peerHeldReleasedMsg struct{ message peermsg.InboundMessage }

type peerDecisionMsg struct {
	message peermsg.InboundMessage
	allow   bool
}

type peerRuntimeErrorMsg struct{ err error }

type peerReceiptErrorMsg struct{ err error }

type peerApprovalExpiredMsg struct{ messageID string }

type agentRowMsg struct {
	runID int
	row   transcriptRow
}

// planUpdateMsg carries a snapshot of plan items from the update_plan tool
// result callback to the live model. The callback runs on the agent goroutine
// and captures model by value, so it cannot mutate m.plan directly — it sends
// this message through the runtimeMessageSink instead.
type planUpdateMsg struct {
	runID int
	items []tools.PlanItem
}

// planStepExplanationMsg carries the model's fresh, plain-English write-up of a
// clicked plan step back to the live model (the one-shot request runs on a
// goroutine via a tea.Cmd, so it can't mutate m directly). text is the written
// explanation; err is set when the request failed (the card then falls back to
// the local summary). key caches the result so re-clicking the step in the same
// state is instant; stepIndex re-renders the card in place when it's still open.
type planStepExplanationMsg struct {
	stepIndex int
	key       string
	gen       int // the planDetailGen when the request started; stale gens are ignored
	text      string
	err       error
}

// specialistStartMsg carries specialist start info from the OnToolCall
// callback to the live model (same rationale as planUpdateMsg).
type specialistStartMsg struct {
	runID          int
	name           string
	description    string
	childSessionID string
}

// specialistCompleteMsg carries specialist completion info from the
// OnToolResult callback to the live model.
type specialistCompleteMsg struct {
	runID          int
	toolCallID     string
	childSessionID string
	status         specialistStatus
	errorMsg       string
}

// swarmSessionsMsg carries swarm task_id -> member session_id pairs (from
// swarm_collect's Meta) so the AGENTS sidebar rows can drill into a member's
// session like a specialist card.
type swarmSessionsMsg struct {
	runID    int
	sessions map[string]string
}

// specialistProgressMsg carries a live tool-call progress update from the
// specialist child process, sent via OnToolProgress → runtimeMessageSink.
type specialistProgressMsg struct {
	runID      int
	toolCallID string
	toolName   string
	detail     string
}

type mcpCommandOrigin int

const (
	mcpCommandOriginTranscript mcpCommandOrigin = iota
	mcpCommandOriginManager
	mcpCommandOriginWizard
)

type mcpCommandRequest struct {
	id              int
	origin          mcpCommandOrigin
	args            []string
	raw             string
	managerSelected int
	managerQuery    string
	wizardDisabled  bool
}

type mcpCommandResultMsg struct {
	request mcpCommandRequest
	result  MCPCommandResult
}

type doctorCommandResultMsg struct {
	id   int
	text string
}

type sandboxSetupCommandResultMsg struct {
	id     int
	result SandboxSetupCommandResult
}

type prStateMsg struct {
	state PrState
}

type prWatcherStartedMsg struct {
	stop func()
}

type permissionDecision = agent.PermissionDecisionAction

const (
	permissionDecisionAllow             permissionDecision = agent.PermissionDecisionAllow
	permissionDecisionAllowStrict       permissionDecision = agent.PermissionDecisionAllowStrict
	permissionDecisionAllowForSession   permissionDecision = agent.PermissionDecisionAllowForSession
	permissionDecisionAllowPrefix       permissionDecision = agent.PermissionDecisionAllowPrefix
	permissionDecisionAlwaysAllowPrefix permissionDecision = agent.PermissionDecisionAlwaysAllowPrefix
	permissionDecisionDeny              permissionDecision = agent.PermissionDecisionDeny
	permissionDecisionAlwaysAllow       permissionDecision = agent.PermissionDecisionAlwaysAllow
	permissionDecisionCancel            permissionDecision = agent.PermissionDecisionCancel
)

type permissionRequestMsg struct {
	runID   int
	request agent.PermissionRequest
	decide  func(agent.PermissionDecision)
}

type pendingPermissionPrompt struct {
	request agent.PermissionRequest
	decide  func(agent.PermissionDecision)
	// decideCmd is the Update-safe alternative to decide for prompts the TUI
	// itself raises: decide forwards through runtimeMessageSink, which blocks on
	// the program's unbuffered message channel when called from Update, so these
	// prompts return a command that yields the follow-up message instead.
	decideCmd func(agent.PermissionDecision) tea.Cmd
	// cursor is the highlighted option index (into permissionOptions): 0 is the
	// resting approval choice. Moved by ↑/↓/Tab; confirmed by Enter or a click.
	// Hotkeys resolve the matching request-provided option directly.
	cursor int
	// typing is true once the user chose "tell Zero what to do differently": the
	// card replaces its option list with a free-text field (sharing the composer
	// input, like the ask_user questionnaire). Submitting sends a Deny decision
	// whose Reason is the typed text, so the model reads it as the tool result and
	// adjusts course in the same turn instead of the run being cancelled.
	typing bool
	// savedDraft holds whatever was in the shared composer input when feedback
	// mode was entered. The field is cleared for typing and restored on both
	// submit and cancel, so a half-typed or queued next-turn message survives the
	// detour (permissionRequestMsg, unlike ask_user, does not clear the composer).
	savedDraft string
}

// askUserRequestMsg is the TUI-loop equivalent of permissionRequestMsg: the
// agent goroutine sends it (via the runtime sink) and blocks until the model
// hands answers back through the answer callback.
type askUserRequestMsg struct {
	runID   int
	request agent.AskUserRequest
	answer  func([]string)
}

// pendingAskUserPrompt tracks an in-progress questionnaire rendered in the composer
// region as a row of tabs — one per question plus a trailing Confirm tab. Questions
// are answered in any order (Tab switches); the answer callback is invoked exactly
// once when the user submits on the Confirm tab or dismisses (Esc). active is the
// current tab (0..N-1 = questions, N = Confirm); states holds the per-question
// picker/free-text state and committed answer. See ask_user_prompt.go.
type pendingAskUserPrompt struct {
	request agent.AskUserRequest
	answer  func([]string)
	active  int
	states  []askUserAnswerState
}

type pendingSpecReviewPrompt struct {
	SpecID         string
	SpecTitle      string
	SpecFilePath   string
	RelativePath   string
	DraftSessionID string
}

type tuiAgentRunOptions struct {
	registry              *tools.Registry
	permissionMode        agent.PermissionMode
	systemPrompt          string
	transientSystemPrompt string
	specDraft             bool
}

// effectiveTUINotifyMode returns the notification mode the TUI should use. An
// empty/unconfigured mode falls back to the TUI's own effective default
// ("both": terminal bell + OSC-9 desktop notification) so the needs-input
// alert works for new users without requiring them to hand-edit config.json.
// The default lives HERE, deliberately not in config.Resolve: the resolver is
// shared with headless `zero exec`, which must stay byte-silent when
// unconfigured (maintainer review, PR #1001). The /notify command persists
// explicit choices; blank stays blank on disk.
func effectiveTUINotifyMode(mode string) notify.Mode {
	m := notify.Mode(strings.TrimSpace(mode))
	if m == "" {
		return notify.ModeBoth
	}
	return m
}

func newModel(ctx context.Context, options Options) model {
	if ctx == nil {
		ctx = context.Background()
	}

	cwd := options.Cwd
	if cwd == "" {
		if current, err := os.Getwd(); err == nil {
			cwd = current
		}
	}

	userConfigDir, _ := config.UserConfigDir()
	loadedUserCommands := usercommands.Load(usercommands.DefaultPaths(cwd, userConfigDir))

	registry := options.Registry
	if registry == nil {
		registry = options.AgentOptions.Registry
	}
	if registry == nil {
		registry = tools.NewRegistry()
	}
	sessionStore := options.SessionStore
	if sessionStore == nil {
		sessionStore = sessions.NewStore(sessions.StoreOptions{})
	}
	sandboxStore := options.SandboxStore
	modelCatalog, err := modelregistry.DefaultRegistry()
	if err != nil {
		panic(err)
	}
	usageTracker := options.UsageTracker
	if usageTracker == nil {
		usageTracker = usage.NewTracker(usage.TrackerOptions{Registry: &modelCatalog})
	}
	prService := options.PrService
	if prService == nil {
		prService = NewPrService(cwd)
	}
	doctorUserConfigPath := options.DoctorUserConfigPath
	if doctorUserConfigPath == "" {
		doctorUserConfigPath = options.UserConfigPath
	}

	permissionMode := options.PermissionMode
	if permissionMode == "" {
		permissionMode = options.AgentOptions.PermissionMode
	}
	if permissionMode == "" {
		permissionMode = agent.PermissionModeAuto
	}

	input := textinput.New()
	input.Prompt = "❯ "
	input.Placeholder = composerPlaceholder
	// Bubble's Ctrl+V binding reads the clipboard itself. Keep it disabled so
	// terminal bracketed paste (Paste: true) is the single paste path.
	input.KeyMap.Paste.SetEnabled(false)
	input.Focus()
	// The main composer paints its own cursor through composerCursorVisible.
	// Keep the embedded textinput cursor static so the shared textinput.Blink
	// message cannot start a second, invisible 530ms tick chain. Setup inputs
	// still use the normal Bubbles cursor and continue blinking independently.
	inputStyles := input.Styles()
	inputStyles.Cursor.Blink = false
	input.SetStyles(inputStyles)

	runSpinner := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	runSpinner.Spinner.FPS = activeAnimationFrameInterval

	notifier := notify.New(os.Stderr, notify.Config{
		Mode:      effectiveTUINotifyMode(options.Notify.Mode),
		FocusMode: notify.FocusMode(strings.TrimSpace(options.Notify.FocusMode)),
	})
	// Opt-in webhook fan-out (ZERO_NOTIFY_WEBHOOK_URL). Delivery failures stay
	// silent here: the TUI owns the alt-screen, so writing to stderr would
	// corrupt the display.
	notify.MaybeAddWebhookSink(notifier, os.Getenv, nil)
	notifier.SetFocused(true)

	resolvedKeyBindings, keyBindingWarnings := sanitizeKeyBindings(resolveKeyBindings(options.KeyBindings))

	m := model{
		ctx:                         ctx,
		cwd:                         cwd,
		appVersion:                  strings.TrimSpace(options.Version),
		startupNotices:              options.StartupNotices,
		swarmDoneAt:                 map[string]time.Time{},
		userCommands:                loadedUserCommands,
		loadSkills:                  options.LoadSkills,
		composerCursorVisible:       true,
		composerBlinkSeq:            1,
		composerBlinkTicking:        true,
		terminalFocused:             true,
		userConfigPath:              options.UserConfigPath,
		doctorUserConfigPath:        doctorUserConfigPath,
		projectConfigPath:           options.ProjectConfigPath,
		savedProviders:              options.SavedProviders,
		gitBranch:                   gitBranch(cwd),
		providerName:                options.ProviderName,
		modelName:                   options.ModelName,
		modelCatalog:                modelCatalog,
		providerProfile:             options.ProviderProfile,
		favoriteModels:              favoriteModelSet(options.FavoriteModels),
		recentModels:                normalizeRecentModelEntries(options.RecentModels),
		compactionModel:             options.CompactionModel,
		recapsEnabled:               options.RecapsEnabled,
		provider:                    options.Provider,
		newProvider:                 options.NewProvider,
		newTurnSessionProvider:      options.NewTurnSessionProvider,
		probeProviderHealth:         options.ProbeProviderHealth,
		discoverProviderModels:      options.DiscoverProviderModels,
		discoverOllamaContextWindow: options.DiscoverOllamaContextWindow,
		deleteProviderKey:           deleteProviderKey,
		clearProviderKeyStored:      config.ClearProviderKeyStoredCaseVariants,
		registry:                    registry,
		awaitToolReadiness:          options.AwaitToolReadiness,
		sessionStore:                sessionStore,
		peerService:                 options.PeerService,
		sandboxStore:                sandboxStore,
		mcpConfig:                   options.MCPConfig,
		mcpPermissionStore:          options.MCPPermissionStore,
		mcpTokenStore:               options.MCPTokenStore,
		mcpCommand:                  options.MCPCommand,
		sandboxSetupCommand:         options.SandboxSetupCommand,
		agentOptions:                options.AgentOptions,
		allowEscalation:             options.AllowEscalation,
		modelRPM:                    options.ModelRPM,
		sessionCompactor:            options.SessionCompactor,
		runtimeMessageSink:          options.RuntimeMessageSink,
		permissionMode:              permissionMode,
		reasoningEffort:             options.ReasoningEffort,
		responseStyle:               defaultedResponseStyle(options.ResponseStyle),
		petEntries:                  map[string]terminalpet.Entry{},
		petID:                       strings.TrimSpace(options.SavedPet),
		keyBindings:                 resolvedKeyBindings,
		themeMode:                   resolveThemeMode(options.Theme, os.Getenv("ZERO_THEME"), options.SavedTheme),
		hasDarkBg:                   true,
		notifyMode:                  string(effectiveTUINotifyMode(options.Notify.Mode)),
		notifyConfiguredMode:        strings.TrimSpace(options.Notify.Mode),
		notifyFocusMode:             strings.TrimSpace(options.Notify.FocusMode),
		userAgent:                   options.UserAgent,
		usageTracker:                usageTracker,
		transcript:                  initialTranscript(),
		transcriptBodyHeights:       newTranscriptBodyHeightCache(defaultTranscriptBodyHeightCacheMaxEntries),
		transcriptInteraction:       &transcriptRenderInteraction{},
		prService:                   prService,
		prState:                     prService.GetState(),
		input:                       input,
		spinner:                     runSpinner,
		prepareRunCompletionWarning: options.PrepareRunCompletionWarning,
		runCompletionWarning:        options.RunCompletionWarning,
		now:                         time.Now,
		notifier:                    notifier,
		altScreen:                   options.AltScreen,
		liveUsageCounts:             map[int]int{},
		swarmSessionMap:             map[string]string{},
		setup:                       newSetupState(options.Setup),
		setupSave:                   options.Setup.Save,
		dictation:                   newDictationController(options),
	}
	petRoot := userConfigDir
	if strings.TrimSpace(options.UserConfigPath) != "" {
		petRoot = filepath.Dir(options.UserConfigPath)
	}
	if strings.TrimSpace(petRoot) != "" {
		m.petClient = terminalpet.NewClient(petRoot)
		if m.petID != "" && m.petID != terminalpet.DisabledID {
			if animation, loadErr := m.petClient.LoadInstalled(m.petID); loadErr == nil {
				m.petAnimation = animation
				m.petName = m.petID
				if entry, entryErr := m.petClient.InstalledEntry(m.petID); entryErr == nil {
					m.petName = entry.Label()
				}
			}
		}
	}
	// Apply an explicit palette immediately. System is applied by Run immediately
	// before Bubble Tea starts, which keeps package-level helper rendering
	// deterministic while models are being constructed in tests.
	if m.themeMode != themeSystem {
		applyTheme(m.themeMode, true)
	}
	m.reducedMotion = defaultReducedMotion()
	// The streaming-text fade (a lime→ink glow on freshly streamed lines) is
	// disabled: it read as a distracting glow rather than a subtle liveness cue.
	// Streaming text always renders statically at base ink (the disabled path in
	// styleStreamingLine), so no accent glow and no per-line fade ticks.
	m.fadeDisabled = true
	// Terminals that handle scroll regions correctly can opt back into the
	// fast incremental path; the redraw workaround (see the ClearScreen
	// scheduling in updateModel) is otherwise on, rate-limited.
	if v := strings.TrimSpace(os.Getenv("ZERO_NO_STREAM_CLEAR")); v != "" && v != "0" && !strings.EqualFold(v, "false") {
		m.streamClearDisabled = true
	}
	// One session-long LSP manager (cheap to build — servers start lazily on the
	// first Check), reused across prompts so gopls stays warm between turns.
	if cwd != "" {
		m.lspManager = lsp.NewManager(cwd)
	}
	m.refreshMCPViewState()
	for _, warning := range keyBindingWarnings {
		m = m.appendSystemNotice(warning)
	}
	m.transcript = m.withStartupNotices(m.transcript)
	return m
}

// withStartupNotices appends the session's startup notices to rows as system
// rows, in order, skipping blank ones so a caller can pass one unconditionally.
//
// EVERY SESSION THIS PROCESS OPENS IS TOLD. The notices describe the sandbox
// every session here runs under, so they open the first session, and they come
// back when /new or /resume rebuilds the transcript for another one; both
// started from an empty transcript and dropped them. Rewind, compaction and
// /clear stay in the same session and keep the notice in the scrollback above
// their divider. Reported by CodeRabbit.
func (m model) withStartupNotices(rows []transcriptRow) []transcriptRow {
	for _, notice := range m.startupNotices {
		if strings.TrimSpace(notice) != "" {
			rows = appendRow(rows, rowSystem, notice)
		}
	}
	return rows
}

func (m model) doctorOptions(connectivity bool) doctor.Options {
	var health *providerhealth.Result
	if connectivity && m.probeProviderHealth != nil && config.HasProviderProfile(m.providerProfile) {
		ctx := m.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		result := m.probeProviderHealth(ctx, providerhealth.Options{
			Profile:      m.providerProfile,
			Connectivity: true,
			UserAgent:    m.userAgent,
		})
		health = &result
	}

	return doctor.Options{
		Now:            m.now,
		Runtime:        "go",
		UserConfig:     m.doctorUserConfigPath,
		ProjectConfig:  m.projectConfigPath,
		Provider:       m.providerProfile,
		WorkspaceRoot:  m.cwd,
		Connectivity:   connectivity,
		ProviderHealth: health,
	}
}

const (
	composerPlaceholder     = "describe a task for zero…"
	composerMaxVisibleLines = 4
)

// composerCursorBlinkInterval is the on/off period of the composer text cursor.
const composerCursorBlinkInterval = 530 * time.Millisecond

// composerBlinkIdleTicksBeforeSettle bounds the software cursor animation.
// Once the terminal is focused but otherwise idle, the cursor finishes a short
// blink sequence and settles visible instead of waking the TUI forever.
const composerBlinkIdleTicksBeforeSettle = 4

// composerTypingIdleThreshold is how long a typing pause must last before the
// cursor resumes blinking; comfortably above normal inter-keystroke gaps
// (~150-300ms) so it won't flicker mid-sentence.
const composerTypingIdleThreshold = 500 * time.Millisecond

// composerBlinkMsg toggles the composer cursor's visibility each tick. The custom
// composer render draws its own cursor (not textinput's), so it drives its own
// blink rather than relying on textinput.Blink.
type composerBlinkMsg struct{ seq int }

func composerBlinkCmd(seq int) tea.Cmd {
	return tea.Tick(composerCursorBlinkInterval, func(time.Time) tea.Msg {
		return composerBlinkMsg{seq: seq}
	})
}

func (m model) armComposerBlink() (model, tea.Cmd) {
	m.composerCursorVisible = true
	m.composerBlinkIdleTicks = 0
	if m.composerBlinkTicking {
		return m, nil
	}
	m.composerBlinkTicking = true
	m.composerBlinkSeq++
	return m, composerBlinkCmd(m.composerBlinkSeq)
}

func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{textinput.Blink, composerBlinkCmd(m.composerBlinkSeq)}
	if m.petAnimation != nil && !m.reducedMotion {
		cmds = append(cmds, petTickCmd(m.petTickSeq, m.petFrameDelay()))
	}
	// Every image protocol wants this, not just the ones that support pixel
	// dragging. Sixel needs it most: it erases itself by writing over cells, so
	// without the pixels-per-cell figure it cannot know how many cells it
	// covered, and falls back to constants describing the reserved area rather
	// than the image.
	if m.petCellMetricsWanted() {
		cmds = append(cmds, tea.Raw(ansi.WindowOp(16)))
	}
	// Bubble Tea documents an initial WindowSizeMsg as delivered automatically
	// on program start, so m.height/m.width are normally set before the first
	// render. But that's the terminal proactively pushing a size — if it's
	// ever missed (a slow/unusual terminal, a multiplexer, a startup race),
	// nothing else ever asks again: m.height stays its zero value forever,
	// `if m.altScreen && m.height > 0` (transcriptView) falls back to the
	// unpadded, non-fullscreen render path for the rest of the session, and
	// the alt-screen viewport never gets filled below the actual content.
	// Explicitly requesting it here means Zero doesn't depend solely on the
	// terminal's unprompted push.
	cmds = append(cmds, tea.RequestWindowSize)
	// Read the terminal background only to keep a selected palette legible. This
	// query never changes the terminal canvas; View deliberately leaves both
	// terminal-wide color fields unset.
	cmds = append(cmds, tea.RequestBackgroundColor)
	// Baseline git snapshot for the FILES sidebar sweep: whatever is already
	// dirty when the TUI opens is pre-existing state, not this session's work
	// (files_git_sweep.go). Async; a non-git workspace just disables the sweep.
	if strings.TrimSpace(m.cwd) != "" {
		cmds = append(cmds, gitSweepCmd(m.ctx, m.cwd, true))
	}
	// Warm model discovery for the active provider in the background so the
	// context-usage gauge (used / total tokens + % fill) knows the active model's
	// window from launch — including proxy/custom models not in the curated
	// registry. Async: never blocks startup; if discovery is unavailable the gauge
	// just shows the used-token count until the window is otherwise learned.
	if descriptor, ok := m.activeProviderDescriptor(); ok {
		if cmd := m.modelPickerProviderDiscoveryCmd(descriptor, m.providerProfile); cmd != nil {
			cmds = append(cmds, cmd)
		}
		// The generic discovery above has no source for a local Ollama model's
		// context window (see ollamaContextWindowDiscoveryCmd); probe its
		// native /api/show separately so the gauge works for custom/local
		// Ollama models too, not just ones in the curated catalog.
		if cmd := m.ollamaContextWindowDiscoveryCmd(descriptor, m.providerProfile.BaseURL, m.modelName); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	if m.prService != nil && m.runtimeMessageSink != nil {
		service := m.prService
		sink := m.runtimeMessageSink
		ctx := m.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		cmds = append(cmds, func() tea.Msg {
			stop := WatchPRStateContext(ctx, service, func(state PrState) {
				sink(prStateMsg{state: state})
			})
			return prWatcherStartedMsg{stop: stop}
		})
	}
	return tea.Batch(cmds...)
}

func (m *model) stopPRWatcher() {
	if m.prWatcherStop == nil {
		return
	}
	m.prWatcherStop()
	m.prWatcherStop = nil
}

// noBlockingModal reports that no modal surface (permission prompt, ask_user,
// spec review, provider/MCP wizard, MCP manager, or picker) is up, so a global
// shortcut may act instead of falling through to a modal's own handler. Shared
// by every shortcut that should defer to whichever modal is focused.
func (m model) noBlockingModal() bool {
	return m.pendingPermission == nil && m.pendingAskUser == nil && m.pendingSpecReview == nil &&
		m.providerWizard == nil && m.mcpAddWizard == nil && m.mcpManager == nil && m.picker == nil &&
		m.sttKeyPrompt == nil && m.renamePrompt == nil && m.terminalAttach == nil
}

func (m model) quit() (tea.Model, tea.Cmd) {
	if m.providerWizard != nil {
		m.providerWizard.resetAimlapiOnboard()
	}
	m.stopPRWatcher()
	m.stopAllBackgroundTerminalSessions()
	m.shutdownLSPManager()
	return m, tea.Quit
}

// shutdownLSPManager gracefully stops the session-long language servers on exit.
// Best-effort with a short deadline so a slow server can't hang the quit; the
// servers are our child processes and would be reaped on exit regardless.
func (m model) shutdownLSPManager() {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if m.lspManager != nil {
		_ = m.lspManager.Shutdown(shutdownCtx)
	}
	// The warm sherpa-onnx streaming server is a session-long child process too;
	// tear it down alongside the language servers (§6a).
	if m.dictation.shutdownServer != nil {
		_ = m.dictation.shutdownServer(shutdownCtx)
	}
}

func (m model) handleCtrlC() (tea.Model, tea.Cmd) {
	if m.btw.active {
		if !m.pending && m.composerValue() != "" && m.noBlockingModal() && !m.transcriptDetailed && !m.subchat.active {
			m.clearComposer()
			m.clearSuggestions()
			return m, nil
		}
		return m.leaveBTW()
	}
	if !m.pending && m.composerValue() != "" && m.noBlockingModal() && !m.transcriptDetailed && !m.subchat.active {
		m.clearComposer()
		m.clearSuggestions()
		m = m.disarmExitConfirmation()
		return m, nil
	}
	if m.exitConfirmActive {
		m = m.disarmExitConfirmation()
		m.cancelRun()
		m.exiting = true
		// A cancelled run may still need to flush checkpoint/session events; quit
		// only after agentResponseMsg drains flushRunIDs so /rewind stays valid.
		if len(m.flushRunIDs) > 0 {
			return m, nil
		}
		return m.quit()
	}
	m.cancelRun()
	m.exitConfirmActive = true
	m.exitConfirmSeq++
	seq := m.exitConfirmSeq
	return m, tea.Tick(ctrlCExitConfirmDuration, func(time.Time) tea.Msg {
		return exitConfirmExpiredMsg{seq: seq}
	})
}

func (m model) disarmExitConfirmation() model {
	if m.exitConfirmActive {
		m.exitConfirmActive = false
		m.exitConfirmSeq++
	}
	return m
}

func (m model) disarmCancelConfirmation() model {
	if m.cancelConfirmActive {
		m.cancelConfirmActive = false
		m.cancelConfirmSeq++
	}
	return m
}

// Update routes every message through updateModel, then advances the flush
// frontier for inline rendering. Alt-screen runs keep rows in the managed view
// instead of printing into terminal scrollback (see flush.go).
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(flushedMsg); ok {
		m.printInFlight = false
		return m.drainFlushQueue()
	}
	var recapActivityCmd tea.Cmd
	switch msg.(type) {
	case tea.KeyPressMsg, tea.PasteMsg, tea.MouseMsg:
		m, recapActivityCmd = m.resetIdleRecapAfterActivity()
	}
	next, cmd := m.updateModel(msg)
	nm, ok := next.(model)
	if !ok {
		return next, cmd
	}
	nm = nm.syncChatScroll()
	nm, mouseCmd := nm.syncMouseCapture()
	nm, flushCmd := nm.settleTranscript()
	var composerCmd tea.Cmd
	switch msg.(type) {
	case tea.KeyPressMsg, tea.PasteMsg, tea.FocusMsg:
		if nm.terminalFocused {
			nm, composerCmd = nm.armComposerBlink()
		}
	}
	nm, noticeCmd := nm.ensureTransientNoticeTimer()
	return nm, batchCommands(cmd, mouseCmd, flushCmd, recapActivityCmd, composerCmd, noticeCmd)
}

func batchCommands(cmds ...tea.Cmd) tea.Cmd {
	filtered := make([]tea.Cmd, 0, len(cmds))
	for _, cmd := range cmds {
		if cmd != nil {
			filtered = append(filtered, cmd)
		}
	}
	switch len(filtered) {
	case 0:
		return nil
	case 1:
		return filtered[0]
	default:
		return tea.Batch(filtered...)
	}
}

func (m model) updateModel(msg tea.Msg) (tea.Model, tea.Cmd) {
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		m = m.resizeBTWParent(size)
	}
	if next, cmd, routed := m.routeBTWParentMessage(msg); routed {
		return next, cmd
	}
	switch msg := msg.(type) {
	case uv.CellSizeEvent:
		if msg.Width > 0 && msg.Height > 0 {
			m.petCellPixelWidth = msg.Width
			m.petCellPixelHeight = msg.Height
		}
		return m, nil
	case peerMessageMsg:
		admitted := m.canAcceptPeerMessage(msg.message)
		if msg.admit != nil {
			msg.admit <- admitted
		}
		if !admitted {
			m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowError, text: "Dropped peer message because the inbound queue is full."})
			return m, nil
		}
		return m.handlePeerMessage(msg.message)
	case peerStatusMsg:
		return m.handlePeerStatus(msg.event), nil
	case peerHeldReleasedMsg:
		return m.handleReleasedPeerMessage(msg.message)
	case peerDecisionMsg:
		return m.handlePeerDecision(msg.message, msg.allow)
	case peerApprovalExpiredMsg:
		return m.handlePeerApprovalExpired(msg.messageID)
	case peerReceiptErrorMsg:
		if msg.err != nil {
			m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowError, text: "peer receipt: " + msg.err.Error()})
		}
		return m, nil
	case peerRuntimeErrorMsg:
		if msg.err != nil {
			m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowError, text: "peer messaging unavailable: " + msg.err.Error()})
		}
		return m, nil
	case composerBlinkMsg:
		if !m.composerBlinkTicking || msg.seq != m.composerBlinkSeq {
			return m, nil
		}
		switch {
		case !m.terminalFocused:
			m.composerCursorVisible = false
			m.composerBlinkTicking = false
			return m, nil
		case m.now().Sub(m.lastCharTime) < composerTypingIdleThreshold:
			m.composerCursorVisible = true
			m.composerBlinkIdleTicks = 0
		default:
			m.composerBlinkIdleTicks++
			if m.composerBlinkIdleTicks >= composerBlinkIdleTicksBeforeSettle {
				m.composerCursorVisible = true
				m.composerBlinkTicking = false
				return m, nil
			}
			m.composerCursorVisible = !m.composerCursorVisible
		}
		return m, composerBlinkCmd(m.composerBlinkSeq)
	case tea.BackgroundColorMsg:
		// A background reading changes only the contrast direction used for named
		// palettes. It never repaints the terminal canvas.
		m.hasDarkBg = msg.IsDark()
		if m.themeMode != themeSystem {
			applyTheme(m.themeMode, m.hasDarkBg)
		}
		return m, nil
	case tea.MouseMsg:
		// Attached to a live terminal: the overlay owns the viewport, so mouse
		// events go nowhere rather than hitting transcript selection below.
		if m.terminalAttach != nil {
			return m, nil
		}
		if m.setup.visible {
			return m.handleSetupMouse(msg)
		}
		return m.handleMouse(msg)
	case transcriptCopiedMsg:
		m.copyStatusSeq++
		if msg.err != nil {
			// Keep the selection so the user can retry; just surface the failure.
			m.copyStatus = "Copy failed"
		} else {
			m.transcriptSelection = transcriptSelectionState{}
			m.copyStatus = "Copied!"
		}
		seq := m.copyStatusSeq
		return m, tea.Tick(2*time.Second, func(time.Time) tea.Msg {
			return transcriptCopyStatusExpiredMsg{seq: seq}
		})
	case transcriptCopyStatusExpiredMsg:
		if msg.seq == m.copyStatusSeq {
			m.copyStatus = ""
		}
		return m, nil
	case transientNoticeExpiredMsg:
		if msg.seq == m.transientNoticeSeq {
			m.transientNotice = transientNotice{}
		}
		return m, nil
	case exitConfirmExpiredMsg:
		if msg.seq == m.exitConfirmSeq {
			m.exitConfirmActive = false
		}
		return m, nil
	case cancelConfirmExpiredMsg:
		if msg.seq == m.cancelConfirmSeq {
			m.cancelConfirmActive = false
		}
		return m, nil
	case leaderExpiredMsg:
		if msg.seq == m.leaderSeq {
			m.leaderPending = false
		}
		return m, nil
	case dragEdgeScrollTickMsg:
		if msg.seq != m.edgeScrollSeq || m.edgeScrollDelta == 0 || !m.transcriptSelection.active {
			return m, nil // stale, or the chain was stopped since this tick was scheduled
		}
		m = m.dragToEdgeScroll(m.edgeScrollDelta, m.edgeScrollMouseX)
		return m, dragEdgeScrollTickCmd(m.edgeScrollSeq)
	case providerWizardOAuthMsg:
		return m.applyProviderWizardOAuth(msg)
	case aimlapiOnboardMsg:
		return m.applyAimlapiOnboard(msg)
	case aimlapiExistingBalanceMsg:
		return m.applyExistingAimlapiBalance(msg)
	case providerWizardDeviceCodeMsg:
		return m.applyProviderWizardDeviceCode(msg)
	case providerManagerCredsMsg:
		return m.applyProviderManagerCreds(msg)
	case providerManagerCleanupMsg:
		return m.applyProviderManagerCleanup(msg)
	case clipboardReadMsg:
		// Result of a right-click paste. Insert on success; surface a brief
		// status if the clipboard couldn't be read (e.g. no clipboard utility on
		// a remote session). An empty clipboard is a silent no-op.
		if msg.err != nil {
			m.copyStatusSeq++
			m.copyStatus = "Paste failed"
			seq := m.copyStatusSeq
			return m, tea.Tick(2*time.Second, func(time.Time) tea.Msg {
				return transcriptCopyStatusExpiredMsg{seq: seq}
			})
		}
		if msg.content == "" {
			// Empty text clipboard — may be a screenshot. Probe for image.
			return m, readClipboardImageCmd()
		}
		return m.routePaste(msg.content)
	case clipboardImageMsg:
		if msg.err != nil {
			return m.appendImageNotice("Clipboard image read failed: " + msg.err.Error()), nil
		}
		if msg.data == nil {
			return m, nil // no image — silent no-op
		}
		return m.attachClipboardImage(msg.data, msg.mediaType), nil
	case tea.PasteMsg:
		// While attached to a live terminal, a paste forwards verbatim to the
		// PTY stdin — never into the composer or transcript.
		if m.terminalAttach != nil {
			return m.handleTerminalAttachPaste(msg.Content)
		}
		// A paste into the cloud-STT key prompt fills the key (the common way to
		// enter an API key), not the composer.
		if m.sttKeyPrompt != nil {
			m.sttKeyPrompt.input += strings.TrimSpace(msg.Content)
			return m, nil
		}
		return m.routePaste(msg.Content)
	case dictationStartedMsg:
		return m.handleDictationStarted(msg)
	case dictationTranscribedMsg:
		return m.handleDictationTranscribed(msg)
	case sttPartialMsg:
		return m.handleDictationPartial(msg), nil
	case sttDownloadProgressMsg:
		return m.handleDictationDownloadProgress(msg), nil
	case dictationDownloadedMsg:
		return m.handleDictationDownloaded(msg)
	case sttModelsFetchedMsg:
		return m.handleSTTModelsFetched(msg), nil
	case recTickMsg:
		return m.handleRecTick()
	case sttLevelMsg:
		return m.handleDictationLevel(msg), nil
	case tea.KeyboardEnhancementsMsg:
		return m.handleKeyboardEnhancements(msg), nil
	case tea.KeyReleaseMsg:
		// Voice mode's hold-to-record ends on Ctrl+Space release; every other
		// release event is ignored (dispatch elsewhere is press-based).
		if m.dictation.voiceModeEnabled && voiceCaptureReleaseKey(msg) {
			return m.handleVoiceCaptureRelease()
		}
		return m, nil
	case terminalAttachTickMsg:
		return m.refreshTerminalAttach()
	case interactiveExecStartMsg:
		if msg.runID != m.activeRunID {
			return m, nil
		}
		m.terminalAutoAttach = &terminalAutoAttachState{
			runID:    msg.runID,
			known:    msg.known,
			deadline: m.now().Add(terminalAutoAttachTimeout),
		}
		return m, terminalAutoAttachTickCmd(msg.runID)
	case terminalAutoAttachTickMsg:
		return m.pollTerminalAutoAttach(msg)
	case tea.KeyPressMsg:
		if m.petDragActive {
			pixelDrag := m.petPixelDrag
			if !keyIs(msg, tea.KeyEsc) && !keyCtrl(msg, 'c') {
				return m, nil
			}
			m.cancelPetDrag()
			m.lastKeyTime = time.Time{}
			m.burstCount = 0
			if pixelDrag {
				return m, petPixelMouseDisableCmd()
			}
			return m, nil
		}
		// Paste-detection timing trackers. MUST run before any early return
		// so burst counting stays accurate regardless of which branch fires.
		now := m.now()
		if !m.lastKeyTime.IsZero() && now.Sub(m.lastKeyTime) < 100*time.Millisecond {
			m.burstCount++
		} else {
			m.burstCount = 0
		}
		m.lastKeyTime = now
		if !keyIs(msg, tea.KeyEnter) {
			m.lastCharTime = now
		}
		// Enter the solid-while-typing state right away: only composerBlinkMsg
		// evaluates the typing threshold, so if the blink phase had just hidden
		// the caret, the typed character would render caret-less for up to a
		// full tick before the timer catches up.
		m.composerCursorVisible = true
		if m.setup.visible {
			return m.handleSetupKey(msg)
		}
		// The cloud-STT API-key prompt is modal: it owns every keystroke (masked
		// input) until Enter saves or Esc cancels.
		if m.sttKeyPrompt != nil {
			return m.handleSTTKeyPromptKey(msg)
		}
		// Attached to a live terminal session: keystrokes (including Ctrl+C,
		// which maps to 0x03 for the process) go to the PTY until Esc detaches.
		if m.terminalAttach != nil {
			return m.handleTerminalAttachKey(msg)
		}
		if m.renamePrompt != nil {
			return m.handleSessionRenameKey(msg)
		}
		m.transcriptSelection = transcriptSelectionState{}
		m.composerSelection = composerSelectionState{}
		m.clearMouseSelection()
		if !keyCtrl(msg, 'c') {
			m = m.disarmExitConfirmation()
		}
		// Mirrors the exit-confirmation reset above: any key that isn't itself
		// the confirming Esc means the user moved on to something else, so a
		// later, unrelated Esc must arm a fresh confirmation rather than
		// silently cancel off a stale press from seconds ago.
		if !keyIs(msg, tea.KeyEsc) {
			m = m.disarmCancelConfirmation()
		}
		// The `?` help overlay is modal: `?`, Esc, q, or Enter close it; every
		// other key is swallowed so nothing types into the hidden composer.
		if m.helpOverlay {
			if keyText(msg) == "?" || keyText(msg) == "q" || keyIs(msg, tea.KeyEsc) || keyIs(msg, tea.KeyEnter) || keyCtrl(msg, 'c') {
				m.helpOverlay = false
			}
			m.burstCount = 0
			return m, nil
		}
		// Ctrl+X ? leader-chord map: same dismiss keys as the general help overlay.
		if m.leaderHelpOverlay {
			if keyText(msg) == "?" || keyText(msg) == "q" || keyIs(msg, tea.KeyEsc) || keyIs(msg, tea.KeyEnter) || keyCtrl(msg, 'c') {
				m.leaderHelpOverlay = false
			}
			m.burstCount = 0
			return m, nil
		}
		if keyCtrl(msg, 'c') {
			if m.leaderPending {
				m = m.clearLeader()
			}
			return m.handleCtrlC()
		}
		if m.leaderPending {
			// Leader owns every keystroke until resolved; never type into the composer.
			return m.handleLeaderKey(msg)
		}
		if keyCtrl(msg, 'x') && m.canArmLeader() {
			return m.armLeader()
		}
		// Emacs Ctrl+P / Ctrl+N move selection in open menus. Runs before the
		// switch so menus win over global Ctrl+P (plan toggle). Idle Ctrl+P
		// falls through to that binding; idle Ctrl+N is a reserved no-op so it
		// never reaches remapped configurable bindings (e.g. toggleSidebar).
		if !m.transcriptDetailed && (keyCtrl(msg, 'p') || keyCtrl(msg, 'n')) {
			delta := 1
			if keyCtrl(msg, 'p') {
				delta = -1
			}
			if next, cmd, ok := m.moveModalSelection(delta); ok {
				return next, cmd
			}
			if keyCtrl(msg, 'n') {
				return m, nil
			}
		}
		switch {
		case m.runDetailsOpen:
			if keyIs(msg, tea.KeyEsc) || m.keyMatch(m.keyBindings.toggleSidebar, msg, func(tea.KeyMsg) bool { return keyCtrl(msg, 'b') }) {
				m.runDetailsOpen = false
			}
			return m, nil
		case m.keyMatch(m.keyBindings.toggleDetailed, msg, func(tea.KeyMsg) bool { return keyCtrl(msg, 'o') }):
			return m.toggleDetailedTranscript(), nil
		case m.fileView.active && m.noBlockingModal() && m.composerValue() == "" && (keyText(msg) == "d" || keyText(msg) == "f"):
			// Mode toggle for the file drill-in, only while the composer is empty
			// (so mid-sentence typing is never hijacked) and no modal is up (so a
			// permission prompt / ask-user / wizard keeps its own key handling).
			if keyText(msg) == "f" {
				return m.setFileViewMode(fileViewFull), nil
			}
			return m.setFileViewMode(fileViewDiff), nil
		case m.keyMatch(m.keyBindings.toggleMouse, msg, func(tea.KeyMsg) bool { return keyCtrl(msg, 'e') }) && canFireComposerGatedToggle(m.keyBindings.toggleMouse, defaultToggleMouseChord, m.composerValue() == ""):
			// Release/recapture the mouse so the user can drag-select and copy text
			// natively (mouse capture otherwise intercepts terminal selection). The
			// composer-empty requirement only applies when the binding resolves to
			// the conflicting default Ctrl+E chord (unset, or explicitly configured
			// to the same chord), which readline navigation (move-to-end-of-line)
			// also claims while typing; a binding that resolves to a genuinely
			// different chord still fires mid-type.
			m.mouseReleased = !m.mouseReleased
			if m.mouseReleased {
				mouseKey := labelOr(m.keyBindings.toggleMouse, "Ctrl+E")
				return m.appendSystemNotice(fmt.Sprintf("Mouse released — drag to select and copy text. Press %s again to re-enable mouse interaction (clicks, right-click paste).", mouseKey)), nil
			}
			return m.showTransientNoticeInline("Mouse interaction re-enabled.", transientNoticeSuccess), nil
		case m.dictation.voiceModeEnabled && !m.transcriptDetailed && voiceCaptureKey(msg) && m.noBlockingModal():
			// Voice mode reserves Ctrl+Space for recording; ordinary Space continues
			// through the composer path below.
			return m.handleVoiceCapturePress(msg)
		case keyIs(msg, tea.KeyEsc):
			// Esc is heavily overloaded below (subchat exit, MCP cancel, ask-user,
			// permission deny, wizard/picker/suggestions dismiss, ...) before ever
			// reaching the run-cancel fallback. Capture whether this press really
			// is the confirming second Esc BEFORE any of those branches can fire,
			// then disarm unconditionally: an Esc that gets consumed by one of
			// them wasn't a confirm, so it must not leave cancelConfirmActive
			// armed for some later, unrelated Esc to silently act on.
			wasConfirmingCancel := m.pending && m.cancelConfirmActive
			m = m.disarmCancelConfirmation()
			// An active dictation recording cancels on Esc (releases the mic, drops
			// the audio) — but only if this Esc isn't a confirming run-cancel
			// press. Without this guard, a user mid-recording who double-Esc's
			// to kill the run finds the first Esc swallowed by dictation and
			// the run still going. The pending run cancel happens further
			// down at the bottom of the Esc branch.
			if m.dictation.active() && !wasConfirmingCancel {
				return m.cancelDictation()
			}
			// Subchat view exits on Esc (returns to main chat).
			if m.subchat.active {
				m.chatScrollOffset = m.subchat.exit()
				m = m.clearHover() // bodyY numbering differs between subchat and the parent transcript
				return m, nil
			}
			// File drill-in exits on Esc (returns to the chat at its saved scroll
			// position); the file stays selected so a second Esc clears that. Only
			// with no blocking modal up: Esc on a permission prompt / ask-user /
			// wizard must reach THAT surface's deny/cancel handling below, not
			// silently close the drill-in behind it.
			if m.fileView.active && m.noBlockingModal() {
				return m.exitFileView(), nil
			}
			if m.mcpCommandCancel != nil {
				m.cancelMCPCommand()
				if m.mcpAddWizard != nil {
					m.mcpAddWizard.result = mcpAddWizardResult{Title: "MCP setup cancelled", State: "cancelled", Message: "MCP action was cancelled.", ActionHint: "Edit config"}
					m.mcpAddWizard.step = mcpAddWizardStepResult
					return m, nil
				}
				m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowSystem, tool: "mcp", text: "MCP action cancelled"})
				return m, nil
			}
			if m.transcriptDetailed {
				m.transcriptDetailed = false
				return m, nil
			}
			// Esc on an ask-user prompt: from the "type my own" free-text it steps
			// back to the selector for that question; otherwise it cancels the
			// questionnaire (not the run), delivering whatever answers were collected
			// so the agent loop unblocks and degrades to its best-assumption path.
			if m.pendingAskUser != nil {
				return m.escapeAskUser()
			}
			// Esc in the permission feedback field steps back to the option list
			// rather than resolving, so a stray keystroke is recoverable.
			if m.pendingPermission != nil && m.pendingPermission.typing {
				return m.cancelPermissionTyping()
			}
			if m.pendingSpecReview != nil {
				m.burstCount = 0
				return m.cancelSpecReview()
			}
			if m.pendingPermission != nil && m.pendingPermission.request.ToolName == tools.RequestPermissionsToolName {
				return m.resolvePermission(permissionDecisionDeny)
			}
			if m.pendingPermission != nil && m.pendingPermission.request.ToolName == peerPermissionToolName {
				return m.resolvePermission(permissionDecisionDeny)
			}
			if m.providerWizard != nil {
				// Delegate so multi-level surfaces (provider manager list → edit →
				// field, manage-key step) can walk BACK one level; the wizard's own
				// handler closes the overlay for the single-level steps.
				return m.handleProviderWizardKey(msg)
			}
			if m.mcpAddWizard != nil {
				m.mcpAddWizard = nil
				return m, nil
			}
			if m.mcpManager != nil {
				m.mcpManager = nil
				return m, nil
			}
			// An open picker cancels first; then an active suggestion overlay is
			// dismissed. Neither cancels the run or clears the input.
			if m.picker != nil {
				if m.picker.kind == pickerModel {
					m.clearModelPickerLoadState()
				}
				if m.picker.kind == pickerPet {
					m.cancelPetPreview()
				}
				m.picker = nil
				return m, nil
			}
			if m.suggestionsActive() {
				return m.dismissSuggestions(), nil
			}
			// A selected FILES row clears before anything run-related: the
			// selection is a passive highlight, so Esc dropping it is cheap and
			// expected (mirrors how editors clear selection on Esc).
			if m.selectedFile != "" {
				m.setSelectedFile("")
				return m, nil
			}
			if m.hasQueuedMessage() {
				return m.clearQueuedMessage(), nil
			}
			m.clearSuggestions()
			if m.pending {
				if wasConfirmingCancel {
					m.clearComposer()
					m.cancelRun()
					return m, nil
				}
				// First Esc only arms the confirmation — preserve whatever
				// draft the user has typed, since nothing has actually been
				// cancelled yet and they may not press Esc again.
				m.cancelConfirmActive = true
				m.cancelConfirmSeq++
				seq := m.cancelConfirmSeq
				return m, tea.Tick(escCancelConfirmDuration, func(time.Time) tea.Msg {
					return cancelConfirmExpiredMsg{seq: seq}
				})
			}
			m.clearComposer()
			return m, nil
		case keyIs(msg, tea.KeyEnter):
			if m.transcriptDetailed {
				if command := parseCommand(m.input.Value()); command.kind == commandTranscript {
					m.input.SetValue("")
					return m.toggleDetailedTranscript(), nil
				}
				return m, nil
			}
			if m.pendingPermission != nil {
				// Enter confirms the highlighted option (default: allow once); the
				// a/y/d hotkeys and a click still resolve directly.
				m.burstCount = 0
				return m.confirmPermissionCursor()
			}
			if m.pendingAskUser != nil {
				m.burstCount = 0
				return m.confirmAskUser()
			}
			if m.pendingSpecReview != nil {
				m.burstCount = 0
				return m, nil
			}
			if m.providerWizard != nil {
				m.burstCount = 0
				return m.handleProviderWizardKey(msg)
			}
			if m.mcpAddWizard != nil {
				m.burstCount = 0
				return m.handleMCPAddWizardKey(msg)
			}
			if m.mcpManager != nil {
				m.burstCount = 0
				return m.handleMCPManagerKey(msg)
			}
			if m.picker != nil {
				m.burstCount = 0
				return m.choosePicker()
			}
			if keyAlt(msg) || keyShift(msg) {
				if next, ok := m.applyComposerKey(msg); ok {
					return next, nil
				}
			}
			// Enter on file suggestions inserts the @file token for continued
			// composing. Command suggestions execute only when the selected command
			// is self-contained; commands that require a value are inserted so the
			// user can finish the argument first.
			if m.suggestionsActive() {
				return m.chooseSuggestion()
			}
			// Timing-based paste protection: under Termux, context-menu paste
			// injects characters one at a time (including newlines as raw
			// KeyEnter events). A sustained burst of 3+ keys within 100ms
			// means we are inside a char-by-char paste — insert newline
			// instead of submitting. Gated to Termux so fast desktop typing
			// (which can reach similar inter-key intervals) is never affected.
			if os.Getenv("TERMUX_VERSION") != "" && m.burstCount > 2 {
				state := m.currentComposerState()
				m = m.insertComposerTextWithPastePreview(state, "\n", "")
				m.clearSuggestions()
				return m, nil
			}

			// Composer-based paste protection: when the composer already has
			// multiline text (e.g. pasted via bracketed paste / Ctrl+Shift+V),
			// plain Enter inserts a newline instead of submitting so each
			// pasted \n does not trigger a premature submit. Uses the same
			// burstCount > 2 threshold as the Termux path so a single fast
			// key + Enter on a multiline prompt still submits.
			if m.composerActive && m.burstCount > 2 && strings.Contains(m.composer.text, "\n") {
				state := m.currentComposerState()
				m = m.insertComposerTextWithPastePreview(state, "\n", "")
				m.clearSuggestions()
				return m, nil
			}
			m.burstCount = 0
			return m.handleSubmit()
		case keyIs(msg, tea.KeyTab) && keyShift(msg):
			if m.transcriptDetailed {
				return m, nil
			}
			if m.pendingPermission != nil {
				return m.movePermissionCursor(-1), nil
			}
			if m.pendingAskUser != nil {
				return m.moveAskUserTab(-1), nil
			}
			// shift+tab toggles the permission mode between Auto and Ask (Unsafe
			// is intentionally not reachable by a casual keypress — see
			// nextPermissionMode), but only when nothing modal is up: a permission
			// prompt, ask_user questionnaire, or open picker all take precedence
			// and let the key fall through to their own handlers below.
			if m.noBlockingModal() {
				m.permissionMode = nextPermissionMode(m.permissionMode)
				m = m.syncPeerIdentity()
				return m, nil
			}
		case m.keyMatch(m.keyBindings.cycleReasoning, msg, func(tea.KeyMsg) bool { return keyCtrl(msg, 't') }):
			if m.transcriptDetailed {
				return m, nil
			}
			// Ctrl+T cycles reasoning effort (auto -> low ->
			// medium -> high -> auto), but only when nothing modal is up — the
			// same gate shift+tab uses above. Not gated on m.pending: cycling
			// mid-run is allowed and takes effect on the next turn, matching
			// /effort. cycleReasoningEffort is a silent no-op on models with no
			// effort controls.
			if m.noBlockingModal() {
				return m.cycleReasoningEffort()
			}
		case m.keyMatch(m.keyBindings.toggleSidebar, msg, func(tea.KeyMsg) bool { return keyCtrl(msg, 'b') }) && canFireComposerGatedToggle(m.keyBindings.toggleSidebar, defaultToggleSidebarChord, m.composerValue() == ""):
			// Ctrl+B opens a compact, on-demand run summary. The composer-empty rule
			// preserves readline's Ctrl+B move-to-beginning behavior; a remapped
			// binding continues to work while composing.
			if !m.transcriptDetailed && m.noBlockingModal() && m.runDetailsAllowed() {
				m.runDetailsOpen = !m.runDetailsOpen
				return m, nil
			}
		case keyCtrl(msg, 'v'), keySuper(msg, 'v'):
			// Ctrl+V probes the clipboard for an IMAGE only. Text pasting stays
			// exclusively on the terminal's bracketed-paste path (Bubble's own
			// Ctrl+V binding is disabled in newModel for exactly that reason), so
			// this cannot double-insert text. It is needed because a clipboard
			// holding a screenshot produces no bracketed paste at all: the terminal
			// has no text to send, so routePaste never runs and its empty-content
			// image probe never fires. Right-click paste reached that probe only
			// because it always delivers a clipboardReadMsg, empty or not.
			// readClipboardImageCmd yields no message when the clipboard holds no
			// image, so Ctrl+V with text on the clipboard stays a no-op here and is
			// handled by the bracketed paste exactly as before.
			//
			// Cmd+V is matched too, since macOS reports Command as ModSuper rather
			// than ModCtrl. That only helps on terminals that deliver the key to the
			// application: one that handles Cmd+V itself pastes the clipboard TEXT
			// and sends no key event, so an image-only clipboard still produces
			// nothing for this to react to.
			if m.noBlockingModal() {
				return m, readClipboardImageCmd()
			}
		case keyCtrl(msg, 'f'):
			if m.picker != nil && m.picker.kind == pickerModel {
				if m.modelPickerIsLoading() {
					return m, nil
				}
				return m.toggleModelFavorite(), nil
			}
		case keyText(msg) == "?" && !keyAlt(msg) && !keyHasMod(msg, tea.ModCtrl):
			// `?` opens the keyboard-shortcut overlay, but ONLY on an empty
			// composer with nothing modal up — otherwise it must type a literal
			// "?" into the prompt. Falls through to the rune-insert path below
			// when the composer is non-empty or a popup is active.
			if m.composerValue() == "" && m.noBlockingModal() && !m.transcriptDetailed && !m.subchat.active && !m.suggestionsActive() {
				m.helpOverlay = true
				return m, nil
			}
		case keyBackspace(msg):
			// In permission feedback mode Backspace is a plain edit of the feedback
			// text. This case runs before the typing branch below and, on an empty
			// field (feedback mode clears the composer), would otherwise fall to the
			// removeLastAttachment path and silently drop a staged image/doc that
			// savedDraft does not restore. Route it to the shared input instead.
			if m.pendingPermission != nil && m.pendingPermission.typing {
				var cmd tea.Cmd
				m.input, cmd = m.input.Update(msg)
				return m, cmd
			}
			if m.picker != nil {
				if m.modelPickerIsLoading() {
					return m, nil
				}
				m.picker.deleteQueryRune()
				if m.picker.kind == pickerPet {
					return m.schedulePetPreview()
				}
				return m, nil
			}
			// On an empty composer, Backspace removes the last attachment chip
			// ([Image #N] / [Doc #N]) so you can drop one you don't need without
			// clearing them all. With text present it deletes a character as usual.
			if m.composerValue() == "" {
				if next, removed := m.removeLastAttachment(); removed {
					return next, nil
				}
			}
		case keyIs(msg, tea.KeyTab):
			if m.transcriptDetailed {
				return m, nil
			}
			if m.pendingPermission != nil {
				return m.movePermissionCursor(1), nil
			}
			if m.pendingAskUser != nil {
				return m.moveAskUserTab(1), nil
			}
			if m.providerWizard != nil {
				m.burstCount = 0
				return m.handleProviderWizardKey(msg)
			}
			if m.mcpAddWizard != nil {
				m.burstCount = 0
				return m.handleMCPAddWizardKey(msg)
			}
			if m.mcpManager != nil {
				m.burstCount = 0
				return m.handleMCPManagerKey(msg)
			}
			if m.picker == nil && m.suggestionsActive() {
				m.moveSuggestion(1)
				return m, nil
			}
		case keyIs(msg, tea.KeyPgUp):
			m = m.clearHover()
			return m.scrollChat(m.chatPageScrollLines()), nil
		case keyIs(msg, tea.KeyPgDown):
			m = m.clearHover()
			return m.scrollChat(-m.chatPageScrollLines()), nil
		case keyShift(msg) && keyIs(msg, tea.KeyUp):
			// Shift+Up scrolls the transcript up one line. Must be checked before
			// plain KeyUp so shifted arrows aren't consumed by the composer-path.
			if m.transcriptDetailed {
				return m, nil
			}
			// Suggestions keep Shift+arrows for the composer path (unchanged); plain
			// ↑/↓ and Ctrl+P/N still move the palette via moveModalSelection.
			if m.suggestionsActive() {
				break
			}
			if next, cmd, ok := m.moveModalSelection(-1); ok {
				return next, cmd
			}
			if m.composerValue() != "" {
				break // let the input handle multiline navigation
			}
			m = m.clearHover()
			return m.scrollChat(1), nil
		case keyShift(msg) && keyIs(msg, tea.KeyDown):
			// Shift+Down scrolls the transcript down one line. Must be checked
			// before plain KeyDown so shifted arrows aren't consumed.
			if m.transcriptDetailed {
				return m, nil
			}
			if m.suggestionsActive() {
				break
			}
			if next, cmd, ok := m.moveModalSelection(1); ok {
				return next, cmd
			}
			if m.composerValue() != "" {
				break // let the input handle multiline navigation
			}
			m = m.clearHover()
			return m.scrollChat(-1), nil
		case keyIs(msg, tea.KeyDown):
			if m.transcriptDetailed {
				m = m.clearHover()
				return m.scrollChat(-1), nil
			}
			if next, cmd, ok := m.moveModalSelection(1); ok {
				return next, cmd
			}
			if next, ok := m.moveComposerVisualCursor(1); ok {
				return next, nil
			}
			if m.historyRecallActive() {
				return m.recallHistory(1), nil
			}
		case keyIs(msg, tea.KeyUp):
			// ArrowUp exits subchat view (returns to main chat).
			if m.subchat.active {
				m.chatScrollOffset = m.subchat.exit()
				m = m.clearHover() // bodyY numbering differs between subchat and the parent transcript
				return m, nil
			}
			if m.transcriptDetailed {
				m = m.clearHover()
				return m.scrollChat(1), nil
			}
			if next, cmd, ok := m.moveModalSelection(-1); ok {
				return next, cmd
			}
			if next, ok := m.moveComposerVisualCursor(-1); ok {
				return next, nil
			}
			// A queued message takes ↑ priority over history recall: pop it back
			// into the composer for editing before it sends on the next turn.
			if m.hasQueuedMessage() && m.pendingSpecReview == nil {
				return m.popQueuedMessageForEdit(), nil
			}
			if m.historyRecallActive() {
				return m.recallHistory(-1), nil
			}
		case keyCtrl(msg, 'u'):
			// Ctrl+U scrolls up half a page, or moves the cursor up in
			// permission/ask-user prompts. Falls through to the active modal
			// (wizard, etc.) when none of the above are in focus.
			if m.transcriptDetailed {
				return m, nil
			}
			if m.pendingPermission != nil {
				return m.movePermissionCursor(-1), nil
			}
			if m.pendingAskUser != nil {
				return m.moveAskUserCursor(-1), nil
			}
			if m.providerWizard != nil || m.mcpAddWizard != nil || m.mcpManager != nil || m.picker != nil || m.pendingSpecReview != nil {
				break
			}
			if m.composerValue() != "" {
				break // let the input handle its own Ctrl+U (delete-to-bol)
			}
			m = m.clearHover()
			return m.scrollChat(m.chatPageScrollLines()), nil
		case keyCtrl(msg, 'd'):
			// Ctrl+D scrolls down half a page, or moves the cursor down in
			// permission/ask-user prompts. Falls through to the active modal
			// when none of the above are in focus.
			if m.transcriptDetailed {
				return m, nil
			}
			if m.pendingPermission != nil {
				return m.movePermissionCursor(1), nil
			}
			if m.pendingAskUser != nil {
				return m.moveAskUserCursor(1), nil
			}
			if m.providerWizard != nil || m.mcpAddWizard != nil || m.mcpManager != nil || m.picker != nil || m.pendingSpecReview != nil {
				break
			}
			if m.composerValue() != "" {
				break // let the input handle its own Ctrl+D (delete-next-char)
			}
			m = m.clearHover()
			return m.scrollChat(-m.chatPageScrollLines()), nil
		}
		if m.transcriptDetailed {
			return m, nil
		}
		if m.pendingAskUser != nil {
			_, state, ok := m.pendingAskUser.activeQuestion()
			if !ok {
				return m, nil // Confirm tab: ignore stray keys
			}
			// In picker mode a printable keystroke means the user wants to type their
			// own answer, so flip into free-text first instead of letting the text
			// accumulate invisibly and then be discarded when Enter picks an option.
			if !state.typing && keyPrintable(msg) {
				state.typing = true
				m.input.SetValue("")
			}
			if state.typing {
				var cmd tea.Cmd
				m.input, cmd = m.input.Update(msg)
				return m, cmd
			}
			return m, nil // picker mode: non-navigation keys do nothing
		}
		if m.pendingSpecReview != nil {
			m.burstCount = 0
			return m.handleSpecReviewKey(msg)
		}
		if m.pendingPermission != nil {
			// Feedback mode: a printable keystroke (and editing keys like
			// backspace) types into the shared composer input, mirroring the
			// ask_user free-text path above. Enter/Esc/↑/↓ were already handled
			// earlier in this switch; the remaining keys reach the input here.
			if m.pendingPermission.typing {
				var cmd tea.Cmd
				m.input, cmd = m.input.Update(msg)
				return m, cmd
			}
			m.burstCount = 0
			return m.handlePermissionKey(msg)
		}
		if m.providerWizard != nil {
			m.burstCount = 0
			return m.handleProviderWizardKey(msg)
		}
		if m.mcpAddWizard != nil {
			m.burstCount = 0
			return m.handleMCPAddWizardKey(msg)
		}
		if m.mcpManager != nil {
			m.burstCount = 0
			return m.handleMCPManagerKey(msg)
		}
		// An open picker is modal over the input: swallow remaining keys so they
		// don't type into the field. ↑/↓/Enter/Esc were already handled above.
		if m.picker != nil {
			if m.modelPickerIsLoading() {
				return m, nil
			}
			if keyPrintable(msg) {
				m.picker.appendQuery(keyRunes(msg))
				if m.picker.kind == pickerPet {
					return m.schedulePetPreview()
				}
			}
			return m, nil
		}
		if next, ok := m.applyComposerKey(msg); ok {
			return next, nil
		}
		if m.composerActive && strings.Contains(m.composer.text, "\n") {
			return m, nil
		}
		// The key fell through to the text input: let it update, then refresh the
		// autocomplete match list from the new value.
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		m.resetComposerFromInput()
		m.recomputeSuggestions()
		return m, cmd
	case tea.FocusMsg:
		m.terminalFocused = true
		// Sync the caret with focus immediately: leaving it to the next
		// composerBlinkMsg tick can keep it hidden for up to a tick after the
		// terminal regains focus (and, on blur below, leave it visible in an
		// unfocused terminal for the same window).
		m.composerCursorVisible = true
		if m.notifier != nil {
			m.notifier.SetFocused(true)
		}
		return m, nil
	case tea.BlurMsg:
		var petMouseCmd tea.Cmd
		if m.petDragActive {
			pixelDrag := m.petPixelDrag
			m.cancelPetDrag()
			m.lastKeyTime = time.Time{}
			m.burstCount = 0
			if pixelDrag {
				petMouseCmd = petPixelMouseDisableCmd()
			}
		}
		m.terminalFocused = false
		m.composerCursorVisible = false
		m.composerBlinkTicking = false
		m.composerBlinkSeq++
		if m.notifier != nil {
			m.notifier.SetFocused(false)
		}
		return m, petMouseCmd
	case toolCallStreamStartMsg:
		if msg.runID != m.activeRunID {
			return m, nil
		}
		// A new tool call opened — reset the live "writing" block to it.
		m.streamCallID = msg.id
		m.streamCallName = msg.name
		m.streamCallDecoder = newStreamingDecoder()
		return m, nil
	case toolCallStreamDeltaMsg:
		if msg.runID != m.activeRunID || msg.id != m.streamCallID || m.streamCallDecoder == nil {
			return m, nil
		}
		m.streamCallDecoder.feed(msg.fragment)
		// A streamed tool-call argument (e.g. a file's contents in write_file) is
		// real generated output: count it toward the live token estimate so the
		// "↑ N tok" pulse climbs during a long write, and bump lastStreamActivity so
		// the quiet-generation hint stays clear of an actively-streaming provider.
		m.turnStreamedRunes += utf8.RuneCountInString(msg.fragment)
		m.lastStreamActivity = m.now()
		return m, nil
	case agentTextMsg:
		if msg.runID != m.activeRunID {
			return m, nil
		}
		// Streaming text means any in-progress tool call has finished — clear the
		// live "writing" block so it doesn't linger over new prose.
		m.clearStreamingToolCall()
		m.streamingText = append(m.streamingText, msg.delta...)
		m.turnStreamedRunes += utf8.RuneCountInString(msg.delta)
		// recordStreamingDelta appends a time.Time to lineAges for every
		// newline in the delta and bumps lastStreamActivity. It also
		// re-stamps the in-progress last entry so the line that's still
		// being filled stays visibly fresh.
		m.recordStreamingDelta(msg.delta)
		var cmds []tea.Cmd
		// The streaming caret (appendStreamingCursor) is appended to whatever
		// visual line is currently last. Some terminal/renderer combinations
		// (observed over multipass + Windows Terminal) fail to clear the
		// caret's old cell when a newline moves it to a new line, leaving
		// ghost carets behind. A newline is exactly the moment that risk
		// exists, so force one full-screen redraw right then rather than
		// leaving it to the incremental diff. Rate-limited: heavy streaming
		// output (code, logs, diffs) would otherwise turn every coalesced
		// newline into a full-screen repaint, a real throughput/latency cost
		// on SSH and slow links. ~10 redraws/second is enough to keep the
		// caret clean without dominating the write path; terminals that
		// render scroll regions correctly can opt out entirely with
		// ZERO_NO_STREAM_CLEAR=1. A newline that arrives inside the throttle
		// window still owes a repair — it's marked pending and a one-shot
		// timer is scheduled to flush it (see streamClearFlushMsg), instead
		// of being dropped outright. That covers a throttled newline that
		// turns out to be the turn's last one (agentResponseMsg also flushes
		// any still-pending clear at stream end, belt-and-suspenders) as
		// well as one buried in the middle of a long, still-streaming turn.
		if strings.Contains(msg.delta, "\n") && !m.streamClearDisabled {
			now := m.now()
			if elapsed := now.Sub(m.lastStreamClear); elapsed >= streamClearThrottle {
				m.lastStreamClear = now
				m.pendingStreamClear = false
				cmds = append(cmds, tea.ClearScreen)
			} else if !m.pendingStreamClear {
				m.pendingStreamClear = true
				cmds = append(cmds, scheduleStreamClearFlush(streamClearThrottle-elapsed))
			}
		}
		// The fade's tick is self-perpetuating (the streamingFadeTickMsg
		// case schedules the next one). Schedule the FIRST tick only on
		// the inactive→active transition; subsequent deltas just refresh
		// state and rely on the existing tick chain.
		// When the fade is disabled (ZERO_NO_FADE / SSH / tmux / low-color),
		// fadeActive stays false so styleStreamingLine renders streaming text
		// statically at base ink, and no self-perpetuating tick is scheduled.
		if !m.fadeDisabled {
			startTick := !m.fadeActive
			m.fadeActive = true
			if startTick {
				cmds = append(cmds, streamingFadeTick())
			}
		}
		return m, tea.Batch(cmds...)
	case agentReasoningMsg:
		if msg.runID != m.activeRunID {
			return m, nil
		}
		m.streamingReasoning += msg.delta
		m.turnStreamedRunes += utf8.RuneCountInString(msg.delta)
		// Reasoning IS live provider output, so refresh the activity clock — else the
		// quiet-generation hint can wrongly read "still generating…" mid-think.
		if msg.delta != "" {
			m.lastStreamActivity = m.now()
		}
		return m, nil
	case spinner.TickMsg:
		// Record when swarm members first finish so the sidebar can linger them
		// with a fading ✓ before removal. Cheap (the tick only fires while a run is
		// in flight or the sidebar holds agents — exactly when this can change).
		m.stampSwarmDone()
		// Not forwarding the tick while idle stops the spinner's self-scheduling,
		// so no timer fires between runs. The one exception is active agent state:
		// its short lifecycle fade needs the phase to keep advancing until the
		// agents clear.
		if !m.pending && !m.compactInFlight && !m.doctorInFlight {
			// The tick also keeps advancing while the aimlapi.com onboarding sub-flow is
			// busy (its progress screen is spinner-only), even though no agent run is in
			// flight, so its shared MiniDot spinner keeps animating.
			//
			// Return the FPS-throttled tick that Update hands back — NOT m.spinner.Tick,
			// which fires immediately and busy-loops the frame at event-loop speed. That
			// makes the glyph spin far too fast (and burns CPU) on screens that sit here
			// for a while, e.g. the aimlapi checkout wait. The active-run path below
			// already does this; this keeps idle animation at the same cadence.
			if (m.sidebarHasAgents() || m.aimlapiOnboardAnimating()) && !m.reducedMotion {
				var cmd tea.Cmd
				m.spinner, cmd = m.spinner.Update(msg)
				m.spinnerPhase++
				m.spinnerTicking = true
				return m, cmd
			}
			m.spinnerTicking = false
			return m, nil
		}
		m.spinnerTicking = true
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		// Advance bounded secondary motion in lock-step with the activity glyph;
		// freeze it under reduced motion.
		if !m.reducedMotion {
			m.spinnerPhase++
		}
		if m.compactInFlight {
			if !m.reducedMotion {
				m.compactFrame++ // frozen frame under reduced motion -> static ring
			}
			m = m.setCompactStatusRow(m.compactText(true))
		}
		if m.doctorInFlight {
			if !m.reducedMotion {
				m.doctorFrame++
			}
			m = m.setDoctorStatusRow(m.doctorConnectivityRunningText())
		}
		return m, cmd
	case streamingFadeTickMsg:
		// The fade's own tick (separate from the spinner so a slower
		// cadence is enough). Short-circuits when fadeActive is false,
		// which is how the ticker stops cleanly at stream end: the
		// agentResponseMsg handler sets fadeActive = false, and the
		// next tick that lands after that point returns nil here.
		if !m.fadeActive {
			return m, nil
		}
		return m, streamingFadeTick()
	case streamClearFlushMsg:
		// Flush a newline-triggered redraw that the stream-clear throttle
		// deferred (see agentTextMsg) once its window has elapsed. This runs
		// independent of the streaming fade (which is unconditionally off —
		// fadeDisabled is hardcoded true in newModel — so its tick can't be
		// relied on to drive this), and independent of stream end: a turn
		// that keeps streaming for a while after the throttled newline would
		// otherwise leave the ghost caret up for the rest of the turn.
		now := m.now()
		if m.pendingStreamClear && now.Sub(m.lastStreamClear) >= streamClearThrottle {
			m.lastStreamClear = now
			m.pendingStreamClear = false
			return m, tea.ClearScreen
		}
		return m, nil
	case tea.WindowSizeMsg:
		m.resizeFreePetPosition(m.width, m.height, msg.Width, msg.Height)
		m.width = msg.Width
		m.height = msg.Height
		if msg.Width < runDetailsMinWidth {
			m.runDetailsOpen = false
		}
		// A resize re-wraps content at a new width, shifting every row's bodyY;
		// a stale transcript-hover target could coincidentally land on an
		// unrelated clickable row (same reasoning as clearHover's other callers).
		m = m.clearHover()
		// Reset the streaming-text fade state. A width change can re-wrap
		// the in-progress text into a different number of visual lines,
		// which invalidates the per-line age mapping. The next delta
		// will reseed lineAges and restart the tick.
		m.lineAges = nil
		m.lastStreamActivity = m.now()
		// Size the composer so long input scrolls horizontally with the cursor
		// visible instead of being clipped invisibly past the right edge.
		m.input.SetWidth(maxInt(20, chatWidth(msg.Width)-14))
		// An attached terminal fills the viewport, so the PTY tracks the
		// live size instead of a fixed default.
		m = m.resizeAttachedTerminalPTY()
		// The title bar prints once into native scrollback when the inline
		// renderer is active. In alt-screen mode it stays pinned inside View.
		if !m.altScreen && !m.headerPrinted && msg.Width > 0 {
			m.headerPrinted = true
			m.flushQueue = append(m.flushQueue, m.titleBar(chatWidth(msg.Width)))
		}
		// A resumed/idle session may already hold agents; keep their short lifecycle
		// fade alive. No-op when the loop is already running or nothing animates.
		return m, m.ensureSpinnerTick()
	case permissionRequestMsg:
		// The agent goroutine that raised this request is BLOCKED waiting on the
		// decision callback, so every branch below must resolve it exactly once —
		// or store it in pendingPermission, which resolves it on the user's reply.
		// Dropping a request without resolving parks the run forever (the reported
		// "stuck for 33 minutes" deadlock): the agent waits on a decision channel
		// nothing will ever signal, with no visible prompt and no network activity.
		if msg.runID != m.activeRunID {
			// A superseded/stale run: unblock its parked goroutine now rather than
			// relying on that run's context cancel to fire first.
			if msg.decide != nil {
				msg.decide(agent.PermissionDecision{Action: agent.PermissionDecisionCancel, Reason: "run superseded"})
			}
			return m, nil
		}
		if msg.request.Action != agent.PermissionActionPrompt {
			// Not a user-facing prompt (e.g. a sandbox-allowed command that still
			// blocked because it requested additional permissions). The UI has
			// nothing to ask, so resolve immediately and FAIL CLOSED — never
			// silently grant access that was never surfaced to the user. (The agent
			// now marks such elevation requests as prompts, so in practice this is a
			// defensive backstop; matches the ACP handler's fail-closed contract.)
			if msg.decide != nil {
				msg.decide(autoResolvedPermissionDecision(msg.request.Action))
			}
			return m, nil
		}
		promptEvent := permissionEventFromRequest(msg.request)
		promptRow := permissionTranscriptRow(promptEvent)
		// The focused modal owns the actionable reason while the decision is
		// pending. Keep only structured block context in the transcript row so
		// the same explanation is not shown immediately above the card.
		if promptEvent.Block == nil {
			promptRow.detail = ""
		} else {
			promptRow.detail = permissionBlockDetail(promptEvent)
		}
		promptRow.runID = msg.runID
		m.transcript = appendTranscriptRow(m.transcript, promptRow)
		m.pendingPermission = &pendingPermissionPrompt{
			request: msg.request,
			decide:  msg.decide,
		}
		return m, nil
	case askUserRequestMsg:
		if msg.runID != m.activeRunID {
			if msg.answer != nil {
				msg.answer(nil)
			}
			return m, nil
		}
		// A request with no questions has nothing to answer — resolve it
		// immediately so the run isn't stalled waiting on manual input. Mirror the
		// normal flow: record the (empty) request in the transcript and answer with
		// an empty slice (not nil) so downstream sees the same Answers shape.
		if len(msg.request.Questions) == 0 {
			m.transcript = appendTranscriptRow(m.transcript, askUserTranscriptRow(msg.request))
			if msg.answer != nil {
				msg.answer([]string{})
			}
			return m, nil
		}
		m.transcript = appendTranscriptRow(m.transcript, askUserTranscriptRow(msg.request))
		m.pendingAskUser = &pendingAskUserPrompt{
			request: msg.request,
			answer:  msg.answer,
			states:  newAskUserStates(msg.request.Questions),
		}
		m.clearComposer()
		m.clearSuggestions()
		return m, nil
	case loopTickMsg:
		// Idle poll for due loops. A stale tick (loops changed) or an empty loop set
		// ends the ticker; otherwise fire the earliest due loop if idle and reschedule.
		if msg.seq != m.loopSeq || len(m.loops) == 0 {
			m.loopTicking = false
			return m, nil
		}
		var fireCmd tea.Cmd
		m, fireCmd = m.fireDueLoopIfIdle()
		return m, tea.Batch(fireCmd, m.scheduleLoopTick())
	case agentResponseMsg:
		if msg.runID != m.activeRunID {
			// A run cancelled while in flight still finishes in its goroutine and
			// returns its accumulated session events here. Persist ONLY those events
			// (notably the EventSessionCheckpoint payloads captured before each
			// mutating tool) so the checkpoint blobs stay referenced and /rewind
			// works; the cancel path already wrote the "Run cancelled." marker, so
			// skip transcript rows, the trailing cancellation error, and any pending
			// state changes.
			if flushSessionID, flushing := m.flushRunIDs[msg.runID]; flushing {
				delete(m.flushRunIDs, msg.runID)
				// The cancelled run still consumed tokens; record them so the usage
				// readout doesn't undercount interrupted turns.
				liveUsageCount := m.liveUsageCounts[msg.runID]
				for index, event := range msg.usageEvents {
					if index < liveUsageCount {
						continue
					}
					var usageRows []transcriptRow
					m, usageRows = m.recordUsageEvent(msg.usageModelIDAt(index), event)
					for _, row := range usageRows {
						m.transcript = appendTranscriptRow(m.transcript, row)
					}
				}
				delete(m.liveUsageCounts, msg.runID)
				// Events are persisted into the session the run was recording into AT
				// CANCEL TIME — the active session may have changed since (/resume),
				// and writing there would contaminate its log with checkpoint payloads
				// whose blobs live under the original session. appendSessionEvents*
				// only returns rows for persist FAILURES; surface them so a failed
				// checkpoint/tool flush (which would silently degrade /rewind) is
				// visible rather than swallowed.
				var flushRows []transcriptRow
				events := flushableSessionEvents(msg.sessionEvents)
				if flushSessionID == m.activeSession.SessionID {
					m, flushRows = m.appendSessionEvents(events)
				} else {
					flushRows = m.appendSessionEventsTo(flushSessionID, events)
				}
				for _, row := range flushRows {
					m.transcript = appendTranscriptRow(m.transcript, row)
				}
				// A Ctrl+C during an in-flight run defers its quit until the run's
				// checkpoint session events have been flushed (above). Now that the
				// last pending flush is drained, fire the deferred quit.
				if m.exiting && len(m.flushRunIDs) == 0 {
					return m.quit()
				}
			}
			return m, nil
		}
		m.clearStreamingToolCall() // active run finished — drop any lingering "writing" block
		if msg.err != nil {
			m.petOutcome = terminalpet.Failed
		} else {
			m.petOutcome = terminalpet.Review
		}
		m.petOutcomeAt = m.now()
		m.pending = false
		m = m.disarmCancelConfirmation() // the run finished on its own — nothing left to confirm cancelling
		// A newline-triggered redraw deferred by the stream-clear throttle
		// (see agentTextMsg) may never get a later newline or fade tick to
		// flush it if this was the turn's last delta — flush it here so the
		// ghost caret isn't left behind at stream end.
		var pendingClearCmd tea.Cmd
		if m.pendingStreamClear {
			m.pendingStreamClear = false
			pendingClearCmd = tea.ClearScreen
		}
		// Fully reset the fade state at stream end. The next render
		// emits the final row in solid ink (no settling animation), and
		// the pending streamingFadeTickMsg that lands after this point
		// short-circuits because fadeActive is false. Clearing lineAges
		// and lastStreamActivity here too prevents stale age data from
		// carrying over to the next turn (and stops lineAges from
		// growing indefinitely across many runs).
		m.resetStreamingFade()
		// The run is complete: release its context now instead of waiting for the
		// parent context — every prompt leaked a CancelFunc (and its timer
		// resources) until app exit otherwise.
		if m.runCancel != nil {
			m.runCancel()
		}
		m.runCancel = nil
		m.activeRunID = 0
		m.terminalAutoAttach = nil
		m.plan.frozenAt = m.now() // freeze the plan clock while idle (no run in flight)
		// A fully successful turn means the task is done. Weaker models often
		// forget the final update_plan, leaving the panel stuck mid-progress;
		// reconcile it to complete here. Read pendingAskUser/pendingPermission
		// BEFORE the reset below clears them, and skip spec-draft reviews — those
		// are legitimate mid-plan err==nil yields where the plan is NOT done.
		if msg.err == nil && msg.specReview == nil &&
			m.pendingAskUser == nil && m.pendingPermission == nil {
			m.plan.completeRemaining(m.now())
		}
		m.pendingPermission = nil
		if m.pendingAskUser != nil && m.pendingAskUser.answer != nil {
			m.pendingAskUser.answer(nil)
		}
		m.pendingAskUser = nil
		liveUsageCount := m.liveUsageCounts[msg.runID]
		for index, event := range msg.usageEvents {
			if index < liveUsageCount {
				continue
			}
			var usageRows []transcriptRow
			m, usageRows = m.recordUsageEvent(msg.usageModelIDAt(index), event)
			for _, row := range usageRows {
				m.transcript = appendTranscriptRow(m.transcript, row)
			}
		}
		delete(m.liveUsageCounts, msg.runID)
		var sessionRows []transcriptRow
		m, sessionRows = m.appendSessionEvents(msg.sessionEvents)
		for _, row := range sessionRows {
			m.transcript = appendTranscriptRow(m.transcript, row)
		}
		for _, row := range msg.rows {
			if row.kind == rowReasoning {
				m.streamingReasoning = ""
				m.streamingReasoningExpanded = false
			}
			m.transcript = appendTranscriptRow(m.transcript, row)
		}
		if msg.err != nil {
			// A failed turn has no final answer row to supersede the streamed
			// text the user already watched — keep the partial answer instead of
			// letting it vanish from history.
			if row, ok := reasoningTranscriptRow("", msg.runID, m.streamingReasoning); ok {
				m.transcript = appendTranscriptRow(m.transcript, row)
			}
			if text := strings.TrimRight(m.streamingTextString(), "\n"); strings.TrimSpace(text) != "" {
				m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowAssistant, text: text})
			}
			// The error row terminates the turn, so it carries the done-line
			// metadata a final assistant row would have carried. A recognized
			// provider failure (auth/rate-limit/connectivity/…) also carries a
			// one-line next step so the user isn't left staring at a raw blob.
			m.transcript = appendTranscriptRow(m.transcript, transcriptRow{
				kind:        rowError,
				text:        msg.err.Error(),
				hint:        errhint.TUIHint(msg.err),
				final:       true,
				turnTools:   msg.turnTools,
				turnElapsed: msg.turnElapsed,
			})
		}
		if m.runCompletionWarning != nil {
			if notice := strings.TrimSpace(m.runCompletionWarning()); notice != "" {
				m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowSystem, text: notice})
			}
		}
		m.streamingText = nil
		m.streamingReasoning = ""
		m.streamingReasoningExpanded = false
		if msg.goalAware {
			m = m.reconcileGoalAfterRun(msg.usageEvents, msg.err)
		}
		// Roll the completed run's wall-time into the session's rolling average so
		// /context can surface typical turn latency, not just token counts.
		if msg.turnElapsed > 0 {
			m.turnLatencySum += msg.turnElapsed
			m.turnLatencyCount++
		}
		if msg.ttft > 0 {
			m.turnTTFTSum += msg.ttft
			m.turnTTFTCount++
		}
		if msg.specReview != nil {
			m = m.activateSpecReview(*msg.specReview)
		}
		if m.notifier != nil {
			m.notifier.Notify(notify.Completion, notify.DefaultMessage(notify.Completion))
		}
		// A successful turn gives the session real content; if it still carries its
		// default first-message title, generate a concise one in the background
		// (one-shot per session). A failed turn is skipped — there's nothing to name.
		var titleCmd, recapCmd tea.Cmd
		if msg.err == nil {
			m, titleCmd = m.maybeAutoTitleActiveSession()
			// Arm an idle recap only after a successful answer. The timer performs
			// no provider work unless the session stays untouched long enough.
			var finalAnswer string
			for _, row := range msg.rows {
				if row.kind == rowAssistant && row.final {
					finalAnswer = row.text
				}
			}
			m, recapCmd = m.maybeScheduleIdleRecap(msg.runID, finalAnswer)
		}
		// End-of-turn git sweep: catch file mutations the tool stream couldn't
		// report (bash scaffolding, subagent edits) so the FILES sidebar is
		// complete once the turn settles.
		var sweepCmd tea.Cmd
		m, sweepCmd = m.maybeGitSweep()
		// If this run was a loop iteration, advance that loop (schedule its next wake
		// or stop it). Done before launchQueuedMessageIfReady so a user's queued prompt
		// still wins the immediate re-launch; the loop fires on the next idle tick.
		var loopTickCmd tea.Cmd
		if loopID := m.activeLoopID; loopID != "" {
			m.activeLoopID = ""
			loopFinalAnswer := ""
			for _, row := range msg.rows {
				if row.kind == rowAssistant && row.final {
					loopFinalAnswer = row.text
				}
			}
			m = m.advanceLoop(loopID, loopFinalAnswer, msg.err)
			// advanceLoop -> removeLoop may have stopped the ticker; restart it if
			// other loops remain.
			m, loopTickCmd = m.ensureLoopTick()
		}
		hadQueuedMessage := strings.TrimSpace(m.queuedMessage) != ""
		next, queuedCmd := m.launchQueuedMessageIfReady()
		var peerCmd tea.Cmd
		if queuedCmd == nil {
			next, peerCmd = next.launchQueuedPeerIfReady()
		}
		var peerApprovalCmd tea.Cmd
		if queuedCmd == nil && peerCmd == nil {
			next, peerApprovalCmd = next.openNextPeerApproval()
		}
		var goalCmd tea.Cmd
		if msg.goalAware && !hadQueuedMessage && peerCmd == nil && next.pendingPermission == nil && msg.specReview == nil {
			next, goalCmd = next.launchGoalContinuationIfReady()
		}
		return next, tea.Batch(pendingClearCmd, titleCmd, recapCmd, sweepCmd, queuedCmd, peerCmd, peerApprovalCmd, loopTickCmd, goalCmd)
	case sessionTitleGeneratedMsg:
		return m.handleSessionTitleGenerated(msg)
	case recapIdleMsg:
		return m.handleRecapIdle(msg)
	case recapGeneratedMsg:
		return m.handleRecapGenerated(msg)
	case compactResultMsg:
		if !m.compactInFlight {
			return m, nil
		}
		m.compactInFlight = false
		m.compactFrame = 0
		m.lastCompactResult = nil
		m.lastCompactError = ""
		if msg.err != nil {
			m.lastCompactError = msg.err.Error()
			m = m.setCompactStatusRow(m.compactText(true))
			return m, nil
		}
		if msg.hasSessionSnapshot {
			m.activeSession = msg.activeSession
			m.sessionEvents = append([]sessions.Event{}, msg.sessionEvents...)
			m.transcript = append([]transcriptRow{}, msg.transcript...)
			m.resetFlushFrontier("· compacted ·")
		}
		m.lastCompactResult = &msg.result
		m = m.setCompactStatusRow(m.compactText(true))
		return m, nil
	case planUpdateMsg:
		if msg.runID != m.activeRunID {
			return m, nil
		}
		m.plan.updateFromItems(msg.items, m.now())
		return m, nil
	case planStepExplanationMsg:
		// Drop a result from a previous run: beginRun bumps planDetailGen and clears
		// stepExplanation, so a stale in-flight write-up must not repopulate the
		// cache or overwrite the new run's data.
		if msg.gen != m.planDetailGen {
			return m, nil
		}
		// Cache the write-up so re-clicking the step is instant; an empty result
		// (failed/blank) caches "" so the card shows the local fallback summary and
		// we don't retry the model on every re-click. Only re-render the card when
		// this step's detail is still the one open (the user may have closed it or
		// clicked another step while the request was in flight).
		if m.stepExplanation == nil {
			m.stepExplanation = map[string]string{}
		}
		text := strings.TrimSpace(msg.text)
		if msg.err != nil {
			text = ""
		}
		m.stepExplanation[msg.key] = text
		if m.planDetailOpen && m.planDetailStep == msg.stepIndex &&
			msg.stepIndex >= 0 && msg.stepIndex < len(m.plan.steps) &&
			planStepExplanationKey(m.plan.steps[msg.stepIndex]) == msg.key {
			m.transcript = dropTranscriptRowsByID(m.transcript, planStepDetailRowID)
			m.transcript = m.appendPlanStepCard(msg.stepIndex, text, false)
		}
		return m, nil
	case agentUsageMsg:
		if msg.runID != m.activeRunID {
			return m, nil
		}
		var usageRows []transcriptRow
		m, usageRows = m.recordUsageEvent(msg.modelID, msg.usage)
		if m.liveUsageCounts == nil {
			m.liveUsageCounts = map[int]int{}
		}
		m.liveUsageCounts[msg.runID]++
		for _, row := range usageRows {
			m.transcript = appendTranscriptRow(m.transcript, row)
		}
		return m, nil
	case specialistStartMsg:
		if msg.runID != m.activeRunID {
			return m, nil
		}
		m.specialists.start(msg.name, msg.description, msg.childSessionID, m.now())
		return m, nil
	case specialistCompleteMsg:
		if msg.runID != m.activeRunID {
			return m, nil
		}
		// The specialist was started with the tool call ID as a temporary key
		// (the real session ID isn't known until the child process creates it).
		// Reconcile: complete by the tool call ID, then rewrite the tracker
		// entry's childSessionID to the real session ID so subchat.enter can
		// find the child session's events in the store.
		m.specialists.complete(msg.toolCallID, msg.status, 0, msg.errorMsg, m.now())
		if msg.childSessionID != "" && msg.childSessionID != msg.toolCallID {
			m.specialists.reconcileSessionID(msg.toolCallID, msg.childSessionID)
		}
		if info, ok := m.specialists.getBySessionID(msg.childSessionID); ok {
			if info.childSessionID == "" {
				info.childSessionID = msg.toolCallID
			}
			cardRow := transcriptRow{
				kind:           rowSpecialist,
				runID:          msg.runID,
				specialistInfo: &info,
			}
			m.transcript = appendTranscriptRow(m.transcript, cardRow)
		}
		return m, nil
	case specialistProgressMsg:
		if msg.runID != m.activeRunID {
			return m, nil
		}
		// Each progress message is one specialist tool call (OnToolProgress fires only
		// for EventToolCall); bump the card's tool-call counter so it stops showing a
		// permanent "0 tool calls" (M18). The tracker is still keyed by the tool-call
		// id at this point (reconciled to the session id only on completion).
		m.specialists.incrementToolCount(msg.toolCallID)
		m.specialists.setCurrentTool(msg.toolCallID, msg.toolName, msg.detail)
		return m, nil
	case agentRowMsg:
		if msg.runID != m.activeRunID {
			return m, nil
		}
		if msg.row.kind == rowReasoning {
			m.streamingReasoning = ""
			m.streamingReasoningExpanded = false
		}
		// A tool call ends the current streamed text segment. The segment is the
		// assistant's working narration ("Let me check X…") — append it as a
		// non-final assistant row so it stays in history instead of silently
		// vanishing when the tool card replaces the interim block.
		if msg.row.kind == rowToolCall {
			if row, ok := reasoningTranscriptRow("", msg.runID, m.streamingReasoning); ok {
				m.transcript = appendTranscriptRow(m.transcript, row)
				m.streamingReasoning = ""
				m.streamingReasoningExpanded = false
			}
			if text := strings.TrimRight(m.streamingTextString(), "\n"); strings.TrimSpace(text) != "" {
				m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowAssistant, text: text})
				// This interim narration is the agent explaining what it's about to
				// do — attribute it to the active plan step so the step-detail card
				// can replay the agent's own account of the work.
				m = m.captureStepNarration(text)
			}
			m.streamingText = nil
			// The tool call has finalized into its card — drop the live "writing"
			// preview so it doesn't linger or duplicate beneath the card.
			m.clearStreamingToolCall()
		}
		// Collapse a repeated swarm status/collect card so re-checks don't flood
		// the chat with identical blocks.
		beforeCollapse := len(m.transcript)
		m.transcript = collapseRepeatedStatusCard(m.transcript, msg.row)
		if removed := beforeCollapse - len(m.transcript); removed > 0 {
			m.flushed = max(0, m.flushed-removed)
			m.altScreenSettledWidth = 0
		}
		m.transcript = appendTranscriptRow(m.transcript, msg.row)
		m = m.captureStepWork(msg.row)
		// A finished command tool may have mutated files git can see but no
		// changedFiles reports (npm create, heredoc writes, subagent edits) —
		// re-sweep so the FILES sidebar picks them up mid-turn.
		if msg.row.kind == rowToolResult && isPlanCommandTool(msg.row.tool) {
			var sweep tea.Cmd
			m, sweep = m.maybeGitSweep()
			return m, sweep
		}
		return m, nil
	case swarmSessionsMsg:
		// Merge completed swarm members' session ids so their AGENTS sidebar rows
		// become drill-in clickable. Session ids are durable facts, so this is not
		// gated on the active run.
		if m.swarmSessionMap == nil {
			m.swarmSessionMap = map[string]string{}
		}
		for taskID, sessionID := range msg.sessions {
			if taskID != "" && sessionID != "" {
				m.swarmSessionMap[taskID] = sessionID
			}
		}
		return m, nil
	case doctorCommandResultMsg:
		if msg.id == 0 || msg.id == m.doctorCommandSeq {
			m.doctorInFlight = false
			m.doctorFrame = 0
			m = m.setDoctorStatusRow(msg.text)
		}
		return m, nil
	case sandboxSetupCommandResultMsg:
		if msg.id == 0 || msg.id == m.sandboxSetupSeq {
			m.sandboxSetupInFlight = false
			m = m.setSandboxSetupStatusRow(sandboxSetupResultText(msg.result))
		}
		return m, nil
	case prStateMsg:
		m.prState = msg.state
		return m, nil
	case gitSweepMsg:
		return m.handleGitSweepMsg(msg), nil
	case prWatcherStartedMsg:
		if msg.stop == nil {
			return m, nil
		}
		if m.prWatcherStop != nil {
			m.prWatcherStop()
		}
		m.prWatcherStop = msg.stop
		return m, nil
	case bashResultMsg:
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: msg.output})
		return m, nil
	case providerModelsDiscoveredMsg:
		return m.applyProviderModelsDiscovered(msg), nil
	case setupModelsDiscoveredMsg:
		return m.applySetupModelsDiscovered(msg), nil
	case setupOAuthMsg:
		return m.applySetupOAuth(msg)
	case setupOAuthDeviceMsg:
		return m.applySetupOAuthDeviceCode(msg)
	case modelPickerModelsDiscoveredMsg:
		return m.applyModelPickerModelsDiscovered(msg), nil
	case petCatalogLoadedMsg:
		return m.applyPetCatalog(msg)
	case petPreviewDebounceMsg:
		return m.startPetPreview(msg)
	case petPreviewLoadedMsg:
		m = m.applyPetPreview(msg)
		if m.petPreview != nil && m.petAnimation == nil && !m.reducedMotion {
			m.petTickSeq++
			return m, petTickCmd(m.petTickSeq, m.petFrameDelay())
		}
		return m, nil
	case petInstalledMsg:
		return m.applyPetInstall(msg)
	case petTickMsg:
		if msg.seq != m.petTickSeq {
			return m, nil
		}
		if (m.petAnimation == nil && (m.picker == nil || m.petPreview == nil)) || m.reducedMotion {
			return m, nil
		}
		_, state := m.petPlayback()
		if state != m.petPlaybackState {
			m.petPlaybackState = state
			m.petPhase = 0
		} else {
			m.petPhase++
		}
		return m, petTickCmd(m.petTickSeq, m.petFrameDelay())
	case ollamaContextWindowDiscoveredMsg:
		if msg.err == nil && msg.contextWindow > 0 {
			if m.ollamaContextWindowByModel == nil {
				m.ollamaContextWindowByModel = map[string]int{}
			}
			m.ollamaContextWindowByModel[msg.modelName] = msg.contextWindow
		}
		return m, nil
	case mcpCommandResultMsg:
		return m.applyMCPCommandResultMessage(msg), nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m model) View() tea.View {
	var content string
	if m.setup.visible {
		content = m.setupView(chatWidth(m.width))
	} else if m.helpOverlay || m.leaderHelpOverlay || !m.transcriptDetailed {
		// When helpOverlay / leaderHelpOverlay is active the panel is composited into
		// the normal transcript view as a true overlay (scrim + vertical centering), matching
		// how the suggestion picker / provider wizard / pickers are drawn.
		content = m.transcriptView()
	} else {
		content = m.detailedTranscriptView()
	}
	if m.petRenderer != nil {
		m.petRenderer.Set(m.petImageDraw(content))
	}
	for index, renderer := range m.attachmentRenderers {
		renderer.Set(m.attachmentImageDraw(index))
	}

	view := tea.NewView(content)
	view.AltScreen = m.altScreen
	// Keep the terminal's canvas intact. Named themes may color local cards, but
	// Zero never replaces the user's background, opacity, wallpaper, or profile.
	// Always requested, independent of the notifier: the composer cursor's
	// focus/blink behavior (composerBlinkMsg above) needs tea.FocusMsg/BlurMsg
	// regardless of notification config. A standard, widely supported DEC
	// private mode (CSI ?1004h) that unsupported terminals silently ignore.
	view.ReportFocus = true
	// Voice mode's Space-hold gesture needs key-release events (Kitty protocol).
	// Request them only while voice mode is on — the renderer re-sends the request
	// only when the value changes, so gating this costs nothing (§10).
	view.KeyboardEnhancements.ReportEventTypes = m.dictation.voiceModeEnabled
	if m.wantsMouseCapture() {
		// AllMotion (1003) is what hover highlighting needs: it reports cursor
		// movement with no button pressed, where CellMotion (1002) reports motion
		// only while a button is held. mouseModeFor decides which is safe here,
		// and documents why one is not simply better than the other: a terminal
		// that does not implement 1003 sends nothing at all rather than degrading.
		// The 15ms throttle (mouseEventThrottleInterval) already bounds the redraw
		// rate from AllMotion's extra motion events.
		view.MouseMode = mouseModeFor(runtime.GOOS, os.Getenv, isRunningUnderPRoot())
	}
	return view
}

// transcriptEmpty reports whether the chat surface has no real content yet
// (only the welcome row), which is when the empty state renders.
func (m model) transcriptEmpty() bool {
	for _, row := range m.transcript {
		if row.kind != rowWelcome {
			return false
		}
	}
	return true
}

// transcriptView renders the visible chat surface: in inline mode this is the
// live tail not yet settled into native scrollback; in alt-screen mode it is
// the managed conversation view. Streaming/modal blocks and composer chrome are
// always rendered here.
func (m model) transcriptView() string {
	if m.petLayoutActive() {
		return m.floatingPetTranscriptView()
	}

	width := chatWidth(m.width)

	// Subchat drill-in: when active, show the child session's transcript with
	// a nav bar instead of the main chat.
	if m.subchat.active {
		navBar := renderSubchatNavBar(m.subchat.childSessionTitle, width)
		childBodyItems := m.transcriptBodyItemsFromRows(m.subchat.childRows, width)
		footer := m.footerView(width)
		if m.altScreen && m.height > 0 {
			return m.scrollableTranscriptItemsView(navBar, childBodyItems, footer, width, "")
		}
		bodyLayout := layoutTranscriptBodyItems(childBodyItems)
		body := navBar + "\n\n" + bodyLayout.String()
		return body + footer
	}

	helpOverlayContent := ""
	if m.helpOverlay {
		helpOverlayContent = m.renderKeybindingHelpOverlay(width)
	}
	leaderHelpOverlayContent := ""
	if m.leaderHelpOverlay {
		leaderHelpOverlayContent = m.renderLeaderHelpOverlay(width)
	}

	suggestionOverlay := m.suggestionOverlay(width)
	runDetailsOverlay := m.runDetailsOverlay(width)
	providerOverlay := m.providerWizardOverlay(width)
	mcpAddOverlay := m.mcpAddWizardOverlay(width)
	mcpOverlay := m.mcpManagerOverlay(width)
	pickerOverlay := m.pickerOverlay(width)
	sttKeyOverlay := m.sttKeyPromptOverlay(width)
	terminalAttachOverlay := m.terminalAttachOverlay(width)
	viewportOverlay := ""
	switch {
	case terminalAttachOverlay != "":
		viewportOverlay = terminalAttachOverlay
	case sttKeyOverlay != "":
		viewportOverlay = sttKeyOverlay
	case helpOverlayContent != "":
		viewportOverlay = helpOverlayContent
	case leaderHelpOverlayContent != "":
		viewportOverlay = leaderHelpOverlayContent
	case runDetailsOverlay != "":
		viewportOverlay = runDetailsOverlay
	case providerOverlay != "":
		viewportOverlay = providerOverlay
	case mcpAddOverlay != "":
		viewportOverlay = mcpAddOverlay
	case mcpOverlay != "":
		viewportOverlay = mcpOverlay
	case pickerOverlay != "":
		viewportOverlay = pickerOverlay
	case suggestionOverlay != "":
		viewportOverlay = suggestionOverlay
	}
	emptyOverlay := ""
	if m.transcriptEmpty() && !m.pending && viewportOverlay != "" {
		emptyOverlay = viewportOverlay
	}
	bodyItems := m.transcriptBodyItems(width, emptyOverlay, false)

	footer := m.footerView(width)

	overlayForViewport := viewportOverlay
	if m.transcriptEmpty() && !m.pending && viewportOverlay != "" {
		overlayForViewport = ""
	}

	// Plan panel renders inline in the transcript body (as a transcript row),
	// not pinned at the top. It appears above the specialist cards like a
	// chat message, the way todo/plan updates render inline.
	if m.altScreen && m.height > 0 {
		header := m.pinnedTitleBar(width)
		return m.scrollableTranscriptItemsView(header, bodyItems, footer, width, overlayForViewport)
	}

	bodyLayout := layoutTranscriptBodyItems(bodyItems)
	body := bodyLayout.String()
	if overlayForViewport != "" {
		body += "\n" + overlayForViewport + "\n"
	}
	return body + footer
}

func (m model) titleBarInTranscriptBody() bool {
	return !m.altScreen && !m.headerPrinted
}

func (m model) pinnedTitleBar(width int) string {
	if !m.altScreen || m.height <= 0 {
		return ""
	}
	// The file drill-in replaces the title bar with its nav line (path + key
	// hints). Both are exactly one line, and every frame computation routes
	// through here, so the swap never desyncs the viewport geometry.
	if m.fileView.active {
		return m.fileViewNavBar(width)
	}
	return m.titleBar(width)
}

func (m model) footerView(width int) string {
	var footer strings.Builder
	// An attached terminal owns the keyboard and the viewport; the composer is
	// inert, so like the ask_user and permission modals only the status line
	// renders. Frame math routes through footerView, so this also shrinks the
	// footer rect the overlay's row budget is derived from.
	if m.terminalAttach != nil {
		return m.footerStatusLine(width)
	}
	if m.renamePrompt != nil {
		footer.WriteString(m.sessionRenamePromptView(width))
		footer.WriteString("\n")
		footer.WriteString(m.footerStatusLine(width))
		return footer.String()
	}
	// While an ask-user questionnaire is active it REPLACES the composer box (the
	// text box becomes the questionnaire): render the tabbed prompt + status line and
	// skip the plan panel / idle hints / composer for a focused modal.
	if m.pendingAskUser != nil {
		footer.WriteString(renderAskUserQuestionnaire(*m.pendingAskUser, m.input.Value(), width))
		footer.WriteString("\n")
		footer.WriteString(m.footerStatusLine(width))
		return footer.String()
	}
	// A focused permission prompt owns the keyboard: its options (and the feedback
	// field) consume every key, so the composer is inert. Suppress it and the idle
	// hints/plan panel like the ask_user modal above, keeping only the status line.
	// The card itself renders in the transcript body. This also keeps the shared
	// input from echoing in two places once "tell Zero what to do differently"
	// opens the on-card feedback field.
	if m.pendingPermission != nil {
		footer.WriteString(m.footerStatusLine(width))
		return footer.String()
	}
	// The row above the composer: transient copy feedback takes priority; otherwise
	// a faint idle affordance — discoverable key hints on the left, a jump-to-bottom
	// cue on the right when scrolled up. Always one line (blank when nothing shows),
	// so the footer height is unchanged.
	if copyStatus := strings.TrimSpace(m.copyStatus); copyStatus != "" {
		footer.WriteString(rightAlignedLine(zeroTheme.ink.Render(copyStatus), width))
	} else if notice := m.transientNoticeLine(width); notice != "" {
		footer.WriteString(notice)
	} else if recap := strings.TrimSpace(m.idleRecap); recap != "" {
		footer.WriteString(fitStyledLine("  "+zeroTheme.faint.Render("※ "+recap), width))
	} else if left, right := m.composerIdleHint(), m.jumpToBottomHint(); left != "" || right != "" {
		footer.WriteString(fitStyledLine(joinHeaderLine("  "+left, right, width), width))
	}
	footer.WriteString("\n")
	// A message typed while a run is active is queued for the next turn; show its
	// preview directly ABOVE the input box (not below), so it reads as "waiting to
	// send" sitting on top of what you're currently typing.
	if queued := renderQueuedMessagePreview(m.queuedMessage, width); queued != "" {
		footer.WriteString(queued)
		footer.WriteString("\n")
	}
	footer.WriteString(m.composerBox(width))
	footer.WriteString("\n")
	footer.WriteString(m.footerStatusLine(width))
	return footer.String()
}

// composerIdleHint returns a faint one-line key-shortcut hint shown above the
// composer on an empty, idle prompt, so the chord bindings are discoverable
// without opening the ? overlay. Empty while typing, during a run, in the
// full-screen transcript, or under any modal/overlay so it never competes for
// attention. Width-tiered so a narrow terminal only shows the essential pointer.
func (m model) composerIdleHint() string {
	// Leader-pending is always shown (even mid-type) so the user knows the next
	// key is a chord, not composer input.
	if m.leaderPending {
		return zeroTheme.faint.Render("Ctrl+X — await shortcut (m model · p provider · ? list · Esc cancel)")
	}
	// Managed (alt-screen) mode only: inline mode prints to native scrollback where
	// this footer row isn't a stable surface. Hidden while typing, during a run, in
	// the full-screen transcript, or under any modal/overlay.
	if !m.altScreen || m.pending || m.composerValue() != "" || !m.noBlockingModal() ||
		m.subchat.active || m.suggestionsActive() || m.transcriptDetailed {
		return ""
	}
	sidebarKey := labelOr(m.keyBindings.toggleSidebar, "Ctrl+B")
	detailKey := labelOr(m.keyBindings.toggleDetailed, "Ctrl+O")
	mouseKey := labelOr(m.keyBindings.toggleMouse, "Ctrl+E")

	var hint string
	switch widthTier(m.width) {
	case tierTiny:
		return "" // too cramped for a hint
	case tierNarrow:
		hint = "? shortcuts"
	case tierMedium:
		parts := []string{"? shortcuts", "Ctrl+X cmds"}
		if m.runDetailsAvailable() {
			parts = append(parts, sidebarKey+" details")
		}
		hint = strings.Join(parts, " · ")
	default:
		parts := []string{"? shortcuts", "Ctrl+X cmds"}
		if m.runDetailsAvailable() {
			parts = append(parts, sidebarKey+" details")
		}
		parts = append(parts, detailKey+" detail", mouseKey+" copy", "Shift+Tab mode")
		hint = strings.Join(parts, " · ")
	}
	return zeroTheme.faint.Render(hint)
}

// jumpToBottomHint returns a faint "↓ N more · PgDn" cue when the transcript is
// scrolled up (chatScrollOffset counts lines below the fold), so it's clear new
// output may be below and how to catch up. Empty at the bottom.
func (m model) jumpToBottomHint() string {
	if m.chatScrollOffset <= 0 {
		return ""
	}
	return zeroTheme.faint.Render(fmt.Sprintf("↓ %d more · PgDn", m.chatScrollOffset))
}

type tuiRect struct {
	x      int
	y      int
	width  int
	height int
}

func (r tuiRect) contains(x int, y int) bool {
	return x >= r.x && y >= r.y && x < r.x+r.width && y < r.y+r.height
}

func (r tuiRect) local(x int, y int) (int, int, bool) {
	if !r.contains(x, y) {
		return 0, 0, false
	}
	return x - r.x, y - r.y, true
}

type transcriptFrameLayout struct {
	width           int
	height          int
	headerRect      tuiRect
	bodyRect        tuiRect
	footerRect      tuiRect
	composerRect    tuiRect
	statusRect      tuiRect
	headerLines     []string
	bodyHeight      int
	footerLines     []string
	fullFooterLines []string
	footerClip      int
}

func (m model) scrollableTranscriptFrame(header string, footer string) transcriptFrameLayout {
	headerLines := viewLines(header)
	fullFooterLines := viewLines(footer)
	footerLines := append([]string(nil), fullFooterLines...)

	maxFooterLines := maxInt(0, m.height-1)
	if len(footerLines) > maxFooterLines {
		footerLines = footerLines[len(footerLines)-maxFooterLines:]
	}
	if len(headerLines)+len(footerLines) >= m.height {
		maxHeaderLines := maxInt(0, m.height-len(footerLines)-1)
		if len(headerLines) > maxHeaderLines {
			headerLines = headerLines[:maxHeaderLines]
		}
	}
	if len(headerLines)+len(footerLines) >= m.height {
		maxFooterLines = maxInt(0, m.height-len(headerLines)-1)
		if len(footerLines) > maxFooterLines {
			footerLines = footerLines[len(footerLines)-maxFooterLines:]
		}
	}

	bodyHeight := m.height - len(headerLines) - len(footerLines)
	if bodyHeight < 1 {
		bodyHeight = 1
	}
	width := m.chatColumnWidth()
	footerTop := len(headerLines) + bodyHeight
	frame := transcriptFrameLayout{
		width:           width,
		height:          m.height,
		headerRect:      tuiRect{width: width, height: len(headerLines)},
		bodyRect:        tuiRect{y: len(headerLines), width: width, height: bodyHeight},
		footerRect:      tuiRect{y: footerTop, width: width, height: len(footerLines)},
		headerLines:     headerLines,
		bodyHeight:      bodyHeight,
		footerLines:     footerLines,
		fullFooterLines: fullFooterLines,
		footerClip:      maxInt(0, len(fullFooterLines)-len(footerLines)),
	}
	frame.composerRect = frame.footerSubrect(viewLines(m.composerBox(width)))
	if len(fullFooterLines) > 0 {
		frame.statusRect = frame.footerLineRect(len(fullFooterLines) - 1)
	}
	return frame
}

func (f transcriptFrameLayout) footerSubrect(sequence []string) tuiRect {
	if len(sequence) == 0 || len(f.footerLines) == 0 {
		return tuiRect{}
	}
	top := lineSequenceIndex(f.fullFooterLines, sequence)
	if top < 0 {
		return tuiRect{}
	}
	visibleTop := maxInt(top, f.footerClip)
	visibleBottom := minInt(top+len(sequence), f.footerClip+len(f.footerLines))
	if visibleTop >= visibleBottom {
		return tuiRect{}
	}
	return tuiRect{
		y:      f.footerRect.y + visibleTop - f.footerClip,
		width:  f.width,
		height: visibleBottom - visibleTop,
	}
}

func (f transcriptFrameLayout) footerLineRect(line int) tuiRect {
	if line < f.footerClip || line >= f.footerClip+len(f.footerLines) {
		return tuiRect{}
	}
	return tuiRect{
		y:      f.footerRect.y + line - f.footerClip,
		width:  f.width,
		height: 1,
	}
}

func (m model) scrollableTranscriptItemsView(header string, items []transcriptBodyItem, footer string, width int, overlay string) string {
	frame := m.scrollableTranscriptFrame(header, footer)
	metrics := measureTranscriptBodyItems(items, m.transcriptBodyHeights)
	window := transcriptViewportForLayout(metrics, frame, m.chatScrollOffset).window()
	body := layoutVisibleTranscriptBodyItems(items, metrics, window)

	return m.renderScrollableTranscriptWindow(frame, body.lines, window, width, overlay)
}

func (m model) renderScrollableTranscriptWindow(frame transcriptFrameLayout, bodyWindow []string, window transcriptViewportWindow, width int, overlay string) string {
	for len(bodyWindow) < window.height {
		bodyWindow = append(bodyWindow, "")
	}
	bodyWindow = overlayViewportLines(bodyWindow, overlay, width)

	lines := make([]string, 0, len(frame.headerLines)+len(bodyWindow)+len(frame.footerLines))
	lines = append(lines, frame.headerLines...)
	lines = append(lines, bodyWindow...)
	lines = append(lines, frame.footerLines...)
	for index, line := range lines {
		lines[index] = fitStyledLine(line, width)
	}
	return strings.Join(lines, "\n")
}

func overlayViewportLines(lines []string, overlay string, width int) []string {
	if strings.TrimSpace(overlay) == "" || len(lines) == 0 {
		return lines
	}
	overlayLines := viewLines(overlay)
	if len(overlayLines) == 0 {
		return lines
	}
	left, overlayLines, overlayWidth := normalizeOverlayBlock(overlayLines, width)
	if overlayWidth <= 0 {
		return lines
	}
	// Scrim: dim the whole transcript backdrop so a floating overlay (slash-command
	// palette, picker, wizard) clearly stands out instead of blending into the live
	// chat behind it. Header and composer are rendered separately and stay bright.
	for index := range lines {
		lines[index] = scrimViewportLine(lines[index], width)
	}
	start := maxInt(0, (len(lines)-len(overlayLines))/2)
	for offset, line := range overlayLines {
		target := start + offset
		if target >= len(lines) {
			break
		}
		lines[target] = overlayViewportLine(lines[target], line, left, overlayWidth, width)
	}
	return lines
}

// scrimViewportLine dims one backdrop line while keeping semantic styling intact.
// A transcript can contain syntax colors, diff backgrounds, warnings, and errors;
// reducing all of them to faint grey makes an overlay harder to understand instead
// of merely less prominent. Plain text uses the regular faint style. ANSI-styled
// text keeps its colors and is made faint between resets, which lets the overlay
// take focus while red, green, and syntax roles remain legible.
// Blank lines are left untouched.
func scrimViewportLine(line string, width int) string {
	if strings.TrimSpace(ansi.Strip(line)) == "" {
		return line
	}
	if !hasExternalANSIStyle(line) {
		return zeroTheme.faint.Render(line)
	}

	const faintSGR = "\x1b[2m"
	const resetSGR = "\x1b[0m"
	var out strings.Builder
	out.Grow(len(line) + 16)
	out.WriteString(faintSGR)
	for index := 0; index < len(line); {
		if line[index] == '\x1b' {
			if end := ansiSequenceEnd(line, index); end > index {
				sequence := line[index:end]
				out.WriteString(sequence)
				if sgrClearsFaint(sequence) {
					out.WriteString(faintSGR)
				}
				index = end
				continue
			}
		}
		out.WriteByte(line[index])
		index++
	}
	out.WriteString(resetSGR)
	return out.String()
}

// sgrClearsFaint reports whether an SGR sequence resets intensity. Lipgloss
// commonly emits ESC[0m around styled spans, but 22 also clears faint/bold, so
// both need the faint scrim reapplied for the next unstyled segment.
func sgrClearsFaint(sequence string) bool {
	if !strings.HasPrefix(sequence, "\x1b[") || !strings.HasSuffix(sequence, "m") {
		return false
	}
	params := strings.TrimSuffix(strings.TrimPrefix(sequence, "\x1b["), "m")
	if params == "" {
		return true
	}
	for _, param := range strings.Split(params, ";") {
		if param == "0" || param == "22" {
			return true
		}
	}
	return false
}

func normalizeOverlayBlock(lines []string, width int) (int, []string, int) {
	left := -1
	for _, line := range lines {
		if strings.TrimSpace(ansi.Strip(line)) == "" {
			continue
		}
		spaces := leadingPlainSpaces(line)
		if left < 0 || spaces < left {
			left = spaces
		}
	}
	if left < 0 {
		left = 0
	}
	left = minInt(left, maxInt(0, width-1))

	trimmed := make([]string, 0, len(lines))
	blockWidth := 0
	for _, line := range lines {
		if left > 0 && len(line) >= left {
			line = line[left:]
		}
		trimmed = append(trimmed, line)
		blockWidth = maxInt(blockWidth, lipgloss.Width(line))
	}
	blockWidth = minInt(blockWidth, maxInt(0, width-left))
	return left, trimmed, blockWidth
}

func leadingPlainSpaces(line string) int {
	spaces := 0
	for spaces < len(line) && line[spaces] == ' ' {
		spaces++
	}
	return spaces
}

func overlayViewportLine(base string, overlay string, left int, overlayWidth int, width int) string {
	if width <= 0 {
		return ""
	}
	left = clampInt(left, 0, width)
	overlayWidth = minInt(overlayWidth, width-left)
	rightStart := minInt(width, left+overlayWidth)

	base = fitStyledLine(base, width)
	prefix := padStyledLine(ansi.Cut(base, 0, left), left)
	panel := padStyledLine(overlay, overlayWidth)
	suffix := padStyledLine(ansi.Cut(base, rightStart, width), width-rightStart)
	return prefix + panel + suffix
}

func padStyledLine(line string, width int) string {
	if width <= 0 {
		return ""
	}
	line = fitStyledLine(line, width)
	if pad := width - lipgloss.Width(line); pad > 0 {
		line += strings.Repeat(" ", pad)
	}
	return line
}

func viewLines(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(value, "\n"), "\n")
}

func (m model) scrollChat(delta int) model {
	if !m.altScreen || delta == 0 {
		return m
	}
	viewport, ok := m.chatTranscriptViewport()
	if !ok {
		return m
	}
	m.chatScrollOffset = viewport.scroll(delta).offset
	if m.chatScrollOffset == 0 {
		m.chatBodyLines = 0
	}
	return m
}

func (m model) chatScrollMetrics() (int, int) {
	viewport, ok := m.chatTranscriptViewport()
	if !ok {
		return 0, 0
	}
	return viewport.totalLines, viewport.maxOffset()
}

func (m model) chatTranscriptViewport() (transcriptViewport, bool) {
	if !m.altScreen || m.height <= 0 {
		return transcriptViewport{}, false
	}
	width := m.chatColumnWidth()
	if m.transcriptDetailed {
		items := m.transcriptBodyItems(width, "", true)
		body := measureTranscriptBodyItems(items, m.transcriptBodyHeights)
		header := detailedTranscriptHeader(width) + "\n" + zeroTheme.line.Render(strings.Repeat("-", width))
		footer := m.detailedTranscriptFooter(width)
		frame := m.scrollableTranscriptFrame(header, footer)
		return transcriptViewportForLayout(body, frame, m.chatScrollOffset), true
	}
	items := m.transcriptBodyItems(width, "", false)
	body := measureTranscriptBodyItems(items, m.transcriptBodyHeights)
	frame := m.scrollableTranscriptFrame(m.pinnedTitleBar(width), m.footerView(width))
	return transcriptViewportForLayout(body, frame, m.chatScrollOffset), true
}

// syncChatScroll pins the viewport to what the user is reading. The scroll offset
// is measured from the bottom, so when the transcript grows (streaming) the window
// would otherwise follow the new bottom and drag the user off their spot. While
// the user has scrolled up, shift the offset by however many lines the body changed
// so the absolute view holds; at the bottom (offset 0) it follows normally. Only the
// scrolled-up path renders the body, so the common case stays cheap.
func (m model) syncChatScroll() model {
	if !m.altScreen || m.chatScrollOffset <= 0 {
		// At the bottom (or inline mode): follow the tail; reset the pin baseline.
		m.chatBodyLines = 0
		return m
	}
	current, maxOffset := m.chatScrollMetrics()
	m.chatScrollOffset = clampInt(m.chatScrollOffset, 0, maxOffset)
	if m.chatScrollOffset <= 0 {
		m.chatBodyLines = 0
		return m
	}
	if m.chatBodyLines == 0 {
		// Just scrolled up: establish the baseline, no adjustment this frame.
		m.chatBodyLines = current
		return m
	}
	// Shift by the signed delta so the absolute view holds whether the body grew
	// (streaming appended lines) or shrank (a tool card collapsed, transcript
	// cleared). Clamp at zero so a large shrink lands the user back at the tail
	// rather than underflowing past it.
	m.chatScrollOffset = clampInt(m.chatScrollOffset+current-m.chatBodyLines, 0, maxOffset)
	m.chatBodyLines = current
	return m
}

func (m model) chatPageScrollLines() int {
	if m.height <= 0 {
		return 10
	}
	return maxInt(3, m.height-8)
}

// interimBlock renders the live assistant text while a turn streams. It uses
// the same lightweight markdown renderer as completed assistant rows, so
// tables and simple formatting stabilize as soon as enough tokens arrive.
// Before the first delta arrives it falls back to the spinner so the surface
// still shows liveness. The cursor needs no ticker — it appears exactly while
// pending.
// liveReasoningBodyCap caps an EXPANDED live ("Thinking…") reasoning block to
// roughly half the screen so it doesn't fill the terminal and its clickable
// toggle header stays on-screen. Returns 0 (no cap) when the height is unknown.
func (m model) liveReasoningBodyCap() int {
	if m.height <= 0 {
		return 0
	}
	return maxInt(6, m.height/2)
}

func (m model) interimBlock(width int) string {
	text := strings.TrimRight(m.streamingTextString(), "\n")
	reasoning := strings.TrimRight(m.streamingReasoning, "\n")
	blocks := []string{}
	if strings.TrimSpace(reasoning) != "" {
		blocks = append(blocks, renderReasoningBlock(reasoning, m.streamingReasoningExpanded, width, true, 0, m.liveReasoningBodyCap()))
	}
	if strings.TrimSpace(text) == "" {
		if writing := m.streamingToolCallView(width); writing != "" {
			blocks = append(blocks, writing)
		}
		blocks = append(blocks, m.workingStatusLine())
		// During a long think the reasoning block is collapsed to just its header;
		// show a live tail of the streaming reasoning beneath the working line so
		// the screen keeps changing (never looks stuck) and the user can see WHAT
		// the model is reasoning about. Skipped when expanded (the full body shows).
		if reasoning != "" && !m.streamingReasoningExpanded {
			blocks = append(blocks, reasoningPreviewLines(reasoning, width)...)
		}
		return strings.Join(blocks, "\n")
	}
	// Live streaming block: prose streams normally, but an open fenced code block
	// is buffered until its closing fence arrives so the code appears as one
	// highlighted block instead of recoloring token-by-token.
	lines := renderStreamingAssistantMarkdownText(text, assistantMeasure(width), width)
	for index, line := range lines {
		// styleStreamingLine fades plain prose but leaves already-highlighted
		// markdown/code lines alone, so live colors match the committed row.
		lines[index] = m.styleStreamingLine(line, index, len(lines))
	}
	lines = m.appendStreamingCursor(lines, width)
	blocks = append(blocks, strings.Join(lines, "\n"))
	// Live preview of a file currently being written, so a long write_file/edit
	// shows the code streaming in rather than looking frozen.
	if writing := m.streamingToolCallView(width); writing != "" {
		blocks = append(blocks, writing)
	}
	// Always show the live working line (motion cue + verb + elapsed) BELOW the
	// streamed text so an upstream stall keeps animating, never a frozen screen.
	blocks = append(blocks, m.workingStatusLine())
	return strings.Join(blocks, "\n")
}

// workingStatusLine renders the live "working" indicator shown on every pending
// render: a subtle liveness pulse, the current phase, and the elapsed time.
// It is shown even once partial text has streamed so an upstream stall never
// looks like a frozen terminal — the spinner tick (~33ms, time-based) drives the
// re-render, so the elapsed clock keeps advancing for ANY provider/model even
// when no stream data arrives.
// spinnerGlyph is the liveness glyph every renderer should use instead of
// m.spinner.View() directly: the animated frame normally, or a steady dot under
// reduced motion. The caller applies its own color; liveness is preserved by the
// advancing elapsed timer, so the static glyph never reads as frozen.
func (m model) spinnerGlyph() string {
	if m.reducedMotion {
		return "•"
	}
	return m.spinner.View()
}

// workingActivity labels the current live phase for the working status line.
// User-blocked states take precedence, then a streamed or outstanding tool call,
// then assistant text/reasoning. This makes a quiet but healthy run legible
// without guessing from elapsed time alone.
func (m model) workingActivity() string {
	if m.pendingPermission != nil {
		return "waiting for approval"
	}
	if m.pendingAskUser != nil {
		return "waiting for your answer"
	}
	if m.streamCallName != "" {
		return strings.ToLower(toolCardActionLabel(m.streamCallName, "", true))
	}
	if row, ok := m.activeToolCall(); ok {
		return strings.ToLower(toolCardActionLabel(toolRowName(row), row.detail, true))
	}
	if strings.TrimSpace(m.streamingTextString()) != "" {
		return "writing"
	}
	return "thinking"
}

// activeToolCallScanLimit bounds per-frame work while the spinner is active.
// A current tool call belongs near the transcript tail; if it falls outside this
// window we use the conservative "thinking" label rather than scan history on
// every animation frame.
const activeToolCallScanLimit = 200

// activeToolCall returns the newest unresolved tool call from the active run.
// Results are encountered before their calls while scanning backwards, so a
// small resolved set prevents an earlier completed call from being reported as
// live. Tool IDs are required: unkeyed historical rows cannot be paired safely.
func (m model) activeToolCall() (transcriptRow, bool) {
	if !m.pending || m.activeRunID == 0 {
		return transcriptRow{}, false
	}
	var resolved map[string]struct{}
	for i, scanned := len(m.transcript)-1, 0; i >= 0 && scanned < activeToolCallScanLimit; i, scanned = i-1, scanned+1 {
		row := m.transcript[i]
		if row.runID != m.activeRunID || row.id == "" {
			continue
		}
		key := rcKey(row.runID, row.id)
		switch row.kind {
		case rowToolResult:
			if resolved == nil {
				resolved = make(map[string]struct{})
			}
			resolved[key] = struct{}{}
		case rowToolCall:
			if _, complete := resolved[key]; !complete {
				return row, true
			}
		}
	}
	return transcriptRow{}, false
}

func toolResultCardSuppressedInTranscript(name string, status tools.Status) bool {
	return isHiddenPlumbingTool(name) && status != tools.StatusError
}

func (m model) workingStatusLine() string {
	// Keep one quiet liveness signal at the start of the line. Tool and plan
	// labels stay still, so the display reads as active without competing motion
	// scattered through the transcript.
	line := m.workingStatusLabel()
	if indicator := m.workingStatusIndicator(); indicator != "" {
		line = indicator + line
	}
	// Phase label so a long, output-less step reads as live progress rather than a
	// frozen screen: "writing" while the answer streams, "thinking" otherwise
	// (reasoning, waiting on the model, or running a tool).
	line += zeroTheme.faint.Render("  ·  " + m.workingActivity())
	if !m.turnStartedAt.IsZero() {
		line += zeroTheme.faint.Render("  ·  " + formatWorkingElapsed(m.activeTurnElapsed(m.turnStartedAt)))
	}
	// Live token estimate so the working line visibly climbs as the model reasons
	// and writes, instead of a static figure. Shown from the start of the turn (at
	// 0) so the counter is never missing — the authoritative totals stay in the
	// status line and sidebar; this is the at-a-glance "it's generating" pulse.
	line += zeroTheme.faint.Render("  ·  " + m.workingTokenIndicator())
	// If the model has gone quiet (no streamed text, reasoning, OR tool-call output
	// for a while — common when a provider buffers a large tool call instead of
	// streaming it), say so plainly with an advancing timer, so a long silent
	// generation never reads as a frozen screen.
	if hint := m.quietGenerationHint(); hint != "" {
		line += zeroTheme.amber.Render("  ·  " + hint)
	}
	return line
}

// workingTokenIndicator renders a live "↑ <n> tok" estimate of the tokens
// generated so far in the current turn, so the working line keeps moving while
// the model reasons and writes. It is shown for the whole turn — starting at
// "↑ 0 tok" before the first delta and climbing — so the counter never blinks
// out. Providers only report exact usage when a step finishes, so this estimates
// from the streamed reasoning+answer length at the usual ~4 characters per
// token; turnStreamedRunes accumulates across the whole turn (it survives the
// per-segment buffer clears), giving a monotonic climb that resets on the next
// turn.
func (m model) workingTokenIndicator() string {
	tokens := m.turnStreamedRunes / 4
	if m.turnStreamedRunes > 0 && tokens < 1 {
		tokens = 1
	}
	return "↑ " + humanCount(tokens) + " tok"
}

// quietWorkingHint is how long the stream must be silent (no streamed text,
// reasoning, or tool-call output) during an active turn before the working line
// calls out that it's still generating — so a provider that buffers a big tool
// call (instead of streaming the file as it's written) doesn't read as stuck.
const quietWorkingHint = 8 * time.Second

// quietGenerationHint returns a "still generating…" cue with an advancing
// quiet-timer when the active turn has produced no streamed output for a while,
// else "". The advancing number is itself the liveness signal.
//
// Past half the provider's idle timeout, the cue escalates to name what's
// actually happening and when Zero will act on its own: a heartbeating-but-
// silent stream (observed on chatgpt/gpt-5.x and ollama reasoning models,
// see providerio.ErrStreamStalled) is bounded by the content-stall watchdog at
// providerio.ContentStallTimeout(idle), but until it fires this exact same
// plain "still generating… Xs" text is indistinguishable from a genuine hang —
// the ticking number was the only signal, and it looks identical whether real
// (if slow) content is coming or nothing ever will.
func (m model) quietGenerationHint() string {
	if m.activeRunID == 0 || m.pendingPermission != nil {
		return ""
	}
	last := m.lastStreamActivity
	if last.IsZero() {
		last = m.turnStartedAt
	}
	if last.IsZero() {
		return ""
	}
	quiet := m.now().Sub(last)
	if quiet < quietWorkingHint {
		return ""
	}
	if idleTimeout := providerio.ResolveStreamIdleTimeout(0); idleTimeout > 0 && quiet >= idleTimeout/2 {
		ceiling := providerio.ContentStallTimeout(idleTimeout)
		return fmt.Sprintf("still generating… %s — unusually quiet, Zero will auto-recover by ~%s if it doesn't resume", formatWorkingElapsed(quiet), formatWorkingElapsed(ceiling))
	}
	return "still generating… " + formatWorkingElapsed(quiet)
}

// formatWorkingElapsed renders a turn's running time compactly: "8s", "1m04s".
func formatWorkingElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d/time.Minute), int(d.Seconds())%60)
}

// reasoningPreviewLines renders the last 1-2 lines of the in-flight reasoning
// stream as a dimmed preview so a long "Thinking" phase shows live, changing
// content instead of a static header. Each line shows its TAIL (the most recent
// text) so a single continuously-growing reasoning line still visibly moves as
// tokens arrive. Returns nil when there is no reasoning text.
func reasoningPreviewLines(reasoning string, width int) []string {
	var lines []string
	for _, raw := range strings.Split(strings.TrimSpace(reasoning), "\n") {
		if t := strings.TrimSpace(raw); t != "" {
			lines = append(lines, t)
		}
	}
	if len(lines) == 0 {
		return nil
	}
	if len(lines) > 2 {
		lines = lines[len(lines)-2:]
	}
	avail := width - 2
	if avail < 8 {
		avail = 8
	}
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, "  "+zeroTheme.faint.Render(previewTail(line, avail)))
	}
	return out
}

// previewTail returns the last `width` runes of s, prefixed with "…" when text
// was dropped, so a streaming preview shows the newest content. s is plain text
// (reasoning deltas carry no ANSI), so rune counting is a safe width proxy.
func previewTail(s string, width int) string {
	runes := []rune(s)
	if width <= 0 || len(runes) <= width {
		return s
	}
	if width == 1 {
		return string(runes[len(runes)-1:])
	}
	return "…" + string(runes[len(runes)-(width-1):])
}

func (m model) appendStreamingCursor(lines []string, width int) []string {
	// Pulse the caret on the shared spinner clock so the typing edge reads as alive
	// even during fade-tick gaps or upstream stalls. Width-stable (bright ↔ dim,
	// never on/off, so the line never jitters). Steady bright under reduced motion.
	cursor := zeroTheme.accent.Render("▌")
	if !m.reducedMotion && (m.spinnerPhase/6)%2 == 1 {
		cursor = zeroTheme.faint.Render("▌")
	}
	if len(lines) == 0 {
		return []string{cursor}
	}
	last := len(lines) - 1
	if width > 0 && lipgloss.Width(lines[last])+1 > width {
		return append(lines, cursor)
	}
	lines[last] += cursor
	return lines
}

// composerLine renders the borderless composer.
func (m model) composerLine(width int) string {
	input := m.input
	if m.hasQueuedMessage() {
		input.Placeholder = queuedEditHint
	}
	hideInputForSuggestions := m.suggestionsActive() && (!m.suggestionsAreFiles || fileSuggestionOnlyInput(m.input.Value()))
	if hideInputForSuggestions {
		input.SetValue("")
		input.Placeholder = ""
		input.CursorEnd()
	}
	state := composerState{text: input.Value(), cursor: input.Position()}
	if m.composerActive {
		state = m.composer
	}
	if hideInputForSuggestions {
		state = composerState{}
	}
	argumentHint := commandArgumentHintForInput(input.Value())
	if argumentHint != "" && input.Position() != len([]rune(input.Value())) {
		argumentHint = ""
	}
	if argumentHint != "" {
		input.SetWidth(0)
		return fitStyledLine(commandArgumentHintComposerLine(input, argumentHint, m.composerCursorVisible), width)
	}
	previews := validComposerPastePreviews(state, m.composerPastePreviews)
	displayState := composerDisplayStateForPastePreviews(state, previews)
	displaySelection := composerSelectionState{}
	if start, end, ok := m.composerSelection.rangeFor(state); ok {
		displaySelection = composerSelectionState{
			active: true,
			anchor: composerDisplayCursorForPastePreviews(start, previews),
			cursor: composerDisplayCursorForPastePreviews(end, previews),
		}
	}
	return renderComposerInput(input, displayState, width, m.composerCursorVisible, displaySelection)
}

type composerVisualLine struct {
	first bool
	start int
	end   int
}

func renderComposerInput(input textinput.Model, state composerState, width int, cursorVisible bool, selection composerSelectionState) string {
	state = normalizeComposerState(state)
	if width <= 0 {
		return ""
	}
	if state.text == "" {
		// Empty box: show a (blinking) cursor before the placeholder so the focused
		// input always has a visible caret. A plain space when blinked off keeps the
		// placeholder column stable.
		cursor := " "
		if cursorVisible {
			cursor = composerCursor(" ")
		}
		return fitStyledLine(composerVisualLinePrefix(input, true)+cursor+zeroTheme.faint.Render(input.Placeholder), width)
	}

	segments, cursorLine := composerVisibleVisualLines(input, state, width)
	lines := make([]string, 0, len(segments))
	for index, segment := range segments {
		lines = append(lines, fitStyledLine(renderComposerVisualLine(input, state, segment, index == cursorLine, cursorVisible, selection), width))
	}
	return strings.Join(lines, "\n")
}

func composerVisibleVisualLines(input textinput.Model, state composerState, width int) ([]composerVisualLine, int) {
	segments := composerWrappedVisualLines(input, state, width)
	cursorLine := composerCursorVisualLine(segments, state.cursor)
	if len(segments) <= composerMaxVisibleLines {
		return segments, cursorLine
	}
	start := clamp(cursorLine-composerMaxVisibleLines+1, 0, len(segments)-composerMaxVisibleLines)
	end := start + composerMaxVisibleLines
	cursorLine -= start
	segments = segments[start:end]
	if len(segments) > 0 {
		segments[0].first = true
	}
	return segments, cursorLine
}

func composerWrappedVisualLines(input textinput.Model, state composerState, width int) []composerVisualLine {
	runes := []rune(state.text)
	segments := []composerVisualLine{}
	first := true
	start := 0
	for index, r := range runes {
		if r != '\n' {
			continue
		}
		segments = appendComposerWrappedVisualLines(segments, input, runes, start, index, width, &first)
		start = index + 1
	}
	segments = appendComposerWrappedVisualLines(segments, input, runes, start, len(runes), width, &first)
	return segments
}

func appendComposerWrappedVisualLines(segments []composerVisualLine, input textinput.Model, runes []rune, start int, end int, width int, first *bool) []composerVisualLine {
	if start >= end {
		segments = append(segments, composerVisualLine{first: *first, start: start, end: end})
		*first = false
		return segments
	}
	for start < end {
		lineFirst := *first
		measure := maxInt(1, width-lipgloss.Width(composerVisualLinePrefix(input, lineFirst)))
		split := start
		used := 0
		for split < end {
			nextWidth := lipgloss.Width(string(runes[split]))
			if used+nextWidth > measure {
				break
			}
			used += nextWidth
			split++
		}
		if split == start {
			split++
		}
		segments = append(segments, composerVisualLine{first: lineFirst, start: start, end: split})
		*first = false
		start = split
	}
	return segments
}

func composerCursorVisualLine(segments []composerVisualLine, cursor int) int {
	if len(segments) == 0 {
		return 0
	}
	for index, segment := range segments {
		if cursor < segment.start || cursor > segment.end {
			continue
		}
		if cursor == segment.end && index+1 < len(segments) && segments[index+1].start == cursor {
			continue
		}
		return index
	}
	return len(segments) - 1
}

func renderComposerVisualLine(input textinput.Model, state composerState, segment composerVisualLine, hasCursor bool, cursorVisible bool, selection composerSelectionState) string {
	runes := []rune(state.text)
	prefix := composerVisualLinePrefix(input, segment.first)
	textStyle := zeroTheme.ink.Inline(true)
	selectionStart, selectionEnd, hasSelection := selection.rangeFor(state)
	cursorIndex := -1
	if hasCursor && !hasSelection {
		cursorIndex = clamp(state.cursor, segment.start, segment.end)
	}

	var line strings.Builder
	line.WriteString(prefix)
	for index := segment.start; index < segment.end; index++ {
		cell := string(runes[index])
		switch {
		case index == cursorIndex && cursorVisible:
			line.WriteString(composerCursor(cell))
		case hasSelection && index >= selectionStart && index < selectionEnd:
			line.WriteString(zeroTheme.selection.Render(cell))
		default:
			line.WriteString(textStyle.Render(cell))
		}
	}
	if cursorIndex == segment.end && cursorVisible {
		line.WriteString(composerCursor(" "))
	}
	return line.String()
}

func composerVisualLinePrefix(input textinput.Model, first bool) string {
	if first {
		return zeroTheme.userPrompt.Render(input.Prompt)
	}
	return "  "
}

func composerDisplayStateForPastePreviews(state composerState, previews []composerPastePreview) composerState {
	state = normalizeComposerState(state)
	valid := validComposerPastePreviews(state, previews)
	if len(valid) == 0 {
		return state
	}
	runes := []rune(state.text)
	display := make([]rune, 0, len(runes))
	last := 0
	for _, preview := range valid {
		display = append(display, runes[last:preview.start]...)
		display = append(display, []rune(preview.label)...)
		last = preview.end
	}
	display = append(display, runes[last:]...)
	return composerState{
		text:   string(display),
		cursor: composerDisplayCursorForPastePreviews(state.cursor, valid),
	}
}

func composerDisplayCursorForPastePreviews(cursor int, previews []composerPastePreview) int {
	delta := 0
	for _, preview := range previews {
		labelLen := len([]rune(preview.label))
		hiddenLen := preview.end - preview.start
		displayStart := preview.start + delta
		switch {
		case cursor <= preview.start:
			return cursor + delta
		case cursor <= preview.end:
			return displayStart + labelLen
		default:
			delta += labelLen - hiddenLen
		}
	}
	return cursor + delta
}

func (m model) moveComposerVisualCursor(direction int) (model, bool) {
	if direction == 0 {
		return m, false
	}
	width := chatWidth(m.width)
	if width < 8 {
		return m, false
	}
	input := m.input
	state := m.currentComposerState()
	state = normalizeComposerState(state)
	if state.text == "" {
		return m, false
	}
	previews := validComposerPastePreviews(state, m.composerPastePreviews)
	displayState := composerDisplayStateForPastePreviews(state, previews)
	segments := composerWrappedVisualLines(input, displayState, maxInt(1, width-4))
	if len(segments) <= 1 {
		return m, false
	}
	cursorLine := composerCursorVisualLine(segments, displayState.cursor)
	targetLine := clamp(cursorLine+direction, 0, len(segments)-1)
	if targetLine == cursorLine {
		return m, true
	}
	column := composerVisualCursorColumn(displayState, segments[cursorLine])
	displayState.cursor = composerCursorForVisualColumn(displayState, segments[targetLine], column)
	state.cursor = composerOriginalCursorForPastePreviews(displayState.cursor, previews)
	m.setComposerState(state)
	return m, true
}

func composerOriginalCursorForPastePreviews(displayCursor int, previews []composerPastePreview) int {
	if len(previews) == 0 {
		return displayCursor
	}
	delta := 0
	for _, preview := range previews {
		labelLen := len([]rune(preview.label))
		hiddenLen := preview.end - preview.start
		displayStart := preview.start + delta
		displayEnd := displayStart + labelLen
		switch {
		case displayCursor <= displayStart:
			return displayCursor - delta
		case displayCursor <= displayEnd:
			return preview.end
		default:
			delta += labelLen - hiddenLen
		}
	}
	return displayCursor - delta
}

func composerVisualCursorColumn(state composerState, segment composerVisualLine) int {
	state = normalizeComposerState(state)
	runes := []rune(state.text)
	cursor := clamp(state.cursor, segment.start, segment.end)
	column := 0
	for index := segment.start; index < cursor && index < len(runes); index++ {
		column += lipgloss.Width(string(runes[index]))
	}
	return column
}

func composerCursorForVisualColumn(state composerState, segment composerVisualLine, column int) int {
	state = normalizeComposerState(state)
	runes := []rune(state.text)
	used := 0
	for index := segment.start; index < segment.end && index < len(runes); index++ {
		width := lipgloss.Width(string(runes[index]))
		if used+width > column {
			return index
		}
		used += width
	}
	return segment.end
}

func commandArgumentHintComposerLine(input textinput.Model, argumentHint string, cursorVisible bool) string {
	hintRunes := []rune(argumentHint)
	if len(hintRunes) == 0 {
		return input.View()
	}
	displayValue := strings.TrimRightFunc(input.Value(), unicode.IsSpace)
	// This alternate composer path must follow the same caret contract as
	// renderComposerInput: hidden while the terminal is unfocused and blinking
	// per composerCursorVisible, not a permanently painted cursor cell.
	cursor := zeroTheme.faint.Render(string(hintRunes[0]))
	if cursorVisible {
		cursor = composerCursor(cursor)
	}
	return zeroTheme.userPrompt.Render(input.Prompt) +
		zeroTheme.ink.Inline(true).Render(displayValue) +
		zeroTheme.faint.Render(" ") +
		cursor +
		zeroTheme.faint.Render(string(hintRunes[1:]))
}

func composerCursor(char string) string {
	return zeroTheme.selection.Render(char)
}

func commandArgumentHintForInput(value string) string {
	command := parseCommand(value)
	if command.name == "" || strings.TrimSpace(command.text) != "" {
		return ""
	}
	return commandRequiredInputHint(command.name)
}

func (m model) composerBox(width int) string {
	if width < 8 {
		return fitStyledLine(m.composerLine(width), width)
	}
	reserved := m.petComposerReservedColumns(width)
	boxWidth := width - reserved
	innerWidth := maxInt(1, boxWidth-4)
	content := m.composerLine(innerWidth)
	lines := strings.Split(content, "\n")
	rightPad := strings.Repeat(" ", reserved)

	rendered := make([]string, 0, len(lines)+3)
	rendered = append(rendered, zeroTheme.lineStrong.Render("╭"+strings.Repeat("─", boxWidth-2)+"╮")+rightPad)
	// On graphics-capable terminals the first image receives a real thumbnail in
	// this compact strip. Text-only terminals retain the numbered chip row below.
	if m.attachmentThumbnailVisible(width) {
		for _, line := range m.attachmentThumbnailLines(innerWidth) {
			fitted := fitStyledLine(line, innerWidth)
			pad := strings.Repeat(" ", maxInt(0, innerWidth-lipgloss.Width(fitted)))
			rendered = append(rendered, zeroTheme.lineStrong.Render("│ ")+fitted+pad+zeroTheme.lineStrong.Render(" │")+rightPad)
		}
		// A thumbnail gallery makes the first few attachments visible. Keep a compact
		// numbered row whenever there is more than one item (or a document), so the
		// rest of a longer batch is never silently hidden.
		if chips := m.attachmentThumbnailSupplementalChips(); chips != "" {
			fitted := fitStyledLine(zeroTheme.muted.Render(chips), innerWidth)
			pad := strings.Repeat(" ", maxInt(0, innerWidth-lipgloss.Width(fitted)))
			rendered = append(rendered, zeroTheme.lineStrong.Render("│ ")+fitted+pad+zeroTheme.lineStrong.Render(" │")+rightPad)
		}
	} else if chips := renderAttachmentChips(m.pendingImageLabels, m.pendingDocuments); chips != "" {
		fitted := fitStyledLine(zeroTheme.muted.Render(chips), innerWidth)
		pad := strings.Repeat(" ", maxInt(0, innerWidth-lipgloss.Width(fitted)))
		rendered = append(rendered, zeroTheme.lineStrong.Render("│ ")+fitted+pad+zeroTheme.lineStrong.Render(" │")+rightPad)
	}
	for _, line := range lines {
		fitted := fitStyledLine(line, innerWidth)
		pad := strings.Repeat(" ", maxInt(0, innerWidth-lipgloss.Width(fitted)))
		rendered = append(rendered, zeroTheme.lineStrong.Render("│ ")+fitted+pad+zeroTheme.lineStrong.Render(" │")+rightPad)
	}
	rendered = append(rendered, m.composerDividerLine(width))
	return strings.Join(rendered, "\n")
}

// startsTurn reports whether a row begins a new conversational turn and therefore
// gets a blank line of separation above it (tool rows stay grouped together).
func startsTurn(kind rowKind) bool {
	switch kind {
	case rowUser, rowAssistant, rowSystem, rowError:
		return true
	default:
		return false
	}
}

// isToolCardKind reports whether a row renders as a tool card (a running call or
// its collapsed result). Used to add one blank line between consecutive tool
// cards in a turn. Specialist cards are excluded — they own their own grouping
// (summary line + injected spacing) and must not be double-spaced.
func isToolCardKind(kind rowKind) bool {
	return kind == rowToolCall || kind == rowToolResult
}

func needsSeparatorBeforeToolCard(previous rowKind, current rowKind) bool {
	if !isToolCardKind(current) {
		return false
	}
	return isToolCardKind(previous) || previous == rowAssistant || previous == rowUser
}

func shouldRuleBeforeTurn(previous rowKind, current rowKind) bool {
	return current == rowAssistant && isToolCardKind(previous)
}

func (m model) handlePermissionKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.pendingPermission == nil {
		return m, nil
	}
	key := strings.ToLower(msg.String())
	for _, option := range permissionOptions(m.pendingPermission.request) {
		if option.hotkey == key {
			return m.choosePermissionOption(option.choice)
		}
	}
	return m, nil
}

func (m model) resolvePermission(decision permissionDecision) (tea.Model, tea.Cmd) {
	return m.resolvePermissionWithReason(decision, permissionDecisionReason(decision))
}

// resolvePermissionWithReason resolves the pending prompt with an explicit reason
// string. It backs both the fixed-label choices (reason = permissionDecisionReason)
// and the free-text "tell Zero what to do differently" path, where the reason is
// the user's typed instruction and the action is Deny so the agent surfaces it as
// the tool result and keeps going.
func (m model) resolvePermissionWithReason(decision permissionDecision, reason string) (tea.Model, tea.Cmd) {
	pending := m.pendingPermission
	if pending == nil {
		return m, nil
	}

	resolved := agent.PermissionDecision{Action: decision, Reason: reason}
	if pending.decide != nil {
		pending.decide(resolved)
	}
	var decideCmd tea.Cmd
	if pending.decideCmd != nil {
		decideCmd = pending.decideCmd(resolved)
	}
	m.pendingPermission = nil
	// Time spent at the prompt is user wait, not provider silence. Restart the
	// quiet-generation clock so resuming a long-blocked run does not immediately
	// claim the model has been inactive for the entire approval interval.
	m.lastStreamActivity = m.now()
	if pending.request.ToolName == peerPermissionToolName {
		// Receipt delivery completes asynchronously. That completion advances
		// the peer queue after this prompt is fully settled.
		return m, decideCmd
	}
	next, cmd := m.openNextPeerApproval()
	return next, tea.Batch(decideCmd, cmd)
}

func permissionDecisionReason(decision permissionDecision) string {
	switch decision {
	case permissionDecisionAllow:
		return "approved in TUI"
	case permissionDecisionAllowStrict:
		return "approved with model review request in TUI"
	case permissionDecisionAllowForSession:
		return "approved for this session in TUI"
	case permissionDecisionAllowPrefix:
		return "approved command prefix for this session in TUI"
	case permissionDecisionAlwaysAllowPrefix:
		return "persistently approved command prefix in TUI"
	case permissionDecisionAlwaysAllow:
		return "persistently approved in TUI"
	case permissionDecisionCancel:
		return "cancelled in TUI"
	case permissionDecisionDeny:
		return "denied in TUI"
	default:
		return "denied in TUI"
	}
}

// choosePicker applies the highlighted picker item through the same handler the
// typed command would have used, appends the resulting status text, and closes
// the picker. Behavior is identical to running "/model <id>" or "/effort <v>".
func (m model) choosePicker() (tea.Model, tea.Cmd) {
	if m.modelPickerIsLoading() {
		return m, nil
	}
	picker := m.picker
	if picker != nil && picker.kind == pickerModel {
		m.clearModelPickerLoadState()
	}
	m.picker = nil
	if picker == nil {
		return m, nil
	}
	item, ok := picker.current()
	if !ok {
		return m, nil
	}
	var cmd tea.Cmd
	switch picker.kind {
	case pickerModel:
		previousProvider, previousModel := m.providerName, m.modelName
		text := ""
		var switchPersistErr error
		// The row's owner label is derived from the same decision, so it always
		// names the provider used here.
		if owner := m.modelPickerSwitchOwner(item); owner != "" {
			// A model from another saved provider row: switch provider + model together.
			m, text, _, cmd, switchPersistErr = m.switchProviderModel(owner, item.Value)
		} else {
			// Apply against the active provider instead of attempting an
			// unresolvable or self-directed provider switch.
			m, text, switchPersistErr = m.handleModelCommand(item.Value)
		}
		if m.providerName != previousProvider || m.modelName != previousModel {
			notice, tone := m.modelAppliedNoticeFor(switchPersistErr)
			return m.showTransientNoticeInline(notice, tone), cmd
		}
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: text})
	case pickerEffort:
		previous := m.reasoningEffort
		text := ""
		m, text = m.handleEffortCommand(item.Value)
		if m.reasoningEffort != previous {
			return m.showTransientNoticeInline(m.effortAppliedNotice(), transientNoticeSuccess), cmd
		}
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: text})
	case pickerSession:
		// item.Value is the chosen session id; handleResumeCommand hydrates it and
		// rebuilds the transcript (returning "" on success, an error note on failure).
		text := ""
		m, text = m.handleResumeCommand(item.Value)
		if text != "" {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: text})
		}
	case pickerSkill:
		// Fill the composer with "/name " so the user adds their request before
		// submitting (a bare second Enter runs it without one); names the slash
		// path cannot reach run immediately instead.
		m, cmd = m.chooseSkillFromPicker(item)
	case pickerSTTModel:
		// Selecting the local engine with no model (and auto-download available)
		// chains into the variant-download picker instead of finalizing.
		if next, fetchCmd, opened := m.maybeOpenSTTDownloadPicker(item.Value); opened {
			return next, fetchCmd
		}
		text := ""
		m, text = m.handleSTTModelSelection(item.Value)
		if text != "" { // empty when a key prompt opened instead of finalizing
			m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: text})
		}
	case pickerSTTDownload:
		return m.handleSTTDownloadSelection(item.Value)
	case pickerPet:
		return m.installPet(item.Value)
	case pickerTheme:
		// Selection is applied only after Enter. Moving through the picker renders a
		// local preview and never changes the active palette.
		text := ""
		m, text = m.handleThemeCommand(item.Value)
		if validThemeMode(item.Value) && !strings.Contains(text, "could not save theme preference") {
			return m.showTransientNotice(m.themeAppliedNotice(), transientNoticeSuccess)
		}
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: text})
	case pickerNotify:
		// The picker item's Value is "<mode> <focus>"; reusing the text handler
		// keeps validation, persistence, and the user-facing message in one
		// place. There is no live preview for notify, so no follow-up command
		// is needed here.
		text := ""
		m, text = m.handleNotifyCommand(item.Value)
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: text})
	}
	return m, cmd
}

func (m model) chooseSuggestion() (tea.Model, tea.Cmd) {
	if !m.suggestionsActive() || len(m.suggestions) == 0 {
		return m, nil
	}
	wasFiles := m.suggestionsAreFiles
	wasDirectory := m.selectedSuggestionIsDirectory()
	requiresInput := m.selectedCommandSuggestionRequiresInput()
	next := m.completeSuggestion()
	if !wasFiles {
		next.resetComposerFromInput()
	}
	if wasFiles && wasDirectory {
		next.recomputeSuggestions()
		return next, nil
	}
	if !wasFiles {
		if requiresInput {
			return next, nil
		}
		return next.handleSubmit()
	}
	return next, nil
}

func (m model) handleSubmit() (tea.Model, tea.Cmd) {
	input := m.composerValue()
	// A drag-dropped image/PDF path that reached the composer (e.g. inserted as
	// text) attaches instead of being parsed as an unknown "/…" command.
	if path, ok := droppedAttachmentPath(input, m.cwd); ok {
		m = m.handleImageCommand(path)
		m.clearComposer()
		m.clearSuggestions()
		return m, nil
	}
	command := parseCommand(input)
	// A pending /clear or /quit leave-confirmation is armed for the immediately-next
	// repeat of that same command only; any other submission — including one queued
	// or deferred by the early returns below — disarms it. Runs before those returns
	// so an interposed prompt can't be skipped over and leave the gate falsely armed.
	if m.loopLeavePrompt != commandEmpty && command.kind != m.loopLeavePrompt {
		m.loopLeavePrompt = commandEmpty
	}
	// While exiting (Ctrl+C waiting on the cancelled run's checkpoint flush) a
	// new run must not start: the deferred tea.Quit would abort it mid-flight
	// and orphan its checkpoint blobs — the exact loss flushRunIDs prevents.
	if command.kind == commandPrompt && m.exiting {
		return m, nil
	}
	if command.kind == commandPrompt && m.pending {
		return m.queueMessage(command.text), nil
	}
	if command.kind == commandPrompt && m.compactInFlight {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{
			kind: actionAppendSystem,
			text: "Compact\nstatus: warning\nCompaction is running. Your next prompt will use the compacted context when this finishes.",
		})
		return m, nil
	}
	m.rememberInput(input)
	m.clearComposer()
	m.clearSuggestions()
	// Snap the viewport back to the bottom for a real submission, but not for an
	// empty Enter (a no-op) — that would yank the user away from wherever they
	// had scrolled without anything actually being submitted.
	if command.kind != commandEmpty {
		m.chatScrollOffset = 0
	}

	return m.dispatchCommand(command)
}

// dispatchCommand runs a parsed slash/prompt command after submit/leader
// preamble (history, composer clear, leave-prompt disarm) has already run.
func (m model) dispatchCommand(command parsedCommand) (tea.Model, tea.Cmd) {
	if m.btw.active && command.kind == commandExit && m.btw.parent != nil &&
		(m.btw.parent.pending || m.btw.parent.compactInFlight || len(m.btw.parent.flushRunIDs) > 0) {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{
			kind: actionAppendSystem,
			text: "The main session is still running. Return to it first with /btw or Ctrl+C before exiting.",
		})
		return m, nil
	}
	if m.btw.active && btwCommandUnavailable(command) {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{
			kind: actionAppendSystem,
			text: command.name + " is unavailable in a BTW conversation. Return to the main session first with /btw or Ctrl+C.",
		})
		return m, nil
	}
	if m.permissionMode == agent.PermissionModePlan && planModeCommandUnavailable(command) {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{
			kind: actionAppendSystem,
			text: command.name + " is unavailable in plan mode — it mutates the workspace or spawns a process outside the read-only gate. Exit with /plan off first.",
		})
		return m, nil
	}
	switch command.kind {
	case commandEmpty:
		return m, nil
	case commandHelp:
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: helpText()})
		return m, nil
	case commandClear:
		// A foreground loop keeps firing after /clear (it wipes the screen, not the
		// session), so warn once before clearing the context it will run into.
		if m.loopActive() && m.loopLeavePrompt != commandClear {
			m.loopLeavePrompt = commandClear
			m = m.appendLoopSystem(m.loopFooterSummary() + " still running — /clear keeps them firing behind a cleared screen. Run /clear again to confirm, or /loop stop all first.")
			return m, nil
		}
		m.loopLeavePrompt = commandEmpty
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionClear})
		// Clearing wipes the visible transcript only — the session's context is
		// intact, so the next prompt still replays the full history. Say so, and
		// point to /new, so "cleared screen" isn't mistaken for "fresh context."
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Transcript cleared. The agent still has the full session context — use /new to start a fresh session."})
		// Scrollback above can't be un-printed; a faint divider marks where the
		// cleared surface ended and the frontier restarts for the fresh transcript.
		m.resetFlushFrontier("· cleared ·")
		// /clear wipes the transcript but not the session, so loops keep running;
		// say so rather than let them silently fire into a "cleared" screen.
		if m.loopActive() {
			m = m.appendLoopSystem(m.loopFooterSummary() + " still running — /loop stop all to end them.")
		}
		return m, nil
	case commandNew:
		// A fresh session mid-run would strand the in-flight turn's events; make the
		// user cancel first. Idle, /new saves the current session (already on disk)
		// and clears the conversation in place.
		if m.pending || m.compactInFlight {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "A run is in progress. Press Esc to cancel it first, then /new."})
			return m, nil
		}
		return m.startNewSession(), nil
	case commandBTW:
		return m.handleBTWCommand(command.text)
	case commandLoop:
		return m.handleLoopCommand(command.text)
	case commandGoal:
		return m.handleGoalCommand(command.text)
	case commandPets:
		return m.handlePetsCommand(command.text)
	case commandExit:
		// Closing the session stops its foreground loops mid-task; warn once so a
		// token-spending loop isn't ended by reflex.
		if m.loopActive() && m.loopLeavePrompt != commandExit {
			m.loopLeavePrompt = commandExit
			m = m.appendLoopSystem(m.loopFooterSummary() + " active — closing the session stops them. Run /exit again to confirm, or /loop stop all first.")
			return m, nil
		}
		m.loopLeavePrompt = commandEmpty
		// /exit gets the same protection as Ctrl+C: cancel any in-flight run and
		// defer the quit until its checkpoint session events flush — quitting
		// immediately would orphan the blobs and break /rewind.
		m.cancelRun()
		m.exiting = true
		if len(m.flushRunIDs) > 0 {
			return m, nil
		}
		return m.quit()
	case commandTools:
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: m.toolsText()})
		return m, nil
	case commandSkills:
		// With skills installed, /skills opens a searchable picker (like /model);
		// the text card remains only as the no-skills install hint.
		if picker := m.newSkillPicker(); picker != nil {
			m.picker = picker
			return m, nil
		}
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: m.skillsText()})
		return m, nil
	case commandMCP:
		if strings.TrimSpace(command.text) == "" {
			return m.openMCPManager(), nil
		}
		return m.startMCPTranscriptCommand(command.text)
	case commandPermissions:
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: m.permissionsText()})
		return m, nil
	case commandPS:
		if len(m.backgroundTerminalSessions()) == 0 {
			return m.showTransientNoticeInline("No background terminals running.", transientNoticeInfo), nil
		}
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: m.backgroundTerminalsText()})
		return m, nil
	case commandStop:
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: m.stopBackgroundTerminalsText(command.text)})
		return m, nil
	case commandAttach:
		return m.attachTerminalCommand(command.text)
	case commandSandboxSetup:
		return m.startSandboxSetupCommand(command.text)
	case commandProvider:
		arg := strings.ToLower(strings.TrimSpace(command.text))
		if arg == "" || arg == "add" {
			if m.pending {
				m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: pickerBusyText(command.name)})
				return m, nil
			}
			// Bare /provider opens the list-first manager over the saved
			// providers; /provider add jumps straight into the add wizard.
			if arg == "add" {
				m.providerWizard = m.newProviderWizard()
				m.clearSuggestions()
				return m, nil
			}
			return m.openProviderManager()
		}
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: m.providerText()})
		return m, nil
	case commandModel:
		if strings.TrimSpace(command.text) == "" {
			if m.pending {
				m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: pickerBusyText(command.name)})
				return m, nil
			}
			next, cmd := m.openModelPicker()
			if next.picker != nil {
				return next, cmd
			}
		}
		previousProvider, previousModel := m.providerName, m.modelName
		text := ""
		var persistErr error
		m, text, persistErr = m.handleModelCommand(command.text)
		if m.providerName != previousProvider || m.modelName != previousModel {
			// The status text this branch drops is the only other place the failure
			// appears, so the typed command needs the qualifier as much as the picker.
			notice, tone := m.modelAppliedNoticeFor(persistErr)
			return m.showTransientNoticeInline(notice, tone), nil
		}
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: text})
		return m, nil
	case commandSTTModel:
		if m.pending {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: pickerBusyText(command.name)})
			return m, nil
		}
		return m.openSTTModelPicker()
	case commandVoice:
		return m.toggleVoiceMode()
	case commandContext:
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: m.contextText()})
		return m, nil
	case commandConfig:
		if arg := strings.ToLower(strings.TrimSpace(command.text)); arg != "" {
			var text string
			m, text = m.handleConfigCommand(arg)
			m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: text})
			return m, nil
		}
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: m.configText()})
		return m, nil
	case commandDebug:
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: m.debugText()})
		return m, nil
	case commandPlan:
		text := ""
		m, text = m.handlePlanCommand(command.text)
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: text})
		return m, nil
	case commandDoctor:
		return m.startDoctorCommand(command.text)
	case commandSearch:
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: m.searchText(command.text)})
		return m, nil
	case commandResume:
		if m.pending {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{
				kind: actionAppendError,
				text: "Cannot resume sessions while a run is active.",
			})
			return m, nil
		}
		// Bare `/resume` opens an interactive session picker (like /model & /provider);
		// `/resume <id>` and `/resume latest` still resolve directly. The picker falls
		// back to the text path when there is nothing to resume.
		if strings.TrimSpace(command.text) == "" {
			if next, ok := m.openSessionPicker(); ok {
				return next, nil
			}
		}
		text := ""
		m, text = m.handleResumeCommand(command.text)
		if strings.HasPrefix(text, sessionsCardsPrefix) {
			// The list payload renders as stacked session cards, not a note.
			m.transcript = appendTranscriptRow(m.transcript, transcriptRow{
				kind: rowSystem,
				tool: "sessions",
				text: strings.TrimPrefix(text, sessionsCardsPrefix),
			})
		} else if text != "" {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: text})
		}
		return m, nil
	case commandRename:
		if title := strings.TrimSpace(command.text); title != "" {
			return m.renameActiveSession(title), nil
		}
		return m.openSessionRenamePrompt(), nil
	case commandSpec:
		return m.handleSpecCommand(command.text)
	case commandInit:
		return m.handleInitCommand()
	case commandCompact:
		text := ""
		var compactCmd tea.Cmd
		m, text, compactCmd = m.handleCompactCommand(command.text)
		m = m.setCompactStatusRow(text)
		return m, compactCmd
	case commandTranscript:
		return m.toggleDetailedTranscript(), nil
	case commandRewind:
		text := ""
		m, text = m.handleRewindCommand(command.text)
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: text})
		return m, nil
	case commandEffort:
		if strings.TrimSpace(command.text) == "" {
			if m.pending {
				m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: pickerBusyText(command.name)})
				return m, nil
			}
			if picker := m.newEffortPicker(); picker != nil {
				m.picker = picker
				return m, nil
			}
		}
		previous := m.reasoningEffort
		text := ""
		m, text = m.handleEffortCommand(command.text)
		if m.reasoningEffort != previous {
			return m.showTransientNoticeInline(m.effortAppliedNotice(), transientNoticeSuccess), nil
		}
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: text})
		return m, nil
	case commandFast:
		previous := m.activeServiceTier()
		text := ""
		m, text = m.handleFastCommand(command.text)
		if m.activeServiceTier() != previous {
			return m.showTransientNoticeInline(m.fastAppliedNotice(), transientNoticeSuccess), nil
		}
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: text})
		return m, nil
	case commandStyle:
		previous := m.responseStyle
		text := ""
		m, text = m.handleStyleCommand(command.text)
		if m.responseStyle != previous {
			return m.showTransientNoticeInline("Style: "+m.responseStyle, transientNoticeSuccess), nil
		}
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: text})
		return m, nil
	case commandSelfCorrect:
		previous := m.selfCorrectTests
		text := ""
		m, text = m.handleSelfCorrectCommand(command.text)
		if m.selfCorrectTests != previous {
			return m.showTransientNoticeInline(m.selfCorrectAppliedNotice(), transientNoticeSuccess), nil
		}
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: text})
		return m, nil
	case commandTurns:
		// Changing the budget mid-run would mutate the inherited ZERO_MAX_TURNS env
		// that sub-agents spawned later in THIS run read, making the run's budget
		// inconsistent. Require an idle session (the new budget applies next run).
		if m.pending && strings.TrimSpace(command.text) != "" {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Turns\nFinish or stop the current run before changing the tool-turn budget."})
			return m, nil
		}
		previous := m.agentOptions.MaxTurns
		text := ""
		m, text = m.handleTurnsCommand(command.text)
		if m.agentOptions.MaxTurns != previous {
			return m.showTransientNoticeInline(m.turnsAppliedNotice(), transientNoticeSuccess), nil
		}
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: text})
		return m, nil
	case commandProfile:
		// Same idle-session rule as /turns: switching the profile mutates the
		// turn budget (and its ZERO_MAX_TURNS propagation), so a change needs
		// an idle session; bare /profile (status) is always allowed.
		if m.pending && strings.TrimSpace(command.text) != "" && !strings.EqualFold(strings.TrimSpace(command.text), "status") {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Profile\nFinish or stop the current run before switching the execution profile."})
			return m, nil
		}
		previous := m.execProfileName
		text := ""
		m, text = m.handleProfileCommand(command.text)
		if m.execProfileName != previous {
			return m.showTransientNoticeInline(m.profileAppliedNotice(), transientNoticeSuccess), nil
		}
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: text})
		return m, nil
	case commandTheme:
		// Bare `/theme` opens a picker with a contained candidate preview. An
		// explicit `/theme <name>` (or `/theme list`) still runs the text handler.
		if strings.TrimSpace(command.text) == "" {
			m.picker = m.newThemePicker()
			return m, nil
		}
		text := ""
		m, text = m.handleThemeCommand(command.text)
		if validThemeMode(command.text) && !strings.Contains(text, "could not save theme preference") {
			return m.showTransientNotice(m.themeAppliedNotice(), transientNoticeSuccess)
		}
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: text})
		return m, nil
	case commandNotify:
		// Bare `/notify` opens the popup picker so the user can pick mode + focus
		// with arrow keys, matching /model and /theme. An explicit
		// `/notify off|bell|notify|both [unfocused|always]` runs the text handler.
		if strings.TrimSpace(command.text) == "" {
			m.picker = m.newNotifyPicker()
			return m, nil
		}
		text := ""
		m, text = m.handleNotifyCommand(command.text)
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: text})
		return m, nil
	case commandImage:
		m = m.handleImageCommand(command.text)
		return m, nil
	case commandAddDir:
		m = m.handleAddDirCommand(command.text)
		return m, nil
	case commandUnknown:
		// A "/name" not in the builtin registry may be a user-defined command
		// from .zero/commands/<name>.md — expand its template and run it as a
		// normal prompt before reporting "unknown".
		if next, cmd, handled := m.handleUserCommand(command.text); handled {
			return next, cmd
		}
		// Then an installed skill: "/<skill-name> [args]" runs the skill directly
		// (deterministic invocation, vs waiting for the model to pull it in).
		if next, cmd, handled := m.handleSkillCommand(command.text); handled {
			return next, cmd
		}
		m.transcript = reduceTranscript(m.transcript, transcriptAction{
			kind: actionAppendError,
			text: "unknown command: " + command.text,
		})
		return m, nil
	case commandBash:
		cmdText := strings.TrimSpace(command.text)
		if cmdText == "" {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Usage: !<shell command>"})
			return m, nil
		}
		// A "!cmd" shell escape runs OUTSIDE the agent sandbox, so gate it behind
		// the explicit unsafe permission mode. In auto/ask mode it is not executed;
		// the user is told how to enable it. This keeps a sandbox-bypassing exec
		// from running without a deliberate safety posture.
		if m.permissionMode != agent.PermissionModeUnsafe {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{
				kind: actionAppendSystem,
				text: "Shell escape (!) is disabled in " + string(m.permissionMode) + " mode — it bypasses the sandbox. Relaunch with --skip-permissions-unsafe to run shell commands directly.",
			})
			return m, nil
		}
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "$ " + cmdText})
		return m, runBashEscape(m.cwd, cmdText)
	case commandRetry:
		// /retry launches a run, so it needs the same guards a normal prompt gets:
		// never start one while exiting (would strand the shutdown flush) or during
		// compaction (would race compactResultMsg's wholesale rewrite of
		// transcript/sessionEvents and silently drop events).
		if m.exiting {
			return m, nil
		}
		if m.pending {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Retry\ncannot retry while a run is in progress."})
			return m, nil
		}
		if m.compactInFlight {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{
				kind: actionAppendSystem,
				text: "Retry\nstatus: warning\nCompaction is running. Retry once it finishes.",
			})
			return m, nil
		}
		if strings.TrimSpace(m.lastPrompt) == "" {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Retry\nno previous prompt to resend."})
			return m, nil
		}
		// Re-stage the attachments the last prompt carried so launchPrompt rebuilds
		// an identical request (document preamble + images + vision re-check). Without
		// this the queues are empty and /retry would resend a text-only prompt,
		// silently dropping the image/PDF context and answering a different task.
		m.pendingImages = m.lastImages
		m.pendingImageLabels = m.lastImageLabels
		m.pendingDocuments = m.lastDocuments
		m.refreshPendingImageThumbnail()
		return m.launchPrompt(m.lastPrompt)
	case commandEdit:
		if strings.TrimSpace(m.lastPrompt) == "" {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Edit\nno previous prompt to recall."})
			return m, nil
		}
		// Re-stage the remembered attachments alongside the recalled text so an
		// edited resend carries the same image/PDF context — the reappearing chip
		// row is the visible confirmation. Without this, editing a vision- or
		// document-backed prompt would silently submit a text-only version and
		// answer a different task (the same gap /retry guards against).
		m.pendingImages = m.lastImages
		m.pendingImageLabels = m.lastImageLabels
		m.pendingDocuments = m.lastDocuments
		m.refreshPendingImageThumbnail()
		m.input.SetValue(m.lastPrompt)
		return m, nil
	case commandCopy:
		text := m.lastAssistantAnswer()
		if strings.TrimSpace(text) == "" {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Copy\nno answer to copy yet."})
			return m, nil
		}
		return m, copyTranscriptSelectionCmd(text)
	case commandExport:
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: m.handleExportCommand(command.text)})
		return m, nil
	case commandPrompt:
		if intent, ok := detectMCPSetupIntent(command.text); ok {
			return m.openMCPAddWizardFromIntent(intent), nil
		}
		return m.launchPrompt(command.text)
	default:
		return m, nil
	}
}

// executeSlash runs a builtin slash command as if the user typed it and pressed
// Enter, without clearing the composer draft. Used by Ctrl+X leader chords so a
// mid-type prompt is preserved while /model (etc.) still opens.
func (m model) executeSlash(input string) (tea.Model, tea.Cmd) {
	command := parseCommand(input)
	if command.kind == commandEmpty || command.kind == commandPrompt {
		return m, nil
	}
	if m.loopLeavePrompt != commandEmpty && command.kind != m.loopLeavePrompt {
		m.loopLeavePrompt = commandEmpty
	}
	m.rememberInput(input)
	m.clearSuggestions()
	m.chatScrollOffset = 0
	return m.dispatchCommand(command)
}

// launchPrompt starts a normal agent turn from text already accepted by the
// composer. Queued prompts use this path too, so session and image behavior
// stays identical to immediate submissions.
func (m model) launchPrompt(prompt string) (model, tea.Cmd) {
	return m.launchPromptInternal(prompt, nil)
}

func (m model) launchPromptInternal(prompt string, peer *peermsg.InboundMessage) (model, tea.Cmd) {
	// Remember the verbatim prompt (before specialist/document expansion) so /retry
	// and /edit can act on exactly what the user submitted. Snapshot the staged
	// attachments too: launchPrompt clears the pending queues below, so /retry
	// re-stages these to resend an identical vision/PDF-backed request rather than
	// a degraded text-only one.
	var attachments transcriptAttachmentSummary
	if peer == nil {
		m.lastPrompt = prompt
		m.lastImages = m.pendingImages
		m.lastImageLabels = m.pendingImageLabels
		m.lastDocuments = m.pendingDocuments
		attachments = transcriptAttachmentSummary{
			images:    len(m.pendingImages),
			documents: len(m.pendingDocuments),
		}
		// A switched model may no longer accept a staged image. The matching
		// system notice below explains the drop; the sent user row must not claim
		// the image was included.
		if attachments.images > 0 && !m.modelSupportsVisionTUI() {
			attachments.images = 0
		}
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendUser, text: prompt, attachments: attachments})
	} else {
		m.transcript = appendTranscriptRow(m.transcript, peerTranscriptRow(peerDisplayName(peer.From), peer.Body))
	}
	if m.provider == nil {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{
			kind: actionAppendAssistant,
			text: "No provider configured. Run `zero setup` (guided) or `zero auth` (OAuth) from a shell, or set a provider API key env var, then relaunch.",
		})
		return m, nil
	}
	// A leading "@specialist <task>" is expanded into an explicit Task-delegation
	// directive for the agent only; the transcript above keeps the user's verbatim
	// "@mention". Non-mentions and mid-message "@file" references are unchanged.
	if peer == nil {
		if expanded, ok := expandSpecialistMention(prompt, m.agentOptions.Specialists); ok {
			prompt = expanded
		}
	}
	// Prepend any staged PDF document text as a model-facing preamble. The
	// visible transcript above keeps the user's clean prompt; the agent (and the
	// recorded session, for resume fidelity) sees the document text first.
	if peer == nil {
		if preamble := m.consumePendingDocuments(); preamble != "" {
			prompt = preamble + prompt
		}
	}
	var err error
	m, err = m.ensureActiveSession(prompt)
	if err != nil {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{
			kind: actionAppendError,
			text: "session create error: " + err.Error(),
		})
	} else {
		if m.activeLoopID == "" && sessions.IsResumableKind(m.activeSession.SessionKind) &&
			m.activeSession.Goal != nil && m.activeSession.Goal.Status == sessions.GoalStatusActive {
			if updated, resetErr := m.sessionStore.ResetGoalContinuations(m.activeSession.SessionID); resetErr != nil {
				m = m.appendGoalError("reset automatic continuation count: " + resetErr.Error())
			} else {
				m.activeSession = updated
			}
		}
		agentPrompt := m.sessionPrompt(prompt)
		messagePayload := map[string]any{
			"role":    "user",
			"content": prompt,
		}
		if peer == nil {
			if !attachments.empty() {
				messagePayload["attachments"] = map[string]int{
					"images":    attachments.images,
					"documents": attachments.documents,
				}
			}
		}
		if peer != nil {
			messagePayload["origin"] = "cross_session"
			messagePayload["from"] = peerDisplayName(peer.From)
			messagePayload["messageId"] = peer.ID
			messagePayload["displayContent"] = peer.Body
		}
		m, err = m.appendSessionEvent(sessions.EventMessage, messagePayload)
		if err != nil {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{
				kind: actionAppendError,
				text: "session record error: " + err.Error(),
			})
		}
		prompt = agentPrompt
	}
	// Re-check vision support against the CURRENT effective model at submit
	// time, not just at /image attach time: the user may have attached on a
	// vision model and then /model-switched to a non-vision one. If the active
	// model can't accept images, drop them (with an inline notice mirroring
	// exec's drop+warn wording) rather than sending them to a model that
	// rejects them. Pending state is cleared either way below.
	turnImages := m.pendingImages
	if peer != nil {
		turnImages = nil
	}
	if len(turnImages) > 0 && !m.modelSupportsVisionTUI() {
		name := m.modelName
		if name == "" {
			name = "the active model"
		}
		m.transcript = reduceTranscript(m.transcript, transcriptAction{
			kind: actionAppendSystem,
			text: fmt.Sprintf("Model %s does not support image input; ignoring %d image(s).", name, len(turnImages)),
		})
		turnImages = nil
	}
	if peer == nil {
		m.pendingImages = nil
		m.pendingImageLabels = nil
		m.pendingImageThumbnails = nil
	}
	runCtx, cancel := context.WithCancel(m.ctx)
	if peer != nil {
		runCtx = peermsg.WithInboundMessage(runCtx, *peer)
	}
	m = m.beginRun(cancel)
	var agentCmd tea.Cmd
	if peer != nil {
		agentCmd = m.runAgentWithOptions(m.activeRunID, runCtx, prompt, turnImages, tuiAgentRunOptions{
			transientSystemPrompt: peerTurnSystemPrompt,
		})
	} else {
		agentCmd = m.runAgent(m.activeRunID, runCtx, prompt, turnImages)
	}
	return m, tea.Batch(agentCmd, m.spinner.Tick)
}

// beginRun stamps the shared run-start state for a new agent turn: a fresh run
// ID, the cancel func, pending = true, the turn-start timestamp (the source for
// the working status line's live elapsed clock), and a reset working-verb
// rotation so the brand word shows first. Centralized so every launch path
// (normal prompt + spec draft/impl) keeps these in sync — a missing
// turnStartedAt previously dropped the elapsed timer on spec-mode runs.
func (m model) beginRun(cancel context.CancelFunc) model {
	m = m.cancelIdleRecap()
	if m.prepareRunCompletionWarning != nil {
		m.prepareRunCompletionWarning()
	}
	m.runID++
	m.activeRunID = m.runID
	m.terminalAutoAttach = nil
	m.runCancel = cancel
	m.pending = true
	// Clear per-run tracking state so stale specialists and plans from the
	// previous turn don't bleed into the new one.
	m.specialists.clear()
	m.plan.clear()
	m.stepWork = nil
	m.stepNarration = nil
	m.stepExplanation = nil
	m.planDetailOpen = false
	m.planDetailGen++ // invalidate any in-flight step-explanation from the prior run
	m.runDetailsOpen = false
	m.turnStartedAt = m.now()
	m.turnTimer = newActiveTurnTimer(m.turnStartedAt)
	m.lastStreamActivity = m.turnStartedAt
	m.turnStreamedRunes = 0
	m.spinnerTicking = true
	return m
}

// ensureSpinnerTick returns the spinner.Tick cmd to (re)start the self-scheduling
// tick loop when active agent state needs its short lifecycle fade but the loop
// is not already running (e.g. a resumed session with live agents before any
// run starts). It returns nil — issuing no second timer — when the loop is
// already alive, reduced motion is set, or nothing needs animation.
func (m *model) ensureSpinnerTick() tea.Cmd {
	if m.spinnerTicking || m.reducedMotion {
		return nil
	}
	if !m.sidebarHasAgents() && !m.aimlapiOnboardAnimating() {
		return nil
	}
	m.spinnerTicking = true
	return m.spinner.Tick
}

func (m model) launchQueuedMessageIfReady() (model, tea.Cmd) {
	if !m.hasQueuedMessage() || m.pending || m.exiting || m.pendingPermission != nil || m.pendingAskUser != nil || m.pendingSpecReview != nil {
		return m, nil
	}
	prompt := m.queuedMessage
	m.queuedMessage = ""
	return m.launchPrompt(prompt)
}

// historyRecallActive reports whether ↑/↓ should navigate previously submitted
// inputs: history exists and no modal surface owns the arrow keys.
func (m model) historyRecallActive() bool {
	return len(m.inputHistory) > 0 &&
		m.pendingAskUser == nil && m.pendingPermission == nil && m.pendingSpecReview == nil
}

// recallHistory steps through submitted inputs (-1 = older, +1 = newer),
// stashing the in-progress draft so stepping back past the newest recalled
// entry restores whatever was being typed.
func (m model) recallHistory(direction int) model {
	if m.historyIdx == len(m.inputHistory) {
		if direction > 0 {
			return m
		}
		m.historyDraft = m.composerValue()
	}
	next := clamp(m.historyIdx+direction, 0, len(m.inputHistory))
	if next == m.historyIdx {
		return m
	}
	m.historyIdx = next
	if next == len(m.inputHistory) {
		m.input.SetValue(m.historyDraft)
	} else {
		m.input.SetValue(m.inputHistory[next])
	}
	m.input.CursorEnd()
	m.resetComposerFromInput()
	m.recomputeSuggestions()
	return m
}

// rememberInput records a submitted composer value for ↑ recall and resets the
// navigation cursor past the newest entry.
func (m *model) rememberInput(value string) {
	trimmed := strings.TrimSpace(value)
	if trimmed != "" && (len(m.inputHistory) == 0 || m.inputHistory[len(m.inputHistory)-1] != trimmed) {
		m.inputHistory = append(m.inputHistory, trimmed)
	}
	m.historyIdx = len(m.inputHistory)
	m.historyDraft = ""
}

func (m *model) cancelRun() {
	goalWasActive := m.pending && m.activeSession.Goal != nil &&
		m.activeSession.Goal.Status == sessions.GoalStatusActive
	if m.runCancel != nil {
		m.runCancel()
	}
	m.clearStreamingToolCall() // a cancelled file-write must not linger into the next run
	// A cancelled loop iteration bypasses the agentResponseMsg completion seam (its
	// late message is drained through flushRunIDs, not advanceLoop), so clear the
	// loop tag here and re-arm the interrupted loop for its next cadence. Otherwise
	// the loop is left "running" forever (nextRunAt stays zero) and the NEXT
	// unrelated turn would be misattributed as this loop's completion.
	if m.activeLoopID != "" {
		if l := m.findLoop(m.activeLoopID); l != nil {
			if l.mode == loopModeSelfPaced {
				l.nextRunAt = m.now().Add(clampSelfPaceDelay(adaptiveSelfPaceDelay(l.iteration)))
			} else {
				l.nextRunAt = m.now().Add(l.interval)
			}
		}
		m.activeLoopID = ""
	}
	// Remember the in-flight run — and the session it was recording into — so
	// its final agentResponseMsg is still drained for session-event persistence
	// after activeRunID is cleared. Otherwise the checkpoint blobs it captured
	// before each mutating tool are orphaned on disk and /rewind can't reference
	// them; without the session id, a /resume before the flush lands would
	// append the old run's events into the newly active session.
	if m.pending && m.activeRunID != 0 {
		if m.flushRunIDs == nil {
			m.flushRunIDs = make(map[int]string)
		}
		m.flushRunIDs[m.activeRunID] = m.activeSession.SessionID
	}
	if m.pending {
		// A cancelled run must terminate visibly in the transcript: first the
		// partial streamed answer (if any), then the cancellation marker — the
		// session log gets the same marker below.
		if row, ok := reasoningTranscriptRow("", m.activeRunID, m.streamingReasoning); ok {
			m.transcript = appendTranscriptRow(m.transcript, row)
		}
		if text := strings.TrimRight(m.streamingTextString(), "\n"); strings.TrimSpace(text) != "" {
			m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowAssistant, text: text})
		}
		m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowSystem, text: "Run cancelled."})
	}
	if m.pending && m.activeSession.SessionID != "" {
		if next, err := (*m).appendSessionEvent(sessions.EventError, map[string]any{
			"message": "Run cancelled.",
		}); err == nil {
			*m = next
		}
	}
	if goalWasActive && m.sessionStore != nil && m.activeSession.SessionID != "" {
		updated, event, err := m.sessionStore.PauseGoalIfActive(m.activeSession.SessionID, "run cancelled by user")
		if err != nil {
			m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowError, text: "Goal: pause after cancellation: " + err.Error()})
		} else {
			m.activeSession = updated
			if event != nil {
				m.sessionEvents = append(m.sessionEvents, *event)
				m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowSystem, text: "Goal paused. Use /goal resume to continue."})
			}
		}
	}
	m.pending = false
	m.runCancel = nil
	m.activeRunID = 0
	m.terminalAutoAttach = nil
	m.cancelConfirmActive = false // whatever path got here, there's nothing left to confirm cancelling
	m.plan.frozenAt = m.now()     // freeze the plan clock while idle (no run in flight)
	m.pendingPermission = nil
	m.pendingAskUser = nil
	// The interim block renders streamingText live; a cancelled run's partial
	// answer must not leak into (and concatenate with) the next turn's stream.
	m.streamingText = nil
	m.streamingReasoning = ""
	m.streamingReasoningExpanded = false
	// Hard-stop the fade and drop the per-line age map. The next turn's
	// first agentTextMsg will seed a fresh lineAges slice and restart
	// the tick.
	m.resetStreamingFade()
}

func (m model) runAgent(runID int, runCtx context.Context, prompt string, images []zeroruntime.ImageBlock) tea.Cmd {
	return m.runAgentWithOptions(runID, runCtx, prompt, images, tuiAgentRunOptions{})
}

// selfCorrectAutonomyForMode maps the active permission mode to the self-correct
// autonomy gate: more autonomous modes auto-fix after a failed verification,
// while restrictive modes only surface the failure. Mirrors exec's --auto levels.
func selfCorrectAutonomyForMode(mode agent.PermissionMode) string {
	switch mode {
	case agent.PermissionModeUnsafe:
		return "high"
	case agent.PermissionModeAuto:
		return "medium"
	default: // ask, etc. — report the failure without starting an auto-fix round
		return "low"
	}
}

func (m model) runAgentWithOptions(runID int, runCtx context.Context, prompt string, images []zeroruntime.ImageBlock, runOptions tuiAgentRunOptions) tea.Cmd {
	var activeDiscovered []providermodeldiscovery.Model
	if models, ok := m.discoveredModelsForActiveRoute(); ok {
		activeDiscovered = make([]providermodeldiscovery.Model, len(models))
		for i, dm := range models {
			copied := dm
			if len(dm.InputModalities) > 0 {
				copied.InputModalities = append([]string{}, dm.InputModalities...)
			}
			activeDiscovered[i] = copied
		}
	}
	catalog := m.modelCatalog

	return func() tea.Msg {
		started := m.now()
		if m.turnTimer != nil {
			m.turnTimer.start(started)
		}
		// firstTokenElapsed is stamped from the pause-aware turn timer when the
		// first reasoning or text token streams, so TTFT and total elapsed use
		// the same clock.
		var firstTokenElapsed time.Duration
		firstTokenSeen := false
		stampFirstToken := func(at time.Time) {
			if firstTokenSeen {
				return
			}
			firstTokenSeen = true
			firstTokenElapsed = m.activeTurnElapsedAt(started, at)
		}
		toolCalls := 0
		rows := []transcriptRow{}
		usageEvents := []zeroruntime.Usage{}
		sessionEvents := []pendingSessionEvent{}
		usageModelID := m.modelName
		// usageModelIDs records, per usage event, the model in force when it
		// fired; the escalation switcher reassigns usageModelID mid-run.
		usageModelIDs := []string{}
		var specReview *pendingSpecReviewPrompt
		if m.awaitToolReadiness != nil {
			m.awaitToolReadiness(runCtx)
		}
		options := m.agentOptions
		options.Registry = cloneToolRegistry(m.registry)
		goalAwareRun := !runOptions.specDraft && m.activeLoopID == "" &&
			sessions.IsResumableKind(m.activeSession.SessionKind)
		if goalAwareRun {
			options.Registry = m.goalRegistry()
		}
		if runOptions.registry != nil {
			options.Registry = cloneToolRegistry(runOptions.registry)
			if goalAwareRun && m.activeSession.SessionID != "" {
				for _, tool := range tools.NewGoalTools(m.sessionStore, m.activeSession.SessionID) {
					options.Registry.Register(tool)
				}
			}
		}
		peerAwareRun := runOptions.transientSystemPrompt != "" || m.sessionContainsPeerMessages()
		if peerAwareRun && m.peerService != nil {
			options.Registry.Register(tools.NewPeerReplyTool(m.peerService))
		}
		options.PermissionMode = m.permissionMode
		if runOptions.permissionMode != "" {
			options.PermissionMode = runOptions.permissionMode
		}
		if runOptions.systemPrompt != "" {
			options.SystemPrompt = runOptions.systemPrompt
		}
		if runOptions.transientSystemPrompt != "" {
			options.TransientSystemPrompt = runOptions.transientSystemPrompt
		} else if peerAwareRun {
			options.TransientSystemPrompt = peerTurnSystemPrompt
		}
		if goalAwareRun {
			options.SystemPrompt = m.goalSystemPrompt(options.SystemPrompt)
		}
		options.SessionID = m.activeSession.SessionID
		options.ProviderName = m.providerName
		options.Model = m.modelName
		options.ReasoningEffort = string(m.reasoningEffort)
		options.ServiceTier = m.activeServiceTier()
		options.ResponseStyle = m.responseStyle
		options.Cwd = m.cwd
		options.Images = images
		if m.newTurnSessionProvider != nil && m.provider != nil {
			options.TurnSessionProvider = m.newTurnSessionProvider(m.providerProfile, m.provider)
		}
		if m.captureRunImages != nil {
			m.captureRunImages(images)
		}
		// Enable agent-loop compaction sized to the active model's context window.
		// AgentContextWindow applies a positive fallback for unknown/custom models so
		// compaction (proactive + reactive) is enabled for every model, not just
		// catalogued ones.
		options.ContextWindow = modelregistry.AgentContextWindow(m.modelContextWindow(m.modelName))
		// Route compaction summarization to a cheap model when one applies
		// (env > config > curated default). The factory is lazy, re-evaluated
		// against the model in force when the compactor asks (so an escalation
		// away from the cheap model gains a summarizer), and the agent falls
		// back to the main provider on any summarizer failure. Resolved against
		// the ACTIVE profile so it tracks mid-session model switches.
		options.Summarizer = providers.CompactionSummarizerFactory(m.providerProfile, m.compactionModel, m.newProvider)
		// Let compaction re-derive its threshold after a mid-run escalate_model
		// switch instead of keeping the original model's window.
		options.ContextWindowFor = func(modelID string) int {
			return modelregistry.AgentContextWindow(m.modelContextWindow(modelID))
		}
		// And make that switch reachable, when the operator asked for it. The
		// consequences of an escalation were already handled here (the window
		// above, and the summarizer resolved against the active profile) while
		// nothing on this surface could cause one: escalate_model was registered
		// only by exec.
		//
		// BUILT FROM THE ACTIVE PROFILE, NOT THE STARTUP ONE. A TUI session can
		// change models with /model, so escalating from the profile captured at
		// launch would switch from whatever the session began with rather than
		// from what is in force now, and would carry that stale profile's base URL
		// and credential with it. m.providerProfile tracks the switches, which is
		// why this is built per turn rather than once in the caller.
		if m.allowEscalation {
			options.ModelSwitcher, options.ModelSessionSwitcher = providers.EscalationSwitchers(
				m.providerProfile, m.provider, m.newProvider,
				// Usage attribution follows the switch, as exec reassigns its
				// currentModel: every usage event after a real escalation is billed
				// to the escalated model, not the one the run started on.
				func(modelID string) { usageModelID = modelID },
				providers.Options{ModelRPM: m.modelRPM},
			)
		}
		options.SupportsVision = func(modelID string) bool {
			trimmed := strings.TrimSpace(modelID)
			if trimmed == "" {
				return false
			}
			if len(activeDiscovered) > 0 {
				if supported, ok := discoveredVisionSupport(activeDiscovered, trimmed); ok {
					return supported
				}
			}
			if entry, known := catalog.Resolve(trimmed); known {
				return entry.Supports(modelregistry.ModelCapabilityVision)
			}
			return modelregistry.SupportsVision(catalog, trimmed)
		}
		if m.captureRunSupportsVision != nil {
			m.captureRunSupportsVision(options.SupportsVision)
		}

		// Post-edit self-correction is on by default in the TUI but kept FAST: it
		// runs LSP diagnostics over the changed files only — cheap, change-scoped,
		// and a no-op when no language server is installed. The project test plan
		// (`go test ./...`, whole-repo) is NOT run per edit by default — that would
		// add the full suite's latency to every turn and let a pre-existing failure
		// hijack the agent — so the test half is opt-in via `/selfcorrect on`
		// (m.selfCorrectTests). The spec-draft (planning) path never wires it,
		// matching exec; the per-turn lsp.Manager is torn down when this run
		// returns; auto-fix vs report-only follows the active permission mode.
		if !runOptions.specDraft && options.Cwd != "" {
			// Prefer the session-long manager (kept warm across prompts). Only when it
			// is absent — e.g. cwd was unknown at construction, or a test built the
			// model directly — fall back to a per-run manager that is shut down here.
			lspManager := m.lspManager
			if lspManager == nil {
				lspManager = lsp.NewManager(options.Cwd)
				defer func() {
					shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					_ = lspManager.Shutdown(shutdownCtx)
				}()
			}
			options.SelfCorrect = agent.NewSelfCorrector(options.Cwd, agent.NewLSPDiagnosticsChecker(lspManager), agent.NewProjectVerifier(options.Cwd), agent.SelfCorrectConfig{
				Enabled:      true,
				IncludeTests: m.selfCorrectTests,
				IncludeLSP:   true,
				Autonomy:     selfCorrectAutonomyForMode(options.PermissionMode),
			})
			// Background post-edit diagnostics: the loop checks files changed by
			// edit_file/write_file off the tool-call path and nudges the model with
			// any errors before its next request. Shares the run's lazy manager.
			options.FileDiagnostics = agent.NewFileDiagnostics(lspManager, options.Cwd)
		}

		// Some providers synthesize tool-call ids that repeat within a run (e.g.
		// Gemini restarts its gemini_tool_N numbering on every provider turn).
		// Transcript rows need distinct ids for dedup and call→result collapse,
		// so repeats get an ordinal suffix; session payloads keep the provider's
		// original ids.
		callSeq := map[string]int{}
		reasoningText := ""
		reasoningSeq := 0
		var reasoningStarted time.Time
		var reasoningLast time.Time
		flushReasoning := func(closedAt time.Time) {
			if row, ok := reasoningTranscriptRow(fmt.Sprintf("reasoning_%d", reasoningSeq+1), runID, reasoningText); ok {
				if !reasoningStarted.IsZero() {
					if closedAt.IsZero() {
						closedAt = reasoningLast
					}
					if !reasoningLast.IsZero() && closedAt.Before(reasoningLast) {
						closedAt = reasoningLast
					}
					if elapsed := closedAt.Sub(reasoningStarted); elapsed > 0 {
						row.turnElapsed = elapsed
					}
				}
				reasoningSeq++
				rows = append(rows, row)
				m.sendAgentRow(runID, row)
			}
			reasoningText = ""
			reasoningStarted = time.Time{}
			reasoningLast = time.Time{}
		}

		onText := options.OnText
		options.OnText = func(delta string) {
			now := m.now()
			stampFirstToken(now)
			if strings.TrimSpace(reasoningText) != "" {
				flushReasoning(now)
			}
			m.sendAgentText(runID, delta)
			if onText != nil {
				onText(delta)
			}
		}
		// Stream a tool call's arguments live so a long write_file/edit shows the
		// code being written instead of a frozen spinner (see streamingToolCallView).
		options.OnToolCallStart = func(id, name string) {
			m.sendToolCallStreamStart(runID, id, name)
		}
		options.OnToolCallDelta = func(id, fragment string) {
			m.sendToolCallStreamDelta(runID, id, fragment)
		}
		onPermissionRequest := options.OnPermissionRequest
		options.OnPermissionRequest = func(ctx context.Context, request agent.PermissionRequest) (agent.PermissionDecision, error) {
			if m.turnTimer != nil {
				m.turnTimer.pause(m.now())
				defer func() {
					m.turnTimer.resume(m.now())
				}()
			}
			if onPermissionRequest != nil {
				return onPermissionRequest(ctx, request)
			}
			if m.runtimeMessageSink == nil {
				return agent.PermissionDecision{Action: agent.PermissionDecisionDeny, Reason: "permission prompt unavailable"}, nil
			}
			if m.notifier != nil {
				m.notifier.Notify(notify.AwaitingInput, notify.DefaultMessage(notify.AwaitingInput))
			}
			decisionCh := make(chan agent.PermissionDecision, 1)
			m.sendPermissionRequest(runID, request, func(decision agent.PermissionDecision) {
				select {
				case decisionCh <- decision:
				default:
				}
			})
			sessionEvents = append(sessionEvents, pendingSessionEvent{
				Type:    sessions.EventPermissionRequest,
				Payload: request,
			})
			select {
			case decision := <-decisionCh:
				if strings.TrimSpace(decision.Reason) == "" {
					decision.Reason = permissionDecisionReason(permissionDecision(decision.Action))
				}
				return decision, nil
			case <-ctx.Done():
				return agent.PermissionDecision{Action: agent.PermissionDecisionDeny, Reason: ctx.Err().Error()}, ctx.Err()
			}
		}

		onAskUser := options.OnAskUser
		options.OnAskUser = func(ctx context.Context, request agent.AskUserRequest) (agent.AskUserResponse, error) {
			if onAskUser != nil {
				return onAskUser(ctx, request)
			}
			if m.runtimeMessageSink == nil {
				// No interactive surface: let the loop degrade gracefully.
				return agent.AskUserResponse{}, fmt.Errorf("ask_user prompt unavailable")
			}
			// Only notify when there is actually something to answer — a request
			// with no questions auto-resolves without ever prompting the user.
			if m.notifier != nil && len(request.Questions) > 0 {
				m.notifier.Notify(notify.AwaitingInput, notify.DefaultMessage(notify.AwaitingInput))
			}
			answerCh := make(chan []string, 1)
			m.sendAskUserRequest(runID, request, func(answers []string) {
				select {
				case answerCh <- answers:
				default:
				}
			})
			sessionEvents = append(sessionEvents, pendingSessionEvent{
				Type:    sessions.EventMessage,
				Payload: askUserSessionPayload(request),
			})
			select {
			case answers := <-answerCh:
				// Persist the answers next to the question event so the exchange
				// is complete on /resume; rehydration renders them as a system note.
				sessionEvents = append(sessionEvents, pendingSessionEvent{
					Type: sessions.EventMessage,
					Payload: map[string]any{
						"role":       "ask_user_answers",
						"toolCallId": request.ToolCallID,
						"answers":    answers,
					},
				})
				return agent.AskUserResponse{Answers: answers}, nil
			case <-ctx.Done():
				return agent.AskUserResponse{}, ctx.Err()
			}
		}

		onReasoning := options.OnReasoning
		options.OnReasoning = func(delta string) {
			now := m.now()
			if strings.TrimSpace(delta) != "" {
				stampFirstToken(now)
			}
			if strings.TrimSpace(reasoningText) == "" && strings.TrimSpace(delta) != "" {
				reasoningStarted = now
			}
			if strings.TrimSpace(delta) != "" {
				reasoningLast = now
			}
			reasoningText += delta
			m.sendAgentReasoning(runID, delta)
			if onReasoning != nil {
				onReasoning(delta)
			}
		}

		onToolCall := options.OnToolCall
		options.OnToolCall = func(call agent.ToolCall) {
			flushReasoning(m.now())
			toolCalls++
			callSeq[call.ID]++
			row := transcriptRow{
				kind:   rowToolCall,
				id:     effectiveToolRowID(call.ID, callSeq[call.ID]),
				text:   "tool call: " + call.Name,
				tool:   call.Name,
				detail: argHint(call.Arguments),
				arg:    argHintSecondary(call.Arguments),
				runID:  runID,
			}
			// Specialist delegation and an in-flight plan update have dedicated UI, so
			// omit their redundant call cards from the transcript. The completed plan
			// result remains as a durable checklist in the conversation history.
			if !toolCallCardSuppressedInTranscript(call.Name) {
				rows = append(rows, row)
				m.sendAgentRow(runID, row)
			}
			// Track specialist delegation: when the Task tool is called, register
			// the specialist start so the specialist card + task table can show
			// live status. The child session ID is not known yet (it's created
			// inside the executor), so we use the tool call ID as a temporary
			// key and reconcile on the result.
			// A tty exec_command wants the attach overlay: the session registers
			// with the process manager inside the tool's Run, so the update loop
			// polls for it on a tick.
			if call.Name == tools.ExecCommandToolName && execCallWantsTTY(call.Arguments) && m.runtimeMessageSink != nil {
				known := map[int]bool{}
				if controller, ok := m.execSessionController(); ok {
					for _, session := range controller.ExecSessions() {
						known[session.ID] = true
					}
				}
				m.runtimeMessageSink(interactiveExecStartMsg{runID: runID, known: known})
			}
			if call.Name == "Task" {
				name, desc := parseTaskCallArgs(call.Arguments)
				if m.runtimeMessageSink != nil {
					m.runtimeMessageSink(specialistStartMsg{
						runID:          runID,
						name:           name,
						description:    desc,
						childSessionID: call.ID,
					})
				}
			}
			sessionEvents = append(sessionEvents, pendingSessionEvent{
				Type: sessions.EventToolCall,
				Payload: map[string]any{
					"id":        call.ID,
					"name":      call.Name,
					"arguments": call.Arguments,
				},
			})
			// Snapshot before-state of files this call will mutate, NOW (before the
			// mutation runs), then batch the checkpoint event IN ORDER with the other
			// session events so the recorded sequence matches execution (recording it
			// out-of-band would reorder it ahead of the batched tool_call/result).
			// SnapshotForCheckpoint writes the blobs; the batched event referencing
			// them is flushed at end-of-run AND on cancel (flushRunIDs), so the blobs
			// never stay orphaned — see its contract in internal/sessions.
			if m.sessionStore != nil && m.activeSession.SessionID != "" {
				var args map[string]any
				if call.Arguments != "" {
					_ = json.Unmarshal([]byte(call.Arguments), &args)
				}
				if targets := tools.MutationTargets(m.cwd, call.Name, args); len(targets) > 0 {
					if payload, ok := m.sessionStore.SnapshotForCheckpoint(m.activeSession.SessionID, m.cwd, call.Name, targets); ok {
						sessionEvents = append(sessionEvents, pendingSessionEvent{
							Type:    sessions.EventSessionCheckpoint,
							Payload: payload,
						})
					}
				}
			}
			if onToolCall != nil {
				onToolCall(call)
			}
		}

		options.OnToolProgress = func(toolCallID string, event streamjson.Event) {
			if event.Type == streamjson.EventToolCall && m.runtimeMessageSink != nil {
				m.runtimeMessageSink(specialistProgressMsg{
					runID:      runID,
					toolCallID: toolCallID,
					toolName:   event.Name,
					detail:     toolCallSummary(event),
				})
			}
		}

		onToolResult := options.OnToolResult
		options.OnToolResult = func(result agent.ToolResult) {
			if runOptions.specDraft {
				if info, ok := tuiSpecReviewFromToolResult(result, m.activeSession.SessionID); ok {
					specReview = &info
				}
			}
			row := transcriptRow{
				kind:            rowToolResult,
				id:              effectiveToolRowID(result.ToolCallID, callSeq[result.ToolCallID]),
				text:            toolResultRowText(result),
				tool:            result.Name,
				status:          result.Status,
				detail:          toolResultDetail(result),
				meta:            result.Meta,
				runID:           runID,
				changedFiles:    result.ChangedFiles,
				changeSummaries: result.ChangeSummaries,
			}
			// A successful Task/TaskOutput result is represented by a specialist card.
			// update_plan stays in the transcript as a rendered checklist; failures
			// always remain visible because a dedicated surface cannot explain them.
			if !toolResultCardSuppressedInTranscript(result.Name, result.Status) {
				rows = append(rows, row)
				m.sendAgentRow(runID, row)
			}
			// Keep the latest plan state in sync for run details and step drill-in.
			if result.Name == "update_plan" && m.registry != nil {
				if planTool, ok := m.registry.Get("update_plan"); ok {
					if reader, ok := planTool.(interface{ CurrentPlan() []tools.PlanItem }); ok {
						if m.runtimeMessageSink != nil {
							m.runtimeMessageSink(planUpdateMsg{runID: runID, items: reader.CurrentPlan()})
						}
					}
				}
			}
			sessionEvents = append(sessionEvents, pendingSessionEvent{
				Type:    sessions.EventToolResult,
				Payload: toolResultSessionPayload(result),
			})
			// Complete specialist tracking when the Task tool returns.
			if result.Name == "Task" {
				status := specialistCompleted
				if result.Status == tools.StatusError {
					status = specialistError
				}
				childSessionID := result.ToolCallID
				if sid, ok := result.Meta["session_id"]; ok && sid != "" {
					childSessionID = sid
				}
				if m.runtimeMessageSink != nil {
					m.runtimeMessageSink(specialistCompleteMsg{
						runID:          runID,
						toolCallID:     result.ToolCallID,
						childSessionID: childSessionID,
						status:         status,
						errorMsg:       result.Output,
					})
				}
			}
			// swarm_collect carries task_id -> session_id for completed members, so
			// the AGENTS sidebar rows can drill into a member's session like a
			// specialist card.
			if result.Name == "swarm_collect" && len(result.Meta) > 0 && m.runtimeMessageSink != nil {
				m.runtimeMessageSink(swarmSessionsMsg{runID: runID, sessions: result.Meta})
			}
			if onToolResult != nil {
				onToolResult(result)
			}
		}

		onPermission := options.OnPermission
		options.OnPermission = func(event agent.PermissionEvent) {
			// The audit event is recorded for every call so the session log stays
			// complete; the visible row is only emitted when the event carries
			// user-facing information (a real prompt, a denial, an explicit durable
			// grant), not for silent auto-approvals.
			if permissionEventIsNoteworthy(event) {
				row := permissionTranscriptRow(event)
				row.runID = runID
				rows = append(rows, row)
				m.sendAgentRow(runID, row)
			}
			sessionEvents = append(sessionEvents, pendingSessionEvent{
				Type:    tuiPermissionEventType(event),
				Payload: event,
			})
			if onPermission != nil {
				onPermission(event)
			}
		}

		onUsage := options.OnUsage
		options.OnUsage = func(event zeroruntime.Usage) {
			usageEvents = append(usageEvents, event)
			usageModelIDs = append(usageModelIDs, usageModelID)
			payload := usage.EventUsagePayload(event)
			// AND ON THE PERSISTED EVENT TOO, not only the in-memory record: the
			// report reconstructs cost from the payload, falling back to the
			// session-wide model, so an escalated run would be priced entirely at
			// the model it started on. Written only under escalation, which is the
			// only way the model in force can change mid-run, matching what exec
			// records under the same flag.
			if m.allowEscalation {
				payload["model"] = usageModelID
			}
			sessionEvents = append(sessionEvents, pendingSessionEvent{
				Type:    sessions.EventUsage,
				Payload: payload,
			})
			m.sendAgentUsage(runID, usageModelID, event)
			if onUsage != nil {
				onUsage(event)
			}
		}

		result, err := agent.Run(runCtx, prompt, m.provider, options)
		if err != nil {
			flushReasoning(m.now())
			sessionEvents = append(sessionEvents, pendingSessionEvent{
				Type:    sessions.EventError,
				Payload: map[string]any{"message": err.Error()},
			})
			return agentResponseMsg{runID: runID, rows: rows, usageEvents: usageEvents, usageModelID: usageModelID, usageModelIDs: usageModelIDs, sessionEvents: sessionEvents, err: err, goalAware: goalAwareRun, turnTools: toolCalls, turnElapsed: m.activeTurnElapsed(started)}
		}
		if runOptions.specDraft {
			if result.StopReason != agent.StopReasonSpecReviewRequired || specReview == nil || specReview.SpecID == "" || specReview.SpecFilePath == "" {
				err := fmt.Errorf("spec draft ended without submit_spec")
				flushReasoning(m.now())
				sessionEvents = append(sessionEvents, pendingSessionEvent{
					Type:    sessions.EventError,
					Payload: map[string]any{"message": err.Error()},
				})
				return agentResponseMsg{runID: runID, rows: rows, usageEvents: usageEvents, usageModelID: usageModelID, usageModelIDs: usageModelIDs, sessionEvents: sessionEvents, err: err, goalAware: goalAwareRun, turnTools: toolCalls, turnElapsed: m.activeTurnElapsed(started)}
			}
			flushReasoning(m.now())
			return agentResponseMsg{runID: runID, rows: rows, usageEvents: usageEvents, usageModelID: usageModelID, usageModelIDs: usageModelIDs, sessionEvents: sessionEvents, specReview: specReview, goalAware: goalAwareRun, turnTools: toolCalls, turnElapsed: m.activeTurnElapsed(started)}
		}
		flushReasoning(m.now())
		elapsed := m.activeTurnElapsed(started)
		rows = append(rows, transcriptRow{
			kind:        rowAssistant,
			text:        result.FinalAnswer,
			final:       true,
			turnTools:   toolCalls,
			turnElapsed: elapsed,
		})
		if notice := result.TruncationNotice(); notice != "" {
			rows = append(rows, transcriptRow{kind: rowSystem, text: notice})
		}
		sessionEvents = append(sessionEvents, pendingSessionEvent{
			Type: sessions.EventMessage,
			Payload: map[string]any{
				"role":    "assistant",
				"content": result.FinalAnswer,
			},
		})
		return agentResponseMsg{runID: runID, rows: rows, usageEvents: usageEvents, usageModelID: usageModelID, usageModelIDs: usageModelIDs, sessionEvents: sessionEvents, goalAware: goalAwareRun, turnTools: toolCalls, turnElapsed: elapsed, ttft: firstTokenElapsed}
	}
}

func (m model) sendPermissionRequest(runID int, request agent.PermissionRequest, decide func(agent.PermissionDecision)) {
	if m.runtimeMessageSink == nil {
		return
	}
	m.runtimeMessageSink(permissionRequestMsg{runID: runID, request: request, decide: decide})
}

// autoResolvedPermissionDecision resolves a permission request the TUI cannot
// turn into a user prompt (Action != prompt). The agent is blocked awaiting a
// decision, so one must ALWAYS be produced. Only an explicit Cancel is honored
// as such; every other non-prompt action — including allow — is DENIED, so the
// UI never silently grants access it did not surface for approval.
func autoResolvedPermissionDecision(action agent.PermissionAction) agent.PermissionDecision {
	if action == agent.PermissionActionCancel {
		return agent.PermissionDecision{Action: agent.PermissionDecisionCancel, Reason: "run cancelled"}
	}
	return agent.PermissionDecision{
		Action: agent.PermissionDecisionDeny,
		Reason: "permission request could not be surfaced for approval",
	}
}

func (m model) sendAskUserRequest(runID int, request agent.AskUserRequest, answer func([]string)) {
	if m.runtimeMessageSink == nil {
		return
	}
	m.runtimeMessageSink(askUserRequestMsg{runID: runID, request: request, answer: answer})
}

func tuiPermissionEventType(event agent.PermissionEvent) sessions.EventType {
	if event.Action == agent.PermissionActionPrompt {
		return sessions.EventPermissionRequest
	}
	if event.Action == agent.PermissionActionAllow || event.Action == agent.PermissionActionDeny || event.Action == agent.PermissionActionCancel {
		return sessions.EventPermissionDecision
	}
	return sessions.EventPermission
}

func (m model) sendAgentRow(runID int, row transcriptRow) {
	if m.runtimeMessageSink == nil {
		return
	}
	m.runtimeMessageSink(agentRowMsg{runID: runID, row: row})
}

func (m model) sendAgentText(runID int, delta string) {
	if m.runtimeMessageSink == nil {
		return
	}
	m.runtimeMessageSink(agentTextMsg{runID: runID, delta: delta})
}

// streamingTextString returns the accumulated live assistant text. streamingText
// is stored as []byte for O(1) amortized appends; the conversion here is bounded
// by the segment length, the same cost the renderer already pays.
func (m model) streamingTextString() string {
	return string(m.streamingText)
}

func (m model) sendToolCallStreamStart(runID int, id, name string) {
	if m.runtimeMessageSink == nil {
		return
	}
	m.runtimeMessageSink(toolCallStreamStartMsg{runID: runID, id: id, name: name})
}

func (m model) sendToolCallStreamDelta(runID int, id, fragment string) {
	if m.runtimeMessageSink == nil {
		return
	}
	m.runtimeMessageSink(toolCallStreamDeltaMsg{runID: runID, id: id, fragment: fragment})
}

// clearStreamingToolCall drops the in-progress live "writing" block (id + name +
// accumulated args). Called whenever the streamed tool call is no longer the
// active live preview: it finalizes into a card, text resumes, the run ends, or
// the run is cancelled. Releasing the args buffer also caps memory after a write.
func (m *model) clearStreamingToolCall() {
	m.streamCallID = ""
	m.streamCallName = ""
	m.streamCallDecoder = nil
}

func (m model) sendAgentReasoning(runID int, delta string) {
	if m.runtimeMessageSink == nil {
		return
	}
	m.runtimeMessageSink(agentReasoningMsg{runID: runID, delta: delta})
}

func (m model) sendAgentUsage(runID int, modelID string, event zeroruntime.Usage) {
	if m.runtimeMessageSink == nil {
		return
	}
	m.runtimeMessageSink(agentUsageMsg{runID: runID, modelID: modelID, usage: event})
}

// toolResultDetail is the card body source: the rich card-only Display.Preview
// (a code/diff preview) when present on a successful result, else the Output that
// the model also saw. Error results keep their Output so the failure shows.
func toolResultDetail(result agent.ToolResult) string {
	display := result.HumanDisplay()
	if strings.TrimSpace(display.Preview) != "" && (result.Status != tools.StatusError || result.Outcome.Finalized()) {
		return display.Preview
	}
	return result.ModelOutput()
}

// toolResultSessionPayload preserves both views of a tool result: output remains
// the provider-facing text used for session context, while displayPreview keeps
// the richer card body that was visible during the live run. The preview is only
// stored when it differs, so ordinary tool results retain their compact event.
func toolResultSessionPayload(result agent.ToolResult) map[string]any {
	output := result.ModelOutput()
	payload := map[string]any{
		"toolCallId": result.ToolCallID,
		"name":       result.Name,
		"status":     string(result.Status),
		"output":     output,
	}
	if preview := toolResultDetail(result); strings.TrimSpace(preview) != "" && preview != output {
		payload["displayPreview"] = preview
	}
	if result.Redacted {
		payload["redacted"] = true
	}
	if len(result.Meta) > 0 {
		payload["meta"] = result.Meta
	}
	if len(result.ChangedFiles) > 0 {
		payload["changedFiles"] = result.ChangedFiles
	}
	if len(result.ChangeSummaries) > 0 {
		payload["changeSummaries"] = result.ChangeSummaries
	}
	return payload
}

func toolResultRowText(result agent.ToolResult) string {
	status := result.Status
	if status == "" {
		status = tools.StatusOK
	}
	return fmt.Sprintf("tool result: %s %s %s", result.Name, status, truncateTUIOutput(result.ModelOutput(), tuiToolOutputLimit))
}
