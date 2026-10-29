//go:build windows

package sandbox

import (
	"strings"
	"testing"
)

// THE ACCOUNT SID MUST NOT BE A RESTRICTING SID.
//
// WRITE_RESTRICTED allows a write only when the normal token AND the
// restricting-SID list both allow it. The principal's account SID is already
// enabled in its normal token, so listing it as a restricting SID makes the
// second check a formality for anything granted to that account: every path
// carrying a direct principal ACE passes both halves and is writable wherever it
// sits, including the profile directory Windows creates on first logon outside
// every configured write root.
//
// Same shape as the World SID in the restricted list (#865). A SID already
// carried by the normal token cannot also restrict it.
//
// The pre-existing jail test could not catch this: it grants a GROUP
// (WinBuiltinGuestsSid), so it exercises a configuration the product never
// ships. This asserts the composition that actually ships.
func TestPrincipalJailExcludesTheAccountsOwnSID(t *testing.T) {
	accountSID := "S-1-5-21-9-9-9-1500"
	// The account SID is planted IN the capability list on purpose. Passing a
	// list that never contained it would make this test unfalsifiable: it would
	// pass against any implementation, including one that appends the account SID
	// straight back. This is the route the World SID took in #865.
	capabilities := []string{"S-1-5-21-1-2-3-1001", accountSID, "S-1-5-21-1-2-3-1002"}

	jail := windowsPrincipalJailSIDs(capabilities, accountSID)

	for _, sid := range jail {
		if strings.EqualFold(sid, accountSID) {
			t.Fatalf("the principal's own account SID is a restricting SID, so any path with a direct principal ACE escapes the write jail: %v", jail)
		}
	}
	// ...and the capability SIDs are still all present, or the principal would be
	// jailed out of the workspace it owns and the fix would be a denial of service
	// rather than a fix.
	for _, want := range []string{"S-1-5-21-1-2-3-1001", "S-1-5-21-1-2-3-1002"} {
		if !containsString(jail, want) {
			t.Errorf("capability SID %q missing from the jail, so the principal loses write access to its own workspace: %v", want, jail)
		}
	}
}

// The jail must not alias the caller's slice. Appending to a shared backing
// array would let a later append mutate the restricting set of a token already
// being built.
//
// Both assertions read through to the BACKING ARRAY, which is the only place
// aliasing is observable. An earlier version of this test appended a sentinel and
// then ranged over the caller's slice looking for it, which can never fail:
// append returns a new header, so the caller's length never grows and the range
// never reaches the slot the write landed in. It passed against every
// implementation, including one that returns the caller's slice verbatim.
func TestPrincipalJailDoesNotAliasTheCapabilitySlice(t *testing.T) {
	capabilities := make([]string, 2, 8)
	capabilities[0] = "S-1-5-21-1-2-3-1001"
	capabilities[1] = "S-1-5-21-1-2-3-1002"

	jail := windowsPrincipalJailSIDs(capabilities, "S-1-5-21-9-9-9-1500")
	if len(jail) == 0 {
		t.Fatal("jail is empty, so neither assertion below proves anything")
	}
	// Shared element storage: a mutation through jail[0] would rewrite a SID in
	// the restricting set of a token already being built.
	if &jail[0] == &capabilities[0] {
		t.Fatal("jail shares element storage with the caller's capability slice")
	}
	// Spare capacity: appending to an aliased jail writes into the caller's
	// backing array past its length, which is invisible through the caller's own
	// header but very visible to anything else holding a longer view of it.
	grown := append(jail, "S-1-1-0")
	shared := capabilities[:cap(capabilities)]
	if shared[len(capabilities)] == "S-1-1-0" {
		t.Fatalf("appending to the jail wrote through into the caller's backing array: jail=%v shared=%v",
			grown, shared[:len(capabilities)+1])
	}
	if len(capabilities) != 2 {
		t.Errorf("caller's slice grew to %d", len(capabilities))
	}
}
