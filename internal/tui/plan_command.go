package tui

import (
	"fmt"
	"strings"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/tools"
)

type currentPlanReader interface {
	CurrentPlan() []tools.PlanItem
}

// handlePlanCommand drives /plan: bare or "status" just reports the current
// plan (pre-existing behavior); "on" and "off" are the entry/exit path into
// PermissionModePlan. Unlike /spec (which drafts in a separate, forked
// session), plan mode applies to the CURRENT session, so entering/exiting it
// is a direct m.permissionMode flip rather than a run-option override.
func (m model) handlePlanCommand(args string) (model, string) {
	switch strings.ToLower(strings.TrimSpace(args)) {
	case "", "status":
		return m, m.planText()
	case "on":
		if m.pending {
			return m, "Cannot change plan mode while a turn is active."
		}
		if m.permissionMode == agent.PermissionModePlan {
			return m, "Plan mode\nAlready active. Write and shell tools stay hidden until /plan off."
		}
		m.permissionModeBeforePlan = m.permissionMode
		m.permissionMode = agent.PermissionModePlan
		return m, "Plan mode\nActive: read-only planning. Write and shell tools are hidden until /plan off."
	case "off":
		if m.pending {
			return m, "Cannot change plan mode while a turn is active."
		}
		if m.permissionMode != agent.PermissionModePlan {
			return m, "Plan mode\nNot currently active."
		}
		restored := m.permissionModeBeforePlan
		if restored == "" {
			restored = agent.PermissionModeAuto
		}
		m.permissionMode = restored
		m.permissionModeBeforePlan = ""
		return m, "Plan mode\nExited. Permission mode restored to " + string(restored) + "."
	default:
		return m, "Plan mode\nUsage: /plan [status|on|off]"
	}
}

// planModeCommandUnavailable reports whether a local (non-tool) TUI command
// must be blocked while plan mode is active. Plan mode's tool-advertisement
// gate only covers agent tool calls; these commands run entirely inside the
// TUI process and would mutate the workspace or spawn a host process outside
// that gate: /rewind restores files from a checkpoint, /export writes a
// transcript file to disk, /sandbox-setup runs native platform setup,
// /spec forks a drafting session, /mcp <sub> mutates server configuration, and
// /init's whole job is writing AGENTS.md (which plan mode then denies).
// Bare /mcp (empty text) only opens the read-only manager view, so it stays
// available. Modeled on btwCommandUnavailable's shape for the analogous BTW guard.
func planModeCommandUnavailable(command parsedCommand) bool {
	switch command.kind {
	case commandRewind, commandExport, commandSandboxSetup, commandSpec, commandInit:
		return true
	case commandMCP:
		// Bare /mcp only opens the read-only manager view; subcommands mutate config.
		return strings.TrimSpace(command.text) != ""
	default:
		return false
	}
}

func (m model) planText() string {
	tool, ok := m.registry.Get("update_plan")
	if !ok {
		return "No plan is active."
	}

	reader, ok := tool.(currentPlanReader)
	if !ok {
		return "No plan is active."
	}

	plan := reader.CurrentPlan()
	if len(plan) == 0 {
		return "No plan is active."
	}

	lines := make([]string, 0, len(plan)+1)
	lines = append(lines, "Current Plan")
	for index, item := range plan {
		line := fmt.Sprintf("%d. [%s] %s", index+1, item.Status, item.Content)
		if item.Notes != "" {
			line += "\n   Notes: " + item.Notes
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}
