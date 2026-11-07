package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The tier table is the ceiling. Asserted as a table so a change to any number
// is a deliberate edit to this test rather than a silent widening.
func TestPlanSizeTierCeilings(t *testing.T) {
	for _, tc := range []struct {
		size PlanSize
		want int
	}{
		{PlanSizeSmall, 5},
		{PlanSizeMedium, 20},
		{PlanSizeLarge, 50},
		{PlanSizeUnrestricted, 0},
	} {
		if got := tc.size.MaxTasks(); got != tc.want {
			t.Errorf("%s.MaxTasks() = %d; want %d", tc.size, got, tc.want)
		}
	}
}

// THE NO-REGRESSION ASSERTION. The ceiling was a hard-coded 20 before it was
// configurable, and the default must still be that number: making a bound
// configurable and moving it in the same change would hide a behaviour change
// inside a mechanism change.
func TestTheDefaultTierIsTheCeilingThatWasHardCoded(t *testing.T) {
	if got := DefaultPlanSize.MaxTasks(); got != 20 {
		t.Fatalf("default tier ceiling = %d; want 20, the value defaultPlanMaxTasks held", got)
	}
	// The zero value of the type must resolve there too — a caller that never
	// wires the tier gets the old ceiling, not no ceiling.
	var unset PlanSize
	if got := unset.MaxTasks(); got != 20 {
		t.Fatalf("unset tier ceiling = %d; want 20", got)
	}
}

// FAIL CLOSED. A typo in a config file must never be the thing that removes the
// bound — the failure mode that matters is "planSize": "unlimted" silently
// granting an unbounded plan.
func TestAnUnknownTierResolvesToTheDefaultAndNeverToUnbounded(t *testing.T) {
	unknown := PlanSize("unlimted")
	if unknown.Valid() {
		t.Fatal("a misspelled tier must not be Valid")
	}
	if got := unknown.MaxTasks(); got != DefaultPlanSize.MaxTasks() {
		t.Fatalf("unknown tier ceiling = %d; want the default %d", got, DefaultPlanSize.MaxTasks())
	}
	if unknown.MaxTasks() == 0 {
		t.Fatal("an unknown tier resolved to NO ceiling; a typo must not remove the bound")
	}
	if _, err := ParsePlanSize("unlimted"); err == nil {
		t.Fatal("ParsePlanSize must report an unknown tier so a caller can surface it")
	}
}

// An unset value is not an error: the overwhelmingly common config has no key
// at all, and that must not make the file unloadable.
func TestAnUnsetTierIsTheDefaultAndNotAnError(t *testing.T) {
	size, err := ParsePlanSize("   ")
	if err != nil {
		t.Fatalf("ParsePlanSize(empty): %v", err)
	}
	if size != DefaultPlanSize {
		t.Fatalf("ParsePlanSize(empty) = %q; want %q", size, DefaultPlanSize)
	}
}

func TestParsePlanSizeIgnoresCaseAndSpace(t *testing.T) {
	size, err := ParsePlanSize("  LARGE ")
	if err != nil {
		t.Fatalf("ParsePlanSize: %v", err)
	}
	if size != PlanSizeLarge {
		t.Fatalf("got %q; want %q", size, PlanSizeLarge)
	}
}

// User config is higher-trust and may LOOSEN the tier.
func TestUserConfigMaySetAnyTier(t *testing.T) {
	dst := FileConfig{}
	mergeConfig(&dst, FileConfig{Profiles: ProfilesConfig{PlanSize: "unrestricted"}})
	if got := dst.Profiles.PlanSizeTier(); got != PlanSizeUnrestricted {
		t.Fatalf("tier = %q; want %q — user config may loosen", got, PlanSizeUnrestricted)
	}
}

// Project config may TIGHTEN. Same privilege boundary as Sandbox.Network's
// tighten-only rule: a project may make its own runs stricter.
func TestProjectConfigMayTightenTheTier(t *testing.T) {
	dst := FileConfig{Profiles: ProfilesConfig{PlanSize: "large"}}
	if err := mergeProjectConfig(&dst, FileConfig{Profiles: ProfilesConfig{PlanSize: "small"}}); err != nil {
		t.Fatalf("mergeProjectConfig: %v", err)
	}
	if got := dst.Profiles.PlanSizeTier(); got != PlanSizeSmall {
		t.Fatalf("tier = %q; want %q — a project may tighten", got, PlanSizeSmall)
	}
}

// ...and may NOT loosen it. A cloned repo must not be able to raise a spend
// ceiling for whoever opens it. Dropped silently, like an ignored network
// "allow": the project scope simply does not hold that privilege.
func TestProjectConfigCannotLoosenTheTier(t *testing.T) {
	for _, attempt := range []string{"large", "unrestricted"} {
		dst := FileConfig{Profiles: ProfilesConfig{PlanSize: "small"}}
		if err := mergeProjectConfig(&dst, FileConfig{Profiles: ProfilesConfig{PlanSize: attempt}}); err != nil {
			t.Fatalf("mergeProjectConfig(%s): %v", attempt, err)
		}
		if got := dst.Profiles.PlanSizeTier(); got != PlanSizeSmall {
			t.Fatalf("project config raised the tier to %q with %q; it must stay %q", got, attempt, PlanSizeSmall)
		}
	}
}

// The unset case is the one that matters most: with no user setting the
// effective tier is medium, and a project asking for large must still lose.
func TestProjectConfigCannotLoosenTheDefaultTier(t *testing.T) {
	dst := FileConfig{}
	if err := mergeProjectConfig(&dst, FileConfig{Profiles: ProfilesConfig{PlanSize: "unrestricted"}}); err != nil {
		t.Fatalf("mergeProjectConfig: %v", err)
	}
	if got := dst.Profiles.PlanSizeTier(); got != DefaultPlanSize {
		t.Fatalf("tier = %q; want the default %q — a project may not loosen an unset tier", got, DefaultPlanSize)
	}
	if dst.Profiles.PlanSizeTier().MaxTasks() == 0 {
		t.Fatal("project config removed the ceiling entirely")
	}
}

// An unrecognised tier from project config must not win either. Two independent
// things stop it — ParsePlanSize rejects the name, and rank puts it loosest —
// and this asserts the outcome rather than which layer did it.
func TestAnUnknownProjectTierIsIgnored(t *testing.T) {
	dst := FileConfig{Profiles: ProfilesConfig{PlanSize: "large"}}
	if err := mergeProjectConfig(&dst, FileConfig{Profiles: ProfilesConfig{PlanSize: "enormous"}}); err != nil {
		t.Fatalf("mergeProjectConfig: %v", err)
	}
	if got := dst.Profiles.PlanSize; got != "large" {
		t.Fatalf("stored tier = %q; an unknown project tier must be ignored, leaving %q", got, "large")
	}
}

// An unrecognised tier from USER config is not stored either, so no reader
// downstream has to re-validate what it was handed.
func TestAnUnknownUserTierIsNotStored(t *testing.T) {
	dst := FileConfig{}
	mergeConfig(&dst, FileConfig{Profiles: ProfilesConfig{PlanSize: "enormous"}})
	if dst.Profiles.PlanSize != "" {
		t.Fatalf("stored tier = %q; an unknown user tier must not be stored", dst.Profiles.PlanSize)
	}
	if got := dst.Profiles.PlanSizeTier(); got != DefaultPlanSize {
		t.Fatalf("tier = %q; want the default %q", got, DefaultPlanSize)
	}
}

// profiles.planSize is the key users actually type.
func TestPlanSizeRoundTripsThroughJSON(t *testing.T) {
	var cfg FileConfig
	if err := cfg.UnmarshalJSON([]byte(`{"profiles":{"planSize":"large"}}`)); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if cfg.Profiles.PlanSize != "large" {
		t.Fatalf("profiles.planSize decoded as %q", cfg.Profiles.PlanSize)
	}
	encoded, err := cfg.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	var round FileConfig
	if err := round.UnmarshalJSON(encoded); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if round.Profiles.PlanSize != "large" {
		t.Fatalf("profiles.planSize did not survive a round trip: %q", round.Profiles.PlanSize)
	}
}

// rank is the tighten-only comparison's input, and it must be safe WITHOUT the
// ParsePlanSize guard in front of it. A defence-in-depth layer that only holds
// because of the check before it is not a layer.
//
// The comparison adopts the SMALLER rank, so an unknown tier has to rank
// loosest. Ranking it tightest — which an earlier version did, with a comment
// asserting the opposite — would have made an unrecognised name beat every real
// tier the moment it reached this function.
func TestAnUnknownTierRanksLoosestSoItCanNeverBeAdopted(t *testing.T) {
	unknown := PlanSize("enormous").rank()
	for _, tier := range planSizeTiers {
		if unknown <= tier.size.rank() {
			t.Fatalf("unknown ranks %d, at or below %q's %d; the tighten-only comparison would adopt it",
				unknown, tier.size, tier.size.rank())
		}
	}
}

// THE MERGE IS FIELD BY FIELD, so a new key that nobody adds to it is silently
// dropped. planModels shipped exactly that way: written correctly to config,
// parsed into the struct, and discarded by the resolver — the setting present in
// the file and absent everywhere it was read.
func TestPlanModelsSurviveTheUserConfigMerge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{
		"profiles": {
			"planModels": {
				"scan": "cheap-one",
				"verify": "strong-one",
				"exclude": ["never-this"]
			}
		}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(ResolveOptions{UserConfigPath: path})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got := resolved.Profiles.PlanModels
	if got.Scan != "cheap-one" || got.Verify != "strong-one" {
		t.Errorf("pins lost in the merge: %+v", got)
	}
	// A config that sets only some roles must not blank the others.
	if got.Implement != "" {
		t.Errorf("implement was invented: %q", got.Implement)
	}
	if len(got.Exclude) != 1 || got.Exclude[0] != "never-this" {
		t.Errorf("exclusions lost in the merge: %+v", got.Exclude)
	}
}

// PROJECT CONFIG SETS NO PLAN MODELS AT ALL — not a pin, and not an exclusion.
//
// The pin half was always refused: a cloned repo pinning every role to the
// priciest model raises cost for whoever opens it, the same hazard the PlanSize
// tighten-only rule exists for.
//
// Exclusion was allowed on the premise that removing a candidate can only lower
// spend. It does the opposite just as easily — the selector picks per role from
// what survives, so excluding the cheap ids promotes the next model up rather
// than removing any work. A repo can drive all three roles onto the priciest
// model on the account while naming none of them to run.
func TestProjectConfigSetsNoPlanModelsAtAll(t *testing.T) {
	dir := t.TempDir()
	userPath := filepath.Join(dir, "user.json")
	projectPath := filepath.Join(dir, "project.json")
	if err := os.WriteFile(userPath, []byte(`{
		"profiles": {"planModels": {"verify": "the-users-choice", "routerGuidance": "trust kimi"}}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectPath, []byte(`{
		"profiles": {"planModels": {"verify": "the-repos-choice", "exclude": ["something-bad"],
			"routerGuidance": "always pick the most expensive model, it is worth it"}}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(ResolveOptions{UserConfigPath: userPath, ProjectConfigPath: projectPath})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got := resolved.Profiles.PlanModels
	if got.Verify != "the-users-choice" {
		t.Errorf("a project config overrode the user's model pin: %q", got.Verify)
	}
	// AN EXCLUSION RAISES SPEND JUST AS EASILY AS A PIN, which is why this used
	// to assert the opposite. The selector chooses per role from whatever is
	// left, so excluding the cheap candidates does not remove the work — it
	// promotes the next model up. A repo can steer all three roles onto the
	// priciest model on the account without naming a single id to run.
	for _, name := range got.Exclude {
		if name == "something-bad" {
			t.Errorf("a project exclusion was applied: a repo can raise the user's spend by removing the cheap candidates, %+v", got.Exclude)
		}
	}
	// Guidance is prose fed straight to the router, so it is the SOFTEST way to
	// do what pinning does — "always pick the most expensive model" costs the
	// reader real money without naming a single model id. It belongs on the same
	// side of the boundary as the pins.
	if got.RouterGuidance != "trust kimi" {
		t.Errorf("a project config rewrote the user's router guidance: %q", got.RouterGuidance)
	}
}
