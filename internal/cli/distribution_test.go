package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/skills"
)

func writeSourceSkillDir(t *testing.T, dir string, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	return dir
}

func writeSourcePluginDir(t *testing.T, dir string, manifest map[string]any) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), data, 0o644); err != nil {
		t.Fatalf("write plugin.json: %v", err)
	}
	return dir
}

// --- skill add/info/remove ---

func TestRunSkillAddInfoRemove(t *testing.T) {
	skillsDir := t.TempDir()
	src := writeSourceSkillDir(t, filepath.Join(t.TempDir(), "src"),
		"---\nname: confirmation-policy\ndescription: Ask first.\n---\nbody\n")
	// Install records the canonical (symlink-resolved) source, which "info" then
	// shows. Normalize here too so the assertion matches on every platform — on
	// Windows the temp dir is an 8.3 short path EvalSymlinks expands, and on macOS
	// it resolves /var -> /private/var.
	if resolved, err := filepath.EvalSymlinks(src); err == nil {
		src = resolved
	}

	deps := appDeps{skillsDir: func() string { return skillsDir }}

	// add
	var stdout, stderr bytes.Buffer
	if exit := runWithDeps([]string{"skill", "add", src}, &stdout, &stderr, deps); exit != 0 {
		t.Fatalf("skill add exit = %d, stderr = %s", exit, stderr.String())
	}
	if !strings.Contains(stdout.String(), "confirmation-policy") {
		t.Fatalf("add output missing name:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "sha256:") {
		t.Fatalf("add output should show the recorded hash:\n%s", stdout.String())
	}

	// list reflects the install
	stdout.Reset()
	stderr.Reset()
	if exit := runWithDeps([]string{"skill", "list"}, &stdout, &stderr, deps); exit != 0 {
		t.Fatalf("skill list exit = %d, stderr = %s", exit, stderr.String())
	}
	if !strings.Contains(stdout.String(), "confirmation-policy") {
		t.Fatalf("list missing installed skill:\n%s", stdout.String())
	}

	// info shows source + hash
	stdout.Reset()
	stderr.Reset()
	if exit := runWithDeps([]string{"skill", "info", "confirmation-policy"}, &stdout, &stderr, deps); exit != 0 {
		t.Fatalf("skill info exit = %d, stderr = %s", exit, stderr.String())
	}
	info := stdout.String()
	if !strings.Contains(info, src) || !strings.Contains(info, "sha256:") {
		t.Fatalf("info missing source/hash:\n%s", info)
	}
	if !strings.Contains(info, "hash drift: false") {
		t.Fatalf("info missing hash drift status:\n%s", info)
	}

	stdout.Reset()
	stderr.Reset()
	if exit := runWithDeps([]string{"skill", "info", "confirmation-policy", "--json"}, &stdout, &stderr, deps); exit != 0 {
		t.Fatalf("skill info --json exit = %d, stderr = %s", exit, stderr.String())
	}
	var beforeJSON map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &beforeJSON); err != nil {
		t.Fatalf("parse skill info json: %v\n%s", err, stdout.String())
	}
	if beforeJSON["hashDrift"] != false {
		t.Fatalf("hashDrift before edit = %#v, want false", beforeJSON["hashDrift"])
	}

	skillPath := filepath.Join(skillsDir, "confirmation-policy", "SKILL.md")
	if err := os.WriteFile(skillPath, []byte("---\ndescription: edited\n---\nchanged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if exit := runWithDeps([]string{"skill", "info", "confirmation-policy"}, &stdout, &stderr, deps); exit != 0 {
		t.Fatalf("skill info after edit exit = %d, stderr = %s", exit, stderr.String())
	}
	if !strings.Contains(stdout.String(), "hash drift: true") {
		t.Fatalf("info missing hash drift after edit:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if exit := runWithDeps([]string{"skill", "info", "confirmation-policy", "--json"}, &stdout, &stderr, deps); exit != 0 {
		t.Fatalf("skill info --json after edit exit = %d, stderr = %s", exit, stderr.String())
	}
	var afterJSON map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &afterJSON); err != nil {
		t.Fatalf("parse skill info json after edit: %v\n%s", err, stdout.String())
	}
	if afterJSON["hashDrift"] != true {
		t.Fatalf("hashDrift after edit = %#v, want true", afterJSON["hashDrift"])
	}

	// remove deletes it
	stdout.Reset()
	stderr.Reset()
	if exit := runWithDeps([]string{"skill", "remove", "confirmation-policy"}, &stdout, &stderr, deps); exit != 0 {
		t.Fatalf("skill remove exit = %d, stderr = %s", exit, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(skillsDir, "confirmation-policy")); err == nil {
		t.Fatalf("skill dir should be gone after remove")
	}
}

func TestRunSkillAddRejectsInvalid(t *testing.T) {
	skillsDir := t.TempDir()
	bad := filepath.Join(t.TempDir(), "empty")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	exit := runWithDeps([]string{"skill", "add", bad}, &stdout, &stderr, appDeps{skillsDir: func() string { return skillsDir }})
	if exit == 0 {
		t.Fatalf("expected a non-zero exit for an invalid skill source")
	}
}

func TestRunSkillAddClashIsNotSilentlyOverwritten(t *testing.T) {
	skillsDir := t.TempDir()
	deps := appDeps{skillsDir: func() string { return skillsDir }}
	srcA := writeSourceSkillDir(t, filepath.Join(t.TempDir(), "a"), "---\nname: shared\ndescription: a\n---\noriginal\n")
	srcB := writeSourceSkillDir(t, filepath.Join(t.TempDir(), "b"), "---\nname: shared\ndescription: b\n---\nreplacement\n")

	var stdout, stderr bytes.Buffer
	if exit := runWithDeps([]string{"skill", "add", srcA}, &stdout, &stderr, deps); exit != 0 {
		t.Fatalf("seed add failed: %s", stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	exit := runWithDeps([]string{"skill", "add", srcB}, &stdout, &stderr, deps)
	if exit == 0 {
		t.Fatalf("expected clash to fail without --force")
	}
	if !strings.Contains(strings.ToLower(stderr.String()), "force") {
		t.Fatalf("clash error should mention --force:\n%s", stderr.String())
	}
	// --force overwrites
	stdout.Reset()
	stderr.Reset()
	if exit := runWithDeps([]string{"skill", "add", srcB, "--force"}, &stdout, &stderr, deps); exit != 0 {
		t.Fatalf("forced add failed: %s", stderr.String())
	}
}

func TestRunSkillAddReinstallShowsHashChange(t *testing.T) {
	skillsDir := t.TempDir()
	deps := appDeps{skillsDir: func() string { return skillsDir }}
	src := filepath.Join(t.TempDir(), "src")
	writeSourceSkillDir(t, src, "---\nname: demo\ndescription: v1\n---\nfirst\n")

	var stdout, stderr bytes.Buffer
	if exit := runWithDeps([]string{"skill", "add", src}, &stdout, &stderr, deps); exit != 0 {
		t.Fatalf("first add: %s", stderr.String())
	}
	writeSourceSkillDir(t, src, "---\nname: demo\ndescription: v2\n---\nsecond\n")
	stdout.Reset()
	stderr.Reset()
	if exit := runWithDeps([]string{"skill", "add", src}, &stdout, &stderr, deps); exit != 0 {
		t.Fatalf("reinstall: %s", stderr.String())
	}
	// The output should communicate that the hash changed (an update).
	if !strings.Contains(strings.ToLower(stdout.String()), "updated") {
		t.Fatalf("reinstall output should report an update:\n%s", stdout.String())
	}
}

// --- plugin add/remove ---

func TestRunPluginAddListRemove(t *testing.T) {
	pluginsDir := t.TempDir()
	src := writeSourcePluginDir(t, filepath.Join(t.TempDir(), "src"), map[string]any{
		"schemaVersion": float64(1),
		"id":            "zero.demo",
		"name":          "Zero Demo",
		"version":       "0.1.0",
	})
	deps := appDeps{pluginsDir: func() string { return pluginsDir }}

	var stdout, stderr bytes.Buffer
	if exit := runWithDeps([]string{"plugin", "add", src}, &stdout, &stderr, deps); exit != 0 {
		t.Fatalf("plugin add exit = %d, stderr = %s", exit, stderr.String())
	}
	if !strings.Contains(stdout.String(), "zero.demo") {
		t.Fatalf("add output missing id:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if exit := runWithDeps([]string{"plugin", "info", "zero.demo"}, &stdout, &stderr, deps); exit != 0 {
		t.Fatalf("plugin info exit = %d, stderr = %s", exit, stderr.String())
	}
	if !strings.Contains(stdout.String(), "zero.demo") || !strings.Contains(stdout.String(), "manifest:") {
		t.Fatalf("info output missing plugin details:\n%s", stdout.String())
	}

	// remove
	stdout.Reset()
	stderr.Reset()
	if exit := runWithDeps([]string{"plugin", "remove", "zero.demo"}, &stdout, &stderr, deps); exit != 0 {
		t.Fatalf("plugin remove exit = %d, stderr = %s", exit, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(pluginsDir, "zero.demo")); err == nil {
		t.Fatalf("plugin dir should be gone after remove")
	}
}

func TestRunPluginAddRejectsInvalidManifest(t *testing.T) {
	pluginsDir := t.TempDir()
	src := writeSourcePluginDir(t, filepath.Join(t.TempDir(), "bad"), map[string]any{"schemaVersion": float64(1)})
	var stdout, stderr bytes.Buffer
	exit := runWithDeps([]string{"plugin", "add", src}, &stdout, &stderr, appDeps{pluginsDir: func() string { return pluginsDir }})
	if exit == 0 {
		t.Fatalf("expected a non-zero exit for an invalid manifest")
	}
}

// --- tools make/list ---

func TestRunToolsMakeAndList(t *testing.T) {
	toolsDir := t.TempDir()
	deps := appDeps{toolsDir: func() string { return toolsDir }}

	var stdout, stderr bytes.Buffer
	if exit := runWithDeps([]string{"tools", "make", "foo"}, &stdout, &stderr, deps); exit != 0 {
		t.Fatalf("tools make exit = %d, stderr = %s", exit, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "foo") {
		t.Fatalf("make output missing name:\n%s", out)
	}
	// The path and next steps should be printed.
	if !strings.Contains(out, filepath.Join(toolsDir, "foo")) {
		t.Fatalf("make output should print the created path:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(toolsDir, "foo", "plugin.json")); err != nil {
		t.Fatalf("manifest not created: %v", err)
	}

	// list reflects the scaffolded tool
	stdout.Reset()
	stderr.Reset()
	if exit := runWithDeps([]string{"tools", "list"}, &stdout, &stderr, deps); exit != 0 {
		t.Fatalf("tools list exit = %d, stderr = %s", exit, stderr.String())
	}
	if !strings.Contains(stdout.String(), "foo") {
		t.Fatalf("tools list missing scaffolded tool:\n%s", stdout.String())
	}
}

func TestRunToolsMakeRejectsBadName(t *testing.T) {
	toolsDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	exit := runWithDeps([]string{"tools", "make", "../escape"}, &stdout, &stderr, appDeps{toolsDir: func() string { return toolsDir }})
	if exit == 0 {
		t.Fatalf("expected a non-zero exit for an invalid tool name")
	}
}

func TestRunToolsMakeRequiresName(t *testing.T) {
	toolsDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	exit := runWithDeps([]string{"tools", "make"}, &stdout, &stderr, appDeps{toolsDir: func() string { return toolsDir }})
	if exit == 0 {
		t.Fatalf("expected a non-zero exit when no name is given")
	}
}

// A broken plugin in the toolbox dir must not vanish silently: tools list surfaces
// the loader diagnostic in both text and JSON output.
func TestRunToolsListSurfacesDiagnostics(t *testing.T) {
	toolsDir := t.TempDir()
	// A plugin dir whose plugin.json is not valid JSON yields a load diagnostic.
	broken := filepath.Join(toolsDir, "broken")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "plugin.json"), []byte("{ not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	deps := appDeps{toolsDir: func() string { return toolsDir }}

	var stdout, stderr bytes.Buffer
	if exit := runWithDeps([]string{"tools", "list"}, &stdout, &stderr, deps); exit != exitSuccess {
		t.Fatalf("tools list exit = %d, stderr = %s", exit, stderr.String())
	}
	if !strings.Contains(strings.ToLower(stdout.String()), "diagnostic") {
		t.Fatalf("text listing should surface loader diagnostics:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if exit := runWithDeps([]string{"tools", "list", "--json"}, &stdout, &stderr, deps); exit != exitSuccess {
		t.Fatalf("tools list --json exit = %d, stderr = %s", exit, stderr.String())
	}
	var payload struct {
		Tools       []map[string]any `json:"tools"`
		Diagnostics []map[string]any `json:"diagnostics"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("tools list --json not valid JSON: %v\n%s", err, stdout.String())
	}
	if len(payload.Diagnostics) == 0 {
		t.Fatalf("JSON listing should include loader diagnostics:\n%s", stdout.String())
	}
}

func TestRunRemoveRejectsJSONFlag(t *testing.T) {
	for _, args := range [][]string{
		{"skill", "remove", "demo", "--json"},
		{"plugin", "remove", "zero.demo", "--json"},
	} {
		var stdout, stderr bytes.Buffer
		exit := runWithDeps(args, &stdout, &stderr, appDeps{
			skillsDir:  func() string { return t.TempDir() },
			pluginsDir: func() string { return t.TempDir() },
		})
		if exit == exitSuccess {
			t.Fatalf("%v should reject --json, got success", args)
		}
		if !strings.Contains(strings.ToLower(stderr.String()), "json") {
			t.Fatalf("%v error should mention json:\n%s", args, stderr.String())
		}
	}
}

func TestRunToolsListEmpty(t *testing.T) {
	toolsDir := filepath.Join(t.TempDir(), "missing")
	var stdout, stderr bytes.Buffer
	exit := runWithDeps([]string{"tools", "list"}, &stdout, &stderr, appDeps{toolsDir: func() string { return toolsDir }})
	if exit != 0 {
		t.Fatalf("tools list on empty dir exit = %d, stderr = %s", exit, stderr.String())
	}
	if !strings.Contains(strings.ToLower(stdout.String()), "no") {
		t.Fatalf("expected an empty-toolbox message:\n%s", stdout.String())
	}
}

// Distribution must never launch the interactive TUI: each command resolves to
// its own dispatch path and returns without invoking the TUI launcher (which is
// left nil here, so a stray launch would panic).
func TestRunDistributionCommandsDoNotLaunchTUI(t *testing.T) {
	for _, args := range [][]string{
		{"skill", "list"},
		{"plugin", "list"},
		{"tools", "list"},
	} {
		var stdout, stderr bytes.Buffer
		exit := runWithDeps(args, &stdout, &stderr, appDeps{
			skillsDir:  func() string { return t.TempDir() },
			pluginsDir: func() string { return t.TempDir() },
			toolsDir:   func() string { return t.TempDir() },
			getwd:      func() (string, error) { return t.TempDir(), nil },
		})
		if exit != exitSuccess {
			t.Fatalf("%v exit = %d, stderr = %s", args, exit, stderr.String())
		}
	}
}

// A reinstall that only changed a bundled script must still read as an update
// (#584): the SKILL.md hash is the same, so "hash: X -> X" would say nothing.
func TestRunSkillAddReportsAnUpdateToBundledFilesOnly(t *testing.T) {
	skillsDir := t.TempDir()
	src := writeSourceSkillDir(t, filepath.Join(t.TempDir(), "src"),
		"---\nname: pdf\ndescription: Fill forms.\n---\nRun scripts/fill.py.\n")
	script := filepath.Join(src, "scripts", "fill.py")
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("print('v1')\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	deps := appDeps{skillsDir: func() string { return skillsDir }}

	var stdout, stderr bytes.Buffer
	if exit := runWithDeps([]string{"skill", "add", src}, &stdout, &stderr, deps); exit != 0 {
		t.Fatalf("first skill add exit = %d, stderr = %s", exit, stderr.String())
	}
	if err := os.WriteFile(script, []byte("print('v2')\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if exit := runWithDeps([]string{"skill", "add", src}, &stdout, &stderr, deps); exit != 0 {
		t.Fatalf("second skill add exit = %d, stderr = %s", exit, stderr.String())
	}
	for _, want := range []string{`Updated skill "pdf".`, "SKILL.md is unchanged; the files bundled with it changed"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("second add output does not say %q:\n%s", want, stdout.String())
		}
	}
}

// An install from before tree hashes copied SKILL.md alone and recorded no tree
// hash. Adding the skill again over that entry must not claim bundled files
// changed: there are none, or they are installed for the first time.
func TestRunSkillAddOverAnEntryWithoutATreeHash(t *testing.T) {
	for _, tc := range []struct {
		name    string
		script  bool
		want    []string
		notWant []string
	}{
		{
			name:    "SKILL.md only",
			want:    []string{`Installed skill "pdf".`},
			notWant: []string{"Updated skill", "files bundled with it"},
		},
		{
			name:    "with a bundled script",
			script:  true,
			want:    []string{`Updated skill "pdf".`, "SKILL.md is unchanged; the files bundled with it are now installed too"},
			notWant: []string{"files bundled with it changed"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			skillsDir := t.TempDir()
			src := writeSourceSkillDir(t, filepath.Join(t.TempDir(), "src"),
				"---\nname: pdf\ndescription: Fill forms.\n---\nRun scripts/fill.py.\n")
			if tc.script {
				script := filepath.Join(src, "scripts", "fill.py")
				if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(script, []byte("print('fill')\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			deps := appDeps{skillsDir: func() string { return skillsDir }}

			var stdout, stderr bytes.Buffer
			if exit := runWithDeps([]string{"skill", "add", src}, &stdout, &stderr, deps); exit != 0 {
				t.Fatalf("first skill add exit = %d, stderr = %s", exit, stderr.String())
			}
			lock, err := skills.ReadLock(skillsDir)
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
			if err := os.WriteFile(filepath.Join(skillsDir, skills.LockFileName), data, 0o644); err != nil {
				t.Fatal(err)
			}

			stdout.Reset()
			stderr.Reset()
			if exit := runWithDeps([]string{"skill", "add", src}, &stdout, &stderr, deps); exit != 0 {
				t.Fatalf("second skill add exit = %d, stderr = %s", exit, stderr.String())
			}
			for _, want := range tc.want {
				if !strings.Contains(stdout.String(), want) {
					t.Fatalf("second add output does not say %q:\n%s", want, stdout.String())
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(stdout.String(), notWant) {
					t.Fatalf("second add output says %q:\n%s", notWant, stdout.String())
				}
			}
		})
	}
}

func TestRunSkillAddListsTheSymlinksItDidNotCopy(t *testing.T) {
	skillsDir := t.TempDir()
	src := writeSourceSkillDir(t, filepath.Join(t.TempDir(), "src"),
		"---\nname: pdf\ndescription: Fill forms.\n---\nbody\n")
	outside := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(src, "notes.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	var stdout, stderr bytes.Buffer
	deps := appDeps{skillsDir: func() string { return skillsDir }}
	if exit := runWithDeps([]string{"skill", "add", src}, &stdout, &stderr, deps); exit != 0 {
		t.Fatalf("skill add exit = %d, stderr = %s", exit, stderr.String())
	}
	if !strings.Contains(stdout.String(), "not copied (symlinks): notes.md") {
		t.Fatalf("add output does not list the skipped symlink:\n%s", stdout.String())
	}
}
