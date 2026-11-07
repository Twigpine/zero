package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/doctor"
	"github.com/Twigpine/zero/internal/zerocommands"
)

func TestDoctorCommandOutputMapsOverallStatus(t *testing.T) {
	tests := []struct {
		name   string
		checks []doctor.Check
		want   commandStatus
	}{
		{
			name: "all pass is ok",
			checks: []doctor.Check{
				doctorCheck("provider.config", doctor.StatusPass, "Provider config loaded."),
				doctorCheck("runtime.go", doctor.StatusPass, "Go runtime is available."),
			},
			want: commandStatusOK,
		},
		{
			name: "warnings only is warning",
			checks: []doctor.Check{
				doctorCheck("provider.config", doctor.StatusPass, "Provider config loaded."),
				doctorCheck("lsp.servers", doctor.StatusWarn, "1 language server missing from PATH."),
			},
			want: commandStatusWarning,
		},
		{
			name: "any failure is blocked",
			checks: []doctor.Check{
				doctorCheck("provider.config", doctor.StatusFail, "No LLM provider is configured."),
				doctorCheck("runtime.go", doctor.StatusPass, "Go runtime is available."),
			},
			want: commandStatusBlocked,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output := doctorCommandOutput(doctor.Report{GeneratedAt: "2026-06-14T00:00:00Z", Checks: test.checks}, nil)
			if output.Status != test.want {
				t.Fatalf("Status = %q, want %q", output.Status, test.want)
			}
		})
	}
}

func TestDoctorSummaryLinesUseSingularGrammar(t *testing.T) {
	healthy := doctorSummaryLines([]doctor.Check{
		doctorCheck("runtime.go", doctor.StatusPass, "Go runtime is available."),
	})
	if got, want := healthy[0], "1 check healthy"; got != want {
		t.Fatalf("healthy summary = %q, want %q", got, want)
	}

	attention := doctorSummaryLines([]doctor.Check{
		doctorCheck("provider.model", doctor.StatusWarn, "Provider model is not configured."),
	})
	if got, want := attention[0], "1 check needs attention"; got != want {
		t.Fatalf("attention summary = %q, want %q", got, want)
	}
}

func TestDoctorResultBorderStyleUsesSummaryStatusLine(t *testing.T) {
	text := strings.Join([]string{
		"Diagnostics",
		"",
		"status: blocked",
		"",
		"Summary",
		"- status: ok appears in detail text only",
	}, "\n")

	got := fmt.Sprint(doctorResultBorderStyle(text).GetForeground())
	want := fmt.Sprint(zeroTheme.red.GetForeground())
	if got != want {
		t.Fatalf("border foreground = %s, want blocked red %s", got, want)
	}
}

func TestDoctorCommandOutputGroupsProviderAndPlatformChecks(t *testing.T) {
	output := doctorCommandOutput(doctor.Report{Checks: []doctor.Check{
		doctorCheck("provider.config", doctor.StatusPass, "Provider config loaded."),
		doctorCheck("provider.model", doctor.StatusWarn, "Provider model is not configured."),
		doctorCheck("provider.connectivity", doctor.StatusFail, "Provider connectivity failed."),
		doctorCheck("sandbox.backend", doctor.StatusWarn, "Native sandbox backend unavailable on windows: Windows sandbox command runner is not available."),
		doctorCheck("runtime.go", doctor.StatusPass, "Zero Go runtime is available."),
		doctorCheck("config.files", doctor.StatusPass, "Zero config file inputs are available."),
	}}, nil)

	provider := doctorSection(output, "Provider")
	if provider == nil {
		t.Fatalf("missing Provider section: %#v", output.Sections)
	}
	assertRowsContain(t, provider.Rows, "provider.model", "provider.connectivity")
	assertRowsDoNotContain(t, provider.Rows, "provider.config")

	platform := doctorSection(output, "Platform")
	if platform == nil {
		t.Fatalf("missing Platform section: %#v", output.Sections)
	}
	assertRowsContain(t, platform.Rows, "sandbox.backend")
	assertRowsDoNotContain(t, platform.Rows, "runtime.go", "config.files")
}

func TestDoctorCommandOutputIsProblemFirstDiagnosticCenter(t *testing.T) {
	output := doctorCommandOutput(doctor.Report{GeneratedAt: "2026-06-14T00:00:00Z", Checks: []doctor.Check{
		doctorCheck("provider.config", doctor.StatusPass, "Provider config loaded."),
		doctorCheck("provider.model", doctor.StatusWarn, "Provider model is not configured."),
		doctorCheck("provider.connectivity", doctor.StatusWarn, "Connectivity probe skipped."),
		doctorCheck("runtime.go", doctor.StatusPass, "Zero Go runtime is available."),
		doctorCheck("config.files", doctor.StatusPass, "Zero config files are available."),
		doctorCheck("lsp.servers", doctor.StatusWarn, "2 language server(s) missing from PATH."),
	}}, nil)

	text := formatCommandOutput(output)
	for _, want := range []string{
		"3 checks need attention",
		"3 checks healthy",
		"provider.model",
		"provider.connectivity",
		"lsp.servers",
		"Actions",
		"/doctor fix",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("formatted output missing %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{
		"Generated",
		"Checks",
		"provider.config",
		"runtime.go",
		"config.files",
	} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("formatted output should hide %q:\n%s", unwanted, text)
		}
	}
}

func TestDoctorCommandOutputAddsActionableHints(t *testing.T) {
	output := doctorCommandOutput(doctor.Report{Checks: []doctor.Check{
		doctorCheck("provider.config", doctor.StatusFail, "No LLM provider is configured."),
		doctorCheck("provider.model", doctor.StatusWarn, "Provider model is not configured."),
		doctorCheck("provider.connectivity", doctor.StatusWarn, "Provider connectivity was skipped."),
		{
			ID:      "sandbox.backend",
			Label:   "Sandbox backend",
			Status:  doctor.StatusWarn,
			Message: "Native sandbox backend unavailable on windows: Windows sandbox setup helper is not available.",
			Details: map[string]any{
				"remedy": "install the Windows sandbox command runner and setup helper together, then run `zero sandbox setup`",
			},
		},
		doctorCheck("lsp.servers", doctor.StatusWarn, "2 language server(s) missing from PATH; affected files degrade to text-only edits."),
	}}, nil)

	text := formatCommandOutput(output)
	for _, want := range []string{
		"/provider",
		"/doctor --connectivity",
		"zero sandbox setup",
		"install missing language servers",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("formatted output missing %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"WSL2", "Linux container"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("formatted output should not contain stale sandbox guidance %q:\n%s", unwanted, text)
		}
	}
}

func TestDoctorCommandOutputIsNilBackendSafe(t *testing.T) {
	output := doctorCommandOutput(doctor.Report{Checks: []doctor.Check{
		doctorCheck("runtime.go", doctor.StatusPass, "Go runtime is available."),
	}}, nil)

	if section := doctorSection(output, "Backend"); section != nil {
		t.Fatalf("unexpected Backend section for nil backend: %#v", section)
	}
}

func TestDoctorCommandOutputIncludesBackendCountsAndHints(t *testing.T) {
	backend := zerocommands.BackendLifecycleSnapshot{
		MCPServers: []zerocommands.MCPServerSnapshot{{Name: "filesystem"}, {Name: "github"}},
		Hooks:      []zerocommands.HookSnapshot{{ID: "lint", Enabled: true}},
		Plugins:    []zerocommands.PluginSnapshot{{ID: "browser"}, {ID: "github"}, {ID: "linear"}},
	}

	output := doctorCommandOutput(doctor.Report{Checks: []doctor.Check{
		doctorCheck("runtime.go", doctor.StatusPass, "Go runtime is available."),
	}}, &backend)

	section := doctorSection(output, "Backend")
	if section == nil {
		t.Fatalf("missing Backend section: %#v", output.Sections)
	}
	assertField(t, section.Fields, "MCP servers", "2")
	assertField(t, section.Fields, "Hooks", "1")
	assertField(t, section.Fields, "Plugins", "3")

	text := formatCommandOutput(output)
	for _, want := range []string{"/mcp", "/hooks", "/plugins"} {
		if !strings.Contains(text, want) {
			t.Fatalf("formatted output missing %q:\n%s", want, text)
		}
	}
}

func doctorCheck(id string, status doctor.Status, message string) doctor.Check {
	return doctor.Check{
		ID:      id,
		Label:   id,
		Status:  status,
		Message: message,
	}
}

func doctorSection(output commandOutput, title string) *commandSection {
	for index := range output.Sections {
		if output.Sections[index].Title == title {
			return &output.Sections[index]
		}
	}
	return nil
}

func assertRowsContain(t *testing.T, rows []commandRow, wants ...string) {
	t.Helper()
	text := ""
	for _, row := range rows {
		text += row.Text + "\n"
	}
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Fatalf("rows missing %q:\n%s", want, text)
		}
	}
}

func assertRowsDoNotContain(t *testing.T, rows []commandRow, wants ...string) {
	t.Helper()
	text := ""
	for _, row := range rows {
		text += row.Text + "\n"
	}
	for _, want := range wants {
		if strings.Contains(text, want) {
			t.Fatalf("rows unexpectedly contain %q:\n%s", want, text)
		}
	}
}

func assertField(t *testing.T, fields []commandField, key string, value string) {
	t.Helper()
	for _, field := range fields {
		if field.Key == key && field.Value == value {
			return
		}
	}
	t.Fatalf("missing field %s=%s in %#v", key, value, fields)
}
