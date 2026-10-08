package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func readTestConfig(t *testing.T, path string) FileConfig {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	cfg := FileConfig{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("invalid config JSON after concurrent writes: %v\n%s", err, data)
	}
	return cfg
}

func seedTestConfig(t *testing.T, path string) {
	t.Helper()
	if _, err := UpsertProvider(path, ProviderProfile{Name: "seed", Model: "seed-model"}, true); err != nil {
		t.Fatalf("seed config: %v", err)
	}
}

// TestConcurrentMutationsDoNotLoseUpdates is the issue #832 regression. Each
// mutator loads the whole document, edits one field, and publishes a complete
// replacement by rename. Without a lock spanning load-through-publish, two
// writers that loaded the same revision both write a full document and the
// second rename silently discards the first one's acknowledged update.
//
// Every mutation below touches an INDEPENDENT field, so a correct
// implementation ends with all of them present. A lost update shows up as a
// zero value in exactly one of the fields, with valid JSON either way — which
// is what made the bug silent.
func TestConcurrentMutationsDoNotLoseUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	seedTestConfig(t, path)

	mutations := []struct {
		name  string
		apply func() error
		check func(FileConfig) error
	}{
		{
			name:  "theme",
			apply: func() error { _, err := SetTheme(path, "dracula"); return err },
			check: func(cfg FileConfig) error {
				if cfg.Preferences.Theme != "dracula" {
					return fmt.Errorf("theme = %q, want dracula", cfg.Preferences.Theme)
				}
				return nil
			},
		},
		{
			name:  "pet",
			apply: func() error { _, err := SetPet(path, "otter"); return err },
			check: func(cfg FileConfig) error {
				if cfg.Preferences.Pet != "otter" {
					return fmt.Errorf("pet = %q, want otter", cfg.Preferences.Pet)
				}
				return nil
			},
		},
		{
			name:  "recaps",
			apply: func() error { _, err := SetRecapsEnabled(path, true); return err },
			check: func(cfg FileConfig) error {
				if cfg.Preferences.Recaps == nil || !*cfg.Preferences.Recaps {
					return fmt.Errorf("recaps = %v, want true", cfg.Preferences.Recaps)
				}
				return nil
			},
		},
		{
			name:  "favorites",
			apply: func() error { _, err := SetFavoriteModels(path, []string{"fav-model"}); return err },
			check: func(cfg FileConfig) error {
				if len(cfg.Preferences.FavoriteModels) != 1 || cfg.Preferences.FavoriteModels[0] != "fav-model" {
					return fmt.Errorf("favorites = %v, want [fav-model]", cfg.Preferences.FavoriteModels)
				}
				return nil
			},
		},
		{
			name: "provider",
			apply: func() error {
				_, err := UpsertProvider(path, ProviderProfile{Name: "added", Model: "added-model"}, false)
				return err
			},
			check: func(cfg FileConfig) error {
				for _, provider := range cfg.Providers {
					if provider.Name == "added" {
						return nil
					}
				}
				return fmt.Errorf("provider %q missing from %d providers", "added", len(cfg.Providers))
			},
		},
	}

	// SetPet edits the raw bytes rather than round-tripping the struct, so it is
	// included deliberately: it must take the same lock as the struct writers or
	// it would clobber, and be clobbered by, everything else.
	errs := make([]error, len(mutations))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for index, mutation := range mutations {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[index] = mutation.apply()
		}()
	}
	close(start)
	wg.Wait()

	for index, err := range errs {
		if err != nil {
			t.Fatalf("%s mutation failed: %v", mutations[index].name, err)
		}
	}
	cfg := readTestConfig(t, path)
	for _, mutation := range mutations {
		if err := mutation.check(cfg); err != nil {
			t.Errorf("%s update was lost: %v", mutation.name, err)
		}
	}
	if cfg.ActiveProvider != "seed" {
		t.Errorf("activeProvider = %q, want the seeded value preserved", cfg.ActiveProvider)
	}
}

// TestConcurrentSameFieldMutationsSerialize proves the lock serializes writers
// that contend on ONE field: every call must succeed and the file must end with
// exactly one of the written values, never a merged or truncated document.
func TestConcurrentSameFieldMutationsSerialize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	seedTestConfig(t, path)

	const writers = 24
	errs := make([]error, writers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for index := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[index] = SetTheme(path, fmt.Sprintf("theme-%02d", index))
		}()
	}
	close(start)
	wg.Wait()

	for index, err := range errs {
		if err != nil {
			t.Fatalf("writer %d failed: %v", index, err)
		}
	}
	cfg := readTestConfig(t, path)
	if !strings.HasPrefix(cfg.Preferences.Theme, "theme-") {
		t.Fatalf("theme = %q, want one of the written values", cfg.Preferences.Theme)
	}
}

// TestConcurrentProviderUpsertsAllSurvive is the shape the issue describes most
// directly: N distinct providers added at once. Each add is an independent
// successful mutation, so all N must be present afterwards.
func TestConcurrentProviderUpsertsAllSurvive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	seedTestConfig(t, path)

	const providers = 16
	errs := make([]error, providers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for index := range providers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[index] = UpsertProvider(path, ProviderProfile{
				Name:  fmt.Sprintf("provider-%02d", index),
				Model: fmt.Sprintf("model-%02d", index),
			}, false)
		}()
	}
	close(start)
	wg.Wait()

	for index, err := range errs {
		if err != nil {
			t.Fatalf("upsert %d failed: %v", index, err)
		}
	}
	cfg := readTestConfig(t, path)
	present := map[string]string{}
	for _, provider := range cfg.Providers {
		present[provider.Name] = provider.Model
	}
	for index := range providers {
		name := fmt.Sprintf("provider-%02d", index)
		want := fmt.Sprintf("model-%02d", index)
		if got, ok := present[name]; !ok {
			t.Errorf("provider %s was lost (config holds %d providers)", name, len(cfg.Providers))
		} else if got != want {
			t.Errorf("provider %s model = %q, want %q", name, got, want)
		}
	}
}

// TestCrossProcessMutationExcludesAndPreserves is the coordinated two-process
// test the issue asks for. Goroutines share this process's descriptors, so only
// a second OS process shows the lock is held by the kernel rather than by
// in-process state.
//
// The child announces itself and then mutates; the parent holds the lock across
// that window and asserts the child's write CANNOT land, then does its own
// mutation and releases. Both updates must survive. The exclusion window is
// what makes this deterministic in the passing direction: with the lock held,
// the child is blocked in its retry loop and the theme provably stays unset.
func TestCrossProcessMutationExcludesAndPreserves(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	seedTestConfig(t, path)

	unlock, err := lockConfigFile(path)
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			unlock()
		}
	}
	defer release()

	child := exec.Command(os.Args[0], "-test.run=^TestConfigConcurrentHelperProcess$")
	child.Env = append(os.Environ(),
		"ZERO_CONFIG_CONCURRENT_HELPER=1",
		"ZERO_CONFIG_CONCURRENT_PATH="+path,
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
		if theme := readTestConfig(t, path).Preferences.Theme; theme != "" {
			t.Fatalf("child wrote %q while the lock was held elsewhere; mutations are not excluded", theme)
		}
		time.Sleep(5 * time.Millisecond)
	}

	if _, err := upsertProviderLocked(path, ProviderProfile{Name: "parent", Model: "parent-model"}, false); err != nil {
		t.Fatalf("parent mutation: %v", err)
	}
	release()

	if err := child.Wait(); err != nil {
		t.Fatalf("helper process failed: %v", err)
	}

	cfg := readTestConfig(t, path)
	if cfg.Preferences.Theme != "child-theme" {
		t.Errorf("child update was lost: theme = %q, want child-theme", cfg.Preferences.Theme)
	}
	found := false
	for _, provider := range cfg.Providers {
		if provider.Name == "parent" {
			found = true
		}
	}
	if !found {
		t.Errorf("parent update was lost: provider %q missing from %d providers", "parent", len(cfg.Providers))
	}
}

// TestConfigConcurrentHelperProcess is the child half of the cross-process
// test. It is skipped unless the parent selected it through the environment.
func TestConfigConcurrentHelperProcess(t *testing.T) {
	if os.Getenv("ZERO_CONFIG_CONCURRENT_HELPER") == "" {
		t.Skip("helper process for TestCrossProcessMutationExcludesAndPreserves")
	}
	path := os.Getenv("ZERO_CONFIG_CONCURRENT_PATH")
	if path == "" {
		t.Fatal("ZERO_CONFIG_CONCURRENT_PATH is required")
	}
	// Announce BEFORE contending, so the parent's exclusion window starts with
	// this process already running rather than still being forked.
	fmt.Println("ready")
	if _, err := SetTheme(path, "child-theme"); err != nil {
		t.Fatalf("child mutation: %v", err)
	}
}

// TestMutationReportsUnlockFailure pins the AGENTS.md rule that a mutator must
// never report success when unlock failed. A failed release can leave the lock
// held for the rest of the process, so returning (cfg, nil) after one would
// claim a state the next mutation cannot reproduce.
//
// lockutil.Release is idempotent and reports nil once released, so a real
// release failure cannot be provoked through the public API — hence the seam.
// The mutation itself must still have been published: the release failure
// annotates the result, it does not undo the write.
func TestMutationReportsUnlockFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	seedTestConfig(t, path)

	releaseErr := errors.New("release exploded")
	original := lockConfigFileFn
	t.Cleanup(func() { lockConfigFileFn = original })
	lockConfigFileFn = func(p string) (func() error, error) {
		unlock, err := original(p)
		if err != nil {
			return nil, err
		}
		return func() error {
			_ = unlock()
			return releaseErr
		}, nil
	}

	cfg, err := SetTheme(path, "dracula")
	if !errors.Is(err, releaseErr) {
		t.Fatalf("SetTheme err = %v, want it to carry the release failure", err)
	}
	if cfg.Preferences.Theme != "dracula" {
		t.Errorf("returned config lost the mutation: theme = %q", cfg.Preferences.Theme)
	}
	if got := readTestConfig(t, path).Preferences.Theme; got != "dracula" {
		t.Errorf("mutation was not published despite only the release failing: theme = %q", got)
	}
}

// TestMutationErrorSurvivesUnlockFailure proves the two errors are joined
// rather than chosen between: a release failure must not mask the mutation
// error that actually explains what went wrong.
func TestMutationErrorSurvivesUnlockFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	seedTestConfig(t, path)

	releaseErr := errors.New("release exploded")
	original := lockConfigFileFn
	t.Cleanup(func() { lockConfigFileFn = original })
	lockConfigFileFn = func(p string) (func() error, error) {
		unlock, err := original(p)
		if err != nil {
			return nil, err
		}
		return func() error {
			_ = unlock()
			return releaseErr
		}, nil
	}

	// An unknown provider is the mutation error; the release failure is joined
	// onto it, and neither one disappears.
	_, err := SetActiveProvider(path, "definitely-not-configured")
	if !errors.Is(err, releaseErr) {
		t.Fatalf("err = %v, want it to carry the release failure", err)
	}
	if !strings.Contains(err.Error(), "definitely-not-configured") {
		t.Fatalf("err = %v, want it to still name the missing provider", err)
	}
}
