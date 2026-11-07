package sandbox

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ensureRuntimeTreeDirs creates every directory of the runtime tree without ever
// following a link below the operator-owned base.
//
// THE TREE IS PERSISTENT INPUT FROM THE PREVIOUS SANDBOXED COMMAND.
//
// The runtime root is deterministic so setup and later runners agree on one
// path, and the sandboxed command is granted write access to cache, data and
// tmp. So the command can replace one of them with a symlink or a Windows
// junction on its way out. The next preparation ran os.MkdirAll and os.Chmod on
// raw pathnames, which follow, and the HOST Zero process, the ordinary user
// rather than the confined principal, then created package-cache directories
// inside a target the previous command chose.
//
// The anchor check is not enough on its own: it proves the per-user anchor and
// says nothing about the reusable root or any descendant beneath it.
//
// NOT peermsg.EnsurePrivateDir. That ends in a protected-DACL write, which on
// Windows strips the sandbox principal's grant that elevated setup installed on
// the runtime tree and brings back the bare access denials from npm and go that
// the grant exists to prevent. This descent validates and creates; it does not
// re-secure.
func ensureRuntimeTreeDirs(root string, directories []string) error {
	// Canonicalized the same way the rest of the runtime state is, because
	// runtimeCandidateBase decides ownership with a containment test and that
	// test runs on spellings. An 8.3 short name (C:UsersRUNNER~1) or a
	// symlinked temp compares unequal to the long form of the same directory,
	// which reads as "no operator-owned base" for a perfectly ordinary tree.
	if canonical := canonicalSandboxWorkspaceRoot(root); canonical != "" {
		root = canonical
	}
	base, ok := runtimeCandidateBase(root)
	if !ok || strings.TrimSpace(base) == "" {
		return fmt.Errorf("sandbox runtime root %s has no operator-owned base to descend from", root)
	}
	// Physical, so a redirected cache or TEMP above the tree stays the operator's
	// business while everything Zero owns below it is addressed no-follow.
	physical, err := physicalDir(base)
	if err != nil {
		return fmt.Errorf("resolve the sandbox runtime base %s: %w", base, err)
	}
	for _, directory := range directories {
		tail, err := runtimeTreeTail(physical, base, directory)
		if err != nil {
			return err
		}
		if err := ensureRuntimeTreeDir(physical, tail); err != nil {
			return err
		}
	}
	return nil
}

// runtimeTreeTail returns the components of directory below the base, which are
// the ones Zero owns and the ones that must not be links.
func runtimeTreeTail(physicalBase string, base string, directory string) ([]string, error) {
	cleaned := filepath.Clean(strings.TrimSpace(directory))
	relative, err := filepath.Rel(base, cleaned)
	if err != nil || relative == "." || strings.HasPrefix(relative, "..") {
		// Try the physical spelling too: the caller's path may already be physical.
		relative, err = filepath.Rel(physicalBase, cleaned)
		if err != nil || relative == "." || strings.HasPrefix(relative, "..") {
			return nil, fmt.Errorf("sandbox runtime directory %s does not sit under its base %s", directory, base)
		}
	}
	parts := strings.Split(relative, string(filepath.Separator))
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			return nil, fmt.Errorf("sandbox runtime directory %s escapes its base %s", directory, base)
		}
		out = append(out, part)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("sandbox runtime directory %s resolves to its own base", directory)
	}
	return out, nil
}
