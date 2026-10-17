//go:build windows

package sandbox

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"
)

// revokeWindowsSandboxLogonRights treats "this account holds no rights" as
// success, because that is the state teardown is trying to reach anyway. That
// relies on the NTSTATUS for it surviving the trip through LsaNtStatusToWinError
// as an error errors.Is can still match, which is exactly the kind of Windows
// errno assumption that quietly turns out to be false. Assert it rather than
// trust it.
//
// Needs no privilege: LsaNtStatusToWinError is a pure status translation, so
// this runs everywhere rather than joining the gated set.
func TestLsaStatusErrorMapsObjectNameNotFound(t *testing.T) {
	// STATUS_OBJECT_NAME_NOT_FOUND, what LsaRemoveAccountRights reports for an
	// account that has no LSA entry.
	const statusObjectNameNotFound = 0xC0000034

	err := lsaStatusError("LsaRemoveAccountRights", statusObjectNameNotFound)
	if err == nil {
		t.Fatal("a nonzero NTSTATUS produced no error")
	}
	if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		t.Fatalf("error = %v, want one errors.Is matches against ERROR_FILE_NOT_FOUND; "+
			"without that, revoking a principal that simply holds no rights fails teardown", err)
	}

	// The tolerance must be specific. If any failure matched it, revoke would
	// swallow a real one and teardown would report success having done nothing.
	const statusAccessDenied = 0xC0000022
	if other := lsaStatusError("LsaRemoveAccountRights", statusAccessDenied); errors.Is(other, windows.ERROR_FILE_NOT_FOUND) {
		t.Fatalf("access denied matched the not-found tolerance: %v", other)
	}
}
