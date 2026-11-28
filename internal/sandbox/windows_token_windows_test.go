//go:build windows

package sandbox

import (
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A SID that parses but names nothing on the machine. CreateRestrictedToken does
// not require a restricting SID to resolve, and using a real group would make the
// test depend on local account layout.
const testCapabilitySID = "S-1-5-21-1111111111-1111111111-1111111111-4001"

// restrictedSIDStrings returns the token's restricted-SID list.
//
// This list IS the write jail. Under WRITE_RESTRICTED a write must pass both the
// ordinary access check and a second check against these SIDs, so the jail holds
// exactly as long as the list contains nothing every principal already carries.
func restrictedSIDStrings(t *testing.T, token windows.Token) []string {
	t.Helper()
	var size uint32
	err := windows.GetTokenInformation(token, windows.TokenRestrictedSids, nil, 0, &size)
	if err != nil && err != windows.ERROR_INSUFFICIENT_BUFFER {
		t.Fatalf("size restricted SID list: %v", err)
	}
	if size == 0 {
		return nil
	}
	buffer := make([]byte, size)
	if err := windows.GetTokenInformation(token, windows.TokenRestrictedSids, &buffer[0], size, &size); err != nil {
		t.Fatalf("read restricted SID list: %v", err)
	}
	groups := (*windows.Tokengroups)(unsafe.Pointer(&buffer[0]))
	values := make([]string, 0, groups.GroupCount)
	for _, group := range groups.AllGroups() {
		values = append(values, group.Sid.String())
	}
	return values
}

func restrictedTokenForTest(t *testing.T, writeRestricted bool) windows.Token {
	t.Helper()
	token, err := createWindowsRestrictedTokenForCapabilitySIDs([]string{testCapabilitySID}, writeRestricted)
	if err != nil {
		t.Fatalf("create restricted token (writeRestricted=%v): %v", writeRestricted, err)
	}
	t.Cleanup(func() { _ = token.Close() })
	return token
}

func containsSID(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

// THE REGRESSION GUARD FOR #865. The World SID (Everyone) must not be a
// restricting SID on the WRITE_RESTRICTED token.
//
// Every principal carries Everyone, so if it is on this list the second check
// passes for free on any path whose DACL grants Everyone write, and confinement
// silently falls back to the user's own permissions. No privilege, no symlink and
// no race is needed; an Everyone-writable directory is enough.
//
// This is a unit test on purpose. The existing coverage
// (TestWindowsRestrictedTokenDeniesWritesToEveryoneWritablePaths) sits behind
// ZERO_SANDBOX_REAL_SMOKE=1, which no workflow sets, so until now a refactor that
// restored the unconditional World SID went green in CI. CreateRestrictedToken
// works unelevated against the caller's own token, so there is no reason this
// invariant cannot be checked on every run.
func TestWriteRestrictedTokenExcludesTheWorldSID(t *testing.T) {
	values := restrictedSIDStrings(t, restrictedTokenForTest(t, true))
	if len(values) == 0 {
		t.Fatal("write-restricted token has no restricting SIDs at all, so there is no write jail to speak of")
	}
	if containsSID(values, "S-1-1-0") {
		t.Fatalf("the World SID is a restricting SID on the write-restricted token, which collapses the write jail: %v", values)
	}
}

// No universal group belongs on this list, for the same reason Everyone does not.
// #869 calls these out by name as the ones that would reopen the gap, and the
// runner's own comment already states the rule, so this pins it rather than
// trusting the next reader to remember.
//
// Checked on BOTH token shapes: the non-WRITE_RESTRICTED one still must not gain
// any of these beyond the World SID it is documented to carry.
func TestRestrictedSIDListNeverCarriesABroadGroup(t *testing.T) {
	forbidden := map[string]string{
		"S-1-5-32-545": `BUILTIN\Users`,
		"S-1-5-11":     "Authenticated Users",
		"S-1-5-4":      "INTERACTIVE",
		"S-1-5-3":      "BATCH",
		"S-1-5-32-544": `BUILTIN\Administrators`,
		"S-1-5-18":     "SYSTEM",
		"S-1-5-6":      "SERVICE",
		"S-1-5-2":      "NETWORK",
	}
	for _, writeRestricted := range []bool{true, false} {
		values := restrictedSIDStrings(t, restrictedTokenForTest(t, writeRestricted))
		for sid, name := range forbidden {
			if containsSID(values, sid) {
				t.Errorf("writeRestricted=%v: %s (%s) is a restricting SID; it has write access nearly everywhere, so the jail would not hold",
					writeRestricted, name, sid)
			}
		}
		// The user's own SID is the boundary this token exists to be stricter
		// than, so it must never be its own key.
		if user := currentUserSIDForTest(t); containsSID(values, user) {
			t.Errorf("writeRestricted=%v: the current user SID is a restricting SID, which defeats the token entirely", writeRestricted)
		}
	}
}

// The capability SID must actually be present, or the jail denies everything and
// the sandbox cannot write even where Zero granted access. A test that only
// checked for absences would pass against a token with an empty list.
func TestRestrictedSIDListCarriesTheCapabilitySID(t *testing.T) {
	for _, writeRestricted := range []bool{true, false} {
		values := restrictedSIDStrings(t, restrictedTokenForTest(t, writeRestricted))
		if !containsSID(values, testCapabilitySID) {
			t.Errorf("writeRestricted=%v: the capability SID is missing from %v, so no ACL-granted path would be writable",
				writeRestricted, values)
		}
	}
}

// Documents the gap #869 tracks rather than asserting the desired end state.
//
// Without WRITE_RESTRICTED the restricted-SID check covers reads too, and default
// Windows DACLs grant BUILTIN\Users, so a token with no universal group cannot
// open cmd.exe and dies at launch with STATUS_ACCESS_DENIED. Everyone is
// load-bearing here, which is why #865 could not remove it from this shape.
//
// The consequence is that this shape, selected whenever a profile sets DenyRead,
// has no effective write jail. If someone closes #869 by giving reads a grant
// that is not a universal group, this test FAILS and must be replaced by the
// exclusion assertion in the same change, rather than deleted or skipped past.
func TestNonWriteRestrictedTokenStillCarriesTheWorldSID(t *testing.T) {
	values := restrictedSIDStrings(t, restrictedTokenForTest(t, false))
	if !containsSID(values, "S-1-1-0") {
		// FAILS rather than skips, and the difference matters more than it looks.
		//
		// This SID is availability-critical as well as security-relevant: without
		// WRITE_RESTRICTED the restricted-SID check covers reads, default Windows
		// DACLs grant BUILTINUsers, and a token carrying no universal group cannot
		// open cmd.exe. Removing it therefore breaks every command with DenyRead at
		// launch. A skip here would let exactly that land on green CI, which is the
		// one outcome this test exists to prevent.
		//
		// If you are reading this because you deliberately changed the token shape
		// for #869: good, and this assertion is now yours to replace, in the same
		// change, with tests proving the new token still launches an ordinary
		// executable, still denies the intended read path, and has not restored the
		// broad write bypass. Deleting it without those is not the same thing.
		t.Fatal("the World SID is gone from the DenyRead token shape: every DenyRead command now fails at launch unless reads were given a non-universal grant; replace this assertion with the #869 exclusion and launch tests")
	}
	t.Log("known gap (#869): the DenyRead token shape carries the World SID, so its write jail does not hold")
}

// currentUserSIDForTest FAILS rather than returning empty.
//
// It used to swallow the error, and its one caller guarded on the result being
// non-empty, so a machine where GetTokenUser fails ran the assertion on nothing
// and reported a pass. The check exists to catch the token keying itself to the
// very SID it must be stricter than, which is the whole point of the shape, so
// not being able to read the prerequisite is a failure and not a skip.
func currentUserSIDForTest(t *testing.T) string {
	t.Helper()
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatalf("read the current user SID, which this assertion depends on: %v", err)
	}
	sid := user.User.Sid.String()
	if strings.TrimSpace(sid) == "" {
		t.Fatal("the current user SID came back empty, so the assertion below would check nothing")
	}
	return sid
}
