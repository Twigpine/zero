package plugins

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/installtxn"
	"github.com/Twigpine/zero/internal/pathjail"
)

func enableTestOptions(root string) LoadOptions {
	return LoadOptions{Roots: []Root{{Source: SourceUser, Path: root}}}
}

func writeRawManifest(t *testing.T, root, dir, raw string) string {
	t.Helper()
	pluginDir := filepath.Join(root, dir)
	if err := os.MkdirAll(pluginDir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(pluginDir, "plugin.json")
	if err := os.WriteFile(manifestPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return manifestPath
}

func demoManifest(enabled bool) string {
	state := "false"
	if enabled {
		state = "true"
	}
	return `{
  "schemaVersion": 1,
  "id": "zero.demo",
  "name": "Demo",
  "version": "1.0.0",
  "enabled": ` + state + `
}
`
}

func TestSetEnabledByIDPreservesManifestBytes(t *testing.T) {
	root := t.TempDir()
	raw := `{
  "schemaVersion": 1,
  "id": "zero.demo",
  "name": "Demo",
  "version": "1.0.0",
  "limits": {
    "maxBytes": 9007199254740993
  },
  "description": "keep me",
  "enabled": true,
  "tools": []
}
`
	manifestPath := writeRawManifest(t, root, "demo", raw)

	result, err := SetEnabledByID(enableTestOptions(root), "zero.demo", false)
	if err != nil {
		t.Fatalf("SetEnabledByID(false): %v", err)
	}
	if !result.Changed || result.Enabled {
		t.Fatalf("unexpected result: %#v", result)
	}

	disabled, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(raw, `"enabled": true`, `"enabled": false`, 1)
	if string(disabled) != want {
		t.Fatalf("toggle changed more than the enabled value:\n got %q\nwant %q", disabled, want)
	}
	if !strings.Contains(string(disabled), "9007199254740993") {
		t.Fatal("large integer literal was rounded by the toggle")
	}

	plugin, ok, err := FindByID(enableTestOptions(root), "zero.demo")
	if err != nil || !ok {
		t.Fatalf("FindByID after disable: ok=%v err=%v", ok, err)
	}
	if plugin.Enabled {
		t.Fatal("loader still reports the plugin enabled after disable")
	}

	result, err = SetEnabledByID(enableTestOptions(root), "zero.demo", true)
	if err != nil {
		t.Fatalf("SetEnabledByID(true): %v", err)
	}
	if !result.Changed || !result.Enabled {
		t.Fatalf("unexpected enable result: %#v", result)
	}
	enabled, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(enabled) != raw {
		t.Fatalf("round trip did not restore the original bytes:\n got %q\nwant %q", enabled, raw)
	}
}

func TestSetEnabledByIDTogglesManifestAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	writePluginManifest(t, filepath.Join(root, "demo"), map[string]any{
		"schemaVersion": 1,
		"id":            "zero.demo",
		"name":          "Demo",
		"version":       "1.0.0",
		"description":   "keep me",
		"tools":         []any{},
	})
	manifestPath := filepath.Join(root, "demo", "plugin.json")

	result, err := SetEnabledByID(enableTestOptions(root), "zero.demo", false)
	if err != nil {
		t.Fatalf("SetEnabledByID(false): %v", err)
	}
	if !result.Changed {
		t.Fatal("expected first disable to change the manifest")
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"enabled": false`) {
		t.Fatalf("manifest not disabled: %s", raw)
	}
	if !strings.Contains(string(raw), `"keep me"`) {
		t.Fatalf("description was not preserved: %s", raw)
	}

	result, err = SetEnabledByID(enableTestOptions(root), "zero.demo", false)
	if err != nil {
		t.Fatalf("SetEnabledByID(false) again: %v", err)
	}
	if result.Changed {
		t.Fatal("expected second disable to be a no-op")
	}

	result, err = SetEnabledByID(enableTestOptions(root), "zero.demo", true)
	if err != nil {
		t.Fatalf("SetEnabledByID(true): %v", err)
	}
	if !result.Changed {
		t.Fatal("expected enable to change the manifest")
	}
}

func TestSetEnabledByIDInsertsMissingField(t *testing.T) {
	t.Run("pretty manifest", func(t *testing.T) {
		root := t.TempDir()
		raw := `{
  "schemaVersion": 1,
  "id": "zero.demo",
  "name": "Demo",
  "version": "1.0.0"
}
`
		manifestPath := writeRawManifest(t, root, "demo", raw)

		if _, err := SetEnabledByID(enableTestOptions(root), "zero.demo", false); err != nil {
			t.Fatalf("SetEnabledByID(false): %v", err)
		}
		disabled, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		wantDisabled := `{
  "schemaVersion": 1,
  "id": "zero.demo",
  "name": "Demo",
  "version": "1.0.0",
  "enabled": false
}
`
		if string(disabled) != wantDisabled {
			t.Fatalf("after insert:\n got %q\nwant %q", disabled, wantDisabled)
		}

		if _, err := SetEnabledByID(enableTestOptions(root), "zero.demo", true); err != nil {
			t.Fatalf("SetEnabledByID(true): %v", err)
		}
		enabled, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		wantEnabled := strings.Replace(wantDisabled, `"enabled": false`, `"enabled": true`, 1)
		if string(enabled) != wantEnabled {
			t.Fatalf("after replace:\n got %q\nwant %q", enabled, wantEnabled)
		}
	})

	t.Run("compact manifest", func(t *testing.T) {
		root := t.TempDir()
		raw := `{"schemaVersion":1,"id":"zero.demo","name":"Demo","version":"1.0.0"}`
		manifestPath := writeRawManifest(t, root, "demo", raw)

		if _, err := SetEnabledByID(enableTestOptions(root), "zero.demo", false); err != nil {
			t.Fatalf("SetEnabledByID(false): %v", err)
		}
		disabled, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		want := `{"schemaVersion":1,"id":"zero.demo","name":"Demo","version":"1.0.0","enabled": false}`
		if string(disabled) != want {
			t.Fatalf("compact insert:\n got %q\nwant %q", disabled, want)
		}
	})

	t.Run("null value", func(t *testing.T) {
		root := t.TempDir()
		raw := `{
  "schemaVersion": 1,
  "id": "zero.demo",
  "name": "Demo",
  "version": "1.0.0",
  "enabled": null
}
`
		manifestPath := writeRawManifest(t, root, "demo", raw)

		result, err := SetEnabledByID(enableTestOptions(root), "zero.demo", true)
		if err != nil {
			t.Fatalf("SetEnabledByID(true): %v", err)
		}
		if result.Changed {
			t.Fatal("null counts as enabled, so enabling must be a no-op")
		}
		unchanged, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(unchanged) != raw {
			t.Fatalf("no-op toggle rewrote the manifest: %q", unchanged)
		}

		if _, err := SetEnabledByID(enableTestOptions(root), "zero.demo", false); err != nil {
			t.Fatalf("SetEnabledByID(false): %v", err)
		}
		disabled, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		want := strings.Replace(raw, `"enabled": null`, `"enabled": false`, 1)
		if string(disabled) != want {
			t.Fatalf("null replacement:\n got %q\nwant %q", disabled, want)
		}
	})
}

func TestToggleEnabledManifestFormattingEdges(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		enabled bool
		want    string
		changed bool
	}{
		{"empty object", `{}`, false, `{"enabled": false}`, true},
		{"empty object with space", `{ }`, false, `{"enabled": false }`, true},
		{"empty multiline", "{\n}", false, "{\n  \"enabled\": false\n}", true},
		{"compact members", `{"a":1}`, false, `{"a":1,"enabled": false}`, true},
		{"crlf members", "{\r\n  \"a\": 1\r\n}", false, "{\r\n  \"a\": 1,\r\n  \"enabled\": false\r\n}", true},
		{"nested enabled untouched", `{"nested":{"enabled":true},"a":2}`, false, `{"nested":{"enabled":true},"a":2,"enabled": false}`, true},
		{"replace with true", `{"enabled": false}`, true, `{"enabled": true}`, true},
		{"replace with false", `{"enabled":true}`, false, `{"enabled":false}`, true},
		{"null to false", `{"enabled": null}`, false, `{"enabled": false}`, true},
		{"null to true is a no-op", `{"enabled": null}`, true, `{"enabled": null}`, false},
		{"already enabled is a no-op", `{"enabled": true}`, true, `{"enabled": true}`, false},
		// The loader keeps the last duplicate, so an overwritten non-boolean
		// value is not an error and only the last value is rewritten.
		{"overwritten string", `{"enabled":"obsolete","enabled":true}`, false, `{"enabled":"obsolete","enabled":false}`, true},
		{"overwritten composite", `{"enabled":{"a":[1,{"enabled":2}]},"enabled":null}`, false, `{"enabled":{"a":[1,{"enabled":2}]},"enabled":false}`, true},
		{"overwritten number is a no-op", `{"enabled":1,"enabled":false}`, false, `{"enabled":1,"enabled":false}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, changed, err := toggleEnabledManifest([]byte(tc.in), tc.enabled)
			if err != nil {
				t.Fatalf("toggleEnabledManifest: %v", err)
			}
			if changed != tc.changed {
				t.Fatalf("changed = %v, want %v", changed, tc.changed)
			}
			if !tc.changed {
				if out != nil {
					t.Fatalf("no-op returned bytes: %q", out)
				}
				return
			}
			if string(out) != tc.want {
				t.Fatalf("got %q, want %q", out, tc.want)
			}
		})
	}
}

func TestToggleEnabledManifestRejectsMalformedInput(t *testing.T) {
	for _, raw := range []string{`{"enabled": "yes"}`, `{"enabled": 1}`, `{"enabled": []}`, `{"enabled": {}}`, `{"enabled": true, "enabled": "yes"}`} {
		t.Run("value "+raw, func(t *testing.T) {
			_, _, err := toggleEnabledManifest([]byte(raw), false)
			if err == nil {
				t.Fatal("expected error")
			}
			var manifestErr ManifestError
			if !errors.As(err, &manifestErr) {
				t.Fatalf("error = %v, want ManifestError", err)
			}
			if !strings.Contains(err.Error(), "enabled") {
				t.Fatalf("error = %v, want it to name the enabled field", err)
			}
		})
	}
	for _, raw := range []string{``, `{`, `[]`, `null`, `"str"`, `{} {}`, `{}{}`} {
		t.Run("document "+raw, func(t *testing.T) {
			if _, _, err := toggleEnabledManifest([]byte(raw), false); err == nil {
				t.Fatalf("toggleEnabledManifest(%q) succeeded, want error", raw)
			}
		})
	}
}

// TestSetEnabledByIDHonorsTheLastDuplicateEnabledValue keeps the toggle in step
// with the loader on a manifest whose earlier "enabled" value is overwritten:
// the loader discovers the plugin from the last value, so the toggle must be
// able to change it too.
func TestSetEnabledByIDHonorsTheLastDuplicateEnabledValue(t *testing.T) {
	root := t.TempDir()
	writeRawManifest(t, root, "demo", `{
  "schemaVersion": 1,
  "id": "zero.demo",
  "name": "Demo",
  "version": "1.0.0",
  "enabled": "obsolete",
  "enabled": true
}
`)
	loadedEnabled := func() bool {
		t.Helper()
		plugin, ok, err := FindByID(enableTestOptions(root), "zero.demo")
		if err != nil || !ok {
			t.Fatalf("FindByID: ok=%v err=%v", ok, err)
		}
		return plugin.Enabled
	}
	if !loadedEnabled() {
		t.Fatal("the loader should discover the plugin as enabled from the last value")
	}
	for _, enabled := range []bool{false, true} {
		result, err := SetEnabledByID(enableTestOptions(root), "zero.demo", enabled)
		if err != nil {
			t.Fatalf("SetEnabledByID(%v): %v", enabled, err)
		}
		if !result.Changed {
			t.Fatalf("SetEnabledByID(%v) reported no change", enabled)
		}
		if got := loadedEnabled(); got != enabled {
			t.Fatalf("after SetEnabledByID(%v) the loader sees enabled=%v", enabled, got)
		}
	}
}

// TestSetEnabledRefusesASwappedPluginDirectory swaps the plugin directory that
// discovery found for a link between the lookup and the edit. The install lock
// does not cover that window, because it only serializes cooperating
// installers. The edit must fail without following the link: neither the
// link's target nor the moved-away original may change.
func TestSetEnabledRefusesASwappedPluginDirectory(t *testing.T) {
	cases := []struct {
		name string
		// link returns the symlink target to plant and the manifest it leads to.
		link func(t *testing.T, root string) (target string, manifest string)
	}{
		{"to a directory outside the plugins root", func(t *testing.T, root string) (string, string) {
			manifest := writeRawManifest(t, t.TempDir(), "elsewhere", demoManifest(true))
			return filepath.Dir(manifest), manifest
		}},
		{"to another plugin in the same root", func(t *testing.T, root string) (string, string) {
			// A relative link stays inside the root, so confinement to the
			// root alone would follow it.
			manifest := writeRawManifest(t, root, "other", strings.Replace(demoManifest(true), "zero.demo", "zero.other", 1))
			return "other", manifest
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			raw := demoManifest(true)
			writeRawManifest(t, root, "demo", raw)
			plugin, ok, err := FindByID(enableTestOptions(root), "zero.demo")
			if err != nil || !ok {
				t.Fatalf("FindByID: ok=%v err=%v", ok, err)
			}
			target, targetManifest := tc.link(t, root)
			targetBefore, err := os.ReadFile(targetManifest)
			if err != nil {
				t.Fatal(err)
			}
			moved := filepath.Join(t.TempDir(), "moved-away")
			if err := os.Rename(plugin.PluginDir, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, plugin.PluginDir); err != nil {
				t.Skipf("cannot create a directory symlink here: %v", err)
			}

			if _, err := setEnabledLocked(plugin, false); !errors.Is(err, pathjail.ErrReparse) {
				t.Fatalf("setEnabledLocked through a swapped directory: err = %v, want %v", err, pathjail.ErrReparse)
			}
			if after, err := os.ReadFile(targetManifest); err != nil || string(after) != string(targetBefore) {
				t.Fatalf("the edit followed the link and changed %s (err %v):\n got %q\nwant %q", targetManifest, err, after, targetBefore)
			}
			if after, err := os.ReadFile(filepath.Join(moved, "plugin.json")); err != nil || string(after) != raw {
				t.Fatalf("the moved-away manifest changed (err %v): %q", err, after)
			}
		})
	}
}

// TestSetEnabledDetectsAPluginDirectoryReplacedAfterOpening replaces the live
// plugin directory after the toggle pinned it. The edit then lands in the
// moved-away directory, so the toggle must not refresh the lockfile from either
// tree and must not report success: the plugin installed at the path now is
// unchanged.
func TestSetEnabledDetectsAPluginDirectoryReplacedAfterOpening(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pinned  bool // the pinned manifest's enabled value
		changes bool // whether disabling it is a real edit
	}{
		{"edit", true, true},
		{"no-op", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			manifestPath := writeRawManifest(t, root, "demo", demoManifest(tc.pinned))
			pluginDir := filepath.Dir(manifestPath)
			hash, err := hashTree(pluginDir)
			if err != nil {
				t.Fatal(err)
			}
			if err := writeLock(root, map[string]LockEntry{"zero.demo": {Source: "./demo", Hash: hash}}); err != nil {
				t.Fatal(err)
			}
			lockPath := filepath.Join(root, LockFileName)
			lockBefore, err := os.ReadFile(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			plugin, ok, err := FindByID(enableTestOptions(root), "zero.demo")
			if err != nil || !ok {
				t.Fatalf("FindByID: ok=%v err=%v", ok, err)
			}
			pinned, err := openPluginDir(plugin)
			if err != nil {
				t.Fatalf("openPluginDir: %v", err)
			}
			defer pinned.Close()

			moved := filepath.Join(t.TempDir(), "moved-away")
			if err := os.Rename(pluginDir, moved); err != nil {
				t.Fatal(err)
			}
			replacement := strings.Replace(demoManifest(true), `"version": "1.0.0"`, `"version": "2.0.0"`, 1)
			writeRawManifest(t, root, "demo", replacement)

			// The lockfile check hashes the pinned tree, which still matches the
			// recorded hash, not the replacement now at the path.
			if _, _, clean := cleanLockEntry(plugin, pinned.dir); !clean {
				t.Error("cleanLockEntry hashed the replacement at the path instead of the pinned directory")
			}

			if _, err := setEnabledPinned(plugin, pinned, false); !errors.Is(err, errPluginDirReplaced) {
				t.Fatalf("setEnabledPinned after the directory was replaced: err = %v, want %v", err, errPluginDirReplaced)
			}
			if after, err := os.ReadFile(manifestPath); err != nil || string(after) != replacement {
				t.Fatalf("the replacement manifest changed (err %v): %q", err, after)
			}
			if after, err := os.ReadFile(lockPath); err != nil || string(after) != string(lockBefore) {
				t.Fatalf("the lockfile changed (err %v):\n got %s\nwant %s", err, after, lockBefore)
			}
			movedRaw, err := os.ReadFile(filepath.Join(moved, "plugin.json"))
			if err != nil {
				t.Fatal(err)
			}
			if tc.changes != (string(movedRaw) != demoManifest(tc.pinned)) {
				t.Fatalf("pinned manifest edit = %v, want %v: %q", !tc.changes, tc.changes, movedRaw)
			}
		})
	}
}

func TestSetEnabledByIDRespectsUserOnlyFilter(t *testing.T) {
	dir := t.TempDir()
	userRoot := filepath.Join(dir, "user")
	projectRoot := filepath.Join(dir, "project")
	writePluginManifest(t, filepath.Join(userRoot, "demo"), map[string]any{
		"schemaVersion": 1,
		"id":            "zero.demo",
		"name":          "User Demo",
		"version":       "0.1.0",
		"enabled":       true,
	})
	writePluginManifest(t, filepath.Join(projectRoot, "demo"), map[string]any{
		"schemaVersion": 1,
		"id":            "zero.demo",
		"name":          "Project Demo",
		"version":       "0.2.0",
		"enabled":       true,
	})

	result, err := SetEnabledByID(LoadOptions{
		Roots: []Root{
			{Source: SourceUser, Path: userRoot},
			{Source: SourceProject, Path: projectRoot},
		},
	}, "zero.demo", false)
	if err != nil {
		t.Fatalf("SetEnabledByID: %v", err)
	}
	if result.Source != SourceProject {
		t.Fatalf("source = %s, want project (precedence)", result.Source)
	}
	if !result.Changed || result.Enabled {
		t.Fatalf("unexpected result: %#v", result)
	}

	userOnly, err := SetEnabledByID(LoadOptions{
		Roots: []Root{
			{Source: SourceUser, Path: userRoot},
			{Source: SourceProject, Path: projectRoot},
		},
		ExcludeProject: true,
	}, "zero.demo", false)
	if err != nil {
		t.Fatalf("SetEnabledByID user-only: %v", err)
	}
	if userOnly.Source != SourceUser {
		t.Fatalf("user-only source = %s, want user", userOnly.Source)
	}
}

func TestSetEnabledPreservesManifestMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not preserve chmod bits in os.Stat mode")
	}
	root := t.TempDir()
	writePluginManifest(t, filepath.Join(root, "demo"), map[string]any{
		"schemaVersion": 1,
		"id":            "zero.demo",
		"name":          "Demo",
		"version":       "1.0.0",
	})
	manifestPath := filepath.Join(root, "demo", "plugin.json")
	if err := os.Chmod(manifestPath, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := SetEnabledByID(enableTestOptions(root), "zero.demo", false); err != nil {
		t.Fatalf("SetEnabledByID: %v", err)
	}
	info, err := os.Stat(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("mode = %04o, want 0644", got)
	}
}

func TestSetEnabledByIDMissingPlugin(t *testing.T) {
	_, err := SetEnabledByID(LoadOptions{Roots: []Root{{Source: SourceUser, Path: t.TempDir()}}}, "missing", false)
	if err == nil {
		t.Fatal("expected missing plugin error")
	}
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("error = %v, want ErrNotInstalled", err)
	}
}

func TestSetEnabledPublishFailureLeavesManifestIntact(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows directory permissions do not block writes the way this test needs")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission checks")
	}
	root := t.TempDir()
	raw := demoManifest(true)
	manifestPath := writeRawManifest(t, root, "demo", raw)
	pluginDir := filepath.Dir(manifestPath)
	if err := os.Chmod(pluginDir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(pluginDir, 0o755) }()

	if _, err := SetEnabledByID(enableTestOptions(root), "zero.demo", false); err == nil {
		t.Fatal("expected the publish to fail in a read-only plugin directory")
	}
	after, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != raw {
		t.Fatalf("failed publish changed the manifest:\n got %q\nwant %q", after, raw)
	}
}

// TestSetEnabledByIDWaitsForInstallRootLock proves the toggle takes the same
// cross-process lock Install and Remove take on the plugin's install root: it
// must not complete while an install holds that lock.
func TestSetEnabledByIDWaitsForInstallRootLock(t *testing.T) {
	root := t.TempDir()
	manifestPath := writeRawManifest(t, root, "demo", demoManifest(true))

	unlock, err := installtxn.Lock(root)
	if err != nil {
		t.Fatal(err)
	}

	results := make(chan SetEnabledResult, 1)
	failures := make(chan error, 1)
	go func() {
		result, err := SetEnabledByID(enableTestOptions(root), "zero.demo", false)
		if err != nil {
			failures <- err
			return
		}
		results <- result
	}()

	select {
	case result := <-results:
		unlock()
		t.Fatalf("toggle completed while the install lock was held: %#v", result)
	case err := <-failures:
		unlock()
		t.Fatalf("toggle failed while the install lock was held: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	unlock()

	select {
	case result := <-results:
		if !result.Changed {
			t.Fatalf("toggle did not report a change: %#v", result)
		}
	case err := <-failures:
		t.Fatalf("toggle failed after the lock was released: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("toggle did not finish after the install lock was released")
	}

	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"enabled": false`) {
		t.Fatalf("manifest not disabled: %s", raw)
	}
}

// TestSetEnabledByIDSynchronizesWithStagedInstallCommit pauses an install
// between staging and commit, lets the toggle try to run, then commits the
// install. The toggle must apply to the newly installed manifest instead of
// overwriting it with bytes read before the install, and the lockfile entry
// must describe the tree actually on disk.
func TestSetEnabledByIDSynchronizesWithStagedInstallCommit(t *testing.T) {
	root := t.TempDir()
	oldRaw := demoManifest(true)
	pluginDir := filepath.Dir(writeRawManifest(t, root, "zero.demo", oldRaw))

	// Build the replacement tree with the same staging primitives Install
	// uses and leave it staged but uncommitted.
	src := t.TempDir()
	newRaw := `{
  "schemaVersion": 1,
  "id": "zero.demo",
  "name": "Demo",
  "version": "2.0.0",
  "description": "reinstalled",
  "enabled": true
}
`
	srcPlugin := filepath.Join(src, "zero.demo")
	if err := os.MkdirAll(srcPlugin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcPlugin, "plugin.json"), []byte(newRaw), 0o600); err != nil {
		t.Fatal(err)
	}
	staged, cleanup, err := installtxn.StageDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := copyTree(srcPlugin, staged); err != nil {
		t.Fatal(err)
	}
	stagedHash, err := hashTree(staged)
	if err != nil {
		t.Fatal(err)
	}

	// An install holds the root lock from before its commit until after it.
	unlock, err := installtxn.Lock(root)
	if err != nil {
		t.Fatal(err)
	}

	results := make(chan SetEnabledResult, 1)
	failures := make(chan error, 1)
	go func() {
		result, err := SetEnabledByID(enableTestOptions(root), "zero.demo", false)
		if err != nil {
			failures <- err
			return
		}
		results <- result
	}()

	select {
	case result := <-results:
		unlock()
		t.Fatalf("toggle completed while the install lock was held: %#v", result)
	case err := <-failures:
		unlock()
		t.Fatalf("toggle failed while the install lock was held: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	// Commit the staged install exactly like Install does: swap the tree and
	// publish the lockfile entry while still holding the lock.
	lock, err := ReadLock(root)
	if err != nil {
		unlock()
		t.Fatal(err)
	}
	lock["zero.demo"] = LockEntry{Source: string(SourceUser), Hash: stagedHash}
	if err := installtxn.CommitDir(pluginDir, staged, func() error { return writeLock(root, lock) }); err != nil {
		unlock()
		t.Fatal(err)
	}
	unlock()

	var result SetEnabledResult
	select {
	case result = <-results:
	case err := <-failures:
		t.Fatalf("toggle failed after the install committed: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("toggle did not finish after the install committed")
	}
	if !result.Changed {
		t.Fatalf("toggle result = %#v, want changed", result)
	}

	// The toggle must have applied to the newly installed manifest, not to the
	// bytes it first read.
	final, err := os.ReadFile(filepath.Join(pluginDir, "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(newRaw, `"enabled": true`, `"enabled": false`, 1)
	if string(final) != want {
		t.Fatalf("manifest after concurrent install + disable:\n got %q\nwant %q", final, want)
	}

	// The lockfile entry must describe the tree that is actually on disk.
	entries, err := ReadLock(root)
	if err != nil {
		t.Fatal(err)
	}
	current, err := hashTree(pluginDir)
	if err != nil {
		t.Fatal(err)
	}
	if entries["zero.demo"].Hash != current {
		t.Fatalf("plugins.lock hash = %q, want current tree hash %q", entries["zero.demo"].Hash, current)
	}
}

func TestSetEnabledByIDRefreshesCleanLockEntry(t *testing.T) {
	root := t.TempDir()
	manifestPath := writeRawManifest(t, root, "zero.demo", demoManifest(true))
	pluginDir := filepath.Dir(manifestPath)

	current, err := hashTree(pluginDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeLock(root, map[string]LockEntry{"zero.demo": {Source: string(SourceUser), Hash: current}}); err != nil {
		t.Fatal(err)
	}

	if _, err := SetEnabledByID(enableTestOptions(root), "zero.demo", false); err != nil {
		t.Fatalf("SetEnabledByID: %v", err)
	}
	entries, err := ReadLock(root)
	if err != nil {
		t.Fatal(err)
	}
	afterToggle, err := hashTree(pluginDir)
	if err != nil {
		t.Fatal(err)
	}
	if entries["zero.demo"].Hash == current {
		t.Fatal("lock hash was not refreshed for a clean install")
	}
	if entries["zero.demo"].Hash != afterToggle {
		t.Fatalf("lock hash = %q, want refreshed tree hash %q", entries["zero.demo"].Hash, afterToggle)
	}

	info, err := Info(InfoOptions{LoadOptions: enableTestOptions(root)}, "zero.demo")
	if err != nil {
		t.Fatal(err)
	}
	if info.HashDrift {
		t.Fatal("toggle left hash drift behind for a clean install")
	}

	// Pre-existing drift must survive the next toggle instead of being
	// absorbed into the lockfile.
	if err := os.WriteFile(filepath.Join(pluginDir, "extra.txt"), []byte("edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SetEnabledByID(enableTestOptions(root), "zero.demo", true); err != nil {
		t.Fatalf("SetEnabledByID(true): %v", err)
	}
	entries, err = ReadLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if entries["zero.demo"].Hash != afterToggle {
		t.Fatalf("toggle absorbed pre-existing drift: hash = %q, want %q", entries["zero.demo"].Hash, afterToggle)
	}
}

// TestSetEnabledByIDReportsAPluginRemovedWhileWaiting removes the plugin while
// the toggle is blocked on the install-root lock, exactly as Remove does while
// it holds that lock. The toggle must report the plugin as not installed
// instead of editing, or recreating, a tree that no longer exists.
func TestSetEnabledByIDReportsAPluginRemovedWhileWaiting(t *testing.T) {
	root := t.TempDir()
	pluginDir := filepath.Dir(writeRawManifest(t, root, "zero.demo", demoManifest(true)))

	unlock, err := installtxn.Lock(root)
	if err != nil {
		t.Fatal(err)
	}

	failures := make(chan error, 1)
	go func() {
		_, err := SetEnabledByID(enableTestOptions(root), "zero.demo", false)
		failures <- err
	}()

	select {
	case err := <-failures:
		unlock()
		t.Fatalf("toggle returned while the install lock was held: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	if err := installtxn.RemoveDir(pluginDir, func() error { return nil }); err != nil {
		unlock()
		t.Fatal(err)
	}
	unlock()

	select {
	case err := <-failures:
		if !errors.Is(err, ErrNotInstalled) {
			t.Fatalf("toggle after a concurrent removal: error = %v, want ErrNotInstalled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("toggle did not finish after the removal released the lock")
	}
	if _, err := os.Stat(pluginDir); !os.IsNotExist(err) {
		t.Fatalf("toggle recreated the removed plugin: %v", err)
	}
}

// TestSetEnabledByIDResolvesInterruptedUpdatesFirst covers every point an
// update can be killed at that leaves the plugin discoverable. The toggle
// holds the install-root lock, so it has to
// run the same recovery pass Install and Remove run before it reads the tree:
// otherwise it edits, or fails to find, a tree that is still owed a restore,
// and the next install or removal puts the backup back over the user's edit.
func TestSetEnabledByIDResolvesInterruptedUpdatesFirst(t *testing.T) {
	for _, state := range interruptedStates() {
		t.Run(state.name, func(t *testing.T) {
			u := seedInterruptedUpdate(t)
			workspace := state.plant(t, u)
			want := state.want(u)
			if _, err := os.Lstat(u.target()); os.IsNotExist(err) {
				// No root lists a plugin whose only copy is in a workspace, so the
				// toggle reports it not installed without taking any lock, just as
				// list and info do; the next install or removal restores it.
				t.Skip("nothing is at the target, so the toggle never takes the lock")
			}

			result, err := SetEnabledByID(enableTestOptions(u.dir), "zero.demo", false)
			if err != nil {
				t.Fatalf("toggle after the interrupted update: %v", err)
			}
			if !result.Changed || result.Enabled {
				t.Fatalf("toggle result = %#v, want changed and disabled", result)
			}
			assertDisabledAfterRecovery(t, u, workspace, want)

			// The next lock holder runs recovery again. The disable has to survive
			// it rather than be replaced by a backup the toggle never saw.
			if err := installOther(t, u.dir, "zero.other"); err != nil {
				t.Fatalf("install after the toggle: %v", err)
			}
			assertDisabledAfterRecovery(t, u, workspace, want)
		})
	}
}

func assertDisabledAfterRecovery(t *testing.T, u interruptedUpdate, workspace string, want recoveryWant) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(u.target(), "generation.txt"))
	if err != nil {
		t.Fatalf("reading the live tree: %v", err)
	}
	if string(data) != want.generation {
		t.Errorf("the live tree is the %q generation, want %q", data, want.generation)
	}
	plugin, ok, err := FindByID(enableTestOptions(u.dir), "zero.demo")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Load does not see the toggled plugin")
	}
	if plugin.Version != want.version {
		t.Errorf("Load sees version %q, want %q", plugin.Version, want.version)
	}
	if plugin.Enabled {
		t.Errorf("the disable did not stick: Load sees the plugin enabled")
	}
	_, err = os.Stat(workspace)
	if want.workspace && err != nil {
		t.Errorf("the workspace should be left alone, got %v", err)
	}
	if !want.workspace && !os.IsNotExist(err) {
		t.Errorf("the resolved workspace should be cleared, got %v", err)
	}
}

// TestSetEnabledByIDAbortsOnAnUnresolvedInterruptedUpdate leaves a workspace
// whose trees match neither lockfile hash. Recovery cannot pick one, so the
// toggle must fail without editing the live manifest or the lockfile.
func TestSetEnabledByIDAbortsOnAnUnresolvedInterruptedUpdate(t *testing.T) {
	u := seedInterruptedUpdate(t)
	workspace := u.plantWorkspace(t)
	u.mustRename(t, u.target(), filepath.Join(workspace, "previous"))
	u.mustRename(t, filepath.Join(workspace, "staged"), u.target())
	unmatched := LockEntry{Source: u.oldSource, Hash: "sha256:0000000000000000000000000000000000000000000000000000000000000000"}
	lock, err := ReadLock(u.dir)
	if err != nil {
		t.Fatal(err)
	}
	lock["zero.demo"] = unmatched
	if err := writeLock(u.dir, lock); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(u.target(), "plugin.json")
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := SetEnabledByID(enableTestOptions(u.dir), "zero.demo", false); err == nil {
		t.Fatal("toggle did not report the unresolved transaction")
	}
	after, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("the aborted toggle edited the manifest:\n got %q\nwant %q", after, before)
	}
	assertUntouched(t, u, "new", unmatched)
	if _, err := os.Stat(filepath.Join(workspace, "previous")); err != nil {
		t.Errorf("the backup should still be there for a hand rescue, got %v", err)
	}
}
