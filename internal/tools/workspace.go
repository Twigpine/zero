package tools

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Gitlawb/zero/internal/sandbox"
	"github.com/Gitlawb/zero/internal/workspaceindex"
)

var ignoredDirectories = map[string]bool{
	".git":         true,
	"node_modules": true,
	"dist":         true,
	"build":        true,
	".next":        true,
	".turbo":       true,
	"coverage":     true,
	".cache":       true,
	"tmp":          true,
	"temp":         true,
}

func normalizeWorkspaceRoot(workspaceRoot string) string {
	root, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return workspaceRoot
	}
	return root
}

func resolveWorkspacePath(workspaceRoot string, requestedPath string) (string, string, error) {
	if requestedPath == "" {
		requestedPath = "."
	}

	root, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return "", "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", err
	}

	target := requestedPath
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, target)
	}

	target, err = filepath.Abs(target)
	if err != nil {
		return "", "", err
	}
	target, err = filepath.EvalSymlinks(target)
	if err != nil {
		if hint := posixRootedPathHint(runtime.GOOS, requestedPath, root, err); hint != "" {
			return "", "", errors.New(hint)
		}
		return "", "", err
	}

	relative, err := filepath.Rel(root, target)
	if err != nil {
		return "", "", outsideWorkspaceError(requestedPath)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", "", outsideWorkspaceError(requestedPath)
	}
	if relative == "." {
		return target, ".", nil
	}
	return target, filepath.ToSlash(relative), nil
}

// posixRootedPathHint explains read misses whose requested path is
// POSIX-rooted. On Windows a leading "/" names no drive, so filepath.IsAbs is
// false and "/home/user/repo/file.go" joins onto the workspace root; the raw
// miss that follows ("GetFileAttributesEx <root>\home: ...") names a directory
// the model never meant and invites retrying the same wrong shape. Naming the
// host OS and the workspace root lets one retry correct the shape. The hint
// deliberately does not echo the requested path: echoing a POSIX fragment
// next to the workspace root trips the no-leak checks that confine
// out-of-workspace escapes, and tool wrappers that name the request add it
// themselves. Only the message changes; the path itself is never rewritten.
func posixRootedPathHint(goos, requestedPath, root string, err error) string {
	if goos != "windows" || !strings.HasPrefix(requestedPath, "/") || !errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	return fmt.Sprintf("path does not exist on this Windows host: a leading / carries no drive here, so it resolved against the workspace root %s. Use a workspace-relative path or a Windows absolute path", root)
}

func resolveWorkspaceTargetPath(workspaceRoot string, requestedPath string) (string, string, error) {
	if requestedPath == "" {
		requestedPath = "."
	}

	root, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return "", "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", err
	}

	target := requestedPath
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, target)
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return "", "", err
	}
	if err := recheckWorkspaceWriteTarget(root, target); err != nil {
		return "", "", err
	}

	existing := target
	missingSegments := []string{}
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		} else if os.IsNotExist(err) {
			parent := filepath.Dir(existing)
			if parent == existing {
				return "", "", err
			}
			missingSegments = append([]string{filepath.Base(existing)}, missingSegments...)
			existing = parent
			continue
		} else {
			return "", "", err
		}
	}

	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", "", err
	}
	for _, segment := range missingSegments {
		resolved = filepath.Join(resolved, segment)
	}

	relative, err := filepath.Rel(root, resolved)
	if err != nil {
		return "", "", outsideWorkspaceError(requestedPath)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", "", outsideWorkspaceError(requestedPath)
	}
	if relative == "." {
		return resolved, ".", nil
	}
	return resolved, filepath.ToSlash(relative), nil
}

func recheckWorkspaceWriteTarget(workspaceRoot string, requestedPath string) error {
	if requestedPath == "" {
		requestedPath = "."
	}

	root, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}

	target := requestedPath
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, target)
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return err
	}

	relative, err := filepath.Rel(root, target)
	if err != nil {
		return outsideWorkspaceError(requestedPath)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return outsideWorkspaceError(requestedPath)
	}
	if relative == "." {
		return nil
	}

	current := root
	for _, segment := range strings.Split(filepath.Clean(relative), string(filepath.Separator)) {
		if segment == "." || segment == "" {
			continue
		}

		current = filepath.Join(current, segment)
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			symlinkRelative, err := filepath.Rel(root, current)
			if err != nil {
				symlinkRelative = current
			}
			return fmt.Errorf("%s must not traverse symlink %s", requestedPath, filepath.ToSlash(symlinkRelative))
		}
	}

	return nil
}

func outsideWorkspaceError(requestedPath string) error {
	return fmt.Errorf("%s must stay inside the workspace", requestedPath)
}

func shouldSkipDirectory(name string) bool {
	return ignoredDirectories[name] || workspaceindex.ShouldSkipDir(name)
}

func shouldSkipWorkspaceFile(path string) bool {
	return workspaceindex.ShouldSkipFile(path)
}

// PathScope is the multi-root write scope shared with the sandbox engine.
// *sandbox.Scope satisfies it; nil means workspace-only (today's behavior).
// Roots()[0] must be the workspace root (sandbox.Scope guarantees this
// ordering); relative paths and error messages key off it.
type PathScope interface {
	Roots() []string
}

type readPathScope interface {
	ReadRoots() []string
}

// scopedRoots returns the ordered roots to try for an absolute path: the
// scope's roots when present, else just the workspace root. A non-nil scope
// must expose at least one root (the workspace root first, per PathScope); an
// empty Roots() is a contract block that fails closed with an error so the
// scoped helpers never silently accept a path with no root to validate against.
// Returning success there would resolve to an empty path and, e.g., run bash
// with Cmd.Dir == "" (the process cwd) instead of an allowed root.
func scopedRoots(workspaceRoot string, scope PathScope) ([]string, error) {
	if scope == nil {
		return []string{workspaceRoot}, nil
	}
	roots := scope.Roots()
	if len(roots) == 0 {
		return nil, fmt.Errorf("invalid path scope: no write roots configured")
	}
	return roots, nil
}

func scopedReadRoots(workspaceRoot string, scope PathScope) ([]string, error) {
	if scope == nil {
		return []string{workspaceRoot}, nil
	}
	if reader, ok := scope.(readPathScope); ok {
		roots := reader.ReadRoots()
		if len(roots) == 0 {
			return nil, fmt.Errorf("invalid path scope: no read roots configured")
		}
		return roots, nil
	}
	return scopedRoots(workspaceRoot, scope)
}

func resolveScopedReadPath(workspaceRoot string, scope PathScope, requestedPath string) (string, string, error) {
	// Spill files (truncated tool output saved under the per-uid temp dir) are
	// readable regardless of scope: the truncation notice tells the model to
	// read_file/grep them, which must actually work. resolveSpillReadPath
	// verifies containment after symlink resolution, so this cannot be used to
	// reach anything outside the spill dir.
	if spillPath, ok := resolveSpillReadPath(requestedPath); ok {
		return spillPath, spillPath, nil
	}
	if requestedPath == "" || !filepath.IsAbs(requestedPath) || scope == nil {
		return resolveWorkspacePath(workspaceRoot, requestedPath)
	}
	roots, err := scopedReadRoots(workspaceRoot, scope)
	if err != nil {
		return "", "", err
	}
	var firstErr error
	for index, root := range roots {
		candidate := sandbox.NormalizePrefixForRoot(requestedPath, root)
		absolute, relative, err := resolveWorkspacePath(root, candidate)
		if err == nil {
			if index > 0 {
				return absolute, absolute, nil
			}
			return absolute, relative, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return "", "", firstErr
}

// resolveScopedPath is resolveWorkspacePath generalized to a scope: relative
// paths resolve against the workspace root only; an absolute path resolves
// against the first root that contains it. The workspace root's error is
// returned when no root matches so messages stay stable. Prefix symlinks
// outside a root are resolved per root (macOS /var aliasing); symlinks INSIDE
// a root stay visible to the single-root checks, so a write target may resolve
// through a symlink only when its final location lies inside a DIFFERENT
// granted root — mirroring sandbox.Scope.validate's documented widening.
// When the matched root is an extra (non-workspace) root the second return
// value is the absolute path rather than a per-root-relative path; "relative
// to which root" is ambiguous for downstream consumers (ChangedFiles, cwd
// meta, display summaries) that document workspace-relative paths.
// When all roots deny, the workspace root's error is returned; unlike
// sandbox.Scope.validate this does not prefer traversal blocks — the
// engine layer reports those with full fidelity before tools run.
func resolveScopedPath(workspaceRoot string, scope PathScope, requestedPath string) (string, string, error) {
	if requestedPath == "" || !filepath.IsAbs(requestedPath) || scope == nil {
		return resolveWorkspacePath(workspaceRoot, requestedPath)
	}
	roots, err := scopedRoots(workspaceRoot, scope)
	if err != nil {
		return "", "", err
	}
	var firstErr error
	for index, root := range roots {
		// Normalize platform-level symlinks (e.g. macOS /var -> /private/var)
		// in the prefix outside this root only, leaving in-root components
		// verbatim for the single-root symlink checks.
		candidate := sandbox.NormalizePrefixForRoot(requestedPath, root)
		absolute, relative, err := resolveWorkspacePath(root, candidate)
		if err == nil {
			if index > 0 {
				// Extra-root matches report the absolute path: "relative to
				// which root" is ambiguous downstream (ChangedFiles, cwd
				// meta, display), and consumers document workspace-relative.
				return absolute, absolute, nil
			}
			return absolute, relative, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return "", "", firstErr
}

// resolveScopedTargetPath mirrors resolveWorkspaceTargetPath for write targets
// (the target may not exist yet) across all scope roots. When the matched root
// is an extra (non-workspace) root the second return value is the absolute
// path rather than a per-root-relative path; "relative to which root" is
// ambiguous for downstream consumers (ChangedFiles, cwd meta, display
// summaries) that document workspace-relative paths.
// When all roots deny, the workspace root's error is returned; unlike
// sandbox.Scope.validate this does not prefer traversal blocks — the
// engine layer reports those with full fidelity before tools run.
func resolveScopedTargetPath(workspaceRoot string, scope PathScope, requestedPath string) (string, string, error) {
	if requestedPath == "" || !filepath.IsAbs(requestedPath) || scope == nil {
		return resolveWorkspaceTargetPath(workspaceRoot, requestedPath)
	}
	roots, err := scopedRoots(workspaceRoot, scope)
	if err != nil {
		return "", "", err
	}
	var firstErr error
	for index, root := range roots {
		// Normalize platform-level symlinks (e.g. macOS /var -> /private/var)
		// in the prefix outside this root only; in-root components stay
		// verbatim so the per-segment write-target symlink checks apply.
		candidate := sandbox.NormalizePrefixForRoot(requestedPath, root)
		absolute, relative, err := resolveWorkspaceTargetPath(root, candidate)
		if err == nil {
			if index > 0 {
				// Extra-root matches report the absolute path: "relative to
				// which root" is ambiguous downstream (ChangedFiles, cwd
				// meta, display), and consumers document workspace-relative.
				return absolute, absolute, nil
			}
			return absolute, relative, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return "", "", firstErr
}

// recheckScopedWriteTarget mirrors recheckWorkspaceWriteTarget across roots.
// Prefix symlinks outside a root are resolved per root (macOS /var aliasing);
// symlinks INSIDE a root stay visible to the single-root checks, so a write
// target may resolve through a symlink only when its final location lies
// inside a DIFFERENT granted root — mirroring sandbox.Scope.validate's
// documented widening.
func recheckScopedWriteTarget(workspaceRoot string, scope PathScope, requestedPath string) error {
	if requestedPath == "" || !filepath.IsAbs(requestedPath) || scope == nil {
		return recheckWorkspaceWriteTarget(workspaceRoot, requestedPath)
	}
	roots, err := scopedRoots(workspaceRoot, scope)
	if err != nil {
		return err
	}
	var firstErr error
	for _, root := range roots {
		candidate := sandbox.NormalizePrefixForRoot(requestedPath, root)
		err := recheckWorkspaceWriteTarget(root, candidate)
		if err == nil {
			return nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
