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
	"runtime"
	"sort"
	"strings"
)

const WindowsSandboxSetupName = "zero-windows-sandbox-setup.exe"

// Bumped to 6 when the SELECTED RUNTIME ROOT itself became part of the marker,
// so a command consumes setup's choice instead of re-deriving one. A marker
// written by the previous version records no root, and honouring it would pin
// commands to whatever this build happens to select first.
//
// Bumped to 5 when setup began recording the CONCRETE runtime root it
// provisioned, and stamping that tree, instead of fingerprinting a plan built
// from a root it merely derived. A marker written by the previous version has no
// stamp, and requiring one without a bump would report every already-set-up
// machine as broken rather than as out of date. Bumping says the true thing: the
// setup protocol changed, run it once more.
const windowsSandboxSetupMarkerSchemaVersion = 6

type WindowsSandboxSetupArgsOptions struct {
	SandboxHome       string
	CommandCWD        string
	WorkspaceRoots    []string
	PermissionProfile PermissionProfile
}

type WindowsSandboxSetupConfig struct {
	SandboxHome       string
	CommandCWD        string
	WorkspaceRoots    []string
	PermissionProfile PermissionProfile
	// ConsumerSID is the ordinary token that will READ the setup stamp after
	// elevation returns. Resolved in the operator's shell and carried across the
	// elevation boundary, never inferred here: the elevated helper creates the
	// runtime leaf when it is absent, so that leaf's owner describes the
	// installer rather than the consumer.
	ConsumerSID string
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
	// RuntimeRoot is the runtime tree setup ACTUALLY PROVISIONED, recorded rather
	// than re-derived.
	//
	// Selection consults a lease, and a lease is a fact about one moment. Setup
	// took the lease only to learn which root won and released it immediately, so
	// a command ran the same selector later and was free to reach a different
	// answer: setup relocating to the temp fallback while the cache root was
	// briefly unavailable, then a command taking the cache root once it freed up.
	// Two selections, two roots, and a marker that can never validate again --
	// re-running setup does not help, because setup is equally free to pick the
	// other one.
	//
	// Recording the choice removes the disagreement instead of trying to make two
	// independent selections agree. See pinnedSandboxRuntimeRoot for the consuming
	// side.
	RuntimeRoot string `json:"runtimeRoot,omitempty"`
}

func WindowsSandboxSetupMarkerPath(sandboxHome string) string {
	return filepath.Join(sandboxHome, "windows-setup.json")
}

// setupConsumerSID is the process SID lookup, behind a variable because the
// interval between creating the runtime tree and returning the args is the whole
// reason the rollback exists, and this is the only failure that can happen in it.
// Without a seam here that interval is unreachable from a test, and an untestable
// compensation path is one nobody finds out is broken.
var setupConsumerSID = currentProcessSID

// WindowsSandboxSetupPlan is the helper invocation plus the undo for what
// building it created.
//
// THE TRANSACTION STARTS AT THE FIRST MUTATING LEASE OPERATION, not when the
// helper starts. Selecting the runtime root takes a lease, and taking a lease
// creates zero/runtime/v1 and the lease file when they are not there. Those are
// this invocation's writes, made in this process, before the helper exists. The
// helper reacquires an already-existing tree and records no creation, and
// provisioning records only the leaf, so nothing downstream could undo them.
//
// Rollback is safe to call once the caller knows setup did not complete, and
// must not be called after it did: the tree it removes is the tree the marker
// now attests to.
type WindowsSandboxSetupPlan struct {
	Args []string
	// Rollback removes all and only what building these args created, in reverse
	// creation order, and reports what it could not remove rather than claiming a
	// clean undo. Never nil.
	Rollback func() error
}

// BuildWindowsSandboxSetupArgs prepares the elevated helper invocation.
//
// It returns a plan rather than bare args so the creation ledger cannot be
// dropped by a caller that only wanted the command line. That is exactly what
// happened while the lease helper had a recording form nobody in production
// called.
func BuildWindowsSandboxSetupArgs(options WindowsSandboxSetupArgsOptions) (WindowsSandboxSetupPlan, error) {
	commandCWD := strings.TrimSpace(options.CommandCWD)
	if commandCWD == "" {
		return WindowsSandboxSetupPlan{}, errors.New("windows sandbox setup requires command cwd")
	}
	sandboxHome := strings.TrimSpace(options.SandboxHome)
	if sandboxHome == "" {
		var err error
		sandboxHome, err = ResolveWindowsSandboxHome(nil)
		if err != nil {
			return WindowsSandboxSetupPlan{}, err
		}
	}
	workspaceRoots := trimNonEmptyStrings(options.WorkspaceRoots)
	if len(workspaceRoots) == 0 {
		workspaceRoots = []string{commandCWD}
	}
	// SELECTED, NOT DERIVED, and selected here in the operator's shell because a
	// command runs in that same environment and will reach the same answer.
	//
	// This used to derive the cache-based root and fingerprint a plan naming it.
	// A command derived the same root, failed to LEASE it, and silently relocated
	// to the temp fallback, so its plan named a tree setup had never provisioned
	// and the marker rejected the command with "permission roots or deny lists
	// changed" -- which blames permissions for a runtime-root disagreement.
	// Re-running setup could not recover, because setup chose the same unleasable
	// root again. That is a permanent brick rather than a retry.
	//
	// selectSandboxRuntimeRoot is the function commands use, lease attempt and
	// fallback included, so setup provisions the tree a command will actually
	// select. The lease is released straight away: it is taken here only to learn
	// which root wins, and the command acquires its own.
	//
	// A SELECTION FAILURE IS FATAL HERE, and continuing was the bug.
	//
	// Continuing left the profile with no runtime root while setup went on to
	// provision a derived tree, apply the capability ACLs and write the marker.
	// That marker records an empty runtime root, so it attests nothing about the
	// tree. A later command makes its own concrete selection, finds no stamp on
	// what it selected and refuses, and re-running setup reaches this same branch
	// and records nothing again: the permanent brick described above, reached by
	// the one path that was allowed to skip the fix.
	//
	// Failing now is failing before any ACL or marker state is persisted, so the
	// operator is left in a state a retry can get out of, and the message names
	// the step that actually failed.
	selected, lease, created, selectErr := selectSandboxRuntimeRoot(firstNonEmpty(workspaceRoots...), false, sandboxHome)
	// UNDO BEFORE RETURNING, because a selection that failed part-way still made
	// the components it got through. The old shape returned the error and left
	// them, and nothing later had a record they existed.
	if selectErr != nil {
		undo := windowsRuntimeRootRollback{created: created}.run()
		selectErr = fmt.Errorf("select the sandbox runtime root for setup: %w", selectErr)
		if undo != nil {
			selectErr = fmt.Errorf("%w; and the partial runtime tree could not be removed: %w", selectErr, undo)
		}
		return WindowsSandboxSetupPlan{}, selectErr
	}
	// The lease file is one of this invocation's artifacts when this acquisition
	// created it, and rollback refuses a non-empty directory, so the undo has to
	// own it or v1 can never be removed. It is released first: compensation
	// retakes it exclusively, which is what proves no other process is using the
	// tree it is about to delete.
	leasePath, ownsLease := lease.createdLeaseFile()
	lease.release()
	rollback := func() error {
		return undoWindowsSetupRuntimeCreation(selected, created, leasePath, ownsLease)
	}
	failed := func(err error) (WindowsSandboxSetupPlan, error) {
		if undo := rollback(); undo != nil {
			return WindowsSandboxSetupPlan{}, fmt.Errorf("%w; and the partial runtime tree could not be removed: %w", err, undo)
		}
		return WindowsSandboxSetupPlan{}, err
	}
	options.PermissionProfile = permissionProfileWithRuntime(options.PermissionProfile, SandboxRuntime{Root: selected})
	options.PermissionProfile = WindowsSandboxProfileWithRuntimeRoots(options.PermissionProfile, workspaceRoots)
	profileJSON, err := json.Marshal(options.PermissionProfile)
	if err != nil {
		return failed(fmt.Errorf("marshal windows sandbox setup permission profile: %w", err))
	}
	// RESOLVED HERE, IN THE SAME PROCESS, and that is the whole supported model.
	//
	// Zero never elevates: the operator elevates the terminal, and runSandboxSetup
	// launches the helper with a plain exec.Command. So this runs in a process that
	// is already elevated and this SID is the installer's. It is also the consumer's
	// only because the documented contract is an elevated terminal belonging to the
	// account that will run Zero, which same-account UAC satisfies. The helper
	// re-checks that the carried identity matches its own token and refuses
	// otherwise, so the unsupported alternate-account shape fails loudly instead of
	// provisioning a stamp for a token that can never read it. It creates the runtime leaf when it is missing, so a
	// reader derived from that leaf is BUILTINAdministrators, and a later
	// UAC-filtered administrator carries that group deny-only while a standard
	// user given alternate admin credentials is not in it at all. Either way the
	// protected stamp ends up with no enabled allow ACE for the token that has to
	// validate it, and every restricted command stops before launch.
	consumerSID, sidErr := setupConsumerSID()
	if sidErr != nil {
		return failed(sidErr)
	}
	args := []string{
		"--sandbox-home", sandboxHome,
		"--command-cwd", commandCWD,
		"--permission-profile", string(profileJSON),
	}
	if consumerSID != "" {
		args = append(args, "--consumer-sid", consumerSID)
	}
	for _, root := range workspaceRoots {
		args = append(args, "--workspace-root", root)
	}
	return WindowsSandboxSetupPlan{Args: args, Rollback: rollback}, nil
}

// undoWindowsSetupRuntimeCreation removes what building the setup args created.
//
// EXCLUSIVITY IS HELD ACROSS EVERY MUTATION THAT DEPENDS ON IT, not just taken
// and handed back. Taking the lease exclusively proves no command is holding the
// runtime root this is about to delete, and that proof has to still be true when
// the last directory goes, not only when the first one does.
//
// Two ways the old shape broke that. It released the lease before removing the
// lease pathname and before compensating the tree, so a command blocked on the
// shared lease was let in during the removal: it could take the old lease object
// while cleanup unlinked the name out from under it (the lease is opened
// FILE_SHARE_DELETE, and the comment claiming Windows forbids that delete was
// simply wrong), after which a later cleanup could lock a different object at the
// same pathname and conclude the root was free. And an acquisition that came back
// in-use said the tree was "left in place" and then removed it anyway on the very
// next statement, which is the outcome the message exists to promise never
// happens.
//
// So: acquire, mutate, release, in that order, and mutate nothing at all unless
// the acquisition succeeded. Leaving a tree behind is recoverable by the next
// run; deleting a live one is not.
func undoWindowsSetupRuntimeCreation(root string, created []windowsCreatedRuntimeDir, leasePath string, ownsLease bool) error {
	if !ownsLease {
		return windowsRuntimeRootRollback{created: created}.run()
	}
	lease, inUse, err := tryAcquireSandboxRuntimeCleanupLease(root)
	switch {
	case err != nil:
		return fmt.Errorf("take the sandbox runtime lease before removing it: %w; %s and the tree it protects were left in place", err, leasePath)
	case inUse:
		return fmt.Errorf("the sandbox runtime root %s is in use by another process, so %s and the tree it protects were left in place", root, leasePath)
	}
	defer lease.release()
	if runtimeCleanupExclusivityBarrier != nil {
		runtimeCleanupExclusivityBarrier()
	}
	return windowsRuntimeRootRollback{created: created, leasePath: leasePath}.run()
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
		case "--consumer-sid":
			value, next, err := nextWindowsSandboxFlagValue(args, index)
			if err != nil {
				return WindowsSandboxSetupConfig{}, err
			}
			config.ConsumerSID = strings.TrimSpace(value)
			index = next
		case "--permission-profile":
			value, next, err := nextWindowsSandboxFlagValue(args, index)
			if err != nil {
				return WindowsSandboxSetupConfig{}, err
			}
			profileJSON = strings.TrimSpace(value)
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

func (config WindowsSandboxSetupConfig) commandConfig() WindowsSandboxCommandConfig {
	return WindowsSandboxCommandConfig{
		SandboxHome:       config.SandboxHome,
		CommandCWD:        config.CommandCWD,
		WorkspaceRoots:    cloneStrings(config.WorkspaceRoots),
		PermissionProfile: config.PermissionProfile,
		SandboxLevel:      WindowsSandboxLevelRestrictedToken,
	}
}

func WindowsSandboxSetupConfigFromCommand(config WindowsSandboxCommandConfig) WindowsSandboxSetupConfig {
	return WindowsSandboxSetupConfig{
		SandboxHome:       config.SandboxHome,
		CommandCWD:        config.CommandCWD,
		WorkspaceRoots:    cloneStrings(config.WorkspaceRoots),
		PermissionProfile: config.PermissionProfile,
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
	return WindowsSandboxSetupMarker{
		SchemaVersion:    windowsSandboxSetupMarkerSchemaVersion,
		ACLPlanHash:      hash,
		ACLPlanEntries:   len(plan.Entries),
		NetworkInfraHash: infraHash,
		OfflineFilterSID: offlineSID,
		NetworkFilters:   len(infraPlan.Filters),
		RuntimeRoot:      windowsSandboxSelectedRuntimeRoot(config.PermissionProfile),
	}, nil
}

// WriteWindowsSandboxSetupMarker builds the marker, stamps the runtime tree and
// records the marker file.
//
// The elevated setup path does NOT use this. It splits the two, because the
// stamp has to ride along with the capability ACE through one handle rather than
// re-open the runtime root by name afterwards. See windowsACLStampRequest. This
// entry point remains for callers that record a marker without applying an ACL
// plan, where there is no handle to ride.
func WriteWindowsSandboxSetupMarker(config WindowsSandboxSetupConfig) (written WindowsSandboxSetupMarker, err error) {
	marker, err := BuildWindowsSandboxSetupMarker(config)
	if err != nil {
		return WindowsSandboxSetupMarker{}, err
	}
	// Every writer of the stamp and marker pair holds the same lock, this one
	// included, so it cannot land between the two halves of an elevated setup.
	unlock, err := lockWindowsSandboxSetup(config.SandboxHome)
	if err != nil {
		return WindowsSandboxSetupMarker{}, err
	}
	defer func() {
		if releaseErr := unlock(); releaseErr != nil {
			written, err = WindowsSandboxSetupMarker{}, errors.Join(err, releaseErr)
		}
	}()
	// Stamped alongside the marker, because this is the one place setup records
	// that it completed and the two have to be recorded together: a marker whose
	// stamp is missing reports setup as current when the tree it provisioned is
	// gone, which is the whole failure. Written FIRST so a marker never outlives
	// its stamp if the process dies between the two.
	if err := writeWindowsSandboxRuntimeStamp(windowsSandboxSelectedRuntimeRoot(config.PermissionProfile), marker.ACLPlanHash); err != nil {
		return WindowsSandboxSetupMarker{}, err
	}
	if err := writeWindowsSandboxSetupMarkerFile(config, marker); err != nil {
		return WindowsSandboxSetupMarker{}, err
	}
	return marker, nil
}

// writeWindowsSandboxSetupMarkerFile records an already-built marker and touches
// nothing else. It never names the runtime tree.
func writeWindowsSandboxSetupMarkerFile(config WindowsSandboxSetupConfig, marker WindowsSandboxSetupMarker) error {
	path := WindowsSandboxSetupMarkerPath(config.SandboxHome)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create windows sandbox setup marker dir: %w", err)
	}
	bytes, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal windows sandbox setup marker: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".windows-setup-*.tmp")
	if err != nil {
		return fmt.Errorf("create windows sandbox setup marker temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(bytes); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write windows sandbox setup marker temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close windows sandbox setup marker temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace windows sandbox setup marker: %w", err)
	}
	return nil
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
	if actual.ACLPlanHash != expected.ACLPlanHash || actual.ACLPlanEntries != expected.ACLPlanEntries {
		// Name both sides. This message fires when setup and the command derived
		// different runtime roots, and without the hashes the operator cannot tell
		// that case apart from a genuine policy edit.
		return fmt.Errorf("windows sandbox setup is out of date: permission roots or deny lists changed (marker plan %s, %d entries; this command wants %s, %d entries)",
			shortWindowsACLPlanHash(actual.ACLPlanHash), actual.ACLPlanEntries,
			shortWindowsACLPlanHash(expected.ACLPlanHash), expected.ACLPlanEntries)
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
	// THE PLAN HASH IS ABOUT PATHNAMES, NOT ABOUT OBJECTS. Everything above
	// compares what setup INTENDED with what this command wants, and both sides
	// agree as long as the same paths are named. Whether the directory those paths
	// resolve to is still the one setup provisioned is a different question, and
	// nothing here was asking it: cleanupSandboxRuntimeRoots evicts inactive roots
	// and the next run recreates the pathname with ordinary permissions, so the
	// hashes still matched while the capability ACE was gone and a
	// WRITE_RESTRICTED token could not write anything under it.
	if err := validateWindowsSandboxRuntimeStamp(config.PermissionProfile, expected.ACLPlanHash); err != nil {
		return err
	}
	if actual.NetworkFilters != expected.NetworkFilters {
		return errors.New("windows sandbox setup is out of date: network enforcement plan changed")
	}
	// Named explicitly, and last, because the checks above cannot tell this case
	// apart from a policy edit. A runtime-root disagreement used to surface as
	// "permission roots or deny lists changed", which sends the operator looking
	// at permissions for a problem that is nothing to do with them.
	//
	// With the root recorded this should not be reachable, since the command
	// consumes what setup wrote. It stays as the assertion that the contract held.
	if recorded := strings.TrimSpace(actual.RuntimeRoot); recorded != "" {
		if selected := strings.TrimSpace(expected.RuntimeRoot); selected != "" && !sameWindowsRuntimeRootPath(recorded, selected) {
			return fmt.Errorf("windows sandbox setup is out of date: setup provisioned runtime root %s, this command selected %s -- run `zero sandbox setup` from an elevated (Administrator) terminal", recorded, selected)
		}
	}
	return nil
}

// sameWindowsRuntimeRootPath compares two runtime roots the way the filesystem
// does on this platform. Windows paths are case-insensitive, and the recorded
// root and the selected root can differ only in spelling.
func sameWindowsRuntimeRootPath(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

// WindowsSandboxRecordedRuntimeRoot returns the runtime root a previous setup
// provisioned, or "" when there is no usable marker.
//
// Deliberately silent on every failure. A missing, unreadable or malformed
// marker means there is nothing to honour, and the caller's job is to select
// normally rather than to report on marker health -- validation does that, with
// a much better message than a selector could produce.
func WindowsSandboxRecordedRuntimeRoot(sandboxHome string) string {
	sandboxHome = strings.TrimSpace(sandboxHome)
	if sandboxHome == "" {
		return ""
	}
	bytes, err := os.ReadFile(WindowsSandboxSetupMarkerPath(sandboxHome))
	if err != nil {
		return ""
	}
	var marker WindowsSandboxSetupMarker
	if err := json.Unmarshal(bytes, &marker); err != nil {
		return ""
	}
	// A root recorded by an older schema describes a tree provisioned under
	// different rules, so it is not a root this build may pin to.
	if marker.SchemaVersion != windowsSandboxSetupMarkerSchemaVersion {
		return ""
	}
	return strings.TrimSpace(marker.RuntimeRoot)
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

// windowsSandboxRuntimeRoots returns the runtime root the plan must name.
//
// PINNED to the profile's own runtime when it has one, rather than derived a
// second time. A command's profile has already been through
// permissionProfileWithRuntime, so its runtime tree is the one this process
// chose, created and took a lease on. Asking a separate function to work out
// which tree that "should" be is how the plan comes to name one directory while
// the command writes to another, which is the whole of issue #881.
//
// Deriving BOTH candidates was the previous answer to that problem. It bought
// agreement at the price of putting os.TempDir() into a machine-wide
// fingerprint: setup run under one TEMP recorded a plan that a later parent
// process with a different TEMP could not reproduce, so every command failed the
// equality check even though the cache runtime was untouched and healthy.
// Pinning removes the second derivation instead of trying to keep two in step.
//
// The derive branch below serves callers that have no runtime yet (elevated
// setup, doctor) and goes through sandboxRuntimeRootFor, THE selector
// prepareSandboxRuntime uses, so the two cannot drift.
//
// The FIRST root only, and that is deliberate rather than an oversight. The
// marker compares plan hashes for EQUALITY, and a command presents exactly one
// workspace root, so setup has to derive its candidates from the same single root
// the command will. Deriving them for every root instead would put candidates in
// the marker that no single command reproduces, and every command would fail with
// the same "permission roots or deny lists changed" this pairing exists to fix.
// Nothing passes more than one root today: every construction site is a
// one-element slice. Whoever adds multi-root support has to change the marker to a
// per-root or subset comparison FIRST; widening this function on its own would
// reintroduce the outage.
func windowsSandboxRuntimeRoots(profile PermissionProfile, workspaceRoots []string) []string {
	// The pin. Nothing is derived when the profile already carries the answer,
	// including after prepareSandboxRuntime relocated on a lease failure: the plan
	// names whatever tree the command actually holds.
	if profile.Runtime != nil {
		if root := strings.TrimSpace(profile.Runtime.Root); root != "" {
			return []string{root}
		}
	}
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
	cacheRoot, err := sandboxUserCacheDir()
	if err != nil {
		return nil
	}
	// Canonicalized exactly as prepareSandboxRuntime canonicalizes it, because
	// sandboxRuntimeRootFor compares this against the workspace root to decide
	// whether the cache-derived tree lands inside it.
	if cacheRoot = canonicalSandboxWorkspaceRoot(cacheRoot); cacheRoot == "" || cacheRoot == "." {
		return nil
	}
	root, err := sandboxRuntimeRootFor(workspaceRoot, cacheRoot)
	if err != nil {
		return nil
	}
	return []string{root}
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
// Naming the SAME root on both sides makes the two hashes agree, and it puts the
// runtime root into the CAPABILITY plan as well. That second part matters since
// the principal command runs on a WRITE_RESTRICTED token restricted to the
// capability SIDs: a runtime root carrying only the principal ACE satisfies the
// normal token and fails the restricted check, so cache and temp writes were
// denied even once the marker agreed.
func windowsSandboxProfileWithRuntime(profile PermissionProfile, workspaceRoots []string) PermissionProfile {
	candidates := windowsSandboxRuntimeRoots(profile, workspaceRoots)
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

// ensureWindowsSandboxRuntimeRoots creates every runtime root the plan grants.
//
// Paired with windowsSandboxProfileWithRuntime: that function puts the root into
// the ACL plan, and this one makes it exist. Splitting the two is what broke
// elevated setup once already, because the capability plan refuses to
// materialize a write root and fails the whole run on a path that is merely
// absent. Both now go through windowsSandboxRuntimeRoots, so they cannot name
// different trees.
//
// Called by WHOEVER APPLIES THE PLAN, which is both tiers rather than only the
// elevated one. Setup applies it under Administrator; the unelevated tier applies
// its own workspace ACLs per command by design, since capability grants on trees
// the user already owns need no privilege. A command creating a runtime root under
// its own cache or temp grants itself nothing it could not create anyway, and the
// tier that skips this is the tier that dies on "windows ACL target does not
// exist".
// windowsRuntimeRootRollback removes the runtime directories one provisioning
// call actually created, and only those.
//
// Setup materializes runtime roots before the network plan, the ACL apply, the
// network apply and the marker write. Every one of those can fail, and the
// existing rollback only restored ACL snapshots, so a failed `zero sandbox
// setup` reported failure and left new persistent state behind. It could not
// clean up even in principle, because provisioning returned nothing about what
// it had made.
type windowsRuntimeRootRollback struct {
	// created is in creation order, outermost first, so undo walks it backwards.
	//
	// Identity-bound, not pathname-bound. Compensation runs after the apply
	// handles have closed, so resolving these names again can reach a different
	// object: rename the original aside, drop an ordinary directory in its place,
	// and a pathname-only undo removes the substitute while the original keeps
	// this run's grant and stamp.
	created []windowsCreatedRuntimeDir
	// leasePath is the lease file this invocation created, or "" when it did not
	// create one and has no business removing it.
	//
	// Removed DURING the walk rather than before it, immediately ahead of the
	// directory that contains it, because the two orderings each break something.
	// Removing it first ends the exclusion the whole cleanup depends on: the name
	// disappears at once, and the next process to want the root creates a fresh
	// lease file at that name and enters a tree this walk is still deleting.
	// Removing it last leaves its parent non-empty, so the parent can never be
	// compensated. Between those, the latest possible moment is just before the
	// parent is removed.
	leasePath string
	// stamp is the runtime stamp's state before this run touched it.
	//
	// The stamp is the one artifact setup writes INSIDE the runtime root, and it
	// is written before the marker. A marker write that failed therefore left a
	// root this run had created holding a file this run had written, and the
	// rollback below refuses a non-empty directory on purpose, so the failed setup
	// kept its own residue forever. Owning the stamp is what makes the root empty
	// again and the whole transaction complete.
	stamp windowsSandboxStampSnapshot
}

// windowsCreatedRuntimeDir is one directory this run made, remembered by the
// object it was rather than by the name it had.
type windowsCreatedRuntimeDir struct {
	path     string
	identity string
	// identified separates "no identity" from "identity not established", for the
	// same reason as the stamp snapshot above.
	identified bool
}

// windowsSandboxStampSnapshot records the runtime stamp as it was before setup
// overwrote it, so a failed run restores rather than deletes.
//
// Restoring matters where a previous setup had succeeded. Deleting the stamp
// would leave that machine's still-valid marker pointing at a tree with no
// stamp, which reads as "the runtime directory was removed since setup ran" --
// a healthy machine reporting itself broken because an unrelated later setup
// failed.
// runtimeStampState is what the snapshot could actually establish about the
// stamp that was there BEFORE this run.
//
// Two booleans could not say it. "Read it, and there was nothing" and "could not
// read it" both arrived as existed=false, so compensation could not tell an undo
// from destruction: it deleted the current stamp and returned immediately with
// nothing to put back, and a setup attempt that REPORTED FAILURE had destroyed
// the attestation belonging to the previous successful setup.
type runtimeStampState int

const (
	// runtimeStampUnknown is the zero value on purpose: a snapshot nobody filled
	// in must never read as proven absence.
	runtimeStampUnknown runtimeStampState = iota
	// runtimeStampAbsent is a POSITIVE observation of nothing there, and only
	// ERROR_FILE_NOT_FOUND or ERROR_PATH_NOT_FOUND produces it.
	runtimeStampAbsent
	// runtimeStampPresent means the prior bytes were read completely.
	runtimeStampPresent
)

type windowsSandboxStampSnapshot struct {
	path string
	// name is the stamp's own file name inside root. It is derived from the plan
	// this setup is applying, so compensation can only ever reach that plan's
	// attestation and never the one another sandbox home left in the same tree.
	name  string
	prior []byte
	// priorState is what was actually observed. See runtimeStampState: an
	// encoding, directory-open, identity, child-open or read failure stays
	// UNKNOWN and must stop the forward mutation rather than authorizing a
	// delete-without-restore later.
	priorState runtimeStampState
	// root and rootIdentity identify the DIRECTORY the stamp lives in, captured
	// when the snapshot was taken. The stamp file itself may not exist yet, so the
	// directory is the object whose replacement this has to detect.
	root         string
	rootIdentity string
	// rootIdentified records whether the identity above was ESTABLISHED, as
	// opposed to being empty because the capture failed. The two were
	// indistinguishable, and restore treated the empty case as permission to
	// mutate the pathname: exactly the case where compensation cannot prove what
	// it is undoing.
	rootIdentified bool
}

// snapshotWindowsSandboxRuntimeStamp captures what compensation will need, and
// FAILS rather than guessing. An error here must stop setup before the ACL and
// stamp are applied: the writer can replace an existing stamp even when the
// earlier read was denied, and compensation would then delete it with nothing
// recorded to restore.
func snapshotWindowsSandboxRuntimeStamp(root string, planHash string) (windowsSandboxStampSnapshot, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return windowsSandboxStampSnapshot{}, nil
	}
	name := windowsSandboxRuntimeStampName(planHash)
	path := windowsSandboxRuntimeStampPath(root, planHash)
	// ONE HANDLE FOR BOTH FACTS. Taking the identity and then re-resolving the
	// pathname to read the stamp let a rename in between pair one directory's
	// identity with another's bytes, so a rollback could verify the right object
	// and then write the wrong contents into it.
	rootIdentity, rootIdentified, prior, state, err := snapshotRuntimeStampBound(root, name)
	if err != nil {
		return windowsSandboxStampSnapshot{}, fmt.Errorf("record the sandbox runtime stamp at %s before changing it: %w", path, err)
	}
	return windowsSandboxStampSnapshot{
		path:           path,
		name:           name,
		prior:          prior,
		priorState:     state,
		root:           root,
		rootIdentity:   rootIdentity,
		rootIdentified: rootIdentified,
	}, nil
}

func (snapshot windowsSandboxStampSnapshot) restore() error {
	if snapshot.path == "" {
		return nil
	}
	// THE NAME IS NOT THE OBJECT ONCE THE APPLY HANDLES HAVE CLOSED. Removing a
	// stamp from, or writing one onto, whatever now answers to this path can
	// mutate a directory this run never touched, while the original keeps the
	// grant and the stamp. Leave the substitute alone and say what was left
	// behind.
	if !snapshot.rootIdentified {
		// AN IDENTITY THAT WAS NEVER ESTABLISHED IS NOT PERMISSION TO MUTATE. This
		// used to fall through to the pathname operations below, so a root that
		// could not be opened when setup began was compensated by writing to, or
		// removing from, whatever answered to the name afterwards.
		//
		// A root that is simply absent is different: there is no object to confuse
		// and nothing of a previous run to put back, and the created-directory
		// rollback owns that case.
		if _, err := os.Lstat(snapshot.root); err == nil {
			return fmt.Errorf("sandbox runtime root %s could not be identified when setup began, so the stamp at %s cannot be shown to belong to this run; leaving it untouched", snapshot.root, snapshot.path)
		}
		return nil
	}
	// AN UNPROVEN PRIOR STATE IS NOT PERMISSION TO DELETE. Absence has to have
	// been observed, not inferred from a generic error: compensation for
	// "existed=false" removes the current stamp and returns with nothing to put
	// back, so reaching here on an unreadable snapshot would destroy the
	// attestation of the previous successful setup on behalf of a run that
	// failed. Setup refuses before the apply for exactly this reason, and this is
	// the second half of that guard for any path that assembled a record anyway.
	if snapshot.priorState == runtimeStampUnknown {
		return fmt.Errorf("the stamp at %s could not be read when setup began, so removing or restoring it now cannot be shown to be an undo; leaving it untouched", snapshot.path)
	}
	// BOUND TO THE OBJECT, not to the name. The identity check and the mutation
	// now share one handle, so a rename and replacement cannot land between them.
	return compensateRuntimeStampBound(snapshot.root, snapshot.rootIdentity, snapshot.name, snapshot.prior, snapshot.priorState == runtimeStampPresent)
}

// run removes what was created, innermost first.
//
// os.Remove rather than os.RemoveAll, deliberately. A directory that is not
// empty by the time we get here holds something this call did not create, and
// removing it would turn a failed setup into data loss. Refusing is the right
// answer: the error is reported and the residue stays findable.
func (rollback windowsRuntimeRootRollback) run() error {
	var errs []error
	// The stamp first, so a root this run created is empty again by the time the
	// directory walk reaches it. EVERY compensation still runs if this one fails:
	// one broken undo must not strand the rest.
	if err := rollback.stamp.restore(); err != nil {
		errs = append(errs, err)
	}
	leaseRemoved := false
	removeLease := func() {
		if rollback.leasePath == "" || leaseRemoved {
			return
		}
		leaseRemoved = true
		if err := os.Remove(rollback.leasePath); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove the sandbox runtime lease %s: %w", rollback.leasePath, err))
		}
	}
	for index := len(rollback.created) - 1; index >= 0; index-- {
		entry := rollback.created[index]
		// The lease sits beside the runtime root, so this is the directory whose
		// removal it would otherwise block. See the field comment for why it goes
		// here and not at either end of the walk.
		if filepath.Clean(entry.path) == filepath.Dir(rollback.leasePath) {
			removeLease()
		}
		// Same rule as the stamp: prove this is the directory we made before
		// removing it. A substitute at the same name belongs to whoever put it
		// there.
		if !entry.identified {
			// Created, but never identified. Removing whatever is here now would be
			// a pathname-authoritative delete of an object this run cannot prove it
			// made.
			errs = append(errs, fmt.Errorf("sandbox runtime root %s was created by this run but could not be identified, so it cannot be removed safely; leaving it in place", entry.path))
			continue
		}
		if err := removeCreatedRuntimeDirBound(entry.path, entry.identity); err != nil {
			errs = append(errs, err)
		}
	}
	// Its parent was already there, so the walk never reached it.
	removeLease()
	return errors.Join(errs...)
}

// runWindowsSandboxSetupCompensations undoes a failed setup COMPLETELY, and
// reports everything that went wrong doing it.
//
// One function because the old code had two failure closures that composed by
// calling each other, and the outer one returned as soon as the ACL rollback
// reported an error. The runtime rollback then never ran, so the failure most
// likely to leave a machine in a strange state was the one failure that skipped
// half the cleanup. Every compensation runs here, unconditionally, and the
// errors are joined rather than raced.
//
// aclRollback is nil before the ACL plan has been applied, which is the only
// difference between the two failure points.
func runWindowsSandboxSetupCompensations(cause error, aclRollback func() error, runtimeRollback windowsRuntimeRootRollback) error {
	errs := []error{cause}
	if aclRollback != nil {
		if err := aclRollback(); err != nil {
			errs = append(errs, fmt.Errorf("acl rollback failed: %w", err))
		}
	}
	if err := runtimeRollback.run(); err != nil {
		errs = append(errs, fmt.Errorf("runtime rollback failed: %w", err))
	}
	return errors.Join(errs...)
}

// createRuntimeDirRecording is MkdirAll that reports which components it made.
//
// The distinction between "created" and "already there" is the whole contract:
// a pre-existing cache or temp ancestor belongs to the user and must survive a
// failed setup, while the components this run added must not.

// refuseReparsedRuntimeAncestors rejects a reparse point at any component Zero
// creates, so an elevated ACL is never written through one.
//
// ONLY THE COMPONENTS WE OWN. The cache root above them is the user's, and on a
// machine with a redirected LOCALAPPDATA it is legitimately a reparse point, so
// refusing there would break ordinary setups. Everything below it is ours, was
// created by us, and has no business being a link.
//
// The check has to cover EVERY owned component, not just the deepest one that
// exists. A junction planted at "zero" with the components below it created by
// the attacker leaves the deepest existing component an ordinary directory, so a
// check that looks only there passes while creation follows the junction and the
// leaf lands in the attacker's tree. openWindowsACLTarget then opens that leaf,
// sees no reparse point on it, and the capability ACL is written outside the
// runtime hierarchy entirely.
//
// os.Lstat reports a junction as ModeIrregular rather than ModeSymlink, which is
// why both are tested: a Windows junction needs no privilege to create, so this
// is reachable by any local user.
func refuseReparsedRuntimeAncestors(root string) error {
	cleaned := filepath.Clean(root)
	owned := make([]string, 0, windowsSandboxRuntimeOwnedDepth)
	current := cleaned
	for range windowsSandboxRuntimeOwnedDepth {
		owned = append(owned, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	for _, component := range owned {
		info, err := os.Lstat(component)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("inspect sandbox runtime component %s: %w", component, err)
		}
		if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return fmt.Errorf("refusing to provision the sandbox runtime through a link at %s: a reparse point here would redirect the directory the sandbox is granted write access to", component)
		}
	}
	return nil
}

func createRuntimeDirRecording(root string) ([]windowsCreatedRuntimeDir, error) {
	if strings.TrimSpace(root) == "" {
		return nil, nil
	}
	// Checked BEFORE anything is created, and again by the caller after, because
	// this alone is a check-then-use: an ancestor swapped between the two would
	// still redirect the leaf. Pairing it with the post-check narrows the window
	// to the creation itself rather than to the whole of setup.
	if err := refuseReparsedRuntimeAncestors(root); err != nil {
		return nil, err
	}
	// THE BASE IS FIXED, NOT DISCOVERED. This used to os.Stat its way to the
	// deepest component that already existed and open THAT by name. Existence then
	// chose the trust boundary: on the ordinary second-workspace shape
	// <base>\zero\runtime\v1 is already there and only the digest is missing, so
	// the single by-name open was v1, one of the predictable components this
	// traversal exists to protect. A local owner could replace it with a junction
	// between the pre-check and that open, let elevated setup create the digest
	// beneath the redirected target, and put the original back before the
	// post-check; the creation record then held the redirected object under the
	// original pathname, so compensation found a mismatch and left a privileged
	// creation as residue.
	//
	// So the split comes from the same inventory that built the root. Only the
	// cache or TEMP directory above the owned tail is opened by name, and every
	// owned component below it, existing or missing, is reached from the retained
	// parent handle.
	base, components, owned := windowsSandboxRuntimeOwnedTail(root)
	if !owned {
		// Refused rather than walked by name. Falling back to the pathname walk is
		// exactly the unprotected path this replaced, and both production builders
		// derive a root from windowsSandboxRuntimeOwnedNames, so a root that does
		// not have the shape is a bug rather than a configuration.
		return nil, fmt.Errorf("sandbox runtime root %s does not have the owned shape %v/<workspace>, so its trust boundary cannot be established", root, windowsSandboxRuntimeOwnedNames)
	}
	// The base itself is the operator's: a redirected LOCALAPPDATA or TMP is an
	// ordinary configuration, and setup has always created it when absent. Only
	// what Zero owns below it is handle-relative.
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, fmt.Errorf("create sandbox runtime base %s: %w", base, err)
	}
	tail := make([]string, 0, len(components))
	current := base
	for _, component := range components {
		current = filepath.Join(current, component)
		tail = append(tail, current)
	}
	created, err := createRuntimeTailHandleRelative(base, tail)
	if err != nil {
		return created, err
	}
	// Re-checked after creation. If an ancestor was swapped for a junction while
	// we were creating, the leaf we just made is in the wrong tree, and granting
	// it the capability ACL would put it on someone elses directory. Reported as
	// a failure so the caller rolls back rather than proceeding.
	if err := refuseReparsedRuntimeAncestors(root); err != nil {
		return created, err
	}
	return created, nil
}

func ensureWindowsSandboxRuntimeRoots(profile PermissionProfile, workspaceRoots []string) (windowsRuntimeRootRollback, error) {
	var rollback windowsRuntimeRootRollback
	for _, root := range windowsSandboxRuntimeRoots(profile, workspaceRoots) {
		created, err := createRuntimeDirRecording(root)
		// Appended before the error check: a partial creation still has to be
		// undone, and returning the record with the error is what lets the caller
		// do that.
		rollback.created = append(rollback.created, created...)
		if err != nil {
			return rollback, err
		}
	}
	return rollback, nil
}

// buildWindowsSandboxSetupACLPlan provisions the runtime roots and then builds the
// plan that grants them, in that order.
//
// One function rather than two statements at the call site because the ordering is
// the contract: BuildWindowsACLPlan emits AllowWrite entries for the runtime
// candidates, applyWindowsACLPlan materializes only DenyRead targets, and an
// AllowWrite target that does not exist fails the entire run. The elevated setup
// path had the provisioning omitted once already, which turned a clean `zero
// sandbox setup` into "windows ACL target does not exist". Keeping the two joined
// here means a caller cannot get the plan without the trees it names.
func buildWindowsSandboxSetupACLPlan(config WindowsSandboxSetupConfig) (WindowsACLPlan, windowsRuntimeRootRollback, error) {
	rollback, err := ensureWindowsSandboxRuntimeRoots(config.PermissionProfile, config.WorkspaceRoots)
	if err != nil {
		return WindowsACLPlan{}, rollback, err
	}
	plan, err := BuildWindowsACLPlan(config.commandConfig())
	if err != nil {
		return WindowsACLPlan{}, rollback, err
	}
	return plan, rollback, nil
}

// windowsSandboxProfileWithProvisionedRuntime is the command-side counterpart:
// it creates the runtime candidates and returns the profile that grants them.
//
// Joined for the same reason as the setup helper, and called from the PARENT for
// one more. The unelevated tier applies its own ACL plan inside the re-exec'd
// runner, where TEMP points into the runtime tree; deriving the candidates there
// yields a temp-side root under the redirected temp rather than the one the plan
// names, so the runner can neither derive nor provision them. The parent still has
// the operator's environment, so it does both and the runner only applies.
func windowsSandboxProfileWithProvisionedRuntime(profile PermissionProfile, workspaceRoots []string) (PermissionProfile, error) {
	// The command side does not roll back: it is not transactional, and a runtime
	// root it created is the tree the command is about to use.
	if _, err := ensureWindowsSandboxRuntimeRoots(profile, workspaceRoots); err != nil {
		return PermissionProfile{}, err
	}
	return windowsSandboxProfileWithRuntime(profile, workspaceRoots), nil
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

// PermissionProfileWithRuntimeRoot names the CONCRETE runtime root on a profile,
// which is what makes the stamp check run.
//
// validateWindowsSandboxRuntimeStamp returns nil when profile.Runtime is nil,
// and that is the correct answer for the setup side and for every unrestricted
// profile. It was the wrong answer for doctor: doctor built its profile with
// PermissionProfileFromPolicy, which never sets Runtime, so the one check that
// can tell an evicted runtime tree from a healthy one was skipped and `zero
// doctor` reported a healthy sandbox on exactly the machine state the stamp was
// added to detect.
func PermissionProfileWithRuntimeRoot(profile PermissionProfile, root string) PermissionProfile {
	if strings.TrimSpace(root) == "" {
		return profile
	}
	return permissionProfileWithRuntime(profile, SandboxRuntime{Root: root})
}

// windowsSandboxRuntimeStampPrefix begins the name of every stamp: a file marking
// a runtime root that ELEVATED SETUP actually provisioned and applied the
// capability ACL to.
const windowsSandboxRuntimeStampPrefix = ".zero-sandbox-setup."

// windowsSandboxRuntimeStampName names the stamp for ONE plan.
//
// THE ATTESTATION IS STORED AT THE SCOPE OF WHAT IT PROVES. The stamp says "this
// tree carries the capability ACL for this exact plan", and it used to live in
// one fixed file per runtime root. The runtime root is chosen per WORKSPACE,
// while the plan is specific to a sandbox HOME: the capability SIDs in it come
// from that home's own store, so two homes for one workspace share a root and
// can never share a plan hash. Setup for home B then overwrote the one file with
// the hash for B, and home A, whose marker and configuration had not changed,
// was rejected as "provisioned for a different configuration". Re-running setup
// for A only moved the breakage to B. Reported by @jatmn.
//
// Naming the file after the plan removes the shared slot instead of arbitrating
// it. Each plan's attestation is its own object: a setup for B cannot replace
// the one for A, and a failed setup's compensation can only reach the stamp of
// the plan it was applying. Eviction still takes every stamp with the tree,
// which is the property the stamp exists for.
//
// A digest of the hash rather than the hash itself, so the name has a fixed
// length and is a safe single path component whatever a caller passes in.
//
// A stamp for a plan nobody uses any more stays until the tree is evicted. It is
// inert: validation reads a stamp only after the home's marker has named the
// same plan, so a leftover proves nothing to anyone.
func windowsSandboxRuntimeStampName(planHash string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(planHash)))
	return windowsSandboxRuntimeStampPrefix + hex.EncodeToString(digest[:16])
}

func windowsSandboxRuntimeStampPath(root string, planHash string) string {
	return filepath.Join(root, windowsSandboxRuntimeStampName(planHash))
}

// writeWindowsSandboxRuntimeStamp records, INSIDE the runtime root, that this
// exact tree carries the capability ACL for this exact plan.
//
// The marker alone cannot tell. It hashes ACL-plan ENTRIES, which are pathnames,
// not the objects those pathnames resolve to. cleanupSandboxRuntimeRoots removes
// inactive roots with os.RemoveAll on an age and count policy, and the next run
// for that workspace recreates the same deterministic pathname through
// os.MkdirAll with ordinary inherited permissions. The plan hash is unchanged,
// so both the elevated and the unelevated marker checks reported setup as
// current while the recreated directory carried NO capability ACE, and a
// WRITE_RESTRICTED token could not write TMP, GOCACHE or anything else under it.
//
// A file inside the tree survives exactly as long as the tree does. Eviction
// takes it, ordinary recreation does not restore it, so its absence is precisely
// the condition "this pathname exists but is not the object setup provisioned".
func writeWindowsSandboxRuntimeStamp(root string, planHash string) error {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create sandbox runtime root for the setup stamp: %w", err)
	}
	// Written through the rooted traversal where one is available, so the stamp
	// lands in the object the ACL was applied to rather than in whatever the
	// pathname resolves to by now. MkdirAll above and a pathname write left a
	// second unbound interval: a tree replaced after the ACL apply could be
	// recreated and stamped with no capability grant on it at all, and validation
	// still passed because it only compares the stamp contents.
	if err := writeRuntimeStampThroughHandle(root, planHash); err == nil {
		return nil
	} else if !errors.Is(err, errRuntimeTailNotOwned) && !errors.Is(err, errNoRootedStampWriter) {
		return err
	}
	if err := os.WriteFile(windowsSandboxRuntimeStampPath(root, planHash), []byte(planHash), 0o600); err != nil {
		return fmt.Errorf("write sandbox runtime setup stamp: %w", err)
	}
	return nil
}

// validateWindowsSandboxRuntimeStamp reports whether the runtime root this
// command will use is the one setup provisioned.
//
// Absent when the profile carries no runtime root, which is the setup side
// itself and every non-restricted profile, so this adds no requirement where
// there is nothing to check.
func validateWindowsSandboxRuntimeStamp(profile PermissionProfile, planHash string) error {
	if profile.Runtime == nil {
		return nil
	}
	root := strings.TrimSpace(profile.Runtime.Root)
	if root == "" {
		return nil
	}
	// Read with FILE_SHARE_DELETE on Windows, so a setup republishing the stamp
	// while this runs replaces it instead of failing on this read.
	recorded, err := readWindowsSandboxRuntimeStampFile(windowsSandboxRuntimeStampPath(root, planHash))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("the sandbox runtime directory for this workspace was removed since setup ran, so it no longer carries the permissions the sandbox needs — run `zero sandbox setup` from an elevated (Administrator) terminal (%s)", root)
		}
		return fmt.Errorf("read sandbox runtime setup stamp: %w", err)
	}
	// The name already says which plan this stamp is for, so a mismatch here is no
	// longer "another setup got here last". It means the file for THIS plan does
	// not hold this plan, which only damage or tampering produces. The contents
	// are still compared because they are the claim itself; the name is only a
	// digest that says where to look for it.
	if strings.TrimSpace(string(recorded)) != strings.TrimSpace(planHash) {
		return fmt.Errorf("the sandbox runtime directory for this workspace was provisioned for a different configuration — run `zero sandbox setup` from an elevated (Administrator) terminal (%s)", root)
	}
	return nil
}

// windowsSandboxSelectedRuntimeRoot returns the concrete runtime root a profile
// carries, or empty when it carries none.
func windowsSandboxSelectedRuntimeRoot(profile PermissionProfile) string {
	if profile.Runtime == nil {
		return ""
	}
	return strings.TrimSpace(profile.Runtime.Root)
}

// ValidateWindowsSandboxLaunchGrants reports whether the objects this command's
// ACL plan names still carry its allow grants.
//
// SEPARATE FROM THE MARKER, BECAUSE IT IS A DIFFERENT KIND OF QUESTION. The
// marker comparisons ask whether setup's intent matches this command's, and both
// sides can agree perfectly while the filesystem has moved on underneath them.
// The runtime stamp narrows it to "the directory was not removed and recreated
// under this pathname", which an ordinary file answers by existing: it survives
// an ACL edit untouched, so an `icacls /reset`, an inheritance change on a
// parent, or a security product rewriting the DACL all leave a valid stamp over
// a runtime root the WRITE_RESTRICTED child cannot write.
//
// This one reads the security descriptors, and it is a LAUNCH decision rather
// than a report: the elevated tier calls it beside its marker validation, which
// puts both tiers' attestation in the same place, one refusing because it cannot
// repeat an elevated provisioning and one re-applying because it can.
func ValidateWindowsSandboxLaunchGrants(config WindowsSandboxSetupConfig) error {
	plan, err := BuildWindowsACLPlan(config.commandConfig())
	if err != nil {
		return err
	}
	if windowsACLPlanApplied(plan) {
		return nil
	}
	return errors.New("the sandbox directories for this workspace no longer carry the permissions setup granted them, " +
		"so the sandboxed command would be unable to write its temp and cache directories — " +
		"run `zero sandbox setup` from an elevated (Administrator) terminal")
}
