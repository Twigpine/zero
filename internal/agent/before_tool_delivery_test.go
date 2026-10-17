package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/execution"
	"github.com/Twigpine/zero/internal/hooks"
	"github.com/Twigpine/zero/internal/tools"
	zeroruntime "github.com/Twigpine/zero/internal/zeroruntime"
)

const beforeToolNotice = "denyRead is configured, so the write jail is not confining writes"

// beforeToolChatter is what a hook prints for its own reasons. It must never
// reach the model: main is silent for a successful hook, and a hook that logs is
// not asking to be heard by anything but the operator's terminal.
const beforeToolChatter = "hook-ran-and-logged-this"

// noticeHookPreparer plans the hook command with an enforcement notice attached,
// the way the sandbox does for a command it weakened. The prepared child prints
// ordinary output as well, so one run carries both kinds of text and the
// delivery decision has to tell them apart.
type noticeHookPreparer struct{}

func (noticeHookPreparer) PrepareExecution(_ context.Context, _ execution.Request) (execution.PreparedCommand, error) {
	var command *exec.Cmd
	if runtime.GOOS == "windows" {
		command = exec.Command("cmd.exe", "/c", "echo "+beforeToolChatter)
	} else {
		command = exec.Command("/bin/sh", "-c", "echo "+beforeToolChatter)
	}
	return execution.PreparedCommand{
		Command:     command,
		Enforcement: execution.Enforcement{Notices: []string{beforeToolNotice}},
	}, nil
}

func beforeToolDispatcher(t *testing.T, event hooks.Event, exitCode int) *hooks.Dispatcher {
	t.Helper()
	audit, err := hooks.NewAuditStore(hooks.AuditStoreOptions{AuditPath: filepath.Join(t.TempDir(), "audit.jsonl")})
	if err != nil {
		t.Fatalf("NewAuditStore: %v", err)
	}
	return hooks.NewDispatcher(hooks.DispatcherOptions{
		Config: hooks.Config{
			Enabled: true,
			Hooks: []hooks.Definition{
				{ID: "zero.before-tool", Event: event, Matcher: "read_file", Command: "hook", Enabled: true},
			},
		},
		Audit:     audit,
		Cwd:       t.TempDir(),
		Execution: execution.NewRunner(noticeHookPreparer{}),
	})
}

func readFileRunOptions(t *testing.T, dispatcher *hooks.Dispatcher) (Options, *mockProvider, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write notes.txt: %v", err)
	}
	registry := tools.NewRegistry()
	registry.Register(tools.NewReadFileTool(root))
	provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{
		{
			{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "read_file"},
			{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"path":"notes.txt"}`},
			{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
			{Type: zeroruntime.StreamEventDone},
		},
		{
			{Type: zeroruntime.StreamEventText, Content: "read it"},
			{Type: zeroruntime.StreamEventDone},
		},
	}}
	return Options{
		SessionID:    "session-hook",
		Cwd:          root,
		Registry:     registry,
		ProviderName: "test-provider",
		Model:        "test-model",
		Hooks:        dispatcher,
		MaxTurns:     2,
	}, provider, root
}

// countRequestsContaining reports how many provider requests carry needle, so a
// notice delivered twice is distinguishable from one delivered once.
func countRequestsContaining(requests []zeroruntime.CompletionRequest, needle string) int {
	total := 0
	for _, request := range requests {
		for _, message := range request.Messages {
			if strings.Contains(message.Content, needle) {
				total++
			}
		}
	}
	return total
}

// THE NOTICE CROSSES TO THE MODEL. THE HOOK'S OWN OUTPUT DOES NOT.
//
// executeToolCall used to read the beforeTool outcome only when Blocked was
// true, so a hook that ran under the weakened DenyRead token said so to nobody.
// Delivering DispatchOutcome.Messages fixed that and overshot: hookMessage folds
// the notice together with the hook's ordinary stdout, so every successful
// hook's routine logging became a standing input channel into the next model
// request, which is not what main does.
//
// One hook run produces both kinds of text here, because the bug is exactly a
// failure to tell them apart. Asserted on what the PROVIDER received, since that
// is the boundary that matters; a unit test on the joining helper cannot see
// which slice the loop passes it.
func TestSuccessfulBeforeToolHookDeliversItsNoticeAndNotItsOutput(t *testing.T) {
	options, provider, _ := readFileRunOptions(t, beforeToolDispatcher(t, hooks.EventBeforeTool, 0))
	if _, err := Run(context.Background(), "read the notes", provider, options); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The tool ran, so this is the successful-hook path rather than a blocked
	// call that never reached the tool.
	if !someRequestContains(provider.requests, "hello") {
		t.Fatal("SETUP INVALID: the tool result never reached the model, so nothing was delivered to check")
	}
	// And the hook really did run and really did print, or the silence asserted
	// below would be the silence of a hook that never executed.
	if !someRequestContains(provider.requests, beforeToolNotice) {
		t.Fatal("the enforcement notice never reached the model, so a hook could run under the weakened token and say so to nobody")
	}
	if got := countRequestsContaining(provider.requests, beforeToolNotice); got != 1 {
		t.Errorf("the notice reached the model %d times, want exactly once", got)
	}
	if someRequestContains(provider.requests, beforeToolChatter) {
		t.Error("the hook's ordinary output reached the model; main is silent for a successful hook and routine logging must not become model input")
	}
}

// A VETO MUST NOT SWALLOW A NOTICE FROM A HOOK THAT ALREADY RAN.
//
// Dispatch runs hooks in order and returns at the first veto. The successful
// hook ahead of it may already have run under the weakened token, and that is a
// fact about something that happened. The veto result used to be built from the
// blocking hook's Reason alone, so the earlier disclosure existed only in the
// audit record.
func TestABlockedCallStillCarriesTheEarlierHooksNotice(t *testing.T) {
	audit, err := hooks.NewAuditStore(hooks.AuditStoreOptions{AuditPath: filepath.Join(t.TempDir(), "audit.jsonl")})
	if err != nil {
		t.Fatalf("NewAuditStore: %v", err)
	}
	dispatcher := hooks.NewDispatcher(hooks.DispatcherOptions{
		Config: hooks.Config{
			Enabled: true,
			Hooks: []hooks.Definition{
				{ID: "zero.first", Event: hooks.EventBeforeTool, Matcher: "read_file", Command: "hook", Enabled: true},
				{ID: "zero.veto", Event: hooks.EventBeforeTool, Matcher: "read_file", Command: "veto", Enabled: true},
			},
		},
		Audit:     audit,
		Cwd:       t.TempDir(),
		Execution: execution.NewRunner(vetoSecondPreparer{}),
	})
	options, provider, _ := readFileRunOptions(t, dispatcher)
	if _, err := Run(context.Background(), "read the notes", provider, options); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// SETUP: the second hook really did veto, or this is the ordinary path.
	if !someRequestContains(provider.requests, "was blocked by hook") {
		t.Fatal("SETUP INVALID: the call was not blocked, so the veto path is not under test")
	}
	if !someRequestContains(provider.requests, beforeToolNotice) {
		t.Error("the veto result dropped the notice from the hook that had already run under the weakened token")
	}
	if got := countRequestsContaining(provider.requests, beforeToolNotice); got != 1 {
		t.Errorf("the notice reached the model %d times, want exactly once", got)
	}
	if someRequestContains(provider.requests, beforeToolChatter) {
		t.Error("the vetoed result carried the earlier hook's ordinary output")
	}
}

// vetoSecondPreparer runs the first hook successfully with a notice and makes
// the second one exit non-zero, which is a veto for a blocking event.
type vetoSecondPreparer struct{}

func (vetoSecondPreparer) PrepareExecution(_ context.Context, request execution.Request) (execution.PreparedCommand, error) {
	script := "echo " + beforeToolChatter
	notices := []string{beforeToolNotice}
	if request.Command.Name == "veto" {
		script = "exit 2"
		notices = nil
	}
	var command *exec.Cmd
	if runtime.GOOS == "windows" {
		command = exec.Command("cmd.exe", "/c", script)
	} else {
		command = exec.Command("/bin/sh", "-c", script)
	}
	return execution.PreparedCommand{
		Command:     command,
		Enforcement: execution.Enforcement{Notices: notices},
	}, nil
}
