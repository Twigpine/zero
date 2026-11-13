package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/sandbox"
	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/specmode"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

func TestRunExecUseSpecCreatesDraftSession(t *testing.T) {
	workspaceRoot := t.TempDir()
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir(), Now: fixedCLISpecTime})
	provider := &submitSpecExecProvider{}
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{
		"exec",
		"--cwd", workspaceRoot,
		"--use-spec",
		"--spec-model", "draft-model",
		"--spec-reasoning-effort", "high",
		"-o", "json",
		"plan the review flow",
	}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return workspaceRoot, nil
		},
		resolveConfig: func(_ string, overrides config.Overrides) (config.ResolvedConfig, error) {
			model := "default-model"
			if overrides.Provider.Model != "" {
				model = overrides.Provider.Model
			}
			return config.ResolvedConfig{
				ActiveProvider: "test",
				Provider: config.ProviderProfile{
					Name:         "test",
					ProviderKind: config.ProviderKindOpenAICompatible,
					BaseURL:      "http://127.0.0.1/v1",
					Model:        model,
				},
				MaxTurns: 3,
			}, nil
		},
		resolveMCPConfig: func(string, bool) (config.MCPConfig, error) {
			return config.MCPConfig{}, nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return provider, nil
		},
		newSessionStore: func() *sessions.Store {
			return store
		},
		newSandboxStore: func() (*sandbox.GrantStore, error) {
			return sandbox.NewGrantStore(sandbox.StoreOptions{FilePath: t.TempDir() + "/grants.json"})
		},
		now: fixedCLISpecTime,
	})

	if exitCode != exitSuccess {
		t.Fatalf("exit = %d stderr=%s stdout=%s", exitCode, stderr.String(), stdout.String())
	}
	events := decodeJSONLines(t, stdout.String())
	if !slicesContains(jsonEventTypes(events), string(agent.StopReasonSpecReviewRequired)) {
		t.Fatalf("missing spec_review_required event in %v", jsonEventTypes(events))
	}
	items, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("session count = %d, want 1: %#v", len(items), items)
	}
	draft := items[0]
	if draft.SessionKind != sessions.SessionKindSpecDraft || draft.SpecStatus != sessions.SpecStatusDraft {
		t.Fatalf("unexpected draft session: %#v", draft)
	}
	if draft.SpecID == "" || !strings.Contains(filepath.ToSlash(draft.SpecFilePath), ".zero/specs/") {
		t.Fatalf("draft spec metadata missing: %#v", draft)
	}
	if draft.SpecDraftModelID != "draft-model" || draft.ModelID != "draft-model" || draft.SpecDraftReasoning != "high" {
		t.Fatalf("draft model metadata = %#v", draft)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("provider request count = %d, want 1", len(provider.requests))
	}
	if !provider.requestIncludesTool(specmode.SubmitToolName) {
		t.Fatalf("provider tools missing submit_spec: %#v", provider.requests[0].Tools)
	}
}

func TestParseExecUseSpecRejectsResume(t *testing.T) {
	_, _, err := parseExecArgs([]string{"--use-spec", "--resume", "zero_1", "plan"})
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("expected --use-spec/--resume validation, got %v", err)
	}
}

func TestParseExecSpecOverridesRequireUseSpec(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "spec model",
			args: []string{"--spec-model", "gpt-5", "plan"},
			want: "--spec-model requires --use-spec",
		},
		{
			name: "spec reasoning effort",
			args: []string{"--spec-reasoning-effort", "high", "plan"},
			want: "--spec-reasoning-effort requires --use-spec",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := parseExecArgs(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q validation, got %v", tc.want, err)
			}
		})
	}
}

func TestRunExecUseSpecRejectsSpecialistTag(t *testing.T) {
	exitCode, _, stderr := runExecWithEcho(t, []string{"exec", "--use-spec", "--tag", "specialist", "plan"})
	if exitCode != exitUsage {
		t.Fatalf("exit = %d, want %d stderr=%s", exitCode, exitUsage, stderr)
	}
	if !strings.Contains(stderr, "specialist child session") {
		t.Fatalf("expected --use-spec specialist tag validation, got %q", stderr)
	}
}

func TestRunExecUseSpecRejectsFiltersThatHideSubmitSpec(t *testing.T) {
	for _, args := range [][]string{
		{"exec", "--use-spec", "--disabled-tools", specmode.SubmitToolName, "plan"},
		{"exec", "--use-spec", "--enabled-tools", "read_file", "plan"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			exitCode, _, stderr := runExecWithEcho(t, args)
			if exitCode != exitUsage {
				t.Fatalf("exit = %d, want %d stderr=%s", exitCode, exitUsage, stderr)
			}
			if !strings.Contains(stderr, "submit_spec") {
				t.Fatalf("expected submit_spec validation, got %q", stderr)
			}
		})
	}
}

type submitSpecExecProvider struct {
	requests []zeroruntime.CompletionRequest
}

func (provider *submitSpecExecProvider) StreamCompletion(ctx context.Context, request zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	provider.requests = append(provider.requests, request)
	arguments, _ := json.Marshal(map[string]string{
		"title": "Review Flow",
		"plan":  "# Goal\n\nAdd the review flow.",
	})
	ch := make(chan zeroruntime.StreamEvent, 4)
	select {
	case <-ctx.Done():
		close(ch)
		return ch, ctx.Err()
	case ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: specmode.SubmitToolName}:
	}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: string(arguments)}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"}
	ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}
	close(ch)
	return ch, nil
}

func (provider *submitSpecExecProvider) requestIncludesTool(name string) bool {
	if len(provider.requests) == 0 {
		return false
	}
	for _, tool := range provider.requests[0].Tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

func slicesContains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
