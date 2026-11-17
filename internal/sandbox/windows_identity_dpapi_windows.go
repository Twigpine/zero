//go:build windows

package sandbox

// DPAPI wrapping for the stored sandbox principal password.
//
// The file ACL is the primary control and remains the thing that keeps the
// sandbox principal itself from reading its own credential. This adds the layer
// the ACL cannot: an ACL is only meaningful while the filesystem is being asked
// to enforce it, so a backup, a mounted disk image, or a copy taken by anyone
// who can bypass the DACL yields the password in the clear. CryptProtectData
// binds the ciphertext to the invoking user's logon secret, so an offline copy
// is inert without that user's credentials.
//
// The principal name is passed as optional entropy, which makes a blob usable
// only for the account it was minted for; moving one secret file over another
// then fails to decrypt instead of silently authenticating the wrong principal.

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// protectWindowsSecret encrypts a password to the current user.
//
// CRYPTPROTECT_UI_FORBIDDEN matters here: this runs inside a CLI and, on the
// setup path, potentially without an interactive desktop, so DPAPI must fail
// rather than try to prompt.
func protectWindowsSecret(plaintext string, entropy string) ([]byte, error) {
	if plaintext == "" {
		return nil, errors.New("windows sandbox secret: refusing to protect an empty password")
	}
	in := windows.DataBlob{
		Size: uint32(len(plaintext)),
		Data: &[]byte(plaintext)[0],
	}
	entropyBytes := []byte(entropy)
	var entropyBlob *windows.DataBlob
	if len(entropyBytes) > 0 {
		entropyBlob = &windows.DataBlob{Size: uint32(len(entropyBytes)), Data: &entropyBytes[0]}
	}
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, entropyBlob, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, fmt.Errorf("protect sandbox secret: %w", err)
	}
	// DPAPI allocates the output with LocalAlloc; copy it out and hand it back.
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}

// unprotectWindowsSecret reverses protectWindowsSecret. It fails for any user
// other than the one that wrote the blob, and for a blob minted with a different
// principal name as entropy.
func unprotectWindowsSecret(ciphertext []byte, entropy string) (string, error) {
	if len(ciphertext) == 0 {
		return "", errWindowsSandboxIdentityUnavailable
	}
	in := windows.DataBlob{
		Size: uint32(len(ciphertext)),
		Data: &ciphertext[0],
	}
	entropyBytes := []byte(entropy)
	var entropyBlob *windows.DataBlob
	if len(entropyBytes) > 0 {
		entropyBlob = &windows.DataBlob{Size: uint32(len(entropyBytes)), Data: &entropyBytes[0]}
	}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, entropyBlob, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return "", fmt.Errorf("unprotect sandbox secret: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return string(unsafe.Slice(out.Data, out.Size)), nil
}
