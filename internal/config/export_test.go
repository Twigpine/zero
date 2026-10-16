// Test seams: helpers only test code uses, kept out of the production binary.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// ToolsOverride builds a ToolsConfig that explicitly overrides the deferred-tool
// threshold (including to 0, which disables deferral). Use this for programmatic
// Overrides — a bare ToolsConfig{DeferThreshold: 0} is indistinguishable from
// "unset" and will not override.
func ToolsOverride(deferThreshold int) ToolsConfig {
	return ToolsConfig{DeferThreshold: deferThreshold, deferThresholdSet: true}
}

// ValidateFile reads and parses path as a Zero FileConfig and runs the same
// semantic provider/model rules used during resolution. It returns the parsed
// config (zero value on parse failure) plus any structured issues. A parse
// failure yields a single issue whose Message is the underlying JSON error's
// text (already flattened via Error(), not chained — callers cannot recover
// the original *json.SyntaxError / *json.UnmarshalTypeError via errors.As).
func ValidateFile(path string) (FileConfig, []Issue) {
	data, err := os.ReadFile(path)
	if err != nil {
		return FileConfig{}, []Issue{{Message: fmt.Sprintf("read config %s: %v", path, err)}}
	}

	var cfg FileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return FileConfig{}, []Issue{{Message: fmt.Sprintf("invalid config JSON %s: %v", path, err)}}
	}

	issues := validateSemantics(cfg)
	issues = append(issues, unknownFieldIssues(data)...)
	return cfg, issues
}

// SetProviderDescription sets a provider's description VERBATIM — including to
// empty. The generic UpsertProvider merge treats empty fields as "leave
// unchanged", so clearing a description needs this dedicated setter.
func SetProviderDescription(path string, name string, description string) (result FileConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, fmt.Errorf("config path is required")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return FileConfig{}, fmt.Errorf("provider name is required")
	}
	// Locks like the production mutators so this seam can stand in for one in a
	// concurrency test rather than being the one unsynchronized writer.
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()

	data, err := os.ReadFile(path)
	if err != nil {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg := FileConfig{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
	}
	for index := range cfg.Providers {
		if strings.EqualFold(strings.TrimSpace(cfg.Providers[index].Name), name) {
			cfg.Providers[index].Description = strings.TrimSpace(description)
			if err := writeConfigFile(path, cfg); err != nil {
				return FileConfig{}, err
			}
			return cfg, nil
		}
	}
	return FileConfig{}, fmt.Errorf("provider %q not found", name)
}
