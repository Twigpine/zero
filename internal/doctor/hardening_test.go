package doctor

import (
	"errors"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/sandbox"
)

// validProvider is a fully-formed provider profile so the report's overall OK
// status reflects only the check under test (not an unrelated provider failure).
func validProvider() config.ProviderProfile {
	return config.ProviderProfile{
		Name:         "openai",
		ProviderKind: config.ProviderKindOpenAI,
		BaseURL:      config.OpenAIBaseURL,
		APIKey:       "sk-test",
		Model:        "gpt-4.1",
	}
}

// stubLookup returns a lookup function that resolves only the named binaries.
func stubLookup(present ...string) func(string) (string, error) {
	set := map[string]bool{}
	for _, name := range present {
		set[name] = true
	}
	return func(name string) (string, error) {
		if set[name] {
			return "/usr/bin/" + name, nil
		}
		return "", errors.New("executable file not found in $PATH")
	}
}

func TestSandboxCheckPassesWhenBackendPresent(t *testing.T) {
	report := Run(Options{
		Now:              fixedDoctorClock("2026-06-12T10:00:00Z"),
		Runtime:          "go",
		GOOS:             "linux",
		LookupExecutable: stubLookup(sandbox.LinuxSandboxHelperName, "bwrap"),
	})

	check := report.Check("sandbox.backend")
	if check == nil || check.Status != StatusPass {
		t.Fatalf("expected sandbox.backend pass, got %#v", report.Checks)
	}
	if !strings.Contains(check.Message, string(sandbox.BackendLinuxBwrap)) {
		t.Fatalf("expected Linux sandbox backend named in message, got %q", check.Message)
	}
}

func TestSandboxCheckWarnsWithRemedyWhenBackendMissing(t *testing.T) {
	report := Run(Options{
		Now:              fixedDoctorClock("2026-06-12T10:05:00Z"),
		Runtime:          "go",
		GOOS:             "linux",
		Provider:         validProvider(),
		LookupExecutable: stubLookup("gopls", "typescript-language-server", "pyright-langserver", "rust-analyzer"),
	})

	check := report.Check("sandbox.backend")
	if check == nil || check.Status != StatusWarn {
		t.Fatalf("expected sandbox.backend warn, got %#v", report.Checks)
	}
	remedy, _ := check.Details["remedy"].(string)
	if !strings.Contains(remedy, "bwrap") && !strings.Contains(remedy, "bubblewrap") {
		t.Fatalf("expected actionable bubblewrap remedy, got %q (details %#v)", remedy, check.Details)
	}
	// A missing native sandbox is a degradation, not a hard failure: non-shell
	// permission checks still run, so the overall report must not fail on it alone.
	if !report.OK {
		t.Fatalf("missing sandbox backend should warn, not fail the report: %#v", report.Checks)
	}
}

func TestSandboxCheckRemedyIsPlatformSpecific(t *testing.T) {
	darwin := Run(Options{
		Now:              fixedDoctorClock("2026-06-12T10:06:00Z"),
		Runtime:          "go",
		GOOS:             "darwin",
		LookupExecutable: stubLookup(),
	}).Check("sandbox.backend")
	if darwin == nil || darwin.Status != StatusWarn {
		t.Fatalf("expected darwin sandbox warn, got %#v", darwin)
	}
	remedy, _ := darwin.Details["remedy"].(string)
	if !strings.Contains(remedy, "sandbox-exec") {
		t.Fatalf("darwin remedy should mention sandbox-exec, got %q", remedy)
	}

	macPresent := Run(Options{
		Now:              fixedDoctorClock("2026-06-12T10:07:00Z"),
		Runtime:          "go",
		GOOS:             "darwin",
		LookupExecutable: stubLookup("sandbox-exec"),
	}).Check("sandbox.backend")
	if macPresent == nil || macPresent.Status != StatusPass {
		t.Fatalf("expected darwin sandbox pass when sandbox-exec present, got %#v", macPresent)
	}
}

func TestSandboxCheckReportsWindowsNativeSetupStates(t *testing.T) {
	native := Run(Options{
		Now:     fixedDoctorClock("2026-06-12T10:08:00Z"),
		Runtime: "go",
		GOOS:    "windows",
		LookupExecutable: stubLookup(
			sandbox.WindowsSandboxCommandRunnerName,
			sandbox.WindowsSandboxSetupName,
		),
	}).Check("sandbox.backend")
	if native == nil || native.Status != StatusPass {
		t.Fatalf("expected windows native sandbox pass when helpers are present, got %#v", native)
	}
	if !strings.Contains(native.Message, string(sandbox.BackendWindowsRestrictedToken)) {
		t.Fatalf("expected native Windows backend in message, got %q", native.Message)
	}

	t.Setenv("ZERO_WINDOWS_SANDBOX_HOME", t.TempDir())
	needsSetup := Run(Options{
		Now:           fixedDoctorClock("2026-06-12T10:08:30Z"),
		Runtime:       "go",
		GOOS:          "windows",
		WorkspaceRoot: t.TempDir(),
		LookupExecutable: stubLookup(
			sandbox.WindowsSandboxCommandRunnerName,
			sandbox.WindowsSandboxSetupName,
		),
	}).Check("sandbox.backend")
	if needsSetup == nil || needsSetup.Status != StatusWarn {
		t.Fatalf("expected setup-missing warning when marker is absent, got %#v", needsSetup)
	}
	setupStatus, _ := needsSetup.Details["setupStatus"].(string)
	setupRemedy, _ := needsSetup.Details["remedy"].(string)
	if setupStatus != "missing-or-out-of-date" || !strings.Contains(setupRemedy, "zero sandbox setup") {
		t.Fatalf("setup warning details = %#v, want missing/out-of-date with setup remedy", needsSetup.Details)
	}

	// With self-dispatch, the standalone helper .exe files no longer need to be
	// on PATH for the Windows backend to be usable — the running zero binary acts
	// as its own command-runner/setup helper. So a missing PATH helper no longer
	// reports "unavailable"; the backend is available and (with no workspace to
	// require a setup marker) the check passes. The actionable "run `zero sandbox
	// setup`" guidance is covered by the marker-missing case above.
	noPathHelpers := Run(Options{
		Now:              fixedDoctorClock("2026-06-12T10:09:00Z"),
		Runtime:          "go",
		GOOS:             "windows",
		LookupExecutable: stubLookup(),
	}).Check("sandbox.backend")
	if noPathHelpers == nil || noPathHelpers.Status != StatusPass {
		t.Fatalf("expected available windows backend via self-dispatch when no PATH helpers, got %#v", noPathHelpers)
	}
	if !strings.Contains(noPathHelpers.Message, string(sandbox.BackendWindowsRestrictedToken)) {
		t.Fatalf("expected native Windows backend in message, got %q", noPathHelpers.Message)
	}
}

func TestLSPCheckWarnsForMissingServersWithRemedy(t *testing.T) {
	report := Run(Options{
		Now:              fixedDoctorClock("2026-06-12T10:10:00Z"),
		Runtime:          "go",
		GOOS:             "linux",
		Provider:         validProvider(),
		LookupExecutable: stubLookup("gopls", "bwrap"), // gopls present, other servers missing
	})

	check := report.Check("lsp.servers")
	if check == nil || check.Status != StatusWarn {
		t.Fatalf("expected lsp.servers warn, got %#v", report.Checks)
	}
	// gopls present -> not in the missing list; a missing one (e.g. the TS
	// server) must come with an actionable install command.
	missing, ok := check.Details["missing"].(map[string]any)
	if !ok || len(missing) == 0 {
		t.Fatalf("expected a non-empty missing-server map, got %#v", check.Details)
	}
	if _, present := missing["gopls"]; present {
		t.Fatalf("gopls is on PATH and must not be listed as missing: %#v", missing)
	}
	tsRemedy, _ := missing["typescript-language-server"].(string)
	if !strings.Contains(tsRemedy, "install") && !strings.Contains(tsRemedy, "npm") {
		t.Fatalf("expected an actionable install remedy for the TS server, got %q", tsRemedy)
	}
	if !report.OK {
		t.Fatalf("missing optional LSP servers should warn, not fail: %#v", report.Checks)
	}
}

func TestLSPCheckPassesWhenAllServersPresent(t *testing.T) {
	report := Run(Options{
		Now:              fixedDoctorClock("2026-06-12T10:15:00Z"),
		Runtime:          "go",
		LookupExecutable: stubLookup("gopls", "typescript-language-server", "pyright-langserver", "rust-analyzer"),
	})

	check := report.Check("lsp.servers")
	if check == nil || check.Status != StatusPass {
		t.Fatalf("expected lsp.servers pass, got %#v", report.Checks)
	}
}

func TestHardeningChecksSkipWhenNoLookupProvided(t *testing.T) {
	// Without an injected lookup the checks fall back to the real PATH. That is
	// environment-dependent, so we only assert the checks are present and never
	// fail the report (they degrade to warn/pass, never fail).
	report := Run(Options{Now: fixedDoctorClock("2026-06-12T10:20:00Z"), Runtime: "go"})
	for _, id := range []string{"sandbox.backend", "lsp.servers"} {
		check := report.Check(id)
		if check == nil {
			t.Fatalf("expected %s check to be present, got %#v", id, report.Checks)
		}
		if check.Status == StatusFail {
			t.Fatalf("%s must never hard-fail the report, got %#v", id, check)
		}
	}
}
