// Package remotetoken models the configured and resolved identities of the
// remote daemon's file-backed bearer token.
package remotetoken

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Environment variables used to select the remote daemon bearer token.
const (
	EnvToken             = "ZERO_DAEMON_REMOTE_TOKEN"
	EnvTokenFile         = "ZERO_DAEMON_REMOTE_TOKEN_FILE"
	EnvTokenFileResolved = "ZERO_INTERNAL_DAEMON_REMOTE_TOKEN_FILE_RESOLVED"
	// EnvTokenFileIdentity is a trusted daemon-to-worker handoff containing the
	// stable identity of the opened file whose bytes supplied authentication.
	EnvTokenFileIdentity = "ZERO_INTERNAL_DAEMON_REMOTE_TOKEN_FILE_IDENTITY"
)

// FileSource carries both identities needed for file-backed token enforcement.
// Configured is the operator-selected absolute spelling. Resolved is the
// symlink-resolved object selected at daemon startup, when one exists.
type FileSource struct {
	Configured string
	Resolved   string
	Identity   string
}

// SelectedFilePath returns the configured token-file pathname exactly as the
// operator supplied it, unless an inline token takes precedence or the file
// variable is unset. Filename whitespace is data; only all-whitespace is unset.
func SelectedFilePath() string {
	if strings.TrimSpace(os.Getenv(EnvToken)) != "" {
		return ""
	}
	configured := os.Getenv(EnvTokenFile)
	if strings.TrimSpace(configured) == "" {
		return ""
	}
	return configured
}

// SourceFromEnv returns the selected file source without requiring the token to
// exist. It preserves the startup-resolved identity across rotation and falls
// back to resolving the current target for callers outside serve-remote.
func SourceFromEnv() (FileSource, bool) {
	configured := SelectedFilePath()
	if configured == "" {
		return FileSource{}, false
	}
	absolute, err := filepath.Abs(configured)
	if err != nil {
		return FileSource{}, false
	}
	source := FileSource{Configured: absolute}
	if resolved := os.Getenv(EnvTokenFileResolved); strings.TrimSpace(resolved) != "" {
		if absoluteResolved, err := filepath.Abs(resolved); err == nil {
			source.Resolved = absoluteResolved
		}
	}
	if source.Resolved == "" {
		if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
			source.Resolved = resolved
		}
	}
	source.Identity = os.Getenv(EnvTokenFileIdentity)
	return source, true
}

// ResolveSource resolves the currently selected file source for daemon startup.
func ResolveSource() (FileSource, bool, error) {
	configured := SelectedFilePath()
	if configured == "" {
		return FileSource{}, false, nil
	}
	absolute, err := filepath.Abs(configured)
	if err != nil {
		return FileSource{}, false, fmt.Errorf("resolve token file %q: %w", configured, err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return FileSource{}, false, fmt.Errorf("resolve token file %q: %w", configured, err)
	}
	return FileSource{Configured: absolute, Resolved: resolved}, true, nil
}

// PersistSource makes both source identities available to daemon workers.
func PersistSource(source FileSource) error {
	if err := os.Setenv(EnvTokenFile, source.Configured); err != nil {
		return err
	}
	if err := os.Setenv(EnvTokenFileResolved, source.Resolved); err != nil {
		return err
	}
	return os.Setenv(EnvTokenFileIdentity, source.Identity)
}

// IdentityOf returns stable identity when FileInfo exposes it. On Windows,
// FileInfo lacks the required file index; handle-consuming callers must use
// IdentityOfFile instead.
func IdentityOf(info os.FileInfo) (string, bool) { return fileIdentity(info) }

// IdentityOfFile returns the cross-process stable identity of file's already
// opened handle. It never reopens the pathname represented by file.
func IdentityOfFile(file *os.File) (string, bool) { return openFileIdentity(file) }

// Paths returns the distinct configured and resolved identities.
func (source FileSource) Paths() []string {
	if source.Configured == "" {
		return nil
	}
	if source.Resolved == "" || source.Resolved == source.Configured {
		return []string{source.Configured}
	}
	return []string{source.Configured, source.Resolved}
}

// ReadPath is the object pinned at startup, falling back to the configured
// spelling for callers that have not persisted a resolved identity.
func (source FileSource) ReadPath() string {
	if source.Resolved != "" {
		return source.Resolved
	}
	return source.Configured
}
