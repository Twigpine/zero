package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Twigpine/zero/internal/execution"
	"github.com/Twigpine/zero/internal/redaction"
)

const (
	grantSchemaVersion = 3
	grantLockTimeout   = 5 * time.Second
	grantLockRetry     = 10 * time.Millisecond
)

type Grant struct {
	ToolName   string        `json:"toolName"`
	Scope      string        `json:"scope,omitempty"`     // absolute path, host, or "" for tool-wide
	ScopeKind  ScopeKind     `json:"scopeKind,omitempty"` // file | dir | host | "" (tool-wide)
	Decision   GrantDecision `json:"decision"`
	ApprovedAt string        `json:"approvedAt"`
	Reason     string        `json:"reason,omitempty"`
	Session    bool          `json:"session,omitempty"`
}

type StoreOptions struct {
	FilePath string
	Now      func() time.Time
	Env      map[string]string
}

type GrantInput struct {
	ToolName string
	Decision GrantDecision
	Reason   string
	// Scope is the raw path or host the grant covers; ScopeKind classifies it.
	// engine.Grant resolves path scopes to absolute paths and normalizes host
	// scopes before persisting. Both empty means a tool-wide grant.
	Scope     string
	ScopeKind ScopeKind
}

type GrantLookup struct {
	Matched bool  `json:"matched"`
	Grant   Grant `json:"grant,omitempty"`
}

type grantFile struct {
	SchemaVersion   int                             `json:"schemaVersion"`
	PolicyVersion   int                             `json:"policyVersion"`
	Grants          map[string][]Grant              `json:"grants"`
	CommandPrefixes map[string][]CommandPrefixGrant `json:"commandPrefixes,omitempty"`
	Migration       *grantMigration                 `json:"migration,omitempty"`
}

type grantMigration struct {
	FromSchemaVersion int    `json:"fromSchemaVersion"`
	BackupPath        string `json:"backupPath"`
	Migrated          int    `json:"migrated"`
	Invalidated       int    `json:"invalidated"`
	NoticePending     bool   `json:"noticePending"`
}

type GrantStore struct {
	filePath string
	now      func() time.Time
	mu       sync.Mutex
}

var toolGrantNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func ResolveGrantPath(env map[string]string) (string, error) {
	override := strings.TrimSpace(envValue(env, "ZERO_SANDBOX_GRANTS_PATH"))
	if override != "" {
		if filepath.IsAbs(override) {
			return filepath.Clean(override), nil
		}
		return filepath.Abs(override)
	}
	configHome := strings.TrimSpace(envValue(env, "XDG_CONFIG_HOME"))
	if configHome == "" {
		home := strings.TrimSpace(firstNonEmpty(envValue(env, "HOME"), envValue(env, "USERPROFILE")))
		var err error
		if home == "" {
			home, err = os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("resolve user home: %w", err)
			}
		}
		configHome = filepath.Join(home, ".config")
	} else if !filepath.IsAbs(configHome) {
		resolved, err := filepath.Abs(configHome)
		if err != nil {
			return "", err
		}
		configHome = resolved
	}
	return filepath.Join(configHome, "zero", "sandbox-grants.json"), nil
}

func NewGrantStore(options StoreOptions) (*GrantStore, error) {
	filePath := strings.TrimSpace(options.FilePath)
	var err error
	if filePath == "" {
		filePath, err = ResolveGrantPath(options.Env)
		if err != nil {
			return nil, err
		}
	}
	if !filepath.IsAbs(filePath) {
		filePath, err = filepath.Abs(filePath)
		if err != nil {
			return nil, err
		}
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &GrantStore{filePath: filepath.Clean(filePath), now: now}, nil
}

func (store *GrantStore) FilePath() string {
	return store.filePath
}

// ConsumeMigrationNotice returns the legacy-grant migration summary once and
// atomically records it as seen. Frontends call this during startup so users are
// told when approvals were invalidated instead of encountering unexplained
// re-prompts. The original file remains available at the reported backup path.
func (store *GrantStore) ConsumeMigrationNotice() (string, error) {
	if store == nil {
		return "", nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	// Look for a pending notice WITHOUT taking the interprocess lock. Acquiring
	// that lock creates the grants directory and opens <grants>.lockfile, so doing
	// it unconditionally would make this call require a WRITABLE grants directory
	// even when it has nothing to write. Every startup and every `zero exec` calls
	// this, so a user with no grants file whose configured grants path (or its
	// parent) sits on a read-only mount would fail outright with "failed to
	// migrate sandbox grants", where reading alone reports an empty state and
	// writes nothing.
	state, needsMigration, err := store.readStateFile(false)
	if err != nil {
		return "", err
	}
	if !needsMigration && (state.Migration == nil || !state.Migration.NoticePending) {
		return "", nil
	}
	// Past here a write is genuinely required — either a schema/policy migration,
	// which only a lock holder may perform, or clearing NoticePending — so the
	// lock is warranted and a read-only grants path legitimately fails (as it did
	// before this lock existed, via readState's own migration path).
	//
	// Clearing the pending flag is a read-modify-write like any other mutator, so
	// it takes the same interprocess lock: two frontends starting at once must not
	// both read NoticePending, both print the notice, and clobber each other's
	// write of the rest of the state.
	unlock, err := store.lockStateFile()
	if err != nil {
		return "", err
	}
	defer unlock()
	// Re-read under the lock. The unlocked peek above is only a snapshot: another
	// process may have consumed the notice or migrated the file while this one
	// waited, and when needsMigration was set that snapshot's contents are
	// meaningless by readStateFile's contract.
	state, err = store.readStateLocked()
	if err != nil {
		return "", err
	}
	if state.Migration == nil || !state.Migration.NoticePending {
		return "", nil
	}
	migration := *state.Migration
	state.Migration.NoticePending = false
	if err := store.writeState(state); err != nil {
		return "", err
	}
	return fmt.Sprintf("sandbox grants updated for policy v%d: migrated %d, invalidated %d; backup: %s", execution.PolicyVersion, migration.Migrated, migration.Invalidated, migration.BackupPath), nil
}

func (store *GrantStore) Grant(input GrantInput) (Grant, error) {
	grant, err := createGrant(input, store.now)
	if err != nil {
		return Grant{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	unlock, err := store.lockStateFile()
	if err != nil {
		return Grant{}, err
	}
	defer unlock()

	state, err := store.readStateLocked()
	if err != nil {
		return Grant{}, err
	}
	bucket := state.Grants[grant.ToolName]
	replaced := false
	for i := range bucket {
		// A re-grant of the same (scope, kind) updates the existing record rather
		// than accumulating duplicates.
		if bucket[i].Scope == grant.Scope && bucket[i].ScopeKind == grant.ScopeKind {
			bucket[i] = grant
			replaced = true
			break
		}
	}
	if !replaced {
		bucket = append(bucket, grant)
	}
	state.Grants[grant.ToolName] = bucket
	if err := store.writeState(state); err != nil {
		return Grant{}, err
	}
	return grant, nil
}

func (store *GrantStore) GrantCommandPrefix(input CommandPrefixInput) (CommandPrefixGrant, error) {
	grant, err := createCommandPrefixGrant(input, store.now)
	if err != nil {
		return CommandPrefixGrant{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	unlock, err := store.lockStateFile()
	if err != nil {
		return CommandPrefixGrant{}, err
	}
	defer unlock()

	state, err := store.readStateLocked()
	if err != nil {
		return CommandPrefixGrant{}, err
	}
	bucket := state.CommandPrefixes[grant.ToolName]
	replaced := false
	for i := range bucket {
		if sameStringSlice(bucket[i].Prefix, grant.Prefix) {
			bucket[i] = grant
			replaced = true
			break
		}
	}
	if !replaced {
		bucket = append(bucket, grant)
	}
	state.CommandPrefixes[grant.ToolName] = bucket
	if err := store.writeState(state); err != nil {
		return CommandPrefixGrant{}, err
	}
	return grant, nil
}

// Lookup returns the grant that governs a tool call whose normalized scope is
// reqScope (empty for a tool-wide request, e.g. a shell command with no cwd).
// Among the tool's grants that cover the request, a covering deny wins outright;
// otherwise the most-specific covering allow is returned.
func (store *GrantStore) Lookup(toolName string, reqScope string) (GrantLookup, error) {
	if err := ValidateToolName(toolName); err != nil {
		return GrantLookup{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	state, err := store.readState()
	if err != nil {
		return GrantLookup{}, err
	}
	bucket := state.Grants[strings.TrimSpace(toolName)]
	return lookupGrantBucket(bucket, reqScope), nil
}

func (store *GrantStore) LookupCommandPrefix(toolName string, command []string) (CommandPrefixGrant, bool, error) {
	if err := ValidateToolName(toolName); err != nil {
		return CommandPrefixGrant{}, false, err
	}
	if _, ok := NormalizeCommandPrefix(command); !ok {
		return CommandPrefixGrant{}, false, nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	state, err := store.readState()
	if err != nil {
		return CommandPrefixGrant{}, false, err
	}
	bucket := state.CommandPrefixes[strings.TrimSpace(toolName)]
	for _, grant := range bucket {
		if hasStringPrefix(command, grant.Prefix) {
			grant.Prefix = append([]string(nil), grant.Prefix...)
			return grant, true, nil
		}
	}
	return CommandPrefixGrant{}, false, nil
}

func lookupGrantBucket(bucket []Grant, reqScope string) GrantLookup {
	var bestAllow *Grant
	for i := range bucket {
		grant := bucket[i]
		if !grantCovers(grant, reqScope) {
			continue
		}
		if grant.Decision == GrantDeny {
			covering := grant
			return GrantLookup{Matched: true, Grant: covering}
		}
		if bestAllow == nil || moreSpecific(grant, *bestAllow) {
			covering := grant
			bestAllow = &covering
		}
	}
	if bestAllow != nil {
		return GrantLookup{Matched: true, Grant: *bestAllow}
	}
	return GrantLookup{}
}

func (store *GrantStore) List() ([]Grant, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	state, err := store.readState()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(state.Grants))
	for name := range state.Grants {
		names = append(names, name)
	}
	sort.Strings(names)
	grants := make([]Grant, 0, len(names))
	for _, name := range names {
		bucket := append([]Grant(nil), state.Grants[name]...)
		sort.Slice(bucket, func(i, j int) bool {
			if bucket[i].Scope != bucket[j].Scope {
				return bucket[i].Scope < bucket[j].Scope
			}
			return bucket[i].ScopeKind < bucket[j].ScopeKind
		})
		grants = append(grants, bucket...)
	}
	return grants, nil
}

func (store *GrantStore) ListCommandPrefixes() ([]CommandPrefixGrant, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	state, err := store.readState()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(state.CommandPrefixes))
	for name := range state.CommandPrefixes {
		names = append(names, name)
	}
	sort.Strings(names)
	grants := make([]CommandPrefixGrant, 0, len(names))
	for _, name := range names {
		bucket := append([]CommandPrefixGrant(nil), state.CommandPrefixes[name]...)
		sort.Slice(bucket, func(i, j int) bool {
			return strings.Join(bucket[i].Prefix, "\x00") < strings.Join(bucket[j].Prefix, "\x00")
		})
		for i := range bucket {
			bucket[i].Prefix = append([]string(nil), bucket[i].Prefix...)
		}
		grants = append(grants, bucket...)
	}
	return grants, nil
}

func (store *GrantStore) Revoke(toolName string) (int, error) {
	if err := ValidateToolName(toolName); err != nil {
		return 0, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	unlock, err := store.lockStateFile()
	if err != nil {
		return 0, err
	}
	defer unlock()

	state, err := store.readStateLocked()
	if err != nil {
		return 0, err
	}
	key := strings.TrimSpace(toolName)
	bucket, ok := state.Grants[key]
	prefixes := state.CommandPrefixes[key]
	if (!ok || len(bucket) == 0) && len(prefixes) == 0 {
		return 0, nil
	}
	count := len(bucket) + len(prefixes)
	delete(state.Grants, key)
	delete(state.CommandPrefixes, key)
	if err := store.writeState(state); err != nil {
		return 0, err
	}
	return count, nil
}

// RevokePath revokes only the grants for toolName whose scope matches scopePath
// (a file or directory grant), leaving tool-wide and other-path grants intact.
// scopePath is canonicalized to an absolute, cleaned path the same way stored
// scopes are, so the caller can pass either an absolute path or one relative to
// the working directory (or copy it from `grants list`). Returns the number of
// grants removed.
func (store *GrantStore) RevokePath(toolName string, scopePath string) (int, error) {
	if err := ValidateToolName(toolName); err != nil {
		return 0, err
	}
	target := resolveScopeAbs(scopePath, "")
	if target == "" {
		return 0, fmt.Errorf("a non-empty --path is required to revoke a single grant")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	unlock, err := store.lockStateFile()
	if err != nil {
		return 0, err
	}
	defer unlock()

	state, err := store.readStateLocked()
	if err != nil {
		return 0, err
	}
	key := strings.TrimSpace(toolName)
	bucket, ok := state.Grants[key]
	if !ok || len(bucket) == 0 {
		return 0, nil
	}
	kept := make([]Grant, 0, len(bucket))
	removed := 0
	for _, grant := range bucket {
		if grant.Scope == target {
			removed++
			continue
		}
		kept = append(kept, grant)
	}
	if removed == 0 {
		return 0, nil
	}
	if len(kept) == 0 {
		delete(state.Grants, key)
	} else {
		state.Grants[key] = kept
	}
	if err := store.writeState(state); err != nil {
		return 0, err
	}
	return removed, nil
}

func (store *GrantStore) Clear() (int, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	unlock, err := store.lockStateFile()
	if err != nil {
		return 0, err
	}
	defer unlock()

	state, err := store.readStateLocked()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, bucket := range state.Grants {
		count += len(bucket)
	}
	for _, bucket := range state.CommandPrefixes {
		count += len(bucket)
	}
	if count == 0 {
		return 0, nil
	}
	if err := store.writeState(emptyGrantState()); err != nil {
		return 0, err
	}
	return count, nil
}

func FormatGrantListWithCommandPrefixes(grants []Grant, prefixes []CommandPrefixGrant) string {
	if len(grants) == 0 && len(prefixes) == 0 {
		return "No persistent sandbox grants."
	}
	lines := []string{"Sandbox Grants:"}
	for _, grant := range grants {
		scope := grant.Scope
		if scope == "" {
			scope = "*" // tool-wide
		}
		line := fmt.Sprintf("  %s (%s) [%s] approved %s", grant.ToolName, scope, grant.Decision, grant.ApprovedAt)
		if grant.Reason != "" {
			line += " - " + redaction.RedactString(grant.Reason, redaction.Options{})
		}
		lines = append(lines, line)
	}
	for _, grant := range prefixes {
		line := fmt.Sprintf("  %s (`%s`) [command-prefix] approved %s", grant.ToolName, strings.Join(grant.Prefix, " "), grant.ApprovedAt)
		if grant.Reason != "" {
			line += " - " + redaction.RedactString(grant.Reason, redaction.Options{})
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func ValidateToolName(name string) error {
	trimmed := strings.TrimSpace(name)
	if !toolGrantNamePattern.MatchString(trimmed) {
		return fmt.Errorf("invalid sandbox tool name %q. Use letters, numbers, dots, dashes, or underscores", name)
	}
	return nil
}

func createGrant(input GrantInput, now func() time.Time) (Grant, error) {
	toolName := strings.TrimSpace(input.ToolName)
	if err := ValidateToolName(toolName); err != nil {
		return Grant{}, err
	}
	decision, err := NormalizeGrantDecision(input.Decision)
	if err != nil {
		return Grant{}, err
	}
	kind, err := normalizeScopeKind(input.ScopeKind)
	if err != nil {
		return Grant{}, err
	}
	scope := strings.TrimSpace(input.Scope)
	scope, kind = reconcileScope(scope, kind)
	return Grant{
		ToolName:   toolName,
		Scope:      scope,
		ScopeKind:  kind,
		Decision:   decision,
		ApprovedAt: now().UTC().Format(time.RFC3339),
		Reason:     redaction.RedactString(strings.TrimSpace(input.Reason), redaction.Options{}),
	}, nil
}

func createCommandPrefixGrant(input CommandPrefixInput, now func() time.Time) (CommandPrefixGrant, error) {
	toolName := strings.TrimSpace(input.ToolName)
	if err := ValidateToolName(toolName); err != nil {
		return CommandPrefixGrant{}, err
	}
	prefix, ok := NormalizeCommandPrefix(input.Prefix)
	if !ok {
		return CommandPrefixGrant{}, fmt.Errorf("invalid command prefix")
	}
	return CommandPrefixGrant{
		ToolName:   toolName,
		Prefix:     prefix,
		ApprovedAt: now().UTC().Format(time.RFC3339),
		Reason:     redaction.RedactString(strings.TrimSpace(input.Reason), redaction.Options{}),
	}, nil
}

// normalizeScopeKind validates and lower-cases a scope kind. An empty kind is the
// tool-wide grant.
func normalizeScopeKind(kind ScopeKind) (ScopeKind, error) {
	switch ScopeKind(strings.ToLower(strings.TrimSpace(string(kind)))) {
	case ScopeToolWide:
		return ScopeToolWide, nil
	case ScopeFile:
		return ScopeFile, nil
	case ScopeDir:
		return ScopeDir, nil
	case ScopeHost:
		return ScopeHost, nil
	default:
		return "", fmt.Errorf("invalid sandbox scope kind %q. Expected file, dir, host, or empty", kind)
	}
}

// reconcileScope keeps scope and kind consistent: a tool-wide kind carries no
// path, and a file/dir kind with no path degrades to tool-wide (a scoped grant
// with nothing to scope is meaningless).
func reconcileScope(scope string, kind ScopeKind) (string, ScopeKind) {
	if kind == ScopeToolWide || scope == "" {
		return "", ScopeToolWide
	}
	if kind == ScopeHost {
		host := normalizeHostScope(scope)
		if host == "" {
			return "", ScopeToolWide
		}
		return host, ScopeHost
	}
	return scope, kind
}

// readState reads the grant file for a caller that does NOT hold the
// interprocess lock (the lookup/list paths). A schema or policy migration has to
// write — a backup plus the migrated state — so it is performed under the lock,
// and the read is retaken there because another process may have migrated the
// file while this one waited.
func (store *GrantStore) readState() (grantFile, error) {
	state, needsMigration, err := store.readStateFile(false)
	if err != nil || !needsMigration {
		return state, err
	}
	unlock, err := store.lockStateFile()
	if err != nil {
		return grantFile{}, err
	}
	defer unlock()
	return store.readStateLocked()
}

// readStateLocked reads the grant file for a caller that already holds the
// interprocess lock, so a migration writes through directly instead of
// re-acquiring it (which would deadlock against the caller's own lock).
func (store *GrantStore) readStateLocked() (grantFile, error) {
	state, _, err := store.readStateFile(true)
	return state, err
}

// readStateFile decodes the grant file. needsMigration reports that the state on
// disk must be rewritten (legacy schema or a changed policy version) and that the
// caller has to retake this read while holding the interprocess lock; the
// returned state is meaningless in that case.
func (store *GrantStore) readStateFile(locked bool) (grantFile, bool, error) {
	state, err := store.decodeState(locked)
	if err != nil {
		if errors.Is(err, errGrantMigrationNeedsLock) {
			return grantFile{}, true, nil
		}
		return grantFile{}, false, err
	}
	return state, false, nil
}

// errGrantMigrationNeedsLock is the internal signal from decodeState that the
// file needs a migration write, which only a lock holder may perform.
var errGrantMigrationNeedsLock = errors.New("sandbox grants file needs a migration write")

func (store *GrantStore) decodeState(locked bool) (grantFile, error) {
	data, err := os.ReadFile(store.filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return emptyGrantState(), nil
		}
		return grantFile{}, err
	}
	// Peek at the schema version so a legacy (v1) file — whose grants are keyed
	// directly to a single Grant — can be decoded and migrated separately from the
	// current (v2) map[tool][]Grant shape.
	var head struct {
		SchemaVersion   int             `json:"schemaVersion"`
		PolicyVersion   int             `json:"policyVersion"`
		Grants          json.RawMessage `json:"grants"`
		CommandPrefixes json.RawMessage `json:"commandPrefixes"`
		Migration       *grantMigration `json:"migration"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return grantFile{}, store.invalidGrantFile(err)
	}
	if head.SchemaVersion == 1 || head.SchemaVersion == 2 {
		if !locked {
			return grantFile{}, errGrantMigrationNeedsLock
		}
		return store.migrateLegacyState(data, head.SchemaVersion, head.Grants, head.CommandPrefixes)
	}
	if head.SchemaVersion != grantSchemaVersion {
		return grantFile{}, fmt.Errorf("invalid sandbox grants file at %s: unsupported schemaVersion", store.filePath)
	}
	if head.PolicyVersion != execution.PolicyVersion {
		if !locked {
			return grantFile{}, errGrantMigrationNeedsLock
		}
		return store.migrateChangedPolicy(data, head.PolicyVersion, head.Grants, head.CommandPrefixes)
	}
	buckets := map[string][]Grant{}
	commandPrefixBuckets := map[string][]CommandPrefixGrant{}
	switch head.SchemaVersion {
	case grantSchemaVersion:
		if len(head.Grants) > 0 {
			if err := json.Unmarshal(head.Grants, &buckets); err != nil {
				return grantFile{}, store.invalidGrantFile(err)
			}
		}
		if len(head.CommandPrefixes) > 0 {
			if err := json.Unmarshal(head.CommandPrefixes, &commandPrefixBuckets); err != nil {
				return grantFile{}, store.invalidGrantFile(err)
			}
		}
	default:
		return grantFile{}, fmt.Errorf("invalid sandbox grants file at %s: unsupported schemaVersion", store.filePath)
	}
	// Validate and normalize every grant, canonicalizing tool keys (trimmed) so a
	// whitespace-padded key in the file still matches at lookup time.
	normalized := map[string][]Grant{}
	for name, bucket := range buckets {
		key := strings.TrimSpace(name)
		if err := ValidateToolName(key); err != nil {
			return grantFile{}, store.invalidGrantFile(err)
		}
		for _, grant := range bucket {
			ng, err := normalizeStoredGrant(key, grant)
			if err != nil {
				return grantFile{}, store.invalidGrantFile(err)
			}
			normalized[key] = append(normalized[key], ng)
		}
	}
	normalizedPrefixes := map[string][]CommandPrefixGrant{}
	for name, bucket := range commandPrefixBuckets {
		key := strings.TrimSpace(name)
		if err := ValidateToolName(key); err != nil {
			return grantFile{}, store.invalidGrantFile(err)
		}
		for _, grant := range bucket {
			ng, err := normalizeStoredCommandPrefixGrant(key, grant)
			if err != nil {
				return grantFile{}, store.invalidGrantFile(err)
			}
			normalizedPrefixes[key] = append(normalizedPrefixes[key], ng)
		}
	}
	return grantFile{SchemaVersion: grantSchemaVersion, PolicyVersion: execution.PolicyVersion, Grants: normalized, CommandPrefixes: normalizedPrefixes, Migration: head.Migration}, nil
}

func (store *GrantStore) migrateLegacyState(data []byte, schemaVersion int, grantsJSON json.RawMessage, prefixesJSON json.RawMessage) (grantFile, error) {
	buckets := map[string][]Grant{}
	if schemaVersion == 1 {
		legacy := map[string]Grant{}
		if len(grantsJSON) > 0 {
			if err := json.Unmarshal(grantsJSON, &legacy); err != nil {
				return grantFile{}, store.invalidGrantFile(err)
			}
		}
		for name, grant := range legacy {
			buckets[name] = []Grant{grant}
		}
	} else if len(grantsJSON) > 0 {
		if err := json.Unmarshal(grantsJSON, &buckets); err != nil {
			return grantFile{}, store.invalidGrantFile(err)
		}
	}
	prefixes := map[string][]CommandPrefixGrant{}
	if len(prefixesJSON) > 0 {
		if err := json.Unmarshal(prefixesJSON, &prefixes); err != nil {
			return grantFile{}, store.invalidGrantFile(err)
		}
	}

	backupPath, err := store.writeLegacyBackup(data, schemaVersion)
	if err != nil {
		return grantFile{}, err
	}
	state := emptyGrantState()
	migrated, invalidated := 0, 0
	for name, bucket := range buckets {
		key := strings.TrimSpace(name)
		if err := ValidateToolName(key); err != nil {
			invalidated += len(bucket)
			continue
		}
		for _, grant := range bucket {
			normalized, err := normalizeStoredGrant(key, grant)
			if err != nil || legacyShellGrant(normalized) {
				invalidated++
				continue
			}
			state.Grants[key] = append(state.Grants[key], normalized)
			migrated++
		}
	}
	for name, bucket := range prefixes {
		key := strings.TrimSpace(name)
		if err := ValidateToolName(key); err != nil {
			invalidated += len(bucket)
			continue
		}
		for _, grant := range bucket {
			normalized, err := normalizeStoredCommandPrefixGrant(key, grant)
			if err != nil {
				invalidated++
				continue
			}
			state.CommandPrefixes[key] = append(state.CommandPrefixes[key], normalized)
			migrated++
		}
	}
	state.Migration = &grantMigration{FromSchemaVersion: schemaVersion, BackupPath: backupPath, Migrated: migrated, Invalidated: invalidated, NoticePending: true}
	if err := store.writeState(state); err != nil {
		return grantFile{}, err
	}
	return state, nil
}

func (store *GrantStore) migrateChangedPolicy(data []byte, policyVersion int, grantsJSON json.RawMessage, prefixesJSON json.RawMessage) (grantFile, error) {
	buckets := map[string][]Grant{}
	prefixes := map[string][]CommandPrefixGrant{}
	if len(grantsJSON) > 0 {
		if err := json.Unmarshal(grantsJSON, &buckets); err != nil {
			return grantFile{}, store.invalidGrantFile(err)
		}
	}
	if len(prefixesJSON) > 0 {
		if err := json.Unmarshal(prefixesJSON, &prefixes); err != nil {
			return grantFile{}, store.invalidGrantFile(err)
		}
	}
	backupPath, err := store.writeBackup(data, fmt.Sprintf("policy-v%d", policyVersion))
	if err != nil {
		return grantFile{}, err
	}
	state := emptyGrantState()
	migrated, invalidated := 0, 0
	for name, bucket := range buckets {
		key := strings.TrimSpace(name)
		if err := ValidateToolName(key); err != nil {
			invalidated += len(bucket)
			continue
		}
		for _, grant := range bucket {
			normalized, err := normalizeStoredGrant(key, grant)
			if err != nil || normalized.Decision != GrantDeny {
				invalidated++
				continue
			}
			state.Grants[key] = append(state.Grants[key], normalized)
			migrated++
		}
	}
	for _, bucket := range prefixes {
		invalidated += len(bucket)
	}
	state.Migration = &grantMigration{FromSchemaVersion: grantSchemaVersion, BackupPath: backupPath, Migrated: migrated, Invalidated: invalidated, NoticePending: true}
	if err := store.writeState(state); err != nil {
		return grantFile{}, err
	}
	return state, nil
}

func legacyShellGrant(grant Grant) bool {
	if grant.Decision == GrantDeny {
		return false
	}
	switch grant.ToolName {
	case "bash", "exec_command", "write_stdin":
		return true
	default:
		return false
	}
}

func (store *GrantStore) writeLegacyBackup(data []byte, schemaVersion int) (string, error) {
	return store.writeBackup(data, fmt.Sprintf("v%d", schemaVersion))
}

func (store *GrantStore) writeBackup(data []byte, label string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(store.filePath), 0o700); err != nil {
		return "", err
	}
	base := fmt.Sprintf("%s.%s.backup", store.filePath, label)
	for attempt := 0; ; attempt++ {
		path := base
		if attempt > 0 {
			path = fmt.Sprintf("%s.%d", base, attempt)
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if _, err := file.Write(data); err != nil {
			_ = file.Close()
			_ = os.Remove(path)
			return "", err
		}
		if err := file.Close(); err != nil {
			_ = os.Remove(path)
			return "", err
		}
		return path, nil
	}
}

func (store *GrantStore) invalidGrantFile(err error) error {
	return fmt.Errorf("invalid sandbox grants file at %s: %w", store.filePath, err)
}

func (store *GrantStore) writeState(state grantFile) error {
	if err := os.MkdirAll(filepath.Dir(store.filePath), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tempPath := fmt.Sprintf("%s.tmp-%d-%d", store.filePath, os.Getpid(), store.now().UnixNano())
	if err := os.WriteFile(tempPath, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tempPath, store.filePath); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	return nil
}

func (store *GrantStore) lockStateFile() (func(), error) {
	lockPath := store.filePath + ".lockfile"
	if err := os.MkdirAll(filepath.Dir(store.filePath), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(grantLockTimeout)
	for {
		locked, err := tryLockGrantFile(file)
		if err != nil {
			_ = file.Close()
			return nil, err
		}
		if locked {
			return func() {
				_ = unlockGrantFile(file)
				_ = file.Close()
			}, nil
		}
		if time.Now().After(deadline) {
			_ = file.Close()
			return nil, fmt.Errorf("timed out waiting for sandbox grants lock at %s", lockPath)
		}
		time.Sleep(grantLockRetry)
	}
}

func normalizeStoredGrant(name string, grant Grant) (Grant, error) {
	if strings.TrimSpace(grant.ToolName) == "" {
		grant.ToolName = name
	}
	if strings.TrimSpace(grant.ToolName) != name {
		return Grant{}, fmt.Errorf("grant key %q does not match toolName %q", name, grant.ToolName)
	}
	grant.ToolName = name
	if strings.TrimSpace(grant.ApprovedAt) == "" {
		return Grant{}, fmt.Errorf("grant %s approvedAt is required", name)
	}
	decision, err := NormalizeGrantDecision(grant.Decision)
	if err != nil {
		return Grant{}, err
	}
	kind, err := normalizeScopeKind(grant.ScopeKind)
	if err != nil {
		return Grant{}, err
	}
	grant.Decision = decision
	grant.Scope, grant.ScopeKind = reconcileScope(strings.TrimSpace(grant.Scope), kind)
	grant.ApprovedAt = strings.TrimSpace(grant.ApprovedAt)
	grant.Reason = redaction.RedactString(strings.TrimSpace(grant.Reason), redaction.Options{})
	return grant, nil
}

func normalizeStoredCommandPrefixGrant(name string, grant CommandPrefixGrant) (CommandPrefixGrant, error) {
	if strings.TrimSpace(grant.ToolName) == "" {
		grant.ToolName = name
	}
	if strings.TrimSpace(grant.ToolName) != name {
		return CommandPrefixGrant{}, fmt.Errorf("command prefix key %q does not match toolName %q", name, grant.ToolName)
	}
	if strings.TrimSpace(grant.ApprovedAt) == "" {
		return CommandPrefixGrant{}, fmt.Errorf("command prefix grant %s approvedAt is required", name)
	}
	prefix, ok := NormalizeCommandPrefix(grant.Prefix)
	if !ok {
		return CommandPrefixGrant{}, fmt.Errorf("invalid command prefix for %s", name)
	}
	grant.ToolName = name
	grant.Prefix = prefix
	grant.ApprovedAt = strings.TrimSpace(grant.ApprovedAt)
	grant.Reason = redaction.RedactString(strings.TrimSpace(grant.Reason), redaction.Options{})
	return grant, nil
}

func emptyGrantState() grantFile {
	return grantFile{
		SchemaVersion:   grantSchemaVersion,
		PolicyVersion:   execution.PolicyVersion,
		Grants:          map[string][]Grant{},
		CommandPrefixes: map[string][]CommandPrefixGrant{},
	}
}

func envValue(env map[string]string, key string) string {
	if env != nil {
		return env[key]
	}
	return os.Getenv(key)
}
