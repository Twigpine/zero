//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// The principal path must apply the write jail, not just hand over the account's
// own token. A LogonUser token carries every write the account's ambient
// memberships grant, so without this the profile's write roots are advisory.
//
// Driven with the process token as base because minting a real principal token
// needs an elevated, provisioned machine; the restriction machinery under test
// is identical either way.
func TestRestrictWindowsTokenJailsWritesOutsideCapabilitySIDs(t *testing.T) {
	var base windows.Token
	desired := uint32(windows.TOKEN_DUPLICATE | windows.TOKEN_QUERY | windows.TOKEN_ASSIGN_PRIMARY |
		windows.TOKEN_ADJUST_DEFAULT | windows.TOKEN_ADJUST_SESSIONID | windows.TOKEN_ADJUST_PRIVILEGES)
	if err := windows.OpenProcessToken(windows.CurrentProcess(), desired, &base); err != nil {
		t.Skipf("cannot open the process token here: %v", err)
	}
	defer base.Close()

	// A capability SID granted nowhere near the probe directory.
	capSID, err := windows.CreateWellKnownSid(windows.WinBuiltinGuestsSid)
	if err != nil {
		t.Fatal(err)
	}
	jailed, err := restrictWindowsTokenForCapabilitySIDs(base, []string{capSID.String()}, true)
	if err != nil {
		t.Skipf("cannot build a restricted token here: %v", err)
	}
	defer jailed.Close()

	// Setup assertion: the unrestricted process can write here, so a denial below
	// is the jail and not a broken fixture.
	dir := t.TempDir()
	target := filepath.Join(dir, "written.txt")
	if err := os.WriteFile(target, []byte("probe"), 0o600); err != nil {
		t.Fatalf("SETUP INVALID: the test process itself cannot write %s: %v", target, err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}

	config := WindowsSandboxCommandConfig{
		CommandCWD:     dir,
		WorkspaceRoots: []string{dir},
		Command:        []string{"cmd", "/c", "echo probe> " + target},
	}
	if _, err := runWindowsCommandAsUser(jailed, config); err != nil {
		t.Fatalf("run under the jailed token: %v", err)
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("the jailed token wrote a path no capability SID covers; the write jail is not applied")
	}
}

// parseWindowsCapabilitySIDs must reject an empty list rather than build an
// unrestricted token, and must not leak the SIDs it already parsed on failure.
func TestParseWindowsCapabilitySIDsRejectsEmptyAndBadInput(t *testing.T) {
	if _, err := parseWindowsCapabilitySIDs(nil); err == nil {
		t.Error("empty SID list accepted; that would build a token with no restriction")
	}
	valid, err := windows.CreateWellKnownSid(windows.WinBuiltinGuestsSid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseWindowsCapabilitySIDs([]string{valid.String(), "not-a-sid"}); err == nil {
		t.Error("an unparseable SID was accepted")
	}
}
