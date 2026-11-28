//go:build windows

package sandbox

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// THE TRANSACTION ITSELF, NOT A STAND-IN FOR IT.
//
// These tests run runWindowsSandboxSetup, the function the elevated helper
// process runs, against configurations that came off the real wire. Two seams
// make that possible on an ordinary box: the elevation check is answered yes,
// and the WFP install, which needs a real Administrator token and changes
// machine-wide state, is replaced. Everything this contract is about still runs
// for real: the setup lock, the lease, provisioning, the snapshot, the ACL apply
// with the stamp riding on its handle, the marker, and every compensation.
//
// The WFP seam is also the barrier. It sits exactly between the two halves of
// the pair setup publishes: the stamp is already on disk when it runs and the
// marker is not written until it returns.

// windowsSetupTransactionSeams answers the elevation check and replaces the WFP
// install with apply, or with a no-op when apply is nil.
func windowsSetupTransactionSeams(t *testing.T, apply func(WindowsNetworkPlan) error) {
	t.Helper()
	previousElevated := windowsSetupProcessIsElevated
	previousNetwork := windowsSetupApplyNetworkPlan
	t.Cleanup(func() {
		windowsSetupProcessIsElevated = previousElevated
		windowsSetupApplyNetworkPlan = previousNetwork
	})
	windowsSetupProcessIsElevated = func() bool { return true }
	if apply == nil {
		apply = func(WindowsNetworkPlan) error { return nil }
	}
	windowsSetupApplyNetworkPlan = apply
}

// runWindowsSetupTransaction runs the helper's transaction and returns its exit
// code and what it printed.
func runWindowsSetupTransaction(config WindowsSandboxSetupConfig) (int, string) {
	var stderr bytes.Buffer
	code := runWindowsSandboxSetup(config, &stderr)
	return code, strings.TrimSpace(stderr.String())
}

func publishThroughTheSetupTransaction(t *testing.T, config WindowsSandboxSetupConfig) {
	t.Helper()
	if code, output := runWindowsSetupTransaction(config); code != 0 {
		t.Fatalf("setup exited %d:\n%s", code, output)
	}
}

// The harness has to complete one ordinary setup that a later command accepts,
// or nothing built on it means anything.
func TestWindowsSetupTransactionRunsEndToEnd(t *testing.T) {
	workspace, _ := windowsRuntimeTestRoots(t)
	home := t.TempDir()
	windowsSetupTransactionSeams(t, nil)

	profile := bareWindowsProfile(workspace)
	publishThroughTheSetupTransaction(t, preparedWindowsSetupConfig(t, workspace, home, profile))
	if err := validateAsALaterCommand(t, workspace, home, profile); err != nil {
		t.Fatalf("a command prepared from the same profile was rejected after a successful setup: %v", err)
	}
}

// The two-homes contract again, through the transaction that actually publishes
// the stamp in production: on the handle the capability ACL was applied through.
func TestTwoSandboxHomesKeepTheirOwnAttestationThroughRealSetup(t *testing.T) {
	windowsSetupTransactionSeams(t, nil)
	twoSandboxHomesScenario(t, publishThroughTheSetupTransaction)
}

// pausedSetup runs one setup in the background and parks it inside the WFP seam,
// which is after its stamp is written and before its marker is.
type pausedSetup struct {
	reached chan struct{}
	resume  chan error
	done    chan struct{}
	code    int
	output  string
}

// startSetupsWithTheFirstPaused installs a seam that parks the FIRST setup to
// reach it and lets every later one through, then starts that first setup.
func startSetupsWithTheFirstPaused(t *testing.T, first WindowsSandboxSetupConfig) *pausedSetup {
	t.Helper()
	paused := &pausedSetup{reached: make(chan struct{}), resume: make(chan error, 1), done: make(chan struct{})}
	parked := false
	windowsSetupTransactionSeams(t, func(WindowsNetworkPlan) error {
		if parked {
			return nil
		}
		parked = true
		close(paused.reached)
		return <-paused.resume
	})
	go func() {
		defer close(paused.done)
		paused.code, paused.output = runWindowsSetupTransaction(first)
	}()
	select {
	case <-paused.reached:
	case <-paused.done:
		t.Fatalf("SETUP INVALID: the first setup finished (exit %d) without reaching the barrier:\n%s", paused.code, paused.output)
	case <-time.After(30 * time.Second):
		t.Fatal("SETUP INVALID: the first setup never reached the barrier")
	}
	return paused
}

// queueBehind starts a second setup and returns once it has found the lock held,
// which is the production signal that it is waiting and not running.
func queueBehind(t *testing.T, paused *pausedSetup, second WindowsSandboxSetupConfig) (done chan struct{}, code *int, output *string) {
	t.Helper()
	contended := make(chan struct{})
	previousHook := windowsSandboxSetupLockContendedHook
	t.Cleanup(func() { windowsSandboxSetupLockContendedHook = previousHook })
	windowsSandboxSetupLockContendedHook = func() { close(contended) }

	done = make(chan struct{})
	code, output = new(int), new(string)
	go func() {
		defer close(done)
		*code, *output = runWindowsSetupTransaction(second)
	}()
	select {
	case <-contended:
	case <-done:
		// Not an error by itself: this is what the unserialized code does, and the
		// caller's assertions are what say so.
	case <-time.After(30 * time.Second):
		paused.resume <- nil
		t.Fatal("SETUP INVALID: the second setup neither queued nor finished")
	}
	return done, code, output
}

func readWindowsSetupMarkerHash(t *testing.T, home string) string {
	t.Helper()
	body, err := os.ReadFile(WindowsSandboxSetupMarkerPath(home))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ""
		}
		t.Fatalf("read the setup marker: %v", err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.Contains(line, `"acl_plan_hash"`) || strings.Contains(line, `"aclPlanHash"`) {
			return line
		}
	}
	return string(body)
}

// TWO SETUPS FOR ONE HOME DO NOT INTERLEAVE.
//
// The first is parked between its stamp and its marker. The second, for a
// different configuration of the same home, must not publish anything while the
// first still owns the transaction. Whichever finishes last is then the setup the
// home describes, and a command for THAT configuration validates: the marker and
// the tree cannot be left describing two different setups.
func TestConcurrentSetupsForOneHomeAreSerialized(t *testing.T) {
	workspace, _ := windowsRuntimeTestRoots(t)
	home := t.TempDir()
	extra := t.TempDir()
	profileA := bareWindowsProfile(workspace)
	profileB := bareWindowsProfile(workspace, extra)

	// An established home and capability store, so first-use SID creation is not
	// part of what is being raced. Established with a THIRD configuration that
	// names every root the other two use: the store then holds all their SIDs,
	// while neither of the plans being raced has a stamp yet. That second half is
	// about this harness rather than the contract. An existing stamp is replaced
	// through the Administrators entry in its DACL, which an elevated helper has
	// and this unelevated test process does not.
	windowsSetupTransactionSeams(t, nil)
	publishThroughTheSetupTransaction(t, preparedWindowsSetupConfig(t, workspace, home, bareWindowsProfile(workspace, extra, t.TempDir())))
	markerBefore := readWindowsSetupMarkerHash(t, home)

	configA := preparedWindowsSetupConfig(t, workspace, home, profileA)
	configB := preparedWindowsSetupConfig(t, workspace, home, profileB)

	paused := startSetupsWithTheFirstPaused(t, configA)
	doneB, codeB, outputB := queueBehind(t, paused, configB)

	// A is still inside its transaction. B must not have published.
	select {
	case <-doneB:
		t.Errorf("the second setup ran to completion (exit %d) while the first still held the transaction:\n%s", *codeB, *outputB)
	default:
	}
	if got := readWindowsSetupMarkerHash(t, home); got != markerBefore {
		t.Errorf("the marker changed while the first setup was still between its stamp and its marker:\n before %s\n now    %s", markerBefore, got)
	}

	paused.resume <- nil
	<-paused.done
	<-doneB
	if paused.code != 0 {
		t.Fatalf("the first setup exited %d:\n%s", paused.code, paused.output)
	}
	if *codeB != 0 {
		t.Fatalf("the second setup exited %d:\n%s", *codeB, *outputB)
	}

	// B finished last, so B is what the home describes, completely.
	if err := validateAsALaterCommand(t, workspace, home, profileB); err != nil {
		t.Errorf("the last completed setup is rejected by its own command: %v", err)
	}
	if err := validateAsALaterCommand(t, workspace, home, profileA); err == nil {
		t.Error("a command for the earlier configuration still validates, so the home describes two setups at once")
	}
}

// A FAILED SETUP DOES NOT UNDO A LATER SUCCESS.
//
// Both setups apply the SAME plan, which is the case where they share a stamp
// file. The first found no stamp, wrote one, and is parked. Unserialized, the
// second then commits on top of it, and when the first fails its compensation
// removes "the stamp this run wrote", which by now is the attestation of a setup
// that succeeded: the home reports a removed runtime tree with nothing wrong.
func TestAFailedSetupDoesNotUndoALaterCommittedOne(t *testing.T) {
	workspace, _ := windowsRuntimeTestRoots(t)
	home := t.TempDir()
	profile := bareWindowsProfile(workspace)

	// Establish the home and capability store with a DIFFERENT plan, so the plan
	// being raced has no stamp yet and the first setup records it as absent.
	windowsSetupTransactionSeams(t, nil)
	publishThroughTheSetupTransaction(t, preparedWindowsSetupConfig(t, workspace, home, bareWindowsProfile(workspace, t.TempDir())))

	config := preparedWindowsSetupConfig(t, workspace, home, profile)
	paused := startSetupsWithTheFirstPaused(t, config)
	doneSecond, codeSecond, outputSecond := queueBehind(t, paused, config)

	paused.resume <- errors.New("the filtering platform refused the transaction")
	<-paused.done
	<-doneSecond

	if paused.code == 0 {
		t.Fatal("SETUP INVALID: the first setup was meant to fail and reported success")
	}
	if !strings.Contains(paused.output, "the filtering platform refused the transaction") {
		t.Errorf("the first setup's failure does not carry its cause:\n%s", paused.output)
	}
	if *codeSecond != 0 {
		t.Fatalf("the second setup exited %d:\n%s", *codeSecond, *outputSecond)
	}
	if err := validateAsALaterCommand(t, workspace, home, profile); err != nil {
		t.Errorf("a setup that succeeded is rejected because another one failed: %v", err)
	}
}

// A setup that cannot get the transaction says so and changes nothing, rather
// than hanging an elevated terminal or going ahead unordered.
func TestSetupReportsABusySandboxHomeAndChangesNothing(t *testing.T) {
	workspace, _ := windowsRuntimeTestRoots(t)
	home := t.TempDir()
	windowsSetupTransactionSeams(t, nil)

	previousTimeout, previousRetry := windowsSandboxSetupLockTimeout, windowsSandboxSetupLockRetry
	t.Cleanup(func() { windowsSandboxSetupLockTimeout, windowsSandboxSetupLockRetry = previousTimeout, previousRetry })
	windowsSandboxSetupLockTimeout, windowsSandboxSetupLockRetry = 50*time.Millisecond, 5*time.Millisecond

	release, err := lockWindowsSandboxSetup(home)
	if err != nil {
		t.Fatalf("lockWindowsSandboxSetup: %v", err)
	}
	config := preparedWindowsSetupConfig(t, workspace, home, bareWindowsProfile(workspace))
	code, output := runWindowsSetupTransaction(config)
	if err := release(); err != nil {
		t.Fatalf("release the held setup lock: %v", err)
	}

	if code == 0 {
		t.Fatal("setup went ahead while another setup held the sandbox home")
	}
	if !strings.Contains(output, "still running") || !strings.Contains(output, "nothing was changed") {
		t.Errorf("the refusal does not say what happened or that it is safe to retry:\n%s", output)
	}
	if _, statErr := os.Stat(WindowsSandboxSetupMarkerPath(home)); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a refused setup left a marker behind (stat err %v)", statErr)
	}
	// And the home is usable again the moment the holder lets go.
	publishThroughTheSetupTransaction(t, config)
}

// THE LOCK IS ADDED TO THE LEASE, NOT TRADED FOR IT. Ordering setups against each
// other says nothing to an eviction, which takes the runtime lease exclusively
// and never looks at the setup lock. A setup parked in the middle of its
// transaction has to keep that eviction out exactly as before.
func TestARunningSetupStillExcludesEviction(t *testing.T) {
	workspace, _ := windowsRuntimeTestRoots(t)
	home := t.TempDir()
	config := preparedWindowsSetupConfig(t, workspace, home, bareWindowsProfile(workspace))
	root := windowsSandboxSelectedRuntimeRoot(config.PermissionProfile)
	if root == "" {
		t.Fatal("SETUP INVALID: setup selected no runtime root")
	}

	paused := startSetupsWithTheFirstPaused(t, config)

	lease, inUse, err := tryAcquireSandboxRuntimeCleanupLease(root)
	if lease != nil {
		lease.release()
	}
	if err != nil {
		t.Errorf("eviction failed against a root a setup legitimately holds: %v", err)
	}
	if !inUse {
		t.Error("eviction found the runtime root free while a setup was between its stamp and its marker")
	}

	paused.resume <- nil
	<-paused.done
	if paused.code != 0 {
		t.Fatalf("setup exited %d:\n%s", paused.code, paused.output)
	}

	lease, inUse, err = tryAcquireSandboxRuntimeCleanupLease(root)
	if err != nil || inUse || lease == nil {
		t.Fatalf("the root is still held after setup returned (inUse %v, err %v)", inUse, err)
	}
	lease.release()
}
