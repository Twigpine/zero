//go:build windows

package sandbox

import (
	"errors"
	"strings"
	"testing"
)

// The offline group's SID goes onto the persistent WFP deny filters and the
// sandbox principal is made a member of it. Adopting a same-named group that
// something else owns therefore cuts off every existing member's outbound
// traffic and hands our principal that group's permissions, so an unmarked
// group has to be refused rather than reused.
func TestEnsureWindowsLocalGroupRefusesForeignSameNameGroup(t *testing.T) {
	for name, testCase := range map[string]struct {
		status    uintptr
		owned     bool
		ownedErr  error
		wantError string
	}{
		"foreign group with our name": {
			status: nerrGroupExists, owned: false,
			wantError: "not managed by zero",
		},
		"foreign group reported via ERROR_ALIAS_EXISTS": {
			status: errorAliasExists, owned: false,
			wantError: "not managed by zero",
		},
		"our own group on a re-run": {
			status: nerrGroupExists, owned: true,
		},
		"ownership lookup fails": {
			status: nerrGroupExists, ownedErr: errors.New("lookup refused"),
			wantError: "lookup refused",
		},
		"group did not exist": {
			status: nerrSuccess,
		},
	} {
		t.Run(name, func(t *testing.T) {
			prevAdd, prevOwned := addWindowsLocalGroupFn, windowsLocalGroupOwnedByZeroFn
			t.Cleanup(func() {
				addWindowsLocalGroupFn = prevAdd
				windowsLocalGroupOwnedByZeroFn = prevOwned
			})
			addWindowsLocalGroupFn = func(string, string) (uintptr, error) { return testCase.status, nil }
			lookups := 0
			windowsLocalGroupOwnedByZeroFn = func(string, string) (bool, error) {
				lookups++
				return testCase.owned, testCase.ownedErr
			}

			err := ensureWindowsSandboxOfflineGroup()
			if testCase.wantError == "" {
				if err != nil {
					t.Fatalf("ensureWindowsSandboxOfflineGroup: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("adopted a group it should have refused (lookups=%d)", lookups)
			}
			if !strings.Contains(err.Error(), testCase.wantError) {
				t.Fatalf("error = %q, want it to mention %q", err, testCase.wantError)
			}
		})
	}
}

// The marker compared against is the offline group's own comment, not the
// principals group's: two managed groups with different markers must not be
// mistaken for each other.
func TestEnsureWindowsLocalGroupChecksTheGroupsOwnMarker(t *testing.T) {
	prevAdd, prevOwned := addWindowsLocalGroupFn, windowsLocalGroupOwnedByZeroFn
	t.Cleanup(func() {
		addWindowsLocalGroupFn = prevAdd
		windowsLocalGroupOwnedByZeroFn = prevOwned
	})
	addWindowsLocalGroupFn = func(string, string) (uintptr, error) { return nerrGroupExists, nil }
	var gotName, gotComment string
	windowsLocalGroupOwnedByZeroFn = func(name, comment string) (bool, error) {
		gotName, gotComment = name, comment
		return true, nil
	}
	if err := ensureWindowsSandboxOfflineGroup(); err != nil {
		t.Fatalf("ensureWindowsSandboxOfflineGroup: %v", err)
	}
	if gotName != windowsSandboxOfflineGroupName {
		t.Fatalf("checked group %q, want %q", gotName, windowsSandboxOfflineGroupName)
	}
	if gotComment != windowsSandboxOfflineGroupComment {
		t.Fatalf("compared marker %q, want %q", gotComment, windowsSandboxOfflineGroupComment)
	}
}
