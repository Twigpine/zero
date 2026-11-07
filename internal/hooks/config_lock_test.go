package hooks

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/lockutil"
)

// TestConfigStoreMutationWaitsForSiblingConfigLock pins the cross-process
// contract of ConfigStore: Upsert/Remove/SetEnabled are read-modify-write
// operations, so they must participate in the same <config>.lock protocol as
// every other writer. The package-local mutex only serializes goroutines in
// this process; it cannot exclude a second `zero hooks` process.
//
// On the unfixed code Upsert ignores <config>.lock and completes while the test
// holds it, which is exactly the window in which two CLI processes overwrite
// each other's hooks.
func TestConfigStoreMutationWaitsForSiblingConfigLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	store, err := NewConfigStore(StoreOptions{ConfigPath: path})
	if err != nil {
		t.Fatalf("NewConfigStore: %v", err)
	}
	if _, err := store.Upsert(Definition{ID: "seed", Event: EventAfterTool, Command: "node"}); err != nil {
		t.Fatalf("seed hook: %v", err)
	}

	lock, err := lockutil.TryAcquireFileLock(path + ".lock")
	if err != nil {
		t.Fatalf("acquire sibling config lock: %v", err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		if err := lock.Release(); err != nil {
			t.Errorf("release sibling config lock: %v", err)
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
		t.Fatalf("Upsert completed while %s was held elsewhere (err=%v); mutations are not excluded across processes", path+".lock", err)
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
// new lock: removing or disabling a hook that does not exist must not create
// .zero/ or hooks.json.lock.
func TestNoOpMutationsDoNotCreateConfigState(t *testing.T) {
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
		t.Fatalf("no-op mutation created lock file: %v", err)
	}
}

// TestCrossProcessHooksMutationExcludesAndPreserves is the coordinated
// two-process regression for lost updates. The child announces itself and then
// mutates; the parent holds the config lock across that window and asserts the
// child's write cannot land, then does its own mutation and releases. Both
// updates must survive.
func TestCrossProcessHooksMutationExcludesAndPreserves(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	store, err := NewConfigStore(StoreOptions{ConfigPath: path})
	if err != nil {
		t.Fatalf("NewConfigStore: %v", err)
	}
	if _, err := store.Upsert(Definition{ID: "seed", Event: EventAfterTool, Command: "node"}); err != nil {
		t.Fatalf("seed hook: %v", err)
	}

	parentLock, err := lockutil.TryAcquireFileLock(path + ".lock")
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
			t.Fatalf("child wrote while %s was held elsewhere; mutations are not excluded across processes", path+".lock")
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
// config directory already exists (t.TempDir), so the only directory sync this
// call performs is the one after the rename.
func TestWriteConfigSyncsFileAndDirectory(t *testing.T) {
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
