package tui

import (
	"context"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/doctor"
	"github.com/Twigpine/zero/internal/providerhealth"
)

func TestParseDoctorCommandArgsRejectsFixWithConnectivity(t *testing.T) {
	_, _, _, err := parseDoctorCommandArgs("fix --connectivity")
	if err == nil {
		t.Fatal("parseDoctorCommandArgs accepted both fix and --connectivity")
	}
	if !strings.Contains(err.Error(), "choose either") {
		t.Fatalf("error = %q, want mutually exclusive flag guidance", err)
	}
}

func TestDoctorOptionsIncludeConfigPaths(t *testing.T) {
	m := newModel(context.Background(), Options{
		UserConfigPath:    "C:/zero/user.json",
		ProjectConfigPath: "C:/repo/.zero/config.json",
		ProviderProfile: config.ProviderProfile{
			Name:         "openai",
			ProviderKind: config.ProviderKindOpenAI,
			Model:        "gpt-4.1",
		},
	})

	options := m.doctorOptions(false)

	if options.UserConfig != "C:/zero/user.json" {
		t.Fatalf("UserConfig = %q, want %q", options.UserConfig, "C:/zero/user.json")
	}
	if options.ProjectConfig != "C:/repo/.zero/config.json" {
		t.Fatalf("ProjectConfig = %q, want %q", options.ProjectConfig, "C:/repo/.zero/config.json")
	}
	if options.Connectivity {
		t.Fatal("Connectivity = true, want false")
	}
	if options.ProviderHealth != nil {
		t.Fatalf("ProviderHealth = %#v, want nil without connectivity", options.ProviderHealth)
	}
}

func TestDoctorOptionsConnectivityInvokesConfiguredProviderHealthProbe(t *testing.T) {
	profile := config.ProviderProfile{
		Name:         "custom",
		ProviderKind: config.ProviderKindOpenAICompatible,
		BaseURL:      "https://api.example.com/v1",
		Model:        "custom-model",
	}
	var called int
	var gotOptions providerhealth.Options
	m := newModel(context.Background(), Options{
		ProviderProfile: profile,
		UserAgent:       "zero-test",
		ProbeProviderHealth: func(_ context.Context, options providerhealth.Options) providerhealth.Result {
			called++
			gotOptions = options
			return providerhealth.Result{
				Status: providerhealth.StatusPass,
				Checks: []providerhealth.Check{{
					ID:      "provider.connectivity",
					Status:  providerhealth.StatusPass,
					Message: "reachable",
				}},
			}
		},
	})

	options := m.doctorOptions(true)
	report := doctor.Run(options)

	if called != 1 {
		t.Fatalf("probe called %d time(s), want 1", called)
	}
	if !gotOptions.Connectivity {
		t.Fatal("probe Connectivity = false, want true")
	}
	if gotOptions.UserAgent != "zero-test" {
		t.Fatalf("probe UserAgent = %q, want %q", gotOptions.UserAgent, "zero-test")
	}
	if !reflect.DeepEqual(gotOptions.Profile, profile) {
		t.Fatalf("probe Profile = %#v, want %#v", gotOptions.Profile, profile)
	}
	check := report.Check("provider.connectivity")
	if options.ProviderHealth == nil || check == nil || check.Status != doctor.StatusPass {
		t.Fatalf("connectivity check = %#v, ProviderHealth = %#v; want passing injected health", check, options.ProviderHealth)
	}
}

func TestDoctorConnectivityCommandRunsProbeAsynchronously(t *testing.T) {
	profile := config.ProviderProfile{
		Name:         "custom",
		ProviderKind: config.ProviderKindOpenAICompatible,
		BaseURL:      "https://api.example.com/v1",
		Model:        "custom-model",
	}
	called := false
	m := newModel(context.Background(), Options{
		ProviderProfile: profile,
		ProbeProviderHealth: func(context.Context, providerhealth.Options) providerhealth.Result {
			called = true
			return providerhealth.Result{
				Status: providerhealth.StatusPass,
				Checks: []providerhealth.Check{{
					ID:      "provider.connectivity",
					Status:  providerhealth.StatusPass,
					Message: "reachable",
				}},
			}
		},
	})
	m.input.SetValue("/doctor --connectivity")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd == nil {
		t.Fatal("expected /doctor --connectivity to return an async command")
	}
	if called {
		t.Fatal("provider probe ran synchronously before the returned command executed")
	}
	if !transcriptContains(next.transcript, "checking provider connectivity") {
		t.Fatalf("expected running doctor status, got %#v", next.transcript)
	}

	msg := execCmd(cmd)
	if !called {
		t.Fatal("provider probe did not run when the async command executed")
	}
	updated, _ = next.Update(msg)
	final := updated.(model)
	for _, want := range []string{"Diagnostics", "[pass] provider.connectivity", "reachable"} {
		if !transcriptContains(final.transcript, want) {
			t.Fatalf("expected final doctor transcript to contain %q, got %#v", want, final.transcript)
		}
	}
}

func TestDoctorCommandUsesDiagnosticCenterRow(t *testing.T) {
	m := newModel(context.Background(), Options{
		ProviderProfile: config.ProviderProfile{
			Name:         "custom",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "https://api.example.com/v1",
			Model:        "custom-model",
		},
	})
	m.input.SetValue("/doctor")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd != nil {
		t.Fatal("expected plain /doctor to render synchronously")
	}
	row := newestDoctorStatusRow(next.transcript)
	if row == nil {
		t.Fatalf("expected /doctor to render a doctor status row, got %#v", next.transcript)
	}
	if row.id != doctorStatusRowID {
		t.Fatalf("expected /doctor row id %q, got %q", doctorStatusRowID, row.id)
	}
	text := row.text
	for _, want := range []string{"Diagnostics", "checks need attention", "Actions"} {
		if !strings.Contains(text, want) {
			t.Fatalf("doctor row missing %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"Generated", "Checks", "[pass]"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("doctor row should hide %q:\n%s", unwanted, text)
		}
	}
}

func TestDoctorConnectivityCommandAnimatesAndReplacesStatusRow(t *testing.T) {
	profile := config.ProviderProfile{
		Name:         "custom",
		ProviderKind: config.ProviderKindOpenAICompatible,
		BaseURL:      "https://api.example.com/v1",
		Model:        "custom-model",
	}
	m := newModel(context.Background(), Options{
		ProviderProfile: profile,
		ProbeProviderHealth: func(context.Context, providerhealth.Options) providerhealth.Result {
			return providerhealth.Result{
				Status: providerhealth.StatusPass,
				Checks: []providerhealth.Check{{
					ID:      "provider.connectivity",
					Status:  providerhealth.StatusPass,
					Message: "reachable",
				}},
			}
		},
	})
	// This test exercises the tick-driven status animation, which is gated off
	// under reduced motion. CI runs with no TTY (reducedMotion auto-on), so force
	// it off to test the animation mechanism deterministically.
	m.reducedMotion = false
	m.input.SetValue("/doctor --connectivity")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd == nil {
		t.Fatal("expected /doctor --connectivity to start an async command")
	}
	beforeRows := len(next.transcript)
	before := newestDoctorStatusText(next.transcript)

	updated, _ = next.Update(next.spinner.Tick())
	ticked := updated.(model)
	after := newestDoctorStatusText(ticked.transcript)
	if before == "" || after == "" {
		t.Fatalf("expected doctor status row before=%q after=%q", before, after)
	}
	if before == after {
		t.Fatalf("expected doctor status to animate on tick, still %q", after)
	}

	updated, _ = ticked.Update(execCmd(cmd))
	final := updated.(model)
	if len(final.transcript) != beforeRows {
		t.Fatalf("expected final doctor report to replace the running row, rows before=%d after=%d: %#v", beforeRows, len(final.transcript), final.transcript)
	}
	if got := countDoctorDiagnosticRows(final.transcript); got != 1 {
		t.Fatalf("expected one doctor diagnostic row after completion, got %d: %#v", got, final.transcript)
	}
	if transcriptContains(final.transcript, "checking provider connectivity") {
		t.Fatalf("expected stale running copy to be replaced, got %#v", final.transcript)
	}
	if !transcriptContains(final.transcript, "[pass] provider.connectivity") {
		t.Fatalf("expected completed diagnostics result, got %#v", final.transcript)
	}
}

func TestDoctorFixOpensProviderWizardWhenProviderMissing(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.input.SetValue("/doctor fix")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd != nil {
		t.Fatal("expected /doctor fix provider setup path to be handled synchronously")
	}
	if next.providerWizard == nil {
		t.Fatalf("expected /doctor fix to open provider setup wizard, transcript %#v", next.transcript)
	}
	if !transcriptContains(next.transcript, "Opening provider setup") {
		t.Fatalf("expected doctor fix transcript to explain provider setup, got %#v", next.transcript)
	}
}

func TestDoctorFixLinesShowNoAutomaticFixForUnmappedFailure(t *testing.T) {
	lines := doctorFixLines(doctor.Report{Checks: []doctor.Check{{
		ID:      "provider.auth",
		Status:  doctor.StatusFail,
		Message: "API key rejected.",
	}}})

	text := strings.Join(lines, "\n")
	if strings.Contains(text, "already clean") {
		t.Fatalf("doctorFixLines reported clean diagnostics for an unmapped failure:\n%s", text)
	}
	if !strings.Contains(text, "No automatic fixes are available") {
		t.Fatalf("doctorFixLines = %q, want no automatic fix guidance", text)
	}
}

func TestDoctorFixLinesUseSandboxRemedy(t *testing.T) {
	lines := doctorFixLines(doctor.Report{Checks: []doctor.Check{{
		ID:      "sandbox.backend",
		Status:  doctor.StatusWarn,
		Message: "Native sandbox backend unavailable on windows: Windows sandbox setup helper is not available.",
		Details: map[string]any{
			"remedy": "install the Windows sandbox command runner and setup helper together, then run `zero sandbox setup`",
		},
	}}})

	text := strings.Join(lines, "\n")
	if !strings.Contains(text, "zero sandbox setup") {
		t.Fatalf("doctorFixLines missing sandbox setup remedy:\n%s", text)
	}
	if strings.Contains(text, "WSL2") || strings.Contains(text, "Linux container") {
		t.Fatalf("doctorFixLines used stale Windows sandbox guidance:\n%s", text)
	}
}

func newestDoctorStatusText(rows []transcriptRow) string {
	if row := newestDoctorStatusRow(rows); row != nil {
		return row.text
	}
	return ""
}

func newestDoctorStatusRow(rows []transcriptRow) *transcriptRow {
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		if row.kind == rowSystem && row.id == doctorStatusRowID {
			return &rows[i]
		}
	}
	return nil
}

func countDoctorDiagnosticRows(rows []transcriptRow) int {
	count := 0
	for _, row := range rows {
		if row.kind == rowSystem && row.id == doctorStatusRowID {
			count++
		}
	}
	return count
}

func TestDoctorFixRunsConnectivityWhenProviderConfigured(t *testing.T) {
	profile := config.ProviderProfile{
		Name:         "custom",
		ProviderKind: config.ProviderKindOpenAICompatible,
		BaseURL:      "https://api.example.com/v1",
		Model:        "custom-model",
		APIKey:       "sk-test", // credentialed so provider.config passes and /doctor fix reaches connectivity
	}
	called := false
	m := newModel(context.Background(), Options{
		ProviderProfile: profile,
		ProbeProviderHealth: func(context.Context, providerhealth.Options) providerhealth.Result {
			called = true
			return providerhealth.Result{
				Status: providerhealth.StatusPass,
				Checks: []providerhealth.Check{{
					ID:      "provider.connectivity",
					Status:  providerhealth.StatusPass,
					Message: "reachable",
				}},
			}
		},
	})
	m.input.SetValue("/doctor fix")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd == nil {
		t.Fatal("expected /doctor fix to run provider connectivity asynchronously")
	}
	if called {
		t.Fatal("provider probe ran synchronously before the returned command executed")
	}
	if !transcriptContains(next.transcript, "checking provider connectivity") {
		t.Fatalf("expected running doctor status, got %#v", next.transcript)
	}

	updated, _ = next.Update(execCmd(cmd))
	final := updated.(model)
	if !called {
		t.Fatal("provider probe did not run when /doctor fix command executed")
	}
	if !transcriptContains(final.transcript, "[pass] provider.connectivity") {
		t.Fatalf("expected connectivity result in transcript, got %#v", final.transcript)
	}
}
