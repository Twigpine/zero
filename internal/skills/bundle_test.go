package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const bundledSkillManifest = "---\nname: pdf\ndescription: fill PDF forms\n---\nRun scripts/fill.py with the form path.\n"

// writeBundledSourceSkill lays down a skill in the agentskills.io shape: a
// SKILL.md plus the scripts, references and assets it points at, and the hidden
// clutter a cloned source carries.
func writeBundledSourceSkill(t *testing.T, dir string) string {
	t.Helper()
	writeSourceSkill(t, dir, bundledSkillManifest)
	files := map[string]string{
		"scripts/fill.py":    "print('fill')\n",
		"references/form.md": "# Form fields\n",
		"assets/logo.svg":    "<svg/>\n",
		"LICENSE.txt":        "MIT\n",
		".git/config":        "[core]\n",
		".env":               "TOKEN=local-only\n",
		"scripts/.cache/tmp": "scratch\n",
	}
	for rel, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasPrefix(rel, "scripts/") {
			mode = 0o755
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestInstallCopiesTheSkillDirectory(t *testing.T) {
	destDir := t.TempDir()
	src := writeBundledSourceSkill(t, filepath.Join(t.TempDir(), "pdf"))

	result, err := Install(context.Background(), InstallOptions{Source: src, Dir: destDir})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	installed := filepath.Join(destDir, "pdf")
	for _, rel := range []string{"SKILL.md", "scripts/fill.py", "references/form.md", "assets/logo.svg", "LICENSE.txt"} {
		if _, err := os.Stat(filepath.Join(installed, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s was not installed: %v", rel, err)
		}
	}
	for _, rel := range []string{".git", ".env", "scripts/.cache"} {
		if _, err := os.Stat(filepath.Join(installed, filepath.FromSlash(rel))); err == nil {
			t.Errorf("hidden %s was installed; it is not part of the skill", rel)
		}
	}
	if !strings.HasPrefix(result.TreeHash, "sha256:") {
		t.Fatalf("TreeHash = %q, want a sha256 tree hash", result.TreeHash)
	}
	lock, err := ReadLock(destDir)
	if err != nil {
		t.Fatal(err)
	}
	if lock["pdf"].TreeHash != result.TreeHash || lock["pdf"].Hash != result.Hash {
		t.Fatalf("lock entry = %+v, want the install's hashes %q and %q", lock["pdf"], result.Hash, result.TreeHash)
	}
	if runtime.GOOS == "windows" {
		return
	}
	script, err := os.Stat(filepath.Join(installed, "scripts", "fill.py"))
	if err != nil {
		t.Fatal(err)
	}
	if script.Mode().Perm() != 0o755 {
		t.Errorf("installed script mode = %v, want 0755 so the skill can still run it", script.Mode().Perm())
	}
	reference, err := os.Stat(filepath.Join(installed, "references", "form.md"))
	if err != nil {
		t.Fatal(err)
	}
	if reference.Mode().Perm() != 0o644 {
		t.Errorf("installed reference mode = %v, want 0644", reference.Mode().Perm())
	}
}

func TestInstallSkipsSymlinksAndSaysSo(t *testing.T) {
	destDir := t.TempDir()
	src := writeBundledSourceSkill(t, filepath.Join(t.TempDir(), "pdf"))
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("not part of the skill\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(src, "references", "linked.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	result, err := Install(context.Background(), InstallOptions{Source: src, Dir: destDir})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(result.SkippedLinks) != 1 || result.SkippedLinks[0] != "references/linked.md" {
		t.Fatalf("SkippedLinks = %v, want [references/linked.md]", result.SkippedLinks)
	}
	if _, err := os.Lstat(filepath.Join(destDir, "pdf", "references", "linked.md")); err == nil {
		t.Fatal("a symlink from the source was installed")
	}
}

func TestInstallRefusesAnOversizedSkill(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files int
		bytes int64
		want  string
	}{
		{name: "too many files", files: 3, bytes: maxSkillTreeBytes, want: "more than 3 files"},
		{name: "too many bytes", files: maxSkillTreeFiles, bytes: 1 << 20, want: "larger than 1 MiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files, bytes := maxSkillTreeFiles, maxSkillTreeBytes
			t.Cleanup(func() { maxSkillTreeFiles, maxSkillTreeBytes = files, bytes })
			maxSkillTreeFiles, maxSkillTreeBytes = tc.files, tc.bytes

			destDir := t.TempDir()
			src := writeBundledSourceSkill(t, filepath.Join(t.TempDir(), "pdf"))
			big := make([]byte, 2<<20)
			if err := os.WriteFile(filepath.Join(src, "assets", "big.bin"), big, 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Install(context.Background(), InstallOptions{Source: src, Dir: destDir})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Install error = %v, want %q", err, tc.want)
			}
			if _, statErr := os.Stat(filepath.Join(destDir, "pdf")); statErr == nil {
				t.Fatal("a refused install left a skill directory behind")
			}
		})
	}
}

func TestReinstallWithOnlyABundledFileChangedIsAnUpdate(t *testing.T) {
	destDir := t.TempDir()
	src := writeBundledSourceSkill(t, filepath.Join(t.TempDir(), "pdf"))
	first, err := Install(context.Background(), InstallOptions{Source: src, Dir: destDir})
	if err != nil {
		t.Fatalf("first Install: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "scripts", "fill.py"), []byte("print('fill v2')\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	second, err := Install(context.Background(), InstallOptions{Source: src, Dir: destDir})
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if !second.Updated {
		t.Fatal("a reinstall that changed a bundled script was not reported as an update")
	}
	if second.Hash != first.Hash || second.TreeHash == first.TreeHash {
		t.Fatalf("hashes = %q/%q after %q/%q, want the same SKILL.md hash and a new tree hash", second.Hash, second.TreeHash, first.Hash, first.TreeHash)
	}
	got, err := os.ReadFile(filepath.Join(destDir, "pdf", "scripts", "fill.py"))
	if err != nil || string(got) != "print('fill v2')\n" {
		t.Fatalf("installed script = %q, %v; want the new version", got, err)
	}
}

func TestInfoReportsDriftInABundledFile(t *testing.T) {
	destDir := t.TempDir()
	src := writeBundledSourceSkill(t, filepath.Join(t.TempDir(), "pdf"))
	if _, err := Install(context.Background(), InstallOptions{Source: src, Dir: destDir}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if info, ok := Info(destDir, "pdf"); !ok || info.HashDrift {
		t.Fatalf("a fresh install reports drift: %+v, %v", info, ok)
	}
	script := filepath.Join(destDir, "pdf", "scripts", "fill.py")
	if err := os.WriteFile(script, []byte("print('edited by hand')\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if info, _ := Info(destDir, "pdf"); !info.HashDrift {
		t.Fatal("an edited bundled script is not reported as drift")
	}
}

// A lock entry written before installs carried their bundled files has no tree
// hash. Checking its tree would report every such skill as drifted, so only
// its SKILL.md is checked, as before.
func TestLockEntryWithoutATreeHashChecksOnlySKILLmd(t *testing.T) {
	destDir := t.TempDir()
	src := writeBundledSourceSkill(t, filepath.Join(t.TempDir(), "pdf"))
	if _, err := Install(context.Background(), InstallOptions{Source: src, Dir: destDir}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	lock, err := ReadLock(destDir)
	if err != nil {
		t.Fatal(err)
	}
	entry := lock["pdf"]
	entry.TreeHash = ""
	lock["pdf"] = entry
	data, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destDir, LockFileName), data, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(destDir, "pdf", "scripts", "fill.py"), []byte("print('edited')\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if info, _ := Info(destDir, "pdf"); info.HashDrift {
		t.Fatal("an entry without a tree hash reported drift for a bundled file")
	}
	if err := os.WriteFile(filepath.Join(destDir, "pdf", skillFileName), []byte(bundledSkillManifest+"edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if info, _ := Info(destDir, "pdf"); !info.HashDrift {
		t.Fatal("an edited SKILL.md under an old lock entry is not reported as drift")
	}
}

func TestToolOutputSaysWhereTheBundledFilesAre(t *testing.T) {
	root := t.TempDir()
	writeBundledSourceSkill(t, filepath.Join(root, "pdf"))
	skill, ok := Get(root, "pdf")
	if !ok {
		t.Fatal("skill not loaded")
	}

	output := ToolOutput(skill)
	if !strings.HasPrefix(output, strings.TrimRight(skill.Content, "\n")) {
		t.Fatalf("the instructions do not come first:\n%s", output)
	}
	if !strings.Contains(output, filepath.Dir(skill.Path)) {
		t.Fatalf("the output does not say where the skill's files are:\n%s", output)
	}
	for _, want := range []string{"- LICENSE.txt", "- assets/logo.svg", "- references/form.md", "- scripts/fill.py"} {
		if !strings.Contains(output, want+"\n") {
			t.Errorf("output does not list %q:\n%s", want, output)
		}
	}
	for _, hidden := range []string{".env", ".git", ".cache", "SKILL.md\n"} {
		if strings.Contains(output, "- "+hidden) {
			t.Errorf("output lists %q:\n%s", hidden, output)
		}
	}
}

func TestToolOutputCapsALongListing(t *testing.T) {
	root := t.TempDir()
	dir := writeSourceSkill(t, filepath.Join(root, "many"), "---\nname: many\n---\nbody\n")
	for i := 0; i < bundledFileListLimit+7; i++ {
		path := filepath.Join(dir, "references", fmt.Sprintf("part-%03d.md", i))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	skill, ok := Get(root, "many")
	if !ok {
		t.Fatal("skill not loaded")
	}
	output := ToolOutput(skill)
	if got := strings.Count(output, "\n- references/"); got != bundledFileListLimit {
		t.Fatalf("listed %d files, want the first %d:\n%s", got, bundledFileListLimit, output)
	}
	if !strings.Contains(output, "- (and 7 more)\n") {
		t.Fatalf("output does not say how many more there are:\n%s", output)
	}
}

func TestToolOutputOfAManifestOnlySkillIsItsContent(t *testing.T) {
	root := t.TempDir()
	writeSourceSkill(t, filepath.Join(root, "plain"), "---\nname: plain\n---\nJust instructions.\n")
	skill, ok := Get(root, "plain")
	if !ok {
		t.Fatal("skill not loaded")
	}
	if got := ToolOutput(skill); got != skill.Content {
		t.Fatalf("ToolOutput = %q, want the unchanged content %q", got, skill.Content)
	}
}
