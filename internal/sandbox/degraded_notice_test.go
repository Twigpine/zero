package sandbox

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// Only DEGRADED says anything. A disabled sandbox is the user's own choice, and
// the unelevated Windows tier still enforces the write jail and reports what it
// lacks through `zero sandbox policy`; a notice on every session for either
// would stop meaning anything.
func TestDegradedNoticeSpeaksOnlyForADegradedPlan(t *testing.T) {
	for _, level := range []EnforcementLevel{EnforcementNative, EnforcementUnelevated, EnforcementDisabled, ""} {
		plan := BackendPlan{EnforcementLevel: level, DowngradeReason: "some reason"}
		if notice := plan.DegradedNotice(); notice != "" {
			t.Errorf("level %q produced a notice: %q", level, notice)
		}
	}

	plan := BackendPlan{EnforcementLevel: EnforcementDegraded, DowngradeReason: "Linux sandbox helper is not available"}
	want := "Sandbox enforcement is degraded: Linux sandbox helper is not available. See `zero sandbox policy --effective`."
	if got := plan.DegradedNotice(); got != want {
		t.Errorf("DegradedNotice() = %q, want %q", got, want)
	}
}

// A reason that already ends a sentence keeps one period, and a missing reason
// still says what is wrong.
func TestDegradedNoticeWordsItsReasonOnce(t *testing.T) {
	withPeriod := BackendPlan{EnforcementLevel: EnforcementDegraded, DowngradeReason: "Run `zero sandbox setup` first."}
	if got := withPeriod.DegradedNotice(); strings.Contains(got, "..") {
		t.Errorf("the reason's own period was doubled: %q", got)
	}
	empty := BackendPlan{EnforcementLevel: EnforcementDegraded}
	if got := empty.DegradedNotice(); !strings.Contains(got, "no native sandbox is available") {
		t.Errorf("a degraded plan with no reason gave no reason: %q", got)
	}
}

// THE SESSION'S NOTICE AND `zero sandbox policy` DESCRIBE ONE SANDBOX. The engine
// builds its notice from its own backend and policy through BuildPlan, the
// function the policy command prints from, rather than deciding "degraded" a
// second way.
func TestEngineDegradedNoticeComesFromThePolicyCommandsPlan(t *testing.T) {
	clearNestingMarkers(t)
	root := t.TempDir()
	unavailable := Backend{Name: BackendUnavailable, Platform: "linux", Fallback: true, Message: "Linux sandbox helper is not available"}

	engine := NewEngine(EngineOptions{WorkspaceRoot: root, Policy: DefaultPolicy(), Backend: unavailable})
	want := unavailable.BuildPlan(root, DefaultPolicy()).DegradedNotice()
	if want == "" {
		t.Fatal("SETUP INVALID: an unavailable backend under the default policy is not degraded")
	}
	if got := engine.DegradedNotice(); got != want {
		t.Errorf("engine notice = %q, want the policy command's %q", got, want)
	}

	disabled := DefaultPolicy()
	disabled.Mode = ModeDisabled
	off := NewEngine(EngineOptions{WorkspaceRoot: root, Policy: disabled, Backend: unavailable})
	if got := off.DegradedNotice(); got != "" {
		t.Errorf("a sandbox the user turned off still produced a notice: %q", got)
	}

	var none *Engine
	if got := none.DegradedNotice(); got != "" {
		t.Errorf("a nil engine produced a notice: %q", got)
	}
}

// clearNestingMarkers keeps a test from inheriting the markers when the suite
// itself runs inside a Zero sandbox.
func clearNestingMarkers(t *testing.T) {
	t.Helper()
	t.Setenv(EnvSandboxed, "")
	t.Setenv(EnvSandboxBackend, "")
}

// THE NESTING MARKERS SKIP WRAPPING, SO THEY HAVE TO BE SAID.
//
// With ZERO_SANDBOXED and ZERO_SANDBOX_BACKEND set, the runner passes every
// command through unwrapped on the word of an outer sandbox nothing verifies
// (#727). A native backend's plan knows nothing of that and reports full
// enforcement, so the session used to run every command unwrapped in silence.
func TestEngineDegradedNoticeSpeaksWhenTheNestingMarkersSkipWrapping(t *testing.T) {
	root := t.TempDir()
	native := Backend{Name: BackendLinuxBwrap, Platform: "linux", Available: true, NativeIsolation: true, CommandWrapping: true, Executable: "/usr/bin/bwrap"}

	clearNestingMarkers(t)
	if got := NewEngine(EngineOptions{WorkspaceRoot: root, Policy: DefaultPolicy(), Backend: native}).DegradedNotice(); got != "" {
		t.Fatalf("SETUP INVALID: a native backend without the markers already gives a notice: %q", got)
	}

	t.Setenv(EnvSandboxed, "1")
	t.Setenv(EnvSandboxBackend, string(BackendLinuxBwrap))
	got := NewEngine(EngineOptions{WorkspaceRoot: root, Policy: DefaultPolicy(), Backend: native}).DegradedNotice()
	if !strings.Contains(got, EnvSandboxed) || !strings.Contains(got, "cannot verify") {
		t.Errorf("with the nesting markers set the notice = %q, want one naming the markers and the unverified outer sandbox", got)
	}

	disabled := DefaultPolicy()
	disabled.Mode = ModeDisabled
	if got := NewEngine(EngineOptions{WorkspaceRoot: root, Policy: disabled, Backend: native}).DegradedNotice(); got != "" {
		t.Errorf("a sandbox the user turned off still produced a notice under the markers: %q", got)
	}
}

// NO GRANT REACHES A DEGRADED PLAN THE SESSION'S NOTICE MISSED.
//
// The notice is decided once, from the policy the session starts with, and
// grants made during the session change the policy each command runs under.
// They cannot open a gap: a grant only opens the network or adds allow and
// deny paths, never the mode, and every mode but disabled builds a restricted
// filesystem profile that needs the platform sandbox from the start. So a
// degraded command plan means the notice already spoke, and a disabled sandbox
// stays disabled whatever is granted. A deny path the backend cannot enforce is
// refused at the command rather than run degraded. Raised by CodeRabbit.
func TestNoGrantReachesADegradedPlanTheSessionNoticeMissed(t *testing.T) {
	clearNestingMarkers(t)
	cacheRoot, tempRoot := t.TempDir(), t.TempDir()
	original := sandboxUserCacheDir
	sandboxUserCacheDir = func() (string, error) { return cacheRoot, nil }
	t.Cleanup(func() { sandboxUserCacheDir = original })
	t.Setenv("TMPDIR", tempRoot)
	t.Setenv("TMP", tempRoot)
	t.Setenv("TEMP", tempRoot)

	root := t.TempDir()
	extra := t.TempDir()
	secret := filepath.Join(root, "secret.txt")
	enabled := true
	grants := []struct {
		name    string
		profile RequestPermissionProfile
	}{
		{"network", RequestPermissionProfile{Network: &NetworkPermissions{Enabled: &enabled}}},
		{"write root", RequestPermissionProfile{FileSystem: &FileSystemPermissions{Write: []string{extra}}}},
		{"deny read", RequestPermissionProfile{FileSystem: &FileSystemPermissions{DenyRead: []string{secret}}}},
	}
	unavailable := Backend{Name: BackendUnavailable, Platform: "linux", Fallback: true, Message: "Linux sandbox helper is not available"}
	degradedAndSaid, refused := 0, 0
	for _, mode := range []PolicyMode{ModeEnforce, ModeDisabled} {
		for _, network := range []NetworkMode{NetworkAllow, NetworkDeny} {
			for _, grant := range grants {
				for _, scope := range []PermissionGrantScope{PermissionGrantScopeSession, PermissionGrantScopeTurn} {
					name := fmt.Sprintf("%s/%s/%s/%s", mode, network, grant.name, scope)
					policy := DefaultPolicy()
					policy.Mode, policy.Network = mode, network
					engine := NewEngine(EngineOptions{WorkspaceRoot: root, Policy: policy, Backend: unavailable})
					notice := engine.DegradedNotice()
					undo, err := engine.GrantRequestPermissions(grant.profile, scope)
					if err != nil {
						t.Fatalf("%s: SETUP INVALID: grant: %v", name, err)
					}
					plan, err := engine.BuildCommandPlan(CommandSpec{Name: "/bin/sh", Args: []string{"-c", "true"}, Dir: root})
					undo()
					// The plan holds the runtime lease until it is cleaned up, and a lease
					// file still open fails the temp directory removal on Windows.
					plan.Cleanup()
					if err != nil {
						if grant.name != "deny read" || mode == ModeDisabled {
							t.Errorf("%s: refused a command it should have planned: %v", name, err)
						}
						refused++
						continue
					}
					if plan.EnforcementLevel == EnforcementDegraded {
						if notice == "" {
							t.Errorf("%s: the command plan is degraded but the session started with no notice", name)
						}
						degradedAndSaid++
					}
					if mode == ModeDisabled && plan.EnforcementLevel != EnforcementDisabled {
						t.Errorf("%s: a grant took a disabled sandbox to %q", name, plan.EnforcementLevel)
					}
				}
			}
		}
	}
	// Not vacuous: the enforced modes did reach degraded plans, and the deny
	// grants were refused rather than degraded.
	if degradedAndSaid == 0 || refused == 0 {
		t.Fatalf("SETUP INVALID: %d degraded plans and %d refusals, so the invariant was never exercised", degradedAndSaid, refused)
	}
}
