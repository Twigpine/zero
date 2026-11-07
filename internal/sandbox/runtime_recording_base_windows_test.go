//go:build windows

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// EXISTENCE MUST NOT CHOOSE THE TRUST BOUNDARY.
//
// createRuntimeDirRecording used to os.Stat its way to the deepest component
// that already existed and open THAT by name. On the ordinary second-workspace
// shape <base>\zero\runtime\v1 is already there and only the digest is missing,
// so the single by-name open was v1 — one of the predictable, user-owned
// components the rooted traversal exists to protect, not the cache directory
// above the tail.
//
// The race that opens: the pre-check accepts an ordinary v1, os.Stat selects it,
// the local owner replaces it with a junction, the by-name open follows it, and
// elevated setup creates the digest beneath the redirected target. Putting the
// original back makes the post-check see an ordinary pathname again, so the
// creation record names the redirected object under the original path and
// compensation leaves a privileged creation as residue.
//
// This drives the PRODUCTION entry point rather than the descent helper, because
// the helper is handed a base that has already been chosen — which is precisely
// the decision under test. A junction needs no privilege on Windows, so this
// runs unelevated.
func TestRuntimeRecordingOpensTheFixedBaseNotTheDeepestExistingComponent(t *testing.T) {
	cacheBase := t.TempDir()
	target := t.TempDir()

	// The shape a second workspace finds: everything above the digest already
	// exists, so the old walk would have picked v1 as its by-name base.
	v1 := filepath.Join(cacheBase, "zero", "runtime", "v1")
	if err := os.MkdirAll(v1, 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(v1, "deadbeefdeadbeef")

	// Sanity: the helper agrees the fixed base is the cache directory, not v1.
	base, components, owned := windowsSandboxRuntimeOwnedTail(root)
	if !owned || base != cacheBase {
		t.Fatalf("SETUP INVALID: owned tail resolved base=%q owned=%v, want %q", base, owned, cacheBase)
	}
	if len(components) == 0 {
		t.Fatal("SETUP INVALID: the owned tail has no components")
	}

	// THE PROPERTY, OBSERVED DIRECTLY. Whether a particular swap is caught depends
	// on where the barrier sits; which path is opened by name does not, and that is
	// the finding. Recorded here so the assertion cannot pass for an unrelated
	// reason.
	var openedByName []string
	previousOpen := runtimeBaseOpenedByName
	runtimeBaseOpenedByName = func(path string) { openedByName = append(openedByName, path) }
	t.Cleanup(func() { runtimeBaseOpenedByName = previousOpen })

	swapped := false
	previous := runtimeDescentBarrier
	runtimeDescentBarrier = func() {
		// Fires after the base has been opened and before the first owned
		// component is touched: exactly the interval the old walk was vulnerable
		// in. Replace v1 with a junction into a directory the test watches.
		if err := os.Remove(v1); err != nil {
			t.Fatalf("SETUP INVALID: could not clear v1 to plant the junction: %v", err)
		}
		out, err := exec.Command("cmd", "/c", "mklink", "/J", v1, target).CombinedOutput()
		if err != nil {
			t.Fatalf("SETUP INVALID: mklink /J: %v\n%s", err, out)
		}
		swapped = true
	}
	t.Cleanup(func() { runtimeDescentBarrier = previous })

	created, err := createRuntimeDirRecording(root)

	if !swapped {
		t.Fatal("SETUP INVALID: the barrier never ran, so no swap was attempted")
	}
	// Exactly one by-name open, and it is the cache directory above the tail.
	// Under the old walk this was v1, a component the local user controls.
	if len(openedByName) != 1 {
		t.Fatalf("the descent opened %d paths by name, want exactly 1: %v", len(openedByName), openedByName)
	}
	if !strings.EqualFold(openedByName[0], cacheBase) {
		t.Fatalf("opened %q by name, want the fixed base %q: existence must not choose the trust boundary", openedByName[0], cacheBase)
	}
	if err == nil {
		t.Fatalf("the recording walked through a junction at an owned component and reported success: created=%v", created)
	}
	// The redirected target must be untouched: no digest created beneath it.
	entries, readErr := os.ReadDir(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Errorf("elevated setup created %v beneath the redirected target", names)
	}
	// And nothing may be recorded as ours under a pathname that now names
	// somebody else's object, since that is what compensation acts on.
	for _, record := range created {
		if strings.HasPrefix(strings.ToLower(record.path), strings.ToLower(v1)) {
			t.Errorf("a redirected creation was recorded as ours: %+v", record)
		}
	}
}
