package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/errhint"
	"github.com/Gitlawb/zero/internal/execprofile"
	"github.com/Gitlawb/zero/internal/execution"
	"github.com/Gitlawb/zero/internal/imageinput"
	"github.com/Gitlawb/zero/internal/lsp"
	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/notify"
	"github.com/Gitlawb/zero/internal/providercatalog"
	"github.com/Gitlawb/zero/internal/providermodeldiscovery"
	"github.com/Gitlawb/zero/internal/providers"
	"github.com/Gitlawb/zero/internal/sandbox"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/specmode"
	"github.com/Gitlawb/zero/internal/streamjson"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/trace"
	"github.com/Gitlawb/zero/internal/usage"
	"github.com/Gitlawb/zero/internal/worktrees"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

const (
	exitSuccess  = 0
	exitCrash    = 1
	exitUsage    = 2
	exitProvider = 3
	// exitIncomplete marks a headless run that stopped with work clearly
	// unfinished — the completion gate exhausted its continue budget on a
	// no-tool-call turn while plan items were pending or the model ended mid-step.
	// Distinct from success so callers/benchmarks don't treat an abandoned run as
	// finished.
	exitIncomplete = 4
	// exitInterrupted is the conventional shell exit code for a process stopped by
	// SIGINT (128 + signal number 2).
	exitInterrupted = 130
)

// defaultExecMaxTurns is the headless tool-turn budget when no source sets one.
// An interactive run can be continued past the shared interactive default by the
// user; an exec run has nobody to unstick it, so evals and automation would
// truncate mid-task at 80 turns. The ceiling is the already-justified bound —
// still finite against a genuinely runaway loop, which the loop's empty-turn
// guard also cuts independently of this budget.
const defaultExecMaxTurns = config.MaxTurnsCeiling

type execOutputFormat string
type execInputFormat string

const (
	execOutputText       execOutputFormat = "text"
	execOutputJSON       execOutputFormat = "json"
	execOutputStreamJSON execOutputFormat = "stream-json"
	execInputText        execInputFormat  = "text"
	execInputStreamJSON  execInputFormat  = "stream-json"
)

type execOptions struct {
	promptParts []string
	file        string
	imagePaths  []string
	mode        string
	model       string
	// modelProfile captures the legacy --profile flag. It is accepted for
	// backward compatibility (so old invocations do not error) but is
	// intentionally inert: nothing consumes it. Model selection is driven by
	// --model / --mode instead. See writeExecHelp ("Accept legacy model profile
	// selection") and TestRunExecAcceptsLegacyModelProfileFlags.
	modelProfile string
	// execProfile selects a named execution profile (balanced, fast, thorough):
	// a loop-posture bundle (turn budget, reasoning effort, self-correction,
	// escalation triggers) applied AFTER --mode with the same
	// fill-only-if-unset rule, so precedence is explicit flag > mode > profile.
	// Distinct from the mode preset also named "fast" (which picks a model) and
	// from the legacy inert --profile above. See internal/execprofile.
	execProfile         string
	reasoningEffort     string
	useSpec             bool
	specModel           string
	specReasoningEffort string
	// plan selects PermissionModePlan for this run: the same read-only,
	// in-session planning mode the TUI enters with /plan on. Unlike --use-spec
	// (a separate draft session with its own review flow), this only swaps the
	// permission mode for the run already being made — ToolAdvertised gates
	// write/shell tools generically for any mode, so no other exec plumbing
	// needs to know about it.
	plan                  bool
	maxTurns              int
	cwd                   string
	inputFormat           execInputFormat
	outputFormat          execOutputFormat
	autonomy              string
	enabledTools          []string
	disabledTools         []string
	listTools             bool
	resume                string
	resumeLatest          bool
	fork                  string
	callingSessionID      string
	callingToolUseID      string
	tag                   string
	depth                 int
	sessionTitle          string
	initSessionID         string
	worktree              bool
	worktreeName          string
	worktreeDir           string
	skipPermissionsUnsafe bool
	permissionMode        string
	// allowEscalation opts the run into mid-run model escalation: it registers
	// the escalate_model tool and wires agent.Options.ModelSwitcher. Off by
	// default — a run without the flag is byte-identical to before (no tool, nil
	// switcher).
	allowEscalation bool
	// selfCorrect opts the run into the post-edit verify-and-correct loop: after a
	// mutating tool call ZERO runs the workspace verification plan and feeds
	// failures back to the model to fix, bounded by an attempt ceiling and the
	// autonomy gate. Off by default — a run without the flag wires a nil
	// SelfCorrector, leaving the agent loop byte-identical to before.
	selfCorrect bool
	// notifyMode overrides config.Notify.Mode for this run. Mutually exclusive
	// with noNotify.
	notifyMode string
	// noNotify forces ModeOff for this run. Mutually exclusive with notifyMode.
	noNotify bool
	// noCompletionGate turns off the headless completion gate
	// (agent.Options.RequireCompletionSignal) for this run. It exists for
	// CONVERSATIONAL exec callers — a chat frontend driving exec with an operator
	// present to reply (e.g. zeroclaw chat). There, a final message that hands
	// the turn back ("the sandbox blocks network egress, approve it and I'll
	// continue") is a COMPLETE answer, not an unfinished task, so the gate's
	// self-report admission check and INCOMPLETE downgrade (exit 4) would only
	// mislabel honest blocker reports. Default off: plain `zero exec` keeps the
	// gate, preserving CI/cron semantics.
	noCompletionGate bool
	// addDirs holds directories passed via --add-dir that should be allowed as
	// additional write roots for this run. Unioned with
	// config.SandboxConfig.AdditionalWriteRoots at scope construction time.
	addDirs []string
	// tracePath, when set, writes a per-turn NDJSON trace (agenteval-compatible)
	// to the given file path — or to stderr when the value is "-". Falls back to
	// the ZERO_TRACE env var when the flag is absent. Off by default: a run
	// without it leaves agent.Options.Trace nil and is byte-identical to before.
	// The trace is pure observation — enabling it does not change agent behavior.
	tracePath string
}

type execUsageError struct {
	message string
}

func (err execUsageError) Error() string {
	return err.message
}

func runExec(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, help, err := parseExecArgs(args)
	if err != nil {
		return writeExecFormatUsageError(stdout, stderr, options.outputFormat, err.Error())
	}
	if help {
		if err := writeExecHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}

	// Refresh the models.dev pricing/limits cache in the background when stale;
	// the overlay is read at registry construction from the cache file, so this
	// benefits the next run and never blocks or fails this one.
	modelregistry.StartModelsDevRefresh()

	// A mode seeds model/effort/max-turns/tool filters as a preset. Expand it up
	// front — before tool-filter validation and the --list-tools branch — so a
	// mode-injected tool filter is validated and reflected in --list-tools, and a
	// mode-supplied model flows through the same resolution (and deprecation
	// notice) path as an explicit --model. Explicit flags still win: applyExecMode
	// only fills fields the caller left unset.
	if err := applyExecMode(&options); err != nil {
		return writeExecFormatUsageError(stdout, stderr, options.outputFormat, err.Error())
	}
	// A profile tunes loop posture the way a mode tunes model selection, and
	// runs after it so the mode's fills count as "set" and win. MaxTurns is
	// deferred to config resolution below, where the displaced resolved budget
	// is known and becomes the escalation restore target.
	execProfile, execProfileFilledEffort, err := applyExecProfile(&options)
	if err != nil {
		return writeExecFormatUsageError(stdout, stderr, options.outputFormat, err.Error())
	}

	workspaceRoot, err := resolveWorkspaceRoot(options.cwd, deps)
	if err != nil {
		return writeExecFormatUsageError(stdout, stderr, options.outputFormat, err.Error())
	}
	// trustRoot is the ORIGINAL launch directory, captured before any --worktree
	// reassignment below, so a worktree of a trusted repo inherits that repo's
	// trust instead of being seen as a fresh untrusted path.
	trustRoot := workspaceRoot
	if options.worktree {
		preparedWorktree, err := deps.prepareWorktree(context.Background(), worktrees.Options{
			Cwd:     workspaceRoot,
			Name:    options.worktreeName,
			BaseDir: options.worktreeDir,
			Now:     deps.now,
			// The worktree's lifetime is bound to this process (the deferred
			// release below), so record the PID: if this process dies without
			// releasing, Clean can expire the lease instead of skipping the
			// locked worktree forever.
			LeasePID: os.Getpid(),
		})
		if err != nil {
			return writeExecFormatUsageError(stdout, stderr, options.outputFormat, err.Error())
		}
		workspaceRoot = preparedWorktree.Path
		// When this run's own Prepare call took the worktree lock, its
		// lifetime is bound to this function: release the lock once it returns
		// so Clean can reclaim the worktree later if it goes stale. A reused
		// worktree whose lock an external `zero worktrees prepare` caller
		// still holds reports LockAcquired=false; releasing it here would
		// clear that caller's lease and expose its workspace to Clean, so the
		// matching release stays that caller's responsibility. A failed unlock
		// leaves a lock Clean will permanently skip, so it must not pass
		// silently; the run's primary result has already been emitted by the
		// time the defer runs, so surface it as a diagnostic.
		if preparedWorktree.LockAcquired {
			defer func() {
				if releaseErr := deps.releaseWorktree(context.Background(), worktrees.Options{Cwd: trustRoot}, preparedWorktree.Path); releaseErr != nil {
					fmt.Fprintf(stderr, "zero: failed to release worktree lock on %s: %s\n", redactCLIString(preparedWorktree.Path), redactCLIString(releaseErr.Error()))
				}
			}()
		}
	}

	registry := newCoreRegistry(workspaceRoot)
	executionRunner := execution.NewRunner(nil)
	// Register the escalate_model tool only when the run opted into mid-run
	// escalation. It is registered before --list-tools formatting and
	// validateExecToolFilters so the tool is both listable and filter-validatable.
	// The no-arg constructor builds its own default registry internally (and
	// degrades to an inert tool if the catalog fails), so no registry is threaded.
	if options.allowEscalation {
		registry.Register(tools.NewEscalateModelTool())
	}
	var specialistRuntime *agentToolRuntime
	if shouldRegisterExecSpecialistTools(options) {
		// Specialist tools register before the full config resolve below (so
		// --list-tools stays offline). swarm.maxTeamSize is not affected by
		// overrides, so an empty-overrides resolve yields the same value; a resolve
		// error falls back to the swarm's built-in default (0 => 8).
		maxTeamSize := 0
		if swarmCfg, cfgErr := deps.resolveConfig(workspaceRoot, config.Overrides{}); cfgErr == nil {
			maxTeamSize = swarmCfg.Swarm.MaxTeamSize
		}
		var err error
		specialistRuntime, err = registerSpecialistTools(registry, workspaceRoot, maxTeamSize)
		if err != nil {
			return writeExecProviderError(stdout, stderr, options.outputFormat, "specialist_error", err.Error())
		}
		defer closeSpecialistRuntime(stderr, specialistRuntime)
	}
	permissionMode, err := resolveExecPermissionMode(options)
	if err != nil {
		return writeExecFormatUsageError(stdout, stderr, options.outputFormat, err.Error())
	}
	if options.useSpec {
		permissionMode = agent.PermissionModeSpecDraft
	}
	if options.plan {
		permissionMode = agent.PermissionModePlan
	}
	var mcpRuntime mcpToolRuntime
	var mcpSkip trustSkip
	// Make local plugins live for this run: register their declared tools into the
	// registry and collect their hooks + skill roots for the dispatcher and skill
	// tool below. Done before --list-tools and filter validation so plugin tools
	// are listable and filter-validatable; it fails OPEN (a bad plugin is skipped).
	var pluginActivation pluginActivation
	if options.useSpec {
		specmode.RegisterDraftTools(registry, workspaceRoot, deps.now)
	}
	// Build the model registry once and reuse it across every model-aware
	// lookup in this exec run (model resolution, reasoning-effort advisory, and
	// context-window sizing). DefaultRegistry builds the full catalog and
	// compiles its match patterns, so rebuilding it per lookup is wasteful.
	// modelRegistry is the zero Registry when the catalog fails to build; the
	// helpers below degrade to safe no-op behavior in that case.
	modelRegistry, _ := modelregistry.DefaultRegistry()

	overrides := config.Overrides{}
	modelOverride := options.model
	if options.useSpec && options.specModel != "" {
		modelOverride = options.specModel
	}
	if modelOverride != "" {
		resolvedModel, notice := resolveSelectedModel(modelRegistry, modelOverride)
		overrides.Provider.Model = resolvedModel
		if notice != "" {
			if _, err := fmt.Fprintln(stderr, notice); err != nil {
				return exitCrash
			}
		}
	}
	if options.maxTurns > 0 {
		overrides.MaxTurns = options.maxTurns
	}
	resolved, err := deps.resolveConfig(workspaceRoot, overrides)
	if err != nil {
		if !options.listTools {
			if preflightErr := preflightExecSession(options); preflightErr != nil {
				return writeExecFormatUsageError(stdout, stderr, options.outputFormat, preflightErr.Error())
			}
			if _, _, promptErr := resolveExecPrompt(options, workspaceRoot, deps.stdin); promptErr != nil {
				return writeExecFormatUsageError(stdout, stderr, options.outputFormat, promptErr.Error())
			}
		}
		return writeExecProviderError(stdout, stderr, options.outputFormat, "provider_error", err.Error())
	}
	resolved.MaxTurns = execTurnBudget(resolved)
	var displacedMaxTurns int
	resolved.MaxTurns, displacedMaxTurns = applyProfileTurnBudget(execProfile, options.maxTurns, resolved.MaxTurns)
	execScope, err := sandbox.NewScope(workspaceRoot, append(append([]string{}, resolved.Sandbox.AdditionalWriteRoots...), options.addDirs...))
	if err != nil {
		return writeExecProviderError(stdout, stderr, options.outputFormat, "sandbox_error", err.Error())
	}
	for _, tool := range tools.CoreToolsScoped(workspaceRoot, execScope) {
		registry.Register(tool)
	}
	sandboxEngine, err := buildExecSandboxEngine(workspaceRoot, resolved, deps, execScope)
	if err != nil {
		return writeExecProviderError(stdout, stderr, options.outputFormat, "sandbox_error", err.Error())
	}
	if notice, err := sandboxEngine.ConsumeGrantMigrationNotice(); err != nil {
		return writeExecProviderError(stdout, stderr, options.outputFormat, "sandbox_error", "migrate sandbox grants: "+err.Error())
	} else if notice != "" {
		_, _ = fmt.Fprintln(stderr, "[zero] "+notice)
	}
	// Before any tool runs, and before the MCP servers and plugins below start
	// through the same sandbox, so a run that goes ahead with reduced isolation
	// says so where its output starts, not only when someone runs `zero doctor`.
	// A notice that cannot be written stops the run, as the image and effort
	// notices further down do: going ahead would mean reduced isolation and no
	// warning delivered.
	if notice := sandboxEngine.DegradedNotice(); notice != "" {
		if _, err := fmt.Fprintln(stderr, "[zero] "+notice); err != nil {
			return exitCrash
		}
	}
	executionRunner.SetPreparer(sandboxEngine)
	if permissionMode != agent.PermissionModePlan {
		mcpRuntime, mcpSkip, err = registerMCPToolsForWorkspace(context.Background(), workspaceRoot, registry, deps, execMCPAutonomy(options), trustRoot, executionRunner)
		if err != nil {
			return writeExecProviderError(stdout, stderr, options.outputFormat, "mcp_error", err.Error())
		}
		defer closeMCPRuntime(stderr, mcpRuntime)
	}
	pluginActivation = activatePlugins(workspaceRoot, registry, deps, stderr, trustRoot, executionRunner)
	registerLocalControlTools(registry, workspaceRoot, resolved.LocalControl)
	if err := validateExecToolFilters(options, registry); err != nil {
		return writeExecFormatUsageError(stdout, stderr, options.outputFormat, err.Error())
	}
	if options.listTools {
		if options.outputFormat == execOutputStreamJSON {
			return writeExecStreamJSONFinal(stdout, workspaceRoot, execRunMetadata{}, permissionMode, formatExecToolList(registry, options, permissionMode), exitSuccess)
		}
		if options.outputFormat == execOutputJSON {
			if err := writeExecToolListJSON(stdout, registry, options, permissionMode); err != nil {
				return exitCrash
			}
			return exitSuccess
		}
		if err := writeExecToolList(stdout, registry, options, permissionMode); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if err := preflightExecSession(options); err != nil {
		return writeExecFormatUsageError(stdout, stderr, options.outputFormat, err.Error())
	}

	prompt, streamImages, err := resolveExecPrompt(options, workspaceRoot, deps.stdin)
	if err != nil {
		return writeExecFormatUsageError(stdout, stderr, options.outputFormat, err.Error())
	}
	sessionTitle := execSessionTitle(options, prompt)

	if !config.HasProviderProfile(resolved.Provider) {
		return writeExecProviderError(stdout, stderr, options.outputFormat, "provider_error", "No provider configured. Run `zero setup` (guided), `zero auth` (OAuth providers), set a provider API key env var (e.g. OPENAI_API_KEY / ANTHROPIC_API_KEY / GEMINI_API_KEY), or add .zero/config.json.")
	}
	// Activate deferred MCP-tool loading for this run only when the VISIBLE
	// deferred-eligible count meets the resolved threshold; below threshold this
	// is a no-op and tool advertising stays byte-identical. The registry is
	// already complete (core + MCP) at this point, so the count is accurate. The
	// permission mode and operator tool filters MUST match the values passed to
	// agent.Run below so this registration gate counts the same population the
	// loop's partition gate counts.
	// tool_search is the only way to reach a hidden deferred tool, so if the
	// operator explicitly disables it, deferral must not activate at all —
	// otherwise a positive threshold would hide tools behind a loader the run
	// rejects (a dead-end). Force the effective threshold to 0 so this
	// registration gate and agent.Run's partition gate agree the run is inactive.
	effectiveDeferThreshold := resolved.Tools.DeferThreshold
	if toolListContains(options.disabledTools, tools.ToolSearchToolName) {
		effectiveDeferThreshold = 0
	}
	registerToolSearchIfEligible(registry, effectiveDeferThreshold, permissionMode, options.enabledTools, options.disabledTools)
	images, err := resolveExecImages(options.imagePaths, workspaceRoot)
	if err != nil {
		return writeExecFormatUsageError(stdout, stderr, options.outputFormat, err.Error())
	}
	// Merge stream-json message images with --image attachments so BOTH input
	// sources flow through the same vision gate (drop+warn) and the same
	// agent.Options.Images wiring below. Without this merge, images sent over
	// stream-json are parsed and validated but never reach the agent.
	images = append(images, streamImages...)
	// Gate against the EFFECTIVE resolved model (not the --model override). An
	// unknown/custom id can't be confirmed vision-capable, so drop+warn rather
	// than error: image input is best-effort, never fatal to the run.
	if len(images) > 0 && !modelregistry.SupportsVision(modelRegistry, resolved.Provider.Model) {
		if _, err := fmt.Fprintf(stderr, "Model %s does not support image input; ignoring %d image(s).\n", resolved.Provider.Model, len(images)); err != nil {
			return exitCrash
		}
		images = nil
	}
	runReasoningEffort := options.reasoningEffort
	if options.useSpec && options.specReasoningEffort != "" {
		runReasoningEffort = options.specReasoningEffort
	}
	// Evaluate the --reasoning-effort advisory against the EFFECTIVE resolved
	// model (resolved.Provider.Model), not the override. Without an explicit
	// --model the override model is empty, so checking it here would silently
	// skip the advisory even though the run uses a concrete effective model.
	if runReasoningEffort != "" {
		if notice := reasoningEffortNotice(modelRegistry, resolved.Provider.Model, runReasoningEffort); notice != "" {
			if _, err := fmt.Fprintln(stderr, notice); err != nil {
				return exitCrash
			}
		}
	}
	// Effort to forward on the provider request, gated to the resolved model's
	// supported levels (empty for non-reasoning models).
	forwardEffort := forwardedReasoningEffort(modelRegistry, resolved.Provider.Model, runReasoningEffort)

	provider, err := buildProvider(resolved, deps)
	if err != nil {
		return writeExecProviderError(stdout, stderr, options.outputFormat, "provider_error", err.Error())
	}

	// currentModel tracks the model in force for usage attribution. It starts at
	// the resolved model and is reassigned by the model switcher on a mid-run
	// escalation so post-switch turns are attributed to the escalated model.
	currentModel := resolved.Provider.Model
	// Optimized OpenAI turn sessions (ZERO_OPENAI_TURN_SESSION, default off). nil
	// when gated off or ineligible, unless a configured RPM limiter needs to
	// wrap the default adapter. The limiter is shared across model switches.
	sessionOptions := providers.Options{ModelRPM: zeroruntime.NewModelRPMLimiter(resolved.ModelRPM)}
	turnSessions, _ := providers.ConfiguredTurnSessions(resolved.Provider, provider, sessionOptions)
	// Both switchers come from one shared builder so exec and the interactive TUI
	// cannot drift on the nil contracts the agent loop depends on. The session
	// switcher preserves the starting transport and shares the RPM limiter.
	var modelSwitcher func(context.Context, string) (agent.Provider, error)
	var modelSessionSwitcher func(context.Context, string) (zeroruntime.TurnSessionProvider, error)
	if options.allowEscalation {
		modelSwitcher, modelSessionSwitcher = providers.EscalationSwitchers(
			resolved.Provider, provider, deps.newProvider,
			func(modelID string) { currentModel = modelID },
			sessionOptions,
		)
	}

	runMetadata, err := resolveExecRunMetadata(resolved.Provider)
	if err != nil {
		return writeExecProviderError(stdout, stderr, options.outputFormat, "provider_error", err.Error())
	}
	// Notify on completion via stderr (never stdout, which may carry stream-json).
	// Headless has no terminal-focus signal, so focus gating does not apply here:
	// always emit when a mode is configured (focusMode is a TUI-only concept).
	notifier := notify.New(stderr, notify.Config{
		Mode:      notify.Mode(strings.TrimSpace(execNotifyMode(options, resolved))),
		FocusMode: notify.FocusAlways,
	})
	// Opt-in webhook fan-out (ZERO_NOTIFY_WEBHOOK_URL). Headless runs can safely
	// log a failed delivery to stderr (never stdout). The sink redacts before
	// logging, so a token in the URL or message is masked.
	notify.MaybeAddWebhookSink(notifier, os.Getenv, func(format string, args ...any) {
		fmt.Fprintf(stderr, "[notify] "+format+"\n", args...)
	})
	// Spec-draft runs synthesize a prompt offline and never drive a model turn, so
	// there is no per-turn trace to emit. Reject --trace / ZERO_TRACE up front with
	// a clear error rather than silently accepting and writing nothing.
	if options.useSpec && resolveTracePath(options) != "" {
		return writeExecFormatUsageError(stdout, stderr, options.outputFormat, "--trace / ZERO_TRACE are not supported for spec-draft runs")
	}
	if options.useSpec {
		return runExecSpecDraft(execSpecDraftRun{
			options:            options,
			stdout:             stdout,
			stderr:             stderr,
			deps:               deps,
			workspaceRoot:      workspaceRoot,
			trustRoot:          trustRoot,
			mcpSkip:            mcpSkip,
			registry:           registry,
			modelRegistry:      modelRegistry,
			resolved:           resolved,
			runMetadata:        runMetadata,
			provider:           provider,
			sandboxEngine:      sandboxEngine,
			prompt:             prompt,
			sessionTitle:       sessionTitle,
			images:             images,
			reasoningEffort:    forwardEffort,
			specPermissionMode: permissionMode,
			notifier:           notifier,
			// The profile displaced resolved.MaxTurns above, so the spec-draft
			// run arms the same escalation policy as the main run (in practice
			// only the failure-streak and risky-mutation triggers can fire
			// here: the draft runs without the completion gate or a
			// self-corrector).
			profilePolicy: execProfile.Policy(displacedMaxTurns,
				specProfileEffortFilled(execProfileFilledEffort, options.specReasoningEffort)),
		})
	}

	preparedSession := sessions.PreparedExec{}
	agentPrompt := prompt
	if shouldUseExecSession(options) {
		preparedSession, err = sessions.PrepareExec(sessions.PrepareExecOptions{
			SessionID:        options.initSessionID,
			Title:            sessionTitle,
			Cwd:              workspaceRoot,
			ModelID:          resolved.Provider.Model,
			Provider:         runMetadata.Provider,
			Tag:              options.tag,
			Depth:            options.depth,
			CallingSessionID: options.callingSessionID,
			CallingToolUseID: options.callingToolUseID,
			AgentName:        specialistAgentName(options.sessionTitle),
			TaskID:           options.initSessionID,
			Resume:           options.resume,
			ResumeLatest:     options.resumeLatest,
			Fork:             options.fork,
		})
		if err != nil {
			return writeExecFormatUsageError(stdout, stderr, options.outputFormat, err.Error())
		}
		agentPrompt = sessions.FormatExecPrompt(prompt, preparedSession)
	}
	runID, err := streamjson.CreateRunID(time.Now())
	if err != nil {
		return writeAppError(stderr, "failed to create run id: "+err.Error(), exitCrash)
	}
	// Per-turn tracing is opt-in (--trace <path> or ZERO_TRACE=<path>). The
	// recorder is stamped throughout agent.Run and the providerio seam; the run
	// itself is byte-identical to an untraced run. We finish the recorder at the
	// agent.Run boundary (below) so the snapshot captures exactly one turn, then
	// serialize the snapshot on every exit path via the defer. "-" writes NDJSON
	// to stderr; otherwise the path is created/truncated. A failure to emit is
	// logged to stderr, never fatal.
	tracePath := resolveTracePath(options)
	var traceRecorder *trace.Recorder
	var traceSnapshot *trace.TurnTrace
	if tracePath != "" {
		traceRecorder = trace.NewRecorder(preparedSession.Session.SessionID, runID, execProfile.Name)
		defer func() {
			if err := writeTraceSnapshot(traceSnapshot, tracePath, stderr); err != nil {
				fmt.Fprintf(stderr, "[zero] failed to write trace: %s\n", err)
			}
		}()
	}
	writer := execEventWriter{
		stdout:       stdout,
		stderr:       stderr,
		format:       options.outputFormat,
		runID:        runID,
		sessionID:    preparedSession.Session.SessionID,
		streamedText: &strings.Builder{},
	}
	writer.runStart(workspaceRoot, runMetadata, permissionMode)
	if writer.err != nil {
		return exitCrash
	}
	// Surface the unsafe-permissions warning whenever the run resolves to unsafe
	// mode, covering BOTH --skip-permissions-unsafe and --auto high (which also
	// resolves to PermissionModeUnsafe). Previously only the explicit flag path
	// warned, so --auto high silently ran without notice.
	if permissionMode == agent.PermissionModeUnsafe {
		reason := "--auto high"
		switch {
		case options.skipPermissionsUnsafe:
			reason = "--skip-permissions-unsafe"
		case strings.EqualFold(strings.TrimSpace(options.permissionMode), "unsafe"),
			strings.EqualFold(strings.TrimSpace(options.permissionMode), "high"):
			reason = "--permission-mode unsafe"
		}
		writer.warning(fmt.Sprintf("Unsafe permissions are active for this run because %s was passed.", reason))
		if writer.err != nil {
			return exitCrash
		}
	}

	sessionRecorder := execSessionRecorder{prepared: preparedSession}
	// Surface a best-effort session-recording failure once, on every exit path.
	defer sessionRecorder.warnIfRecordingFailed(stderr)
	sessionRecorder.append(sessions.EventMessage, map[string]any{
		"role":    "user",
		"content": prompt,
	})

	// OnAskUser is intentionally left unset: headless runs have no interactive
	// user, so ask_user degrades to a "proceed with your best assumption" result
	// rather than blocking. (Future enhancement: collect answers over stream-json
	// input when a controlling client is attached.)
	// Cancel the agent run on Ctrl+C / SIGTERM so a long headless run shuts down
	// cleanly (the loop honors context cancellation) instead of being killed.
	runCtx, stopSignals := signalContext()
	defer stopSignals()
	// --self-correct opts the run into the post-edit verify-and-correct loop. Off
	// by default the corrector is nil, leaving the agent loop byte-identical. When
	// on we verify with both the workspace test plan and LSP diagnostics over the
	// changed files; the autonomy gate inside the corrector still decides whether
	// failures auto-fix or just report. fileDiagnostics (always on, lazy) gives
	// edit_file/write_file inline error diagnostics for the file they just wrote.
	// lspShutdown tears down any language-server sessions either half spawned.
	selfCorrector, fileDiagnostics, lspShutdown := newExecSelfCorrector(options.selfCorrect, workspaceRoot, options.autonomy)
	defer lspShutdown()
	// Kept as a named variable (rather than inlined into Options below) because
	// the write tools also use it for conflict checks across a run.
	fileTracker := tools.NewFileTracker()
	scratchBaseline := scratchFileSnapshot(workspaceRoot)
	emitScratchWarning := func() {
		if notice := scratchFileWarning(workspaceRoot, scratchBaseline); notice != "" {
			writer.warning(notice)
		}
	}
	// Build the hooks dispatcher out of the struct literal so its trust skip report
	// can be combined with the plugin activation's, and emit at most one notice when
	// project hooks/plugins were dropped for an untrusted workspace.
	hookDispatcher, hookSkip := newHookDispatcherWithExtra(workspaceRoot, pluginActivation.hooks, trustRoot, executionRunner)
	emitTrustNotice(stderr, hookSkip, pluginActivation.trustSkip, mcpSkip)
	result, err := agent.Run(runCtx, agentPrompt, provider, agent.Options{
		MaxTurns:             resolved.MaxTurns,
		ContextWindow:        resolveAgentContextWindow(runCtx, modelRegistry, resolved.Provider),
		DeferThreshold:       effectiveDeferThreshold,
		Specialists:          specialistRuntime.specialistInfos(),
		Skills:               pluginActivation.skillInfos(deps.skillsDir()),
		SessionID:            preparedSession.Session.SessionID,
		CallingSessionID:     options.callingSessionID,
		CallingToolUseID:     options.callingToolUseID,
		Tag:                  options.tag,
		Depth:                options.depth,
		SessionTitle:         sessionTitle,
		ProviderName:         resolved.Provider.Name,
		Model:                resolved.Provider.Model,
		ModelSwitcher:        modelSwitcher,
		TurnSessionProvider:  turnSessions,
		ModelSessionSwitcher: modelSessionSwitcher,
		Summarizer:           summarizerFactory(resolved, deps.newProvider),
		ContextWindowFor: func(modelID string) int {
			return modelregistry.AgentContextWindow(modelContextWindow(modelRegistry, modelID))
		},
		SupportsVision: func(modelID string) bool {
			return modelregistry.SupportsVision(modelRegistry, modelID)
		},
		ReasoningEffort: forwardEffort,
		Trace:           traceRecorder,
		Cwd:             workspaceRoot,
		Images:          images,
		Registry:        registry,
		PermissionMode:  permissionMode,
		Autonomy:        options.autonomy,
		SelfCorrect:     selfCorrector,
		FileDiagnostics: fileDiagnostics,
		Profile:         execProfile.Policy(displacedMaxTurns, execProfileFilledEffort),
		// Headless exec: don't accept a no-tool-call turn as "done" while work
		// clearly remains (pending plan items / a mid-step continuation cue) —
		// nudge to continue, and finalize as INCOMPLETE rather than false success
		// if the model keeps stalling. The interactive TUI leaves this off, and
		// --no-completion-gate lets conversational exec callers (a chat frontend
		// with an operator present) opt out the same way.
		RequireCompletionSignal: !options.noCompletionGate,
		Sandbox:                 sandboxEngine,
		FileTracker:             fileTracker,
		Hooks:                   hookDispatcher,
		EnabledTools:            options.enabledTools,
		DisabledTools:           options.disabledTools,
		OnText:                  writer.text,
		OnReasoning:             writer.reasoning,
		OnToolCall: func(call agent.ToolCall) {
			writer.toolCall(call, registry)
			sessionRecorder.append(sessions.EventToolCall, map[string]any{
				"id":        call.ID,
				"name":      call.Name,
				"arguments": call.Arguments,
			})
			// Snapshot before-state of files this call will mutate (safe rewind).
			if checkpoint, ok := sessionRecorder.captureCheckpoint(workspaceRoot, call); ok {
				writer.checkpoint(checkpoint)
			}
		},
		OnPermission: func(event agent.PermissionEvent) {
			writer.permission(event)
			sessionRecorder.append(sessionPermissionEventType(event), event)
		},
		OnToolResult: func(result agent.ToolResult) {
			writer.toolResult(result)
			payload := map[string]any{
				"toolCallId": result.ToolCallID,
				"name":       result.Name,
				"status":     string(result.Status),
				"output":     result.Output,
			}
			if len(result.Meta) > 0 {
				payload["meta"] = result.Meta
			}
			if result.Truncated {
				payload["truncated"] = true
			}
			if result.Redacted {
				payload["redacted"] = true
			}
			if len(result.ChangedFiles) > 0 {
				payload["changedFiles"] = result.ChangedFiles
			}
			sessionRecorder.append(sessions.EventToolResult, payload)
		},
		OnUsage: func(u agent.Usage) {
			writer.usage(u)
			payload := usage.EventUsagePayload(u)
			// Attribute usage to a specific model ONLY when escalation is enabled:
			// the model in force can change mid-run only under --allow-escalation, so
			// the "model" key is meaningful exclusively then. Omitting it otherwise
			// keeps a non-escalation run's persisted usage payload compact.
			if options.allowEscalation {
				payload["model"] = currentModel
			}
			sessionRecorder.append(sessions.EventUsage, payload)
		},
	})
	// Finish the trace now that the turn is done, so the snapshot captures exactly
	// agent.Run's work and nothing the post-run cleanup stamps. The deferred
	// writer serializes the snapshot on every exit path.
	if traceRecorder != nil {
		traceSnapshot = traceRecorder.Finish()
	}
	notifier.Notify(notify.Completion, notify.DefaultMessage(notify.Completion))
	if writer.err != nil {
		return exitCrash
	}
	if err != nil {
		emitScratchWarning()
		// A Ctrl+C / SIGTERM cancellation is a clean shutdown, not a provider error.
		if errors.Is(err, context.Canceled) || runCtx.Err() != nil {
			sessionRecorder.append(sessions.EventError, map[string]any{"message": "interrupted"})
			switch options.outputFormat {
			case execOutputStreamJSON:
				writer.errorEvent("interrupted", "run cancelled by signal", false)
				writer.runEnd("interrupted", exitInterrupted)
				if writer.err != nil {
					return exitCrash
				}
			case execOutputJSON:
				// Emit a terminal error+done so a -o json consumer sees a clean end
				// of stream instead of silence (the stream-json path already did).
				if writeErr := writeJSONLine(stdout, map[string]any{
					"type":    "error",
					"code":    "interrupted",
					"message": "run cancelled by signal",
				}); writeErr != nil {
					return exitCrash
				}
				if writeErr := writeJSONLine(stdout, map[string]any{
					"type":      "done",
					"exit_code": exitInterrupted,
				}); writeErr != nil {
					return exitCrash
				}
			default:
				fmt.Fprintln(stderr, "Interrupted.")
			}
			return exitInterrupted
		}
		sessionRecorder.append(sessions.EventError, map[string]any{"message": err.Error()})
		if options.outputFormat == execOutputStreamJSON {
			writer.errorEvent("provider_error", err.Error(), false)
			writer.runEnd("error", exitProvider)
			if writer.err != nil {
				return exitCrash
			}
			return exitProvider
		}
		return writeExecProviderError(stdout, stderr, options.outputFormat, "provider_error", err.Error())
	}
	sessionRecorder.append(sessions.EventMessage, map[string]any{
		"role":    "assistant",
		"content": result.FinalAnswer,
	})

	if notice := result.TruncationNotice(); notice != "" {
		writer.warning(notice)
	}
	// Surface new scratch/debug-like files left untracked in git when we finish —
	// including files created by shell commands that bypass the file tools (issue
	// #551). Normal new deliverable files stay quiet to avoid warning fatigue.
	emitScratchWarning()
	// A headless run the completion gate marked INCOMPLETE (no-tool-call stall,
	// self-reported non-completion, or max-turns cutoff) must NOT be reported as a
	// success: exit 4 AND a machine-readable terminal event saying so. Handled per
	// output format BEFORE writer.final(), because for -o json final() emits a
	// {"type":"done","exit_code":0} that would otherwise mask the incomplete exit.
	// An `error` event (not just a warning) is emitted so log/cron consumers that
	// scan for type=="error" can recover the reason.
	if result.Incomplete {
		reason := result.IncompleteReason
		if reason == "" {
			reason = "run stopped with work unfinished"
		}
		sessionRecorder.append(sessions.EventError, map[string]any{"message": "incomplete: " + reason})
		switch options.outputFormat {
		case execOutputStreamJSON:
			writer.final(result.FinalAnswer)
			writer.errorEvent("incomplete", reason, false)
			writer.runEnd("incomplete", exitIncomplete)
			if writer.err != nil {
				return exitCrash
			}
		case execOutputJSON:
			if writeErr := writeJSONLine(stdout, map[string]any{"type": "final", "text": result.FinalAnswer}); writeErr != nil {
				return exitCrash
			}
			if writeErr := writeJSONLine(stdout, map[string]any{"type": "error", "code": "incomplete", "message": reason}); writeErr != nil {
				return exitCrash
			}
			if writeErr := writeJSONLine(stdout, map[string]any{"type": "done", "exit_code": exitIncomplete}); writeErr != nil {
				return exitCrash
			}
		default:
			writer.final(result.FinalAnswer)
			fmt.Fprintln(stderr, "Run incomplete (not reported as success): "+reason)
		}
		return exitIncomplete
	}
	writer.final(result.FinalAnswer)
	writer.runEnd("success", exitSuccess)
	if writer.err != nil {
		return exitCrash
	}
	return exitSuccess
}

// deferredEligibleCount returns the number of registered tools that are
// deferred-eligible (MCP tools) AND visible to the model for THIS run — i.e. they
// pass the same agent.ToolVisible gate (permission-mode advertising + operator
// allow/deny filters) that the agent loop's partitionTools applies when it
// decides whether deferral activates. Counting the SAME visible-deferred
// population here keeps registration and activation in agreement: tool_search is
// registered iff the partition will actually go active. Built-ins never implement
// the Deferred interface, so they never count.
// newExecSelfCorrector builds the post-edit corrector for a headless run and a
// cleanup func the caller must defer. When enabled it wires both halves: the
// workspace test plan AND an LSP diagnostics checker backed by a per-run
// lsp.Manager. The manager is lazy — Manager.Check degrades a missing/unsupported
// language server to (nil, nil), so enabling the LSP half never spawns a server
// unless a changed file's language actually has one installed on PATH. The
// returned cleanup shuts that manager down (terminating any spawned server
// sessions); it is a no-op when self-correct is off.
func newExecSelfCorrector(enabled bool, workspaceRoot string, autonomy string) (*agent.SelfCorrector, func(context.Context, string) string, func()) {
	// The manager is created regardless of --self-correct: it also backs the
	// always-on background post-edit diagnostics (agent.NewFileDiagnostics,
	// collected off the tool-call path by the loop), and it stays lazy — no
	// language server is spawned unless an edited file's language actually has
	// one installed on PATH.
	manager := lsp.NewManager(workspaceRoot)
	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = manager.Shutdown(ctx)
	}
	fileDiagnostics := agent.NewFileDiagnostics(manager, workspaceRoot)
	if !enabled {
		return nil, fileDiagnostics, cleanup
	}
	corrector := agent.NewSelfCorrector(workspaceRoot, agent.NewLSPDiagnosticsChecker(manager), agent.NewProjectVerifier(workspaceRoot), agent.SelfCorrectConfig{
		Enabled:      true,
		IncludeTests: true,
		IncludeLSP:   true,
		Autonomy:     autonomy,
	})
	return corrector, fileDiagnostics, cleanup
}

func deferredEligibleCount(registry *tools.Registry, permissionMode agent.PermissionMode, enabledTools []string, disabledTools []string) int {
	count := 0
	for _, tool := range registry.All() {
		// Count by deferral-eligibility (not current deferred state) to match
		// partitionTools' active-gate: a tool that un-defers at runtime still
		// participates in the threshold. At registration time the swarm is empty
		// so this equals IsDeferred, but it keeps the two count sites consistent.
		if !tools.IsDeferralEligible(tool) {
			continue
		}
		if !agent.ToolVisible(tool, permissionMode, enabledTools, disabledTools) {
			continue
		}
		count++
	}
	return count
}

// registerToolSearchIfEligible registers the tool_search tool only when deferral
// is active for this run: the visible-deferred count (the same population the
// agent loop's partition counts) meets the (positive) threshold. Below threshold
// or with a zero/negative threshold, tool_search is never registered, so the
// agent loop's partition stays inactive and tool advertising is byte-identical to
// today. The permissionMode + enabled/disabled filters MUST match the values the
// run passes to agent.Run so the registration gate and the activation gate count
// the same tools.
func registerToolSearchIfEligible(registry *tools.Registry, deferThreshold int, permissionMode agent.PermissionMode, enabledTools []string, disabledTools []string) {
	if deferThreshold <= 0 {
		return
	}
	if _, registered := registry.Get(tools.ToolSearchToolName); registered {
		return
	}
	if deferredEligibleCount(registry, permissionMode, enabledTools, disabledTools) < deferThreshold {
		return
	}
	registry.Register(tools.NewToolSearchTool(registry))
}

func buildExecSandboxEngine(workspaceRoot string, resolved config.ResolvedConfig, deps appDeps, scope *sandbox.Scope) (*sandbox.Engine, error) {
	store, err := deps.newSandboxStore()
	if err != nil {
		return nil, err
	}
	policy := applyConfiguredSandboxPolicy(sandbox.DefaultPolicy(), resolved.Sandbox)
	backend := deps.selectSandboxBackend(sandbox.BackendOptions{})
	return sandbox.NewEngine(sandbox.EngineOptions{
		WorkspaceRoot:    workspaceRoot,
		Policy:           policy,
		Store:            store,
		Backend:          backend,
		Scope:            scope,
		SensitiveEnvKeys: providerSensitiveEnvKeys(resolved),
	}), nil
}

func providerSensitiveEnvKeys(resolved config.ResolvedConfig) []string {
	keys := make([]string, 0, len(resolved.Providers)+1)
	for _, profile := range resolved.Providers {
		if key := strings.TrimSpace(profile.APIKeyEnv); key != "" {
			keys = append(keys, key)
		}
	}
	if key := strings.TrimSpace(resolved.Provider.APIKeyEnv); key != "" {
		keys = append(keys, key)
	}
	return keys
}

// applyConfiguredSandboxPolicy overlays every config-sourced sandbox knob onto
// the default policy.
func applyConfiguredSandboxPolicy(policy sandbox.Policy, cfg config.SandboxConfig) sandbox.Policy {
	// An explicit `"sandbox": {"enabled": false}` disables the sandbox: every
	// request is allowed and commands run unwrapped. Only reachable from global
	// config / CLI — project config never sets Enabled (see mergeProjectConfig).
	if cfg.Enabled != nil && !*cfg.Enabled {
		policy.Mode = sandbox.ModeDisabled
	}
	if network := strings.TrimSpace(cfg.Network); network != "" {
		switch sandbox.NetworkMode(network) {
		case sandbox.NetworkAllow, sandbox.NetworkDeny:
			policy.Network = sandbox.NetworkMode(network)
		}
	}
	// Opt-in hardening/diagnostic flags: only ever turn a feature ON from
	// config, never off, so a programmatic default can't be silently disabled by
	// an omitted key.
	if cfg.BlockUnixSockets {
		policy.BlockUnixSockets = true
	}
	if cfg.MonitorDenials {
		policy.MonitorDenials = true
	}
	return policy
}

func resolveWorkspaceRoot(cwd string, deps appDeps) (string, error) {
	current, err := deps.getwd()
	if err != nil {
		return "", fmt.Errorf("failed to resolve workspace: %w", err)
	}

	workspaceRoot := strings.TrimSpace(cwd)
	if workspaceRoot == "" {
		workspaceRoot = current
	} else if !filepath.IsAbs(workspaceRoot) {
		workspaceRoot = filepath.Join(current, workspaceRoot)
	}
	workspaceRoot = filepath.Clean(workspaceRoot)

	info, err := os.Stat(workspaceRoot)
	if err != nil || !info.IsDir() {
		return "", execUsageError{fmt.Sprintf("cwd must be an existing directory: %s", workspaceRoot)}
	}
	return workspaceRoot, nil
}

// resolveExecPrompt resolves the run's prompt text and, for stream-json input,
// the images carried on its message events. The returned image slice is nil for
// text input and for stream-json input that carries no images; it is merged with
// any --image attachments by the caller before the shared vision gate, so both
// sources flow through the same drop+warn and agent.Options.Images wiring.
func resolveExecPrompt(options execOptions, workspaceRoot string, stdin io.Reader) (string, []zeroruntime.ImageBlock, error) {
	if options.inputFormat == execInputStreamJSON {
		input := ""
		if options.file != "" {
			promptPath := options.file
			if !filepath.IsAbs(promptPath) {
				promptPath = filepath.Join(workspaceRoot, promptPath)
			}
			data, err := os.ReadFile(promptPath)
			if err != nil {
				return "", nil, execUsageError{fmt.Sprintf("prompt file not found: %s", promptPath)}
			}
			input = string(data)
		} else {
			data, err := io.ReadAll(stdin)
			if err != nil {
				return "", nil, execUsageError{fmt.Sprintf("failed to read stream-json input: %v", err)}
			}
			input = string(data)
		}
		events, err := streamjson.ParseInput(input)
		if err != nil {
			return "", nil, execUsageError{err.Error()}
		}
		streamImages, err := streamjson.ResolveImages(events)
		if err != nil {
			return "", nil, execUsageError{err.Error()}
		}
		prompt, perr := streamjson.ResolvePrompt(events)
		if perr != nil {
			// An image-only turn (a message event with empty content but at least
			// one image) is valid: ResolvePrompt rejects empty content, but with
			// images present the run proceeds with an empty prompt. Only a turn
			// with neither text nor images is a real error.
			if len(streamImages) == 0 {
				return "", nil, execUsageError{perr.Error()}
			}
			prompt = ""
		}
		return prompt, streamImages, nil
	}

	parts := []string{}
	inlinePrompt := strings.TrimSpace(strings.Join(options.promptParts, " "))
	if inlinePrompt != "" {
		parts = append(parts, inlinePrompt)
	}

	if options.file != "" {
		promptPath := options.file
		if !filepath.IsAbs(promptPath) {
			promptPath = filepath.Join(workspaceRoot, promptPath)
		}
		data, err := os.ReadFile(promptPath)
		if err != nil {
			return "", nil, execUsageError{fmt.Sprintf("prompt file not found: %s", promptPath)}
		}
		filePrompt := strings.TrimSpace(string(data))
		if filePrompt == "" {
			return "", nil, execUsageError{fmt.Sprintf("prompt file is empty: %s", promptPath)}
		}
		parts = append(parts, filePrompt)
	}

	prompt := strings.TrimSpace(strings.Join(parts, "\n\n"))
	if prompt == "" {
		return "", nil, execUsageError{"Prompt required. Use `zero exec \"prompt\"` or `zero exec --file prompt.txt`."}
	}
	return prompt, nil, nil
}

// resolveExecImages loads each --image attachment through the shared
// imageinput.LoadFile loader (read, sniff, normalize, 10-MiB cap), resolving
// relative paths against workspaceRoot. It is a thin per-path loop: the actual
// read/sniff/cap logic lives in internal/imageinput so the CLI and TUI surfaces
// never duplicate it. Any loader error (missing file, unsupported type,
// oversized) is wrapped into an execUsageError so the run reports it as a usage
// problem rather than reaching a provider with an invalid image. Returns nil for
// an empty path list (text-only behavior unchanged).
func resolveExecImages(paths []string, workspaceRoot string) ([]zeroruntime.ImageBlock, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	images := make([]zeroruntime.ImageBlock, 0, len(paths))
	for _, path := range paths {
		image, err := imageinput.LoadFile(path, workspaceRoot)
		if err != nil {
			return nil, execUsageError{err.Error()}
		}
		images = append(images, image)
	}
	return images, nil
}

func writeExecUsageError(stderr io.Writer, message string) int {
	if _, err := fmt.Fprintf(stderr, "[zero] %s\n", message); err != nil {
		return exitCrash
	}
	return exitUsage
}

func writeExecFormatUsageError(stdout io.Writer, stderr io.Writer, format execOutputFormat, message string) int {
	if format == execOutputStreamJSON {
		return writeStreamJSONError(stdout, "usage_error", message, false, exitUsage)
	}
	return writeExecUsageError(stderr, message)
}

func writeExecProviderError(stdout io.Writer, stderr io.Writer, format execOutputFormat, code string, message string) int {
	if format == execOutputStreamJSON {
		return writeStreamJSONError(stdout, code, message, false, exitProvider)
	}
	if format == execOutputJSON {
		if err := writeJSONLine(stdout, map[string]any{
			"type":    "error",
			"code":    code,
			"message": message,
		}); err != nil {
			return exitCrash
		}
		if err := writeJSONLine(stdout, map[string]any{
			"type":      "done",
			"exit_code": exitProvider,
		}); err != nil {
			return exitCrash
		}
		return exitProvider
	}
	if _, err := fmt.Fprintf(stderr, "[zero] %s\n", message); err != nil {
		return exitCrash
	}
	// Append a one-line next step for recognized provider failures (auth /
	// rate-limit / connectivity / …). The classifier gates on a provider-origin
	// marker, so non-provider codes (sandbox_error, mcp_error) never draw a hint.
	if hint := errhint.CLIHint(errors.New(message)); hint != "" {
		if _, err := fmt.Fprintf(stderr, "[zero] %s\n", hint); err != nil {
			return exitCrash
		}
	}
	return exitProvider
}

// applyExecMode expands a --mode preset onto the exec options. The preset only
// fills fields the caller left unset, so an explicit --model / --reasoning-effort
// / --max-turns / tool filter always wins over the mode. The mode's model is left
// as the preset's raw id/alias so the shared --model resolution path resolves it
// through the registry (canonical ids/deprecation fallbacks) AND surfaces any
// deprecation notice on stderr, exactly like an explicit --model. An unknown mode
// is a usage error listing the valid presets.
func applyExecMode(options *execOptions) error {
	name := strings.TrimSpace(options.mode)
	if name == "" {
		return nil
	}
	mode, ok := modelregistry.LookupMode(name)
	if !ok {
		return execUsageError{fmt.Sprintf("unknown mode %q. Valid modes: %s.", options.mode, strings.Join(modelregistry.ModeNames(), ", "))}
	}
	if options.model == "" && mode.Model != "" {
		options.model = mode.Model
	}
	if options.reasoningEffort == "" && mode.Effort != "" {
		options.reasoningEffort = string(mode.Effort)
	}
	if options.maxTurns == 0 && mode.MaxTurns > 0 {
		options.maxTurns = mode.MaxTurns
	}
	if len(options.enabledTools) == 0 && len(mode.EnabledTools) > 0 {
		options.enabledTools = append([]string{}, mode.EnabledTools...)
	}
	if len(options.disabledTools) == 0 && len(mode.DisabledTools) > 0 {
		options.disabledTools = append([]string{}, mode.DisabledTools...)
	}
	return nil
}

// applyExecProfile expands an --exec-profile selection onto the exec options.
// Like applyExecMode it only fills fields the caller left unset, and it runs
// AFTER the mode so a mode-filled field counts as set: precedence is explicit
// flag > --mode > --exec-profile. Only the options-level knobs are applied here
// (reasoning effort still flows through the model-supported gating downstream;
// self-correct is presence-only so a profile can only turn it on). MaxTurns is
// intentionally NOT applied here — the profile displaces the resolved budget
// after config resolution, where the displaced value is known and becomes the
// escalation restore target. An unknown profile is a usage error listing the
// valid names. Not to be confused with the legacy inert --profile flag.
// The second return reports whether the profile actually filled the reasoning
// effort (false when the user set one explicitly), so the escalation policy
// knows the displaced effort was the provider default and can restore it.
func applyExecProfile(options *execOptions) (execprofile.Profile, bool, error) {
	name := strings.TrimSpace(options.execProfile)
	if name == "" {
		return execprofile.Profile{}, false, nil
	}
	profile, ok := execprofile.Lookup(name)
	if !ok {
		return execprofile.Profile{}, false, execUsageError{fmt.Sprintf("unknown execution profile %q. Valid profiles: %s.", options.execProfile, strings.Join(execprofile.Names(), ", "))}
	}
	effortFilled := false
	if options.reasoningEffort == "" && profile.ReasoningEffort != "" {
		options.reasoningEffort = profile.ReasoningEffort
		effortFilled = true
	}
	// Spec-safe projection: the spec-draft path wires no self-corrector (an
	// explicit --self-correct --use-spec is rejected at parse time, before
	// profiles apply), so a profile's self-correct knob is meaningless there.
	// Project it away rather than erroring: the user asked for a thorough
	// DRAFT, and the profile's other knobs (budget, effort) apply to it fine.
	if profile.SelfCorrect && !options.useSpec {
		options.selfCorrect = true
	}
	return profile, effortFilled, nil
}

// specProfileEffortFilled reports whether the profile's effort fill actually
// governs the spec-draft run. An explicit --spec-reasoning-effort replaces the
// filled effort for the draft, so the escalation's effort restore must not arm
// there: escalation must never clear an effort the user pinned by hand.
func specProfileEffortFilled(effortFilled bool, specReasoningEffort string) bool {
	return effortFilled && strings.TrimSpace(specReasoningEffort) == ""
}

// execTurnBudget applies the headless default to the resolved turn budget: an
// interactive run can be continued past the shared default by the user, but an
// exec run has nobody to unstick it, so an unconfigured budget starts at the
// documented ceiling instead of truncating mid-task. Explicit sources
// (--max-turns, mode presets, ZERO_MAX_TURNS, config files) resolve as
// MaxTurnsSet and pass through; an exec-profile budget still wins via
// applyProfileTurnBudget afterward.
func execTurnBudget(resolved config.ResolvedConfig) int {
	if resolved.MaxTurnsSet {
		return resolved.MaxTurns
	}
	return defaultExecMaxTurns
}

// applyProfileTurnBudget decides the run's turn budget once config is resolved.
// The profile displaces the RESOLVED value, not the flag: a pinned budget
// always wins and the profile backs off entirely, while an env/config budget
// is the "balanced posture" a mid-run escalation can restore. pinnedMaxTurns
// is any caller-pinned budget — an explicit --max-turns flag OR a mode
// preset's fill (both land in options.maxTurns, and a mode outranks a profile
// just like the flag does). displaced is 0 when nothing was displaced, so an
// escalation leaves the ceiling untouched.
func applyProfileTurnBudget(profile execprofile.Profile, pinnedMaxTurns int, resolvedMaxTurns int) (effective int, displaced int) {
	if profile.MaxTurns > 0 && pinnedMaxTurns == 0 {
		return profile.MaxTurns, resolvedMaxTurns
	}
	return resolvedMaxTurns, 0
}

// resolveSelectedModel routes a user-supplied --model value through the model
// registry so that fuzzy aliases (e.g. "sonnet 4.5") resolve to canonical ids
// and deprecated models auto-redirect to their fallback. It returns the model id
// to use plus a non-empty notice when a deprecation redirect or warning applies.
// Inputs that the registry does not recognize (e.g. custom openai-compatible
// model names) are returned unchanged so provider passthrough still works.
func resolveSelectedModel(registry modelregistry.Registry, input string) (string, string) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return input, ""
	}
	entry, notice, ok := registry.ResolveWithFallback(trimmed)
	if !ok {
		return input, ""
	}
	return entry.ID, notice
}

// modelContextWindow returns the resolved model's exact context window (max input
// tokens) from the registry, or 0 when the model isn't catalogued. Compaction call
// sites wrap this in modelregistry.AgentContextWindow to apply a positive fallback;
// display/report sites use the raw value so an unknown model shows no denominator.
func modelContextWindow(registry modelregistry.Registry, modelID string) int {
	trimmed := strings.TrimSpace(modelID)
	if trimmed == "" {
		return 0
	}
	if entry, ok := registry.Resolve(trimmed); ok {
		return entry.ContextLimits.ContextWindow
	}
	return 0
}

// resolveAgentContextWindow returns the context window used to enable/size agent
// compaction: the exact registry value when catalogued, else a window learned from
// live provider discovery (so an uncatalogued proxy/custom model gets its real
// window instead of the generic fallback), else the positive FallbackContextWindow.
// Discovery runs only on a registry miss (catalogued models pay no latency), is
// bounded by a short timeout, and degrades to the fallback on any error — so a
// headless run is never blocked or failed by it.
func resolveAgentContextWindow(ctx context.Context, registry modelregistry.Registry, profile config.ProviderProfile) int {
	if window := modelContextWindow(registry, profile.Model); window > 0 {
		return window
	}
	if window := discoveredModelContextWindow(ctx, profile); window > 0 {
		return window
	}
	return modelregistry.AgentContextWindow(0)
}

// discoveredModelContextWindow queries the provider's live model list for the
// active model's context window. Returns 0 when the provider isn't catalogued, the
// credential can't be resolved, discovery fails, or the model reports no window.
func discoveredModelContextWindow(ctx context.Context, profile config.ProviderProfile) int {
	descriptor, ok := providercatalog.Get(strings.TrimSpace(profile.CatalogID))
	if !ok {
		return 0
	}
	// Authenticate discovery with the resolved key (inline, then stored, then env).
	authed := profile
	if strings.TrimSpace(authed.APIKey) == "" {
		if store, err := config.ProviderKeyStore(); err == nil {
			authed = config.ApplyStoredAPIKey(authed, store)
		}
	}
	if strings.TrimSpace(authed.APIKey) == "" && strings.TrimSpace(authed.APIKeyEnv) != "" {
		authed.APIKey = strings.TrimSpace(os.Getenv(authed.APIKeyEnv))
	}
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	models, err := providermodeldiscovery.DiscoverCatalog(dctx, descriptor, authed, providermodeldiscovery.Options{})
	if err != nil {
		return 0
	}
	target := strings.TrimSpace(profile.Model)
	for _, model := range models {
		if strings.EqualFold(strings.TrimSpace(model.ID), target) && model.ContextWindow > 0 {
			return model.ContextWindow
		}
	}
	return 0
}

// forwardedReasoningEffort returns the effort to send on the provider request.
// It mirrors reasoningEffortNotice: a known model that does not support reasoning
// yields "" (matching the "ignoring" advisory, so the request never carries an
// unsupported parameter); a known reasoning model yields its effective level; an
// unknown model (e.g. a custom OpenAI-compatible endpoint) forwards the requested
// value as-is, since no support claim can be made for it.
func forwardedReasoningEffort(registry modelregistry.Registry, modelID string, requested string) string {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return ""
	}
	entry, ok := registry.Get(strings.TrimSpace(modelID))
	if !ok {
		return requested
	}
	effective := modelregistry.EffectiveReasoningEffort(entry, modelregistry.ReasoningEffort(strings.ToLower(requested)))
	if effective == modelregistry.ReasoningEffortNone {
		return ""
	}
	return string(effective)
}

// reasoningEffortNotice resolves the requested --reasoning-effort against the
// selected model's supported efforts via EffectiveReasoningEffort and returns a
// short advisory when the requested value is unsupported (and was coerced to the
// model default).
func reasoningEffortNotice(registry modelregistry.Registry, modelID string, requested string) string {
	trimmed := strings.TrimSpace(modelID)
	if trimmed == "" {
		return ""
	}
	entry, ok := registry.Get(trimmed)
	if !ok {
		return ""
	}
	want := modelregistry.ReasoningEffort(strings.TrimSpace(strings.ToLower(requested)))
	effective := modelregistry.EffectiveReasoningEffort(entry, want)
	if effective == modelregistry.ReasoningEffortNone {
		return fmt.Sprintf("%s does not support reasoning effort; ignoring --reasoning-effort %s", entry.ID, requested)
	}
	if want != "" && effective != want {
		return fmt.Sprintf("reasoning effort %q is not supported by %s; using %s instead", requested, entry.ID, effective)
	}
	return ""
}

func resolveExecRunMetadata(profile config.ProviderProfile) (execRunMetadata, error) {
	metadata, err := providers.ResolveRuntimeMetadata(profile, providers.Options{})
	if err != nil {
		return execRunMetadata{}, err
	}
	provider := strings.TrimSpace(string(metadata.ProviderKind))
	if provider == "" {
		provider = strings.TrimSpace(profile.Name)
	}
	apiModel := strings.TrimSpace(metadata.APIModel)
	if apiModel == "" {
		apiModel = strings.TrimSpace(profile.Model)
	}
	return execRunMetadata{
		Provider: provider,
		Model:    strings.TrimSpace(profile.Model),
		APIModel: apiModel,
	}, nil
}

func writeExecStreamJSONFinal(stdout io.Writer, cwd string, metadata execRunMetadata, permissionMode agent.PermissionMode, text string, exitCode int) int {
	runID, err := streamjson.CreateRunID(time.Now())
	if err != nil {
		return exitCrash
	}
	writer := execEventWriter{
		stdout:       stdout,
		format:       execOutputStreamJSON,
		runID:        runID,
		streamedText: &strings.Builder{},
	}
	writer.runStart(cwd, metadata, permissionMode)
	writer.final(text)
	writer.runEnd("success", exitCode)
	if writer.err != nil {
		return exitCrash
	}
	return exitCode
}

func sessionPermissionEventType(event agent.PermissionEvent) sessions.EventType {
	if event.Action == agent.PermissionActionPrompt {
		return sessions.EventPermissionRequest
	}
	if event.Action == agent.PermissionActionAllow || event.Action == agent.PermissionActionDeny || event.Action == agent.PermissionActionCancel {
		return sessions.EventPermissionDecision
	}
	return sessions.EventPermission
}

// execNotifyMode resolves the effective notification mode for a run:
// --no-notify forces "off", --notify <mode> overrides config, otherwise the
// config value is used.
func execNotifyMode(options execOptions, resolved config.ResolvedConfig) string {
	if options.noNotify {
		return string(notify.ModeOff)
	}
	if options.notifyMode != "" {
		return options.notifyMode
	}
	return resolved.Notify.Mode
}

// resolveTracePath returns the trace destination for this run: the --trace flag
// value when set, else the ZERO_TRACE env var, else "" (tracing off). A value of
// "-" means "write to stderr".
func resolveTracePath(options execOptions) string {
	if v := strings.TrimSpace(options.tracePath); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("ZERO_TRACE")); v != "" {
		return v
	}
	return ""
}

// writeTraceSnapshot writes an already-finished trace snapshot to dest ("-" =>
// stderr, otherwise a file path created/truncated). A nil snapshot is a no-op
// (tracing off, or the run exited before agent.Run produced a turn). The trace
// is best-effort: a write error is returned to the caller, which logs it but
// never fails the run.
func writeTraceSnapshot(snapshot *trace.TurnTrace, dest string, stderr io.Writer) error {
	if snapshot == nil || dest == "" {
		return nil
	}
	if strings.TrimSpace(dest) == "-" {
		return trace.WriteNDJSON(stderr, snapshot)
	}
	file, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer file.Close()
	return trace.WriteNDJSON(file, snapshot)
}
