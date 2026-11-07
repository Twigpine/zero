//go:build windows

package sandbox

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	aceAdmissionLegEnv = "ZERO_TEST_ACE_ADMISSION_LEG"

	// Not in x/sys/windows. Values from winnt.h.
	testAccessAllowedObjectACEType   = 0x5
	testAccessAllowedCallbackACEType = 0x9

	aceAdmissionTrustee = "S-1-5-21-1111-2222-3333-4444"
)

// rawACE lays out an ACE by hand: header, mask, then whatever the leg wants to
// sit where a plain ACE keeps its SID.
func rawACE(aceType, aceFlags byte, mask uint32, tail []byte) []byte {
	ace := make([]byte, windowsPlainACEHeaderSize+len(tail))
	ace[0] = aceType
	ace[1] = aceFlags
	binary.LittleEndian.PutUint16(ace[2:], uint16(len(ace)))
	binary.LittleEndian.PutUint32(ace[4:], mask)
	copy(ace[windowsPlainACEHeaderSize:], tail)
	return ace
}

// aclEndingAtAGuardPage builds an ACL whose last byte is the last readable byte
// before a PAGE_NOACCESS page. A reader that trusts a length the entry did not
// vouch for walks off the end and faults, instead of quietly reading whatever
// the heap had next, which is what makes an over-read observable at all.
func aclEndingAtAGuardPage(t *testing.T, aces ...[]byte) *windows.ACL {
	t.Helper()
	size := 8
	for _, ace := range aces {
		size += len(ace)
	}
	if size%4 != 0 {
		t.Fatalf("SETUP INVALID: ACL size %d is not DWORD aligned", size)
	}
	page := os.Getpagesize()
	base, err := windows.VirtualAlloc(0, uintptr(2*page), windows.MEM_RESERVE|windows.MEM_COMMIT, windows.PAGE_READWRITE)
	if err != nil {
		t.Fatalf("VirtualAlloc: %v", err)
	}
	var previous uint32
	if err := windows.VirtualProtect(base+uintptr(page), uintptr(page), windows.PAGE_NOACCESS, &previous); err != nil {
		t.Fatalf("VirtualProtect: %v", err)
	}
	start := base + uintptr(page) - uintptr(size)
	// The address is not Go memory, so it is turned into a pointer without the
	// uintptr-to-Pointer conversion vet rejects.
	region := unsafe.Slice((*byte)(*(*unsafe.Pointer)(unsafe.Pointer(&start))), size)
	region[0] = 4 // ACL_REVISION_DS, the revision that admits object ACEs
	binary.LittleEndian.PutUint16(region[2:], uint16(size))
	binary.LittleEndian.PutUint16(region[4:], uint16(len(aces)))
	offset := 8
	for _, ace := range aces {
		offset += copy(region[offset:], ace)
	}
	return (*windows.ACL)(unsafe.Pointer(&region[0]))
}

func sidBytes(t *testing.T, sid *windows.SID) []byte {
	t.Helper()
	return append([]byte(nil), unsafe.Slice((*byte)(unsafe.Pointer(sid)), int(windows.GetLengthSid(sid)))...)
}

// TestACEAdmissionHelperProcess is the child. Every leg puts an ACL against a
// guard page, so a regression is an access violation, and an access violation
// in the test binary itself would take every other test in the package down
// with it and could be misread as a pass.
func TestACEAdmissionHelperProcess(t *testing.T) {
	leg := os.Getenv(aceAdmissionLegEnv)
	if leg == "" {
		return
	}
	wanted, err := windows.StringToSid(aceAdmissionTrustee)
	if err != nil {
		t.Fatal(err)
	}
	_, required, err := windowsACLAccess(WindowsACLAllowWrite)
	if err != nil {
		t.Fatal(err)
	}
	whole := sidBytes(t, wanted)
	// The first sub-authority and no more: revision, count and authority all
	// match the trustee, so a comparison that believes the count keeps going.
	prefix := whole[:12]
	inherit := byte(windows.CONTAINER_INHERIT_ACE | windows.OBJECT_INHERIT_ACE)

	var acl *windows.ACL
	switch leg {
	case "plain":
		acl = aclEndingAtAGuardPage(t, rawACE(windows.ACCESS_ALLOWED_ACE_TYPE, inherit, uint32(required), whole))
	case "object":
		acl = aclEndingAtAGuardPage(t, rawACE(testAccessAllowedObjectACEType, inherit, uint32(required), prefix))
	case "callback":
		acl = aclEndingAtAGuardPage(t, rawACE(testAccessAllowedCallbackACEType, inherit, uint32(required), prefix))
	case "truncated":
		acl = aclEndingAtAGuardPage(t, rawACE(windows.ACCESS_ALLOWED_ACE_TYPE, inherit, uint32(required), prefix))
	case "object then plain":
		// An unreadable entry must not end the walk: the grant behind it counts.
		acl = aclEndingAtAGuardPage(t,
			rawACE(testAccessAllowedObjectACEType, inherit, uint32(required), prefix),
			rawACE(windows.ACCESS_ALLOWED_ACE_TYPE, inherit, uint32(required), whole))
	default:
		t.Fatalf("unknown leg %q", leg)
	}
	fmt.Printf("carries=%v\n", windowsDACLCarriesGrant(acl, wanted, required, inherit))
}

// ONLY A PLAIN ALLOW OR DENY ACE IS READ AS ONE.
//
// The attestation walks the DACL and compared every entry's would-be SID with
// the capability before it looked at the entry's type. Only ACCESS_ALLOWED_ACE
// and ACCESS_DENIED_ACE keep a SID directly behind the mask; an object ACE has a
// flags word and GUIDs there, and EqualSid is undefined for something that is
// not a SID. The bytes below are arranged so that the misreading is not
// hypothetical: each entry's tail spells the START of the trustee's SID, so its
// revision and sub-authority count match and the comparison runs on for the
// length that count claims, which is past the end of the entry and into a page
// that cannot be read. With the type and size admitted first nothing is read
// past the header, the legs answer false, and the plain entry still answers
// true so the walk has not simply stopped crediting anything.
//
// Each leg runs in a child process. An access violation cannot be recovered in
// Go, and one in this binary would end the package's other tests with it.
// Reported by @jatmn.
func TestOnlyPlainACEsAreReadDuringAttestation(t *testing.T) {
	for _, testCase := range []struct {
		leg  string
		want bool
	}{
		{leg: "plain", want: true},
		{leg: "object", want: false},
		{leg: "callback", want: false},
		{leg: "truncated", want: false},
		{leg: "object then plain", want: true},
	} {
		t.Run(testCase.leg, func(t *testing.T) {
			child := exec.Command(os.Args[0], "-test.run=^TestACEAdmissionHelperProcess$", "-test.count=1")
			child.Env = append(os.Environ(), aceAdmissionLegEnv+"="+testCase.leg)
			output, err := child.CombinedOutput()
			if err != nil {
				t.Fatalf("the attestation did not survive this entry (%v); an entry was read in a layout its type does not have:\n%s", err, lastLines(string(output), 12))
			}
			want := fmt.Sprintf("carries=%v", testCase.want)
			if !strings.Contains(string(output), want) {
				t.Fatalf("want %s, got:\n%s", want, lastLines(string(output), 12))
			}
		})
	}
}

func lastLines(text string, count int) string {
	lines := strings.Split(strings.TrimRight(text, "\r\n"), "\n")
	if len(lines) > count {
		lines = lines[len(lines)-count:]
	}
	return strings.Join(lines, "\n")
}

// THE TYPE RULE HAS TO HOLD ON ITS OWN. The guard-page legs above are also
// stopped by the size rule, because their tails are short, so they pass with the
// type rule removed. An object or callback entry can just as well be long enough
// to hold the SID its would-be count claims, and then only its type says it is
// not a plain ACE. Here every entry carries the trustee's whole SID at the plain
// offset, which is the layout that would make a misread entry compare equal, and
// only the two plain types may be admitted.
func TestOnlyPlainACETypesAreAdmitted(t *testing.T) {
	wanted, err := windows.StringToSid(aceAdmissionTrustee)
	if err != nil {
		t.Fatal(err)
	}
	whole := sidBytes(t, wanted)
	for _, testCase := range []struct {
		name    string
		aceType byte
		admit   bool
	}{
		{name: "allowed", aceType: windows.ACCESS_ALLOWED_ACE_TYPE, admit: true},
		{name: "denied", aceType: windows.ACCESS_DENIED_ACE_TYPE, admit: true},
		{name: "allowed object", aceType: 0x5, admit: false},
		{name: "denied object", aceType: 0x6, admit: false},
		{name: "allowed callback", aceType: 0x9, admit: false},
		{name: "denied callback", aceType: 0xA, admit: false},
		{name: "allowed callback object", aceType: 0xB, admit: false},
		{name: "mandatory label", aceType: 0x11, admit: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			raw := rawACE(testCase.aceType, 0, 0x1F01FF, whole)
			ace, sid := windowsPlainACESID((*windows.ACE_HEADER)(unsafe.Pointer(&raw[0])))
			if (ace != nil) != testCase.admit {
				t.Fatalf("type 0x%X admitted=%v, want %v", testCase.aceType, ace != nil, testCase.admit)
			}
			if testCase.admit && !sid.Equals(wanted) {
				t.Errorf("an admitted plain entry did not yield its SID")
			}
		})
	}
	if ace, _ := windowsPlainACESID(nil); ace != nil {
		t.Error("a nil header was admitted")
	}
}
