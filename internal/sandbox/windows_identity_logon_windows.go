//go:build windows

package sandbox

// Minting a token for a sandbox principal.
//
// A provisioned account is inert until something can log on as it. Windows
// gates that behind account rights held in the local security policy, so setup
// grants the principal exactly one: the right to be logged on as a batch job,
// which is what a non-interactive service-style logon needs. It is deliberately
// NOT granted interactive, network or remote-interactive logon, and those three
// are explicitly DENIED, so the account cannot be used to sign in at the
// console, over SMB, or through RDP even if its password leaked. The password
// exists only so LogonUser can mint a token; nobody is meant to type it.
//
// Rights are granted at setup (elevated) because LsaAddAccountRights requires
// administrator privileges. The per-command path only calls LogonUser, which
// needs no special privilege once the batch right is in place.

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	// Logon type/provider for a non-interactive token. Batch is the closest
	// match for "run this command as a service-like principal": it produces a
	// full token without a desktop or network-credential footprint.
	logon32LogonBatch      = 4
	logon32ProviderDefault = 0

	// LSA policy access rights needed to add account rights.
	policyCreateAccount = 0x00000010
	policyLookupNames   = 0x00000800

	// Account rights. The sandbox principal gets the batch right and is denied
	// every interactive path.
	seBatchLogonRight              = "SeBatchLogonRight"
	seDenyInteractiveLogonRight    = "SeDenyInteractiveLogonRight"
	seDenyNetworkLogonRight        = "SeDenyNetworkLogonRight"
	seDenyRemoteInteractiveRight   = "SeDenyRemoteInteractiveLogonRight"
	seDenyServiceLogonRightName    = "SeDenyServiceLogonRight"
	windowsIdentityLogonRightsNote = "granted by `zero sandbox setup`"
)

var (
	procLogonUserW          = windows.NewLazySystemDLL("advapi32.dll").NewProc("LogonUserW")
	procLsaOpenPolicy       = windows.NewLazySystemDLL("advapi32.dll").NewProc("LsaOpenPolicy")
	procLsaClose            = windows.NewLazySystemDLL("advapi32.dll").NewProc("LsaClose")
	procLsaAddAccountRights = windows.NewLazySystemDLL("advapi32.dll").NewProc("LsaAddAccountRights")
	// Retiring a principal has to drop its rights as well as its account, or the
	// LSA policy database keeps an entry keyed to a SID that no longer resolves.
	procLsaRemoveAccountRights = windows.NewLazySystemDLL("advapi32.dll").NewProc("LsaRemoveAccountRights")
	procLsaNtStatusToWinErr    = windows.NewLazySystemDLL("advapi32.dll").NewProc("LsaNtStatusToWinError")
)

// lsaUnicodeString mirrors LSA_UNICODE_STRING. Length and MaximumLength are
// BYTE counts, not rune counts, which is the usual source of bugs here.
type lsaUnicodeString struct {
	Length        uint16
	MaximumLength uint16
	Buffer        *uint16
}

// lsaObjectAttributes mirrors LSA_OBJECT_ATTRIBUTES. Every field except Length
// is unused for LsaOpenPolicy, but the struct must still be the right size.
type lsaObjectAttributes struct {
	Length                   uint32
	RootDirectory            windows.Handle
	ObjectName               *lsaUnicodeString
	Attributes               uint32
	SecurityDescriptor       unsafe.Pointer
	SecurityQualityOfService unsafe.Pointer
}

// newLSAString builds an LSA_UNICODE_STRING over a UTF-16 buffer the caller
// keeps alive. The returned value borrows that buffer, so the buffer must
// outlive every use of the string.
func newLSAString(buffer []uint16) lsaUnicodeString {
	if len(buffer) == 0 {
		return lsaUnicodeString{}
	}
	// The buffer is NUL-terminated; the LSA length counts bytes WITHOUT the
	// terminator, while MaximumLength counts bytes WITH it.
	runes := len(buffer) - 1
	return lsaUnicodeString{
		Length:        uint16(runes * 2),
		MaximumLength: uint16(len(buffer) * 2),
		Buffer:        &buffer[0],
	}
}

// lsaStatusError converts an NTSTATUS from an Lsa* call into a Go error, going
// through LsaNtStatusToWinError so the message is the familiar Win32 one rather
// than a raw NTSTATUS.
func lsaStatusError(call string, status uintptr) error {
	if status == 0 {
		return nil
	}
	winErr, _, _ := procLsaNtStatusToWinErr.Call(status)
	return fmt.Errorf("%s: %w", call, windows.Errno(winErr))
}

// grantWindowsSandboxLogonRights gives the principal the batch logon right and
// denies every interactive logon path. Idempotent: LsaAddAccountRights silently
// succeeds when the account already holds a right, so setup can re-run.
//
// Requires an elevated caller.
func grantWindowsSandboxLogonRights(sid *windows.SID) error {
	if sid == nil {
		return fmt.Errorf("grant sandbox logon rights: nil SID")
	}
	var attributes lsaObjectAttributes
	attributes.Length = uint32(unsafe.Sizeof(attributes))
	var policy windows.Handle
	status, _, _ := procLsaOpenPolicy.Call(
		0, // local system
		uintptr(unsafe.Pointer(&attributes)),
		uintptr(policyCreateAccount|policyLookupNames),
		uintptr(unsafe.Pointer(&policy)),
	)
	// LsaOpenPolicy borrows the attributes struct by address, so it has to stay
	// reachable until the call has returned.
	runtime.KeepAlive(attributes)
	if err := lsaStatusError("LsaOpenPolicy", status); err != nil {
		return err
	}
	defer procLsaClose.Call(uintptr(policy))

	rights := []string{
		seBatchLogonRight,
		seDenyInteractiveLogonRight,
		seDenyNetworkLogonRight,
		seDenyRemoteInteractiveRight,
		seDenyServiceLogonRightName,
	}
	// Each right is added on its own call so one unsupported name on an odd SKU
	// cannot silently drop the others.
	for _, right := range rights {
		buffer, err := windows.UTF16FromString(right)
		if err != nil {
			return err
		}
		entry := newLSAString(buffer)
		status, _, _ := procLsaAddAccountRights.Call(
			uintptr(policy),
			uintptr(unsafe.Pointer(sid)),
			uintptr(unsafe.Pointer(&entry)),
			1,
		)
		// Both the descriptor and the buffer it points at are borrowed by the
		// call. Kept alive before the error check, not after, so the failure path
		// does not return with them already collectable.
		runtime.KeepAlive(entry)
		runtimeKeepAliveUint16(buffer)
		if err := lsaStatusError("LsaAddAccountRights("+right+")", status); err != nil {
			return err
		}
	}
	return nil
}

// revokeWindowsSandboxLogonRights drops every account right held by a principal
// and removes its entry from the LSA policy database.
//
// This is the logon-rights counterpart to revoking ACEs by trustee, and it has
// the same reason to exist: deleting the account on its own leaves the rights
// behind, keyed to a SID that no longer resolves, which is the orphaned residue
// this model is supposed to avoid. It must therefore run BEFORE the account is
// deleted, while the SID is still resolvable.
//
// Removing all rights rather than naming them is deliberate. The principal is
// being retired, so anything keyed to it should go, including rights a previous
// version of setup granted and this one no longer knows about.
//
// Requires an elevated caller. A principal that holds no rights is not an error:
// LsaRemoveAccountRights reports ERROR_FILE_NOT_FOUND for an account with no LSA
// entry, which is the state teardown is trying to reach anyway.
func revokeWindowsSandboxLogonRights(sid *windows.SID) error {
	if sid == nil {
		return fmt.Errorf("revoke sandbox logon rights: nil SID")
	}
	var attributes lsaObjectAttributes
	attributes.Length = uint32(unsafe.Sizeof(attributes))
	var policy windows.Handle
	status, _, _ := procLsaOpenPolicy.Call(
		0, // local system
		uintptr(unsafe.Pointer(&attributes)),
		uintptr(policyCreateAccount|policyLookupNames),
		uintptr(unsafe.Pointer(&policy)),
	)
	runtime.KeepAlive(attributes)
	if err := lsaStatusError("LsaOpenPolicy", status); err != nil {
		return err
	}
	defer procLsaClose.Call(uintptr(policy))

	status, _, _ = procLsaRemoveAccountRights.Call(
		uintptr(policy),
		uintptr(unsafe.Pointer(sid)),
		1, // AllRights: drop everything and delete the LSA account object
		0, // UserRights ignored when AllRights is set
		0, // CountOfRights likewise
	)
	runtime.KeepAlive(sid)
	if err := lsaStatusError("LsaRemoveAccountRights", status); err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return nil
		}
		return err
	}
	return nil
}

// logonWindowsSandboxPrincipal mints a primary token for the sandbox account.
// The caller owns the returned token and must Close it.
//
// This needs no elevation: the batch logon right granted at setup is what makes
// it work, which is why the per-command path can run unelevated once setup has
// been done once.
func logonWindowsSandboxPrincipal(username string, password string) (windows.Token, error) {
	user, err := windows.UTF16PtrFromString(username)
	if err != nil {
		return 0, err
	}
	// "." is the local machine, so the lookup never leaves this host even if the
	// machine is domain-joined and a same-named domain account exists.
	domain, err := windows.UTF16PtrFromString(".")
	if err != nil {
		return 0, err
	}
	secret, err := windows.UTF16PtrFromString(password)
	if err != nil {
		return 0, err
	}
	var token windows.Token
	result, _, callErr := procLogonUserW.Call(
		uintptr(unsafe.Pointer(user)),
		uintptr(unsafe.Pointer(domain)),
		uintptr(unsafe.Pointer(secret)),
		logon32LogonBatch,
		logon32ProviderDefault,
		uintptr(unsafe.Pointer(&token)),
	)
	// The three strings are borrowed for the duration of the call.
	runtime.KeepAlive(user)
	runtime.KeepAlive(domain)
	runtime.KeepAlive(secret)
	if result == 0 {
		if callErr != nil && callErr != windows.ERROR_SUCCESS {
			return 0, fmt.Errorf("LogonUser(%s): %w", username, callErr)
		}
		return 0, fmt.Errorf("LogonUser(%s) failed", username)
	}
	return token, nil
}

// runtimeKeepAliveUint16 keeps a UTF-16 buffer reachable across a syscall that
// borrows it. Declared rather than inlined so the intent is explicit at each
// call site; the compiler must not free the slice while LSA holds the pointer.
func runtimeKeepAliveUint16(buffer []uint16) {
	if len(buffer) == 0 {
		return
	}
	_ = buffer[0]
}
