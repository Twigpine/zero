//go:build windows

package sandbox

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

// windowsSetupSeams stubs every external effect of the setup transaction so a
// test can drive its control flow on an ordinary machine. Each seam defaults to
// a benign success, and a test overrides only the one it is about.
type windowsSetupSeams struct {
	elevated bool
	// principalStillInstalled is what the account lookup reports AFTER retirement
	// ran. It is the whole of the opt-out branch's fatality rule, so it is stated
	// separately from retireErr rather than inferred from it.
	principalStillInstalled bool
	aclRollback             func() error
	retireErr               error
	provisionErr            error
	networkErr              error
	markerErr               error
	rollbackCalled          *bool
	markerWritten           *bool
}

func withWindowsSetupSeams(t *testing.T, seams windowsSetupSeams) {
	t.Helper()

	originalElevated := windowsProcessIsElevatedFn
	originalLock := acquireWindowsSandboxSetupLockFn
	originalACL := applyWindowsACLPlanFn
	originalPrincipal := setupWindowsSandboxPrincipalFn
	originalRetire := removeWindowsSandboxPrincipalsForSetupFn
	originalGroupSID := resolveWindowsSandboxOfflineGroupSIDHook
	originalLookup := lookupWindowsSandboxIdentityFn
	originalNetwork := applyWindowsNetworkPlanFn
	originalMarker := writeWindowsSandboxSetupMarkerFn
	t.Cleanup(func() {
		lookupWindowsSandboxIdentityFn = originalLookup
		windowsProcessIsElevatedFn = originalElevated
		acquireWindowsSandboxSetupLockFn = originalLock
		applyWindowsACLPlanFn = originalACL
		setupWindowsSandboxPrincipalFn = originalPrincipal
		removeWindowsSandboxPrincipalsForSetupFn = originalRetire
		resolveWindowsSandboxOfflineGroupSIDHook = originalGroupSID
		applyWindowsNetworkPlanFn = originalNetwork
		writeWindowsSandboxSetupMarkerFn = originalMarker
	})

	// The launch seam says no on every machine today; these fixtures test the
	// provisioning flow behind it, so they open it and restore it after.
	originalLaunch := windowsPrincipalLaunchAvailable
	windowsPrincipalLaunchAvailable = func() error { return nil }
	t.Cleanup(func() { windowsPrincipalLaunchAvailable = originalLaunch })
	windowsProcessIsElevatedFn = func() bool { return seams.elevated }
	// A zero lock is safe to release: release() tolerates a nil handle and
	// runtime.UnlockOSThread is a no-op when the thread was never pinned.
	acquireWindowsSandboxSetupLockFn = func(string) (*windowsSandboxSetupLock, error) {
		return &windowsSandboxSetupLock{}, nil
	}
	applyWindowsACLPlanFn = func(WindowsACLPlan) (func() error, error) {
		return func() error {
			if seams.rollbackCalled != nil {
				*seams.rollbackCalled = true
			}
			if seams.aclRollback != nil {
				return seams.aclRollback()
			}
			return nil
		}, nil
	}
	setupWindowsSandboxPrincipalFn = func(WindowsSandboxCommandConfig) (func() error, error) {
		if seams.provisionErr != nil {
			return nil, seams.provisionErr
		}
		return func() error { return nil }, nil
	}
	removeWindowsSandboxPrincipalsForSetupFn = func(WindowsSandboxCommandConfig) error {
		return seams.retireErr
	}
	// Dual-role setup asserts the block filters name the offline group, which a
	// stubbed provisioning never creates.
	resolveWindowsSandboxOfflineGroupSIDHook = func() (string, error) { return "S-1-5-32-999", nil }
	lookupWindowsSandboxIdentityFn = func(string, windowsSandboxRole) (windowsSandboxIdentity, error) {
		if seams.principalStillInstalled {
			return windowsSandboxIdentity{Username: "zero-sbx-stub"}, nil
		}
		return windowsSandboxIdentity{}, errWindowsSandboxIdentityUnavailable
	}
	applyWindowsNetworkPlanFn = func(WindowsNetworkPlan) error { return seams.networkErr }
	writeWindowsSandboxSetupMarkerFn = func(config WindowsSandboxSetupConfig) (WindowsSandboxSetupMarker, error) {
		if seams.markerWritten != nil {
			*seams.markerWritten = true
		}
		if seams.markerErr != nil {
			return WindowsSandboxSetupMarker{}, seams.markerErr
		}
		return BuildWindowsSandboxSetupMarker(config)
	}
}

func windowsSetupTestConfig(t *testing.T, optIn bool) WindowsSandboxSetupConfig {
	t.Helper()
	root := t.TempDir()
	return WindowsSandboxSetupConfig{
		SandboxHome:    t.TempDir(),
		CommandCWD:     root,
		WorkspaceRoots: []string{root},
		PermissionProfile: PermissionProfile{
			FileSystem: FileSystemPolicy{Kind: FileSystemRestricted, WriteRoots: []WritableRoot{{Root: root}}},
			Network:    NetworkPolicy{Mode: NetworkDeny},
		},
		PrincipalOptIn: optIn,
	}
}

// windowsForeignSID is a well-formed account SID that no test machine's own
// process can be running as.
const windowsForeignSID = "S-1-5-21-1111111111-2222222222-3333333333-1001"

// The finding: opting out printed the retirement failure and carried on to write
// an opted-out marker and exit 0.
//
// That is the one outcome this branch must never produce. Teardown is
// idempotent, so a failure here means a real account, secret, logon right, ACE
// set and ledger are still installed — and the marker now says none of them
// exist, so nothing afterwards will ever look for them again.
//
// Asserting on the marker as well as the exit code is deliberate: an exit code
// alone would still pass if the marker had already been written, and the marker
// is what makes the residue permanent.
func TestOptOutRetirementFailureFailsSetupAndWritesNoMarker(t *testing.T) {
	markerWritten := false
	rollbackCalled := false
	withWindowsSetupSeams(t, windowsSetupSeams{
		elevated:                true,
		retireErr:               errors.New("sandbox principal zero-sbx-abc could not be deleted"),
		principalStillInstalled: true,
		markerWritten:           &markerWritten,
		rollbackCalled:          &rollbackCalled,
	})

	var stderr bytes.Buffer
	config := windowsSetupTestConfig(t, false)
	if code := runWindowsSandboxSetup(config, &stderr); code == 0 {
		t.Fatal("setup reported success after failing to retire an existing sandbox principal, so the operator is told the machine is clean while the account is still installed")
	}
	if markerWritten {
		t.Error("setup wrote an opted-out marker after the retirement failed, which is what makes the leftover principal permanently invisible")
	}
	if !rollbackCalled {
		t.Error("setup failed without undoing the ACL grants it had already applied")
	}
	if !strings.Contains(stderr.String(), "could not be deleted") {
		t.Errorf("the underlying retirement error must reach the operator, got %q", stderr.String())
	}
	if _, err := os.Stat(WindowsSandboxSetupMarkerPath(config.SandboxHome)); !os.IsNotExist(err) {
		t.Errorf("no marker file should exist on disk after a failed setup, stat error = %v", err)
	}
}

// The other side of that rule, and a regression I introduced fixing the first
// one: failing on ANY retirement error is wrong.
//
// removeWindowsSandboxPrincipalForSetup deliberately carries on past a secret
// file owned by another administrator's setup (errWindowsSandboxSecretNotOurs
// goes into secretErr and is reported at the end via errors.Join) so one
// unlinkable file cannot strand the account, its logon rights and its ACEs. The
// account is therefore already gone when that error is returned, and treating it
// as fatal made `zero sandbox setup` fail permanently on such a machine, taking
// the DEFAULT restricted-token sandbox down with it over inert residue.
//
// Setup must complete, and must still say what was left behind.
func TestOptOutSucceedsWhenOnlyInertResidueSurvivesRetirement(t *testing.T) {
	markerWritten := false
	rollbackCalled := false
	withWindowsSetupSeams(t, windowsSetupSeams{
		elevated: true,
		// What teardown reports when the only leftover is another administrator's
		// secret file: the account, its rights, its ACEs and its ledger are gone.
		retireErr:               errWindowsSandboxSecretNotOurs,
		principalStillInstalled: false,
		markerWritten:           &markerWritten,
		rollbackCalled:          &rollbackCalled,
	})

	var stderr bytes.Buffer
	if code := runWindowsSandboxSetup(windowsSetupTestConfig(t, false), &stderr); code != 0 {
		t.Fatalf("setup failed over residue left behind by an already-retired principal, which makes the default sandbox unsetupable on this machine: %s", stderr.String())
	}
	if !markerWritten {
		t.Error("setup that completed must write its marker")
	}
	if rollbackCalled {
		t.Error("setup rolled back its ACL work over residue it had already reported as survivable")
	}
	if !strings.Contains(stderr.String(), "residue") {
		t.Errorf("the leftover must still be named for whoever cleans it up, got %q", stderr.String())
	}
}

// The opt-out path still succeeds when retirement succeeds. Without this the
// test above would pass against a branch that always failed.
func TestOptOutSucceedsWhenRetirementSucceeds(t *testing.T) {
	markerWritten := false
	withWindowsSetupSeams(t, windowsSetupSeams{elevated: true, markerWritten: &markerWritten})

	var stderr bytes.Buffer
	if code := runWindowsSandboxSetup(windowsSetupTestConfig(t, false), &stderr); code != 0 {
		t.Fatalf("opting out with nothing to retire must succeed, got %d: %s", code, stderr.String())
	}
	if !markerWritten {
		t.Error("a successful setup must write its marker")
	}
}

// The other half of the caller-identity finding: a principal provisioned by an
// administrator who is not the caller can never be used, because its password is
// sealed to the user that stores it. Refuse before anything is created rather
// than leave an account, a secret and a grant nobody can reach.
func TestPrincipalSetupRefusesWhenElevatedAsAnotherUser(t *testing.T) {
	markerWritten := false
	withWindowsSetupSeams(t, windowsSetupSeams{elevated: true, markerWritten: &markerWritten})

	config := windowsSetupTestConfig(t, true)
	config.CallerSID = windowsForeignSID
	if current := windowsCurrentUserSID(); current == "" || strings.EqualFold(current, config.CallerSID) {
		t.Skipf("cannot distinguish the caller from this process (current SID %q)", current)
	}

	var stderr bytes.Buffer
	if code := runWindowsSandboxSetup(config, &stderr); code == 0 {
		t.Fatal("setup provisioned a sandbox principal for a caller whose secret it cannot seal, so every later command would find the account and fail to unseal its password")
	}
	if markerWritten {
		t.Error("setup wrote a marker for a principal it refused to provision")
	}
	if !strings.Contains(stderr.String(), "different Windows user") {
		t.Errorf("the refusal must say what is wrong and how to clear it, got %q", stderr.String())
	}
}

// The refusal is scoped to the principal opt-in. The default restricted-token
// sandbox stores no secret and names no account after the user, so it works
// perfectly well across this boundary and must keep doing so.
func TestRestrictedTokenSetupIsUnaffectedByACrossUserElevation(t *testing.T) {
	withWindowsSetupSeams(t, windowsSetupSeams{elevated: true})

	config := windowsSetupTestConfig(t, false)
	config.CallerSID = windowsForeignSID

	var stderr bytes.Buffer
	if code := runWindowsSandboxSetup(config, &stderr); code != 0 {
		t.Fatalf("the restricted-token sandbox does not depend on the invoking user and must still set up, got %d: %s", code, stderr.String())
	}
}

// An unknown caller SID is treated as a match. That is the pre-existing
// behaviour for an older caller or a token query that failed, and refusing on
// absence would break setup on machines where nothing is wrong.
func TestPrincipalSetupProceedsWhenTheCallerIsUnknown(t *testing.T) {
	withWindowsSetupSeams(t, windowsSetupSeams{elevated: true})

	var stderr bytes.Buffer
	if code := runWindowsSandboxSetup(windowsSetupTestConfig(t, true), &stderr); code != 0 {
		t.Fatalf("an unstated caller must not block setup, got %d: %s", code, stderr.String())
	}
}

// The account name, its ledger and its ACEs are all derived from this key, and
// under over-the-shoulder UAC the elevated helper is a different user than the
// caller. Keying off the caller is what makes setup provision the account the
// caller's next command will actually look for.
func TestPrincipalKeyFollowsTheCallerNotTheCurrentProcess(t *testing.T) {
	roots := []string{`C:\ws\shared`}
	self := windowsSandboxPrincipalKey(WindowsSandboxCommandConfig{WorkspaceRoots: roots})
	caller := windowsSandboxPrincipalKey(WindowsSandboxCommandConfig{WorkspaceRoots: roots, CallerSID: windowsForeignSID})
	if self == caller {
		t.Fatal("the principal key ignored the caller SID, so elevated setup would provision an account named after the administrator who typed the credentials")
	}
	if again := windowsSandboxPrincipalKey(WindowsSandboxCommandConfig{WorkspaceRoots: roots, CallerSID: windowsForeignSID}); again != caller {
		t.Error("the caller-scoped key is not stable, so setup and the command half would derive different accounts")
	}
}

// THE HELPER REFUSES WHAT THE BUILDER REFUSES. An elevated same-caller run that
// hands the helper `--sandbox-principal 1` directly must stop before any
// account, secret or ACL effect while no launch path exists, exactly as
// BuildWindowsSandboxSetupArgs would have stopped it from being serialized.
func TestPrincipalSetupRefusesAtTheHelperWhileLaunchIsUnavailable(t *testing.T) {
	withWindowsSetupSeams(t, windowsSetupSeams{elevated: true})
	windowsPrincipalLaunchAvailable = func() error { return errWindowsPrincipalLaunchUnavailable }

	provisioned := false
	setupWindowsSandboxPrincipalFn = func(WindowsSandboxCommandConfig) (func() error, error) {
		provisioned = true
		return func() error { return nil }, nil
	}
	applied := false
	applyWindowsACLPlanFn = func(WindowsACLPlan) (func() error, error) {
		applied = true
		return func() error { return nil }, nil
	}

	var stderr bytes.Buffer
	if code := runWindowsSandboxSetup(windowsSetupTestConfig(t, true), &stderr); code == 0 {
		t.Fatalf("setup provisioned a principal that nothing can launch: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "refusing to provision a sandbox principal") {
		t.Fatalf("refusal did not say why: %s", stderr.String())
	}
	if provisioned || applied {
		t.Fatalf("the refusal came after an effect: provisioned=%v aclApplied=%v", provisioned, applied)
	}

	// CONTROL: the opt-out path does not consult launch availability at all.
	stderr.Reset()
	if code := runWindowsSandboxSetup(windowsSetupTestConfig(t, false), &stderr); code != 0 {
		t.Fatalf("setup without a principal must not depend on launch availability, got %d: %s", code, stderr.String())
	}
}
