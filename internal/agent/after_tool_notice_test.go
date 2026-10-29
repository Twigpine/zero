package agent

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/execution"
	"github.com/Twigpine/zero/internal/hooks"
	"github.com/Twigpine/zero/internal/tools"
)

// The tool's own disclosure and the afterTool hook's are the SAME STRING, which
// is the real case: both come from the fixed Windows deny_read warning, so a hook
// running under the same token shape as the tool it follows reports exactly what
// the tool reported.
const sharedEnforcementNotice = "least-privilege notice"

const afterToolChatter = "vet-found-nothing"

// sharedNoticeHookPreparer runs a hook that prints ordinary output and carries
// the same enforcement notice the tool carries.
type sharedNoticeHookPreparer struct{}

func (sharedNoticeHookPreparer) PrepareExecution(_ context.Context, _ execution.Request) (execution.PreparedCommand, error) {
	var command *exec.Cmd
	if runtime.GOOS == "windows" {
		command = exec.Command("cmd.exe", "/c", "echo "+afterToolChatter)
	} else {
		command = exec.Command("/bin/sh", "-c", "echo "+afterToolChatter)
	}
	return execution.PreparedCommand{
		Command:     command,
		Enforcement: execution.Enforcement{Notices: []string{sharedEnforcementNotice}},
	}, nil
}

func afterToolNoticeDispatcher(t *testing.T) *hooks.Dispatcher {
	t.Helper()
	audit, err := hooks.NewAuditStore(hooks.AuditStoreOptions{AuditPath: filepath.Join(t.TempDir(), "audit.jsonl")})
	if err != nil {
		t.Fatalf("NewAuditStore: %v", err)
	}
	return hooks.NewDispatcher(hooks.DispatcherOptions{
		Config: hooks.Config{
			Enabled: true,
			Hooks: []hooks.Definition{
				{ID: "zero.after-tool", Event: hooks.EventAfterTool, Matcher: "notice_projection", Command: "vet", Enabled: true},
			},
		},
		Audit:     audit,
		Cwd:       t.TempDir(),
		Execution: execution.NewRunner(sharedNoticeHookPreparer{}),
	})
}

// ONE DISCLOSURE, ONE DELIVERY, FROM EITHER HOOK PHASE.
//
// beforeTool was moved onto the typed EnforcementNotices slice and afterTool was
// left folding its notices into the prose feedback. Both halves then wrote the
// same fact: the typed slice, which every surface composes through ModelOutput
// and HumanDisplay, and the "Hook output:" block appended to the body. Since the
// two carry the identical fixed string, the model saw the disclosure twice and a
// bash or exec card showed it in the amber furniture and again in the body.
//
// Half a symmetry is its own defect, and this is the composition that catches it:
// a tool that reports a notice, followed by an afterTool hook that reports the
// same one.
func TestAnAfterToolNoticeIsDeliveredOnce(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(noticeProjectionTool{})

	result, err := executeToolCall(context.Background(), registry, ToolCall{
		ID: "call-1", Name: "notice_projection", Arguments: `{}`,
	}, PermissionModeAuto, Options{Cwd: t.TempDir(), Hooks: afterToolNoticeDispatcher(t)})
	if err != nil {
		t.Fatalf("executeToolCall: %v", err)
	}

	// SETUP: the hook really ran, or the silence below is the silence of a hook
	// that never executed.
	if !strings.Contains(result.ModelOutput(), afterToolChatter) {
		t.Fatalf("SETUP INVALID: the afterTool hook's own output never arrived, so nothing here is under test:\n%s", result.ModelOutput())
	}

	if got := strings.Count(result.ModelOutput(), sharedEnforcementNotice); got != 1 {
		t.Errorf("the disclosure reached the model %d times, want once:\n%s", got, result.ModelOutput())
	}
	if got := strings.Count(strings.Join(result.EnforcementNotices, "\n"), sharedEnforcementNotice); got != 1 {
		t.Errorf("the typed slice carries the disclosure %d times, want once: %v", got, result.EnforcementNotices)
	}
	// The body must not carry it at all: the surfaces draw it from the slice, so a
	// copy in the body is what renders it twice.
	if strings.Contains(result.BaseModelOutput(), sharedEnforcementNotice) {
		t.Errorf("the disclosure is in the result body as well as the typed slice:\n%s", result.BaseModelOutput())
	}
	if strings.Count(result.HumanDisplay().Summary, sharedEnforcementNotice) != 1 {
		t.Errorf("the card summary shows the disclosure %d times, want once:\n%s",
			strings.Count(result.HumanDisplay().Summary, sharedEnforcementNotice), result.HumanDisplay().Summary)
	}
}

// And an afterTool hook that discloses something the tool did not still gets
// through, on the typed channel rather than as prose.
func TestAnAfterToolNoticeReachesTheTypedSlice(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(quietTool{})

	result, err := executeToolCall(context.Background(), registry, ToolCall{
		ID: "call-1", Name: "quiet_tool", Arguments: `{}`,
	}, PermissionModeAuto, Options{Cwd: t.TempDir(), Hooks: quietToolAfterHookDispatcher(t)})
	if err != nil {
		t.Fatalf("executeToolCall: %v", err)
	}
	if !strings.Contains(result.ModelOutput(), afterToolChatter) {
		t.Fatalf("SETUP INVALID: the afterTool hook never ran:\n%s", result.ModelOutput())
	}
	if len(result.EnforcementNotices) != 1 || result.EnforcementNotices[0] != sharedEnforcementNotice {
		t.Errorf("an afterTool hook's disclosure did not reach the typed slice, so no card renders it: %v", result.EnforcementNotices)
	}
	if strings.Contains(result.BaseModelOutput(), sharedEnforcementNotice) {
		t.Errorf("the disclosure travelled as prose in the body instead:\n%s", result.BaseModelOutput())
	}
}

// quietTool carries no enforcement notice of its own.
type quietTool struct{}

func (quietTool) Name() string             { return "quiet_tool" }
func (quietTool) Description() string      { return "test tool with no enforcement notice" }
func (quietTool) Parameters() tools.Schema { return tools.Schema{Type: "object"} }
func (quietTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectRead, Permission: tools.PermissionAllow}
}

func (quietTool) Run(ctx context.Context, args map[string]any) tools.Result {
	return tools.Result{Status: tools.StatusOK, Output: "the command output"}
}

func quietToolAfterHookDispatcher(t *testing.T) *hooks.Dispatcher {
	t.Helper()
	audit, err := hooks.NewAuditStore(hooks.AuditStoreOptions{AuditPath: filepath.Join(t.TempDir(), "audit.jsonl")})
	if err != nil {
		t.Fatalf("NewAuditStore: %v", err)
	}
	return hooks.NewDispatcher(hooks.DispatcherOptions{
		Config: hooks.Config{
			Enabled: true,
			Hooks: []hooks.Definition{
				{ID: "zero.after-tool", Event: hooks.EventAfterTool, Matcher: "quiet_tool", Command: "vet", Enabled: true},
			},
		},
		Audit:     audit,
		Cwd:       t.TempDir(),
		Execution: execution.NewRunner(sharedNoticeHookPreparer{}),
	})
}
