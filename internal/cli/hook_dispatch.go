package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Twigpine/zero/internal/execution"
	"github.com/Twigpine/zero/internal/hooks"
	"github.com/Twigpine/zero/internal/workspacetrust"
)

// trustSkip reports whether a runtime chokepoint dropped the project layer because
// the workspace was not trusted, so the caller can emit a single combined notice.
// Both chokepoints (hooks and plugins) return one, and the caller ORs them.
type trustSkip struct {
	// excludedProjectConfig is true when the project layer was excluded AND a
	// project config for this surface actually exists on disk (so a notice is
	// worth showing). It stays false in a trusted workspace, or when the
	// workspace has no project config to skip.
	excludedProjectConfig bool
	// trustCheckErrored is true when the exclusion was caused by a trust-store
	// read error rather than a clean untrusted verdict, so the caller can name
	// the error in its notice.
	trustCheckErrored bool
}

// resolveTrust computes the fail-closed trust verdict for a chokepoint. It treats
// any error OR an empty trustRoot as untrusted, so a forgotten or future call site
// cannot fail open. It returns whether the project layer should be excluded and
// whether the decision was driven by a store-read error.
//
// Each chokepoint (hooks and plugins) calls this itself rather than the caller
// resolving trust once and passing a bool down. That is deliberate: keeping the
// check inside the chokepoint is what makes the gate fail-closed by construction.
// Do NOT hoist it to the callers behind an excludeProject bool whose zero value
// includes the project layer, that reintroduces a fail-open default. The extra
// store read per session is negligible.
func resolveTrust(trustRoot string) (excludeProject bool, trustCheckErrored bool) {
	if trustRoot == "" {
		return true, false
	}
	trusted, err := workspacetrust.IsTrusted(trustRoot)
	if err != nil {
		return true, true
	}
	return !trusted, false
}

// newHookDispatcher builds the per-session hooks dispatcher for a workspace,
// merging user + project hooks.json and wiring the audit store. It fails OPEN:
// any load or setup error yields a nil dispatcher, which Dispatch treats as a
// no-op, so a malformed hooks config can never wedge tool execution. With no
// hooks configured the dispatcher selects nothing and runs no commands, so the
// hot path stays free of overhead until a user opts in via hooks.json.
//
// trustRoot is the original launch directory (resolved before any --worktree
// reassignment); the project layer loads only when that root is trusted.
func newHookDispatcher(workspaceRoot string, trustRoot string, runners ...*execution.Runner) (*hooks.Dispatcher, trustSkip) {
	return newHookDispatcherWithExtra(workspaceRoot, nil, trustRoot, runners...)
}

// newHookDispatcherWithExtra builds the dispatcher like newHookDispatcher but also
// folds plugin-activated hook definitions into the active hook set, so a plugin's
// declared hooks run alongside the user/project hooks.json hooks. Plugin hooks are
// appended after the configured hooks; their ids are plugin-namespaced (plugin
// id + hook name) so they never collide with hooks.json ids. A nil/empty extra
// slice is byte-equivalent to newHookDispatcher.
//
// The trust check lives here, inside the chokepoint, so no caller can bypass it:
// the project layer is dropped (ExcludeProject) whenever trustRoot is empty, the
// trust store cannot be read, or the workspace is not trusted (fail-closed). The
// returned trustSkip lets the caller emit one combined notice; the notice itself
// is NOT emitted here.
func newHookDispatcherWithExtra(workspaceRoot string, extra []hooks.Definition, trustRoot string, runners ...*execution.Runner) (*hooks.Dispatcher, trustSkip) {
	excludeProject, trustCheckErrored := resolveTrust(trustRoot)
	skip := trustSkip{
		excludedProjectConfig: excludeProject && projectHooksFileExists(workspaceRoot),
		trustCheckErrored:     trustCheckErrored,
	}

	loaded, err := hooks.LoadConfig(hooks.LoadOptions{Cwd: workspaceRoot, ExcludeProject: excludeProject})
	if err != nil {
		return nil, skip
	}
	var audit *hooks.AuditStore
	if store, err := hooks.NewAuditStore(hooks.AuditStoreOptions{}); err == nil {
		audit = store
	}
	config := loaded.Config
	if len(extra) > 0 {
		// Plugin hooks only run when hooks are enabled overall; an explicit
		// `enabled:false` in hooks.json still disables the whole hook surface.
		merged := append([]hooks.Definition{}, config.Hooks...)
		existing := make(map[string]bool, len(merged))
		for _, hook := range merged {
			existing[hook.ID] = true
		}
		for _, hook := range extra {
			// A hooks.json hook with the same (namespaced) id wins, so an operator can
			// still disable a plugin hook by id without the plugin re-enabling it.
			if existing[hook.ID] {
				continue
			}
			merged = append(merged, hook)
		}
		config.Hooks = merged
	}
	var runner *execution.Runner
	if len(runners) > 0 {
		runner = runners[0]
	}
	return hooks.NewDispatcher(hooks.DispatcherOptions{
		Config:    config,
		Audit:     audit,
		Cwd:       workspaceRoot,
		Execution: runner,
	}), skip
}

// projectHooksFileExists reports whether a ./.zero/hooks.json is present under
// workspaceRoot, so the caller only notices about config it actually skipped.
func projectHooksFileExists(workspaceRoot string) bool {
	if workspaceRoot == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(workspaceRoot, ".zero", "hooks.json"))
	return err == nil && !info.IsDir()
}

// emitTrustNotice writes at most one stderr line summarizing that project-scoped
// hooks, plugins, and/or MCP servers were skipped in an untrusted workspace. It
// takes the skip report from each trust-gated surface (hooks, plugins, MCP) and ORs
// them, so a workspace that only drops one surface still gets the notice. It is
// computed once per session by the caller (each session-setup site runs once), so it
// is naturally once-per-process. When any surface's skip was a trust-store read
// error, the notice names that so a transient config-dir problem is diagnosable.
func emitTrustNotice(stderr io.Writer, skips ...trustSkip) {
	if stderr == nil {
		return
	}
	excluded := false
	storeErrored := false
	for _, skip := range skips {
		excluded = excluded || skip.excludedProjectConfig
		storeErrored = storeErrored || skip.trustCheckErrored
	}
	if !excluded {
		return
	}
	if storeErrored {
		_, _ = fmt.Fprintln(stderr, "zero: the workspace-trust store could not be read; ignoring project hooks/plugins/MCP servers (fail-closed). Run 'zero trust' to enable.")
		return
	}
	_, _ = fmt.Fprintln(stderr, "zero: ignoring project hooks/plugins/MCP servers in an untrusted workspace. Run 'zero trust' to enable.")
}
