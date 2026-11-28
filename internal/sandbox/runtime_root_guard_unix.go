//go:build !windows

package sandbox

import (
	"fmt"
	"os"
	"syscall"
)

// refuseForeignRuntimeComponent rejects a component owned by somebody else.
//
// The link check above stops the redirection; this stops the quieter half. /tmp
// is world-writable and sticky, so another local user can create the components
// Zero owns BEFORE Zero ever runs. A directory they own but Zero writes into is
// a place they can read the sandbox's caches out of, and the sticky bit does not
// help because they own it. os.MkdirAll accepts it silently, since the path
// already exists as a directory.
//
// Ownership rather than mode, because a 0777 directory belonging to this user is
// the user's own business while a 0700 directory belonging to another user is
// not something Zero should adopt.
func refuseForeignRuntimeComponent(component string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		// No ownership information on this filesystem. The link check already ran.
		return nil
	}
	if uid := os.Getuid(); uid >= 0 && int(stat.Uid) != uid {
		return fmt.Errorf("%w: refusing to use the sandbox runtime directory %s: it belongs to uid %d, not to this user (uid %d)",
			errRuntimeComponentAliased, component, stat.Uid, uid)
	}
	return nil
}

// sandboxRuntimeUserScope isolates the derived runtime tree per user.
//
// Two users on one host derive the same digest for the same workspace path, so
// without this they name one directory in shared temp and the first one there
// owns it. A uid is not a secret and is not meant to be: it removes the
// collision, and refuseAliasedRuntimeComponents handles the case where somebody
// got there first.
func sandboxRuntimeUserScope() string {
	return fmt.Sprintf("u%d", os.Getuid())
}

// sandboxRuntimeFallbackOwnedNames are the components the temp-derived runtime
// root is built from.
//
// THE USER BOUNDARY COMES FIRST, ABOVE EVERY PRIVATE COMPONENT. On Unix
// os.TempDir() is a SHARED directory whenever TMPDIR is unset, and runtime
// preparation creates and ownership-checks each of these components at 0700. A
// fixed first component therefore meant the first account to use the fallback
// created a private directory that every other account was then refused at:
// traversal fails on the mode, and relaxing the mode fails the ownership guard
// instead. The per-workspace digest is the leaf, so it never got the chance to
// separate them, and the fallback became first-user-wins on a shared host.
//
// Scoping the FIRST component keeps every ownership-checked ancestor inside a
// namespace that already belongs to one user, which is the property the guards
// below assume. The workspace digest stays the leaf so setup and the command
// still derive the same path for the same workspace.
func sandboxRuntimeFallbackOwnedNames() []string {
	names := append([]string(nil), windowsSandboxRuntimeOwnedNames...)
	names[0] = names[0] + "-" + sandboxRuntimeUserScope()
	return names
}
