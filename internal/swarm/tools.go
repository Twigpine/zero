package swarm

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/tools"
)

// Tool names. All swarm tools are additive and only act when invoked; the
// existing single "Task" tool is unchanged.
const (
	SpawnToolName   = "swarm_spawn"
	SendToolName    = "swarm_send"
	InboxToolName   = "swarm_inbox"
	StatusToolName  = "swarm_status"
	HandoffToolName = "swarm_handoff"
	CollectToolName = "swarm_collect"
)

// RegisterTools registers the seven swarm tools backed by one shared Swarm. The
// caller owns the Swarm's lifetime (call Swarm.Close on shutdown).
func RegisterTools(registry *tools.Registry, sw *Swarm) {
	registry.Register(&spawnTool{sw: sw})
	registry.Register(&sendTool{sw: sw})
	registry.Register(&inboxTool{sw: sw})
	registry.Register(&statusTool{sw: sw})
	registry.Register(&handoffTool{sw: sw})
	registry.Register(&collectTool{sw: sw})
	registry.Register(&scheduleTool{sw: sw})
}

// deferredSwarmTool marks a swarm tool as deferred-eligible. Swarm tools are an
// advanced, rarely-first-move orchestration feature and the base system prompt
// does not name them, so they are hidden behind tool_search (loaded on demand)
// instead of shipping their full schemas in the eager per-request tool prefix.
// Embedding it keys each tool into partitionTools' deferral, exactly like MCP.
//
// The COORDINATION tools (swarm_send/swarm_status/swarm_inbox/swarm_collect)
// override Deferred() to un-defer (expose eagerly) once a swarm is active (see
// hasActiveSwarm): a weaker model coordinating a live swarm that can't see those
// tools tends to misroute the calls to the specialist tool ("specialist
// swarm_send not found") in a retry loop. The ENTRY-POINT tools (swarm_spawn/
// swarm_schedule) and swarm_handoff keep the default true — they stay
// discoverable via tool_search from a cold start.
//
// Un-deferring a coordination tool must NOT lower the global deferral count and
// risk dropping it below DeferThreshold (which would deactivate deferral and
// force-expose every other deferred tool, e.g. MCP). DeferralEligible() keeps
// every swarm tool counting toward the threshold even while exposed eagerly, so
// partitionTools' active-gate is unaffected by the un-defer in ANY config —
// raised threshold or filtered tools included (see tools.IsDeferralEligible).
type deferredSwarmTool struct{}

func (deferredSwarmTool) Deferred() bool { return true }

// DeferralEligible keeps a swarm tool counting toward the DeferThreshold even
// when a coordination tool un-defers (Deferred()==false). This decouples
// "counts toward the threshold" from "currently hidden", so un-deferring the
// coordination tools can never deactivate deferral for other tools.
func (deferredSwarmTool) DeferralEligible() bool { return true }

// hasActiveSwarm reports whether the swarm currently tracks any task — i.e. a
// swarm exists worth coordinating (members running, queued, on standby, or done
// awaiting collection). It is the gate the coordination tools' Deferred() reads,
// so they surface eagerly for the swarm's whole life rather than flapping as
// members move to standby/done. Thread-safe (Summarize RLocks the coordinator)
// and nil-safe: a zero-value or unwired Swarm reports inactive (stays deferred).
func (s *Swarm) hasActiveSwarm() bool {
	if s == nil || s.coord == nil {
		return false
	}
	return s.coord.Summarize().Total > 0
}

// policyFrom derives the member-inheritance policy from the live tool options so
// each spawned member runs on the orchestrator's current model + permission mode.
func policyFrom(options tools.RunOptions) Policy {
	return Policy{Model: options.Model, PermissionMode: options.PermissionMode}
}

func swarmStr(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func okResult(output, kind, summary string) tools.Result {
	return tools.Result{Status: tools.StatusOK, Output: output, Display: tools.Display{Kind: kind, Summary: summary}}
}

func errResult(format string, a ...any) tools.Result {
	return tools.Result{Status: tools.StatusError, Output: "Error: " + fmt.Sprintf(format, a...)}
}

// agentTypeProperty builds the schema for an agent-type parameter, constraining it
// to the roster's currently registered types (the built-ins plus any user-registered
// agents) via an enum so a model can't spawn or hand off to an unknown type — the
// schema teaches the valid set up front instead of the model learning it from a
// failed call. Falls back to a free-form string if the roster is unavailable or
// empty (so the tool never advertises an empty, un-satisfiable enum).
func agentTypeProperty(sw *Swarm, description string) tools.PropertySchema {
	prop := tools.PropertySchema{Type: "string", Description: description}
	if sw == nil {
		return prop
	}
	if registry := sw.Registry(); registry != nil {
		if types := registry.AgentTypes(); len(types) > 0 {
			prop.Enum = types
		}
	}
	return prop
}

// ---- swarm_spawn -----------------------------------------------------------

type spawnTool struct {
	sw *Swarm
	deferredSwarmTool
}

func (t *spawnTool) Name() string { return SpawnToolName }
func (t *spawnTool) Description() string {
	return "Spawn a swarm member of the given agent type to run a task concurrently under a team. Returns a task id immediately and the member runs in the background. After spawning all members, call swarm_collect once to wait for them and read their results — do not poll swarm_status in a loop."
}
func (t *spawnTool) Parameters() tools.Schema {
	return tools.Schema{
		Type: "object",
		Properties: map[string]tools.PropertySchema{
			"agent_type": agentTypeProperty(t.sw, "Roster agent type to spawn."),
			"task":       {Type: "string", Description: "The focused task/briefing for the member."},
			"team":       {Type: "string", Description: "Team to spawn into. Defaults to \"default\"."},
		},
		Required:             []string{"agent_type", "task"},
		AdditionalProperties: false,
	}
}
func (t *spawnTool) Safety() tools.Safety {
	return tools.Safety{
		SideEffect:      tools.SideEffectShell,
		Permission:      tools.PermissionPrompt,
		Reason:          "Spawns a swarm member sub-agent under the orchestrator's sandbox and policy.",
		AdvertiseInAuto: true,
	}
}
func (t *spawnTool) Run(ctx context.Context, args map[string]any) tools.Result {
	return t.RunWithOptions(ctx, args, tools.RunOptions{})
}
func (t *spawnTool) RunWithOptions(_ context.Context, args map[string]any, options tools.RunOptions) tools.Result {
	agentType := swarmStr(args, "agent_type")
	task := swarmStr(args, "task")
	team := swarmStr(args, "team")
	if agentType == "" {
		return errResult("swarm_spawn requires agent_type")
	}
	if task == "" {
		return errResult("swarm_spawn requires task")
	}
	id, err := t.sw.Spawn(policyFrom(options), team, agentType, task, options.Cwd)
	if err != nil {
		if errors.Is(err, ErrUnknownAgentType) {
			return errResult("%v; available agent types: %s", err, strings.Join(t.sw.Registry().AgentTypes(), ", "))
		}
		return errResult("%v", err)
	}
	out := fmt.Sprintf("Spawned %s as task %s on team %s.", agentType, id, displayTeam(team))
	res := okResult(out, "swarm", out)
	res.Meta = map[string]string{"task_id": id, "team": sanitizeName(team), "agent_type": agentType}
	return res
}

// ---- swarm_send ------------------------------------------------------------

type sendTool struct {
	sw *Swarm
	deferredSwarmTool
}

// Deferred un-defers swarm_send once a swarm is active (see deferredSwarmTool).
func (t *sendTool) Deferred() bool { return !t.sw.hasActiveSwarm() }

func (t *sendTool) Name() string { return SendToolName }
func (t *sendTool) Description() string {
	return "Send a message to another swarm member's inbox on a team."
}
func (t *sendTool) Parameters() tools.Schema {
	return tools.Schema{
		Type: "object",
		Properties: map[string]tools.PropertySchema{
			"to":      {Type: "string", Description: "Recipient member/agent id."},
			"body":    {Type: "string", Description: "Message body."},
			"subject": {Type: "string", Description: "Optional subject."},
			"from":    {Type: "string", Description: "Optional sender id (the orchestrator by default)."},
			"team":    {Type: "string", Description: "Team to send within. Defaults to \"default\"."},
		},
		Required:             []string{"to", "body"},
		AdditionalProperties: false,
	}
}
func (t *sendTool) Safety() tools.Safety {
	return tools.Safety{
		SideEffect:      tools.SideEffectWrite,
		Permission:      tools.PermissionAllow,
		Reason:          "Writes a message to an owner-only swarm inbox file.",
		AdvertiseInAuto: true,
	}
}
func (t *sendTool) Run(ctx context.Context, args map[string]any) tools.Result {
	return t.RunWithOptions(ctx, args, tools.RunOptions{})
}
func (t *sendTool) RunWithOptions(_ context.Context, args map[string]any, _ tools.RunOptions) tools.Result {
	to := swarmStr(args, "to")
	body := swarmStr(args, "body")
	if to == "" {
		return errResult("swarm_send requires to")
	}
	if body == "" {
		return errResult("swarm_send requires body")
	}
	team := swarmStr(args, "team")
	from := swarmStr(args, "from")
	if from == "" {
		from = "orchestrator"
	}
	msg := Message{From: from, Subject: swarmStr(args, "subject"), Body: body}
	if err := t.sw.Mailbox().Send(team, to, msg); err != nil {
		return errResult("%v", err)
	}
	out := fmt.Sprintf("Delivered message to %s on team %s.", sanitizeName(to), displayTeam(team))
	return okResult(out, "swarm", out)
}

// ---- swarm_inbox -----------------------------------------------------------

type inboxTool struct {
	sw *Swarm
	deferredSwarmTool
}

// Deferred un-defers swarm_inbox once a swarm is active (see deferredSwarmTool).
func (t *inboxTool) Deferred() bool { return !t.sw.hasActiveSwarm() }

func (t *inboxTool) Name() string { return InboxToolName }
func (t *inboxTool) Description() string {
	return "Read a member's inbox on a team. Returns all messages and marks previously-unread ones as read."
}
func (t *inboxTool) Parameters() tools.Schema {
	return tools.Schema{
		Type: "object",
		Properties: map[string]tools.PropertySchema{
			"agent": {Type: "string", Description: "Member/agent id whose inbox to read."},
			"team":  {Type: "string", Description: "Team the inbox belongs to. Defaults to \"default\"."},
		},
		Required:             []string{"agent"},
		AdditionalProperties: false,
	}
}
func (t *inboxTool) Safety() tools.Safety {
	return tools.Safety{
		// Reading an inbox marks messages read (a benign metadata flip in an
		// owner-only file), so it is classified as a write but auto-allowed.
		SideEffect:      tools.SideEffectWrite,
		Permission:      tools.PermissionAllow,
		Reason:          "Reads and marks-read an owner-only swarm inbox file.",
		AdvertiseInAuto: true,
	}
}
func (t *inboxTool) Run(ctx context.Context, args map[string]any) tools.Result {
	return t.RunWithOptions(ctx, args, tools.RunOptions{})
}
func (t *inboxTool) RunWithOptions(_ context.Context, args map[string]any, _ tools.RunOptions) tools.Result {
	agent := swarmStr(args, "agent")
	if agent == "" {
		return errResult("swarm_inbox requires agent")
	}
	team := swarmStr(args, "team")
	messages, err := t.sw.Mailbox().ReadAndConsume(team, agent)
	if err != nil {
		return errResult("%v", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Inbox for %s on team %s: %d message(s)\n", sanitizeName(agent), displayTeam(team), len(messages))
	for i, msg := range messages {
		state := "read"
		if !msg.Read {
			state = "unread"
		}
		subject := msg.Subject
		if subject != "" {
			subject = " [" + subject + "]"
		}
		fmt.Fprintf(&b, "  [%d] (%s) from %s%s: %s\n", i, state, msg.From, subject, msg.Body)
	}
	out := strings.TrimRight(b.String(), "\n")
	return okResult(out, "swarm", fmt.Sprintf("%d message(s)", len(messages)))
}

// ---- swarm_status ----------------------------------------------------------

type statusTool struct {
	sw *Swarm
	deferredSwarmTool
}

// Deferred un-defers swarm_status once a swarm is active (see deferredSwarmTool).
func (t *statusTool) Deferred() bool { return !t.sw.hasActiveSwarm() }

func (t *statusTool) Name() string { return StatusToolName }
func (t *statusTool) Description() string {
	return "Return a one-shot snapshot of swarm task status: counts by state and per-task lines, optionally scoped to one team. " +
		"This does NOT wait. To wait for members to finish and read their results, call swarm_collect once — do not call swarm_status repeatedly in a loop."
}
func (t *statusTool) Parameters() tools.Schema {
	return tools.Schema{
		Type: "object",
		Properties: map[string]tools.PropertySchema{
			"team": {Type: "string", Description: "Optional team to scope to."},
		},
		AdditionalProperties: false,
	}
}
func (t *statusTool) Safety() tools.Safety {
	return tools.Safety{
		SideEffect:      tools.SideEffectRead,
		Permission:      tools.PermissionAllow,
		Reason:          "Reports in-memory swarm task status.",
		AdvertiseInAuto: true,
	}
}
func (t *statusTool) Run(ctx context.Context, args map[string]any) tools.Result {
	return t.RunWithOptions(ctx, args, tools.RunOptions{})
}
func (t *statusTool) RunWithOptions(_ context.Context, args map[string]any, _ tools.RunOptions) tools.Result {
	team := swarmStr(args, "team")
	tasks := t.sw.Coordinator().List()
	if team != "" {
		want := sanitizeName(team)
		filtered := tasks[:0:0]
		for _, task := range tasks {
			if task.Team == want {
				filtered = append(filtered, task)
			}
		}
		tasks = filtered
	}
	return okResult(renderTasks(t.sw.Coordinator(), tasks, team), "swarm", fmt.Sprintf("%d task(s)", len(tasks)))
}

// ---- swarm_handoff ---------------------------------------------------------

type handoffTool struct {
	sw *Swarm
	deferredSwarmTool
}

func (t *handoffTool) Name() string { return HandoffToolName }
func (t *handoffTool) Description() string {
	return "Stop a task's current member, then hand it off to a fresh member of another agent type, delivering an optional note to the new member's inbox."
}
func (t *handoffTool) Parameters() tools.Schema {
	return tools.Schema{
		Type: "object",
		Properties: map[string]tools.PropertySchema{
			"task_id":       {Type: "string", Description: "Task id to hand off."},
			"to_agent_type": agentTypeProperty(t.sw, "Roster agent type to take over."),
			"note":          {Type: "string", Description: "Optional handoff note delivered to the new member's inbox."},
			"team":          {Type: "string", Description: "Team the task belongs to. Defaults to \"default\"."},
		},
		Required:             []string{"task_id", "to_agent_type"},
		AdditionalProperties: false,
	}
}
func (t *handoffTool) Safety() tools.Safety {
	return tools.Safety{
		SideEffect:      tools.SideEffectShell,
		Permission:      tools.PermissionPrompt,
		Reason:          "Stops the current swarm member, spawns its replacement, and writes the handoff note to the new member's inbox.",
		AdvertiseInAuto: true,
	}
}
func (t *handoffTool) Run(ctx context.Context, args map[string]any) tools.Result {
	return t.RunWithOptions(ctx, args, tools.RunOptions{})
}
func (t *handoffTool) RunWithOptions(_ context.Context, args map[string]any, options tools.RunOptions) tools.Result {
	taskID := swarmStr(args, "task_id")
	to := swarmStr(args, "to_agent_type")
	if taskID == "" {
		return errResult("swarm_handoff requires task_id")
	}
	if to == "" {
		return errResult("swarm_handoff requires to_agent_type")
	}
	team := swarmStr(args, "team")
	newID, err := t.sw.Handoff(policyFrom(options), team, taskID, to, swarmStr(args, "note"))
	if err != nil {
		return errResult("%v", err)
	}
	out := fmt.Sprintf("Handed off task %s to %s as task %s on team %s.", taskID, to, newID, displayTeam(team))
	res := okResult(out, "swarm", out)
	res.Meta = map[string]string{"task_id": newID, "previous_task_id": taskID}
	return res
}

// ---- swarm_collect ---------------------------------------------------------

type collectTool struct {
	sw *Swarm
	deferredSwarmTool
}

// Deferred un-defers swarm_collect once a swarm is active (see deferredSwarmTool).
func (t *collectTool) Deferred() bool { return !t.sw.hasActiveSwarm() }

func (t *collectTool) Name() string { return CollectToolName }
func (t *collectTool) Description() string {
	return "Wait for a team's tasks to finish, then return each task's status, result, and any error. " +
		"This BLOCKS until every member completes (bounded by a timeout), so call it once after spawning to get the results — " +
		"do not poll swarm_status in a loop. Use swarm_status only for a quick mid-run snapshot."
}
func (t *collectTool) Parameters() tools.Schema {
	return tools.Schema{
		Type: "object",
		Properties: map[string]tools.PropertySchema{
			"team":            {Type: "string", Description: "Team to collect from. Defaults to \"default\"."},
			"timeout_seconds": {Type: "number", Description: "Max seconds to wait for members to finish before returning partial results. Defaults to 600, capped at 1800."},
		},
		AdditionalProperties: false,
	}
}
func (t *collectTool) Safety() tools.Safety {
	return tools.Safety{
		SideEffect:      tools.SideEffectRead,
		Permission:      tools.PermissionAllow,
		Reason:          "Reports in-memory swarm task results.",
		AdvertiseInAuto: true,
	}
}
func (t *collectTool) Run(ctx context.Context, args map[string]any) tools.Result {
	return t.RunWithOptions(ctx, args, tools.RunOptions{})
}

const (
	defaultCollectWaitTimeout = 10 * time.Minute
	maxCollectWaitTimeout     = 30 * time.Minute
)

// collectWaitTimeout reads the optional timeout_seconds arg (JSON numbers arrive
// as float64), falling back to the default and clamping to the max so a single
// collect call can never block unboundedly.
func collectWaitTimeout(args map[string]any) time.Duration {
	if v, ok := args["timeout_seconds"]; ok {
		if f, ok := v.(float64); ok && f > 0 {
			// Clamp in float space first: a very large f * float64(time.Second)
			// overflows the int64 nanosecond Duration and wraps negative, which
			// would make context.WithTimeout fire immediately instead of honoring
			// the cap. Compare seconds, then convert only a known-safe value.
			if f >= maxCollectWaitTimeout.Seconds() {
				return maxCollectWaitTimeout
			}
			return time.Duration(f * float64(time.Second))
		}
	}
	return defaultCollectWaitTimeout
}

func (t *collectTool) RunWithOptions(ctx context.Context, args map[string]any, _ tools.RunOptions) tools.Result {
	team := swarmStr(args, "team")
	// Block until the team's members all finish so the orchestrator gets final
	// results in one call instead of polling. Bounded by the run context (user
	// cancel) and a timeout cap; on timeout/cancel it returns the latest partial
	// state rather than hanging on a stuck member.
	if ctx == nil {
		ctx = context.Background()
	}
	waitCtx, cancel := context.WithTimeout(ctx, collectWaitTimeout(args))
	defer cancel()
	tasks := t.sw.CollectWait(waitCtx, team)
	var b strings.Builder
	fmt.Fprintf(&b, "Results for team %s: %d task(s)\n", displayTeam(team), len(tasks))
	for _, task := range tasks {
		line := fmt.Sprintf("  - %s [%s] %s", task.ID, task.Status, task.Description)
		if task.Result != "" {
			line += "\n      result: " + collapse(task.Result)
		}
		if task.Err != "" {
			line += "\n      error: " + collapse(task.Err)
		}
		b.WriteString(line + "\n")
	}
	out := strings.TrimRight(b.String(), "\n")
	res := okResult(out, "swarm", fmt.Sprintf("%d task(s)", len(tasks)))
	// Surface each completed member's durable session id (task_id -> session_id)
	// so the TUI can make the AGENTS sidebar rows drill into the member's session.
	meta := map[string]string{}
	for _, task := range tasks {
		if task.SessionID != "" {
			meta[task.ID] = task.SessionID
		}
	}
	if len(meta) > 0 {
		res.Meta = meta
	}
	return res
}

// renderTasks formats a status summary + per-task lines, colored per agent.
func renderTasks(coord *Coordinator, tasks []Task, team string) string {
	s := coord.Summarize()
	var b strings.Builder
	scope := "all teams"
	if strings.TrimSpace(team) != "" {
		scope = "team " + sanitizeName(team)
	}
	fmt.Fprintf(&b, "Swarm status (%s): %d task(s) — %d running, %d pending, %d done, %d failed, %d handed-off\n",
		scope, len(tasks), s.Running, s.Pending, s.Done, s.Failed, s.HandedOff)
	sorted := make([]Task, len(tasks))
	copy(sorted, tasks)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	for _, task := range sorted {
		fmt.Fprintf(&b, "  - %s [%s] (%s) %s\n", task.ID, task.Status, coord.Color(task.AgentID), collapse(task.Description))
	}
	return strings.TrimRight(b.String(), "\n")
}

// collapse trims a possibly-multiline string to a single compact line for
// status/collect output.
func collapse(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	const max = 200 // runes, not bytes — never slice mid-character
	runes := []rune(s)
	if len(runes) > max {
		return string(runes[:max]) + "…"
	}
	return s
}
