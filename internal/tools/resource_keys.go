package tools

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// Resource-key prefixes used for future conflict detection. Keys are pure
// metadata in this PR — no locks or scheduling are applied.
//
// Unused prefixes (repository/pty/browser) are reserved for follow-up
// classifiers so PR6 conflict keys stay vocabulary-stable.
const (
	ResourceKeyFile       = "file:"
	ResourceKeyDirectory  = "directory:"
	ResourceKeyRepository = "repository:" // reserved
	ResourceKeyProcess    = "process:"
	ResourceKeyPTY        = "pty:"     // reserved
	ResourceKeyBrowser    = "browser:" // reserved
	ResourceKeyEndpoint   = "endpoint:"
	ResourceKeySession    = "session:"
	ResourceKeyWorkspace  = "workspace:"
)

// NormalizeResourcePath produces a deterministic path token for resource keys.
// It never touches the filesystem (no EvalSymlinks / Stat) so it cannot cause
// side effects or panic on missing paths. Empty input returns "".
//
// Rules:
//   - Trim space
//   - filepath.Clean
//   - Convert separators to '/'
//   - On Windows, lower-case for case-insensitive comparison
//   - Strip a leading "./"
//   - Never include credentials or query fragments (paths only)
func NormalizeResourcePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	// Reject URL-shaped values — those use endpoint: keys.
	if strings.Contains(path, "://") {
		return ""
	}
	cleaned := filepath.Clean(path)
	if cleaned == "." {
		return "."
	}
	normalized := filepath.ToSlash(cleaned)
	normalized = strings.TrimPrefix(normalized, "./")
	if runtime.GOOS == "windows" {
		normalized = strings.ToLower(normalized)
	}
	return normalized
}

// fileResourceKeys extracts a file: resource key from common path argument names.
// Missing or empty path returns nil (not an error); never panics.
func fileResourceKeys(args map[string]any) []string {
	path := firstStringArg(args, "path", "file", "file_path", "filepath", "filename", "target")
	normalized := NormalizeResourcePath(path)
	if normalized == "" {
		return nil
	}
	return []string{ResourceKeyFile + normalized}
}

// directoryResourceKeys extracts a directory: key from path/directory/cwd args.
func directoryResourceKeys(args map[string]any) []string {
	path := firstStringArg(args, "path", "directory", "dir", "cwd", "workdir")
	normalized := NormalizeResourcePath(path)
	if normalized == "" {
		return nil
	}
	return []string{ResourceKeyDirectory + normalized}
}

// endpointResourceKeys extracts an endpoint: key from url/endpoint args.
// Host only is kept when a full URL is provided (no secrets/query/userinfo).
func endpointResourceKeys(args map[string]any) []string {
	raw := firstStringArg(args, "url", "endpoint", "uri")
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	host := resourceHost(raw)
	if host == "" {
		return nil
	}
	return []string{ResourceKeyEndpoint + host}
}

// sessionResourceKeys extracts a session: key from session / session_id args.
// Numeric session ids (JSON numbers) are coerced to strings.
func sessionResourceKeys(args map[string]any) []string {
	return scopedIDResourceKeys(ResourceKeySession, args, "session", "session_id", "sessionId")
}

// processResourceKeys extracts process: keys for retained process / terminal
// tools. Accepts session, session_id, pid, and numeric JSON values so
// write_stdin (integer session_id) and terminal_session (string session)
// both produce keys.
func processResourceKeys(args map[string]any) []string {
	return scopedIDResourceKeys(ResourceKeyProcess, args,
		"session", "session_id", "sessionId", "process_id", "pid", "id")
}

// scopedIDResourceKeys is the shared extract/trim/bound/prefix path for
// session- and process-scoped resource keys.
func scopedIDResourceKeys(prefix string, args map[string]any, keys ...string) []string {
	id := strings.TrimSpace(firstStringArg(args, keys...))
	if id == "" {
		return nil
	}
	// Never put free-form content in keys — IDs only, bounded length.
	if len(id) > 128 {
		id = id[:128]
	}
	return []string{prefix + id}
}

// workspaceResourceKeys returns a single workspace: root marker when the call
// affects the whole workspace (e.g. glob without a path root).
func workspaceResourceKeys(_ map[string]any) []string {
	return []string{ResourceKeyWorkspace + "root"}
}

// scopedScanResourceKeys returns conflict keys for directory-scoped scan tools
// (glob, grep). Workspace-wide scans (missing / empty / ".") return nil so
// they do not force a false conflict with every other concurrent scan —
// ThreadSafe is the concurrency gate; keys refine same-directory collisions.
// A non-default path/cwd yields a single directory: key.
func scopedScanResourceKeys(args map[string]any) []string {
	path := firstStringArg(args, "path", "cwd", "dir", "directory")
	normalized := NormalizeResourcePath(path)
	if normalized == "" || normalized == "." {
		return nil
	}
	return []string{ResourceKeyDirectory + normalized}
}

// multiFileResourceKeys collects path and paths[] arguments into file: keys.
func multiFileResourceKeys(args map[string]any) []string {
	var keys []string
	keys = append(keys, fileResourceKeys(args)...)
	if raw, ok := args["paths"]; ok {
		switch typed := raw.(type) {
		case []string:
			for _, p := range typed {
				if n := NormalizeResourcePath(p); n != "" {
					keys = append(keys, ResourceKeyFile+n)
				}
			}
		case []any:
			for _, item := range typed {
				if s, ok := item.(string); ok {
					if n := NormalizeResourcePath(s); n != "" {
						keys = append(keys, ResourceKeyFile+n)
					}
				}
			}
		}
	}
	return uniqueKeys(keys)
}

// applyPatchResourceKeys derives conflict keys for apply_patch: the cwd as a
// directory key, optional path/paths args, plus any +++/--- file paths
// parseable from the unified diff. Relative diff paths are joined under cwd
// so the same file under different cwd values does not collide (e.g. cwd=pkg
// + +++ b/new.go → file:pkg/new.go). Falls back to workspace:root when nothing
// is available so a patch call is never "keyless" for the future conflict planner.
func applyPatchResourceKeys(args map[string]any) []string {
	var keys []string
	keys = append(keys, directoryResourceKeys(args)...)
	keys = append(keys, multiFileResourceKeys(args)...)
	cwd := firstStringArg(args, "cwd", "workdir")
	patch := firstStringArg(args, "patch", "diff")
	for _, p := range pathsFromApplyPatch(patch) {
		joined := joinUnderResourceCwd(cwd, p)
		if n := NormalizeResourcePath(joined); n != "" {
			keys = append(keys, ResourceKeyFile+n)
		}
	}
	keys = uniqueKeys(keys)
	if len(keys) == 0 {
		return workspaceResourceKeys(args)
	}
	return keys
}

func pathsFromApplyPatch(patch string) []string {
	if !isStructuredPatch(patch) {
		return pathsFromUnifiedDiff(patch)
	}
	operations, err := parseStructuredPatch(patch)
	if err != nil {
		return nil
	}
	return structuredPatchOperationPaths(operations)
}

// joinUnderResourceCwd joins a relative path under cwd for resource keys only.
// Absolute paths are left unchanged. Empty cwd or "." leaves rel as-is.
// Never touches the filesystem.
func joinUnderResourceCwd(cwd, rel string) string {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return ""
	}
	// Absolute (Unix or Windows) paths are already workspace-rooted or outside;
	// do not re-prefix.
	if filepath.IsAbs(rel) {
		return rel
	}
	cwd = strings.TrimSpace(cwd)
	if cwd == "" || cwd == "." {
		return rel
	}
	return filepath.ToSlash(filepath.Join(cwd, rel))
}

// pathsFromUnifiedDiff extracts a/ and b/ file paths from unified-diff headers.
// Malformed input returns nil; never panics.
func pathsFromUnifiedDiff(patch string) []string {
	if strings.TrimSpace(patch) == "" {
		return nil
	}
	var out []string
	for _, line := range strings.Split(patch, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "+++ ") || strings.HasPrefix(line, "--- "):
			rest := strings.TrimSpace(line[4:])
			// Drop trailing tab timestamp if present.
			if i := strings.IndexByte(rest, '\t'); i >= 0 {
				rest = rest[:i]
			}
			if rest == "/dev/null" || rest == "" {
				continue
			}
			// Strip a/ or b/ prefix common in git diffs.
			if len(rest) > 2 && (rest[0] == 'a' || rest[0] == 'b') && rest[1] == '/' {
				rest = rest[2:]
			}
			out = append(out, rest)
		}
	}
	return out
}

// firstStringArg returns the first present argument as a string. Numeric JSON
// values (float64/int from encoding/json) are coerced so tools that declare
// integer session ids still yield resource keys.
func firstStringArg(args map[string]any, keys ...string) string {
	if args == nil {
		return ""
	}
	for _, key := range keys {
		value, ok := args[key]
		if !ok || value == nil {
			continue
		}
		switch typed := value.(type) {
		case string:
			return typed
		case float64:
			// JSON numbers decode as float64. Prefer integer form when exact.
			if typed == float64(int64(typed)) {
				return fmt.Sprintf("%d", int64(typed))
			}
			return fmt.Sprintf("%v", typed)
		case float32:
			return fmt.Sprintf("%v", typed)
		case int:
			return fmt.Sprintf("%d", typed)
		case int64:
			return fmt.Sprintf("%d", typed)
		case int32:
			return fmt.Sprintf("%d", typed)
		case jsonNumber:
			return typed.String()
		}
	}
	return ""
}

// jsonNumber matches encoding/json.Number without importing encoding/json here
// for a one-line type assert (tests may inject it).
type jsonNumber interface {
	String() string
}

// resourceHost returns a lower-cased host for endpoint keys, stripping
// userinfo, path, query, and fragment. Returns "" when not parseable safely.
//
// Order matters: path/query/fragment are removed BEFORE userinfo so a query
// value containing "@" cannot steal the authority (e.g.
// https://api.example.com/v1?token=@secret → endpoint:api.example.com).
// host:port is preserved so two services on different ports do not collide.
func resourceHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// Strip scheme.
	if idx := strings.Index(raw, "://"); idx >= 0 {
		raw = raw[idx+3:]
	}
	// Isolate authority: strip path/query/fragment first.
	if slash := strings.IndexAny(raw, "/?#"); slash >= 0 {
		raw = raw[:slash]
	}
	// Strip userinfo from the authority only.
	if at := strings.LastIndex(raw, "@"); at >= 0 {
		raw = raw[at+1:]
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.Contains(raw, "@") {
		return ""
	}
	return strings.ToLower(raw)
}

func uniqueKeys(keys []string) []string {
	if len(keys) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(keys))
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if k == "" {
			continue
		}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	return out
}
