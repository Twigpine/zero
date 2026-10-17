package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/localcontrol"
)

type fakeArtifactRunner struct {
	args   []string
	stdout string
}

func (runner *fakeArtifactRunner) Run(_ context.Context, path string, args []string, _ []string, _ time.Duration) (localcontrol.CommandResult, error) {
	runner.args = append([]string(nil), args...)
	for _, arg := range args {
		switch filepath.Ext(arg) {
		case ".png", ".pdf":
			_ = os.WriteFile(arg, []byte("artifact"), 0o600)
		}
	}
	stdout := runner.stdout
	if stdout == "" {
		stdout = "captured\n"
	}
	return localcontrol.CommandResult{
		Path:     path,
		Args:     append([]string(nil), args...),
		Stdout:   stdout,
		ExitCode: 0,
	}, nil
}

func TestCaptureArtifactBrowserScreenshotWritesMetadataAndSanitizesName(t *testing.T) {
	dir := t.TempDir()
	runner := &fakeArtifactRunner{}
	tool := newCaptureArtifactTool(LocalControlArtifactOptions{
		Browser:      localBrowserTestOptions(t, runner),
		ArtifactsDir: dir,
	})

	result := tool.Run(context.Background(), map[string]any{
		"action":   "browser_screenshot",
		"name":     "../proof.txt",
		"full":     true,
		"annotate": true,
	})
	if result.Status != StatusOK {
		t.Fatalf("status = %s output = %q", result.Status, result.Output)
	}
	path := filepath.Join(dir, "proof.png")
	wantArgs := []string{"screenshot", "--full", "--annotate", path}
	if !reflect.DeepEqual(runner.args, wantArgs) {
		t.Fatalf("args = %#v, want %#v", runner.args, wantArgs)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("artifact missing: %v", err)
	}
	var metadata map[string]any
	data, err := os.ReadFile(path + ".json")
	if err != nil {
		t.Fatalf("metadata missing: %v", err)
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatalf("metadata json: %v", err)
	}
	if metadata["action"] != "browser_screenshot" || metadata["driver"] != "browser" || metadata["path"] != path {
		t.Fatalf("metadata = %#v", metadata)
	}
	if !reflect.DeepEqual(result.ChangedFiles, []string{path, path + ".json"}) {
		t.Fatalf("changed files = %#v", result.ChangedFiles)
	}
}

func TestCaptureArtifactTerminalSnapshotWritesOutput(t *testing.T) {
	dir := t.TempDir()
	runner := &fakeArtifactRunner{stdout: "screen\n"}
	tool := newCaptureArtifactTool(LocalControlArtifactOptions{
		Terminal:     localBrowserTestOptions(t, runner),
		ArtifactsDir: dir,
	})

	result := tool.Run(context.Background(), map[string]any{
		"action":  "terminal_snapshot",
		"session": "demo",
		"name":    "term",
	})
	if result.Status != StatusOK {
		t.Fatalf("status = %s output = %q", result.Status, result.Output)
	}
	wantArgs := []string{"-s", "demo", "snapshot", "--trim"}
	if !reflect.DeepEqual(runner.args, wantArgs) {
		t.Fatalf("args = %#v, want %#v", runner.args, wantArgs)
	}
	data, err := os.ReadFile(filepath.Join(dir, "term.txt"))
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if string(data) != "screen" {
		t.Fatalf("artifact content = %q, want screen", string(data))
	}
}

func TestCaptureArtifactRejectsDisabledDriverBeforePermission(t *testing.T) {
	tool := newCaptureArtifactTool(LocalControlArtifactOptions{
		Browser:      localcontrol.BrowserOptions{Enabled: true},
		ArtifactsDir: t.TempDir(),
	})

	result, rejected := tool.(PrePermissionRejecter).RejectBeforePermission(map[string]any{
		"action": "desktop_screenshot",
	})
	if !rejected || result.Status != StatusError || !strings.Contains(result.Output, "disabled") {
		t.Fatalf("reject = (%v, %#v), want disabled desktop rejection", rejected, result)
	}
}

func TestCaptureArtifactRejectsMissingArtifactDirBeforePermission(t *testing.T) {
	tool := newCaptureArtifactTool(LocalControlArtifactOptions{
		Browser: localcontrol.BrowserOptions{Enabled: true},
	})
	if tool.Safety().Permission != PermissionDeny {
		t.Fatalf("permission = %s, want deny", tool.Safety().Permission)
	}
	result, rejected := tool.(PrePermissionRejecter).RejectBeforePermission(map[string]any{
		"action": "browser_screenshot",
	})
	if !rejected || result.Status != StatusError || !strings.Contains(result.Output, "artifact directory") {
		t.Fatalf("reject = (%v, %#v), want missing artifact directory", rejected, result)
	}
}

func TestCaptureArtifactDesktopWindowStateBuildsDriverArgs(t *testing.T) {
	dir := t.TempDir()
	runner := &fakeArtifactRunner{}
	tool := newCaptureArtifactTool(LocalControlArtifactOptions{
		Desktop:      localBrowserTestOptions(t, runner),
		ArtifactsDir: dir,
	})

	result := tool.Run(context.Background(), map[string]any{
		"action":    "desktop_window_state",
		"name":      "window",
		"pid":       float64(123),
		"window_id": float64(456),
		"query":     "Save",
	})
	if result.Status != StatusOK {
		t.Fatalf("status = %s output = %q", result.Status, result.Output)
	}
	wantPath := filepath.Join(dir, "window.png")
	wantArgs := []string{"get_window_state", `{"pid":123,"query":"Save","window_id":456}`, "--screenshot-out-file", wantPath}
	if !reflect.DeepEqual(runner.args, wantArgs) {
		t.Fatalf("args = %#v, want %#v", runner.args, wantArgs)
	}
}
