package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrepareSandboxRuntimeStaysOutsideWorkspace(t *testing.T) {
	isolateSandboxRuntimeRoots(t)
	workspace := t.TempDir()
	cacheRoot := t.TempDir()
	original := sandboxUserCacheDir
	sandboxUserCacheDir = func() (string, error) { return cacheRoot, nil }
	t.Cleanup(func() { sandboxUserCacheDir = original })

	runtimeState, release, err := prepareSandboxRuntime(workspace)
	if err != nil {
		t.Fatalf("prepareSandboxRuntime: %v", err)
	}
	defer release()
	if pathWithinRoot(workspace, runtimeState.Root) {
		t.Fatalf("runtime root %q must stay outside workspace %q", runtimeState.Root, workspace)
	}
	for _, path := range []string{runtimeState.Cache, runtimeState.Data, runtimeState.Temp, filepath.Join(runtimeState.Data, "cargo")} {
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			t.Fatalf("managed runtime directory %q was not prepared: %v", path, err)
		}
	}
	for _, name := range []string{"home", "config", "state"} {
		path := filepath.Join(runtimeState.Root, name)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("obsolete synthetic runtime directory %q exists: %v", path, err)
		}
	}
}

func TestPrepareSandboxRuntimeCleansExpiredSibling(t *testing.T) {
	isolateSandboxRuntimeRoots(t)
	workspace := t.TempDir()
	cacheRoot := t.TempDir()
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	originalCache := sandboxUserCacheDir
	originalNow := sandboxRuntimeNow
	sandboxUserCacheDir = func() (string, error) { return cacheRoot, nil }
	sandboxRuntimeNow = func() time.Time { return now }
	t.Cleanup(func() {
		sandboxUserCacheDir = originalCache
		sandboxRuntimeNow = originalNow
	})
	parent := filepath.Join(cacheRoot, "zero", "runtime", "v1")
	expired := filepath.Join(parent, "expired")
	if err := os.MkdirAll(expired, 0o700); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-sandboxRuntimeMaxAge - time.Hour)
	if err := os.Chtimes(expired, old, old); err != nil {
		t.Fatal(err)
	}
	_, release, err := prepareSandboxRuntime(workspace)
	if err != nil {
		t.Fatalf("prepareSandboxRuntime: %v", err)
	}
	release()
	if _, err := os.Stat(expired); !os.IsNotExist(err) {
		t.Fatalf("expired runtime still exists: %v", err)
	}
}

func TestPrepareSandboxRuntimeFallsBackWhenUserCacheIsInsideWorkspace(t *testing.T) {
	isolateSandboxRuntimeRoots(t)
	workspace := t.TempDir()
	original := sandboxUserCacheDir
	sandboxUserCacheDir = func() (string, error) { return filepath.Join(workspace, ".cache"), nil }
	t.Cleanup(func() { sandboxUserCacheDir = original })

	runtimeState, release, err := prepareSandboxRuntime(workspace)
	if err != nil {
		t.Fatalf("prepareSandboxRuntime: %v", err)
	}
	defer release()
	if pathWithinRoot(workspace, runtimeState.Root) {
		t.Fatalf("fallback runtime root %q must stay outside workspace %q", runtimeState.Root, workspace)
	}
	if filepath.Clean(filepath.Dir(runtimeState.Root)) == filepath.Clean(os.TempDir()) {
		t.Fatalf("fallback runtime %q must use a private cleanup parent", runtimeState.Root)
	}
}

func TestCleanupSandboxRuntimeSkipsActiveLease(t *testing.T) {
	isolateSandboxRuntimeRoots(t)
	workspace := t.TempDir()
	cacheRoot := t.TempDir()
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	originalCache := sandboxUserCacheDir
	originalNow := sandboxRuntimeNow
	sandboxUserCacheDir = func() (string, error) { return cacheRoot, nil }
	sandboxRuntimeNow = func() time.Time { return now }
	t.Cleanup(func() {
		sandboxUserCacheDir = originalCache
		sandboxRuntimeNow = originalNow
	})

	runtimeState, release, err := prepareSandboxRuntime(workspace)
	if err != nil {
		t.Fatalf("prepareSandboxRuntime: %v", err)
	}
	old := now.Add(-sandboxRuntimeMaxAge - time.Hour)
	if err := os.Chtimes(runtimeState.Root, old, old); err != nil {
		release()
		t.Fatal(err)
	}
	parent := filepath.Dir(runtimeState.Root)
	cleanupSandboxRuntimeRoots(parent, filepath.Join(parent, "other"), now)
	if _, err := os.Stat(runtimeState.Root); err != nil {
		release()
		t.Fatalf("active runtime was removed: %v", err)
	}

	release()
	cleanupSandboxRuntimeRoots(parent, filepath.Join(parent, "other"), now)
	if _, err := os.Stat(runtimeState.Root); !os.IsNotExist(err) {
		t.Fatalf("released expired runtime still exists: %v", err)
	}
}

func TestSandboxRuntimeEnvironmentPreservesUserConfiguration(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtime")
	runtimeState := SandboxRuntime{
		Root:  root,
		Cache: filepath.Join(root, "cache"),
		Data:  filepath.Join(root, "data"),
		Temp:  filepath.Join(root, "tmp"),
	}
	env := sandboxRuntimeEnvironment([]string{
		"HOME=/home/user",
		"XDG_CACHE_HOME=/host/cache",
		"XDG_CONFIG_HOME=/home/user/.config",
		"XDG_DATA_HOME=/home/user/.local/share",
		"XDG_STATE_HOME=/home/user/.local/state",
		"NPM_CONFIG_USERCONFIG=/home/user/.npmrc",
		"PATH=/usr/bin",
	}, &runtimeState)

	for key, want := range map[string]string{
		"HOME":                  "/home/user",
		"XDG_CONFIG_HOME":       "/home/user/.config",
		"XDG_DATA_HOME":         "/home/user/.local/share",
		"XDG_STATE_HOME":        "/home/user/.local/state",
		"NPM_CONFIG_USERCONFIG": "/home/user/.npmrc",
	} {
		if got := envListValue(env, key, ""); got != want {
			t.Fatalf("%s = %q, want %q; env=%#v", key, got, want, env)
		}
	}
}

func TestSandboxRuntimeEnvironmentUsesManagedWritableState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtime")
	runtimeState := SandboxRuntime{
		Root:  root,
		Cache: filepath.Join(root, "cache"),
		Data:  filepath.Join(root, "data"),
		Temp:  filepath.Join(root, "tmp"),
	}
	env := sandboxRuntimeEnvironment([]string{
		"XDG_CACHE_HOME=/host/cache",
		"TMPDIR=/host/tmp",
		"PATH=/usr/bin",
	}, &runtimeState)

	for key, want := range map[string]string{
		"XDG_CACHE_HOME":    runtimeState.Cache,
		"TMPDIR":            runtimeState.Temp,
		"TMP":               runtimeState.Temp,
		"TEMP":              runtimeState.Temp,
		"npm_config_cache":  filepath.Join(runtimeState.Cache, "npm"),
		"YARN_CACHE_FOLDER": filepath.Join(runtimeState.Cache, "yarn"),
		"COREPACK_HOME":     filepath.Join(runtimeState.Cache, "corepack"),
		"PIP_CACHE_DIR":     filepath.Join(runtimeState.Cache, "pip"),
		"GOCACHE":           filepath.Join(runtimeState.Cache, "go-build"),
		"GOMODCACHE":        filepath.Join(runtimeState.Data, "go-mod"),
		"CARGO_HOME":        filepath.Join(runtimeState.Data, "cargo"),
	} {
		if got := envListValue(env, key, ""); got != want {
			t.Fatalf("%s = %q, want %q; env=%#v", key, got, want, env)
		}
	}
	if got := envListValue(env, "PATH", ""); got != "/usr/bin" {
		t.Fatalf("PATH = %q, want preserved caller path", got)
	}
}

func TestSandboxRuntimeEnvironmentPreservesGitGlobalIgnore(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is unavailable")
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	configHome := filepath.Join(home, ".config")
	repository := filepath.Join(root, "repository")
	runtimeRoot := filepath.Join(root, "runtime")
	for _, directory := range []string{
		filepath.Join(configHome, "git"),
		repository,
		filepath.Join(runtimeRoot, "cache"),
		filepath.Join(runtimeRoot, "data"),
		filepath.Join(runtimeRoot, "tmp"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(configHome, "git", "ignore"), []byte("ignored.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "ignored.txt"), []byte("ignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "visible.txt"), []byte("visible\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	runtimeState := SandboxRuntime{
		Root:  runtimeRoot,
		Cache: filepath.Join(runtimeRoot, "cache"),
		Data:  filepath.Join(runtimeRoot, "data"),
		Temp:  filepath.Join(runtimeRoot, "tmp"),
	}
	inherited := append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(root, "missing-global-config"),
		"GIT_DIR="+filepath.Join(root, "wrong-repository"),
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=core.excludesFile",
		"GIT_CONFIG_VALUE_0="+filepath.Join(root, "missing-ignore"),
	)
	env := sandboxRuntimeEnvironment(upsertEnvList(withoutGitEnvironmentOverrides(inherited),
		"HOME="+home,
		"XDG_CONFIG_HOME="+configHome,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
	), &runtimeState)
	runGit := func(args ...string) string {
		t.Helper()
		command := exec.Command(git, args...)
		command.Dir = repository
		command.Env = env
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
		}
		return string(output)
	}
	runGit("init", "--quiet")
	status := runGit("status", "--short", "--untracked-files=all")
	if strings.Contains(status, "ignored.txt") {
		t.Fatalf("global ignore was not honored:\n%s", status)
	}
	if !strings.Contains(status, "visible.txt") {
		t.Fatalf("non-ignored file is missing from status:\n%s", status)
	}
}

func withoutGitEnvironmentOverrides(env []string) []string {
	out := make([]string, 0, len(env))
	for _, value := range env {
		key, _, ok := strings.Cut(value, "=")
		if ok && strings.HasPrefix(strings.ToUpper(key), "GIT_") {
			continue
		}
		out = append(out, value)
	}
	return out
}

func TestEngineCommandPlanCarriesManagedRuntime(t *testing.T) {
	workspace := t.TempDir()
	cacheRoot := t.TempDir()
	t.Setenv("HOME", filepath.Join(t.TempDir(), "home"))
	original := sandboxUserCacheDir
	sandboxUserCacheDir = func() (string, error) { return cacheRoot, nil }
	t.Cleanup(func() { sandboxUserCacheDir = original })
	engine := NewEngine(EngineOptions{
		WorkspaceRoot: workspace,
		Policy:        DefaultPolicy(),
		Backend: Backend{
			Name:            BackendLinuxBwrap,
			Available:       true,
			Platform:        "linux",
			Executable:      "/usr/bin/zero-linux-sandbox",
			CommandWrapping: true,
			NativeIsolation: true,
		},
	})

	plan, err := engine.BuildCommandPlan(CommandSpec{Name: "/bin/sh", Args: []string{"-c", "true"}, Dir: workspace})
	if err != nil {
		t.Fatalf("BuildCommandPlan: %v", err)
	}
	defer plan.Cleanup()
	if plan.PermissionProfile.Runtime == nil || plan.PermissionProfile.Runtime.Root == "" {
		t.Fatal("command plan is missing managed runtime state")
	}
	if cleanupLease, inUse, err := tryAcquireSandboxRuntimeCleanupLease(plan.PermissionProfile.Runtime.Root); err != nil {
		t.Fatalf("inspect active runtime lease: %v", err)
	} else if cleanupLease != nil {
		cleanupLease.release()
		t.Fatal("command plan did not retain its runtime lease")
	} else if !inUse {
		t.Fatal("command plan runtime must be marked in use")
	}
	if got := envListValue(plan.Env, "HOME", ""); got != os.Getenv("HOME") {
		t.Fatalf("HOME = %q, want caller home %q", got, os.Getenv("HOME"))
	}
	foundWriteRoot := false
	for _, root := range plan.PermissionProfile.FileSystem.WriteRoots {
		if root.Root == plan.PermissionProfile.Runtime.Root {
			foundWriteRoot = true
		}
	}
	if !foundWriteRoot {
		t.Fatalf("runtime root is not writable in profile: %#v", plan.PermissionProfile.FileSystem.WriteRoots)
	}
	plan.Cleanup()
	cleanupLease, inUse, err := tryAcquireSandboxRuntimeCleanupLease(plan.PermissionProfile.Runtime.Root)
	if err != nil || inUse || cleanupLease == nil {
		t.Fatalf("runtime lease after plan cleanup = lease %v inUse %t err %v", cleanupLease, inUse, err)
	}
	cleanupLease.release()
}

// sandboxRuntimeRootFor compares the workspace root against the derived runtime
// root to decide whether to fall back to a private temp tree. Both sides
// therefore have to be the same spelling of the same path.
//
// Canonicalizing only the workspace root broke this on CI: the workspace
// resolved (/var to /private/var on macOS, an 8.3 short name to its long form
// on Windows) while the cache root kept its original spelling, so the
// containment check compared two different strings, the fallback never fired,
// and the runtime tree was placed inside the workspace it exists to stay out of.
//
// A symlink is the portable way to produce a spelling that only resolution
// reconciles — Clean cannot see through one. Windows refuses to create symlinks
// without privilege, so this skips there; the platforms that CI caught the bug
// on are the ones that run it.
func TestPrepareSandboxRuntimeNormalizesTheCacheRootBeforeComparingIt(t *testing.T) {
	isolateSandboxRuntimeRoots(t)
	workspace := t.TempDir()
	link := filepath.Join(t.TempDir(), "workspace-link")
	if err := os.Symlink(workspace, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	// The cache root reaches us spelled through the symlink; the workspace does
	// not. Resolved, it is plainly inside the workspace and the fallback must
	// fire. Unresolved, the two strings share no prefix and it does not.
	original := sandboxUserCacheDir
	sandboxUserCacheDir = func() (string, error) { return filepath.Join(link, ".cache"), nil }
	t.Cleanup(func() { sandboxUserCacheDir = original })

	runtimeState, release, err := prepareSandboxRuntime(workspace)
	if err != nil {
		t.Fatalf("prepareSandboxRuntime: %v", err)
	}
	if release != nil {
		defer release()
	}
	// Resolve before comparing. Spelled through the link the runtime root shares
	// no textual prefix with the workspace, so an unresolved comparison would
	// call it "outside" while it sits physically inside — the test would pass
	// against the very bug it exists for.
	resolved := runtimeState.Root
	if actual, err := filepath.EvalSymlinks(runtimeState.Root); err == nil {
		resolved = actual
	}
	if pathWithinRoot(workspace, resolved) {
		t.Fatalf("runtime root %q resolves to %q, inside workspace %q; the containment check did not see through the cache root's spelling", runtimeState.Root, resolved, workspace)
	}
}

// A path whose final segments do not exist yet must still normalize the same
// way as one that does. This is the shape macOS CI hit: t.TempDir() sits under
// /var, a symlink to /private/var, and the cache root it derives has not been
// created when it is first normalized. Resolving only the workspace left the
// two sides of the containment check spelled differently.
func TestCanonicalSandboxWorkspaceRootResolvesThroughAMissingLeaf(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	existing := canonicalSandboxWorkspaceRoot(link)
	if existing != canonicalSandboxWorkspaceRoot(real) {
		t.Fatalf("an existing symlinked dir did not resolve: %q vs %q", existing, canonicalSandboxWorkspaceRoot(real))
	}

	// The leaf, and its parent, do not exist.
	missing := filepath.Join(link, ".cache", "zero")
	got := canonicalSandboxWorkspaceRoot(missing)
	want := filepath.Join(existing, ".cache", "zero")
	if got != want {
		t.Errorf("missing leaf normalized to %q, want %q — the ancestor was not resolved", got, want)
	}
	if !pathWithinRoot(existing, got) {
		t.Errorf("%q should be inside %q once both are canonical", got, existing)
	}
}

// isolateSandboxRuntimeRoots gives a test its own cache root AND its own temp
// root, so every runtime tree the production code can select, the preferred
// cache-derived one and the temp-derived fallback, lands under the fixture.
// Workspace isolation is not runtime-storage isolation: those paths come from
// separate resolver inputs, and a test that redirected only the workspace ran
// real setup, preparation and reclamation against the developer's own cache
// and temp. Both roots are returned for assertions on where things landed.
func isolateSandboxRuntimeRoots(t *testing.T) (cacheRoot, tempRoot string) {
	t.Helper()
	cacheRoot = filepath.Join(t.TempDir(), "cache")
	tempRoot = filepath.Join(t.TempDir(), "temp")
	for _, dir := range []string{cacheRoot, tempRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	previous := sandboxUserCacheDir
	sandboxUserCacheDir = func() (string, error) { return cacheRoot, nil }
	t.Cleanup(func() { sandboxUserCacheDir = previous })
	// os.TempDir reads these at call time on every platform this runs on.
	t.Setenv("TMP", tempRoot)
	t.Setenv("TEMP", tempRoot)
	t.Setenv("TMPDIR", tempRoot)
	return cacheRoot, tempRoot
}
