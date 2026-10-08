package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/Gitlawb/zero/internal/redaction"
	"github.com/Gitlawb/zero/internal/sandbox"
	"github.com/Gitlawb/zero/internal/streamjson"
)

type Registry struct {
	mu         sync.RWMutex
	tools      map[string]Tool
	generation uint64
}

// RegistrySnapshot is one immutable registry generation. Tools is a detached,
// name-sorted slice: later registrations cannot change the snapshot observed by
// an in-flight agent turn.
type RegistrySnapshot struct {
	Tools      []Tool
	Generation uint64
}

type RunOptions struct {
	PermissionGranted bool
	PermissionMode    string
	Autonomy          string
	Sandbox           *sandbox.Engine
	ToolCallID        string
	SessionID         string
	Model             string
	ReasoningEffort   string
	Depth             int
	Cwd               string
	// FileTracker, when set, records the version of each file read or written this
	// session so write_file/edit_file can refuse to clobber a file that changed on
	// disk outside Zero since it was last read. nil disables the feature entirely
	// (the read/write tools behave exactly as before).
	FileTracker *FileTracker
	// DeferFileObservationCommit lets a caller with an additional output layer
	// commit exact-read credit after that final layer has run.
	DeferFileObservationCommit bool
	deferFileObservation       bool
	// EnabledTools / DisabledTools carry the run's operator tool filters so a
	// filter-aware tool (tool_search) never discloses or loads an operator-hidden
	// tool. They use the same allow/deny semantics as the agent's filter gate:
	// denied if listed in DisabledTools; if EnabledTools is non-empty, allowed
	// only when listed in it.
	EnabledTools  []string
	DisabledTools []string
	// Progress, when set, is called with each stream-json event emitted by a
	// specialist child process while it runs. Used by the TUI to show live
	// tool-call progress in the specialist card. nil is a no-op (the default
	// for every non-Task tool).
	Progress func(streamjson.Event)
	// Diagnostics, when set, returns a formatted language-diagnostics block for
	// a file a mutating tool just wrote ("" when clean or no server available).
	// edit_file/write_file append it to their output so the model sees an error
	// it introduced in the same turn instead of waiting for a later verification
	// pass. nil disables inline diagnostics.
	Diagnostics func(ctx context.Context, absPath string) string
}

type sandboxAwareTool interface {
	RunWithSandbox(ctx context.Context, args map[string]any, engine *sandbox.Engine) Result
}

type optionsAwareTool interface {
	RunWithOptions(ctx context.Context, args map[string]any, options RunOptions) Result
}

// deferredTool is an optional interface a tool implements to mark itself
// deferred-eligible: when many such tools are registered, the agent loop may
// withhold their full schema and advertise them via tool_search instead.
type deferredTool interface {
	Deferred() bool
}

func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// IsDeferred reports whether a tool opts into deferred loading. A tool is
// deferred-eligible only if it implements deferredTool and its Deferred()
// method returns true; tools that do not implement the interface are eager.
func IsDeferred(t Tool) bool {
	d, ok := t.(deferredTool)
	return ok && d.Deferred()
}

// deferralEligibleTool lets a tool keep counting toward the deferral threshold
// even when it is dynamically exposed eagerly (Deferred()==false). A tool whose
// Deferred() flips false at runtime — e.g. the swarm coordination tools once a
// swarm is active — would otherwise lower the global eligible count and risk
// dropping it below DeferThreshold, which would deactivate deferral for ALL
// tools (force-exposing every MCP schema). Implementing this keeps the count
// stable so un-deferring one tool can never force-expose others.
type deferralEligibleTool interface {
	DeferralEligible() bool
}

// IsDeferralEligible reports whether a tool counts toward the DeferThreshold.
// A currently-deferred tool always counts. A tool may ALSO opt in via
// DeferralEligible() to keep counting while exposed eagerly (see above). This is
// the count partitionTools uses for the active-gate; whether a tool is actually
// hidden is still decided by IsDeferred.
func IsDeferralEligible(t Tool) bool {
	if IsDeferred(t) {
		return true
	}
	d, ok := t.(deferralEligibleTool)
	return ok && d.DeferralEligible()
}

func (registry *Registry) Register(tool Tool) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.tools[tool.Name()] = tool
	registry.generation++
}

// RegisterBatch publishes a group of tools as one registry generation. Readers
// observe either the complete prior generation or the complete new one.
func (registry *Registry) RegisterBatch(batch []Tool) {
	if len(batch) == 0 {
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for _, tool := range batch {
		registry.tools[tool.Name()] = tool
	}
	registry.generation++
}

func (registry *Registry) Get(name string) (Tool, bool) {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	tool, ok := registry.tools[name]
	return tool, ok
}

func (registry *Registry) All() []Tool {
	return registry.Snapshot().Tools
}

// Snapshot returns a stable, sorted view of the registry at one generation.
func (registry *Registry) Snapshot() RegistrySnapshot {
	registry.mu.RLock()
	tools := make([]Tool, 0, len(registry.tools))
	for _, tool := range registry.tools {
		tools = append(tools, tool)
	}
	generation := registry.generation
	registry.mu.RUnlock()
	sort.Slice(tools, func(left, right int) bool {
		return tools[left].Name() < tools[right].Name()
	})
	return RegistrySnapshot{Tools: tools, Generation: generation}
}

// Clone returns an independent registry containing exactly one source snapshot.
func (registry *Registry) Clone() *Registry {
	clone := NewRegistry()
	if registry == nil {
		return clone
	}
	snapshot := registry.Snapshot()
	clonedTools := make([]Tool, 0, len(snapshot.Tools))
	for _, tool := range snapshot.Tools {
		if cloner, ok := tool.(interface{ cloneForRegistry(*Registry) Tool }); ok {
			tool = cloner.cloneForRegistry(clone)
		}
		clonedTools = append(clonedTools, tool)
	}
	clone.RegisterBatch(clonedTools)
	clone.generation = snapshot.Generation
	return clone
}

func (registry *Registry) Run(ctx context.Context, name string, args map[string]any) Result {
	return registry.RunWithOptions(ctx, name, args, RunOptions{})
}

func (registry *Registry) RunWithOptions(ctx context.Context, name string, args map[string]any, options RunOptions) (result Result) {
	// Every return path passes through scrubResultSecrets exactly once, so denial,
	// permission, and unknown-tool error messages (which can echo secret-bearing
	// args/paths) are redacted at the boundary just like tool output. The output
	// ceiling runs after the scrub so the transcript and the spill file agree on
	// what was hidden.
	commitFileObservation := !options.DeferFileObservationCommit
	options.deferFileObservation = true
	selfManagedOutput := false
	var tool Tool
	var ok bool
	defer func() {
		result = scrubResultSecrets(result)
		boundaryOutput := result.Output
		result = reduceCommandOutput(name, args, result)
		if selfManagedOutput {
			result = applySelfManagedOutputBudget(tool, name, args, result)
		} else {
			result = applyRegistryOutputBudget(tool, name, args, result)
			result = enforceOutputCeiling(name, result)
		}
		result = finalizeToolOutcome(result, boundaryOutput)
		if commitFileObservation {
			result = registry.CommitFileObservation(result, options.FileTracker)
		}
	}()

	tool, ok = registry.Get(name)
	if !ok {
		return errorResult(`Error: Unknown tool "` + name + `".`)
	}
	if _, ok := tool.(selfBudgeting); ok {
		selfManagedOutput = true
	}
	if rejecter, ok := tool.(PrePermissionRejecter); ok {
		if res, rejected := rejecter.RejectBeforePermission(args); rejected {
			return res
		}
	}

	permission := effectiveToolPermission(tool, args)
	sandboxGrantAuthorized := false
	var sandboxDecision *sandbox.Decision
	if options.Sandbox != nil {
		d := options.Sandbox.Evaluate(ctx, sandbox.Request{
			ToolName:          name,
			SideEffect:        sandbox.SideEffect(tool.Safety().SideEffect),
			Permission:        sandbox.Permission(permission),
			PermissionGranted: options.PermissionGranted,
			PermissionMode:    sandbox.PermissionMode(options.PermissionMode),
			Args:              args,
			Reason:            tool.Safety().Reason,
		})
		sandboxDecision = &d
		if d.Action == sandbox.ActionDeny {
			res := errorResult(d.ErrorString())
			res.SandboxDecision = sandboxDecision
			return res
		}
		if d.Action == sandbox.ActionPrompt && !options.PermissionGranted {
			res := errorResult("Error: Sandbox approval required for " + name + ": " + d.Reason)
			res.SandboxDecision = sandboxDecision
			return res
		}
		// A persistent grant OR a sandbox auto-allow authorizes a prompt tool to run
		// without a separately-recorded PermissionGranted; the sandbox is the safety
		// boundary.
		sandboxGrantAuthorized = d.Action == sandbox.ActionAllow && (d.GrantMatched || d.AutoAllowed)
	}

	switch permission {
	case PermissionAllow:
	case PermissionPrompt:
		if !options.PermissionGranted && !sandboxGrantAuthorized {
			res := errorResult("Error: Permission required for " + name + ": " + tool.Safety().Reason + ` The tool is marked "prompt" and was not executed.`)
			res.SandboxDecision = sandboxDecision
			return res
		}
	default:
		res := errorResult("Error: Permission denied for " + name + ": " + tool.Safety().Reason)
		res.SandboxDecision = sandboxDecision
		return res
	}

	if optioned, ok := tool.(optionsAwareTool); ok {
		res := optioned.RunWithOptions(ctx, args, options)
		if res.SandboxDecision == nil {
			res.SandboxDecision = sandboxDecision
		}
		return res
	}

	if options.Sandbox != nil {
		if sandboxed, ok := tool.(sandboxAwareTool); ok {
			res := sandboxed.RunWithSandbox(ctx, args, options.Sandbox)
			res.SandboxDecision = sandboxDecision
			return res
		}
	}
	res := tool.Run(ctx, args)
	res.SandboxDecision = sandboxDecision
	return res
}

// CommitFileObservation grants exact-read credit only when the tool's complete
// output remains intact at the final boundary that will be sent to the model.
func (registry *Registry) CommitFileObservation(result Result, tracker *FileTracker) Result {
	observation := result.pendingFileObservation
	result.pendingFileObservation = nil
	if observation == nil || tracker == nil || result.Status != StatusOK || result.Truncated ||
		!strings.HasPrefix(result.Output, observation.output) {
		return result
	}
	if observation.byteMode {
		tracker.RecordSeenBytes(observation.path, observation.start, observation.end, observation.total)
	} else {
		tracker.RecordSeenRange(observation.path, observation.start, observation.end, observation.total)
	}
	if result.Meta == nil {
		result.Meta = map[string]string{}
	}
	result.Meta["file_version"] = observation.hash
	if observation.byteMode {
		result.Meta["seen_bytes"] = fmt.Sprintf("%d-%d", observation.start, observation.end)
	} else {
		result.Meta["seen_lines"] = fmt.Sprintf("%d-%d", observation.start, observation.end)
	}
	return result
}

func effectiveToolPermission(tool Tool, args map[string]any) Permission {
	if permissioner, ok := tool.(ArgsPermissioner); ok {
		return permissioner.PermissionForArgs(args)
	}
	return tool.Safety().Permission
}

// scrubResultSecrets redacts known secret forms from a tool result's Output at
// the registry boundary, so a tool can never leak a secret into the transcript.
func scrubResultSecrets(res Result) Result {
	if scrubbed := redaction.RedactString(res.Output, redaction.Options{}); scrubbed != res.Output {
		res.Output = scrubbed
		res.Redacted = true
	}
	// Display.Summary can echo command/output fragments, so scrub it too: a caller
	// that prefers Display must not bypass the boundary redaction.
	if scrubbed := redaction.RedactString(res.Display.Summary, redaction.Options{}); scrubbed != res.Display.Summary {
		res.Display.Summary = scrubbed
		res.Redacted = true
	}
	// Display.Preview is a file head / diff that can contain secrets, so scrub it at
	// the same boundary as Output/Summary before it reaches the card.
	if scrubbed := redaction.RedactString(res.Display.Preview, redaction.Options{}); scrubbed != res.Display.Preview {
		res.Display.Preview = scrubbed
		res.Redacted = true
	}
	fileDiffs := make([]FileDiff, 0, len(res.FileDiffs))
	for _, diff := range res.FileDiffs {
		// Never normalize control bytes in a diff: normalizing after redaction can
		// reassemble a split credential. Decline unsafe rich content entirely and
		// leave ChangedFiles as the safe fallback.
		if unsafeDiffText(diff.OldText) || unsafeDiffText(diff.NewText) {
			res.Redacted = true
			continue
		}
		if scrubbed := redaction.RedactString(diff.OldText, redaction.Options{}); scrubbed != diff.OldText {
			diff.OldText = scrubbed
			res.Redacted = true
		}
		if scrubbed := redaction.RedactString(diff.NewText, redaction.Options{}); scrubbed != diff.NewText {
			diff.NewText = scrubbed
			res.Redacted = true
		}
		// Redaction can collapse two distinct credentials to the same token. At
		// this final outbound boundary, retain rich evidence only when the
		// transformed sides still describe a real transition; ChangedFiles
		// remains the safe path-only fallback.
		if diff.OldExists == diff.NewExists && diff.OldText == diff.NewText {
			continue
		}
		fileDiffs = append(fileDiffs, diff)
	}
	res.FileDiffs = fileDiffs
	// Meta values carry model-controlled strings (e.g. glob pattern, bash cwd) and
	// are forwarded into the transcript, so they are part of the boundary too.
	for key, value := range res.Meta {
		if scrubbed := redaction.RedactString(value, redaction.Options{}); scrubbed != value {
			res.Meta[key] = scrubbed
			res.Redacted = true
		}
	}
	return res
}

// ScrubResultSecrets applies the registry's transcript boundary to a result
// that was produced before Registry.RunWithOptions could own it, such as a
// local pre-permission rejection in the agent loop.
func ScrubResultSecrets(res Result) Result {
	return scrubResultSecrets(res)
}

func CoreReadOnlyToolsScoped(workspaceRoot string, scope PathScope) []Tool {
	return []Tool{
		NewScopedReadMinifiedFileTool(workspaceRoot, scope),
		NewScopedReadFileTool(workspaceRoot, scope),
		// view_image is read-only and shares read_file's path scoping, so it
		// belongs with the other readers rather than behind a gate.
		NewScopedViewImageTool(workspaceRoot, scope),
		NewScopedListDirectoryTool(workspaceRoot, scope),
		NewScopedGlobTool(workspaceRoot, scope),
		NewScopedGrepTool(workspaceRoot, scope),
		// lsp_navigate is semantic code navigation (definition/references/impl/
		// workspace_symbol) via the language server; read-only, scoped to the
		// workspace, and degrades to a clear "unavailable" when no server is
		// installed for the file type.
		NewScopedLSPNavigateTool(workspaceRoot, scope),
		// skill reads reusable instruction files from the skills dir (it resolves
		// skills.DefaultDir itself); read-only, so it is safe in the core/MCP set.
		NewSkillTool(""),
		NewAskUserTool(),
		NewRequestPermissionsTool(),
	}
}

func CoreWriteToolsScoped(workspaceRoot string, scope PathScope) []Tool {
	return []Tool{
		NewScopedWriteFileTool(workspaceRoot, scope),
		NewScopedEditFileTool(workspaceRoot, scope),
		NewScopedApplyPatchTool(workspaceRoot, scope),
		NewUpdatePlanTool(),
	}
}

func CoreShellToolsScoped(workspaceRoot string, scope PathScope) []Tool {
	execManager := newExecSessionManager()
	return []Tool{
		NewScopedExecCommandTool(workspaceRoot, scope, execManager),
		NewWriteStdinTool(execManager),
		NewScopedBashTool(workspaceRoot, scope),
	}
}

func CoreNetworkTools() []Tool {
	tools := []Tool{NewWebFetchTool()}
	// Only offer the built-in web_search when a backend is actually configured.
	// Registering it unconfigured makes the model waste calls (and high-risk
	// permission prompts) on a tool that can only return "no backend configured";
	// an MCP-provided search tool (e.g. Exa) stands on its own without it.
	if defaultSearchBackend() != nil {
		tools = append(tools, NewWebSearchTool())
	}
	return tools
}

func CoreToolsScoped(workspaceRoot string, scope PathScope) []Tool {
	tools := append([]Tool{}, CoreReadOnlyToolsScoped(workspaceRoot, scope)...)
	tools = append(tools, CoreWriteToolsScoped(workspaceRoot, scope)...)
	tools = append(tools, CoreShellToolsScoped(workspaceRoot, scope)...)
	tools = append(tools, CoreNetworkTools()...)
	return tools
}
