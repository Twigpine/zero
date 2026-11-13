package swarm

import (
	"context"
	"errors"

	"github.com/Twigpine/zero/internal/specialist"
	"github.com/Twigpine/zero/internal/tools"
)

// NewSpecialistLauncher adapts internal/specialist.Executor into a
// MemberLauncher: each swarm member runs as a specialist sub-agent. This is the
// production "direct" launch path (used when no daemon pool is wired). The
// member inherits the orchestrator's model via spec.Model and runs in spec.Cwd,
// so it stays under the same sandbox + policy as its parent — the swarm never
// grants a member more authority.
//
// agentType maps to the specialist agent name; the specialist's own definition
// for that name governs the member's system prompt and tools. RunInBackground is
// deliberately left false: the swarm provides concurrency itself (one goroutine
// per member), so each Run executes to completion inside its goroutine.
//
// spec.PermissionMode (resolved + clamped by buildSpec, never above the
// orchestrator's) is propagated so the child only runs unsafe/high autonomy when
// the orchestrator is itself unsafe; otherwise it runs non-unsafe. An empty mode
// is substituted with the non-unsafe "auto" default so a swarm member can never
// fall into the historical empty-means-high path — that is the Task tool's
// behavior, not the swarm's.
func NewSpecialistLauncher(executor specialist.Executor) MemberLauncher {
	return FuncLauncher{Run: func(ctx context.Context, spec MemberSpec) (MemberResult, error) {
		permissionMode := spec.PermissionMode
		if permissionMode == "" {
			permissionMode = permissionModeAuto
		}
		// Run the member from its swarm agent definition (its own system prompt),
		// NOT by resolving spec.AgentType against the specialist registry — swarm
		// agent types (e.g. "subagent"/"teammate") are not registered specialists,
		// so a name lookup fails with "specialist not found" and the member never
		// executes. The inline manifest carries the member's prompt plus a general
		// task-executor toolset; authority is still clamped to permissionMode
		// below, so a member never exceeds the orchestrator's authority.
		manifest := specialistManifestForMember(spec)
		res, err := executor.Run(ctx, specialist.TaskParameters{
			Name:        spec.AgentType,
			Prompt:      spec.Task,
			Description: spec.AgentType + " · team " + spec.Team,
			Manifest:    &manifest,
		}, specialist.TaskRunOptions{
			ParentModel:    spec.Model,
			Cwd:            spec.Cwd,
			PermissionMode: permissionMode,
			// A swarm member runs headless and is meant to do real work, so it
			// gets in-workspace write/edit + sandboxed shell (member autonomy)
			// rather than the read-only "low" a plain specialist child would get.
			// Still clamped to non-unsafe and sandbox-confined to the workspace.
			MemberAutonomy: true,
		})
		if err != nil {
			// Preserve the child session id on a post-start failure too (exec.go
			// returns it on the error path) so lifecycle's FailWithSession can keep
			// the failed member drillable, not just the StatusError case below.
			return MemberResult{SessionID: res.SessionID}, err
		}
		if res.Result.Status == tools.StatusError {
			// The child ran but its task FAILED (e.g. non-zero exit / max-turns).
			// Surface it as a member failure so the swarm marks it [failed], not
			// [done] — otherwise the orchestrator (which can't see the AGENTS panel)
			// trusts incomplete work. Keep the session id so the failed member is
			// still drillable, and carry its report as the failure message.
			return MemberResult{SessionID: res.SessionID}, errors.New(res.Result.Output)
		}
		return MemberResult{Result: res.Result.Output, SessionID: res.SessionID}, nil
	}}
}

// swarmMemberToolGroups is the tool allowance for a swarm member running as a
// specialist. It mirrors the built-in "worker" specialist (a general-purpose
// task executor) since swarm members execute delegated work; the member still
// runs under the orchestrator's clamped permission mode, so write/execute tools
// stay gated unless the orchestrator is itself unsafe.
var swarmMemberToolGroups = []string{"read-only", "edit", "execute", "plan"}

// specialistManifestForMember builds an inline specialist manifest from a swarm
// member spec so the executor runs the member with the swarm definition's system
// prompt and a general toolset, without requiring a registered specialist.
func specialistManifestForMember(spec MemberSpec) specialist.Manifest {
	return specialist.Manifest{
		Metadata: specialist.Metadata{
			Name:        spec.AgentType,
			Description: "Swarm " + spec.AgentType + " member.",
			Tools:       swarmMemberToolGroups,
		},
		SystemPrompt: spec.SystemPrompt,
		Location:     specialist.LocationBuiltin,
		FilePath:     "(swarm)",
	}
}
