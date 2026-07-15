package planmode

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Gitlawb/zero/internal/config"
)

// PlanDirName is the workspace-relative directory where /plan plan files live,
// mirroring the spec-draft convention under .zero (see specmode.SpecDirName).
const PlanDirName = ".zero/plans"

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
// session under the workspace .zero/plans directory, for display and for
// handing to an external editor process. It performs no filesystem access and
// gives no containment guarantee by itself: ReadPlan and WritePlan are the
// safe way to actually read or write plan content, since they resolve paths
// through os.Root and cannot be redirected outside the workspace even by a
// symlink planted between this call and theirs.
func PlanFilePath(workspaceRoot, sessionID string) (string, error) {
	root := strings.TrimSpace(workspaceRoot)
	if root == "" {
		return "", fmt.Errorf("workspace root is required")
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	return filepath.Join(absoluteRoot, planRelativePath(sessionID)), nil
}

// ReadPlan reads the plan file for a session. The bool reports whether a plan
// file exists; a missing file is not an error.
func ReadPlan(workspaceRoot, sessionID string) (string, bool, error) {
	root, err := openWorkspaceRoot(workspaceRoot)
	if err != nil {
		return "", false, err
	}
	defer root.Close()
	data, err := root.ReadFile(planRelativePath(sessionID))
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read plan file: %w", err)
	}
	return string(data), true, nil
}

// WritePlan writes (creating the directory as needed) the plan file for a
// session and returns its path.
func WritePlan(workspaceRoot, sessionID, content string) (string, error) {
	root, err := openWorkspaceRoot(workspaceRoot)
	if err != nil {
		return "", err
	}
	defer root.Close()

	dirRelPath := filepath.FromSlash(PlanDirName)
	if err := root.MkdirAll(dirRelPath, 0o700); err != nil {
		return "", fmt.Errorf("create plan directory: %w", err)
	}
	// MkdirAll's mode only applies at creation: it does not tighten an
	// already-existing, more permissive directory (e.g. one predating this
	// restriction, or created some other way). Chmod unconditionally so a
	// pre-existing 0755 directory is brought back to owner-only on every
	// write, matching the storage contract.
	if err := root.Chmod(dirRelPath, 0o700); err != nil {
		return "", fmt.Errorf("restrict plan directory permissions: %w", err)
	}
	fileRelPath := planRelativePath(sessionID)
	file, err := root.OpenFile(fileRelPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("write plan file: %w", err)
	}
	defer file.Close()
	// Same reasoning as the directory Chmod above: OpenFile's mode only
	// applies when it creates the file, so a pre-existing 0644 plan file
	// would otherwise stay group/other-readable.
	if err := root.Chmod(fileRelPath, 0o600); err != nil {
		return "", fmt.Errorf("restrict plan file permissions: %w", err)
	}
	if _, err := file.WriteString(strings.TrimRight(content, "\n") + "\n"); err != nil {
		return "", fmt.Errorf("write plan file: %w", err)
	}
	return PlanFilePath(workspaceRoot, sessionID)
}

// StageForEditor copies a session's current plan content (read safely via
// ReadPlan) into a fresh file outside the workspace, for handing to an
// external $EDITOR process launched by /plan open.
//
// Handing $EDITOR a path inside the workspace itself would leave a
// symlink-swap race: ReadPlan/WritePlan resolve descriptor-relative through
// os.Root and cannot be redirected, but the external editor process opens
// its argument path with its own ordinary (non-Root) I/O, so a sandboxed
// tool invocation could replace the plan file with a symlink between our
// protected write and the editor's open, causing the editor (which runs
// unsandboxed, under the real user) to follow it and edit an arbitrary
// user-writable target. The OS temp directory does not avoid this: the
// sandbox's default write scope explicitly includes it (see
// defaultTempWriteRootCandidates in internal/sandbox), so a sandboxed
// process could plant the same symlink there. config.UserConfigDir() is
// usually outside that default scope, but it honors XDG_CONFIG_HOME (on
// macOS explicitly here, on Linux via os.UserConfigDir itself), so a
// misconfigured or sandboxed-process environment pointing that at the
// workspace or the OS temp dir would put the staging directory right back in
// a default-writable root. editorStagingDirIsPrivate rejects that case
// instead of silently staging somewhere unsafe.
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
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", nil, fmt.Errorf("resolve plan editor staging directory: %w", err)
	}
	if !editorStagingDirIsPrivate(resolvedDir, workspaceRoot, os.TempDir()) {
		return "", nil, fmt.Errorf("plan editor staging directory %s resolves into a default sandbox-writable root (the workspace or the OS temp directory); check XDG_CONFIG_HOME", dir)
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
// user's $EDITOR) and writes its content back into the workspace via
// WritePlan, which is the safe, descriptor-relative path back through
// os.Root.
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

// openWorkspaceRoot opens the workspace directory as an os.Root, which the
// Go runtime resolves relative to using descriptor-relative (openat-style)
// operations: every subsequent Root method call re-walks the path from that
// descriptor and refuses to follow a symlink referencing a location outside
// it. That closes the check/use race a separate Lstat-then-open preflight
// would leave open (a symlink planted at .zero, .zero/plans, or the plan file
// itself between the check and the later open could otherwise redirect the
// read/write outside the workspace).
func openWorkspaceRoot(workspaceRoot string) (*os.Root, error) {
	root := strings.TrimSpace(workspaceRoot)
	if root == "" {
		return nil, fmt.Errorf("workspace root is required")
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("open workspace root: %w", err)
	}
	return r, nil
}

// planRelativePath returns the workspace-relative plan file path for a
// session. The session ID is slugified to a filesystem-safe alphabet (see
// slugify), so the result can never contain ".." or an absolute path.
func planRelativePath(sessionID string) string {
	return filepath.Join(filepath.FromSlash(PlanDirName), slugify(sessionID)+".md")
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
