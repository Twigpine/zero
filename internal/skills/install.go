package skills

// Distribution: install a skill from a git URL or a local path into the skills
// directory, with a content hash recorded in a lockfile (skills.lock) so every
// install/update is verifiable. Skills are markdown, so install NEVER executes
// fetched content — it copies and validates the SKILL.md and nothing else.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Gitlawb/zero/internal/installtxn"
)

// LockFileName is the name of the per-directory lockfile that maps an installed
// skill name to the source it was installed from and the content hash recorded
// at install time.
const LockFileName = "skills.lock"

// ErrNameClash is returned when an install would overwrite a skill that was
// installed from a DIFFERENT source, unless InstallOptions.Force is set. It is a
// safety guard so a remote source can never silently shadow a skill the user
// already trusts.
var ErrNameClash = errors.New("a different skill with that name is already installed")

// GitRunner fetches the skill at source into destination. The default runner
// shallow-clones with the system git, which inherits the process environment
// (and therefore any proxy/egress settings). It is injectable so tests never hit
// the network. A runner must only fetch — it must never execute fetched content.
type GitRunner func(ctx context.Context, destination string, source string) error

// InstallOptions configures a single skill install.
type InstallOptions struct {
	// Source is a git URL or a local filesystem path to a skill directory (one
	// that contains a SKILL.md, or whose tree contains exactly one).
	Source string
	// Dir is the skills directory to install into (typically DefaultDir(env)).
	Dir string
	// Force allows overwriting a skill that was installed from a different source.
	Force bool
	// GitRunner overrides the fetch implementation (tests/proxy control). When
	// nil, a git source is shallow-cloned with the system git.
	GitRunner GitRunner
}

// InstallResult reports what an install did.
type InstallResult struct {
	Name string `json:"name"`
	// Path is the absolute path to the installed SKILL.md.
	Path string `json:"path"`
	// Hash is the content hash recorded for the installed skill.
	Hash string `json:"hash"`
	// Source echoes the source the skill was installed from.
	Source string `json:"source"`
	// Updated is true when an existing install was replaced; PreviousHash then
	// carries the prior recorded hash so a caller can show the change.
	Updated      bool   `json:"updated"`
	PreviousHash string `json:"previousHash,omitempty"`
}

// LockEntry records the source and content hash for one installed skill.
type LockEntry struct {
	Source string `json:"source"`
	Hash   string `json:"hash"`
}

// SkillInfo bundles a discovered skill with its recorded source and hash, for
// `skill info`.
type SkillInfo struct {
	Skill     Skill  `json:"skill"`
	Source    string `json:"source,omitempty"`
	Hash      string `json:"hash,omitempty"`
	HashDrift bool   `json:"hashDrift"`
}

// Install fetches the skill at options.Source and copies its SKILL.md into
// options.Dir/<name>/, validating the frontmatter and recording a content hash
// in the lockfile. A git URL is fetched via the (injectable) GitRunner into a
// temp dir; a local path is read in place. Fetched content is never executed.
func Install(ctx context.Context, options InstallOptions) (InstallResult, error) {
	source := strings.TrimSpace(options.Source)
	if source == "" {
		return InstallResult{}, errors.New("a skill source (git URL or path) is required")
	}
	dir := strings.TrimSpace(options.Dir)
	if dir == "" {
		return InstallResult{}, errors.New("a skills directory is required")
	}
	// Canonicalize a local source so clash detection keys off the resolved path,
	// not the spelling the user typed (relative vs absolute, symlinked vs not).
	source = canonicalSource(source)

	fetchDir, cleanup, err := fetchSource(ctx, source, options.GitRunner)
	if err != nil {
		return InstallResult{}, err
	}
	defer cleanup()

	skillDir, err := locateSkillDir(fetchDir)
	if err != nil {
		return InstallResult{}, err
	}

	// Validate by parsing the SKILL.md through the same loader the runtime uses;
	// reject anything without a usable frontmatter/dir name.
	manifestPath := filepath.Join(skillDir, skillFileName)
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return InstallResult{}, fmt.Errorf("read SKILL.md: %w", err)
	}
	parsed := parseSkill(filepath.Base(skillDir), manifestPath, string(data))
	name := strings.TrimSpace(parsed.Name)
	if name == "" || !validSkillName(name) {
		return InstallResult{}, fmt.Errorf("skill has no usable name (set a frontmatter `name:` or use a directory name of letters, numbers, dots, dashes, or underscores)")
	}

	hash := hashContent(data)

	staged, cleanupStage, err := installtxn.StageDir(dir)
	if err != nil {
		return InstallResult{}, err
	}
	defer cleanupStage()
	if err := os.MkdirAll(staged, 0o755); err != nil {
		return InstallResult{}, fmt.Errorf("create staged skill dir: %w", err)
	}
	stagedManifest := filepath.Join(staged, skillFileName)
	if err := os.WriteFile(stagedManifest, data, 0o644); err != nil {
		return InstallResult{}, fmt.Errorf("stage SKILL.md: %w", err)
	}
	stagedData, err := os.ReadFile(stagedManifest)
	if err != nil || hashContent(stagedData) != hash {
		if err != nil {
			return InstallResult{}, fmt.Errorf("validate staged SKILL.md: %w", err)
		}
		return InstallResult{}, errors.New("validate staged SKILL.md: copied content hash differs from source")
	}

	unlock, err := installtxn.Lock(dir)
	if err != nil {
		return InstallResult{}, err
	}
	defer unlock()
	// Put back anything an earlier run was killed mid-commit; see installtxn.Recover.
	// Abort on a transaction it could not resolve: installing over a target that is
	// still owed a restore destroys the only copy of the tree it is owed.
	if err := installtxn.Recover(dir, lockReconciler(dir)); err != nil {
		return InstallResult{}, err
	}

	// Re-read under the cross-process lock. Another install may have updated the
	// lockfile while this skill was fetched and staged.
	lock, err := ReadLock(dir)
	if err != nil {
		return InstallResult{}, err
	}
	previous, existed := lock[name]
	// A different source under the same name is a clash unless Force is set.
	if existed && previous.Source != source && !options.Force {
		return InstallResult{}, fmt.Errorf("%w: %q is installed from %s (use --force to overwrite)", ErrNameClash, name, previous.Source)
	}

	target := filepath.Join(dir, name)
	lock[name] = LockEntry{Source: source, Hash: hash}
	if err := installtxn.CommitDir(target, staged, func() error {
		return writeLock(dir, lock)
	}); err != nil {
		return InstallResult{}, err
	}

	result := InstallResult{
		Name:   name,
		Path:   filepath.Join(target, skillFileName),
		Hash:   hash,
		Source: source,
	}
	if existed {
		result.Updated = previous.Hash != hash
		result.PreviousHash = previous.Hash
	}
	return result, nil
}

// lockReconciler answers, for one interrupted workspace, which of the two trees
// beside each other the lockfile records. A kill between the tree swap and the
// lockfile publish leaves exactly what a kill after both leaves, so filesystem
// shape cannot tell them apart and only the recorded hash can: the tree it
// describes is the one the user was told they had.
func lockReconciler(dir string) installtxn.Reconciler {
	return func(name string, target string, backup string) (installtxn.Phase, error) {
		lock, err := ReadLock(dir)
		if err != nil {
			return installtxn.PhaseUnknown, err
		}
		entry, locked := lock[name]
		if !locked {
			// A missing entry is not proof that the publish never ran. The entry can
			// also be lost after a publish that did run: a truncated or deleted
			// lockfile reads as an empty one. A hand-written skill the lockfile never
			// named is an ordinary thing to find here too, and replacing it with the
			// retained backup would delete the user's own work.
			return installtxn.PhaseUnknown, nil
		}
		if entry.Hash == "" {
			// Nothing to compare against, so neither tree can be shown to be the
			// recorded one and recovery must not pick between them.
			return installtxn.PhaseUnknown, nil
		}
		targetHash, err := manifestHash(target)
		if err != nil {
			return installtxn.PhaseUnknown, err
		}
		if targetHash == entry.Hash {
			return installtxn.PhaseCommitted, nil
		}
		backupHash, err := manifestHash(backup)
		if err != nil {
			return installtxn.PhaseUnknown, err
		}
		if backupHash == entry.Hash {
			return installtxn.PhasePrePublish, nil
		}
		return installtxn.PhaseUnknown, nil
	}
}

// manifestHash hashes the SKILL.md under path the same way an install records
// it. A tree with no SKILL.md is a non-match rather than a failure: an install
// killed mid-copy can leave one, and it simply is not the tree the lockfile
// describes. Any other read error means the comparison could not be made at all,
// which is not the same answer and is reported.
func manifestHash(path string) (string, error) {
	data, err := os.ReadFile(filepath.Join(path, skillFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read %s: %w", skillFileName, err)
	}
	return hashContent(data), nil
}

// Remove deletes an installed skill directory and its lockfile entry. It errors
// if the named skill is not present in either the dir or the lockfile.
func Remove(dir string, name string) error {
	dir = strings.TrimSpace(dir)
	name = strings.TrimSpace(name)
	if dir == "" || name == "" {
		return errors.New("a skills directory and skill name are required")
	}
	if !validSkillName(name) {
		return fmt.Errorf("invalid skill name %q", name)
	}

	unlock, err := installtxn.Lock(dir)
	if err != nil {
		return err
	}
	defer unlock()
	// Put back anything an earlier run was killed mid-commit; see installtxn.Recover.
	// Abort on a transaction it could not resolve: reporting a successful removal
	// leaves a backup nothing else reads, and the next install's recovery would
	// publish it again and reinstate what the user deleted.
	if err := installtxn.Recover(dir, lockReconciler(dir)); err != nil {
		return err
	}

	lock, err := ReadLock(dir)
	if err != nil {
		return err
	}
	_, locked := lock[name]
	target := filepath.Join(dir, name)
	_, statErr := os.Stat(target)
	present := statErr == nil
	if !locked && !present {
		return fmt.Errorf("skill %q is not installed", name)
	}

	if present {
		if err := installtxn.RemoveDir(target, func() error {
			if !locked {
				return nil
			}
			delete(lock, name)
			return writeLock(dir, lock)
		}); err != nil {
			return fmt.Errorf("remove skill dir: %w", err)
		}
	} else if locked {
		delete(lock, name)
		if err := writeLock(dir, lock); err != nil {
			return err
		}
	}
	return nil
}

// InfoFromRoots resolves the named skill across discovery roots (earlier roots
// win). Lock source/hash are attached only when the winning skill lives under
// primaryDir and that dir's lockfile has an entry — agents-only skills return
// frontmatter + path with empty Source/Hash. primaryDir is typically the Zero
// write root (DefaultDir / skillsDir).
func InfoFromRoots(primaryDir string, roots []string, name string) (SkillInfo, bool) {
	loaded, _, err := LoadFromRoots(roots)
	if err != nil {
		return SkillInfo{}, false
	}
	target := strings.TrimSpace(name)
	var skill Skill
	found := false
	for _, candidate := range loaded {
		if candidate.Name == target {
			skill = candidate
			found = true
			break
		}
	}
	if !found {
		return SkillInfo{}, false
	}
	info := SkillInfo{Skill: skill}
	primaryDir = strings.TrimSpace(primaryDir)
	if primaryDir == "" {
		return info, true
	}
	// Only attach lock metadata when the winner is from the primary write root.
	// Compare path prefixes after cleaning so agents-only skills never pick up a
	// Zero lock entry by name coincidence.
	skillPath := filepath.Clean(skill.Path)
	primaryRoot := filepath.Clean(primaryDir)
	rel, err := filepath.Rel(primaryRoot, skillPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return info, true
	}
	if lock, err := ReadLock(primaryDir); err == nil {
		if entry, found := lock[skill.Name]; found {
			info.Source = entry.Source
			info.Hash = entry.Hash
		}
	}
	info.HashDrift = skillHashDrift(skill, info.Hash)
	return info, true
}

// ReadLock loads the lockfile from dir. A missing lockfile yields an empty map
// with no error so callers can treat "no lockfile" as "nothing installed".
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

// writeLock persists the lockfile deterministically (json.Marshal sorts map keys),
// creating the skills dir if necessary.
func writeLock(dir string, entries map[string]LockEntry) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create skills dir: %w", err)
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

// fetchSource resolves a source into a local directory to read from. A local
// path is used in place (no copy, no cleanup); a git URL is shallow-cloned into a
// temp dir via the runner. The returned cleanup is always safe to call.
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
	temp, err := os.MkdirTemp("", "zero-skill-fetch-")
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
// inherits the process environment, so proxy/egress settings are honored, and
// GIT_TERMINAL_PROMPT=0 keeps a missing-credential clone from blocking on a
// terminal prompt. Cloning only fetches; it never executes repository content.
func defaultGitRunner(ctx context.Context, destination string, source string) error {
	command := exec.CommandContext(ctx, "git", "clone", "--depth", "1", "--", source, destination)
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("git clone failed: %v: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// locateSkillDir finds the directory holding the SKILL.md within root. The common
// case is a SKILL.md at the root; otherwise it searches one level deeper (a repo
// whose skill lives in a subdirectory) and requires exactly one match so the
// install target is unambiguous.
func locateSkillDir(root string) (string, error) {
	if fileExists(filepath.Join(root, skillFileName)) {
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
		if fileExists(filepath.Join(candidate, skillFileName)) {
			matches = append(matches, candidate)
		}
	}
	sort.Strings(matches)
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no %s found in source", skillFileName)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("source contains multiple skills (%d); install one at a time", len(matches))
	}
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

// isLocalPath reports whether source should be treated as a local filesystem
// path rather than a remote URL. URLs (scheme://… or scp-style host:path) and
// the common git shorthand are treated as remote.
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
	// scp-style git remote: user@host:path or host:path (no scheme). A Windows
	// drive path (C:\…) is local, so only treat host:path as remote when the
	// segment before ':' is not a single drive letter.
	if idx := strings.Index(source, ":"); idx > 0 {
		host := source[:idx]
		if strings.Contains(host, "@") {
			return false
		}
		if len(host) == 1 {
			return true // drive letter
		}
		if strings.Contains(host, ".") {
			return false // looks like a hostname
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

// validSkillName mirrors the install target safety rule: a skill name becomes a
// single directory component, so it must not contain path separators or traversal.
func validSkillName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return false
	}
	return name == filepath.Base(name)
}

func hashContent(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// skillHashDrift reports whether the on-disk SKILL.md no longer matches the
// lockfile hash recorded at install time. Missing lock hashes never count as
// drift (agents-only / unlocked skills). A locked skill whose SKILL.md cannot
// be read is treated as drifted — not as a clean match.
func skillHashDrift(skill Skill, lockHash string) bool {
	lockHash = strings.TrimSpace(lockHash)
	if lockHash == "" {
		return false
	}
	data, err := os.ReadFile(skill.Path)
	if err != nil {
		return true
	}
	return hashContent(data) != lockHash
}
