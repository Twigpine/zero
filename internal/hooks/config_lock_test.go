package hooks

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/lockutil"
)

// isolateStateRoot points every per-user directory root the config lock
// resolves at a throwaway root: project-scope locks live under the user state
// directory (XDG_STATE_HOME, LOCALAPPDATA on Windows) and user-config
// detection resolves the default user config from HOME/XDG_CONFIG_HOME. All of
// them must be redirected so a test never reads or writes the developer's
// real user state; macOS derives these from HOME, so HOME is set along with
// the XDG variables.
func isolateStateRoot(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "LOCALAPPDATA"} {
		t.Setenv(name, root)
	}
}

// TestConfigStoreMutationWaitsForConfigLock pins the cross-process contract of
// ConfigStore: Upsert/Remove/SetEnabled are read-modify-write operations, so
// they must participate in the same config lock protocol as every other
// writer. The package-local mutex only serializes goroutines in this process;
// it cannot exclude a second `zero hooks` process.
//
// On the unfixed code Upsert ignores the config lock and completes while the
// test holds it, which is exactly the window in which two CLI processes
// overwrite each other's hooks.
func TestConfigStoreMutationWaitsForConfigLock(t *testing.T) {
	isolateStateRoot(t)
	path := filepath.Join(t.TempDir(), "hooks.json")
	store, err := NewConfigStore(StoreOptions{ConfigPath: path})
	if err != nil {
		t.Fatalf("NewConfigStore: %v", err)
	}
	if _, err := store.Upsert(Definition{ID: "seed", Event: EventAfterTool, Command: "node"}); err != nil {
		t.Fatalf("seed hook: %v", err)
	}

	lockPath, err := configLockPath(path)
	if err != nil {
		t.Fatalf("configLockPath: %v", err)
	}
	lock, err := lockutil.TryAcquireFileLock(lockPath)
	if err != nil {
		t.Fatalf("acquire config lock: %v", err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		if err := lock.Release(); err != nil {
			t.Errorf("release config lock: %v", err)
		}
	}
	defer release()

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, err := store.Upsert(Definition{ID: "blocked", Event: EventAfterTool, Command: "node"})
		done <- err
	}()
	<-started

	select {
	case err := <-done:
		t.Fatalf("Upsert completed while %s was held elsewhere (err=%v); mutations are not excluded across processes", lockPath, err)
	case <-time.After(500 * time.Millisecond):
	}

	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Upsert after releasing the config lock: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Upsert did not resume after the config lock was released")
	}

	config, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if findHook(config.Hooks, "seed") == nil {
		t.Fatalf("seed hook was lost: %v", hookIDs(config.Hooks))
	}
	if findHook(config.Hooks, "blocked") == nil {
		t.Fatalf("blocked hook was lost: %v", hookIDs(config.Hooks))
	}
}

// TestNoOpMutationsDoNotCreateConfigState pins the working-tree rule for the
// lock: removing or disabling a hook that does not exist must not create
// .zero/, a sibling lock file, or the state-directory lock.
func TestNoOpMutationsDoNotCreateConfigState(t *testing.T) {
	isolateStateRoot(t)
	configDir := filepath.Join(t.TempDir(), ".zero")
	path := filepath.Join(configDir, "hooks.json")
	store, err := NewConfigStore(StoreOptions{ConfigPath: path})
	if err != nil {
		t.Fatalf("NewConfigStore: %v", err)
	}

	if removed, err := store.Remove("missing"); err != nil || removed {
		t.Fatalf("Remove(missing) = %v, %v; want false, nil", removed, err)
	}
	if changed, err := store.SetEnabled("missing", false); err != nil || changed {
		t.Fatalf("SetEnabled(missing) = %v, %v; want false, nil", changed, err)
	}
	if _, err := os.Lstat(configDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no-op mutation created config state %s: %v", configDir, err)
	}
	if _, err := os.Lstat(path + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no-op mutation created a sibling lock file: %v", err)
	}
	lockPath, err := configLockPath(path)
	if err != nil {
		t.Fatalf("configLockPath: %v", err)
	}
	if _, err := os.Lstat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no-op mutation created the state lock %s: %v", lockPath, err)
	}
}

// TestProjectConfigLockLivesInUserState pins the lock-location rule for the
// project config: it lives inside a repository the user may commit, so a
// mutation must not leave a lock file anywhere in the working tree. The lock
// lives under the user state directory instead, keyed by the config path.
func TestProjectConfigLockLivesInUserState(t *testing.T) {
	isolateStateRoot(t)
	configDir := filepath.Join(t.TempDir(), ".zero")
	path := filepath.Join(configDir, "hooks.json")
	store, err := NewConfigStore(StoreOptions{ConfigPath: path})
	if err != nil {
		t.Fatalf("NewConfigStore: %v", err)
	}

	if _, err := store.Upsert(Definition{ID: "seed", Event: EventAfterTool, Command: "node"}); err != nil {
		t.Fatalf("seed hook: %v", err)
	}

	entries, err := os.ReadDir(configDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() != "hooks.json" {
			t.Fatalf("the project config directory gained %q; a committed .zero/ must not carry lock state", entry.Name())
		}
	}

	lockPath, err := configLockPath(path)
	if err != nil {
		t.Fatalf("configLockPath: %v", err)
	}
	if _, err := os.Lstat(lockPath); err != nil {
		t.Fatalf("the project config lock was not created at %s: %v", lockPath, err)
	}
}

// TestUserConfigLockIsStableSibling pins the other half of the location rule:
// the user config is already user state and keeps its sibling lock.
func TestUserConfigLockIsStableSibling(t *testing.T) {
	isolateStateRoot(t)
	paths, err := ResolvePaths(ResolvePathOptions{})
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	store, err := NewConfigStore(StoreOptions{ConfigPath: paths.UserConfigPath})
	if err != nil {
		t.Fatalf("NewConfigStore: %v", err)
	}
	if _, err := store.Upsert(Definition{ID: "user", Event: EventAfterTool, Command: "node"}); err != nil {
		t.Fatalf("user hook: %v", err)
	}

	if _, err := os.Lstat(paths.UserConfigPath + ".lock"); err != nil {
		t.Fatalf("the user config lock was not created as a sibling: %v", err)
	}
	lockPath, err := configLockPath(paths.UserConfigPath)
	if err != nil {
		t.Fatalf("configLockPath: %v", err)
	}
	wantLock := canonicalConfigPath(paths.UserConfigPath) + ".lock"
	if lockPath != wantLock {
		t.Fatalf("configLockPath(user config) = %s, want the sibling %s", lockPath, wantLock)
	}
}

// TestUpsertReplacesLinkedHooksConfigWithoutTouchingTarget pins the behavior
// of a symlinked hooks.json: the atomic rename must replace the link with a
// regular file and leave the file it pointed at unchanged. Canonicalizing the
// config file itself (final component included) would instead write through
// the link and clobber its target — a repository can commit .zero/hooks.json
// as a relative link, so that would overwrite a file outside the project.
func TestUpsertReplacesLinkedHooksConfigWithoutTouchingTarget(t *testing.T) {
	isolateStateRoot(t)
	configDir := filepath.Join(t.TempDir(), ".zero")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	target := filepath.Join(t.TempDir(), "package.json")
	targetData := []byte("{\"name\":\"not-a-hooks-config\"}\n")
	if err := os.WriteFile(target, targetData, 0o600); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	link := filepath.Join(configDir, "hooks.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable on %s: %v", runtime.GOOS, err)
	}
	store, err := NewConfigStore(StoreOptions{ConfigPath: link})
	if err != nil {
		t.Fatalf("NewConfigStore: %v", err)
	}
	if _, err := store.Upsert(Definition{ID: "linked", Event: EventAfterTool, Command: "node"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat link: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("the config link survived the write; Upsert wrote through it instead of replacing it")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile target: %v", err)
	}
	if string(got) != string(targetData) {
		t.Fatalf("the symlink target was overwritten: got %q, want %q", got, targetData)
	}
	config, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if findHook(config.Hooks, "linked") == nil {
		t.Fatalf("the written hook was not published: %v", hookIDs(config.Hooks))
	}
}

// TestCrossProcessHooksMutationExcludesAndPreserves is the coordinated
// two-process regression for lost updates. The child announces itself and then
// mutates; the parent holds the config lock across that window and asserts the
// child's write cannot land, then does its own mutation and releases. Both
// updates must survive.
func TestCrossProcessHooksMutationExcludesAndPreserves(t *testing.T) {
	isolateStateRoot(t)
	path := filepath.Join(t.TempDir(), "hooks.json")
	store, err := NewConfigStore(StoreOptions{ConfigPath: path})
	if err != nil {
		t.Fatalf("NewConfigStore: %v", err)
	}
	if _, err := store.Upsert(Definition{ID: "seed", Event: EventAfterTool, Command: "node"}); err != nil {
		t.Fatalf("seed hook: %v", err)
	}

	lockPath, err := configLockPath(path)
	if err != nil {
		t.Fatalf("configLockPath: %v", err)
	}
	parentLock, err := lockutil.TryAcquireFileLock(lockPath)
	if err != nil {
		t.Fatalf("acquire config lock: %v", err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		if err := parentLock.Release(); err != nil {
			t.Errorf("release config lock: %v", err)
		}
	}
	defer release()

	child := exec.Command(os.Args[0], "-test.run=^TestHooksConfigConcurrentHelperProcess$")
	child.Env = append(os.Environ(),
		"ZERO_HOOKS_CONFIG_CONCURRENT_HELPER=1",
		"ZERO_HOOKS_CONFIG_CONCURRENT_PATH="+path,
	)
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := child.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	defer func() {
		if child.ProcessState == nil {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	}()

	ready := make([]byte, len("ready\n"))
	if _, err := io.ReadFull(stdout, ready); err != nil {
		t.Fatalf("helper never signalled ready: %v", err)
	}

	// The child is now live and contending for a lock this process holds. Its
	// write must not appear while we hold it.
	for range 20 {
		config, err := store.List()
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if findHook(config.Hooks, "child") != nil {
			t.Fatalf("child wrote while %s was held elsewhere; mutations are not excluded across processes", lockPath)
		}
		time.Sleep(5 * time.Millisecond)
	}

	release()
	if _, err := store.Upsert(Definition{ID: "parent", Event: EventAfterTool, Command: "node"}); err != nil {
		t.Fatalf("parent mutation: %v", err)
	}
	if err := child.Wait(); err != nil {
		t.Fatalf("helper process failed: %v", err)
	}

	config, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, id := range []string{"seed", "child", "parent"} {
		if findHook(config.Hooks, id) == nil {
			t.Fatalf("hook %q was lost: %v", id, hookIDs(config.Hooks))
		}
	}
}

// TestHooksConfigConcurrentHelperProcess is the child half of the cross-process
// test. It is skipped unless the parent selected it through the environment.
func TestHooksConfigConcurrentHelperProcess(t *testing.T) {
	if os.Getenv("ZERO_HOOKS_CONFIG_CONCURRENT_HELPER") == "" {
		t.Skip("helper process for TestCrossProcessHooksMutationExcludesAndPreserves")
	}
	path := os.Getenv("ZERO_HOOKS_CONFIG_CONCURRENT_PATH")
	if path == "" {
		t.Fatal("ZERO_HOOKS_CONFIG_CONCURRENT_PATH is required")
	}
	store, err := NewConfigStore(StoreOptions{ConfigPath: path})
	if err != nil {
		t.Fatalf("NewConfigStore: %v", err)
	}
	// Announce before contending, so the parent's exclusion window starts with
	// this process already running rather than still being forked.
	fmt.Println("ready")
	if _, err := store.Upsert(Definition{ID: "child", Event: EventAfterTool, Command: "node"}); err != nil {
		t.Fatalf("child mutation: %v", err)
	}
}

// TestWriteConfigSyncsFileAndDirectory pins the crash-safety barriers. The
// config directory already exists (t.TempDir), so the only directory sync the
// write itself performs is the one after the rename; taking the config lock
// may add its own barrier calls for the state lock directory it creates.
func TestWriteConfigSyncsFileAndDirectory(t *testing.T) {
	isolateStateRoot(t)
	path := filepath.Join(t.TempDir(), "hooks.json")
	origFile, origDir := syncHooksFileFn, syncHooksDirFn
	t.Cleanup(func() {
		syncHooksFileFn, syncHooksDirFn = origFile, origDir
	})
	fileSynced, dirSynced := false, false
	syncHooksFileFn = func(file *os.File) error {
		fileSynced = true
		return origFile(file)
	}
	syncHooksDirFn = func(dir string) error {
		dirSynced = true
		return origDir(dir)
	}

	config := Config{Enabled: true, Hooks: []Definition{{ID: "a", Event: EventAfterTool, Command: "node"}}}
	if err := WriteConfig(path, config); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	if !fileSynced {
		t.Fatal("WriteConfig returned without syncing the temp file")
	}
	if !dirSynced {
		t.Fatal("WriteConfig returned without syncing the parent directory")
	}
}

// TestWriteConfigSyncFailurePreservesPreviousConfig makes the new file fail
// before it becomes visible. The old hooks must still load and the failed
// attempt must not leave its temp file behind.
func TestWriteConfigSyncFailurePreservesPreviousConfig(t *testing.T) {
	isolateStateRoot(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	oldConfig := Config{Enabled: true, Hooks: []Definition{{ID: "old", Event: EventAfterTool, Command: "node"}}}
	if err := WriteConfig(path, oldConfig); err != nil {
		t.Fatalf("seed WriteConfig: %v", err)
	}

	origFile := syncHooksFileFn
	t.Cleanup(func() { syncHooksFileFn = origFile })
	syncErr := errors.New("sync failed")
	syncHooksFileFn = func(*os.File) error { return syncErr }

	newConfig := Config{Enabled: true, Hooks: []Definition{{ID: "new", Event: EventAfterTool, Command: "node"}}}
	err := WriteConfig(path, newConfig)
	if !errors.Is(err, syncErr) {
		t.Fatalf("WriteConfig error = %v, want it to wrap %v", err, syncErr)
	}

	store, err := NewConfigStore(StoreOptions{ConfigPath: path})
	if err != nil {
		t.Fatalf("NewConfigStore: %v", err)
	}
	config, err := store.List()
	if err != nil {
		t.Fatalf("List after failed write: %v", err)
	}
	if findHook(config.Hooks, "old") == nil {
		t.Fatalf("previous hook was lost: %v", hookIDs(config.Hooks))
	}
	if findHook(config.Hooks, "new") != nil {
		t.Fatalf("failed write became visible: %v", hookIDs(config.Hooks))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".hooks-") && strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("failed write left temp file %s", entry.Name())
		}
	}
}

// TestWriteConfigReportsUnlockFailure pins the AGENTS.md rule that a mutator
// must not report success when unlock failed. The write itself is already
// committed and must remain; the release failure annotates the result.
func TestWriteConfigReportsUnlockFailure(t *testing.T) {
	isolateStateRoot(t)
	path := filepath.Join(t.TempDir(), "hooks.json")
	orig := configLockFileFn
	t.Cleanup(func() { configLockFileFn = orig })
	releaseErr := errors.New("release exploded")
	configLockFileFn = func(path string) (func() error, error) {
		unlock, err := orig(path)
		if err != nil {
			return nil, err
		}
		return func() error {
			_ = unlock()
			return releaseErr
		}, nil
	}

	config := Config{Enabled: true, Hooks: []Definition{{ID: "a", Event: EventAfterTool, Command: "node"}}}
	err := WriteConfig(path, config)
	if !errors.Is(err, releaseErr) {
		t.Fatalf("WriteConfig error = %v, want it to carry the release failure", err)
	}
	store, err := NewConfigStore(StoreOptions{ConfigPath: path})
	if err != nil {
		t.Fatalf("NewConfigStore: %v", err)
	}
	loaded, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if findHook(loaded.Hooks, "a") == nil {
		t.Fatalf("published config lost the hook: %v", hookIDs(loaded.Hooks))
	}
}

// TestWriteConfigCreatesMissingDirectories covers the first-write path: the
// config directory and its parents may not exist yet.
func TestWriteConfigCreatesMissingDirectories(t *testing.T) {
	isolateStateRoot(t)
	path := filepath.Join(t.TempDir(), "a", "b", "hooks.json")
	config := Config{Enabled: true, Hooks: []Definition{{ID: "a", Event: EventAfterTool, Command: "node"}}}
	if err := WriteConfig(path, config); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config was not created: %v", err)
	}
	store, err := NewConfigStore(StoreOptions{ConfigPath: path})
	if err != nil {
		t.Fatalf("NewConfigStore: %v", err)
	}
	loaded, err := store.List()
	if err != nil || findHook(loaded.Hooks, "a") == nil {
		t.Fatalf("List = %#v err=%v", loaded, err)
	}
}

// TestConfigLockRootFor exercises every branch of the state-directory
// resolution on any host: the Windows variables, the XDG_STATE_HOME default,
// and the HOME/USERPROFILE fallbacks. configLockRoot's Windows branches never
// run on a Linux unit-test host, and isolateStateRoot satisfies every lookup
// in the integration tests, so the parameterized form carries the coverage
// AGENTS.md requires for path-sensitive logic.
func TestConfigLockRootFor(t *testing.T) {
	// isolateStateRoot points every per-user root at a throwaway root first;
	// the explicit HOME/USERPROFILE override below then makes the
	// os.UserHomeDir fallback cases deterministic on every platform.
	isolateStateRoot(t)
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("USERPROFILE", homeDir)

	env := func(values map[string]string) func(string) string {
		return func(key string) string { return values[key] }
	}
	lockRoot := func(base string) string {
		return filepath.Join(base, "zero", "hooks", "config-locks")
	}
	localAppData := filepath.Join("c", "local")
	profile := filepath.Join("c", "profile")
	stateHome := filepath.Join("s", "state")

	for _, tc := range []struct {
		name string
		goos string
		env  map[string]string
		want string
	}{
		{"windows prefers LOCALAPPDATA", "windows", map[string]string{"LOCALAPPDATA": localAppData}, lockRoot(localAppData)},
		{"windows falls back to USERPROFILE", "windows", map[string]string{"USERPROFILE": profile}, lockRoot(filepath.Join(profile, "AppData", "Local"))},
		{"windows falls through to XDG_STATE_HOME", "windows", map[string]string{"XDG_STATE_HOME": stateHome}, lockRoot(stateHome)},
		{"windows falls back to the home directory", "windows", map[string]string{}, lockRoot(filepath.Join(homeDir, ".local", "state"))},
		{"non-windows prefers XDG_STATE_HOME", "darwin", map[string]string{"XDG_STATE_HOME": stateHome}, lockRoot(stateHome)},
		{"non-windows falls back to HOME", "linux", map[string]string{"HOME": homeDir}, lockRoot(filepath.Join(homeDir, ".local", "state"))},
		{"non-windows falls back to USERPROFILE", "linux", map[string]string{"USERPROFILE": profile}, lockRoot(filepath.Join(profile, ".local", "state"))},
		{"non-windows falls back to the home directory", "linux", map[string]string{}, lockRoot(filepath.Join(homeDir, ".local", "state"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := configLockRootFor(tc.goos, env(tc.env))
			if err != nil {
				t.Fatalf("configLockRootFor: %v", err)
			}
			if got != tc.want {
				t.Fatalf("configLockRootFor(%s) = %s, want %s", tc.goos, got, tc.want)
			}
		})
	}
}

// TestWriteConfigRollsBackCreatedDirectoriesOnSyncFailure pins the failure
// path of ensureConfigDir: when the barrier for a directory this call created
// fails, the call must remove only the directories it created and report the
// failure. The write to a/b/hooks.json fails while syncing the barrier of a,
// so both a and its child b must be rolled back and a must not exist again.
func TestWriteConfigRollsBackCreatedDirectoriesOnSyncFailure(t *testing.T) {
	isolateStateRoot(t)
	base := t.TempDir()
	path := filepath.Join(base, "a", "b", "hooks.json")

	origDir := syncHooksDirFn
	t.Cleanup(func() { syncHooksDirFn = origDir })
	dirSyncErr := errors.New("sync failed")
	// WriteConfig canonicalizes the path before touching the filesystem, so
	// the barrier for a is synced under its physical spelling.
	failingDir := canonicalConfigPath(filepath.Join(base, "a"))
	syncHooksDirFn = func(dir string) error {
		if dir == failingDir {
			return dirSyncErr
		}
		return origDir(dir)
	}

	config := Config{Enabled: true, Hooks: []Definition{{ID: "a", Event: EventAfterTool, Command: "node"}}}
	err := WriteConfig(path, config)
	if !errors.Is(err, dirSyncErr) {
		t.Fatalf("WriteConfig error = %v, want it to wrap %v", err, dirSyncErr)
	}
	if _, statErr := os.Lstat(filepath.Join(base, "a", "b")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("the deepest directory created by the failed call survived: %v", statErr)
	}
	if _, statErr := os.Lstat(filepath.Join(base, "a")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("the directory created by the failed call survived: %v", statErr)
	}
	if _, statErr := os.Lstat(base); statErr != nil {
		t.Fatalf("rollback removed a pre-existing directory: %v", statErr)
	}
}

// TestWriteConfigDirSyncFailureAfterRenameKeepsNewConfig pins the other
// directory barrier: the rename has already published the new config when the
// final directory sync fails, so the call reports the error and the new
// document stays readable — there is no rollback of an already-visible file.
func TestWriteConfigDirSyncFailureAfterRenameKeepsNewConfig(t *testing.T) {
	isolateStateRoot(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	oldConfig := Config{Enabled: true, Hooks: []Definition{{ID: "old", Event: EventAfterTool, Command: "node"}}}
	if err := WriteConfig(path, oldConfig); err != nil {
		t.Fatalf("seed WriteConfig: %v", err)
	}

	origDir := syncHooksDirFn
	t.Cleanup(func() { syncHooksDirFn = origDir })
	dirSyncErr := errors.New("sync failed")
	// The final barrier targets the canonical config directory, since
	// WriteConfig canonicalizes the path before touching the filesystem.
	configDir := canonicalConfigPath(filepath.Dir(path))
	syncHooksDirFn = func(dir string) error {
		if dir == configDir {
			return dirSyncErr
		}
		return origDir(dir)
	}

	newConfig := Config{Enabled: true, Hooks: []Definition{{ID: "new", Event: EventAfterTool, Command: "node"}}}
	err := WriteConfig(path, newConfig)
	if !errors.Is(err, dirSyncErr) {
		t.Fatalf("WriteConfig error = %v, want it to wrap %v", err, dirSyncErr)
	}

	store, err := NewConfigStore(StoreOptions{ConfigPath: path})
	if err != nil {
		t.Fatalf("NewConfigStore: %v", err)
	}
	config, err := store.List()
	if err != nil {
		t.Fatalf("List after failed directory sync: %v", err)
	}
	if findHook(config.Hooks, "new") == nil {
		t.Fatalf("the published config was lost: %v", hookIDs(config.Hooks))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".hooks-") && strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("failed write left temp file %s", entry.Name())
		}
	}
}

// TestConfigLockAcquireTimesOut pins the give-up path of lockConfigFile: a
// mutation that cannot take the config lock fails with a timeout error that
// names the lock, instead of blocking forever.
func TestConfigLockAcquireTimesOut(t *testing.T) {
	isolateStateRoot(t)
	path := filepath.Join(t.TempDir(), "hooks.json")
	store, err := NewConfigStore(StoreOptions{ConfigPath: path})
	if err != nil {
		t.Fatalf("NewConfigStore: %v", err)
	}
	if _, err := store.Upsert(Definition{ID: "seed", Event: EventAfterTool, Command: "node"}); err != nil {
		t.Fatalf("seed hook: %v", err)
	}

	lockPath, err := configLockPath(path)
	if err != nil {
		t.Fatalf("configLockPath: %v", err)
	}
	held, err := lockutil.TryAcquireFileLock(lockPath)
	if err != nil {
		t.Fatalf("acquire config lock: %v", err)
	}
	defer func() {
		if err := held.Release(); err != nil {
			t.Errorf("release config lock: %v", err)
		}
	}()

	origTimeout := configLockTimeout
	configLockTimeout = 50 * time.Millisecond
	t.Cleanup(func() { configLockTimeout = origTimeout })

	start := time.Now()
	_, err = store.Upsert(Definition{ID: "blocked", Event: EventAfterTool, Command: "node"})
	if err == nil {
		t.Fatal("Upsert succeeded while the config lock was held elsewhere")
	}
	if !strings.Contains(err.Error(), "timed out acquiring config lock") || !strings.Contains(err.Error(), lockPath) {
		t.Fatalf("Upsert error = %v, want a config-lock timeout naming %s", err, lockPath)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Upsert took %s to time out, want a prompt failure", elapsed)
	}

	config, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if findHook(config.Hooks, "blocked") != nil || findHook(config.Hooks, "seed") == nil {
		t.Fatalf("a timed-out mutation must write nothing: %v", hookIDs(config.Hooks))
	}
}

// TestCanonicalConfigPath pins the physical-form rule the project lock key
// relies on: two spellings of the same location resolve to one path, and a
// not-yet-created tail is kept spelled under the resolved ancestor, so a
// first-write race keys the same lock either way.
func TestCanonicalConfigPath(t *testing.T) {
	root := t.TempDir()
	physicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks(root): %v", err)
	}

	if got := canonicalConfigPath(filepath.Join(root, "missing-dir", "hooks.json")); got != filepath.Join(physicalRoot, "missing-dir", "hooks.json") {
		t.Fatalf("canonicalConfigPath = %s, want the physical ancestor with the missing tail kept: %s", got, filepath.Join(physicalRoot, "missing-dir", "hooks.json"))
	}

	dir := filepath.Join(root, "project")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	link := filepath.Join(root, "project-link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("symlinks unavailable on %s: %v", runtime.GOOS, err)
	}
	direct := canonicalConfigPath(filepath.Join(dir, "hooks.json"))
	throughLink := canonicalConfigPath(filepath.Join(link, "hooks.json"))
	if throughLink != direct {
		t.Fatalf("canonicalConfigPath through a link = %s, want the physical form %s", throughLink, direct)
	}

	// A symlinked config file is deliberately NOT resolved: its own path is
	// kept so the atomic rename replaces the link instead of writing through
	// it. Only the parent directory is canonicalized.
	targetFile := filepath.Join(root, "target.json")
	if err := os.WriteFile(targetFile, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	linkFile := filepath.Join(root, "hooks-link.json")
	if err := os.Symlink(targetFile, linkFile); err != nil {
		t.Skipf("symlinks unavailable on %s: %v", runtime.GOOS, err)
	}
	want := filepath.Join(physicalRoot, "hooks-link.json")
	if got := canonicalConfigPath(linkFile); got != want {
		t.Fatalf("canonicalConfigPath kept the file link = %s, want the link's own path under the resolved parent: %s", got, want)
	}

	// Windows junctions are not symlinks: filepath.EvalSymlinks leaves them
	// unresolved, so a real directory and a junction to it hash to two lock
	// files. That is a documented limit, not covered by a portable unit test
	// (creating a junction needs elevation); the kernel lock still prevents
	// partial writes through either spelling.
}
