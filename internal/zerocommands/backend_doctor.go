package zerocommands

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/hooks"
	"github.com/Twigpine/zero/internal/mcp"
	"github.com/Twigpine/zero/internal/plugins"
	"github.com/Twigpine/zero/internal/redaction"
)

type BackendDoctorStatus string

const (
	BackendDoctorStatusPass BackendDoctorStatus = "pass"
	BackendDoctorStatusWarn BackendDoctorStatus = "warn"
	BackendDoctorStatusFail BackendDoctorStatus = "fail"
)

type BackendDoctorInput struct {
	MCP     config.MCPConfig
	Hooks   hooks.LoadResult
	Plugins plugins.LoadResult
}

type BackendDoctorReport struct {
	OK     bool                 `json:"ok"`
	Status BackendDoctorStatus  `json:"status"`
	Checks []BackendDoctorCheck `json:"checks"`
}

type BackendDoctorCheck struct {
	ID      string              `json:"id"`
	Surface string              `json:"surface"`
	Target  string              `json:"target"`
	Status  BackendDoctorStatus `json:"status"`
	Message string              `json:"message"`
	Action  string              `json:"action,omitempty"`
	Details map[string]string   `json:"details,omitempty"`
}

func NewBackendDoctorReport(input BackendDoctorInput) BackendDoctorReport {
	checks := []BackendDoctorCheck{}
	checks = append(checks, mcpDoctorChecks(input.MCP)...)
	checks = append(checks, hookDoctorChecks(input.Hooks)...)
	checks = append(checks, pluginDoctorChecks(input.Plugins)...)

	report := BackendDoctorReport{OK: true, Status: BackendDoctorStatusPass, Checks: checks}
	for _, check := range checks {
		if check.Status == BackendDoctorStatusFail {
			report.OK = false
			report.Status = BackendDoctorStatusFail
			break
		}
		if check.Status == BackendDoctorStatusWarn && report.Status == BackendDoctorStatusPass {
			report.Status = BackendDoctorStatusWarn
		}
	}
	return report
}

func mcpDoctorChecks(cfg config.MCPConfig) []BackendDoctorCheck {
	if len(cfg.Servers) == 0 {
		return []BackendDoctorCheck{{
			ID:      "backend.mcp.configured",
			Surface: "mcp",
			Target:  "mcp",
			Status:  BackendDoctorStatusPass,
			Message: "No MCP servers configured.",
			Action:  "zero mcp add <name> --url <url>",
		}}
	}

	names := make([]string, 0, len(cfg.Servers))
	for name := range cfg.Servers {
		names = append(names, name)
	}
	sort.Strings(names)

	checks := make([]BackendDoctorCheck, 0, len(names))
	for _, name := range names {
		raw := cfg.Servers[name]
		target := redactSnapshotString(name)
		if raw.Disabled {
			checks = append(checks, BackendDoctorCheck{
				ID:      "backend.mcp.disabled",
				Surface: "mcp",
				Target:  target,
				Status:  BackendDoctorStatusWarn,
				Message: fmt.Sprintf("MCP server %s is disabled.", target),
				Action:  "zero mcp enable " + target,
			})
			continue
		}

		servers, err := mcp.NormalizeConfig(config.MCPConfig{Servers: map[string]config.MCPServerConfig{name: raw}})
		if err != nil {
			checks = append(checks, BackendDoctorCheck{
				ID:      "backend.mcp.invalid",
				Surface: "mcp",
				Target:  target,
				Status:  BackendDoctorStatusFail,
				Message: redaction.RedactString(err.Error(), redaction.Options{}),
				Action:  "zero mcp add " + target,
			})
			continue
		}
		detail := ""
		if len(servers) == 1 {
			detail = string(servers[0].Type)
		}
		check := BackendDoctorCheck{
			ID:      "backend.mcp.server",
			Surface: "mcp",
			Target:  target,
			Status:  BackendDoctorStatusPass,
			Message: fmt.Sprintf("MCP server %s is configured.", target),
			Action:  "zero mcp check " + target,
		}
		if detail != "" {
			check.Details = map[string]string{"type": detail}
		}
		checks = append(checks, check)
	}
	return checks
}

func hookDoctorChecks(result hooks.LoadResult) []BackendDoctorCheck {
	checks := []BackendDoctorCheck{}
	if len(result.Config.Hooks) == 0 && len(result.Diagnostics) == 0 {
		checks = append(checks, BackendDoctorCheck{
			ID:      "backend.hooks.configured",
			Surface: "hooks",
			Target:  "hooks",
			Status:  BackendDoctorStatusPass,
			Message: "No hooks configured.",
			Action:  "zero hooks list",
		})
	}
	if len(result.Config.Hooks) > 0 && !result.Config.Enabled {
		checks = append(checks, BackendDoctorCheck{
			ID:      "backend.hooks.disabled",
			Surface: "hooks",
			Target:  "hooks",
			Status:  BackendDoctorStatusWarn,
			Message: "Hooks are configured but globally disabled.",
			Action:  "zero hooks list",
		})
	}
	for _, diagnostic := range result.Diagnostics {
		target := firstNonEmpty(diagnostic.HookID, diagnostic.FieldPath, string(diagnostic.Source), "hooks")
		checks = append(checks, BackendDoctorCheck{
			ID:      "backend.hooks.diagnostic",
			Surface: "hooks",
			Target:  redactSnapshotString(target),
			Status:  hookDiagnosticStatus(diagnostic.Kind),
			Message: redactSnapshotString(diagnostic.Message),
			Action:  "zero hooks list",
			Details: compactDetails(map[string]string{
				"kind":      string(diagnostic.Kind),
				"source":    string(diagnostic.Source),
				"path":      diagnostic.Path,
				"fieldPath": diagnostic.FieldPath,
			}),
		})
	}
	if len(checks) == 0 {
		checks = append(checks, BackendDoctorCheck{
			ID:      "backend.hooks.configured",
			Surface: "hooks",
			Target:  "hooks",
			Status:  BackendDoctorStatusPass,
			Message: fmt.Sprintf("%d hooks configured.", len(result.Config.Hooks)),
			Action:  "zero hooks list",
		})
	}
	return checks
}

func pluginDoctorChecks(result plugins.LoadResult) []BackendDoctorCheck {
	checks := []BackendDoctorCheck{}
	if len(result.Plugins) == 0 && len(result.Diagnostics) == 0 {
		checks = append(checks, BackendDoctorCheck{
			ID:      "backend.plugins.configured",
			Surface: "plugins",
			Target:  "plugins",
			Status:  BackendDoctorStatusPass,
			Message: "No plugins loaded.",
			Action:  "zero plugins list",
		})
	}
	for _, plugin := range result.Plugins {
		if plugin.Enabled {
			continue
		}
		target := redactSnapshotString(plugin.ID)
		checks = append(checks, BackendDoctorCheck{
			ID:      "backend.plugins.disabled",
			Surface: "plugins",
			Target:  target,
			Status:  BackendDoctorStatusWarn,
			Message: fmt.Sprintf("Plugin %s is disabled.", target),
			Action:  "zero plugins list",
		})
	}
	for _, diagnostic := range result.Diagnostics {
		target := firstNonEmpty(diagnostic.PluginID, diagnostic.FieldPath, string(diagnostic.Source), "plugins")
		checks = append(checks, BackendDoctorCheck{
			ID:      "backend.plugins.diagnostic",
			Surface: "plugins",
			Target:  redactSnapshotString(target),
			Status:  pluginDiagnosticStatus(diagnostic.Kind),
			Message: redactSnapshotString(diagnostic.Message),
			Action:  "zero plugins list",
			Details: compactDetails(map[string]string{
				"kind":         string(diagnostic.Kind),
				"source":       string(diagnostic.Source),
				"root":         diagnostic.Root,
				"pluginPath":   diagnostic.PluginPath,
				"manifestPath": diagnostic.ManifestPath,
				"fieldPath":    diagnostic.FieldPath,
			}),
		})
	}
	if len(checks) == 0 {
		checks = append(checks, BackendDoctorCheck{
			ID:      "backend.plugins.configured",
			Surface: "plugins",
			Target:  "plugins",
			Status:  BackendDoctorStatusPass,
			Message: fmt.Sprintf("%d plugins loaded.", len(result.Plugins)),
			Action:  "zero plugins list",
		})
	}
	return checks
}

func hookDiagnosticStatus(kind hooks.DiagnosticKind) BackendDoctorStatus {
	if kind == hooks.DiagnosticDuplicate {
		return BackendDoctorStatusWarn
	}
	return BackendDoctorStatusFail
}

func pluginDiagnosticStatus(kind plugins.DiagnosticKind) BackendDoctorStatus {
	if kind == plugins.DiagnosticDuplicate {
		return BackendDoctorStatusWarn
	}
	return BackendDoctorStatusFail
}

func compactDetails(values map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range values {
		value = redactSnapshotString(value)
		if value != "" {
			out[key] = value
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
