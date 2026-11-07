//go:build windows

package sandbox

import (
	"fmt"
	"io"
	"strings"

	"golang.org/x/sys/windows"
)

// Seams for the identity gate above. The gate refuses a setup whose carried
// consumer is not the account doing the installing, and both halves of that are
// otherwise unobservable from a test: an unelevated box never reaches the gate,
// and one process cannot hold two user SIDs. Production values by default.
var (
	windowsSetupProcessIsElevated = windowsProcessIsElevated
	windowsSetupInstallerSID      = currentProcessSID
	// windowsSetupApplyNetworkPlan installs the WFP filters, which needs a real
	// Administrator token and machine-wide state. Behind a variable so a test can
	// drive the rest of this transaction, the part that publishes and compensates
	// the stamp and the marker, on a box that cannot touch the filtering platform.
	windowsSetupApplyNetworkPlan = applyWindowsNetworkPlan
)

func runWindowsSandboxSetup(config WindowsSandboxSetupConfig, stderr io.Writer) (code int) {
	// Do not provision DenyRead ACLs for a token mode that cannot launch normal
	// tools with DenyRead under the narrow restricting-SID set (PR #640).
	if err := windowsDenyReadRestrictedTokenUnsupportedProfile(config.PermissionProfile); err != nil {
		fmt.Fprintln(stderr, WindowsSandboxSetupName+": "+err.Error())
		return 1
	}
	// Applying the WFP network filters and workspace ACLs requires Administrator
	// rights; without them WFP fails deep inside with a raw ACCESS_DENIED (0x5).
	// Check up front and return an actionable message instead.
	if !windowsSetupProcessIsElevated() {
		fmt.Fprintln(stderr, WindowsSandboxSetupName+": Administrator rights are required. Re-run `zero sandbox setup` from an elevated (Run as administrator) terminal.")
		return 1
	}
	// THE CONSUMER IS CARRIED, AND VALIDATED BEFORE ANYTHING IS PROVISIONED.
	//
	// Zero does not elevate. runSandboxSetupHelper launches this with a plain
	// exec.Command, and the check above requires the caller to have elevated the
	// terminal already, so the SID that arrives here was resolved in a process that
	// was ALREADY elevated. The only model that actually works is the documented
	// one: an elevated terminal belonging to the account that will run Zero
	// afterwards. Same-account UAC satisfies it, because both tokens carry the same
	// user SID.
	//
	// Alternate-administrator setup does not. The SID would describe the admin, the
	// stamp would get its allow ACE for the wrong token, and every restricted
	// command would stop before launch on a setup that had just reported success.
	// That case cannot be detected from inside an already-elevated process, so it is
	// EXCLUDED rather than silently mis-provisioned: the carried identity must match
	// this token. That holds today, and it makes the unsupported model fail loudly
	// the moment a real elevation step is introduced between the two.
	if trimmed := strings.TrimSpace(config.ConsumerSID); trimmed != "" {
		consumer, sidErr := windows.StringToSid(trimmed)
		if sidErr != nil {
			fmt.Fprintln(stderr, WindowsSandboxSetupName+": the calling user SID could not be parsed: "+sidErr.Error())
			return 1
		}
		installer, installerErr := windowsSetupInstallerSID()
		if installerErr != nil {
			fmt.Fprintln(stderr, WindowsSandboxSetupName+": "+installerErr.Error())
			return 1
		}
		if !strings.EqualFold(strings.TrimSpace(installer), trimmed) {
			fmt.Fprintln(stderr, WindowsSandboxSetupName+": setup is running as "+installer+" but the stamp would be provisioned for "+trimmed+
				". Alternate-account setup is not supported: run `zero sandbox setup` from a terminal elevated as the account that will use Zero.")
			return 1
		}
		restore := setWindowsSetupConsumerSID(consumer)
		defer restore()
	}

	// ONE SETUP PER SANDBOX HOME AT A TIME, from before the first thing recorded
	// about the previous state until the marker is published or every
	// compensation has run. Taken ahead of the lease, the provisioning and the
	// snapshot, and released by the deferred call after whichever return below
	// runs, so the stamp and marker this process publishes, or the snapshot it
	// restores, can never interleave with another helper's. See
	// lockWindowsSandboxSetup.
	unlockSetup, lockErr := lockWindowsSandboxSetup(config.SandboxHome)
	if lockErr != nil {
		fmt.Fprintln(stderr, WindowsSandboxSetupName+": "+lockErr.Error())
		return 1
	}
	defer func() {
		// Released after whichever return below ran, so a failed release is
		// reported on top of that result, and a setup that could not confirm it
		// let go of the lock does not exit 0. See lockWindowsSandboxSetup.
		if releaseErr := unlockSetup(); releaseErr != nil {
			fmt.Fprintln(stderr, WindowsSandboxSetupName+": "+releaseErr.Error())
			code = 1
		}
	}()

	// HOLD THE SELECTED ROOT FOR THE WHOLE TRANSACTION.
	//
	// The unelevated caller took a lease only to learn which root wins and released
	// it immediately, so nothing owned that root while this process provisions the
	// tree, applies the ACL and stamp, installs network state and writes the
	// marker. A command for another workspace scanning the same runtime parent
	// excludes only ITS own current root, so it can take this root's cleanup lease
	// and RemoveAll it. In the damaging ordering cleanup selects the root before
	// setup refreshes its mtime and removes it after the stamp handle closes but
	// before the marker is published, so setup reports success for a pathname that
	// is gone or delete-pending and the next command finds no stamp on what it
	// selected.
	//
	// A shared lease is what the cleanup's exclusive acquire fails against, and it
	// is released when this function returns, which is after the marker write. If
	// it cannot be taken, fail here: that is before any ACL, network or marker
	// state is persisted, so a retry can still get the operator out.
	if root := strings.TrimSpace(windowsSandboxSelectedRuntimeRoot(config.PermissionProfile)); root != "" {
		lease, leaseErr := prepareSandboxRuntimeLease(root)
		if leaseErr != nil {
			fmt.Fprintln(stderr, WindowsSandboxSetupName+": reserve the sandbox runtime root "+root+" for setup: "+leaseErr.Error())
			return 1
		}
		defer lease.release()
	}
	// Provisions the runtime candidate roots, then builds the plan that grants
	// them. One call because a granted-but-absent write root fails the whole apply.
	plan, runtimeRollback, err := buildWindowsSandboxSetupACLPlan(config)
	// SETUP IS TRANSACTIONAL FOR THE STATE IT CREATED. Runtime roots are
	// materialized before the network plan, the ACL apply, the network apply and
	// the marker write, and every one of those can fail. Previously only ACL
	// snapshots were restored, so a run that reported failure still left new
	// persistent runtime directories behind, and it could not have cleaned them up
	// even in principle because provisioning returned nothing about what it made.
	//
	// Composed once here so no later failure path can forget it. It removes only
	// directories THIS run created, innermost first, and refuses to remove a
	// non-empty one, so a pre-existing cache or temp tree is never touched.
	failed := func(cause error) int {
		fmt.Fprintln(stderr, WindowsSandboxSetupName+": "+runWindowsSandboxSetupCompensations(cause, nil, runtimeRollback).Error())
		return 1
	}
	if err != nil {
		return failed(err)
	}
	// Always provision the mode-INDEPENDENT infrastructure: the outbound block
	// filters scoped to the offline-marker SID. Runtime gates network per command
	// by whether the token carries that SID, so one setup serves both modes.
	networkPlan, err := BuildWindowsNetworkInfraPlan(config.commandConfig())
	if err != nil {
		return failed(err)
	}
	// THE STAMP RIDES WITH THE ACE. Computed before the apply so the apply can
	// write it through the very handle it grants the capability on, which is the
	// only way the two are provably about one object. Writing it afterwards by
	// pathname left a window in which the predictable root could be replaced by an
	// ordinary directory that then collected a valid-looking stamp while carrying
	// no ACE at all.
	marker, err := BuildWindowsSandboxSetupMarker(config)
	if err != nil {
		return failed(err)
	}
	var stamp *windowsACLStampRequest
	if root := windowsSandboxSelectedRuntimeRoot(config.PermissionProfile); strings.TrimSpace(root) != "" {
		stamp = &windowsACLStampRequest{Root: root, PlanHash: marker.ACLPlanHash}
		// Snapshotted BEFORE the apply, because the apply is now what writes the
		// stamp. Taking it afterwards would record this run's own stamp as the
		// state to restore, so a failed setup would put its own artifact back
		// rather than what it found.
		//
		// AND REFUSED IF IT CANNOT BE ESTABLISHED. The stamp writer uses
		// FILE_OVERWRITE_IF, so it can replace an existing stamp even where this
		// read was denied. Continuing on an unknown prior state would let a setup
		// that then fails delete an attestation it never recorded, leaving the
		// previous run's marker pointing at a runtime root it can no longer
		// prove. No privileged state is applied until this is known.
		snapshot, snapshotErr := snapshotWindowsSandboxRuntimeStamp(root, marker.ACLPlanHash)
		if snapshotErr != nil {
			return failed(snapshotErr)
		}
		runtimeRollback.stamp = snapshot
		// Carried to the apply so the two stages are provably about one object,
		// rather than about one pathname resolved twice.
		stamp.RootIdentity = snapshot.rootIdentity
		stamp.RootIdentified = snapshot.rootIdentified
	}
	rollback, err := applyWindowsACLPlanWithStamp(plan, stamp)
	if err != nil {
		return failed(err)
	}
	// From here both have to be undone, ACLs first so the directories are empty
	// of our grants before they are removed. This used to return as soon as the
	// ACL rollback reported an error, so the runtime rollback never ran and a
	// failed setup kept the persistent directories it had just created; one undo
	// failing is the moment the others matter most.
	failedAfterACL := func(cause error) int {
		fmt.Fprintln(stderr, WindowsSandboxSetupName+": "+runWindowsSandboxSetupCompensations(cause, rollback, runtimeRollback).Error())
		return 1
	}
	if err := windowsSetupApplyNetworkPlan(networkPlan); err != nil {
		return failedAfterACL(err)
	}
	// The stamp is already on disk, written through the handle the capability ACE
	// was applied on, so this records the marker only and never names the runtime
	// tree again. The rollback already owns that stamp, snapshotted above, so a
	// failure here still leaves the root removable.
	if err := writeWindowsSandboxSetupMarkerFile(config, marker); err != nil {
		return failedAfterACL(err)
	}
	return 0
}

// windowsProcessIsElevated reports whether the current process runs with an
// elevated (Administrator) token. On any error obtaining the token it returns
// true so the setup proceeds and surfaces the real WFP/ACL error rather than a
// false "needs admin" claim.
func windowsProcessIsElevated() bool {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return true
	}
	defer token.Close()
	return token.IsElevated()
}
