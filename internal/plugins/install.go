package plugins

// Distribution: install a plugin from a git URL or a local path into a plugins
// directory, with the manifest validated and a content hash recorded in a
// lockfile (plugins.lock). Install copies the plugin tree verbatim but NEVER
// executes any of it — installed plugins still go through normal Stage 09
// activation with permission gating before any tool can run.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Gitlawb/zero/internal/installtxn"
)

// manifestFileName is the plugin manifest filename, matching the loader.
const manifestFileName = "plugin.json"

// LockFileName maps an installed plugin id to its source and content hash.
const LockFileName = "plugins.lock"

// ErrNameClash is returned when an install would overwrite a plugin already
// installed from a DIFFERENT source, unless InstallOptions.Force is set.
var ErrNameClash = errors.New("a different plugin with that id is already installed")

// GitRunner fetches the plugin at source into destination. The default runner
// shallow-clones with the system git (inheriting the process environment, so
// proxy/egress settings are honored). It is injectable so tests never hit the
// network. A runner must only fetch — it must never execute fetched content.
type GitRunner func(ctx context.Context, destination string, source string) error

// InstallOptions configures a single plugin install.
type InstallOptions struct {
	// Source is a git URL or a local filesystem path to a plugin directory (one
	// that contains a plugin.json, or whose tree contains exactly one).
	Source string
	// Dir is the plugins directory to install into (typically the user plugins
	// root from ResolveRoots).
	Dir string
	// Force allows overwriting a plugin installed from a different source.
	Force bool
	// GitRunner overrides the fetch implementation. When nil, a git source is
	// shallow-cloned with the system git.
	GitRunner GitRunner
}

// InstallResult reports what an install did.
type InstallResult struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Version      string `json:"version"`
	ManifestPath string `json:"manifestPath"`
	Hash         string `json:"hash"`
	Source       string `json:"source"`
	Updated      bool   `json:"updated"`
	PreviousHash string `json:"previousHash,omitempty"`
}

// LockEntry records the source and content hash for one installed plugin.
type LockEntry struct {
	Source string `json:"source"`
	Hash   string `json:"hash"`
}

// Install fetches the plugin at options.Source, validates its manifest, copies
// the plugin tree into options.Dir/<id>/, and records a content hash (over the
// manifest bytes) in the lockfile. Fetched content is never executed.
func Install(ctx context.Context, options InstallOptions) (InstallResult, error) {
	source := strings.TrimSpace(options.Source)
	if source == "" {
		return InstallResult{}, errors.New("a plugin source (git URL or path) is required")
	}
	dir := strings.TrimSpace(options.Dir)
	if dir == "" {
		return InstallResult{}, errors.New("a plugins directory is required")
	}
	// Canonicalize a local source so clash detection keys off the resolved path,
	// not the spelling the user typed (relative vs absolute, symlinked vs not).
	source = canonicalSource(source)

	fetchDir, cleanup, err := fetchSource(ctx, source, options.GitRunner)
	if err != nil {
		return InstallResult{}, err
	}
	defer cleanup()

	pluginDir, err := locatePluginDir(fetchDir)
	if err != nil {
		return InstallResult{}, err
	}

	manifestPath := filepath.Join(pluginDir, manifestFileName)
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return InstallResult{}, fmt.Errorf("read %s: %w", manifestFileName, err)
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return InstallResult{}, fmt.Errorf("parse %s: %w", manifestFileName, err)
	}

	// Validate against the same schema the loader uses. The install target id is
	// derived from the (validated) manifest id, so it is safe as a directory name.
	parsed, err := ParseManifest(raw, ParseManifestOptions{
		Source:       SourceUser,
		Root:         dir,
		PluginDir:    filepath.Join(dir, "pending"),
		ManifestPath: manifestPath,
	})
	if err != nil {
		return InstallResult{}, fmt.Errorf("invalid plugin manifest: %w", err)
	}
	id := parsed.ID

	// Hash the SAME filtered tree that copyTree installs (not just the manifest),
	// so a change to any installed file — a tool script, prompt, or bundled skill —
	// is reflected in the lock hash and reported as an update.
	hash, err := hashTree(pluginDir)
	if err != nil {
		return InstallResult{}, fmt.Errorf("hash plugin: %w", err)
	}

	staged, cleanupStage, err := installtxn.StageDir(dir)
	if err != nil {
		return InstallResult{}, err
	}
	defer cleanupStage()
	// Copy the whole plugin tree (entry scripts, prompts, skills) into a sibling
	// staging area. Copy DATA only — never execute it.
	if err := copyTree(pluginDir, staged); err != nil {
		return InstallResult{}, fmt.Errorf("stage plugin: %w", err)
	}
	stagedHash, err := hashTree(staged)
	if err != nil {
		return InstallResult{}, fmt.Errorf("validate staged plugin: %w", err)
	}
	if stagedHash != hash {
		return InstallResult{}, errors.New("validate staged plugin: copied content hash differs from source")
	}

	unlock, err := installtxn.Lock(dir)
	if err != nil {
		return InstallResult{}, err
	}
	defer unlock()
	// Put back anything an earlier run was killed mid-commit, and abort if a
	// transaction could not be resolved: installing over a tree that is still
	// owed a restore destroys the only copy of it. See installtxn.Recover.
	if err := installtxn.Recover(dir, reconcileInterrupted(dir)); err != nil {
		return InstallResult{}, err
	}

	// Re-read under the cross-process lock. Another install may have updated the
	// lockfile while this plugin was fetched and staged.
	lock, err := ReadLock(dir)
	if err != nil {
		return InstallResult{}, err
	}
	previous, existed := lock[id]
	if existed && previous.Source != source && !options.Force {
		return InstallResult{}, fmt.Errorf("%w: %q is installed from %s (use --force to overwrite)", ErrNameClash, id, previous.Source)
	}

	target := filepath.Join(dir, id)
	lock[id] = LockEntry{Source: source, Hash: hash}
	if err := installtxn.CommitDir(target, staged, func() error {
		return writeLock(dir, lock)
	}); err != nil {
		return InstallResult{}, err
	}

	result := InstallResult{
		ID:           id,
		Name:         parsed.Name,
		Version:      parsed.Version,
		ManifestPath: filepath.Join(target, manifestFileName),
		Hash:         hash,
		Source:       source,
	}
	if existed {
		result.Updated = previous.Hash != hash
		result.PreviousHash = previous.Hash
	}
	return result, nil
}

// Remove deletes an installed plugin directory and its lockfile entry. It errors
// if the named plugin is not present in either the dir or the lockfile.
func Remove(dir string, id string) error {
	dir = strings.TrimSpace(dir)
	id = strings.TrimSpace(id)
	if dir == "" || id == "" {
		return errors.New("a plugins directory and plugin id are required")
	}
	if !validInstallID(id) {
		return fmt.Errorf("invalid plugin id %q", id)
	}

	unlock, err := installtxn.Lock(dir)
	if err != nil {
		return err
	}
	defer unlock()
	// Put back anything an earlier run was killed mid-commit, and abort if a
	// transaction could not be resolved: reporting a removal while a backup this
	// run never saw stays on disk lets the next install publish it again. See
	// installtxn.Recover.
	if err := installtxn.Recover(dir, reconcileInterrupted(dir)); err != nil {
		return err
	}

	lock, err := ReadLock(dir)
	if err != nil {
		return err
	}
	_, locked := lock[id]
	target := filepath.Join(dir, id)
	_, statErr := os.Stat(target)
	present := statErr == nil
	if !locked && !present {
		return fmt.Errorf("plugin %q is not installed", id)
	}
	if present {
		if err := installtxn.RemoveDir(target, func() error {
			if !locked {
				return nil
			}
			delete(lock, id)
			return writeLock(dir, lock)
		}); err != nil {
			return fmt.Errorf("remove plugin dir: %w", err)
		}
	} else if locked {
		delete(lock, id)
		if err := writeLock(dir, lock); err != nil {
			return err
		}
	}
	return nil
}

// reconcileInterrupted answers the one question installtxn.Recover cannot read
// off the filesystem: of the two trees an interrupted commit left, which one is
// the one the lockfile records. CommitDir swaps the trees and only then
// publishes, so a kill between the two leaves exactly what a kill after both
// leaves, and only the published metadata tells them apart. The recorded hash is
// the same hashTree the install computed over the tree it copied, so matching it
// against each tree is an exact answer rather than a guess at filesystem shape.
func reconcileInterrupted(dir string) installtxn.Reconciler {
	return func(id string, target string, backup string) (installtxn.Phase, error) {
		lock, err := ReadLock(dir)
		if err != nil {
			// An unreadable lockfile is not an empty one. Reporting a phase from it
			// would be reading a fact out of a file we could not read.
			return installtxn.PhaseUnknown, err
		}
		entry, ok := lock[id]
		if !ok {
			// A missing entry is not proof that the publish never ran. The entry can
			// also be lost after a publish that did run: a truncated or deleted
			// lockfile reads as an empty one, and a directory with no entry is a
			// state this package supports (Load returns it, Remove handles it). The
			// live tree here may be nothing to do with this transaction at all, so
			// there is no phase to report and recovery must not pick between them.
			return installtxn.PhaseUnknown, nil
		}
		if entry.Hash == "" {
			// A hand edited entry records no tree at all, so there is nothing to
			// match either side against and no phase to report.
			return installtxn.PhaseUnknown, nil
		}
		matched, err := treeMatchesHash(target, entry.Hash)
		if err != nil {
			return installtxn.PhaseUnknown, err
		}
		if matched {
			return installtxn.PhaseCommitted, nil
		}
		matched, err = treeMatchesHash(backup, entry.Hash)
		if err != nil {
			return installtxn.PhaseUnknown, err
		}
		if matched {
			return installtxn.PhasePrePublish, nil
		}
		return installtxn.PhaseUnknown, nil
	}
}

// treeMatchesHash reports whether the tree at path hashes to want. A path that
// is not there is a non-match rather than an error: recovery asks about two
// trees it has just seen on disk, so one going missing under it only means that
// tree is not the one the lockfile records. Every other hashing failure is
// returned, because a tree we could not read may well be the recorded one.
func treeMatchesHash(path string, want string) (bool, error) {
	hash, err := hashTree(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return hash == want, nil
}

// ReadLock loads the plugins lockfile from dir. A missing lockfile yields an
// empty map with no error.
func ReadLock(dir string) (map[string]LockEntry, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return map[string]LockEntry{}, nil
	}
	data, err := os.ReadFile(filepath.Join(dir, LockFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]LockEntry{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", LockFileName, err)
	}
	entries := map[string]LockEntry{}
	if len(strings.TrimSpace(string(data))) == 0 {
		return entries, nil
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse %s: %w", LockFileName, err)
	}
	return entries, nil
}

func writeLock(dir string, entries map[string]LockEntry) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create plugins dir: %w", err)
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", LockFileName, err)
	}
	if err := installtxn.WriteFileAtomically(filepath.Join(dir, LockFileName), append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", LockFileName, err)
	}
	return nil
}

// fetchSource resolves a source into a local directory. A local path is used in
// place; a git URL is shallow-cloned into a temp dir via the runner.
func fetchSource(ctx context.Context, source string, runner GitRunner) (string, func(), error) {
	if isLocalPath(source) {
		info, err := os.Stat(source)
		if err != nil {
			return "", func() {}, fmt.Errorf("read source: %w", err)
		}
		if !info.IsDir() {
			return "", func() {}, fmt.Errorf("source must be a directory: %s", source)
		}
		abs, err := filepath.Abs(source)
		if err != nil {
			return "", func() {}, err
		}
		return abs, func() {}, nil
	}

	if runner == nil {
		runner = defaultGitRunner
	}
	temp, err := os.MkdirTemp("", "zero-plugin-fetch-")
	if err != nil {
		return "", func() {}, fmt.Errorf("create temp dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(temp) }
	if err := runner(ctx, temp, source); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("fetch %s: %w", source, err)
	}
	return temp, cleanup, nil
}

// defaultGitRunner shallow-clones source into destination. exec.CommandContext
// inherits the process environment, so proxy/egress settings are honored;
// GIT_TERMINAL_PROMPT=0 prevents a credential prompt from blocking. Cloning only
// fetches; it never executes repository content.
func defaultGitRunner(ctx context.Context, destination string, source string) error {
	command := exec.CommandContext(ctx, "git", "clone", "--depth", "1", "--", source, destination)
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("git clone failed: %v: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// locatePluginDir finds the directory holding plugin.json within root: the root
// itself, or exactly one immediate subdirectory.
func locatePluginDir(root string) (string, error) {
	if fileExists(filepath.Join(root, manifestFileName)) {
		return root, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", fmt.Errorf("scan source: %w", err)
	}
	matches := []string{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(root, entry.Name())
		if fileExists(filepath.Join(candidate, manifestFileName)) {
			matches = append(matches, candidate)
		}
	}
	sort.Strings(matches)
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no %s found in source", manifestFileName)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("source contains multiple plugins (%d); install one at a time", len(matches))
	}
}

// copyTree recursively copies regular files and directories from src to dst. It
// skips the .git directory (clone metadata) and refuses symlinks so a malicious
// source cannot smuggle a link that escapes the install dir. Copying is pure
// I/O — it never executes anything it copies.
func copyTree(src string, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == ".git" {
			continue
		}
		srcPath := filepath.Join(src, name)
		dstPath := filepath.Join(dst, name)
		info, err := os.Lstat(srcPath)
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			// Never recreate a symlink: it could point outside the install dir and
			// turn a copy into a write/read primitive elsewhere.
			continue
		case info.IsDir():
			if err := copyTree(srcPath, dstPath); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			if err := copyFile(srcPath, dstPath, info.Mode().Perm()); err != nil {
				return err
			}
		default:
			// Skip FIFOs, sockets, devices.
			continue
		}
	}
	return nil
}

func copyFile(src string, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// canonicalSource normalizes a local filesystem source to an absolute,
// symlink-evaluated path so a re-install via a different spelling of the same
// directory is recognized as the same source. Remote sources (git URLs) are
// returned unchanged. On any resolution error the input is returned as-is so a
// non-existent local path still surfaces its real error later in fetchSource.
func canonicalSource(source string) string {
	if !isLocalPath(source) {
		return source
	}
	abs, err := filepath.Abs(source)
	if err != nil {
		return source
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// isLocalPath reports whether source is a local filesystem path rather than a
// remote URL. URLs (scheme://… or scp-style host:path) and git shorthand are
// remote.
func isLocalPath(source string) bool {
	if source == "" {
		return false
	}
	if strings.HasPrefix(source, ".") || strings.HasPrefix(source, "/") || strings.HasPrefix(source, "~") {
		return true
	}
	if filepath.IsAbs(source) {
		return true
	}
	if hasURLScheme(source) {
		return false
	}
	if idx := strings.Index(source, ":"); idx > 0 {
		host := source[:idx]
		if strings.Contains(host, "@") {
			return false
		}
		if len(host) == 1 {
			return true // drive letter
		}
		if strings.Contains(host, ".") {
			return false // hostname
		}
	}
	return true
}

func hasURLScheme(source string) bool {
	for _, scheme := range []string{"http://", "https://", "git://", "ssh://", "git+ssh://", "ftp://", "ftps://", "file://"} {
		if strings.HasPrefix(strings.ToLower(source), scheme) {
			return true
		}
	}
	return false
}

// validInstallID guards a plugin id used as a directory component. Manifest ids
// already match pluginIDPattern, but Remove takes an id directly from the user.
func validInstallID(id string) bool {
	if !pluginIDPattern.MatchString(id) {
		return false
	}
	return id == filepath.Base(id) && !strings.ContainsAny(id, `/\`) && !strings.Contains(id, "..")
}

// hashTree computes a content hash over the same filtered tree that copyTree
// installs: regular files only, .git and symlinks skipped, walked in a stable
// sorted order. Each file contributes its plugin-relative path, executable bit,
// and bytes, so renames, mode flips, and content edits all change the hash.
func hashTree(root string) (string, error) {
	hasher := sha256.New()
	if err := hashTreeInto(hasher, root, root); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}

func hashTreeInto(hasher io.Writer, root string, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		if name == ".git" {
			continue
		}
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			// Skipped by copyTree, so excluded from the hash too.
			continue
		case info.IsDir():
			if err := hashTreeInto(hasher, root, path); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			executable := 0
			if info.Mode().Perm()&0o111 != 0 {
				executable = 1
			}
			// Null-delimited header keeps file boundaries unambiguous (paths cannot
			// contain null bytes) so two trees cannot collide by shifting bytes.
			header := fmt.Sprintf("%s\x00%d\x00", filepath.ToSlash(rel), executable)
			if _, err := io.WriteString(hasher, header); err != nil {
				return err
			}
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			if _, err := io.Copy(hasher, file); err != nil {
				_ = file.Close()
				return err
			}
			if err := file.Close(); err != nil {
				return err
			}
		default:
			// FIFOs, sockets, devices: skipped by copyTree, excluded here.
			continue
		}
	}
	return nil
}
