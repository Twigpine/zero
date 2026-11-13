package tools

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/localcontrol"
)

func TestDesktopSnapshotBuildsDriverJSON(t *testing.T) {
	runner := &fakeBrowserRunner{}
	tool := newDesktopSnapshotTool(localBrowserTestOptions(t, runner))

	result := tool.Run(context.Background(), map[string]any{
		"pid":       float64(123),
		"window_id": float64(456),
		"query":     "Save",
	})
	if result.Status != StatusOK {
		t.Fatalf("status = %s output = %q", result.Status, result.Output)
	}
	want := []string{"get_window_state", `{"pid":123,"query":"Save","window_id":456}`}
	if !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("args = %#v, want %#v", runner.args, want)
	}
}

func TestDesktopWindowsRejectsInvalidPIDBeforePermission(t *testing.T) {
	tool := newDesktopWindowsTool(localcontrol.DesktopOptions{Enabled: true})
	result, rejected := tool.(PrePermissionRejecter).RejectBeforePermission(map[string]any{"pid": float64(0)})
	if !rejected || result.Status != StatusError || !strings.Contains(result.Output, "pid") {
		t.Fatalf("reject = (%v, %#v), want pid rejection", rejected, result)
	}
}

func TestDesktopSnapshotRejectsInvalidWindowArgsBeforePermission(t *testing.T) {
	tool := newDesktopSnapshotTool(localcontrol.DesktopOptions{Enabled: true})
	result, rejected := tool.(PrePermissionRejecter).RejectBeforePermission(map[string]any{
		"pid":       float64(123),
		"window_id": float64(0),
	})
	if !rejected || result.Status != StatusError || !strings.Contains(result.Output, "window_id") {
		t.Fatalf("reject = (%v, %#v), want window_id rejection", rejected, result)
	}
}

func TestDesktopActionMapsAllowedCommand(t *testing.T) {
	runner := &fakeBrowserRunner{}
	tool := newDesktopActionTool(localBrowserTestOptions(t, runner))

	result := tool.Run(context.Background(), map[string]any{
		"command": "click",
		"input": map[string]any{
			"pid":           float64(123),
			"window_id":     float64(456),
			"element_index": float64(7),
		},
	})
	if result.Status != StatusOK {
		t.Fatalf("status = %s output = %q", result.Status, result.Output)
	}
	want := []string{"click", `{"element_index":7,"pid":123,"window_id":456}`}
	if !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("args = %#v, want %#v", runner.args, want)
	}
}

func TestDesktopActionRejectsUnknownCommandBeforePermission(t *testing.T) {
	tool := newDesktopActionTool(localcontrol.DesktopOptions{Enabled: true})
	result, rejected := tool.(PrePermissionRejecter).RejectBeforePermission(map[string]any{
		"command": "screenshot",
		"input":   map[string]any{},
	})
	if !rejected || result.Status != StatusError || !strings.Contains(result.Output, "command must be one of") {
		t.Fatalf("reject = (%v, %#v), want command rejection", rejected, result)
	}
}

func TestTerminalLaunchBuildsTuistoryArgs(t *testing.T) {
	runner := &fakeBrowserRunner{}
	tool := newTerminalSessionTool(localBrowserTestOptions(t, runner))

	result := tool.Run(context.Background(), map[string]any{
		"action":  "launch",
		"session": "demo",
		"command": "npm start",
		"cols":    float64(100),
		"rows":    float64(30),
	})
	if result.Status != StatusOK {
		t.Fatalf("status = %s output = %q", result.Status, result.Output)
	}
	want := []string{"launch", "npm start", "-s", "demo", "--cols", "100", "--rows", "30"}
	if !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("args = %#v, want %#v", runner.args, want)
	}
}

func TestTerminalPressBuildsTuistoryArgs(t *testing.T) {
	runner := &fakeBrowserRunner{}
	tool := newTerminalSessionTool(localBrowserTestOptions(t, runner))

	result := tool.Run(context.Background(), map[string]any{
		"action":  "press",
		"session": "demo",
		"key":     "Ctrl-C",
	})
	if result.Status != StatusOK {
		t.Fatalf("status = %s output = %q", result.Status, result.Output)
	}
	want := []string{"-s", "demo", "press", "ctrl", "c"}
	if !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("args = %#v, want %#v", runner.args, want)
	}
}

func TestTerminalPressKeysRunsSequentialPresses(t *testing.T) {
	runner := &fakeBrowserRunner{}
	tool := newTerminalSessionTool(localBrowserTestOptions(t, runner))

	result := tool.Run(context.Background(), map[string]any{
		"action":  "press",
		"session": "demo",
		"keys":    []any{"tab", "Return", "ctrl-c"},
	})
	if result.Status != StatusOK {
		t.Fatalf("status = %s output = %q", result.Status, result.Output)
	}
	want := [][]string{
		{"-s", "demo", "press", "tab"},
		{"-s", "demo", "press", "enter"},
		{"-s", "demo", "press", "ctrl", "c"},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, want)
	}
}

func TestTerminalSendLineTypesTextThenEnter(t *testing.T) {
	runner := &fakeBrowserRunner{}
	tool := newTerminalSessionTool(localBrowserTestOptions(t, runner))

	result := tool.Run(context.Background(), map[string]any{
		"action":  "send_line",
		"session": "demo",
		"text":    "echo hello",
	})
	if result.Status != StatusOK {
		t.Fatalf("status = %s output = %q", result.Status, result.Output)
	}
	want := [][]string{
		{"-s", "demo", "type", "echo hello"},
		{"-s", "demo", "press", "enter"},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, want)
	}
}

func TestTerminalSendLineWithEmptyTextPressesEnterOnly(t *testing.T) {
	runner := &fakeBrowserRunner{}
	tool := newTerminalSessionTool(localBrowserTestOptions(t, runner))

	result := tool.Run(context.Background(), map[string]any{
		"action":  "send_line",
		"session": "demo",
	})
	if result.Status != StatusOK {
		t.Fatalf("status = %s output = %q", result.Status, result.Output)
	}
	want := [][]string{{"-s", "demo", "press", "enter"}}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, want)
	}
}

func TestTerminalSnapshotHidesCursorByDefault(t *testing.T) {
	runner := &fakeBrowserRunner{}
	tool := newTerminalSessionTool(localBrowserTestOptions(t, runner))

	result := tool.Run(context.Background(), map[string]any{
		"action":  "snapshot",
		"session": "demo",
	})
	if result.Status != StatusOK {
		t.Fatalf("status = %s output = %q", result.Status, result.Output)
	}
	want := []string{"-s", "demo", "snapshot", "--trim", "--no-cursor"}
	if !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("args = %#v, want %#v", runner.args, want)
	}
}

func TestTerminalReadBuildsTuistoryArgs(t *testing.T) {
	runner := &fakeBrowserRunner{}
	tool := newTerminalSessionTool(localBrowserTestOptions(t, runner))

	result := tool.Run(context.Background(), map[string]any{
		"action":     "read",
		"session":    "demo",
		"all":        true,
		"follow":     true,
		"timeout_ms": float64(3000),
	})
	if result.Status != StatusOK {
		t.Fatalf("status = %s output = %q", result.Status, result.Output)
	}
	want := []string{"-s", "demo", "read", "--all", "--trim", "--follow", "--timeout", "3000"}
	if !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("args = %#v, want %#v", runner.args, want)
	}
}

func TestTerminalWaitIdleAcceptsTimeout(t *testing.T) {
	runner := &fakeBrowserRunner{}
	tool := newTerminalSessionTool(localBrowserTestOptions(t, runner))

	result := tool.Run(context.Background(), map[string]any{
		"action":     "wait_idle",
		"session":    "demo",
		"timeout_ms": float64(2000),
	})
	if result.Status != StatusOK {
		t.Fatalf("status = %s output = %q", result.Status, result.Output)
	}
	want := []string{"-s", "demo", "wait-idle", "--timeout", "2000"}
	if !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("args = %#v, want %#v", runner.args, want)
	}
}

func TestTerminalRejectsInvalidKeyBeforePermission(t *testing.T) {
	tool := newTerminalSessionTool(localcontrol.TerminalOptions{Enabled: true})
	result, rejected := tool.(PrePermissionRejecter).RejectBeforePermission(map[string]any{
		"action":  "press",
		"session": "demo",
		"key":     "hyper",
	})
	if !rejected || result.Status != StatusError || !strings.Contains(result.Output, "unsupported key") {
		t.Fatalf("reject = (%v, %#v), want key rejection", rejected, result)
	}
}

func TestTerminalRejectsInvalidActionBeforePermission(t *testing.T) {
	tool := newTerminalSessionTool(localcontrol.TerminalOptions{Enabled: true})
	result, rejected := tool.(PrePermissionRejecter).RejectBeforePermission(map[string]any{
		"action":  "record",
		"session": "demo",
	})
	if !rejected || result.Status != StatusError || !strings.Contains(result.Output, "action must be one of") {
		t.Fatalf("reject = (%v, %#v), want action rejection", rejected, result)
	}
}
