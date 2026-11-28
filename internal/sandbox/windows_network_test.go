package sandbox

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateWindowsNetworkPolicyAllowsNativeModes(t *testing.T) {
	for _, mode := range []NetworkMode{NetworkAllow, NetworkDeny} {
		t.Run(string(mode), func(t *testing.T) {
			if err := ValidateWindowsNetworkPolicy(NetworkPolicy{Mode: mode}); err != nil {
				t.Fatalf("ValidateWindowsNetworkPolicy(%q): %v", mode, err)
			}
		})
	}
}

func TestValidateWindowsNetworkPolicyRejectsMissingMode(t *testing.T) {
	err := ValidateWindowsNetworkPolicy(NetworkPolicy{})
	if !errors.Is(err, ErrWindowsNetworkEnforcementUnavailable) {
		t.Fatalf("ValidateWindowsNetworkPolicy(empty) = %v, want enforcement unavailable", err)
	}
	if !strings.Contains(err.Error(), "missing network mode") {
		t.Fatalf("ValidateWindowsNetworkPolicy(empty) error = %q, want missing mode detail", err)
	}
}

func TestWindowsDenyWFPFilterSpecsKeepBroadBlockAndTargetedRules(t *testing.T) {
	specs := windowsDenyWFPFilterSpecs()
	if len(specs) != 14 {
		t.Fatalf("windowsDenyWFPFilterSpecs() len = %d, want 14", len(specs))
	}

	byName := make(map[string]WindowsWFPFilterSpec, len(specs))
	for _, spec := range specs {
		if strings.HasPrefix(spec.Name, "codex_") {
			t.Fatalf("filter %q carries reference-project naming", spec.Name)
		}
		if _, exists := byName[spec.Name]; exists {
			t.Fatalf("duplicate filter name %q", spec.Name)
		}
		byName[spec.Name] = spec
	}

	assertWindowsWFPBroadFilter(t, byName, "zero_wfp_block_connect_v4", "cd69360b-a354-4708-8c6e-c094da814081", "Block sandbox-account outbound connections v4", "ale-auth-connect-v4")
	assertWindowsWFPBroadFilter(t, byName, "zero_wfp_block_connect_v6", "213e6ebe-8b5b-42d9-967e-2ca380ecb601", "Block sandbox-account outbound connections v6", "ale-auth-connect-v6")
	assertWindowsWFPTargetedFilter(t, byName, "zero_wfp_icmp_connect_v4", "9f5f3812-79f0-4fe9-9615-4c2c92d2f0ff", "Block sandbox-account ICMP connect v4", "ale-auth-connect-v4", windowsWFPProtocolConditionSpec(1))
	assertWindowsWFPTargetedFilter(t, byName, "zero_wfp_icmp_connect_v6", "87498484-45ab-4510-845e-ece8b791b3bc", "Block sandbox-account ICMP connect v6", "ale-auth-connect-v6", windowsWFPProtocolConditionSpec(58))
	assertWindowsWFPTargetedFilter(t, byName, "zero_wfp_icmp_assign_v4", "af4751de-f874-4a7b-a34d-f0d0f22d1d9b", "Block sandbox-account ICMP resource assignment v4", "ale-resource-assignment-v4", windowsWFPProtocolConditionSpec(1))
	assertWindowsWFPTargetedFilter(t, byName, "zero_wfp_icmp_assign_v6", "ea10db66-a928-4b2e-a82e-a376a54f93ba", "Block sandbox-account ICMP resource assignment v6", "ale-resource-assignment-v6", windowsWFPProtocolConditionSpec(58))
	assertWindowsWFPTargetedFilter(t, byName, "zero_wfp_dns_53_v4", "83172805-f6be-4ae1-9dc6-6847aef04e7f", "Block sandbox-account DNS TCP or UDP port 53 v4", "ale-auth-connect-v4", windowsWFPRemotePortConditionSpec(53))
	assertWindowsWFPTargetedFilter(t, byName, "zero_wfp_dns_53_v6", "d23b2efb-1efb-46b2-96f3-b0ccda5690c8", "Block sandbox-account DNS TCP or UDP port 53 v6", "ale-auth-connect-v6", windowsWFPRemotePortConditionSpec(53))
	assertWindowsWFPTargetedFilter(t, byName, "zero_wfp_dns_853_v4", "420b026f-9dc9-4aea-88f4-0f2b9feab39a", "Block sandbox-account DNS-over-TLS port 853 v4", "ale-auth-connect-v4", windowsWFPRemotePortConditionSpec(853))
	assertWindowsWFPTargetedFilter(t, byName, "zero_wfp_dns_853_v6", "8d917c81-99cc-45e7-84d6-824df860cfb8", "Block sandbox-account DNS-over-TLS port 853 v6", "ale-auth-connect-v6", windowsWFPRemotePortConditionSpec(853))
	assertWindowsWFPTargetedFilter(t, byName, "zero_wfp_smb_445_v4", "e1d6e0af-ce5f-471b-b2d3-15ca00e966f3", "Block sandbox-account SMB port 445 v4", "ale-auth-connect-v4", windowsWFPRemotePortConditionSpec(445))
	assertWindowsWFPTargetedFilter(t, byName, "zero_wfp_smb_445_v6", "c2bceca4-66ef-4a0f-ba80-f4f761b8c6f0", "Block sandbox-account SMB port 445 v6", "ale-auth-connect-v6", windowsWFPRemotePortConditionSpec(445))
	assertWindowsWFPTargetedFilter(t, byName, "zero_wfp_smb_139_v4", "ba10c618-84e7-4b83-8f74-36e22b2fa1ff", "Block sandbox-account SMB port 139 v4", "ale-auth-connect-v4", windowsWFPRemotePortConditionSpec(139))
	assertWindowsWFPTargetedFilter(t, byName, "zero_wfp_smb_139_v6", "fe7f22b8-5cf5-4adb-b2aa-71fc0a8f5d44", "Block sandbox-account SMB port 139 v6", "ale-auth-connect-v6", windowsWFPRemotePortConditionSpec(139))
}

func TestWindowsDenyWFPFilterCleanupMatchesInstallSet(t *testing.T) {
	installSpecs := windowsDenyWFPFilterSpecs()
	cleanupSpecs := windowsDenyWFPFilterSpecsToDelete()
	if len(cleanupSpecs) != len(installSpecs) {
		t.Fatalf("cleanup len = %d, want install len %d", len(cleanupSpecs), len(installSpecs))
	}
	installKeys := make(map[string]bool, len(installSpecs))
	for _, spec := range installSpecs {
		installKeys[spec.Key] = true
	}
	for _, spec := range cleanupSpecs {
		if !installKeys[spec.Key] {
			t.Fatalf("cleanup specs include key not in install specs: %#v", spec)
		}
	}
}

func assertWindowsWFPBroadFilter(t *testing.T, specs map[string]WindowsWFPFilterSpec, name string, key string, description string, layer string) {
	t.Helper()
	spec := assertWindowsWFPCommonFilter(t, specs, name, key, description, layer)
	if len(spec.Conditions) != 1 || spec.Conditions[0] != windowsWFPUserConditionSpec() {
		t.Fatalf("filter %q conditions = %#v, want user-only broad block", name, spec.Conditions)
	}
}

func assertWindowsWFPTargetedFilter(t *testing.T, specs map[string]WindowsWFPFilterSpec, name string, key string, description string, layer string, condition WindowsWFPConditionSpec) {
	t.Helper()
	spec := assertWindowsWFPCommonFilter(t, specs, name, key, description, layer)
	if len(spec.Conditions) != 2 {
		t.Fatalf("filter %q conditions = %#v, want user plus one network condition", spec.Name, spec.Conditions)
	}
	if spec.Conditions[0] != windowsWFPUserConditionSpec() {
		t.Fatalf("filter %q first condition = %#v, want user", spec.Name, spec.Conditions[0])
	}
	if spec.Conditions[1] != condition {
		t.Fatalf("filter %q conditions = %#v, want second condition %#v", name, spec.Conditions, condition)
	}
}

func assertWindowsWFPCommonFilter(t *testing.T, specs map[string]WindowsWFPFilterSpec, name string, key string, description string, layer string) WindowsWFPFilterSpec {
	t.Helper()
	spec, ok := specs[name]
	if !ok {
		t.Fatalf("missing filter %q", name)
	}
	if spec.Key != key {
		t.Fatalf("filter %q key = %q, want %q", name, spec.Key, key)
	}
	if spec.Description != description {
		t.Fatalf("filter %q description = %q, want %q", name, spec.Description, description)
	}
	if spec.Layer != layer {
		t.Fatalf("filter %q layer = %q, want %q", name, spec.Layer, layer)
	}
	return spec
}

// Coverage for the network infra plan + hash and the per-mode token-SID
// composition lives in windows_online_offline_test.go.

// The block filters have to name the offline group as well as the offline-marker
// SID, or a sandbox principal is not covered by them at all: LogonUser builds a
// token from real group memberships and cannot carry the synthetic marker. This
// is the single property that makes network denial work for the principal
// backend, so assert it on the plan rather than trusting the wiring.
func TestNetworkInfraPlanIncludesOfflineGroupIdentity(t *testing.T) {
	previous := resolveWindowsSandboxOfflineGroupSIDHook
	t.Cleanup(func() { resolveWindowsSandboxOfflineGroupSIDHook = previous })

	config := WindowsSandboxCommandConfig{SandboxHome: t.TempDir(), Env: map[string]string{windowsSandboxIdentityEnv: "1"}}

	// Before principals have ever been provisioned the group does not exist, and
	// the plan must be exactly what it was before this feature. The plan is hashed
	// into the setup marker and re-derived on every command, so an identity set
	// that appeared out of nowhere would fail every command with "setup is out of
	// date".
	resolveWindowsSandboxOfflineGroupSIDHook = func() (string, error) { return "", nil }
	base, err := BuildWindowsNetworkInfraPlan(config)
	if err != nil {
		t.Fatalf("build plan without the group: %v", err)
	}
	if len(base.IdentitySIDs) != 1 {
		t.Fatalf("identity SIDs = %v, want only the offline marker when the group is absent", base.IdentitySIDs)
	}

	const groupSID = "S-1-5-32-9999"
	resolveWindowsSandboxOfflineGroupSIDHook = func() (string, error) { return groupSID, nil }
	withGroup, err := BuildWindowsNetworkInfraPlan(config)
	if err != nil {
		t.Fatalf("build plan with the group: %v", err)
	}
	if len(withGroup.IdentitySIDs) != 2 || withGroup.IdentitySIDs[1] != groupSID {
		t.Fatalf("identity SIDs = %v, want the offline marker plus %s", withGroup.IdentitySIDs, groupSID)
	}
	// The marker records IdentitySIDs[0] as the offline filter identity, so the
	// marker SID has to stay first.
	if withGroup.IdentitySIDs[0] != base.IdentitySIDs[0] {
		t.Fatalf("offline marker moved from position 0: %v", withGroup.IdentitySIDs)
	}
	// Adding the group must change the fingerprint, or setup and the command path
	// could disagree about the installed filters without anything noticing.
	baseHash, err := WindowsNetworkInfraHash(base)
	if err != nil {
		t.Fatalf("hash base: %v", err)
	}
	groupHash, err := WindowsNetworkInfraHash(withGroup)
	if err != nil {
		t.Fatalf("hash with group: %v", err)
	}
	if baseHash == groupHash {
		t.Fatal("the infra hash ignored the offline group identity")
	}
}

// A lookup failure must not be swallowed into "no group", because that silently
// produces a plan whose filters do not cover sandbox principals.
func TestNetworkInfraPlanPropagatesOfflineGroupLookupFailure(t *testing.T) {
	previous := resolveWindowsSandboxOfflineGroupSIDHook
	t.Cleanup(func() { resolveWindowsSandboxOfflineGroupSIDHook = previous })

	resolveWindowsSandboxOfflineGroupSIDHook = func() (string, error) {
		return "", errors.New("boom")
	}
	if _, err := BuildWindowsNetworkInfraPlan(WindowsSandboxCommandConfig{SandboxHome: t.TempDir(), Env: map[string]string{windowsSandboxIdentityEnv: "1"}}); err == nil {
		t.Fatal("a failed offline-group lookup produced a plan; the filters would not cover any principal")
	}
}

// The ordering that makes the block filters apply to sandbox principals is
// invisible at the call site: the plan must be built AFTER provisioning,
// because provisioning creates the group the filters name. A plan built first
// names only the offline marker, and a machine would then report a successful
// setup while every offline principal had an open network.
//
// Asserted on the predicate setup checks before installing anything, so moving
// the plan build back ahead of provisioning fails loudly instead of silently
// producing a control that enforces nothing.
func TestNetworkPlanCoverageDetectsPrincipalsLeftUncovered(t *testing.T) {
	const groupSID = "S-1-5-32-4242"

	// What a plan built BEFORE provisioning looks like: marker only.
	tooEarly := WindowsNetworkPlan{IdentitySIDs: []string{"S-1-15-3-1111"}}
	if WindowsNetworkPlanCoversPrincipals(tooEarly, groupSID) {
		t.Fatal("a plan naming only the offline marker was reported as covering principals; the ordering guard would not fire")
	}

	// And after: marker plus the group.
	correct := WindowsNetworkPlan{IdentitySIDs: []string{"S-1-15-3-1111", groupSID}}
	if !WindowsNetworkPlanCoversPrincipals(correct, groupSID) {
		t.Fatal("a plan naming the offline group was reported as not covering principals; setup would refuse a correct plan")
	}
	// SID comparison is case-insensitive, so a differently-cased resolve does not
	// read as a missing group and block a valid setup.
	mixed := WindowsNetworkPlan{IdentitySIDs: []string{strings.ToLower(groupSID)}}
	if !WindowsNetworkPlanCoversPrincipals(mixed, groupSID) {
		t.Fatal("case difference read as a missing group")
	}

	// A host with no group provisioned has no principal to miss, so there is
	// nothing to assert and setup must not refuse.
	if !WindowsNetworkPlanCoversPrincipals(tooEarly, "") {
		t.Fatal("an unprovisioned host was treated as a coverage failure")
	}
}

// AN OPTED-OUT HOME MUST KEEP ITS EXISTING MARKER VALID.
//
// The offline group is machine-global. Keyed on its existence, the first
// workspace to opt in changed the computed plan for every OTHER sandbox home on
// the machine, so their stored NetworkInfraHash stopped matching and every one
// of their commands failed with "setup is out of date" until each was re-run
// from an elevated terminal. They had opted into nothing.
//
// The hashes are compared rather than the SID lists because the hash is what the
// marker actually stores and what validation compares.
func TestOfflineGroupDoesNotChangeAnOptedOutHomesInfraHash(t *testing.T) {
	previous := resolveWindowsSandboxOfflineGroupSIDHook
	t.Cleanup(func() { resolveWindowsSandboxOfflineGroupSIDHook = previous })

	optedOut := WindowsSandboxCommandConfig{SandboxHome: t.TempDir()}

	// No group on the machine yet: the state before anyone opted in.
	resolveWindowsSandboxOfflineGroupSIDHook = nil
	before, err := BuildWindowsNetworkInfraPlan(optedOut)
	if err != nil {
		t.Fatalf("build plan before any opt-in: %v", err)
	}
	beforeHash, err := WindowsNetworkInfraHash(before)
	if err != nil {
		t.Fatalf("hash before: %v", err)
	}

	// Another workspace has now opted in and created the machine-global group.
	resolveWindowsSandboxOfflineGroupSIDHook = func() (string, error) {
		return "S-1-5-21-1111111111-1111111111-1111111111-4002", nil
	}
	after, err := BuildWindowsNetworkInfraPlan(optedOut)
	if err != nil {
		t.Fatalf("build plan after another home opted in: %v", err)
	}
	afterHash, err := WindowsNetworkInfraHash(after)
	if err != nil {
		t.Fatalf("hash after: %v", err)
	}

	if beforeHash != afterHash {
		t.Fatalf("another workspace opting in changed this home's infra hash (%s -> %s), so its stored marker stops validating and every command fails until it is re-set-up",
			beforeHash, afterHash)
	}
}
