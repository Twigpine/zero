//go:build windows

package sandbox

import (
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// windowsACLPlanStillApplied reports whether the objects a plan names still
// carry the grants it describes.
//
// THE MARKER FINGERPRINTS A PLAN, NOT AN OBJECT. Its hash is computed from
// pathnames and entries, so it records what SHOULD be granted and can never
// establish that whatever directory currently answers to that name has received
// it. The runtime root makes the difference reachable rather than theoretical:
// it is deterministic and disposable, so cleanup removes the tree, the next
// command's parent recreates the same pathname with ordinary inherited
// permissions, and the plan hash is unchanged. The fast path then skipped the
// apply entirely and the WRITE_RESTRICTED child could not write TMP or its
// language and package caches, with nothing failing to say why.
//
// PRESENCE OF THE SID IS NOT THE CONTRACT. What the child needs is the grant
// windowsACLAccess actually creates, and on a directory it needs to reach
// descendants. An ACE that still names the capability SID but has been reduced
// to a metadata or read-only mask, or that no longer propagates, leaves a
// runtime root that attests as healthy and returns ACCESS_DENIED on the first
// write into TMP or a cache: the same silent unusable runtime the attestation
// exists to eliminate. So the check compares the effective grant.
//
// Attesting costs one security-descriptor read per allow entry, on a path that
// is about to create a process, and it covers every reason a grant can be
// missing or insufficient rather than only recreation by the parent.
//
// Anything unprovable reads as "not applied". Re-applying an adequate grant is
// idempotent and cheap; skipping an inadequate one produces a sandbox that
// silently cannot write.
func windowsACLPlanStillApplied(plan WindowsACLPlan) bool {
	for _, entry := range plan.Entries {
		if entry.Action != WindowsACLAllowWrite {
			// Only the allow grants decide whether the child can run. A missing
			// DENY is a weaker boundary rather than a broken one, and re-applying
			// the whole plan is what fixes either.
			continue
		}
		if strings.TrimSpace(entry.Path) == "" || strings.TrimSpace(entry.Capability) == "" {
			continue
		}
		_, required, err := windowsACLAccess(entry.Action)
		if err != nil {
			return false
		}
		if !windowsPathCarriesGrant(entry.Path, entry.Capability, required) {
			return false
		}
	}
	return true
}

// windowsPathCarriesGrant reports whether path's DACL gives trustee at least
// required, and whether that grant reaches descendants when path is a directory.
func windowsPathCarriesGrant(path, trustee string, required windows.ACCESS_MASK) bool {
	wanted, err := windows.StringToSid(trustee)
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	// windowsExplicitAccessEntries sets SUB_CONTAINERS_AND_OBJECTS_INHERIT on a
	// directory, and that is what lets the child create TMP and cache entries
	// underneath. A grant on the directory alone would not.
	needInherit := uint8(0)
	if info.IsDir() {
		needInherit = windows.CONTAINER_INHERIT_ACE | windows.OBJECT_INHERIT_ACE
	}

	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		return false
	}
	return windowsDACLCarriesGrant(dacl, wanted, required, needInherit)
}

// windowsPlainACEHeaderSize is the part of an ACCESS_ALLOWED_ACE or
// ACCESS_DENIED_ACE in front of its SID: the ACE header and the access mask.
const windowsPlainACEHeaderSize = 8

// windowsMinimalSIDSize is a SID with no sub-authorities: revision, count and
// the six-byte identifier authority.
const windowsMinimalSIDSize = 8

// windowsPlainACESID returns the SID of a plain allow or deny ACE, or nil when
// the entry is not one or cannot be shown to hold a whole SID.
//
// THE TYPE DECIDES THE LAYOUT, SO IT IS READ FIRST. Only ACCESS_ALLOWED_ACE and
// ACCESS_DENIED_ACE keep their SID directly behind the mask. An object ACE puts
// a flags word and up to two GUIDs there, and the loop below used to cast every
// entry to the plain layout and hand whatever sat at that offset to EqualSid,
// whose behaviour is undefined for a structure that is not a SID. A sub-authority
// count read out of a flags word is a length nobody vouched for. The entry is
// now admitted on its header alone, before any byte past the header is touched.
//
// Entries in any other layout are skipped, which is the conservative reading for
// this attestation: an allow that is not read is not credited, so the worst case
// is one idempotent re-apply. A conditional or object DENY is not evaluated
// either. Setup never writes one, and nothing else has a reason to name a
// capability SID Zero minted.
//
// The size test is the same rule one level down. AceSize is what the ACL itself
// says the entry occupies, so a SID whose own count claims more than that is not
// a SID this function can stand behind, and unprovable reads as not applied.
// Reported by @jatmn.
func windowsPlainACESID(header *windows.ACE_HEADER) (*windows.ACCESS_ALLOWED_ACE, *windows.SID) {
	if header == nil {
		return nil, nil
	}
	if header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE && header.AceType != windows.ACCESS_DENIED_ACE_TYPE {
		return nil, nil
	}
	if uintptr(header.AceSize) < windowsPlainACEHeaderSize+windowsMinimalSIDSize {
		return nil, nil
	}
	ace := (*windows.ACCESS_ALLOWED_ACE)(unsafe.Pointer(header))
	sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	// The count is the second byte of a SID, inside the eight bytes just shown to
	// be there.
	subAuthorities := uintptr(*(*byte)(unsafe.Add(unsafe.Pointer(sid), 1)))
	if uintptr(header.AceSize) < windowsPlainACEHeaderSize+windowsMinimalSIDSize+4*subAuthorities {
		return nil, nil
	}
	return ace, sid
}

// windowsDACLCarriesGrant is the decision windowsPathCarriesGrant makes once it
// has the DACL in hand. It is separate so the rule about which entries may be
// read can be tested against an ACL built in memory, including layouts a
// filesystem would not store.
func windowsDACLCarriesGrant(dacl *windows.ACL, wanted *windows.SID, required windows.ACCESS_MASK, needInherit uint8) bool {
	if dacl == nil || wanted == nil {
		return false
	}
	var granted windows.ACCESS_MASK
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var header *windows.ACE_HEADER
		if err := windows.GetAce(dacl, index, (**windows.ACCESS_ALLOWED_ACE)(unsafe.Pointer(&header))); err != nil {
			return false
		}
		ace, sid := windowsPlainACESID(header)
		if ace == nil {
			continue
		}
		if !sid.Equals(wanted) {
			continue
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			// A deny naming the capability itself takes precedence over any allow,
			// so the grant cannot be proven adequate. Fail closed and re-apply.
			if ace.Mask&required != 0 {
				return false
			}
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			// INHERIT_ONLY DOES NOT APPLY TO THE OBJECT ITSELF. An ACE carrying it
			// grants descendants and grants the directory nothing, so the child can
			// still be refused FILE_ADD_FILE on the runtime root while every other
			// bit here looks satisfied. Checking the inherit flags alone would take
			// the propagation half of the grant as evidence for the whole of it,
			// which is the substitution this attestation exists to stop making.
			if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
				continue
			}
			// AND PROPAGATION THAT STOPS AT THE IMMEDIATE CHILDREN IS NOT PROPAGATION.
			//
			// NO_PROPAGATE_INHERIT_ACE lets an ACE be inherited once and then stops:
			// the children get it, their children do not. The flag test below reads
			// only the two inherit bits, so OI|CI|NP satisfied it and the whole mask
			// was credited, while the runtime tree's real consumers sit a level
			// deeper - cache/npm, cache/go-build, data/go-mod, and the package files
			// under them, which sandboxRuntimeEnvironment points npm_config_cache,
			// GOCACHE and GOMODCACHE at. Attestation said the grant was adequate and
			// the writes were refused. Reported by @jatmn.
			//
			// Gated on needInherit so a file entry, where inherit flags mean nothing,
			// is unaffected. The apply path never sets NP for an AllowWrite entry, so
			// this cannot make a grant Zero itself wrote look insufficient.
			if needInherit != 0 && ace.Header.AceFlags&windows.NO_PROPAGATE_INHERIT_ACE != 0 {
				continue
			}
			// And only ACEs that propagate count towards a directory's grant, since a
			// non-inheriting one leaves descendants ungranted.
			if ace.Header.AceFlags&needInherit != needInherit {
				continue
			}
			granted |= ace.Mask
		}
	}
	return granted&required == required
}
