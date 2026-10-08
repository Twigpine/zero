//go:build windows

package update

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
)

// randomStagingSuffix returns hex-encoded random bytes for stagingFilePath.
// Overridden in tests for a deterministic path; production always takes this
// default, cryptographically random one.
var randomStagingSuffix = func() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// stagingFilePath returns an unpredictable path in targetPath's directory
// (same filesystem, so the later rename into place is atomic). A fixed
// "<target>.new" name is guessable in advance, and a lower-privileged
// process that can write in the installation directory could pre-create it
// as a hard link or reparse point to another file the elevated updater can
// write, turning the staging copy into an arbitrary-file-overwrite primitive.
// createStagingFile's exclusive, no-follow creation is the other half of
// closing that: even a correctly-guessed name can't be opened through.
func stagingFilePath(targetPath string) (string, error) {
	suffix, err := randomStagingSuffix()
	if err != nil {
		return "", fmt.Errorf("generate staging file name: %w", err)
	}
	name := filepath.Base(targetPath) + "." + suffix + ".new"
	return filepath.Join(filepath.Dir(targetPath), name), nil
}
