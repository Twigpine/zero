package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// AN ELEVATED ACL MUST NOT BE WRITTEN THROUGH A LINK.
//
// The runtime root is a predictable path under the user cache, and every
// component below the cache root is created by us. A local user needs no
// privilege to create a junction, so they can plant one at "zero", "runtime" or
// "v1" and have provisioning follow it: the leaf is created in their tree
// instead of ours, openWindowsACLTarget opens that leaf, sees no reparse point
// ON THE LEAF, and elevated setup grants the sandbox capability write access to
// a directory the attacker controls.
//
// The variant that matters is the one where the attacker ALSO creates the
// components below the junction. A check that looks only at the deepest existing
// component then finds an ordinary directory and passes.
func TestProvisioningRefusesAReparsePointAtAnOwnedAncestor(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("junctions are a Windows construct; the guard is only reachable there")
	}

	// The owned tail, exactly as deterministicSandboxRuntimeRoot joins it.
	for _, ancestor := range []string{"zero", filepath.Join("zero", "runtime"), filepath.Join("zero", "runtime", "v1")} {
		t.Run(ancestor, func(t *testing.T) {
			base := t.TempDir()
			cache := filepath.Join(base, "cache")
			decoy := filepath.Join(base, "attacker-owned")
			root := filepath.Join(cache, "zero", "runtime", "v1", "abc123def456")

			link := filepath.Join(cache, ancestor)
			if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(decoy, 0o700); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command("cmd", "/c", "mklink", "/J", link, decoy).CombinedOutput()
			if err != nil {
				t.Skipf("cannot create a junction here: %v %s", err, out)
			}

			// The attacker fills in everything below the junction, so the deepest
			// EXISTING component is an ordinary directory and a check that looks only
			// there is satisfied.
			below := strings.TrimPrefix(strings.TrimPrefix(filepath.Dir(root), filepath.Join(cache, ancestor)), string(filepath.Separator))
			if below != "" {
				if err := os.MkdirAll(filepath.Join(decoy, below), 0o700); err != nil {
					t.Fatal(err)
				}
			}

			created, err := createRuntimeDirRecording(root)
			if err == nil {
				physical := physicalSandboxPath(root)
				t.Fatalf("provisioning followed a junction at %s and created %v (physically %s); an elevated ACL applied to that leaf lands on a directory the attacker controls", ancestor, created, physical)
			}
			if !strings.Contains(err.Error(), "link") {
				t.Errorf("the refusal does not explain that a link was in the way: %v", err)
			}
		})
	}
}

// The ordinary case must still provision. A guard that refuses everything would
// satisfy the test above and break every real machine.
func TestProvisioningStillCreatesAnOrdinaryRuntimeRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache", "zero", "runtime", "v1", "abc123def456")
	created, err := createRuntimeDirRecording(root)
	if err != nil {
		t.Fatalf("createRuntimeDirRecording on a clean tree: %v", err)
	}
	if len(created) == 0 {
		t.Fatal("nothing was recorded as created")
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		t.Fatalf("the runtime root was not created: %v", err)
	}
}

// The CACHE ROOT above the owned components is the user's, and a redirected
// LOCALAPPDATA legitimately makes it a reparse point. Refusing there would break
// ordinary machines, so the guard must stop at the components Zero creates.
func TestProvisioningAllowsAReparsePointAboveTheOwnedComponents(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("junctions are a Windows construct")
	}
	base := t.TempDir()
	real := filepath.Join(base, "real-cache")
	link := filepath.Join(base, "redirected-cache")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, real).CombinedOutput(); err != nil {
		t.Skipf("cannot create a junction here: %v %s", err, out)
	}

	root := filepath.Join(link, "zero", "runtime", "v1", "abc123def456")
	if _, err := createRuntimeDirRecording(root); err != nil {
		t.Errorf("provisioning refused a redirected cache root, which is an ordinary Windows configuration: %v", err)
	}
}
