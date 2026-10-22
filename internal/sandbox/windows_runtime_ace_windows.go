//go:build windows

package sandbox

import (
	"fmt"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// verifyWindowsRuntimeRootCapability confirms the runtime roots this command is
// about to write to still carry the capability ACE setup gave them.
//
// THE MARKER ATTESTS TO A PLAN, NOT TO AN OBJECT. cleanupSandboxRuntimeRoots
// removes inactive sibling workspace roots on an age and count policy. When that
// workspace runs again, command-side preparation recreates the SAME deterministic
// pathname as the ordinary caller, and the new directory inherits from its
// parent: it does not carry the capability SID ACE that elevated setup applied to
// the object that used to be there. ValidateWindowsSandboxSetupMarker still
// passes, because it fingerprints pathnames and actions rather than the identity
// of the ACL-bearing object, so the restricted child launched and then failed its
// cache, temp and package-cache writes with a bare ACCESS_DENIED and nothing
// pointing at setup.
//
// Cleanup treats these directories as disposable cache state; setup and its
// marker treat their DACL as durable provisioned state. Checking the ACE here is
// what reconciles the two: the command refuses with the remedy instead of
// launching into a tree it cannot write.
func verifyWindowsRuntimeRootCapability(config WindowsSandboxCommandConfig) error {
	runtime := config.PermissionProfile.Runtime
	if runtime == nil {
		return nil
	}
	root := strings.TrimSpace(runtime.Root)
	if root == "" {
		return nil
	}
	sid, err := windowsCapabilitySIDForWriteRoot(config, root)
	if err != nil {
		return fmt.Errorf("resolve the sandbox runtime capability for %s: %w", root, err)
	}
	present, err := windowsPathGrantsCapability(root, sid)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("the sandbox runtime root %s does not exist, so setup's provisioning is gone — run `zero sandbox setup` from an elevated (Administrator) terminal", root)
		}
		return fmt.Errorf("inspect the sandbox runtime root %s: %w", root, err)
	}
	if !present {
		return fmt.Errorf("the sandbox runtime root %s no longer carries the capability grant setup applied to it, "+
			"which happens when the directory was reclaimed and recreated by an ordinary run; "+
			"every sandboxed write into it would fail with access denied — run `zero sandbox setup` from an elevated (Administrator) terminal", root)
	}
	return nil
}

// windowsPathGrantsCapability reports whether path's DACL carries an allow ACE
// for sid. Read off the object, because the whole point is that the pathname
// says nothing about which object now answers to it.
func windowsPathGrantsCapability(path string, sid string) (bool, error) {
	wanted, err := windows.StringToSid(sid)
	if err != nil {
		return false, fmt.Errorf("parse the capability SID %q: %w", sid, err)
	}
	handle, _, err := openWindowsACLTarget(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		return false, err
	}
	// GetAce hands back a generic header that is reinterpreted here, which is
	// sound only for the fixed-layout ACE types: an object ACE carries Flags and
	// two GUIDs ahead of the trustee, so SidStart would land mid-structure. A type
	// this does not recognise is skipped rather than decoded into a nonsense SID.
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}
		aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if aceSID.Equals(wanted) {
			return true, nil
		}
	}
	return false, nil
}
