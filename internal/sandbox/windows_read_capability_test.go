package sandbox

import "testing"

// readCapabilityProfile is the shape that selects the strict token: a restricted
// filesystem with DenyRead set. Without DenyRead the token stays
// WRITE_RESTRICTED and reads never reach the restricted-SID check at all, so
// every assertion below would hold vacuously.
func readCapabilityProfile(readRoot string) PermissionProfile {
	return PermissionProfile{
		FileSystem: FileSystemPolicy{
			Kind:       FileSystemRestricted,
			WriteRoots: []WritableRoot{{Root: `C:\workspace`}},
			ReadRoots:  []string{`C:\`, readRoot},
			DenyRead:   []string{`C:\Users\someone\.aws\credentials`},
		},
		Network: NetworkPolicy{Mode: NetworkAllow},
	}
}

// A READ ROOT MUST CARRY A RESTRICTING SID.
//
// On the active-principal path the command runs on a strict token, because
// DenyRead drops WRITE_RESTRICTED and Windows then applies the restricted-SID
// check to reads as well as writes. Read roots were granted only to the
// principal's account SID, which windowsPrincipalJailSIDs removes from the
// restricting set on purpose, so the normal check passed and the restricted
// check matched nothing. The effect was not a narrower sandbox but an unusable
// one: no read root readable, up to and including the executable being launched.
//
// The custom read root is the case a workspace-only assertion would miss: an SDK
// or toolchain directory outside the workspace that the command must read.
func TestCapabilityPlanGrantsReadRootsToARestrictingSID(t *testing.T) {
	home := t.TempDir()
	custom := `C:\sdk\toolchain`
	config := WindowsSandboxCommandConfig{
		SandboxHome:       home,
		CommandCWD:        `C:\workspace`,
		WorkspaceRoots:    []string{`C:\workspace`},
		PermissionProfile: readCapabilityProfile(custom),
	}
	plan, err := BuildWindowsACLPlan(config)
	if err != nil {
		t.Fatalf("BuildWindowsACLPlan: %v", err)
	}
	readSID, err := WindowsReadAllowSID(home)
	if err != nil {
		t.Fatalf("WindowsReadAllowSID: %v", err)
	}
	if readSID == "" {
		t.Fatal("no read capability SID, so this test proves nothing")
	}

	for _, want := range []string{`C:\`, custom} {
		found := false
		for _, entry := range plan.Entries {
			if entry.Action == WindowsACLAllowRead &&
				windowsCapabilityPathKey(entry.Path) == windowsCapabilityPathKey(want) &&
				entry.Capability == readSID {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("read root %s has no allow-read entry for the read capability SID, so a strict token cannot read it", want)
		}
	}
}

// The read grant must not reopen the deny list.
//
// The read roots start at the filesystem root, so the capability that makes them
// readable also covers every DenyRead carveout underneath. If the deny entries
// name only the write capabilities, the carveout stays readable through the read
// capability and DenyRead silently stops meaning anything — which is the whole
// reason the strict token is selected in the first place.
func TestDenyReadCoversTheReadCapabilitySID(t *testing.T) {
	home := t.TempDir()
	config := WindowsSandboxCommandConfig{
		SandboxHome:       home,
		CommandCWD:        `C:\workspace`,
		WorkspaceRoots:    []string{`C:\workspace`},
		PermissionProfile: readCapabilityProfile(`C:\sdk\toolchain`),
	}
	plan, err := BuildWindowsACLPlan(config)
	if err != nil {
		t.Fatalf("BuildWindowsACLPlan: %v", err)
	}
	readSID, err := WindowsReadAllowSID(home)
	if err != nil {
		t.Fatalf("WindowsReadAllowSID: %v", err)
	}

	denied := false
	sawDeny := false
	for _, entry := range plan.Entries {
		if entry.Action != WindowsACLDenyRead {
			continue
		}
		sawDeny = true
		if entry.Capability == readSID {
			denied = true
		}
	}
	if !sawDeny {
		t.Fatal("plan has no deny-read entries at all, so this test proves nothing")
	}
	if !denied {
		t.Error("no deny-read entry names the read capability SID, so every DenyRead path stays readable through the read grant")
	}
}

// Without DenyRead the token stays WRITE_RESTRICTED, reads skip the restricted
// check, and the read grant would be ACEs that buy nothing — on roots as broad
// as the filesystem root. Both halves of the protocol decide this from the
// profile alone, so setup and the command cannot disagree about whether the
// entries exist.
func TestReadCapabilityIsAbsentWithoutDenyRead(t *testing.T) {
	profile := readCapabilityProfile(`C:\sdk\toolchain`)
	profile.FileSystem.DenyRead = nil
	plan, err := BuildWindowsACLPlan(WindowsSandboxCommandConfig{
		SandboxHome:       t.TempDir(),
		CommandCWD:        `C:\workspace`,
		WorkspaceRoots:    []string{`C:\workspace`},
		PermissionProfile: profile,
	})
	if err != nil {
		t.Fatalf("BuildWindowsACLPlan: %v", err)
	}
	for _, entry := range plan.Entries {
		if entry.Action == WindowsACLAllowRead {
			t.Fatalf("allow-read entry %s present without DenyRead, where reads are never restricted", entry.Path)
		}
	}
}
