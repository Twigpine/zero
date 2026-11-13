package agentsessions

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Env is the environment agentsessions resolves store roots against. Tests
// supply a synthetic one; production uses OSEnv. Keeping this behind a struct
// means no test has to mutate os.Setenv (which the repo's own suite is
// sensitive to) to exercise the redirect variables.
type Env struct {
	Home string
	// Getenv resolves the redirect variables below. Nil behaves as "unset".
	Getenv func(string) string
}

// OSEnv is the real environment.
func OSEnv() Env {
	home, _ := os.UserHomeDir()
	return Env{Home: home, Getenv: os.Getenv}
}

func (env Env) lookup(name string) string {
	if env.Getenv == nil {
		return ""
	}
	return strings.TrimSpace(env.Getenv(name))
}

// underHome joins parts onto the home directory, returning "" when home is
// unknown so callers degrade to "no store" rather than probing a relative path
// off the process working directory.
func (env Env) underHome(parts ...string) string {
	home := strings.TrimSpace(env.Home)
	if home == "" {
		return ""
	}
	return filepath.Join(append([]string{home}, parts...)...)
}

// Each of these honours the agent's documented redirect variable rather than
// hardcoding ~. A user with CLAUDE_CONFIG_DIR or CODEX_HOME set has their
// transcripts somewhere else entirely, and hardcoding would silently discover
// nothing while looking like it worked.

func claudeCodeRoot(env Env) string {
	if dir := env.lookup("CLAUDE_CONFIG_DIR"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "projects")
	}
	return env.underHome(".claude", "projects")
}

func codexRoot(env Env) string {
	if dir := env.lookup("CODEX_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "sessions")
	}
	return env.underHome(".codex", "sessions")
}

func factoryRoot(env Env) string {
	return env.underHome(".factory", "sessions")
}

// piRoot is the one to be careful with: ~/.pi/agent/auth.json is the direct
// sibling of ~/.pi/agent/sessions/. Rooting discovery at ~/.pi/agent instead of
// ~/.pi/agent/sessions would put a live credential file one glob away.
func piRoot(env Env) string {
	if dir := env.lookup("PI_CODING_AGENT_SESSION_DIR"); filepath.IsAbs(dir) {
		return dir
	}
	if dir := env.lookup("PI_CODING_AGENT_DIR"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "sessions")
	}
	return env.underHome(".pi", "agent", "sessions")
}

// transcriptExt is the only file extension this package will open. Every
// credential file found in these trees (auth.json, oauth_creds.json,
// .credentials.json, auth.v2.key, config.json) ends in something else, so
// pinning the extension is a cheap, total defence — but it is a backstop, not
// the primary one. The primary defence is that the globs below are fixed-depth
// and rooted at a sessions directory.
const transcriptExt = ".jsonl"

// globTranscripts expands a fixed-depth glob and returns only the results that
// are genuinely safe to open.
//
// filepath.Glob never recurses, so the shape of the pattern already bounds what
// can match. Two further filters matter:
//
//   - the extension must be exactly .jsonl, so no credential file can match even
//     if an agent later drops one into a sessions directory;
//   - the entry must be a REGULAR file by Lstat, which rejects symlinks. Without
//     this, a symlink named transcript.jsonl pointing at ~/.codex/auth.json would
//     satisfy both the pattern and the extension check and be read as a
//     transcript. Lstat does not follow the link, so the symlink is seen for
//     what it is.
//
// A malformed pattern or an unreadable directory yields no results rather than
// an error: discovery is fail-soft by design (see Adapter).
func globTranscripts(root string, pattern string) []string {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(pattern) == "" {
		return nil
	}
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil
	}
	safe := make([]string, 0, len(matches))
	for _, match := range matches {
		if !strings.EqualFold(filepath.Ext(match), transcriptExt) {
			continue
		}
		if pathHasSymlink(root, match) {
			continue
		}
		info, err := os.Lstat(match)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		safe = append(safe, match)
	}
	return safe
}

// pathHasSymlink rejects a match when any component beneath the trusted store
// root is a symlink. The eventual read also goes through os.Root (openContained),
// which binds containment at open time and closes the check/open race; this
// discovery-time check prevents a symlinked project/date directory from being
// indexed in the first place. On Windows, Lstat does not identify junctions as
// ModeSymlink; this function is therefore only a best-effort index filter there.
// The os.Root read is the containment boundary on every platform.
func pathHasSymlink(root string, match string) bool {
	relative, err := filepath.Rel(root, match)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return true
	}
	current := root
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return true
		}
	}
	return false
}

// globSessionDirs lists the immediate subdirectories of root — one fixed level,
// never a walk. Used when no slug candidate matches and every project directory
// has to be considered.
func globSessionDirs(root string) []string {
	if strings.TrimSpace(root) == "" {
		return nil
	}
	matches, err := filepath.Glob(filepath.Join(root, "*"))
	if err != nil {
		return nil
	}
	dirs := make([]string, 0, len(matches))
	for _, match := range matches {
		// Lstat, not Stat: a symlinked project directory could point anywhere,
		// including a tree of credentials, and following it would put arbitrary
		// paths one glob below a directory we do trust.
		info, err := os.Lstat(match)
		if err != nil || !info.IsDir() {
			continue
		}
		dirs = append(dirs, match)
	}
	return dirs
}

// slugCandidates returns the directory names an agent might have used for cwd.
//
// These agents name a directory after the working directory by replacing path
// separators with hyphens. That mapping is LOSSY and cannot be reversed:
// "-Users-kratos-dev-zero" is what both /Users/kratos/dev/zero and
// /Users/kratos/dev-zero produce. It is therefore used only to NARROW the
// search — a hit means "look here first", never "this is the session's cwd".
// The cwd is always taken from the record body instead (see jsonl.go), and
// callers fall back to scanning every slug directory when no candidate matches.
//
// Different agents mangle more than just the separator (some also fold "." and
// "_"), and none of them document it. Rather than guess at a scheme that may
// change, this returns several plausible spellings and treats a miss as a cache
// miss rather than an error.
func slugCandidates(cwd string) []string {
	cleaned := filepath.Clean(strings.TrimSpace(cwd))
	if cleaned == "" || cleaned == "." {
		return nil
	}
	separatorOnly := strings.ReplaceAll(cleaned, string(filepath.Separator), "-")
	folded := strings.NewReplacer(
		string(filepath.Separator), "-",
		".", "-",
		"_", "-",
	).Replace(cleaned)

	candidates := []string{separatorOnly}
	if folded != separatorOnly {
		candidates = append(candidates, folded)
	}
	return candidates
}

// normalizeDir resolves a directory to a stable form for comparison, following
// symlinks so that /tmp and /private/tmp (or any other symlinked prefix) do not
// read as different workspaces. A path that cannot be resolved — typically
// because the directory has since been deleted, which is common in old
// transcripts — degrades to a lexical clean rather than dropping the session.
func normalizeDir(path string) string {
	return normalizeDirForOS(path, runtime.GOOS)
}

func normalizeDirForOS(path string, goos string) string {
	return normalizeDirWithFS(path, goos, os.Stat, filepath.EvalSymlinks)
}

func normalizeDirWithFS(
	path string,
	goos string,
	stat func(string) (os.FileInfo, error),
	evalSymlinks func(string) (string, error),
) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return ""
	}
	cleaned := filepath.Clean(trimmed)
	// A foreign transcript controls this value. Windows path resolution can dial
	// UNC shares (and mapped drives) while merely building /resume, so discovery
	// never performs filesystem I/O for Windows paths. Exact lexical matching is
	// still useful and case-folded below; any stronger containment decision is
	// deferred to the rooted open used when the transcript is actually read.
	if goos == "windows" {
		return cleaned
	}
	// EvalSymlinks is useful for local aliases such as /tmp -> /private/tmp, but
	// only after a local stat proves the path currently exists. Missing or stale
	// transcript paths stay lexical and cannot trigger resolver-specific probes.
	if _, err := stat(cleaned); err != nil {
		return cleaned
	}
	resolved, err := evalSymlinks(cleaned)
	if err != nil {
		return cleaned
	}
	return resolved
}

// sameDir reports whether two directory paths refer to the same workspace.
func sameDir(left string, right string) bool {
	return sameDirForOS(left, right, runtime.GOOS)
}

func sameDirForOS(left string, right string, goos string) bool {
	return sameDirWithFS(left, right, goos, os.Stat, filepath.EvalSymlinks)
}

// sameDirWithFS is the comparison with its filesystem access injectable, so a
// test can prove that a Windows path is compared WITHOUT touching the resolver.
func sameDirWithFS(
	left string,
	right string,
	goos string,
	stat func(string) (os.FileInfo, error),
	evalSymlinks func(string) (string, error),
) bool {
	normalizedLeft := normalizeDirWithFS(left, goos, stat, evalSymlinks)
	normalizedRight := normalizeDirWithFS(right, goos, stat, evalSymlinks)
	if normalizedLeft == "" || normalizedRight == "" {
		return false
	}
	if goos == "windows" {
		return strings.EqualFold(normalizedLeft, normalizedRight)
	}
	return normalizedLeft == normalizedRight
}

// SameWorkspace reports whether two workspace paths name the same directory,
// under the policy discovery already uses for foreign paths.
//
// EXPORTED BECAUSE THE TUI WAS COMPARING WITH A DIFFERENT POLICY. Discovery
// deliberately never resolves a Windows path (normalizeDirWithFS): a transcript
// controls that value, and EvalSymlinks on "\\server\share\repo" dials the
// share — SMB authentication and a stalled UI on an unavailable host — merely
// to decide whether a row belongs in /resume. The TUI's own comparison then
// reintroduced exactly that call, for the same foreign value, on the Update
// loop: once while formatting the post-import note, and again for every
// persisted WorkspaceKey while filtering the picker and choosing the latest
// session. Checking runtime.GOOS after the resolver had already run could not
// prevent the effect. One comparison, one policy: lexical and case-insensitive
// on Windows, stat-then-resolve elsewhere so local aliases such as
// /tmp -> /private/tmp still match. Reported by @jatmn.
func SameWorkspace(left string, right string) bool {
	return sameDir(left, right)
}
