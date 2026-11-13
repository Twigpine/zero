//go:build windows

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const windowsFileDeleteChild windows.ACCESS_MASK = 0x00000040

const (
	windowsAccessAllowedObjectAceType         = 0x5
	windowsAccessDeniedObjectAceType          = 0x6
	windowsAccessAllowedCallbackAceType       = 0x9
	windowsAccessAllowedCallbackObjectAceType = 0xB
)

type windowsACLPathGroup struct {
	Path string
	// Anchor is the write root Path was derived from, empty when the operator
	// named the path. See verifyWindowsACLHandleUnderAnchor.
	Anchor          string
	Entries         []WindowsACLEntry
	Materialize     bool
	MaterializeFile bool
}

// windowsACLChainStep is one directory component beneath the anchor, and
// whether THIS run created it. The flag comes from the kernel rather than from
// "it was missing when we looked", because something else can win the gap
// between the probe and the create, and removing a directory the sandbox did not
// make is how a rollback deletes a user's data.
type windowsACLChainStep struct {
	Name string
	Made bool
	// ID is the identity of the directory this run created at Name, read from
	// the handle that created it. Rollback removes only an object with this
	// identity: a created name says the parent is unchanged, not that the
	// child has not been renamed aside and replaced since.
	ID windowsFileIdentity
}

// windowsACLMaterialization is exactly what materialization created, recorded in
// the shape rollback needs to undo it without resolving a single pathname below
// the anchor.
//
// The anchor is the deepest directory that already existed, and it is the ONLY
// pathname rollback re-resolves. Everything under it is a list of single
// components walked one handle at a time, because a name containing a separator
// is resolved the ordinary way by the kernel and would follow an intermediate
// junction straight out of the approved tree.
type windowsACLMaterialization struct {
	AnchorPath string
	AnchorID   windowsFileIdentity
	// Chain is every component between the anchor and the target, shallow to
	// deep. All of them are needed to descend at rollback time; only the ones
	// with Made set are removed.
	Chain []windowsACLChainStep
	// File is the leaf file component created inside the deepest Chain entry,
	// for the .git/config carveout. Empty when the target is a directory.
	File     string
	FileMade bool
	// FileID identifies the leaf file this run created, for the same reason.
	FileID windowsFileIdentity
}

func (materialization windowsACLMaterialization) createdAnything() bool {
	if materialization.FileMade {
		return true
	}
	for _, step := range materialization.Chain {
		if step.Made {
			return true
		}
	}
	return false
}

// windowsACLRestoreHook observes restore order. Nil in production; the ordering
// it exposes is load-bearing and was previously pinned by nothing at all,
// despite a comment claiming otherwise.
var windowsACLRestoreHook func(path string)

type windowsACLSnapshot struct {
	Path       string
	Descriptor *windows.SECURITY_DESCRIPTOR
	Created    windowsACLMaterialization
	// TargetID is the object the DACL was read from, so the restore can prove it
	// is writing back to that same object rather than to whatever the pathname
	// resolves to at rollback time.
	//
	// A no-follow re-open catches a REPARSE POINT swapped in since apply, and
	// that is what the restore relied on. It cannot catch the other
	// substitution: rename the target aside and put an ordinary directory of the
	// same name in its place. Nothing about that decoy is a reparse point, so the
	// re-open succeeds and the old DACL is written onto the attacker's object
	// while the real one keeps the aborted setup's ACEs.
	//
	// Volume serial plus file index is the same identity the materialization
	// unwind already anchors on. See reopenWindowsACLDirectoryAsIdentity.
	TargetID windowsFileIdentity
}

func applyWindowsACLPlan(plan WindowsACLPlan) (func() error, error) {
	groups := groupWindowsACLPlanByPath(plan)
	snapshots := make([]windowsACLSnapshot, 0, len(groups))
	abort := func(err error) (func() error, error) {
		if rollbackErr := rollbackWindowsACLSnapshots(snapshots); rollbackErr != nil {
			return nil, fmt.Errorf("%w; rollback failed: %v", err, rollbackErr)
		}
		return nil, err
	}

	var deferred []windowsACLPathGroup
	for _, group := range groups {
		snapshot, applied, err := applyWindowsACLPathGroup(group)
		if err != nil {
			return abort(err)
		}
		if applied {
			snapshots = append(snapshots, snapshot)
			continue
		}
		deferred = append(deferred, group)
	}

	// A group that applied to nothing was skipped because its target did not
	// exist and the group does not materialize one. A LATER group can still
	// create it, so the skip has to be retried rather than treated as final.
	//
	// The .git rename guard is exactly this shape and was silently absent
	// because of it. .git must NOT be materialized, since an empty one breaks
	// `git init`, so its deny-delete group carries no Materialize. But .git does
	// get created, as the parent chain of the .git\config carveout, and that
	// group sorts AFTER it. So on every workspace that did not already have a
	// .git, the guard was skipped, the directory appeared moments later, and
	// nothing went back for it: the principal could then rename .git aside and
	// shed the config and hooks carveouts, which is the escape the guard exists
	// to stop. Groups that are genuinely absent simply skip again here.
	for _, group := range deferred {
		snapshot, applied, err := applyWindowsACLPathGroup(group)
		if err != nil {
			return abort(err)
		}
		if applied {
			snapshots = append(snapshots, snapshot)
		}
	}
	return func() error {
		return rollbackWindowsACLSnapshots(snapshots)
	}, nil
}

func groupWindowsACLPlanByPath(plan WindowsACLPlan) []windowsACLPathGroup {
	byPath := map[string]*windowsACLPathGroup{}
	for _, entry := range dedupeWindowsACLEntries(plan.Entries) {
		key := windowsCapabilityPathKey(entry.Path)
		if key == "" {
			continue
		}
		group := byPath[key]
		if group == nil {
			group = &windowsACLPathGroup{Path: entry.Path}
			byPath[key] = group
		}
		group.Entries = append(group.Entries, entry)
		group.Materialize = group.Materialize || entry.Materialize
		group.MaterializeFile = group.MaterializeFile || entry.MaterializeFile
		// One path is derived from at most one root, so the first anchor seen is
		// the anchor. Taking it rather than overwriting keeps a later
		// operator-named duplicate of the same path from clearing it.
		if group.Anchor == "" {
			group.Anchor = entry.Anchor
		}
	}
	out := make([]windowsACLPathGroup, 0, len(byPath))
	for _, group := range byPath {
		out = append(out, *group)
	}
	sort.Slice(out, func(i, j int) bool {
		return windowsCapabilityPathKey(out[i].Path) < windowsCapabilityPathKey(out[j].Path)
	})
	return out
}

func applyWindowsACLPathGroup(group windowsACLPathGroup) (windowsACLSnapshot, bool, error) {
	path := strings.TrimSpace(group.Path)
	if path == "" || len(group.Entries) == 0 {
		return windowsACLSnapshot{}, false, nil
	}
	// Open ONE no-follow handle to the target and drive every ACL operation
	// (read + write) through it, so the read and the write hit the same kernel
	// object. The previous pathname-based Stat/GetNamedSecurityInfo/
	// SetNamedSecurityInfo each re-resolved the path independently, so during
	// elevated setup a lower-privileged local user could swap the target for a
	// symlink/junction between operations and redirect the ACL change onto a
	// system object it never validated (issue #728, a TOCTOU privilege boundary).
	// Reject a malformed group BEFORE touching the filesystem. This validation
	// used to run after materialization, so a bad SID or an unknown action
	// created a directory chain and only then failed, leaving the error path to
	// unwind work that never needed doing. Nothing here depends on isDir: that
	// argument only selects the inheritance flag, while the errors come from the
	// action lookup and the SID parse.
	if _, _, err := prepareWindowsACLPathGroupEntries(group.Entries, false, nil); err != nil {
		return windowsACLSnapshot{}, false, err
	}

	var created windowsACLMaterialization
	var handle windows.Handle
	// Every exit from here goes through one closure, because there are now two
	// things to undo rather than one: the open handle, and whatever
	// materialization created. Both are captured by reference and both start
	// zero, so calling this before either is set is safe and does nothing.
	//
	// The unwind is handle-relative. It must never fall back to a pathname
	// delete: the failure being cleaned up here can BE the path swap, and
	// os.RemoveAll on a swapped ancestor is precisely the recursive elevated
	// delete outside the workspace that this cleanup is supposed to prevent.
	fail := func(err error) (windowsACLSnapshot, bool, error) {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		// The apply itself is failing, so nothing has been left with a DACL to
		// restore; only the removal outcome matters here.
		if _, unwindErr := rollbackWindowsACLMaterialization(created); unwindErr != nil {
			return windowsACLSnapshot{}, false, fmt.Errorf("%w; cleanup failed: %v", err, unwindErr)
		}
		return windowsACLSnapshot{}, false, err
	}

	var isDir bool
	var err error
	handle, isDir, err = openWindowsACLTargetFinalComponent(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return windowsACLSnapshot{}, false, err
		}
		if !group.Materialize {
			if windowsACLGroupRequiresExistingTarget(group) {
				return windowsACLSnapshot{}, false, fmt.Errorf("windows ACL target does not exist: %s", path)
			}
			return windowsACLSnapshot{}, false, nil
		}
		// Before creating anything: os.MkdirAll walks a pathname and follows every
		// reparse point on it, so a junction on the derived tail would have this
		// elevated setup create the directory outside the write root and only the
		// containment check below would notice, after the fact.
		if err := verifyWindowsACLPathUnderAnchor(group.Anchor, path); err != nil {
			return fail(err)
		}
		// created is assigned even on failure: materialization reports what it
		// managed to make before it stopped, and fail() unwinds exactly that.
		created, err = materializeWindowsACLTarget(path, group.MaterializeFile)
		if err != nil {
			return fail(fmt.Errorf("materialize windows ACL target %s: %w", path, err))
		}
		handle, isDir, err = openWindowsACLTargetFinalComponent(path)
		if err != nil {
			// This is the branch that fires when the post-create verify catches a
			// swap, so it is the single most important cleanup in the file.
			return fail(fmt.Errorf("open materialized windows ACL target %s: %w", path, err))
		}
	}
	// THE OBJECT HAS TO BE WHERE THE PLAN SAID IT WOULD BE. The open above
	// refuses a reparse point at the final component and resolves every one
	// above it, which is right for a path the operator named and not enough for
	// one this package derived from a write root: a junction on the derived tail
	// redirects the handle out of the sandbox with nothing about the final
	// object looking wrong.
	if err := verifyWindowsACLHandleUnderAnchor(handle, group.Anchor, path); err != nil {
		return fail(err)
	}
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fail(fmt.Errorf("read windows ACL for %s: %w", path, err))
	}
	oldDACL, _, err := descriptor.DACL()
	if err != nil {
		return fail(fmt.Errorf("read windows DACL for %s: %w", path, err))
	}
	baseDACL, accessEntries, err := prepareWindowsACLPathGroupEntries(group.Entries, isDir, oldDACL)
	if err != nil {
		return fail(err)
	}
	var nextDACL *windows.ACL
	if len(accessEntries) > 0 {
		nextDACL, err = windows.ACLFromEntries(accessEntries, baseDACL)
		if err != nil {
			return fail(fmt.Errorf("build windows ACL for %s: %w", path, err))
		}
	} else {
		nextDACL = baseDACL
	}
	if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, nextDACL, nil); err != nil {
		return fail(fmt.Errorf("apply windows ACL for %s: %w", path, err))
	}
	// The apply is committed; the retained descriptor is the rollback baseline.
	// The handle has served its purpose (read+write bound to one object) and is
	// closed now — rollback re-opens no-follow rather than holding a handle for
	// the whole sandbox lifetime, since one caller discards the rollback closure.
	// Captured BEFORE the handle closes, because that handle is the only thing
	// that names the object rather than the path.
	targetID, err := windowsIdentityOfHandle(handle)
	if err != nil {
		return fail(fmt.Errorf("read windows ACL target identity for %s: %w", path, err))
	}
	_ = windows.CloseHandle(handle)
	return windowsACLSnapshot{Path: path, Descriptor: descriptor, Created: created, TargetID: targetID}, true, nil
}

// openWindowsACLTarget opens path for reading and rewriting its DACL without
// following a final-component reparse point (FILE_FLAG_OPEN_REPARSE_POINT), and
// with FILE_FLAG_BACKUP_SEMANTICS so a directory can be opened. It returns the
// handle and whether the target is a directory. A reparse-point target is
// rejected outright: a sandbox setup target that resolves to a symlink/junction
// during elevated setup is the signature of a path-swap attack, and following it
// is exactly the redirection this guard exists to prevent. A missing target is
// surfaced as os.ErrNotExist so the caller's materialize path still fires.
func openWindowsACLTarget(path string) (windows.Handle, bool, error) {
	handle, isDir, err := openWindowsACLTargetFinalComponent(path)
	if err != nil {
		return 0, false, err
	}
	// Ancestors are resolved by CreateFile even with FILE_FLAG_OPEN_REPARSE_POINT,
	// so the final-component check is not enough on its own here: this opener
	// serves the rollback re-open and the runtime ACE, which know no anchor and
	// must not be redirected by anything on the way down.
	if err := verifyWindowsACLTargetNotRedirected(handle, path); err != nil {
		_ = windows.CloseHandle(handle)
		return 0, false, err
	}
	return handle, isDir, nil
}

// openWindowsACLTargetFinalComponent is the open without the ancestor check.
// The plan apply uses it: a reparse point ABOVE a target is the operator's
// business (a workspace on a junction or a dev drive is a configuration that
// works), and a reparse point on a tail this package derived from a write root
// is caught by verifyWindowsACLHandleUnderAnchor, which knows the anchor and
// tolerates exactly the aliases above it. That is main's rule from #1040 and
// the two containment tests pin it; the blanket refusal above is kept for the
// callers that have no anchor to reason with.
func openWindowsACLTargetFinalComponent(path string) (windows.Handle, bool, error) {
	utf16Path, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, false, fmt.Errorf("encode windows ACL target %s: %w", path, err)
	}
	handle, err := windows.CreateFile(
		utf16Path,
		windows.READ_CONTROL|windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		// syscall.Errno.Is maps ERROR_FILE_NOT_FOUND/ERROR_PATH_NOT_FOUND to
		// os.ErrNotExist, so the %w keeps the caller's errors.Is check working.
		return 0, false, fmt.Errorf("open windows ACL target %s: %w", path, err)
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = windows.CloseHandle(handle)
		return 0, false, fmt.Errorf("inspect windows ACL target %s: %w", path, err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(handle)
		return 0, false, fmt.Errorf("refusing to apply ACL to reparse-point target %s: possible path swap during elevated setup", path)
	}
	isDir := info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	return handle, isDir, nil
}

func windowsACLGroupRequiresExistingTarget(group windowsACLPathGroup) bool {
	for _, entry := range group.Entries {
		if entry.Action == WindowsACLAllowWrite {
			return true
		}
	}
	return false
}

func prepareWindowsACLPathGroupEntries(entries []WindowsACLEntry, isDir bool, oldDACL *windows.ACL) (*windows.ACL, []windows.EXPLICIT_ACCESS, error) {
	baseDACL := oldDACL
	var out []windows.EXPLICIT_ACCESS
	for _, entry := range entries {
		sid, err := windows.StringToSid(entry.Capability)
		if err != nil {
			return nil, nil, fmt.Errorf("parse windows capability SID %q: %w", entry.Capability, err)
		}
		if entry.Action == WindowsACLRevokeCapability {
			// Clear all explicit write-deny ACEs for sid from baseDACL while preserving any DenyRead
			if baseDACL != nil {
				filtered, err := windowsFilterDACL(baseDACL, sid)
				if err != nil {
					return nil, nil, err
				}
				baseDACL = filtered
			}
			continue
		}
		if entry.Action == WindowsACLDenyWrite {
			// Replace any pre-existing broader DenyWrite mask (e.g. from
			// builds that included SYNCHRONIZE) with the current narrow
			// mask. We patch the mask in-place within a DACL copy rather
			// than filtering the old ACE and re-adding via ACLFromEntries,
			// because SetEntriesInAcl merges DENY entries for the same
			// SID — which would combine the new DenyWrite with any
			// co-resident DenyRead into a single deny-all ACE.
			if baseDACL != nil && windowsHasExplicitDenyWriteForSID(baseDACL, sid) {
				migrated, err := windowsMigrateDenyWriteInDACL(baseDACL, sid, isDir, entry.NoInherit)
				if err != nil {
					return nil, nil, err
				}
				baseDACL = migrated
				continue
			}
		}
		accessMode, permissions, err := windowsACLAccess(entry.Action)
		if err != nil {
			return nil, nil, err
		}
		inheritance := uint32(0)
		// DenyDelete governs the object it names and nothing beneath it. Inherited
		// onto .git's children it would deny DELETE on every file inside, so git
		// could not remove a lock file, a ref, or anything else it rewrites, and
		// the guard would read as a broken repository rather than as a blocked
		// rename. Decided here, not in the planner, so no plan can make it inherit.
		if isDir && !entry.NoInherit && entry.Action != WindowsACLDenyDelete {
			inheritance = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
		}
		entryInheritance := inheritance
		// DenyDelete governs the object it names and nothing beneath it. Inherited
		// onto .git's children it would deny DELETE on every file inside, so git
		// could not remove a lock file, a ref, or anything else it rewrites, and
		// the guard would read as a broken repository rather than as a blocked
		// rename.
		if entry.Action == WindowsACLDenyDelete {
			entryInheritance = windows.NO_INHERITANCE
		}
		out = append(out, windows.EXPLICIT_ACCESS{
			AccessPermissions: permissions,
			AccessMode:        accessMode,
			Inheritance:       entryInheritance,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		})
	}
	return baseDACL, out, nil
}

type windowsACLHeader struct {
	AclRevision byte
	Sbz1        byte
	AclSize     uint16
	AceCount    uint16
	Sbz2        uint16
}

func windowsFilterDACL(oldDACL *windows.ACL, removeSID *windows.SID) (*windows.ACL, error) {
	if oldDACL == nil || removeSID == nil {
		return oldDACL, nil
	}
	var keepBytes uint32 = uint32(unsafe.Sizeof(windowsACLHeader{}))
	var keepCount uint16 = 0
	for i := uint32(0); i < uint32(oldDACL.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(oldDACL, i, &ace); err != nil {
			return nil, fmt.Errorf("read ACE %d for filter: %w", i, err)
		}
		if ace.Header.AceFlags&windows.INHERITED_ACE == 0 {
			if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE || ace.Header.AceType == windowsAccessDeniedObjectAceType {
				if sid, ok := windowsAceSID(ace); ok && sid.Equals(removeSID) {
					if windowsIsExperimentalWriteDenyMask(ace.Mask) {
						continue
					}
				}
			}
		}
		keepBytes += uint32(ace.Header.AceSize)
		keepCount++
	}

	buf := make([]byte, keepBytes)
	hdr := (*windowsACLHeader)(unsafe.Pointer(&buf[0]))
	oldHdr := (*windowsACLHeader)(unsafe.Pointer(oldDACL))
	*hdr = *oldHdr
	hdr.AclSize = uint16(keepBytes)
	hdr.AceCount = keepCount

	offset := unsafe.Sizeof(windowsACLHeader{})
	for i := uint32(0); i < uint32(oldDACL.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(oldDACL, i, &ace); err != nil {
			return nil, fmt.Errorf("read ACE %d for copy: %w", i, err)
		}
		aceSize := uintptr(ace.Header.AceSize)
		if ace.Header.AceFlags&windows.INHERITED_ACE == 0 {
			if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE || ace.Header.AceType == windowsAccessDeniedObjectAceType {
				if sid, ok := windowsAceSID(ace); ok && sid.Equals(removeSID) {
					if windowsIsExperimentalWriteDenyMask(ace.Mask) {
						continue
					}
					// If this is a combined read/write deny ACE, preserve its DenyRead
					// bits by stripping the write-denial bits rather than dropping the ACE.
					if ace.Mask&windowsReadContentBits != 0 && windowsHasWriteDenyBits(ace.Mask) {
						dest := buf[offset : offset+aceSize]
						srcSlice := unsafe.Slice((*byte)(unsafe.Pointer(ace)), aceSize)
						copy(dest, srcSlice)
						migratedAce := (*windows.ACCESS_ALLOWED_ACE)(unsafe.Pointer(&dest[0]))
						const legacyWriteMask = windows.FILE_GENERIC_WRITE | windows.DELETE | windowsFileDeleteChild | windows.WRITE_DAC | windows.WRITE_OWNER
						migratedAce.Mask &^= legacyWriteMask
						offset += aceSize
						continue
					}
				}
			}
		}
		srcSlice := unsafe.Slice((*byte)(unsafe.Pointer(ace)), aceSize)
		copy(buf[offset:offset+aceSize], srcSlice)
		offset += aceSize
	}

	return (*windows.ACL)(unsafe.Pointer(hdr)), nil
}

// windowsMigrateDenyWriteInDACL copies oldDACL and narrows any explicit
// deny-write ACE for targetSID to the current narrow mask, carrying requested
// inheritance (isDir, noInherit) into the migrated ACE. It drops inherit-only
// ACEs when no inheritance is requested, avoiding propagation to children.
// All other ACEs (including DenyRead) are preserved in their original positions.
func windowsMigrateDenyWriteInDACL(oldDACL *windows.ACL, targetSID *windows.SID, isDir bool, noInherit bool) (*windows.ACL, error) {
	if oldDACL == nil || targetSID == nil {
		return oldDACL, nil
	}
	_, narrowMask, err := windowsACLAccess(WindowsACLDenyWrite)
	if err != nil {
		return nil, err
	}

	const inheritFlags = byte(windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE | windows.INHERIT_ONLY_ACE | windows.NO_PROPAGATE_INHERIT_ACE)
	shouldDropInheritOnly := noInherit || !isDir

	var keepBytes uint32 = uint32(unsafe.Sizeof(windowsACLHeader{}))
	var keepCount uint16 = 0
	for i := uint32(0); i < uint32(oldDACL.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(oldDACL, i, &ace); err != nil {
			return nil, fmt.Errorf("read ACE %d for migration size: %w", i, err)
		}
		if ace.Header.AceFlags&windows.INHERITED_ACE == 0 &&
			(ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE || ace.Header.AceType == windowsAccessDeniedObjectAceType) {
			if sid, ok := windowsAceSID(ace); ok && sid.Equals(targetSID) && windowsHasWriteDenyBits(ace.Mask) {
				if shouldDropInheritOnly && (ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0) {
					// Drop inherit-only ACE when no inheritance is requested on the target
					continue
				}
			}
		}
		keepBytes += uint32(ace.Header.AceSize)
		keepCount++
	}

	buf := make([]byte, keepBytes)
	hdr := (*windowsACLHeader)(unsafe.Pointer(&buf[0]))
	oldHdr := (*windowsACLHeader)(unsafe.Pointer(oldDACL))
	*hdr = *oldHdr
	hdr.AclSize = uint16(keepBytes)
	hdr.AceCount = keepCount

	offset := unsafe.Sizeof(windowsACLHeader{})
	for i := uint32(0); i < uint32(oldDACL.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(oldDACL, i, &ace); err != nil {
			return nil, fmt.Errorf("read ACE %d for migration copy: %w", i, err)
		}
		aceSize := uintptr(ace.Header.AceSize)
		if ace.Header.AceFlags&windows.INHERITED_ACE == 0 &&
			(ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE || ace.Header.AceType == windowsAccessDeniedObjectAceType) {
			if sid, ok := windowsAceSID(ace); ok && sid.Equals(targetSID) && windowsHasWriteDenyBits(ace.Mask) {
				if shouldDropInheritOnly && (ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0) {
					continue
				}
				dest := buf[offset : offset+aceSize]
				srcSlice := unsafe.Slice((*byte)(unsafe.Pointer(ace)), aceSize)
				copy(dest, srcSlice)
				migratedAce := (*windows.ACCESS_ALLOWED_ACE)(unsafe.Pointer(&dest[0]))
				migratedAce.Mask = narrowMask
				if ace.Mask&windowsReadContentBits != 0 {
					_, readMask, _ := windowsACLAccess(WindowsACLDenyRead)
					migratedAce.Mask |= (ace.Mask & readMask)
				}
				const legacyWriteMask = windows.FILE_GENERIC_WRITE | windows.DELETE | windowsFileDeleteChild | windows.WRITE_DAC | windows.WRITE_OWNER
				migratedAce.Mask |= (ace.Mask &^ legacyWriteMask)
				if noInherit || !isDir {
					migratedAce.Header.AceFlags &^= inheritFlags
				} else if isDir {
					migratedAce.Header.AceFlags |= byte(windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE)
				}
				offset += aceSize
				continue
			}
		}

		srcSlice := unsafe.Slice((*byte)(unsafe.Pointer(ace)), aceSize)
		copy(buf[offset:offset+aceSize], srcSlice)
		offset += aceSize
	}

	return (*windows.ACL)(unsafe.Pointer(hdr)), nil
}

func windowsHasExplicitDenyWriteForSID(oldDACL *windows.ACL, wantSID *windows.SID) bool {
	if oldDACL == nil || wantSID == nil {
		return false
	}
	for index := uint16(0); index < oldDACL.AceCount; index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(oldDACL, uint32(index), &ace); err != nil {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_DENIED_ACE_TYPE && ace.Header.AceType != windowsAccessDeniedObjectAceType {
			continue
		}
		if ace.Header.AceFlags&windows.INHERITED_ACE != 0 {
			continue
		}
		sid, ok := windowsAceSID(ace)
		if !ok || !sid.Equals(wantSID) {
			continue
		}
		if windowsHasWriteDenyBits(ace.Mask) {
			return true
		}
	}
	return false
}

// windowsPreservedReadDenyAccessEntries returns DENY_ACCESS EXPLICIT_ACCESS
// entries that re-apply any non-write-related DENY ACEs for wantSID from
// oldDACL. Write-related DENY ACEs (the experimental shared/descendant
// DenyWrite shape) are intentionally omitted so migration revoke can drop
// them without also clearing a live DenyRead for the same SID.
func windowsPreservedReadDenyAccessEntries(oldDACL *windows.ACL, wantSID *windows.SID, isDir bool) ([]windows.EXPLICIT_ACCESS, error) {
	if oldDACL == nil || wantSID == nil {
		return nil, nil
	}
	var out []windows.EXPLICIT_ACCESS
	for index := uint16(0); index < oldDACL.AceCount; index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(oldDACL, uint32(index), &ace); err != nil {
			return nil, fmt.Errorf("read ACE %d while preserving read deny: %w", index, err)
		}
		if ace.Header.AceType != windows.ACCESS_DENIED_ACE_TYPE && ace.Header.AceType != windowsAccessDeniedObjectAceType {
			continue
		}
		if ace.Header.AceFlags&windows.INHERITED_ACE != 0 {
			continue
		}
		sid, ok := windowsAceSID(ace)
		if !ok || !sid.Equals(wantSID) {
			continue
		}
		if windowsIsExperimentalWriteDenyMask(ace.Mask) {
			continue
		}
		// Preserve non-write DENY ACEs (typically DenyRead for the stable
		// sandbox-home ReadOnly SID), keeping their original inheritance
		// scope rather than promoting every variant to container+object or
		// dropping inherit-only ACEs that SET_ACCESS zero-mask already cleared.
		inheritance := uint32(0)
		if isDir {
			inheritance = uint32(ace.Header.AceFlags) & (windows.OBJECT_INHERIT_ACE |
				windows.CONTAINER_INHERIT_ACE |
				windows.NO_PROPAGATE_INHERIT_ACE |
				windows.INHERIT_ONLY_ACE)
		}
		mask := ace.Mask
		const legacyWriteMask = windows.FILE_GENERIC_WRITE | windows.DELETE | windowsFileDeleteChild | windows.WRITE_DAC | windows.WRITE_OWNER
		mask &^= legacyWriteMask
		out = append(out, windows.EXPLICIT_ACCESS{
			AccessPermissions: mask,
			AccessMode:        windows.DENY_ACCESS,
			Inheritance:       inheritance,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(wantSID),
			},
		})
	}
	return out, nil
}

const (
	windowsWriteContentBits = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA |
		windows.FILE_WRITE_EA | windows.FILE_WRITE_ATTRIBUTES |
		windowsFileDeleteChild | windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER
	windowsReadContentBits = windows.FILE_READ_DATA | windows.FILE_READ_EA | windows.FILE_EXECUTE
)

// windowsHasWriteDenyBits reports whether mask contains content-write, delete,
// or ownership deny bits that indicate write denial (whether pure or combined
// with read denial).
func windowsHasWriteDenyBits(mask windows.ACCESS_MASK) bool {
	_, writeMask, err := windowsACLAccess(WindowsACLDenyWrite)
	if err != nil {
		return false
	}
	if mask&writeMask == writeMask {
		return true
	}
	return mask&windowsWriteContentBits != 0
}

// windowsIsExperimentalWriteDenyMask reports whether mask is a synthetic
// DenyWrite (or partial write deny) from earlier broadening builds without any
// co-resident DenyRead bits — the only ACEs migration revoke may drop for the
// stable ReadOnly SID. If the mask also denies read-content bits, it is a combined
// read/write deny rather than a pure experimental write deny.
func windowsIsExperimentalWriteDenyMask(mask windows.ACCESS_MASK) bool {
	if !windowsHasWriteDenyBits(mask) {
		return false
	}
	return mask&windowsReadContentBits == 0
}

func windowsAceSID(ace *windows.ACCESS_ALLOWED_ACE) (sid *windows.SID, ok bool) {
	switch ace.Header.AceType {
	case windows.ACCESS_ALLOWED_ACE_TYPE, windows.ACCESS_DENIED_ACE_TYPE, windowsAccessAllowedCallbackAceType:
		return (*windows.SID)(unsafe.Pointer(&ace.SidStart)), true
	case windowsAccessAllowedObjectAceType, windowsAccessDeniedObjectAceType, windowsAccessAllowedCallbackObjectAceType:
		flags := ace.SidStart
		offset := unsafe.Sizeof(ace.SidStart)
		if flags&windows.ACE_OBJECT_TYPE_PRESENT != 0 {
			offset += 16
		}
		if flags&windows.ACE_INHERITED_OBJECT_TYPE_PRESENT != 0 {
			offset += 16
		}
		return (*windows.SID)(unsafe.Pointer(uintptr(unsafe.Pointer(&ace.SidStart)) + offset)), true
	default:
		return nil, false
	}
}

func windowsACLAccess(action WindowsACLAction) (windows.ACCESS_MODE, windows.ACCESS_MASK, error) {
	switch action {
	case WindowsACLAllowWrite:
		// DELETE is part of the grant, not an extra. FILE_GENERIC_WRITE covers
		// creating and modifying but not removing or renaming, and a rename needs
		// delete access on the source. Under the old same-user token that gap was
		// invisible, because the caller already held inherited rights on its own
		// tree; a sandbox principal is a separate account with no such
		// inheritance, so without DELETE it can write a file it can never delete.
		// Ordinary editing and most git operations rewrite files by replacing
		// them, so the omission fails normal work rather than an edge case.
		//
		// FILE_DELETE_CHILD is deliberately NOT granted, for the same reason
		// WRITE_DAC and WRITE_OWNER are not. On a parent it authorises deleting a
		// child whatever the child's own DACL says, so granting it on a write root
		// hands back the write-denied carve-outs underneath it: a principal could
		// delete .git/config and recreate it, and the replacement inherits this
		// grant with no deny of its own — restoring exactly the credential.helper
		// and core.hooksPath control the carve-out exists to prevent.
		//
		// It was granted here originally to keep the mask symmetric with
		// WindowsACLDenyWrite, which does treat FILE_DELETE_CHILD as part of
		// write. Symmetry is the wrong goal: denying a capability is not a reason
		// to grant it. DELETE alone covers removing and renaming files the
		// principal owns inside its roots, which is what the grant is for.
		return windows.GRANT_ACCESS, windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE | windows.FILE_GENERIC_EXECUTE | windows.DELETE, nil
	case WindowsACLAllowRead:
		// Read and traverse without write. A sandbox principal is a separate
		// account with no inherent access to the caller's tree, so a read-only
		// root has to be granted rather than assumed. Deliberately omits
		// FILE_GENERIC_WRITE, DELETE and WRITE_DAC.
		return windows.GRANT_ACCESS, windows.FILE_GENERIC_READ | windows.FILE_GENERIC_EXECUTE, nil
	case windowsACLRevoke:
		// REVOKE_ACCESS drops every ACE naming the trustee regardless of the mask,
		// so the mask is ignored here. Used to retire a principal without having
		// to remember which access each path was granted.
		return windows.REVOKE_ACCESS, 0, nil
	case WindowsACLDenyRead:
		return windows.DENY_ACCESS, windows.FILE_GENERIC_READ | windows.FILE_GENERIC_EXECUTE, nil
	case WindowsACLDenyWrite:
		return windows.DENY_ACCESS, (windows.FILE_GENERIC_WRITE | windows.DELETE | windowsFileDeleteChild | windows.WRITE_DAC | windows.WRITE_OWNER) &^ windows.SYNCHRONIZE, nil
	case WindowsACLDenyDelete:
		// Deny removing or RENAMING the object itself, nothing more. Renaming a
		// directory needs DELETE on that directory, so denying DELETE is what
		// stops .git being moved aside and recreated without its carveouts.
		//
		// WRITE_DAC and WRITE_OWNER come along because a guard the principal can
		// rewrite, or take ownership of and then rewrite, is not a guard.
		//
		// FILE_GENERIC_WRITE is deliberately absent: git writes index, objects and
		// refs constantly, and denying it would break every commit rather than the
		// rename. FILE_DELETE_CHILD is absent for the same reason one level down,
		// since git deletes its own lock files and refs. Neither is needed here:
		// this ACE does not inherit (see prepareWindowsACLPathGroupEntries), so it
		// governs the .git directory object alone.
		return windows.DENY_ACCESS, windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER, nil
	case WindowsACLRevokeCapability:
		// Handled specially in prepareWindowsACLPathGroupEntries (preserve DenyRead).
		return windows.SET_ACCESS, 0, nil
	default:
		return 0, 0, fmt.Errorf("unsupported windows ACL action %q", action)
	}
}

func rollbackWindowsACLSnapshots(snapshots []windowsACLSnapshot) error {
	var errs []error
	// Reverse order is load-bearing twice over. Sorted ascending by path key, an
	// ancestor key is always a proper prefix of its descendants, so walking
	// backwards unwinds descendant before ancestor: a materialized directory is
	// therefore empty by the time its own removal is attempted. Restoring ACLs in
	// the same order is independently right, because SetSecurityInfo propagates
	// inheritable ACEs down, so the ancestor must go last.
	// TestRollbackUnwindsDescendantsBeforeAncestors pins it.
	//
	// SORTED HERE, NOT ASSUMED. The groups are sorted, but the snapshots are in
	// APPLY order, and applyWindowsACLPlan's deferred pass appends a group whose
	// target only appeared during the first pass, which is typically an ancestor
	// of first-pass groups (.git after .git\config). Walking that list backwards
	// restored the ancestor first. Reported by CodeRabbit;
	// TestRollbackUnwindsADeferredAncestorAfterItsDescendants pins it.
	snapshots = slices.Clone(snapshots)
	sort.SliceStable(snapshots, func(i, j int) bool {
		return windowsCapabilityPathKey(snapshots[i].Path) < windowsCapabilityPathKey(snapshots[j].Path)
	})
	for index := len(snapshots) - 1; index >= 0; index-- {
		snapshot := snapshots[index]
		if snapshot.Created.createdAnything() {
			removed, err := rollbackWindowsACLMaterialization(snapshot.Created)
			if err != nil {
				errs = append(errs, err)
			}
			// Only skip the restore when the object carrying the DACL is actually
			// gone. A racer can win the leaf while this run made its parent, and
			// then the aggregate says "ours" about a file that is not: the parent
			// cannot be removed because it is non-empty, and skipping here left a
			// pre-existing file wearing an aborted setup's ACL. Falling through
			// reaches the same no-follow, TargetID-guarded restore as any other
			// snapshot, which refuses on a mismatch rather than forcing.
			if removed {
				continue
			}
		}
		dacl, _, err := snapshot.Descriptor.DACL()
		if err != nil {
			errs = append(errs, fmt.Errorf("read rollback windows DACL for %s: %w", snapshot.Path, err))
			continue
		}
		// Re-open no-follow AND prove it is the same object. The no-follow open
		// alone only rules out a reparse point swapped in since apply; a target
		// renamed aside and replaced by an ordinary directory of the same name
		// passes it, and the old DACL would land on the decoy while the real
		// object kept the aborted setup's ACEs. See windowsACLSnapshot.TargetID.
		handle, _, err := openWindowsACLTargetFinalComponent(snapshot.Path)
		if err != nil {
			errs = append(errs, fmt.Errorf("re-open windows ACL target %s for rollback: %w", snapshot.Path, err))
			continue
		}
		// An identity to compare against makes the pathname route irrelevant: the
		// object either is the one the DACL was read from or it is not. Without
		// one, the route is all there is, so keep the blanket redirection check.
		if snapshot.TargetID.empty() {
			if err := verifyWindowsACLTargetNotRedirected(handle, snapshot.Path); err != nil {
				_ = windows.CloseHandle(handle)
				errs = append(errs, fmt.Errorf("re-open windows ACL target %s for rollback: %w", snapshot.Path, err))
				continue
			}
		}
		if !snapshot.TargetID.empty() {
			got, identityErr := windowsIdentityOfHandle(handle)
			if identityErr != nil {
				_ = windows.CloseHandle(handle)
				errs = append(errs, fmt.Errorf("read rollback identity for %s: %w", snapshot.Path, identityErr))
				continue
			}
			if got != snapshot.TargetID {
				// REFUSED, not forced through. Leaving the real object with the
				// aborted setup's ACEs is the safe direction: the caller is failing
				// anyway, and writing a stale DACL onto an object setup never
				// touched is the one outcome rollback must not produce.
				_ = windows.CloseHandle(handle)
				errs = append(errs, fmt.Errorf("refusing to restore windows ACL for %s: it is no longer the object setup applied to", snapshot.Path))
				continue
			}
		}
		if windowsACLRestoreHook != nil {
			windowsACLRestoreHook(snapshot.Path)
		}
		if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
			errs = append(errs, fmt.Errorf("rollback windows ACL for %s: %w", snapshot.Path, err))
		}
		_ = windows.CloseHandle(handle)
	}
	return errors.Join(errs...)
}

// materializeWindowsACLTarget creates a missing ACL target with the shape the
// owning tool expects. A directory target is created whole; a file target gets
// its parent chain created and then an empty file, because creating it as a
// directory would break the tool that owns it rather than just mis-ACL it.
// The returned record is meaningful even when the error is non-nil: a chain that
// got three levels deep and then failed still has three levels to unwind.
func materializeWindowsACLTarget(path string, asFile bool) (windowsACLMaterialization, error) {
	directory := path
	leaf := ""
	if asFile {
		directory = filepath.Dir(path)
		leaf = filepath.Base(path)
	}
	created, parent, err := makeWindowsACLDirChainNoFollow(directory)
	if err != nil {
		return created, err
	}
	defer func() { _ = windows.CloseHandle(parent) }()
	if !asFile {
		return created, nil
	}
	// windowsACLRacedLeafHook fires at the ONE instant a racer can win the leaf
	// while this run already owns the chain above it: the directories exist and
	// the file does not yet. That mixed ownership is what made the old rollback
	// skip its restore, and it is unreachable from the anchor-stage hook, which
	// fires before the chain exists at all. Always nil in production.
	if windowsACLRacedLeafHook != nil {
		windowsACLRacedLeafHook(path)
	}
	// A racing creator winning is still fine: the target exists, which is all
	// materialization needed. createWindowsACLChildFile reports that as
	// created=false, so rollback will not delete a file the sandbox did not make.
	created.File = leaf
	madeFile, fileID, err := createWindowsACLChildFile(parent, leaf)
	created.FileMade = madeFile
	if madeFile {
		created.FileID = fileID
	}
	return created, err
}

// rollbackWindowsACLMaterialization removes exactly what materialization
// created, deepest first, without resolving any pathname below the anchor.
//
// This is the other half of the pathname problem. The old cleanup called
// os.RemoveAll on the target pathname, which re-resolves every ancestor at the
// moment it runs, so an ancestor swapped to a junction after the object was
// created sent a recursive elevated delete into an unrelated tree. It also only
// ever removed the final component, leaving every intermediate directory the
// chain had created behind.
//
// The anchor is the one pathname that has to be resolved again, and it is
// checked by file identity rather than by name: replacing a directory with
// another REAL directory of the same name needs no reparse point at all and
// would otherwise pass every no-follow check there is.
//
// Residue is preferable to over-deletion throughout. When something cannot be
// removed safely this reports it and leaves it, and never falls back to a
// pathname delete.
// rollbackWindowsACLMaterialization removes what this run created and reports
// whether the ACL-BEARING TARGET is among what it removed.
//
// AGGREGATE OWNERSHIP IS NOT THE TARGET'S DISPOSITION. The caller used to treat
// createdAnything() as "this run owns the object it applied a DACL to", which is
// an OR across the whole materialization record. When this run creates the
// parent chain and a racer wins the leaf, that is true while the ACL-bearing
// file belongs to somebody else. The caller then skipped the identity-validated
// restore and left a pre-existing file carrying an aborted setup's ACL.
//
// The target is the file leaf when FileMade is set, and the deepest chain entry
// otherwise. Anything this cannot prove it removed reports false, which routes
// the caller to a restore that is already no-follow and TargetID-guarded, so the
// conservative direction is the default.
func rollbackWindowsACLMaterialization(materialization windowsACLMaterialization) (targetRemoved bool, err error) {
	if !materialization.createdAnything() {
		return false, nil
	}
	anchor, anchorErr := reopenWindowsACLDirectoryAsIdentity(materialization.AnchorPath, materialization.AnchorID)
	if anchorErr != nil {
		if errors.Is(anchorErr, os.ErrNotExist) {
			// The anchor is gone, so everything created beneath it is gone too,
			// the target included. Nothing to undo, and no way to undo it.
			return true, nil
		}
		return false, fmt.Errorf("unwind windows ACL materialization under %s: %w", materialization.AnchorPath, anchorErr)
	}

	// One handle per level: handles[i] is the parent of Chain[i], which is what
	// deleting Chain[i] relative to a pinned parent requires.
	handles := []windows.Handle{anchor}
	defer func() {
		for _, handle := range handles {
			if handle != 0 {
				_ = windows.CloseHandle(handle)
			}
		}
	}()

	// Descend only as far as is actually required. Removing a directory needs its
	// PARENT's handle, not its own, so the deepest component is opened only when
	// a file leaf lives inside it. This is not just economy: the deepest
	// component is usually the ACL target itself, so it may already carry the
	// deny-read ACE this rollback is undoing, and opening it would be refused by
	// the very ACL being unwound.
	needed := len(materialization.Chain)
	if !materialization.FileMade && needed > 0 {
		needed--
	}
	depth := 0
	for ; depth < needed; depth++ {
		child, openErr := openWindowsACLChildDirectory(handles[depth], materialization.Chain[depth].Name)
		if openErr != nil {
			// Already removed by something else. Stop descending; whatever is
			// below it is gone with it.
			if isWindowsNotExist(openErr) {
				break
			}
			return false, fmt.Errorf("unwind windows ACL materialization under %s: %w", materialization.AnchorPath, openErr)
		}
		// A component this run created has to still be the object it created
		// before anything beneath it is touched: an ordinary directory renamed
		// into its place beneath the unchanged anchor is not ours to descend
		// into, let alone delete.
		if step := materialization.Chain[depth]; step.Made {
			got, identityErr := windowsIdentityOfHandle(child)
			if identityErr != nil {
				_ = windows.CloseHandle(child)
				return false, fmt.Errorf("unwind windows ACL materialization under %s: identify %s: %w", materialization.AnchorPath, step.Name, identityErr)
			}
			if got != step.ID {
				_ = windows.CloseHandle(child)
				return false, fmt.Errorf("refusing to unwind windows ACL materialization under %s: %s is no longer the directory setup created", materialization.AnchorPath, step.Name)
			}
		}
		handles = append(handles, child)
	}

	var errs []error
	// An ancestor vanished mid-descent, so the target went with it.
	targetRemoved = depth < needed
	// The file leaf lives inside the deepest chain directory, so it goes first
	// and only if the descent actually reached that far.
	if materialization.FileMade && depth == needed {
		if err := deleteWindowsACLChildFileAs(handles[len(handles)-1], materialization.File, materialization.FileID); err != nil {
			errs = append(errs, fmt.Errorf("remove materialized windows ACL file %s: %w", materialization.File, err))
		} else {
			targetRemoved = true
		}
	}
	// Chain[i] is removed through handles[i], so the deepest one that can be
	// removed is bounded by how far the descent actually got.
	deepest := len(handles) - 1
	if last := len(materialization.Chain) - 1; deepest > last {
		deepest = last
	}
	for index := deepest; index >= 0; index-- {
		// Close the child's own handle, if the descent opened one, before asking
		// its parent to remove it. A directory with a live handle open still
		// counts as present, so the parent's delete would come back as non-empty.
		if index+1 < len(handles) && handles[index+1] != 0 {
			_ = windows.CloseHandle(handles[index+1])
			handles[index+1] = 0
		}
		if !materialization.Chain[index].Made {
			continue
		}
		if err := deleteWindowsACLChildDirectoryAs(handles[index], materialization.Chain[index].Name, materialization.Chain[index].ID); err != nil {
			errs = append(errs, fmt.Errorf("remove materialized windows ACL directory %s: %w", materialization.Chain[index].Name, err))
			continue
		}
		// The deepest chain entry IS the ACL target for a directory
		// materialization, and it is the leaf's parent when a racer made the leaf,
		// so removing it means the target is gone either way.
		if !materialization.FileMade && index == len(materialization.Chain)-1 {
			targetRemoved = true
		}
	}
	return targetRemoved, errors.Join(errs...)
}

// windowsACLMaterializeSwapHook is a test seam and nothing else. It fires inside
// makeWindowsACLDirChainNoFollow at the exact instant the race used to be
// exploitable: the anchor is verified and pinned, and nothing has been created
// yet. A race reproducible only by luck is not a regression test, so the instant
// is made addressable rather than hoped for. Always nil in production.
var windowsACLMaterializeSwapHook func(anchor string)

// windowsACLRacedLeafHook fires after the directory chain is created and before
// the file leaf is. See materializeWindowsACLTarget. Always nil in production.
var windowsACLRacedLeafHook func(path string)

// makeWindowsACLDirChainNoFollow is an os.MkdirAll that never resolves a
// pathname below its anchor.
//
// It walks UP to the deepest ancestor that already exists and opens it
// no-follow. Because GetFinalPathNameByHandle answers for the whole resolved
// path, that single check clears every ancestor above it too. Then it walks back
// DOWN, creating one component at a time relative to the HANDLE of the level
// above, so the tree it descends is pinned to objects rather than named by
// strings.
//
// That is the difference that matters. This used to verify a component by
// pathname, close the handle, and then hand the same string to os.Mkdir: two
// independent kernel resolutions with a gap between them. A workspace owner who
// swapped the verified ancestor for a junction inside that gap got the component
// created outside the approved tree, as Administrator, and verifying again
// afterwards cannot un-create it. Junctions need no privilege, so this was
// reachable by exactly the unprivileged user the sandbox exists to contain.
//
// It deliberately does NOT re-verify each created component by pathname. A child
// created relative to a pinned parent is in the right place by construction, so
// comparing pathnames afterwards would add nothing and would reject correct
// creates whenever the tree was legitimately renamed mid-setup.
//
// Returns what it created, plus an open handle to the deepest directory which
// the caller must close.
func makeWindowsACLDirChainNoFollow(dir string) (windowsACLMaterialization, windows.Handle, error) {
	cleaned := filepath.Clean(strings.TrimSpace(dir))
	if cleaned == "" || cleaned == "." {
		return windowsACLMaterialization{}, 0, fmt.Errorf("materialize windows ACL target: empty directory path %q", dir)
	}

	// Walk up to the deepest ancestor that exists, collecting the component
	// NAMES that are missing. Names, not paths: everything below the anchor is
	// addressed relative to a handle from here on.
	var missing []string
	current := cleaned
	var anchor windows.Handle
	var anchorID windowsFileIdentity
	for {
		handle, identity, err := openWindowsACLDirectoryNoFollowWithIdentity(current)
		if err == nil {
			anchor, anchorID = handle, identity
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return windowsACLMaterialization{}, 0, err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return windowsACLMaterialization{}, 0, fmt.Errorf("materialize windows ACL target %s: no existing ancestor to anchor on", dir)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}

	created := windowsACLMaterialization{AnchorPath: current, AnchorID: anchorID}

	if hook := windowsACLMaterializeSwapHook; hook != nil {
		hook(current)
	}

	// Walk back down, one component per handle. The anchor handle is released as
	// soon as its child is open, so at most two levels are held at once.
	parent := anchor
	for index := len(missing) - 1; index >= 0; index-- {
		name := missing[index]
		child, madeNow, err := createWindowsACLChildDirectory(parent, name)
		if err != nil {
			_ = windows.CloseHandle(parent)
			return created, 0, err
		}
		step := windowsACLChainStep{Name: name, Made: madeNow}
		if madeNow {
			if step.ID, err = windowsIdentityOfHandle(child); err != nil {
				_ = windows.CloseHandle(parent)
				_ = windows.CloseHandle(child)
				return created, 0, fmt.Errorf("identify materialized windows ACL directory %s: %w", name, err)
			}
		}
		created.Chain = append(created.Chain, step)
		_ = windows.CloseHandle(parent)
		parent = child
	}
	return created, parent, nil
}
