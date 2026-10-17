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
	for _, raw := range []string{`{"enabled": "yes"}`, `{"enabled": 1}`, `{"enabled": []}`, `{"enabled": {}}`} {
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
