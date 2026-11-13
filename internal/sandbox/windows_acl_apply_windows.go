//go:build windows

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	Anchor      string
	Entries     []WindowsACLEntry
	Materialize bool
}

type windowsACLSnapshot struct {
	Path         string
	Descriptor   *windows.SECURITY_DESCRIPTOR
	Materialized bool
	// Identity is the object the forward apply actually modified, captured from
	// the open handle. Compensation reopens BY NAME, and a name is not an
	// object: see rollbackWindowsACLSnapshots.
	Identity windowsObjectIdentity
}

// windowsObjectIdentity identifies a filesystem object independently of the
// name it currently answers to.
type windowsObjectIdentity struct {
	volume uint32
	high   uint32
	low    uint32
	valid  bool
}

func windowsIdentityFromHandle(handle windows.Handle) windowsObjectIdentity {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return windowsObjectIdentity{}
	}
	return windowsObjectIdentity{
		volume: info.VolumeSerialNumber,
		high:   info.FileIndexHigh,
		low:    info.FileIndexLow,
		valid:  true,
	}
}

// matches is deliberately false when either side is unknown. A compensation
// that cannot prove it is acting on the object it changed must not act.
func (id windowsObjectIdentity) matches(other windowsObjectIdentity) bool {
	return id.valid && other.valid &&
		id.volume == other.volume && id.high == other.high && id.low == other.low
}

// windowsACLStampRequest asks the apply to write the runtime setup stamp THROUGH
// THE SAME HANDLE it just applied the capability ACE through.
//
// The stamp exists to prove that the directory a command later uses is the
// object setup granted the ACE to. Writing it afterwards by pathname cannot
// prove that, however carefully the second open is done: the ACE goes on
// through a rooted handle, that handle closes, network setup runs, and only then
// does the marker write re-open the name. A local process that can reach the
// user-owned runtime tree can remove the predictable root in that window and
// put an ordinary directory in its place. The re-open correctly rejects a
// junction, but an ordinary replacement is not a reparse point and is not the
// ACL-bearing object either, so it collects a valid-looking stamp. Marker
// validation then passes over a directory with no capability ACE, and the next
// WRITE_RESTRICTED command fails its cache and TMP writes with setup insisting
// it is current.
//
// Closing that means never naming the target again after the ACE lands. The
// hash is known before the apply, so the stamp can simply ride along.
type windowsACLStampRequest struct {
	Root     string
	PlanHash string
	// RootIdentity is the file identity of the runtime directory the SNAPSHOT
	// read, and RootIdentified says whether that capture actually succeeded.
	//
	// TWO OPENS OF ONE NAME ARE NOT ONE OBJECT. The snapshot proved "these prior
	// bytes belong to B" through its own handle and then closed it; the apply
	// resolved the same pathname again and mutated whatever answered, without
	// either of them ever proving B and that object are the same. The root owner
	// can rename the prior root aside, put an ordinary directory at the
	// predictable name for the snapshot, and restore the original before the
	// apply. The setup lease is a sibling and does not bind the root entry.
	//
	// The consequence was not merely a wrong ACL. On a later failure, stamp
	// compensation compared the object it found against the snapshot's identity,
	// refused to restore, and left this run's stamp on a directory whose marker
	// still described the previous successful setup: a failed setup invalidating a
	// good one.
	RootIdentity   string
	RootIdentified bool
}

// windowsACLStampSwapHook fires in the exact window this design closes: after
// the capability ACE is on the object and before the stamp is written. Nil in
// production; a test uses it to replace the runtime root with an ordinary
// directory, which is what a local process would do.
var windowsACLStampSwapHook func(path string)

// windowsACLStampWriteHook replaces the ride-along stamp write. Nil in
// production; a test uses it to reach the post-commit failure path, which no
// ordinary input produces once the bound handle is already open.
var windowsACLStampWriteHook func(path string) error

// verifyStampRootIdentity refuses when the object this apply holds is not the one
// the snapshot read.
//
// windowsACLStampIdentitySwapHook exists so a test can substitute the directory
// in exactly the interval between the two opens, which is otherwise unreachable
// from any ordinary input.
func verifyStampRootIdentity(handle windows.Handle, path string, stamp *windowsACLStampRequest) error {
	if !stamp.RootIdentified {
		return fmt.Errorf("the sandbox runtime root %s could not be identified when its prior state was recorded, so this setup cannot prove it is about to change the same directory", path)
	}
	identity, err := handleRuntimeIdentity(handle)
	if err != nil {
		return fmt.Errorf("identify the sandbox runtime root %s before applying its ACL: %w", path, err)
	}
	if identity != stamp.RootIdentity {
		return fmt.Errorf("the sandbox runtime root %s is no longer the directory this run recorded the prior state of, so applying the ACL and stamp here would attest to an object nobody inspected", path)
	}
	return nil
}

// windowsACLStampIdentitySwapHook fires in the interval between the snapshot's
// close and the apply's open, which is where a substitution can actually land.
// Nil in production.
var windowsACLStampIdentitySwapHook func(path string)

// windowsSameRuntimeRootPath compares two runtime roots the way the filesystem
// does, so the stamp rides along with the right target regardless of spelling.
// Beside its only caller: in the shared file it was dead code on every other
// platform.
func windowsSameRuntimeRootPath(left, right string) bool {
	left = filepath.Clean(strings.TrimSpace(left))
	right = filepath.Clean(strings.TrimSpace(right))
	if left == "" || right == "" {
		return false
	}
	return strings.EqualFold(left, right)
}

// writeRidingStamp writes the stamp through the handle the capability ACE was
// applied on, or through the test hook when one is installed.
func writeRidingStamp(handle windows.Handle, path string, planHash string) error {
	if windowsACLStampWriteHook != nil {
		return windowsACLStampWriteHook(path)
	}
	return writeWindowsRuntimeStampToDirectoryHandle(handle, windowsSandboxRuntimeStampName(planHash), planHash)
}

// restoreWindowsACLThroughHandle puts a captured DACL back on the object the
// handle names, by handle rather than by pathname for the same reason the stamp
// rides along: after the apply, the name is no longer proof of the object.
func restoreWindowsACLThroughHandle(handle windows.Handle, descriptor *windows.SECURITY_DESCRIPTOR) error {
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return fmt.Errorf("read the captured windows DACL: %w", err)
	}
	return windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

func applyWindowsACLPlan(plan WindowsACLPlan) (func() error, error) {
	return applyWindowsACLPlanWithStamp(plan, nil)
}

func applyWindowsACLPlanWithStamp(plan WindowsACLPlan, stamp *windowsACLStampRequest) (func() error, error) {
	groups := groupWindowsACLPlanByPath(plan)
	snapshots := make([]windowsACLSnapshot, 0, len(groups))
	for _, group := range groups {
		snapshot, applied, err := applyWindowsACLPathGroupWithStamp(group, stamp)
		if err != nil {
			rollbackErr := rollbackWindowsACLSnapshots(snapshots)
			if rollbackErr != nil {
				return nil, fmt.Errorf("%w; rollback failed: %v", err, rollbackErr)
			}
			return nil, err
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
	return applyWindowsACLPathGroupWithStamp(group, nil)
}

func applyWindowsACLPathGroupWithStamp(group windowsACLPathGroup, stamp *windowsACLStampRequest) (windowsACLSnapshot, bool, error) {
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
	// THE INTERVAL IS HERE, BEFORE THIS OPEN. The snapshot read its object through
	// its own handle and closed it; a handle already open cannot be renamed out
	// from under itself, so the only place a substitution can land is between that
	// close and this open. The hook exists so a test can put it exactly there.
	if windowsACLStampIdentitySwapHook != nil && stamp != nil && windowsSameRuntimeRootPath(stamp.Root, path) {
		windowsACLStampIdentitySwapHook(path)
	}
	materialized := false
	handle, isDir, err := openWindowsACLTarget(path)
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
			return windowsACLSnapshot{}, false, err
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			return windowsACLSnapshot{}, false, fmt.Errorf("materialize windows ACL target %s: %w", path, err)
		}
		materialized = true
		handle, isDir, err = openWindowsACLTarget(path)
		if err != nil {
			_ = os.RemoveAll(path)
			return windowsACLSnapshot{}, false, fmt.Errorf("open materialized windows ACL target %s: %w", path, err)
		}
	}
	// From here the handle is open; every early return must close it first (and
	// remove a freshly materialized target) so a failure leaks neither.
	fail := func(err error) (windowsACLSnapshot, bool, error) {
		_ = windows.CloseHandle(handle)
		if materialized {
			_ = os.RemoveAll(path)
		}
		return windowsACLSnapshot{}, false, err
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
	// BEFORE THE FIRST MUTATION, NOT BEFORE THE STAMP. Checking later would leave
	// the ACL already applied to the wrong object, which is the change that
	// matters most.
	//
	// Fails CLOSED. An unestablished identity refuses rather than passing, because
	// "we could not tell" is exactly the case this exists for; treating it as
	// permission would make the guard a no-op precisely when it is needed.
	if stamp != nil && windowsSameRuntimeRootPath(stamp.Root, path) {
		if err := verifyStampRootIdentity(handle, path, stamp); err != nil {
			return fail(err)
		}
	}
	if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, nextDACL, nil); err != nil {
		return fail(fmt.Errorf("apply windows ACL for %s: %w", path, err))
	}
	// The apply is committed; the retained descriptor is the rollback baseline.
	// The handle has served its purpose (read+write bound to one object) and is
	// closed now — rollback re-opens no-follow rather than holding a handle for
	// the whole sandbox lifetime, since one caller discards the rollback closure.
	// BEFORE THE HANDLE CLOSES, and only for the target the stamp names. This is
	// the whole point: the ACE and the stamp land on one kernel object with no
	// pathname resolution in between.
	if stamp != nil && windowsSameRuntimeRootPath(stamp.Root, path) {
		if windowsACLStampSwapHook != nil {
			windowsACLStampSwapHook(path)
		}
		if err := writeRidingStamp(handle, path, stamp.PlanHash); err != nil {
			// THE ACE AND ITS STAMP ARE ONE TRANSACTION.
			//
			// SetSecurityInfo above has already committed, and this function
			// returns no rollback closure on its error paths, so the caller has
			// nothing to compensate with. Without this restore a failed setup
			// reports failure while leaving the capability grant on a pre-existing
			// runtime root: the tree stays writable by the restricted token and
			// nothing on disk records that it should not be.
			if restoreErr := restoreWindowsACLThroughHandle(handle, descriptor); restoreErr != nil {
				return fail(fmt.Errorf("stamp windows ACL target %s: %w (the committed ACL could not be restored either: %v)", path, err, restoreErr))
			}
			return fail(fmt.Errorf("stamp windows ACL target %s: %w", path, err))
		}
	}
	// Captured while the handle is still open, because this is the last moment
	// the object and the name are known to be the same thing.
	identity := windowsIdentityFromHandle(handle)
	_ = windows.CloseHandle(handle)
	return windowsACLSnapshot{Path: path, Descriptor: descriptor, Materialized: materialized, Identity: identity}, true, nil
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
	// A RUNTIME ROOT IS OPENED BY HANDLE, NOT BY NAME.
	//
	// FILE_FLAG_OPEN_REPARSE_POINT below protects only the FINAL component; every
	// ancestor in the pathname is resolved normally. The runtime tail is the one
	// part of the tree Zero creates and therefore the one part an unprivileged
	// local user can predict and pre-empt, and junctions need no privilege, so a
	// swap at an owned ancestor between the last check and this open redirects the
	// elevated capability ACL into a directory of their choosing.
	//
	// Everything else here is the user's own tree, where an ancestor reparse point
	// is ordinary configuration and following it is correct.
	if _, _, owned := windowsSandboxRuntimeOwnedTail(path); owned {
		handle, err := openWindowsRuntimeTailDirectory(path, windows.READ_CONTROL|windows.WRITE_DAC|windows.FILE_TRAVERSE)
		if err != nil {
			return 0, false, err
		}
		return handle, true, nil
	}
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
		if isDir && !entry.NoInherit {
			inheritance = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
		}
		out = append(out, windows.EXPLICIT_ACCESS{
			AccessPermissions: permissions,
			AccessMode:        accessMode,
			Inheritance:       inheritance,
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
		return windows.GRANT_ACCESS, windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE | windows.FILE_GENERIC_EXECUTE, nil
	case WindowsACLDenyRead:
		return windows.DENY_ACCESS, windows.FILE_GENERIC_READ | windows.FILE_GENERIC_EXECUTE, nil
	case WindowsACLDenyWrite:
		return windows.DENY_ACCESS, (windows.FILE_GENERIC_WRITE | windows.DELETE | windowsFileDeleteChild | windows.WRITE_DAC | windows.WRITE_OWNER) &^ windows.SYNCHRONIZE, nil
	case WindowsACLRevokeCapability:
		// Handled specially in windowsExplicitAccessEntries (preserve DenyRead).
		return windows.SET_ACCESS, 0, nil
	default:
		return 0, 0, fmt.Errorf("unsupported windows ACL action %q", action)
	}
}

func rollbackWindowsACLSnapshots(snapshots []windowsACLSnapshot) error {
	var errs []error
	for index := len(snapshots) - 1; index >= 0; index-- {
		snapshot := snapshots[index]
		// A NAME IS NOT AN OBJECT ONCE THE APPLY HANDLE HAS CLOSED.
		//
		// The forward apply and its stamp go through one handle, so they are
		// provably about one object. Compensation runs later, after a network or
		// marker failure, and resolves these names again. Opening no-follow stops
		// a reparse point but accepts an ORDINARY directory moved into the name
		// since: the original is renamed aside, a substitute is created, and
		// rollback then restores the pre-apply DACL onto the substitute, strips a
		// stamp there, and reports success, while the moved original keeps this
		// run's capability ACE and a valid stamp. Setup would claim a completed
		// rollback with the modified object still reachable elsewhere, having
		// also mutated something it never touched going forward.
		//
		// So every compensation proves it holds the object it changed, and
		// otherwise leaves the substitute alone and says plainly what was left
		// behind.
		if snapshot.Materialized {
			// NOT identity-checked, and the reason is a real limit rather than an
			// oversight. A materialized target is one this run created, and its
			// plan routinely denies Everyone read (that is what a protected
			// metadata carve-out IS), so the attributes identity needs cannot be
			// read back even by the owner: the check turned rollback of every
			// materialized directory into "Access is denied". Establishing
			// identity here would mean holding the apply handle open until the
			// last failure point, which is a larger change than this one.
			if err := os.RemoveAll(snapshot.Path); err != nil {
				errs = append(errs, fmt.Errorf("remove materialized windows ACL target %s: %w", snapshot.Path, err))
			}
			continue
		}
		// ONE OPEN, AND THE IDENTITY COMES FROM THE HANDLE THAT GETS MUTATED.
		//
		// Checking identity through a separate open and then resolving the name
		// again for the restore proves nothing about the second handle: the two
		// opens are a check-then-use, and the fact established (this NAME resolved
		// to the object we changed) is not the fact the write depends on (this
		// HANDLE is that object). Opening once and asking the handle who it is
		// removes the window rather than narrowing it.
		handle, _, err := openWindowsACLTarget(snapshot.Path)
		if err != nil {
			errs = append(errs, fmt.Errorf("re-open windows ACL target %s for rollback: %w", snapshot.Path, err))
			continue
		}
		// TWO DIFFERENT REFUSALS. A snapshot whose identity was never established
		// fails the match below exactly as a replaced target does, and both must
		// leave the object alone, but telling the operator it was replaced when it
		// was never identified sends them looking for a substitution that did not
		// happen. Reported by CodeRabbit.
		if !snapshot.Identity.valid {
			_ = windows.CloseHandle(handle)
			errs = append(errs, fmt.Errorf(
				"windows ACL target %s could not be identified when this setup changed it, "+
					"so rollback cannot prove it is the same object and leaves it with this run's grant",
				snapshot.Path))
			continue
		}
		if !snapshot.Identity.matches(windowsIdentityFromHandle(handle)) {
			_ = windows.CloseHandle(handle)
			errs = append(errs, fmt.Errorf(
				"windows ACL target %s is no longer the object this setup modified; "+
					"leaving the replacement untouched, and the original still carries this run's grant",
				snapshot.Path))
			continue
		}
		dacl, _, err := snapshot.Descriptor.DACL()
		if err != nil {
			_ = windows.CloseHandle(handle)
			errs = append(errs, fmt.Errorf("read rollback windows DACL for %s: %w", snapshot.Path, err))
			continue
		}
		if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
			errs = append(errs, fmt.Errorf("rollback windows ACL for %s: %w", snapshot.Path, err))
		}
		_ = windows.CloseHandle(handle)
	}
	return errors.Join(errs...)
}
