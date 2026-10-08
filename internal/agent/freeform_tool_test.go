package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/sandbox"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

type customApplyPatchTool struct {
	calls int
}

type fixedPathScope struct {
	roots []string
}

func (scope fixedPathScope) Roots() []string {
	return append([]string(nil), scope.roots...)
}

func newFixedPathScope(t *testing.T, roots ...string) fixedPathScope {
	t.Helper()
	resolved := make([]string, 0, len(roots))
	for _, root := range roots {
		absolute, err := filepath.Abs(root)
		if err != nil {
			t.Fatalf("Abs(%q): %v", root, err)
		}
		if physical, err := filepath.EvalSymlinks(absolute); err == nil {
			absolute = physical
		}
		resolved = append(resolved, absolute)
	}
	return fixedPathScope{roots: resolved}
}

func (*customApplyPatchTool) Name() string        { return "apply_patch" }
func (*customApplyPatchTool) Description() string { return "custom JSON tool" }
func (*customApplyPatchTool) Parameters() tools.Schema {
	return tools.Schema{Type: "object", Properties: map[string]tools.PropertySchema{"value": {Type: "string"}}}
}
func (*customApplyPatchTool) Safety() tools.Safety {
	return tools.Safety{Permission: tools.PermissionAllow, SideEffect: tools.SideEffectRead}
}
func (tool *customApplyPatchTool) Run(context.Context, map[string]any) tools.Result {
	tool.calls++
	return tools.Result{Status: tools.StatusOK}
}

func TestFreeformApplyPatchUsesExistingToolExecutionPath(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedApplyPatchTool(root, nil))
	patch := "*** Begin Patch\n*** Add File: hello.txt\n+hello\n*** End Patch\n"

	result, err := executeToolCall(context.Background(), registry, zeroruntime.ToolCall{
		ID:        "call-1",
		Name:      "apply_patch",
		Arguments: patch,
		Freeform:  true,
	}, PermissionModeUnsafe, Options{Cwd: root, FileTracker: tools.NewFileTracker()})
	if err != nil {
		t.Fatalf("executeToolCall: %v", err)
	}
	if result.Status != tools.StatusOK {
		t.Fatalf("freeform apply_patch status = %s: %s", result.Status, result.Output)
	}
	content, err := os.ReadFile(filepath.Join(root, "hello.txt"))
	if err != nil {
		t.Fatalf("read patched file: %v", err)
	}
	if string(content) != "hello\n" {
		t.Fatalf("patched content = %q, want hello newline", content)
	}
}

func TestFreeformApplyPatchSupportsGrantedExtraRoot(t *testing.T) {
	root := t.TempDir()
	extra := t.TempDir()
	scope, err := sandbox.NewScope(root, []string{extra})
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedApplyPatchTool(root, scope))
	target := filepath.Join(extra, "hello.txt")
	patch := "*** Begin Patch\n*** Add File: " + target + "\n+hello\n*** End Patch\n"

	result, err := executeToolCall(context.Background(), registry, zeroruntime.ToolCall{
		ID: "call-1", Name: "apply_patch", Arguments: patch, Freeform: true,
	}, PermissionModeUnsafe, Options{Cwd: root, FileTracker: tools.NewFileTracker()})
	if err != nil {
		t.Fatalf("executeToolCall: %v", err)
	}
	if result.Status != tools.StatusOK {
		t.Fatalf("freeform extra-root apply_patch status = %s: %s", result.Status, result.Output)
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read extra-root patched file: %v", err)
	}
	if string(content) != "hello\n" {
		t.Fatalf("patched content = %q, want hello newline", content)
	}
}

func TestFreeformApplyPatchRejectsMixedWorkspaceAndExtraRoots(t *testing.T) {
	for _, extraHasMatchingPath := range []bool{false, true} {
		name := "extra target missing"
		if extraHasMatchingPath {
			name = "extra target exists"
		}
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			extra := t.TempDir()
			if err := os.MkdirAll(filepath.Join(workspace, "src"), 0o755); err != nil {
				t.Fatal(err)
			}
			workspaceTarget := filepath.Join(workspace, "src", "a.go")
			if err := os.WriteFile(workspaceTarget, []byte("old\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			extraTarget := filepath.Join(extra, "src", "a.go")
			if extraHasMatchingPath {
				if err := os.MkdirAll(filepath.Dir(extraTarget), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(extraTarget, []byte("old\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			scope, err := sandbox.NewScope(workspace, []string{extra})
			if err != nil {
				t.Fatal(err)
			}
			registry := tools.NewRegistry()
			registry.Register(tools.NewScopedApplyPatchTool(workspace, scope))
			extraCreated := filepath.Join(extra, "b.go")
			patch := strings.Join([]string{
				"*** Begin Patch",
				"*** Update File: src/a.go",
				"@@",
				"-old",
				"+new",
				"*** Add File: " + extraCreated,
				"+extra",
				"*** End Patch",
			}, "\n")

			result, err := executeToolCall(context.Background(), registry, zeroruntime.ToolCall{
				ID: "call-1", Name: "apply_patch", Arguments: patch, Freeform: true,
			}, PermissionModeUnsafe, Options{Cwd: workspace, FileTracker: tools.NewFileTracker()})
			if err != nil {
				t.Fatalf("executeToolCall: %v", err)
			}
			if result.Status != tools.StatusError || !strings.Contains(result.Output, "mixes workspace-relative paths with an extra write root") {
				t.Fatalf("mixed-root patch result = %s: %s", result.Status, result.Output)
			}
			if content, err := os.ReadFile(workspaceTarget); err != nil || string(content) != "old\n" {
				t.Fatalf("workspace target changed: content=%q err=%v", content, err)
			}
			if extraHasMatchingPath {
				if content, err := os.ReadFile(extraTarget); err != nil || string(content) != "old\n" {
					t.Fatalf("extra-root target changed: content=%q err=%v", content, err)
				}
			}
			if _, err := os.Stat(extraCreated); !os.IsNotExist(err) {
				t.Fatalf("mixed-root patch created extra file: %v", err)
			}
		})
	}
}

func TestFreeformApplyPatchAcceptsOneSemanticRoot(t *testing.T) {
	for _, test := range []struct {
		name         string
		useExtraRoot bool
	}{
		{name: "workspace"},
		{name: "extra root", useExtraRoot: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			extra := t.TempDir()
			scope, err := sandbox.NewScope(workspace, []string{extra})
			if err != nil {
				t.Fatal(err)
			}
			root := workspace
			if test.useExtraRoot {
				root = extra
			}
			if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, "src", "a.go")
			if err := os.WriteFile(target, []byte("old\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			updatePath := target
			if !test.useExtraRoot {
				updatePath = filepath.Join("src", "a.go")
			}
			created := filepath.Join(root, "b.go")
			patch := strings.Join([]string{
				"*** Begin Patch",
				"*** Update File: " + updatePath,
				"@@",
				"-old",
				"+new",
				"*** Add File: " + created,
				"+created",
				"*** End Patch",
			}, "\n")
			registry := tools.NewRegistry()
			registry.Register(tools.NewScopedApplyPatchTool(workspace, scope))
			result, err := executeToolCall(context.Background(), registry, zeroruntime.ToolCall{
				ID: "call-1", Name: "apply_patch", Arguments: patch, Freeform: true,
			}, PermissionModeUnsafe, Options{Cwd: workspace, FileTracker: tools.NewFileTracker()})
			if err != nil {
				t.Fatalf("executeToolCall: %v", err)
			}
			if result.Status != tools.StatusOK {
				t.Fatalf("single-root patch status = %s: %s", result.Status, result.Output)
			}
			if content, err := os.ReadFile(target); err != nil || string(content) != "new\n" {
				t.Fatalf("updated content=%q err=%v", content, err)
			}
			if content, err := os.ReadFile(created); err != nil || string(content) != "created\n" {
				t.Fatalf("created content=%q err=%v", content, err)
			}
		})
	}
}

func TestFreeformApplyPatchRejectsAbsolutePathOutsideGrantedRoots(t *testing.T) {
	root := t.TempDir()
	granted := t.TempDir()
	outside := t.TempDir()
	scope := newFixedPathScope(t, root, granted)
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedApplyPatchTool(root, scope))
	target := filepath.Join(outside, "blocked.txt")
	patch := "*** Begin Patch\n*** Add File: " + target + "\n+blocked\n*** End Patch\n"

	result, err := executeToolCall(context.Background(), registry, zeroruntime.ToolCall{
		ID: "call-1", Name: "apply_patch", Arguments: patch, Freeform: true,
	}, PermissionModeUnsafe, Options{Cwd: root})
	if err != nil {
		t.Fatalf("executeToolCall: %v", err)
	}
	if result.Status != tools.StatusError {
		t.Fatalf("ungranted absolute patch status = %s, want error: %s", result.Status, result.Output)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("ungranted absolute patch created target: %v", err)
	}
}

func TestPreparedFreeformPatchRejectsIntermediateSymlinkSwap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating directory symlinks requires privileges on Windows")
	}
	root := t.TempDir()
	extra := t.TempDir()
	outside := t.TempDir()
	intermediate := filepath.Join(extra, "nested")
	if err := os.Mkdir(intermediate, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	scope := newFixedPathScope(t, root, extra)
	tool := tools.NewScopedApplyPatchTool(root, scope)
	target := filepath.Join(intermediate, "blocked.txt")
	patch := "*** Begin Patch\n*** Add File: " + target + "\n+blocked\n*** End Patch\n"
	args, err := tools.PrepareFreeformApplyPatchArguments(tool, patch)
	if err != nil {
		t.Fatalf("PrepareFreeformApplyPatchArguments: %v", err)
	}

	if err := os.Remove(intermediate); err != nil {
		t.Fatalf("Remove intermediate: %v", err)
	}
	if err := os.Symlink(outside, intermediate); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	result := tool.Run(context.Background(), args)
	if result.Status != tools.StatusError {
		t.Fatalf("symlink-swapped patch status = %s, want error: %s", result.Status, result.Output)
	}
	if _, err := os.Stat(filepath.Join(outside, "blocked.txt")); !os.IsNotExist(err) {
		t.Fatalf("symlink-swapped patch wrote outside root: %v", err)
	}
}

func TestUnknownFreeformToolFailsClosed(t *testing.T) {
	result, err := executeToolCall(context.Background(), tools.NewRegistry(), zeroruntime.ToolCall{
		ID: "call-1", Name: "unknown", Arguments: "raw", Freeform: true,
	}, PermissionModeUnsafe, Options{})
	if err != nil {
		t.Fatalf("executeToolCall: %v", err)
	}
	if result.Status != tools.StatusError {
		t.Fatalf("unknown freeform status = %s, want error", result.Status)
	}
}

func TestCustomApplyPatchCannotUseFreeformContract(t *testing.T) {
	custom := &customApplyPatchTool{}
	registry := tools.NewRegistry()
	registry.Register(custom)

	result, err := executeToolCall(context.Background(), registry, zeroruntime.ToolCall{
		ID: "call-1", Name: "apply_patch", Arguments: "raw patch", Freeform: true,
	}, PermissionModeUnsafe, Options{})
	if err != nil {
		t.Fatalf("executeToolCall: %v", err)
	}
	if result.Status != tools.StatusError || !strings.Contains(result.Output, "Unsupported freeform tool call") {
		t.Fatalf("custom freeform apply_patch result = %#v, want unsupported error", result)
	}
	if custom.calls != 0 {
		t.Fatalf("custom apply_patch executed %d times", custom.calls)
	}
}

func TestDeniedFreeformApplyPatchDoesNotWrite(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedApplyPatchTool(root, nil))
	patch := "*** Begin Patch\n*** Add File: denied.txt\n+must not exist\n*** End Patch\n"

	result, err := executeToolCall(context.Background(), registry, zeroruntime.ToolCall{
		ID:        "call-1",
		Name:      "apply_patch",
		Arguments: patch,
		Freeform:  true,
	}, PermissionModeAsk, Options{
		Cwd: root,
		OnPermissionRequest: func(context.Context, PermissionRequest) (PermissionDecision, error) {
			return PermissionDecision{Action: PermissionDecisionDeny, Reason: "test denial"}, nil
		},
	})
	if err != nil {
		t.Fatalf("executeToolCall: %v", err)
	}
	if result.Status != tools.StatusError {
		t.Fatalf("denied freeform status = %s, want error", result.Status)
	}
	if _, err := os.Stat(filepath.Join(root, "denied.txt")); !os.IsNotExist(err) {
		t.Fatalf("denied freeform patch changed the workspace: %v", err)
	}
}

func TestHistorySafeToolCallsPreservesRawFreeformArguments(t *testing.T) {
	raw := "*** Begin Patch\n*** Update File: internal/main.go\n@@\n-old\n+new\n*** End Patch\n"
	safe := historySafeToolCalls([]ToolCall{{
		ID: "call-1", ProviderCallID: "provider-1", Name: "apply_patch", Arguments: raw, Freeform: true,
	}})
	if len(safe) != 1 || safe[0].Arguments != raw || safe[0].ProviderCallID != "provider-1" || !safe[0].Freeform {
		t.Fatalf("freeform history changed raw call: %#v", safe)
	}
}

func TestAbortedToolResultPreservesProviderCallID(t *testing.T) {
	messages := appendAbortedToolResults(nil, []ToolCall{{
		ID: "call-1", ProviderCallID: "provider-1", Name: "read_file",
	}})
	if len(messages) != 1 || messages[0].ToolCallID != "call-1" || messages[0].ToolCallProviderID != "provider-1" || !messages[0].IsError {
		t.Fatalf("aborted tool result lost provider identity: %#v", messages)
	}
}
