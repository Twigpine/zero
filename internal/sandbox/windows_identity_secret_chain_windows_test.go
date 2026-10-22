//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// AND THE ANCESTOR IS PART OF THE PATH TOO.
//
// The sibling test puts the junction at the directory the secret is addressed
// into, which the no-follow open of that directory refuses. This one puts it
// ABOVE a component that does not exist yet, which is the case the refusal never
// saw: os.MkdirAll walked the pathname, followed the junction, and created the
// missing component on the other side of it as Administrator. The no-follow open
// that came next then found a perfectly ordinary directory at the requested
// pathname, because the caller-controlled redirection had already happened one
// level up, and the elevated write landed outside the sandbox home.
//
// A junction rather than a symlink for the same reason as the sibling test: it
// needs no privilege, so it is reachable by exactly the unprivileged user this
// guards against.
func TestWriteWindowsSandboxSecretRefusesAJunctionedAncestor(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	// home is the junction. The secret is addressed two levels below it, and the
	// level in between does not exist, so it has to be created.
	home := filepath.Join(base, "home")
	makeJunction(t, home, outside)

	path := filepath.Join(home, "identity", "zero-sbx-test.secret")
	err := writeWindowsSandboxSecret(path, "Zs1!EXAMPLEPASSWORDVALUE")
	if err == nil {
		t.Error("wrote the principal secret below a junctioned ancestor, so an elevated setup placed it in a tree the caller controls")
	}

	// The refusal is only half of it: nothing may have been created on the other
	// side of the junction, because an elevated create there is a write primitive
	// at a path the caller chose even when no secret follows it.
	entries, readErr := os.ReadDir(outside)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		names := []string{}
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Errorf("the junction target gained %v, so an elevated create followed the ancestor", names)
	}
}

// The same shape for the runtime root, which is the other elevated pathname
// create in this setup path. Its candidates sit under the invoking user's own
// cache directory, which is to say under a tree the party this sandbox contains
// can rearrange before elevated setup runs.
func TestSetupWindowsSandboxRuntimeRootRefusesAJunctionedAncestor(t *testing.T) {
	isolateSandboxRuntimeRoots(t)
	cache := t.TempDir()
	t.Setenv("LOCALAPPDATA", cache)
	t.Setenv("XDG_CACHE_HOME", cache)
	workspace := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	config := WindowsSandboxCommandConfig{WorkspaceRoots: []string{workspace}}

	candidates := windowsSandboxRuntimeCandidates(config.WorkspaceRoots)
	if len(candidates) == 0 {
		t.Fatal("SETUP INVALID: no runtime root candidate for a configured workspace")
	}
	// PICKED BY EXCLUDING THE OTHER ONE, NOT BY PATH PREFIX. The second candidate
	// is the shared fallback under the system temp directory, which this test
	// must not junction. Selecting the cache-derived one by prefix looks
	// equivalent and is not: a CI runner spells one directory two ways
	// (RUNNER~1 against runneradmin), so os.UserCacheDir and t.TempDir disagree
	// on a path they both mean and the prefix matches nothing.
	fallback, _ := fallbackSandboxRuntimeRoot(workspace)
	cacheCandidate := ""
	for _, candidate := range candidates {
		if candidate != fallback {
			cacheCandidate = candidate
			break
		}
	}
	if cacheCandidate == "" {
		t.Fatalf("SETUP INVALID: no cache-derived candidate among %v (fallback %q)", candidates, fallback)
	}

	// The swap: the candidate's parent is a junction out of the cache tree, and
	// the candidate itself does not exist, so it has to be created.
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(cacheCandidate)
	if err := os.MkdirAll(filepath.Dir(parent), 0o700); err != nil {
		t.Fatal(err)
	}
	makeJunction(t, parent, outside)

	if _, err := setupWindowsSandboxRuntimeRoot(config); err == nil {
		t.Error("created the runtime root below a junctioned ancestor, as an elevated process, in a tree the caller controls")
	}

	entries, readErr := os.ReadDir(outside)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		names := []string{}
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Errorf("the junction target gained %v, so an elevated create followed the ancestor", names)
	}
}

// And an ordinary nested directory is still created, or the refusals above
// prove only that these functions fail.
func TestWriteWindowsSandboxSecretCreatesAMissingChain(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "home", "identity", "nested", "zero-sbx-test.secret")
	const password = "Zs1!EXAMPLEPASSWORDVALUE"
	if err := writeWindowsSandboxSecret(path, password); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := readWindowsSandboxSecret(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != password {
		t.Fatalf("read %q, want the stored password", got)
	}
}
