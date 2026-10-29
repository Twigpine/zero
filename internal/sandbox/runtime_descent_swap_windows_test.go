//go:build windows

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// THE SWAP THE OLD WALK COULD NOT SEE.
//
// createRuntimeDirRecording used to find the deepest existing ancestor with
// os.Stat and then create each missing component by opening its parent BY
// NAME. Both follow a junction. So between the pre-check and the create, a
// local user could replace the predictable owned "zero" component with a
// junction into a directory of their choosing, and elevated setup would create
// runtime\v1\<hash> beneath that target, apply its ACL plan there, and then
// find an ordinary path at the post-check once the original was put back.
//
// This plants the junction at the deterministic barrier between opening the
// base and touching the first owned component, which is that exact interval,
// and proves two things: nothing is created beneath the redirected target, and
// the descent refuses rather than continuing through the link. A junction
// needs no privilege on Windows, which is why it is the shape used here rather
// than a symlink that would skip on an unelevated box.
func TestRuntimeDescentRefusesAJunctionSwappedIntoAnOwnedComponent(t *testing.T) {
	base := t.TempDir()
	target := t.TempDir()

	zero := filepath.Join(base, "zero")
	tail := []string{zero, filepath.Join(zero, "runtime")}

	previous := runtimeDescentBarrier
	runtimeDescentBarrier = func() {
		out, err := exec.Command("cmd", "/c", "mklink", "/J", zero, target).CombinedOutput()
		if err != nil {
			t.Fatalf("mklink /J: %v\n%s", err, out)
		}
	}
	t.Cleanup(func() { runtimeDescentBarrier = previous })

	created, err := createRuntimeTailHandleRelative(base, tail)
	if err == nil {
		t.Fatalf("the descent continued through a junction and reported success: created=%v", created)
	}
	if len(created) != 0 {
		t.Errorf("a junction-swapped component still produced ownership records: %v", created)
	}

	// The whole point: the redirected target must be untouched.
	entries, readErr := os.ReadDir(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Errorf("setup created %v beneath the junction target, which is somebody else's directory", names)
	}
}

// And the honest control: with no swap, the same descent creates the tail,
// records an identity for each component from the creation handle, and the
// result matches what the name resolves to afterwards.
func TestRuntimeDescentCreatesTheOwnedTailFromHandles(t *testing.T) {
	base := t.TempDir()
	zero := filepath.Join(base, "zero")
	tail := []string{zero, filepath.Join(zero, "runtime"), filepath.Join(zero, "runtime", "v1")}

	created, err := createRuntimeTailHandleRelative(base, tail)
	if err != nil {
		t.Fatalf("descent: %v", err)
	}
	if len(created) != len(tail) {
		t.Fatalf("created %d components, want %d: %v", len(created), len(tail), created)
	}
	for i, record := range created {
		if record.path != tail[i] {
			t.Errorf("record %d path = %q, want %q", i, record.path, tail[i])
		}
		if !record.identified {
			t.Errorf("record %d for %s carries no identity", i, record.path)
			continue
		}
		if now, ok := runtimeDirIdentity(record.path); !ok || now != record.identity {
			t.Errorf("record %d identity %q does not describe the directory at %s (%q)", i, record.identity, record.path, now)
		}
	}
}
