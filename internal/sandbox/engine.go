package sandbox

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"
)

type EngineOptions struct {
	WorkspaceRoot string
	Policy        Policy
	Store         *GrantStore
	Backend       Backend
	Scope         *Scope
	// SensitiveEnvKeys adds config-derived credential variable names to the
	// catalog and namespace secrets scrubbed from sandboxed commands.
	SensitiveEnvKeys []string
}

type Engine struct {
	workspaceRoot    string
	policy           Policy
	store            *GrantStore
	backend          Backend
	scope            *Scope
	sensitiveEnvKeys []string
	sessionGrants    *memoryGrantSet
	sessionProfiles  *permissionProfileGrantSet
	turnProfiles     *permissionProfileGrantSet
	commandPrefixes  *commandPrefixGrantSet
}

func NewEngine(options EngineOptions) *Engine {
	policy := options.Policy
	if policy.Mode == "" {
		policy = DefaultPolicy()
	}
	scope := options.Scope
	workspaceRoot := strings.TrimSpace(options.WorkspaceRoot)
	if scope != nil && workspaceRoot == "" {
		// Scope-only construction must still populate workspaceRoot: Evaluate's
		// path classification and EnforceWorkspace denial both guard on
		// request.WorkspaceRoot != "", and resolveCommandDir hard-requires it, so
		// leaving it blank would silently skip enforcement. Roots()[0] is the
		// workspace root by the Scope contract.
		if roots := scope.Roots(); len(roots) > 0 {
			workspaceRoot = roots[0]
		}
	}
	if scope == nil && workspaceRoot != "" {
		scope = newScopeBestEffort(workspaceRoot)
	}
	return &Engine{
		workspaceRoot:    workspaceRoot,
		policy:           policy,
		store:            options.Store,
		backend:          options.Backend,
		scope:            scope,
		sensitiveEnvKeys: normalizeSensitiveEnvKeys(options.SensitiveEnvKeys),
		sessionGrants:    newMemoryGrantSet(),
		sessionProfiles:  newPermissionProfileGrantSet(),
		turnProfiles:     newPermissionProfileGrantSet(),
		commandPrefixes:  newCommandPrefixGrantSet(),
	}
}

// Scope returns the engine's shared write scope (nil when the engine was
// built without a workspace root and no explicit Scope option). The TUI uses
// it for /add-dir.
func (engine *Engine) Scope() *Scope {
	if engine == nil {
		return nil
	}
	return engine.scope
}

func (engine *Engine) CanPersistGrants() bool {
	return engine != nil && engine.store != nil
}

// DegradedNotice is the line a session shows when this engine's sandbox cannot
// enforce what its policy asks for, or "" when it can or the sandbox is off.
//
// Built from the engine's own backend and policy through the same plan `zero
// sandbox policy` prints, so the notice and the command that explains it cannot
// describe two different sandboxes.
//
// EXCEPT WHERE THE RUNNER TAKES A DIFFERENT PATH. With the nesting markers set,
// BuildCommandPlan passes every command through unwrapped, trusting an outer
// sandbox to contain it, and the markers are ambient environment that nothing
// authenticates (#727). The plan above does not model that, so a session started
// with them ran every command unwrapped and said nothing. Reported by CodeRabbit.
func (engine *Engine) DegradedNotice() string {
	if engine == nil || engine.policy.Mode == ModeDisabled {
		return ""
	}
	if IsAlreadySandboxed() {
		return nestedSandboxNotice
	}
	return engine.backend.BuildPlan(engine.workspaceRoot, engine.policy).DegradedNotice()
}

// nestedSandboxNotice says why a session with the nesting markers set does not
// wrap its commands, and that the outer sandbox it relies on is unverified.
const nestedSandboxNotice = "Sandbox enforcement is degraded: " + EnvSandboxed + " and " + EnvSandboxBackend +
	" say this session already runs inside a sandbox, so it does not wrap commands again, and Zero cannot verify that outer sandbox."

func (engine *Engine) ConsumeGrantMigrationNotice() (string, error) {
	if engine == nil || engine.store == nil {
		return "", nil
	}
	return engine.store.ConsumeMigrationNotice()
}

func (engine *Engine) GrantCommandPrefixForSession(toolName string, prefix []string) {
	if engine == nil || len(prefix) == 0 {
		return
	}
	prefix, ok := NormalizeCommandPrefix(prefix)
	if !ok {
		return
	}
	engine.commandPrefixes.add(CommandPrefixGrant{
		ToolName: toolName,
		Prefix:   prefix,
		Session:  true,
	})
}

func (engine *Engine) GrantCommandPrefix(input CommandPrefixInput) (CommandPrefixGrant, error) {
	if engine == nil || engine.store == nil {
		return CommandPrefixGrant{}, errors.New("sandbox grant store is not configured")
	}
	return engine.store.GrantCommandPrefix(input)
}

func (engine *Engine) LookupCommandPrefix(toolName string, command []string) (CommandPrefixGrant, bool) {
	if engine == nil || engine.store == nil || len(command) == 0 {
		return CommandPrefixGrant{}, false
	}
	grant, matched, err := engine.store.LookupCommandPrefix(toolName, command)
	if err != nil {
		return CommandPrefixGrant{}, false
	}
	return grant, matched
}

func (engine *Engine) ApprovedCommandPrefixes() []CommandPrefixGrant {
	if engine == nil || engine.store == nil {
		return nil
	}
	grants, err := engine.store.ListCommandPrefixes()
	if err != nil {
		return nil
	}
	return grants
}

func (engine *Engine) LookupCommandPrefixForSession(toolName string, command []string) (CommandPrefixGrant, bool) {
	if engine == nil || len(command) == 0 {
		return CommandPrefixGrant{}, false
	}
	return engine.commandPrefixes.match(toolName, command)
}

// ReadExclusions returns the resolved DenyRead/AllowRead exclusion matcher for
// this engine's policy, resolving each policy entry ONCE. The search tools build
// it a single time per run and reuse it across the whole walk so the predicates
// don't re-run Abs/EvalSymlinks per visited path. Returns nil for a nil engine
// (the matcher's methods treat nil as "exclude nothing").
func (engine *Engine) ReadExclusions() *ReadExclusions {
	// A disabled policy enforces nothing, so it must not filter search results
	// either (Evaluate already allows every request under ModeDisabled).
	if engine == nil {
		return nil
	}
	policy := engine.effectivePolicy(engine.policy)
	if policy.Mode == ModeDisabled {
		return nil
	}
	return &ReadExclusions{
		workspaceRoot: engine.workspaceRoot,
		denyRoots:     resolvePolicyPaths(policy.DenyRead),
		allowRoots:    resolvePolicyPaths(policy.AllowRead),
	}
}

// ReadExclusionGlobs returns the ripgrep-style --glob exclusion args for this
// engine's policy + scope (see the package-level ReadExclusionGlobs). Empty when
// DenyRead is unset or the engine has no scope.
func (engine *Engine) ReadExclusionGlobs() []string {
	// A disabled policy filters nothing (parity with ReadExclusions / Evaluate).
	if engine == nil {
		return nil
	}
	policy := engine.effectivePolicy(engine.policy)
	if policy.Mode == ModeDisabled {
		return nil
	}
	return ReadExclusionGlobs(policy, engine.scope)
}

// effectiveNetworkMode is the single source of truth for sandboxed command
// egress. Shell network policy is binary: restricted commands use NetworkDeny,
// and approved network access widens it to NetworkAllow.
func (engine *Engine) effectiveNetworkMode(policy Policy) NetworkMode {
	return NormalizeNetworkMode(policy.Network)
}

// UnsandboxedExecutionAllowed reports whether an escalated shell attempt may
// bypass the native sandbox without dropping active denied-read restrictions.
func (engine *Engine) UnsandboxedExecutionAllowed() bool {
	if engine == nil {
		return true
	}
	policy := engine.effectivePolicy(engine.policy)
	if policy.Mode == ModeDisabled {
		return true
	}
	return len(normalizeProfilePaths(policy.DenyRead)) == 0
}

// toolNetworkExempt reports whether a request is exempt from the engine-level
// network deny because it is a first-party, in-process network tool. Such tools
// do not use sandboxed shell egress; they keep their own SSRF/host safeguards. A
// shell command merely classified as network (SideEffectShell) is NOT exempt, so
// shell egress stays blocked under deny.
func (engine *Engine) toolNetworkExempt(request Request) bool {
	return request.SideEffect == SideEffectNetwork
}

// scopeFor returns the scope to validate request paths against. The engine's
// shared scope applies only when the request targets the engine's own
// workspace root; a per-request override root gets an ad-hoc single-root scope
// (single-root semantics; it deliberately ignores the engine's extra roots so
// an override can never inherit broader write access). The ad-hoc root is left
// unnormalized on purpose: validateWorkspacePath re-resolves roots internally,
// and skipping normalization avoids per-Evaluate EvalSymlinks syscalls.
func (engine *Engine) scopeFor(requestRoot string) *Scope {
	if engine.scope != nil && requestRoot == engine.workspaceRoot {
		return engine.scope
	}
	return newScopeBestEffort(requestRoot)
}

func newScopeBestEffort(workspaceRoot string) *Scope {
	scope, err := NewScope(workspaceRoot, nil)
	if err == nil {
		return scope
	}
	return &Scope{workspaceRoot: normalizeWorkspaceRootBestEffort(workspaceRoot)}
}

// shellSandboxActive reports whether a native wrapping sandbox would actually
// wrap a shell command under the given policy. It is true only when the policy
// is enforcing and the engine's backend can wrap commands with native isolation
// (bubblewrap / sandbox-exec available). An unavailable backend, a disabled
// policy, or a nil engine all report false, so ordinary shell auto-allow never
// runs an unsandboxed command.
func (engine *Engine) shellSandboxActive(policy Policy) bool {
	if engine == nil {
		return false
	}
	if policy.Mode == ModeDisabled {
		return false
	}
	// Re-entrancy skips wrapping: when the process carries the sandbox markers,
	// the runner returns a pass-through (unwrapped) plan, so the native sandbox
	// is NOT the boundary for THIS command. Because the markers are ambient and
	// forgeable at an unsandboxed process boundary (issue #727), auto-allowing a
	// shell command on that basis would run it unwrapped with no prompt. Mirror
	// the runner: report inactive so the command takes the normal approval path.
	if IsAlreadySandboxed() {
		return false
	}
	backend := engine.backend
	if !backend.Available || backend.Executable == "" || !backend.CommandWrapping || !backend.NativeIsolation {
		return false
	}
	// On Windows the command is only actually wrapped once `zero sandbox setup`
	// has written the marker; until then execution DEGRADES to unwrapped (see
	// manager.BuildExecutionRequest). The engine must mirror that — otherwise it
	// would auto-allow a shell command as "sandboxed" while it really runs
	// unsandboxed, breaking the invariant above and the per-command approval
	// floor. When not initialized, fall through so the command prompts.
	if backend.Platform == "windows" && !windowsSandboxInitialized() {
		return false
	}
	return true
}

// Precheck reports the sandbox blocks that would block a tool request BEFORE
// it executes, so a caller (e.g. a batch confirmation or a "would this run?"
// check) can fail fast and surface the reason instead of discovering it mid-run.
// It reuses Evaluate, so policy is never duplicated: a request the engine would
// allow or merely prompt for yields no blocks; a denied request yields its
// block. A nil engine (sandbox disabled) yields no blocks.
func (engine *Engine) Precheck(ctx context.Context, request Request) []Block {
	if engine == nil {
		return nil
	}
	return blocksFromDecision(engine.Evaluate(ctx, request))
}

// blocksFromDecision extracts the blocking reasons from a decision. Only a deny
// carries one; the fallback synthesizes one for the rare deny without a
// structured block so a caller always gets a reason.
func blocksFromDecision(decision Decision) []Block {
	if decision.Action != ActionDeny {
		return nil
	}
	if decision.Block != nil {
		return []Block{*decision.Block}
	}
	return []Block{{
		Code:   BlockDenied,
		Action: ActionDeny,
		Risk:   decision.Risk,
		Reason: decision.ErrorString(),
	}}
}

func (engine *Engine) Evaluate(ctx context.Context, request Request) Decision {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		risk := Classify(request)
		return deny(request, risk, BlockContextCanceled, "", "sandbox evaluation cancelled: "+err.Error(), false)
	}
	if engine == nil {
		return Decision{Action: ActionAllow, Risk: Classify(request), Reason: "sandbox disabled"}
	}
	policy := engine.effectivePolicy(engine.policy)
	if policy.Mode == "" {
		policy = DefaultPolicy()
	}
	request.WorkspaceRoot = firstNonEmpty(request.WorkspaceRoot, engine.workspaceRoot)
	request.Permission = NormalizePermission(request.Permission)
	request.PermissionMode = NormalizePermissionMode(request.PermissionMode)
	request.SideEffect = NormalizeSideEffect(request.SideEffect)
	scope := engine.scopeFor(request.WorkspaceRoot)
	risk := classifyWithScope(request, scope)

	if policy.Mode == ModeDisabled {
		return Decision{Action: ActionAllow, Risk: risk, Reason: "sandbox disabled"}
	}
	if request.Permission == PermissionDeny {
		return deny(request, risk, BlockDeniedPermission, "", permissionReason(request), false)
	}
	reqRaw, reqKind := DeriveScope(request.ToolName, request.Args)
	reqScope := resolveScopeForKind(reqRaw, reqKind, request.WorkspaceRoot)
	var persistentAllow *Grant
	if engine.store != nil {
		match, err := engine.store.Lookup(request.ToolName, reqScope)
		if err == nil && match.Matched {
			grant := match.Grant
			if grant.Decision == GrantDeny {
				decision := deny(request, risk, BlockPersistentDeny, "", "persistent sandbox deny grant matched", true)
				decision.GrantMatched = true
				decision.Grant = &grant
				return decision
			}
			persistentAllow = &grant
		}
	}
	var sessionAllow *Grant
	if match := engine.lookupSessionGrant(request.ToolName, reqScope); match.Matched {
		grant := match.Grant
		sessionAllow = &grant
	}
	// The fine-grained path lists (DenyRead/DenyWrite/AllowRead/AllowWrite) apply
	// whenever the sandbox is enforcing, independent of EnforceWorkspace and even
	// when there is no workspace root (absolute paths are still resolved and
	// matched), so they are honored consistently with the grep/glob exclusion path
	// and can't be bypassed by an engine built without a workspace root. The
	// workspace boundary itself needs a root, so it is gated on having one. Mode is
	// already known to be enforcing here (ModeDisabled returned above).
	enforceWorkspace := policy.EnforceWorkspace && request.WorkspaceRoot != ""
	if block := applyPatchPathBlock(request); block != nil {
		return deny(request, risk, block.Code, block.Path, block.Reason, false)
	}
	var promptableBlock *pathBlock
	for _, requested := range requestPaths(request) {
		if block := validatePathWithPolicy(scope, policy, request.SideEffect, enforceWorkspace, request.WorkspaceRoot, requested); block != nil {
			if promptablePathBlock(request, block) {
				if promptableBlock == nil {
					promptableBlock = block
				}
				continue
			}
			return deny(request, risk, block.Code, block.Path, block.Reason, false)
		}
	}
	netMode := engine.effectiveNetworkMode(policy)
	if netMode == NetworkDeny && HasRiskCategory(risk, "network") && !engine.toolNetworkExempt(request) {
		if request.SideEffect == SideEffectShell && request.PermissionMode != PermissionUnsafe {
			return Decision{Action: ActionPrompt, Risk: risk, Reason: ReasonNetworkBlocked}
		}
		return deny(request, risk, BlockNetwork, "", ReasonNetworkBlocked, false)
	}
	if HasRiskCategory(risk, "destructive") {
		if request.SideEffect == SideEffectShell && !request.PermissionGranted && request.PermissionMode != PermissionUnsafe {
			return Decision{Action: ActionPrompt, Risk: risk, Reason: "destructive shell command requires approval"}
		}
		if request.SideEffect == SideEffectShell && !request.PermissionGranted {
			return deny(request, risk, BlockDestructiveCommand, "", "destructive shell command requires approval", false)
		}
	}
	if request.SideEffect == SideEffectShell && requestRequiresEscalatedSandbox(request) {
		if !request.PermissionGranted && request.PermissionMode != PermissionUnsafe {
			return Decision{Action: ActionPrompt, Risk: risk, Reason: ReasonEscalatedSandboxRequired}
		}
		// Unsafe mode may auto-allow ordinary shell prompts, but it must not
		// silently convert a sandboxed shell run into an unsandboxed one.
		if !request.PermissionGranted {
			return deny(request, risk, BlockDeniedPermission, "", ReasonEscalatedSandboxRequired, false)
		}
	}
	if persistentAllow != nil {
		return Decision{
			Action:       ActionAllow,
			Reason:       "persistent sandbox allow grant matched",
			Risk:         risk,
			GrantMatched: true,
			Grant:        persistentAllow,
		}
	}
	if sessionAllow != nil {
		return Decision{
			Action:       ActionAllow,
			Reason:       "session sandbox allow grant matched",
			Risk:         risk,
			GrantMatched: true,
			Grant:        sessionAllow,
		}
	}
	if promptableBlock != nil {
		return promptPathBlock(request, risk, promptableBlock)
	}
	if request.Permission == PermissionAllow {
		return Decision{Action: ActionAllow, Risk: risk, Reason: permissionReason(request)}
	}
	if workspaceWriteAutoAllowed(policy, request, scope) {
		return Decision{Action: ActionAllow, Risk: risk, Reason: "workspace write is allowed", AutoAllowed: true}
	}
	// Auto-allow an ordinary shell command when the active native sandbox is the
	// safety boundary. Network, destructive, and path checks run before this
	// branch, so they still prompt or deny as configured. If the backend is
	// unavailable or disabled, shell commands keep the normal approval prompt.
	if request.SideEffect == SideEffectShell && engine.shellSandboxActive(policy) {
		return Decision{Action: ActionAllow, Risk: risk, Reason: "auto-allowed: sandbox is active for this shell command", AutoAllowed: true}
	}
	if request.PermissionGranted || request.PermissionMode == PermissionUnsafe {
		return Decision{Action: ActionAllow, Risk: risk, Reason: permissionReason(request)}
	}
	return Decision{Action: ActionPrompt, Risk: risk, Reason: permissionReason(request)}
}

func requestRequiresEscalatedSandbox(request Request) bool {
	return strings.TrimSpace(firstArgString(request.Args, "sandbox_permissions")) == "require_escalated"
}

func (engine *Engine) Grant(input GrantInput) (Grant, error) {
	if engine == nil || engine.store == nil {
		return Grant{}, errors.New("sandbox grant store is not configured")
	}
	input, err := engine.normalizeGrantInput(input)
	if err != nil {
		return Grant{}, err
	}
	return engine.store.Grant(input)
}

func (engine *Engine) GrantForSession(input GrantInput) (Grant, error) {
	if engine == nil {
		return Grant{}, errors.New("sandbox engine is not configured")
	}
	input, err := engine.normalizeGrantInput(input)
	if err != nil {
		return Grant{}, err
	}
	grant, err := createGrant(input, time.Now)
	if err != nil {
		return Grant{}, err
	}
	grant.Session = true
	engine.sessionGrants.add(grant)
	return grant, nil
}

func (engine *Engine) normalizeGrantInput(input GrantInput) (GrantInput, error) {
	kind, err := normalizeScopeKind(input.ScopeKind)
	if err != nil {
		return GrantInput{}, err
	}
	scope, kind := reconcileScope(strings.TrimSpace(input.Scope), kind)
	if kind != ScopeToolWide {
		// Anchor relative path scopes to this workspace so the grant cannot match
		// a same-named path in another project. Host scopes remain network hosts.
		scope = resolveScopeForKind(scope, kind, engine.workspaceRoot)
	}
	input.Scope = scope
	input.ScopeKind = kind
	return input, nil
}

func (engine *Engine) lookupSessionGrant(toolName string, reqScope string) GrantLookup {
	if engine == nil {
		return GrantLookup{}
	}
	return engine.sessionGrants.lookup(toolName, reqScope)
}

func workspaceWriteAutoAllowed(policy Policy, request Request, scope *Scope) bool {
	if !policy.EnforceWorkspace || request.WorkspaceRoot == "" || request.SideEffect != SideEffectWrite {
		return false
	}
	paths := requestPaths(request)
	if len(paths) == 0 || requestPathsTouchProtectedMetadata(scope, request.WorkspaceRoot, paths) {
		return false
	}
	switch request.ToolName {
	case "write_file", "edit_file", "apply_patch":
		return true
	default:
		return false
	}
}

func promptablePathBlock(request Request, block *pathBlock) bool {
	if block == nil || block.Code != BlockOutsideWorkspace {
		return false
	}
	if request.PermissionMode == PermissionUnsafe {
		return false
	}
	if request.ToolName == "apply_patch" {
		return false
	}
	switch request.SideEffect {
	case SideEffectRead, SideEffectWrite, SideEffectOutOfWorkspace:
	default:
		return false
	}
	path := strings.TrimSpace(block.Path)
	if path == "" {
		return false
	}
	if !filepath.IsAbs(path) && strings.TrimSpace(request.WorkspaceRoot) == "" {
		return false
	}
	return !strings.Contains(block.Reason, "cannot be validated without a workspace root")
}

func promptPathBlock(request Request, risk Risk, block *pathBlock) Decision {
	promptBlock := &Block{
		Code:        block.Code,
		ToolName:    request.ToolName,
		Action:      ActionPrompt,
		Risk:        risk,
		Path:        block.Path,
		Reason:      block.Reason,
		Recoverable: true,
	}
	return Decision{
		Action: ActionPrompt,
		Reason: block.Reason,
		Risk:   risk,
		Block:  promptBlock,
	}
}

func requestPathsTouchProtectedMetadata(scope *Scope, workspaceRoot string, paths []string) bool {
	if len(paths) == 0 {
		return false
	}
	for _, path := range paths {
		if pathTouchesProtectedMetadata(scope, workspaceRoot, path) {
			return true
		}
	}
	return false
}

func pathTouchesProtectedMetadata(scope *Scope, workspaceRoot string, path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	if !filepath.IsAbs(path) {
		return relativePathTouchesProtectedMetadata(path)
	}
	roots := []string{}
	if scope != nil {
		roots = scope.Roots()
	} else if workspaceRoot != "" {
		roots = []string{workspaceRoot}
	}
	for _, root := range roots {
		root = normalizeWorkspaceRootBestEffort(root)
		if root == "" {
			continue
		}
		normalized := NormalizePrefixForRoot(filepath.Clean(path), root)
		relative, err := filepath.Rel(root, normalized)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
			continue
		}
		if relativePathTouchesProtectedMetadata(relative) {
			return true
		}
	}
	return false
}

func relativePathTouchesProtectedMetadata(path string) bool {
	cleaned := filepath.Clean(filepath.FromSlash(strings.TrimSpace(path)))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) || filepath.IsAbs(cleaned) {
		return false
	}
	first := cleaned
	if index := strings.Index(first, string(filepath.Separator)); index >= 0 {
		first = first[:index]
	}
	for _, protected := range protectedMetadataNames {
		if first == protected {
			return true
		}
	}
	return false
}

func deny(request Request, risk Risk, code BlockCode, path string, reason string, recoverable bool) Decision {
	block := &Block{
		Code:        code,
		ToolName:    request.ToolName,
		Action:      ActionDeny,
		Risk:        risk,
		Path:        path,
		Reason:      reason,
		Recoverable: recoverable,
	}
	return Decision{
		Action: ActionDeny,
		Reason: reason,
		Risk:   risk,
		Block:  block,
	}
}

func permissionReason(request Request) string {
	if request.Reason != "" {
		return request.Reason
	}
	switch request.Permission {
	case PermissionAllow:
		return "tool safety allows execution"
	case PermissionDeny:
		return "tool safety denies execution"
	default:
		return "tool requires approval before execution"
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
