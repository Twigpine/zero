//go:build windows

package sandbox

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"
)

// Choosing the offline account is only half of what denies it the network: the
// WFP filters match the offline GROUP'S SID. An account that has drifted out of
// that group still logs on from its stored secret, and its token no longer
// satisfies the filter condition — so a no-network profile would get full
// egress. The command path has to re-check the membership, not trust the marker
// setup wrote when it last succeeded.
func TestPrincipalTokenRechecksOfflineGroupMembership(t *testing.T) {
	for name, testCase := range map[string]struct {
		mode        NetworkMode
		member      bool
		memberErr   error
		wantChecked bool
		wantReached bool
		wantErr     bool
	}{
		"offline principal drifted out of the group": {
			mode: NetworkDeny, member: false,
			wantChecked: true, wantReached: false,
		},
		"offline principal still a member": {
			mode: NetworkDeny, member: true,
			wantChecked: true, wantReached: true,
		},
		"membership lookup fails": {
			mode: NetworkDeny, memberErr: errors.New("group lookup refused"),
			wantChecked: true, wantReached: false, wantErr: true,
		},
		// An allow-network command uses the online principal, which is not in the
		// offline group by design. Checking it there would refuse every approved
		// network command.
		"online principal is not checked": {
			mode: NetworkAllow, member: false,
			wantChecked: false, wantReached: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			prevLookup := lookupWindowsSandboxPrincipalForCommandFn
			prevMember := windowsSandboxUserInLocalGroupFn
			prevSecret := readWindowsSandboxSecretFn
			prevWarn := warnWindowsSandboxOfflineMembershipMissing
			t.Cleanup(func() {
				lookupWindowsSandboxPrincipalForCommandFn = prevLookup
				windowsSandboxUserInLocalGroupFn = prevMember
				readWindowsSandboxSecretFn = prevSecret
				warnWindowsSandboxOfflineMembershipMissing = prevWarn
			})

			sid, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
			if err != nil {
				t.Fatal(err)
			}
			lookupWindowsSandboxPrincipalForCommandFn = func(string, windowsSandboxRole) (windowsSandboxIdentity, error) {
				return windowsSandboxIdentity{Username: "zero-sbx-test", SID: sid}, nil
			}
			checkedGroup := ""
			windowsSandboxUserInLocalGroupFn = func(_ string, group string) (bool, error) {
				checkedGroup = group
				return testCase.member, testCase.memberErr
			}
			secretRead := false
			readWindowsSandboxSecretFn = func(string) (string, error) {
				secretRead = true
				return "", errWindowsSandboxIdentityUnavailable
			}
			warnWindowsSandboxOfflineMembershipMissing = func(string) {}

			config := windowsSandboxTestConfig(t)
			config.Env = map[string]string{windowsSandboxIdentityEnv: "1"}
			config.PermissionProfile.Network.Mode = testCase.mode

			_, ok, err := windowsSandboxPrincipalToken(config)
			if testCase.wantErr {
				if err == nil {
					t.Fatal("a failed membership lookup was swallowed")
				}
			} else if err != nil {
				t.Fatalf("windowsSandboxPrincipalToken: %v", err)
			}
			_ = ok
			if secretRead != testCase.wantReached {
				t.Fatalf("reached the secret read = %v, want %v (gate must short-circuit before it)", secretRead, testCase.wantReached)
			}
			if checked := checkedGroup != ""; checked != testCase.wantChecked {
				t.Fatalf("membership checked = %v, want %v", checked, testCase.wantChecked)
			}
			if testCase.wantChecked && checkedGroup != windowsSandboxOfflineGroupName {
				t.Fatalf("checked group %q, want %q", checkedGroup, windowsSandboxOfflineGroupName)
			}
		})
	}
}
