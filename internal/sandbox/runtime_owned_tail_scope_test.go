package sandbox

import (
	"path/filepath"
	"testing"
)

// THE SHAPE HAS TO HOLD FOR BOTH SPELLINGS OF THE FIRST COMPONENT.
//
// The cache-derived root uses the fixed name; the temp-derived fallback scopes
// that component to the user wherever the temp root is shared. A tail that
// stops matching loses the rooted no-follow traversal AND the owned-component
// guard, and neither failure is visible where it happens.
//
// The scoped spelling does not exist on Windows, so without the seam this
// branch could only ever be exercised by another platform's CI.
func TestOwnedTailAcceptsBothFirstComponentSpellings(t *testing.T) {
	previous := fallbackOwnedNamesForMatch
	t.Cleanup(func() { fallbackOwnedNamesForMatch = previous })
	fallbackOwnedNamesForMatch = func() []string {
		return []string{windowsSandboxRuntimeOwnedNames[0] + "-u1001", "runtime", "v1"}
	}

	base := filepath.Join("C:", "shared")
	for _, testCase := range []struct {
		name  string
		first string
		want  bool
	}{
		{"fixed cache spelling", windowsSandboxRuntimeOwnedNames[0], true},
		{"user-scoped fallback spelling", windowsSandboxRuntimeOwnedNames[0] + "-u1001", true},
		{"a different user's scope", windowsSandboxRuntimeOwnedNames[0] + "-u2002", false},
		{"an unrelated directory", "notzero", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := filepath.Join(base, testCase.first, "runtime", "v1", "abcdef0123456789")
			if _, _, ok := windowsSandboxRuntimeOwnedTail(root); ok != testCase.want {
				t.Errorf("owned tail for %s = %v, want %v", root, ok, testCase.want)
			}
		})
	}

	// The components below the first stay fixed for both spellings, or the
	// traversal would accept a tree Zero does not own.
	wrong := filepath.Join(base, windowsSandboxRuntimeOwnedNames[0]+"-u1001", "elsewhere", "v1", "abcdef0123456789")
	if _, _, ok := windowsSandboxRuntimeOwnedTail(wrong); ok {
		t.Errorf("owned tail accepted %s, whose middle component is not one Zero owns", wrong)
	}
}
