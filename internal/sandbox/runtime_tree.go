package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// runtimeTreeChildren are the directories prepared inside every runtime root,
// relative to it, each listed after its parent.
var runtimeTreeChildren = []string{
	"cache",
	"data",
	"tmp",
	filepath.Join("cache", "npm"),
	filepath.Join("cache", "yarn"),
	filepath.Join("cache", "corepack"),
	filepath.Join("cache", "pip"),
	filepath.Join("cache", "go-build"),
	filepath.Join("data", "go-mod"),
	filepath.Join("data", "cargo"),
}

// prepareSandboxRuntimeTree creates and secures the runtime root's own
// directories through a handle on the root, and refuses any that is a link.
//
// THE CHILDREN ARE THE SANDBOX'S TO CHANGE. The root is a write root: every
// backend grants or binds it read-write so a sandboxed command can use its TMP
// and caches, and that same grant lets the command remove cache, data or tmp
// and leave a link to a host directory in its place. The tree outlives the
// command, and the fallback root is derived rather than minted, so the NEXT Zero
// process prepares the same tree, unsandboxed. It used to MkdirAll and Chmod each
// child by pathname after checking only the root and its ancestors, which
// followed the planted link: a contained command could get Zero to create npm
// inside a host directory and change that directory's mode. Reported by @jatmn.
//
// So nothing below the root is reached by pathname. os.Root resolves every
// component relative to the root's handle and refuses one that leads out of it,
// which covers the parent of every grandchild too; a child that is a link at
// all is refused outright, because nothing but tampering puts one there; and
// the mode is set on the opened directory rather than on a name that could be
// swapped between the check and the change.
func prepareSandboxRuntimeTree(root string, now time.Time) error {
	tree, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("open sandbox runtime root %s: %w", root, err)
	}
	defer func() { _ = tree.Close() }()
	if err := secureRuntimeTreeDirectory(tree, ".", root); err != nil {
		return err
	}
	for _, child := range runtimeTreeChildren {
		path := filepath.Join(root, child)
		if err := tree.Mkdir(child, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("create sandbox runtime directory %s: %w", path, err)
		}
		info, err := tree.Lstat(child)
		if err != nil {
			return fmt.Errorf("inspect sandbox runtime directory %s: %w", path, err)
		}
		// ModeIrregular as well as ModeSymlink: a Windows junction reports as
		// irregular, and needs no privilege to create.
		if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return fmt.Errorf("%w: refusing to prepare the sandbox runtime through a link at %s, which a command run inside "+
				"the sandbox can leave there to redirect Zero's own writes; remove %s and Zero will recreate it",
				errRuntimeComponentAliased, path, root)
		}
		if !info.IsDir() {
			return fmt.Errorf("sandbox runtime directory %s exists and is not a directory; remove %s and Zero will recreate it", path, root)
		}
		if err := secureRuntimeTreeDirectory(tree, child, path); err != nil {
			return err
		}
	}
	if err := tree.Chtimes(".", now, now); err != nil {
		return fmt.Errorf("touch sandbox runtime root: %w", err)
	}
	return nil
}

// secureRuntimeTreeDirectory sets 0700 on the directory it opened, not on a
// name, so a swap after the check cannot move the change onto something else.
//
// Not on Windows, where a directory's access is its ACL, which setup manages,
// and chmod only toggles a read-only attribute Windows does not honour on
// directories. Setting it there would also need write-attribute access this
// handle does not have, for no effect.
func secureRuntimeTreeDirectory(tree *os.Root, name string, path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := tree.Open(name)
	if err != nil {
		return fmt.Errorf("open sandbox runtime directory %s: %w", path, err)
	}
	defer func() { _ = directory.Close() }()
	if err := directory.Chmod(0o700); err != nil {
		return fmt.Errorf("secure sandbox runtime directory %s: %w", path, err)
	}
	return nil
}
