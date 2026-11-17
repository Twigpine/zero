package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// errRuntimeComponentAliased marks a refusal that must NOT be treated as a
// reason to relocate.
//
// selectSandboxRuntimeRoot falls back to the temp root when the preferred one
// cannot be leased, which is right for an unusable directory and wrong for a
// hostile one: relocating would leave the attacker's link in place, say nothing,
// and simply move to the next predictable name for them to take as well. A
// machine in this state needs an operator, not a retry.
var errRuntimeComponentAliased = errors.New("sandbox runtime component is aliased")

// THE RUNTIME TREE IS CREATED IN A DIRECTORY OTHER PEOPLE CAN WRITE TO.
//
// The fallback root used to come from os.MkdirTemp, which mints a random name
// atomically at mode 0700, so no other local user could name the directory, let
// alone pre-create it. Deriving it from a digest of the workspace path bought
// stable cache reuse across runs and gave that name away: every component is
// computable by anyone who can guess the workspace path, and on Linux
// os.TempDir() is the shared, world-writable /tmp whenever TMPDIR is unset.
//
// What follows from a name another user can create is not subtle.
// os.MkdirAll returns nil when Stat says the path is already a directory, and
// Stat FOLLOWS LINKS, so a link planted at the leaf is silently accepted; the
// cache, data and tmp directories are then created inside whatever it points at,
// os.Chmod and os.Chtimes follow it too, and the root is handed to the platform
// backend as a WRITE ROOT (a read-write bind under bwrap) with TMPDIR, GOCACHE,
// GOMODCACHE and the package-manager caches all pointed inside it. The sandbox
// would be granting the confined command write access to a directory an
// attacker chose.
//
// Two things close it, and both are needed. The path carries a per-user
// component so ordinary users are not sharing one tree, and every component Zero
// owns is verified to be a real directory belonging to this user before anything
// is created through it. The name alone is not enough: /tmp is world-writable,
// so another user can create the per-user directory FIRST and wait.
//
// This is the same rule refuseReparsedRuntimeAncestors applies during elevated
// Windows setup. That guard was never on this path, which is the shared one
// every platform takes for every command.
func refuseAliasedRuntimeComponents(root string) error {
	for _, component := range ownedRuntimeComponents(root) {
		info, err := os.Lstat(component)
		if err != nil {
			if os.IsNotExist(err) {
				// Not there yet, so there is nothing to alias. The caller re-checks
				// after creation, because this alone is a check-then-use.
				continue
			}
			return fmt.Errorf("inspect sandbox runtime component %s: %w", component, err)
		}
		// ModeIrregular as well as ModeSymlink: a Windows junction is reported as
		// irregular, needs no privilege to create, and a guard written against
		// symlinks alone is inert against it.
		if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return fmt.Errorf("%w: refusing to use the sandbox runtime through a link at %s: "+
				"a link here redirects the directory the sandbox is granted write access to", errRuntimeComponentAliased, component)
		}
		if !info.IsDir() {
			// NOT tagged as aliased, deliberately. An ordinary file sitting where a
			// runtime component belongs is a broken machine rather than a hostile
			// one, and relocating to the other candidate is the sane recovery that
			// was already there. Only a link or a directory belonging to somebody
			// else says an attacker chose this path, and those are the two the
			// caller must refuse outright rather than route around.
			return fmt.Errorf("sandbox runtime component %s exists and is not a directory", component)
		}
		if err := refuseForeignRuntimeComponent(component, info); err != nil {
			return err
		}
	}
	return nil
}

// ownedRuntimeComponents lists the trailing components Zero creates, deepest
// first. Anything above them belongs to the user or the machine.
func ownedRuntimeComponents(root string) []string {
	cleaned := filepath.Clean(root)
	if cleaned == "" || cleaned == "." {
		return nil
	}
	components := make([]string, 0, windowsSandboxRuntimeOwnedDepth)
	current := cleaned
	for range windowsSandboxRuntimeOwnedDepth {
		components = append(components, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return components
}
