package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Twigpine/zero/internal/peermsg"
)

const WindowsSandboxSetupName = "zero-windows-sandbox-setup.exe"

// Bumped to 7 when the single principal was split into offline and online role
// accounts. THE NAMES CHANGED, so a marker written by the previous version is
// not evidence that this version's setup has run: it validates by comparing
// serialized plans and hashes, and neither names an account. Left at 6, an
// upgraded machine kept a valid marker, setup was never re-run, and the runner
// found neither "zero-sbx-d<key>" nor "zero-sbx-n<key>" and fell back to the
// restricted-token backend with no read confinement, silently.
//
// Bumping it makes that installation report as out of date, which is what sends
// the operator back through elevated setup. Setup then provisions both roles and
// retires the untagged predecessor (see removeWindowsSandboxPrincipalsForSetup).
const windowsSandboxSetupMarkerSchemaVersion = 7

// windowsSandboxIdentityEnv opts a machine into the principal backend while it
// is still experimental. Provisioning is inert without it, so an existing
// install keeps the restricted-token behaviour until someone turns this on.
//
// Lives here, beside the setup protocol rather than beside the Windows-only
// runtime, because the opt-in is part of that protocol: it has to be readable on
// every platform so the setup args and the marker can carry it.
const windowsSandboxIdentityEnv = "ZERO_WINDOWS_SANDBOX_IDENTITY"

// windowsSandboxIdentityEnabled reports whether the principal backend is opted
// into. An explicit entry in env is authoritative; otherwise the process
// environment decides.
func windowsSandboxIdentityEnabled(env map[string]string) bool {
	if value, ok := env[windowsSandboxIdentityEnv]; ok {
		return strings.TrimSpace(value) == "1"
	}
	return strings.TrimSpace(os.Getenv(windowsSandboxIdentityEnv)) == "1"
}

// WindowsSandboxPrincipalOptIn resolves the principal opt-in for callers outside
// this package (the `zero sandbox setup` CLI and `zero doctor`), so both sides
// of the setup protocol read the opt-in the same way. Pass nil to consult the
// current process environment.
func WindowsSandboxPrincipalOptIn(env map[string]string) bool {
	return windowsSandboxIdentityEnabled(env)
}

func windowsSandboxPrincipalOptInValue(optIn bool) string {
	if optIn {
		return "1"
	}
	return "0"
}

type WindowsSandboxSetupArgsOptions struct {
	SandboxHome       string
	CommandCWD        string
	WorkspaceRoots    []string
	PermissionProfile PermissionProfile
	// PrincipalOptIn is the caller's principal opt-in, serialized into the setup
	// args. Elevated setup runs in its own process — a UAC-elevated one whose
	// environment is not the caller's — so it must be told the value rather than
	// left to sample an environment nobody set.
	//
	// Tri-state on purpose. nil means "this caller did not resolve the opt-in",
	// and BuildWindowsSandboxSetupArgs then resolves it from the environment of
	// the process building the args — which is the caller's own process, the one
	// place where the ambient value is the value the operator typed. A plain bool
	// could not say that: its zero value asserts "opted out", so every caller that
	// simply did not know about this field would serialize `--sandbox-principal 0`
	// while the command half still resolved the opt-in from its environment. Under
	// a machine-wide opt-in the two halves would then disagree and marker
	// validation would refuse every command — the same silent-disagreement bug
	// this flag exists to remove, re-created one layer up.
	//
	// Set it only to override the environment (a caller holding a command's Env
	// map, or a test pinning a value); leave it nil to mean "whatever this shell
	// says", which is what `zero sandbox setup` and `zero doctor` want.
	PrincipalOptIn *bool
	// CallerSID identifies the Windows user setup was invoked BY, serialized into
	// the args for the same reason PrincipalOptIn is: elevated setup runs in its
	// own process, and under over-the-shoulder UAC (or `runas`) that process
	// belongs to a DIFFERENT administrator than the caller.
	//
	// Everything a principal needs is scoped to the invoking user — the account
	// name, its ACL ledger and its DPAPI secret all key off that identity — so a
	// helper that sampled its own token would provision an account for the
	// administrator who typed the credentials while every later command looked for
	// one named after the ordinary user, found nothing, fell back to the restricted
	// token, and left an admin-keyed account nothing would ever reclaim.
	//
	// Empty means "resolve from the process building the args", which IS the
	// caller: these args are built before the UAC boundary is crossed.
	CallerSID string
}

// callerSID resolves the invoking user, in the caller's own process, before the
// args cross the UAC boundary. Empty when the token cannot be read (and on
// non-Windows builds), which leaves the elevated helper on its pre-existing
// behaviour of sampling its own token rather than inventing an identity.
func (options WindowsSandboxSetupArgsOptions) callerSID() string {
	if sid := strings.TrimSpace(options.CallerSID); sid != "" {
		return sid
	}
	return windowsCurrentUserSID()
}

// principalOptIn resolves the tri-state. It runs inside
// BuildWindowsSandboxSetupArgs, i.e. in the caller's process, before the args
// cross the UAC boundary — so an unset caller still ships an explicit 0|1 that
// the elevated helper can trust.
func (options WindowsSandboxSetupArgsOptions) principalOptIn() bool {
	if options.PrincipalOptIn != nil {
		return *options.PrincipalOptIn
	}
	return windowsSandboxIdentityEnabled(nil)
}

type WindowsSandboxSetupConfig struct {
	SandboxHome       string
	CommandCWD        string
	WorkspaceRoots    []string
	PermissionProfile PermissionProfile
	PrincipalOptIn    bool
	// CallerSID is the invoking user this setup is provisioning FOR. See
	// WindowsSandboxSetupArgsOptions.CallerSID. Empty means the caller did not
	// say, and the helper falls back to its own token.
	CallerSID string
}

type WindowsSandboxSetupMarker struct {
	SchemaVersion  int    `json:"schemaVersion"`
	ACLPlanHash    string `json:"aclPlanHash"`
	ACLPlanEntries int    `json:"aclPlanEntries"`
	// NetworkInfraHash fingerprints the mode-INDEPENDENT network infrastructure
	// setup provisioned (block filters scoped to the offline-marker SID), so one
	// marker validly serves both an allow command and a deny command. It replaces
	// the old per-command NetworkPolicyHash/NetworkPlanHash, which locked the
	// marker to a single mode and bricked approved network commands.
	NetworkInfraHash string `json:"networkInfraHash"`
	OfflineFilterSID string `json:"offlineFilterSid"`
	NetworkFilters   int    `json:"networkFilters"`
	// PrincipalOptIn records whether the run that wrote this marker provisioned a
	// sandbox principal. Without it the two halves each sampled their own
	// environment and could disagree silently — see
	// ValidateWindowsSandboxSetupMarker.
	PrincipalOptIn bool `json:"principalOptIn"`
	// PrincipalPlanHash fingerprints the PRINCIPAL ACL plan, which ACLPlanHash
	// above does not cover: that one hashes BuildWindowsACLPlan, the
	// capability-SID plan, while principal grants are built separately by
	// buildWindowsPrincipalACLPlan from the same profile.
	//
	// Without it, narrowing or removing a principal read root left setup looking
	// current, so the old AllowRead ACEs stayed on disk with nothing to notice
	// they no longer matched the policy. Empty when the principal backend is not
	// opted into, which keeps the marker stable for the default install.
	PrincipalPlanHash string `json:"principalPlanHash,omitempty"`
}

func WindowsSandboxSetupMarkerPath(sandboxHome string) string {
	return filepath.Join(sandboxHome, "windows-setup.json")
}

func BuildWindowsSandboxSetupArgs(options WindowsSandboxSetupArgsOptions) ([]string, error) {
	commandCWD := strings.TrimSpace(options.CommandCWD)
	if commandCWD == "" {
		return nil, errors.New("windows sandbox setup requires command cwd")
	}
	sandboxHome := strings.TrimSpace(options.SandboxHome)
	if sandboxHome == "" {
		var err error
		sandboxHome, err = ResolveWindowsSandboxHome(nil)
		if err != nil {
			return nil, err
		}
	}
	workspaceRoots := trimNonEmptyStrings(options.WorkspaceRoots)
	if len(workspaceRoots) == 0 {
		workspaceRoots = []string{commandCWD}
	}
	// THE PRINCIPAL BACKEND HAS NO LAUNCH PATH YET, SO IT IS NOT PROVISIONED.
	//
	// Launching a command as a separate account needs SeAssignPrimaryTokenPrivilege
	// and SeIncreaseQuotaPrivilege. An ordinary unelevated process holds neither,
	// and the command runs LATER, from the caller, not from setup. So the
	// advertised lifecycle -- provision once from an elevated terminal, then run
	// ordinary commands -- cannot execute: the elevated setup process holds the
	// privileges and the process that needs them does not.
	//
	// An earlier version of this check tested the CALLING process's token, which
	// is the wrong lifetime. It refused unelevated setup, which was never the
	// dangerous case, and PASSED elevated setup, which is exactly the case that
	// creates a local account, its password, its logon-right assignments,
	// workspace ACEs, the recovery ledger and network filter state for a backend
	// no later command can use. Durable machine state serving something that
	// cannot run is the outcome the check existed to prevent, and it produced it.
	//
	// Requiring every sandboxed command to run elevated would make the privileges
	// line up, and is deliberately not done: it is a worse boundary than the
	// restricted token this would replace, and it is not the lifecycle the feature
	// is for. Until a reviewed launch mechanism exists -- a bootstrap or an
	// authenticated broker that carries launch authority across the UAC boundary
	// -- provisioning stays unavailable rather than half-working.
	//
	// Everything below the launch path (the plans, the ledger, the ACL and filter
	// work) still builds and is still tested; it is the provisioning entry point
	// that is closed. See #662, which this PR is foundation for and does not
	// close.
	if options.principalOptIn() {
		if err := windowsPrincipalLaunchAvailable(); err != nil {
			return nil, fmt.Errorf("refusing to provision a sandbox principal: %w", err)
		}
	}
	// Augmented here, in the caller's shell, before the args cross into the
	// elevated helper. Same reason the opt-in and caller SID are: the value has to
	// be resolved where the environment is the operator's.
	options.PermissionProfile = WindowsSandboxProfileWithRuntimeRoots(options.PermissionProfile, workspaceRoots)
	profileJSON, err := json.Marshal(options.PermissionProfile)
	if err != nil {
		return nil, fmt.Errorf("marshal windows sandbox setup permission profile: %w", err)
	}
	args := []string{
		"--sandbox-home", sandboxHome,
		"--command-cwd", commandCWD,
		"--permission-profile", string(profileJSON),
		// Always explicit, never omitted-means-false: the elevated helper must be
		// able to tell "the caller wants no principal" from "an older caller said
		// nothing", and only the first of those is safe to run silently.
		"--sandbox-principal", windowsSandboxPrincipalOptInValue(options.principalOptIn()),
	}
	// Omitted rather than sent empty when the caller's identity cannot be read:
	// absence means "this caller did not say", which the helper answers by
	// sampling its own token exactly as it did before this flag existed.
	if callerSID := options.callerSID(); callerSID != "" {
		args = append(args, "--caller-sid", callerSID)
	}
	for _, root := range workspaceRoots {
		args = append(args, "--workspace-root", root)
	}
	return args, nil
}

func ParseWindowsSandboxSetupArgs(args []string) (WindowsSandboxSetupConfig, error) {
	var config WindowsSandboxSetupConfig
	var profileJSON string
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch arg {
		case "--command-cwd":
			value, next, err := nextWindowsSandboxFlagValue(args, index)
			if err != nil {
				return WindowsSandboxSetupConfig{}, err
			}
			config.CommandCWD = strings.TrimSpace(value)
			index = next
		case "--sandbox-home":
			value, next, err := nextWindowsSandboxFlagValue(args, index)
			if err != nil {
				return WindowsSandboxSetupConfig{}, err
			}
			config.SandboxHome = strings.TrimSpace(value)
			index = next
		case "--workspace-root":
			value, next, err := nextWindowsSandboxFlagValue(args, index)
			if err != nil {
				return WindowsSandboxSetupConfig{}, err
			}
			if root := strings.TrimSpace(value); root != "" {
				config.WorkspaceRoots = append(config.WorkspaceRoots, root)
			}
			index = next
		case "--permission-profile":
			value, next, err := nextWindowsSandboxFlagValue(args, index)
			if err != nil {
				return WindowsSandboxSetupConfig{}, err
			}
			profileJSON = strings.TrimSpace(value)
			index = next
		case "--sandbox-principal":
			value, next, err := nextWindowsSandboxFlagValue(args, index)
			if err != nil {
				return WindowsSandboxSetupConfig{}, err
			}
			switch strings.TrimSpace(value) {
			case "1":
				config.PrincipalOptIn = true
			case "0":
				config.PrincipalOptIn = false
			default:
				// Refused rather than treated as off: a value this helper cannot read
				// is a caller it does not understand, and guessing "no principal"
				// there would provision a weaker sandbox than the caller asked for
				// while reporting success.
				return WindowsSandboxSetupConfig{}, fmt.Errorf("invalid --sandbox-principal %q, want 0 or 1", value)
			}
			index = next
		case "--caller-sid":
			value, next, err := nextWindowsSandboxFlagValue(args, index)
			if err != nil {
				return WindowsSandboxSetupConfig{}, err
			}
			config.CallerSID = strings.TrimSpace(value)
			index = next
		default:
			return WindowsSandboxSetupConfig{}, fmt.Errorf("unknown windows sandbox setup flag %q", arg)
		}
	}
	if config.CommandCWD == "" {
		return WindowsSandboxSetupConfig{}, errors.New("missing --command-cwd")
	}
	if config.SandboxHome == "" {
		return WindowsSandboxSetupConfig{}, errors.New("missing --sandbox-home")
	}
	if len(config.WorkspaceRoots) == 0 {
		config.WorkspaceRoots = []string{config.CommandCWD}
	}
	if profileJSON == "" {
		return WindowsSandboxSetupConfig{}, errors.New("missing --permission-profile")
	}
	if err := json.Unmarshal([]byte(profileJSON), &config.PermissionProfile); err != nil {
		return WindowsSandboxSetupConfig{}, fmt.Errorf("invalid --permission-profile: %w", err)
	}
	return config, nil
}

func RunWindowsSandboxSetup(args []string, stderr io.Writer) int {
	config, err := ParseWindowsSandboxSetupArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, WindowsSandboxSetupName+": "+err.Error())
		return 2
	}
	if err := windowsDenyReadRestrictedTokenUnsupportedProfile(config.PermissionProfile); err != nil {
		fmt.Fprintln(stderr, WindowsSandboxSetupName+": "+err.Error())
		return 1
	}
	return runWindowsSandboxSetup(config, stderr)
}

// commandConfig is the command-shaped view the setup half plans against. Its Env
// carries the opt-in the caller serialized into the setup args, so every
// downstream windowsSandboxIdentityEnabled call — the gate that decides whether
// elevated setup provisions a principal at all — reads the caller's intent
// rather than sampling the elevated helper's own environment, which UAC does not
// inherit from the shell the user typed in.
func (config WindowsSandboxSetupConfig) commandConfig() WindowsSandboxCommandConfig {
	return WindowsSandboxCommandConfig{
		SandboxHome:    config.SandboxHome,
		CommandCWD:     config.CommandCWD,
		WorkspaceRoots: cloneStrings(config.WorkspaceRoots),
		// Taken as given. This computes the hash BOTH halves compare, and it runs
		// in whichever process is asking — including the command runner, whose TEMP
		// points at the sandbox runtime temp. Deriving the candidates here made the
		// answer depend on the caller's environment, which is the one thing a
		// shared fingerprint cannot afford. WindowsSandboxProfileWithRuntimeRoots
		// is applied by the callers whose TEMP is still the operator's.
		PermissionProfile: config.PermissionProfile,
		Env:               map[string]string{windowsSandboxIdentityEnv: windowsSandboxPrincipalOptInValue(config.PrincipalOptIn)},
		SandboxLevel:      WindowsSandboxLevelRestrictedToken,
		// Carried through for the same reason as the opt-in above: every principal
		// name, ledger and secret path the setup half plans is derived from the
		// invoking user, and that is the caller's identity, not this process's.
		CallerSID: config.CallerSID,
	}
}

// WindowsSandboxSetupConfigFromCommand is how a command asks "was setup run for
// what I need?". It carries the command's own opt-in so marker validation can
// compare it against what setup actually provisioned.
func WindowsSandboxSetupConfigFromCommand(config WindowsSandboxCommandConfig) WindowsSandboxSetupConfig {
	return WindowsSandboxSetupConfig{
		SandboxHome:    config.SandboxHome,
		CommandCWD:     config.CommandCWD,
		WorkspaceRoots: cloneStrings(config.WorkspaceRoots),
		// NOT augmented here. This runs inside the command runner, whose TEMP and
		// TMP are the sandbox runtime's, so deriving the temp-based candidate here
		// yields a path no other process agrees on. The parent already folded the
		// candidates into the profile it serialized, back when TEMP was still the
		// operator's, so take the profile as given.
		PermissionProfile: config.PermissionProfile,
		PrincipalOptIn:    windowsSandboxIdentityEnabled(config.Env),
		CallerSID:         config.CallerSID,
	}
}

func BuildWindowsSandboxSetupMarker(config WindowsSandboxSetupConfig) (WindowsSandboxSetupMarker, error) {
	plan, err := BuildWindowsACLPlan(config.commandConfig())
	if err != nil {
		return WindowsSandboxSetupMarker{}, err
	}
	hash, err := WindowsACLPlanHash(plan)
	if err != nil {
		return WindowsSandboxSetupMarker{}, err
	}
	// Fingerprint the mode-INDEPENDENT network infrastructure (block filters
	// scoped to the offline-marker SID), NOT the per-command network mode, so the
	// marker validates for both allow and deny commands against this one setup.
	infraPlan, err := BuildWindowsNetworkInfraPlan(config.commandConfig())
	if err != nil {
		return WindowsSandboxSetupMarker{}, err
	}
	infraHash, err := WindowsNetworkInfraHash(infraPlan)
	if err != nil {
		return WindowsSandboxSetupMarker{}, err
	}
	offlineSID := ""
	if len(infraPlan.IdentitySIDs) > 0 {
		offlineSID = infraPlan.IdentitySIDs[0]
	}
	principalHash, err := windowsPrincipalPlanFingerprint(config)
	if err != nil {
		return WindowsSandboxSetupMarker{}, err
	}
	return WindowsSandboxSetupMarker{
		SchemaVersion:     windowsSandboxSetupMarkerSchemaVersion,
		ACLPlanHash:       hash,
		ACLPlanEntries:    len(plan.Entries),
		NetworkInfraHash:  infraHash,
		OfflineFilterSID:  offlineSID,
		NetworkFilters:    len(infraPlan.Filters),
		PrincipalOptIn:    config.PrincipalOptIn,
		PrincipalPlanHash: principalHash,
	}, nil
}

// windowsPrincipalPlanFingerprint hashes the principal ACL plan so a change to
// principal read or write roots invalidates setup.
//
// The SID is a fixed placeholder rather than the real principal's, deliberately.
// The account is recreated with a fresh SID on reprovision, so hashing the real
// one would make the fingerprint change every time the account is rebuilt even
// though the GRANTED PATHS are identical, and every command would then rerun
// setup. What must invalidate the marker is the set of paths and actions, which
// is exactly what this captures.
//
// Returns empty when the principal backend is not opted into, so the default
// install's marker is unchanged.
func windowsPrincipalPlanFingerprint(config WindowsSandboxSetupConfig) (string, error) {
	if !config.PrincipalOptIn {
		return "", nil
	}
	filesystem := config.commandConfig().PermissionProfile.FileSystem
	// EVERY FIELD apply and teardown pass, or the fingerprint describes a
	// different plan than the one that gets applied. This omitted DenyWrite while
	// both buildWindowsPrincipalACLPlan call sites in windows_identity_runtime_windows.go
	// passed it, so a change to the policy's deny-write paths moved the applied
	// plan and left this hash where it was. The capability ACLPlanHash happens to
	// cover the same paths today, which is what kept it from being a live hole
	// and also what would have kept it invisible.
	plan, err := buildWindowsPrincipalACLPlan(windowsPrincipalACLInput{
		PrincipalSID: windowsPrincipalFingerprintSID,
		WriteRoots:   filesystem.WriteRoots,
		ReadRoots:    filesystem.ReadRoots,
		DenyRead:     filesystem.DenyRead,
		DenyWrite:    filesystem.DenyWrite,
	})
	if err != nil {
		return "", fmt.Errorf("fingerprint windows principal ACL plan: %w", err)
	}
	return WindowsACLPlanHash(plan)
}

// windowsPrincipalFingerprintSID is a placeholder trustee used only for hashing.
// It never reaches an ACE.
const windowsPrincipalFingerprintSID = "S-1-0-0"

func WriteWindowsSandboxSetupMarker(config WindowsSandboxSetupConfig) (WindowsSandboxSetupMarker, error) {
	marker, err := BuildWindowsSandboxSetupMarker(config)
	if err != nil {
		return WindowsSandboxSetupMarker{}, err
	}
	path := WindowsSandboxSetupMarkerPath(config.SandboxHome)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return WindowsSandboxSetupMarker{}, fmt.Errorf("create windows sandbox setup marker dir: %w", err)
	}
	bytes, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return WindowsSandboxSetupMarker{}, fmt.Errorf("marshal windows sandbox setup marker: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".windows-setup-*.tmp")
	if err != nil {
		return WindowsSandboxSetupMarker{}, fmt.Errorf("create windows sandbox setup marker temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(bytes); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return WindowsSandboxSetupMarker{}, fmt.Errorf("write windows sandbox setup marker temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return WindowsSandboxSetupMarker{}, fmt.Errorf("close windows sandbox setup marker temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return WindowsSandboxSetupMarker{}, fmt.Errorf("replace windows sandbox setup marker: %w", err)
	}
	return marker, nil
}

func ValidateWindowsSandboxSetupMarker(config WindowsSandboxSetupConfig) error {
	expected, err := BuildWindowsSandboxSetupMarker(config)
	if err != nil {
		return err
	}
	path := WindowsSandboxSetupMarkerPath(config.SandboxHome)
	bytes, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("windows sandbox is not initialized for this workspace — run `zero sandbox setup` from an elevated (Administrator) terminal (missing %s)", filepath.Base(path))
		}
		return fmt.Errorf("read windows sandbox setup marker: %w", err)
	}
	var actual WindowsSandboxSetupMarker
	if err := json.Unmarshal(bytes, &actual); err != nil {
		return fmt.Errorf("parse windows sandbox setup marker: %w", err)
	}
	if actual.SchemaVersion != expected.SchemaVersion {
		return fmt.Errorf("windows sandbox setup is out of date: schema %d, want %d", actual.SchemaVersion, expected.SchemaVersion)
	}
	// The two halves of the protocol run in different processes, so they can
	// disagree about the principal opt-in. Refuse the command rather than pick a
	// winner.
	//
	// The direction that matters is the first one: the opt-in is on, setup never
	// provisioned an account, and the runtime's lookup declines with a nil error —
	// so without this the command runs on the restricted token, which does not
	// confine reads, while the operator believes a principal is isolating them.
	// A sandbox that is weaker than advertised has to be loud.
	//
	// The reverse is refused too. It is not the dangerous direction — the command
	// gets the well-worn restricted token it asked for — but setup did create a
	// local account and grant it ACEs on the workspace, and letting commands run
	// as if that had not happened leaves nothing to reconcile it. Both directions
	// clear the same way: run `zero sandbox setup` again with the environment you
	// actually want.
	if actual.PrincipalOptIn != expected.PrincipalOptIn {
		if expected.PrincipalOptIn {
			return fmt.Errorf("windows sandbox setup is out of date: %s=1 asks for a sandbox principal, but setup provisioned none — "+
				"re-run `zero sandbox setup` from an elevated (Administrator) terminal with %s=1, or unset it to use the restricted-token sandbox",
				windowsSandboxIdentityEnv, windowsSandboxIdentityEnv)
		}
		// "principals", plural: a workspace gets an offline and an online account,
		// and the opt-out path retires the legacy one alongside them. The singular
		// described the pre-split world and understated what the re-run does.
		return fmt.Errorf("windows sandbox setup is out of date: setup provisioned sandbox principals, but %s is not set for this command — "+
			"set %s=1, or re-run `zero sandbox setup` from an elevated (Administrator) terminal without it to retire them",
			windowsSandboxIdentityEnv, windowsSandboxIdentityEnv)
	}
	if actual.ACLPlanHash != expected.ACLPlanHash || actual.ACLPlanEntries != expected.ACLPlanEntries {
		// Say WHAT disagrees. The bare "permission roots or deny lists changed"
		// left an operator with a command that refuses to run and no way to tell
		// which side is wrong, or even whether the marker belongs to this
		// workspace at all. The entry counts separate the two shapes this takes:
		// equal counts mean the same roots spelled differently, unequal counts
		// mean one side has roots the other has never heard of.
		return fmt.Errorf("windows sandbox setup is out of date: permission roots or deny lists changed "+
			"(marker %s has %d entries, hash %s; this command expects %d entries, hash %s) — "+
			"re-run `zero sandbox setup` from an elevated (Administrator) terminal",
			path, actual.ACLPlanEntries, shortWindowsACLPlanHash(actual.ACLPlanHash),
			expected.ACLPlanEntries, shortWindowsACLPlanHash(expected.ACLPlanHash))
	}
	// The capability-SID plan above and the principal plan are built separately
	// from the same profile, so the hash above does not cover principal grants.
	// Without this check, removing a principal read root left setup looking
	// current and the stale AllowRead ACE in place, which is the opposite of
	// what narrowing a policy is supposed to do.
	if actual.PrincipalPlanHash != expected.PrincipalPlanHash {
		return errors.New("windows sandbox setup is out of date: sandbox principal grants changed — " +
			"re-run `zero sandbox setup` from an elevated (Administrator) terminal so the old grants are revoked")
	}
	// Mode-agnostic: validate the provisioned infrastructure, never the
	// per-command network mode — so an approved (allow) network command and an
	// ordinary (deny) command both validate against this one setup.
	if actual.NetworkInfraHash != expected.NetworkInfraHash {
		return errors.New("windows sandbox setup is out of date: network infrastructure changed")
	}
	if actual.OfflineFilterSID != expected.OfflineFilterSID {
		return errors.New("windows sandbox setup is out of date: offline network identity changed")
	}
	if actual.NetworkFilters != expected.NetworkFilters {
		return errors.New("windows sandbox setup is out of date: network enforcement plan changed")
	}
	return nil
}

func WindowsACLPlanHash(plan WindowsACLPlan) (string, error) {
	entries := canonicalWindowsACLEntries(plan.Entries)
	bytes, err := json.Marshal(entries)
	if err != nil {
		return "", fmt.Errorf("marshal windows ACL plan hash input: %w", err)
	}
	sum := sha256.Sum256(bytes)
	return hex.EncodeToString(sum[:]), nil
}

func canonicalWindowsACLEntries(entries []WindowsACLEntry) []WindowsACLEntry {
	out := make([]WindowsACLEntry, 0, len(entries))
	for _, entry := range dedupeWindowsACLEntries(entries) {
		entry.Path = windowsCapabilityPathKey(entry.Path)
		entry.Capability = strings.ToLower(strings.TrimSpace(entry.Capability))
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool {
		left := out[i]
		right := out[j]
		if left.Action != right.Action {
			return left.Action < right.Action
		}
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		if left.Capability != right.Capability {
			return left.Capability < right.Capability
		}
		return !left.Materialize && right.Materialize
	})
	return out
}

// windowsSandboxRuntimeCandidates returns every runtime root setup provisions.
//
// BOTH candidates, not the one this process would select. sandboxRuntimeRootFor
// prefers the cache-derived root and falls back to the temp-derived one when the
// first would land inside the workspace or its lease cannot be taken, and that
// choice is made per process. Setup that granted only its own choice left the
// other unprovisioned, so a command that fell back wrote to a tree with no ACE
// on it. Both are deterministic now, so setup can cover both and command
// selection lands on a provisioned root either way.
func windowsSandboxRuntimeCandidates(workspaceRoots []string) []string {
	workspaceRoot := ""
	for _, candidate := range workspaceRoots {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			workspaceRoot = canonicalSandboxWorkspaceRoot(trimmed)
			break
		}
	}
	if workspaceRoot == "" || workspaceRoot == "." {
		return nil
	}
	var roots []string
	if cacheRoot, err := sandboxUserCacheDir(); err == nil {
		if cacheRoot = canonicalSandboxWorkspaceRoot(cacheRoot); cacheRoot != "" && cacheRoot != "." {
			if root, ok := deterministicSandboxRuntimeRoot(workspaceRoot, cacheRoot); ok {
				roots = append(roots, root)
			}
		}
	}
	if root, err := fallbackSandboxRuntimeRoot(workspaceRoot); err == nil {
		roots = append(roots, root)
	}
	return roots
}

// windowsSandboxProfileWithRuntime adds the runtime candidates as write roots.
//
// Applied on BOTH sides of the setup protocol, which is the whole point. The
// marker fingerprints the capability ACL plan built from this profile, while
// every command reaches the Windows runner having already had
// permissionProfileWithRuntime append the root it selected. Setup fingerprinted
// the bare profile and the command presented an augmented one, so a marker
// written seconds earlier was rejected with "permission roots or deny lists
// changed" and no command could run at all.
//
// Adding the full candidate set on both sides makes the two hashes agree without
// the command having to know which root setup happened to pick, and it puts the
// runtime roots into the CAPABILITY plan as well. That second part matters since
// the principal command runs on a WRITE_RESTRICTED token restricted to the
// capability SIDs: a runtime root carrying only the principal ACE satisfies the
// normal token and fails the restricted check, so cache and temp writes were
// denied even once the marker agreed.
func windowsSandboxProfileWithRuntime(profile PermissionProfile, workspaceRoots []string) PermissionProfile {
	candidates := windowsSandboxRuntimeCandidates(workspaceRoots)
	if len(candidates) == 0 {
		return profile
	}
	existing := make(map[string]struct{}, len(profile.FileSystem.WriteRoots))
	for _, root := range profile.FileSystem.WriteRoots {
		existing[windowsCapabilityPathKey(root.Root)] = struct{}{}
	}
	writeRoots := append([]WritableRoot{}, profile.FileSystem.WriteRoots...)
	for _, candidate := range candidates {
		if _, ok := existing[windowsCapabilityPathKey(candidate)]; ok {
			continue
		}
		existing[windowsCapabilityPathKey(candidate)] = struct{}{}
		writeRoots = append(writeRoots, WritableRoot{Root: candidate})
	}
	profile.FileSystem.WriteRoots = writeRoots
	return profile
}

// ensureWindowsSandboxRuntimeCandidates creates every runtime root setup grants.
//
// Paired with windowsSandboxProfileWithRuntime: that function puts the candidates
// into the ACL plan, and this one makes them exist. Splitting the two is what
// broke elevated setup once already, because the capability plan refuses to
// materialize a write root and fails the whole run on a path that is merely
// absent. Whenever one of these grows a candidate, so must the other.
//
// Called on the setup side only. A command must never create these: setup is the
// gate that decides which trees the sandbox may write to, and a command that
// created its own root would be granting itself one.
func ensureWindowsSandboxRuntimeCandidates(workspaceRoots []string) error {
	for _, root := range windowsSandboxRuntimeCandidates(workspaceRoots) {
		if err := ensureRuntimeCandidateDir(root); err != nil {
			return fmt.Errorf("create sandbox runtime root %s: %w", root, err)
		}
	}
	return nil
}

// ensureRuntimeCandidateDir creates one candidate without following a reparse
// point into anything Zero does not own.
//
// VALIDATION BEFORE THE PRIVILEGED MUTATION, NOT AFTER IT. This runs as
// Administrator, and os.MkdirAll follows links: the invoking user can prepare
// either candidate's ancestry, so a junction planted at a not-yet-created
// component below the cache directory, or at the fallback anchor, made elevated
// setup create the tree beneath a redirected, Administrator-writable target. The
// ACL applier's own no-follow check runs later and correctly spots the
// redirection, but by then the privileged create has already happened, outside
// the applier's rollback boundary. A leaf check after MkdirAll would leave the
// same gap.
//
// EnsurePrivateDir is the descent that does not have it: handle-relative,
// no-follow, refusing any component that is a link and any leaf this user does
// not own. It is given a PHYSICAL base first, because it walks from the volume
// root and a redirected cache or TEMP above the owned tail is the operator's
// business, not ours -- the same pairing the fallback anchor uses, and the same
// mistake that refused every fallback on macOS when the parent was left
// unresolved.
// ensureMissingWindowsSandboxRuntimeCandidates creates, with the same ownership
// and no-follow checks setup uses, every runtime candidate that does not exist
// yet, and leaves an existing one exactly as it is. The unelevated tier calls
// this on every command, and an existing candidate carries the capability ACE
// the previous command applied: re-running EnsurePrivateDir on it would strip
// that grant, fail the marker's capability verification, and reapply the plan
// on every launch. An existing candidate is still validated by the plan apply,
// which opens it without following a reparse point.
func ensureMissingWindowsSandboxRuntimeCandidates(workspaceRoots []string) error {
	for _, root := range windowsSandboxRuntimeCandidates(workspaceRoots) {
		info, err := os.Lstat(root)
		switch {
		case err == nil:
			// Left alone, but not unexamined: a file or a reparse point at a
			// candidate's name is not a runtime root, and the apply would happily
			// put an ACE on either. Setup's full validation refuses both shapes
			// too; this is the same answer without the DACL rewrite.
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeIrregular != 0 {
				return fmt.Errorf("sandbox runtime root %s exists but is not a plain directory", root)
			}
			continue
		case errors.Is(err, os.ErrNotExist):
			if err := ensureRuntimeCandidateDir(root); err != nil {
				return fmt.Errorf("create sandbox runtime root %s: %w", root, err)
			}
		default:
			return fmt.Errorf("inspect sandbox runtime root %s: %w", root, err)
		}
	}
	return nil
}

func ensureRuntimeCandidateDir(root string) error {
	base, ok := runtimeCandidateBase(root)
	if !ok {
		return fmt.Errorf("candidate %s does not sit beneath a known base, so its trust boundary cannot be established", root)
	}
	tail, err := filepath.Rel(base, root)
	if err != nil {
		return fmt.Errorf("locate %s beneath %s: %w", root, base, err)
	}
	// A resolve failure keeps the unresolved base deliberately: EnsurePrivateDir
	// is still the fail-closed check, and refusing there names the real component
	// rather than hiding it behind a resolver error.
	physical := base
	if resolved, resolveErr := physicalDir(base); resolveErr == nil && resolved != "" {
		physical = resolved
	}
	return peermsg.EnsurePrivateDir(filepath.Join(physical, tail))
}

// runtimeCandidateBase returns the ancestor of a candidate that belongs to the
// operator rather than to Zero. Everything below it was created by us and has
// no business being a link.
func runtimeCandidateBase(root string) (string, bool) {
	// The fallback candidate is <temp>/zero-runtime-<tag>/v1/<digest>, and the
	// anchor's own parent is the operator's temp directory.
	if isFallbackRuntimeRoot(root) {
		return filepath.Dir(fallbackRuntimeAnchor()), true
	}
	// The cache candidate is <cacheRoot>/zero/runtime/v1/<digest>.
	if cacheRoot, err := sandboxUserCacheDir(); err == nil {
		if cleaned := canonicalSandboxWorkspaceRoot(cacheRoot); cleaned != "" && cleaned != "." && pathWithinRoot(cleaned, root) {
			return cleaned, true
		}
	}
	return "", false
}

// shortWindowsACLPlanHash trims a plan hash for a human-facing error. Twelve hex
// characters is plenty to tell two plans apart by eye, and the full 64 buries the
// rest of the message.
func shortWindowsACLPlanHash(hash string) string {
	hash = strings.TrimSpace(hash)
	if hash == "" {
		return "(none)"
	}
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

// WindowsSandboxProfileWithRuntimeRoots folds the sandbox runtime roots into a
// permission profile, for callers that build a setup config outside this package.
//
// Call it ONLY from a process whose TEMP and TMP are the operator's. The
// temp-derived candidate reads os.TempDir(), and the sandbox points those
// variables at its own runtime temp for anything it launches, so a process on the
// far side of that redirection derives a path no other process agrees on. The
// command runner is exactly such a process: it takes the profile it is handed.
func WindowsSandboxProfileWithRuntimeRoots(profile PermissionProfile, workspaceRoots []string) PermissionProfile {
	return windowsSandboxProfileWithRuntime(profile, workspaceRoots)
}

// WindowsSandboxPrincipalRoleForNetwork names the principal a command in this
// network mode runs as.
//
// Exported and portable so `zero doctor` and the Windows runtime answer from ONE
// rule. The helper this replaces existed for that reason and said so, and the
// thing it was protecting against happened anyway once dual-role provisioning
// changed the behaviour under NetworkDeny: the runtime started using an offline
// principal while doctor still reported the old restricted-token standdown, so
// operators were told their reads were unconfined when they were confined.
// windowsSandboxRoleForNetwork delegates here rather than repeating the switch.
//
// Anything that is not an EXACT allow is offline: a mode this does not recognise
// should lose the network, not keep it.
//
// Deliberately not normalized first. NormalizeNetworkMode case-folds, so routing
// through it made "ALLOW" select the online principal where the runtime required
// an exact match and failed closed to offline. Sharing one rule is only an
// improvement if it shares the STRICTER one; a shared rule that quietly widened
// the runtime would be worse than the drift it fixes.
func WindowsSandboxPrincipalRoleForNetwork(network NetworkMode) string {
	if network == NetworkAllow {
		return "online"
	}
	return "offline"
}

// errWindowsPrincipalLaunchUnavailable is why provisioning refuses.
//
// Stated as one sentence about the MECHANISM rather than about the caller's
// token, because the caller's token was never the question: the process that
// needs the launch privileges is a later, ordinary one, and no supported path
// carries launch authority to it. A message about the current process invited
// the operator to re-run from an elevated terminal, which passes such a check
// and still cannot run a command.
var errWindowsPrincipalLaunchUnavailable = errors.New(
	"launching a command as a separate account needs SeAssignPrimaryTokenPrivilege and SeIncreaseQuotaPrivilege, " +
		"which an ordinary command process does not hold; elevated setup cannot grant them to it, and no bootstrap " +
		"or broker exists yet to carry that authority across. The restricted-token sandbox is unaffected: unset " +
		windowsSandboxIdentityEnv + " to use it",
)

// windowsPrincipalLaunchAvailable reports whether a provisioned principal could
// actually be launched by the process that will run the command.
//
// ONE SEAM, DEFAULTING TO UNAVAILABLE. The answer does not depend on the token
// of whoever calls setup, so this takes no argument: it is a statement about
// which mechanisms exist, not about the current process. When a bootstrap or
// broker lands, this is the single place that learns to say yes, and the plans,
// ledger and ACL work below it are already built and tested against it.
//
// It stays a var so the argument-plumbing tests can exercise the opt-in path
// without depending on a capability no machine currently has.
var windowsPrincipalLaunchAvailable = func() error {
	return errWindowsPrincipalLaunchUnavailable
}
