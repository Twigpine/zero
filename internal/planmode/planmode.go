package planmode

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/config"
)

// PlanDirName is the config-relative directory (under UserConfigDir) where
// durable /plan files live. Plans are kept outside the workspace so the
// auto-allowed, read-only update_plan tool can persist without a write grant
// and without mutating the workspace.
const PlanDirName = "zero/plans"

// DraftSystemPrompt is the system prompt the TUI runs while /plan mode is active
// on the current session. It is read-only: the agent inspects the workspace and
// shapes the plan, but must not mutate anything until plan mode is exited.
const DraftSystemPrompt = `Plan mode is active on this session.

You are planning an implementation, not changing files.

Use read-only tools to inspect the workspace. You may use ask_user only when a
decision is genuinely blocking and cannot be resolved from the workspace or a
reasonable safe assumption.

Do not write files, edit files, apply patches, run shell commands, spawn
specialists, or implement the requested change while in plan mode.

Capture the plan with update_plan as you work. When the user is ready to
implement, they exit plan mode and you continue normally.

The plan should converge on one concrete approach. Do not leave unresolved
choices such as "Option A" and "Option B". If something remains uncertain, make
the safest reasonable assumption and state it clearly.`

// PlanFilePath returns the deterministic, absolute plan file path for a
// session under the per-user config plans directory, scoped by workspace so
// two workspaces never share a plan file. It performs no filesystem access;
// ReadPlan and WritePlan are the safe way to actually read or write plan
// content.
func PlanFilePath(workspaceRoot, sessionID string) (string, error) {
	base, absWorkspace, err := planStorageBase(workspaceRoot)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, slugify(absWorkspace), slugify(sessionID)+".md"), nil
}

// ReadPlan reads the plan file for a session. The bool reports whether a plan
// file exists; a missing file is not an error.
func ReadPlan(workspaceRoot, sessionID string) (string, bool, error) {
	path, err := PlanFilePath(workspaceRoot, sessionID)
	if err != nil {
		return "", false, err
	}
	if err := ensurePlanPathContained(workspaceRoot, path); err != nil {
		return "", false, err
	}
	// Refuse a symlinked plan file so a planted link cannot redirect the read
	// to an arbitrary target.
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", false, fmt.Errorf("plan file %s is a symlink; refusing to read through it", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read plan file: %w", err)
	}
	return string(data), true, nil
}

// WritePlan writes (creating the directory as needed) the plan file for a
// session and returns its path. The file is stored under the user config
// directory, never inside the workspace, so an auto-allowed read-only tool
// can persist without a workspace write grant.
func WritePlan(workspaceRoot, sessionID, content string) (string, error) {
	path, err := PlanFilePath(workspaceRoot, sessionID)
	if err != nil {
		return "", err
	}
	if err := ensurePlanPathContained(workspaceRoot, path); err != nil {
		return "", err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create plan directory: %w", err)
	}
	// MkdirAll's mode only applies at creation: it does not tighten an
	// already-existing, more permissive directory (e.g. one predating this
	// restriction, or created some other way). Chmod unconditionally so a
	// pre-existing 0755 directory is brought back to owner-only on every
	// write, matching the storage contract.
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("restrict plan directory permissions: %w", err)
	}
	// Re-check containment after creation: MkdirAll follows intermediate
	// symlinks, so a planted link under the config plans root could otherwise
	// land the durable file inside the workspace or elsewhere.
	if err := ensurePlanPathContained(workspaceRoot, path); err != nil {
		return "", err
	}
	// Refuse a symlinked plan file. A `<session>.md -> victim` planted during
	// an earlier run would otherwise turn a plan write into an overwrite of
	// an arbitrary user-writable target.
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("plan file %s is a symlink; refusing to write through it", path)
	}
	// Write an owner-only temporary sibling and rename it into place: a
	// disk-full failure, short write, or interruption must never leave the
	// durable plan empty or partial. The random suffix plus O_EXCL means a
	// colliding or pre-planted path is refused, and the rename target was
	// verified above not to be a symlink (rename replaces the name itself).
	tmpPath := fmt.Sprintf("%s.tmp-%d-%d", path, os.Getpid(), time.Now().UnixNano())
	file, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("write plan file: %w", err)
	}
	if _, err := file.WriteString(strings.TrimRight(content, "\n") + "\n"); err != nil {
		file.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("write plan file: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("write plan file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("replace plan file: %w", err)
	}
	return path, nil
}

// StageForEditor copies a session's current plan content (read safely via
// ReadPlan) into a fresh file outside the workspace, for handing to an
// external $EDITOR process launched by /plan open.
//
// Handing $EDITOR a path at the durable plan location would leave a
// symlink-swap race between our protected write and the editor's open. The
// OS temp directory does not avoid this either: the sandbox's default write
// scope explicitly includes it (see defaultTempWriteRootCandidates in
// internal/sandbox), so a sandboxed process could plant the same symlink
// there. config.UserConfigDir() is usually outside that default scope, but
// it honors XDG_CONFIG_HOME (on macOS explicitly here, on Linux via
// os.UserConfigDir itself), so a misconfigured or sandboxed-process
// environment pointing that at the workspace or the OS temp dir would put
// the staging directory right back in a default-writable root.
// editorStagingDirIsPrivate rejects that case instead of silently staging
// somewhere unsafe.
//
// Two more layers close the remaining gap even when the directory itself is
// private: the filename includes a random, per-invocation suffix (os.CreateTemp)
// so a sandboxed process can't pre-plant a symlink at a path it can't predict,
// and CreateTemp opens with O_EXCL, so even a guessed or colliding path is
// refused rather than followed if something is already there. The random
// suffix also means two Zero instances editing the same resumed session no
// longer collide on the same staged file.
func StageForEditor(workspaceRoot, sessionID string) (stagedPath string, cleanup func(), err error) {
	content, _, err := ReadPlan(workspaceRoot, sessionID)
	if err != nil {
		return "", nil, err
	}
	dir, err := editorStagingDir()
	if err != nil {
		return "", nil, err
	}
	// Create the directory before judging it, then judge (and use) its
	// PHYSICAL path: a lexical check would pass an XDG_CONFIG_HOME that is
	// itself a symlink into the workspace or the OS temp directory, while
	// MkdirAll/CreateTemp followed the link and staged the file somewhere a
	// sandboxed process can write. Resolving after MkdirAll also covers a
	// pre-existing staging directory that was replaced with a symlink, and
	// anchoring the staging on the resolved path means the file is created
	// where it was checked, not wherever the link points next.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, fmt.Errorf("create plan editor staging directory: %w", err)
	}
	// MkdirAll's mode only applies at creation: it does not tighten an
	// already-existing, more permissive directory (e.g. one predating this
	// restriction). Chmod unconditionally, matching WritePlan's plan
	// directory handling, so a pre-existing 0755 staging directory can't
	// leave a closed staged file visible to another local user before the
	// editor reopens it.
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", nil, fmt.Errorf("restrict plan editor staging directory permissions: %w", err)
	}
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", nil, fmt.Errorf("resolve plan editor staging directory: %w", err)
	}
	if !editorStagingDirIsPrivate(resolvedDir, workspaceRoot, os.TempDir()) {
		return "", nil, fmt.Errorf("plan editor staging directory %s resolves into a default sandbox-writable root (the workspace or the OS temp directory); check XDG_CONFIG_HOME", dir)
	}
	// Verify the resolved directory after chmod: refuse anything that is not
	// a plain directory or that is still group/world-writable. A pre-existing
	// sticky or ACL-permissive directory that chmod could not fully lock down
	// must not host a closed staged file the unsandboxed editor will reopen.
	if err := verifyPrivateDirectory(resolvedDir); err != nil {
		return "", nil, fmt.Errorf("plan editor staging directory: %w", err)
	}
	return stageContentForEditor(resolvedDir, sessionID, content)
}

// stageContentForEditor creates a fresh, uniquely-named file under dir
// holding content, for StageForEditor to hand to $EDITOR. Split out from
// StageForEditor so the staging mechanics (CreateTemp, O_EXCL) are testable
// against an arbitrary directory without needing to fake config.UserConfigDir
// or XDG_CONFIG_HOME; the privacy check above is StageForEditor's job, not
// this function's.
func stageContentForEditor(dir, sessionID, content string) (stagedPath string, cleanup func(), err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, fmt.Errorf("create plan editor staging directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", nil, fmt.Errorf("restrict plan editor staging directory permissions: %w", err)
	}
	file, err := os.CreateTemp(dir, slugify(sessionID)+"-*.md")
	if err != nil {
		return "", nil, fmt.Errorf("stage plan file for editor: %w", err)
	}
	path := file.Name()
	if _, err := file.WriteString(strings.TrimRight(content, "\n") + "\n"); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", nil, fmt.Errorf("stage plan file for editor: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", nil, fmt.Errorf("stage plan file for editor: %w", err)
	}
	return path, func() { _ = os.Remove(path) }, nil
}

// editorStagingDirIsPrivate reports whether dir avoids the sandbox's default
// writable roots (tempDir, normally os.TempDir(), and the workspace itself),
// which are writable from inside the sandbox by default regardless of any
// extra grant. All three paths are compared in physical form: dir or either
// root may be reached through symlinks (an XDG_CONFIG_HOME symlinked into
// the workspace, macOS's /var -> /private/var), and a lexical comparison of
// unlike spellings would wave a staging directory through a boundary it
// actually sits inside. tempDir is a parameter so tests can exercise the
// symlink cases without needing to plant links outside the real temp dir.
func editorStagingDirIsPrivate(dir, workspaceRoot, tempDir string) bool {
	dir = physicalPath(dir)
	if isUnderOrEqual(dir, physicalPath(tempDir)) {
		return false
	}
	if absRoot, err := filepath.Abs(workspaceRoot); err == nil && isUnderOrEqual(dir, physicalPath(absRoot)) {
		return false
	}
	return true
}

// verifyPrivateDirectory reports an error when path is not a plain directory
// or is still group/world-writable after the caller tightened it. Symlinks
// are rejected via Lstat so a TOCTOU swap of the directory for a link cannot
// host a staged file that $EDITOR will follow. The permission-bit check is
// skipped on Windows: NTFS reports a directory's POSIX mode via ACLs rather
// than the bits os.Chmod sets, so it does not reflect what os.Chmod(0o700)
// actually restricted (see the same rationale on the file-mode check in
// TestWritePlanUsesRestrictivePermissions) — containment there relies on the
// path checks in editorStagingDirIsPrivate instead.
func verifyPrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to stage through it", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("%s is group/world-writable (mode %o) after restriction", path, perm)
	}
	return nil
}

// physicalPath resolves symlinks best-effort. A path that does not exist yet
// is resolved through its deepest existing ancestor with the remainder
// rejoined, so a not-yet-created staging directory still compares in the
// same physical spelling as the (existing, resolved) roots: without this,
// macOS's /var vs /private/var and Windows's 8.3 short names (RUNNER~1)
// would make the containment comparison silently miss.
func physicalPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	cleaned := filepath.Clean(path)
	parent := filepath.Dir(cleaned)
	if parent == cleaned {
		// Reached a filesystem root that itself cannot be resolved.
		return cleaned
	}
	return filepath.Join(physicalPath(parent), filepath.Base(cleaned))
}

// isUnderOrEqual reports whether path is root itself or a descendant of it.
func isUnderOrEqual(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// CommitStagedEdit reads a file staged by StageForEditor (now edited by the
// user's $EDITOR) and writes its content back into the durable plan store
// via WritePlan.
func CommitStagedEdit(workspaceRoot, sessionID, stagedPath string) error {
	data, err := os.ReadFile(stagedPath)
	if err != nil {
		return fmt.Errorf("read staged plan file: %w", err)
	}
	_, err = WritePlan(workspaceRoot, sessionID, string(data))
	return err
}

// editorStagingDir is where plan files are staged for external $EDITOR
// access. See StageForEditor for why this location, not the OS temp
// directory, is what actually closes the containment race.
func editorStagingDir() (string, error) {
	dir, err := config.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve editor staging directory: %w", err)
	}
	return filepath.Join(dir, "zero", "plan-edit"), nil
}

// planStorageBase returns the absolute user-config plans root and the
// absolute workspace path used to scope per-workspace plan files.
func planStorageBase(workspaceRoot string) (base string, absWorkspace string, err error) {
	root := strings.TrimSpace(workspaceRoot)
	if root == "" {
		return "", "", fmt.Errorf("workspace root is required")
	}
	absWorkspace, err = filepath.Abs(root)
	if err != nil {
		return "", "", fmt.Errorf("resolve workspace root: %w", err)
	}
	cfg, err := config.UserConfigDir()
	if err != nil {
		return "", "", fmt.Errorf("resolve plan storage directory: %w", err)
	}
	return filepath.Join(cfg, filepath.FromSlash(PlanDirName)), absWorkspace, nil
}

// ensurePlanPathContained verifies that path stays under the config plans
// root and does not resolve into the workspace. A mis-set XDG_CONFIG_HOME
// pointing at the workspace would otherwise turn every update_plan
// persistence into a silent workspace write, which is the gap this storage
// layout exists to close.
func ensurePlanPathContained(workspaceRoot, path string) error {
	base, absWorkspace, err := planStorageBase(workspaceRoot)
	if err != nil {
		return err
	}
	physPath := physicalPath(path)
	physBase := physicalPath(base)
	if !isUnderOrEqual(physPath, physBase) {
		return fmt.Errorf("plan path %s escapes plan storage root %s", path, base)
	}
	if isUnderOrEqual(physPath, physicalPath(absWorkspace)) {
		return fmt.Errorf("plan storage %s resolves into the workspace; check XDG_CONFIG_HOME", path)
	}
	return nil
}

// slugify turns an arbitrary session identifier into a filesystem-safe slug.
func slugify(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		// A stable fallback, not a per-call timestamp: PlanFilePath is called
		// independently from several sites (planEnterText, planText,
		// openPlanInEditor) before a session ID may exist, and they must all
		// resolve to the same file rather than a fresh one each time.
		id = "plan"
	}
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(id) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case r == '-' || r == '_' || r == '/':
			if !prevDash && b.Len() > 0 {
				b.WriteRune('-')
				prevDash = true
			}
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteRune('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "plan"
	}
	return out
}
