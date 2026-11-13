package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Twigpine/zero/internal/redaction"
	"github.com/Twigpine/zero/internal/secrets"
)

// Spill-to-disk for truncated tool output. When a command produces more than
// its context budget, the tail was previously simply gone — the model's only
// recourse was re-running the (possibly expensive or non-idempotent) command
// with a bigger budget. Instead, the full output is written to a cache file
// whose path is included in the truncation notice, so the model can grep or
// read_file the remainder. Spilling is best-effort: on any error truncation
// behaves exactly as before, just without the file hint.

// spillRetention is how long spilled outputs are kept; files older than this
// are opportunistically removed whenever a new spill happens.
const spillRetention = 7 * 24 * time.Hour

// spillRootPath returns the spill directory path without creating or checking
// it. Used by the read-path resolver to recognize spill files as readable.
func spillRootPath() string {
	name := "zero-tool-output"
	if uid := os.Getuid(); uid >= 0 {
		name = fmt.Sprintf("zero-tool-output-%d", uid)
	}
	return filepath.Join(os.TempDir(), name)
}

// resolveSpillReadPath reports whether requestedPath is a file inside the
// spill directory and, if so, returns its verified absolute path. Spill files
// are zero-created, per-uid owned, and already secret-redacted, so letting the
// scoped read tools open them is what makes the truncation notice's
// "read_file it" recovery actually work. Symlinks are resolved and the result
// must still be inside the spill dir, so a planted link cannot smuggle an
// out-of-scope file through this gate.
func resolveSpillReadPath(requestedPath string) (string, bool) {
	if requestedPath == "" || !filepath.IsAbs(requestedPath) {
		return "", false
	}
	root := spillRootPath()
	cleaned := filepath.Clean(requestedPath)
	if cleaned == root || !strings.HasPrefix(cleaned, root+string(filepath.Separator)) {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(cleaned)
	if err != nil {
		return "", false
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false
	}
	if !strings.HasPrefix(resolved, resolvedRoot+string(filepath.Separator)) {
		return "", false
	}
	return resolved, true
}

// spillDir returns the per-user spill directory, creating it on first use.
// Hardened for shared temp dirs (Linux /tmp): the name carries the uid so
// users cannot collide, and a pre-existing path is only accepted when it is a
// real directory (not a symlink that would redirect spills) owned by the
// current user. Any doubt fails the spill — it is best-effort anyway.
func spillDir() (string, error) {
	dir := spillRootPath()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// MkdirAll follows symlinks and leaves an existing directory untouched, so
	// verify what is actually at the path.
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("spill path %s is not a directory", dir)
	}
	if err := checkSpillDirOwner(info); err != nil {
		return "", err
	}
	return dir, nil
}

// spillTruncatedOutput writes the full pre-truncation output to the spill
// directory and returns the file path, or "" when spilling fails. Output is
// scrubbed with the same configured-key redaction the registry applies at the
// tool boundary PLUS the pattern-based secret scanner (AWS keys, tokens, PEM
// blocks, JWTs) that bash applies to its model-visible output — a spill runs
// before that formatter, so without the scan here a spilled file would hold
// pattern-matched credentials in cleartext that the transcript hides.
func spillTruncatedOutput(toolName, output string) string {
	dir, err := spillDir()
	if err != nil {
		return ""
	}
	sweepSpillDir(dir)
	prefix := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			return r
		}
		return '-'
	}, toolName)
	file, err := os.CreateTemp(dir, prefix+"-*.txt")
	if err != nil {
		return ""
	}
	defer file.Close()
	scrubbed := redaction.RedactString(output, redaction.Options{})
	scrubbed, _ = secrets.Redact(scrubbed)
	if _, err := file.WriteString(scrubbed); err != nil {
		_ = os.Remove(file.Name())
		return ""
	}
	return file.Name()
}

// sweepSpillDir removes spill files older than spillRetention. Best-effort:
// errors are ignored, the directory is small, and a sweep runs only when a new
// spill is about to happen anyway.
func sweepSpillDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-spillRetention)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
}
