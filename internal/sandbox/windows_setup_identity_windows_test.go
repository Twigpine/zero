//go:build windows

package sandbox

import (
	"bytes"
	"strings"
	"testing"
)

// SETUP MUST NOT PROVISION A STAMP FOR AN ACCOUNT THAT IS NOT DOING THE SETUP.
//
// Zero never elevates: runSandboxSetupHelper launches the helper with a plain
// exec.Command, and the helper refuses a non-elevated token, so the consumer SID
// is resolved in a process that is ALREADY elevated. Under same-account UAC that
// is harmless, because both tokens carry the same user SID. Under alternate
// administrator credentials it is not: the serialized SID describes the admin,
// the protected stamp gets its allow ACE for the wrong token, and every
// restricted command stops before launch on a setup that has just reported
// success.
//
// That case cannot be detected from inside an already-elevated process, so it is
// excluded rather than mis-provisioned. This pins the exclusion, and it pins it
// at the point that matters: BEFORE anything is provisioned.
func TestWindowsSetupRefusesAConsumerThatIsNotTheInstaller(t *testing.T) {
	const consumer = "S-1-5-21-1111111111-2222222222-3333333333-1001"
	const installer = "S-1-5-21-1111111111-2222222222-3333333333-500"

	previousElevated := windowsSetupProcessIsElevated
	previousSID := windowsSetupInstallerSID
	t.Cleanup(func() {
		windowsSetupProcessIsElevated = previousElevated
		windowsSetupInstallerSID = previousSID
	})
	// Elevated, because otherwise the gate under test is never reached on an
	// ordinary developer box.
	windowsSetupProcessIsElevated = func() bool { return true }
	windowsSetupInstallerSID = func() (string, error) { return installer, nil }

	var stderr bytes.Buffer
	code := runWindowsSandboxSetup(WindowsSandboxSetupConfig{
		ConsumerSID: consumer,
		// Deliberately nothing else: if the gate does not fire first, setup would
		// go on to provision with an empty config, and this test would fail on that
		// instead, which is still a failure but for the wrong reason. The message
		// assertions below are what distinguish them.
	}, &stderr)

	if code == 0 {
		t.Fatalf("setup accepted a consumer that is not the installer and reported success:\n%s", stderr.String())
	}
	message := stderr.String()
	if !strings.Contains(message, "Alternate-account setup is not supported") {
		t.Fatalf("the refusal does not name the unsupported model, so an operator cannot act on it:\n%s", message)
	}
	if !strings.Contains(message, consumer) || !strings.Contains(message, installer) {
		t.Errorf("the refusal names neither identity, so it cannot be diagnosed:\n%s", message)
	}
}

// And the supported model is still accepted at this gate: same account, so the
// carried consumer matches the installing token and setup proceeds past it.
//
// Asserting the gate is passed rather than that setup succeeds, because setup
// goes on to do real provisioning that an unelevated test box cannot perform.
func TestWindowsSetupAcceptsTheSameAccountConsumer(t *testing.T) {
	const same = "S-1-5-21-1111111111-2222222222-3333333333-1001"

	previousElevated := windowsSetupProcessIsElevated
	previousSID := windowsSetupInstallerSID
	t.Cleanup(func() {
		windowsSetupProcessIsElevated = previousElevated
		windowsSetupInstallerSID = previousSID
	})
	windowsSetupProcessIsElevated = func() bool { return true }
	windowsSetupInstallerSID = func() (string, error) { return same, nil }

	var stderr bytes.Buffer
	_ = runWindowsSandboxSetup(WindowsSandboxSetupConfig{ConsumerSID: same}, &stderr)

	if strings.Contains(stderr.String(), "Alternate-account setup is not supported") {
		t.Fatalf("the supported same-account model was refused by the identity gate:\n%s", stderr.String())
	}
}
