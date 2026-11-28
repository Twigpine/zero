//go:build windows

package sandbox

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	windowsDisableMaxPrivilege = 0x01
	windowsLUAToken            = 0x04
	windowsWriteRestricted     = 0x08
	windowsGroupLogonID        = 0xC0000000
)

var procCreateRestrictedToken = windows.NewLazySystemDLL("advapi32.dll").NewProc("CreateRestrictedToken")

// procSetEntriesInAclW merges new ACEs into an existing ACL. Not wrapped by
// x/sys/windows (its internal setEntriesInAcl is unexported), so it is
// declared directly the same way procCreateRestrictedToken is.
var procSetEntriesInAclW = windows.NewLazySystemDLL("advapi32.dll").NewProc("SetEntriesInAclW")

type windowsLocalSID struct {
	sid *windows.SID
}

func newWindowsLocalSID(value string) (windowsLocalSID, error) {
	ptr, err := windows.UTF16PtrFromString(value)
	if err != nil {
		return windowsLocalSID{}, err
	}
	var sid *windows.SID
	if err := windows.ConvertStringSidToSid(ptr, &sid); err != nil {
		return windowsLocalSID{}, err
	}
	return windowsLocalSID{sid: sid}, nil
}

func (sid windowsLocalSID) close() {
	if sid.sid != nil {
		_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(sid.sid)))
	}
}

// restrictWindowsTokenForCapabilitySIDs applies the same write jail to an
// arbitrary base token that createWindowsRestrictedTokenForCapabilitySIDs
// applies to the calling process's own.
//
// The sandbox principal path needs this. A LogonUser token is a full token for
// that account: the ACL plan can deny it at named paths, but it cannot revoke
// what the account's ambient memberships already grant, so an opted-in command
// could still write any path whose DACL admits BUILTIN\Users, Authenticated
// Users, or NT AUTHORITY\BATCH - C:\Users\Public\Documents being the obvious
// one - regardless of the profile's write roots.
//
// The caller must include the principal's OWN SID among the capability SIDs.
// The plan grants the workspace to that SID rather than to a capability SID, so
// without it the restricted-SID check has nothing to match and the principal
// loses its own workspace: a jail that locks out the inmate and no one else.
func restrictWindowsTokenForCapabilitySIDs(base windows.Token, capabilitySIDStrings []string, writeRestricted bool) (windows.Token, error) {
	capabilitySIDs, err := parseWindowsCapabilitySIDs(capabilitySIDStrings)
	if err != nil {
		return 0, err
	}
	defer func() {
		for _, sid := range capabilitySIDs {
			sid.close()
		}
	}()
	return createWindowsRestrictedTokenFromBase(base, capabilitySIDs, writeRestricted)
}

// parseWindowsCapabilitySIDs converts SID strings, closing what it already
// allocated if one fails to parse.
func parseWindowsCapabilitySIDs(values []string) ([]windowsLocalSID, error) {
	if len(values) == 0 {
		return nil, errors.New("windows restricted token requires at least one capability SID")
	}
	parsed := make([]windowsLocalSID, 0, len(values))
	for _, value := range values {
		sid, err := newWindowsLocalSID(value)
		if err != nil {
			for _, existing := range parsed {
				existing.close()
			}
			return nil, fmt.Errorf("parse windows capability SID %q: %w", value, err)
		}
		parsed = append(parsed, sid)
	}
	return parsed, nil
}

func createWindowsRestrictedTokenForCapabilitySIDs(capabilitySIDStrings []string, writeRestricted bool) (windows.Token, error) {
	if len(capabilitySIDStrings) == 0 {
		return 0, errors.New("windows restricted token requires at least one capability SID")
	}
	capabilitySIDs := make([]windowsLocalSID, 0, len(capabilitySIDStrings))
	for _, value := range capabilitySIDStrings {
		sid, err := newWindowsLocalSID(value)
		if err != nil {
			for _, existing := range capabilitySIDs {
				existing.close()
			}
			return 0, fmt.Errorf("parse windows capability SID %q: %w", value, err)
		}
		capabilitySIDs = append(capabilitySIDs, sid)
	}
	defer func() {
		for _, sid := range capabilitySIDs {
			sid.close()
		}
	}()

	var base windows.Token
	desired := uint32(windows.TOKEN_DUPLICATE |
		windows.TOKEN_QUERY |
		windows.TOKEN_ASSIGN_PRIMARY |
		windows.TOKEN_ADJUST_DEFAULT |
		windows.TOKEN_ADJUST_SESSIONID |
		windows.TOKEN_ADJUST_PRIVILEGES)
	if err := windows.OpenProcessToken(windows.CurrentProcess(), desired, &base); err != nil {
		return 0, fmt.Errorf("open process token: %w", err)
	}
	defer base.Close()
	return createWindowsRestrictedTokenFromBase(base, capabilitySIDs, writeRestricted)
}

func createWindowsRestrictedTokenFromBase(base windows.Token, capabilitySIDs []windowsLocalSID, writeRestricted bool) (windows.Token, error) {
	logonSID, err := copyWindowsLogonSID(base)
	if err != nil {
		return 0, err
	}
	entries := make([]windows.SIDAndAttributes, 0, len(capabilitySIDs)+2)
	for _, sid := range capabilitySIDs {
		entries = append(entries, windows.SIDAndAttributes{Sid: sid.sid})
	}
	entries = append(entries, windows.SIDAndAttributes{Sid: sidFromBytes(logonSID)})

	// The World SID (S-1-1-0, Everyone) is added ONLY to the token that does not
	// carry WRITE_RESTRICTED, and putting it back unconditionally would reopen a
	// write-jail bypass.
	//
	// A restricted SID is a key to every object whose DACL names it. That is why
	// the runner refuses to add the user SID, Administrators or SYSTEM — "each
	// has write access nearly everywhere". Everyone is the broadest of the lot:
	// every principal carries it, so under WRITE_RESTRICTED the second
	// (restricted-SID) check passes for free on any path whose DACL grants
	// Everyone write, and confinement falls back to the ordinary user's own
	// permissions — the exact boundary this token exists to be stricter than. It
	// needs no privilege, no symlink and no race: an Everyone-writable directory
	// is enough, and share roots opened with Everyone:F and loose installer ACLs
	// supply them. It was present from the original sandbox baseline,
	// uncommented, and under WRITE_RESTRICTED nothing depends on it, because that
	// flag already exempts reads from the restricted-SID check.
	//
	// Without the flag it IS load-bearing and cannot simply be dropped. The
	// restricted-SID check then applies to READS too, and default Windows DACLs
	// grant BUILTIN\Users rather than anything in this list, so a token without
	// Everyone cannot open cmd.exe — the process dies at launch with
	// STATUS_ACCESS_DENIED (0xC0000022) before it runs anything. That path is
	// only taken when the profile configures DenyRead, which is already the
	// posture that trades capability for read-deny enforcement (#612).
	//
	// So the bypass survives for DenyRead profiles, deliberately and narrowly,
	// rather than being traded for a sandbox that cannot start a command. Closing
	// it there needs a different mechanism (a read-side grant that is not a
	// universal group), tracked separately.
	//
	// The logon SID above stays in both modes: it is this token's own rather than
	// a broad group, and broadenWindowsRestrictedTokenDefaultDacl depends on it so
	// the process can use pipes and events it creates for itself.
	//
	// Anything added here needs the same scrutiny — Authenticated Users, Users,
	// INTERACTIVE and BATCH would each produce this bypass on a DACL naming them.
	if !writeRestricted {
		worldSID, err := windows.CreateWellKnownSid(windows.WinWorldSid)
		if err != nil {
			return 0, fmt.Errorf("create world SID: %w", err)
		}
		entries = append(entries, windows.SIDAndAttributes{Sid: worldSID})
	}

	// WRITE_RESTRICTED scopes the restricted-SID check to write-type accesses:
	// reads use only the normal token identity, so the sandboxed process can
	// open executables, DLLs, and per-user config the user can read, while
	// writes still require a restricted-SID match (the workspace write-jail).
	// Without it the restricted-SID check applies to READS too, and because
	// default Windows DACLs grant BUILTIN\Users rather than any SID in the
	// restricted list, the token cannot open ANY executable outside the
	// ACL-granted write roots — every spawned command dies silently.
	//
	// The flag also makes the kernel skip restricted-SID deny ACEs for reads,
	// which is the DenyRead bypass fixed in #612 — so it is only safe when the
	// profile has no DenyRead paths. The caller passes writeRestricted=false
	// whenever DenyRead is configured, keeping #612's enforcement for exactly
	// the profiles that need it.
	flags := uint32(windowsDisableMaxPrivilege | windowsLUAToken)
	if writeRestricted {
		flags |= windowsWriteRestricted
	}
	var restricted windows.Token
	result, _, callErr := procCreateRestrictedToken.Call(
		uintptr(base),
		uintptr(flags),
		0,
		0,
		0,
		0,
		uintptr(len(entries)),
		uintptr(unsafe.Pointer(&entries[0])),
		uintptr(unsafe.Pointer(&restricted)),
	)
	runtime.KeepAlive(logonSID)
	runtime.KeepAlive(entries)
	runtime.KeepAlive(capabilitySIDs)
	if result == 0 {
		if callErr != syscall.Errno(0) {
			return 0, fmt.Errorf("CreateRestrictedToken: %w", callErr)
		}
		return 0, errors.New("CreateRestrictedToken failed")
	}
	if err := enableWindowsTokenPrivilege(restricted, "SeChangeNotifyPrivilege"); err != nil {
		_ = restricted.Close()
		return 0, err
	}
	if err := broadenWindowsRestrictedTokenDefaultDacl(restricted, sidFromBytes(logonSID)); err != nil {
		_ = restricted.Close()
		return 0, err
	}
	runtime.KeepAlive(logonSID)
	return restricted, nil
}

// broadenWindowsRestrictedTokenDefaultDacl adds the token's own logon SID to
// its default DACL (the DACL applied to any kernel object the token's process
// creates WITHOUT an explicit security descriptor, e.g. anonymous pipes,
// events, mutexes).
//
// A WRITE_RESTRICTED token requires a WRITE-type access check to match BOTH
// the normal enabled-SID grant AND a SEPARATE grant to one of the token's
// restricted SIDs. CreateRestrictedToken does not touch the default DACL it
// inherits from the base token, so newly created objects only carry ACEs for
// the base token's normal identity (owner, SYSTEM, Administrators) — none of
// which are in the restricted-SID list — so the second check fails. This
// breaks anything that creates a pipe/handle for itself with default
// security, which is exactly what any tool using a language runtime's
// "spawn a subprocess and capture its output" primitive does (Go's os/exec,
// and by extension `gh`, and many other CLI tools that shell out
// internally): CreatePipe() succeeds, but the creating process's own token
// then fails to actually read/write the handle it just made.
//
// The logon SID is already unconditionally present in this token's
// restricted-SID list (see createWindowsRestrictedTokenFromBase), so adding
// it here — and ONLY it — to the default DACL is the minimal change that
// satisfies the WRITE_RESTRICTED extra check for self-created objects. It
// does not touch file-system access: NTFS write-jailing is enforced by the
// explicit ACL grants applied to workspace paths (windows_acl.go), which are
// created WITH an explicit security descriptor and never consult the token's
// default DACL. The exposure is bounded to the same logon session (other
// processes running as the same signed-in user could, in principle, open a
// NAMED kernel object the sandboxed process creates with default security;
// anonymous pipes — the actual object type this fixes — have no name and are
// only reachable via an inherited handle, so this is a no-op for them).
func broadenWindowsRestrictedTokenDefaultDacl(token windows.Token, logonSID *windows.SID) error {
	oldDacl, oldDaclBuf, err := windowsTokenDefaultDacl(token)
	if err != nil {
		return fmt.Errorf("read token default DACL: %w", err)
	}
	// The second (WRITE-type) access check only needs a read/write match; a
	// full GENERIC_ALL grant (which also implies DELETE, WRITE_DAC, and
	// WRITE_OWNER) is broader than the pipe/event/mutex use case this exists
	// for actually requires.
	access := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_READ | windows.GENERIC_WRITE,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			MultipleTrusteeOperation: windows.NO_MULTIPLE_TRUSTEE,
			TrusteeForm:              windows.TRUSTEE_IS_SID,
			TrusteeType:              windows.TRUSTEE_IS_USER,
			TrusteeValue:             windows.TrusteeValueFromSID(logonSID),
		},
	}}
	newDacl, err := setEntriesInACL(access, oldDacl)
	// oldDacl points INTO oldDaclBuf (see windowsTokenDefaultDacl) and is
	// dereferenced by native code inside setEntriesInACL, above. Nothing in Go
	// references oldDaclBuf once windowsTokenDefaultDacl returned, so without
	// this KeepAlive it is eligible for GC the moment an allocation in this
	// function (the EXPLICIT_ACCESS literal, TrusteeValueFromSID) hits a
	// safepoint, before the native call ever reads it - a real, if narrow,
	// use-after-free window in security-boundary code.
	runtime.KeepAlive(oldDaclBuf)
	if err != nil {
		return fmt.Errorf("merge token default DACL: %w", err)
	}
	defer func() { _, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(newDacl))) }()
	if err := windowsSetTokenDefaultDacl(token, newDacl); err != nil {
		return fmt.Errorf("set token default DACL: %w", err)
	}
	return nil
}

type windowsTokenDefaultDaclInfo struct {
	DefaultDacl *windows.ACL
}

// windowsTokenDefaultDacl returns the token's current default DACL along with
// the buffer backing it. TOKEN_DEFAULT_DACL embeds the ACL data inside the
// same allocation GetTokenInformation fills, so the returned *windows.ACL is
// only valid for as long as the returned buffer stays reachable: the caller
// must runtime.KeepAlive(buf) until every native call that dereferences the
// ACL has completed. A KeepAlive inside this function, ending at its own
// return, would not protect any use by the caller after that.
func windowsTokenDefaultDacl(token windows.Token) (*windows.ACL, []byte, error) {
	var size uint32
	err := windows.GetTokenInformation(token, windows.TokenDefaultDacl, nil, 0, &size)
	if err == nil || err != windows.ERROR_INSUFFICIENT_BUFFER {
		return nil, nil, fmt.Errorf("size TokenDefaultDacl: %w", err)
	}
	buf := make([]byte, size)
	if err := windows.GetTokenInformation(token, windows.TokenDefaultDacl, &buf[0], size, &size); err != nil {
		return nil, nil, fmt.Errorf("get TokenDefaultDacl: %w", err)
	}
	info := (*windowsTokenDefaultDaclInfo)(unsafe.Pointer(&buf[0]))
	return info.DefaultDacl, buf, nil
}

func windowsSetTokenDefaultDacl(token windows.Token, dacl *windows.ACL) error {
	info := windowsTokenDefaultDaclInfo{DefaultDacl: dacl}
	err := windows.SetTokenInformation(token, windows.TokenDefaultDacl, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	runtime.KeepAlive(info)
	return err
}

// setEntriesInACL merges entries into oldACL via Win32 SetEntriesInAclW,
// returning a newly allocated ACL the caller must free with windows.LocalFree.
func setEntriesInACL(entries []windows.EXPLICIT_ACCESS, oldACL *windows.ACL) (*windows.ACL, error) {
	if len(entries) == 0 {
		return nil, errors.New("setEntriesInACL requires at least one entry")
	}
	var newACL *windows.ACL
	// SetEntriesInAclW reports failure directly in its return value (ERROR_SUCCESS
	// on success, a Win32 error code otherwise); unlike many Win32 APIs it does not
	// set the thread's last-error, so the syscall trio's second/third return
	// values (which surface GetLastError) are not meaningful here.
	ret, _, _ := procSetEntriesInAclW.Call(
		uintptr(len(entries)),
		uintptr(unsafe.Pointer(&entries[0])),
		uintptr(unsafe.Pointer(oldACL)),
		uintptr(unsafe.Pointer(&newACL)),
	)
	runtime.KeepAlive(entries)
	if ret != 0 {
		return nil, fmt.Errorf("SetEntriesInAclW: %w", syscall.Errno(ret))
	}
	return newACL, nil
}

func copyWindowsLogonSID(token windows.Token) ([]byte, error) {
	groups, err := token.GetTokenGroups()
	if err == nil {
		if sid := logonSIDFromGroups(groups); sid != nil {
			copied, copyErr := copyWindowsSID(sid)
			runtime.KeepAlive(groups)
			return copied, copyErr
		}
	}
	linked, linkedErr := token.GetLinkedToken()
	if linkedErr == nil {
		defer linked.Close()
		groups, err = linked.GetTokenGroups()
		if err == nil {
			if sid := logonSIDFromGroups(groups); sid != nil {
				copied, copyErr := copyWindowsSID(sid)
				runtime.KeepAlive(groups)
				return copied, copyErr
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("read token groups: %w", err)
	}
	return nil, errors.New("logon SID not present on token")
}

func logonSIDFromGroups(groups *windows.Tokengroups) *windows.SID {
	for _, group := range groups.AllGroups() {
		if group.Attributes&windowsGroupLogonID == windowsGroupLogonID {
			return group.Sid
		}
	}
	return nil
}

func copyWindowsSID(sid *windows.SID) ([]byte, error) {
	length := windows.GetLengthSid(sid)
	if length == 0 {
		return nil, errors.New("invalid SID length")
	}
	out := make([]byte, length)
	if err := windows.CopySid(length, sidFromBytes(out), sid); err != nil {
		return nil, err
	}
	return out, nil
}

func sidFromBytes(value []byte) *windows.SID {
	return (*windows.SID)(unsafe.Pointer(&value[0]))
}

func enableWindowsTokenPrivilege(token windows.Token, name string) error {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	var luid windows.LUID
	if err := windows.LookupPrivilegeValue(nil, namePtr, &luid); err != nil {
		return fmt.Errorf("lookup token privilege %s: %w", name, err)
	}
	privileges := windows.Tokenprivileges{PrivilegeCount: 1}
	privileges.Privileges[0] = windows.LUIDAndAttributes{
		Luid:       luid,
		Attributes: windows.SE_PRIVILEGE_ENABLED,
	}
	if err := windows.AdjustTokenPrivileges(token, false, &privileges, 0, nil, nil); err != nil {
		return fmt.Errorf("enable token privilege %s: %w", name, err)
	}
	return nil
}
