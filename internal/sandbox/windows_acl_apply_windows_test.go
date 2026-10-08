//go:build windows

package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// A workspace-owned target (no elevation needed: the current user holds WRITE_DAC
// on its own temp dir) must apply and roll back through the handle-based path.
// This exercises the whole open -> GetSecurityInfo -> SetSecurityInfo -> close
// sequence and the re-open rollback that replaced the pathname-based calls in
// #728, so a regression that reintroduced GetNamedSecurityInfo/SetNamedSecurityInfo
// (or broke the handle plumbing) would fail here.
func TestApplyWindowsACLPathGroupHandleBasedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	group := windowsACLPathGroup{
		Path: dir,
		Entries: []WindowsACLEntry{{
			Action:     WindowsACLAllowWrite,
			Path:       dir,
			Capability: "S-1-1-0", // Everyone: a well-known, StringToSid-parseable group SID
		}},
	}

	snapshot, applied, err := applyWindowsACLPathGroup(group)
	if err != nil {
		t.Fatalf("applyWindowsACLPathGroup: %v", err)
	}
	if !applied {
		t.Fatal("applied = false, want true for an existing directory target")
	}
	if snapshot.Path != dir || snapshot.Materialized {
		t.Fatalf("snapshot = %#v, want Path=%q Materialized=false", snapshot, dir)
	}
	if snapshot.Descriptor == nil {
		t.Fatal("snapshot has no captured descriptor to roll back to")
	}
	if err := rollbackWindowsACLSnapshots([]windowsACLSnapshot{snapshot}); err != nil {
		t.Fatalf("rollbackWindowsACLSnapshots: %v", err)
	}
}

// dirDeniesReadSID reports whether path's DACL has a DENY ACE for wantSID whose
// mask covers FILE_GENERIC_READ (DenyRead shape) without the full write-probe
// mask of experimental DenyWrite.
func dirDeniesReadSID(t *testing.T, path, wantSID string) bool {
	t.Helper()
	want, err := windows.StringToSid(wantSID)
	if err != nil {
		t.Fatalf("StringToSid %q: %v", wantSID, err)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo %s: %v", path, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("DACL %s: %v", path, err)
	}
	if dacl == nil {
		return false
	}
	_, readMask, err := windowsACLAccess(WindowsACLDenyRead)
	if err != nil {
		t.Fatalf("windowsACLAccess DenyRead: %v", err)
	}
	for index := uint16(0); index < dacl.AceCount; index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(index), &ace); err != nil {
			t.Fatalf("GetAce %d of %s: %v", index, path, err)
		}
		if ace.Header.AceType != windows.ACCESS_DENIED_ACE_TYPE && ace.Header.AceType != windowsAccessDeniedObjectAceType {
			continue
		}
		sid, ok := windowsAceSID(ace)
		if !ok || !sid.Equals(want) {
			continue
		}
		if ace.Mask&readMask == readMask && !windowsIsExperimentalWriteDenyMask(ace.Mask) {
			return true
		}
	}
	return false
}

// A materialized target that does not exist yet is created, ACL'd through the
// handle, and removed on rollback.
func TestApplyWindowsACLPathGroupMaterializes(t *testing.T) {
	target := filepath.Join(t.TempDir(), "created")
	group := windowsACLPathGroup{
		Path:        target,
		Materialize: true,
		Entries: []WindowsACLEntry{{
			Action:      WindowsACLDenyRead,
			Path:        target,
			Capability:  "S-1-1-0",
			Materialize: true,
		}},
	}

	snapshot, applied, err := applyWindowsACLPathGroup(group)
	if err != nil {
		t.Fatalf("applyWindowsACLPathGroup: %v", err)
	}
	if !applied || !snapshot.Materialized {
		t.Fatalf("applied=%v materialized=%v, want both true", applied, snapshot.Materialized)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("materialized target not created: %v", err)
	}
	if err := rollbackWindowsACLSnapshots([]windowsACLSnapshot{snapshot}); err != nil {
		t.Fatalf("rollbackWindowsACLSnapshots: %v", err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("materialized target still present after rollback: stat err = %v", err)
	}
}

// The core #728 guard: a target that resolves to a reparse point (symlink /
// junction) is refused rather than followed, so a swapped-in link during elevated
// setup cannot redirect the ACL change onto a system object.
func TestOpenWindowsACLTargetRejectsReparsePoint(t *testing.T) {
	realDir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	// Prefer a junction: unlike a symlink it needs no admin/Developer Mode, so
	// this guard actually runs in CI. A junction is a directory reparse point,
	// exactly the swap vector openWindowsACLTarget must refuse to follow. Fall
	// back to a symlink and skip only if neither reparse form can be created.
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, realDir).CombinedOutput(); err != nil {
		if serr := os.Symlink(realDir, link); serr != nil {
			t.Skipf("cannot create a reparse point (junction: %v %q; symlink: %v)", err, strings.TrimSpace(string(out)), serr)
		}
	}
	handle, _, err := openWindowsACLTarget(link)
	if err == nil {
		_ = windows.CloseHandle(handle)
		t.Fatal("openWindowsACLTarget followed a reparse point, want rejection")
	}
	if !strings.Contains(err.Error(), "reparse-point") {
		t.Fatalf("openWindowsACLTarget(symlink) err = %v, want a reparse-point rejection", err)
	}
}

// A missing target surfaces as os.ErrNotExist so the caller's materialize path
// still fires (a real open error, e.g. reparse rejection, must NOT look missing).
func TestOpenWindowsACLTargetMissingIsNotExist(t *testing.T) {
	_, _, err := openWindowsACLTarget(filepath.Join(t.TempDir(), "does-not-exist"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("openWindowsACLTarget(missing) err = %v, want os.ErrNotExist", err)
	}
}

// isDir is read from the same handle used for the ACL ops, not a separate Stat.
func TestOpenWindowsACLTargetReportsIsDir(t *testing.T) {
	dir := t.TempDir()
	handle, isDir, err := openWindowsACLTarget(dir)
	if err != nil {
		t.Fatalf("openWindowsACLTarget(dir): %v", err)
	}
	_ = windows.CloseHandle(handle)
	if !isDir {
		t.Fatal("isDir = false for a directory target, want true")
	}

	file := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	handle, isDir, err = openWindowsACLTarget(file)
	if err != nil {
		t.Fatalf("openWindowsACLTarget(file): %v", err)
	}
	_ = windows.CloseHandle(handle)
	if isDir {
		t.Fatal("isDir = true for a regular file, want false")
	}
}

// TestWindowsACLDenyWriteMigratesLegacySynchronizeMask regression tests that an
// existing legacy DenyWrite ACE containing SYNCHRONIZE (from older PR builds) is
// replaced in-place with the narrow mask that excludes SYNCHRONIZE, preserving
// co-resident DenyRead ACEs and operating idempotently.
func TestWindowsACLDenyWriteMigratesLegacySynchronizeMask(t *testing.T) {
	dir := t.TempDir()
	childDir := filepath.Join(dir, "sub")
	if err := os.Mkdir(childDir, 0o755); err != nil {
		t.Fatalf("mkdir childDir: %v", err)
	}
	childFile := filepath.Join(childDir, "child.txt")
	if err := os.WriteFile(childFile, []byte("data"), 0o644); err != nil {
		t.Fatalf("write childFile: %v", err)
	}

	caps, err := LoadOrCreateWindowsCapabilitySIDs(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateWindowsCapabilitySIDs: %v", err)
	}
	sidStr := caps.ReadOnly
	sid, err := windows.StringToSid(sidStr)
	if err != nil {
		t.Fatalf("StringToSid: %v", err)
	}

	// 1. Seed legacy DenyWrite ACE containing SYNCHRONIZE + co-resident DenyRead ACE.
	legacyWriteMask := (windows.FILE_GENERIC_WRITE | windows.DELETE | windowsFileDeleteChild | windows.WRITE_DAC | windows.WRITE_OWNER | windows.SYNCHRONIZE)
	_, readMask, err := windowsACLAccess(WindowsACLDenyRead)
	if err != nil {
		t.Fatalf("windowsACLAccess DenyRead: %v", err)
	}
	seedEntries := []windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: legacyWriteMask,
			AccessMode:        windows.DENY_ACCESS,
			Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		},
		{
			AccessPermissions: readMask,
			AccessMode:        windows.DENY_ACCESS,
			Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		},
	}
	handle, _, err := openWindowsACLTarget(dir)
	if err != nil {
		t.Fatalf("openWindowsACLTarget: %v", err)
	}
	seededDACL, err := windows.ACLFromEntries(seedEntries, nil)
	if err != nil {
		_ = windows.CloseHandle(handle)
		t.Fatalf("ACLFromEntries: %v", err)
	}
	if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, seededDACL, nil); err != nil {
		_ = windows.CloseHandle(handle)
		t.Fatalf("SetSecurityInfo: %v", err)
	}
	_ = windows.CloseHandle(handle)

	// 2. Apply WindowsACLPlan with new narrow DenyWrite action.
	plan := WindowsACLPlan{
		Entries: []WindowsACLEntry{{
			Action:     WindowsACLDenyWrite,
			Path:       dir,
			Capability: sidStr,
		}},
	}
	rollback, err := applyWindowsACLPlan(plan)
	if err != nil {
		t.Fatalf("applyWindowsACLPlan: %v", err)
	}
	t.Cleanup(func() { _ = rollback() })

	// 3. Verify effective DACL: SYNCHRONIZE must NOT be denied, write rights denied, DenyRead preserved.
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo: %v", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("DACL: %v", err)
	}

	var hasNarrowWriteDeny, hasSynchronizeDeny, hasReadDeny bool
	_, narrowWriteMask, err := windowsACLAccess(WindowsACLDenyWrite)
	if err != nil {
		t.Fatalf("windowsACLAccess: %v", err)
	}
	for i := uint16(0); i < dacl.AceCount; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
			t.Fatalf("GetAce: %v", err)
		}
		if ace.Header.AceType != windows.ACCESS_DENIED_ACE_TYPE && ace.Header.AceType != windowsAccessDeniedObjectAceType {
			continue
		}
		aceSID, ok := windowsAceSID(ace)
		if !ok || !aceSID.Equals(sid) {
			continue
		}
		if ace.Mask&windows.SYNCHRONIZE != 0 && windowsIsExperimentalWriteDenyMask(ace.Mask) {
			hasSynchronizeDeny = true
		}
		if ace.Mask&narrowWriteMask == narrowWriteMask {
			hasNarrowWriteDeny = true
		}
		if ace.Mask&readMask == readMask && !windowsIsExperimentalWriteDenyMask(ace.Mask) {
			hasReadDeny = true
		}
	}

	if hasSynchronizeDeny {
		t.Fatal("resulting DACL still denies SYNCHRONIZE for trustee; migration failed to narrow mask")
	}
	if !hasNarrowWriteDeny {
		t.Fatal("resulting DACL is missing narrow DenyWrite ACE")
	}
	if !hasReadDeny {
		t.Fatal("resulting DACL lost co-resident DenyRead ACE during migration")
	}

	// 4. Assert synchronous directory read works.
	if entries, err := os.ReadDir(dir); err != nil || len(entries) == 0 {
		t.Fatalf("os.ReadDir failed on migrated directory: entries=%v, err=%v", entries, err)
	}

	// 5. Assert second apply is idempotent.
	if _, err := applyWindowsACLPlan(plan); err != nil {
		t.Fatalf("second applyWindowsACLPlan failed: %v", err)
	}
	sd2, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo 2: %v", err)
	}
	dacl2, _, err := sd2.DACL()
	if err != nil {
		t.Fatalf("DACL 2: %v", err)
	}
	if dacl2.AceCount != dacl.AceCount {
		t.Fatalf("second apply changed ACE count: %d vs %d", dacl2.AceCount, dacl.AceCount)
	}
}

// TestWindowsACLDenyWriteMigratesLegacyNoInherit regression tests that when an
// existing directory carries a legacy inheritable deny-write ACE, migrating it
// with NoInherit: true removes any inherit-only ACEs and clears inheritance flags
// on the direct ACE so child files/directories do not inherit the deny.
func TestWindowsACLDenyWriteMigratesLegacyNoInherit(t *testing.T) {
	dir := t.TempDir()
	childDir := filepath.Join(dir, "sub")
	if err := os.Mkdir(childDir, 0o755); err != nil {
		t.Fatalf("mkdir childDir: %v", err)
	}
	childFile := filepath.Join(childDir, "child.txt")
	if err := os.WriteFile(childFile, []byte("data"), 0o644); err != nil {
		t.Fatalf("write childFile: %v", err)
	}

	caps, err := LoadOrCreateWindowsCapabilitySIDs(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateWindowsCapabilitySIDs: %v", err)
	}
	sidStr := caps.ReadOnly
	sid, err := windows.StringToSid(sidStr)
	if err != nil {
		t.Fatalf("StringToSid: %v", err)
	}

	// 1. Seed legacy inheritable DenyWrite ACEs: direct + inherit-only.
	legacyWriteMask := (windows.FILE_GENERIC_WRITE | windows.DELETE | windowsFileDeleteChild | windows.WRITE_DAC | windows.WRITE_OWNER | windows.SYNCHRONIZE)
	seedEntries := []windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: legacyWriteMask,
			AccessMode:        windows.DENY_ACCESS,
			Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		},
	}
	handle, _, err := openWindowsACLTarget(dir)
	if err != nil {
		t.Fatalf("openWindowsACLTarget: %v", err)
	}
	seededDACL, err := windows.ACLFromEntries(seedEntries, nil)
	if err != nil {
		_ = windows.CloseHandle(handle)
		t.Fatalf("ACLFromEntries: %v", err)
	}
	if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, seededDACL, nil); err != nil {
		_ = windows.CloseHandle(handle)
		t.Fatalf("SetSecurityInfo: %v", err)
	}
	_ = windows.CloseHandle(handle)

	// 2. Apply WindowsACLPlan with NoInherit: true.
	plan := WindowsACLPlan{
		Entries: []WindowsACLEntry{{
			Action:     WindowsACLDenyWrite,
			Path:       dir,
			Capability: sidStr,
			NoInherit:  true,
		}},
	}
	rollback, err := applyWindowsACLPlan(plan)
	if err != nil {
		t.Fatalf("applyWindowsACLPlan: %v", err)
	}
	t.Cleanup(func() { _ = rollback() })

	// 3. Verify effective DACL on dir: must have NO inheritance flags and no inherit-only ACE.
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo: %v", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("DACL: %v", err)
	}

	const inheritFlags = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE | windows.INHERIT_ONLY_ACE
	for i := uint16(0); i < dacl.AceCount; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
			t.Fatalf("GetAce: %v", err)
		}
		if ace.Header.AceType != windows.ACCESS_DENIED_ACE_TYPE && ace.Header.AceType != windowsAccessDeniedObjectAceType {
			continue
		}
		aceSID, ok := windowsAceSID(ace)
		if !ok || !aceSID.Equals(sid) {
			continue
		}
		if ace.Header.AceFlags&inheritFlags != 0 {
			t.Fatalf("migrated ACE for NoInherit entry retained inheritance flags: 0x%x", ace.Header.AceFlags)
		}
	}
}

// TestWindowsACLDenyWriteMigratesCombinedLegacyReadWriteDeny regression tests
// that migrating a pre-existing DACL with a single combined legacy read/write
// deny ACE preserves the DenyRead bits while narrowing only the write denial bits.
func TestWindowsACLDenyWriteMigratesCombinedLegacyReadWriteDeny(t *testing.T) {
	dir := t.TempDir()
	probeFile := filepath.Join(dir, "probe.txt")
	if err := os.WriteFile(probeFile, []byte("content"), 0o644); err != nil {
		t.Fatalf("write probeFile: %v", err)
	}

	caps, err := LoadOrCreateWindowsCapabilitySIDs(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateWindowsCapabilitySIDs: %v", err)
	}
	sidStr := caps.ReadOnly
	sid, err := windows.StringToSid(sidStr)
	if err != nil {
		t.Fatalf("StringToSid: %v", err)
	}

	_, readMask, err := windowsACLAccess(WindowsACLDenyRead)
	if err != nil {
		t.Fatalf("windowsACLAccess DenyRead: %v", err)
	}
	_, narrowWriteMask, err := windowsACLAccess(WindowsACLDenyWrite)
	if err != nil {
		t.Fatalf("windowsACLAccess DenyWrite: %v", err)
	}

	// 1. Seed a single combined legacy read/write deny ACE.
	legacyWriteMask := windows.FILE_GENERIC_WRITE | windows.DELETE | windowsFileDeleteChild | windows.WRITE_DAC | windows.WRITE_OWNER | windows.SYNCHRONIZE
	combinedMask := readMask | legacyWriteMask
	seedEntries := []windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: combinedMask,
			AccessMode:        windows.DENY_ACCESS,
			Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		},
	}
	handle, _, err := openWindowsACLTarget(dir)
	if err != nil {
		t.Fatalf("openWindowsACLTarget: %v", err)
	}
	seededDACL, err := windows.ACLFromEntries(seedEntries, nil)
	if err != nil {
		_ = windows.CloseHandle(handle)
		t.Fatalf("ACLFromEntries: %v", err)
	}
	if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, seededDACL, nil); err != nil {
		_ = windows.CloseHandle(handle)
		t.Fatalf("SetSecurityInfo: %v", err)
	}
	_ = windows.CloseHandle(handle)

	// 2. Apply WindowsACLPlan with WindowsACLDenyWrite.
	plan := WindowsACLPlan{
		Entries: []WindowsACLEntry{{
			Action:     WindowsACLDenyWrite,
			Path:       dir,
			Capability: sidStr,
		}},
	}
	rollback, err := applyWindowsACLPlan(plan)
	if err != nil {
		t.Fatalf("applyWindowsACLPlan: %v", err)
	}
	t.Cleanup(func() { _ = rollback() })

	// 3. Inspect DACL on dir.
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo: %v", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("DACL: %v", err)
	}

	foundCombinedMigrated := false
	for i := uint16(0); i < dacl.AceCount; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
			t.Fatalf("GetAce: %v", err)
		}
		if ace.Header.AceType != windows.ACCESS_DENIED_ACE_TYPE && ace.Header.AceType != windowsAccessDeniedObjectAceType {
			continue
		}
		aceSID, ok := windowsAceSID(ace)
		if !ok || !aceSID.Equals(sid) {
			continue
		}
		// The migrated ACE must retain read denial bits and have narrowed write denial bits.
		if ace.Mask&readMask == readMask && ace.Mask&narrowWriteMask == narrowWriteMask {
			foundCombinedMigrated = true
		}
	}

	if !foundCombinedMigrated {
		t.Fatal("resulting DACL lost DenyRead bits or failed to narrow DenyWrite in combined ACE")
	}

	// 4. Verify dirDeniesReadSID recognizes the DenyRead on the migrated combined ACE.
	if !dirDeniesReadSID(t, dir, sidStr) {
		t.Fatal("dirDeniesReadSID failed to recognize DenyRead on migrated combined ACE")
	}

	// 5. Verify idempotency on second apply.
	if _, err := applyWindowsACLPlan(plan); err != nil {
		t.Fatalf("second applyWindowsACLPlan failed: %v", err)
	}
	sd2, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo 2: %v", err)
	}
	dacl2, _, err := sd2.DACL()
	if err != nil {
		t.Fatalf("DACL 2: %v", err)
	}
	if dacl2.AceCount != dacl.AceCount {
		t.Fatalf("second apply changed ACE count: %d vs %d", dacl2.AceCount, dacl.AceCount)
	}
}
