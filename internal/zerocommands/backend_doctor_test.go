package zerocommands

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/hooks"
	"github.com/Twigpine/zero/internal/plugins"
)

func TestNewBackendDoctorReportSurfacesDiagnosticsAndActions(t *testing.T) {
	secret := "sk-proj-" + strings.Repeat("a", 24)
	report := NewBackendDoctorReport(BackendDoctorInput{
		MCP: config.MCPConfig{Servers: map[string]config.MCPServerConfig{
			"remote": {
				Type: "http",
				URL:  "https://api.example.com/mcp?token=" + secret,
			},
			"broken": {
				Type: "http",
			},
			"disabled": {
				Type:     "stdio",
				Command:  "docs-mcp",
				Disabled: true,
			},
		}},
		Hooks: hooks.LoadResult{
			Config: hooks.Config{Enabled: false, Hooks: []hooks.Definition{{
				ID:      "zero.preflight",
				Event:   hooks.EventBeforeTool,
				Command: "sh",
				Enabled: true,
			}}},
			Diagnostics: []hooks.Diagnostic{{
				Kind:      hooks.DiagnosticSchema,
				Message:   "bad arg " + secret,
				Path:      "/tmp/" + secret + "/hooks.json",
				HookID:    "zero.preflight-" + secret,
				FieldPath: "hooks.0.command." + secret,
			}},
		},
		Plugins: plugins.LoadResult{
			Plugins: []plugins.LoadedPlugin{{
				ID:      "zero.docs",
				Name:    "Docs",
				Enabled: false,
				Source:  plugins.SourceProject,
			}},
			Diagnostics: []plugins.Diagnostic{{
				Kind:         plugins.DiagnosticDuplicate,
				Message:      "duplicate " + secret,
				Root:         "/tmp/" + secret + "/plugins",
				PluginPath:   "/tmp/" + secret + "/plugins/docs",
				ManifestPath: "/tmp/plugin.json?token=" + secret,
				FieldPath:    "tools.0.command." + secret,
				PluginID:     "zero.docs-" + secret,
			}},
		},
	})

	if report.OK {
		t.Fatalf("report.OK = true, want false because broken MCP and hook schema diagnostics exist: %#v", report.Checks)
	}
	if report.Status != BackendDoctorStatusFail {
		t.Fatalf("report.Status = %q, want %q", report.Status, BackendDoctorStatusFail)
	}
	assertBackendDoctorCheck(t, report, "backend.mcp.server", "remote", BackendDoctorStatusPass, "zero mcp check remote")
	assertBackendDoctorCheck(t, report, "backend.mcp.invalid", "broken", BackendDoctorStatusFail, "zero mcp add broken")
	assertBackendDoctorCheck(t, report, "backend.mcp.disabled", "disabled", BackendDoctorStatusWarn, "zero mcp enable disabled")
	assertBackendDoctorCheck(t, report, "backend.hooks.disabled", "hooks", BackendDoctorStatusWarn, "zero hooks list")
	assertBackendDoctorCheck(t, report, "backend.hooks.diagnostic", "zero.preflight-[REDACTED]", BackendDoctorStatusFail, "zero hooks list")
	assertBackendDoctorCheck(t, report, "backend.plugins.disabled", "zero.docs", BackendDoctorStatusWarn, "zero plugins list")
	assertBackendDoctorCheck(t, report, "backend.plugins.diagnostic", "zero.docs-[REDACTED]", BackendDoctorStatusWarn, "zero plugins list")

	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "sk-proj-") {
		t.Fatalf("backend doctor report leaked secret material: %s", string(encoded))
	}
}

func TestNewBackendDoctorReportPassesEmptySetup(t *testing.T) {
	report := NewBackendDoctorReport(BackendDoctorInput{})
	if !report.OK {
		t.Fatalf("empty setup should be a passing report, got %#v", report.Checks)
	}
	if report.Status != BackendDoctorStatusPass {
		t.Fatalf("report.Status = %q, want %q", report.Status, BackendDoctorStatusPass)
	}
	assertBackendDoctorCheck(t, report, "backend.mcp.configured", "mcp", BackendDoctorStatusPass, "zero mcp add")
	assertBackendDoctorCheck(t, report, "backend.hooks.configured", "hooks", BackendDoctorStatusPass, "zero hooks list")
	assertBackendDoctorCheck(t, report, "backend.plugins.configured", "plugins", BackendDoctorStatusPass, "zero plugins list")
}

func TestNewBackendDoctorReportWarnsWithoutFailing(t *testing.T) {
	report := NewBackendDoctorReport(BackendDoctorInput{
		MCP: config.MCPConfig{Servers: map[string]config.MCPServerConfig{
			"disabled": {Type: "stdio", Command: "docs-mcp", Disabled: true},
		}},
	})
	if !report.OK {
		t.Fatalf("warning-only report should keep OK=true, got %#v", report.Checks)
	}
	if report.Status != BackendDoctorStatusWarn {
		t.Fatalf("report.Status = %q, want %q", report.Status, BackendDoctorStatusWarn)
	}
}

func assertBackendDoctorCheck(t *testing.T, report BackendDoctorReport, id string, target string, status BackendDoctorStatus, actionContains string) {
	t.Helper()
	for _, check := range report.Checks {
		if check.ID == id && check.Target == target {
			if check.Status != status {
				t.Fatalf("%s/%s status = %q, want %q (check %#v)", id, target, check.Status, status, check)
			}
			if actionContains != "" && !strings.Contains(check.Action, actionContains) {
				t.Fatalf("%s/%s action = %q, want to contain %q", id, target, check.Action, actionContains)
			}
			return
		}
	}
	t.Fatalf("check %s/%s not found in %#v", id, target, report.Checks)
}
