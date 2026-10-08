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
	"strconv"
	"strings"

	"github.com/Gitlawb/zero/internal/background"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/streamjson"
	"github.com/Gitlawb/zero/internal/tools"
)

const (
	sessionTagSpecialist     = "specialist"
	promptFileThresholdBytes = 4 * 1024
	maxSpecialistDepth       = 8 // hard cap to prevent infinite recursion/resource exhaustion via Task
)

const SessionTagSpecialist = sessionTagSpecialist

type NewSessionIDFunc func() (string, error)
type WritePromptFileFunc func(prompt string) (string, error)
type LoadFunc func(LoadOptions) (LoadResult, error)
type RunChildFunc func(ctx context.Context, binaryPath string, args []string, progress func(streamjson.Event)) (ChildRunResult, error)
type LaunchBackgroundFunc func(binaryPath string, args []string, outputFile string, onExit func(exitCode int)) (int, error)
type BackgroundManagerFunc func() (*background.Manager, error)

type Executor struct {
	NewSessionID          NewSessionIDFunc
	WritePromptFile       WritePromptFileFunc
	PromptFileMaxSize     int
	Load                  LoadFunc
	RunChild              RunChildFunc
	LaunchBackground      LaunchBackgroundFunc
	BinaryPath            string
	Paths                 Paths
	SessionStore          *sessions.Store
	BackgroundManager     *background.Manager
	BackgroundManagerFunc BackgroundManagerFunc
	BackgroundRuntime     *Runtime
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
	// Progress, when set, is called with each stream-json event emitted by the
	// child process while it runs. nil is a no-op.
	Progress func(streamjson.Event)
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
		return executor.runResume(ctx, params, options)
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
	return BuildArgsResult{Args: args, SessionID: sessionID, PromptFile: promptFile}, nil
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
	return BuildArgsResult{Args: args, SessionID: sessionID, PromptFile: promptFile}, nil
}

func (executor Executor) runFresh(ctx context.Context, params TaskParameters, options TaskRunOptions) (ExecResult, error) {
	manifest, err := executor.freshManifest(params)
	if err != nil {
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
	})
	if err != nil {
		return ExecResult{}, err
	}
	if params.RunInBackground {
		return executor.runBackground(ctx, built, manifest, params, options)
	}
	return executor.runBuiltArgs(ctx, built, manifest, params, options, "foreground", options.Progress)
}

// freshManifest resolves the manifest for a fresh run. A caller-supplied inline
// manifest (validated here) takes precedence over a registry lookup by name, so a
// caller with its own definition — the swarm launcher running a member whose
// agent type is not a registered specialist — can run without a registry entry.
func (executor Executor) freshManifest(params TaskParameters) (Manifest, error) {
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
	manifest, err := executor.loadManifest(specialistName)
	if err != nil {
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
	})
	if err != nil {
		return ExecResult{}, err
	}
	return executor.runBuiltArgs(ctx, built, manifest, params, options, "resume", options.Progress)
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
		Type:           "specialist",
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
		Description:     params.Description,
		ToolCallID:      options.ToolCallID,
		Mode:            "background",
		Background:      true,
	}
	executor.recordSpecialistStart(accounting)
	pid, err := executor.launchBackground(binaryPath, built.Args, outputFile, func(exitCode int) {
		status := background.StatusCompleted
		if exitCode != 0 {
			status = background.StatusError
		}
		_ = manager.MarkExited(built.SessionID, status, exitCode)
		if task, ok := manager.Get(built.SessionID); ok {
			summary := StreamResult{ExitCode: task.ExitCode}
			if data, err := os.ReadFile(task.OutputFile); err == nil {
				summary, _ = summarizeTaskData(string(data), task.ExitCode)
			}
			executor.recordBackgroundTaskAccounting(task, summary)
		}
		executor.cleanupBackgroundPromptFile(built.SessionID, built.PromptFile)
	})
	if err != nil {
		_ = manager.UpdateStatus(built.SessionID, background.StatusError, -1)
		executor.cleanupBackgroundPromptFile(built.SessionID, built.PromptFile)
		executor.recordSpecialistStop(accounting, StreamResult{ExitCode: -1}, "error", -1, err, false)
		return ExecResult{}, err
	}
	if pid > 0 {
		if err := manager.SetPID(built.SessionID, pid); err != nil {
			// The child is running but its PID was never recorded, so nothing can
			// track or stop it later. Kill it (and its process group) now, mark the
			// task errored, and record the stop so it cannot become an untracked
			// orphan. The dedup in recordSpecialistStop makes the later onExit
			// accounting a no-op.
			if killErr := background.TerminateProcess(pid); killErr != nil {
				// Don't mask a failed kill: a surviving orphan is worse than the
				// SetPID error alone, so surface both to the caller.
				err = errors.Join(err, fmt.Errorf("terminate orphaned pid %d: %w", pid, killErr))
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
		Description:     params.Description,
		ToolCallID:      options.ToolCallID,
		Mode:            mode,
		Background:      false,
	}
	executor.recordSpecialistStart(accounting)
	run, err := executor.runChild(ctx, binaryPath, built.Args, progress)
	if err != nil {
		exitCode := run.exitCodeOr(-1)
		summary := SummarizeStream(run.Events, exitCode)
		executor.recordSpecialistStop(accounting, summary, "error", summary.ExitCode, err, false)
		// Carry the child session id even on a post-start failure so a caller (the
		// swarm launcher -> FailWithSession) can still make the failed member
		// drillable; the session exists once the child has started.
		return ExecResult{SessionID: built.SessionID}, err
	}
	summary := SummarizeStream(run.Events, run.ExitCode)
	rolledUp := executor.rollUpSpecialistUsage(accounting, summary)
	executor.recordSpecialistStop(accounting, summary, summary.Status, summary.ExitCode, nil, rolledUp)
	return ExecResult{
		Result:    BuildFinalResult(run.Events, run.Stderr, run.ExitCode, run.Signal),
		SessionID: built.SessionID,
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

func (executor Executor) runChild(ctx context.Context, binaryPath string, args []string, progress func(streamjson.Event)) (ChildRunResult, error) {
	if executor.RunChild != nil {
		return executor.RunChild(ctx, binaryPath, append([]string(nil), args...), progress)
	}
	return runChildProcess(ctx, binaryPath, args, progress)
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

func appendModelArgs(args []string, manifest Manifest, parentModel string, parentReasoningEffort string) []string {
	resolvedModel := strings.TrimSpace(manifest.Metadata.Model)
	if resolvedModel == "" {
		resolvedModel = strings.TrimSpace(parentModel)
	}
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

func resolvedToolAllowlist(manifest Manifest) ([]string, error) {
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

func runChildProcess(ctx context.Context, binaryPath string, args []string, progress func(streamjson.Event)) (ChildRunResult, error) {
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
