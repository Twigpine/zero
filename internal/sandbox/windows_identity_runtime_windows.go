//go:build windows

package sandbox

// Using a sandbox principal at command time.
//
// This is the seam between the identity model and the existing runner. It is
// deliberately fail-soft: when no principal is provisioned, when the secret is
// missing, or when the opt-in is off, it reports "not available" and the caller
// keeps using today's restricted-token backend. Only an outright failure to log
// on with a principal that IS provisioned surfaces as an error, because that
// means setup ran but the identity is broken, and silently downgrading the
// sandbox in that case would be the wrong kind of quiet.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"golang.org/x/sys/windows"
)

// The opt-in itself (windowsSandboxIdentityEnv and windowsSandboxIdentityEnabled)
// lives in windows_setup.go: it is part of the setup protocol, which the elevated
// half and the command half both have to read the same way, so it cannot be
// Windows-only.

// windowsSandboxWorkspaceKey derives the per-workspace key a principal is named
// after. It hashes the workspace root the same way the sandbox runtime keys its
// own state, so the account name leaks no path and one workspace always maps to
// one principal.
func windowsSandboxWorkspaceKey(workspaceRoots []string) string {
	root := ""
	for _, candidate := range workspaceRoots {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			root = normalizeProfilePath(trimmed)
			break
		}
	}
	if root == "" {
		root = "default"
	}
	digest := sha256.Sum256([]byte(strings.ToLower(root)))
	return hex.EncodeToString(digest[:])
}

// windowsSandboxPrincipalEligible reports whether the principal backend may be
// used for this command at all, before any account or secret is consulted.
//
// Kept separate from the lookup so the decision is observable on its own: on a
// machine with nothing provisioned the lookup declines anyway, which would let a
// missing guard here pass unnoticed.
func windowsSandboxPrincipalEligible(config WindowsSandboxCommandConfig) bool {
	return windowsSandboxIdentityEnabled(config.Env)
}

// windowsSandboxRoleForNetwork maps a command's network mode onto the principal
// that enforces it.
//
// The two roles differ only in membership of the offline group, which the block
// filters are keyed to. That indirection exists because network denial cannot be
// expressed on a logon token the way it is on a restricted token: the restricted
// token carries the offline-marker SID as a restricting SID, and LogonUser has
// no equivalent, since it builds a token from the account's real memberships.
//
// Defaulting anything that is not an explicit allow to the offline principal is
// deliberate. A mode this does not recognise should lose the network, not keep
// it.
func windowsSandboxRoleForNetwork(mode NetworkMode) windowsSandboxRole {
	// Delegated so this and `zero doctor` answer from one rule. Doctor reporting a
	// different account than the runtime actually uses is the drift that made the
	// old inactive-reason helper wrong.
	if WindowsSandboxPrincipalRoleForNetwork(mode) == string(windowsSandboxRoleOnline) {
		return windowsSandboxRoleOnline
	}
	return windowsSandboxRoleOffline
}

// windowsSandboxPrincipalToken returns a token for this workspace's sandbox
// principal.
//
// ok is false, with a nil error, whenever the principal backend simply is not in
// play: the opt-in is off, setup has not provisioned an account, or no secret is
// stored. The caller falls back to the restricted token in those cases. An error
// means the identity exists but could not be used, which is worth surfacing
// rather than downgrading around.
func windowsSandboxPrincipalToken(config WindowsSandboxCommandConfig) (windows.Token, bool, error) {
	if !windowsSandboxPrincipalEligible(config) {
		// Deliberately silent, and this is a change of mind worth recording.
		//
		// Announcing it looks right: deny is the DEFAULT network mode, so an
		// operator who opted in never gets a principal for ordinary commands, and
		// that is worth knowing. But the warning cannot be delivered here. This
		// runner is re-exec'd per command as `zero __windows-command-runner`, so the
		// sync.Once below is once per COMMAND, not once per session — the notice
		// would land on the stderr of essentially every sandboxed tool call. Noise
		// that repeats gets filtered by the reader rather than acted on, which is
		// the exact failure the helper's own comment warns about.
		//
		// It is also not actionable per command: windowsSandboxPrincipalEligible
		// prefers network enforcement over read confinement on purpose, so there is
		// nothing to do differently. A standing configuration fact belongs on a
		// surface read once — `zero doctor`, which carries the opt-in now.
		return 0, false, nil
	}
	key := windowsSandboxPrincipalKey(config)
	// The network mode picks the principal. Both are provisioned by setup; the
	// offline one is in the group the block filters match, so choosing here is
	// what enforces the mode.
	role := windowsSandboxRoleForNetwork(config.PermissionProfile.Network.Mode)
	identity, err := lookupWindowsSandboxPrincipalForCommandFn(key, role)
	if err != nil {
		if errors.Is(err, errWindowsSandboxIdentityUnavailable) {
			// Not provisioned. On the restricted-token tier the marker check has
			// already refused a command whose opt-in disagrees with setup, so reaching
			// here means the unelevated tier, which validates no marker at all and
			// cannot provision an account (that needs Administrator). Falling back is
			// right — refusing would break every machine-wide opt-in that relies on
			// the unelevated tier — but it must not be silent.
			warnWindowsSandboxPrincipalNotUsed("no sandbox principal is provisioned for this workspace; `zero sandbox setup` from an elevated (Administrator) terminal provisions one")
			return 0, false, nil
		}
		// The name resolves to something that is not a usable principal, most
		// likely squatted by a local group or alias. That is a conflict an
		// operator has to see, not a reason to pretend setup never ran.
		return 0, false, err
	}
	// Selecting the offline account is only half of what denies it the network.
	// The WFP filters match the OFFLINE GROUP'S SID, so an account that has
	// drifted out of that group — local policy, an administrator, a re-setup that
	// could not re-add it — still logs on from a stored secret and its token no
	// longer satisfies the filter condition. It would get full egress under a
	// profile that asked for none. Re-check the membership the mode depends on
	// rather than trusting the marker written when setup last succeeded.
	if role == windowsSandboxRoleOffline {
		member, err := windowsSandboxUserInLocalGroupFn(identity.Username, windowsSandboxOfflineGroupName)
		if err != nil {
			return 0, false, err
		}
		if !member {
			// Fail closed toward the weaker-but-still-denied path: the restricted
			// token carries the offline marker the same filters match, so egress
			// stays blocked. Handing back this principal token would not.
			warnWindowsSandboxOfflineMembershipMissing(identity.Username)
			return 0, false, nil
		}
	}
	secretPath, err := windowsSandboxSecretPath(config.SandboxHome, identity.Username)
	if err != nil {
		return 0, false, err
	}
	password, err := readWindowsSandboxSecretFn(secretPath)
	if err != nil {
		if errors.Is(err, errWindowsSandboxIdentityUnavailable) {
			// The account exists but its password does not. Setup was interrupted
			// or the secret was removed; fall back rather than fail the command.
			//
			// Falling back is right, staying quiet about it was not. The opt-in is
			// set and an account IS provisioned, so the operator asked for
			// principal isolation and is silently getting the weaker same-user
			// restricted token instead. That is the one fail-soft case worth
			// announcing: the others mean the backend was never set up, while this
			// one means it was and has broken since.
			warnWindowsSandboxPrincipalUnavailable(identity.Username)
			return 0, false, nil
		}
		return 0, false, err
	}
	token, err := logonWindowsSandboxPrincipal(identity.Username, password)
	if err != nil {
		// Provisioned but unusable. Surface it: a wrong password or a revoked
		// batch-logon right is a broken sandbox, not an absent one.
		return 0, false, err
	}
	return token, true, nil
}

// warnWindowsSandboxPrincipalUnavailable tells the operator once per process
// that the backend they opted into is not the one running.
//
// Once, because this sits on the command path: a warning per command would be
// noise on every tool call for the whole session, and noise that repeats gets
// filtered out by the reader rather than acted on. Indirected through a var so a
// test can observe it without capturing stderr.
var warnWindowsSandboxPrincipalUnavailable = func(username string) {
	windowsSandboxPrincipalWarnOnce.Do(func() {
		fmt.Fprintf(os.Stderr,
			"[zero] %s is set and sandbox principal %q is provisioned, but its stored password is missing or unreadable. "+
				"Falling back to the restricted-token sandbox, which does not confine reads. "+
				"Re-run `zero sandbox setup` from an elevated terminal to restore it.\n",
			windowsSandboxIdentityEnv, username)
	})
}

var windowsSandboxPrincipalWarnOnce sync.Once

// warnWindowsSandboxPrincipalNotUsed covers the other ways an opted-in command
// ends up on the restricted token: the principal is ineligible for this
// command's policy, or none is provisioned on a tier that validates no marker.
// Neither is an error — both are correct fallbacks — but both leave the operator
// believing an account boundary is isolating them when it is not, which is the
// one thing this backend must never do quietly.
//
// Once per process and behind its own sync.Once, so it neither silences nor is
// silenced by the provisioned-but-secretless warning above.
var warnWindowsSandboxPrincipalNotUsed = func(reason string) {
	windowsSandboxPrincipalNotUsedWarnOnce.Do(func() {
		fmt.Fprintf(os.Stderr,
			"[zero] %s is set, but this command is not running as a sandbox principal: %s. "+
				"Falling back to the restricted-token sandbox, which does not confine reads.\n",
			windowsSandboxIdentityEnv, reason)
	})
}

var windowsSandboxPrincipalNotUsedWarnOnce sync.Once

// Seamed so the dual-role setup rollback can be exercised without an elevated
// machine. The contract under test is which principals the outer rollback is
// willing to delete, which is a data-loss decision and the one thing here worth
// proving rather than reasoning about.
var (
	provisionWindowsSandboxPrincipalForSetupFn = provisionWindowsSandboxPrincipalForSetup
	removeWindowsSandboxPrincipalForSetupFn    = removeWindowsSandboxPrincipalForSetup
	// The both-roles retirement setup runs when the opt-in is off. It belongs
	// beside its single-role sibling for the reason recorded below: one seam
	// declared in two places lets a test stub one while production uses the
	// other.
	removeWindowsSandboxPrincipalsForSetupFn = removeWindowsSandboxPrincipalsForSetup
	// applyWindowsACLPlanFn lives with the other identity seams in
	// windows_identity_windows.go — the ACL-ordering guarantee it exists for
	// predates the dual-role split, and two declarations of the same seam would
	// let a test stub one while production used the other.
)

// provisionWindowsSandboxPrincipalForSetup does the elevated half: create the
// account, grant it the batch logon right, and store its password locked to the
// invoking user. Called from `zero sandbox setup`.
//
// The password is written BEFORE the caller applies any ACL plan, so a setup
// that fails partway leaves a principal that can at least be logged on and
// therefore cleaned up, rather than an account nothing holds the secret for.
func provisionWindowsSandboxPrincipalForSetup(config WindowsSandboxCommandConfig, role windowsSandboxRole) (windowsSandboxIdentity, bool, error) {
	key := windowsSandboxPrincipalKey(config)
	identity, password, created, err := provisionWindowsSandboxIdentityFn(key, role)

	// Undo whatever this run actually did, in reverse, on any failure after the
	// account exists. Without it a failure between creating the account and
	// storing its secret left the account behind with no caller able to remove
	// it: the rollback the setup path installs is only built once this function
	// has returned successfully.
	//
	// Scoped to what THIS run created on purpose. An account that already existed
	// and belongs to Zero is a working principal from an earlier setup, and
	// deleting it because a later run failed would turn a partial failure into a
	// total one.
	rightsAttempted := false
	rotated := false
	// Resolved from the account name rather than the identity, so it is known
	// before anything can fail.
	secretPath, secretPathErr := windowsSandboxSecretPath(config.SandboxHome, windowsSandboxUserName(key, role))
	undo := func() error {
		// Only when this run invalidated it. The secret is removed if this run
		// created the account, or if it rotated an existing account's password,
		// because in both cases what is on disk cannot authenticate and absent
		// beats stale: the command path treats a missing secret as "not
		// provisioned" and falls back, while a stale one fails the logon and
		// reports a broken sandbox.
		//
		// Removing it unconditionally, as this used to, destroyed a WORKING
		// secret whenever setup failed before rotation on a machine that was
		// already provisioned. The account kept its old password, the only copy
		// of it was deleted, and the sandbox silently degraded.
		//
		// This one failure is reported rather than swallowed. The others below
		// leave residue; this one leaves a CREDENTIAL for a password that no
		// longer works, which is the stale-secret state the invariant above
		// exists to prevent. Staying quiet would claim the invariant was restored
		// when it was not, and the operator would instead meet a
		// provisioned-but-unusable principal on the next command.
		var cleanupErr error
		if secretPath != "" && (created || rotated) {
			if err := removeWindowsSandboxSecretFn(secretPath); err != nil {
				cleanupErr = fmt.Errorf(
					"sandbox secret %s is stale and could not be removed, so the next command will "+
						"fail to log the principal on rather than falling back; delete it and re-run "+
						"`zero sandbox setup`: %w", secretPath, err)
			}
		}
		// Only for an account this run created, and attempted rather than
		// completed.
		//
		// Attempted, because grantWindowsSandboxLogonRights adds rights one at a
		// time and returns on the first failure, so a partial grant is possible
		// and gating on success left those entries behind, keyed to a SID that
		// deleting the account then made unresolvable.
		//
		// Created, because revokeWindowsSandboxLogonRights passes AllRights, which
		// drops every right the account holds and deletes its LSA object outright.
		// On an adopted principal that is not a rollback, it is destruction: a
		// transient failure anywhere below would strip the SeBatchLogonRight and
		// deny-logon rights a previous setup established, leaving exactly the
		// broken-but-present principal this whole function exists to avoid. The
		// rights this run granted are the ones the account is supposed to have, so
		// leaving them in place on an adopted account is the safe direction.
		if identity.SID != nil && rightsAttempted && created {
			_ = revokeWindowsSandboxLogonRightsFn(identity.SID)
		}
		if created {
			_ = removeWindowsSandboxIdentityFn(identity.Username, key)
		}
		return cleanupErr
	}

	if err != nil {
		// provisionWindowsSandboxIdentity can fail after creating the account, so
		// this path needs the same cleanup even though nothing below ran.
		return windowsSandboxIdentity{}, false, errors.Join(err, undo())
	}
	rightsAttempted = true
	if err := grantWindowsSandboxLogonRightsFn(identity.SID); err != nil {
		return windowsSandboxIdentity{}, false, errors.Join(err, undo())
	}
	if secretPathErr != nil {
		return windowsSandboxIdentity{}, false, errors.Join(secretPathErr, undo())
	}
	// Rotation happens HERE, immediately before the secret is committed, rather
	// than inside provisioning where it used to.
	//
	// A new account already has this password from NetUserAdd, so only an
	// adopted one needs setting. Doing it at the top of provisioning meant every
	// step in between ran with the account's password already replaced and no
	// copy of it stored, so any failure there stranded a working principal. The
	// two operations are now adjacent, which is the smallest window this can
	// have without a way to restore the previous password, which Windows does
	// not offer.
	if !created {
		if err := resetWindowsSandboxUserPasswordFn(identity.Username, password); err != nil {
			return windowsSandboxIdentity{}, false, errors.Join(err, undo())
		}
		rotated = true
	}
	if err := writeWindowsSandboxSecretFn(secretPath, password); err != nil {
		return windowsSandboxIdentity{}, false, errors.Join(err, undo())
	}
	return identity, created, nil
}

// setupWindowsSandboxPrincipal provisions this workspace's principal and grants
// it the filesystem access its permission profile describes. It returns a
// rollback that undoes everything it created, so a later setup step failing does
// not leave a half-provisioned account behind.
//
// Rollback order is the inverse of creation and matters: ACEs are revoked BEFORE
// the account is deleted, because removing the account first would leave ACEs
// naming a SID that no longer resolves, which is the orphaned-entry residue this
// model exists to avoid.
func setupWindowsSandboxPrincipal(config WindowsSandboxCommandConfig) (func() error, error) {
	key := windowsSandboxPrincipalKey(config)
	// Both roles are provisioned regardless of the profile this setup ran with.
	// Setup needs elevation and a command does not, so a command whose network
	// mode differs from the one setup happened to see must still find a principal
	// waiting; provisioning lazily would mean an unelevated command discovering it
	// needs an account it cannot create.
	roles := windowsSandboxLiveRoles

	var undo []func() error
	rollback := func() error {
		// Unwind in reverse, and keep going after a failure so one broken step
		// cannot strand everything behind it. The first error is reported.
		var firstErr error
		for i := len(undo) - 1; i >= 0; i-- {
			if err := undo[i](); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}

	filesystem := config.PermissionProfile.FileSystem
	// Resolved once, before the loop: both principals need write access to the
	// same runtime tree.
	//
	// permissionProfileWithRuntime appends this root to WriteRoots on every
	// COMMAND and redirects HOME, GOCACHE, npm_config_cache and friends into it,
	// but it lives under the user cache rather than the workspace, so the profile
	// setup sees never contains it. On the restricted-token path that costs
	// nothing, since the child still runs as the caller. A principal is a separate
	// local account with none of those rights, so without this every npm install,
	// go build or pip install fails on a cache write with a bare ACCESS_DENIED and
	// nothing pointing at the sandbox as the cause.
	runtimeRoots, err := setupWindowsSandboxRuntimeRoot(config)
	if err != nil {
		return nil, err
	}

	for _, role := range roles {
		username := windowsSandboxUserName(key, role)
		// Retire a principal whose grants were never recorded, BEFORE provisioning
		// adopts it.
		//
		// This is the one case where the prior grant set is not empty but unknowable:
		// an account from an earlier setup exists, and nothing on Windows can
		// enumerate the paths whose DACL names its SID. Carrying on would revoke only
		// what the new plan happens to name and leave the rest — the fail-open the
		// record exists to close, reached on the single path where it cannot be ruled
		// out.
		//
		// Retiring is a real fix rather than a gesture because Windows never reuses a
		// deleted local account's RID: whatever ACEs survive name a principal that no
		// longer exists and grant access to nobody, and the SID minted below is one
		// no DACL on this machine can already carry. It also needs no new operator
		// action, which matters — there is no `zero sandbox teardown` to send anyone
		// to, so refusing here would strand the workspace instead of fixing it.
		//
		// Per role, like everything else in this loop: the two principals are
		// separate accounts with separate records, and one having lost its record
		// says nothing about the other.
		if _, recorded := readWindowsPrincipalACLLedger(config.SandboxHome, username); !recorded {
			if err := retireUnrecordedWindowsSandboxPrincipal(config, role); err != nil {
				_ = rollback()
				return nil, err
			}
		}
		identity, created, err := provisionWindowsSandboxPrincipalForSetupFn(config, role)
		if err != nil {
			_ = rollback()
			return nil, err
		}
		// Only for a principal this run created.
		//
		// Appending unconditionally meant that if the offline role adopted an
		// account that already existed and the online role, or any later step,
		// then failed, the outer rollback deleted a principal that was working
		// before setup started. Dual-role provisioning makes that likely rather
		// than unlucky: the first role usually succeeds, so there is almost
		// always something for a later failure to destroy. This mirrors the
		// contract provisioning already applies to its own inner rollback.
		if created {
			undo = append(undo, func() error { return removeWindowsSandboxPrincipalForSetupFn(config, role) })
		}

		// Both principals get the same filesystem access. They differ only in
		// network reach, so granting the offline one less would make an approved
		// network command see a different filesystem than an ordinary one.
		writeRoots := filesystem.WriteRoots
		for _, root := range runtimeRoots {
			writeRoots = append(append([]WritableRoot{}, writeRoots...), WritableRoot{Root: root})
		}
		// Revokes this trustee's existing ACEs on the paths the plan touches AND
		// on the paths this role's record names, before applying it, so a re-run
		// after narrowing a root does not leave the wider grant behind. Per role:
		// the two principals are separate trustees with separate records, and
		// revoking one must not disturb the other.
		revertACL, err := applyWindowsPrincipalACLs(config.SandboxHome, username, identity.SID.String(), filesystem, writeRoots)
		if err != nil {
			_ = rollback()
			return nil, err
		}
		// ACEs are reverted before the account they name is deleted, because
		// removing the account first leaves ACEs naming a SID that no longer
		// resolves, which is the orphaned residue this model exists to avoid.
		// Appending after the removal closure puts it earlier in the reverse
		// unwind, which is what gets that ordering.
		undo = append(undo, revertACL)
	}
	return rollback, nil
}

// retireUnrecordedWindowsSandboxPrincipal removes this role's principal when one
// exists, and does nothing when one does not.
//
// The absent case is the ordinary one and is not a problem: with no account
// there is nothing that could be holding an ACE, so a missing record is simply
// a machine where setup has not run yet.
//
// Scoped to one role. The other role is a separate account holding its own ACEs
// under its own record, so retiring both because one lost its record would
// destroy a principal there is nothing wrong with.
func retireUnrecordedWindowsSandboxPrincipal(config WindowsSandboxCommandConfig, role windowsSandboxRole) error {
	_, err := lookupWindowsSandboxIdentityFn(windowsSandboxPrincipalKey(config), role)
	if err != nil {
		if errors.Is(err, errWindowsSandboxIdentityUnavailable) {
			return nil
		}
		return err
	}
	return removeWindowsSandboxPrincipalForSetupFn(config, role)
}

// removeWindowsSandboxPrincipalForSetup retires one role's principal in the
// order that leaves nothing behind: secret, then ACEs, then LSA logon rights,
// then the account itself. Everything keyed to the SID has to go while the SID
// still resolves.
func removeWindowsSandboxPrincipalForSetup(config WindowsSandboxCommandConfig, role windowsSandboxRole) error {
	key := windowsSandboxPrincipalKey(config)
	username := windowsSandboxUserName(key, role)
	secretPath, err := windowsSandboxSecretPath(config.SandboxHome, username)
	if err != nil {
		return err
	}
	// A secret owned by another administrator's setup is not a reason to abandon
	// the rest of teardown. Aborting here left the account, its logon rights, its
	// ACEs and its ledger all installed because one file could not be unlinked,
	// which is the worst of both outcomes: the operator asked for removal, got a
	// failure, and kept a working principal.
	//
	// Remembered and reported at the end rather than swallowed, so the leftover
	// secret is visible to whoever has to clean it up.
	var secretErr error
	if err := removeWindowsSandboxSecret(secretPath); err != nil {
		if !errors.Is(err, errWindowsSandboxSecretNotOurs) {
			return err
		}
		secretErr = err
	}
	// Set when ACE revocation could not complete. Teardown continues regardless,
	// but the ledger is kept and the error surfaced, so the residue stays
	// findable instead of being silently orphaned.
	var revokeErr error
	// Drop the LSA account rights before the account itself. Deleting the account
	// first would leave its rights behind keyed to a SID that no longer resolves,
	// which is the same orphaned residue the trustee-keyed ACE revocation exists
	// to avoid. A principal that was never provisioned has no SID to resolve and
	// nothing to revoke, so that case is not an error.
	if identity, err := lookupWindowsSandboxIdentity(key, role); err == nil {
		// ACEs first, for the same reason: once the account is gone its SID stops
		// resolving and every ACE naming it becomes an orphaned raw-SID entry on
		// the user's own tree, which is precisely the residue the capability-SID
		// model left behind and this one exists to avoid. Revocation is by
		// trustee, so it clears grants written by older versions too, and it is
		// scoped to THIS role's principal — the other role is a separate trustee
		// whose ACEs must survive one role being retired.
		//
		// Failing to revoke is not fatal. A path the user has since deleted or
		// renamed cannot be cleaned, and refusing to remove the account over it
		// would strand the principal and its logon rights permanently — a worse
		// outcome than a leftover ACE on a path that may not exist any more.
		//
		// The rollback is discarded here on purpose, unlike at setup: this is
		// teardown, the account is about to be deleted, and putting its ACEs back
		// is the opposite of what the caller asked for.
		//
		// The ERROR is not discarded, though it used to be. Teardown carried on
		// and reported success, which meant a failed revocation left ACEs naming
		// this SID on the user's tree while the ledger recording which paths they
		// sat on was deleted moments later: residue nothing could find again.
		// Remembered below rather than returned here, so removing the account
		// still happens and the principal is not stranded.
		//
		// The revocation is per ROLE: each workspace has two principals and they
		// are separate trustees, so the ledger has to be asked for this one's
		// paths rather than the workspace's.
		paths, pathsErr := windowsPrincipalRevocationPaths(config, identity.SID.String(), role)
		if pathsErr != nil {
			revokeErr = fmt.Errorf("resolve the paths holding ACEs for sandbox principal %s: %w", username, pathsErr)
		} else if _, err := revokeWindowsPrincipalACEs(identity.SID.String(), paths); err != nil {
			revokeErr = fmt.Errorf("revoke ACEs for sandbox principal %s: %w", username, err)
		}
		if err := revokeWindowsSandboxLogonRights(identity.SID); err != nil {
			return err
		}
	} else if !errors.Is(err, errWindowsSandboxIdentityUnavailable) {
		return err
	}
	// A foreign account under our derived name is left in place deliberately, and
	// that is the goal state for teardown: no principal of ours exists under it.
	// Surfacing it as a teardown failure would make setup rollback report an
	// error for having correctly declined to delete somebody else's account.
	if err := removeWindowsSandboxIdentity(username, key); err != nil {
		if !errors.Is(err, errWindowsSandboxForeignAccountRetained) {
			return err
		}
		// The record is KEPT in that case. It names paths this role's principal
		// was granted, and a foreign account holding the name is not evidence
		// those grants are gone — dropping the record would leave them
		// unrevokable, which is the whole failure it exists to prevent. A record
		// with no principal is revoked harmlessly by the next setup.
		return nil
	}
	// Otherwise last, and only once the account is actually gone, so a failure
	// anywhere above leaves the record describing a principal that still exists.
	//
	// It describes grants for a SID that no longer resolves, and leaving it would
	// have the next setup revoke those paths on behalf of a freshly minted SID
	// that never held them. That is a harmless no-op rather than a hole — the
	// deleted account's RID is never reused, but a record that outlives its
	// principal is a lie the next reader has no way to detect.
	//
	// Unless revocation failed. Then the ledger is the ONLY surviving record of
	// which paths still carry ACEs for this SID, and deleting it turns a
	// reportable leftover into permanent unfindable residue. Keeping a record
	// that outlives its principal is the lesser problem, and the error says so.
	if revokeErr != nil {
		return errors.Join(secretErr, fmt.Errorf("%w; the principal ACL ledger has been kept so the remaining ACEs can still be found", revokeErr))
	}
	// The account and its ACEs are gone either way. A secret left behind is
	// residue worth naming, not a reason to keep the ledger: there are no ACEs
	// left for it to describe.
	return errors.Join(secretErr, removeWindowsPrincipalACLLedger(config.SandboxHome, username))
}

// setupWindowsSandboxRuntimeRoot resolves this workspace's runtime root and
// makes sure it exists, so the principal ACL plan can name it.
//
// It is created here rather than left to the first command because
// applyWindowsACLPlan skips a target that does not exist: granting write on a
// directory that setup never made would silently no-op, and the failure would
// only show up later as a denied cache write. Creating it under the elevated
// setup process is safe, since it lives under the invoking user's own cache
// directory and prepareSandboxRuntime would create it on the same path anyway.
//
// An empty return means there is no runtime root to grant (no workspace root
// configured), which is not an error: the caller simply grants nothing extra.
func windowsSandboxRuntimeRootPath(config WindowsSandboxCommandConfig) ([]string, error) {
	// EVERY candidate, not just the cache-derived one.
	//
	// This used to take the deterministic cache-derived root alone and return
	// nothing when the cache sat inside the workspace, on the reasoning that the
	// other branch minted a random per-process directory through os.MkdirTemp and
	// so had no name both sides could agree on. That reasoning is now stale:
	// fallbackSandboxRuntimeRoot derives its path by hashing the workspace and
	// creates nothing, so every process reaches the same answer.
	//
	// Leaving it stale had a cost. Commands still SELECT the fallback in that
	// layout, and prepareSandboxRuntime redirects TMP, GOCACHE and the package
	// caches into it, while setup granted neither principal an ACE on it. A
	// principal command in a supported layout then failed ordinary cache writes
	// with a bare ACCESS_DENIED and nothing naming the sandbox as the cause.
	//
	// So this now answers with the same candidate set the capability plan already
	// grants, and for the same reason: setup covers both so a command lands on a
	// provisioned tree whichever one it picks.
	return windowsSandboxRuntimeCandidates(config.WorkspaceRoots), nil
}

// setupWindowsSandboxRuntimeRoot resolves the runtime root AND creates it.
// Teardown wants the name without the side effect, so the derivation lives in
// windowsSandboxRuntimeRootPath above and this only adds the mkdir.
func setupWindowsSandboxRuntimeRoot(config WindowsSandboxCommandConfig) ([]string, error) {
	roots, err := windowsSandboxRuntimeRootPath(config)
	if err != nil {
		return nil, err
	}
	// Every candidate is created, because setup grants an ACE on every candidate
	// and applyWindowsACLPlan fails the whole run on a target that does not exist.
	for _, root := range roots {
		// Built relative to handles rather than by pathname, for the reason the
		// secret directory is: this runs elevated over a tree the invoking user
		// owns, so a junction planted mid-walk turns an os.MkdirAll into an
		// elevated create wherever that junction points.
		created, parent, err := makeWindowsACLDirChainNoFollow(root)
		if parent != 0 {
			_ = windows.CloseHandle(parent)
		}
		if err != nil {
			if _, unwindErr := rollbackWindowsACLMaterialization(created); unwindErr != nil {
				err = errors.Join(err, unwindErr)
			}
			return nil, fmt.Errorf("create sandbox runtime root: %w", err)
		}
	}
	return roots, nil
}

// revokeWindowsPrincipalACEs drops every ACE naming principalSID on paths and
// returns a rollback that puts them back.
//
// A path that does not exist is skipped rather than failing: revocation is
// cleanup, and there is nothing to clean on a path that was never created.
func revokeWindowsPrincipalACEs(principalSID string, paths []string) (func() error, error) {
	if len(paths) == 0 {
		return func() error { return nil }, nil
	}
	plan, err := windowsPrincipalRevokePlan(principalSID, paths)
	if err != nil {
		return nil, err
	}
	return applyWindowsACLPlanFn(plan)
}

// applyWindowsPrincipalACLs writes the principal's ACEs for one policy: it
// revokes whatever this trustee already had on the paths the new plan touches
// AND on the paths an earlier setup recorded, then applies the plan.
//
// The order is the whole point. applyWindowsACLPlan MERGES into the existing
// DACL, so without the revocation first a re-run after narrowing a write root
// or shortening a deny list leaves the previous, wider ACEs beside the new ones
// and the principal keeps access the current policy no longer grants — the
// sandbox silently widens as a result of tightening it. Setup does get the
// chance to notice: marker validation refuses commands with "permission roots
// or deny lists changed" until setup runs again.
//
// The recorded paths are what makes that revocation complete. Revoking only the
// new plan's paths could never reach a root that had LEFT the policy, which is
// precisely the root whose ACE has to go: absent from the new plan, it was
// skipped, so the re-setup that was supposed to resolve the widening preserved
// it instead.
//
// Revocation is by TRUSTEE, so it drops every ACE naming this principal on
// these paths whatever an older version of Zero granted, and a recorded path
// that no longer exists or never held an ACE costs nothing.
func applyWindowsPrincipalACLs(sandboxHome string, username string, principalSID string, filesystem FileSystemPolicy, writeRoots []WritableRoot) (func() error, error) {
	plan, err := buildWindowsPrincipalACLPlan(windowsPrincipalACLInput{
		PrincipalSID: principalSID,
		WriteRoots:   writeRoots,
		ReadRoots:    filesystem.ReadRoots,
		DenyRead:     filesystem.DenyRead,
		DenyWrite:    filesystem.DenyWrite,
	})
	if err != nil {
		return nil, err
	}
	granted := windowsACLPlanPaths(plan)
	// An unreadable record arrives here as an empty prior set, which taken alone
	// would be the fail-open. It cannot be reached: setupWindowsSandboxPrincipal
	// retires any principal whose record is missing BEFORE provisioning, so by
	// this point either the record is trustworthy or principalSID is one that no
	// DACL on this machine has ever been able to name.
	recorded, _ := readWindowsPrincipalACLLedger(sandboxHome, username)
	stale := unionWindowsPrincipalACLPaths(recorded, granted)

	// Recorded BEFORE a single DACL changes, and as the union rather than the new
	// set. A crash between the grant below and the narrowing write would otherwise
	// leave a record that omits paths this run granted, and the next policy change
	// would strand them in exactly the way this fix exists to prevent. A superset
	// is the safe direction to be wrong in: revoking a path that holds no ACE for
	// this trustee is a no-op.
	if err := writeWindowsPrincipalACLLedger(sandboxHome, username, stale); err != nil {
		return nil, err
	}

	// The revocation's own rollback matters, and discarding it was a real bug.
	//
	// It was discarded on the reasoning that the only failure path from here
	// removes the principal outright, so restoring stale ACEs for an account
	// about to be deleted would be pointless. That holds for a principal this
	// run CREATED. It is false for one this run ADOPTED: setup keeps a
	// pre-existing account on failure rather than destroying someone else's
	// working principal, so discarding the snapshot left that account alive with
	// its previous ACEs stripped and the new ones rolled back — logged on and
	// unable to reach its own workspace.
	//
	// Restoring the pre-revocation DACL first, then the grant, unwinds in the
	// reverse order they were applied.
	restoreRevoked, err := revokeWindowsPrincipalACEs(principalSID, stale)
	if err != nil {
		return nil, err
	}
	revertGrant, err := applyWindowsACLPlanFn(plan)
	if err != nil {
		if restoreRevoked != nil {
			_ = restoreRevoked()
		}
		return nil, err
	}
	// Narrow the record to what is granted now, so it tracks the policy instead
	// of accumulating every root the workspace has ever had.
	//
	// A failure here fails the setup rather than being shrugged off. The same
	// file was written successfully moments ago, so failing now means the sandbox
	// home has stopped being writable, and reporting a provisioned sandbox on a
	// sandbox home that cannot hold its own state is the kind of quiet this
	// backend must not have. Unwinding leaves the union recorded, which is the
	// safe direction.
	if err := writeWindowsPrincipalACLLedger(sandboxHome, username, granted); err != nil {
		if revertErr := revertGrant(); revertErr != nil {
			err = errors.Join(err, revertErr)
		}
		if restoreRevoked != nil {
			if restoreErr := restoreRevoked(); restoreErr != nil {
				err = errors.Join(err, restoreErr)
			}
		}
		return nil, err
	}
	return func() error {
		grantErr := revertGrant()
		// Restore the pre-revocation ACEs even when reverting the grant failed:
		// leaving the principal with neither set is the state this exists to
		// avoid. Report the grant error, since that is the one leaving residue.
		var restoreErr error
		if restoreRevoked != nil {
			restoreErr = restoreRevoked()
		}
		// THE LEDGER HAS TO COME BACK TOO.
		//
		// The narrowed set was written above only once everything had succeeded.
		// This closure puts the old ACEs back, so leaving that narrowed ledger in
		// place describes a principal holding ACEs the ledger does not name, and
		// a later setup or teardown reads the ledger to decide what to revoke.
		// The restored paths would never be revisited: access outside the current
		// policy, held by an account nothing knows to clean up.
		//
		// The union is what was recorded before any DACL changed, so writing it
		// back returns the ledger to the state the restored ACEs belong to.
		// Over-recording is the safe direction: a path named but no longer held
		// costs one redundant revoke, while a path held but unnamed is residue
		// nothing can find.
		if ledgerErr := writeWindowsPrincipalACLLedger(sandboxHome, username, stale); ledgerErr != nil {
			restoreErr = errors.Join(restoreErr, ledgerErr)
		}
		if grantErr != nil {
			return grantErr
		}
		return restoreErr
	}, nil
}

// windowsPrincipalTeardownPaths names every path this principal could hold an
// ACE on: the policy's roots plus the per-workspace runtime tree.
//
// The runtime root is derived through the same resolver setup uses, so teardown
// revokes ACEs on the tree commands actually wrote to. That was not possible
// while the fallback minted a random per-process directory: nothing here could
// name it, so an opted-out machine kept principal ACEs on whatever tree the
// fallback had produced. Both branches are creation-free now, so teardown can
// ask for the real answer without materializing anything on its way out.
func windowsPrincipalTeardownPaths(config WindowsSandboxCommandConfig, principalSID string) ([]string, error) {
	filesystem := config.PermissionProfile.FileSystem
	writeRoots := filesystem.WriteRoots
	runtimeRoots, err := windowsSandboxRuntimeRootPath(config)
	if err != nil {
		return nil, err
	}
	// Every candidate, matching what setup granted: teardown that revoked only
	// one would leave principal ACEs on the tree commands actually used.
	for _, root := range runtimeRoots {
		writeRoots = append(append([]WritableRoot{}, writeRoots...), WritableRoot{Root: root})
	}
	plan, err := buildWindowsPrincipalACLPlan(windowsPrincipalACLInput{
		PrincipalSID: principalSID,
		WriteRoots:   writeRoots,
		ReadRoots:    filesystem.ReadRoots,
		DenyRead:     filesystem.DenyRead,
		DenyWrite:    filesystem.DenyWrite,
	})
	if err != nil {
		return nil, err
	}
	return windowsACLPlanPaths(plan), nil
}

// windowsPrincipalRevocationPaths is what teardown actually has to revoke: the
// paths the CURRENT policy describes, plus every path an earlier setup recorded.
//
// Teardown used the current policy alone, which reproduced the setup-side bug on
// the way out. A root the user removed from their policy is missing from today's
// plan, so retiring the principal revoked every ACE except the one that was
// widening the sandbox — and then deleted the account, leaving that ACE naming a
// SID nothing could resolve to clean it up later.
func windowsPrincipalRevocationPaths(config WindowsSandboxCommandConfig, principalSID string, role windowsSandboxRole) ([]string, error) {
	current, err := windowsPrincipalTeardownPaths(config, principalSID)
	if err != nil {
		return nil, err
	}
	// This role's record, not the workspace's: the other role is a separate
	// trustee whose recorded paths are none of this revocation's business.
	recorded, _ := readWindowsPrincipalACLLedger(
		config.SandboxHome, windowsSandboxUserName(windowsSandboxPrincipalKey(config), role))
	return unionWindowsPrincipalACLPaths(recorded, current), nil
}

// Seams for the two elevated calls the provisioning rollback depends on, so the
// stale-secret recovery path is reachable in tests without an elevated machine.
var (
	provisionWindowsSandboxIdentityFn = provisionWindowsSandboxIdentity
	removeWindowsSandboxSecretFn      = removeWindowsSandboxSecret
	writeWindowsSandboxSecretFn       = writeWindowsSandboxSecret
)

// warnWindowsSandboxOfflineMembershipMissing reports the one drift that would
// otherwise silently hand a no-network profile full egress.
//
// Separate from the missing-secret warning because the remedy differs and
// because this one is a containment failure rather than a setup gap: the
// account is fine, its group membership is not. Once per process, for the same
// reason as its sibling — this sits on the command path.
var warnWindowsSandboxOfflineMembershipMissing = func(username string) {
	windowsSandboxOfflineMembershipWarnOnce.Do(func() {
		fmt.Fprintf(os.Stderr,
			"[zero] sandbox principal %q is no longer a member of %q, which is the group the network "+
				"block filters match. Falling back to the restricted-token sandbox, which still denies "+
				"the network but does not confine reads. "+
				"Re-run `zero sandbox setup` from an elevated terminal to restore it.\n",
			username, windowsSandboxOfflineGroupName)
	})
}

var windowsSandboxOfflineMembershipWarnOnce sync.Once

// Seam for the principal lookup, so the command path's mode-enforcement checks
// are reachable in tests without a provisioned machine.
var lookupWindowsSandboxPrincipalForCommandFn = lookupWindowsSandboxPrincipalForCommand

// Seam for the secret read on the command path, so the mode-enforcement gates
// ahead of it can be exercised without a provisioned secret on disk.
var readWindowsSandboxSecretFn = readWindowsSandboxSecret

// removeWindowsSandboxIdentityFn is the rollback's account deletion. A seam so a
// fixture that fakes creation can also fake its inverse: a mocked provisioning
// test that reports "created" and then injects a failure must not reach the
// real NetUserDel on an account it never made.
var removeWindowsSandboxIdentityFn = removeWindowsSandboxIdentity

// Seam for the lookup the unrecorded-principal retirement decides on, so that
// decision is observable in a test without a provisioned machine — on which the
// lookup declines for its own reasons and would report success whether or not
// the guard existed.
//
// The retirement's other elevated call, removeWindowsSandboxPrincipalForSetup,
// is seamed with the dual-role rollback seams above rather than here, for the
// reason recorded there: two declarations of one seam let a test stub one while
// production uses the other.
var lookupWindowsSandboxIdentityFn = lookupWindowsSandboxIdentity

// windowsACLPlanPaths returns each distinct path a plan touches, in plan order.
//
// Windows-tagged deliberately: every caller is, so defining it in the portable
// file made it unused on Linux and macOS builds and failed static analysis.
func windowsACLPlanPaths(plan WindowsACLPlan) []string {
	seen := make(map[string]struct{}, len(plan.Entries))
	paths := make([]string, 0, len(plan.Entries))
	for _, entry := range plan.Entries {
		if _, ok := seen[entry.Path]; ok {
			continue
		}
		seen[entry.Path] = struct{}{}
		paths = append(paths, entry.Path)
	}
	return paths
}

// removeWindowsSandboxPrincipalsForSetup retires BOTH of a workspace's
// principals.
//
// Each workspace has an offline and an online account, and they are separate
// trustees with their own secret, logon rights, ACEs and ledger entries.
// Retiring one and calling it done is what the opt-out path is there to
// prevent: the marker would flip to opted-out while the other account and
// everything it owns stayed on the machine, invisible, because nothing
// afterwards looks for a principal it believes was never provisioned.
//
// Both are attempted even if the first fails, and the errors are joined, so one
// stubborn account cannot hide residue left by the other.
// windowsSandboxRoleLegacy is retired alongside the two live roles. A machine
// set up before the roles were split holds one untagged "zero-sbx-<key>"
// account, and neither role name resolves to it, so every other path on this
// branch reports it as never provisioned. Opting out or re-running setup would
// otherwise leave that account, its secret, its logon rights, its ACEs and its
// ledger installed and unreferenced. Retirement is idempotent and a principal
// that was never provisioned is not an error, so this costs a lookup on the
// machines that never had one.
func removeWindowsSandboxPrincipalsForSetup(config WindowsSandboxCommandConfig) error {
	var errs []error
	for _, role := range windowsSandboxRetirableRoles {
		// Through the seam, like retireUnrecordedWindowsSandboxPrincipal, so the
		// set of roles this retires is assertable without provisioning real
		// accounts on the machine running the tests.
		if err := removeWindowsSandboxPrincipalForSetupFn(config, role); err != nil {
			errs = append(errs, fmt.Errorf("retire the %s sandbox principal: %w", role, err))
		}
	}
	return errors.Join(errs...)
}

// windowsCurrentUserSID returns the SID of the user this process runs as, or
// empty when the token cannot be read. Elevation does not change it: a UAC
// consent prompt splits the caller's token but keeps the user, so this differs
// from the caller's SID only when a DIFFERENT account supplied the credentials
// (over-the-shoulder elevation, or `runas /user:`).
func windowsCurrentUserSID() string {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return ""
	}
	return user.User.Sid.String()
}

// windowsSandboxPrincipalKey derives the key a principal ACCOUNT is named after,
// scoped to the invoking user as well as the workspace.
//
// The workspace alone is not enough. The account name is machine-wide, but its
// DPAPI secret and its ACL ledger both live under the invoking user's sandbox
// home, so two Windows users sharing one workspace path derive the same account
// while keeping private bookkeeping for it. The second user finds an account
// with no ledger of their own and either retires the first user's working
// principal or adopts it and rotates its password while storing only their own
// secret. Either way the first user's marker still validates and their next
// command cannot log on.
//
// Deliberately NOT used for the setup lock, which stays keyed to the workspace
// alone. Two users setting up one shared workspace still write DACLs on the same
// paths, so they must continue to serialize against each other even though they
// now provision separate accounts.
//
// The invoking user is config.CallerSID when the caller supplied one, and only
// then the current process token. Those are the same identity for an ordinary
// command, which runs as the caller. They are NOT the same inside elevated
// setup: over-the-shoulder UAC runs the helper as whichever administrator typed
// the credentials, so sampling its own token here named the account after the
// administrator while every later command derived the ordinary user's name,
// found nothing, and fell back to the restricted token — leaving an admin-keyed
// account, secret and ledger that nothing afterwards would ever reclaim.
//
// Falls back to the workspace-only key when neither is available. That is the
// pre-existing behaviour rather than a new failure mode, and refusing to derive
// a name at all would break setup on a machine whose token query fails for
// unrelated reasons.
func windowsSandboxPrincipalKey(config WindowsSandboxCommandConfig) string {
	workspace := windowsSandboxWorkspaceKey(config.WorkspaceRoots)
	sid := strings.TrimSpace(config.CallerSID)
	if sid == "" {
		sid = windowsCurrentUserSID()
	}
	if sid == "" {
		return workspace
	}
	digest := sha256.Sum256([]byte(workspace + "\x00" + sid))
	return hex.EncodeToString(digest[:])
}
