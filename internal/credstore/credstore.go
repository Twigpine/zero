// Package credstore persists provider API keys outside config.json so secrets are
// not written to disk in cleartext. It resolves one backend at construction: the OS
// keyring on macOS (the `security` keychain), otherwise an AES-256-GCM encrypted file
// (the Linux secret-tool keyring needs a running secret service that is absent on
// headless/CI machines, so it is opt-in via ZERO_CRED_STORAGE=keyring). A plaintext
// file is available only as an explicit ZERO_CRED_STORAGE=file opt-out. OAuth tokens
// stay in internal/oauth; this store is for raw API keys.
package credstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/Gitlawb/zero/internal/keyring"
	"github.com/Gitlawb/zero/internal/securefile"
)

const (
	keyringService = "zero"
	keyringPrefix  = "apikey:" // keyring account = "apikey:<provider>"

	encryptedFileName = "credentials.enc"
	plaintextFileName = "credentials.json"
)

// KeyringClient is the minimal OS-keyring surface the store needs;
// *keyring.Keyring satisfies it. Injectable for tests.
type KeyringClient interface {
	Available() bool
	Set(service, account, secret string) error
	Get(service, account string) (string, bool, error)
	Delete(service, account string) (bool, error)
}

// Options configures the credential store.
type Options struct {
	// Dir is the per-user data directory for the file backends.
	Dir string
	// Storage selects the backend: "" (auto), "keyring", "encrypted-file", or "file"
	// (plaintext opt-out). When empty it falls back to ZERO_CRED_STORAGE, then auto:
	// keyring on macOS (the `security` keychain is reliable and needs no daemon),
	// encrypted-file everywhere else (Linux `secret-tool` needs a running secret
	// service that is absent on headless/CI boxes and would block/error). Linux/other
	// users can still opt into the keyring with ZERO_CRED_STORAGE=keyring.
	Storage string
	// Keyring is the keyring client; nil => keyring.New().
	Keyring KeyringClient
	// Env reads environment variables; nil => os.Getenv.
	Env func(string) string
	// GOOS overrides the platform for auto backend resolution; "" => runtime.GOOS.
	// Exists so tests can exercise the per-OS default deterministically.
	GOOS string
}

// Store reads/writes provider API keys via the resolved backend.
type Store struct {
	backend string // "keyring", "encrypted-file", or "file"
	kr      KeyringClient
	file    string
	crypter *securefile.Crypter // non-nil only for "encrypted-file"
}

// New resolves the backend. It never silently downgrades to plaintext: "file" must
// be chosen explicitly; auto mode picks keyring then encrypted-file.
func New(options Options) (*Store, error) {
	env := options.Env
	if env == nil {
		env = os.Getenv
	}
	kr := options.Keyring
	if kr == nil {
		kr = keyring.New()
	}
	goos := strings.TrimSpace(options.GOOS)
	if goos == "" {
		goos = runtime.GOOS
	}
	storage := strings.TrimSpace(options.Storage)
	if storage == "" {
		storage = strings.TrimSpace(env("ZERO_CRED_STORAGE"))
	}
	if storage == "" {
		// Keyring is the default only on macOS, where `security` is reliable and needs
		// no running daemon. Elsewhere (Linux secret-tool, headless/CI) default to the
		// encrypted file; keyring stays available via an explicit ZERO_CRED_STORAGE.
		if goos == "darwin" && kr.Available() {
			storage = "keyring"
		} else {
			storage = "encrypted-file"
		}
	}

	switch storage {
	case "keyring":
		if !kr.Available() {
			return nil, fmt.Errorf("credstore: keyring requested but no OS keyring backend on this platform; use encrypted-file")
		}
		return &Store{backend: "keyring", kr: kr}, nil
	case "encrypted-file":
		if strings.TrimSpace(options.Dir) == "" {
			return nil, fmt.Errorf("credstore: Dir is required for encrypted-file storage")
		}
		path := filepath.Join(options.Dir, encryptedFileName)
		return &Store{backend: "encrypted-file", file: path, crypter: securefile.NewCrypter(path + ".secret")}, nil
	case "file":
		if strings.TrimSpace(options.Dir) == "" {
			return nil, fmt.Errorf("credstore: Dir is required for file storage")
		}
		return &Store{backend: "file", file: filepath.Join(options.Dir, plaintextFileName)}, nil
	default:
		return nil, fmt.Errorf("credstore: unknown storage %q (want \"keyring\", \"encrypted-file\", or \"file\")", storage)
	}
}

// Backend reports the resolved backend ("keyring", "encrypted-file", "file"), for
// display ("how is this key stored").
func (s *Store) Backend() string { return s.backend }

// Encrypted reports whether secrets are protected at rest (keyring or AES file).
func (s *Store) Encrypted() bool { return s.backend == "keyring" || s.backend == "encrypted-file" }

// Set stores (or overwrites) the API key for provider.
func (s *Store) Set(provider, key string) (err error) {
	provider = normalizeProvider(provider)
	if provider == "" {
		return fmt.Errorf("credstore: provider is required")
	}
	if s.backend == "keyring" {
		// The OS keyring serializes its own writes; the file lock guards the
		// file backends only.
		return s.kr.Set(keyringService, keyringPrefix+provider, key)
	}
	// READ AND WRITE UNDER ONE LOCK. Separately locking each half would still
	// lose keys: two writers would each read the same map, each add their own
	// provider, and the second rename would publish a file missing the first.
	// Measured before this existed: 1 of 100 concurrent Set calls survived.
	release, err := s.acquireFileLock(true)
	if err != nil {
		return err
	}
	// Joined, not chosen between: a release failure annotates the result instead
	// of masking the write error that actually explains what went wrong.
	defer func() { err = errors.Join(err, release()) }()
	data, err := s.read()
	if err != nil {
		return err
	}
	data[provider] = key
	return s.write(data)
}

// Get returns the API key for provider and whether one is stored.
func (s *Store) Get(provider string) (value string, found bool, err error) {
	provider = normalizeProvider(provider)
	if provider == "" {
		return "", false, nil
	}
	if s.backend == "keyring" {
		return s.kr.Get(keyringService, keyringPrefix+provider)
	}
	// A shared lock, so a read serializes against a writer's publish. On unix
	// the rename is atomic and this only prevents reading an empty file mid-swap;
	// on Windows an unsynchronized reader holding the file open would block the
	// writer's rename outright, so readers must coordinate through the same lock.
	release, err := s.acquireFileLock(false)
	if err != nil {
		return "", false, err
	}
	defer func() { err = errors.Join(err, release()) }()
	data, err := s.read()
	if err != nil {
		return "", false, err
	}
	key, ok := data[provider]
	return key, ok, nil
}

// Delete removes the API key for provider, reporting whether one existed.
func (s *Store) Delete(provider string) (existed bool, err error) {
	provider = normalizeProvider(provider)
	if provider == "" {
		return false, nil
	}
	if s.backend == "keyring" {
		return s.kr.Delete(keyringService, keyringPrefix+provider)
	}
	// The same lock as Set: a delete is a read-modify-write too, and one racing
	// a set would otherwise resurrect or erase an unrelated provider.
	release, err := s.acquireFileLock(true)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, release()) }()
	data, err := s.read()
	if err != nil {
		return false, err
	}
	if _, ok := data[provider]; !ok {
		return false, nil
	}
	delete(data, provider)
	return true, s.write(data)
}

// Providers lists providers with a stored key (sorted), for display/audit.
func (s *Store) Providers() (names []string, err error) {
	if s.backend == "keyring" {
		// The OS keyring has no enumerate-by-prefix in our minimal surface; callers
		// that need a list pass known provider names to Get. Return empty here.
		return nil, nil
	}
	release, err := s.acquireFileLock(false)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, release()) }()
	data, err := s.read()
	if err != nil {
		return nil, err
	}
	names = make([]string, 0, len(data))
	for name := range data {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// --- file backends (encrypted-file / file) ---------------------------------

func (s *Store) read() (map[string]string, error) {
	raw, err := os.ReadFile(s.file)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("credstore: read %s: %w", s.file, err)
	}
	if s.crypter != nil {
		raw, err = s.crypter.Open(raw)
		if err != nil {
			return nil, err
		}
	}
	data := map[string]string{}
	if len(raw) == 0 {
		return data, nil
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("credstore: parse %s: %w", s.file, err)
	}
	return data, nil
}

func (s *Store) write(data map[string]string) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("credstore: encode: %w", err)
	}
	if s.crypter != nil {
		payload, err = s.crypter.Seal(payload)
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(s.file), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.file), filepath.Base(s.file)+".*.tmp")
	if err != nil {
		return fmt.Errorf("credstore: temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("credstore: chmod: %w", err)
	}
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("credstore: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("credstore: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("credstore: write: %w", err)
	}
	if err := os.Rename(tmpPath, s.file); err != nil {
		return fmt.Errorf("credstore: publish: %w", err)
	}
	return nil
}

// lockPath is the advisory lock guarding a read-modify-write of the credential
// file. Beside the data file, never the data file itself — write publishes by
// rename and would carry the lock away with the old inode.
func (s *Store) lockPath() string { return s.file + ".lock" }

// filepathDir is filepath.Dir, named locally so the two platform lock files can
// share it without either importing path/filepath for one call.
func filepathDir(path string) string { return filepath.Dir(path) }

func normalizeProvider(provider string) string {
	return strings.ToLower(strings.TrimSpace(provider))
}
