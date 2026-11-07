package swarm

import (
	"context"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/specialist"
	"github.com/Twigpine/zero/internal/streamjson"
)

// A SWARM MEMBER'S SESSION ID IS NOT PART OF ITS ANSWER.
//
// BuildFinalResult prefixes "session_id: <id>" to a successful child's output so
// a Task caller can continue that child. A swarm member has no such caller —
// MemberResult.SessionID carries the id structurally — and the string it returns
// is RENDERED: swarm_collect writes `result: <text>` (tools.go) and collapse
// only flattens newlines and truncates to 200 runes, so the id reached the user
// verbatim.
//
// THE TASK PATH ESCAPED THE SAME LEAK BY ACCIDENT OF RENDERING, not by design:
// toolCardSuppressedInTranscript drops "Task" and "update_plan" result rows from
// the transcript entirely. swarm_collect has no such suppression.
//
// THROUGH THE REAL LAUNCHER. The existing swarm tests use okFor, a fake that
// returns MemberResult{Result: "ok:" + spec.Task} and never touches
// BuildFinalResult — so no session_id prefix exists in the fixture and the test
// passes whether the leak is there or not. This drives NewSpecialistLauncher
// against a real executor seam instead.
func specialistLauncherFor(t *testing.T, childSession, answer string) MemberLauncher {
	t.Helper()
	executor := specialist.Executor{
		BinaryPath:   "/bin/true",
		NewSessionID: func() (string, error) { return childSession, nil },
		Load:         func(specialist.LoadOptions) (specialist.LoadResult, error) { return specialist.LoadResult{}, nil },
		RunChild: func(_ context.Context, _ string, _ []string, progress func(streamjson.Event)) (specialist.ChildRunResult, error) {
			events := []streamjson.Event{
				{Type: streamjson.EventRunStart, SessionID: childSession},
				{Type: streamjson.EventFinal, Text: answer},
				{Type: streamjson.EventRunEnd, Status: "success"},
			}
			for _, event := range events {
				if progress != nil {
					progress(event)
				}
			}
			return specialist.ChildRunResult{Started: true, ExitCode: 0, Events: events}, nil
		},
	}
	return NewSpecialistLauncher(executor)
}

func TestASwarmMembersResultCarriesNoSessionIDLine(t *testing.T) {
	const childSession = "specialist_00000000000000000000000a"
	const answer = "The retry watchdog resets on every event."

	launcher := specialistLauncherFor(t, childSession, answer)
	handle, err := launcher.Launch(context.Background(), MemberSpec{
		ID: "m1", Team: "t", AgentType: "subagent", Task: "trace the watchdog", Cwd: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("the member did not start: %v", err)
	}
	result, err := handle.Wait()
	if err != nil {
		t.Fatalf("the member failed: %v", err)
	}
	// The premise: the id really did reach the launcher, so the assertion below
	// is about stripping rather than about an id that was never there.
	if result.SessionID != childSession {
		t.Fatalf("setup: the launcher did not receive the child's id, got %q", result.SessionID)
	}
	if strings.Contains(result.Result, "session_id") {
		t.Fatalf("the raw child session id is in what swarm_collect renders:\n%q", result.Result)
	}
	if !strings.Contains(result.Result, "watchdog resets") {
		t.Fatalf("the answer itself did not survive: %q", result.Result)
	}
}

// AND IT MUST SURVIVE THE RENDER. Stripping at the launcher is only worth
// anything if the string that reaches the user is the stripped one — so this
// follows it through the coordinator and out through swarm_collect's own
// rendering, which is where the leak was actually visible.
func TestTheRenderedCollectOutputCarriesNoSessionID(t *testing.T) {
	const childSession = "specialist_00000000000000000000000a"
	launcher := specialistLauncherFor(t, childSession, "found three call sites")

	handle, err := launcher.Launch(context.Background(), MemberSpec{
		ID: "m1", Team: "t", AgentType: "subagent", Task: "find them", Cwd: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := handle.Wait()
	if err != nil {
		t.Fatal(err)
	}
	// collapse is what swarm_collect applies before printing: it flattens
	// newlines and truncates, and does NOT remove the line — which is precisely
	// why the strip has to happen upstream of it.
	rendered := "      result: " + collapse(result.Result)
	if strings.Contains(rendered, "session_id") {
		t.Fatalf("the id survives into the rendered collect output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "found three call sites") {
		t.Fatalf("the answer did not survive the render: %s", rendered)
	}
}

// THE FIXTURE THAT COULD NOT CATCH THIS, pinned so it cannot quietly come back.
//
// okFor returns a hand-written Result and never runs BuildFinalResult, so a test
// built on it is green whether or not the production launcher strips anything.
// This asserts the gap rather than pretending it is closed.
func TestTheFakeLauncherCannotExerciseTheStrip(t *testing.T) {
	fake, err := okFor(MemberSpec{ID: "m1", Task: "anything"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fake.Result, "session_id") {
		t.Fatal("okFor now produces a session_id line; the tests built on it may be meaningful after all — re-read them")
	}
	// It is a fixture, not the production path: the real launcher is the only
	// thing that can prove the strip, which is what the tests above use.
	if fake.Result != "ok:anything" {
		t.Fatalf("okFor changed shape to %q; the tests that rely on it need re-reading", fake.Result)
	}
}
