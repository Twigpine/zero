package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/localcontrol"
)

type fakeBrowserRunner struct {
	args     []string
	calls    [][]string
	stdout   string
	stderr   string
	exitCode int
	err      error
}

type fakeBrowserAppLauncher struct {
	request localcontrol.BrowserAppLaunchRequest
	result  localcontrol.BrowserAppLaunchResult
	err     error
}

func (runner *fakeBrowserRunner) Run(_ context.Context, path string, args []string, _ []string, _ time.Duration) (localcontrol.CommandResult, error) {
	runner.args = append([]string(nil), args...)
	runner.calls = append(runner.calls, append([]string(nil), args...))
	stdout := runner.stdout
	if stdout == "" && runner.stderr == "" && runner.err == nil {
		stdout = "browser ok\n"
	}
	return localcontrol.CommandResult{
		Path:     path,
		Args:     append([]string(nil), args...),
		Stdout:   stdout,
		Stderr:   runner.stderr,
		ExitCode: runner.exitCode,
	}, runner.err
}

func (launcher *fakeBrowserAppLauncher) LaunchBrowserApp(_ context.Context, request localcontrol.BrowserAppLaunchRequest) (localcontrol.BrowserAppLaunchResult, error) {
	launcher.request = request
	if launcher.err != nil {
		return localcontrol.BrowserAppLaunchResult{}, launcher.err
	}
	if launcher.result.App == "" {
		launcher.result = localcontrol.BrowserAppLaunchResult{
			App:         request.App,
			PID:         1234,
			DebugPort:   request.DebugPort,
			DevToolsURL: "http://127.0.0.1:9222",
		}
	}
	return launcher.result, nil
}

func localBrowserTestOptions(t *testing.T, runner localcontrol.CommandRunner) localcontrol.BrowserOptions {
	t.Helper()
	helper := filepath.Join(t.TempDir(), "agent-browser")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write helper: %v", err)
	}
	return localcontrol.BrowserOptions{
		Enabled:    true,
		HelperPath: helper,
		Runner:     runner,
	}
}

func TestBrowserLaunchRunsSupportedAppAndConnects(t *testing.T) {
	runner := &fakeBrowserRunner{}
	launcher := &fakeBrowserAppLauncher{}
	tool := newBrowserLaunchToolWithLauncher(localBrowserTestOptions(t, runner), launcher)

	result := tool.Run(context.Background(), map[string]any{"app": "discord"})
	if result.Status != StatusOK {
		t.Fatalf("status = %s output = %q", result.Status, result.Output)
	}
	wantRequest := localcontrol.BrowserAppLaunchRequest{
		App:          "discord",
		DebugPort:    localcontrol.DefaultDevToolsPort,
		StopExisting: true,
		Wait:         true,
	}
	if !reflect.DeepEqual(launcher.request, wantRequest) {
		t.Fatalf("launch request = %#v, want %#v", launcher.request, wantRequest)
	}
	if want := []string{"connect", "9222"}; !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("browser args = %#v, want %#v", runner.args, want)
	}
	if !strings.Contains(result.Output, "Launched discord") {
		t.Fatalf("output = %q, want launch summary", result.Output)
	}
	if !strings.Contains(result.Output, "already connected") {
		t.Fatalf("output = %q, want connected handoff", result.Output)
	}
}

func TestBrowserLaunchCanSkipConnect(t *testing.T) {
	runner := &fakeBrowserRunner{}
	launcher := &fakeBrowserAppLauncher{}
	tool := newBrowserLaunchToolWithLauncher(localBrowserTestOptions(t, runner), launcher)

	result := tool.Run(context.Background(), map[string]any{
		"app":           "discord",
		"debug_port":    float64(9333),
		"stop_existing": false,
		"wait":          false,
		"connect":       false,
	})
	if result.Status != StatusOK {
		t.Fatalf("status = %s output = %q", result.Status, result.Output)
	}
	wantRequest := localcontrol.BrowserAppLaunchRequest{
		App:          "discord",
		DebugPort:    9333,
		StopExisting: false,
		Wait:         false,
	}
	if !reflect.DeepEqual(launcher.request, wantRequest) {
		t.Fatalf("launch request = %#v, want %#v", launcher.request, wantRequest)
	}
	if runner.args != nil {
		t.Fatalf("browser args = %#v, want no connect", runner.args)
	}
	if !strings.Contains(result.Output, "Next call browser_connect with target 9333") {
		t.Fatalf("output = %q, want connect hint", result.Output)
	}
}

func TestBrowserLaunchRejectsUnsupportedAppBeforePermission(t *testing.T) {
	tool := newBrowserLaunchToolWithLauncher(localcontrol.BrowserOptions{Enabled: true}, &fakeBrowserAppLauncher{})
	result, rejected := tool.(PrePermissionRejecter).RejectBeforePermission(map[string]any{"app": "telegram"})
	if !rejected || result.Status != StatusError || !strings.Contains(result.Output, "app must be one of") {
		t.Fatalf("reject = (%v, %#v), want app rejection", rejected, result)
	}
}

func TestBrowserOpenNormalizesBareHost(t *testing.T) {
	runner := &fakeBrowserRunner{}
	tool := newBrowserOpenTool(localBrowserTestOptions(t, runner))

	result := tool.Run(context.Background(), map[string]any{"url": "example.com"})
	if result.Status != StatusOK {
		t.Fatalf("status = %s output = %q", result.Status, result.Output)
	}
	if want := []string{"open", "https://example.com"}; !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("args = %#v, want %#v", runner.args, want)
	}
}

func TestBrowserClickTypeAndPressRunStructuredCommands(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
		want []string
	}{
		{
			name: "click",
			args: map[string]any{"ref": "e114"},
			want: []string{"click", "e114"},
		},
		{
			name: "type",
			args: map[string]any{"ref": "e114", "text": "hey from zero"},
			want: []string{"type", "e114", "hey from zero"},
		},
		{
			name: "press",
			args: map[string]any{"key": "Enter"},
			want: []string{"press", "Enter"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeBrowserRunner{}
			var tool Tool
			switch tc.name {
			case "click":
				tool = newBrowserClickTool(localBrowserTestOptions(t, runner))
			case "type":
				tool = newBrowserTypeTool(localBrowserTestOptions(t, runner))
			case "press":
				tool = newBrowserPressTool(localBrowserTestOptions(t, runner))
			}
			result := tool.Run(context.Background(), tc.args)
			if result.Status != StatusOK {
				t.Fatalf("status = %s output = %q", result.Status, result.Output)
			}
			if !reflect.DeepEqual(runner.args, tc.want) {
				t.Fatalf("args = %#v, want %#v", runner.args, tc.want)
			}
		})
	}
}

func TestBrowserTypeRejectsMissingRefBeforePermission(t *testing.T) {
	tool := newBrowserTypeTool(localcontrol.BrowserOptions{Enabled: true})
	result, rejected := tool.(PrePermissionRejecter).RejectBeforePermission(map[string]any{"text": "hey from zero"})
	if !rejected || result.Status != StatusError || !strings.Contains(result.Output, "ref is required") {
		t.Fatalf("reject = (%v, %#v), want ref rejection", rejected, result)
	}
}

func TestBrowserInstallRunsHelperInstall(t *testing.T) {
	runner := &fakeBrowserRunner{}
	tool := newBrowserInstallTool(localBrowserTestOptions(t, runner))

	result := tool.Run(context.Background(), map[string]any{"with_deps": true})
	if result.Status != StatusOK {
		t.Fatalf("status = %s output = %q", result.Status, result.Output)
	}
	if want := []string{"install", "--with-deps"}; !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("args = %#v, want %#v", runner.args, want)
	}
}

func TestBrowserInstallUsesLocalBrowserSafety(t *testing.T) {
	tool := newBrowserInstallTool(localcontrol.BrowserOptions{Enabled: true})
	if tool.Safety().SideEffect != SideEffectLocalBrowser {
		t.Fatalf("browser_install side effect = %s, want %s", tool.Safety().SideEffect, SideEffectLocalBrowser)
	}
}

func TestBrowserConnectRunsHelperConnect(t *testing.T) {
	runner := &fakeBrowserRunner{}
	tool := newBrowserConnectTool(localBrowserTestOptions(t, runner))

	result := tool.Run(context.Background(), map[string]any{"target": "9222"})
	if result.Status != StatusOK {
		t.Fatalf("status = %s output = %q", result.Status, result.Output)
	}
	if want := []string{"connect", "9222"}; !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("args = %#v, want %#v", runner.args, want)
	}
}

func TestBrowserConnectAcceptsLoopbackTargetsBeforePermission(t *testing.T) {
	tool := newBrowserConnectTool(localcontrol.BrowserOptions{Enabled: true})
	for _, target := range []string{
		"9222",
		"localhost:9222",
		"127.0.0.1:9222",
		"http://127.0.0.1:9222/json/version",
		"ws://[::1]:9222/devtools/browser/abc",
	} {
		if result, rejected := tool.(PrePermissionRejecter).RejectBeforePermission(map[string]any{"target": target}); rejected {
			t.Fatalf("target %q rejected before permission: %#v", target, result)
		}
	}
}

func TestBrowserConnectRejectsShellLikeTargetsBeforePermission(t *testing.T) {
	tool := newBrowserConnectTool(localcontrol.BrowserOptions{Enabled: true})
	for _, target := range []string{"--version", "9222;rm", "http://127.0.0.1:9222/json list"} {
		result, rejected := tool.(PrePermissionRejecter).RejectBeforePermission(map[string]any{"target": target})
		if !rejected || result.Status != StatusError || !strings.Contains(result.Output, "browser_connect") {
			t.Fatalf("target %q reject = (%v, %#v), want target rejection", target, rejected, result)
		}
	}
}

func TestBrowserConnectRejectsRemoteTargetsBeforePermission(t *testing.T) {
	tool := newBrowserConnectTool(localcontrol.BrowserOptions{Enabled: true})
	for _, target := range []string{
		"example.com:9222",
		"192.168.1.20:9222",
		"https://example.com:9222/json/version",
		"ws://example.com:9222/devtools/browser/abc",
		"localhost:notaport",
	} {
		result, rejected := tool.(PrePermissionRejecter).RejectBeforePermission(map[string]any{"target": target})
		if !rejected || result.Status != StatusError || !strings.Contains(result.Output, "browser_connect") {
			t.Fatalf("target %q reject = (%v, %#v), want target rejection", target, rejected, result)
		}
	}
}

func TestBrowserOpenRejectsUnsupportedSchemeBeforePermission(t *testing.T) {
	tool := newBrowserOpenTool(localcontrol.BrowserOptions{Enabled: true})
	result, rejected := tool.(PrePermissionRejecter).RejectBeforePermission(map[string]any{"url": "file:///etc/passwd"})
	if !rejected || result.Status != StatusError || !strings.Contains(result.Output, "http or https") {
		t.Fatalf("reject = (%v, %#v), want scheme rejection", rejected, result)
	}
}

func TestBrowserSnapshotBuildsStructuredArgs(t *testing.T) {
	runner := &fakeBrowserRunner{}
	tool := newBrowserSnapshotTool(localBrowserTestOptions(t, runner))

	result := tool.Run(context.Background(), map[string]any{
		"compact":  true,
		"depth":    float64(3),
		"selector": "#app",
	})
	if result.Status != StatusOK {
		t.Fatalf("status = %s output = %q", result.Status, result.Output)
	}
	want := []string{"snapshot", "-i", "-c", "-d", "3", "-s", "#app"}
	if !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("args = %#v, want %#v", runner.args, want)
	}
}

func TestBrowserSnapshotRejectsObviousInvalidSelectorBeforePermission(t *testing.T) {
	tool := newBrowserSnapshotTool(localcontrol.BrowserOptions{Enabled: true})
	result, rejected := tool.(PrePermissionRejecter).RejectBeforePermission(map[string]any{
		"selector": `main[aria-label*="channel"] [role="textbox"]*`,
	})
	if !rejected || result.Status != StatusError || !strings.Contains(result.Output, "selector appears invalid") {
		t.Fatalf("reject = (%v, %#v), want selector rejection", rejected, result)
	}
}

func TestBrowserActionMapsAllowedCommand(t *testing.T) {
	runner := &fakeBrowserRunner{}
	tool := newBrowserActionTool(localBrowserTestOptions(t, runner))

	result := tool.Run(context.Background(), map[string]any{
		"command": "keyboard_insert_text",
		"args":    []any{"hello"},
	})
	if result.Status != StatusOK {
		t.Fatalf("status = %s output = %q", result.Status, result.Output)
	}
	want := []string{"keyboard", "inserttext", "hello"}
	if !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("args = %#v, want %#v", runner.args, want)
	}
}

func TestBrowserActionConnectAcceptsLoopbackTarget(t *testing.T) {
	runner := &fakeBrowserRunner{}
	tool := newBrowserActionTool(localBrowserTestOptions(t, runner))

	result := tool.Run(context.Background(), map[string]any{
		"command": "connect",
		"args":    []any{"127.0.0.1:9222"},
	})
	if result.Status != StatusOK {
		t.Fatalf("status = %s output = %q", result.Status, result.Output)
	}
	want := []string{"connect", "127.0.0.1:9222"}
	if !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("args = %#v, want %#v", runner.args, want)
	}
}

func TestBrowserActionScrollAcceptsSignedIntegerDeltas(t *testing.T) {
	runner := &fakeBrowserRunner{}
	tool := newBrowserActionTool(localBrowserTestOptions(t, runner))

	result := tool.Run(context.Background(), map[string]any{
		"command": "scroll",
		"args":    []any{"0", "-500"},
	})
	if result.Status != StatusOK {
		t.Fatalf("status = %s output = %q", result.Status, result.Output)
	}
	want := []string{"scroll", "0", "-500"}
	if !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("args = %#v, want %#v", runner.args, want)
	}
}

func TestBrowserActionRejectsUnknownCommandBeforePermission(t *testing.T) {
	tool := newBrowserActionTool(localcontrol.BrowserOptions{Enabled: true})
	result, rejected := tool.(PrePermissionRejecter).RejectBeforePermission(map[string]any{
		"command": "shell",
	})
	if !rejected || result.Status != StatusError || !strings.Contains(result.Output, "command must be one of") {
		t.Fatalf("reject = (%v, %#v), want command rejection", rejected, result)
	}
}

func TestBrowserActionRejectsInvalidStructuredArgsBeforePermission(t *testing.T) {
	tool := newBrowserActionTool(localcontrol.BrowserOptions{Enabled: true})
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{
			name: "remote connect",
			args: map[string]any{"command": "connect", "args": []any{"example.com:9222"}},
			want: "loopback",
		},
		{
			name: "click ref with space",
			args: map[string]any{"command": "click", "args": []any{"e 1"}},
			want: "ref",
		},
		{
			name: "type ref option",
			args: map[string]any{"command": "type", "args": []any{"--ref", "hello"}},
			want: "ref",
		},
		{
			name: "press key with space",
			args: map[string]any{"command": "press", "args": []any{"Control L"}},
			want: "key",
		},
		{
			name: "drag target ref with space",
			args: map[string]any{"command": "drag", "args": []any{"e1", "e 2"}},
			want: "ref",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, rejected := tool.(PrePermissionRejecter).RejectBeforePermission(tc.args)
			if !rejected || result.Status != StatusError || !strings.Contains(result.Output, tc.want) {
				t.Fatalf("reject = (%v, %#v), want %q", rejected, result, tc.want)
			}
		})
	}
}

func TestBrowserCDPErrorsDoNotSuggestBrowserInstall(t *testing.T) {
	runner := &fakeBrowserRunner{
		stdout:   "",
		stderr:   "✗ CDP error (DOM.describeNode): Object id doesn't reference a Node\n",
		exitCode: 1,
		err:      errors.New("exit status 1"),
	}
	tool := newBrowserSnapshotTool(localBrowserTestOptions(t, runner))

	result := tool.Run(context.Background(), map[string]any{})
	if result.Status != StatusError {
		t.Fatalf("status = %s output = %q", result.Status, result.Output)
	}
	if strings.Contains(result.Output, "browser_install") {
		t.Fatalf("output = %q, should not suggest browser_install for CDP errors", result.Output)
	}
}

func TestMissingBrowserHelperSuggestsBrowserInstall(t *testing.T) {
	browser := localcontrol.NewBrowser(localcontrol.BrowserOptions{
		Enabled: true,
		Driver:  "definitely-missing-agent-browser",
	})

	result := browserCommandResult(context.Background(), browser, "snapshot", []string{"snapshot"})
	if result.Status != StatusError {
		t.Fatalf("status = %s output = %q", result.Status, result.Output)
	}
	if !strings.Contains(result.Output, "browser_install") {
		t.Fatalf("output = %q, want browser_install hint", result.Output)
	}
}

func TestDisabledBrowserToolsAreDenied(t *testing.T) {
	var toolset []Tool
	toolset = append(toolset, NewLocalBrowserTools(localcontrol.BrowserOptions{})...)
	toolset = append(toolset, NewLocalDesktopTools(localcontrol.DesktopOptions{})...)
	toolset = append(toolset, NewLocalTerminalTools(localcontrol.TerminalOptions{})...)
	for _, tool := range toolset {
		if tool.Safety().Permission != PermissionDeny {
			t.Fatalf("%s permission = %s, want deny", tool.Name(), tool.Safety().Permission)
		}
	}
}
