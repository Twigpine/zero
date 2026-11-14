package specialist

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Twigpine/zero/internal/background"
	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/streamjson"
	"github.com/Twigpine/zero/internal/tools"
)

const (
	sessionTagSpecialist     = "specialist"
	specialistTaskType       = "specialist"
	promptFileThresholdBytes = 4 * 1024
	maxSpecialistDepth       = 8 // hard cap to prevent infinite recursion/resource exhaustion via Task
)

const SessionTagSpecialist = sessionTagSpecialist

// terminateOrphanedProcess stops a launched background child whose PID could not
// be recorded. Tests replace it to drive the failed-kill path.
var terminateOrphanedProcess = background.TerminateProcess

type NewSessionIDFunc func() (string, error)
type WritePromptFileFunc func(prompt string) (string, error)
type LoadFunc func(LoadOptions) (LoadResult, error)
type RunChildFunc func(ctx context.Context, binaryPath string, args []string, progress func(streamjson.Event)) (ChildRunResult, error)
type LaunchBackgroundFunc func(binaryPath string, args []string, outputFile string, onExit func(exitCode int)) (int, error)
type BackgroundManagerFunc func() (*background.Manager, error)

type Executor struct {
	NewSessionID      NewSessionIDFunc
	WritePromptFile   WritePromptFileFunc
	PromptFileMaxSize int
	Load              LoadFunc
	RunChild          RunChildFunc
	LaunchBackground  LaunchBackgroundFunc
	BinaryPath        string
	Paths             Paths
	SessionStore      *sessions.Store
	// SessionBudget bounds how many sub-agents this SESSION may start, across
	// every run and every plan in it. nil is unbounded, which is every caller
	// that does not wire one.
	SessionBudget         *SessionBudget
	BackgroundManager     *background.Manager
	BackgroundManagerFunc BackgroundManagerFunc
	BackgroundRuntime     *Runtime
	// ExtraWriteRoots reports the directories THIS RUN may reach outside its
	// workspace, so a child is confined to the same ground its parent stands on
	// rather than to less.
	//
	// A CHILD GETTING LESS THAN ITS PARENT IS ALSO A BUG, and it produced a real
	// one: a run was granted ~/zm-lab mid-session, created it, copied packages
	// into it, then dispatched plan tasks to read them. Every task was refused
	// — "the target directory is outside the workspace boundary" — because a
	// child rebuilds its sandbox from --cwd alone and the grant lived only in
	// the parent's engine. Two tasks died, two dependents were skipped, and a
	// whole retry plan was spent rediscovering the boundary.
	//
	// A FUNCTION, not a slice, because the answer changes DURING the run: a
	// request_permissions grant lands mid-session, and a value captured at
	// wiring time would carry the roots the run started with forever — which is
	// precisely the staleness that made plan model discovery probe the wrong
	// provider.
	//
	// This never widens beyond the parent. It reports what the parent already
	// holds; a child still cannot ask for more, and the sandbox it builds is its
	// own.
	//
	// withinTurn says whether the child is certain to exit before the parent's
	// current turn ends. Only then may the answer include TURN-SCOPED grants: a
	// root in a child's argv cannot be revoked, so a turn-scoped grant handed to
	// a child that outlives the turn becomes a permanent one. A false caller gets
	// only grants that outlive the turn.
	ExtraWriteRoots func(withinTurn bool) []string
	// ExtraReadRoots is the READ counterpart of ExtraWriteRoots: every path the
	// run may read beyond its workspace — its request_permissions read grants
	// among them — read at every launch, fresh or resumed, and emitted ONLY as
	// --add-read-dir. A read grant handed over as --add-dir would be a write.
	//
	// It was wired into the orchestrate tool alone, so an ordinary Task could
	// not read a path the parent had just been granted, and a resume could not
	// either. Same withinTurn rule as above. A root that is also a write root
	// the child receives is not repeated; one withheld from an isolated child's
	// write roots (an ancestor of the parent workspace) still arrives here, read
	// only, because reading the parent tree does not defeat write isolation.
	ExtraReadRoots func(withinTurn bool) []string
	// WorkspaceRoot is the PARENT run's own workspace. A child whose --cwd lies
	// outside it runs in a tree of its own (an isolated plan worktree), and no
	// write root that overlaps this directory may follow it there: an ancestor
	// such as --add-dir ~/dev, or a root inside the parent tree, would hand the
	// child write access to the very tree the worktree exists to protect. Empty
	// means unknown, and only the exact --cwd match is skipped.
	WorkspaceRoot string
	// ModelPrefs carries the user's per-role model pins and the auto-assign
	// default. A Task that names no model can then be routed to a role-appropriate
	// pin — exactly as the same work would be inside a plan — instead of always
	// inheriting the parent's model. The zero value assigns nothing.
	ModelPrefs ModelPreferences
	// PostureActive reports the zeromaxing gate, read at every spawn because the
	// posture can flip mid-session — a captured bool would carry the value the run
	// started with. Per-task model auto-assignment is off unless this returns true
	// AND ModelPrefs.AutoAssign is configured, so a posture-off spawn never
	// consults a pin and is byte-identical to before this existed. nil is off.
	PostureActive func() bool
	// DiscoverModels lists what the active provider can serve, for checking a
	// role pin before it is applied to a spawn. A pin the provider does not
	// serve is not a pin, it is a leftover — applied blind, three real children
	// died at spawn with `"not-found": The model kimi-k2.6 does not exist`
	// because the session had moved to xai while the pins named Ollama models.
	// nil disables auto-assignment entirely (the honest default for a headless
	// executor), exactly as it does on the plan tool.
	DiscoverModels ModelDiscoverer
	// ServeCache memoizes DiscoverModels for a short window, shared across the
	// copies of this value-typed Executor. Per-spawn discovery would add a
	// provider round-trip to every delegation in a fan-out; a short TTL keeps
	// that to one. Safe across a mid-session provider switch because the
	// session-model guard in autoTaskModel rejects a list the current session's
	// model is not on, whether the list is stale or fresh. nil discovers on
	// every call.
	ServeCache *ModelServeCache
	// LifecycleHooks optionally dispatches specialistStart/specialistStop hooks.
	// The shared pointer lets the CLI attach the hooks dispatcher after specialist
	// tools are registered (hooks are constructed later in startup).
	LifecycleHooks *LifecycleHooks
}

// LifecycleHooks bridges specialist accounting into the hooks dispatcher without
// importing the hooks package into every specialist call site.
type LifecycleHooks struct {
	// Dispatch runs configured hooks for event ("specialistStart"/"specialistStop").
	// specialistName is used as the matcher subject. nil Dispatch is a no-op.
	Dispatch func(ctx context.Context, event string, specialistName string, payload map[string]any)
	// stopMu guards stopClaims, the in-process fallback used when no durable
	// claim can be written (swarm members have no parent session) or the session
	// store is unavailable.
	stopMu     sync.Mutex
	stopClaims map[string]map[string]struct{}
}

// claimInProcessStopDispatch reports whether this bridge should run
// specialistStop for the given child/run. It is the process-local fallback for
// callers without a durable ledger; the run-ID equivalence mirrors
// specialistPayloadMatches: an empty runId is a catch-all for both the query and
// any recorded claim, so a later observation with a runId cannot re-fire a stop
// that was already claimed without one. An empty child session never dedups
// (there is nothing to key on), matching the accounting matcher.
func (h *LifecycleHooks) claimInProcessStopDispatch(childSessionID, runID string) bool {
	if h == nil || h.Dispatch == nil {
		return false
	}
	childSessionID = strings.TrimSpace(childSessionID)
	if childSessionID == "" {
		return true
	}
	runID = strings.TrimSpace(runID)

	h.stopMu.Lock()
	defer h.stopMu.Unlock()
	if h.stopClaims == nil {
		h.stopClaims = map[string]map[string]struct{}{}
	}
	claims := h.stopClaims[childSessionID]
	if claims == nil {
		claims = map[string]struct{}{}
		h.stopClaims[childSessionID] = claims
	}
	claimed := false
	if runID == "" {
		claimed = len(claims) > 0
	} else {
		_, emptyClaim := claims[""]
		_, exactClaim := claims[runID]
		claimed = emptyClaim || exactClaim
	}
	if !claimed {
		claims[runID] = struct{}{}
	}
	return !claimed
}

// ModelServeCache memoizes one provider model listing for a short window.
type ModelServeCache struct {
	mu     sync.Mutex
	at     time.Time
	served map[string]bool
	valid  bool
}

// modelServeCacheTTL bounds staleness. Short: a pin check is advisory routing,
// and two minutes is long enough to cover a five-agent fan-out with one probe.
const modelServeCacheTTL = 2 * time.Minute

// servedModelSet answers "what does the provider serve right now", through the
// cache when one is wired. ok is false when discovery is unavailable or failed
// — the caller inherits rather than guessing.
func (executor Executor) servedModelSet(ctx context.Context) (map[string]bool, bool) {
	if executor.DiscoverModels == nil {
		return nil, false
	}
	cache := executor.ServeCache
	if cache != nil {
		cache.mu.Lock()
		if cache.valid && time.Since(cache.at) < modelServeCacheTTL {
			served := cache.served
			cache.mu.Unlock()
			return served, true
		}
		cache.mu.Unlock()
	}
	models, err := executor.DiscoverModels(ctx)
	if err != nil {
		return nil, false
	}
	served := servedModels(models)
	if cache != nil {
		cache.mu.Lock()
		cache.at, cache.served, cache.valid = time.Now(), served, true
		cache.mu.Unlock()
	}
	return served, true
}

type BuildArgsInput struct {
	Manifest              Manifest
	Prompt                string
	ParentSessionID       string
	ParentToolUseID       string
	ParentModel           string
	ParentReasoningEffort string
	CurrentDepth          int
	Description           string
	Cwd                   string
	// PermissionMode is the parent's resolved permission mode. Only an explicit
	// unsafe mode runs the child at "--auto high"; empty or any other value is
	// fail-safe "low", so a caller that forgets to wire it never escalates the
	// child to unsafe. Authority is therefore never widened beyond the parent.
	PermissionMode string
	// MemberAutonomy marks a headless swarm member: when set and the parent is
	// non-unsafe, the child runs at "--auto member" (PermissionModeMemberAuto) so
	// it can write/edit + run sandboxed shell IN the workspace, instead of the
	// read-only "--auto low". Off by default, so the Task tool's specialists are
	// unchanged. The sandbox still confines writes to the workspace root.
	MemberAutonomy bool
	// ExtraReadRoots are directories this child may read beyond its workspace,
	// emitted as --add-dir. A plan task's scratchpad arrives here.
	ExtraReadRoots []string
	// ReadOnlyRoots are directories this child may READ but not write, emitted as
	// --add-read-dir (scope.AddRead). The parent's request_permissions read grants
	// arrive here — separate from ExtraReadRoots because --add-dir grants write.
	ReadOnlyRoots []string
	// WithinTurn marks a child that exits before the parent's turn ends, so it
	// may inherit the parent's turn-scoped grants. See TaskRunOptions.WithinTurn.
	WithinTurn bool
}

type BuildResumeArgsInput struct {
	SessionID    string
	Prompt       string
	CurrentDepth int
	Manifest     Manifest
	Cwd          string
	// ParentModel and ParentReasoningEffort are the same fallbacks the fresh
	// path takes, and they are here for the same reason: appendModelArgs uses
	// them only when the manifest pins nothing of its own.
	ParentModel           string
	ParentReasoningEffort string
	PermissionMode        string
	// WithinTurn: see BuildArgsInput.WithinTurn.
	WithinTurn bool
}

type BuildArgsResult struct {
	Args      []string
	SessionID string
	// PromptFile is created for large prompts; callers own cleanup after exec finishes.
	PromptFile string
}

type TaskParameters struct {
	Name            string
	Prompt          string
	Description     string
	RunInBackground bool
	Resume          string
	// Model, when set, runs THIS sub-agent on a specific model rather than the
	// parent's. It is the standalone-Task equivalent of a plan task's per-task
	// model: it lets a caller send one worker to a cheap model and another to a
	// strong one without an orchestrate plan. Resolved through the registry
	// (aliases and deprecated ids redirected); a provider is the final authority
	// on whether it will run.
	Model string
	// Manifest, when non-nil, supplies the specialist definition inline instead
	// of resolving Name against the specialist registry. It is validated before
	// use. The swarm launcher sets this so a swarm member can run from its own
	// agent definition (e.g. "subagent"/"teammate"), which is not a registered
	// specialist and would otherwise fail the name lookup.
	Manifest *Manifest
}

type TaskRunOptions struct {
	ToolCallID            string
	ParentSessionID       string
	ParentModel           string
	ParentReasoningEffort string
	CurrentDepth          int
	Cwd                   string
	// PermissionMode propagates the parent's resolved permission mode to the
	// child. Only an explicit unsafe mode runs the child unsafe; empty or any
	// other value is fail-safe "low", so the child never gains more authority
	// than the parent.
	PermissionMode string
	// MemberAutonomy marks a headless swarm member so it can write/edit + run
	// sandboxed shell in the workspace (see BuildArgsInput.MemberAutonomy). Off
	// for Task-tool specialists.
	MemberAutonomy bool
	// ExtraReadRoots are directories this child may read beyond its workspace.
	// Emitted as --add-dir, the same flag the run's own extra roots use.
	ExtraReadRoots []string
	// ReadOnlyRoots are directories this child may READ but not write, emitted as
	// --add-read-dir. The parent's request_permissions read grants arrive here.
	ReadOnlyRoots []string
	// WithinTurn marks a child whose whole life lies inside the parent's current
	// turn — a foreground Task or resume, which the tool call waits on. Only such
	// a child inherits the parent's TURN-SCOPED grants; anything that can outlive
	// the turn (a background Task, a plan task, a swarm member) gets only grants
	// that outlive it too. The zero value is the conservative one.
	WithinTurn bool
	// Progress, when set, is called with each stream-json event emitted by the
	// child process while it runs. nil is a no-op.
	Progress func(streamjson.Event)
	// OnLaunch, when set, is called once immediately before the child is
	// launched — after every parent-side step (manifest, admission, argument
	// and prompt-file building, start accounting). It marks where the CHILD's
	// time begins, so a caller timing the child's silence does not charge it
	// for the parent's preparation. Not called when a run fails before launch.
	OnLaunch func()
}

// specialistAutonomy maps a parent permission mode to the child's "--auto"
// level. A headless specialist child cannot answer interactive prompts, so it
// runs autonomously — but only at "high" (unsafe) when the parent is EXPLICITLY
// unsafe. An empty or unrecognized mode is fail-safe "low": an orchestrator that
// forgets to wire PermissionMode never silently escalates the child to unsafe.
// (Both the Task tool and the swarm propagate a resolved mode, so "" only occurs
// for a caller that omitted it.) The child's authority never exceeds the parent's.
func specialistAutonomy(permissionMode string) string {
	switch strings.TrimSpace(permissionMode) {
	case string(permissionModeUnsafe):
		return "high"
	default:
		return "low" // unset/unknown modes do NOT inherit unsafe autonomy
	}
}

// memberAwareAutonomy is specialistAutonomy with one extra rung for headless
// swarm MEMBERS: a non-unsafe member runs at "member" (PermissionModeMemberAuto)
// so it can write/edit + run sandboxed shell in the workspace, rather than the
// read-only "low". An unsafe parent still yields "high" (full unsafe), and a
// non-member (Task specialist) is unchanged. Authority stays sandbox-confined.
func memberAwareAutonomy(permissionMode string, member bool) string {
	pm := strings.TrimSpace(permissionMode)
	if pm == "plan" || pm == "spec-draft" {
		return "low"
	}
	autonomy := specialistAutonomy(permissionMode)
	if member && autonomy == "low" {
		return "member"
	}
	return autonomy
}

// childPermissionModeFlag returns the --permission-mode value to pass a child
// process, or "" when the child's tool set must be driven by --auto alone.
// Only plan and spec-draft require the flag: resolveExecPermissionMode prefers
// --permission-mode over --auto, so forwarding auto/ask/member/unsafe would
// discard the member rung and widen headless ask children.
func childPermissionModeFlag(permissionMode string) string {
	switch strings.TrimSpace(permissionMode) {
	case "plan", "spec-draft":
		return strings.TrimSpace(permissionMode)
	default:
		return ""
	}
}

// permissionModeUnsafe mirrors agent.PermissionModeUnsafe without importing the
// agent package (which would create an import cycle): exec resolves "--auto high"
// to this mode.
const permissionModeUnsafe = "unsafe"

// readOnlySpecialistTools are the tools a "safe" specialist may hold — pure reads
// plus planning. A specialist whose resolved tools are ALL in this set cannot
// modify the workspace or run commands, so spawning it is harmless and the Task
// tool auto-approves it (no permission prompt).
var readOnlySpecialistTools = map[string]bool{
	"read_file":          true,
	"read_minified_file": true,
	// Reads one image file through read_file's own path scoping and mutates
	// nothing, so it carries the same auto-approval as the other pure readers.
	"view_image":     true,
	"list_directory": true,
	"grep":           true,
	"glob":           true,
	"update_plan":    true,
	// lsp_navigate declares EffectReadOnly with the safety reason "reads files,
	// modifies nothing", and resolves its path through the same scoped
	// confinement its read-only siblings use. Its absence was chronology, not a
	// decision: this list was written in #243 (2026-06-18) and the tool arrived
	// in #276 two days later. planReadOnlyTools already listed it, so the two
	// sets disagreed about what "read-only" means —
	// TestReadOnlyToolSetsAgree now makes that fail rather than ship.
	"lsp_navigate": true,
}

// IsReadOnlySpecialist reports whether the named specialist resolves to a
// read-only tool set. Unknown names and load errors return false, so a caller
// (the Task tool's permission gate) stays on the safe prompt path when in doubt.
func (executor Executor) IsReadOnlySpecialist(name string) bool {
	manifest, err := executor.loadManifest(name)
	if err != nil {
		return false
	}
	return manifestIsReadOnly(manifest)
}

func manifestIsReadOnly(manifest Manifest) bool {
	if len(manifest.ResolvedTools) == 0 {
		return false
	}
	for _, tool := range manifest.ResolvedTools {
		if !readOnlySpecialistTools[tool] {
			return false
		}
	}
	return true
}

type ExecResult struct {
	Result    tools.Result
	SessionID string
	// TotalTokens is the child's own token usage, when the stream reported it.
	// Additive and zero when unknown, so every existing caller is unchanged.
	// The plan executor meters its budget from this: without it the budget was
	// enforced at dispatch against a counter that never moved, so it could
	// never fire — found by driving the real binary, invisible to a unit test
	// whose fake runner fabricated its own token counts.
	TotalTokens int
	// ExitCode is the child process's own exit code, carried so a caller can tell
	// WHY it stopped without reading its prose. childExitIncomplete in particular
	// means "stopped with work unfinished", which a plan treats differently from a
	// wrong answer.
	ExitCode int
	// Model is what the child ACTUALLY ran on — the manifest's model when it
	// names one, the parent's otherwise (resolvedChildModel).
	//
	// CARRIED BACK, because the caller cannot recompute it: only this side has
	// the resolved manifest. The AGENTS sidebar already renders "on <model>" for
	// a plan task and showed nothing for a Task sub-agent, because nothing ever
	// told it which model the child used.
	Model string
	// Signal describes the signal that terminated the child, empty when it exited
	// normally.
	//
	// STRUCTURAL, because the caller has to BRANCH on it. BuildFinalResult already
	// writes the signal into the result prose, and a caller that needed the fact
	// had to match that prose — invariant 9, the rule this package keeps
	// relearning. The plan executor needs it to tell "the plan killed this task"
	// from "something outside killed it", and those call for opposite reports.
	Signal string
}

type ChildRunResult struct {
	Events   []streamjson.Event
	Stderr   string
	ExitCode int
	// Signal is a human-readable description (e.g. "signal: killed") when the child
	// was terminated by a signal rather than exiting normally; empty otherwise. It
	// turns an opaque exit -1 into an actionable reason (SIGKILL ~ out of memory).
	Signal  string
	Started bool
}

func (executor Executor) Run(ctx context.Context, params TaskParameters, options TaskRunOptions) (ExecResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if options.CurrentDepth < 0 {
		return ExecResult{}, fmt.Errorf("current depth cannot be negative")
	}
	// >= (not >): this Run call spawns a CHILD at options.CurrentDepth+1, so a
	// parent already AT the cap must still be rejected here rather than being
	// allowed to launch one more level before the child's own next call trips
	// the guard.
	if options.CurrentDepth >= maxSpecialistDepth {
		return ExecResult{}, fmt.Errorf("spawning a specialist at depth %d would exceed maximum nesting depth %d", options.CurrentDepth+1, maxSpecialistDepth)
	}
	if strings.TrimSpace(params.Prompt) == "" {
		return ExecResult{}, fmt.Errorf("specialist prompt is required")
	}
	if strings.TrimSpace(params.Resume) != "" {
		if params.RunInBackground {
			return ExecResult{}, fmt.Errorf("specialist resume cannot run in background")
		}
		// NOT CHARGED. The budget bounds how many sub-agents this session may
		// START; a resume continues one that was already counted. Charging it
		// meant iterating on a single sub-agent's answer thirty times consumed
		// thirty of the session's slots while spawning nothing.
		return executor.runResume(ctx, params, options)
	}
	// COUNTED HERE: after the checks that can refuse this call outright, and on
	// the FRESH path only, because this is the only path that starts a child.
	// Charging before them spent a slot on a malformed call that never ran.
	// Still ahead of every side effect — no prompt file, no session, no process.
	if err := executor.SessionBudget.admit(); err != nil {
		return ExecResult{}, err
	}
	return executor.runFresh(ctx, params, options)
}

func (executor Executor) BuildArgs(input BuildArgsInput) (BuildArgsResult, error) {
	if input.CurrentDepth < 0 {
		return BuildArgsResult{}, fmt.Errorf("current depth cannot be negative")
	}
	if strings.TrimSpace(input.Prompt) == "" {
		return BuildArgsResult{}, fmt.Errorf("specialist prompt is required")
	}
	sessionID, err := executor.newSessionID()
	if err != nil {
		return BuildArgsResult{}, err
	}
	sessionID = strings.TrimSpace(sessionID)
	if !sessions.ValidSessionID(sessionID) {
		return BuildArgsResult{}, fmt.Errorf("invalid specialist session id %q", sessionID)
	}
	wrappedPrompt := WrapSystemPrompt(input.Manifest.Metadata.Name, input.Manifest.SystemPrompt, input.Prompt, input.Description)
	promptArgs, promptFile, err := executor.buildPromptArgs(wrappedPrompt)
	if err != nil {
		return BuildArgsResult{}, err
	}

	args := []string{"exec", "--init-session-id", sessionID}
	args = append(args, promptArgs...)
	args = appendModelArgs(args, input.Manifest, input.ParentModel, input.ParentReasoningEffort)
	args = append(args, "--auto", memberAwareAutonomy(input.PermissionMode, input.MemberAutonomy), "--output-format", "stream-json")
	// Only plan/spec-draft need an explicit --permission-mode on the child.
	// resolveExecPermissionMode short-circuits on --permission-mode and ignores
	// --auto, so propagating auto/ask/member/unsafe would strip member write
	// tools (member + --permission-mode auto) or widen headless ask children
	// past the read-only --auto low tool set. Plan/spec-draft cannot be
	// expressed via --auto alone, so they still require the flag.
	if permMode := childPermissionModeFlag(input.PermissionMode); permMode != "" {
		args = append(args, "--permission-mode", permMode)
	}
	toolAllowlist, err := resolvedToolAllowlist(input.Manifest)
	if err != nil {
		return BuildArgsResult{}, err
	}
	if len(toolAllowlist) == 0 {
		return BuildArgsResult{}, fmt.Errorf("specialist %q resolved no enabled tools", input.Manifest.Metadata.Name)
	}
	args = append(args, "--enabled-tools", strings.Join(toolAllowlist, ","))
	args = append(args, "--depth", strconv.Itoa(input.CurrentDepth+1), "--tag", sessionTagSpecialist)
	if parentSessionID := strings.TrimSpace(input.ParentSessionID); parentSessionID != "" {
		args = append(args, "--calling-session-id", parentSessionID)
	}
	if parentToolUseID := strings.TrimSpace(input.ParentToolUseID); parentToolUseID != "" {
		args = append(args, "--calling-tool-use-id", parentToolUseID)
	}
	// Always record the specialist name in the session title. AgentName is
	// derived from the title's "name:" prefix, and resume refuses a session whose
	// AgentName is empty — so a description-less run (description is optional)
	// must still carry the name or it can never be resumed.
	name := strings.TrimSpace(input.Manifest.Metadata.Name)
	if description := strings.TrimSpace(input.Description); description != "" {
		args = append(args, "--session-title", name+": "+description)
	} else {
		args = append(args, "--session-title", name)
	}
	if cwd := strings.TrimSpace(input.Cwd); cwd != "" {
		args = append(args, "--cwd", cwd)
	}
	// READ HERE, not passed in. A field on the input would be one more thing every
	// call site must remember, and this file already carries the scar: the resume
	// path forgot the model the fresh path carried, and the plan path forgot the
	// progress callback the Task path carried. The executor knows its own run;
	// asking it directly is the only version with no seam to forget.
	args = appendExtraWriteRootArgs(args, executor.extraWriteRoots(input.WithinTurn), input.Cwd, executor.WorkspaceRoot)
	// PER-CALL ROOTS, after the run's own. A plan task's scratchpad belongs to
	// ONE plan, so it cannot come from executor.extraWriteRoots — that is read at
	// every launch and would leak one plan's directory into every other child the
	// session starts.
	args = appendExtraWriteRootArgs(args, input.ExtraReadRoots, input.Cwd, executor.WorkspaceRoot)
	// READ-ONLY roots on their own flag: the parent's request_permissions read
	// grants, so a plan can audit a granted external path. --add-read-dir, never
	// --add-dir, so the child can read but not write — a read grant must not
	// become a write.
	args = appendReadOnlyRootArgs(args, input.ReadOnlyRoots, input.Cwd)
	// The run's own read grants, read at launch like its write roots above.
	args = appendReadOnlyRootArgs(args, executor.extraReadRoots(input.WithinTurn), input.Cwd)
	return BuildArgsResult{Args: args, SessionID: sessionID, PromptFile: promptFile}, nil
}

// appendExtraWriteRootArgs emits one --add-dir per root the RUN already holds.
//
// The child's own workspace is skipped: --cwd already covers it, and repeating
// it as an extra root says the same thing twice in a way a reader would have to
// check. Blank entries are dropped rather than emitted as an empty flag value,
// which the child would reject outright and turn a scope detail into a launch
// failure.
//
// AN ISOLATED CHILD GETS NO WRITE ROOT THAT OVERLAPS THE PARENT'S TREE. When
// --cwd lies outside parentWorkspace the child runs in a tree of its own, and a
// root that CONTAINS the parent workspace (an ancestor: --add-dir ~/dev, a
// config additionalWriteRoots entry) or is CONTAINED in it re-opens the parent
// tree for writing. Skipping only the byte-equal root let exactly that through:
// a plan task in its worktree wrote into the user's real checkout with no
// branch and no diff. Unrelated roots still follow the child.
func appendExtraWriteRootArgs(args []string, roots []string, cwd string, parentWorkspace string) []string {
	workspace := strings.TrimSpace(cwd)
	isolated := childRunsOutsideParent(parentWorkspace, workspace)
	parent := canonicalChildPath(parentWorkspace)
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" || root == workspace {
			continue
		}
		if isolated && pathsOverlap(canonicalChildPath(root), parent, hostPathFlavor) {
			continue
		}
		args = append(args, "--add-dir", root)
	}
	return args
}

// childRunsOutsideParent reports whether a child launched at cwd runs in a tree
// of its own rather than in (or below) the parent's workspace. Either side
// unknown answers false: there is no separate tree to protect.
func childRunsOutsideParent(parentWorkspace, cwd string) bool {
	parent := canonicalChildPath(parentWorkspace)
	child := canonicalChildPath(cwd)
	if parent == "" || child == "" {
		return false
	}
	return !pathContains(parent, child, hostPathFlavor)
}

// canonicalChildPath is the form two launch paths are compared in: absolute,
// cleaned, and symlink-resolved when the path exists, so /var and /private/var
// (or a symlinked checkout) are recognised as one place.
func canonicalChildPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(path)
}

// pathFlavor is how a platform spells and compares paths. A value rather than
// runtime checks inline, so the Windows rules are testable on every host.
type pathFlavor struct {
	// foldCase compares case-insensitively: Windows always, macOS by default.
	// Folding where the volume is in fact case-sensitive can only withhold a
	// root, never grant one.
	foldCase bool
	// backslash treats '\' as a separator as well as '/'.
	backslash bool
}

var hostPathFlavor = pathFlavor{
	foldCase:  runtime.GOOS == "windows" || runtime.GOOS == "darwin",
	backslash: runtime.GOOS == "windows",
}

// pathsOverlap reports whether either path contains the other.
func pathsOverlap(a, b string, flavor pathFlavor) bool {
	return pathContains(a, b, flavor) || pathContains(b, a, flavor)
}

// pathContains reports whether path is root or lies beneath it, on a separator
// boundary: "/a/b" contains "/a/b/c" but not "/a/bc".
func pathContains(root, path string, flavor pathFlavor) bool {
	if root == "" || path == "" {
		return false
	}
	if flavor.foldCase {
		root, path = strings.ToLower(root), strings.ToLower(path)
	}
	if flavor.backslash {
		// One spelling of the separator, so C:\a and C:/a/b compare.
		root, path = strings.ReplaceAll(root, `\`, "/"), strings.ReplaceAll(path, `\`, "/")
	}
	if root == path {
		return true
	}
	// A filesystem root ("/", `C:\`) trims to its volume and still contains
	// everything on it.
	trimmed := strings.TrimRight(root, "/")
	if !strings.HasPrefix(path, trimmed) || len(path) <= len(trimmed) {
		return false
	}
	return path[len(trimmed)] == '/'
}

// appendReadOnlyRootArgs emits one --add-read-dir per root — READ access only, the
// read counterpart of appendExtraWriteRootArgs. Same skips (the child's own
// workspace, blank entries) for the same reasons, plus any root the args
// already grant: a write root already confers read, and a second read flag for
// the same path says nothing new.
func appendReadOnlyRootArgs(args []string, roots []string, cwd string) []string {
	workspace := strings.TrimSpace(cwd)
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" || root == workspace || rootArgPresent(args, root) {
			continue
		}
		args = append(args, "--add-read-dir", root)
	}
	return args
}

// rootArgPresent reports whether args already pass root as --add-dir or
// --add-read-dir.
func rootArgPresent(args []string, root string) bool {
	for index := 0; index+1 < len(args); index++ {
		if (args[index] == "--add-dir" || args[index] == "--add-read-dir") && args[index+1] == root {
			return true
		}
	}
	return false
}

// extraWriteRoots reports the run's non-workspace roots, or none when the caller
// never wired the supplier — the same fail-safe direction as every other unset
// hook here: a child confined more tightly than its parent is a smaller problem
// than one confined less.
func (executor Executor) extraWriteRoots(withinTurn bool) []string {
	if executor.ExtraWriteRoots == nil {
		return nil
	}
	return executor.ExtraWriteRoots(withinTurn)
}

// extraReadRoots is extraWriteRoots for the read supplier: none when unwired.
func (executor Executor) extraReadRoots(withinTurn bool) []string {
	if executor.ExtraReadRoots == nil {
		return nil
	}
	return executor.ExtraReadRoots(withinTurn)
}

func (executor Executor) BuildResumeArgs(input BuildResumeArgsInput) (BuildArgsResult, error) {
	sessionID := strings.TrimSpace(input.SessionID)
	if sessionID == "" {
		return BuildArgsResult{}, fmt.Errorf("resume session id is required")
	}
	if !sessions.ValidSessionID(sessionID) {
		return BuildArgsResult{}, fmt.Errorf("invalid resume session id %q", sessionID)
	}
	if input.CurrentDepth < 0 {
		return BuildArgsResult{}, fmt.Errorf("current depth cannot be negative")
	}
	if strings.TrimSpace(input.Prompt) == "" {
		return BuildArgsResult{}, fmt.Errorf("specialist prompt is required")
	}
	promptArgs, promptFile, err := executor.buildPromptArgs(WrapResumePrompt(input.Prompt))
	if err != nil {
		return BuildArgsResult{}, err
	}
	args := []string{"exec", "--resume", sessionID}
	args = append(args, promptArgs...)
	// A RESUMED SPECIALIST KEEPS THE MODEL ITS MANIFEST PINNED.
	//
	// The fresh path appends this and the resume path did not, so a specialist
	// pinned to a cheap model ran on that model once and then silently reverted
	// to the parent's configured model on every resume. Nothing reports it: the
	// child starts normally and the only symptom is the bill.
	//
	// Resuming does not restore the recorded model on its own. sessions.PrepareExec
	// records the model it ran under but never feeds it back into provider
	// construction, so without this the child takes whatever the config default
	// resolves to now.
	args = appendModelArgs(args, input.Manifest, input.ParentModel, input.ParentReasoningEffort)
	args = append(args, "--auto", specialistAutonomy(input.PermissionMode), "--output-format", "stream-json")
	// See BuildArgs: only plan/spec-draft propagate --permission-mode so --auto
	// remains the authority for member/auto/ask/unsafe child resolution.
	if permMode := childPermissionModeFlag(input.PermissionMode); permMode != "" {
		args = append(args, "--permission-mode", permMode)
	}
	toolAllowlist, err := resolvedToolAllowlist(input.Manifest)
	if err != nil {
		return BuildArgsResult{}, err
	}
	if len(toolAllowlist) == 0 {
		return BuildArgsResult{}, fmt.Errorf("specialist %q resolved no enabled tools", input.Manifest.Metadata.Name)
	}
	args = append(args, "--enabled-tools", strings.Join(toolAllowlist, ","))
	args = append(args, "--depth", strconv.Itoa(input.CurrentDepth+1), "--tag", sessionTagSpecialist)
	if cwd := strings.TrimSpace(input.Cwd); cwd != "" {
		args = append(args, "--cwd", cwd)
	}
	// THE SAME ROOTS THE FRESH PATH EMITS. This file's own history is the reason
	// it is spelled out: the resume path once forgot the model the fresh path
	// carried, so a resumed task silently ran on a different one. A resumed task
	// confined more tightly than the task it resumes would fail on files it had
	// already read.
	args = appendExtraWriteRootArgs(args, executor.extraWriteRoots(input.WithinTurn), input.Cwd, executor.WorkspaceRoot)
	// And the same READ grants, read-only: a resumed task must still be able to
	// read the external path the fresh launch could.
	args = appendReadOnlyRootArgs(args, executor.extraReadRoots(input.WithinTurn), input.Cwd)
	return BuildArgsResult{Args: args, SessionID: sessionID, PromptFile: promptFile}, nil
}

func (executor Executor) runFresh(ctx context.Context, params TaskParameters, options TaskRunOptions) (ExecResult, error) {
	manifest, err := executor.resolveManifest(params)
	if err != nil {
		return ExecResult{}, err
	}
	// A Task that named no model inherits the parent's — unless the zeromaxing
	// posture is on and auto-assignment is configured, in which case it is routed
	// to the pin for its role, just as a plan task would be. An explicit model on
	// the call always wins, and a specialist that DECLARES its own model keeps it:
	// autoTaskModel is consulted only when neither the call nor the manifest named
	// one, so it never overrides a choice already made.
	requested := params.Model
	if strings.TrimSpace(requested) == "" && strings.TrimSpace(manifest.Metadata.Model) == "" {
		requested = executor.autoTaskModel(ctx, manifest, params.Prompt, options.ParentModel)
	}
	if err := applyTaskModel(&manifest, requested, options.ParentReasoningEffort); err != nil {
		return ExecResult{}, err
	}
	built, err := executor.BuildArgs(BuildArgsInput{
		Manifest:              manifest,
		Prompt:                params.Prompt,
		ParentSessionID:       options.ParentSessionID,
		ParentToolUseID:       options.ToolCallID,
		ParentModel:           options.ParentModel,
		ParentReasoningEffort: options.ParentReasoningEffort,
		CurrentDepth:          options.CurrentDepth,
		Description:           params.Description,
		Cwd:                   options.Cwd,
		PermissionMode:        options.PermissionMode,
		MemberAutonomy:        options.MemberAutonomy,
		ExtraReadRoots:        options.ExtraReadRoots,
		ReadOnlyRoots:         options.ReadOnlyRoots,
		WithinTurn:            options.WithinTurn,
	})
	if err != nil {
		return ExecResult{}, err
	}
	if params.RunInBackground {
		return executor.runBackground(ctx, built, manifest, params, options)
	}
	return executor.runBuiltArgs(ctx, built, manifest, params, options, "foreground", options.Progress)
}

// resolveManifest resolves the manifest for a run. A caller-supplied inline
// manifest (validated here) takes precedence over a registry lookup by name, so a
// caller with its own definition — the swarm launcher running a member whose
// agent type is not a registered specialist, or a plan task running under a
// narrowed grant — can run without a registry entry.
//
// USED BY BOTH the fresh and resume paths. It used to serve only the fresh one:
// runResume looked the manifest up by session.AgentName and ignored
// params.Manifest entirely, so the two paths answered "what may this child do?"
// differently. For an inline-manifest child that is not merely a lost
// definition — it is a WIDENING. A plan task launched under a parent holding
// only grep carries the inline grant [grep]; resuming it reloaded the
// registered explorer manifest and handed it back
// [glob grep list_directory read_file read_minified_file]. Resuming a task
// must never grant it more than launching it did.
func (executor Executor) resolveManifest(params TaskParameters) (Manifest, error) {
	if params.Manifest != nil {
		manifest := *params.Manifest
		if err := Validate(&manifest); err != nil {
			return Manifest{}, fmt.Errorf("inline specialist manifest: %w", err)
		}
		return manifest, nil
	}
	return executor.loadManifest(params.Name)
}

func (executor Executor) runResume(ctx context.Context, params TaskParameters, options TaskRunOptions) (ExecResult, error) {
	session, err := executor.resumeSession(params.Resume)
	if err != nil {
		return ExecResult{}, err
	}
	specialistName := strings.TrimSpace(session.AgentName)
	if specialistName == "" {
		return ExecResult{}, fmt.Errorf("resume session %q does not identify a specialist", session.SessionID)
	}
	if requestedName := strings.TrimSpace(params.Name); requestedName != "" && requestedName != specialistName {
		return ExecResult{}, fmt.Errorf("resume session %q belongs to specialist %q, not %q", session.SessionID, specialistName, requestedName)
	}
	// The SAME resolver the fresh path uses: an inline manifest wins, and only
	// a caller that supplied none falls back to the registry. params.Name is
	// already reconciled against the session's agent name above, so the
	// fallback still looks up the specialist the session actually belongs to.
	resumeParams := params
	if strings.TrimSpace(resumeParams.Name) == "" {
		resumeParams.Name = specialistName
	}
	manifest, err := executor.resolveManifest(resumeParams)
	if err != nil {
		return ExecResult{}, err
	}
	// THE MODEL THE SESSION ACTUALLY RAN ON. BuildResumeArgs falls back to the
	// parent's model when the manifest names none, so a task launched on an
	// explicit or auto-assigned model came back from a resume on a different one
	// — the drift BuildResumeArgsInput's own comment records, reintroduced one
	// layer up. The child recorded its resolved model in the session store at
	// launch; prefer it when neither the resume call nor the manifest names one.
	//
	// An explicit model on the resume call wins, exactly as on a fresh launch —
	// it was silently ignored here before, and the two doors must not disagree.
	// Auto-assignment is deliberately NOT re-run: a resume prompt is a follow-up
	// ("now verify it"), and re-classifying it would flip a session's model
	// midway through its own conversation.
	requested := params.Model
	if strings.TrimSpace(requested) == "" && strings.TrimSpace(manifest.Metadata.Model) == "" {
		requested = executor.resumeModelStillServed(ctx, session.ModelID)
	}
	if err := applyTaskModel(&manifest, requested, options.ParentReasoningEffort); err != nil {
		return ExecResult{}, err
	}
	built, err := executor.BuildResumeArgs(BuildResumeArgsInput{
		SessionID:             params.Resume,
		Prompt:                params.Prompt,
		CurrentDepth:          options.CurrentDepth,
		Manifest:              manifest,
		Cwd:                   options.Cwd,
		ParentModel:           options.ParentModel,
		ParentReasoningEffort: options.ParentReasoningEffort,
		PermissionMode:        options.PermissionMode,
		WithinTurn:            options.WithinTurn,
	})
	if err != nil {
		return ExecResult{}, err
	}
	return executor.runBuiltArgs(ctx, built, manifest, params, options, "resume", options.Progress)
}

// resumeModelStillServed returns the model a session ran on, unless discovery
// says this provider no longer serves it.
//
// REJECTED ON EVIDENCE, not on the absence of proof — the opposite direction to
// autoTaskModel, and deliberately so. autoTaskModel ADDS a model the caller did
// not ask for, so it applies one only when the provider's list confirms it.
// This RESTORES the model the session demonstrably ran on, which is the correct
// answer in every case except one: the user switched provider since. So the
// recorded model stands unless the current provider's own listing is available
// AND does not contain it — at which point applying it would spawn a child that
// dies with "not-found", the failure this whole family of guard exists for.
//
// No discoverer, a failed listing, or an empty one all leave the recovery in
// place: without evidence, continuing on the model the session actually used
// beats drifting to a different one mid-conversation.
func (executor Executor) resumeModelStillServed(ctx context.Context, recorded string) string {
	recorded = strings.TrimSpace(recorded)
	if recorded == "" {
		return ""
	}
	served, ok := executor.servedModelSet(ctx)
	if !ok || len(served) == 0 {
		return recorded
	}
	if !servedContains(served, recorded) {
		return ""
	}
	return recorded
}

func (executor Executor) runBackground(ctx context.Context, built BuildArgsResult, manifest Manifest, params TaskParameters, options TaskRunOptions) (ExecResult, error) {
	if err := ctx.Err(); err != nil {
		if built.PromptFile != "" {
			cleanupPromptFile(built.PromptFile)
		}
		return ExecResult{}, err
	}
	manager, err := executor.backgroundManager()
	if err != nil {
		if built.PromptFile != "" {
			cleanupPromptFile(built.PromptFile)
		}
		return ExecResult{}, err
	}
	if err := ctx.Err(); err != nil {
		if built.PromptFile != "" {
			cleanupPromptFile(built.PromptFile)
		}
		return ExecResult{}, err
	}
	outputFile, err := manager.Register(background.RegisterInput{
		TaskID:         built.SessionID,
		Type:           specialistTaskType,
		SpecialistName: manifest.Metadata.Name,
		Description:    params.Description,
		ParentID:       options.ParentSessionID,
	})
	if err != nil {
		if built.PromptFile != "" {
			cleanupPromptFile(built.PromptFile)
		}
		return ExecResult{}, err
	}
	binaryPath, err := executor.binaryPath()
	if err != nil {
		_ = manager.UpdateStatus(built.SessionID, background.StatusError, -1)
		if built.PromptFile != "" {
			cleanupPromptFile(built.PromptFile)
		}
		return ExecResult{}, err
	}
	if err := ctx.Err(); err != nil {
		_ = manager.UpdateStatus(built.SessionID, background.StatusError, -1)
		if built.PromptFile != "" {
			cleanupPromptFile(built.PromptFile)
		}
		return ExecResult{}, err
	}
	executor.trackBackgroundPromptFile(built.SessionID, built.PromptFile)

	accounting := specialistAccountingInput{
		ParentSessionID: options.ParentSessionID,
		ChildSessionID:  built.SessionID,
		SpecialistName:  manifest.Metadata.Name,
		Model:           resolvedChildModel(manifest, options.ParentModel),
		Description:     params.Description,
		ToolCallID:      options.ToolCallID,
		Mode:            "background",
		Background:      true,
	}
	executor.recordSpecialistStart(accounting)
	// The launcher's exit callback runs the terminal accounting, which can claim
	// and dispatch the specialistStop hook. A child that exits immediately would
	// otherwise publish specialistStop before the start decision below completes
	// (and before SetPID records the pid). The callback waits for startSettled
	// from its own goroutine: waiting inline would deadlock a synchronous
	// injected launcher whose onExit runs before it returns.
	startSettled := make(chan struct{})
	defer close(startSettled)
	if options.OnLaunch != nil {
		options.OnLaunch()
	}
	pid, err := executor.launchBackground(binaryPath, built.Args, outputFile, func(exitCode int) {
		go func() {
			<-startSettled
			status := background.StatusCompleted
			if exitCode != 0 {
				status = background.StatusError
			}
			_ = manager.MarkExited(built.SessionID, status, exitCode)
			if task, ok := manager.Get(built.SessionID); ok {
				executor.recordBackgroundTaskAccounting(task, taskStreamSummary(task))
			}
			executor.cleanupBackgroundPromptFile(built.SessionID, built.PromptFile)
		}()
	})
	if err != nil {
		// The child never started: keep the attempt accounting, but publish no
		// public lifecycle hooks for a sub-agent that did not exist.
		accounting.LaunchFailed = true
		_ = manager.UpdateStatus(built.SessionID, background.StatusError, -1)
		executor.cleanupBackgroundPromptFile(built.SessionID, built.PromptFile)
		executor.recordSpecialistStop(accounting, StreamResult{ExitCode: -1}, "error", -1, err, false)
		return ExecResult{}, err
	}
	// Startup is confirmed (launchBackground only returns without error after the
	// child process started), so the specialistStart hook can fire now.
	executor.dispatchSpecialistStartHook(accounting)
	if pid > 0 {
		if err := manager.SetPID(built.SessionID, pid); err != nil {
			// The child is running but its PID was never recorded, so nothing can
			// track or stop it later. Kill it (and its process group) now, mark the
			// task errored, and record the stop so it cannot become an untracked
			// orphan. Once the kill succeeds the stop is a real terminal transition:
			// both the accounting dedup and the shared stop-hook claim make the
			// later onExit accounting a no-op.
			if killErr := terminateOrphanedProcess(pid); killErr != nil {
				// Don't mask a failed kill: a surviving orphan is worse than the
				// SetPID error alone, so surface both to the caller. The child may
				// still be running, so this is not a terminal stop: leave the task
				// running and let the launcher's exit callback mark it exited,
				// record the stop (and claim specialistStop), and remove the prompt
				// file it may still need to read when it really ends.
				err = errors.Join(err, fmt.Errorf("terminate orphaned pid %d: %w", pid, killErr))
				return ExecResult{}, err
			}
			_ = manager.UpdateStatus(built.SessionID, background.StatusError, -1)
			executor.cleanupBackgroundPromptFile(built.SessionID, built.PromptFile)
			executor.recordSpecialistStop(accounting, StreamResult{ExitCode: -1}, "error", -1, err, false)
			return ExecResult{}, err
		}
	}

	output := fmt.Sprintf(`Task launched in background.
task_id: %s
pid: %d
specialist: %s
description: %s

Use TaskOutput with task_id "%s" to check progress.
Use TaskStop with task_id "%s" to stop it.`,
		built.SessionID,
		pid,
		manifest.Metadata.Name,
		strings.TrimSpace(params.Description),
		built.SessionID,
		built.SessionID,
	)
	return ExecResult{
		Result: tools.Result{
			Status: tools.StatusOK,
			Output: strings.TrimSpace(output),
			Meta: map[string]string{
				"task_id":    built.SessionID,
				"session_id": built.SessionID,
			},
		},
		SessionID: built.SessionID,
	}, nil
}

func (executor Executor) resumeSession(sessionID string) (*sessions.Metadata, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("resume session id is required")
	}
	if !sessions.ValidSessionID(sessionID) {
		return nil, fmt.Errorf("invalid resume session id %q", sessionID)
	}
	store := executor.SessionStore
	if store == nil {
		store = sessions.NewStore(sessions.StoreOptions{})
	}
	session, err := store.Get(sessionID)
	if err != nil {
		return nil, err
	}
	if session == nil {
		return nil, fmt.Errorf("resume session not found: %s", sessionID)
	}
	if session.SessionKind != sessions.SessionKindChild || strings.TrimSpace(session.Tag) != sessionTagSpecialist {
		return nil, fmt.Errorf("resume session %q is not a specialist child session", sessionID)
	}
	return session, nil
}

func (executor Executor) runBuiltArgs(ctx context.Context, built BuildArgsResult, manifest Manifest, params TaskParameters, options TaskRunOptions, mode string, progress func(streamjson.Event)) (ExecResult, error) {
	if built.PromptFile != "" {
		defer cleanupPromptFile(built.PromptFile)
	}
	binaryPath, err := executor.binaryPath()
	if err != nil {
		return ExecResult{}, err
	}
	accounting := specialistAccountingInput{
		ParentSessionID: options.ParentSessionID,
		ChildSessionID:  built.SessionID,
		SpecialistName:  manifest.Metadata.Name,
		Model:           resolvedChildModel(manifest, options.ParentModel),
		Description:     params.Description,
		ToolCallID:      options.ToolCallID,
		Mode:            mode,
		Background:      false,
	}
	executor.recordSpecialistStart(accounting)
	startDispatched := false
	onStarted := func() {
		if startDispatched {
			return
		}
		startDispatched = true
		executor.dispatchSpecialistStartHook(accounting)
	}
	if options.OnLaunch != nil {
		options.OnLaunch()
	}
	run, err := executor.runChild(ctx, binaryPath, built.Args, progress, onStarted)
	started := run.Started || err == nil
	if started {
		// A seam that returns a completed run without the explicit Started flag
		// still proves the child existed; make sure the hook fired exactly once.
		onStarted()
	} else {
		accounting.LaunchFailed = true
	}
	if err != nil {
		exitCode := run.exitCodeOr(-1)
		summary := SummarizeStream(run.Events, exitCode)
		// ROLLED UP EVEN THOUGH IT FAILED, for the same reason TotalTokens is
		// returned below: spend does not become hypothetical because the task
		// died. This branch passed a hard-coded false and appended no usage
		// event, so a child killed mid-run had every token it had already spent
		// vanish from the parent's session record — and `zero usage` under-
		// reported by exactly that much.
		rolledUp := executor.rollUpSpecialistUsage(accounting, summary)
		executor.recordSpecialistStop(accounting, summary, "error", summary.ExitCode, err, rolledUp)
		// Carry the child session id even on a post-start failure so a caller (the
		// swarm launcher -> FailWithSession) can still make the failed member
		// drillable; the session exists once the child has started.
		//
		// AND ITS TOKENS. The summary above already holds what the child spent
		// before it died, and returning ExecResult without them reported zero for
		// work that was billed: a plan's budget was never decremented for a failed
		// task and its total under-counted every one. Spend does not become
		// hypothetical because the task failed.
		// AND WHAT IT WROTE BEFORE IT DIED. summary.Text holds everything the
		// child emitted, and this branch returned an empty Result — so a task
		// stopped at its token budget handed on NOTHING, however much it had
		// already found.
		//
		// That is what turned a cut-short task into a lost one: four finders were
		// stopped on budget with real partial findings, and every dependent was
		// skipped because there was nothing to pass along. The work was done and
		// paid for; discarding it at this boundary is the only reason it was lost.
		//
		// Status stays StatusError — the task did not succeed, and a caller must
		// not read this as a finished answer. It is evidence, labelled as partial
		// by whoever passes it on.
		partial := strings.TrimSpace(summary.Text)
		return ExecResult{
			Result: tools.Result{
				Status: tools.StatusError,
				Output: partial,
			},
			SessionID:   built.SessionID,
			TotalTokens: summary.Usage.EffectiveTotalTokens(),
			ExitCode:    summary.ExitCode,
			Signal:      run.Signal,
			Model:       resolvedChildModel(manifest, options.ParentModel),
		}, err
	}
	summary := SummarizeStream(run.Events, run.ExitCode)
	rolledUp := executor.rollUpSpecialistUsage(accounting, summary)
	executor.recordSpecialistStop(accounting, summary, summary.Status, summary.ExitCode, nil, rolledUp)
	return ExecResult{
		Result:      BuildFinalResult(run.Events, run.Stderr, run.ExitCode, run.Signal),
		SessionID:   built.SessionID,
		TotalTokens: summary.Usage.EffectiveTotalTokens(),
		ExitCode:    summary.ExitCode,
		Signal:      run.Signal,
		Model:       resolvedChildModel(manifest, options.ParentModel),
	}, nil
}

// availableSpecialistList renders the registered specialist names for a corrective
// "not found" error, so a model that guessed a wrong name learns the real options.
func availableSpecialistList(result LoadResult) string {
	names := make([]string, 0, len(result.Specialists))
	for _, manifest := range result.Specialists {
		if n := strings.TrimSpace(manifest.Metadata.Name); n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return "(none registered)"
	}
	return strings.Join(names, ", ")
}

func (executor Executor) loadManifest(name string) (Manifest, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Manifest{}, fmt.Errorf("specialist name is required")
	}
	load := executor.Load
	if load == nil {
		load = Load
	}
	result, err := load(LoadOptions{Paths: executor.Paths})
	if err != nil {
		return Manifest{}, err
	}
	manifest, ok := Find(result, name)
	if !ok {
		// Corrective error: a model that invents a specialist name (e.g.
		// "validator-runner", "file-writer") otherwise gets an opaque "not found"
		// and keeps retrying made-up names. List the ACTUALLY-registered ones so it
		// self-corrects — no hardcoded names, since a custom registry may not have
		// worker/explorer/code-review.
		return Manifest{}, fmt.Errorf("specialist %q not found. Available: %s. Pick one of these whose tools fit the task, or omit the name to use the default", name, availableSpecialistList(result))
	}
	return manifest, nil
}

func (executor Executor) binaryPath() (string, error) {
	if path := strings.TrimSpace(executor.BinaryPath); path != "" {
		return path, nil
	}
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve zero executable: %w", err)
	}
	return path, nil
}

// runChild runs one child process. onStarted, when non-nil, is invoked exactly
// once as soon as the child process has actually started (after os/exec Start
// succeeds), so public lifecycle hooks cannot fire for a launch that failed.
// The injected RunChild seam cannot stream that signal, so a seam result marked
// Started reports it when the seam returns.
func (executor Executor) runChild(ctx context.Context, binaryPath string, args []string, progress func(streamjson.Event), onStarted func()) (ChildRunResult, error) {
	if executor.RunChild != nil {
		result, err := executor.RunChild(ctx, binaryPath, append([]string(nil), args...), progress)
		if result.Started && onStarted != nil {
			onStarted()
		}
		return result, err
	}
	return runChildProcess(ctx, binaryPath, args, progress, onStarted)
}

func (executor Executor) launchBackground(binaryPath string, args []string, outputFile string, onExit func(exitCode int)) (int, error) {
	if executor.LaunchBackground != nil {
		return executor.LaunchBackground(binaryPath, append([]string(nil), args...), outputFile, onExit)
	}
	return launchBackgroundProcess(binaryPath, args, outputFile, onExit)
}

func (executor Executor) backgroundManager() (*background.Manager, error) {
	if executor.BackgroundRuntime != nil {
		return executor.BackgroundRuntime.Manager()
	}
	if executor.BackgroundManager != nil {
		return executor.BackgroundManager, nil
	}
	if executor.BackgroundManagerFunc != nil {
		return executor.BackgroundManagerFunc()
	}
	return background.NewManager("")
}

func (executor Executor) trackBackgroundPromptFile(taskID string, promptFile string) {
	if promptFile == "" {
		return
	}
	if executor.BackgroundRuntime != nil {
		executor.BackgroundRuntime.TrackPromptFile(taskID, promptFile)
	}
}

func (executor Executor) cleanupBackgroundPromptFile(taskID string, promptFile string) {
	if promptFile == "" {
		return
	}
	if executor.BackgroundRuntime != nil {
		executor.BackgroundRuntime.UntrackPromptFile(taskID)
		return
	}
	cleanupPromptFile(promptFile)
}

// resolvedChildModel is what the child will actually run on: its own model when
// the manifest names one, the parent's otherwise.
//
// ONE RULE, TWO READERS. The argv builder needs it to pass --model, and usage
// accounting needs it to price the tokens against the right model. Computing it
// twice is how the command line and the bill come to disagree about which model
// did the work.
func resolvedChildModel(manifest Manifest, parentModel string) string {
	if model := strings.TrimSpace(manifest.Metadata.Model); model != "" {
		return model
	}
	return strings.TrimSpace(parentModel)
}

func appendModelArgs(args []string, manifest Manifest, parentModel string, parentReasoningEffort string) []string {
	resolvedModel := resolvedChildModel(manifest, parentModel)
	if resolvedModel != "" {
		args = append(args, "--model", resolvedModel)
	}

	reasoningEffort := strings.TrimSpace(manifest.Metadata.ReasoningEffort)
	if reasoningEffort == "" && strings.TrimSpace(manifest.Metadata.Model) == "" {
		reasoningEffort = strings.TrimSpace(parentReasoningEffort)
	}
	if reasoningEffort != "" {
		args = append(args, "--reasoning-effort", reasoningEffort)
	}
	return args
}

// resolvedToolAllowlist returns the tools a child may hold.
//
// ToolsResolved is checked FIRST and on its own: when the producer says the
// list is authoritative, an empty list means the child gets nothing and the
// caller's "resolved no enabled tools" check refuses the run. Testing only
// len(ResolvedTools) > 0 conflated "deliberately nothing" with "unspecified"
// and expanded the empty case to the default read-only category.
func resolvedToolAllowlist(manifest Manifest) ([]string, error) {
	if manifest.ToolsResolved {
		return append([]string(nil), manifest.ResolvedTools...), nil
	}
	if len(manifest.ResolvedTools) > 0 {
		return append([]string(nil), manifest.ResolvedTools...), nil
	}
	return ResolveTools(manifest.Metadata.Tools)
}

func (executor Executor) buildPromptArgs(prompt string) ([]string, string, error) {
	threshold := executor.PromptFileMaxSize
	if threshold <= 0 {
		threshold = promptFileThresholdBytes
	}
	if len([]byte(prompt)) <= threshold {
		return []string{prompt}, "", nil
	}
	path, err := executor.writePromptFile(prompt)
	if err != nil {
		return nil, "", err
	}
	return []string{"--file", path}, path, nil
}

func (executor Executor) newSessionID() (string, error) {
	if executor.NewSessionID != nil {
		return executor.NewSessionID()
	}
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("create specialist session id: %w", err)
	}
	return "specialist_" + hex.EncodeToString(random), nil
}

func (executor Executor) writePromptFile(prompt string) (string, error) {
	if executor.WritePromptFile != nil {
		return executor.WritePromptFile(prompt)
	}
	return writePromptFile(prompt)
}

func writePromptFile(prompt string) (string, error) {
	tmpDir, err := os.MkdirTemp("", "zero-specialist-")
	if err != nil {
		return "", fmt.Errorf("create specialist prompt temp dir: %w", err)
	}
	if err := os.Chmod(tmpDir, 0o700); err != nil {
		return "", fmt.Errorf("secure specialist prompt temp dir: %w", err)
	}
	promptPath := filepath.Join(tmpDir, "prompt.md")
	if err := os.WriteFile(promptPath, []byte(prompt), 0o600); err != nil {
		return "", fmt.Errorf("write specialist prompt file: %w", err)
	}
	return promptPath, nil
}

func cleanupPromptFile(promptFile string) {
	if promptFile == "" {
		return
	}
	dir := filepath.Dir(promptFile)
	if strings.HasPrefix(filepath.Base(dir), "zero-specialist-") {
		_ = os.RemoveAll(dir)
		return
	}
	_ = os.Remove(promptFile)
}

func runChildProcess(ctx context.Context, binaryPath string, args []string, progress func(streamjson.Event), onStarted func()) (ChildRunResult, error) {
	var stderr bytes.Buffer
	command := osexec.CommandContext(ctx, binaryPath, args...)
	// Put the child in its own process group and group-kill on cancel/timeout, so a
	// build/server/bash the sub-agent forked dies with it instead of orphaning (M6).
	hardenSpecialistChild(command)
	command.Stderr = &stderr
	stdout, err := command.StdoutPipe()
	if err != nil {
		return ChildRunResult{Stderr: stderr.String()}, fmt.Errorf("create stdout pipe: %w", err)
	}
	if err := command.Start(); err != nil {
		return ChildRunResult{Stderr: stderr.String()}, fmt.Errorf("start specialist child: %w", err)
	}
	if onStarted != nil {
		onStarted()
	}
	events := []streamjson.Event{}
	reader := bufio.NewReader(stdout)
	for {
		line, readErr := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		if line != "" {
			var event streamjson.Event
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				// Surface parse errors instead of silently dropping lines —
				// ParseStream did this too. Accumulate what we have and let
				// the caller decide via the error.
				events = append(events, streamjson.Event{Type: streamjson.EventError, Message: fmt.Sprintf("parse stream-json line: %v", err)})
			} else {
				events = append(events, event)
				if progress != nil {
					progress(event)
				}
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				events = append(events, streamjson.Event{Type: streamjson.EventError, Message: fmt.Sprintf("read child stdout: %v", readErr)})
			}
			break
		}
	}
	exitCode := 0
	started := true
	signalDesc := ""
	if err := command.Wait(); err != nil {
		var exitErr *osexec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
			if exitCode < 0 && exitErr.ProcessState != nil {
				// ExitCode() is -1 when the child was terminated by a signal rather
				// than exiting. ProcessState.String() is the portable description
				// (e.g. "signal: killed") — capture it so the failure isn't opaque.
				signalDesc = exitErr.String()
			}
		} else {
			return ChildRunResult{Events: events, Stderr: stderr.String(), ExitCode: -1, Started: started}, fmt.Errorf("run specialist child: %w", err)
		}
	}
	return ChildRunResult{Events: events, Stderr: stderr.String(), ExitCode: exitCode, Signal: signalDesc, Started: started}, nil
}

func (run ChildRunResult) exitCodeOr(defaultExitCode int) int {
	if run.Started || run.ExitCode != 0 {
		return run.ExitCode
	}
	return defaultExitCode
}

func launchBackgroundProcess(binaryPath string, args []string, outputFile string, onExit func(exitCode int)) (int, error) {
	file, err := os.OpenFile(outputFile, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, fmt.Errorf("open specialist background output: %w", err)
	}
	command := osexec.Command(binaryPath, args...)
	command.Stdout = file
	command.Stderr = file
	command.Stdin = nil
	// Put the child in its own process group so terminating the task later kills
	// the whole group (any grandchildren it forks) instead of orphaning them.
	background.ConfigureChildProcessGroup(command)
	if err := command.Start(); err != nil {
		_ = file.Close()
		return 0, fmt.Errorf("launch specialist background child: %w", err)
	}
	pid := command.Process.Pid
	go func() {
		exitCode := 0
		if err := command.Wait(); err != nil {
			var exitErr *osexec.ExitError
			if errors.As(err, &exitErr) {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = 1
			}
		}
		_ = file.Close()
		if onExit != nil {
			onExit(exitCode)
		}
	}()
	return pid, nil
}

// applyTaskModel sets a per-Task model on the manifest it will run under.
//
// A MANIFEST MODEL OVERRIDES THE PARENT'S — resolvedChildModel prefers it — so
// setting it here is all it takes for a standalone Task to run on a different
// model. An empty request changes nothing, keeping every existing spawn on the
// inherited model exactly as before.
//
// AND IT FORWARDS THE PARENT'S EFFORT. appendModelArgs inherits the parent's
// reasoning effort ONLY when no model is named, so naming a model would
// otherwise drop the zeromaxing posture's raised effort. Setting the effort
// explicitly here keeps it — the child re-clamps against the named model, so
// forwarding a tier it does not support is safe. A manifest that already names
// its own effort is left alone.
// autoTaskModel picks a per-role model for a Task that named none, but only
// under the zeromaxing posture with auto-assignment configured.
//
// It reuses the plan path's own classifier and role pins, so a delegated
// sub-agent is routed EXACTLY as the same work would be inside a plan: the grant
// outranks the prose (a write-capable Task is "implement"), and an unclassifiable
// task, or one whose role has no pin, returns "" and inherits the parent's model.
//
// OFF UNLESS ASKED FOR. Both the posture and the configured auto-assign default
// must be set; otherwise this returns "" before touching the classifier, so a
// posture-off spawn never reaches resolveTaskModel and stays byte-identical to
// before this existed. The classifier reads the tools the child will actually
// hold (resolvedToolAllowlist), and a resolution error there degrades to "" —
// a routing hint is never worth failing a spawn over.
func (executor Executor) autoTaskModel(ctx context.Context, manifest Manifest, prompt, parentModel string) string {
	if executor.PostureActive == nil || !executor.PostureActive() || !executor.ModelPrefs.AutoAssign {
		return ""
	}
	// A PLAN-AUTHORED MANIFEST ALREADY WENT THROUGH ASSIGNMENT. The plan tool
	// runs the router, the pins, the served-check and the probes, reports every
	// decision in its notes, and honours a per-plan auto_assign override — so a
	// plan task arriving here with no model is a DECISION (inherit), not an
	// absence. Second-guessing it would contradict the plan's own report, defeat
	// an explicit auto_assign:false, and re-apply a pin the plan level passed
	// over as not served. Provenance is already on the manifest; derived, never
	// carried as a second flag.
	if manifest.FilePath == planManifestFilePath {
		return ""
	}
	granted, err := resolvedToolAllowlist(manifest)
	if err != nil {
		return ""
	}
	pin := executor.ModelPrefs.pinned(classifyTaskRole(Task{Tools: granted, Prompt: prompt}))
	if pin == "" {
		return ""
	}
	// PROVED SERVED BEFORE IT IS APPLIED, fail-safe in every other case. The
	// plan path checks pins against discovery when discovery answers; this path
	// applied them blind, and a session that had moved to xai spawned three
	// children onto Ollama pins — each died with `"not-found"`. So the pin fires
	// only when the provider's own list carries BOTH the pin and the session's
	// model; the second check is the plan path's provider-mismatch guard, which
	// catches discovery answering for a different provider than the session is
	// actually on (including a stale cache across a provider switch). Anything
	// short of that — no discoverer, a failed listing, a mismatched list — is
	// an inherit, never a guess: a sub-agent on the session's model is slower
	// routing; a sub-agent dead at spawn is a failed task.
	served, ok := executor.servedModelSet(ctx)
	if !ok || len(served) == 0 {
		return ""
	}
	if parent := strings.TrimSpace(parentModel); parent != "" && !servedContains(served, parent) {
		return ""
	}
	if !servedContains(served, pin) {
		return ""
	}
	return pin
}

func applyTaskModel(manifest *Manifest, requested, parentEffort string) error {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return nil
	}
	resolved, err := resolveTaskModel(requested)
	if err != nil {
		return err
	}
	manifest.Metadata.Model = resolved
	// ONLY FOR MODELS THE REGISTRY CAN VOUCH FOR — the same gate the plan path
	// applies (planTaskReasoningEffort). The child clamps a forwarded effort
	// only for models it can look up; for anything else it passes the value to
	// the provider untouched, and a provider that does not take the parameter
	// rejects the whole request. Under zeromaxing the parent's effort is always
	// raised, so forwarding it unconditionally made EVERY Task that named an
	// uncurated model on such a provider die at spawn — a real orchestrator hit
	// exactly that, gave up, and ran all five sub-agents on the session's model.
	// An unknown model runs at the provider's default effort instead, which is
	// what it did before per-task models existed.
	if strings.TrimSpace(manifest.Metadata.ReasoningEffort) == "" && modelTakesExplicitEffort(resolved) {
		manifest.Metadata.ReasoningEffort = strings.TrimSpace(parentEffort)
	}
	return nil
}
