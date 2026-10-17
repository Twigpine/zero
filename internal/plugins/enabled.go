package plugins

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SetEnabledResult describes a successful enable/disable of one plugin manifest.
type SetEnabledResult struct {
	ID           string `json:"id"`
	Enabled      bool   `json:"enabled"`
	Changed      bool   `json:"changed"`
	Source       Source `json:"source"`
	ManifestPath string `json:"manifestPath"`
}

// FindByID returns the loaded plugin with the given id after normal discovery
// and precedence (project overrides user). A missing id returns ok=false.
func FindByID(options LoadOptions, id string) (LoadedPlugin, bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return LoadedPlugin{}, false, errors.New("plugin id is required")
	}
	result, err := Load(options)
	if err != nil {
		return LoadedPlugin{}, false, err
	}
	for _, plugin := range result.Plugins {
		if plugin.ID == id {
			return plugin, true, nil
		}
	}
	return LoadedPlugin{}, false, nil
}

// SetEnabled updates the "enabled" field in plugin.json, preserving the rest of
// the manifest. It returns whether the file actually changed.
func SetEnabled(manifestPath string, enabled bool) (changed bool, err error) {
	manifestPath = strings.TrimSpace(manifestPath)
	if manifestPath == "" {
		return false, errors.New("plugin manifest path is required")
	}
	resolved, err := filepath.Abs(manifestPath)
	if err != nil {
		return false, err
	}

	raw, err := os.ReadFile(resolved)
	if err != nil {
		return false, err
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false, fmt.Errorf("parse plugin manifest: %w", err)
	}
	if obj == nil {
		return false, errors.New("plugin manifest must be a JSON object")
	}

	previous, err := optionalBool(obj, "enabled", true)
	if err != nil {
		return false, err
	}
	if previous == enabled {
		return false, nil
	}
	obj["enabled"] = enabled

	data, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return false, err
	}
	mode := os.FileMode(0o644)
	if info, statErr := os.Stat(resolved); statErr == nil {
		mode = info.Mode().Perm()
	}
	tempPath := fmt.Sprintf("%s.tmp-%d-%d", resolved, os.Getpid(), time.Now().UnixNano())
	if err := os.WriteFile(tempPath, append(data, '\n'), mode); err != nil {
		return false, err
	}
	if err := os.Rename(tempPath, resolved); err != nil {
		_ = os.Remove(tempPath)
		return false, err
	}
	return true, nil
}

// SetEnabledByID finds a plugin by id (respecting LoadOptions) and toggles its
// manifest enabled field.
func SetEnabledByID(options LoadOptions, id string, enabled bool) (SetEnabledResult, error) {
	plugin, ok, err := FindByID(options, id)
	if err != nil {
		return SetEnabledResult{}, err
	}
	if !ok {
		return SetEnabledResult{}, fmt.Errorf("%w: %q", ErrNotInstalled, id)
	}
	changed, err := SetEnabled(plugin.ManifestPath, enabled)
	if err != nil {
		return SetEnabledResult{}, err
	}
	return SetEnabledResult{
		ID:           plugin.ID,
		Enabled:      enabled,
		Changed:      changed,
		Source:       plugin.Source,
		ManifestPath: plugin.ManifestPath,
	}, nil
}
