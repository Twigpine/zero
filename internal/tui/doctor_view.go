package tui

import (
	"fmt"
	"strings"

	"github.com/Twigpine/zero/internal/doctor"
	"github.com/Twigpine/zero/internal/zerocommands"
)

func doctorCommandOutput(report doctor.Report, backend *zerocommands.BackendLifecycleSnapshot) commandOutput {
	sections := []commandSection{{
		Title: "Summary",
		Lines: doctorSummaryLines(report.Checks),
	}}

	provider, platform, other := doctorCheckSections(report.Checks)
	sections = appendNonEmptyDoctorSection(sections, "Provider", provider)
	sections = appendNonEmptyDoctorSection(sections, "Platform", platform)
	sections = appendNonEmptyDoctorSection(sections, "Other", other)
	if backend != nil {
		sections = append(sections, doctorBackendSection(*backend))
	}
	if actions := doctorActions(report.Checks, backend); len(actions) > 0 {
		sections = append(sections, commandSection{
			Title: "Actions",
			Lines: actions,
		})
	}

	return commandOutput{
		Title:    "Diagnostics",
		Status:   doctorCommandStatus(report),
		Sections: sections,
	}
}

func doctorCommandStatus(report doctor.Report) commandStatus {
	hasWarning := false
	for _, check := range report.Checks {
		switch check.Status {
		case doctor.StatusFail:
			return commandStatusBlocked
		case doctor.StatusWarn:
			hasWarning = true
		}
	}
	if hasWarning {
		return commandStatusWarning
	}
	return commandStatusOK
}

func doctorCheckSections(checks []doctor.Check) (provider []commandRow, platform []commandRow, other []commandRow) {
	for _, check := range checks {
		if check.Status == doctor.StatusPass && check.ID != "provider.connectivity" {
			continue
		}
		row := commandRow{Text: doctorCheckRow(check)}
		switch doctorCheckGroup(check.ID) {
		case "provider":
			provider = append(provider, row)
		case "platform":
			platform = append(platform, row)
		default:
			other = append(other, row)
		}
	}
	return provider, platform, other
}

func doctorCheckGroup(id string) string {
	switch id {
	case "provider.config", "provider.model", "provider.connectivity", "provider.auth", "provider.runtime":
		return "provider"
	case "sandbox.backend", "lsp.servers", "runtime.go", "config.files", "config.validation":
		return "platform"
	default:
		return ""
	}
}

func doctorSummaryLines(checks []doctor.Check) []string {
	pass, warn, fail := doctorStatusCounts(checks)
	attention := warn + fail
	if attention == 0 {
		return []string{
			fmt.Sprintf("%s healthy", doctorCheckCountLabel(pass)),
			"All core systems are ready.",
		}
	}
	return []string{
		fmt.Sprintf("%s %s attention", doctorCheckCountLabel(attention), doctorAttentionVerb(attention)),
		fmt.Sprintf("%s healthy", doctorCheckCountLabel(pass)),
	}
}

func doctorCheckCountLabel(count int) string {
	if count == 1 {
		return "1 check"
	}
	return fmt.Sprintf("%d checks", count)
}

func doctorAttentionVerb(count int) string {
	if count == 1 {
		return "needs"
	}
	return "need"
}

func doctorStatusCounts(checks []doctor.Check) (pass int, warn int, fail int) {
	for _, check := range checks {
		switch check.Status {
		case doctor.StatusPass:
			pass++
		case doctor.StatusWarn:
			warn++
		case doctor.StatusFail:
			fail++
		}
	}
	return pass, warn, fail
}

func doctorCheckRow(check doctor.Check) string {
	parts := []string{fmt.Sprintf("[%s]", check.Status)}
	if check.ID != "" {
		parts = append(parts, check.ID)
	}
	if check.Message != "" {
		parts = append(parts, "-", check.Message)
	}
	return strings.Join(parts, " ")
}

func appendNonEmptyDoctorSection(sections []commandSection, title string, rows []commandRow) []commandSection {
	if len(rows) == 0 {
		return sections
	}
	return append(sections, commandSection{
		Title: title,
		Rows:  rows,
	})
}

func doctorBackendSection(backend zerocommands.BackendLifecycleSnapshot) commandSection {
	return commandSection{
		Title: "Backend",
		Fields: []commandField{
			{Key: "MCP servers", Value: fmt.Sprintf("%d", len(backend.MCPServers))},
			{Key: "Hooks", Value: fmt.Sprintf("%d", len(backend.Hooks))},
			{Key: "Plugins", Value: fmt.Sprintf("%d", len(backend.Plugins))},
		},
	}
}

func doctorActions(checks []doctor.Check, backend *zerocommands.BackendLifecycleSnapshot) []string {
	seen := map[string]bool{}
	actions := []string{}
	add := func(action string) {
		action = strings.TrimSpace(action)
		if action == "" || seen[action] {
			return
		}
		seen[action] = true
		actions = append(actions, action)
	}

	for _, check := range checks {
		if check.Status == doctor.StatusPass {
			continue
		}
		message := strings.ToLower(check.Message)
		switch check.ID {
		case "provider.config", "provider.model":
			add("/provider - configure the active provider and model")
			add("/doctor fix - open guided diagnostics repair")
		case "provider.connectivity":
			add("/doctor --connectivity - probe the provider endpoint")
		case "sandbox.backend":
			if remedy := doctorCheckDetailString(check, "remedy"); remedy != "" {
				add(remedy)
			} else if strings.Contains(message, "unavailable") {
				add("zero sandbox policy --effective - inspect sandbox backend and enforcement status")
			}
		case "lsp.servers":
			if strings.Contains(message, "missing") {
				add("install missing language servers - restore code intelligence on PATH")
			}
		}
	}

	if backend != nil {
		add("/mcp - inspect MCP servers")
		add("/hooks - inspect hooks")
		add("/plugins - inspect plugins")
	}
	return actions
}

func doctorCheckDetailString(check doctor.Check, key string) string {
	if check.Details == nil {
		return ""
	}
	value, _ := check.Details[key].(string)
	return strings.TrimSpace(value)
}
