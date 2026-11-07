package sandbox

import (
	"errors"
	"path/filepath"
	"strings"
)

// windowsSandboxRuntimeOwnedNames are the fixed components Zero joins under the
// cache or temp root, above the per-workspace digest.
//
// ONE INVENTORY. deterministicSandboxRuntimeRoot builds a root from these and
// the traversal below recognizes one by them, and the two have to stay the same
// list or the traversal silently stops treating a real runtime root as owned:
// it would fall back to opening by name, which is exactly the unprotected path
// this file exists to replace. A wrong answer here fails open, so the two uses
// read from the same place.
var windowsSandboxRuntimeOwnedNames = []string{"zero", "runtime", "v1"}

// windowsSandboxRuntimeOwnedDepth is how many trailing components of a runtime
// root Zero creates and therefore owns: the fixed names plus the digest.
var windowsSandboxRuntimeOwnedDepth = len(windowsSandboxRuntimeOwnedNames) + 1

// windowsSandboxRuntimeOwnedTail splits a runtime root into the ancestor that
// belongs to the user and the components Zero created.
//
// The base is deliberately not our business. On a machine with a redirected
// LOCALAPPDATA it is legitimately a reparse point, and refusing there would
// break ordinary setups. Everything below it was created by us and has no
// business being a link.
//
// ok is false when the path does not have the shape a runtime root has, which
// means the caller must not treat it as owned.
func windowsSandboxRuntimeOwnedTail(root string) (string, []string, bool) {
	cleaned := filepath.Clean(strings.TrimSpace(root))
	if cleaned == "" || cleaned == "." {
		return "", nil, false
	}
	components := make([]string, 0, windowsSandboxRuntimeOwnedDepth)
	current := cleaned
	for range windowsSandboxRuntimeOwnedDepth {
		parent := filepath.Dir(current)
		if parent == current {
			return "", nil, false
		}
		components = append(components, filepath.Base(current))
		current = parent
	}
	// components came off the tail, deepest first.
	for index := range windowsSandboxRuntimeOwnedNames {
		if !windowsRuntimeOwnedNameMatches(index, components[len(components)-1-index]) {
			return "", nil, false
		}
	}
	ordered := make([]string, 0, len(components))
	for index := len(components) - 1; index >= 0; index-- {
		ordered = append(ordered, components[index])
	}
	return current, ordered, true
}

// errRuntimeTailNotOwned reports a path the rooted traversal will not handle.
// Callers must fail rather than quietly opening it by name.
var errRuntimeTailNotOwned = errors.New("path is not a sandbox runtime root")

// windowsRuntimeOwnedNameMatches reports whether one component of a candidate
// tail is a name Zero owns at that position.
//
// The FIRST position has two accepted spellings. The cache-derived root uses the
// fixed name; the temp-derived fallback scopes that component to the user on
// platforms where the temp root is shared between accounts, because every
// private ownership-checked ancestor has to sit inside one user's namespace.
// Both spellings are Zero's own and everything below them is fixed.
//
// Accepting only the fixed name silently cost the fallback BOTH protections it
// depends on: the rooted no-follow traversal fell back to opening the tree by
// name, and the shape guard stopped recognising it. Neither failure is visible
// at the point it happens, which is why the shape has a test of its own.
func windowsRuntimeOwnedNameMatches(index int, component string) bool {
	if strings.EqualFold(component, windowsSandboxRuntimeOwnedNames[index]) {
		return true
	}
	if index != 0 {
		return false
	}
	return strings.EqualFold(component, fallbackOwnedNamesForMatch()[0])
}

// fallbackOwnedNamesForMatch is the seam the shape tests use. The scoped
// spelling only exists on platforms whose temp root is shared, so without it the
// accepting branch cannot be exercised at all on Windows, and a break there
// would only surface on another platform's CI.
var fallbackOwnedNamesForMatch = sandboxRuntimeFallbackOwnedNames
