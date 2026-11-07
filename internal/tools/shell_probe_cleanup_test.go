package tools

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/sandbox"
)

// openFilesUnder counts this process's open file descriptors that point below
// dir. Linux only: it reads /proc/self/fd.
func openFilesUnder(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("cannot list open files: %v", err)
	}
	count := 0
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if err == nil && strings.HasPrefix(target, dir+string(filepath.Separator)) {
			count++
		}
	}
	return count
}

// THE SHELL PROBE RELEASES WHAT ITS PLAN HOLDS. Building a sandboxed command
// plan takes a shared lease (an open, locked file) on the workspace's sandbox
// runtime directory; only the plan's Cleanup releases it. The probe discarded
// the plan, so every probe leaked that lease for the life of the process.
//
// Driven on Linux, where the lease is observable as an open descriptor; the
// probe itself is platform-neutral, only its caller is Windows-gated.
func TestTheSandboxedShellProbeReleasesItsPlan(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("observes the lease through /proc/self/fd")
	}
	shell, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	root := t.TempDir()
	engine := sandbox.NewEngine(sandbox.EngineOptions{
		WorkspaceRoot: root,
		Policy:        sandboxedBashPolicy(),
	})

	before := openFilesUnder(t, cache)
	// The probe's arguments mean nothing to the test binary; whether it "starts"
	// is not the point — only what the probe leaves open afterwards.
	_ = shellStartsUnderEngine(engine, shell, root)
	if after := openFilesUnder(t, cache); after != before {
		t.Fatalf("the probe left %d file(s) open under the sandbox runtime cache — its plan's lease was never released", after-before)
	}
	if entries, _ := os.ReadDir(filepath.Join(cache, "zero", "runtime", "v1")); len(entries) == 0 {
		t.Fatal("SETUP INVALID: the probe prepared no sandbox runtime, so there was no lease to leak")
	}
}
