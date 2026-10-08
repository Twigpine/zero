package acp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/providercatalog"
	"github.com/Gitlawb/zero/internal/providermodelcatalog"
	"github.com/Gitlawb/zero/internal/providermodeldiscovery"
	"github.com/Gitlawb/zero/internal/redaction"
	"github.com/Gitlawb/zero/internal/sandbox"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// Deps are the ZERO capabilities the ACP Agent drives. The CLI fills these with
// real implementations; tests inject fakes (e.g. a canned provider) to drive the
// full ACP flow without a live model. Keeping auth/model/keys behind these deps
// means the editor only hosts the thread — ZERO owns BYOK and telemetry-free
// operation.
type Deps struct {
	ResolveConfig  func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error)
	DiscoverModels func(context.Context, config.ProviderProfile) ([]providermodeldiscovery.Model, error)
	NewProvider    func(profile config.ProviderProfile) (zeroruntime.Provider, error)
	RunAgent       func(ctx context.Context, prompt string, provider zeroruntime.Provider, opts agent.Options) (agent.Result, error)
	// BuildWorkspace creates the per-turn scoped tool workspace for a validated
	// workspace root. Its Close method releases any resources the registry owns
	// (notably MCP server connections) after the turn completes or is cancelled.
	// Shell tools remain confined exactly like the exec surface — never unconfined
	// on the host.
	BuildWorkspace func(ctx context.Context, workspaceRoot string, resolved config.ResolvedConfig, mode agent.PermissionMode) (*Workspace, error)
	// ResolveWorkspaceRoot validates + normalizes a client-supplied cwd (must be an
	// existing directory; never the bare root). It is the file-tool confinement root.
	ResolveWorkspaceRoot func(cwd string) (string, error)
	// PersistEvents is an optional test seam for injecting event-store failures.
	// It receives a COMPLETED TURN as one batch, never one event at a time: the
	// batch is the unit the shared store keeps contiguous (see persistTurn).
	PersistEvents func(sessionID string, inputs []sessions.AppendEventInput) error
	Store         *sessions.Store
	AgentInfo     Implementation
}

// Workspace is the per-turn execution environment passed to the agent. ACP does
// not retain it across turns because MCP connections belong to the registry that
// advertised their tools; keeping a stale registry after a cancelled turn would
// leak its server process and its permission state into the next turn.
type Workspace struct {
	Registry       *tools.Registry
	Sandbox        *sandbox.Engine
	DeferThreshold int
	Notices        []string
	Cleanup        func() error
}

// Close releases resources created with the workspace. A nil cleanup is valid
// for core-only workspaces.
func (w *Workspace) Close() error {
	if w == nil || w.Cleanup == nil {
		return nil
	}
	return w.Cleanup()
}

// Agent is the ACP agent server bound to one JSON-RPC connection (one editor).
type Agent struct {
	conn *Conn
	deps Deps

	mu          sync.Mutex
	clientCaps  ClientCapabilities
	initialized bool
	sessions    map[string]*acpSession
}

type turnRecord struct {
	user      string
	assistant string
}

type acpSession struct {
	id  string
	cwd string

	// turnMu serializes prompt turns for one session: concurrent session/prompt
	// calls run one at a time so they can't interleave history or clobber the
	// single cancel slot.
	turnMu  sync.Mutex
	modelMu sync.Mutex

	mu             sync.Mutex
	mode           agent.PermissionMode
	model          string
	models         []SessionConfigOptionValue
	restrictModels bool
	cancel         context.CancelFunc
	history        []turnRecord
}

// NewAgent builds the ACP server and registers its method handlers on conn.
func NewAgent(conn *Conn, deps Deps) *Agent {
	a := &Agent{conn: conn, deps: deps, sessions: make(map[string]*acpSession)}
	conn.Handle(MethodInitialize, a.handleInitialize)
	conn.Handle(MethodSessionNew, a.handleSessionNew)
	conn.Handle(MethodSessionLoad, a.handleSessionLoad)
	conn.Handle(MethodSessionList, a.handleSessionList)
	conn.Handle(MethodSessionResume, a.handleSessionResume)
	conn.Handle(MethodSessionPrompt, a.handleSessionPrompt)
	conn.Handle(MethodSessionSetMode, a.handleSetMode)
	conn.Handle(MethodSessionSetConfigOption, a.handleSetConfigOption)
	conn.Handle(MethodZeroSetModel, a.handleZeroSetModel)
	conn.HandleNotify(MethodSessionCancel, a.handleCancel)
	return a
}

// ---- initialize ----

func (a *Agent) handleInitialize(_ context.Context, params json.RawMessage) (any, error) {
	var p InitializeParams
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	negotiated := ProtocolVersion
	if p.ProtocolVersion > 0 && p.ProtocolVersion < ProtocolVersion {
		negotiated = p.ProtocolVersion
	}
	a.mu.Lock()
	a.clientCaps = p.ClientCapabilities
	a.initialized = true
	a.mu.Unlock()

	info := a.deps.AgentInfo
	return InitializeResult{
		ProtocolVersion: negotiated,
		AgentCapabilities: AgentCapabilities{
			// ACP v1 optional methods are advertised as empty capability objects;
			// clients must gate session/list and session/resume on their presence.
			LoadSession:         true,
			PromptCapabilities:  PromptCapabilities{Image: true},
			SessionCapabilities: &SessionCapabilities{List: &struct{}{}, Resume: &struct{}{}},
		},
		AgentInfo: &info,
		// ZERO owns credentials (BYOK) and does not delegate auth to the editor.
		AuthMethods: []AuthMethod{},
	}, nil
}

// ---- session lifecycle ----

// requestedWorkspace is the single rule for a cwd that arrived from the client,
// applied before the value can reach ResolveWorkspaceRoot.
//
// THE REQUEST'S OWN CWD IS THE ONLY THING ALLOWED TO PICK A ROOT. Every params
// type spells cwd as a plain string, so an absent field and an empty one decode
// to exactly the same "", and the resolver treats "" as "use the directory this
// process was started in" and joins a relative cwd onto it. Those two defaults
// meet in the middle: `{"sessionId":"known"}` used to activate a persisted
// session, and `{}` used to create one, both rooted at wherever the editor
// happened to spawn `zero acp` — which then becomes the sandbox and file-tool
// confinement root. The client never named that directory, so it is a root the
// server invented, and ACP requires an absolute cwd on every method carrying
// one. Rejecting here is what keeps a missing field from being answered with a
// guess.
func requestedWorkspace(cwd string) (string, error) {
	trimmed := strings.TrimSpace(cwd)
	if trimmed == "" {
		return "", RPCError(codeInvalidParams, "cwd is required and must be an absolute path")
	}
	if !filepath.IsAbs(trimmed) {
		return "", RPCError(codeInvalidParams, "cwd must be an absolute path: "+trimmed)
	}
	return trimmed, nil
}

func (a *Agent) handleSessionNew(ctx context.Context, params json.RawMessage) (any, error) {
	var p NewSessionParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, RPCError(codeInvalidParams, "invalid session/new params")
	}
	cwd, err := requestedWorkspace(p.Cwd)
	if err != nil {
		return nil, err
	}
	root, err := a.deps.ResolveWorkspaceRoot(cwd)
	if err != nil {
		return nil, RPCError(codeInvalidParams, err.Error())
	}
	model, models, restrictModels, err := a.resolveModelChoices(ctx, root)
	if err != nil {
		return nil, RPCError(codeInternalError, "config: "+err.Error())
	}
	meta, err := a.deps.Store.Create(sessions.CreateInput{Title: "ACP session", Cwd: root, ModelID: model})
	if err != nil {
		return nil, RPCError(codeInternalError, "create session: "+err.Error())
	}
	sess, _ := a.registerSession(meta.SessionID, root, nil, model, models, restrictModels)
	return NewSessionResult{
		SessionID:     sess.id,
		ConfigOptions: a.configOptions(sess),
		Modes:         a.modeState(sess),
	}, nil
}

func (a *Agent) handleSessionLoad(ctx context.Context, params json.RawMessage) (any, error) {
	var p LoadSessionParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, RPCError(codeInvalidParams, "invalid session/load params")
	}
	return a.activatePersistedSession(ctx, p, persistedSessionLoad)
}

func (a *Agent) handleSessionResume(ctx context.Context, params json.RawMessage) (any, error) {
	var p ResumeSessionParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, RPCError(codeInvalidParams, "invalid session/resume params")
	}
	return a.activatePersistedSession(ctx, p, persistedSessionResume)
}

type persistedSessionOperation uint8

const (
	persistedSessionLoad persistedSessionOperation = iota
	persistedSessionResume
)

// activatePersistedSession restores the agent's internal conversation context
// for both lifecycle methods. session/load additionally replays user-visible
// history as ordered session/update notifications; session/resume deliberately
// does not, which makes it safe for an already-rendered desktop reconnect.
func (a *Agent) activatePersistedSession(ctx context.Context, p LoadSessionParams, operation persistedSessionOperation) (any, error) {
	// BOTH METHODS, BEFORE ANYTHING IS LOOKED UP. session/resume shares this
	// params type with session/load, so the wire cannot tell an omitted cwd from
	// an empty one; the blank case used to be answered with the persisted cwd,
	// which made {"sessionId":"known"} a complete, successful activation request.
	// Validating here rather than in one handler is what keeps the two entry
	// points from drifting apart.
	cwd, err := requestedWorkspace(p.Cwd)
	if err != nil {
		return nil, err
	}
	meta, err := a.deps.Store.Get(p.SessionID)
	if err != nil || meta == nil {
		return nil, RPCError(codeInvalidParams, "session not found: "+p.SessionID)
	}
	if operation == persistedSessionResume && !sessions.IsResumableKind(meta.SessionKind) {
		return nil, RPCError(codeInvalidParams, "session is not resumable: "+p.SessionID)
	}
	if strings.TrimSpace(meta.Cwd) == "" {
		return nil, RPCError(codeInvalidParams, "session has no persisted workspace: "+p.SessionID)
	}
	// SAME RULE ON THE WAY IN. Omitting a relative entry from the listing is not
	// enough: a stored "." still resolves against this process's directory, so a
	// client that hands back that same directory as its cwd would be sold the
	// invented root as a match. A workspace that cannot be identified is not one
	// this session can be restored into.
	if !filepath.IsAbs(meta.Cwd) {
		return nil, RPCError(codeInvalidParams, "session workspace is not an absolute path, so it cannot be identified: "+p.SessionID)
	}
	persistedRoot, err := a.deps.ResolveWorkspaceRoot(meta.Cwd)
	if err != nil {
		return nil, RPCError(codeInvalidParams, "persisted session workspace is unavailable: "+err.Error())
	}
	root, err := a.deps.ResolveWorkspaceRoot(cwd)
	if err != nil {
		return nil, RPCError(codeInvalidParams, err.Error())
	}
	// ACP session cwd is immutable. Loading history under a different root
	// would give a conversation from one workspace access to another
	// workspace's configuration, files, and tools.
	if !sameWorkspace(root, persistedRoot) {
		return nil, RPCError(codeInvalidParams, "session cwd does not match its persisted workspace")
	}
	// Load history BEFORE publishing the session so no concurrent prompt observes
	// a half-initialized session (registerSession sets history under the lock and
	// reuses an already-live session rather than orphaning its in-flight turn).
	requireHistoryLog := operation == persistedSessionResume && meta.EventCount > 0
	history, messages, historyWarning, historyErr := a.loadHistory(meta.SessionID, requireHistoryLog)
	// RESUME PROMISES RESTORED CONTEXT, SO A FAILED RESTORATION IS A FAILED
	// RESUME. historyErr used to do nothing but suppress replay and raise a
	// warning: the session was registered and reported ready regardless, so a
	// corrupt record or an unreadable events file left the caller holding a live,
	// promptable session ID whose next prompt ran as a brand-new conversation
	// under the old identity. Nothing in the response said so.
	//
	// Publication is now gated on restoration for resume. LOAD keeps the
	// best-effort policy deliberately rather than by inheriting this helper: it
	// replays what it has and warns about the rest, which is the right trade for
	// a client rebuilding a transcript it can still scroll. Reported by @jatmn.
	if operation == persistedSessionResume && historyErr != nil {
		return nil, RPCError(codeInternalError, "restore session history: "+historyErr.Error())
	}
	model, models, restrictModels, err := a.resolveModelChoices(ctx, root)
	if err != nil {
		return nil, RPCError(codeInternalError, "config: "+err.Error())
	}
	if persistedModel := strings.TrimSpace(meta.ModelID); persistedModel != "" && (!restrictModels || modelChoiceExists(models, persistedModel)) {
		model = persistedModel
		if !modelChoiceExists(models, persistedModel) {
			models = append(models, SessionConfigOptionValue{Value: persistedModel, Name: persistedModel})
		}
	}
	sess, existed := a.registerSession(meta.SessionID, root, history, model, models, restrictModels)
	if existed {
		_, messages, historyWarning, historyErr = a.refreshSessionHistory(sess, requireHistoryLog)
		if operation == persistedSessionResume && historyErr != nil {
			return nil, RPCError(codeInternalError, "restore session history: "+historyErr.Error())
		}
	}
	note := &notifier{conn: a.conn, sessionID: sess.id}
	if operation == persistedSessionLoad && historyErr == nil {
		for _, message := range messages {
			if message.tool != nil {
				note.send(*message.tool)
				continue
			}
			note.send(replayMessageChunk(message.role, replayMessageID(message.eventID), message.content))
		}
	}
	a.warnPersistence(
		note,
		"rehydrate session compaction",
		"Could not apply session compaction. Raw session history was restored instead.",
		historyWarning,
		operation == persistedSessionLoad,
	)
	a.warnPersistence(
		note,
		"load session history",
		"Could not load session history. The session is open, but earlier turns may be missing until storage recovers.",
		historyErr,
		operation == persistedSessionLoad,
	)
	return LoadSessionResult{
		ConfigOptions: a.configOptions(sess),
		Modes:         a.modeState(sess),
	}, nil
}

func (a *Agent) handleSessionList(_ context.Context, params json.RawMessage) (any, error) {
	var p ListSessionsParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, RPCError(codeInvalidParams, "invalid session/list params")
		}
	}
	if p.Cursor != "" {
		return nil, RPCError(codeInvalidParams, "invalid session/list cursor")
	}
	items, err := a.deps.Store.ListResumable()
	if err != nil {
		return nil, RPCError(codeInternalError, "list sessions: "+err.Error())
	}
	// The filter is the one cwd ACP leaves optional, so a blank one means "no
	// filter" rather than an error — but a relative one is rebased onto this
	// process's directory exactly like the others, and would then silently answer
	// about a workspace the client never named.
	var cwd string
	if strings.TrimSpace(p.Cwd) != "" {
		filter, err := requestedWorkspace(p.Cwd)
		if err != nil {
			return nil, err
		}
		cwd, err = a.deps.ResolveWorkspaceRoot(filter)
		if err != nil {
			return nil, RPCError(codeInvalidParams, err.Error())
		}
	}
	result := ListSessionsResult{Sessions: make([]SessionInfo, 0, len(items))}
	for _, item := range items {
		// EVERY ENTRY IS RESOLVED, FILTER OR NO FILTER. Listing is a menu, and
		// every item on it has to be orderable: activatePersistedSession
		// resolves the persisted workspace and refuses what it cannot reach, so
		// anything this loop cannot resolve is something the client would be
		// offered and then denied.
		//
		// Resolving only when a cwd filter was supplied left two shapes through —
		// a session whose workspace has since been deleted, and a legacy entry
		// holding a relative path, which was then reported as cwd "." even though
		// ACP requires SessionInfo.cwd to be absolute.
		if strings.TrimSpace(item.Cwd) == "" {
			continue
		}
		// A RELATIVE PERSISTED CWD HAS NO RECOVERABLE IDENTITY. Resolving one
		// rebases it onto wherever this ACP server happens to be running and
		// advertises that invented absolute path as the session's workspace — so a
		// conversation created for one project could be resumed against another
		// project's configuration, files and tools. The original base is not
		// knowable from the metadata, so the honest answer is to omit the entry
		// rather than to guess at it.
		if !filepath.IsAbs(item.Cwd) {
			continue
		}
		itemRoot, err := a.deps.ResolveWorkspaceRoot(item.Cwd)
		if err != nil {
			continue
		}
		if cwd != "" && !sameWorkspace(itemRoot, cwd) {
			continue
		}
		result.Sessions = append(result.Sessions, SessionInfo{
			SessionID: item.SessionID,
			Title:     item.Title,
			// The RESOLVED root, not the stored string: absolute as ACP
			// requires, and the same value the client will hand back on resume.
			Cwd:       itemRoot,
			UpdatedAt: item.UpdatedAt,
			Meta:      &SessionInfoMeta{ModelID: item.ModelID, CreatedAt: item.CreatedAt},
		})
	}
	return result, nil
}

// ---- prompt turn ----

func (a *Agent) handleSessionPrompt(ctx context.Context, params json.RawMessage) (any, error) {
	var p PromptParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, RPCError(codeInvalidParams, "invalid session/prompt params")
	}
	sess := a.session(p.SessionID)
	if sess == nil {
		return nil, RPCError(codeInvalidParams, "unknown session: "+p.SessionID)
	}

	// Serialize turns for this session so two prompts can't interleave history or
	// fight over the single cancel slot. session/cancel still works concurrently
	// (it doesn't take turnMu).
	sess.turnMu.Lock()
	defer sess.turnMu.Unlock()

	userText := promptText(p.Prompt)
	images := promptImages(p.Prompt)

	turnCtx, cancel := context.WithCancel(ctx)
	sess.setCancel(cancel)
	defer func() {
		cancel()
		sess.setCancel(nil)
	}()

	reason, err := a.runTurn(turnCtx, sess, userText, images)
	if err != nil {
		return nil, err
	}
	return PromptResult{StopReason: reason}, nil
}

func (a *Agent) runTurn(ctx context.Context, sess *acpSession, userText string, images []zeroruntime.ImageBlock) (string, error) {
	overrides := config.Overrides{}
	if model := sess.currentModel(); model != "" {
		overrides.Provider.Model = model
	}
	resolved, err := a.deps.ResolveConfig(sess.cwd, overrides)
	if err != nil {
		return "", RPCError(codeInternalError, "config: "+err.Error())
	}
	provider, err := a.deps.NewProvider(resolved.Provider)
	if err != nil {
		return "", RPCError(codeInternalError, "provider: "+err.Error())
	}
	mode := sess.currentMode()
	// Build the SCOPED registry + sandbox engine for this session's workspace so
	// shell/file tools are confined to the workspace exactly like the exec surface.
	// The workspace can also own MCP connections, which must be closed after every
	// turn even if the agent run returns an error or the client cancels it.
	workspace, err := a.deps.BuildWorkspace(ctx, sess.cwd, resolved, mode)
	if err != nil {
		return "", RPCError(codeInternalError, "workspace: "+err.Error())
	}
	if workspace == nil || workspace.Registry == nil {
		if workspace != nil {
			_ = workspace.Close()
		}
		return "", RPCError(codeInternalError, "workspace: missing tool registry")
	}
	defer func() {
		if err := workspace.Close(); err != nil {
			log.Printf("acp: close workspace: %v", err)
		}
	}()
	registry := workspace.Registry
	sandboxEngine := workspace.Sandbox
	note := &notifier{conn: a.conn, sessionID: sess.id}
	for _, notice := range workspace.Notices {
		if text := strings.TrimSpace(redaction.RedactString(notice, redaction.Options{})); text != "" {
			note.text("\n\n[zero warning] " + text + "\n")
		}
	}
	// Buffer the complete turn until RunAgent has a valid ACP outcome. A hard run
	// failure is an aborted turn: neither live history nor durable history may
	// retain its user/tool prefix under an RPC error. StopCancelled is a valid
	// outcome and commits through the same boundary below.
	pendingEvents := make([]sessions.AppendEventInput, 0, 4)
	queue := func(input sessions.AppendEventInput) {
		pendingEvents = append(pendingEvents, input)
	}
	queue(messageEvent("user", userText))

	opts := agent.Options{
		Cwd:            sess.cwd,
		SessionID:      sess.id,
		ProviderName:   resolved.Provider.Name,
		Model:          resolved.Provider.Model,
		Registry:       registry,
		Sandbox:        sandboxEngine,
		PermissionMode: mode,
		DeferThreshold: workspace.DeferThreshold,
		MaxTurns:       resolved.MaxTurns,
		Images:         images,
		OnText:         note.text,
		OnReasoning:    note.thought,
		OnToolCall: func(call agent.ToolCall) {
			queue(toolCallEvent(call))
			note.toolCall(call)
		},
		OnToolResult: func(result agent.ToolResult) {
			queue(toolResultEvent(result))
			note.toolResult(result)
			if result.Name == "update_plan" {
				a.emitPlan(registry, note)
			}
		},
		OnPermissionRequest: func(ctx context.Context, req agent.PermissionRequest) (agent.PermissionDecision, error) {
			return a.requestPermission(ctx, sess.id, req)
		},
	}

	agentPrompt := buildPrompt(sess.snapshotHistory(), userText)
	result, runErr := a.deps.RunAgent(ctx, agentPrompt, provider, opts)
	if result.FinalAnswer != "" {
		queue(messageEvent("assistant", result.FinalAnswer))
	}
	reason, stopErr := stopReasonFor(result, runErr)
	if stopErr != nil {
		return "", RPCError(codeInternalError, stopErr.Error())
	}
	// ONE BATCH, ONE LOCK. The outcome gate above decides WHETHER the turn is
	// committed; the store batch decides WHICH RECORDS STAY TOGETHER relative to
	// other writers, and these are different guarantees. Persisting the buffer
	// one event per call took and released the store's session lock between
	// events, so a second ACP instance holding the same session — its turnMu is
	// its own, only the store is shared — could append its whole turn in the
	// gap: user A, user B, answer B, answer A. Every write was individually
	// locked and the log was valid, but a fresh load pairs prose by order and
	// reconstructed three wrong turns from two right ones. Before this branch,
	// persistTurn handed the user and assistant events to AppendEvents together;
	// buffering until the outcome is known was correct, splitting the commit
	// into singleton writes was not. The store writes a batch under one lock and
	// one file write, which is the contiguity restored here — not a disk
	// transaction, and not a promise about crashes mid-fsync. Reported by @jatmn.
	persistenceErr := a.persistTurn(sess.id, pendingEvents)
	sess.appendHistory(turnRecord{user: userText, assistant: result.FinalAnswer})
	a.warnPersistence(
		note,
		"save session history",
		"Could not save session history. This turn is available in memory, but future resume may miss it until storage recovers.",
		persistenceErr,
		true,
	)
	return reason, nil
}

func stopReasonFor(result agent.Result, err error) (string, error) {
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return StopCancelled, nil
		}
		// A CLIENT THAT CANCELLED A PERMISSION PROMPT DID NOT FAIL. Only
		// context.Canceled was matched here, so dismissing a permission dialog —
		// a deliberate action, and for apply_patch the only refusal a client is
		// offered — came back as JSON-RPC -32603 carrying the internal sentinel
		// text. Editors and the desktop app both render that as a crashed turn,
		// so declining a tool looked like ZERO falling over. It is a
		// cancellation, and StopCancelled is what ACP has for saying so.
		if errors.Is(err, agent.ErrPermissionApprovalCanceled) {
			return StopCancelled, nil
		}
		return "", err
	}
	if result.FinishReason == "length" {
		return StopMaxTokens, nil
	}
	if result.FinishReason == "content_filter" {
		return StopRefusal, nil
	}
	return StopEndTurn, nil
}

// requestPermission forwards a ZERO permission prompt to the client as an ACP
// session/request_permission request and maps the outcome back. Failure to reach
// the client fails closed to deny.
func (a *Agent) requestPermission(ctx context.Context, sessionID string, req agent.PermissionRequest) (agent.PermissionDecision, error) {
	// Built ONCE and used for both halves. Sending one list and validating the
	// reply against another is the defect this whole path had; see
	// decisionFromOutcome.
	options := buildPermissionOptions(req)
	params := RequestPermissionParams{
		SessionID: sessionID,
		ToolCall:  permissionToolCall(req),
		Options:   options,
	}
	var result RequestPermissionResult
	if err := a.conn.Call(ctx, MethodSessionRequestPerm, params, &result); err != nil {
		if errors.Is(err, context.Canceled) {
			return agent.PermissionDecision{Action: agent.PermissionDecisionCancel, Reason: "cancelled"}, nil
		}
		return agent.PermissionDecision{Action: agent.PermissionDecisionDeny, Reason: "permission request failed: " + err.Error()}, nil
	}
	return decisionFromOutcome(result.Outcome, options), nil
}

func (a *Agent) emitPlan(registry *tools.Registry, note *notifier) {
	t, ok := registry.Get("update_plan")
	if !ok {
		return
	}
	planner, ok := t.(interface{ CurrentPlan() []tools.PlanItem })
	if !ok {
		return
	}
	note.plan(planner.CurrentPlan())
}

// ---- mode + model selection ----

func (a *Agent) handleSetMode(_ context.Context, params json.RawMessage) (any, error) {
	var p SetSessionModeParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, RPCError(codeInvalidParams, "invalid set_mode params")
	}
	sess := a.session(p.SessionID)
	if sess == nil {
		return nil, RPCError(codeInvalidParams, "unknown session: "+p.SessionID)
	}
	sess.turnMu.Lock()
	defer sess.turnMu.Unlock()
	mode := agent.PermissionMode(p.ModeID)
	switch mode {
	case agent.PermissionModeAuto, agent.PermissionModeAsk, agent.PermissionModePlan:
		sess.setMode(mode)
		(&notifier{conn: a.conn, sessionID: sess.id}).currentMode(string(mode))
		return SetSessionModeResult{}, nil
	case agent.PermissionModeUnsafe:
		// Unsafe = run every tool with no prompt. The TUI gates this behind an
		// explicit --skip-permissions-unsafe operator flag; an editor client must
		// not be able to grant itself unconfined, no-prompt access over the wire.
		return nil, RPCError(codeInvalidParams, "mode not permitted over ACP: "+p.ModeID)
	default:
		return nil, RPCError(codeInvalidParams, "unknown mode: "+p.ModeID)
	}
}

func (a *Agent) handleSetConfigOption(_ context.Context, params json.RawMessage) (any, error) {
	var p SetSessionConfigOptionParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, RPCError(codeInvalidParams, "invalid set_config_option params")
	}
	sess := a.session(p.SessionID)
	if sess == nil {
		return nil, RPCError(codeInvalidParams, "unknown session: "+p.SessionID)
	}
	switch p.ConfigID {
	case configIDModel:
		model := strings.TrimSpace(p.Value)
		if err := a.updateModel(sess, model, sess.restrictModels); err != nil {
			return nil, err
		}
	case configIDMode:
		// Same turnMu as handleSetMode so the two advertised mode doors (session
		// set_mode and set_config_option) serialize mode flips consistently.
		sess.turnMu.Lock()
		defer sess.turnMu.Unlock()
		mode := agent.PermissionMode(p.Value)
		switch mode {
		case agent.PermissionModeAuto, agent.PermissionModeAsk, agent.PermissionModePlan:
			sess.setMode(mode)
			(&notifier{conn: a.conn, sessionID: sess.id}).currentMode(string(mode))
		case agent.PermissionModeUnsafe:
			return nil, RPCError(codeInvalidParams, "mode not permitted over ACP: "+p.Value)
		default:
			return nil, RPCError(codeInvalidParams, "unknown mode: "+p.Value)
		}
	default:
		return nil, RPCError(codeInvalidParams, "unknown config option: "+p.ConfigID)
	}
	return SetSessionConfigOptionResult{ConfigOptions: a.configOptions(sess)}, nil
}

func (a *Agent) handleZeroSetModel(_ context.Context, params json.RawMessage) (any, error) {
	var p ZeroSetModelParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, RPCError(codeInvalidParams, "invalid _zero/set_model params")
	}
	sess := a.session(p.SessionID)
	if sess == nil {
		return nil, RPCError(codeInvalidParams, "unknown session: "+p.SessionID)
	}
	model := strings.TrimSpace(p.Model)
	if err := a.updateModel(sess, model, false); err != nil {
		return nil, err
	}
	return ZeroSetModelResult{Model: model}, nil
}

func (a *Agent) updateModel(sess *acpSession, model string, restrictModels bool) error {
	sess.modelMu.Lock()
	defer sess.modelMu.Unlock()
	if restrictModels && !sess.hasModel(model) {
		return RPCError(codeInvalidParams, "unknown model: "+model)
	}
	if _, err := a.deps.Store.UpdateModel(sess.id, model); err != nil {
		return RPCError(codeInternalError, "save model selection: "+err.Error())
	}
	sess.setModel(model)
	return nil
}

func (a *Agent) handleCancel(_ context.Context, params json.RawMessage) {
	var p CancelParams
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	if sess := a.session(p.SessionID); sess != nil {
		sess.invokeCancel()
	}
}

// ---- advertising helpers ----

func (a *Agent) modeState(s *acpSession) *SessionModeState {
	// auto/ask/plan are offered over ACP; Unsafe is gated to the operator (see
	// handleSetMode) so a client can't grant itself no-prompt host access. Plan
	// only narrows what a client can do (read-only, no write/shell tools), so
	// unlike Unsafe there is no elevation risk in letting a client select it.
	return &SessionModeState{
		CurrentModeID: string(s.currentMode()),
		AvailableModes: []SessionMode{
			{ID: string(agent.PermissionModeAuto), Name: "Auto", Description: "Run safe tools automatically; ask before risky ones."},
			{ID: string(agent.PermissionModeAsk), Name: "Ask", Description: "Ask before every tool that changes state."},
			{ID: string(agent.PermissionModePlan), Name: "Plan", Description: "Read-only planning; write and shell tools are hidden."},
		},
	}
}

// resolveModelChoices advertises authenticated live models when discovery is
// available. The configured model is always retained as the sole fallback.
func (a *Agent) resolveModelChoices(ctx context.Context, cwd string) (string, []SessionConfigOptionValue, bool, error) {
	resolved, err := a.deps.ResolveConfig(cwd, config.Overrides{})
	if err != nil {
		return "", nil, false, err
	}
	selected := strings.TrimSpace(resolved.Provider.Model)
	options := make([]SessionConfigOptionValue, 0, 8)
	seen := make(map[string]bool)
	add := func(id, description string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		options = append(options, SessionConfigOptionValue{Value: id, Name: id, Description: description})
	}
	add(selected, "")
	descriptor, knownProvider := providercatalog.Get(resolved.Provider.CatalogID)
	restrictModels := knownProvider && !descriptor.Custom
	if a.deps.DiscoverModels != nil {
		discovered, discoverErr := a.deps.DiscoverModels(ctx, resolved.Provider)
		if ctx.Err() != nil {
			return "", nil, false, ctx.Err()
		}
		if discoverErr == nil && len(discovered) > 0 {
			for _, model := range discovered {
				if providermodelcatalog.ModelIDAllowedForProvider(resolved.Provider.CatalogID, model.ID) {
					add(model.ID, model.Description)
				}
			}
		}
	}
	return selected, options, restrictModels, nil
}

func modelChoiceExists(models []SessionConfigOptionValue, model string) bool {
	for _, option := range models {
		if option.Value == model {
			return true
		}
	}
	return false
}

func (a *Agent) configOptions(s *acpSession) []SessionConfigOption {
	model, models, mode := s.configState()
	return []SessionConfigOption{{
		ID:           configIDModel,
		Name:         "Model",
		Description:  "Model used for this session.",
		Category:     configCategoryModel,
		Type:         configOptionTypeSelect,
		CurrentValue: model,
		Options:      models,
	}, {
		ID:           configIDMode,
		Name:         "Mode",
		Description:  "Permission mode used for this session.",
		Category:     configCategoryMode,
		Type:         configOptionTypeSelect,
		CurrentValue: string(mode),
		Options: []SessionConfigOptionValue{
			{Value: string(agent.PermissionModeAuto), Name: "Auto", Description: "Run safe tools automatically; ask before risky ones."},
			{Value: string(agent.PermissionModeAsk), Name: "Ask", Description: "Ask before every tool that changes state."},
			{Value: string(agent.PermissionModePlan), Name: "Plan", Description: "Read-only planning; write and shell tools are hidden."},
		},
	}}
}

// ---- persistence + continuity ----

// persistTurn commits a completed turn's buffered events as one store batch, so
// no other writer's records can land between them. A failure is the failure of
// the whole batch: nothing after a failed prerequisite is appended on its own.
func (a *Agent) persistTurn(sessionID string, inputs []sessions.AppendEventInput) error {
	if len(inputs) == 0 {
		return nil
	}
	if a.deps.PersistEvents != nil {
		return a.deps.PersistEvents(sessionID, inputs)
	}
	if a.deps.Store == nil {
		return nil
	}
	_, err := a.deps.Store.AppendEvents(sessionID, inputs)
	return err
}

func messageEvent(role, content string) sessions.AppendEventInput {
	return sessions.AppendEventInput{
		Type:    sessions.EventMessage,
		Payload: map[string]any{"role": role, "content": content},
	}
}

func toolCallEvent(call agent.ToolCall) sessions.AppendEventInput {
	return sessions.AppendEventInput{
		Type: sessions.EventToolCall,
		Payload: map[string]any{
			"toolCallId": call.ID,
			"name":       call.Name,
			"arguments":  call.Arguments,
		},
	}
}

func toolResultEvent(result agent.ToolResult) sessions.AppendEventInput {
	return sessions.AppendEventInput{
		Type: sessions.EventToolResult,
		Payload: map[string]any{
			"toolCallId":   result.ToolCallID,
			"name":         result.Name,
			"status":       result.Status,
			"output":       result.Output,
			"changedFiles": append([]string(nil), result.ChangedFiles...),
		},
	}
}

// persistedMessage is one entry of the restored transcript in stored order. It
// is either a message (role/content) or a tool-call notification (tool), never
// both: keeping them in ONE ordered slice is what preserves the interleaving a
// client needs to redraw the conversation as it originally happened.
type persistedMessage struct {
	eventID string
	role    string
	content string
	// tool is set for a replayed tool call or its result, and is already in wire
	// shape — it comes from the same toolCallStart/toolCallResult mapping the
	// live turn uses, so a replayed tool call renders identically to a fresh one
	// and there is no second mapping to drift out of sync.
	tool *ToolCallUpdate
}

func (a *Agent) loadHistory(sessionID string, requireHistoryLog bool) ([]turnRecord, []persistedMessage, error, error) {
	if a.deps.Store == nil {
		return nil, nil, nil, nil
	}
	// THE EFFECTIVE CONVERSATION, NOT THE RAW LOG. A compacted session keeps its
	// original prefix on disk alongside an EventCompaction that names the events
	// it replaced and carries their durable summary. ReadEvents hands back both,
	// so restoring from it replayed superseded turns AND dropped the summary that
	// replaced them — resume then seeded the next turn with context the
	// conversation had already moved past, and load visibly resurrected
	// transcript entries the user had seen compacted away.
	//
	// ReadRehydratedEvents is the projection the TUI and exec paths already use
	// (internal/tui/session.go, internal/sessions/exec_session.go), so all three
	// now agree on what the conversation IS. Switching readers alone is not
	// enough: rehydration substitutes the compaction event in place of the events
	// it replaced, so a loop that skips everything but EventMessage would drop the
	// summary exactly as before. It is projected below. Reported by @jatmn.
	events, eventLogPresent, err := a.deps.Store.ReadRehydratedEventsWithPresence(sessionID)
	var rehydrateWarning error
	if err != nil {
		rehydrateWarning = err
		events, eventLogPresent, err = a.deps.Store.ReadEventsWithPresence(sessionID)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	if requireHistoryLog && !eventLogPresent {
		return nil, nil, nil, fmt.Errorf("zero session %s event log is missing", sessionID)
	}
	var records []turnRecord
	var messages []persistedMessage
	activeToolCalls := make(map[string]string)
	var pendingUser string
	havePending := false
	for _, e := range events {
		if e.Type == sessions.EventCompaction {
			// The summary stands in for the turns it replaced, so it enters the
			// history as an assistant message: it is prior context the next turn
			// must see, and the wire has no separate role for it.
			raw, marshalErr := json.Marshal(e.Payload)
			if marshalErr != nil {
				continue
			}
			var payload struct {
				Summary string `json:"summary"`
			}
			if json.Unmarshal(raw, &payload) != nil || strings.TrimSpace(payload.Summary) == "" {
				continue
			}
			messages = append(messages, persistedMessage{
				eventID: persistedMessageIdentity(sessionID, e),
				role:    "assistant",
				content: payload.Summary,
			})
			records = append(records, turnRecord{user: pendingUser, assistant: payload.Summary})
			pendingUser = ""
			havePending = false
			continue
		}
		// TOOL ACTIVITY IS PART OF THE TRANSCRIPT, NOT DECORATION. A restored
		// conversation that shows only the prose reads as if the agent asserted
		// its edits and command output from nowhere: the user sees "I updated
		// scope.go" with no record that any tool ran. These are collected for
		// replay only. They deliberately do NOT enter turnRecord, so the prompt
		// context a resumed turn receives stays byte-identical to what it was —
		// load and resume continue to consume the SAME effective history, and
		// only the wire rendering differs. Reported by @jatmn.
		if e.Type == sessions.EventToolCall || e.Type == sessions.EventToolResult {
			if upd := replayToolUpdate(e); upd != nil {
				rawID := upd.ToolCallID
				if e.Type == sessions.EventToolResult {
					replayID, ok := activeToolCalls[rawID]
					if !ok {
						continue
					}
					upd.ToolCallID = replayID
					delete(activeToolCalls, rawID)
				} else {
					replayID := replayToolCallID(persistedMessageIdentity(sessionID, e))
					activeToolCalls[rawID] = replayID
					upd.ToolCallID = replayID
				}
				messages = append(messages, persistedMessage{
					eventID: persistedMessageIdentity(sessionID, e),
					tool:    upd,
				})
			}
			continue
		}
		if e.Type != sessions.EventMessage {
			continue
		}
		raw, err := json.Marshal(e.Payload)
		if err != nil {
			continue
		}
		var msg struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		if json.Unmarshal(raw, &msg) != nil {
			continue
		}
		switch msg.Role {
		case "user":
			messages = append(messages, persistedMessage{eventID: persistedMessageIdentity(sessionID, e), role: msg.Role, content: msg.Content})
			if havePending {
				records = append(records, turnRecord{user: pendingUser})
			}
			pendingUser = msg.Content
			havePending = true
		case "assistant":
			messages = append(messages, persistedMessage{eventID: persistedMessageIdentity(sessionID, e), role: msg.Role, content: msg.Content})
			records = append(records, turnRecord{user: pendingUser, assistant: msg.Content})
			pendingUser = ""
			havePending = false
		}
	}
	if havePending {
		records = append(records, turnRecord{user: pendingUser})
	}
	return records, messages, rehydrateWarning, nil
}

// replayToolUpdate rebuilds the ACP notification for one stored tool event.
//
// The stored toolCallId is returned here as a correlation key. loadHistory maps
// each call occurrence to a stable session-wide replay identity and applies that
// identity to its result. Older records wrote the field as "id", so both
// spellings are accepted — the TUI projection already does the same
// (internal/tui/session.go).
//
// A record that carries no usable id is skipped rather than replayed under a
// synthesized one: an unpairable update is worse than a missing row.
func replayToolUpdate(event sessions.Event) *ToolCallUpdate {
	raw, err := json.Marshal(event.Payload)
	if err != nil {
		return nil
	}
	var payload struct {
		Name         string   `json:"name"`
		ToolCallID   string   `json:"toolCallId"`
		ID           string   `json:"id"`
		Arguments    string   `json:"arguments"`
		Status       string   `json:"status"`
		Output       string   `json:"output"`
		ChangedFiles []string `json:"changedFiles"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return nil
	}
	id := payload.ToolCallID
	if id == "" {
		id = payload.ID
	}
	if id == "" {
		return nil
	}
	if event.Type == sessions.EventToolCall {
		upd := toolCallStart(agent.ToolCall{ID: id, Name: payload.Name, Arguments: payload.Arguments})
		return &upd
	}
	status := tools.Status(payload.Status)
	if status == "" {
		status = tools.StatusOK
	}
	upd := toolCallResult(agent.ToolResult{
		ToolCallID:   id,
		Name:         payload.Name,
		Status:       status,
		Output:       payload.Output,
		ChangedFiles: append([]string(nil), payload.ChangedFiles...),
	})
	return &upd
}

func persistedMessageIdentity(sessionID string, event sessions.Event) string {
	if event.ID != "" {
		return event.ID
	}
	return fmt.Sprintf("%s:%d", sessionID, event.Sequence)
}

// replayMessageID maps ZERO's stable event identity to a standards-shaped UUID
// without leaking or parsing the event id on the wire. The same stored message
// receives the same opaque id across loads, which also gives clients an exact
// chunk boundary when two adjacent persisted messages have the same role.
func replayMessageID(eventID string) string {
	return replayOpaqueID("message", eventID)
}

func replayToolCallID(eventID string) string {
	return replayOpaqueID("tool-call", eventID)
}

func replayOpaqueID(kind, eventID string) string {
	sum := sha256.Sum256([]byte("zero-acp-" + kind + ":" + eventID))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (a *Agent) warnPersistence(note *notifier, action string, message string, err error, notify bool) {
	if err == nil {
		return
	}
	sessionID := ""
	if note != nil {
		sessionID = note.sessionID
	}
	log.Printf("zero acp: failed to %s for session %s: %v", action, sessionID, err)
	if note != nil && notify {
		note.text("\n\n[zero warning] " + message + "\n")
	}
}

// buildPrompt prepends prior conversation as context, since agent.Run drives a
// single seeded turn. Mirrors how headless resume folds history into the prompt.
func buildPrompt(history []turnRecord, userText string) string {
	if len(history) == 0 {
		return userText
	}
	var b strings.Builder
	b.WriteString("Conversation so far:\n")
	for _, t := range history {
		b.WriteString("User: ")
		b.WriteString(t.user)
		b.WriteString("\n")
		if t.assistant != "" {
			b.WriteString("Assistant: ")
			b.WriteString(t.assistant)
			b.WriteString("\n")
		}
	}
	b.WriteString("\n---\nContinue with this request:\n")
	b.WriteString(userText)
	return b.String()
}

func promptImages(blocks []ContentBlock) []zeroruntime.ImageBlock {
	var images []zeroruntime.ImageBlock
	for _, blk := range blocks {
		if blk.Type != "image" || blk.Data == "" {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(blk.Data)
		if err != nil {
			continue
		}
		images = append(images, zeroruntime.ImageBlock{MediaType: blk.MimeType, Data: data})
	}
	return images
}

// ---- session registry + accessors ----

// registerSession publishes a session under the agent's lock. If one is already
// registered for id (e.g. a re-load of an in-flight session) the existing live
// session is returned rather than orphaning its turn or resetting its mode/model.
// The bool reports that case so persisted activation can re-read and apply disk
// history under turnMu. history is set BEFORE first publication so no concurrent
// prompt can read a half-initialized session.
func (a *Agent) registerSession(id, cwd string, history []turnRecord, model string, models []SessionConfigOptionValue, restrictModels bool) (*acpSession, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if existing := a.sessions[id]; existing != nil {
		return existing, true
	}
	sess := &acpSession{id: id, cwd: cwd, mode: agent.PermissionModeAuto, model: model, models: models, restrictModels: restrictModels, history: history}
	a.sessions[id] = sess
	return sess, false
}

// refreshSessionHistory is the activation apply boundary for an already-live
// session. The history is read while prompts are excluded, then applied to the
// same object the prompt path uses. Re-reading here avoids overwriting a turn
// that committed between activatePersistedSession's pre-publication read and
// discovering that another activation had already registered the session.
func (a *Agent) refreshSessionHistory(sess *acpSession, requireHistoryLog bool) ([]turnRecord, []persistedMessage, error, error) {
	sess.turnMu.Lock()
	defer sess.turnMu.Unlock()
	history, messages, warning, err := a.loadHistory(sess.id, requireHistoryLog)
	if err == nil {
		sess.setHistory(history)
	}
	return history, messages, warning, err
}

func (a *Agent) session(id string) *acpSession {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[id]
}

func (s *acpSession) setCancel(cancel context.CancelFunc) {
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()
}

func (s *acpSession) invokeCancel() {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *acpSession) setMode(mode agent.PermissionMode) {
	s.mu.Lock()
	s.mode = mode
	s.mu.Unlock()
}

func (s *acpSession) currentMode() agent.PermissionMode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode
}

func (s *acpSession) setModel(model string) {
	model = strings.TrimSpace(model)
	s.mu.Lock()
	found := false
	for _, option := range s.models {
		if option.Value == model {
			found = true
			break
		}
	}
	if !found && model != "" {
		s.models = append(s.models, SessionConfigOptionValue{Value: model, Name: model})
	}
	s.model = model
	s.mu.Unlock()
}

func (s *acpSession) currentModel() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.model
}

func (s *acpSession) hasModel(model string) bool {
	model = strings.TrimSpace(model)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, option := range s.models {
		if option.Value == model {
			return true
		}
	}
	return false
}

func (s *acpSession) configState() (string, []SessionConfigOptionValue, agent.PermissionMode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.model, append([]SessionConfigOptionValue(nil), s.models...), s.mode
}

func (s *acpSession) appendHistory(rec turnRecord) {
	s.mu.Lock()
	s.history = append(s.history, rec)
	s.mu.Unlock()
}

func (s *acpSession) setHistory(history []turnRecord) {
	s.mu.Lock()
	s.history = append([]turnRecord(nil), history...)
	s.mu.Unlock()
}

func (s *acpSession) snapshotHistory() []turnRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]turnRecord(nil), s.history...)
}

// sameWorkspace reports whether two resolved roots name the same directory.
//
// STRING EQUALITY IS NOT DIRECTORY IDENTITY. ResolveWorkspaceRoot is abs plus
// filepath.Clean plus a stat: it does not fold case and does not resolve
// junctions, so one directory reached by two spellings produces two different
// roots. A session persisted from the TUI was then unresumable from an editor
// holding a different spelling of the same project folder, and session/list
// filtered by the other spelling returned nothing — not a failed resume but an
// invisible one, on exactly the case this feature exists for.
//
// filepath.EvalSymlinks is NOT the fix on Windows: it normalises a drive letter
// but returns a junction path unchanged, so the alias survives it. Junctions need
// no privilege, so this is ordinary rather than exotic. os.SameFile compares the
// filesystem's own identity for the two directories, which is the question being
// asked.
//
// The string comparison stays as the fast path, and a stat failure falls back to
// it rather than widening the match — this gate refuses access to another
// workspace's files and configuration, so an unanswerable comparison denies.
func sameWorkspace(left, right string) bool {
	if left == right {
		return true
	}
	leftInfo, err := os.Stat(left)
	if err != nil {
		return false
	}
	rightInfo, err := os.Stat(right)
	if err != nil {
		return false
	}
	return os.SameFile(leftInfo, rightInfo)
}
