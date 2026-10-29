package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Twigpine/zero/internal/fsutil"
)

type Event string
type DiagnosticKind string
type ConfigSource string
type AuditStatus string

const (
	EventBeforeTool      Event = "beforeTool"
	EventAfterTool       Event = "afterTool"
	EventSessionStart    Event = "sessionStart"
	EventSessionEnd      Event = "sessionEnd"
	EventSpecialistStart Event = "specialistStart"
	EventSpecialistStop  Event = "specialistStop"
)

const (
	DiagnosticIO        DiagnosticKind = "io"
	DiagnosticJSON      DiagnosticKind = "json"
	DiagnosticSchema    DiagnosticKind = "schema"
	DiagnosticDuplicate DiagnosticKind = "duplicate"
)

const (
	SourceUser    ConfigSource = "user"
	SourceProject ConfigSource = "project"
)

const (
	AuditCompleted AuditStatus = "completed"
	AuditError     AuditStatus = "error"
	AuditBlocked   AuditStatus = "blocked"
)

type Command struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

type Definition struct {
	ID          string   `json:"id"`
	Name        string   `json:"name,omitempty"`
	Description string   `json:"description,omitempty"`
	Event       Event    `json:"event"`
	Matcher     string   `json:"matcher,omitempty"`
	Command     string   `json:"command"`
	Args        []string `json:"args"`
	Enabled     bool     `json:"enabled"`
}

type Config struct {
	Enabled bool         `json:"enabled"`
	Hooks   []Definition `json:"hooks"`
}

type Diagnostic struct {
	Kind      DiagnosticKind `json:"kind"`
	Message   string         `json:"message"`
	Source    ConfigSource   `json:"source,omitempty"`
	Path      string         `json:"path,omitempty"`
	HookID    string         `json:"hookId,omitempty"`
	FieldPath string         `json:"fieldPath,omitempty"`
}

type Paths struct {
	UserConfigPath    string `json:"userConfigPath"`
	ProjectConfigPath string `json:"projectConfigPath"`
	AuditPath         string `json:"auditPath"`
}

type LoadResult struct {
	Config      Config       `json:"config"`
	Diagnostics []Diagnostic `json:"diagnostics"`
	Paths       Paths        `json:"paths"`
}

type ResolvePathOptions struct {
	Cwd string
	Env map[string]string
}

type LoadOptions struct {
	Cwd               string
	Env               map[string]string
	UserConfigPath    string
	ProjectConfigPath string
	// ExcludeProject drops the project layer (./.zero/hooks.json) so only the
	// user layer loads. Its zero value (false) preserves the user+project merge.
	ExcludeProject bool
}

type StoreOptions struct {
	ConfigPath string
}

type SelectInput struct {
	Event    Event
	ToolName string
}

type AuditCommand = Command

type AuditResult struct {
	ExitCode int    `json:"exitCode"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
}

type AuditEvent struct {
	Sequence   int            `json:"sequence"`
	CreatedAt  string         `json:"createdAt"`
	Type       string         `json:"type"`
	HookID     string         `json:"hookId"`
	Event      Event          `json:"event"`
	Matcher    string         `json:"matcher,omitempty"`
	ToolCallID string         `json:"toolCallId,omitempty"`
	Commands   []AuditCommand `json:"commands,omitempty"`
	Status     AuditStatus    `json:"status,omitempty"`
	Results    []AuditResult  `json:"results,omitempty"`
	DurationMs int            `json:"durationMs,omitempty"`
}

type AppendStartedInput struct {
	HookID     string
	Event      Event
	Matcher    string
	ToolCallID string
	Commands   []AuditCommand
}

type AppendCompletedInput struct {
	HookID     string
	Event      Event
	Matcher    string
	ToolCallID string
	Status     AuditStatus
	Results    []AuditResult
	DurationMs int
}

type AuditStoreOptions struct {
	AuditPath string
	Now       func() time.Time
}

type manifestError struct {
	fieldPath string
	message   string
}

func (err manifestError) Error() string {
	if err.fieldPath == "" {
		return err.message
	}
	return err.fieldPath + ": " + err.message
}

type hookLayer struct {
	source         ConfigSource
	path           string
	config         Config
	enabledSet     bool
	hookEnabledSet map[string]bool
}

var (
	hookIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	storeLocksMu  sync.Mutex
	storeLocks    = map[string]*sync.Mutex{}
)

func ResolvePaths(options ResolvePathOptions) (Paths, error) {
	cwd, err := resolveCwd(options.Cwd)
	if err != nil {
		return Paths{}, err
	}

	home := strings.TrimSpace(firstNonEmpty(envValue(options.Env, "HOME"), envValue(options.Env, "USERPROFILE")))
	if home == "" {
		home, err = os.UserHomeDir()
		if err != nil {
			return Paths{}, fmt.Errorf("resolve user home: %w", err)
		}
	}

	configHome := resolveEnvDir(options.Env, "XDG_CONFIG_HOME", filepath.Join(home, ".config"), cwd)
	dataHome := resolveEnvDir(options.Env, "XDG_DATA_HOME", filepath.Join(home, ".local", "share"), cwd)
	return Paths{
		UserConfigPath:    filepath.Join(configHome, "zero", "hooks.json"),
		ProjectConfigPath: filepath.Join(cwd, ".zero", "hooks.json"),
		AuditPath:         filepath.Join(dataHome, "zero", "hooks", "audit.jsonl"),
	}, nil
}

func LoadConfig(options LoadOptions) (LoadResult, error) {
	paths, err := ResolvePaths(ResolvePathOptions{Cwd: options.Cwd, Env: options.Env})
	if err != nil {
		return LoadResult{}, err
	}
	userConfigPath := firstNonEmpty(options.UserConfigPath, paths.UserConfigPath)
	projectConfigPath := firstNonEmpty(options.ProjectConfigPath, paths.ProjectConfigPath)
	diagnostics := []Diagnostic{}
	layers := []hookLayer{}

	for _, candidate := range []struct {
		source ConfigSource
		path   string
	}{
		{source: SourceUser, path: userConfigPath},
		{source: SourceProject, path: projectConfigPath},
	} {
		if candidate.source == SourceProject && options.ExcludeProject {
			continue
		}
		layer, ok := readLayer(candidate.source, candidate.path, &diagnostics)
		if ok {
			layers = append(layers, layer)
		}
	}

	paths.UserConfigPath = userConfigPath
	paths.ProjectConfigPath = projectConfigPath
	return LoadResult{
		Config:      mergeLayers(layers, &diagnostics),
		Diagnostics: diagnostics,
		Paths:       paths,
	}, nil
}

// WriteConfig durably publishes config at path. It takes the same
// <path>.lock used by ConfigStore mutations, so a direct caller cannot
// interleave with a CLI mutation, and it replaces the file with a synced temp
// file, an atomic rename, and a parent-directory sync.
func WriteConfig(path string, config Config) (err error) {
	resolved, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	return withConfigLock(resolved, func() error {
		return writeConfigLocked(resolved, config)
	})
}

// withConfigLock serializes a path's read-modify-write across goroutines and
// processes. The in-process mutex is acquired first; the OS lock is held until
// fn returns. A release failure is joined into the result: reporting success
// while the lock is still held would let the next mutation fail or block
// unexpectedly. The kernel releases the lock on process exit, so the lock file
// is never removed.
func withConfigLock(path string, fn func() error) (err error) {
	lock := lockForPath(path)
	lock.Lock()
	defer lock.Unlock()

	unlock, err := configLockFileFn(path)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, unlock())
	}()
	return fn()
}

// writeConfigLocked normalizes and publishes config. The caller must hold the
// config lock for path.
func writeConfigLocked(path string, config Config) error {
	normalized, err := normalizeConfig(map[string]any{
		"enabled": config.Enabled,
		"hooks":   definitionsToRaw(config.Hooks),
	})
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(normalized, "", "  ")
	if err != nil {
		return err
	}
	return writeConfigData(path, append(data, '\n'))
}

// Seams allow tests to inject lock and persistence failures at each barrier.
var (
	configLockFileFn = lockConfigFile
	syncHooksFileFn  = (*os.File).Sync
	syncHooksDirFn   = syncHooksDir
)

// syncHooksDir fsyncs a directory so a rename/create within it is durable
// across a crash. Windows cannot sync directory handles, so the rename is
// best-effort durable there. A directory the user can write but not read
// cannot be opened for syncing either; that is also best effort, because the
// write it follows already succeeded. Every other failure is reported.
func syncHooksDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return nil
		}
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

// writeConfigData writes a complete temp file, syncs it, atomically replaces
// path, and syncs the directory. An interrupted in-place write would leave the
// user's only hooks config truncated or corrupt; the rename makes readers see
// either the old document or the new one, never a partial file.
func writeConfigData(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := ensureConfigDir(dir); err != nil {
		return err
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	tmp, err := os.CreateTemp(dir, ".hooks-*.tmp")
	if err != nil {
		return fmt.Errorf("write hooks config %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure hooks config permissions %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write hooks config %s: %w", path, err)
	}
	if err := syncHooksFileFn(tmp); err != nil {
		return fmt.Errorf("sync hooks config %s: %w", path, errors.Join(err, tmp.Close()))
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write hooks config %s: %w", path, err)
	}
	if err := fsutil.RenameWithRetry(tmpPath, path, nil); err != nil {
		return fmt.Errorf("replace hooks config %s: %w", path, err)
	}
	// The replacement is already visible if this fails; do not roll it back.
	if err := syncHooksDirFn(dir); err != nil {
		return fmt.Errorf("sync hooks config directory %s: %w", dir, err)
	}
	return nil
}

// ensureConfigDir creates dir and each missing ancestor 0700, syncing every
// new entry into its parent so a crash cannot lose the directory (and with it
// the hooks config) after a successful write. Components that appeared between
// the lookup and the mkdir belong to another writer and are not rolled back.
// Only directories created by this call are removed if a barrier fails.
func ensureConfigDir(dir string) error {
	if dir == "." || dir == "" {
		return nil
	}
	missing := missingConfigDirs(dir)
	if len(missing) == 0 {
		return nil
	}
	var created []string
	for i := len(missing) - 1; i >= 0; i-- {
		d := missing[i]
		err := os.Mkdir(d, 0o700)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return errors.Join(fmt.Errorf("create hooks config directory %s: %w", d, err), removeConfigDirs(created))
		}
		created = append(created, d)
	}
	for _, d := range missing {
		parent := filepath.Dir(d)
		if err := syncHooksDirFn(parent); err != nil {
			return errors.Join(fmt.Errorf("sync hooks config directory %s: %w", parent, err), removeConfigDirs(created))
		}
	}
	return nil
}

// missingConfigDirs lists dir and each ancestor that does not exist yet,
// deepest first, so ensureConfigDir can create them shallowest first and sync
// each new entry into its parent deepest first.
func missingConfigDirs(dir string) []string {
	var missing []string
	for d := dir; ; {
		if _, err := os.Lstat(d); !errors.Is(err, os.ErrNotExist) {
			return missing
		}
		missing = append(missing, d)
		parent := filepath.Dir(d)
		if parent == d {
			return missing
		}
		d = parent
	}
}

// removeConfigDirs rolls back directories ensureConfigDir created in this call,
// deepest first. os.Remove refuses a non-empty directory, so an entry another
// process already populated survives and its error is reported.
func removeConfigDirs(created []string) error {
	var errs []error
	for i := len(created) - 1; i >= 0; i-- {
		if err := os.Remove(created[i]); err != nil {
			errs = append(errs, fmt.Errorf("roll back hooks config directory %s: %w", created[i], err))
		}
	}
	return errors.Join(errs...)
}

type ConfigStore struct {
	configPath string
}

func NewConfigStore(options StoreOptions) (*ConfigStore, error) {
	path := options.ConfigPath
	if strings.TrimSpace(path) == "" {
		paths, err := ResolvePaths(ResolvePathOptions{})
		if err != nil {
			return nil, err
		}
		path = paths.ProjectConfigPath
	}
	resolved, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return &ConfigStore{configPath: resolved}, nil
}

func (store *ConfigStore) List() (Config, error) {
	return readSingleConfig(store.configPath)
}

func (store *ConfigStore) Upsert(hook Definition) (Definition, error) {
	var saved Definition
	err := withConfigLock(store.configPath, func() error {
		config, err := store.List()
		if err != nil {
			return err
		}
		raw := definitionToRaw(hook)
		// Upsert treats the zero-value Enabled field as omitted so new hooks default
		// to enabled. Disable persisted hooks explicitly with SetEnabled.
		if !hook.Enabled {
			delete(raw, "enabled")
		}
		normalized, err := normalizeDefinition(raw, "hooks.0")
		if err != nil {
			return err
		}
		next := make([]Definition, 0, len(config.Hooks)+1)
		for _, existing := range config.Hooks {
			if existing.ID != normalized.ID {
				next = append(next, existing)
			}
		}
		next = append(next, normalized)
		sortDefinitions(next)
		config.Hooks = next
		if err := writeConfigLocked(store.configPath, config); err != nil {
			return err
		}
		saved = normalized
		return nil
	})
	if err != nil {
		return Definition{}, err
	}
	return saved, nil
}

func (store *ConfigStore) Remove(hookID string) (bool, error) {
	// A no-op must not create .zero/ or the lock file: removing an unknown hook
	// from a project with no hooks config should leave the working tree alone.
	// A concurrent Upsert that creates the file is simply ordered after this
	// read.
	if _, err := os.Lstat(store.configPath); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	removed := false
	err := withConfigLock(store.configPath, func() error {
		config, err := store.List()
		if err != nil {
			return err
		}
		next := make([]Definition, 0, len(config.Hooks))
		for _, hook := range config.Hooks {
			if hook.ID == hookID {
				removed = true
				continue
			}
			next = append(next, hook)
		}
		if !removed {
			return nil
		}
		config.Hooks = next
		return writeConfigLocked(store.configPath, config)
	})
	return removed, err
}

func (store *ConfigStore) SetEnabled(hookID string, enabled bool) (bool, error) {
	// See Remove: changing an unknown hook must not create config state.
	if _, err := os.Lstat(store.configPath); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	changed := false
	err := withConfigLock(store.configPath, func() error {
		config, err := store.List()
		if err != nil {
			return err
		}
		for index := range config.Hooks {
			if config.Hooks[index].ID == hookID {
				config.Hooks[index].Enabled = enabled
				changed = true
			}
		}
		if !changed {
			return nil
		}
		return writeConfigLocked(store.configPath, config)
	})
	return changed, err
}

func Select(config Config, input SelectInput) []Definition {
	if !config.Enabled {
		return []Definition{}
	}
	selected := []Definition{}
	for _, hook := range config.Hooks {
		if !hook.Enabled || hook.Event != input.Event {
			continue
		}
		if hook.Matcher == "" {
			selected = append(selected, hook)
			continue
		}
		if input.ToolName != "" && matchesHookMatcher(hook.Matcher, input.ToolName) {
			selected = append(selected, hook)
		}
	}
	return selected
}

func FormatList(config Config, diagnostics []Diagnostic) string {
	state := "disabled"
	if config.Enabled {
		state = "enabled"
	}
	lines := []string{"Zero Hooks: " + state}
	if len(config.Hooks) == 0 {
		lines = append(lines, "  No hooks configured.")
	} else {
		for _, hook := range config.Hooks {
			matcher := ""
			if hook.Matcher != "" {
				matcher = " " + hook.Matcher
			}
			command := strings.Join(append([]string{hook.Command}, hook.Args...), " ")
			hookState := "disabled"
			if hook.Enabled {
				hookState = "enabled"
			}
			lines = append(lines, fmt.Sprintf("  %s [%s%s] %s - %s", hook.ID, hook.Event, matcher, hookState, command))
		}
	}
	if len(diagnostics) > 0 {
		lines = append(lines, "Hook diagnostics:")
		for _, diagnostic := range diagnostics {
			lines = append(lines, fmt.Sprintf("  [%s] %s", diagnostic.Kind, diagnostic.Message))
		}
	}
	return strings.Join(lines, "\n")
}

type AuditStore struct {
	auditPath string
	now       func() time.Time
	mu        sync.Mutex
}

func NewAuditStore(options AuditStoreOptions) (*AuditStore, error) {
	path := options.AuditPath
	if strings.TrimSpace(path) == "" {
		paths, err := ResolvePaths(ResolvePathOptions{})
		if err != nil {
			return nil, err
		}
		path = paths.AuditPath
	}
	resolved, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &AuditStore{auditPath: resolved, now: now}, nil
}

func (store *AuditStore) AppendStarted(input AppendStartedInput) (AuditEvent, error) {
	return store.append(AuditEvent{
		Type:       "hook_execution_started",
		HookID:     input.HookID,
		Event:      input.Event,
		Matcher:    input.Matcher,
		ToolCallID: input.ToolCallID,
		Commands:   input.Commands,
	})
}

func (store *AuditStore) AppendCompleted(input AppendCompletedInput) (AuditEvent, error) {
	return store.append(AuditEvent{
		Type:       "hook_execution_completed",
		HookID:     input.HookID,
		Event:      input.Event,
		Matcher:    input.Matcher,
		ToolCallID: input.ToolCallID,
		Status:     input.Status,
		Results:    input.Results,
		DurationMs: input.DurationMs,
	})
}

// ReadEvents deliberately does not acquire store.mu. append holds store.mu while
// writing a single O_APPEND JSONL record, and lock-free readers may miss or skip
// an in-progress append while still avoiding deadlocks with append's read step.
func (store *AuditStore) ReadEvents() ([]AuditEvent, error) {
	data, err := os.ReadFile(store.auditPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []AuditEvent{}, nil
		}
		return nil, err
	}
	events := []AuditEvent{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var event AuditEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		events = append(events, event)
	}
	return events, nil
}

func (store *AuditStore) append(event AuditEvent) (AuditEvent, error) {
	store.mu.Lock()
	defer store.mu.Unlock()

	// Cross-process lock around the read-then-append. store.mu only serializes
	// THIS process; the audit log is a shared global file, so without a
	// cross-process lock two processes could both read sequence N via
	// lastSequence() and both write N+1 (duplicate sequences). Held only for the
	// millisecond read+append, mirroring the cron/oauth file locks.
	unlock, err := store.lockAudit()
	if err != nil {
		return AuditEvent{}, err
	}
	defer unlock()

	highest, err := store.lastSequence()
	if err != nil {
		return AuditEvent{}, err
	}
	event.Sequence = highest + 1
	event.CreatedAt = store.now().UTC().Format(time.RFC3339Nano)

	if err := os.MkdirAll(filepath.Dir(store.auditPath), 0o700); err != nil {
		return AuditEvent{}, err
	}
	data, err := json.Marshal(event)
	if err != nil {
		return AuditEvent{}, err
	}
	file, err := os.OpenFile(store.auditPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return AuditEvent{}, err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		return AuditEvent{}, err
	}
	if err := file.Close(); err != nil {
		return AuditEvent{}, err
	}
	return event, nil
}

// lastSequence returns the Sequence of the last record in the append-only audit
// log (or 0 when empty/absent). It reads only the file's TAIL rather than the
// whole file, so append stays O(1) instead of O(n) per call (the previous code
// re-read and scanned every event on every append → O(n²) over a session). It
// reads fresh from disk each time so a concurrent process's prior appends are
// observed; the read-then-append is made atomic across processes by the caller's
// lockAudit — store.mu alone would not prevent a cross-process sequence collision.
func (store *AuditStore) lastSequence() (int, error) {
	file, err := os.Open(store.auditPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	size := info.Size()
	if size == 0 {
		return 0, nil
	}
	const tailWindow = 8 * 1024
	start := size - tailWindow
	if start < 0 {
		start = 0
	}
	buf := make([]byte, size-start)
	// ReadAt may return io.EOF with a short read if the file shrank between Stat
	// and here; treat EOF as non-fatal and use only the bytes actually read.
	n, rerr := file.ReadAt(buf, start)
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		return 0, rerr
	}
	buf = buf[:n]
	if len(buf) == 0 {
		return 0, nil
	}
	text := strings.TrimRight(string(buf), "\r\n \t")
	if text == "" {
		return 0, nil
	}
	if idx := strings.LastIndexByte(text, '\n'); idx >= 0 {
		text = strings.TrimSpace(text[idx+1:])
	} else if start > 0 {
		// The last record is longer than the tail window (rare) — fall back to a
		// full scan so the sequence stays correct.
		return store.lastSequenceFullScan()
	}
	if text == "" {
		return 0, nil
	}
	var record AuditEvent
	if err := json.Unmarshal([]byte(text), &record); err != nil {
		return 0, err
	}
	return record.Sequence, nil
}

func (store *AuditStore) lastSequenceFullScan() (int, error) {
	events, err := store.ReadEvents()
	if err != nil {
		return 0, err
	}
	highest := 0
	for _, existing := range events {
		if existing.Sequence > highest {
			highest = existing.Sequence
		}
	}
	return highest, nil
}

func readLayer(source ConfigSource, path string, diagnostics *[]Diagnostic) (hookLayer, bool) {
	resolved, err := filepath.Abs(path)
	if err == nil {
		path = resolved
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return hookLayer{}, false
		}
		*diagnostics = append(*diagnostics, Diagnostic{Kind: DiagnosticIO, Source: source, Path: path, Message: err.Error()})
		return hookLayer{}, false
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		*diagnostics = append(*diagnostics, Diagnostic{Kind: DiagnosticJSON, Source: source, Path: path, Message: err.Error()})
		return hookLayer{}, false
	}
	enabledSet, hookEnabledSet := extractLayerEnabledMarkers(raw)
	config, err := normalizeConfig(raw)
	if err != nil {
		*diagnostics = append(*diagnostics, toDiagnostic(err, source, path))
		return hookLayer{}, false
	}
	return hookLayer{source: source, path: path, config: config, enabledSet: enabledSet, hookEnabledSet: hookEnabledSet}, true
}

func readSingleConfig(path string) (Config, error) {
	diagnostics := []Diagnostic{}
	layer, ok := readLayer(SourceProject, path, &diagnostics)
	if len(diagnostics) > 0 {
		diagnostic := diagnostics[0]
		return Config{}, fmt.Errorf("invalid Zero hook config at %s: %s", diagnostic.Path, diagnostic.Message)
	}
	if !ok {
		return Config{Enabled: true, Hooks: []Definition{}}, nil
	}
	return layer.config, nil
}

func mergeLayers(layers []hookLayer, diagnostics *[]Diagnostic) Config {
	enabled := true
	byID := map[string]Definition{}
	sourceByID := map[string]hookLayer{}
	for _, layer := range layers {
		if layer.enabledSet {
			enabled = layer.config.Enabled
		}
		for _, hook := range layer.config.Hooks {
			if previous, ok := sourceByID[hook.ID]; ok {
				*diagnostics = append(*diagnostics, Diagnostic{
					Kind:    DiagnosticDuplicate,
					Source:  layer.source,
					Path:    layer.path,
					HookID:  hook.ID,
					Message: fmt.Sprintf("Hook %q from %s overrides %s hook at %s.", hook.ID, layer.source, previous.source, previous.path),
				})
				if !layer.hookEnabledSet[hook.ID] {
					hook.Enabled = byID[hook.ID].Enabled
				}
			}
			byID[hook.ID] = hook
			sourceByID[hook.ID] = layer
		}
	}
	hooks := make([]Definition, 0, len(byID))
	for _, hook := range byID {
		hooks = append(hooks, hook)
	}
	sortDefinitions(hooks)
	return Config{Enabled: enabled, Hooks: hooks}
}

func extractLayerEnabledMarkers(raw any) (bool, map[string]bool) {
	hookEnabledSet := map[string]bool{}
	obj, ok := raw.(map[string]any)
	if !ok {
		return false, hookEnabledSet
	}
	_, enabledSet := obj["enabled"]
	items, ok := obj["hooks"].([]any)
	if !ok {
		return enabledSet, hookEnabledSet
	}
	for _, item := range items {
		hook, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id, ok := hook["id"].(string)
		if !ok {
			continue
		}
		if _, ok := hook["enabled"]; ok {
			hookEnabledSet[strings.TrimSpace(id)] = true
		}
	}
	return enabledSet, hookEnabledSet
}

func normalizeConfig(raw any) (Config, error) {
	if raw == nil {
		raw = map[string]any{}
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return Config{}, manifestError{message: "Expected hooks config to be a JSON object."}
	}
	enabled := true
	if rawEnabled, ok := obj["enabled"]; ok {
		parsed, ok := rawEnabled.(bool)
		if !ok {
			return Config{}, manifestError{fieldPath: "enabled", message: "Expected a boolean."}
		}
		enabled = parsed
	}
	items, err := optionalArray(obj["hooks"], "hooks")
	if err != nil {
		return Config{}, err
	}
	definitions := make([]Definition, 0, len(items))
	for index, item := range items {
		definition, err := normalizeDefinition(item, fmt.Sprintf("hooks.%d", index))
		if err != nil {
			return Config{}, err
		}
		definitions = append(definitions, definition)
	}
	sortDefinitions(definitions)
	return Config{Enabled: enabled, Hooks: definitions}, nil
}

func normalizeDefinition(raw any, field string) (Definition, error) {
	obj, ok := raw.(map[string]any)
	if !ok {
		return Definition{}, manifestError{fieldPath: field, message: "Expected hook definition to be an object."}
	}
	id, err := requiredID(obj, field+".id")
	if err != nil {
		return Definition{}, err
	}
	name, err := optionalString(obj, field+".name")
	if err != nil {
		return Definition{}, err
	}
	description, err := optionalString(obj, field+".description")
	if err != nil {
		return Definition{}, err
	}
	event, err := parseEvent(obj["event"], field+".event")
	if err != nil {
		return Definition{}, err
	}
	matcher, err := optionalString(obj, field+".matcher")
	if err != nil {
		return Definition{}, err
	}
	if matcher != "" && !eventSupportsMatcher(event) {
		return Definition{}, manifestError{fieldPath: field + ".matcher", message: "matcher is only supported for beforeTool and afterTool hooks."}
	}
	command, err := requiredString(obj, field+".command")
	if err != nil {
		return Definition{}, err
	}
	args, err := optionalStringArray(obj["args"], field+".args")
	if err != nil {
		return Definition{}, err
	}
	enabled := true
	if rawEnabled, ok := obj["enabled"]; ok {
		parsed, ok := rawEnabled.(bool)
		if !ok {
			return Definition{}, manifestError{fieldPath: field + ".enabled", message: "Expected a boolean."}
		}
		enabled = parsed
	}
	return Definition{ID: id, Name: name, Description: description, Event: event, Matcher: matcher, Command: command, Args: args, Enabled: enabled}, nil
}

func matchesHookMatcher(matcher string, toolName string) bool {
	if matcher == "*" {
		return true
	}
	if !strings.Contains(matcher, "*") {
		return matcher == toolName
	}
	segments := strings.Split(matcher, "*")
	cursor := 0
	searchEnd := len(toolName)
	if !strings.HasPrefix(matcher, "*") {
		first := segments[0]
		segments = segments[1:]
		if !strings.HasPrefix(toolName, first) {
			return false
		}
		cursor = len(first)
	}
	if !strings.HasSuffix(matcher, "*") {
		last := segments[len(segments)-1]
		segments = segments[:len(segments)-1]
		if !strings.HasSuffix(toolName, last) {
			return false
		}
		searchEnd = len(toolName) - len(last)
	}
	for _, segment := range segments {
		if segment == "" {
			continue
		}
		index := strings.Index(toolName[cursor:], segment)
		if index < 0 {
			return false
		}
		cursor += index + len(segment)
		if cursor > searchEnd {
			return false
		}
	}
	return cursor <= searchEnd
}

func eventSupportsMatcher(event Event) bool {
	return event == EventBeforeTool || event == EventAfterTool
}

func toDiagnostic(err error, source ConfigSource, path string) Diagnostic {
	var manifestErr manifestError
	if errors.As(err, &manifestErr) {
		return Diagnostic{Kind: DiagnosticSchema, Source: source, Path: path, FieldPath: manifestErr.fieldPath, Message: manifestErr.message}
	}
	return Diagnostic{Kind: DiagnosticSchema, Source: source, Path: path, Message: err.Error()}
}

func requiredString(obj map[string]any, field string) (string, error) {
	value, ok := obj[lastPathSegment(field)]
	if !ok {
		return "", manifestError{fieldPath: field, message: "Expected a non-empty string."}
	}
	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return "", manifestError{fieldPath: field, message: "Expected a non-empty string."}
	}
	return strings.TrimSpace(text), nil
}

func optionalString(obj map[string]any, field string) (string, error) {
	value, ok := obj[lastPathSegment(field)]
	if !ok || value == nil {
		return "", nil
	}
	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return "", manifestError{fieldPath: field, message: "Expected a non-empty string."}
	}
	return strings.TrimSpace(text), nil
}

func requiredID(obj map[string]any, field string) (string, error) {
	value, err := requiredString(obj, field)
	if err != nil {
		return "", err
	}
	if !hookIDPattern.MatchString(value) {
		return "", manifestError{fieldPath: field, message: "Use letters, numbers, dots, dashes, or underscores."}
	}
	return value, nil
}

// KnownEvents returns the hook events Zero recognizes, in dispatch order.
func KnownEvents() []Event {
	return []Event{EventBeforeTool, EventAfterTool, EventSessionStart, EventSessionEnd, EventSpecialistStart, EventSpecialistStop}
}

// IsValidEvent reports whether event is one Zero recognizes.
func IsValidEvent(event Event) bool {
	for _, known := range KnownEvents() {
		if event == known {
			return true
		}
	}
	return false
}

func parseEvent(raw any, field string) (Event, error) {
	text, ok := raw.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return "", manifestError{fieldPath: field, message: "Expected a hook event."}
	}
	event := Event(strings.TrimSpace(text))
	if !IsValidEvent(event) {
		return "", manifestError{fieldPath: field, message: "Expected beforeTool, afterTool, sessionStart, sessionEnd, specialistStart, or specialistStop."}
	}
	return event, nil
}

func optionalArray(raw any, field string) ([]any, error) {
	if raw == nil {
		return []any{}, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, manifestError{fieldPath: field, message: "Expected an array."}
	}
	return items, nil
}

func optionalStringArray(raw any, field string) ([]string, error) {
	if raw == nil {
		return []string{}, nil
	}
	if values, ok := raw.([]string); ok {
		return append([]string{}, values...), nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, manifestError{fieldPath: field, message: "Expected an array."}
	}
	values := make([]string, 0, len(items))
	for index, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, manifestError{fieldPath: fmt.Sprintf("%s.%d", field, index), message: "Expected a string."}
		}
		values = append(values, text)
	}
	return values, nil
}

func definitionsToRaw(definitions []Definition) []any {
	values := make([]any, 0, len(definitions))
	for _, definition := range definitions {
		values = append(values, definitionToRaw(definition))
	}
	return values
}

func definitionToRaw(definition Definition) map[string]any {
	value := map[string]any{
		"id":      definition.ID,
		"event":   string(definition.Event),
		"command": definition.Command,
		"args":    definition.Args,
		"enabled": definition.Enabled,
	}
	if definition.Name != "" {
		value["name"] = definition.Name
	}
	if definition.Description != "" {
		value["description"] = definition.Description
	}
	if definition.Matcher != "" {
		value["matcher"] = definition.Matcher
	}
	return value
}

func sortDefinitions(definitions []Definition) {
	sort.Slice(definitions, func(left int, right int) bool {
		return definitions[left].ID < definitions[right].ID
	})
}

func lockForPath(path string) *sync.Mutex {
	storeLocksMu.Lock()
	defer storeLocksMu.Unlock()
	lock := storeLocks[path]
	if lock == nil {
		lock = &sync.Mutex{}
		storeLocks[path] = lock
	}
	return lock
}

func resolveEnvDir(env map[string]string, key string, fallback string, cwd string) string {
	value := strings.TrimSpace(envValue(env, key))
	if value == "" {
		return fallback
	}
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Join(cwd, value)
}

func resolveCwd(cwd string) (string, error) {
	if strings.TrimSpace(cwd) == "" {
		return os.Getwd()
	}
	if filepath.IsAbs(cwd) {
		return filepath.Clean(cwd), nil
	}
	return filepath.Abs(cwd)
}

func envValue(env map[string]string, key string) string {
	if env != nil {
		return env[key]
	}
	return os.Getenv(key)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func lastPathSegment(field string) string {
	if index := strings.LastIndex(field, "."); index >= 0 {
		return field[index+1:]
	}
	return field
}
