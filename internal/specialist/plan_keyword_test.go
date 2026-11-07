package specialist

import (
	"context"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
)

func keywordTool(t *testing.T, require bool) *OrchestrateTool {
	t.Helper()
	return &OrchestrateTool{
		PostureActive:      func() bool { return true },
		RequirePlanKeyword: require,
		ParentTools:        []string{"read_file"},
		RunTask: func(context.Context, PlanTaskRequest) (TaskResult, error) {
			return TaskResult{Outcome: TaskSucceeded, Output: "done"}, nil
		},
	}
}

func keywordArgs() map[string]any {
	return map[string]any{"tasks": []any{
		map[string]any{"id": "a", "prompt": "look at the tree"},
	}}
}

// A PLAN THE USER DID NOT ASK FOR IS REFUSED, when the session requires asking.
//
// Once the posture is on, orchestrate exists for the rest of the session, and
// everything the model reads shares context with the user's instructions — file
// contents, PR comments, MCP output, a scheduled payload. An imperative sentence
// in any of them reads like an instruction to the tool that spends the most.
func TestAPlanIsRefusedWhenTheUserDidNotAskForOne(t *testing.T) {
	tool := keywordTool(t, true)
	result := tool.RunWithOptions(context.Background(), keywordArgs(), tools.RunOptions{
		UserMessage: "have a look at the auth code and tell me what you think",
	})
	if result.Status != tools.StatusError {
		t.Fatalf("a plan ran without the user asking: %+v", result)
	}
	// The refusal must name the remedy, or it reads as "plans are broken".
	for _, required := range []string{"run a plan for", "your own words"} {
		if !strings.Contains(result.Output, required) {
			t.Errorf("the refusal does not tell the user how to ask: %q", result.Output)
		}
	}
}

// ...and one the user DID ask for runs. A gate that blocks the honest case is a
// gate that gets turned off.
func TestAPlanTheUserAskedForStillRuns(t *testing.T) {
	for _, message := range []string{
		"run a plan for the auth packages",
		"fan out and investigate the sandbox",
		"can you orchestrate this across the three packages",
		"look at these in parallel please",
		"use a workflow for this",
	} {
		t.Run(message, func(t *testing.T) {
			tool := keywordTool(t, true)
			result := tool.RunWithOptions(context.Background(), keywordArgs(), tools.RunOptions{UserMessage: message})
			if result.Status == tools.StatusError && strings.Contains(result.Output, "your own words") {
				t.Errorf("a plan the user asked for was refused: %q", result.Output)
			}
		})
	}
}

// AN EMPTY MESSAGE IS NOT CONSENT. A caller that supplies no user text cannot
// demonstrate a request, and treating absence as permission would leave the gate
// off in exactly the headless and scheduled paths most exposed to untrusted
// payloads.
func TestAnAbsentUserMessageIsNotTakenAsARequest(t *testing.T) {
	tool := keywordTool(t, true)
	result := tool.RunWithOptions(context.Background(), keywordArgs(), tools.RunOptions{UserMessage: ""})
	if result.Status != tools.StatusError {
		t.Fatal("an empty user message was treated as having asked for a plan")
	}
}

// DEFAULT OFF, and unchanged for everyone who does not enable it — the same call
// this codebase made for auto_assign, and for the same reason.
func TestTheGateIsOffUnlessTheSessionAsksForIt(t *testing.T) {
	tool := keywordTool(t, false)
	result := tool.RunWithOptions(context.Background(), keywordArgs(), tools.RunOptions{
		UserMessage: "have a look at the auth code",
	})
	if result.Status == tools.StatusError && strings.Contains(result.Output, "your own words") {
		t.Errorf("the gate fired with RequirePlanKeyword false: %q", result.Output)
	}
}

// The matcher itself, at its edges.
func TestPlanRequestedByUserMatching(t *testing.T) {
	for message, want := range map[string]bool{
		"RUN A PLAN for this":                true, // case-insensitive
		"please fan-out over the packages":   true, // hyphenated
		"audit this repo thoroughly":         false,
		"":                                   false,
		"the TODO says to fan out the tests": true, // a false POSITIVE the gate accepts
		"read the file and summarise it":     false,
	} {
		if got := planRequestedByUser(message); got != want {
			t.Errorf("planRequestedByUser(%q) = %v, want %v", message, got, want)
		}
	}
}

// PLAN TASKS REACH NEITHER MEMORY TOOL, and that is a decision rather than an
// oversight.
//
// A note is read back in every future session and believed. Letting a task READ
// one means a stale note can steer a whole fan-out; letting a task WRITE one
// means twenty tasks race to describe the same finding and the last wins — which
// is precisely why update_plan was taken out of this grant. Granting either is a
// one-line change here, made deliberately.
func TestPlanTasksMayNotReachMemory(t *testing.T) {
	for _, name := range []string{tools.MemoryToolName, tools.MemoryWriteToolName} {
		if planReadOnlyTools[name] {
			t.Errorf("%q is grantable to a plan task as read-only", name)
		}
		if planWriteTools[name] {
			t.Errorf("%q is grantable to a plan task as a write tool", name)
		}
	}
}
