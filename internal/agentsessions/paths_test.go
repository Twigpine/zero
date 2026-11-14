package agentsessions

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func testEnv(home string, vars map[string]string) Env {
	return Env{Home: home, Getenv: func(name string) string { return vars[name] }}
}

func TestRootsHonourTheAgentsRedirectVariables(t *testing.T) {
	home := t.TempDir()
	elsewhere := t.TempDir()

	plain := testEnv(home, nil)
	if got, want := claudeCodeRoot(plain), filepath.Join(home, ".claude", "projects"); got != want {
		t.Errorf("claudeCodeRoot = %q, want %q", got, want)
	}
	if got, want := codexRoot(plain), filepath.Join(home, ".codex", "sessions"); got != want {
		t.Errorf("codexRoot = %q, want %q", got, want)
	}

	// A user with these set keeps their transcripts somewhere else entirely.
	// Hardcoding ~ would discover nothing while looking like it worked.
	redirected := testEnv(home, map[string]string{
		"CLAUDE_CONFIG_DIR": elsewhere,
		"CODEX_HOME":        elsewhere,
	})
	if got, want := claudeCodeRoot(redirected), filepath.Join(elsewhere, "projects"); got != want {
		t.Errorf("CLAUDE_CONFIG_DIR ignored: got %q, want %q", got, want)
	}
	if got, want := codexRoot(redirected), filepath.Join(elsewhere, "sessions"); got != want {
		t.Errorf("CODEX_HOME ignored: got %q, want %q", got, want)
	}
}

func TestRelativeRedirectVariablesAreRejected(t *testing.T) {
	env := testEnv(t.TempDir(), map[string]string{
		"CLAUDE_CONFIG_DIR": ".config/claude",
		"CODEX_HOME":        ".codex",
	})
	if got, want := claudeCodeRoot(env), filepath.Join(env.Home, ".claude", "projects"); got != want {
		t.Fatalf("relative CLAUDE_CONFIG_DIR resolved from process cwd: got %q, want %q", got, want)
	}
	if got, want := codexRoot(env), filepath.Join(env.Home, ".codex", "sessions"); got != want {
		t.Fatalf("relative CODEX_HOME resolved from process cwd: got %q, want %q", got, want)
	}
}

func TestAnUnknownHomeYieldsNoRootRatherThanARelativePath(t *testing.T) {
	// With no home, a naive filepath.Join would produce ".claude/projects" and
	// probe relative to the process working directory — a different user's
	// checkout, in the worst case. Every root must come back empty instead.
	blank := Env{}
	for name, root := range map[string]string{
		"claude":  claudeCodeRoot(blank),
		"codex":   codexRoot(blank),
		"factory": factoryRoot(blank),
		"pi":      piRoot(blank),
	} {
		if root != "" {
			t.Errorf("%s root with no home = %q, want empty", name, root)
		}
	}
}

// credentialFilenames are the real filenames found beside real transcripts on a
// working machine. Every one of the seven agents surveyed keeps at least one of
// these in the same tree as its sessions.
var credentialFilenames = []string{
	"auth.json",
	"auth.v2.key",
	"auth.v2.file",
	".credentials.json",
	"oauth_creds.json",
	"config.json",
	"credentials",
}

// TestDiscoveryGlobsNeverMatchACredentialFile is the reason this package uses
// fixed-depth globs instead of filepath.WalkDir.
//
// The layout below mirrors ~/.pi/agent/, where auth.json is the direct sibling
// of sessions/ — the tightest real case. If a future change swaps the glob for
// a walk, or roots it one directory higher, this test fails.
func TestDiscoveryGlobsNeverMatchACredentialFile(t *testing.T) {
	home := t.TempDir()
	agentDir := filepath.Join(home, ".pi", "agent")
	sessionsDir := filepath.Join(agentDir, "sessions", "-Users-someone-proj")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// A real transcript, which must be found.
	transcript := filepath.Join(sessionsDir, "2026-01-01T00-00-00Z_abc.jsonl")
	writeFile(t, transcript, `{"type":"session","cwd":"/Users/someone/proj"}`)

	// Decoy transcripts at depths the fixed-depth glob must not reach. These
	// pin the MECHANISM rather than the outcome: the credential filenames below
	// are already caught by the extension check, so without these a directory
	// walk substituted for the glob would still pass. Each of these is a
	// well-formed .jsonl that only a walk (or a wrong root) can find.
	offDepth := []string{
		filepath.Join(agentDir, "sessions", "stray.jsonl"),                 // too shallow
		filepath.Join(sessionsDir, "nested", "deep.jsonl"),                 // too deep
		filepath.Join(sessionsDir, "nested", "deeper", "deepest.jsonl"),    // deeper still
		filepath.Join(agentDir, "leaked.jsonl"),                            // above the sessions root
		filepath.Join(agentDir, "checkpoints", "proj", "checkpoint.jsonl"), // sibling tree
	}
	for _, path := range offDepth {
		writeFile(t, path, `{"type":"session","cwd":"/Users/someone/proj"}`)
	}

	// Credentials planted at every level a careless root or a walk would reach:
	// beside the transcript, in the sessions root, and — the ~/.pi/agent case —
	// one level above the sessions directory.
	planted := []string{}
	for _, dir := range []string{sessionsDir, filepath.Join(agentDir, "sessions"), agentDir} {
		for _, name := range credentialFilenames {
			path := filepath.Join(dir, name)
			writeFile(t, path, `{"access_token":"tok_MUST_NEVER_BE_READ"}`)
			planted = append(planted, path)
		}
	}

	env := testEnv(home, nil)
	matches := globTranscripts(piRoot(env), filepath.Join(piRoot(env), "*", "*"+transcriptExt))

	if len(matches) != 1 || matches[0] != transcript {
		t.Fatalf("glob = %v, want exactly [%s] — anything extra means discovery "+
			"is reaching beyond one fixed depth below the sessions root", matches, transcript)
	}
	for _, got := range matches {
		for _, path := range offDepth {
			if got == path {
				t.Errorf("discovery reached an off-depth file (a walk, not a glob): %s", got)
			}
		}
	}
	// Belt and braces: neither the exact planted paths nor any file merely
	// *named* like a credential may appear, so a future glob that reaches a
	// different directory still fails here.
	for _, got := range matches {
		for _, path := range planted {
			if got == path {
				t.Errorf("glob matched a credential file: %s", got)
			}
		}
		for _, name := range credentialFilenames {
			if strings.EqualFold(filepath.Base(got), name) {
				t.Errorf("glob matched a credential filename: %s", got)
			}
		}
	}
}

// TestGlobRejectsASymlinkWearingATranscriptExtension closes the gap the
// extension check alone leaves open: the name says .jsonl, the target is a
// credential file. Lstat does not follow the link, so the entry is seen for
// what it is and dropped.
func TestGlobRejectsASymlinkWearingATranscriptExtension(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on Windows")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "-Users-someone-proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(root, "auth.json")
	writeFile(t, secret, `{"access_token":"tok_MUST_NEVER_BE_READ"}`)

	disguised := filepath.Join(dir, "innocent.jsonl")
	if err := os.Symlink(secret, disguised); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(dir, "real.jsonl")
	writeFile(t, real, `{"type":"session"}`)

	matches := globTranscripts(root, filepath.Join(root, "*", "*"+transcriptExt))
	if len(matches) != 1 || matches[0] != real {
		t.Fatalf("glob = %v, want exactly [%s] — the symlink must be rejected", matches, real)
	}
}

func TestGlobRejectsTranscriptBelowSymlinkedDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	outsideTranscript := filepath.Join(outside, "escaped.jsonl")
	writeFile(t, outsideTranscript, `{"type":"session"}`)
	linkedDir := filepath.Join(root, "project")
	if err := os.Symlink(outside, linkedDir); err != nil {
		t.Fatal(err)
	}

	matches := globTranscripts(root, filepath.Join(root, "*", "*"+transcriptExt))
	if len(matches) != 0 {
		t.Fatalf("glob followed a symlinked directory outside the store: %v", matches)
	}
	if _, err := openContained(root, filepath.Join(linkedDir, "escaped.jsonl")); err == nil {
		t.Fatal("rooted open followed a symlink outside the store")
	}
}

func TestGlobDegradesToEmptyRatherThanFailing(t *testing.T) {
	// A store that was never created, and a pattern that cannot compile. Both
	// mean "this adapter has nothing", never an error that fails the command
	// for the other six agents.
	root := t.TempDir()
	if got := globTranscripts(root, filepath.Join(root, "absent", "*", "*.jsonl")); len(got) != 0 {
		t.Errorf("missing root = %v, want empty", got)
	}
	if got := globTranscripts(root, filepath.Join(root, "[", "*.jsonl")); len(got) != 0 {
		t.Errorf("malformed pattern = %v, want empty", got)
	}
	if got := globTranscripts("", ""); len(got) != 0 {
		t.Errorf("empty pattern = %v, want empty", got)
	}
}

func TestGlobIgnoresNonTranscriptExtensions(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.json", "b.db", "c.jsonl.bak", "d.txt", "keep.jsonl"} {
		writeFile(t, filepath.Join(dir, name), "{}")
	}
	matches := globTranscripts(root, filepath.Join(root, "*", "*"))
	if len(matches) != 1 || filepath.Base(matches[0]) != "keep.jsonl" {
		t.Fatalf("glob = %v, want only keep.jsonl", matches)
	}
}

func TestSlugCandidatesNarrowButAreNeverAuthoritative(t *testing.T) {
	got := slugCandidates("/Users/kratos/dev/zero")
	if len(got) == 0 || got[0] != "-Users-kratos-dev-zero" {
		t.Fatalf("slugCandidates = %v, want the separator-folded form first", got)
	}

	// The lossiness this guards against: two different directories produce the
	// same slug, which is why the cwd is always re-read from the record body.
	if slugCandidates("/Users/kratos/dev/zero")[0] != slugCandidates("/Users/kratos/dev-zero")[0] {
		t.Fatal("expected these two cwds to collide — if they no longer do, the " +
			"comment in paths.go about slug lossiness needs revisiting")
	}

	if got := slugCandidates(""); got != nil {
		t.Errorf("slugCandidates(\"\") = %v, want nil", got)
	}
}

func TestSameDirFollowsSymlinks(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if !sameDir(real, real+string(filepath.Separator)+".") {
		t.Error("a path and its own clean form should match")
	}
	if sameDir(real, filepath.Join(root, "other")) {
		t.Error("different directories should not match")
	}
	if sameDir("", real) || sameDir(real, "") {
		t.Error("an empty path should never match anything")
	}

	if runtime.GOOS != "windows" {
		link := filepath.Join(root, "link")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		if !sameDir(link, real) {
			t.Error("a symlinked workspace should resolve to the same directory")
		}
	}
}

func TestSameDirUsesCaseInsensitiveComparisonOnWindows(t *testing.T) {
	if !sameDirForOS("/Work/Project", "/work/project", "windows") {
		t.Fatal("Windows workspace comparison rejected a case-only spelling difference")
	}
	if sameDirForOS("/Work/Project", "/work/other", "windows") {
		t.Fatal("Windows workspace comparison accepted different paths")
	}
}

func TestWindowsForeignPathsNeverTouchTheFilesystemDuringNormalization(t *testing.T) {
	path := `\\192.0.2.201\share\loot`
	called := false
	got := normalizeDirWithFS(path, "windows",
		func(string) (os.FileInfo, error) {
			called = true
			return nil, errors.New("must not stat a foreign Windows path")
		},
		func(string) (string, error) {
			called = true
			return "", errors.New("must not resolve a foreign Windows path")
		},
	)
	if called {
		t.Fatal("Windows path normalization performed filesystem I/O")
	}
	if got != filepath.Clean(path) {
		t.Fatalf("normalizeDirWithFS = %q, want lexical %q", got, filepath.Clean(path))
	}
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func fileSizeOf(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func itoa(value int) string { return strconv.Itoa(value) }

// COMPARING A FOREIGN WINDOWS PATH MUST NOT TOUCH THE FILESYSTEM. A transcript
// controls its cwd, and resolving "\\server\share\repo" on Windows dials the
// share. Discovery already compares such paths lexically; the TUI's picker,
// latest-session filter and post-import note now go through the same helper,
// so this is the one place that policy is proved. The resolver seams here fail
// the test if they are ever called for a Windows path.
func TestAForeignWindowsPathIsComparedWithoutResolverIO(t *testing.T) {
	fatalStat := func(path string) (os.FileInfo, error) {
		t.Fatalf("stat was called for a foreign Windows path: %q", path)
		return nil, nil
	}
	fatalEval := func(path string) (string, error) {
		t.Fatalf("EvalSymlinks was called for a foreign Windows path: %q", path)
		return "", nil
	}
	if !sameDirWithFS(`\\server\share\repo`, `\\SERVER\share\REPO`, "windows", fatalStat, fatalEval) {
		t.Error("case-insensitive UNC match failed")
	}
	if sameDirWithFS(`\\server\share\repo`, `\\server\share\other`, "windows", fatalStat, fatalEval) {
		t.Error("different UNC paths compared equal")
	}
	if !sameDirWithFS(`C:\Proj\repo`, `c:\proj\REPO`, "windows", fatalStat, fatalEval) {
		t.Error("case-insensitive drive path match failed")
	}
	// Elsewhere a local alias still resolves — after a stat proves it exists —
	// so /tmp -> /private/tmp keeps matching for the TUI.
	if runtime.GOOS != "windows" {
		target := t.TempDir()
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(target, link); err != nil {
			t.Skip(err)
		}
		if !SameWorkspace(link, target) {
			t.Errorf("a symlinked local workspace no longer matches its target")
		}
	}
}
