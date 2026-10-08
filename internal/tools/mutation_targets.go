package tools

import "path/filepath"

// MutationTargets returns the workspace-relative paths a tool call will write to,
// so the session layer can snapshot their before-state for safe rewind. It is a
// pure helper (no I/O beyond path resolution) and returns nil for read-only tools
// and for bash (whose affected paths are not knowable before execution).
func MutationTargets(workspaceRoot string, name string, args map[string]any) []string {
	switch name {
	case "write_file", "edit_file":
		// Resolve the path via the SAME alias key list write_file/edit_file use,
		// so a checkpoint is captured even when the model writes via an alias key
		// (e.g. {"file": ...}); otherwise /rewind could not undo the write.
		path, err := aliasedStringArg(args, []string{"path", "file", "file_path", "filename"}, "", true, false)
		if err != nil {
			return nil
		}
		_, relative, err := resolveWorkspaceTargetPath(workspaceRoot, path)
		if err != nil {
			return nil
		}
		return []string{relative}
	case "apply_patch":
		// Resolve the patch via the SAME alias key list apply_patch uses.
		patch, err := aliasedStringArg(args, []string{"patch", "diff"}, "", true, false)
		if err != nil {
			return nil
		}
		// Mirror apply_patch's cwd handling so the returned targets are
		// WORKSPACE-relative (cwd-prefixed) when cwd != ".". Without this, a
		// patch applied under a subdir would snapshot the wrong rewind path.
		cwd, err := stringArg(args, "cwd", ".", false)
		if err != nil {
			return nil
		}
		applyRoot, relativeRoot, err := resolveWorkspacePath(workspaceRoot, cwd)
		if err != nil {
			return nil
		}
		if isStructuredPatch(patch) {
			operations, err := parseStructuredPatch(patch)
			if err != nil {
				return nil
			}
			paths := structuredPatchOperationPaths(operations)
			for _, path := range paths {
				if _, _, err := resolveWorkspaceTargetPath(applyRoot, path); err != nil {
					return nil
				}
			}
			return prefixPatchPaths(relativeRoot, paths)
		}
		// Enforce the same workspace confinement apply_patch applies (against the
		// resolved apply dir), so a patch with a traversal path (../x) never yields
		// an out-of-workspace target.
		if err := validatePatchPaths(applyRoot, patch); err != nil {
			return nil
		}
		paths := changedFilesFromPatch(relativeRoot, patch)
		if len(paths) == 0 {
			return nil
		}
		return paths
	default:
		return nil
	}
}

func prefixPatchPaths(relativeRoot string, paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	var out []string
	for _, path := range paths {
		workspacePath := path
		if relativeRoot != "" && relativeRoot != "." {
			workspacePath = filepath.ToSlash(filepath.Join(relativeRoot, path))
		}
		if !seen[workspacePath] {
			seen[workspacePath] = true
			out = append(out, workspacePath)
		}
	}
	return out
}
