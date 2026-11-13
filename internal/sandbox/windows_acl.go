package sandbox

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// isWindowsVolumeRoot reports whether a cleaned path is the top of a volume,
// with nothing above it: `C:\`, a bare separator, or a UNC share root.
//
// Detected structurally rather than by pattern matching drive letters, because
// filepath.Dir of a root is that same root and of anything else is strictly
// shorter. That holds for drive-qualified paths, for the separator alone, and
// for UNC roots, on either build host.
func isWindowsVolumeRoot(path string) bool {
	cleaned := filepath.Clean(strings.TrimSpace(path))
	if cleaned == "" || cleaned == "." {
		return false
	}
	return filepath.Dir(cleaned) == cleaned
}

// validateWindowsACLComponent rejects anything that is not a single path
// component.
//
// This is load-bearing in two places, which is why it lives in the portable file
// rather than beside either of them.
//
// At apply time NtCreateFile happily resolves a RELATIVE name containing
// separators, and it resolves it the ordinary way, so an intermediate junction
// inside that name is followed and the object lands outside the pinned parent.
// A name with a separator reopens exactly the hole the parent handle exists to
// close.
//
// At plan time the same shape escapes the write root: a name is joined onto the
// root to place a deny ACE, so ".." or a separator puts that ACE on a directory
// outside the workspace entirely.
//
// The separators are checked explicitly rather than via filepath.Base, because
// these are Windows paths whatever the build host is, and on Linux
// filepath.Base leaves a backslash-joined name untouched and would wave it
// through. A colon is rejected too: it names an alternate data stream or a
// drive, neither of which is a child.
func validateWindowsACLComponent(name string) error {
	switch {
	case name == "":
		return errors.New("windows ACL path component is empty")
	case name == "." || name == "..":
		return fmt.Errorf("windows ACL path component %q is a relative reference, not a child", name)
	case strings.ContainsAny(name, `\/`):
		return fmt.Errorf("windows ACL path component %q contains a separator, so it would resolve through intermediate directories instead of staying a child", name)
	case strings.Contains(name, ":"):
		return fmt.Errorf("windows ACL path component %q contains a colon, which names a stream or a drive rather than a child", name)
	}
	return nil
}

type WindowsACLAction string

const (
	WindowsACLAllowWrite WindowsACLAction = "allow-write"
	WindowsACLDenyRead   WindowsACLAction = "deny-read"
	WindowsACLDenyWrite  WindowsACLAction = "deny-write"
	// WindowsACLDenyDelete denies removing or renaming the object it names,
	// WITHOUT denying writes to it or inside it, and without inheriting.
	//
	// It exists for .git. The write-denied carveouts live on .git/config and
	// .git/hooks as objects, so replacing the .git directory discards them: the
	// recreated config and hooks inherit the workspace allow with no deny, which
	// restores credential.helper and core.hooksPath. .git cannot simply join
	// sandboxFullyProtectedMetadataNames, because DenyWrite's mask includes
	// FILE_GENERIC_WRITE and git must write index, objects and refs.
	WindowsACLDenyDelete WindowsACLAction = "deny-delete"
	// WindowsACLRevokeCapability removes any existing ACE (allow or deny) for
	// Capability at Path, without itself granting or denying anything (applied
	// via SetEntriesInAclW's SET_ACCESS mode with a zero mask, not
	// REVOKE_ACCESS — see windowsACLAccess for why). It is consumed by
	// applyWindowsACLPlan for migration cleanup and tests; no current
	// plan-generation path emits this action, preserving legacy-process confinement.
	WindowsACLRevokeCapability WindowsACLAction = "revoke-capability"
)

type WindowsACLEntry struct {
	Action     WindowsACLAction `json:"action"`
	Path       string           `json:"path"`
	Capability string           `json:"capability"`
	// NoInherit forces the applied ACE to carry no inheritance flags, even
	// when the target is a directory. Without it, applyWindowsACLPlan makes
	// every directory ACE inheritable (SUB_CONTAINERS_AND_OBJECTS_INHERIT),
	// and SetNamedSecurityInfo automatically propagates any inheritable ACE
	// down onto the target's EXISTING descendants (not just new ones it
	// creates going forward), which is why direct-only denies must set this
	// flag rather than rely on inheritance.
	NoInherit bool `json:"noInherit,omitempty"`
	// Anchor is the write root Path was DERIVED from, for the paths this
	// package constructs rather than the operator naming. The apply requires
	// the object it finally opens to still live under it, so a reparse point
	// planted on the derived tail cannot walk an elevated ACL write out of the
	// sandbox. Empty for an operator-named path, which has no owned tail.
	Anchor      string `json:"anchor,omitempty"`
	Materialize bool   `json:"materialize,omitempty"`
	// MaterializeFile makes Materialize create an empty FILE instead of a
	// directory. Only meaningful with Materialize. .git/config is the case that
	// forces the distinction: created as a directory it does not merely carry
	// the wrong ACL, it makes `git init` fail outright.
	MaterializeFile bool `json:"materializeFile,omitempty"`
}

type WindowsACLPlan struct {
	Entries []WindowsACLEntry `json:"entries"`
}

func BuildWindowsACLPlan(config WindowsSandboxCommandConfig) (WindowsACLPlan, error) {
	if config.PermissionProfile.FileSystem.Kind != FileSystemRestricted {
		return WindowsACLPlan{}, errors.New("windows ACL setup requires a restricted filesystem permission profile")
	}
	writeCapabilities, err := windowsWriteRootCapabilities(config)
	if err != nil {
		return WindowsACLPlan{}, err
	}
	var entries []WindowsACLEntry
	for _, capability := range writeCapabilities {
		entries = append(entries, WindowsACLEntry{
			Action:     WindowsACLAllowWrite,
			Path:       capability.Root,
			Capability: capability.SID,
		})
		// The grant above carries DELETE, so the same object guard the principal
		// plan has must come with it: not DenyWrite (git writes index, objects and
		// refs), not materialized (git creates .git, and an empty one breaks
		// git init), and not inherited, so everything underneath stays writable.
		entries = append(entries, WindowsACLEntry{
			Action:     WindowsACLDenyDelete,
			Path:       windowsRenameProtectedObject(capability.Root),
			Capability: capability.SID,
		})
		// MATERIALIZED, LIKE THE PRINCIPAL PLAN'S, AND FOR TWO REASONS.
		//
		// First, a guard attached to an object that does not exist is not applied.
		// On a workspace that had no .git when setup ran, the first pass skipped all
		// three of these as missing and the deferred pass skipped them again,
		// because nothing else in this plan created them. Setup still recorded
		// success. A later git init then created .git, config and hooks beneath the
		// already-granted workspace, where they inherit the allow, DELETE included,
		// with no object-specific deny of their own: the sandboxed command could
		// rename .git aside, recreate it, and get credential.helper and
		// core.hooksPath back.
		//
		// Second, materializing these is what lets the applier's deferred pass land
		// the deny-delete on .git above, since creating .gitconfig and .githooks
		// creates .git as their parent.
		//
		// The principal planner has done this from the start. Sharing the shape
		// rather than only the pathname is the point: this tier is the DEFAULT
		// backend, so the weaker of the two rules was the one almost everyone got.
		for _, path := range capability.ProtectedWriteDenyPaths {
			// ANCHORED ONLY WHERE THE ANCHOR IS TRUE. Most of these are derived
			// from the root and sit under it, and holding those inside it is the
			// point. But ReadOnlySubpaths is a profile field an operator can set to
			// any path, and one deliberately placed outside the root is a
			// configuration that works today; anchoring it would turn that into a
			// containment refusal. A path that is not under the root gets no
			// anchor and keeps the final-component guard it always had.
			anchor := ""
			if pathWithinRoot(capability.Root, path) {
				anchor = capability.Root
			}
			entries = append(entries, WindowsACLEntry{
				Action:          WindowsACLDenyWrite,
				Path:            path,
				Capability:      capability.SID,
				Anchor:          anchor,
				Materialize:     true,
				MaterializeFile: gitMetadataCarveoutIsFile(path),
			})
		}
	}
	// Read roots, granted to the read-capability SID.
	//
	// A profile carrying DenyRead runs the command on a strict token rather than a
	// WRITE_RESTRICTED one, and the strict token applies the restricted-SID check
	// to reads. Read roots reached that check granted only to the principal's own
	// account SID, which the write jail keeps out of the restricting set on
	// purpose, so every read failed it — including opening the executable. The
	// principal plan still grants the account SID; this is the other half of the
	// same grant, so the allow-list and the restriction now come from one place.
	// AND IT REFUSES TO EXPRESS "READ EVERYWHERE" AS AN ACE ON THE VOLUME ROOT.
	//
	// Production profiles seed ReadRoots with the bare filesystem root, so this
	// loop used to add an inheritable allow-read for the synthetic read SID at
	// "C:\". SetSecurityInfo propagates inheritable ACEs onto existing children,
	// so that is not one sandbox-owned object being changed: it walks and rewrites
	// DACL inheritance across unrelated system, application and user trees on the
	// drive, and a locked or exclusively opened descendant makes the result depend
	// on ambient filesystem state.
	//
	// It is not even complete after paying that price. A bare root resolves on one
	// volume, while the executables, DLLs and tool installations a command needs
	// can sit on another without appearing as their own read roots, so the strict
	// token still fails before its executable starts. Measured: with the read SID
	// granted nowhere the strict token cannot open C:WindowsSystem32cmd.exe.
	//
	// So the profile that needs this is refused, at both setup tiers, rather than
	// half-served by a persistent volume-wide ACL edit. That leaves denyRead
	// unavailable on Windows until there is a bounded authorization model covering
	// the real platform, runtime and executable dependencies on every relevant
	// volume, which is what #869 tracks.
	readSID, err := windowsReadAllowCapabilitySID(config)
	if err != nil {
		return WindowsACLPlan{}, err
	}
	if readSID != "" {
		for _, path := range config.PermissionProfile.FileSystem.ReadRoots {
			if path = strings.TrimSpace(path); path == "" {
				continue
			}
			entries = append(entries, WindowsACLEntry{
				Action:     WindowsACLAllowRead,
				Path:       path,
				Capability: readSID,
			})
		}
	}
	writeSIDs := windowsWriteCapabilitySIDs(writeCapabilities)
	for _, path := range config.PermissionProfile.FileSystem.DenyWrite {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		for _, sid := range writeSIDs {
			entries = append(entries, WindowsACLEntry{
				Action:     WindowsACLDenyWrite,
				Path:       path,
				Capability: sid,
			})
		}
	}
	readDenySIDs, err := windowsReadDenyCapabilitySIDs(config, writeSIDs)
	if err != nil {
		return WindowsACLPlan{}, err
	}
	for _, path := range planWindowsDenyReadPaths(config.PermissionProfile.FileSystem.DenyRead) {
		for _, sid := range readDenySIDs {
			entries = append(entries, WindowsACLEntry{
				Action:      WindowsACLDenyRead,
				Path:        path,
				Capability:  sid,
				Materialize: true,
			})
		}
	}

	return WindowsACLPlan{Entries: dedupeWindowsACLEntries(entries)}, nil
}

// WindowsACLPlanReadGrantRefusal reports why a plan must not be applied, or "".
//
// ONE ANSWER FOR EVERY TIER THAT APPLIES A PLAN. This started inside the
// unelevated tier, because that is where it was first observed: an ordinary user
// cannot write the volume root's DACL, so every command failed there with a
// generic diagnosis. Elevated setup CAN write it, and that is worse rather than
// better. SetSecurityInfo propagates inheritable ACEs to existing children, so
// applying it rewrites DACL inheritance across unrelated trees, and it still does
// not cover a second volume, so the strict token can fail to open its own
// executable after all that.
//
// KEYED ON THE READ GRANT, NOT ON THE VOLUME ROOT. Checking for a volume root
// tested a symptom of the production profile rather than the thing that is
// unsafe. permissionProfileReadRoots happens to seed the bare filesystem root, so
// that check covered production by coincidence; a profile with a narrowed read
// list and a denyRead still put an inheritable ACE on C:Windows and passed. The
// grant itself is what cannot be applied safely to a directory Zero does not own,
// and the grant exists only for a denyRead profile, so that is what is refused.
func WindowsACLPlanReadGrantRefusal(plan WindowsACLPlan) string {
	root := windowsPlanReadGrantTarget(plan)
	if root == "" {
		return ""
	}
	return "this sandbox profile configures denyRead, which selects a fully restricted token, and that token applies its restricted-SID check to reads as well as writes. " +
		"Serving it needs the read capability granted on directories Zero does not own, starting at " + root + ", which cannot be done safely: the grant is inheritable, so applying it rewrites permissions across unrelated system, application and user directories, " +
		"and it still would not cover executables or libraries on another volume. " +
		"denyRead is therefore not available on Windows yet (see #869). Remove denyRead from the sandbox configuration to run on a write-restricted token, which keeps the workspace write jail intact"
}

// windowsPlanReadGrantTarget returns the broadest path the plan would grant the
// read capability on that Zero does not already own, preferring a volume root so
// the diagnostic names the worst of them.
//
// A read grant on a write root is Zero's own directory and is not the problem;
// the objection is to writing an inheritable ACE onto somebody else's tree. So
// the write roots are excluded, and an empty answer means every read grant lands
// on a directory this sandbox already governs, which needs no refusal.
func windowsPlanReadGrantTarget(plan WindowsACLPlan) string {
	owned := make(map[string]struct{})
	for _, entry := range plan.Entries {
		if entry.Action == WindowsACLAllowWrite {
			owned[strings.ToLower(normalizeProfilePath(entry.Path))] = struct{}{}
		}
	}
	first := ""
	for _, entry := range plan.Entries {
		if entry.Action != WindowsACLAllowRead {
			continue
		}
		normalized := normalizeProfilePath(entry.Path)
		if _, ours := owned[strings.ToLower(normalized)]; ours {
			continue
		}
		if isWindowsVolumeRoot(normalized) {
			return entry.Path
		}
		if first == "" {
			first = entry.Path
		}
	}
	return first
}

// windowsPlanVolumeRootGrant returns the first volume root the plan would have
// to change permissions on, or empty when it needs none.
//
// Only elevated setup can write a volume-root DACL, so this is the one entry
// that decides whether a plan is applicable by the unelevated tier at all. The
// check is on the PLAN rather than on the profile because the plan is what gets
// applied: a future entry that lands at a volume root for some other reason is
// caught by the same test.
func windowsPlanVolumeRootGrant(plan WindowsACLPlan) string {
	for _, entry := range plan.Entries {
		if isWindowsVolumeRoot(normalizeProfilePath(entry.Path)) {
			return entry.Path
		}
	}
	return ""
}

// windowsRenameProtectedObject names the object whose DELETE must be denied on
// a write root, whatever trustee holds the grant.
//
// ONE DERIVATION, BECAUSE TWO PLANNERS CONSUME IT. The allow-write mask both
// backends share includes DELETE, and it inherits from the write root onto .git.
// The carveouts that actually protect git live on .git/config and .git/hooks as
// OBJECTS, so renaming .git aside and recreating it discards them: the fresh
// config and hooks inherit the workspace allow with no deny of their own, which
// hands back credential.helper and core.hooksPath. The principal planner denied
// DELETE here and the capability planner did not, so the default restricted-token
// backend was missing the guard entirely. Deriving it in one place is what stops
// a change to the shared mask from updating one consumer and not the other.
func windowsRenameProtectedObject(root string) string {
	return filepath.Join(root, sandboxRenameProtectedMetadataName)
}

type windowsWriteRootCapability struct {
	Root                    string
	SID                     string
	ProtectedWriteDenyPaths []string
}

func windowsWriteRootCapabilities(config WindowsSandboxCommandConfig) ([]windowsWriteRootCapability, error) {
	out := make([]windowsWriteRootCapability, 0, len(config.PermissionProfile.FileSystem.WriteRoots))
	for _, root := range config.PermissionProfile.FileSystem.WriteRoots {
		rootPath := strings.TrimSpace(root.Root)
		if rootPath == "" {
			continue
		}
		sid, err := windowsCapabilitySIDForWriteRoot(config, rootPath)
		if err != nil {
			return nil, err
		}
		protected := make([]string, 0, len(root.ProtectedMetadataNames)+len(root.ReadOnlySubpaths))
		for _, subpath := range root.ReadOnlySubpaths {
			if trimmed := strings.TrimSpace(subpath); trimmed != "" {
				protected = append(protected, trimmed)
			}
		}
		for _, name := range root.ProtectedMetadataNames {
			if trimmed := strings.TrimSpace(name); trimmed != "" {
				protected = append(protected, filepath.Join(rootPath, trimmed))
			}
		}
		out = append(out, windowsWriteRootCapability{
			Root:                    rootPath,
			SID:                     sid,
			ProtectedWriteDenyPaths: protected,
		})
	}
	return out, nil
}

func windowsCapabilitySIDForWriteRoot(config WindowsSandboxCommandConfig, root string) (string, error) {
	if windowsRootMatchesWorkspace(root, config.WorkspaceRoots) {
		return WindowsWorkspaceCapabilitySID(config.SandboxHome, root)
	}
	return WindowsWritableRootCapabilitySID(config.SandboxHome, root)
}

func windowsWriteCapabilitySIDs(capabilities []windowsWriteRootCapability) []string {
	out := make([]string, 0, len(capabilities))
	seen := map[string]struct{}{}
	for _, capability := range capabilities {
		if capability.SID == "" {
			continue
		}
		if _, ok := seen[capability.SID]; ok {
			continue
		}
		seen[capability.SID] = struct{}{}
		out = append(out, capability.SID)
	}
	return out
}

func windowsReadDenyCapabilitySIDs(config WindowsSandboxCommandConfig, writeSIDs []string) ([]string, error) {
	if len(config.PermissionProfile.FileSystem.DenyRead) == 0 {
		return nil, nil
	}
	// The read-capability SID must be denied here as well as granted above. It
	// holds read on the read roots, and those start at the filesystem root, so a
	// carveout sitting inside one of them would stay readable through that grant
	// and the deny list would quietly stop meaning anything.
	// Every SID the token can carry has to be denied, not just the newest one.
	// With no write roots the token's only capability is ReadOnly, so dropping it
	// here would leave the deny naming a SID the command never holds.
	denySIDs := append([]string{}, writeSIDs...)
	if len(denySIDs) == 0 {
		caps, err := LoadOrCreateWindowsCapabilitySIDs(config.SandboxHome)
		if err != nil {
			return nil, err
		}
		denySIDs = append(denySIDs, caps.ReadOnly)
	}
	readSID, err := windowsReadAllowCapabilitySID(config)
	if err != nil {
		return nil, err
	}
	if readSID != "" {
		denySIDs = append(denySIDs, readSID)
	}
	return denySIDs, nil
}

func planWindowsDenyReadPaths(paths []string) []string {
	out := make([]string, 0, len(paths)*2)
	seen := map[string]struct{}{}
	push := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		key := windowsCapabilityPathKey(path)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, path)
	}
	for _, path := range paths {
		push(path)
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			push(resolved)
		}
	}
	return out
}

func dedupeWindowsACLEntries(entries []WindowsACLEntry) []WindowsACLEntry {
	out := make([]WindowsACLEntry, 0, len(entries))
	seen := map[string]struct{}{}
	for _, entry := range entries {
		if entry.Action == "" || strings.TrimSpace(entry.Path) == "" || strings.TrimSpace(entry.Capability) == "" {
			continue
		}
		// NoInherit is part of the identity: a direct-only deny and an
		// inheritable one on the same path/SID are different ACL shapes, and
		// collapsing them could silently promote a deliberately non-inherited
		// shared-path deny into an inheritable one (or vice versa).
		key := string(entry.Action) + "\x00" + windowsCapabilityPathKey(entry.Path) + "\x00" + strings.ToLower(entry.Capability) + "\x00" + fmt.Sprintf("%t", entry.NoInherit)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, entry)
	}
	return out
}

// windowsReadAllowCapabilitySID returns the read-capability SID when this profile
// will run on a strict token, and "" otherwise.
//
// Gated on DenyRead because that is exactly what selects the strict token in the
// runner: with no DenyRead the token stays WRITE_RESTRICTED, reads skip the
// restricted-SID check entirely, and granting read on roots as broad as the
// filesystem root would add ACEs that buy nothing. Derived purely from the
// profile, so the setup half and the command half reach the same answer without
// consulting anything ambient.
func windowsReadAllowCapabilitySID(config WindowsSandboxCommandConfig) (string, error) {
	if len(config.PermissionProfile.FileSystem.DenyRead) == 0 {
		return "", nil
	}
	return WindowsReadAllowSID(config.SandboxHome)
}
