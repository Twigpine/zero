//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE AUGMENTATION HAS TO HAPPEN ON THE COMMAND PATH, NOT ONLY IN A HELPER.
//
// windows_setup_runtime_root_test.go proves the pieces compose: given a profile
// put through WindowsSandboxProfileWithRuntimeRoots on both sides, the marker
// validates. It calls that function directly, so it stays green even when the
// production call site in BuildCommandPlan is deleted, which is exactly the
// shape of the bug being fixed here. Reverting the runner call and watching that
// test still pass is how this gap was found.
//
// So this drives the real path and asserts on what the runner is actually handed.
// The config is serialized into the runner's argv, so the argv is where to look:
// recomputing the profile in the test would just be the helper test again.
func TestBuildCommandPlanCarriesTheRuntimeRootsIntoTheRunnerArgs(t *testing.T) {
	workspace := t.TempDir()
	cacheRoot := t.TempDir()
	original := sandboxUserCacheDir
	sandboxUserCacheDir = func() (string, error) { return cacheRoot, nil }
	t.Cleanup(func() { sandboxUserCacheDir = original })

	engine := NewEngine(EngineOptions{
		WorkspaceRoot: workspace,
		Policy:        DefaultPolicy(),
		Backend: Backend{
			Name:            BackendWindowsRestrictedToken,
			Available:       true,
			Platform:        "windows",
			Executable:      filepath.Join(t.TempDir(), WindowsSandboxCommandRunnerName),
			CommandWrapping: true,
			NativeIsolation: true,
		},
	})

	plan, err := engine.BuildCommandPlan(CommandSpec{
		Name: "cmd.exe",
		Args: WindowsShellArgs("echo hi"),
		Dir:  workspace,
	})
	if err != nil {
		t.Fatalf("BuildCommandPlan: %v", err)
	}
	defer plan.Cleanup()

	candidates := windowsSandboxRuntimeRoots(PermissionProfile{}, []string{workspace})
	if len(candidates) == 0 {
		t.Fatal("no runtime candidates derived, so this test would pass vacuously")
	}

	// The profile reaches the runner as JSON inside one of these arguments.
	argv := strings.Join(plan.Args, "\x00")
	for _, candidate := range candidates {
		// JSON escapes the backslashes in a Windows path, so compare in the same
		// spelling the encoder produced rather than the raw path.
		encoded := strings.ReplaceAll(candidate, `\`, `\\`)
		if !strings.Contains(argv, encoded) && !strings.Contains(argv, candidate) {
			t.Errorf("the runner argv does not carry runtime root %s; setup grants it, so the plans disagree and every command dies on \"permission roots or deny lists changed\"", candidate)
		}
	}
}

// GRANTING A ROOT IS NOT THE SAME AS PROVISIONING IT.
//
// The unelevated tier applies this plan itself, and applyWindowsACLPlan
// materializes only DenyRead targets: an AllowWrite target that does not exist
// aborts the run with "windows ACL target does not exist". prepareSandboxRuntime
// creates only the candidate the process SELECTS, so the other one has to be
// created explicitly, and it has to happen in the parent because the runner runs
// with TEMP redirected into the runtime tree and derives a different temp-side
// spelling.
func TestBuildCommandPlanProvisionsTheRuntimeRootsItGrants(t *testing.T) {
	workspace := t.TempDir()
	cacheRoot := t.TempDir()
	original := sandboxUserCacheDir
	sandboxUserCacheDir = func() (string, error) { return cacheRoot, nil }
	t.Cleanup(func() { sandboxUserCacheDir = original })

	tempRoot := t.TempDir()
	t.Setenv("TMP", tempRoot)
	t.Setenv("TEMP", tempRoot)

	candidates := windowsSandboxRuntimeRoots(PermissionProfile{}, []string{workspace})
	if len(candidates) == 0 {
		t.Fatal("no runtime candidates derived, so this test would pass vacuously")
	}
	for _, candidate := range candidates {
		if err := os.RemoveAll(candidate); err != nil {
			t.Fatalf("clear candidate %s: %v", candidate, err)
		}
	}

	engine := NewEngine(EngineOptions{
		WorkspaceRoot: workspace,
		Policy:        DefaultPolicy(),
		Backend: Backend{
			Name:            BackendWindowsRestrictedToken,
			Available:       true,
			Platform:        "windows",
			Executable:      filepath.Join(t.TempDir(), WindowsSandboxCommandRunnerName),
			CommandWrapping: true,
			NativeIsolation: true,
		},
	})

	plan, err := engine.BuildCommandPlan(CommandSpec{
		Name: "cmd.exe",
		Args: WindowsShellArgs("echo hi"),
		Dir:  workspace,
	})
	if err != nil {
		t.Fatalf("BuildCommandPlan: %v", err)
	}
	defer plan.Cleanup()

	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err != nil {
			t.Errorf("runtime root %s is granted by the plan but was not created: %v; the unelevated tier aborts on \"windows ACL target does not exist\"", candidate, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("runtime root %s exists but is not a directory", candidate)
		}
	}
}
