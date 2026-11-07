package plugins

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Twigpine/zero/internal/installtxn"
	"github.com/Twigpine/zero/internal/pathjail"
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
// install root, runs the same interrupted-transaction recovery they run under
// it, re-resolves the plugin under that lock, and publishes the
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
		// Put back anything an earlier install or removal was killed mid-commit,
		// and abort if a transaction could not be resolved: editing a tree that is
		// still owed a restore loses the edit when the next install or removal
		// puts the backup back over it. See installtxn.Recover.
		if err := installtxn.Recover(plugin.Root, reconcileInterrupted(plugin.Root)); err != nil {
			unlock()
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
//
// The lock only serializes cooperating installers; it does not stop the plugin
// directory from being swapped after discovery found it. So the whole update
// works on one directory, pinned by openPluginDir: the manifest is read and
// published, and the tree hashed for the lockfile, through that handle, never
// by re-resolving the path.
func setEnabledLocked(plugin LoadedPlugin, enabled bool) (bool, error) {
	pinned, err := openPluginDir(plugin)
	if err != nil {
		return false, err
	}
	defer pinned.Close()
	return setEnabledPinned(plugin, pinned, enabled)
}

// setEnabledPinned is setEnabledLocked on an already pinned plugin directory.
//
// The edit lands in the pinned directory even if the path now names another
// one, so before refreshing the lockfile or reporting any result it confirms
// that the directory entry still is the pinned directory. Otherwise the
// command would report a state that the installed plugin does not have.
func setEnabledPinned(plugin LoadedPlugin, pinned *pinnedPluginDir, enabled bool) (bool, error) {
	manifestName := filepath.Base(plugin.ManifestPath)
	raw, perm, err := readManifest(pinned.dir, manifestName)
	if err != nil {
		return false, err
	}
	data, changed, err := toggleEnabledManifest(raw, enabled)
	if err != nil {
		return false, err
	}
	if !changed {
		if err := pinned.verifyLive(); err != nil {
			return false, err
		}
		return false, nil
	}

	// Read the lockfile before the edit so the refresh below only runs when the
	// installed tree still matched its recorded hash; a toggle must never mask
	// pre-existing drift.
	lock, entry, hashClean := cleanLockEntry(plugin, pinned.dir)

	if err := publishManifest(pinned.dir, manifestName, data, perm); err != nil {
		return false, err
	}
	if !hashClean {
		if err := pinned.verifyLive(); err != nil {
			return false, err
		}
		return true, nil
	}
	after, err := hashRootedTree(pinned.dir)
	if err != nil {
		return true, fmt.Errorf("plugin manifest updated but %s hash refresh failed: %w", LockFileName, err)
	}
	if err := pinned.verifyLive(); err != nil {
		return false, err
	}
	entry.Hash = after
	lock[plugin.ID] = entry
	if err := writeLock(plugin.Root, lock); err != nil {
		return true, fmt.Errorf("plugin manifest updated but %s hash refresh failed: %w", LockFileName, err)
	}
	return true, nil
}

// errPluginDirReplaced reports that the plugin's directory entry no longer
// names the directory the toggle pinned and edited.
var errPluginDirReplaced = errors.New("plugin directory was replaced while it was being updated; the plugin installed there now was not changed")

// pinnedPluginDir is the plugin directory discovery found, held open for the
// length of one toggle.
type pinnedPluginDir struct {
	root *os.Root // the plugin's install root
	name string   // the plugin directory's entry name in root
	path string   // the plugin directory's path, for messages
	dir  *os.Root // the plugin directory itself
}

// Close releases both handles.
func (p *pinnedPluginDir) Close() {
	_ = p.dir.Close()
	_ = p.root.Close()
}

// verifyLive confirms that the plugin's entry in its install root is still a
// real directory, not a symlink, junction, or other reparse point, and is the
// very directory p.dir holds open.
func (p *pinnedPluginDir) verifyLive() error {
	opened, err := p.dir.Stat(".")
	if err != nil {
		return fmt.Errorf("inspect plugin directory: %w", err)
	}
	entry, err := p.root.Lstat(p.name)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s", errPluginDirReplaced, p.path)
	}
	if err != nil {
		return fmt.Errorf("inspect plugin directory: %w", err)
	}
	if entry.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return fmt.Errorf("%w: plugin directory %s", pathjail.ErrReparse, p.path)
	}
	if !os.SameFile(entry, opened) {
		return fmt.Errorf("%w: %s", errPluginDirReplaced, p.path)
	}
	return nil
}

// openPluginDir pins the plugin directory discovery found, confined to the
// plugin's install root.
//
// A link swapped in before the open is refused here. One swapped in after it
// cannot redirect anything, because every later read, write, and hash is
// relative to the returned handle rather than to a path; verifyLive then
// detects that the path no longer names the edited directory.
func openPluginDir(plugin LoadedPlugin) (*pinnedPluginDir, error) {
	root, name, err := pathjail.Open(plugin.Root, plugin.PluginDir)
	if err != nil {
		return nil, fmt.Errorf("open plugin directory: %w", err)
	}
	if name == "." || filepath.Base(name) != name {
		_ = root.Close()
		return nil, fmt.Errorf("%w: plugin directory %s is not an entry of %s", pathjail.ErrEscapes, plugin.PluginDir, plugin.Root)
	}
	if err := pathjail.RefuseReparse(root, name); err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("open plugin directory: %w", err)
	}
	dir, err := root.OpenRoot(name)
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("open plugin directory: %w", err)
	}
	pinned := &pinnedPluginDir{root: root, name: name, path: plugin.PluginDir, dir: dir}
	if err := pinned.verifyLive(); err != nil {
		pinned.Close()
		return nil, err
	}
	return pinned, nil
}

// readManifest reads name from dir and returns its bytes and permission bits.
// Failing on a stat error keeps the publish from widening the permissions.
func readManifest(dir *os.Root, name string) ([]byte, os.FileMode, error) {
	file, err := dir.Open(name)
	if err != nil {
		return nil, 0, fmt.Errorf("read plugin manifest: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, 0, fmt.Errorf("inspect plugin manifest: %w", err)
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		return nil, 0, fmt.Errorf("read plugin manifest: %w", err)
	}
	return raw, info.Mode().Perm(), nil
}

// publishManifest replaces name in dir with data by renaming a complete
// temporary sibling over it, so a concurrent reader sees either the old
// manifest or the new one, never a partial write.
func publishManifest(dir *os.Root, name string, data []byte, perm os.FileMode) error {
	temp, tempName, err := pathjail.CreateTemp(dir, ".", ".zero-manifest", "")
	if err != nil {
		return fmt.Errorf("write plugin manifest: %w", err)
	}
	defer func() { _ = dir.Remove(tempName) }()
	if err := temp.Chmod(perm); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write plugin manifest: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write plugin manifest: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write plugin manifest: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("write plugin manifest: %w", err)
	}
	if err := dir.Rename(tempName, name); err != nil {
		return fmt.Errorf("write plugin manifest: %w", err)
	}
	return nil
}

// cleanLockEntry reports whether the pinned plugin directory dir still matches
// the lockfile entry recorded for the plugin and returns both for a refresh.
func cleanLockEntry(plugin LoadedPlugin, dir *os.Root) (map[string]LockEntry, LockEntry, bool) {
	lock, err := ReadLock(plugin.Root)
	if err != nil {
		return nil, LockEntry{}, false
	}
	entry, ok := lock[plugin.ID]
	if !ok {
		return nil, LockEntry{}, false
	}
	current, err := hashRootedTree(dir)
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
	lastInvalid := false
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
			// Only the last occurrence of a duplicate key counts, as in the
			// loader's json.Unmarshal, so an earlier non-boolean value that a
			// later one overwrites is not an error. Remember it and decide after
			// the last member.
			if err := skipManifestComposite(dec, valueTok); err != nil {
				return nil, false, fmt.Errorf("parse plugin manifest: %w", err)
			}
			lastInvalid = true
			continue
		}
		lastInvalid = false
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
	if lastInvalid {
		return nil, false, ManifestError{FieldPath: "enabled", Message: "Expected a boolean."}
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
	return skipManifestComposite(dec, tok)
}

// skipManifestComposite consumes the rest of the value whose first token tok
// was just read from dec. A scalar is already complete; an object or array is
// consumed through its matching closing delimiter.
func skipManifestComposite(dec *json.Decoder, tok json.Token) error {
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
