//go:build windows

package sandbox

// Storing a sandbox principal's password.
//
// The elevated setup path provisions the account, but the per-command path runs
// UNELEVATED and needs the password to call LogonUser. So the secret has to
// cross that boundary on disk, and the only thing standing between it and the
// sandboxed child is the file's ACL.
//
// The file is locked to the invoking user: an explicit, INHERITANCE-PROTECTED
// DACL granting that user and SYSTEM, and nobody else. The sandbox principal is
// deliberately absent from it, which is the property that matters, because a
// principal that could read this file could mint its own token and the whole
// identity boundary would be decorative. Administrators are not added either;
// an admin can already take ownership, so naming them buys nothing and widens
// the visible grant.
//
// Ordering is load-bearing: the ACL is applied to an EMPTY file before the
// password is written, so the bytes never exist under the directory's inherited
// permissions even briefly.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// windowsSandboxSecretDirName holds per-principal secrets under the Zero config
// directory. Kept in its own directory so the whole set can be removed when the
// sandbox is torn down.
const windowsSandboxSecretDirName = "windows-sandbox"

// windowsSandboxSecretPath returns where a principal's password lives. The
// account name is already sanitised to [a-z0-9-] by windowsSandboxUserName, so
// it cannot escape the directory.
func windowsSandboxSecretPath(configDir string, username string) (string, error) {
	if strings.TrimSpace(configDir) == "" {
		return "", errors.New("windows sandbox secret: empty config directory")
	}
	if strings.TrimSpace(username) == "" {
		return "", errors.New("windows sandbox secret: empty principal name")
	}
	// Defence in depth against a caller passing something windowsSandboxUserName
	// did not produce: refuse anything with a separator or a parent reference.
	if strings.ContainsAny(username, `\/:`) || strings.Contains(username, "..") {
		return "", fmt.Errorf("windows sandbox secret: unsafe principal name %q", username)
	}
	return filepath.Join(configDir, windowsSandboxSecretDirName, username+".secret"), nil
}

// currentTokenUserSID returns the SID of the user this process runs as. Under
// UAC the elevated token keeps the same user SID as the desktop session, so
// setup and the later unelevated command path agree on the owner, which is what
// makes an owner-scoped ACL usable across the elevation boundary.
func currentTokenUserSID() (*windows.SID, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return nil, fmt.Errorf("open process token: %w", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("get token user: %w", err)
	}
	// The SID points into a buffer owned by the Tokenuser, so copy it out before
	// that buffer goes away.
	copied, err := user.User.Sid.Copy()
	if err != nil {
		return nil, fmt.Errorf("copy token user SID: %w", err)
	}
	return copied, nil
}

// windowsSecretOwnerACL builds the explicit, inheritance-protected DACL granting
// only owner and SYSTEM. PROTECTED is what drops any ACE inherited from the
// config directory; without it a permissive parent would still grant access to
// whoever it names.
//
// Returns the ACL rather than applying it, because the caller applies it to an
// open HANDLE. Applying by pathname meant the object being locked was whatever
// the name resolved to at that instant, which is not necessarily the object that
// was just created.
func windowsSecretOwnerACL(owner *windows.SID) (*windows.ACL, error) {
	if owner == nil {
		return nil, errors.New("windows sandbox secret: nil owner SID")
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, fmt.Errorf("resolve SYSTEM SID: %w", err)
	}
	entries := []windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(owner),
			},
		},
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(system),
			},
		},
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return nil, fmt.Errorf("build secret ACL: %w", err)
	}
	return acl, nil
}

// writeWindowsSandboxSecret stores a principal's password readable only by the
// invoking user.
//
// The file is created empty, locked down, and only then written, so the secret
// is never on disk under the directory's inherited ACL. An existing file is
// replaced rather than appended, since a stale password would make LogonUser
// fail in a way that looks like a sandbox bug.
func writeWindowsSandboxSecret(path string, password string) error {
	owner, err := currentTokenUserSID()
	if err != nil {
		return err
	}
	// THE PARENT CHAIN IS PART OF THE RESOLUTION COUNT. os.MkdirAll walks a
	// pathname, so a junction planted on the way down pointed this elevated
	// create at a directory of the caller's choosing, and the no-follow open
	// below then refused a tree this process had already made outside the
	// sandbox home. The chain is built the same way the ACL targets are: from
	// the deepest existing ancestor, one component at a time relative to the
	// handle above it, with a record of what was created so a later failure
	// unwinds exactly that and nothing else.
	created, parent, err := makeWindowsACLDirChainNoFollow(filepath.Dir(path))
	if err != nil {
		if _, unwindErr := rollbackWindowsACLMaterialization(created); unwindErr != nil {
			err = errors.Join(err, unwindErr)
		}
		return fmt.Errorf("create secret directory: %w", err)
	}
	defer func() { _ = windows.CloseHandle(parent) }()
	unwindChain := func(cause error) error {
		if _, unwindErr := rollbackWindowsACLMaterialization(created); unwindErr != nil {
			return errors.Join(cause, unwindErr)
		}
		return cause
	}
	// Sealed BEFORE the file exists, so a failure here leaves nothing on disk at
	// all rather than an empty file for someone else to win a race on.
	//
	// Encrypted to the invoking user on top of the ACL, so a copy taken outside
	// the filesystem's enforcement (backup, disk image) is inert. The principal
	// name is the entropy, which keeps one principal's blob from authenticating
	// another.
	sealed, err := protectWindowsSecret(password, windowsSandboxSecretEntropy(path))
	if err != nil {
		return unwindChain(err)
	}
	// ONE resolution of the path, not four. The parent chain above was built
	// relative to handles, the leaf is created relative to the deepest of them,
	// and the DACL and the bytes are both applied to the leaf handle. Resolving
	// the name again between any two of those steps is what let a junction or
	// symlink swap point an elevated write at a file of the caller's choosing.
	leaf := filepath.Base(path)
	handle, err := createWindowsSecretFileAt(parent, leaf)
	if err != nil {
		return unwindChain(err)
	}
	leafClosed := false
	closeLeaf := func() {
		if leafClosed {
			return
		}
		leafClosed = true
		_ = windows.CloseHandle(handle)
	}
	defer closeLeaf()
	// CLEANUP ACTS ON THE OBJECT THE CREATE MADE, NOT ON THE NAME AGAIN.
	//
	// This used to call os.Remove(path), which is the one pathname resolution
	// the rest of this function was written to remove, and it did not even
	// work: the leaf is created with FILE_SHARE_READ alone, so a delete by
	// name while this handle is open comes back as a sharing violation. The
	// error was discarded, unwindChain rolled back only the directories, and a
	// failed provisioning left an empty secret file it believed it had removed.
	//
	// The handle is closed first and the leaf is then deleted relative to the
	// pinned parent, the same way rollbackWindowsACLMaterialization removes
	// what it created. Closing first rather than widening the share mask keeps
	// the secret file unshareable for delete while it exists, which is the
	// narrower of the two ways to make this work.
	removeLeaf := func(cause error) error {
		closeLeaf()
		if deleteErr := deleteWindowsSecretLeafFn(parent, leaf); deleteErr != nil {
			// Reported, not discarded: a secret file left behind after a failed
			// provisioning is state the caller has to know about, and the empty
			// file is the thing a later run would read as a stale password.
			cause = errors.Join(cause, fmt.Errorf("remove partially written secret %s: %w", path, deleteErr))
		}
		return unwindChain(cause)
	}
	if err := lockWindowsSecretHandleToOwnerFn(handle, owner); err != nil {
		return removeLeaf(err)
	}
	if err := writeWindowsSecretHandle(handle, sealed); err != nil {
		return removeLeaf(err)
	}
	return nil
}

// lockWindowsSecretHandleToOwnerFn is a seam for the one step between creating
// the secret file and writing it, so the cleanup that runs when it fails is
// reachable without an elevated machine or a way to make SetSecurityInfo fail.
var lockWindowsSecretHandleToOwnerFn = lockWindowsSecretHandleToOwner

// deleteWindowsSecretLeafFn is the matching seam for the cleanup itself, so the
// case where cleanup cannot finish is reachable without arranging a Windows
// sharing conflict, which is not something a test can stage reliably.
var deleteWindowsSecretLeafFn = deleteWindowsACLChildFile

// windowsSandboxSecretEntropy derives the DPAPI entropy from the secret's own
// filename, which is the principal name. Deriving it rather than threading the
// name through keeps read and write agreeing by construction.
func windowsSandboxSecretEntropy(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".secret")
}

// Seamed so the permission-denied mapping in readWindowsSandboxSecret is
// testable. Producing a real ERROR_ACCESS_DENIED needs DACL surgery on Windows,
// since a 0000 file is still readable and reading a directory reports
// "Incorrect function", so a test built that way would exercise the platform
// rather than the mapping.
var readWindowsSandboxSecretFile = os.ReadFile

// readWindowsSandboxSecret loads a principal's password. A missing file means
// setup has not run for this workspace, which the caller turns into a fallback
// rather than a hard failure.
func readWindowsSandboxSecret(path string) (string, error) {
	data, err := readWindowsSandboxSecretFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", errWindowsSandboxIdentityUnavailable
		}
		// Permission denied is unavailability, not breakage. The secret's DACL
		// names whoever ran setup, so an operator who elevated with a separate
		// administrative account, through runas or an over-the-shoulder UAC
		// prompt, ends up with a secret their ordinary account cannot open. That
		// is the documented fail-soft case: fall back to the restricted token and
		// let the warning say so. Treating it as a hard error instead made every
		// sandboxed command fail on a machine that was merely set up by a
		// different admin, which is a common way to run an elevated setup.
		if os.IsPermission(err) {
			return "", errWindowsSandboxIdentityUnavailable
		}
		return "", fmt.Errorf("read sandbox secret: %w", err)
	}
	if len(data) == 0 {
		return "", errWindowsSandboxIdentityUnavailable
	}
	secret, err := unprotectWindowsSecret(data, windowsSandboxSecretEntropy(path))
	if err != nil {
		// A blob written by another user, for another principal, or by an older
		// build that stored the password in the clear. Report it as unavailable so
		// the caller falls back to the restricted token; the next elevated setup
		// rewrites the secret in the current format.
		return "", errWindowsSandboxIdentityUnavailable
	}
	if strings.TrimSpace(secret) == "" {
		return "", errWindowsSandboxIdentityUnavailable
	}
	return secret, nil
}

// removeWindowsSandboxSecret deletes a stored password. Called before the
// account itself is removed so a secret never outlives the principal it
// authenticates.
// errWindowsSandboxSecretNotOurs reports a secret this account cannot delete
// because another administrator's setup owns its DACL.
//
// Distinguishable so teardown can carry on. The alternative is what it used to
// do: abort before removing the account, its logon rights, its ACEs and its
// ledger, leaving a fully provisioned principal on the machine because one file
// could not be unlinked.
var errWindowsSandboxSecretNotOurs = errors.New("the sandbox secret belongs to a different administrator's setup")

func removeWindowsSandboxSecret(path string) error {
	err := os.Remove(path)
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	// The read path already treats permission-denied as unavailability rather
	// than breakage, for the reason documented on readWindowsSandboxSecret: an
	// operator who elevated with a separate administrative account gets a secret
	// their ordinary account cannot open. Removal has to agree. It did not, so on
	// exactly those machines teardown stopped at the first file and stranded
	// everything after it.
	if os.IsPermission(err) {
		return fmt.Errorf("%w: %s", errWindowsSandboxSecretNotOurs, path)
	}
	return fmt.Errorf("remove sandbox secret: %w", err)
}
