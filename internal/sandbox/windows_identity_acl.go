package sandbox

// ACLs for a sandbox principal.
//
// The capability-SID model this sits beside starts from "the caller can already
// read everything" and narrows writes, because the sandboxed child runs as the
// caller. A principal inverts that: a separate local account has no access to
// the caller's profile at all, so the interesting direction is what to GRANT.
//
// That inversion is the point. Credential stores under the user's profile are
// unreachable because the principal is a different account, not because a deny
// rule enumerated them, which is what makes this able to close #662 and #675 on
// Windows where a deny-read ACE against the caller's own SID never could. Deny
// rules stay useful only for objects that are readable by everyone.
//
// Grants are explicit and narrow: the workspace and any extra write roots get
// read+write, declared read-only roots get read, and the protected metadata
// carve-outs the profile already defines stay denied so .git internals and
// .zero/.agents cannot be rewritten from inside the sandbox.

import (
	"errors"
	"fmt"
	"path/filepath"
)

// WindowsACLAllowRead grants read and execute without write. It exists for the
// principal model, where a read root must be granted rather than assumed.
const WindowsACLAllowRead WindowsACLAction = "allow-read"

// windowsPrincipalACLInput is everything needed to describe a principal's
// access. It is deliberately a plain struct rather than the full command config
// so the plan can be built and tested without a live sandbox.
type windowsPrincipalACLInput struct {
	// PrincipalSID is the string SID of the sandbox account every ACE names.
	PrincipalSID string
	// WriteRoots receive read+write+execute. The workspace lives here.
	WriteRoots []WritableRoot
	// ReadRoots receive read+execute only.
	ReadRoots []string
	// DenyRead covers objects a principal could otherwise reach because they are
	// world-readable; per-user secrets need no entry.
	DenyRead []string
	// DenyWrite carries the policy's own deny-write paths. The capability plan
	// has always emitted these; the principal plan denied write only on
	// protected metadata and read-only subpaths inside write roots, so a policy
	// deny sitting anywhere else was simply not enforced once the runner used a
	// principal token, and a shell child could write where the restricted-token
	// backend would have blocked it.
	DenyWrite []string
}

// buildWindowsPrincipalACLPlan turns a principal's access into ACL entries.
//
// Ordering matters at apply time: deny entries are emitted before allows so a
// carve-out inside a granted root survives, which mirrors how Windows evaluates
// an explicit DACL (deny ACEs first).
func buildWindowsPrincipalACLPlan(input windowsPrincipalACLInput) (WindowsACLPlan, error) {
	if input.PrincipalSID == "" {
		return WindowsACLPlan{}, errors.New("windows principal ACL plan requires a principal SID")
	}
	if len(input.WriteRoots) == 0 && len(input.ReadRoots) == 0 {
		return WindowsACLPlan{}, errors.New("windows principal ACL plan requires at least one root")
	}

	entries := make([]WindowsACLEntry, 0, len(input.WriteRoots)*2+len(input.ReadRoots)+len(input.DenyRead))

	// Deny first. A deny ACE inside a write root (protected metadata, git
	// internals) has to win over the grant that follows it.
	// Materialized, matching the capability plan. applyWindowsACLPlan skips a
	// target that does not exist, so without this a deny-read path created after
	// setup ran never got an ACE at all and the principal could read it. The
	// deny has to be in place before the object is.
	for _, path := range normalizeProfilePaths(input.DenyRead) {
		entries = append(entries, WindowsACLEntry{
			Action:      WindowsACLDenyRead,
			Path:        path,
			Capability:  input.PrincipalSID,
			Materialize: true,
		})
	}
	for _, path := range normalizeProfilePaths(input.DenyWrite) {
		entries = append(entries, WindowsACLEntry{
			Action:      WindowsACLDenyWrite,
			Path:        path,
			Capability:  input.PrincipalSID,
			Materialize: true,
		})
	}
	for _, root := range input.WriteRoots {
		// Normalized the same way as read and deny paths: a write root may arrive
		// with "~" or as a relative path, and an ACE has to name the same absolute,
		// symlink-resolved object the deny entries do or the two disagree.
		cleaned := normalizeProfilePath(root.Root)
		if cleaned == "" {
			return WindowsACLPlan{}, fmt.Errorf("windows principal ACL plan: unusable write root %q", root.Root)
		}
		// Materialized, like the metadata and policy denies below and above.
		//
		// These are the git control-plane carveouts (.git/config, .git/hooks). On a
		// workspace where git has not run yet they do not exist at setup time, and
		// applyWindowsACLPlan skips a target that is absent, so the ACEs were never
		// written. Once git created those paths the principal still held inherited
		// write access to the workspace and could install a hook or rewrite
		// credential.helper. The capability plan gets away without this because its
		// child runs as the caller; a separate principal account does not.
		// Which carveouts are files rather than directories comes from the same
		// spec list the profile built ReadOnlySubpaths from, so a new carveout
		// cannot be added without its shape coming along.
		for _, subpath := range normalizeProfilePaths(root.ReadOnlySubpaths) {
			entries = append(entries, WindowsACLEntry{
				Action:          WindowsACLDenyWrite,
				Path:            subpath,
				Capability:      input.PrincipalSID,
				Materialize:     true,
				MaterializeFile: gitMetadataCarveoutIsFile(subpath),
			})
		}
		for _, name := range root.ProtectedMetadataNames {
			// These are documented as NAMES, and the join below is what makes that
			// documentation load-bearing rather than descriptive: ".." or a
			// separator would place this deny ACE, and the directory it
			// materializes, outside the write root entirely. Today every caller
			// passes a package constant, so this is unreachable; it is here so that
			// stays true when a future caller sources these from config.
			if err := validateWindowsACLComponent(name); err != nil {
				return WindowsACLPlan{}, fmt.Errorf("windows principal ACL plan: protected metadata name: %w", err)
			}
			entries = append(entries, WindowsACLEntry{
				Action:      WindowsACLDenyWrite,
				Path:        filepath.Join(cleaned, name),
				Capability:  input.PrincipalSID,
				Materialize: true,
			})
		}
		// .git gets DELETE denied on the directory itself, because the carveouts
		// that protect it are attached to .git/config and .git/hooks as OBJECTS.
		// Rename .git aside and recreate it and those objects are gone, so the
		// fresh config and hooks inherit the workspace allow with no deny of their
		// own, handing back credential.helper and core.hooksPath.
		//
		// Not DenyWrite (git writes index, objects and refs), not materialized
		// (git creates .git, and an empty one breaks git init), and not inherited,
		// so everything underneath stays writable.
		entries = append(entries, WindowsACLEntry{
			Action:     WindowsACLDenyDelete,
			Path:       windowsRenameProtectedObject(cleaned),
			Capability: input.PrincipalSID,
		})
	}

	// Then the grants the principal cannot work without.
	for _, root := range input.WriteRoots {
		cleaned := normalizeProfilePath(root.Root)
		if cleaned == "" {
			continue
		}
		entries = append(entries, WindowsACLEntry{
			Action:     WindowsACLAllowWrite,
			Path:       cleaned,
			Capability: input.PrincipalSID,
		})
	}
	for _, path := range normalizeProfilePaths(input.ReadRoots) {
		// NEVER grant at a volume root.
		//
		// permissionProfileReadRoots seeds its list with profileRootPath(), which
		// is the separator alone, because the workspace-write posture is
		// read-all/write-jail. That is harmless for the capability backend, whose
		// child runs as the CALLER and therefore reads what the caller could read
		// anyway. It is not harmless here: a principal is a separate local
		// account, so this loop turns that synthetic entry into a real,
		// persistent, inheritable allow-read ACE for that account at the root of
		// the current drive, reaching every directory that does not block
		// inheritance.
		//
		// Dropping it does not take away the reads the principal needs to run
		// commands. NetUserAdd with USER_PRIV_USER puts the account in the
		// built-in Users group (see usrPrivUser in windows_identity_windows.go),
		// and the machine's own ACLs already grant Users read on the system and
		// program directories. What the grant added on top was read access to the
		// places Users are deliberately kept out of, which is the opposite of what
		// a sandbox is for.
		//
		// Note this inherits rather than asserts: nothing here checks that those
		// default ACLs are actually in place, so a hardened image that strips
		// Users read would need an explicit bounded read set instead.
		if isWindowsVolumeRoot(path) {
			continue
		}
		entries = append(entries, WindowsACLEntry{
			Action:     WindowsACLAllowRead,
			Path:       path,
			Capability: input.PrincipalSID,
		})
	}

	return WindowsACLPlan{Entries: dedupeWindowsACLEntries(entries)}, nil
}

// windowsPrincipalRevokePlan returns the entries whose ACEs should be removed
// when a principal is retired. Revocation is by TRUSTEE rather than by path:
// every ACE naming this principal is dropped, so cleanup does not depend on
// remembering which paths were granted, and a grant added by an older version
// is still removed.
//
// This is the removal path the capability-SID model never had, where a synthetic
// SID left ACEs behind on the user's tree with nothing to match them against.
func windowsPrincipalRevokePlan(principalSID string, paths []string) (WindowsACLPlan, error) {
	if principalSID == "" {
		return WindowsACLPlan{}, errors.New("windows principal revoke plan requires a principal SID")
	}
	cleaned := normalizeProfilePaths(paths)
	entries := make([]WindowsACLEntry, 0, len(cleaned))
	for _, path := range cleaned {
		entries = append(entries, WindowsACLEntry{
			Action:     windowsACLRevoke,
			Path:       path,
			Capability: principalSID,
		})
	}
	return WindowsACLPlan{Entries: entries}, nil
}

// windowsACLRevoke removes every ACE naming the trustee on a path, whatever
// access it granted or denied.
const windowsACLRevoke WindowsACLAction = "revoke"

// windowsACLPlanPaths lives in windows_identity_runtime_windows.go, beside its
// only callers. It was here, in the portable file, which made it dead code on
// every non-Windows build and failed the static analysis gate.
