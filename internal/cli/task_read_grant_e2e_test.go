package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/sandbox"
	"github.com/Twigpine/zero/internal/specialist"
	"github.com/Twigpine/zero/internal/streamjson"
	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// echoToolResultProvider calls one tool, then answers with what the tool
// returned, so the run's output shows whether the call succeeded.
type echoToolResultProvider struct {
	toolName  string
	arguments string
}

func (provider echoToolResultProvider) StreamCompletion(_ context.Context, request zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	for _, message := range request.Messages {
		if message.Role == zeroruntime.MessageRoleTool {
			ch := make(chan zeroruntime.StreamEvent, 2)
			ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventText,
				Content: fmt.Sprintf("TOOL-RESULT error=%v: %s", message.IsError, message.Content)}
			ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}
			close(ch)
			return ch, nil
		}
	}
	ch := make(chan zeroruntime.StreamEvent, 4)
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call_probe", ToolName: provider.toolName}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call_probe", ArgumentsFragment: provider.arguments}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call_probe"}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}
	close(ch)
	return ch, nil
}

// AN ORDINARY TASK'S CHILD CAN READ THE PARENT'S READ GRANT AND CANNOT WRITE IT.
//
// End to end on one path: the parent's scope holds a read grant outside its
// workspace; the Task tool launches a write-capable child through the executor
// the CLI wires; and the argv that launch produced is run through the real exec
// entry point, where read_file inside the grant succeeds and write_file inside
// it is refused. Before the executor carried the read supplier, the read failed
// "outside the workspace"; had the grant been emitted as --add-dir, the write
// would land.
//
// The resumed launch is covered at the argv level only
// (TestTheCLIExecutorPassesReadGrantsReadOnlyToTaskAndResume): exec --resume
// refuses a child session ("not resumable") on this tree, independently of the
// roots it is given, so a resumed child cannot be run in-process here.
func TestAnOrdinaryTaskChildCanReadButNotWriteTheParentsReadGrant(t *testing.T) {
	// Session state for the in-process child, redirected on every platform
	// (HOME covers macOS and Windows).
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	readRoot := tempDirOutsideDefaultTemp(t)
	evidence := filepath.Join(readRoot, "evidence.txt")
	if err := os.WriteFile(evidence, []byte("READ-GRANT-EVIDENCE"), 0o644); err != nil {
		t.Fatal(err)
	}
	parentScope, err := sandbox.NewScope(workspace, nil)
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	if _, err := parentScope.AddRead(readRoot); err != nil {
		t.Fatalf("AddRead: %v", err)
	}

	launches := 0
	var childArgs []string
	executor := specialist.Executor{
		BinaryPath: "/bin/true",
		NewSessionID: func() (string, error) {
			launches++
			return fmt.Sprintf("specialist_%024d", launches), nil
		},
		Load: func(specialist.LoadOptions) (specialist.LoadResult, error) {
			return specialist.LoadResult{Specialists: []specialist.Manifest{{
				Metadata:      specialist.Metadata{Name: "auditor", Description: "reads and writes"},
				SystemPrompt:  "x",
				ResolvedTools: []string{"read_file", "write_file"},
				ToolsResolved: true,
				Location:      specialist.LocationBuiltin,
			}}}, nil
		},
		RunChild: func(_ context.Context, _ string, args []string, _ func(streamjson.Event)) (specialist.ChildRunResult, error) {
			childArgs = append([]string(nil), args...)
			return specialist.ChildRunResult{Started: true}, nil
		},
	}
	wireChildRoots(&executor, workspace, orchestrateWiring{
		ExtraWriteRoots: func(withinTurn bool) []string { return childWriteRoots(parentScope, withinTurn) },
		ExtraReadRoots:  func(withinTurn bool) []string { return childReadRoots(parentScope, withinTurn) },
	})
	taskTool := specialist.NewTaskTool(executor)

	// launchAndRun runs the Task tool with taskArgs and then the child it would
	// have started, with a provider that makes the given tool call.
	launchAndRun := func(taskArgs map[string]any, toolName string, arguments map[string]string) string {
		t.Helper()
		result := taskTool.RunWithOptions(context.Background(), taskArgs,
			// Unsafe, so the child's tool layer — not a permission prompt — is
			// what decides whether the write may land.
			tools.RunOptions{Cwd: workspace, PermissionMode: "unsafe"})
		if result.Status != tools.StatusOK {
			t.Fatalf("Task launch failed: %s", result.Output)
		}
		if len(childArgs) == 0 || childArgs[0] != "exec" {
			t.Fatalf("unexpected child argv: %v", childArgs)
		}
		encoded, err := json.Marshal(arguments)
		if err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		exitCode := runWithDeps(childArgs, &stdout, &stderr, appDeps{
			getwd: func() (string, error) { return workspace, nil },
			resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
				return execResolvedConfig(), nil
			},
			newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
				return echoToolResultProvider{toolName: toolName, arguments: string(encoded)}, nil
			},
			newSandboxStore: func() (*sandbox.GrantStore, error) {
				return sandbox.NewGrantStore(sandbox.StoreOptions{FilePath: filepath.Join(t.TempDir(), "sandbox-grants.json")})
			},
		})
		if exitCode != exitSuccess {
			t.Fatalf("child exit = %d; argv=%v stdout=%q stderr=%q", exitCode, childArgs, stdout.String(), stderr.String())
		}
		return stdout.String()
	}

	task := map[string]any{"name": "auditor", "prompt": "probe the grant"}
	readOutput := launchAndRun(task, "read_file", map[string]string{"path": evidence})
	if !strings.Contains(readOutput, "READ-GRANT-EVIDENCE") || strings.Contains(readOutput, "error=true") {
		t.Fatalf("the child could not read the parent's read grant %q:\nargv=%v\n%s", readRoot, childArgs, readOutput)
	}
	target := filepath.Join(readRoot, "must-not-write.txt")
	writeOutput := launchAndRun(task, "write_file", map[string]string{"path": target, "content": "escalated"})
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("the child WROTE inside the parent's read grant — a read grant escalated to write (stat err=%v):\nargv=%v\n%s",
			err, childArgs, writeOutput)
	}
	if !strings.Contains(writeOutput, "error=true") {
		t.Fatalf("the write inside the read grant was not reported as refused:\n%s", writeOutput)
	}
}
