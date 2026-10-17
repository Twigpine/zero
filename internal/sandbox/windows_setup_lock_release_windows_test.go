//go:build windows

package sandbox

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// NEVER SUCCESS OVER A FAILED RELEASE.
//
// The setup lock's release discarded both the unlock and the close error, and
// setup defers it after publishing the marker, so the operator was told setup
// succeeded whatever happened to the lock. Reported by @jatmn.

// stageWindowsSetupLockRelease makes the release report unlockErr and closeErr
// while still unlocking and closing for real, so the lock does not outlive the
// test that staged the failure.
func stageWindowsSetupLockRelease(t *testing.T, unlockErr, closeErr error) {
	t.Helper()
	previousUnlock, previousClose := windowsSandboxSetupUnlock, windowsSandboxSetupLockClose
	t.Cleanup(func() {
		windowsSandboxSetupUnlock, windowsSandboxSetupLockClose = previousUnlock, previousClose
	})
	unlocked, closed := 0, 0
	windowsSandboxSetupUnlock = func(file *os.File) error {
		unlocked++
		_ = unlockGrantFile(file)
		return unlockErr
	}
	windowsSandboxSetupLockClose = func(file *os.File) error {
		closed++
		_ = file.Close()
		return closeErr
	}
	t.Cleanup(func() {
		if unlocked == 0 || closed == 0 {
			t.Errorf("SETUP INVALID: the staged release ran unlock %d times and close %d times", unlocked, closed)
		}
	})
}

// Through the transaction the elevated helper runs: the setup does its work and
// publishes, then cannot confirm the release, and must not exit 0.
func TestSetupDoesNotReportSuccessOverAFailedLockRelease(t *testing.T) {
	for _, tc := range []struct {
		name      string
		unlockErr error
		closeErr  error
		want      []string
	}{
		{name: "unlock fails", unlockErr: errors.New("staged unlock failure"),
			want: []string{"unlock the sandbox setup lock", "staged unlock failure"}},
		{name: "close fails", closeErr: errors.New("staged close failure"),
			want: []string{"close the sandbox setup lock", "staged close failure"}},
		{name: "both fail", unlockErr: errors.New("staged unlock failure"), closeErr: errors.New("staged close failure"),
			want: []string{"staged unlock failure", "staged close failure"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace, _ := windowsRuntimeTestRoots(t)
			home := t.TempDir()
			windowsSetupTransactionSeams(t, nil)
			stageWindowsSetupLockRelease(t, tc.unlockErr, tc.closeErr)

			code, output := runWindowsSetupTransaction(preparedWindowsSetupConfig(t, workspace, home, bareWindowsProfile(workspace)))
			if code == 0 {
				t.Fatalf("setup exited 0 although releasing its lock failed:\n%s", output)
			}
			for _, want := range tc.want {
				if !strings.Contains(output, want) {
					t.Errorf("the output does not name %q:\n%s", want, output)
				}
			}
			// The failure is only the release. The transaction still ran to its
			// end, in order, and published.
			if _, err := os.Stat(WindowsSandboxSetupMarkerPath(home)); err != nil {
				t.Errorf("the transaction did not publish its marker before the release: %v", err)
			}
		})
	}
}

// The release on its own: both halves always run, and both failures come back.
func TestSetupLockReleaseRunsBothHalvesAndReportsBoth(t *testing.T) {
	stageWindowsSetupLockRelease(t, errors.New("staged unlock failure"), errors.New("staged close failure"))
	release, err := lockWindowsSandboxSetup(t.TempDir())
	if err != nil {
		t.Fatalf("take the setup lock: %v", err)
	}
	err = release()
	if err == nil {
		t.Fatal("a release whose unlock and close both failed reported nothing")
	}
	for _, want := range []string{"unlock the sandbox setup lock", "staged unlock failure", "close the sandbox setup lock", "staged close failure"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the release error does not name %q: %v", want, err)
		}
	}
}
