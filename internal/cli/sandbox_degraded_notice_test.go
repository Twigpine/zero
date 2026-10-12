package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/mcp"
	"github.com/Gitlawb/zero/internal/sandbox"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/tui"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// hostSandboxNotice is the degraded-sandbox line exec prints on THIS machine for
// a run under the default sandbox policy, or "" where the sandbox is not
// degraded. Exec tests that expect a quiet stderr compare against it rather than
// against "": unless a test pins a backend, the run gets the host's own, and a
// Linux runner without the sandbox helper is degraded where macOS and Windows
// are not.
func hostSandboxNotice(t *testing.T) string {
	t.Helper()
	engine := sandbox.NewEngine(sandbox.EngineOptions{
		WorkspaceRoot: t.TempDir(),
		Policy:        sandbox.DefaultPolicy(),
		Backend:       sandbox.SelectBackend(sandbox.BackendOptions{}),
	})
	notice := engine.DegradedNotice()
	if notice == "" {
		return ""
	}
	return "[zero] " + notice + "\n"
}

// clearSandboxNestingMarkers keeps a test that pins the notice from inheriting
// the nesting markers when the suite itself runs inside a Zero sandbox.
func clearSandboxNestingMarkers(t *testing.T) {
	t.Helper()
	t.Setenv(sandbox.EnvSandboxed, "")
	t.Setenv(sandbox.EnvSandboxBackend, "")
}

// unavailableTestSandbox is the backend a Linux box without the helper gets,
// pinned so a test sees the degraded case on every platform.
func unavailableTestSandbox(sandbox.BackendOptions) sandbox.Backend {
	return sandbox.Backend{Name: sandbox.BackendUnavailable, Platform: "linux", Fallback: true, Message: "Linux sandbox helper is not available"}
}

const unavailableTestSandboxNotice = "[zero] Sandbox enforcement is degraded: Linux sandbox helper is not available. See `zero sandbox policy --effective`.\n"

func runExecWithSandbox(t *testing.T, args []string, backend func(sandbox.BackendOptions) sandbox.Backend, sandboxConfig config.SandboxConfig) (int, string, string) {
	t.Helper()
	isolateCLIUserState(t)
	var stdout, stderr bytes.Buffer
	cwd := t.TempDir()
	exitCode := runWithDeps(args, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return cwd, nil },
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{
				ActiveProvider: "echo",
				Provider: config.ProviderProfile{
					Name:         "echo",
					ProviderKind: config.ProviderKindOpenAICompatible,
					BaseURL:      "http://127.0.0.1/v1",
					Model:        "echo-model",
				},
				MaxTurns: 3,
				Sandbox:  sandboxConfig,
			}, nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return echoExecProvider{}, nil
		},
		selectSandboxBackend: backend,
	})
	return exitCode, stdout.String(), stderr.String()
}

// A RUN THAT GOES AHEAD WITH REDUCED ISOLATION SAYS SO. Before this only `zero
// doctor` and `zero sandbox policy` did, so `zero exec` ran every tool with the
// native sandbox effectively off and printed nothing about it. #1041.
func TestExecSaysOnStderrWhenTheSandboxIsDegraded(t *testing.T) {
	clearSandboxNestingMarkers(t)
	exitCode, _, stderr := runExecWithSandbox(t, []string{"exec", "hello"}, unavailableTestSandbox, config.SandboxConfig{})
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr %q", exitCode, stderr)
	}
	if stderr != unavailableTestSandboxNotice {
		t.Fatalf("stderr = %q, want exactly the degraded notice %q", stderr, unavailableTestSandboxNotice)
	}
}

// The notice goes to stderr in the JSON modes as well, so stdout stays one JSON
// object per line for whatever reads it.
func TestExecKeepsTheDegradedNoticeOffStdoutInJSONMode(t *testing.T) {
	clearSandboxNestingMarkers(t)
	exitCode, stdout, stderr := runExecWithSandbox(t, []string{"exec", "--output-format", "json", "hello"}, unavailableTestSandbox, config.SandboxConfig{})
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr %q", exitCode, stderr)
	}
	if stderr != unavailableTestSandboxNotice {
		t.Fatalf("stderr = %q, want exactly the degraded notice", stderr)
	}
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("stdout carries a line that is not JSON: %q (%v)", line, err)
		}
	}
}

// A sandbox the user turned off is their choice, not a degradation.
func TestExecSaysNothingWhenTheSandboxIsTurnedOff(t *testing.T) {
	clearSandboxNestingMarkers(t)
	off := false
	exitCode, _, stderr := runExecWithSandbox(t, []string{"exec", "hello"}, unavailableTestSandbox, config.SandboxConfig{Enabled: &off})
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr %q", exitCode, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want nothing for a disabled sandbox", stderr)
	}
}

// countingExecProvider is echoExecProvider, counting the completions it streams.
type countingExecProvider struct {
	echoExecProvider
	streamed *int
}

func (provider countingExecProvider) StreamCompletion(ctx context.Context, request zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	*provider.streamed++
	return provider.echoExecProvider.StreamCompletion(ctx, request)
}

// A degraded notice that cannot be written stops the run before the model is
// asked for anything, as the image and effort notices do, instead of going ahead
// with reduced isolation and no warning delivered.
func TestExecStopsWhenTheDegradedNoticeCannotBeWritten(t *testing.T) {
	clearSandboxNestingMarkers(t)
	isolateCLIUserState(t)
	var stdout bytes.Buffer
	cwd := t.TempDir()
	streamed := 0
	exitCode := runWithDeps([]string{"exec", "hello"}, &stdout, failingWriter{}, appDeps{
		getwd: func() (string, error) { return cwd, nil },
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{
				ActiveProvider: "echo",
				Provider: config.ProviderProfile{
					Name:         "echo",
					ProviderKind: config.ProviderKindOpenAICompatible,
					BaseURL:      "http://127.0.0.1/v1",
					Model:        "echo-model",
				},
				MaxTurns: 3,
			}, nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return countingExecProvider{streamed: &streamed}, nil
		},
		selectSandboxBackend: unavailableTestSandbox,
	})
	if exitCode != exitCrash {
		t.Fatalf("exit code = %d, want %d when the degraded notice cannot be written", exitCode, exitCrash)
	}
	if streamed != 0 {
		t.Fatalf("the model was asked %d times: the run went ahead without its warning", streamed)
	}
}

// launchTUIWithSandbox runs the root command the way a bare `zero` does, with
// the sandbox backend pinned to the unavailable one, and returns the options the
// TUI would have started with.
func launchTUIWithSandbox(t *testing.T, sandboxConfig config.SandboxConfig) tui.Options {
	t.Helper()
	isolateCLIUserState(t)
	var stdout, stderr bytes.Buffer
	cwd := t.TempDir()
	var launched tui.Options
	exitCode := runWithDeps([]string{}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return cwd, nil },
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{MaxTurns: 12, Sandbox: sandboxConfig}, nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			t.Fatal("newProvider should not be called without a resolved provider")
			return nil, nil
		},
		registerMCPTools: func(context.Context, *tools.Registry, config.MCPConfig, mcp.RegisterOptions) (mcpToolRuntime, error) {
			return noopMCPRuntime{}, nil
		},
		selectSandboxBackend: unavailableTestSandbox,
		runTUI: func(_ context.Context, options tui.Options) int {
			launched = options
			return 0
		},
	})
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr %q", exitCode, stderr.String())
	}
	return launched
}

// The TUI shows the same notice when the session opens, taken from the engine
// its commands run through.
func TestTUILaunchCarriesTheDegradedNotice(t *testing.T) {
	clearSandboxNestingMarkers(t)
	for _, testCase := range []struct {
		name    string
		sandbox config.SandboxConfig
		want    bool
	}{
		{name: "degraded", want: true},
		{name: "turned off", sandbox: config.SandboxConfig{Enabled: func() *bool { off := false; return &off }()}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			launched := launchTUIWithSandbox(t, testCase.sandbox)
			var notices []string
			for _, notice := range launched.StartupNotices {
				if strings.TrimSpace(notice) != "" {
					notices = append(notices, notice)
				}
			}
			want := strings.TrimSuffix(strings.TrimPrefix(unavailableTestSandboxNotice, "[zero] "), "\n")
			if testCase.want && (len(notices) != 1 || notices[0] != want) {
				t.Fatalf("StartupNotices = %q, want exactly %q", notices, want)
			}
			if !testCase.want && len(notices) != 0 {
				t.Fatalf("StartupNotices = %q, want none", notices)
			}
		})
	}
}
