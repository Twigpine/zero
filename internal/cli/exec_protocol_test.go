package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/mcp"
	"github.com/Gitlawb/zero/internal/sandbox"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

func TestRunExecHelpDocumentsProtocolFlags(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := Run([]string{"exec", "--help"}, &stdout, &stderr)

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d", exitSuccess, exitCode)
	}
	for _, want := range []string{
		"--auto",
		"--enabled-tools",
		"--disabled-tools",
		"--list-tools",
		"--input-format text|stream-json",
		"--output-format text|json|stream-json",
		"--resume",
		"--fork",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("expected exec help to contain %q, got %q", want, stdout.String())
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
}

func TestRunExecListsFilteredToolsWithoutPromptOrProvider(t *testing.T) {
	cwd := t.TempDir()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	providerBuilt := false

	exitCode := runWithDeps([]string{"exec", "--list-tools", "--enabled-tools", "read_file,grep"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return execResolvedConfig(), nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			providerBuilt = true
			return nil, errors.New("provider should not be constructed for --list-tools")
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if stderr.String() != hostSandboxNotice(t) {
		t.Fatalf("expected no stderr beyond this host's sandbox notice, got %q", stderr.String())
	}
	if providerBuilt {
		t.Fatal("provider should not be constructed for --list-tools")
	}
	for _, want := range []string{"Tools visible to model", "read_file", "grep"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("expected tool list to contain %q, got %q", want, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "bash") {
		t.Fatalf("expected filtered tool list to hide bash, got %q", stdout.String())
	}
}

func TestRunExecListsToolsAsStreamJSONWhenRequested(t *testing.T) {
	cwd := t.TempDir()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	providerBuilt := false

	exitCode := runWithDeps([]string{"exec", "--list-tools", "--output-format", "stream-json", "--enabled-tools", "read_file"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return execResolvedConfig(), nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			providerBuilt = true
			return nil, errors.New("provider should not be constructed for --list-tools")
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if stderr.String() != hostSandboxNotice(t) {
		t.Fatalf("expected no stderr beyond this host's sandbox notice, got %q", stderr.String())
	}
	if providerBuilt {
		t.Fatal("provider should not be constructed for --list-tools")
	}
	events := decodeJSONLines(t, stdout.String())
	if len(events) != 3 {
		t.Fatalf("expected run_start, final, run_end events, got %#v", events)
	}
	if events[0]["type"] != "run_start" || events[1]["type"] != "final" || events[2]["type"] != "run_end" {
		t.Fatalf("unexpected stream-json tool list events: %#v", events)
	}
	text, _ := events[1]["text"].(string)
	if !strings.Contains(text, "Tools visible to model") || !strings.Contains(text, "read_file") {
		t.Fatalf("expected final event to contain tool list, got %#v", events[1])
	}
	for _, name := range []string{"bash", "grep", "write_file"} {
		if strings.Contains(text, name) {
			t.Fatalf("unexpected non-enabled tool %q leaked into stream-json output: %#v", name, events[1])
		}
	}
}

func TestRunExecListsMCPToolsWithoutProviderConstruction(t *testing.T) {
	cwd := t.TempDir()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	providerBuilt := false
	closed := false

	exitCode := runWithDeps([]string{"exec", "--list-tools", "--enabled-tools", "mcp_docs_lookup"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return execResolvedConfig(), nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			providerBuilt = true
			return nil, errors.New("provider should not be constructed for --list-tools")
		},
		resolveMCPConfig: func(workspaceRoot string, _ bool) (config.MCPConfig, error) {
			if workspaceRoot != cwd {
				t.Fatalf("workspaceRoot = %q, want %q", workspaceRoot, cwd)
			}
			return config.MCPConfig{Servers: map[string]config.MCPServerConfig{
				"docs": {Type: "stdio", Command: "docs-mcp"},
			}}, nil
		},
		registerMCPTools: func(ctx context.Context, registry *tools.Registry, cfg config.MCPConfig, options mcp.RegisterOptions) (mcpToolRuntime, error) {
			registry.Register(cliFakeMCPRegistryTool{})
			return closeFunc(func() error {
				closed = true
				return nil
			}), nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if providerBuilt {
		t.Fatal("provider should not be constructed for --list-tools")
	}
	if !closed {
		t.Fatal("MCP runtime was not closed after --list-tools")
	}
	if stderr.String() != hostSandboxNotice(t) {
		t.Fatalf("expected no stderr beyond this host's sandbox notice, got %q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "mcp_docs_lookup") || !strings.Contains(stdout.String(), "Lookup documentation") {
		t.Fatalf("expected MCP tool in list output, got %q", stdout.String())
	}
}

func TestRunExecLogsMCPRuntimeCloseError(t *testing.T) {
	cwd := t.TempDir()
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"exec", "--list-tools", "--enabled-tools", "mcp_docs_lookup"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveMCPConfig: func(workspaceRoot string, _ bool) (config.MCPConfig, error) {
			if workspaceRoot != cwd {
				t.Fatalf("workspaceRoot = %q, want %q", workspaceRoot, cwd)
			}
			return config.MCPConfig{Servers: map[string]config.MCPServerConfig{
				"docs": {Type: "stdio", Command: "docs-mcp"},
			}}, nil
		},
		newMCPStore: func() (*mcp.PermissionStore, error) {
			return nil, nil
		},
		registerMCPTools: func(ctx context.Context, registry *tools.Registry, cfg config.MCPConfig, options mcp.RegisterOptions) (mcpToolRuntime, error) {
			registry.Register(cliFakeMCPRegistryTool{})
			return closeFunc(func() error {
				return errors.New("close failed")
			}), nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "mcp_close_error: close failed") {
		t.Fatalf("stderr = %q, want MCP close error", stderr.String())
	}
}

func TestRunExecRejectsInvalidProtocolOptionsBeforeRuntime(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "auto", args: []string{"exec", "--auto", "chaos", "hello"}, want: "Invalid autonomy level"},
		{name: "enabled", args: []string{"exec", "--enabled-tools", "missing_tool", "hello"}, want: "Unknown tool"},
		{name: "overlap", args: []string{"exec", "--enabled-tools", "read_file", "--disabled-tools", "read_file", "hello"}, want: "both enabled and disabled"},
		{name: "input", args: []string{"exec", "--input-format", "yaml", "hello"}, want: "Invalid input format"},
		{name: "resume-fork", args: []string{"exec", "--resume", "abc", "--fork", "def", "hello"}, want: "Use either --resume or --fork, not both"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer

			exitCode := Run(tc.args, &stdout, &stderr)

			if exitCode != exitUsage {
				t.Fatalf("expected exit code %d, got %d", exitUsage, exitCode)
			}
			if stdout.Len() != 0 {
				t.Fatalf("expected empty stdout before runtime, got %q", stdout.String())
			}
			if got := stderr.String(); !strings.Contains(got, tc.want) {
				t.Fatalf("expected stderr to contain %q, got %q", tc.want, got)
			}
		})
	}
}

func TestRunExecRejectsFlagShapedMissingValues(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := Run([]string{"exec", "--model", "--output-format", "json", "hello"}, &stdout, &stderr)

	if exitCode != exitUsage {
		t.Fatalf("expected exit code %d, got %d", exitUsage, exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected empty stdout, got %q", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "--model requires a value") {
		t.Fatalf("expected missing model value error, got %q", got)
	}
}

func TestRunExecStreamJSONUsageErrorsStayInProtocol(t *testing.T) {
	cwd := t.TempDir()
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"exec", "--input-format", "stream-json", "--output-format", "stream-json"}, &stdout, &stderr, appDeps{
		stdin: strings.NewReader("{bad\n"),
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{}, errors.New("provider should not be resolved for invalid stream-json input")
		},
	})

	if exitCode != exitUsage {
		t.Fatalf("expected exit code %d, got %d", exitUsage, exitCode)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
	events := decodeJSONLines(t, stdout.String())
	if len(events) != 2 || events[0]["type"] != "error" || events[1]["type"] != "run_end" {
		t.Fatalf("expected stream-json error and run_end, got %#v", events)
	}
	if events[0]["code"] != "usage_error" || !strings.Contains(events[0]["message"].(string), "Invalid stream-json input") {
		t.Fatalf("expected usage error event, got %#v", events[0])
	}
	if events[1]["exitCode"] != float64(exitUsage) {
		t.Fatalf("expected usage exit code, got %#v", events[1])
	}
}

func TestRunExecStreamJSONOutputsRunEndAndRecordsSession(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	cwd := t.TempDir()
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"exec", "--output-format", "stream-json", "persist this"}, &stdout, &stderr, appDeps{
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
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if stderr.String() != hostSandboxNotice(t) {
		t.Fatalf("expected no stderr beyond this host's sandbox notice, got %q", stderr.String())
	}

	events := decodeJSONLines(t, stdout.String())
	types := jsonEventTypes(events)
	for _, want := range []string{"run_start", "text", "final", "run_end"} {
		if !slices.Contains(types, want) {
			t.Fatalf("expected event %q in %v; output %q", want, types, stdout.String())
		}
	}
	runStart := events[0]
	sessionID, ok := runStart["sessionId"].(string)
	if !ok || sessionID == "" {
		t.Fatalf("expected run_start sessionId, got %#v", runStart)
	}
	if runStart["provider"] != "openai-compatible" || runStart["apiModel"] != "echo-model" {
		t.Fatalf("expected resolved runtime metadata in run_start, got %#v", runStart)
	}
	if got := events[len(events)-1]["type"]; got != "run_end" {
		t.Fatalf("expected last event run_end, got %#v", events[len(events)-1])
	}

	store := sessions.NewStore(sessions.StoreOptions{
		RootDir: filepath.Join(dataHome, "zero", "sessions"),
	})
	recorded, err := store.ReadEvents(sessionID)
	if err != nil {
		t.Fatalf("ReadEvents returned error: %v", err)
	}
	if len(recorded) != 2 || recorded[0].Type != sessions.EventMessage || recorded[1].Type != sessions.EventMessage {
		t.Fatalf("recorded events = %#v", recorded)
	}
}

func TestRunExecStreamJSONEmitsAndRecordsPermissionEvents(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	cwd := t.TempDir()
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"exec", "--output-format", "stream-json", "write notes"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(_ string, _ config.Overrides) (config.ResolvedConfig, error) {
			return execResolvedConfig(), nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return toolCallingExecProvider{
				toolCallID: "call_write",
				toolName:   "write_file",
				arguments:  `{"path":"notes.txt","content":"hello"}`,
				answer:     "write allowed",
			}, nil
		},
		newSandboxStore: func() (*sandbox.GrantStore, error) {
			return sandbox.NewGrantStore(sandbox.StoreOptions{FilePath: filepath.Join(t.TempDir(), "sandbox-grants.json")})
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if stderr.String() != hostSandboxNotice(t) {
		t.Fatalf("expected no stderr beyond this host's sandbox notice, got %q", stderr.String())
	}

	events := decodeJSONLines(t, stdout.String())
	eventTypes := jsonEventTypes(events)
	if slices.Contains(eventTypes, "permission_request") {
		t.Fatalf("workspace write should not request permission in %v; output %q", eventTypes, stdout.String())
	}
	permissionEvent := findJSONEvent(t, events, "permission_decision")
	if permissionEvent["id"] != "call_write" || permissionEvent["name"] != "write_file" || permissionEvent["action"] != "allow" {
		t.Fatalf("unexpected permission event: %#v", permissionEvent)
	}
	if permissionEvent["permission"] != "prompt" || permissionEvent["permissionMode"] != "auto" || permissionEvent["sideEffect"] != "write" {
		t.Fatalf("unexpected permission metadata: %#v", permissionEvent)
	}
	if permissionEvent["permissionGranted"] == true {
		t.Fatalf("workspace allow should not be recorded as user-granted permission: %#v", permissionEvent)
	}
	if permissionEvent["reason"] != "workspace write is allowed" {
		t.Fatalf("unexpected workspace permission reason: %#v", permissionEvent)
	}
	risk, ok := permissionEvent["risk"].(map[string]any)
	if !ok || risk["level"] == "" {
		t.Fatalf("expected permission risk payload, got %#v", permissionEvent)
	}

	sessionID, ok := events[0]["sessionId"].(string)
	if !ok || sessionID == "" {
		t.Fatalf("expected run_start sessionId, got %#v", events[0])
	}
	store := sessions.NewStore(sessions.StoreOptions{
		RootDir: filepath.Join(dataHome, "zero", "sessions"),
	})
	recorded, err := store.ReadEvents(sessionID)
	if err != nil {
		t.Fatalf("ReadEvents returned error: %v", err)
	}
	permissionRecord := findSessionEvent(t, recorded, sessions.EventPermissionDecision)
	var payload map[string]any
	if err := json.Unmarshal(permissionRecord.Payload, &payload); err != nil {
		t.Fatalf("decode permission payload: %v", err)
	}
	if payload["toolCallId"] != "call_write" || payload["name"] != "write_file" || payload["action"] != "allow" {
		t.Fatalf("unexpected recorded permission payload: %#v", payload)
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = runWithDeps([]string{"exec", "--skip-permissions-unsafe", "--output-format", "stream-json", "write approved"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(_ string, _ config.Overrides) (config.ResolvedConfig, error) {
			return execResolvedConfig(), nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return toolCallingExecProvider{
				toolCallID: "call_write_approved",
				toolName:   "write_file",
				arguments:  `{"path":"approved.txt","content":"hello"}`,
				answer:     "write approved",
			}, nil
		},
		newSandboxStore: func() (*sandbox.GrantStore, error) {
			return sandbox.NewGrantStore(sandbox.StoreOptions{FilePath: filepath.Join(t.TempDir(), "sandbox-grants.json")})
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("approved exec exit = %d, stderr = %q", exitCode, stderr.String())
	}
	if stderr.String() != hostSandboxNotice(t) {
		t.Fatalf("approved exec wrote stderr = %q", stderr.String())
	}
	approvedEvents := decodeJSONLines(t, stdout.String())
	approvedPermissionEvent := findJSONEvent(t, approvedEvents, "permission_decision")
	if approvedPermissionEvent["id"] != "call_write_approved" || approvedPermissionEvent["action"] != "allow" {
		t.Fatalf("unexpected approved permission event: %#v", approvedPermissionEvent)
	}
	if approvedPermissionEvent["permissionGranted"] != true {
		t.Fatalf("approved permission event did not preserve permissionGranted: %#v", approvedPermissionEvent)
	}
	approvedSessionID, ok := approvedEvents[0]["sessionId"].(string)
	if !ok || approvedSessionID == "" {
		t.Fatalf("expected approved run_start sessionId, got %#v", approvedEvents[0])
	}
	approvedRecorded, err := store.ReadEvents(approvedSessionID)
	if err != nil {
		t.Fatalf("ReadEvents approved session returned error: %v", err)
	}
	approvedPermissionRecord := findSessionEvent(t, approvedRecorded, sessions.EventPermissionDecision)
	payload = map[string]any{}
	if err := json.Unmarshal(approvedPermissionRecord.Payload, &payload); err != nil {
		t.Fatalf("decode approved permission payload: %v", err)
	}
	if payload["permissionGranted"] != true {
		t.Fatalf("approved recorded permission payload did not preserve permissionGranted: %#v", payload)
	}
}

func TestRunExecStreamJSONRunStartUsesResolvedAPIModel(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	cwd := t.TempDir()
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"exec", "--output-format", "stream-json", "hello"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(_ string, _ config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{
				ActiveProvider: "work",
				Provider: config.ProviderProfile{
					Name:  "work",
					Model: "sonnet-4.5",
				},
				MaxTurns: 2,
			}, nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return echoExecProvider{}, nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	events := decodeJSONLines(t, stdout.String())
	runStart := events[0]
	if runStart["provider"] != "anthropic" {
		t.Fatalf("expected resolved provider kind anthropic, got %#v", runStart)
	}
	if runStart["model"] != "sonnet-4.5" {
		t.Fatalf("expected logical model alias, got %#v", runStart)
	}
	if runStart["apiModel"] != "claude-sonnet-4-5-20250929" {
		t.Fatalf("expected resolved API model, got %#v", runStart)
	}
}

func TestRunExecStreamJSONEmitsReasoningEvents(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	cwd := t.TempDir()
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"exec", "--output-format", "stream-json", "think then answer"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(_ string, _ config.Overrides) (config.ResolvedConfig, error) {
			return execResolvedConfig(), nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return reasoningExecProvider{}, nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	events := decodeJSONLines(t, stdout.String())
	reasoningIdx, reasoning := findJSONEventIndex(t, events, "reasoning")
	if reasoning["delta"] != "Thinking. " {
		t.Fatalf("unexpected reasoning event: %#v", reasoning)
	}
	textIdx, text := findJSONEventIndex(t, events, "text")
	if text["delta"] != "done" {
		t.Fatalf("unexpected text event: %#v", text)
	}
	if reasoningIdx >= textIdx {
		t.Fatalf("expected reasoning event before text event, got indices %d and %d", reasoningIdx, textIdx)
	}
	final := findJSONEvent(t, events, "final")
	if final["text"] != "done" {
		t.Fatalf("reasoning must not be folded into final answer: %#v", final)
	}
}

func TestExecEventWriterTruncatesStreamJSONToolResults(t *testing.T) {
	var stdout bytes.Buffer
	writer := execEventWriter{
		stdout:       &stdout,
		format:       execOutputStreamJSON,
		runID:        "run_test",
		streamedText: &strings.Builder{},
	}
	writer.toolResult(agent.ToolResult{
		ToolCallID: "call_1",
		Name:       "read_file",
		Status:     tools.StatusOK,
		Output:     strings.Repeat("x", streamJSONToolResultOutputLimit+100),
		Meta: map[string]string{
			"sandbox_backend": "bubblewrap",
			"sandbox_wrapped": "true",
		},
	})
	if writer.err != nil {
		t.Fatalf("toolResult returned writer error: %v", writer.err)
	}

	events := decodeJSONLines(t, stdout.String())
	if len(events) != 1 {
		t.Fatalf("expected one tool_result event, got %#v", events)
	}
	if events[0]["truncated"] != true {
		t.Fatalf("expected truncated=true, got %#v", events[0])
	}
	if events[0]["name"] != "read_file" {
		t.Fatalf("expected tool_result name to be read_file, got %#v", events[0])
	}
	meta := events[0]["meta"].(map[string]any)
	if meta["sandbox_backend"] != "bubblewrap" || meta["sandbox_wrapped"] != "true" {
		t.Fatalf("expected sandbox metadata, got %#v", meta)
	}
	output := events[0]["output"].(string)
	if len(output) >= streamJSONToolResultOutputLimit+100 || !strings.Contains(output, "[truncated]") {
		t.Fatalf("expected shortened output with marker, got len=%d", len(output))
	}
}

func TestExecEventWriterEmitsPermissionDecisionReason(t *testing.T) {
	var stdout bytes.Buffer
	writer := execEventWriter{
		stdout:       &stdout,
		format:       execOutputStreamJSON,
		runID:        "run_test",
		sessionID:    "session_test",
		streamedText: &strings.Builder{},
	}

	writer.permission(agent.PermissionEvent{
		ToolCallID:        "call_1",
		ToolName:          "write_file",
		Action:            agent.PermissionActionAllow,
		Permission:        string(tools.PermissionPrompt),
		PermissionGranted: true,
		PermissionMode:    agent.PermissionModeAuto,
		SideEffect:        string(tools.SideEffectWrite),
		Reason:            "write access required",
		DecisionReason:    "approved by operator",
	})

	if writer.err != nil {
		t.Fatalf("permission returned writer error: %v", writer.err)
	}
	events := decodeJSONLines(t, stdout.String())
	if len(events) != 1 {
		t.Fatalf("expected one permission event, got %#v", events)
	}
	if events[0]["type"] != "permission_decision" {
		t.Fatalf("expected permission_decision event, got %#v", events[0])
	}
	if events[0]["decisionReason"] != "approved by operator" {
		t.Fatalf("expected decisionReason to be preserved, got %#v", events[0])
	}
}

func TestRunExecStreamJSONProviderErrorEmitsErrorAndRunEnd(t *testing.T) {
	cwd := t.TempDir()
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"exec", "--output-format", "stream-json", "hello"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(_ string, _ config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{}, errors.New("provider failed")
		},
	})

	if exitCode != exitProvider {
		t.Fatalf("expected provider exit %d, got %d", exitProvider, exitCode)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
	events := decodeJSONLines(t, stdout.String())
	if len(events) != 2 {
		t.Fatalf("expected error and run_end events, got %#v", events)
	}
	if events[0]["type"] != "error" || events[0]["code"] != "provider_error" {
		t.Fatalf("expected provider error event, got %#v", events[0])
	}
	if events[1]["type"] != "run_end" || events[1]["exitCode"] != float64(exitProvider) {
		t.Fatalf("expected run_end provider exit, got %#v", events[1])
	}
	if events[0]["runId"] != events[1]["runId"] {
		t.Fatalf("expected matching runId, got %#v", events)
	}
}

func TestRunExecJSONInterruptedEmitsTerminalEvents(t *testing.T) {
	cwd := t.TempDir()
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"exec", "--output-format", "json", "hello"}, &stdout, &stderr, appDeps{
		getwd:         func() (string, error) { return cwd, nil },
		resolveConfig: func(_ string, _ config.Overrides) (config.ResolvedConfig, error) { return execResolvedConfig(), nil },
		newProvider:   func(config.ProviderProfile) (zeroruntime.Provider, error) { return canceledExecProvider{}, nil },
	})

	if exitCode != exitInterrupted {
		t.Fatalf("expected interrupted exit %d, got %d: %s", exitInterrupted, exitCode, stderr.String())
	}
	// -o json must end the stream with a terminal error+done, not just print
	// "Interrupted." to stderr (which the stream-json path already avoided).
	events := decodeJSONLines(t, stdout.String())
	if len(events) < 2 {
		t.Fatalf("expected a terminal error+done on -o json interruption, got %#v", events)
	}
	errEvent := events[len(events)-2]
	doneEvent := events[len(events)-1]
	if errEvent["type"] != "error" || errEvent["code"] != "interrupted" {
		t.Fatalf("expected interrupted error event, got %#v", errEvent)
	}
	if doneEvent["type"] != "done" || doneEvent["exit_code"] != float64(exitInterrupted) {
		t.Fatalf("expected done event with interrupted exit code, got %#v", doneEvent)
	}
}

func TestRunExecWarnsAboutScratchFileWhenProviderErrors(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	cwd := t.TempDir()
	runGitForScratchTest(t, cwd, "init")
	if err := os.WriteFile(filepath.Join(cwd, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitForScratchTest(t, cwd, "add", "README.md")
	runGitForScratchTest(t, cwd, "commit", "-m", "init")
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"exec", "--output-format", "stream-json", "write scratch then fail"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return cwd, nil },
		resolveConfig: func(_ string, _ config.Overrides) (config.ResolvedConfig, error) {
			return execResolvedConfig(), nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return toolThenErrorExecProvider{
				toolCallID: "call_scratch",
				toolName:   "write_file",
				arguments:  `{"path":"_debug.py","content":"print('debug')"}`,
				err:        errors.New("provider failed after tool"),
			}, nil
		},
		newSandboxStore: func() (*sandbox.GrantStore, error) {
			return sandbox.NewGrantStore(sandbox.StoreOptions{FilePath: filepath.Join(t.TempDir(), "sandbox-grants.json")})
		},
	})

	if exitCode != exitProvider {
		t.Fatalf("expected provider exit %d, got %d: stdout=%q stderr=%q", exitProvider, exitCode, stdout.String(), stderr.String())
	}
	events := decodeJSONLines(t, stdout.String())
	warning := findJSONEvent(t, events, "warning")
	message, _ := warning["message"].(string)
	if !strings.Contains(message, "_debug.py") {
		t.Fatalf("expected scratch warning to mention _debug.py, got %#v", warning)
	}
}

func TestRunExecReadsStreamJSONPromptFromStdin(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	cwd := t.TempDir()
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"exec", "--input-format", "stream-json", "--output-format", "stream-json"}, &stdout, &stderr, appDeps{
		stdin: strings.NewReader(`{"schemaVersion":2,"type":"prompt","content":"from stdin"}` + "\n"),
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
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	events := decodeJSONLines(t, stdout.String())
	final := events[len(events)-2]
	if final["type"] != "final" || final["text"] != "from stdin" {
		t.Fatalf("expected final event from stdin, got %#v", final)
	}
}

type cliFakeMCPRegistryTool struct{}

func (cliFakeMCPRegistryTool) Name() string {
	return "mcp_docs_lookup"
}

func (cliFakeMCPRegistryTool) Description() string {
	return "Lookup documentation"
}

func (cliFakeMCPRegistryTool) Parameters() tools.Schema {
	return tools.Schema{
		Type:                 "object",
		AdditionalProperties: false,
		Properties: map[string]tools.PropertySchema{
			"query": {Type: "string"},
		},
	}
}

func (cliFakeMCPRegistryTool) Safety() tools.Safety {
	return tools.Safety{
		SideEffect: tools.SideEffectNetwork,
		Permission: tools.PermissionAllow,
		Reason:     "MCP test tool",
	}
}

func (cliFakeMCPRegistryTool) Run(context.Context, map[string]any) tools.Result {
	return tools.Result{Status: tools.StatusOK, Output: "ok"}
}

type closeFunc func() error

func (fn closeFunc) Close() error {
	return fn()
}

func (fn closeFunc) Skipped() []mcp.SkippedServer {
	return nil
}

type toolCallingExecProvider struct {
	toolCallID string
	toolName   string
	arguments  string
	answer     string
}

type toolThenErrorExecProvider struct {
	toolCallID string
	toolName   string
	arguments  string
	err        error
}

type reasoningExecProvider struct{}

func (reasoningExecProvider) StreamCompletion(context.Context, zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	ch := make(chan zeroruntime.StreamEvent, 3)
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventReasoning, Content: "Thinking. "}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventText, Content: "done"}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}
	close(ch)
	return ch, nil
}

func (provider toolCallingExecProvider) StreamCompletion(ctx context.Context, request zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	for _, message := range request.Messages {
		if message.Role == zeroruntime.MessageRoleTool {
			ch := make(chan zeroruntime.StreamEvent, 2)
			ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventText, Content: provider.answer}
			ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}
			close(ch)
			return ch, nil
		}
	}
	ch := make(chan zeroruntime.StreamEvent, 4)
	select {
	case <-ctx.Done():
		close(ch)
		return ch, ctx.Err()
	case ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: provider.toolCallID, ToolName: provider.toolName}:
	}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: provider.toolCallID, ArgumentsFragment: provider.arguments}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: provider.toolCallID}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}
	close(ch)
	return ch, nil
}

func (provider toolThenErrorExecProvider) StreamCompletion(ctx context.Context, request zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	for _, message := range request.Messages {
		if message.Role == zeroruntime.MessageRoleTool {
			return nil, provider.err
		}
	}
	ch := make(chan zeroruntime.StreamEvent, 4)
	select {
	case <-ctx.Done():
		close(ch)
		return ch, ctx.Err()
	case ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: provider.toolCallID, ToolName: provider.toolName}:
	}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: provider.toolCallID, ArgumentsFragment: provider.arguments}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: provider.toolCallID}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}
	close(ch)
	return ch, nil
}

func findJSONEvent(t *testing.T, events []map[string]any, eventType string) map[string]any {
	t.Helper()
	_, event := findJSONEventIndex(t, events, eventType)
	return event
}

func findJSONEventIndex(t *testing.T, events []map[string]any, eventType string) (int, map[string]any) {
	t.Helper()
	for idx, event := range events {
		if event["type"] == eventType {
			return idx, event
		}
	}
	t.Fatalf("event %q not found in %#v", eventType, events)
	return -1, nil
}

func findSessionEvent(t *testing.T, events []sessions.Event, eventType sessions.EventType) sessions.Event {
	t.Helper()
	for _, event := range events {
		if event.Type == eventType {
			return event
		}
	}
	t.Fatalf("session event %q not found in %#v", eventType, events)
	return sessions.Event{}
}
