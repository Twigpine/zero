package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Twigpine/zero/internal/sandbox"
	"github.com/Twigpine/zero/internal/workspaceindex"
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
	return resolveScopedReadPathForGOOS(runtimeGOOS(), workspaceRoot, scope, requestedPath)
}

// resolveScopedReadPathForGOOS is resolveScopedReadPath with the host OS
// injected, so the Windows-only missing-POSIX-path hint can be checked for
// every platform from any one of them (the same seam as detectShellRuntime).
func resolveScopedReadPathForGOOS(goos string, workspaceRoot string, scope PathScope, requestedPath string) (string, string, error) {
	// Spill files (truncated tool output saved under the per-uid temp dir) are
	// readable regardless of scope: the truncation notice tells the model to
	// read_file/grep them, which must actually work. resolveSpillReadPath
	// verifies containment after symlink resolution, so this cannot be used to
	// reach anything outside the spill dir.
	if spillPath, ok := resolveSpillReadPath(requestedPath); ok {
		return spillPath, spillPath, nil
	}
	if requestedPath == "" || !filepath.IsAbs(requestedPath) || scope == nil {
		absolute, relative, err := resolveWorkspacePath(workspaceRoot, requestedPath)
		if err != nil {
			// The hint only belongs on this branch: the scoped loop below runs
			// only when filepath.IsAbs(requestedPath) holds, which a
			// POSIX-looking path never does on Windows. Confinement errors are
			// not missing-path errors, so the annotator leaves them unchanged.
			err = annotateMissingPosixPathError(goos, workspaceRoot, requestedPath, err)
		}
		return absolute, relative, err
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

// errWriteRootSubstituted reports that the granted root directory changed
// identity between the pre-open stat and the descriptor open, so the handle may
// point outside the validated boundary. The operation fails closed.
var errWriteRootSubstituted = errors.New("write root changed identity between validation and open")

// writeRootBeforeOpen is a deterministic test hook. Production leaves it nil;
// tests use it to substitute the root path after its identity is captured but
// before os.OpenRoot resolves it, exercising the same check-to-use window a
// concurrent attacker would race.
var writeRootBeforeOpen func(path string)

// openScopedWriteRoot opens the granted write root that contains absolutePath
// and returns a descriptor-bound handle plus the path relative to it. The
// caller closes the handle.
//
// absolutePath is expected to be symlink-resolved, as resolveScopedPath and
// resolveScopedTargetPath return it. Each configured root is resolved before
// comparison so a workspace that legitimately sits under a platform alias
// (macOS /var -> /private/var, Windows 8.3 short names) still matches. Every
// mutation is then performed relative to the returned handle, so no component
// above the target can be swapped for an escaping link between the pathname
// check and the open.
func openScopedWriteRoot(workspaceRoot string, scope PathScope, absolutePath string) (*os.Root, string, error) {
	roots, err := scopedRoots(workspaceRoot, scope)
	if err != nil {
		return nil, "", err
	}
	var firstErr error
	for _, configuredRoot := range roots {
		resolvedRoot, err := filepath.Abs(configuredRoot)
		if err == nil {
			resolvedRoot, err = filepath.EvalSymlinks(resolvedRoot)
		}
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		// Capture the root directory identity BEFORE opening the handle.
		// os.OpenRoot resolves every component again, so replacing the root path
		// between this stat and the open (a symlink or directory swap) would bind
		// the returned descriptor to a different, possibly external directory.
		// Comparing identities after the open closes that check-to-use window.
		expectedStat, err := os.Stat(resolvedRoot)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		candidate := sandbox.NormalizePrefixForRoot(absolutePath, resolvedRoot)
		relativePath, err := filepath.Rel(resolvedRoot, candidate)
		if err != nil || relativePath == ".." || strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) || filepath.IsAbs(relativePath) {
			continue
		}
		if writeRootBeforeOpen != nil {
			writeRootBeforeOpen(resolvedRoot)
		}
		root, err := os.OpenRoot(resolvedRoot)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		actualStat, err := root.Stat(".")
		if err != nil {
			_ = root.Close()
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !os.SameFile(expectedStat, actualStat) {
			_ = root.Close()
			if firstErr == nil {
				firstErr = fmt.Errorf("%w: %s", errWriteRootSubstituted, resolvedRoot)
			}
			continue
		}
		return root, relativePath, nil
	}
	if firstErr != nil {
		return nil, "", firstErr
	}
	return nil, "", fmt.Errorf("%s must stay inside the configured write roots", absolutePath)
}
