package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/modelregistry"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

func TestRunExecHelpDocumentsM1Flags(t *testing.T) {
	for _, args := range [][]string{
		{"exec", "--help"},
		{"exec", "--help", "--model", "m1"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer

			exitCode := Run(args, &stdout, &stderr)

			if exitCode != 0 {
				t.Fatalf("expected exit code 0, got %d", exitCode)
			}
			for _, want := range []string{
				"-f, --file",
				"--image <path>",
				"--mode <name>",
				"-m, --model",
				"--max-turns",
				"--profile <profile>",
				"-r, --reasoning-effort <effort>",
				"-C, --cwd",
				"-o, --output-format text|json",
				"--prompt",
				"--calling-session-id",
				"--calling-tool-use-id",
				"--tag <tag>",
				"--depth <number>",
				"--session-title",
				"--init-session-id",
				"--skip-permissions-unsafe",
			} {
				if !strings.Contains(stdout.String(), want) {
					t.Fatalf("expected exec help to contain %q, got %q", want, stdout.String())
				}
			}
			if stderr.Len() != 0 {
				t.Fatalf("expected empty stderr, got %q", stderr.String())
			}
		})
	}
}

func TestRunExecRejectsInvalidMaxTurnsBeforeRuntime(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  string
	}{
		{value: "nope", want: "invalid --max-turns"},
		{value: "-1", want: "invalid --max-turns"},
		{value: "0", want: "invalid --max-turns"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer

			exitCode := Run([]string{"exec", "--max-turns", tc.value, "hello"}, &stdout, &stderr)

			if exitCode != exitUsage {
				t.Fatalf("expected exit code %d, got %d", exitUsage, exitCode)
			}
			if stdout.Len() != 0 {
				t.Fatalf("expected empty stdout before runtime, got %q", stdout.String())
			}
			if got := stderr.String(); !strings.Contains(got, tc.want) {
				t.Fatalf("expected max-turns validation error containing %q, got %q", tc.want, got)
			}
		})
	}

	t.Run("equals-empty", func(t *testing.T) {
		var stdout bytes.Buffer
		var stderr bytes.Buffer

		exitCode := Run([]string{"exec", "--max-turns=", "hello"}, &stdout, &stderr)

		if exitCode != exitUsage {
			t.Fatalf("expected exit code %d, got %d", exitUsage, exitCode)
		}
		if stdout.Len() != 0 {
			t.Fatalf("expected empty stdout before runtime, got %q", stdout.String())
		}
		if got := stderr.String(); !strings.Contains(got, "--max-turns requires a value") {
			t.Fatalf("expected empty max-turns validation error, got %q", got)
		}
	})
}

func TestRunExecMaxTurnsReachesConfigOverrides(t *testing.T) {
	cwd := t.TempDir()
	var gotMaxTurns int

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"exec", "--max-turns", "7", "hello"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(_ string, overrides config.Overrides) (config.ResolvedConfig, error) {
			gotMaxTurns = overrides.MaxTurns
			return config.ResolvedConfig{}, errors.New("stop before provider")
		},
	})

	if exitCode != exitProvider {
		t.Fatalf("expected provider exit %d, got %d", exitProvider, exitCode)
	}
	if gotMaxTurns != 7 {
		t.Fatalf("overrides.MaxTurns = %d, want 7", gotMaxTurns)
	}
}

func TestParseExecSpecialistMetadataFlags(t *testing.T) {
	options, help, err := parseExecArgs([]string{
		"--calling-session-id", "parent_session",
		"--calling-tool-use-id=toolu_123",
		"--tag", "specialist",
		"--depth=2",
		"--session-title", "Explorer child",
		"--init-session-id", "child_session",
		"--output-format", "debug",
		"inspect the parser",
	})
	if err != nil {
		t.Fatalf("parseExecArgs returned error: %v", err)
	}
	if help {
		t.Fatal("help = true, want false")
	}
	if options.callingSessionID != "parent_session" ||
		options.callingToolUseID != "toolu_123" ||
		options.tag != "specialist" ||
		options.depth != 2 ||
		options.sessionTitle != "Explorer child" ||
		options.initSessionID != "child_session" {
		t.Fatalf("metadata flags did not parse correctly: %#v", options)
	}
	if options.outputFormat != execOutputStreamJSON {
		t.Fatalf("outputFormat = %q, want stream-json debug alias", options.outputFormat)
	}
	if strings.Join(options.promptParts, " ") != "inspect the parser" {
		t.Fatalf("promptParts = %#v", options.promptParts)
	}
}

func TestParseExecSpecialistMetadataRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "negative depth", args: []string{"--depth=-1", "hello"}, want: "invalid --depth"},
		{name: "non numeric depth", args: []string{"--depth", "many", "hello"}, want: "invalid --depth"},
		{name: "empty tag", args: []string{"--tag=", "hello"}, want: "--tag requires a value"},
		{name: "bad init session", args: []string{"--init-session-id", "../escape", "hello"}, want: "invalid --init-session-id"},
		{name: "init with resume", args: []string{"--init-session-id", "child", "--resume", "parent", "hello"}, want: "Use --init-session-id only"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := parseExecArgs(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q error, got %v", tc.want, err)
			}
		})
	}
}

func TestRunExecRegistersTaskOnlyForUnsafeTopLevelRuns(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantTask bool
	}{
		{name: "default headless", args: []string{"exec", "--list-tools"}, wantTask: false},
		{name: "unsafe headless", args: []string{"exec", "--auto", "high", "--list-tools"}, wantTask: true},
		{name: "specialist child", args: []string{"exec", "--auto", "high", "--tag", "specialist", "--list-tools"}, wantTask: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			exitCode := runWithDeps(tc.args, &stdout, &stderr, appDeps{
				getwd: func() (string, error) {
					return t.TempDir(), nil
				},
			})
			if exitCode != exitSuccess {
				t.Fatalf("exitCode = %d stdout=%s stderr=%s", exitCode, stdout.String(), stderr.String())
			}
			for _, toolName := range []string{"Task", "TaskOutput", "TaskStop", "GenerateSpecialist"} {
				hasTool := strings.Contains(stdout.String(), "  "+toolName+" ")
				if hasTool != tc.wantTask {
					t.Fatalf("%s visibility = %v, want %v; output:\n%s", toolName, hasTool, tc.wantTask, stdout.String())
				}
			}
		})
	}
}

func TestRunExecUsesInitSessionIDAndSessionTitle(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	cwd := t.TempDir()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{
		"exec",
		"--init-session-id", "specialist_child",
		"--session-title", "Explorer child",
		"--tag", "specialist",
		"--depth", "1",
		"hello",
	}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(_ string, _ config.Overrides) (config.ResolvedConfig, error) {
			return execResolvedConfig(), nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return echoExecProvider{}, nil
		},
	})
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stdout=%s stderr=%s", exitCode, stdout.String(), stderr.String())
	}

	store := sessions.NewStore(sessions.StoreOptions{RootDir: filepath.Join(dataHome, "zero", "sessions")})
	session, err := store.Get("specialist_child")
	if err != nil {
		t.Fatalf("Get session returned error: %v", err)
	}
	if session == nil {
		t.Fatal("expected initialized session specialist_child")
	}
	if session.Title != "Explorer child" || session.Tag != "specialist" || session.Depth != 1 {
		t.Fatalf("session metadata = %#v, want title/tag/depth", session)
	}
	if session.Cwd != cwd {
		t.Fatalf("session cwd = %q, want %q", session.Cwd, cwd)
	}
}

func TestRunExecPersistsCallingSessionChildMetadata(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	store := sessions.NewStore(sessions.StoreOptions{RootDir: filepath.Join(dataHome, "zero", "sessions")})
	parent, err := store.Create(sessions.CreateInput{SessionID: "parent_session", Title: "Parent", Cwd: "/repo", ModelID: "gpt-parent", Provider: "openai"})
	if err != nil {
		t.Fatalf("Create parent returned error: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{
		"exec",
		"--output-format", "stream-json",
		"--init-session-id", "child_session",
		"--session-title", "worker: Auth check",
		"--tag", "specialist",
		"--depth", "1",
		"--calling-session-id", parent.SessionID,
		"--calling-tool-use-id", "toolu_123",
		"hello",
	}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return t.TempDir(), nil
		},
		resolveConfig: func(_ string, _ config.Overrides) (config.ResolvedConfig, error) {
			return execResolvedConfig(), nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return echoExecProvider{}, nil
		},
	})
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stdout=%s stderr=%s", exitCode, stdout.String(), stderr.String())
	}

	child, err := store.Get("child_session")
	if err != nil {
		t.Fatalf("Get child returned error: %v", err)
	}
	if child == nil {
		t.Fatal("expected child session metadata")
	}
	if child.SessionKind != sessions.SessionKindChild ||
		child.ParentSessionID != parent.SessionID ||
		child.RootSessionID != parent.SessionID ||
		child.AgentName != "worker" ||
		child.TaskID != "child_session" ||
		child.SpawnedFromEventID != "toolu_123" ||
		child.Tag != "specialist" ||
		child.Depth != 1 {
		t.Fatalf("unexpected child metadata: %#v", child)
	}
}

func TestRunExecModeSeedsModelAndTurnOverrides(t *testing.T) {
	cwd := t.TempDir()
	var gotModel string
	var gotMaxTurns int

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"exec", "--mode", "deep", "hello"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(_ string, overrides config.Overrides) (config.ResolvedConfig, error) {
			gotModel = overrides.Provider.Model
			gotMaxTurns = overrides.MaxTurns
			return config.ResolvedConfig{}, errors.New("stop before provider")
		},
	})

	if exitCode != exitProvider {
		t.Fatalf("expected provider exit %d, got %d", exitProvider, exitCode)
	}
	if gotModel != "claude-opus-4.1" {
		t.Fatalf("overrides.Provider.Model = %q, want claude-opus-4.1", gotModel)
	}
	if gotMaxTurns != 160 {
		t.Fatalf("overrides.MaxTurns = %d, want 160 (deep mode)", gotMaxTurns)
	}
}

func TestRunExecExplicitModelOverridesMode(t *testing.T) {
	cwd := t.TempDir()
	var gotModel string

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"exec", "--mode", "deep", "--model", "gpt-4.1", "hello"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(_ string, overrides config.Overrides) (config.ResolvedConfig, error) {
			gotModel = overrides.Provider.Model
			return config.ResolvedConfig{}, errors.New("stop before provider")
		},
	})

	if exitCode != exitProvider {
		t.Fatalf("expected provider exit %d, got %d", exitProvider, exitCode)
	}
	if gotModel != "gpt-4.1" {
		t.Fatalf("explicit --model should override mode: got %q, want gpt-4.1", gotModel)
	}
}

func TestRunExecModeRoutesModelThroughRegistry(t *testing.T) {
	cwd := t.TempDir()
	var gotModel string

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	// "smart" maps to claude-sonnet-4.5; the mode's model must be routed through
	// the registry (Resolve) so the canonical id reaches the overrides.
	exitCode := runWithDeps([]string{"exec", "--mode", "smart", "hi"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(_ string, overrides config.Overrides) (config.ResolvedConfig, error) {
			gotModel = overrides.Provider.Model
			return config.ResolvedConfig{}, errors.New("stop before provider")
		},
	})

	if exitCode != exitProvider {
		t.Fatalf("expected provider exit %d, got %d", exitProvider, exitCode)
	}
	if gotModel != "claude-sonnet-4.5" {
		t.Fatalf("expected mode smart to select claude-sonnet-4.5, got %q", gotModel)
	}
}

func TestRunExecUnknownModeErrors(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := Run([]string{"exec", "--mode", "turbo", "hello"}, &stdout, &stderr)

	if exitCode != exitUsage {
		t.Fatalf("expected usage exit %d, got %d", exitUsage, exitCode)
	}
	if !strings.Contains(stderr.String(), "unknown mode") {
		t.Fatalf("expected unknown mode error, got %q", stderr.String())
	}
	for _, want := range []string{"smart", "deep", "fast", "large", "precise"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("expected error to list valid mode %q, got %q", want, stderr.String())
		}
	}
}

func TestApplyExecModeLeavesRawModelForSharedResolution(t *testing.T) {
	// applyExecMode must NOT pre-resolve the mode's model: leaving the raw id/alias
	// lets the shared --model resolution path resolve it AND surface any deprecation
	// notice on stderr (the bug was that applyExecMode resolved it and discarded the
	// notice). The raw alias must be the preset's exact Model value.
	mode, ok := modelregistry.LookupMode("deep")
	if !ok {
		t.Fatal("expected built-in mode deep")
	}
	options := execOptions{mode: "deep"}
	if err := applyExecMode(&options); err != nil {
		t.Fatalf("applyExecMode returned error: %v", err)
	}
	if options.model != mode.Model {
		t.Fatalf("options.model = %q, want raw mode model %q (resolution must be delegated)", options.model, mode.Model)
	}
}

func TestRunExecModeModelSurfacesDeprecationNoticeViaSharedPath(t *testing.T) {
	// A mode-supplied model must flow through the same resolution path as an
	// explicit --model, so a deprecated id redirects AND prints a notice. No
	// built-in mode references a deprecated model, so emulate one by setting the
	// raw mode model directly through applyExecMode and threading it through the
	// shared resolver, exactly as runExec does after the reorder.
	options := execOptions{mode: "smart"}
	if err := applyExecMode(&options); err != nil {
		t.Fatalf("applyExecMode returned error: %v", err)
	}
	// Sanity: the shared resolver surfaces a notice + redirect for a deprecated id,
	// which is the path the mode model now feeds into.
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	resolved, notice := resolveSelectedModel(registry, "gpt-4-turbo")
	if resolved != "gpt-4.1" {
		t.Fatalf("expected deprecated model to redirect to gpt-4.1, got %q", resolved)
	}
	if !strings.Contains(notice, "deprecated") {
		t.Fatalf("expected shared resolver to surface a deprecation notice, got %q", notice)
	}
}

func TestRunExecListToolsAppliesModeBeforeListing(t *testing.T) {
	// applyExecMode now runs before tool-filter validation and the --list-tools
	// branch, so a --mode preset is expanded for --list-tools. Combining a mode
	// with --list-tools must still succeed without constructing a provider.
	cwd := t.TempDir()
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"exec", "--list-tools", "--mode", "deep"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return execResolvedConfig(), nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Tools visible to model") {
		t.Fatalf("expected --list-tools --mode to list tools, got %q", stdout.String())
	}
}

func TestRunExecModeToolFilterReflectedInListTools(t *testing.T) {
	// The reorder also means a mode-injected tool filter would be reflected in
	// --list-tools. No built-in mode ships a tool filter, so drive the equivalent
	// surface through applyExecMode + formatExecToolList: a filter seeded onto the
	// options before the listing must narrow the tools the model can see.
	options := execOptions{enabledTools: []string{"read_file", "grep"}}
	registry := newCoreRegistry(t.TempDir())
	list := formatExecToolList(registry, options, agent.PermissionModeAuto)
	for _, want := range []string{"read_file", "grep"} {
		if !strings.Contains(list, want) {
			t.Fatalf("expected tool list to contain %q, got %q", want, list)
		}
	}
	if strings.Contains(list, "bash") {
		t.Fatalf("expected mode-style tool filter to hide bash, got %q", list)
	}
}

func TestResolveExecPermissionModeMember(t *testing.T) {
	cases := []struct {
		autonomy string
		want     agent.PermissionMode
	}{
		{"", agent.PermissionModeAuto},
		{"low", agent.PermissionModeAuto},
		{"medium", agent.PermissionModeAuto},
		{"member", agent.PermissionModeMemberAuto}, // headless members: write + sandboxed shell
		{"high", agent.PermissionModeUnsafe},
	}
	for _, c := range cases {
		got, err := resolveExecPermissionMode(execOptions{autonomy: c.autonomy})
		if err != nil {
			t.Fatalf("resolveExecPermissionMode(%q): %v", c.autonomy, err)
		}
		if got != c.want {
			t.Errorf("resolveExecPermissionMode(%q) = %q, want %q", c.autonomy, got, c.want)
		}
	}
	if _, err := resolveExecPermissionMode(execOptions{autonomy: "bogus"}); err == nil {
		t.Fatal("an unknown autonomy level must still be rejected")
	}
}

// A member-auto headless tool list must include the in-workspace mutators that
// plain Auto hides, so a swarm member can actually build. Match the tool ENTRY
// line ("  write_file [") — a bare substring would false-match tool descriptions.
func TestExecMemberAutoToolListIncludesMutators(t *testing.T) {
	registry := newCoreRegistry(t.TempDir())
	const writeEntry = "\n  write_file ["

	member := formatExecToolList(registry, execOptions{}, agent.PermissionModeMemberAuto)
	if !strings.Contains(member, writeEntry) {
		t.Fatalf("member-auto tool list must include write_file, got %q", member)
	}
	// Plain Auto must still hide it (unchanged behavior — this is the read-only gate).
	auto := formatExecToolList(registry, execOptions{}, agent.PermissionModeAuto)
	if strings.Contains(auto, writeEntry) {
		t.Fatalf("plain Auto must still hide write_file, got %q", auto)
	}
}

func TestRunExecAcceptsLegacyModelProfileFlags(t *testing.T) {
	// --profile (legacy, inert model profile) and --exec-profile (execution
	// profile) are distinct flags and must coexist on one invocation.
	exitCode, stdout, stderr := runExecWithEcho(t, []string{
		"exec",
		"--profile",
		"fast",
		"--exec-profile",
		"balanced",
		"--reasoning-effort",
		"low",
		"hello",
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr)
	}
	if !strings.Contains(stdout, "hello") {
		t.Fatalf("expected prompt output, got %q", stdout)
	}
	if stderr != "" {
		t.Fatalf("expected empty stderr, got %q", stderr)
	}
}

func TestRunExecJSONRunStartWriteFailureSkipsAgent(t *testing.T) {
	cwd := t.TempDir()
	called := false

	exitCode := runWithDeps([]string{"exec", "-o", "json", "hello"}, failingWriter{}, io.Discard, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(_ string, _ config.Overrides) (config.ResolvedConfig, error) {
			return execResolvedConfig(), nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return recordingExecProvider{called: &called}, nil
		},
	})

	if exitCode != exitCrash {
		t.Fatalf("expected exit code %d, got %d", exitCrash, exitCode)
	}
	if called {
		t.Fatal("expected agent provider not to run after run_start write failure")
	}
}

func TestRunExecUnsafeWarningWriteFailureSkipsAgent(t *testing.T) {
	cwd := t.TempDir()
	called := false

	exitCode := runWithDeps([]string{"exec", "--skip-permissions-unsafe", "hello"}, io.Discard, failingWriter{}, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(_ string, _ config.Overrides) (config.ResolvedConfig, error) {
			return execResolvedConfig(), nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return recordingExecProvider{called: &called}, nil
		},
	})

	if exitCode != exitCrash {
		t.Fatalf("expected exit code %d, got %d", exitCrash, exitCode)
	}
	if called {
		t.Fatal("expected agent provider not to run after warning write failure")
	}
}

func TestRunExecJSONProviderErrorWriteFailureReturnsCrash(t *testing.T) {
	cwd := t.TempDir()

	exitCode := runWithDeps([]string{"exec", "-o", "json", "hello"}, failingWriter{}, io.Discard, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(_ string, _ config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{}, errors.New("provider config failed")
		},
	})

	if exitCode != exitCrash {
		t.Fatalf("expected exit code %d, got %d", exitCrash, exitCode)
	}
}

func execResolvedConfig() config.ResolvedConfig {
	return config.ResolvedConfig{
		ActiveProvider: "echo",
		Provider: config.ProviderProfile{
			Name:         "echo",
			ProviderKind: config.ProviderKindOpenAICompatible,
			BaseURL:      "http://127.0.0.1/v1",
			Model:        "echo-model",
		},
	}
}

type recordingExecProvider struct {
	called *bool
}

func (provider recordingExecProvider) StreamCompletion(context.Context, zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	*provider.called = true
	return nil, errors.New("provider should not run")
}

func TestRunPromptFlagRoutesToExecRunner(t *testing.T) {
	execExitCode, execStdout, execStderr := runExecWithEcho(t, []string{"exec", "hello zero"})

	for _, args := range [][]string{
		{"-p", "hello zero"},
		{"--prompt", "hello zero"},
	} {
		t.Run(args[0], func(t *testing.T) {
			exitCode, stdout, stderr := runExecWithEcho(t, args)

			if exitCode != execExitCode {
				t.Fatalf("expected exit code %d, got %d", execExitCode, exitCode)
			}
			if stdout != execStdout {
				t.Fatalf("expected stdout %q, got %q", execStdout, stdout)
			}
			if stderr != execStderr {
				t.Fatalf("expected stderr %q, got %q", execStderr, stderr)
			}
		})
	}
}

func TestRunExecAssemblesInlineAndFilePromptRelativeToCwd(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "prompt.txt"), []byte("file prompt\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	exitCode, stdout, stderr := runExecWithEcho(t, []string{"exec", "--cwd", root, "--file", "prompt.txt", "inline prompt"})

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d: %s", exitCode, stderr)
	}
	if !strings.Contains(stdout, "inline prompt\n\nfile prompt") {
		t.Fatalf("expected inline and file prompt joined by blank line, got %q", stdout)
	}
	if stderr != "" {
		t.Fatalf("expected empty stderr, got %q", stderr)
	}
}

func TestRunExecAcceptsFileOnlyPrompt(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "prompt.txt"), []byte("file only prompt\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	exitCode, stdout, stderr := runExecWithEcho(t, []string{"exec", "-C", root, "-f", "prompt.txt"})

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d: %s", exitCode, stderr)
	}
	if !strings.Contains(stdout, "file only prompt") {
		t.Fatalf("expected file prompt output, got %q", stdout)
	}
	if stderr != "" {
		t.Fatalf("expected empty stderr, got %q", stderr)
	}
}

func TestRunExecRejectsInvalidCwdBeforeRuntime(t *testing.T) {
	root := t.TempDir()
	filePath := filepath.Join(root, "not-a-directory.txt")
	if err := os.WriteFile(filePath, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		cwd  string
	}{
		{name: "missing", cwd: filepath.Join(root, "missing")},
		{name: "file", cwd: filePath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer

			exitCode := Run([]string{"exec", "--cwd", tc.cwd, "hello"}, &stdout, &stderr)

			if exitCode != 2 {
				t.Fatalf("expected exit code 2, got %d", exitCode)
			}
			if stdout.Len() != 0 {
				t.Fatalf("expected empty stdout before runtime, got %q", stdout.String())
			}
			if got := stderr.String(); !strings.Contains(got, "cwd must be an existing directory") {
				t.Fatalf("expected cwd validation error, got %q", got)
			}
			if strings.Contains(stdout.String()+stderr.String(), "Go agent runtime ready") {
				t.Fatalf("expected validation before runtime, got stdout %q stderr %q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestRunExecRejectsInvalidOutputFormatBeforeRuntime(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := Run([]string{"exec", "-o", "yaml", "hello"}, &stdout, &stderr)

	if exitCode != 2 {
		t.Fatalf("expected exit code 2, got %d", exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected empty stdout before runtime, got %q", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, `invalid output format "yaml"`) || !strings.Contains(got, "debug") {
		t.Fatalf("expected output format validation error, got %q", got)
	}
	if strings.Contains(stdout.String()+stderr.String(), "Go agent runtime ready") {
		t.Fatalf("expected validation before runtime, got stdout %q stderr %q", stdout.String(), stderr.String())
	}
}

func TestRunExecUnsafeTextModeWarns(t *testing.T) {
	exitCode, stdout, stderr := runExecWithEcho(t, []string{"exec", "--skip-permissions-unsafe", "hello"})

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d: %s", exitCode, stderr)
	}
	if !strings.Contains(stdout, "hello") {
		t.Fatalf("expected prompt in stdout, got %q", stdout)
	}
	if got := stderr; !strings.Contains(got, "WARNING") || !strings.Contains(got, "--skip-permissions-unsafe") {
		t.Fatalf("expected unsafe warning, got %q", got)
	}
}

func TestRunExecJSONOutputsNDJSONEvents(t *testing.T) {
	root := t.TempDir()

	exitCode, stdout, stderr := runExecWithEcho(t, []string{"exec", "--cwd", root, "-m", "m1-test", "-o", "json", "hello json"})

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d: %s", exitCode, stderr)
	}
	if stderr != "" {
		t.Fatalf("expected empty stderr, got %q", stderr)
	}

	events := decodeJSONLines(t, stdout)
	eventTypes := jsonEventTypes(events)
	for _, want := range []string{"run_start", "text", "final", "done"} {
		if !slices.Contains(eventTypes, want) {
			t.Fatalf("expected JSON event %q in %v; output %q", want, eventTypes, stdout)
		}
	}
	if got := events[0]["type"]; got != "run_start" {
		t.Fatalf("expected first event run_start, got %v", got)
	}
	if got := events[0]["model"]; got != "m1-test" {
		t.Fatalf("expected run_start model m1-test, got %v", got)
	}
	if got := events[0]["cwd"]; got != root {
		t.Fatalf("expected run_start cwd %q, got %v", root, got)
	}
	if got := events[0]["permission_mode"]; got != "auto" {
		t.Fatalf("expected default permission_mode auto, got %v", got)
	}
}

func TestRunExecResolvesCanonicalModelAlias(t *testing.T) {
	root := t.TempDir()

	// "openai:gpt-4.1" is a registry alias for the canonical gpt-4.1 id; the
	// selection boundary should normalize it before the provider sees it.
	exitCode, stdout, stderr := runExecWithEcho(t, []string{"exec", "--cwd", root, "-m", "openai:gpt-4.1", "-o", "json", "hi"})

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d: %s", exitCode, stderr)
	}
	if stderr != "" {
		t.Fatalf("expected empty stderr for active model, got %q", stderr)
	}
	events := decodeJSONLines(t, stdout)
	if got := events[0]["model"]; got != "gpt-4.1" {
		t.Fatalf("expected alias to resolve to gpt-4.1, got %v", got)
	}
}

func TestRunExecRedirectsDeprecatedModelWithNotice(t *testing.T) {
	root := t.TempDir()

	exitCode, stdout, stderr := runExecWithEcho(t, []string{"exec", "--cwd", root, "-m", "gpt-4-turbo", "-o", "json", "hi"})

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d: %s", exitCode, stderr)
	}
	if !strings.Contains(stderr, "deprecated") || !strings.Contains(stderr, "gpt-4.1") {
		t.Fatalf("expected deprecation notice on stderr, got %q", stderr)
	}
	events := decodeJSONLines(t, stdout)
	if got := events[0]["model"]; got != "gpt-4.1" {
		t.Fatalf("expected deprecated model to redirect to gpt-4.1, got %v", got)
	}
}

func TestRunExecReasoningEffortNoticeForNonReasoningModel(t *testing.T) {
	root := t.TempDir()

	exitCode, stdout, stderr := runExecWithEcho(t, []string{"exec", "--cwd", root, "-m", "gpt-4.1", "-r", "high", "-o", "json", "hi"})

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d: %s", exitCode, stderr)
	}
	if !strings.Contains(stderr, "does not support reasoning effort") {
		t.Fatalf("expected non-reasoning effort notice on stderr, got %q", stderr)
	}
	if stdout == "" {
		t.Fatal("expected run output on stdout")
	}
}

func TestReasoningEffortNoticeCoercesUnsupportedEffort(t *testing.T) {
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	// claude-sonnet-4.5 supports low/medium/high with a medium default; xhigh is
	// unsupported and should be coerced to the model default.
	notice := reasoningEffortNotice(registry, "claude-sonnet-4.5", "xhigh")
	if !strings.Contains(notice, "not supported") || !strings.Contains(notice, "medium") {
		t.Fatalf("expected coercion notice to default medium, got %q", notice)
	}
	if got := reasoningEffortNotice(registry, "claude-sonnet-4.5", "high"); got != "" {
		t.Fatalf("expected no notice for a supported effort, got %q", got)
	}
	if got := reasoningEffortNotice(registry, "gpt-4.1", "high"); !strings.Contains(got, "does not support") {
		t.Fatalf("expected unsupported-model notice, got %q", got)
	}
}

func TestForwardedReasoningEffortGating(t *testing.T) {
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	cases := []struct {
		name      string
		model     string
		requested string
		want      string
	}{
		{"empty request", "claude-sonnet-4.5", "", ""},
		{"supported reasoning model", "claude-sonnet-4.5", "high", "high"},
		{"unsupported effort coerced to default", "claude-sonnet-4.5", "xhigh", "medium"},
		{"known non-reasoning model suppressed", "gpt-4.1", "high", ""},
		{"unknown model forwards as-is", "custom-endpoint-model", "high", "high"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := forwardedReasoningEffort(registry, tc.model, tc.requested); got != tc.want {
				t.Fatalf("forwardedReasoningEffort(%q, %q) = %q, want %q", tc.model, tc.requested, got, tc.want)
			}
		})
	}
}

func TestRunExecJSONUnsafeOutputsWarningEvent(t *testing.T) {
	exitCode, stdout, stderr := runExecWithEcho(t, []string{"exec", "--skip-permissions-unsafe", "-o", "json", "hello"})

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d: %s", exitCode, stderr)
	}
	if stderr != "" {
		t.Fatalf("expected empty stderr, got %q", stderr)
	}

	events := decodeJSONLines(t, stdout)
	eventTypes := jsonEventTypes(events)
	if !slices.Contains(eventTypes, "warning") {
		t.Fatalf("expected JSON warning event in %v; output %q", eventTypes, stdout)
	}
	if got := events[0]["permission_mode"]; got != "unsafe" {
		t.Fatalf("expected run_start permission_mode unsafe, got %v", got)
	}
}

func TestRunExecUsesProjectConfigAndOpenAICompatibleProvider(t *testing.T) {
	clearProviderEnv(t)
	root := t.TempDir()
	configDir := filepath.Join(root, ".zero")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}

	var gotAuth string
	var gotMethod string
	var gotPath string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode provider request: %v", err)
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"provider ok\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	writeConfig := `{
		"activeProvider": "local",
		"providers": [{
			"name": "local",
			"provider_kind": "openai-compatible",
			"base_url": "` + server.URL + `",
			"api_key": "sk-local",
			"model": "local-model"
		}]
	}`
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(writeConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := Run([]string{"exec", "--cwd", root, "hello provider"}, &stdout, &stderr)

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d: %s", exitCode, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "provider ok" {
		t.Fatalf("stdout = %q, want provider response", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
	if gotAuth != "Bearer sk-local" {
		t.Fatalf("Authorization = %q, want project config token", gotAuth)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want %q", gotMethod, http.MethodPost)
	}
	if !strings.HasSuffix(gotPath, "/chat/completions") {
		t.Fatalf("path = %q, want suffix /chat/completions", gotPath)
	}
	if gotBody["model"] != "local-model" {
		t.Fatalf("provider model = %v, want local-model", gotBody["model"])
	}
	messages, ok := gotBody["messages"].([]any)
	if !ok || len(messages) == 0 {
		t.Fatalf("messages = %#v, want non-empty []any", gotBody["messages"])
	}
	lastMessage, ok := messages[len(messages)-1].(map[string]any)
	if !ok {
		t.Fatalf("last message = %#v, want map[string]any", messages[len(messages)-1])
	}
	if lastMessage["content"] != "hello provider" {
		t.Fatalf("last provider message = %#v, want prompt", lastMessage)
	}
}

func runExecWithEcho(t *testing.T, args []string) (int, string, string) {
	t.Helper()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cwd := t.TempDir()
	exitCode := runWithDeps(args, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(_ string, overrides config.Overrides) (config.ResolvedConfig, error) {
			model := "echo-model"
			if overrides.Provider.Model != "" {
				model = overrides.Provider.Model
			}
			return config.ResolvedConfig{
				ActiveProvider: "echo",
				Provider: config.ProviderProfile{
					Name:         "echo",
					ProviderKind: config.ProviderKindOpenAICompatible,
					BaseURL:      "http://127.0.0.1/v1",
					Model:        model,
				},
				MaxTurns: 3,
			}, nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return echoExecProvider{}, nil
		},
	})
	return exitCode, stdout.String(), stderr.String()
}

func TestExecSessionRecorderWarnsOnRecordingFailure(t *testing.T) {
	// A latched session-append failure must be surfaced once to stderr so a user
	// is not misled into believing the session was persisted; a clean recorder
	// stays silent.
	var failed bytes.Buffer
	(&execSessionRecorder{err: errors.New("disk full")}).warnIfRecordingFailed(&failed)
	if !strings.Contains(failed.String(), "session not fully recorded") || !strings.Contains(failed.String(), "disk full") {
		t.Fatalf("expected a recording-failure warning, got %q", failed.String())
	}

	var clean bytes.Buffer
	(&execSessionRecorder{}).warnIfRecordingFailed(&clean)
	if clean.Len() != 0 {
		t.Fatalf("expected silence when recording succeeded, got %q", clean.String())
	}
}

// canceledExecProvider fails its first call with context.Canceled, simulating a
// signal-interrupted run (agent.Run returns the error verbatim).
type canceledExecProvider struct{}

func (canceledExecProvider) StreamCompletion(context.Context, zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	return nil, context.Canceled
}

type echoExecProvider struct{}

func (echoExecProvider) StreamCompletion(ctx context.Context, request zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	prompt := ""
	for index := len(request.Messages) - 1; index >= 0; index-- {
		if request.Messages[index].Role == zeroruntime.MessageRoleUser {
			prompt = request.Messages[index].Content
			break
		}
	}
	ch := make(chan zeroruntime.StreamEvent, 2)
	select {
	case <-ctx.Done():
		close(ch)
		return ch, ctx.Err()
	case ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventText, Content: prompt}:
	}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}
	close(ch)
	return ch, nil
}

func clearProviderEnv(t *testing.T) {
	t.Helper()

	for _, key := range []string{
		"ZERO_PROVIDER_COMMAND",
		"ZERO_PROVIDER",
		"OPENAI_API_KEY",
		"OPENAI_BASE_URL",
		"OPENAI_MODEL",
	} {
		t.Setenv(key, "")
	}
}

func decodeJSONLines(t *testing.T, output string) []map[string]any {
	t.Helper()

	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatalf("expected JSON lines, got %q", output)
	}

	events := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("expected JSON object line, got %q: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

func jsonEventTypes(events []map[string]any) []string {
	types := make([]string, 0, len(events))
	for _, event := range events {
		eventType, _ := event["type"].(string)
		types = append(types, eventType)
	}
	return types
}

// runExecWithEffectiveModel runs exec with the echo provider but forces the
// resolved (effective) model regardless of any --model flag, so tests can
// exercise behavior that depends on the effective model rather than the
// override-supplied one.
func runExecWithEffectiveModel(t *testing.T, effectiveModel string, args []string) (int, string, string) {
	t.Helper()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cwd := t.TempDir()
	exitCode := runWithDeps(args, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(_ string, _ config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{
				ActiveProvider: "echo",
				Provider: config.ProviderProfile{
					Name:         "echo",
					ProviderKind: config.ProviderKindOpenAICompatible,
					BaseURL:      "http://127.0.0.1/v1",
					Model:        effectiveModel,
				},
				MaxTurns: 3,
			}, nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return echoExecProvider{}, nil
		},
	})
	return exitCode, stdout.String(), stderr.String()
}

// TestRunExecReasoningEffortNoticeUsesEffectiveModel asserts that
// --reasoning-effort is validated against the EFFECTIVE (resolved) model even
// when --model is omitted, so the advisory notice still surfaces.
func TestRunExecReasoningEffortNoticeUsesEffectiveModel(t *testing.T) {
	// gpt-4.1 is a registry-known non-reasoning model. With no --model the
	// override model is empty, so the notice must be evaluated against the
	// effective model resolved by resolveConfig.
	exitCode, stdout, stderr := runExecWithEffectiveModel(t, "gpt-4.1", []string{
		"exec", "-r", "high", "hi",
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr)
	}
	if !strings.Contains(stderr, "does not support reasoning effort") {
		t.Fatalf("expected effort notice for effective model on stderr, got %q", stderr)
	}
	if stdout == "" {
		t.Fatal("expected run output on stdout")
	}
}

// TestRunExecAutoHighEmitsUnsafeWarning asserts that --auto high (which resolves
// to PermissionModeUnsafe) surfaces the same unsafe warning as
// --skip-permissions-unsafe.
func TestRunExecAutoHighEmitsUnsafeWarning(t *testing.T) {
	exitCode, stdout, stderr := runExecWithEcho(t, []string{
		"exec", "--auto", "high", "-o", "json", "hello",
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr)
	}
	if stderr != "" {
		t.Fatalf("expected empty stderr, got %q", stderr)
	}

	events := decodeJSONLines(t, stdout)
	if !slices.Contains(jsonEventTypes(events), "warning") {
		t.Fatalf("expected JSON warning event for --auto high, got %v; output %q", jsonEventTypes(events), stdout)
	}
	if got := events[0]["permission_mode"]; got != "unsafe" {
		t.Fatalf("expected run_start permission_mode unsafe, got %v", got)
	}
}

// TestRunExecInvalidAutoValidatedWithSkipPermissions asserts that an invalid
// --auto value is still rejected even when --skip-permissions-unsafe is also
// passed (the unsafe path must not short-circuit --auto validation).
func TestRunExecInvalidAutoValidatedWithSkipPermissions(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := Run([]string{"exec", "--auto", "bogus", "--skip-permissions-unsafe", "hello"}, &stdout, &stderr)

	if exitCode != exitUsage {
		t.Fatalf("expected exit code %d, got %d", exitUsage, exitCode)
	}
	if got := stderr.String(); !strings.Contains(got, "Invalid autonomy level") {
		t.Fatalf("expected autonomy validation error, got %q", got)
	}
}

func TestParseExecAllowEscalationFlag(t *testing.T) {
	t.Run("absent defaults to false", func(t *testing.T) {
		options, help, err := parseExecArgs([]string{"hello"})
		if err != nil {
			t.Fatalf("parseExecArgs returned error: %v", err)
		}
		if help {
			t.Fatal("help = true, want false")
		}
		if options.allowEscalation {
			t.Fatal("allowEscalation = true, want false by default")
		}
	})

	t.Run("flag sets true", func(t *testing.T) {
		options, _, err := parseExecArgs([]string{"--allow-escalation", "hello"})
		if err != nil {
			t.Fatalf("parseExecArgs returned error: %v", err)
		}
		if !options.allowEscalation {
			t.Fatal("allowEscalation = false, want true")
		}
		if strings.Join(options.promptParts, " ") != "hello" {
			t.Fatalf("promptParts = %#v, want [hello]", options.promptParts)
		}
	})
}

func TestRunExecHelpDocumentsAllowEscalation(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := Run([]string{"exec", "--help"}, &stdout, &stderr)

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
	if !strings.Contains(stdout.String(), "--allow-escalation") {
		t.Fatalf("expected exec help to document --allow-escalation, got %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
}

func TestRunExecRegistersEscalateModelOnlyWithFlag(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantTool bool
	}{
		{name: "absent", args: []string{"exec", "--list-tools"}, wantTool: false},
		{name: "present", args: []string{"exec", "--allow-escalation", "--list-tools"}, wantTool: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			exitCode := runWithDeps(tc.args, &stdout, &stderr, appDeps{
				getwd: func() (string, error) {
					return t.TempDir(), nil
				},
			})
			if exitCode != exitSuccess {
				t.Fatalf("exitCode = %d stdout=%s stderr=%s", exitCode, stdout.String(), stderr.String())
			}
			hasTool := strings.Contains(stdout.String(), "  escalate_model ")
			if hasTool != tc.wantTool {
				t.Fatalf("escalate_model visibility = %v, want %v; output:\n%s", hasTool, tc.wantTool, stdout.String())
			}
		})
	}
}

// escalatingExecProvider drives an exec run through a mid-run switch without a
// live model. Each instance tracks how many turns IT served (turns) so a test
// can assert exactly which provider handled which turn across a switch.
//
// escalateOnce controls behavior: when true, the FIRST turn this instance serves
// emits an escalate_model tool call (later turns answer); when false, every turn
// emits a final text answer. Pointing escalateOnce at distinct instances lets a
// test prove the post-switch turn lands on the SECOND (escalated) provider.
type escalatingExecProvider struct {
	// turns counts how many StreamCompletion calls THIS instance handled.
	turns int
	// escalateOnce makes this instance's first served turn request an escalation.
	escalateOnce bool
}

func (provider *escalatingExecProvider) StreamCompletion(ctx context.Context, request zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	turn := provider.turns
	provider.turns++
	ch := make(chan zeroruntime.StreamEvent, 4)
	if provider.escalateOnce && turn == 0 {
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call_escalate", ToolName: "escalate_model"}
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call_escalate", ArgumentsFragment: "{}"}
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call_escalate"}
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}
		close(ch)
		return ch, nil
	}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventText, Content: "done"}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}
	close(ch)
	return ch, nil
}

func TestRunExecWiresModelSwitcherUnderFlag(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	cwd := t.TempDir()

	var providerModels []string
	// Each newProvider build returns a DISTINCT provider instance with its OWN
	// turn counter, mirroring the agent-layer firstProvider/secondProvider split.
	// The first build (escalation source) must handle exactly the escalation turn;
	// the second build (escalation target) must handle exactly the post-switch
	// answer turn. Sharing a single counter would hide a dropped `provider =
	// newProvider` in loop.go — here it is mutation-proven by the per-instance
	// turn assertions below.
	var builtProviders []*escalatingExecProvider
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"exec", "--allow-escalation", "--model", "claude-haiku-4.5", "escalate please"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(_ string, overrides config.Overrides) (config.ResolvedConfig, error) {
			model := "claude-haiku-4.5"
			if overrides.Provider.Model != "" {
				model = overrides.Provider.Model
			}
			cfg := execResolvedConfig()
			// The escalation chain (haiku -> sonnet) is anthropic; declare the
			// provider kind to match so provider-metadata resolution accepts it.
			cfg.Provider.ProviderKind = config.ProviderKindAnthropic
			cfg.Provider.Model = model
			cfg.MaxTurns = 3
			return cfg, nil
		},
		newProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			providerModels = append(providerModels, profile.Model)
			// First build escalates; every later build answers. Each instance owns
			// its turn counter so we can assert exactly-one-turn per provider.
			provider := &escalatingExecProvider{escalateOnce: len(builtProviders) == 0}
			builtProviders = append(builtProviders, provider)
			return provider, nil
		},
	})
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stdout=%s stderr=%s", exitCode, stdout.String(), stderr.String())
	}
	// Initial provider build + one rebuild for the escalation target.
	if len(providerModels) != 2 {
		t.Fatalf("newProvider called for models %#v, want exactly 2 builds (initial + escalated)", providerModels)
	}
	if providerModels[0] != "claude-haiku-4.5" {
		t.Fatalf("initial provider model = %q, want claude-haiku-4.5", providerModels[0])
	}
	target, ok := mustUpgradeTarget(t, "claude-haiku-4.5")
	if !ok {
		t.Skip("registry has no upgrade target for claude-haiku-4.5")
	}
	if providerModels[1] != target {
		t.Fatalf("escalated provider model = %q, want %q", providerModels[1], target)
	}
	if len(builtProviders) != 2 {
		t.Fatalf("built %d providers, want 2", len(builtProviders))
	}
	// The original (escalation-source) provider handled ONLY the escalation turn;
	// the escalated (target) provider handled ONLY the post-switch answer turn.
	// This FAILS if loop.go drops `provider = newProvider` (the second provider
	// would then handle zero turns and the first would handle both).
	if builtProviders[0].turns != 1 {
		t.Fatalf("first (source) provider handled %d turns, want exactly 1 (the escalation turn)", builtProviders[0].turns)
	}
	if builtProviders[1].turns != 1 {
		t.Fatalf("second (escalated) provider handled %d turns, want exactly 1 (the post-switch answer turn)", builtProviders[1].turns)
	}
}

func mustUpgradeTarget(t *testing.T, id string) (string, bool) {
	t.Helper()
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	entry, ok := registry.UpgradeTarget(id)
	return entry.ID, ok
}

func TestRunExecNoSwitcherWithoutFlag(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	cwd := t.TempDir()

	var providerModels []string
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"exec", "--model", "claude-haiku-4.5", "escalate please"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(_ string, overrides config.Overrides) (config.ResolvedConfig, error) {
			model := "claude-haiku-4.5"
			if overrides.Provider.Model != "" {
				model = overrides.Provider.Model
			}
			cfg := execResolvedConfig()
			// The escalation chain (haiku -> sonnet) is anthropic; declare the
			// provider kind to match so provider-metadata resolution accepts it.
			cfg.Provider.ProviderKind = config.ProviderKindAnthropic
			cfg.Provider.Model = model
			cfg.MaxTurns = 3
			return cfg, nil
		},
		newProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			providerModels = append(providerModels, profile.Model)
			return &escalatingExecProvider{escalateOnce: true}, nil
		},
	})
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stdout=%s stderr=%s", exitCode, stdout.String(), stderr.String())
	}
	if len(providerModels) != 1 {
		t.Fatalf("newProvider called for %#v, want exactly 1 build (no switcher wired)", providerModels)
	}
	if providerModels[0] != "claude-haiku-4.5" {
		t.Fatalf("provider model = %q, want claude-haiku-4.5", providerModels[0])
	}
}

func TestRunExecAttributesUsageToEscalatedModel(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	cwd := t.TempDir()

	target, ok := mustUpgradeTarget(t, "claude-haiku-4.5")
	if !ok {
		t.Skip("registry has no upgrade target for claude-haiku-4.5")
	}

	// The escalation turn (turn 0, pre-switch) emits usage with distinct tokens so
	// it can be told apart from the post-switch usage; the first build escalates,
	// the second answers.
	builds := 0
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{
		"exec",
		"--allow-escalation",
		"--model", "claude-haiku-4.5",
		"--init-session-id", "escalation_run",
		"escalate please",
	}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(_ string, overrides config.Overrides) (config.ResolvedConfig, error) {
			model := "claude-haiku-4.5"
			if overrides.Provider.Model != "" {
				model = overrides.Provider.Model
			}
			cfg := execResolvedConfig()
			// The escalation chain (haiku -> sonnet) is anthropic; declare the
			// provider kind to match so provider-metadata resolution accepts it.
			cfg.Provider.ProviderKind = config.ProviderKindAnthropic
			cfg.Provider.Model = model
			cfg.MaxTurns = 3
			return cfg, nil
		},
		newProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			// First build = escalation source (escalate + usage 3/4 pre-switch);
			// second build = escalation target (answer + usage 5/7 post-switch).
			escalate := builds == 0
			builds++
			return &usageEmittingEscalatingProvider{escalate: escalate}, nil
		},
	})
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stdout=%s stderr=%s", exitCode, stdout.String(), stderr.String())
	}

	store := sessions.NewStore(sessions.StoreOptions{RootDir: filepath.Join(dataHome, "zero", "sessions")})
	events, err := store.ReadEvents("escalation_run")
	if err != nil {
		t.Fatalf("ReadEvents returned error: %v", err)
	}
	// Collect every usage model attribution IN ORDER. The pre-switch usage must
	// attribute to the ORIGINAL model and the post-switch usage to the escalated
	// target — proving the loop's ordering guarantee (usage is attributed to the
	// model that produced it, not retroactively reassigned after the switch).
	var usageModels []string
	for _, event := range events {
		if event.Type != sessions.EventUsage {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("unmarshal usage payload: %v", err)
		}
		model, _ := payload["model"].(string)
		usageModels = append(usageModels, model)
	}
	if len(usageModels) != 2 {
		t.Fatalf("recorded usage model attributions = %#v, want exactly 2 (pre- and post-switch)", usageModels)
	}
	if usageModels[0] != "claude-haiku-4.5" {
		t.Fatalf("first (pre-switch) usage model = %q, want claude-haiku-4.5", usageModels[0])
	}
	if usageModels[1] != target {
		t.Fatalf("second (post-switch) usage model = %q, want escalated target %q", usageModels[1], target)
	}
}

func TestRunExecNilSwitchProviderKeepsOriginalAttribution(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	cwd := t.TempDir()

	if _, ok := mustUpgradeTarget(t, "claude-haiku-4.5"); !ok {
		t.Skip("registry has no upgrade target for claude-haiku-4.5")
	}

	// The escalation rebuild returns (nil, nil): the loop must NOT swap the
	// provider, and the CLI switcher must leave currentModel unchanged so usage
	// after the declined switch stays attributed to the ORIGINAL model. Without
	// the switchedProvider != nil guard, currentModel would advance to the target
	// even though no switch happened, misattributing the post-turn usage.
	builds := 0
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{
		"exec",
		"--allow-escalation",
		"--model", "claude-haiku-4.5",
		"--init-session-id", "nil_switch_run",
		"escalate please",
	}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return cwd, nil },
		resolveConfig: func(_ string, overrides config.Overrides) (config.ResolvedConfig, error) {
			model := "claude-haiku-4.5"
			if overrides.Provider.Model != "" {
				model = overrides.Provider.Model
			}
			cfg := execResolvedConfig()
			cfg.Provider.ProviderKind = config.ProviderKindAnthropic
			cfg.Provider.Model = model
			cfg.MaxTurns = 3
			return cfg, nil
		},
		newProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			builds++
			// First build = the original (haiku). The escalation rebuild returns
			// (nil, nil), so the loop keeps the original provider for every turn.
			if builds == 1 {
				return &escalateThenAnswerProvider{}, nil
			}
			return nil, nil
		},
	})
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stdout=%s stderr=%s", exitCode, stdout.String(), stderr.String())
	}

	store := sessions.NewStore(sessions.StoreOptions{RootDir: filepath.Join(dataHome, "zero", "sessions")})
	events, err := store.ReadEvents("nil_switch_run")
	if err != nil {
		t.Fatalf("ReadEvents returned error: %v", err)
	}
	sawUsage := false
	for _, event := range events {
		if event.Type != sessions.EventUsage {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("unmarshal usage payload: %v", err)
		}
		sawUsage = true
		if model, _ := payload["model"].(string); model != "claude-haiku-4.5" {
			t.Fatalf("usage attributed to %q after a (nil,nil) switch, want original claude-haiku-4.5", model)
		}
	}
	if !sawUsage {
		t.Fatal("expected at least one usage event")
	}
}

// escalateThenAnswerProvider escalates on its first turn and answers afterward,
// so a single instance can serve an entire run when no provider swap occurs
// (e.g. a (nil,nil) switcher). It emits usage on every turn for attribution.
type escalateThenAnswerProvider struct {
	turns int
}

func (provider *escalateThenAnswerProvider) StreamCompletion(ctx context.Context, request zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	turn := provider.turns
	provider.turns++
	ch := make(chan zeroruntime.StreamEvent, 6)
	if turn == 0 {
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call_escalate", ToolName: "escalate_model"}
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call_escalate", ArgumentsFragment: "{}"}
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call_escalate"}
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventUsage, Usage: zeroruntime.Usage{InputTokens: 3, OutputTokens: 4}}
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}
		close(ch)
		return ch, nil
	}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventText, Content: "done"}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventUsage, Usage: zeroruntime.Usage{InputTokens: 5, OutputTokens: 7}}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}
	close(ch)
	return ch, nil
}

// usageEmittingEscalatingProvider emits a usage event on every turn it serves so
// usage attribution can be traced across a mid-run switch. When escalate is set,
// its turn requests an escalation (and emits pre-switch usage tokens 3/4);
// otherwise it answers (post-switch usage tokens 5/7).
type usageEmittingEscalatingProvider struct {
	escalate bool
}

func (provider *usageEmittingEscalatingProvider) StreamCompletion(ctx context.Context, request zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	ch := make(chan zeroruntime.StreamEvent, 6)
	if provider.escalate {
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call_escalate", ToolName: "escalate_model"}
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call_escalate", ArgumentsFragment: "{}"}
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call_escalate"}
		// Pre-switch usage: still attributed to the ORIGINAL model.
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventUsage, Usage: zeroruntime.Usage{InputTokens: 3, OutputTokens: 4}}
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}
		close(ch)
		return ch, nil
	}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventText, Content: "done"}
	// Post-switch usage: attributed to the escalated model.
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventUsage, Usage: zeroruntime.Usage{InputTokens: 5, OutputTokens: 7}}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}
	close(ch)
	return ch, nil
}

// usageEmittingEchoProvider answers in a single turn and emits a usage event. It
// has no escalation behavior, so it exercises a plain (flag-OFF) run that still
// records usage.
type usageEmittingEchoProvider struct{}

func (usageEmittingEchoProvider) StreamCompletion(ctx context.Context, request zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	ch := make(chan zeroruntime.StreamEvent, 3)
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventText, Content: "done"}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventUsage, Usage: zeroruntime.Usage{InputTokens: 5, OutputTokens: 7}}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}
	close(ch)
	return ch, nil
}

// TestRunExecUsageOmitsModelKeyWithoutEscalationFlag verifies the back-compat
// guarantee: a run WITHOUT --allow-escalation persists EventUsage payloads that
// carry NO "model" key (byte-identical to before the escalation feature), since
// the model can never change mid-run when escalation is off. The flag-ON,
// post-escalation case is covered by TestRunExecAttributesUsageToEscalatedModel.
func TestRunExecUsageOmitsModelKeyWithoutEscalationFlag(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	cwd := t.TempDir()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{
		"exec",
		"--model", "claude-haiku-4.5",
		"--init-session-id", "no_escalation_run",
		"hello",
	}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(_ string, overrides config.Overrides) (config.ResolvedConfig, error) {
			model := "claude-haiku-4.5"
			if overrides.Provider.Model != "" {
				model = overrides.Provider.Model
			}
			cfg := execResolvedConfig()
			cfg.Provider.ProviderKind = config.ProviderKindAnthropic
			cfg.Provider.Model = model
			cfg.MaxTurns = 3
			return cfg, nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return usageEmittingEchoProvider{}, nil
		},
	})
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stdout=%s stderr=%s", exitCode, stdout.String(), stderr.String())
	}

	store := sessions.NewStore(sessions.StoreOptions{RootDir: filepath.Join(dataHome, "zero", "sessions")})
	events, err := store.ReadEvents("no_escalation_run")
	if err != nil {
		t.Fatalf("ReadEvents returned error: %v", err)
	}
	var sawUsage bool
	for _, event := range events {
		if event.Type != sessions.EventUsage {
			continue
		}
		sawUsage = true
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("unmarshal usage payload: %v", err)
		}
		if _, ok := payload["model"]; ok {
			t.Fatalf("flag-OFF usage payload must NOT carry a model key, got %#v", payload)
		}
	}
	if !sawUsage {
		t.Fatal("expected at least one usage event to be recorded")
	}
}

// usageThenAnswerProvider serves a two-turn run on ONE instance: turn 0 requests
// an escalation and emits usage, turn 1 answers and emits usage. It is used by
// the switch-error test where no second provider is ever built, so a single
// instance must serve both turns on the original model.
type usageThenAnswerProvider struct {
	turns int
}

func (provider *usageThenAnswerProvider) StreamCompletion(ctx context.Context, request zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	turn := provider.turns
	provider.turns++
	ch := make(chan zeroruntime.StreamEvent, 6)
	if turn == 0 {
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call_escalate", ToolName: "escalate_model"}
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call_escalate", ArgumentsFragment: "{}"}
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call_escalate"}
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventUsage, Usage: zeroruntime.Usage{InputTokens: 3, OutputTokens: 4}}
		ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}
		close(ch)
		return ch, nil
	}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventText, Content: "done"}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventUsage, Usage: zeroruntime.Usage{InputTokens: 5, OutputTokens: 7}}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}
	close(ch)
	return ch, nil
}

// TestRunExecSwitcherErrorKeepsOriginalModelAttribution verifies that when the
// ModelSwitcher fails on the rebuild (deps.newProvider errors for the escalation
// target), the run is NON-FATAL and stays on the original provider — and every
// usage event, including the one AFTER the failed switch, stays attributed to the
// ORIGINAL model (currentModel is never reassigned on a failed switch).
func TestRunExecSwitcherErrorKeepsOriginalModelAttribution(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	cwd := t.TempDir()

	if _, ok := mustUpgradeTarget(t, "claude-haiku-4.5"); !ok {
		t.Skip("registry has no upgrade target for claude-haiku-4.5")
	}

	builds := 0
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{
		"exec",
		"--allow-escalation",
		"--model", "claude-haiku-4.5",
		"--init-session-id", "switch_error_run",
		"escalate please",
	}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(_ string, overrides config.Overrides) (config.ResolvedConfig, error) {
			model := "claude-haiku-4.5"
			if overrides.Provider.Model != "" {
				model = overrides.Provider.Model
			}
			cfg := execResolvedConfig()
			cfg.Provider.ProviderKind = config.ProviderKindAnthropic
			cfg.Provider.Model = model
			cfg.MaxTurns = 3
			return cfg, nil
		},
		newProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			builds++
			// The first build (original model) succeeds; the rebuild on escalation
			// FAILS, so the switcher returns an error and the run continues on the
			// original provider.
			if builds >= 2 {
				return nil, errors.New("provider rebuild failed")
			}
			return &usageThenAnswerProvider{}, nil
		},
	})
	if exitCode != exitSuccess {
		t.Fatalf("expected non-fatal switch error, exitCode = %d stdout=%s stderr=%s", exitCode, stdout.String(), stderr.String())
	}
	if builds != 2 {
		t.Fatalf("newProvider builds = %d, want 2 (initial + one failed rebuild attempt)", builds)
	}

	store := sessions.NewStore(sessions.StoreOptions{RootDir: filepath.Join(dataHome, "zero", "sessions")})
	events, err := store.ReadEvents("switch_error_run")
	if err != nil {
		t.Fatalf("ReadEvents returned error: %v", err)
	}
	var usageModels []string
	for _, event := range events {
		if event.Type != sessions.EventUsage {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("unmarshal usage payload: %v", err)
		}
		model, _ := payload["model"].(string)
		usageModels = append(usageModels, model)
	}
	if len(usageModels) != 2 {
		t.Fatalf("recorded usage attributions = %#v, want 2", usageModels)
	}
	// Both usages (pre- AND post-failed-switch) stay on the ORIGINAL model.
	for i, model := range usageModels {
		if model != "claude-haiku-4.5" {
			t.Fatalf("usage[%d] model = %q, want claude-haiku-4.5 (currentModel must not change on a failed switch)", i, model)
		}
	}
}

// TestRunExecTopTierDeclineNoSwitch verifies an end-to-end flag-ON run where the
// agent calls escalate_model while ALREADY on a top-tier model (claude-opus-4.1):
// the tool returns the informational no-meta result, NO switch happens
// (newProvider is called exactly once), and the run succeeds.
func TestParseExecNotifyFlag(t *testing.T) {
	opts, _, err := parseExecArgs([]string{"--notify", "both", "hello"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if opts.notifyMode != "both" || opts.noNotify {
		t.Fatalf("got mode=%q noNotify=%v", opts.notifyMode, opts.noNotify)
	}
}

func TestParseExecNoNotifyFlag(t *testing.T) {
	opts, _, err := parseExecArgs([]string{"--no-notify", "hello"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !opts.noNotify {
		t.Fatal("expected noNotify=true")
	}
}

func TestParseExecNotifyConflict(t *testing.T) {
	if _, _, err := parseExecArgs([]string{"--notify", "bell", "--no-notify", "hi"}); err == nil {
		t.Fatal("expected error for --notify with --no-notify")
	}
}

func TestParseExecNotifyInvalidValue(t *testing.T) {
	for _, arg := range [][]string{{"--notify", "buzz", "hi"}, {"--notify=loud", "hi"}} {
		if _, _, err := parseExecArgs(arg); err == nil {
			t.Fatalf("expected error for invalid notify value in %v", arg)
		}
	}
	// Valid values still parse.
	for _, mode := range []string{"off", "bell", "notify", "both"} {
		if _, _, err := parseExecArgs([]string{"--notify", mode, "hi"}); err != nil {
			t.Fatalf("valid --notify %s rejected: %v", mode, err)
		}
	}
}

func TestExecNotifyModeResolution(t *testing.T) {
	resolved := config.ResolvedConfig{Notify: config.NotifyConfig{Mode: "bell"}}
	if got := execNotifyMode(execOptions{}, resolved); got != "bell" {
		t.Fatalf("config passthrough got %q", got)
	}
	if got := execNotifyMode(execOptions{notifyMode: "both"}, resolved); got != "both" {
		t.Fatalf("flag override got %q", got)
	}
	if got := execNotifyMode(execOptions{noNotify: true}, resolved); got != "off" {
		t.Fatalf("--no-notify got %q", got)
	}
}

func TestRunExecTopTierDeclineNoSwitch(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	cwd := t.TempDir()

	// Sanity: claude-opus-4.1 must genuinely be top-tier (no upgrade target), or
	// the scenario is meaningless.
	if _, ok := mustUpgradeTarget(t, "claude-opus-4.1"); ok {
		t.Skip("registry unexpectedly has an upgrade target for claude-opus-4.1")
	}

	var providerModels []string
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{
		"exec",
		"--allow-escalation",
		"--model", "claude-opus-4.1",
		"escalate please",
	}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(_ string, overrides config.Overrides) (config.ResolvedConfig, error) {
			model := "claude-opus-4.1"
			if overrides.Provider.Model != "" {
				model = overrides.Provider.Model
			}
			cfg := execResolvedConfig()
			cfg.Provider.ProviderKind = config.ProviderKindAnthropic
			cfg.Provider.Model = model
			cfg.MaxTurns = 3
			return cfg, nil
		},
		newProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			providerModels = append(providerModels, profile.Model)
			return &escalatingExecProvider{escalateOnce: true}, nil
		},
	})
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stdout=%s stderr=%s", exitCode, stdout.String(), stderr.String())
	}
	// The escalate_model call resolved to a no-target informational result, so the
	// loop never requested a switch and newProvider was built exactly once.
	if len(providerModels) != 1 {
		t.Fatalf("newProvider built for %#v, want exactly 1 (top-tier decline performs no switch)", providerModels)
	}
	if providerModels[0] != "claude-opus-4.1" {
		t.Fatalf("provider model = %q, want claude-opus-4.1", providerModels[0])
	}
}

func TestExecTurnBudgetDefaultsToCeilingUnlessSet(t *testing.T) {
	cases := []struct {
		name     string
		resolved config.ResolvedConfig
		want     int
	}{
		// A headless run has no user to continue past the shared interactive
		// default, so an unconfigured budget starts at the ceiling.
		{"unconfigured default", config.ResolvedConfig{MaxTurns: 80, MaxTurnsSet: false}, config.MaxTurnsCeiling},
		// A user who configured 80 on purpose (e.g. cost control in CI) keeps 80.
		{"explicit default value", config.ResolvedConfig{MaxTurns: 80, MaxTurnsSet: true}, 80},
		{"configured value passes through", config.ResolvedConfig{MaxTurns: 42, MaxTurnsSet: true}, 42},
	}
	for _, tc := range cases {
		if got := execTurnBudget(tc.resolved); got != tc.want {
			t.Fatalf("%s: execTurnBudget = %d, want %d", tc.name, got, tc.want)
		}
	}
}
