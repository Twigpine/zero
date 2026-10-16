package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestWriteConfigDataSync(t *testing.T) {
	failure := errors.New("injected sync failure")
	for _, stage := range []string{"success", "file", "directory"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			oldData, newData := "{\"old\":true}\n", "{\"new\":true}\n"
			if err := os.WriteFile(path, []byte(oldData), 0o600); err != nil {
				t.Fatal(err)
			}
			checkData := func(path, want string) {
				t.Helper()
				got, err := os.ReadFile(path)
				if err != nil || string(got) != want {
					t.Fatalf("read %s = %q, %v; want %q", path, got, err, want)
				}
			}
			fileSync, dirSync := syncConfigFileFn, syncConfigDirFn
			t.Cleanup(func() { syncConfigFileFn, syncConfigDirFn = fileSync, dirSync })
			var tmp *os.File
			var calls []string
			syncConfigFileFn = func(f *os.File) error {
				tmp = f
				calls = append(calls, "file")
				checkData(f.Name(), newData)
				checkData(path, oldData)
				if stage == "file" {
					return failure
				}
				return fileSync(f)
			}
			syncConfigDirFn = func(got string) error {
				calls = append(calls, "directory")
				if got != dir || tmp == nil {
					t.Fatalf("directory sync = %q, temp = %v", got, tmp)
				}
				if _, err := tmp.Seek(0, io.SeekCurrent); !errors.Is(err, os.ErrClosed) {
					t.Fatalf("temp must be closed before directory sync: %v", err)
				}
				checkData(path, newData)
				if stage == "directory" {
					return failure
				}
				return dirSync(got)
			}
			err := writeConfigData(path, []byte(strings.TrimSuffix(newData, "\n")))
			if stage == "success" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, failure) {
				t.Fatalf("error = %v, want injected sync failure", err)
			}
			wantCalls := []string{"file", "directory"}
			wantData := newData
			if stage == "file" {
				wantCalls, wantData = []string{"file"}, oldData
			}
			if !reflect.DeepEqual(calls, wantCalls) {
				t.Fatalf("sync calls = %v, want %v", calls, wantCalls)
			}
			checkData(path, wantData)
			if _, err := tmp.Seek(0, io.SeekCurrent); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("temp must be closed on return: %v", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 || entries[0].Name() != "config.json" {
				t.Fatalf("temporary file not cleaned up: %v, %v", entries, err)
			}
		})
	}
}

func TestWriteConfigDataSyncsCreatedDirectories(t *testing.T) {
	root := t.TempDir()
	outer := filepath.Join(root, "a")
	dir := filepath.Join(outer, "b")
	path := filepath.Join(dir, "config.json")
	failure := errors.New("injected sync failure")
	dirSync := syncConfigDirFn
	t.Cleanup(func() { syncConfigDirFn = dirSync })

	for _, fail := range []bool{false, true} {
		var calls []string
		syncConfigDirFn = func(got string) error {
			calls = append(calls, got)
			if fail && got == root {
				return failure
			}
			return dirSync(got)
		}
		if err := os.RemoveAll(outer); err != nil {
			t.Fatal(err)
		}
		err := writeConfigData(path, []byte(`{}`))
		if fail {
			if !errors.Is(err, failure) {
				t.Fatalf("error = %v, want injected sync failure", err)
			}
			if want := []string{outer, root}; !reflect.DeepEqual(calls, want) {
				t.Fatalf("sync calls = %v, want %v", calls, want)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("config must not be written after a failed directory sync: %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		// Each new directory's entry is synced into its parent, then the
		// config's own directory after the rename.
		if want := []string{outer, root, dir}; !reflect.DeepEqual(calls, want) {
			t.Fatalf("sync calls = %v, want %v", calls, want)
		}
	}
}

func TestLockingMutatorSyncsCreatedDirectories(t *testing.T) {
	root := t.TempDir()
	outer := filepath.Join(root, "a")
	dir := filepath.Join(outer, "b")
	path := filepath.Join(dir, "config.json")
	failure := errors.New("injected sync failure")
	dirSync := syncConfigDirFn
	t.Cleanup(func() { syncConfigDirFn = dirSync })

	exists := func(p string) bool {
		t.Helper()
		_, err := os.Lstat(p)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		return err == nil
	}

	// Every exported mutator acquires the config lock before it writes, and
	// the lock file is a sibling of the config, so the lock path is what first
	// creates the config directory. The barrier for those new entries has to
	// run there, before the lock file exists: by the time writeConfigData
	// looks, nothing is missing any more.
	for _, fail := range []bool{true, false} {
		var calls []string
		syncConfigDirFn = func(got string) error {
			calls = append(calls, got)
			if got == outer || got == root {
				if exists(path+".lock") || exists(path) {
					t.Fatalf("parent sync of %s ran after the lock file or config was created", got)
				}
			}
			if fail && got == root {
				return failure
			}
			return dirSync(got)
		}
		_, err := UpdateNotify(path, func(NotifyConfig) NotifyConfig {
			return NotifyConfig{Mode: "off"}
		})
		if fail {
			if !errors.Is(err, failure) {
				t.Fatalf("error = %v, want injected sync failure", err)
			}
			if want := []string{outer, root}; !reflect.DeepEqual(calls, want) {
				t.Fatalf("sync calls = %v, want %v", calls, want)
			}
			// The directories this attempt created are gone again, so the
			// retry below re-creates them and re-runs the barrier instead of
			// finding them present and reporting a durability it never had.
			if exists(outer) {
				t.Fatalf("%s must be rolled back after a failed parent sync", outer)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{outer, root, dir}; !reflect.DeepEqual(calls, want) {
			t.Fatalf("sync calls = %v, want %v", calls, want)
		}
		got, err := UserNotify(path)
		if err != nil || got.Mode != "off" {
			t.Fatalf("UserNotify = %+v, %v; want mode off", got, err)
		}
	}
}

func TestWriteConfigDataRollsBackOnlyCreatedDirectories(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "existing")
	if err := os.Mkdir(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(existing, "keep")
	if err := os.WriteFile(keep, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outer := filepath.Join(existing, "a")
	dir := filepath.Join(outer, "b")
	path := filepath.Join(dir, "config.json")
	failure := errors.New("injected sync failure")
	dirSync := syncConfigDirFn
	t.Cleanup(func() { syncConfigDirFn = dirSync })
	stat := func(p string) error {
		_, err := os.Lstat(p)
		return err
	}

	// The barrier for "a" fails: both directories this call created go, the
	// pre-existing directory and its file stay.
	syncConfigDirFn = func(got string) error {
		if got == existing {
			return failure
		}
		return dirSync(got)
	}
	if err := writeConfigData(path, []byte(`{}`)); !errors.Is(err, failure) {
		t.Fatalf("error = %v, want injected sync failure", err)
	}
	for _, p := range []string{dir, outer} {
		if err := stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s must be removed after a failed parent sync: %v", p, err)
		}
	}
	for _, p := range []string{existing, keep} {
		if err := stat(p); err != nil {
			t.Fatalf("pre-existing %s must survive the rollback: %v", p, err)
		}
	}

	// A directory another process populated in the meantime is not ours to
	// remove: it stays, the failed rollback is reported with the barrier
	// failure, and the write still does not acknowledge the save.
	intruder := filepath.Join(dir, "other.lock")
	syncConfigDirFn = func(got string) error {
		if got == existing {
			if err := os.WriteFile(intruder, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return failure
		}
		return dirSync(got)
	}
	err := writeConfigData(path, []byte(`{}`))
	if !errors.Is(err, failure) {
		t.Fatalf("error = %v, want injected sync failure", err)
	}
	if err := stat(intruder); err != nil {
		t.Fatalf("a populated directory must not be removed: %v", err)
	}
	if !strings.Contains(err.Error(), "roll back config directory") {
		t.Fatalf("error must report the failed rollback: %v", err)
	}
	if err := stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("config must not be written after a failed directory sync: %v", err)
	}
}

func TestSyncConfigDir(t *testing.T) {
	dir := t.TempDir()
	if err := syncConfigDir(dir); err != nil {
		t.Fatal(err)
	}
	err := syncConfigDir(filepath.Join(dir, "missing"))
	if runtime.GOOS == "windows" {
		// Directory handles cannot be synced on Windows, so every call is a
		// no-op there, including one for a directory that does not exist.
		if err != nil {
			t.Fatalf("syncConfigDir must be a no-op on windows: %v", err)
		}
		return
	}
	// A directory that is gone means the persistence barrier was never
	// reached, so the caller must not acknowledge the save.
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing directory: error = %v, want %v", err, os.ErrNotExist)
	}
	// A directory the user can write but not read cannot be opened for
	// syncing; that is best effort, as in the sessions store, because the
	// write it follows has already succeeded.
	unreadable := filepath.Join(dir, "unreadable")
	if err := os.Mkdir(unreadable, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o700) })
	if err := syncConfigDir(unreadable); err != nil {
		t.Fatalf("write-only directory must be best effort: %v", err)
	}
}

func TestSetActiveProviderSwitchesConfiguredProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "OpenAI",
		Providers: []ProviderProfile{
			{
				Name:         "OpenAI",
				ProviderKind: ProviderKindOpenAI,
				Model:        "gpt-4.1",
			},
			{
				Name:         "Anthropic",
				ProviderKind: ProviderKindAnthropic,
				Model:        "claude-3-5-sonnet-latest",
			},
		},
	}, 0o600)

	cfg, err := SetActiveProvider(path, "  Anthropic  ")
	if err != nil {
		t.Fatalf("SetActiveProvider() error = %v", err)
	}

	if cfg.ActiveProvider != "Anthropic" {
		t.Fatalf("ActiveProvider = %q, want Anthropic", cfg.ActiveProvider)
	}

	persisted := readConfigFixture(t, path)
	if persisted.ActiveProvider != "Anthropic" {
		t.Fatalf("persisted ActiveProvider = %q, want Anthropic", persisted.ActiveProvider)
	}
}

func TestSetActiveProviderRejectsUnknownProviderWithoutRewriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	before := writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "openai",
		Providers: []ProviderProfile{
			{Name: "openai", ProviderKind: ProviderKindOpenAI, Model: "gpt-4.1"},
			{Name: "anthropic", ProviderKind: ProviderKindAnthropic, Model: "claude-3-5-sonnet-latest"},
		},
	}, 0o600)

	_, err := SetActiveProvider(path, "google")
	if err == nil {
		t.Fatal("SetActiveProvider() error = nil, want error")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("config was rewritten for unknown provider\nbefore: %s\nafter: %s", before, after)
	}

	persisted := readConfigFixture(t, path)
	if persisted.ActiveProvider != "openai" {
		t.Fatalf("persisted ActiveProvider = %q, want openai", persisted.ActiveProvider)
	}
}

func TestSetActiveProviderRejectsEmptyProviderName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	before := writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "openai",
		Providers: []ProviderProfile{
			{Name: "openai", ProviderKind: ProviderKindOpenAI, Model: "gpt-4.1"},
		},
	}, 0o600)

	_, err := SetActiveProvider(path, " \t\n ")
	if err == nil {
		t.Fatal("SetActiveProvider() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "provider name is required") {
		t.Fatalf("SetActiveProvider() error = %q, want provider name required", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("config was rewritten for empty provider name\nbefore: %s\nafter: %s", before, after)
	}
}

func TestSetActiveProviderRejectsEmptyConfigPath(t *testing.T) {
	_, err := SetActiveProvider(" \t\n ", "openai")
	if err == nil {
		t.Fatal("SetActiveProvider() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "config path is required") {
		t.Fatalf("SetActiveProvider() error = %q, want config path required", err)
	}
}

func TestSetActiveProviderRejectsMissingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")

	_, err := SetActiveProvider(path, "openai")
	if err == nil {
		t.Fatal("SetActiveProvider() error = nil, want error")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("SetActiveProvider() error = %v, want not-exist error", err)
	}
}

func TestSetActiveProviderTightensExistingConfigFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose POSIX mode bits reliably")
	}

	path := filepath.Join(t.TempDir(), "zero.json")
	writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "openai",
		Providers: []ProviderProfile{
			{Name: "openai", ProviderKind: ProviderKindOpenAI, Model: "gpt-4.1"},
			{Name: "anthropic", ProviderKind: ProviderKindAnthropic, Model: "claude-3-5-sonnet-latest"},
		},
	}, 0o644)

	_, err := SetActiveProvider(path, "anthropic")
	if err != nil {
		t.Fatalf("SetActiveProvider() error = %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config mode = %o, want 0600", got)
	}
}

func TestSetActiveProviderModelPersistsSelectionTogether(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "openai",
		Providers: []ProviderProfile{
			{Name: "openai", ProviderKind: ProviderKindOpenAI, Model: "gpt-4.1"},
			{Name: "Anthropic", ProviderKind: ProviderKindAnthropic, Model: "old-model"},
		},
	}, 0o600)

	cfg, err := SetActiveProviderModel(path, " anthropic ", " claude-sonnet-4.5 ")
	if err != nil {
		t.Fatalf("SetActiveProviderModel() error = %v", err)
	}
	if cfg.ActiveProvider != "Anthropic" || cfg.Providers[1].Model != "claude-sonnet-4.5" {
		t.Fatalf("returned selection = active %q model %q", cfg.ActiveProvider, cfg.Providers[1].Model)
	}
	persisted := readConfigFixture(t, path)
	if persisted.ActiveProvider != "Anthropic" || persisted.Providers[1].Model != "claude-sonnet-4.5" {
		t.Fatalf("persisted selection = active %q model %q", persisted.ActiveProvider, persisted.Providers[1].Model)
	}
	if persisted.Providers[0].Model != "gpt-4.1" {
		t.Fatalf("unrelated provider changed: %#v", persisted.Providers[0])
	}
}

func TestSetActiveProviderModelKeepsUnicodeIdentitiesDistinct(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "s",
		Providers: []ProviderProfile{
			{Name: "s", Model: "latin-model"},
			{Name: "ſ", Model: "long-model"},
		},
	}, 0o600)
	if _, err := SetActiveProviderModel(path, "ſ", "new-long-model"); err != nil {
		t.Fatal(err)
	}
	cfg := readConfigFixture(t, path)
	if cfg.ActiveProvider != "ſ" || cfg.Providers[0].Model != "latin-model" || cfg.Providers[1].Model != "new-long-model" {
		t.Fatal("selection changed the wrong Unicode identity")
	}
}

func TestSetProviderModelUpdatesConfiguredProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "openai",
		Providers: []ProviderProfile{
			{
				Name:         "openai",
				ProviderKind: ProviderKindOpenAI,
				APIKey:       "sk-test",
				Model:        "gpt-4.1",
			},
			{
				Name:         "anthropic",
				ProviderKind: ProviderKindAnthropic,
				Model:        "claude-sonnet-4.5",
			},
		},
	}, 0o600)

	cfg, err := SetProviderModel(path, " openai ", " gpt-4.1-mini ")
	if err != nil {
		t.Fatalf("SetProviderModel() error = %v", err)
	}

	if cfg.Providers[0].Model != "gpt-4.1-mini" {
		t.Fatalf("updated provider model = %q, want gpt-4.1-mini", cfg.Providers[0].Model)
	}
	if cfg.Providers[0].APIKey != "sk-test" {
		t.Fatalf("provider credential was not preserved: %#v", cfg.Providers[0])
	}
	if cfg.Providers[1].Model != "claude-sonnet-4.5" {
		t.Fatalf("unrelated provider changed: %#v", cfg.Providers[1])
	}

	persisted := readConfigFixture(t, path)
	if persisted.Providers[0].Model != "gpt-4.1-mini" {
		t.Fatalf("persisted provider model = %q, want gpt-4.1-mini", persisted.Providers[0].Model)
	}
	if persisted.ActiveProvider != "openai" {
		t.Fatalf("active provider changed to %q", persisted.ActiveProvider)
	}
}

func TestSetProviderModelRejectsUnknownProviderWithoutRewriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	before := writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "openai",
		Providers: []ProviderProfile{
			{Name: "openai", ProviderKind: ProviderKindOpenAI, Model: "gpt-4.1"},
		},
	}, 0o600)

	_, err := SetProviderModel(path, "anthropic", "claude-sonnet-4.5")
	if err == nil {
		t.Fatal("SetProviderModel() error = nil, want error")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("config was rewritten for unknown provider\nbefore: %s\nafter: %s", before, after)
	}
}

func TestUpsertProviderTightensExistingConfigFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose POSIX mode bits reliably")
	}

	path := filepath.Join(t.TempDir(), "zero.json")
	if err := os.WriteFile(path, []byte(`{"providers":[]}`), 0o644); err != nil {
		t.Fatalf("write existing config: %v", err)
	}

	_, err := UpsertProvider(path, ProviderProfile{
		Name:         "openai",
		ProviderKind: ProviderKindOpenAI,
		APIKey:       "sk-test",
		Model:        "gpt-4.1",
	}, true)
	if err != nil {
		t.Fatalf("UpsertProvider() error = %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config mode = %o, want 0600", got)
	}
}

func TestSetFavoriteModelsPersistsUserPreferences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "openai",
		Providers: []ProviderProfile{
			{Name: "openai", ProviderKind: ProviderKindOpenAI, Model: "gpt-4.1"},
		},
	}, 0o600)

	cfg, err := SetFavoriteModels(path, []string{" qwen3-coder:480b ", "", "rnj-1:8b", "qwen3-coder:480b"})
	if err != nil {
		t.Fatalf("SetFavoriteModels() error = %v", err)
	}

	want := []string{"qwen3-coder:480b", "rnj-1:8b"}
	if !reflect.DeepEqual(cfg.Preferences.FavoriteModels, want) {
		t.Fatalf("FavoriteModels = %#v, want %#v", cfg.Preferences.FavoriteModels, want)
	}
	persisted := readConfigFixture(t, path)
	if !reflect.DeepEqual(persisted.Preferences.FavoriteModels, want) {
		t.Fatalf("persisted FavoriteModels = %#v, want %#v", persisted.Preferences.FavoriteModels, want)
	}
	if persisted.ActiveProvider != "openai" || len(persisted.Providers) != 1 {
		t.Fatalf("provider config was not preserved: %#v", persisted)
	}
}

func TestSetRecentModelsPersistsOrderDedupesAndCaps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "openai",
		Providers: []ProviderProfile{
			{Name: "openai", ProviderKind: ProviderKindOpenAI, Model: "gpt-4.1"},
		},
	}, 0o600)

	cfg, err := SetRecentModels(path, []RecentModelEntry{
		{Provider: " openrouter ", Model: " google/gemini-2.5-pro "},
		{Provider: "openrouter", Model: "minimax/minimax-m2.1"},
		{Provider: "openrouter", Model: "google/gemini-2.5-pro"}, // duplicate pair, older: dropped
		{Provider: "", Model: ""},                                // blank model: dropped
		{Provider: "openrouter", Model: "a"},
		{Provider: "openrouter", Model: "b"},
		{Provider: "openrouter", Model: "c"},
		{Provider: "openrouter", Model: "d"}, // beyond MaxRecentModels (5): dropped
	})
	if err != nil {
		t.Fatalf("SetRecentModels() error = %v", err)
	}

	want := []RecentModelEntry{
		{Provider: "openrouter", Model: "google/gemini-2.5-pro"},
		{Provider: "openrouter", Model: "minimax/minimax-m2.1"},
		{Provider: "openrouter", Model: "a"},
		{Provider: "openrouter", Model: "b"},
		{Provider: "openrouter", Model: "c"},
	}
	if !reflect.DeepEqual(cfg.Preferences.RecentModels, want) {
		t.Fatalf("RecentModels = %#v, want %#v", cfg.Preferences.RecentModels, want)
	}
	persisted := readConfigFixture(t, path)
	if !reflect.DeepEqual(persisted.Preferences.RecentModels, want) {
		t.Fatalf("persisted RecentModels = %#v, want %#v", persisted.Preferences.RecentModels, want)
	}
	if persisted.ActiveProvider != "openai" || len(persisted.Providers) != 1 {
		t.Fatalf("provider config was not preserved: %#v", persisted)
	}
}

// Two providers offering the same model id must both survive normalization —
// recent history de-duplicates by provider+model pair, not model id alone.
func TestSetRecentModelsDedupesByProviderAndModelPair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")

	cfg, err := SetRecentModels(path, []RecentModelEntry{
		{Provider: "provider-a", Model: "shared-model"},
		{Provider: "provider-b", Model: "shared-model"},
	})
	if err != nil {
		t.Fatalf("SetRecentModels() error = %v", err)
	}
	want := []RecentModelEntry{
		{Provider: "provider-a", Model: "shared-model"},
		{Provider: "provider-b", Model: "shared-model"},
	}
	if !reflect.DeepEqual(cfg.Preferences.RecentModels, want) {
		t.Fatalf("RecentModels = %#v, want both providers preserved: %#v", cfg.Preferences.RecentModels, want)
	}
}

func TestSetThemePersistsUserPreference(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "openai",
		Providers: []ProviderProfile{
			{Name: "openai", ProviderKind: ProviderKindOpenAI, Model: "gpt-4.1"},
		},
	}, 0o600)

	cfg, err := SetTheme(path, "  dracula  ")
	if err != nil {
		t.Fatalf("SetTheme() error = %v", err)
	}
	if cfg.Preferences.Theme != "dracula" {
		t.Fatalf("Theme = %q, want dracula (trimmed)", cfg.Preferences.Theme)
	}
	persisted := readConfigFixture(t, path)
	if persisted.Preferences.Theme != "dracula" {
		t.Fatalf("persisted Theme = %q, want dracula", persisted.Preferences.Theme)
	}
	if persisted.ActiveProvider != "openai" || len(persisted.Providers) != 1 {
		t.Fatalf("provider config was not preserved by SetTheme: %#v", persisted)
	}

	// A blank value clears the stored preference.
	if cfg, err = SetTheme(path, ""); err != nil {
		t.Fatalf("SetTheme(\"\") error = %v", err)
	}
	if cfg.Preferences.Theme != "" {
		t.Fatalf("SetTheme(\"\") should clear the theme, got %q", cfg.Preferences.Theme)
	}
}

func TestSetThemePreservesUnknownTopLevelFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	if err := os.WriteFile(path, []byte(`{
  "preferences": {"theme": "default"},
  "futureSetting": {"a": 1}
}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := SetTheme(path, "dracula"); err != nil {
		t.Fatalf("SetTheme() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted map[string]json.RawMessage
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	var futureSetting struct {
		A int `json:"a"`
	}
	if err := json.Unmarshal(persisted["futureSetting"], &futureSetting); err != nil {
		t.Fatalf("unknown field was not preserved: %v\nconfig: %s", err, data)
	}
	if futureSetting.A != 1 {
		t.Fatalf("futureSetting = %s, want {\"a\":1}", persisted["futureSetting"])
	}
}

// setNotify replaces the stored notify block with value through UpdateNotify
// and returns the re-read config, so the tests below can assert on a
// FileConfig-shaped result. Every product surface uses UpdateNotify's merge
// form directly; this replace form exists only for these tests.
func setNotify(path string, value NotifyConfig) (FileConfig, error) {
	next, err := UpdateNotify(path, func(NotifyConfig) NotifyConfig { return value })
	if err != nil {
		return FileConfig{}, err
	}
	cfg, err := loadConfigFile(path)
	if err != nil {
		return FileConfig{}, err
	}
	cfg.Notify = next
	return cfg, nil
}

func TestSetNotifyPersistsValidValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "openai",
		Providers: []ProviderProfile{
			{Name: "openai", ProviderKind: ProviderKindOpenAI, Model: "gpt-4.1"},
		},
	}, 0o600)

	cfg, err := setNotify(path, NotifyConfig{Mode: "  both  ", FocusMode: "  unfocused  "})
	if err != nil {
		t.Fatalf("setNotify() error = %v", err)
	}
	if cfg.Notify.Mode != "both" || cfg.Notify.FocusMode != "unfocused" {
		t.Fatalf("Notify = %+v, want mode=both focusMode=unfocused (trimmed)", cfg.Notify)
	}
	persisted := readConfigFixture(t, path)
	if persisted.Notify.Mode != "both" || persisted.Notify.FocusMode != "unfocused" {
		t.Fatalf("persisted Notify = %+v, want mode=both focusMode=unfocused", persisted.Notify)
	}
	if persisted.ActiveProvider != "openai" || len(persisted.Providers) != 1 {
		t.Fatalf("provider config was not preserved by setNotify: %#v", persisted)
	}
}

func TestSetNotifyRejectsInvalidMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	writeConfigFixture(t, path, FileConfig{ActiveProvider: "openai"}, 0o600)
	if _, err := setNotify(path, NotifyConfig{Mode: "loud", FocusMode: "unfocused"}); err == nil {
		t.Fatal("expected error for invalid notify.mode")
	}
}

func TestSetNotifyRejectsInvalidFocusMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	writeConfigFixture(t, path, FileConfig{ActiveProvider: "openai"}, 0o600)
	if _, err := setNotify(path, NotifyConfig{Mode: "off", FocusMode: "sideways"}); err == nil {
		t.Fatal("expected error for invalid notify.focusMode")
	}
}

func TestSetNotifyRejectsEmptyConfigPath(t *testing.T) {
	if _, err := setNotify("", NotifyConfig{Mode: "off"}); err == nil {
		t.Fatal("expected error for empty config path")
	}
}

func TestSetNotifyBlankValuesPreservedAsDefaults(t *testing.T) {
	// An empty mode/focusMode stored on disk is a valid "use the built-in
	// defaults" signal — setNotify must not reject blanks, and they must round
	// trip unchanged.
	path := filepath.Join(t.TempDir(), "zero.json")
	writeConfigFixture(t, path, FileConfig{ActiveProvider: "openai"}, 0o600)
	if _, err := setNotify(path, NotifyConfig{}); err != nil {
		t.Fatalf("setNotify({}) should accept blank values, got error: %v", err)
	}
	persisted := readConfigFixture(t, path)
	if persisted.Notify.Mode != "" || persisted.Notify.FocusMode != "" {
		t.Fatalf("blank notify values should round-trip, got %+v", persisted.Notify)
	}
}

// Maintainer regression (PR #1001): a notification save must preserve unrelated
// values whose EXPLICIT presence matters. The typed serializer's omitempty
// cannot round-trip tools.deferThreshold: 0 (explicit "never defer" vs unset
// "use default 3") or an MCP server's disabled: false (explicitly enabled), so
// UpdateNotify edits only the notify member's bytes instead. The
// assertions run against the RAW file, because reading through FileConfig
// would hide exactly the presence loss being tested.
func TestSetNotifyPreservesExplicitUnrelatedValues(t *testing.T) {
	dir := t.TempDir()

	for name, body := range map[string]string{
		"explicit zero deferThreshold": `{
	"tools": {"deferThreshold": 0},
	"mcp": {"servers": {"firecrawl": {"command": "npx", "disabled": false}}}
}`,
		"unknown keys stay untouched": `{
	"activeProvider": "openai",
	"customTopLevel": {"nested": [1, 2, 3]},
	"notify": {"mode": "both"}
}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name+".json")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}

			if _, err := setNotify(path, NotifyConfig{Mode: "off", FocusMode: "always"}); err != nil {
				t.Fatalf("setNotify: %v", err)
			}

			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(raw, &obj); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if strings.Contains(name, "explicit zero") {
				tools, ok := obj["tools"]
				if !ok || !strings.Contains(string(tools), `"deferThreshold": 0`) {
					t.Errorf("tools.deferThreshold: 0 lost through a notify write; tools = %s", string(tools))
				}
				mcp, ok := obj["mcp"]
				if !ok || !strings.Contains(string(mcp), `"disabled": false`) {
					t.Errorf("mcp disabled: false lost through a notify write; mcp = %s", string(mcp))
				}
			}
			if strings.Contains(name, "unknown keys") {
				if _, ok := obj["customTopLevel"]; !ok {
					t.Error("unknown top-level key lost through a notify write")
				}
			}
		})
	}
}

// Maintainer regression (PR #1001, CodeRabbit follow-up): a hand-edited config
// may contain DUPLICATE notify members (JSON decoders tolerate them and the
// last occurrence wins). A reset must remove EVERY notify member — removing
// only the last leaves the earlier block as the new effective preference, so
// `zero config notify --reset` would report success while the old value still
// applies.
func TestSetNotifyResetRemovesEveryDuplicateNotifyMember(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	duplicated := `{"notify":{"mode":"off"},"activeProvider":"openai","notify":{"mode":"bell"}}`
	if err := os.WriteFile(path, []byte(duplicated), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := setNotify(path, NotifyConfig{}); err != nil {
		t.Fatalf("setNotify({}) reset: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, still := obj["notify"]; still {
		t.Fatalf("reset left a notify member behind: %s", string(raw))
	}
	if obj["activeProvider"] == nil || string(obj["activeProvider"]) != `"openai"` {
		t.Errorf("unrelated member lost through the reset: %s", string(raw))
	}

	// A reset on a file with NO notify member is a no-op, and a partial update
	// against duplicates replaces the LAST member (the effective one under
	// last-occurrence-wins decoding).
	if err := os.WriteFile(path, []byte(duplicated), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := setNotify(path, NotifyConfig{Mode: "off", FocusMode: "always"}); err != nil {
		t.Fatalf("setNotify replace: %v", err)
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := UserNotify(path)
	if err != nil {
		t.Fatalf("UserNotify: %v", err)
	}
	if stored.Mode != "off" || stored.FocusMode != "always" {
		t.Fatalf("after replace, effective notify = %+v, want off/always (last member replaced)", stored)
	}
	if !strings.Contains(string(raw), `"activeProvider"`) {
		t.Errorf("unrelated member lost through the replace: %s", string(raw))
	}
}

// Maintainer regression (PR #1001): concurrent partial updates must not lose
// each other's explicit change. Two `zero config notify` calls (--mode off and
// --focus always) racing from both/unfocused previously interleaved their
// read-merge-write and one write silently undid the other (reproduced 50/50
// without the lock). UpdateNotify serializes the whole transaction.
func TestUpdateNotifyConcurrentPartialUpdatesLoseNoField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	if err := os.WriteFile(path, []byte(`{"notify":{"mode":"both","focusMode":"unfocused"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := UpdateNotify(path, func(current NotifyConfig) NotifyConfig {
			current.Mode = "off"
			return current
		}); err != nil {
			errs <- fmt.Errorf("mode update: %w", err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := UpdateNotify(path, func(current NotifyConfig) NotifyConfig {
			current.FocusMode = "always"
			return current
		}); err != nil {
			errs <- fmt.Errorf("focus update: %w", err)
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent update failed: %v", err)
	}

	stored, err := UserNotify(path)
	if err != nil {
		t.Fatalf("read stored: %v", err)
	}
	// BOTH explicit changes must survive: mode from writer 1, focus from
	// writer 2. The old race let the second writer's stale focus
	// ("unfocused") or stale mode ("both") win.
	if stored.Mode != "off" {
		t.Errorf("mode = %q, want off (concurrent focus update must not clobber it)", stored.Mode)
	}
	if stored.FocusMode != "always" {
		t.Errorf("focusMode = %q, want always (concurrent mode update must not clobber it)", stored.FocusMode)
	}
}

// UserNotify reads the notify block from the user's own file. Partial updates
// seed from this value so they preserve what the USER chose (blank included)
// instead of copying a project config's setting or a pinned default into the
// global file (maintainer review, PR #1001).
func TestUserNotify(t *testing.T) {
	dir := t.TempDir()

	// Missing file: zero value, no error.
	got, err := UserNotify(filepath.Join(dir, "missing.json"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if got.Mode != "" || got.FocusMode != "" {
		t.Fatalf("missing file = %+v, want zero value", got)
	}

	// Present block: trimmed values returned.
	path := filepath.Join(dir, "zero.json")
	writeConfigFixture(t, path, FileConfig{Notify: NotifyConfig{Mode: "  bell  ", FocusMode: " always "}}, 0o600)
	got, err = UserNotify(path)
	if err != nil {
		t.Fatalf("UserNotify: %v", err)
	}
	if got.Mode != "bell" || got.FocusMode != "always" {
		t.Fatalf("UserNotify = %+v, want bell/always (trimmed)", got)
	}

	// Blank path: zero value, no error.
	if got, err = UserNotify(""); err != nil || got.Mode != "" || got.FocusMode != "" {
		t.Fatalf("blank path = %+v err=%v, want zero value", got, err)
	}
}

func TestRecapsPreferenceRoundTrips(t *testing.T) {
	// Default (unset) is ON.
	if !(PreferencesConfig{}).RecapsEnabled() {
		t.Error("unset recaps should default to ON")
	}

	path := filepath.Join(t.TempDir(), "zero.json")
	writeConfigFixture(t, path, FileConfig{ActiveProvider: "openai"}, 0o600)

	// Persist OFF, then read it back.
	cfg, err := SetRecapsEnabled(path, false)
	if err != nil {
		t.Fatalf("SetRecapsEnabled(false) error = %v", err)
	}
	if cfg.Preferences.RecapsEnabled() {
		t.Error("after SetRecapsEnabled(false), RecapsEnabled() should be false")
	}
	persisted := readConfigFixture(t, path)
	if persisted.Preferences.Recaps == nil || *persisted.Preferences.Recaps {
		t.Errorf("persisted recaps should be explicit false, got %v", persisted.Preferences.Recaps)
	}
	if persisted.ActiveProvider != "openai" {
		t.Errorf("unrelated config must be preserved, got %q", persisted.ActiveProvider)
	}

	// Flip back ON — the write must succeed and persist an explicit true.
	cfg, err = SetRecapsEnabled(path, true)
	if err != nil {
		t.Fatalf("SetRecapsEnabled(true) error = %v", err)
	}
	if !cfg.Preferences.RecapsEnabled() {
		t.Error("after SetRecapsEnabled(true), RecapsEnabled() should be true")
	}
	if reread := readConfigFixture(t, path); reread.Preferences.Recaps == nil || !*reread.Preferences.Recaps {
		t.Errorf("re-enable should persist an explicit true, got %v", reread.Preferences.Recaps)
	}
}

func TestSetFavoriteModelsCreatesMissingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero", "config.json")

	cfg, err := SetFavoriteModels(path, []string{"glm-5.1"})
	if err != nil {
		t.Fatalf("SetFavoriteModels() error = %v", err)
	}

	if !reflect.DeepEqual(cfg.Preferences.FavoriteModels, []string{"glm-5.1"}) {
		t.Fatalf("FavoriteModels = %#v, want glm-5.1", cfg.Preferences.FavoriteModels)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected config file to be created: %v", err)
	}
}

func writeConfigFixture(t *testing.T, path string, cfg FileConfig, mode fs.FileMode) []byte {
	t.Helper()

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("encode config: %v", err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return data
}

func readConfigFixture(t *testing.T, path string) FileConfig {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg FileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	return cfg
}

func TestEnsureCatalogProviderCreatesProfileWithoutStealingActive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "opengateway",
		Providers: []ProviderProfile{
			{
				Name:         "opengateway",
				ProviderKind: ProviderKindOpenAICompatible,
				BaseURL:      "https://gateway.example.com/v1",
				APIKeyStored: true,
				Model:        "some-model",
			},
		},
	}, 0o600)

	ensured, err := EnsureCatalogProvider(path, "chatgpt")
	if err != nil {
		t.Fatalf("EnsureCatalogProvider: %v", err)
	}
	if !ensured.Created {
		t.Fatalf("expected profile to be created")
	}
	if ensured.Name != "chatgpt" {
		t.Fatalf("expected profile name chatgpt, got %q", ensured.Name)
	}
	if ensured.Active != "opengateway" {
		t.Fatalf("active provider must stay opengateway, got %q", ensured.Active)
	}

	cfg := readConfigFixture(t, path)
	if cfg.ActiveProvider != "opengateway" {
		t.Fatalf("persisted active provider changed to %q", cfg.ActiveProvider)
	}
	if len(cfg.Providers) != 2 {
		t.Fatalf("expected 2 providers, got %d", len(cfg.Providers))
	}
	chatgpt := cfg.Providers[1]
	if chatgpt.Name != "chatgpt" || chatgpt.CatalogID != "chatgpt" {
		t.Fatalf("unexpected created profile: %+v", chatgpt)
	}
	if chatgpt.Model == "" || chatgpt.BaseURL == "" {
		t.Fatalf("created profile must carry catalog defaults, got %+v", chatgpt)
	}
	if chatgpt.APIKey != "" || chatgpt.APIKeyStored {
		t.Fatalf("OAuth-created profile must stay keyless, got %+v", chatgpt)
	}
}

func TestEnsureCatalogProviderLeavesExistingProfileUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	original := FileConfig{
		ActiveProvider: "opengateway",
		Providers: []ProviderProfile{
			{Name: "opengateway", ProviderKind: ProviderKindOpenAICompatible, BaseURL: "https://gateway.example.com/v1", Model: "some-model"},
			// Renamed profile that already serves the chatgpt catalog entry.
			{Name: "codex", CatalogID: "chatgpt", Model: "gpt-5.5"},
		},
	}
	data := writeConfigFixture(t, path, original, 0o600)

	ensured, err := EnsureCatalogProvider(path, "chatgpt")
	if err != nil {
		t.Fatalf("EnsureCatalogProvider: %v", err)
	}
	if ensured.Created {
		t.Fatalf("existing profile must not be recreated")
	}
	if ensured.Name != "codex" {
		t.Fatalf("expected existing profile name codex, got %q", ensured.Name)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(after) != string(data) {
		t.Fatalf("config rewritten for a no-op ensure:\nbefore: %s\nafter: %s", data, after)
	}
}

func TestEnsureCatalogProviderActivatesOnEmptyConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")

	ensured, err := EnsureCatalogProvider(path, "chatgpt")
	if err != nil {
		t.Fatalf("EnsureCatalogProvider: %v", err)
	}
	if !ensured.Created {
		t.Fatalf("expected profile to be created")
	}
	if ensured.Active != "chatgpt" {
		t.Fatalf("blank active must adopt the new provider, got %q", ensured.Active)
	}
}

func TestEnsureCatalogProviderRejectsUnknownCatalogID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	if _, err := EnsureCatalogProvider(path, "no-such-provider"); err == nil {
		t.Fatalf("expected unknown catalog id to error")
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("config must not be written for an unknown catalog id")
	}
}

func TestMarkProviderAPIKeyStoredClearsInlineAndEnvKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "openrouter",
		Providers: []ProviderProfile{{
			Name:      "openrouter",
			CatalogID: "openrouter",
			APIKey:    "sk-inline",
			APIKeyEnv: "OPENROUTER_API_KEY",
			Model:     "openai/gpt-4.1",
		}},
	}, 0o600)

	if err := MarkProviderAPIKeyStored(path, "openrouter"); err != nil {
		t.Fatalf("MarkProviderAPIKeyStored: %v", err)
	}

	cfg := readConfigFixture(t, path)
	if len(cfg.Providers) != 1 {
		t.Fatalf("providers = %#v", cfg.Providers)
	}
	profile := cfg.Providers[0]
	if !profile.APIKeyStored || profile.APIKey != "" || profile.APIKeyEnv != "" {
		t.Fatalf("provider credential fields = %#v", profile)
	}
	if profile.Model != "openai/gpt-4.1" || cfg.ActiveProvider != "openrouter" {
		t.Fatalf("unrelated config changed: %#v", cfg)
	}
}

func TestRemoveProviderDeletesAndHandsOffActive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "beta",
		Providers: []ProviderProfile{
			{Name: "alpha", ProviderKind: ProviderKindOpenAICompatible, BaseURL: "https://a.example.com/v1", Model: "m1"},
			{Name: "beta", ProviderKind: ProviderKindOpenAICompatible, BaseURL: "https://b.example.com/v1", Model: "m2"},
		},
	}, 0o600)

	cfg, err := RemoveProvider(path, " beta ")
	if err != nil {
		t.Fatalf("RemoveProvider() error = %v", err)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "alpha" {
		t.Fatalf("expected only alpha to remain, got %+v", cfg.Providers)
	}
	if cfg.ActiveProvider != "alpha" {
		t.Fatalf("active must hand off to the first remaining provider, got %q", cfg.ActiveProvider)
	}

	persisted := readConfigFixture(t, path)
	if len(persisted.Providers) != 1 || persisted.ActiveProvider != "alpha" {
		t.Fatalf("persisted config wrong: %+v", persisted)
	}

	// Removing the last provider clears the active pointer entirely.
	cfg, err = RemoveProvider(path, "alpha")
	if err != nil {
		t.Fatalf("RemoveProvider(last) error = %v", err)
	}
	if len(cfg.Providers) != 0 || cfg.ActiveProvider != "" {
		t.Fatalf("expected empty providers and no active, got %+v", cfg)
	}
}

func TestRemoveProviderKeepsActiveWhenOtherRemoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "alpha",
		Providers: []ProviderProfile{
			{Name: "alpha", ProviderKind: ProviderKindOpenAICompatible, BaseURL: "https://a.example.com/v1", Model: "m1"},
			{Name: "beta", ProviderKind: ProviderKindOpenAICompatible, BaseURL: "https://b.example.com/v1", Model: "m2"},
		},
	}, 0o600)

	cfg, err := RemoveProvider(path, "beta")
	if err != nil {
		t.Fatalf("RemoveProvider() error = %v", err)
	}
	if cfg.ActiveProvider != "alpha" {
		t.Fatalf("active provider must be untouched, got %q", cfg.ActiveProvider)
	}
}

func TestRemoveProviderRejectsUnknownWithoutRewriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	before := writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "alpha",
		Providers:      []ProviderProfile{{Name: "alpha", ProviderKind: ProviderKindOpenAICompatible, BaseURL: "https://a.example.com/v1", Model: "m1"}},
	}, 0o600)

	if _, err := RemoveProvider(path, "ghost"); err == nil {
		t.Fatal("RemoveProvider() error = nil, want not-found error")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("config was rewritten for unknown provider")
	}
}

func TestRenameProviderFollowsActiveAndMigratesStoredKey(t *testing.T) {
	dir := t.TempDir()
	// Force the file credential backend so the test never touches the real OS
	// keyring regardless of platform.
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	path := filepath.Join(dir, "config.json")
	writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "oldname",
		Providers: []ProviderProfile{
			{Name: "oldname", ProviderKind: ProviderKindOpenAICompatible, BaseURL: "https://a.example.com/v1", APIKeyStored: true, Model: "m1"},
			{Name: "other", ProviderKind: ProviderKindOpenAICompatible, BaseURL: "https://b.example.com/v1", Model: "m2"},
		},
	}, 0o600)
	store, err := ProviderKeyStoreAt(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.Set("oldname", "sk-secret"); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	cfg, err := RenameProvider(path, "oldname", "newname")
	if err != nil {
		t.Fatalf("RenameProvider() error = %v", err)
	}
	if cfg.ActiveProvider != "newname" {
		t.Fatalf("active must follow the rename, got %q", cfg.ActiveProvider)
	}
	if cfg.Providers[0].Name != "newname" || !cfg.Providers[0].APIKeyStored {
		t.Fatalf("renamed profile wrong: %+v", cfg.Providers[0])
	}
	if cfg.Providers[1].Name != "other" {
		t.Fatalf("unrelated profile changed: %+v", cfg.Providers[1])
	}

	key, ok, err := store.Get("newname")
	if err != nil || !ok || key != "sk-secret" {
		t.Fatalf("stored key must migrate to the new name, got key=%q ok=%v err=%v", key, ok, err)
	}
	if _, ok, _ := store.Get("oldname"); ok {
		t.Fatalf("old credential entry must be deleted after migration")
	}
}

func TestRenameProviderRejectsCollisionAndUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	before := writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "alpha",
		Providers: []ProviderProfile{
			{Name: "alpha", ProviderKind: ProviderKindOpenAICompatible, BaseURL: "https://a.example.com/v1", Model: "m1"},
			{Name: "beta", ProviderKind: ProviderKindOpenAICompatible, BaseURL: "https://b.example.com/v1", Model: "m2"},
		},
	}, 0o600)

	if _, err := RenameProvider(path, "alpha", "BETA"); err == nil {
		t.Fatal("rename onto an existing name must fail")
	}
	if _, err := RenameProvider(path, "ghost", "gamma"); err == nil {
		t.Fatal("renaming an unknown provider must fail")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("config was rewritten by a rejected rename")
	}
}

func TestUpsertProviderPreservesStoredKeyMarkerOnExistingProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	// An env-keyed profile with NO stored-key marker — the shape a provider has
	// before its key is captured into the credential store.
	writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "groq",
		Providers: []ProviderProfile{
			{Name: "groq", ProviderKind: ProviderKindOpenAICompatible, BaseURL: "https://api.groq.com/openai/v1", APIKeyEnv: "GROQ_API_KEY", Model: "m1"},
		},
	}, 0o600)

	// The manager/setup edit paths persist a SecureProviderProfile-shaped
	// profile: key already in the store, marker set, inline key cleared.
	cfg, err := UpsertProvider(path, ProviderProfile{Name: "groq", APIKeyStored: true}, false)
	if err != nil {
		t.Fatalf("UpsertProvider() error = %v", err)
	}
	if !cfg.Providers[0].APIKeyStored {
		t.Fatalf("APIKeyStored marker must survive the merge, got %+v", cfg.Providers[0])
	}
	if cfg.Providers[0].APIKeyEnv != "GROQ_API_KEY" || cfg.Providers[0].BaseURL == "" {
		t.Fatalf("other fields must be preserved: %+v", cfg.Providers[0])
	}
	persisted := readConfigFixture(t, path)
	if !persisted.Providers[0].APIKeyStored {
		t.Fatalf("marker not persisted to disk: %+v", persisted.Providers[0])
	}
}

func TestSetProviderDescriptionSetsAndClears(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "alpha",
		Providers: []ProviderProfile{
			{Name: "alpha", ProviderKind: ProviderKindOpenAICompatible, BaseURL: "https://a.example.com/v1", Model: "m1", Description: "old text"},
		},
	}, 0o600)

	cfg, err := SetProviderDescription(path, " ALPHA ", "new text")
	if err != nil {
		t.Fatalf("SetProviderDescription() error = %v", err)
	}
	if cfg.Providers[0].Description != "new text" {
		t.Fatalf("description not set: %+v", cfg.Providers[0])
	}

	// Clearing must persist too — the reason this setter exists (UpsertProvider's
	// merge treats an empty description as "leave unchanged").
	cfg, err = SetProviderDescription(path, "alpha", "  ")
	if err != nil {
		t.Fatalf("SetProviderDescription(clear) error = %v", err)
	}
	if cfg.Providers[0].Description != "" {
		t.Fatalf("description not cleared: %+v", cfg.Providers[0])
	}
	persisted := readConfigFixture(t, path)
	if persisted.Providers[0].Description != "" {
		t.Fatalf("cleared description not persisted: %+v", persisted.Providers[0])
	}

	if _, err := SetProviderDescription(path, "ghost", "x"); err == nil {
		t.Fatal("unknown provider must error")
	}
}

func TestRenameProviderRollsBackKeyMigrationWhenConfigWriteFails(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("uses chflags uchg to force the config write to fail; macOS only")
	}
	dir := t.TempDir()
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	path := filepath.Join(dir, "config.json")
	writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "oldname",
		Providers: []ProviderProfile{
			{Name: "oldname", ProviderKind: ProviderKindOpenAICompatible, BaseURL: "https://a.example.com/v1", APIKeyStored: true, Model: "m1"},
		},
	}, 0o600)
	store, err := ProviderKeyStoreAt(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.Set("oldname", "sk-secret"); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	// Immutable flag: temp-file creation and store writes in the directory keep
	// working, but the final rename over config.json fails — the exact window
	// where a migrated key would otherwise strand under the new name.
	if out, err := exec.Command("chflags", "uchg", path).CombinedOutput(); err != nil {
		t.Skipf("chflags uchg unavailable: %v (%s)", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("chflags", "nouchg", path).Run() })

	if _, err := RenameProvider(path, "oldname", "newname"); err == nil {
		t.Fatal("expected the config write to fail under the immutable flag")
	}

	key, ok, err := store.Get("oldname")
	if err != nil || !ok || key != "sk-secret" {
		t.Fatalf("key must be rolled back to the old name, got key=%q ok=%v err=%v", key, ok, err)
	}
	if _, ok, _ := store.Get("newname"); ok {
		t.Fatalf("rolled-back migration must not leave a key under the new name")
	}
}

// TestRenameProviderCaseOnlyKeepsStoredKey: the credential store normalizes
// names case-insensitively, so a case-only rename targets ONE store entry —
// migrating would Set and then Delete the same key, losing it (PR #560 review).
func TestRenameProviderCaseOnlyKeepsStoredKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	path := filepath.Join(dir, "config.json")
	writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "groq",
		Providers: []ProviderProfile{
			{Name: "groq", ProviderKind: ProviderKindOpenAICompatible, BaseURL: "https://api.groq.com/openai/v1", APIKeyStored: true, Model: "m1"},
		},
	}, 0o600)
	store, err := ProviderKeyStoreAt(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.Set("groq", "sk-secret"); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	cfg, err := RenameProvider(path, "groq", "Groq")
	if err != nil {
		t.Fatalf("RenameProvider() error = %v", err)
	}
	if cfg.Providers[0].Name != "Groq" || cfg.ActiveProvider != "Groq" {
		t.Fatalf("case-only rename must still apply to config: %+v", cfg)
	}
	// The store is case-insensitive: the key must remain retrievable under the
	// new casing (same entry), not deleted by a same-entry "migration".
	if key, ok, err := store.Get("Groq"); err != nil || !ok || key != "sk-secret" {
		t.Fatalf("stored key lost on case-only rename: key=%q ok=%v err=%v", key, ok, err)
	}
}

func TestEditProviderAppliesRenameFieldsAndDescriptionAtomically(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	path := filepath.Join(dir, "config.json")
	writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "groq",
		Providers: []ProviderProfile{
			{Name: "groq", ProviderKind: ProviderKindOpenAICompatible, BaseURL: "https://api.groq.com/openai/v1", APIKeyStored: true, Model: "m1", Description: "old text"},
			{Name: "other", ProviderKind: ProviderKindOpenAICompatible, BaseURL: "https://o.example.com/v1", Model: "m2"},
		},
	}, 0o600)
	store, err := ProviderKeyStoreAt(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.Set("groq", "sk-old"); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	cfg, err := EditProvider(path, ProviderEdit{
		Name:        "groq",
		NewName:     "grok-main",
		Model:       "m1-pro",
		Description: "", // cleared — must persist verbatim
	})
	if err != nil {
		t.Fatalf("EditProvider() error = %v", err)
	}
	edited := cfg.Providers[0]
	if edited.Name != "grok-main" || edited.Model != "m1-pro" || edited.Description != "" {
		t.Fatalf("edit not applied: %+v", edited)
	}
	if edited.BaseURL != "https://api.groq.com/openai/v1" || !edited.APIKeyStored {
		t.Fatalf("untouched fields must survive: %+v", edited)
	}
	if cfg.ActiveProvider != "grok-main" {
		t.Fatalf("active must follow the rename, got %q", cfg.ActiveProvider)
	}
	if key, ok, _ := store.Get("grok-main"); !ok || key != "sk-old" {
		t.Fatalf("stored key must migrate with the rename, got %q ok=%v", key, ok)
	}
	if len(cfg.Providers) != 2 || cfg.Providers[1].Name != "other" {
		t.Fatalf("unrelated profile changed: %+v", cfg.Providers)
	}
}

// TestEditProviderCaseOnlyRenameUpdatesInPlace: the manager previously skipped
// RenameProvider on case-insensitively-equal names and fell into UpsertProvider,
// whose case-SENSITIVE merge appended a duplicate profile. EditProvider applies
// NewName to the exact current profile, so a case-only rename is an in-place
// update and the store entry (case-normalized) survives.
func TestEditProviderCaseOnlyRenameUpdatesInPlace(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	path := filepath.Join(dir, "config.json")
	writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "groq",
		Providers: []ProviderProfile{
			{Name: "groq", ProviderKind: ProviderKindOpenAICompatible, BaseURL: "https://api.groq.com/openai/v1", APIKeyStored: true, Model: "m1"},
		},
	}, 0o600)
	store, err := ProviderKeyStoreAt(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.Set("groq", "sk-secret"); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	cfg, err := EditProvider(path, ProviderEdit{Name: "groq", NewName: "Groq"})
	if err != nil {
		t.Fatalf("EditProvider() error = %v", err)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("case-only rename must not duplicate the profile: %+v", cfg.Providers)
	}
	if cfg.Providers[0].Name != "Groq" || !cfg.Providers[0].APIKeyStored {
		t.Fatalf("in-place update wrong: %+v", cfg.Providers[0])
	}
	if cfg.Providers[0].BaseURL != "https://api.groq.com/openai/v1" {
		t.Fatalf("fields must survive a case-only rename: %+v", cfg.Providers[0])
	}
	if cfg.ActiveProvider != "Groq" {
		t.Fatalf("active must follow, got %q", cfg.ActiveProvider)
	}
	if key, ok, _ := store.Get("Groq"); !ok || key != "sk-secret" {
		t.Fatalf("stored key lost on case-only rename: %q ok=%v", key, ok)
	}
}

// TestEditProviderRenameMigratesFreshlyCapturedKey: replacing the key AND
// renaming in one edit — the caller captures under the CURRENT name and the
// rename migration moves it, so the new key lands under the new name.
func TestEditProviderRenameMigratesFreshlyCapturedKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	path := filepath.Join(dir, "config.json")
	writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "gw",
		Providers: []ProviderProfile{
			{Name: "gw", ProviderKind: ProviderKindOpenAICompatible, BaseURL: "https://gw.example.com/v1", APIKeyEnv: "GW_KEY", Model: "m1"},
		},
	}, 0o600)
	store, err := ProviderKeyStoreAt(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// The caller's contract: a replacement key is stored under the CURRENT name
	// before EditProvider runs (what SecureProviderProfile does).
	if err := store.Set("gw", "sk-new"); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	cfg, err := EditProvider(path, ProviderEdit{Name: "gw", NewName: "gateway", APIKeyStored: true})
	if err != nil {
		t.Fatalf("EditProvider() error = %v", err)
	}
	if !cfg.Providers[0].APIKeyStored || cfg.Providers[0].Name != "gateway" {
		t.Fatalf("marker/rename wrong: %+v", cfg.Providers[0])
	}
	if key, ok, _ := store.Get("gateway"); !ok || key != "sk-new" {
		t.Fatalf("captured key must migrate to the new name, got %q ok=%v", key, ok)
	}
	if _, ok, _ := store.Get("gw"); ok {
		t.Fatalf("old entry must be cleaned up after migration")
	}
}

func TestEditProviderRejectsCollisionAndUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	before := writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "alpha",
		Providers: []ProviderProfile{
			{Name: "alpha", ProviderKind: ProviderKindOpenAICompatible, BaseURL: "https://a.example.com/v1", Model: "m1"},
			{Name: "beta", ProviderKind: ProviderKindOpenAICompatible, BaseURL: "https://b.example.com/v1", Model: "m2"},
		},
	}, 0o600)

	if _, err := EditProvider(path, ProviderEdit{Name: "alpha", NewName: "BETA"}); err == nil {
		t.Fatal("rename onto an existing name must fail")
	}
	if _, err := EditProvider(path, ProviderEdit{Name: "ghost", Model: "x"}); err == nil {
		t.Fatal("editing an unknown provider must fail")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("config was rewritten by a rejected edit")
	}
}

func TestUpsertProviderRejectsCaseVariantWithoutRewritingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	before := writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "work",
		Providers: []ProviderProfile{
			{Name: "work", ProviderKind: ProviderKindOpenAICompatible, BaseURL: "https://work.example/v1", Model: "m1"},
		},
	}, 0o600)

	_, err := UpsertProvider(path, ProviderProfile{Name: "WORK", Model: "m2"}, false)
	if err == nil || !strings.Contains(err.Error(), `provider "WORK" already exists as "work"`) {
		t.Fatalf("UpsertProvider() error = %v, want case-variant collision", err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read config: %v", readErr)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("rejected upsert rewrote config\nbefore: %s\nafter: %s", before, after)
	}
}

func TestSetActiveProviderUsesCredentialIdentityWithoutUnicodeFolding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "ſ",
		Providers: []ProviderProfile{
			{Name: "s", ProviderKind: ProviderKindOpenAI, Model: "gpt-4.1"},
			{Name: "ſ", ProviderKind: ProviderKindOpenAI, Model: "gpt-4.1"},
		},
	}, 0o600)

	cfg, err := SetActiveProvider(path, "S")
	if err != nil {
		t.Fatalf("SetActiveProvider() error = %v", err)
	}
	if cfg.ActiveProvider != "s" {
		t.Fatalf("ActiveProvider = %q, want exact persisted spelling s", cfg.ActiveProvider)
	}
}

func TestMarkProviderAPIKeyStoredRequiresExactProviderIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	before := writeConfigFixture(t, path, FileConfig{Providers: []ProviderProfile{{Name: "work", APIKeyEnv: "WORK_KEY"}}}, 0o600)
	if err := MarkProviderAPIKeyStored(path, "WORK"); err == nil || !strings.Contains(err.Error(), `provider "WORK" not found`) {
		t.Fatalf("MarkProviderAPIKeyStored() error = %v, want exact-case not-found", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("case-variant mark rewrote config")
	}
}

func TestProviderPersistedUsesCredentialIdentityWithoutUnicodeFolding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	writeConfigFixture(t, path, FileConfig{Providers: []ProviderProfile{{Name: "s"}}}, 0o600)

	persisted, err := ProviderPersisted(path, "S")
	if err != nil {
		t.Fatalf("ProviderPersisted() error = %v", err)
	}
	if !persisted {
		t.Fatal("ProviderPersisted() = false for case-variant credential identity")
	}
	persisted, err = ProviderPersisted(path, "ſ")
	if err != nil {
		t.Fatalf("ProviderPersisted(long-s) error = %v", err)
	}
	if persisted {
		t.Fatal("ProviderPersisted() conflated s with Unicode long-s")
	}
}

// Same scenario as RemoveProvider/RenameProvider: two rows differing only by
// case must not let SetProviderModel update the wrong one.
func TestSetProviderModelRequiresExactProviderIdentityAmongCaseVariants(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	before := writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "work",
		Providers: []ProviderProfile{
			{Name: "work", ProviderKind: ProviderKindOpenAICompatible, Model: "m1"},
			{Name: "WORK", ProviderKind: ProviderKindOpenAICompatible, Model: "m2"},
		},
	}, 0o600)

	_, err := SetProviderModel(path, "WORK", "m2-updated")
	assertAmbiguousConfigUnchanged(t, path, before, err, "work", "WORK")
}

func TestProviderMutatorsHandOffCaseVariantActiveProvider(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(string) (FileConfig, error)
		wantActive string
		wantName   string
	}{
		{name: "remove", mutate: func(path string) (FileConfig, error) { return RemoveProvider(path, "work") }},
		{name: "rename", mutate: func(path string) (FileConfig, error) { return RenameProvider(path, "work", "office") }, wantActive: "office", wantName: "office"},
		{name: "edit", mutate: func(path string) (FileConfig, error) {
			return EditProvider(path, ProviderEdit{Name: "work", NewName: "office", Model: "updated"})
		}, wantActive: "office", wantName: "office"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			writeConfigFixture(t, path, FileConfig{ActiveProvider: "WORK", Providers: []ProviderProfile{{Name: "work", Model: "old"}}}, 0o600)
			cfg, err := test.mutate(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.ActiveProvider != test.wantActive {
				t.Fatalf("activeProvider = %q, want %q", cfg.ActiveProvider, test.wantActive)
			}
			if test.wantName == "" && len(cfg.Providers) != 0 {
				t.Fatalf("providers = %+v, want none", cfg.Providers)
			}
			if test.wantName != "" && (len(cfg.Providers) != 1 || cfg.Providers[0].Name != test.wantName) {
				t.Fatalf("providers = %+v, want canonical name %q", cfg.Providers, test.wantName)
			}
		})
	}
}

// UpsertProvider merges by exact name, so a config file can end up with two
// rows that differ only by case (e.g. one saved as "work", another later
// saved as "WORK"). RemoveProvider must delete the exact row the caller
// named, not whichever case-variant sorts first.
func TestRemoveProviderRequiresExactProviderIdentityAmongCaseVariants(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "work",
		Providers: []ProviderProfile{
			{Name: "work", ProviderKind: ProviderKindOpenAICompatible, BaseURL: "https://a.example.com/v1", Model: "m1"},
			{Name: "WORK", ProviderKind: ProviderKindOpenAICompatible, BaseURL: "https://b.example.com/v1", Model: "m2"},
		},
	}, 0o600)

	cfg, err := RemoveProvider(path, "WORK")
	if err != nil {
		t.Fatalf("exact removal should repair case duplicates: %v", err)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "work" || cfg.ActiveProvider != "work" {
		t.Fatalf("repaired config = %+v", cfg)
	}
}

func TestRemoveProviderRejectsNonExactCaseDuplicateTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	before := writeConfigFixture(t, path, FileConfig{Providers: []ProviderProfile{{Name: "work"}, {Name: "WORK"}}}, 0o600)
	_, err := RemoveProvider(path, "WoRk")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error = %v, want exact-target not-found error", err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(after, before) {
		t.Fatalf("rejected removal rewrote config: readErr=%v", readErr)
	}
}

func TestRemoveProviderPublishesRepairThatReducesRemainingAmbiguity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	writeConfigFixture(t, path, FileConfig{Providers: []ProviderProfile{{Name: "work"}, {Name: "WORK"}, {Name: "Work"}}}, 0o600)
	cfg, err := RemoveProvider(path, "Work")
	if err != nil {
		t.Fatalf("strictly reducing repair failed: %v", err)
	}
	if len(cfg.Providers) != 2 || cfg.Providers[0].Name != "work" || cfg.Providers[1].Name != "WORK" {
		t.Fatalf("repaired config = %+v", cfg)
	}
}

func TestRemoveProviderPublishesRepairWhileUnnamedProblemRemains(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	writeConfigFixture(t, path, FileConfig{Providers: []ProviderProfile{{Name: ""}, {Name: "work"}, {Name: "WORK"}}}, 0o600)
	cfg, err := RemoveProvider(path, "WORK")
	if err != nil {
		t.Fatalf("exact duplicate repair failed while an unnamed row remained: %v", err)
	}
	if len(cfg.Providers) != 2 || cfg.Providers[0].Name != "" || cfg.Providers[1].Name != "work" {
		t.Fatalf("repaired config = %+v", cfg)
	}
	if err := ValidatePersistedProviderNames(cfg); err == nil || !strings.Contains(err.Error(), "cannot be empty") {
		t.Fatalf("repair should leave only the independent unnamed-row problem, got %v", err)
	}
}

func TestRepairUnnamedProviderRejectsRepairThatIntroducesDuplicate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	before := writeConfigFixture(t, path, FileConfig{Providers: []ProviderProfile{{Name: ""}, {Name: "work"}}}, 0o600)
	_, _, err := RepairUnnamedProvider(path, "WORK")
	// The collision is now caught BEFORE the candidate config is built, so the
	// message names the row that owns the identity instead of reporting an
	// "ambiguous persisted provider names" state the file never had. The
	// rejection and the untouched file are unchanged.
	if err == nil || !strings.Contains(err.Error(), `persisted provider "work" already uses that identity`) {
		t.Fatalf("error = %v, want a collision rejection naming the owning row", err)
	}
	if !strings.Contains(err.Error(), "--name") {
		t.Fatalf("error = %v, want the escape flag named", err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(after, before) {
		t.Fatalf("rejected repair rewrote config: readErr=%v", readErr)
	}
}

func TestRemoveProviderKeepsExactActiveCaseVariant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "work",
		Providers:      []ProviderProfile{{Name: "alpha"}, {Name: "work"}, {Name: "WORK"}},
	}, 0o600)

	cfg, err := RemoveProvider(path, "WORK")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ActiveProvider != "work" {
		t.Fatalf("activeProvider = %q, want exact surviving row work", cfg.ActiveProvider)
	}
}

// Same scenario as RemoveProvider: two rows differing only by case must not
// let RenameProvider act on the wrong one.
func TestRenameProviderRequiresExactProviderIdentityAmongCaseVariants(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	before := writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "work",
		Providers: []ProviderProfile{
			{Name: "work", ProviderKind: ProviderKindOpenAICompatible, BaseURL: "https://a.example.com/v1", Model: "m1"},
			{Name: "WORK", ProviderKind: ProviderKindOpenAICompatible, BaseURL: "https://b.example.com/v1", Model: "m2"},
		},
	}, 0o600)

	_, err := RenameProvider(path, "WORK", "renamed")
	assertAmbiguousConfigUnchanged(t, path, before, err, "work", "WORK")
}

func TestEditProviderRequiresExactProviderIdentityAmongCaseVariants(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	before := writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "work",
		Providers: []ProviderProfile{
			{Name: "WORK", ProviderKind: ProviderKindOpenAICompatible, BaseURL: "https://upper.example.com/v1", Model: "upper"},
			{Name: "work", ProviderKind: ProviderKindOpenAICompatible, BaseURL: "https://lower.example.com/v1", Model: "lower"},
		},
	}, 0o600)

	_, err := EditProvider(path, ProviderEdit{Name: "WORK", NewName: "renamed", Model: "updated"})
	assertAmbiguousConfigUnchanged(t, path, before, err, "WORK", "work")
}

func assertAmbiguousConfigUnchanged(t *testing.T, path string, before []byte, err error, first, second string) {
	t.Helper()
	// The message must name the repair command: this rejection reaches the user
	// at config read time, where it blocks interactive startup entirely.
	want := fmt.Sprintf("ambiguous persisted provider names %q and %q differ only by case; run `zero providers remove %s` (exact spelling) or rename one row in config.json", first, second, second)
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("ambiguous mutation rewrote config\nbefore: %s\nafter: %s", before, after)
	}
}

// TestValidatePersistedProviderNamesRejectsExactDuplicates covers jatmn's #725
// finding: the validator only rejected a repeated folded name when the
// SPELLINGS differed, so two rows literally named "work" passed. That breaks
// the same one-credential-per-folded-name invariant the case check protects —
// resolver merging coalesces the rows, and plaintext-key migration writes both
// values into one normalized credential-store entry, overwriting the first key.
func TestValidatePersistedProviderNamesRejectsExactDuplicates(t *testing.T) {
	for name, providers := range map[string][]ProviderProfile{
		"identical spellings": {{Name: "work"}, {Name: "work"}},
		"same after trimming": {{Name: "work"}, {Name: "  work  "}},
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidatePersistedProviderNames(FileConfig{Providers: providers})
			if err == nil {
				t.Fatal("a repeated folded provider identity must be rejected")
			}
			if want := `duplicate persisted provider name "work"`; !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %v, want it to contain %q", err, want)
			}
		})
	}
	if err := ValidatePersistedProviderNames(FileConfig{Providers: []ProviderProfile{{Name: "work"}, {Name: "fast"}}}); err != nil {
		t.Fatalf("distinct names must validate: %v", err)
	}
}

func TestValidatePersistedProviderNamesRejectsImplicitOpenAICollision(t *testing.T) {
	err := ValidatePersistedProviderNames(FileConfig{Providers: []ProviderProfile{
		{Name: ""},
		{Name: "openai"},
	}})
	if err == nil || !strings.Contains(err.Error(), "persisted provider name cannot be empty") {
		t.Fatalf("error = %v, want empty persisted-provider name rejection", err)
	}
}

func TestRepairUnnamedProviderPreservesLegacyNameResolution(t *testing.T) {
	t.Run("active provider", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		writeConfigFixture(t, path, FileConfig{
			ActiveProvider: "work",
			Providers:      []ProviderProfile{{Name: "  ", Model: "legacy-model"}},
			MaxTurns:       17,
		}, 0o600)
		cfg, _, err := RepairUnnamedProvider(path, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "work" || cfg.Providers[0].Model != "legacy-model" || cfg.MaxTurns != 17 {
			t.Fatalf("repaired config = %+v", cfg)
		}
		if err := ValidatePersistedProviderNames(cfg); err != nil {
			t.Fatalf("repaired config remains invalid: %v", err)
		}
	})

	t.Run("explicit name migrates legacy active reference", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		writeConfigFixture(t, path, FileConfig{
			ActiveProvider: "legacy",
			Providers: []ProviderProfile{
				{Name: "", ProviderKind: ProviderKindOpenAI, Model: "gpt-4o"},
				{Name: "other", ProviderKind: ProviderKindOpenAI, Model: "gpt-4.1"},
			},
		}, 0o600)
		cfg, _, err := RepairUnnamedProvider(path, "work")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ActiveProvider != "work" {
			t.Fatalf("active provider = %q, want repaired name work", cfg.ActiveProvider)
		}
		resolved, err := Resolve(ResolveOptions{UserConfigPath: path, Env: map[string]string{}})
		if err != nil {
			t.Fatalf("Resolve after repair: %v", err)
		}
		if resolved.ActiveProvider != "work" || resolved.Provider.Name != "work" {
			t.Fatalf("resolved active provider = %q profile = %q, want work", resolved.ActiveProvider, resolved.Provider.Name)
		}
	})

	t.Run("openai fallback", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		writeConfigFixture(t, path, FileConfig{Providers: []ProviderProfile{{Model: "gpt-4o"}}}, 0o600)
		cfg, _, err := RepairUnnamedProvider(path, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "openai" {
			t.Fatalf("repaired config = %+v, want openai", cfg)
		}
	})
}

func TestRepairUnnamedProviderRejectsAmbiguousRepairWithoutWriting(t *testing.T) {
	for name, cfg := range map[string]FileConfig{
		"name collision":   {Providers: []ProviderProfile{{Name: ""}, {Name: "OPENAI"}}},
		"multiple unnamed": {Providers: []ProviderProfile{{Name: ""}, {Name: "  "}}},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			before := writeConfigFixture(t, path, cfg, 0o600)
			if _, _, err := RepairUnnamedProvider(path, ""); err == nil {
				t.Fatal("ambiguous repair succeeded")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, before) {
				t.Fatalf("rejected repair changed config\nbefore: %s\nafter: %s", before, after)
			}
		})
	}
}

func TestRepairUnnamedProviderAllowsExplicitUniqueName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfigFixture(t, path, FileConfig{Providers: []ProviderProfile{{Name: ""}, {Name: "OPENAI"}}}, 0o600)
	cfg, _, err := RepairUnnamedProvider(path, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers[0].Name != "legacy" {
		t.Fatalf("repaired name = %q, want legacy", cfg.Providers[0].Name)
	}
}

func TestRepairUnnamedStoredProviderCaseCollisionExplainsManualRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	before := writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "legacy",
		Providers: []ProviderProfile{
			{APIKeyStored: true},
			{Name: "LEGACY"},
		},
	}, 0o600)
	for _, name := range []string{"", "work", "Legacy"} {
		_, _, err := RepairUnnamedProvider(path, name)
		if err == nil || !strings.Contains(err.Error(), `manually rename the sibling "LEGACY" in config.json`) {
			t.Fatalf("repair with name %q must explain the manual recovery: %v", name, err)
		}
		after, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(before, after) {
			t.Fatal("refused repair changed config")
		}
	}
}

func TestEnsureCatalogProviderValidatesBeforeExistingProfileShortcut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	before := writeConfigFixture(t, path, FileConfig{Providers: []ProviderProfile{{Name: "xai"}, {Name: "XAI"}}}, 0o600)
	if _, err := EnsureCatalogProvider(path, "xai"); err == nil || !strings.Contains(err.Error(), "ambiguous persisted provider names") {
		t.Fatalf("EnsureCatalogProvider error = %v, want ambiguous config rejection", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("rejected ensure changed config: readErr=%v", err)
	}
}

func TestResolvePersistedProviderNameBridgesIdentityToExactSpelling(t *testing.T) {
	cases := []struct {
		name      string
		providers []ProviderProfile
		input     string
		want      string
		wantErr   string
	}{
		{
			name:      "exact spelling",
			providers: []ProviderProfile{{Name: "OpenAI"}},
			input:     "OpenAI",
			want:      "OpenAI",
		},
		{
			name:      "case variant resolves to the row's own spelling",
			providers: []ProviderProfile{{Name: "WORK"}},
			input:     "work",
			want:      "WORK",
		},
		{
			name:      "exact spelling wins over an earlier case variant",
			providers: []ProviderProfile{{Name: "WORK"}, {Name: "work"}},
			input:     "work",
			want:      "work",
		},
		{
			name:      "ambiguous identity is an error, not an arbitrary pick",
			providers: []ProviderProfile{{Name: "WORK"}, {Name: "Work"}},
			input:     "work",
			wantErr:   "ambiguous provider",
		},
		{
			// The credential store keeps "s" and Unicode long-s apart, so these
			// are two identities and neither resolves the other.
			name:      "unicode long-s is a distinct identity",
			providers: []ProviderProfile{{Name: "ſ"}},
			input:     "s",
			wantErr:   "not found",
		},
		{
			name:      "unknown name",
			providers: []ProviderProfile{{Name: "openai"}},
			input:     "anthropic",
			wantErr:   "not found",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "zero.json")
			writeConfigFixture(t, path, FileConfig{Providers: testCase.providers}, 0o600)
			got, err := ResolvePersistedProviderName(path, testCase.input)
			if testCase.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, testCase.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != testCase.want {
				t.Fatalf("resolved = %q, want %q", got, testCase.want)
			}
		})
	}
}

// Credential ownership is the marker, not the name: a surviving case variant
// that never claimed the shared key cannot keep it alive, or the secret is
// orphaned behind a profile ApplyStoredAPIKey will never read.
func TestCredentialKeyRetainedRequiresASurvivingOwner(t *testing.T) {
	cases := []struct {
		name      string
		providers []ProviderProfile
		removed   string
		want      bool
	}{
		{
			name:      "survivor claims the credential",
			providers: []ProviderProfile{{Name: "WORK", APIKeyStored: true}},
			removed:   "work",
			want:      true,
		},
		{
			name:      "survivor exists but never claimed the credential",
			providers: []ProviderProfile{{Name: "WORK"}},
			removed:   "work",
			want:      false,
		},
		{
			name:      "no survivor shares the identity",
			providers: []ProviderProfile{{Name: "other", APIKeyStored: true}},
			removed:   "work",
			want:      false,
		},
		{
			name:      "unicode long-s does not share the identity",
			providers: []ProviderProfile{{Name: "ſ", APIKeyStored: true}},
			removed:   "s",
			want:      false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := CredentialKeyRetained(testCase.providers, testCase.removed); got != testCase.want {
				t.Fatalf("CredentialKeyRetained = %v, want %v", got, testCase.want)
			}
		})
	}
}

// The delete confirmation must be able to promise exactly what the delete does,
// so the pre-mutation answer has to match the post-mutation one.
func TestProviderKeyRetainedAfterRemovalMatchesPostRemovalAnswer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	writeConfigFixture(t, path, FileConfig{
		Providers: []ProviderProfile{{Name: "work", APIKeyStored: true}, {Name: "WORK", APIKeyStored: true}},
	}, 0o600)

	before, err := ProviderKeyRetainedAfterRemoval(path, "work")
	if err != nil {
		t.Fatal(err)
	}
	if !before {
		t.Fatal("pre-removal answer = false, want true (WORK still claims the credential)")
	}
	cfg, err := RemoveProvider(path, "work")
	if err != nil {
		t.Fatal(err)
	}
	if after := CredentialKeyRetained(cfg.Providers, "work"); after != before {
		t.Fatalf("post-removal answer = %v, want %v", after, before)
	}
}

// Repairing a case-duplicate config can leave activeProvider on a third
// spelling that matches no remaining row exactly, which every exact mutator
// then fails against. Removal re-points it at the survivor's own spelling.
func TestRemoveProviderNormalizesStaleActiveProviderSpelling(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "WoRk",
		Providers:      []ProviderProfile{{Name: "work"}, {Name: "WORK"}},
	}, 0o600)

	cfg, err := RemoveProvider(path, "WORK")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ActiveProvider != "work" {
		t.Fatalf("activeProvider = %q, want the surviving row's spelling work", cfg.ActiveProvider)
	}
	if _, err := SetProviderModel(path, cfg.ActiveProvider, "gpt-4"); err != nil {
		t.Fatalf("exact mutator still cannot find the active row: %v", err)
	}
}

func TestRepairExplicitSplitPreservesExactNamedSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfigFixture(t, path, FileConfig{ActiveProvider: "other", Providers: []ProviderProfile{
		{ProviderKind: ProviderKindOpenAI, Model: "gpt-4o"},
		{Name: "other", ProviderKind: ProviderKindOpenAI, Model: "gpt-4.1", APIKey: "other-key"},
	}}, 0600)
	if _, _, err := RepairUnnamedProvider(path, "legacy"); err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(ResolveOptions{UserConfigPath: path, Env: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	if p := resolved.Provider; p.Name != "other" || p.Model != "gpt-4.1" || p.APIKey != "other-key" {
		t.Fatal("explicit split changed the exact named selection")
	}
}

// Maintainer regression (PR #1001): notify writes must contend with EXISTING
// config mutators, not only each other. UpdateNotify originally took a private
// lock (acquireConfigLock); on Windows its LockFileEx range did not overlap
// lockutil's, so `zero config notify --mode off` could race a theme (or
// provider/credential/MCP) write and silently discard it — the later rename
// wins and the other acknowledged change is gone. UpdateNotify now uses the
// same lockConfigFileFn authority as every other mutator; this race holds it
// against SetTheme and requires BOTH acknowledged edits to survive.
func TestUpdateNotifyRacesExistingConfigMutatorWithoutLoss(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	if err := os.WriteFile(path, []byte(`{"notify":{"mode":"both","focusMode":"unfocused"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := UpdateNotify(path, func(NotifyConfig) NotifyConfig {
			return NotifyConfig{Mode: "off", FocusMode: "always"}
		}); err != nil {
			errs <- fmt.Errorf("notify write: %w", err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := SetTheme(path, "dracula"); err != nil {
			errs <- fmt.Errorf("theme write: %w", err)
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	cfg := readConfigFixture(t, path)
	if cfg.Notify.Mode != "off" || cfg.Notify.FocusMode != "always" {
		t.Errorf("notify lost the race: %+v, want off/always — the two writers did not contend on one lock", cfg.Notify)
	}
	if cfg.Preferences.Theme != "dracula" {
		t.Errorf("theme lost the race: %q, want dracula — the two writers did not contend on one lock", cfg.Preferences.Theme)
	}
}

// Maintainer regression (PR #1001, jatmn review): Go's decoder MERGES the
// fields of duplicate JSON members into one struct, so a full replacement that
// omits a field could inherit it from an earlier duplicate —
// {"notify":{"mode":"off"},"notify":{"focusMode":"focused"}} +
// setNotify({FocusMode:"always"}) must yield blank/always, not off/always.
// The editor removes every duplicate and inserts the replacement fresh.
func TestSetNotifyReplacementInheritsNoFieldFromEarlierDuplicate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	duplicated := `{"notify":{"mode":"off"},"notify":{"focusMode":"focused"},"activeProvider":"openai"}`
	if err := os.WriteFile(path, []byte(duplicated), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := setNotify(path, NotifyConfig{FocusMode: "always"}); err != nil {
		t.Fatalf("setNotify: %v", err)
	}
	stored, err := UserNotify(path)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Mode != "" || stored.FocusMode != "always" {
		t.Fatalf("replacement inherited a field from an earlier duplicate: %+v, want blank/always", stored)
	}

	// Unrelated bytes still survive the rewrite.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"activeProvider":"openai"`) {
		t.Fatalf("unrelated member lost through the replace: %s", string(raw))
	}
}

// Maintainer regression (PR #1001, Vasanthdev2004 review): the duplicate-key
// fix must not reach the ORDINARY single-member file. Removing and re-inserting
// the notify member there moved the key to the end of the object and collapsed
// its formatting, so every /notify toggle reordered the user's config — JSON
// member order carries no meaning, but a byte-preserving write should not
// reformat anything it does not have to. A single notify member is replaced
// in place: key position and surrounding formatting unchanged.
func TestSetNotifySingleMemberReplacedInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	original := "{\n  \"activeProvider\": \"openai\",\n  \"notify\": {\"mode\": \"both\", \"focusMode\": \"unfocused\"},\n  \"preferences\": {\"theme\": \"dracula\"}\n}\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := setNotify(path, NotifyConfig{Mode: "off", FocusMode: "always"}); err != nil {
		t.Fatalf("setNotify: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"activeProvider\": \"openai\",\n  \"notify\": {\"mode\":\"off\",\"focusMode\":\"always\"},\n  \"preferences\": {\"theme\": \"dracula\"}\n}\n"
	if string(raw) != want {
		t.Fatalf("single-member replace moved or reformatted unrelated bytes:\n got: %q\nwant: %q", string(raw), want)
	}
}
