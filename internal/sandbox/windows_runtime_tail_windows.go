//go:build windows

package sandbox

import (
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// windowsFileAddFile is FILE_ADD_FILE, the directory right to create a file in
// it. x/sys/windows does not export it.
const windowsFileAddFile = 0x00000002

// A PATHNAME IS NOT AN OBJECT.
//
// refuseReparsedRuntimeAncestors inspects the owned components before and after
// creation, and that is a check-then-use however many times it runs. Everything
// afterwards reopens the tree BY NAME: openWindowsACLTarget passes the whole
// path to CreateFile, and FILE_FLAG_OPEN_REPARSE_POINT governs only the final
// component, so every ancestor is resolved normally. A local user who plants a
// junction at an owned ancestor between the last check and the open gets the
// capability ACL written to an ordinary leaf inside a directory they chose.
// Windows junctions need no privilege to create, so this is not a theoretical
// attacker.
//
// A second Lstat narrows that window; it cannot close it. The only thing that
// closes it is never resolving the path again: open the base once, then descend
// one component at a time RELATIVE TO THE HANDLE ABOVE IT, refusing a reparse
// point at each step, and use the handle that comes out for everything that
// follows. NtCreateFile is what allows a relative open at all; Win32 CreateFile
// has no equivalent.

// runtimeTailNotOwned wraps errRuntimeTailNotOwned with the path refused. It
// lives here rather than beside the sentinel because this traversal is its only
// caller, and in the shared file it was dead code on every other platform.
func runtimeTailNotOwned(root string) error {
	return fmt.Errorf("%w: %s", errRuntimeTailNotOwned, root)
}

// openWindowsRuntimeTailDirectory walks the components Zero owns and returns a
// handle to the runtime root itself. The caller closes it.
func openWindowsRuntimeTailDirectory(root string, access uint32) (windows.Handle, error) {
	base, components, ok := windowsSandboxRuntimeOwnedTail(root)
	if !ok {
		// NOT a fallback to opening by name. A path that does not have a runtime
		// root's shape is one this traversal cannot vouch for, and the whole point
		// is to stop trusting a name.
		return 0, runtimeTailNotOwned(root)
	}
	// The base belongs to the user, so it is opened by name and its own reparse
	// points are followed: a redirected LOCALAPPDATA is an ordinary machine
	// configuration, not an attack.
	parent, err := openWindowsDirectoryByName(base)
	if err != nil {
		return 0, fmt.Errorf("open sandbox runtime base %s: %w", base, err)
	}
	for index, name := range components {
		// FILE_READ_ATTRIBUTES on every component, including the intermediates:
		// each open is followed by a GetFileInformationByHandle to decide whether
		// it is a reparse point, and without that right the check itself fails with
		// "Access is denied" and refuses the whole tree.
		wanted := access | windows.FILE_READ_ATTRIBUTES
		if index < len(components)-1 {
			// Intermediate components are only traversed.
			wanted = windows.FILE_TRAVERSE | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE
		}
		child, err := openWindowsChildNoFollow(parent, name, wanted, windows.FILE_DIRECTORY_FILE)
		_ = windows.CloseHandle(parent)
		if err != nil {
			return 0, err
		}
		parent = child
	}
	return parent, nil
}

func openWindowsDirectoryByName(path string) (windows.Handle, error) {
	utf16Path, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(
		utf16Path,
		windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
}

// openWindowsChildNoFollow opens exactly one component beneath parent.
//
// FILE_OPEN_REPARSE_POINT makes the open land on a link rather than following
// it, so a swapped component is opened as the link it is and then refused,
// instead of silently resolving into somebody else's tree. Since the name is
// relative to a handle, no ancestor is re-resolved and there is no interval for
// a swap to land in.
func openWindowsChildNoFollow(parent windows.Handle, name string, access uint32, options uint32) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, fmt.Errorf("encode sandbox runtime component %s: %w", name, err)
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		RootDirectory: parent,
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	attributes.Length = uint32(unsafe.Sizeof(attributes))

	var handle windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(
		&handle,
		access|windows.SYNCHRONIZE,
		&attributes,
		&iosb,
		nil,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		options|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT,
		0,
		0,
	)
	if err != nil {
		return 0, fmt.Errorf("open sandbox runtime component %s: %w", name, err)
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = windows.CloseHandle(handle)
		return 0, fmt.Errorf("inspect sandbox runtime component %s: %w", name, err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(handle)
		return 0, fmt.Errorf("refusing to use the sandbox runtime through a link at %s: a reparse point here redirects the directory the sandbox is granted write access to", name)
	}
	return handle, nil
}

// writeWindowsRuntimeStampToDirectoryHandle publishes the stamp into an ALREADY
// OPEN directory, naming nothing. The caller holds the handle the capability ACE
// was applied through, so the stamp cannot land anywhere else. The stamp is
// replaced in one step; see publishWindowsRuntimeStamp.
func writeWindowsRuntimeStampToDirectoryHandle(directory windows.Handle, name string, planHash string) error {
	// BEFORE ANYTHING IS CREATED. A reader that could not be established used to
	// fail after the stamp had already been truncated. Everything that can fail
	// without touching the stamp still fails before anything is created.
	// Reported by CodeRabbit.
	reader, err := windowsRuntimeStampReader(directory)
	if err != nil {
		return err
	}
	// PROTECTED FROM THE FIRST INSTANT, because the stamp lives inside the tree
	// it attests. See windowsRuntimeStampDACL.
	descriptor, err := windowsRuntimeStampSecurityDescriptor(reader)
	if err != nil {
		return err
	}
	return publishWindowsRuntimeStamp(directory, name, []byte(planHash), descriptor)
}

// windowsRuntimeStampReader resolves the identity that has to read the stamp
// AFTER setup returns, from the runtime root the stamp is created in.
//
// Taken from the DIRECTORY HANDLE rather than the setup token, because the
// question is not who elevated but whose install this is. Setup runs elevated
// and may run as a different administrator account than the one that later runs
// the command or zero doctor; the runtime root lives under the ordinary user
// profile and its owner is stable across that boundary.
// windowsSetupConsumerSID is the ordinary reader carried across the elevation
// boundary by `zero sandbox setup`, resolved in the operator's shell.
//
// Guarded because tests set it; production writes it once, before setup touches
// anything, and clears it on the way out.
var (
	windowsSetupConsumerMu  sync.Mutex
	windowsSetupConsumerSID *windows.SID
)

func setWindowsSetupConsumerSID(sid *windows.SID) func() {
	windowsSetupConsumerMu.Lock()
	previous := windowsSetupConsumerSID
	windowsSetupConsumerSID = sid
	windowsSetupConsumerMu.Unlock()
	return func() {
		windowsSetupConsumerMu.Lock()
		windowsSetupConsumerSID = previous
		windowsSetupConsumerMu.Unlock()
	}
}

func carriedWindowsSetupConsumerSID() *windows.SID {
	windowsSetupConsumerMu.Lock()
	defer windowsSetupConsumerMu.Unlock()
	return windowsSetupConsumerSID
}

// windowsRuntimeStampReader resolves the identity that must READ the stamp once
// setup has returned.
//
// The carried SID wins. It is the token that will actually run the commands,
// resolved before elevation, and it is the only source that survives the token
// boundary: the elevated helper CREATES the runtime leaf when it is absent, so
// deriving the reader from that leaf yields BUILTINAdministrators. A later
// UAC-filtered administrator carries that group deny-only and a standard user
// given alternate admin credentials is not in it at all, so the protected stamp
// would end up with no enabled allow ACE for the token that has to validate it,
// and every restricted command would stop before launch on a setup that had
// just reported success.
//
// The owner fallback stays for the paths that are not setup, notably rollback
// recreating a stamp it just removed, where the leaf already exists and belongs
// to whoever owns the install.
func windowsRuntimeStampReader(directory windows.Handle) (*windows.SID, error) {
	if carried := carriedWindowsSetupConsumerSID(); carried != nil {
		return carried, nil
	}
	descriptor, err := windows.GetSecurityInfo(directory, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return nil, fmt.Errorf("read the sandbox runtime root owner: %w", err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return nil, fmt.Errorf("read the sandbox runtime root owner: %w", err)
	}
	if owner == nil {
		return nil, fmt.Errorf("read the sandbox runtime root owner: the descriptor carried none")
	}
	return owner, nil
}

// windowsRuntimeStampDACL builds the stamp its own DACL, excluding the
// capability SID the sandboxed command runs with.
//
// THE ATTESTATION CANNOT LIVE IN THE SUBJECT'S OWN WRITABLE NAMESPACE. The
// runtime root carries an AllowWrite entry for the capability SID with
// SUB_CONTAINERS_AND_OBJECTS_INHERIT, which is exactly what lets a sandboxed
// command write TMP, GOCACHE and the package-manager caches under it. A file
// created inside that root inherits the same grant, so the restricted command
// could open the stamp and overwrite it after passing its own pre-launch
// validation. Its current command would continue, and every later elevated
// command and zero doctor would then reject the altered plan hash until an
// Administrator re-ran setup: a sandboxed process bricking the sandbox.
//
// Moving the stamp outside the tree is the other way to fix it and is worse:
// the stamp works precisely because it dies with the tree, so eviction is
// detectable without reading an ACE. Keeping it inside with inheritance
// switched off preserves that and closes the hole.
//
// PROTECTED, not merely explicit: without SE_DACL_PROTECTED the inherited
// capability ACE stays in the DACL alongside whatever is set here.
func windowsRuntimeStampDACL(reader *windows.SID) (*windows.ACL, error) {
	if reader == nil {
		return nil, fmt.Errorf("resolve the reader SID for the sandbox runtime stamp: no identity was supplied")
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, fmt.Errorf("resolve LocalSystem SID for the sandbox runtime stamp: %w", err)
	}
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, fmt.Errorf("resolve Administrators SID for the sandbox runtime stamp: %w", err)
	}
	// Setup writes it, doctor and the elevated command read it; nothing else
	// needs to reach it, and the capability SID is deliberately absent.
	//
	// The reader is named EXPLICITLY and gets READ ONLY. It used to be
	// WinCreatorOwnerSid at GENERIC_ALL. SetSecurityInfo does substitute that
	// placeholder even in a NO_INHERITANCE ACE, so the resulting ACE did name a
	// concrete SID, but the one it named was whoever happened to run setup. When
	// setup is elevated by a different administrator account than the one that
	// later runs the command or zero doctor, the reader matches no ACE and
	// os.ReadFile on the stamp returns Access is denied, so a successful setup
	// hands over an unreadable attestation. Resolving the identity from the
	// runtime root the stamp lives in binds it to the install rather than to the
	// elevation.
	//
	// Read and DELETE, not write. Withholding write from the SANDBOX is the real
	// boundary and the capability SID has no ACE here at all. Withholding it from
	// the root owner is not: they own the parent directory, so delete-then-create
	// forges a stamp exactly as well as an overwrite would. What read-only would
	// actually cost is rollback, which has to remove a stamp this run wrote under
	// the same token that wrote it.
	//
	// So: no FILE_WRITE_DATA, no WRITE_DAC, no WRITE_OWNER, which keeps an
	// accidental in-place rewrite off the table, and DELETE so compensation can
	// undo its own work and the next publish can rename a replacement over it.
	// The publish itself still writes: see createWindowsStampReplacement.
	// The reader can BE one of the repair identities. A runtime root created by
	// an elevated process is commonly owned by BUILTINAdministrators rather than
	// by the invoking user, which is what CI runners do. Naming the same SID
	// twice let the narrower read-only entry win and left repair unable to rewrite
	// the stamp, so skip the reader entry when the broader grant already covers it.
	entries := make([]windows.EXPLICIT_ACCESS, 0, 3)
	if !reader.Equals(system) && !reader.Equals(administrators) {
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.FILE_GENERIC_READ | windows.DELETE,
			AccessMode:        windows.SET_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(reader),
			},
		})
	}
	for _, sid := range []*windows.SID{system, administrators} {
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.SET_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		})
	}
	dacl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return nil, fmt.Errorf("build the sandbox runtime stamp DACL: %w", err)
	}
	return dacl, nil
}

// windowsRuntimeStampSecurityDescriptor is the descriptor a stamp is created
// with: windowsRuntimeStampDACL, protected so nothing is inherited from the root.
func windowsRuntimeStampSecurityDescriptor(reader *windows.SID) (*windows.SECURITY_DESCRIPTOR, error) {
	dacl, err := windowsRuntimeStampDACL(reader)
	if err != nil {
		return nil, err
	}
	descriptor, err := windows.NewSecurityDescriptor()
	if err != nil {
		return nil, fmt.Errorf("build the sandbox runtime stamp security descriptor: %w", err)
	}
	if err := descriptor.SetDACL(dacl, true, false); err != nil {
		return nil, fmt.Errorf("build the sandbox runtime stamp security descriptor: %w", err)
	}
	if err := descriptor.SetControl(windows.SE_DACL_PROTECTED, windows.SE_DACL_PROTECTED); err != nil {
		return nil, fmt.Errorf("protect the sandbox runtime stamp security descriptor: %w", err)
	}
	return descriptor, nil
}

// writeWindowsRuntimeStampThroughHandle writes the setup stamp INTO the object
// the traversal reached, not into whatever the pathname resolves to now.
//
// The old writer used MkdirAll and a pathname write, which left a second
// unbound interval: a tree replaced after the ACL apply could be recreated and
// stamped without ever carrying the capability grant, and marker validation
// still passed because it only reads the stamp's contents. The restricted
// process then got a marker-valid runtime path with no grant on it.
//
// ONE WRITER. This path used to carry its own copy of the create, protect and
// write sequence, so an ordering fix to one copy left the other behind. It
// delegates now, and the stamp is protected and ordered the same way whichever
// handle it arrives through.
func writeWindowsRuntimeStampThroughHandle(root string, planHash string) error {
	directory, err := openWindowsRuntimeTailDirectory(root, windows.FILE_TRAVERSE|windowsFileAddFile|windows.READ_CONTROL|windows.SYNCHRONIZE)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(directory)
	return writeWindowsRuntimeStampToDirectoryHandle(directory, windowsSandboxRuntimeStampName(planHash), planHash)
}
