package swarm

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Gitlawb/zero/internal/testutil"
	"github.com/Gitlawb/zero/internal/tools"
)

func TestSpawnAndHandoffAgentTypeEnumReflectsRoster(t *testing.T) {
	sw := newSwarmFor(t, newLauncher(okFor))
	want := sw.Registry().AgentTypes() // sorted built-ins: subagent, teammate

	spawnEnum := (&spawnTool{sw: sw}).Parameters().Properties["agent_type"].Enum
	if !slices.Equal(spawnEnum, want) {
		t.Fatalf("spawn agent_type enum = %v, want roster %v", spawnEnum, want)
	}
	if !slices.Contains(spawnEnum, "subagent") || !slices.Contains(spawnEnum, "teammate") {
		t.Fatalf("enum missing built-in roster types: %v", spawnEnum)
	}
	handoffEnum := (&handoffTool{sw: sw}).Parameters().Properties["to_agent_type"].Enum
	if !slices.Equal(handoffEnum, want) {
		t.Fatalf("handoff to_agent_type enum = %v, want roster %v", handoffEnum, want)
	}

	// A user-registered custom type must extend the enum (not a hardcoded list).
	if err := sw.Registry().Register(Definition{AgentType: "researcher"}); err != nil {
		t.Fatalf("register custom agent: %v", err)
	}
	spawnEnum = (&spawnTool{sw: sw}).Parameters().Properties["agent_type"].Enum
	if !slices.Contains(spawnEnum, "researcher") {
		t.Fatalf("custom agent type not reflected in spawn enum: %v", spawnEnum)
	}
	handoffEnum = (&handoffTool{sw: sw}).Parameters().Properties["to_agent_type"].Enum
	if !slices.Contains(handoffEnum, "researcher") {
		t.Fatalf("custom agent type not reflected in handoff enum: %v", handoffEnum)
	}
}

func TestAgentTypePropertyFallsBackWithoutRoster(t *testing.T) {
	prop := agentTypeProperty(nil, "desc")
	if prop.Type != "string" || prop.Description != "desc" {
		t.Fatalf("unexpected property: %+v", prop)
	}
	if prop.Enum != nil {
		t.Fatalf("nil swarm must not advertise an (empty) enum, got %v", prop.Enum)
	}
}

func TestCollapseRuneSafeTruncation(t *testing.T) {
	// 300 multi-byte runes: truncation must not split a rune (no invalid UTF-8).
	got := collapse(strings.Repeat("🚀", 300))
	if !utf8.ValidString(got) {
		t.Fatalf("collapse produced invalid UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n != 201 { // 200 runes + ellipsis
		t.Fatalf("collapse rune count = %d, want 201", n)
	}
}

func newToolSwarm(t *testing.T, l MemberLauncher) (*tools.Registry, *Swarm) {
	t.Helper()
	sw := newSwarmFor(t, l)
	reg := tools.NewRegistry()
	RegisterTools(reg, sw)
	return reg, sw
}

func TestRegisterToolsRegistersAll(t *testing.T) {
	reg, _ := newToolSwarm(t, newLauncher(okFor))
	for _, name := range []string{SpawnToolName, SendToolName, InboxToolName, StatusToolName, HandoffToolName, CollectToolName, ScheduleToolName} {
		if _, ok := reg.Get(name); !ok {
			t.Fatalf("tool %q not registered", name)
		}
	}
}

func TestSpawnToolThroughRegistry(t *testing.T) {
	l := newLauncher(okFor)
	reg, sw := newToolSwarm(t, l)
	res := reg.RunWithOptions(context.Background(), SpawnToolName, map[string]any{
		"agent_type": "teammate",
		"task":       "do the thing",
		"team":       "alpha",
	}, tools.RunOptions{PermissionGranted: true, Model: "m1", Cwd: "/work"})
	if res.Status != tools.StatusOK {
		t.Fatalf("spawn result status = %v, output=%q", res.Status, res.Output)
	}
	id := res.Meta["task_id"]
	if id == "" {
		t.Fatal("spawn must return a task_id in Meta")
	}
	testutil.WaitFor(t, "spec recorded", func() bool { return len(l.recorded()) == 1 })
	spec := l.recorded()[0]
	if spec.Model != "m1" || spec.Cwd != "/work" {
		t.Fatalf("policy/cwd not threaded into member: model=%q cwd=%q", spec.Model, spec.Cwd)
	}
	if _, ok := sw.Coordinator().Get(id); !ok {
		t.Fatalf("task %q not tracked by coordinator", id)
	}
}

func TestSpawnToolRequiresPermission(t *testing.T) {
	reg, _ := newToolSwarm(t, newLauncher(okFor))
	// swarm_spawn is a prompt tool: without a grant the registry refuses it.
	res := reg.RunWithOptions(context.Background(), SpawnToolName, map[string]any{
		"agent_type": "teammate", "task": "x",
	}, tools.RunOptions{})
	if res.Status != tools.StatusError || !strings.Contains(res.Output, "Permission required") {
		t.Fatalf("ungranted spawn should be refused, got status=%v output=%q", res.Status, res.Output)
	}
}

func TestSpawnToolValidation(t *testing.T) {
	_, sw := newToolSwarm(t, newLauncher(okFor))
	tool := &spawnTool{sw: sw}
	if res := tool.Run(context.Background(), map[string]any{"task": "x"}); res.Status != tools.StatusError {
		t.Fatal("missing agent_type must error")
	}
	if res := tool.Run(context.Background(), map[string]any{"agent_type": "teammate"}); res.Status != tools.StatusError {
		t.Fatal("missing task must error")
	}
}

func TestSendAndInboxTools(t *testing.T) {
	reg, _ := newToolSwarm(t, newLauncher(okFor))
	send := reg.RunWithOptions(context.Background(), SendToolName, map[string]any{
		"to": "bob", "body": "ping", "team": "alpha", "subject": "hi",
	}, tools.RunOptions{})
	if send.Status != tools.StatusOK {
		t.Fatalf("send status = %v output=%q", send.Status, send.Output)
	}
	inbox := reg.RunWithOptions(context.Background(), InboxToolName, map[string]any{
		"agent": "bob", "team": "alpha",
	}, tools.RunOptions{})
	if inbox.Status != tools.StatusOK {
		t.Fatalf("inbox status = %v", inbox.Status)
	}
	if !strings.Contains(inbox.Output, "ping") || !strings.Contains(inbox.Output, "[hi]") {
		t.Fatalf("inbox output missing message: %q", inbox.Output)
	}
	if !strings.Contains(inbox.Output, "unread") {
		t.Fatalf("first inbox read should show unread: %q", inbox.Output)
	}
}

func TestSendToolValidation(t *testing.T) {
	_, sw := newToolSwarm(t, newLauncher(okFor))
	tool := &sendTool{sw: sw}
	if res := tool.Run(context.Background(), map[string]any{"body": "x"}); res.Status != tools.StatusError {
		t.Fatal("missing to must error")
	}
	if res := tool.Run(context.Background(), map[string]any{"to": "bob"}); res.Status != tools.StatusError {
		t.Fatal("missing body must error")
	}
}

func TestStatusAndCollectTools(t *testing.T) {
	reg, sw := newToolSwarm(t, newLauncher(okFor))
	spawn := reg.RunWithOptions(context.Background(), SpawnToolName, map[string]any{
		"agent_type": "teammate", "task": "compute", "team": "alpha",
	}, tools.RunOptions{PermissionGranted: true, Model: "m"})
	id := spawn.Meta["task_id"]
	testutil.WaitFor(t, "task done", func() bool {
		task, ok := sw.Coordinator().Get(id)
		return ok && task.Status == StatusDone
	})
	status := reg.RunWithOptions(context.Background(), StatusToolName, map[string]any{"team": "alpha"}, tools.RunOptions{})
	if status.Status != tools.StatusOK || !strings.Contains(status.Output, id) {
		t.Fatalf("status output missing task: %q", status.Output)
	}
	collect := reg.RunWithOptions(context.Background(), CollectToolName, map[string]any{"team": "alpha"}, tools.RunOptions{})
	if collect.Status != tools.StatusOK || !strings.Contains(collect.Output, "ok:compute") {
		t.Fatalf("collect output missing result: %q", collect.Output)
	}
}

func TestCollectWaitTimeoutClampsAndOverflows(t *testing.T) {
	// A huge timeout_seconds must clamp to the cap, not overflow the int64-ns
	// Duration into a negative value (which would make collect return at once).
	if got := collectWaitTimeout(map[string]any{"timeout_seconds": 1e20}); got != maxCollectWaitTimeout {
		t.Fatalf("huge timeout = %v, want cap %v", got, maxCollectWaitTimeout)
	}
	if got := collectWaitTimeout(map[string]any{"timeout_seconds": float64(30)}); got != 30*time.Second {
		t.Fatalf("30s timeout = %v, want 30s", got)
	}
	if got := collectWaitTimeout(map[string]any{}); got != defaultCollectWaitTimeout {
		t.Fatalf("missing timeout = %v, want default %v", got, defaultCollectWaitTimeout)
	}
}

// swarm_collect must BLOCK until the team's members finish, then return their
// results in one call — this is what frees the orchestrator from polling.
func TestCollectBlocksUntilMembersFinish(t *testing.T) {
	gate := make(chan struct{})
	gateClosed := false
	// Release the gated member even if an assertion fails before the explicit
	// close below, so a failing run can't leak the blocked member's goroutine.
	defer func() {
		if !gateClosed {
			close(gate)
		}
	}()
	l := newLauncher(okFor)
	l.gate = gate // members block until the gate is closed
	reg, sw := newToolSwarm(t, l)

	spawn := reg.RunWithOptions(context.Background(), SpawnToolName, map[string]any{
		"agent_type": "teammate", "task": "compute", "team": "alpha",
	}, tools.RunOptions{PermissionGranted: true, Model: "m"})
	id := spawn.Meta["task_id"]
	testutil.WaitFor(t, "member running", func() bool {
		task, ok := sw.Coordinator().Get(id)
		return ok && task.Status == StatusRunning
	})

	done := make(chan tools.Result, 1)
	go func() {
		done <- reg.RunWithOptions(context.Background(), CollectToolName, map[string]any{"team": "alpha"}, tools.RunOptions{})
	}()

	// collect must not return while the member is still running.
	select {
	case res := <-done:
		t.Fatalf("collect returned before the member finished: %q", res.Output)
	case <-time.After(60 * time.Millisecond):
	}

	close(gate) // let the member finish
	gateClosed = true
	select {
	case res := <-done:
		if res.Status != tools.StatusOK || !strings.Contains(res.Output, "ok:compute") {
			t.Fatalf("collect after completion missing result: %q", res.Output)
		}
		if !strings.Contains(res.Output, "[done]") {
			t.Fatalf("collect should report the task done: %q", res.Output)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("collect did not return after the member finished")
	}
}

// A stuck member must not hang collect forever: with a small timeout it returns
// the latest partial state (the still-running task) instead of blocking.
func TestCollectReturnsPartialOnTimeout(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate) // release the member at cleanup so the swarm closes cleanly
	l := newLauncher(okFor)
	l.gate = gate
	reg, sw := newToolSwarm(t, l)

	spawn := reg.RunWithOptions(context.Background(), SpawnToolName, map[string]any{
		"agent_type": "teammate", "task": "compute", "team": "alpha",
	}, tools.RunOptions{PermissionGranted: true, Model: "m"})
	id := spawn.Meta["task_id"]
	testutil.WaitFor(t, "member running", func() bool {
		task, ok := sw.Coordinator().Get(id)
		return ok && task.Status == StatusRunning
	})

	res := reg.RunWithOptions(context.Background(), CollectToolName, map[string]any{
		"team": "alpha", "timeout_seconds": 0.03,
	}, tools.RunOptions{})
	if res.Status != tools.StatusOK {
		t.Fatalf("collect status = %v output=%q", res.Status, res.Output)
	}
	if !strings.Contains(res.Output, "[running]") {
		t.Fatalf("a stuck member should come back as running on timeout, got %q", res.Output)
	}
}

// swarm_collect surfaces each completed member's durable session id in Meta so
// the TUI can drill into the member's conversation.
func TestCollectMetaCarriesMemberSessionIDs(t *testing.T) {
	reg, _ := newToolSwarm(t, newLauncher(okFor))
	spawn := reg.RunWithOptions(context.Background(), SpawnToolName, map[string]any{
		"agent_type": "teammate", "task": "compute", "team": "alpha",
	}, tools.RunOptions{PermissionGranted: true, Model: "m"})
	id := spawn.Meta["task_id"]

	// collect blocks until the member finishes, so its session id is recorded.
	collect := reg.RunWithOptions(context.Background(), CollectToolName, map[string]any{"team": "alpha"}, tools.RunOptions{})
	if collect.Meta == nil || collect.Meta[id] == "" {
		t.Fatalf("collect Meta should carry the member session id for %s, got %#v", id, collect.Meta)
	}
	if want := "sess-" + id; collect.Meta[id] != want {
		t.Fatalf("collect Meta[%s] = %q, want %q", id, collect.Meta[id], want)
	}
}

func TestSpawnToolUnknownTypeListsAvailable(t *testing.T) {
	_, sw := newToolSwarm(t, newLauncher(okFor))
	tool := &spawnTool{sw: sw}
	res := tool.RunWithOptions(context.Background(), map[string]any{
		"agent_type": "ghost", "task": "x",
	}, tools.RunOptions{})
	if res.Status != tools.StatusError || !strings.Contains(res.Output, "available agent types") {
		t.Fatalf("unknown type should list available agents, got %q", res.Output)
	}
	if !strings.Contains(res.Output, "teammate") {
		t.Fatalf("available agents should include builtins, got %q", res.Output)
	}
}

func TestNewWithUserDefinitions(t *testing.T) {
	used := false
	sw, err := New(Options{
		BaseDir:  t.TempDir(),
		Launcher: newLauncher(okFor),
		Definitions: []Definition{{
			AgentType:    "researcher",
			SystemPrompt: func(PromptContext) string { used = true; return "research" },
		}},
	})
	if err != nil {
		t.Fatalf("New with definitions: %v", err)
	}
	t.Cleanup(sw.Close)
	if _, err := sw.Spawn(Policy{Model: "m"}, "team", "researcher", "find things", ""); err != nil {
		t.Fatalf("Spawn user-defined agent: %v", err)
	}
	testutil.WaitFor(t, "researcher used", func() bool { return used })

	// A bad definition makes New fail closed.
	if _, err := New(Options{BaseDir: t.TempDir(), Launcher: newLauncher(okFor), Definitions: []Definition{{AgentType: "  "}}}); err == nil {
		t.Fatal("New with a blank-agentType definition must error")
	}
}

func TestHandoffToolThroughRegistry(t *testing.T) {
	gate := make(chan struct{})
	l := newLauncher(okFor)
	l.gate = gate
	reg, sw := newToolSwarm(t, l)
	spawn := reg.RunWithOptions(context.Background(), SpawnToolName, map[string]any{
		"agent_type": "teammate", "task": "long", "team": "alpha",
	}, tools.RunOptions{PermissionGranted: true, Model: "m"})
	origID := spawn.Meta["task_id"]
	testutil.WaitFor(t, "running", func() bool {
		task, ok := sw.Coordinator().Get(origID)
		return ok && task.Status == StatusRunning
	})
	res := reg.RunWithOptions(context.Background(), HandoffToolName, map[string]any{
		"task_id": origID, "to_agent_type": "subagent", "note": "carry on", "team": "alpha",
	}, tools.RunOptions{PermissionGranted: true, Model: "m"})
	if res.Status != tools.StatusOK {
		t.Fatalf("handoff status = %v output=%q", res.Status, res.Output)
	}
	if res.Meta["previous_task_id"] != origID || res.Meta["task_id"] == "" {
		t.Fatalf("handoff meta wrong: %+v", res.Meta)
	}
	close(gate)
}
