package workspaceindex

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// isolateGitConfig keeps the git child processes away from the developer's real global and
// system config: a global core.excludesFile would change what counts as ignored and make
// these tests depend on the machine. GIT_CONFIG_GLOBAL needs git >= 2.32; HOME and
// XDG_CONFIG_HOME cover older git and macOS/Windows home resolution.
func isolateGitConfig(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

// stopGitDiscoveryAbove keeps git from finding an enclosing repository above root, so a
// TMPDIR that happens to live inside a git work tree cannot turn a "not a repo" fixture
// into one.
func stopGitDiscoveryAbove(t *testing.T, root string) {
	t.Helper()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(root))
}

// requireGitIgnoredMatching skips below git 2.16, where `status --ignored=matching` landed.
// Older git takes Scan's non-git path, which the fallback tests cover separately.
func requireGitIgnoredMatching(t *testing.T) {
	t.Helper()
	out, err := exec.Command("git", "version").Output()
	if err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	var major, minor int
	if _, err := fmt.Sscanf(strings.TrimPrefix(strings.TrimSpace(string(out)), "git version "), "%d.%d", &major, &minor); err != nil {
		t.Skipf("cannot parse %q: %v", out, err)
	}
	if major < 2 || (major == 2 && minor < 16) {
		t.Skipf("needs git >= 2.16 for status --ignored=matching, have %d.%d", major, minor)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=zero-test", "-c", "user.email=zero-test@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func initGitRepo(t *testing.T, root, gitignore string) {
	t.Helper()
	isolateGitConfig(t)
	requireGitIgnoredMatching(t)
	stopGitDiscoveryAbove(t, root)
	runGit(t, root, "init", "-q")
	if gitignore != "" {
		writeFile(t, root, ".gitignore", gitignore)
	}
}

// A virtualenv or cache directory whose name no fixed list anticipates must still be
// skipped when the repository's own .gitignore excludes it. Reported against a real repo
// where `zero repo-map` spent its whole file budget inside a `.locust_env/` virtualenv and
// never reached the repo's own README.md.
func TestScanHonorsRepoGitignore(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root, ".locust_env/\n*.db\ncafé-cache/\n")

	writeFile(t, root, "README.md", "# Example\n")
	writeFile(t, root, "app/main.py", "print('hi')\n")
	writeFile(t, root, "app/café.py", "x = 1\n")
	writeFile(t, root, ".locust_env/lib/python3.13/site-packages/flask/sansio/README.md", "vendored")
	writeFile(t, root, "audit_results.db", "sqlite")
	// Ignored file inside an untracked directory: `ls-files --directory` would miss it.
	writeFile(t, root, "app/cache.db", "sqlite")
	// Non-ASCII ignored directory: only matches because the lookup uses -z (no C-quoting).
	writeFile(t, root, "café-cache/blob.json", "{}")

	got, err := Scan(root, Options{MaxDepth: DefaultMaxDepth})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	want := []string{".gitignore", "README.md", "app/café.py", "app/main.py"}
	if !reflect.DeepEqual(pathsOf(got.Files), want) {
		t.Fatalf("Files=%v want %v", pathsOf(got.Files), want)
	}
}

// Scanning a repository subdirectory must resolve ignore rules relative to that
// subdirectory (Scan's `rel`), not relative to the repository top level.
func TestScanHonorsRepoGitignoreFromSubdirectoryRoot(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo, "build-out/\n")
	writeFile(t, repo, "svc/handler.go", "package svc\n")
	writeFile(t, repo, "svc/build-out/gen.go", "package gen\n")

	got, err := Scan(filepath.Join(repo, "svc"), Options{MaxDepth: DefaultMaxDepth})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if want := []string{"handler.go"}; !reflect.DeepEqual(pathsOf(got.Files), want) {
		t.Fatalf("Files=%v want %v", pathsOf(got.Files), want)
	}
}

// Inside a git work tree, git decides. A tracked directory that merely shares a name with
// a build-cache directory (a Go package called "target", say) is real source and must be
// scanned; the fixed list is only a stand-in for when git cannot be asked.
func TestScanKeepsTrackedBuildCacheNamedDirInsideGitRepo(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root, "")
	writeFile(t, root, "main.go", "package main\n")
	writeFile(t, root, "target/target.go", "package target\n")
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-q", "-m", "fixture")

	got, err := Scan(root, Options{MaxDepth: DefaultMaxDepth})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if want := []string{"main.go", "target/target.go"}; !reflect.DeepEqual(pathsOf(got.Files), want) {
		t.Fatalf("Files=%v want %v", pathsOf(got.Files), want)
	}
}

// Scanning a directory that the enclosing repository ignores as a whole must still list
// its contents: git reports the root itself as ignored, which is not a reason to skip
// everything under it.
func TestScanOfIgnoredRootStillListsItsFiles(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo, "out/\n")
	writeFile(t, repo, "out/report.md", "# report\n")
	writeFile(t, repo, "out/data/rows.csv", "a,b\n")

	got, err := Scan(filepath.Join(repo, "out"), Options{MaxDepth: DefaultMaxDepth})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if want := []string{"data/rows.csv", "report.md"}; !reflect.DeepEqual(pathsOf(got.Files), want) {
		t.Fatalf("Files=%v want %v", pathsOf(got.Files), want)
	}
}

// Outside a git work tree, Scan falls back to the fixed build/cache list so the file
// budget is not spent on build output. Reported against a repo where
// rust/*/target/debug/.fingerprint alone held hundreds of files: with a small budget the
// real source must still be reached and the scan must not report truncation.
func TestScanSkipsBuildCacheDirsOutsideGit(t *testing.T) {
	isolateGitConfig(t)
	root := t.TempDir()
	stopGitDiscoveryAbove(t, root)
	if _, ok := gitIgnoredPaths(root); ok {
		t.Fatalf("gitIgnoredPaths(non-repo) reported a work tree")
	}

	writeFile(t, root, "src/lib.rs", "pub fn f() {}\n")
	writeFile(t, root, "app/main.py", "print('hi')\n")
	for i := range 10 {
		writeFile(t, root, fmt.Sprintf("rust/crate/target/debug/.fingerprint/foo-%d/lib-foo.d", i), "x")
	}
	writeFile(t, root, "app/__pycache__/main.cpython-312.pyc", "x")
	writeFile(t, root, ".venv/lib/site-packages/pkg.py", "x")
	writeFile(t, root, "Venv/lib/pkg.py", "x")
	writeFile(t, root, ".terraform/providers/provider.json", "x")
	writeFile(t, root, ".pytest_cache/v/cache/nodeids", "x")
	writeFile(t, root, ".mypy_cache/3.12/mod.json", "x")
	writeFile(t, root, ".ruff_cache/content", "x")

	got, err := Scan(root, Options{MaxFiles: 3, MaxDepth: DefaultMaxDepth})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if want := []string{"app/main.py", "src/lib.rs"}; !reflect.DeepEqual(pathsOf(got.Files), want) {
		t.Fatalf("Files=%v want %v", pathsOf(got.Files), want)
	}
	if got.Truncated {
		t.Fatalf("Truncated=true: build-cache files consumed the file budget")
	}
}

// When git cannot run at all, Scan must not error and must behave exactly as outside a
// repository, even inside one: .gitignore is not consulted, the fixed list applies.
func TestScanFallsBackWhenGitUnavailable(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root, ".locust_env/\n")
	writeFile(t, root, "main.go", "package main\n")
	writeFile(t, root, ".locust_env/lib/x.py", "kept: git is not available to honor .gitignore")
	writeFile(t, root, "target/debug/app.d", "skipped by the fixed list")

	t.Setenv("PATH", t.TempDir())
	if paths, ok := gitIgnoredPaths(root); ok || paths != nil {
		t.Fatalf("gitIgnoredPaths without git = (%v, %v), want (nil, false)", paths, ok)
	}
	got, err := Scan(root, Options{MaxDepth: DefaultMaxDepth})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if want := []string{".gitignore", ".locust_env/lib/x.py", "main.go"}; !reflect.DeepEqual(pathsOf(got.Files), want) {
		t.Fatalf("Files=%v want %v", pathsOf(got.Files), want)
	}
}

// ShouldSkipDir is shared with glob, grep, list_directory and path autocomplete. The
// build/cache names Scan skips must stay visible there, so a question about a dependency in
// .venv or generated code under target can still be answered.
func TestShouldSkipDirLeavesScanOnlyDirsVisibleToTools(t *testing.T) {
	for _, name := range []string{"target", "__pycache__", ".venv", "venv", ".pytest_cache", ".terraform", ".mypy_cache", ".ruff_cache"} {
		if ShouldSkipDir(name) {
			t.Fatalf("ShouldSkipDir(%q)=true: tools would stop seeing it", name)
		}
		if !isScanOnlySkipDir(name) {
			t.Fatalf("isScanOnlySkipDir(%q)=false want true", name)
		}
	}
	for _, name := range []string{"Target", " target ", "__PYCACHE__", ".VENV"} {
		if !isScanOnlySkipDir(name) {
			t.Fatalf("isScanOnlySkipDir(%q)=false want true (case/whitespace-insensitive)", name)
		}
	}
	for _, name := range []string{"src", "targets", "venv2", "internal"} {
		if isScanOnlySkipDir(name) {
			t.Fatalf("isScanOnlySkipDir(%q)=true want false", name)
		}
	}
}

// A repository's core.fsmonitor can run an external command during status; the
// ignored-path lookup must not trigger it. Git for Windows hands both that
// command and the hook body to sh, which consumes backslashes. The script path
// stored in core.fsmonitor and the marker path inside the hook are quoted and
// use forward slashes: an unquoted backslash script path never starts the
// hook, and an unquoted backslash marker (touch C:\tmp\run\marker) creates
// C:tmprunmarker, so the file this test stats is never created. A status
// without the override must create the marker first; otherwise a hook that
// does not run would still pass.
func TestGitIgnoredPathsDisablesRepoFsmonitor(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root, ".locust_env/\n")
	writeFile(t, root, "README.md", "# x\n")
	writeFile(t, root, ".locust_env/lib/x.py", "x\n")

	marker := filepath.Join(root, ".fsmonitor-ran")
	script := filepath.Join(root, "fsmonitor-hook.sh")
	hook := "#!/bin/sh\ntouch '" + filepath.ToSlash(marker) + "'\n"
	if err := os.WriteFile(script, []byte(hook), 0o755); err != nil {
		t.Fatalf("write fsmonitor hook: %v", err)
	}
	runGit(t, root, "config", "core.fsmonitor", "'"+filepath.ToSlash(script)+"'")

	runGit(t, root, "status", "--porcelain=v1", "-z", "--untracked-files=normal", "--ignored=matching", ".")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("fsmonitor hook did not run without the override: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatalf("remove marker: %v", err)
	}

	if _, ok := gitIgnoredPaths(root); !ok {
		t.Fatal("gitIgnoredPaths failed")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("fsmonitor hook ran during gitIgnoredPaths (marker %q exists)", marker)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat marker: %v", err)
	}
}

// status.showUntrackedFiles=no makes git reject --ignored=matching unless
// --untracked-files=normal is passed explicitly; ignored paths must still come from git.
func TestScanHonorsRepoGitignoreWhenShowUntrackedFilesNo(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root, ".locust_env/\n")
	runGit(t, root, "config", "status.showUntrackedFiles", "no")
	writeFile(t, root, "README.md", "# Example\n")
	writeFile(t, root, "app/main.py", "print('hi')\n")
	writeFile(t, root, ".locust_env/lib/x.py", "vendored\n")

	got, err := Scan(root, Options{MaxDepth: DefaultMaxDepth})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	want := []string{".gitignore", "README.md", "app/main.py"}
	if !reflect.DeepEqual(pathsOf(got.Files), want) {
		t.Fatalf("Files=%v want %v", pathsOf(got.Files), want)
	}
}

// Git exports repository-location variables to hooks (githooks(5)). With them
// pointing at another repository, Scan must still apply root's own .gitignore
// and match a lookup made without those variables.
func TestScanIgnoresInheritedRepositoryLocation(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root, ".locust_env/\n")
	writeFile(t, root, "README.md", "# Example\n")
	writeFile(t, root, ".locust_env/lib/x.py", "vendored\n")

	clean, err := Scan(root, Options{MaxDepth: DefaultMaxDepth})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	other := t.TempDir()
	initGitRepo(t, other, "")
	writeFile(t, other, "README.md", "other\n")
	gitDir := filepath.Join(other, ".git")
	t.Setenv("GIT_DIR", gitDir)
	t.Setenv("GIT_WORK_TREE", other)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(gitDir, "index"))
	t.Setenv("GIT_COMMON_DIR", gitDir)
	t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(gitDir, "objects"))
	t.Setenv("GIT_ALTERNATE_OBJECT_DIRECTORIES", filepath.Join(gitDir, "objects"))

	got, err := Scan(root, Options{MaxDepth: DefaultMaxDepth})
	if err != nil {
		t.Fatalf("Scan with inherited repository location: %v", err)
	}
	if !reflect.DeepEqual(pathsOf(got.Files), pathsOf(clean.Files)) {
		t.Fatalf("Files=%v want clean environment %v", pathsOf(got.Files), pathsOf(clean.Files))
	}
}

// The ignored-path lookup runs every turn, often next to a git command the user or agent
// is running in the same repository. It must not take the optional index lock, and a
// GIT_OPTIONAL_LOCKS value inherited from the environment must not re-enable it.
func TestGitCommandDisablesOptionalLocks(t *testing.T) {
	t.Setenv("GIT_OPTIONAL_LOCKS", "1")
	cmd := gitCommand(context.Background(), t.TempDir(), "status")
	value := ""
	for _, entry := range cmd.Env {
		if after, ok := strings.CutPrefix(entry, "GIT_OPTIONAL_LOCKS="); ok {
			value = after
		}
	}
	if value != "0" {
		t.Fatalf("effective GIT_OPTIONAL_LOCKS=%q want \"0\"", value)
	}
}

// A rename or copy record carries its original path in the next NUL field, with no XY
// prefix. An original name that starts with "!! " must not be read as an ignored entry.
func TestParseIgnoredStatusSkipsRenameAndCopyOrigPaths(t *testing.T) {
	out := []byte("R  b\x00!! a\x00" +
		"C  d\x00!! c\x00" +
		" R e\x00!! f\x00" +
		"!! out/\x00" +
		"?? new.txt\x00" +
		" M main.go\x00" +
		"!! svc/build/\x00")

	if got, want := parseIgnoredStatus(out, ""), map[string]bool{"out": true, "svc/build": true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("prefix \"\": got %v want %v", got, want)
	}
	if got, want := parseIgnoredStatus(out, "svc/"), map[string]bool{"build": true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("prefix \"svc/\": got %v want %v", got, want)
	}
}

// End to end: after `git mv "!! a" b`, status reports "R  b\0!! a\0". The tracked file "a"
// must still be scanned.
func TestScanKeepsFileNamedLikeRenameOrigPath(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root, "")
	writeFile(t, root, "!! a", "moved\n")
	writeFile(t, root, "a", "kept\n")
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-q", "-m", "fixture")
	runGit(t, root, "mv", "!! a", "b")

	got, err := Scan(root, Options{MaxDepth: DefaultMaxDepth})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if want := []string{"a", "b"}; !reflect.DeepEqual(pathsOf(got.Files), want) {
		t.Fatalf("Files=%v want %v", pathsOf(got.Files), want)
	}
}
