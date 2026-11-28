//go:build windows

package sandbox

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsSandboxSecretRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg", "zero-sbx-test.secret")
	const password = "Zs1!EXAMPLEPASSWORDVALUE"

	if err := writeWindowsSandboxSecret(path, password); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := readWindowsSandboxSecret(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != password {
		t.Fatalf("read %q, want the stored password", got)
	}
}

// THE security property: the stored password must be readable only by the user
// who owns it. If any other trustee appears in the DACL, and in particular the
// sandbox principal, that account could mint its own token and the identity
// boundary would be worthless.
func TestWindowsSandboxSecretIsLockedToOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locked.secret")
	if err := writeWindowsSandboxSecret(path, "Zs1!SECRET"); err != nil {
		t.Fatalf("write: %v", err)
	}

	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatalf("read back security info: %v", err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatalf("read DACL: %v", err)
	}
	if dacl == nil {
		t.Fatal("secret has a nil DACL, which grants everyone access")
	}

	owner, err := currentTokenUserSID()
	if err != nil {
		t.Fatalf("owner SID: %v", err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatalf("SYSTEM SID: %v", err)
	}

	entries, err := windowsSecretACEList(dacl)
	if err != nil {
		t.Fatalf("enumerate ACEs: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("secret DACL has no ACEs")
	}
	for _, sid := range entries {
		if sid.Equals(owner) || sid.Equals(system) {
			continue
		}
		t.Fatalf("secret DACL grants an unexpected trustee %s; only the owner and SYSTEM may appear", sid)
	}
}

// The DACL must be inheritance-protected, otherwise a permissive ACE on the
// config directory would still reach the secret.
func TestWindowsSandboxSecretDaclIsProtected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "protected.secret")
	if err := writeWindowsSandboxSecret(path, "Zs1!SECRET"); err != nil {
		t.Fatalf("write: %v", err)
	}
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("read back security info: %v", err)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatalf("read control bits: %v", err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("secret DACL is not protected, so inherited ACEs still apply")
	}
}

// Rewriting must replace the previous secret rather than append to it, or
// LogonUser would be handed two concatenated passwords.
func TestWindowsSandboxSecretOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rewrite.secret")
	if err := writeWindowsSandboxSecret(path, "Zs1!FIRST"); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := writeWindowsSandboxSecret(path, "Zs1!SECOND"); err != nil {
		t.Fatalf("second write: %v", err)
	}
	got, err := readWindowsSandboxSecret(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != "Zs1!SECOND" {
		t.Fatalf("read %q, want only the newest password", got)
	}
}

// A workspace whose setup has not run must report the actionable sentinel so the
// command path falls back to the restricted-token backend instead of failing.
func TestWindowsSandboxSecretMissingIsSentinel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.secret")
	if _, err := readWindowsSandboxSecret(path); err != errWindowsSandboxIdentityUnavailable {
		t.Fatalf("missing secret returned %v, want errWindowsSandboxIdentityUnavailable", err)
	}
}

// An empty file is a half-written secret, not a valid empty password.
//
// Both seeds matter and only one of them tests what the name says. A
// zero-length file is the truncated-write case, and it is the only one that
// reaches the length check. Whitespace is several bytes, so it travels on to
// DPAPI and fails to unprotect instead, which is the path
// TestWindowsSandboxSecretRejectsLegacyPlaintext already covers. Seeding only
// the whitespace, as this did, left the branch in the test's own name
// unexercised.
func TestWindowsSandboxSecretEmptyIsSentinel(t *testing.T) {
	for name, seed := range map[string][]byte{
		"truncated write": {},
		"whitespace only": []byte("   \r\n"),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "empty.secret")
			if err := os.WriteFile(path, seed, 0o600); err != nil {
				t.Fatalf("seed: %v", err)
			}
			if _, err := readWindowsSandboxSecret(path); err != errWindowsSandboxIdentityUnavailable {
				t.Fatalf("secret of %d bytes returned %v, want the unavailable sentinel", len(seed), err)
			}
		})
	}
}

// The principal name lands in a filename, so anything that could escape the
// directory has to be refused even though windowsSandboxUserName already
// sanitises its output.
func TestWindowsSandboxSecretPathRejectsTraversal(t *testing.T) {
	for _, name := range []string{`..\evil`, "sub/dir", `C:\abs`, "..", ""} {
		if _, err := windowsSandboxSecretPath(`C:\cfg`, name); err == nil {
			t.Fatalf("principal name %q was accepted", name)
		}
	}
	if _, err := windowsSandboxSecretPath("", "zero-sbx-a"); err == nil {
		t.Fatal("empty config directory was accepted")
	}
	path, err := windowsSandboxSecretPath(`C:\cfg`, "zero-sbx-abc")
	if err != nil {
		t.Fatalf("valid name rejected: %v", err)
	}
	if !strings.HasSuffix(path, `zero-sbx-abc.secret`) {
		t.Fatalf("unexpected secret path %q", path)
	}
}

// Removal must be idempotent so teardown converges the same way provisioning
// does, and must actually delete the secret.
func TestWindowsSandboxSecretRemoveIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gone.secret")
	if err := writeWindowsSandboxSecret(path, "Zs1!SECRET"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := removeWindowsSandboxSecret(path); err != nil {
		t.Fatalf("first remove: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("secret still present after removal (stat err %v)", err)
	}
	if err := removeWindowsSandboxSecret(path); err != nil {
		t.Fatalf("removing an absent secret must succeed, got %v", err)
	}
}

// windowsSecretACEList returns the trustee SID of every ACE in a DACL so a test
// can assert exactly who is named.
func windowsSecretACEList(dacl *windows.ACL) ([]*windows.SID, error) {
	var out []*windows.SID
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return nil, err
		}
		// GetAce hands back a generic ACE_HEADER and we reinterpret it. That is
		// only sound for the fixed-layout types: an object ACE carries Flags and
		// two GUIDs ahead of the trustee, so SidStart would land mid-structure
		// and Copy would read whatever bytes happen to be there. The caller's
		// "unexpected trustee" assertion would then print a nonsense SID instead
		// of naming the ACE that does not belong, which is the opposite of what
		// a failing test should do. Nothing under test builds anything but
		// allowed ACEs today, so this exists to keep the failure legible if that
		// ever changes.
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return nil, fmt.Errorf("ACE %d has type %d, want ACCESS_ALLOWED_ACE_TYPE (%d); refusing to decode its trustee", index, ace.Header.AceType, windows.ACCESS_ALLOWED_ACE_TYPE)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		copied, err := sid.Copy()
		if err != nil {
			return nil, err
		}
		out = append(out, copied)
	}
	return out, nil
}

// DPAPI round-trip through the real store, which needs no privilege and so is
// genuine coverage rather than a gated stub.
func TestWindowsSandboxSecretRoundTripsThroughDPAPI(t *testing.T) {
	path, err := windowsSandboxSecretPath(t.TempDir(), "zero-sbx-roundtrip")
	if err != nil {
		t.Fatalf("secret path: %v", err)
	}
	const password = "S0me-Sandbox-P@ssw0rd-value"
	if err := writeWindowsSandboxSecret(path, password); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	got, err := readWindowsSandboxSecret(path)
	if err != nil {
		t.Fatalf("read secret: %v", err)
	}
	if got != password {
		t.Fatalf("round-trip returned %q, want %q", got, password)
	}
	// The point of the exercise: the password must not be recoverable by reading
	// the file, or the encryption layer is decorative.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read raw secret file: %v", err)
	}
	if bytes.Contains(raw, []byte(password)) {
		t.Fatal("the password appears verbatim in the stored file; it was not encrypted")
	}
}

// Entropy is the principal name, so a blob moved onto another principal's secret
// path must fail to decrypt rather than authenticate the wrong account.
func TestWindowsSandboxSecretDoesNotTransferBetweenPrincipals(t *testing.T) {
	home := t.TempDir()
	minePath, err := windowsSandboxSecretPath(home, "zero-sbx-mine")
	if err != nil {
		t.Fatalf("secret path: %v", err)
	}
	theirsPath, err := windowsSandboxSecretPath(home, "zero-sbx-theirs")
	if err != nil {
		t.Fatalf("secret path: %v", err)
	}
	if err := writeWindowsSandboxSecret(minePath, "a-password-for-mine"); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	blob, err := os.ReadFile(minePath)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if err := os.WriteFile(theirsPath, blob, 0o600); err != nil {
		t.Fatalf("plant blob: %v", err)
	}
	if _, err := readWindowsSandboxSecret(theirsPath); !errors.Is(err, errWindowsSandboxIdentityUnavailable) {
		t.Fatalf("a blob planted at another principal's path decrypted; got err = %v", err)
	}
}

// An older plaintext secret must degrade to a fallback rather than being handed
// to LogonUser as if it were a password.
func TestWindowsSandboxSecretRejectsLegacyPlaintext(t *testing.T) {
	path, err := windowsSandboxSecretPath(t.TempDir(), "zero-sbx-legacy")
	if err != nil {
		t.Fatalf("secret path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("plaintext-password"), 0o600); err != nil {
		t.Fatalf("write legacy secret: %v", err)
	}
	if _, err := readWindowsSandboxSecret(path); !errors.Is(err, errWindowsSandboxIdentityUnavailable) {
		t.Fatalf("legacy plaintext secret was accepted; got err = %v", err)
	}
}
