package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A LINK AT AN OWNED COMPONENT MUST NOT BE BUILT THROUGH.
//
// This is the shared path every platform takes for every command, and until now
// it had no guard at all: the only link refusal in this package was wired into
// elevated Windows setup. os.MkdirAll returns nil when Stat says the path is
// already a directory, and Stat FOLLOWS links, so a link planted at an owned
// component is accepted silently. The cache, data and tmp directories are then
// created inside whatever it points at, Chmod and Chtimes follow it too, and the
// root is handed to the backend as a WRITE ROOT with TMPDIR, GOCACHE and the
// package-manager caches pointed inside it. The sandbox would be granting the
// confined command write access to a directory somebody else chose.
func TestTheRuntimeGuardRefusesALinkAtEveryOwnedComponent(t *testing.T) {
	for depth := range windowsSandboxRuntimeOwnedDepth {
		t.Run("replaced "+string(rune('0'+depth))+" levels above the leaf", func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(append([]string{base}, append(append([]string{}, windowsSandboxRuntimeOwnedNames...), "abcdef0123456789")...)...)
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatalf("create the runtime tree: %v", err)
			}

			swapped := root
			for range depth {
				swapped = filepath.Dir(swapped)
			}
			tail, err := filepath.Rel(swapped, root)
			if err != nil {
				t.Fatalf("relate the swapped component to the root: %v", err)
			}
			if err := os.RemoveAll(swapped); err != nil {
				t.Fatalf("clear the component to replace: %v", err)
			}
			target := filepath.Join(t.TempDir(), "somewhere-else")
			if err := os.MkdirAll(target, 0o700); err != nil {
				t.Fatalf("create the link target: %v", err)
			}
			linkRuntimeComponent(t, swapped, target)
			if tail != "." {
				if err := os.MkdirAll(filepath.Join(target, tail), 0o700); err != nil {
					t.Fatalf("recreate the components below the link: %v", err)
				}
			}

			err = refuseAliasedRuntimeComponents(root)
			if err == nil {
				t.Fatalf("a link at an owned component was accepted; the runtime tree would have been built inside %s and granted to the sandbox as a write root", target)
			}
			// ASSERTED ON THE SENTINEL, not on a word in the message.
			//
			// This read strings.Contains(err.Error(), "link") and was vacuous:
			// t.TempDir() names its directory after the test, the subtest was called
			// "link N levels above the leaf", and every component path therefore
			// contained "link". Deleting the reparse refusal left a different error
			// ("exists and is not a directory") whose PATH still satisfied the
			// assertion, so all four subtests passed with the fix removed.
			//
			// The sentinel is also the real contract: selectSandboxRuntimeRoot
			// branches on errors.Is to decide refuse-versus-relocate.
			if !errors.Is(err, errRuntimeComponentAliased) {
				t.Errorf("a link was not reported as a hostile alias, so selection would relocate around it instead of refusing: %v", err)
			}
			if !strings.Contains(err.Error(), "redirects the directory") {
				t.Errorf("the refusal does not name the reason: %v", err)
			}
		})
	}
}

// An ordinary tree passes, or the guard above would be satisfied by one that
// refuses everything.
func TestTheRuntimeGuardAcceptsAnOrdinaryTree(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(append([]string{base}, append(append([]string{}, windowsSandboxRuntimeOwnedNames...), "abcdef0123456789")...)...)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create the runtime tree: %v", err)
	}
	if err := refuseAliasedRuntimeComponents(root); err != nil {
		t.Fatalf("an ordinary runtime tree was refused: %v", err)
	}
}

// A tree that does not exist yet is fine: there is nothing to alias, and this is
// the ordinary first-run case.
func TestTheRuntimeGuardAcceptsATreeThatDoesNotExistYet(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(append([]string{base}, append(append([]string{}, windowsSandboxRuntimeOwnedNames...), "abcdef0123456789")...)...)
	if err := refuseAliasedRuntimeComponents(root); err != nil {
		t.Fatalf("an absent runtime tree was refused: %v", err)
	}
}

// A FILE where an owned component belongs is refused too, rather than producing
// a confusing failure deeper in.
func TestTheRuntimeGuardRefusesAFileWhereADirectoryBelongs(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(append([]string{base}, append(append([]string{}, windowsSandboxRuntimeOwnedNames...), "abcdef0123456789")...)...)
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		t.Fatalf("create the runtime parents: %v", err)
	}
	if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("seed the file: %v", err)
	}
	if err := refuseAliasedRuntimeComponents(root); err == nil {
		t.Fatal("a file standing where the runtime root belongs was accepted")
	}
}

// THE SHARED-TEMP FALLBACK IS SCOPED TO THIS USER.
//
// It replaced os.MkdirTemp, which minted a random 0700 directory atomically, so
// no other local user could name it. A digest of the workspace path alone is the
// same string for every account on the host, and on Linux os.TempDir() is the
// world-writable /tmp whenever TMPDIR is unset: two users on the same path would
// name one directory and the first one there would own it.
func TestTheTempFallbackRootIsScopedToTheUser(t *testing.T) {
	workspace := t.TempDir()
	root, err := fallbackSandboxRuntimeRoot(canonicalSandboxWorkspaceRoot(workspace))
	if err != nil {
		t.Fatalf("fallbackSandboxRuntimeRoot: %v", err)
	}
	scope := sandboxRuntimeUserScope()
	if strings.TrimSpace(scope) == "" {
		t.Fatal("the user scope is empty, so the digest is the same for every account")
	}

	// The leaf must move when the scope does. Recomputed the way the function
	// does, rather than asserting on a hardcoded digest.
	same := digestFor(workspace, scope)
	other := digestFor(workspace, scope+"-someone-else")
	if same == other {
		t.Fatal("the user scope does not reach the digest")
	}
	if !strings.HasSuffix(filepath.Clean(root), same) {
		t.Errorf("the fallback root %s does not end in the user-scoped digest %s", root, same)
	}
}

// And the fallback still has the shape the owned-component guard and the Windows
// rooted traversal both recognize. A path that stops matching silently loses
// BOTH protections, which is the expensive direction.
func TestTheTempFallbackRootKeepsTheOwnedShape(t *testing.T) {
	workspace := t.TempDir()
	root, err := fallbackSandboxRuntimeRoot(canonicalSandboxWorkspaceRoot(workspace))
	if err != nil {
		t.Fatalf("fallbackSandboxRuntimeRoot: %v", err)
	}
	if _, _, ok := windowsSandboxRuntimeOwnedTail(root); !ok {
		t.Fatalf("the fallback root %s is not recognized as an owned runtime tail, so the rooted traversal falls back to opening it by name", root)
	}
	components := ownedRuntimeComponents(root)
	if len(components) != windowsSandboxRuntimeOwnedDepth {
		t.Fatalf("the guard walks %d components of %s, want %d", len(components), root, windowsSandboxRuntimeOwnedDepth)
	}
}

// THROUGH prepareSandboxRuntime, not the helper.
//
// The helper being correct proves nothing on its own: the guard has to be on the
// path runner.go actually calls, before the MkdirAll that would build the tree
// through the link and before the root is handed back as a write root.
func TestPreparingTheRuntimeRefusesALinkedRoot(t *testing.T) {
	workspace := t.TempDir()
	cacheRoot := t.TempDir()
	original := sandboxUserCacheDir
	sandboxUserCacheDir = func() (string, error) { return cacheRoot, nil }
	t.Cleanup(func() { sandboxUserCacheDir = original })

	canonical := canonicalSandboxWorkspaceRoot(workspace)
	root, err := sandboxRuntimeRootFor(canonical, canonicalSandboxWorkspaceRoot(cacheRoot))
	if err != nil {
		t.Skipf("no cache-derived runtime root in this environment: %v", err)
	}

	// Somebody else got to the predictable name first and pointed it elsewhere.
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		t.Fatalf("create the runtime parents: %v", err)
	}
	target := filepath.Join(t.TempDir(), "somewhere-else")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatalf("create the link target: %v", err)
	}
	linkRuntimeComponent(t, root, target)

	runtimeState, cleanup, err := prepareSandboxRuntime(canonical, "")
	if cleanup != nil {
		cleanup()
	}
	if err == nil {
		t.Fatalf("the runtime was prepared at %s through a link; %s would have been bound read-write into the sandbox with TMPDIR and the build caches inside it", runtimeState.Root, target)
	}
	if !errors.Is(err, errRuntimeComponentAliased) {
		t.Errorf("the linked root was not reported as a hostile alias: %v", err)
	}
	for _, name := range []string{"cache", "data", "tmp"} {
		if _, statErr := os.Stat(filepath.Join(target, name)); statErr == nil {
			t.Errorf("the runtime tree was created inside the link target at %s", filepath.Join(target, name))
		}
	}
}
