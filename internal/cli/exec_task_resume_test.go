package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/specialist"
	"github.com/Twigpine/zero/internal/streamjson"
	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// TestTaskResumeContinuesTheSpecialistChildThroughExec runs the Task tool's own
// arguments through the real exec path, for the fresh run and for the resume.
// The fresh run creates a child session (kind child, tag specialist), and exec
// refused to resume that kind, so every Task resume failed with "Zero session
// is not resumable" while the specialist tests, which launch a fake binary,
// stayed green.
func TestTaskResumeContinuesTheSpecialistChildThroughExec(t *testing.T) {
	isolateCLIUserState(t)
	cwd := t.TempDir()
	store := sessions.NewStore(sessions.StoreOptions{})
	parent, err := store.Create(sessions.CreateInput{SessionID: "parent_task", Cwd: cwd})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	provider := &capturingImageProvider{}
	deps := appDeps{
		stdin: strings.NewReader(""),
		getwd: func() (string, error) { return cwd, nil },
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return execResolvedConfig(), nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) { return provider, nil },
	}
	var childStderr []string
	executor := specialist.Executor{
		BinaryPath:   "zero",
		SessionStore: store,
		NewSessionID: func() (string, error) { return "child_task", nil },
		Load: func(specialist.LoadOptions) (specialist.LoadResult, error) {
			return specialist.LoadResult{Specialists: []specialist.Manifest{{
				Metadata: specialist.Metadata{
					Name:        "skim",
					Description: "Bounded read-only lookups.",
					Tools:       []string{"read_file"},
				},
				SystemPrompt:  "Find the thing and stop.",
				ResolvedTools: []string{"read_file"},
			}}}, nil
		},
		// The child is the real exec entry point, run in process with the
		// arguments the executor built, instead of a fake binary.
		RunChild: func(_ context.Context, _ string, args []string, _ func(streamjson.Event)) (specialist.ChildRunResult, error) {
			var stdout, stderr bytes.Buffer
			code := runWithDeps(args, &stdout, &stderr, deps)
			childStderr = append(childStderr, stderr.String())
			events := []streamjson.Event{}
			for _, line := range strings.Split(stdout.String(), "\n") {
				if line = strings.TrimSpace(line); line == "" {
					continue
				}
				var event streamjson.Event
				if err := json.Unmarshal([]byte(line), &event); err != nil {
					t.Fatalf("child stream-json line %q: %v", line, err)
				}
				events = append(events, event)
			}
			return specialist.ChildRunResult{Events: events, Stderr: stderr.String(), ExitCode: code, Started: true}, nil
		},
	}
	options := specialist.TaskRunOptions{ParentSessionID: parent.SessionID, ToolCallID: "call_1", Cwd: cwd}

	fresh, err := executor.Run(context.Background(), specialist.TaskParameters{Name: "skim", Prompt: "find the config"}, options)
	if err != nil || fresh.Result.Status != tools.StatusOK {
		t.Fatalf("SETUP: fresh Task run = %+v, %v; child stderr %q", fresh.Result, err, childStderr)
	}
	child, err := store.Get("child_task")
	if err != nil || child == nil {
		t.Fatalf("SETUP: child session = %v, %v", child, err)
	}
	// SETUP: the fresh run made exactly the kind of session the resume is about.
	if child.SessionKind != sessions.SessionKindChild || child.Tag != specialist.SessionTagSpecialist || child.ParentSessionID != parent.SessionID {
		t.Fatalf("SETUP: child kind %q tag %q parent %q, want child/specialist/%s", child.SessionKind, child.Tag, child.ParentSessionID, parent.SessionID)
	}

	options.ToolCallID = "call_2"
	resumed, err := executor.Run(context.Background(), specialist.TaskParameters{Name: "skim", Prompt: "now check the tests", Resume: "child_task"}, options)
	if err != nil || resumed.Result.Status != tools.StatusOK {
		t.Fatalf("Task resume = %+v, %v; child stderr %q", resumed.Result, err, childStderr)
	}
	provider.mu.Lock()
	prompt := provider.prompt
	provider.mu.Unlock()
	if !strings.Contains(prompt, "Continuing Zero session child_task.") || !strings.Contains(prompt, "now check the tests") {
		t.Fatalf("resumed child prompt = %q, want the child's session continued with the follow-up", prompt)
	}
	sessionsAfter, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessionsAfter) != 2 {
		t.Fatalf("sessions after resume = %d, want the parent and the one child, not a new session", len(sessionsAfter))
	}

	// The child is still not resumable by hand, or from another session.
	other, err := store.Create(sessions.CreateInput{SessionID: "other_parent", Cwd: cwd})
	if err != nil {
		t.Fatalf("create other parent: %v", err)
	}
	for _, args := range [][]string{
		{"exec", "--resume", "child_task", "keep going"},
		{"exec", "--resume", "child_task", "--tag", "specialist", "--calling-session-id", other.SessionID, "keep going"},
	} {
		var stdout, stderr bytes.Buffer
		code := runWithDeps(args, &stdout, &stderr, deps)
		if code == exitSuccess || !strings.Contains(stderr.String(), "Zero session is not resumable: child_task") {
			t.Errorf("%q exit=%d stderr=%q, want the not-resumable refusal", args, code, stderr.String())
		}
	}
}
