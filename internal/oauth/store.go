package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Gitlawb/zero/internal/keyring"
)

const (
	storeSchemaVersion = 1
	// KeyPrefixProvider namespaces provider-login tokens; MCP server tokens live
	// under KeyPrefixMCP in the same format (so a future MCP migration is a key
	// rename, not a format change).
	KeyPrefixProvider = "provider:"
	KeyPrefixMCP      = "mcp:"
)

// keyPattern bounds a token key to a safe, single-segment namespaced identifier
// so a key can never traverse or collide with store internals.
var keyPattern = regexp.MustCompile(`^(provider|mcp):[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidateKey reports whether key is a well-formed namespaced token key.
func ValidateKey(key string) error {
	if !keyPattern.MatchString(key) {
		return fmt.Errorf("oauth: invalid token key %q (want \"provider:<name>\" or \"mcp:<name>\")", key)
	}
	return nil
}

// ProviderKey builds the store key for a provider login, normalizing the name
// to lower case. Every write (Manager.Login, the ChatGPT flow) and every
// lookup (FirstStored, GetFresh, logout, status filters) funnels through here,
// so normalizing at this one choke point keeps them symmetric: without it,
// `zero auth login xAI` stored "provider:xAI" while the profile scaffolded for
// it looked up "provider:xai" case-sensitively — a fresh, successful login
// that was invisible to the runtime.
func ProviderKey(name string) string {
	return KeyPrefixProvider + strings.ToLower(strings.TrimSpace(name))
}

// FirstStored returns the token and its ProviderKey for the FIRST candidate name
// that has a token in the store, with ok=false when none do. Callers pass
// ProviderProfile.OAuthLoginCandidates() so that everything derived from a login
// — the bearer token AND any header claim like chatgpt-account-id — comes from
// the SAME login; selecting independently per consumer could otherwise pair a
// bearer from one login with an account header from another. A load error on a
// candidate is treated as a miss (skip to the next), never a hard failure.
func FirstStored(store *Store, candidates []string) (Token, string, bool) {
	if store == nil {
		return Token{}, "", false
	}
	for _, name := range candidates {
		key := ProviderKey(name)
		if token, ok, err := store.Load(key); err == nil && ok {
			return token, key, true
		}
	}
	return Token{}, "", false
}

// Status is a redaction-safe summary of a stored token (no secret material).
type Status struct {
	Key             string    `json:"key"`
	HasToken        bool      `json:"hasToken"`
	HasRefreshToken bool      `json:"hasRefreshToken"`
	TokenType       string    `json:"tokenType,omitempty"`
	Account         string    `json:"account,omitempty"`
	Scopes          []string  `json:"scopes,omitempty"`
	ExpiresAt       time.Time `json:"expiresAt,omitempty"`
	Expired         bool      `json:"expired"`
}

// StoreOptions configures where provider OAuth tokens are persisted.
type StoreOptions struct {
	FilePath string
	Env      map[string]string
	Now      func() time.Time
	// Storage selects the backend: "" / "file" => a 0600 JSON file (default);
	// "encrypted-file" => an AES-256-GCM encrypted file; "keyring" => the OS
	// keyring. When empty it falls back to ZERO_OAUTH_STORAGE.
	Storage string
	// Encrypted is a legacy alias for Storage=="encrypted-file" (AES-256-GCM at
	// rest). Ignored when Storage is set.
	Encrypted bool
	// Keyring is the client used when Storage=="keyring"; nil => keyring.New().
	// Injected by tests to avoid touching a real keychain.
	Keyring KeyringClient
	// KeyringLockPath overrides the cross-process lock file used by the keyring
	// backend. When empty, it is derived from the keyring storage identity
	// via ResolveKeyringLockPath(Env).
	KeyringLockPath string
}

// KeyringClient is the minimal OS-keyring surface the store needs. *keyring.Keyring
// satisfies it; tests inject a fake.
type KeyringClient interface {
	Get(service, account string) (string, bool, error)
	Set(service, account, secret string) error
	Delete(service, account string) (bool, error)
	// MaxSecretLen reports the largest secret, in bytes, Set accepts under
	// (service, account); ok is false when the backend has no practical limit.
	// keyringBlob needs it because macOS caps a keychain write well below the
	// size of a multi-provider token blob (see the keyringBlob doc).
	MaxSecretLen(service, account string) (int, bool)
}

// Keyring storage anchors the token blob at one fixed entry, which holds either
// the whole blob or, once that no longer fits, the manifest naming the chunks
// that do (see keyringBlob).
const (
	keyringService = "zero"
	keyringAccount = "oauth-tokens"
)

// Chunked keyring layout.
//
// keyringManifestPrefix marks the anchor entry as a manifest rather than a
// whole blob. ":" is outside the base64 alphabet, so a stored blob can never
// begin with this prefix and the two layouts are distinguishable without a
// schema migration.
//
// keyringChunkFamilyA and B name the two generations a write alternates
// between; keyringMaxChunks bounds both the accounts a read will probe and the
// blob size the backend will accept.
const (
	keyringManifestPrefix = "zc1:"
	keyringChunkFamilyA   = "a"
	keyringChunkFamilyB   = "b"
	keyringMaxChunks      = 64
	// keyringMinChunkLen is the smallest per-entry budget worth splitting into.
	// Below it the service and account names have eaten the command line, so no
	// chunk count would fit the blob and the store says so instead of writing
	// hundreds of tiny entries.
	keyringMinChunkLen = 256
)

// Store persists OAuth tokens (provider + MCP namespaces) as one JSON blob,
// written atomically through a pluggable backend (a 0600 file guarded by a
// cross-process lock, or the OS keyring). When crypter is non-nil the file blob
// is AES-256-GCM ciphertext at rest.
type Store struct {
	blob    blobStore
	crypter *aesGCMCrypter // nil => plaintext blob
	now     func() time.Time
	mu      sync.Mutex
}

type storeFile struct {
	SchemaVersion int              `json:"schemaVersion"`
	Tokens        map[string]Token `json:"tokens"`
}

// ResolveStorePath determines the on-disk location for provider OAuth tokens,
// honoring ZERO_OAUTH_TOKENS_PATH, then XDG_CONFIG_HOME, then the home dir.
func ResolveStorePath(env map[string]string) (string, error) {
	if override := strings.TrimSpace(envValue(env, "ZERO_OAUTH_TOKENS_PATH")); override != "" {
		if filepath.IsAbs(override) {
			return filepath.Clean(override), nil
		}
		return filepath.Abs(override)
	}
	configHome := strings.TrimSpace(envValue(env, "XDG_CONFIG_HOME"))
	if configHome == "" {
		home := strings.TrimSpace(firstNonEmpty(envValue(env, "HOME"), envValue(env, "USERPROFILE")))
		if home == "" {
			var err error
			home, err = os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("oauth: resolve user home: %w", err)
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
	return filepath.Join(configHome, "zero", "oauth-tokens.json"), nil
}

// ResolveKeyringLockPath determines the on-disk cross-process lock file for the
// keyring backend. Unlike file-backed storage, keyring entries are anchored to
// the user's OS keychain (zero/oauth-tokens) and are shared across all processes
// for that OS user, regardless of file-store overrides such as
// ZERO_OAUTH_TOKENS_PATH, FilePath, or XDG_CONFIG_HOME.
func ResolveKeyringLockPath(env map[string]string) (string, error) {
	if override := strings.TrimSpace(envValue(env, "ZERO_OAUTH_KEYRING_LOCK_PATH")); override != "" {
		if filepath.IsAbs(override) {
			return filepath.Clean(override), nil
		}
		return filepath.Abs(override)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("oauth: resolve user home for keyring lock: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	return filepath.Join(home, ".zero", "oauth-keyring.lockfile"), nil
}

// NewStore builds a token store with the configured backend (file by default,
// or the OS keyring when Storage/ZERO_OAUTH_STORAGE selects it).
func NewStore(options StoreOptions) (*Store, error) {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	storage := strings.TrimSpace(options.Storage)
	if storage == "" {
		storage = strings.TrimSpace(envValue(options.Env, "ZERO_OAUTH_STORAGE"))
	}
	if storage == "" && options.Encrypted {
		storage = "encrypted-file" // legacy alias
	}
	switch storage {
	case "", "file":
		path, err := resolveStoreFilePath(options)
		if err != nil {
			return nil, err
		}
		return &Store{blob: fileBlob{path: path}, now: now}, nil
	case "encrypted-file":
		path, err := resolveStoreFilePath(options)
		if err != nil {
			return nil, err
		}
		// The file blob holds AES-256-GCM ciphertext; the per-user secret lives in
		// a sibling ".secret" file (see encrypt.go).
		return &Store{blob: fileBlob{path: path}, crypter: newAESGCMCrypter(path + ".secret"), now: now}, nil
	case "keyring":
		kr := options.Keyring
		if kr == nil {
			osKeyring := keyring.New()
			if !osKeyring.Available() {
				return nil, fmt.Errorf("oauth: keyring storage requested but not available on %s; use file storage", runtime.GOOS)
			}
			kr = osKeyring
		}
		// Serialize the keyring's read-modify-write across processes with a lock
		// file derived from the keyring storage identity ("zero"/"oauth-tokens").
		// Unlike the file backend, the keyring does not vary with file configuration
		// (ZERO_OAUTH_TOKENS_PATH, FilePath, or XDG_CONFIG_HOME). Best-effort: if no
		// home location resolves, fall back to in-process serialization only.
		lockPath := strings.TrimSpace(options.KeyringLockPath)
		if lockPath == "" {
			lp, perr := ResolveKeyringLockPath(options.Env)
			if perr != nil {
				return nil, perr
			}
			lockPath = lp
		}
		return &Store{blob: keyringBlob{kr: kr, service: keyringService, account: keyringAccount, lockPath: lockPath}, now: now}, nil
	default:
		return nil, fmt.Errorf("oauth: unknown storage %q (want \"file\", \"encrypted-file\", or \"keyring\")", storage)
	}
}

// resolveStoreFilePath resolves the absolute file path for the file backend.
func resolveStoreFilePath(options StoreOptions) (string, error) {
	filePath := options.FilePath
	var err error
	if strings.TrimSpace(filePath) == "" {
		filePath, err = ResolveStorePath(options.Env)
		if err != nil {
			return "", err
		}
	}
	if !filepath.IsAbs(filePath) {
		filePath, err = filepath.Abs(filePath)
		if err != nil {
			return "", err
		}
	}
	return filepath.Clean(filePath), nil
}

// FilePath returns the resolved token store location (a path for the file
// backend, or a "keyring:..." identifier for the keyring backend).
func (s *Store) FilePath() string { return s.blob.location() }

// Save persists a token under key, replacing any existing entry.
func (s *Store) Save(key string, token Token) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blob.withLock(s.now, func() error {
		state, err := s.readState()
		if err != nil {
			return err
		}
		state.Tokens[key] = token
		return s.writeState(state)
	})
}

// Load returns the token for key; the bool is false when none is stored.
func (s *Store) Load(key string) (Token, bool, error) {
	if err := ValidateKey(key); err != nil {
		return Token{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readState()
	if err != nil {
		return Token{}, false, err
	}
	token, ok := state.Tokens[key]
	return token, ok, nil
}

// Delete removes the token for key, reporting whether one was present.
func (s *Store) Delete(key string) (bool, error) {
	if err := ValidateKey(key); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var removed bool
	err := s.blob.withLock(s.now, func() error {
		state, err := s.readState()
		if err != nil {
			return err
		}
		if _, ok := state.Tokens[key]; !ok {
			return nil
		}
		delete(state.Tokens, key)
		removed = true
		return s.writeState(state)
	})
	return removed, err
}

// Reset clears all persistent state in the store (removing files or deleting all keyring entries).
func (s *Store) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blob.withLock(s.now, func() error {
		return s.blob.reset()
	})
}

// Status returns redaction-safe summaries of every stored token, sorted by key.
// An optional prefix filters to one namespace (e.g. KeyPrefixProvider).
func (s *Store) Status(prefix string) ([]Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readState()
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(state.Tokens))
	for k := range state.Tokens {
		if prefix == "" || strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	now := s.now()
	out := make([]Status, 0, len(keys))
	for _, k := range keys {
		token := state.Tokens[k]
		out = append(out, Status{
			Key:             k,
			HasToken:        strings.TrimSpace(token.AccessToken) != "",
			HasRefreshToken: strings.TrimSpace(token.RefreshToken) != "",
			TokenType:       token.TokenType,
			Account:         token.Account,
			Scopes:          token.Scopes,
			ExpiresAt:       token.ExpiresAt,
			Expired:         token.Expired(now),
		})
	}
	return out, nil
}

func (s *Store) readState() (storeFile, error) {
	data, ok, err := s.blob.read()
	if err != nil {
		return storeFile{}, err
	}
	if !ok {
		return emptyStoreFile(), nil
	}
	if s.crypter != nil {
		// Encrypted backend: the blob is AES-256-GCM ciphertext, not JSON.
		data, err = s.crypter.open(data)
		if err != nil {
			return storeFile{}, fmt.Errorf("oauth: decrypt token store at %s: %w", s.blob.location(), err)
		}
	}
	var state storeFile
	if err := json.Unmarshal(data, &state); err != nil {
		return storeFile{}, fmt.Errorf("oauth: invalid token store at %s: %w", s.blob.location(), err)
	}
	if state.SchemaVersion != storeSchemaVersion {
		return storeFile{}, fmt.Errorf("oauth: invalid token store at %s: unsupported schemaVersion", s.blob.location())
	}
	if state.Tokens == nil {
		state.Tokens = map[string]Token{}
	}
	for key := range state.Tokens {
		if err := ValidateKey(key); err != nil {
			return storeFile{}, fmt.Errorf("oauth: invalid token store at %s: %w", s.blob.location(), err)
		}
	}
	return state, nil
}

func (s *Store) writeState(state storeFile) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	// Plaintext keeps the trailing newline for a tidy file; the encrypted backend
	// writes opaque ciphertext instead.
	payload := append(data, '\n')
	if s.crypter != nil {
		payload, err = s.crypter.seal(data)
		if err != nil {
			return err
		}
	}
	return s.blob.write(payload)
}

func emptyStoreFile() storeFile {
	return storeFile{SchemaVersion: storeSchemaVersion, Tokens: map[string]Token{}}
}

// blobStore abstracts the persistence of the whole token blob behind the Store,
// so the same store logic backs either a 0600 file or the OS keyring.
type blobStore interface {
	// read returns the stored blob; ok is false when nothing is stored yet.
	read() (data []byte, ok bool, err error)
	// write replaces the stored blob.
	write(data []byte) error
	// reset removes all stored state and entries.
	reset() error
	// withLock runs fn under whatever cross-process exclusion the backend offers
	// (a lock file for the file backend; none for the keyring, which is the
	// authoritative store and is serialized within the process by Store.mu).
	withLock(now func() time.Time, fn func() error) error
	// location is a human-readable identifier for diagnostics/errors.
	location() string
}

// fileBlob persists the blob as a 0600 JSON file, written atomically and guarded
// by a cross-process lock file. Behavior matches the original file store.
type fileBlob struct{ path string }

func (b fileBlob) reset() error {
	var errs []error
	if err := os.Remove(b.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	if err := os.Remove(b.path + ".secret"); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	for _, dir := range []string{b.path + ".publish", b.path + ".secret.publish"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
			continue
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "publish-") {
				if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
					errs = append(errs, err)
				}
			}
		}
	}
	if err := os.Remove(b.path + ".secret.lock"); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (b fileBlob) read() ([]byte, bool, error) {
	data, err := os.ReadFile(b.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return data, true, nil
}

func (b fileBlob) write(data []byte) error {
	if err := os.MkdirAll(filepath.Dir(b.path), 0o700); err != nil {
		return err
	}
	temp, tempPath, err := createPublicationFile(b.path)
	if err != nil {
		return err
	}
	defer os.Remove(tempPath)
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, b.path)
}

// PublicationDirSuffix names the per-store directory a token store publishes new
// contents through. Sandbox profiles deny it by name (see
// internal/sandbox.credentialPublicationDir), which is why the directory name is
// derived from the store path while the file inside it is randomly named: the
// deterministic part is what a deny rule can reference, and the random part is
// what stops a same-user process from waiting for the plaintext to appear at a
// path it can open or rename away.
const PublicationDirSuffix = ".publish"

// PublicationDir returns the publication directory for a store path.
func PublicationDir(path string) string { return path + PublicationDirSuffix }

// createPublicationFile creates the randomly-named 0600 file that path's next
// contents are written to before being renamed into place. It lives in
// PublicationDir(path) — same filesystem as path, so the rename stays atomic —
// and the directory is created 0700 if it does not exist yet.
func createPublicationFile(path string) (*os.File, string, error) {
	dir := PublicationDir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	temp, err := os.CreateTemp(dir, "publish-*")
	if err != nil {
		return nil, "", err
	}
	if err := temp.Chmod(0o600); err != nil {
		name := temp.Name()
		_ = temp.Close()
		_ = os.Remove(name)
		return nil, "", err
	}
	return temp, temp.Name(), nil
}

func (b fileBlob) withLock(now func() time.Time, fn func() error) error {
	unlock, err := acquireFileLock(b.path+".lockfile", now)
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

func (b fileBlob) location() string { return b.path }

// keyringBlob persists the blob in the OS keyring, base64-encoded (base64 keeps
// the multi-line JSON a single, control-character-free value).
//
// macOS caps a keychain write at securityMaxLine bytes, because the secret
// rides inside a `security -i` command line, and the blob holds every provider
// and MCP login at once. That budget is 4039 bytes of base64 under the anchor
// account, which is 3027 bytes of JSON: one OIDC login carrying an access
// token, an ID token and a refresh token can fill it on its own, and two
// reliably do. Storing the blob as one entry therefore turned a second login
// into a hard "secret too large" failure with nothing saved.
//
// So a blob that does not fit is split across numbered entries and the anchor
// account holds a manifest instead. Chunks live under two alternating
// generations ("families"): a write fills the family that is NOT live, then
// replaces the manifest. That single Set is the commit point, so until it lands
// read() still returns the previous generation intact and a crash mid-write
// loses nothing. Cleanup of the retired generation runs after the commit and
// correctness never depends on it: the manifest states how many chunks each
// family holds, and it is only ever written with a count it has already made
// true, so a failed cleanup over-states and the next write deletes the excess.
//
// Backends with no size limit (Linux secret-tool reads the secret from stdin)
// never reach the chunked layout, so their stored form is unchanged.
type keyringBlob struct {
	kr      KeyringClient
	service string
	account string
	// lockPath, when set, is a cross-process lock file serializing the keyring's
	// read-modify-write so concurrent processes don't clobber each other's tokens.
	lockPath string
}

// keyringManifest describes which chunk generation is live and how many entries
// each generation currently holds.
type keyringManifest struct {
	live   string
	counts map[string]int
	digest string
}

func (b keyringBlob) corruptError(detail string) error {
	return fmt.Errorf("oauth: keyring token data at %s (account %q) %s; run `zero auth reset --confirm` (clears all OAuth logins and shared MCP tokens) or remove entries %q, %q.a.0..%d, and %q.b.0..%d to recover",
		b.location(), b.account, detail, b.account, b.account, keyringMaxChunks-1, b.account, keyringMaxChunks-1)
}

func (b keyringBlob) read() ([]byte, bool, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(10 * time.Millisecond)
		}
		head, ok, err := b.kr.Get(b.service, b.account)
		if err != nil || !ok {
			return nil, ok, err
		}
		head = strings.TrimSpace(head)
		if !strings.HasPrefix(head, keyringManifestPrefix) {
			data, err := decodeKeyringBlob(head)
			if err != nil {
				lastErr = b.corruptError("contains invalid base64 data: " + err.Error())
				continue
			}
			return data, true, nil
		}
		manifest, err := parseKeyringManifest(head)
		if err != nil {
			lastErr = b.corruptError("has a malformed manifest (" + err.Error() + ")")
			continue
		}
		count := manifest.counts[manifest.live]
		var encoded strings.Builder
		var chunkErr error
		for index := range count {
			part, ok, err := b.kr.Get(b.service, b.chunkAccount(manifest.live, index))
			if err != nil {
				chunkErr = err
				break
			}
			if !ok {
				chunkErr = b.corruptError(fmt.Sprintf("is missing chunk %d of %d", index+1, count))
				break
			}
			encoded.WriteString(strings.TrimSpace(part))
		}
		if chunkErr != nil {
			lastErr = chunkErr
			continue
		}
		data, err := decodeKeyringBlob(encoded.String())
		if err != nil {
			lastErr = b.corruptError("contains invalid chunk data: " + err.Error())
			continue
		}
		// The corruption this guards against is the one that motivated chunking:
		// `security -i` splits an overlong line into two garbage commands rather
		// than refusing it, so a stored chunk can come back truncated.
		if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != manifest.digest {
			lastErr = b.corruptError("failed its integrity check")
			continue
		}
		return data, true, nil
	}
	return nil, false, lastErr
}

func (b keyringBlob) write(data []byte) error {
	encoded := base64.StdEncoding.EncodeToString(data)
	// Read the live manifest before overwriting anything: it names the
	// generation this write must avoid, and the counts cleanup needs afterwards.
	previous, err := b.readManifest()
	if err != nil {
		return err
	}
	if budget, bounded := b.kr.MaxSecretLen(b.service, b.account); !bounded || len(encoded) <= budget {
		return b.writeWhole(encoded, previous)
	}
	return b.writeChunked(data, encoded, previous)
}

// writeWhole stores the blob under the anchor account, the layout every backend
// without a size limit uses and the one a shrinking store returns to. The Set
// is the commit point; the chunks it retires are deleted after it lands.
//
// A delete that fails here leaves residue the manifest can no longer describe,
// because the anchor now holds the blob rather than the counts. Nothing
// reclaims it until the store next outgrows a single entry, where writeChunked
// sweeps both generations; a store that shrinks once and never grows again
// keeps it. Sweeping on every whole write would close that, but it costs a
// keyringMaxChunks-wide probe per save — 128 `security` invocations on macOS —
// for residue that only an already-failed delete can produce.
func (b keyringBlob) writeWhole(encoded string, previous keyringManifest) error {
	if err := b.kr.Set(b.service, b.account, encoded); err != nil {
		return err
	}
	b.sweepCleanupAccount()
	err := b.deleteChunkRange(keyringChunkFamilyA, 0, previous.counts[keyringChunkFamilyA], nil)
	if err = b.deleteChunkRange(keyringChunkFamilyB, 0, previous.counts[keyringChunkFamilyB], err); err != nil {
		return fmt.Errorf("oauth: tokens were saved, but a superseded keyring entry at %s could not be removed: %w", b.location(), err)
	}
	return nil
}

func (b keyringBlob) writeChunked(data []byte, encoded string, previous keyringManifest) (retErr error) {
	b.sweepCleanupAccount()
	family := keyringChunkFamilyA
	if previous.live == keyringChunkFamilyA {
		family = keyringChunkFamilyB
	}
	// Size every chunk against the LONGEST account name the family can produce.
	// On macOS the account shares the command line with the secret, so a budget
	// derived from chunk 0 would overflow once the index grew a digit; pinning
	// it to the highest index keeps the size independent of the final count.
	budget, bounded := b.kr.MaxSecretLen(b.service, b.chunkAccount(family, keyringMaxChunks-1))
	if !bounded || budget < keyringMinChunkLen {
		return fmt.Errorf("oauth: keyring entries at %s hold at most %d bytes, too small to store the token blob; use file storage", b.location(), budget)
	}
	if len(encoded) > budget*keyringMaxChunks {
		return fmt.Errorf("oauth: token blob is %d encoded bytes, over the %d the keyring at %s can hold; use file storage", len(encoded), budget*keyringMaxChunks, b.location())
	}

	count := (len(encoded) + budget - 1) / budget
	// Make the manifest cover the range this write is about to occupy BEFORE
	// occupying it. Without that, a write that dies partway through leaves
	// chunks above the count the manifest records, and no later cleanup would
	// know to delete them: a fragment of a token blob would sit in the keychain
	// for good. Raising the count of a generation that is not live changes
	// nothing a reader looks at, so the reservation is invisible until the
	// commit below.
	if previous.live == "" {
		// Nothing to reserve against: the anchor still holds the whole blob, so
		// writing a manifest here would destroy the only copy. Sweep both
		// generations instead. This runs once, when a store first outgrows a
		// single entry, and only has anything to find if an earlier shrink was
		// interrupted before its cleanup finished.
		//
		// Both, not just the target: writeWhole replaced the anchor with the
		// blob, so the counts that named the chunks a failed cleanup left
		// behind are gone. The manifest cannot state the range any more and no
		// later write derives it, so this is the only sweep that will ever
		// reach them. Sweeping just the target would leave the other
		// generation's chunks, and the token material in them, unreferenced
		// for good.
		other := keyringChunkFamilyB
		if family == keyringChunkFamilyB {
			other = keyringChunkFamilyA
		}
		err := b.deleteChunkRange(family, count, keyringMaxChunks, nil)
		if err = b.deleteChunkRange(other, 0, keyringMaxChunks, err); err != nil {
			return err
		}
	} else if count > previous.counts[family] {
		reservation := keyringManifest{live: previous.live, counts: maps.Clone(previous.counts), digest: previous.digest}
		reservation.counts[family] = count
		if err := b.kr.Set(b.service, b.account, formatKeyringManifest(reservation)); err != nil {
			return err
		}
		previous.counts[family] = count
	}

	writtenChunks := 0
	defer func() {
		if retErr != nil && previous.live == "" && writtenChunks > 0 {
			// During the first migration from whole-entry to chunked layout,
			// a failure before the manifest commit point must clean up any chunks
			// written so far to avoid leaving orphaned credential material in the keychain.
			if err := b.deleteChunkRange(family, 0, writtenChunks, nil); err != nil {
				_ = b.kr.Set(b.service, b.cleanupAccount(), fmt.Sprintf("%s:%d", family, writtenChunks))
				retErr = errors.Join(retErr, fmt.Errorf("cleanup orphaned migration chunks: %w", err))
			} else {
				_, _ = b.kr.Delete(b.service, b.cleanupAccount())
			}
		}
	}()

	for index := range count {
		offset := index * budget
		if err := b.kr.Set(b.service, b.chunkAccount(family, index), encoded[offset:min(offset+budget, len(encoded))]); err != nil {
			return err
		}
		writtenChunks++
	}
	// This generation is not live yet, so trimming a longer previous one of it
	// now is safe, and it makes the count the manifest is about to publish true
	// before the manifest claims it.
	if err := b.deleteChunkRange(family, count, previous.counts[family], nil); err != nil {
		return err
	}

	next := keyringManifest{live: family, counts: maps.Clone(previous.counts), digest: hexDigest(data)}
	next.counts[family] = count
	if err := b.kr.Set(b.service, b.account, formatKeyringManifest(next)); err != nil {
		return err
	}

	// Committed. The retired generation is now unreferenced, so deleting it is
	// secret hygiene rather than correctness. Its count deliberately stays in
	// the manifest afterwards: over-stating is the safe direction, it saves the
	// next write a reservation, and if a delete here failed the next cleanup
	// retries the same range.
	retired := previous.live
	if retired == "" || next.counts[retired] == 0 {
		return nil
	}
	if err := b.deleteChunkRange(retired, 0, next.counts[retired], nil); err != nil {
		return fmt.Errorf("oauth: tokens were saved, but a superseded keyring entry at %s could not be removed: %w", b.location(), err)
	}
	return nil
}

func (b keyringBlob) reset() error {
	b.sweepCleanupAccount()
	_, bounded := b.kr.MaxSecretLen(b.service, b.account)
	if !bounded {
		var errs []error
		if _, err := b.kr.Delete(b.service, b.account); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", b.account, err))
		}
		_, _ = b.kr.Delete(b.service, b.cleanupAccount())
		return errors.Join(errs...)
	}

	manifest, err := b.readManifest()
	if err == nil && (manifest.live != "" || len(manifest.counts) > 0) {
		var delErr error
		delErr = b.deleteChunkRange(keyringChunkFamilyA, 0, manifest.counts[keyringChunkFamilyA], nil)
		delErr = b.deleteChunkRange(keyringChunkFamilyB, 0, manifest.counts[keyringChunkFamilyB], delErr)
		if _, kerr := b.kr.Delete(b.service, b.account); kerr != nil && delErr == nil {
			delErr = fmt.Errorf("remove %s: %w", b.account, kerr)
		}
		_, _ = b.kr.Delete(b.service, b.cleanupAccount())
		return delErr
	}

	var delErr error
	delErr = b.deleteChunkRange(keyringChunkFamilyA, 0, keyringMaxChunks, nil)
	delErr = b.deleteChunkRange(keyringChunkFamilyB, 0, keyringMaxChunks, delErr)
	if _, kerr := b.kr.Delete(b.service, b.account); kerr != nil && delErr == nil {
		delErr = fmt.Errorf("remove %s: %w", b.account, kerr)
	}
	_, _ = b.kr.Delete(b.service, b.cleanupAccount())
	return delErr
}

func (b keyringBlob) cleanupAccount() string {
	return b.account + ".cleanup"
}

func (b keyringBlob) sweepCleanupAccount() {
	raw, ok, err := b.kr.Get(b.service, b.cleanupAccount())
	if err != nil || !ok {
		return
	}
	raw = strings.TrimSpace(raw)
	if raw != "" {
		parts := strings.Split(raw, ":")
		if len(parts) == 2 {
			family := parts[0]
			if family == keyringChunkFamilyA || family == keyringChunkFamilyB {
				if count, err := strconv.Atoi(parts[1]); err == nil && count > 0 {
					manifest, err := b.readManifest()
					if err != nil {
						return
					}
					// A successful migration can adopt chunks whose cleanup failed.
					// The manifest owns them even if removing the stale marker fails.
					// Any surplus beyond its count is left orphaned rather than
					// risking deletion of a live generation during recovery.
					if manifest.live == family {
						_, _ = b.kr.Delete(b.service, b.cleanupAccount())
						return
					}
					if count > keyringMaxChunks {
						count = keyringMaxChunks
					}
					if err := b.deleteChunkRange(family, 0, count, nil); err != nil {
						return
					}
				}
			}
		}
	}
	_, _ = b.kr.Delete(b.service, b.cleanupAccount())
}

// readManifest returns the live manifest, or a zero manifest when the anchor
// holds a whole blob or nothing at all. A malformed manifest is an error: the
// chunks it names are unreachable without it, and overwriting it would strand
// them in the keychain.
func (b keyringBlob) readManifest() (keyringManifest, error) {
	head, ok, err := b.kr.Get(b.service, b.account)
	if err != nil || !ok {
		return keyringManifest{counts: map[string]int{}}, err
	}
	head = strings.TrimSpace(head)
	if !strings.HasPrefix(head, keyringManifestPrefix) {
		return keyringManifest{counts: map[string]int{}}, nil
	}
	m, err := parseKeyringManifest(head)
	if err != nil {
		return keyringManifest{counts: map[string]int{}}, b.corruptError("has a malformed manifest (" + err.Error() + ")")
	}
	return m, nil
}

// deleteChunkRange removes chunks [from, to) of family, joining onto prior. It
// takes prior so the two-family cleanup in writeWhole reports the first failure
// without either delete being skipped.
func (b keyringBlob) deleteChunkRange(family string, from, to int, prior error) error {
	for index := from; index < to; index++ {
		account := b.chunkAccount(family, index)
		if _, err := b.kr.Delete(b.service, account); err != nil && prior == nil {
			prior = fmt.Errorf("remove %s: %w", account, err)
		}
	}
	return prior
}

func (b keyringBlob) chunkAccount(family string, index int) string {
	return b.account + "." + family + "." + strconv.Itoa(index)
}

// withLock serializes the keyring's read-modify-write. Store.mu covers the
// in-process case; lockPath (when set) adds cross-process exclusion so two
// processes can't both read the blob, modify, and write — dropping a token.
// The chunked layout needs it for a second reason: it makes a write's chunk
// fills and its manifest commit one indivisible sequence to other processes.
//
// Readers (Load, Status) take it as well, which is deliberate and not a
// leftover: the generational layout already hands them a consistent view
// without it, but holding it keeps a read from observing a torn manifest on a
// backend whose Set is not atomic, and it bounds a reader behind at most one
// writer. acquireFileLock reclaims after fileLockStaleAfter, so a crashed
// holder cannot wedge the hot path.
func (b keyringBlob) withLock(now func() time.Time, fn func() error) error {
	if b.lockPath == "" {
		return fn()
	}
	unlock, err := acquireFileLock(b.lockPath, now)
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

func (b keyringBlob) location() string { return "keyring:" + b.service + "/" + b.account }

func decodeKeyringBlob(encoded string) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("oauth: decode keyring token blob: %w", err)
	}
	return data, nil
}

func hexDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// formatKeyringManifest renders "zc1:<live>:<countA>:<countB>:<sha256hex>".
func formatKeyringManifest(m keyringManifest) string {
	return keyringManifestPrefix + m.live +
		":" + strconv.Itoa(m.counts[keyringChunkFamilyA]) +
		":" + strconv.Itoa(m.counts[keyringChunkFamilyB]) +
		":" + m.digest
}

func parseKeyringManifest(head string) (keyringManifest, error) {
	malformed := func(detail string) (keyringManifest, error) {
		return keyringManifest{}, fmt.Errorf("oauth: keyring token manifest %s", detail)
	}
	fields := strings.Split(strings.TrimPrefix(head, keyringManifestPrefix), ":")
	if len(fields) != 4 {
		return malformed("is malformed")
	}
	m := keyringManifest{live: fields[0], counts: map[string]int{}, digest: fields[3]}
	if m.live != keyringChunkFamilyA && m.live != keyringChunkFamilyB {
		return malformed(fmt.Sprintf("names unknown generation %q", m.live))
	}
	for i, family := range []string{keyringChunkFamilyA, keyringChunkFamilyB} {
		count, err := strconv.Atoi(fields[i+1])
		if err != nil || count < 0 || count > keyringMaxChunks {
			return malformed(fmt.Sprintf("has invalid chunk count %q", fields[i+1]))
		}
		m.counts[family] = count
	}
	if m.counts[m.live] == 0 {
		return malformed("names a generation with no chunks")
	}
	if len(m.digest) != hex.EncodedLen(sha256.Size) {
		return malformed("has an invalid digest")
	}
	if _, err := hex.DecodeString(m.digest); err != nil {
		return malformed("has an invalid digest")
	}
	return m, nil
}

// FormatStatuses renders a human-readable status table without leaking token
// material.
func FormatStatuses(statuses []Status) string {
	if len(statuses) == 0 {
		return "No OAuth provider logins are stored."
	}
	var b strings.Builder
	for i, st := range statuses {
		if i > 0 {
			b.WriteByte('\n')
		}
		name := strings.TrimPrefix(st.Key, KeyPrefixProvider)
		b.WriteString(name)
		b.WriteString(": ")
		if !st.HasToken {
			b.WriteString("no token")
			continue
		}
		b.WriteString("logged in")
		if st.Account != "" {
			b.WriteString(" as " + st.Account)
		}
		if st.HasRefreshToken {
			b.WriteString(" (refreshable)")
		}
		if !st.ExpiresAt.IsZero() {
			if st.Expired {
				b.WriteString(", expired at ")
			} else {
				b.WriteString(", expires ")
			}
			b.WriteString(st.ExpiresAt.UTC().Format(time.RFC3339))
		}
	}
	return b.String()
}

// envValue reads a variable. A non-nil env map is authoritative (hermetic): a
// missing key returns "" rather than falling back to the process environment, so
// a caller/test that passes a controlled map can never pick up ambient
// ZERO_OAUTH_* / HOME / XDG_CONFIG_HOME values. Only a nil map uses os.Getenv.
func envValue(env map[string]string, key string) string {
	if env != nil {
		return env[key]
	}
	return os.Getenv(key)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
