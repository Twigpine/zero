package agent

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// legacyDenialSignaturePrefix is the string provenance used to be encoded in.
// The identity was one namespace holding both a normalized error signature and
// a synthetic "denial:<category>" key, and provenance was recovered afterwards
// with strings.HasPrefix. It survives here, and only here, so the regression
// names the exact spelling that used to collide.
const legacyDenialSignaturePrefix = "denial:"

func runUntilGuardHalt(t *testing.T, gate bool) Result {
	t.Helper()
	tool := &uncategorizedSandboxTool{}
	registry := tools.NewRegistry()
	registry.Register(tool)
	turns := make([][]zeroruntime.StreamEvent, 0, toolFailureAnyErrorStopAt+4)
	for i := range toolFailureAnyErrorStopAt + 4 {
		turns = append(turns, toolTurn("c"+strconv.Itoa(i), "write_file", `{"path":"/o-`+strconv.Itoa(i)+`.txt"}`))
	}
	result, err := Run(context.Background(), "write", &mockProvider{turns: turns}, Options{
		Registry:                registry,
		PermissionMode:          PermissionModeAsk,
		MaxTurns:                len(turns) + 5,
		RequireCompletionSignal: gate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result.FinalAnswer, "Agent stopped:") {
		t.Fatalf("the guard did not halt this run: %q", result.FinalAnswer)
	}
	return result
}

// A GUARD HALT IS UNFINISHED WORK, AND HEADLESS HAS TO HEAR ABOUT IT.
//
// The halt returns straight out of the tool loop, so it never reached the
// completion gate the max-turns paths go through. zero exec treats only
// Result.Incomplete as exit 4 and otherwise reports run_end success with exit
// 0, so a task denied six times, or failed through the twelve-call bound, came
// back as a successful automation result having done none of the work asked.
func TestGuardHaltIsIncompleteUnderTheCompletionGate(t *testing.T) {
	headless := runUntilGuardHalt(t, true)
	if !headless.Incomplete {
		t.Error("a guard halt reports the run as complete; zero exec would call a run that did nothing a success")
	}
	if !strings.Contains(headless.IncompleteReason, "write_file") {
		t.Errorf("the incomplete reason does not name the tool that halted the run: %q", headless.IncompleteReason)
	}
	// And the interactive default is untouched: Incomplete exists for the
	// headless gate, so setting it without one would be inventing a status.
	if interactive := runUntilGuardHalt(t, false); interactive.Incomplete {
		t.Error("Incomplete was set without RequireCompletionSignal, changing interactive behaviour")
	}
}

// executedDenialLookalikeTool runs, fails, and prints exactly the string the
// guard used to store for a real permission refusal.
type executedDenialLookalikeTool struct{ ran int }

func (t *executedDenialLookalikeTool) Name() string             { return "bash" }
func (t *executedDenialLookalikeTool) Description() string      { return "probe" }
func (t *executedDenialLookalikeTool) Parameters() tools.Schema { return tools.Schema{Type: "object"} }
func (t *executedDenialLookalikeTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectShell, Permission: tools.PermissionAllow}
}

func (t *executedDenialLookalikeTool) Run(context.Context, map[string]any) tools.Result {
	t.ran++
	return tools.Result{Status: tools.StatusError, Output: legacyDenialSignaturePrefix + string(DenialPermissionDenied)}
}

// TRUSTED PROVENANCE AND UNTRUSTED CONTENT MUST NOT SHARE A NAMESPACE.
//
// While the identity was a single string, a command that printed exactly
// "denial:permission_denied" and exited non-zero took on the identity of a real
// permission refusal: it merged streaks with one, and the run told the user the
// tool had been refused when it had executed every time.
func TestAnExecutedErrorCannotImpersonateARefusal(t *testing.T) {
	tool := &executedDenialLookalikeTool{}
	registry := tools.NewRegistry()
	registry.Register(tool)
	turns := make([][]zeroruntime.StreamEvent, 0, toolFailureStopAt+4)
	for i := range toolFailureStopAt + 4 {
		turns = append(turns, toolTurn("c"+strconv.Itoa(i), "bash", `{}`))
	}
	result, err := Run(context.Background(), "run it", &mockProvider{turns: turns}, Options{
		Registry: registry, PermissionMode: PermissionModeAsk, MaxTurns: len(turns) + 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if tool.ran == 0 {
		t.Fatal("the tool never ran, so this no longer covers an executed failure")
	}
	if strings.Contains(result.FinalAnswer, "was refused") {
		t.Errorf("the tool executed %d times and nothing refused it, but the run says it was refused: %q", tool.ran, result.FinalAnswer)
	}
	if !strings.Contains(result.FinalAnswer, "with the same error") {
		t.Errorf("an executed failure repeating identically should read as the same error: %q", result.FinalAnswer)
	}
}

// AND THE AGGREGATE IS ITS OWN FACT.
//
// The content-blind bound fires precisely when no single identity repeated
// enough, so the identity present at the end says nothing about the eleven
// before it. Alternating two refusal categories reaches twelve without either
// reaching six; the tool never ran once, and the answer used to describe that
// as the tool failing with varying errors.
func TestTheContentBlindBoundKeepsRefusalProvenance(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		kinds      []DenialCategory
		wantCause  toolFailureCause
		wantAnswer string
	}{
		{
			name:       "alternating refusal categories",
			kinds:      []DenialCategory{DenialPermissionDenied, DenialSandboxBlock},
			wantCause:  toolFailureCauseVariedRefused,
			wantAnswer: "was refused",
		},
		{
			// Mixed is DECIDED, not inherited from whichever field a switch
			// tested first. Neither "failed" alone nor "refused" alone is true of
			// this sequence, so the wording says both.
			name:       "executed failures mixed with refusals",
			kinds:      []DenialCategory{DenialNone, DenialPermissionDenied},
			wantCause:  toolFailureCauseVariedMixed,
			wantAnswer: "failed or was refused",
		},
		{
			name:       "executed failures only",
			kinds:      []DenialCategory{DenialNone, DenialNone},
			wantCause:  toolFailureCauseVariedExecuted,
			wantAnswer: "with varying errors",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			state := newGuardState()
			var outcome toolFailureOutcome
			for index := 0; index < toolFailureAnyErrorStopAt; index++ {
				// Distinct prose every call, exactly as a real refusal or error
				// naming what it touched would produce.
				outcome = state.observeToolResult("bash", true, false,
					"failure on item "+strconv.Itoa(index), testCase.kinds[index%len(testCase.kinds)])
				if outcome.Stop {
					break
				}
			}
			if !outcome.Stop {
				t.Fatal("twelve consecutive failures did not trip the content-blind bound")
			}
			if outcome.Count != toolFailureAnyErrorStopAt {
				t.Errorf("Count = %d, want the counter that tripped (%d)", outcome.Count, toolFailureAnyErrorStopAt)
			}
			if outcome.Cause != testCase.wantCause {
				t.Errorf("Cause = %v, want %v", outcome.Cause, testCase.wantCause)
			}
			answer := toolFailureStopAnswer("bash", outcome.Count, outcome.Cause)
			if !strings.Contains(answer, testCase.wantAnswer) {
				t.Errorf("stop answer = %q, want it to contain %q", answer, testCase.wantAnswer)
			}
		})
	}
}
