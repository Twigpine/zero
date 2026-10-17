package daemon

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/sandbox"
)

func TestScrubWorkerEnvRemovesReentrancyMarkers(t *testing.T) {
	in := []string{
		"PATH=/bin",
		sandbox.EnvSandboxed + "=1",
		"ZERO_USER_SETTING=1",
		sandbox.EnvSandboxBackend + "=bubblewrap",
		"HOME=/home/u",
	}
	out := scrubWorkerEnv(in)

	for _, kv := range out {
		if strings.HasPrefix(kv, sandbox.EnvSandboxed+"=") || strings.HasPrefix(kv, sandbox.EnvSandboxBackend+"=") {
			t.Fatalf("re-entrancy marker not scrubbed: %q", kv)
		}
	}
	want := map[string]bool{"PATH=/bin": true, "ZERO_USER_SETTING=1": true, "HOME=/home/u": true}
	got := map[string]bool{}
	for _, kv := range out {
		got[kv] = true
	}
	for w := range want {
		if !got[w] {
			t.Fatalf("scrub dropped a non-marker var: %q", w)
		}
	}
	if len(out) != len(want) {
		t.Fatalf("scrub result = %v, want exactly %d preserved vars", out, len(want))
	}
	if len(in) != 5 {
		t.Fatal("scrubWorkerEnv must not mutate the caller's slice")
	}
}

// TestExecLauncherScrubsEnvEndToEnd proves the spawned WORKER PROCESS actually
// receives an environment without the re-entrancy markers — the security
// invariant. It uses /bin/sh to dump the worker env, so it is POSIX-only.
func TestExecLauncherScrubsEnvEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses the POSIX env dumper /bin/sh -c env")
	}
	launcher, err := NewExecLauncher(ExecLauncherConfig{
		Executable: "/bin/sh",
		BaseArgs:   []string{"-c", "env"},
		Env: append([]string{"KEEP=yes"},
			sandbox.EnvSandboxed+"=1",
			sandbox.EnvSandboxBackend+"=bubblewrap"),
	})
	if err != nil {
		t.Fatalf("NewExecLauncher: %v", err)
	}
	h, err := launcher(context.Background(), WorkerSpec{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	lines := h.Stdout()
	var got []string
	for {
		line, ok, lerr := lines.Next()
		if lerr != nil {
			t.Fatalf("read worker stdout: %v", lerr)
		}
		if !ok {
			break
		}
		got = append(got, line)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatalf("worker wait: %v", err)
	}

	keepSeen := false
	for _, l := range got {
		if strings.HasPrefix(l, sandbox.EnvSandboxed+"=") || strings.HasPrefix(l, sandbox.EnvSandboxBackend+"=") {
			t.Fatalf("worker process still has a re-entrancy marker in its env: %q", l)
		}
		if l == "KEEP=yes" {
			keepSeen = true
		}
	}
	if !keepSeen {
		t.Fatalf("worker env lost a non-marker var (KEEP=yes); got %v", got)
	}
}

func TestNewExecLauncherDefaults(t *testing.T) {
	// With no Executable it resolves the test binary's own path (os.Executable),
	// and constructs without error.
	if _, err := NewExecLauncher(ExecLauncherConfig{}); err != nil {
		t.Fatalf("NewExecLauncher with defaults: %v", err)
	}
}
