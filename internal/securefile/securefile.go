// Package securefile provides AES-256-GCM encryption-at-rest for small on-disk
// blobs under a per-user random secret persisted (0600) beside the data file. The
// on-disk blob is nonce || ciphertext; GCM provides confidentiality AND tamper
// detection, so a corrupted/forged file fails closed on Open. It is a neutral,
// reusable extraction of the OAuth token store's encrypted-file backend, shared by
// the credential store (and available to other callers).
package securefile

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"
)

const (
	// secretBytes is the AES-256 key length kept in the per-user secret file.
	secretBytes = 32

	secretRetryAttempts  = 500
	secretRetryDelay     = 2 * time.Millisecond
	secureLockStaleAfter = 10 * time.Second
)

var (
	openSecretLockFile = os.OpenFile
	syncSecretFileFn   = (*os.File).Sync
	syncSecretDirFn    = syncSecretDir
	secureLockSeq      atomic.Uint64
)

// syncSecretDir fsyncs the directory that now holds the published secret.
// Windows cannot sync directory handles, so the rename is best-effort durable
// there. A directory the user can write but not read cannot be opened for
// syncing either; that is also best effort, because the write it follows
// already succeeded. Every other failure is reported.
func syncSecretDir(dir string) error {
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

// Crypter encrypts a blob at rest with AES-256-GCM under a per-user random secret
// persisted (0600) at secretPath.
type Crypter struct {
	secretPath     string
	publicationDir string
}

// NewCrypter returns a Crypter whose key lives at secretPath (created on first
// Seal). Keep secretPath beside the data file (e.g. data + ".secret").
func NewCrypter(secretPath string) *Crypter {
	return &Crypter{secretPath: secretPath}
}

// NewCrypterWithPublicationDir is NewCrypter with secret temp files published
// through publicationDir instead of beside the secret. Callers that protect a
// sandboxed process from racing the plaintext key use this to put the temp file
// in a directory their sandbox profile denies. The directory must be on the
// same filesystem as secretPath; the final rename remains atomic.
func NewCrypterWithPublicationDir(secretPath, publicationDir string) *Crypter {
	return &Crypter{secretPath: secretPath, publicationDir: publicationDir}
}

// aead loads (or, when create is set, generates) the secret and returns the GCM
// AEAD. Open passes create=false so a missing secret is a hard error rather than
// silently minting a new key that could never decrypt the existing file.
func (c *Crypter) aead(create bool) (cipher.AEAD, error) {
	secret, err := loadOrCreateSecret(c.secretPath, c.publicationDir, create)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(secret)
	if err != nil {
		return nil, fmt.Errorf("securefile: build cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

// Seal encrypts plaintext, prefixing a fresh random nonce. It creates the secret
// on first use.
func (c *Crypter) Seal(plaintext []byte) ([]byte, error) {
	gcm, err := c.aead(true)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("securefile: generate nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// Open decrypts a nonce||ciphertext blob, failing closed on a missing secret, a
// short blob, or a failed authentication tag (tampering / wrong key).
func (c *Crypter) Open(blob []byte) ([]byte, error) {
	gcm, err := c.aead(false)
	if err != nil {
		return nil, err
	}
	if len(blob) < gcm.NonceSize() {
		return nil, errors.New("securefile: encrypted file is too short")
	}
	nonce, ciphertext := blob[:gcm.NonceSize()], blob[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("securefile: decrypt (wrong secret or tampered): %w", err)
	}
	return plaintext, nil
}

// loadOrCreateSecret reads the 32-byte secret at path. When create is set and the
// file is absent, it generates a random secret and creates the file atomically
// (0600). A wrong-sized existing secret fails closed (corruption).
func loadOrCreateSecret(path, publicationDir string, create bool) ([]byte, error) {
	if data, err := readSecretFileRetry(path); err == nil {
		return data, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if !create {
		return nil, fmt.Errorf("securefile: secret %s is missing; cannot decrypt the file", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return createSecretFile(path, publicationDir)
}

func createSecretFile(path, publicationDir string) ([]byte, error) {
	lockPath := path + ".lock"
	token := fmt.Sprintf("%d-%d-%d", os.Getpid(), time.Now().UnixNano(), secureLockSeq.Add(1))
	var lastErr error
	for attempt := 0; attempt < secretRetryAttempts; attempt++ {
		lock, err := openSecretLockFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			if _, werr := lock.WriteString(token); werr != nil {
				_ = lock.Close()
				_ = os.Remove(lockPath)
				return nil, fmt.Errorf("securefile: write secret lock: %w", werr)
			}
			if cerr := lock.Close(); cerr != nil {
				_ = os.Remove(lockPath)
				return nil, fmt.Errorf("securefile: close secret lock: %w", cerr)
			}
			defer func() {
				if data, rerr := os.ReadFile(lockPath); rerr == nil && string(data) == token {
					_ = os.Remove(lockPath)
				}
			}()
			if data, rerr := readSecretFileRetry(path); rerr == nil {
				return data, nil
			} else if !errors.Is(rerr, os.ErrNotExist) {
				return nil, rerr
			}
			return writeNewSecretFile(path, publicationDir)
		}
		// On Windows a concurrent holder's os.Remove leaves the lock file in a
		// "delete pending" state, so an O_EXCL create races it with
		// ERROR_ACCESS_DENIED (os.ErrPermission) rather than ErrExist. Treat that
		// as contention and retry, otherwise concurrent secret creation
		// spuriously fails on Windows.
		if !errors.Is(err, os.ErrExist) && !errors.Is(err, os.ErrPermission) {
			return nil, fmt.Errorf("securefile: create secret lock: %w", err)
		}
		// Remember the lock-creation error itself rather than a subsequent
		// "secret file doesn't exist yet" read error -- otherwise a real
		// contention/ACL problem is masked by the expected-while-waiting
		// ErrNotExist once the retries are exhausted.
		lastErr = err

		// Reclaim a stale lock left by a crashed holder
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > secureLockStaleAfter {
			if reclaimStaleLock(lockPath, token, secureLockStaleAfter) {
				continue
			}
		}

		if data, rerr := readSecretFileRetry(path); rerr == nil {
			return data, nil
		}
		time.Sleep(secretRetryDelay)
	}
	return nil, fmt.Errorf("securefile: timed out waiting for secret %s: %w", path, lastErr)
}

func reclaimStaleLock(lockPath, token string, staleAfter time.Duration) bool {
	reclaimed := fmt.Sprintf("%s.stale.%s", lockPath, token)
	if err := os.Rename(lockPath, reclaimed); err != nil {
		return false
	}
	if info, err := os.Stat(reclaimed); err == nil && time.Since(info.ModTime()) <= staleAfter {
		// The lock was replaced between our stat and rename, so put the fresh
		// lock back without clobbering a third holder that acquired lockPath in
		// the meantime.
		_ = restoreReclaimedLock(reclaimed, lockPath)
		return false
	}
	_ = os.Remove(reclaimed)
	return true
}

// restoreReclaimedLock restores a quarantined lock only if lockPath is still
// free. The destination is created exclusively; a newer holder is never
// replaced. The quarantined copy is discarded either way.
func restoreReclaimedLock(reclaimed, lockPath string) error {
	data, err := os.ReadFile(reclaimed)
	if err != nil {
		_ = os.Remove(reclaimed)
		return err
	}
	lock, err := openSecretLockFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		_ = os.Remove(reclaimed)
		return err
	}
	if _, err := lock.Write(data); err != nil {
		_ = lock.Close()
		_ = os.Remove(lockPath)
		_ = os.Remove(reclaimed)
		return err
	}
	if err := lock.Close(); err != nil {
		_ = os.Remove(lockPath)
		_ = os.Remove(reclaimed)
		return err
	}
	_ = os.Remove(reclaimed)
	return nil
}

func writeNewSecretFile(path, publicationDir string) ([]byte, error) {
	secret := make([]byte, secretBytes)
	if _, err := io.ReadFull(rand.Reader, secret); err != nil {
		return nil, fmt.Errorf("securefile: generate secret: %w", err)
	}
	dir := publicationDir
	prefix := filepath.Base(path) + ".*.tmp"
	if dir == "" {
		dir = filepath.Dir(path)
	} else {
		// Keep the prefix oauth's store reset uses to clean crash residue from a
		// publication directory.
		prefix = "publish-*"
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("securefile: create secret publication dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, prefix)
	if err != nil {
		return nil, fmt.Errorf("securefile: create secret temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("securefile: chmod secret temp file: %w", err)
	}
	if _, werr := tmp.Write(secret); werr != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("securefile: write secret: %w", werr)
	}
	if serr := syncSecretFileFn(tmp); serr != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("securefile: sync secret: %w", serr)
	}
	if cerr := tmp.Close(); cerr != nil {
		return nil, fmt.Errorf("securefile: write secret: %w", cerr)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return nil, fmt.Errorf("securefile: publish secret: %w", err)
	}
	// The key is now visible at its final name; sync the destination directory
	// so the rename itself survives a crash. The source publication directory
	// only held an unlinked temp entry and does not need a barrier.
	if err := syncSecretDirFn(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("securefile: sync secret directory: %w", err)
	}
	return secret, nil
}

func readSecretFileRetry(path string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < secretRetryAttempts; attempt++ {
		data, err := readSecretFile(path)
		if !isTransientSecretAccessError(err) {
			return data, err
		}
		lastErr = err
		time.Sleep(secretRetryDelay)
	}
	return nil, fmt.Errorf("securefile: timed out waiting for secret %s: %w", path, lastErr)
}

// readSecretFile reads and validates the secret at path, returning a wrapped
// os.ErrNotExist when it is absent so callers can branch on creation.
func readSecretFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("securefile: read secret: %w", err)
	}
	if len(data) != secretBytes {
		return nil, fmt.Errorf("securefile: secret at %s has unexpected size %d", path, len(data))
	}
	return data, nil
}
