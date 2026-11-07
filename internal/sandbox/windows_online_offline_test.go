package sandbox

import (
	"encoding/json"
	"os"
	"testing"
)

// windowsRuntimeTokenSIDs is the per-mode restricting-SID composer: both modes
// keep the write-capability SIDs (write-jail), deny additionally carries the
// offline-marker SID the WFP block filter matches.
func TestWindowsRuntimeTokenSIDs(t *testing.T) {
	caps := []string{"S-1-write-a", "S-1-write-b"}
	offline := "S-1-offline"

	deny := windowsRuntimeTokenSIDs(caps, offline, NetworkDeny)
	if !containsString(deny, offline) {
		t.Errorf("deny token must carry the offline-marker SID: %v", deny)
	}
	allow := windowsRuntimeTokenSIDs(caps, offline, NetworkAllow)
	if containsString(allow, offline) {
		t.Errorf("allow token must NOT carry the offline-marker SID: %v", allow)
	}
	// Both modes keep the write-capability SIDs — the workspace write-jail holds
	// either way.
	for _, mode := range []NetworkMode{NetworkDeny, NetworkAllow} {
		got := windowsRuntimeTokenSIDs(caps, offline, mode)
		for _, c := range caps {
			if !containsString(got, c) {
				t.Errorf("mode %q dropped write-capability SID %q: %v", mode, c, got)
			}
		}
	}
}

// The provisioned infrastructure is identical for allow and deny configs, so one
// setup serves both modes (and its fingerprint is stable across modes).
func TestBuildWindowsNetworkInfraPlanIsModeIndependent(t *testing.T) {
	home := t.TempDir()
	// Pinned, because the count below depends on whether this machine happens to
	// have the offline group already. BuildWindowsNetworkInfraPlan folds that
	// group's SID in when it resolves, so on a Windows host where an earlier
	// elevated setup created ZeroSandboxOffline the plan legitimately carries two
	// identity SIDs and this test failed on a correct plan. CI never saw it: the
	// Linux and macOS jobs leave the hook nil, and a fresh Windows runner has no
	// group yet. Stubbing it makes the assertion about the plan rather than about
	// the machine it runs on.
	previousHook := resolveWindowsSandboxOfflineGroupSIDHook
	t.Cleanup(func() { resolveWindowsSandboxOfflineGroupSIDHook = previousHook })
	resolveWindowsSandboxOfflineGroupSIDHook = nil

	mk := func(mode NetworkMode) WindowsSandboxCommandConfig {
		return WindowsSandboxCommandConfig{
			SandboxHome: home,
			// Opted in, because the offline group only enters the plan for a home
			// that asked for principals. Without this the plan is the pre-principal
			// one and the group assertions below have nothing to find.
			Env:            map[string]string{windowsSandboxIdentityEnv: "1"},
			CommandCWD:     `C:\ws`,
			WorkspaceRoots: []string{`C:\ws`},
			PermissionProfile: PermissionProfile{
				FileSystem: FileSystemPolicy{Kind: FileSystemRestricted, WriteRoots: []WritableRoot{{Root: `C:\ws`}}},
				Network:    NetworkPolicy{Mode: mode},
			},
		}
	}
	denyPlan, err := BuildWindowsNetworkInfraPlan(mk(NetworkDeny))
	if err != nil {
		t.Fatalf("deny infra plan: %v", err)
	}
	allowPlan, err := BuildWindowsNetworkInfraPlan(mk(NetworkAllow))
	if err != nil {
		t.Fatalf("allow infra plan: %v", err)
	}
	if len(denyPlan.Filters) != 14 || len(denyPlan.IdentitySIDs) != 1 {
		t.Fatalf("infra plan should be 2 broad plus 12 targeted block filters scoped to 1 offline SID, got %#v", denyPlan)
	}
	denyHash, _ := WindowsNetworkInfraHash(denyPlan)
	allowHash, _ := WindowsNetworkInfraHash(allowPlan)
	if denyHash != allowHash || denyHash == "" {
		t.Fatalf("infra hash must be identical across modes: deny=%q allow=%q", denyHash, allowHash)
	}
	// The plan is scoped to the offline-marker SID, never the write-capability SIDs.
	offline, err := WindowsOfflineMarkerSID(home)
	if err != nil {
		t.Fatalf("offline SID: %v", err)
	}
	if denyPlan.IdentitySIDs[0] != offline {
		t.Errorf("infra plan SID = %q, want offline-marker %q", denyPlan.IdentitySIDs[0], offline)
	}

	// Mode independence has to hold on a machine that already has the offline
	// group too, which is where the pinned counts above would otherwise be
	// asserting something about the host rather than the plan. The group SID is
	// folded in for both modes, so the hashes must still agree; only the count
	// changes.
	resolveWindowsSandboxOfflineGroupSIDHook = func() (string, error) { return "S-1-5-32-9999", nil }
	withGroupDeny, err := BuildWindowsNetworkInfraPlan(mk(NetworkDeny))
	if err != nil {
		t.Fatalf("deny infra plan with the group present: %v", err)
	}
	withGroupAllow, err := BuildWindowsNetworkInfraPlan(mk(NetworkAllow))
	if err != nil {
		t.Fatalf("allow infra plan with the group present: %v", err)
	}
	// Assert the SIDs themselves, not the count. A duplicate offline marker or an
	// unrelated SID would satisfy a length check while meaning something quite
	// different.
	if len(withGroupDeny.IdentitySIDs) != 2 ||
		withGroupDeny.IdentitySIDs[0] != offline ||
		withGroupDeny.IdentitySIDs[1] != "S-1-5-32-9999" {
		t.Fatalf("group present should add its SID after the offline marker, got %v (offline marker %q)", withGroupDeny.IdentitySIDs, offline)
	}
	groupDenyHash, _ := WindowsNetworkInfraHash(withGroupDeny)
	groupAllowHash, _ := WindowsNetworkInfraHash(withGroupAllow)
	if groupDenyHash != groupAllowHash || groupDenyHash == "" {
		t.Fatalf("infra hash must stay mode-independent with the group present: deny=%q allow=%q", groupDenyHash, groupAllowHash)
	}
	// And it must differ from the no-group hash, which is the cross-workspace
	// coupling called out for a maintainer decision: one workspace creating the
	// group changes every other sandbox home's expected fingerprint.
	if groupDenyHash == denyHash {
		t.Error("group presence did not change the infra hash; the marker staleness this causes is a deliberate property and should be visible here")
	}
}

// A pre-existing schema-1 capability file (no Offline SID) is upgraded in place:
// an Offline SID is minted and persisted, idempotently.
func TestLoadOrCreateWindowsCapabilitySIDsUpgradesOffline(t *testing.T) {
	home := t.TempDir()
	legacy := WindowsCapabilitySIDs{SchemaVersion: 1, ReadOnly: "S-1-ro"}
	bytes, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(WindowsCapabilitySIDPath(home), bytes, 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}
	caps, err := LoadOrCreateWindowsCapabilitySIDs(home)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if caps.Offline == "" {
		t.Fatal("upgrade must mint an offline-marker SID for a legacy file")
	}
	if caps.ReadOnly != "S-1-ro" {
		t.Errorf("upgrade must preserve existing ReadOnly SID, got %q", caps.ReadOnly)
	}
	// Idempotent: reload returns the same persisted Offline SID.
	again, err := LoadOrCreateWindowsCapabilitySIDs(home)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if again.Offline != caps.Offline {
		t.Errorf("offline SID not stable across reload: %q vs %q", again.Offline, caps.Offline)
	}
}
