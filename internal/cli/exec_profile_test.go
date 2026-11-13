package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/execprofile"
)

func TestApplyExecProfileFillsOnlyUnset(t *testing.T) {
	// Unset effort: the profile fills it and reports the fill, so the
	// escalation policy knows the displaced effort was the provider default.
	options := execOptions{execProfile: "fast"}
	profile, effortFilled, err := applyExecProfile(&options)
	if err != nil {
		t.Fatalf("applyExecProfile: %v", err)
	}
	if profile.Name != "fast" {
		t.Fatalf("profile.Name = %q, want fast", profile.Name)
	}
	if options.reasoningEffort != "low" {
		t.Fatalf("reasoningEffort = %q, want the profile's low", options.reasoningEffort)
	}
	if !effortFilled {
		t.Fatal("the profile filled the effort, so effortFilled must report it")
	}

	// Set effort (explicit flag or a mode's fill): the profile backs off.
	options = execOptions{execProfile: "fast", reasoningEffort: "high"}
	_, effortFilled, err = applyExecProfile(&options)
	if err != nil {
		t.Fatalf("applyExecProfile: %v", err)
	}
	if options.reasoningEffort != "high" {
		t.Fatalf("reasoningEffort = %q, an explicit value must win over the profile", options.reasoningEffort)
	}
	if effortFilled {
		t.Fatal("the profile backed off, so effortFilled must be false")
	}
}

func TestApplyExecProfileThoroughArmsSelfCorrect(t *testing.T) {
	options := execOptions{execProfile: "thorough"}
	if _, _, err := applyExecProfile(&options); err != nil {
		t.Fatalf("applyExecProfile: %v", err)
	}
	if !options.selfCorrect {
		t.Fatal("thorough must arm the post-edit self-correct loop")
	}
	if options.reasoningEffort != "high" {
		t.Fatalf("reasoningEffort = %q, want high", options.reasoningEffort)
	}
}

// The spec-draft path wires no self-corrector (explicit --self-correct
// --use-spec is rejected at parse time, before profiles apply), so a profile's
// self-correct knob is projected away under --use-spec instead of silently
// arming a knob the draft runner ignores. The other knobs still apply.
func TestApplyExecProfileSpecProjectsAwaySelfCorrect(t *testing.T) {
	options := execOptions{execProfile: "thorough", useSpec: true}
	if _, _, err := applyExecProfile(&options); err != nil {
		t.Fatalf("applyExecProfile: %v", err)
	}
	if options.selfCorrect {
		t.Fatal("thorough under --use-spec must not arm self-correct (the draft runner has none)")
	}
	if options.reasoningEffort != "high" {
		t.Fatalf("reasoningEffort = %q, the profile's other knobs must still apply", options.reasoningEffort)
	}
}

func TestApplyExecProfileUnknownIsUsageError(t *testing.T) {
	options := execOptions{execProfile: "turbo"}
	_, _, err := applyExecProfile(&options)
	if err == nil {
		t.Fatal("expected a usage error for an unknown profile")
	}
	if _, ok := err.(execUsageError); !ok {
		t.Fatalf("expected execUsageError, got %T: %v", err, err)
	}
	for _, name := range execprofile.Names() {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("usage error must list %q, got %q", name, err.Error())
		}
	}
}

// Precedence: explicit flag > --mode > --exec-profile. The deep mode fills
// effort=high and max-turns=160; the fast profile must not override either,
// because a mode-filled field counts as set by the time the profile runs.
func TestExecProfilePrecedenceFlagOverModeOverProfile(t *testing.T) {
	options := execOptions{mode: "deep", execProfile: "fast"}
	if err := applyExecMode(&options); err != nil {
		t.Fatalf("applyExecMode: %v", err)
	}
	profile, _, err := applyExecProfile(&options)
	if err != nil {
		t.Fatalf("applyExecProfile: %v", err)
	}
	if options.reasoningEffort != "high" {
		t.Fatalf("reasoningEffort = %q, the mode's high must win over the profile's low", options.reasoningEffort)
	}
	// The mode filled options.maxTurns, so the turn budget must not be displaced.
	effective, displaced := applyProfileTurnBudget(profile, options.maxTurns, options.maxTurns)
	if effective != 160 || displaced != 0 {
		t.Fatalf("turn budget = (%d, displaced %d), the mode's 160 must win with nothing displaced", effective, displaced)
	}
}

func TestApplyProfileTurnBudget(t *testing.T) {
	fast, _ := execprofile.Lookup("fast")
	balanced, _ := execprofile.Lookup("balanced")

	// No explicit flag: the profile displaces the resolved budget.
	if effective, displaced := applyProfileTurnBudget(fast, 0, 80); effective != 30 || displaced != 80 {
		t.Fatalf("fast over resolved 80 = (%d, %d), want (30, 80)", effective, displaced)
	}
	// Explicit --max-turns pins the budget; the profile backs off entirely.
	if effective, displaced := applyProfileTurnBudget(fast, 50, 50); effective != 50 || displaced != 0 {
		t.Fatalf("fast with explicit 50 = (%d, %d), want (50, 0)", effective, displaced)
	}
	// Balanced never displaces anything.
	if effective, displaced := applyProfileTurnBudget(balanced, 0, 80); effective != 80 || displaced != 0 {
		t.Fatalf("balanced over resolved 80 = (%d, %d), want (80, 0)", effective, displaced)
	}
}

// The no-regression invariant at the options level: selecting balanced leaves
// the options byte-identical to not selecting a profile at all, and produces
// no loop policy.
func TestExecProfileBalancedLeavesOptionsUntouched(t *testing.T) {
	options := execOptions{execProfile: "balanced"}
	reference := options
	profile, _, err := applyExecProfile(&options)
	if err != nil {
		t.Fatalf("applyExecProfile: %v", err)
	}
	if !reflect.DeepEqual(options, reference) {
		t.Fatalf("balanced changed the options: %+v vs %+v", options, reference)
	}
	if policy := profile.Policy(80, false); policy != nil {
		t.Fatalf("balanced must produce a nil policy, got %+v", policy)
	}
}

// --exec-profile= with an inline empty value must error like its two-token
// form and the sibling --mode=, not silently run the default posture (the
// classic shell hazard: an unset variable expanding into the inline form).
func TestParseExecProfileInlineEmptyIsError(t *testing.T) {
	_, _, err := parseExecArgs([]string{"--exec-profile=", "hello"})
	if err == nil || !strings.Contains(err.Error(), "--exec-profile requires a value") {
		t.Fatalf("expected a required-value error, got %v", err)
	}
}

// Full echo-provider run: the selected profile must be stamped into the
// per-turn trace so benchmark attribution can group runs by posture.
func TestRunExecTraceRecordsSelectedProfile(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "trace.ndjson")
	exitCode, _, stderr := runExecWithEcho(t, []string{
		"exec", "--exec-profile", "fast", "--trace", tracePath, "hello",
	})
	if exitCode != exitSuccess {
		t.Fatalf("expected exit %d, got %d: %s", exitSuccess, exitCode, stderr)
	}
	raw, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	if !strings.Contains(string(raw), `"profile":"fast"`) {
		t.Fatalf("trace must record the selected profile, got %s", raw)
	}
}

// An explicit --spec-reasoning-effort governs the spec draft's effort, so the
// escalation effort-restore must not arm there even when the profile filled
// the (unused) main effort.
func TestSpecProfileEffortFilled(t *testing.T) {
	if !specProfileEffortFilled(true, "") {
		t.Fatal("no explicit spec effort: the profile's fill governs, restore must arm")
	}
	if specProfileEffortFilled(true, "high") {
		t.Fatal("an explicit spec effort must disarm the effort restore")
	}
	if specProfileEffortFilled(false, "") {
		t.Fatal("nothing filled, nothing to restore")
	}
}

// The trace label deliberately records the SELECTED profile, balanced
// included, so captures are self-describing. Run behavior stays identical
// (balanced fills nothing and arms no policy); the label is the one intended
// difference in the opt-in trace artifact.
func TestRunExecTraceRecordsBalancedSelection(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "trace.ndjson")
	exitCode, _, stderr := runExecWithEcho(t, []string{
		"exec", "--exec-profile", "balanced", "--trace", tracePath, "hello",
	})
	if exitCode != exitSuccess {
		t.Fatalf("expected exit %d, got %d: %s", exitSuccess, exitCode, stderr)
	}
	raw, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	if !strings.Contains(string(raw), `"profile":"balanced"`) {
		t.Fatalf("trace must record the balanced selection, got %s", raw)
	}
}

func TestRunExecUnknownExecProfileIsUsageError(t *testing.T) {
	exitCode, _, stderr := runExecWithEcho(t, []string{
		"exec", "--exec-profile", "turbo", "hello",
	})
	if exitCode != exitUsage {
		t.Fatalf("expected usage exit %d, got %d", exitUsage, exitCode)
	}
	if !strings.Contains(stderr, "unknown execution profile") {
		t.Fatalf("expected an unknown-profile usage error, got %q", stderr)
	}
}
