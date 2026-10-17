package plugins

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Twigpine/zero/internal/installtxn"
)

// SetEnabledResult describes a successful enable/disable of one plugin manifest.
type SetEnabledResult struct {
	ID           string `json:"id"`
	Enabled      bool   `json:"enabled"`
	Changed      bool   `json:"changed"`
	Source       Source `json:"source"`
	ManifestPath string `json:"manifestPath"`
}

// maxEnabledRelockAttempts bounds how many times SetEnabledByID re-resolves the
// plugin after it moves to another install root while waiting for the lock.
const maxEnabledRelockAttempts = 4

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

// SetEnabledByID finds a plugin by id (respecting LoadOptions) and toggles the
// top-level "enabled" field of its plugin.json.
//
// The read-modify-write is serialized with plugin installs and removals: it
// takes the same cross-process installtxn lock they take on the plugin's
// install root, re-resolves the plugin under that lock, and publishes the
// manifest with an atomic rename while still holding it, so a concurrent
// install can never be interleaved between the read and the write. Only the
// bytes of the "enabled" value change, so field order, formatting, and numeric
// literals in the rest of the manifest survive the toggle.
func SetEnabledByID(options LoadOptions, id string, enabled bool) (SetEnabledResult, error) {
	for attempt := 0; ; attempt++ {
		plugin, ok, err := FindByID(options, id)
		if err != nil {
			return SetEnabledResult{}, err
		}
		if !ok {
			return SetEnabledResult{}, fmt.Errorf("%w: %q", ErrNotInstalled, id)
		}
		// Take the install root lock before the final lookup and read so an
		// install or remove cannot swap the tree between them.
		unlock, err := installtxn.Lock(plugin.Root)
		if err != nil {
			return SetEnabledResult{}, err
		}
		final, ok, err := FindByID(options, id)
		if err != nil {
			unlock()
			return SetEnabledResult{}, err
		}
		if !ok {
			unlock()
			return SetEnabledResult{}, fmt.Errorf("%w: %q", ErrNotInstalled, id)
		}
		if final.Root != plugin.Root {
			// Precedence moved to another root while this lock was pending;
			// retry against the root that wins now so the edit still serializes
			// with that root's installs.
			unlock()
			if attempt >= maxEnabledRelockAttempts {
				return SetEnabledResult{}, fmt.Errorf("plugin %q kept changing roots while waiting for the install lock", id)
			}
			continue
		}
		changed, err := setEnabledLocked(final, enabled)
		unlock()
		if err != nil {
			return SetEnabledResult{}, err
		}
		return SetEnabledResult{
			ID:           final.ID,
			Enabled:      enabled,
			Changed:      changed,
			Source:       final.Source,
			ManifestPath: final.ManifestPath,
		}, nil
	}
}

// setEnabledLocked performs the manifest read-modify-write while the caller
// holds the installtxn lock for the plugin's install root.
func setEnabledLocked(plugin LoadedPlugin, enabled bool) (bool, error) {
	raw, err := os.ReadFile(plugin.ManifestPath)
	if err != nil {
		return false, err
	}
	data, changed, err := toggleEnabledManifest(raw, enabled)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}

	// Read the lockfile before the edit so the refresh below only runs when the
	// installed tree still matched its recorded hash; a toggle must never mask
	// pre-existing drift.
	lock, entry, hashClean := cleanLockEntry(plugin)

	// Fail closed on stat errors instead of widening the permissions.
	info, err := os.Stat(plugin.ManifestPath)
	if err != nil {
		return false, fmt.Errorf("inspect plugin manifest: %w", err)
	}
	if err := installtxn.WriteFileAtomically(plugin.ManifestPath, data, info.Mode().Perm()); err != nil {
		return false, err
	}
	if !hashClean {
		return true, nil
	}
	after, err := hashTree(plugin.PluginDir)
	if err != nil {
		return true, fmt.Errorf("plugin manifest updated but %s hash refresh failed: %w", LockFileName, err)
	}
	entry.Hash = after
	lock[plugin.ID] = entry
	if err := writeLock(plugin.Root, lock); err != nil {
		return true, fmt.Errorf("plugin manifest updated but %s hash refresh failed: %w", LockFileName, err)
	}
	return true, nil
}

// cleanLockEntry reports whether the tree on disk still matches the lockfile
// entry recorded for the plugin and returns both for a refresh.
func cleanLockEntry(plugin LoadedPlugin) (map[string]LockEntry, LockEntry, bool) {
	lock, err := ReadLock(plugin.Root)
	if err != nil {
		return nil, LockEntry{}, false
	}
	entry, ok := lock[plugin.ID]
	if !ok {
		return nil, LockEntry{}, false
	}
	current, err := hashTree(plugin.PluginDir)
	if err != nil || current != entry.Hash {
		return nil, LockEntry{}, false
	}
	return lock, entry, true
}

// toggleEnabledManifest returns raw with the top-level "enabled" member set to
// enabled. A missing or null value counts as enabled, matching the loader's
// optionalBool fallback. changed reports whether the result differs from raw.
func toggleEnabledManifest(raw []byte, enabled bool) (out []byte, changed bool, err error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	openTok, err := dec.Token()
	if err != nil {
		return nil, false, fmt.Errorf("parse plugin manifest: %w", err)
	}
	if delim, ok := openTok.(json.Delim); !ok || delim != '{' {
		return nil, false, errors.New("plugin manifest must be a JSON object")
	}
	openEnd := dec.InputOffset()

	current := true
	var valueStart, valueEnd int64 = -1, -1
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, false, fmt.Errorf("parse plugin manifest: %w", err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, false, errors.New("plugin manifest must be a JSON object")
		}
		keyEnd := dec.InputOffset()
		if key != "enabled" {
			if err := skipManifestValue(dec); err != nil {
				return nil, false, fmt.Errorf("parse plugin manifest: %w", err)
			}
			continue
		}
		valueTok, err := dec.Token()
		if err != nil {
			return nil, false, fmt.Errorf("parse plugin manifest: %w", err)
		}
		var literal string
		switch value := valueTok.(type) {
		case bool:
			current = value
			literal = "false"
			if value {
				literal = "true"
			}
		case nil:
			current = true
			literal = "null"
		default:
			return nil, false, ManifestError{FieldPath: "enabled", Message: "Expected a boolean."}
		}
		start, err := enabledValueOffset(raw, keyEnd)
		if err != nil {
			return nil, false, err
		}
		if start+int64(len(literal)) > int64(len(raw)) ||
			!bytes.Equal(raw[start:start+int64(len(literal))], []byte(literal)) {
			return nil, false, errors.New("parse plugin manifest: cannot locate the enabled value")
		}
		// A duplicate key keeps the last occurrence, matching json.Unmarshal.
		valueStart, valueEnd = start, start+int64(len(literal))
	}
	closingTok, err := dec.Token()
	if err != nil {
		return nil, false, fmt.Errorf("parse plugin manifest: %w", err)
	}
	if delim, ok := closingTok.(json.Delim); !ok || delim != '}' {
		return nil, false, errors.New("plugin manifest must be a JSON object")
	}
	closingEnd := dec.InputOffset()
	if _, err := dec.Token(); err != io.EOF {
		return nil, false, errors.New("parse plugin manifest: unexpected data after the top-level object")
	}

	if current == enabled {
		return nil, false, nil
	}
	newLiteral := "false"
	if enabled {
		newLiteral = "true"
	}
	if valueStart >= 0 {
		out := make([]byte, 0, len(raw)-int(valueEnd-valueStart)+len(newLiteral))
		out = append(out, raw[:valueStart]...)
		out = append(out, newLiteral...)
		out = append(out, raw[valueEnd:]...)
		return out, true, nil
	}
	return insertEnabledMember(raw, openEnd, closingEnd-1, newLiteral), true, nil
}

// enabledValueOffset returns the byte offset of the value that follows the
// colon after an "enabled" key token that ended at keyEnd.
func enabledValueOffset(raw []byte, keyEnd int64) (int64, error) {
	i := keyEnd
	for i < int64(len(raw)) && isJSONSpace(raw[i]) {
		i++
	}
	if i >= int64(len(raw)) || raw[i] != ':' {
		return 0, errors.New("parse plugin manifest: missing colon after key")
	}
	i++
	for i < int64(len(raw)) && isJSONSpace(raw[i]) {
		i++
	}
	return i, nil
}

// insertEnabledMember adds a top-level "enabled" member to a manifest that has
// none, mirroring the document's own whitespace style where it can. openEnd is
// the byte offset just after the opening brace and bracePos is the byte index
// of the closing brace.
func insertEnabledMember(raw []byte, openEnd, bracePos int64, literal string) []byte {
	insertPos := bracePos
	for insertPos > openEnd && isJSONSpace(raw[insertPos-1]) {
		insertPos--
	}

	newline := "\n"
	if bytes.Contains(raw, []byte("\r\n")) {
		newline = "\r\n"
	}

	first := openEnd
	for first < bracePos && isJSONSpace(raw[first]) {
		first++
	}
	empty := first >= bracePos
	multiline := bytes.IndexByte(raw[openEnd:bracePos], '\n') >= 0

	member := `"enabled": ` + literal
	var insert string
	switch {
	case empty && !multiline:
		insert = member
	case empty && multiline:
		insert = newline + closingLineIndent(raw, openEnd, bracePos) + "  " + member
	case !empty && !multiline:
		insert = "," + member
	default:
		insert = "," + newline + memberLineIndent(raw, openEnd, first) + member
	}

	out := make([]byte, 0, len(raw)+len(insert))
	out = append(out, raw[:insertPos]...)
	out = append(out, insert...)
	out = append(out, raw[insertPos:]...)
	return out
}

// memberLineIndent returns the indentation of the line the first member of a
// JSON object starts on. first is the byte index of that member's first token.
func memberLineIndent(raw []byte, openEnd, first int64) string {
	lineStart := first
	for lineStart > openEnd && raw[lineStart-1] != '\n' {
		lineStart--
	}
	end := lineStart
	for end < first && (raw[end] == ' ' || raw[end] == '\t') {
		end++
	}
	return string(raw[lineStart:end])
}

// closingLineIndent returns the indentation before an object's closing brace.
func closingLineIndent(raw []byte, openEnd, bracePos int64) string {
	start := bracePos
	for start > openEnd && (raw[start-1] == ' ' || raw[start-1] == '\t') {
		start--
	}
	return string(raw[start:bracePos])
}

// skipManifestValue consumes the next JSON value from dec.
func skipManifestValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok || (delim != '{' && delim != '[') {
		return nil
	}
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if inner, ok := tok.(json.Delim); ok {
			switch inner {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return nil
}

func isJSONSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
