package plugins

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSetEnabledTogglesManifestAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	pluginDir := filepath.Join(root, "demo")
	writePluginManifest(t, pluginDir, map[string]any{
		"schemaVersion": 1,
		"id":            "zero.demo",
		"name":          "Demo",
		"version":       "1.0.0",
		"description":   "keep me",
		"tools":         []any{},
	})
	manifestPath := filepath.Join(pluginDir, "plugin.json")

	changed, err := SetEnabled(manifestPath, false)
	if err != nil {
		t.Fatalf("SetEnabled(false): %v", err)
	}
	if !changed {
		t.Fatal("expected first disable to change the manifest")
	}

	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("re-read manifest: %v", err)
	}
	if obj["enabled"] != false {
		t.Fatalf("enabled = %#v, want false", obj["enabled"])
	}
	if obj["description"] != "keep me" {
		t.Fatalf("description was not preserved: %#v", obj["description"])
	}

	changed, err = SetEnabled(manifestPath, false)
	if err != nil {
		t.Fatalf("SetEnabled(false) again: %v", err)
	}
	if changed {
		t.Fatal("expected second disable to be a no-op")
	}

	changed, err = SetEnabled(manifestPath, true)
	if err != nil {
		t.Fatalf("SetEnabled(true): %v", err)
	}
	if !changed {
		t.Fatal("expected enable to change the manifest")
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
	pluginDir := filepath.Join(root, "demo")
	writePluginManifest(t, pluginDir, map[string]any{
		"schemaVersion": 1,
		"id":            "zero.demo",
		"name":          "Demo",
		"version":       "1.0.0",
	})
	manifestPath := filepath.Join(pluginDir, "plugin.json")
	if err := os.Chmod(manifestPath, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := SetEnabled(manifestPath, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
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
