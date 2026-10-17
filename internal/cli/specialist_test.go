package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/specialist"
)

func TestRunSpecialistListShowAndPath(t *testing.T) {
	cwd := t.TempDir()
	configRoot := setSpecialistConfigRoot(t)
	userDir := filepath.Join(configRoot, "zero", "specialists")
	writeSpecialistManifest(t, filepath.Join(userDir, "triage.md"), `---
name: triage
description: Triage failing tests
tools: [read-only]
---
Find the likely failure area.`)
	deps := appDeps{getwd: func() (string, error) { return cwd, nil }}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"specialist", "list"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	for _, want := range []string{"Zero Specialists", "worker [builtin]", "triage [user]", "code-review"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("list output missing %q: %s", want, stdout.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = runWithDeps([]string{"specialist", "show", "triage"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Zero Specialist: triage") || !strings.Contains(stdout.String(), "Find the likely failure area.") {
		t.Fatalf("unexpected show output: %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = runWithDeps([]string{"specialist", "path"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), userDir) || !strings.Contains(stdout.String(), filepath.Join(cwd, ".zero", "specialists")) {
		t.Fatalf("unexpected path output: %s", stdout.String())
	}
}

func TestRunSpecialistShowAndPathJSON(t *testing.T) {
	cwd := t.TempDir()
	setSpecialistConfigRoot(t)
	deps := appDeps{getwd: func() (string, error) { return cwd, nil }}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"specialist", "show", "worker", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("show --json exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	var showPayload struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Location string `json:"location"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &showPayload); err != nil {
		t.Fatalf("failed to decode show JSON: %v\n%s", err, stdout.String())
	}
	if showPayload.Metadata.Name != "worker" || showPayload.Location == "" {
		t.Fatalf("unexpected show JSON: %#v", showPayload)
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = runWithDeps([]string{"specialist", "path", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("path --json exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	var pathPayload struct {
		UserDir    string `json:"userDir"`
		ProjectDir string `json:"projectDir"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &pathPayload); err != nil {
		t.Fatalf("failed to decode path JSON: %v\n%s", err, stdout.String())
	}
	if pathPayload.UserDir == "" || pathPayload.ProjectDir != filepath.Join(cwd, ".zero", "specialists") {
		t.Fatalf("unexpected path JSON: %#v", pathPayload)
	}
}

func TestRunSpecialistListJSON(t *testing.T) {
	cwd := t.TempDir()
	setSpecialistConfigRoot(t)
	deps := appDeps{getwd: func() (string, error) { return cwd, nil }}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"specialists", "list", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	var payload struct {
		Specialists []struct {
			Name     string `json:"name"`
			Location string `json:"location"`
		} `json:"specialists"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("failed to decode JSON: %v\n%s", err, stdout.String())
	}
	if len(payload.Specialists) == 0 {
		t.Fatalf("unexpected JSON payload: %#v", payload)
	}
	for _, item := range payload.Specialists {
		if item.Name == "" || item.Location == "" {
			t.Fatalf("specialist JSON item missing name or location: %#v", item)
		}
	}
}

func TestRunSpecialistCreateDeleteAndEdit(t *testing.T) {
	cwd := t.TempDir()
	configRoot := setSpecialistConfigRoot(t)
	userDir := filepath.Join(configRoot, "zero", "specialists")
	deps := appDeps{getwd: func() (string, error) { return cwd, nil }}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{
		"specialist", "create", "triage",
		"--description", "Triage failures",
		"--prompt", "Find the most likely failure.",
		"--tools", "read-only,plan",
	}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("create exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	path := filepath.Join(userDir, "triage.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected specialist file: %v", err)
	}
	content := string(data)
	for _, want := range []string{`name: "triage"`, `description: "Triage failures"`, `"read-only"`, `"plan"`, "Find the most likely failure."} {
		if !strings.Contains(content, want) {
			t.Fatalf("created file missing %q:\n%s", want, content)
		}
	}

	var editedPath string
	deps.runEditor = func(path string) error {
		editedPath = path
		return nil
	}
	stdout.Reset()
	stderr.Reset()
	exitCode = runWithDeps([]string{"specialist", "edit", "triage"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("edit exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	if editedPath != path {
		t.Fatalf("edited path = %q, want %q", editedPath, path)
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = runWithDeps([]string{"specialist", "delete", "triage", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("delete exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected specialist file deleted, stat err=%v", err)
	}
	if !strings.Contains(stdout.String(), `"deleted": true`) {
		t.Fatalf("delete JSON missing deleted=true: %s", stdout.String())
	}
}

func TestWriteSpecialistCreateResultSurfacesWarningsOnlyForHumanOutput(t *testing.T) {
	manifest := specialist.Manifest{
		Metadata: specialist.Metadata{Name: "triage"},
		FilePath: "/specialists/triage.md",
		Warnings: []string{"replacement backup /specialists/.triage.bak retained: access denied"},
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := writeSpecialistCreateResult(manifest, false, &stdout, &stderr); code != exitSuccess {
		t.Fatalf("human output exit code = %d", code)
	}
	if got, want := stdout.String(), "Created specialist triage at /specialists/triage.md\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got, want := stderr.String(), "warning: replacement backup /specialists/.triage.bak retained: access denied\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}

	stdout.Reset()
	stderr.Reset()
	if code := writeSpecialistCreateResult(manifest, true, &stdout, &stderr); code != exitSuccess {
		t.Fatalf("JSON output exit code = %d", code)
	}
	var payload specialist.Manifest
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, stdout.String())
	}
	if len(payload.Warnings) != 1 || payload.Warnings[0] != manifest.Warnings[0] {
		t.Fatalf("JSON warnings = %#v", payload.Warnings)
	}
	if stderr.Len() != 0 {
		t.Fatalf("JSON warning duplicated to stderr: %q", stderr.String())
	}
}

func TestRunSpecialistEditRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs extra privileges on Windows")
	}
	cwd := t.TempDir()
	configRoot := setSpecialistConfigRoot(t)
	userDir := filepath.Join(configRoot, "zero", "specialists")
	if err := os.MkdirAll(userDir, 0o700); err != nil {
		t.Fatalf("create specialist dir: %v", err)
	}
	target := filepath.Join(t.TempDir(), "target.md")
	if err := os.WriteFile(target, []byte("do not edit"), 0o600); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	link := filepath.Join(userDir, "linked.md")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("create symlink: %v", err)
	}
	deps := appDeps{
		getwd: func() (string, error) { return cwd, nil },
		runEditor: func(path string) error {
			t.Fatalf("editor should not be invoked for symlink path %s", path)
			return nil
		},
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"specialist", "edit", "linked"}, &stdout, &stderr, deps)

	if exitCode != exitUsage {
		t.Fatalf("exitCode = %d, want usage; stderr=%s", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "refusing to edit symlink") {
		t.Fatalf("expected symlink refusal, got %q", stderr.String())
	}
}

func TestRunSpecialistCreateProjectAndRejectDuplicate(t *testing.T) {
	cwd := t.TempDir()
	setSpecialistConfigRoot(t)
	deps := appDeps{getwd: func() (string, error) { return cwd, nil }}
	args := []string{"specialist", "create", "project-helper", "--project", "--description=Project helper"}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps(args, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("create project exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(cwd, ".zero", "specialists", "project-helper.md")); err != nil {
		t.Fatalf("expected project specialist file: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = runWithDeps(args, &stdout, &stderr, deps)
	if exitCode != exitUsage {
		t.Fatalf("duplicate exitCode = %d, want usage", exitCode)
	}
	if !strings.Contains(stderr.String(), "already exists") {
		t.Fatalf("duplicate error = %q", stderr.String())
	}
}

func TestRunSpecialistShowMissingReturnsUsage(t *testing.T) {
	setSpecialistConfigRoot(t)
	deps := appDeps{getwd: func() (string, error) { return t.TempDir(), nil }}
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"specialist", "show", "missing"}, &stdout, &stderr, deps)

	if exitCode != exitUsage {
		t.Fatalf("exitCode = %d, want usage", exitCode)
	}
	if !strings.Contains(stderr.String(), "not found") {
		t.Fatalf("expected not found error, got %q", stderr.String())
	}
}

func TestRunSpecialistUnknownCommandDoesNotResolveWorkspace(t *testing.T) {
	deps := appDeps{getwd: func() (string, error) {
		t.Fatal("getwd should not be called for an unknown specialist command")
		return "", nil
	}}
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"specialist", "missing"}, &stdout, &stderr, deps)

	if exitCode != exitUsage {
		t.Fatalf("exitCode = %d, want usage", exitCode)
	}
	if !strings.Contains(stderr.String(), `unknown specialist command "missing"`) {
		t.Fatalf("expected unknown command error, got %q", stderr.String())
	}
}

func TestRunSpecialistArgCountErrors(t *testing.T) {
	setSpecialistConfigRoot(t)
	deps := appDeps{getwd: func() (string, error) {
		t.Fatal("getwd should not be called for invalid specialist arguments")
		return "", nil
	}}
	tests := []struct {
		args []string
		want string
	}{
		{args: []string{"specialist", "list", "extra"}, want: "does not accept positional"},
		{args: []string{"specialist", "show"}, want: "show requires a specialist name"},
		{args: []string{"specialist", "create"}, want: "create requires a specialist name"},
		{args: []string{"specialist", "delete"}, want: "delete requires a specialist name"},
		{args: []string{"specialist", "edit"}, want: "edit requires a specialist name"},
		{args: []string{"specialist", "path", "extra"}, want: "path does not accept positional"},
	}
	for _, tc := range tests {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			exitCode := runWithDeps(tc.args, &stdout, &stderr, deps)
			if exitCode != exitUsage {
				t.Fatalf("exitCode = %d, want usage", exitCode)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("expected %q error, got %q", tc.want, stderr.String())
			}
		})
	}
}

func setSpecialistConfigRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	switch runtime.GOOS {
	case "windows":
		t.Setenv("APPDATA", root)
	case "darwin":
		t.Setenv("HOME", root)
	default:
		t.Setenv("XDG_CONFIG_HOME", root)
	}
	configRoot, err := config.UserConfigDir()
	if err != nil {
		t.Fatalf("UserConfigDir() error = %v", err)
	}
	return configRoot
}

func writeSpecialistManifest(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create manifest dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}
