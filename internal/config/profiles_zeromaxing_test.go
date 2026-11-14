package config

import "testing"

// (n) A project .zero/config.json may DISABLE the posture. This mirrors the
// Sandbox.Network tighten-only rule: project scope may make a run stricter.
func TestProjectConfigCanDisableZeromaxing(t *testing.T) {
	dst := FileConfig{}
	if err := mergeProjectConfig(&dst, FileConfig{Profiles: ProfilesConfig{DisableZeromaxing: true}}); err != nil {
		t.Fatalf("mergeProjectConfig: %v", err)
	}
	if !dst.Profiles.DisableZeromaxing {
		t.Fatal("a project config must be able to disable the posture")
	}
}

// (m) ...and may NOT enable it. A cloned repo must not be able to switch a cost
// multiplier ON for whoever opens it. Like an ignored network "allow", the
// attempt is dropped silently rather than raised as an error — the project
// scope simply does not hold that privilege.
func TestProjectConfigCannotEnableZeromaxing(t *testing.T) {
	dst := FileConfig{Profiles: ProfilesConfig{DisableZeromaxing: true}}
	if err := mergeProjectConfig(&dst, FileConfig{Profiles: ProfilesConfig{DisableZeromaxing: false}}); err != nil {
		t.Fatalf("mergeProjectConfig: %v", err)
	}
	if !dst.Profiles.DisableZeromaxing {
		t.Fatal("a project config must NOT re-enable the posture after the user scope disabled it")
	}
}

// The user scope is higher-trust and may disable it too.
func TestUserConfigCanDisableZeromaxing(t *testing.T) {
	dst := FileConfig{}
	mergeConfig(&dst, FileConfig{Profiles: ProfilesConfig{DisableZeromaxing: true}})
	if !dst.Profiles.DisableZeromaxing {
		t.Fatal("user config must be able to disable the posture")
	}
}

// The default is OFF: an absent setting leaves it available, so the feature is
// opt-out and no existing config changes behaviour.
func TestZeromaxingIsEnabledByDefault(t *testing.T) {
	dst := FileConfig{}
	mergeConfig(&dst, FileConfig{})
	if err := mergeProjectConfig(&dst, FileConfig{}); err != nil {
		t.Fatalf("mergeProjectConfig: %v", err)
	}
	if dst.Profiles.DisableZeromaxing {
		t.Fatal("the posture must be available unless a config explicitly disables it")
	}
}

// The setting survives JSON round-tripping, so profiles.disableZeromaxing is the
// actual key users type.
func TestProfilesConfigRoundTripsThroughJSON(t *testing.T) {
	var cfg FileConfig
	if err := cfg.UnmarshalJSON([]byte(`{"profiles":{"disableZeromaxing":true}}`)); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if !cfg.Profiles.DisableZeromaxing {
		t.Fatal("profiles.disableZeromaxing did not decode")
	}
	encoded, err := cfg.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	var round FileConfig
	if err := round.UnmarshalJSON(encoded); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if !round.Profiles.DisableZeromaxing {
		t.Fatalf("the setting was lost in the round trip: %s", encoded)
	}
}
