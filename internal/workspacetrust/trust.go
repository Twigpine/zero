// Package workspacetrust records which workspace roots the user has explicitly
// trusted, so Zero can gate project-scoped executable config (hooks, plugins)
// behind an opt-in per workspace and fail closed on any error.
//
// Trust is keyed on the normalized absolute workspace root (filepath.Abs then
// filepath.EvalSymlinks), and membership is an EXACT match: a nested repo or
// subdirectory under a trusted root is NOT trusted, so trusting a monorepo root
// does not implicitly trust a vendored dependency or submodule that ships its
// own .zero/. The store is a plaintext JSON file at
// <UserConfigDir>/zero/trust.json; it holds workspace paths, not secrets, so it
// is not encrypted, but it is written atomically with restrictive permissions
// (dir 0o700, file 0o600).
package workspacetrust

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/Twigpine/zero/internal/config"
)

// store is the on-disk JSON shape: {"trusted": ["<abs path>", ...]}.
type store struct {
	Trusted []string `json:"trusted"`
}

// storeFilePath returns <UserConfigDir>/zero/trust.json, reusing the config
// package's XDG resolution rather than re-implementing it.
func storeFilePath() (string, error) {
	dir, err := config.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config directory: %w", err)
	}
	return filepath.Join(dir, "zero", "trust.json"), nil
}

// normalize resolves a workspace root to its canonical absolute form:
// filepath.Abs then filepath.EvalSymlinks, falling back to the Abs path when
// EvalSymlinks errors (typically because the path does not exist). Each incoming
// path is normalized once, at its Trust/Untrust/IsTrusted entry point; stored
// entries are then compared as canonical literals and never re-resolved (so a
// retargeted symlink cannot drift or forge a stored match).
func normalize(workspaceRoot string) (string, error) {
	abs, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return "", fmt.Errorf("resolve absolute path for %q: %w", workspaceRoot, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// The path may not exist yet; the absolute form is still a stable key.
		return abs, nil
	}
	return resolved, nil
}

// loadStore reads the trust store. A missing store is not an error: it returns
// an empty store and a nil error. A store that exists but cannot be read or
// parsed returns a non-nil error so callers can fail closed.
func loadStore() (store, error) {
	path, err := storeFilePath()
	if err != nil {
		return store{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return store{}, nil
		}
		return store{}, fmt.Errorf("read trust store %s: %w", path, err)
	}
	var s store
	if err := json.Unmarshal(data, &s); err != nil {
		return store{}, fmt.Errorf("parse trust store %s: %w", path, err)
	}
	return s, nil
}

// saveStore writes the trust store atomically: normalize and dedupe entries,
// marshal with indentation, then write to a temp file (mode 0o600) in the same
// directory and rename it into place. The parent directory is created with mode
// 0o700. This mirrors the atomic-write-with-perms convention in
// internal/securefile/securefile.go (a plaintext write, no encryption here).
//
// Entries are NOT re-normalized here: they are already canonical (each incoming
// path is normalized once at its Trust/Untrust entry point). Re-resolving stored
// entries through the filesystem would re-follow symlinks at write time and let a
// retargeted symlink drift the stored value.
func saveStore(s store) error {
	path, err := storeFilePath()
	if err != nil {
		return err
	}

	// Dedupe and sort so the on-disk form is stable. Entries are treated as
	// already-canonical literals, never re-resolved.
	seen := make(map[string]struct{}, len(s.Trusted))
	roots := make([]string, 0, len(s.Trusted))
	for _, entry := range s.Trusted {
		if _, ok := seen[entry]; ok {
			continue
		}
		seen[entry] = struct{}{}
		roots = append(roots, entry)
	}
	sort.Strings(roots)

	data, err := json.MarshalIndent(store{Trusted: roots}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal trust store: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create trust store directory %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create trust store temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod trust store temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write trust store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write trust store: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("publish trust store: %w", err)
	}
	return nil
}

// IsTrusted reports whether workspaceRoot's own normalized root is in the trust
// store. It is an EXACT membership check: no ancestor or subtree walk, so a
// nested repo under a trusted root is not trusted. An empty root returns
// (false, nil). A missing store returns (false, nil). A store that exists but
// cannot be read returns (false, non-nil) so callers fail closed.
func IsTrusted(workspaceRoot string) (bool, error) {
	if workspaceRoot == "" {
		return false, nil
	}
	query, err := normalize(workspaceRoot)
	if err != nil {
		return false, err
	}
	s, err := loadStore()
	if err != nil {
		return false, err
	}
	// Compare stored entries literally: they are already canonical (normalized at
	// Trust time). Re-normalizing them here would re-run EvalSymlinks on every
	// check, so a trusted path later replaced by a symlink to attacker content
	// would re-resolve to the new target and match (a false positive). Only the
	// incoming query is normalized.
	for _, entry := range s.Trusted {
		if entry == query {
			return true, nil
		}
	}
	return false, nil
}

// Trust adds workspaceRoot's normalized root to the store. It is idempotent:
// trusting an already-trusted root is a no-op that returns nil.
func Trust(workspaceRoot string) error {
	norm, err := normalize(workspaceRoot)
	if err != nil {
		return err
	}
	s, err := loadStore()
	if err != nil {
		return err
	}
	s.Trusted = append(s.Trusted, norm)
	return saveStore(s)
}

// Untrust removes workspaceRoot's normalized root from the store. Removing an
// absent path is a no-op and returns nil.
func Untrust(workspaceRoot string) error {
	target, err := normalize(workspaceRoot)
	if err != nil {
		return err
	}
	s, err := loadStore()
	if err != nil {
		return err
	}
	kept := make([]string, 0, len(s.Trusted))
	for _, entry := range s.Trusted {
		if entry == target {
			continue
		}
		kept = append(kept, entry)
	}
	return saveStore(store{Trusted: kept})
}

// List returns the sorted normalized trusted roots. A missing store returns an
// empty slice and a nil error.
func List() ([]string, error) {
	s, err := loadStore()
	if err != nil {
		return nil, err
	}
	roots := append([]string(nil), s.Trusted...)
	sort.Strings(roots)
	return roots, nil
}
