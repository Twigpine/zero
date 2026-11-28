//go:build windows

package sandbox

import (
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Privileges CreateProcessAsUser requires when the token belongs to a DIFFERENT
// account than the caller.
//
// The documented exemption from SE_ASSIGNPRIMARYTOKEN_NAME is for a restricted
// version of the caller's OWN primary token, which is exactly what the ordinary
// restricted-token path passes and exactly why that path works without holding
// anything special. A principal token comes from LogonUser against a separate
// local account, so the exemption does not apply and both privileges are back in
// play.
const (
	seAssignPrimaryTokenPrivilege = "SeAssignPrimaryTokenPrivilege"
	seIncreaseQuotaPrivilege      = "SeIncreaseQuotaPrivilege"
)

// enableWindowsPrincipalLaunchPrivileges enables what launching another account
// needs, and reports precisely what is missing when it cannot.
//
// Enabling matters on its own: a privilege present in a token but DISABLED still
// fails the access check, and an elevated administrator typically holds
// SeIncreaseQuotaPrivilege in exactly that state. So this is not only a
// diagnostic, it is the step that makes the call work wherever the rights are
// actually held.
//
// Where they are not held, which is the ordinary unelevated case, this turns a
// bare "Access is denied" from deep inside process creation into a statement of
// what the sandbox cannot do and why. That failure is otherwise indistinguishable
// from the command itself being rejected, and it happens before the command's
// executable is ever opened.
func enableWindowsPrincipalLaunchPrivileges() error {
	var token windows.Token
	if err := windows.OpenProcessToken(
		windows.CurrentProcess(),
		windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY,
		&token,
	); err != nil {
		return fmt.Errorf("open process token for principal launch: %w", err)
	}
	defer token.Close()

	// ENUMERATE the token, do not infer from AdjustTokenPrivileges.
	//
	// AdjustTokenPrivileges reports a privilege the token does not hold by
	// returning success and setting ERROR_NOT_ALL_ASSIGNED, and this binding does
	// not surface that: it returns nil for SeTcbPrivilege on an ordinary user
	// process, which holds nothing of the sort. A check built on its error would
	// pass on every machine, which is worse than no check, because it would report
	// the sandbox ready in exactly the case it cannot run.
	held, err := windowsTokenPrivilegeLUIDs(token)
	if err != nil {
		return err
	}
	var missing []string
	for _, name := range []string{seAssignPrimaryTokenPrivilege, seIncreaseQuotaPrivilege} {
		luid, err := windowsPrivilegeLUID(name)
		if err != nil {
			return err
		}
		if _, ok := held[luid]; !ok {
			missing = append(missing, name)
			continue
		}
		// Held but possibly disabled, which still fails the access check. This is
		// the case an elevated administrator lands in.
		if err := enableWindowsTokenPrivilege(token, name); err != nil {
			return fmt.Errorf("enable %s: %w", name, err)
		}
	}
	return principalLaunchPrivilegeError(missing)
}

// principalLaunchPrivilegeError renders the refusal for a set of missing
// privileges, and is separate from the token work so it can be driven for every
// combination.
//
// It has to be, because WHICH privilege is missing depends on the account
// running the test. An ordinary unelevated user typically holds neither, but not
// always: this developer box holds SeAssignPrimaryTokenPrivilege and not
// SeIncreaseQuotaPrivilege, so a test that named one of them and ran the real
// preflight asserted a machine's configuration rather than the code. Splitting
// the rendering out makes each case deterministic and leaves the token path with
// only the question that genuinely varies.
func principalLaunchPrivilegeError(missing []string) error {
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf(
		"launching a command as the sandbox principal needs %s, which this process does not hold. "+
			"CreateProcessAsUser exempts only a restricted version of the caller's own token, and a principal "+
			"is a separate account, so an ordinary unelevated process cannot start one. "+
			"Unset %s to use the restricted-token sandbox, which needs no such privilege",
		strings.Join(missing, " and "), windowsSandboxIdentityEnv)
}

// windowsPrivilegeLUID resolves a privilege name to its locally unique id, which
// is how a token actually names the privileges it holds.
func windowsPrivilegeLUID(name string) (windows.LUID, error) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return windows.LUID{}, fmt.Errorf("encode privilege name %s: %w", name, err)
	}
	var luid windows.LUID
	if err := windows.LookupPrivilegeValue(nil, namePtr, &luid); err != nil {
		return windows.LUID{}, fmt.Errorf("look up privilege %s: %w", name, err)
	}
	return luid, nil
}

// windowsTokenPrivilegeLUIDs returns every privilege the token holds, enabled or
// not. Presence and enabled-ness are separate questions: a privilege absent from
// this set can never be enabled, while one present but disabled only needs
// AdjustTokenPrivileges.
func windowsTokenPrivilegeLUIDs(token windows.Token) (map[windows.LUID]struct{}, error) {
	var size uint32
	// First call sizes the buffer and is expected to fail with
	// ERROR_INSUFFICIENT_BUFFER, so its error is deliberately not checked.
	_ = windows.GetTokenInformation(token, windows.TokenPrivileges, nil, 0, &size)
	if size == 0 {
		return nil, errors.New("query token privileges: zero-length result")
	}
	buffer := make([]byte, size)
	if err := windows.GetTokenInformation(token, windows.TokenPrivileges, &buffer[0], size, &size); err != nil {
		return nil, fmt.Errorf("query token privileges: %w", err)
	}
	privileges := (*windows.Tokenprivileges)(unsafe.Pointer(&buffer[0]))
	if privileges.PrivilegeCount == 0 {
		return map[windows.LUID]struct{}{}, nil
	}
	entries := unsafe.Slice(&privileges.Privileges[0], privileges.PrivilegeCount)
	held := make(map[windows.LUID]struct{}, len(entries))
	for _, entry := range entries {
		held[entry.Luid] = struct{}{}
	}
	return held, nil
}
