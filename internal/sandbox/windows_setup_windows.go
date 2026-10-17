//go:build windows

package sandbox

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/sys/windows"
)

// Seams for the setup transaction's external effects.
//
// Every step below needs something a test machine cannot be asked for: an
// elevated token, a machine-global mutex, real DACLs, the WFP engine, a local
// account. That left the ORDER of those steps and the handling of each failure
// — which is where this function's bugs actually live — with no coverage at all.
// Each var defaults to its production function and is only ever reassigned by a
// test. removeWindowsSandboxPrincipalForSetupFn and applyWindowsACLPlanFn
// already existed in windows_identity_windows.go for the same reason; the rest
// follow them, and the ACL call below now goes through that one so a single stub
// covers both plans.
var (
	windowsProcessIsElevatedFn       = windowsProcessIsElevated
	acquireWindowsSandboxSetupLockFn = acquireWindowsSandboxSetupLock
	applyWindowsNetworkPlanFn        = applyWindowsNetworkPlan
	setupWindowsSandboxPrincipalFn   = setupWindowsSandboxPrincipal
	writeWindowsSandboxSetupMarkerFn = WriteWindowsSandboxSetupMarker
)

func runWindowsSandboxSetup(config WindowsSandboxSetupConfig, stderr io.Writer) int {
	// Do not provision DenyRead ACLs for a token mode that cannot launch normal
	// tools with DenyRead under the narrow restricting-SID set (PR #640).
	if err := windowsDenyReadRestrictedTokenUnsupportedProfile(config.PermissionProfile); err != nil {
		fmt.Fprintln(stderr, WindowsSandboxSetupName+": "+err.Error())
		return 1
	}
	// Applying the WFP network filters and workspace ACLs requires Administrator
	// rights; without them WFP fails deep inside with a raw ACCESS_DENIED (0x5).
	// Check up front and return an actionable message instead.
	if !windowsProcessIsElevatedFn() {
		fmt.Fprintln(stderr, WindowsSandboxSetupName+": Administrator rights are required. Re-run `zero sandbox setup` from an elevated (Run as administrator) terminal.")
		return 1
	}
	// Serialize the WHOLE transaction, not the individual writes.
	//
	// Setup rotates the principal's password, stores the secret,
	// read-modify-writes the ACL ledger, installs WFP filters and writes the
	// marker. Two setups for this workspace at once interleave those steps: the
	// account ends up with one process's password while the stored secret is the
	// other's, so every later command fails to log on, and the ledger's
	// read-modify-write silently loses a path set. Each write being individually
	// atomic does nothing about interleaving BETWEEN writes, which is the actual
	// failure, so the lock is taken here and held to the end.
	//
	// Immediately after the elevation check, because acquiring a Global object
	// needs the rights that check just confirmed.
	lock, err := acquireWindowsSandboxSetupLockFn(windowsSandboxWorkspaceKey(config.WorkspaceRoots))
	if err != nil {
		fmt.Fprintln(stderr, WindowsSandboxSetupName+": "+err.Error())
		return 1
	}
	defer lock.release()
	if lock.Abandoned() {
		// The previous holder died somewhere inside the transaction, so this
		// machine may be half set up. Setup is idempotent and re-running it is
		// the repair, but say so: a silent recovery hides that something crashed
		// while holding the machine's sandbox state open.
		fmt.Fprintf(stderr, "%s: a previous setup for this workspace exited without finishing; re-running to repair it.\n",
			WindowsSandboxSetupName)
	}

	// Before anything is provisioned: a principal this helper could not hand back
	// to the caller must not be created at all.
	if config.PrincipalOptIn {
		// THE SAME REFUSAL THE ARGUMENT BUILDER MAKES, AT THE BOUNDARY THAT MUTATES.
		// BuildWindowsSandboxSetupArgs refuses to serialize a principal opt-in while
		// no launch path exists, but both shipped entrypoints reach this function
		// through the parser, and handwritten `--sandbox-principal 1` arrived here
		// with nothing in the way of account, secret and ACL provisioning for a
		// principal nothing can run. Arguments are an input format, not proof that
		// the builder approved them.
		if err := windowsPrincipalLaunchAvailable(); err != nil {
			fmt.Fprintln(stderr, WindowsSandboxSetupName+": refusing to provision a sandbox principal: "+err.Error())
			return 1
		}
		if err := assertWindowsSetupRunsAsCaller(config.CallerSID); err != nil {
			fmt.Fprintln(stderr, WindowsSandboxSetupName+": "+err.Error())
			return 1
		}
	}

	// Create the runtime roots BEFORE building the plan that grants them.
	//
	// The capability plan deliberately does not materialize a write root: an
	// entry whose path is absent is a typo or a stale config, and inventing the
	// tree would silently grant write on a directory nobody asked for. So every
	// write root in the plan has to exist by the time it is applied, and the
	// runtime roots are the only ones setup owns rather than receives.
	//
	// Both of them, not the one this process would select. Setup grants the whole
	// candidate set so a later command that falls back still lands on a
	// provisioned tree, and granting a path setup never created fails the entire
	// run with "windows ACL target does not exist".
	//
	// And recoverable. Preparing an EXISTING candidate rewrites its DACL, which
	// strips the grant a previous setup put there, before the ACL transaction
	// below has a baseline to restore. Record what was there first and put it
	// back on every failure between here and a completed apply.
	candidateDACLs, err := snapshotWindowsRuntimeCandidateDACLs(windowsSandboxRuntimeCandidates(config.WorkspaceRoots))
	if err != nil {
		fmt.Fprintln(stderr, WindowsSandboxSetupName+": "+err.Error())
		return 1
	}
	restoreCandidateDACLs := func() {
		if err := restoreWindowsRuntimeCandidateDACLs(candidateDACLs); err != nil {
			fmt.Fprintln(stderr, WindowsSandboxSetupName+": restoring runtime root permissions after the failure: "+err.Error())
		}
	}
	if err := ensureWindowsSandboxRuntimeCandidates(config.WorkspaceRoots); err != nil {
		fmt.Fprintln(stderr, WindowsSandboxSetupName+": "+err.Error())
		restoreCandidateDACLs()
		return 1
	}

	plan, err := BuildWindowsACLPlan(config.commandConfig())
	if err != nil {
		fmt.Fprintln(stderr, WindowsSandboxSetupName+": "+err.Error())
		restoreCandidateDACLs()
		return 1
	}
	// Before the first mutation, and on this tier too. Elevated setup CAN write a
	// volume root's DACL, which is exactly why it must not: the grant is
	// inheritable, so applying it rewrites permissions across the drive rather than
	// changing one sandbox-owned object.
	if refusal := WindowsACLPlanReadGrantRefusal(plan); refusal != "" {
		fmt.Fprintln(stderr, WindowsSandboxSetupName+": "+refusal)
		restoreCandidateDACLs()
		return 1
	}
	rollback, err := applyWindowsACLPlanFn(plan)
	if err != nil {
		fmt.Fprintln(stderr, WindowsSandboxSetupName+": "+err.Error())
		restoreCandidateDACLs()
		return 1
	}
	// The transaction's own baseline is the post-preparation DACL, so a later
	// failure that rolls the apply back still needs the pre-preparation one put
	// back after it.
	appliedRollback := rollback
	rollback = func() error {
		err := appliedRollback()
		restoreCandidateDACLs()
		return err
	}
	// Provision this workspace's sandbox principal, when opted in. A principal is
	// a separate local account, so it is created only on an explicit opt-in: it
	// is visible in `net user`, and account creation is exactly the kind of thing
	// endpoint protection and enterprise policy object to. Without the opt-in the
	// capability-SID backend above is the whole of setup, unchanged.
	if windowsSandboxIdentityEnabled(config.commandConfig().Env) {
		principalRollback, err := setupWindowsSandboxPrincipalFn(config.commandConfig())
		if err != nil {
			if rollbackErr := rollback(); rollbackErr != nil {
				fmt.Fprintf(stderr, "%s: %v; rollback failed: %v\n", WindowsSandboxSetupName, err, rollbackErr)
				return 1
			}
			fmt.Fprintln(stderr, WindowsSandboxSetupName+": "+err.Error())
			return 1
		}
		// Fold the principal into the existing rollback so every later failure
		// path undoes it too, rather than each one having to remember.
		aclRollback := rollback
		rollback = func() error {
			principalErr := principalRollback()
			aclErr := aclRollback()
			if principalErr != nil {
				return principalErr
			}
			return aclErr
		}
	} else if err := removeWindowsSandboxPrincipalsForSetupFn(config.commandConfig()); err != nil {
		// Opting out has to actually retire the principal, because that is what
		// we tell people it does. ValidateWindowsSandboxSetupMarker sends an
		// operator here in as many words: re-run setup from an elevated terminal
		// without the opt-in to retire the principal. Until now this branch did
		// not exist, so the marker flipped to opted-out while the account, its
		// secret, its logon rights, its ACEs and its ledger all stayed exactly
		// where they were. The instruction was a lie, and the leftovers were
		// invisible, because nothing afterwards looks for a principal it believes
		// was never provisioned.
		//
		// Fatal only when the ACCOUNT survived, which is a narrower rule than
		// either of the two this branch has had.
		//
		// It first printed and carried on, which was wrong: a marker that says "no
		// principal" while the account is still installed is a lie nothing
		// afterwards can detect, because nothing looks for a principal it believes
		// was never provisioned.
		//
		// Then it failed on any error at all, which was wrong in the other
		// direction. removeWindowsSandboxPrincipalForSetup deliberately continues
		// past a secret file owned by ANOTHER administrator's setup and reports it
		// at the end, so that one unlinkable file does not strand the account, its
		// logon rights and its ACEs. Treating that report as fatal made the whole
		// default sandbox unsetupable on such a machine, permanently, over inert
		// residue: the account is already gone and the leftover is a file with a
		// DACL we do not own.
		//
		// So the question is not whether teardown reported a problem, it is
		// whether the thing the marker is about to deny is still there. Asked of
		// the account rather than inferred from the error, because the error is
		// a join and its parts are not separable at this distance.
		if windowsSandboxPrincipalIsInstalled(config.commandConfig()) {
			if rollbackErr := rollback(); rollbackErr != nil {
				fmt.Fprintf(stderr, "%s: opted out, but the existing sandbox principal is still installed: %v; rollback failed: %v\n",
					WindowsSandboxSetupName, err, rollbackErr)
				return 1
			}
			fmt.Fprintf(stderr, "%s: opted out, but the existing sandbox principal is still installed: %v\n",
				WindowsSandboxSetupName, err)
			return 1
		}
		// Retired, with something inert left over. Named rather than swallowed:
		// whoever has to clean it up needs to know it is there.
		fmt.Fprintf(stderr, "%s: opted out; the sandbox principal was retired, but some of its residue could not be removed: %v\n",
			WindowsSandboxSetupName, err)
	}
	// Built AFTER any principal provisioning, not before. The block filters are
	// keyed to the offline group as well as the offline-marker SID, and that group
	// is created as part of provisioning: planning first would install filters
	// that name only the marker, leaving every offline principal with an open
	// network while looking correctly set up.
	//
	// Mode-INDEPENDENT by design. Which identity a command runs under is what
	// selects allow or deny, so one setup serves both modes.
	networkPlan, err := BuildWindowsNetworkInfraPlan(config.commandConfig())
	if err != nil {
		if rollbackErr := rollback(); rollbackErr != nil {
			fmt.Fprintf(stderr, "%s: %v; rollback failed: %v\n", WindowsSandboxSetupName, err, rollbackErr)
			return 1
		}
		fmt.Fprintln(stderr, WindowsSandboxSetupName+": "+err.Error())
		return 1
	}
	// Refuse to install filters that would not cover the principals just
	// provisioned. The ordering above is what makes them cover it, and nothing at
	// this call site shows that, so assert it rather than trusting it to survive
	// a later refactor. Installing anyway would leave a machine reporting a
	// successful setup while every offline principal had an open network.
	// Gated on the SAME opt-in that decides whether principals were provisioned
	// at all (line 36). The offline group is created inside provisioning, so on
	// an opt-out machine it legitimately does not exist, and asserting there
	// would refuse a setup that has nothing to get wrong.
	if err := assertWindowsNetworkPlanCoversOfflineGroup(networkPlan, resolveWindowsSandboxOfflineGroupSIDHook,
		windowsSandboxIdentityEnabled(config.commandConfig().Env)); err != nil {
		if rollbackErr := rollback(); rollbackErr != nil {
			fmt.Fprintf(stderr, "%s: %v; rollback failed: %v\n", WindowsSandboxSetupName, err, rollbackErr)
			return 1
		}
		fmt.Fprintln(stderr, WindowsSandboxSetupName+": "+err.Error())
		return 1
	}
	// Install the machine's plan, not this home's. The filters are global and are
	// replaced wholesale on every setup, so an opted-out run here would otherwise
	// strip the offline group SID that another workspace's principals depend on
	// and hand them egress under a NetworkDeny profile. networkPlan itself is left
	// alone because it is what this home's marker fingerprints.
	planToInstall, planErr := WindowsNetworkPlanForApply(networkPlan, resolveWindowsSandboxOfflineGroupSIDHook)
	if planErr != nil {
		if rollbackErr := rollback(); rollbackErr != nil {
			fmt.Fprintf(stderr, "%s: %v; rollback failed: %v\n", WindowsSandboxSetupName, planErr, rollbackErr)
			return 1
		}
		fmt.Fprintln(stderr, WindowsSandboxSetupName+": "+planErr.Error())
		return 1
	}
	if err := applyWindowsNetworkPlanFn(planToInstall); err != nil {
		if rollbackErr := rollback(); rollbackErr != nil {
			fmt.Fprintf(stderr, "%s: %v; rollback failed: %v\n", WindowsSandboxSetupName, err, rollbackErr)
			return 1
		}
		fmt.Fprintln(stderr, WindowsSandboxSetupName+": "+err.Error())
		return 1
	}
	if _, err := writeWindowsSandboxSetupMarkerFn(config); err != nil {
		if rollbackErr := rollback(); rollbackErr != nil {
			fmt.Fprintf(stderr, "%s: %v; rollback failed: %v\n", WindowsSandboxSetupName, err, rollbackErr)
			return 1
		}
		fmt.Fprintln(stderr, WindowsSandboxSetupName+": "+err.Error())
		return 1
	}
	return 0
}

// windowsSandboxPrincipalIsInstalled reports whether this workspace's principal
// account still resolves.
//
// Fails CLOSED. Only an outright "not provisioned" counts as gone; a name that
// resolves to something unexpected, or a lookup that fails for its own reasons,
// is treated as still installed. The consumer uses this to decide whether an
// opted-out marker would be a lie, and the expensive mistake there is claiming a
// principal is gone when nobody actually checked.
// A workspace now has TWO principals, and either one still installed makes the
// answer yes. Checking a single role would let an opted-out marker claim the
// workspace is clean while the other account is still on the machine, which is
// the exact lie this guard exists to prevent.
func windowsSandboxPrincipalIsInstalled(config WindowsSandboxCommandConfig) bool {
	// EVERY RETIRABLE ROLE, not just the live pair. Teardown retires legacy too,
	// so an upgraded machine still holding the untagged pre-split account would
	// otherwise be reported clean here while that account was still installed,
	// and the opted-out marker would be exactly the lie this guard prevents.
	key := windowsSandboxPrincipalKey(config)
	for _, role := range windowsSandboxRetirableRoles {
		if _, err := lookupWindowsSandboxIdentityFn(key, role); !errors.Is(err, errWindowsSandboxIdentityUnavailable) {
			return true
		}
	}
	return false
}

// assertWindowsSetupRunsAsCaller refuses to provision a sandbox principal when
// this elevated helper belongs to a different Windows user than the caller that
// launched it.
//
// A principal is unusable across that boundary, and not for a reason more
// plumbing can fix. Its password is sealed with CryptProtectData, which derives
// its key from the CALLING user, so a blob written here is decryptable only by
// this administrator; the caller's next command would find the account, find the
// secret file, and fail to unseal it. Threading the caller's SID through the
// setup args fixes the account NAME, the ledger and the ACEs — everything that
// is merely named after the invoking user — but it cannot re-key DPAPI for a
// user whose token this process does not hold.
//
// So the answer is to say so before anything exists, rather than provision an
// account, a secret and a grant that resolve to a sandbox nobody can log into.
//
// Same-account elevation is unaffected: a UAC consent prompt splits the caller's
// token but leaves the user SID identical, which is the ordinary path. An
// unknown caller SID (an older caller, or a token query that failed) is treated
// as a match: that is the pre-existing behaviour, and refusing on absence would
// break setup on machines where nothing is wrong.
//
// What this does NOT do, stated plainly because the name suggests otherwise:
// it cannot fire on any path Zero itself takes. Zero never elevates the helper.
// runSandboxSetupHelper (internal/cli/app.go) is a plain exec.Command with no
// token work, and runWindowsSandboxSetup refuses unless the process is ALREADY
// elevated, so the operator supplies elevation by opening an elevated terminal.
// Both halves then run in that terminal: BuildWindowsSandboxSetupArgs resolves
// the caller SID from the very process that later becomes the helper, so the two
// are equal by construction and this returns nil every time.
//
// It is kept rather than deleted because it is a correctness assertion, not a
// no-op. It becomes load-bearing the moment anyone adds a ShellExecute "runas"
// path, which is the natural way this command grows, and it already covers a
// helper .exe invoked directly with hand-written args. What it must not do is be
// mistaken for a live defence against over-the-shoulder elevation today.
//
// The residual hazard that boundary was supposed to describe is real but has a
// different shape: an operator who elevates a terminal as a DIFFERENT
// administrator runs both halves as that administrator, so setup provisions into
// THAT account's sandbox home. Their own unelevated session resolves its own
// home, finds no marker, and is told to run setup. Loud, not silent, and not a
// weakened sandbox.
//
// Scoped to the opt-in by its caller, deliberately. The default restricted-token
// sandbox stores no secret and names no account after the user, so it works
// perfectly well across this boundary and must keep doing so.
func assertWindowsSetupRunsAsCaller(callerSID string) error {
	caller := strings.TrimSpace(callerSID)
	if caller == "" {
		return nil
	}
	current := windowsCurrentUserSID()
	if current == "" || strings.EqualFold(current, caller) {
		return nil
	}
	return fmt.Errorf("this elevated setup is running as a different Windows user (%s) than the one that started it (%s), "+
		"and a sandbox principal's password is sealed to the user that stores it, so the account provisioned here could never be used. "+
		"Re-run `zero sandbox setup` from a terminal elevated as your own account, or unset %s to use the restricted-token sandbox",
		current, caller, windowsSandboxIdentityEnv)
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
