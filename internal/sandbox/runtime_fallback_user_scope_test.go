package sandbox

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// EVERY OWNERSHIP-CHECKED ANCESTOR MUST ALREADY BE INSIDE ONE USER'S NAMESPACE.
//
// The temp-derived fallback lives under a directory that is shared on Unix
// whenever TMPDIR is unset. Runtime preparation creates and ownership-checks
// each component of the tail at 0700, so a fixed first component meant the
// first account to use the fallback created a private directory every other
// account was then refused at: traversal fails on the mode, and relaxing the
// mode fails the ownership guard instead. The per-workspace digest is the leaf,
// so it never got the chance to separate them.
//
// The invariant is therefore about the SHALLOWEST checked component, not the
// leaf: it has to carry the user scope, or the guards below it are guarding a
// namespace two users share.
func TestFallbackRuntimeRootScopesItsShallowestOwnedComponent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.TempDir() resolves inside the user profile on Windows, so the shared-temp collision cannot arise")
	}
	workspace := t.TempDir()
	tempHome := t.TempDir()
	t.Setenv("TMPDIR", tempHome)

	root, err := fallbackSandboxRuntimeRoot(workspace)
	if err != nil {
		t.Fatalf("fallbackSandboxRuntimeRoot: %v", err)
	}
	components := ownedRuntimeComponents(root)
	if len(components) == 0 {
		t.Fatal("no ownership-checked components derived for the fallback root")
	}
	// ownedRuntimeComponents walks upward, so the last entry is the shallowest
	// directory the guards create and validate.
	shallowest := filepath.Base(components[len(components)-1])
	scope := sandboxRuntimeUserScope()
	if !strings.Contains(shallowest, scope) {
		t.Errorf("the shallowest ownership-checked component is %q and does not carry the user scope %q; "+
			"two accounts on a shared temp would contend for it", shallowest, scope)
	}
}

// The workspace still decides the leaf, so setup and the command derive the same
// root for the same workspace. Scoping the top must not have moved that.
func TestFallbackRuntimeRootStaysStableForOneWorkspace(t *testing.T) {
	workspace := t.TempDir()
	tempHome := t.TempDir()
	t.Setenv("TMPDIR", tempHome)
	t.Setenv("TMP", tempHome)
	t.Setenv("TEMP", tempHome)

	first, err := fallbackSandboxRuntimeRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	second, err := fallbackSandboxRuntimeRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("the fallback root is not stable for one workspace:\n  %s\n  %s", first, second)
	}

	other, err := fallbackSandboxRuntimeRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Error("two different workspaces derived the same fallback root")
	}
	if filepath.Dir(other) != filepath.Dir(first) {
		t.Errorf("two workspaces for one user should differ only in the leaf:\n  %s\n  %s", first, other)
	}
}

// And the Windows side is a deliberate choice, not an omission. Its temp root
// is already inside the user profile, and the names have to stay fixed so
// windowsSandboxRuntimeOwnedTail can still recognise a root that an elevated
// setup running as a different account built.
func TestFallbackOwnedNamesAreUnscopedOnWindowsOnly(t *testing.T) {
	names := sandboxRuntimeFallbackOwnedNames()
	if len(names) != len(windowsSandboxRuntimeOwnedNames) {
		t.Fatalf("fallback names = %v, want the same depth as %v", names, windowsSandboxRuntimeOwnedNames)
	}
	// The components below the first are shared by both platforms, so a change
	// to them would break the tail matcher on Windows.
	for index := 1; index < len(names); index++ {
		if names[index] != windowsSandboxRuntimeOwnedNames[index] {
			t.Errorf("component %d = %q, want %q", index, names[index], windowsSandboxRuntimeOwnedNames[index])
		}
	}
	scoped := names[0] != windowsSandboxRuntimeOwnedNames[0]
	if runtime.GOOS == "windows" && scoped {
		t.Errorf("the Windows fallback scoped its first component to %q; the tail matcher compares fixed names", names[0])
	}
	if runtime.GOOS != "windows" && !scoped {
		t.Errorf("the first component is %q on %s, where the temp root can be shared between accounts",
			names[0], runtime.GOOS)
	}
}
