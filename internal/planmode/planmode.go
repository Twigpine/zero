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

	if err := root.MkdirAll(filepath.FromSlash(PlanDirName), 0o700); err != nil {
		return "", fmt.Errorf("create plan directory: %w", err)
	}
	file, err := root.OpenFile(planRelativePath(sessionID), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("write plan file: %w", err)
	}
	defer file.Close()
	if _, err := file.WriteString(strings.TrimRight(content, "\n") + "\n"); err != nil {
		return "", fmt.Errorf("write plan file: %w", err)
	}
	return PlanFilePath(workspaceRoot, sessionID)
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
