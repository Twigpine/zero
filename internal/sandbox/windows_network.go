package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

var ErrWindowsNetworkEnforcementUnavailable = errors.New("Windows sandbox network enforcement is not available") //nolint:staticcheck // Preserve the exported sentinel's established text.

const (
	windowsWFPProviderKey = "2e31d31c-3948-4753-9117-e5d1a6496f41"
	windowsWFPSubLayerKey = "e65054fd-4d32-4c7c-95ef-621f0cf6431a"
)

type WindowsNetworkPlan struct {
	Mode         NetworkMode            `json:"mode"`
	ProviderKey  string                 `json:"providerKey,omitempty"`
	SubLayerKey  string                 `json:"subLayerKey,omitempty"`
	IdentitySIDs []string               `json:"identitySids,omitempty"`
	Filters      []WindowsWFPFilterSpec `json:"filters,omitempty"`
}

type WindowsWFPFilterSpec struct {
	Key         string                    `json:"key"`
	Name        string                    `json:"name"`
	Description string                    `json:"description,omitempty"`
	Layer       string                    `json:"layer"`
	Action      string                    `json:"action"`
	Conditions  []WindowsWFPConditionSpec `json:"conditions,omitempty"`
}

type WindowsWFPConditionSpec struct {
	Kind  string `json:"kind"`
	Value uint16 `json:"value,omitempty"`
}

func ValidateWindowsNetworkPolicy(network NetworkPolicy) error {
	switch network.Mode {
	case NetworkAllow, NetworkDeny:
		return nil
	case "":
		return fmt.Errorf("%w: missing network mode", ErrWindowsNetworkEnforcementUnavailable)
	default:
		return fmt.Errorf("unsupported Windows sandbox network mode %q", network.Mode)
	}
}

// BuildWindowsNetworkInfraPlan returns the mode-INDEPENDENT network
// infrastructure that `zero sandbox setup` installs: the persistent outbound
// block filters scoped to the sandbox home's offline-marker SID. It is identical
// for allow and deny command configs — the per-command mode is enforced at
// runtime by whether the restricted token carries the offline-marker SID, not by
// which filters exist. This is what the setup marker fingerprints, so one setup
// validly serves both modes.
func BuildWindowsNetworkInfraPlan(config WindowsSandboxCommandConfig) (WindowsNetworkPlan, error) {
	offlineSID, err := WindowsOfflineMarkerSID(config.SandboxHome)
	if err != nil {
		return WindowsNetworkPlan{}, err
	}
	// The offline-marker SID stays first: the setup marker records
	// IdentitySIDs[0] as the offline filter identity.
	identitySIDs := []string{offlineSID}
	// A sandbox principal cannot carry the offline-marker SID, because LogonUser
	// builds a token from an account's real group memberships and the marker is a
	// synthetic capability SID. So the same filters additionally match a real
	// local group that network-denied principals belong to.
	//
	// Gated on THIS home's opt-in, not merely on the group existing.
	//
	// The group is machine-global, so keying off its existence meant the first
	// workspace to opt in changed the computed plan for every OTHER sandbox home
	// on the machine. The plan is hashed into the setup marker and compared on
	// every command, so those homes started failing every command with "setup is
	// out of date" until each was re-run from an elevated terminal, having never
	// opted into anything. Existing markers have to keep validating.
	//
	// Reading the opt-in instead keeps an opted-out home computing exactly the
	// plan it computed before any of this existed. An opted-in home records the
	// group in its own marker, and setup and the command path both derive the
	// flag from the same environment, so they agree. Opting in AFTER setup does
	// invalidate that home's marker, which is correct: it has no principals yet
	// and setup is genuinely required.
	if windowsSandboxIdentityEnabled(config.Env) && resolveWindowsSandboxOfflineGroupSIDHook != nil {
		groupSID, err := resolveWindowsSandboxOfflineGroupSIDHook()
		if err != nil {
			return WindowsNetworkPlan{}, err
		}
		if trimmed := strings.TrimSpace(groupSID); trimmed != "" {
			identitySIDs = append(identitySIDs, trimmed)
		}
	}
	return WindowsNetworkPlan{
		Mode:         NetworkDeny,
		ProviderKey:  windowsWFPProviderKey,
		SubLayerKey:  windowsWFPSubLayerKey,
		IdentitySIDs: identitySIDs,
		Filters:      windowsDenyWFPFilterSpecs(),
	}, nil
}

// WindowsNetworkPlanForApply returns the plan to INSTALL, which is not always
// the plan a home fingerprints.
//
// The filters are machine-global and every setup installs them by deleting and
// recreating the fixed set. So an opted-OUT setup for workspace B was replacing
// filters that workspace A's opted-in setup had installed, dropping the offline
// group SID as it went. A's offline principal still passed the runtime
// membership check and A's marker still validated, but no filter matched its
// token any more, so a NetworkDeny command in A quietly gained egress. Nothing
// on either side reported anything: B did exactly what it was asked to.
//
// The gate in BuildWindowsNetworkInfraPlan stays as it is, and this is
// deliberately a SEPARATE function rather than a change to it. That gate exists
// because the plan is hashed into each home's setup marker: keying it on the
// group's existence made the first workspace to opt in invalidate every other
// home's marker on the machine, and those homes then failed every command with
// "setup is out of date" having opted into nothing. That has to keep working.
//
// The two questions are simply different. What a home RECORDS is about that
// home's own configuration; what setup INSTALLS is about the machine, where the
// group either exists or does not. Answering both from one plan is what forced a
// choice between stale markers and a silent hole.
//
// The group not existing is the ordinary opted-out machine, and it adds nothing.
//
// A resolution FAILURE is a different answer and is fatal, because the filters
// are replaced wholesale. The earlier reasoning here was that refusing to
// install would trade a partial denial for no denial at all, and that is wrong:
// the alternative to installing is leaving the filters that are already there
// alone. Returning a marker-only plan does not decline to add coverage, it
// actively REMOVES coverage another workspace is relying on. An opted-out home
// skips assertWindowsNetworkPlanCoversOfflineGroup entirely (it is gated on
// provisioned), so this is the only thing between a transient lookup error and
// workspace A silently regaining egress under NetworkDeny.
//
// "Absent" and "could not be read" have to be different answers, which is the
// same distinction the marker/apply split above is built on.
func WindowsNetworkPlanForApply(plan WindowsNetworkPlan, resolveOfflineGroupSID func() (string, error)) (WindowsNetworkPlan, error) {
	if resolveOfflineGroupSID == nil {
		return plan, nil
	}
	groupSID, err := resolveOfflineGroupSID()
	if err != nil {
		return WindowsNetworkPlan{}, fmt.Errorf("resolve the sandbox offline group before replacing the machine network filters: %w", err)
	}
	trimmed := strings.TrimSpace(groupSID)
	if trimmed == "" {
		return plan, nil
	}
	for _, existing := range plan.IdentitySIDs {
		if strings.EqualFold(strings.TrimSpace(existing), trimmed) {
			return plan, nil
		}
	}
	// Copied rather than appended in place: the caller's plan is what gets
	// fingerprinted, and growing its backing array would risk changing the
	// recorded plan as a side effect of installing one.
	augmented := plan
	augmented.IdentitySIDs = append(append([]string{}, plan.IdentitySIDs...), trimmed)
	return augmented, nil
}

// WindowsNetworkPlanCoversPrincipals reports whether a plan's block filters name
// the offline group, which is the only thing that makes them apply to a sandbox
// principal.
//
// This exists to be asserted, not consulted. The plan must be built AFTER
// provisioning, because provisioning is what creates the group it resolves; a
// plan built first names only the offline marker and leaves every offline
// principal with an open network while setup reports success. That is a control
// that enforces nothing while claiming to work, and the ordering which prevents
// it is invisible at the call site, so a later refactor can undo it silently.
func WindowsNetworkPlanCoversPrincipals(plan WindowsNetworkPlan, offlineGroupSID string) bool {
	offlineGroupSID = strings.TrimSpace(offlineGroupSID)
	if offlineGroupSID == "" {
		// No group provisioned on this host, so there is no principal for the
		// filters to miss and nothing to assert.
		return true
	}
	for _, sid := range plan.IdentitySIDs {
		if strings.EqualFold(strings.TrimSpace(sid), offlineGroupSID) {
			return true
		}
	}
	return false
}

// assertWindowsNetworkPlanCoversOfflineGroup is the post-provisioning check that
// setup runs before it installs filters and reports success.
//
// It fails closed on every answer that is not a definite yes, because the thing
// it guards is invisible when it goes wrong: filters that do not name the
// offline group leave every offline principal with an open network on a machine
// whose setup marker says it is protected. Nobody sees an error, and the sandbox
// looks correctly installed.
//
// Two answers other than "the plan omits the group" also mean the check did not
// happen and must not be treated as a pass:
//
//   - The lookup failed. Whether the group is covered is then unknown, and the
//     Win32 reason is worth surfacing: an operator seeing "access is denied"
//     knows to re-run elevated.
//   - The lookup succeeded and found nothing. ("", nil) means the group does not
//     exist, which is the ordinary state BEFORE provisioning and an impossible
//     one after it, so here it means the group vanished or was never created.
//     WindowsNetworkPlanCoversPrincipals answers true for an empty SID, correctly
//     for the pre-provisioning callers that ask it, so the emptiness has to be
//     rejected here rather than delegated to it.
//
// provisioned says whether principals were actually provisioned on this run.
// When they were not there is no offline group, no principal carrying it, and
// nothing for the filters to miss, so there is nothing to assert. Running the
// check anyway turns the ordinary opt-out setup into a hard failure, because a
// machine with no group resolves to ("", nil) and the empty-SID rejection below
// is correct only after provisioning.
func assertWindowsNetworkPlanCoversOfflineGroup(plan WindowsNetworkPlan, resolve func() (string, error), provisioned bool) error {
	if !provisioned {
		return nil
	}
	if resolve == nil {
		return errors.New("sandbox offline group resolver is not wired up, so filter coverage cannot be verified")
	}
	groupSID, err := resolve()
	if err != nil {
		return fmt.Errorf("resolve the sandbox offline group after provisioning, so filter coverage cannot be verified: %w", err)
	}
	if strings.TrimSpace(groupSID) == "" {
		return errors.New("the sandbox offline group does not exist after provisioning, so the block filters would not apply to any sandbox principal")
	}
	if !WindowsNetworkPlanCoversPrincipals(plan, groupSID) {
		return errors.New("network block filters do not name the sandbox offline group, so they would not apply to any sandbox principal; the network plan must be built after principals are provisioned")
	}
	return nil
}

// resolveWindowsSandboxOfflineGroupSIDHook resolves the local group that
// network-denied principals belong to. It is wired up on Windows only, so this
// file stays free of Win32 calls and the plan on other platforms is unchanged.
//
// Returning ("", nil) means the group does not exist yet, which is the state
// before principals have ever been provisioned.
var resolveWindowsSandboxOfflineGroupSIDHook func() (string, error)

// WindowsNetworkInfraHash fingerprints the provisioned (mode-independent) network
// infrastructure so the setup marker validates against the same setup for BOTH
// command modes. It never folds in the per-command network mode.
func WindowsNetworkInfraHash(plan WindowsNetworkPlan) (string, error) {
	canonical := struct {
		ProviderKey  string                 `json:"providerKey"`
		SubLayerKey  string                 `json:"subLayerKey"`
		IdentitySIDs []string               `json:"identitySids"`
		Filters      []WindowsWFPFilterSpec `json:"filters"`
	}{
		ProviderKey:  plan.ProviderKey,
		SubLayerKey:  plan.SubLayerKey,
		IdentitySIDs: canonicalWindowsNetworkSIDs(plan.IdentitySIDs),
		Filters:      canonicalWindowsWFPFilterSpecs(plan.Filters),
	}
	bytes, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("marshal windows network infra hash input: %w", err)
	}
	sum := sha256.Sum256(bytes)
	return hex.EncodeToString(sum[:]), nil
}

func windowsDenyWFPFilterSpecs() []WindowsWFPFilterSpec {
	return []WindowsWFPFilterSpec{
		{
			Key:         "cd69360b-a354-4708-8c6e-c094da814081",
			Name:        "zero_wfp_block_connect_v4",
			Description: "Block sandbox-account outbound connections v4",
			Layer:       "ale-auth-connect-v4",
			Action:      "block",
			Conditions:  []WindowsWFPConditionSpec{windowsWFPUserConditionSpec()},
		},
		{
			Key:         "213e6ebe-8b5b-42d9-967e-2ca380ecb601",
			Name:        "zero_wfp_block_connect_v6",
			Description: "Block sandbox-account outbound connections v6",
			Layer:       "ale-auth-connect-v6",
			Action:      "block",
			Conditions:  []WindowsWFPConditionSpec{windowsWFPUserConditionSpec()},
		},
		{
			Key:         "9f5f3812-79f0-4fe9-9615-4c2c92d2f0ff",
			Name:        "zero_wfp_icmp_connect_v4",
			Description: "Block sandbox-account ICMP connect v4",
			Layer:       "ale-auth-connect-v4",
			Action:      "block",
			Conditions:  []WindowsWFPConditionSpec{windowsWFPUserConditionSpec(), windowsWFPProtocolConditionSpec(1)},
		},
		{
			Key:         "87498484-45ab-4510-845e-ece8b791b3bc",
			Name:        "zero_wfp_icmp_connect_v6",
			Description: "Block sandbox-account ICMP connect v6",
			Layer:       "ale-auth-connect-v6",
			Action:      "block",
			Conditions:  []WindowsWFPConditionSpec{windowsWFPUserConditionSpec(), windowsWFPProtocolConditionSpec(58)},
		},
		{
			Key:         "af4751de-f874-4a7b-a34d-f0d0f22d1d9b",
			Name:        "zero_wfp_icmp_assign_v4",
			Description: "Block sandbox-account ICMP resource assignment v4",
			Layer:       "ale-resource-assignment-v4",
			Action:      "block",
			Conditions:  []WindowsWFPConditionSpec{windowsWFPUserConditionSpec(), windowsWFPProtocolConditionSpec(1)},
		},
		{
			Key:         "ea10db66-a928-4b2e-a82e-a376a54f93ba",
			Name:        "zero_wfp_icmp_assign_v6",
			Description: "Block sandbox-account ICMP resource assignment v6",
			Layer:       "ale-resource-assignment-v6",
			Action:      "block",
			Conditions:  []WindowsWFPConditionSpec{windowsWFPUserConditionSpec(), windowsWFPProtocolConditionSpec(58)},
		},
		{
			Key:         "83172805-f6be-4ae1-9dc6-6847aef04e7f",
			Name:        "zero_wfp_dns_53_v4",
			Description: "Block sandbox-account DNS TCP or UDP port 53 v4",
			Layer:       "ale-auth-connect-v4",
			Action:      "block",
			Conditions:  []WindowsWFPConditionSpec{windowsWFPUserConditionSpec(), windowsWFPRemotePortConditionSpec(53)},
		},
		{
			Key:         "d23b2efb-1efb-46b2-96f3-b0ccda5690c8",
			Name:        "zero_wfp_dns_53_v6",
			Description: "Block sandbox-account DNS TCP or UDP port 53 v6",
			Layer:       "ale-auth-connect-v6",
			Action:      "block",
			Conditions:  []WindowsWFPConditionSpec{windowsWFPUserConditionSpec(), windowsWFPRemotePortConditionSpec(53)},
		},
		{
			Key:         "420b026f-9dc9-4aea-88f4-0f2b9feab39a",
			Name:        "zero_wfp_dns_853_v4",
			Description: "Block sandbox-account DNS-over-TLS port 853 v4",
			Layer:       "ale-auth-connect-v4",
			Action:      "block",
			Conditions:  []WindowsWFPConditionSpec{windowsWFPUserConditionSpec(), windowsWFPRemotePortConditionSpec(853)},
		},
		{
			Key:         "8d917c81-99cc-45e7-84d6-824df860cfb8",
			Name:        "zero_wfp_dns_853_v6",
			Description: "Block sandbox-account DNS-over-TLS port 853 v6",
			Layer:       "ale-auth-connect-v6",
			Action:      "block",
			Conditions:  []WindowsWFPConditionSpec{windowsWFPUserConditionSpec(), windowsWFPRemotePortConditionSpec(853)},
		},
		{
			Key:         "e1d6e0af-ce5f-471b-b2d3-15ca00e966f3",
			Name:        "zero_wfp_smb_445_v4",
			Description: "Block sandbox-account SMB port 445 v4",
			Layer:       "ale-auth-connect-v4",
			Action:      "block",
			Conditions:  []WindowsWFPConditionSpec{windowsWFPUserConditionSpec(), windowsWFPRemotePortConditionSpec(445)},
		},
		{
			Key:         "c2bceca4-66ef-4a0f-ba80-f4f761b8c6f0",
			Name:        "zero_wfp_smb_445_v6",
			Description: "Block sandbox-account SMB port 445 v6",
			Layer:       "ale-auth-connect-v6",
			Action:      "block",
			Conditions:  []WindowsWFPConditionSpec{windowsWFPUserConditionSpec(), windowsWFPRemotePortConditionSpec(445)},
		},
		{
			Key:         "ba10c618-84e7-4b83-8f74-36e22b2fa1ff",
			Name:        "zero_wfp_smb_139_v4",
			Description: "Block sandbox-account SMB port 139 v4",
			Layer:       "ale-auth-connect-v4",
			Action:      "block",
			Conditions:  []WindowsWFPConditionSpec{windowsWFPUserConditionSpec(), windowsWFPRemotePortConditionSpec(139)},
		},
		{
			Key:         "fe7f22b8-5cf5-4adb-b2aa-71fc0a8f5d44",
			Name:        "zero_wfp_smb_139_v6",
			Description: "Block sandbox-account SMB port 139 v6",
			Layer:       "ale-auth-connect-v6",
			Action:      "block",
			Conditions:  []WindowsWFPConditionSpec{windowsWFPUserConditionSpec(), windowsWFPRemotePortConditionSpec(139)},
		},
	}
}

func windowsDenyWFPFilterSpecsToDelete() []WindowsWFPFilterSpec {
	return windowsDenyWFPFilterSpecs()
}

func windowsWFPUserConditionSpec() WindowsWFPConditionSpec {
	return WindowsWFPConditionSpec{Kind: "user"}
}

func windowsWFPProtocolConditionSpec(protocol uint16) WindowsWFPConditionSpec {
	return WindowsWFPConditionSpec{Kind: "protocol", Value: protocol}
}

func windowsWFPRemotePortConditionSpec(port uint16) WindowsWFPConditionSpec {
	return WindowsWFPConditionSpec{Kind: "remote-port", Value: port}
}

func canonicalWindowsNetworkSIDs(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i]) < strings.ToLower(out[j])
	})
	return out
}

func canonicalWindowsWFPFilterSpecs(filters []WindowsWFPFilterSpec) []WindowsWFPFilterSpec {
	out := make([]WindowsWFPFilterSpec, 0, len(filters))
	seen := map[string]struct{}{}
	for _, filter := range filters {
		filter.Key = strings.ToLower(strings.TrimSpace(filter.Key))
		filter.Name = strings.TrimSpace(filter.Name)
		filter.Layer = strings.TrimSpace(filter.Layer)
		filter.Action = strings.TrimSpace(filter.Action)
		filter.Conditions = canonicalWindowsWFPConditionSpecs(filter.Conditions)
		if filter.Key == "" || filter.Name == "" || filter.Layer == "" || filter.Action == "" {
			continue
		}
		if _, ok := seen[filter.Key]; ok {
			continue
		}
		seen[filter.Key] = struct{}{}
		out = append(out, filter)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Key < out[j].Key
	})
	return out
}

func canonicalWindowsWFPConditionSpecs(conditions []WindowsWFPConditionSpec) []WindowsWFPConditionSpec {
	out := make([]WindowsWFPConditionSpec, 0, len(conditions))
	for _, condition := range conditions {
		condition.Kind = strings.TrimSpace(condition.Kind)
		if condition.Kind == "" {
			continue
		}
		out = append(out, condition)
	}
	return out
}
