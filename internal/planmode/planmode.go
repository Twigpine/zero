package planmode

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// PlanFilePath returns the deterministic plan file path for a session under the
// workspace .zero/plans directory. The session ID is slugified so the file name
// is stable across re-entering plan mode within the same session.
func PlanFilePath(workspaceRoot, sessionID string) (string, error) {
	root := strings.TrimSpace(workspaceRoot)
	if root == "" {
		return "", fmt.Errorf("workspace root is required")
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	id := slugify(sessionID)
	relativePath := filepath.ToSlash(filepath.Join(PlanDirName, id+".md"))
	path := filepath.Join(absoluteRoot, filepath.FromSlash(relativePath))
	if err := ensurePlanPathContained(absoluteRoot, path); err != nil {
		return "", err
	}
	return path, nil
}

// ReadPlan reads the plan file for a session. The bool reports whether a plan
// file exists; a missing file is not an error.
func ReadPlan(workspaceRoot, sessionID string) (string, bool, error) {
	path, err := PlanFilePath(workspaceRoot, sessionID)
	if err != nil {
		return "", false, err
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
// session and returns its path.
func WritePlan(workspaceRoot, sessionID, content string) (string, error) {
	path, err := PlanFilePath(workspaceRoot, sessionID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create plan directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(strings.TrimRight(content, "\n")+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write plan file: %w", err)
	}
	return path, nil
}

func ensurePlanPathContained(workspaceRoot, path string) error {
	relative, err := filepath.Rel(filepath.Clean(workspaceRoot), filepath.Clean(path))
	if err != nil {
		return fmt.Errorf("resolve plan file path: %w", err)
	}
	if relative == "." || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("plan file path escapes workspace root")
	}
	return rejectSymlinkEscape(workspaceRoot, path)
}

// rejectSymlinkEscape walks path's ancestors up to (but not including)
// workspaceRoot and refuses to proceed if any existing component - the plan
// file itself, the plans directory, or .zero - is a symlink. The lexical
// filepath.Rel check above only guards against ".." traversal in the
// constructed path; a pre-planted symlink at any of those locations would
// still let os.MkdirAll/os.WriteFile/os.ReadFile follow it outside the
// workspace despite the path string looking contained.
func rejectSymlinkEscape(workspaceRoot, path string) error {
	root := filepath.Clean(workspaceRoot)
	for current := filepath.Clean(path); current != root; current = filepath.Dir(current) {
		parent := filepath.Dir(current)
		if parent == current {
			// Reached the filesystem root without hitting workspaceRoot; the
			// filepath.Rel check above already rejects this case.
			return nil
		}
		info, err := os.Lstat(current)
		switch {
		case err == nil && info.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("plan file path %s contains a symlink", current)
		case err != nil && !os.IsNotExist(err):
			return fmt.Errorf("check plan file path: %w", err)
		}
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
