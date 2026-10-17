package specialist

import (
	"context"
	"testing"

	"github.com/Twigpine/zero/internal/streamjson"
	"github.com/Twigpine/zero/internal/tools"
)

// TestTaskToolForwardsPermissionMode guards the fix for the Task tool dropping the
// parent's permission mode: a non-unsafe parent must yield a non-unsafe ("--auto
// low") child so an approved sub-agent never gains more authority than its parent.
func TestTaskToolForwardsPermissionMode(t *testing.T) {
	for _, tc := range []struct {
		name           string
		permissionMode string
		wantAuto       string
	}{
		{"non-unsafe parent yields non-unsafe child", "auto", "low"},
		{"unsafe parent yields unsafe child", "unsafe", "high"},
		{"empty is fail-safe low (no silent escalation)", "", "low"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			zero := 0
			var gotArgs []string
			executor := Executor{
				BinaryPath:   "/usr/local/bin/zero",
				NewSessionID: func() (string, error) { return "child_task", nil },
				Load: func(LoadOptions) (LoadResult, error) {
					return LoadResult{Specialists: []Manifest{{
						Metadata:      Metadata{Name: "worker", Description: "Does focused work", Tools: []string{"read-only"}},
						SystemPrompt:  "Work carefully.",
						ResolvedTools: []string{"grep", "read_file"},
					}}}, nil
				},
				RunChild: func(ctx context.Context, binaryPath string, args []string, progress func(streamjson.Event)) (ChildRunResult, error) {
					gotArgs = append([]string(nil), args...)
					return ChildRunResult{Events: []streamjson.Event{
						{Type: streamjson.EventRunStart, SessionID: "child_task"},
						{Type: streamjson.EventFinal, Text: "ok"},
						{Type: streamjson.EventRunEnd, Status: "success", ExitCode: &zero},
					}}, nil
				},
			}
			result := NewTaskTool(executor).RunWithOptions(context.Background(), map[string]any{
				"name":   "worker",
				"prompt": "inspect auth",
			}, tools.RunOptions{PermissionMode: tc.permissionMode})
			if result.Status != tools.StatusOK {
				t.Fatalf("status = %s, output = %s", result.Status, result.Output)
			}
			if !containsSequence(gotArgs, []string{"--auto", tc.wantAuto}) {
				t.Fatalf("permission mode %q: want --auto %s, args = %#v", tc.permissionMode, tc.wantAuto, gotArgs)
			}
		})
	}
}

// TestRunFreshUsesInlineManifestWithoutRegistryLookup guards the fix that lets a
// caller (the swarm launcher) supply an inline manifest so a member whose agent
// type is NOT a registered specialist still executes — instead of failing with
// "specialist ... not found".
func TestRunFreshUsesInlineManifestWithoutRegistryLookup(t *testing.T) {
	zero := 0
	var ran bool
	var gotArgs []string
	executor := Executor{
		BinaryPath:   "/usr/local/bin/zero",
		NewSessionID: func() (string, error) { return "child_task", nil },
		// Registry has NO "subagent" specialist: a name lookup would fail.
		Load: func(LoadOptions) (LoadResult, error) {
			return LoadResult{Specialists: []Manifest{}}, nil
		},
		RunChild: func(ctx context.Context, binaryPath string, args []string, progress func(streamjson.Event)) (ChildRunResult, error) {
			ran = true
			gotArgs = append([]string(nil), args...)
			return ChildRunResult{Events: []streamjson.Event{
				{Type: streamjson.EventRunStart, SessionID: "child_task"},
				{Type: streamjson.EventFinal, Text: "done"},
				{Type: streamjson.EventRunEnd, Status: "success", ExitCode: &zero},
			}}, nil
		},
	}
	inline := Manifest{
		Metadata:     Metadata{Name: "subagent", Description: "Swarm subagent member.", Tools: []string{"read-only", "edit"}},
		SystemPrompt: "You are a subagent. Complete the task.",
	}
	res, err := executor.Run(context.Background(), TaskParameters{
		Name:     "subagent",
		Prompt:   "do the thing",
		Manifest: &inline,
	}, TaskRunOptions{})
	if err != nil {
		t.Fatalf("inline-manifest run failed: %v", err)
	}
	if !ran {
		t.Fatal("RunChild was not invoked — the member never executed")
	}
	if res.Result.Status != tools.StatusOK {
		t.Fatalf("status = %s output = %s", res.Result.Status, res.Result.Output)
	}
	// The inline manifest drove the run: its name titles the session and its
	// tool groups resolve into the child's enabled-tools allowlist.
	if !containsSequence(gotArgs, []string{"--session-title", "subagent"}) {
		t.Fatalf("inline manifest name not used: %#v", gotArgs)
	}
	if !containsSequence(gotArgs, []string{"--enabled-tools"}) {
		t.Fatalf("inline manifest tools not resolved: %#v", gotArgs)
	}
}
