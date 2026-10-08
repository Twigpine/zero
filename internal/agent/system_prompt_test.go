package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/sandbox"
)

// TestMain points userConfigDirForPrompt at an empty, package-wide temp
// directory for every test in this package. Without this, buildSystemPrompt
// falls back to the real config.UserConfigDir, so any developer with a
// personal ~/.config/zero/ZERO.md would get its content folded into
// otherwise-unrelated prompt assertions, making test runs non-deterministic
// across contributor machines. Tests that specifically exercise user
// guidelines stub userConfigDirForPrompt themselves via
// withSystemPromptTestUserConfigDir(Func) and restore this default on cleanup.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "zero-agent-tests-*")
	if err != nil {
		panic(err)
	}
	userConfigDirForPrompt = func() (string, error) { return dir, nil }
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func TestCoreSystemPromptIncludesCodingQualityRules(t *testing.T) {
	prompt := strings.Join(strings.Fields(strings.ToLower(buildSystemPrompt(Options{}))), " ")
	if strings.Contains(prompt, "clickable file paths") {
		t.Fatalf("system prompt must not claim inline-code file paths are clickable: %s", prompt)
	}

	for _, want := range []string{
		"read-before-edit",
		"inspect the target file",
		"plan then act",
		"choose the narrowest tool",
		"for edits to existing files, use edit_file",
		"verify after edits",
		"honor the active permission mode",
		"avoid broad refactors",
		"search the web before answering",
		"do not recognize",
		"scaled to the work",
		"comment density",
		"skip it for bounded changes in one component",
		"do not read both unless a new need for exact content appears",
		"combine compatible commands when diagnostics stay useful",
		"do not invent a parallel flow",
		"a successful native patch confirms the edit",
		"once the required behavior passes validation, stop",
		"do not look them up solely for the final summary",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("expected core system prompt to include %q, got:\n%s", want, buildSystemPrompt(Options{}))
		}
	}
}

func TestTransientSystemPromptIsIncludedOnlyWhenProvided(t *testing.T) {
	const transient = "Handle this peer request under the current session policy."
	if prompt := buildSystemPrompt(Options{}); strings.Contains(prompt, transient) {
		t.Fatal("ordinary system prompt unexpectedly contains transient instructions")
	}
	if prompt := buildSystemPrompt(Options{TransientSystemPrompt: transient}); !strings.Contains(prompt, transient) {
		t.Fatalf("transient instructions missing from system prompt:\n%s", prompt)
	}
}

func TestSystemPromptExplainsSandboxEscalationForHiddenHostState(t *testing.T) {
	prompt := strings.ToLower(buildSystemPrompt(Options{}))
	for _, want := range []string{
		"sandboxed shell command returns only sandbox-local state",
		"sandbox_permissions: \"require_escalated\"",
		"prefix_rule",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("expected sandbox escalation guidance %q, got:\n%s", want, buildSystemPrompt(Options{}))
		}
	}
}

func TestBuildSystemPromptIncludesWorkspaceSeedFromCwd(t *testing.T) {
	cwd := t.TempDir()
	writeSystemPromptTestFile(t, cwd, "go.mod", "module example.test/zero\n")
	writeSystemPromptTestFile(t, cwd, "AGENTS.md", "Use Go commands.\n")
	writeSystemPromptTestFile(t, cwd, "cmd/zero/main.go", "package main\n")
	writeSystemPromptTestFile(t, cwd, "internal/agent/loop.go", "package agent\n")
	writeSystemPromptTestFile(t, cwd, "node_modules/pkg/index.js", "ignored")
	writeSystemPromptTestFile(t, cwd, filepath.Join(".git", "HEAD"), "ref: refs/heads/feature/seed\n")

	prompt := buildSystemPrompt(Options{Cwd: cwd})

	for _, want := range []string{
		"<workspace_seed>",
		"Workspace context seed",
		"cwd: " + filepath.Base(cwd),
		"git: feature/seed",
		"layout: AGENTS.md, cmd/, go.mod, internal/",
		"project files: go.mod, AGENTS.md",
		"memory hints: AGENTS.md",
		"</workspace_seed>",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("expected workspace seed to include %q, got:\n%s", want, prompt)
		}
	}
	seed := systemPromptTestBlock(t, prompt, "<workspace_seed>", "</workspace_seed>")
	if strings.Contains(seed, cwd) {
		t.Fatalf("workspace seed should use safe cwd label, not absolute path %q, got:\n%s", cwd, seed)
	}
	if strings.Contains(prompt, "node_modules") {
		t.Fatalf("workspace seed should inherit workspace skip rules, got:\n%s", prompt)
	}
}

func TestBuildSystemPromptOmitsWorkspaceSeedWithoutCwd(t *testing.T) {
	prompt := buildSystemPrompt(Options{})

	if strings.Contains(prompt, "<workspace_seed>") || strings.Contains(prompt, "Workspace context seed") {
		t.Fatalf("workspace seed should be absent without cwd, got:\n%s", prompt)
	}
}

func TestBuildSystemPromptIncludesApprovedCommandPrefixes(t *testing.T) {
	store, err := sandbox.NewGrantStore(sandbox.StoreOptions{FilePath: filepath.Join(t.TempDir(), "sandbox-grants.json")})
	if err != nil {
		t.Fatalf("NewGrantStore returned error: %v", err)
	}
	if _, err := store.GrantCommandPrefix(sandbox.CommandPrefixInput{ToolName: "bash", Prefix: []string{"git", "status"}}); err != nil {
		t.Fatalf("GrantCommandPrefix returned error: %v", err)
	}
	prompt := buildSystemPrompt(Options{Sandbox: sandbox.NewEngine(sandbox.EngineOptions{Store: store})})
	for _, want := range []string{"Approved Command Prefixes", "bash: git status"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("system prompt missing %q:\n%s", want, prompt)
		}
	}
}

func writeSystemPromptTestFile(t *testing.T, root, rel, contents string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func systemPromptTestBlock(t *testing.T, prompt, start, end string) string {
	t.Helper()
	startIndex := strings.Index(prompt, start)
	if startIndex < 0 {
		t.Fatalf("missing block start %q", start)
	}
	afterStart := prompt[startIndex+len(start):]
	body, _, ok := strings.Cut(afterStart, end)
	if !ok {
		t.Fatalf("missing block end %q", end)
	}
	return body
}

func TestBuildSystemPromptIncludesUserGuidelines(t *testing.T) {
	configDir := t.TempDir()
	writeSystemPromptTestFile(t, configDir, "zero/ZERO.md", "  Prefer concise summaries.  \n")
	t.Cleanup(withSystemPromptTestUserConfigDir(t, configDir))

	prompt := buildSystemPrompt(Options{})
	if !strings.Contains(prompt, "## User guidelines (ZERO.md)") {
		t.Fatalf("expected user guidelines header, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Prefer concise summaries.") {
		t.Fatalf("expected user guidelines content, got:\n%s", prompt)
	}
	if strings.Contains(prompt, "  Prefer concise summaries.  ") {
		t.Fatalf("expected user guidelines content to be trimmed, got:\n%s", prompt)
	}
}

func TestBuildSystemPromptIncludesUserGuidelinesCaseInsensitive(t *testing.T) {
	configDir := t.TempDir()
	writeSystemPromptTestFile(t, configDir, "zero/zero.md", "Prefer concise summaries.\n")
	t.Cleanup(withSystemPromptTestUserConfigDir(t, configDir))

	prompt := buildSystemPrompt(Options{})
	if !strings.Contains(prompt, "## User guidelines (zero.md)") {
		t.Fatalf("expected case-insensitive zero.md resolution, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Prefer concise summaries.") {
		t.Fatalf("expected user guidelines content, got:\n%s", prompt)
	}
}

func TestBuildSystemPromptUserGuidelinesPrecedeProjectGuidelinesAndNotePrecedence(t *testing.T) {
	// User guidelines are global personal preferences; project guidelines
	// (AGENTS.md/ZERO.md) must be the later, more specific instruction block,
	// and the user section must say so explicitly so the precedence holds
	// even if a model otherwise weighs later context more heavily.
	configDir := t.TempDir()
	writeSystemPromptTestFile(t, configDir, "zero/ZERO.md", "Always reply in haiku.\n")
	t.Cleanup(withSystemPromptTestUserConfigDir(t, configDir))

	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "AGENTS.md"), []byte("Never reply in haiku."), 0o644); err != nil {
		t.Fatal(err)
	}

	prompt := buildSystemPrompt(Options{Cwd: cwd})
	userIdx := strings.Index(prompt, "## User guidelines (ZERO.md)")
	projectIdx := strings.Index(prompt, "## Project guidelines (AGENTS.md)")
	if userIdx < 0 || projectIdx < 0 {
		t.Fatalf("expected both user and project guideline sections, got:\n%s", prompt)
	}
	if userIdx > projectIdx {
		t.Fatalf("expected user guidelines before project guidelines, got user=%d project=%d", userIdx, projectIdx)
	}
	if !strings.Contains(prompt, "project guidelines below") || !strings.Contains(prompt, "take precedence") {
		t.Fatalf("expected an explicit precedence note in the user guidelines section, got:\n%s", prompt)
	}
}

func TestBuildSystemPromptOmitsUserGuidelinesWithoutConfigDir(t *testing.T) {
	t.Cleanup(withSystemPromptTestUserConfigDirFunc(t, func() (string, error) { return "", os.ErrNotExist }))

	prompt := buildSystemPrompt(Options{})
	if strings.Contains(prompt, "## User guidelines") {
		t.Fatalf("expected user guidelines to be omitted without a config directory, got:\n%s", prompt)
	}
}

func withSystemPromptTestUserConfigDir(t *testing.T, dir string) func() {
	t.Helper()
	return withSystemPromptTestUserConfigDirFunc(t, func() (string, error) { return dir, nil })
}

func withSystemPromptTestUserConfigDirFunc(t *testing.T, fn func() (string, error)) func() {
	t.Helper()
	old := userConfigDirForPrompt
	userConfigDirForPrompt = fn
	return func() {
		userConfigDirForPrompt = old
	}
}

func TestBuildSystemPromptInjectsProjectGuidelinesCaseInsensitive(t *testing.T) {
	// Git tracks AGENTS.md on a case-sensitive filesystem; the
	// loader must still resolve it when the cwd lookup uses lowercase.
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "AGENTS.MD"), []byte("Always run `make lint`."), 0o644); err != nil {
		t.Fatal(err)
	}
	prompt := buildSystemPrompt(Options{Cwd: cwd})
	if !strings.Contains(prompt, "## Project guidelines (AGENTS.MD)") {
		t.Fatalf("expected case-insensitive AGENTS.MD resolution, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "make lint") {
		t.Fatalf("expected AGENTS.MD content injected, got:\n%s", prompt)
	}
}

func TestBuildSystemPromptProjectGuidelinesPathWalkingMonorepo(t *testing.T) {
	// Simulate a monorepo: root + sub-tree each have their own AGENTS.md.
	// The user launches Zero from the sub-tree, so both files should be
	// injected in general-to-specific order (root first, cwd last).
	root := t.TempDir()
	leaf := filepath.Join(root, "services", "api")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("Repo-wide: prefer Go."), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leaf, "AGENTS.md"), []byte("API: follow REST conventions."), 0o644); err != nil {
		t.Fatal(err)
	}
	prompt := buildSystemPrompt(Options{Cwd: leaf})
	normalized := filepath.ToSlash(prompt)
	rootLabel := "Project guidelines (AGENTS.md)"
	leafLabel := "Project guidelines (services/api/AGENTS.md)"
	rootBlock := systemPromptTestBlock(t, normalized, rootLabel, leafLabel)
	leafBlock := systemPromptTestBlock(t, normalized, leafLabel, "## Repo map")
	if !strings.Contains(rootBlock, "Repo-wide: prefer Go.") {
		t.Fatalf("expected root AGENTS.md in general-to-specific slot, got:\n%s", rootBlock)
	}
	if !strings.Contains(leafBlock, "API: follow REST conventions.") {
		t.Fatalf("expected leaf AGENTS.md in specific slot, got:\n%s", leafBlock)
	}
	// Root must appear before leaf in the prompt.
	rootIdx := strings.Index(normalized, "## "+rootLabel)
	leafIdx := strings.Index(normalized, "## "+leafLabel)
	if rootIdx < 0 || leafIdx < 0 || rootIdx > leafIdx {
		t.Fatalf("expected root (general) before leaf (specific) in prompt, got root=%d leaf=%d", rootIdx, leafIdx)
	}
}

func TestBuildSystemPromptProjectGuidelinesZeroFallback(t *testing.T) {
	// ZERO.md is the second-priority name at each level; the loader picks it
	// when no AGENTS.md is present.
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "ZERO.md"), []byte("Brand-specific rule."), 0o644); err != nil {
		t.Fatal(err)
	}
	prompt := buildSystemPrompt(Options{Cwd: cwd})
	if !strings.Contains(prompt, "## Project guidelines (ZERO.md)") {
		t.Fatalf("expected ZERO.md fallback to be injected, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Brand-specific rule.") {
		t.Fatalf("expected ZERO.md content, got:\n%s", prompt)
	}
}

func TestBuildSystemPromptProjectGuidelinesProjectLocalFallback(t *testing.T) {
	cwd := t.TempDir()
	dot := filepath.Join(cwd, ".zero")
	if err := os.MkdirAll(dot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dot, "AGENTS.md"), []byte("Personal: use dark theme."), 0o644); err != nil {
		t.Fatal(err)
	}
	prompt := buildSystemPrompt(Options{Cwd: cwd})
	// Without a git root, the label collapses to the basename; the test
	// confirms the .zero/AGENTS.md file's content is the one injected, not
	// any other file.
	if !strings.Contains(prompt, "Personal: use dark theme.") {
		t.Fatalf("expected .zero/AGENTS.md content, got:\n%s", prompt)
	}
	// The project guidelines block must be present (regardless of label).
	if !strings.Contains(prompt, "## Project guidelines (") {
		t.Fatalf("expected a project guidelines block, got:\n%s", prompt)
	}
}

func TestBuildSystemPromptProjectGuidelinesTruncatesAtTotalCap(t *testing.T) {
	// Root file fits in the per-file cap; leaf file is bigger than what's
	// left of the total budget, so it must be truncated. The truncation
	// marker must appear in the prompt and the full untruncated payload
	// must not.
	root := t.TempDir()
	leaf := filepath.Join(root, "sub")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatal(err)
	}
	rootContent := strings.Repeat("r", maxProjectContextBytes)        // exactly per-file cap
	leafContent := strings.Repeat("L", maxProjectContextTotalBytes+1) // over the total cap
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(rootContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leaf, "AGENTS.md"), []byte(leafContent), 0o644); err != nil {
		t.Fatal(err)
	}
	prompt := buildSystemPrompt(Options{Cwd: leaf})
	if !strings.Contains(prompt, "truncated") {
		t.Fatalf("expected truncation marker on second file, prompt length=%d", len(prompt))
	}
	// Sanity: the leaf file's full payload must NOT be present untruncated.
	if strings.Contains(prompt, strings.Repeat("L", maxProjectContextTotalBytes-1)) {
		t.Fatalf("leaf file appears untruncated; total cap is not enforced")
	}
}

func TestTruncateGuidelineContentStaysWithinLimit(t *testing.T) {
	limit := 64
	content := strings.Repeat("x", limit+1)
	got := truncateGuidelineContent(content, limit)
	if len(got) > limit {
		t.Fatalf("truncated result is %d bytes, want at most %d", len(got), limit)
	}
	if !strings.Contains(got, "truncated") {
		t.Fatalf("expected truncation marker, got %q", got)
	}
}

func TestTruncateGuidelineContentStaysWithinLimitForSmallBudgets(t *testing.T) {
	// Limits at or below the truncation marker's own length can't fit the
	// marker at all; the helper must still never exceed the requested limit.
	content := strings.Repeat("x", 100)
	for _, limit := range []int{0, 1, len(truncationMarker) - 1, len(truncationMarker), len(truncationMarker) + 1} {
		got := truncateGuidelineContent(content, limit)
		if len(got) > limit {
			t.Fatalf("truncateGuidelineContent(_, %d) = %q (%d bytes), want at most %d bytes", limit, got, len(got), limit)
		}
	}
}

func TestProjectGuidelineDirsOrdersRootToLeaf(t *testing.T) {
	root := filepath.Join("r")
	leaf := filepath.Join(root, "a", "b")
	got := projectGuidelineDirs(leaf, root)
	want := []string{root, filepath.Join(root, "a"), leaf}
	if len(got) != len(want) {
		t.Fatalf("projectGuidelineDirs = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("projectGuidelineDirs[%d] = %q, want %q (full %v)", i, got[i], want[i], got)
		}
	}
}

func TestProjectGuidelineDirsCollapsesToCwdWithoutGitRoot(t *testing.T) {
	got := projectGuidelineDirs(filepath.Join("some", "path"), "")
	if len(got) != 1 || got[0] != filepath.Join("some", "path") {
		t.Fatalf("projectGuidelineDirs = %v, want [some/path]", got)
	}
}

func TestFindProjectContextFileCaseInsensitiveBasename(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "AGENTS.MD"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := findProjectContextFile(cwd)
	if filepath.Base(got) != "AGENTS.MD" {
		t.Fatalf("findProjectContextFile = %q, want basename AGENTS.MD", got)
	}
}

func TestFindProjectGitRootIgnoresEmptyGitDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(root, "child")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := FindProjectGitRoot(leaf); got != "" {
		t.Fatalf("FindProjectGitRoot = %q, want empty for invalid .git directory", got)
	}
}
